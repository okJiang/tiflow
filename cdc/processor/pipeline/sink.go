// Copyright 2020 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package pipeline

import (
	"sync/atomic"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	"github.com/pingcap/log"
	"github.com/pingcap/ticdc/cdc/model"
	"github.com/pingcap/ticdc/cdc/sink"
	cerror "github.com/pingcap/ticdc/pkg/errors"
	"github.com/pingcap/ticdc/pkg/pipeline"
	"go.uber.org/zap"
)

const (
	defaultSyncResolvedBatch = 64
)

// TableStatus is status of the table pipeline
type TableStatus int32

// TableStatus for table pipeline
const (
	TableStatusInitializing TableStatus = iota
	TableStatusRunning
	TableStatusStopped
)

func (s TableStatus) String() string {
	switch s {
	case TableStatusInitializing:
		return "Initializing"
	case TableStatusRunning:
		return "Running"
	case TableStatusStopped:
		return "Stopped"
	}
	return "Unknown"
}

// Load TableStatus with THREAD-SAFE
func (s *TableStatus) Load() TableStatus {
	return TableStatus(atomic.LoadInt32((*int32)(s)))
}

// Store TableStatus with THREAD-SAFE
func (s *TableStatus) Store(new TableStatus) {
	atomic.StoreInt32((*int32)(s), int32(new))
}

type sinkNode struct {
	sink   sink.Sink
	status TableStatus

	tableID      model.TableID
	resolvedTs   model.Ts
	checkpointTs model.Ts
	targetTs     model.Ts
	barrierTs    model.Ts

	eventBuffer []*model.PolymorphicEvent
	rowBuffer   []*model.RowChangedEvent

	flowController tableFlowController
}

func newSinkNode(sink sink.Sink, startTs model.Ts, targetTs model.Ts, tableID model.TableID) *sinkNode {
	return &sinkNode{
		sink:         sink,
		status:       TableStatusInitializing,
		targetTs:     targetTs,
		resolvedTs:   startTs,
		checkpointTs: startTs,
		barrierTs:    startTs,
		tableID:      tableID,
	}
}

func (n *sinkNode) ResolvedTs() model.Ts   { return atomic.LoadUint64(&n.resolvedTs) }
func (n *sinkNode) CheckpointTs() model.Ts { return atomic.LoadUint64(&n.checkpointTs) }
func (n *sinkNode) Status() TableStatus    { return n.status.Load() }

func (n *sinkNode) Init(ctx pipeline.NodeContext) error {
	// do nothing
	return nil
}

// stop is called when sink receives a stop command or checkpointTs reaches targetTs.
// In this method, the builtin table sink will be closed by calling `Close`, and
// no more events can be sent to this sink node afterwards.
func (n *sinkNode) stop(ctx pipeline.NodeContext) (err error) {
	n.status.Store(TableStatusStopped)
	err = n.sink.Close(ctx)
	if err != nil {
		return
	}
	err = cerror.ErrTableProcessorStoppedSafely.GenWithStackByArgs()
	return
}

func (n *sinkNode) flushSink(ctx pipeline.NodeContext, resolvedTs model.Ts, verbose bool) (err error) {
	defer func() {
		if err != nil {
			n.status.Store(TableStatusStopped)
			return
		}
		if n.checkpointTs >= n.targetTs {
			err = n.stop(ctx)
		}
	}()
	if verbose {
		log.Debug("sinkNode flushSink",
			zap.Uint64("resolvedTs", resolvedTs),
			zap.Uint64("barrierTs", n.barrierTs),
			zap.Uint64("checkpointTs", n.checkpointTs),
			zap.Uint64("tableID", uint64(n.tableID)))
	}
	if resolvedTs > n.barrierTs {
		resolvedTs = n.barrierTs
	}
	if resolvedTs > n.targetTs {
		resolvedTs = n.targetTs
	}
	if resolvedTs <= n.checkpointTs {
		return nil
	}
	if err := n.flushRow2Sink(ctx); err != nil {
		return errors.Trace(err)
	}
	checkpointTs, err := n.sink.FlushRowChangedEvents(ctx, resolvedTs)
	if err != nil {
		return errors.Trace(err)
	}
	if checkpointTs <= n.checkpointTs {
		return nil
	}
	atomic.StoreUint64(&n.checkpointTs, checkpointTs)

	return nil
}

