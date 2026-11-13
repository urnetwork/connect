package connect

// Tests for epoch identity on EncryptedControl (see EncryptedControl.epoch_id).
//
// The generation on the wire exists so a control can be routed to the epoch
// that produced it. Without it, a proof bound to the peer's NEW exporter was
// judged against our OLD one, failed as a bad signature, and terminally
// tombstoned a session the peer was still encrypting into — a permanent stall
// (see repro-epochdesync). These cover the routing rules:
//   - foreign proofs never adopt a generation, reset, or tombstone an epoch
//   - only a ClientHello can select the responder's generation
//   - ordering drops stragglers only while a TLS handshake remains in flight;
//     an unauthenticated generation must never become a permanent lower bound
//   - a legacy (unset) generation keeps the pre-epoch behavior
//   - the responder adopts the initiator's generation and echoes it

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
)

func injectTestEpochWithId(sess *peerEncryptionSession, completed bool, handshakeErr error, epochId Id) *tlsHandshakeEpoch {
	e := injectTestEpoch(sess, completed, handshakeErr)
	sess.stateLock.Lock()
	e.epochId = epochId
	sess.stateLock.Unlock()
	return e
}

// olderId/newerId build ulid-ordered generations around a reference.
func olderAndNewerIds(t *testing.T) (older Id, newer Id) {
	t.Helper()
	older = Id{15: 1}
	newer = Id{15: 2}
	if !older.LessThan(newer) {
		t.Fatalf("expected ulid ordering: %s < %s", older, newer)
	}
	return older, newer
}

// A foreign proof cannot prove which generation the peer is on; it must not
// reset or tombstone the current handshake, even when its id sorts higher.
func TestIdentityProofForNewerEpochIgnored(t *testing.T) {
	older, newer := olderAndNewerIds(t)

	sess, cleanup := newTestEncryptionSession(t, sequenceTlsRoleClient)
	defer cleanup()
	e1 := injectTestEpochWithId(sess, true, nil, older)

	sess.receivePeerIdentityProofForEpoch(make([]byte, 64), newer)

	if sess.currentEpoch() != e1 {
		t.Fatal("a foreign-generation proof must not reset the current epoch")
	}
	sess.stateLock.Lock()
	failed := e1.identityFailed
	sess.stateLock.Unlock()
	if failed {
		t.Fatal("a foreign-generation proof must never mark this epoch identity-failed")
	}
}

// TestIdentityProofForOlderEpochIgnored: a straggling proof from a superseded
// generation is dropped — it neither tombstones nor resets the live epoch.
func TestIdentityProofForOlderEpochIgnored(t *testing.T) {
	older, newer := olderAndNewerIds(t)

	sess, cleanup := newTestEncryptionSession(t, sequenceTlsRoleClient)
	defer cleanup()
	e1 := injectTestEpochWithId(sess, true, nil, newer)

	sess.receivePeerIdentityProofForEpoch(make([]byte, 64), older)

	if sess.currentEpoch() != e1 {
		t.Fatal("expected a stale-generation proof to leave the live epoch alone")
	}
	sess.stateLock.Lock()
	failed := e1.identityFailed
	sess.stateLock.Unlock()
	if failed {
		t.Fatal("a stale-generation proof must never mark the epoch identity-failed")
	}
}

// Straggling handshake bytes must not interrupt a live TLS handshake. Once
// that attempt ends, its unauthenticated id cannot block a lower generation.
func TestHandshakeForOlderEpochDropped(t *testing.T) {
	older, newer := olderAndNewerIds(t)
	clientHello := []byte{22, 3, 3, 0, 100, 1, 0, 0, 96}

	sess, cleanup := newTestEncryptionSession(t, sequenceTlsRoleServer)
	defer cleanup()
	e1 := injectTestEpochWithId(sess, false, nil, newer)

	sess.deliverHandshake(clientHello, older)

	if sess.currentEpoch() != e1 {
		t.Fatal("expected stale-generation handshake bytes to be dropped without reset")
	}
}

