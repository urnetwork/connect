package connect

import (
	"context"
	"fmt"
	"math"
	"testing"
	"testing/synctest"
	"time"
)

// Only the immutable route policy is needed by these recovery decisions.
type windowPacingPolicyWriter struct {
	MultiRouteWriter
	policy transferFlightPolicySnapshot
}

func (self *windowPacingPolicyWriter) transferFlightPolicy() transferFlightPolicySnapshot {
	return self.policy
}

// A healthy slow H1 FIFO can drain across more than two retransmit timers.
// Every extra deferral requires new cumulative progress. Silence, a changed
// carrier, a mixed path and an unreliable item retain ordinary recovery.
func TestWindowPacingSlowServiceDeferralNeedsFreshProgress(t *testing.T) {
	settings := DefaultSendBufferSettings()
	settings.DeliverySizedWindowScale = 2
	writer := &windowPacingPolicyWriter{policy: transferFlightPolicySnapshot{h1Only: true}}
	sequence := &SendSequence{sendBufferSettings: settings, contractMultiRouteWriter: writer}
	start := time.Now()
	item := &sendItem{sendTime: start, reliableCarrierObserved: true}
	for i := range 20 {
		sequence.lastCumulativeAckTime = start.Add(time.Duration(i+1) * time.Second)
		if !sequence.shouldDeferTimeoutResend(item, time.Second) {
			t.Fatalf("live FIFO rewrote its initial train at deferral %d", i)
		}
		item.timeoutDeferCount++
		item.timeoutDeferAckTime = sequence.lastCumulativeAckTime
		if sequence.shouldDeferTimeoutResend(item, time.Second) {
			t.Fatalf("silence granted a further deferral at %d", i)
		}
	}
	sequence.lastCumulativeAckTime = sequence.lastCumulativeAckTime.Add(time.Second)
	item.carrierChanged = true
	if sequence.shouldDeferTimeoutResend(item, time.Second) {
		t.Fatal("old carrier borrowed H1's progress")
	}
	item.carrierChanged, item.unreliableCarrierObserved = false, true
	if sequence.shouldDeferTimeoutResend(item, time.Second) {
		t.Fatal("unreliable item borrowed FIFO progress")
	}
	item.unreliableCarrierObserved = false
	writer.policy.h1Only = false
	if sequence.shouldDeferTimeoutResend(item, time.Second) {
		t.Fatal("mixed path exceeded its deferral limit")
	}
	writer.policy.h1Only = true
	settings.DeliverySizedWindowScale = 0
	if sequence.shouldDeferTimeoutResend(item, time.Second) {
		t.Fatal("constant-window recovery changed")
	}
}

// Every prefix must fit the service performed so far plus one ten-millisecond
// burst. Check mixed wire sizes and rates, not only the final average rate.
func TestWindowPacingServiceByteEnvelope(t *testing.T) {
	for _, rate := range []ByteCount{125000, 1250000, 12500000, 125000000, 137500000} {
		for _, sizes := range [][]int{{64}, {1280}, {8192}, {65536}, {64, 1280, 8192, 65536}} {
			t.Logf("case: %s", fmt.Sprintf("rate=%d/sizes=%v", rate, sizes))
			synctest.Test(t, func(t *testing.T) {
				pacer := &windowBurstPacer{}
				defer pacer.close()
				start := time.Now()
				bytes := int64(0)
				for i := range 200 {
					size := sizes[i%len(sizes)]
					if err := pacer.wait(context.Background(), size, rate); err != nil {
						t.Fatal(err)
					}
					bytes += int64(size)
					elapsed := time.Since(start)
					serviceTime := time.Duration(float64(bytes) / float64(rate) * float64(time.Second))
					// A duration truncates by less than a nanosecond per write.
					quantization := time.Duration(i+1) * time.Nanosecond
					if elapsed+10*time.Millisecond+quantization < serviceTime || elapsed > serviceTime+quantization {
						t.Fatalf("prefix=%d bytes=%d service=%s elapsed=%s", i+1, bytes, serviceTime, elapsed)
					}
				}
			})
		}
	}
}

// A rate change charges future bytes at the new rate without forgetting an
// earlier burst's service time. These deadlines are hand-calculated.
func TestWindowPacingServiceChangesPreserveDebt(t *testing.T) {
	for _, test := range []struct {
		name                  string
		firstRate, nextRate   ByteCount
		firstBytes, nextBytes int
		want                  time.Duration
	}{
		{name: "down", firstRate: 1000000, nextRate: 100000, firstBytes: 8000, nextBytes: 1000, want: 8 * time.Millisecond},
		{name: "up", firstRate: 100000, nextRate: 1000000, firstBytes: 1000, nextBytes: 10000, want: 10 * time.Millisecond},
		// Eight kB remain from the old allowance. The new 2 MB/s rate
		// earns the remaining twelve kB in six ms, without free new credit.
		{name: "small-step", firstRate: 1000000, nextRate: 2000000, firstBytes: 2000, nextBytes: 20000, want: 6 * time.Millisecond},
	} {
		t.Logf("case: %s", test.name)
		synctest.Test(t, func(t *testing.T) {
			pacer := &windowBurstPacer{}
			defer pacer.close()
			start := time.Now()
			if err := pacer.wait(context.Background(), test.firstBytes, test.firstRate); err != nil {
				t.Fatal(err)
			}
			if time.Now() != start {
				t.Fatal("first burst unnecessarily slept")
			}
			if err := pacer.wait(context.Background(), test.nextBytes, test.nextRate); err != nil {
				t.Fatal(err)
			}
			if elapsed := time.Since(start); elapsed != test.want {
				t.Fatalf("rate change erased or duplicated service: %s want %s", elapsed, test.want)
			}
		})
	}
}

