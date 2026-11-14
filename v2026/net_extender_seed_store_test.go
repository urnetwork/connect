package connect

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Seed state tests (EXTENDER-UPDATE2.md §2.2, §2.4): replay, rollback,
// conflicts, interrupted persistence and restart, and root rollover and
// removal.

// A byte backend that counts saves and can fail either way.
type testSeedBackend struct {
	stateLock sync.Mutex
	value     []byte
	loadErr   error
	saveErr   error
	saveCount int
}

func (self *testSeedBackend) Load() ([]byte, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.loadErr != nil {
		return nil, self.loadErr
	}
	return append([]byte(nil), self.value...), nil
}

func (self *testSeedBackend) Save(value []byte) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.saveErr != nil {
		return self.saveErr
	}
	self.value = append([]byte(nil), value...)
	self.saveCount += 1
	return nil
}

func (self *testSeedBackend) snapshot() ([]byte, int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]byte(nil), self.value...), self.saveCount
}

// A verified bundle of `sequence` signed by `rootPrivateKey`, whose records it
// also signed. `n` picks the record keys, so two bundles at one sequence can
// differ.
func testSeedBundle(
	t *testing.T,
	rootPrivateKey ed25519.PrivateKey,
	rootKeys *ExtenderRootKeySet,
	sequence uint64,
	n byte,
	now time.Time,
) *ExtenderSeedBundle {
	t.Helper()
	records := []*protocol.ExtenderRecord{
		testSeedRecord(t, rootPrivateKey, n, now, fmt.Sprintf("203.0.113.%d", n)),
	}
	bundle, err := VerifyExtenderSeed(
		testSeedEnvelope(t, rootPrivateKey, sequence, now, records, nil),
		rootKeys,
		[]string{testExtenderNetworkHost},
		now,
	)
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

func requireExtenderSeedAccept(t *testing.T, acceptor *ExtenderSeedAcceptor, bundle *ExtenderSeedBundle, rootKeys *ExtenderRootKeySet, outcome string) {
	t.Helper()
	got, err := acceptor.Accept(bundle, rootKeys)
	if err != nil {
		t.Fatalf("accept of sequence %d: %v", bundle.Sequence, err)
	}
	if got != outcome {
		t.Fatalf("accept of sequence %d = %q, expected %q", bundle.Sequence, got, outcome)
	}
}

func requireExtenderSeedSequence(t *testing.T, acceptor *ExtenderSeedAcceptor, rootKeys *ExtenderRootKeySet, sequence uint64) {
	t.Helper()
	got, err := acceptor.Sequence(testExtenderNetworkHost, rootKeys)
	if err != nil {
		t.Fatal(err)
	}
	if got != sequence {
		t.Fatalf("floor = %d, expected %d", got, sequence)
	}
}

// A higher sequence is accepted, the same bundle again is idempotent, and a
// rollback or a different body at the accepted sequence is refused. Nothing is
// written for an idempotent accept or a refusal, so the last valid state stays
// byte for byte.
func TestExtenderSeedAcceptorSequenceRules(t *testing.T) {
	clock := newTestClock()
	now := clock.Now()
	rootKeys := testSeedRootKeys(testSeedRootKey)
	backend := &testSeedBackend{}
	acceptor := NewExtenderSeedAcceptor(backend, nil)

	requireExtenderSeedAccept(t, acceptor, testSeedBundle(t, testSeedRootKey, rootKeys, 5, 1, now), rootKeys, ExtenderSeedOutcomeAccepted)
	value, saveCount := backend.snapshot()
	if saveCount != 1 {
		t.Fatalf("saves = %d, expected 1", saveCount)
	}

	requireExtenderSeedAccept(t, acceptor, testSeedBundle(t, testSeedRootKey, rootKeys, 5, 1, now), rootKeys, ExtenderSeedOutcomeIdentical)
	_, err := acceptor.Accept(testSeedBundle(t, testSeedRootKey, rootKeys, 4, 1, now), rootKeys)
	requireExtenderSeedCode(t, err, ExtenderSeedCodeRollback)
	_, err = acceptor.Accept(testSeedBundle(t, testSeedRootKey, rootKeys, 5, 2, now), rootKeys)
	requireExtenderSeedCode(t, err, ExtenderSeedCodeConflict)
	if afterValue, afterSaveCount := backend.snapshot(); afterSaveCount != 1 || !bytes.Equal(afterValue, value) {
		t.Fatalf("an idempotent accept or a refusal wrote the state (saves %d)", afterSaveCount)
	}
	requireExtenderSeedSequence(t, acceptor, rootKeys, 5)

	requireExtenderSeedAccept(t, acceptor, testSeedBundle(t, testSeedRootKey, rootKeys, 6, 2, now), rootKeys, ExtenderSeedOutcomeAccepted)
	requireExtenderSeedSequence(t, acceptor, rootKeys, 6)
	stored, err := acceptor.Stored(testExtenderNetworkHost, rootKeys, []string{testExtenderNetworkHost}, now)
	if err != nil || stored == nil || stored.Sequence != 6 {
		t.Fatalf("stored = %v, %v; expected sequence 6", stored, err)
	}
}

// With no history, the minimum sequence the application configures is the
// floor (§2.3).
func TestExtenderSeedAcceptorMinSequence(t *testing.T) {
	clock := newTestClock()
	now := clock.Now()
	rootKeys := testSeedRootKeys(testSeedRootKey)
	acceptor := NewExtenderSeedAcceptor(NewExtenderSeedMemoryStore(), &ExtenderSeedAcceptorSettings{MinSequence: 10})
	_, err := acceptor.Accept(testSeedBundle(t, testSeedRootKey, rootKeys, 9, 1, now), rootKeys)
	requireExtenderSeedCode(t, err, ExtenderSeedCodeRollback)
	requireExtenderSeedSequence(t, acceptor, rootKeys, 0)
	requireExtenderSeedAccept(t, acceptor, testSeedBundle(t, testSeedRootKey, rootKeys, 10, 1, now), rootKeys, ExtenderSeedOutcomeAccepted)
}

// A commit interrupted before its rename leaves the previous state whole --
// its sequence with its bundle -- and a restart reads that state; the
// temporary file the interruption left is never read and is removed by the
// next commit.
func TestExtenderSeedFileStoreInterruptedCommit(t *testing.T) {
	clock := newTestClock()
	now := clock.Now()
	rootKeys := testSeedRootKeys(testSeedRootKey)
	path := filepath.Join(t.TempDir(), "seed", "extender-seed.json")

	fileStore := NewExtenderSeedFileStore(path)
	acceptor := NewExtenderSeedAcceptor(fileStore, nil)
	requireExtenderSeedAccept(t, acceptor, testSeedBundle(t, testSeedRootKey, rootKeys, 5, 1, now), rootKeys, ExtenderSeedOutcomeAccepted)

	crash := errors.New("the process ended before the rename")
	fileStore.beforeRenameHook = func() error { return crash }
	_, err := acceptor.Accept(testSeedBundle(t, testSeedRootKey, rootKeys, 6, 2, now), rootKeys)
	requireExtenderSeedCode(t, err, ExtenderSeedCodeStore)
	if !errors.Is(err, crash) {
		t.Fatalf("the store refusal does not carry its cause: %v", err)
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".*.tmp"))
	if len(leftovers) != 1 {
		t.Fatalf("temporary files = %d, expected the 1 the interruption left", len(leftovers))
	}

	// the restart: a new store and acceptor over the same file
	restarted := NewExtenderSeedAcceptor(NewExtenderSeedFileStore(path), nil)
	requireExtenderSeedSequence(t, restarted, rootKeys, 5)
	stored, err := restarted.Stored(testExtenderNetworkHost, rootKeys, []string{testExtenderNetworkHost}, now)
	if err != nil || stored == nil || stored.Sequence != 5 {
		t.Fatalf("stored after the restart = %v, %v; expected the bundle of sequence 5", stored, err)
	}
	if !bytes.Equal(stored.BodyDigest, testSeedBundle(t, testSeedRootKey, rootKeys, 5, 1, now).BodyDigest) {
		t.Fatal("the restart kept a sequence without its bundle")
	}

	requireExtenderSeedAccept(t, restarted, testSeedBundle(t, testSeedRootKey, rootKeys, 6, 2, now), rootKeys, ExtenderSeedOutcomeAccepted)
	leftovers, _ = filepath.Glob(filepath.Join(filepath.Dir(path), ".*.tmp"))
	if len(leftovers) != 0 {
		t.Fatalf("temporary files = %d after a commit, expected none", len(leftovers))
	}
	again := NewExtenderSeedAcceptor(NewExtenderSeedFileStore(path), nil)
	requireExtenderSeedSequence(t, again, rootKeys, 6)

	// a missing file is no history
	empty := NewExtenderSeedAcceptor(NewExtenderSeedFileStore(filepath.Join(t.TempDir(), "none.json")), nil)
	requireExtenderSeedSequence(t, empty, rootKeys, 0)
}

