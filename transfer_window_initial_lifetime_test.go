// Initial physical recovery and the original ACK lifetime are separate bounds.
package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The owner publishes these values after physical confirmation and retiming.
type windowInitialLifetimeBoundary struct {
	at       time.Time
	offered  time.Time
	physical time.Time
	recovery time.Time
	lifetime time.Time
}

// One real sender is held before dispatch so the test can place its reservation.
type windowInitialLifetimeFixture struct {
	start    time.Time
	client   *Client
	sequence *SendSequence
	route    Route
	written  chan windowInitialLifetimeBoundary
	release  chan struct{}
	failed   chan time.Time
	cancel   context.CancelFunc
	cleanup  func()
}

// Workers and every transferred route buffer are owned and joined by this row.
func runWindowInitialLifetimeFixture(t *testing.T, delay, lifetime time.Duration, retained bool, check func(*testing.T, *windowInitialLifetimeFixture), configure ...func(*windowInitialLifetimeFixture)) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		startWorker := make(chan struct{})
		destination := NewId()
		fixture := &windowInitialLifetimeFixture{
			route: make(Route, 8), written: make(chan windowInitialLifetimeBoundary, 1),
			release: make(chan struct{}), failed: make(chan time.Time, 1), cancel: cancel,
		}
		settings := DefaultClientSettings()
		settings.Log = NewNoopLogger()
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		settings.SendBufferSettings.WindowSizing = WindowSizingFromDelivery
		settings.SendBufferSettings.ApplyWindowSizing()
		settings.SendBufferSettings.MinResendInterval = 300 * time.Millisecond
		settings.SendBufferSettings.RttMinResendInterval = 300 * time.Millisecond
		settings.SendBufferSettings.MaxResendInterval = 4 * time.Second
		settings.SendBufferSettings.AckTimeout = lifetime
		settings.SendBufferSettings.beforeRunSendSequenceForTest = func(id sendSequenceId) {
			if id.Destination == destination {
				select {
				case <-startWorker:
				case <-ctx.Done():
				}
			}
		}
		settings.SendBufferSettings.afterInitialWriteQueuedForTest = func(id sendSequenceId, number uint64) {
			if id.Destination == destination && number == 0 {
				item := fixture.sequence.resendQueue.PeekFirst()
				if !item.transportWriteObserved {
					return
				}
				fixture.written <- windowInitialLifetimeBoundary{
					at: time.Now(), offered: item.sendTime,
					physical: fixture.sequence.windowPacer.waiter.sentAt,
					recovery: item.resendTime, lifetime: item.sendTime.Add(item.ackTimeout),
				}
				select {
				case <-fixture.release:
				case <-ctx.Done():
				}
			}
		}
		fixture.client = NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
		defer func() {
			cancel()
			if fixture.cleanup != nil {
				fixture.cleanup()
			}
			if err := fixture.client.CloseAndWait(context.Background()); err != nil {
				t.Errorf("client cleanup: %v", err)
			}
			for len(fixture.route) > 0 {
				MessagePoolReturn(<-fixture.route)
			}
		}()
		fixture.client.ContractManager().AddNoContractPeer(destination)
		fixture.client.RouteManager().UpdateTransport(&h1SendClientTransportForGroupTest{sendClientTransport: NewSendClientTransport(DestinationId(destination))}, []Route{fixture.route})
		fixture.sequence = fixture.client.sendBuffer.createSendSequence(sendSequenceId{Destination: destination}, &SendPack{Destination: destination})
		if fixture.sequence == nil || fixture.sequence.windowPacer.service == nil {
			t.Fatal("no shared pacing service")
		}
		fixture.start = time.Now()
		service := fixture.sequence.windowPacer.service
		service.stateLock.Lock()
		service.next = fixture.start.Add(delay)
		service.stateLock.Unlock()
		for _, configure := range configure {
			configure(fixture)
		}
		frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(1280)}
		clear(frame.MessageBytes)
		if !fixture.client.SendWithTimeout(frame, destination, func(err error) {
			if err != nil {
				fixture.failed <- time.Now()
			}
		}, time.Second, sendPackRecoveryOption{retainAfterAckTimeout: retained}) {
			MessagePoolReturn(frame.MessageBytes)
			t.Fatal("Pack not admitted")
		}
		close(startWorker)
		synctest.Wait()
		check(t, fixture)
	})
}

