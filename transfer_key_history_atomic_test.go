// Forces history admission races at the snapshot/commit boundary. The checked
// methods keep these regressions executable against the pre-CAS resolver too.
package connect

import (
	"context"
	"crypto/ed25519"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Adds barriers and counters around a concurrent-safe store. Hooks are fixed
// before use and run outside its state lock; no test waits while holding it.
type testHistoryAdmissionStore struct {
	*testPeerClientKeyPinStore
	reads                  atomic.Int32
	compares               atomic.Int32
	legacyReadUsesHook     bool
	afterRead              func(int32, ClientKeyPinSnapshot)
	afterLegacyLatchRead   func()
	beforeCompare          func(int32)
	afterCompare           func(int32)
	compareErr             error
	refuseUnchangedCompare bool
}

// Creates one independently owned store, shared only by the test's sessions.
func newTestHistoryAdmissionStore() *testHistoryAdmissionStore {
	return &testHistoryAdmissionStore{
		testPeerClientKeyPinStore: newTestPeerClientKeyPinStore(),
		legacyReadUsesHook:        true,
	}
}

// Captures the snapshot before running a barrier, so other managers can commit.
func (self *testHistoryAdmissionStore) ReadPeerClientKeyPinSnapshot(peerId Id) (ClientKeyPinSnapshot, error) {
	snapshot, err := self.testPeerClientKeyPinStore.ReadPeerClientKeyPinSnapshot(peerId)
	count := self.reads.Add(1)
	if self.afterRead != nil {
		self.afterRead(count, snapshot)
	}
	return snapshot, err
}

// Routes the old resolver's read through the same barrier for semantic red runs.
func (self *testHistoryAdmissionStore) GetPeerClientKeyPinChecked(peerId Id) (ClientKeyPin, bool, error) {
	if !self.legacyReadUsesHook {
		return self.testPeerClientKeyPinStore.GetPeerClientKeyPinChecked(peerId)
	}
	snapshot, err := self.ReadPeerClientKeyPinSnapshot(peerId)
	return snapshot.Pin, snapshot.Pinned, err
}

// Captures the old resolver's separately read latch before its matching barrier.
func (self *testHistoryAdmissionStore) SignedHistorySeen() bool {
	seen := self.testPeerClientKeyPinStore.SignedHistorySeen()
	if self.afterLegacyLatchRead != nil {
		self.afterLegacyLatchRead()
	}
	return seen
}

// Mirrors the SDK's head-only checks for the old resolver's unconditional commit.
func (self *testHistoryAdmissionStore) CommitPeerClientKeyPin(peerId Id, pin ClientKeyPin) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if previous, pinned := self.pins[peerId]; pinned &&
		(pin.Generation < previous.Generation || pin.DomainDigest != previous.DomainDigest || pin.Signer != previous.Signer ||
			(pin.Generation == previous.Generation && pin.PublicKey != previous.PublicKey)) {
		return errors.New("pin rollback or conflict")
	}
	self.pins[peerId] = pin
	self.signedSeen = true
	self.setPinCalls++
	return nil
}

// Runs optional admission interleavings before the real atomic comparison.
func (self *testHistoryAdmissionStore) CompareAndSwapPeerClientKeyPin(peerId Id, previous ClientKeyPinSnapshot, next *ClientKeyPin) (bool, error) {
	count := self.compares.Add(1)
	if self.beforeCompare != nil {
		self.beforeCompare(count)
	}
	if self.compareErr != nil {
		return false, self.compareErr
	}
	if self.refuseUnchangedCompare {
		return false, nil
	}
	admitted, err := self.testPeerClientKeyPinStore.CompareAndSwapPeerClientKeyPin(peerId, previous, next)
	if self.afterCompare != nil {
		self.afterCompare(count)
	}
	return admitted, err
}

// Waits for a positive barrier or worker completion; timeout is only a hang guard.
func awaitHistoryAdmissionTest(t *testing.T, ready <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatalf("history admission did not reach %s", name)
	}
}

