// The RTT-growth model's first partial feedback after a propagation gap
// cannot reprice all remaining physical writes as an almost stopped service.
package connect

import (
	"testing"
	"time"
)

// Replays actual ACK timestamps and byte deltas without a scheduler, probe,
// logging observer, or delayed application of old ACK bytes.
func TestWindowPacingPartialFeedbackGapPreservesEstablishedService(t *testing.T) {
	start := time.Unix(0, 0)
	service := &windowPacingService{
		minRoundTrip:    4519520 * time.Nanosecond,
		lastRoundTrip:   start.Add(4067185520 * time.Nanosecond),
		latestRoundTrip: 67125064 * time.Nanosecond,
		compression:     10 * time.Millisecond,
		bucketInterval:  10 * time.Millisecond,
		serviceEpochAt:  start.Add(160513600 * time.Nanosecond),
		total:           49860882, sent: 50502162,
		maxMessageByteCount: 2672,
		burstMeter:          windowPacingBurstMeter{limit: 125584},
	}
	for _, sample := range []struct {
		at    time.Duration
		bytes ByteCount
	}{
		{at: 4050363600 * time.Nanosecond, bytes: 122912},
		{at: 4060363600 * time.Nanosecond, bytes: 125584},
		{at: 4067185520 * time.Nanosecond, bytes: 74816},
	} {
		service.observe(sample.bytes, start.Add(sample.at))
	}
	prior, _, _ := service.measured(time.Second, start.Add(4067185520*time.Nanosecond))
	if prior < 11000000 {
		t.Fatalf("forced serialization train did not establish its measured service: %d", prior)
	}
	now := start.Add(4116249280 * time.Nanosecond)
	service.observeRoundTrip(116184088*time.Nanosecond, 10*time.Millisecond, now)
	if service.minRoundTrip != 4519520*time.Nanosecond {
		t.Fatalf("ordinary queued RTT changed the propagation floor: %s", service.minRoundTrip)
	}
	service.observe(2672, now)
	rate, _, latest := service.measured(time.Second, now)
	if got := max(rate, latest); got < prior*9/10 {
		t.Fatalf("one partial ACK across a propagation gap repriced established service: %d -> %d; pending probe=%t old-byte debt=%d", prior, got, service.roundTripProbe.resetService, service.roundTripProbe.pendingByteCount)
	}
}
