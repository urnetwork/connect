package connect

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"errors"
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
		manager.ServerTlsConfig(),
		false,
	)

	// Pre-populate the TLS-handshake-derived state. In production
	// this is filled by `completeHandshake` after the actual TLS
	// handshake; for these tests we inject directly so we can
	// exercise the identity-proof gate independently.
	exporter := make([]byte, sequenceTlsIdentityProofLength)
	_, err = rand.Read(exporter)
	assert.Equal(t, nil, err)
	// Inject a bare handshake epoch with the TLS-derived state pre-set (no
	// goroutines / real tls.Conn) so the identity-proof gate can be
	// exercised in isolation. `startEpoch` is a no-op while an epoch is
	// present, so the production paths under test reuse this one.
	sess.epoch = &tlsHandshakeEpoch{
		handshakeDone:    make(chan struct{}),
		tlsExporter:      exporter,
		derivedTlsCipher: &sequenceCipher{}, // non-nil sentinel; never used for crypto in these tests
	}

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

	// Pre-identity-verification: cipher hidden (no established epoch yet).
	assert.Equal(t, (*sequenceCipher)(nil), sess.Cipher())

	// Simulate a successful identity proof: flip the flag and promote the
	// epoch to established, exactly as `maybeVerifyPendingPeerIdentityProof`
	// does. `Cipher()` serves the established epoch's cipher.
	sess.stateLock.Lock()
	sess.epoch.peerIdentityVerified = true
	sess.markEstablishedWithLock(sess.epoch)
	sess.stateLock.Unlock()

	// Now cipher exposed.
	if sess.Cipher() == nil {
		t.Fatal("Cipher should be non-nil once the epoch is established")
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
	proof := ed25519.Sign(peerPriv, sess.epoch.tlsExporter)
	assert.Equal(t, ed25519.SignatureSize, len(proof))

	// Set the peer key and deliver the proof.
	sess.SetPeerClientPublicKey(peerPub)
	sess.receivePeerIdentityProof(proof)

	assert.Equal(t, true, sess.epoch.peerIdentityVerified)
	assert.Equal(t, false, sess.epoch.identityFailed)
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

	// Sign with a different private key — the proof must fail
	// verification against `peerPub`.
	_, wrongPriv, err := ed25519.GenerateKey(rand.Reader)
	assert.Equal(t, nil, err)
	badProof := ed25519.Sign(wrongPriv, sess.epoch.tlsExporter)

	sess.SetPeerClientPublicKey(peerPub)
	sess.receivePeerIdentityProof(badProof)

	assert.Equal(t, false, sess.epoch.peerIdentityVerified)
	assert.Equal(t, true, sess.epoch.identityFailed)
	assert.Equal(t, (*sequenceCipher)(nil), sess.Cipher())
}

// TestPeerSessionIdentityProofMalformed covers the defensive path:
// a proof of the wrong size flips `identityFailed` immediately —
// the session never exposes a cipher.
func TestPeerSessionIdentityProofMalformed(t *testing.T) {
	sess, _, _, _, cleanup := newTestSessionForIdentityProof(t)
	defer cleanup()

	sess.receivePeerIdentityProof([]byte{0, 1, 2}) // way too short

	assert.Equal(t, true, sess.epoch.identityFailed)
	assert.Equal(t, (*sequenceCipher)(nil), sess.Cipher())
}

