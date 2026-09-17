// Explicit write, ACK and carrier transitions reproduce RTT refresh without
// depending on scheduler timing or on the number of samples in an old window.
package connect

import (
	"context"
	"math"
	"testing"
	"testing/synctest"
	"time"
)

// A known drain separates two serialization trains. Its first resumed ACK
// measures propagation, while its gap to the drained tail includes local idle.
// Coalescing can leave only that gap in the ring after the older checkpoint
// expires; it must not replace the already established service rate.
func TestWindowPacingDrainedProbeHoldsServiceUntilFreshEvidence(t *testing.T) {
	for _, covering := range []bool{false, true} {
		start := time.Unix(1700000000, 0)
		service := &windowPacingService{}
		sequenceId, tail, probe := NewId(), NewId(), NewId()
		service.observeRoundTrip(400*time.Millisecond, 50*time.Millisecond, start)
		service.beginWrite(sequenceId, tail, 1, start, false)
		service.finishWrite(sequenceId, tail, true)
		service.observe(2671, start)
		service.observeRoundTrip(2*time.Second, 50*time.Millisecond, start.Add(100*time.Millisecond))
		if delay, _ := service.admitBurst(start.Add(110*time.Millisecond), 2671, false, &windowPacingWaiter{}); delay <= 0 {
			t.Fatal("the service did not request a controlled drain")
		}
		drainedAt := start.Add(550 * time.Millisecond)
		service.acknowledgeWrite(sequenceId, tail, 1, false, 50*time.Millisecond, drainedAt)
		service.observe(6875000, drainedAt)
		if rate, _, _ := service.measured(time.Second, drainedAt); rate != 12500000 {
			t.Fatalf("covering=%t: opening train measured %d B/s", covering, rate)
		}
		service.beginWrite(sequenceId, probe, 2, drainedAt, false)
		service.finishWrite(sequenceId, probe, true)
		ack := probe
		number := uint64(2)
		if covering {
			ack, number = NewId(), 3
		}
		resumedAt := drainedAt.Add(400 * time.Millisecond)
		service.acknowledgeWrite(sequenceId, ack, number, false, 50*time.Millisecond, resumedAt)
		service.observe(2671, resumedAt)
		rate, total, latest := service.measured(time.Second, resumedAt)
		if effective := max(rate, latest); effective != 12500000 {
			t.Errorf("covering=%t: one resumed probe measured idle as service: %d B/s, want 12500000", covering, effective)
		}
		if total != 6875000+2*2671 {
			t.Fatalf("covering=%t: a sampling epoch discarded delivered bytes: %d", covering, total)
		}
		// A late old ACK is still delivery, but cannot bridge the drained
		// gap or contaminate the first fresh serialization interval.
		service.observe(10000000, drainedAt.Add(time.Millisecond))
		service.observe(62500, resumedAt.Add(50*time.Millisecond))
		service.observe(62500, resumedAt.Add(100*time.Millisecond))
		if rate, _, _ := service.measured(time.Second, resumedAt.Add(100*time.Millisecond)); rate != 1250000 {
			t.Errorf("covering=%t: fresh slower service did not replace the hold: %d B/s", covering, rate)
		}
		at := resumedAt.Add(100 * time.Millisecond)
		service.acknowledgeWrite(sequenceId, probe, 2, false, 50*time.Millisecond, at)
		natural := NewId()
		service.beginWrite(sequenceId, natural, 3, at, false)
		service.finishWrite(sequenceId, natural, true)
		service.acknowledgeWrite(sequenceId, natural, 3, false, 50*time.Millisecond, at.Add(50*time.Millisecond))
		if service.serviceEpochAt != resumedAt {
			t.Errorf("covering=%t: a later natural probe reused the controlled drain: epoch=%s want=%s", covering, service.serviceEpochAt, resumedAt)
		}
	}
}

