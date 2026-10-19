// Capacity proof belongs to a measured service interval, including the
// budgeted estimator and explicit retirement of old route evidence.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// The budgeted estimator must not recover the lifetime-byte qualification
// rejected by the earlier fixed-window service read.
func TestWindowOpeningQualificationBudgetedSparseDrainsStayUnqualified(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newOpeningQualificationFixture(t)
		sequence := fixture.sequence
		defer sequence.windowPacer.close()
		settings := sequence.sendBufferSettings
		settings.ResendQueueBudget = NewTransferMemoryBudget(4 * settings.ResendQueueMaxByteCount)
		sequence.resendQueue = newResendQueue(settings.ResendQueueBudget, settings.ResendQueueMinByteCount)
		estimate := sequence.sendWindowEstimate(time.Now())
		if estimate.WindowRoundTrip <= 0 || estimate.Reason == "no round trip samples" || sequence.resendQueue.Budget() == nil {
			t.Fatal("budgeted fixture did not reach the later service estimator")
		}
		if estimate.ServiceEstablished || estimate.ServiceSized {
			t.Errorf("budgeted lifetime sparse bytes established capacity or sized service: established=%t sized=%t rate=%d", estimate.ServiceEstablished, estimate.ServiceSized, estimate.ServiceByteRate)
		}
		sequence.windowPacer.service.probeSent = estimate.PacingProbeByteCount
		sequence.windowPacer.serviceSent += 3444
		waiter, _ := reserveOpeningCredit(sequence, time.Now(), estimate, 3444, false)
		releaseOpeningCreditReservation(sequence.windowPacer.service, waiter, 3444, false)
		if waiter.serialization >= time.Millisecond {
			t.Errorf("budgeted lifetime sparse cadence priced first bulk: serialization=%s pace=%d", waiter.serialization, estimate.PacingByteRate)
		}
		if estimate.Window != estimate.Initial || estimate.Ceiling != estimate.Initial || sequence.windowPacer.service.pacingReservations != 0 {
			t.Fatal("budgeted qualification changed byte permission or reservation ownership")
		}
	})
}

// Introducing the existing owner budget preserves independently measured
// bulk capacity, including its bounded service sizing candidate.
func TestWindowOpeningQualificationBudgetedBulkRetainsCapacity(t *testing.T) {
	runOpeningQualificationBulk(t, 10000, 9, 10*time.Millisecond, 1000000, func(t *testing.T, fixture *openingQualificationFixture, before SendWindowEstimate) {
		sequence := fixture.sequence
		settings := sequence.sendBufferSettings
		settings.ResendQueueBudget = NewTransferMemoryBudget(4 * settings.ResendQueueMaxByteCount)
		sequence.resendQueue = newResendQueue(settings.ResendQueueBudget, settings.ResendQueueMinByteCount)
		after := sequence.sendWindowEstimate(time.Now())
		if !after.ServiceEstablished || !after.ServiceSized || after.ServiceByteRate != before.ServiceByteRate {
			t.Fatalf("budgeted estimator lost independent bulk capacity: before=%+v after=%+v", before, after)
		}
		if after.Window != before.Window || after.Ceiling != before.Ceiling || after.Window > settings.ResendQueueBudget.TotalByteCount() {
			t.Fatal("budgeted bulk qualification changed hard byte permission")
		}
	})
}

// A quality signal preserves the last qualified rate and proof together
// until new-path serialization replaces them; it does not replenish credit.
func TestWindowOpeningQualificationQualityResetPreservesQualifiedHold(t *testing.T) {
	runOpeningQualificationBulk(t, 10000, 9, 10*time.Millisecond, 1000000, func(t *testing.T, fixture *openingQualificationFixture, before SendWindowEstimate) {
		sequence := fixture.sequence
		service := sequence.windowPacer.service
		total, sent, probeSent := service.total, service.sent, service.probeSent
		service.networkQualityChanged(time.Now())
		after := sequence.sendWindowEstimate(time.Now())
		if !after.ServiceEstablished || after.ServiceByteRate != before.ServiceByteRate {
			t.Fatalf("quality reset detached qualified hold from its capacity proof: before=%+v after=%+v", before, after)
		}
		if after.Window != before.Window || after.Ceiling != before.Ceiling || service.total != total || service.sent != sent || service.probeSent != probeSent {
			t.Fatal("quality reset changed byte permission, delivery accounting or spent probe credit")
		}
	})
}

// A previous bulk epoch cannot qualify a new 153-byte/300-ms measurement.
// This forces a real new rate after reset, rather than testing a zero hold.
func TestWindowOpeningQualificationQualityResetDoesNotLendOldProofToSparseRate(t *testing.T) {
	runOpeningQualificationBulk(t, 10000, 9, 10*time.Millisecond, 1000000, func(t *testing.T, fixture *openingQualificationFixture, before SendWindowEstimate) {
		sequence := fixture.sequence
		service := sequence.windowPacer.service
		total := service.total
		service.networkQualityChanged(time.Now())
		time.Sleep(300 * time.Millisecond)
		first := fixture.write(t, 153)
		tail := fixture.write(t, 153)
		time.Sleep(10 * time.Millisecond)
		fixture.acknowledge(first)
		time.Sleep(300 * time.Millisecond)
		fixture.acknowledge(tail)
		rate, delivered, latest := service.measured(time.Second, time.Now())
		if max(rate, latest) != 510 || delivered != total+2*153 || !service.drained || service.sent != service.total {
			t.Fatalf("quality fixture lost its new sparse interval or physical accounting: rate=%d latest=%d delivered=%d", rate, latest, delivered)
		}
		after := sequence.sendWindowEstimate(time.Now())
		if after.ServiceEstablished && after.ServiceByteRate == 510 {
			t.Errorf("old bulk byte proof qualified new sparse rate after quality reset: before=%d after=%d", before.ServiceByteRate, after.ServiceByteRate)
		}
		if after.Window != before.Window || after.Ceiling != before.Ceiling || service.pacingReservations != 0 {
			t.Fatal("quality sparse qualification changed byte permission or reservation ownership")
		}
	})
}
