package connect

import (
	"context"
	"fmt"
	"sync"
)

// Cancellation is safe only before a queued Pack becomes a reliable sendItem.
// This error proves that no sequence number or data wire owner was published.
var errPreparedSendCanceled = fmt.Errorf("prepared Pack canceled before reliable handoff: %w", context.Canceled)

// This small capsule never retains the upstream provider/flow or a sequence.
// The two wake channels refer to fixed workers without keeping those workers'
// object graphs alive. A contract wait binds a cancel function only while it
// is active; serialization or queued disposal clears every temporary owner.
type preparedSendHandoff struct {
	mutex                        sync.Mutex
	canceled, committed, settled bool
	upstreamWake, sequenceWake   chan struct{}
	contractCancel               context.CancelFunc
}

type preparedSendHandoffTarget interface {
	preparedHandoff() *preparedSendHandoff
}

func (record sendAckRecord) preparedHandoff() *preparedSendHandoff {
	if record.group != nil {
		return record.group.ack.preparedHandoff()
	}
	if target, ok := record.target.(preparedSendHandoffTarget); ok {
		return target.preparedHandoff()
	}
	return nil
}

func notifyPreparedWake(wake chan struct{}) {
	select {
	case wake <- struct{}{}:
	default:
	}
}

func (handoff *preparedSendHandoff) bindSequence(wake chan struct{}) {
	if handoff == nil {
		return
	}
	handoff.mutex.Lock()
	handoff.sequenceWake = wake
	handoff.settled = false
	canceled := handoff.canceled
	handoff.mutex.Unlock()
	if canceled {
		notifyPreparedWake(wake)
	}
}

// True means upstream teardown may release its original bytes: either an
// independent reliable flight owns them, or the queued Pack has been joined.
func (handoff *preparedSendHandoff) cancelBeforeHandoff() bool {
	handoff.mutex.Lock()
	if !handoff.committed {
		handoff.canceled = true
	}
	joined := handoff.committed || handoff.settled
	cancel, wake := handoff.contractCancel, handoff.sequenceWake
	handoff.mutex.Unlock()
	if cancel != nil {
		cancel()
	}
	notifyPreparedWake(wake)
	return joined
}

func (handoff *preparedSendHandoff) cancellationRequested() bool {
	if handoff == nil {
		return false
	}
	handoff.mutex.Lock()
	defer handoff.mutex.Unlock()
	return handoff.canceled
}

func (handoff *preparedSendHandoff) transferred() bool {
	if handoff == nil {
		return false
	}
	handoff.mutex.Lock()
	defer handoff.mutex.Unlock()
	return handoff.committed
}

func (handoff *preparedSendHandoff) settle() {
	if handoff == nil {
		return
	}
	handoff.mutex.Lock()
	handoff.settled = true
	handoff.sequenceWake = nil
	wake := handoff.upstreamWake
	handoff.mutex.Unlock()
	notifyPreparedWake(wake)
}

func (handoff *preparedSendHandoff) contractContext(parent context.Context) (context.Context, func()) {
	if handoff == nil {
		return parent, func() {}
	}
	ctx, cancel := context.WithCancel(parent)
	handoff.mutex.Lock()
	handoff.contractCancel = cancel
	canceled := handoff.canceled
	handoff.mutex.Unlock()
	if canceled {
		cancel()
	}
	return ctx, func() {
		handoff.mutex.Lock()
		handoff.contractCancel = nil
		handoff.mutex.Unlock()
		cancel()
	}
}

// All-or-none commitment preserves ready-drain coalescing. A canceled member
// refuses the still-unwritten batch; live upstream owners can retry its exact
// bytes. Only the sequence owns a multi-lock operation, and each handoff is
// present in just one queued Pack, so independent batches cannot invert locks.
func (acks *sendAckSet) commitPreparedHandoffs(reserve func() error) error {
	var handoffs [sendPackH1GroupMaxFrames]*preparedSendHandoff
	count := 0
	for index := 0; index < int(acks.count); index++ {
		var record sendAckRecord
		if index < len(acks.records) {
			record = acks.records[index]
		} else {
			record = acks.overflow.records[index-len(acks.records)]
		}
		handoff := record.preparedHandoff()
		if handoff == nil {
			continue
		}
		duplicate := false
		for _, previous := range handoffs[:count] {
			duplicate = duplicate || previous == handoff
		}
		if !duplicate {
			handoffs[count] = handoff
			count++
		}
	}
	var err error
	for _, handoff := range handoffs[:count] {
		handoff.mutex.Lock()
		if handoff.canceled {
			err = errPreparedSendCanceled
		}
	}
	if err == nil {
		err = reserve()
	}
	for _, handoff := range handoffs[:count] {
		if err == nil {
			handoff.committed = true
		}
		handoff.mutex.Unlock()
		if err == nil {
			notifyPreparedWake(handoff.upstreamWake)
		}
	}
	return err
}
