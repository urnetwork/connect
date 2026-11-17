// The gossip node (EXTENDER.md D1, D3, D6).
//
// One node joins the mesh of one network space. It carries no discovery of its
// own -- no dht, no mdns, no relay, no nat service -- because the mesh is
// reached from two places only: the operator, whose address the network space
// resolves, and the extenders the directory already names. Everything the mesh
// carries is a root-signed record or revocation, so a node needs no membership
// of its own and a hostile peer can only observe the drip and add bounded load
// (D3).
//
// There is no full sync (D6). gossipsub delivers live messages only, so a
// joining node starts from the bounded feed sample and hears the operator's
// drip from then on.
//
// Three loops run for the life of a node and are joined by Close: the peering
// loop, which keeps the operator link and up to `PeerTarget` extender links
// alive; the receive loop, which applies every accepted message to the
// directory; and the topic event loop, which keeps the status current as the
// mesh grows and shrinks. Every message is verified twice -- once by the topic
// validator, which rejects a forgery before it is relayed, and once by the
// directory on apply -- because the directory is also written by the feed and
// must judge for itself.
//
// Whoever reaches the node can open connections and streams on it, and a mesh
// identity costs nothing to make: an open extender hands the gossip service of
// any client to its node, and the operator's listener is public. So what the
// node holds open is bounded where it is accepted, whoever asks. yamux resets
// a connection's inbound streams past `MaxInboundStreamCountPerConn` before
// the host spends anything on them, the resource manager bounds the
// connections, streams and window memory of one peer and of every peer
// together (`NodeResourceLimits`), and a stream that has not agreed on a
// protocol within `NegotiationTimeout` is reset (partner security report,
// 2026-11-16).
//
// Every connection the node makes goes through the space's dial: the operator
// link's tcp, tls and websocket upgrade (transport_websocket.go) through
// `ConnectSettings.DialContext`, which carries the space's proxy, its egress
// binding (R1) and its DialControl, and the extender carrier's dials through
// `ConnectSettings`. The operator's name goes to that dial unresolved. A node
// whose owner supplies a dialer of its own (`DialContext`) makes every
// connection through that one instead (connect#227).
//
// The node dials only what it chose: the operator addresses it was given and
// the addresses the directory's records name, each for its own peer. A dial
// to anything else -- an address a peer announced over identify, a peer a
// prune's peer exchange named -- is refused before any transport sees it, and
// gossipsub takes no peer exchange at all (`nodeGossipSubParams`).
//
// The node is safe for concurrent use.

package gossip

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"fmt"
	mathrand "math/rand/v2"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/sec"
	basichost "github.com/libp2p/go-libp2p/p2p/host/basic"
	"github.com/libp2p/go-libp2p/p2p/host/eventbus"
	"github.com/libp2p/go-libp2p/p2p/host/peerstore/pstoremem"
	rcmgr "github.com/libp2p/go-libp2p/p2p/host/resource-manager"
	"github.com/libp2p/go-libp2p/p2p/muxer/yamux"
	"github.com/libp2p/go-libp2p/p2p/net/connmgr"
	"github.com/libp2p/go-libp2p/p2p/net/swarm"
	"github.com/libp2p/go-libp2p/p2p/net/upgrader"
	"github.com/libp2p/go-libp2p/p2p/security/noise"
	websocket "github.com/libp2p/go-libp2p/p2p/transport/websocket"
	"github.com/libp2p/go-libp2p/x/rate"

	pubsub "github.com/libp2p/go-libp2p-pubsub"

	ma "github.com/multiformats/go-multiaddr"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// The roles a node runs in (D1, D5). They differ only in how much of the mesh
// they carry: an extender is reachable and holds twice the links of a member
// app, which is outbound only.
const (
	NodeRoleMember   = "member"
	NodeRoleExtender = "extender"
)

// Peers drawn from the directory per round, before the target trims them. A
// pool larger than the target is what makes the choice random rather than the
// directory's deterministic order.
const nodeCandidatePoolCount = 64

