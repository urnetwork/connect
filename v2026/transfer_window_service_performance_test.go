package connect

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"testing/synctest"
	"time"
)

// A rate change must retain each waiting frame's own propagation delay,
// including the FIFO constraint across an earlier propagation change.
func TestWindowPathFifoCombinedServiceAndPropagationChange(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, delay := range []time.Duration{time.Second, 20 * time.Second} {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			from, to := make(Route), make(Route)
			link := &windowPathLink{rate: 1000, rateAfter: 2000, rateChangeAfter: 500 * time.Millisecond,
				delay: 10 * time.Second, delayAfter: delay, delayChangeAfter: 250 * time.Millisecond,
				queueCount: 3, queueBytes: 3000}
			done := make(chan struct{})
			start := time.Now()
			go func() { defer close(done); link.run(ctx, from, to) }()
			defer func() { cancel(); <-done }()
			for i := range 3 {
				if i == 1 {
					time.Sleep(250 * time.Millisecond)
				}
				frame := MessagePoolGet(1000)
				frame[0] = byte(i)
				from <- frame
			}
			for i, want := range []time.Duration{11 * time.Second, max(11*time.Second, 1500*time.Millisecond+delay), max(11*time.Second, 2*time.Second+delay)} {
				frame := <-to
				got := frame[0]
				MessagePoolReturn(frame)
				if got != byte(i) || time.Since(start) != want {
					t.Errorf("delay=%s frame=%d identity=%d arrival=%s want=%s", delay, i, got, time.Since(start), want)
				}
			}
		})
	}
}

// Opt-in startup trace complements the steady-state ledger without changing
// the ordinary matrix's measurement boundary.
func TestWindowPathPacingStartupTrace(t *testing.T) {
	if os.Getenv("CONNECT_WINDOW_PACING_TRACE") == "" {
		t.Skip("set CONNECT_WINDOW_PACING_TRACE=1")
	}
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		cell := windowPathCell{Arm: "delivery", RoundTrip: 400 * time.Millisecond, Compression: 10 * time.Millisecond, Flows: 1, RoundRobinOffer: true, Payload: 1280, Budget: mib(48), Rate: 125000000, Drop: true}
		switch os.Getenv("CONNECT_WINDOW_PACING_TRACE_CASE") {
		case "rtt-growth":
			cell.RoundTrip, cell.RoundTripAfter, cell.RoundTripChangeAfter = 300*time.Microsecond, 100*time.Millisecond, 4*time.Second
			cell.Rate, cell.Flows, cell.Warmup = 12500000, 8, 8*time.Second
		case "shared-slow":
			cell.RoundTrip, cell.Rate, cell.Flows, cell.Lanes, cell.Warmup = 100*time.Millisecond, 125000, 8, 4, 135*time.Second
		}
		reading := measureWindowPathCell(t, cell, time.Second)
		logWindowServiceReading(t, reading)
	})
}

// Retain every arm, including startup loss, queue bounds and pacing evidence.
func logWindowServiceReading(t *testing.T, reading windowPathReading) {
	t.Helper()
	bytes, err := json.Marshal(reading)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("service-reading %s", bytes)
}

// Expiry at exactly the gap deadline must stop the sequence. Re-arming a
// zero timer here spins forever in virtual time and repeats work in real time.
func TestWindowPathGapDeadline(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		settings := DefaultClientSettings()
		settings.Log = NewNoopLogger()
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
		t.Cleanup(func() {
			cancel()
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		})
		receiveSettings := DefaultReceiveBufferSettings()
		receiveSettings.GapTimeout = 10 * time.Millisecond
		sequence := newReceiveSequence(ctx, client, SourceId(NewId()), NewId(), TransferKey{}, receiveSettings)
		start := time.Now()
		sequence.receiveQueue.Add(&receiveItem{transferItem: transferItem{messageId: NewId(), sequenceNumber: 1, messageByteCount: 16}, receiveTime: start, committed: true, transferFrameBytes: MessagePoolGet(16)})
		go sequence.Run()
		t.Cleanup(func() { sequence.Cancel(); <-sequence.exit })
		time.Sleep(9 * time.Millisecond)
		select {
		case <-sequence.exit:
			t.Fatal("gap expired early")
		default:
		}
		time.Sleep(time.Millisecond)
		<-sequence.exit
		if time.Since(start) != 10*time.Millisecond || sequence.receiveQueue.Len() != 0 {
			t.Fatal("gap did not expire and release ownership at its deadline")
		}
	})
}

