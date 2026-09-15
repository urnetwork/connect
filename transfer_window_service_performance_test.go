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

// Opt-in startup trace complements the steady-state ledger without changing
// the ordinary matrix's measurement boundary.
func TestWindowPathPacingStartupTrace(t *testing.T) {
	if os.Getenv("CONNECT_WINDOW_PACING_TRACE") == "" {
		t.Skip("set CONNECT_WINDOW_PACING_TRACE=1")
	}
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		reading := measureWindowPathCell(t, windowPathCell{Arm: "delivery", RoundTrip: 400 * time.Millisecond, Compression: 10 * time.Millisecond, Flows: 1, RoundRobinOffer: true, Payload: 1280, Budget: mib(48), Rate: 125000000, Drop: true}, time.Second)
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
	for _, rate := range []ByteCount{125000, 1250000, 12500000} {
		for _, rtt := range []time.Duration{300 * time.Microsecond, 100 * time.Millisecond, 400 * time.Millisecond} {
			for _, flows := range []int{1, 8} {
				for _, compression := range []time.Duration{0, 10 * time.Millisecond, 50 * time.Millisecond} {
					t.Logf("case: %s", fmt.Sprintf("rate=%d/rtt=%s/flows=%d/compression=%s", rate, rtt, flows, compression))
					var ceiling, fixed windowPathReading
					warmup := max(300*time.Millisecond+5*rtt, time.Duration(2*int64(mib(2))*int64(time.Second)/int64(rate))+5*rtt)
					for _, arm := range []string{"ceiling", "delivery"} {
						synctest.Test(t, func(t *testing.T) {
							reading := measureWindowPathCell(t, windowPathCell{Arm: arm, RoundTrip: rtt, Compression: compression, Flows: flows, RoundRobinOffer: true, Payload: 1280, Budget: mib(48), Rate: rate, Drop: arm == "delivery", Warmup: warmup}, max(time.Second, 2*rtt+4*compression))
							logWindowServiceReading(t, reading)
							if arm == "ceiling" {
								ceiling = reading
							} else {
								fixed = reading
							}
						})
					}
					t.Logf("service=%d rtt=%s flows=%d compression=%s ceiling=%.3f fixed=%.3f min-flow=%.3f model Mb/s drops=%d queue=%d/%d", rate, rtt, flows, compression, ceiling.Mbps, fixed.Mbps, fixed.MinFlowMbps, fixed.RelayDrops, fixed.MaxRelayQueued, fixed.MaxRelayQueuedBytes)
					if ceiling.Mbps < .90*float64(rate)*8/1e6 || fixed.Mbps < .90*ceiling.Mbps || fixed.MinFlowMbps == 0 || fixed.MeasurementRelayDrops != 0 || fixed.MaxRelayQueued > 4096 || fixed.MaxRelayQueuedBytes > int64(mib(8)) {
						t.Errorf("service pacing underfilled, stalled or exceeded a finite queue: %+v", fixed)
					}
				}
			}
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
