// Delayed timer dispatch must preserve attainable service without turning
// scheduler stalls into an unlimited catch-up burst.
package connect

import (
	"context"
	"math"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// Late dispatch can expire a nominal reservation while its physical burst
// has only just started. RTT evidence follows the bytes actually released.
func TestWindowPacingBurstEpochFollowsActualDispatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service := &windowPacingService{next: time.Now().Add(time.Millisecond)}
		pacer := &windowBurstPacer{service: service, rate: 100000, afterWaitForTest: func() { time.Sleep(30 * time.Millisecond) }}
		defer pacer.close()
		if err := pacer.waitForService(context.Background(), 400); err != nil {
			t.Fatal(err)
		}
		burst, at := pacer.waiter.burst, time.Now()
		if err := pacer.waitForService(context.Background(), 400); err != nil {
			t.Fatal(err)
		}
		if time.Now() != at || pacer.waiter.burst != burst {
			t.Fatalf("one actual 800-byte burst used two reservation epochs: first=%d second=%d", burst, pacer.waiter.burst)
		}
		time.Sleep(21 * time.Millisecond)
		if err := pacer.waitForService(context.Background(), 100); err != nil {
			t.Fatal(err)
		}
		if pacer.waiter.burst != burst+1 {
			t.Fatal("an expired actual burst retained its measurement epoch")
		}
	})
}

// A late timer can leave the serialization schedule in the past. Changing
// the byte estimate must still retain the bytes just released at that wake.
func TestWindowPacingChangedEstimateRetainsActualBurstCharge(t *testing.T) {
	for _, rate := range []ByteCount{5000, 20000} {
		synctest.Test(t, func(t *testing.T) {
			service := &windowPacingService{next: time.Now().Add(time.Millisecond)}
			pacer := &windowBurstPacer{service: service, rate: 10000, afterWaitForTest: func() { time.Sleep(15 * time.Millisecond) }}
			defer pacer.close()
			if err := pacer.waitForService(context.Background(), 40); err != nil {
				t.Fatal(err)
			}
			pacer.rate, pacer.afterWaitForTest = rate, nil
			remaining := int(rate/100) - 40
			if err := pacer.waitForService(context.Background(), remaining); err != nil {
				t.Fatal(err)
			}
			at := time.Now()
			if err := pacer.waitForService(context.Background(), 1); err != nil {
				t.Fatal(err)
			}
			if time.Now() == at {
				t.Fatalf("rate=%d: estimate change forgot bytes released at the delayed wake", rate)
			}
		})
	}
}

// Raising a limit withholds fresh credit; lowering it again must not confuse
// that unissued credit with spent bytes. A true overspend still needs service.
func TestWindowPacingBurstMeterEstimateRoundTripRetainsOnlyRealDebt(t *testing.T) {
	start := time.Unix(1700000000, 0)
	meter := &windowPacingBurstMeter{}
	meter.update(start, 100, 1000)
	if meter.wait(start, 40) != 0 {
		t.Fatal("initial bytes did not fit")
	}
	meter.update(start, 200, 1000)
	meter.update(start, 100, 1000)
	if delay := meter.wait(start, 60); delay != 0 {
		t.Fatalf("unissued credit became false byte debt: delay=%s", delay)
	}
	meter.update(start, 50, 1000)
	if delay := meter.wait(start, 1); delay != 51*time.Millisecond {
		t.Fatalf("a smaller estimate erased real byte debt: delay=%s want=51ms", delay)
	}
	if delay := meter.wait(start.Add(51*time.Millisecond), 1); delay != 0 {
		t.Fatalf("elapsed service did not repay byte debt: delay=%s", delay)
	}
}

// Landing just before the next nominal burst does not serialize a late
// physical release. Old-rate credit starts when those bytes actually leave.
func TestWindowPacingDecreasedEstimateCannotForgiveALateRelease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service := &windowPacingService{next: time.Now().Add(time.Nanosecond)}
		pacer := &windowBurstPacer{service: service, rate: 1000000, afterWaitForTest: func() { time.Sleep(8*time.Millisecond - time.Nanosecond) }}
		defer pacer.close()
		if err := pacer.waitForService(context.Background(), 8000); err != nil {
			t.Fatal(err)
		}
		at := time.Now()
		pacer.rate, pacer.afterWaitForTest = 100000, nil
		if err := pacer.waitForService(context.Background(), 1000); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(at); elapsed < 8*time.Millisecond {
			t.Fatalf("nominal deadline forgave a late physical burst: elapsed=%s want>=8ms", elapsed)
		}
	})
}

