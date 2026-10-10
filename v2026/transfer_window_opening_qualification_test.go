// Sparse control flights own their delivered bytes and physical drain proofs,
// but their lifetime total is not evidence of a saturated data serializer.
package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

// Fixed test traffic is offered independently of the estimator being checked.
// Only the real pacing service owns reservations, writes, tails and ACK bytes.
type openingQualificationFixture struct {
	sequence   *SendSequence
	sequenceId Id
	number     uint64
}

// A physical message retains its own source and arrival times for the
// independently configured serializer used by the slow and fast controls.
type openingQualificationArrival struct {
	messageId Id
	number    uint64
	bytes     ByteCount
	sentAt    time.Time
	arriveAt  time.Time
}

// Use production reservation and actual-write enrollment, including the
// source-idle transition at a resumed offer. No transport goroutine is needed.
func (self *openingQualificationFixture) write(t *testing.T, bytes int) openingQualificationArrival {
	t.Helper()
	self.number++
	messageId := NewId()
	pacer := &self.sequence.windowPacer
	if err := pacer.waitForServiceMessage(context.Background(), bytes, false, self.sequenceId, messageId, self.number); err != nil {
		t.Fatal(err)
	}
	pacer.service.finishWrite(self.sequenceId, messageId, true)
	return openingQualificationArrival{messageId: messageId, number: self.number, bytes: ByteCount(bytes), sentAt: time.Now()}
}

// Publish the physical acknowledgement and its once-only byte accounting in
// the same order as the current sender, then retain the measured raw evidence.
func (self *openingQualificationFixture) acknowledge(arrival openingQualificationArrival) {
	service := self.sequence.windowPacer.service
	at := time.Now()
	roundTrip := at.Sub(arrival.sentAt)
	service.observeReceiverRoundTripForWrite(self.sequenceId, arrival.messageId, 0, roundTrip, roundTrip, 0, at)
	service.acknowledgeWrite(self.sequenceId, arrival.messageId, arrival.number, false, 0, at)
	service.observe(arrival.bytes, at)
	self.sequence.windowPacer.serviceAcked += arrival.bytes
	service.measured(time.Second, at)
}

// Two tiny preoffered controls supply a 153-byte/300-ms raw interval. Thirty
// isolated 153-byte replies then exceed 4 KiB only across distinct drained
// flights, each separated by 300 ms of source idle. No bulk train was offered.
func newOpeningQualificationFixture(t *testing.T) *openingQualificationFixture {
	t.Helper()
	sequence := newEstimatorFixture(t, nil)
	sequence.contractMultiRouteWriter = &windowPacingPolicyWriter{policy: transferFlightPolicySnapshot{h1Only: true}}
	sequence.observeReceiveWindowAdvertisement(receiveAckMessage{ackCompressTimeoutSet: true, ackCompressTimeoutMicros: 0})
	service := newWindowPacingService(sequence.sendBufferSettings)
	fixture := &openingQualificationFixture{sequence: sequence, sequenceId: NewId()}
	sequence.windowPacer = windowBurstPacer{
		service: service, serviceSequenceId: fixture.sequenceId,
		rate: 147928994, estimateRate: 147928994,
	}
	first := fixture.write(t, 153)
	tail := fixture.write(t, 153)
	time.Sleep(10 * time.Millisecond)
	fixture.acknowledge(first)
	time.Sleep(300 * time.Millisecond)
	fixture.acknowledge(tail)
	for range 30 {
		time.Sleep(300 * time.Millisecond)
		arrival := fixture.write(t, 153)
		time.Sleep(10 * time.Millisecond)
		fixture.acknowledge(arrival)
		if !service.drained || service.pendingWrites != 0 || service.sent != service.total {
			t.Fatal("sparse fixture lost an exact physical drain or once-only byte accounting")
		}
	}
	if service.total != 32*153 || service.pacingReservations != 0 || service.total < 4*1024 {
		t.Fatal("sparse fixture did not cross the lifetime-byte threshold with drained controls")
	}
	rate, total, latest := service.measured(time.Second, time.Now())
	t.Logf("sparse physical controls: delivered=%d current=%d held=%d outstanding=%d", total, rate, latest, service.sent-service.total)
	return fixture
}

// A lifetime sum of tiny drained flights must not establish capacity or make
// the first data message wait seconds at their sparse request/reply cadence.
func TestWindowOpeningQualificationSparseDrainsDoNotEstablishCapacity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newOpeningQualificationFixture(t)
		sequence := fixture.sequence
		defer sequence.windowPacer.close()
		estimate := sequence.sendWindowEstimate(time.Now())
		if estimate.ServiceEstablished {
			t.Errorf("lifetime sparse control bytes established saturated service: rate=%d pacing=%d", estimate.ServiceByteRate, estimate.PacingByteRate)
		}
		// Remove the independent one-shot allowance as a possible explanation
		// for progress. This checks adaptive pricing, not target-credit refill.
		sequence.windowPacer.service.probeSent = estimate.PacingProbeByteCount
		sequence.windowPacer.serviceSent += 3444
		waiter, _ := reserveOpeningCredit(sequence, time.Now(), estimate, 3444, false)
		releaseOpeningCreditReservation(sequence.windowPacer.service, waiter, 3444, false)
		if waiter.serialization >= time.Millisecond {
			t.Errorf("lifetime sparse control cadence priced the first bulk message: serialization=%s pace=%d", waiter.serialization, estimate.PacingByteRate)
		}
		if estimate.Window != estimate.Initial || estimate.Ceiling != estimate.Initial {
			t.Fatal("qualification changed fixed-window byte permission")
		}
	})
}