// Both malformed and correctly sized foreign proofs leave the unnamed
// responder available for the actual ClientHello, without waiting for timeout.
func TestServerUnboundProofCannotChooseEpoch(t *testing.T) {
	for _, size := range []int{1, 64} {
		func() {
			sess, cleanup := newTestEncryptionSession(t, sequenceTlsRoleServer)
			defer cleanup()
			sess.settings.TlsTimeout = 0
			var maxEpochId Id
			for i := range maxEpochId {
				maxEpochId[i] = 0xff
			}
			sess.DeliverEncryptedControl(&protocol.EncryptedControl{
				ControlType: protocol.EncryptedControlType_EncryptedControlIdentityProof,
				EpochId:     maxEpochId.Bytes(), Payload: make([]byte, size),
			})
			e := sess.currentEpoch()
			sess.stateLock.Lock()
			poisoned := e != nil && (e.epochId != (Id{}) || e.identityFailed || len(e.pendingPeerIdentityProof) != 0)
			sess.stateLock.Unlock()
			if poisoned {
				t.Fatalf("%d-byte foreign proof changed the unnamed responder", size)
			}
			legitEpochId := Id{15: 1}
			sess.deliverHandshake([]byte{22, 3, 3, 0, 100, 1, 0, 0, 96}, legitEpochId)
			if e = sess.currentEpoch(); e == nil || sess.epochIdOf(e) != legitEpochId || isClosed(e.handshakeDone) {
				t.Fatalf("%d-byte foreign proof prevented the real ClientHello", size)
			}
		}()
	}
}

// A bogus ClientHello can also carry a maximum id. Its failed TLS attempt
// must release that ordering preference so the legitimate client can recover.
func TestServerEpochIdPoisoningViaClientHello(t *testing.T) {
	sess, cleanup := newTestEncryptionSession(t, sequenceTlsRoleServer)
	defer cleanup()
	sess.settings.TlsTimeout = 0
	var maxEpochId Id
	for i := range maxEpochId {
		maxEpochId[i] = 0xff
	}
	clientHello := []byte{22, 3, 3, 0, 100, 1, 0, 0, 96}
	sess.DeliverEncryptedControl(&protocol.EncryptedControl{
		ControlType: protocol.EncryptedControlType_EncryptedControlHandshake,
		EpochId:     maxEpochId.Bytes(), Payload: clientHello,
	})
	poisoned := sess.currentEpoch()
	if poisoned == nil || sess.epochIdOf(poisoned) != maxEpochId {
		t.Fatal("test did not install the attacker's handshake candidate")
	}
	sess.completeHandshake(poisoned, errors.New("synthetic handshake timeout"))
	legitEpochId := Id{15: 1}
	sess.DeliverEncryptedControl(&protocol.EncryptedControl{
		ControlType: protocol.EncryptedControlType_EncryptedControlHandshake,
		EpochId:     legitEpochId.Bytes(), Payload: clientHello,
	})
	if e := sess.currentEpoch(); e == poisoned || sess.epochIdOf(e) != legitEpochId || isClosed(e.handshakeDone) {
		t.Fatal("failed unauthenticated ClientHello left a permanent epoch lower bound")
	}
}

// With no timeout worker, even a live candidate cannot impose an ordering
// preference indefinitely. A different ClientHello must be able to replace it.
func TestServerEpochOrderingCannotPinWithoutTimeout(t *testing.T) {
	for _, timeout := range []time.Duration{0, -time.Second} {
		func() {
			sess, cleanup := newTestEncryptionSession(t, sequenceTlsRoleServer)
			defer cleanup()
			sess.settings.TlsTimeout = timeout
			older, newer := olderAndNewerIds(t)
			e := injectTestEpochWithId(sess, false, nil, newer)
			defer e.cancel()
			sess.deliverHandshake([]byte{22, 3, 3, 0, 100, 1, 0, 0, 96}, older)
			if current := sess.currentEpoch(); current == e || sess.epochIdOf(current) != older || isClosed(current.handshakeDone) {
				t.Fatalf("timeout %s left an unbounded epoch ordering preference", timeout)
			}
		}()
	}
}

