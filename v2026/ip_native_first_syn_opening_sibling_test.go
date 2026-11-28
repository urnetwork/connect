// A paid native opening can outlive one SendSequence while another candidate remains writable.
package connect

import (
	"bytes"
	"context"
	"testing"
	"testing/synctest"
	"time"
)

// Force the selected SendSequence to close after its capacity turn is passed.
// A second real source opens during that gap. The same refused native offer
// must use that source without borrowing another offer or advancing time.
func TestNativeFirstSynOpeningPreservesReadySiblingWhenGrantedSourceCloses(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		first := newFirstSynPressureFixture(t, 1, 51337)
		sibling := newFirstSynPressureFixture(t, 1, 51338)
		sibling.syn = first.syn
		parent := first.harness.multiClient
		parent.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel {
			if first.base.selected.IsDone() {
				return []*multiClientChannel{sibling.base.selected}
			}
			return []*multiClientChannel{first.base.selected, sibling.base.selected}
		}
		entered, release := synRetryReadyHold(t, parent, false)
		started := time.Now()
		update, owner := first.requirePending(t)
		ready, packetCredit, readinessCredit := owner.readiness, owner.credit, owner.readiness.credit
		firstGate := first.requireRawProtocolHeld(t)
		siblingGate := sibling.requireRawProtocolHeld(t)
		serial, sequence := owner.serial, owner.sequence
		bothWatched := func() bool {
			update.stateLock.Lock()
			defer update.stateLock.Unlock()
			if ready.count != 2 || !ready.indexed.Load() || update.synRetry != owner {
				return false
			}
			var firstWatched, siblingWatched bool
			for index := range ready.count {
				slot := &ready.slots[index]
				firstWatched = firstWatched || slot.source == first.source && slot.gate == firstGate && slot.installation != 0
				siblingWatched = siblingWatched || slot.source == sibling.source && slot.gate == siblingGate && slot.installation != 0
			}
			return firstWatched && siblingWatched
		}()
		if !bothWatched || firstGate.waiterCount.Load() != 1 || siblingGate.waiterCount.Load() != 1 ||
			serial == 0 || sequence != first.path.SequenceNumber || first.native.Load() != 1 ||
			first.refusals.Load() != 1 || sibling.refusals.Load() != 1 {
			t.Fatal("the one native opening did not register both actual refused source gates")
		}

		first.base.unpark()
		firstAcks, firstSyns := first.drain(t, true)
		synRetryControlEntered(t, entered)
		if firstAcks != len(first.acks) || firstSyns != 0 || firstGate.Load() ||
			firstGate.waiterCount.Load() != 0 || !siblingGate.Load() || first.retries.Load() != 0 {
			t.Fatal("the held readiness turn did not come from the first actual source")
		}

		// One independently admitted protocol ACK frees the sibling slot.
		// Both routes were selected for this same SYN before it was refused.
		sibling.base.unpark()
		siblingAcks, siblingSyns := sibling.drain(t, true)
		if siblingAcks != len(sibling.acks) || siblingSyns != 0 || siblingGate.Load() ||
			siblingGate.closed.Load() || sibling.source.ctx.Err() != nil {
			t.Fatal("the independent sibling did not publish real available capacity")
		}

		// These are ordinary lifetime transitions for the selected provider's
		// route and Client. They leave the native parent and sibling running.
		first.source.Cancel()
		first.base.parent.cancel()
		if err := first.base.closeClient(t.Context()); err != nil {
			t.Fatalf("join the passed source: %v", err)
		}
		stillOwned := func() bool {
			update.stateLock.Lock()
			defer update.stateLock.Unlock()
			return update.synRetry == owner && owner.serial == serial && owner.sequence == sequence &&
				owner.validWithLock(update) && owner.credit == packetCredit &&
				bytes.Equal(owner.packet, first.syn) && !owner.queued && update.race == nil
		}()
		if !first.base.selected.IsDone() || first.source.ctx.Err() == nil || !firstGate.closed.Load() ||
			parent.ctx.Err() != nil || sibling.base.selected.IsDone() || sibling.source.ctx.Err() != nil ||
			!stillOwned || first.native.Load() != 1 || first.retries.Load() != 0 || time.Since(started) != 0 {
			t.Fatal("source shutdown changed the original SYN generation or the independent ready route")
		}

		release()
		extraAcks, resumedSyns := sibling.drain(t, true)
		if resumedSyns == 0 && extraAcks == 0 && first.native.Load() == 1 && first.retries.Load() == 0 &&
			parent.ctx.Err() == nil && sibling.source.ctx.Err() == nil && !siblingGate.Load() && time.Since(started) == 0 {
			t.Fatal("NATIVE_FIRST_SYN_READY_SIBLING_LOST exact_opening=true passed_source_closed=true sibling_ready=true native_offers=1 emitted_syns=0")
		}
		// drain compares the serialized bytes, exact SendSequence and
		// absorbing peer acknowledgement of each original.
		if resumedSyns != 1 || extraAcks != 0 || len(first.base.route) != 0 ||
			first.native.Load() != 1 || first.retries.Load() != 1 || time.Since(started) != 0 {
			t.Fatalf("same paid opening did not use the ready sibling once: native=%d retries=%d SYNs=%d ACKs=%d elapsed=%s",
				first.native.Load(), first.retries.Load(), resumedSyns, extraAcks, time.Since(started))
		}
		sibling.requirePeerAckIdle(t, 1, started)
		if err := parent.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
		synRetryControlReleased(t, owner)
		requireSynRetryReadyRetired(t, ready, readinessCredit)
		if !packetCredit.released.Load() || firstGate.waiterCount.Load() != 0 || siblingGate.waiterCount.Load() != 0 {
			t.Fatal("joined native opening retained paid custody or a readiness watch")
		}
		t.Log("NATIVE_FIRST_SYN_READY_SIBLING_RESUMED exact_opening=true passed_source_closed=true sibling_ready=true native_offers=1 emitted_syns=1")
	})
}
