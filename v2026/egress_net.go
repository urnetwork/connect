//go:build !js

package connect

import (
	"net"
	"syscall"

	"github.com/pion/transport/v4"
	"github.com/pion/transport/v4/stdnet"
)

// egressNet is a pion transport.Net that binds the UDP sockets pion creates for
// ICE to the egress interface, so p2p (webrtc) traffic does not loop back into
// the tunnel this process provides (R1). It embeds the standard net and only
// overrides socket creation; a no-op unless an egress index is set / off
// Windows.
type egressNet struct {
	transport.Net
}

func newEgressNet() (transport.Net, error) {
	base, err := stdnet.NewNet()
	if err != nil {
		return nil, err
	}
	return &egressNet{Net: base}, nil
}

func (self *egressNet) ListenUDP(network string, locAddr *net.UDPAddr) (transport.UDPConn, error) {
	conn, err := self.Net.ListenUDP(network, locAddr)
	if err != nil {
		return nil, err
	}
	if sc, ok := conn.(syscall.Conn); ok {
		_ = applyEgress(sc)
	}
	return conn, nil
}

func (self *egressNet) ListenPacket(network string, address string) (net.PacketConn, error) {
	conn, err := self.Net.ListenPacket(network, address)
	if err != nil {
		return nil, err
	}
	if sc, ok := conn.(syscall.Conn); ok {
		_ = applyEgress(sc)
	}
	return conn, nil
}