// Completion, identity failure, and cancellation all end the temporary
// ordering preference. Even a verified TLS exporter does not sign epoch_id.
func TestServerLowerEpochRecoversAfterAttemptEnds(t *testing.T) {
	for _, state := range []string{"tls complete", "identity failed", "canceled", "established"} {
		func() {
			sess, cleanup := newTestEncryptionSession(t, sequenceTlsRoleServer)
			defer cleanup()
			sess.settings.TlsTimeout = 0
			older, newer := olderAndNewerIds(t)
			e := injectTestEpochWithId(sess, state == "tls complete" || state == "established", nil, newer)
			defer e.cancel()
			sess.stateLock.Lock()
			e.identityFailed = state == "identity failed"
			if state == "established" {
				e.peerIdentityVerified = true
				e.derivedTlsCipher = &sequenceCipher{}
				sess.establishedEpoch = e
			}
			sess.stateLock.Unlock()
			if state == "canceled" {
				e.cancel()
			}
			sess.deliverHandshake([]byte{22, 3, 3, 0, 100, 1, 0, 0, 96}, older)
			if current := sess.currentEpoch(); current == e || sess.epochIdOf(current) != older || isClosed(current.handshakeDone) {
				t.Fatalf("%s epoch blocked the lower ClientHello", state)
			}
			if state == "established" && sess.Cipher() != e.derivedTlsCipher {
				t.Fatal("starting a lower generation discarded the established cipher")
			}
		}()
	}
}

// A server can select a different generation only from a ClientHello, never
// from a foreign second flight, alert, or arbitrary bytes.
func TestServerForeignNonHelloCannotChooseEpoch(t *testing.T) {
	for _, activeId := range []Id{{}, {15: 1}} {
		for _, payload := range [][]byte{{23, 3, 3, 0, 1, 0}, {21, 3, 3, 0, 1, 0}, {0}} {
			func() {
				sess, cleanup := newTestEncryptionSession(t, sequenceTlsRoleServer)
				defer cleanup()
				e := injectTestEpochWithId(sess, false, nil, activeId)
				defer e.cancel()
				e.transport = newSequenceTlsTransport(e.ctx)
				sess.DeliverEncryptedControl(&protocol.EncryptedControl{
					ControlType: protocol.EncryptedControlType_EncryptedControlHandshake,
					EpochId:     Id{15: 2}.Bytes(), Payload: payload,
				})
				if sess.currentEpoch() != e || sess.epochIdOf(e) != activeId || e.ctx.Err() != nil {
					t.Fatalf("foreign record %d changed server epoch %s", payload[0], activeId)
				}
				e.transport.inboxLock.Lock()
				inboxBytes := len(e.transport.inboxBuf)
				e.transport.inboxLock.Unlock()
				if inboxBytes != 0 {
					t.Fatal("foreign record reached the current TLS state")
				}
			}()
		}
	}
}

// The client owns its generation names. A claimed newer response cannot
// replace a live client handshake; a matching response still reaches TLS.
func TestClientForeignHandshakeCannotResetEpoch(t *testing.T) {
	sess, cleanup := newTestEncryptionSession(t, sequenceTlsRoleClient)
	defer cleanup()
	older, newer := olderAndNewerIds(t)
	e := injectTestEpochWithId(sess, false, nil, older)
	defer e.cancel()
	e.transport = newSequenceTlsTransport(e.ctx)
	payload := []byte{22, 3, 3, 0, 1, 2}
	sess.DeliverEncryptedControl(&protocol.EncryptedControl{
		ControlType: protocol.EncryptedControlType_EncryptedControlHandshake,
		EpochId:     newer.Bytes(), Payload: payload,
	})
	if sess.currentEpoch() != e || e.ctx.Err() != nil {
		t.Fatal("foreign response reset the client's own generation")
	}
	e.transport.inboxLock.Lock()
	inboxBytes := len(e.transport.inboxBuf)
	e.transport.inboxLock.Unlock()
	if inboxBytes != 0 {
		t.Fatal("foreign response reached the client TLS state")
	}
	sess.DeliverEncryptedControl(&protocol.EncryptedControl{
		ControlType: protocol.EncryptedControlType_EncryptedControlHandshake,
		EpochId:     older.Bytes(), Payload: payload,
	})
	e.transport.inboxLock.Lock()
	inboxBytes = len(e.transport.inboxBuf)
	e.transport.inboxLock.Unlock()
	if inboxBytes != len(payload) {
		t.Fatal("matching response did not reach the client TLS state")
	}
}

