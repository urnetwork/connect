// Test relays own received wire roots until downstream handoff or disposal.
package connect

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Takes the message. Successful handoff transfers it; cancellation and a nil
// sink return it locally. The caller relinquishes its share in either case.
func forwardOwnedTestWire(ctx context.Context, out chan<- []byte, message []byte) bool {
	if out == nil {
		MessagePoolReturn(message)
		return true
	}
	select {
	case <-ctx.Done():
		MessagePoolReturn(message)
		return false
	case out <- message:
		return true
	}
}

// Takes each received root, lets the observer borrow it, then forwards or
// disposes it. The returned channel joins the relay after custody ends.
func startOwnedTestWireTap(ctx context.Context, in <-chan []byte, out chan<- []byte, observe func([]byte)) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case message, ok := <-in:
				if !ok {
					return
				}
				if observe != nil {
					observe(message)
				}
				if !forwardOwnedTestWire(ctx, out, message) {
					return
				}
			}
		}
	}()
	return done
}

// Takes the frame: admission transfers it to the client; refusal returns it.
func sendOwnedTestFrame(client *Client, frame *protocol.Frame, destinationId Id) bool {
	if client.Send(frame, destinationId, func(error) {}) {
		return true
	}
	MessagePoolReturn(frame.MessageBytes)
	return false
}

// One witness keeps this exact 256-byte root valid independently of unrelated
// pool activity. Its final return proves the joined relay kept no share.
func ownedTestWireWithWitness(t *testing.T) ([]byte, []byte) {
	t.Helper()
	message := MessagePoolGet(32)
	copy(message, bytes.Repeat([]byte{0x5a}, len(message)))
	witness := MessagePoolShareReadOnly(message)
	pooled, shared := MessagePoolCheck(witness)
	if !pooled || !shared || MessagePoolRootByteCount(witness) != 256 {
		t.Fatal("wire control did not acquire a shared 256-byte pooled root")
	}
	return message, witness
}

// Called only after every potential owner is joined. A failed negative
// control has exactly one extra share, which is reclaimed before reporting.
func requireOwnedTestWireFinalReturn(t *testing.T, witness []byte, marker string) {
	t.Helper()
	if !MessagePoolReturn(witness) {
		MessagePoolReturn(witness)
		t.Errorf("%s: joined worker retained the exact 256-byte root", marker)
	}
}

// Pool diagnostics have process lifetime and must be created outside the
// synctest bubble, including when this is the first selected root in a process.
// The balanced warm-up leaves no owner and changes no ownership assertion.
func ownedTestWireSynctest(t *testing.T, body func(*testing.T)) {
	t.Helper()
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, body)
}

// Cancellation is forced while the observer borrows the already-owned wire;
// no downstream receiver exists, so a handoff cannot win the cancellation.
func TestOwnedTestWireTapReturnsHeldRootBeforeJoinOnCancel(t *testing.T) {
	ownedTestWireSynctest(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		in, out := make(chan []byte), make(chan []byte)
		held, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		resume := func() { releaseOnce.Do(func() { close(release) }) }
		message, witness := ownedTestWireWithWitness(t)
		done := startOwnedTestWireTap(ctx, in, out, func(borrowed []byte) {
			if &borrowed[0] != &witness[0] || !bytes.Equal(borrowed, witness) {
				t.Error("tap observer did not borrow the original root")
			}
			close(held)
			<-release
		})
		t.Cleanup(func() { cancel(); resume(); <-done })
		in <- message
		<-held
		cancel()
		resume()
		<-done
		select {
		case <-out:
			t.Fatal("canceled tap fabricated a downstream handoff")
		default:
		}
		requireOwnedTestWireFinalReturn(t, witness, "OWNED_TAP_CANCEL_ROOT_HELD")
	})
}

// A completed handoff belongs to the downstream receiver even if the relay
// is canceled immediately afterwards. Its witness return must remain nonfinal.
func TestOwnedTestWireTapTransfersRootExactlyOnceOnHandoff(t *testing.T) {
	ownedTestWireSynctest(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		in, out := make(chan []byte), make(chan []byte)
		message, witness := ownedTestWireWithWitness(t)
		done := startOwnedTestWireTap(ctx, in, out, nil)
		in <- message
		received := <-out
		cancel()
		<-done
		if &received[0] != &witness[0] || !bytes.Equal(received, witness) {
			t.Fatal("tap changed the handed-off root")
		}
		if MessagePoolReturn(witness) {
			t.Fatal("OWNED_TAP_HANDOFF_RETURNED: tap returned the downstream owner's share")
		}
		if !MessagePoolReturn(received) {
			t.Fatal("tap handoff did not preserve exactly one downstream share")
		}
	})
}

