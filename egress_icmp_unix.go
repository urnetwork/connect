//go:build !windows && !js

// The egress machinery's datagram icmp socket, the one the provider's echo
// egress off Windows sends through (ip_icmp_egress_unix.go). Windows has no echo
// socket to bind: its transaction takes its source from the egress interface
// (ip_icmp_egress_windows.go). Here the socket is made and then bound before its
// first send, the forced interface on macOS and the egress mark on Linux, so an
// echo a provider serves for a peer never follows a tunnel this process provides
// back into it (QD-800, QD-810).
package connect

import (
	"fmt"
	"net"
	"syscall"

	"golang.org/x/net/icmp"
)

// listenEgressIcmp makes one unprivileged datagram icmp socket (`SOCK_DGRAM`
// with `IPPROTO_ICMP` for "udp4", `IPPROTO_ICMPV6` for "udp6") on the wildcard
// address, for echoes to destinationIp alone, and binds it through the egress
// machinery before it is returned. The socket is unconnected, so the kernel
// routes each send by the binding in force at that send: bound before the first
// one, every echo leaves outside the tunnel. A socket that cannot take the
// binding is closed and refused, as a dial is, and the flow fails soft (see
// IcmpSequence.Run). With neither an egress index nor a mark set the socket is
// made exactly as icmp.ListenPacket makes it.
func listenEgressIcmp(network string, destinationIp net.IP) (*icmp.PacketConn, error) {
	conn, err := icmp.ListenPacket(network, "")
	if err != nil {
		return nil, err
	}
	if !egressConfigured() {
		return conn, nil
	}
	// icmp.PacketConn keeps its socket behind the per-family conn it wraps; the
	// datagram listen made it a *net.UDPConn
	var socketConn syscall.Conn
	if p := conn.IPv4PacketConn(); p != nil {
		socketConn, _ = p.PacketConn.(syscall.Conn)
	} else if p := conn.IPv6PacketConn(); p != nil {
		socketConn, _ = p.PacketConn.(syscall.Conn)
	}
	if socketConn == nil {
		conn.Close()
		return nil, fmt.Errorf("connect: the %s icmp socket has no descriptor to bind to the egress interface", network)
	}
	// the port is a placeholder: the destination's family is all egressControl reads
	destination := ""
	if destinationIp != nil {
		destination = net.JoinHostPort(destinationIp.String(), "0")
	}
	if err := applyEgressFor(socketConn, destination); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}
