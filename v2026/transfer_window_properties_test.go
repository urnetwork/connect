package connect

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// Builds a sequence whose estimator can be driven by injected values rather
// than by racing a real transfer. Deterministic by construction: no timers, no
// goroutines, no dependence on how fast the machine is.
func newEstimatorFixture(
	t *testing.T,
	configure func(*SendBufferSettings),
) *SendSequence {
	t.Helper()
	settings := DefaultSendBufferSettings()
	if configure != nil {
		configure(settings)
	}
	return &SendSequence{
		sendBufferSettings: settings,
		resendQueue: newResendQueue(
			settings.ResendQueueBudget,
			settings.ResendQueueMinByteCount,
		),
		deliveredBytes: make([]deliveredBytesSample, deliveredBytesRingSize),
		rttWindow: NewRttWindow(
			NewNoopLogger(),
			settings.RttWindowSize,
			settings.RttWindowTimeout,
			settings.RttScale,
			settings.MinResendInterval,
			settings.RttMinResendInterval,
			settings.MaxResendInterval,
		),
	}
}

// Closes enough round trips against the wall clock the window reads that the
// estimate is sampled at about the given value.
func sampleRoundTrip(sequence *SendSequence, roundTrip time.Duration) {
	for range 8 {
		sequence.rttWindow.CloseSendTime(
			uint64(time.Now().Add(-roundTrip).UnixMilli()))
	}
}

// An advertisement replaces blind permission immediately. It supplies no
// delivery evidence, so learned capacity remains the configured opening.
func TestAdvertisementRaisesPermissionWithoutLearningCapacity(t *testing.T) {
	const advertised = ByteCount(4 * 1024 * 1024)
	sequence := newEstimatorFixture(t, func(settings *SendBufferSettings) {
		settings.DeliverySizedWindowScale = deliverySizedWindowScale
		settings.ResendQueueBudget = NewTransferMemoryBudget(mib(64))
	})

	now := time.Unix(1700000000, 0)
	blind := sequence.sendWindowEstimate(now)
	if blind.Window != defaultInitialWindowByteCount() || blind.Ceiling != blind.Window || blind.LearnedWindow != blind.Initial {
		t.Errorf(
			"a blind sender's window is %d rather than the %d byte receive hold floor it may assume of any peer",
			blind.Window, defaultInitialWindowByteCount(),
		)
	}

	// the first acknowledgement carries a capacity
	sequence.observeReceiveWindowAdvertisement(receiveAckMessage{
		receiveWindowSet:       true,
		receiveWindowByteCount: uint32(advertised),
	})
	stepped := sequence.sendWindowEstimate(now)
	t.Logf(
		"blind %d, then advertised %d gives window %d ceiling %d reason %q",
		blind.Window, advertised, stepped.Window, stepped.Ceiling, stepped.Reason,
	)
	if stepped.Window != stepped.Initial || stepped.LearnedWindow != blind.LearnedWindow || stepped.Ceiling != advertised || stepped.Sized {
		t.Errorf(
			"advertisement changed learned capacity without delivery evidence: blind=%+v advertised=%d after=%+v",
			blind, advertised, stepped,
		)
	}
}

