package connect

// Family balance in the gossip release.
//
// A directory is normally mostly v4, because v4 addresses are easier to come
// by. A plain shuffle therefore hands a v6-only client a sample it cannot dial,
// and a client has no way to ask for more. These tests pin that a sample aims
// for an equal number of v4-reachable and v6-reachable extenders, and falls
// back to whatever exists when one family runs out.

import (
	"fmt"
	"testing"

	"github.com/urnetwork/connect/protocol"
)

// balanceTestRecords builds `ipv4Only` v4-only, `ipv6Only` v6-only and `dual`
// dual-stack records, and the body map the balancer reads families from.
func balanceTestRecords(ipv4Only int, ipv6Only int, dual int) (
	[]*protocol.ExtenderRecord,
	map[*protocol.ExtenderRecord]*protocol.ExtenderRecordBody,
) {
	records := []*protocol.ExtenderRecord{}
	bodies := map[*protocol.ExtenderRecord]*protocol.ExtenderRecordBody{}

	add := func(addresses []*protocol.ExtenderAddress) {
		record := &protocol.ExtenderRecord{Body: []byte(fmt.Sprintf("r%d", len(records)))}
		records = append(records, record)
		bodies[record] = &protocol.ExtenderRecordBody{Addresses: addresses}
	}
	for i := range ipv4Only {
		add([]*protocol.ExtenderAddress{
			&protocol.ExtenderAddress{Ip: fmt.Sprintf("192.0.2.%d", 1+i), IpVersion: 4},
		})
	}
	for i := range ipv6Only {
		add([]*protocol.ExtenderAddress{
			&protocol.ExtenderAddress{Ip: fmt.Sprintf("2001:db8::%x", 1+i), IpVersion: 6},
		})
	}
	for i := range dual {
		add([]*protocol.ExtenderAddress{
			&protocol.ExtenderAddress{Ip: fmt.Sprintf("198.51.100.%d", 1+i), IpVersion: 4},
			&protocol.ExtenderAddress{Ip: fmt.Sprintf("2001:db8:1::%x", 1+i), IpVersion: 6},
		})
	}
	return records, bodies
}

// countFamilies reports how many of the first `count` records are reachable
// over each family. A dual-stack record counts for both.
func countFamilies(
	balanced []*protocol.ExtenderRecord,
	bodies map[*protocol.ExtenderRecord]*protocol.ExtenderRecordBody,
	count int,
) (ipv4 int, ipv6 int) {
	for i := 0; i < count && i < len(balanced); i += 1 {
		hasIpv4, hasIpv6 := recordIpFamilies(bodies[balanced[i]])
		if hasIpv4 {
			ipv4 += 1
		}
		if hasIpv6 {
			ipv6 += 1
		}
	}
	return ipv4, ipv6
}

// The case the change exists for: a directory dominated by v4 must still yield
// v6-reachable extenders in a small sample.
func TestGossipSampleBalancesAnIpv4HeavyDirectory(t *testing.T) {
	records, bodies := balanceTestRecords(100, 20, 0)

	// repeated because the ordering is randomised within each family
	for range 50 {
		balanced := balanceRecordsByIpFamily(records, bodies)
		ipv4, ipv6 := countFamilies(balanced, bodies, 8)
		if ipv4 != 4 || ipv6 != 4 {
			t.Fatalf("sample of 8 from 100 v4 / 20 v6 gave %d v4 and %d v6, want 4 and 4", ipv4, ipv6)
		}
	}
}

// "if there are no more ipv4/ipv6 then it can use more of what it has"
func TestGossipSampleFallsBackWhenOneFamilyIsShort(t *testing.T) {
	records, bodies := balanceTestRecords(100, 3, 0)

	for range 50 {
		balanced := balanceRecordsByIpFamily(records, bodies)
		ipv4, ipv6 := countFamilies(balanced, bodies, 8)
		if ipv6 != 3 {
			t.Fatalf("got %d v6, want all 3 that exist", ipv6)
		}
		if ipv4 != 5 {
			t.Fatalf("got %d v4, want 5 filling the rest of the 8", ipv4)
		}
	}
}