// Executes the real SetPeerClientPublicKey -> fetch -> resolve path. The later
// session snapshots generation one, the first pins generation two, then the
// later session resumes. Both shared-manager and shared-store owners are tested.
func testConcurrentHistoryAdmission(t *testing.T, separateManagers bool, laterKeys [][32]byte, want clientKeyHistoryState) {
	t.Helper()
	signer := newTestClientKeySigner(t, goldenSignerPrivateKeyHex)
	policy := testPolicyFor(t, signer)
	peerId := goldenClientKeyId()
	accepted := buildTestChain(t, signer, peerId, [][32]byte{testClientKeyPublicKey(0x61), testClientKeyPublicKey(0x62)})
	later := buildTestChain(t, signer, peerId, laterKeys)
	_, pin, err := VerifyClientKeyHistory(peerId, encodeTestChain(t, accepted[:1]), policy)
	if err != nil {
		t.Fatal(err)
	}
	store := newTestHistoryAdmissionStore()
	if err := store.CommitPeerClientKeyPin(peerId, pin); err != nil {
		t.Fatal(err)
	}
	read := make(chan struct{})
	resume := make(chan struct{})
	release := sync.OnceFunc(func() { close(resume) })
	defer release()
	store.afterRead = func(count int32, _ ClientKeyPinSnapshot) {
		if count == 1 {
			close(read)
			<-resume
		}
	}
	acceptedBytes := encodeTestChain(t, accepted)
	laterBytes := encodeTestChain(t, later)
	first, doneFirst := newTestKeyHistorySession(t, store, func(context.Context) ([][]byte, error) { return acceptedBytes, nil })
	defer doneFirst()
	first.settings.TrustedClientKeySigners = policy.TrustedSigners
	var second *peerEncryptionSession
	if separateManagers {
		var doneSecond func()
		second, doneSecond = newTestKeyHistorySession(t, store, func(context.Context) ([][]byte, error) { return laterBytes, nil })
		defer doneSecond()
		second.settings.TrustedClientKeySigners = policy.TrustedSigners
	} else {
		second = newPeerEncryptionSession(first.ctx, first.manager, first.client, peerId,
			sequenceTlsRoleServer, first.settings, first.manager.ServerTlsConfig(), true)
		defer second.close()
		second.peerClientKeyHistoryFetcher = func(context.Context) ([][]byte, error) { return laterBytes, nil }
	}
	firstDone, secondDone := make(chan struct{}), make(chan struct{})
	first.beforeWorkerDoneForTest = func(name string) {
		if name == "signed identity resolve" {
			close(firstDone)
		}
	}
	second.beforeWorkerDoneForTest = func(name string) {
		if name == "signed identity resolve" {
			close(secondDone)
		}
	}
	second.SetPeerClientPublicKey(later[len(later)-1].ClientKeyRegistrationPublicKey())
	awaitHistoryAdmissionTest(t, read, "old snapshot")
	first.SetPeerClientPublicKey(accepted[1].ClientKeyRegistrationPublicKey())
	awaitHistoryAdmissionTest(t, firstDone, "new pin commit")
	release()
	awaitHistoryAdmissionTest(t, secondDone, "conflict resolution")
	if keyHistoryState(first) != clientKeyHistoryVerified || keyHistoryState(second) != want {
		t.Fatalf("separate managers=%t: first=%s later=%s, want verified/%s", separateManagers, keyHistoryState(first), keyHistoryState(second), want)
	}
	retained, pinned := store.testPeerClientKeyPinStore.GetPeerClientKeyPin(peerId)
	wantHead := accepted[1]
	if want == clientKeyHistoryVerified {
		wantHead = later[len(later)-1]
	}
	if !pinned || retained.Generation != wantHead.Generation || retained.PublicKey != wantHead.PublicKey {
		t.Fatalf("separate managers=%t: admitted the wrong durable head: %+v", separateManagers, retained)
	}
	if store.compares.Load() != 3 {
		t.Fatalf("separate managers=%t: compares=%d, want first admission plus conflict and retry", separateManagers, store.compares.Load())
	}
}

// An advancing signed fork must not replace a pin installed by another session.
func TestSignedIdentityAtomicAdmissionRejectsConcurrentFork(t *testing.T) {
	for _, separateManagers := range []bool{false, true} {
		testConcurrentHistoryAdmission(t, separateManagers,
			[][32]byte{testClientKeyPublicKey(0x61), testClientKeyPublicKey(0x69), testClientKeyPublicKey(0x70)}, clientKeyHistoryRejected)
	}
}

