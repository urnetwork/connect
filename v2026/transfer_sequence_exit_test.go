package connect

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Keep the reliable P2P policy while replacing only the physical network with
// a bounded route. The real SendSequence, ACK coalescer and callbacks still run.
type sequenceExitP2pTransport struct{ Transport }

func (*sequenceExitP2pTransport) TransportType() TransportType { return TransportTypeP2p }

// All three causes used to collapse into the same callback text. An ACK expiry
// must be recorded before its own cancellation and before callback teardown;
// a canceled parent or failed ACK worker must not be labelled missing feedback.
func TestSendSequenceExitDiscriminator(t *testing.T) {
	for _, cause := range []string{"ack_lifetime", "context", "ack_worker_exit", "timely_ack"} {
		t.Run(cause, func(t *testing.T) {
			assertMessagePoolOwnership(t)
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				destination := NewId()
				log := newRecordingLogger()
				settings := DefaultClientSettings()
				settings.Log = log
				settings.EncryptionSettings.Mode = EncryptionModeOff
				settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
				settings.SendBufferSettings.AckTimeout = DefaultMultiClientSettings().AckTimeout
				if cause == "ack_worker_exit" {
					settings.SendBufferSettings.afterAckCoalescedForTest = func(id sendSequenceId, _ uint64) {
						if id.Destination == destination {
							panic(errors.New("deterministic ACK-worker exit"))
						}
					}
				}
				client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
				client.ContractManager().AddNoContractPeer(destination)
				route := make(Route, 256)
				client.RouteManager().UpdateTransport(&sequenceExitP2pTransport{
					Transport: NewSendClientTransport(DestinationId(destination)),
				}, []Route{route})
				defer func() {
					cancel()
					if err := client.CloseAndWait(context.Background()); err != nil {
						t.Errorf("join sender: %v", err)
					}
					for len(route) > 0 {
						MessagePoolReturn(<-route)
					}
				}()
				failed := make(chan error, 4)
				frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(64)}
				clear(frame.MessageBytes)
				if !client.SendWithTimeout(frame, destination, func(err error) { failed <- err }, time.Second) {
					MessagePoolReturn(frame.MessageBytes)
					t.Fatal("initial packet not admitted")
				}
				synctest.Wait()
				if len(route) != 1 {
					t.Fatalf("initial route writes=%d, want 1", len(route))
				}
				wire := <-route
				pack := decodeSendPackLifecycleWirePack(t, wire)
				MessagePoolReturn(wire)
				client.sendBuffer.mutex.Lock()
				sequence := client.sendBuffer.sendSequences[sendSequenceId{Destination: destination}]
				client.sendBuffer.mutex.Unlock()
				if sequence == nil {
					t.Fatal("no live send sequence")
				}
				switch cause {
				case "ack_lifetime":
					time.Sleep(settings.SendBufferSettings.AckTimeout)
				case "context":
					time.Sleep(time.Second)
					cancel()
				case "ack_worker_exit":
					time.Sleep(time.Second)
					ok, err := sequence.ackMessage(receiveAckMessage{
						sequenceId: sequence.sequenceId, messageId: RequireIdFromBytes(pack.MessageId), selective: true,
					}, 0)
					if !ok || err != nil {
						t.Fatalf("ACK worker input: %t %v", ok, err)
					}
				case "timely_ack":
					time.Sleep(settings.SendBufferSettings.AckTimeout - time.Second)
					acknowledgeSendPackLifecycleWirePack(t, client, destination, pack)
					synctest.Wait()
					time.Sleep(2 * time.Second)
				}
				synctest.Wait()
				lines := log.linesWith("event=sequence_exit")
				var relevant []string
				for _, line := range lines {
					if strings.Contains(line, "destination="+destination.String()) {
						relevant = append(relevant, line)
					}
				}
				if cause == "timely_ack" {
					if len(relevant) != 0 || sequence.ctx.Err() != nil {
						t.Fatalf("timely ACK fabricated sequence failure: %v", relevant)
					}
					if len(failed) != 1 || <-failed != nil {
						t.Fatal("timely ACK did not complete the real callback")
					}
					return
				}
				if len(relevant) != 1 || !strings.Contains(relevant[0], "reason="+cause+" ") {
					t.Fatalf("first sequence exit reason=%v, want exactly %s", relevant, cause)
				}
				if !strings.Contains(relevant[0], "sequence="+sequence.sequenceId.String()) {
					t.Fatalf("missing exact sequence identity: %s", relevant[0])
				}
				if cause == "ack_lifetime" && (!strings.Contains(relevant[0], "message="+RequireIdFromBytes(pack.MessageId).String()) ||
					!strings.Contains(relevant[0], "ctx=<nil> parent_ctx=<nil>")) {
					t.Fatalf("expiry lost due identity or was observed after cleanup: %s", relevant[0])
				}
				if len(failed) != 1 || <-failed == nil {
					t.Fatal("diagnostics changed the terminal callback")
				}
				t.Log(relevant[0])
			})
		})
	}
}

