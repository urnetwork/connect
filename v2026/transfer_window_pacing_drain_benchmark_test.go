// Isolate candidate-drain CPU cost from virtual-time goodput and network timing.
package connect

import (
	"testing"
	"time"
)

// One ambiguous pending tail is surrounded by completed tails, all enrolled
// through physical-write and ACK callbacks before timing starts. The rejected
// arm qualifies on every call; the typical arm uses the ordinary cooldown.
// Both replenish identical meter state locally to keep the actual admission
// branch constant. This is CPU evidence, not a physical throughput model.
func benchmarkWindowPacingDrainEligibility(b *testing.B, tails int, rejected bool) {
	at := time.Unix(1700000000, 0)
	service := &windowPacingService{
		drainMaximumTime:  time.Minute,
		writes:            make(map[Id]windowPacingWrite, tails),
		burstEstimateTime: 10 * time.Millisecond,
		burstMeter:        windowPacingBurstMeter{at: at, limit: 1000, rate: 1000000},
	}
	service.observeRoundTrip(time.Millisecond, 0, at.Add(-90*time.Millisecond))
	for ago := 8; ago > 0; ago-- {
		service.observeRoundTrip(20256*time.Millisecond, 0, at.Add(-time.Duration(ago)*10*time.Millisecond))
	}
	for i := range tails {
		sequence, message := NewId(), NewId()
		service.beginWrite(sequence, message, 1, at, i == 0)
		service.finishWrite(sequence, message, true)
		if i != 0 {
			service.acknowledgeWrite(sequence, message, 1, false, 0, at)
		}
	}
	mean, count := service.roundTripStats.ring.mean(at)
	if count == 0 || mean <= float64(3*time.Millisecond) || service.pendingWrites != 1 || service.canDrainWithLock() {
		b.Fatal("fixture did not create qualifying residence with an unprovable tail")
	}
	if !rejected {
		service.drainCheckAt = at.Add(time.Minute)
	}
	waiter := &windowPacingWaiter{deadline: at}
	var delay time.Duration
	var update <-chan struct{}
	b.ReportAllocs()
	for b.Loop() {
		service.burstMeter.available = 1000
		service.burstMeter.spent = 0
		delay, update = service.admitBurst(at, 1000, false, waiter)
	}
	if delay != 0 || update != nil || !service.drainUntil.IsZero() || service.drained {
		b.Fatal("admission invented drain evidence or changed the measured branch")
	}
}

// One live sequence exercises the smallest rejected candidate scan.
func BenchmarkWindowPacingDrainEligibilityReject1(b *testing.B) {
	benchmarkWindowPacingDrainEligibility(b, 1, true)
}

// Typical admission has the same map but no eligible controlled drain.
func BenchmarkWindowPacingDrainEligibilityTypical1(b *testing.B) {
	benchmarkWindowPacingDrainEligibility(b, 1, false)
}

// Thirty-two live entries cover several times the ordinary logical lane set.
func BenchmarkWindowPacingDrainEligibilityReject32(b *testing.B) {
	benchmarkWindowPacingDrainEligibility(b, 32, true)
}

// Control for thirty-two retained sequence entries.
func BenchmarkWindowPacingDrainEligibilityTypical32(b *testing.B) {
	benchmarkWindowPacingDrainEligibility(b, 32, false)
}

// Stress a large set of completed entries around one unprovable pending tail.
func BenchmarkWindowPacingDrainEligibilityReject512(b *testing.B) {
	benchmarkWindowPacingDrainEligibility(b, 512, true)
}

// Control for five hundred twelve retained sequence entries.
func BenchmarkWindowPacingDrainEligibilityTypical512(b *testing.B) {
	benchmarkWindowPacingDrainEligibility(b, 512, false)
}

// Largest declared rejected-candidate stress point.
func BenchmarkWindowPacingDrainEligibilityReject1024(b *testing.B) {
	benchmarkWindowPacingDrainEligibility(b, 1024, true)
}

// Control for one thousand twenty-four retained sequence entries.
func BenchmarkWindowPacingDrainEligibilityTypical1024(b *testing.B) {
	benchmarkWindowPacingDrainEligibility(b, 1024, false)
}