// Idle credit belongs only to this sequence and stays bounded. Changing its
// measured rate must not start a second opening-window probe.
func TestWindowPacingProbeBoundaryIdleAndIndependentSequences(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pacer := &windowBurstPacer{rate: 1000000, probeRate: 10000000, probeLimit: 15000}
		defer pacer.close()
		start := time.Now()
		if err := pacer.waitForService(context.Background(), 10000); err != nil {
			t.Fatal(err)
		}
		if err := pacer.waitForService(context.Background(), 10000); err != nil {
			t.Fatal(err)
		}
		// 15 kB at 10 MB/s, then 5 kB at 1 MB/s, including the split message.
		// The next full measured burst waits for that 6.5 ms serialization.
		if err := pacer.waitForService(context.Background(), 10000); err != nil {
			t.Fatal(err)
		}
		if time.Since(start) != 6500*time.Microsecond || pacer.service.probeSent != 15000 {
			t.Fatalf("probe boundary: elapsed=%s probe=%d", time.Since(start), pacer.service.probeSent)
		}
		time.Sleep(time.Second)
		pacer.rate = 2000000
		start = time.Now()
		for range 3 {
			if err := pacer.waitForService(context.Background(), 10000); err != nil {
				t.Fatal(err)
			}
		}
		if time.Since(start) != 10*time.Millisecond || pacer.service.probeSent != 15000 {
			t.Fatal("idle or service update replenished the probe")
		}
		other := &windowBurstPacer{rate: 1000000, probeRate: 10000000, probeLimit: 15000}
		defer other.close()
		start = time.Now()
		if err := other.waitForService(context.Background(), 10000); err != nil {
			t.Fatal(err)
		}
		if time.Now() != start || other.service.probeSent != 10000 || pacer.service.probeSent != 15000 {
			t.Fatal("one sequence spent another sequence's probe or service")
		}
	})
}

// Cancellation applies even when a short write fits the current burst. The
// next producer must never be admitted merely because it would not sleep.
func TestWindowPacingCanceledSmallWrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		pacer := &windowBurstPacer{rate: 1000000, probeRate: 10000000, probeLimit: 15000}
		defer pacer.close()
		if err := pacer.waitForService(ctx, 64); err != context.Canceled {
			t.Fatalf("canceled short write admitted: %v", err)
		}
		if pacer.service != nil {
			t.Fatal("canceled write consumed service")
		}
	})
}

// An indivisible write crossing the probe boundary charges each portion at
// its own rate. Cancellation joins the wait and retains that reservation debt.
func TestWindowPacingCancellationDuringProbe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		pacer := &windowBurstPacer{rate: 1000000, probeRate: 10000000, probeLimit: 100000}
		defer pacer.close()
		done := make(chan error, 1)
		go func() { done <- pacer.waitForService(ctx, 200000) }()
		synctest.Wait()
		cancel()
		if err := <-done; err != context.Canceled {
			t.Fatalf("probe cancellation: %v", err)
		}
		if delay := pacer.service.next.Sub(time.Now()); delay != 110*time.Millisecond {
			t.Fatalf("canceled probe lost its reserved serialization: %s", delay)
		}
	})
}

// Compressed ACKs must preserve the serializer's byte/time pairs at every
// tested service. The estimator sees only cumulative delivered bytes.
func TestWindowPacingServiceMeasurementAcrossAckCompression(t *testing.T) {
	for _, rate := range []ByteCount{125000, 1250000, 12500000, 125000000} {
		for _, compression := range []time.Duration{time.Millisecond, 10 * time.Millisecond, 50 * time.Millisecond} {
			t.Logf("case: %s", fmt.Sprintf("rate=%d/compression=%s", rate, compression))
			sequence := &SendSequence{
				sendBufferSettings: &SendBufferSettings{DeliverySizedWindowScale: 2},
				deliveredBytes:     make([]deliveredBytesSample, deliveredBytesRingSize),
			}
			start := time.Now()
			sequence.observeDeliveredBytes(1, start)
			for tick := 1; tick <= 200; tick++ {
				sequence.observeDeliveredBytes(ByteCount(int64(rate)*int64(compression)/int64(time.Second)), start.Add(time.Duration(tick)*compression))
			}
			got, total, _ := sequence.deliveredServiceRate(200*time.Millisecond, start.Add(200*compression))
			if got != rate || total != 1+ByteCount(200*int64(rate)*int64(compression)/int64(time.Second)) {
				t.Fatalf("compression changed service: rate=%d total=%d", got, total)
			}
		}
	}
}

