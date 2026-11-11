// Actual acquired H1 writes retain source readiness through pipelined and
// compressed ACKs. These roots compile against the pre-correction API.
package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

// A second real writer shares service with the first, while owning its own
// sequence, route, pending reservation and exact first-write records.
func newWindowPipelinedReadySibling(t *testing.T, first *windowSparsePaidFixture) *windowSparsePaidFixture {
	t.Helper()
	credit := newWindowReceiverCreditFixture(t, first.credit.service, time.Now(), 10*time.Millisecond)
	sequence := credit.sequence
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	sequence.ctx, sequence.cancel, sequence.log = ctx, cancel, NewNoopLogger()
	sequence.sendBufferSettings = DefaultSendBufferSettings()
	sequence.sendBufferSettings.ApplicationPacing = true
	sequence.flightController = newSendFlightController(sequence.sendBufferSettings)
	sequence.deliveredBytes = make([]deliveredBytesSample, deliveredBytesRingSize)
	sequence.client.ctx, sequence.client.clientId, sequence.client.log = ctx, first.credit.sequence.client.clientId, sequence.log
	sequence.destination = first.credit.sequence.destination
	sequence.ackWindow = credit.ackWindow
	selector := NewMultiRouteSelector(ctx, "pipelined-ready-sibling", nil, DestinationId(sequence.destination), true)
	route := make(Route, 1)
	transport := NewSendGatewayTransportWithType(TransportTypeH1)
	selector.updateTransport(transport, []Route{route})
	sequence.contractMultiRouteWriter, sequence.contractMultiRouteWriterDestination = selector, DestinationId(sequence.destination)
	sequence.observeReceiveWindowAdvertisement(receiveAckMessage{receiveWindowSet: true, receiveWindowByteCount: 2 * 1024 * 1024})
	fixture := &windowSparsePaidFixture{credit: credit, selector: selector, transport: transport, route: route}
	t.Cleanup(func() {
		cancel()
		sequence.windowPacer.close()
		selector.Close()
		sequence.ackLifetimes.clear()
		for len(route) > 0 {
			MessagePoolReturn(<-route)
		}
		for _, item := range sequence.resendQueue.Clear() {
			item.messagePoolReturn()
		}
	})
	return fixture
}

// The ACK is the real confirmed head and covers only the actual newly offered
// prefix. It is repeated to check once-only credit, then copied under its owner.
func windowPipelinedReadyAck(t *testing.T, fixture *windowSparsePaidFixture, item *sendItem, count int) windowServiceSample {
	t.Helper()
	sequence, service := fixture.credit.sequence, fixture.credit.service
	state := windowSparsePaidReadService(service)
	// A distinct actual ACK turn prevents a later terminal control from
	// sharing the earlier checkpoint bucket. No sampler state is assigned.
	time.Sleep(max(deliverySizedWindowSampleInterval, state.bucketInterval))
	before := windowSparsePaidReadService(service).total
	at := time.Now()
	ack := fixture.credit.ack(item, at, 0)
	sequence.coalesceReceivedAck(fixture.credit.ackWindow, ack)
	sequence.coalesceReceivedAck(fixture.credit.ackWindow, ack)
	service.stateLock.Lock()
	defer service.stateLock.Unlock()
	if service.total != before+ByteCount(count)*windowSparsePaidBytes {
		t.Fatalf("compressed prefix lost or repeated physical bytes: got=%d want=%d", service.total-before, ByteCount(count)*windowSparsePaidBytes)
	}
	for _, sample := range service.samples {
		if sample.lastAtNanos == at.UnixNano() && sample.firstAtNanos == at.UnixNano() && sample.bytes == ByteCount(count)*windowSparsePaidBytes {
			if !sample.supply.recorded || !sample.supply.complete || !sample.supply.headComplete {
				t.Fatal("actual confirmed prefix did not retain its offered/head provenance")
			}
			return sample
		}
	}
	t.Fatal("actual ACK prefix did not produce its exact bounded sample")
	return windowServiceSample{}
}

