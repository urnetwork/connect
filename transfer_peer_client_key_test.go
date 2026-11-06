package connect

// Peer.VerifiedClientPublicKey (SUBPROTOCOL.md §11): what a receive callback
// learns about the session that opened its frames. The pairs here complete a
// real handshake and identity proof over the in-process transports, so the key
// reaches a callback by the same path it would in production.

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
)

// What the accessor gives when it has no key.
var noPeerKey [ed25519.PublicKeySize]byte

func exposeVerifiedPeerClientKey(settings *ClientSettings) {
	settings.EncryptionSettings.ExposeVerifiedPeerClientKey = true
}

// One delivery as a listener saw it.
type peerKeyDelivery struct {
	listener string
	content  string
	key      [ed25519.PublicKeySize]byte
	verified bool
}

func newPeerKeyDelivery(listener string, content string, peer Peer) peerKeyDelivery {
	key, verified := peer.VerifiedClientPublicKey()
	return peerKeyDelivery{listener: listener, content: content, key: key, verified: verified}
}

const peerKeyTestSubprotocolId SubprotocolId = 4400

// Records what every kind of listener on a client sees.
func recordPeerKeys(t *testing.T, client *Client) <-chan peerKeyDelivery {
	t.Helper()
	deliveries := make(chan peerKeyDelivery, 64)
	record := func(delivery peerKeyDelivery) {
		select {
		case deliveries <- delivery:
		default:
			t.Error("delivery collector overflow")
		}
	}
	client.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, peer Peer) {
		for _, frame := range frames {
			if frame.MessageType != protocol.MessageType_TestSimpleMessage {
				continue
			}
			var message protocol.SimpleMessage
			if err := proto.Unmarshal(frame.MessageBytes, &message); err != nil {
				t.Error(err)
				continue
			}
			record(newPeerKeyDelivery("generic", message.Content, peer))
		}
	})
	if _, err := client.AddSubprotocolRawCallback(peerKeyTestSubprotocolId, func(_ TransferPath, _ SubprotocolId, messageBytes []byte, peer Peer) {
		record(newPeerKeyDelivery("raw", string(messageBytes), peer))
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := RegisterSubprotocol[*fixedMessage](client, peerKeyTestSubprotocolId, &fixedCodec{}, func(_ TransferPath, m *fixedMessage, peer Peer) {
		record(newPeerKeyDelivery("typed", string(m.bytes()), peer))
	}); err != nil {
		t.Fatal(err)
	}
	return deliveries
}

func requirePeerKeyDelivery(t *testing.T, deliveries <-chan peerKeyDelivery, listener string, content string) peerKeyDelivery {
	t.Helper()
	select {
	case delivery := <-deliveries:
		if delivery.listener != listener || delivery.content != content {
			t.Fatalf("delivered %+v, want %s %q", delivery, listener, content)
		}
		return delivery
	case <-time.After(60 * time.Second):
		t.Fatalf("%s %q was not delivered", listener, content)
		return peerKeyDelivery{}
	}
}

// Sends and waits for the transfer ack, the point at which the sender's
// contract is acknowledged and no-ack traffic may ride it.
func sendAndAwaitAck(t *testing.T, client *Client, destinationId Id, content string, opts ...any) {
	t.Helper()
	acked := make(chan error, 1)
	if !client.SendWithTimeout(requiredGateFrame(t, content), destinationId, func(err error) {
		acked <- err
	}, 60*time.Second, opts...) {
		t.Fatalf("send of %q was not admitted", content)
	}
	select {
	case err := <-acked:
		if err != nil {
			t.Fatalf("ack of %q = %v", content, err)
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("no ack of %q", content)
	}
}

// §6.4's identity check compares the binding against this key, so every delivery
// path must carry it: acked and no-ack Packs, the generic callback, and a
// subprotocol's raw listener and typed handler. Each direction's key is its
// sender's, from the receiver's own session.
func TestPeerVerifiedClientKeyReachesEveryDeliveryPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()
	a, b, aId, bId, _, _ := requiredGatePair(ctx, EncryptionModeRequired, EncryptionModeRequired, exposeVerifiedPeerClientKey, true)
	defer a.Cancel()
	defer b.Cancel()
	atB := recordPeerKeys(t, b)
	atA := recordPeerKeys(t, a)
	aKey := a.ClientKeyManager().PublicKey()
	bKey := b.ClientKeyManager().PublicKey()

	requireKey := func(delivery peerKeyDelivery, want ed25519.PublicKey) {
		t.Helper()
		if !delivery.verified || !bytes.Equal(delivery.key[:], want) {
			t.Fatalf("delivery %+v, want key %x", delivery, want)
		}
	}

	sendAndAwaitAck(t, a, bId, "acked")
	requireKey(requirePeerKeyDelivery(t, atB, "generic", "acked"), aKey)

	if !a.SendWithTimeout(requiredGateFrame(t, "no ack"), bId, func(error) {}, 60*time.Second, NoAck()) {
		t.Fatal("no-ack send was not admitted")
	}
	requireKey(requirePeerKeyDelivery(t, atB, "generic", "no ack"), aKey)

	message := &fixedMessage{}
	message.set([]byte("subprotocol"))
	codec := WithSubprotocolCodec[*fixedMessage](&fixedCodec{})
	if !SendSubprotocol[*fixedMessage](a, peerKeyTestSubprotocolId, message, bId, func(error) {}, codec) {
		t.Fatal("subprotocol send was not admitted")
	}
	requireKey(requirePeerKeyDelivery(t, atB, "raw", "subprotocol"), aKey)
	requireKey(requirePeerKeyDelivery(t, atB, "typed", "subprotocol"), aKey)

	sendAndAwaitAck(t, b, aId, "return")
	requireKey(requirePeerKeyDelivery(t, atA, "generic", "return"), bKey)
}

// Off by default: a sealed, identity-verified session delivers its frames as it
// always has, and the accessor reports nothing.
func TestPeerVerifiedClientKeyIsOffByDefault(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()
	a, b, _, bId, _, _ := requiredGatePair(ctx, EncryptionModeRequired, EncryptionModeRequired, nil, true)
	defer a.Cancel()
	defer b.Cancel()
	atB := recordPeerKeys(t, b)

	sendAndAwaitAck(t, a, bId, "sealed")
	if delivery := requirePeerKeyDelivery(t, atB, "generic", "sealed"); delivery.verified || delivery.key != noPeerKey {
		t.Fatalf("a client that did not opt in was given %+v", delivery)
	}
	if identities := b.encryptionSessionManager.PeerIdentities(); len(identities) == 0 {
		t.Fatal("the session is not identity-verified, so this proved nothing")
	}
}

// The key vouches for frames, not for a peer. A plaintext frame from a peer
// with a verified session was never opened by that session, so it carries no
// key even though the session is sealed.
func TestPeerVerifiedClientKeyIsNotForPlaintextFrames(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()
	a, b, aId, bId, _, _ := requiredGatePair(ctx, EncryptionModeOpportunistic, EncryptionModeOpportunistic, func(settings *ClientSettings) {
		exposeVerifiedPeerClientKey(settings)
		// so the first send waits for the seal and is not itself plaintext
		settings.EncryptionSettings.OpportunisticEstablishHold = 60 * time.Second
	}, true)
	defer a.Cancel()
	defer b.Cancel()
	atB := recordPeerKeys(t, b)

	sendAndAwaitAck(t, a, bId, "sealed")
	if delivery := requirePeerKeyDelivery(t, atB, "generic", "sealed"); !delivery.verified ||
		!bytes.Equal(delivery.key[:], a.ClientKeyManager().PublicKey()) {
		t.Fatalf("delivery %+v", delivery)
	}

	route := make(chan []byte)
	b.RouteManager().UpdateTransport(NewReceiveGatewayTransportWithType(TransportTypeH1), []Route{route})
	b.ContractManager().AddNoContractPeer(aId)
	// its own lane, so it neither supersedes the sealed sequence nor waits on it
	frame := sourceRuleSimpleFrame("plaintext")
	defer MessagePoolReturn(frame.MessageBytes)
	plaintext, err := ProtoMarshal(&protocol.TransferFrame{
		TransferPath: TransferPath{SourceId: aId, DestinationId: bId}.ToProtobuf(),
		Pack: &protocol.Pack{
			MessageId:   NewId().Bytes(),
			SequenceId:  NewId().Bytes(),
			Head:        true,
			Frames:      []*protocol.Frame{frame},
			ForceStream: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case route <- plaintext:
	case <-ctx.Done():
		t.Fatal("the client did not read its route")
	}
	if delivery := requirePeerKeyDelivery(t, atB, "generic", "plaintext"); delivery.verified || delivery.key != noPeerKey {
		t.Fatalf("a plaintext frame carried %+v", delivery)
	}
}

// Frames the platform sends have no session at all.
func TestPeerVerifiedClientKeyIsNotForTheControlId(t *testing.T) {
	fixture := newSourceRuleFixture(t, TransportTypeH1, exposeVerifiedPeerClientKey, func(settings *ClientSettings) {
		settings.EncryptionSettings.Mode = EncryptionModeRequired
	})
	fixture.inject(newSourceRuleSequence(ControlId), sourceRuleSimpleFrame("control"))
	delivery := fixture.requireDelivery("control")
	if key, verified := delivery.peer.VerifiedClientPublicKey(); verified || key != noPeerKey {
		t.Fatalf("a control frame carried %x", key)
	}
}

// Read-only: nothing a caller does to its value reaches the session's key or
// another caller's value. A per-frame check costs no allocation, so §22.5's
// receive allocation gate still holds for a listener that reads it.
func TestPeerVerifiedClientKeyIsReadOnly(t *testing.T) {
	key := [ed25519.PublicKeySize]byte{1, 2, 3}
	peer := Peer{verifiedClientKey: &key}
	first, ok := peer.VerifiedClientPublicKey()
	if !ok || first != key {
		t.Fatalf("key %x, %t", first, ok)
	}
	first[0] = 0xff
	second, ok := peer.VerifiedClientPublicKey()
	if !ok || second[0] != 1 || key[0] != 1 {
		t.Fatalf("a caller wrote through its value: %x, stored %x", second, key)
	}
	if key, ok := (Peer{}).VerifiedClientPublicKey(); ok || key != noPeerKey {
		t.Fatalf("the zero Peer has %x", key)
	}
	if allocs := testing.AllocsPerRun(100, func() {
		if _, ok := peer.VerifiedClientPublicKey(); !ok {
			t.Fatal("no key")
		}
	}); allocs != 0 {
		t.Fatalf("reading the key allocated %.0f times", allocs)
	}
}

// Verified means what `Cipher()` means. While the signed-identity gate holds or
// has refused the key, or before the proof, there is no verified key, even for
// a frame an established cipher could open.
func TestPeerVerifiedClientKeyFollowsTheSignedIdentityGate(t *testing.T) {
	sess, _, _, peerPriv, cleanup := newTestSessionForIdentityProof(t)
	defer cleanup()
	peerPub := peerPriv.Public().(ed25519.PublicKey)

	if sess.verifiedClientKey() != nil {
		t.Fatal("a key before any proof")
	}
	sess.SetPeerClientPublicKey(peerPub)
	if sess.verifiedClientKey() != nil {
		t.Fatal("the contract's key before its proof")
	}
	sess.receivePeerIdentityProof(ed25519.Sign(peerPriv, sess.epoch.tlsExporter))
	key := sess.verifiedClientKey()
	if key == nil || !bytes.Equal(key[:], peerPub) {
		t.Fatalf("the proved key is %x", key)
	}
	if sess.verifiedClientKey() != key {
		t.Fatal("one session allocated its key twice, so every frame would split the batch")
	}

	for _, c := range []struct {
		state    clientKeyHistoryState
		verified bool
	}{
		{clientKeyHistoryNotRequired, true},
		{clientKeyHistoryPending, false},
		{clientKeyHistoryVerified, true},
		{clientKeyHistoryRejected, false},
		{clientKeyHistoryStoreUnavailable, false},
	} {
		func() {
			sess.stateLock.Lock()
			defer sess.stateLock.Unlock()
			sess.keyHistoryState = c.state
		}()
		if verified := sess.verifiedClientKey() != nil; verified != c.verified {
			t.Errorf("%s: verified=%t", c.state, verified)
		}
	}
}

func testPeerKeyCipher(t *testing.T) *sequenceCipher {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	return &sequenceCipher{aead: aead}
}

// The key comes from the session that opened the frame, found by trial here
// because the sender gave no hint, and never from another session of the same
// peer. Two sessions of one peer can hold different keys: a contract's key is
// first-write-wins per session.
func TestUnwrapFrameTakesTheKeyOfTheSessionThatOpenedIt(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	settings := closeWaitClientSettings()
	settings.EncryptionSettings.Mode = EncryptionModeOpportunistic
	exposeVerifiedPeerClientKey(settings)
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	defer client.Cancel()

	peerId := NewId()
	establish := func(companion bool) (*sequenceCipher, ed25519.PublicKey) {
		session := client.encryptionSessionManager.Acquire(peerId, sequenceTlsRoleServer, companion)
		publicKey, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		sessionCipher := testPeerKeyCipher(t)
		func() {
			session.stateLock.Lock()
			defer session.stateLock.Unlock()
			session.peerClientPublicKey = publicKey
			session.establishedEpoch = &tlsHandshakeEpoch{
				derivedTlsCipher:     sessionCipher,
				peerIdentityVerified: true,
				cancel:               func() {},
			}
		}()
		return sessionCipher, publicKey
	}
	regularCipher, regularKey := establish(false)
	companionCipher, companionKey := establish(true)

	for _, c := range []struct {
		sessionCipher *sequenceCipher
		key           ed25519.PublicKey
		companion     bool
	}{
		{regularCipher, regularKey, false},
		{companionCipher, companionKey, true},
	} {
		wrapped, err := c.sessionCipher.Seal([]byte("frame"))
		if err != nil {
			t.Fatal(err)
		}
		plaintext, _, companion, verifiedKey, err := client.unwrapFrame(peerId, protocol.SequenceRole_SequenceRoleUnknown, nil, wrapped)
		if err != nil {
			t.Fatal(err)
		}
		defer MessagePoolReturn(plaintext)
		if companion != c.companion || verifiedKey == nil || !bytes.Equal(verifiedKey[:], c.key) {
			t.Fatalf("companion=%t key=%x, want %t %x", companion, verifiedKey, c.companion, c.key)
		}
	}
}

// One callback carries one Peer, so a sealed frame never rides a callback with
// a plaintext one. A client that did not opt in has no keys, so its batches form
// as they always did: one callback here.
func TestReceiveBatchBoundaryFollowsTheVerifiedKey(t *testing.T) {
	key := &[ed25519.PublicKeySize]byte{1}
	otherKey := &[ed25519.PublicKeySize]byte{2}
	for _, c := range []struct {
		itemKeys []*[ed25519.PublicKeySize]byte
		// frames per callback, and whether that callback's Peer has a key
		callbacks []int
		verified  []bool
	}{
		{
			itemKeys:  []*[ed25519.PublicKeySize]byte{nil, nil, nil},
			callbacks: []int{3},
			verified:  []bool{false},
		},
		{
			itemKeys:  []*[ed25519.PublicKeySize]byte{key, key},
			callbacks: []int{2},
			verified:  []bool{true},
		},
		{
			itemKeys:  []*[ed25519.PublicKeySize]byte{nil, key, key, nil},
			callbacks: []int{1, 2, 1},
			verified:  []bool{false, true, false},
		},
		{
			itemKeys:  []*[ed25519.PublicKeySize]byte{key, otherKey},
			callbacks: []int{1, 1},
			verified:  []bool{true, true},
		},
	} {
		func() {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			client := NewClient(ctx, NewId(), NewNoContractClientOob(), closeWaitClientSettings())
			defer client.Cancel()
			source := SourceId(NewId())
			sequence := NewReceiveSequence(ctx, client, source, NewId(), sequenceTlsRoleServer, false, DefaultReceiveBufferSettings())
			sequence.peerAudit = NewSequencePeerAudit(client, source, 0)

			var callbacks []int
			var verified []bool
			callback := func(_ TransferPath, frames []*protocol.Frame, peer Peer) {
				callbacks = append(callbacks, len(frames))
				_, ok := peer.VerifiedClientPublicKey()
				verified = append(verified, ok)
			}
			for _, itemKey := range c.itemKeys {
				sequence.receiveHead(&receiveItem{
					frames:            []*protocol.Frame{{MessageType: protocol.MessageType_TestSimpleMessage}},
					receiveCallback:   callback,
					verifiedClientKey: itemKey,
				})
			}
			sequence.flushDeliver()
			if len(callbacks) != len(c.callbacks) {
				t.Fatalf("callbacks %v, want %v", callbacks, c.callbacks)
			}
			for i := range callbacks {
				if callbacks[i] != c.callbacks[i] || verified[i] != c.verified[i] {
					t.Fatalf("callbacks %v verified %v, want %v %v", callbacks, verified, c.callbacks, c.verified)
				}
			}
		}()
	}
}
