// An exact capacity grant cannot erase a retained race request while a native
// admission temporarily owns its vacancy and later refuses at real capacity.
package connect

import (
	"bytes"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Both paths retain the same cached SYN and unanswered race. Only the barrier
// selecting readiness's first or second vacancy check changes between them.
func testNativeSynRetryReadinessVacancyReopens(t *testing.T, secondCheck bool) {
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
		speed := privateCrossWindowChannel(t, harness, "speed")
		fresh := credit.base.selected

		sibling := privateCrossWindowFlow(4, 72, 48372)
		sibling.DestinationPort = 80
		if !privateCrossWindowNativeSyn(harness, sibling) || harness.boundTo(sibling) != speed {
			t.Fatal("healthy sibling did not acquire its real speed source")
		}
		harness.exit("speed").sendTo(speed.ClientId(), reraceSynAck(sibling))
		harness.settle()
		siblingUpdate := harness.update(sibling)
		if !siblingUpdate.receivedInbound.Load() || len(harness.delivered(sibling)) != 1 {
			t.Fatal("healthy sibling did not establish through its actual peer")
		}

		path := privateCrossWindowFlow(4, 73, 48373)
		if !privateNativeDataSyn(t, harness, path, reraceSendTimeout) || harness.boundTo(path) != first {
			t.Fatal("initial native SYN did not acquire the preferred source")
		}
		update := harness.update(path)
		owner := synRetryControlOwner(t, update)
		if owner.credit.noBudget || protocolClassPureTcpControl(owner.packet) {
			t.Fatal("DATA-credit readiness fixture retained a protocol-only SYN")
		}
		original := bytes.Clone(owner.packet)
		packetCredit, serial := owner.credit, owner.serial
		harness.exit("first").sendTo(first.ClientId(), reraceProviderSignal(t, path))
		harness.settle()
		privateCrossWindowAssertFailed(t, harness, path, "first")
		update.stateLock.Lock()
		race := update.race
		initial := race != nil && !race.single && race.clientLimit == 2 &&
			len(race.clientStates) == 2 && race.clientStates[refused] != nil && race.clientStates[live] != nil &&
			race.pendingSendCount == 0 && update.synRetry == owner && owner.references == 1 && !owner.queued
		update.stateLock.Unlock()
		if !initial || len(harness.exit("refused").syns(path)) != 1 || len(harness.exit("live").syns(path)) != 1 {
			t.Fatal("actual first refusal did not form the original two-wide unanswered race")
		}

		readinessEntered, readinessRelease := make(chan struct{}), make(chan struct{})
		dispatchEntered, dispatchRelease := make(chan struct{}), make(chan struct{})
		nativeEntered, nativeRelease := make(chan struct{}), make(chan struct{})
		var readinessOnce, dispatchOnce, nativeOnce sync.Once
		releaseReadiness := func() { readinessOnce.Do(func() { close(readinessRelease) }) }
		releaseDispatch := func() { dispatchOnce.Do(func() { close(dispatchRelease) }) }
		releaseNative := func() { nativeOnce.Do(func() { close(nativeRelease) }) }
		var dispatchCalls, readinessCalls, nativeCalls atomic.Int32
		var holdNative atomic.Bool
		var nativeDone chan struct{}
		t.Cleanup(func() {
			releaseReadiness()
			releaseDispatch()
			releaseNative()
			credit.releaseCapacity()
			harness.settle()
			if nativeDone != nil {
				<-nativeDone
			}
		})
		harness.multiClient.beforeSynRetryDispatchForTest = func(current *multiClientChannelUpdate) {
			if current == update && dispatchCalls.Add(1) == 2 {
				close(dispatchEntered)
				select {
				case <-dispatchRelease:
				case <-harness.multiClient.ctx.Done():
				}
			}
		}
		harness.multiClient.beforeSynRetryReadinessForTest = func(current *multiClientChannelUpdate) {
			if current != update {
				return
			}
			if readinessCalls.Add(1) == 1 && secondCheck {
				close(readinessEntered)
				select {
				case <-readinessRelease:
				case <-harness.multiClient.ctx.Done():
				}
			}
		}
		harness.multiClient.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel {
			return []*multiClientChannel{fresh}
		}
		harness.exit("refused").sendTo(refused.ClientId(), reraceProviderSignal(t, path))
		harness.settle()
		privateCrossWindowAssertFailed(t, harness, path, "refused")
		update.stateLock.Lock()
		ready := owner.readiness
		registered := update.race == race && len(race.clientStates) == 1 && race.clientStates[live] != nil &&
			race.pendingSendCount == 0 && update.synRetry == owner && owner.references == 1 && !owner.queued &&
			ready != nil && ready.indexed.Load() && ready.count == 1 && ready.references.Load() == 2 &&
			ready.raceOrder == race.collapseOrder && owner.validWithLock(update)
		update.stateLock.Unlock()
		if !registered || dispatchCalls.Load() != 1 || readinessCalls.Load() != 0 ||
			!credit.source.resendCapacityUnavailable.Load() || credit.source.resendCapacityUnavailable.waiterCount.Load() != 1 ||
			len(credit.base.route) != 0 || fresh.dialFailureCount() != 0 {
			t.Fatal("actual retained source refusal did not install exactly one quiet readiness owner")
		}

		// Only the selected public native offer takes this barrier. The refill
		// and later retained retry use their actual synchronous admission paths.
		fresh.beforeGroupCompletionForTest = func() {
			if holdNative.CompareAndSwap(true, false) {
				nativeCalls.Add(1)
				close(nativeEntered)
				select {
				case <-nativeRelease:
				case <-harness.multiClient.ctx.Done():
				}
			}
		}
		nativeResult := make(chan bool, 1)
		startNative := func() {
			holdNative.Store(true)
			nativeDone = make(chan struct{})
			go func() {
				defer close(nativeDone)
				packet := MessagePoolCopy(original)
				accepted := [1]bool{}
				count := harness.multiClient.SendNativePacketBatchWithResults(harness.source,
					protocol.ProvideMode_Network, [][]byte{packet}, 0, accepted[:])
				nativeResult <- count == 1 && accepted[0]
			}()
			harness.settle()
			select {
			case <-nativeEntered:
			default:
				t.Fatal("public native candidate did not reach its real pre-admission barrier")
			}
			update.stateLock.Lock()
			filled := update.race == race && len(race.clientStates) == 2 && race.clientStates[live] != nil &&
				race.clientStates[fresh] != nil && race.pendingSendCount == 1 &&
				!harness.multiClient.synRetryDispatchEligibleWithLock(update, race.collapseOrder) &&
				update.synRetry == owner && owner.serial == serial && owner.credit == packetCredit &&
				owner.validWithLock(update) && bytes.Equal(owner.packet, original)
			update.stateLock.Unlock()
			if !filled || nativeCalls.Load() != 1 {
				t.Fatal("native registration did not temporarily occupy the exact same cached owner's vacancy")
			}
		}

		// A new real reliable data original re-closes the source after the seed
		// ACK. Keep its decoded ACK identity; no memory/gate flag is assigned.
		refillSource := func() *protocol.Ack {
			if credit.source.resendCapacityUnavailable.Load() || credit.base.offer(credit.tcp) != 1 {
				t.Fatal("exact seed ACK did not permit one real source refill")
			}
			harness.settle()
			count, retainedBytes := credit.source.resendQueue.QueueSize()
			if count != 1 || retainedBytes <= 1 || !credit.source.resendCapacityUnavailable.Load() || len(credit.base.route) != 1 {
				t.Fatal("real refill did not retain one reliable original and re-close source capacity")
			}
			wire := <-credit.base.route
			pack := decodeSendPackLifecycleWirePack(t, wire)
			if pack.Nack || len(pack.Frames) != 1 || !bytes.Equal(pack.Frames[0].MessageBytes, credit.tcp) {
				MessagePoolReturn(wire)
				t.Fatal("refill wire changed its actual reliable packet")
			}
			ack := &protocol.Ack{MessageId: append([]byte(nil), pack.MessageId...), SequenceId: append([]byte(nil), pack.SequenceId...)}
			MessagePoolReturn(wire)
			return ack
		}
		var refillAck *protocol.Ack
		if secondCheck {
			credit.releasePeerCredit()
			select {
			case <-readinessEntered:
			default:
				t.Fatal("actual source ACK did not reach readiness's held second check")
			}
			refillAck = refillSource()
			startNative()
			releaseReadiness()
		} else {
			startNative()
			credit.releasePeerCredit()
			refillAck = refillSource()
		}
		harness.settle()
		update.stateLock.Lock()
		dropped := update.race == race && owner.references == 1 && !owner.queued && owner.validWithLock(update) &&
			race.pendingSendCount == 1 && race.clientStates[fresh] != nil && !ready.indexed.Load() &&
			ready.count == 0 && ready.references.Load() == 1
		update.stateLock.Unlock()
		if !dropped || dispatchCalls.Load() != 1 || secondCheck && readinessCalls.Load() != 1 ||
			!secondCheck && readinessCalls.Load() != 0 || len(credit.base.route) != 0 {
			t.Fatal("blocked readiness did not leave only the exact cache and pending native admission")
		}
		started, terminal := credit.base.started.Load(), credit.base.terminal.Load()
		for range 3 {
			harness.multiClient.notifyFlowReaper()
			harness.settle()
		}
		if dispatchCalls.Load() != 1 || credit.base.started.Load() != started || nativeCalls.Load() != 1 {
			t.Fatal("a preserved request spun a producer without a native return or actual capacity edge")
		}

		releaseNative()
		harness.settle()
		select {
		case accepted := <-nativeResult:
			if accepted {
				t.Fatal("native offer bypassed the actual refilled source window")
			}
		default:
			t.Fatal("held public native offer did not finish its actual capacity refusal")
		}
		if credit.base.started.Load() != started+1 || credit.base.terminal.Load() != terminal+1 ||
			!credit.source.resendCapacityUnavailable.Load() || len(credit.base.route) != 0 {
			t.Fatal("native refusal did not complete exactly one real source admission")
		}
		update.stateLock.Lock()
		reopened := update.race == race && len(race.clientStates) == 1 && race.clientStates[live] != nil &&
			race.clientStates[fresh] == nil && race.pendingSendCount == 0 && owner.validWithLock(update) &&
			harness.multiClient.synRetryDispatchEligibleWithLock(update, race.collapseOrder)
		update.stateLock.Unlock()
		if !reopened || fresh.IsDone() || fresh.dialFailureCount() != 0 || packetCredit.released.Load() {
			t.Fatal("actual native refusal changed the canonical owner or failed to reopen its original vacancy")
		}
		select {
		case <-dispatchEntered:
		default:
			t.Fatalf("RETAINED_SYN_READINESS_VACANCY_WAKE_LOST: second_check=%t dispatches=%d want=2 native_offers=1 local_refusals=1 reopened=true",
				secondCheck, dispatchCalls.Load())
		}
		if !credit.base.client.sendBuffer.Ack(credit.base.peer, refillAck, 0) {
			t.Fatal("exact decoded refill acknowledgement was refused")
		}
		harness.settle()
		if credit.source.resendCapacityUnavailable.Load() {
			t.Fatal("exact refill peer ACK did not reopen the retained source")
		}
		releaseDispatch()
		harness.settle()
		if credit.base.started.Load() != started+2 || dispatchCalls.Load() != 2 || len(harness.exit("live").syns(path)) != 1 {
			t.Fatal("restored readiness request did not offer exactly one cached SYN without duplicating its live sibling")
		}
		credit.finishPackets(original)
		harness.exit("live").sendTo(live.ClientId(), reraceSynAck(path))
		harness.settle()
		if harness.update(path) != update || harness.boundTo(path) != live || !update.receivedInbound.Load() ||
			len(harness.delivered(path)) != 1 || harness.update(sibling) != siblingUpdate ||
			harness.boundTo(sibling) != speed || len(harness.delivered(sibling)) != 1 ||
			live.IsDone() || speed.IsDone() || fresh.IsDone() || len(harness.generator.clientList()) != 4 {
			t.Fatal("late original reply changed the tuple, healthy sibling, or bounded candidate owner census")
		}
		synRetryControlReleased(t, owner)
		reraceAssertNoErrorDelivered(t, harness, path)
	})
}

// Native occupancy precedes the actual capacity grant's first readiness check.
func TestNativeSynRetryReadinessVacancyBeforeFirstCheckReopens(t *testing.T) {
	testNativeSynRetryReadinessVacancyReopens(t, false)
}

// A held granted FIFO turn sees native occupancy only at its second check.
func TestNativeSynRetryReadinessVacancyBeforeSecondCheckReopens(t *testing.T) {
	testNativeSynRetryReadinessVacancyReopens(t, true)
}