// The rate owner clamps at the target and survives tiny or extreme settings.
// Handshake bytes alone must not impose their low apparent rate on bulk data.
func TestWindowPacingRateBoundsAndHandshake(t *testing.T) {
	for _, test := range []struct {
		name         string
		estimate     SendWindowEstimate
		target, want ByteCount
	}{
		{name: "unknown", estimate: SendWindowEstimate{}, target: 125000000, want: 125000000},
		{name: "handshake", estimate: SendWindowEstimate{Initial: 2000000, WindowRoundTrip: time.Second, ServiceByteRate: 24}, target: 125000000, want: 2000000},
		{name: "slow", estimate: SendWindowEstimate{Initial: 2000000, WindowRoundTrip: time.Second, ServiceByteRate: 125000, ServiceEstablished: true}, target: 125000000, want: 137500},
		{name: "target", estimate: SendWindowEstimate{Initial: 2000000, WindowRoundTrip: time.Second, ServiceByteRate: 125000000, ServiceEstablished: true}, target: 125000000, want: 125000000},
		{name: "tiny", estimate: SendWindowEstimate{Initial: 1, WindowRoundTrip: 2 * time.Second}, target: 125000000, want: 1},
		{name: "overflow", estimate: SendWindowEstimate{Initial: math.MaxInt64, WindowRoundTrip: time.Nanosecond}, target: 125000000, want: 125000000},
	} {
		t.Logf("case: %s", test.name)
		if got := windowPacingRate(test.estimate, test.target); got != test.want {
			t.Fatalf("rate=%d want=%d", got, test.want)
		}
	}
}

// A slow send loop must not turn a steady ACK train into a faster service
// sample when it finally applies the queued replies. Exercise the real ACK
// handoff, coalescer and cumulative-item release with two different delays.
func TestWindowPacingServiceUsesAckArrival(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultSendBufferSettings()
		settings.DeliverySizedWindowScale = 2
		sequence := &SendSequence{
			ctx: context.Background(), client: &Client{}, log: NewNoopLogger(),
			sequenceId: NewId(), acks: make(chan receiveAckMessage, 1),
			ackWindow: newSequenceAckWindow(), resendQueue: newResendQueue(nil, 0),
			sendBufferSettings: settings,
			flightController:   newSendFlightController(settings),
			deliveredBytes:     make([]deliveredBytesSample, deliveredBytesRingSize),
		}
		start := time.Now()
		for i, workerDelay := range []time.Duration{15 * time.Millisecond, 5 * time.Millisecond} {
			if i != 0 {
				time.Sleep(5 * time.Millisecond)
			}
			item := &sendItem{transferItem: transferItem{messageId: NewId(), sequenceNumber: uint64(i + 1)}, transferFrameBytes: MessagePoolGet(2500)}
			sequence.sendItems = append(sequence.sendItems, item)
			sequence.resendQueue.Add(item)
			if result, err := sequence.ackMessageDetailed(receiveAckMessage{messageId: item.messageId, sequenceId: sequence.sequenceId}, 0); err != nil || result != receiveAckHandoffAccepted {
				t.Fatalf("handoff: %v %v", result, err)
			}
			time.Sleep(workerDelay)
			sequence.coalesceReceivedAck(sequence.ackWindow, <-sequence.acks)
			ack := sequence.ackWindow.Snapshot(true).headAck
			sequence.receiveAckAt(ack.messageId, false, ack.tag, false, ack.receivedAtNanos)
		}
		// 2500 bytes arrived 20 ms apart. Application times are only 10 ms
		// apart and would incorrectly report twice this service rate.
		if rate, _, _ := sequence.deliveredServiceRate(time.Second, time.Now()); rate != 125000 {
			t.Fatalf("send-loop delays changed service: %d want 125000", rate)
		}
		if got := sequence.deliveredBytes[sequence.deliveredBytesHead].atNanos; got != start.Add(20*time.Millisecond).UnixNano() {
			t.Fatalf("latest checkpoint is at processing time: %d", got)
		}
	})
}

