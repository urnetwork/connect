// A real same-SYN original may temporarily fence a paid refusal. Its eventual
// materialization must reconsider eligibility without another native packet.
package connect

import (
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

func TestServiceRefreshReconsidersExactSourceEligibility(t *testing.T) {
	testServiceRefreshSourceEligibility(t, true, false, false)
}

func TestServiceRefreshSourceEligibilityRequiresActualRefusal(t *testing.T) {
	testServiceRefreshSourceEligibility(t, false, false, false)
}

func TestServiceRefreshMaterializedSourceCanceledBeforeReassertCannotClaim(t *testing.T) {
	testServiceRefreshSourceEligibility(t, true, true, false)
}

func TestServiceRefreshLosingSameOwnerClaimPreservesWinningReservation(t *testing.T) {
	testServiceRefreshSourceEligibility(t, true, false, true)
}

func testServiceRefreshSourceEligibility(t *testing.T, refused, cancelAtIndex, collideAtIndex bool) {
	t.Helper()
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		const port = 53306
		root := NewTransferMemoryBudget(8 * 1024 * 1024)
		budget := NewTransferMemoryBudgetWithParent(root.TotalByteCount(), root)
		t.Cleanup(func() {
			for _, ledger := range []*TransferMemoryBudget{budget, root} {
				reserved, released := ledger.Counts()
				if ledger.UsedByteCount() != 0 || reserved != released {
					t.Errorf("eligibility cleanup retained paid custody: used=%d reserved=%d released=%d", ledger.UsedByteCount(), reserved, released)
				}
			}
		})
		var heldClient, victimClient atomic.Pointer[Client]
		var armed, heldOnce, stopAtIndex atomic.Bool
		entered, release, materialized, victimPaid := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
		indexEntered, indexRelease := make(chan struct{}), make(chan struct{})
		var releaseOnce, materializedOnce, paidOnce, indexOnce, indexReleaseOnce sync.Once
		resumeIndex := func() { indexReleaseOnce.Do(func() { close(indexRelease) }) }
		unpark := func() { releaseOnce.Do(func() { close(release) }) }
		configure := func(settings *ClientSettings) {
			settings.SendBufferSettings.ResendQueueBudget = budget
			settings.SendBufferSettings.ResendQueueRetainedByteAccounting = true
			settings.SendBufferSettings.beforeGroupDequeueForTest = func(source *SendSequence) {
				if !armed.Load() {
					return
				}
				if source.client == heldClient.Load() && heldOnce.CompareAndSwap(false, true) {
					close(entered)
					select {
					case <-release:
					case <-source.ctx.Done():
					}
				} else if source.client == victimClient.Load() {
					// The refusal's real source runs only after the other original
					// is actually parked at its pending-control boundary.
					select {
					case <-entered:
					case <-source.ctx.Done():
					}
				} else {
					// Initial cache identity cannot depend on racing materialization.
					select {
					case <-victimPaid:
					case <-source.ctx.Done():
					}
				}
			}
			settings.SendBufferSettings.afterGroupDispositionForTest = func(source *SendSequence, _ sendGroupDisposition, frames []*protocol.Frame) {
				if source.client != heldClient.Load() {
					return
				}
				for _, frame := range frames {
					if frame == nil || frame.MessageType != protocol.MessageType_IpIpPacketToProvider {
						continue
					}
					packet, err := ipPacketToProviderBytes(frame)
					if err != nil {
						continue
					}
					path, err := ParseIpPath(packet)
					if err == nil && path.SourcePort == port && path.Syn && !path.Ack {
						if cancelAtIndex || collideAtIndex {
							stopAtIndex.Store(true)
						}
						materializedOnce.Do(func() { close(materialized) })
					}
				}
			}
		}
		f := newEarlyIdleRefusalFixtureWithBodyCount(t, port, false, 4, configure)
		t.Cleanup(unpark)
		t.Cleanup(resumeIndex)
		heldClient.Store(f.siblingOwners[0].client)
		victimClient.Store(f.victim.client)
		f.h.multiClient.beforeSynPublicationIndexCaptureForTest = func(update *multiClientChannelUpdate) {
			if update.ipPath != nil && update.ipPath.SourcePort == port && stopAtIndex.Load() {
				indexOnce.Do(func() { close(indexEntered) })
				select {
				case <-indexRelease:
				case <-update.ctx.Done():
				}
			}
			update.stateLock.Lock()
			owner := update.synRetry
			paid := update.ipPath != nil && update.ipPath.SourcePort == port && owner != nil &&
				owner.credit != nil && !owner.credit.released.Load() && owner.validGenerationWithLock(update)
			update.stateLock.Unlock()
			if paid {
				paidOnce.Do(func() { close(victimPaid) })
			}
		}
		f.refuseIdle.Store(refused)
		f.generator.mutex.Lock()
		f.generator.holdNextAvailable = true
		f.generator.mutex.Unlock()
		f.h.addExit(reraceExitSpec{name: "early-late", clientSettings: configure})
		publication := discoveryDemandTestObservePublication(f.window)
		f.started = time.Now()
		armed.Store(true)
		f.h.concurrent = true
		accepted := privateCrossWindowNativeSyn(f.h, f.path)
		f.h.concurrent = false
		if !accepted {
			t.Fatal("eligibility witness original native SYN was not admitted")
		}
		if !discoveryDemandTestAwaitPublication(t, entered, f.started.Add(2*time.Second)) {
			t.Fatal("eligibility witness did not hold its exact original before dequeue")
		}
		f.h.settle()
		f.update = f.h.update(f.path)
		if f.update == nil {
			t.Fatal("eligibility witness has no actual source update")
		}
		f.update.stateLock.Lock()
		f.owner = f.update.synRetry
		owner := f.owner
		var credit *transferRetainedControlCredit
		if owner != nil {
			credit = owner.credit
		}
		live := owner != nil && owner.credit != nil && !owner.credit.released.Load() && owner.validGenerationWithLock(f.update)
		pending := live && owner.pendingConflictingControlWithLock(f.update)
		recorded := live && slices.Contains(owner.refusedRemoteIds, f.victim.Destination().Tail())
		gated := f.update.synRetryHasRemoteRefusalWithLock()
		f.stamp = f.update.discoveryAttemptStamp.Load()
		f.attemptStarted = f.update.discoveryAttemptStarted
		if owner != nil {
			f.serial = owner.serial
		}
		f.update.stateLock.Unlock()
		failures := uint64(0)
		if refused {
			failures = 1
		}
		if !live || !pending || recorded != refused || gated || uint64(f.h.metrics().DialFailuresIntercepted) != failures {
			t.Fatalf("eligibility witness lacks exact paid pending-source/refusal boundary: live=%t pending=%t recorded=%t refused=%t gated=%t failures=%d", live, pending, recorded, refused, gated, f.h.metrics().DialFailuresIntercepted)
		}
		f.window.stateLock.Lock()
		premature := f.window.serviceRefreshOwner != nil
		f.window.stateLock.Unlock()
		if premature {
			t.Fatal("pending original bypassed the service-refresh validity fence")
		}
		unpark()
		if !discoveryDemandTestAwaitPublication(t, materialized, f.started.Add(2*time.Second)) {
			t.Fatal("held original did not actually materialize after release")
		}
		if collideAtIndex {
			if !discoveryDemandTestAwaitPublication(t, indexEntered, f.started.Add(2*time.Second)) {
				t.Fatal("same-owner collision did not reach actual post-materialization boundary")
			}
			members := f.window.unorderedClients()
			type claimResult struct {
				owner *tcpSynRetry
				stamp uint64
			}
			winner := make(chan claimResult, 1)
			winnerDone := make(chan struct{})
			stolen := false
			func() {
				// The first claim acquires its real CAS reservation before
				// blocking at the existing parent lock. No new hook is needed.
				f.h.multiClient.stateLock.Lock()
				defer f.h.multiClient.stateLock.Unlock()
				go func() {
					claimed, stamp := f.h.multiClient.claimServiceRefreshOwner(f.update, f.path, f.victim, members)
					winner <- claimResult{claimed, stamp}
					close(winnerDone)
				}()
				acquired := false
				for range 100000 {
					if f.victim.serviceRefreshReservation.Load() == owner {
						acquired = true
						break
					}
					// A mutex waiter is not a synctest durable wait. Observe
					// the actual atomic CAS, yielding only to its real owner.
					runtime.Gosched()
				}
				if !acquired {
					t.Fatal("first actual claim did not acquire its bounded reservation")
				}
				loser, stamp := f.h.multiClient.claimServiceRefreshOwner(f.update, f.path, f.victim, members)
				if loser != nil || stamp != 0 {
					t.Fatal("simultaneous same-owner loser unexpectedly acquired a second claim")
				}
				stolen = f.victim.serviceRefreshReservation.Load() != owner
			}()
			if !discoveryDemandTestAwaitPublication(t, winnerDone, f.started.Add(2*time.Second)) {
				t.Fatal("first claim did not return after parent lock released")
			}
			result := <-winner
			if result.owner != nil {
				if !f.window.installServiceRefresh(result.owner, f.victim, result.stamp, members) {
					f.victim.releaseServiceRefreshReservation(result.owner)
					f.update.stateLock.Lock()
					f.update.releaseSynRetryReferenceWithLock(result.owner)
					f.update.stateLock.Unlock()
					f.update.releaseSynRetryMemory()
					t.Fatal("winning claim could not transfer its exact reservation to the window")
				}
			}
			resumeIndex()
			f.h.settle()
			if stolen {
				t.Fatal("SERVICE_REFRESH_CAS_LOSER_RELEASED_WINNING_RESERVATION: failed CAS cleared another invocation's exact paid owner")
			}
			if result.owner != owner || result.stamp != f.stamp {
				t.Fatal("preserved winner did not retain the actual source and attempt")
			}
		}
		if cancelAtIndex {
			if !discoveryDemandTestAwaitPublication(t, indexEntered, f.started.Add(2*time.Second)) {
				t.Fatal("actual materialization did not reach its post-unlock publication boundary")
			}
			// Close the actual source while its materialization is suspended
			// outside every lock, before indexing and the new reassertion.
			f.update.Close()
			resumeIndex()
			f.h.settle()
			f.h.sleep(f.started.Add(2 * time.Second).Sub(time.Now()))
			reads, _, mints, _ := f.generator.counts()
			f.window.stateLock.Lock()
			reserved := f.window.serviceRefreshOwner != nil
			f.window.stateLock.Unlock()
			if !f.update.IsDone() || !credit.released.Load() || reads != f.reads || mints != f.mints || reserved || f.victim.IsDone() || f.victim.serviceRefreshReservation.Load() != nil {
				t.Fatal("late materialization revived canceled source retirement authority")
			}
			discoveryDemandFullWindowAssertSiblings(t, f.h, f.siblingPaths, f.siblingOwners)
			return
		}
		f.h.settle()
		f.update.stateLock.Lock()
		stillCurrent := f.update.synRetry == owner && owner.validWithLock(f.update) && owner.retryLineage == f.owner.retryLineage
		stillRecorded := slices.Contains(owner.refusedRemoteIds, f.victim.Destination().Tail())
		f.update.stateLock.Unlock()
		if !stillCurrent || stillRecorded != refused {
			t.Fatal("actual original completion changed its paid generation or refusal receipt")
		}
		if !refused {
			f.h.sleep(f.started.Add(2 * time.Second).Sub(time.Now()))
			reads, _, mints, _ := f.generator.counts()
			f.window.stateLock.Lock()
			reserved := f.window.serviceRefreshOwner != nil
			f.window.stateLock.Unlock()
			if reads != f.reads || mints != f.mints || reserved || f.victim.IsDone() || f.victim.serviceRefreshReservation.Load() != nil {
				t.Fatal("source completion invented authenticated retirement authority")
			}
			discoveryDemandFullWindowAssertSiblings(t, f.h, f.siblingPaths, f.siblingOwners)
			return
		}
		if !discoveryDemandTestAwaitPublication(t, f.generator.availableEntered, f.started.Add(2*time.Second)) {
			t.Fatal("SERVICE_REFRESH_SOURCE_ELIGIBILITY_EDGE_LOST: exact original completed but paid refusal never reasserted acquisition")
		}
		before := f.window.serviceRefreshSnapshot()
		f.update.stateLock.Lock()
		references := owner.references
		f.update.stateLock.Unlock()
		paidBytes := budget.UsedByteCount()
		reads, _, mints, _ := f.generator.counts()
		var reasserts sync.WaitGroup
		for range 8 {
			reasserts.Add(1)
			go func() {
				defer reasserts.Done()
				f.update.reassertServiceRefreshAfterMaterialization()
			}()
		}
		reasserts.Wait()
		f.h.settle()
		after := f.window.serviceRefreshSnapshot()
		f.update.stateLock.Lock()
		sameReferences := owner.references == references
		f.update.stateLock.Unlock()
		afterReads, _, afterMints, _ := f.generator.counts()
		if before.owner != owner || after != before || !sameReferences || budget.UsedByteCount() != paidBytes || reads != afterReads || mints != afterMints {
			t.Fatal("repeated eligibility reassertion duplicated or renewed the paid reservation")
		}
		close(f.generator.availableRelease)
		f.requireLate(t, publication, f.started.Add(2500*time.Millisecond), "SERVICE_REFRESH_ELIGIBILITY_REPLAY_STALLED")
		if !credit.released.Load() {
			t.Fatal("actual answer retained source permission")
		}
		reads, _, mints, _ = f.generator.counts()
		for range 8 {
			f.update.reassertServiceRefreshAfterMaterialization()
		}
		f.h.settle()
		afterReads, _, afterMints, _ = f.generator.counts()
		if reads != afterReads || mints != afterMints || f.window.serviceRefreshSnapshot().owner != nil {
			t.Fatal("late eligibility reassertion renewed an answered source")
		}
	})
}