// Recovery must complete real TLS and identity verification, then deliver
// required-mode application data through the ordinary transfer path.
func TestServerEpochIdPoisoningRecoversEncryptedDelivery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a, b, aClientId, bClientId, _, receivesB := requiredGatePair(
		ctx, EncryptionModeRequired, EncryptionModeRequired, nil, true,
	)
	defer func() {
		cancel()
		for _, client := range []*Client{a, b} {
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Errorf("client cleanup: %v", err)
			}
		}
	}()
	sess := b.encryptionSessionManager.Acquire(aClientId, sequenceTlsRoleServer, false)
	defer sess.Release()
	var maxEpochId Id
	for i := range maxEpochId {
		maxEpochId[i] = 0xff
	}
	b.encryptionSessionManager.DeliverEncryptedControl(aClientId, sequenceTlsRoleServer, &protocol.EncryptedControl{
		ControlType: protocol.EncryptedControlType_EncryptedControlIdentityProof,
		SessionRole: sequenceTlsRoleClient.toProtobuf(),
		EpochId:     maxEpochId.Bytes(), Payload: []byte{0},
	})
	sess.completeHandshake(sess.currentEpoch(), errors.New("synthetic handshake timeout"))
	if !a.SendWithTimeout(requiredGateFrame(t, "recovered after epoch injection"), bClientId, func(error) {}, 20*time.Second) {
		t.Fatal("required-mode send did not establish after epoch injection")
	}
	select {
	case got := <-receivesB:
		if got != "recovered after epoch injection" {
			t.Fatalf("unexpected delivered data %q", got)
		}
	case <-ctx.Done():
		t.Fatal("encrypted delivery did not recover after epoch injection")
	}
	if sess.Cipher() == nil {
		t.Fatal("delivery recovered without establishing the responder's cipher")
	}
	clientSession := a.encryptionSessionManager.Lookup(bClientId, sequenceTlsRoleClient, false)
	if clientSession == nil || sess.epochIdOf(sess.currentEpoch()) != clientSession.epochIdOf(clientSession.currentEpoch()) {
		t.Fatal("the recovered peers did not agree on the legitimate epoch")
	}
}

// TestHandshakeForNewerEpochResets: the peer restarted; its ClientHello names a
// newer generation, so we reset onto it even though our epoch is live.
func TestHandshakeForNewerEpochResets(t *testing.T) {
	older, newer := olderAndNewerIds(t)
	clientHello := []byte{22, 3, 3, 0, 100, 1, 0, 0, 96}

	sess, cleanup := newTestEncryptionSession(t, sequenceTlsRoleServer)
	defer cleanup()
	e1 := injectTestEpochWithId(sess, false, nil, older)

	sess.deliverHandshake(clientHello, newer)

	if sess.currentEpoch() == e1 {
		t.Fatal("expected a newer-generation ClientHello to reset onto the peer's generation")
	}
}

// TestLegacyUnsetEpochKeepsPriorBehavior: a peer that predates epoch_id sends
// controls with no generation; those must behave exactly as before (the
// completed-server-handshake ClientHello reset).
func TestLegacyUnsetEpochKeepsPriorBehavior(t *testing.T) {
	clientHello := []byte{22, 3, 3, 0, 100, 1, 0, 0, 96}

	sess, cleanup := newTestEncryptionSession(t, sequenceTlsRoleServer)
	defer cleanup()
	e1 := injectTestEpochWithId(sess, true, nil, NewId())

	sess.deliverHandshake(clientHello, Id{})

	if sess.currentEpoch() == e1 {
		t.Fatal("expected legacy (unset generation) delivery to keep the prior reset behavior")
	}

	// and a legacy proof still routes to the current epoch's verification path
	e2 := injectTestEpochWithId(sess, true, errors.New("x"), NewId())
	sess.receivePeerIdentityProofForEpoch(make([]byte, 64), Id{})
	if sess.currentEpoch() != e2 {
		t.Fatal("expected a legacy proof to leave the epoch in place")
	}
}

