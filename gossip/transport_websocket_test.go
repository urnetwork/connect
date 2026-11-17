// The address handling of the websocket dial transport (transport_websocket.go).
//
// Nothing here opens a connection: what the dialer is handed, and what the tls
// verifies, are decided from the multiaddr alone.

package gossip

import (
	"context"
	"fmt"
	"strings"
	"testing"

	ma "github.com/multiformats/go-multiaddr"
)

// The dialer gets the address's own name or ip and family, unresolved; the
// url names the server the tls and the host header expect; and the tls
// verifies the sni, else the dns name, else the ip.
func TestGossipWebsocketDialTargetForAddr(t *testing.T) {
	operatorPeerId, err := PeerIdForExtenderPublicKey(newTestKey(t).publicKey)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		addr             string
		expectNetwork    string
		expectAddress    string
		expectUrl        string
		expectSecure     bool
		expectServerName string
		expectErr        string
	}{
		{
			addr:             "/dns/gossip.space.example/tcp/443/wss",
			expectNetwork:    "tcp",
			expectAddress:    "gossip.space.example:443",
			expectUrl:        "wss://gossip.space.example:443",
			expectSecure:     true,
			expectServerName: "gossip.space.example",
		},
		// the swarm strips /p2p before dialing, but an address built by hand
		// still carries it
		{
			addr:             fmt.Sprintf("/dns/gossip.space.example/tcp/443/wss/p2p/%s", operatorPeerId),
			expectNetwork:    "tcp",
			expectAddress:    "gossip.space.example:443",
			expectUrl:        "wss://gossip.space.example:443",
			expectSecure:     true,
			expectServerName: "gossip.space.example",
		},
		// a family-pinned name keeps its family
		{
			addr:             "/dns6/gossip.space.example/tcp/443/tls/ws",
			expectNetwork:    "tcp6",
			expectAddress:    "gossip.space.example:443",
			expectUrl:        "wss://gossip.space.example:443",
			expectSecure:     true,
			expectServerName: "gossip.space.example",
		},
		{
			addr:             "/ip4/192.0.2.1/tcp/8080/ws",
			expectNetwork:    "tcp4",
			expectAddress:    "192.0.2.1:8080",
			expectUrl:        "ws://192.0.2.1:8080",
			expectServerName: "192.0.2.1",
		},
		{
			addr:             "/ip6/2001:db8::1/tcp/443/wss",
			expectNetwork:    "tcp6",
			expectAddress:    "[2001:db8::1]:443",
			expectUrl:        "wss://[2001:db8::1]:443",
			expectSecure:     true,
			expectServerName: "2001:db8::1",
		},
		// an sni names the server, and the endpoint is still the ip
		{
			addr:             "/ip4/192.0.2.1/tcp/443/tls/sni/gossip.space.example/ws",
			expectNetwork:    "tcp4",
			expectAddress:    "192.0.2.1:443",
			expectUrl:        "wss://gossip.space.example:443",
			expectSecure:     true,
			expectServerName: "gossip.space.example",
		},
		{addr: "/ip4/192.0.2.1/tcp/443", expectErr: "is not a websocket address"},
		{addr: "/ip4/192.0.2.1/tcp/443/tls", expectErr: "is not a websocket address"},
		{addr: "/ip4/192.0.2.1/udp/443/quic-v1", expectErr: "is not a websocket address"},
	}
	for _, c := range cases {
		addr, err := ma.NewMultiaddr(c.addr)
		if err != nil {
			t.Errorf("%s: %v", c.addr, err)
			continue
		}
		target, err := websocketDialTargetForAddr(addr)
		if c.expectErr != "" {
			if err == nil {
				t.Errorf("%s: expected an error, got %+v", c.addr, target)
				continue
			}
			if !strings.Contains(err.Error(), c.expectErr) {
				t.Errorf("%s: error = %v, expected %q", c.addr, err, c.expectErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.addr, err)
			continue
		}
		if target.network != c.expectNetwork {
			t.Errorf("%s: network = %q, expected %q", c.addr, target.network, c.expectNetwork)
		}
		if target.address != c.expectAddress {
			t.Errorf("%s: address = %q, expected %q", c.addr, target.address, c.expectAddress)
		}
		if target.url != c.expectUrl {
			t.Errorf("%s: url = %q, expected %q", c.addr, target.url, c.expectUrl)
		}
		if target.secure != c.expectSecure {
			t.Errorf("%s: secure = %v, expected %v", c.addr, target.secure, c.expectSecure)
		}
		if target.serverName != c.expectServerName {
			t.Errorf("%s: server name = %q, expected %q", c.addr, target.serverName, c.expectServerName)
		}
	}
}

// The transport takes the websocket addresses libp2p's own transport takes and
// nothing else, so the extender carrier keeps every plain tcp address, and it
// keeps every name away from this host's resolver.
func TestGossipWebsocketDialTransportDialsOnlyWebsocketAddrs(t *testing.T) {
	dialTransport, err := newWebsocketDialTransport(&websocketDialTransportSettings{
		DialContext: (&testRecordingDialer{}).DialContext,
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		addr          string
		expectCanDial bool
	}{
		{addr: "/dns/gossip.space.example/tcp/443/wss", expectCanDial: true},
		{addr: "/dns4/gossip.space.example/tcp/443/wss", expectCanDial: true},
		{addr: "/ip4/192.0.2.1/tcp/80/ws", expectCanDial: true},
		{addr: "/ip6/2001:db8::1/tcp/443/tls/ws", expectCanDial: true},
		{addr: "/ip4/192.0.2.1/tcp/443/tls/sni/gossip.space.example/ws", expectCanDial: true},
		{addr: "/ip4/192.0.2.1/tcp/443", expectCanDial: false},
		{addr: "/ip4/192.0.2.1/udp/443/quic-v1", expectCanDial: false},
		{addr: "/dns/gossip.space.example/tcp/443", expectCanDial: false},
	}
	for _, c := range cases {
		addr, err := ma.NewMultiaddr(c.addr)
		if err != nil {
			t.Errorf("%s: %v", c.addr, err)
			continue
		}
		if canDial := dialTransport.CanDial(addr); canDial != c.expectCanDial {
			t.Errorf("%s: can dial = %v, expected %v", c.addr, canDial, c.expectCanDial)
		}
		if c.expectCanDial && !dialTransport.SkipResolve(context.Background(), addr) {
			t.Errorf("%s: the swarm would resolve the name on this host", c.addr)
		}
	}

	// it is a dialer's transport; with nothing to listen through it refuses
	listenAddr, err := ma.NewMultiaddr("/ip4/127.0.0.1/tcp/0/ws")
	if err != nil {
		t.Fatal(err)
	}
	if listener, err := dialTransport.Listen(listenAddr); err == nil {
		listener.Close()
		t.Fatal("the websocket dial transport listened with no listen transport")
	}
	if _, err := newWebsocketDialTransport(&websocketDialTransportSettings{}); err == nil {
		t.Fatal("a websocket dial transport was built with no dialer")
	}
}
