package connect

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// The directory tiers (EXTENDER.md Q): the keyed partitions every channel
// releases by, the open channels' exclusion of the gated tier, the gated
// release policy, and canaries with per-country blocked state. Every clock is
// a fake and every secret is pinned, so nothing here depends on wall time or
// on a random draw.

// A fixed partition secret, so the placements are the same in every run.
var testPartitionSecret = bytes.Repeat([]byte{0x5a}, 32)

// Signs one open or gated record with its own key.
func signTestTierRecord(
	t *testing.T,
	rootPrivateKey ed25519.PrivateKey,
	extenderPublicKey ed25519.PublicKey,
	issueTime time.Time,
	directoryTier int,
	ip string,
) *protocol.ExtenderRecord {
	t.Helper()
	record, err := SignExtenderRecord(rootPrivateKey, &protocol.ExtenderRecordBody{
		PublicKey:     extenderPublicKey,
		Addresses:     []*protocol.ExtenderAddress{testExtenderAddress(ip)},
		TcpPort:       443,
		UdpPort:       443,
		DnsPort:       53,
		DnsTld:        "x.example.",
		CountryCode:   "us",
		IssueTimeMs:   uint64(issueTime.UnixMilli()),
		ExpireTimeMs:  uint64(issueTime.Add(24 * time.Hour).UnixMilli()),
		NetworkHost:   testExtenderNetworkHost,
		DirectoryTier: uint32(directoryTier),
	})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

// Applies `count` open records on distinct documentation addresses and
// returns their keys, hex and sorted.
func applyTestOpenRecords(
	t *testing.T,
	directory *ExtenderDirectory,
	rootPrivateKey ed25519.PrivateKey,
	issueTime time.Time,
	count int,
) []string {
	t.Helper()
	keyHexes := []string{}
	for i := range count {
		extenderKey := newTestExtenderKey(t)
		record := signTestTierRecord(
			t,
			rootPrivateKey,
			extenderKey,
			issueTime,
			ExtenderDirectoryTierOpen,
			fmt.Sprintf("198.51.100.%d", 1+i),
		)
		if _, err := directory.ApplyRecord(record, ExtenderSourceGossip); err != nil {
			t.Fatal(err)
		}
		keyHexes = append(keyHexes, hex.EncodeToString(extenderKey))
	}
	slices.Sort(keyHexes)
	return keyHexes
}

// The key of each record in a sample, hex.
func sampleKeyHexes(t *testing.T, directory *ExtenderDirectory, messages []*protocol.ExtenderGossipMessage) []string {
	t.Helper()
	keyHexes := []string{}
	for _, message := range messages {
		body, err := directory.RootKeys().VerifyRecord(message.GetRecord())
		if err != nil {
			t.Fatal(err)
		}
		keyHexes = append(keyHexes, hex.EncodeToString(body.PublicKey))
	}
	return keyHexes
}

// The partition count is the power of two at or above the square root, so it
// moves only when the fleet quadruples, and never cuts a partition below the
// minimum size: a small fleet is split less, down to one (Q2).
func TestExtenderPartitionCountIsThePowerOfTwoAboveTheRoot(t *testing.T) {
	cases := []struct {
		recordCount int
		expect      int
	}{
		{recordCount: 0, expect: 1},
		{recordCount: 1, expect: 1},
		{recordCount: 2, expect: 1},
		{recordCount: 5, expect: 1},
		{recordCount: 7, expect: 1},
		{recordCount: 8, expect: 2},
		{recordCount: 15, expect: 2},
		{recordCount: 16, expect: 4},
		{recordCount: 17, expect: 4},
		{recordCount: 31, expect: 4},
		{recordCount: 32, expect: 8},
		{recordCount: 64, expect: 8},
		{recordCount: 65, expect: 16},
		{recordCount: 256, expect: 16},
		{recordCount: 257, expect: 32},
		{recordCount: 512, expect: 32},
		{recordCount: 1025, expect: 64},
		{recordCount: 4096, expect: 64},
	}
	for _, c := range cases {
		if count := ExtenderPartitionCount(c.recordCount); count != c.expect {
			t.Errorf("partition count of %d = %d, expected %d", c.recordCount, count, c.expect)
		}
		// never fewer than the minimum per partition past one partition
		if count := ExtenderPartitionCount(c.recordCount); 1 < count && c.recordCount < count*ExtenderPartitionMinSize {
			t.Errorf("partition count of %d = %d leaves fewer than %d per partition", c.recordCount, count, ExtenderPartitionMinSize)
		}
	}
}

// A record's partition is a function of its key, the channel and the count:
// stable from call to call, and unrelated from one channel to the next, so a
// partition that leaks on one channel says nothing about the others (Q2).
func TestExtenderPartitionPlacementIsStableAndChannelSeparated(t *testing.T) {
	keyHexes := []string{}
	for i := range 64 {
		keyHexes = append(keyHexes, hex.EncodeToString(newTestExtenderKey(t)))
		_ = i
	}
	partitionCount := ExtenderPartitionCount(len(keyHexes))
	sameAcrossChannels := 0
	for _, keyHex := range keyHexes {
		dns := ExtenderRecordPartition(testPartitionSecret, ExtenderChannelDns, keyHex, partitionCount)
		if again := ExtenderRecordPartition(testPartitionSecret, ExtenderChannelDns, keyHex, partitionCount); again != dns {
			t.Fatalf("the placement of %s moved from %d to %d", keyHex, dns, again)
		}
		if dns < 0 || partitionCount <= dns {
			t.Fatalf("the placement of %s is %d, outside [0, %d)", keyHex, dns, partitionCount)
		}
		feed := ExtenderRecordPartition(testPartitionSecret, ExtenderChannelFeed, keyHex, partitionCount)
		gated := ExtenderRecordPartition(testPartitionSecret, ExtenderChannelGated, keyHex, partitionCount)
		if dns == feed && feed == gated {
			sameAcrossChannels += 1
		}
	}
	// one in sixty-four lands in the same partition on all three channels by
	// chance; all sixty-four would mean the channels share a key space
	if 16 < sameAcrossChannels {
		t.Errorf("%d of 64 records share a partition across every channel", sameAcrossChannels)
	}
	// and another secret deals everything again
	otherSecret := bytes.Repeat([]byte{0xa5}, 32)
	moved := 0
	for _, keyHex := range keyHexes {
		if ExtenderRecordPartition(testPartitionSecret, ExtenderChannelDns, keyHex, partitionCount) !=
			ExtenderRecordPartition(otherSecret, ExtenderChannelDns, keyHex, partitionCount) {
			moved += 1
		}
	}
	if moved < 32 {
		t.Errorf("another secret moved only %d of 64 records", moved)
	}
}

// A vantage whose partition is empty is handed the next partition that is
// not, and still exactly one (Q2).
func TestExtenderPartitionMembersHandsAnEmptyPartitionTheNext(t *testing.T) {
	// eight records make two partitions; a pool built of records that all
	// hash to one of them leaves the other empty for every vantage that
	// hashes there
	partitionCount := ExtenderPartitionCount(8)
	if partitionCount != 2 {
		t.Fatalf("eight records make %d partitions, expected 2", partitionCount)
	}
	keyHexes := []string{}
	for len(keyHexes) < 8 {
		keyHex := hex.EncodeToString(newTestExtenderKey(t))
		if ExtenderRecordPartition(testPartitionSecret, ExtenderChannelFeed, keyHex, partitionCount) == 0 {
			keyHexes = append(keyHexes, keyHex)
		}
	}
	emptyPartition := 1
	// a vantage that hashes to the empty partition
	var vantage []byte
	for i := range 4096 {
		candidate := []byte(fmt.Sprintf("vantage-%d", i))
		if ExtenderVantagePartition(testPartitionSecret, ExtenderChannelFeed, candidate, partitionCount) == emptyPartition {
			vantage = candidate
			break
		}
	}
	if vantage == nil {
		t.Fatal("no vantage hashed to the empty partition")
	}
	members, partition, count := ExtenderPartitionMembers(testPartitionSecret, ExtenderChannelFeed, vantage, keyHexes)
	if count != partitionCount {
		t.Fatalf("partition count = %d, expected %d", count, partitionCount)
	}
	if partition == emptyPartition {
		t.Fatalf("the vantage was left on empty partition %d", partition)
	}
	if len(members) == 0 {
		t.Fatal("the vantage was handed no members")
	}
	for _, keyHex := range members {
		if ExtenderRecordPartition(testPartitionSecret, ExtenderChannelFeed, keyHex, partitionCount) != partition {
			t.Fatalf("member %s is not in partition %d", keyHex, partition)
		}
	}
}

// The epoch deals a partition in another order, and the same epoch in the
// same one (Q2).
func TestExtenderPartitionOrderRotatesByEpoch(t *testing.T) {
	members := []string{}
	for range 16 {
		members = append(members, hex.EncodeToString(newTestExtenderKey(t)))
	}
	vantage := []byte("vantage")
	first := ExtenderPartitionOrder(testPartitionSecret, ExtenderChannelFeed, vantage, 7, members)
	again := ExtenderPartitionOrder(testPartitionSecret, ExtenderChannelFeed, vantage, 7, members)
	if !slices.Equal(first, again) {
		t.Fatal("the same epoch dealt another order")
	}
	next := ExtenderPartitionOrder(testPartitionSecret, ExtenderChannelFeed, vantage, 8, members)
	if slices.Equal(first, next) {
		t.Fatal("the next epoch dealt the same order")
	}
	sortedFirst := slices.Clone(first)
	slices.Sort(sortedFirst)
	sortedMembers := slices.Clone(members)
	slices.Sort(sortedMembers)
	if !slices.Equal(sortedFirst, sortedMembers) {
		t.Fatal("the order is not a permutation of the members")
	}
	// the epoch is whole epochs since the unix epoch
	if epoch := ExtenderEpoch(time.UnixMilli(3*int64(time.Hour/time.Millisecond)+1), time.Hour); epoch != 3 {
		t.Errorf("epoch = %d, expected 3", epoch)
	}
	if epoch := ExtenderEpoch(time.UnixMilli(1), 0); epoch != 0 {
		t.Errorf("epoch with no timeout = %d, expected 0", epoch)
	}
}

// Root cause: one observer enumerates the fleet in about a week because the
// open channels serve the whole set, shared region wide. The feed sample is
// keyed by the client's vantage and the epoch into its partition, so a vantage
// that polls forever sees its partition of the open tier and no more (Q2).
func TestExtenderDirectorySampleRecordsBoundsAVantageToItsPartition(t *testing.T) {
	clock := newTestClock()
	directory, rootPrivateKey := newTestExtenderDirectory(t, clock, func(settings *ExtenderDirectorySettings) {
		settings.PartitionSecret = testPartitionSecret
		settings.OpenEpochTimeout = time.Hour
	})
	keyHexes := applyTestOpenRecords(t, directory, rootPrivateKey, clock.Now(), 64)

	vantage := ExtenderVantageKey(netip.MustParseAddr("203.0.113.7"))
	members, _, partitionCount := ExtenderPartitionMembers(testPartitionSecret, ExtenderChannelFeed, vantage, keyHexes)
	if partitionCount != 8 {
		t.Fatalf("partition count = %d, expected 8 for 64 records", partitionCount)
	}

	// two polls in one epoch are the same sample
	first := sampleKeyHexes(t, directory, directory.SampleRecords(ExtenderFeedMaxSampleCount, nil, vantage))
	again := sampleKeyHexes(t, directory, directory.SampleRecords(ExtenderFeedMaxSampleCount, nil, vantage))
	if !slices.Equal(first, again) {
		t.Fatalf("two polls in one epoch differ: %v and %v", first, again)
	}
	if len(first) == 0 {
		t.Fatal("the sample is empty")
	}

	// a poll every ten minutes for a week sees the partition and no more
	seen := map[string]bool{}
	for range 7 * 24 * 6 {
		for _, keyHex := range sampleKeyHexes(t, directory, directory.SampleRecords(ExtenderFeedMaxSampleCount, nil, vantage)) {
			seen[keyHex] = true
		}
		clock.advance(10 * time.Minute)
	}
	seenKeyHexes := slices.Sorted(func(yield func(string) bool) {
		for keyHex := range seen {
			if !yield(keyHex) {
				return
			}
		}
	})
	if !slices.Equal(seenKeyHexes, members) {
		t.Fatalf(
			"a week of polling saw %d records, expected exactly the %d of the partition (of %d)",
			len(seenKeyHexes),
			len(members),
			len(keyHexes),
		)
	}
	if len(keyHexes) <= len(seenKeyHexes) {
		t.Fatalf("the vantage saw the whole open tier")
	}

	// the same prefix is the same vantage
	sibling := ExtenderVantageKey(netip.MustParseAddr("203.0.113.200"))
	if !bytes.Equal(sibling, vantage) {
		t.Fatalf("two addresses of one /24 are different vantages")
	}
	// another prefix in another partition sees other records; one is found
	// deterministically, since the secret is pinned
	var otherVantage []byte
	for i := range 4096 {
		candidate := ExtenderVantageKey(netip.MustParseAddr(fmt.Sprintf("192.0.%d.1", i%256)))
		candidate = append(candidate, byte(i/256))
		otherMembers, _, _ := ExtenderPartitionMembers(testPartitionSecret, ExtenderChannelFeed, candidate, keyHexes)
		if !slices.Equal(otherMembers, members) {
			otherVantage = candidate
			break
		}
	}
	if otherVantage == nil {
		t.Fatal("no other vantage fell in another partition")
	}
	other := sampleKeyHexes(t, directory, directory.SampleRecords(ExtenderFeedMaxSampleCount, nil, otherVantage))
	for _, keyHex := range other {
		if slices.Contains(members, keyHex) {
			t.Fatalf("another partition's sample carried %s of this one", keyHex)
		}
	}
}

// Root cause: blocking the open tier kills durable service because operator
// hosts are in the open channels. A gated record is applied and dialed like
// any other, and no open channel carries it: not the feed sample from any
// vantage, not the feed stream, not the mesh (Q1).
func TestExtenderDirectoryOpenChannelsExcludeAGatedRecord(t *testing.T) {
	clock := newTestClock()
	directory, rootPrivateKey := newTestExtenderDirectory(t, clock, func(settings *ExtenderDirectorySettings) {
		settings.PartitionSecret = testPartitionSecret
	})
	messages, unsubscribe := directory.Subscribe()
	defer unsubscribe()

	gatedKey := newTestExtenderKey(t)
	gatedKeyHex := hex.EncodeToString(gatedKey)
	gatedIp := "192.0.2.77"
	gatedRecord := signTestTierRecord(t, rootPrivateKey, gatedKey, clock.Now(), ExtenderDirectoryTierGated, gatedIp)
	// a leak: the gated record arrives over the mesh, the most open source
	if changed, err := directory.ApplyRecord(gatedRecord, ExtenderSourceGossip); err != nil || !changed {
		t.Fatalf("the gated record was not applied: changed=%t err=%v", changed, err)
	}
	openKeyHexes := applyTestOpenRecords(t, directory, rootPrivateKey, clock.Now(), 4)

	// the subscriber got every open record and never the gated one
	streamed := []string{}
	for range len(openKeyHexes) {
		select {
		case message := <-messages:
			body, err := directory.RootKeys().VerifyRecord(message.GetRecord())
			if err != nil {
				t.Fatal(err)
			}
			streamed = append(streamed, hex.EncodeToString(body.PublicKey))
		default:
			t.Fatalf("the stream carried %d records, expected %d", len(streamed), len(openKeyHexes))
		}
	}
	select {
	case message := <-messages:
		t.Fatalf("the stream carried more than the open records: %v", message)
	default:
	}
	slices.Sort(streamed)
	if !slices.Equal(streamed, openKeyHexes) {
		t.Fatalf("the stream carried %v, expected the open records %v", streamed, openKeyHexes)
	}

	// no vantage and no epoch samples it
	for i := range 64 {
		vantage := []byte(fmt.Sprintf("vantage-%d", i))
		for _, keyHex := range sampleKeyHexes(t, directory, directory.SampleRecords(ExtenderFeedMaxSampleCount, nil, vantage)) {
			if keyHex == gatedKeyHex {
				t.Fatalf("vantage %d was sampled the gated record", i)
			}
		}
		if directory.OpenPartitionContains(vantage, gatedKey) {
			t.Fatalf("vantage %d's partition contains the gated record", i)
		}
		clock.advance(time.Hour)
	}
	// not even as the extender's own record
	if sample := directory.SampleRecords(ExtenderFeedMaxSampleCount, gatedKey, nil); 0 < len(sample) {
		if body, _ := directory.RootKeys().VerifyRecord(sample[0].GetRecord()); hex.EncodeToString(body.PublicKey) == gatedKeyHex {
			t.Fatal("the gated record was served as the extender's own")
		}
	}

	// and it is a candidate all the same, marked gated
	found := false
	for _, candidate := range directory.Candidates(4, 64) {
		if candidate.Ip == netip.MustParseAddr(gatedIp) {
			found = true
			if candidate.DirectoryTier != ExtenderDirectoryTierGated {
				t.Errorf("the gated candidate has tier %d", candidate.DirectoryTier)
			}
		}
	}
	if !found {
		t.Fatal("the gated record is not a candidate")
	}
	entry := testDirectoryEntry(t, directory, netip.MustParseAddr(gatedIp))
	if entry.DirectoryTier != ExtenderDirectoryTierGated {
		t.Errorf("the status shows tier %d for the gated address", entry.DirectoryTier)
	}
	if entry := testDirectoryEntry(t, directory, netip.MustParseAddr("198.51.100.1")); entry.DirectoryTier != ExtenderDirectoryTierOpen {
		t.Errorf("the status shows tier %d for an open address", entry.DirectoryTier)
	}
}

// A record that predates the tier field is open, and a legacy reader skips
// the field: the tier is additive on the wire (Q1).
func TestExtenderRecordTierIsAdditive(t *testing.T) {
	if ExtenderRecordGated(nil) {
		t.Error("a nil body is gated")
	}
	if ExtenderRecordGated(&protocol.ExtenderRecordBody{}) {
		t.Error("a body with no tier is gated")
	}
	if !ExtenderRecordGated(&protocol.ExtenderRecordBody{DirectoryTier: ExtenderDirectoryTierGated}) {
		t.Error("a gated body is not gated")
	}
	candidate := &ExtenderCandidate{}
	if candidate.DirectoryTier != ExtenderDirectoryTierOpen {
		t.Error("an unverified candidate is not open")
	}
}

// The vantage of an address is its prefix: the /24 of v4 and the /48 of v6
// (Q2), so a poller cannot change partition by changing its last octets.
func TestExtenderVantageKeyIsThePrefix(t *testing.T) {
	cases := []struct {
		a      string
		b      string
		expect bool
	}{
		{a: "198.51.100.7", b: "198.51.100.200", expect: true},
		{a: "198.51.100.7", b: "198.51.101.7", expect: false},
		{a: "2001:db8:1:2::1", b: "2001:db8:1:ffff::1", expect: true},
		{a: "2001:db8:1:2::1", b: "2001:db8:2:2::1", expect: false},
		{a: "::ffff:198.51.100.7", b: "198.51.100.9", expect: true},
	}
	for _, c := range cases {
		a := ExtenderVantageKey(netip.MustParseAddr(c.a))
		b := ExtenderVantageKey(netip.MustParseAddr(c.b))
		if bytes.Equal(a, b) != c.expect {
			t.Errorf("vantage(%s) == vantage(%s) is %t, expected %t", c.a, c.b, !c.expect, c.expect)
		}
	}
	if ExtenderVantageKey(netip.Addr{}) != nil {
		t.Error("an invalid address has a vantage")
	}
	if ExtenderVantageKeyOfAddr(nil) != nil {
		t.Error("a nil address has a vantage")
	}
	if prefix := ExtenderVantagePrefix(netip.MustParseAddr("198.51.100.7")); prefix != "198.51.100.0/24" {
		t.Errorf("vantage prefix = %s", prefix)
	}
	if prefix := ExtenderVantagePrefix(netip.MustParseAddr("2001:db8:1:2::1")); prefix != "2001:db8:1::/48" {
		t.Errorf("vantage prefix = %s", prefix)
	}
}

// A seeded gated fleet, hex and sorted, with its population pinned per partition.
func testGatedKeyHexes(t *testing.T, count int) []string {
	t.Helper()
	partitionSizes := make([]int, ExtenderPartitionCount(count))
	for index := range count {
		partitionSizes[index%len(partitionSizes)]++
	}
	return testGatedKeyHexesByPartition(t, partitionSizes)
}

// One policy over a memory ledger with the fake clock and the test secret.
func newTestReleasePolicy(
	t *testing.T,
	clock *testClock,
	blocked ExtenderReleaseBlockedSource,
	configure func(settings *ExtenderReleaseSettings),
) *ExtenderReleasePolicy {
	t.Helper()
	settings := DefaultExtenderReleaseSettings()
	settings.Now = clock.Now
	if configure != nil {
		configure(settings)
	}
	ledger := NewExtenderReleaseMemoryLedger(settings.RequestWindow, settings.ClientWindow)
	return NewExtenderReleasePolicy(testPartitionSecret, ledger, blocked, settings)
}

// Root cause: a Sybil harvests the gated tier because nothing limits an
// identity. A release is deterministic in the identity and the epoch, so
// asking again learns nothing; an identity's requests are capped per window;
// and over any number of epochs an identity sees its partition and no more
// (Q3).
func TestExtenderReleaseIsDeterministicAndCappedPerIdentity(t *testing.T) {
	clock := newTestClock()
	policy := newTestReleasePolicy(t, clock, nil, nil)
	keyHexes := testGatedKeyHexes(t, 64)
	identity := []byte("network-a/client-a")
	request := func() *ExtenderReleaseRequest {
		return &ExtenderReleaseRequest{
			Identity:           identity,
			IdentityCreateTime: clock.Now().Add(-60 * 24 * time.Hour),
			Vantage:            "198.51.100.0/24",
			CountryCode:        "aa",
		}
	}

	first, err := policy.Release(request(), keyHexes)
	if err != nil {
		t.Fatal(err)
	}
	if first.Probation {
		t.Fatal("a sixty day old identity is in probation")
	}
	if len(first.KeyHexes) != ExtenderReleaseTrustedIdentityCount {
		t.Fatalf("a trusted identity got %d records, expected %d", len(first.KeyHexes), ExtenderReleaseTrustedIdentityCount)
	}
	// the limit is requests per window, and every request within the epoch
	// is the same release
	for i := 1; i < ExtenderReleaseIdentityRequestLimit; i += 1 {
		again, err := policy.Release(request(), keyHexes)
		if err != nil {
			t.Fatalf("request %d: %s", i+1, err)
		}
		if !slices.Equal(again.KeyHexes, first.KeyHexes) {
			t.Fatalf("request %d released %v, expected the same %v", i+1, again.KeyHexes, first.KeyHexes)
		}
	}
	if _, err := policy.Release(request(), keyHexes); !errors.Is(err, ErrExtenderReleaseIdentityLimited) {
		t.Fatalf("request %d was answered with %v, expected the identity limit", ExtenderReleaseIdentityRequestLimit+1, err)
	}
	// the window passes and the next epoch deals the partition again
	members, _, _ := ExtenderPartitionMembers(testPartitionSecret, ExtenderChannelGated, identity, keyHexes)
	seen := map[string]bool{}
	for _, keyHex := range first.KeyHexes {
		seen[keyHex] = true
	}
	for range 52 {
		clock.advance(ExtenderReleaseEpochTimeout)
		result, err := policy.Release(request(), keyHexes)
		if err != nil {
			t.Fatal(err)
		}
		for _, keyHex := range result.KeyHexes {
			if !slices.Contains(members, keyHex) {
				t.Fatalf("%s was released outside the identity's partition", keyHex)
			}
			seen[keyHex] = true
		}
	}
	if len(members) < len(seen) || len(keyHexes) <= len(seen) {
		t.Fatalf("a year of epochs released %d records, expected at most the partition's %d of %d", len(seen), len(members), len(keyHexes))
	}
}

// Root cause: a Sybil harvests the gated tier from one network with many
// identities. Requests are capped per vantage -- the asn, or the prefix --
// across identities, and another vantage is not held by it (Q3).
func TestExtenderReleaseIsCappedPerVantage(t *testing.T) {
	clock := newTestClock()
	policy := newTestReleasePolicy(t, clock, nil, func(settings *ExtenderReleaseSettings) {
		settings.VantageRequestLimit = 5
	})
	keyHexes := testGatedKeyHexes(t, 16)
	request := func(identity string, vantage string) *ExtenderReleaseRequest {
		return &ExtenderReleaseRequest{
			Identity:           []byte(identity),
			IdentityCreateTime: clock.Now().Add(-time.Hour),
			Vantage:            vantage,
			CountryCode:        "aa",
		}
	}
	for i := range 5 {
		if _, err := policy.Release(request(fmt.Sprintf("sybil-%d", i), "AS64496"), keyHexes); err != nil {
			t.Fatalf("identity %d of the vantage: %s", i, err)
		}
	}
	if _, err := policy.Release(request("sybil-5", "AS64496"), keyHexes); !errors.Is(err, ErrExtenderReleaseVantageLimited) {
		t.Fatalf("the sixth identity of the vantage was answered with %v, expected the vantage limit", err)
	}
	// a refused request counts too: trying does not open the gate
	if _, err := policy.Release(request("sybil-5", "AS64496"), keyHexes); !errors.Is(err, ErrExtenderReleaseVantageLimited) {
		t.Fatalf("a retry was answered with %v, expected the vantage limit", err)
	}
	if _, err := policy.Release(request("honest", "AS64497"), keyHexes); err != nil {
		t.Fatalf("another vantage was held by the first: %s", err)
	}
	// the window is inclusive of its start: at exactly one window every
	// request still counts, and a moment later none does
	clock.advance(ExtenderReleaseRequestWindow)
	if _, err := policy.Release(request("sybil-5", "AS64496"), keyHexes); !errors.Is(err, ErrExtenderReleaseVantageLimited) {
		t.Fatalf("at the window's edge: %v, expected the vantage limit", err)
	}
	clock.advance(time.Second)
	if _, err := policy.Release(request("sybil-5", "AS64496"), keyHexes); err != nil {
		t.Fatalf("after the window: %s", err)
	}
}

// A new identity is released one record, a trusted one three (Q3, Lox).
func TestExtenderReleaseProbationReleasesOne(t *testing.T) {
	clock := newTestClock()
	policy := newTestReleasePolicy(t, clock, nil, nil)
	keyHexes := testGatedKeyHexes(t, 64)
	cases := []struct {
		name       string
		createTime time.Time
		expect     int
		probation  bool
	}{
		{name: "unknown age", createTime: time.Time{}, expect: 1, probation: true},
		{name: "a day old", createTime: clock.Now().Add(-24 * time.Hour), expect: 1, probation: true},
		{name: "just short of probation", createTime: clock.Now().Add(-ExtenderReleaseProbationTimeout + time.Second), expect: 1, probation: true},
		{name: "probation served", createTime: clock.Now().Add(-ExtenderReleaseProbationTimeout), expect: 3, probation: false},
	}
	for i, c := range cases {
		result, err := policy.Release(&ExtenderReleaseRequest{
			Identity:           []byte(fmt.Sprintf("identity-%d", i)),
			IdentityCreateTime: c.createTime,
			Vantage:            "AS64496",
			CountryCode:        "aa",
		}, keyHexes)
		if err != nil {
			t.Fatalf("%s: %s", c.name, err)
		}
		if result.Probation != c.probation {
			t.Errorf("%s: probation = %t, expected %t", c.name, result.Probation, c.probation)
		}
		if len(result.KeyHexes) != c.expect || result.Count != c.expect {
			t.Errorf("%s: released %d (count %d), expected %d", c.name, len(result.KeyHexes), result.Count, c.expect)
		}
	}
}

// A record is released to at most the cap of distinct identities per country
// (Q4, Salmon), counted per country and never against the identity's own
// earlier release (Q3).
func TestExtenderReleaseCapsClientsPerExtenderPerCountry(t *testing.T) {
	clock := newTestClock()
	policy := newTestReleasePolicy(t, clock, nil, func(settings *ExtenderReleaseSettings) {
		settings.MaxClientsPerExtenderPerCountry = 2
	})
	// one record: one partition, so every identity is dealt it
	keyHexes := testGatedKeyHexes(t, 1)
	request := func(identity string, countryCode string) *ExtenderReleaseRequest {
		return &ExtenderReleaseRequest{
			Identity:           []byte(identity),
			IdentityCreateTime: clock.Now().Add(-time.Hour),
			Vantage:            "AS64496",
			CountryCode:        countryCode,
		}
	}
	release := func(identity string, countryCode string) []string {
		result, err := policy.Release(request(identity, countryCode), keyHexes)
		if err != nil {
			t.Fatalf("%s in %s: %s", identity, countryCode, err)
		}
		return result.KeyHexes
	}
	if released := release("first", "aa"); !slices.Equal(released, keyHexes) {
		t.Fatalf("the first identity got %v", released)
	}
	if released := release("second", "aa"); !slices.Equal(released, keyHexes) {
		t.Fatalf("the second identity got %v", released)
	}
	if released := release("third", "aa"); len(released) != 0 {
		t.Fatalf("the third identity in the country got %v, expected nothing: the record is at its cap", released)
	}
	if released := release("fourth", "bb"); !slices.Equal(released, keyHexes) {
		t.Fatalf("an identity in another country got %v", released)
	}
	// the first identity's own release takes no slot from itself, in this
	// epoch or the next
	if released := release("first", "aa"); !slices.Equal(released, keyHexes) {
		t.Fatalf("the first identity's repeat got %v", released)
	}
	clock.advance(ExtenderReleaseEpochTimeout)
	if released := release("first", "aa"); !slices.Equal(released, keyHexes) {
		t.Fatalf("the first identity's next epoch got %v", released)
	}
	if released := release("third", "aa"); len(released) != 0 {
		t.Fatalf("the third identity got %v in the next epoch", released)
	}
	// once the client window has passed the slots are free
	clock.advance(ExtenderReleaseClientWindow)
	if released := release("third", "aa"); !slices.Equal(released, keyHexes) {
		t.Fatalf("the third identity got %v after the client window", released)
	}
}

// A blocked source for the policy: the keys blocked per country.
type testBlockedSource map[string]map[string]bool

func (self testBlockedSource) Blocked(keyHex string, countryCode string) bool {
	return self[countryCode][keyHex]
}

// A record blocked in the requester's country is not released there, and is
// elsewhere (Q4).
func TestExtenderReleaseSkipsARecordBlockedInTheCountry(t *testing.T) {
	clock := newTestClock()
	keyHexes := testGatedKeyHexes(t, 1)
	blocked := testBlockedSource{"aa": {keyHexes[0]: true}}
	policy := newTestReleasePolicy(t, clock, blocked, nil)
	request := func(identity string, countryCode string) *ExtenderReleaseRequest {
		return &ExtenderReleaseRequest{
			Identity:           []byte(identity),
			IdentityCreateTime: clock.Now().Add(-time.Hour),
			Vantage:            "AS64496",
			CountryCode:        countryCode,
		}
	}
	result, err := policy.Release(request("a", "aa"), keyHexes)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.KeyHexes) != 0 {
		t.Fatalf("a record blocked in the country was released there: %v", result.KeyHexes)
	}
	result, err = policy.Release(request("b", "bb"), keyHexes)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(result.KeyHexes, keyHexes) {
		t.Fatalf("the record was not released in another country: %v", result.KeyHexes)
	}
	// an eligibility filter is honored the same way
	result, err = policy.Release(&ExtenderReleaseRequest{
		Identity:           []byte("c"),
		IdentityCreateTime: clock.Now().Add(-time.Hour),
		Vantage:            "AS64496",
		CountryCode:        "bb",
		Eligible:           func(string) bool { return false },
	}, keyHexes)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.KeyHexes) != 0 {
		t.Fatalf("an ineligible record was released: %v", result.KeyHexes)
	}
}

