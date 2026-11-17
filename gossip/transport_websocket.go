// The websocket transport every node dials with (EXTENDER.md D2, connect#227).
//
// libp2p's websocket transport takes no dialer. It resolves the operator's
// name with this host's resolver and opens the socket with net.DialTCP, so the
// node of a miner proxy slot reached the operator from the host's own address
// rather than through the slot's proxy, and an app's node dialed outside the
// space's egress binding (R1) and DialControl. Every node dials ws and wss
// addresses here instead, through the space's dial (`ConnectSettings.DialContext`)
// or the dialer its owner supplies (`NodeSettings.DialContext`), and every step
// of the link goes through that dial:
//   - The swarm hands the address over unresolved (SkipResolve), so the dial
//     gets the operator's name: a socks proxy resolves it on its own side, and
//     a direct dial resolves it the way every other dial of the space does.
//   - tls runs over the dial's stream, verified against the space's roots
//     (ConnectSettings.TlsConfig), with the process's session cache
//     (tls_session_cache.go), so a redial resumes rather than verifying the
//     whole chain again.
//   - The websocket upgrade follows on the same stream, then noise and yamux,
//     exactly as over libp2p's transport.
//
// Listening stays with libp2p's transport. An inbound connection is not a
// dial.
//
// The transport is safe for concurrent use.

package gossip

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/transport"

	ma "github.com/multiformats/go-multiaddr"
	mafmt "github.com/multiformats/go-multiaddr-fmt"
	manet "github.com/multiformats/go-multiaddr/net"

	ws "github.com/gorilla/websocket"

	"github.com/urnetwork/connect"
)

// The addresses libp2p's websocket transport dials: an ip or a dns name, tcp,
// then ws, wss, tls/ws or tls/sni/<name>/ws.
var websocketDialMatcher = mafmt.And(
	mafmt.Or(mafmt.IP, mafmt.DNS),
	mafmt.Base(ma.P_TCP),
	mafmt.Or(
		mafmt.Base(ma.P_WS),
		mafmt.And(
			mafmt.Or(
				mafmt.And(mafmt.Base(ma.P_TLS), mafmt.Base(ma.P_SNI)),
				mafmt.Base(ma.P_TLS),
			),
			mafmt.Base(ma.P_WS),
		),
		mafmt.Base(ma.P_WSS),
	),
)

type websocketDialTransportSettings struct {
	// Every connection this transport makes goes through it.
	DialContext connect.DialContextFunction
	// The roots and versions of a wss dial. Nil is crypto/tls's defaults. The
	// server name, the application protocol and the session cache are set per
	// dial.
	TlsConfig *tls.Config
	// Shared by every wss dial. Nil resumes nothing.
	TlsSessionCache tls.ClientSessionCache

	// Supplied by libp2p when the transport is constructed.
	Upgrader        transport.Upgrader
	ResourceManager network.ResourceManager
	// libp2p's websocket transport, which listens for this one. Nil refuses
	// every Listen.
	ListenTransport transport.Transport
}

type websocketDialTransport struct {
	settings *websocketDialTransportSettings
}

var _ transport.Transport = (*websocketDialTransport)(nil)
var _ transport.SkipResolver = (*websocketDialTransport)(nil)

func newWebsocketDialTransport(settings *websocketDialTransportSettings) (*websocketDialTransport, error) {
	if settings == nil || settings.DialContext == nil {
		return nil, fmt.Errorf("the websocket dial transport needs a dialer")
	}
	copied := *settings
	if copied.ResourceManager == nil {
		copied.ResourceManager = &network.NullResourceManager{}
	}
	return &websocketDialTransport{
		settings: &copied,
	}, nil
}

func (self *websocketDialTransport) CanDial(addr ma.Multiaddr) bool {
	return websocketDialMatcher.Matches(addr)
}

func (self *websocketDialTransport) Protocols() []int {
	return []int{ma.P_WS, ma.P_WSS}
}

func (self *websocketDialTransport) Proxy() bool {
	return false
}

func (self *websocketDialTransport) String() string {
	return "WebsocketDial"
}

// Always true: the swarm would otherwise resolve a dns address with this
// host's resolver before the dial, a lookup outside the dialer. The name goes
// to the dialer as it is.
func (self *websocketDialTransport) SkipResolve(ctx context.Context, addr ma.Multiaddr) bool {
	return true
}

