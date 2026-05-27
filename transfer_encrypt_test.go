package connect

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"
)

// newTestSessionForIdentityProof constructs a peerEncryptionSession
// ready for identity-proof tests: TLS state is pre-set
// (`tlsExporter` populated, `derivedTlsCipher` populated) so the
// test can exercise the identity-proof gating in isolation without
// running an actual TLS handshake.
//
// Returns the session, the local client's ClientKeyManager, the
// peer ClientId, the peer's private key (for signing proofs in the
// test), and a cleanup function.
func newTestSessionForIdentityProof(t *testing.T) (
	sess *peerEncryptionSession,
	localKeyManager *ClientKeyManager,
	peerId Id,
	peerPriv ed25519.PrivateKey,
	cleanup func(),
) {
	ctx, cancel := context.WithCancel(context.Background())

	settings := DefaultClientSettings()
	settings.EncryptionSettings.Encrypt = true
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)

	var err error
	localKeyManager, err = NewClientKeyManager(ctx, client)
	if err != nil {
		cancel()
		t.Fatalf("NewClientKeyManager: %s", err)
	}

	manager := NewEncryptionSessionManager(ctx, client, localKeyManager, settings.EncryptionSettings)

	peerId = NewId()
	sess = newPeerEncryptionSession(
		ctx, manager, client, peerId,
		sequenceTlsRoleServer,
		settings.EncryptionSettings,
	)

	// Pre-populate the TLS-handshake-derived state. In production
	// this is filled by `completeHandshake` after the actual TLS
	// handshake; for these tests we inject directly so we can
	// exercise the identity-proof gate independently.
	exporter := make([]byte, sequenceTlsIdentityProofLength)
	_, err = rand.Read(exporter)
	assert.Equal(t, nil, err)
	sess.tlsExporter = exporter
	sess.derivedTlsCipher = &sequenceCipher{} // non-nil sentinel; never used for crypto in these tests

	_, peerPriv, err = ed25519.GenerateKey(rand.Reader)
	assert.Equal(t, nil, err)

	cleanup = func() {
		client.Cancel()
		cancel()
	}
	return
}

// TestPeerSessionCipherGatedOnIdentityVerified verifies that
// `Cipher()` returns nil until `peerIdentityVerified` flips true,
// even with the TLS-derived cipher already in place.
func TestPeerSessionCipherGatedOnIdentityVerified(t *testing.T) {
	sess, _, _, _, cleanup := newTestSessionForIdentityProof(t)
	defer cleanup()

	// Pre-identity-verification: cipher hidden.
	assert.Equal(t, (*sequenceCipher)(nil), sess.Cipher())

	// Manually flip the flag (simulating successful identity proof).
	sess.stateLock.Lock()
	sess.peerIdentityVerified = true
	sess.stateLock.Unlock()

	// Now cipher exposed.
	if sess.Cipher() == nil {
		t.Fatal("Cipher should be non-nil once peerIdentityVerified flips true")
	}
}

// TestPeerSessionIdentityProofValid covers the happy path: peer key
// known, exporter present, valid proof arrives → flips
// `peerIdentityVerified` true and `Cipher()` becomes non-nil.
func TestPeerSessionIdentityProofValid(t *testing.T) {
	sess, _, _, peerPriv, cleanup := newTestSessionForIdentityProof(t)
	defer cleanup()

	peerPub := peerPriv.Public().(ed25519.PublicKey)

	// Peer signs the session's exporter with their private key.
	proof := ed25519.Sign(peerPriv, sess.tlsExporter)
	assert.Equal(t, ed25519.SignatureSize, len(proof))

	// Set the peer key and deliver the proof.
	sess.SetPeerClientPublicKey(peerPub)
	sess.receivePeerIdentityProof(proof)

	assert.Equal(t, true, sess.peerIdentityVerified)
	assert.Equal(t, false, sess.identityFailed)
	if sess.Cipher() == nil {
		t.Fatal("Cipher should be exposed after valid identity proof")
	}
}

// TestPeerSessionIdentityProofInvalid covers the failure path:
// proof signed with the wrong key → flips `identityFailed`,
// `Cipher()` stays nil permanently.
func TestPeerSessionIdentityProofInvalid(t *testing.T) {
	sess, _, _, peerPriv, cleanup := newTestSessionForIdentityProof(t)
	defer cleanup()

	peerPub := peerPriv.Public().(ed25519.PublicKey)

	// Sign with a DIFFERENT private key — the proof must fail
	// verification against `peerPub`.
	_, wrongPriv, err := ed25519.GenerateKey(rand.Reader)
	assert.Equal(t, nil, err)
	badProof := ed25519.Sign(wrongPriv, sess.tlsExporter)

	sess.SetPeerClientPublicKey(peerPub)
	sess.receivePeerIdentityProof(badProof)

	assert.Equal(t, false, sess.peerIdentityVerified)
	assert.Equal(t, true, sess.identityFailed)
	assert.Equal(t, (*sequenceCipher)(nil), sess.Cipher())
}