// A producer whose timer fired first retains its place while dispatch is
// delayed. Later smaller writes cannot continually spend its release credit.
func TestWindowPacingWaitingWritersKeepReservationOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service := &windowPacingService{}
		first := &windowBurstPacer{service: service, rate: 100000}
		defer first.close()
		if err := first.waitForService(context.Background(), 1000); err != nil {
			t.Fatal(err)
		}
		dispatch := make(chan struct{})
		done := make(chan int, 2)
		go func() {
			pacer := &windowBurstPacer{service: service, rate: 100000, afterWaitForTest: func() { <-dispatch }}
			defer pacer.close()
			if err := pacer.waitForService(context.Background(), 2000); err != nil {
				t.Error(err)
			}
			done <- 1
		}()
		synctest.Wait()
		go func() {
			pacer := &windowBurstPacer{service: service, rate: 100000}
			defer pacer.close()
			if err := pacer.waitForService(context.Background(), 1000); err != nil {
				t.Error(err)
			}
			done <- 2
		}()
		time.Sleep(50 * time.Millisecond)
		select {
		case newer := <-done:
			close(dispatch)
			<-done
			t.Fatalf("writer %d overtook the older delayed reservation", newer)
		default:
		}
		close(dispatch)
		<-done
		<-done
	})
}

// Removing the head or an interior waiter must preserve both queue links.
// Reusing a canceled producer also cannot consume an old wakeup out of order.
func TestWindowPacingCanceledWaitersPreserveTheirSuccessors(t *testing.T) {
	for _, canceled := range []int{0, 1} {
		synctest.Test(t, func(t *testing.T) {
			service := &windowPacingService{}
			var pacers [3]*windowBurstPacer
			var cancels [3]context.CancelFunc
			var done [3]chan error
			for i := range pacers {
				ctx, cancel := context.WithCancel(context.Background())
				cancels[i] = cancel
				defer cancel()
				pacers[i] = &windowBurstPacer{service: service, rate: 100000}
				defer pacers[i].close()
				done[i] = make(chan error, 1)
				go func() { done[i] <- pacers[i].waitForService(ctx, 10000) }()
				synctest.Wait()
			}
			cancels[canceled]()
			if err := <-done[canceled]; err != context.Canceled {
				t.Fatalf("waiter %d ignored cancellation: %v", canceled, err)
			}
			// Put the same node at the tail behind its live successors.
			go func() { done[canceled] <- pacers[canceled].waitForService(context.Background(), 1000) }()
			for i := range done {
				if i != canceled {
					if err := <-done[i]; err != nil {
						t.Fatalf("successor %d failed: %v", i, err)
					}
				}
			}
			if err := <-done[canceled]; err != nil {
				t.Fatal(err)
			}
			if service.waiterHead != nil || service.waiterTail != nil || service.pacingReservations != 0 {
				t.Fatal("completed or canceled reservations retained queue links")
			}
		})
	}
}

// A physical message is indivisible. Quantize the byte/time estimate to fit
// one message and charge all of its bytes, including on a very slow service.
func TestWindowPacingBurstAccountsFullLargeMessages(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pacer := &windowBurstPacer{rate: 125000}
		defer pacer.close()
		start := time.Now()
		if err := pacer.waitForService(context.Background(), 65536); err != nil {
			t.Fatal(err)
		}
		if time.Since(start) != 524288*time.Microsecond || pacer.service.burstMeter.limit != 65536 || pacer.service.burstMeter.available != 0 {
			t.Fatal("large physical write exceeded its estimate or paid only a partial byte charge")
		}
		start = time.Now()
		if err := pacer.waitForService(context.Background(), 1024); err != nil {
			t.Fatal(err)
		}
		if time.Since(start) != 8192*time.Microsecond {
			t.Fatal("the following small write borrowed a large message's allowance")
		}
	})
}