// The memory ledger forgets the oldest identity, vantage and record beyond
// its bounds, and nothing within them (Q3).
func TestExtenderReleaseMemoryLedgerIsBounded(t *testing.T) {
	ledger := NewExtenderReleaseMemoryLedger(time.Hour, time.Hour)
	ledger.SetBounds(2, 2, 2)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range 3 {
		ledger.RecordRequest([]byte(fmt.Sprintf("identity-%d", i)), fmt.Sprintf("vantage-%d", i), now.Add(time.Duration(i)*time.Minute))
		ledger.RecordRelease([]byte("identity"), fmt.Sprintf("%02x", i), "aa", 0, now.Add(time.Duration(i)*time.Minute))
	}
	since := now.Add(-time.Hour)
	if identityCount, vantageCount := ledger.RequestCounts([]byte("identity-0"), "vantage-0", since); identityCount != 0 || vantageCount != 0 {
		t.Errorf("the oldest identity and vantage were kept: %d, %d", identityCount, vantageCount)
	}
	for i := 1; i < 3; i += 1 {
		if identityCount, vantageCount := ledger.RequestCounts([]byte(fmt.Sprintf("identity-%d", i)), fmt.Sprintf("vantage-%d", i), since); identityCount != 1 || vantageCount != 1 {
			t.Errorf("identity %d was forgotten: %d, %d", i, identityCount, vantageCount)
		}
	}
	if count := ledger.ClientCount("00", "aa", []byte("other"), since); count != 0 {
		t.Errorf("the oldest record was kept: %d", count)
	}
	if count := ledger.ClientCount("02", "aa", []byte("other"), since); count != 1 {
		t.Errorf("the newest record was forgotten: %d", count)
	}
	// and a count never includes the identity itself
	if count := ledger.ClientCount("02", "aa", []byte("identity"), since); count != 0 {
		t.Errorf("the identity counts against itself: %d", count)
	}
}