type NodeSettings struct {
	Log connect.Logger

	// The network space this mesh belongs to (B2, D1). It names the topic and
	// is what every accepted record must carry.
	NetworkHost string

	// The directory this node fills and validates against (E1). Its root key
	// set is the trust anchor: a message signed by a key it does not hold is
	// rejected before it is relayed.
	Directory *connect.ExtenderDirectory

	// The ed25519 seed of this node's identity (B1). On an extender it is the
	// extender's own key, so the mesh identity and the record identity are the
	// same; elsewhere it is a per-install key. Nil generates an ephemeral key,
	// which is what a node with no storage gets.
	IdentityKeySeed []byte

	// member or extender.
	Role string

	// Extender links this node keeps (D3): 8 for a member, 16 for an extender.
	PeerTarget int
	// Connection manager watermarks (D1): 8/16 for a member, 16/32 for an
	// extender. The manager trims above the high water; the peering loop never
	// drops a link itself.
	ConnManagerLowWater  int
	ConnManagerHighWater int

	// The most inbound streams one connection may hold open at once, which is
	// yamux's own bound. A connection carries one gossipsub stream each way and
	// identify's short-lived ones, so the default of 32 is a wide margin; a
	// stream past it is reset before the host spends a goroutine on it.
	// Non-positive takes the role default.
	MaxInboundStreamCountPerConn int
	// The largest receive window one stream may grow to, in bytes. The libp2p
	// default of 16 MiB is sized for bulk transfer; a gossip stream carries
	// signed records, and every byte of window is buffer a peer can fill. The
	// default is 1 MiB. yamux opens every stream at the 256 KiB its protocol
	// fixes, so a smaller value is taken as that. Non-positive takes the role
	// default.
	MaxStreamWindowByteCount int
	// How long a new stream has to agree on a protocol before it is reset.
	// Until it agrees it holds a goroutine and counts against the transient
	// limits, so this is how long a client that never proposes anything can
	// hold one. Non-positive takes the role default, 10 s, the libp2p default.
	NegotiationTimeout time.Duration
	// The resource manager's limits. Nil derives them from
	// `ConnManagerHighWater` when the node is built
	// (`DefaultNodeResourceLimits`), so a node sized as a hub -- the operator,
	// which every member dials -- gets a hub's limits without restating them.
	ResourceLimits *NodeResourceLimits

	// The operator's mesh addresses, each carrying `/p2p/<id>` (D3). Empty
	// means no operator link, which is what a space whose operator has not
	// published its identity yet has.
	OperatorAddrs []ma.Multiaddr
	// The websocket addresses this node listens on, which only the operator
	// has (C6). An extender listens through ExtenderListener instead, and a
	// member listens not at all.
	ListenAddrs []ma.Multiaddr
	// The extender's in-process gossip listener (D2), set on extenders only.
	// Its own advertised addresses are added to ListenAddrs.
	ExtenderListener *InProcessListener

	// The space's dial configuration. The operator link dials through its
	// DialContext -- the space's proxy, egress binding (R1) and DialControl --
	// and verifies the operator against its TlsConfig's roots; the extender
	// carrier dials through it as a whole. Nil takes
	// `connect.DefaultConnectSettings()`.
	ConnectSettings *connect.ConnectSettings
	// When set, every connection of this node goes through this dialer in
	// place of `ConnectSettings`' own: the operator link's tcp, tls and
	// websocket upgrade, and every extender carrier dial, which then ignores
	// ConnectSettings' proxy and dialer. The operator's name is handed to it
	// unresolved, for a socks proxy to resolve on its own side (connect#227).
	// Nil, the default, dials through `ConnectSettings.DialContext`.
	DialContext connect.DialContextFunction
	// The session cache of the operator link's tls. Nil takes the process's
	// shared cache, which hands out each session once (tls_session_cache.go).
	TlsSessionCache tls.ClientSessionCache

	// The peering round period (D3).
	PeerTimeout time.Duration
	// A burst of directory changes is collapsed into one round this long after
	// the first change.
	PeerCoalesceTimeout time.Duration
	// Budget of one peer dial.
	DialTimeout time.Duration
	// The status refresh period. The topic event loop keeps the status prompt;
	// this keeps it correct, because a peer can appear in the topic before this
	// node's own outgoing pubsub stream to it exists and no further topic event
	// is delivered when it does.
	StatusTimeout time.Duration

	// The only clock this node reads.
	Now func() time.Time
	// The generator of the peer choice, owned by this node's peering loop and
	// never shared with another node. Nil, the default, uses a
	// connect.NewSecureRand generator, so the choice does not follow from the
	// clock or the node's start time; a test injects a deterministic one.
	Rand *mathrand.Rand

	// Test seam only, nil otherwise: called on pubsub's event loop each time
	// the peer exchange of an incoming prune is dropped (`Node.inspectRpc`).
	peerExchangeStrippedHook func(from peer.ID, peerCount int)
}

// The defaults of one role (D1, D3).
func DefaultNodeSettings(role string) *NodeSettings {
	settings := &NodeSettings{
		Role:                         NodeRoleMember,
		PeerTarget:                   8,
		ConnManagerLowWater:          8,
		ConnManagerHighWater:         16,
		MaxInboundStreamCountPerConn: 32,
		MaxStreamWindowByteCount:     1024 * 1024,
		NegotiationTimeout:           10 * time.Second,
		PeerTimeout:                  60 * time.Second,
		PeerCoalesceTimeout:          1 * time.Second,
		DialTimeout:                  30 * time.Second,
		StatusTimeout:                5 * time.Second,
		Now:                          time.Now,
	}
	if role == NodeRoleExtender {
		settings.Role = NodeRoleExtender
		settings.PeerTarget = 16
		settings.ConnManagerLowWater = 16
		settings.ConnManagerHighWater = 32
	}
	return settings
}

// The resource manager's limits of one node. Every count bounds what is open
// at once. Memory is what yamux reserves to grow stream windows past the
// 256 KiB every stream opens with. A mesh identity costs nothing to make, so
// the peer limits bound one identity and the other limits bound every peer
// together; the transient ones are what a client that never finishes a
// handshake, or never proposes a protocol, can hold.
type NodeResourceLimits struct {
	// Connections of every peer together, both directions.
	ConnCount int
	// Inbound and outbound streams of every peer together.
	InboundStreamCount  int
	OutboundStreamCount int
	// Window memory every peer together may reserve, in bytes.
	MemoryByteCount int64

	// Connections still in their security or muxer handshake, every peer
	// together.
	TransientConnCount int
	// Streams that have not agreed on a protocol yet, every peer together.
	TransientStreamCount int

	// One peer's connections, inbound and outbound streams, and window memory
	// in bytes.
	PeerConnCount           int
	PeerInboundStreamCount  int
	PeerOutboundStreamCount int
	PeerMemoryByteCount     int64
}