// Retransmissions reserve serialization without adding first-delivery flight.
// Another producer cannot lower the message-size floor while one is waiting.
func TestWindowPacingWaitingRetryRetainsMessageFloor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		service := &windowPacingService{}
		done := make(chan error, 2)
		for _, bytes := range []int{65536, 1000} {
			go func() {
				pacer := &windowBurstPacer{service: service, rate: 125000}
				defer pacer.close()
				done <- pacer.waitForServiceWrite(ctx, bytes, true)
			}()
			synctest.Wait()
		}
		if service.maxMessageByteCount != 65536 || service.burstMeter.limit != 65536 || service.pacingReservations != 2 {
			t.Error("a smaller retry reset the floor under a larger waiting message")
		}
		cancel()
		for range 2 {
			if err := <-done; err != context.Canceled {
				t.Errorf("retry reservation escaped cancellation: %v", err)
			}
		}
		synctest.Wait()
		if service.pacingReservations != 0 || service.reservedByteCount != 0 || service.sent != service.total {
			t.Fatal("canceled retry retained pacing or flight ownership")
		}
	})
}

// A long serialization remains a future wait, rather than becoming immediate
// permission when conversion to a signed duration overflows.
func TestWindowPacingSerializationCannotOverflow(t *testing.T) {
	if got := windowPacingSerializationTime(ByteCount(math.MaxInt64), 1); got != time.Duration(math.MaxInt64) {
		t.Fatalf("serialization overflowed: %s", got)
	}
	if got := windowPacingSerializationTime(65536, 125000); got != 524288*time.Microsecond {
		t.Fatalf("ordinary serialization changed: %s", got)
	}
}

// A waiting attempt does not consume or manufacture credit. Only successful
// release spends bytes; estimator changes refill past time at the old rate.
func TestWindowPacingBurstMeterCreditAndEstimateChanges(t *testing.T) {
	start := time.Unix(1700000000, 0)
	meter := &windowPacingBurstMeter{}
	meter.update(start, 100, 1000)
	if meter.wait(start, 60) != 0 || meter.wait(start, 50) != 10*time.Millisecond || meter.wait(start, 50) != 10*time.Millisecond {
		t.Fatal("a retry changed the unspent byte allowance")
	}
	meter.update(start.Add(10*time.Millisecond), 200, 2000)
	if got := meter.wait(start.Add(10*time.Millisecond), 100); got != 25*time.Millisecond {
		t.Fatalf("rate change granted credit for earlier time: wait=%s want=25ms", got)
	}
	if got := meter.wait(start.Add(35*time.Millisecond), 100); got != 0 {
		t.Fatalf("earned bytes did not release the message: %s", got)
	}
	meter.update(start.Add(time.Hour), 20, 2000)
	if meter.wait(start.Add(time.Hour), 20) != 0 || meter.wait(start.Add(time.Hour), 1) != 500*time.Microsecond {
		t.Fatal("idle or a smaller estimate accumulated more than one burst")
	}
}

// Producers capture time before acquiring the service lock. An older caller
// winning the lock later cannot rewind the refill clock and earn time twice.
func TestWindowPacingBurstMeterOutOfOrderUpdates(t *testing.T) {
	start := time.Unix(1700000000, 0)
	meter := &windowPacingBurstMeter{}
	meter.update(start, 100, 1000)
	meter.wait(start, 100)
	if meter.wait(start.Add(50*time.Millisecond), 50) != 0 {
		t.Fatal("fifty milliseconds did not earn fifty bytes")
	}
	meter.update(start, 100, 1000)
	if got := meter.wait(start.Add(50*time.Millisecond), 1); got != time.Millisecond {
		t.Fatalf("out-of-order update earned the same elapsed service twice: wait=%s", got)
	}
}

// The time multiplier has no corresponding multiplier on the byte ceiling,
// including when a valid large duration would overflow ordinary arithmetic.
func TestWindowPacingBurstTimeBoundCannotOverflow(t *testing.T) {
	for _, interval := range []time.Duration{time.Millisecond, time.Duration(math.MaxInt64 / 2), time.Duration(math.MaxInt64/2 + 1), time.Duration(math.MaxInt64)} {
		want := time.Duration(math.MaxInt64)
		if interval <= time.Duration(math.MaxInt64/2) {
			want = 2 * interval
		}
		if got := windowPacingBurstMaximumTime(interval); got != want {
			t.Errorf("interval=%s maximum=%s want=%s", interval, got, want)
		}
	}
}

