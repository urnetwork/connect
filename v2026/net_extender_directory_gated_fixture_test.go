package connect

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// A finite deterministic candidate stream replaces crypto/rand at the fixture
// boundary. Partition functions select the requested geometry; this is not an
// independent test of the hmac algorithm or a production key-generation path.
const testGatedKeyAttemptLimit = 4096

// Pin actual distinct Ed25519 identities to an explicit population per partition.
// Exhausting the bounded seed stream fails setup instead of rerolling the test.
func testGatedKeyHexesByPartition(t *testing.T, partitionSizes []int) []string {
	t.Helper()
	count := 0
	for _, size := range partitionSizes {
		if size < 0 || 64 < size {
			t.Fatal("gated key fixture partition size is outside [0,64]")
		}
		count += size
	}
	if 64 < count || len(partitionSizes) != ExtenderPartitionCount(count) {
		t.Fatal("gated key fixture population does not match its finite partition count")
	}
	remaining := slices.Clone(partitionSizes)
	keyHexes := make([]string, 0, count)
	seen := map[string]bool{}
	for candidate := 0; candidate < testGatedKeyAttemptLimit && len(keyHexes) < count; candidate++ {
		seed := sha256.Sum256([]byte(fmt.Sprintf("urnetwork-test-gated-key-v1/%d", candidate)))
		publicKey, err := ExtenderPublicKeyFromSeed(seed[:])
		if err != nil {
			t.Fatal(err)
		}
		keyHex := hex.EncodeToString(publicKey)
		partition := ExtenderRecordPartition(testPartitionSecret, ExtenderChannelGated, keyHex, len(partitionSizes))
		if remaining[partition] == 0 || seen[keyHex] {
			continue
		}
		remaining[partition]--
		seen[keyHex] = true
		keyHexes = append(keyHexes, keyHex)
	}
	if len(keyHexes) != count {
		t.Fatalf("bounded gated fixture populated %d/%d records in %d seeds", len(keyHexes), count, testGatedKeyAttemptLimit)
	}
	slices.Sort(keyHexes)
	return keyHexes
}

// The exact-count release rows require population, not a lucky random draw.
// Pin every helper size currently used by the directory-tier tests.
func TestExtenderGatedFixturePinsPartitionPopulation(t *testing.T) {
	for _, count := range []int{0, 1, 3, 16, 64} {
		keys := testGatedKeyHexes(t, count)
		if len(keys) != count || !slices.IsSorted(keys) || len(slices.Compact(slices.Clone(keys))) != count ||
			!slices.Equal(keys, testGatedKeyHexes(t, count)) {
			t.Fatalf("records=%d: gated fixture changed cardinality, uniqueness, order or seeded identity", count)
		}
		partitionCount := ExtenderPartitionCount(count)
		if count == 16 && partitionCount != 4 || count == 64 && partitionCount != 8 {
			t.Fatalf("records=%d: gated fixture partition calibration changed", count)
		}
		populations := make([]int, partitionCount)
		for _, key := range keys {
			publicKey, err := hex.DecodeString(key)
			if err != nil || len(publicKey) != ed25519.PublicKeySize {
				t.Fatalf("records=%d: gated fixture did not supply an actual Ed25519 public identity", count)
			}
			populations[ExtenderRecordPartition(testPartitionSecret, ExtenderChannelGated, key, partitionCount)]++
		}
		for partition, population := range populations {
			expected := count / partitionCount
			if partition < count%partitionCount {
				expected++
			}
			if population != expected {
				t.Fatalf("records=%d: partition %d got %d records, expected pinned population %d", count, partition, population, expected)
			}
		}
	}
}

// A trusted count is an entitlement cap, not permission to leave a sparse
// identity partition. Only an empty partition may select the next nonempty one.
func TestExtenderReleaseSparsePartitionKeepsIsolation(t *testing.T) {
	for _, available := range []int{0, 1, 2, 3, 4, 8} {
		clock := newTestClock()
		policy := newTestReleasePolicy(t, clock, nil, nil)
		identity := []byte("network-a/client-a")
		partitionSizes := []int{8, 8, 8, 8, 8, 8, 8, 8}
		target := ExtenderVantagePartition(testPartitionSecret, ExtenderChannelGated, identity, len(partitionSizes))
		partitionSizes[target] = available
		partitionSizes[(target+1)%len(partitionSizes)] += 8 - available
		keys := testGatedKeyHexesByPartition(t, partitionSizes)
		members, partition, partitionCount := ExtenderPartitionMembers(testPartitionSecret, ExtenderChannelGated, identity, keys)
		expectedPartition, expectedPopulation := target, available
		if available == 0 {
			expectedPartition = (target + 1) % len(partitionSizes)
			expectedPopulation = partitionSizes[expectedPartition]
		}
		if len(keys) != 64 || partitionCount != 8 || partition != expectedPartition || len(members) != expectedPopulation {
			t.Fatalf("available=%d: explicit 64-key sparse fixture did not have its specified partition geometry", available)
		}
		request := &ExtenderReleaseRequest{
			Identity:           identity,
			IdentityCreateTime: clock.Now().Add(-60 * 24 * time.Hour),
			Vantage:            "198.51.100.0/24",
			CountryCode:        "aa",
		}
		first, err := policy.Release(request, keys)
		if err != nil {
			t.Fatalf("available=%d: release failed: %v", available, err)
		}
		if first.Probation || first.Count != ExtenderReleaseTrustedIdentityCount ||
			len(first.KeyHexes) != min(ExtenderReleaseTrustedIdentityCount, expectedPopulation) ||
			first.Partition != expectedPartition || first.PartitionCount != 8 {
			t.Fatalf("available=%d: trusted sparse release changed entitlement or partition: %+v", available, first)
		}
		for _, key := range first.KeyHexes {
			if !slices.Contains(members, key) {
				t.Fatalf("available=%d: sparse release borrowed a record outside its one selected partition", available)
			}
		}
		// Reordered, repeated and uppercase input cannot change the unique
		// fleet geometry or disclose replacements within the same epoch.
		reordered := append(slices.Clone(keys), strings.ToUpper(keys[0]), keys[1])
		slices.Reverse(reordered)
		for attempt := 1; attempt < ExtenderReleaseIdentityRequestLimit; attempt++ {
			again, err := policy.Release(request, reordered)
			if err != nil || !slices.Equal(again.KeyHexes, first.KeyHexes) {
				t.Fatalf("available=%d: sparse repeat %d changed disclosure: %+v err=%v", available, attempt+1, again, err)
			}
		}
		if _, err := policy.Release(request, keys); !errors.Is(err, ErrExtenderReleaseIdentityLimited) {
			t.Fatalf("available=%d: sparse release bypassed the unchanged identity request cap: %v", available, err)
		}
	}
}