// Changing service reserializes waiting frames without moving frames already
// in propagation or interrupting the frame currently in service.
func TestWindowPathFifoServiceChange(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, test := range []struct {
		rate     ByteCount
		arrivals []time.Duration
	}{
		{rate: 2000, arrivals: []time.Duration{11 * time.Second, 11500 * time.Millisecond, 12 * time.Second}},
		{rate: 500, arrivals: []time.Duration{11 * time.Second, 13 * time.Second, 15 * time.Second}},
	} {
		t.Logf("case: %s", fmt.Sprintf("rate=%d", test.rate))
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			from, to := make(Route, 3), make(Route)
			link := windowPathLink{rate: 1000, rateAfter: test.rate, rateChangeAfter: 500 * time.Millisecond, delay: 10 * time.Second, queueCount: 3, queueBytes: 3000, dropOnFull: true}
			done := make(chan struct{})
			start := time.Now()
			go func() { defer close(done); link.run(ctx, from, to) }()
			t.Cleanup(func() { cancel(); <-done })
			for i := range 3 {
				bytes := MessagePoolGet(1000)
				bytes[0] = byte(i)
				from <- bytes
			}
			for i, want := range test.arrivals {
				bytes := <-to
				got := bytes[0]
				MessagePoolReturn(bytes)
				if got != byte(i) || time.Since(start) != want {
					t.Fatalf("frame=%d arrived=%s want=%s", got, time.Since(start), want)
				}
			}
			if link.maxQueued.Load() != 3 || link.maxQueuedBytes.Load() != 3000 || link.dropped.Load() != 0 {
				t.Fatal("rate change changed queue ownership")
			}
		})
	}
}

// The 1 Gb/s configured target must not be imposed on slower physical
// services. Allow the at-most-4-MiB discovery allowance to drain on the slowest
// links, then require sustained capacity with all startup drops retained.
func TestWindowPathServicePerformanceMatrix(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, cell := range windowServicePerformanceCells() {
		t.Logf("case: rate=%d/rtt=%s/flows=%d/compression=%s", cell.Rate, cell.RoundTrip, cell.Flows, cell.Compression)
		var ceiling, fixed windowPathReading
		for _, arm := range []string{"ceiling", "delivery"} {
			synctest.Test(t, func(t *testing.T) {
				trial := cell
				trial.Arm, trial.Drop = arm, arm == "delivery"
				reading := measureWindowPathCell(t, trial, max(time.Second, 2*cell.RoundTrip+4*cell.Compression))
				logWindowServiceReading(t, reading)
				if arm == "ceiling" {
					ceiling = reading
				} else {
					fixed = reading
				}
			})
		}
		t.Logf("service=%d rtt=%s flows=%d compression=%s ceiling=%.3f fixed=%.3f min-flow=%.3f model Mb/s drops=%d queue=%d/%d", cell.Rate, cell.RoundTrip, cell.Flows, cell.Compression, ceiling.Mbps, fixed.Mbps, fixed.MinFlowMbps, fixed.RelayDrops, fixed.MaxRelayQueued, fixed.MaxRelayQueuedBytes)
		if ceiling.Mbps < .90*float64(cell.Rate)*8/1e6 || fixed.Mbps < .90*ceiling.Mbps || fixed.MinFlowMbps == 0 || fixed.MeasurementRelayDrops != 0 || fixed.MaxRelayQueued > 4096 || fixed.MaxRelayQueuedBytes > int64(mib(8)) {
			t.Errorf("service pacing underfilled, stalled or exceeded a finite queue: %+v", fixed)
		}
	}
}

// An established busy path may gain or lose capacity. Compare the settled
// post-change interval to the same final service with a fixed large window;
// retain loss from the transition and enforce the queue's hard bounds.
func TestWindowPathServiceCapacityChanges(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, rates := range [][2]ByteCount{{1250000, 12500000}, {12500000, 1250000}} {
		for _, compression := range []time.Duration{0, 10 * time.Millisecond, 50 * time.Millisecond} {
			t.Logf("case: %s", fmt.Sprintf("rate=%d-to-%d/compression=%s", rates[0], rates[1], compression))
			var ceiling, fixed windowPathReading
			for _, arm := range []string{"ceiling", "delivery"} {
				synctest.Test(t, func(t *testing.T) {
					cell := windowPathCell{Arm: arm, RoundTrip: 100 * time.Millisecond, Compression: compression, Flows: 8, RoundRobinOffer: true, Payload: 1280, Budget: mib(48), Rate: rates[1], Warmup: 8 * time.Second}
					if arm == "delivery" {
						cell.Rate, cell.RateAfter, cell.RateChangeAfter, cell.Drop = rates[0], rates[1], 4*time.Second, true
					}
					reading := measureWindowPathCell(t, cell, time.Second)
					logWindowServiceReading(t, reading)
					if arm == "ceiling" {
						ceiling = reading
					} else {
						fixed = reading
					}
				})
			}
			t.Logf("capacity %d->%d compression=%s ceiling=%.3f fixed=%.3f min-flow=%.3f model Mb/s drops=%d queue=%d/%d", rates[0], rates[1], compression, ceiling.Mbps, fixed.Mbps, fixed.MinFlowMbps, fixed.RelayDrops, fixed.MaxRelayQueued, fixed.MaxRelayQueuedBytes)
			if fixed.Mbps < .90*ceiling.Mbps || fixed.MinFlowMbps == 0 || fixed.MeasurementRelayDrops != 0 || fixed.MaxRelayQueued > 4096 || fixed.MaxRelayQueuedBytes > int64(mib(8)) {
				t.Errorf("service change did not converge within four seconds: %+v", fixed)
			}
		}
	}
}

