// Actual source/removal edges after the committed swap and before ordinary
// victim removal must retain generation invalidation and source ownership.
package connect

import (
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

func testServiceRefreshRetirementInvalidation(t *testing.T, change string) {
	t.Helper()
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newServiceRefreshRetirementFixture(t, 53321)
		entered, release := make(chan struct{}), make(chan struct{})
		var enteredOnce, releaseOnce sync.Once
		finish := func() {
			// Release the real callback's mutex holder before any settle or
			// fixture cleanup may wait for the rest of the expansion.
			releaseOnce.Do(func() { close(release) })
			f.h.concurrent = false
		}
		t.Cleanup(finish)
		f.window.beforeServiceRefreshRetireForTest = func(owner *tcpSynRetry, victim *multiClientChannel) {
			if owner == f.owner && victim == f.victim {
				enteredOnce.Do(func() { close(entered) })
				<-release
			}
		}
		// No late provider exists during this virtual-time advance, so the
		// retirement callback cannot already hold the expansion mutex.
		f.h.sleep(f.started.Add(1109 * time.Millisecond).Sub(time.Now()))
		f.h.concurrent = true
		f.h.addExit(reraceExitSpec{name: "retirement-late", answer: func(path *IpPath) reraceAnswer {
			if reraceSameFlow(path, f.path) {
				return reraceSilent
			}
			return reraceAccept
		}})
		// The existing public producer is synchronous; concurrent mode only
		// suppresses its helper's quiescence wait while the real hook is held.
		accepted := privateCrossWindowNativeSyn(f.h, f.path)
		t.Logf("held-retirement actual native offer: accepted=%t age=%s", accepted, time.Since(f.started))
		if !discoveryDemandTestAwaitPublication(t, entered, f.started.Add(2500*time.Millisecond)) {
			t.Fatal("retirement invalidation did not reach the actual completed-swap removal boundary")
		}
		f.update.stateLock.Lock()
		original := f.update.synRetry == f.owner && f.update.discoveryAttemptStamp.Load() == f.stamp && f.owner.references >= 2 &&
			f.update.synRetryRemoteTailRefusedWithLock(f.victim)
		f.update.stateLock.Unlock()
		if !original || f.victim.IsDone() || strictHandoverTestInstalled(f.window, f.victim) || len(f.window.unorderedClients()) != 5 {
			t.Fatal("retirement invalidation lacks the committed swap and still-live original proof")
		}
		newSynSequence := f.path.SequenceNumber + 1
		newSynAccepted := false
		switch change {
		case "new-syn":
			next := *f.path
			next.SequenceNumber = newSynSequence
			newSynAccepted = privateCrossWindowNativeSyn(f.h, &next)
		case "native-reset":
			packet := MessagePoolCopy(reraceTcpPacket(f.path, tcpFlagRst|tcpFlagAck, f.path.SequenceNumber+1, 0, nil))
			accepted := [1]bool{}
			count := f.h.multiClient.SendNativePacketBatchWithResults(f.h.source, protocol.ProvideMode_Network, [][]byte{packet}, reraceSendTimeout, accepted[:])
			if count != 1 || !accepted[0] {
				t.Fatal("retirement native-reset control did not admit its actual source control packet")
			}
		case "source-close":
			f.update.Close()
		case "unrelated-removal":
			other := f.siblingOwners[0]
			if other == f.victim || other.IsDone() {
				t.Fatal("retirement invalidation has no distinct actual failed provider")
			}
			other.Close()
			f.window.removeClients(other)
		default:
			t.Fatal("unknown actual retirement invalidation control")
		}
		f.update.stateLock.Lock()
		changedStamp := f.update.discoveryAttemptStamp.Load()
		changedOwner := f.update.synRetry
		changedSyn, changedSynSeen := f.update.dialFailedSyn, f.update.dialFailedSynSeen
		canceled := f.update.IsDone()
		reset := f.update.sourceReset.Load() || f.update.pendingCollapseResetWithLock() || changedOwner != f.owner
		f.update.stateLock.Unlock()
		if change == "source-close" {
			if !canceled {
				t.Fatal("retirement source-close edge did not actually cancel its context")
			}
		} else if change == "native-reset" {
			if !reset {
				t.Fatal("retirement native-reset control did not invalidate the actual retained source")
			}
		} else if changedStamp == f.stamp {
			t.Fatal("retirement control did not actually change the source generation/evidence token")
		}
		if change == "new-syn" && (!changedSynSeen || changedSyn != newSynSequence) {
			t.Fatal("retirement control did not observe the actual new native SYN attempt")
		}
		finish()
		f.h.settle()
		f.awaitLate(t)
		f.update.stateLock.Lock()
		afterStamp := f.update.discoveryAttemptStamp.Load()
		freshAttempt := f.update.dialFailedSynSeen && f.update.dialFailedSyn == newSynSequence
		freshCache := true
		if current := f.update.synRetry; current != nil {
			if current == f.owner {
				// A refused fresh source owns no replacement cache. Do not
				// invent ownership from its earlier synchronous observation.
				freshCache = !newSynAccepted
			} else {
				freshCache = current.sequence == newSynSequence && current.serial != f.serial && current.references != 0 &&
					current.packet != nil && current.credit != nil && !current.credit.released.Load() && current.validGenerationWithLock(f.update)
			}
		}
		resetStillInvalid := f.update.synRetry != f.owner || f.update.sourceReset.Load() || f.update.pendingCollapseResetWithLock()
		f.update.stateLock.Unlock()
		if afterStamp < changedStamp {
			t.Fatal("SERVICE_REFRESH_RETIREMENT_REWOUND_INVALIDATED_STAMP")
		}
		if (change == "new-syn" || change == "unrelated-removal") && afterStamp == f.stamp {
			t.Fatal("SERVICE_REFRESH_RETIREMENT_REUSED_INVALIDATED_STAMP")
		}
		if change == "native-reset" && !resetStillInvalid {
			t.Fatal("SERVICE_REFRESH_RETIREMENT_REOPENED_NATIVE_RESET_SOURCE")
		}
		if change == "new-syn" && (!freshAttempt || !freshCache) {
			t.Fatal("own retirement disturbed the actual fresh native source attempt/cache generation")
		}
		if change == "unrelated-removal" && afterStamp != changedStamp+1 {
			t.Fatalf("SERVICE_REFRESH_RETIREMENT_IGNORED_UNRELATED_REMOVAL: before=%d changed=%d after=%d", f.stamp, changedStamp, afterStamp)
		}
		f.window.stateLock.Lock()
		owned := f.window.serviceRefreshOwner
		f.window.stateLock.Unlock()
		if owned != nil || !f.victim.IsDone() {
			t.Fatal("invalidated retirement did not return its exact window reference and retire the committed victim")
		}
		if change == "source-close" && !f.credit.released.Load() {
			t.Fatal("invalidated retirement retained the canceled source's paid credit")
		}
	})
}

func TestFullServiceRefreshOwnRetirementRejectsChangedNativeSyn(t *testing.T) {
	testServiceRefreshRetirementInvalidation(t, "new-syn")
}

func TestFullServiceRefreshOwnRetirementRejectsCanceledSource(t *testing.T) {
	testServiceRefreshRetirementInvalidation(t, "source-close")
}

func TestFullServiceRefreshOwnRetirementKeepsUnrelatedRemovalInvalidation(t *testing.T) {
	testServiceRefreshRetirementInvalidation(t, "unrelated-removal")
}

func TestFullServiceRefreshOwnRetirementRejectsNativeReset(t *testing.T) {
	testServiceRefreshRetirementInvalidation(t, "native-reset")
}