// A later successful original is already physically outstanding when the
// earlier one- or two-original cumulative prefix arrives. No readiness state,
// service rate, offer timestamp or ACK-owned counter is assigned by the test.
func TestWindowPacingPipelinedOriginalAndCompressedPrefixKeepReadyProof(t *testing.T) {
	windowSparsePaidPool(t)
	// Both cases intentionally fail on old production. Isolate the outer
	// failure so synctest.Test cannot abort the remaining counterexample.
	for _, count := range []int{1, 2} {
		t.Run([]string{"singleton", "compressed-prefix"}[count-1], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fixture := newWindowSparsePaidFixture(t)
				sequence, service := fixture.credit.sequence, fixture.credit.service
				var items [3]*sendItem
				for i := range items {
					items[i] = fixture.item(t)
					fixture.write(t, items[i])
				}
				if sequence.pacingReadyWrite.confirmedId != items[2].messageId || items[count-1].messageId == items[2].messageId {
					t.Fatal("pipelined control did not retain a strictly later confirmed physical original")
				}
				time.Sleep(time.Millisecond)
				sample := windowPipelinedReadyAck(t, fixture, items[count-1], count)
				missing := sample.supply.readyAtNanos <= 0 || sample.supply.readyLane == (Id{})
				if sample.supply.earliestSentNanos != items[0].pacingSentAtNanos || sample.supply.latestSentNanos != items[count-1].pacingSentAtNanos {
					t.Fatal("compressed readiness borrowed another original's physical envelope")
				}
				time.Sleep(time.Millisecond)
				windowPipelinedReadyAck(t, fixture, items[2], len(items)-count)
				settled := windowSparsePaidReadService(service)
				if settled.sent != settled.total || settled.reservedByteCount != 0 || settled.pacingReservations != 0 || !settled.drained {
					t.Fatal("confirmed prefix/root left physical custody after real terminal ACKs")
				}
				if missing {
					t.Errorf("PIPELINED_READY_PREFIX_LOST: count=%d current_tail=%d acknowledged_head=%d", count, items[2].sequenceNumber, items[count-1].sequenceNumber)
				}
			})
		})
	}
}

// An actual source idle or acquired-generation change breaks the prefix run.
// The earlier receipt still repays its own bytes and the later original lives.
func TestWindowPacingPipelinedSourceIdleAndGenerationDoNotBorrowReadyProof(t *testing.T) {
	windowSparsePaidPool(t)
	for _, changeGeneration := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			fixture := newWindowSparsePaidFixture(t)
			first := fixture.item(t)
			fixture.write(t, first)
			if changeGeneration {
				fixture.selector.updateTransport(fixture.transport, []Route{make(Route, 1)})
				fixture.selector.updateTransport(fixture.transport, []Route{fixture.route})
			} else {
				time.Sleep(2*deliverySizedWindowSampleInterval + time.Nanosecond)
			}
			second := fixture.item(t)
			fixture.write(t, second)
			time.Sleep(time.Millisecond)
			old := windowPipelinedReadyAck(t, fixture, first, 1)
			if old.supply.readyAtNanos != 0 || old.supply.readyLane != (Id{}) {
				t.Fatal("an expired source run lent readiness to an earlier outstanding original")
			}
			time.Sleep(time.Millisecond)
			windowPipelinedReadyAck(t, fixture, second, 1)
		})
	}
}

// Distinct SendSequences share the same service, but cannot compose one
// source-ready train merely because the sampler places their credits together.
func TestWindowPacingSharedServiceDoesNotJoinSiblingReadyRuns(t *testing.T) {
	windowSparsePaidPool(t)
	synctest.Test(t, func(t *testing.T) {
		first := newWindowSparsePaidFixture(t)
		second := newWindowPipelinedReadySibling(t, first)
		if first.credit.service != second.credit.service || first.credit.sequence.sequenceId == second.credit.sequence.sequenceId {
			t.Fatal("sibling control lost its actual shared-service/separate-sequence premise")
		}
		var samples [2]windowServiceSample
		for i, fixture := range []*windowSparsePaidFixture{first, second} {
			var items [3]*sendItem
			for j := range items {
				items[j] = fixture.item(t)
				fixture.write(t, items[j])
			}
			time.Sleep(time.Millisecond)
			samples[i] = windowPipelinedReadyAck(t, fixture, items[1], 2)
			time.Sleep(time.Millisecond)
			windowPipelinedReadyAck(t, fixture, items[2], 1)
		}
		if samples[0].supply.readyAtNanos <= 0 || samples[1].supply.readyAtNanos <= 0 || samples[0].supply.readyLane == samples[1].supply.readyLane {
			t.Fatal("actual independent ready runs were not observed")
		}
		merged := samples[0].coverage()
		merged.merge(samples[1].coverage())
		if merged.supply.readyAtNanos != 0 || merged.supply.readyLane != (Id{}) || merged.bytes != 4*windowSparsePaidBytes {
			t.Fatal("a common service merged distinct sources' ready demand or lost exact byte credit")
		}
	})
}