// Blocked state is per country, needs enough distinct reporters within the
// window, and needs the operator's own probe to have reached the record, so
// an outage is not a block (Q4).
func TestExtenderBlockedStateIsPerCountryAndNeedsAProbe(t *testing.T) {
	clock := newTestClock()
	settings := DefaultExtenderBlockedStateSettings()
	settings.Now = clock.Now
	state := NewExtenderBlockedState(settings)
	keyHex := hex.EncodeToString(newTestExtenderKey(t))

	// three reports from one reporter are one report
	for range 3 {
		state.Report(keyHex, "aa", []byte("reporter-1"))
	}
	state.ProbeSucceeded(keyHex)
	if state.Blocked(keyHex, "aa") {
		t.Fatal("one reporter made a block")
	}
	state.Report(keyHex, "aa", []byte("reporter-2"))
	if state.Blocked(keyHex, "aa") {
		t.Fatal("two reporters made a block")
	}
	state.Report(keyHex, "aa", []byte("reporter-3"))
	if !state.Blocked(keyHex, "aa") {
		t.Fatal("three reporters and a probe did not make a block")
	}
	if state.Blocked(keyHex, "bb") {
		t.Fatal("a block in one country is a block in another")
	}
	if blocked := state.BlockedKeyHexes("aa"); !slices.Equal(blocked, []string{keyHex}) {
		t.Fatalf("blocked in aa = %v", blocked)
	}
	if blocked := state.BlockedKeyHexes("bb"); len(blocked) != 0 {
		t.Fatalf("blocked in bb = %v", blocked)
	}

	// the probe ages out: the record may simply be down now
	clock.advance(settings.ProbeWindow)
	if state.Blocked(keyHex, "aa") {
		t.Fatal("a block stood without a recent probe")
	}
	state.ProbeSucceeded(keyHex)
	if !state.Blocked(keyHex, "aa") {
		t.Fatal("a fresh probe did not restore the block")
	}
	// the reports age out
	clock.advance(settings.ReportWindow)
	state.ProbeSucceeded(keyHex)
	if state.Blocked(keyHex, "aa") {
		t.Fatal("a block stood on reports older than the window")
	}

	// a record never probed is never blocked, however many report it
	otherKeyHex := hex.EncodeToString(newTestExtenderKey(t))
	for i := range 8 {
		state.Report(otherKeyHex, "aa", []byte(fmt.Sprintf("reporter-%d", i)))
	}
	if state.Blocked(otherKeyHex, "aa") {
		t.Fatal("a record the operator never reached is blocked")
	}
}

