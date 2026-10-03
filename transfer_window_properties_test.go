package connect

import (
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

// The occupancy band applies after measured delivery grows the opening.
// A wall-clock producer did not establish that premise: under instrumentation
// it could leave the retained 2 MiB opening mostly empty. Controlled offer and
// Ack clocks establish growth here without changing the original band.
func TestWindowRetainedSteadyPathPreservesOccupancyBand(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		estimate, ratio := measureRetainedWindowOccupancy(t, 64*1024)
		if !estimate.Sized || estimate.CandidateWindow <= estimate.Floor ||
			estimate.Ceiling <= estimate.CandidateWindow || estimate.Ceiling <= estimate.Window {
			t.Fatalf("the steady-path candidate or retained window reached a hard clamp: %+v", estimate)
		}
		if estimate.CandidateWindow <= estimate.Initial || estimate.Window <= estimate.Initial {
			t.Fatalf("steady delivery did not establish growth above the opening: %+v", estimate)
		}
		if ratio < 0.45 || 0.85 < ratio {
			t.Errorf("occupancy rests at %.2f of the window, outside the 0.45 to 0.85 band", ratio)
		}
	})
}

// The same path at a quarter of the offer rate reproduces the former 0.20
// occupancy failure deterministically. Its candidate remains interior, but
// ordinary slow delivery cannot shrink the opening to manufacture half fill.
func TestWindowRetainedApplicationLimitedPathKeepsOpening(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		estimate, ratio := measureRetainedWindowOccupancy(t, 16*1024)
		if !estimate.Sized || estimate.CandidateWindow <= estimate.Floor || estimate.Initial <= estimate.CandidateWindow || estimate.Window != estimate.Initial || estimate.LearnedWindow != estimate.Initial {
			t.Fatalf("application-limited delivery changed retained capacity or failed to qualify an interior candidate: %+v", estimate)
		}
		if 0.45 <= ratio {
			t.Fatalf("application-limited control did not leave the old occupancy band: %.3f", ratio)
		}
	})
}

// One real Pack per virtual millisecond fixes the offered rate independently
// of host execution speed. FIFO Ack propagation and quiescence pair actual
// retained bytes with the admitted window after every completed transition.
func measureRetainedWindowOccupancy(t *testing.T, payloadByteCount int) (SendWindowEstimate, float64) {
	t.Helper()
	const propagation = 25 * time.Millisecond
	const spacing = time.Millisecond
	const ticks = 512
	fixture := newWindowRoundFixture(t, func(settings *SendBufferSettings) {
		settings.DeliverySizedWindowScale = deliverySizedWindowScale
		settings.ResendQueueMaxByteCount = mib(2)
		settings.ResendQueueMinByteCount = kib(256)
		settings.ResendQueueBudget = NewTransferMemoryBudget(mib(64))
		settings.DeliverySizedWindowCeilingByteCount = mib(64)
		settings.TargetGoodputByteRate = 0
	}, func(settings *ReceiveBufferSettings) {
		settings.ReceiveQueueMaxByteCount = mib(64)
		settings.AdvertiseReceiveWindow = true
	})
	// Establish the receiver's permission before sustained traffic; the
	// initial blind hold is a separate bound from the learned opening.
	warmup := fixture.write(payloadByteCount)
	ack := fixture.receive(warmup)
	time.Sleep(propagation)
	fixture.forward(ack, fixture.senderIn)
	fixture.startWire(propagation, 0)
	sequence := fixture.sequence()
	var estimate SendWindowEstimate
	var ratioTotal float64
	for tick := range ticks {
		at := time.Now()
		if admitted, err := fixture.send(payloadByteCount); !admitted || err != nil {
			t.Fatalf("steady Pack %d was not admitted: %v", tick, err)
		}
		synctest.Wait()
		if !time.Now().Equal(at) {
			t.Fatalf("steady Pack %d waited for admission and changed the offered rate", tick)
		}
		if ticks/2 <= tick {
			estimate = sequence.sendWindowSnapshot(at)
			count, queued := sequence.resendQueue.QueueSize()
			if count != int(propagation/spacing) || !estimate.Sized || estimate.RoundTrip != propagation {
				t.Fatalf("tick %d: retained=%d estimate=%+v, want one sampled propagation flight", tick, count, estimate)
			}
			ratioTotal += float64(queued) / float64(estimate.Window)
		}
		time.Sleep(spacing)
	}
	// All offered messages must finish without eviction or a stranded owner.
	time.Sleep(propagation)
	synctest.Wait()
	if count, _ := sequence.resendQueue.QueueSize(); count != 0 || fixture.deliveredCount != ticks+1 || fixture.ackedCount != fixture.deliveredCount {
		t.Fatalf("steady path did not drain: retained=%d delivered=%d acknowledged=%d, want %d messages", count, fixture.deliveredCount, fixture.ackedCount, ticks+1)
	}
	if stats := fixture.receiver.ReceiveStats(); stats.ReceiveQueueEvictionCount != 0 || stats.ReceiveQueueDropCount != 0 {
		t.Fatalf("steady path caused evictions=%d drops=%d", stats.ReceiveQueueEvictionCount, stats.ReceiveQueueDropCount)
	}
	ratio := ratioTotal / (ticks / 2)
	t.Logf("payload=%d per %s: mean paired occupancy=%.3f; window=%d candidate=%d initial=%d ceiling=%d reason=%q", payloadByteCount, spacing, ratio, estimate.Window, estimate.CandidateWindow, estimate.Initial, estimate.Ceiling, estimate.Reason)
	return estimate, ratio
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
