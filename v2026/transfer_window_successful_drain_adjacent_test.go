// Controlled measurement pauses retain qualified service through partial
// feedback, while physical completion or bounded abandonment permits progress.
package connect

import (
	"testing"
	"time"
)

// Exact physical tails and a real pacing reservation expose the transitions
// between drain proof, admission and the resumed carrier handoff.
type windowDrainFeedbackFixture struct {
	service   *windowPacingService
	start     time.Time
	partialAt time.Time
	flight    ByteCount
	tails     [8]struct{ sequenceId, messageId Id }
	waiter    windowPacingWaiter
}

// The first partial turn lasts exactly one compression interval. Every
// physical tail remains unproved when the returned fixture may be read.
func newWindowDrainFeedbackFixture(t *testing.T, metadata bool, flight ByteCount) *windowDrainFeedbackFixture {
	t.Helper()
	fixture := &windowDrainFeedbackFixture{service: newWindowPacingService(DefaultSendBufferSettings()), start: time.Unix(1700000000, 0), flight: flight}
	service, start := fixture.service, fixture.start
	if metadata {
		service.observeReceiverRoundTrip(0, 10300*time.Microsecond, 300*time.Microsecond, 10*time.Millisecond, start)
	} else {
		service.observeRoundTrip(10300*time.Microsecond, 10*time.Millisecond, start)
	}
	service.observe(2673, start)
	service.observe(125000, start.Add(10*time.Millisecond))
	if rate, _, latest := service.measured(time.Second, start.Add(10*time.Millisecond)); max(rate, latest) != 12500000 {
		t.Fatal("the physical opening did not establish its serialization rate")
	}
	service.sent = service.total + flight
	for i := range fixture.tails {
		fixture.tails[i].sequenceId, fixture.tails[i].messageId = NewId(), NewId()
		item := fixture.tails[i]
		service.beginWrite(item.sequenceId, item.messageId, 2, start.Add(10*time.Millisecond), false)
		service.finishWrite(item.sequenceId, item.messageId, true)
	}
	if !metadata {
		service.observeRoundTrip(1218914705*time.Nanosecond, 10*time.Millisecond, start.Add(580*time.Millisecond))
	}
	for i, item := range fixture.tails {
		at := start.Add(601170080*time.Nanosecond + time.Duration(i)*213840*time.Nanosecond)
		service.acknowledgeWrite(item.sequenceId, NewId(), 1, false, 10*time.Millisecond, at)
		service.observe(2673, at)
		if metadata {
			service.observeReceiverRoundTrip(0, 1218914705*time.Nanosecond, 1208914705*time.Nanosecond, 10*time.Millisecond, at)
		} else {
			service.observeRoundTrip(1218914705*time.Nanosecond, 10*time.Millisecond, at)
		}
	}
	at := start.Add(602666960 * time.Nanosecond)
	service.reserve(at, 2673, 12500000, 12500000, 0, 0, false, &fixture.waiter)
	if delay, _ := service.admitBurst(at, 2673, false, &fixture.waiter); delay <= 0 || !service.drainServiceEpoch || service.pendingWrites != len(fixture.tails) {
		t.Fatal("the physical flight did not enter a controlled drain")
	}
	fixture.partialAt = start.Add(611170080 * time.Nanosecond)
	service.acknowledgeWrite(fixture.tails[0].sequenceId, NewId(), 1, false, 10*time.Millisecond, fixture.partialAt)
	service.observe(16038, fixture.partialAt)
	return fixture
}

// Accounting is applied only after each corresponding exact tail proves
// physical delivery, matching the original worker root's complete flight.
func (self *windowDrainFeedbackFixture) complete(at time.Time) {
	remaining := self.flight - 8*2673 - 16038
	for i, item := range self.tails {
		self.service.acknowledgeWrite(item.sequenceId, item.messageId, 2, false, 10*time.Millisecond, at)
		bytes := remaining / ByteCount(len(self.tails)-i)
		self.service.observe(bytes, at)
		remaining -= bytes
	}
}

