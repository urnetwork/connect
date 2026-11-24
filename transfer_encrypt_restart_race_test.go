package connect

// A race in the encryption layer cost a client restarted in place, under its
// kept identity, one resend interval on its first stream to a listener that held
// its old session: part of the one first stream in two hundred that sidecar's
// TestEdgeClientPoolRestartedInPlaceReachesItsListenerAgain measured slow after
// QD-1232's fixes.
//
//   - The listener, the responder of the restarted client's new handshake, sends
//     its identity proof before it verifies the client's, which reached it before
//     its handshake completed. The client verifies the listener's proof and seals
//     its first open at once, and the open reached the listener before its own
//     verification did: no cipher opened it, and it waited for the client's
//     resend. The unwrap path now verifies the proof the epoch holds and opens
//     the wrap (`peerEncryptionSession.verifyHeldPeerIdentityProofForUnwrap`).
//
// The restart test runs real clients on conditioned loopback routes
// (transfer_restart_first_stream_test.go's pair) with barriers on the listener's
// identity exchange, so it drives the race's ordering every run; its bounds are
// real time, each a broken run's, and the timer a broken run waits for is set
// past them. The session tests drive the same decisions directly.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Hooks on the listener's identity exchange with one peer, armed once the pair
// reaches its restart, so the first session's handshake runs free.
type restartRaceHooks struct {
	armed  atomic.Bool
	peerId atomic.Pointer[Id]
}

// Whether a hook call is the listener's responder session with the armed peer.
func (self *restartRaceHooks) matches(peerId Id, role sequenceTlsRole) bool {
	if !self.armed.Load() || role != sequenceTlsRoleServer {
		return false
	}
	armedPeerId := self.peerId.Load()
	return armedPeerId != nil && *armedPeerId == peerId
}

func (self *restartRaceHooks) arm(peerId Id) {
	self.peerId.Store(&peerId)
	self.armed.Store(true)
}

// Signals edge without blocking; one pending signal is enough to wake its
// waiter.
func restartRaceSignal(edge chan struct{}) {
	select {
	case edge <- struct{}{}:
	default:
	}
}

// Waits for edge until timeout, and fails the test with what otherwise.
func restartRaceAwait(t *testing.T, edge chan struct{}, timeout time.Duration, what string) {
	t.Helper()
	select {
	case <-edge:
	case <-time.After(timeout):
		t.Fatalf("%s did not happen within %s", what, timeout)
	}
}

