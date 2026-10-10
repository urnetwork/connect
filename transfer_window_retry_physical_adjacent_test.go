// Paced retries retain finite recovery, cancellation and actual-carrier ownership.
package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Captured by the owning worker before its second due recovery proceeds.
type windowRetryClockBoundary struct {
	at             time.Time
	physical       time.Time
	deadline       time.Time
	copies         int
	unreliable     bool
	carrierChanged bool
}

// One initial H1 write is held until a test releases its real send worker.
type windowRetryClockFixture struct {
	start           time.Time
	client          *Client
	sequence        *SendSequence
	transport       *h1SendClientTransportForGroupTest
	route           Route
	otherRoutes     []Route
	releaseInitial  chan struct{}
	secondDue       chan windowRetryClockBoundary
	resumeSecondDue <-chan struct{}
	ackFailedAt     chan time.Time
	cancel          context.CancelFunc
}

// Runs each row with owned workers and pooled route buffers fully joined.
func runWindowRetryClockFixture(t *testing.T, maximum, ackTimeout time.Duration, check func(*testing.T, *windowRetryClockFixture)) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		destination := NewId()
		written := make(chan struct{})
		fixture := &windowRetryClockFixture{
			route: make(Route, 8), releaseInitial: make(chan struct{}),
			secondDue: make(chan windowRetryClockBoundary, 1), ackFailedAt: make(chan time.Time, 1), cancel: cancel,
			transport: &h1SendClientTransportForGroupTest{sendClientTransport: NewSendClientTransport(DestinationId(destination))},
		}
		settings := DefaultClientSettings()
		settings.Log = NewNoopLogger()
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		settings.SendBufferSettings.WindowSizing = WindowSizingFromDelivery
		settings.SendBufferSettings.ApplyWindowSizing()
		settings.SendBufferSettings.MinResendInterval = 300 * time.Millisecond
		settings.SendBufferSettings.RttMinResendInterval = 300 * time.Millisecond
		settings.SendBufferSettings.MaxResendInterval = maximum
		settings.SendBufferSettings.AckTimeout = ackTimeout
		settings.SendBufferSettings.WriteTimeout = 50 * time.Millisecond
		settings.SendBufferSettings.afterInitialWriteQueuedForTest = func(id sendSequenceId, number uint64) {
			if id.Destination == destination && number == 0 {
				close(written)
				select {
				case <-fixture.releaseInitial:
				case <-ctx.Done():
				}
			}
		}
		firings := 0
		settings.SendBufferSettings.beforeDueResendForTest = func(id sendSequenceId, number uint64) {
			if id.Destination != destination || number != 0 {
				return
			}
			firings++
			if firings == 2 {
				item := fixture.sequence.resendQueue.PeekFirst()
				fixture.secondDue <- windowRetryClockBoundary{
					at: time.Now(), physical: fixture.sequence.windowPacer.waiter.sentAt,
					deadline: item.resendTime, copies: item.sendCount,
					unreliable: item.unreliableCarrierObserved, carrierChanged: item.carrierChanged,
				}
				// Existing tests leave this nil and keep the original hold.
				select {
				case <-ctx.Done():
				case <-fixture.resumeSecondDue:
				}
			}
		}
		fixture.client = NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
		defer func() {
			cancel()
			if err := fixture.client.CloseAndWait(context.Background()); err != nil {
				t.Errorf("client cleanup: %v", err)
			}
			for _, route := range append(fixture.otherRoutes, fixture.route) {
				for len(route) > 0 {
					MessagePoolReturn(<-route)
				}
			}
		}()
		fixture.client.ContractManager().AddNoContractPeer(destination)
		fixture.client.RouteManager().UpdateTransport(fixture.transport, []Route{fixture.route})
		frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(1280)}
		clear(frame.MessageBytes)
		fixture.start = time.Now()
		if !fixture.client.SendWithTimeout(frame, destination, func(err error) {
			if err != nil {
				fixture.ackFailedAt <- time.Now()
			}
		}, time.Second) {
			MessagePoolReturn(frame.MessageBytes)
			t.Fatal("Pack not admitted")
		}
		<-written
		fixture.sequence = fixture.client.sendBuffer.lookupSendSequence(sendSequenceId{Destination: destination}, nil)
		MessagePoolReturn(<-fixture.route)
		check(t, fixture)
	})
}

