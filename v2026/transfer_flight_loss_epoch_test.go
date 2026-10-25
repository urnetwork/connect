// Loss recovery contracts one outstanding datagram flight once, even when
// several of its packets time out before the cumulative acknowledgement.
package connect

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// A delayed cumulative ACK expires an entire flight at the same instant.
// Retransmission must retain its packet cadence without repeatedly halving
// admission for packets sent before the first congestion response.
func TestSendSequenceUnreliableTimeoutBurstPreservesRecoveryFlight(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		settings := flightGateSettings(kib(64))
		settings.Log = NewNoopLogger()
		var client *Client
		clientReady := make(chan struct{})
		settings.beforeClientKeyPublishForTest = func() {
			<-clientReady
			<-client.ctx.Done()
		}
		settings.SendBufferSettings.UnreliableMinimumFlightByteCount = kib(8)
		settings.SendBufferSettings.UnreliableInitialFlightMessageCount = 64
		settings.SendBufferSettings.UnreliableMinimumFlightMessageCount = 4
		settings.SendBufferSettings.UnreliableMaximumFlightMessageCount = 64
		client, peerId, fromPeer, _ := newFlightGateSender(t, settings)
		close(clientReady)
		_, route := addFlightGateRoute(t, client, TransportTypeP2p, 128, true)
		packs := make([]*protocol.Pack, 64)
		for index := range packs {
			sendFlightGateMessage(t, client, peerId, index)
			packs[index] = takeFlightGatePack(t, route, time.Second)
		}
		synctest.Wait()
		time.Sleep(settings.SendBufferSettings.MinResendInterval)
		synctest.Wait()
		stats := client.SendRecoveryStats()
		if stats.UnreliableFlightTimeoutCount != uint64(len(packs)) {
			t.Fatalf("timeout burst recovered %d packets, want %d", stats.UnreliableFlightTimeoutCount, len(packs))
		}
		sequence := flightGateSendSequence(t, client, peerId)
		if stats.UnreliableFlightReductionCount != 1 ||
			sequence.flightController.byteLimit != kib(32) ||
			sequence.flightController.messageLimit != 32 {
			t.Fatalf("one expired flight collapsed admission: reductions=%d flight=%dB/%d messages, want 1 and 32768B/32 messages", stats.UnreliableFlightReductionCount, sequence.flightController.byteLimit, sequence.flightController.messageLimit)
		}
		pendingMessageIds := map[Id]bool{}
		for _, pack := range packs {
			pendingMessageIds[Id(pack.MessageId)] = true
		}
		for range packs {
			resent := takeFlightGatePack(t, route, time.Second)
			messageId := Id(resent.MessageId)
			if !pendingMessageIds[messageId] {
				t.Fatal("timeout changed or repeated an original packet identity")
			}
			delete(pendingMessageIds, messageId)
		}
		ackFlightGatePack(t, client, peerId, fromPeer, packs[len(packs)-1], false)
		synctest.Wait()
		if sequence.flightController.messageCount != 0 {
			t.Fatal("cumulative acknowledgement did not release the recovered flight")
		}

		// A new original packet belongs to a later flight. Its loss must still
		// contract admission, independently of the completed earlier episode.
		sendFlightGateMessage(t, client, peerId, len(packs))
		last := takeFlightGatePack(t, route, time.Second)
		time.Sleep(settings.SendBufferSettings.MinResendInterval)
		synctest.Wait()
		if stats := client.SendRecoveryStats(); stats.UnreliableFlightReductionCount != 2 {
			t.Fatalf("loss in the next flight did not reduce admission: %+v", stats)
		}
		resent := takeFlightGatePack(t, route, time.Second)
		if Id(resent.MessageId) != Id(last.MessageId) {
			t.Fatal("new-flight loss did not retransmit the original packet")
		}
		ackFlightGatePack(t, client, peerId, fromPeer, last, false)
		synctest.Wait()
	})
}

// Selective-gap evidence and later timeout recovery can identify several
// packets from one original flight in either order. They share one response.
func TestSendSequenceUnreliableGapAndTimeoutShareLossFlight(t *testing.T) {
	for _, timeoutFirst := range []bool{false, true} {
		at := time.Unix(1_700_000_000, 0)
		sequence, items := newSelectiveAckRecoveryTestSequence(12, at)
		sequence.client = &Client{}
		sequence.nextSequenceNumber = uint64(len(items))
		settings := sequence.sendBufferSettings
		settings.UnreliableInitialFlightByteCount = kib(64)
		settings.UnreliableMaximumFlightByteCount = kib(64)
		settings.UnreliableInitialFlightMessageCount = 64
		settings.UnreliableMaximumFlightMessageCount = 64
		sequence.flightController = newSendFlightController(settings)
		policy := transferFlightPolicySnapshot{generation: 1, limited: true}
		sequence.flightController.applyPolicy(policy)
		for _, index := range []int{0, 4, 8} {
			sequence.observeCarrierWrite(items[index], transferWriteDisposition{unreliable: true})
		}
		for _, index := range []int{1, 2, 3, 5, 6, 7, 9, 10, 11} {
			items[index].selectiveAcked = true
		}
		if timeoutFirst {
			sequence.observeUnreliableResendTimeout(items[0], policy)
		}
		if !sequence.scheduleSelectiveAckRecovery(at.Add(time.Second)) {
			t.Fatal("three proven datagram gaps were not scheduled")
		}
		for _, index := range []int{0, 4, 8} {
			if items[index].recoveryKind != sendRecoverySelectiveGap {
				t.Fatalf("packet %d lost its gap recovery", index)
			}
			sequence.observeUnreliableResendTimeout(items[index], policy)
		}
		stats := sequence.client.SendRecoveryStats()
		if stats.UnreliableFlightReductionCount != 1 ||
			sequence.flightController.byteLimit != kib(32) ||
			sequence.flightController.messageLimit != 32 {
			t.Fatalf("timeout-first=%t: gap/timeout evidence compounded one loss response: reductions=%d flight=%dB/%d messages", timeoutFirst, stats.UnreliableFlightReductionCount, sequence.flightController.byteLimit, sequence.flightController.messageLimit)
		}
	}
}

// A new carrier generation starts its own cold flight; loss evidence retained
// from the old route must not suppress that new route's congestion response.
func TestSendFlightControllerLossBoundaryResetsWithCarrier(t *testing.T) {
	settings := DefaultSendBufferSettings()
	settings.UnreliableInitialFlightByteCount = kib(64)
	settings.UnreliableMaximumFlightByteCount = kib(64)
	controller := newSendFlightController(settings)
	controller.applyPolicy(transferFlightPolicySnapshot{generation: 1, limited: true})
	if !controller.reduceForSequenceLoss(0, 64) || controller.byteLimit != kib(32) {
		t.Fatal("first route did not contract its lost flight")
	}
	controller.applyPolicy(transferFlightPolicySnapshot{generation: 2, limited: true})
	if !controller.reduceForSequenceLoss(1, 64) || controller.byteLimit != kib(32) {
		t.Fatal("old route's loss boundary suppressed the new route's response")
	}
}
