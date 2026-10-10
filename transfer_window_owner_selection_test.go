// Selected recovery is metadata only; the original writer remains the sole
// physical continuation and every real ACK/expiry still owns its item.
package connect

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
	"unsafe"

	"github.com/urnetwork/connect/protocol"
)

// Canceling after selection releases the one original reservation without
// refunding either serialization or opening credit, or returning a second wire.
func TestWindowPacingOwnerSelectionCancelKeepsDebt(t *testing.T) {
	runWindowOwnerProgressFixture(t, false, false, time.Second, func(t *testing.T, fixture *windowOwnerProgressFixture) {
		time.Sleep(time.Until(fixture.due))
		synctest.Wait()
		sequence := fixture.sequence
		selection := sequence.pendingRecovery
		if selection == nil || selection.messageId != fixture.head.messageId ||
			selection.at != fixture.due || selection.due != fixture.due || selection.kind != sendRecoveryNone ||
			sequence.selectedRecoveryItem() != fixture.head {
			t.Fatal("real due selection did not retain exactly the older recovery identity")
		}
		service := sequence.windowPacer.service
		fixture.cancel()
		if err := fixture.client.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
		service.stateLock.Lock()
		kept := service.next == fixture.chargedNext && service.probeSent == fixture.chargedProbe &&
			service.pacingReservations == 0 && service.reservedByteCount == 0 &&
			service.waiterHead == nil && service.waiterTail == nil && !service.drained
		service.stateLock.Unlock()
		if !kept || sequence.pendingRecovery != nil || len(fixture.route) != 0 ||
			sequence.resendWriteCount.Load() != 0 || len(sequence.sendItems) != 0 {
			t.Fatal("canceled selection leaked ownership, dispatched or manufactured service credit")
		}
	})
}

// Pending selective and cumulative ACKs invalidate a selected copy immediately,
// but remain in the ordinary ACK owner. Duplicate publication credits bytes once.
func TestWindowPacingOwnerSelectionAckInvalidatesCopy(t *testing.T) {
	for _, selective := range []bool{false, true} {
		runWindowOwnerProgressFixture(t, false, false, time.Second, func(t *testing.T, fixture *windowOwnerProgressFixture) {
			time.Sleep(time.Until(fixture.due))
			synctest.Wait()
			sequence, head, younger := fixture.sequence, fixture.head, fixture.younger
			arrived, resume := fixture.inspection.arm(fixture.youngerDeadline)
			if sequence.pendingRecovery == nil || sequence.pendingRecovery.messageId != head.messageId {
				t.Fatal("fixture did not select the real older retry")
			}
			headId, headBytes := head.messageId, head.pacingByteCount
			ack := &protocol.Ack{MessageId: headId.Bytes(), SequenceId: sequence.sequenceId.Bytes(), Selective: selective}
			service := sequence.windowPacer.service
			for range 2 {
				if ok, err := sequence.Ack(ack, 0); !ok || err != nil {
					t.Fatalf("real older ACK was refused: %t %v", ok, err)
				}
				synctest.Wait()
				service.stateLock.Lock()
				kept := service.next == fixture.chargedNext && service.probeSent == fixture.chargedProbe &&
					service.pacingReservations == 1 && service.reservedByteCount == younger.pacingByteCount &&
					service.total == headBytes && service.waiterHead == &sequence.windowPacer.waiter
				service.stateLock.Unlock()
				if !kept || sequence.pendingRecovery != nil || len(fixture.route) != 0 ||
					!sequence.ackWindow.PendingDispositionFor(head.sequenceNumber, headId) ||
					head.deliveryObserved || younger.transportWriteObserved {
					t.Fatal("selected-copy invalidation consumed delivery ownership or duplicated service credit")
				}
			}
			time.Sleep(time.Until(fixture.youngerDeadline))
			synctest.Wait()
			resumeWindowOwnerInspection(t, arrived, resume, fixture.youngerDeadline)
			if len(fixture.written) != 1 || <-fixture.written != younger.sequenceNumber || len(fixture.route) != 1 {
				t.Fatal("real ACK lost the still-charged younger continuation")
			}
			wire := <-fixture.route
			pack := decodeSendPackLifecycleWirePack(t, wire)
			MessagePoolReturn(wire)
			if string(pack.MessageId) != string(younger.messageId[:]) {
				t.Fatal("stale selected copy took the younger FIFO slot")
			}
			close(fixture.resumeYounger)
			synctest.Wait()
			retained := sequence.resendQueue.GetByMessageId(headId)
			if selective && (retained == nil || !retained.selectiveAcked) ||
				!selective && retained != nil || sequence.pendingRecovery != nil ||
				sequence.resendWriteCount.Load() != 0 || len(fixture.route) != 0 {
				t.Fatalf("selective=%t ordinary ACK owner did not preempt the selected physical copy", selective)
			}
		})
	}
}

