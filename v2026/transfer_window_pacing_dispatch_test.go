// Physical release quanta must not inherit the longer ACK measurement window.
package connect

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"testing/synctest"
	"time"
)

// A 300 us path still needs a whole compressed ACK interval for its rate
// measurement. Releasing that interval's bytes at once, however, puts a
// 10 ms ordinary-data FIFO in front of the other direction's ACKs.
func TestWindowPacingDispatchQuantumSeparatesMeasurement(t *testing.T) {
	for _, test := range []struct {
		name                       string
		network, wait, compression time.Duration
		wantDispatch, wantSampling time.Duration
	}{
		{"short", 300 * time.Microsecond, 10 * time.Millisecond, 10 * time.Millisecond, 2500 * time.Microsecond, 10 * time.Millisecond},
		{"medium", 20 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond, 5 * time.Millisecond, 10 * time.Millisecond},
		{"long", 100 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond},
		{"receiver-wait-is-not-network", 300 * time.Microsecond, 400 * time.Millisecond, 400 * time.Millisecond, 2500 * time.Microsecond, 100 * time.Millisecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := newWindowPacingService(DefaultSendBufferSettings())
			at := time.Unix(1700000000, 0)
			service.observeReceiverRoundTrip(1, test.network+test.wait, test.network, test.compression, at)
			if service.bucketInterval != test.wantSampling {
				t.Fatalf("dispatch tuning changed the measurement horizon: got=%s want=%s", service.bucketInterval, test.wantSampling)
			}
			waiter := &windowPacingWaiter{}
			service.reserve(at, 1280, 125000000, 125000000, 0, 0, false, waiter)
			wantBytes := ByteCount(float64(125000000) * test.wantDispatch.Seconds())
			if service.burstEstimateTime != test.wantDispatch || service.burstMeter.limit != wantBytes {
				t.Fatalf("measurement bucket became a physical FIFO burst: duration=%s bytes=%d want=%s/%d", service.burstEstimateTime, service.burstMeter.limit, test.wantDispatch, wantBytes)
			}
		})
	}
}

// Every concurrent producer spends the same physical release allowance. The
// prefix gate fails deterministically with the former 1.25 MB / 10 ms burst,
// rather than relying on an aggregate throughput value landing near a cutoff.
func TestWindowPacingShortPathSharedDispatchEnvelope(t *testing.T) {
	for _, lanes := range []int{1, 8} {
		t.Run(fmt.Sprintf("lanes=%d", lanes), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const rate = ByteCount(125000000)
				const packet = 1250
				const perLane = 400
				service := newWindowPacingService(DefaultSendBufferSettings())
				ids := make([]Id, lanes)
				service.writes = map[Id]windowPacingWrite{}
				for i := range ids {
					ids[i] = NewId()
					service.writes[ids[i]] = windowPacingWrite{}
				}
				start := time.Now()
				service.observeReceiverRoundTrip(1, 10300*time.Microsecond, 300*time.Microsecond, 10*time.Millisecond, start)
				sent := make(chan time.Time, lanes*perLane)
				done := make(chan struct{}, lanes)
				for _, id := range ids {
					go func() {
						defer func() { done <- struct{}{} }()
						pacer := &windowBurstPacer{service: service, serviceSequenceId: id, rate: rate, estimateRate: rate}
						defer pacer.close()
						for range perLane {
							if err := pacer.waitForService(context.Background(), packet); err != nil {
								t.Error(err)
								return
							}
							sent <- time.Now()
						}
					}()
				}
				var arrivals []time.Time
				for range lanes * perLane {
					arrivals = append(arrivals, <-sent)
				}
				for range lanes {
					<-done
				}
				slices.SortFunc(arrivals, time.Time.Compare)
				const burstBytes = ByteCount(312500)
				for i, at := range arrivals {
					bytes := ByteCount((i + 1) * packet)
					allowed := ByteCount(float64(rate)*at.Sub(start).Seconds()) + burstBytes
					if bytes > allowed {
						t.Fatalf("shared short-path burst blocked reverse ACKs: prefix=%d bytes=%d allowed=%d elapsed=%s", i+1, bytes, allowed, at.Sub(start))
					}
				}
			})
		})
	}
}

