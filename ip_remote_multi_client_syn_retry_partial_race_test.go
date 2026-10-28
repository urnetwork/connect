// A standing explicit failure may fill one vacated race slot from an already
// owned permitted window without resending a live candidate or selecting early.
package connect

import (
	"sync/atomic"
	"testing"
	"testing/synctest"
)

// The real native source, provider signal, retained owner and raced return all
// participate. Durable quiescence supplies the negative observation, not a timer.
func TestNativeSynPartialRaceRefusalVisitsUntriedOwnedWindow(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		harness := newPrivateCrossWindowHarness(t, []privateCrossWindowExitSpec{
			{exitSpec: reraceExitSpec{name: "quality-refused", answer: reraceAlways(reraceSilent)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "quality-live", answer: reraceAlways(reraceSilent)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "speed-ready", answer: reraceAlways(reraceSilent)}, windowType: WindowTypeSpeed},
		}, func(settings *MultiClientSettings) {
			settings.MultiRaceClientCount = 2
		})
		refused := privateCrossWindowChannel(t, harness, "quality-refused")
		live := privateCrossWindowChannel(t, harness, "quality-live")
		ready := privateCrossWindowChannel(t, harness, "speed-ready")
		// An independent public source proves the owned speed carrier actually
		// writes and returns. Port 80 ordinarily prefers speed; no profile edit.
		sibling := privateCrossWindowFlow(4, 30, 48200)
		sibling.DestinationPort = 80
		if !privateCrossWindowNativeSyn(harness, sibling) || len(harness.exit("speed-ready").syns(sibling)) != 1 {
			t.Fatal("owned speed premise did not admit an actual independent native SYN")
		}
		harness.exit("speed-ready").sendTo(ready.ClientId(), reraceSynAck(sibling))
		harness.settle()
		siblingUpdate := harness.update(sibling)
		if harness.boundTo(sibling) != ready || !siblingUpdate.receivedInbound.Load() || len(harness.delivered(sibling)) != 1 {
			t.Fatal("owned speed carrier did not return its independent authorized fixture reply")
		}
		for _, client := range []*multiClientChannel{refused, live, ready} {
			if client.IsDone() || client.isWarning() || client.isQuarantined() {
				t.Fatal("partial-race premise includes an unavailable owned candidate")
			}
		}
		path := privateCrossWindowFlow(4, 31, 48201)
		var sourceOffers, retryOffers atomic.Int32
		harness.multiClient.beforePacketGroupGateForTest = func(_ *multiClientChannelUpdate, _ *multiClientChannel, group *parsedPacketGroup) {
			if reraceSameFlow(group.ipPath, path) {
				if group.isSynRetry() {
					retryOffers.Add(1)
				} else {
					sourceOffers.Add(1)
				}
			}
		}
		if !privateCrossWindowNativeSyn(harness, path) {
			t.Fatal("original native source SYN was not admitted")
		}
		update := harness.update(path)
		owner := synRetryControlOwner(t, update)
		update.stateLock.Lock()
		race := update.race
		validRace := race != nil && !race.single && len(race.clientStates) == 2 &&
			race.clientStates[refused] != nil && race.clientStates[live] != nil && update.client.Load() == nil
		var collapseOrder uint64
		if race != nil {
			collapseOrder = race.collapseOrder
		}
		update.stateLock.Unlock()
		if !validRace || collapseOrder == 0 || len(harness.exit("quality-refused").syns(path)) != 1 ||
			len(harness.exit("quality-live").syns(path)) != 1 || len(harness.exit("speed-ready").syns(path)) != 0 {
			t.Fatal("quality did not receive the first two-candidate race with one retained native source")
		}
		harness.exit("quality-refused").sendTo(refused.ClientId(), reraceProviderSignal(t, path))
		harness.settle()
		privateCrossWindowAssertFailed(t, harness, path, "quality-refused")
		update.stateLock.Lock()
		sameRace := update.race == race && update.race != nil && update.race.collapseOrder == collapseOrder
		liveStillEligible := sameRace && update.race.clientStates[live] != nil && update.client.Load() == nil
		update.stateLock.Unlock()
		if !liveStillEligible || harness.update(path) != update || synRetryControlOwner(t, update) != owner ||
			sourceOffers.Load() != 1 || len(harness.exit("quality-live").syns(path)) != 1 ||
			len(harness.exit("quality-refused").syns(path)) != 1 || live.dialFailureCount() != 0 || refused.dialFailureCount() != 1 {
			t.Fatal("partial refusal lost live candidate, retained source, generation, or exact failure accounting")
		}
		if got := len(harness.exit("speed-ready").syns(path)); got != 1 {
			t.Fatalf("PARTIAL_RACE_OWNED_FIELD_BLOCKED_READY_WINDOW: fresh_speed_syns=%d want=1 with live_quality_owned=1 source_syns=1", got)
		}
		if retryOffers.Load() != 1 {
			t.Fatal("one explicit partial refusal did not create exactly one retained continuation")
		}
		// A continuation fills the vacated slot in the original race. It must
		// not commit a new single-candidate race or revoke the live old reply.
		update.stateLock.Lock()
		joinedRace := update.race == race && !race.single && len(race.clientStates) == 2 &&
			race.clientStates[live] != nil && race.clientStates[ready] != nil && update.client.Load() == nil
		update.stateLock.Unlock()
		if !joinedRace {
			t.Fatal("fresh window offer replaced the original race or exceeded its two live slots")
		}
		harness.exit("quality-live").sendTo(live.ClientId(), reraceSynAck(path))
		harness.settle()
		if harness.boundTo(path) != live || !update.receivedInbound.Load() || len(harness.delivered(path)) != 1 {
			t.Fatal("a still-live original candidate could not win after continuation")
		}
		harness.exit("speed-ready").sendTo(ready.ClientId(), reraceSynAck(path))
		harness.settle()
		if harness.boundTo(path) != live || len(harness.delivered(path)) != 1 ||
			harness.boundTo(sibling) != ready || harness.update(sibling) != siblingUpdate || !siblingUpdate.receivedInbound.Load() ||
			ready.IsDone() || len(harness.generator.clientList()) != 3 {
			t.Fatal("late continuation reply changed winner, healthy sibling, or owned client census")
		}
		synRetryControlReleased(t, owner)
		reraceAssertNoErrorDelivered(t, harness, path)
	})
}
