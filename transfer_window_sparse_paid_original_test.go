// A ready source can be physically sparse because each small original pays
// its own serialization. Exact ACKs must not strand that source at an old hold.
package connect

import (
	"bytes"
	"context"
	"testing"
	"testing/synctest"
	"time"
)

const windowSparsePaidBytes ByteCount = 2102
const windowSparsePaidHold ByteCount = 11563

// Start the process-wide pool worker outside any synthetic clock bubble.
func windowSparsePaidPool(t *testing.T) {
	t.Helper()
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
}

type windowSparsePaidFixture struct {
	credit    *windowReceiverCreditFixture
	selector  *MultiRouteSelector
	transport Transport
	route     Route
	number    uint64
}

// The low hold is earned by two preoffered original envelopes and their exact
// ACKs. No rate, hold, timing state, or discovery flag is assigned by the test.
func newWindowSparsePaidFixture(t *testing.T) *windowSparsePaidFixture {
	t.Helper()
	start := time.Now()
	credit := newWindowReceiverCreditFixture(t, nil, start, 10*time.Millisecond)
	sequence := credit.sequence
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	sequence.ctx, sequence.cancel, sequence.log = ctx, cancel, NewNoopLogger()
	sequence.sendBufferSettings = DefaultSendBufferSettings()
	sequence.sendBufferSettings.ApplicationPacing = true
	sequence.flightController = newSendFlightController(sequence.sendBufferSettings)
	sequence.deliveredBytes = make([]deliveredBytesSample, deliveredBytesRingSize)
	sequence.client.ctx, sequence.client.clientId, sequence.client.log = ctx, NewId(), sequence.log
	sequence.destination = NewId()
	sequence.ackWindow = credit.ackWindow
	selector := NewMultiRouteSelector(ctx, "sparse-paid-original", nil, DestinationId(sequence.destination), true)
	route := make(Route, 1)
	transport := NewSendGatewayTransportWithType(TransportTypeH1)
	selector.updateTransport(transport, []Route{route})
	sequence.contractMultiRouteWriter = selector
	sequence.contractMultiRouteWriterDestination = DestinationId(sequence.destination)
	sequence.observeReceiveWindowAdvertisement(receiveAckMessage{receiveWindowSet: true, receiveWindowByteCount: 2 * 1024 * 1024})
	fixture := &windowSparsePaidFixture{credit: credit, selector: selector, transport: transport, route: route, number: 2}
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
		service := credit.service
		if service.sent != service.total || service.pendingWrites != 0 || service.pacingReservations != 0 ||
			service.reservedByteCount != 0 || service.waiterHead != nil || service.waiterTail != nil {
			t.Error("joined sparse fixture retained flight or pacing ownership")
		}
	})
	first := credit.write(0, windowSparsePaidBytes, start)
	last := credit.write(1, windowSparsePaidBytes, start)
	time.Sleep(10 * time.Millisecond)
	sequence.coalesceReceivedAck(credit.ackWindow, credit.ack(first, time.Now(), 0))
	// Floor the exact physical interval so integer byte-rate truncation is11563.
	time.Sleep(time.Duration(int64(windowSparsePaidBytes) * int64(time.Second) / int64(windowSparsePaidHold)))
	sequence.coalesceReceivedAck(credit.ackWindow, credit.ack(last, time.Now(), 0))
	estimate := sequence.sendWindowEstimate(time.Now())
	if estimate.ServiceByteRate != windowSparsePaidHold || !estimate.ServiceEstablished ||
		estimate.PacingDiscovery || credit.service.queueObservedAt.IsZero() ||
		credit.service.total != 2*windowSparsePaidBytes || !credit.service.drained {
		t.Fatalf("qualified sparse precondition: estimate=%+v total=%d drained=%t", estimate, credit.service.total, credit.service.drained)
	}
	return fixture
}