// A genuine extension retries against the new pin and remains available.
func TestSignedIdentityAtomicAdmissionRetriesConcurrentExtension(t *testing.T) {
	for _, separateManagers := range []bool{false, true} {
		testConcurrentHistoryAdmission(t, separateManagers,
			[][32]byte{testClientKeyPublicKey(0x61), testClientKeyPublicKey(0x62), testClientKeyPublicKey(0x63)}, clientKeyHistoryVerified)
	}
}

// Equal histories can converge without a false fork or local-store failure.
func TestSignedIdentityAtomicAdmissionKeepsIdenticalHeadIdempotent(t *testing.T) {
	for _, separateManagers := range []bool{false, true} {
		testConcurrentHistoryAdmission(t, separateManagers,
			[][32]byte{testClientKeyPublicKey(0x61), testClientKeyPublicKey(0x62)}, clientKeyHistoryVerified)
	}
}

// Equal-generation conflicts are peer evidence, not a late persistence failure.
func TestSignedIdentityAtomicAdmissionRejectsConcurrentSameGenerationFork(t *testing.T) {
	for _, separateManagers := range []bool{false, true} {
		testConcurrentHistoryAdmission(t, separateManagers,
			[][32]byte{testClientKeyPublicKey(0x61), testClientKeyPublicKey(0x69)}, clientKeyHistoryRejected)
	}
}

// A read-only fallback must re-evaluate both stale acceptance and stale rejection.
func TestSignedIdentityAtomicFetchErrorRechecksChangedPin(t *testing.T) {
	for _, contractMatchesNewPin := range []bool{false, true} {
		func() {
			store := newTestHistoryAdmissionStore()
			peerId := goldenClientKeyId()
			oldPin := ClientKeyPin{Generation: 1, PublicKey: testClientKeyPublicKey(0x61)}
			newPin := ClientKeyPin{Generation: 2, PublicKey: testClientKeyPublicKey(0x62)}
			if err := store.CommitPeerClientKeyPin(peerId, oldPin); err != nil {
				t.Fatal(err)
			}
			contractPub := ed25519.PublicKey(oldPin.PublicKey[:])
			want := clientKeyHistoryRejected
			if contractMatchesNewPin {
				contractPub = ed25519.PublicKey(newPin.PublicKey[:])
				want = clientKeyHistoryVerified
			}
			read, resume, completed := make(chan struct{}), make(chan struct{}), make(chan struct{})
			release := sync.OnceFunc(func() { close(resume) })
			defer release()
			store.afterRead = func(count int32, _ ClientKeyPinSnapshot) {
				if count == 1 {
					close(read)
					<-resume
				}
			}
			session, done := newTestKeyHistorySession(t, store, nil)
			defer done()
			armKeyHistoryGate(session)
			go func() {
				defer close(completed)
				session.applyPeerClientKeyHistoryFetchError(contractPub, errors.New("evidence unavailable"))
			}()
			awaitHistoryAdmissionTest(t, read, "fallback snapshot")
			if err := store.CommitPeerClientKeyPin(peerId, newPin); err != nil {
				t.Fatal(err)
			}
			release()
			awaitHistoryAdmissionTest(t, completed, "fallback resolution")
			if got := keyHistoryState(session); got != want {
				t.Fatalf("contract matches new pin=%t: state=%s want=%s", contractMatchesNewPin, got, want)
			}
			if store.compares.Load() != 2 || store.setPinCalls != 2 {
				t.Fatal("fallback did not retry read-only admission or wrote a pin")
			}
		}()
	}
}

// An empty answer cannot win after another peer turns on the global ratchet.
// The compatibility hook pauses the old resolver after its separate latch read.
func TestSignedIdentityAtomicEmptyHistoryRechecksLatch(t *testing.T) {
	for _, samePeer := range []bool{false, true} {
		func() {
			store := newTestHistoryAdmissionStore()
			store.legacyReadUsesHook = false
			read, resume, completed := make(chan struct{}), make(chan struct{}), make(chan struct{})
			release := sync.OnceFunc(func() { close(resume) })
			defer release()
			var paused atomic.Bool
			pause := func() {
				if paused.CompareAndSwap(false, true) {
					close(read)
					<-resume
				}
			}
			store.afterRead = func(_ int32, _ ClientKeyPinSnapshot) { pause() }
			store.afterLegacyLatchRead = pause
			session, done := newTestKeyHistorySession(t, store, nil)
			defer done()
			armKeyHistoryGate(session)
			go func() {
				defer close(completed)
				session.applyPeerClientKeyHistory(goldenHeadPublicKey(t), nil)
			}()
			awaitHistoryAdmissionTest(t, read, "unsigned decision snapshot")
			peerId := NewId()
			if samePeer {
				peerId = session.peerId
			}
			if err := store.CommitPeerClientKeyPin(peerId, ClientKeyPin{Generation: 1, PublicKey: testClientKeyPublicKey(0x61)}); err != nil {
				t.Fatal(err)
			}
			release()
			awaitHistoryAdmissionTest(t, completed, "unsigned decision retry")
			if keyHistoryState(session) != clientKeyHistoryRejected || store.compares.Load() != 2 || store.setPinCalls != 1 {
				t.Fatalf("same peer=%t: stale unsigned decision survived the ratchet or wrote a pin", samePeer)
			}
		}()
	}
}

