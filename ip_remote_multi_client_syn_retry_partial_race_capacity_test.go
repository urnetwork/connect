// Partial-race controls keep live admission, degraded fan-out and the existing
// response-window clock composed with real native source and provider replies.
package connect

import (
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// A refusal may arrive after wire serialization but before SendGroup returns.
// The still-held producer owns its slot until return, then its real completion
// wakes one previously requested continuation without another failure or timer.
func TestNativeSynPartialRacePendingAdmissionKeepsSlot(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		path := privateCrossWindowFlow(4, 32, 48202)
		observed := make(chan struct{})
		var observedOnce sync.Once
		harness := newPrivateCrossWindowHarness(t, []privateCrossWindowExitSpec{
			{exitSpec: reraceExitSpec{name: "refused", answer: func(p *IpPath) reraceAnswer {
				if reraceSameFlow(p, path) {
					observedOnce.Do(func() { close(observed) })
				}
				return reraceSilent
			}}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "live", answer: reraceAlways(reraceSilent)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "ready"}, windowType: WindowTypeSpeed},
		}, func(settings *MultiClientSettings) { settings.MultiRaceClientCount = 2 })
		refused := privateCrossWindowChannel(t, harness, "refused")
		live := privateCrossWindowChannel(t, harness, "live")
		ready := privateCrossWindowChannel(t, harness, "ready")
		held, release := make(chan struct{}), make(chan struct{})
		var heldOnce, releaseOnce sync.Once
		finish := func() { releaseOnce.Do(func() { close(release) }) }
		settings := refused.client.sendBuffer.sendBufferSettings
		previous := settings.afterGroupAdmissionForTest
		settings.afterGroupAdmissionForTest = func(target sendGroupAdmissionTarget) {
			if previous != nil {
				previous(target)
			}
			group, ok := target.(*parsedPacketGroup)
			if !ok || group.completionClient != refused || !reraceSameFlow(group.ipPath, path) || group.isSynRetry() {
				return
			}
			select {
			case <-observed:
			case <-harness.ctx.Done():
				return
			}
			heldOnce.Do(func() { close(held) })
			select {
			case <-release:
			case <-harness.ctx.Done():
			}
		}
		t.Cleanup(func() { finish(); harness.settle(); settings.afterGroupAdmissionForTest = previous })
		result := make(chan bool, 1)
		go func() {
			packet := MessagePoolCopy(reraceTcpPacket(path, tcpFlagSyn, path.SequenceNumber, 0, nil))
			accepted := [1]bool{}
			count := harness.multiClient.SendNativePacketBatchWithResults(harness.source, protocol.ProvideMode_Network, [][]byte{packet}, 0, accepted[:])
			result <- count == 1 && accepted[0]
		}()
		harness.settle()
		select {
		case <-held:
		default:
			t.Fatal("producer hold did not follow actual provider SYN observation")
		}
		update := harness.update(path)
		owner := synRetryControlOwner(t, update)
		harness.exit("refused").sendTo(refused.ClientId(), reraceProviderSignal(t, path))
		harness.settle()
		privateCrossWindowAssertFailed(t, harness, path, "refused")
		if len(harness.exit("ready").syns(path)) != 0 || harness.boundTo(path) != nil ||
			len(harness.exit("live").syns(path)) != 1 || live.dialFailureCount() != 0 {
			t.Fatal("withdrawal reused a slot still owned by a pending source admission")
		}
		finish()
		harness.settle()
		select {
		case accepted := <-result:
			if !accepted {
				t.Fatal("original source lost its admitted result")
			}
		default:
			t.Fatal("released source producer did not join")
		}
		if harness.boundTo(path) != ready || len(harness.exit("ready").syns(path)) != 1 ||
			len(harness.exit("refused").syns(path)) != 1 || len(harness.exit("live").syns(path)) != 1 || len(harness.delivered(path)) != 1 {
			t.Fatal("actual slot release did not complete one bounded continuation through the ready window")
		}
		synRetryControlReleased(t, owner)
		reraceAssertNoErrorDelivered(t, harness, path)
	})
}

