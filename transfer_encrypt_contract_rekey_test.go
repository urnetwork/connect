package connect

// Contract-only carriers must stay readable while a peer replaces a lost
// encryption session; application data keeps the established cipher throughout.

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Forces a responder to retain an old cipher before its replacement handshake
// opens the reply contract, then checks the actual wire head and data delivery.
func testContractOpenDuringRekey(t *testing.T, companion bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	block, err := aes.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	retainedCipher := &sequenceCipher{aead: aead}
	retainedCtx, retainedCancel := context.WithCancel(ctx)
	defer retainedCancel()
	retainedEpoch := &tlsHandshakeEpoch{
		ctx: retainedCtx, cancel: retainedCancel, epochId: NewId(),
		derivedTlsCipher: retainedCipher, peerIdentityVerified: true,
	}

	var responder *Client
	var injectionOnce sync.Once
	var headOnce sync.Once
	var dataOnce sync.Once
	injectionErrors := make(chan error, 1)
	headWrapped := make(chan bool, 1)
	dataWrapped := make(chan bool, 1)
	settingsIndex := 0
	initiator, receiver, _, receiverId, _, receives := requiredGatePair(
		ctx, EncryptionModeRequired, EncryptionModeRequired,
		func(settings *ClientSettings) {
			settings.EncryptionSettings.EncryptionControlUseCompanion = companion
			isResponder := settingsIndex == 1
			settingsIndex++
			if isResponder {
				settings.SendBufferSettings.beforeTakeContractForTest = func(key sendSequenceId) {
					if key.EncryptionRole != sequenceTlsRoleServer {
						return
					}
					injectionOnce.Do(func() {
						manager := responder.encryptionSessionManager
						manager.stateLock.Lock()
						session := manager.sessions[sessionKey{
							peerId: key.Destination, role: key.EncryptionRole,
							companion: key.EncryptionCompanion,
						}]
						manager.stateLock.Unlock()
						if session == nil {
							injectionErrors <- fmt.Errorf("reply contract has no encryption session")
							return
						}
						session.stateLock.Lock()
						defer session.stateLock.Unlock()
						if !session.handshakeInFlightLocked() || session.establishedEpoch != nil {
							injectionErrors <- fmt.Errorf("reply contract did not precede handshake establishment")
							return
						}
						session.establishedEpoch = retainedEpoch
					})
				}
			}
			settings.SendBufferSettings.TransferWireMessageObserver = func(observation TransferWireMessageObservation) {
				var inner, outer protocol.TransferFrame
				if err := ProtoUnmarshal(observation.TransferFrameBytes, &inner); err != nil {
					return
				}
				if err := ProtoUnmarshal(observation.WireMessageBytes, &outer); err != nil {
					return
				}
				pack := inner.GetPack()
				if pack == nil {
					return
				}
				wrapped := len(outer.EncryptedTransferFrame) != 0
				if isResponder && pack.Head && pack.SequenceNumber == 0 &&
					pack.ContractFrame != nil && len(pack.Frames) == 0 {
					headOnce.Do(func() { headWrapped <- wrapped })
				}
				if !isResponder {
					for _, frame := range pack.Frames {
						message, err := FromFrame(frame)
						if _, ok := message.(*protocol.SimpleMessage); err == nil && ok {
							dataOnce.Do(func() { dataWrapped <- wrapped })
						}
					}
				}
			}
		}, true,
	)
	responder = receiver
	defer func() {
		cancel()
		initiator.CloseAndWait(context.Background())
		responder.CloseAndWait(context.Background())
	}()
	frame := requiredGateFrame(t, "replacement-handshake")
	sent := make(chan bool, 1)
	go func() {
		ok := initiator.SendWithTimeout(frame, receiverId, func(error) {}, -1)
		if !ok {
			MessagePoolReturn(frame.MessageBytes)
		}
		sent <- ok
	}()
	select {
	case err := <-injectionErrors:
		t.Fatal(err)
	case wrapped := <-headWrapped:
		if wrapped {
			t.Fatal("replacement handshake contract head was encrypted with the retained cipher")
		}
	case <-ctx.Done():
		t.Fatal("replacement handshake did not write its contract head")
	}
	select {
	case ok := <-sent:
		if !ok {
			t.Fatal("replacement handshake refused application data")
		}
	case <-ctx.Done():
		t.Fatal("replacement handshake did not admit application data")
	}
	select {
	case wrapped := <-dataWrapped:
		if !wrapped {
			t.Fatal("replacement handshake exposed application data")
		}
	case <-ctx.Done():
		t.Fatal("replacement handshake did not write application data")
	}
	select {
	case got := <-receives:
		if got != "replacement-handshake" {
			t.Fatalf("application delivery = %q", got)
		}
	case <-ctx.Done():
		t.Fatal("replacement handshake did not deliver application data")
	}
}

// Symmetric contracts share the production handshake carrier with data.
func TestContractOpenDuringRekeySymmetric(t *testing.T) {
	testContractOpenDuringRekey(t, false)
}

