// A node whose owner supplies a dialer (NodeSettings.DialContext, connect#227).
//
// A miner proxy slot's mesh traffic must all leave through its proxy. Each test
// tells a node where its peer is in a way only the dialer resolves to the real
// peer, and puts a canary where a dial outside the dialer would land: the
// address the node was told, or ConnectSettings' own route. The canary counts
// what reaches it and closes it, so a dial outside the dialer fails the test at
// once, by name, rather than at a timeout.
//
// The names are .example names and localhost; the addresses are loopback and
// RFC 5737 documentation addresses.

package gossip

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"math/big"
	mathrand "math/rand/v2"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/p2p/net/swarm"
	websocket "github.com/libp2p/go-libp2p/p2p/transport/websocket"

	ma "github.com/multiformats/go-multiaddr"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/extender"
)

// A dialer that records every dial and connects where `route` says, which is
// all a proxy is to the node that dials through it.
type testRecordingDialer struct {
	// the real address of a dialed address, or an error for one the proxy
	// cannot reach. Nil refuses everything.
	route func(network string, address string) (string, error)

	stateLock sync.Mutex
	// "<network> <address>" of every dial, in order
	dials []string
	// the local port of every connection it made
	localPorts map[string]bool
}

func (self *testRecordingDialer) DialContext(ctx context.Context, network string, address string) (net.Conn, error) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.dials = append(self.dials, network+" "+address)
	}()
	if self.route == nil {
		return nil, fmt.Errorf("no route to %s", address)
	}
	routedAddress, err := self.route(network, address)
	if err != nil {
		return nil, err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", routedAddress)
	if err != nil {
		return nil, err
	}
	_, localPort, err := net.SplitHostPort(conn.LocalAddr().String())
	if err != nil {
		conn.Close()
		return nil, err
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.localPorts == nil {
			self.localPorts = map[string]bool{}
		}
		self.localPorts[localPort] = true
	}()
	return conn, nil
}

func (self *testRecordingDialer) dialsCopy() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.dials)
}

func (self *testRecordingDialer) madeLocalPort(port string) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.localPorts[port]
}

// A listener at an address a node may be told but must never reach itself.
// Each connection is counted and closed at once.
type testCanaryListener struct {
	listener net.Listener
	// receives on the first connection, never blocking the accept loop
	accepted      chan struct{}
	acceptedCount atomic.Int64
}

func newTestCanaryListener(t *testing.T) *testCanaryListener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	canary := &testCanaryListener{
		listener: listener,
		accepted: make(chan struct{}, 1),
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			canary.acceptedCount.Add(1)
			select {
			case canary.accepted <- struct{}{}:
			default:
			}
			conn.Close()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		<-done
	})
	return canary
}

func (self *testCanaryListener) port() int {
	return self.listener.Addr().(*net.TCPAddr).Port
}

func (self *testCanaryListener) address() string {
	return self.listener.Addr().String()
}

// A tls front for the operator's plain websocket listener, as the operator's lb
// is (C6): it terminates tls 1.3 for the operator's name, issues session
// tickets, reports whether each handshake resumed, and pipes the stream on.
type testTlsFront struct {
	listener net.Listener
	// one value per completed handshake: whether it resumed
	handshakes chan bool
}

func newTestTlsFront(t *testing.T, certificate tls.Certificate, backendAddress string) *testTlsFront {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	front := &testTlsFront{
		listener:   listener,
		handshakes: make(chan bool, 64),
	}
	config := &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS13,
	}

	connsLock := sync.Mutex{}
	conns := []net.Conn{}
	track := func(conn net.Conn) bool {
		connsLock.Lock()
		defer connsLock.Unlock()
		if conns == nil {
			return false
		}
		conns = append(conns, conn)
		return true
	}
	wg := sync.WaitGroup{}
	serve := func(conn net.Conn) {
		defer wg.Done()
		tlsConn := tls.Server(conn, config)
		handshakeCtx, handshakeCancel := context.WithTimeout(context.Background(), testMeshTimeout)
		err := tlsConn.HandshakeContext(handshakeCtx)
		handshakeCancel()
		if err != nil {
			tlsConn.Close()
			return
		}
		front.handshakes <- tlsConn.ConnectionState().DidResume
		backendConn, err := net.Dial("tcp", backendAddress)
		if err != nil {
			tlsConn.Close()
			return
		}
		if !track(backendConn) {
			backendConn.Close()
			tlsConn.Close()
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			io.Copy(backendConn, tlsConn)
			backendConn.Close()
		}()
		io.Copy(tlsConn, backendConn)
		tlsConn.Close()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			if !track(conn) {
				conn.Close()
				return
			}
			wg.Add(1)
			go serve(conn)
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		func() {
			connsLock.Lock()
			defer connsLock.Unlock()
			for _, conn := range conns {
				conn.Close()
			}
			conns = nil
		}()
		wg.Wait()
	})
	return front
}

