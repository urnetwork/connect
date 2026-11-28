// A watched sibling may still own a future capacity transition when the passed source exits.
package connect

import (
	"bytes"
	"testing"
	"testing/synctest"
	"time"
)

// Keep the sibling's DATA permit closed across the final readiness check.
// Its actual seed acknowledgement, with no new native offer, must later
// resume the same retained SYN through the original gate registration.
func TestNativeSynRetryReadinessRetainsPendingSiblingWhenGrantedSourceCloses(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		first := newNativeBatchCreditFixture(t)
		sibling := newNativeBatchCreditFixture(t)
		control := newSynRetryDataReadyControl(t, 51339)
		update, owner, parent := control.update, control.owner, control.harness.multiClient
		siblingGate := &sibling.source.resendCapacityUnavailable
		start := time.Now()
		control.refuseOn(t, first, sibling)
		ready, paid := owner.readiness, owner.readiness.credit
		ownerSerial, sequence := owner.serial, owner.sequence
		firstGate := &first.source.resendCapacityUnavailable
		firstStarted, siblingStarted := first.base.started.Load(), sibling.base.started.Load()
		var siblingInstall uint64
		func() {
			update.stateLock.Lock()
			defer update.stateLock.Unlock()
			for index := range ready.count {
				slot := &ready.slots[index]
				if slot.source == sibling.source && slot.gate == siblingGate {
					siblingInstall = slot.installation
				}
			}
		}()
		if siblingInstall == 0 {
			t.Fatal("the actual DATA refusal did not install the sibling's original watch")
		}
		parent.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel {
			if first.base.selected.IsDone() {
				return []*multiClientChannel{sibling.base.selected}
			}
			return []*multiClientChannel{first.base.selected, sibling.base.selected}
		}

		entered, release := synRetryReadyHold(t, parent, false)
		first.releasePeerCredit()
		synRetryControlEntered(t, entered)
		if firstGate.Load() || firstGate.waiterCount.Load() != 0 || !siblingGate.Load() {
			t.Fatal("the first source did not pass the only available turn")
		}
		first.source.Cancel()
		first.base.parent.cancel()
		if err := first.base.closeClient(t.Context()); err != nil {
			t.Fatal(err)
		}
		release()
		synctest.Wait()
		sameOwner, pending := func() (bool, bool) {
			update.stateLock.Lock()
			defer update.stateLock.Unlock()
			sameOwner := update.synRetry == owner && owner.readiness == ready && owner.serial == ownerSerial &&
				owner.sequence == sequence && owner.validWithLock(update) && bytes.Equal(owner.packet, control.original)
			if !sameOwner || !ready.indexed.Load() || owner.queued {
				return sameOwner, false
			}
			for index := range ready.count {
				slot := &ready.slots[index]
				if slot.source == sibling.source && slot.gate == siblingGate &&
					slot.installation == siblingInstall && slot.waiter.owner.Load() == siblingGate {
					return true, true
				}
			}
			return true, false
		}()
		if sameOwner && !pending && first.base.selected.IsDone() && first.source.ctx.Err() != nil &&
			parent.ctx.Err() == nil && sibling.source.ctx.Err() == nil && siblingGate.Load() &&
			sibling.base.started.Load() == siblingStarted && time.Since(start) == 0 {
			t.Fatal("NATIVE_SYN_PENDING_SIBLING_LOST same_owner=true passed_source_closed=true still_full=true")
		}
		if !pending || siblingGate.waiterCount.Load() != 1 || first.base.started.Load() != firstStarted ||
			sibling.base.started.Load() != siblingStarted || len(sibling.base.route) != 0 {
			t.Fatal("coalesced readiness did not stay parked on the exact original watch")
		}

		sibling.releasePeerCredit()
		if sibling.base.started.Load() != siblingStarted+1 || first.base.started.Load() != firstStarted ||
			control.harness.boundTo(control.path) != sibling.base.selected || time.Since(start) != 0 {
			t.Fatal("the original sibling's later capacity edge did not resume one retained SYN")
		}
		sibling.finishPackets(control.original)
		synRetryReadyProof(t, control, sibling.base.selected)
		control.close(t)
		requireSynRetryReadyRetired(t, ready, paid)
		if siblingGate.waiterCount.Load() != 0 || len(first.base.route) != 0 {
			t.Fatal("joined passed-source fallback left a wait or wrote to the retired source")
		}
		t.Log("NATIVE_SYN_PENDING_SIBLING_RESUMED same_owner=true original_watch=true exact_wire=1 native_reoffers=0")
	})
}
