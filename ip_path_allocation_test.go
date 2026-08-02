package connect

import (
	"net"
	"testing"
)

var ipPathAllocationSink *IpPath
var ipPathPortAllocationSink int

func TestBorrowedIpPathAvoidsAddressAllocations(t *testing.T) {
	packet := testingUdp4Packet("10.0.0.1", "203.0.113.7", 443, []byte("payload"))

	ownedAllocs := testing.AllocsPerRun(1000, func() {
		ipPath, _, err := ParseIpPathWithPayload(packet)
		if err != nil {
			panic(err)
		}
		ipPathAllocationSink = ipPath
	})
	borrowedAllocs := testing.AllocsPerRun(1000, func() {
		var ipPath IpPath
		if _, err := parseIpPathWithPayloadBorrowed(packet, &ipPath); err != nil {
			panic(err)
		}
		ipPathPortAllocationSink = ipPath.DestinationPort
	})

	if borrowedAllocs != 0 {
		t.Fatalf("borrowed IP path allocated %.0f times, want 0", borrowedAllocs)
	}
	if ownedAllocs <= borrowedAllocs {
		t.Fatalf("borrowed IP path did not reduce allocations: owned=%.0f borrowed=%.0f",
			ownedAllocs, borrowedAllocs)
	}

	var borrowed IpPath
	if _, err := parseIpPathWithPayloadBorrowed(packet, &borrowed); err != nil {
		t.Fatal(err)
	}
	if !borrowed.SourceIp.Equal(net.ParseIP("10.0.0.1")) ||
		!borrowed.DestinationIp.Equal(net.ParseIP("203.0.113.7")) {
		t.Fatalf("unexpected borrowed path: %s -> %s", borrowed.SourceIp, borrowed.DestinationIp)
	}
}
