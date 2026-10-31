// Real native source ownership and provider returns bound partial-race work
// across source reset, buffered proof, and receive-buffer overflow.
package connect

import (
	"bytes"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

// A public source reset revokes a stopped partial continuation before a fresh
// source generation can reuse the tuple. Its retained bytes remain charged
// until that stopped worker relinquishes its last reference.
func TestNativeSynPartialRaceContinuationRejectsResetGeneration(t *testing.T) {
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
		path := privateCrossWindowFlow(4, 32, 48202)
		var oldSourceOffers, newSourceOffers, retryOffers atomic.Int32
		harness.multiClient.beforePacketGroupGateForTest = func(_ *multiClientChannelUpdate, _ *multiClientChannel, group *parsedPacketGroup) {
			if !reraceSameFlow(group.ipPath, path) || !group.ipPath.Syn || group.ipPath.Ack {
				return
			}
			if group.isSynRetry() {
				retryOffers.Add(1)
			} else if group.ipPath.SequenceNumber == path.SequenceNumber {
				oldSourceOffers.Add(1)
			} else {
				newSourceOffers.Add(1)
			}
		}
		if !privateCrossWindowNativeSyn(harness, path) {
			t.Fatal("original native source was refused")
		}
		update := harness.update(path)
		owner := synRetryControlOwner(t, update)
		update.stateLock.Lock()
		race := update.race
		credit := owner.credit
		initialRace := race != nil && !race.single && len(race.clientStates) == 2 &&
			race.clientStates[refused] != nil && race.clientStates[live] != nil
		update.stateLock.Unlock()
		if !initialRace || len(harness.exit("quality-refused").syns(path)) != 1 || len(harness.exit("quality-live").syns(path)) != 1 {
			t.Fatal("the original native source did not own the two quality candidates")
		}
		retry := reraceHoldSynRetry(t, harness, update)
		harness.exit("quality-refused").sendTo(refused.ClientId(), reraceProviderSignal(t, path))
		harness.settle()
		privateCrossWindowAssertFailed(t, harness, path, "quality-refused")
		retry.assertEntered(t)
		update.stateLock.Lock()
		partial := update.race == race && len(race.clientStates) == 1 && race.clientStates[live] != nil &&
			update.client.Load() == nil && owner.references == 2
		update.stateLock.Unlock()
		if !partial || len(harness.exit("speed-ready").syns(path)) != 0 {
			t.Fatal("the stopped continuation did not retain its exact partial race and source")
		}

		reset := MessagePoolCopy(reraceTcpPacket(path, tcpFlagRst|tcpFlagAck, path.SequenceNumber+1, reraceServerIsn+1, nil))
		if !synRetryControlOffer(t, harness, reset) {
			t.Fatal("the actual native source reset was refused")
		}
		update.stateLock.Lock()
		revoked := update.IsDone() && update.race == nil && update.synRetry == nil &&
			owner.references == 1 && owner.packet != nil && owner.credit == credit
		update.stateLock.Unlock()
		if !revoked || credit.released.Load() {
			t.Fatal("source reset did not revoke the old race while preserving stopped-worker custody")
		}
		next := *path
		next.SequenceNumber += 100
		if !privateCrossWindowNativeSyn(harness, &next) {
			t.Fatal("the fresh native source generation was refused")
		}
		current := harness.update(path)
		if current == nil || current == update || current.IsDone() || harness.boundTo(path) != live {
			t.Fatal("the fresh public source did not replace the reset canonical generation")
		}
		newOwner := synRetryControlOwner(t, current)
		if newOwner == owner || newOwner.sequence != next.SequenceNumber || len(harness.exit("quality-live").syns(path)) != 2 {
			t.Fatal("the fresh source did not obtain its own retained bytes and provider write")
		}
		retry.release()
		harness.settle()
		synRetryControlReleased(t, owner)
		if !credit.released.Load() || harness.update(path) != current || synRetryControlOwner(t, current) != newOwner ||
			oldSourceOffers.Load() != 1 || newSourceOffers.Load() != 1 || retryOffers.Load() != 0 ||
			len(harness.exit("quality-refused").syns(path)) != 1 || len(harness.exit("quality-live").syns(path)) != 2 ||
			len(harness.exit("speed-ready").syns(path)) != 0 || ready.IsDone() || live.IsDone() {
			t.Fatal("stale partial work joined or wrote over the fresh generation, or retained its final credit")
		}
		reraceAssertNoErrorDelivered(t, harness, path)
	})
}

