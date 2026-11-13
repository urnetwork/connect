package connect

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// The K7 "use settings" hostile root (EXTENDER-UPDATE2.md §2.2, §2.4; the void
// rule of net_extender_seed_store.go).
//
// A share's settings block is the one place K7 replaces the trust anchor, and
// only by an explicit import decision: the importer takes the settings' root
// keys (ExtenderShareRootKeyHexes) into the space's keys, which the directory
// then holds (SetRootKeys). That is what the sdk's ImportShare with
// useSettings and the restart after it do. While such a root is in force,
// whatever it signs verifies, including a seed bundle for the real space at a
// sequence far above the real one. Once the roots change again -- the first
// hello over the platform's pinned tls, or the settings put back -- that
// bundle's floor is void: it never pins the real space's sequence, the stored
// apply never brings it back, and a restart over the same stores keeps it so.
//
// The episode does cost the genuine floor (the hostile bundle replaced it, or
// the void rule dropped it under the hostile keys), which leaves the space
// where a fresh install starts: the configured minimum sequence and the
// validity window bound replay (§2.3).

const (
	// the genuine records' addresses
	testSeedK7GenuineIp  = "203.0.113.1"
	testSeedK7RenewedIp  = "203.0.113.2"
	testSeedK7OutdatedIp = "203.0.113.3"
	// the hostile record's address
	testSeedK7HostileIp = "203.0.113.66"
	// the hostile bundle's sequence, far above any genuine one
	testSeedK7HostileSequence = uint64(1) << 40
	// the minimum sequence the application configures
	testSeedK7MinSequence = 3
)

// The seed text of a bundle of `sequence` signed by `rootPrivateKey`, whose one
// record -- extender `n` at `ip` -- the same root signed.
func testSeedK7Text(
	t *testing.T,
	rootPrivateKey ed25519.PrivateKey,
	sequence uint64,
	n byte,
	ip string,
	now time.Time,
) string {
	t.Helper()
	text, err := EncodeExtenderSeedText(testSeedEnvelope(t, rootPrivateKey, sequence, now, []*protocol.ExtenderRecord{
		testSeedRecord(t, rootPrivateKey, n, now, ip),
	}, nil))
	if err != nil {
		t.Fatal(err)
	}
	return text
}

// The hex digest of a seed text's body, which is how the stored document names
// the bundle it holds.
func testSeedK7BodyDigestHex(t *testing.T, text string) string {
	t.Helper()
	envelope, err := DecodeExtenderSeedText(text)
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(ExtenderSeedBodyDigest(envelope.Body))
}

func requireExtenderSeedImport(
	t *testing.T,
	directory *ExtenderDirectory,
	acceptor *ExtenderSeedAcceptor,
	text string,
	outcome string,
) {
	t.Helper()
	result, err := ImportExtenderSeed(directory, acceptor, []byte(text))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if result.Outcome != outcome {
		t.Fatalf("import = %q, expected %q", result.Outcome, outcome)
	}
}

