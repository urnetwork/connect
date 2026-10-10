package connect

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// An opening admission policy must leave the owning sequence free to run
// recovery and notice route replacement. Recovery starts at actual H1
// acceptance; deliberately missing ACKs provide no credit oracle to the sender.
func TestWindowPacingH1CreditAdmissionCannotStarveResend(t *testing.T) {
	testWindowPacingH1AdmissionOwnerProgress(t, false)
}

func TestWindowPacingH1CreditAdmissionCannotHideRouteChange(t *testing.T) {
	testWindowPacingH1AdmissionOwnerProgress(t, true)
}

func testWindowPacingH1AdmissionOwnerProgress(t *testing.T, changeRoute bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		destination := NewId()
		startWorker := make(chan struct{})
		type initialWriteBoundary struct {
			messageId                                 Id
			at, created, physical, recovery, lifetime time.Time
			bytes                                     ByteCount
			accepted                                  bool
		}
		firstWritten := make(chan initialWriteBoundary, 1)
		var sequence *SendSequence
		// Copy hook observations under their own lock. Fake-time advancement
		// does not publish a test read to a future owner callback.
		type recoveryObservation struct {
			initialWrites, dueInspections                       int
			lastInitialPhysical, inspectedAt, inspectedDeadline time.Time
		}
		var stateLock sync.Mutex
		var observation recoveryObservation
		snapshot := func() recoveryObservation {
			stateLock.Lock()
			defer stateLock.Unlock()
			return observation
		}
		settings := DefaultClientSettings()
		settings.Log = NewNoopLogger()
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		settings.SendBufferSettings.WindowSizing = WindowSizingFromDelivery
		settings.SendBufferSettings.ApplyWindowSizing()
		settings.SendBufferSettings.beforeRunSendSequenceForTest = func(id sendSequenceId) {
			if id.Destination == destination {
				select {
				case <-ctx.Done():
				case <-startWorker:
				}
			}
		}
		if !changeRoute {
			if settings.SendBufferSettings.MinResendInterval != 2*time.Second || settings.SendBufferSettings.AckTimeout != time.Minute {
				t.Fatal("fixture changed the original recovery interval or ACK lifetime")
			}
			// Copy the existing owner's post-write boundary; neither hook holds
			// its worker, supplies feedback, or changes a deadline.
			settings.SendBufferSettings.afterInitialWriteQueuedForTest = func(id sendSequenceId, number uint64) {
				if id.Destination != destination {
					return
				}
				stateLock.Lock()
				defer stateLock.Unlock()
				observation.initialWrites++
				for _, item := range sequence.sendItems {
					if item.sequenceNumber == number {
						observation.lastInitialPhysical = sequence.firstPhysicalRecoveryTime(item)
						if number == 0 {
							firstWritten <- initialWriteBoundary{
								messageId: item.messageId, at: time.Now(), created: item.sendTime,
								physical: observation.lastInitialPhysical, recovery: item.resendTime,
								lifetime: item.sendTime.Add(item.ackTimeout), bytes: item.pacingByteCount,
								accepted: item.transportWriteObserved && item.rttH1,
							}
						}
						return
					}
				}
			}
			settings.SendBufferSettings.beforeDueResendForTest = func(id sendSequenceId, number uint64) {
				if id.Destination == destination && number == 0 {
					stateLock.Lock()
					defer stateLock.Unlock()
					observation.dueInspections++
					observation.inspectedAt, observation.inspectedDeadline = time.Now(), sequence.resendQueue.PeekFirst().resendTime
				}
			}
		}
		client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
		route := make(Route, 64)
		enqueued := make(chan struct{})
		producerStarted := false
		defer func() {
			cancel()
			if producerStarted {
				<-enqueued
			}
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
			for len(route) > 0 {
				MessagePoolReturn(<-route)
			}
		}()
		client.ContractManager().AddNoContractPeer(destination)
		transport := NewSendGatewayTransportWithType(TransportTypeH1)
		client.RouteManager().UpdateTransport(transport, []Route{route})
		sequence = client.sendBuffer.createSendSequence(sendSequenceId{Destination: destination}, &SendPack{Destination: destination, Ctx: ctx})
		synctest.Wait()
		service := sequence.windowPacer.service
		service.queueObservedAt = time.Now().Add(-time.Second)
		service.heldPacingRate = 2510
		sequence.windowPacer.rate, sequence.windowPacer.estimateRate = 2510, 183
		sequence.windowPacer.probeRate, sequence.windowPacer.probeLimit = 147928994, 2958580
		sequence.windowPacer.rateUpdated = time.Now()
		producerStarted = true
		go func() {
			defer close(enqueued)
			for range 10 {
				frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(4000)}
				if !client.SendWithTimeout(frame, destination, func(error) {}, -1) {
					MessagePoolReturn(frame.MessageBytes)
					return
				}
			}
		}()
		start := time.Now()
		close(startWorker)
		synctest.Wait()
		time.Sleep(10 * time.Millisecond)
		synctest.Wait()
		seen := map[Id]bool{}
		var head Id
		initial, retries := 0, 0
		var originalBytes ByteCount
		drain := func() {
			for len(route) > 0 {
				wire := <-route
				pack := decodeSendPackLifecycleWirePack(t, wire)
				id, err := IdFromBytes(pack.MessageId)
				wireBytes := ByteCount(len(wire))
				MessagePoolReturn(wire)
				if err != nil {
					t.Fatal(err)
				}
				if seen[id] {
					if id != head || pack.SequenceNumber != 0 {
						t.Fatal("younger original retried before the head's first recovery")
					}
					retries++
				} else {
					if initial == 0 {
						head = id
					}
					seen[id] = true
					initial++
					originalBytes += wireBytes
				}
			}
		}
		drain()
		if changeRoute {
			before := sequence.transferFlightPolicy()
			client.RouteManager().UpdateTransport(transport, nil)
			client.RouteManager().UpdateTransport(NewSendGatewayTransportWithType(TransportTypeH3), []Route{route})
			synctest.Wait()
			after := sequence.transferFlightPolicy()
			if after.h1Only || after.generation == before.generation {
				t.Fatal("fixture did not replace the H1-only route generation")
			}
			time.Sleep(time.Until(start.Add(100 * time.Millisecond)))
			synctest.Wait()
			drain()
			t.Logf("initial physical messages after H1 retirement=%d", initial)
			if initial != 10 {
				t.Error("H1-only admission wait ignored the replacement reliable route generation")
			}
			cancel()
			<-enqueued
			return
		}
		retained := sequence.resendQueue.PeekFirst()
		observed := snapshot()
		if retained == nil || retained.sequenceNumber != 0 || retained.pacingByteCount <= 0 || retained.pacingByteCount > 64*1024 || observed.dueInspections != 0 {
			t.Fatal("fixture lost its bounded first original before recovery")
		}
		if !retained.transportWriteObserved {
			service.stateLock.Lock()
			owned := service.pacingReservations == 1 && service.reservedByteCount == retained.pacingByteCount &&
				service.waiterHead == &sequence.windowPacer.waiter && service.waiterTail == &sequence.windowPacer.waiter
			service.stateLock.Unlock()
			if !owned || observed.initialWrites != 0 || initial != 0 {
				t.Fatal("first original did not retain its single charged H1 reservation")
			}
		}
		// Bound the first acceptance independently by the unchanged encoded
		// bytes/rate, then test its recovery clock rather than offer+3s.
		serializationBound := time.Duration((int64(retained.pacingByteCount)*int64(time.Second) + 2510 - 1) / 2510)
		if serializationBound >= 2*time.Second {
			t.Fatal("first-write serialization no longer fits the original recovery interval")
		}
		var first initialWriteBoundary
		select {
		case first = <-firstWritten:
		case <-time.After(time.Until(start.Add(serializationBound))):
			t.Fatal("first H1 physical acceptance exceeded its independent byte/rate bound")
		}
		synctest.Wait()
		if !first.accepted || first.messageId != retained.messageId || first.bytes != retained.pacingByteCount ||
			first.at != first.physical || first.physical.Before(start) || first.physical.After(start.Add(serializationBound)) ||
			first.recovery != first.physical.Add(2*time.Second) || first.lifetime != first.created.Add(time.Minute) {
			t.Fatalf("initial H1 acceptance changed its physical recovery or original lifetime: %+v", first)
		}
		time.Sleep(time.Until(first.recovery.Add(-time.Nanosecond)))
		synctest.Wait()
		drain()
		service.stateLock.Lock()
		settled := service.pacingReservations == 0 && service.reservedByteCount == 0 && service.waiterHead == nil && service.waiterTail == nil
		next, probe := service.next, service.probeSent
		service.stateLock.Unlock()
		observed = snapshot()
		queued, _ := sequence.resendQueue.QueueSize()
		if initial != 10 || observed.initialWrites != 10 || head != first.messageId || retries != 0 || observed.dueInspections != 0 ||
			!settled || !observed.lastInitialPhysical.Before(first.recovery) || queued != 10 ||
			sequence.windowPacer.serviceSent != originalBytes || !sequence.lastCumulativeAckTime.IsZero() || sequence.resendWriteCount.Load() != 0 {
			t.Fatal("finite ten-message opening lost ownership or retried before physical+2s")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		drain()
		retained = sequence.resendQueue.GetByMessageId(first.messageId)
		service.stateLock.Lock()
		keptDebt := !service.next.Before(next) && service.probeSent >= probe && service.pacingReservations == 0 && service.reservedByteCount == 0
		service.stateLock.Unlock()
		observed = snapshot()
		if retries != 1 || observed.dueInspections != 1 || observed.inspectedAt != first.recovery || observed.inspectedDeadline != first.recovery ||
			sequence.resendWriteCount.Load() != 1 || sequence.windowPacer.waiter.sentAt != first.recovery || !keptDebt ||
			retained == nil || retained.sendCount != 2 || retained.sendTime.Add(retained.ackTimeout) != first.lifetime {
			t.Fatal("fresh-data credit wait blocked exact physical+2s head recovery or changed its original ownership/lifetime")
		}
		t.Logf("initial physical messages=%d first_physical=%s first_due=%s actual_retry=%s original_lifetime=%s old_offer_plus_3s_already_due=%t",
			initial, first.physical.Sub(start), first.recovery.Sub(start), sequence.windowPacer.waiter.sentAt.Sub(start),
			first.lifetime.Sub(first.created), !first.recovery.After(start.Add(3*time.Second)))
		cancel()
		<-enqueued
	})
}
