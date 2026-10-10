// Recovery is timed from the first accepted physical write, not application offer.
package connect

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Keep the original ten-message, missing-reply opening. This is an assertion-clock
// control, not evidence of a production recovery change: both live and the frozen
// startup candidate must recover at their own first physical write plus two
// seconds. A separate ongoing younger-write wait is not exercised by this row.
func TestH1AdmissionRecoveryPhysicalDeadline(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		destination := NewId()
		startWorker := make(chan struct{})
		enqueued := make(chan struct{})
		// The send owner copies one initial physical/recovery boundary.
		type initialBoundary struct {
			messageId Id
			at        time.Time
			created   time.Time
			physical  time.Time
			recovery  time.Time
			lifetime  time.Time
			bytes     ByteCount
			accepted  bool
		}
		var sequence *SendSequence
		// Wait publishes earlier owner work, not these reads to a later timer.
		// Only copied hook observations share this lock; sender state does not.
		type recoveryObservation struct {
			first                           initialBoundary
			initialReturns, dueSelections   int
			lastInitialPhysical             time.Time
			dueSelectedAt, selectedDeadline time.Time
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
		if settings.SendBufferSettings.MinResendInterval != 2*time.Second ||
			settings.SendBufferSettings.AckTimeout != time.Minute {
			t.Fatal("fixture no longer uses the original two-second recovery and minute ACK lifetime")
		}
		settings.SendBufferSettings.beforeRunSendSequenceForTest = func(id sendSequenceId) {
			if id.Destination == destination {
				select {
				case <-ctx.Done():
				case <-startWorker:
				}
			}
		}
		// Existing owner hooks only copy bounded state. They do not hold the
		// sender, log, receive feedback, force deadlines or refresh estimates.
		settings.SendBufferSettings.afterInitialWriteQueuedForTest = func(id sendSequenceId, number uint64) {
			if id.Destination != destination {
				return
			}
			stateLock.Lock()
			defer stateLock.Unlock()
			observation.initialReturns++
			for _, item := range sequence.sendItems {
				if item.sequenceNumber != number {
					continue
				}
				observation.lastInitialPhysical = sequence.firstPhysicalRecoveryTime(item)
				if number == 0 {
					observation.first = initialBoundary{
						messageId: item.messageId, at: time.Now(), created: item.sendTime,
						physical: observation.lastInitialPhysical, recovery: item.resendTime,
						lifetime: item.sendTime.Add(item.ackTimeout), bytes: item.pacingByteCount,
						accepted: item.transportWriteObserved && item.rttH1,
					}
				}
				return
			}
		}
		settings.SendBufferSettings.beforeDueResendForTest = func(id sendSequenceId, number uint64) {
			if id.Destination == destination && number == 0 {
				stateLock.Lock()
				defer stateLock.Unlock()
				observation.dueSelections++
				if observation.dueSelections == 1 {
					observation.dueSelectedAt = time.Now()
					observation.selectedDeadline = sequence.resendQueue.PeekFirst().resendTime
				}
			}
		}
		client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
		route := make(Route, 64)
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
		client.RouteManager().UpdateTransport(NewSendGatewayTransportWithType(TransportTypeH1), []Route{route})
		sequence = client.sendBuffer.createSendSequence(sendSequenceId{Destination: destination}, &SendPack{Destination: destination, Ctx: ctx})
		synctest.Wait()
		if sequence == nil || sequence.windowPacer.service == nil {
			t.Fatal("fixture did not create its real shared H1 pacing owner")
		}
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

		// The opening probe may already have admitted the first write. Only
		// an unaccepted first record needs the pending-reservation proof; do
		// not change live pacing merely to force both arms into that state.
		head := sequence.resendQueue.PeekFirst()
		observed := snapshot()
		first := observed.first
		if head == nil || head.sequenceNumber != 0 || observed.dueSelections != 0 {
			t.Fatal("fixture lost its retained first head before recovery inspection")
		}
		bytes := first.bytes
		alreadyAccepted := first.accepted
		if alreadyAccepted {
			if !head.transportWriteObserved || head.messageId != first.messageId || observed.initialReturns == 0 {
				t.Fatal("accepted first-write witness does not match the retained physical head")
			}
		} else {
			if head.transportWriteObserved || observed.initialReturns != 0 || len(route) != 0 {
				t.Fatal("unaccepted first write is not a pending original reservation")
			}
			bytes = head.pacingByteCount
			service.stateLock.Lock()
			reserved, reservations := service.reservedByteCount, service.pacingReservations
			service.stateLock.Unlock()
			if reserved != bytes || reservations != 1 {
				t.Fatalf("pending first reservation is not singly owned: bytes=%d reserved=%d reservations=%d", bytes, reserved, reservations)
			}
		}
		// Independently bound the first cold write by its actual encoded
		// bytes and unchanged 2510 B/s seed, not by an arbitrary extra grace.
		if bytes <= 0 || bytes > 64*1024 {
			t.Fatalf("unbounded initial physical byte count: %d", bytes)
		}
		serializationBound := time.Duration((int64(bytes)*int64(time.Second) + 2510 - 1) / 2510)
		if serializationBound >= 2*time.Second {
			t.Fatal("fixture's one-message serialization bound no longer fits its original recovery interval")
		}
		if !alreadyAccepted {
			if !time.Now().Before(start.Add(serializationBound)) {
				t.Fatal("pending initial write already exceeded its independent startup bound")
			}
			time.Sleep(time.Until(start.Add(serializationBound)))
			synctest.Wait()
		}
		observed = snapshot()
		first = observed.first
		if !first.accepted || first.bytes != bytes || first.physical.Before(start) ||
			first.physical.After(start.Add(serializationBound)) || first.at != first.physical {
			t.Fatalf("first accepted H1 write missed its unchanged byte/rate bound: %+v bound=%s", first, serializationBound)
		}
		if first.recovery != first.physical.Add(2*time.Second) || first.lifetime != first.created.Add(time.Minute) {
			t.Fatalf("initial physical retime changed recovery or original lifetime: %+v", first)
		}

		seenMessageIds := map[Id]bool{}
		originals, headRetries := 0, 0
		var originalBytes ByteCount
		drain := func() {
			for len(route) > 0 {
				wire := <-route
				func() {
					// This test takes each accepted route share and returns it
					// even if decoding or a semantic assertion fails.
					defer MessagePoolReturn(wire)
					pack := decodeSendPackLifecycleWirePack(t, wire)
					id, err := IdFromBytes(pack.MessageId)
					if err != nil {
						t.Fatal(err)
					}
					if seenMessageIds[id] {
						if id != first.messageId || pack.SequenceNumber != 0 {
							t.Fatal("a younger original retried before the head's first recovery")
						}
						headRetries++
						return
					}
					if pack.SequenceNumber == 0 && (id != first.messageId || ByteCount(len(wire)) != first.bytes) {
						t.Fatal("physical head identity or bytes differ from the owner witness")
					}
					seenMessageIds[id] = true
					originals++
					originalBytes += ByteCount(len(wire))
				}()
			}
		}
		time.Sleep(time.Until(first.recovery.Add(-time.Nanosecond)))
		synctest.Wait()
		drain()
		service.stateLock.Lock()
		reserved, reservations := service.reservedByteCount, service.pacingReservations
		pendingWaiter := service.waiterHead != nil || service.waiterTail != nil
		service.stateLock.Unlock()
		observed = snapshot()
		queued, _ := sequence.resendQueue.QueueSize()
		if originals != 10 || observed.initialReturns != 10 || headRetries != 0 || observed.dueSelections != 0 {
			t.Fatalf("before physical+2s: originals=%d returns=%d head_retries=%d due_selections=%d", originals, observed.initialReturns, headRetries, observed.dueSelections)
		}
		if reserved != 0 || reservations != 0 || pendingWaiter || sequence.windowPacer.serviceSent != originalBytes ||
			!observed.lastInitialPhysical.Before(first.recovery) || queued != 10 {
			t.Fatal("finite opening did not settle before recovery; this row cannot classify an ongoing younger-write wait")
		}
		if !sequence.lastCumulativeAckTime.IsZero() || sequence.resendWriteCount.Load() != 0 {
			t.Fatal("fixture gained delivery evidence or an early physical recovery")
		}

		time.Sleep(time.Nanosecond)
		synctest.Wait()
		drain()
		observed = snapshot()
		if observed.dueSelections != 1 || observed.dueSelectedAt != first.recovery || observed.selectedDeadline != first.recovery {
			t.Fatalf("owner missed the actual unchanged recovery deadline: selections=%d selected=%s stored=%s wanted=%s", observed.dueSelections, observed.dueSelectedAt.Sub(start), observed.selectedDeadline.Sub(start), first.recovery.Sub(start))
		}
		if headRetries != 1 || sequence.resendWriteCount.Load() != 1 || sequence.windowPacer.waiter.sentAt != first.recovery {
			t.Fatalf("ready H1 route did not accept exactly one head retry at physical+2s: retries=%d writes=%d physical=%s", headRetries, sequence.resendWriteCount.Load(), sequence.windowPacer.waiter.sentAt.Sub(start))
		}
		head = sequence.resendQueue.GetByMessageId(first.messageId)
		if head == nil || head.sendCount != 2 || head.sendTime.Add(head.ackTimeout) != first.lifetime {
			t.Fatal("first recovery lost retained identity, send count or original ACK lifetime")
		}
		t.Logf("first_inspection_already_accepted=%t first_physical=%s encoded_bytes=%d startup_bound=%s first_due=%s last_original=%s old_offer_plus_3s_already_due=%t actual_retry=%s original_lifetime=%s",
			alreadyAccepted, first.physical.Sub(start), first.bytes, serializationBound, first.recovery.Sub(start), observed.lastInitialPhysical.Sub(start),
			!first.recovery.After(start.Add(3*time.Second)), sequence.windowPacer.waiter.sentAt.Sub(start), first.lifetime.Sub(start))
	})
}