// Publication already credited the physical older envelope. Teardown must join
// that admitted publisher before retiring selected metadata and original buffers.
func TestWindowPacingOwnerSelectionAckCancelPublicationRace(t *testing.T) {
	runWindowOwnerProgressFixture(t, false, false, time.Second, func(t *testing.T, fixture *windowOwnerProgressFixture) {
		time.Sleep(time.Until(fixture.due))
		synctest.Wait()
		sequence := fixture.sequence
		if sequence.pendingRecovery == nil {
			t.Fatal("fixture did not select an older recovery")
		}
		headId, headBytes := fixture.head.messageId, fixture.head.pacingByteCount
		release := make(chan struct{})
		fixture.releaseAck = release
		releasePublisher := func() {
			select {
			case <-release:
			default:
				close(release)
			}
		}
		defer releasePublisher()
		ackDone := make(chan error, 1)
		go func() {
			ok, err := sequence.Ack(&protocol.Ack{MessageId: headId.Bytes(), SequenceId: sequence.sequenceId.Bytes(), Selective: true}, 0)
			if !ok && err == nil {
				err = errors.New("ACK publisher was not admitted")
			}
			ackDone <- err
		}()
		<-fixture.ackPublished
		synctest.Wait()
		service := sequence.windowPacer.service
		service.stateLock.Lock()
		published := service.total == headBytes && service.reservedByteCount == fixture.younger.pacingByteCount
		service.stateLock.Unlock()
		if !published || sequence.pendingRecovery != nil || len(ackDone) != 0 {
			t.Fatal("barrier did not hold a real credited ACK after invalidating the selected copy")
		}
		closeWaitEntered := make(chan struct{})
		fixture.client.sendBuffer.beforeCloseWaitForTest = func(id sendSequenceId) {
			if id == sequence.id() {
				close(closeWaitEntered)
			}
		}
		defer func() { fixture.client.sendBuffer.beforeCloseWaitForTest = nil }()
		fixture.cancel()
		closed := make(chan error, 1)
		go func() { closed <- fixture.client.CloseAndWait(context.Background()) }()
		// Teardown's ackMutex wait is not durable synctest blocking. Observe
		// its existing join boundary directly before releasing the publisher.
		<-closeWaitEntered
		if sequence.ctx.Err() == nil {
			t.Fatal("close join boundary did not cancel the active sequence")
		}
		if len(closed) != 0 {
			t.Fatal("Client teardown escaped its admitted ACK publisher")
		}
		releasePublisher()
		if err := <-ackDone; err != nil {
			t.Fatal(err)
		}
		if err := <-closed; err != nil {
			t.Fatal(err)
		}
		service.stateLock.Lock()
		balanced := service.total == headBytes && service.sent == headBytes &&
			service.pacingReservations == 0 && service.reservedByteCount == 0 &&
			service.next == fixture.chargedNext && service.probeSent == fixture.chargedProbe
		service.stateLock.Unlock()
		if !balanced || sequence.pendingRecovery != nil || len(fixture.route) != 0 {
			t.Fatal("publication/cancellation race lost credited bytes or refunded a selected/original reservation")
		}
	})
}

