//go:build windows

package connect

import (
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// IP_UNICAST_IF (IPPROTO_IP, option 31) forces the outgoing interface for
// unicast, overriding the route table. The interface index must be passed in
// NETWORK byte order. IPV6_UNICAST_IF (IPPROTO_IPV6, option 31) does the same
// for IPv6 but takes the index in HOST byte order. This asymmetry matches
// wireguard-windows' bindSocketRoute.
const (
	ipUnicastIf   = 31
	ipv6UnicastIf = 31
)

// SO_PROTOCOL_INFOW, which x/sys/windows does not name.
const soProtocolInfoW = 0x2005

// The other family's options cannot tell: IPPROTO_IPV6 reads succeed on AF_INET.
func socketIsIpv6(h windows.Handle) (bool, error) {
	var info windows.WSAProtocolInfo
	size := int32(unsafe.Sizeof(info))
	if err := syscall.Getsockopt(
		syscall.Handle(h),
		windows.SOL_SOCKET,
		soProtocolInfoW,
		(*byte)(unsafe.Pointer(&info)),
		&size,
	); err != nil {
		return false, err
	}
	return info.AddressFamily == windows.AF_INET6, nil
}

func htonl(x uint32) uint32 {
	return (x&0x000000ff)<<24 | (x&0x0000ff00)<<8 | (x&0x00ff0000)>>8 | (x&0xff000000)>>24
}

// applyEgressInterface sets the forced egress interface on the socket.
//
// The caller does not know the socket's family, and setting the other family's
// option does not reliably tell: a single-family socket refuses it, but a
// dual-stack AF_INET6 socket, which is what a "udp" or "tcp" dial to an IPv6
// address makes, accepts IP_UNICAST_IF and stays unpinned for IPv6. So the IPv6
// option is tried first. It succeeds only on an AF_INET6 socket on an interface
// with IPv6, and the IPv4 option then covers that socket's mapped traffic.
//
// A family with no default route of its own is pinned to the other family's
// interface. When that interface has no IPv6 at all the IPv6 option is refused,
// and an AF_INET6 socket is reported as errEgressIpv6Unpinned. A socket with no
// forced interface follows the route table, which on the machine this exists
// for is the tunnel this very process provides: each datagram comes back as a
// new flow and opens another socket.
//
// The error travels back through egressControl to net.Dialer.Control, so the
// dial fails and shows up in the existing [net]dial log line. A failed dial
// retries; an unpinned socket blackholes. The egress mark is Linux's
// (egress_linux.go) and has no effect here.
func applyEgressInterface(fd uintptr, index4 uint32, index6 uint32, _ uint32) error {
	if index4 == 0 && index6 == 0 {
		return nil
	}
	if index4 == 0 {
		index4 = index6
	}
	if index6 == 0 {
		index6 = index4
	}
	h := windows.Handle(fd)
	err6 := windows.SetsockoptInt(h, windows.IPPROTO_IPV6, ipv6UnicastIf, int(index6))
	err4 := windows.SetsockoptInt(h, windows.IPPROTO_IP, ipUnicastIf, int(htonl(index4)))
	is6, err := socketIsIpv6(h)
	if err != nil {
		return fmt.Errorf("connect: could not read the socket family: %w", err)
	}
	switch {
	case is6 && err6 == nil, !is6 && err4 == nil:
		return nil
	case is6 && err4 == nil:
		return fmt.Errorf("%w (v4=%d v6=%d): %w", errEgressIpv6Unpinned, index4, index6, err6)
	}
	err = err4
	if is6 {
		err = err6
	}
	return fmt.Errorf(
		"connect: could not pin the socket to the egress interface (v4=%d v6=%d): %w; "+
			"the socket would follow the route table and loop into the tunnel",
		index4,
		index6,
		err,
	)
}