// Exploration may refill faster than measured service, but cannot enlarge
// the per-burst byte estimate to match that faster pacing rate.
func TestWindowPacingBurstBytesUseTheServiceEstimate(t *testing.T) {
	for _, probeRate := range []ByteCount{0, 2000000} {
		synctest.Test(t, func(t *testing.T) {
			pacer := &windowBurstPacer{rate: 1100000, estimateRate: 1000000, probeRate: probeRate, probeLimit: 100000}
			defer pacer.close()
			start := time.Now()
			for range 10 {
				if err := pacer.waitForService(context.Background(), 1000); err != nil {
					t.Fatal(err)
				}
			}
			if time.Now() != start {
				t.Fatal("the estimated ten kB burst did not fit its allowance")
			}
			if err := pacer.waitForService(context.Background(), 1000); err != nil {
				t.Fatal(err)
			}
			if time.Now() == start {
				t.Fatal("the exploration rate enlarged the estimated byte ceiling")
			}
		})
	}
}

// Cancellation can arrive after the timer fires but before the worker runs.
// That write must not escape merely because its pacing deadline has passed.
func TestWindowPacingCancellationDuringTimerDispatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		service := &windowPacingService{}
		pacer := &windowBurstPacer{service: service, rate: 1000000, afterWaitForTest: cancel}
		if err := pacer.waitForService(ctx, 20000); err != context.Canceled {
			t.Fatalf("canceled timer dispatch admitted its write: %v", err)
		}
		if service.reservedByteCount != 0 {
			t.Fatal("canceled dispatch retained a waiting reservation")
		}
		pacer.close()
		if service.sent != service.total {
			t.Fatal("canceled dispatch retained its owner's physical flight")
		}
	})
}

// The byte limit uses one estimate, while its time limit uses two. Crossing
// either boundary starts a new burst without changing previously charged work.
func TestWindowPacingBurstLimitsAreIndependent(t *testing.T) {
	start := time.Unix(1700000000, 0)
	burst := &windowPacingBurst{}
	burst.reserve(start, 60, 100, 10*time.Millisecond, start)
	if got := burst.reserve(start.Add(15*time.Millisecond), 40, 100, 10*time.Millisecond, start.Add(6*time.Millisecond)); got != start || burst.bytes != 100 {
		t.Fatal("time multiplier changed the byte limit or ended a valid partial burst")
	}
	if got := burst.reserve(start.Add(15*time.Millisecond), 1, 100, 10*time.Millisecond, start.Add(10*time.Millisecond)); got != start.Add(10*time.Millisecond) || burst.bytes != 1 {
		t.Fatal("one extra byte borrowed the time multiplier as byte capacity")
	}
	burst = &windowPacingBurst{}
	burst.reserve(start, 1, 100, 10*time.Millisecond, start)
	if got := burst.reserve(start.Add(20*time.Millisecond), 1, 100, 10*time.Millisecond, start.Add(20*time.Millisecond)); got != start.Add(20*time.Millisecond) || burst.bytes != 1 {
		t.Fatal("partial burst survived its maximum duration")
	}
}

// A decrease cannot spend the old larger byte allowance, and an increase
// cannot erase bytes already charged to this burst.
func TestWindowPacingBurstEstimateChanges(t *testing.T) {
	start := time.Unix(1700000000, 0)
	for _, limit := range []ByteCount{50, 200} {
		burst := &windowPacingBurst{}
		burst.reserve(start, 60, 100, 10*time.Millisecond, start)
		got := burst.reserve(start.Add(time.Millisecond), 40, limit, 10*time.Millisecond, start.Add(6*time.Millisecond))
		if limit == 50 && (got != start.Add(6*time.Millisecond) || burst.bytes != 40) {
			t.Fatal("smaller estimate retained an oversized burst")
		}
		if limit == 200 && (got != start || burst.bytes != 100) {
			t.Fatal("larger estimate forgot already spent bytes")
		}
	}
}