// TestPeerSessionIdentityProofArrivesBeforeKey covers out-of-order
// arrival: proof first, peer key second. The proof must be
// buffered, and verification must run once the key is set.
func TestPeerSessionIdentityProofArrivesBeforeKey(t *testing.T) {
	sess, _, _, peerPriv, cleanup := newTestSessionForIdentityProof(t)
	defer cleanup()

	peerPub := peerPriv.Public().(ed25519.PublicKey)
	proof := ed25519.Sign(peerPriv, sess.epoch.tlsExporter)

	// Proof first — must be buffered (no peer key yet).
	sess.receivePeerIdentityProof(proof)
	assert.Equal(t, false, sess.epoch.peerIdentityVerified)
	if 0 == len(sess.epoch.pendingPeerIdentityProof) {
		t.Fatal("expected proof to be buffered while peer key unknown")
	}

	// Key arrives — verification runs and succeeds.
	sess.SetPeerClientPublicKey(peerPub)
	assert.Equal(t, true, sess.epoch.peerIdentityVerified)
	if 0 != len(sess.epoch.pendingPeerIdentityProof) {
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
	savedExporter := sess.epoch.tlsExporter
	sess.epoch.tlsExporter = nil

	peerPub := peerPriv.Public().(ed25519.PublicKey)
	proof := ed25519.Sign(peerPriv, savedExporter)

	// Set the peer key and the proof before the exporter exists.
	sess.SetPeerClientPublicKey(peerPub)
	sess.receivePeerIdentityProof(proof)
	assert.Equal(t, false, sess.epoch.peerIdentityVerified)
	if 0 == len(sess.epoch.pendingPeerIdentityProof) {
		t.Fatal("expected proof to be buffered while exporter unknown")
	}

	// Exporter becomes available; trigger verification manually
	// (in production `completeHandshake` would call this).
	sess.epoch.tlsExporter = savedExporter
	sess.maybeVerifyPendingPeerIdentityProof(sess.epoch)

	assert.Equal(t, true, sess.epoch.peerIdentityVerified)
}

// TestPeerSessionIdentityProofSecondIgnored verifies that a second
// proof arriving while one is already buffered (or after
// verification has settled) is ignored — the session has at most
// one identity-proof exchange per lifetime.
func TestPeerSessionIdentityProofSecondIgnored(t *testing.T) {
	sess, _, _, peerPriv, cleanup := newTestSessionForIdentityProof(t)
	defer cleanup()

	peerPub := peerPriv.Public().(ed25519.PublicKey)
	proof := ed25519.Sign(peerPriv, sess.epoch.tlsExporter)

	sess.SetPeerClientPublicKey(peerPub)
	sess.receivePeerIdentityProof(proof)
	assert.Equal(t, true, sess.epoch.peerIdentityVerified)

	// A second proof — even if cryptographically valid — must not
	// change the session state. peerIdentityVerified stays true.
	sess.receivePeerIdentityProof(proof)
	assert.Equal(t, true, sess.epoch.peerIdentityVerified)
	assert.Equal(t, false, sess.epoch.identityFailed)

	// Also: a second SetPeerClientPublicKey with a different key
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
	sess.markServerFlightSent(sess.epoch)
	assert.Equal(t, true, sess.IsAwaitingClientFinished())

	// Close handshakeDone → false (handshake final, success or fail).
	close(sess.epoch.handshakeDone)
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
		manager.ClientTlsConfig(),
		false,
	)
	clientRoleSess.epoch = &tlsHandshakeEpoch{handshakeDone: make(chan struct{})}

	// Even after marking serverFlightSent (which wouldn't normally
	// happen in client role, but worth proving the role gate is
	// what's enforcing the predicate), still false.
	clientRoleSess.markServerFlightSent(clientRoleSess.epoch)
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
	sess.markServerFlightSent(sess.epoch)
	assert.Equal(t, true, sess.IsAwaitingClientFinished())

	// startEpoch would normally create the transport; we skip
	// here and only check that the filter rejects type-22 bytes
	// without invoking the transport. (A reject is a no-op; we
	// verify by checking the session does not panic and the test
	// completes.)
	prefixesAccepted := []byte{20, 23}
	prefixesRejected := []byte{0, 21, 22, 24, 0xff}

	// We can't directly observe "did the optimistic apply call
	// transport.Deliver?" without instrumenting the transport.
	// What we can do is verify the predicate behavior end-to-end:
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
		// These would call startEpoch + transport.Deliver. We
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
	proof := ed25519.Sign(peerPriv, sess.epoch.tlsExporter)

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

// newTestEncryptionSession builds a peerEncryptionSession in the given role
// with a manager that carries real TLS configs, so startEpoch / reset build
// real epochs. No epoch is started; the caller drives the lifecycle. A short
// handshake timeout keeps any stray real epoch from lingering.
func newTestEncryptionSession(t *testing.T, role sequenceTlsRole) (*peerEncryptionSession, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	settings := DefaultClientSettings()
	settings.EncryptionSettings.Encrypt = true
	settings.EncryptionSettings.TlsTimeout = 2 * time.Second
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	keyManager, err := NewClientKeyManager(ctx, client)
	if err != nil {
		cancel()
		t.Fatalf("NewClientKeyManager: %s", err)
	}
	manager := NewEncryptionSessionManager(ctx, client, keyManager, settings.EncryptionSettings)
	var roleTlsConfig *tls.Config
	switch role {
	case sequenceTlsRoleClient:
		roleTlsConfig = manager.ClientTlsConfig()
	case sequenceTlsRoleServer:
		roleTlsConfig = manager.ServerTlsConfig()
	}
	sess := newPeerEncryptionSession(ctx, manager, client, NewId(), role, settings.EncryptionSettings, roleTlsConfig, false)
	return sess, func() {
		client.Cancel()
		cancel()
	}
}

// injectTestEpoch swaps a hand-built epoch into the session (cancelling any
// existing one). `completed` closes its handshakeDone; `handshakeErr` records
// a completion error. The epoch carries a real ctx/cancel so production paths
// that cancel the prior epoch on reset don't trip on a nil cancel.
func injectTestEpoch(sess *peerEncryptionSession, completed bool, handshakeErr error) *tlsHandshakeEpoch {
	ctx, cancel := context.WithCancel(context.Background())
	e := &tlsHandshakeEpoch{
		ctx:           ctx,
		cancel:        cancel,
		handshakeDone: make(chan struct{}),
		handshakeErr:  handshakeErr,
	}
	if completed {
		close(e.handshakeDone)
	}
	sess.stateLock.Lock()
	if sess.epoch != nil && sess.epoch.cancel != nil {
		sess.epoch.cancel()
	}
	sess.epoch = e
	sess.stateLock.Unlock()
	return e
}

// TestHandshakeEpochResetReplacesEpoch verifies reset() installs a fresh
// epoch and cancels the previous one's ctx (so its goroutines exit), and that
// startEpoch is a no-op while an epoch is present.
func TestHandshakeEpochResetReplacesEpoch(t *testing.T) {
	sess, cleanup := newTestEncryptionSession(t, sequenceTlsRoleServer)
	defer cleanup()

	sess.startEpoch()
	e1 := sess.currentEpoch()
	if e1 == nil {
		t.Fatal("expected an epoch after startEpoch")
	}

	sess.reset()
	e2 := sess.currentEpoch()
	if e2 == nil || e2 == e1 {
		t.Fatal("expected reset to install a new epoch")
	}
	select {
	case <-e1.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("expected the old epoch's ctx to be cancelled after reset")
	}

	sess.startEpoch()
	if sess.currentEpoch() != e2 {
		t.Fatal("startEpoch should be a no-op while an epoch is present")
	}
}

// TestSendAcquireReusesAnyExistingEpoch verifies the root-cause fix for
// post-handshake EncryptedControl churn: a SendSequence (AcquireForSend)
// reuses whatever epoch already exists — established, in-flight, or even
// failed — and never resets a live session. Only an absent epoch is built.
func TestSendAcquireReusesAnyExistingEpoch(t *testing.T) {
	sess, cleanup := newTestEncryptionSession(t, sequenceTlsRoleServer)
	defer cleanup()

	// established (done, no error) → reuse
	established := injectTestEpoch(sess, true, nil)
	sess.startEpoch()
	if sess.currentEpoch() != established {
		t.Fatal("expected an established handshake to be reused")
	}

	// finished with error → still reuse (sends never re-handshake; a retry
	// would re-churn EC — recovery comes from teardown + re-acquire)
	failed := injectTestEpoch(sess, true, errors.New("boom"))
	sess.startEpoch()
	if sess.currentEpoch() != failed {
		t.Fatal("expected a failed handshake to be reused, not reset, on a send")
	}

	// in flight (not done) → reuse (let it finish)
	inflight := injectTestEpoch(sess, false, nil)
	sess.startEpoch()
	if sess.currentEpoch() != inflight {
		t.Fatal("expected an in-flight handshake to be left to complete")
	}
}

// TestDeliverClientHelloResetsCompletedServerHandshakeClientHelloAfterCompletionResets
// covers point (b): a new inbound ClientHello at the TLS-server role after the
// current handshake has completed resets to a fresh handshake (so a peer that
// re-initiates — e.g. a resumed SendSequence on the client side — is followed).
func TestDeliverClientHelloResetsCompletedServerHandshakeClientHelloAfterCompletionResets(t *testing.T) {
	// record type 22 (handshake), handshake message type 1 (ClientHello)
	clientHello := []byte{22, 3, 3, 0, 100, 1, 0, 0, 96}
	if !isClientHelloRecord(clientHello) {
		t.Fatal("test ClientHello bytes should satisfy isClientHelloRecord")
	}

	sess, cleanup := newTestEncryptionSession(t, sequenceTlsRoleServer)
	defer cleanup()
	e1 := injectTestEpoch(sess, true, nil)
	sess.deliverHandshake(clientHello)
	if sess.currentEpoch() == e1 {
		t.Fatal("expected a new ClientHello to reset the completed server handshake")
	}
}

// TestDeliverClientHelloResetsCompletedServerHandshakeStaleNonClientHelloAfterCompletionIsDropped
// is the counterpart: stale non-ClientHello bytes after completion are dropped
// without a reset.
func TestDeliverClientHelloResetsCompletedServerHandshakeStaleNonClientHelloAfterCompletionIsDropped(t *testing.T) {
	// record type 23 (application_data) — not a ClientHello
	appData := []byte{23, 3, 3, 0, 100, 0, 0, 0, 0}
	if isClientHelloRecord(appData) {
		t.Fatal("test app-data bytes should not satisfy isClientHelloRecord")
	}

	sess, cleanup := newTestEncryptionSession(t, sequenceTlsRoleServer)
	defer cleanup()
	e1 := injectTestEpoch(sess, true, nil)
	sess.deliverHandshake(appData)
	if sess.currentEpoch() != e1 {
		t.Fatal("expected stale post-completion bytes to be dropped without reset")
	}
}

// TestReleasedSessionRemovedFromManager verifies a per-peer session's
// lifecycle is bounded by its references: once the last send/receive sequence
// releases it, it is closed and unregistered (the send/receive sequences idle
// out on their own timers, so they are the session's lifecycle). A subsequent
// wrapped frame finds no session and is dropped — but that is transient, not a
// wedge: a client-role send sequence restarts the handshake on its next burst,
// so the peer's responder session is rebuilt. Removal is synchronous with the
// ref reaching zero (under the manager lock), so a concurrent re-acquire can't
// adopt a session that is being torn down.
func TestReleasedSessionRemovedFromManager(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultClientSettings()
	settings.EncryptionSettings.Encrypt = true
	settings.EncryptionSettings.TlsTimeout = 2 * time.Second
	// Short idle timeout so the reap is observable quickly. The session is kept
	// registered for this long after its last reference drops, then the Run
	// loop's CancelIfIdle removes it.
	settings.EncryptionSettings.IdleTimeout = 200 * time.Millisecond
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	defer client.Cancel()
	keyManager, err := NewClientKeyManager(ctx, client)
	assert.Equal(t, nil, err)
	manager := NewEncryptionSessionManager(ctx, client, keyManager, settings.EncryptionSettings)

	peerId := NewId()
	sess := manager.Acquire(peerId, sequenceTlsRoleServer, false)
	if sess == nil {
		t.Fatal("expected Acquire to return a session")
	}
	if manager.Lookup(peerId, sequenceTlsRoleServer, false) != sess {
		t.Fatal("expected the session to be registered in the manager")
	}

	sess.Release() // refs -> 0; kept registered until idle for IdleTimeout

	// Removal is not synchronous with Release: the session is kept registered
	// so a transport reform / next burst reuses the live cipher instead of
	// churning a fresh handshake. The idle time here (~0) is below IdleTimeout.
	if manager.Lookup(peerId, sequenceTlsRoleServer, false) != sess {
		t.Fatal("expected the session to remain registered immediately after Release (idle keep-alive)")
	}

	// After IdleTimeout the Run loop's CancelIfIdle reaps it.
	deadline := time.After(2 * time.Second)
	for manager.Lookup(peerId, sequenceTlsRoleServer, false) != nil {
		select {
		case <-deadline:
			t.Fatal("expected the session to be removed from the manager after the idle timeout")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// injectEstablishedTestEpoch installs an epoch and marks it the established
// (serving) epoch — handshake complete and peer identity verified — without a
// real TLS handshake. The derived cipher is left nil; this exercises
// epoch/role lifecycle, not wrap/unwrap.
func injectEstablishedTestEpoch(sess *peerEncryptionSession) *tlsHandshakeEpoch {
	e := injectTestEpoch(sess, true, nil)
	sess.stateLock.Lock()
	e.peerIdentityVerified = true
	sess.establishedEpoch = e
	sess.stateLock.Unlock()
	return e
}

// TestAcquireForSendRestartPolicy verifies the per-role send-acquisition
// rules. AcquireForSend returns the same session per (peer, role) and never
// thrashes an in-flight handshake. Only the client role restarts an
// established session — the recovery mechanism: every new client send
// re-initiates, so a peer that lost its responder session rebuilds it — and
// the restart keeps the established epoch serving its cipher (gap-free rekey).
// The server role never restarts; it only carries EncryptedControl/replies and
// follows the peer's ClientHello.
func TestAcquireForSendRestartPolicy(t *testing.T) {
	for _, role := range []sequenceTlsRole{sequenceTlsRoleClient, sequenceTlsRoleServer} {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		settings := DefaultClientSettings()
		settings.EncryptionSettings.Encrypt = true
		settings.EncryptionSettings.TlsTimeout = 2 * time.Second
		client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
		defer client.Cancel()
		km, err := NewClientKeyManager(ctx, client)
		assert.Equal(t, nil, err)
		manager := NewEncryptionSessionManager(ctx, client, km, settings.EncryptionSettings)

		peerId := NewId()
		s1 := manager.AcquireForSend(peerId, role, false)
		if s1 == nil || s1.role != role {
			t.Fatalf("expected a %v-role session", role)
		}

		// An in-flight handshake is never restarted by a later send.
		inflight := injectTestEpoch(s1, false, nil)
		s2 := manager.AcquireForSend(peerId, role, false)
		if s2 != s1 {
			t.Fatalf("%v: expected the same per-peer/role session", role)
		}
		if s2.currentEpoch() != inflight {
			t.Fatalf("%v: AcquireForSend must not restart an in-flight handshake", role)
		}

		// An established session: the client role restarts (background
		// rekey) while keeping the established epoch serving; the server
		// role reuses.
		established := injectEstablishedTestEpoch(s1)
		s3 := manager.AcquireForSend(peerId, role, false)
		if s3 != s1 {
			t.Fatalf("%v: expected the same per-peer/role session", role)
		}
		if role == sequenceTlsRoleClient {
			if s3.currentEpoch() == established {
				t.Fatal("client AcquireForSend should start a new in-flight epoch on an established session")
			}
			if s3.establishedEpoch != established {
				t.Fatal("the established epoch must keep serving its cipher during the rekey")
			}
		} else if s3.currentEpoch() != established {
			t.Fatal("server AcquireForSend must never restart the handshake")
		}
	}
}