func TestGossipSampleWithOnlyOneFamily(t *testing.T) {
	records, bodies := balanceTestRecords(20, 0, 0)
	balanced := balanceRecordsByIpFamily(records, bodies)
	ipv4, ipv6 := countFamilies(balanced, bodies, 8)
	if ipv4 != 8 || ipv6 != 0 {
		t.Fatalf("v4-only directory gave %d v4 and %d v6, want 8 and 0", ipv4, ipv6)
	}
}

// A dual-stack extender satisfies both sides, so an all-dual directory is
// balanced whatever order it comes out in.
func TestGossipSampleWithDualStackOnly(t *testing.T) {
	records, bodies := balanceTestRecords(0, 0, 20)
	balanced := balanceRecordsByIpFamily(records, bodies)
	ipv4, ipv6 := countFamilies(balanced, bodies, 8)
	if ipv4 != 8 || ipv6 != 8 {
		t.Fatalf("all dual-stack gave %d v4 and %d v6, want 8 and 8", ipv4, ipv6)
	}
}

// Dual-stack extenders are the most useful ones and must not be held back in
// favour of single-family records.
func TestGossipSampleDoesNotStarveDualStack(t *testing.T) {
	records, bodies := balanceTestRecords(50, 50, 4)

	dualSeen := 0
	for range 50 {
		balanced := balanceRecordsByIpFamily(records, bodies)
		for i := 0; i < 8 && i < len(balanced); i += 1 {
			hasIpv4, hasIpv6 := recordIpFamilies(bodies[balanced[i]])
			if hasIpv4 && hasIpv6 {
				dualSeen += 1
			}
		}
	}
	if dualSeen == 0 {
		t.Fatal("dual-stack records never appeared in 50 samples of 8")
	}
}

// Balancing reorders; it must not drop or duplicate.
func TestGossipSampleIsAPermutation(t *testing.T) {
	records, bodies := balanceTestRecords(7, 5, 3)
	balanced := balanceRecordsByIpFamily(records, bodies)

	if len(balanced) != len(records) {
		t.Fatalf("balanced %d records from %d", len(balanced), len(records))
	}
	seen := map[*protocol.ExtenderRecord]int{}
	for _, record := range balanced {
		seen[record] += 1
	}
	for _, record := range records {
		if seen[record] != 1 {
			t.Fatalf("record appears %d times, want exactly once", seen[record])
		}
	}
}

// A record naming no parseable address is reachable over neither family. It is
// kept rather than dropped, so what the directory reports as available does not
// change, but it must not crowd out a usable one.
func TestGossipSampleKeepsUnreachableRecordsLast(t *testing.T) {
	records, bodies := balanceTestRecords(4, 4, 0)
	unreachable := &protocol.ExtenderRecord{Body: []byte("unreachable")}
	records = append(records, unreachable)
	bodies[unreachable] = &protocol.ExtenderRecordBody{
		Addresses: []*protocol.ExtenderAddress{
			&protocol.ExtenderAddress{Ip: "not-an-address", IpVersion: 4},
		},
	}

	for range 50 {
		balanced := balanceRecordsByIpFamily(records, bodies)
		if len(balanced) != len(records) {
			t.Fatalf("dropped a record: %d of %d", len(balanced), len(records))
		}
		// with 4+4 usable records, a sample of 8 should be all usable
		ipv4, ipv6 := countFamilies(balanced, bodies, 8)
		if ipv4+ipv6 < 8 {
			t.Fatalf("an unreachable record displaced a usable one (%d v4, %d v6)", ipv4, ipv6)
		}
	}
}

// A nil body must not panic the sampler.
func TestGossipSampleToleratesAMissingBody(t *testing.T) {
	records, bodies := balanceTestRecords(2, 2, 0)
	orphan := &protocol.ExtenderRecord{Body: []byte("orphan")}
	records = append(records, orphan)

	balanced := balanceRecordsByIpFamily(records, bodies)
	if len(balanced) != len(records) {
		t.Fatalf("balanced %d of %d", len(balanced), len(records))
	}
}