// The restarted edge seals its first open under its new session as soon as it
// has verified the listener's identity proof, while the listener, which sent
// that proof, has not yet verified the edge's: the edge's proof reached the
// listener before its handshake completed (a barrier holds the completion until
// it has), and a barrier holds the completion between sending the listener's
// proof and verifying the edge's. The open reaches the listener within
// ReachTimeout all the same: the unwrap path verifies the edge's proof and opens
// it. The edge's resend is set past the bound, so a run that drops the open, as
// connect did, fails.
func TestRestartedClientsFirstSealedOpenReachesTheListenerBeforeItsVerification(t *testing.T) {
	settings := defaultRestartFirstStreamTestSettings()
	ctx, cancel := context.WithTimeout(context.Background(), 3*settings.StepTimeout)
	defer cancel()

	hooks := &restartRaceHooks{}
	proofBuffered := make(chan struct{}, 1)
	completionHeld := make(chan struct{}, 1)
	release := make(chan struct{})
	pair := newRestartFirstStreamPair(t, ctx, settings, func(s *ClientSettings) {
		s.EncryptionSettings.peerIdentityProofBufferedForTest = func(peerId Id, role sequenceTlsRole) {
			if hooks.matches(peerId, role) {
				restartRaceSignal(proofBuffered)
			}
		}
		s.EncryptionSettings.beforeHandshakeCompletionForTest = func(peerId Id, role sequenceTlsRole) {
			if !hooks.matches(peerId, role) {
				return
			}
			// the edge's proof is here before the handshake completes
			select {
			case <-proofBuffered:
			case <-ctx.Done():
			}
		}
		s.EncryptionSettings.afterIdentityProofSentForTest = func(peerId Id, role sequenceTlsRole) {
			if !hooks.matches(peerId, role) {
				return
			}
			restartRaceSignal(completionHeld)
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
	}, func(s *ClientSettings) {
		s.SendBufferSettings.MinResendInterval = settings.ColdResendInterval
		s.SendBufferSettings.RttMinResendInterval = settings.ColdResendInterval
		s.SendBufferSettings.MaxResendInterval = settings.ColdResendInterval
	})
	defer pair.close()
	var releaseOnce sync.Once
	releaseCompletion := func() {
		releaseOnce.Do(func() { close(release) })
	}
	defer releaseCompletion()

	kept, _ := pair.runToRestart()
	hooks.arm(pair.edgeId)
	startTime := time.Now()
	pair.startEdge(kept)
	// The Required gate holds the send until the restarted edge's session
	// seals, which takes the listener's proof, so the send runs beside the
	// barriers.
	restarted := pair.currentEdge()
	sent := make(chan bool, 1)
	go func() {
		sent <- restarted.SendWithTimeout(requiredGateFrame(t, "open after the restart"), pair.listenerId, func(error) {}, settings.StepTimeout)
	}()

	restartRaceAwait(t, completionHeld, settings.StepTimeout, "the listener's completion of the restarted edge's handshake")
	if _, ok := restartFirstStreamWait(pair.listenerGot, "open after the restart", startTime.Add(settings.ReachTimeout)); !ok {
		t.Fatalf("the restarted edge's first sealed open did not reach the listener within %s while the listener's verification of the edge's proof was held",
			settings.ReachTimeout)
	}
	releaseCompletion()
	select {
	case ok := <-sent:
		if !ok {
			t.Fatal("the restarted edge's send was refused")
		}
	case <-time.After(settings.StepTimeout):
		t.Fatal("the restarted edge's send did not return")
	}
	t.Logf("the restarted edge's first sealed open reached the listener %s after the restart", time.Since(startTime))
}

// A responder's session with a peer, on a real client, whose newest epoch has
// completed its handshake and holds the peer's identity proof unverified, as it
// does between sending its own proof and verifying the peer's: the epoch's
// cipher opens what the peer seals under the epoch.
type heldProofSession struct {
	client  *Client
	session *peerEncryptionSession
	peerId  Id
	epoch   *tlsHandshakeEpoch
}

// signer signs the held proof, the peer's own key when nil.
func newHeldProofSession(t *testing.T, settings *ClientSettings, signer ed25519.PrivateKey) *heldProofSession {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	settings.EncryptionSettings.Mode = EncryptionModeRequired
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	t.Cleanup(client.Cancel)
	peerId := NewId()
	session := client.encryptionSessionManager.Acquire(peerId, sequenceTlsRoleServer, false)
	if session == nil {
		t.Fatal("no responder session")
	}
	t.Cleanup(session.Release)
	peerPub, peerPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if signer == nil {
		signer = peerPriv
	}
	exporter := make([]byte, sequenceTlsIdentityProofLength)
	if _, err := rand.Read(exporter); err != nil {
		t.Fatal(err)
	}
	epochCtx, epochCancel := context.WithCancel(ctx)
	handshakeDone := make(chan struct{})
	close(handshakeDone)
	epoch := &tlsHandshakeEpoch{
		ctx:                      epochCtx,
		cancel:                   epochCancel,
		epochId:                  NewId(),
		handshakeDone:            handshakeDone,
		establishmentDone:        make(chan struct{}),
		tlsExporter:              exporter,
		derivedTlsCipher:         newFrameCodecTestSequenceCipher(t),
		identityProofSent:        true,
		clientHelloAnswered:      true,
		pendingPeerIdentityProof: ed25519.Sign(signer, exporter),
	}
	func() {
		session.stateLock.Lock()
		defer session.stateLock.Unlock()
		session.peerClientPublicKey = peerPub
		session.epoch = epoch
	}()
	return &heldProofSession{client: client, session: session, peerId: peerId, epoch: epoch}
}

// Seals content under the held epoch, as the peer's initiator session does
// once it has verified this side's proof.
func (self *heldProofSession) sealUnderTheEpoch(t *testing.T, content string) []byte {
	t.Helper()
	wrapped, err := self.epoch.derivedTlsCipher.Seal([]byte(content))
	if err != nil {
		t.Fatal(err)
	}
	return wrapped
}

func (self *heldProofSession) established() *tlsHandshakeEpoch {
	self.session.stateLock.Lock()
	defer self.session.stateLock.Unlock()
	return self.session.establishedEpoch
}

// The peer's first wrap under the newest epoch reaches the unwrap path while
// the epoch holds the peer's proof unverified. The unwrap path verifies it,
// which establishes the epoch, and opens the wrap. Before, no cipher opened it
// and it was dropped.
func TestUnwrapVerifiesTheProofTheEpochHoldsAndOpensWhatWasSealedUnderIt(t *testing.T) {
	held := newHeldProofSession(t, DefaultClientSettings(), nil)
	companion := false
	plaintext, role, openedCompanion, _, err := held.client.unwrapFrame(
		held.peerId, sequenceTlsRoleClient.toProtobuf(), &companion, held.sealUnderTheEpoch(t, "first under the new epoch"))
	if err != nil {
		t.Fatalf("the wrap sealed under the epoch whose proof the session held was not opened: %v", err)
	}
	defer MessagePoolReturn(plaintext)
	if string(plaintext) != "first under the new epoch" || role != sequenceTlsRoleServer || openedCompanion {
		t.Fatalf("opened %q as %s companion=%t", plaintext, role, openedCompanion)
	}
	if held.established() != held.epoch {
		t.Fatal("the epoch whose proof verified is not the established one")
	}
}

// A held proof that does not verify, signed by another key than the peer's,
// opens nothing: the unwrap path's verification fails the epoch, as the
// completion's would, and the wrap is refused.
func TestUnwrapOpensNothingUnderAnEpochWhoseHeldProofFails(t *testing.T) {
	_, otherKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	held := newHeldProofSession(t, DefaultClientSettings(), otherKey)
	companion := false
	plaintext, _, _, _, err := held.client.unwrapFrame(
		held.peerId, sequenceTlsRoleClient.toProtobuf(), &companion, held.sealUnderTheEpoch(t, "under an unproven epoch"))
	if err == nil {
		MessagePoolReturn(plaintext)
		t.Fatal("a wrap was opened under an epoch whose proof did not verify")
	}
	if held.established() != nil {
		t.Fatal("an epoch whose proof did not verify was established")
	}
	failed := func() bool {
		held.session.stateLock.Lock()
		defer held.session.stateLock.Unlock()
		return held.epoch.identityFailedTerminal && !held.epoch.peerIdentityVerified
	}()
	if !failed {
		t.Fatal("the epoch whose proof did not verify was not failed")
	}
	if ciphers, _ := held.session.decryptCiphers(); len(ciphers) != 0 {
		t.Fatalf("%d receive ciphers after a failed proof", len(ciphers))
	}
}

// The completion and the unwrap path verify one held proof at once: the
// completion checks the signature first and is held before it records the
// verdict, while the unwrap path checks it and records it. The session seals
// once, and the completion, released, does nothing more.
func TestTwoVerificationsOfOneHeldProofSealTheSessionOnce(t *testing.T) {
	settings := DefaultClientSettings()
	checked := make(chan struct{}, 1)
	release := make(chan struct{})
	var checks atomic.Int32
	settings.EncryptionSettings.peerIdentityProofCheckedForTest = func(peerId Id, role sequenceTlsRole) {
		if checks.Add(1) == 1 {
			restartRaceSignal(checked)
			<-release
		}
	}
	held := newHeldProofSession(t, settings, nil)
	var sealedCount atomic.Int32
	held.client.encryptionSessionManager.AddEncryptionEventCallback(func(event *EncryptionEvent) {
		if event.PeerId == held.peerId && event.Type == EncryptionEventSealed {
			sealedCount.Add(1)
		}
	})

	completion := make(chan struct{})
	go func() {
		defer close(completion)
		held.session.maybeVerifyPendingPeerIdentityProof(held.epoch)
	}()
	restartRaceAwait(t, checked, 10*time.Second, "the completion's signature check")
	if !held.session.verifyHeldPeerIdentityProofForUnwrap() {
		close(release)
		t.Fatal("the unwrap path did not establish the epoch")
	}
	close(release)
	select {
	case <-completion:
	case <-time.After(10 * time.Second):
		t.Fatal("the completion's verification did not return")
	}
	if got := sealedCount.Load(); got != 1 {
		t.Fatalf("the session sealed %d times", got)
	}
	if held.established() != held.epoch {
		t.Fatal("the verified epoch is not the established one")
	}
}