// A tightly limited window must keep the former batching that earns early
// ACK credit. The check uses hard permission, not a transient learned size,
// and cannot increase the former maximum burst for either class of sender.
func TestWindowPacingDispatchConstrainedWindowKeepsCompressionBurst(t *testing.T) {
	for _, test := range []struct {
		name                  string
		ceiling, window, rate ByteCount
		compression           time.Duration
		hold                  bool
	}{
		{"below-interval", 1249999, 1000000, 125000000, 10 * time.Millisecond, true},
		{"exact-interval", 1250000, 1000000, 125000000, 10 * time.Millisecond, false},
		{"ample-permission-small-current", 2000000, 10000, 125000000, 10 * time.Millisecond, false},
		{"fixed-window", 0, 1000000, 125000000, 10 * time.Millisecond, true},
		{"no-permission", 0, 0, 125000000, 10 * time.Millisecond, true},
		{"unknown-rate", 2000000, 2000000, 0, 10 * time.Millisecond, true},
		{"unknown-compression", 2000000, 2000000, 125000000, 0, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			estimate := SendWindowEstimate{Ceiling: test.ceiling, Window: test.window, PacingProbeByteRate: test.rate}
			hold := windowPacingHoldCompressionBurst(estimate, test.compression)
			if hold != test.hold {
				t.Fatalf("compressed ACK interval eligibility: got=%t want=%t", hold, test.hold)
			}
			service := newWindowPacingService(DefaultSendBufferSettings())
			at := time.Unix(1700000000, 0)
			service.observeReceiverRoundTrip(1, 10300*time.Microsecond, 300*time.Microsecond, 10*time.Millisecond, at)
			service.reserve(at, 1280, 125000000, 125000000, 0, 0, false, &windowPacingWaiter{holdCompressionBurst: hold})
			want := ByteCount(312500)
			if hold {
				want = 1250000
			}
			if service.burstMeter.limit != want {
				t.Fatalf("compressed ACK interval burst: got=%d want=%d", service.burstMeter.limit, want)
			}
		})
	}
}

// An opposing service still has its own batching delay. A smaller local
// release must not lower the pre-existing flight tolerance and reprice that
// healthy reverse-ACK residence as congestion. Excess flight remains bounded.
func TestWindowPacingSmallDispatchPreservesFeedbackFlightBound(t *testing.T) {
	service := newWindowPacingService(DefaultSendBufferSettings())
	at := time.Unix(1700000000, 0)
	service.observeReceiverRoundTrip(1, 10300*time.Microsecond, 300*time.Microsecond, 10*time.Millisecond, at)
	service.reserve(at, 1280, 125000000, 125000000, 0, 0, false, &windowPacingWaiter{})
	service.sent, service.total, service.reservedByteCount = 5000000, 3200000, 0
	at = at.Add(30 * time.Millisecond)
	service.observeReceiverRoundTrip(2, 20300*time.Microsecond, 10300*time.Microsecond, 10*time.Millisecond, at)
	if service.burstMeter.limit != 312500 || service.flightBoundAtWithLock(125000000, 10300*time.Microsecond) != 2537500 || service.backloggedAt(125000000, at) {
		t.Fatal("a smaller physical release invented congestion from a permitted opposing burst")
	}
	service.total = 2000000
	if !service.backloggedAt(125000000, at) {
		t.Fatal("retaining feedback tolerance excused excess physical flight")
	}
}

// An RTT decrease cannot erase payment already owed by a previous, larger
// burst. It changes future admission, not the bytes physically dispatched.
func TestWindowPacingDispatchShrinkPreservesDebt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service := newWindowPacingService(DefaultSendBufferSettings())
		service.observeRoundTrip(100*time.Millisecond, 0, time.Now())
		pacer := &windowBurstPacer{service: service, rate: 10000000, estimateRate: 10000000}
		defer pacer.close()
		start := time.Now()
		if err := pacer.waitForService(context.Background(), 90000); err != nil {
			t.Fatal(err)
		}
		service.observeReceiverRoundTrip(1, 10300*time.Microsecond, 300*time.Microsecond, 10*time.Millisecond, time.Now())
		if err := pacer.waitForService(context.Background(), 10000); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed < 9*time.Millisecond {
			t.Fatalf("smaller dispatch quantum erased prior burst debt: elapsed=%s", elapsed)
		}
		if service.probeSent != 0 {
			t.Fatal("dispatch resizing manufactured a new discovery probe")
		}
	})
}

