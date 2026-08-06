//go:build windows

package connect

import (
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

func htonl(x uint32) uint32 {
	return (x&0x000000ff)<<24 | (x&0x0000ff00)<<8 | (x&0x00ff0000)>>8 | (x&0xff000000)>>24
}

// applyEgressInterface sets the forced egress interface on the socket. Both
// families are set best-effort: the option for the wrong family on a
// single-family socket fails harmlessly, so per-option errors are ignored (as
// in wireguard-windows). This guarantees the call never breaks connection
// setup even when only one family applies.
func applyEgressInterface(fd uintptr, index4 uint32, index6 uint32) error {
	h := windows.Handle(fd)
	if index4 != 0 {
		_ = windows.SetsockoptInt(h, windows.IPPROTO_IP, ipUnicastIf, int(htonl(index4)))
	}
	if index6 != 0 {
		_ = windows.SetsockoptInt(h, windows.IPPROTO_IPV6, ipv6UnicastIf, int(index6))
	}
	return nil
}
