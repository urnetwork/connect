//go:build windows

package connect

import (
	"context"
	"errors"
	"net"
	"testing"

	"golang.org/x/sys/windows"
)

// A dual-stack socket accepts the IPv4 option, so a machine with no IPv6 default
// route used to leave its IPv6 traffic unpinned, following the route into the
// tunnel. Index 1 is the loopback interface, present on every host.
func TestApplyEgressInterfacePinsEverySocketFamily(t *testing.T) {
	const index = 1
	sockets := []struct {
		network string
		address string
		is6     bool
	}{
		{"udp4", "127.0.0.1:0", false},
		{"udp6", "[::1]:0", true},
		{"udp", ":0", true},
	}
	for _, c := range []struct{ index4, index6 uint32 }{{index, 0}, {0, index}, {index, index}} {
		for _, socket := range sockets {
			conn, err := net.ListenPacket(socket.network, socket.address)
			if err != nil {
				t.Skipf("%s unavailable: %v", socket.network, err)
			}
			defer conn.Close()
			raw, err := conn.(*net.UDPConn).SyscallConn()
			AssertEqual(t, err, nil)
			var applyErr, readErr error
			var pinned int
			AssertEqual(t, raw.Control(func(fd uintptr) {
				applyErr = applyEgressInterface(fd, c.index4, c.index6, 0)
				if socket.is6 {
					pinned, readErr = windows.GetsockoptInt(windows.Handle(fd), windows.IPPROTO_IPV6, ipv6UnicastIf)
				} else {
					pinned, readErr = windows.GetsockoptInt(windows.Handle(fd), windows.IPPROTO_IP, ipUnicastIf)
				}
			}), nil)
			AssertEqual(t, applyErr, nil)
			AssertEqual(t, readErr, nil)
			AssertEqual(t, pinned, index)
		}
	}
}

// An interface with no IPv6 refuses the IPv6 option while a dual-stack socket
// still accepts the IPv4 one. An IPv6 dial must fail there, not follow the route
// table. A wildcard listener keeps its pinned IPv4 traffic.
func TestEgressControlFailsIpv6DialOnInterfaceWithoutIpv6(t *testing.T) {
	var index uint32
	interfaces, err := net.Interfaces()
	AssertEqual(t, err, nil)
	for _, candidate := range interfaces {
		conn, err := net.ListenPacket("udp", ":0")
		AssertEqual(t, err, nil)
		raw, err := conn.(*net.UDPConn).SyscallConn()
		AssertEqual(t, err, nil)
		var refused bool
		raw.Control(func(fd uintptr) {
			refused = errors.Is(applyEgressInterface(fd, uint32(candidate.Index), 0, 0), errEgressIpv6Unpinned)
		})
		conn.Close()
		if refused {
			index = uint32(candidate.Index)
			break
		}
	}
	if index == 0 {
		t.Skip("every interface on this host has ipv6")
	}
	saved4, saved6 := EgressInterfaceIndex()
	defer SetEgressInterfaceIndex(saved4, saved6)
	SetEgressInterfaceIndex(index, 0)

	_, err = egressDialer(&net.Dialer{}).Dial("udp", "[2001:db8::1]:53")
	AssertEqual(t, errors.Is(err, errEgressIpv6Unpinned), true)

	config := net.ListenConfig{Control: egressControl}
	listener, err := config.ListenPacket(context.Background(), "udp", ":0")
	AssertEqual(t, err, nil)
	listener.Close()
}