// A repaired head releases the retained suffix but supplies no new service
// for bytes already SACKed. Repeated SACKs, including after a resend cleared
// its recovery marker, must not inflate the measurement either.
func TestWindowPacingServiceCountsSacksOnce(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultSendBufferSettings()
		settings.DeliverySizedWindowScale = 2
		sequence := &SendSequence{client: &Client{}, log: NewNoopLogger(),
			resendQueue: newResendQueue(nil, 0), sendBufferSettings: settings,
			flightController: newSendFlightController(settings),
			deliveredBytes:   make([]deliveredBytesSample, deliveredBytesRingSize)}
		var items []*sendItem
		for i := range 3 {
			item := &sendItem{transferItem: transferItem{messageId: NewId(), sequenceNumber: uint64(i + 1)}, transferFrameBytes: MessagePoolGet(2500)}
			items = append(items, item)
			sequence.sendItems = append(sequence.sendItems, item)
			sequence.resendQueue.Add(item)
		}
		sequence.receiveAck(items[1].messageId, true, sequenceTag{}, false)
		time.Sleep(20 * time.Millisecond)
		sequence.receiveAck(items[2].messageId, true, sequenceTag{}, false)
		time.Sleep(10 * time.Millisecond)
		sequence.receiveAck(items[1].messageId, true, sequenceTag{}, false)
		items[1].selectiveAcked = false
		sequence.receiveAck(items[1].messageId, true, sequenceTag{}, false)
		if sequence.deliveredByteTotal != 0 || sequence.deliveredServiceByteTotal != 5000 {
			t.Fatal("selective acknowledgements released retained bytes or counted duplicate service")
		}
		time.Sleep(10 * time.Millisecond)
		sequence.receiveAck(items[2].messageId, false, sequenceTag{}, false)
		if rate, total, _ := sequence.deliveredServiceRate(time.Second, time.Now()); rate != 125000 || total != 7500 || sequence.deliveredByteTotal != 7500 {
			t.Fatalf("head repair created false service: rate=%d first-delivered=%d cumulative=%d", rate, total, sequence.deliveredByteTotal)
		}
	})
}

// A smaller service makes queued RTTs grow. Those RTTs must not extend the
// lifetime of the old fast rate while new, slower ACK evidence accumulates.
func TestWindowPacingSlowerServiceSupersedesFastHistory(t *testing.T) {
	sequence := &SendSequence{sendBufferSettings: &SendBufferSettings{DeliverySizedWindowScale: 2}, deliveredBytes: make([]deliveredBytesSample, deliveredBytesRingSize)}
	start := time.Now()
	sequence.observeDeliveredBytes(1, start)
	sequence.observeDeliveredBytes(1250000, start.Add(10*time.Millisecond))
	for tick := 2; tick <= 12; tick++ {
		sequence.observeDeliveredBytes(1250, start.Add(time.Duration(tick)*10*time.Millisecond))
	}
	if rate, _, _ := sequence.deliveredServiceRate(20*time.Second, start.Add(120*time.Millisecond)); rate != 125000 {
		t.Fatalf("queued RTT kept superseded fast service alive: %d want 125000", rate)
	}
}