// Complete the reserved physical handoff after a deliberately separate
// admission call, so a test can place a controller read between those phases.
func (self *windowDrainFeedbackFixture) dispatch(sequenceId, messageId Id, at time.Time) {
	self.service.stateLock.Lock()
	self.service.beginWriteWithLock(sequenceId, messageId, 1, at, false)
	self.service.removeWaiterWithLock(&self.waiter)
	self.service.pacingReservations--
	self.service.reservedByteCount -= 2673
	self.service.stateLock.Unlock()
	self.service.finishWrite(sequenceId, messageId, true)
}

// A complete physical drain can precede both the waiting owner's admission
// and its write registration. Neither intervening read may lose the hold.
func TestWindowPacingSuccessfulDrainReadBetweenProofAndDispatch(t *testing.T) {
	for _, metadata := range []bool{false, true} {
		fixture := newWindowDrainFeedbackFixture(t, metadata, 2131032)
		service := fixture.service
		at := fixture.start.Add(1210300 * time.Microsecond)
		fixture.complete(at)
		if !service.drained {
			t.Fatal("the exact complete physical flight did not drain")
		}
		for _, admitted := range []bool{false, true} {
			if admitted {
				if delay, _ := service.admitBurst(at, 2673, false, &fixture.waiter); delay != 0 {
					t.Fatal("the proved drain did not admit its reserved write")
				}
			}
			if rate, total, latest := service.measured(time.Second, at); max(rate, latest) != 12500000 || total != 2258705 {
				t.Errorf("metadata=%t admitted=%t: delivery-to-dispatch gap repriced service=%d/%d total=%d", metadata, admitted, rate, latest, total)
			}
		}
	}
}

// Expiry has its original absolute bound even if the waiting owner has not
// processed the timer. Fresh slow replies then replace the previous hold.
func TestWindowPacingSuccessfulDrainTimeoutAcceptsSlowService(t *testing.T) {
	for _, metadata := range []bool{false, true} {
		fixture := newWindowDrainFeedbackFixture(t, metadata, 2131032)
		service := fixture.service
		deadline := service.drainUntil
		service.observe(1000, deadline.Add(-100*time.Millisecond))
		service.observe(1000, deadline)
		if rate, _, latest := service.measured(time.Second, deadline); max(rate, latest) != 10000 {
			t.Errorf("metadata=%t: expired drain retained old service over a genuine slow pair: %d/%d", metadata, rate, latest)
		}
		if delay, _ := service.admitBurst(deadline, 2673, false, &fixture.waiter); delay != 0 {
			t.Fatal("the configured drain deadline did not release admission")
		}
	}
}

// Invalidating physical proof ends the measurement pause immediately. It
// cannot become a permanent service floor on a truly slower serializer.
func TestWindowPacingSuccessfulDrainAbandonmentAcceptsSlowService(t *testing.T) {
	for _, outcome := range []string{"retry", "carrier-change", "failed-write", "cancel"} {
		fixture := newWindowDrainFeedbackFixture(t, true, 2131032)
		service := fixture.service
		item := fixture.tails[0]
		switch outcome {
		case "retry":
			service.beginWrite(item.sequenceId, item.messageId, 2, fixture.partialAt, true)
		case "carrier-change":
			service.invalidateProbe(item.sequenceId)
		case "failed-write":
			service.finishWrite(item.sequenceId, item.messageId, false)
		case "cancel":
			pacer := &windowBurstPacer{service: service, serviceSequenceId: item.sequenceId}
			pacer.close()
		}
		if !service.drainUntil.IsZero() {
			t.Fatalf("%s: lost physical proof did not abandon the pause", outcome)
		}
		service.observe(1000, fixture.partialAt.Add(100*time.Millisecond))
		at := fixture.partialAt.Add(200 * time.Millisecond)
		service.observe(1000, at)
		if rate, _, latest := service.measured(time.Second, at); max(rate, latest) != 10000 {
			t.Errorf("%s: abandoned drain suppressed genuine slow service: %d/%d", outcome, rate, latest)
		}
	}
}

