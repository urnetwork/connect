// A rate observed after a caller's snapshot cannot borrow that snapshot's
// older queue tuple, even when it matches the service's newest arrival clock.
package connect

import (
	"testing"
	"time"
)

// Real cumulative credit crosses the captured read clock. Both tuples have
// queue residence, but only the later tuple belongs to the accepted rate.
func TestWindowPacingFutureRateCannotBorrowOlderQueue(t *testing.T) {
	const frameBytes ByteCount = 2671
	service, at := newWindowQualifiedServiceFixture(t, 12500000, 50*time.Millisecond, 100*time.Millisecond)
	service.observeReceiverRoundTrip(0, 180*time.Millisecond, 130*time.Millisecond, 50*time.Millisecond, at.Add(time.Millisecond))
	at = at.Add(2 * time.Millisecond)
	service.observeReceiverRoundTrip(0, 150*time.Millisecond, 100*time.Millisecond, 50*time.Millisecond, at)
	fixture := newWindowReceiverCreditFixture(t, service, at, 50*time.Millisecond)
	sequence := fixture.sequence
	sequence.sendBufferSettings = DefaultSendBufferSettings()
	sequence.deliveredBytes = make([]deliveredBytesSample, deliveredBytesRingSize)
	sequence.contractMultiRouteWriter = &windowPacingPolicyWriter{policy: transferFlightPolicySnapshot{h1Only: true}}
	sequence.observeReceiveWindowAdvertisement(receiveAckMessage{receiveWindowSet: true, receiveWindowByteCount: uint32(kib(64))})
	t.Cleanup(func() {
		sequence.windowPacer.close()
		sequence.resendQueue.Clear()
		if service.sent != service.total || service.pendingWrites != 0 || service.reservedByteCount != 0 {
			t.Error("closing the actual owner retained physical flight")
		}
	})
	before := sequence.sendWindowEstimate(at)
	if before.PacingByteRate != 13750000 || before.ServiceBacklogged || before.PacingDiscovery {
		t.Fatalf("ordinary admission did not grant the prior pace: %+v", before)
	}
	priorTotal := service.total
	offerAt := at.Add(time.Second)
	var originals [24]*sendItem
	for number := range originals {
		originals[number] = fixture.write(uint64(number), frameBytes, offerAt)
	}
	leftAt := offerAt.Add(100 * time.Millisecond)
	sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(originals[0], leftAt, 0))
	guard := fixture.write(24, frameBytes, leftAt)
	olderAt := offerAt.Add(144 * time.Millisecond)
	sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(originals[1], olderAt, 40*time.Millisecond))
	readAt := offerAt.Add(150 * time.Millisecond)
	futureAt := offerAt.Add(151 * time.Millisecond)
	sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(originals[4], futureAt, 47*time.Millisecond))
	older := service.roundTripEvidence(readAt)
	newer := service.roundTripEvidence(futureAt)
	if older.latestRaw != 144*time.Millisecond || newer.latestRaw != 151*time.Millisecond ||
		older.latest != 104*time.Millisecond || newer.latest != older.latest || older.minimum != 100*time.Millisecond ||
		newer.count != older.count+1 || !service.lastRoundTrip.Equal(futureAt) {
		t.Fatalf("the captured clock did not exclude only the later receiver tuple: older=%+v newer=%+v", older, newer)
	}
	rate, supported, latest, proofAtNanos := service.measureCapacityEvidence(time.Second, readAt, false)
	if rate != 209490 || latest != rate || supported != kib(4) || proofAtNanos != futureAt.UnixNano() ||
		service.total-priorTotal != 5*frameBytes || service.sent-service.total != 20*frameBytes ||
		service.heldPacingRate != before.PacingByteRate || service.drained || service.feedbackPending || guard.serviceCreditObserved {
		t.Fatalf("real future credit lost its accepted endpoint or prior grant: rate=%d latest=%d support=%d proof=%d", rate, latest, supported, proofAtNanos)
	}
	if !service.backloggedAt(rate, readAt) {
		t.Fatal("the explicit-rate contract no longer recognizes excess low-rate flight")
	}
	snapshot := sequence.sendWindowSnapshot(readAt)
	if snapshot.ServiceByteRate != rate || snapshot.ServiceBacklogged || snapshot.PacingByteRate != before.PacingByteRate ||
		service.heldPacingRate != before.PacingByteRate {
		t.Errorf("future rate borrowed older queue timing: %+v", snapshot)
	}
	current := sequence.sendWindowSnapshot(futureAt)
	wantPace := ByteCount(.95 * float64(rate))
	if current.ServiceByteRate != rate || !current.ServiceBacklogged || current.PacingByteRate != wantPace ||
		service.heldPacingRate != before.PacingByteRate {
		t.Errorf("the matching current queue pair lost decrease authority: %+v", current)
	}
	after := sequence.sendWindowEstimate(futureAt)
	if after.ServiceByteRate != rate || !after.ServiceBacklogged || after.PacingByteRate != wantPace || service.heldPacingRate != wantPace {
		t.Errorf("admission ignored the current accepted queue pair: %+v", after)
	}
}
