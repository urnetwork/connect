// Datagram timeout recovery follows cumulative delivery progress without
// postponing receiver-proven gaps or a tail that has stopped advancing.
package connect

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Suppress unrelated key publication so every virtual-time send and ACK is
// owned by the test's single application sequence.
func newUnreliableProgressTestSender(t *testing.T, beforeDue func(sendSequenceId, uint64)) (*Client, Id, Route, Route) {
	t.Helper()
	settings := flightGateSettings(kib(64))
	settings.Log = NewNoopLogger()
	settings.SendBufferSettings.UnreliableMinimumFlightByteCount = kib(8)
	settings.SendBufferSettings.UnreliableInitialFlightMessageCount = 64
	settings.SendBufferSettings.UnreliableMaximumFlightMessageCount = 64
	settings.SendBufferSettings.beforeDueResendForTest = beforeDue
	var client *Client
	clientReady := make(chan struct{})
	settings.beforeClientKeyPublishForTest = func() {
		<-clientReady
		<-client.ctx.Done()
	}
	client, peerId, fromPeer, _ := newFlightGateSender(t, settings)
	close(clientReady)
	_, route := addFlightGateRoute(t, client, TransportTypeP2p, 16, true)
	return client, peerId, fromPeer, route
}

// An older cumulative ACK proves that an ordered prefix is still draining.
// The next item may exceed its initial timer without being lost; once the
// prefix stops advancing, its remaining tail must recover within one interval.
func TestSendSequenceUnreliableTimeoutTracksCumulativeProgress(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		client, peerId, fromPeer, route := newUnreliableProgressTestSender(t, nil)
		packs := make([]*protocol.Pack, 3)
		for index := range packs {
			sendFlightGateMessage(t, client, peerId, index)
			packs[index] = takeFlightGatePack(t, route, time.Second)
		}
		synctest.Wait()
		time.Sleep(time.Second)
		ackFlightGatePack(t, client, peerId, fromPeer, packs[0], false)
		synctest.Wait()
		time.Sleep(1500 * time.Millisecond)
		synctest.Wait()
		if stats := client.SendRecoveryStats(); stats.UnreliableFlightTimeoutCount != 0 || stats.UnreliableFlightReductionCount != 0 {
			t.Fatalf("draining datagram prefix was treated as loss: timeouts=%d reductions=%d", stats.UnreliableFlightTimeoutCount, stats.UnreliableFlightReductionCount)
		}
		ackFlightGatePack(t, client, peerId, fromPeer, packs[1], false)
		synctest.Wait()
		time.Sleep(500 * time.Millisecond)
		synctest.Wait()
		if stats := client.SendRecoveryStats(); stats.UnreliableFlightTimeoutCount != 0 {
			t.Fatalf("new cumulative progress did not rearm the tail: timeouts=%d", stats.UnreliableFlightTimeoutCount)
		}

		// No later ACK can cover the missing tail. Its ordinary two-second
		// silence bound and one flight contraction remain intact.
		time.Sleep(1500 * time.Millisecond)
		synctest.Wait()
		stats := client.SendRecoveryStats()
		if stats.UnreliableFlightTimeoutCount != 1 || stats.UnreliableFlightReductionCount != 1 {
			t.Fatalf("silent datagram tail did not recover once: timeouts=%d reductions=%d", stats.UnreliableFlightTimeoutCount, stats.UnreliableFlightReductionCount)
		}
		resent := takeFlightGatePack(t, route, time.Second)
		if Id(resent.MessageId) != Id(packs[2].MessageId) {
			t.Fatal("silence recovery changed the missing tail identity")
		}
		ackFlightGatePack(t, client, peerId, fromPeer, packs[2], false)
		synctest.Wait()
	})
}

