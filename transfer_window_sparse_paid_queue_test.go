// A bounded carrier consumer separates physical offering from the independent
// serializer's ACK clock. Waiting for one ACK must not stall the next offer.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// Wait observes prior quiescence, not future ownership. A producer can enroll
// its next physical write after that barrier; copy each related predicate
// under the same lock as reservation release, without parking that producer.
type windowSparsePaidServiceState struct {
	total, sent, reservedByteCount              ByteCount
	bucketInterval                              time.Duration
	pendingWrites, pacingReservations           int
	drained, feedbackPending, drainServiceEpoch bool
	waiterHead, waiterTail                      *windowPacingWaiter
}

func windowSparsePaidReadService(service *windowPacingService) windowSparsePaidServiceState {
	service.stateLock.Lock()
	defer service.stateLock.Unlock()
	return windowSparsePaidServiceState{
		total: service.total, sent: service.sent, reservedByteCount: service.reservedByteCount,
		bucketInterval: service.bucketInterval, pendingWrites: service.pendingWrites,
		pacingReservations: service.pacingReservations, drained: service.drained,
		feedbackPending: service.feedbackPending, drainServiceEpoch: service.drainServiceEpoch,
		waiterHead: service.waiterHead, waiterTail: service.waiterTail,
	}
}

// The two existing dispatch barriers surround actual reservation release.
// The middle snapshot has no Wait or channel edge ordering it against that
// transaction: only the service mutex may prevent a torn ownership tuple.
func TestWindowPacingSparsePaidSnapshotOrdersReservationRelease(t *testing.T) {
	windowSparsePaidPool(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newWindowSparsePaidFixture(t)
		sequence, service := fixture.credit.sequence, fixture.credit.service
		item := fixture.item(t)
		enteredWait, releaseWait := make(chan struct{}), make(chan struct{})
		admitted, releaseAdmission := make(chan struct{}), make(chan struct{})
		sequence.windowPacer.afterWaitForTest = func() {
			sequence.windowPacer.afterWaitForTest = nil
			close(enteredWait)
			select {
			case <-releaseWait:
			case <-sequence.ctx.Done():
			}
		}
		sequence.windowPacer.afterAdmissionForTest = func() {
			close(admitted)
			select {
			case <-releaseAdmission:
			case <-sequence.ctx.Done():
			}
		}
		before := windowSparsePaidReadService(service)
		result := make(chan windowSparsePaidResult, 1)
		done := make(chan struct{})
		go func() {
			defer close(done)
			disposition, err := sequence.writeMaybeWrappedBytes(item.transferFrameBytes, TransferPath{}, true, item, false, false)
			if err == nil {
				sequence.observeCarrierWrite(item, disposition)
			}
			result <- windowSparsePaidResult{item: item, at: time.Now(), rate: sequence.windowPacer.rate,
				serialization: sequence.windowPacer.waiter.serialization, err: err}
		}()
		defer func() { sequence.cancel(); <-done }()
		select {
		case <-enteredWait:
		case <-sequence.ctx.Done():
			t.Fatal("snapshot control never reached its real paid wait")
		}
		pending := windowSparsePaidReadService(service)
		if pending.total != before.total || pending.sent != before.sent+windowSparsePaidBytes ||
			pending.reservedByteCount != windowSparsePaidBytes || pending.pacingReservations != 1 ||
			!pending.drained || pending.pendingWrites != 0 ||
			pending.waiterHead != &sequence.windowPacer.waiter || pending.waiterTail != &sequence.windowPacer.waiter {
			t.Fatalf("snapshot control did not own one real pending reservation: %+v", pending)
		}
		close(releaseWait)
		crossing := windowSparsePaidReadService(service)
		select {
		case <-admitted:
		case <-sequence.ctx.Done():
			t.Fatal("snapshot control never completed actual reservation release")
		}
		enrolled := windowSparsePaidReadService(service)
		if enrolled.total != before.total || enrolled.sent != pending.sent ||
			enrolled.reservedByteCount != 0 || enrolled.pacingReservations != 0 ||
			enrolled.drained || enrolled.pendingWrites != 1 || enrolled.drainServiceEpoch ||
			enrolled.waiterHead != nil || enrolled.waiterTail != nil {
			t.Fatalf("snapshot control lost the enrolled physical transaction: %+v", enrolled)
		}
		if crossing != pending && crossing != enrolled {
			t.Fatalf("snapshot crossed reservation release with a torn tuple: pending=%+v crossing=%+v enrolled=%+v", pending, crossing, enrolled)
		}
		close(releaseAdmission)
		<-done
		written := <-result
		if written.err != nil || written.item != item ||
			written.serialization != windowPacingSerializationTime(windowSparsePaidBytes, written.rate) || written.serialization <= 0 ||
			item.pacingByteCount != windowSparsePaidBytes || item.rttState != sendItemRttWriteConfirmed || !item.rttH1 {
			t.Fatalf("snapshot control bypassed paid physical confirmation: %+v", written)
		}
		fixture.consume(t, item)
		time.Sleep(10 * time.Millisecond)
		fixture.acknowledge(t, item, 0)
		paid := windowSparsePaidReadService(service)
		if paid.total != before.total+windowSparsePaidBytes || paid.sent != paid.total ||
			paid.reservedByteCount != 0 || paid.pacingReservations != 0 || paid.pendingWrites != 0 || !paid.drained ||
			paid.waiterHead != nil || paid.waiterTail != nil || sequence.resendWriteCount.Load() != 0 {
			t.Fatalf("snapshot control retained physical, reservation, or duplicate-credit ownership: %+v", paid)
		}
	})
}