// Reservation-time bounds alone are insufficient: a long timer delay can
// deliver several nominal bursts together. Count actual release times across
// four producers so the shared byte ceiling also applies after timer dispatch.
func TestWindowPacingSharedBurstsBoundActualDispatch(t *testing.T) {
	for _, delay := range []time.Duration{0, 3 * time.Millisecond, 15 * time.Millisecond} {
		synctest.Test(t, func(t *testing.T) {
			service := &windowPacingService{}
			sent := make(chan time.Time, 400)
			var workers sync.WaitGroup
			for range 4 {
				workers.Go(func() {
					pacer := &windowBurstPacer{service: service, rate: 1000000, afterWaitForTest: func() { time.Sleep(delay) }}
					defer pacer.close()
					for range 100 {
						if err := pacer.waitForService(context.Background(), 1000); err != nil {
							t.Error(err)
							return
						}
						sent <- time.Now()
					}
				})
			}
			workers.Wait()
			close(sent)
			counts := map[time.Time]int{}
			for at := range sent {
				counts[at] += 1000
			}
			maximum := 0
			for _, bytes := range counts {
				maximum = max(maximum, bytes)
			}
			if maximum > 10000 {
				t.Errorf("delay=%s: actual shared burst=%d exceeds estimated 10000-byte limit", delay, maximum)
			}
		})
	}
}

// A fixed virtual dispatch delay isolates lost pacing credit from CPU load,
// socket scheduling and ACK estimation. Every write still pays its wire size.
func TestWindowPacingDelayedWakeKeepsService(t *testing.T) {
	for _, delay := range []time.Duration{time.Millisecond, 3 * time.Millisecond} {
		synctest.Test(t, func(t *testing.T) {
			const rate = ByteCount(125000000)
			service := &windowPacingService{}
			pacer := &windowBurstPacer{service: service, rate: rate, afterWaitForTest: func() { time.Sleep(delay) }}
			defer pacer.close()
			start := time.Now()
			bytes := ByteCount(0)
			for time.Since(start) < time.Second {
				if err := pacer.waitForService(context.Background(), 16384); err != nil {
					t.Fatal(err)
				}
				bytes += 16384
			}
			ratio := float64(bytes) / time.Since(start).Seconds() / float64(rate)
			t.Logf("wake delay=%s service fraction=%.3f", delay, ratio)
			if ratio < .90 {
				t.Errorf("dispatch delay=%s discarded service capacity: fraction=%.3f", delay, ratio)
			}
		})
	}
}

// The end-to-end virtual link continues serializing while only the sender's
// pacing timer dispatch is delayed. Keep finite queue and per-flow checks.
func TestWindowPathServiceDelayedPacingWake(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, delay := range []time.Duration{time.Millisecond, 3 * time.Millisecond} {
		for _, rtt := range []time.Duration{300 * time.Microsecond, 100 * time.Millisecond} {
			for _, flows := range []int{1, 8} {
				var ceiling, candidate windowPathReading
				for _, arm := range []string{"ceiling", "delivery"} {
					synctest.Test(t, func(t *testing.T) {
						reading := measureWindowPathCell(t, windowPathCell{Arm: arm, RoundTrip: rtt, Compression: 10 * time.Millisecond,
							Flows: flows, RoundRobinOffer: true, Payload: 16384, Budget: mib(48), Rate: 125000000,
							Drop: arm == "delivery", Warmup: time.Second + 5*rtt, PacingWakeDelay: delay}, 2*time.Second)
						logWindowServiceReading(t, reading)
						if arm == "ceiling" {
							ceiling = reading
						} else {
							candidate = reading
						}
					})
				}
				if candidate.Mbps < .9*ceiling.Mbps || candidate.MinFlowMbps == 0 || candidate.MeasurementRelayDrops != 0 {
					t.Errorf("pacing wake delay=%s RTT=%s flows=%d: reference=%.3f candidate=%.3f minimum=%.3f measured-drops=%d",
						delay, rtt, flows, ceiling.Mbps, candidate.Mbps, candidate.MinFlowMbps, candidate.MeasurementRelayDrops)
				}
			}
		}
	}
}