// Concrete H1, H3 and non-direct P2P writers share one absolute 200ms operation.
// A real older physical write becomes due halfway through that blocked original;
// selection neither restarts its budget nor repeats its wire observation.
func TestWindowPacingOwnerSelectionWriterBudgetIsAbsolute(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, row := range []struct {
		carrier      TransportType
		unreliable   bool
		reliableOnly bool
		noAck        bool
	}{
		{carrier: TransportTypeH1},
		{carrier: TransportTypeH3},
		{carrier: TransportTypeH3, unreliable: true},
		{carrier: TransportTypeP2p},
		{carrier: TransportTypeP2p, unreliable: true},
		{carrier: TransportTypeH3, unreliable: true, reliableOnly: true},
		{carrier: TransportTypeP2p, unreliable: true, reliableOnly: true},
		{carrier: TransportTypeH1, noAck: true},
	} {
		synctest.Test(t, func(t *testing.T) {
			sequence, older, selector, route, observations, state := newWindowRouteAccountingFixture(t, 1)
			sequence.sendBufferSettings.WriteTimeout = 200 * time.Millisecond
			older.ackTimeout = time.Minute
			var h1 Transport
			selector.mutex.Lock()
			for transport := range selector.transportRoutes {
				h1 = transport
			}
			selector.mutex.Unlock()
			if row.carrier != TransportTypeH1 {
				selector.updateTransport(h1, nil)
				selector.updateTransportWithProperties(NewSendGatewayTransportWithType(row.carrier), []Route{route},
					TransferCarrierProperties{Unreliable: row.unreliable})
			}
			sequence.flightController.applyPolicy(selector.transferFlightPolicy())
			disposition, err := sequence.writeMaybeWrappedBytes(older.transferFrameBytes, TransferPath{}, true, older, false, false)
			if err != nil || disposition.transportType != row.carrier || disposition.unreliable != row.unreliable || len(route) != 1 {
				t.Fatalf("fixture failed actual older carrier: carrier=%s unreliable=%t err=%v", row.carrier, row.unreliable, err)
			}
			older.transportWriteObserved = true
			sequence.observeCarrierWrite(older, disposition)
			physical := time.Now()
			sequence.setResendTime(older, physical.Add(sequence.resendIntervalForItem(older, 1)))
			due, lifetime := older.resendTime, older.sendTime.Add(older.ackTimeout)
			inspection := &windowOwnerInspection{ctx: sequence.ctx}
			arrived, resume := inspection.arm(due)
			sequence.beforeContractWriterAccessForTest = inspection.beforeWriterAccess
			if row.reliableOnly {
				selector.updateTransport(h1, []Route{make(Route)})
			}
			time.Sleep(time.Until(due.Add(-100 * time.Millisecond)))
			start := time.Now()
			younger := &sendItem{transferItem: transferItem{messageId: NewId(), sequenceNumber: older.sequenceNumber + 1},
				sendTime: start, ackTimeout: time.Minute, sendCount: 1, expectsAck: !row.noAck}
			younger.transferFrameBytes = marshalSendPackTransferFrame(&sendPackFrame{
				path:      sendTransferPath(sequence.client.ClientId(), DestinationId(sequence.destination)),
				messageId: younger.messageId, sequenceId: sequence.sequenceId, sequenceNumber: younger.sequenceNumber, nack: row.noAck,
			})
			if row.noAck {
				defer MessagePoolReturn(younger.transferFrameBytes)
			} else {
				sequence.sendItems = append(sequence.sendItems, younger)
				sequence.addResendItem(younger)
			}
			sequence.nextSequenceNumber = younger.sequenceNumber + 1
			sequence.windowPacer.rateUpdated = start
			var until time.Time
			sequence.sendBufferSettings.afterTransferWriteDeadlineForTest = func(at time.Time) { until = at }
			done := make(chan error, 1)
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				_, err := sequence.writeMaybeWrappedBytes(younger.transferFrameBytes, TransferPath{}, true, younger, false, row.reliableOnly)
				done <- err
			}()
			defer func() {
				sequence.cancel()
				<-finished
			}()
			synctest.Wait()
			if until != start.Add(200*time.Millisecond) || *observations != 2 || state.pendingLocalWrites.Load() != 1 {
				t.Fatal("fixture lost the real writer's original absolute budget")
			}
			time.Sleep(time.Until(due.Add(-time.Nanosecond)))
			synctest.Wait()
			if sequence.pendingRecovery != nil || len(done) != 0 {
				t.Fatal("owner selection preceded the older physical recovery boundary")
			}
			time.Sleep(time.Nanosecond)
			synctest.Wait()
			resumeWindowOwnerInspection(t, arrived, resume, due)
			selection := sequence.pendingRecovery
			if selection == nil || selection.messageId != older.messageId || selection.at != due ||
				selection.reliableOnly != row.reliableOnly || len(done) != 0 || len(route) != 1 ||
				*observations != 2 || older.sendTime.Add(older.ackTimeout) != lifetime {
				t.Fatalf("carrier=%s unreliable=%t reliableOnly=%t noAck=%t older selection lost exact owner/budget/lifetime",
					row.carrier, row.unreliable, row.reliableOnly, row.noAck)
			}
			if row.unreliable && sequence.client.unreliableFlightTimeoutCount.Load() != 1 {
				t.Fatal("due selection omitted the real unreliable-flight timeout decision")
			}
			time.Sleep(time.Until(until.Add(-time.Nanosecond)))
			synctest.Wait()
			if len(done) != 0 {
				t.Fatal("owner wake shortened the original writer budget")
			}
			time.Sleep(time.Nanosecond)
			synctest.Wait()
			if len(done) != 1 {
				t.Fatal("owner wake restarted the original writer budget")
			}
			if err := <-done; !errors.Is(err, errTransferRouteWriteTimeout) || *observations != 2 ||
				len(route) != 1 || state.pendingLocalWrites.Load() != 0 || sequence.client.resendQueueUnackedItemCount.Load() != 0 {
				t.Fatalf("bounded original lost its one observed outcome or retained a NoAck: %v", err)
			}
		})
	}
}