func (self *testTlsFront) port() int {
	return self.listener.Addr().(*net.TCPAddr).Port
}

func (self *testTlsFront) address() string {
	return self.listener.Addr().String()
}

// Whether the next handshake the front completes resumed.
func (self *testTlsFront) nextHandshakeResumed(t *testing.T) bool {
	t.Helper()
	select {
	case resumed := <-self.handshakes:
		return resumed
	case <-time.After(testMeshTimeout):
		t.Fatal("the front completed no tls handshake")
		return false
	}
}

// A certificate for `serverName` under a root of this test's own, and a pool
// that holds only that root.
func newTestTlsCertificate(t *testing.T, serverName string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rootTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "gossip test root"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	rootDer, err := x509.CreateCertificate(rand.Reader, rootTemplate, rootTemplate, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	root, err := x509.ParseCertificate(rootDer)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: serverName},
		DNSNames:     []string{serverName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDer, err := x509.CreateCertificate(rand.Reader, leafTemplate, root, &leafKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	rootCertPool := x509.NewCertPool()
	rootCertPool.AddCert(root)
	return tls.Certificate{
		Certificate: [][]byte{leafDer, rootDer},
		PrivateKey:  leafKey,
	}, rootCertPool
}

// One node with a long peering tick and a short coalesce, so only a directory
// change or a wake drives a round, as in the mesh fixture.
func newTestDialerNode(
	t *testing.T,
	ctx context.Context,
	name string,
	configure func(settings *NodeSettings),
) *Node {
	t.Helper()
	settings := DefaultNodeSettings(NodeRoleMember)
	settings.NetworkHost = testNetworkHost
	settings.Directory = newTestDirectory(t, newTestKey(t))
	settings.IdentityKeySeed = newTestKey(t).seed
	settings.PeerTimeout = 5 * time.Minute
	settings.PeerCoalesceTimeout = 10 * time.Millisecond
	settings.DialTimeout = 20 * time.Second
	settings.StatusTimeout = 50 * time.Millisecond
	// a fixed peer order per node, so a run replays the same way
	settings.Rand = mathrand.New(mathrand.NewPCG(uint64(len(name)), 0))
	if configure != nil {
		configure(settings)
	}
	node, err := NewNode(ctx, settings)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	t.Cleanup(node.Close)
	return node
}

// A node listening on plain websocket on loopback, the operator's shape behind
// its lb (C6), and the tcp address it bound.
func newTestWebsocketOperator(t *testing.T, ctx context.Context) (*Node, string) {
	t.Helper()
	listenAddrs, err := WebsocketListenAddrs("127.0.0.1", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	operator := newTestDialerNode(t, ctx, "operator", func(settings *NodeSettings) {
		settings.ListenAddrs = listenAddrs
	})
	boundAddrs := operator.ListenAddrs()
	if len(boundAddrs) == 0 {
		t.Fatal("the operator bound no address")
	}
	port, err := boundAddrs[0].ValueForProtocol(ma.P_TCP)
	if err != nil {
		t.Fatal(err)
	}
	return operator, net.JoinHostPort("127.0.0.1", port)
}

func newTestMultiaddr(t *testing.T, format string, args ...any) ma.Multiaddr {
	t.Helper()
	addr, err := ma.NewMultiaddr(fmt.Sprintf(format, args...))
	if err != nil {
		t.Fatal(err)
	}
	return addr
}

// Waits until the node holds its operator link after the dialer has made more
// than `previousDialCount` dials, and fails at once if the canary is reached
// instead.
func waitForDialerOperatorLink(
	t *testing.T,
	node *Node,
	dialer *testRecordingDialer,
	previousDialCount int,
	canary *testCanaryListener,
) {
	t.Helper()
	deadline := time.After(testMeshTimeout)
	for {
		status, change := node.StatusMonitor().Get()
		if status.OperatorConnected && previousDialCount < len(dialer.dialsCopy()) {
			return
		}
		select {
		case <-change:
		case <-canary.accepted:
			t.Fatal("the node reached its operator's address itself, outside its dialer")
		case <-deadline:
			t.Fatalf(
				"the operator link did not come up through the dialer, status = %+v, dials = %v",
				node.Status(),
				dialer.dialsCopy(),
			)
		}
	}
}

// With a dialer the operator link is the dialer's alone (connect#227). The node
// is told the operator is at a canary, by a name. The dialer is handed the name
// unresolved and takes the link to the real operator, the canary sees no
// connection, and every connection the operator accepts from the node came
// from a socket the dialer made. A redial goes the same way.
func TestGossipNodeDialerCarriesTheOperatorLink(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	operator, operatorAddress := newTestWebsocketOperator(t, ctx)
	// the remote port of every connection the operator accepts
	operatorConnPorts := make(chan string, 64)
	operator.host.Network().Notify(&network.NotifyBundle{
		ConnectedF: func(_ network.Network, conn network.Conn) {
			port, _ := conn.RemoteMultiaddr().ValueForProtocol(ma.P_TCP)
			select {
			case operatorConnPorts <- port:
			default:
			}
		},
	})

	canary := newTestCanaryListener(t)
	operatorName := net.JoinHostPort("localhost", strconv.Itoa(canary.port()))
	dialer := &testRecordingDialer{
		route: func(network string, address string) (string, error) {
			if address == operatorName {
				return operatorAddress, nil
			}
			// a proxy reaches whatever else it is asked to
			return address, nil
		},
	}
	member := newTestDialerNode(t, ctx, "member", func(settings *NodeSettings) {
		settings.OperatorAddrs = []ma.Multiaddr{
			newTestMultiaddr(t, "/dns/localhost/tcp/%d/ws/p2p/%s", canary.port(), operator.PeerId()),
		}
		settings.DialContext = dialer.DialContext
	})

	waitForDialerOperatorLink(t, member, dialer, 0, canary)
	if dials := dialer.dialsCopy(); dials[0] != "tcp "+operatorName {
		t.Fatalf("the dialer was handed %v, expected the operator's name first and unresolved, %q", dials, "tcp "+operatorName)
	}

	// a redial goes through the dialer too
	dialCount := len(dialer.dialsCopy())
	if err := member.host.Network().ClosePeer(operator.PeerId()); err != nil {
		t.Fatal(err)
	}
	waitForDialerOperatorLink(t, member, dialer, dialCount, canary)

	// the first link and the redial's, then any other the operator accepted
	checkPort := func(port string) {
		if !dialer.madeLocalPort(port) {
			t.Fatalf("the operator accepted a connection from port %s, which the dialer did not make", port)
		}
	}
	for range 2 {
		select {
		case port := <-operatorConnPorts:
			checkPort(port)
		case <-time.After(testMeshTimeout):
			t.Fatal("the operator did not accept the node's links")
		}
	}
	for drained := false; !drained; {
		select {
		case port := <-operatorConnPorts:
			checkPort(port)
		default:
			drained = true
		}
	}
	if count := canary.acceptedCount.Load(); count != 0 {
		t.Fatalf("the canary saw %d connections from outside the dialer", count)
	}
}

// With a dialer the extender carrier dials through it as well, and no longer
// through ConnectSettings' own proxy or dialer (connect#227). Here
// ConnectSettings would take the extender's address to a canary, and the
// node's dialer takes it to the extender.
func TestGossipNodeDialerCarriesTheExtenderCarrier(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	rootKey := newTestKey(t)
	extenderKey := newTestKey(t)
	tcpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tcpPort := tcpListener.Addr().(*net.TCPAddr).Port
	ip := netip.MustParseAddr("192.0.2.71")
	listenAddrs, err := ExtenderListenAddrs([]netip.Addr{ip}, tcpPort)
	if err != nil {
		t.Fatal(err)
	}
	listenerSettings := DefaultInProcessListenerSettings()
	listenerSettings.ListenAddrs = listenAddrs
	listener := NewInProcessListener(ctx, listenerSettings)
	t.Cleanup(listener.Close)
	newTestExtenderServer(t, ctx, extenderKey, tcpListener, tcpPort, func(settings *extender.ExtenderSettings) {
		settings.GossipConnHandler = listener.Handle
	})
	extenderNode := newTestDialerNode(t, ctx, "extender", func(settings *NodeSettings) {
		settings.Role = NodeRoleExtender
		settings.Directory = newTestDirectory(t, rootKey)
		settings.IdentityKeySeed = extenderKey.seed
		settings.ExtenderListener = listener
	})

	canary := newTestCanaryListener(t)
	connectSettings := connect.DefaultConnectSettings()
	connectSettings.DialContextSettings = &connect.DialContextSettings{
		DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			// ConnectSettings' own route, which a node with a dialer must not take
			return (&net.Dialer{}).DialContext(ctx, "tcp", canary.address())
		},
	}
	extenderAddress := net.JoinHostPort(ip.String(), strconv.Itoa(tcpPort))
	dialer := &testRecordingDialer{
		route: func(network string, address string) (string, error) {
			if address == extenderAddress {
				return tcpListener.Addr().String(), nil
			}
			return "", fmt.Errorf("no route to %s", address)
		},
	}
	memberDirectory := newTestDirectory(t, rootKey)
	record := signTestRecord(t, rootKey, extenderKey, ip.String(), tcpPort, time.Now())
	if _, err := memberDirectory.ApplyRecord(record, connect.ExtenderSourceGossip); err != nil {
		t.Fatal(err)
	}
	member := newTestDialerNode(t, ctx, "member", func(settings *NodeSettings) {
		settings.Directory = memberDirectory
		settings.ConnectSettings = connectSettings
		settings.DialContext = dialer.DialContext
	})

	deadline := time.After(testMeshTimeout)
	for {
		_, change := member.StatusMonitor().Get()
		if member.host.Network().Connectedness(extenderNode.PeerId()) == network.Connected {
			break
		}
		select {
		case <-change:
		case <-canary.accepted:
			t.Fatal("the extender carrier dialed through ConnectSettings, outside the node's dialer")
		case <-deadline:
			t.Fatalf("the member did not reach the extender, status = %+v, dials = %v", member.Status(), dialer.dialsCopy())
		}
	}
	if dials := dialer.dialsCopy(); !slices.Contains(dials, "tcp "+extenderAddress) {
		t.Fatalf("the dialer was handed %v, expected the extender's address %q", dials, "tcp "+extenderAddress)
	}
	if count := canary.acceptedCount.Load(); count != 0 {
		t.Fatalf("ConnectSettings' own route saw %d connections", count)
	}
}

// The operator link's tls resumes on a redial (connect#227). The first dial
// through a tls front, which is where the operator's lb terminates it, is a
// full handshake, and the second resumes from the session the first was
// issued, so it verifies no chain.
func TestGossipNodeDialerResumesTheOperatorTls(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	operator, operatorAddress := newTestWebsocketOperator(t, ctx)
	certificate, rootCertPool := newTestTlsCertificate(t, "gossip.space.example")
	front := newTestTlsFront(t, certificate, operatorAddress)
	frontName := net.JoinHostPort("gossip.space.example", strconv.Itoa(front.port()))
	dialer := &testRecordingDialer{
		route: func(network string, address string) (string, error) {
			if address == frontName {
				return front.address(), nil
			}
			// only the front's name reaches the operator, so every link this
			// test makes crosses the front
			return "", fmt.Errorf("no route to %s", address)
		},
	}
	connectSettings := connect.DefaultConnectSettings()
	connectSettings.TlsConfig = &tls.Config{
		RootCAs: rootCertPool,
	}
	member := newTestDialerNode(t, ctx, "member", func(settings *NodeSettings) {
		settings.OperatorAddrs = []ma.Multiaddr{
			newTestMultiaddr(t, "/dns/gossip.space.example/tcp/%d/wss/p2p/%s", front.port(), operator.PeerId()),
		}
		settings.ConnectSettings = connectSettings
		settings.DialContext = dialer.DialContext
		// a cache of this test's own, so no other test's sessions are offered
		settings.TlsSessionCache = newTlsSessionCache(defaultTlsSessionCacheSettings())
	})

	waitForNodeStatus(t, member, "the operator link", func(status NodeStatus) bool {
		return status.OperatorConnected
	})
	if resumed := front.nextHandshakeResumed(t); resumed {
		t.Fatal("the first dial resumed a session it was never issued")
	}

	if err := member.host.Network().ClosePeer(operator.PeerId()); err != nil {
		t.Fatal(err)
	}
	if resumed := front.nextHandshakeResumed(t); !resumed {
		t.Fatal("the second dial to the same operator did a full handshake, expected a resumption")
	}
	waitForNodeStatus(t, member, "the operator link again", func(status NodeStatus) bool {
		return status.OperatorConnected
	})
}

// A node with a dialer still listens on websocket, through libp2p's transport,
// and accepting a connection dials nothing.
func TestGossipNodeWithDialerStillListensOnWebsocket(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	dialer := &testRecordingDialer{}
	listenAddrs, err := WebsocketListenAddrs("127.0.0.1", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	listening := newTestDialerNode(t, ctx, "listening", func(settings *NodeSettings) {
		settings.ListenAddrs = listenAddrs
		settings.DialContext = dialer.DialContext
	})
	boundAddrs := listening.ListenAddrs()
	if len(boundAddrs) == 0 {
		t.Fatal("a node with a dialer bound no websocket address")
	}
	p2pComponent, err := ma.NewComponent("p2p", listening.PeerId().String())
	if err != nil {
		t.Fatal(err)
	}
	member := newTestDialerNode(t, ctx, "member", func(settings *NodeSettings) {
		settings.OperatorAddrs = []ma.Multiaddr{boundAddrs[0].Encapsulate(p2pComponent)}
	})
	waitForNodeStatus(t, member, "the link to the listening node", func(status NodeStatus) bool {
		return status.OperatorConnected
	})
	if dials := dialer.dialsCopy(); len(dials) != 0 {
		t.Fatalf("accepting a connection dialed %v", dials)
	}
}

// A node with no dialer is the node it always was: libp2p's websocket
// transport dials the operator, from this host, and the extender carrier dials
// through the very ConnectSettings it was given. Nothing of the dialer path is
// built, the process's session cache included.
func TestGossipNodeWithoutDialerIsUnchanged(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	canary := newTestCanaryListener(t)
	operatorPeerId, err := PeerIdForExtenderPublicKey(newTestKey(t).publicKey)
	if err != nil {
		t.Fatal(err)
	}
	connectSettings := connect.DefaultConnectSettings()
	settings := DefaultNodeSettings(NodeRoleMember)
	if settings.DialContext != nil || settings.TlsSessionCache != nil {
		t.Fatal("the default settings carry a dialer")
	}
	member := newTestDialerNode(t, ctx, "member", func(settings *NodeSettings) {
		settings.ConnectSettings = connectSettings
		settings.OperatorAddrs = []ma.Multiaddr{
			newTestMultiaddr(t, "/ip4/127.0.0.1/tcp/%d/ws/p2p/%s", canary.port(), operatorPeerId),
		}
	})
	if member.settings.DialContext != nil || member.settings.TlsSessionCache != nil {
		t.Fatal("a node with no dialer was given a dialer or a session cache")
	}

	swarmNetwork, ok := member.host.Network().(*swarm.Swarm)
	if !ok {
		t.Fatalf("the host's network is %T, expected the swarm", member.host.Network())
	}
	websocketAddr := newTestMultiaddr(t, "/dns/gossip.space.example/tcp/443/wss")
	if dialTransport := swarmNetwork.TransportForDialing(websocketAddr); dialTransport == nil {
		t.Fatal("no transport dials the operator")
	} else if _, ok := dialTransport.(*websocket.WebsocketTransport); !ok {
		t.Fatalf("the operator is dialed by %T, expected libp2p's websocket transport", dialTransport)
	}
	carrierAddr := newTestMultiaddr(t, "/ip4/192.0.2.1/tcp/443")
	carrier, ok := swarmNetwork.TransportForDialing(carrierAddr).(*extenderTransport)
	if !ok {
		t.Fatalf("an extender is dialed by %T, expected the extender transport", swarmNetwork.TransportForDialing(carrierAddr))
	}
	if carrier.settings.ConnectSettings != connectSettings {
		t.Fatal("the extender carrier dials through other ConnectSettings than the node was given")
	}

	// the operator is dialed from this host directly, as it always was
	select {
	case <-canary.accepted:
	case <-time.After(testMeshTimeout):
		t.Fatal("the node did not dial its operator")
	}
}
