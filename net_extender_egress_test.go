// The extender carrier's socket and the egress machinery (QD-810). The platform
// cases, the mark on Linux, the pin on Windows and macOS, are in
// egress_dial_sites_linux_test.go, egress_windows_test.go and
// egress_darwin_test.go.

package connect

import (
	"context"
	"errors"
	"net"
	"syscall"
	"testing"
)

// A packet endpoint an embedder injected, which records whether anything asked
// it for its socket.
type testInjectedPacketConn struct {
	net.PacketConn
	syscallConnCount int
}

func (self *testInjectedPacketConn) SyscallConn() (syscall.RawConn, error) {
	self.syscallConnCount += 1
	return nil, errors.New("the injected endpoint was asked for its socket")
}

// An injected endpoint is the embedder's, with its own binding: both callers'
// openExtenderPacketConn hands it back as it was made and the egress machinery
// never reaches into it, whatever egress is set.
func TestExtenderPacketConnLeavesAnInjectedEndpointAlone(t *testing.T) {
	inner, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer inner.Close()
	injected := &testInjectedPacketConn{PacketConn: inner}

	previousMark := EgressMark()
	previous4, previous6 := EgressInterfaceIndex()
	defer func() {
		SetEgressMark(previousMark)
		SetEgressInterfaceIndex(previous4, previous6)
	}()
	SetEgressMark(0x7572)
	SetEgressInterfaceIndex(1, 1)

	connectSettings := DefaultConnectSettings()
	connectSettings.DialContextSettings = &DialContextSettings{
		PacketConnFactory: func(context.Context) (net.PacketConn, error) {
			return injected, nil
		},
	}
	packetConn, err := openExtenderPacketConn(context.Background(), connectSettings, &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 443})
	AssertEqual(t, err, nil)
	if packetConn != net.PacketConn(injected) {
		t.Fatalf("the injected endpoint came back as %T", packetConn)
	}
	AssertEqual(t, injected.syscallConnCount, 0)
}

// With no egress index or mark set, the socket openExtenderPacketConn makes is
// an ordinary wildcard socket of the destination's family, as net.ListenUDP
// made it before the machinery did.
func TestExtenderPacketConnIsAnOrdinarySocketWithNoEgress(t *testing.T) {
	AssertEqual(t, egressConfigured(), false)
	for _, c := range []struct {
		destination *net.UDPAddr
		ip          net.IP
	}{
		{destination: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 443}, ip: net.IPv4zero},
		{destination: &net.UDPAddr{IP: net.IPv6loopback, Port: 443}, ip: net.IPv6zero},
	} {
		packetConn, err := openExtenderPacketConn(context.Background(), DefaultConnectSettings(), c.destination)
		if err != nil {
			t.Fatalf("%s: %v", c.destination, err)
		}
		udpConn, ok := packetConn.(*net.UDPConn)
		if !ok {
			packetConn.Close()
			t.Fatalf("%s: a %T", c.destination, packetConn)
		}
		local := udpConn.LocalAddr().(*net.UDPAddr)
		udpConn.Close()
		if !local.IP.Equal(c.ip) || local.Port == 0 {
			t.Errorf("%s: bound to %s", c.destination, local)
		}
	}
}
