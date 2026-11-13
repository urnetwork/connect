// Cold-start sampling follows confirmed physical train boundaries even when
// byte accounting, sibling replies and write confirmation arrive separately.
package connect

import (
	"testing"
	"time"
)

// Physical handoffs and their exact byte accounting are forced independently;
// the companion sdk fixture verifies the same boundary through the real pacer.
type windowSdkColdProbeFixture struct {
	service    *windowPacingService
	sequenceId Id
	probeId    Id
	sentAt     time.Time
	ackedAt    time.Time
}

// One cumulative opening reply drains the confirmed carrier before the next
// physical write. Its byte accounting may remain pending in another worker.
func newWindowSdkColdProbeFixture(applyOpening bool) *windowSdkColdProbeFixture {
	start := time.Unix(1700000000, 0)
	fixture := &windowSdkColdProbeFixture{
		service: &windowPacingService{sent: 328656}, sequenceId: NewId(), probeId: NewId(),
		sentAt: start.Add(405 * time.Millisecond), ackedAt: start.Add(805*time.Millisecond + 11072*time.Nanosecond),
	}
	tail := NewId()
	fixture.service.beginWrite(fixture.sequenceId, tail, 122, start, false)
	fixture.service.finishWrite(fixture.sequenceId, tail, true)
	fixture.service.observeRoundTrip(405*time.Millisecond, 10*time.Millisecond, fixture.sentAt)
	fixture.service.acknowledgeWrite(fixture.sequenceId, tail, 122, false, 10*time.Millisecond, fixture.sentAt)
	if applyOpening {
		fixture.service.observe(328656, fixture.sentAt)
	}
	fixture.service.sent += 1384
	fixture.service.beginWrite(fixture.sequenceId, fixture.probeId, 123, fixture.sentAt, false)
	return fixture
}

// An early reply is provisional until the carrier confirms it. Failed
// confirmation restores the original samples, including genuine slow service.
func TestWindowPacingSdkInitialAckBeforeWriteConfirmation(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		fixture := newWindowSdkColdProbeFixture(true)
		service := fixture.service
		service.acknowledgeWrite(fixture.sequenceId, fixture.probeId, 123, false, 10*time.Millisecond, fixture.ackedAt)
		service.observe(1384, fixture.ackedAt)
		if rate, total, latest := service.measured(time.Second, fixture.ackedAt); max(rate, latest) != 0 || total != 330040 {
			t.Errorf("confirmed=%t: provisional cold reply established service=%d/%d total=%d", confirmed, rate, latest, total)
		}
		service.finishWrite(fixture.sequenceId, fixture.probeId, confirmed)
		want := ByteCount(0)
		if !confirmed {
			want = ByteCount(float64(1384) / fixture.ackedAt.Sub(fixture.sentAt).Seconds())
		}
		if rate, total, latest := service.measured(time.Second, fixture.ackedAt); max(rate, latest) != want || total != 330040 {
			t.Errorf("confirmed=%t: carrier result left service=%d/%d total=%d want=%d", confirmed, rate, latest, total, want)
		}
	}
}

// All prior physical bytes can be acknowledged before their owner applies
// any samples. Applying those older bytes later cannot bridge the new train.
func TestWindowPacingSdkInitialDelayedOpeningAccounting(t *testing.T) {
	for _, openingFirst := range []bool{false, true} {
		fixture := newWindowSdkColdProbeFixture(false)
		service := fixture.service
		service.finishWrite(fixture.sequenceId, fixture.probeId, true)
		if deadline := service.probeRecoveryDeadline(fixture.sequenceId, fixture.probeId, 2, time.Second); !deadline.IsZero() {
			t.Fatal("a natural cold drain changed the controlled-probe recovery deadline")
		}
		if openingFirst {
			service.observe(328656, fixture.sentAt)
		}
		service.acknowledgeWrite(fixture.sequenceId, fixture.probeId, 123, false, 10*time.Millisecond, fixture.ackedAt)
		service.observe(1384, fixture.ackedAt)
		if !openingFirst {
			service.observe(328656, fixture.sentAt)
		}
		if rate, total, latest := service.measured(time.Second, fixture.ackedAt); max(rate, latest) != 0 || total != 330040 {
			t.Errorf("opening-first=%t: delayed accounting crossed the drain: service=%d/%d total=%d", openingFirst, rate, latest, total)
		}
	}
}

