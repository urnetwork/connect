// The inbound bounds of the gossip node (EXTENDER.md D1, D2; the partner
// security report of 2026-11-16).
//
// An open extender hands the gossip service of any client to its node, and a
// mesh identity costs nothing to make, so whatever one connection or one peer
// can hold open on the node is what an anonymous client can hold. These tests
// drive the node through its real entry point -- the gossip service of an open
// extender server on loopback -- and judge every stream they open: the node
// either takes it into protocol negotiation, answering `na` to a protocol it
// does not serve and waiting for the next proposal, or refuses it with a
// reset. Each stream is judged before the next opens, so the counts are exact
// and nothing waits on time.
//
// Everything here is synthetic: `.example` names, RFC 5737 documentation
// addresses, and keys generated in the test.

package gossip

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/transport"

	ma "github.com/multiformats/go-multiaddr"
	multistream "github.com/multiformats/go-multistream"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/extender"
)

// Long enough that no held stream is reset by its negotiation timeout while a
// test still counts, so a count never depends on how fast the test runs.
const testHeldNegotiationTimeout = 10 * time.Minute

// A protocol no gossip node serves. Proposing it is how a stream is held in
// negotiation: the node answers `na` and waits for another proposal.
const testUnservedProtocol = "/urnetwork/test/unserved/1.0.0"

// One open extender running a node in the extender role, and the way a client
// reaches its gossip service.
type testGossipTarget struct {
	t   *testing.T
	ctx context.Context

	node     *Node
	listener *InProcessListener

	peerId          peer.ID
	raddr           ma.Multiaddr
	clientDirectory *connect.ExtenderDirectory
	connectSettings *connect.ConnectSettings
}

// Stands up an open extender server on loopback with its in-process gossip
// listener and a node in the extender role behind it. `configure` adjusts the
// node settings before the node is built.
func newTestGossipTarget(t *testing.T, configure func(settings *NodeSettings)) *testGossipTarget {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	rootKey := newTestKey(t)
	extenderKey := newTestKey(t)

	tcpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tcpPort := tcpListener.Addr().(*net.TCPAddr).Port
	ip := netip.MustParseAddr("192.0.2.62")
	connectSettings := newTestLoopbackConnectSettings(t, ip, tcpListener.Addr().String())

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
		// every client of these tests dials from loopback, and the extender's
		// own admission limit is not what is under test
		settings.AdmissionUnlimitedSources = []netip.Prefix{
			netip.MustParsePrefix("127.0.0.0/8"),
		}
	})

	settings := DefaultNodeSettings(NodeRoleExtender)
	settings.NetworkHost = testNetworkHost
	settings.Directory = newTestDirectory(t, rootKey)
	settings.IdentityKeySeed = extenderKey.seed
	settings.ExtenderListener = listener
	settings.PeerTimeout = 5 * time.Minute
	settings.StatusTimeout = 5 * time.Minute
	if configure != nil {
		configure(settings)
	}
	node, err := NewNode(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(node.Close)

	clientDirectory := newTestDirectory(t, rootKey)
	record := signTestRecord(t, rootKey, extenderKey, ip.String(), tcpPort, time.Now())
	if _, err := clientDirectory.ApplyRecord(record, connect.ExtenderSourceGossip); err != nil {
		t.Fatal(err)
	}
	peerId, err := PeerIdForExtenderPublicKey(extenderKey.publicKey)
	if err != nil {
		t.Fatal(err)
	}
	raddr, err := ma.NewMultiaddr(fmt.Sprintf("/ip4/%s/tcp/%d", ip, tcpPort))
	if err != nil {
		t.Fatal(err)
	}
	return &testGossipTarget{
		t:               t,
		ctx:             ctx,
		node:            node,
		listener:        listener,
		peerId:          peerId,
		raddr:           raddr,
		clientDirectory: clientDirectory,
		connectSettings: connectSettings,
	}
}

// Opens one connection to the node's gossip service as `clientKey`, the way
// any client of an open extender can: the extender dial, then noise and yamux
// with no host behind them.
func (self *testGossipTarget) dial(clientKey *testKey) transport.CapableConn {
	self.t.Helper()
	clientTransport, err := newExtenderTransport(&extenderTransportSettings{
		Directory:       self.clientDirectory,
		ConnectSettings: self.connectSettings,
		Upgrader:        newTestUpgrader(self.t, clientKey),
	})
	if err != nil {
		self.t.Fatal(err)
	}
	dialCtx, dialCancel := context.WithTimeout(self.ctx, testMeshTimeout)
	defer dialCancel()
	capableConn, err := clientTransport.Dial(dialCtx, self.raddr, self.peerId)
	if err != nil {
		self.t.Fatal(err)
	}
	self.t.Cleanup(func() {
		capableConn.Close()
	})
	return capableConn
}