// The reporter and entry tables are bounded: the oldest goes (Q4).
func TestExtenderBlockedStateIsBounded(t *testing.T) {
	clock := newTestClock()
	settings := DefaultExtenderBlockedStateSettings()
	settings.Now = clock.Now
	settings.ReportThreshold = 2
	settings.MaxReporterCount = 2
	settings.MaxEntryCount = 2
	state := NewExtenderBlockedState(settings)
	keyHexes := testGatedKeyHexes(t, 3)

	// reporters beyond the cap push the oldest out, so the count never
	// exceeds the cap and a block still needs the threshold among those kept
	for i := range 3 {
		state.Report(keyHexes[0], "aa", []byte(fmt.Sprintf("reporter-%d", i)))
		clock.advance(time.Second)
	}
	state.ProbeSucceeded(keyHexes[0])
	if !state.Blocked(keyHexes[0], "aa") {
		t.Fatal("two kept reporters did not make a block")
	}
	// entries beyond the cap push the oldest out
	state.Report(keyHexes[1], "aa", []byte("reporter-0"))
	state.Report(keyHexes[1], "aa", []byte("reporter-1"))
	state.ProbeSucceeded(keyHexes[1])
	clock.advance(time.Second)
	state.Report(keyHexes[2], "aa", []byte("reporter-0"))
	if state.Blocked(keyHexes[0], "aa") {
		t.Fatal("the oldest entry was kept past the cap")
	}
	if !state.Blocked(keyHexes[1], "aa") {
		t.Fatal("a newer entry was evicted")
	}
}

// Root cause: a leak cannot be attributed because nothing is unique to a
// partition. A canary is in exactly one place, so a blocked canary names
// exactly that place, and a blocked record that is no canary names none (Q4).
func TestExtenderBlockedCanaryMapsToExactlyOnePlacement(t *testing.T) {
	gatedKeyHexes := testGatedKeyHexes(t, 64)
	gatedPartitionCount := ExtenderPartitionCount(len(gatedKeyHexes))
	// two gated canaries in different partitions, found deterministically
	// under the pinned secret
	gatedA := gatedKeyHexes[0]
	partitionA := ExtenderRecordPartition(testPartitionSecret, ExtenderChannelGated, gatedA, gatedPartitionCount)
	gatedB := ""
	for _, keyHex := range gatedKeyHexes[1:] {
		if ExtenderRecordPartition(testPartitionSecret, ExtenderChannelGated, keyHex, gatedPartitionCount) != partitionA {
			gatedB = keyHex
			break
		}
	}
	if gatedB == "" {
		t.Fatal("every gated key fell in one partition")
	}
	dnsEu := hex.EncodeToString(newTestExtenderKey(t))
	dnsNa := hex.EncodeToString(newTestExtenderKey(t))
	plain := hex.EncodeToString(newTestExtenderKey(t))
	canaries := []*ExtenderCanary{
		{KeyHex: dnsEu, Channel: ExtenderChannelDns, Region: "EU"},
		{KeyHex: dnsNa, Channel: ExtenderChannelDns, Region: "NA"},
		{KeyHex: gatedA, Channel: ExtenderChannelGated},
		{KeyHex: gatedB, Channel: ExtenderChannelGated},
	}
	attribute := func(blockedKeyHexes ...string) []string {
		placements := []string{}
		for _, placement := range ExtenderAttributeBlockedCanaries(testPartitionSecret, canaries, blockedKeyHexes, gatedPartitionCount) {
			placements = append(placements, placement.String())
		}
		return placements
	}
	if placements := attribute(gatedA); !slices.Equal(placements, []string{fmt.Sprintf("gated:%d", partitionA)}) {
		t.Fatalf("a blocked gated canary attributed %v, expected its one partition", placements)
	}
	if placements := attribute(dnsEu); !slices.Equal(placements, []string{"dns:EU"}) {
		t.Fatalf("a blocked dns canary attributed %v, expected its one region", placements)
	}
	if placements := attribute(plain); len(placements) != 0 {
		t.Fatalf("a blocked record that is no canary attributed %v", placements)
	}
	if placements := attribute(dnsEu, gatedA, plain); len(placements) != 2 {
		t.Fatalf("two blocked canaries attributed %v", placements)
	}
	if placements := attribute(gatedA, gatedB); len(placements) != 2 || placements[0] == placements[1] {
		t.Fatalf("two gated canaries of different partitions attributed %v", placements)
	}
	// a canary is never two places: a gated one has no region and a dns one
	// no partition
	if placement, ok := ExtenderCanaryPlace(testPartitionSecret, canaries[2], gatedPartitionCount); !ok || placement.Region != "" {
		t.Errorf("a gated canary has region %q", placement.Region)
	}
	if placement, ok := ExtenderCanaryPlace(testPartitionSecret, canaries[0], gatedPartitionCount); !ok || placement.Partition != -1 {
		t.Errorf("a dns canary has partition %d", placement.Partition)
	}
	if _, ok := ExtenderCanaryPlace(testPartitionSecret, &ExtenderCanary{KeyHex: plain}, gatedPartitionCount); ok {
		t.Error("a canary with no channel was placed")
	}
}

// The gated-preferred connection policy (EXTENDER-UPDATE2.md §1.1, §1.3,
// §1.4): what the connection draw hands the strategy in each mode, the
// bounded fallback to verified open records, recovery, and what never becomes
// a candidate or leaves on a public path. The clock is the fake one; the
// strategy-level dial order is pinned in net_extender_directory_gated_test.go.

// The budget the policy tests run with.
const testGatedPreferenceAttemptTimeout = 10 * time.Second