// Moves only the existing local serialization reservation, then resumes work.
func (self *windowRetryClockFixture) delayRetry(until time.Duration) {
	service := self.sequence.windowPacer.service
	service.stateLock.Lock()
	service.next = self.start.Add(until)
	service.stateLock.Unlock()
	close(self.releaseInitial)
	synctest.Wait()
}

// A lower configured ceiling still bounds the same post-write backoff.
func TestWindowPacingRetryPhysicalClockKeepsMaximum(t *testing.T) {
	runWindowRetryClockFixture(t, 400*time.Millisecond, time.Minute, func(t *testing.T, fixture *windowRetryClockFixture) {
		fixture.delayRetry(1200 * time.Millisecond)
		time.Sleep(1600*time.Millisecond - time.Nanosecond)
		synctest.Wait()
		if len(fixture.route) != 1 || len(fixture.secondDue) != 0 {
			t.Fatal("retry spent its capped 400ms interval before physical dispatch")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		select {
		case boundary := <-fixture.secondDue:
			if boundary.deadline != fixture.start.Add(1600*time.Millisecond) || boundary.at != boundary.deadline || boundary.physical != fixture.start.Add(1200*time.Millisecond) {
				t.Fatalf("maximum interval changed: %+v", boundary)
			}
		default:
			t.Fatal("missing reply outlived the configured post-write maximum")
		}
	})
}

// Re-anchoring one retry cannot move the original message's lifetime deadline.
func TestWindowPacingRetryPhysicalClockKeepsAckLifetime(t *testing.T) {
	runWindowRetryClockFixture(t, 4*time.Second, 1500*time.Millisecond, func(t *testing.T, fixture *windowRetryClockFixture) {
		fixture.delayRetry(1200 * time.Millisecond)
		time.Sleep(1500*time.Millisecond - time.Nanosecond)
		synctest.Wait()
		if len(fixture.route) != 1 || len(fixture.ackFailedAt) != 0 || len(fixture.secondDue) != 0 {
			t.Fatal("retry or expiration occurred before the original lifetime boundary")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		select {
		case at := <-fixture.ackFailedAt:
			if at != fixture.start.Add(1500*time.Millisecond) {
				t.Fatalf("retry extended acknowledgement lifetime to %s", at.Sub(fixture.start))
			}
		default:
			t.Fatal("physical retry extended the original acknowledgement lifetime")
		}
		if len(fixture.secondDue) != 0 {
			t.Fatal("expiration emitted another physical recovery attempt")
		}
	})
}

// An unpaced retry on H1 must not borrow an earlier waiter's timestamp.
func TestWindowPacingUnpacedRetryIgnoresPreviousWaiter(t *testing.T) {
	runWindowRetryClockFixture(t, 4*time.Second, time.Minute, func(t *testing.T, fixture *windowRetryClockFixture) {
		previous := fixture.sequence.windowPacer.waiter.sentAt
		fixture.sequence.sendBufferSettings.disableWindowPacingForTest = true
		time.Sleep(500 * time.Millisecond)
		close(fixture.releaseInitial)
		synctest.Wait()
		if len(fixture.route) != 1 {
			t.Fatal("unpaced retry did not physically write at500ms")
		}
		time.Sleep(600*time.Millisecond - time.Nanosecond)
		synctest.Wait()
		if len(fixture.secondDue) != 0 {
			t.Fatal("unpaced retry reused the old physical reservation")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		select {
		case boundary := <-fixture.secondDue:
			if boundary.physical != previous || boundary.deadline != fixture.start.Add(1100*time.Millisecond) || boundary.at != boundary.deadline {
				t.Fatalf("unpaced retry changed its original recovery contract: %+v", boundary)
			}
		default:
			t.Fatal("unpaced retry lost its ordinary600ms recovery interval")
		}
	})
}

// A completed pacing wait is not a successful physical write. Failed writes
// retain the previous recovery contract and cannot grant a fresh H1 interval.
func TestWindowPacingFailedRetryIgnoresWaiter(t *testing.T) {
	runWindowRetryClockFixture(t, 4*time.Second, time.Minute, func(t *testing.T, fixture *windowRetryClockFixture) {
		for range cap(fixture.route) {
			fixture.route <- MessagePoolGet(1)
		}
		fixture.delayRetry(1200 * time.Millisecond)
		time.Sleep(1250 * time.Millisecond)
		synctest.Wait()
		select {
		case boundary := <-fixture.secondDue:
			if boundary.physical != fixture.start.Add(1200*time.Millisecond) || boundary.deadline != fixture.start.Add(900*time.Millisecond) || boundary.at != fixture.start.Add(1250*time.Millisecond) {
				t.Fatalf("failed H1 attempt acquired a successful-write interval: %+v", boundary)
			}
		default:
			t.Fatal("failed physical write incorrectly granted a fresh recovery interval")
		}
		if fixture.sequence.resendWriteCount.Load() != 0 {
			t.Fatal("blocked route unexpectedly accepted a retry")
		}
	})
}

// A route change during a local H1 pacing wait is classified by the carrier
// that actually accepts the retry, rather than its original H1 sample flag.
func TestWindowPacingChangedCarrierRetryIgnoresH1Waiter(t *testing.T) {
	for _, unreliable := range []bool{false, true} {
		runWindowRetryClockFixture(t, 4*time.Second, time.Minute, func(t *testing.T, fixture *windowRetryClockFixture) {
			head := fixture.sequence.resendQueue.PeekFirst()
			if head == nil || head.sendCount != 1 || !head.transportWriteObserved {
				t.Fatal("fixture lost its accepted initial H1 head")
			}
			messageId, number := head.messageId, head.sequenceNumber
			lifetime := head.sendTime.Add(head.ackTimeout)
			if unreliable {
				lifetime = head.sendTime.Add(max(head.ackTimeout, fixture.sequence.sendBufferSettings.UnreliableAckTimeout))
			}
			previousH1 := fixture.sequence.windowPacer.waiter.sentAt
			resumeSecondDue := make(chan struct{})
			fixture.resumeSecondDue = resumeSecondDue
			// This owner callback is installed while releaseInitial still holds
			// the worker. Copy the completed retry before a later loop can renew
			// its mutable lifetime; Wait alone does not order that future write.
			type retryObservation struct {
				messageId                                 Id
				number                                    uint64
				at, created, deadline, lifetime, physical time.Time
				copies                                    int
				unreliable, carrierChanged                bool
				route                                     Route
			}
			firstRetry := make(chan retryObservation, 1)
			retryObserved := false
			fixture.sequence.sendBuffer.afterApplyAckSnapshotForTest = func(id sendSequenceId) {
				if id != fixture.sequence.id() || retryObserved {
					return
				}
				item := fixture.sequence.resendQueue.GetByMessageId(messageId)
				if item == nil || item.sendCount != 2 || !item.transportWriteObserved || fixture.sequence.resendWriteCount.Load() != 1 {
					return
				}
				retryObserved = true
				firstRetry <- retryObservation{
					messageId: item.messageId, number: item.sequenceNumber, at: time.Now(), created: item.sendTime,
					deadline: item.resendTime, lifetime: item.sendTime.Add(item.ackTimeout), physical: fixture.sequence.windowPacer.waiter.sentAt,
					copies: item.sendCount, unreliable: item.unreliableCarrierObserved, carrierChanged: item.carrierChanged, route: item.carrierRoute,
				}
			}
			fixture.delayRetry(1200 * time.Millisecond)
			time.Sleep(time.Until(fixture.start.Add(400 * time.Millisecond)))
			synctest.Wait()
			service := fixture.sequence.windowPacer.service
			service.stateLock.Lock()
			next, probe, sent := service.next, service.probeSent, service.sent
			// A retry owns one FIFO reservation, not a second copy of the
			// original's lifetime byte charge.
			held := service.pacingReservations == 1 && service.reservedByteCount == 0 && service.sent == head.pacingByteCount &&
				service.waiterHead == &fixture.sequence.windowPacer.waiter && service.waiterTail == &fixture.sequence.windowPacer.waiter
			service.stateLock.Unlock()
			if !held || fixture.sequence.resendWriteCount.Load() != 0 || len(fixture.route) != 0 {
				t.Fatal("fixture did not retain the first retry's charged H1 reservation")
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
			// Withdrawal publishes immediately but joins readers of the old
			// snapshot. The actual H3 acceptance must retire that H1 wait.
			synctest.Wait()
			if fixture.sequence.transferFlightPolicy().h1Only {
				t.Fatal("carrier change was not published during the pacing wait")
			}
			accept := func(at time.Time, copies uint64) {
				t.Helper()
				if time.Now() != at || len(h3Route) != 1 || len(fixture.route) != 0 ||
					fixture.sequence.resendWriteCount.Load() != copies {
					t.Fatalf("unreliable=%t missing actual H3 copy: at=%s want=%s route=%d writes=%d",
						unreliable, time.Since(fixture.start), at.Sub(fixture.start), len(h3Route), fixture.sequence.resendWriteCount.Load())
				}
				wire := <-h3Route
				defer MessagePoolReturn(wire)
				pack := decodeSendPackLifecycleWirePack(t, wire)
				id, err := IdFromBytes(pack.MessageId)
				if err != nil || id != messageId || pack.SequenceNumber != number {
					t.Fatal("changed carrier accepted a different logical recovery")
				}
			}
			accept(fixture.start.Add(400*time.Millisecond), 1)
			var observed retryObservation
			select {
			case observed = <-firstRetry:
			default:
				t.Fatal("actual H3 retry did not publish its completed owner boundary")
			}
			service.stateLock.Lock()
			keptDebt := service.next == next && service.probeSent == probe && service.sent == sent &&
				service.pacingReservations == 0 && service.reservedByteCount == 0 && service.waiterHead == nil && service.waiterTail == nil
			service.stateLock.Unlock()
			if observed.messageId != messageId || observed.number != number || observed.at != fixture.start.Add(400*time.Millisecond) ||
				observed.copies != 2 || observed.deadline != fixture.start.Add(900*time.Millisecond) ||
				observed.unreliable != unreliable || !observed.carrierChanged || observed.route != h3Route ||
				observed.lifetime != lifetime || observed.physical != previousH1 || !keptDebt {
				t.Fatal("changed carrier borrowed the H1 clock, refunded paid debt or changed retained ownership/lifetime")
			}
			time.Sleep(time.Until(fixture.start.Add(900*time.Millisecond - time.Nanosecond)))
			synctest.Wait()
			if len(h3Route) != 0 || len(fixture.secondDue) != 0 || fixture.sequence.resendWriteCount.Load() != 1 {
				t.Fatal("changed carrier retried before its original 900ms boundary")
			}
			time.Sleep(time.Nanosecond)
			synctest.Wait()
			select {
			case boundary := <-fixture.secondDue:
				if boundary.unreliable != unreliable || !boundary.carrierChanged || boundary.physical != previousH1 ||
					boundary.deadline != fixture.start.Add(900*time.Millisecond) || boundary.at != boundary.deadline || boundary.copies != 2 ||
					len(h3Route) != 0 || fixture.sequence.resendWriteCount.Load() != 1 {
					t.Fatalf("changed carrier borrowed the H1 physical interval: %+v", boundary)
				}
			default:
				t.Fatal("changed carrier missed its original 900ms recovery interval")
			}
			close(resumeSecondDue)
			synctest.Wait()
			accept(fixture.start.Add(900*time.Millisecond), 2)
			t.Logf("unreliable=%t actual_h3_acceptances=400ms,900ms old_h1_waiter=%s original_lifetime=%s",
				unreliable, previousH1.Sub(fixture.start), lifetime.Sub(observed.created))
		})
	}
}

// Cancellation during the retry wait joins the worker and returns its retained
// bytes without manufacturing a second physical copy or waiting for a timer.
func TestWindowPacingRetryPhysicalWaitCancellation(t *testing.T) {
	runWindowRetryClockFixture(t, 4*time.Second, time.Minute, func(t *testing.T, fixture *windowRetryClockFixture) {
		fixture.delayRetry(1200 * time.Millisecond)
		time.Sleep(time.Second)
		synctest.Wait()
		fixture.cancel()
		if err := fixture.client.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
		// A due-loop inspection while unwinding cancellation is not a write.
		// Count physical acceptance and retained ownership after joining.
		if len(fixture.route) != 0 || fixture.sequence.resendWriteCount.Load() != 0 || !fixture.sequence.windowPacer.waiter.sentAt.Equal(fixture.start) {
			t.Fatal("canceled pacing wait emitted a physical retry")
		}
		select {
		case at := <-fixture.ackFailedAt:
			if at != fixture.start.Add(time.Second) {
				t.Fatal("cancellation waited for a physical or recovery deadline")
			}
		default:
			t.Fatal("canceled retained Pack did not complete ownership")
		}
	})
}
