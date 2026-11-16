// Idle retirement must inspect activity without waiting for a producer whose
// admission needs that same worker to keep consuming.
package connect

import (
	"context"
	"crypto/tls"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Hold the TLS server flight after ClientHello delivery while a Required
// application Pack owns its admission read lock. The idle check must return
// before the flight is released, then handshake controls and data must flow.
func TestRequiredEncryptionIdleCheckPreservesHandshakeAdmission(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	clientHelloReceived := make(chan struct{})
	serverFlightReady := make(chan struct{})
	applicationWaiting := make(chan struct{})
	idleChecked := make(chan struct{})
	var clientHelloOnce, applicationOnce, idleOnce sync.Once
	var idleClosed atomic.Bool
	var serverReceived atomic.Bool
	var destinationId atomic.Pointer[Id]
	a, b, _, peerId, _, receivesB := requiredGatePair(
		ctx, EncryptionModeRequired, EncryptionModeRequired,
		func(settings *ClientSettings) {
			settings.Log = NewNoopLogger()
			settings.SendBufferSettings.IdleTimeout = 50 * time.Millisecond
			settings.SendBufferSettings.SequenceBufferSize = 0
			settings.SendBufferSettings.beforeRunSendSequenceForTest = func(id sendSequenceId) {
				if destination := destinationId.Load(); destination != nil && id.Destination == *destination {
					select {
					case <-applicationWaiting:
					case <-ctx.Done():
					}
				}
			}
			settings.SendBufferSettings.beforeRequiredEncryptionWaitForTest = func(id sendSequenceId) {
				if destination := destinationId.Load(); destination != nil && id.Destination == *destination {
					applicationOnce.Do(func() { close(applicationWaiting) })
				}
			}
			settings.SendBufferSettings.afterIdleCloseForTest = func(id sendSequenceId, closed bool) {
				if destination := destinationId.Load(); destination != nil && id.Destination == *destination && serverReceived.Load() {
					idleOnce.Do(func() {
						idleClosed.Store(closed)
						close(idleChecked)
					})
				}
			}
			serverTlsConfig, err := DefaultSequenceServerTlsConfig()
			if err != nil {
				t.Fatal(err)
			}
			serverTlsConfig.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
				serverReceived.Store(true)
				clientHelloOnce.Do(func() { close(clientHelloReceived) })
				select {
				case <-serverFlightReady:
					return nil, nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			settings.EncryptionSettings.ServerTlsConfig = serverTlsConfig
		}, true,
	)
	destinationId.Store(&peerId)
	sendDone := make(chan struct{})
	defer func() {
		cancel()
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		waitCloseWaitBarrier(t, closeCtx, sendDone, "encrypted application sender cleanup")
		for _, client := range []*Client{a, b} {
			if err := client.CloseAndWait(closeCtx); err != nil {
				t.Errorf("join encrypted idle fixture: %v", err)
			}
		}
	}()

	sent := make(chan bool, 1)
	frame := requiredGateFrame(t, "after-idle-handshake")
	go func() {
		defer close(sendDone)
		accepted := a.SendWithTimeout(frame, peerId, func(error) {}, -1)
		if !accepted {
			MessagePoolReturn(frame.MessageBytes)
		}
		sent <- accepted
	}()
	waitCloseWaitBarrier(t, ctx, applicationWaiting, "required application admission")
	waitCloseWaitBarrier(t, ctx, clientHelloReceived, "TLS ClientHello delivery")
	waitCloseWaitBarrier(t, ctx, idleChecked, "idle check behind required application admission")
	if idleClosed.Load() {
		t.Fatal("idle check retired a sequence with a pending application Pack")
	}
	close(serverFlightReady)
	select {
	case accepted := <-sent:
		if !accepted {
			t.Fatal("required send failed after the server flight was released")
		}
	case <-ctx.Done():
		t.Fatal("handshake controls could not pass the parked application send")
	}
	select {
	case content := <-receivesB:
		if content != "after-idle-handshake" {
			t.Fatalf("unexpected encrypted content: %q", content)
		}
	case <-ctx.Done():
		t.Fatal("encrypted data did not arrive after the idle check")
	}
}

// The receive worker may select its idle timer while a reliable producer owns
// Pack's mutex and waits for its synchronous handoff. The activity reservation
// is sufficient to reject retirement; waiting for that mutex strands the reader.
func TestReceiveSequenceIdleCheckDoesNotWaitForAdmission(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), closeWaitClientSettings())
	idleChecks := make(chan bool, 2)
	settings := DefaultReceiveBufferSettings()
	settings.IdleTimeout = time.Millisecond
	settings.afterIdleCloseForTest = func(_ receiveSequenceId, closed bool) {
		select {
		case idleChecks <- closed:
		case <-ctx.Done():
		}
	}
	sequence := NewReceiveSequence(ctx, client, SourceId(NewId()), NewId(),
		sequenceTlsRoleServer, false, settings)
	sequence.packMutex.Lock()
	sequence.idleCondition.UpdateOpen()
	held := true
	releaseAdmission := func() {
		if held {
			held = false
			sequence.idleCondition.UpdateClose()
			sequence.packMutex.Unlock()
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		sequence.Run()
	}()
	defer func() {
		cancel()
		releaseAdmission()
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		waitCloseWaitBarrier(t, closeCtx, done, "receive idle worker cleanup")
		sequence.Close()
		if err := client.CloseAndWait(closeCtx); err != nil {
			t.Errorf("join receive idle client: %v", err)
		}
	}()
	select {
	case closed := <-idleChecks:
		if closed {
			t.Fatal("receive worker retired with an open admission")
		}
	case <-ctx.Done():
		t.Fatal("receive idle check waited for its blocked producer")
	}
	releaseAdmission()
	waitCloseWaitBarrier(t, ctx, done, "receive retirement after admission completes")
}

// Forwarding has the same unbuffered producer/worker ordering as receiving.
// Hold the exact producer state before Run and require both retained activity
// and normal retirement once that activity is released.
func TestForwardSequenceIdleCheckDoesNotWaitForAdmission(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), closeWaitClientSettings())
	idleChecks := make(chan bool, 2)
	settings := DefaultForwardBufferSettings()
	settings.IdleTimeout = time.Millisecond
	settings.afterIdleCloseForTest = func(_ TransferPath, closed bool) {
		select {
		case idleChecks <- closed:
		case <-ctx.Done():
		}
	}
	sequence := NewForwardSequence(ctx, client, DestinationId(NewId()), settings)
	sequence.packMutex.Lock()
	sequence.idleCondition.UpdateOpen()
	held := true
	releaseAdmission := func() {
		if held {
			held = false
			sequence.idleCondition.UpdateClose()
			sequence.packMutex.Unlock()
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		sequence.Run()
	}()
	defer func() {
		cancel()
		releaseAdmission()
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		waitCloseWaitBarrier(t, closeCtx, done, "forward idle worker cleanup")
		sequence.Close()
		if err := client.CloseAndWait(closeCtx); err != nil {
			t.Errorf("join forward idle client: %v", err)
		}
	}()
	select {
	case closed := <-idleChecks:
		if closed {
			t.Fatal("forward worker retired with an open admission")
		}
	case <-ctx.Done():
		t.Fatal("forward idle check waited for its blocked producer")
	}
	releaseAdmission()
	waitCloseWaitBarrier(t, ctx, done, "forward retirement after admission completes")
}