// A directory with the gated preference on and the fake clock.
func newTestGatedPreferenceDirectory(t *testing.T, clock *testClock) (*ExtenderDirectory, ed25519.PrivateKey) {
	t.Helper()
	directory, rootPrivateKey := newTestExtenderDirectory(t, clock, func(settings *ExtenderDirectorySettings) {
		settings.PartitionSecret = testPartitionSecret
	})
	directory.SetGatedPreference(&ExtenderGatedPreferenceSettings{
		Enabled:        true,
		AttemptTimeout: testGatedPreferenceAttemptTimeout,
	})
	return directory, rootPrivateKey
}

// Releases one gated record of `extenderKey` at `ips` as the release fetch
// applies it, issued now.
func applyTestGatedRelease(
	t *testing.T,
	directory *ExtenderDirectory,
	rootPrivateKey ed25519.PrivateKey,
	clock *testClock,
	extenderKey ed25519.PublicKey,
	ips ...string,
) {
	t.Helper()
	addresses := []*protocol.ExtenderAddress{}
	for _, ip := range ips {
		addresses = append(addresses, testExtenderAddress(ip))
	}
	record, err := SignExtenderRecord(rootPrivateKey, &protocol.ExtenderRecordBody{
		PublicKey:     extenderKey,
		Addresses:     addresses,
		TcpPort:       443,
		UdpPort:       443,
		DnsPort:       53,
		DnsTld:        "x.example.",
		CountryCode:   "us",
		IssueTimeMs:   uint64(clock.Now().UnixMilli()),
		ExpireTimeMs:  uint64(clock.Now().Add(24 * time.Hour).UnixMilli()),
		NetworkHost:   testExtenderNetworkHost,
		DirectoryTier: ExtenderDirectoryTierGated,
	})
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := directory.ApplyReleaseRecord(record, directory.ResetGeneration()); err != nil || !changed {
		t.Fatalf("the released record was not applied: changed=%t err=%v", changed, err)
	}
}

// Applies one open record at `ip` from the feed and returns its key.
func applyTestOpenTierRecord(
	t *testing.T,
	directory *ExtenderDirectory,
	rootPrivateKey ed25519.PrivateKey,
	clock *testClock,
	ip string,
) ed25519.PublicKey {
	t.Helper()
	extenderKey := newTestExtenderKey(t)
	record := signTestTierRecord(t, rootPrivateKey, extenderKey, clock.Now(), ExtenderDirectoryTierOpen, ip)
	if _, err := directory.ApplyRecord(record, ExtenderSourceFeed); err != nil {
		t.Fatal(err)
	}
	return extenderKey
}

// The addresses of one connection draw of one family, in order.
func testConnectDrawIps(directory *ExtenderDirectory, ipVersion int) []string {
	ips := []string{}
	for _, candidate := range directory.ConnectCandidates([]int{ipVersion}, 64, false, nil)[0] {
		ips = append(ips, candidate.Ip.String())
	}
	return ips
}

// With the preference off, which is the default, the connection draw is the
// candidate order every caller had: gated records rank as any other (§1.1 is
// a cohort setting, not a change for everyone).
func TestExtenderGatedPreferenceOffKeepsTheCandidateOrder(t *testing.T) {
	clock := newTestClock()
	directory, rootPrivateKey := newTestExtenderDirectory(t, clock, nil)
	if directory.GatedPreferenceEnabled() {
		t.Fatal("the gated preference is on by default")
	}
	applyTestOpenTierRecord(t, directory, rootPrivateKey, clock, "198.51.100.1")
	applyTestGatedRelease(t, directory, rootPrivateKey, clock, newTestExtenderKey(t), "192.0.2.10")
	applyTestOpenTierRecord(t, directory, rootPrivateKey, clock, "198.51.100.2")
	directory.AddManual(netip.MustParseAddr("203.0.113.5"))
	directory.RecordSuccess(netip.MustParseAddr("198.51.100.2"), ExtenderConnectModeTcpTls)

	for _, verifiedOnly := range []bool{false, true} {
		expected := directory.Candidates(4, 64)
		if verifiedOnly {
			expected = directory.VerifiedCandidates(4, 64)
		}
		drawn := directory.ConnectCandidates([]int{4, 6}, 64, verifiedOnly, nil)
		if len(drawn) != 2 || len(drawn[1]) != 0 {
			t.Fatalf("verifiedOnly=%t: the draw has %d families, %v", verifiedOnly, len(drawn), drawn)
		}
		if len(drawn[0]) != len(expected) {
			t.Fatalf("verifiedOnly=%t: drew %d candidates, expected %d", verifiedOnly, len(drawn[0]), len(expected))
		}
		for i := range expected {
			if drawn[0][i].Ip != expected[i].Ip {
				t.Fatalf("verifiedOnly=%t: candidate %d is %s, expected %s", verifiedOnly, i, drawn[0][i].Ip, expected[i].Ip)
			}
		}
	}
	status := directory.GatedPreference()
	if status.Mode != ExtenderGatedModeOff || 0 < len(status.Stats.DrawCounts) {
		t.Fatalf("the preference judged a draw while off: %+v", status)
	}
	for _, ip := range []string{"198.51.100.1", "198.51.100.2", "192.0.2.10"} {
		if !directory.ConnectAddressUsable(netip.MustParseAddr(ip)) {
			t.Fatalf("%s is not usable while the preference is off", ip)
		}
	}
}

// A usable gated record is drawn alone, ahead of the manual addresses the user
// configured, and the open records are held back from new draws; an open
// dialer the strategy already holds keeps serving until the gated record
// connects, and then the policy is gated and the open dialers drain (§1.1
// rules 1 and 5).
func TestExtenderGatedPreferenceDrawsAUsableGatedRecordAlone(t *testing.T) {
	clock := newTestClock()
	directory, rootPrivateKey := newTestGatedPreferenceDirectory(t, clock)
	applyTestOpenTierRecord(t, directory, rootPrivateKey, clock, "198.51.100.1")
	applyTestOpenTierRecord(t, directory, rootPrivateKey, clock, "198.51.100.2")
	applyTestGatedRelease(t, directory, rootPrivateKey, clock, newTestExtenderKey(t), "192.0.2.10")
	directory.AddManual(netip.MustParseAddr("203.0.113.5"))

	if drawn := testConnectDrawIps(directory, 4); !slices.Equal(drawn, []string{"192.0.2.10", "203.0.113.5"}) {
		t.Fatalf("the attempt drew %v, expected the gated record and then the manual address", drawn)
	}
	if mode := directory.GatedPreference().Mode; mode != ExtenderGatedModeAttempt {
		t.Fatalf("mode %s, expected an attempt", mode)
	}
	// an open dialer the strategy already holds is not drained by an attempt
	// alone: no gated record has connected yet
	if !directory.ConnectAddressUsable(netip.MustParseAddr("198.51.100.1")) {
		t.Fatal("an open dialer drained before any gated record connected")
	}
	if !directory.ConnectAddressUsable(netip.MustParseAddr("192.0.2.10")) || !directory.ConnectAddressUsable(netip.MustParseAddr("203.0.113.5")) {
		t.Fatal("the gated record or the manual address is not usable during a gated attempt")
	}

	directory.RecordSuccess(netip.MustParseAddr("192.0.2.10"), ExtenderConnectModeTcpTls)
	clock.advance(time.Hour)
	if drawn := testConnectDrawIps(directory, 4); !slices.Equal(drawn, []string{"192.0.2.10", "203.0.113.5"}) {
		t.Fatalf("the gated policy drew %v", drawn)
	}
	status := directory.GatedPreference()
	if status.Mode != ExtenderGatedModeGated || status.FallbackReason != "" {
		t.Fatalf("mode %s reason %q, expected gated: its budget no longer applies once a gated record connected", status.Mode, status.FallbackReason)
	}
	if status.Stats.CandidateCounts[ExtenderGatedClassOpen] != 0 || status.Stats.CandidateCounts[ExtenderGatedClassGated] != 2 {
		t.Fatalf("the draws handed out %v", status.Stats.CandidateCounts)
	}
	if directory.ConnectAddressUsable(netip.MustParseAddr("198.51.100.2")) {
		t.Fatal("an open dialer is kept once gated connected")
	}
	// the manual address the user configured is never drained
	if !directory.ConnectAddressUsable(netip.MustParseAddr("203.0.113.5")) {
		t.Fatal("the manual address drained once gated connected")
	}
}

// The gated attempt has a bounded budget: at its end, or at once when every
// gated record has failed, verified open records are drawn after the gated
// ones (§1.1 rule 3). Every draw in the window shares one budget.
func TestExtenderGatedPreferenceFallsBackWithinTheAttemptBudget(t *testing.T) {
	t.Run("budget", func(t *testing.T) {
		clock := newTestClock()
		directory, rootPrivateKey := newTestGatedPreferenceDirectory(t, clock)
		applyTestGatedRelease(t, directory, rootPrivateKey, clock, newTestExtenderKey(t), "192.0.2.10")
		applyTestOpenTierRecord(t, directory, rootPrivateKey, clock, "198.51.100.1")

		for elapsed := time.Duration(0); elapsed < testGatedPreferenceAttemptTimeout; elapsed += time.Second {
			if drawn := testConnectDrawIps(directory, 4); !slices.Equal(drawn, []string{"192.0.2.10"}) {
				t.Fatalf("at %s the attempt drew %v", elapsed, drawn)
			}
			clock.advance(time.Second)
		}
		if drawn := testConnectDrawIps(directory, 4); !slices.Equal(drawn, []string{"192.0.2.10", "198.51.100.1"}) {
			t.Fatalf("at the budget the fallback drew %v, expected gated first and then open", drawn)
		}
		status := directory.GatedPreference()
		if status.Mode != ExtenderGatedModeFallback || status.FallbackReason != ExtenderGatedFallbackGatedTimeout {
			t.Fatalf("mode %s reason %q, expected the timeout fallback", status.Mode, status.FallbackReason)
		}
		if status.Stats.FallbackCounts[ExtenderGatedFallbackGatedTimeout] != 1 {
			t.Fatalf("fallback counts %v", status.Stats.FallbackCounts)
		}
		if !directory.ConnectAddressUsable(netip.MustParseAddr("198.51.100.1")) {
			t.Fatal("the open record is not usable in the fallback")
		}
	})
	t.Run("failed", func(t *testing.T) {
		clock := newTestClock()
		directory, rootPrivateKey := newTestGatedPreferenceDirectory(t, clock)
		applyTestGatedRelease(t, directory, rootPrivateKey, clock, newTestExtenderKey(t), "192.0.2.10")
		applyTestOpenTierRecord(t, directory, rootPrivateKey, clock, "198.51.100.1")
		testConnectDrawIps(directory, 4)
		directory.RecordFailure(netip.MustParseAddr("192.0.2.10"), ExtenderConnectModeTcpTls)
		if drawn := testConnectDrawIps(directory, 4); !slices.Equal(drawn, []string{"198.51.100.1"}) {
			t.Fatalf("after the gated record failed the draw was %v, expected the open record at once", drawn)
		}
		if reason := directory.GatedPreference().FallbackReason; reason != ExtenderGatedFallbackGatedFailed {
			t.Fatalf("reason %q, expected gated_failed", reason)
		}
	})
}

