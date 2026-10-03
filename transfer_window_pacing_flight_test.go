// Service flight excludes work still waiting for a pacing deadline and
// accounts for the indivisible size of one physical message.
package connect

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// Completes the real SendSequence pacing callback with a confirmed H1 write.
type windowPacingHandoffWriter struct {
	windowPacingPolicyWriter
}

func (self *windowPacingHandoffWriter) WriteDetailedWithTransport(_ context.Context, bytes []byte, _ time.Duration) (bool, TransportType, error) {
	MessagePoolReturn(bytes)
	return true, TransportTypeH1, nil
}

// An ACK may run after pacing releases a producer but before its caller can
// expose the next physical write. That local handoff is not acknowledged flight.
func TestWindowPacingAdmissionCannotCreditAnUnbegunWrite(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		service := &windowPacingService{sent: 1000}
		sequenceId, earlier, next := NewId(), NewId(), NewId()
		sequence := newEstimatorFixture(t, nil)
		sequence.ctx, sequence.client, sequence.log = context.Background(), &Client{}, NewNoopLogger()
		sequence.sequenceId = sequenceId
		sequence.contractMultiRouteWriter = &windowPacingHandoffWriter{windowPacingPolicyWriter{policy: transferFlightPolicySnapshot{h1Only: true}}}
		sequence.ackWindow = newSequenceAckWindow()
		sequence.resendQueue.Add(&sendItem{transferItem: transferItem{messageId: earlier, sequenceNumber: 1}})
		service.beginWrite(sequenceId, earlier, 1, time.Now(), false)
		service.finishWrite(sequenceId, earlier, true)
		admitted, resume := make(chan struct{}), make(chan struct{})
		sequence.windowPacer = windowBurstPacer{service: service, serviceSequenceId: sequenceId, rate: 1000000, rateUpdated: time.Now(),
			afterAdmissionForTest: func() {
				close(admitted)
				<-resume
			}}
		defer sequence.windowPacer.close()
		item := &sendItem{transferItem: transferItem{messageId: next, sequenceNumber: 2}, expectsAck: true, sendCount: 1, transferFrameBytes: MessagePoolGet(1000)}
		defer item.messagePoolReturn()
		done := make(chan error, 1)
		go func() {
			_, err := sequence.writeMaybeWrappedBytes(item.transferFrameBytes, TransferPath{}, true, item, false, false)
			done <- err
		}()
		<-admitted
		sequence.coalesceReceivedAck(sequence.ackWindow, receiveAckMessage{messageId: earlier, receivedAtNanos: time.Now().UnixNano()})
		close(resume)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if service.drainedSent > 1000 {
			t.Fatalf("the earlier tail credited an unbegun physical write: credited=%d want<=1000", service.drainedSent)
		}
		if service.pendingWrites != 1 || service.writes[sequenceId].messageId != next {
			t.Fatal("admission lost the new message's pending physical write")
		}
	})
}

// A byte-bounded burst can legitimately remain in flight while the previous
// bytes propagate. Use that actual allowance, including packet quantization,
// rather than the retired two-millisecond scheduling allowance.
func TestWindowPacingFlightIncludesEstimatedBurstBytes(t *testing.T) {
	service := &windowPacingService{sent: mib(10) + 116000, total: mib(10), maxMessageByteCount: 1000}
	service.burstMeter.limit = 6000
	service.observeRoundTrip(100*time.Millisecond, 10*time.Millisecond, time.Unix(1700000000, 0))
	if service.backlogged(1000000) {
		t.Fatal("a six kB burst plus 110 kB of propagation was called a standing queue")
	}
	service.sent++
	if !service.backlogged(1000000) {
		t.Fatal("flight beyond propagation and one estimated burst lost queue evidence")
	}
	service.burstMeter.limit = 12000
	if service.backlogged(1000000) {
		t.Fatal("a larger byte estimate did not update its flight allowance")
	}
	service.burstMeter.limit = 2000
	if !service.backlogged(1000000) {
		t.Fatal("a smaller byte estimate retained the retired larger allowance")
	}
}

