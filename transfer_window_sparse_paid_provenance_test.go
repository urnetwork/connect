// Candidate-only structural controls. The behavioral OLD regression is in the
// separate existing-API file, so these new fields never create a compile red.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

func TestWindowPacingSparsePaidSingletonMergeIsConservative(t *testing.T) {
	lane, message := NewId(), NewId()
	one := windowServiceAckCredit{bytes: 2102, singleMessageId: message, readyAtNanos: 100, readyLane: lane}
	var merged windowServiceAckCredit
	merged.add(one)
	merged.add(windowServiceAckCredit{})
	if merged.singleMessageId != message || merged.readyAtNanos != 100 || merged.readyLane != lane {
		t.Fatal("an empty destination or zero-byte duplicate lost singleton evidence")
	}
	merged.add(one)
	if merged.singleMessageId != (Id{}) || merged.readyAtNanos != 0 || merged.readyLane != (Id{}) || merged.bytes != 4204 {
		t.Fatal("a positive credit merge retained singleton readiness")
	}
	for _, reversed := range []bool{false, true} {
		for _, tied := range []bool{false, true} {
			left := windowServiceSample{firstAtNanos: 200, lastAtNanos: 200, firstBytes: 2102, bytes: 2102,
				supply: windowServiceSupply{readyAtNanos: 100, readyLane: lane}}
			right := left
			if !tied {
				right.firstAtNanos, right.lastAtNanos = 300, 300
			}
			if reversed {
				left, right = right, left
			}
			left.mergeSupply(right)
			if left.supply.readyAtNanos != 0 || left.supply.readyLane != (Id{}) {
				t.Fatal("reordered or tied sample merge retained singleton readiness")
			}
			if !tied && left.afterFirst.bytes != 2102 {
				t.Fatal("conservative readiness merge changed exact suffix byte ownership")
			}
		}
	}
}

func TestWindowPacingSparsePaidPendingCancellationKeepsPriorTail(t *testing.T) {
	windowSparsePaidPool(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newWindowSparsePaidFixture(t)
		sequence := fixture.credit.sequence
		first := fixture.item(t)
		fixture.write(t, first)
		before := sequence.pacingReadyWrite
		if before.confirmedId != first.messageId || before.route != fixture.route || before.confirmedAtNanos <= 0 || before.lane == (Id{}) {
			t.Fatal("successful acquired H1 original did not confirm exact readiness")
		}
		canceled := fixture.item(t)
		result := make(chan error, 1)
		joined := make(chan struct{})
		go func() {
			defer close(joined)
			_, err := sequence.writeMaybeWrappedBytes(canceled.transferFrameBytes, TransferPath{}, true, canceled, false, false)
			result <- err
		}()
		defer func() { sequence.cancel(); <-joined }()
		synctest.Wait()
		if sequence.pacingReadyWrite.pendingId != canceled.messageId ||
			sequence.pacingReadyWrite.confirmedId != first.messageId ||
			fixture.credit.service.pacingReservations != 1 || fixture.credit.service.reservedByteCount != windowSparsePaidBytes {
			t.Fatal("cancellation control never owned the next paid reservation")
		}
		sequence.cancel()
		<-joined
		if err := <-result; err == nil {
			t.Fatal("canceled original unexpectedly reached the physical writer")
		}
		after := sequence.pacingReadyWrite
		if after.pendingId != (Id{}) || after.pendingAtNanos != 0 || after.pendingGeneration != 0 ||
			after.confirmedId != before.confirmedId || after.confirmedAtNanos != before.confirmedAtNanos || after.lane != before.lane ||
			len(fixture.route) != 0 || fixture.credit.service.pacingReservations != 0 {
			t.Fatal("pre-dispatch cancellation lent pending demand or erased the prior confirmed tail")
		}
		sequence.resetLaneAcks(before.confirmedGeneration + 1)
		if sequence.pacingReadyWrite != (windowPacingReadyWrite{}) {
			t.Fatal("route generation retirement retained a channel or ready identity")
		}
	})
}

// This scalar boundary check does not claim an actual shared-sibling replay.
func TestWindowPacingSparsePaidLaneIdentityCannotBeBorrowed(t *testing.T) {
	base := time.Unix(1700000000, 0).UnixNano()
	lane := NewId()
	supply := windowServiceSupply{readyAtNanos: base, readyLane: lane,
		earliestSentNanos: base + int64(165*time.Millisecond), latestSentNanos: base + int64(165*time.Millisecond),
		headMaximumAtNanos: base + int64(175*time.Millisecond), recorded: true, complete: true, headComplete: true}
	span := int64(165 * time.Millisecond)
	headStart := base + int64(10*time.Millisecond)
	if supply.pacedOfferSpan(span, headStart, base, lane, 10*time.Millisecond) != span {
		t.Fatal("same exact physical lane lost its full paid denominator")
	}
	for _, other := range []Id{NewId(), {}} {
		if supply.pacedOfferSpan(span, headStart, base, other, 10*time.Millisecond) != 0 {
			t.Fatal("another or unknown physical lane lent its readiness")
		}
	}
	supply.readyAtNanos = base + int64(20*time.Millisecond) + 1
	if supply.pacedOfferSpan(span, headStart, base, lane, 10*time.Millisecond) != 0 {
		t.Fatal("one nanosecond beyond the real source-idle boundary became paid demand")
	}
}

