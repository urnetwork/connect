package connect

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	mathrand "math/rand"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
)

// The active cap of the client directory (GEOMAP §2.1, D26; EXTENDER.md E1):
// the records on the hinted continent are kept first, then the measured ones,
// then a random sample of the rest, and a record new to a full directory
// evicts a random one of the rest, whether it arrived by gossip or the feed.

// One active record for a cap test, signed, with its continent.
type testCapRecord struct {
	publicKey ed25519.PublicKey
	keyHex    string
	ip        netip.Addr
	continent string
	record    *protocol.ExtenderRecord
}

// The i-th address of a cap test, one per record, in the v6 documentation
// prefix.
func testCapIp(i int) netip.Addr {
	return netip.AddrFrom16([16]byte{
		0x20, 0x01, 0x0d, 0xb8,
		12: byte(i >> 24),
		13: byte(i >> 16),
		14: byte(i >> 8),
		15: byte(i),
	})
}

// Signs one record on a continent at one address.
func signTestCapRecord(
	t *testing.T,
	rootPrivateKey ed25519.PrivateKey,
	now time.Time,
	i int,
	continent string,
) *testCapRecord {
	t.Helper()
	publicKey := newTestExtenderKey(t)
	ip := testCapIp(i)
	record, err := SignExtenderRecord(rootPrivateKey, &protocol.ExtenderRecordBody{
		PublicKey:     publicKey,
		Addresses:     []*protocol.ExtenderAddress{testExtenderAddress(ip.String())},
		TcpPort:       443,
		UdpPort:       443,
		DnsPort:       53,
		CountryCode:   "zz",
		ContinentCode: continent,
		// each record newer than the one before
		IssueTimeMs:  uint64(now.UnixMilli()) + uint64(i),
		ExpireTimeMs: uint64(now.Add(24 * time.Hour).UnixMilli()),
		NetworkHost:  testExtenderNetworkHost,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &testCapRecord{
		publicKey: publicKey,
		keyHex:    hex.EncodeToString(publicKey),
		ip:        ip,
		continent: continent,
		record:    record,
	}
}

// Whether the directory holds an active record for the key.
func testCapRetained(directory *ExtenderDirectory, capRecord *testCapRecord) bool {
	return directory.IsActiveKey(capRecord.publicKey)
}

// A seeded draw for the Random seams, safe to share.
func testSeededRandom(seed int64) func() float64 {
	var stateLock sync.Mutex
	random := mathrand.New(mathrand.NewSource(seed))
	return func() float64 {
		stateLock.Lock()
		defer stateLock.Unlock()
		return random.Float64()
	}
}

// Six hundred records into a directory of the default cap keep 512, every
// one on the hinted continent among them, and the rest a sample of the
// others. The counts agree with what is kept, and the candidate order still
// puts the hinted continent first.
func TestExtenderDirectoryActiveCapKeepsTheHintedContinent(t *testing.T) {
	clock := newTestClock()
	directory, rootPrivateKey := newTestExtenderDirectory(t, clock, nil)
	if cap := directory.MaxActiveRecordCount(); cap != 512 {
		t.Fatalf("cap = %d, expected the default 512", cap)
	}
	directory.SetContinentHint("EU")

	capRecords := []*testCapRecord{}
	for i := range 600 {
		continent := "NA"
		if i%6 == 0 {
			continent = "EU"
		}
		capRecord := signTestCapRecord(t, rootPrivateKey, clock.Now(), i, continent)
		if _, err := directory.ApplyRecord(capRecord.record, ExtenderSourceGossip); err != nil {
			t.Fatal(err)
		}
		capRecords = append(capRecords, capRecord)
	}

	retainedCount := 0
	for _, capRecord := range capRecords {
		retained := testCapRetained(directory, capRecord)
		if capRecord.continent == "EU" && !retained {
			t.Fatalf("the hinted continent's %s was evicted", capRecord.ip)
		}
		if retained {
			retainedCount += 1
		}
	}
	connectAssertCount(t, "retained", retainedCount, 512)
	connectAssertCount(t, "active records", directory.ActiveRecordCount(), 512)
	// an evicted record takes its address with it, so every count agrees
	snapshot := directory.Snapshot()
	connectAssertCount(t, "known addresses", snapshot.KnownCount, 512)
	connectAssertCount(t, "active addresses", snapshot.ActiveCount, 512)
	connectAssertCount(t, "active count", directory.ActiveCount(0), 512)
	connectAssertCount(t, "usable count", directory.UsableCount(0), 512)
	// the hinted continent's hundred lead the candidate order
	candidates := directory.Candidates(6, 512)
	connectAssertCount(t, "candidates", len(candidates), 512)
	for i, candidate := range candidates {
		if want := i < 100; (candidate.ContinentCode == "EU") != want {
			t.Fatalf("candidate %d is on %q", i, candidate.ContinentCode)
		}
	}
}

// Fails the test when a count is not the one expected.
func connectAssertCount(t *testing.T, name string, got int, want int) {
	t.Helper()
	if got != want {
		t.Fatalf("%s = %d, expected %d", name, got, want)
	}
}

// Past the cap an eviction never takes a preferred record -- the hinted
// continent's or a measured one -- while one of the rest remains, whatever
// order they arrive in, and once none of the rest remains the measured go
// before the hinted continent's, the oldest applied first.
func TestExtenderDirectoryActiveCapEvictsTheRestFirst(t *testing.T) {
	clock := newTestClock()
	directory, rootPrivateKey := newTestExtenderDirectory(t, clock, func(settings *ExtenderDirectorySettings) {
		settings.MaxActiveRecordCount = 16
		settings.Random = testSeededRandom(1)
	})
	directory.SetContinentHint("EU")

	random := mathrand.New(mathrand.NewSource(2))
	preferredRecords := []*testCapRecord{}
	restRecords := []*testCapRecord{}
	retainedCount := func(capRecords []*testCapRecord) int {
		count := 0
		for _, capRecord := range capRecords {
			if testCapRetained(directory, capRecord) {
				count += 1
			}
		}
		return count
	}
	// the evictions of either kind the stream made, so the test proves both
	restEvictionCount := 0
	preferredEvictionCount := 0
	for i := range 400 {
		continent := "NA"
		if random.Intn(5) == 0 {
			continent = "EU"
		}
		capRecord := signTestCapRecord(t, rootPrivateKey, clock.Now(), i, continent)
		// what the apply may take: one of the rest while any is left, the
		// new record counted, since a record off the hinted continent
		// arrives unmeasured
		restLeft := 0 < retainedCount(restRecords) || continent != "EU"
		preferredBefore := retainedCount(preferredRecords)
		full := directory.ActiveRecordCount() == 16
		if _, err := directory.ApplyRecord(capRecord.record, ExtenderSourceFeed); err != nil {
			t.Fatal(err)
		}
		preferredAfter := retainedCount(preferredRecords)
		switch {
		case restLeft && preferredAfter < preferredBefore:
			t.Fatalf("apply %d evicted a preferred record with one of the rest left", i)
		case full && restLeft:
			restEvictionCount += 1
		case full && preferredAfter < preferredBefore:
			preferredEvictionCount += 1
		}
		switch {
		case continent == "EU":
			preferredRecords = append(preferredRecords, capRecord)
		case random.Intn(4) == 0 && testCapRetained(directory, capRecord):
			// a measured record is preferred as well
			directory.RecordLatency(capRecord.ip, time.Duration(1+random.Intn(100))*time.Millisecond, false)
			preferredRecords = append(preferredRecords, capRecord)
		default:
			restRecords = append(restRecords, capRecord)
		}
		connectAssertCount(t, fmt.Sprintf("active records after %d", i), directory.ActiveRecordCount(), min(i+1, 16))
	}
	if restEvictionCount == 0 || preferredEvictionCount == 0 {
		t.Fatalf(
			"the stream evicted %d of the rest and %d preferred, expected some of each",
			restEvictionCount,
			preferredEvictionCount,
		)
	}
}

// With none of the rest left, the measured go before the hinted continent's,
// each the oldest applied first, and a new record of the rest evicts itself.
func TestExtenderDirectoryActiveCapEvictsTheOldestPreferred(t *testing.T) {
	clock := newTestClock()
	directory, rootPrivateKey := newTestExtenderDirectory(t, clock, func(settings *ExtenderDirectorySettings) {
		settings.MaxActiveRecordCount = 4
	})
	directory.SetContinentHint("EU")

	near := []*testCapRecord{}
	for i := range 3 {
		capRecord := signTestCapRecord(t, rootPrivateKey, clock.Now(), i, "EU")
		if _, err := directory.ApplyRecord(capRecord.record, ExtenderSourceGossip); err != nil {
			t.Fatal(err)
		}
		near = append(near, capRecord)
	}
	measured := signTestCapRecord(t, rootPrivateKey, clock.Now(), 3, "NA")
	if _, err := directory.ApplyRecord(measured.record, ExtenderSourceGossip); err != nil {
		t.Fatal(err)
	}
	directory.RecordLatency(measured.ip, 20*time.Millisecond, true)

	// a record of the rest, into a directory with no other: it is the one
	// evicted
	restRecord := signTestCapRecord(t, rootPrivateKey, clock.Now(), 4, "NA")
	if changed, err := directory.ApplyRecord(restRecord.record, ExtenderSourceGossip); err != nil || !changed {
		t.Fatalf("the apply = %t, %v; a record is applied even when the cap takes it", changed, err)
	}
	if testCapRetained(directory, restRecord) {
		t.Fatal("the rest record displaced a preferred one")
	}
	// a hinted continent's record evicts the measured one first
	nearRecord := signTestCapRecord(t, rootPrivateKey, clock.Now(), 5, "EU")
	if _, err := directory.ApplyRecord(nearRecord.record, ExtenderSourceGossip); err != nil {
		t.Fatal(err)
	}
	if testCapRetained(directory, measured) {
		t.Fatal("the measured record outlived a record of the hinted continent")
	}
	// and the next one the oldest of the hinted continent
	secondNearRecord := signTestCapRecord(t, rootPrivateKey, clock.Now(), 6, "EU")
	if _, err := directory.ApplyRecord(secondNearRecord.record, ExtenderSourceGossip); err != nil {
		t.Fatal(err)
	}
	if testCapRetained(directory, near[0]) {
		t.Fatal("the oldest record of the hinted continent was not evicted")
	}
	for _, capRecord := range []*testCapRecord{near[1], near[2], nearRecord, secondNearRecord} {
		if !testCapRetained(directory, capRecord) {
			t.Fatalf("%s was evicted before an older one", capRecord.ip)
		}
	}
}

// A held record and an expired one are outside the cap: never counted, never
// evicted by it. This extender's own record and a record with a manual
// address are counted and never evicted.
func TestExtenderDirectoryActiveCapKeepsTheHeldExpiredOwnAndManual(t *testing.T) {
	clock := newTestClock()
	directory, rootPrivateKey := newTestExtenderDirectory(t, clock, func(settings *ExtenderDirectorySettings) {
		settings.MaxActiveRecordCount = 4
	})

	own := signTestCapRecord(t, rootPrivateKey, clock.Now(), 0, "NA")
	directory.KeepPublicKey(own.publicKey)
	manual := signTestCapRecord(t, rootPrivateKey, clock.Now(), 1, "NA")
	directory.AddManual(manual.ip)
	held := signTestCapRecord(t, rootPrivateKey, clock.Now(), 2, "NA")
	expiring := signTestCapRecord(t, rootPrivateKey, clock.Now(), 3, "NA")
	for _, capRecord := range []*testCapRecord{own, manual, held} {
		if _, err := directory.ApplyRecord(capRecord.record, ExtenderSourceGossip); err != nil {
			t.Fatal(err)
		}
	}
	expiringRecord, err := SignExtenderRecord(rootPrivateKey, &protocol.ExtenderRecordBody{
		PublicKey:    expiring.publicKey,
		Addresses:    []*protocol.ExtenderAddress{testExtenderAddress(expiring.ip.String())},
		TcpPort:      443,
		IssueTimeMs:  uint64(clock.Now().UnixMilli()),
		ExpireTimeMs: uint64(clock.Now().Add(time.Minute).UnixMilli()),
		NetworkHost:  testExtenderNetworkHost,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := directory.ApplyRecord(expiringRecord, ExtenderSourceGossip); err != nil {
		t.Fatal(err)
	}
	connectAssertCount(t, "active records", directory.ActiveRecordCount(), 4)

	// held for failure: out of the tier
	directory.RecordFailure(held.ip, ExtenderConnectModeTcpTls)
	connectAssertCount(t, "active records with one held", directory.ActiveRecordCount(), 3)
	// expired: out of the tier with the clock alone
	clock.advance(time.Minute + directory.settings.RecordExpireSkew + time.Second)
	connectAssertCount(t, "active records with one expired", directory.ActiveRecordCount(), 2)

	// a flood of the rest fills the tier and evicts only its own kind
	flood := []*testCapRecord{}
	for i := range 20 {
		capRecord := signTestCapRecord(t, rootPrivateKey, clock.Now(), 10+i, "NA")
		if _, err := directory.ApplyRecord(capRecord.record, ExtenderSourceGossip); err != nil {
			t.Fatal(err)
		}
		flood = append(flood, capRecord)
	}
	connectAssertCount(t, "active records after the flood", directory.ActiveRecordCount(), 4)
	for _, capRecord := range []*testCapRecord{own, manual, held} {
		if !testDirectoryKnown(directory, capRecord.ip) {
			t.Fatalf("%s was evicted by the active cap", capRecord.ip)
		}
	}
	if !directory.AddressUsable(expiring.ip) {
		t.Fatal("the retained expired record was evicted by the active cap")
	}
	floodRetained := 0
	for _, capRecord := range flood {
		if testCapRetained(directory, capRecord) {
			floodRetained += 1
		}
	}
	connectAssertCount(t, "flood records retained", floodRetained, 2)

	// the held record's hold lapsing brings it back into the tier, which the
	// next apply meets the cap for again
	clock.advance(directory.settings.HoldTimeout)
	connectAssertCount(t, "active records with the hold lapsed", directory.ActiveRecordCount(), 5)
	extra := signTestCapRecord(t, rootPrivateKey, clock.Now(), 40, "NA")
	if _, err := directory.ApplyRecord(extra.record, ExtenderSourceGossip); err != nil {
		t.Fatal(err)
	}
	connectAssertCount(t, "active records after the next apply", directory.ActiveRecordCount(), 4)
	for _, capRecord := range []*testCapRecord{own, manual} {
		if !testCapRetained(directory, capRecord) {
			t.Fatalf("%s was evicted by the active cap", capRecord.ip)
		}
	}
}

// Records from gossip and from the feed are one stream to the cap. A record
// the cap takes on arrival is still published to the subscribers, whose own
// caps judge it; a sample serves only what is kept; lowering the cap at run
// time evicts at once.
func TestExtenderDirectoryActiveCapBoundsGossipAndFeed(t *testing.T) {
	clock := newTestClock()
	directory, rootPrivateKey := newTestExtenderDirectory(t, clock, func(settings *ExtenderDirectorySettings) {
		settings.MaxActiveRecordCount = 32
		settings.Random = testSeededRandom(3)
	})
	messages, unsubscribe := directory.Subscribe()
	defer unsubscribe()

	capRecords := []*testCapRecord{}
	for i := range 48 {
		capRecord := signTestCapRecord(t, rootPrivateKey, clock.Now(), i, "NA")
		if i%2 == 0 {
			if _, err := directory.ApplySource(&protocol.ExtenderGossipMessage{
				Message: &protocol.ExtenderGossipMessage_Record{Record: capRecord.record},
			}, ExtenderSourceGossip); err != nil {
				t.Fatal(err)
			}
		} else if _, err := directory.ApplyRecord(capRecord.record, ExtenderSourceFeed); err != nil {
			t.Fatal(err)
		}
		capRecords = append(capRecords, capRecord)
		// every apply reaches the subscriber, the evicted ones included
		select {
		case message := <-messages:
			if message.GetRecord() != capRecord.record {
				t.Fatalf("apply %d published another record", i)
			}
		default:
			t.Fatalf("apply %d published nothing", i)
		}
	}
	connectAssertCount(t, "active records", directory.ActiveRecordCount(), 32)
	connectAssertCount(t, "event count", directory.EventCountLastMinute(), 48)

	retainedKeyHexes := map[string]bool{}
	for _, capRecord := range capRecords {
		if testCapRetained(directory, capRecord) {
			retainedKeyHexes[capRecord.keyHex] = true
		}
	}
	connectAssertCount(t, "retained", len(retainedKeyHexes), 32)
	sample := directory.SampleRecords(64, nil)
	connectAssertCount(t, "sample", len(sample), 32)
	for _, message := range sample {
		body := &protocol.ExtenderRecordBody{}
		if err := proto.Unmarshal(message.GetRecord().GetBody(), body); err != nil {
			t.Fatal(err)
		}
		if !retainedKeyHexes[hex.EncodeToString(body.PublicKey)] {
			t.Fatal("the sample served a record the cap evicted")
		}
	}

	directory.SetMaxActiveRecordCount(8)
	connectAssertCount(t, "cap", directory.MaxActiveRecordCount(), 8)
	connectAssertCount(t, "active records at the lowered cap", directory.ActiveRecordCount(), 8)
	connectAssertCount(t, "known addresses at the lowered cap", directory.Snapshot().KnownCount, 8)
}

// An unbounded cap keeps every record, which is the directory as it was.
func TestExtenderDirectoryActiveCapUnbounded(t *testing.T) {
	for _, maxActiveRecordCount := range []int{0, -1} {
		clock := newTestClock()
		directory, rootPrivateKey := newTestExtenderDirectory(t, clock, func(settings *ExtenderDirectorySettings) {
			settings.MaxActiveRecordCount = maxActiveRecordCount
		})
		directory.SetContinentHint("EU")
		for i := range 600 {
			capRecord := signTestCapRecord(t, rootPrivateKey, clock.Now(), i, "NA")
			if _, err := directory.ApplyRecord(capRecord.record, ExtenderSourceGossip); err != nil {
				t.Fatal(err)
			}
		}
		connectAssertCount(t, fmt.Sprintf("cap %d active records", maxActiveRecordCount), directory.ActiveRecordCount(), 600)
		connectAssertCount(t, fmt.Sprintf("cap %d known addresses", maxActiveRecordCount), directory.Snapshot().KnownCount, 600)
		connectAssertCount(t, fmt.Sprintf("cap %d candidates", maxActiveRecordCount), len(directory.Candidates(0, 1024)), 600)
	}
}

// A store written under a larger cap loads within the current one, and the
// retained records survive a second restart unchanged.
func TestExtenderDirectoryActiveCapAppliesToALoadedStore(t *testing.T) {
	clock := newTestClock()
	store := newTestExtenderDirectoryStore()
	directory, rootPrivateKey := newTestExtenderDirectory(t, clock, func(settings *ExtenderDirectorySettings) {
		settings.Store = store
		settings.MaxActiveRecordCount = 0
	})
	directory.SetContinentHint("EU")
	capRecords := []*testCapRecord{}
	for i := range 40 {
		continent := "NA"
		if i < 5 {
			continent = "EU"
		}
		capRecord := signTestCapRecord(t, rootPrivateKey, clock.Now(), i, continent)
		if _, err := directory.ApplyRecord(capRecord.record, ExtenderSourceFeed); err != nil {
			t.Fatal(err)
		}
		capRecords = append(capRecords, capRecord)
	}
	directory.Close()
	if saveCount, _ := store.counts(); saveCount == 0 {
		t.Fatal("nothing was saved")
	}

	// the same root key as the directory that saved, so what it saved still
	// verifies
	rootKeySet := NewExtenderRootKeySet(rootPrivateKey.Public().(ed25519.PublicKey))
	restore := func() *ExtenderDirectory {
		settings := DefaultExtenderDirectorySettings()
		settings.Now = clock.Now
		settings.NetworkHosts = []string{testExtenderNetworkHost}
		settings.Store = store
		settings.MaxActiveRecordCount = 16
		restored := NewExtenderDirectory(t.Context(), settings)
		restored.SetRootKeys(rootKeySet)
		return restored
	}
	reloaded := restore()
	connectAssertCount(t, "active records after the load", reloaded.ActiveRecordCount(), 16)
	connectAssertCount(t, "known addresses after the load", reloaded.Snapshot().KnownCount, 16)
	retained := []string{}
	for _, capRecord := range capRecords {
		if reloaded.IsActiveKey(capRecord.publicKey) {
			retained = append(retained, capRecord.keyHex)
		}
	}
	connectAssertCount(t, "retained after the load", len(retained), 16)
	slices.Sort(retained)
	reloaded.Close()

	again := restore()
	defer again.Close()
	retainedAgain := []string{}
	for _, capRecord := range capRecords {
		if again.IsActiveKey(capRecord.publicKey) {
			retainedAgain = append(retainedAgain, capRecord.keyHex)
		}
	}
	slices.Sort(retainedAgain)
	if !slices.Equal(retained, retainedAgain) {
		t.Fatal("a second restart changed what the cap kept")
	}
}

// A flood of applies inside one event window keeps the event ring's backing
// slice within twice its cap and its count at the cap: the head moves past
// what is dropped rather than the ring shifting on every apply, whose cost the
// million feed of the scale test judges.
func TestExtenderDirectoryEventRingStaysBoundedUnderAFlood(t *testing.T) {
	directory, now := newTestScaleDirectory(t, func(settings *ExtenderDirectorySettings) {
		settings.EventWindowTimeout = time.Hour
	})
	record := &protocol.ExtenderRecord{}
	for i := range 10 * ExtenderDirectoryEventRingCount {
		body, _ := testScaleRecord(i, "NA", now)
		directory.applyVerifiedRecord(record, body, ExtenderSourceGossip)
		directory.stateLock.Lock()
		eventCount := len(directory.eventTimes)
		directory.stateLock.Unlock()
		if 2*ExtenderDirectoryEventRingCount < eventCount {
			t.Fatalf("apply %d left %d event times behind the ring of %d", i, eventCount, ExtenderDirectoryEventRingCount)
		}
	}
	connectAssertCount(t, "events", directory.EventCountSince(now.Add(-time.Hour)), ExtenderDirectoryEventRingCount)
}