// An answer buffered by the original race is evidence even before winner
// commitment sets receivedInbound. Another candidate's explicit failure must
// preserve that exact packet owner and avoid any fresh fallback admission.
func TestNativeSynPartialRaceBufferedProofRetainsOriginalWinner(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		harness := newPrivateCrossWindowHarness(t, []privateCrossWindowExitSpec{
			{exitSpec: reraceExitSpec{name: "quality-refused", answer: reraceAlways(reraceSilent)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "quality-proof", answer: reraceAlways(reraceSilent)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "speed-ready", answer: reraceAlways(reraceSilent)}, windowType: WindowTypeSpeed},
		}, func(settings *MultiClientSettings) {
			settings.MultiRaceClientCount = 2
			settings.MultiRaceClientEarlyCompleteFraction = 1
		})
		refused := privateCrossWindowChannel(t, harness, "quality-refused")
		proofClient := privateCrossWindowChannel(t, harness, "quality-proof")
		path := privateCrossWindowFlow(4, 33, 48203)
		var dispatches, retryOffers atomic.Int32
		harness.multiClient.beforePacketGroupGateForTest = func(_ *multiClientChannelUpdate, _ *multiClientChannel, group *parsedPacketGroup) {
			if group.isSynRetry() && reraceSameFlow(group.ipPath, path) {
				retryOffers.Add(1)
			}
		}
		if !privateCrossWindowNativeSyn(harness, path) {
			t.Fatal("original native source was refused")
		}
		update := harness.update(path)
		owner := synRetryControlOwner(t, update)
		harness.multiClient.beforeSynRetryDispatchForTest = func(candidate *multiClientChannelUpdate) {
			if candidate == update {
				dispatches.Add(1)
			}
		}
		first := reraceSynAck(path)
		harness.exit("quality-proof").sendTo(proofClient.ClientId(), first)
		harness.settle()
		update.stateLock.Lock()
		race := update.race
		var proof *receivePacket
		if race != nil && race.clientStates[proofClient] != nil && len(race.clientStates[proofClient].packets) == 1 {
			proof = race.clientStates[proofClient].packets[0]
		}
		buffered := race != nil && !race.single && race.packetCount == 1 && race.clientsWithPacketCount == 1 &&
			len(race.clientStates) == 2 && proof != nil && proof.Pooled && bytes.Equal(proof.Packet, first) &&
			update.client.Load() == nil && !update.receivedInbound.Load()
		update.stateLock.Unlock()
		if !buffered || len(harness.delivered(path)) != 0 {
			t.Fatal("the original provider proof was not legitimately buffered before commitment")
		}
		harness.exit("quality-refused").sendTo(refused.ClientId(), reraceProviderSignal(t, path))
		harness.settle()
		privateCrossWindowAssertFailed(t, harness, path, "quality-refused")
		update.stateLock.Lock()
		preserved := update.race == race && len(race.clientStates) == 1 && race.clientStates[proofClient] != nil &&
			len(race.clientStates[proofClient].packets) == 1 && race.clientStates[proofClient].packets[0] == proof &&
			proof.Pooled && race.packetCount == 1 && race.clientsWithPacketCount == 1 && update.client.Load() == nil
		update.stateLock.Unlock()
		if !preserved || harness.update(path) != update || synRetryControlOwner(t, update) != owner ||
			dispatches.Load() != 0 || retryOffers.Load() != 0 || len(harness.exit("speed-ready").syns(path)) != 0 ||
			len(harness.exit("quality-proof").syns(path)) != 1 || proofClient.dialFailureCount() != 0 {
			t.Fatal("a partial failure replaced buffered proof or admitted fresh fallback over it")
		}
		// Negative fallback proof is complete above at durable quiescence.
		// Now let the existing positive comparison deadline commit its sole
		// already-buffered responder; no timer supplies the negative assertion.
		harness.sleep(harness.multiClient.settings.MultiRaceSetOnResponseTimeout)
		delivered := harness.delivered(path)
		if harness.update(path) != update || harness.boundTo(path) != proofClient || !update.receivedInbound.Load() ||
			len(delivered) != 1 || !bytes.Equal(delivered[0].packet, first) || proof.Pooled ||
			dispatches.Load() != 0 || retryOffers.Load() != 0 || len(harness.exit("speed-ready").syns(path)) != 0 {
			t.Fatal("the preserved original buffered proof did not become the exact sole winner")
		}
		synRetryControlReleased(t, owner)
		reraceAssertNoErrorDelivered(t, harness, path)
	})
}