func TestWindowPacingSparsePaidFailedPhysicalWriteHasNoReadiness(t *testing.T) {
	windowSparsePaidPool(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newWindowSparsePaidFixture(t)
		sequence := fixture.credit.sequence
		first := fixture.item(t)
		fixture.write(t, first)
		sequence.sendBufferSettings.WriteTimeout = 0
		fixture.route <- MessagePoolCopy([]byte("occupied test route"))
		failed := fixture.item(t)
		if _, err := sequence.writeMaybeWrappedBytes(failed.transferFrameBytes, TransferPath{}, true, failed, false, false); err == nil {
			t.Fatal("full zero-budget physical route unexpectedly accepted the original")
		}
		MessagePoolReturn(<-fixture.route)
		credit := sequence.takePacingServiceCredit(failed)
		if sequence.pacingReadyWrite != (windowPacingReadyWrite{}) || credit.offerComplete || credit.readyAtNanos != 0 ||
			credit.readyLane != (Id{}) || failed.rttState != sendItemRttUnavailable || fixture.credit.service.pacingReservations != 0 {
			t.Fatal("failed physical write retained confirmed readiness or exact RTT authority")
		}
	})
}

// This unit controls callback order directly: it is not the paid-rate fixture.
func TestWindowPacingSparsePaidEarlyAckCannotUpgradeCredit(t *testing.T) {
	windowSparsePaidPool(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newWindowSparsePaidFixture(t)
		sequence, service := fixture.credit.sequence, fixture.credit.service
		at := time.Now()
		item := fixture.credit.offer(fixture.number, windowSparsePaidBytes, at)
		generation := fixture.selector.transferFlightPolicy().generation
		sequence.pacingReadyWrite.pendingId = item.messageId
		sequence.pacingReadyWrite.pendingAtNanos = at.UnixNano()
		sequence.pacingReadyWrite.pendingGeneration = generation
		time.Sleep(10 * time.Millisecond)
		ack := fixture.credit.ack(item, time.Now(), 0)
		before := service.total
		sequence.coalesceReceivedAck(fixture.credit.ackWindow, ack)
		sequence.finishPacingReadyWrite(item, transferWriteDisposition{transportType: TransportTypeH1, reliable: true, route: fixture.route}, nil, generation)
		fixture.credit.confirm(item)
		sequence.coalesceReceivedAck(fixture.credit.ackWindow, ack)
		if service.total != before+windowSparsePaidBytes || !item.serviceCreditObserved ||
			sequence.takePacingServiceCredit(item).bytes != 0 {
			t.Fatal("late physical confirmation recreated first-credit ownership")
		}
		found := false
		for _, sample := range service.samples {
			if sample.lastAtNanos == ack.receivedAtNanos {
				found = true
				if sample.supply.complete || sample.supply.readyAtNanos != 0 || sample.supply.readyLane != (Id{}) {
					t.Fatal("late physical confirmation upgraded an already published early ACK")
				}
			}
		}
		if !found {
			t.Fatal("early ACK's original byte sample was not retained")
		}
	})
}

func TestWindowPacingSparsePaidQualityBoundaryRejectsOldReadiness(t *testing.T) {
	windowSparsePaidPool(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newWindowSparsePaidFixture(t)
		sequence, service := fixture.credit.sequence, fixture.credit.service
		item := fixture.item(t)
		requested := time.Now()
		sequence.windowPacer.afterWaitForTest = func() {
			service.networkQualityChanged(time.Now())
			sequence.windowPacer.afterWaitForTest = nil
			time.Sleep(time.Nanosecond)
		}
		fixture.write(t, item)
		if service.qualityChangedAt.Before(requested) || !time.Unix(0, item.pacingSentAtNanos).After(service.qualityChangedAt) {
			t.Fatal("quality fixture did not separate ready demand from its new-generation physical write")
		}
		time.Sleep(10 * time.Millisecond)
		before := service.total
		fixture.acknowledge(t, item, 0)
		if service.total != before+windowSparsePaidBytes {
			t.Fatal("quality boundary discarded raw original ownership")
		}
		for _, sample := range service.samples {
			if sample.bytes > 0 && sample.supply.readyAtNanos != 0 {
				t.Fatal("old-generation source readiness joined a fresh physical sample")
			}
		}
	})
}