// The receive worker can report a probe before the carrier call returns.
// Readers must not price that isolated ACK's idle gap while H1 confirmation
// is pending, and a failed confirmation must retain the original sample epoch.
func TestWindowPacingProbeAckBeforeWriteConfirmationHoldsService(t *testing.T) {
	for _, h1 := range []bool{false, true} {
		start := time.Unix(1700000000, 0)
		service := &windowPacingService{}
		sequenceId, tail, probe := NewId(), NewId(), NewId()
		service.observeRoundTrip(400*time.Millisecond, 50*time.Millisecond, start)
		service.beginWrite(sequenceId, tail, 1, start, false)
		service.finishWrite(sequenceId, tail, true)
		service.observe(2671, start)
		service.observeRoundTrip(2*time.Second, 50*time.Millisecond, start.Add(100*time.Millisecond))
		if delay, _ := service.admitBurst(start.Add(110*time.Millisecond), 2671, false, &windowPacingWaiter{}); delay <= 0 {
			t.Fatal("the service did not request a controlled drain")
		}
		drainedAt := start.Add(550 * time.Millisecond)
		service.acknowledgeWrite(sequenceId, tail, 1, false, 50*time.Millisecond, drainedAt)
		service.observe(6875000, drainedAt)
		if rate, _, _ := service.measured(time.Second, drainedAt); rate != 12500000 {
			t.Fatalf("h1=%t: opening train measured %d B/s", h1, rate)
		}
		service.beginWrite(sequenceId, probe, 2, drainedAt, false)
		resumedAt := drainedAt.Add(400 * time.Millisecond)
		service.acknowledgeWrite(sequenceId, probe, 2, false, 50*time.Millisecond, resumedAt)
		service.observe(2671, resumedAt)
		if rate, _, latest := service.measured(time.Second, resumedAt); max(rate, latest) != 12500000 {
			t.Errorf("h1=%t: ACK before write confirmation lost the previous service: rate=%d latest=%d", h1, rate, latest)
		}
		service.finishWrite(sequenceId, probe, h1)
		want := ByteCount(6677)
		if h1 {
			want = 12500000
		}
		if rate, _, latest := service.measured(time.Second, resumedAt); max(rate, latest) != want {
			t.Errorf("h1=%t: write confirmation left the wrong sample epoch: rate=%d latest=%d want=%d", h1, rate, latest, want)
		}
	}
}

// A packet can finish before its paced successor is released. That natural
// empty flight still supplies the serialization interval needed to discover
// added capacity, including when its ACK precedes write confirmation.
func TestWindowPacingNaturalProbePreservesSerializationEvidence(t *testing.T) {
	for _, ackFirst := range []bool{false, true} {
		start := time.Unix(1700000000, 0)
		service := &windowPacingService{}
		sequenceId, tail, probe := NewId(), NewId(), NewId()
		service.observeRoundTrip(100*time.Millisecond, 10*time.Millisecond, start)
		service.beginWrite(sequenceId, tail, 1, start, false)
		service.finishWrite(sequenceId, tail, true)
		service.observe(100000, start)
		at := start.Add(100 * time.Millisecond)
		service.acknowledgeWrite(sequenceId, tail, 1, false, 10*time.Millisecond, at)
		service.observe(100000, at)
		if rate, _, _ := service.measured(time.Second, at); rate != 1000000 {
			t.Fatalf("ack-first=%t: initial service=%d", ackFirst, rate)
		}
		service.beginWrite(sequenceId, probe, 2, at, false)
		if !ackFirst {
			service.finishWrite(sequenceId, probe, true)
		}
		at = at.Add(50 * time.Millisecond)
		service.acknowledgeWrite(sequenceId, probe, 2, false, 10*time.Millisecond, at)
		service.observe(100000, at)
		if rate, _, latest := service.measured(time.Second, at); rate != 2000000 {
			t.Errorf("ack-first=%t: natural empty flight lost faster serialization: rate=%d latest=%d", ackFirst, rate, latest)
		}
		if ackFirst {
			service.finishWrite(sequenceId, probe, true)
		}
		if !service.serviceEpochAt.IsZero() || service.roundTrip() != 50*time.Millisecond {
			t.Errorf("ack-first=%t: natural probe changed service epoch or lost RTT: epoch=%s rtt=%s", ackFirst, service.serviceEpochAt, service.roundTrip())
		}
	}
}