// Selection cannot replace the immutable 60s ACK lifetime with either its due
// clock or a younger reservation's later paid-service deadline.
func TestWindowPacingOwnerSelectionKeepsOriginalAckLifetime(t *testing.T) {
	runWindowOwnerProgressFixture(t, false, false, 59*time.Second, func(t *testing.T, fixture *windowOwnerProgressFixture) {
		sequence := fixture.sequence
		time.Sleep(time.Until(fixture.due))
		synctest.Wait()
		if sequence.pendingRecovery == nil || fixture.head.sendTime.Add(fixture.head.ackTimeout) != fixture.lifetime ||
			!fixture.youngerDeadline.After(fixture.lifetime) || fixture.lifetime.Sub(fixture.head.sendTime) != time.Minute {
			t.Fatal("selection changed the original lifetime or fixture did not overlap it")
		}
		selection := *sequence.pendingRecovery
		youngerId := fixture.younger.messageId
		time.Sleep(time.Until(fixture.lifetime.Add(-time.Nanosecond)))
		synctest.Wait()
		service := sequence.windowPacer.service
		service.stateLock.Lock()
		owned := service.pacingReservations == 1 && service.reservedByteCount == fixture.younger.pacingByteCount &&
			service.next == fixture.chargedNext && service.probeSent == fixture.chargedProbe
		service.stateLock.Unlock()
		if !owned || sequence.ctx.Err() != nil || len(fixture.route) != 0 || sequence.resendWriteCount.Load() != 0 {
			t.Fatal("selected recovery shortened lifetime, dropped its younger owner or invented physical credit")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		service.stateLock.Lock()
		released := service.pacingReservations == 0 && service.reservedByteCount == 0 &&
			service.next == fixture.chargedNext && service.probeSent == fixture.chargedProbe && !service.drained
		service.stateLock.Unlock()
		// The fixture holds this hook after both successful and failed initial
		// writes. Expiry cancels the source, not the still-live fixture context.
		if !released || time.Now() != fixture.lifetime || sequence.ctx.Err() == nil || fixture.ctx.Err() != nil ||
			len(fixture.written) != 1 || <-fixture.written != fixture.younger.sequenceNumber ||
			sequence.pendingRecovery == nil || *sequence.pendingRecovery != selection ||
			len(sequence.sendItems) != 2 || len(sequence.ackLifetimes.items) != 2 ||
			sequence.resendQueue.GetByMessageId(selection.messageId) != fixture.head ||
			sequence.resendQueue.GetByMessageId(youngerId) != fixture.younger ||
			fixture.head.sendTime != selection.sendTime || fixture.head.sendTime.Add(fixture.head.ackTimeout) != fixture.lifetime ||
			fixture.head.deliveryObserved || fixture.younger.deliveryObserved || fixture.younger.transportWriteObserved ||
			len(fixture.route) != 0 || sequence.resendWriteCount.Load() != 0 {
			t.Fatal("exact ACK expiry did not return the failed younger write to its still-owned fixture hook")
		}
		select {
		case <-sequence.done:
			t.Fatal("source teardown crossed the held younger write hook")
		default:
		}
		close(fixture.resumeYounger)
		<-sequence.done
		synctest.Wait()
		if time.Now() != fixture.lifetime || fixture.ctx.Err() != nil {
			t.Fatal("fixture release moved the exact ACK deadline or canceled the parent")
		}
		service.stateLock.Lock()
		released = service.pacingReservations == 0 && service.reservedByteCount == 0 &&
			service.next == fixture.chargedNext && service.probeSent == fixture.chargedProbe && !service.drained
		service.stateLock.Unlock()
		if !released || sequence.ctx.Err() == nil || sequence.pendingRecovery != nil ||
			len(sequence.sendItems) != 0 || len(fixture.route) != 0 || sequence.resendWriteCount.Load() != 0 {
			t.Fatal("selected recovery hid the exact original ACK expiry or refunded physical debt")
		}
	})
}

// A second producer already queued behind the younger original keeps that FIFO
// position when the selected older retry finally asks for its own service.
func TestWindowPacingOwnerSelectionPreservesSiblingFifo(t *testing.T) {
	runWindowOwnerProgressFixture(t, false, false, time.Second, func(t *testing.T, fixture *windowOwnerProgressFixture) {
		sequence := fixture.sequence
		time.Sleep(time.Until(fixture.due))
		synctest.Wait()
		if sequence.pendingRecovery == nil {
			t.Fatal("fixture did not select the older copy")
		}
		service := sequence.windowPacer.service
		siblingCtx, cancelSibling := context.WithCancel(fixture.ctx)
		sibling := windowBurstPacer{
			service: service, rate: sequence.windowPacer.rate, estimateRate: sequence.windowPacer.estimateRate,
			probeRate: sequence.windowPacer.probeRate, probeLimit: sequence.windowPacer.probeLimit,
		}
		sibling.waiter.holdCompressionBurst = sequence.windowPacer.waiter.holdCompressionBurst
		waiting, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
		signaled := false
		sibling.afterWaitForTest = func() {
			if !signaled {
				signaled = true
				close(waiting)
			}
			select {
			case <-siblingCtx.Done():
			case <-release:
			}
		}
		var siblingErr error
		go func() {
			defer close(finished)
			siblingErr = sibling.waitForService(siblingCtx, int(fixture.younger.pacingByteCount))
		}()
		defer func() {
			cancelSibling()
			<-finished
			sibling.close()
		}()
		synctest.Wait()
		service.stateLock.Lock()
		queued := service.waiterHead == &sequence.windowPacer.waiter && service.waiterTail == &sibling.waiter &&
			service.pacingReservations == 2 && service.reservedByteCount == 2*fixture.younger.pacingByteCount
		chargedNext, chargedProbe, chargedSent := service.next, service.probeSent, service.sent
		service.stateLock.Unlock()
		if !queued || len(fixture.route) != 0 {
			t.Fatal("sibling did not acquire its real FIFO position behind the charged original")
		}
		time.Sleep(time.Until(fixture.youngerDeadline))
		synctest.Wait()
		if len(fixture.written) != 1 || <-fixture.written != fixture.younger.sequenceNumber || len(fixture.route) != 1 {
			t.Fatal("younger original did not physically settle its own reservation")
		}
		MessagePoolReturn(<-fixture.route)
		select {
		case <-waiting:
		case <-sequence.ctx.Done():
			t.Fatal("sibling never reached its original paid deadline")
		}
		close(fixture.resumeYounger)
		synctest.Wait()
		service.stateLock.Lock()
		ordered := service.waiterHead == &sibling.waiter && service.waiterTail == &sequence.windowPacer.waiter &&
			service.pacingReservations == 2 && service.reservedByteCount == fixture.younger.pacingByteCount &&
			!service.next.Before(chargedNext) && service.probeSent == chargedProbe && service.sent == chargedSent
		service.stateLock.Unlock()
		if !ordered || len(fixture.route) != 0 || sequence.resendWriteCount.Load() != 0 || sequence.pendingRecovery != nil {
			t.Fatal("selected retry replaced the sibling, recharged original bytes or bypassed the shared FIFO")
		}
		close(release)
		<-finished
		if siblingErr != nil {
			t.Fatal(siblingErr)
		}
		select {
		case <-fixture.retryAdmitted:
		case <-sequence.ctx.Done():
			t.Fatal("selected retry did not progress after the paid sibling")
		}
		synctest.Wait()
		if len(fixture.route) != 1 || sequence.resendWriteCount.Load() != 1 {
			t.Fatal("selected retry lost physical progress after the FIFO handoff")
		}
		wire := <-fixture.route
		pack := decodeSendPackLifecycleWirePack(t, wire)
		MessagePoolReturn(wire)
		if string(pack.MessageId) != string(fixture.head.messageId[:]) {
			t.Fatal("FIFO handoff dispatched a different recovery identity")
		}
	})
}

// A selected record adds no payload ownership. Exact runtime sizes are evidence
// for root's later retained-memory assessment, never a host/device memory claim.
func TestWindowPacingOwnerSelectionMetadataSizeCensus(t *testing.T) {
	t.Logf("send_sequence_bytes=%d pacing_owner_bytes=%d selected_recovery_bytes=%d",
		unsafe.Sizeof(SendSequence{}), unsafe.Sizeof(windowBurstPacer{}), unsafe.Sizeof(sendRecoverySelection{}))
}