// Delivery before a permission increase cannot qualify cumulative sizing.
// Independently measured service and a fresh cumulative interval can supply
// smaller candidates without reducing the learned window.
func TestDeliveryCandidatesRequireFreshPermissionHistory(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const advertised = ByteCount(4 * 1024 * 1024)
		const roundTrip = 50 * time.Millisecond
		sequence := newEstimatorFixture(t, func(settings *SendBufferSettings) {
			settings.DeliverySizedWindowScale = deliverySizedWindowScale
			settings.ResendQueueBudget = NewTransferMemoryBudget(mib(64))
		})
		sampleRoundTrip(sequence, roundTrip)

		// delivery from before the step: a small blind round trip's worth
		now := time.Now()
		for i := range 40 {
			sequence.observeDeliveredBytes(
				ByteCount(8*1024), now.Add(-400*time.Millisecond+time.Duration(i)*10*time.Millisecond))
		}
		sequence.observeReceiveWindowAdvertisement(receiveAckMessage{
			receiveWindowSet:       true,
			receiveWindowByteCount: uint32(advertised),
		})

		lagged := sequence.sendWindowEstimate(time.Now())
		t.Logf("with delivery from before the step: window %d, reason %q", lagged.Window, lagged.Reason)
		if lagged.Sized || lagged.Window != lagged.Initial || lagged.LearnedWindow != lagged.Initial || lagged.Ceiling != advertised {
			t.Errorf(
				"delivery predating permission qualified or changed learned capacity: %+v",
				lagged,
			)
		}
		if !lagged.ServiceSized || lagged.Interval < 2*lagged.WindowRoundTrip || lagged.DeliveredByteCount == 0 || lagged.CandidateWindow != lagged.Floor {
			t.Errorf(
				"old cumulative history did not remain separate from the valid service candidate: %+v",
				lagged,
			)
		}

		// Fresh slow delivery qualifies a floor candidate without reducing capacity.
		for range 40 {
			sequence.observeDeliveredBytes(ByteCount(8*1024), time.Now())
			time.Sleep(10 * time.Millisecond)
		}
		fresh := sequence.sendWindowEstimate(time.Now())
		t.Logf("with delivery from after the step: window %d candidate %d reason %q", fresh.Window, fresh.CandidateWindow, fresh.Reason)
		if !fresh.Sized || fresh.CandidateWindow != fresh.Floor || fresh.Window != lagged.Window || fresh.LearnedWindow != lagged.LearnedWindow || fresh.Ceiling != advertised {
			t.Errorf(
				"fresh delivery failed to qualify its candidate or reduced learned capacity: %+v",
				fresh,
			)
		}
	})
}

// Keep the established 0.45–0.85 occupancy band on the same steady path,
// offer duration and paired sampling clock. Retention can preserve a larger
// earlier candidate, so the final reason need not be "delivery". The fresh
// candidate and effective window must still lie inside their hard bounds.
// Form each ratio from contemporaneous occupancy and effective window; dividing
// average occupancy by a single final window mixes different populations.
func TestWindowRetainedSteadyPathPreservesOccupancyBand(t *testing.T) {
	assertMessagePoolOwnership(t)

	const propagation = 25 * time.Millisecond
	const payloadByteCount = 4 * 1024

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	harness := newSendWindowHarness(t, ctx, propagation,
		func(settings *SendBufferSettings) {
			settings.DeliverySizedWindowScale = deliverySizedWindowScale
			// far above twice the delivery, so the clamp never binds and the
			// delivery term is what sets the window
			settings.ResendQueueBudget = NewTransferMemoryBudget(mib(64))
			settings.DeliverySizedWindowCeilingByteCount = mib(64)
			// the target clamp is a separate mechanism with its own rows, and
			// it ships on; this row measures the fixed point, so the clamp is
			// held out
			settings.TargetGoodputByteRate = 0
		})
	harness.receiveHold(mib(64))
	// This identity is for immediate ACKs. A compressed receiver reserves
	// additional residence, so occupancy need not be half that larger window.
	harness.receiver.settings.ReceiveBufferSettings.AckCompressTimeout = 0

	// paired at each tick: occupancy and the window as they stand together
	ratioTotal := &atomic.Int64{}
	ratioSamples := &atomic.Int64{}
	watching := make(chan struct{})
	go func() {
		defer close(watching)
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Millisecond):
				_, queued, _ := harness.sender.ResendQueueSize(
					harness.receiverId, MultiHopId{}, false, false)
				window := harness.sender.
					DestinationSendStats(harness.receiverId).SendWindow
				if window.Window <= 0 || !window.Sized {
					continue
				}
				// in parts per thousand, so the average is integer arithmetic
				ratioTotal.Add(int64(queued) * 1000 / int64(window.Window))
				ratioSamples.Add(1)
			}
		}
	}()
	harness.offer(t, payloadByteCount, 3*time.Second)
	estimate := harness.sender.DestinationSendStats(harness.receiverId).SendWindow
	cancel()
	<-watching

	if ratioSamples.Load() == 0 {
		t.Fatal("occupancy was never sampled against a sized window, so this cell reads nothing")
	}
	ratio := float64(ratioTotal.Load()) / float64(ratioSamples.Load()) / 1000
	t.Logf(
		"mean of occupancy over window, paired at %d ticks: %.2f; last window %d, ceiling %d, reason %q",
		ratioSamples.Load(), ratio, estimate.Window,
		estimate.Ceiling, estimate.Reason,
	)

	if !estimate.Sized || estimate.CandidateWindow <= estimate.Floor ||
		estimate.Ceiling <= estimate.CandidateWindow || estimate.Ceiling <= estimate.Window {
		t.Fatalf(
			"the steady-path candidate or retained window reached a hard clamp, so this cell does not isolate delivery: %+v",
			estimate,
		)
	}
	if ratio < 0.45 || 0.85 < ratio {
		t.Errorf(
			"occupancy rests at %.2f of the window, outside the 0.45 to 0.85 band four runs put it in at 0.68 to 0.71; approaching the window means the delivery term has stopped binding, and collapsing toward zero means the window has, and either is a defect in the loop",
			ratio,
		)
	}
}

