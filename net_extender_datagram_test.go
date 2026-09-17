package connect

import (
	"net"
	"testing"
)

func TestExtenderDatagramRemoteAddressDoesNotRequireResolution(t *testing.T) {
	for _, test := range []struct {
		name    string
		network string
		address string
		udp     bool
	}{
		{"ipv4 literal", "udp4", "192.0.2.1:443", true},
		{"ipv6 literal", "udp6", "[2001:db8::1]:443", true},
		{"unresolved hostname", "udp", "destination.invalid:443", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			addr := newExtenderDatagramAddr(test.network, test.address)
			if got := addr.String(); got != test.address {
				t.Fatalf("address = %q, want %q", got, test.address)
			}
			_, udp := addr.(*net.UDPAddr)
			if udp != test.udp {
				t.Fatalf("address type = %T, want UDP address = %t", addr, test.udp)
			}
			if !test.udp && addr.Network() != test.network {
				t.Fatalf("network = %q, want %q", addr.Network(), test.network)
			}
		})
	}
}