// A sparse train of large messages must not compound a low rate estimate by
// treating one propagating message as a standing queue. Two messages exceeding
// both the residence bound and one message still supply queue evidence.
func TestWindowPacingOneMessageIsNotAStandingQueue(t *testing.T) {
	for _, rate := range []ByteCount{125000, 1250000} {
		for _, rtt := range []time.Duration{300 * time.Microsecond, 100 * time.Millisecond} {
			synctest.Test(t, func(t *testing.T) {
				service := &windowPacingService{sent: mib(10), total: mib(10)}
				service.observeRoundTrip(rtt, 10*time.Millisecond, time.Now())
				pacer := &windowBurstPacer{service: service, rate: rate}
				defer pacer.close()
				if err := pacer.waitForService(context.Background(), 64*1024); err != nil {
					t.Fatal(err)
				}
				if service.backlogged(rate) {
					t.Fatalf("rate=%d RTT=%s: one 64 KiB message was treated as a standing queue", rate, rtt)
				}
				if err := pacer.waitForService(context.Background(), 64*1024); err != nil {
					t.Fatal(err)
				}
				if float64(128*1024) > float64(rate)*(rtt+12*time.Millisecond).Seconds() && !service.backlogged(rate) {
					t.Fatalf("rate=%d RTT=%s: excess multi-message flight lost its queue evidence", rate, rtt)
				}
			})
		}
	}
}

// Crossing the continuous BDP by less than one whole message is normal
// packetization: the previous message may still be propagating when the next
// pacing deadline arrives. The allowance is added to residence, not its max.
func TestWindowPacingPropagationIncludesOneMessageOfRounding(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service := &windowPacingService{sent: mib(10), total: mib(10)}
		service.observeRoundTrip(100*time.Millisecond, 10*time.Millisecond, time.Now())
		pacer := &windowBurstPacer{service: service, rate: 1000000}
		defer pacer.close()
		for range 2 {
			if err := pacer.waitForService(context.Background(), 64*1024); err != nil {
				t.Fatal(err)
			}
		}
		if service.backlogged(1000000) {
			t.Fatal("128 KiB is within one message of a 112 kB propagating flight")
		}
		if err := pacer.waitForService(context.Background(), 64*1024); err != nil {
			t.Fatal(err)
		}
		if !service.backlogged(1000000) {
			t.Fatal("the message allowance hid excess beyond one serialization quantum")
		}
	})
}

// Concurrent sequences can reserve long future deadlines. Before those
// deadlines, no new bytes have reached the wire and no relay queue is proven.
func TestWindowPacingWaitingReservationsAreNotWireFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		service := &windowPacingService{sent: mib(10), total: mib(10)}
		service.observeRoundTrip(100*time.Millisecond, 10*time.Millisecond, time.Now())
		var workers sync.WaitGroup
		for range 2 {
			workers.Go(func() {
				pacer := &windowBurstPacer{service: service, rate: 125000}
				defer pacer.close()
				if err := pacer.waitForService(ctx, 64*1024); err != context.Canceled {
					t.Errorf("reservation escaped its blocked pacing deadline: %v", err)
				}
			})
		}
		synctest.Wait()
		if service.backlogged(125000) {
			t.Error("two unsent reservations manufactured a physical relay backlog")
		}
		cancel()
		workers.Wait()
		if service.sent != service.total || service.backlogged(125000) {
			t.Fatal("canceled reservations retained service flight")
		}
	})
}

