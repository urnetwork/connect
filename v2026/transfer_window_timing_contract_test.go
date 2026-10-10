// Physical acceptance and owner recovery selection use distinct clocks.
package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// The changed route accepts real retry copies at 400ms and 900ms. The old
// H1 waiter's timestamp grants neither a later H3 send nor a new H1 interval.
func TestWindowPacingChangedCarrierRecoveryUsesActualDispatch(t *testing.T) {
	for _, unreliable := range []bool{false, true} {
		runWindowRetryClockFixture(t, 4*time.Second, time.Minute, func(t *testing.T, fixture *windowRetryClockFixture) {
			head := fixture.sequence.resendQueue.PeekFirst()
			if head == nil || head.sendCount != 1 || !head.transportWriteObserved {
				t.Fatal("fixture lost its accepted initial head")
			}
			messageId, number := head.messageId, head.sequenceNumber
			lifetime := head.sendTime.Add(head.ackTimeout)
			if unreliable {
				// The existing datagram policy may extend, never shorten,
				// its lifetime; this test does not change that setting.
				lifetime = head.sendTime.Add(max(head.ackTimeout, fixture.sequence.sendBufferSettings.UnreliableAckTimeout))
			}
			previousH1 := fixture.sequence.windowPacer.waiter.sentAt
			resumeSecondDue := make(chan struct{})
			fixture.resumeSecondDue = resumeSecondDue
			// releaseInitial still owns the worker. This earlier policy hook
			// orders the 400ms inspection before the 900ms lifetime update.
			inspection := &windowOwnerInspection{ctx: fixture.sequence.ctx}
			arrived, resumeInspection := inspection.arm(fixture.start.Add(900 * time.Millisecond))
			fixture.sequence.beforeContractWriterAccessForTest = inspection.beforeWriterAccess
			fixture.delayRetry(1200 * time.Millisecond)
			time.Sleep(time.Until(fixture.start.Add(400 * time.Millisecond)))
			synctest.Wait()
			service := fixture.sequence.windowPacer.service
			next, probe := service.next, service.probeSent
			if service.pacingReservations != 1 || fixture.sequence.resendWriteCount.Load() != 0 {
				t.Fatal("fixture did not hold the first retry in H1 service")
			}
			h3 := &h3SendClientTransportForGroupTest{sendClientTransport: NewSendClientTransport(DestinationId(fixture.sequence.destination))}
			h3Route := make(Route, 8)
			fixture.otherRoutes = append(fixture.otherRoutes, h3Route)
			fixture.client.RouteManager().UpdateTransportWithProperties(h3, []Route{h3Route}, TransferCarrierProperties{Unreliable: unreliable})
			withdrawn := make(chan struct{})
			go func() {
				fixture.client.RouteManager().UpdateTransport(fixture.transport, nil)
				close(withdrawn)
			}()
			defer func() {
				fixture.cancel()
				if err := fixture.client.CloseAndWait(context.Background()); err != nil {
					t.Error(err)
				}
				<-withdrawn
			}()
			synctest.Wait()
			if fixture.sequence.transferFlightPolicy().h1Only {
				t.Fatal("fixture did not publish the H1 retirement")
			}
			accept := func(want time.Time, copies uint64) {
				t.Helper()
				// H3 was first published at 400ms; quiescence does not advance
				// this clock. Reading its ready route proves actual acceptance.
				if time.Now() != want || len(h3Route) != 1 || len(fixture.route) != 0 ||
					fixture.sequence.resendWriteCount.Load() != copies {
					t.Fatalf("unreliable=%t actual H3 acceptance: at=%s want=%s route=%d writes=%d",
						unreliable, time.Since(fixture.start), want.Sub(fixture.start), len(h3Route), fixture.sequence.resendWriteCount.Load())
				}
				wire := <-h3Route
				defer MessagePoolReturn(wire)
				pack := decodeSendPackLifecycleWirePack(t, wire)
				id, err := IdFromBytes(pack.MessageId)
				if err != nil || id != messageId || pack.SequenceNumber != number {
					t.Fatalf("H3 accepted another logical recovery: id=%s number=%d err=%v", id, pack.SequenceNumber, err)
				}
			}
			accept(fixture.start.Add(400*time.Millisecond), 1)
			head = fixture.sequence.resendQueue.GetByMessageId(messageId)
			if head == nil || head.sendCount != 2 || head.resendTime != fixture.start.Add(900*time.Millisecond) ||
				head.unreliableCarrierObserved != unreliable || !head.carrierChanged || head.carrierRoute != h3Route ||
				!head.sendTime.Add(head.ackTimeout).Equal(lifetime) || service.pacingReservations != 0 ||
				service.next != next || service.probeSent != probe || fixture.sequence.windowPacer.waiter.sentAt != previousH1 {
				t.Fatal("changed-carrier dispatch changed its original recovery, debt, lifetime or physical owner")
			}
			time.Sleep(time.Until(fixture.start.Add(900*time.Millisecond - time.Nanosecond)))
			synctest.Wait()
			if len(h3Route) != 0 || len(fixture.secondDue) != 0 || fixture.sequence.resendWriteCount.Load() != 1 {
				t.Fatal("changed carrier retried before its original 900ms boundary")
			}
			time.Sleep(time.Nanosecond)
			synctest.Wait()
			resumeWindowOwnerInspection(t, arrived, resumeInspection, fixture.start.Add(900*time.Millisecond))
			if len(fixture.secondDue) != 1 || len(h3Route) != 0 || fixture.sequence.resendWriteCount.Load() != 1 {
				t.Fatal("owner missed its exact due selection or bypassed the inspection barrier")
			}
			boundary := <-fixture.secondDue
			if boundary.at != fixture.start.Add(900*time.Millisecond) || boundary.deadline != boundary.at ||
				boundary.physical != previousH1 || boundary.copies != 2 || boundary.unreliable != unreliable || !boundary.carrierChanged {
				t.Fatalf("changed carrier borrowed a stale H1 clock: %+v", boundary)
			}
			close(resumeSecondDue)
			synctest.Wait()
			accept(fixture.start.Add(900*time.Millisecond), 2)
			t.Logf("unreliable=%t actual_h3_acceptances=400ms,900ms next_due_selection=900ms old_h1_waiter=%s",
				unreliable, previousH1.Sub(fixture.start))
		})
	}
}