// A controlled pause marks only its first resumed physical write. Timeout,
// retry, carrier change and cancellation cannot tag a later natural probe.
func TestWindowPacingAbandonedDrainCannotResetLaterService(t *testing.T) {
	for _, outcome := range []string{"timeout", "retry", "carrier-change", "cancel"} {
		start := time.Unix(1700000000, 0)
		service := &windowPacingService{}
		sequenceId, tail, resumed := NewId(), NewId(), NewId()
		service.observeRoundTrip(time.Millisecond, 0, start)
		service.beginWrite(sequenceId, tail, 1, start, false)
		service.finishWrite(sequenceId, tail, true)
		service.observeRoundTrip(100*time.Millisecond, 0, start.Add(time.Millisecond))
		if delay, _ := service.admitBurst(start.Add(10*time.Millisecond), 1000, false, &windowPacingWaiter{}); delay <= 0 {
			t.Fatalf("%s: controlled drain was not armed", outcome)
		}
		at := start.Add(250 * time.Millisecond)
		switch outcome {
		case "timeout":
			// The first resumed write still has an earlier unacked tail.
			service.beginWrite(sequenceId, resumed, 2, at, false)
			service.finishWrite(sequenceId, resumed, true)
		case "retry":
			service.acknowledgeWrite(sequenceId, tail, 1, false, 0, at)
			service.beginWrite(sequenceId, resumed, 2, at, true)
			service.finishWrite(sequenceId, resumed, true)
			service.beginWrite(sequenceId, resumed, 2, at, false)
			service.finishWrite(sequenceId, resumed, true)
		case "carrier-change":
			service.acknowledgeWrite(sequenceId, tail, 1, false, 0, at)
			service.invalidateProbe(sequenceId)
			service.beginWrite(sequenceId, resumed, 2, at, false)
			service.finishWrite(sequenceId, resumed, true)
		case "cancel":
			pacer := &windowBurstPacer{service: service, serviceSequenceId: sequenceId}
			pacer.close()
			sequenceId = NewId()
			service.beginWrite(sequenceId, resumed, 2, at, false)
			service.finishWrite(sequenceId, resumed, true)
		}
		at = at.Add(time.Millisecond)
		service.acknowledgeWrite(sequenceId, resumed, 2, false, 0, at)
		probe := NewId()
		service.beginWrite(sequenceId, probe, 3, at, false)
		service.finishWrite(sequenceId, probe, true)
		at = at.Add(100 * time.Millisecond)
		service.acknowledgeWrite(sequenceId, probe, 3, false, 0, at)
		if !service.serviceEpochAt.IsZero() || service.roundTrip() != 100*time.Millisecond {
			t.Errorf("%s: abandoned drain marked a later natural probe: epoch=%s rtt=%s", outcome, service.serviceEpochAt, service.roundTrip())
		}
	}
}

// A successful tail ACK ends the controlled gap even if its waiting writer
// is dispatched after the original deadline. No intervening physical write
// means that timer lateness cannot turn this into a natural serialization gap.
func TestWindowPacingLateDispatchKeepsSuccessfulDrainEpoch(t *testing.T) {
	start := time.Unix(1700000000, 0)
	service := &windowPacingService{}
	sequenceId, tail, probe := NewId(), NewId(), NewId()
	service.observeRoundTrip(time.Millisecond, 0, start)
	service.beginWrite(sequenceId, tail, 1, start, false)
	service.finishWrite(sequenceId, tail, true)
	pausedAt := start.Add(20 * time.Millisecond)
	service.observeRoundTrip(100*time.Millisecond, 0, pausedAt.Add(-10*time.Millisecond))
	waiter := &windowPacingWaiter{}
	service.reserve(pausedAt, 1000, 1000000, 1000000, 0, 0, false, waiter)
	delay, _ := service.admitBurst(pausedAt, 1000, false, waiter)
	if delay <= 0 {
		t.Fatal("the controlled drain was not armed")
	}
	service.acknowledgeWrite(sequenceId, tail, 1, false, 0, pausedAt.Add(time.Millisecond))
	resumedAt := pausedAt.Add(delay + time.Millisecond)
	if delay, _ := service.admitBurst(resumedAt, 1000, false, waiter); delay != 0 {
		t.Fatalf("successful drain did not release late dispatch: %s", delay)
	}
	service.beginWrite(sequenceId, probe, 2, resumedAt, false)
	service.finishWrite(sequenceId, probe, true)
	ackedAt := resumedAt.Add(100 * time.Millisecond)
	service.acknowledgeWrite(sequenceId, probe, 2, false, 0, ackedAt)
	if service.serviceEpochAt != ackedAt {
		t.Fatalf("late dispatch lost the successful drain: epoch=%s want=%s", service.serviceEpochAt, ackedAt)
	}
}