// Loss consumes an admitted wire; it must return that root rather than
// silently abandoning it. Quiescence places the assertion after the drop.
func TestConditionerReturnsDroppedOwnedRoot(t *testing.T) {
	ownedTestWireSynctest(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		in := make(chan []byte)
		conditioner, _ := newConditioner(ctx, in)
		conditioner.update(func() { conditioner.lossProbability = 1 })
		message, witness := ownedTestWireWithWitness(t)
		in <- message
		synctest.Wait()
		cancel()
		<-conditioner.done
		requireOwnedTestWireFinalReturn(t, witness, "CONDITIONER_LOSS_ROOT_HELD")
	})
}

// The undelayed relay is durably blocked on an unbuffered downstream route.
func TestConditionerReturnsOwnedRootOnCanceledHandoff(t *testing.T) {
	ownedTestWireSynctest(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		in := make(chan []byte)
		conditioner, _ := newConditioner(ctx, in)
		message, witness := ownedTestWireWithWitness(t)
		in <- message
		synctest.Wait()
		cancel()
		<-conditioner.done
		requireOwnedTestWireFinalReturn(t, witness, "CONDITIONER_HANDOFF_ROOT_HELD")
	})
}

// All admitted delayed workers are still parked at their real timer when
// cancellation starts. The conditioner join covers every one of their roots.
func TestConditionerJoinsEveryDelayedRootOnCancel(t *testing.T) {
	ownedTestWireSynctest(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		in := make(chan []byte)
		conditioner, _ := newConditioner(ctx, in)
		conditioner.update(func() { conditioner.fixedDelay = time.Hour })
		var witnesses [][]byte
		for range 4 {
			message, witness := ownedTestWireWithWitness(t)
			witnesses = append(witnesses, witness)
			in <- message
		}
		synctest.Wait()
		cancel()
		<-conditioner.done
		for _, witness := range witnesses {
			requireOwnedTestWireFinalReturn(t, witness, "CONDITIONER_DELAY_ROOT_HELD")
		}
	})
}

// Virtual time expires the configured delay while no receiver exists. The
// worker must then relinquish its held root at the canceled handoff.
func TestConditionerReturnsDelayedRootOnCanceledHandoff(t *testing.T) {
	ownedTestWireSynctest(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		in := make(chan []byte)
		conditioner, _ := newConditioner(ctx, in)
		conditioner.update(func() { conditioner.fixedDelay = time.Second })
		message, witness := ownedTestWireWithWitness(t)
		in <- message
		synctest.Wait()
		time.Sleep(2 * time.Second)
		synctest.Wait()
		cancel()
		<-conditioner.done
		requireOwnedTestWireFinalReturn(t, witness, "CONDITIONER_DELAYED_HANDOFF_ROOT_HELD")
	})
}

// Both conditioner paths transfer the original share without returning it.
// Closing the input joins the main loop after the downstream transition.
func TestConditionerTransfersImmediateAndDelayedRootsExactlyOnce(t *testing.T) {
	for _, delay := range []time.Duration{0, time.Second} {
		ownedTestWireSynctest(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			in := make(chan []byte)
			conditioner, out := newConditioner(ctx, in)
			conditioner.update(func() { conditioner.fixedDelay = delay })
			message, witness := ownedTestWireWithWitness(t)
			in <- message
			received := <-out
			close(in)
			<-conditioner.done
			if &received[0] != &witness[0] || !bytes.Equal(received, witness) {
				t.Fatalf("delay=%s changed the handed-off root", delay)
			}
			if MessagePoolReturn(witness) {
				t.Fatalf("delay=%s conditioner returned the downstream owner's share", delay)
			}
			if !MessagePoolReturn(received) {
				t.Fatalf("delay=%s conditioner did not preserve one downstream share", delay)
			}
		})
	}
}

// The actual ControlId route sink consumes its wire share and joins. The
// writer's successful channel publication transfers the original share.
func TestControlBlackholeReturnsOwnedRootBeforeJoin(t *testing.T) {
	ownedTestWireSynctest(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		manager := NewRouteManager(ctx, "synthetic-control-sink")
		done := blackholeControlId(ctx, manager)
		writer := manager.OpenMultiRouteWriter(DestinationId(ControlId))
		defer manager.CloseMultiRouteWriter(writer)
		message, witness := ownedTestWireWithWitness(t)
		if err := writer.Write(ctx, message, -1); err != nil {
			MessagePoolReturn(message)
			requireOwnedTestWireFinalReturn(t, witness, "CONTROL_SINK_FAILED_WRITE_ROOT_HELD")
			t.Fatal(err)
		}
		cancel()
		<-done
		requireOwnedTestWireFinalReturn(t, witness, "CONTROL_SINK_ROOT_HELD")
	})
}