// A partial prefix may already prove a faster lower bound across its whole
// actual gap. Holding an incomplete drain must not hide those delivered bytes.
func TestWindowPacingSuccessfulDrainPartialDeliveryCanRaiseService(t *testing.T) {
	fixture := newWindowDrainFeedbackFixture(t, true, 48000000)
	service := fixture.service
	at := fixture.start.Add(700 * time.Millisecond)
	service.observe(32000000, at)
	want := ByteCount(float64(32000000) / at.Sub(fixture.partialAt).Seconds())
	if rate, total, latest := service.measured(time.Second, at); max(rate, latest) != want || total != 32165095 || service.pendingWrites != 8 {
		t.Fatalf("new partial delivery lost its faster lower bound: service=%d/%d total=%d want=%d", rate, latest, total, want)
	}
}

// Confirmed propagation retires the paused interval. A fresh slower pair
// must replace the hold immediately using its own physical delivery times.
func TestWindowPacingSuccessfulDrainConfirmedProbeAcceptsSlowService(t *testing.T) {
	fixture := newWindowDrainFeedbackFixture(t, true, 2131032)
	service := fixture.service
	service.measured(time.Second, fixture.partialAt)
	at := fixture.start.Add(1210300 * time.Microsecond)
	fixture.complete(at)
	if delay, _ := service.admitBurst(at, 2673, false, &fixture.waiter); delay != 0 {
		t.Fatal("the complete drain did not release the reserved probe")
	}
	sequenceId, messageId := NewId(), NewId()
	fixture.dispatch(sequenceId, messageId, at)
	at = at.Add(1200 * time.Millisecond)
	service.observeReceiverRoundTripForWrite(sequenceId, messageId, 0, 1200*time.Millisecond, 1200*time.Millisecond, 10*time.Millisecond, at)
	service.acknowledgeWrite(sequenceId, messageId, 1, false, 10*time.Millisecond, at)
	service.observe(2673, at)
	service.measured(time.Second, at)
	messageId = NewId()
	service.sent += 1000
	service.beginWrite(sequenceId, messageId, 2, at, false)
	service.finishWrite(sequenceId, messageId, true)
	at = at.Add(100 * time.Millisecond)
	service.acknowledgeWrite(sequenceId, messageId, 2, false, 10*time.Millisecond, at)
	service.observe(1000, at)
	if rate, total, latest := service.measured(time.Second, at); max(rate, latest) != 10000 || total != 2262378 {
		t.Fatalf("confirmed probe retained its former hold over fresh slow service: %d/%d total=%d", rate, latest, total)
	}
}

// A completed cycle can outlive the read that retired it. Starting a later
// drain must hold the accepted newer service, not restore that old summary.
func TestWindowPacingSuccessfulDrainCannotRestoreRetiredCycle(t *testing.T) {
	service, start := newPartialFeedbackCycleFixture(t)
	service.observe(1000, start.Add(100*time.Millisecond))
	service.observe(9000, start.Add(110*time.Millisecond))
	for offset := 120 * time.Millisecond; offset <= 160*time.Millisecond; offset += 10 * time.Millisecond {
		service.observe(1000, start.Add(offset))
	}
	if rate, _, latest := service.measured(time.Nanosecond, start.Add(160*time.Millisecond)); max(rate, latest) != 100000 {
		t.Fatalf("the newer slower train did not replace the old cycle: %d/%d", rate, latest)
	}
	sequenceId, messageId := NewId(), NewId()
	service.beginWrite(sequenceId, messageId, 1, start.Add(160*time.Millisecond), false)
	service.finishWrite(sequenceId, messageId, true)
	service.observeRoundTrip(500*time.Millisecond, 10*time.Millisecond, start.Add(180*time.Millisecond))
	at := start.Add(200 * time.Millisecond)
	if delay, _ := service.admitBurst(at, 1000, false, &windowPacingWaiter{}); delay <= 0 {
		t.Fatal("the later physical flight did not enter its controlled drain")
	}
	if rate, _, latest := service.measured(time.Nanosecond, at); max(rate, latest) != 100000 {
		t.Fatalf("later drain restored a retired faster cycle: %d/%d, want 100000", rate, latest)
	}
}
