// A real send worker must select recovery without borrowing a younger write's
// FIFO credit. These controls retain the original physical and ACK clocks.
package connect

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Fixed fixture ownership: one accepted older Pack, optionally its acknowledged
// predecessor, one blocked younger Pack, and the original Client worker tree.
type windowOwnerProgressFixture struct {
	ctx             context.Context
	cancel          context.CancelFunc
	client          *Client
	sequence        *SendSequence
	route           Route
	head            *sendItem
	younger         *sendItem
	due             time.Time
	lifetime        time.Time
	youngerDeadline time.Time
	chargedNext     time.Time
	chargedProbe    ByteCount
	chargedSent     ByteCount
	written         chan uint64
	resumeYounger   chan struct{}
	retryAdmitted   chan struct{}
	ackPublished    chan struct{}
	releaseAck      <-chan struct{}
	stateLock       sync.Mutex
	observed        windowOwnerProgressObservation
	inspection      *windowOwnerInspection
	lifecycle       <-chan SendPackLifecycleObservation
}

// Only hook-owned scalar observations use this lock. It never substitutes for
// a handoff protecting SendSequence or sendItem fields.
type windowOwnerProgressObservation struct {
	admissions int
	selections int
	selectedAt time.Time
}

// Copy only the counters and timestamps whose writes belong to fixture hooks.
func (self *windowOwnerProgressFixture) observation() windowOwnerProgressObservation {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.observed
}

// The existing policy-access hook precedes both recovery lifetime writes and
// selection. Arm only while the owner is parked; inspect before releasing it
// at the same fake instant. Gate metadata is shared, owner state is not.
type windowOwnerInspection struct {
	ctx       context.Context
	stateLock sync.Mutex
	at        time.Time
	reached   bool
	arrived   chan time.Time
	resume    chan struct{}
}

// Rearm after the preceding inspection has resumed and the owner has parked.
func (self *windowOwnerInspection) arm(at time.Time) (<-chan time.Time, chan struct{}) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.at, self.reached = at, false
	self.arrived, self.resume = make(chan time.Time, 1), make(chan struct{})
	return self.arrived, self.resume
}

// Writer publication/cleanup is not the policy-read boundary under inspection.
func (self *windowOwnerInspection) beforeWriterAccess(publish bool) {
	if publish {
		return
	}
	self.stateLock.Lock()
	now := time.Now()
	if self.resume == nil || now.Before(self.at) {
		self.stateLock.Unlock()
		return
	}
	arrived, resume, first := self.arrived, self.resume, !self.reached
	self.reached = true
	self.stateLock.Unlock()
	if first {
		arrived <- now
	}
	select {
	case <-self.ctx.Done():
	case <-resume:
	}
}

// Wait supplies the owner-to-reader edge; this release supplies the distinct
// reader-to-future-owner edge before any protected fields can change again.
func resumeWindowOwnerInspection(t *testing.T, arrived <-chan time.Time, resume chan struct{}, at time.Time) {
	t.Helper()
	select {
	case observedAt := <-arrived:
		if observedAt != at || time.Now() != at {
			t.Fatal("policy inspection or release moved the original fake-clock boundary")
		}
	default:
		t.Fatal("worker missed the exact armed policy-access inspection boundary")
	}
	close(resume)
	synctest.Wait()
}