// Both controls preoffer a bounded train within the existing fixed window.
// The serializer is an independent byte/time queue; it does not use the
// estimator's rate, spacing, service hold or admission decisions as its oracle.
func runOpeningQualificationBulk(t *testing.T, bytes, count int, serialization time.Duration, wantRate ByteCount, afterBulk func(*testing.T, *openingQualificationFixture, SendWindowEstimate)) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		fixture := newOpeningQualificationFixture(t)
		sequence := fixture.sequence
		defer sequence.windowPacer.close()
		service := sequence.windowPacer.service
		before := sequence.sendWindowEstimate(time.Now())
		if ByteCount(bytes*count) > before.Window {
			t.Fatal("control train exceeds its existing hard byte window")
		}
		time.Sleep(300 * time.Millisecond)
		arrivals := make([]openingQualificationArrival, 0, count)
		freeAt := time.Now()
		for range count {
			arrival := fixture.write(t, bytes)
			if freeAt.Before(arrival.sentAt) {
				freeAt = arrival.sentAt
			}
			freeAt = freeAt.Add(serialization)
			arrival.arriveAt = freeAt.Add(10 * time.Millisecond)
			arrivals = append(arrivals, arrival)
		}
		if !time.Now().Before(arrivals[0].arriveAt) {
			t.Fatal("bulk fixture did not preoffer its complete train before the first acknowledgement")
		}
		for _, arrival := range arrivals {
			time.Sleep(time.Until(arrival.arriveAt))
			fixture.acknowledge(arrival)
		}
		after := sequence.sendWindowEstimate(time.Now())
		t.Logf("bulk serializer=%d B/s current=%d pace=%d established=%t bytes=%d", wantRate, after.ServiceByteRate, after.PacingByteRate, after.ServiceEstablished, bytes*count)
		if !after.ServiceEstablished || after.ServiceByteRate != wantRate {
			t.Errorf("fresh preoffered bulk did not replace the sparse hold: established=%t service=%d want=%d", after.ServiceEstablished, after.ServiceByteRate, wantRate)
		}
		// New unqueued evidence must not revoke an already admitted pace.
		// Both the measured increase and that hold remain target-bounded.
		maximumPace := min(after.PacingProbeByteRate, max(before.PacingByteRate, ByteCount(1.1*float64(wantRate))))
		if after.PacingByteRate < ByteCount(.95*float64(wantRate)) || after.PacingByteRate > maximumPace {
			t.Errorf("fresh bulk service did not bound adaptive pacing: rate=%d pace=%d", wantRate, after.PacingByteRate)
		}
		if after.Window != before.Window || after.Ceiling != before.Ceiling || !service.drained || service.sent != service.total || service.pacingReservations != 0 {
			t.Fatal("fresh bulk changed hard permission or lost drain/reservation accounting")
		}
		if afterBulk != nil {
			afterBulk(t, fixture, after)
		}
	})
}

// Nine 10 kB messages serialize at 1 MB/s over an 80 ms measured interval.
func TestWindowOpeningQualificationSparseThenSlowBulk(t *testing.T) {
	runOpeningQualificationBulk(t, 10000, 9, 10*time.Millisecond, 1000000, nil)
}

// A 1.3 MB train fits the original 2 MiB window and spans a complete 10 ms
// sampling interval on a 125 MB/s serializer after the sparse control epoch.
func TestWindowOpeningQualificationSparseThenFastBulk(t *testing.T) {
	runOpeningQualificationBulk(t, 2500, 520, 20*time.Microsecond, 125000000, nil)
}

// Once an independent bulk train has established capacity, subsequent sparse
// physical drains keep that proof and its admitted pace until new congestion.
func TestWindowOpeningQualificationQualifiedBulkSurvivesSparseDrains(t *testing.T) {
	runOpeningQualificationBulk(t, 10000, 9, 10*time.Millisecond, 1000000, func(t *testing.T, fixture *openingQualificationFixture, before SendWindowEstimate) {
		sequence := fixture.sequence
		service := sequence.windowPacer.service
		totalBefore := service.total
		for index := range 30 {
			time.Sleep(300 * time.Millisecond)
			arrival := fixture.write(t, 153)
			time.Sleep(10 * time.Millisecond)
			fixture.acknowledge(arrival)
			after := sequence.sendWindowEstimate(time.Now())
			if !after.ServiceEstablished || after.ServiceByteRate != before.ServiceByteRate || after.ServiceBacklogged || after.PacingByteRate != before.PacingByteRate {
				t.Fatalf("sparse physical drain %d erased previously qualified capacity or pace: before=%+v after=%+v", index, before, after)
			}
			if after.Window != before.Window || after.Ceiling != before.Ceiling || !service.drained || service.sent != service.total || service.pacingReservations != 0 {
				t.Fatal("sparse physical drain changed hard permission or lost accounting")
			}
		}
		if service.total != totalBefore+30*153 {
			t.Fatal("retaining qualified capacity discarded sparse delivered bytes")
		}
	})
}