// Two bounded encodes retain one exact2102-byte wire envelope. Payload bytes
// are opaque fixture data: decoding below validates the envelope, not a peer.
func (self *windowSparsePaidFixture) item(t *testing.T) *sendItem {
	t.Helper()
	sequence := self.credit.sequence
	item := &sendItem{transferItem: transferItem{messageId: NewId(), sequenceNumber: self.number},
		sendTime: time.Now(), sendCount: 1, expectsAck: true, ackTimeout: time.Minute}
	self.number++
	frames, witnesses := transferGroupTestFrames(t, 1, int(windowSparsePaidBytes))
	payload := frames[0].MessageBytes
	defer func() {
		MessagePoolReturn(payload)
		releaseTransferGroupTestWitnesses(t, frames, witnesses)
	}()
	pack := &sendPackFrame{path: sendTransferPath(sequence.client.ClientId(), DestinationId(sequence.destination)),
		messageId: item.messageId, sequenceId: sequence.sequenceId, sequenceNumber: item.sequenceNumber,
		tagSendTime: uint64(item.sendTime.UnixMilli()), frames: frames}
	wire := marshalSendPackTransferFrame(pack)
	overhead := len(wire) - int(windowSparsePaidBytes)
	MessagePoolReturn(wire)
	if overhead <= 0 || len(payload)-overhead < 128 {
		t.Fatal("sparse envelope lost its stable payload length")
	}
	frames[0].MessageBytes = payload[:len(payload)-overhead]
	item.transferFrameBytes = marshalSendPackTransferFrame(pack)
	if len(item.transferFrameBytes) != int(windowSparsePaidBytes) {
		MessagePoolReturn(item.transferFrameBytes)
		t.Fatal("sparse envelope does not own exactly2102 bytes")
	}
	sequence.addResendItem(item)
	return item
}

func (self *windowSparsePaidFixture) consume(t *testing.T, item *sendItem) {
	t.Helper()
	if len(self.route) != 1 {
		t.Fatal("sparse carrier did not take exactly one physical share")
	}
	wire := <-self.route
	defer MessagePoolReturn(wire)
	if len(wire) != int(windowSparsePaidBytes) || !bytes.Equal(wire, item.transferFrameBytes) {
		t.Fatal("sparse carrier changed original wire ownership")
	}
	pack := decodeSendPackLifecycleWirePack(t, wire)
	if string(pack.MessageId) != string(item.messageId.Bytes()) {
		t.Fatal("sparse carrier changed original message identity")
	}
}

func (self *windowSparsePaidFixture) acknowledge(t *testing.T, item *sendItem, wait time.Duration) {
	t.Helper()
	service, sequence := self.credit.service, self.credit.sequence
	before := service.total
	ack := self.credit.ack(item, time.Now(), wait)
	sequence.coalesceReceivedAck(self.credit.ackWindow, ack)
	sequence.coalesceReceivedAck(self.credit.ackWindow, ack)
	if service.total != before+windowSparsePaidBytes || !item.serviceCreditObserved ||
		sequence.windowPacer.serviceAcked != service.total {
		t.Fatal("sparse duplicate ACK lost or repeated original byte ownership")
	}
}

// Copy the exact credited singleton after ACK publication has quiesced. Only
// pre-candidate fields are used, including when this file is compiled on old.
func windowSparsePaidSample(t *testing.T, service *windowPacingService, at time.Time) windowServiceSample {
	t.Helper()
	service.stateLock.Lock()
	defer service.stateLock.Unlock()
	interval := max(deliverySizedWindowSampleInterval, service.bucketInterval)
	bucket := at.UnixNano() / int64(interval)
	index := (bucket%int64(len(service.samples)) + int64(len(service.samples))) % int64(len(service.samples))
	sample := service.samples[index]
	if sample.bucket != bucket || sample.firstAtNanos != at.UnixNano() || sample.lastAtNanos != at.UnixNano() ||
		sample.bytes != windowSparsePaidBytes || sample.receiverBytes != sample.bytes ||
		!sample.supply.recorded || !sample.supply.complete || !sample.supply.headComplete ||
		sample.supply.earliestSentNanos <= 0 || sample.supply.earliestSentNanos != sample.supply.latestSentNanos {
		t.Fatalf("slow serializer lost its exact original sample: %+v", sample)
	}
	return sample
}