// Opens one stream on `capableConn` and proposes a protocol the node does not
// serve. True when the node took the stream into negotiation and now holds it
// waiting for another proposal, false when it refused the stream. The stream is
// left open either way: holding it is the point.
func openHeldStream(t *testing.T, ctx context.Context, capableConn transport.CapableConn) bool {
	t.Helper()
	stream, err := capableConn.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = multistream.SelectProtoOrFail(testUnservedProtocol, stream)
	if err == nil {
		t.Fatalf("the node served %s", testUnservedProtocol)
	}
	var notSupported multistream.ErrNotSupported[string]
	return errors.As(err, &notSupported)
}

// Opens `streamCount` streams on one connection, one after another, and counts
// the ones the node holds.
func openHeldStreams(
	t *testing.T,
	ctx context.Context,
	capableConn transport.CapableConn,
	streamCount int,
) int {
	t.Helper()
	heldCount := 0
	for range streamCount {
		if openHeldStream(t, ctx, capableConn) {
			heldCount += 1
		}
	}
	return heldCount
}

// The finding itself: one client opened streams over a single connection to an
// open extender's gossip service and the node took every one, each costing it
// a goroutine and the stream's buffers for as long as the client kept it. The
// node must hold no more of them than its per-connection bound, refuse the
// rest with a reset, and still take the ones within the bound.
func TestGossipNodeBoundsInboundStreamsOnOneConnection(t *testing.T) {
	target := newTestGossipTarget(t, func(settings *NodeSettings) {
		settings.NegotiationTimeout = testHeldNegotiationTimeout
	})
	// the bound the extender role is built with
	maxInboundStreamCountPerConn := DefaultNodeSettings(NodeRoleExtender).MaxInboundStreamCountPerConn

	capableConn := target.dial(newTestKey(t))
	streamCount := 8 * maxInboundStreamCountPerConn
	heldCount := openHeldStreams(t, target.ctx, capableConn, streamCount)

	if heldCount == 0 {
		t.Fatal("the node held no stream at all")
	}
	if maxInboundStreamCountPerConn < heldCount {
		t.Fatalf(
			"the node held %d of %d streams on one connection, over its bound of %d",
			heldCount,
			streamCount,
			maxInboundStreamCountPerConn,
		)
	}
}

// A mesh identity is free, so a client that is refused on one connection opens
// another as someone else. Streams that never agree on a protocol are bounded
// across every peer together: a second identity gets only what the first left
// of the transient bound, however far under its own per-connection bound it is.
func TestGossipNodeBoundsNegotiatingStreamsOfEveryPeer(t *testing.T) {
	maxInboundStreamCountPerConn := DefaultNodeSettings(NodeRoleExtender).MaxInboundStreamCountPerConn
	// room for one connection's worth of held streams and a few more, so the
	// second identity reaches the transient bound well inside its own
	transientStreamCount := maxInboundStreamCountPerConn + maxInboundStreamCountPerConn/4
	target := newTestGossipTarget(t, func(settings *NodeSettings) {
		settings.NegotiationTimeout = testHeldNegotiationTimeout
		resourceLimits := DefaultNodeResourceLimits(settings.ConnManagerHighWater)
		resourceLimits.TransientStreamCount = transientStreamCount
		settings.ResourceLimits = resourceLimits
	})

	firstHeldCount := openHeldStreams(t, target.ctx, target.dial(newTestKey(t)), 2*maxInboundStreamCountPerConn)
	secondHeldCount := openHeldStreams(t, target.ctx, target.dial(newTestKey(t)), 2*maxInboundStreamCountPerConn)

	if firstHeldCount == 0 {
		t.Fatal("the node held no stream of the first identity")
	}
	if transientStreamCount < firstHeldCount+secondHeldCount {
		t.Fatalf(
			"two identities held %d and %d negotiating streams, over the transient bound of %d",
			firstHeldCount,
			secondHeldCount,
			transientStreamCount,
		)
	}
	if maxInboundStreamCountPerConn <= secondHeldCount {
		t.Fatalf(
			"the second identity held %d streams, its whole per-connection bound, past a full transient bound",
			secondHeldCount,
		)
	}
}