// Injected delivery checks the candidate arithmetic from an explicit 1 MiB
// opening. A full opening per RTT approximately doubles learned capacity;
// half an opening approximately preserves it. This is not a throughput model.
func TestGrowthDoublesFromAFullWindowAndStallsFromAHalfFilledOne(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const roundTrip = 50 * time.Millisecond
		const window = ByteCount(1024 * 1024)

		grown := func(deliveredPerRoundTrip ByteCount) SendWindowEstimate {
			sequence := newEstimatorFixture(t, func(settings *SendBufferSettings) {
				settings.DeliverySizedWindowScale = deliverySizedWindowScale
				settings.ResendQueueBudget = NewTransferMemoryBudget(mib(64))
				settings.ResendQueueMaxByteCount = window
				settings.ResendQueueMinByteCount = ByteCount(64 * 1024)
			})
			sampleRoundTrip(sequence, roundTrip)
			sequence.observeReceiveWindowAdvertisement(receiveAckMessage{
				ackCompressTimeoutSet:  true, // this fixture delivers without receiver batching
				receiveWindowSet:       true,
				receiveWindowByteCount: uint32(mib(64)),
			})
			// delivery laid down after the step, at the given rate per round trip
			now := time.Now()
			const samples = 40
			const spacing = 10 * time.Millisecond
			perSample := ByteCount(
				int64(deliveredPerRoundTrip) * int64(spacing) / int64(roundTrip))
			for i := range samples {
				sequence.observeDeliveredBytes(perSample, now.Add(time.Duration(i)*spacing))
			}
			return sequence.sendWindowEstimate(now.Add(samples * spacing))
		}

		full := grown(window)
		half := grown(window / 2)
		for _, estimate := range []SendWindowEstimate{full, half} {
			if !estimate.Sized || estimate.Initial != window || estimate.Window != max(window, estimate.CandidateWindow) || estimate.LearnedWindow != estimate.Window {
				t.Errorf("qualified delivery did not grow from the explicit opening: %+v", estimate)
			}
		}
		t.Logf(
			"delivering a full %d window per round trip gives %d (%.2fx); delivering half gives %d (%.2fx)",
			window, full.Window, float64(full.Window)/float64(window),
			half.Window, float64(half.Window)/float64(window),
		)

		if ratio := float64(full.Window) / float64(window); ratio < 1.8 || 2.2 < ratio {
			t.Errorf(
				"a full window delivered per round trip gives a next window of %d, %.2f times it, rather than about twice; growth is the scale times delivery and a full window delivers its whole self",
				full.Window, ratio,
			)
		}
		if ratio := float64(half.Window) / float64(window); ratio < 0.8 || 1.2 < ratio {
			t.Errorf(
				"a half-filled window gives a next window of %d, %.2f times its explicit opening; half-flight delivery should preserve approximately that capacity",
				half.Window, ratio,
			)
		}
		if half.Window >= full.Window {
			t.Errorf(
				"half-flight delivery learned %d against full-flight delivery's %d; full-flight delivery must establish the larger candidate",
				half.Window, full.Window,
			)
		}
	})
}
