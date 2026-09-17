// A compulsory empty-flight probe must not interrupt a queue that measured
// physical repayment is already draining under the adaptive pacing rate.
package connect

import (
	"testing"
	"time"
)

// This is the 1.2s path-growth trace at the next probe opportunity: the old
// short-path cooldown is ending while a saturated, lossless service drains.
func newWindowDrainProgressFixture(at time.Time) (*windowPacingService, *windowPacingWaiter) {
	service := newWindowPacingService(DefaultSendBufferSettings())
	service.observeRoundTrip(1200*time.Millisecond, 10*time.Millisecond, at.Add(-time.Second))
	service.observeRoundTrip(1700*time.Millisecond, 10*time.Millisecond, at)
	service.sent, service.maxMessageByteCount = 30000000, 1000
	service.burstMeter.update(at, 125000, 11875000)
	service.burstEstimateTime = 10 * time.Millisecond
	sequenceId, messageId := NewId(), NewId()
	service.beginWrite(sequenceId, messageId, 0, at, false)
	service.finishWrite(sequenceId, messageId, true)
	service.drainCheckAt = at.Add(3 * time.Second)
	return service, &windowPacingWaiter{}
}

// Declining flight over a complete feedback turn is direct progress; a full
// stop would merely manufacture another RTT of idle at the physical receiver.
func TestWindowPacingNaturalQueueDrainDoesNotStopService(t *testing.T) {
	at := time.Unix(1700000000, 0)
	service, waiter := newWindowDrainProgressFixture(at)
	for i := range 3 {
		now := at.Add(time.Duration(i) * 1210 * time.Millisecond)
		service.total = ByteCount(i) * 2000000
		if wait, _ := service.admitBurst(now, 1000, false, waiter); wait > 0 {
			t.Fatalf("warmup unexpectedly paused at %s: %s", now.Sub(at), wait)
		}
	}
	if wait, _ := service.admitBurst(at.Add(3*time.Second), 1000, false, waiter); wait > 0 || !service.drainUntil.IsZero() {
		t.Fatalf("already declining flight triggered a full stop: wait=%s outstanding=%.0f", wait, service.outstandingWithLock())
	}
	// Stop repayment for one complete turn. That explicit loss of progress
	// restores the ordinary changed-path probe; grace cannot renew itself.
	if wait, _ := service.admitBurst(at.Add(3630*time.Millisecond), 1000, false, waiter); wait <= 0 || service.drainStartedAt != at.Add(3630*time.Millisecond) {
		t.Fatalf("stalled flight retained natural-drain grace: wait=%s started=%s", wait, service.drainStartedAt)
	}
}

// A burst-sized dip or growing flight is not proof of a sustained drain.
func TestWindowPacingBurstNoiseCannotSuppressDrainProbe(t *testing.T) {
	at := time.Unix(1700000000, 0)
	for _, repaid := range []ByteCount{-2000000, 125000} {
		service, waiter := newWindowDrainProgressFixture(at)
		for i := range 3 {
			if repaid < 0 {
				service.sent = 30000000 - ByteCount(i)*repaid
			} else {
				service.total = ByteCount(i) * repaid
			}
			service.admitBurst(at.Add(time.Duration(i)*1210*time.Millisecond), 1000, false, waiter)
		}
		if wait, _ := service.admitBurst(at.Add(3*time.Second), 1000, false, waiter); wait <= 0 {
			t.Fatalf("insufficient drain evidence bypassed probe: repaid=%d wait=%s", repaid, wait)
		}
	}
}