// TestResponderAdoptsInitiatorEpoch: the TLS-server role mints no generation of
// its own — it adopts the initiator's from the inbound handshake, so both sides
// name the same epoch and the proof exchange matches.
func TestResponderAdoptsInitiatorEpoch(t *testing.T) {
	clientHello := []byte{22, 3, 3, 0, 100, 1, 0, 0, 96}
	initiator := NewId()

	sess, cleanup := newTestEncryptionSession(t, sequenceTlsRoleServer)
	defer cleanup()

	sess.deliverHandshake(clientHello, initiator)

	e := sess.currentEpoch()
	if e == nil {
		t.Fatal("expected an epoch after inbound handshake")
	}
	if got := sess.epochIdOf(e); got != initiator {
		t.Fatalf("responder epoch id = %s, want the initiator's %s", got, initiator)
	}

	// a proof for that same generation is now in-generation (no reset)
	sess.receivePeerIdentityProofForEpoch(make([]byte, 64), initiator)
	if sess.currentEpoch() != e {
		t.Fatal("expected an in-generation proof to keep the adopted epoch")
	}
}

// TestClientRoleMintsEpochId: the initiator names every generation it builds,
// so its controls always carry one.
func TestClientRoleMintsEpochId(t *testing.T) {
	sess, cleanup := newTestEncryptionSession(t, sequenceTlsRoleClient)
	defer cleanup()

	sess.reset()
	e := sess.currentEpoch()
	if e == nil {
		t.Fatal("expected an epoch after reset")
	}
	first := sess.epochIdOf(e)
	if first == (Id{}) {
		t.Fatal("client-role epoch must mint a generation id")
	}

	sess.reset()
	second := sess.epochIdOf(sess.currentEpoch())
	if second == (Id{}) || second == first {
		t.Fatalf("each generation needs a distinct id: %s then %s", first, second)
	}
	if !first.LessThan(second) {
		t.Fatalf("expected monotonic generations: %s then %s", first, second)
	}
}

// TestStaleEpochProofDoesNotConsumePendingSlot locks the invariant whose
// violation caused the new-instance stall: an epoch holds ONE pending
// identity-proof slot, and a proof from a foreign generation must never
// occupy it. If it does, the real proof is refused as "already buffered" and
// the stale one is later verified against this epoch's exporter, fails, and
// terminally tombstones a session the peer is still encrypting into.
//
// The regression arrived through the OPTIMISTIC delivery path (Client.receive
// applies EncryptedControl before the in-order drain), which delivered proofs
// without their generation. Both paths must carry it.
func TestStaleEpochProofDoesNotConsumePendingSlot(t *testing.T) {
	older, newer := olderAndNewerIds(t)

	sess, cleanup := newTestEncryptionSession(t, sequenceTlsRoleClient)
	defer cleanup()
	e := injectTestEpochWithId(sess, true, nil, newer)
	// an exporter exists, so a same-generation proof would be verifiable
	sess.stateLock.Lock()
	e.tlsExporter = make([]byte, 32)
	sess.stateLock.Unlock()

	// a proof from the superseded generation
	sess.receivePeerIdentityProofForEpoch(make([]byte, 64), older)

	sess.stateLock.Lock()
	pending := len(e.pendingPeerIdentityProof)
	failed := e.identityFailed
	sess.stateLock.Unlock()
	if pending != 0 {
		t.Fatal("a stale-generation proof must not occupy the pending slot")
	}
	if failed {
		t.Fatal("a stale-generation proof must not mark the epoch identity-failed")
	}

	// the slot stays available for this generation's proof
	sess.receivePeerIdentityProofForEpoch(make([]byte, 64), newer)
	sess.stateLock.Lock()
	pending = len(e.pendingPeerIdentityProof)
	sess.stateLock.Unlock()
	if pending == 0 {
		t.Fatal("the current generation's proof must be accepted into the pending slot")
	}
}