// The route write's lifetime check cancels the owner internally. Waiting until
// Run observes ctx.Done would therefore mislabel this as context cancellation.
// The event must retain the exact due record and its never-written evidence.
func TestSendSequenceExitDiscriminatorBlockedRoute(t *testing.T) {
	log := newRecordingLogger()
	runWindowInitialLifetimeFixture(t, 400*time.Millisecond, 500*time.Millisecond, false, func(t *testing.T, fixture *windowInitialLifetimeFixture) {
		time.Sleep(500 * time.Millisecond)
		synctest.Wait()
		if len(fixture.failed) != 1 || len(fixture.written) != 0 || len(fixture.route) != cap(fixture.route) {
			t.Fatal("blocked route did not preserve the original expiry/ownership outcome")
		}
		lines := log.linesWith("event=sequence_exit")
		if len(lines) != 1 {
			t.Fatalf("blocked-write exit events=%v", lines)
		}
		for _, field := range []string{"reason=ack_lifetime ", "ctx=<nil> parent_ctx=<nil>", "route_written=false "} {
			if !strings.Contains(lines[0], field) {
				t.Fatalf("blocked-write cause lost %q: %s", field, lines[0])
			}
		}
		t.Log(lines[0])
	}, func(fixture *windowInitialLifetimeFixture) {
		fixture.sequence.log = log
		fixture.sequence.sendBufferSettings.WriteTimeout = time.Second
		for range cap(fixture.route) {
			wire := MessagePoolGet(16)
			clear(wire)
			fixture.route <- wire
		}
	})
}

// Recovery order is not lifetime order. Capture the actual expired record,
// not whichever younger record happens to be first in the resend heap.
func TestSendSequenceExitDiscriminatorDueIdentity(t *testing.T) {
	log := newRecordingLogger()
	runWindowRetryClockFixture(t, 4*time.Second, 500*time.Millisecond, func(t *testing.T, fixture *windowRetryClockFixture) {
		fixture.sequence.log = log
		older := fixture.sequence.resendQueue.PeekFirst()
		olderID := older.messageId
		fixture.sequence.setResendTime(older, fixture.start.Add(2*time.Second))
		time.Sleep(100 * time.Millisecond)
		frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(1280)}
		clear(frame.MessageBytes)
		if !fixture.client.SendWithTimeout(frame, fixture.sequence.destination, func(error) {}, time.Second) {
			MessagePoolReturn(frame.MessageBytes)
			t.Fatal("younger original not admitted")
		}
		close(fixture.releaseInitial)
		synctest.Wait()
		if len(fixture.route) != 1 {
			t.Fatal("younger original was not written")
		}
		MessagePoolReturn(<-fixture.route)
		time.Sleep(300 * time.Millisecond)
		synctest.Wait()
		if len(fixture.route) != 1 {
			t.Fatal("younger recovery did not precede older lifetime")
		}
		MessagePoolReturn(<-fixture.route)
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()
		lines := log.linesWith("event=sequence_exit")
		if len(lines) != 1 || !strings.Contains(lines[0], "reason=ack_lifetime ") ||
			!strings.Contains(lines[0], "message="+olderID.String()+" ") || !strings.Contains(lines[0], "number=0 ") {
			t.Fatalf("wrong due lifetime identity: %v", lines)
		}
		t.Log(lines[0])
	})
}