// A tightly limited sibling may still release the old-sized burst. Switching
// to a better-provisioned writer cannot replenish the shared meter or let its
// smaller quantum erase the serialization already owed by that sibling.
func TestWindowPacingDispatchMixedPermissionsKeepSharedDebt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service := newWindowPacingService(DefaultSendBufferSettings())
		service.observeReceiverRoundTrip(1, 10300*time.Microsecond, 300*time.Microsecond, 10*time.Millisecond, time.Now())
		constrained := &windowBurstPacer{service: service, rate: 125000000, estimateRate: 125000000}
		constrained.waiter.holdCompressionBurst = true
		smooth := &windowBurstPacer{service: service, rate: 125000000, estimateRate: 125000000}
		defer constrained.close()
		defer smooth.close()
		start := time.Now()
		for range 900 {
			if err := constrained.waitForService(context.Background(), 1000); err != nil {
				t.Fatal(err)
			}
		}
		if !time.Now().Equal(start) {
			t.Fatal("constrained sender lost its original compressed burst")
		}
		if err := smooth.waitForService(context.Background(), 1250); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed < 7200*time.Microsecond {
			t.Fatalf("a sibling's smaller quantum forgave the shared debt: %s", elapsed)
		}
		if service.burstMeter.limit != 312500 || service.probeSent != 0 {
			t.Fatal("mixed permissions enlarged the smooth burst or minted a probe")
		}
	})
}

// Timer precision is a host property, not a slower physical network. A
// scheduler that wakes later than the fine quantum must amortize that delay
// within the old bound rather than serializing every packet behind a timer.
func TestWindowPacingDispatchAdaptsToTimerLateness(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const rate = ByteCount(125000000)
		const packet = 16384
		service := newWindowPacingService(DefaultSendBufferSettings())
		service.observeReceiverRoundTrip(1, 10300*time.Microsecond, 300*time.Microsecond, 10*time.Millisecond, time.Now())
		pacer := &windowBurstPacer{service: service, rate: rate, estimateRate: rate, afterWaitForTest: func() { time.Sleep(3 * time.Millisecond) }}
		defer pacer.close()
		for range 100 {
			if err := pacer.waitForService(context.Background(), packet); err != nil {
				t.Fatal(err)
			}
		}
		start := time.Now()
		for range 1000 {
			if err := pacer.waitForService(context.Background(), packet); err != nil {
				t.Fatal(err)
			}
			if service.burstMeter.limit > 1250000 || service.burstEstimateTime > 10*time.Millisecond {
				t.Fatal("late wake increased the old physical burst allowance")
			}
		}
		measured := 1000 * float64(packet) / time.Since(start).Seconds()
		if measured < .9*float64(rate) {
			t.Fatalf("late scheduler became the apparent service rate: got=%.0f want>=%.0f bytes/s", measured, .9*float64(rate))
		}
		if service.timerWakeDelay != 3*time.Millisecond {
			t.Fatalf("wrong timer lateness attribution: %s", service.timerWakeDelay)
		}
		for range 32 {
			service.observePacingTimerWake(0)
		}
		if service.timerWakeDelay >= windowPacingMinimumBurstTime/2 {
			t.Fatal("a recovered scheduler permanently lost fine dispatch")
		}
		service.observePacingTimerWake(time.Hour)
		if service.timerWakeDelay > 5*time.Millisecond {
			t.Fatal("one stall enlarged the original cap")
		}
	})
}

// A single indivisible frame retains its size floor and pays its complete
// serialization time; a smaller time quantum must not split or free it.
func TestWindowPacingDispatchKeepsIndivisibleMessageBound(t *testing.T) {
	service := newWindowPacingService(DefaultSendBufferSettings())
	at := time.Unix(1700000000, 0)
	service.observeReceiverRoundTrip(1, 10300*time.Microsecond, 300*time.Microsecond, 10*time.Millisecond, at)
	deadline := service.reserve(at, 65536, 125000, 125000, 0, 0, false, &windowPacingWaiter{})
	if service.burstMeter.limit != 65536 || service.burstEstimateTime != 524288*time.Microsecond || deadline.Sub(at) != 524288*time.Microsecond {
		t.Fatalf("indivisible write lost its byte/time payment: limit=%d interval=%s deadline=%s", service.burstMeter.limit, service.burstEstimateTime, deadline.Sub(at))
	}
}
