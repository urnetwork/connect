package connect

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"errors"
	"testing"
	"time"
)

// testPeerClientKeyPinStore is an in-memory ratchet store.
type testPeerClientKeyPinStore struct {
	pins        map[Id]ClientKeyPin
	signedSeen  bool
	setPinCalls int
}

func newTestPeerClientKeyPinStore() *testPeerClientKeyPinStore {
	return &testPeerClientKeyPinStore{pins: map[Id]ClientKeyPin{}}
}

func (self *testPeerClientKeyPinStore) GetPeerClientKeyPin(peerId Id) (ClientKeyPin, bool) {
	pin, ok := self.pins[peerId]
	return pin, ok
}

func (self *testPeerClientKeyPinStore) SetPeerClientKeyPin(peerId Id, pin ClientKeyPin) {
	self.pins[peerId] = pin
	self.setPinCalls += 1
}

func (self *testPeerClientKeyPinStore) SignedHistorySeen() bool { return self.signedSeen }

func (self *testPeerClientKeyPinStore) SetSignedHistorySeen() { self.signedSeen = true }

// newTestKeyHistorySession builds a Required-mode session whose peer id is the
// golden vector's client id, so the golden history applies to it.
func newTestKeyHistorySession(
	t *testing.T,
	store PeerClientKeyPinStore,
	history func(ctx context.Context) ([][]byte, error),
) (*peerEncryptionSession, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	settings := DefaultClientSettings()
	settings.EncryptionSettings.Mode = EncryptionModeRequired
	settings.EncryptionSettings.TlsTimeout = 2 * time.Second
	settings.EncryptionSettings.PeerClientKeyPinStore = store
	settings.EncryptionSettings.MaxClientKeyHistoryGenerations = 8
	settings.EncryptionSettings.NewPeerClientKeyHistoryFetcher = func(peerId Id) func(context.Context) ([][]byte, error) {
		return history
	}
	g1, err := decodeClientKeyRegistration([]byte(goldenClientKeyG1))
	if err != nil {
		cancel()
		t.Fatalf("decode golden: %s", err)
	}
	digest, err := g1.Domain.Digest()
	if err != nil {
		cancel()
		t.Fatalf("golden domain digest: %s", err)
	}
	settings.EncryptionSettings.TrustedClientKeySigners = []ClientKeyTrustedSigner{
		ClientKeyTrustedSigner{DomainDigest: digest, Signer: g1.Signer},
	}

	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	keyManager, err := NewClientKeyManager(ctx, client)
	if err != nil {
		cancel()
		t.Fatalf("NewClientKeyManager: %s", err)
	}
	manager := NewEncryptionSessionManager(ctx, client, keyManager, settings.EncryptionSettings)
	var roleTlsConfig *tls.Config = manager.ClientTlsConfig()
	sess := newPeerEncryptionSession(
		ctx,
		manager,
		client,
		goldenClientKeyId(),
		sequenceTlsRoleClient,
		settings.EncryptionSettings,
		roleTlsConfig,
		false,
	)
	return sess, func() {
		client.Cancel()
		cancel()
	}
}

// goldenHeadPublicKey is the identity key the golden chain's head attests.
func goldenHeadPublicKey(t *testing.T) ed25519.PublicKey {
	t.Helper()
	g2, err := decodeClientKeyRegistration([]byte(goldenClientKeyG2))
	if err != nil {
		t.Fatalf("decode golden g2: %s", err)
	}
	return g2.ClientKeyRegistrationPublicKey()
}

func armKeyHistoryGate(sess *peerEncryptionSession) {
	sess.stateLock.Lock()
	sess.keyHistoryState = clientKeyHistoryPending
	sess.stateLock.Unlock()
}

func keyHistoryState(sess *peerEncryptionSession) clientKeyHistoryState {
	sess.stateLock.Lock()
	defer sess.stateLock.Unlock()
	return sess.keyHistoryState
}