// TCP coalescing produces much larger physical messages than the original
// small-packet model. Cover serialization quanta larger than a service BDP,
// with enough measured messages to avoid one-packet sampling ambiguity.
func TestWindowPathServiceLargeMessages(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, rate := range []ByteCount{125000, 1250000, 12500000, 125000000} {
		for _, rtt := range []time.Duration{300 * time.Microsecond, 100 * time.Millisecond} {
			for _, flows := range []int{1, 8} {
				for _, payload := range []int{16 * 1024, 64 * 1024} {
					var ceiling, fixed windowPathReading
					warmup := max(300*time.Millisecond+5*rtt, time.Duration(2*int64(mib(2))*int64(time.Second)/int64(rate))+5*rtt)
					measurement := max(2*time.Second, time.Duration(64*int64(payload)*int64(time.Second)/int64(rate)))
					for _, arm := range []string{"ceiling", "delivery"} {
						synctest.Test(t, func(t *testing.T) {
							reading := measureWindowPathCell(t, windowPathCell{Arm: arm, RoundTrip: rtt, Compression: 10 * time.Millisecond,
								Flows: flows, RoundRobinOffer: true, Payload: payload, Budget: mib(48), Rate: rate, Drop: arm == "delivery", Warmup: warmup}, measurement)
							logWindowServiceReading(t, reading)
							if arm == "ceiling" {
								ceiling = reading
							} else {
								fixed = reading
							}
						})
					}
					if ceiling.Mbps < .9*float64(rate)*8/1e6 || fixed.Mbps < .9*ceiling.Mbps || fixed.MinFlowMbps == 0 || fixed.MeasurementRelayDrops != 0 {
						t.Errorf("large-message service rate=%d RTT=%s flows=%d payload=%d: ceiling=%.3f candidate=%.3f minimum=%.3f measured-drops=%d",
							rate, rtt, flows, payload, ceiling.Mbps, fixed.Mbps, fixed.MinFlowMbps, fixed.MeasurementRelayDrops)
					}
				}
			}
		}
	}
}

// Independent logical sequences compete for the same finite serializer and
// shared memory budget. An eight-flow test on one sequence cannot cover this.
func TestWindowPathServicesShareFiniteRelay(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, rate := range []ByteCount{125000, 12500000, 125000000} {
		t.Logf("case: %s", fmt.Sprintf("rate=%d", rate))
		var ceiling, fixed windowPathReading
		for _, arm := range []string{"ceiling", "delivery"} {
			synctest.Test(t, func(t *testing.T) {
				warmup := max(2*time.Second, time.Duration(2*4*int64(mib(2))*int64(time.Second)/int64(rate))+500*time.Millisecond)
				// Two BDPs per sequence calibrate shared service without
				// putting minutes of slow-link traffic into the control.
				calibration := max(kib(256), ByteCount(float64(rate)*.110*2/4))
				reading := measureWindowPathCell(t, windowPathCell{Arm: arm, CalibrationWindow: calibration, RoundTrip: 100 * time.Millisecond, Compression: 10 * time.Millisecond, Flows: 8, RoundRobinOffer: true, Lanes: 4, Payload: 1280, Budget: mib(48), Rate: rate, Drop: arm == "delivery", Warmup: warmup}, 2*time.Second)
				logWindowServiceReading(t, reading)
				if arm == "ceiling" {
					ceiling = reading
				} else {
					fixed = reading
				}
			})
		}
		t.Logf("shared service=%d ceiling=%.3f fixed=%.3f min-flow=%.3f model Mb/s drops=%d queue=%d/%d", rate, ceiling.Mbps, fixed.Mbps, fixed.MinFlowMbps, fixed.RelayDrops, fixed.MaxRelayQueued, fixed.MaxRelayQueuedBytes)
		if fixed.Mbps < .9*ceiling.Mbps || fixed.MinFlowMbps == 0 || fixed.MeasurementRelayDrops != 0 || fixed.MaxRelayQueued > 4096 || fixed.MaxRelayQueuedBytes > int64(mib(8)) {
			t.Errorf("shared service underfilled, starved or exceeded a finite queue: %+v", fixed)
		}
	}
}

