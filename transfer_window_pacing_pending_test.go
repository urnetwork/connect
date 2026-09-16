// Controlled pauses retain service while their first resumed ACK is pending.
package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

// Old coalesced ACKs may be applied only after a drain releases its first
// probe. They cannot price a sibling at the local pause's apparent rate.
func TestWindowPacingPendingControlledProbeKeepsSiblingRate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		service := &windowPacingService{sent: 37500}
		sequenceId, tail, probe := NewId(), NewId(), NewId()
		otherSequenceId, otherTail := NewId(), NewId()
		service.observeRoundTrip(100*time.Millisecond, 10*time.Millisecond, start)
		service.beginWrite(sequenceId, tail, 1, start, false)
		service.finishWrite(sequenceId, tail, true)
		service.beginWrite(otherSequenceId, otherTail, 1, start, false)
		service.finishWrite(otherSequenceId, otherTail, true)
		service.observe(2500, start)
		time.Sleep(20 * time.Millisecond)
		service.observe(2500, time.Now())
		if rate, _, _ := service.measured(time.Second, time.Now()); rate != 125000 {
			t.Fatalf("initial service=%d", rate)
		}
		time.Sleep(20 * time.Millisecond)
		service.observeRoundTrip(500*time.Millisecond, 10*time.Millisecond, time.Now())
		probePacer := &windowBurstPacer{service: service, serviceSequenceId: sequenceId, rate: 137500, estimateRate: 125000}
		defer probePacer.close()
		ctx, cancel := context.WithCancel(context.Background())
		probeDone := make(chan error, 1)
		go func() {
			defer close(probeDone)
			probeDone <- probePacer.waitForServiceMessage(ctx, 2500, false, sequenceId, probe, 2)
		}()
		defer func() {
			cancel()
			for range probeDone {
			}
		}()
		synctest.Wait()
		time.Sleep(60 * time.Millisecond)
		synctest.Wait()
		if service.drainUntil.IsZero() {
			t.Fatal("controlled drain did not start")
		}
		time.Sleep(180 * time.Millisecond)
		service.acknowledgeWrite(sequenceId, tail, 1, false, 10*time.Millisecond, time.Now())
		synctest.Wait()
		if service.pendingWrites != 1 {
			t.Fatal("the sibling tail did not keep the probe in its drain")
		}
		time.Sleep(20 * time.Millisecond)
		service.acknowledgeWrite(otherSequenceId, otherTail, 1, false, 10*time.Millisecond, time.Now())
		if err := <-probeDone; err != nil {
			t.Fatal(err)
		}
		service.finishWrite(sequenceId, probe, true)
		// Another sequence's old cumulative head was received before the probe
		// started, but its send worker applies the coalesced bytes afterward.
		service.observe(12500, start.Add(280*time.Millisecond))
		rate, _, latest := service.measured(time.Second, time.Now())
		effective := rate
		if effective == 0 {
			effective = latest
		}
		sibling := &windowBurstPacer{service: service, serviceSequenceId: NewId(), estimateRate: effective,
			rate: windowPacingRate(SendWindowEstimate{ServiceByteRate: effective, ServiceEstablished: true}, 125000000)}
		defer sibling.close()
		before := time.Now()
		if err := sibling.waitForServiceMessage(context.Background(), 2500, false, sibling.serviceSequenceId, NewId(), 1); err != nil {
			t.Fatal(err)
		}
		elapsed := time.Since(before)
		// The actual byte meter rounds serialization up to a whole nanosecond.
		want := time.Duration((2500*int64(time.Second) + 137500 - 1) / 137500)
		if effective != 125000 || elapsed != want {
			t.Errorf("pending probe repriced its sibling from old ACKs: service=%d release=%s want service=125000 release=%s", effective, elapsed, want)
		}
		if !service.serviceEpochAt.IsZero() {
			t.Fatal("unacknowledged probe committed a service epoch")
		}
		time.Sleep(start.Add(400 * time.Millisecond).Sub(time.Now()))
		service.acknowledgeWrite(sequenceId, probe, 2, false, 10*time.Millisecond, time.Now())
		service.observe(2500, time.Now())
		if rate, _, latest := service.measured(time.Second, time.Now()); max(rate, latest) != 125000 {
			t.Errorf("probe confirmation lost the service hold: %d/%d", rate, latest)
		}
		time.Sleep(20 * time.Millisecond)
		service.observe(1000, time.Now())
		if rate, _, _ := service.measured(time.Second, time.Now()); rate != 50000 {
			t.Errorf("fresh slower evidence did not replace the hold: %d", rate)
		}
	})
}