// The limits of a node whose connection manager trims above
// `connManagerHighWater`. The all-peer limits scale with it, because the high
// water is what the node expects to hold: per expected peer, 4 connections, 8
// streams each way and 4 MiB of window growth, and 2 connections in handshake
// and 4 streams in negotiation, which covers a peering round that dials every
// address of every peer at once. A member (16) and an extender (32) get small
// limits, the operator (1024) a hub's; a high water under 16 is taken as 16.
// One peer holds a link or two in practice, so its limits do not scale.
func DefaultNodeResourceLimits(connManagerHighWater int) *NodeResourceLimits {
	highWater := max(connManagerHighWater, 16)
	return &NodeResourceLimits{
		ConnCount:               4 * highWater,
		InboundStreamCount:      8 * highWater,
		OutboundStreamCount:     8 * highWater,
		MemoryByteCount:         int64(highWater) * 4 * 1024 * 1024,
		TransientConnCount:      2 * highWater,
		TransientStreamCount:    4 * highWater,
		PeerConnCount:           8,
		PeerInboundStreamCount:  64,
		PeerOutboundStreamCount: 64,
		PeerMemoryByteCount:     32 * 1024 * 1024,
	}
}

// The resource manager of these limits. What the limits leave unnamed --
// services, protocols, single connections and streams -- is bounded by the
// peer and all-peer limits above it.
//
// The per-address limits libp2p applies by default, connection count and
// connection rate alike, are turned off. They key on the address a connection
// arrives from, and here that is not a client's: every connection to the
// operator arrives from its reverse proxy, and an extender's in-process
// connection on a udp carrier carries the extender's own address
// (`remoteMultiaddrOrDefault`), so a per-address bound would be a bound on the
// whole mesh. An extender's admission limits (A12) already bound connections
// per source subnet before a stream reaches the node.
func newNodeResourceManager(limits *NodeResourceLimits) (network.ResourceManager, error) {
	limitConfig := rcmgr.PartialLimitConfig{
		System: rcmgr.ResourceLimits{
			Conns:           rcmgr.LimitVal(limits.ConnCount),
			Streams:         rcmgr.LimitVal(limits.InboundStreamCount + limits.OutboundStreamCount),
			StreamsInbound:  rcmgr.LimitVal(limits.InboundStreamCount),
			StreamsOutbound: rcmgr.LimitVal(limits.OutboundStreamCount),
			Memory:          rcmgr.LimitVal64(limits.MemoryByteCount),
		},
		Transient: rcmgr.ResourceLimits{
			Conns:   rcmgr.LimitVal(limits.TransientConnCount),
			Streams: rcmgr.LimitVal(limits.TransientStreamCount),
			Memory:  rcmgr.LimitVal64(limits.MemoryByteCount),
		},
		PeerDefault: rcmgr.ResourceLimits{
			Conns:           rcmgr.LimitVal(limits.PeerConnCount),
			Streams:         rcmgr.LimitVal(limits.PeerInboundStreamCount + limits.PeerOutboundStreamCount),
			StreamsInbound:  rcmgr.LimitVal(limits.PeerInboundStreamCount),
			StreamsOutbound: rcmgr.LimitVal(limits.PeerOutboundStreamCount),
			Memory:          rcmgr.LimitVal64(limits.PeerMemoryByteCount),
		},
	}.Build(rcmgr.InfiniteLimits)
	return rcmgr.NewResourceManager(
		rcmgr.NewFixedLimiter(limitConfig),
		rcmgr.WithLimitPerSubnet([]rcmgr.ConnLimitPerSubnet{}, []rcmgr.ConnLimitPerSubnet{}),
		rcmgr.WithConnRateLimiters(&rate.Limiter{}),
		// the limits are fixed, and the node exports no libp2p metrics
		rcmgr.WithMetricsDisabled(),
	)
}

// The muxer of D1 with this node's stream bounds. The libp2p default lifts
// yamux's own inbound stream limit to its maximum and leaves the bound to a
// resource manager, so without one a single connection could hold any number
// of streams. Here yamux enforces the per-connection bound itself, and the
// window stops at what a gossip stream needs.
func newNodeMuxer(settings *NodeSettings) *yamux.Transport {
	config := *yamux.DefaultTransport.Config()
	config.MaxIncomingStreams = uint32(settings.MaxInboundStreamCountPerConn)
	config.MaxStreamWindowSize = uint32(max(
		settings.MaxStreamWindowByteCount,
		int(config.InitialStreamWindowSize),
	))
	return (*yamux.Transport)(&config)
}