// A hostile K7 share for the real space: one address, and a settings block
// that names the given roots, a hostile dns name and a hostile gossip url.
func testSeedK7ShareText(t *testing.T, rootPrivateKeys ...ed25519.PrivateKey) string {
	t.Helper()
	rootPublicKeys := [][]byte{}
	for _, rootPrivateKey := range rootPrivateKeys {
		rootPublicKeys = append(rootPublicKeys, rootPrivateKey.Public().(ed25519.PublicKey))
	}
	text, err := EncodeExtenderShare(&protocol.ExtenderShare{
		NetworkHost: testExtenderNetworkHost,
		Addresses:   [][]byte{netip.MustParseAddr(testSeedK7HostileIp).AsSlice()},
		Settings: &protocol.ExtenderShareSettings{
			DnsName:        "hostile.example",
			GossipUrl:      "https://hostile.example/gossip",
			RootPublicKeys: rootPublicKeys,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return text
}

// The importer's side of a share taken with its settings (K7): the addresses as
// unverified import entries, then the settings' root keys as the anchor.
func testSeedK7ImportShareWithSettings(t *testing.T, directory *ExtenderDirectory, shareText string) {
	t.Helper()
	share, err := DecodeExtenderShare(shareText)
	if err != nil {
		t.Fatal(err)
	}
	ApplyExtenderShare(directory, share, "")
	rootKeys, err := NewExtenderRootKeySetFromHex(ExtenderShareRootKeyHexes(share)...)
	if err != nil || rootKeys.Len() == 0 {
		t.Fatalf("the share's settings carry no root key: %v", err)
	}
	directory.SetRootKeys(rootKeys)
}

// One run of the hostile-root case. The stores outlive the directory and the
// acceptor, so a restart reads what the run left.
type testSeedK7Run struct {
	clock            *testClock
	directoryStore   *testExtenderDirectoryStore
	seedPath         string
	acceptorSettings *ExtenderSeedAcceptorSettings
	directory        *ExtenderDirectory
	acceptor         *ExtenderSeedAcceptor
	hostileText      string
}

// Sets the genuine floor, takes the hostile share with its settings, and
// accepts the hostile bundle under the hostile roots.
func newTestSeedK7Run(t *testing.T, shareRootKeys ...ed25519.PrivateKey) *testSeedK7Run {
	t.Helper()
	clock := newTestClock()
	now := clock.Now()
	self := &testSeedK7Run{
		clock:          clock,
		directoryStore: newTestExtenderDirectoryStore(),
		seedPath:       filepath.Join(t.TempDir(), "extender-seed.json"),
		acceptorSettings: &ExtenderSeedAcceptorSettings{
			MinSequence: testSeedK7MinSequence,
			Now:         clock.Now,
		},
		hostileText: testSeedK7Text(t, testSeedForeignRootKey, testSeedK7HostileSequence, 66, testSeedK7HostileIp, now),
	}
	self.directory = newTestSeedDirectoryWithStore(t, clock, self.directoryStore, testSeedRootKey)
	self.acceptor = NewExtenderSeedAcceptor(NewExtenderSeedFileStore(self.seedPath), self.acceptorSettings)

	// the real space's genuine bundle sets the floor
	requireExtenderSeedImport(t, self.directory, self.acceptor, testSeedK7Text(t, testSeedRootKey, 5, 1, testSeedK7GenuineIp, now), ExtenderSeedOutcomeAccepted)
	requireExtenderSeedSequence(t, self.acceptor, self.directory.RootKeys(), 5)

	// the user takes a hostile share with its settings; the network hosts
	// are the space's own and stay so
	testSeedK7ImportShareWithSettings(t, self.directory, testSeedK7ShareText(t, shareRootKeys...))
	if !self.directory.RootKeys().Equal(testSeedRootKeys(shareRootKeys...)) {
		t.Fatal("the share's roots are not in force")
	}
	if !slices.Equal(self.directory.settings.NetworkHosts, []string{testExtenderNetworkHost}) {
		t.Fatal("the share changed the network hosts")
	}

	// under the hostile roots, a hostile bundle for the real space at a
	// sequence far above the real one is accepted, as anything they sign is
	requireExtenderSeedImport(t, self.directory, self.acceptor, self.hostileText, ExtenderSeedOutcomeAccepted)
	requireExtenderSeedSequence(t, self.acceptor, self.directory.RootKeys(), testSeedK7HostileSequence)
	if !slices.Contains(testCandidateIps(self.directory.VerifiedCandidates(0, 64)), testSeedK7HostileIp) {
		t.Fatal("the hostile record is not a candidate while its root is in force")
	}
	return self
}

// What holds once the real roots are back, before the next genuine bundle:
// the hostile floor binds nothing, the hostile bundle is no longer accepted or
// applied and its record is not dialable, and the configured minimum sequence
// is the floor; then the real space's next genuine bundle is accepted at its
// ordinary sequence.
func (self *testSeedK7Run) requireRecovered(t *testing.T) {
	t.Helper()
	now := self.clock.Now()
	if !self.directory.RootKeys().Equal(testSeedRootKeys(testSeedRootKey)) {
		t.Fatal("the real roots are not in force")
	}
	requireExtenderSeedSequence(t, self.acceptor, self.directory.RootKeys(), 0)

	// the hostile bundle again is refused outright, and pins nothing
	_, err := ImportExtenderSeed(self.directory, self.acceptor, []byte(self.hostileText))
	requireExtenderSeedCode(t, err, ExtenderSeedCodeSignature)

	// the stored apply finds nothing in force, so it brings nothing back
	stored, err := ApplyStoredExtenderSeed(self.directory, self.acceptor)
	if err != nil || stored != nil {
		t.Fatalf("stored apply = %v, %v; expected nothing in force", stored, err)
	}
	hostileIp := netip.MustParseAddr(testSeedK7HostileIp)
	if self.directory.AddressUsable(hostileIp) || self.directory.PinnedCandidate(hostileIp) != nil ||
		slices.Contains(testCandidateIps(self.directory.Candidates(0, 64)), testSeedK7HostileIp) {
		t.Fatal("the hostile record is dialable under the real roots")
	}
	if state := testDirectoryState(t, self.directory, hostileIp); state != ExtenderStateUnverified {
		t.Fatalf("the hostile address is %s, expected unverified", state)
	}

	// with no floor, the configured minimum binds
	_, err = ImportExtenderSeed(self.directory, self.acceptor, []byte(testSeedK7Text(t, testSeedRootKey, testSeedK7MinSequence-1, 3, testSeedK7OutdatedIp, now)))
	requireExtenderSeedCode(t, err, ExtenderSeedCodeRollback)

	// the real space's next genuine bundle is accepted at its ordinary
	// sequence, far below the hostile one
	requireExtenderSeedImport(t, self.directory, self.acceptor, testSeedK7Text(t, testSeedRootKey, 6, 2, testSeedK7RenewedIp, now), ExtenderSeedOutcomeAccepted)
	requireExtenderSeedSequence(t, self.acceptor, self.directory.RootKeys(), 6)
	if !slices.Contains(testCandidateIps(self.directory.VerifiedCandidates(0, 64)), testSeedK7RenewedIp) {
		t.Fatal("the genuine bundle's record is not a candidate")
	}

	// the stored document no longer holds the hostile bundle at all
	documentBytes, err := os.ReadFile(self.seedPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(documentBytes, []byte(testSeedK7BodyDigestHex(t, self.hostileText))) {
		t.Fatal("the stored document still holds the hostile bundle")
	}
}

// A restart over the same stores under the real roots: the genuine floor and
// bundle are what come back, and the hostile record stays undialable.
func (self *testSeedK7Run) requireRestart(t *testing.T) {
	t.Helper()
	self.directory.Close()
	directory := newTestSeedDirectoryWithStore(t, self.clock, self.directoryStore, testSeedRootKey)
	acceptor := NewExtenderSeedAcceptor(NewExtenderSeedFileStore(self.seedPath), self.acceptorSettings)
	requireExtenderSeedSequence(t, acceptor, directory.RootKeys(), 6)
	stored, err := ApplyStoredExtenderSeed(directory, acceptor)
	if err != nil || stored == nil || stored.Bundle.Sequence != 6 {
		t.Fatalf("stored apply after the restart = %v, %v; expected the genuine bundle of sequence 6", stored, err)
	}
	if hostileIp := netip.MustParseAddr(testSeedK7HostileIp); directory.AddressUsable(hostileIp) || directory.PinnedCandidate(hostileIp) != nil {
		t.Fatal("the hostile record is dialable after the restart")
	}
	_, err = acceptor.Accept(testSeedBundle(t, testSeedRootKey, directory.RootKeys(), 4, 4, self.clock.Now()), directory.RootKeys())
	requireExtenderSeedCode(t, err, ExtenderSeedCodeRollback)
}

// The roots come back by a settings change -- the share's settings put back,
// or the space's configured keys restored -- whether the hostile share named
// the hostile root alone or beside the real one, and whether the owner
// revalidates the seed state on the change or the next operation finds the
// floor void.
func TestExtenderSeedK7HostileRootCannotPinTheFloor(t *testing.T) {
	cases := []struct {
		name string
		// the roots the hostile share's settings name
		shareRootKeys []ed25519.PrivateKey
		revalidate    bool
	}{
		{"the hostile root alone", []ed25519.PrivateKey{testSeedForeignRootKey}, true},
		{"the hostile root alone, not revalidated", []ed25519.PrivateKey{testSeedForeignRootKey}, false},
		{"the hostile root beside the real one", []ed25519.PrivateKey{testSeedRootKey, testSeedForeignRootKey}, true},
		{"the hostile root beside the real one, not revalidated", []ed25519.PrivateKey{testSeedRootKey, testSeedForeignRootKey}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			run := newTestSeedK7Run(t, c.shareRootKeys...)
			now := run.clock.Now()

			// while the hostile roots are in force they bind as any roots do:
			// with the real root beside the hostile one, the hostile floor pins;
			// without it, a genuine bundle does not even verify
			_, err := ImportExtenderSeed(run.directory, run.acceptor, []byte(testSeedK7Text(t, testSeedRootKey, 6, 2, testSeedK7RenewedIp, now)))
			realRootBeside := slices.ContainsFunc(c.shareRootKeys, func(rootPrivateKey ed25519.PrivateKey) bool {
				return rootPrivateKey.Equal(testSeedRootKey)
			})
			if realRootBeside {
				requireExtenderSeedCode(t, err, ExtenderSeedCodeRollback)
			} else {
				requireExtenderSeedCode(t, err, ExtenderSeedCodeSignature)
			}

			// the roots change back
			run.directory.SetRootKeys(testSeedRootKeys(testSeedRootKey))
			if c.revalidate {
				droppedCount, err := run.acceptor.Revalidate(run.directory.RootKeys())
				if err != nil || droppedCount != 1 {
					t.Fatalf("revalidate = %d, %v; expected the hostile floor dropped", droppedCount, err)
				}
			}
			run.requireRecovered(t)
			run.requireRestart(t)
		})
	}
}

// The roots come back by the first hello over the platform's pinned tls, the
// path K7 names for replacing a share's roots again (refreshRootKeys): the
// network client's own read installs the real roots, and nothing binds to the
// hostile floor after it.
func TestExtenderSeedK7HostileRootIsVoidAfterHello(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		run := newTestSeedK7Run(t, testSeedForeignRootKey)

		hello := newTestExtenderHello(testHelloResultWithRootKeys(testSeedRootKey.Public().(ed25519.PublicKey)))
		loop := newTestHelloLoop(t, run.clock, run.directory, hello.Hello, nil, nil)
		synctest.Wait()
		if count, _ := hello.counts(); count != 1 {
			t.Fatalf("hello reads = %d, expected the first", count)
		}
		run.requireRecovered(t)
		loop.networkClient.Close()
		run.requireRestart(t)
	})
}