// Canceling the first waiting producer transfers the same service pause to
// its FIFO successor. The tail's later ACK still proves a controlled gap;
// cancellation of a writer that never started cannot discard that evidence.
func TestWindowPacingCanceledHeadTransfersControlledDrain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service := &windowPacingService{}
		tailSequence, tail := NewId(), NewId()
		service.observeRoundTrip(time.Millisecond, 0, time.Now())
		service.beginWrite(tailSequence, tail, 1, time.Now(), false)
		service.finishWrite(tailSequence, tail, true)
		service.observeRoundTrip(100*time.Millisecond, 0, time.Now())
		time.Sleep(20 * time.Millisecond)
		headCtx, cancelHead := context.WithCancel(context.Background())
		defer cancelHead()
		head := &windowBurstPacer{service: service, serviceSequenceId: NewId(), rate: 1000000}
		next := &windowBurstPacer{service: service, serviceSequenceId: NewId(), rate: 1000000}
		defer head.close()
		defer next.close()
		headDone, nextDone := make(chan error, 1), make(chan error, 1)
		go func() {
			headDone <- head.waitForServiceMessage(headCtx, 1000, false, head.serviceSequenceId, NewId(), 1)
		}()
		synctest.Wait()
		probe := NewId()
		go func() {
			nextDone <- next.waitForServiceMessage(context.Background(), 1000, false, next.serviceSequenceId, probe, 1)
		}()
		synctest.Wait()
		service.stateLock.Lock()
		armed := !service.drainUntil.IsZero() && service.waiterHead == &head.waiter && service.waiterTail == &next.waiter
		service.stateLock.Unlock()
		if !armed {
			t.Fatal("both producers did not enter the shared controlled pause")
		}
		cancelHead()
		if err := <-headDone; err != context.Canceled {
			t.Fatalf("head cancellation returned %v", err)
		}
		synctest.Wait()
		service.acknowledgeWrite(tailSequence, tail, 1, false, 0, time.Now())
		if err := <-nextDone; err != nil {
			t.Fatal(err)
		}
		service.finishWrite(next.serviceSequenceId, probe, true)
		time.Sleep(100 * time.Millisecond)
		ackedAt := time.Now()
		service.acknowledgeWrite(next.serviceSequenceId, probe, 1, false, 0, ackedAt)
		if service.serviceEpochAt != ackedAt {
			t.Fatalf("head cancellation discarded its successor's controlled gap: epoch=%s want=%s", service.serviceEpochAt, ackedAt)
		}
	})
}

// Sending a probe is not a sampling barrier. A lost or unconfirmed reply
// cannot discard new service evidence while waiting to validate a drained RTT.
func TestWindowPacingMissingProbeKeepsFreshServiceEvidence(t *testing.T) {
	for _, h1 := range []bool{false, true} {
		start := time.Unix(1700000000, 0)
		service := &windowPacingService{}
		sequenceId, tail, probe := NewId(), NewId(), NewId()
		service.observeRoundTrip(100*time.Millisecond, 10*time.Millisecond, start)
		service.beginWrite(sequenceId, tail, 1, start, false)
		service.finishWrite(sequenceId, tail, true)
		service.observe(125000, start)
		service.observe(125000, start.Add(10*time.Millisecond))
		if rate, _, _ := service.measured(time.Second, start.Add(10*time.Millisecond)); rate != 12500000 {
			t.Fatalf("h1=%t: missing initial service: %d", h1, rate)
		}
		service.acknowledgeWrite(sequenceId, tail, 1, false, 10*time.Millisecond, start.Add(100*time.Millisecond))
		service.beginWrite(sequenceId, probe, 2, start.Add(100*time.Millisecond), false)
		service.finishWrite(sequenceId, probe, h1)
		for i := range 30 {
			service.observe(12500, start.Add(time.Duration(20+i)*10*time.Millisecond))
		}
		if rate, _, _ := service.measured(time.Second, start.Add(500*time.Millisecond)); rate != 1250000 {
			t.Errorf("h1=%t: a missing probe prevented measuring slower service: %d", h1, rate)
		}
	}
}

// Convert and multiply only bounded residence when forming drain deadlines.
// A valid long duration can round beyond int64 when represented as float64.
func TestWindowPacingDrainDurationCannotOverflow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service := &windowPacingService{minRoundTrip: time.Millisecond}
		sequenceId, tail := NewId(), NewId()
		service.beginWrite(sequenceId, tail, 1, time.Now(), false)
		service.finishWrite(sequenceId, tail, true)
		service.roundTripStats.ring = newWindowBucketStats(deliverySizedWindowSampleInterval, 4)
		service.roundTripStats.add(1, float64(math.MaxInt64), time.Now().Add(-deliverySizedWindowSampleInterval))
		pacer := &windowBurstPacer{service: service, serviceSequenceId: sequenceId, rate: 1000000}
		defer pacer.close()
		start := time.Now()
		if err := pacer.waitForService(context.Background(), 1000); err != nil {
			t.Fatal(err)
		}
		if time.Since(start) != windowPacingDrainMaximumTime {
			t.Fatalf("long residence wrapped its drain deadline: elapsed=%s", time.Since(start))
		}
	})
}

