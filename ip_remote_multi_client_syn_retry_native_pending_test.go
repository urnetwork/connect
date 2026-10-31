// A real native retransmit can remain queued behind a different source's
// raw-source consumer while another quality contender explicitly fails.
package connect

import (
	"bytes"
	"crypto/sha256"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

// This reproduces ownership at the public native/Transfer boundary, not an
// elapsed-time symptom. The existing raw dequeue barrier holds exactly one
// quality source; no contract request is involved in this NoContract fixture.
func TestNativeSynRetainedFallbackSurvivesQueuedNativeRetransmit(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		var armed atomic.Bool
		var heldPeer Id
		var heldClient *Client
		entered, release := make(chan struct{}), make(chan struct{})
		var enterOnce, releaseOnce sync.Once
		finish := func() { releaseOnce.Do(func() { close(release) }) }
		var harness *reraceHarness
		var targetPath *IpPath
		nativeIdentity := make(chan [32]byte, 1)
		configureHeld := func(settings *ClientSettings) {
			settings.SendBufferSettings.afterGroupAdmissionForTest = func(target sendGroupAdmissionTarget) {
				group, ok := target.(*parsedPacketGroup)
				if !armed.Load() || !ok || group.synRetry == nil || group.synRetry.serial != 0 || len(group.packets) != 1 || !reraceSameFlow(group.ipPath, targetPath) {
					return
				}
				// The held worker cannot consume this raw source admission.
				// Copy identity only inside this actual admission callback.
				identity := sha256.Sum256(group.packets[0].packet)
				select {
				case nativeIdentity <- identity:
				default:
				}
			}
			nativePendingConfigureSourceConsumer(settings, func() *Client { return heldClient }, func(source *SendSequence) {
				id := source.id()
				if !armed.Load() || source.client != heldClient || id.Destination != heldPeer || id.LogicalLane != 0 {
					return
				}
				enterOnce.Do(func() { close(entered) })
				select {
				case <-release:
				case <-source.ctx.Done():
				}
			})
		}
		harness = newPrivateCrossWindowHarness(t, []privateCrossWindowExitSpec{
			{exitSpec: reraceExitSpec{name: "quality-first", tier: 0, answer: reraceAlways(reraceSilent)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "quality-refused", tier: 1, answer: reraceAlways(reraceSilent)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "quality-held", tier: 1, answer: reraceAlways(reraceSilent), clientSettings: configureHeld}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "speed-ready", answer: reraceAlways(reraceSilent)}, windowType: WindowTypeSpeed},
		}, func(settings *MultiClientSettings) { settings.MultiRaceClientCount = 2 })
		t.Cleanup(finish)
		first := privateCrossWindowChannel(t, harness, "quality-first")
		refused := privateCrossWindowChannel(t, harness, "quality-refused")
		held := privateCrossWindowChannel(t, harness, "quality-held")
		ready := privateCrossWindowChannel(t, harness, "speed-ready")
		heldPeer = harness.exit("quality-held").provider.ClientId()
		heldClient = held.client

		// Real progress proves this permitted speed owner and its peer work.
		// The same established flow must survive the independent target race.
		sibling := privateCrossWindowFlow(4, 60, 48360)
		sibling.DestinationPort = 80
		if !privateCrossWindowNativeSyn(harness, sibling) || len(harness.exit("speed-ready").syns(sibling)) != 1 {
			t.Fatal("healthy speed source did not admit its independent native sibling")
		}
		harness.exit("speed-ready").sendTo(ready.ClientId(), reraceSynAck(sibling))
		harness.settle()
		siblingUpdate := harness.update(sibling)
		if harness.boundTo(sibling) != ready || !siblingUpdate.receivedInbound.Load() || len(harness.delivered(sibling)) != 1 {
			t.Fatal("actual speed peer did not establish the independent sibling")
		}

		path := privateCrossWindowFlow(4, 61, 48361)
		targetPath = path
		if !privateCrossWindowNativeSyn(harness, path) || harness.boundTo(path) != first || len(harness.exit("quality-first").syns(path)) != 1 {
			t.Fatal("first actual native SYN did not acquire the preferred quality owner")
		}
		update := harness.update(path)
		owner := synRetryControlOwner(t, update)
		original := bytes.Clone(owner.packet)
		credit, serial := owner.credit, owner.serial
		armed.Store(true)
		harness.exit("quality-first").sendTo(first.ClientId(), reraceProviderSignal(t, path))
		harness.settle()
		privateCrossWindowAssertFailed(t, harness, path, "quality-first")
		select {
		case <-entered:
		default:
			t.Fatal("the exact held quality source never reached its raw materialization consumer")
		}
		update.stateLock.Lock()
		race := update.race
		initial := race != nil && !race.single && len(race.clientStates) == 2 &&
			race.clientStates[refused] != nil && race.clientStates[held] != nil &&
			race.pendingSendCount == 0 && update.client.Load() == nil &&
			update.synRetry == owner && owner.references == 1 && !owner.queued && owner.validWithLock(update)
		update.stateLock.Unlock()
		if !initial || len(harness.exit("quality-refused").syns(path)) != 1 || len(harness.exit("quality-held").syns(path)) != 0 || len(harness.exit("speed-ready").syns(path)) != 0 {
			t.Fatal("retained retry did not form exactly the live/refused quality race before native retransmission")
		}

		// This second public input is the same complete SYN, not a fabricated
		// retry token. Its source admission is real even though held cannot read it.
		if !privateCrossWindowNativeSyn(harness, path) {
			t.Fatal("the actual same-attempt native retransmission was not admitted")
		}
		select {
		case identity := <-nativeIdentity:
			if identity != sha256.Sum256(original) {
				t.Fatal("actual native admission differs from the complete cached SYN")
			}
		default:
			t.Fatal("the real serial-zero native admission did not expose its bounded copied identity")
		}
		update.stateLock.Lock()
		pendingNative := 0
		for group := update.sequenceClaims; group != nil; group = group.collapseNext {
			flags := group.completionFlags.Load()
			const required = groupCompletionAck | groupCompletionTracked | groupCompletionAdmitted | groupCompletionLinked | groupCompletionCandidate
			if group.completionClient != held || group.synRetry == nil || group.synRetry.serial != 0 || flags&required != required ||
				flags&(groupCompletionDequeued|groupCompletionAbandoned|groupCompletionDisposed|groupCompletionDone) != 0 ||
				len(group.packets) != 1 || !reraceSameFlow(group.ipPath, path) || group.packets[0].ipPath.SequenceNumber != path.SequenceNumber {
				continue
			}
			pendingNative++
		}
		validWhileQueued := owner.validWithLock(update)
		preserved := update.race == race && update.client.Load() == nil && update.synRetry == owner &&
			owner.serial == serial && owner.credit == credit && owner.references == 1 && !owner.queued && bytes.Equal(owner.packet, original)
		update.stateLock.Unlock()
		if pendingNative != 1 || !preserved || credit.released.Load() || held.IsDone() {
			t.Fatal("public retransmit did not retain one exact raw held candidate and the original bounded owner")
		}

		harness.exit("quality-refused").sendTo(refused.ClientId(), reraceProviderSignal(t, path))
		harness.settle()
		privateCrossWindowAssertFailed(t, harness, path, "quality-refused")
		if count := len(harness.exit("speed-ready").syns(path)); count != 1 {
			t.Fatalf("NATIVE_RETRANSMIT_PENDING_CLAIM_BLOCKED_RETAINED_FALLBACK: speed_syns=%d want=1 native_inputs=2 pending_native=%d cache_valid_while_queued=%t", count, pendingNative, validWhileQueued)
		}
		update.stateLock.Lock()
		continued := update.race == race && !race.single && len(race.clientStates) == 2 &&
			race.clientStates[held] != nil && race.clientStates[ready] != nil && update.client.Load() == nil
		update.stateLock.Unlock()
		if !continued || len(harness.exit("quality-held").syns(path)) != 0 || held.IsDone() || held.dialFailureCount() != 0 {
			t.Fatal("fallback replaced the exact live quality race or exceeded the vacated slot")
		}
		harness.exit("speed-ready").sendTo(ready.ClientId(), reraceSynAck(path))
		harness.settle()
		if harness.boundTo(path) != ready || !update.receivedInbound.Load() || len(harness.delivered(path)) != 1 {
			t.Fatal("actual speed reply did not win the original retained tuple generation")
		}
		finish()
		harness.settle()
		harness.exit("quality-held").sendTo(held.ClientId(), reraceSynAck(path))
		harness.settle()
		if harness.update(path) != update || harness.boundTo(path) != ready || len(harness.delivered(path)) != 1 ||
			harness.update(sibling) != siblingUpdate || harness.boundTo(sibling) != ready || len(harness.delivered(sibling)) != 1 ||
			held.IsDone() || ready.IsDone() || len(harness.generator.clientList()) != 4 {
			t.Fatal("late quality proof or source release disturbed winner, healthy sibling, or owner census")
		}
		synRetryControlReleased(t, owner)
		reraceAssertNoErrorDelivered(t, harness, path)
	})
}

// This test-only adapter selects the actual source-consumer seam. The getter
// exists only for the preserved unreachable-contract counterfactual.
func nativePendingConfigureSourceConsumer(settings *ClientSettings, _ func() *Client, observe func(*SendSequence)) {
	settings.SendBufferSettings.beforeGroupDequeueForTest = observe
}
