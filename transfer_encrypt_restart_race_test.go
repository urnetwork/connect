package connect

// Two races in the encryption layer cost a client restarted in place, under its
// kept identity, one resend interval on its first stream to a listener that held
// its old session: about one first stream in two hundred, measured by sidecar's
// TestEdgeClientPoolRestartedInPlaceReachesItsListenerAgain after QD-1232's
// fixes.
//
//   - The listener, the responder of the restarted client's new handshake, sends
//     its identity proof before it verifies the client's, which reached it before
//     its handshake completed. The client verifies the listener's proof and seals
//     its first open at once, and the open reached the listener before its own
//     verification did: no cipher opened it, and it waited for the client's
//     resend. The unwrap path now verifies the proof the epoch holds and opens
//     the wrap (`peerEncryptionSession.verifyHeldPeerIdentityProofForUnwrap`).
//   - The listener seals under the old epoch until the new one establishes
//     there. The restarted client, which holds none of its previous process's
//     epochs, drops such a write and nacks; the listener, which cannot
//     re-handshake as the responder, ignored the nack, and the write waited for
//     its own timer, backed off or deferred through the restart. The nack now
//     owes the client a reseal: once the client seals under the established
//     epoch, the listener writes again at once what it last sealed under another
//     (`peerEncryptionSession.oweResealForNack`, `SendSequence.resealUnreadable`).
//
// The restart tests run real clients on conditioned loopback routes
// (transfer_restart_first_stream_test.go's pair) with barriers on the listener's
// identity exchange, so each drives its race's ordering every run; their bounds
// are real time, each a broken run's, and the timers a broken run waits for are
// set past them. The session tests drive the same decisions directly.

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