// TestOptimisticIdentityProofCarriesEpoch drives the OPTIMISTIC EncryptedControl
// path in `Client.receive` — the fast path that applies a control before the
// in-order ReceiveSequence drain — and asserts it honors the generation.
//
// This is the regression test for the delivery path that the epoch fix
// originally missed. The in-order path checked the generation, the optimistic
// path did not, and a stale proof arriving on the fast path took the epoch's
// single pending-proof slot: the real proof was then refused as "already
// buffered", the stale one was finally verified against this epoch's exporter,
// failed, and terminally tombstoned a session the peer was still encrypting
// into (see the WithNewInstance stall).
//
// It must fail if `Client.receive` stops passing the epoch: with no generation
// the stale proof is buffered, and `pendingPeerIdentityProof` becomes non-empty.
func TestOptimisticIdentityProofCarriesEpoch(t *testing.T) {
	older, newer := olderAndNewerIds(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	settings := DefaultClientSettings()
	settings.EncryptionSettings.Mode = EncryptionModeOpportunistic
	settings.EncryptionSettings.TlsTimeout = 5 * time.Second

	peerId := NewId()
	receiver := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	defer receiver.Cancel()

	toReceiver := make(chan []byte, 8)
	receiver.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{toReceiver})
	receiver.ContractManager().AddNoContractPeer(peerId)

	// the local session the peer's client-role control routes into: the peer
	// sends as client, so it drives our server session
	sess := receiver.EncryptionSessionManager().getOrCreate(peerId, sequenceTlsRoleServer, false)
	if sess == nil {
		t.Fatal("expected a per-peer session")
	}
	// hold a live generation with an exporter, so a same-generation proof
	// would be buffered for verification
	e := injectTestEpochWithId(sess, true, nil, newer)
	sess.stateLock.Lock()
	e.tlsExporter = make([]byte, 32)
	sess.stateLock.Unlock()

	// buildProofFrame renders a peer identity-proof control for `epochId` on
	// the wire exactly as the peer would send it: a plaintext pack carrying
	// one EncryptedControl.
	buildProofFrame := func(epochId Id) []byte {
		ec := &protocol.EncryptedControl{
			ControlType: protocol.EncryptedControlType_EncryptedControlIdentityProof,
			Payload:     make([]byte, 64),
			SessionRole: protocol.SequenceRole_SequenceRoleClient,
			Companion:   false,
			EpochId:     epochId.Bytes(),
		}
		ecBytes, err := proto.Marshal(ec)
		if err != nil {
			t.Fatal(err)
		}
		packBytes, err := proto.Marshal(&protocol.Pack{
			MessageId:      NewId().Bytes(),
			SequenceId:     NewId().Bytes(),
			SequenceNumber: 0,
			Head:           true,
			Nack:           true,
			Frames: []*protocol.Frame{{
				MessageType:  protocol.MessageType_TransferEncryptedControl,
				MessageBytes: ecBytes,
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		messageType := protocol.MessageType_TransferPack
		transferFrameBytes, err := proto.Marshal(&protocol.TransferFrame{
			TransferPath: TransferPath{
				SourceId:      peerId,
				DestinationId: receiver.ClientId(),
			}.ToProtobuf(),
			MessageType: &messageType,
			Frame: &protocol.Frame{
				MessageType:  protocol.MessageType_TransferPack,
				MessageBytes: packBytes,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return transferFrameBytes
	}

	deliver := func(transferFrameBytes []byte) {
		select {
		case toReceiver <- transferFrameBytes:
		case <-time.After(5 * time.Second):
			t.Fatal("route write timed out")
		}
	}

	pendingProof := func() (int, bool) {
		sess.stateLock.Lock()
		defer sess.stateLock.Unlock()
		return len(e.pendingPeerIdentityProof), e.identityFailed
	}

	// 1. the superseded generation's proof must never reach this epoch. With
	//    the generation dropped on this path it is buffered instead, taking the
	//    slot the real proof needs.
	deliver(buildProofFrame(older))
	staleDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(staleDeadline) {
		if pending, failed := pendingProof(); 0 < pending || failed {
			t.Fatalf(
				"stale-generation proof reached the epoch through the optimistic path (pending=%d failed=%t): the path is not carrying the generation",
				pending, failed,
			)
		}
		time.Sleep(25 * time.Millisecond)
	}

	// 2. positive control: this generation's proof MUST arrive, proving the
	//    frame shape and the optimistic path are live — without this a broken
	//    delivery would make step 1 pass vacuously.
	deliver(buildProofFrame(newer))
	liveDeadline := time.Now().Add(10 * time.Second)
	for {
		if pending, _ := pendingProof(); 0 < pending {
			break
		}
		if !time.Now().Before(liveDeadline) {
			t.Fatal("current-generation proof never reached the epoch: the optimistic path is not delivering, so this test proves nothing")
		}
		time.Sleep(25 * time.Millisecond)
	}
}