// What the sdk status reports for the mesh (F2). Comparable, so it rides a
// MonitorValue and a consumer is woken only on an actual change.
type NodeStatus struct {
	// Peers this node holds a connection to, of every kind.
	PeerCount int
	// Peers in the topic mesh, which is what carries messages.
	MeshPeerCount int
	// True while a configured operator address is connected.
	OperatorConnected bool
	// True while a peering round has dials in flight and no mesh peer is held
	// yet, which is the app's yellow connecting state for the member role
	// (K4). A node that already has a mesh peer is connected, not connecting,
	// whatever else it is dialing.
	Connecting bool
}

type Node struct {
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	log       connect.Logger

	settings  *NodeSettings
	publicKey ed25519.PublicKey

	host         host.Host
	pubsub       *pubsub.PubSub
	topic        *pubsub.Topic
	topicEvents  *pubsub.TopicEventHandler
	subscription *pubsub.Subscription

	statusMonitor *connect.MonitorValue[NodeStatus]
	// closed and replaced when something should re-run the peering round
	// early: a new operator address, a peer that went away
	wakeMonitor *connect.Monitor

	peeringDone     chan struct{}
	receiveDone     chan struct{}
	topicEventsDone chan struct{}
	statusDone      chan struct{}

	// the peer choice, read only by the peering loop
	rand *mathrand.Rand

	// dials the current peering round has in flight (K4). Atomic rather than
	// state, because the round publishes it through `updateStatus`, which
	// takes the status lock.
	dialCount atomic.Int64

	stateLock     sync.Mutex
	operatorAddrs []ma.Multiaddr
	// the addresses the current peering round chose, per peer: the only ones
	// the swarm may dial (node_gater.go)
	peerIdDialAddrs map[peer.ID][]ma.Multiaddr

	// statusLock serializes the read of the host and the topic with the
	// publish of the status. Both the peering round and the topic event loop
	// update the status, and without this an older read could land last and
	// leave a stale status standing with nothing left to re-notify.
	statusLock sync.Mutex
}

// The node is running when this returns: the host is up, the topic is joined
// and subscribed, and the peering loop has started.
func NewNode(ctx context.Context, settings *NodeSettings) (*Node, error) {
	if settings == nil {
		return nil, fmt.Errorf("the gossip node needs settings")
	}
	if settings.Directory == nil {
		return nil, fmt.Errorf("the gossip node needs a directory")
	}
	if settings.NetworkHost == "" {
		return nil, fmt.Errorf("the gossip node needs a network host")
	}
	copied := *settings
	if copied.Now == nil {
		copied.Now = time.Now
	}
	if copied.PeerTimeout <= 0 {
		copied.PeerTimeout = 60 * time.Second
	}
	if copied.DialTimeout <= 0 {
		copied.DialTimeout = 30 * time.Second
	}
	if copied.StatusTimeout <= 0 {
		copied.StatusTimeout = 5 * time.Second
	}
	if copied.ConnectSettings == nil {
		copied.ConnectSettings = connect.DefaultConnectSettings()
	}
	// the inbound bounds are never off: a caller that built its settings by
	// hand gets the role's
	roleDefaults := DefaultNodeSettings(copied.Role)
	if copied.MaxInboundStreamCountPerConn <= 0 {
		copied.MaxInboundStreamCountPerConn = roleDefaults.MaxInboundStreamCountPerConn
	}
	if copied.MaxStreamWindowByteCount <= 0 {
		copied.MaxStreamWindowByteCount = roleDefaults.MaxStreamWindowByteCount
	}
	if copied.NegotiationTimeout <= 0 {
		copied.NegotiationTimeout = roleDefaults.NegotiationTimeout
	}
	if copied.ResourceLimits == nil {
		copied.ResourceLimits = DefaultNodeResourceLimits(copied.ConnManagerHighWater)
	} else {
		resourceLimits := *copied.ResourceLimits
		copied.ResourceLimits = &resourceLimits
	}
	if copied.TlsSessionCache == nil {
		copied.TlsSessionCache = processTlsSessionCache()
	}
	settings = &copied

	identityKeySeed := settings.IdentityKeySeed
	if len(identityKeySeed) == 0 {
		// an ephemeral identity: the node is still a full member, it simply
		// does not keep the same peer id across restarts
		generatedSeed, err := connect.NewExtenderKeySeed()
		if err != nil {
			return nil, err
		}
		identityKeySeed = generatedSeed
	}
	privateKey, err := privateKeyForSeed(identityKeySeed)
	if err != nil {
		return nil, err
	}
	publicKey, err := connect.ExtenderPublicKeyFromSeed(identityKeySeed)
	if err != nil {
		return nil, err
	}

	random := settings.Rand
	if random == nil {
		random = connect.NewSecureRand()
	}

	cancelCtx, cancel := context.WithCancel(ctx)
	self := &Node{
		ctx:             cancelCtx,
		cancel:          cancel,
		log:             loggerOrDefault(settings.Log),
		settings:        settings,
		publicKey:       publicKey,
		statusMonitor:   connect.NewMonitorValue[NodeStatus](NodeStatus{}),
		wakeMonitor:     connect.NewMonitor(),
		peeringDone:     make(chan struct{}),
		receiveDone:     make(chan struct{}),
		topicEventsDone: make(chan struct{}),
		statusDone:      make(chan struct{}),
		rand:            random,
		operatorAddrs:   slices.Clone(settings.OperatorAddrs),
	}

	// the host closes the resource manager it is built on, so the manager is
	// closed here only when no host took it
	var resourceManager network.ResourceManager
	success := false
	defer func() {
		if !success {
			cancel()
			if self.host != nil {
				self.host.Close()
			} else if resourceManager != nil {
				resourceManager.Close()
			}
		}
	}()

	if resourceManager, err = newNodeResourceManager(settings.ResourceLimits); err != nil {
		return nil, err
	}
	if self.host, err = self.newHost(privateKey, resourceManager); err != nil {
		return nil, err
	}
	if err := self.joinTopic(); err != nil {
		return nil, err
	}

	// a peer that goes away should be replaced without waiting out the round
	self.host.Network().Notify(&network.NotifyBundle{
		DisconnectedF: func(network.Network, network.Conn) {
			self.wakeMonitor.NotifyAll()
		},
	})

	go connect.HandleError(func() {
		defer close(self.peeringDone)
		self.runPeering()
	}, cancel)
	go connect.HandleError(func() {
		defer close(self.receiveDone)
		self.runReceive()
	}, cancel)
	go connect.HandleError(func() {
		defer close(self.topicEventsDone)
		self.runTopicEvents()
	}, cancel)
	go connect.HandleError(func() {
		defer close(self.statusDone)
		self.runStatus()
	}, cancel)

	success = true
	return self, nil
}