// A quality reset cannot reuse old ready demand, but a wholly new continuous
// run must recover its own proof even if the application remains busy.
func TestWindowPacingReadyRunRestartsAfterRealQualityBoundary(t *testing.T) {
	windowSparsePaidPool(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newWindowSparsePaidFixture(t)
		sequence, service := fixture.credit.sequence, fixture.credit.service
		old := fixture.item(t)
		fixture.write(t, old)
		service.networkQualityChanged(time.Now())
		boundary := service.qualityGeneration()
		time.Sleep(time.Nanosecond)
		var items [3]*sendItem
		for i := range items {
			items[i] = fixture.item(t)
			fixture.write(t, items[i])
		}
		time.Sleep(time.Millisecond)
		before := windowSparsePaidReadService(service).total
		oldAt := time.Now()
		oldAck := fixture.credit.ack(old, oldAt, 0)
		sequence.coalesceReceivedAck(fixture.credit.ackWindow, oldAck)
		sequence.coalesceReceivedAck(fixture.credit.ackWindow, oldAck)
		if windowSparsePaidReadService(service).total != before+windowSparsePaidBytes {
			t.Fatal("old quality-epoch ACK lost once-only physical repayment")
		}
		service.stateLock.Lock()
		for _, sample := range service.samples {
			if sample.lastAtNanos == oldAt.UnixNano() && sample.supply.readyAtNanos > 0 {
				t.Error("quality epoch borrowed the old run")
			}
		}
		service.stateLock.Unlock()
		time.Sleep(time.Millisecond)
		fresh := windowPipelinedReadyAck(t, fixture, items[1], 2)
		if fresh.supply.readyAtNanos <= boundary.UnixNano() || fresh.supply.readyLane == (Id{}) {
			t.Fatal("wholly new ready originals did not establish fresh-epoch demand")
		}
		time.Sleep(time.Millisecond)
		windowPipelinedReadyAck(t, fixture, items[2], 1)
		if sequence.resendWriteCount.Load() != 0 {
			t.Fatal("quality control used recovery to manufacture original readiness")
		}
	})
}

// Optional pacing cannot add a service-lock dependency to an unpaced write.
// This reset-only unit control deliberately starts with stale ready metadata.
func TestWindowPacingDisabledWriteClearsReadyWithoutServiceLock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newWindowReceiverCreditFixture(t, nil, time.Now(), 10*time.Millisecond)
		sequence, service := fixture.sequence, fixture.service
		sequence.sendBufferSettings = DefaultSendBufferSettings()
		if sequence.sendBufferSettings.ApplicationPacing {
			t.Fatal("default-off control was unexpectedly paced")
		}
		sequence.pacingReadyWrite = windowPacingReadyWrite{
			pendingId: NewId(), confirmedId: NewId(), route: make(Route), lane: NewId(),
		}
		item := &sendItem{transferItem: transferItem{messageId: NewId()}}
		service.stateLock.Lock()
		sequence.finishPacingReadyWrite(item, transferWriteDisposition{}, nil, 0)
		service.stateLock.Unlock()
		if sequence.pacingReadyWrite != (windowPacingReadyWrite{}) {
			t.Fatal("unpaced reset depended on service quality or retained a stale ready owner")
		}
		if service.sent != 0 || service.total != 0 || item.pacingByteCount != 0 || item.serviceCreditObserved {
			t.Fatal("unpaced reset manufactured physical service ownership")
		}
	})
}