// A continuously occupied window never ACKs its current tail while sending.
// Recent burst residence must trigger a bounded pause so a changed path can
// produce a genuinely drained probe instead of compounding false congestion.
func TestWindowPacingContinuousFlightRefreshesChangedRoundTrip(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service := &windowPacingService{}
		sequenceId, tail := NewId(), NewId()
		start := time.Now()
		service.observeRoundTrip(time.Millisecond, 0, start)
		service.beginWrite(sequenceId, tail, 1, start, false)
		service.finishWrite(sequenceId, tail, true)
		for range 8 {
			time.Sleep(10 * time.Millisecond)
			service.observeRoundTrip(100*time.Millisecond, 0, time.Now())
		}
		before := time.Now()
		go func() {
			time.Sleep(20 * time.Millisecond)
			service.acknowledgeWrite(sequenceId, tail, 1, false, 0, time.Now())
		}()
		pacer := &windowBurstPacer{service: service, serviceSequenceId: sequenceId, rate: 1000000}
		defer pacer.close()
		if err := pacer.waitForService(context.Background(), 1000); err != nil {
			t.Fatal(err)
		}
		probe := NewId()
		service.beginWrite(sequenceId, probe, 2, time.Now(), false)
		service.finishWrite(sequenceId, probe, true)
		time.Sleep(100 * time.Millisecond)
		service.acknowledgeWrite(sequenceId, probe, 2, false, 0, time.Now())
		if got := service.roundTrip(); got != 100*time.Millisecond {
			t.Fatalf("continuous flight kept the stale propagation floor: got=%s want=100ms", got)
		}
		if elapsed := time.Since(before); elapsed != 120*time.Millisecond {
			t.Fatalf("tail ACK did not promptly release the bounded drain: elapsed=%s want=120ms", elapsed)
		}
	})
}

// A missing, selective or changed-carrier tail cannot certify an empty relay.
// The measurement pause expires and remains cancelable without raising RTT.
func TestWindowPacingDrainNeedsDeliveryAndHasDeadline(t *testing.T) {
	for _, outcome := range []string{"missing", "selective", "carrier-change", "cancel"} {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			service := &windowPacingService{}
			sequenceId, tail := NewId(), NewId()
			service.observeRoundTrip(time.Millisecond, 0, time.Now())
			service.beginWrite(sequenceId, tail, 1, time.Now(), false)
			service.finishWrite(sequenceId, tail, true)
			for range 8 {
				time.Sleep(10 * time.Millisecond)
				service.observeRoundTrip(100*time.Millisecond, 0, time.Now())
			}
			go func() {
				time.Sleep(20 * time.Millisecond)
				switch outcome {
				case "selective":
					service.acknowledgeWrite(sequenceId, tail, 1, true, 0, time.Now())
				case "carrier-change":
					service.finishWrite(sequenceId, tail, false)
					service.acknowledgeWrite(sequenceId, tail, 1, false, 0, time.Now())
				case "cancel":
					cancel()
				}
			}()
			pacer := &windowBurstPacer{service: service, serviceSequenceId: sequenceId, rate: 1000000}
			defer pacer.close()
			start := time.Now()
			err := pacer.waitForService(ctx, 1000)
			if outcome == "cancel" {
				if err != context.Canceled || time.Since(start) != 20*time.Millisecond {
					t.Fatalf("drain cancellation: err=%v elapsed=%s", err, time.Since(start))
				}
			} else {
				wait := 200 * time.Millisecond
				if outcome == "carrier-change" {
					// Lost physical proof abandons the measurement; it does not
					// certify an empty relay or grant a new RTT floor below.
					wait = 20 * time.Millisecond
				}
				if err != nil || time.Since(start) != wait {
					t.Fatalf("%s tail drain: err=%v elapsed=%s, want=%s", outcome, err, time.Since(start), wait)
				}
				probe := NewId()
				service.beginWrite(sequenceId, probe, 2, time.Now(), false)
				service.finishWrite(sequenceId, probe, true)
				time.Sleep(100 * time.Millisecond)
				service.acknowledgeWrite(sequenceId, probe, 2, false, 0, time.Now())
			}
			if got := service.roundTrip(); got != time.Millisecond {
				t.Fatalf("%s tail refreshed the propagation floor to %s", outcome, got)
			}
		})
	}
}

// Accepts a physical retry on a carrier whose policy bypasses H1 pacing.
type windowPacingChangedCarrierWriter struct {
	windowPacingPolicyWriter
}

func (self *windowPacingChangedCarrierWriter) WriteDetailedWithTransport(_ context.Context, bytes []byte, _ time.Duration) (bool, TransportType, error) {
	MessagePoolReturn(bytes)
	return true, TransportTypeH3, nil
}

