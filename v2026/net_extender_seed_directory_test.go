package connect

import (
	"context"
	"crypto/ed25519"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Seed directory tests (EXTENDER-UPDATE2.md §2.1, §2.3, §2.4): what a seed may
// and may not make dialable, the order revocations take, and the expiry
// recovery a seed never gets.

// A directory of the test space with the given fixed root keys and store.
func newTestSeedDirectoryWithStore(
	t *testing.T,
	clock *testClock,
	store ExtenderDirectoryStore,
	rootPrivateKeys ...ed25519.PrivateKey,
) *ExtenderDirectory {
	t.Helper()
	settings := DefaultExtenderDirectorySettings()
	settings.Now = clock.Now
	settings.NetworkHosts = []string{testExtenderNetworkHost}
	settings.Store = store
	ctx, cancel := context.WithCancel(context.Background())
	directory := NewExtenderDirectory(ctx, settings)
	directory.SetRootKeys(testSeedRootKeys(rootPrivateKeys...))
	t.Cleanup(func() {
		directory.Close()
		cancel()
	})
	return directory
}

func testCandidateIps(candidates []*ExtenderCandidate) []string {
	ips := []string{}
	for _, candidate := range candidates {
		ips = append(ips, candidate.Ip.String())
	}
	slices.Sort(ips)
	return ips
}

func requireCandidateIps(t *testing.T, candidates []*ExtenderCandidate, ips ...string) {
	t.Helper()
	slices.Sort(ips)
	if got := testCandidateIps(candidates); !slices.Equal(got, ips) {
		t.Fatalf("candidates = %v, expected %v", got, ips)
	}
}

// The directory's whole dialable surface is empty: no candidate of either
// kind, nothing usable or active, and no address a strategy may keep.
func requireNothingDialable(t *testing.T, directory *ExtenderDirectory, ips ...string) {
	t.Helper()
	requireCandidateIps(t, directory.Candidates(0, 64))
	requireCandidateIps(t, directory.VerifiedCandidates(0, 64))
	if count := directory.UsableCount(0); count != 0 {
		t.Fatalf("usable = %d, expected none", count)
	}
	if count := directory.ActiveCount(0); count != 0 {
		t.Fatalf("active = %d, expected none", count)
	}
	for _, ip := range ips {
		addr := netip.MustParseAddr(ip)
		if directory.AddressUsable(addr) || directory.PinnedCandidate(addr) != nil {
			t.Fatalf("%s is usable", ip)
		}
	}
}

// A seed makes an address dialable only through a record that verifies under
// the root keys: a bare address with the seed source never is, and a seed
// record dropped by a root update leaves its address as undialable as any
// other unverified one.
func TestExtenderSeedNeverMakesABareAddressDialable(t *testing.T) {
	clock := newTestClock()
	now := clock.Now()
	directory := newTestSeedDirectory(t, clock, testSeedRootKey)

	if !directory.AddBootstrap(netip.MustParseAddr("203.0.113.50"), ExtenderSourceSeed) {
		t.Fatal("the bare address was not added")
	}
	requireNothingDialable(t, directory, "203.0.113.50")

	bundle, err := VerifyExtenderSeed(
		testSeedEnvelope(t, testSeedRootKey, 1, now, []*protocol.ExtenderRecord{testSeedRecord(t, testSeedRootKey, 1, now, "203.0.113.1")}, nil),
		directory.RootKeys(),
		[]string{testExtenderNetworkHost},
		now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyExtenderSeed(directory, bundle); err != nil {
		t.Fatal(err)
	}
	requireCandidateIps(t, directory.Candidates(0, 64), "203.0.113.1")
	candidate := directory.VerifiedCandidates(0, 64)[0]
	if !candidate.Verified || candidate.Source != ExtenderSourceSeed || !ed25519.PublicKey(candidate.PublicKey).Equal(testSeedPublicKey(0x81)) {
		t.Fatalf("the seed candidate does not carry its verified key: %+v", candidate)
	}

	// an authenticated update that removes the signing root drops the record
	directory.SetRootKeys(testSeedRootKeys(testSeedRotatedRootKey))
	requireNothingDialable(t, directory, "203.0.113.1", "203.0.113.50")
	if entry := testDirectoryEntry(t, directory, netip.MustParseAddr("203.0.113.1")); entry.Source != ExtenderSourceSeed || entry.State != ExtenderStateUnverified {
		t.Fatalf("the dropped seed address is %s/%s, expected an unverified seed address", entry.Source, entry.State)
	}
}

// Root-signed revocations land before any record (§2.3): a revocation in the
// bundle takes a feed-learned extender out of the candidates and keeps a
// bundled record it dominates out of the directory, and a revocation the
// directory already holds dominates a bundled record too. Only what no
// revocation dominates counts as usable.
func TestExtenderSeedRevocationsApplyBeforeCandidates(t *testing.T) {
	clock := newTestClock()
	now := clock.Now()
	directory := newTestSeedDirectory(t, clock, testSeedRootKey)

	// Y was learned from the feed, W's revocation too
	feedRecord := testSeedRecord(t, testSeedRootKey, 2, now, "203.0.113.2")
	if _, err := directory.ApplyRecord(feedRecord, ExtenderSourceFeed); err != nil {
		t.Fatal(err)
	}
	if _, err := directory.ApplyRevocation(testSeedRevocation(t, testSeedRootKey, 4, now.Add(-10*time.Minute))); err != nil {
		t.Fatal(err)
	}
	requireCandidateIps(t, directory.Candidates(0, 64), "203.0.113.2")

	records := []*protocol.ExtenderRecord{
		// X: usable
		testSeedRecord(t, testSeedRootKey, 1, now, "203.0.113.1"),
		// Z: dominated by the bundle's own revocation
		testSeedRecord(t, testSeedRootKey, 3, now, "203.0.113.3"),
		// W: dominated by the directory's revocation
		testSeedRecord(t, testSeedRootKey, 4, now, "203.0.113.4"),
	}
	revocations := []*protocol.ExtenderRevocation{
		testSeedRevocation(t, testSeedRootKey, 2, now.Add(-time.Hour)),
		testSeedRevocation(t, testSeedRootKey, 3, now.Add(-time.Hour)),
	}
	bundle, err := VerifyExtenderSeed(
		testSeedEnvelope(t, testSeedRootKey, 1, now, records, revocations),
		directory.RootKeys(),
		[]string{testExtenderNetworkHost},
		now,
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := ApplyExtenderSeed(directory, bundle)
	if err != nil {
		t.Fatal(err)
	}
	if result.RevocationCount != 2 || result.RecordCount != 2 || result.UsableRecordCount != 1 {
		t.Fatalf("apply = %+v, expected 2 revocations, 2 records, 1 usable", result)
	}
	requireCandidateIps(t, directory.Candidates(0, 64), "203.0.113.1")
	if testDirectoryKnown(directory, netip.MustParseAddr("203.0.113.3")) {
		t.Fatal("a record the bundle revoked entered the directory")
	}
	if state := testDirectoryState(t, directory, netip.MustParseAddr("203.0.113.2")); state != ExtenderStateRevoked {
		t.Fatalf("the feed record the bundle revoked is %s", state)
	}
	if state := testDirectoryState(t, directory, netip.MustParseAddr("203.0.113.4")); state != ExtenderStateRevoked {
		t.Fatalf("the bundled record the directory's revocation dominates is %s", state)
	}
}

// The expiry recovery that keeps the extenders a client last knew past their
// records' expiry never keeps a seed record (§2.3): once it lapses it is no
// last-tier candidate, a strategy drops its dialers, and the tick evicts it.
// The rule outlives a restart, and a newer record of the key from the feed
// lifts it. A feed record with the same lifetime is the control: it is kept.
func TestExtenderSeedExpiredSeedIsNotALastResort(t *testing.T) {
	clock := newTestClock()
	now := clock.Now()
	store := newTestExtenderDirectoryStore()
	directory := newTestSeedDirectoryWithStore(t, clock, store, testSeedRootKey)

	seedRecords := []*protocol.ExtenderRecord{
		testSeedRecord(t, testSeedRootKey, 1, now, "203.0.113.1"),
		testSeedRecord(t, testSeedRootKey, 3, now, "203.0.113.3"),
	}
	bundle, err := VerifyExtenderSeed(
		testSeedEnvelope(t, testSeedRootKey, 1, now, seedRecords, nil),
		directory.RootKeys(),
		[]string{testExtenderNetworkHost},
		now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyExtenderSeed(directory, bundle); err != nil {
		t.Fatal(err)
	}
	// the control, with the seed's own lifetime
	if _, err := directory.ApplyRecord(testSeedRecord(t, testSeedRootKey, 2, now, "203.0.113.2"), ExtenderSourceFeed); err != nil {
		t.Fatal(err)
	}
	// the feed renews the second seed key with a newer record, which is no
	// longer a seed record
	renewed := signTestRecord(t, testSeedRootKey, testSeedPublicKey(0x83), now.Add(-30*time.Minute), now.Add(48*time.Hour), testExtenderAddress("203.0.113.3"))
	if changed, err := directory.ApplyRecord(renewed, ExtenderSourceFeed); err != nil || !changed {
		t.Fatalf("the renewal = %t, %v", changed, err)
	}
	requireCandidateIps(t, directory.Candidates(0, 64), "203.0.113.1", "203.0.113.2", "203.0.113.3")

	// a restart: the store carries which record came from the seed
	directory.Close()
	directory = newTestSeedDirectoryWithStore(t, clock, store, testSeedRootKey)
	requireCandidateIps(t, directory.Candidates(0, 64), "203.0.113.1", "203.0.113.2", "203.0.113.3")

	clock.advance(49 * time.Hour)
	candidates := directory.Candidates(0, 64)
	requireCandidateIps(t, candidates, "203.0.113.2", "203.0.113.3")
	for _, candidate := range candidates {
		if !candidate.Expired {
			t.Fatalf("%s is not marked expired", candidate.Ip)
		}
	}
	seedIp := netip.MustParseAddr("203.0.113.1")
	if directory.AddressUsable(seedIp) || directory.PinnedCandidate(seedIp) != nil {
		t.Fatal("an expired seed record's address is still usable")
	}
	if !directory.AddressUsable(netip.MustParseAddr("203.0.113.2")) {
		t.Fatal("the expired feed record is not retained")
	}
	directory.Expire(clock.Now())
	if testDirectoryKnown(directory, seedIp) {
		t.Fatal("the tick kept an expired seed record")
	}
	if !testDirectoryKnown(directory, netip.MustParseAddr("203.0.113.2")) || !testDirectoryKnown(directory, netip.MustParseAddr("203.0.113.3")) {
		t.Fatal("the tick evicted a retained feed record")
	}
}

// Every refused bundle leaves the directory, its trust and the seed state as
// they were: no address, no candidate, the same root keys and network hosts,
// no floor (§2.4).
func TestExtenderSeedRefusedBundlesChangeNothing(t *testing.T) {
	clock := newTestClock()
	now := clock.Now()
	valid := testSeedRecord(t, testSeedRootKey, 1, now, "203.0.113.1")

	textOf := func(envelope *protocol.ExtenderSeedEnvelope) []byte {
		text, err := EncodeExtenderSeedText(envelope)
		if err != nil {
			t.Fatal(err)
		}
		return []byte(text)
	}
	bodyWith := func(records ...*protocol.ExtenderRecord) *protocol.ExtenderSeedBody {
		recordBytes := [][]byte{}
		for _, record := range records {
			recordBytes = append(recordBytes, testSeedMarshal(t, record))
		}
		return testSeedRawBody(now, 1, recordBytes, nil)
	}
	foreignBody := bodyWith(valid)
	foreignBody.NetworkHost = "other.example"
	settingsBody := testSeedMarshal(t, bodyWith(valid))
	settingsBytes := testSeedMarshal(t, &protocol.ExtenderShareSettings{
		RootPublicKeys: [][]byte{testSeedPublicKey(3)},
		GossipUrl:      "https://hostile.example/gossip",
	})
	settingsBody = append(append(settingsBody, 0x42, byte(len(settingsBytes))), settingsBytes...)
	shareText, err := EncodeExtenderShare(&protocol.ExtenderShare{
		NetworkHost: testExtenderNetworkHost,
		Addresses:   [][]byte{{203, 0, 113, 1}},
		Settings:    &protocol.ExtenderShareSettings{RootPublicKeys: [][]byte{testSeedPublicKey(3)}},
	})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		artifact []byte
		at       time.Time
		code     string
	}{
		{"forged", textOf(testSeedEnvelope(t, testSeedForeignRootKey, 1, now, []*protocol.ExtenderRecord{valid}, nil)), now, ExtenderSeedCodeSignature},
		{"foreign network", textOf(testSeedSignRaw(t, testSeedRootKey, foreignBody)), now, ExtenderSeedCodeForeignNetwork},
		{"gated record", textOf(testSeedSignRaw(t, testSeedRootKey, bodyWith(testSeedEditedRecord(t, testSeedRootKey, 1, now, func(body *protocol.ExtenderRecordBody) {
			body.DirectoryTier = ExtenderDirectoryTierGated
		}, "203.0.113.1")))), now, ExtenderSeedCodeGatedRecord},
		{"canary record", textOf(testSeedSignRaw(t, testSeedRootKey, bodyWith(testSeedEditedRecord(t, testSeedRootKey, 1, now, func(body *protocol.ExtenderRecordBody) {
			body.CanaryChannel = "channel-a"
		}, "203.0.113.1")))), now, ExtenderSeedCodeGatedRecord},
		{"valid envelope with an invalid record", textOf(testSeedSignRaw(t, testSeedRootKey, bodyWith(valid, testSeedRecord(t, testSeedForeignRootKey, 2, now, "203.0.113.2")))), now, ExtenderSeedCodeRecordInvalid},
		{"settings and root replacement", textOf(testSeedSignRawBytes(testSeedRootKey, settingsBody)), now, ExtenderSeedCodeMalformed},
		{"expired", textOf(testSeedEnvelope(t, testSeedRootKey, 1, now, []*protocol.ExtenderRecord{valid}, nil)), now.Add(25 * time.Hour), ExtenderSeedCodeExpired},
		{"future dated", textOf(testSeedEnvelope(t, testSeedRootKey, 1, now, []*protocol.ExtenderRecord{valid}, nil)), now.Add(-time.Hour), ExtenderSeedCodeNotYetValid},
		{"oversized", []byte(ExtenderSeedTextPrefix + strings.Repeat("A", ExtenderSeedMaxTextByteCount)), now, ExtenderSeedCodeTooLarge},
		{"a K7 share with settings", []byte(shareText), now, ExtenderSeedCodeMalformed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			caseClock := newTestClock()
			caseClock.now = c.at
			directory := newTestSeedDirectory(t, caseClock, testSeedRootKey)
			rootKeys := directory.RootKeys()
			acceptor := NewExtenderSeedAcceptor(NewExtenderSeedMemoryStore(), nil)

			_, err := ImportExtenderSeed(directory, acceptor, c.artifact)
			requireExtenderSeedCode(t, err, c.code)

			requireNothingDialable(t, directory, "203.0.113.1", "203.0.113.2")
			if snapshot := directory.Snapshot(); snapshot.KnownCount != 0 {
				t.Fatalf("known = %d, expected none", snapshot.KnownCount)
			}
			if !directory.RootKeys().Equal(rootKeys) || directory.RootKeys() != rootKeys {
				t.Fatal("the root keys changed")
			}
			if !slices.Equal(directory.settings.NetworkHosts, []string{testExtenderNetworkHost}) {
				t.Fatal("the network hosts changed")
			}
			requireExtenderSeedSequence(t, acceptor, rootKeys, 0)
		})
	}
}

// An imported bundle is applied and accepted; a restart applies the accepted
// bundle again from the store; and once it expires -- the embedded bundle of an
// old release -- neither the stored apply nor a fresh import of it gets past
// the freshness check.
func TestExtenderSeedImportAndStoredApply(t *testing.T) {
	clock := newTestClock()
	now := clock.Now()
	text, err := EncodeExtenderSeedText(testSeedEnvelope(t, testSeedRootKey, 3, now, []*protocol.ExtenderRecord{
		testSeedRecord(t, testSeedRootKey, 1, now, "203.0.113.1"),
	}, nil))
	if err != nil {
		t.Fatal(err)
	}
	acceptor := NewExtenderSeedAcceptor(NewExtenderSeedMemoryStore(), nil)

	directory := newTestSeedDirectory(t, clock, testSeedRootKey)
	result, err := ImportExtenderSeed(directory, acceptor, []byte("\n"+text+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != ExtenderSeedOutcomeAccepted || result.Apply.UsableRecordCount != 1 {
		t.Fatalf("import = %q %+v", result.Outcome, result.Apply)
	}
	requireCandidateIps(t, directory.Candidates(0, 64), "203.0.113.1")
	if result, err := ImportExtenderSeed(directory, acceptor, []byte(text)); err != nil || result.Outcome != ExtenderSeedOutcomeIdentical {
		t.Fatalf("a second import = %v, %v; expected identical", result, err)
	}

	// a restart with an empty directory
	restarted := newTestSeedDirectory(t, clock, testSeedRootKey)
	stored, err := ApplyStoredExtenderSeed(restarted, acceptor)
	if err != nil || stored == nil || stored.Bundle.Sequence != 3 {
		t.Fatalf("stored apply = %v, %v", stored, err)
	}
	requireCandidateIps(t, restarted.Candidates(0, 64), "203.0.113.1")

	clock.advance(25 * time.Hour)
	stale := newTestSeedDirectory(t, clock, testSeedRootKey)
	_, err = ApplyStoredExtenderSeed(stale, acceptor)
	requireExtenderSeedCode(t, err, ExtenderSeedCodeExpired)
	_, err = ImportExtenderSeed(stale, acceptor, []byte(text))
	requireExtenderSeedCode(t, err, ExtenderSeedCodeExpired)
	requireNothingDialable(t, stale, "203.0.113.1")

	// a directory with no accepted bundle has nothing to apply
	none, err := ApplyStoredExtenderSeed(newTestSeedDirectory(t, clock, testSeedRootKey), NewExtenderSeedAcceptor(nil, nil))
	if err != nil || none != nil {
		t.Fatalf("stored apply with no state = %v, %v", none, err)
	}
}

// A start applies the accepted bundle of every host the space accepts, each
// judged on its own: a stale bundle of one host keeps its floor and is passed
// over, and never keeps a fresh bundle of another host from applying. Only
// when every host's bundle is refused is the refusal returned.
func TestExtenderSeedStoredApplyPassesOverAStaleHost(t *testing.T) {
	clock := newTestClock()
	now := clock.Now()
	const staleHost = "stale.example"
	networkHosts := []string{staleHost, testExtenderNetworkHost}
	newDirectory := func() *ExtenderDirectory {
		settings := DefaultExtenderDirectorySettings()
		settings.Now = clock.Now
		settings.NetworkHosts = networkHosts
		ctx, cancel := context.WithCancel(context.Background())
		directory := NewExtenderDirectory(ctx, settings)
		directory.SetRootKeys(testSeedRootKeys(testSeedRootKey))
		t.Cleanup(func() {
			directory.Close()
			cancel()
		})
		return directory
	}
	acceptor := NewExtenderSeedAcceptor(NewExtenderSeedMemoryStore(), nil)

	// the first host's bundle is valid for an hour, the space's for a day
	staleRecord := testSeedEditedRecord(t, testSeedRootKey, 1, now, func(body *protocol.ExtenderRecordBody) {
		body.NetworkHost = staleHost
	}, "203.0.113.1")
	staleBody, err := NewExtenderSeedBody(staleHost, 1, now.Add(-time.Minute), now.Add(time.Hour), []*protocol.ExtenderRecord{staleRecord}, nil)
	if err != nil {
		t.Fatal(err)
	}
	staleEnvelope, err := SignExtenderSeed(testSeedRootKey, staleBody)
	if err != nil {
		t.Fatal(err)
	}
	directory := newDirectory()
	for _, envelope := range []*protocol.ExtenderSeedEnvelope{
		staleEnvelope,
		testSeedEnvelope(t, testSeedRootKey, 1, now, []*protocol.ExtenderRecord{testSeedRecord(t, testSeedRootKey, 2, now, "203.0.113.2")}, nil),
	} {
		text, err := EncodeExtenderSeedText(envelope)
		if err != nil {
			t.Fatal(err)
		}
		if result, err := ImportExtenderSeed(directory, acceptor, []byte(text)); err != nil || result.Outcome != ExtenderSeedOutcomeAccepted {
			t.Fatalf("import = %v, %v", result, err)
		}
	}

	// both in force: a start applies both
	started := newDirectory()
	result, err := ApplyStoredExtenderSeed(started, acceptor)
	if err != nil || result == nil || result.Bundle.NetworkHost != staleHost {
		t.Fatalf("stored apply = %v, %v; expected the first host's bundle", result, err)
	}
	requireCandidateIps(t, started.Candidates(0, 64), "203.0.113.1", "203.0.113.2")

	// two hours on, the first host's bundle is stale and the space's is not
	clock.advance(2 * time.Hour)
	later := newDirectory()
	result, err = ApplyStoredExtenderSeed(later, acceptor)
	if err != nil || result == nil || result.Bundle.NetworkHost != testExtenderNetworkHost || result.Apply.UsableRecordCount != 1 {
		t.Fatalf("stored apply with one stale host = %v, %v; expected the space's bundle", result, err)
	}
	requireCandidateIps(t, later.Candidates(0, 64), "203.0.113.2")
	if sequence, err := acceptor.Sequence(staleHost, later.RootKeys()); err != nil || sequence != 1 {
		t.Fatalf("the stale host's floor = %d, %v; expected it kept", sequence, err)
	}

	// once every host's bundle is stale, the first refusal is returned
	clock.advance(48 * time.Hour)
	stale := newDirectory()
	_, err = ApplyStoredExtenderSeed(stale, acceptor)
	requireExtenderSeedCode(t, err, ExtenderSeedCodeExpired)
	requireNothingDialable(t, stale, "203.0.113.1", "203.0.113.2")
}

// A bundle held past its validity is judged again when it is applied, so a
// caller that kept a verified bundle cannot apply it once it is stale.
func TestExtenderSeedApplyRejectsAStaleBundle(t *testing.T) {
	clock := newTestClock()
	now := clock.Now()
	directory := newTestSeedDirectory(t, clock, testSeedRootKey)
	bundle, err := VerifyExtenderSeed(
		testSeedEnvelope(t, testSeedRootKey, 1, now, []*protocol.ExtenderRecord{testSeedRecord(t, testSeedRootKey, 1, now, "203.0.113.1")}, nil),
		directory.RootKeys(),
		[]string{testExtenderNetworkHost},
		now,
	)
	if err != nil {
		t.Fatal(err)
	}
	clock.advance(25 * time.Hour)
	_, err = ApplyExtenderSeed(directory, bundle)
	requireExtenderSeedCode(t, err, ExtenderSeedCodeExpired)
	requireNothingDialable(t, directory, "203.0.113.1")
}

// A bundle whose every record is revoked by what the directory already holds
// is accepted but restores nothing, which the import reports.
func TestExtenderSeedImportWithNoUsableRecord(t *testing.T) {
	clock := newTestClock()
	now := clock.Now()
	directory := newTestSeedDirectory(t, clock, testSeedRootKey)
	if _, err := directory.ApplyRevocation(testSeedRevocation(t, testSeedRootKey, 1, now.Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	text, err := EncodeExtenderSeedText(testSeedEnvelope(t, testSeedRootKey, 1, now, []*protocol.ExtenderRecord{
		testSeedRecord(t, testSeedRootKey, 1, now, "203.0.113.1"),
	}, nil))
	if err != nil {
		t.Fatal(err)
	}
	result, err := ImportExtenderSeed(directory, NewExtenderSeedAcceptor(nil, nil), []byte(text))
	requireExtenderSeedCode(t, err, ExtenderSeedCodeNoUsableRecord)
	if result == nil || result.Apply.UsableRecordCount != 0 {
		t.Fatalf("the refusal carries no result: %+v", result)
	}
	requireNothingDialable(t, directory, "203.0.113.1")
}