// The ACK worker can prove all sibling tails delivered before their pacing
// waits let the send workers apply retained bytes. That proof must survive
// the next physical write without double-crediting later ACK application.
func TestWindowPacingDeliveredSiblingTailsAreNotWireFlight(t *testing.T) {
	for _, prior := range []ByteCount{0, mib(10)} {
		synctest.Test(t, func(t *testing.T) {
			service := &windowPacingService{sent: prior, total: prior}
			service.observeRoundTrip(100*time.Millisecond, 10*time.Millisecond, time.Now())
			var pacers []*windowBurstPacer
			for range 4 {
				sequenceId, messageId := NewId(), NewId()
				pacer := &windowBurstPacer{service: service, serviceSequenceId: sequenceId, rate: 125000}
				pacers = append(pacers, pacer)
				defer pacer.close()
				if err := pacer.waitForService(context.Background(), 64*1024); err != nil {
					t.Fatal(err)
				}
				service.beginWrite(sequenceId, messageId, 1, time.Now(), false)
				service.finishWrite(sequenceId, messageId, true)
				time.Sleep(100 * time.Millisecond)
				service.acknowledgeWrite(sequenceId, messageId, 1, false, 10*time.Millisecond, time.Now())
			}
			if service.backlogged(125000) {
				t.Fatal("cumulatively delivered sibling tails still counted as relay flight")
			}
			pacer := pacers[0]
			if err := pacer.waitForService(context.Background(), 64*1024); err != nil {
				t.Fatal(err)
			}
			service.beginWrite(pacer.serviceSequenceId, NewId(), 2, time.Now(), false)
			if service.backlogged(125000) {
				t.Fatal("the next physical write revived previously delivered sibling bytes")
			}
			if err := pacer.waitForService(context.Background(), 64*1024); err != nil {
				t.Fatal(err)
			}
			service.beginWrite(pacer.serviceSequenceId, NewId(), 3, time.Now(), false)
			if !service.backlogged(125000) {
				t.Fatal("known delivery did not end the opening allowance for new excess flight")
			}
			for _, p := range pacers {
				service.observe(64*1024, time.Now())
				p.serviceAcked += 64 * 1024
			}
			if service.sent-service.total != 128*1024 || !service.backlogged(125000) {
				t.Fatal("applying the known ACKs changed physical flight or counted delivery twice")
			}
		})
	}
}

// Canceling a delivered-but-unapplied owner removes those bytes from sent.
// The previous aggregate delivery checkpoint must not hide later real flight.
func TestWindowPacingCanceledDeliveredTailCannotHideNewFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service := &windowPacingService{sent: mib(10), total: mib(10)}
		service.observeRoundTrip(100*time.Millisecond, 10*time.Millisecond, time.Now())
		var pacers []*windowBurstPacer
		for range 3 {
			sequenceId, messageId := NewId(), NewId()
			pacer := &windowBurstPacer{service: service, serviceSequenceId: sequenceId, rate: 125000}
			pacers = append(pacers, pacer)
			defer pacer.close()
			if err := pacer.waitForService(context.Background(), 64*1024); err != nil {
				t.Fatal(err)
			}
			service.beginWrite(sequenceId, messageId, 1, time.Now(), false)
			service.finishWrite(sequenceId, messageId, true)
			time.Sleep(100 * time.Millisecond)
			service.acknowledgeWrite(sequenceId, messageId, 1, false, 10*time.Millisecond, time.Now())
		}
		pacers[0].close()
		for _, pacer := range pacers[1:] {
			service.observe(64*1024, time.Now())
			pacer.serviceAcked += 64 * 1024
		}
		for number := uint64(2); number <= 3; number++ {
			pacer := pacers[1]
			messageId := NewId()
			if err := pacer.waitForService(context.Background(), 64*1024); err != nil {
				t.Fatal(err)
			}
			service.beginWrite(pacer.serviceSequenceId, messageId, number, time.Now(), false)
			service.finishWrite(pacer.serviceSequenceId, messageId, true)
		}
		if !service.backlogged(125000) {
			t.Fatal("a canceled owner's delivery checkpoint hid new unacknowledged messages")
		}
	})
}