// A backend that cannot load fails closed: the floor is not taken to be empty
// and nothing is written over it. A document that loads but does not decode is
// no history, which is what deleting it would give.
func TestExtenderSeedAcceptorUnreadableState(t *testing.T) {
	clock := newTestClock()
	now := clock.Now()
	rootKeys := testSeedRootKeys(testSeedRootKey)

	failing := &testSeedBackend{loadErr: errors.New("io error")}
	_, err := NewExtenderSeedAcceptor(failing, nil).Accept(testSeedBundle(t, testSeedRootKey, rootKeys, 1, 1, now), rootKeys)
	requireExtenderSeedCode(t, err, ExtenderSeedCodeStore)
	if _, saveCount := failing.snapshot(); saveCount != 0 {
		t.Fatal("a state that did not load was written over")
	}

	corrupt := &testSeedBackend{value: []byte("{not json")}
	requireExtenderSeedAccept(t, NewExtenderSeedAcceptor(corrupt, nil), testSeedBundle(t, testSeedRootKey, rootKeys, 1, 1, now), rootKeys, ExtenderSeedOutcomeAccepted)

	saving := &testSeedBackend{saveErr: errors.New("disk full")}
	savingAcceptor := NewExtenderSeedAcceptor(saving, nil)
	_, err = savingAcceptor.Accept(testSeedBundle(t, testSeedRootKey, rootKeys, 1, 1, now), rootKeys)
	requireExtenderSeedCode(t, err, ExtenderSeedCodeStore)
	saving.saveErr = nil
	requireExtenderSeedSequence(t, savingAcceptor, rootKeys, 0)
}

