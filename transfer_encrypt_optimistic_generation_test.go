// Both wire decoders must preserve handshake generation before the optimistic
// delivery path can touch a TLS transport.
package connect

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Stops the shared receive loop after optimistic delivery and before ordered
// admission. A future sequence number also prevents the ordered drain from
// masking which path delivered the second-flight bytes.
func testOptimisticControlWireGeneration(t *testing.T, controlType protocol.EncryptedControlType, activeEpoch Id, wireEpoch []byte, wantDelivery bool) {
	t.Helper()
	for _, version := range []int{1, 2} {
		func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			peerId := NewId()
			entered := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			settings := DefaultClientSettings()
			settings.EncryptionSettings.Mode = EncryptionModeOpportunistic
			settings.ReceiveBufferSettings.beforeCreateReceiveSequenceForTest = func(id receiveSequenceId) {
				if id.Source.SourceId != peerId {
					return
				}
				once.Do(func() { close(entered) })
				select {
				case <-release:
				case <-ctx.Done():
				}
			}
			receiver := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
			defer func() {
				cancel()
				close(release)
				if err := receiver.CloseAndWait(context.Background()); err != nil {
					t.Errorf("receiver cleanup: %v", err)
				}
			}()
			toReceiver := make(chan []byte, 1)
			receiver.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{toReceiver})
			receiver.ContractManager().AddNoContractPeer(peerId)
			session := receiver.EncryptionSessionManager().getOrCreate(peerId, sequenceTlsRoleServer, false)
			epochCtx, epochCancel := context.WithCancel(ctx)
			defer epochCancel()
			epoch := &tlsHandshakeEpoch{
				ctx: epochCtx, cancel: epochCancel, epochId: activeEpoch,
				handshakeDone: make(chan struct{}), establishmentDone: make(chan struct{}),
				serverFlightSent: true, transport: newSequenceTlsTransport(epochCtx),
			}
			session.stateLock.Lock()
			session.epoch = epoch
			session.stateLock.Unlock()
			payload := []byte{23, 3, 3, 0, 3, 0xab, 0xcd, 0xef}
			controlBytes, err := ProtoMarshal(&protocol.EncryptedControl{
				ControlType: controlType,
				SessionRole: protocol.SequenceRole_SequenceRoleClient,
				Payload:     payload, EpochId: wireEpoch,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer MessagePoolReturn(controlBytes)
			pack := &protocol.Pack{
				MessageId: NewId().Bytes(), SequenceId: NewId().Bytes(), SequenceNumber: 1,
				Frames: []*protocol.Frame{{
					MessageType:  protocol.MessageType_TransferEncryptedControl,
					MessageBytes: controlBytes,
				}},
			}
			frame := &protocol.TransferFrame{TransferPath: TransferPath{
				SourceId: peerId, DestinationId: receiver.ClientId(),
			}.ToProtobuf()}
			if version == 1 {
				packBytes, err := ProtoMarshal(pack)
				if err != nil {
					t.Fatal(err)
				}
				defer MessagePoolReturn(packBytes)
				frame.Frame = &protocol.Frame{MessageType: protocol.MessageType_TransferPack, MessageBytes: packBytes}
			} else {
				frame.Pack = pack
			}
			wire, err := ProtoMarshal(frame)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case toReceiver <- wire:
			case <-ctx.Done():
				MessagePoolReturn(wire)
				t.Fatal("wire route did not admit handshake")
			}
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("wire handshake did not reach the post-optimistic barrier")
			}
			epoch.transport.inboxLock.Lock()
			got := bytes.Clone(epoch.transport.inboxBuf)
			epoch.transport.inboxLock.Unlock()
			var want []byte
			if wantDelivery {
				want = payload
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("v%d optimistic handshake delivered %x, want %x (wire epoch %x, active %s)", version, got, want, wireEpoch, activeEpoch)
			}
			if session.currentEpoch() != epoch {
				t.Fatal("optimistic delivery changed epoch lifecycle")
			}
			session.stateLock.Lock()
			proofBytes, failed := len(epoch.pendingPeerIdentityProof), epoch.identityFailed
			session.stateLock.Unlock()
			if proofBytes != 0 || failed {
				t.Fatalf("optimistic control changed identity state: proof=%d failed=%t", proofBytes, failed)
			}
		}()
	}
}

