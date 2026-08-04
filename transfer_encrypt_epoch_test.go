package connect

// Tests for epoch identity on EncryptedControl (see EncryptedControl.epoch_id).
//
// The generation on the wire exists so a control can be routed to the epoch
// that produced it. Without it, a proof bound to the peer's NEW exporter was
// judged against our OLD one, failed as a bad signature, and terminally
// tombstoned a session the peer was still encrypting into — a permanent stall
// (see repro-epochdesync). These cover the routing rules:
//   - a proof for a NEWER generation converges (re-handshake), never tombstones
//   - a proof for an OLDER generation is ignored, never tombstones
//   - handshake bytes follow the same ordering rule
//   - a legacy (unset) generation keeps the pre-epoch behavior
//   - the responder adopts the initiator's generation and echoes it

import (
	"errors"
	"testing"
	"time"
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
	older = NewId()
	// ulids are time-ordered; a later mint compares greater
	time.Sleep(2 * time.Millisecond)
	newer = NewId()
	if !older.LessThan(newer) {
		t.Fatalf("expected ulid ordering: %s < %s", older, newer)
	}
	return older, newer
}

// TestIdentityProofForNewerEpochConverges: the peer re-handshaked, so its proof
// names a newer generation. We must rebuild onto that generation — and must NOT
// mark this epoch identity-failed (the old behavior, which stalled the session
// permanently while the peer kept encrypting).
func TestIdentityProofForNewerEpochConverges(t *testing.T) {
	older, newer := olderAndNewerIds(t)

	sess, cleanup := newTestEncryptionSession(t, sequenceTlsRoleClient)
	defer cleanup()
	e1 := injectTestEpochWithId(sess, true, nil, older)

	sess.receivePeerIdentityProofForEpoch(make([]byte, 64), newer)

	if sess.currentEpoch() == e1 {
		t.Fatal("expected a newer-generation proof to converge onto a fresh epoch")
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

// TestHandshakeForOlderEpochDropped: straggling handshake bytes from a
// superseded generation must not be fed to the live epoch's TLS state.
func TestHandshakeForOlderEpochDropped(t *testing.T) {
	older, newer := olderAndNewerIds(t)
	clientHello := []byte{22, 3, 3, 0, 100, 1, 0, 0, 96}

	sess, cleanup := newTestEncryptionSession(t, sequenceTlsRoleServer)
	defer cleanup()
	e1 := injectTestEpochWithId(sess, true, nil, newer)

	sess.deliverHandshake(clientHello, older)

	if sess.currentEpoch() != e1 {
		t.Fatal("expected stale-generation handshake bytes to be dropped without reset")
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