func (self *websocketDialTransport) Listen(laddr ma.Multiaddr) (transport.Listener, error) {
	if self.settings.ListenTransport == nil {
		return nil, fmt.Errorf("the websocket dial transport cannot listen on %s", laddr)
	}
	return self.settings.ListenTransport.Listen(laddr)
}

// Dial opens the upgrade at `raddr` through the dialer and upgrades the stream
// to `peerId`, which noise then authenticates.
func (self *websocketDialTransport) Dial(
	ctx context.Context,
	raddr ma.Multiaddr,
	peerId peer.ID,
) (transport.CapableConn, error) {
	target, err := websocketDialTargetForAddr(raddr)
	if err != nil {
		return nil, err
	}
	connScope, err := self.settings.ResourceManager.OpenConnection(network.DirOutbound, true, raddr)
	if err != nil {
		return nil, err
	}
	capableConn, err := self.dialWithScope(ctx, raddr, target, peerId, connScope)
	if err != nil {
		connScope.Done()
		return nil, err
	}
	return capableConn, nil
}

func (self *websocketDialTransport) dialWithScope(
	ctx context.Context,
	raddr ma.Multiaddr,
	target *websocketDialTarget,
	peerId peer.ID,
	connScope network.ConnManagementScope,
) (transport.CapableConn, error) {
	if err := connScope.SetPeer(peerId); err != nil {
		return nil, err
	}
	dialer := &ws.Dialer{
		// The one dial of the upgrade is the address's own. The url names the
		// server the tls and the host header expect, which for a
		// /tls/sni/<name> address is not the endpoint dialed.
		NetDialContext: func(ctx context.Context, _ string, _ string) (net.Conn, error) {
			return self.settings.DialContext(ctx, target.network, target.address)
		},
		// no proxy from the environment: the dialer is the route
		Proxy: nil,
	}
	if target.secure {
		tlsConfig := &tls.Config{}
		if self.settings.TlsConfig != nil {
			tlsConfig = self.settings.TlsConfig.Clone()
		}
		tlsConfig.ServerName = target.serverName
		// the upgrade is an http/1.1 request; a base config shared with an http
		// client may offer h2, which the upgrade cannot speak
		tlsConfig.NextProtos = []string{"http/1.1"}
		tlsConfig.ClientSessionCache = self.settings.TlsSessionCache
		dialer.TLSClientConfig = tlsConfig
	}
	// the swarm's dial timeout bounds the whole upgrade through ctx
	wsConn, _, err := dialer.DialContext(ctx, target.url, nil)
	if err != nil {
		return nil, err
	}
	conn := newWebsocketConn(wsConn)
	success := false
	defer func() {
		if !success {
			conn.Close()
		}
	}()

	localMultiaddr := raddr
	if localAddr := conn.LocalAddr(); localAddr != nil {
		if converted, err := manet.FromNetAddr(localAddr); err == nil {
			localMultiaddr = converted
		}
	}
	capableConn, err := self.settings.Upgrader.Upgrade(
		ctx,
		self,
		newMaConn(conn, localMultiaddr, raddr),
		network.DirOutbound,
		peerId,
		connScope,
	)
	if err != nil {
		return nil, err
	}
	success = true
	return capableConn, nil
}

// What one websocket address dials.
type websocketDialTarget struct {
	// what the dialer gets: the address's own family and its name or ip,
	// unresolved
	network string
	address string
	// the url of the upgrade, whose host is the host header
	url    string
	secure bool
	// the name the tls presents and verifies: the sni of the address, else
	// its dns name, else its ip
	serverName string
}