// The prefix variation makes recovery's real Head rewrite observable before
// physical admission. The writer variation retains an already-accepted route
// share so that the younger write parks in the selector, not in the pacer.
func runWindowOwnerProgressFixture(t *testing.T, prefix, writer bool, serviceWait time.Duration, check func(*testing.T, *windowOwnerProgressFixture)) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		destination := NewId()
		fixture := &windowOwnerProgressFixture{
			ctx: ctx, cancel: cancel, route: make(Route, 1),
			written: make(chan uint64, 4), resumeYounger: make(chan struct{}), retryAdmitted: make(chan struct{}),
			ackPublished: make(chan struct{}, 1),
		}
		headNumber := uint64(0)
		if prefix {
			headNumber = 1
		}
		resumeHead := make(chan struct{})
		producerDone := make(chan struct{})
		producerStarted := false
		settings := DefaultClientSettings()
		settings.Log = NewNoopLogger()
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		settings.SendBufferSettings.WindowSizing = WindowSizingFromDelivery
		settings.SendBufferSettings.ApplyWindowSizing()
		observer, lifecycle := sendPackLifecycleTestObserver(destination)
		fixture.lifecycle = lifecycle
		settings.SendBufferSettings.SendPackLifecycleObserver = observer
		settings.SendBufferSettings.afterInitialWriteQueuedForTest = func(id sendSequenceId, number uint64) {
			if id.Destination != destination {
				return
			}
			fixture.written <- number
			var resume <-chan struct{}
			if number == headNumber {
				resume = resumeHead
			} else if number == headNumber+1 {
				resume = fixture.resumeYounger
			} else {
				return
			}
			select {
			case <-ctx.Done():
			case <-resume:
			}
		}
		settings.SendBufferSettings.beforeDueResendForTest = func(id sendSequenceId, number uint64) {
			if id.Destination == destination && number == headNumber {
				fixture.stateLock.Lock()
				fixture.observed.selections++
				if fixture.observed.selections == 1 {
					fixture.observed.selectedAt = time.Now()
				}
				fixture.stateLock.Unlock()
			}
		}
		settings.SendBufferSettings.afterAckCoalescedForTest = func(id sendSequenceId, number uint64) {
			if id.Destination == destination && number == headNumber && fixture.releaseAck != nil {
				fixture.ackPublished <- struct{}{}
				<-fixture.releaseAck
			}
		}
		client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
		fixture.client = client
		defer func() {
			cancel()
			if producerStarted {
				<-producerDone
			}
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
			for len(fixture.route) > 0 {
				MessagePoolReturn(<-fixture.route)
			}
		}()
		client.ContractManager().AddNoContractPeer(destination)
		client.RouteManager().UpdateTransport(NewSendGatewayTransportWithType(TransportTypeH1), []Route{fixture.route})
		send := func() bool {
			frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(4000)}
			clear(frame.MessageBytes)
			if !client.SendWithTimeout(frame, destination, func(error) {}, -1) {
				MessagePoolReturn(frame.MessageBytes)
				return false
			}
			return true
		}
		var prefixPack *protocol.Pack
		if prefix {
			if !send() || <-fixture.written != 0 || len(fixture.route) != 1 {
				t.Fatal("prefix was not physically accepted")
			}
			wire := <-fixture.route
			prefixPack = decodeSendPackLifecycleWirePack(t, wire)
			MessagePoolReturn(wire)
		}
		if !send() || <-fixture.written != headNumber || len(fixture.route) != 1 {
			t.Fatal("older Pack was not physically accepted")
		}
		fixture.sequence = client.sendBuffer.lookupSendSequence(sendSequenceId{Destination: destination}, nil)
		// The initial-write hook still owns the worker while this hook is installed.
		fixture.inspection = &windowOwnerInspection{ctx: fixture.sequence.ctx}
		fixture.sequence.beforeContractWriterAccessForTest = fixture.inspection.beforeWriterAccess
		wire := <-fixture.route
		pack := decodeSendPackLifecycleWirePack(t, wire)
		messageId, err := IdFromBytes(pack.MessageId)
		if err != nil {
			MessagePoolReturn(wire)
			t.Fatal(err)
		}
		if writer {
			fixture.route <- wire
		} else {
			MessagePoolReturn(wire)
		}
		fixture.head = fixture.sequence.resendQueue.GetByMessageId(messageId)
		head := fixture.head
		if head == nil || head.sequenceNumber != headNumber || !head.transportWriteObserved || !head.rttH1 ||
			head.head == prefix {
			t.Fatal("older retained identity does not match its actual original")
		}
		fixture.due, fixture.lifetime = head.resendTime, head.sendTime.Add(head.ackTimeout)
		fixture.sequence.windowPacer.afterAdmissionForTest = func() {
			fixture.stateLock.Lock()
			fixture.observed.admissions++
			admissions := fixture.observed.admissions
			fixture.stateLock.Unlock()
			if admissions == 2 {
				close(fixture.retryAdmitted)
			}
		}
		if fixture.due != fixture.sequence.firstPhysicalRecoveryTime(head).Add(2*time.Second) ||
			fixture.lifetime != head.sendTime.Add(time.Minute) {
			t.Fatal("fixture changed the original physical recovery or 60s ACK lifetime")
		}
		service := fixture.sequence.windowPacer.service
		service.stateLock.Lock()
		expiry := service.burst.start.Add(windowPacingBurstMaximumTime(service.burstEstimateTime))
		service.stateLock.Unlock()
		if !expiry.After(time.Now()) || !expiry.Before(fixture.due) {
			t.Fatal("opening burst cannot expire before the older due boundary")
		}
		time.Sleep(time.Until(expiry))
		if prefix {
			acknowledgeSendPackLifecycleWirePack(t, client, destination, prefixPack)
		}
		if !writer {
			service.stateLock.Lock()
			service.next = fixture.due.Add(serviceWait)
			service.stateLock.Unlock()
		}
		producerStarted = true
		go func() {
			defer close(producerDone)
			send()
		}()
		close(resumeHead)
		synctest.Wait()
		for _, item := range fixture.sequence.sendItems {
			if item.sequenceNumber == headNumber+1 {
				fixture.younger = item
			}
		}
		if fixture.younger == nil || fixture.younger.transportWriteObserved || fixture.observation().selections != 0 ||
			fixture.sequence.resendWriteCount.Load() != 0 || len(fixture.written) != 0 {
			t.Fatal("fixture lost its one pending younger original")
		}
		if prefix && (len(fixture.sequence.sendItems) != 2 || fixture.sequence.sendItems[0] != head || head.head) {
			t.Fatal("real prefix ACK did not leave an unpromoted older head")
		}
		service.stateLock.Lock()
		fixture.chargedNext, fixture.chargedProbe, fixture.chargedSent = service.next, service.probeSent, service.sent
		reservations, reserved := service.pacingReservations, service.reservedByteCount
		queued := service.waiterHead == &fixture.sequence.windowPacer.waiter
		service.stateLock.Unlock()
		fixture.youngerDeadline = fixture.sequence.windowPacer.waiter.deadline
		if writer {
			if len(fixture.route) != 1 || reservations != 0 || reserved != 0 || fixture.younger.rttState != sendItemRttWritePending {
				t.Fatal("younger write did not reach the real full selector")
			}
		} else if len(fixture.route) != 0 || reservations != 1 || reserved != fixture.younger.pacingByteCount ||
			!queued || !fixture.youngerDeadline.After(fixture.due) {
			t.Fatal("younger write did not retain exactly one charged FIFO entry across recovery")
		}
		check(t, fixture)
	})
}

