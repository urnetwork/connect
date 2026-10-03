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
	start          time.Time
	client         *Client
	sequence       *SendSequence
	transport      *h1SendClientTransportForGroupTest
	route          Route
	otherRoutes    []Route
	releaseInitial chan struct{}
	secondDue      chan windowRetryClockBoundary
	ackFailedAt    chan time.Time
	cancel         context.CancelFunc
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
				<-ctx.Done()
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
			fixture.delayRetry(1200 * time.Millisecond)
			time.Sleep(400 * time.Millisecond)
			synctest.Wait()
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
			// snapshot. The paced sender is one of those readers.
			synctest.Wait()
			if fixture.sequence.transferFlightPolicy().h1Only {
				t.Fatal("carrier change was not published during the pacing wait")
			}
			time.Sleep(800 * time.Millisecond)
			synctest.Wait()
			select {
			case boundary := <-fixture.secondDue:
				t.Logf("physical=%s deadline=%s due=%s changed=%t unreliable=%t", boundary.physical.Sub(fixture.start), boundary.deadline.Sub(fixture.start), boundary.at.Sub(fixture.start), boundary.carrierChanged, boundary.unreliable)
				if boundary.unreliable != unreliable || !boundary.carrierChanged || boundary.physical != fixture.start.Add(1200*time.Millisecond) || boundary.deadline != fixture.start.Add(900*time.Millisecond) || boundary.at != fixture.start.Add(1200*time.Millisecond) {
					t.Fatalf("changed carrier borrowed the H1 physical interval: %+v", boundary)
				}
			default:
				t.Fatal("changed carrier incorrectly acquired a fresh H1 recovery interval")
			}
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
