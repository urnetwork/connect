package connect

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"
)

// The udp of a space whose egress is a proxy (connect#227, support inbox 8395).
//
// A socks proxy (`ConnectSettings.ProxySettings`) carries streams only. Every
// udp socket a space opens is this host's own: a quic or dns extender carrier, an
// alt api dialer, an h3 platform transport, the ice of a webrtc carrier or of a
// p2p stream, an exit flow of the provider's nat. Each would send the space's
// traffic from this host's address, outside the proxy the space is configured
// with -- for a miner proxy slot, from the host every slot shares. So a space
// whose egress is a proxy fails closed for udp, and its tcp keeps going through
// the proxy:
//   - The quic and dns extender carriers dial only through an endpoint the owner
//     supplied (`DialContextSettings.PacketConnFactory`). They dial an ip
//     literal, so nothing is looked up.
//   - The alt api dialers and the h3 platform transport modes are off, endpoint
//     or not: each resolves a platform name on this host before it dials, which
//     is a lookup outside the proxy.
//   - The webrtc carrier is off: ice gathers this host's own candidates.
//   - Any other udp dial through `ConnectSettings.DialContext`, which is how the
//     provider's exit nat reaches a destination, takes the owner's endpoint
//     connected to the destination's ip literal, or is refused
//     (dialProxiedPacket). The exit's tcp goes through the proxy, and its icmp
//     echo is refused, as an echo socket is this host's (owner 2026-11-18,
//     "route through the proxy").
//   - The client opens no p2p stream (`WebRtcSettings.DisablePeerConnections`,
//     which the sdk sets for a client of such a space).
//
// A space whose egress is a dialer of its own (`DialContextSettings`) with no
// proxy keeps that type's contract: a nil PacketConnFactory is the platform's
// ordinary socket.
//
// The carriers are left out where they are chosen -- the strategy's extender
// and alt dialers, the extender network client's feed and probe walks, the
// platform transport's modes -- so one a space cannot dial is never tried and
// never counts against its extender. The sockets refuse too
// (openExtenderPacketConn, dialExtenderWebRtc, openH3PacketConn), whoever
// reaches them, with ErrExtenderPacketCarrierUnavailable, which says nothing
// about the extender.

// ErrExtenderPacketCarrierUnavailable is a udp dial of a space whose egress is a
// proxy with no packet endpoint of its own. Nothing was sent, and the extender
// or the server it was meant for is not at fault.
var ErrExtenderPacketCarrierUnavailable = errors.New("the space's egress is a proxy, which carries no udp")

// Whether the quic and dns extender carriers of these settings have a path: no
// proxy, or an endpoint the owner supplied.
func connectSettingsHavePacketPath(connectSettings *ConnectSettings) bool {
	if connectSettings == nil || connectSettings.ProxySettings == nil {
		return true
	}
	return connectSettings.DialContextSettings != nil && connectSettings.DialContextSettings.PacketConnFactory != nil
}

// Whether a carrier of this connect mode can be dialed under these settings.
func extenderConnectModeDialable(connectSettings *ConnectSettings, connectMode ExtenderConnectMode) bool {
	switch connectMode {
	case ExtenderConnectModeQuic, ExtenderConnectModeDns:
		return connectSettingsHavePacketPath(connectSettings)
	case ExtenderConnectModeWebRtc:
		// ice opens its own sockets, whatever endpoint the owner supplied
		return connectSettings == nil || connectSettings.ProxySettings == nil
	default:
		return true
	}
}

// The configs of `extenderConfigs` these settings can dial, in order; a carrier
// the space's egress cannot carry is left out.
func dialableExtenderConfigs(connectSettings *ConnectSettings, extenderConfigs []*ExtenderConfig) []*ExtenderConfig {
	dialableConfigs := make([]*ExtenderConfig, 0, len(extenderConfigs))
	for _, extenderConfig := range extenderConfigs {
		if extenderConnectModeDialable(connectSettings, extenderConfig.Profile.ConnectMode) {
			dialableConfigs = append(dialableConfigs, extenderConfig)
		}
	}
	return dialableConfigs
}

// Whether a dial network is a datagram one.
func isDatagramDialNetwork(network string) bool {
	switch network {
	case "udp", "udp4", "udp6":
		return true
	default:
		return false
	}
}

