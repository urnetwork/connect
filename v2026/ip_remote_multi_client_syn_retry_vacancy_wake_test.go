// A retained retry request survives a native offer temporarily occupying its
// race vacancy, including genuine refusal at an already-owned resend gate.
package connect

import (
	"bytes"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Real provider failure queues the retry. Two existing barriers force a native
// candidate to fill its slot before dispatch and then refuse at real capacity.
func nativeSynRetryVacancyReopens(t *testing.T, gate string, retire string) {
	t.Helper()
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		credit := newNativeBatchCreditFixture(t)
		harness := newPrivateCrossWindowHarness(t, []privateCrossWindowExitSpec{
			{exitSpec: reraceExitSpec{name: "first", tier: 0, answer: reraceAlways(reraceSilent)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "refused", tier: 1, answer: reraceAlways(reraceSilent)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "live", tier: 1, answer: reraceAlways(reraceSilent)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "speed", answer: reraceAlways(reraceSilent)}, windowType: WindowTypeSpeed},
		}, func(settings *MultiClientSettings) { settings.MultiRaceClientCount = 2 })
		first := privateCrossWindowChannel(t, harness, "first")
		refused := privateCrossWindowChannel(t, harness, "refused")
		live := privateCrossWindowChannel(t, harness, "live")
		fresh := credit.base.selected
		speed := privateCrossWindowChannel(t, harness, "speed")

		// Independent established progress proves the shared parent remains live.
		sibling := privateCrossWindowFlow(4, 70, 48370)
		sibling.DestinationPort = 80
		if !privateCrossWindowNativeSyn(harness, sibling) || harness.boundTo(sibling) != speed {
			t.Fatal("healthy speed sibling did not acquire its native source")
		}
		harness.exit("speed").sendTo(speed.ClientId(), reraceSynAck(sibling))
		harness.settle()
		siblingUpdate := harness.update(sibling)
		if !siblingUpdate.receivedInbound.Load() || len(harness.delivered(sibling)) != 1 {
			t.Fatal("healthy sibling did not establish through its actual peer")
		}

		path := privateCrossWindowFlow(4, 71, 48371)
		if !privateNativeDataSyn(t, harness, path, reraceSendTimeout) || harness.boundTo(path) != first {
			t.Fatal("original native source did not acquire the preferred owner")
		}
		update := harness.update(path)
		owner := synRetryControlOwner(t, update)
		if owner.credit.noBudget || protocolClassPureTcpControl(owner.packet) {
			t.Fatal("DATA-credit readiness fixture retained a protocol-only SYN")
		}
		original := bytes.Clone(owner.packet)
		harness.exit("first").sendTo(first.ClientId(), reraceProviderSignal(t, path))
		harness.settle()
		privateCrossWindowAssertFailed(t, harness, path, "first")
		update.stateLock.Lock()
		race := update.race
		initial := race != nil && !race.single && race.clientLimit == 2 && len(race.clientStates) == 2 &&
			race.clientStates[refused] != nil && race.clientStates[live] != nil && race.pendingSendCount == 0 &&
			update.synRetry == owner && owner.references == 1 && !owner.queued
		update.stateLock.Unlock()
		if !initial || len(harness.exit("refused").syns(path)) != 1 || len(harness.exit("live").syns(path)) != 1 {
			t.Fatal("initial retained retry did not form exactly the original two-wide race")
		}

		var barrierPath *IpPath
		var barrierUpdate *multiClientChannelUpdate
		if gate == "take" {
			barrierPath = privateCrossWindowFlow(4, 73, 48373)
			if !privateCrossWindowNativeSyn(harness, barrierPath) || harness.boundTo(barrierPath) != first {
				t.Fatal("independent reaper barrier did not acquire its actual first source")
			}
			barrierUpdate = harness.update(barrierPath)
		}
		firstDispatchEntered, firstDispatchRelease := make(chan struct{}), make(chan struct{})
		secondDispatchEntered, secondDispatchRelease := make(chan struct{}), make(chan struct{})
		nativeEntered, nativeRelease := make(chan struct{}), make(chan struct{})
		var firstReleaseOnce, secondReleaseOnce, nativeReleaseOnce sync.Once
		releaseFirst := func() { firstReleaseOnce.Do(func() { close(firstDispatchRelease) }) }
		releaseSecond := func() { secondReleaseOnce.Do(func() { close(secondDispatchRelease) }) }
		releaseNative := func() { nativeReleaseOnce.Do(func() { close(nativeRelease) }) }
		t.Cleanup(func() { releaseFirst(); releaseSecond(); releaseNative(); credit.releaseCapacity(); harness.settle() })
		var dispatchCalls, nativeCalls atomic.Int32
		var gateCalls atomic.Int32
		harness.multiClient.beforeSynRetryDispatchForTest = func(current *multiClientChannelUpdate) {
			if barrierUpdate != nil && current == barrierUpdate {
				close(firstDispatchEntered)
				<-firstDispatchRelease
				return
			}
			if current != update {
				return
			}
			count := dispatchCalls.Add(1)
			if count == 1 && gate == "dispatch" {
				close(firstDispatchEntered)
				<-firstDispatchRelease
			} else if count == 2 || count == 1 && gate == "take" {
				close(secondDispatchEntered)
				<-secondDispatchRelease
			}
		}
		if gate == "selection" {
			harness.multiClient.beforePacketGroupGateForTest = func(current *multiClientChannelUpdate, _ *multiClientChannel, group *parsedPacketGroup) {
				if current == update && group.isSynRetry() && gateCalls.Add(1) == 1 {
					close(firstDispatchEntered)
					<-firstDispatchRelease
				}
			}
		}
		// Route this exact target to an owned real source; other source flows
		// keep a fixed actual pair while the reaper barrier is held.
		harness.multiClient.groupRaceCandidatesForTest = func(group *parsedPacketGroup) []*multiClientChannel {
			if !reraceSameFlow(group.ipPath, path) {
				return []*multiClientChannel{refused, live}
			}
			if group.ipPath.Rst || group.ipPath.SequenceNumber != path.SequenceNumber {
				return []*multiClientChannel{live}
			}
			if gate == "registration" && group.isSynRetry() && gateCalls.Add(1) == 1 {
				close(firstDispatchEntered)
				<-firstDispatchRelease
			}
			return []*multiClientChannel{fresh}
		}
		fresh.beforeGroupCompletionForTest = func() {
			if nativeCalls.Add(1) == 1 {
				close(nativeEntered)
				<-nativeRelease
			}
		}
		if barrierPath != nil {
			harness.exit("first").sendTo(first.ClientId(), reraceProviderSignal(t, barrierPath))
			harness.settle()
		}
		harness.exit("refused").sendTo(refused.ClientId(), reraceProviderSignal(t, path))
		harness.settle()
		privateCrossWindowAssertFailed(t, harness, path, "refused")
		select {
		case <-firstDispatchEntered:
		default:
			t.Fatal("actual provider failure did not reach the first retained dispatch")
		}
		started := credit.base.started.Load()
		terminal := credit.base.terminal.Load()
		if !credit.source.resendCapacityUnavailable.Load() || credit.source.ctx.Err() != nil || len(credit.base.route) != 0 {
			t.Fatal("actual retained source did not remain closed before the native offer")
		}
		nativeResult := make(chan bool, 1)
		go func() {
			packet := MessagePoolCopy(original)
			accepted := [1]bool{}
			count := harness.multiClient.SendNativePacketBatchWithResults(harness.source, protocol.ProvideMode_Network, [][]byte{packet}, 0, accepted[:])
			nativeResult <- count == 1 && accepted[0]
		}()
		harness.settle()
		select {
		case <-nativeEntered:
		default:
			t.Fatal("same native SYN did not register the untried candidate before actual Client admission")
		}
		update.stateLock.Lock()
		filled := update.race == race && len(race.clientStates) == 2 && race.clientStates[live] != nil &&
			race.clientStates[fresh] != nil && race.pendingSendCount == 1 &&
			!harness.multiClient.synRetryDispatchEligibleWithLock(update, race.collapseOrder) &&
			update.synRetry == owner && owner.validWithLock(update) && bytes.Equal(owner.packet, original)
		update.stateLock.Unlock()
		if !filled || len(credit.base.route) != 0 {
			t.Fatal("native candidate did not temporarily occupy the exact retained vacancy without wire admission")
		}
		if retire != "" {
			terminalPath := path
			switch retire {
			case "winner":
				harness.exit("live").sendTo(live.ClientId(), reraceSynAck(path))
				harness.settle()
				if harness.boundTo(path) != live || !update.receivedInbound.Load() {
					t.Fatal("actual winner did not revoke the held retry")
				}
			case "reset":
				packet := MessagePoolCopy(reraceTcpPacket(path, tcpFlagRst|tcpFlagAck, path.SequenceNumber+1, reraceServerIsn+1, nil))
				accepted := synRetryControlOffer(t, harness, packet)
				update.stateLock.Lock()
				cacheWithdrawn := update.synRetry == nil && owner.references == 1
				heldOffers, closeReady, done := update.sequenceSynOffers, update.sequenceCloseReady.Load(), update.IsDone()
				update.stateLock.Unlock()
				t.Logf("RESET_ADMISSION_CUSTODY: accepted=%t cache_withdrawn=%t held_syn_offers=%d close_ready=%t done=%t", accepted, cacheWithdrawn, heldOffers, closeReady, done)
				if !accepted || !cacheWithdrawn || heldOffers != 1 || !closeReady || done {
					t.Fatal("admitted reset did not withdraw its cache while the held native offer deferred retirement")
				}
			case "close":
				update.Close()
				harness.settle()
			case "stale":
				next := *path
				next.SequenceNumber += 1000
				terminalPath = &next
				if !privateCrossWindowNativeSyn(harness, &next) {
					t.Fatal("new native generation did not acquire the real live source")
				}
				update.stateLock.Lock()
				replaced := update.synRetry != nil && update.synRetry != owner && update.synRetry.sequence == next.SequenceNumber
				update.stateLock.Unlock()
				if !replaced {
					t.Fatal("new native generation did not revoke exact old cache identity")
				}
			default:
				t.Fatal("unknown retirement fixture")
			}
			releaseFirst()
			harness.settle()
			releaseNative()
			harness.settle()
			select {
			case <-nativeResult:
			default:
				t.Fatal("retired native producer did not finish its actual result")
			}
			if retire == "reset" && !update.IsDone() {
				t.Fatal("released native offer did not let the admitted reset retire its flow")
			}
			update.stateLock.Lock()
			resurrected := race.synRetryPending || owner.queued
			update.stateLock.Unlock()
			if resurrected || dispatchCalls.Load() != 1 || len(credit.base.route) != 0 ||
				!credit.source.resendCapacityUnavailable.Load() || harness.multiClient.ctx.Err() != nil {
				t.Fatal("revoked dispatch resurrected its request or changed shared source lifetime")
			}
			synRetryControlReleased(t, owner)
			if retire == "stale" {
				harness.exit("live").sendTo(live.ClientId(), reraceSynAck(terminalPath))
				harness.settle()
				if harness.boundTo(path) != live || !update.receivedInbound.Load() || len(harness.delivered(path)) != 1 {
					t.Fatal("old request disturbed the new generation's real reply")
				}
			}
			if harness.update(sibling) != siblingUpdate || harness.boundTo(sibling) != speed || len(harness.delivered(sibling)) != 1 ||
				speed.IsDone() || live.IsDone() || fresh.IsDone() || len(harness.generator.clientList()) != 4 {
				t.Fatal("retired child disturbed healthy siblings or the bounded source census")
			}
			return
		}
		releaseFirst()
		harness.settle()
		update.stateLock.Lock()
		dropped := update.race == race && owner.references == 1 && !owner.queued && owner.validWithLock(update) &&
			race.pendingSendCount == 1 && race.clientStates[fresh] != nil
		update.stateLock.Unlock()
		beforeRefusal := int32(1)
		if gate == "take" {
			beforeRefusal = 0
		}
		if !dropped || dispatchCalls.Load() != beforeRefusal {
			t.Fatal("held retained request did not finish at its now-ineligible gate")
		}

		harness.settle()
		if dispatchCalls.Load() != beforeRefusal || credit.base.started.Load() != started {
			t.Fatal("full unchanged race spun another retry or source admission")
		}

		// The retained seed owns the real resend window until its exact peer ack.
		// Its false public result and candidate removal precede the wake check.
		releaseNative()
		harness.settle()
		select {
		case accepted := <-nativeResult:
			if accepted {
				t.Fatal("native offer bypassed the actual closed resend-capacity gate")
			}
		default:
			t.Fatal("native producer did not complete its real local refusal")
		}
		if credit.base.started.Load() != started+1 || credit.base.terminal.Load() != terminal+1 ||
			!credit.source.resendCapacityUnavailable.Load() || len(credit.base.route) != 0 {
			t.Fatal("native refusal did not complete exactly one actual closed-source admission")
		}
		update.stateLock.Lock()
		reopened := update.race == race && len(race.clientStates) == 1 && race.clientStates[live] != nil &&
			race.clientStates[fresh] == nil && race.pendingSendCount == 0 &&
			harness.multiClient.synRetryDispatchEligibleWithLock(update, race.collapseOrder) && owner.validWithLock(update)
		update.stateLock.Unlock()
		if !reopened || nativeCalls.Load() != 1 || fresh.IsDone() || fresh.dialFailureCount() != 0 {
			t.Fatal("real local refusal did not reopen its original race slot without blaming the provider")
		}
		select {
		case <-secondDispatchEntered:
		default:
			t.Fatalf("RETAINED_SYN_DISPATCH_VACANCY_WAKE_LOST: gate=%s dispatches=%d want=%d native_offers=1 local_refusals=1 reopened=true", gate, dispatchCalls.Load(), beforeRefusal+1)
		}
		credit.releasePeerCredit()
		if credit.source.resendCapacityUnavailable.Load() {
			t.Fatal("exact retained seed acknowledgement did not reopen its source capacity")
		}
		releaseSecond()
		harness.settle()
		if credit.base.started.Load() != started+2 || len(harness.exit("live").syns(path)) != 1 || dispatchCalls.Load() != beforeRefusal+1 {
			t.Fatal("restored request did not send exactly one retained SYN through actual source capacity")
		}
		// Decode and acknowledge the exact fresh retained SYN on the real wire.
		credit.finishPackets(original)
		harness.exit("live").sendTo(live.ClientId(), reraceSynAck(path))
		harness.settle()
		if harness.update(path) != update || harness.boundTo(path) != live || !update.receivedInbound.Load() || len(harness.delivered(path)) != 1 ||
			harness.update(sibling) != siblingUpdate || harness.boundTo(sibling) != speed || len(harness.delivered(sibling)) != 1 ||
			speed.IsDone() || live.IsDone() || fresh.IsDone() || len(harness.generator.clientList()) != 4 {
			t.Fatal("recovered retained reply changed the tuple, healthy sibling, or bounded owner census")
		}
		synRetryControlReleased(t, owner)
		reraceAssertNoErrorDelivered(t, harness, path)
	})
}