// An application producer still owns a frame when shutdown rejects Send.
// The same helper is used by the joined identity-proof producer.
func TestIdentityProofProducerReturnsRefusedOwnedFrame(t *testing.T) {
	ownedTestWireSynctest(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		settings := DefaultClientSettings()
		settings.Log = NewNoopLogger()
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
		cancel()
		joinCtx, joinCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer joinCancel()
		if err := client.CloseAndWait(joinCtx); err != nil {
			t.Fatal(err)
		}
		message, witness := ownedTestWireWithWitness(t)
		frame := &protocol.Frame{MessageType: protocol.MessageType_TestSimpleMessage, MessageBytes: message}
		if sendOwnedTestFrame(client, frame, NewId()) {
			t.Fatal("closed client accepted the producer's frame")
		}
		requireOwnedTestWireFinalReturn(t, witness, "IDENTITY_PRODUCER_REFUSAL_ROOT_HELD")
	})
}

// The real synchronous grant consumes its request, returns local serialization
// scratch, and lends the response only until the callback returns.
func TestGrantingClientOobReturnsRequestResponseAndScratchRoots(t *testing.T) {
	destinationId, sourceId := NewId(), NewId()
	secretKey := bytes.Repeat([]byte{0x6b}, 32)
	settings := DefaultContractManagerSettings()
	frame, err := ToFrame(&protocol.CreateContract{
		DestinationId:     destinationId.Bytes(),
		TransferByteCount: 1024,
	}, DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	requestWitness := MessagePoolShareReadOnly(frame.MessageBytes)
	var scratchWitness, responseWitness []byte
	callbackCount, scratchCount := 0, 0
	var decoded *protocol.CreateContractResult
	oob := &grantingClientOob{
		sourceId: sourceId,
		settings: settings,
		destSecretKey: func(actual Id) ([]byte, bool) {
			return secretKey, actual == destinationId
		},
		beforeStoredContractReturnForTest: func(scratch []byte) {
			scratchCount++
			scratchWitness = MessagePoolShareReadOnly(scratch)
		},
	}
	oob.SendControl([]*protocol.Frame{frame}, func(results []*protocol.Frame, err error) {
		callbackCount++
		if err != nil || len(results) != 1 {
			t.Errorf("grant callback got %d results: %v", len(results), err)
			return
		}
		if pooled, _ := MessagePoolCheck(results[0].MessageBytes); !pooled {
			t.Error("grant response was returned before its borrowing callback")
			return
		}
		responseWitness = MessagePoolShareReadOnly(results[0].MessageBytes)
		message, decodeErr := FromFrame(results[0])
		if decodeErr != nil {
			t.Error(decodeErr)
			return
		}
		decoded, _ = message.(*protocol.CreateContractResult)
	})
	for _, witness := range [][]byte{requestWitness, scratchWitness, responseWitness} {
		if witness == nil || MessagePoolRootByteCount(witness) != 256 {
			t.Error("grant ownership control did not retain its exact 256-byte witness")
		}
	}
	if scratchWitness != nil && responseWitness != nil && &scratchWitness[0] == &responseWitness[0] {
		t.Error("grant result aliased the local serialization scratch")
	}
	requireOwnedTestWireFinalReturn(t, requestWitness, "GRANTING_OOB_REQUEST_ROOT_HELD")
	if scratchWitness != nil {
		requireOwnedTestWireFinalReturn(t, scratchWitness, "GRANTING_OOB_SCRATCH_ROOT_HELD")
	}
	if responseWitness != nil {
		requireOwnedTestWireFinalReturn(t, responseWitness, "GRANTING_OOB_RESPONSE_ROOT_HELD")
	}
	if callbackCount != 1 || scratchCount != 1 || decoded == nil || decoded.Contract == nil {
		t.Fatalf("grant callbacks=%d scratch=%d decoded=%v", callbackCount, scratchCount, decoded)
	}
	contract := decoded.Contract
	if !VerifyStoredContract(settings, secretKey, contract.StoredContractBytes, contract.StoredContractHmac) {
		t.Fatal("grant changed its signed result while relinquishing pooled roots")
	}
	var stored protocol.StoredContract
	if err := ProtoUnmarshal(contract.StoredContractBytes, &stored); err != nil ||
		!bytes.Equal(stored.SourceId, sourceId.Bytes()) || !bytes.Equal(stored.DestinationId, destinationId.Bytes()) || stored.TransferByteCount != 1024 {
		t.Fatalf("owned decoded grant did not survive callback disposal: %v %+v", err, &stored)
	}
}