// Concurrent logical producers share one aggregate service envelope. Each
// producer has its own timer and cancellation, but cannot multiply the burst.
func TestWindowPacingSharedServiceEnvelopeAndCancellation(t *testing.T) {
	for _, rate := range []ByteCount{125000, 1250000, 12500000, 125000000} {
		t.Logf("case: %s", fmt.Sprintf("rate=%d", rate))
		synctest.Test(t, func(t *testing.T) {
			service := &windowPacingService{}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sent := make(chan time.Time, 80)
			done := make(chan struct{}, 4)
			start := time.Now()
			for range 4 {
				go func() {
					defer func() { done <- struct{}{} }()
					pacer := &windowBurstPacer{service: service, rate: rate}
					defer pacer.close()
					for range 20 {
						if err := pacer.waitForService(ctx, 1250); err != nil {
							return
						}
						sent <- time.Now()
					}
				}()
			}
			for i := range 80 {
				elapsed := (<-sent).Sub(start)
				need := time.Duration(float64((i+1)*1250) * float64(time.Second) / float64(rate))
				if elapsed+10*time.Millisecond+80*time.Nanosecond < need {
					t.Fatalf("shared service multiplied its budget at packet %d: elapsed=%s need=%s", i+1, elapsed, need)
				}
			}
			for range 4 {
				<-done
			}
			// A canceled sibling exits its wait without canceling service
			// for the remaining sequence or replenishing any burst credit.
			pacer := &windowBurstPacer{service: service, rate: rate}
			defer pacer.close()
			canceled := make(chan error, 1)
			go func() { canceled <- pacer.waitForService(ctx, int(rate)/100) }()
			synctest.Wait()
			cancel()
			if err := <-canceled; err != context.Canceled {
				t.Fatalf("shared cancellation: %v", err)
			}
			other := &windowBurstPacer{service: service, rate: rate}
			defer other.close()
			if err := other.waitForService(context.Background(), 1250); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Startup and resumed traffic share one finite probe, regardless of producer
// count. Two services have independent timing and delivery evidence.
func TestWindowPacingSharedProbeAndServiceIsolation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service := &windowPacingService{}
		start := time.Now()
		for range 4 {
			pacer := &windowBurstPacer{service: service, rate: 1000000, probeRate: 10000000, probeLimit: 15000}
			if err := pacer.waitForService(context.Background(), 10000); err != nil {
				t.Fatal(err)
			}
			pacer.close()
		}
		if time.Since(start) != 16500*time.Microsecond || service.probeSent != 15000 {
			t.Fatalf("shared probe spent more than 15 kB: elapsed=%s probe=%d", time.Since(start), service.probeSent)
		}
		time.Sleep(time.Second)
		pacer := &windowBurstPacer{service: service, rate: 1000000, probeRate: 10000000, probeLimit: 15000}
		defer pacer.close()
		start = time.Now()
		for range 2 {
			if err := pacer.waitForService(context.Background(), 10000); err != nil {
				t.Fatal(err)
			}
		}
		if time.Since(start) != 10*time.Millisecond {
			t.Fatal("idle shared service replenished its probe")
		}
		other := &windowPacingService{}
		service.observe(1, start)
		service.observe(1250, start.Add(10*time.Millisecond))
		if rate, _, _ := service.measured(time.Second, time.Now()); rate != 125000 {
			t.Fatal("shared observation lost bytes")
		}
		if rate, total, _ := other.measured(time.Second, time.Now()); rate != 0 || total != 0 || !other.next.IsZero() {
			t.Fatal("one service consumed another service's evidence or pacing budget")
		}
	})
}

// Two sequences can compress their replies on opposite sides of a bucket
// boundary. The short gap between those ACKs is not the service interval of
// all the bytes they cover. Applying the replies in reverse order is valid.
func TestWindowPacingSharedCompressionUsesAWholeAckInterval(t *testing.T) {
	start := time.Unix(1700000000, 0)
	for _, reverse := range []bool{false, true} {
		service := &windowPacingService{}
		service.observeRoundTrip(100*time.Millisecond, 50*time.Millisecond, start)
		var arrivals []time.Time
		for tick := range 12 {
			arrivals = append(arrivals, start.Add(time.Duration(tick)*50*time.Millisecond+200*time.Microsecond), start.Add(time.Duration(tick)*50*time.Millisecond+49800*time.Microsecond))
		}
		for i := range arrivals {
			index := i
			if reverse {
				index = len(arrivals) - 1 - i
			}
			service.observe(312500, arrivals[index])
		}
		if rate, _, _ := service.measured(time.Second, start.Add(600*time.Millisecond)); rate != 12500000 {
			t.Fatalf("reverse=%t compressed sibling replies inflated service to %d, want 12500000", reverse, rate)
		}
	}
}

// A 2 MiB opening train lasts only about fourteen milliseconds at the target.
// Keep its first checkpoint: replacing it with the end of a ten-millisecond
// bucket can leave no complete service interval before the window goes idle.
func TestWindowPacingSharedShortTrainAcrossSampleBoundaries(t *testing.T) {
	for phase := range 10 {
		for _, reverse := range []bool{false, true} {
			t.Logf("case: %s", fmt.Sprintf("phase=%d/reverse=%t", phase, reverse))
			start := time.Unix(1700000000, 0).Add(time.Duration(phase) * time.Millisecond)
			service := &windowPacingService{}
			service.observeRoundTrip(400*time.Millisecond, 0, start)
			service.observe(1, start.Add(-400*time.Millisecond))
			for i := range 141 {
				index := i
				if reverse {
					index = 140 - i
				}
				service.observe(12500, start.Add(time.Duration(index)*100*time.Microsecond))
			}
			if rate, _, _ := service.measured(800*time.Millisecond, start.Add(400*time.Millisecond)); rate != 125000000 {
				t.Fatalf("short fast train measured %d B/s, want 125000000", rate)
			}
		}
	}
}

// A small immediate-ACK window still reveals the serializer within one
// bucket. A partial compressed tail supplies a lower-bound observation: moving
// the previous reply's bytes into this interval invents capacity on slow links.
func TestWindowPacingShortAndCompressedTrainEdges(t *testing.T) {
	start := time.Unix(1700000000, 0)
	for _, test := range []struct {
		name                 string
		compression, spacing time.Duration
		bytes                []ByteCount
		want                 ByteCount
	}{
		{name: "immediate-half-millisecond", compression: 0, spacing: 100 * time.Microsecond, bytes: []ByteCount{12500, 12500, 12500, 12500, 12500, 12500}, want: 125000000},
		{name: "compressed-partial-tail", compression: 10 * time.Millisecond, spacing: 10 * time.Millisecond, bytes: []ByteCount{1250000, 500000}, want: 50000000},
		{name: "sparse-slow-service", compression: 10 * time.Millisecond, spacing: 20 * time.Millisecond, bytes: []ByteCount{2500, 2500}, want: 125000},
		{name: "different-sized-slow-replies", compression: 10 * time.Millisecond, spacing: 20 * time.Millisecond, bytes: []ByteCount{5000, 2500}, want: 125000},
	} {
		t.Logf("case: %s", test.name)
		service := &windowPacingService{}
		service.observeRoundTrip(400*time.Millisecond, test.compression, start)
		for i, bytes := range test.bytes {
			service.observe(bytes, start.Add(time.Duration(i)*test.spacing))
		}
		if rate, _, _ := service.measured(time.Second, start.Add(400*time.Millisecond)); rate != test.want {
			t.Fatalf("measured=%d want=%d B/s", rate, test.want)
		}
	}
}

// Flight larger than a provisional service estimate does not prove a queue:
// on a fast long path it can still be propagating at the opening train's rate.
// Only excess observed residence can justify stopping the capacity probe.
func TestWindowPacingProbeNeedsQueueDelayEvidence(t *testing.T) {
	start := time.Unix(1700000000, 0)
	service := &windowPacingService{sent: 24000000}
	service.observeRoundTrip(400*time.Millisecond, 10*time.Millisecond, start)
	service.observeRoundTrip(405*time.Millisecond, 10*time.Millisecond, start.Add(time.Second))
	if service.backlogged(50000000) {
		t.Fatal("propagating flight was treated as a standing queue")
	}
	service.observeRoundTrip(450*time.Millisecond, 10*time.Millisecond, start.Add(2*time.Second))
	if !service.backlogged(50000000) {
		t.Fatal("excess flight and queue residence did not stop the probe")
	}
	service.sent, service.total = 48000000, 24000000
	service.observeRoundTrip(405*time.Millisecond, 10*time.Millisecond, start.Add(3*time.Second))
	if !service.backlogged(50000000) {
		t.Fatal("a complete delivered residence did not establish the flight bound")
	}
}

// An opening train must span two compressed replies to measure a complete
// interval. Long compression cannot grant an unlimited discovery burst.
func TestWindowPacingOpeningProbeCoversBoundedCompression(t *testing.T) {
	for _, compression := range []time.Duration{0, 10 * time.Millisecond, 50 * time.Millisecond, time.Hour} {
		sequence := newEstimatorFixture(t, func(settings *SendBufferSettings) {
			settings.DeliverySizedWindowScale = 2
			settings.ResendQueueMaxByteCount = mib(2)
			settings.TargetGoodputByteRate = 125000000
		})
		sequence.observeReceiveWindowAdvertisement(receiveAckMessage{ackCompressTimeoutSet: true, ackCompressTimeoutMicros: uint32(compression.Microseconds())})
		estimate := sequence.sendWindowEstimate(time.Now())
		if estimate.PacingProbeByteCount < mib(2) || estimate.PacingProbeByteCount > mib(4) {
			t.Fatalf("compression=%s unbounded probe=%d", compression, estimate.PacingProbeByteCount)
		}
		if compression == 10*time.Millisecond {
			service := time.Duration(float64(estimate.PacingProbeByteCount) * float64(time.Second) / float64(estimate.PacingProbeByteRate))
			if service < 2*compression-time.Nanosecond {
				t.Fatalf("probe ended before two compressed replies: %s", service)
			}
		}
	}
}

// A new send rate cannot affect ACKs until one round trip later. Discarding a
// fast sample after four ten-millisecond buckets creates a 400 ms rate cycle.
// The minimum path RTT bounds retention even when queue residence grows.
func TestWindowPacingRetainsServiceUntilFeedbackCanReturn(t *testing.T) {
	start := time.Unix(1700000000, 0)
	service := &windowPacingService{}
	service.observeRoundTrip(400*time.Millisecond, 10*time.Millisecond, start)
	for i := range 46 {
		bytes := ByteCount(100000)
		if i < 3 {
			bytes = 1250000
		}
		at := start.Add(time.Duration(i) * 10 * time.Millisecond)
		service.observe(bytes, at)
		if i == 30 {
			service.observeRoundTrip(5*time.Second, 10*time.Millisecond, at)
			if rate, _, _ := service.measured(10*time.Second, at); rate != 125000000 {
				t.Fatalf("service forgotten before feedback could return: %d B/s", rate)
			}
		}
	}
	if rate, _, _ := service.measured(10*time.Second, start.Add(500*time.Millisecond)); rate != 10000000 {
		t.Fatalf("queue growth retained expired fast evidence: %d B/s", rate)
	}
}

// Compressed replies may bunch into one interval and leave the next empty.
// A standing queue needs their sustained service rate, not the burst peak.
func TestWindowPacingBackloggedServiceUsesSustainedDelivery(t *testing.T) {
	start := time.Unix(1700000000, 0)
	service := &windowPacingService{}
	service.observeRoundTrip(120*time.Millisecond, 10*time.Millisecond, start)
	for i := range 101 {
		bytes := []ByteCount{125000, 125000, 250000, 0}[i%4]
		service.observe(bytes, start.Add(time.Duration(i)*10*time.Millisecond))
	}
	now := start.Add(time.Second)
	service.sent = service.total + 2000000
	if rate, _, _ := service.measured(time.Second, now); rate != 25000000 {
		t.Fatalf("uncongested train lost its discovery peak: %d", rate)
	}
	service.observeRoundTrip(160*time.Millisecond, 10*time.Millisecond, now)
	if rate, _, _ := service.measured(time.Second, now); rate != 12500000 {
		t.Fatalf("queued service followed compressed burst peak: %d, want 12500000", rate)
	}
}

// Compressed heads from sparse shared producers can leave gaps longer than
// two buckets. With a proven outstanding queue those gaps belong to the same
// service interval; discarding them turns a 125 kB/s serializer into 250 kB/s.
func TestWindowPacingBackloggedSparseHeadsKeepTheirTime(t *testing.T) {
	start := time.Unix(1700000000, 0)
	for _, reverse := range []bool{false, true} {
		service := &windowPacingService{}
		service.observeRoundTrip(120*time.Millisecond, 10*time.Millisecond, start)
		for i := range 42 {
			index := i
			if reverse {
				index = 41 - i
			}
			at := start.Add(time.Duration(index/2) * 60 * time.Millisecond)
			bytes := ByteCount(5000)
			if index%2 != 0 {
				at = at.Add(10 * time.Millisecond)
				bytes = 2500
			}
			service.observe(bytes, at)
		}
		now := start.Add(1210 * time.Millisecond)
		service.sent = service.total + 1000000
		service.observeRoundTrip(time.Second, 10*time.Millisecond, now)
		if rate, _, _ := service.measured(time.Second, now); rate != 125000 {
			t.Errorf("reverse=%t: sparse heads measured %d B/s, want 125000", reverse, rate)
		}
	}
}

// Matching a noisy service estimate exactly can preserve a full queue. A
// queued sender must leave drain capacity, then resume discovery when clear.
func TestWindowPacingDrainsAQueueAtEachServiceRate(t *testing.T) {
	for _, rate := range []ByteCount{125000, 1250000, 12500000, 125000000} {
		t.Logf("case: %s", fmt.Sprintf("rate=%d", rate))
		synctest.Test(t, func(t *testing.T) {
			estimate := SendWindowEstimate{ServiceByteRate: rate, ServiceEstablished: true, ServiceBacklogged: true}
			pacer := &windowBurstPacer{rate: windowPacingRate(estimate, 4*rate)}
			defer pacer.close()
			start := time.Now()
			for range 100 {
				if err := pacer.waitForService(context.Background(), int(rate/100)); err != nil {
					t.Fatal(err)
				}
			}
			// One second of newly offered service must leave enough
			// elapsed capacity to drain an existing 20 ms queue.
			if time.Since(start) < 1020*time.Millisecond {
				t.Fatalf("queued service had no drain capacity: %s", time.Since(start))
			}
			estimate.ServiceBacklogged = false
			if windowPacingRate(estimate, 4*rate) <= rate {
				t.Fatal("cleared service could not discover spare capacity")
			}
		})
	}
}

// Probing is permitted only with room in the measured flight. A recovery
// consumes service time without inventing another outstanding data message;
// canceled sequence ownership must not suppress future probes forever.
func TestWindowPacingBacklogProbeAndCanceledOwnership(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service := &windowPacingService{}
		start := time.Now()
		service.observeRoundTrip(100*time.Millisecond, 10*time.Millisecond, start)
		pacer := &windowBurstPacer{service: service, rate: 1000000}
		for range 4 {
			if err := pacer.waitForService(context.Background(), 50000); err != nil {
				t.Fatal(err)
			}
		}
		service.observeRoundTrip(250*time.Millisecond, 10*time.Millisecond, time.Now())
		if !service.backlogged(1000000) {
			t.Fatal("200 kB did not fill a 112 ms service flight")
		}
		estimate := SendWindowEstimate{WindowRoundTrip: 110 * time.Millisecond, ServiceByteRate: 1000000, ServiceEstablished: true, ServiceBacklogged: true}
		if rate := windowPacingRate(estimate, 10000000); rate != 950000 {
			t.Fatal("backlogged service did not leave drain capacity")
		}
		before := service.sent
		if err := pacer.waitForServiceWrite(context.Background(), 100000, true); err != nil {
			t.Fatal(err)
		}
		if service.sent != before {
			t.Fatal("recovery duplicated outstanding service bytes")
		}
		service.observe(50000, time.Now())
		pacer.serviceAcked = 50000
		pacer.close()
		if service.backlogged(1000000) || service.sent != service.total {
			t.Fatal("closed sequence retained a phantom service backlog")
		}
		estimate.ServiceBacklogged = false
		if rate := windowPacingRate(estimate, 10000000); rate != 1100000 {
			t.Fatal("available service could not probe spare capacity")
		}
		service.observeRoundTrip(2*time.Second, 10*time.Millisecond, time.Now())
		if service.minRoundTrip != 100*time.Millisecond {
			t.Fatal("queued RTT inflated the service baseline")
		}
		time.Sleep(time.Minute)
		service.observeRoundTrip(500*time.Millisecond, 10*time.Millisecond, time.Now())
		if service.minRoundTrip != 500*time.Millisecond {
			t.Fatal("resumed service retained an expired route baseline")
		}
	})
}