// A real prefix ACK first earns one progress deferral. The next eligible firing
// must prepare the older Head while the younger still owns physical service,
// then dispatch both exact originals in order with one lifecycle each.
func TestWindowPacingOwnerSelectionRewritesThenDispatches(t *testing.T) {
	runWindowOwnerProgressFixture(t, true, false, 5*time.Second, func(t *testing.T, fixture *windowOwnerProgressFixture) {
		sequence, head, younger := fixture.sequence, fixture.head, fixture.younger
		arrived, resume := fixture.inspection.arm(fixture.due)
		time.Sleep(time.Until(fixture.due.Add(-time.Nanosecond)))
		synctest.Wait()
		if head.head || fixture.observation().selections != 0 || len(fixture.route) != 0 {
			t.Fatal("recovery preparation or physical dispatch preceded the exact older due time")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		resumeWindowOwnerInspection(t, arrived, resume, fixture.due)
		// The later policy access must wait for all first-deferral reads.
		arrived, resume = fixture.inspection.arm(fixture.due.Add(2 * time.Second))
		service := sequence.windowPacer.service
		service.stateLock.Lock()
		kept := service.next == fixture.chargedNext && service.probeSent == fixture.chargedProbe &&
			service.sent == fixture.chargedSent && service.pacingReservations == 1 &&
			service.reservedByteCount == younger.pacingByteCount && service.waiterHead == &sequence.windowPacer.waiter
		reservations, reserved, sent := service.pacingReservations, service.reservedByteCount, service.sent
		fifoHead, fifoTail := service.waiterHead == &sequence.windowPacer.waiter, service.waiterTail == &sequence.windowPacer.waiter
		next, probe := service.next, service.probeSent
		service.stateLock.Unlock()
		t.Logf("owner_rewrite_premise now_after_due=%s head=%t promoted=%t selections=%d selected_at_same=%t selected_after_due=%s kept=%t next_same=%t next_after_due=%s charged_next_after_due=%s probe_same=%t probe=%d charged_probe=%d sent_same=%t sent=%d charged_sent=%d reservations=%d reserved=%d younger_bytes=%d fifo_head_same=%t fifo_tail_same=%t lifetime_same=%t lifetime=%s expected_lifetime=%s route=%d younger_written=%t retries=%d head_due_after_fixture=%s timeout_defers=%d deferral_outstanding=%t last_cumulative_ack_after_head=%s pending=%t",
			time.Since(fixture.due), head.head, head.promotedHead, fixture.observation().selections, fixture.observation().selectedAt == fixture.due,
			fixture.observation().selectedAt.Sub(fixture.due), kept, next == fixture.chargedNext, next.Sub(fixture.due), fixture.chargedNext.Sub(fixture.due),
			probe == fixture.chargedProbe, probe, fixture.chargedProbe, sent == fixture.chargedSent, sent, fixture.chargedSent,
			reservations, reserved, younger.pacingByteCount, fifoHead, fifoTail,
			head.sendTime.Add(head.ackTimeout) == fixture.lifetime, head.ackTimeout, fixture.lifetime.Sub(head.sendTime),
			len(fixture.route), younger.transportWriteObserved, sequence.resendWriteCount.Load(), head.resendTime.Sub(fixture.due),
			head.timeoutDeferCount, head.deferralOutstanding, sequence.lastCumulativeAckTime.Sub(head.sendTime), sequence.pendingRecovery != nil)
		eligibleAt := head.resendTime
		prefixAckAt := sequence.lastCumulativeAckTime
		if head.head || head.promotedHead || fixture.observation().selections != 1 || fixture.observation().selectedAt != fixture.due ||
			sequence.pendingRecovery != nil || head.timeoutDeferCount != 1 || !head.deferralOutstanding ||
			head.timeoutDeferAckTime != prefixAckAt || !prefixAckAt.After(head.sendTime) ||
			eligibleAt != fixture.due.Add(2*time.Second) || !fixture.youngerDeadline.After(eligibleAt) ||
			!kept || head.sendTime.Add(head.ackTimeout) != fixture.lifetime || len(fixture.route) != 0 ||
			younger.transportWriteObserved || sequence.resendWriteCount.Load() != 0 {
			t.Fatal("first due inspection did not preserve real prefix progress deferral and younger service ownership")
		}
		time.Sleep(time.Until(eligibleAt.Add(-time.Nanosecond)))
		synctest.Wait()
		if head.head || head.promotedHead || fixture.observation().selections != 1 || sequence.pendingRecovery != nil ||
			head.resendTime != eligibleAt || sequence.lastCumulativeAckTime != prefixAckAt ||
			len(fixture.route) != 0 || younger.transportWriteObserved || sequence.resendWriteCount.Load() != 0 {
			t.Fatal("recovery bypassed the unchanged progress-deferral deadline")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		resumeWindowOwnerInspection(t, arrived, resume, eligibleAt)
		arrived, resume = fixture.inspection.arm(fixture.youngerDeadline)
		service.stateLock.Lock()
		kept = service.next == fixture.chargedNext && service.probeSent == fixture.chargedProbe &&
			service.sent == fixture.chargedSent && service.pacingReservations == 1 &&
			service.reservedByteCount == younger.pacingByteCount && service.waiterHead == &sequence.windowPacer.waiter
		service.stateLock.Unlock()
		selection := sequence.pendingRecovery
		if !head.head || !head.promotedHead || fixture.observation().selections != 2 || fixture.observation().selectedAt != fixture.due ||
			selection == nil || selection.at != eligibleAt || selection.due != eligibleAt ||
			selection.messageId != head.messageId || selection.number != head.sequenceNumber ||
			head.timeoutDeferCount != 1 || head.deferralOutstanding || sequence.lastCumulativeAckTime != prefixAckAt ||
			!kept || head.sendTime.Add(head.ackTimeout) != fixture.lifetime || len(fixture.route) != 0 ||
			younger.transportWriteObserved || sequence.resendWriteCount.Load() != 0 {
			t.Fatal("older due selection did not prepare its real Head while keeping younger bytes and service debt")
		}
		time.Sleep(time.Until(fixture.youngerDeadline))
		synctest.Wait()
		resumeWindowOwnerInspection(t, arrived, resume, fixture.youngerDeadline)
		if len(fixture.written) != 1 || <-fixture.written != younger.sequenceNumber ||
			len(fixture.route) != 1 || !younger.transportWriteObserved || sequence.resendWriteCount.Load() != 0 {
			t.Fatal("paid younger FIFO owner did not complete before recovery dispatch")
		}
		wire := <-fixture.route
		pack := decodeSendPackLifecycleWirePack(t, wire)
		MessagePoolReturn(wire)
		if string(pack.MessageId) != string(younger.messageId[:]) || pack.SequenceNumber != younger.sequenceNumber {
			t.Fatal("recovery stole the younger reservation or replaced its physical identity")
		}
		close(fixture.resumeYounger)
		// This is the real retry's service-admission edge, not a guessed sleep
		// or a demand for unpaid credit. The unchanged ACK lifetime bounds it.
		select {
		case <-fixture.retryAdmitted:
		case <-sequence.ctx.Done():
			t.Fatal("selected retry failed to reach paid service inside its original lifetime")
		}
		synctest.Wait()
		if len(fixture.route) != 1 || sequence.resendWriteCount.Load() != 1 || fixture.observation().selections != 2 {
			t.Fatal("selected older retry did not physically progress once its service was paid")
		}
		wire = <-fixture.route
		pack = decodeSendPackLifecycleWirePack(t, wire)
		MessagePoolReturn(wire)
		if string(pack.MessageId) != string(head.messageId[:]) || pack.SequenceNumber != head.sequenceNumber || !pack.Head ||
			sequence.resendQueue.GetByMessageId(younger.messageId) != younger || !younger.transportWriteObserved {
			t.Fatal("physical retry changed identity, omitted the selected Head or lost its younger owner")
		}
		acknowledgeSendPackLifecycleWirePack(t, fixture.client, sequence.destination, &protocol.Pack{
			MessageId: younger.messageId.Bytes(), SequenceId: sequence.sequenceId.Bytes(),
		})
		synctest.Wait()
		counts := map[uint64][3]int{}
		for len(fixture.lifecycle) > 0 {
			event := <-fixture.lifecycle
			count := counts[event.Token]
			switch event.Phase {
			case SendPackLifecyclePhaseStarted:
				count[0]++
			case SendPackLifecyclePhaseFirstRouteWrite:
				count[1]++
			case SendPackLifecyclePhaseTerminal:
				count[2]++
			}
			if event.Err != nil {
				t.Fatalf("owned Pack completed with an unexpected lifecycle failure: %v", event.Err)
			}
			counts[event.Token] = count
		}
		if len(counts) != 3 || len(sequence.sendItems) != 0 {
			t.Fatal("real cumulative ACK did not retire exactly the three original Packs")
		}
		for _, count := range counts {
			if count != [3]int{1, 1, 1} {
				t.Fatalf("selection repeated or dropped an original lifecycle phase: %v", count)
			}
		}
	})
}