// An empty, refused, timed-out or failed release leaves no usable gated record
// and falls back to the verified open records at the first draw, the reason
// naming the release's answer; an outcome the client does not know is empty,
// and a release route that answered ok with nothing still falls back
// (§1.1 rule 3, contract R).
func TestExtenderGatedPreferenceFallsBackOnAnUnusableRelease(t *testing.T) {
	cases := []struct {
		outcome string
		reason  string
	}{
		{outcome: "", reason: ExtenderGatedFallbackNoRecord},
		{outcome: "ok", reason: ExtenderGatedFallbackNoRecord},
		{outcome: "empty", reason: ExtenderGatedFallbackReleaseEmpty},
		{outcome: "epoch_spent", reason: ExtenderGatedFallbackReleaseEpochSpent},
		{outcome: "probation", reason: ExtenderGatedFallbackReleaseProbation},
		{outcome: "country_capacity", reason: ExtenderGatedFallbackReleaseCountryCapacity},
		{outcome: "Rate_Limited ", reason: ExtenderGatedFallbackReleaseRateLimited},
		{outcome: "incompatible", reason: ExtenderGatedFallbackReleaseIncompatible},
		{outcome: "unavailable", reason: ExtenderGatedFallbackReleaseUnavailable},
		{outcome: "failed", reason: ExtenderGatedFallbackReleaseFailed},
		{outcome: "a_newer_outcome", reason: ExtenderGatedFallbackReleaseEmpty},
	}
	for _, c := range cases {
		clock := newTestClock()
		directory, rootPrivateKey := newTestGatedPreferenceDirectory(t, clock)
		applyTestOpenTierRecord(t, directory, rootPrivateKey, clock, "198.51.100.1")
		if c.outcome != "" {
			directory.SetGatedReleaseOutcome(c.outcome)
		}
		if drawn := testConnectDrawIps(directory, 4); !slices.Equal(drawn, []string{"198.51.100.1"}) {
			t.Fatalf("outcome %q: the first draw was %v, expected the open record at once", c.outcome, drawn)
		}
		status := directory.GatedPreference()
		if status.Mode != ExtenderGatedModeFallback || status.FallbackReason != c.reason {
			t.Fatalf("outcome %q: mode %s reason %q, expected %q", c.outcome, status.Mode, status.FallbackReason, c.reason)
		}
		if !slices.Contains(ExtenderGatedFallbackReasons(), status.FallbackReason) {
			t.Fatalf("outcome %q: reason %q is not in the fixed label set", c.outcome, status.FallbackReason)
		}
		if c.outcome != "" && !slices.Contains(ExtenderGatedReleaseOutcomes(), status.ReleaseOutcome) {
			t.Fatalf("outcome %q was counted as %q, outside the fixed label set", c.outcome, status.ReleaseOutcome)
		}
	}
}

// A revoked gated record is never a candidate, in an attempt or a fallback,
// and a release record that is not a current gated record of this space, or
// that was fetched before a reset, is never applied (§1.4).
func TestExtenderGatedPreferenceNeverDrawsARevokedOrDisallowedRecord(t *testing.T) {
	clock := newTestClock()
	directory, rootPrivateKey := newTestGatedPreferenceDirectory(t, clock)
	revokedKey := newTestExtenderKey(t)
	applyTestGatedRelease(t, directory, rootPrivateKey, clock, revokedKey, "192.0.2.10")
	applyTestOpenTierRecord(t, directory, rootPrivateKey, clock, "198.51.100.1")
	clock.advance(time.Minute)
	if _, err := directory.ApplyRevocation(signTestRevocation(t, rootPrivateKey, revokedKey, clock.Now())); err != nil {
		t.Fatal(err)
	}

	gatedBody := func(ip string, issueTime time.Time, expireTime time.Time) *protocol.ExtenderRecordBody {
		return &protocol.ExtenderRecordBody{
			PublicKey:     newTestExtenderKey(t),
			Addresses:     []*protocol.ExtenderAddress{testExtenderAddress(ip)},
			TcpPort:       443,
			IssueTimeMs:   uint64(issueTime.UnixMilli()),
			ExpireTimeMs:  uint64(expireTime.UnixMilli()),
			NetworkHost:   testExtenderNetworkHost,
			DirectoryTier: ExtenderDirectoryTierGated,
		}
	}
	sign := func(privateKey ed25519.PrivateKey, body *protocol.ExtenderRecordBody) *protocol.ExtenderRecord {
		record, err := SignExtenderRecord(privateKey, body)
		if err != nil {
			t.Fatal(err)
		}
		return record
	}
	otherRootSeed, err := NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	otherRoot, err := ExtenderPrivateKeyFromSeed(otherRootSeed)
	if err != nil {
		t.Fatal(err)
	}
	now := clock.Now()
	openBody := gatedBody("192.0.2.21", now, now.Add(24*time.Hour))
	openBody.DirectoryTier = ExtenderDirectoryTierOpen
	foreignBody := gatedBody("192.0.2.22", now, now.Add(24*time.Hour))
	foreignBody.NetworkHost = "other.example"
	disallowed := map[string]*protocol.ExtenderRecord{
		"open tier":     sign(rootPrivateKey, openBody),
		"foreign host":  sign(rootPrivateKey, foreignBody),
		"another root":  sign(otherRoot, gatedBody("192.0.2.23", now, now.Add(24*time.Hour))),
		"already spent": sign(rootPrivateKey, gatedBody("192.0.2.24", now.Add(-48*time.Hour), now.Add(-24*time.Hour))),
	}
	for name, record := range disallowed {
		if changed, err := directory.ApplyReleaseRecord(record, directory.ResetGeneration()); err == nil || changed {
			t.Fatalf("%s: a disallowed release record was applied", name)
		}
	}
	// a release requested before a reset is dropped when it lands after it
	staleGeneration := directory.ResetGeneration()
	directory.Reset(directory.RootKeys())
	applyTestOpenTierRecord(t, directory, rootPrivateKey, clock, "198.51.100.1")
	if changed, err := directory.ApplyReleaseRecord(sign(rootPrivateKey, gatedBody("192.0.2.25", now, now.Add(24*time.Hour))), staleGeneration); err == nil || changed {
		t.Fatal("a release fetched before the reset was applied after it")
	}
	// the revoked record went with the reset; revoke one again for the draws
	applyTestGatedRelease(t, directory, rootPrivateKey, clock, revokedKey, "192.0.2.10")
	clock.advance(time.Minute)
	if _, err := directory.ApplyRevocation(signTestRevocation(t, rootPrivateKey, revokedKey, clock.Now())); err != nil {
		t.Fatal(err)
	}

	for range 3 {
		for _, ip := range testConnectDrawIps(directory, 4) {
			if ip != "198.51.100.1" {
				t.Fatalf("the draw offered %s, which is revoked or was never applied", ip)
			}
		}
		clock.advance(testGatedPreferenceAttemptTimeout)
	}
	if reason := directory.GatedPreference().FallbackReason; reason != ExtenderGatedFallbackNoRecord {
		t.Fatalf("reason %q, expected no usable gated record", reason)
	}

	// an expired gated record is not drawn while gated is preferred, and in the
	// fallback it is only the last resort it always was
	expiringKey := newTestExtenderKey(t)
	applyTestGatedRelease(t, directory, rootPrivateKey, clock, expiringKey, "192.0.2.30")
	if drawn := testConnectDrawIps(directory, 4); !slices.Equal(drawn, []string{"192.0.2.30"}) {
		t.Fatalf("a newly released record started no attempt: %v", drawn)
	}
	clock.advance(25 * time.Hour)
	applyTestOpenTierRecord(t, directory, rootPrivateKey, clock, "198.51.100.2")
	drawn := testConnectDrawIps(directory, 4)
	if 0 < len(drawn) && drawn[0] == "192.0.2.30" {
		t.Fatalf("an expired gated record leads the draw: %v", drawn)
	}
	if !slices.Contains(drawn, "198.51.100.2") {
		t.Fatalf("the fallback after the gated record expired drew %v", drawn)
	}
}