func (self *windowSparsePaidFixture) write(t *testing.T, item *sendItem) {
	t.Helper()
	sequence := self.credit.sequence
	disposition, err := sequence.writeMaybeWrappedBytes(item.transferFrameBytes, TransferPath{}, true, item, false, false)
	if err != nil {
		t.Fatal(err)
	}
	sequence.observeCarrierWrite(item, disposition)
	self.consume(t, item)
	if item.pacingByteCount != windowSparsePaidBytes || item.rttState != sendItemRttWriteConfirmed || !item.rttH1 {
		t.Fatal("sparse original bypassed paid H1 confirmation")
	}
}

type windowSparsePaidResult struct {
	item          *sendItem
	at            time.Time
	rate          ByteCount
	serialization time.Duration
	err           error
}

// One bounded producer uses the actual writer and enters its next reservation
// before the test publishes the prior10ms ACK. The test joins it on all exits.
func runWindowSparsePaidTrain(t *testing.T, idle, serializer time.Duration, changeGeneration, opaque bool) (SendWindowEstimate, ByteCount) {
	t.Helper()
	fixture := newWindowSparsePaidFixture(t)
	sequence, service := fixture.credit.sequence, fixture.credit.service
	if opaque {
		sequence.contractMultiRouteWriter = &fifoRepairCopyWriter{route: fixture.route,
			windowPacingPolicyWriter: windowPacingPolicyWriter{policy: transferFlightPolicySnapshot{generation: 7, h1Only: true}}}
	}
	var items [12]*sendItem
	for i := range items {
		items[i] = fixture.item(t)
	}
	results := make(chan windowSparsePaidResult)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for index, item := range items {
			if index > 0 && idle > 0 {
				select {
				case <-sequence.ctx.Done():
					return
				case <-time.After(idle):
				}
			}
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
	var final SendWindowEstimate
	lastRate := ByteCount(0)
	physicalFree := time.Time{}
	var previousSlow windowServiceSample
	for index := range items {
		var result windowSparsePaidResult
		select {
		case result = <-results:
		case <-sequence.ctx.Done():
			t.Fatal("sparse producer exceeded its fixed virtual budget")
		}
		if result.err != nil {
			t.Fatal(result.err)
		}
		fixture.consume(t, result.item)
		synctest.Wait()
		if index < len(items)-1 && serializer == 0 && !changeGeneration && !opaque {
			if idle == 0 {
				if service.pacingReservations != 1 || service.reservedByteCount != windowSparsePaidBytes ||
					service.waiterHead != &sequence.windowPacer.waiter || service.waiterTail != &sequence.windowPacer.waiter {
					t.Fatal("ready producer did not enter its next paid reservation before the prior ACK")
				}
			} else if service.pacingReservations != 0 || service.reservedByteCount != 0 {
				t.Fatal("source-idle control already had paid demand")
			}
		}
		if result.serialization != windowPacingSerializationTime(windowSparsePaidBytes, result.rate) || result.serialization <= 0 {
			t.Fatal("sparse original lost its paid serialization")
		}
		if changeGeneration {
			fixture.selector.updateTransport(fixture.transport, []Route{make(Route, 1)})
			fixture.selector.updateTransport(fixture.transport, []Route{fixture.route})
			synctest.Wait()
		}
		arrival := result.at.Add(10 * time.Millisecond)
		if serializer > 0 {
			if physicalFree.Before(result.at) {
				physicalFree = result.at
			}
			physicalFree = physicalFree.Add(serializer)
			arrival = physicalFree.Add(10 * time.Millisecond)
		}
		if time.Now().Before(arrival) {
			time.Sleep(time.Until(arrival))
		}
		fixture.acknowledge(t, result.item, 0)
		synctest.Wait()
		if idle == 0 && serializer == 0 && !changeGeneration && !opaque {
			reservations := 1
			if index == len(items)-1 {
				reservations = 0
			}
			if service.feedbackPending || !service.drained || service.total < service.drainedSent || service.pacingReservations != reservations {
				t.Fatal("a paid next reservation prevented exact physical drain or feedback completion")
			}
		}
		final = sequence.sendWindowSnapshot(time.Now())
		if serializer > 0 {
			current := windowSparsePaidSample(t, service, time.Now())
			if current.supply.minimumPath != serializer+10*time.Millisecond {
				t.Fatalf("underfilled serializer did not confirm its actual empty-flight baseline: %s", current.supply.minimumPath)
			}
			if previousSlow.bytes > 0 {
				span := current.lastAtNanos - previousSlow.lastAtNanos
				supply := current.supply
				supply.maximumGap = time.Duration(span)
				supply.receiverMaximumGap = time.Duration(span)
				interval := max(deliverySizedWindowSampleInterval, service.bucketInterval)
				if span <= int64(2*interval) || supply.lastQueue >= time.Duration(span) ||
					supply.latestSentNanos <= previousSlow.supply.headLastMinAtNanos-int64(supply.minimumPath) ||
					supply.qualifiedSpan(span, previousSlow.receiverLastAtNanos, previousSlow.supply.headLastMinAtNanos, interval, true) != 0 {
					t.Fatal("underfilled result handshake unexpectedly supplied a fully covered slow pair")
				}
			}
			previousSlow = current
		}
		if index == 0 && serializer == 0 {
			timing := service.roundTripEvidence(time.Now())
			if final.ServiceByteRate != windowSparsePaidHold || final.PacingByteRate != 12719 ||
				timing.minimum != 10*time.Millisecond || timing.latest != 10*time.Millisecond || final.ServiceBacklogged {
				t.Fatalf("healthy sparse baseline did not match trace: estimate=%+v timing=%+v", final, timing)
			}
		}
		lastRate = result.rate
	}
	<-done
	if service.sent != service.total || !service.drained || service.pacingReservations != 0 || sequence.resendWriteCount.Load() != 0 {
		t.Fatal("bounded original train retained physical or retry ownership")
	}
	if idle == 0 && serializer == 0 && !changeGeneration && !opaque && final.ServiceByteRate > windowSparsePaidHold {
		_, _, _, proof := service.measureCapacityEvidence(time.Second, time.Now(), false)
		if proof != 0 {
			t.Fatal("paced-offer growth acquired congestion-decrease authority")
		}
	}
	return final, lastRate
}

func TestWindowPacingSparsePaidOriginalRecovers(t *testing.T) {
	windowSparsePaidPool(t)
	synctest.Test(t, func(t *testing.T) {
		estimate, physicalRate := runWindowSparsePaidTrain(t, 0, 0, false, false)
		if estimate.ServiceByteRate <= windowSparsePaidHold || estimate.PacingByteRate <= 12719 || physicalRate <= 12719 {
			t.Fatalf("SPARSE_PAID_ORIGINAL_STALE_HOLD: service=%d pace=%d physical=%d", estimate.ServiceByteRate, estimate.PacingByteRate, physicalRate)
		}
	})
}

func TestWindowPacingSparsePaidSourceIdleDoesNotGrow(t *testing.T) {
	windowSparsePaidPool(t)
	synctest.Test(t, func(t *testing.T) {
		estimate, _ := runWindowSparsePaidTrain(t, 30*time.Millisecond, 0, false, false)
		if estimate.ServiceByteRate != windowSparsePaidHold {
			t.Fatalf("real source idle repriced the hold: %+v", estimate)
		}
	})
}

// The result handshake allows only one physical successor while waiting for
// each ACK. Its refreshed unloaded baseline leaves no full-span queue proof.
func TestWindowPacingSparsePaidUnderfilledSlowFlightHolds(t *testing.T) {
	windowSparsePaidPool(t)
	synctest.Test(t, func(t *testing.T) {
		estimate, _ := runWindowSparsePaidTrain(t, 0, 420400*time.Microsecond, false, false)
		if estimate.ServiceByteRate != windowSparsePaidHold {
			t.Fatalf("underfilled serializer repriced service without covered decrease proof: %+v", estimate)
		}
	})
}

func TestWindowPacingSparsePaidSameChannelGenerationDoesNotJoin(t *testing.T) {
	windowSparsePaidPool(t)
	synctest.Test(t, func(t *testing.T) {
		estimate, _ := runWindowSparsePaidTrain(t, 0, 0, true, false)
		if estimate.ServiceByteRate != windowSparsePaidHold {
			t.Fatalf("same channel across acquired generations supplied a false pair: %+v", estimate)
		}
	})
}

func TestWindowPacingSparsePaidOpaqueWriterDoesNotGrow(t *testing.T) {
	windowSparsePaidPool(t)
	synctest.Test(t, func(t *testing.T) {
		estimate, _ := runWindowSparsePaidTrain(t, 0, 0, false, true)
		if estimate.ServiceByteRate != windowSparsePaidHold {
			t.Fatalf("opaque generation supplied paid-route proof: %+v", estimate)
		}
	})
}

func TestWindowPacingSparsePaidHeldPrefixReleaseDoesNotGrow(t *testing.T) {
	windowSparsePaidPool(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newWindowSparsePaidFixture(t)
		sequence, service := fixture.credit.sequence, fixture.credit.service
		var last *sendItem
		for range 3 {
			last = fixture.item(t)
			fixture.write(t, last)
		}
		before := service.total
		time.Sleep(100 * time.Millisecond)
		ack := fixture.credit.ack(last, time.Now(), 90*time.Millisecond)
		sequence.coalesceReceivedAck(fixture.credit.ackWindow, ack)
		sequence.coalesceReceivedAck(fixture.credit.ackWindow, ack)
		estimate := sequence.sendWindowEstimate(time.Now())
		if service.total != before+3*windowSparsePaidBytes || service.receiverHeldPrefixAtNanos != time.Now().UnixNano() ||
			estimate.ServiceByteRate > windowSparsePaidHold || !sequence.windowPacer.receiverHeld {
			t.Fatalf("held cumulative release acquired faster service or lost ownership: %+v", estimate)
		}
	})
}

