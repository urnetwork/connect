// A later acknowledgment on the same paced FIFO revokes only an ordinary
// queue-residence deferral; actual worker tests cover both wire tag profiles.
package connect

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Later same-route proof contradicts an already granted queued classification.
func TestWindowPacingLateFifoProofRevokesExistingDeferral(t *testing.T) {
	testWindowPacingLateFifoProof(t, true, false)
}

// Echoed wire timing changes future backoff but cannot restore queue residence.
func TestWindowPacingLateFifoProofWithEchoedTag(t *testing.T) {
	testWindowPacingLateFifoProof(t, true, true)
}

// A delayed original without later proof retains its existing valid deferral.
func TestWindowPacingNoLateFifoProofPreservesExistingDeferral(t *testing.T) {
	testWindowPacingLateFifoProof(t, false, false)
}

// Three real writes establish the route; cumulative progress grants a deferral
// before the optional selective proof is admitted through the real ack ingress.
func testWindowPacingLateFifoProof(t *testing.T, proof, tagged bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		destination := NewId()
		route := make(Route, 8)
		written := make(chan uint64, 3)
		resume := make(chan struct{})
		dueReached, dueResume := make(chan struct{}), make(chan struct{})
		var dueOnce sync.Once
		var client *Client
		var dueAt time.Time
		var nextInterval time.Duration
		var duePremise string
		settings := DefaultClientSettings()
		settings.Log = NewNoopLogger()
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		settings.SendBufferSettings.WindowSizing = WindowSizingFromDelivery
		settings.SendBufferSettings.ApplyWindowSizing()
		if settings.SendBufferSettings.ReliableLaneProvenRecovery ||
			settings.SendBufferSettings.DeferredItemIsLateForTheScoreboard ||
			settings.SendBufferSettings.SelectiveAckGapThreshold != 3 {
			t.Fatal("fixture changed the production recovery policy")
		}
		settings.SendBufferSettings.afterInitialWriteQueuedForTest = func(id sendSequenceId, number uint64) {
			if id.Destination != destination {
				return
			}
			written <- number
			if number == 2 {
				select {
				case <-resume:
				case <-ctx.Done():
				}
			}
		}
		settings.SendBufferSettings.beforeDueResendForTest = func(id sendSequenceId, number uint64) {
			if id.Destination != destination || number != 1 {
				return
			}
			dueOnce.Do(func() {
				sequence := client.sendBuffer.lookupSendSequence(id, nil)
				item := sequence.sendItems[0]
				highest, acked := sequence.laneHighestAcked(item.carrierRoute)
				dueAt = time.Now()
				nextInterval = sequence.deferredResendInterval(item, sequence.rttWindow.ScaledRtt())
				if item.sequenceNumber != 1 || item.sendCount != 1 || item.timeoutDeferCount != 0 ||
					!item.reliableCarrierObserved || item.unreliableCarrierObserved || item.carrierChanged ||
					!acked || highest != 0 || !sequence.transferFlightPolicy().h1Only ||
					!sequence.lastCumulativeAckTime.After(item.sendTime) ||
					item.selectiveAcked || item.selectiveGapRecovered ||
					sequence.ackWindow.PendingCumulativeProgress() || nextInterval <= 0 {
					duePremise = fmt.Sprintf("head=%d sends=%d defers=%d highest=%d acked=%t cumulative=%s send=%s selective=%t gap=%t interval=%s",
						item.sequenceNumber, item.sendCount, item.timeoutDeferCount, highest, acked,
						sequence.lastCumulativeAckTime, item.sendTime, item.selectiveAcked, item.selectiveGapRecovered, nextInterval)
				}
				close(dueReached)
				select {
				case <-dueResume:
				case <-ctx.Done():
				}
			})
		}
		client = NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
		defer func() {
			cancel()
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
			for len(route) > 0 {
				MessagePoolReturn(<-route)
			}
		}()
		client.ContractManager().AddNoContractPeer(destination)
		client.RouteManager().UpdateTransport(NewSendGatewayTransportWithType(TransportTypeH1), []Route{route})
		packs := make([]*protocol.Pack, 0, 3)
		for number := range 3 {
			frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(1280)}
			clear(frame.MessageBytes)
			if !client.SendWithTimeout(frame, destination, func(error) {}, time.Second) {
				MessagePoolReturn(frame.MessageBytes)
				t.Fatal("original Pack was not admitted")
			}
			if <-written != uint64(number) {
				t.Fatal("actual FIFO changed original order")
			}
			wire := <-route
			packs = append(packs, decodeSendPackLifecycleWirePack(t, wire))
			MessagePoolReturn(wire)
		}
		sequence := client.sendBuffer.lookupSendSequence(sendSequenceId{Destination: destination}, nil)
		holeId, _ := IdFromBytes(packs[1].MessageId)
		head := sequence.resendQueue.GetByMessageId(holeId)
		originalDue := head.resendTime
		originalLifetime := head.sendTime.Add(head.ackTimeout)
		time.Sleep(100 * time.Millisecond)
		acknowledgeSendPackLifecycleWirePack(t, client, destination, packs[0])
		close(resume)
		<-dueReached
		if duePremise != "" || dueAt != originalDue {
			t.Fatalf("first ordinary due boundary changed: got=%s want=%s premise=%s", dueAt, originalDue, duePremise)
		}
		close(dueResume)
		synctest.Wait()
		deferredUntil := head.resendTime
		if head.timeoutDeferCount != 1 || !head.deferralOutstanding ||
			deferredUntil != dueAt.Add(nextInterval) || !time.Now().Before(deferredUntil) ||
			head.sendCount != 1 || len(route) != 0 {
			t.Fatalf("ordinary lower-only progress did not first grant the valid deferral: now=%s deadline=%s count=%d outstanding=%t sends=%d writes=%d",
				time.Now(), deferredUntil, head.timeoutDeferCount, head.deferralOutstanding, head.sendCount, len(route))
		}
		// Select the midpoint of the already granted interval, not a new
		// timeout value. The original due time is now strictly in the past.
		time.Sleep(time.Until(deferredUntil) / 2)
		synctest.Wait()
		if proof {
			ack := &protocol.Ack{SequenceId: packs[2].SequenceId, MessageId: packs[2].MessageId, Selective: true}
			if tagged {
				ack.Tag = packs[2].Tag
			}
			if ok, err := sequence.Ack(ack, 0); !ok || err != nil {
				t.Fatalf("late same-FIFO proof was not admitted: %t %v", ok, err)
			}
		}
		synctest.Wait()
		highest, acked := sequence.laneHighestAcked(head.carrierRoute)
		wantHighest := uint64(0)
		if proof {
			wantHighest = 2
		}
		if !acked || highest != wantHighest || head.selectiveAcked || head.carrierChanged ||
			!head.reliableCarrierObserved || head.unreliableCarrierObserved ||
			!sequence.transferFlightPolicy().h1Only || sequence.selectiveGapRecoveryActive ||
			sequence.ackWindow.PendingCumulativeProgress() || !originalDue.Before(time.Now()) ||
			head.sendTime.Add(head.ackTimeout) != originalLifetime {
			t.Fatalf("late proof boundary changed: highest=%d acked=%t leased=%t changed=%t gap=%t now=%s originalDue=%s",
				highest, acked, head.selectiveAcked, head.carrierChanged, sequence.selectiveGapRecoveryActive, time.Now(), originalDue)
		}
		recovered := false
		for len(route) > 0 {
			wire := <-route
			pack := decodeSendPackLifecycleWirePack(t, wire)
			MessagePoolReturn(wire)
			if string(pack.MessageId) == string(packs[1].MessageId) {
				if pack.SequenceNumber != 1 || !pack.Head {
					t.Fatal("ordinary recovery lost the original identity or promoted Head")
				}
				recovered = true
			}
		}
		// Paid pacing may still own an eligible repair. This assertion does
		// not demand a zero-latency physical write or alter any policy clock.
		eligible := !head.deferralOutstanding && !time.Now().Before(head.resendTime)
		t.Logf("late proof=%t highest=%d originalDue=%s now=%s inheritedDeadline=%s actualDeadline=%s remaining=%s deferrals=%d outstanding=%t sends=%d eligible=%t physicalRecovery=%t",
			proof, highest, originalDue, time.Now(), deferredUntil, head.resendTime, time.Until(head.resendTime),
			head.timeoutDeferCount, head.deferralOutstanding, head.sendCount, eligible, recovered)
		if proof {
			if !eligible && !recovered {
				t.Fatal("later same-FIFO proof left an already-expired ordinary retry classified queued behind its inherited future deferral")
			}
		} else if recovered || eligible || !head.deferralOutstanding || head.resendTime != deferredUntil || head.timeoutDeferCount != 1 {
			t.Fatal("without later proof, a healthy delayed original lost its valid deferral")
		}
	})
}

