// A retained source can survive race replacement without granting stopped
// continuation work authority over the new race on the same canonical flow.
package connect

import (
	"bytes"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

// Explicit failures retire only the first race. An identical public source
// retransmit starts a new wide race while the retained owner, syn sequence,
// serial and credit remain unchanged. Both old queued references must drain
// without offering another packet or changing that new race's candidates.
func TestNativeSynPartialRaceContinuationRejectsReplacedRaceOrder(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		harness := newPrivateCrossWindowHarness(t, []privateCrossWindowExitSpec{
			{exitSpec: reraceExitSpec{name: "quality-a", tier: 0, answer: reraceAlways(reraceSilent)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "quality-b", tier: 0, answer: reraceAlways(reraceSilent)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "quality-c", tier: 2, answer: reraceAlways(reraceSilent)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "quality-d", tier: 2, answer: reraceAlways(reraceSilent)}, windowType: WindowTypeQuality},
		}, func(settings *MultiClientSettings) {
			settings.MultiRaceClientCount = 2
		})
		a := privateCrossWindowChannel(t, harness, "quality-a")
		b := privateCrossWindowChannel(t, harness, "quality-b")
		c := privateCrossWindowChannel(t, harness, "quality-c")
		d := privateCrossWindowChannel(t, harness, "quality-d")
		path := privateCrossWindowFlow(4, 36, 48206)
		var sourceOffers, retryOffers, resumedDispatches atomic.Int32
		harness.multiClient.beforePacketGroupGateForTest = func(_ *multiClientChannelUpdate, _ *multiClientChannel, group *parsedPacketGroup) {
			if !reraceSameFlow(group.ipPath, path) || !group.ipPath.Syn || group.ipPath.Ack {
				return
			}
			if group.isSynRetry() {
				retryOffers.Add(1)
			} else {
				sourceOffers.Add(1)
			}
		}
		if !privateCrossWindowNativeSyn(harness, path) {
			t.Fatal("the original native source was refused")
		}
		update := harness.update(path)
		owner := synRetryControlOwner(t, update)
		update.stateLock.Lock()
		oldRace := update.race
		var oldOrder uint64
		if oldRace != nil {
			oldOrder = oldRace.collapseOrder
		}
		credit, serial := owner.credit, owner.serial
		retained := bytes.Clone(owner.packet)
		initial := oldRace != nil && oldOrder != 0 && !oldRace.single && len(oldRace.clientStates) == 2 &&
			oldRace.clientStates[a] != nil && oldRace.clientStates[b] != nil && update.client.Load() == nil
		update.stateLock.Unlock()
		if !initial || sourceOffers.Load() != 1 || len(harness.exit("quality-a").syns(path)) != 1 ||
			len(harness.exit("quality-b").syns(path)) != 1 || len(harness.exit("quality-c").syns(path)) != 0 ||
			len(harness.exit("quality-d").syns(path)) != 0 {
			t.Fatal("the original source did not own exactly the preferred two-candidate race")
		}
		harness.multiClient.beforeSynRetryDispatchForTest = func(candidate *multiClientChannelUpdate) {
			if candidate == update {
				resumedDispatches.Add(1)
			}
		}
		retry := reraceHoldSynRetry(t, harness, update)
		harness.exit("quality-a").sendTo(a.ClientId(), reraceProviderSignal(t, path))
		harness.settle()
		privateCrossWindowAssertFailed(t, harness, path, "quality-a")
		retry.assertEntered(t)
		update.stateLock.Lock()
		partial := update.race == oldRace && oldRace.collapseOrder == oldOrder && len(oldRace.clientStates) == 1 &&
			oldRace.clientStates[b] != nil && owner.references == 2 && !owner.queued && update.synRetry == owner
		update.stateLock.Unlock()
		if !partial || retryOffers.Load() != 0 || resumedDispatches.Load() != 0 || credit.released.Load() {
			t.Fatal("the first exact-race continuation did not stop with one live candidate and charged worker reference")
		}

		// The second real failure empties the race while its first dispatcher
		// is held. Its independently queued retry owns a third reference.
		harness.exit("quality-b").sendTo(b.ClientId(), reraceProviderSignal(t, path))
		harness.settle()
		privateCrossWindowAssertFailed(t, harness, path, "quality-a", "quality-b")
		retry.assertEntered(t)
		update.stateLock.Lock()
		emptied := update.race == nil && update.client.Load() == nil && update.synRetry == owner &&
			owner.references == 3 && owner.queued && owner.serial == serial && owner.credit == credit &&
			bytes.Equal(owner.packet, retained) && !update.receivedInbound.Load() && !update.IsDone()
		update.stateLock.Unlock()
		select {
		case <-oldRace.ctx.Done():
		default:
			t.Fatal("the old race remained live after both actual provider failures")
		}
		if !emptied || harness.update(path) != update || credit.released.Load() || len(harness.delivered(path)) != 0 {
			t.Fatal("the second failure did not retire only the race while preserving both queued source references")
		}

		// This is the same packet and sequence through public native ingress;
		// neither cache generation nor canonical flow replacement supplies the fence.
		if !privateCrossWindowNativeSyn(harness, path) {
			t.Fatal("the identical source retransmit was refused")
		}
		update.stateLock.Lock()
		newRace := update.race
		var newOrder uint64
		if newRace != nil {
			newOrder = newRace.collapseOrder
		}
		replaced := newRace != nil && newRace != oldRace && newOrder != 0 && newOrder != oldOrder && !newRace.single &&
			len(newRace.clientStates) == 2 && newRace.clientStates[c] != nil && newRace.clientStates[d] != nil &&
			newRace.clientStates[c].pendingSendCount == 0 && newRace.clientStates[d].pendingSendCount == 0 &&
			newRace.clientStates[c].sentPacketCount == 1 && newRace.clientStates[d].sentPacketCount == 1 &&
			newRace.packetCount == 0 && update.client.Load() == nil && update.synRetry == owner &&
			owner.references == 3 && owner.queued && owner.serial == serial && owner.credit == credit &&
			owner.sequence == path.SequenceNumber && bytes.Equal(owner.packet, retained) && !update.receivedInbound.Load()
		update.stateLock.Unlock()
		if !replaced || harness.update(path) != update || sourceOffers.Load() != 2 || retryOffers.Load() != 0 ||
			len(harness.exit("quality-c").syns(path)) != 1 || len(harness.exit("quality-d").syns(path)) != 1 || credit.released.Load() {
			t.Fatal("public same-sequence ingress did not replace only the race while retaining the identical canonical cache")
		}

		retry.release()
		harness.settle()
		update.stateLock.Lock()
		preserved := update.race == newRace && newRace.collapseOrder == newOrder && !newRace.single &&
			len(newRace.clientStates) == 2 && newRace.clientStates[c] != nil && newRace.clientStates[d] != nil &&
			newRace.clientStates[c].pendingSendCount == 0 && newRace.clientStates[d].pendingSendCount == 0 &&
			newRace.clientStates[c].sentPacketCount == 1 && newRace.clientStates[d].sentPacketCount == 1 &&
			newRace.packetCount == 0 && update.client.Load() == nil && update.synRetry == owner &&
			owner.references == 1 && !owner.queued && owner.serial == serial && owner.credit == credit &&
			bytes.Equal(owner.packet, retained) && !update.receivedInbound.Load()
		update.stateLock.Unlock()
		harness.multiClient.stateLock.Lock()
		queueEmpty := harness.multiClient.synRetryHead == nil && harness.multiClient.synRetryTail == nil
		harness.multiClient.stateLock.Unlock()
		if !preserved || !queueEmpty || harness.update(path) != update || sourceOffers.Load() != 2 || retryOffers.Load() != 0 ||
			resumedDispatches.Load() != 1 || credit.released.Load() || len(harness.delivered(path)) != 0 ||
			len(harness.exit("quality-a").syns(path)) != 1 || len(harness.exit("quality-b").syns(path)) != 1 ||
			len(harness.exit("quality-c").syns(path)) != 1 || len(harness.exit("quality-d").syns(path)) != 1 {
			t.Fatal("STALE_PARTIAL_RACE_ORDER_JOINED_REPLACEMENT: stopped or queued old work changed the new race or emitted a retry")
		}
		metrics := harness.metrics()
		if metrics.DialFailuresIntercepted != 2 || metrics.FlowsReraced != 0 || a.dialFailureCount() != 1 ||
			b.dialFailureCount() != 1 || c.dialFailureCount() != 0 || d.dialFailureCount() != 0 {
			t.Fatal("draining superseded race work changed exact candidate failure accounting")
		}
		t.Logf("same canonical flow and retained owner: race order %d -> %d references 3 -> 1 source_offers=%d retry_offers=%d",
			oldOrder, newOrder, sourceOffers.Load(), retryOffers.Load())
		first := reraceSynAck(path)
		harness.exit("quality-c").sendTo(c.ClientId(), first)
		harness.settle()
		delivered := harness.delivered(path)
		if harness.update(path) != update || harness.boundTo(path) != c || !update.receivedInbound.Load() ||
			len(delivered) != 1 || !bytes.Equal(delivered[0].packet, first) {
			t.Fatal("the replacement race's actual candidate could not win on its retained source")
		}
		synRetryControlReleased(t, owner)
		if !credit.released.Load() {
			t.Fatal("the actual new-race winner did not release the last retained source credit")
		}
		harness.exit("quality-d").sendTo(d.ClientId(), reraceSynAck(path))
		harness.settle()
		if harness.boundTo(path) != c || len(harness.delivered(path)) != 1 || len(harness.generator.clientList()) != 4 ||
			a.IsDone() || b.IsDone() || c.IsDone() || d.IsDone() {
			t.Fatal("a late new-race candidate changed the winner or retired an independent provider owner")
		}
		reraceAssertNoErrorDelivered(t, harness, path)
	})
}