// The libp2p host of D1: the extender transport in place of tcp, the websocket
// transport for the operator (transport_websocket.go dials through the space's
// dial and libp2p's only listens), noise, yamux under this node's stream
// bounds, the node's resource manager, a connection manager, and no
// discovery, relay, nat service or metrics of any kind. The resource manager
// is the swarm's and the upgrader's both: the upgrader opens the scope of
// every inbound connection, so without it inbound connections would go
// uncounted and their yamux window growth unbounded. Closing the host closes
// the resource manager.
//
// The host is assembled from the swarm and the basic host rather than through
// the `libp2p` root package, which is a deliberate deviation: that package's
// default transport set drags in webtransport and webrtc, and the webtransport
// transport of go-libp2p v0.49 does not compile against the quic-go this module
// pins. Naming the two transports we want also keeps quic, webtransport and
// webrtc out of the binary entirely, which is what the mobile size budget
// needs.
func (self *Node) newHost(
	privateKey crypto.PrivKey,
	resourceManager network.ResourceManager,
) (host.Host, error) {
	peerId, err := peer.IDFromPrivateKey(privateKey)
	if err != nil {
		return nil, err
	}
	peerstore, err := pstoremem.NewPeerstore()
	if err != nil {
		return nil, err
	}
	if err := peerstore.AddPrivKey(peerId, privateKey); err != nil {
		return nil, err
	}
	if err := peerstore.AddPubKey(peerId, privateKey.GetPublic()); err != nil {
		return nil, err
	}

	connManager, err := connmgr.NewConnManager(
		self.settings.ConnManagerLowWater,
		self.settings.ConnManagerHighWater,
	)
	if err != nil {
		return nil, err
	}

	eventBus := eventbus.NewBus()
	swarmNetwork, err := swarm.NewSwarm(
		peerId,
		peerstore,
		eventBus,
		swarm.WithDialTimeout(self.settings.DialTimeout),
		swarm.WithResourceManager(resourceManager),
		// only what the peering round chose is dialed (node_gater.go)
		swarm.WithConnectionGater(&nodeDialGater{node: self}),
	)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			swarmNetwork.Close()
		}
	}()

	noiseTransport, err := noise.New(noise.ID, privateKey, nil)
	if err != nil {
		return nil, err
	}
	connUpgrader, err := upgrader.New(
		[]sec.SecureTransport{noiseTransport},
		[]upgrader.StreamMuxer{{ID: yamux.ID, Muxer: newNodeMuxer(self.settings)}},
		nil,
		resourceManager,
		nil,
	)
	if err != nil {
		return nil, err
	}

	extenderConnectSettings := self.settings.ConnectSettings
	if self.settings.DialContext != nil {
		// ConnectSettings.DialContext takes a DialContextSettings dial ahead of
		// its proxy and its own dialer, so every carrier dial goes through the
		// node's dialer
		copied := *self.settings.ConnectSettings
		copied.DialContextSettings = &connect.DialContextSettings{
			DialContext: self.settings.DialContext,
		}
		extenderConnectSettings = &copied
	}
	extenderTransport, err := newExtenderTransport(&extenderTransportSettings{
		Log:             self.settings.Log,
		Directory:       self.settings.Directory,
		ConnectSettings: extenderConnectSettings,
		Listener:        self.settings.ExtenderListener,
		Upgrader:        connUpgrader,
		ResourceManager: swarmNetwork.ResourceManager(),
	})
	if err != nil {
		return nil, err
	}
	if err := swarmNetwork.AddTransport(extenderTransport); err != nil {
		return nil, err
	}
	// libp2p's websocket transport only listens: it dials with net.DialTCP
	// from this host, outside the space's proxy, egress binding and
	// DialControl, so every dial goes through the space's dial instead
	websocketTransport, err := websocket.New(connUpgrader, swarmNetwork.ResourceManager(), nil)
	if err != nil {
		return nil, err
	}
	websocketDialContext := self.settings.DialContext
	if websocketDialContext == nil {
		websocketDialContext = self.settings.ConnectSettings.DialContext
	}
	dialTransport, err := newWebsocketDialTransport(&websocketDialTransportSettings{
		DialContext:     websocketDialContext,
		TlsConfig:       self.settings.ConnectSettings.TlsConfig,
		TlsSessionCache: self.settings.TlsSessionCache,
		Upgrader:        connUpgrader,
		ResourceManager: swarmNetwork.ResourceManager(),
		ListenTransport: websocketTransport,
	})
	if err != nil {
		return nil, err
	}
	if err := swarmNetwork.AddTransport(dialTransport); err != nil {
		return nil, err
	}

	listenAddrs := slices.Clone(self.settings.ListenAddrs)
	if self.settings.ExtenderListener != nil {
		listenAddrs = append(listenAddrs, self.settings.ExtenderListener.ListenAddrs()...)
	}
	if 0 < len(listenAddrs) {
		if err := swarmNetwork.Listen(listenAddrs...); err != nil {
			return nil, err
		}
	}

	basicHost, err := basichost.NewHost(swarmNetwork, &basichost.HostOpts{
		EventBus:    eventBus,
		ConnManager: connManager,
		// an extender advertises the public addresses of its own record, and a
		// member advertises nothing, so nothing here discovers or publishes an
		// address of its own
		EnablePing:         false,
		NegotiationTimeout: self.settings.NegotiationTimeout,
	})
	if err != nil {
		return nil, err
	}
	basicHost.Start()
	success = true
	return basicHost, nil
}