// A busy path can change propagation delay without losing serializer
// capacity. Compare the settled candidate to an identical final path; an old
// minimum RTT must not turn all new propagation into a permanent queue.
func TestWindowPathServiceRoundTripChanges(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, rate := range []ByteCount{12500000, 125000000} {
		for _, roundTrips := range [][2]time.Duration{
			{300 * time.Microsecond, 100 * time.Millisecond},
			{100 * time.Millisecond, 300 * time.Microsecond},
		} {
			var ceiling, fixed windowPathReading
			for _, arm := range []string{"ceiling", "delivery"} {
				synctest.Test(t, func(t *testing.T) {
					cell := windowPathCell{Arm: arm, RoundTrip: roundTrips[1], Compression: 10 * time.Millisecond,
						Flows: 8, RoundRobinOffer: true, Payload: 1280, Budget: mib(48), Rate: rate, Warmup: 8 * time.Second}
					if arm == "delivery" {
						cell.RoundTrip, cell.RoundTripAfter, cell.RoundTripChangeAfter, cell.Drop = roundTrips[0], roundTrips[1], 4*time.Second, true
						cell.QualityChanged = true
					}
					reading := measureWindowPathCell(t, cell, time.Second)
					logWindowServiceReading(t, reading)
					if arm == "ceiling" {
						ceiling = reading
					} else {
						fixed = reading
					}
				})
			}
			if ceiling.Mbps < .9*float64(rate)*8/1e6 || fixed.Mbps < .9*ceiling.Mbps ||
				fixed.MinFlowMbps == 0 || fixed.MeasurementRelayDrops != 0 {
				t.Errorf("rate=%d RTT=%s->%s: ceiling=%.3f candidate=%.3f min-flow=%.3f drops=%d window=%+v",
					rate, roundTrips[0], roundTrips[1], ceiling.Mbps, fixed.Mbps, fixed.MinFlowMbps, fixed.MeasurementRelayDrops, fixed.Window)
			}
		}
	}
}

// Previously propagating frames keep their deadlines while new frames use
// the changed path. A shorter delay cannot reorder a reliable FIFO.
func TestWindowPathFifoPropagationChangePreservesOldFrames(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, delays := range [][2]time.Duration{
		{100 * time.Millisecond, 10 * time.Millisecond},
		{100 * time.Millisecond, 200 * time.Millisecond},
	} {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			from, to := make(Route), make(Route, 2)
			link := &windowPathLink{delay: delays[0], delayAfter: delays[1], delayChangeAfter: 50 * time.Millisecond,
				queueCount: 2, queueBytes: 100}
			done := make(chan struct{})
			go func() { defer close(done); link.run(ctx, from, to) }()
			defer func() { cancel(); <-done }()
			first, second := MessagePoolGet(1), MessagePoolGet(1)
			first[0], second[0] = 1, 2
			from <- first
			time.Sleep(50 * time.Millisecond)
			from <- second
			time.Sleep(49 * time.Millisecond)
			synctest.Wait()
			if len(to) != 0 {
				t.Fatal("a propagation change moved the old frame's deadline")
			}
			time.Sleep(time.Millisecond)
			synctest.Wait()
			if len(to) == 0 {
				t.Fatal("the old frame missed its original deadline")
			}
			frame := <-to
			value := frame[0]
			MessagePoolReturn(frame)
			if value != 1 {
				t.Fatal("a new frame overtook a propagating frame")
			}
			remaining := 50*time.Millisecond + delays[1] - delays[0]
			if remaining > 0 {
				if len(to) != 0 {
					t.Fatal("new frame did not use the longer propagation delay")
				}
				time.Sleep(remaining)
				synctest.Wait()
			}
			if len(to) != 1 {
				t.Fatal("the new frame missed its changed propagation deadline")
			}
			frame = <-to
			value = frame[0]
			MessagePoolReturn(frame)
			if value != 2 || link.dropped.Load() != 0 {
				t.Fatal("propagation transition changed packet identity or dropped it")
			}
		})
	}
}
