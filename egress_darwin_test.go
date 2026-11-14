//go:build darwin

// Darwin socket tests cover fail-closed physical-interface binding.
package connect

import (
	"context"
	"crypto/tls"
	"net"
	"net/netip"
	"strings"
	"syscall"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"golang.org/x/sys/unix"
)

// A valid socket binds while an invalid descriptor fails closed.
func TestApplyEgressInterfaceDarwin(t *testing.T) {
	loopback, err := net.InterfaceByName("lo0")
	if err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := applyEgressInterface(uintptr(fd), uint32(loopback.Index), uint32(loopback.Index), 0); err != nil {
		t.Fatalf("pin valid IPv4 socket: %v", err)
	}
	if err := applyEgressInterface(^uintptr(0), uint32(loopback.Index), uint32(loopback.Index), 0); err == nil {
		t.Fatal("invalid socket was accepted without an egress binding")
	}
}

// Sets the process's egress interface for the test, putting the previous one back at its end.
func setTestEgressInterfaceIndexDarwin(t *testing.T, index4 uint32, index6 uint32) {
	t.Helper()
	previous4, previous6 := EgressInterfaceIndex()
	SetEgressInterfaceIndex(index4, index6)
	t.Cleanup(func() { SetEgressInterfaceIndex(previous4, previous6) })
}

// The interface an IPv4 socket is bound to, 0 for none.
func testBoundInterfaceDarwin(t *testing.T, conn syscall.Conn) int {
	t.Helper()
	raw, err := conn.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var index int
	var readErr error
	if err := raw.Control(func(fd uintptr) {
		index, readErr = unix.GetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_BOUND_IF)
	}); err != nil {
		t.Fatal(err)
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
	return index
}

// The socket of a provider's ICMP echo egress.
func testIcmpEgressSocketDarwin(t *testing.T, egress icmpEgress) syscall.Conn {
	t.Helper()
	conn, ok := egress.(*icmpEgressConn)
	if !ok {
		t.Fatalf("the echo egress is a %T", egress)
	}
	socketConn, ok := conn.conn.IPv4PacketConn().PacketConn.(syscall.Conn)
	if !ok {
		t.Fatalf("the echo socket is a %T", conn.conn.IPv4PacketConn().PacketConn)
	}
	return socketConn
}

// While an egress interface is set, the socket openExtenderPacketConn makes, an
// extender carrier's and the alt api's, and the provider's ICMP echo socket are
// pinned to it before they send; with none set they are made unpinned, as
// before (QD-810).
func TestExtenderAndIcmpEgressSocketsArePinnedDarwin(t *testing.T) {
	loopback, err := net.InterfaceByName("lo0")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	destination := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 443}
	open := func() (int, int) {
		packetConn, err := openExtenderPacketConn(ctx, DefaultConnectSettings(), destination)
		if err != nil {
			t.Fatal(err)
		}
		defer packetConn.Close()
		egress, err := newIcmpEgress(ctx, 4, net.IPv4(127, 0, 0, 1), DefaultIcmpBufferSettings())
		if err != nil {
			t.Fatal(err)
		}
		defer egress.Close()
		return testBoundInterfaceDarwin(t, packetConn.(syscall.Conn)), testBoundInterfaceDarwin(t, testIcmpEgressSocketDarwin(t, egress))
	}

	carrierIndex, icmpIndex := open()
	AssertEqual(t, carrierIndex, 0)
	AssertEqual(t, icmpIndex, 0)

	setTestEgressInterfaceIndexDarwin(t, uint32(loopback.Index), uint32(loopback.Index))
	carrierIndex, icmpIndex = open()
	AssertEqual(t, carrierIndex, loopback.Index)
	AssertEqual(t, icmpIndex, loopback.Index)

	SetEgressInterfaceIndex(0, 0)
	carrierIndex, icmpIndex = open()
	AssertEqual(t, carrierIndex, 0)
	AssertEqual(t, icmpIndex, 0)
}

// An egress interface the sockets cannot be pinned to refuses them, at both
// sites and through every caller -- an extender carrier's QUIC dial, the dns
// carrier's, the alt api's -- before anything is sent, rather than making them
// unpinned (QD-810).
func TestExtenderAndIcmpEgressSocketsRefuseAnUnpinnableInterfaceDarwin(t *testing.T) {
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	unusedIndex := 0
	for _, i := range interfaces {
		unusedIndex = max(unusedIndex, i.Index)
	}
	unusedIndex += 1000
	setTestEgressInterfaceIndexDarwin(t, uint32(unusedIndex), uint32(unusedIndex))
	// a refusal is at once; the bound only keeps a regression, which would dial
	// on, from waiting out a handshake
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	refused := func(name string, err error) {
		if err == nil || !strings.Contains(err.Error(), "egress interface") {
			t.Errorf("%s: an unpinnable socket was not refused: %v", name, err)
		}
	}

	_, err = openExtenderPacketConn(ctx, DefaultConnectSettings(), &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 443})
	refused("openExtenderPacketConn", err)

	for _, connectMode := range []ExtenderConnectMode{ExtenderConnectModeQuic, ExtenderConnectModeDns} {
		_, _, err := DialExtender(
			ctx,
			DefaultConnectSettings(),
			&ExtenderConfig{
				Profile: ExtenderProfile{ConnectMode: connectMode, Port: 443},
				Ip:      netip.MustParseAddr("192.0.2.1"),
			},
			&ExtenderDial{DestinationHost: "dest.example", DestinationPort: 443},
		)
		refused(string(connectMode), err)
	}

	altCtx := (&PlatformTransport{settings: DefaultPlatformTransportSettings()}).dialContext(ctx)
	connectSettings := DefaultConnectSettings()
	wrapCount := 0
	wrap := func(_ context.Context, packetConn net.PacketConn) (net.PacketConn, error) {
		wrapCount += 1
		return packetConn, nil
	}
	_, err = dialAltQuicAttempt(altCtx, connectSettings, &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 443},
		wrap, &tls.Config{}, &quic.Config{}, newExtenderQuicMemoryPolicy(altCtx, connectSettings))
	refused("the alt api", err)
	AssertEqual(t, wrapCount, 0)

	_, err = newIcmpEgress(ctx, 4, net.IPv4(192, 0, 2, 1), DefaultIcmpBufferSettings())
	refused("the ICMP echo egress", err)
}