// Companion replies must preserve the same bootstrap ordering guarantee.
func TestContractOpenDuringRekeyCompanion(t *testing.T) {
	testContractOpenDuringRekey(t, true)
}

// Drives a sequence synchronously so rekey can start between exact writes.
func newContractRekeyWriteFixture(t *testing.T) (*SendSequence, *peerEncryptionSession, chan []byte) {
	t.Helper()
	client, sequence, destinationId, contract := newSendNoContractHarness(t, t.Context())
	sequence.sendBuffer = client.sendBuffer
	contract.contract = &protocol.Contract{StoredContractBytes: NewId().Bytes()}
	block, err := aes.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	epoch := &tlsHandshakeEpoch{derivedTlsCipher: &sequenceCipher{aead: aead}}
	session := &peerEncryptionSession{
		client: client, settings: &EncryptionSettings{Mode: EncryptionModeRequired},
		epoch: epoch, establishedEpoch: epoch,
	}
	sequence.session = session
	route := make(chan []byte, 1)
	client.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(destinationId)), []Route{route})
	t.Cleanup(func() {
		sequence.releaseRetainedSendItems(context.Canceled)
		sequence.closeContractMultiRouteWriter()
	})
	return sequence, session, route
}

// Checks bytes already committed to a buffered route by a synchronous writer.
func requireContractRekeyWireWrapped(t *testing.T, route chan []byte, want bool) {
	t.Helper()
	select {
	case message := <-route:
		defer MessagePoolReturn(message)
		var outer protocol.TransferFrame
		if err := ProtoUnmarshal(message, &outer); err != nil {
			t.Fatal(err)
		}
		if got := len(outer.EncryptedTransferFrame) != 0; got != want {
			t.Fatalf("contract rekey wire wrapped = %t, want %t", got, want)
		}
	default:
		t.Fatal("synchronous write did not publish its wire message")
	}
}

// A queued opening or ahead announcement can become the handshake's gap when
// rekey starts later. Its first recovery write must pin all following resends.
func TestContractControlResendStartsRekeyAfterQueue(t *testing.T) {
	for _, ahead := range []bool{false, true} {
		sequence, session, route := newContractRekeyWriteFixture(t)
		if ahead {
			next := newContractAheadTestContract(t, sequence.client, sequence.destination)
			sequence.sendContractAheadAnnouncement(next, nil)
		} else {
			sequence.sendWithSetContract(nil, nil, true, true, false)
		}
		requireContractRekeyWireWrapped(t, route, true)
		item := sequence.resendQueue.PeekFirst()
		if item == nil || item.forceUnwrapped {
			t.Fatalf("established contract control has no wrapped retained item: ahead=%t", ahead)
		}
		session.stateLock.Lock()
		session.epoch = &tlsHandshakeEpoch{
			establishmentDone: make(chan struct{}),
			derivedTlsCipher:  session.establishedEpoch.derivedTlsCipher,
		}
		session.stateLock.Unlock()
		path := sendTransferPath(sequence.client.ClientId(), DestinationId(sequence.destination))
		if _, err := sequence.writeMaybeWrappedBytes(item.transferFrameBytes, path, item.forceUnwrapped, item, true, false); err != nil {
			t.Fatal(err)
		}
		requireContractRekeyWireWrapped(t, route, false)
		if !item.forceUnwrapped {
			t.Fatal("rekey did not retain the contract bootstrap pin")
		}
		session.stateLock.Lock()
		session.establishedEpoch = session.epoch
		session.stateLock.Unlock()
		if _, err := sequence.writeMaybeWrappedBytes(item.transferFrameBytes, path, item.forceUnwrapped, item, true, false); err != nil {
			t.Fatal(err)
		}
		requireContractRekeyWireWrapped(t, route, false)
	}
}

// Contract metadata on an application head must not grant the control-only
// exemption, including a legal frame whose serialized application body is empty.
func TestContractDataHeadRetainsCipherDuringRekey(t *testing.T) {
	sequence, session, route := newContractRekeyWriteFixture(t)
	session.stateLock.Lock()
	session.epoch = &tlsHandshakeEpoch{establishmentDone: make(chan struct{})}
	session.stateLock.Unlock()
	frame := requiredGateFrame(t, "")
	sequence.sendWithSetContract([]*protocol.Frame{frame}, nil, true, true, false)
	requireContractRekeyWireWrapped(t, route, true)
	item := sequence.resendQueue.PeekFirst()
	if item == nil || item.forceUnwrapped {
		t.Fatal("application head received the contract-only bootstrap pin")
	}
	path := sendTransferPath(sequence.client.ClientId(), DestinationId(sequence.destination))
	if _, err := sequence.writeMaybeWrappedBytes(item.transferFrameBytes, path, item.forceUnwrapped, item, true, false); err != nil {
		t.Fatal(err)
	}
	requireContractRekeyWireWrapped(t, route, true)
}