// This precondition exercises the same closed source independently of retry
// scheduling. Exact peer acknowledgement makes the same original admissible.
func TestNativeSynRetryVacancyFixtureRefusesActualRetainedSource(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		credit := newNativeBatchCreditFixture(t)
		started, terminal := credit.base.started.Load(), credit.base.terminal.Load()
		accepted := [1]bool{}
		count := credit.base.parent.SendNativePacketBatchWithResults(credit.base.source, protocol.ProvideMode_Network,
			credit.ownPackets(credit.tcp), 0, accepted[:])
		synctest.Wait()
		retainedCount, retainedBytes := credit.source.resendQueue.QueueSize()
		if count != 0 || accepted[0] || credit.base.started.Load() != started+1 || credit.base.terminal.Load() != terminal+1 ||
			!credit.source.resendCapacityUnavailable.Load() || retainedCount != 1 || retainedBytes <= 1 || len(credit.base.route) != 0 ||
			credit.base.selected.IsDone() || credit.base.selected.dialFailureCount() != 0 {
			t.Fatal("actual retained source did not refuse one public native offer without changing its seed or owner")
		}
		credit.releasePeerCredit()
		count = credit.base.parent.SendNativePacketBatchWithResults(credit.base.source, protocol.ProvideMode_Network,
			credit.ownPackets(credit.tcp), 0, accepted[:])
		synctest.Wait()
		if count != 1 || !accepted[0] || credit.base.started.Load() != started+2 {
			t.Fatal("same native original did not become admissible after exact source acknowledgement")
		}
		credit.finishPackets(credit.tcp)
	})
}