// The dial of one ws or wss address. A trailing /p2p is not part of the
// endpoint.
func websocketDialTargetForAddr(addr ma.Multiaddr) (*websocketDialTarget, error) {
	// the swarm strips /p2p before dialing, but an address built by hand may
	// still carry it
	baseAddr, _ := peer.SplitAddr(addr)
	if baseAddr == nil {
		baseAddr = addr
	}
	// the endpoint ends at tcp; what follows is the websocket
	endpointCount := len(baseAddr)
	for i, component := range baseAddr {
		if component.Protocol().Code == ma.P_TCP {
			endpointCount = i + 1
			break
		}
	}
	endpointAddr := baseAddr[:endpointCount]
	suffixCodes := []int{}
	sni := ""
	for _, component := range baseAddr[endpointCount:] {
		suffixCodes = append(suffixCodes, component.Protocol().Code)
		if component.Protocol().Code == ma.P_SNI {
			sni = component.Value()
		}
	}

	secure := false
	switch {
	case slices.Equal(suffixCodes, []int{ma.P_WS}):
	case slices.Equal(suffixCodes, []int{ma.P_WSS}),
		slices.Equal(suffixCodes, []int{ma.P_TLS, ma.P_WS}),
		slices.Equal(suffixCodes, []int{ma.P_TLS, ma.P_SNI, ma.P_WS}):
		secure = true
	default:
		return nil, fmt.Errorf("%s is not a websocket address", addr)
	}

	dialNetwork, dialAddress, err := manet.DialArgs(endpointAddr)
	if err != nil {
		return nil, err
	}
	switch dialNetwork {
	case "tcp", "tcp4", "tcp6":
	default:
		return nil, fmt.Errorf("the websocket dial transport cannot dial %s", dialNetwork)
	}
	host, port, err := net.SplitHostPort(dialAddress)
	if err != nil {
		return nil, err
	}
	serverName := host
	if sni != "" {
		serverName = sni
	}
	scheme := "ws"
	if secure {
		scheme = "wss"
	}
	upgradeUrl := &url.URL{
		Scheme: scheme,
		Host:   net.JoinHostPort(serverName, port),
	}
	return &websocketDialTarget{
		network:    dialNetwork,
		address:    dialAddress,
		url:        upgradeUrl.String(),
		secure:     secure,
		serverName: serverName,
	}, nil
}

// A websocket connection as the byte stream the upgrader runs over: each write
// is one binary message, and reads run across message boundaries. Closing drops
// the stream without a close frame; yamux's goaway already says the session is
// over.
//
// Read and Write may be called concurrently with each other. Concurrent reads
// are serialized, and so are concurrent writes.
type websocketConn struct {
	wsConn *ws.Conn

	readLock sync.Mutex
	// the message being read, held only under readLock
	reader io.Reader

	// also guards the write deadline, which the websocket keeps for its
	// writer rather than setting on the socket
	writeLock sync.Mutex
}

var _ net.Conn = (*websocketConn)(nil)

func newWebsocketConn(wsConn *ws.Conn) *websocketConn {
	return &websocketConn{
		wsConn: wsConn,
	}
}

func (self *websocketConn) Read(b []byte) (int, error) {
	self.readLock.Lock()
	defer self.readLock.Unlock()
	for {
		if self.reader == nil {
			messageType, reader, err := self.wsConn.NextReader()
			if err != nil {
				// a normal close, or one with no status, is the end of the
				// stream; any other close is an error, as libp2p's own
				// transport reads it
				var closeErr *ws.CloseError
				if errors.As(err, &closeErr) &&
					(closeErr.Code == ws.CloseNormalClosure || closeErr.Code == ws.CloseNoStatusReceived) {
					return 0, io.EOF
				}
				return 0, err
			}
			if messageType != ws.BinaryMessage && messageType != ws.TextMessage {
				continue
			}
			self.reader = reader
		}
		n, err := self.reader.Read(b)
		if errors.Is(err, io.EOF) {
			self.reader = nil
			if 0 < n {
				return n, nil
			}
			continue
		}
		return n, err
	}
}

func (self *websocketConn) Write(b []byte) (int, error) {
	self.writeLock.Lock()
	defer self.writeLock.Unlock()
	if err := self.wsConn.WriteMessage(ws.BinaryMessage, b); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (self *websocketConn) Close() error {
	return self.wsConn.Close()
}

func (self *websocketConn) LocalAddr() net.Addr {
	return self.wsConn.LocalAddr()
}

func (self *websocketConn) RemoteAddr() net.Addr {
	return self.wsConn.RemoteAddr()
}

func (self *websocketConn) SetDeadline(t time.Time) error {
	if err := self.SetReadDeadline(t); err != nil {
		return err
	}
	return self.SetWriteDeadline(t)
}

// Not under readLock, so a deadline can end a read in progress.
func (self *websocketConn) SetReadDeadline(t time.Time) error {
	return self.wsConn.SetReadDeadline(t)
}

func (self *websocketConn) SetWriteDeadline(t time.Time) error {
	self.writeLock.Lock()
	defer self.writeLock.Unlock()
	return self.wsConn.SetWriteDeadline(t)
}