func TestWindowPacingSparsePaidAmbiguousRetryDoesNotGrow(t *testing.T) {
	windowSparsePaidPool(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newWindowSparsePaidFixture(t)
		sequence, service := fixture.credit.sequence, fixture.credit.service
		prime := fixture.item(t)
		fixture.write(t, prime)
		time.Sleep(10 * time.Millisecond)
		fixture.acknowledge(t, prime, 0)
		sequence.sendWindowEstimate(time.Now())
		for range 4 {
			item := fixture.item(t)
			fixture.write(t, item)
			physical, sent, roundTrip := item.pacingSentAtNanos, service.sent, service.lastRoundTrip
			if err := sequence.writeSelectedRecovery(item, sendRecoverySelection{kind: sendRecoverySelectiveGap}); err != nil {
				t.Fatal(err)
			}
			fixture.consume(t, item)
			time.Sleep(10 * time.Millisecond)
			fixture.acknowledge(t, item, 0)
			if item.sendCount != 1 || item.rttState != sendItemRttUnavailable || item.pacingSentAtNanos != physical ||
				service.sent != sent || service.lastRoundTrip != roundTrip || sequence.sendWindowEstimate(time.Now()).ServiceByteRate > windowSparsePaidHold {
				t.Fatal("actual selective copy acquired new original timing or faster service")
			}
		}
		if sequence.resendWriteCount.Load() != 4 {
			t.Fatal("ambiguous control did not dispatch its four actual copies")
		}
	})
}