// Delayed or future Finished records cannot authenticate against this epoch.
func TestOptimisticHandshakeRejectsForeignWireGenerations(t *testing.T) {
	active := Id{15: 2}
	for _, epoch := range []Id{{15: 1}, {15: 3}} {
		testOptimisticControlWireGeneration(t, protocol.EncryptedControlType_EncryptedControlHandshake, active, epoch.Bytes(), false)
	}
}

// A positive wire-path control proves that the exact active generation still
// reaches TLS without waiting behind the missing sequence head.
func TestOptimisticHandshakeAcceptsExactWireGeneration(t *testing.T) {
	active := Id{15: 2}
	testOptimisticControlWireGeneration(t, protocol.EncryptedControlType_EncryptedControlHandshake, active, active.Bytes(), true)
}

// Legacy remains useful only when both sides are unnamed; an untagged control
// cannot take the shortcut into a named epoch, though ordered delivery remains.
func TestOptimisticHandshakePreservesUnnamedLegacyBoundary(t *testing.T) {
	testOptimisticControlWireGeneration(t, protocol.EncryptedControlType_EncryptedControlHandshake, Id{}, nil, true)
	testOptimisticControlWireGeneration(t, protocol.EncryptedControlType_EncryptedControlHandshake, Id{15: 2}, nil, false)
}

// A malformed tag must not silently become the legacy zero generation in
// either the handshake or identity-proof shortcut.
func TestOptimisticHandshakeRejectsMalformedWireGeneration(t *testing.T) {
	for _, size := range []int{15, 17} {
		testOptimisticControlWireGeneration(t, protocol.EncryptedControlType_EncryptedControlHandshake, Id{15: 2}, bytes.Repeat([]byte{0x71}, size), false)
	}
}

// The identity-proof shortcut shares the parser, so invalid tags cannot occupy
// the current proof slot or produce a false terminal signature failure.
func TestOptimisticIdentityProofRejectsMalformedWireGeneration(t *testing.T) {
	for _, size := range []int{15, 17} {
		testOptimisticControlWireGeneration(t, protocol.EncryptedControlType_EncryptedControlIdentityProof, Id{15: 2}, bytes.Repeat([]byte{0x71}, size), false)
	}
}

// Ordered controls follow the same parse boundary. In particular, a malformed
// nack cannot masquerade as the empty generation used for lost-state recovery.
func TestMalformedEncryptedControlEpochCannotActAsLegacy(t *testing.T) {
	for _, controlType := range []protocol.EncryptedControlType{
		protocol.EncryptedControlType_EncryptedControlHandshake,
		protocol.EncryptedControlType_EncryptedControlIdentityProof,
		protocol.EncryptedControlType_EncryptedControlUnknownWrapNack,
	} {
		for _, size := range []int{15, 17} {
			func() {
				session, cleanup := newTestEncryptionSession(t, sequenceTlsRoleClient)
				defer cleanup()
				epoch := injectTestEpochWithId(session, false, nil, Id{15: 2})
				defer epoch.cancel()
				epoch.transport = newSequenceTlsTransport(epoch.ctx)
				if controlType == protocol.EncryptedControlType_EncryptedControlUnknownWrapNack {
					session.stateLock.Lock()
					session.establishedEpoch = epoch
					session.stateLock.Unlock()
				}
				session.DeliverEncryptedControl(&protocol.EncryptedControl{
					ControlType: controlType, Payload: []byte{23, 3, 3, 0, 3, 0xab, 0xcd, 0xef},
					EpochId: bytes.Repeat([]byte{0x71}, size),
				})
				if session.currentEpoch() != epoch {
					t.Fatalf("malformed %v epoch changed the active session", controlType)
				}
				epoch.transport.inboxLock.Lock()
				inboxBytes := len(epoch.transport.inboxBuf)
				epoch.transport.inboxLock.Unlock()
				session.stateLock.Lock()
				proofBytes, failed := len(epoch.pendingPeerIdentityProof), epoch.identityFailed
				session.stateLock.Unlock()
				if inboxBytes != 0 || proofBytes != 0 || failed {
					t.Fatalf("malformed %v epoch mutated TLS: inbox=%d proof=%d failed=%t", controlType, inboxBytes, proofBytes, failed)
				}
			}()
		}
	}
}