// Joins the space's topic with strict signing and the root-signature validator
// (D1). The validator runs before a message is relayed, so a forgery goes no
// further than the node that received it. Peer scoring is left at the library
// default, which is off, so a rejection costs its sender nothing beyond the
// relay it did not get; the root signature on every message is what bounds a
// hostile member, not a score.
func (self *Node) joinTopic() error {
	pubSub, err := pubsub.NewGossipSub(
		self.ctx,
		self.host,
		// the defaults, taking no peer exchange (node_gater.go)
		pubsub.WithGossipSubParams(nodeGossipSubParams()),
		// and every prune's peer exchange is dropped before gossipsub reads it
		pubsub.WithAppSpecificRpcInspector(self.inspectRpc),
		pubsub.WithMessageSignaturePolicy(pubsub.StrictSign),
		// A message this node originates goes to every topic peer it holds,
		// not only to its grafted mesh. Only the operator originates (D6), and
		// its drip must reach a node that has just joined rather than wait for
		// the next heartbeat to graft it; a relayed message still follows the
		// mesh, so this costs nothing anywhere else.
		pubsub.WithFloodPublish(true),
	)
	if err != nil {
		return err
	}
	self.pubsub = pubSub

	topicName := Topic(self.settings.NetworkHost)
	if err := pubSub.RegisterTopicValidator(topicName, self.validate); err != nil {
		return err
	}
	if self.topic, err = pubSub.Join(topicName); err != nil {
		return err
	}
	if self.topicEvents, err = self.topic.EventHandler(); err != nil {
		return err
	}
	if self.subscription, err = self.topic.Subscribe(); err != nil {
		return err
	}
	return nil
}

// The topic validator (D1). A message that is not a signed record or
// revocation of this network space is rejected outright: there is nothing else
// this topic carries, and accepting it would relay it.
func (self *Node) validate(ctx context.Context, from peer.ID, message *pubsub.Message) pubsub.ValidationResult {
	gossipMessage := &protocol.ExtenderGossipMessage{}
	if err := proto.Unmarshal(message.GetData(), gossipMessage); err != nil {
		return pubsub.ValidationReject
	}
	keySet := self.settings.Directory.RootKeys()
	var networkHost string
	switch {
	case gossipMessage.GetRecord() != nil:
		body, err := keySet.VerifyRecord(gossipMessage.GetRecord())
		if err != nil {
			return pubsub.ValidationReject
		}
		if !connect.ExtenderRecordOpen(body) {
			// a gated record has no business on the mesh (Q1): the operator
			// never publishes one, so it is a leak, and relaying it would
			// make the leak the whole mesh's. Ignored rather than rejected,
			// since the peer that relayed it may hold it from a leak too
			return pubsub.ValidationIgnore
		}
		networkHost = body.NetworkHost
	case gossipMessage.GetRevocation() != nil:
		body, err := keySet.VerifyRevocation(gossipMessage.GetRevocation())
		if err != nil {
			return pubsub.ValidationReject
		}
		networkHost = body.NetworkHost
	default:
		return pubsub.ValidationReject
	}
	if !connect.ExtenderNetworkHostAllowed(networkHost, self.settings.NetworkHost) {
		return pubsub.ValidationReject
	}
	return pubsub.ValidationAccept
}

// Publish sends one signed message to the topic. Only the operator publishes
// (C6, D6); every other node relays what it hears.
func (self *Node) Publish(ctx context.Context, message *protocol.ExtenderGossipMessage) error {
	if message == nil {
		return fmt.Errorf("extender gossip message is missing")
	}
	if record := message.GetRecord(); record != nil {
		body, err := self.settings.Directory.RootKeys().VerifyRecord(record)
		if err != nil {
			return err
		}
		if !connect.ExtenderRecordOpen(body) {
			return fmt.Errorf("extender record is restricted from gossip")
		}
	}
	messageBytes, err := proto.Marshal(message)
	if err != nil {
		return err
	}
	return self.topic.Publish(ctx, messageBytes)
}