// A provisional hold leaves the old sample epoch available when the probe
// fails, becomes ambiguous or is canceled; no missing reply commits an epoch.
func TestWindowPacingPendingProbeInvalidationRestoresSamples(t *testing.T) {
	for _, outcome := range []string{"failed-write", "retry", "carrier-change", "cancel", "missing-reply", "natural"} {
		start := time.Unix(1700000000, 0)
		service := &windowPacingService{sent: 37500}
		sequenceId, tail, probe := NewId(), NewId(), NewId()
		service.observeRoundTrip(100*time.Millisecond, 10*time.Millisecond, start)
		service.beginWrite(sequenceId, tail, 1, start, false)
		service.finishWrite(sequenceId, tail, true)
		service.observe(2500, start)
		service.observe(2500, start.Add(20*time.Millisecond))
		if rate, _, _ := service.measured(time.Second, start.Add(20*time.Millisecond)); rate != 125000 {
			t.Fatalf("%s: initial service=%d", outcome, rate)
		}
		if outcome != "natural" {
			service.observeRoundTrip(500*time.Millisecond, 10*time.Millisecond, start.Add(40*time.Millisecond))
			if delay, _ := service.admitBurst(start.Add(60*time.Millisecond), 2500, false, &windowPacingWaiter{}); delay <= 0 {
				t.Fatalf("%s: controlled drain did not start", outcome)
			}
		}
		at := start.Add(300 * time.Millisecond)
		service.acknowledgeWrite(sequenceId, tail, 1, false, 10*time.Millisecond, at)
		service.beginWrite(sequenceId, probe, 2, at, false)
		service.observe(12500, start.Add(280*time.Millisecond))
		if outcome != "failed-write" {
			service.finishWrite(sequenceId, probe, true)
		}
		want := ByteCount(125000)
		if outcome == "natural" {
			want = 48076
		}
		if rate, _, latest := service.measured(time.Second, at); max(rate, latest) != want {
			t.Errorf("%s: pending probe estimate=%d/%d want=%d", outcome, rate, latest, want)
		}
		switch outcome {
		case "failed-write":
			service.finishWrite(sequenceId, probe, false)
		case "retry":
			service.beginWrite(sequenceId, probe, 2, at, true)
		case "carrier-change":
			service.invalidateProbe(sequenceId)
		case "cancel":
			pacer := &windowBurstPacer{service: service, serviceSequenceId: sequenceId}
			pacer.close()
		case "missing-reply":
			if rate, _, latest := service.measured(time.Second, at.Add(time.Minute)); max(rate, latest) != 125000 {
				t.Errorf("missing reply changed the prior service: %d/%d", rate, latest)
			}
			service.invalidateProbe(sequenceId)
		}
		if !service.serviceEpochAt.IsZero() {
			t.Errorf("%s: an unconfirmed probe changed the service epoch", outcome)
		}
		if rate, _, latest := service.measured(time.Second, at); max(rate, latest) != 48076 {
			t.Errorf("%s: probe rollback lost old samples: %d/%d", outcome, rate, latest)
		}
	}
}

// A different sequence can deliver a new train before the paused probe's
// own covering ACK arrives. Its valid rate must survive later confirmation.
func TestWindowPacingPendingProbeAcceptsFreshSiblingEvidence(t *testing.T) {
	start := time.Unix(1700000000, 0)
	service := &windowPacingService{sent: 37500}
	sequenceId, tail, probe := NewId(), NewId(), NewId()
	service.observeRoundTrip(100*time.Millisecond, 10*time.Millisecond, start)
	service.beginWrite(sequenceId, tail, 1, start, false)
	service.finishWrite(sequenceId, tail, true)
	service.observe(2500, start)
	service.observe(2500, start.Add(20*time.Millisecond))
	if rate, _, _ := service.measured(time.Second, start.Add(20*time.Millisecond)); rate != 125000 {
		t.Fatalf("initial service=%d", rate)
	}
	service.observeRoundTrip(500*time.Millisecond, 10*time.Millisecond, start.Add(40*time.Millisecond))
	if delay, _ := service.admitBurst(start.Add(60*time.Millisecond), 2500, false, &windowPacingWaiter{}); delay <= 0 {
		t.Fatal("controlled drain did not start")
	}
	at := start.Add(300 * time.Millisecond)
	service.acknowledgeWrite(sequenceId, tail, 1, false, 10*time.Millisecond, at)
	service.beginWrite(sequenceId, probe, 2, at, false)
	service.finishWrite(sequenceId, probe, true)
	service.observe(12500, start.Add(280*time.Millisecond))
	if rate, _, latest := service.measured(time.Second, at); max(rate, latest) != 125000 {
		t.Errorf("late old ACK changed pending service: %d/%d", rate, latest)
	}
	at = start.Add(420 * time.Millisecond)
	service.observe(2500, at)
	if rate, _, latest := service.measured(time.Second, at); max(rate, latest) != 125000 {
		t.Errorf("first fresh checkpoint changed the hold: %d/%d", rate, latest)
	}
	at = start.Add(440 * time.Millisecond)
	service.observe(1000, at)
	if rate, _, _ := service.measured(time.Second, at); rate != 50000 {
		t.Errorf("fresh sibling evidence was hidden while awaiting the probe: %d", rate)
	}
	if !service.serviceEpochAt.IsZero() {
		t.Fatal("pending probe committed a service epoch")
	}
	at = start.Add(460 * time.Millisecond)
	service.acknowledgeWrite(sequenceId, probe, 2, false, 10*time.Millisecond, at)
	service.observe(2500, at)
	if rate, _, latest := service.measured(time.Second, at); max(rate, latest) != 50000 {
		t.Errorf("late probe confirmation overwrote fresh sibling service: %d/%d", rate, latest)
	}
}

