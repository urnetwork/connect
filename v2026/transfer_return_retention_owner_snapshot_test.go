// Retention fixtures exchange values with the real send owner before a write
// and after recovery. No mutable sendItem escapes these worker callbacks.
package connect

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Both item state and sequence-local accounting are copied on Run. Shared
// service counters retain their own lock even though this fixture has one lane.
type windowRetryOwnerObservation struct {
	at, lastHeadAckTime            time.Time
	messageId                      Id
	sequenceNumber                 uint64
	present, written               bool
	sendCount, retained, lifetimes int
	queued, reservations           int
	outstanding, owned             ByteCount
}

// One bounded slot per causal boundary; only the initial inspection holds Run.
type windowRetryOwnerSnapshots struct {
	beforeWrite chan windowRetryOwnerObservation
	afterWrite  chan windowRetryOwnerObservation
	afterRetire chan windowRetryOwnerObservation
	resumeWrite chan struct{}
}

// Install while releaseInitial still owns Run. The first cut is after the
// actual original is retained and before even the outer pacing wait begins.
func observeWindowRetryOwner(fixture *windowRetryClockFixture) *windowRetryOwnerSnapshots {
	sequence := fixture.sequence
	snapshots := &windowRetryOwnerSnapshots{
		beforeWrite: make(chan windowRetryOwnerObservation, 1),
		afterWrite:  make(chan windowRetryOwnerObservation, 1),
		afterRetire: make(chan windowRetryOwnerObservation, 1),
		resumeWrite: make(chan struct{}),
	}
	capture := func() windowRetryOwnerObservation {
		observation := windowRetryOwnerObservation{
			at: time.Now(), lastHeadAckTime: sequence.lastHeadAckTime,
			retained: len(sequence.sendItems), lifetimes: len(sequence.ackLifetimes.items),
			queued: sequence.resendQueue.Len(),
			owned:  sequence.windowPacer.serviceSent - sequence.windowPacer.serviceAcked,
		}
		if item := sequence.resendQueue.GetBySequenceNumber(1); item != nil {
			observation.present, observation.messageId = true, item.messageId
			observation.sequenceNumber, observation.written = item.sequenceNumber, item.transportWriteObserved
			observation.sendCount = item.sendCount
		}
		service := sequence.windowPacer.service
		service.stateLock.Lock()
		observation.reservations = service.pacingReservations
		observation.outstanding = service.sent - service.total
		service.stateLock.Unlock()
		return observation
	}
	prepared, written, retired := false, false, false
	previousDisposition := sequence.sendBufferSettings.afterGroupDispositionForTest
	sequence.sendBufferSettings.afterGroupDispositionForTest = func(source *SendSequence, disposition sendGroupDisposition, frames []*protocol.Frame) {
		if previousDisposition != nil {
			previousDisposition(source, disposition, frames)
		}
		if source != sequence || prepared {
			return
		}
		observation := capture()
		if !observation.present {
			return
		}
		prepared = true
		snapshots.beforeWrite <- observation
		select {
		case <-sequence.ctx.Done():
		case <-snapshots.resumeWrite:
		}
	}
	previousApplied := sequence.sendBuffer.afterApplyAckSnapshotForTest
	sequence.sendBuffer.afterApplyAckSnapshotForTest = func(id sendSequenceId) {
		if previousApplied != nil {
			previousApplied(id)
		}
		if id != sequence.id() || !prepared || retired {
			return
		}
		observation := capture()
		if !written && observation.present && observation.written {
			written = true
			snapshots.afterWrite <- observation
		}
		if written && observation.retained == 0 && observation.lifetimes == 0 && observation.queued == 0 {
			retired = true
			snapshots.afterRetire <- observation
		}
	}
	return snapshots
}

// The reader inspects an immutable value, then releases the real writer at the
// same fake instant. Later timer wakes can mutate only the worker's original.
func releaseWindowRetryOwnerWrite(t *testing.T, snapshots *windowRetryOwnerSnapshots, at time.Time) windowRetryOwnerObservation {
	t.Helper()
	var before windowRetryOwnerObservation
	select {
	case before = <-snapshots.beforeWrite:
	default:
		t.Fatal("send owner did not publish its pre-write retention cut")
	}
	if !before.present || before.written || before.sequenceNumber != 1 || before.sendCount != 1 || before.at != at || time.Now() != at {
		t.Fatalf("pre-write owner cut changed: %+v, now=%s want=%s", before, time.Now(), at)
	}
	close(snapshots.resumeWrite)
	synctest.Wait()
	return before
}