// The mesh identity of this node, which on an extender is the identity in its
// own signed record.
func (self *Node) PeerId() peer.ID {
	return self.host.ID()
}

// The addresses this node listens on, empty for a member app. These are what
// the swarm actually bound, not what the host has got around to advertising,
// so the operator can publish its address the instant the node is up.
func (self *Node) ListenAddrs() []ma.Multiaddr {
	return self.host.Network().ListenAddresses()
}

// The role this node runs in (D1, D5).
func (self *Node) Role() string {
	return self.settings.Role
}

// Listen adds addresses this node is reachable at, on top of whatever it was
// constructed with. An extender learns its public addresses from its
// activation result, which arrives after the node is already up (D2, G3), so
// the mesh address of a family is published when that family activates.
// Addresses already listened on are skipped, and listening on none is not an
// error.
func (self *Node) Listen(listenAddrs ...ma.Multiaddr) error {
	existingAddrs := self.host.Network().ListenAddresses()
	newAddrs := []ma.Multiaddr{}
	for _, listenAddr := range listenAddrs {
		if slices.ContainsFunc(existingAddrs, listenAddr.Equal) {
			continue
		}
		if slices.ContainsFunc(newAddrs, listenAddr.Equal) {
			continue
		}
		newAddrs = append(newAddrs, listenAddr)
	}
	if len(newAddrs) == 0 {
		return nil
	}
	if err := self.host.Network().Listen(newAddrs...); err != nil {
		return err
	}
	// a node that has just become reachable should peer now rather than at the
	// end of the round
	self.wakeMonitor.NotifyAll()
	return nil
}

func (self *Node) Status() NodeStatus {
	return self.statusMonitor.Value()
}

// The status value and a channel armed at the same instant, for a consumer
// that renders it.
func (self *Node) StatusMonitor() *connect.MonitorValue[NodeStatus] {
	return self.statusMonitor
}

// SetOperatorAddrs replaces the operator addresses and runs a peering round at
// once. The sdk calls it when hello first carries the operator's identity,
// which is after the node is already up (D3, C7).
func (self *Node) SetOperatorAddrs(operatorAddrs []ma.Multiaddr) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.operatorAddrs = slices.Clone(operatorAddrs)
	}()
	self.wakeMonitor.NotifyAll()
}

func (self *Node) OperatorAddrs() []ma.Multiaddr {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.operatorAddrs)
}

// Ends the loops, joins them, and closes the host, which closes its resource
// manager.
func (self *Node) Close() {
	self.closeOnce.Do(func() {
		self.cancel()
		<-self.peeringDone
		self.subscription.Cancel()
		<-self.receiveDone
		self.topicEvents.Cancel()
		<-self.topicEventsDone
		<-self.statusDone
		self.topic.Close()
		self.host.Close()
	})
}

// The peering round (D3): one pass at start, then on every directory change
// and every `PeerTimeout`. Nothing is ever dropped here; the connection
// manager trims above its high water.
func (self *Node) runPeering() {
	for {
		// subscribe to both wakes immediately before the round reads the
		// directory and the host, so a change during the round is carried into
		// the next wait rather than lost
		_, directoryChange := self.settings.Directory.ChangeMonitor().Get()
		wake := self.wakeMonitor.NotifyChannel()

		self.peer()
		self.updateStatus()

		coalesce := false
		select {
		case <-self.ctx.Done():
			return
		case <-directoryChange:
			coalesce = true
		case <-wake:
			coalesce = true
		case <-time.After(self.settings.PeerTimeout):
		}
		if coalesce && 0 < self.settings.PeerCoalesceTimeout {
			// a burst of applied records is one round, not one round each
			select {
			case <-self.ctx.Done():
				return
			case <-time.After(self.settings.PeerCoalesceTimeout):
			}
		}
	}
}