// Read-only admissions must report storage failure just like durable commits.
func TestSignedIdentityAtomicReadOnlyAdmissionFailsClosed(t *testing.T) {
	for _, fetchError := range []bool{false, true} {
		func() {
			store := newTestHistoryAdmissionStore()
			store.compareErr = errors.New("store unavailable during admission")
			session, done := newTestKeyHistorySession(t, store, nil)
			defer done()
			armKeyHistoryGate(session)
			if fetchError {
				session.applyPeerClientKeyHistoryFetchError(goldenHeadPublicKey(t), errors.New("network unavailable"))
			} else {
				session.applyPeerClientKeyHistory(goldenHeadPublicKey(t), nil)
			}
			if keyHistoryState(session) != clientKeyHistoryStoreUnavailable || session.KeyIdentityRejected() || store.setPinCalls != 0 {
				t.Fatalf("fetch error=%t: local admission failure opened the gate, blamed the peer, or changed pins", fetchError)
			}
		}()
	}
}

// A custom store cannot force an unbounded verification loop by changing pins.
func TestSignedIdentityAtomicAdmissionBoundsConflicts(t *testing.T) {
	store := newTestHistoryAdmissionStore()
	session, done := newTestKeyHistorySession(t, store, nil)
	defer done()
	session.settings.MaxClientKeyPinAdmissionAttempts = 3
	pub := goldenHeadPublicKey(t)
	store.beforeCompare = func(count int32) {
		pin := ClientKeyPin{Generation: uint64(count)}
		copy(pin.PublicKey[:], pub)
		_ = store.CommitPeerClientKeyPin(session.peerId, pin)
	}
	armKeyHistoryGate(session)
	session.applyPeerClientKeyHistoryFetchError(pub, errors.New("network unavailable"))
	if keyHistoryState(session) != clientKeyHistoryStoreUnavailable || session.KeyIdentityRejected() || store.compares.Load() != 3 || store.reads.Load() != 3 {
		t.Fatalf("unbounded or incorrect conflict outcome: state=%s reads=%d compares=%d", keyHistoryState(session), store.reads.Load(), store.compares.Load())
	}
}

// A false comparison without state progress is a broken store, not a spin loop.
func TestSignedIdentityAtomicAdmissionRejectsUnchangedConflict(t *testing.T) {
	store := newTestHistoryAdmissionStore()
	store.refuseUnchangedCompare = true
	session, done := newTestKeyHistorySession(t, store, nil)
	defer done()
	armKeyHistoryGate(session)
	session.applyPeerClientKeyHistoryFetchError(goldenHeadPublicKey(t), errors.New("network unavailable"))
	if keyHistoryState(session) != clientKeyHistoryStoreUnavailable || store.compares.Load() != 1 || store.reads.Load() != 2 {
		t.Fatal("unchanged compare failure retried without progress or opened admission")
	}
}