func (n *sinkNode) emitEvent(ctx pipeline.NodeContext, event *model.PolymorphicEvent) error {
	if event == nil {
		log.Warn("skip emit empty rows", zap.Reflect("event", event))
		return nil
	}
	if err := event.WaitPrepare(ctx); err != nil {
		return err
	}
	if event.Row == nil {
		log.Warn("skip emit empty rows", zap.Reflect("event", event))
		return nil
	}

	colLen := len(event.Row.Columns)
	preColLen := len(event.Row.PreColumns)
	config := ctx.ChangefeedVars().Info.Config

	// This indicates that it is an update event,
	// and after enable old value internally by default(but disable in the configuration).
	// We need to handle the update event to be compatible with the old format.
	if !config.EnableOldValue && colLen != 0 && preColLen != 0 && colLen == preColLen {
		if shouldSplitUpdateEvent(event) {
			deleteEvent, insertEvent, err := splitUpdateEvent(event)
			if err != nil {
				return errors.Trace(err)
			}
			// NOTICE: Please do not change the order, the delete event always comes before the insert event.
			n.eventBuffer = append(n.eventBuffer, deleteEvent, insertEvent)
		} else {
			// If the handle key columns are not updated, PreColumns is directly ignored.
			event.Row.PreColumns = nil
			n.eventBuffer = append(n.eventBuffer, event)
		}
	} else {
		n.eventBuffer = append(n.eventBuffer, event)
	}

	if len(n.eventBuffer) >= defaultSyncResolvedBatch {
		if err := n.flushRow2Sink(ctx); err != nil {
			return errors.Trace(err)
		}
	}
	return nil
}

// shouldSplitUpdateEvent determines if the split event is needed to align the old format based on
// whether the handle key column has been modified.
// If the handle key column is modified,
// we need to use splitUpdateEvent to split the update event into a delete and an insert event.
func shouldSplitUpdateEvent(updateEvent *model.PolymorphicEvent) bool {
	// nil event will never be split.
	if updateEvent == nil {
		return false
	}

	handleKeyCount := 0
	equivalentHandleKeyCount := 0
	for i := range updateEvent.Row.Columns {
		if updateEvent.Row.Columns[i].Flag.IsHandleKey() && updateEvent.Row.PreColumns[i].Flag.IsHandleKey() {
			handleKeyCount++
			colValueString := model.ColumnValueString(updateEvent.Row.Columns[i].Value)
			preColValueString := model.ColumnValueString(updateEvent.Row.PreColumns[i].Value)
			if colValueString == preColValueString {
				equivalentHandleKeyCount++
			}
		}
	}

	// If the handle key columns are not updated, so we do **not** need to split the event row.
	return !(handleKeyCount == equivalentHandleKeyCount)
}

// splitUpdateEvent splits an update event into a delete and an insert event.
func splitUpdateEvent(updateEvent *model.PolymorphicEvent) (*model.PolymorphicEvent, *model.PolymorphicEvent, error) {
	if updateEvent == nil {
		return nil, nil, errors.New("nil event cannot be split")
	}

	// If there is an update to handle key columns,
	// we need to split the event into two events to be compatible with the old format.
	// NOTICE: Here we don't need a full deep copy because our two events need Columns and PreColumns respectively,
	// so it won't have an impact and no more full deep copy wastes memory.
	deleteEvent := *updateEvent
	deleteEventRow := *updateEvent.Row
	deleteEventRowKV := *updateEvent.RawKV
	deleteEvent.Row = &deleteEventRow
	deleteEvent.RawKV = &deleteEventRowKV

	deleteEvent.Row.Columns = nil
	for i := range deleteEvent.Row.PreColumns {
		// NOTICE: Only the handle key pre column is retained in the delete event.
		if !deleteEvent.Row.PreColumns[i].Flag.IsHandleKey() {
			deleteEvent.Row.PreColumns[i] = nil
		}
	}
	// Align with the old format if old value disabled.
	deleteEvent.Row.TableInfoVersion = 0

	insertEvent := *updateEvent
	insertEventRow := *updateEvent.Row
	insertEventRowKV := *updateEvent.RawKV
	insertEvent.Row = &insertEventRow
	insertEvent.RawKV = &insertEventRowKV
	// NOTICE: clean up pre cols for insert event.
	insertEvent.Row.PreColumns = nil

	return &deleteEvent, &insertEvent, nil
}