// The ACK covers the complete paced envelope, including its encrypted
// wrapper. Crediting only inner bytes would manufacture a permanent backlog.
func TestWindowPacingCreditsTheFirstWireEnvelope(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultSendBufferSettings()
		settings.DeliverySizedWindowScale = 2
		service := &windowPacingService{}
		sequence := &SendSequence{client: &Client{}, log: NewNoopLogger(),
			resendQueue: newResendQueue(nil, 0), sendBufferSettings: settings,
			flightController: newSendFlightController(settings), deliveredBytes: make([]deliveredBytesSample, deliveredBytesRingSize),
			windowPacer: windowBurstPacer{service: service, rate: 1000000}}
		item := &sendItem{transferItem: transferItem{messageId: NewId(), sequenceNumber: 1}, transferFrameBytes: MessagePoolGet(2500), pacingByteCount: 2600}
		sequence.sendItems = append(sequence.sendItems, item)
		sequence.resendQueue.Add(item)
		if err := sequence.windowPacer.waitForService(context.Background(), 2600); err != nil {
			t.Fatal(err)
		}
		sequence.receiveAck(item.messageId, false, sequenceTag{}, false)
		if service.sent != 2600 || service.total != 2600 || sequence.deliveredByteTotal != 2500 {
			t.Fatalf("wire and retained byte ownership diverged: sent=%d delivered=%d retained-release=%d", service.sent, service.total, sequence.deliveredByteTotal)
		}
		sequence.windowPacer.close()
		if service.sent != service.total {
			t.Fatal("acknowledged envelope was released twice at shutdown")
		}
	})
}