// A datagram dial of a space whose egress is a proxy: the owner's packet
// endpoint (`DialContextSettings.PacketConnFactory`) connected to `addr`, or
// ErrExtenderPacketCarrierUnavailable when the owner supplied none. No socket of
// this host's is opened, so nothing leaves outside the proxy. `addr` must be an
// ip literal of the network's family: a name would be looked up on this host.
func (self *ConnectSettings) dialProxiedPacket(ctx context.Context, network string, addr string) (net.Conn, error) {
	if self.DialContextSettings == nil || self.DialContextSettings.PacketConnFactory == nil {
		return nil, ErrExtenderPacketCarrierUnavailable
	}
	addrPort, err := netip.ParseAddrPort(addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s %s: a proxied space dials udp to an ip literal alone: %w", network, addr, ErrExtenderPacketCarrierUnavailable)
	}
	addrPort = netip.AddrPortFrom(addrPort.Addr().Unmap(), addrPort.Port())
	if (network == "udp4" && !addrPort.Addr().Is4()) || (network == "udp6" && !addrPort.Addr().Is6()) {
		return nil, fmt.Errorf("dial %s %s: the address is not of the network's family", network, addr)
	}
	packetConn, err := self.DialContextSettings.PacketConnFactory(ctx)
	if err != nil {
		// the owner's endpoint is closed by its caller, even beside an error
		if packetConn != nil {
			packetConn.Close()
		}
		return nil, err
	}
	return newConnectedPacketConn(packetConn, net.UDPAddrFromAddrPort(addrPort)), nil
}

// An unconnected packet endpoint the owner supplied, connected to one remote
// address the way a dialed udp socket is: a write goes to the address, and a
// read returns only what the address sent, any other sender's datagram dropped
// as the kernel drops it for a connected socket. Closing it closes the endpoint.
// It is safe for one reader and one writer at once, as the endpoint is.
type connectedPacketConn struct {
	packetConn net.PacketConn
	remoteAddr *net.UDPAddr
	remote     netip.AddrPort
}

func newConnectedPacketConn(packetConn net.PacketConn, remoteAddr *net.UDPAddr) *connectedPacketConn {
	remote := remoteAddr.AddrPort()
	return &connectedPacketConn{
		packetConn: packetConn,
		remoteAddr: remoteAddr,
		remote:     netip.AddrPortFrom(remote.Addr().Unmap(), remote.Port()),
	}
}

// Reads the next datagram from the remote address. Each pass waits in the
// endpoint's read, which the read deadline and Close end, so a stream of other
// senders' datagrams cannot spin it.
func (self *connectedPacketConn) Read(b []byte) (int, error) {
	for {
		n, addr, err := self.packetConn.ReadFrom(b)
		if err != nil {
			return n, err
		}
		if packetAddrMatches(addr, self.remote) {
			return n, nil
		}
	}
}

func (self *connectedPacketConn) Write(b []byte) (int, error) {
	return self.packetConn.WriteTo(b, self.remoteAddr)
}

func (self *connectedPacketConn) Close() error {
	return self.packetConn.Close()
}

func (self *connectedPacketConn) LocalAddr() net.Addr {
	return self.packetConn.LocalAddr()
}

func (self *connectedPacketConn) RemoteAddr() net.Addr {
	return self.remoteAddr
}

func (self *connectedPacketConn) SetDeadline(t time.Time) error {
	return self.packetConn.SetDeadline(t)
}

func (self *connectedPacketConn) SetReadDeadline(t time.Time) error {
	return self.packetConn.SetReadDeadline(t)
}

func (self *connectedPacketConn) SetWriteDeadline(t time.Time) error {
	return self.packetConn.SetWriteDeadline(t)
}

// Whether a datagram's sender is `remote`. An endpoint of the owner's may name
// the sender with its own address type, so anything but a *net.UDPAddr is read
// from its text.
func packetAddrMatches(addr net.Addr, remote netip.AddrPort) bool {
	var addrPort netip.AddrPort
	switch v := addr.(type) {
	case *net.UDPAddr:
		addrPort = v.AddrPort()
	case nil:
		return false
	default:
		parsed, err := netip.ParseAddrPort(v.String())
		if err != nil {
			return false
		}
		addrPort = parsed
	}
	return netip.AddrPortFrom(addrPort.Addr().Unmap(), addrPort.Port()) == remote
}