// A retry can leave H1 and bypass the pacer's begin-write hook entirely. The
// actual write boundary must invalidate the original probe before that copy
// can return an ambiguous ACK, including through the real coalescer.
func TestWindowPacingCarrierChangeInvalidatesProbeBeforeUnpacedRetry(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		sequence := newEstimatorFixture(t, nil)
		sequence.ctx, sequence.client, sequence.log = context.Background(), &Client{}, NewNoopLogger()
		sequence.sequenceId = NewId()
		sequence.contractMultiRouteWriter = &windowPacingChangedCarrierWriter{}
		sequence.ackWindow = newSequenceAckWindow()
		service := &windowPacingService{}
		sequence.windowPacer = windowBurstPacer{service: service, serviceSequenceId: sequence.sequenceId}
		defer sequence.windowPacer.close()
		service.observeRoundTrip(time.Millisecond, 0, time.Now())
		first := NewId()
		service.beginWrite(sequence.sequenceId, first, 1, time.Now(), false)
		service.finishWrite(sequence.sequenceId, first, true)
		time.Sleep(time.Millisecond)
		service.acknowledgeWrite(sequence.sequenceId, first, 1, false, 0, time.Now())
		item := &sendItem{transferItem: transferItem{messageId: NewId(), sequenceNumber: 2},
			expectsAck: true, sendCount: 2, pacingByteCount: 64, transferFrameBytes: MessagePoolGet(64), carrierChanged: true}
		sequence.resendQueue.Add(item)
		defer func() {
			for _, item := range sequence.resendQueue.Clear() {
				item.messagePoolReturn()
			}
		}()
		service.beginWrite(sequence.sequenceId, item.messageId, 2, time.Now(), false)
		service.finishWrite(sequence.sequenceId, item.messageId, true)
		time.Sleep(100 * time.Millisecond)
		if _, err := sequence.writeMaybeWrappedBytes(item.transferFrameBytes, TransferPath{}, true, item, true, false); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
		sequence.coalesceReceivedAck(sequence.ackWindow, receiveAckMessage{messageId: item.messageId, receivedAtNanos: time.Now().UnixNano()})
		if got := service.roundTrip(); got != time.Millisecond {
			t.Fatalf("an unpaced changed-carrier retry refreshed the H1 RTT probe to %s", got)
		}
	})
}

// The first cumulative ACK covering the probe can name a later head. Its
// arrival, rather than eventual send-loop application, measures residence.
func TestWindowPacingDrainedProbeSurvivesHeadCompression(t *testing.T) {
	for _, ackBeforeWriteReturn := range []bool{false, true} {
		service := &windowPacingService{}
		sequenceId, first, probe, head := NewId(), NewId(), NewId(), NewId()
		start := time.Unix(1700000000, 0)
		service.observeRoundTrip(time.Millisecond, 10*time.Millisecond, start)
		service.beginWrite(sequenceId, first, 1, start, false)
		service.finishWrite(sequenceId, first, true)
		service.acknowledgeWrite(sequenceId, first, 1, false, 10*time.Millisecond, start.Add(10*time.Millisecond))
		service.beginWrite(sequenceId, probe, 2, start.Add(20*time.Millisecond), false)
		if !ackBeforeWriteReturn {
			service.finishWrite(sequenceId, probe, true)
		}
		service.beginWrite(sequenceId, head, 3, start.Add(21*time.Millisecond), false)
		service.finishWrite(sequenceId, head, true)
		service.acknowledgeWrite(sequenceId, head, 3, false, 10*time.Millisecond, start.Add(120*time.Millisecond))
		if ackBeforeWriteReturn {
			if got := service.roundTrip(); got != time.Millisecond {
				t.Fatalf("an unconfirmed physical carrier raised the RTT baseline: %s", got)
			}
			service.finishWrite(sequenceId, probe, true)
		}
		if got := service.roundTrip(); got != 100*time.Millisecond {
			t.Fatalf("ack-before-write=%t: compressed head lost the drained probe's residence: %s", ackBeforeWriteReturn, got)
		}
		service.acknowledgeWrite(sequenceId, head, 3, false, 10*time.Millisecond, start.Add(140*time.Millisecond))
		service.observeRoundTrip(500*time.Millisecond, 10*time.Millisecond, start.Add(150*time.Millisecond))
		if got := service.roundTrip(); got != 100*time.Millisecond {
			t.Fatalf("a duplicate or queued observation replaced the completed probe: %s", got)
		}
	}
}