// Cancellation before reads, after snapshots, after comparison, and on conflict
// cannot open a session or schedule another admission attempt.
func TestSignedIdentityAtomicAdmissionHonorsCancellation(t *testing.T) {
	for _, phase := range []string{"before", "snapshot", "admitted", "conflict"} {
		func() {
			store := newTestHistoryAdmissionStore()
			session, done := newTestKeyHistorySession(t, store, nil)
			defer done()
			armKeyHistoryGate(session)
			switch phase {
			case "before":
				session.cancel()
			case "snapshot":
				store.afterRead = func(int32, ClientKeyPinSnapshot) { session.cancel() }
			case "admitted":
				store.afterCompare = func(int32) { session.cancel() }
			case "conflict":
				store.beforeCompare = func(int32) {
					store.SetSignedHistorySeen()
					session.cancel()
				}
			}
			session.applyPeerClientKeyHistoryFetchError(goldenHeadPublicKey(t), errors.New("network unavailable"))
			if keyHistoryState(session) != clientKeyHistoryPending || store.reads.Load() > 1 || store.compares.Load() > 1 {
				t.Fatalf("phase=%s: cancelled admission changed state or retried", phase)
			}
			if phase == "before" && store.reads.Load() != 0 || phase == "snapshot" && store.compares.Load() != 0 {
				t.Fatalf("phase=%s: admission ran after cancellation", phase)
			}
		}()
	}
}

// Nonpositive configured-store attempts refuse without touching store state.
func TestSignedIdentityAtomicAdmissionRequiresPositiveAttemptBound(t *testing.T) {
	for _, attempts := range []int{0, -1} {
		func() {
			store := newTestHistoryAdmissionStore()
			session, done := newTestKeyHistorySession(t, store, nil)
			defer done()
			session.settings.MaxClientKeyPinAdmissionAttempts = attempts
			armKeyHistoryGate(session)
			session.applyPeerClientKeyHistory(goldenHeadPublicKey(t), goldenClientKeyHistory())
			if keyHistoryState(session) != clientKeyHistoryStoreUnavailable || store.reads.Load() != 0 || store.compares.Load() != 0 {
				t.Fatalf("attempts=%d: unbounded store admission was allowed", attempts)
			}
		}()
	}
}

// Existing store interfaces keep compiling, but cannot silently weaken admission.
func TestSignedIdentityAtomicAdmissionRefusesLegacyStore(t *testing.T) {
	for _, checked := range []bool{false, true} {
		for _, evidence := range []string{"signed", "empty", "error"} {
			func() {
				underlying := newTestHistoryAdmissionStore()
				var store PeerClientKeyPinStore = &struct{ PeerClientKeyPinStore }{PeerClientKeyPinStore: underlying}
				if checked {
					store = &struct{ CheckedPeerClientKeyPinStore }{CheckedPeerClientKeyPinStore: underlying}
				}
				session, done := newTestKeyHistorySession(t, store, nil)
				defer done()
				armKeyHistoryGate(session)
				switch evidence {
				case "signed":
					session.applyPeerClientKeyHistory(goldenHeadPublicKey(t), goldenClientKeyHistory())
				case "empty":
					session.applyPeerClientKeyHistory(goldenHeadPublicKey(t), nil)
				case "error":
					session.applyPeerClientKeyHistoryFetchError(goldenHeadPublicKey(t), errors.New("network unavailable"))
				}
				if keyHistoryState(session) != clientKeyHistoryStoreUnavailable || session.KeyIdentityRejected() || underlying.reads.Load() != 0 {
					t.Fatalf("checked=%t evidence=%s: legacy store admitted or accused peer", checked, evidence)
				}
			}()
		}
	}
}

// Deliberately stateless owners keep the documented signed/unsigned/error policy.
func TestSignedIdentityAtomicAdmissionPreservesNilStorePolicy(t *testing.T) {
	for _, evidence := range []string{"signed", "empty", "error", "invalid"} {
		func() {
			session, done := newTestKeyHistorySession(t, nil, nil)
			defer done()
			session.settings.MaxClientKeyPinAdmissionAttempts = 0
			armKeyHistoryGate(session)
			want := clientKeyHistoryVerified
			switch evidence {
			case "signed":
				session.applyPeerClientKeyHistory(goldenHeadPublicKey(t), goldenClientKeyHistory())
			case "empty":
				session.applyPeerClientKeyHistory(goldenHeadPublicKey(t), nil)
			case "error":
				session.applyPeerClientKeyHistoryFetchError(goldenHeadPublicKey(t), errors.New("network unavailable"))
			case "invalid":
				want = clientKeyHistoryRejected
				session.applyPeerClientKeyHistory(goldenHeadPublicKey(t), [][]byte{[]byte("{}")})
			}
			if got := keyHistoryState(session); got != want {
				t.Fatalf("evidence=%s: state=%s want=%s", evidence, got, want)
			}
		}()
	}
}