func TestWindowPacingUsesDeliveryBeforeTheConfiguredTarget(t *testing.T) {
	const target = ByteCount(125000000)
	if rate := windowPacingRate(SendWindowEstimate{}, target); rate != target {
		t.Fatalf("blind pacing rate %d, want %d", rate, target)
	}
	estimate := SendWindowEstimate{Initial: mib(2), WindowRoundTrip: 100 * time.Millisecond}
	if rate := windowPacingRate(estimate, target); rate != 10*mib(2) {
		t.Fatalf("startup ignores residence: %d", rate)
	}
	estimate.ServiceByteRate = 12500000
	estimate.ServiceEstablished = true
	if rate := windowPacingRate(estimate, target); rate != 13750000 {
		t.Fatalf("slow link ignored measured service: %d", rate)
	}
	estimate.ServiceByteRate = target
	if rate := windowPacingRate(estimate, target); rate != target {
		t.Fatalf("fast link exceeded target: %d", rate)
	}
}

// The initial packet train measures a fast serializer even while a small
// opening window forces long idle gaps. The peak must expire on a changed path.
func TestWindowPacingServiceMeasurementExcludesWindowIdleAndExpires(t *testing.T) {
	start := time.Now()
	sequence := &SendSequence{
		deliveredBytes: []deliveredBytesSample{
			{atNanos: start.UnixNano()},
			{atNanos: start.Add(10 * time.Millisecond).UnixNano(), total: 1250000, serviceTotal: 1250000},
			{atNanos: start.Add(20 * time.Millisecond).UnixNano(), total: 2500000, serviceTotal: 2500000},
			{atNanos: start.Add(400 * time.Millisecond).UnixNano(), total: 2500001, serviceTotal: 2500001},
		},
		deliveredBytesHead: 3, deliveredBytesCount: 4,
	}
	service, _, _ := sequence.deliveredServiceRate(800*time.Millisecond, start.Add(400*time.Millisecond))
	if service != 125000000 {
		t.Fatalf("window-limited idle reduced measured service: %d", service)
	}
	estimate := SendWindowEstimate{Initial: mib(2), WindowRoundTrip: 400 * time.Millisecond, ServiceByteRate: service}
	if rate := windowPacingRate(estimate, 150000000); rate != 137500000 {
		t.Fatalf("long path retained a second startup ramp: %d", rate)
	}
	if rate, _, _ := sequence.deliveredServiceRate(800*time.Millisecond, start.Add(2*time.Second)); rate != 0 {
		t.Fatalf("expired service evidence retained: %d", rate)
	}
}

