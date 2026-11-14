package connect

import (
	"errors"
	"net"
	"net/netip"
	"sync/atomic"
	"syscall"
)

// Socket self-exclusion for Windows VPN services, controlled macOS peers and a
// Linux daemon that routes the host's default route into a tunnel it provides.
//
// When this process itself provides a VPN tunnel (a DeviceLocal writing a tun
// adapter), its own outbound sockets must not route back into that tunnel, or
// the platform/provider connections loop. Windows has no per-application
// exclusion, so connect binds its outbound sockets to the physical interface
// via IP_UNICAST_IF / IPV6_UNICAST_IF (the wireguard-windows pattern). A
// controlled same-host macOS acceptance provider uses IP_BOUND_IF /
// IPV6_BOUND_IF so its traffic stays outside the app's packet tunnel. A socket
// with a forced egress interface ignores the tun's default route.
//
// On Linux the binding is the egress mark instead (egress_linux.go): SO_MARK on
// every socket at creation, so a policy rule that routes every unmarked socket
// into the tunnel, `not fwmark <mark> lookup <table>` as wg-quick installs it,
// passes this process's own sockets by and they keep the host's route. It
// follows no interface, so a roam needs nothing (QD-800; ur tun under exits).
//
// The binding is a no-op unless an egress index or mark is set. The index is a
// no-op on every platform except Windows and macOS, and the mark on every
// platform except Linux, so ordinary mobile and app builds remain inert.

var egressInterfaceIndex4 atomic.Uint32
var egressInterfaceIndex6 atomic.Uint32
var egressMark atomic.Uint32

// SetEgressInterfaceIndex binds connect's outbound sockets to the given
// physical interface indices (IPv4 and IPv6). Pass 0 for a family to leave that
// family unbound. Safe to call repeatedly, e.g. from a network-change handler.
// Only takes effect on Windows and macOS.
func SetEgressInterfaceIndex(index4 uint32, index6 uint32) {
	egressInterfaceIndex4.Store(index4)
	egressInterfaceIndex6.Store(index6)
}

// EgressInterfaceIndex returns the current (index4, index6). Zero means unset.
func EgressInterfaceIndex() (uint32, uint32) {
	return egressInterfaceIndex4.Load(), egressInterfaceIndex6.Load()
}

// SetEgressMark marks every socket connect creates from now on with this fwmark
// (SO_MARK), so a policy rule that sends unmarked traffic into a tunnel this
// process provides passes connect's own sockets by. 0 clears it, and sockets
// made after that carry no mark; a socket keeps the mark it was made with.
// Setting a mark needs CAP_NET_ADMIN: a socket that cannot carry it fails its
// dial rather than follow the tunnel's route. Safe to call at any time. Only
// takes effect on Linux.
func SetEgressMark(mark uint32) {
	egressMark.Store(mark)
}

// EgressMark returns the current egress mark. Zero means unset.
func EgressMark() uint32 {
	return egressMark.Load()
}

// egressConfigured reports whether an egress index or mark is set, which is
// when the egress machinery touches a socket at all.
func egressConfigured() bool {
	index4, index6 := EgressInterfaceIndex()
	return index4 != 0 || index6 != 0 || EgressMark() != 0
}

// errEgressIpv6Unpinned reports an AF_INET6 socket whose IPv4 traffic is pinned
// but whose IPv6 traffic could not be.
var errEgressIpv6Unpinned = errors.New("connect: could not pin the socket's ipv6 traffic to the egress interface")

// egressControl is a net.Dialer / net.ListenConfig Control callback that applies
// the egress binding, the interface index and the mark as read once here, to a
// socket at creation time.
func egressControl(_ string, address string, c syscall.RawConn) error {
	index4, index6 := EgressInterfaceIndex()
	mark := EgressMark()
	if index4 == 0 && index6 == 0 && mark == 0 {
		return nil
	}
	var innerErr error
	err := c.Control(func(fd uintptr) {
		innerErr = applyEgressInterface(fd, index4, index6, mark)
	})
	if err != nil {
		return err
	}
	if errors.Is(innerErr, errEgressIpv6Unpinned) && !egressAddressIsIpv6Destination(address) {
		return nil
	}
	return innerErr
}

// A dial's control sees its destination. A wildcard listener, or a socket that
// already exists, has none, and its IPv4 traffic is pinned.
func egressAddressIsIpv6Destination(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.Is6() && !ip.Is4In6() && !ip.IsUnspecified()
}

// egressDialer installs the egress control on d, chaining any existing Control.
func egressDialer(d *net.Dialer) *net.Dialer {
	prev := d.Control
	d.Control = func(network string, address string, c syscall.RawConn) error {
		if prev != nil {
			if err := prev(network, address, c); err != nil {
				return err
			}
		}
		return egressControl(network, address, c)
	}
	return d
}

// applyEgress binds an already-created socket-backed conn (e.g. *net.UDPConn) to
// the egress interface or marks it. A no-op when neither an egress index nor a
// mark is set, and where the platform takes neither. The caller applies it
// before the socket sends, since the routing decision is made at the send.
func applyEgress(conn syscall.Conn) error {
	if !egressConfigured() {
		return nil
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	return egressControl("", "", raw)
}