// The fallback gives way to gated again once a gated record connects, or one
// becomes newly usable -- released since, or its hold lapsed -- and the open
// dialers then drain (§1.1 rule 5).
func TestExtenderGatedPreferenceRecoversAndDrainsTheOpenRecords(t *testing.T) {
	clock := newTestClock()
	directory, rootPrivateKey := newTestGatedPreferenceDirectory(t, clock)
	gatedIp := netip.MustParseAddr("192.0.2.10")
	openIp := netip.MustParseAddr("198.51.100.1")
	applyTestGatedRelease(t, directory, rootPrivateKey, clock, newTestExtenderKey(t), gatedIp.String())
	applyTestOpenTierRecord(t, directory, rootPrivateKey, clock, openIp.String())

	testConnectDrawIps(directory, 4)
	directory.RecordFailure(gatedIp, ExtenderConnectModeTcpTls)
	if drawn := testConnectDrawIps(directory, 4); !slices.Equal(drawn, []string{openIp.String()}) {
		t.Fatalf("the fallback drew %v", drawn)
	}
	// the fallback holds while the gated record is held
	clock.advance(5 * time.Minute)
	if drawn := testConnectDrawIps(directory, 4); !slices.Equal(drawn, []string{openIp.String()}) {
		t.Fatalf("the held fallback drew %v", drawn)
	}
	// its hold lapses: gated is tried again, with a fresh budget; the open
	// dialers keep serving until it connects, and drain once it has
	clock.advance(6 * time.Minute)
	if drawn := testConnectDrawIps(directory, 4); !slices.Equal(drawn, []string{gatedIp.String()}) {
		t.Fatalf("the recovery drew %v", drawn)
	}
	status := directory.GatedPreference()
	if status.Mode != ExtenderGatedModeAttempt || status.Stats.RecoveryCount != 1 {
		t.Fatalf("mode %s recoveries %d, expected a new attempt", status.Mode, status.Stats.RecoveryCount)
	}
	if !directory.ConnectAddressUsable(openIp) {
		t.Fatal("the open dialers drained before the gated record connected again")
	}
	directory.RecordSuccess(gatedIp, ExtenderConnectModeTcpTls)
	if directory.ConnectAddressUsable(openIp) {
		t.Fatal("the open dialers do not drain once the gated record connected again")
	}
	testConnectDrawIps(directory, 4)
	if mode := directory.GatedPreference().Mode; mode != ExtenderGatedModeGated {
		t.Fatalf("mode %s after the gated record connected", mode)
	}

	// a cold fallback ends when a release brings a record
	clock = newTestClock()
	directory, rootPrivateKey = newTestGatedPreferenceDirectory(t, clock)
	applyTestOpenTierRecord(t, directory, rootPrivateKey, clock, openIp.String())
	if drawn := testConnectDrawIps(directory, 4); !slices.Equal(drawn, []string{openIp.String()}) {
		t.Fatalf("the cold draw was %v", drawn)
	}
	applyTestGatedRelease(t, directory, rootPrivateKey, clock, newTestExtenderKey(t), gatedIp.String())
	if drawn := testConnectDrawIps(directory, 4); !slices.Equal(drawn, []string{gatedIp.String()}) {
		t.Fatalf("the first draw after the release was %v", drawn)
	}

	// and a gated record that connects in the fallback -- a probe, a dial the
	// fallback drew it for -- ends it at once
	clock = newTestClock()
	directory, rootPrivateKey = newTestGatedPreferenceDirectory(t, clock)
	applyTestGatedRelease(t, directory, rootPrivateKey, clock, newTestExtenderKey(t), gatedIp.String())
	applyTestOpenTierRecord(t, directory, rootPrivateKey, clock, openIp.String())
	testConnectDrawIps(directory, 4)
	clock.advance(testGatedPreferenceAttemptTimeout)
	if drawn := testConnectDrawIps(directory, 4); !slices.Equal(drawn, []string{gatedIp.String(), openIp.String()}) {
		t.Fatalf("the timeout fallback drew %v", drawn)
	}
	clock.advance(time.Second)
	directory.RecordSuccess(gatedIp, ExtenderConnectModeQuic)
	if drawn := testConnectDrawIps(directory, 4); !slices.Equal(drawn, []string{gatedIp.String()}) {
		t.Fatalf("the draw after the gated record connected was %v", drawn)
	}
	if mode := directory.GatedPreference().Mode; mode != ExtenderGatedModeGated {
		t.Fatalf("mode %s, expected gated", mode)
	}
}

// The open dialers drain only while a gated record that connected is still
// usable: one held or expired since the last draw -- a probe failed it, its
// refresh is late -- falls the policy back before an open dialer that carries
// the client is drained, with no draw in between, and an expired gated record
// keeps the last-resort rule of AddressUsable (§1.1 rules 3 and 5).
func TestExtenderGatedPreferenceDrainsOnlyWhileAGatedRecordIsUsable(t *testing.T) {
	for _, lapse := range []string{"held", "expired"} {
		clock := newTestClock()
		directory, rootPrivateKey := newTestGatedPreferenceDirectory(t, clock)
		gatedIp := netip.MustParseAddr("192.0.2.10")
		openIp := netip.MustParseAddr("198.51.100.1")
		applyTestGatedRelease(t, directory, rootPrivateKey, clock, newTestExtenderKey(t), gatedIp.String())
		// the open record is issued later, so it is current when the gated one
		// has expired
		clock.advance(12 * time.Hour)
		applyTestOpenTierRecord(t, directory, rootPrivateKey, clock, openIp.String())
		testConnectDrawIps(directory, 4)
		directory.RecordSuccess(gatedIp, ExtenderConnectModeTcpTls)
		if directory.ConnectAddressUsable(openIp) {
			t.Fatalf("%s: an open dialer was kept once gated connected", lapse)
		}

		reason := ""
		switch lapse {
		case "held":
			directory.RecordFailure(gatedIp, ExtenderConnectModeTcpTls)
			reason = ExtenderGatedFallbackGatedFailed
		case "expired":
			clock.advance(13 * time.Hour)
			reason = ExtenderGatedFallbackNoRecord
		}
		if !directory.ConnectAddressUsable(openIp) {
			t.Fatalf("%s: the open dialer drained while no gated record was usable", lapse)
		}
		status := directory.GatedPreference()
		if status.Mode != ExtenderGatedModeFallback || status.FallbackReason != reason {
			t.Fatalf("%s: mode %s reason %q, expected the fallback for %q", lapse, status.Mode, status.FallbackReason, reason)
		}
		if lapse == "expired" && !directory.ConnectAddressUsable(gatedIp) {
			t.Fatal("expired: the dialer of the expired gated record lost the last-resort rule")
		}
		if drawn := testConnectDrawIps(directory, 4); len(drawn) == 0 || drawn[0] != openIp.String() {
			t.Fatalf("%s: the fallback drew %v, expected the open record first", lapse, drawn)
		}
	}
}

// When the control route cannot be reached, the cached verified open records
// carry it at once rather than at the end of the gated budget (§1.1 rule 4),
// and the fallback is in force for anything a seed bootstrap adds.
func TestExtenderGatedPreferenceControlReachAllowsTheCachedOpenRecords(t *testing.T) {
	clock := newTestClock()
	directory, rootPrivateKey := newTestGatedPreferenceDirectory(t, clock)
	applyTestGatedRelease(t, directory, rootPrivateKey, clock, newTestExtenderKey(t), "192.0.2.10")
	applyTestOpenTierRecord(t, directory, rootPrivateKey, clock, "198.51.100.1")
	testConnectDrawIps(directory, 4)
	if !directory.AllowOpenForControlReach() {
		t.Fatal("the cached open record was not let carry the control route")
	}
	if drawn := testConnectDrawIps(directory, 4); !slices.Equal(drawn, []string{"192.0.2.10", "198.51.100.1"}) {
		t.Fatalf("the draw after control reach was %v", drawn)
	}
	if reason := directory.GatedPreference().FallbackReason; reason != ExtenderGatedFallbackControlReach {
		t.Fatalf("reason %q", reason)
	}
	// already falling back, nothing is new
	if directory.AllowOpenForControlReach() {
		t.Fatal("a second reach claimed to add something")
	}

	// with no open record cached it adds nothing, but a record a seed brings
	// next is dialable at once
	clock = newTestClock()
	directory, rootPrivateKey = newTestGatedPreferenceDirectory(t, clock)
	applyTestGatedRelease(t, directory, rootPrivateKey, clock, newTestExtenderKey(t), "192.0.2.10")
	testConnectDrawIps(directory, 4)
	if directory.AllowOpenForControlReach() {
		t.Fatal("a reach with no open record claimed to add one")
	}
	applyTestOpenTierRecord(t, directory, rootPrivateKey, clock, "198.51.100.9")
	if drawn := testConnectDrawIps(directory, 4); !slices.Contains(drawn, "198.51.100.9") {
		t.Fatalf("the seeded open record was held back: %v", drawn)
	}

	// and with the preference off open records are never held back
	clock = newTestClock()
	directory, rootPrivateKey = newTestExtenderDirectory(t, clock, nil)
	applyTestOpenTierRecord(t, directory, rootPrivateKey, clock, "198.51.100.1")
	if directory.AllowOpenForControlReach() {
		t.Fatal("a reach with the preference off claimed to add something")
	}
}

// The policy asks for a release refresh when the current gated records drop
// below the low watermark and when it falls back for a reason a release can
// mend, once per crossing (§1.1, §1.3).
func TestExtenderGatedPreferenceAsksForARefresh(t *testing.T) {
	clock := newTestClock()
	directory, rootPrivateKey := newTestGatedPreferenceDirectory(t, clock)
	applyTestOpenTierRecord(t, directory, rootPrivateKey, clock, "198.51.100.1")
	notified := func(notify chan struct{}) bool {
		select {
		case <-notify:
			return true
		default:
			return false
		}
	}

	notify := directory.GatedRefreshMonitor().NotifyChannel()
	testConnectDrawIps(directory, 4)
	if !notified(notify) {
		t.Fatal("a cold draw asked for no release")
	}
	notify = directory.GatedRefreshMonitor().NotifyChannel()
	for range 4 {
		testConnectDrawIps(directory, 4)
	}
	if notified(notify) {
		t.Fatal("simultaneous draws below the watermark asked again")
	}
	// a released record clears the watermark; its expiry crosses it again
	applyTestGatedRelease(t, directory, rootPrivateKey, clock, newTestExtenderKey(t), "192.0.2.10")
	testConnectDrawIps(directory, 4)
	notify = directory.GatedRefreshMonitor().NotifyChannel()
	clock.advance(25 * time.Hour)
	testConnectDrawIps(directory, 4)
	if !notified(notify) {
		t.Fatal("the expiry of the last gated record asked for no release")
	}
	if status := directory.GatedPreference(); status.Stats.RefreshWantedCount < 2 {
		t.Fatalf("refresh asks %d", status.Stats.RefreshWantedCount)
	}
}