// A native registration revokes the second dispatch eligibility check.
func TestNativeSynRetryVacancyReopensAfterQueuedDispatch(t *testing.T) {
	nativeSynRetryVacancyReopens(t, "dispatch", "")
}

// A sibling owns the reaper while the target queues and loses its vacancy.
func TestNativeSynRetryVacancyReopensBeforeQueuedTake(t *testing.T) {
	nativeSynRetryVacancyReopens(t, "take", "")
}

// Packet construction cannot consume the request before its actual group gate.
func TestNativeSynRetryVacancyReopensAtGroupSelection(t *testing.T) {
	nativeSynRetryVacancyReopens(t, "selection", "")
}

// Candidate selection precedes the final flow-locked vacancy recheck.
func TestNativeSynRetryVacancyReopensAtCandidateRegistration(t *testing.T) {
	nativeSynRetryVacancyReopens(t, "registration", "")
}

// A real reply wins while the old dispatch and native producer remain held.
func TestNativeSynRetryVacancyDoesNotRestoreAfterWinner(t *testing.T) {
	nativeSynRetryVacancyReopens(t, "dispatch", "winner")
}

// An admitted native reset closes the exact generation before dispatch returns.
func TestNativeSynRetryVacancyDoesNotRestoreAfterReset(t *testing.T) {
	nativeSynRetryVacancyReopens(t, "dispatch", "reset")
}

// Closing one flow retires its cache while the independent speed flow survives.
func TestNativeSynRetryVacancyDoesNotRestoreAfterFlowClose(t *testing.T) {
	nativeSynRetryVacancyReopens(t, "dispatch", "close")
}

// A successfully admitted new SYN revokes the old cache's suspended request.
func TestNativeSynRetryVacancyDoesNotRestoreStaleCache(t *testing.T) {
	nativeSynRetryVacancyReopens(t, "dispatch", "stale")
}