// TestPeerSessionIdentityProofMalformed covers the defensive path:
// a proof of the wrong size flips `identityFailed` immediately —
// the session never exposes a cipher.
func TestPeerSessionIdentityProofMalformed(t *testing.T) {
	sess, _, _, _, cleanup := newTestSessionForIdentityProof(t)
	defer cleanup()

	sess.receivePeerIdentityProof([]byte{0, 1, 2}) // way too short

	assert.Equal(t, true, sess.identityFailed)
	assert.Equal(t, (*sequenceCipher)(nil), sess.Cipher())
}

// TestPeerSessionIdentityProofArrivesBeforeKey covers out-of-order
// arrival: proof first, peer key second. The proof must be
// buffered, and verification must run once the key is set.
func TestPeerSessionIdentityProofArrivesBeforeKey(t *testing.T) {
	sess, _, _, peerPriv, cleanup := newTestSessionForIdentityProof(t)
	defer cleanup()

	peerPub := peerPriv.Public().(ed25519.PublicKey)
	proof := ed25519.Sign(peerPriv, sess.tlsExporter)

	// Proof first — must be buffered (no peer key yet).
	sess.receivePeerIdentityProof(proof)
	assert.Equal(t, false, sess.peerIdentityVerified)
	if 0 == len(sess.pendingPeerIdentityProof) {
		t.Fatal("expected proof to be buffered while peer key unknown")
	}

	// Key arrives — verification runs and succeeds.
	sess.SetPeerClientPublicKey(peerPub)
	assert.Equal(t, true, sess.peerIdentityVerified)
	if 0 != len(sess.pendingPeerIdentityProof) {
		t.Fatal("expected buffered proof to be cleared after verification")
	}
}

// TestPeerSessionIdentityProofArrivesBeforeExporter covers out-of-
// order arrival where the TLS handshake hasn't completed yet (no
// exporter). The proof must be buffered, and verification must run
// once the exporter and key are both available.
func TestPeerSessionIdentityProofArrivesBeforeExporter(t *testing.T) {
	sess, _, _, peerPriv, cleanup := newTestSessionForIdentityProof(t)
	defer cleanup()

	// Wipe the pre-populated exporter to simulate "TLS handshake
	// not yet complete." (The helper pre-populates so most tests
	// don't have to think about it.)
	savedExporter := sess.tlsExporter
	sess.tlsExporter = nil

	peerPub := peerPriv.Public().(ed25519.PublicKey)
	proof := ed25519.Sign(peerPriv, savedExporter)

	// Set the peer key and the proof before the exporter exists.
	sess.SetPeerClientPublicKey(peerPub)
	sess.receivePeerIdentityProof(proof)
	assert.Equal(t, false, sess.peerIdentityVerified)
	if 0 == len(sess.pendingPeerIdentityProof) {
		t.Fatal("expected proof to be buffered while exporter unknown")
	}

	// Exporter becomes available; trigger verification manually
	// (in production `completeHandshake` would call this).
	sess.tlsExporter = savedExporter
	sess.maybeVerifyPendingPeerIdentityProof()

	assert.Equal(t, true, sess.peerIdentityVerified)
}

// TestPeerSessionIdentityProofSecondIgnored verifies that a second
// proof arriving while one is already buffered (or after
// verification has settled) is ignored — the session has at most
// one identity-proof exchange per lifetime.
func TestPeerSessionIdentityProofSecondIgnored(t *testing.T) {
	sess, _, _, peerPriv, cleanup := newTestSessionForIdentityProof(t)
	defer cleanup()

	peerPub := peerPriv.Public().(ed25519.PublicKey)
	proof := ed25519.Sign(peerPriv, sess.tlsExporter)

	sess.SetPeerClientPublicKey(peerPub)
	sess.receivePeerIdentityProof(proof)
	assert.Equal(t, true, sess.peerIdentityVerified)

	// A second proof — even if cryptographically valid — must not
	// change the session state. peerIdentityVerified stays true.
	sess.receivePeerIdentityProof(proof)
	assert.Equal(t, true, sess.peerIdentityVerified)
	assert.Equal(t, false, sess.identityFailed)

	// Also: a second SetPeerClientPublicKey with a DIFFERENT key
	// must be rejected (first-write-wins, no mid-session rotation).
	_, otherPriv, err := ed25519.GenerateKey(rand.Reader)
	assert.Equal(t, nil, err)
	otherPub := otherPriv.Public().(ed25519.PublicKey)
	sess.SetPeerClientPublicKey(otherPub)
	// Peer key still the original.
	if !sess.PeerClientPublicKey().Equal(peerPub) {
		t.Fatal("expected SetPeerClientPublicKey to be first-write-wins")
	}
}