// A sibling can return the first resumed checkpoint before the tracked probe.
// Two checkpoints on that fresh side still discover a real slow rate.
func TestWindowPacingSdkInitialSiblingFeedbackStartsFreshTrain(t *testing.T) {
	fixture := newWindowSdkColdProbeFixture(true)
	service := fixture.service
	service.finishWrite(fixture.sequenceId, fixture.probeId, true)
	sequenceId, messageId := NewId(), NewId()
	service.sent += 1000
	service.beginWrite(sequenceId, messageId, 1, fixture.sentAt, false)
	service.finishWrite(sequenceId, messageId, true)
	service.acknowledgeWrite(sequenceId, messageId, 1, false, 10*time.Millisecond, fixture.ackedAt)
	service.observe(1000, fixture.ackedAt)
	if rate, _, latest := service.measured(time.Second, fixture.ackedAt); max(rate, latest) != 0 {
		t.Errorf("one resumed sibling reply crossed the cold drain: service=%d/%d", rate, latest)
	}
	messageId = NewId()
	service.sent += 2673
	service.beginWrite(sequenceId, messageId, 2, fixture.ackedAt, false)
	service.finishWrite(sequenceId, messageId, true)
	at := fixture.ackedAt.Add(100 * time.Millisecond)
	service.acknowledgeWrite(sequenceId, messageId, 2, false, 10*time.Millisecond, at)
	service.observe(2673, at)
	if rate, _, latest := service.measured(time.Second, at); max(rate, latest) != 26730 {
		t.Errorf("fresh sibling pair did not discover slow service: %d/%d", rate, latest)
	}
	at = at.Add(100 * time.Millisecond)
	service.acknowledgeWrite(fixture.sequenceId, fixture.probeId, 123, false, 10*time.Millisecond, at)
	service.observe(1384, at)
	if rate, total, latest := service.measured(time.Second, at); max(rate, latest) != 26730 || total != 333713 {
		t.Errorf("later probe confirmation lost fresh sibling service: %d/%d total=%d", rate, latest, total)
	}
}

// Existing serialization remains valid when the controller has not read it;
// an observational statistics read must not decide whether service is cold.
func TestWindowPacingSdkInitialUnreadPairPreservesService(t *testing.T) {
	for _, readStatistics := range []bool{false, true} {
		start := time.Unix(1700000000, 0)
		service := &windowPacingService{sent: 200000}
		sequenceId, first, tail, resumed := NewId(), NewId(), NewId(), NewId()
		service.observeRoundTrip(100*time.Millisecond, 10*time.Millisecond, start)
		service.beginWrite(sequenceId, first, 1, start, false)
		service.finishWrite(sequenceId, first, true)
		service.beginWrite(sequenceId, tail, 2, start, false)
		service.finishWrite(sequenceId, tail, true)
		service.acknowledgeWrite(sequenceId, first, 1, false, 10*time.Millisecond, start)
		service.observe(100000, start)
		at := start.Add(100 * time.Millisecond)
		service.acknowledgeWrite(sequenceId, tail, 2, false, 10*time.Millisecond, at)
		service.observe(100000, at)
		if readStatistics {
			if rate, _, _ := service.measure(time.Second, at, false); rate != 1000000 {
				t.Fatalf("opening pair did not measure its physical service: %d", rate)
			}
		}
		service.sent += 100000
		service.beginWrite(sequenceId, resumed, 3, at, false)
		service.finishWrite(sequenceId, resumed, true)
		at = at.Add(50 * time.Millisecond)
		service.acknowledgeWrite(sequenceId, resumed, 3, false, 10*time.Millisecond, at)
		service.observe(100000, at)
		if rate, total, latest := service.measured(time.Second, at); max(rate, latest) != 2000000 || total != 300000 {
			t.Errorf("statistics=%t: unread old pair was treated as cold: service=%d/%d total=%d", readStatistics, rate, latest, total)
		}
	}
}

// Late accounting can reveal a valid pair entirely before the drained
// boundary. Its genuine rate remains available across the isolated reply.
func TestWindowPacingSdkInitialLateOpeningPairRemainsEvidence(t *testing.T) {
	for _, readBeforeReply := range []bool{false, true} {
		fixture := newWindowSdkColdProbeFixture(false)
		service := fixture.service
		service.finishWrite(fixture.sequenceId, fixture.probeId, true)
		service.observe(164328, fixture.sentAt.Add(-100*time.Millisecond))
		service.observe(164328, fixture.sentAt)
		if readBeforeReply {
			if rate, _, latest := service.measured(time.Second, fixture.sentAt); max(rate, latest) != 1643280 {
				t.Errorf("late opening pair lost its original timestamps: %d/%d", rate, latest)
			}
		}
		service.acknowledgeWrite(fixture.sequenceId, fixture.probeId, 123, false, 10*time.Millisecond, fixture.ackedAt)
		service.observe(1384, fixture.ackedAt)
		if rate, total, latest := service.measured(time.Second, fixture.ackedAt); max(rate, latest) != 1643280 || total != 330040 {
			t.Errorf("controller-read=%t: cold reply lost valid late opening service: %d/%d total=%d", readBeforeReply, rate, latest, total)
		}
	}
}