// Connects to every operator address and to enough extenders to reach the
// target. The dials run together and are joined here, so a round never
// outlives the loop that started it.
func (self *Node) peer() {
	addrInfos := []peer.AddrInfo{}
	for _, operatorAddr := range self.OperatorAddrs() {
		addrInfo, err := peer.AddrInfoFromP2pAddr(operatorAddr)
		if err != nil {
			self.log.Infof("[gossip]operator address %s err = %s\n", operatorAddr, err)
			continue
		}
		if self.host.Network().Connectedness(addrInfo.ID) != network.Connected {
			addrInfos = append(addrInfos, *addrInfo)
		}
	}

	// the extender target is counted on its own: the operator link is not one
	// of the `PeerTarget` extenders (D3)
	extenderCount := 0
	pendingAddrInfos := []peer.AddrInfo{}
	for _, addrInfo := range self.extenderPeers() {
		if self.host.Network().Connectedness(addrInfo.ID) == network.Connected {
			extenderCount += 1
			continue
		}
		pendingAddrInfos = append(pendingAddrInfos, addrInfo)
	}
	for _, addrInfo := range pendingAddrInfos {
		if self.settings.PeerTarget <= extenderCount {
			break
		}
		addrInfos = append(addrInfos, addrInfo)
		extenderCount += 1
	}

	// what this round chose is all the swarm may dial until the next round
	// (node_gater.go); the previous round's dials were joined before this one
	// began
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		peerIdDialAddrs := map[peer.ID][]ma.Multiaddr{}
		for _, addrInfo := range addrInfos {
			peerIdDialAddrs[addrInfo.ID] = append(peerIdDialAddrs[addrInfo.ID], addrInfo.Addrs...)
		}
		self.peerIdDialAddrs = peerIdDialAddrs
	}()

	if 0 < len(addrInfos) {
		// the round's dials are in flight from here until they are joined,
		// which is what the connecting state reports (K4)
		self.dialCount.Add(int64(len(addrInfos)))
		self.updateStatus()
		defer func() {
			self.dialCount.Add(-int64(len(addrInfos)))
			self.updateStatus()
		}()
	}

	wg := sync.WaitGroup{}
	for _, addrInfo := range addrInfos {
		wg.Add(1)
		go connect.HandleError(func() {
			defer wg.Done()
			self.connectPeer(addrInfo)
		})
	}
	wg.Wait()
}

func (self *Node) connectPeer(addrInfo peer.AddrInfo) {
	ctx, cancel := context.WithTimeout(self.ctx, self.settings.DialTimeout)
	defer cancel()
	if err := self.host.Connect(ctx, addrInfo); err != nil {
		if self.log.V(1).Enabled() {
			self.log.Infof("[gossip]connect %s err = %s\n", addrInfo.ID, err)
		}
	}
}

// The extenders this node may peer with, in random order: verified, active,
// unheld, reachable over the tcp carrier, and not this node itself.
func (self *Node) extenderPeers() []peer.AddrInfo {
	candidates := self.settings.Directory.Candidates(0, nodeCandidatePoolCount)
	addrInfos := []peer.AddrInfo{}
	for _, candidate := range candidates {
		if !candidate.Verified || len(candidate.PublicKey) == 0 {
			continue
		}
		// a retained expired identity is a client's last resort, not a
		// current member of the mesh
		if candidate.Expired {
			continue
		}
		if bytes.Equal(candidate.PublicKey, self.publicKey) {
			continue
		}
		// the mesh rides the tcp carrier only (D2)
		if !slices.Contains(candidate.Carriers, connect.ExtenderCarrierTcp) {
			continue
		}
		if candidate.TcpPort <= 0 {
			continue
		}
		peerId, err := PeerIdForExtenderPublicKey(candidate.PublicKey)
		if err != nil {
			continue
		}
		addrs, err := ExtenderListenAddrs([]netip.Addr{candidate.Ip}, candidate.TcpPort)
		if err != nil {
			continue
		}
		addrInfos = append(addrInfos, peer.AddrInfo{
			ID:    peerId,
			Addrs: addrs,
		})
	}
	self.rand.Shuffle(len(addrInfos), func(i int, j int) {
		addrInfos[i], addrInfos[j] = addrInfos[j], addrInfos[i]
	})
	return addrInfos
}

// Applies every accepted message to the directory (D1). The validator has
// already verified it; the directory verifies it again, because it is also
// written by the feed and judges for itself.
func (self *Node) runReceive() {
	for {
		message, err := self.subscription.Next(self.ctx)
		if err != nil {
			return
		}
		gossipMessage := &protocol.ExtenderGossipMessage{}
		if err := proto.Unmarshal(message.GetData(), gossipMessage); err != nil {
			continue
		}
		if _, err := self.settings.Directory.ApplySource(
			gossipMessage,
			connect.ExtenderSourceGossip,
		); err != nil {
			self.log.Infof("[gossip]apply err = %s\n", err)
		}
	}
}

// Keeps the status current as the topic mesh changes, so a consumer of the
// status monitor is woken by a peer joining rather than by the next round.
func (self *Node) runTopicEvents() {
	for {
		if _, err := self.topicEvents.NextPeerEvent(self.ctx); err != nil {
			return
		}
		self.updateStatus()
	}
}

// Keeps the status correct. A topic peer can appear before this node's own
// outgoing pubsub stream to it exists, and pubsub delivers no second event when
// that stream opens, so the topic event loop alone can leave a status that is
// permanently behind.
func (self *Node) runStatus() {
	for {
		self.updateStatus()
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(self.settings.StatusTimeout):
		}
	}
}

func (self *Node) updateStatus() {
	self.statusLock.Lock()
	defer self.statusLock.Unlock()

	status := NodeStatus{
		PeerCount:     len(self.host.Network().Peers()),
		MeshPeerCount: len(self.topic.ListPeers()),
	}
	status.Connecting = status.MeshPeerCount == 0 && 0 < self.dialCount.Load()
	for _, operatorAddr := range self.OperatorAddrs() {
		addrInfo, err := peer.AddrInfoFromP2pAddr(operatorAddr)
		if err != nil {
			continue
		}
		if self.host.Network().Connectedness(addrInfo.ID) == network.Connected {
			status.OperatorConnected = true
			break
		}
	}
	self.statusMonitor.Set(status)
}