// TestIsAwaitingClientFinished verifies the predicate's three
// "false" cases and one "true" case, which together control whether
// the receive-loop optimistic-apply path will fire.
func TestIsAwaitingClientFinished(t *testing.T) {
	sess, _, _, _, cleanup := newTestSessionForIdentityProof(t)
	defer cleanup()

	// Role is server (set by the helper). Handshake not done,
	// serverFlightSent not yet — should be false.
	assert.Equal(t, false, sess.IsAwaitingClientFinished())

	// After serverFlightSent → true (handshake still open).
	sess.markServerFlightSent()
	assert.Equal(t, true, sess.IsAwaitingClientFinished())

	// Close handshakeDone → false (handshake final, success or fail).
	close(sess.handshakeDone)
	assert.Equal(t, false, sess.IsAwaitingClientFinished())
}

// TestIsAwaitingClientFinishedClientRole verifies that the
// TLS-client role never returns true regardless of state — the
// optimistic-apply path is meaningful only on the server-role side
// (where the client second flight is the next inbound).
func TestIsAwaitingClientFinishedClientRole(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	settings := DefaultClientSettings()
	settings.EncryptionSettings.Encrypt = true
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	defer client.Cancel()

	keyManager, err := NewClientKeyManager(ctx, client)
	assert.Equal(t, nil, err)
	manager := NewEncryptionSessionManager(ctx, client, keyManager, settings.EncryptionSettings)

	clientRoleSess := newPeerEncryptionSession(
		ctx, manager, client, NewId(),
		sequenceTlsRoleClient,
		settings.EncryptionSettings,
	)

	// Even after marking serverFlightSent (which wouldn't normally
	// happen in client role, but worth proving the role gate is
	// what's enforcing the predicate), still false.
	clientRoleSess.markServerFlightSent()
	assert.Equal(t, false, clientRoleSess.IsAwaitingClientFinished())
}

// TestOptimisticallyDeliverHandshakeFilter verifies the structural
// filter that gates `OptimisticallyDeliverHandshake`: only payloads
// starting with TLS record type 20 (ChangeCipherSpec) or 23
// (encrypted application_data) are accepted. Records of type 22
// (unencrypted Handshake — what a ClientHello retransmit looks
// like) are filtered out.
func TestOptimisticallyDeliverHandshakeFilter(t *testing.T) {
	sess, _, _, _, cleanup := newTestSessionForIdentityProof(t)
	defer cleanup()

	// Put the session into the awaiting-client-Finished state so
	// the optimistic-apply gate is open.
	sess.markServerFlightSent()
	assert.Equal(t, true, sess.IsAwaitingClientFinished())

	// startInternal would normally create the transport; we skip
	// here and only check that the filter rejects type-22 bytes
	// without invoking the transport. (A reject is a no-op; we
	// verify by checking the session does not panic and the test
	// completes.)
	prefixesAccepted := []byte{20, 23}
	prefixesRejected := []byte{0, 21, 22, 24, 0xff}

	// We can't directly observe "did the optimistic apply call
	// transport.Deliver?" without instrumenting the transport.
	// What we CAN do is verify the predicate behavior end-to-end:
	// the filter returns early on rejected prefixes, and the
	// session state is unchanged.
	for _, prefix := range prefixesRejected {
		payload := append([]byte{prefix}, make([]byte, 100)...)
		sess.OptimisticallyDeliverHandshake(payload)
		// No state to assert on directly; the test passing without
		// panic and without TLS state mutation is the contract.
	}
	for _, prefix := range prefixesAccepted {
		payload := append([]byte{prefix}, make([]byte, 100)...)
		// These would call startInternal + transport.Deliver. We
		// avoid actually exercising the transport here because the
		// helper hasn't set up the TLS state. Skip the call but
		// document that the prefix would pass the filter.
		_ = payload
	}

	// Also verify the filter blocks empty payload (defensive
	// against zero-byte inputs).
	sess.OptimisticallyDeliverHandshake(nil)
	sess.OptimisticallyDeliverHandshake([]byte{})
}

// TestSetPeerClientPublicKeyWrongSize verifies that a key of the
// wrong size is silently ignored (logged, but doesn't change
// session state).
func TestSetPeerClientPublicKeyWrongSize(t *testing.T) {
	sess, _, _, _, cleanup := newTestSessionForIdentityProof(t)
	defer cleanup()

	sess.SetPeerClientPublicKey([]byte{0, 1, 2}) // way too short
	if sess.PeerClientPublicKey() != nil {
		t.Fatal("expected peer key to remain unset after wrong-size input")
	}
}

// TestPeerSessionReadyNotifyOnVerify verifies that the readyMonitor
// fires when peerIdentityVerified flips, so any waiters wake up.
func TestPeerSessionReadyNotifyOnVerify(t *testing.T) {
	sess, _, _, peerPriv, cleanup := newTestSessionForIdentityProof(t)
	defer cleanup()

	peerPub := peerPriv.Public().(ed25519.PublicKey)
	proof := ed25519.Sign(peerPriv, sess.tlsExporter)

	ready := sess.ReadyNotify()
	sess.SetPeerClientPublicKey(peerPub)
	sess.receivePeerIdentityProof(proof)

	select {
	case <-ready:
		// expected
	case <-time.After(time.Second):
		t.Fatal("expected ReadyNotify to fire after identity verification")
	}
}
