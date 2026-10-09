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

	"github.com/urnetwork/connect/protocol"
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

// A gated fleet of `count` keys, hex and sorted.
func testGatedKeyHexes(t *testing.T, count int) []string {
	t.Helper()
	keyHexes := []string{}
	for range count {
		keyHexes = append(keyHexes, hex.EncodeToString(newTestExtenderKey(t)))
	}
	slices.Sort(keyHexes)
	return keyHexes
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