// The resend timestamp may exceed the lifetime; the owner must wake separately
// at the original lifetime and retire the bytes before an unnecessary retry.
func TestWindowPacingInitialWriteKeepsOriginalAckLifetime(t *testing.T) {
	runWindowInitialLifetimeFixture(t, 400*time.Millisecond, 500*time.Millisecond, false, func(t *testing.T, fixture *windowInitialLifetimeFixture) {
		time.Sleep(400 * time.Millisecond)
		synctest.Wait()
		boundary := <-fixture.written
		if boundary.offered != fixture.start || boundary.physical != fixture.start.Add(400*time.Millisecond) || boundary.recovery != fixture.start.Add(700*time.Millisecond) || boundary.lifetime != fixture.start.Add(500*time.Millisecond) {
			t.Fatalf("wrong forced boundary: %+v", boundary)
		}
		MessagePoolReturn(<-fixture.route)
		close(fixture.release)
		synctest.Wait()
		time.Sleep(100*time.Millisecond - time.Nanosecond)
		synctest.Wait()
		if len(fixture.failed) != 0 || len(fixture.route) != 0 {
			t.Fatal("initial write retired or retried before the lifetime boundary")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		select {
		case at := <-fixture.failed:
			t.Logf("offer=0 physical=%s retained-recovery=%s lifetime=%s callback=%s", boundary.physical.Sub(fixture.start), boundary.recovery.Sub(fixture.start), boundary.lifetime.Sub(fixture.start), at.Sub(fixture.start))
			if at != boundary.lifetime || len(fixture.route) != 0 || fixture.sequence.resendQueue.Len() != 0 {
				t.Fatal("initial physical retime extended the original ACK lifetime")
			}
		default:
			t.Fatal("missing reply was retained until the later physical recovery deadline")
		}
	})
}

// A longer lifetime still permits the first 300 ms physical recovery, then
// independently retires the item at its original 900 ms lifetime.
func TestWindowPacingInitialRecoveryPrecedesLongerAckLifetime(t *testing.T) {
	runWindowInitialLifetimeFixture(t, 400*time.Millisecond, 900*time.Millisecond, false, func(t *testing.T, fixture *windowInitialLifetimeFixture) {
		time.Sleep(400 * time.Millisecond)
		synctest.Wait()
		boundary := <-fixture.written
		if boundary.physical != fixture.start.Add(400*time.Millisecond) || boundary.recovery != fixture.start.Add(700*time.Millisecond) {
			t.Fatalf("wrong initial recovery boundary: %+v", boundary)
		}
		MessagePoolReturn(<-fixture.route)
		close(fixture.release)
		synctest.Wait()
		time.Sleep(300*time.Millisecond - time.Nanosecond)
		synctest.Wait()
		if len(fixture.route) != 0 || len(fixture.failed) != 0 {
			t.Fatal("initial recovery spent local pacing time")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if len(fixture.route) != 1 || len(fixture.failed) != 0 {
			t.Fatal("missing reply was not recovered at physical+300ms")
		}
		MessagePoolReturn(<-fixture.route)
		time.Sleep(200*time.Millisecond - time.Nanosecond)
		synctest.Wait()
		if len(fixture.failed) != 0 {
			t.Fatal("longer ACK lifetime ended early")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		select {
		case at := <-fixture.failed:
			t.Logf("offer=0 physical=400ms first-recovery=700ms lifetime=900ms callback=%s", at.Sub(fixture.start))
			if at != fixture.start.Add(900*time.Millisecond) || len(fixture.route) != 0 {
				t.Fatal("physical recovery renewed the original lifetime")
			}
		default:
			t.Fatal("longer lifetime did not retire the unanswered item")
		}
	})
}

// Diagnostic root: when local pacing exceeds lifetime, retain the physical
// and callback order even on failure. This does not authorize a timer policy.
func TestWindowPacingInitialWaitCannotOutliveAckLifetime(t *testing.T) {
	runWindowInitialLifetimeFixture(t, 600*time.Millisecond, 500*time.Millisecond, false, func(t *testing.T, fixture *windowInitialLifetimeFixture) {
		time.Sleep(500*time.Millisecond - time.Nanosecond)
		synctest.Wait()
		if len(fixture.route) != 0 || len(fixture.failed) != 0 {
			t.Fatal("fixture completed before the original lifetime")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		completed := false
		select {
		case at := <-fixture.failed:
			completed = at == fixture.start.Add(500*time.Millisecond)
			t.Logf("callback-at-lifetime=%s", at.Sub(fixture.start))
		default:
		}
		t.Logf("at-lifetime=500ms callback=%t physical-writes=%d", completed, len(fixture.route))
		if completed {
			if len(fixture.route) != 0 {
				t.Fatal("expired initial item physically dispatched")
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()
		select {
		case boundary := <-fixture.written:
			t.Logf("late-physical=%s offer=%s retained-recovery=%s original-lifetime=%s", boundary.physical.Sub(fixture.start), boundary.offered.Sub(fixture.start), boundary.recovery.Sub(fixture.start), boundary.lifetime.Sub(fixture.start))
			close(fixture.release)
		default:
			t.Fatal("pacing neither expired nor reached its forced physical boundary")
		}
		synctest.Wait()
		select {
		case at := <-fixture.failed:
			t.Logf("late-callback=%s physical-writes=%d", at.Sub(fixture.start), len(fixture.route))
		default:
			t.Error("item still retained after delayed initial dispatch")
		}
		t.Fatal("original ACK lifetime did not cancel the initial pacing wait before physical dispatch")
	})
}

// A retained record owns bytes that no upstream layer can regenerate. Its
// existing exception must survive pacing beyond the ordinary ACK lifetime.
func TestWindowPacingInitialWaitRetainsNonRegenerableBytes(t *testing.T) {
	runWindowInitialLifetimeFixture(t, 600*time.Millisecond, 500*time.Millisecond, true, func(t *testing.T, fixture *windowInitialLifetimeFixture) {
		time.Sleep(500 * time.Millisecond)
		synctest.Wait()
		if len(fixture.route) != 0 || len(fixture.failed) != 0 {
			t.Fatal("retained record lost its recovery lifetime")
		}
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()
		select {
		case boundary := <-fixture.written:
			if boundary.physical != fixture.start.Add(600*time.Millisecond) {
				t.Fatal("retained write missed its paced release")
			}
			MessagePoolReturn(<-fixture.route)
			close(fixture.release)
		default:
			t.Fatal("retained write expired at the ordinary lifetime")
		}
		synctest.Wait()
		if len(fixture.failed) != 0 {
			t.Fatal("retained write was failed at its ordinary lifetime")
		}
	})
}