func TestWindowBurstPacingBoundsBusyAndIdleWrites(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pacer := &windowBurstPacer{}
		defer pacer.close()
		start := time.Now()
		for range 20 {
			if err := pacer.wait(context.Background(), 10000, 10000000); err != nil {
				t.Fatal(err)
			}
		}
		if elapsed := time.Since(start); elapsed != 10*time.Millisecond {
			t.Fatalf("200 kB at 10 MB/s escaped its burst allowance: %s", elapsed)
		}
		time.Sleep(time.Second)
		start = time.Now()
		for range 11 {
			if err := pacer.wait(context.Background(), 10000, 10000000); err != nil {
				t.Fatal(err)
			}
		}
		if time.Since(start) != 10*time.Millisecond {
			t.Fatal("idle time accumulated an unbounded burst")
		}
	})
}

func TestWindowBurstPacingCancellationJoinsWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		pacer := &windowBurstPacer{}
		defer pacer.close()
		done := make(chan error, 1)
		go func() { done <- pacer.wait(ctx, 200000, 10000000) }()
		synctest.Wait()
		cancel()
		if err := <-done; err != context.Canceled {
			t.Fatalf("pacing wait after cancellation: %v", err)
		}
	})
}

// Timer dispatch and the next write can run slightly after the requested
// deadline. Discarding that elapsed service time on every burst lowers the
// configured rate even when the path has capacity.
func TestWindowBurstPacingRetainsBoundedTimerLateness(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pacer := &windowBurstPacer{}
		defer pacer.close()
		start := time.Now()
		for range 10 {
			for range 3 {
				if err := pacer.wait(context.Background(), 10000, 10000000); err != nil {
					t.Fatal(err)
				}
			}
			time.Sleep(500 * time.Microsecond)
		}
		if elapsed := time.Since(start); elapsed > 31*time.Millisecond || elapsed < 20*time.Millisecond {
			t.Fatalf("bounded timer lateness reduced 300 kB at 10 MB/s: %s", elapsed)
		}
	})
}