// One entry of the document that is not one consistent value -- its sequence
// or digest not its bundle's -- is dropped alone; the others keep their
// floors.
func TestExtenderSeedAcceptorDropsAnInconsistentEntry(t *testing.T) {
	clock := newTestClock()
	now := clock.Now()
	rootKeys := testSeedRootKeys(testSeedRootKey)
	bundle := testSeedBundle(t, testSeedRootKey, rootKeys, 5, 1, now)
	envelopeBytes, err := MarshalExtenderSeedEnvelope(bundle.Envelope)
	if err != nil {
		t.Fatal(err)
	}
	documentBytes, err := encodeExtenderSeedStoreDocument(map[string]*ExtenderSeedState{
		testExtenderNetworkHost: {
			NetworkHost: testExtenderNetworkHost,
			// a sequence its bundle does not carry
			Sequence:   500,
			BodyDigest: bundle.BodyDigest,
			Envelope:   envelopeBytes,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	acceptor := NewExtenderSeedAcceptor(&testSeedBackend{value: documentBytes}, nil)
	requireExtenderSeedSequence(t, acceptor, rootKeys, 0)
	requireExtenderSeedAccept(t, acceptor, bundle, rootKeys, ExtenderSeedOutcomeAccepted)
}

// A bundle is accepted as the value its envelope carries: a hand-built bundle
// whose fields disagree with its envelope, or one signed by a key outside the
// keys it is accepted under, commits nothing.
func TestExtenderSeedAcceptorRefusesAnInconsistentBundle(t *testing.T) {
	clock := newTestClock()
	now := clock.Now()
	rootKeys := testSeedRootKeys(testSeedRootKey)
	acceptor := NewExtenderSeedAcceptor(NewExtenderSeedMemoryStore(), nil)

	inflated := *testSeedBundle(t, testSeedRootKey, rootKeys, 5, 1, now)
	inflated.Sequence = 1 << 40
	_, err := acceptor.Accept(&inflated, rootKeys)
	requireExtenderSeedCode(t, err, ExtenderSeedCodeMalformed)

	_, err = acceptor.Accept(testSeedBundle(t, testSeedRootKey, rootKeys, 5, 1, now), testSeedRootKeys(testSeedRotatedRootKey))
	requireExtenderSeedCode(t, err, ExtenderSeedCodeSignature)
	_, err = acceptor.Accept(testSeedBundle(t, testSeedRootKey, rootKeys, 5, 1, now), NewExtenderRootKeySet())
	requireExtenderSeedCode(t, err, ExtenderSeedCodeNoRootKeys)
	requireExtenderSeedSequence(t, acceptor, rootKeys, 0)
}

// Root rollover (§2.2): a bundle signed only by a new key is refused until an
// authenticated root update adds the key; sequences continue across the
// rotation; once the update removes the old key, the old key's bundles are
// refused and the new key's floor still holds.
func TestExtenderSeedRootRollover(t *testing.T) {
	clock := newTestClock()
	now := clock.Now()
	oldKeys := testSeedRootKeys(testSeedRootKey)
	bothKeys := testSeedRootKeys(testSeedRootKey, testSeedRotatedRootKey)
	newKeys := testSeedRootKeys(testSeedRotatedRootKey)
	acceptor := NewExtenderSeedAcceptor(NewExtenderSeedMemoryStore(), nil)

	requireExtenderSeedAccept(t, acceptor, testSeedBundle(t, testSeedRootKey, oldKeys, 5, 1, now), oldKeys, ExtenderSeedOutcomeAccepted)

	// before the update, the new key is no authority
	newKeyEnvelope := testSeedEnvelope(t, testSeedRotatedRootKey, 6, now, []*protocol.ExtenderRecord{testSeedRecord(t, testSeedRotatedRootKey, 2, now, "203.0.113.2")}, nil)
	_, err := VerifyExtenderSeed(newKeyEnvelope, oldKeys, []string{testExtenderNetworkHost}, now)
	requireExtenderSeedCode(t, err, ExtenderSeedCodeSignature)

	// the update adds it, and the sequence continues
	requireExtenderSeedAccept(t, acceptor, testSeedBundle(t, testSeedRotatedRootKey, bothKeys, 6, 2, now), bothKeys, ExtenderSeedOutcomeAccepted)

	// the update removes the old key: its bundles are refused, the floor holds
	if droppedCount, err := acceptor.Revalidate(newKeys); err != nil || droppedCount != 0 {
		t.Fatalf("revalidate = %d, %v; expected the new key's floor kept", droppedCount, err)
	}
	requireExtenderSeedSequence(t, acceptor, newKeys, 6)
	oldKeyEnvelope := testSeedEnvelope(t, testSeedRootKey, 7, now, []*protocol.ExtenderRecord{testSeedRecord(t, testSeedRootKey, 3, now, "203.0.113.3")}, nil)
	_, err = VerifyExtenderSeed(oldKeyEnvelope, newKeys, []string{testExtenderNetworkHost}, now)
	requireExtenderSeedCode(t, err, ExtenderSeedCodeSignature)
	_, err = acceptor.Accept(testSeedBundle(t, testSeedRotatedRootKey, newKeys, 5, 4, now), newKeys)
	requireExtenderSeedCode(t, err, ExtenderSeedCodeRollback)
}

// A floor whose bundle no longer verifies is void (net_extender_seed_store.go):
// a root update that removed its key before this client took any bundle of the
// new key leaves the space where a fresh install starts, with the configured
// minimum still in force.
func TestExtenderSeedRootRemovalVoidsTheFloor(t *testing.T) {
	clock := newTestClock()
	now := clock.Now()
	oldKeys := testSeedRootKeys(testSeedRootKey)
	newKeys := testSeedRootKeys(testSeedRotatedRootKey)
	backend := &testSeedBackend{}
	acceptor := NewExtenderSeedAcceptor(backend, &ExtenderSeedAcceptorSettings{MinSequence: 2})
	requireExtenderSeedAccept(t, acceptor, testSeedBundle(t, testSeedRootKey, oldKeys, 9, 1, now), oldKeys, ExtenderSeedOutcomeAccepted)

	stored, err := acceptor.Stored(testExtenderNetworkHost, newKeys, []string{testExtenderNetworkHost}, now)
	if err != nil || stored != nil {
		t.Fatalf("stored under the new keys = %v, %v; expected none", stored, err)
	}
	// the drop was saved, so the old floor does not come back with the old keys
	requireExtenderSeedSequence(t, acceptor, oldKeys, 0)
	_, err = acceptor.Accept(testSeedBundle(t, testSeedRotatedRootKey, newKeys, 1, 2, now), newKeys)
	requireExtenderSeedCode(t, err, ExtenderSeedCodeRollback)
	requireExtenderSeedAccept(t, acceptor, testSeedBundle(t, testSeedRotatedRootKey, newKeys, 3, 2, now), newKeys, ExtenderSeedOutcomeAccepted)
}

// A root installed by a settings import (K7) that a later authenticated update
// replaces cannot leave behind a floor for the real space: a hostile bundle at
// a huge sequence pins nothing once the real roots are back, whether the owner
// revalidates on the root change or the next accept finds the floor void.
func TestExtenderSeedHostileRootCannotPinTheFloor(t *testing.T) {
	clock := newTestClock()
	now := clock.Now()
	realKeys := testSeedRootKeys(testSeedRootKey)
	hostileKeys := testSeedRootKeys(testSeedForeignRootKey)

	for _, revalidate := range []bool{true, false} {
		t.Run(fmt.Sprintf("revalidate=%t", revalidate), func(t *testing.T) {
			acceptor := NewExtenderSeedAcceptor(NewExtenderSeedMemoryStore(), nil)
			requireExtenderSeedAccept(t, acceptor, testSeedBundle(t, testSeedRootKey, realKeys, 5, 1, now), realKeys, ExtenderSeedOutcomeAccepted)

			// under the imported hostile root, a hostile bundle for the real
			// space is accepted, at a sequence far above the real one
			requireExtenderSeedAccept(t, acceptor, testSeedBundle(t, testSeedForeignRootKey, hostileKeys, 1<<40, 2, now), hostileKeys, ExtenderSeedOutcomeAccepted)

			// the real roots come back over the authenticated path
			if revalidate {
				droppedCount, err := acceptor.Revalidate(realKeys)
				if err != nil || droppedCount != 1 {
					t.Fatalf("revalidate = %d, %v; expected the hostile floor dropped", droppedCount, err)
				}
				requireExtenderSeedSequence(t, acceptor, realKeys, 0)
			}
			requireExtenderSeedAccept(t, acceptor, testSeedBundle(t, testSeedRootKey, realKeys, 6, 3, now), realKeys, ExtenderSeedOutcomeAccepted)
			requireExtenderSeedSequence(t, acceptor, realKeys, 6)
		})
	}
}

// An expired bundle still holds its floor: freshness is no part of whether a
// floor binds, so an older bundle with a longer validity cannot come back once
// a newer one has lapsed.
func TestExtenderSeedExpiredBundleKeepsTheFloor(t *testing.T) {
	clock := newTestClock()
	now := clock.Now()
	rootKeys := testSeedRootKeys(testSeedRootKey)
	acceptor := NewExtenderSeedAcceptor(NewExtenderSeedMemoryStore(), nil)
	requireExtenderSeedAccept(t, acceptor, testSeedBundle(t, testSeedRootKey, rootKeys, 5, 1, now), rootKeys, ExtenderSeedOutcomeAccepted)

	later := now.Add(48 * time.Hour)
	_, err := acceptor.Stored(testExtenderNetworkHost, rootKeys, []string{testExtenderNetworkHost}, later)
	requireExtenderSeedCode(t, err, ExtenderSeedCodeExpired)
	requireExtenderSeedSequence(t, acceptor, rootKeys, 5)
	_, err = acceptor.Accept(testSeedBundle(t, testSeedRootKey, rootKeys, 4, 2, later), rootKeys)
	requireExtenderSeedCode(t, err, ExtenderSeedCodeRollback)
}

// Reset returns every space to what a fresh install has.
func TestExtenderSeedAcceptorReset(t *testing.T) {
	clock := newTestClock()
	now := clock.Now()
	rootKeys := testSeedRootKeys(testSeedRootKey)
	acceptor := NewExtenderSeedAcceptor(NewExtenderSeedMemoryStore(), nil)
	requireExtenderSeedAccept(t, acceptor, testSeedBundle(t, testSeedRootKey, rootKeys, 5, 1, now), rootKeys, ExtenderSeedOutcomeAccepted)
	if err := acceptor.Reset(); err != nil {
		t.Fatal(err)
	}
	requireExtenderSeedSequence(t, acceptor, rootKeys, 0)
	requireExtenderSeedAccept(t, acceptor, testSeedBundle(t, testSeedRootKey, rootKeys, 1, 1, now), rootKeys, ExtenderSeedOutcomeAccepted)
}

// The document keeps at most ExtenderSeedMaxStoredNetworkCount spaces, the
// least recently accepted evicted first, never the one just accepted.
func TestExtenderSeedAcceptorNetworkCap(t *testing.T) {
	clock := newTestClock()
	rootKeys := testSeedRootKeys(testSeedRootKey)
	acceptor := NewExtenderSeedAcceptor(NewExtenderSeedMemoryStore(), &ExtenderSeedAcceptorSettings{Now: clock.Now})
	networkHosts := []string{}
	for i := range ExtenderSeedMaxStoredNetworkCount + 1 {
		networkHosts = append(networkHosts, fmt.Sprintf("space%d.example", i))
	}
	for i, networkHost := range networkHosts {
		now := clock.Now()
		record := testSeedEditedRecord(t, testSeedRootKey, byte(i), now, func(body *protocol.ExtenderRecordBody) {
			body.NetworkHost = networkHost
		}, fmt.Sprintf("203.0.113.%d", i+1))
		body, err := NewExtenderSeedBody(networkHost, 1, now.Add(-time.Minute), now.Add(time.Hour), []*protocol.ExtenderRecord{record}, nil)
		if err != nil {
			t.Fatal(err)
		}
		envelope, err := SignExtenderSeed(testSeedRootKey, body)
		if err != nil {
			t.Fatal(err)
		}
		bundle, err := VerifyExtenderSeed(envelope, rootKeys, networkHosts, now)
		if err != nil {
			t.Fatal(err)
		}
		requireExtenderSeedAccept(t, acceptor, bundle, rootKeys, ExtenderSeedOutcomeAccepted)
		clock.advance(time.Second)
	}
	for i, networkHost := range networkHosts {
		sequence, err := acceptor.Sequence(networkHost, rootKeys)
		if err != nil {
			t.Fatal(err)
		}
		if expected := uint64(min(i, 1)); sequence != expected {
			t.Fatalf("space %d floor = %d, expected %d", i, sequence, expected)
		}
	}
}

// The file backend writes the whole value and reads back exactly it.
func TestExtenderSeedFileStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "extender-seed.json")
	fileStore := NewExtenderSeedFileStore(path)
	if value, err := fileStore.Load(); err != nil || value != nil {
		t.Fatalf("load of a missing file = %q, %v", value, err)
	}
	for _, value := range [][]byte{[]byte(`{"a":1}`), []byte(`{"b":2}`)} {
		if err := fileStore.Save(value); err != nil {
			t.Fatal(err)
		}
		loaded, err := fileStore.Load()
		if err != nil || !bytes.Equal(loaded, value) {
			t.Fatalf("load = %q, %v; expected %q", loaded, err, value)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// the state is the client's own: readable by its user alone
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("state file mode %v, expected no group or other access", info.Mode().Perm())
	}
}

// An empty key set is a space's unconfigured state and judges nothing: no
// floor is dropped while a client waits for its first keys.
func TestExtenderSeedEmptyRootKeysJudgeNothing(t *testing.T) {
	clock := newTestClock()
	now := clock.Now()
	rootKeys := testSeedRootKeys(testSeedRootKey)
	acceptor := NewExtenderSeedAcceptor(NewExtenderSeedMemoryStore(), nil)
	requireExtenderSeedAccept(t, acceptor, testSeedBundle(t, testSeedRootKey, rootKeys, 5, 1, now), rootKeys, ExtenderSeedOutcomeAccepted)

	for _, unconfigured := range []*ExtenderRootKeySet{nil, NewExtenderRootKeySet()} {
		if droppedCount, err := acceptor.Revalidate(unconfigured); err != nil || droppedCount != 0 {
			t.Fatalf("revalidate with no keys = %d, %v", droppedCount, err)
		}
		_, err := acceptor.Stored(testExtenderNetworkHost, unconfigured, []string{testExtenderNetworkHost}, now)
		requireExtenderSeedCode(t, err, ExtenderSeedCodeNoRootKeys)
	}
	requireExtenderSeedSequence(t, acceptor, rootKeys, 5)
}
