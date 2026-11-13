// The reverse direction can delay one acknowledgement behind an ordinary
// permitted data burst. That bounded residence alone cannot reprice a healthy
// forward service while its physical flight is below the backlog bound.
package connect

import (
	"testing"
	"time"
)

// A 1.25 MB burst takes 10 ms at the independently specified 125 MB/s link.
// Its reverse ACK delay fits the existing 20 ms maximum burst duration. The
// corrected receiver tuple removes only the receiver's separate 10 ms wait.
func TestWindowPacingOnePermittedBurstKeepsHeldRate(t *testing.T) {
	service := newWindowPacingService(DefaultSendBufferSettings())
	start := time.Unix(1700000000, 0)
	service.burstEstimateTime = 10 * time.Millisecond
	service.observeReceiverRoundTrip(0, 10300*time.Microsecond, 300*time.Microsecond, 10*time.Millisecond, start)
	service.holdPacing(137500000)
	at := start.Add(30 * time.Millisecond)
	service.observeReceiverRoundTrip(0, 20300*time.Microsecond, 10300*time.Microsecond, 10*time.Millisecond, at)
	if service.backloggedAt(125000000, at) {
		t.Fatal("one bounded reverse burst invented physical forward backlog")
	}
	discovering, held := service.pacingHold()
	if discovering {
		t.Fatal("the queued reply failed to end blind discovery")
	}
	estimate := SendWindowEstimate{
		WindowRoundTrip: 10300 * time.Microsecond, Initial: 524288,
		ServiceByteRate: 101108827, ServiceEstablished: true,
		PacingHeldByteRate: held,
	}
	if rate := windowPacingRate(estimate, 147928994); rate != 137500000 {
		t.Fatalf("a permitted reverse burst repriced healthy forward service: held=%d rate=%d", held, rate)
	}
}

// An observed queue that outlasts the allowed burst duration must still
// release the held pace, even if cumulative delivery has drained the flight.
func TestWindowPacingQueueBeyondBurstReleasesHeldRate(t *testing.T) {
	service := newWindowPacingService(DefaultSendBufferSettings())
	start := time.Unix(1700000000, 0)
	service.burstEstimateTime = 10 * time.Millisecond
	service.observeReceiverRoundTrip(0, 10300*time.Microsecond, 300*time.Microsecond, 10*time.Millisecond, start)
	service.holdPacing(137500000)
	service.observeReceiverRoundTrip(0, 35300*time.Microsecond, 25300*time.Microsecond, 10*time.Millisecond, start.Add(40*time.Millisecond))
	if discovering, held := service.pacingHold(); discovering || held != 0 {
		t.Fatalf("a queue beyond the burst bound retained pacing: discovery=%t held=%d", discovering, held)
	}
}

// A bounded reverse delay does not excuse excess forward flight. Three
// megabytes outstanding exceeds the independent 1.2875 MB residence plus
// 1.25 MB burst allowance; admission must lower and replace the held pace.
func TestWindowPacingPermittedBurstDoesNotMaskForwardBacklog(t *testing.T) {
	service := newWindowPacingService(DefaultSendBufferSettings())
	start := time.Unix(1700000000, 0)
	service.burstEstimateTime = 10 * time.Millisecond
	service.burstMeter.limit = 1250000
	service.sent, service.total = 5000000, 2000000
	service.observeReceiverRoundTrip(0, 10300*time.Microsecond, 300*time.Microsecond, 10*time.Millisecond, start)
	service.holdPacing(137500000)
	at := start.Add(30 * time.Millisecond)
	service.observeReceiverRoundTrip(0, 20300*time.Microsecond, 10300*time.Microsecond, 10*time.Millisecond, at)
	backlogged := service.backloggedAt(125000000, at)
	if !backlogged {
		t.Fatal("a permitted reverse burst hid excess forward flight")
	}
	_, held := service.pacingHold()
	estimate := SendWindowEstimate{
		WindowRoundTrip: 10300 * time.Microsecond, Initial: 524288,
		ServiceByteRate: 101108827, ServiceEstablished: true,
		ServiceBacklogged: backlogged, PacingHeldByteRate: held,
	}
	rate := windowPacingRate(estimate, 147928994)
	if rate != 96053385 {
		t.Fatalf("congested admission did not leave drain capacity: held=%d rate=%d", held, rate)
	}
	service.holdPacing(rate)
	if _, held := service.pacingHold(); held != rate {
		t.Fatalf("congested admission retained the old pace: held=%d want=%d", held, rate)
	}
}