// The listener writes to the restarted edge while its new session with the edge
// is still handshaking (a barrier holds the completion), so it seals the write
// under the old session, which the restarted edge never had: the edge drops it
// and nacks. Once the new session establishes and the edge seals under it, the
// listener writes the old write again at once, sealed under the new session, and
// it reaches the edge within ReachTimeout of the barrier's release. The
// listener's resend timers are set past the bound, and the listener writes
// nothing after it that the edge could acknowledge, so a run that leaves the
// write to its timer, as connect did, fails.
func TestListenerResealsAtOnceWhatTheRestartedClientCouldNotOpen(t *testing.T) {
	settings := defaultRestartFirstStreamTestSettings()
	ctx, cancel := context.WithTimeout(context.Background(), 3*settings.StepTimeout)
	defer cancel()

	hooks := &restartRaceHooks{}
	completionHeld := make(chan struct{}, 1)
	nackHandled := make(chan struct{}, 1)
	release := make(chan struct{})
	pair := newRestartFirstStreamPair(t, ctx, settings, func(s *ClientSettings) {
		s.SendBufferSettings.MinResendInterval = settings.ColdResendInterval
		s.SendBufferSettings.RttMinResendInterval = settings.ColdResendInterval
		s.SendBufferSettings.MaxResendInterval = settings.ColdResendInterval
		s.EncryptionSettings.beforeHandshakeCompletionForTest = func(peerId Id, role sequenceTlsRole) {
			if !hooks.matches(peerId, role) {
				return
			}
			restartRaceSignal(completionHeld)
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		s.EncryptionSettings.unknownWrapNackHandledForTest = func(peerId Id, role sequenceTlsRole, nackEpochId Id) {
			if hooks.matches(peerId, role) {
				restartRaceSignal(nackHandled)
			}
		}
	}, nil)
	defer pair.close()
	var releaseOnce sync.Once
	releaseCompletion := func() {
		releaseOnce.Do(func() { close(release) })
	}
	defer releaseCompletion()

	kept, key := pair.runToRestart()
	hooks.arm(pair.edgeId)
	pair.startEdge(kept)
	restarted := pair.currentEdge()
	sent := make(chan bool, 1)
	go func() {
		sent <- restarted.SendWithTimeout(requiredGateFrame(t, "open after the restart"), pair.listenerId, func(error) {}, settings.StepTimeout)
	}()

	restartRaceAwait(t, completionHeld, settings.StepTimeout, "the listener's completion of the restarted edge's handshake")
	// sealed under the old session, the only one the listener has established
	pair.listenerSends("held", key)
	restartRaceAwait(t, nackHandled, settings.StepTimeout, "the restarted edge's nack of the write it could not open")
	releaseTime := time.Now()
	releaseCompletion()

	if _, ok := restartFirstStreamWait(pair.listenerGot, "open after the restart", time.Now().Add(settings.StepTimeout)); !ok {
		t.Fatal("the restarted edge's open did not reach the listener")
	}
	if _, ok := restartFirstStreamWait(pair.edgeGot, "held", releaseTime.Add(settings.ReachTimeout)); !ok {
		t.Fatalf("the listener's write sealed under the old session did not reach the restarted edge within %s (reseal writes %d)",
			settings.ReachTimeout, pair.listener.SendRecoveryStats().ResealWriteCount)
	}
	if writes := pair.listener.SendRecoveryStats().ResealWriteCount; writes == 0 {
		t.Fatal("the write arrived without a reseal: the scenario did not happen")
	}
	select {
	case ok := <-sent:
		if !ok {
			t.Fatal("the restarted edge's send was refused")
		}
	case <-time.After(settings.StepTimeout):
		t.Fatal("the restarted edge's send did not return")
	}
	t.Logf("the write sealed under the old session reached the restarted edge %s after the new session's completion was released", time.Since(releaseTime))
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

// A responder session, alone, whose reseal decisions tests drive through its nack
// and unwrap entries, recording each reseal it asks for.
type resealTestSession struct {
	session              *peerEncryptionSession
	requestedGenerations []uint8
}

func newResealTestSession() *resealTestSession {
	r := &resealTestSession{}
	settings := DefaultEncryptionSettings()
	settings.resealRequestedForTest = func(peerId Id, role sequenceTlsRole, sealGeneration uint8) {
		r.requestedGenerations = append(r.requestedGenerations, sealGeneration)
	}
	r.session = &peerEncryptionSession{
		peerId:   NewId(),
		role:     sequenceTlsRoleServer,
		settings: settings,
	}
	return r
}

// Establishes a new epoch, as a verified handshake does, and answers it.
func (self *resealTestSession) establish() *tlsHandshakeEpoch {
	_, cancel := context.WithCancel(context.Background())
	e := &tlsHandshakeEpoch{
		epochId:              NewId(),
		cancel:               cancel,
		derivedTlsCipher:     &sequenceCipher{},
		peerIdentityVerified: true,
	}
	self.session.stateLock.Lock()
	defer self.session.stateLock.Unlock()
	self.session.epoch = e
	self.session.markEstablishedWithLock(e)
	return e
}

// Takes the reseals asked for since the last take.
func (self *resealTestSession) takeRequests() []uint8 {
	requests := self.requestedGenerations
	self.requestedGenerations = nil
	return requests
}

// A client restarted in place, or one that lost its session and so names no
// epoch, nacks a wrap sealed under the epoch its previous process held. The
// reseal waits until the client seals under an epoch established since: a wrap
// under the old epoch, a straggler from the previous process, shows nothing.
// Under the new epoch the reseal is asked for once, and neither later wraps nor
// a late copy of the nack ask for it again.
func TestResealWaitsForThePeerToSealUnderAnEpochItHolds(t *testing.T) {
	for _, nackEpochId := range []Id{NewId(), {}} {
		r := newResealTestSession()
		old := r.establish()
		// the previous process sealed under the old epoch
		r.session.observePeerSealed(old.derivedTlsCipher)

		r.session.handleUnknownWrapNack(nackEpochId)
		r.session.observePeerSealed(old.derivedTlsCipher)
		if requests := r.takeRequests(); len(requests) != 0 {
			t.Fatalf("nack %s: a reseal was asked for under the epoch the peer does not hold: %v", nackEpochId, requests)
		}

		renewed := r.establish()
		r.session.observePeerSealed(renewed.derivedTlsCipher)
		if requests := r.takeRequests(); len(requests) != 1 || requests[0] != renewed.derivedTlsCipher.sealGeneration {
			t.Fatalf("nack %s: once the peer sealed under the new epoch, reseals %v, want one under generation %d",
				nackEpochId, requests, renewed.derivedTlsCipher.sealGeneration)
		}
		r.session.observePeerSealed(renewed.derivedTlsCipher)
		r.session.handleUnknownWrapNack(renewed.epochId)
		if requests := r.takeRequests(); len(requests) != 0 {
			t.Fatalf("nack %s: the new epoch was resealed under again: %v", nackEpochId, requests)
		}
	}
}

// The nack reaches the listener after the restarted client has sealed under the
// established epoch: its wrap under the old epoch was still in flight when the
// listener established. The reseal is asked for at once.
func TestLateNackFromAPeerSealingUnderTheEstablishedEpochResealsAtOnce(t *testing.T) {
	r := newResealTestSession()
	r.establish()
	established := r.establish()
	r.session.observePeerSealed(established.derivedTlsCipher)
	if requests := r.takeRequests(); len(requests) != 0 {
		t.Fatalf("reseals %v with no nack", requests)
	}
	r.session.handleUnknownWrapNack(established.epochId)
	if requests := r.takeRequests(); len(requests) != 1 || requests[0] != established.derivedTlsCipher.sealGeneration {
		t.Fatalf("reseals %v, want one under generation %d", requests, established.derivedTlsCipher.sealGeneration)
	}
}

// A nack naming the prior epoch is from a peer that reads it and has yet to
// verify the established one: it reads everything sealed under the prior, and
// what it dropped went out under the established epoch, which a reseal leaves
// alone. Nothing is owed.
func TestNackFromAPeerOnThePriorEpochOwesNoReseal(t *testing.T) {
	r := newResealTestSession()
	prior := r.establish()
	established := r.establish()
	r.session.handleUnknownWrapNack(prior.epochId)
	r.session.observePeerSealed(prior.derivedTlsCipher)
	r.session.observePeerSealed(established.derivedTlsCipher)
	if requests := r.takeRequests(); len(requests) != 0 {
		t.Fatalf("reseals %v for a peer that reads the prior epoch", requests)
	}
	if r.session.resealOwed.Load() {
		t.Fatal("a reseal is owed to a peer that reads the prior epoch")
	}
}

// A reseal makes due at once each unacknowledged Pack the sequence last sealed
// under another epoch than the established one, as receiver-state recovery,
// and leaves alone a Pack sealed under the established epoch, one written in
// plaintext, which the peer reads, and one the peer acknowledged selectively,
// which it holds. A second reseal before the write changes nothing.
func TestResealMakesDueOnlyWhatWentOutUnderAnotherEpoch(t *testing.T) {
	r := newResealTestSession()
	old := r.establish()
	established := r.establish()
	cases := []struct {
		sealGeneration uint8
		selectiveAcked bool
		resealed       bool
	}{
		{sealGeneration: old.derivedTlsCipher.sealGeneration, resealed: true},
		{sealGeneration: established.derivedTlsCipher.sealGeneration},
		{sealGeneration: 0},
		{sealGeneration: old.derivedTlsCipher.sealGeneration, selectiveAcked: true},
	}
	for _, c := range cases {
		backlog := newCompactHeadBacklog(t)
		backlog.sequence.session = r.session
		item := backlog.one
		due := item.resendTime
		item.sealGeneration = c.sealGeneration
		item.selectiveAcked = c.selectiveAcked
		at := time.Now()
		backlog.sequence.resealUnreadable(at)
		if c.resealed {
			if item.recoveryKind != sendRecoveryResealed || item.resendTime.After(at) ||
				backlog.sequence.resendQueue.GetByMessageId(item.messageId) != item {
				t.Fatalf("%+v: the Pack is not due at once as a reseal: kind %d", c, item.recoveryKind)
			}
			backlog.sequence.resealUnreadable(time.Now())
			if item.recoveryKind != sendRecoveryResealed || item.resendTime.After(at) {
				t.Fatalf("%+v: a second reseal changed the Pack", c)
			}
		} else if item.recoveryKind != sendRecoveryNone || item.resendTime != due {
			t.Fatalf("%+v: the Pack was made due: kind %d", c, item.recoveryKind)
		}
		for _, other := range []*sendItem{backlog.head, backlog.two} {
			if other.recoveryKind != sendRecoveryNone {
				t.Fatalf("%+v: Pack %d, written in plaintext, was made due", c, other.sequenceNumber)
			}
		}
	}
}