func TestSignedIdentityVerifiedOpensGate(t *testing.T) {
	store := newTestPeerClientKeyPinStore()
	sess, done := newTestKeyHistorySession(t, store, nil)
	defer done()

	armKeyHistoryGate(sess)
	sess.applyPeerClientKeyHistory(goldenHeadPublicKey(t), goldenClientKeyHistory())

	if got := keyHistoryState(sess); got != clientKeyHistoryVerified {
		t.Fatalf("state = %s, want verified", got)
	}
	if store.setPinCalls != 1 {
		t.Fatalf("pin writes = %d, want 1", store.setPinCalls)
	}
	if !store.signedSeen {
		t.Fatal("signed-history latch not set")
	}
	pin, ok := store.GetPeerClientKeyPin(goldenClientKeyId())
	if !ok || pin.Generation != 2 {
		t.Fatalf("pinned generation = %d (present %t), want 2", pin.Generation, ok)
	}
}

// The substitution case the mechanism exists for: the platform hands one key
// in the contract and has signed a different one.
func TestSignedIdentitySubstitutionRejected(t *testing.T) {
	store := newTestPeerClientKeyPinStore()
	sess, done := newTestKeyHistorySession(t, store, nil)
	defer done()

	substituted := make(ed25519.PublicKey, ed25519.PublicKeySize)
	copy(substituted, goldenHeadPublicKey(t))
	substituted[0] ^= 0x01

	armKeyHistoryGate(sess)
	sess.applyPeerClientKeyHistory(substituted, goldenClientKeyHistory())

	if got := keyHistoryState(sess); got != clientKeyHistoryRejected {
		t.Fatalf("state = %s, want rejected", got)
	}
	if !sess.KeyIdentityRejected() {
		t.Fatal("KeyIdentityRejected() is false after a substitution")
	}
	if sess.Cipher() != nil {
		t.Fatal("cipher exposed for a rejected peer")
	}
	if store.setPinCalls != 0 {
		t.Fatal("a rejected resolution must not write a pin")
	}
}

// An unreachable evidence source is an availability failure, never evidence of
// substitution. Treating it as a rejection would exclude every provider at
// once the moment the API blipped.
func TestSignedIdentityFetchErrorFallsBack(t *testing.T) {
	store := newTestPeerClientKeyPinStore()
	sess, done := newTestKeyHistorySession(t, store, func(ctx context.Context) ([][]byte, error) {
		return nil, errors.New("connection refused")
	})
	defer done()

	armKeyHistoryGate(sess)
	sess.resolvePeerClientKeyHistory(goldenHeadPublicKey(t))

	deadline := time.Now().Add(5 * time.Second)
	for keyHistoryState(sess) == clientKeyHistoryPending && time.Now().Before(deadline) {
		select {
		case <-time.After(10 * time.Millisecond):
		}
	}
	if got := keyHistoryState(sess); got != clientKeyHistoryVerified {
		t.Fatalf("state = %s, want verified (fallback)", got)
	}
	if store.setPinCalls != 0 {
		t.Fatal("a fallback must not write a pin")
	}
}

// Tier P: a legacy peer, or a platform that does not run the signed path.
func TestSignedIdentityAbsentAcceptedWhenNeverSeen(t *testing.T) {
	store := newTestPeerClientKeyPinStore()
	sess, done := newTestKeyHistorySession(t, store, nil)
	defer done()

	armKeyHistoryGate(sess)
	sess.applyPeerClientKeyHistory(goldenHeadPublicKey(t), nil)

	if got := keyHistoryState(sess); got != clientKeyHistoryVerified {
		t.Fatalf("state = %s, want verified", got)
	}
}