// Twelve real paid writes feed a fixed FIFO serializer. The event loop drains
// every physical share promptly, even while an earlier ACK is still scheduled.
// No hold, rate, feedback, baseline, drain policy, or byte counter is assigned.
func TestWindowPacingSparsePaidCoveredSlowQueueStillDecreases(t *testing.T) {
	windowSparsePaidPool(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newWindowSparsePaidFixture(t)
		sequence, service := fixture.credit.sequence, fixture.credit.service
		const serializer = 420400 * time.Microsecond
		const path = 10 * time.Millisecond
		var items [12]*sendItem
		for index := range items {
			items[index] = fixture.item(t)
		}
		beforeBytes := windowSparsePaidReadService(service).total
		results := make(chan windowSparsePaidResult)
		done := make(chan struct{})
		go func() {
			defer close(done)
			for _, item := range items {
				disposition, err := sequence.writeMaybeWrappedBytes(item.transferFrameBytes, TransferPath{}, true, item, false, false)
				if err == nil {
					sequence.observeCarrierWrite(item, disposition)
				}
				result := windowSparsePaidResult{item: item, at: time.Now(), rate: sequence.windowPacer.rate,
					serialization: sequence.windowPacer.waiter.serialization, err: err}
				select {
				case results <- result:
				case <-sequence.ctx.Done():
					return
				}
				if err != nil {
					return
				}
			}
		}()
		defer func() { sequence.cancel(); <-done }()

		// Only scalar deadlines and borrowed retained-item pointers occupy this
		// fixed queue; consume returns each independently owned carrier share.
		type arrival struct {
			item     *sendItem
			sentAt   time.Time
			arriveAt time.Time
		}
		var arrivals [len(items)]arrival
		written, acked, maximumFlight := 0, 0, 0
		physicalFree := time.Time{}
		var previous windowServiceSample
		var final SendWindowEstimate
		coveredAt, decreaseProofAt := int64(0), int64(0)
		for acked < len(items) {
			if acked < written && !time.Now().Before(arrivals[acked].arriveAt) {
				point := arrivals[acked]
				flightBeforeAck := written - acked
				if !time.Now().Equal(point.arriveAt) {
					t.Fatal("physical-result handling delayed the independent serializer's ACK clock")
				}
				fixture.acknowledge(t, point.item, 0)
				acked++
				synctest.Wait()
				observed := windowSparsePaidReadService(service)
				if observed.total != beforeBytes+ByteCount(acked)*windowSparsePaidBytes {
					t.Fatal("covered serializer lost or duplicated exact raw ACK bytes")
				}
				if observed.sent-observed.total-observed.reservedByteCount < ByteCount(written-acked)*windowSparsePaidBytes {
					t.Fatal("covered serializer's current physical flight lost its byte custody")
				}
				current := windowSparsePaidSample(t, service, point.arriveAt)
				if acked == 1 && current.supply.minimumPath != serializer+path {
					t.Fatal("first empty-flight ACK did not retain the real refreshed RTT baseline")
				}
				span, qualified := int64(0), int64(0)
				preoffered := false
				if previous.bytes > 0 {
					span = current.lastAtNanos - previous.lastAtNanos
					supply := current.supply
					supply.maximumGap = time.Duration(span)
					supply.receiverMaximumGap = time.Duration(span)
					interval := max(deliverySizedWindowSampleInterval, observed.bucketInterval)
					qualified = supply.qualifiedSpan(span, previous.receiverLastAtNanos, previous.supply.headLastMinAtNanos, interval, true)
					preoffered = supply.latestSentNanos <= previous.supply.headLastMinAtNanos-int64(supply.minimumPath)
					if qualified == int64(serializer) && preoffered && maximumFlight >= 3 {
						coveredAt = current.lastAtNanos
					}
				}
				// Read without committing a hold; only actual later writer
				// admission may retain the controller's accepted service.
				rate, _, latest, proof := service.measureCapacityEvidence(time.Second, point.arriveAt, false)
				final = sequence.sendWindowSnapshot(point.arriveAt)
				if coveredAt == current.lastAtNanos && proof == current.lastAtNanos &&
					max(rate, latest) == 5000 && final.ServiceByteRate == 5000 {
					decreaseProofAt = proof
				}
				observed = windowSparsePaidReadService(service)
				t.Logf("covered slow ACK%d: physical=%d flightBefore=%d flightMax=%d baseline=%s queue=%s span=%s covered=%s preoffered=%t feedback=%t drain=%t service=%d proof=%t",
					acked, written, flightBeforeAck, maximumFlight, current.supply.minimumPath, current.supply.lastQueue,
					time.Duration(span), time.Duration(qualified), preoffered, observed.feedbackPending,
					observed.drainServiceEpoch, final.ServiceByteRate, proof == current.lastAtNanos)
				previous = current
				continue
			}

			var nextAck <-chan time.Time
			if acked < written {
				nextAck = time.After(time.Until(arrivals[acked].arriveAt))
			}
			var nextWrite <-chan windowSparsePaidResult
			if written < len(items) {
				nextWrite = results
			}
			select {
			case result := <-nextWrite:
				if result.err != nil {
					t.Fatal(result.err)
				}
				if result.item != items[written] || result.item.pacingByteCount != windowSparsePaidBytes ||
					result.item.rttState != sendItemRttWriteConfirmed || !result.item.rttH1 ||
					result.serialization != windowPacingSerializationTime(windowSparsePaidBytes, result.rate) || result.serialization <= 0 {
					t.Fatal("covered serializer bypassed ordered paid original confirmation")
				}
				fixture.consume(t, result.item)
				sentAt := time.Unix(0, result.item.pacingSentAtNanos)
				if !sentAt.Equal(result.at) {
					t.Fatal("covered serializer did not start at the confirmed physical write")
				}
				if physicalFree.Before(sentAt) {
					physicalFree = sentAt
				}
				physicalFree = physicalFree.Add(serializer)
				arrivals[written] = arrival{item: result.item, sentAt: sentAt, arriveAt: physicalFree.Add(path)}
				written++
				maximumFlight = max(maximumFlight, written-acked)
				synctest.Wait()
				observed := windowSparsePaidReadService(service)
				if written-acked >= 3 && (observed.drained || observed.pendingWrites == 0 ||
					observed.sent-observed.total-observed.reservedByteCount < ByteCount(written-acked)*windowSparsePaidBytes) {
					t.Fatal("carrier lookahead lacked its real pending physical bytes")
				}
			case <-nextAck:
				// The loop checks the exact owned deadline before any new wait.
			case <-sequence.ctx.Done():
				t.Fatal("covered serializer exceeded the unchanged fixed virtual budget")
			}
		}
		<-done
		observed := windowSparsePaidReadService(service)
		if written != len(items) || observed.sent != observed.total || !observed.drained ||
			observed.pendingWrites != 0 || observed.pacingReservations != 0 || sequence.resendWriteCount.Load() != 0 {
			t.Fatal("covered serializer retained physical, reservation, or retry ownership")
		}
		if maximumFlight < 3 || coveredAt == 0 || decreaseProofAt == 0 {
			t.Fatalf("covered serializer never established its independent decrease premise: flight=%d covered=%t proof=%t", maximumFlight, coveredAt != 0, decreaseProofAt != 0)
		}
		if final.ServiceByteRate <= 0 || final.ServiceByteRate > 5500 {
			t.Fatalf("independent covered5000-byte/s serializer lost downward authority: %+v", final)
		}
	})
}