// A real younger original can still owe shared serialization when the older
// accepted head reaches physical+2s. The owner must notice that due recovery;
// noticing it does not grant credit to emit an early physical H1 copy.
func TestWindowPacingYoungerAdmissionKeepsOlderRecoveryWake(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		destination := NewId()
		initialWritten := make(chan struct{})
		resumeInitial := make(chan struct{})
		producerDone := make(chan struct{})
		producerStarted := false
		var sequence *SendSequence
		var headId Id
		var selectedAt, selectedDeadline time.Time
		var selectedId Id
		selections := 0
		settings := DefaultClientSettings()
		settings.Log = NewNoopLogger()
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		settings.SendBufferSettings.WindowSizing = WindowSizingFromDelivery
		settings.SendBufferSettings.ApplyWindowSizing()
		if settings.SendBufferSettings.MinResendInterval != 2*time.Second || settings.SendBufferSettings.AckTimeout != time.Minute {
			t.Fatal("fixture lost its original recovery or acknowledgement lifetime")
		}
		settings.SendBufferSettings.afterInitialWriteQueuedForTest = func(id sendSequenceId, number uint64) {
			if id.Destination == destination && number == 0 {
				close(initialWritten)
				select {
				case <-ctx.Done():
				case <-resumeInitial:
				}
			}
		}
		settings.SendBufferSettings.beforeDueResendForTest = func(id sendSequenceId, number uint64) {
			if id.Destination != destination || number != 0 {
				return
			}
			selections++
			if item := sequence.resendQueue.GetByMessageId(headId); item != nil && selections == 1 {
				selectedAt, selectedDeadline, selectedId = time.Now(), item.resendTime, item.messageId
			}
		}
		client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
		route := make(Route, 64)
		defer func() {
			cancel()
			if producerStarted {
				<-producerDone
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
		frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(4000)}
		clear(frame.MessageBytes)
		if !client.SendWithTimeout(frame, destination, func(error) {}, -1) {
			MessagePoolReturn(frame.MessageBytes)
			t.Fatal("initial Pack was not admitted")
		}
		<-initialWritten
		sequence = client.sendBuffer.lookupSendSequence(sendSequenceId{Destination: destination}, nil)
		head := sequence.resendQueue.PeekFirst()
		if head == nil || head.sequenceNumber != 0 || !head.transportWriteObserved || !head.rttH1 || len(route) != 1 {
			t.Fatal("fixture did not accept exactly one real H1 head")
		}
		headId = head.messageId
		firstPhysical := sequence.firstPhysicalRecoveryTime(head)
		due, lifetime := head.resendTime, head.sendTime.Add(head.ackTimeout)
		inspection := &windowOwnerInspection{ctx: sequence.ctx}
		arrived, resumeInspection := inspection.arm(due)
		sequence.beforeContractWriterAccessForTest = inspection.beforeWriterAccess
		if due != firstPhysical.Add(2*time.Second) || lifetime != head.sendTime.Add(time.Minute) {
			t.Fatal("fixture lost its physical recovery anchor or original 60s lifetime")
		}
		wire := <-route
		func() {
			defer MessagePoolReturn(wire)
			pack := decodeSendPackLifecycleWirePack(t, wire)
			id, err := IdFromBytes(pack.MessageId)
			if err != nil || id != headId || pack.SequenceNumber != 0 {
				t.Fatal("physical head does not match its retained recovery identity")
			}
		}()
		service := sequence.windowPacer.service
		// Let the old burst expire naturally while its owner is held. Move
		// only the same service's existing next-serialization clock, as the
		// retry-clock fixture does; no due time, credit or ACK is invented.
		service.stateLock.Lock()
		burstExpiry := service.burst.start.Add(windowPacingBurstMaximumTime(service.burstEstimateTime))
		service.stateLock.Unlock()
		if !burstExpiry.After(time.Now()) || !burstExpiry.Before(due) {
			t.Fatal("fixture cannot expire its opening burst before recovery")
		}
		time.Sleep(time.Until(burstExpiry))
		service.stateLock.Lock()
		service.next = due.Add(time.Second)
		service.stateLock.Unlock()
		producerStarted = true
		go func() {
			defer close(producerDone)
			younger := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(4000)}
			clear(younger.MessageBytes)
			if !client.SendWithTimeout(younger, destination, func(error) {}, -1) {
				MessagePoolReturn(younger.MessageBytes)
			}
		}()
		close(resumeInitial)
		synctest.Wait()
		var younger *sendItem
		for _, item := range sequence.sendItems {
			if item.sequenceNumber == 1 {
				younger = item
			}
		}
		service.stateLock.Lock()
		chargedNext, chargedProbe := service.next, service.probeSent
		reservations, reserved := service.pacingReservations, service.reservedByteCount
		queued := service.waiterHead == &sequence.windowPacer.waiter
		service.stateLock.Unlock()
		if younger == nil || younger.transportWriteObserved || younger.pacingByteCount <= 0 ||
			!sequence.windowPacer.waiter.deadline.After(due) || reservations != 1 || reserved != younger.pacingByteCount || !queued ||
			len(route) != 0 || selections != 0 || sequence.resendWriteCount.Load() != 0 {
			t.Fatal("fixture did not place a real younger original across the older physical recovery boundary")
		}
		if sequence.resendQueue.GetByMessageId(headId) != head || head.resendTime != due ||
			head.sendTime.Add(head.ackTimeout) != lifetime || !sequence.lastCumulativeAckTime.IsZero() {
			t.Fatal("younger admission changed the older identity, lifetime, due time or delivery evidence")
		}
		time.Sleep(time.Until(due.Add(-time.Nanosecond)))
		synctest.Wait()
		if selections != 0 || len(route) != 0 || younger.transportWriteObserved || sequence.resendWriteCount.Load() != 0 {
			t.Fatal("recovery or physical service occurred before the unchanged due boundary")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		resumeWindowOwnerInspection(t, arrived, resumeInspection, due)
		service.stateLock.Lock()
		debtKept := !service.next.Before(chargedNext) && service.probeSent == chargedProbe
		service.stateLock.Unlock()
		selectionAfterPhysical := time.Duration(0)
		if !selectedAt.IsZero() {
			selectionAfterPhysical = selectedAt.Sub(firstPhysical)
		}
		t.Logf("first_physical=%s recovery_after_physical=%s now_after_physical=%s younger_deadline_after_physical=%s due_selections=%d selected_present=%t selected_after_physical=%s accepted_retry_count=%d retained_lifetime=%s debt_kept=%t",
			firstPhysical.Sub(head.sendTime), due.Sub(firstPhysical), time.Since(firstPhysical),
			sequence.windowPacer.waiter.deadline.Sub(firstPhysical), selections, !selectedAt.IsZero(), selectionAfterPhysical,
			sequence.resendWriteCount.Load(), lifetime.Sub(head.sendTime), debtKept)
		if len(route) != 0 || younger.transportWriteObserved || sequence.resendWriteCount.Load() != 0 || !debtKept ||
			sequence.resendQueue.GetByMessageId(younger.messageId) != younger || head.sendTime.Add(head.ackTimeout) != lifetime {
			t.Fatal("a due wake manufactured physical credit, lost younger ownership or changed the original lifetime")
		}
		if selections != 1 || selectedAt != due || selectedDeadline != due || selectedId != headId {
			t.Fatal("younger physical admission hid the older accepted head's unchanged recovery due wake")
		}
	})
}