// The per-address limits libp2p applies by default would count every
// connection that arrives from one address as one client's: 8 at once. Every
// connection to the operator arrives from its reverse proxy, and an extender's
// connections on a udp carrier carry the extender's own address, so under
// those limits the whole mesh would be 8 connections. A node must take more
// identities than that from one address, each with a stream.
func TestGossipNodeTakesManyIdentitiesFromOneAddress(t *testing.T) {
	target := newTestGossipTarget(t, func(settings *NodeSettings) {
		settings.NegotiationTimeout = testHeldNegotiationTimeout
	})
	// past libp2p's default of 8 connections from one address, within the
	// node's own per-peer and transient bounds
	identityCount := 12
	proxyAddr := &net.TCPAddr{
		IP:   net.ParseIP("198.51.100.7"),
		Port: 40000,
	}
	for i := range identityCount {
		capableConn := target.dialThroughListener(newTestKey(t), proxyAddr)
		if !openHeldStream(t, target.ctx, capableConn) {
			t.Fatalf("identity %d of %d from one address was refused its stream", i+1, identityCount)
		}
	}
}

// A node sized as a hub gets a hub's limits from its high water alone: the
// operator raises the member defaults to 512/1024 and states no limits, and
// must not be held to a member's.
func TestGossipNodeDerivesItsLimitsFromItsHighWater(t *testing.T) {
	node := newTestLocalNode(t, func(settings *NodeSettings) {
		settings.ConnManagerLowWater = 512
		settings.ConnManagerHighWater = 1024
	})
	limits := node.settings.ResourceLimits
	expectedLimits := DefaultNodeResourceLimits(1024)
	if *limits != *expectedLimits {
		t.Fatalf("the node's limits = %+v, expected %+v", *limits, *expectedLimits)
	}
	// the operator replicas hold a few hundred peers each, and the connection
	// manager trims them at 1024; the limits must leave room past both
	if limits.ConnCount < 4*1024 || limits.InboundStreamCount < 8*1024 {
		t.Fatalf("a hub's limits are too small: %+v", *limits)
	}

	memberLimits := DefaultNodeResourceLimits(DefaultNodeSettings(NodeRoleMember).ConnManagerHighWater)
	if limits.ConnCount <= memberLimits.ConnCount {
		t.Fatalf("a hub got a member's connection limit of %d", limits.ConnCount)
	}
}

// Hands one connection straight to the node's in-process listener as if the
// extender had taken it over from a client at `remoteAddr`, and upgrades the
// client end as `clientKey`.
func (self *testGossipTarget) dialThroughListener(clientKey *testKey, remoteAddr net.Addr) transport.CapableConn {
	self.t.Helper()
	clientConn, serverConn := net.Pipe()
	self.t.Cleanup(func() {
		clientConn.Close()
		serverConn.Close()
	})
	go self.listener.Handle(&testRemoteAddrConn{
		Conn:       serverConn,
		remoteAddr: remoteAddr,
	})
	upgradeCtx, upgradeCancel := context.WithTimeout(self.ctx, testMeshTimeout)
	defer upgradeCancel()
	capableConn, err := newTestUpgrader(self.t, clientKey).Upgrade(
		upgradeCtx,
		newTestTransport(self.t, self.clientDirectory, nil),
		newMaConn(clientConn, self.raddr, self.raddr),
		network.DirOutbound,
		self.peerId,
		&network.NullScope{},
	)
	if err != nil {
		self.t.Fatal(err)
	}
	self.t.Cleanup(func() {
		capableConn.Close()
	})
	return capableConn
}

// A connection that reports a remote address of the test's choosing, as the
// extender's taken-over streams report their client's.
type testRemoteAddrConn struct {
	net.Conn
	remoteAddr net.Addr
}

func (self *testRemoteAddrConn) RemoteAddr() net.Addr {
	return self.remoteAddr
}

// A partial override preserves the finite role defaults at every other scope.
func TestGossipPartialResourceLimitsKeepRoleBounds(t *testing.T) {
	custom := &NodeResourceLimits{PeerConnCount: 2, PeerMemoryByteCount: -1}
	node := newTestLocalNode(t, func(settings *NodeSettings) {
		settings.ResourceLimits = custom
	})
	want := DefaultNodeResourceLimits(node.settings.ConnManagerHighWater)
	want.PeerConnCount = 2
	if got := *node.settings.ResourceLimits; got != *want {
		t.Errorf("partial limits = %+v, want %+v", got, *want)
	}
	if custom.ConnCount != 0 || custom.PeerMemoryByteCount != -1 {
		t.Error("caller-owned settings mutated")
	}
}

