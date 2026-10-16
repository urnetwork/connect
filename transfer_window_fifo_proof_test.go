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

// An actual cumulative prefix ACK is progress toward a still-queued head.
// Once the same reliable FIFO has selectively acknowledged a later write,
// however, that head has already been passed. Its ordinary timer must keep
// ownership even below the three-SACK threshold and with lane recovery off.
func TestWindowPacingFifoProofDefeatsProgressDeferral(t *testing.T) {
	for _, proof := range []bool{false, true} {
		t.Logf("later_ack=%t", proof)
		func() {
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
						scaled := sequence.rttWindow.ScaledRtt()
						dueAt = time.Now()
						nextInterval = sequence.deferredResendInterval(item, scaled)
						wantHighest := uint64(0)
						if proof {
							wantHighest = 2
						}
						if item.sequenceNumber != 1 || item.sendCount != 1 || item.timeoutDeferCount != 0 ||
							!item.reliableCarrierObserved || item.unreliableCarrierObserved || item.carrierChanged ||
							!acked || highest != wantHighest || !sequence.transferFlightPolicy().h1Only ||
							!sequence.lastCumulativeAckTime.After(item.sendTime) ||
							item.selectiveAcked || item.selectiveGapRecovered ||
							sequence.ackWindow.PendingCumulativeProgress() ||
							nextInterval <= 0 {
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
				originalDue := sequence.resendQueue.GetByMessageId(holeId).resendTime
				// These are the actual peer ACK ingress paths, while the third
				// accepted original still owns the send worker's initial hook.
				time.Sleep(100 * time.Millisecond)
				acknowledgeSendPackLifecycleWirePack(t, client, destination, packs[0])
				if proof {
					if ok, err := sequence.Ack(&protocol.Ack{SequenceId: packs[2].SequenceId, MessageId: packs[2].MessageId, Selective: true}, 0); !ok || err != nil {
						t.Fatalf("same-FIFO proof was not admitted: %t %v", ok, err)
					}
				}
				close(resume)
				<-dueReached
				if duePremise != "" || dueAt != originalDue {
					t.Fatalf("first ordinary due boundary changed: got=%s want=%s premise=%s", dueAt, originalDue, duePremise)
				}
				close(dueResume)
				// Half the existing next interval admits the real paid writer
				// but cannot reach a newly granted progress deferral.
				time.Sleep(nextInterval / 2)
				synctest.Wait()
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
				if recovered != proof {
					t.Fatalf("same-FIFO later ACK=%t, head recovered=%t by original due + %s; timeout deferrals=%d", proof, recovered, nextInterval/2, client.timeoutResendDeferCount.Load())
				}
			})
		}()
	}
}

// The stronger proof belongs only to the existing stable paced FIFO case.
// Merely seeing an equal/lower ACK, or another route's progress, cannot
// revoke that item's ordinary deferral. Other profiles keep their old rule.
func TestWindowPacingFifoProofRequiresStableSameRoute(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*SendSequence, *sendItem, *windowPacingPolicyWriter)
		want   bool
	}{
		{name: "strictly later proof", want: false},
		{name: "equal ACK is not later proof", change: func(s *SendSequence, item *sendItem, _ *windowPacingPolicyWriter) {
			s.laneAcks[0].highestAckedSequenceNumber = item.sequenceNumber
		}, want: true},
		{name: "lower prefix still drains", change: func(s *SendSequence, item *sendItem, _ *windowPacingPolicyWriter) {
			s.laneAcks[0].highestAckedSequenceNumber = item.sequenceNumber - 1
		}, want: true},
		{name: "other route cannot prove passage", change: func(s *SendSequence, _ *sendItem, _ *windowPacingPolicyWriter) {
			s.laneAcks[0].route = make(Route, 1)
		}, want: true},
		{name: "no recorded ACK", change: func(s *SendSequence, _ *sendItem, _ *windowPacingPolicyWriter) {
			s.laneAcks[0].set = false
		}, want: true},
		{name: "no original route", change: func(_ *SendSequence, item *sendItem, _ *windowPacingPolicyWriter) {
			item.carrierRoute = nil
		}, want: true},
		{name: "changed carrier keeps bounded policy", change: func(_ *SendSequence, item *sendItem, _ *windowPacingPolicyWriter) {
			item.carrierChanged = true
		}, want: true},
		{name: "unobserved carrier keeps bounded policy", change: func(_ *SendSequence, item *sendItem, _ *windowPacingPolicyWriter) {
			item.reliableCarrierObserved = false
		}, want: true},
		{name: "mixed route keeps bounded policy", change: func(_ *SendSequence, _ *sendItem, writer *windowPacingPolicyWriter) {
			writer.policy.h1Only = false
		}, want: true},
		{name: "unpaced route keeps bounded policy", change: func(s *SendSequence, _ *sendItem, _ *windowPacingPolicyWriter) {
			s.sendBufferSettings.DeliverySizedWindowScale = 0
		}, want: true},
		{name: "unreliable never borrows FIFO progress", change: func(_ *SendSequence, item *sendItem, _ *windowPacingPolicyWriter) {
			item.unreliableCarrierObserved = true
		}, want: false},
	} {
		t.Logf("case: %s", test.name)
		func() {
			settings := DefaultSendBufferSettings()
			settings.DeliverySizedWindowScale = 2
			writer := &windowPacingPolicyWriter{policy: transferFlightPolicySnapshot{h1Only: true}}
			start := time.Now()
			route := make(Route, 1)
			item := &sendItem{transferItem: transferItem{sequenceNumber: 1}, sendTime: start, carrierRoute: route, reliableCarrierObserved: true}
			sequence := &SendSequence{sendBufferSettings: settings, contractMultiRouteWriter: writer,
				lastCumulativeAckTime: start.Add(time.Second)}
			sequence.laneAcks[0] = laneAckSlot{route: route, set: true, highestAckedSequenceNumber: 2}
			if test.change != nil {
				test.change(sequence, item, writer)
			}
			if got := sequence.shouldDeferTimeoutResend(item, time.Second); got != test.want {
				t.Fatalf("progress deferral=%t, want=%t", got, test.want)
			}
		}()
	}
}