// Gated and channel-restricted records never leave this client on a public
// path, whatever source they arrived from and whether or not a connection
// uses them now: not in a share (K7), not in a feed sample from any vantage,
// not to a feed or mesh subscriber, and not into a seed bundle, which the
// publisher refuses whatever else it holds (§1.3, §1.4, §2.2; R1, R4).
func TestExtenderGatedRecordsNeverLeaveOnAPublicPath(t *testing.T) {
	clock := newTestClock()
	directory, rootPrivateKey := newTestGatedPreferenceDirectory(t, clock)
	messages, unsubscribe := directory.Subscribe()
	defer unsubscribe()

	releasedKey := newTestExtenderKey(t)
	applyTestGatedRelease(t, directory, rootPrivateKey, clock, releasedKey, "192.0.2.10", "2001:db8::10")
	// a gated canary and a channel-marked record of the open tier leak in
	restricted := map[string]ed25519.PublicKey{}
	restrictedRecords := []*protocol.ExtenderRecord{
		signTestTierRecord(t, rootPrivateKey, releasedKey, clock.Now(), ExtenderDirectoryTierGated, "192.0.2.10"),
	}
	for i, tier := range []int{ExtenderDirectoryTierGated, ExtenderDirectoryTierOpen} {
		key := newTestExtenderKey(t)
		ip := fmt.Sprintf("192.0.2.%d", 20+i)
		record, err := SignExtenderRecord(rootPrivateKey, &protocol.ExtenderRecordBody{
			PublicKey:     key,
			Addresses:     []*protocol.ExtenderAddress{testExtenderAddress(ip)},
			TcpPort:       443,
			IssueTimeMs:   uint64(clock.Now().UnixMilli()),
			ExpireTimeMs:  uint64(clock.Now().Add(24 * time.Hour).UnixMilli()),
			NetworkHost:   testExtenderNetworkHost,
			DirectoryTier: uint32(tier),
			CanaryChannel: ExtenderChannelDns,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := directory.ApplyRecord(record, ExtenderSourceGossip); err != nil {
			t.Fatal(err)
		}
		restricted[ip] = key
		restrictedRecords = append(restrictedRecords, record)
	}
	restricted["192.0.2.10"] = releasedKey
	restricted["2001:db8::10"] = releasedKey
	openKey := applyTestOpenTierRecord(t, directory, rootPrivateKey, clock, "198.51.100.1")
	// in use, which a share ranks first
	for ip := range restricted {
		directory.SetInUse(netip.MustParseAddr(ip), 1)
	}

	share := BuildExtenderShare(directory, testExtenderNetworkHost, "", "", nil, false, ExtenderShareMaxAddressCount)
	shared := []string{}
	for _, ip := range ExtenderShareAddresses(share) {
		shared = append(shared, ip.String())
	}
	if !slices.Equal(shared, []string{"198.51.100.1"}) {
		t.Fatalf("the share carries %v, expected the open record alone", shared)
	}
	for i := range 64 {
		for _, keyHex := range sampleKeyHexes(t, directory, directory.SampleRecords(ExtenderFeedMaxSampleCount, nil, []byte(fmt.Sprintf("vantage-%d", i)))) {
			if keyHex != hex.EncodeToString(openKey) {
				t.Fatalf("vantage %d was sampled a restricted record", i)
			}
		}
	}
	select {
	case message := <-messages:
		body, err := directory.RootKeys().VerifyRecord(message.GetRecord())
		if err != nil || !bytes.Equal(body.PublicKey, openKey) {
			t.Fatal("a subscriber was handed a restricted record")
		}
	default:
		t.Fatal("the open record was not published")
	}
	select {
	case <-messages:
		t.Fatal("a subscriber was handed more than the open record")
	default:
	}

	openRecord := signTestTierRecord(t, rootPrivateKey, openKey, clock.Now(), ExtenderDirectoryTierOpen, "198.51.100.1")
	seedExpireTime := clock.Now().Add(time.Hour)
	if _, err := NewExtenderSeedBody(testExtenderNetworkHost, 1, clock.Now(), seedExpireTime, []*protocol.ExtenderRecord{openRecord}, nil); err != nil {
		t.Fatalf("the open record does not make a seed body: %v", err)
	}
	for i, record := range restrictedRecords {
		body, err := NewExtenderSeedBody(
			testExtenderNetworkHost,
			1,
			clock.Now(),
			seedExpireTime,
			[]*protocol.ExtenderRecord{openRecord, record},
			nil,
		)
		if body != nil || ExtenderSeedErrorCode(err) != ExtenderSeedCodeGatedRecord {
			t.Fatalf("restricted record %d entered a seed body: %v", i, err)
		}
	}
}

// Routine endpoint replacement in each family (§1.3): a release re-signs the
// record at a new address, the old one drains as it fails, and when the old
// endpoint disappears before the refresh the fallback carries the client until
// the refreshed record is tried again. Only the host's family is ever drawn.
func TestExtenderGatedReleaseReplacesAnEndpointInEitherFamily(t *testing.T) {
	cases := []struct {
		ipVersion  int
		oldIp      string
		newIp      string
		openIp     string
		otherIp    string
		otherIpNew string
	}{
		{ipVersion: 4, oldIp: "192.0.2.10", newIp: "192.0.2.11", openIp: "198.51.100.1", otherIp: "2001:db8::10", otherIpNew: "2001:db8::11"},
		{ipVersion: 6, oldIp: "2001:db8::10", newIp: "2001:db8::11", openIp: "2001:db8:1::1", otherIp: "192.0.2.10", otherIpNew: "192.0.2.11"},
	}
	for _, c := range cases {
		draw := func(directory *ExtenderDirectory) []string {
			drawn := []string{}
			for _, candidate := range directory.ConnectCandidates([]int{c.ipVersion}, 64, false, nil)[0] {
				if candidate.IpVersion != c.ipVersion {
					t.Fatalf("v%d: a host of one family drew %s", c.ipVersion, candidate.Ip)
				}
				drawn = append(drawn, candidate.Ip.String())
			}
			return drawn
		}

		// routine: the refresh lands before the old endpoint goes
		clock := newTestClock()
		directory, rootPrivateKey := newTestGatedPreferenceDirectory(t, clock)
		gatedKey := newTestExtenderKey(t)
		applyTestGatedRelease(t, directory, rootPrivateKey, clock, gatedKey, c.oldIp, c.otherIp)
		applyTestOpenTierRecord(t, directory, rootPrivateKey, clock, c.openIp)
		if drawn := draw(directory); !slices.Equal(drawn, []string{c.oldIp}) {
			t.Fatalf("v%d: drew %v", c.ipVersion, drawn)
		}
		directory.RecordSuccess(netip.MustParseAddr(c.oldIp), ExtenderConnectModeTcpTls)
		clock.advance(6 * time.Hour)
		applyTestGatedRelease(t, directory, rootPrivateKey, clock, gatedKey, c.newIp, c.otherIpNew)
		if drawn := draw(directory); !slices.Contains(drawn, c.newIp) || slices.Contains(drawn, c.openIp) {
			t.Fatalf("v%d: after the replacement drew %v", c.ipVersion, drawn)
		}
		directory.RecordFailure(netip.MustParseAddr(c.oldIp), ExtenderConnectModeTcpTls)
		if drawn := draw(directory); !slices.Equal(drawn, []string{c.newIp}) {
			t.Fatalf("v%d: once the old endpoint failed drew %v", c.ipVersion, drawn)
		}
		if mode := directory.GatedPreference().Mode; mode != ExtenderGatedModeGated {
			t.Fatalf("v%d: mode %s, expected the replacement to keep gated", c.ipVersion, mode)
		}

		// the old endpoint disappears before the refresh
		clock = newTestClock()
		directory, rootPrivateKey = newTestGatedPreferenceDirectory(t, clock)
		gatedKey = newTestExtenderKey(t)
		applyTestGatedRelease(t, directory, rootPrivateKey, clock, gatedKey, c.oldIp, c.otherIp)
		applyTestOpenTierRecord(t, directory, rootPrivateKey, clock, c.openIp)
		draw(directory)
		directory.RecordSuccess(netip.MustParseAddr(c.oldIp), ExtenderConnectModeTcpTls)
		draw(directory)
		clock.advance(time.Hour)
		directory.RecordFailure(netip.MustParseAddr(c.oldIp), ExtenderConnectModeTcpTls)
		if drawn := draw(directory); !slices.Equal(drawn, []string{c.openIp}) {
			t.Fatalf("v%d: with the old endpoint gone drew %v, expected the open fallback", c.ipVersion, drawn)
		}
		if reason := directory.GatedPreference().FallbackReason; reason != ExtenderGatedFallbackGatedFailed {
			t.Fatalf("v%d: reason %q", c.ipVersion, reason)
		}
		clock.advance(time.Minute)
		applyTestGatedRelease(t, directory, rootPrivateKey, clock, gatedKey, c.newIp, c.otherIpNew)
		if drawn := draw(directory); !slices.Equal(drawn, []string{c.newIp}) {
			t.Fatalf("v%d: the refreshed record drew %v", c.ipVersion, drawn)
		}
		directory.RecordSuccess(netip.MustParseAddr(c.newIp), ExtenderConnectModeTcpTls)
		draw(directory)
		if mode := directory.GatedPreference().Mode; mode != ExtenderGatedModeGated {
			t.Fatalf("v%d: mode %s after the new endpoint connected", c.ipVersion, mode)
		}
	}
}

// An account change drops what the operator released to the identity before
// it (§1.1 rule 2): every gated record and the addresses only they name, never
// an open record, a revocation, this extender's own record, or a manual
// address, which stays as the unverified manual entry it was.
func TestExtenderGatedForgetDropsOnlyTheGatedRecords(t *testing.T) {
	clock := newTestClock()
	directory, rootPrivateKey := newTestGatedPreferenceDirectory(t, clock)
	manualIp := netip.MustParseAddr("192.0.2.11")
	directory.AddManual(manualIp)
	releasedKey := newTestExtenderKey(t)
	applyTestGatedRelease(t, directory, rootPrivateKey, clock, releasedKey, "192.0.2.10", manualIp.String())
	ownKey := newTestExtenderKey(t)
	directory.KeepPublicKey(ownKey)
	applyTestGatedRelease(t, directory, rootPrivateKey, clock, ownKey, "192.0.2.12")
	revokedKey := newTestExtenderKey(t)
	applyTestGatedRelease(t, directory, rootPrivateKey, clock, revokedKey, "192.0.2.13")
	clock.advance(time.Minute)
	if _, err := directory.ApplyRevocation(signTestRevocation(t, rootPrivateKey, revokedKey, clock.Now())); err != nil {
		t.Fatal(err)
	}
	applyTestOpenTierRecord(t, directory, rootPrivateKey, clock, "198.51.100.1")

	if count := directory.ForgetGatedRecords(); count != 2 {
		t.Fatalf("forgot %d gated records, expected the released and the revoked one", count)
	}
	if testDirectoryKnown(directory, netip.MustParseAddr("192.0.2.10")) || testDirectoryKnown(directory, netip.MustParseAddr("192.0.2.13")) {
		t.Fatal("a forgotten gated record kept its address")
	}
	if entry := testDirectoryEntry(t, directory, manualIp); entry.Source != ExtenderSourceManual || entry.State != ExtenderStateUnverified {
		t.Fatalf("the manual address is %s from %s, expected the unverified manual entry", entry.State, entry.Source)
	}
	if !testDirectoryKnown(directory, netip.MustParseAddr("192.0.2.12")) || !testDirectoryKnown(directory, netip.MustParseAddr("198.51.100.1")) {
		t.Fatal("the extender's own record or an open record was forgotten")
	}
	// the revocation stays, so the record it revoked cannot come back
	clock.advance(-2 * time.Minute)
	replayed := signTestTierRecord(t, rootPrivateKey, revokedKey, clock.Now(), ExtenderDirectoryTierGated, "192.0.2.13")
	clock.advance(2 * time.Minute)
	if _, err := directory.ApplyRecord(replayed, ExtenderSourceGossip); err != nil {
		t.Fatal(err)
	}
	if directory.IsActiveKey(revokedKey) {
		t.Fatal("a replayed record brought a revoked key back after the forget")
	}
}