// Buffer pressure commits the selected race while its first pooled response
// still has an owner. The already-buffered packet must precede the borrowed
// current callback packet, and an established sibling keeps making progress.
func TestNativeSynRaceOverflowPreservesBufferedWinnerAndSibling(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		harness := newPrivateCrossWindowHarness(t, []privateCrossWindowExitSpec{
			{exitSpec: reraceExitSpec{name: "quality-winner", answer: reraceAlways(reraceSilent)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "quality-other", answer: reraceAlways(reraceSilent)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "speed-sibling"}, windowType: WindowTypeSpeed},
		}, func(settings *MultiClientSettings) {
			settings.MultiRaceClientCount = 2
			settings.MultiRaceClientEarlyCompleteFraction = 1
			settings.MultiRaceClientPacketMaxCount = 1
		})
		winner := privateCrossWindowChannel(t, harness, "quality-winner")
		other := privateCrossWindowChannel(t, harness, "quality-other")
		speed := privateCrossWindowChannel(t, harness, "speed-sibling")
		sibling := privateCrossWindowFlow(4, 34, 48204)
		sibling.DestinationPort = 80
		if !privateCrossWindowNativeSyn(harness, sibling) || harness.boundTo(sibling) != speed {
			t.Fatal("the independent speed sibling did not establish before overflow")
		}
		siblingUpdate := harness.update(sibling)
		if !siblingUpdate.receivedInbound.Load() || len(harness.delivered(sibling)) != 1 {
			t.Fatal("the speed sibling has no actual returned native-source proof")
		}
		path := privateCrossWindowFlow(4, 35, 48205)
		if !privateCrossWindowNativeSyn(harness, path) {
			t.Fatal("the original native race was refused")
		}
		update := harness.update(path)
		owner := synRetryControlOwner(t, update)
		winner.stateLock.Lock()
		connectsBefore := len(winner.connectSuccessTimes)
		winner.stateLock.Unlock()
		first := reraceSynAck(path)
		second := reraceTcpPacket(path.Reverse(), tcpFlagAck|tcpFlagPsh, reraceServerIsn+1, path.SequenceNumber+1, []byte("second-return"))
		harness.exit("quality-winner").sendTo(winner.ClientId(), first)
		harness.settle()
		update.stateLock.Lock()
		race := update.race
		var proof *receivePacket
		if race != nil && race.clientStates[winner] != nil && len(race.clientStates[winner].packets) == 1 {
			proof = race.clientStates[winner].packets[0]
		}
		buffered := race != nil && !race.single && race.packetCount == 1 && len(race.clientStates) == 2 &&
			race.clientStates[other] != nil && proof != nil && proof.Pooled && bytes.Equal(proof.Packet, first) &&
			update.client.Load() == nil && !update.receivedInbound.Load()
		update.stateLock.Unlock()
		if !buffered || len(harness.delivered(path)) != 0 {
			t.Fatal("the first winner packet was not retained at the actual configured race limit")
		}
		harness.exit("quality-winner").sendTo(winner.ClientId(), second)
		harness.settle()
		delivered := harness.delivered(path)
		if len(delivered) != 2 {
			t.Errorf("RACE_OVERFLOW_LOST_BUFFERED_WINNER: app_packets=%d want=2 original_buffered=1 current_return=1", len(delivered))
		} else if !bytes.Equal(delivered[0].packet, first) || !bytes.Equal(delivered[1].packet, second) {
			t.Error("race overflow changed the selected response bytes or their original order")
		}
		update.stateLock.Lock()
		committed := update.race == nil && update.client.Load() == winner && update.receivedInbound.Load() &&
			len(race.clientStates) == 0 && !proof.Pooled
		update.stateLock.Unlock()
		harness.multiClient.stateLock.Lock()
		winnerBooked := harness.multiClient.clientUpdates[winner][update]
		otherBooked := harness.multiClient.clientUpdates[other][update]
		siblingBooked := harness.multiClient.clientUpdates[speed][siblingUpdate]
		harness.multiClient.stateLock.Unlock()
		winner.stateLock.Lock()
		connectRecorded := len(winner.connectSuccessTimes) == connectsBefore+1 &&
			len(winner.connectSuccessVersions) == len(winner.connectSuccessTimes) &&
			winner.connectSuccessVersions[connectsBefore] == path.Version
		winner.stateLock.Unlock()
		if !winnerBooked || otherBooked || !siblingBooked || !connectRecorded {
			t.Fatal("overflow lost exact winner/sibling flow bookkeeping or counted the upstream connect more than once")
		}
		if !committed || harness.update(path) != update || winner.IsDone() || other.IsDone() || speed.IsDone() ||
			len(harness.exit("quality-winner").syns(path)) != 1 || len(harness.exit("quality-other").syns(path)) != 1 ||
			len(harness.exit("speed-sibling").syns(path)) != 0 || len(harness.generator.clientList()) != 3 {
			t.Fatal("overflow changed source generation, candidate lifetimes, or actual owner census")
		}
		synRetryControlReleased(t, owner)
		before := len(harness.delivered(sibling))
		if !harness.send(reraceSegment(sibling, 0, []byte("sibling-after-overflow"))) ||
			harness.update(sibling) != siblingUpdate || harness.boundTo(sibling) != speed ||
			len(harness.delivered(sibling)) != before+1 {
			t.Fatal("overflow stopped the established sibling or changed its generation")
		}
		reraceAssertNoErrorDelivered(t, harness, path)
	})
}
