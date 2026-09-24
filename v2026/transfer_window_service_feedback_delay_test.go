// Limited-flight continuity uses the receiver wait belonging to its current
// feedback. Older metadata remains useful for the network baseline only.
package connect

import (
	"testing"
	"time"
)

// A preoffered small flight keeps one physical tail outstanding. The last
// metadata tuple has no receiver wait, before a later slow feedback turn.
func newWindowFeedbackDelayFixture(t *testing.T) (*windowPacingService, time.Time) {
	t.Helper()
	service, start := newWindowQualifiedServiceFixture(t, 12500000, 50*time.Millisecond, 100*time.Millisecond)
	service.observeReceiverRoundTrip(0, 100*time.Millisecond, 100*time.Millisecond, 50*time.Millisecond, start)
	sequence, tail := NewId(), NewId()
	service.sent += 10000
	service.beginWrite(sequence, tail, 1, start, false)
	service.finishWrite(sequence, tail, true)
	return service, start
}

// A newer legacy turn must recover its advertised-delay fallback. Equal
// arrival timestamps still preserve the order of metadata and legacy ACKs.
func TestWindowPacingLimitedFlightLegacyTurnUsesOwnDelay(t *testing.T) {
	for _, sameArrival := range []bool{false, true} {
		service, start := newWindowFeedbackDelayFixture(t)
		for i := range 2 {
			at := start.Add(300*time.Millisecond + time.Duration(i)*50*time.Millisecond)
			if sameArrival {
				service.observeReceiverRoundTrip(0, 100*time.Millisecond, 100*time.Millisecond, 50*time.Millisecond, at)
			}
			service.observeRoundTrip(150*time.Millisecond, 50*time.Millisecond, at)
			service.observe(1000, at)
		}
		rate, _, latest := service.measured(time.Second, start.Add(350*time.Millisecond))
		if rate != 20000 || latest != 20000 {
			t.Fatalf("sameArrival=%t: old zero-delay metadata blocked slow legacy feedback: %d/%d", sameArrival, rate, latest)
		}
	}
}

// A real receiver hold can expand a later feedback turn even though the
// earlier metadata established that its own reply was immediate.
func TestWindowPacingLimitedFlightReceiverHeldTurnUsesOwnDelay(t *testing.T) {
	service, start := newWindowFeedbackDelayFixture(t)
	for i := range 2 {
		at := start.Add(300*time.Millisecond + time.Duration(i)*50*time.Millisecond)
		service.observeReceiverRoundTrip(0, 150*time.Millisecond, 100*time.Millisecond, 50*time.Millisecond, at)
		service.observe(1000, at)
	}
	rate, _, latest := service.measured(time.Second, start.Add(350*time.Millisecond))
	if rate != 20000 || latest != 20000 {
		t.Fatalf("the current receiver hold could not qualify slow feedback: %d/%d", rate, latest)
	}
}

// Byte accounting may follow a newer immediate reply. It must still use the
// paired wait at the older arrival, without inheriting future timing changes.
func TestWindowPacingLimitedFlightDelayedAccountingUsesArrivalDelay(t *testing.T) {
	for _, newerLegacy := range []bool{false, true} {
		service, start := newWindowFeedbackDelayFixture(t)
		for i := range 2 {
			at := start.Add(300*time.Millisecond + time.Duration(i)*50*time.Millisecond)
			service.observeReceiverRoundTrip(0, 150*time.Millisecond, 100*time.Millisecond, 50*time.Millisecond, at)
		}
		newer := start.Add(360 * time.Millisecond)
		if newerLegacy {
			service.observeRoundTrip(100*time.Millisecond, 50*time.Millisecond, newer)
		} else {
			service.observeReceiverRoundTrip(0, 100*time.Millisecond, 100*time.Millisecond, 50*time.Millisecond, newer)
		}
		for i := range 2 {
			service.observe(1000, start.Add(300*time.Millisecond+time.Duration(i)*50*time.Millisecond))
		}
		rate, _, latest := service.measured(time.Second, start.Add(350*time.Millisecond))
		if rate != 20000 || latest != 20000 {
			t.Fatalf("newerLegacy=%t: delayed accounting lost its receiver hold: %d/%d", newerLegacy, rate, latest)
		}
	}
}

// Conversely, newer paired timing at the same timestamp supersedes the
// legacy timer. An immediate reply cannot qualify the intervening silence.
func TestWindowPacingLimitedFlightMetadataAfterLegacyUsesPairedDelay(t *testing.T) {
	service, start := newWindowFeedbackDelayFixture(t)
	for i := range 3 {
		at := start.Add(300*time.Millisecond + time.Duration(i)*50*time.Millisecond)
		service.observeRoundTrip(150*time.Millisecond, 50*time.Millisecond, at)
		service.observeReceiverRoundTrip(0, 100*time.Millisecond, 100*time.Millisecond, 50*time.Millisecond, at)
		service.observe(1000, at)
	}
	rate, _, latest := service.measured(time.Second, start.Add(400*time.Millisecond))
	if max(rate, latest) != 12500000 {
		t.Fatalf("an older legacy timer overrode its newer paired reply: %d/%d", rate, latest)
	}
}