func (n *sinkNode) flushRow2Sink(ctx pipeline.NodeContext) error {
	for _, ev := range n.eventBuffer {
		err := ev.WaitPrepare(ctx)
		if err != nil {
			return errors.Trace(err)
		}
		if ev.Row == nil {
			log.Warn("skip emit empty rows", zap.Reflect("event", ev))
			continue
		}
		ev.Row.ReplicaID = ev.ReplicaID
		n.rowBuffer = append(n.rowBuffer, ev.Row)
	}
	failpoint.Inject("ProcessorSyncResolvedPreEmit", func() {
		log.Info("Prepare to panic for ProcessorSyncResolvedPreEmit")
		time.Sleep(10 * time.Second)
		panic("ProcessorSyncResolvedPreEmit")
	})
	err := n.sink.EmitRowChangedEvents(ctx, n.rowBuffer...)
	if err != nil {
		return errors.Trace(err)
	}
	// Do not hog memory.
	for i := range n.rowBuffer {
		n.rowBuffer[i] = nil
	}
	for i := range n.eventBuffer {
		n.eventBuffer[i] = nil
	}
	n.rowBuffer = n.rowBuffer[:0]
	n.eventBuffer = n.eventBuffer[:0]
	return nil
}

// Receive receives the message from the previous node
func (n *sinkNode) Receive(ctx pipeline.NodeContext) error {
	if n.status == TableStatusStopped {
		return cerror.ErrTableProcessorStoppedSafely.GenWithStackByArgs()
	}
	msg := ctx.Message()
	switch msg.Tp {
	case pipeline.MessageTypePolymorphicEvent:
		event := msg.PolymorphicEvent
		if event.RawKV.OpType == model.OpTypeResolved {
			if n.status == TableStatusInitializing {
				n.status.Store(TableStatusRunning)
			}
			failpoint.Inject("ProcessorSyncResolvedError", func() {
				failpoint.Return(errors.New("processor sync resolved injected error"))
			})
			if err := n.flushSink(ctx, msg.PolymorphicEvent.CRTs, true); err != nil {
				return errors.Trace(err)
			}
			atomic.StoreUint64(&n.resolvedTs, msg.PolymorphicEvent.CRTs)
			return nil
		}
		if err := n.emitEvent(ctx, event); err != nil {
			return errors.Trace(err)
		}
	case pipeline.MessageTypeTick:
		if err := n.flushSink(ctx, n.resolvedTs, false); err != nil {
			return errors.Trace(err)
		}
	case pipeline.MessageTypeCommand:
		if msg.Command.Tp == pipeline.CommandTypeStopAtTs {
			if msg.Command.StoppedTs < n.checkpointTs {
				log.Warn("the stopped ts is less than the checkpoint ts, "+
					"the table pipeline can't be stopped accurately, will be stopped soon",
					zap.Uint64("stoppedTs", msg.Command.StoppedTs), zap.Uint64("checkpointTs", n.checkpointTs))
			}
			return n.stop(ctx)
		}
	case pipeline.MessageTypeBarrier:
		n.barrierTs = msg.BarrierTs
		if err := n.flushSink(ctx, n.resolvedTs, false); err != nil {
			return errors.Trace(err)
		}
	}
	return nil
}

func (n *sinkNode) Destroy(ctx pipeline.NodeContext) error {
	n.status.Store(TableStatusStopped)
	return n.sink.Close(ctx)
}
