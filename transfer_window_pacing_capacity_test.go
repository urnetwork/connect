// Established sparse senders must discover later capacity without mistaking
// every naturally empty flight for a requested measurement pause.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// Move the original capacity transition and its measurement forward by 36 s,
// so the slow opening train has drained before the change. Preserve the same
// post-change allowance, ninety-percent gate and sixty-four-message interval.
func TestWindowPathServiceSettledLargeMessageCapacityChanges(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, rates := range [][2]ByteCount{{125000, 1250000}, {1250000, 125000}} {
		for _, payload := range []int{16 * 1024, 64 * 1024} {
			var ceiling, candidate windowPathReading
			warmup := 40500*time.Millisecond + time.Duration(2*int64(mib(2))*int64(time.Second)/int64(min(rates[0], rates[1])))
			measurement := max(2*time.Second, time.Duration(64*int64(payload)*int64(time.Second)/int64(rates[1])))
			for _, arm := range []string{"ceiling", "delivery"} {
				synctest.Test(t, func(t *testing.T) {
					cell := windowPathCell{Arm: arm, RoundTrip: 100 * time.Millisecond, Compression: 10 * time.Millisecond,
						Flows: 8, RoundRobinOffer: true, Payload: payload, Budget: mib(48), Rate: rates[1], Warmup: warmup}
					if arm == "delivery" {
						cell.Rate, cell.RateAfter, cell.RateChangeAfter, cell.Drop = rates[0], rates[1], 40*time.Second, true
					}
					reading := measureWindowPathCell(t, cell, measurement)
					logWindowServiceReading(t, reading)
					if arm == "ceiling" {
						ceiling = reading
					} else {
						candidate = reading
					}
				})
			}
			if ceiling.Mbps < .9*float64(rates[1])*8/1e6 || candidate.Mbps < .9*ceiling.Mbps ||
				candidate.MinFlowMbps == 0 || candidate.MeasurementRelayDrops != 0 ||
				candidate.MaxRelayQueued > 4096 || candidate.MaxRelayQueuedBytes > int64(mib(8)) {
				t.Errorf("large-message rate=%d->%d payload=%d: reference=%.3f candidate=%.3f minimum=%.3f drops=%d queue=%d/%d",
					rates[0], rates[1], payload, ceiling.Mbps, candidate.Mbps, candidate.MinFlowMbps,
					candidate.MeasurementRelayDrops, candidate.MaxRelayQueued, candidate.MaxRelayQueuedBytes)
			}
		}
	}
}

// Retain the complete ramp after a settled slow sender gains capacity. The
// fixed four-second comparison starts ten seconds after the change, while
// three consecutive full intervals define the reported recovery time.
func TestWindowPathServiceCapacityIncreaseRecovery(t *testing.T) {
	assertMessagePoolOwnership(t)
	var ceiling, candidate windowPathReading
	for _, arm := range []string{"ceiling", "delivery"} {
		synctest.Test(t, func(t *testing.T) {
			cell := windowPathCell{Arm: arm, RoundTrip: 100 * time.Millisecond, Compression: 10 * time.Millisecond,
				Flows: 8, RoundRobinOffer: true, Payload: 64 * 1024, Budget: mib(48), Rate: 1250000, Warmup: 40 * time.Second}
			if arm == "delivery" {
				cell.Rate, cell.RateAfter, cell.RateChangeAfter, cell.Drop = 125000, 1250000, 40*time.Second, true
			}
			reading := measureWindowPathCell(t, cell, 20*time.Second)
			logWindowServiceReading(t, reading)
			if arm == "ceiling" {
				ceiling = reading
			} else {
				candidate = reading
			}
		})
	}
	if len(ceiling.IntervalMbps) != 20 || len(candidate.IntervalMbps) != 20 {
		t.Fatalf("transition intervals: reference=%d candidate=%d", len(ceiling.IntervalMbps), len(candidate.IntervalMbps))
	}
	recovery := time.Duration(0)
	consecutive := 0
	referenceMbps, candidateMbps := float64(0), float64(0)
	for i, rate := range candidate.IntervalMbps {
		if rate >= .9*ceiling.IntervalMbps[i] {
			consecutive++
			if recovery == 0 && consecutive == 3 {
				recovery = time.Duration(i+1) * time.Second
			}
		} else {
			consecutive = 0
		}
		if 10 <= i && i < 14 {
			referenceMbps += ceiling.IntervalMbps[i] / 4
			candidateMbps += rate / 4
		}
	}
	t.Logf("capacity recovery after change=%s reference-10-to-14s=%.6f candidate-10-to-14s=%.6f intervals=%v", recovery, referenceMbps, candidateMbps, candidate.IntervalMbps)
	if referenceMbps < 9 || candidateMbps < .9*referenceMbps || recovery == 0 || candidate.MinFlowMbps == 0 ||
		candidate.MeasurementRelayDrops != 0 || candidate.MaxRelayQueued > 4096 || candidate.MaxRelayQueuedBytes > int64(mib(8)) {
		t.Errorf("capacity recovery reference=%.3f candidate=%.3f recovered=%s minimum=%.3f drops=%d queue=%d/%d",
			referenceMbps, candidateMbps, recovery, candidate.MinFlowMbps, candidate.MeasurementRelayDrops,
			candidate.MaxRelayQueued, candidate.MaxRelayQueuedBytes)
	}
}

// A faster ACK is new service evidence even when its shorter RTT changes
// bucket widths. Keep the preceding checkpoint across that bookkeeping change.
func TestWindowPacingShorterRoundTripKeepsFasterServiceEvidence(t *testing.T) {
	start := time.Unix(1700000000, 0)
	service := &windowPacingService{}
	service.observeRoundTrip(625*time.Millisecond, 10*time.Millisecond, start)
	service.observe(65536, start)
	at := start.Add(524288 * time.Microsecond)
	service.observe(65536, at)
	if rate, _, _ := service.measured(time.Second, at); rate != 125000 {
		t.Fatalf("initial service=%d want=125000", rate)
	}
	at = at.Add(52428800 * time.Nanosecond)
	service.observeRoundTrip(152*time.Millisecond, 10*time.Millisecond, at)
	service.observe(65536, at)
	if rate, _, latest := service.measured(time.Second, at); rate != 1250000 {
		t.Fatalf("RTT resize discarded faster ACK pair: rate=%d latest=%d want=1250000", rate, latest)
	}
	if rate, _, latest := service.measured(time.Second, at.Add(time.Minute)); rate != 0 || latest != 1250000 {
		t.Fatalf("no new evidence changed the held service: rate=%d latest=%d", rate, latest)
	}
}