// Once all proved old delivery has reached the sampler, that train is real
// service evidence even if the first resumed write still awaits its own ACK.
func TestWindowPacingPendingProbeAcceptsCompleteDrainedTrain(t *testing.T) {
	start := time.Unix(1700000000, 0)
	service := &windowPacingService{sent: 70000}
	sequenceId, tail, probe := NewId(), NewId(), NewId()
	service.observeRoundTrip(100*time.Millisecond, 10*time.Millisecond, start)
	service.beginWrite(sequenceId, tail, 1, start, false)
	service.finishWrite(sequenceId, tail, true)
	service.observe(2500, start)
	service.observe(2500, start.Add(20*time.Millisecond))
	if rate, _, _ := service.measured(time.Second, start.Add(20*time.Millisecond)); rate != 125000 {
		t.Fatalf("initial service=%d", rate)
	}
	service.observeRoundTrip(500*time.Millisecond, 10*time.Millisecond, start.Add(40*time.Millisecond))
	if delay, _ := service.admitBurst(start.Add(60*time.Millisecond), 2500, false, &windowPacingWaiter{}); delay <= 0 {
		t.Fatal("controlled drain did not start")
	}
	at := start.Add(300 * time.Millisecond)
	service.acknowledgeWrite(sequenceId, tail, 1, false, 10*time.Millisecond, at)
	service.beginWrite(sequenceId, probe, 2, at, false)
	service.finishWrite(sequenceId, probe, true)
	service.observe(12500, start.Add(280*time.Millisecond))
	if rate, _, latest := service.measured(time.Second, at); max(rate, latest) != 125000 {
		t.Errorf("incomplete old train changed the hold: %d/%d", rate, latest)
	}
	service.observe(52500, start.Add(280*time.Millisecond))
	if rate, _, _ := service.measured(time.Second, at); rate != 250000 {
		t.Errorf("fully applied drained train lost valid service evidence: %d", rate)
	}
	at = start.Add(400 * time.Millisecond)
	service.acknowledgeWrite(sequenceId, probe, 2, false, 10*time.Millisecond, at)
	service.observe(2500, at)
	if rate, _, latest := service.measured(time.Second, at); max(rate, latest) != 250000 {
		t.Errorf("confirmation restored older service after complete delivery: %d/%d", rate, latest)
	}
}

// New delivery cannot make an incomplete older train look fully sampled.
// The two epochs share total accounting but have separate rate evidence.
func TestWindowPacingPendingNewBytesCannotFinishOldTrain(t *testing.T) {
	start := time.Unix(1700000000, 0)
	service := &windowPacingService{sent: 37500}
	sequenceId, tail, probe := NewId(), NewId(), NewId()
	service.observeRoundTrip(100*time.Millisecond, 10*time.Millisecond, start)
	service.beginWrite(sequenceId, tail, 1, start, false)
	service.finishWrite(sequenceId, tail, true)
	service.observe(2500, start)
	service.observe(2500, start.Add(20*time.Millisecond))
	if rate, _, _ := service.measured(time.Second, start.Add(20*time.Millisecond)); rate != 125000 {
		t.Fatalf("initial service=%d", rate)
	}
	service.observeRoundTrip(500*time.Millisecond, 10*time.Millisecond, start.Add(40*time.Millisecond))
	if delay, _ := service.admitBurst(start.Add(60*time.Millisecond), 2500, false, &windowPacingWaiter{}); delay <= 0 {
		t.Fatal("controlled drain did not start")
	}
	at := start.Add(300 * time.Millisecond)
	service.acknowledgeWrite(sequenceId, tail, 1, false, 10*time.Millisecond, at)
	service.beginWrite(sequenceId, probe, 2, at, false)
	service.finishWrite(sequenceId, probe, true)
	service.observe(12500, start.Add(280*time.Millisecond))
	at = start.Add(420 * time.Millisecond)
	service.observe(20000, at)
	if service.total != service.drainedSent {
		t.Fatal("new bytes did not numerically fill the older applied-byte gap")
	}
	if rate, _, latest := service.measured(time.Second, at); max(rate, latest) != 125000 {
		t.Errorf("one new checkpoint falsely completed old service: %d/%d", rate, latest)
	}
	at = start.Add(440 * time.Millisecond)
	service.observe(1000, at)
	if rate, _, _ := service.measured(time.Second, at); rate != 50000 {
		t.Errorf("the new train's own pair did not replace the hold: %d", rate)
	}
}