// Membership is attached to the authenticated peer, so sharing an address
// never grants another identity permission. Outbound round selection is separate.
func TestGossipRestrictedInboundPeerSelection(t *testing.T) {
	allowed, other := peer.ID("member"), peer.ID("other")
	peers := []peer.ID{allowed}
	node := newTestLocalNode(t, func(settings *NodeSettings) { settings.AllowedInboundPeers = peers })
	peers[0] = other
	gate := &nodeDialGater{node: node}
	if !gate.InterceptSecured(network.DirInbound, allowed, nil) || gate.InterceptSecured(network.DirInbound, other, nil) {
		t.Error("inbound membership does not enforce its private snapshot")
	}
	if !gate.InterceptSecured(network.DirOutbound, other, nil) {
		t.Error("inbound membership constrained the outbound round")
	}
	closed := newTestLocalNode(t, func(settings *NodeSettings) { settings.AllowedInboundPeers = []peer.ID{} })
	if (&nodeDialGater{node: closed}).InterceptSecured(network.DirInbound, allowed, nil) {
		t.Error("empty membership admitted a peer")
	}
}

// Connection-local reservations, peer streams and their parents release
// independently. Exercise the installed libp2p scopes without any sockets.
func TestGossipConnectionAndPeerQuotaRecovery(t *testing.T) {
	limits := DefaultNodeResourceLimits(16)
	limits.PeerInboundStreamCount = 1
	const peerMemoryLimit = 1024 * 1024
	limits.PeerMemoryByteCount = peerMemoryLimit
	manager, err := newNodeResourceManager(limits)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	addr := ma.StringCast("/ip4/192.0.2.10/tcp/4001")
	conn, err := manager.OpenConnection(network.DirInbound, false, addr)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.ReserveMemory(1+peerMemoryLimit, network.ReservationPriorityAlways); err == nil {
		t.Error("single connection borrowed past its memory limit")
		conn.ReleaseMemory(1 + peerMemoryLimit)
	}
	conn.Done()
	one, err := manager.OpenStream(peer.ID("first"), network.DirInbound)
	if err != nil {
		t.Fatal(err)
	}
	if extra, err := manager.OpenStream(peer.ID("first"), network.DirInbound); err == nil {
		extra.Done()
		t.Error("peer exceeded its stream quota")
	}
	other, err := manager.OpenStream(peer.ID("second"), network.DirInbound)
	if err != nil {
		t.Fatal("healthy sibling", err)
	}
	one.Done()
	again, err := manager.OpenStream(peer.ID("first"), network.DirInbound)
	if err != nil {
		t.Fatal("released peer quota unavailable", err)
	}
	again.Done()
	other.Done()
	if err := manager.ViewSystem(func(scope network.ResourceScope) error {
		stat := scope.Stat()
		if stat.NumStreamsInbound != 0 || stat.Memory != 0 {
			t.Errorf("unreleased resources %+v", stat)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Keep a negotiated stream active on a second authenticated connection while
// the first identity fills its inbound quota and receives a refusal.
func TestGossipPeerQuotaPreservesSiblingTraffic(t *testing.T) {
	target := newTestGossipTarget(t, func(settings *NodeSettings) {
		limits := DefaultNodeResourceLimits(settings.ConnManagerHighWater)
		limits.PeerInboundStreamCount = 1
		settings.ResourceLimits = limits
		settings.NegotiationTimeout = testHeldNegotiationTimeout
	})
	const echo = "/urnetwork/test/echo/1.0.0"
	target.node.host.SetStreamHandler(echo, func(stream network.Stream) {
		defer stream.Close()
		var buffer [16]byte
		for {
			if _, err := io.ReadFull(stream, buffer[:]); err != nil {
				return
			}
			if _, err := stream.Write(buffer[:]); err != nil {
				return
			}
		}
	})
	sibling, err := target.dial(newTestKey(t)).OpenStream(target.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer sibling.Close()
	if err := sibling.SetDeadline(time.Now().Add(testMeshTimeout)); err != nil {
		t.Fatal(err)
	}
	if err := multistream.SelectProtoOrFail(echo, sibling); err != nil {
		t.Fatal(err)
	}
	exchange := func(value byte) {
		t.Helper()
		sent := bytes.Repeat([]byte{value}, 16)
		if _, err := sibling.Write(sent); err != nil {
			t.Fatal(err)
		}
		received := make([]byte, len(sent))
		if _, err := io.ReadFull(sibling, received); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(sent, received) {
			t.Fatalf("sibling returned %x, want %x", received, sent)
		}
	}
	exchange(1)
	other := target.dial(newTestKey(t))
	if !openHeldStream(t, target.ctx, other) || openHeldStream(t, target.ctx, other) {
		t.Fatal("the first stream should fit and the second should be refused")
	}
	exchange(2)
}