// A shared service is drained only after every logical sequence's tail has
// a cumulative ACK. A SACKed newest tail does not cover its earlier holes.
func TestWindowPacingDrainedProbeRequiresEverySibling(t *testing.T) {
	service := &windowPacingService{}
	start := time.Unix(1700000000, 0)
	firstSequence, secondSequence := NewId(), NewId()
	first, second, third, fourth := NewId(), NewId(), NewId(), NewId()
	service.beginWrite(firstSequence, first, 1, start, false)
	service.finishWrite(firstSequence, first, true)
	service.beginWrite(secondSequence, second, 1, start, false)
	service.finishWrite(secondSequence, second, true)
	service.acknowledgeWrite(secondSequence, second, 1, false, 0, start.Add(time.Millisecond))
	service.beginWrite(secondSequence, third, 2, start.Add(2*time.Millisecond), false)
	service.finishWrite(secondSequence, third, true)
	if !service.roundTripProbe.sentAt.IsZero() {
		t.Fatal("one sibling's ACK treated another sibling's flight as drained")
	}
	service.acknowledgeWrite(firstSequence, first, 1, true, 0, start.Add(3*time.Millisecond))
	service.acknowledgeWrite(secondSequence, third, 2, false, 0, start.Add(3*time.Millisecond))
	if service.pendingWrites != 1 {
		t.Fatalf("a SACK released an entire sequence's outstanding flight: %d", service.pendingWrites)
	}
	service.acknowledgeWrite(firstSequence, first, 1, false, 0, start.Add(4*time.Millisecond))
	service.beginWrite(secondSequence, fourth, 3, start.Add(5*time.Millisecond), false)
	if service.roundTripProbe.messageId != fourth || service.pendingWrites != 1 {
		t.Fatal("cumulative delivery of all siblings did not permit exactly one probe")
	}
	// A closed producer cannot strand a phantom tail or a probe in the shared service.
	pacer := &windowBurstPacer{service: service, serviceSequenceId: secondSequence}
	pacer.close()
	pacer.close()
	if service.pendingWrites != 0 || len(service.writes) != 1 || !service.roundTripProbe.sentAt.IsZero() {
		t.Fatal("repeated close leaked or double-released a producer's probe state")
	}
}

// Releasing a canceled sequence's bookkeeping does not deliver its physical
// bytes. An older sibling ACK cannot turn that release into an empty relay.
func TestWindowPacingCanceledTailNeedsFreshDelivery(t *testing.T) {
	service := &windowPacingService{}
	start := time.Unix(1700000000, 0)
	firstSequence, secondSequence := NewId(), NewId()
	first, canceled, fresh, probe := NewId(), NewId(), NewId(), NewId()
	service.beginWrite(firstSequence, first, 1, start, false)
	service.finishWrite(firstSequence, first, true)
	service.beginWrite(secondSequence, canceled, 1, start.Add(time.Millisecond), false)
	service.finishWrite(secondSequence, canceled, true)
	(&windowBurstPacer{service: service, serviceSequenceId: secondSequence}).close()
	service.acknowledgeWrite(firstSequence, first, 1, false, 0, start.Add(2*time.Millisecond))
	service.beginWrite(firstSequence, fresh, 2, start.Add(3*time.Millisecond), false)
	service.finishWrite(firstSequence, fresh, true)
	if !service.roundTripProbe.sentAt.IsZero() {
		t.Fatal("canceling an unacknowledged tail manufactured a drained-burst RTT probe")
	}
	service.acknowledgeWrite(firstSequence, fresh, 2, false, 0, start.Add(4*time.Millisecond))
	service.beginWrite(firstSequence, probe, 3, start.Add(5*time.Millisecond), false)
	if service.roundTripProbe.messageId != probe {
		t.Fatal("fresh cumulative delivery could not restore probing after cancellation")
	}
}

// Failed writes, a different actual carrier, retransmitted probes, unrelated
// sequences and SACKs above a probe cannot establish a larger propagation floor.
func TestWindowPacingDrainedProbeRejectsAmbiguousEvidence(t *testing.T) {
	for _, reason := range []string{"carrier-change", "probe-retransmission", "other-sequence", "sack-above-probe", "old-head", "old-time"} {
		service := &windowPacingService{}
		sequenceId, first, probe, next := NewId(), NewId(), NewId(), NewId()
		start := time.Unix(1700000000, 0)
		service.observeRoundTrip(time.Millisecond, 0, start)
		service.beginWrite(sequenceId, first, 1, start, false)
		service.finishWrite(sequenceId, first, true)
		service.acknowledgeWrite(sequenceId, first, 1, false, 0, start.Add(time.Millisecond))
		service.beginWrite(sequenceId, probe, 2, start.Add(2*time.Millisecond), false)
		service.finishWrite(sequenceId, probe, reason != "carrier-change")
		ackSequence, ackMessage, number, selective, at := sequenceId, probe, uint64(2), false, start.Add(102*time.Millisecond)
		switch reason {
		case "probe-retransmission":
			service.beginWrite(sequenceId, probe, 2, start.Add(3*time.Millisecond), true)
			service.finishWrite(sequenceId, probe, true)
		case "other-sequence":
			ackSequence = NewId()
		case "sack-above-probe":
			ackMessage, number, selective = next, 3, true
		case "old-head":
			ackMessage, number = first, 1
		case "old-time":
			at = start
		}
		service.acknowledgeWrite(ackSequence, ackMessage, number, selective, 0, at)
		if got := service.roundTrip(); got != time.Millisecond {
			t.Fatalf("%s raised the propagation baseline to %s", reason, got)
		}
	}
}