// Repeating an already consumed cumulative ACK cannot extend the missing
// tail's silence clock. Only newly acknowledged sequence progress can do so.
func TestSendSequenceUnreliableDuplicateAckDoesNotPostponeTimeout(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		client, peerId, fromPeer, route := newUnreliableProgressTestSender(t, nil)
		sendFlightGateMessage(t, client, peerId, 0)
		first := takeFlightGatePack(t, route, time.Second)
		sendFlightGateMessage(t, client, peerId, 1)
		tail := takeFlightGatePack(t, route, time.Second)
		synctest.Wait()
		time.Sleep(time.Second)
		ackFlightGatePack(t, client, peerId, fromPeer, first, false)
		synctest.Wait()
		time.Sleep(1500 * time.Millisecond)
		ackFlightGatePack(t, client, peerId, fromPeer, first, false)
		synctest.Wait()
		time.Sleep(500 * time.Millisecond)
		synctest.Wait()
		if stats := client.SendRecoveryStats(); stats.UnreliableFlightTimeoutCount != 1 {
			t.Fatalf("duplicate cumulative ACK changed tail recovery: timeouts=%d", stats.UnreliableFlightTimeoutCount)
		}
		resent := takeFlightGatePack(t, route, time.Second)
		if Id(resent.MessageId) != Id(tail.MessageId) {
			t.Fatal("duplicate-ACK recovery changed the missing tail identity")
		}
		ackFlightGatePack(t, client, peerId, fromPeer, tail, false)
		synctest.Wait()
	})
}

// Selective ACKs beyond a hole are direct loss evidence even when cumulative
// progress was recent. Its immediate repair must not wait for silence.
func TestSendSequenceUnreliableSelectiveGapDoesNotWaitForSilence(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		client, peerId, fromPeer, route := newUnreliableProgressTestSender(t, nil)
		packs := make([]*protocol.Pack, 5)
		for index := range packs {
			sendFlightGateMessage(t, client, peerId, index)
			packs[index] = takeFlightGatePack(t, route, time.Second)
		}
		synctest.Wait()
		time.Sleep(time.Second)
		ackFlightGatePack(t, client, peerId, fromPeer, packs[0], false)
		for _, pack := range packs[2:] {
			ackFlightGatePack(t, client, peerId, fromPeer, pack, true)
		}
		synctest.Wait()
		if stats := client.SendRecoveryStats(); stats.SelectiveGapWriteCount != 1 || stats.UnreliableFlightTimeoutCount != 0 || stats.UnreliableFlightReductionCount != 1 {
			t.Fatalf("recent cumulative progress delayed a proven gap: gaps=%d timeouts=%d reductions=%d", stats.SelectiveGapWriteCount, stats.UnreliableFlightTimeoutCount, stats.UnreliableFlightReductionCount)
		}
		resent := takeFlightGatePack(t, route, time.Second)
		if Id(resent.MessageId) != Id(packs[1].MessageId) {
			t.Fatal("gap recovery changed the missing packet identity")
		}
		ackFlightGatePack(t, client, peerId, fromPeer, packs[4], false)
		synctest.Wait()
	})
}

// A cumulative head arriving after the worker snapshot still advances the
// silence clock, even when it acknowledges only a predecessor of the due item.
func TestSendSequenceUnreliablePendingProgressPreemptsTimeout(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		due := make(chan struct{})
		releaseDue := make(chan struct{})
		intercepted := false
		client, peerId, fromPeer, route := newUnreliableProgressTestSender(t, func(_ sendSequenceId, number uint64) {
			if number == 1 && !intercepted {
				intercepted = true
				close(due)
				<-releaseDue
			}
		})
		sendFlightGateMessage(t, client, peerId, 0)
		first := takeFlightGatePack(t, route, time.Second)
		sendFlightGateMessage(t, client, peerId, 1)
		tail := takeFlightGatePack(t, route, time.Second)
		synctest.Wait()
		// Selective receipt parks the predecessor's ordinary timeout without
		// advancing cumulative progress, leaving the tail's timer due first.
		time.Sleep(time.Second)
		ackFlightGatePack(t, client, peerId, fromPeer, first, true)
		synctest.Wait()
		time.Sleep(time.Second)
		<-due
		ackFlightGatePack(t, client, peerId, fromPeer, first, false)
		synctest.Wait()
		close(releaseDue)
		synctest.Wait()
		stats := client.SendRecoveryStats()
		if stats.UnreliableFlightTimeoutCount != 0 || stats.UnreliableFlightReductionCount != 0 {
			t.Fatalf("pending cumulative progress lost to the tail timer: timeouts=%d reductions=%d", stats.UnreliableFlightTimeoutCount, stats.UnreliableFlightReductionCount)
		}
		ackFlightGatePack(t, client, peerId, fromPeer, tail, false)
		synctest.Wait()
	})
}