// Degradation can shrink the allowance after initial admission. An existing
// live candidate spends the sole remaining slot; explicit failure adds no fanout.
func TestNativeSynPartialRaceHonorsNewDegradedBound(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		harness := newPrivateCrossWindowHarness(t, []privateCrossWindowExitSpec{
			{exitSpec: reraceExitSpec{name: "refused", answer: reraceAlways(reraceSilent)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "live", answer: reraceAlways(reraceSilent)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "ready"}, windowType: WindowTypeSpeed},
		}, func(settings *MultiClientSettings) {
			settings.MultiRaceClientCount = 2
			settings.MultiRaceDegradedClientCount = 1
		})
		var degraded atomic.Bool
		harness.multiClient.backendDegradedForTest = degraded.Load
		path := privateCrossWindowFlow(4, 33, 48203)
		if !privateCrossWindowNativeSyn(harness, path) {
			t.Fatal("initial native race was refused")
		}
		update := harness.update(path)
		owner := synRetryControlOwner(t, update)
		live := privateCrossWindowChannel(t, harness, "live")
		refused := privateCrossWindowChannel(t, harness, "refused")
		degraded.Store(true)
		harness.exit("refused").sendTo(refused.ClientId(), reraceProviderSignal(t, path))
		harness.settle()
		privateCrossWindowAssertFailed(t, harness, path, "refused")
		if len(harness.exit("ready").syns(path)) != 0 || harness.boundTo(path) != nil || len(harness.exit("live").syns(path)) != 1 {
			t.Fatal("partial continuation exceeded the current degraded one-candidate bound")
		}
		harness.exit("live").sendTo(live.ClientId(), reraceSynAck(path))
		harness.settle()
		if harness.boundTo(path) != live || len(harness.delivered(path)) != 1 || live.dialFailureCount() != 0 {
			t.Fatal("degraded continuation altered the already-live candidate's response eligibility")
		}
		synRetryControlReleased(t, owner)
		reraceAssertNoErrorDelivered(t, harness, path)
	})
}

// The existing response deadline may already have elapsed without a reply.
// Filling its vacancy preserves the same race and first-late-response decision.
func TestNativeSynPartialRaceContinuationSurvivesElapsedResponseWindow(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		harness := newPrivateCrossWindowHarness(t, []privateCrossWindowExitSpec{
			{exitSpec: reraceExitSpec{name: "refused", answer: reraceAlways(reraceSilent)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "live", answer: reraceAlways(reraceSilent)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "ready"}, windowType: WindowTypeSpeed},
		}, func(settings *MultiClientSettings) { settings.MultiRaceClientCount = 2 })
		path := privateCrossWindowFlow(4, 34, 48204)
		if !privateCrossWindowNativeSyn(harness, path) {
			t.Fatal("initial native race was refused")
		}
		update := harness.update(path)
		owner := synRetryControlOwner(t, update)
		update.stateLock.Lock()
		race := update.race
		update.stateLock.Unlock()
		// Advance the actual production response clock in virtual time; the
		// following state assertion proves that its existing timer ran.
		time.Sleep(harness.multiClient.settings.MultiRaceSetOnResponseTimeout)
		harness.settle()
		update.stateLock.Lock()
		elapsed := update.race == race && race != nil && race.responseWindowElapsed && race.packetCount == 0
		update.stateLock.Unlock()
		if !elapsed {
			t.Fatal("the original unanswered response window did not expire")
		}
		refused := privateCrossWindowChannel(t, harness, "refused")
		ready := privateCrossWindowChannel(t, harness, "ready")
		harness.exit("refused").sendTo(refused.ClientId(), reraceProviderSignal(t, path))
		harness.settle()
		if harness.boundTo(path) != ready || len(harness.delivered(path)) != 1 || len(harness.exit("ready").syns(path)) != 1 ||
			len(harness.exit("live").syns(path)) != 1 || len(harness.exit("refused").syns(path)) != 1 {
			t.Fatal("elapsed original response window lost the fresh candidate's first real reply")
		}
		live := privateCrossWindowChannel(t, harness, "live")
		harness.exit("live").sendTo(live.ClientId(), reraceSynAck(path))
		harness.settle()
		if harness.boundTo(path) != ready || len(harness.delivered(path)) != 1 {
			t.Fatal("late original reply replaced the selected continuation")
		}
		synRetryControlReleased(t, owner)
		reraceAssertNoErrorDelivered(t, harness, path)
	})
}