// A stale sequence minimum cannot suppress growth earned after the shared
// service discovers a longer path. A timing observation alone is not growth.
func TestWindowPacingWindowUsesRefreshedServiceResidence(t *testing.T) {
	start := time.Unix(1700000000, 0)
	sequence := newEstimatorFixture(t, func(settings *SendBufferSettings) {
		settings.DeliverySizedWindowScale = 2
		settings.ResendQueueBudget = NewTransferMemoryBudget(mib(48))
		settings.TargetGoodputByteRate = 125000000
	})
	t.Cleanup(func() { sequence.resendQueue.Clear() })
	sequence.rttWindow.closeSendTime(uint64(start.Add(-time.Millisecond).UnixMilli()), start)
	sequence.observeReceiveWindowAdvertisement(receiveAckMessage{receiveWindowSet: true, receiveWindowByteCount: uint32(mib(48)), ackCompressTimeoutSet: true, ackCompressTimeoutMicros: 10000})
	sequence.receiveWindowSetAtNanos.Store(start.UnixNano())
	sequence.windowPacer.service = &windowPacingService{minRoundTrip: 100 * time.Millisecond}
	estimate := sequence.sendWindowEstimate(start)
	if estimate.RoundTrip != 100*time.Millisecond || estimate.WindowRoundTrip != 110*time.Millisecond || estimate.Window != estimate.Initial {
		t.Fatalf("timing observation changed the cold window or lost the service residence: %+v", estimate)
	}
	at := start
	for range 40 {
		at = at.Add(10 * time.Millisecond)
		sequence.observeDeliveredBytes(1024*1024, at)
	}
	estimate = sequence.sendWindowEstimate(at)
	if !estimate.Sized || estimate.RoundTrip != 100*time.Millisecond || estimate.WindowRoundTrip != 110*time.Millisecond || estimate.Window < 12500000 {
		t.Fatalf("stale sequence timing suppressed qualified growth on the longer path: %+v", estimate)
	}
}

// Contract repair is not delivery. A real cumulative reply then establishes
// the probe while the sender has not applied either coalesced head yet.
func TestWindowPacingCoalescerPreservesDrainedProbeArrival(t *testing.T) {
	settings := DefaultSendBufferSettings()
	settings.DeliverySizedWindowScale = 2
	service := &windowPacingService{}
	sequence := &SendSequence{client: &Client{}, log: NewNoopLogger(), sequenceId: NewId(),
		sendBufferSettings: settings, resendQueue: newResendQueue(nil, 0), windowPacer: windowBurstPacer{service: service}}
	window := newSequenceAckWindow()
	start := time.Unix(1700000000, 0)
	service.observeRoundTrip(time.Millisecond, 0, start)
	first := &sendItem{transferItem: transferItem{messageId: NewId(), sequenceNumber: 1}}
	sequence.resendQueue.Add(first)
	service.beginWrite(sequence.sequenceId, first.messageId, 1, start, false)
	service.finishWrite(sequence.sequenceId, first.messageId, true)
	ack := receiveAckMessage{messageId: first.messageId, receivedAtNanos: start.Add(time.Millisecond).UnixNano(), contractMissing: true}
	sequence.coalesceReceivedAck(window, ack)
	if service.pendingWrites != 1 {
		t.Fatal("a missing-contract reply was mistaken for delivery")
	}
	ack.contractMissing = false
	sequence.coalesceReceivedAck(window, ack)
	probe := &sendItem{transferItem: transferItem{messageId: NewId(), sequenceNumber: 2}}
	sequence.resendQueue.Add(probe)
	service.beginWrite(sequence.sequenceId, probe.messageId, 2, start.Add(2*time.Millisecond), false)
	service.finishWrite(sequence.sequenceId, probe.messageId, true)
	sequence.coalesceReceivedAck(window, receiveAckMessage{messageId: probe.messageId, receivedAtNanos: start.Add(102 * time.Millisecond).UnixNano()})
	sequence.coalesceReceivedAck(window, receiveAckMessage{messageId: probe.messageId, receivedAtNanos: start.Add(122 * time.Millisecond).UnixNano()})
	if service.roundTrip() != 100*time.Millisecond || sequence.resendQueue.Len() != 2 {
		t.Fatal("delayed head application or a duplicate ACK changed the measured probe")
	}
}
