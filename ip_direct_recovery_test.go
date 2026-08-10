// This file verifies that stream-capable IP data removes the duplicate
// Transfer recovery loop while non-direct compatibility routes retain their
// established reliability policy.
package connect

import "testing"

// Every IP protocol uses unacknowledged Transfer on a direct-capable stream;
// only the historical UDP collapse option changes a non-direct route.
func TestIpPacketTransferAckRequired(t *testing.T) {
	tests := []struct {
		name                  string
		protocol              IpProtocol
		allowDirect           bool
		udpCollapsePrevention bool
		ackRequired           bool
	}{
		{
			name:        "direct tcp",
			protocol:    IpProtocolTcp,
			allowDirect: true,
			ackRequired: false,
		},
		{
			name:        "direct udp",
			protocol:    IpProtocolUdp,
			allowDirect: true,
			ackRequired: false,
		},
		{
			name:        "direct icmp",
			protocol:    IpProtocolIcmp,
			allowDirect: true,
			ackRequired: false,
		},
		{
			name:        "platform tcp",
			protocol:    IpProtocolTcp,
			ackRequired: true,
		},
		{
			name:        "platform udp",
			protocol:    IpProtocolUdp,
			ackRequired: true,
		},
		{
			name:                  "platform udp collapse",
			protocol:              IpProtocolUdp,
			udpCollapsePrevention: true,
			ackRequired:           false,
		},
	}
	for _, test := range tests {
		ackRequired := ipPacketTransferAckRequired(
			&IpPath{Protocol: test.protocol},
			test.allowDirect,
			test.udpCollapsePrevention,
		)
		if ackRequired != test.ackRequired {
			t.Fatalf(
				"%s ack required = %t, want %t",
				test.name,
				ackRequired,
				test.ackRequired,
			)
		}
	}
	if !ipPacketTransferAckRequired(nil, false, true) {
		t.Fatal("missing IP metadata disabled compatibility acknowledgement")
	}
}