// Revocation changes the ordinary timer's eligibility, not its ownership,
// backoff history or lifetime. Evidence from another carrier or profile
// cannot take away a queued original's existing deferral.
func TestWindowPacingLateFifoProofPreservesRecoveryOwnership(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*SendSequence, *sendItem, *windowPacingPolicyWriter, time.Time)
		want   bool
	}{
		{name: "strictly later same FIFO", want: true},
		{name: "equal ACK", change: func(s *SendSequence, item *sendItem, _ *windowPacingPolicyWriter, _ time.Time) {
			s.laneAcks[0].highestAckedSequenceNumber = item.sequenceNumber
		}},
		{name: "lower ACK", change: func(s *SendSequence, item *sendItem, _ *windowPacingPolicyWriter, _ time.Time) {
			s.laneAcks[0].highestAckedSequenceNumber = item.sequenceNumber - 1
		}},
		{name: "other route", change: func(s *SendSequence, _ *sendItem, _ *windowPacingPolicyWriter, _ time.Time) {
			s.laneAcks[0].route = make(Route, 1)
		}},
		{name: "no ACK", change: func(s *SendSequence, _ *sendItem, _ *windowPacingPolicyWriter, _ time.Time) {
			s.laneAcks[0].set = false
		}},
		{name: "no original route", change: func(_ *SendSequence, item *sendItem, _ *windowPacingPolicyWriter, _ time.Time) {
			item.carrierRoute = nil
		}},
		{name: "changed carrier", change: func(_ *SendSequence, item *sendItem, _ *windowPacingPolicyWriter, _ time.Time) {
			item.carrierChanged = true
		}},
		{name: "unobserved carrier", change: func(_ *SendSequence, item *sendItem, _ *windowPacingPolicyWriter, _ time.Time) {
			item.reliableCarrierObserved = false
		}},
		{name: "unreliable carrier", change: func(_ *SendSequence, item *sendItem, _ *windowPacingPolicyWriter, _ time.Time) {
			item.unreliableCarrierObserved = true
		}},
		{name: "mixed profile", change: func(_ *SendSequence, _ *sendItem, writer *windowPacingPolicyWriter, _ time.Time) {
			writer.policy.h1Only = false
		}},
		{name: "unpaced profile", change: func(s *SendSequence, _ *sendItem, _ *windowPacingPolicyWriter, _ time.Time) {
			s.sendBufferSettings.DeliverySizedWindowScale = 0
		}},
		{name: "unexpired original timer", change: func(_ *SendSequence, item *sendItem, _ *windowPacingPolicyWriter, _ time.Time) {
			item.deferralOutstanding = false
		}},
		{name: "explicit recovery retains its kind", change: func(_ *SendSequence, item *sendItem, _ *windowPacingPolicyWriter, _ time.Time) {
			item.recoveryKind = sendRecoveryContractMissing
		}},
		{name: "leased item", change: func(_ *SendSequence, item *sendItem, _ *windowPacingPolicyWriter, _ time.Time) {
			item.selectiveAcked = true
		}},
		{name: "already due timer", change: func(s *SendSequence, item *sendItem, _ *windowPacingPolicyWriter, now time.Time) {
			s.setResendTime(item, now.Add(-time.Second))
		}},
		{name: "absent retained owner", change: func(s *SendSequence, item *sendItem, _ *windowPacingPolicyWriter, _ time.Time) {
			s.resendQueue.RemoveByMessageId(item.messageId)
		}},
	} {
		t.Logf("case: %s", test.name)
		func() {
			start := time.Unix(1_700_000_000, 0)
			now := start.Add(3 * time.Second)
			sequence, items := newSelectiveAckRecoveryTestSequence(3, start)
			writer := &windowPacingPolicyWriter{policy: transferFlightPolicySnapshot{h1Only: true}}
			sequence.contractMultiRouteWriter = writer
			sequence.sendBufferSettings.DeliverySizedWindowScale = 2
			sequence.lastCumulativeAckTime = start.Add(time.Second)
			route := make(Route, 1)
			sequence.laneAcks[0] = laneAckSlot{route: route, set: true, highestAckedSequenceNumber: 2}
			item := items[1]
			item.reliableCarrierObserved, item.carrierRoute = true, route
			item.deferralOutstanding, item.timeoutDeferCount = true, 7
			item.timeoutDeferAckTime = sequence.lastCumulativeAckTime
			item.sendCount = 3
			items[2].selectiveAcked = true
			for _, retained := range items {
				retained.expectsAck = true
				retained.ackTimeout = time.Minute
				sequence.ackLifetimes.update(retained)
			}
			sequence.windowPacer.service = &windowPacingService{next: now.Add(time.Second)}
			sequence.windowPacer.serviceReserved = 1280
			if test.change != nil {
				test.change(sequence, item, writer, now)
			}
			deadline, outstanding, kind := item.resendTime, item.deferralOutstanding, item.recoveryKind
			lifetimeIndex := item.ackLifetimeIndex
			lifetime := sequence.ackLifetimes.items[lifetimeIndex-1]
			retained := sequence.resendQueue.GetByMessageId(item.messageId)
			count, bytes := sequence.resendQueue.QueueSize()
			sequence.pendingRecovery = &sendRecoverySelection{messageId: item.messageId, number: item.sequenceNumber,
				sendTime: item.sendTime, due: item.resendTime, sendCount: item.sendCount}
			sequence.scheduleSelectiveAckRecovery(now)
			if test.want {
				if item.resendTime != now || item.deferralOutstanding || sequence.resendQueue.PeekFirst() != item ||
					sequence.selectedRecoveryItem() != nil {
					t.Fatal("proved deferral did not become an ordinary due item with its old selection invalidated")
				}
			} else if item.resendTime != deadline || item.deferralOutstanding != outstanding {
				t.Fatal("unproved or nonordinary timer lost its existing state")
			}
			newCount, newBytes := sequence.resendQueue.QueueSize()
			if newCount != count || newBytes != bytes || sequence.resendQueue.GetByMessageId(item.messageId) != retained ||
				item.sendCount != 3 || item.timeoutDeferCount != 7 || item.timeoutDeferAckTime != sequence.lastCumulativeAckTime ||
				item.recoveryKind != kind || item.selectiveGapRecovered || sequence.selectiveGapRecoveryActive ||
				item.ackLifetimeIndex != lifetimeIndex || sequence.ackLifetimes.items[lifetimeIndex-1] != lifetime ||
				item.sendTime.Add(item.ackTimeout) != start.Add(time.Minute) ||
				sequence.windowPacer.service.next != now.Add(time.Second) || sequence.windowPacer.serviceReserved != 1280 {
				t.Fatal("feedback revocation changed ownership, backoff, lifetime, lease policy or pacing debt")
			}
		}()
	}
}