// The ratchet: a peer that has produced signed evidence before may not stop.
func TestSignedIdentityDowngradeRejectedPerPeer(t *testing.T) {
	store := newTestPeerClientKeyPinStore()
	sess, done := newTestKeyHistorySession(t, store, nil)
	defer done()

	armKeyHistoryGate(sess)
	sess.applyPeerClientKeyHistory(goldenHeadPublicKey(t), goldenClientKeyHistory())
	if got := keyHistoryState(sess); got != clientKeyHistoryVerified {
		t.Fatalf("setup state = %s, want verified", got)
	}

	// a fresh session for the same peer, now offered nothing
	next, doneNext := newTestKeyHistorySession(t, store, nil)
	defer doneNext()
	armKeyHistoryGate(next)
	next.applyPeerClientKeyHistory(goldenHeadPublicKey(t), nil)

	if got := keyHistoryState(next); got != clientKeyHistoryRejected {
		t.Fatalf("state = %s, want rejected (per-peer downgrade)", got)
	}
}

// The ratchet, blanket form: once a platform has served signed evidence for
// anyone, a peer with none is a downgrade rather than a legacy peer.
func TestSignedIdentityDowngradeRejectedPerOperator(t *testing.T) {
	store := newTestPeerClientKeyPinStore()
	store.SetSignedHistorySeen()
	sess, done := newTestKeyHistorySession(t, store, nil)
	defer done()

	armKeyHistoryGate(sess)
	sess.applyPeerClientKeyHistory(goldenHeadPublicKey(t), nil)

	if got := keyHistoryState(sess); got != clientKeyHistoryRejected {
		t.Fatalf("state = %s, want rejected (per-operator downgrade)", got)
	}
}

func TestSignedIdentityInvalidChainRejected(t *testing.T) {
	store := newTestPeerClientKeyPinStore()
	sess, done := newTestKeyHistorySession(t, store, nil)
	defer done()

	armKeyHistoryGate(sess)
	sess.applyPeerClientKeyHistory(goldenHeadPublicKey(t), [][]byte{[]byte("{}")})

	if got := keyHistoryState(sess); got != clientKeyHistoryRejected {
		t.Fatalf("state = %s, want rejected", got)
	}
}

// A rejection is terminal: a later resolution must not reopen the gate.
func TestSignedIdentityRejectionIsTerminal(t *testing.T) {
	store := newTestPeerClientKeyPinStore()
	sess, done := newTestKeyHistorySession(t, store, nil)
	defer done()

	armKeyHistoryGate(sess)
	sess.applyPeerClientKeyHistory(goldenHeadPublicKey(t), [][]byte{[]byte("{}")})
	if got := keyHistoryState(sess); got != clientKeyHistoryRejected {
		t.Fatalf("setup state = %s, want rejected", got)
	}

	sess.applyPeerClientKeyHistory(goldenHeadPublicKey(t), goldenClientKeyHistory())
	if got := keyHistoryState(sess); got != clientKeyHistoryRejected {
		t.Fatalf("state = %s after a valid retry, want rejected (terminal)", got)
	}
	if sess.Cipher() != nil {
		t.Fatal("cipher exposed after a terminal rejection")
	}
}

// Enforcement is scoped to Required: under Opportunistic a refusal would
// degrade to plaintext, which is what a substituting operator wanted.
func TestSignedIdentityNotGatedUnderOpportunistic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultClientSettings()
	settings.EncryptionSettings.Mode = EncryptionModeOpportunistic
	settings.EncryptionSettings.NewPeerClientKeyHistoryFetcher = func(peerId Id) func(context.Context) ([][]byte, error) {
		return func(ctx context.Context) ([][]byte, error) { return nil, nil }
	}
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	defer client.Cancel()
	keyManager, err := NewClientKeyManager(ctx, client)
	if err != nil {
		t.Fatalf("NewClientKeyManager: %s", err)
	}
	manager := NewEncryptionSessionManager(ctx, client, keyManager, settings.EncryptionSettings)
	sess := newPeerEncryptionSession(
		ctx, manager, client, NewId(), sequenceTlsRoleClient,
		settings.EncryptionSettings, manager.ClientTlsConfig(), false,
	)

	sess.stateLock.Lock()
	required := sess.keyHistoryRequiredWithLock()
	sess.stateLock.Unlock()
	if required {
		t.Fatal("signed-identity gate armed under Opportunistic")
	}
}
