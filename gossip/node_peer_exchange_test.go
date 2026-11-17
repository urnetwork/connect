// A node dials only what its peering round chose (node_gater.go, connect#227).
//
// A mesh peer must not make a node dial an address of the peer's choosing:
// neither by naming other peers in a prune (gossipsub peer exchange) nor by
// announcing addresses for itself. Each test puts a canary where such a dial
// would land, and a dial outside the round's choice fails the test by name.

package gossip

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/record"
	basichost "github.com/libp2p/go-libp2p/p2p/host/basic"
	"github.com/libp2p/go-libp2p/p2p/host/eventbus"
	"github.com/libp2p/go-libp2p/p2p/host/peerstore/pstoremem"
	"github.com/libp2p/go-libp2p/p2p/net/swarm"
	websocket "github.com/libp2p/go-libp2p/p2p/transport/websocket"

	pubsub "github.com/libp2p/go-libp2p-pubsub"
	pubsubpb "github.com/libp2p/go-libp2p-pubsub/pb"

	ma "github.com/multiformats/go-multiaddr"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
)

// A bare libp2p host on loopback websocket that speaks gossipsub by hand: it
// takes the node's pubsub stream and drains it, and writes whatever rpcs a test
// hands it. It is what a hostile mesh peer is to the node.
type testRawGossipPeer struct {
	host       host.Host
	listenAddr ma.Multiaddr
}

func newTestRawGossipPeer(t *testing.T) *testRawGossipPeer {
	t.Helper()
	key := newTestKey(t)
	privateKey, err := crypto.UnmarshalEd25519PrivateKey(key.privateKey)
	if err != nil {
		t.Fatal(err)
	}
	peerId, err := peer.IDFromPrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	peerstore, err := pstoremem.NewPeerstore()
	if err != nil {
		t.Fatal(err)
	}
	if err := peerstore.AddPrivKey(peerId, privateKey); err != nil {
		t.Fatal(err)
	}
	if err := peerstore.AddPubKey(peerId, privateKey.GetPublic()); err != nil {
		t.Fatal(err)
	}
	eventBus := eventbus.NewBus()
	swarmNetwork, err := swarm.NewSwarm(peerId, peerstore, eventBus)
	if err != nil {
		t.Fatal(err)
	}
	websocketTransport, err := websocket.New(newTestUpgrader(t, key), swarmNetwork.ResourceManager(), nil)
	if err != nil {
		swarmNetwork.Close()
		t.Fatal(err)
	}
	if err := swarmNetwork.AddTransport(websocketTransport); err != nil {
		swarmNetwork.Close()
		t.Fatal(err)
	}
	listenAddrs, err := WebsocketListenAddrs("127.0.0.1", 0, false)
	if err != nil {
		swarmNetwork.Close()
		t.Fatal(err)
	}
	if err := swarmNetwork.Listen(listenAddrs...); err != nil {
		swarmNetwork.Close()
		t.Fatal(err)
	}
	basicHost, err := basichost.NewHost(swarmNetwork, &basichost.HostOpts{
		EventBus: eventBus,
	})
	if err != nil {
		swarmNetwork.Close()
		t.Fatal(err)
	}
	basicHost.Start()
	t.Cleanup(func() {
		basicHost.Close()
	})
	for _, protocolId := range pubsub.GossipSubDefaultProtocols {
		basicHost.SetStreamHandler(protocolId, func(stream network.Stream) {
			io.Copy(io.Discard, stream)
			stream.Close()
		})
	}
	boundAddrs := swarmNetwork.ListenAddresses()
	if len(boundAddrs) == 0 {
		t.Fatal("the raw gossip peer bound no address")
	}
	return &testRawGossipPeer{
		host:       basicHost,
		listenAddr: boundAddrs[0],
	}
}

// Its dialable address, with its identity.
func (self *testRawGossipPeer) p2pAddr(t *testing.T) ma.Multiaddr {
	t.Helper()
	p2pComponent, err := ma.NewComponent("p2p", self.host.ID().String())
	if err != nil {
		t.Fatal(err)
	}
	return self.listenAddr.Encapsulate(p2pComponent)
}

// Writes rpcs to the node on one gossipsub stream, in order, each framed as
// gossipsub frames them: a uvarint length and the protobuf.
func (self *testRawGossipPeer) writeRpcs(t *testing.T, ctx context.Context, peerId peer.ID, rpcs ...*pubsubpb.RPC) {
	t.Helper()
	stream, err := self.host.NewStream(ctx, peerId, pubsub.GossipSubID_v11)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stream.Reset()
	})
	for _, rpc := range rpcs {
		rpcBytes, err := proto.Marshal(rpc)
		if err != nil {
			t.Fatal(err)
		}
		frame := binary.AppendUvarint(nil, uint64(len(rpcBytes)))
		frame = append(frame, rpcBytes...)
		if _, err := stream.Write(frame); err != nil {
			t.Fatal(err)
		}
	}
}

// A peer exchange entry: the signed record of a fresh peer that names `addrs`.
func newTestPeerExchangeInfo(t *testing.T, addrs ...ma.Multiaddr) (peer.ID, *pubsubpb.PeerInfo) {
	t.Helper()
	privateKey, err := crypto.UnmarshalEd25519PrivateKey(newTestKey(t).privateKey)
	if err != nil {
		t.Fatal(err)
	}
	peerId, err := peer.IDFromPrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := record.Seal(peer.PeerRecordFromAddrInfo(peer.AddrInfo{ID: peerId, Addrs: addrs}), privateKey)
	if err != nil {
		t.Fatal(err)
	}
	envelopeBytes, err := envelope.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return peerId, &pubsubpb.PeerInfo{
		PeerID:           []byte(peerId),
		SignedPeerRecord: envelopeBytes,
	}
}

// The parameters every node's gossipsub runs with read no peer exchange, and
// are the library's defaults in everything else, so the mesh behaves as it
// always has.
func TestGossipNodeGossipSubParamsTakeNoPeerExchange(t *testing.T) {
	params := nodeGossipSubParams()
	if params.PrunePeers != 0 {
		t.Fatalf("a prune is read for %d peer exchange records, expected none", params.PrunePeers)
	}
	defaults := pubsub.DefaultGossipSubParams()
	params.PrunePeers = defaults.PrunePeers
	if !reflect.DeepEqual(params, defaults) {
		t.Fatalf("the mesh parameters differ from gossipsub's defaults:\n%+v\n%+v", params, defaults)
	}
}

// A mesh peer's prune names another peer at a canary (gossipsub v1.1 peer
// exchange). The node drops the exchange before gossipsub reads the prune:
// nothing dials the canary, and the node's peerstore learns no address of the
// named peer. The drop is observed where it happens, on pubsub's event loop
// ahead of the router, so the test needs no quiet period: with the exchange
// read, as before, gossipsub's connector dials the canary instead.
func TestGossipNodeTakesNoPeerExchange(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	rawPeer := newTestRawGossipPeer(t)
	stripped := make(chan int, 16)
	member := newTestDialerNode(t, ctx, "member", func(settings *NodeSettings) {
		settings.OperatorAddrs = []ma.Multiaddr{rawPeer.p2pAddr(t)}
		settings.peerExchangeStrippedHook = func(from peer.ID, peerCount int) {
			if from == rawPeer.host.ID() {
				stripped <- peerCount
			}
		}
	})
	waitForNodeStatus(t, member, "the link to the raw peer", func(status NodeStatus) bool {
		return status.OperatorConnected
	})

	canary := newTestCanaryListener(t)
	exchangedPeerId, peerInfo := newTestPeerExchangeInfo(
		t,
		newTestMultiaddr(t, "/ip4/127.0.0.1/tcp/%d/ws", canary.port()),
		newTestMultiaddr(t, "/dns/localhost/tcp/%d/ws", canary.port()),
	)
	topic := Topic(testNetworkHost)
	backoff := uint64(60)
	rawPeer.writeRpcs(
		t,
		ctx,
		member.PeerId(),
		&pubsubpb.RPC{
			Control: &pubsubpb.ControlMessage{
				Prune: []*pubsubpb.ControlPrune{{
					TopicID: &topic,
					Peers:   []*pubsubpb.PeerInfo{peerInfo},
					Backoff: &backoff,
				}},
			},
		},
	)

	select {
	case peerCount := <-stripped:
		if peerCount != 1 {
			t.Fatalf("the node dropped the peer exchange of %d peers, the prune named 1", peerCount)
		}
	case <-canary.accepted:
		t.Fatal("the node dialed a peer a prune named")
	case <-time.After(testMeshTimeout):
		t.Fatal("the prune never reached the node")
	}
	if addrs := member.host.Peerstore().Addrs(exchangedPeerId); len(addrs) != 0 {
		t.Fatalf("the node took the addresses %v from a prune's peer exchange", addrs)
	}
	if count := canary.acceptedCount.Load(); count != 0 {
		t.Fatalf("the canary a prune named saw %d connections", count)
	}
}

// The node dials only what its round chose. Exactly what gossipsub's peer
// exchange connector does with a named peer -- connect to it at the addresses
// its record names -- is refused before any transport sees it, and so is an
// address a chosen peer announced for itself, while the operator address the
// node was given stays dialable.
func TestGossipNodeDialsOnlyWhatItsRoundChose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	operator, operatorAddress := newTestWebsocketOperator(t, ctx)
	_, operatorPort, err := net.SplitHostPort(operatorAddress)
	if err != nil {
		t.Fatal(err)
	}
	dialedLock := sync.Mutex{}
	dialedAddresses := []string{}
	connectSettings := connect.DefaultConnectSettings()
	connectSettings.DialNetworkHook = func(network string, address string) {
		dialedLock.Lock()
		defer dialedLock.Unlock()
		dialedAddresses = append(dialedAddresses, address)
	}
	operatorAddr := newTestMultiaddr(t, "/ip4/127.0.0.1/tcp/%s/ws", operatorPort)
	member := newTestDialerNode(t, ctx, "member", func(settings *NodeSettings) {
		settings.ConnectSettings = connectSettings
		settings.OperatorAddrs = []ma.Multiaddr{
			newTestMultiaddr(t, "/ip4/127.0.0.1/tcp/%s/ws/p2p/%s", operatorPort, operator.PeerId()),
		}
	})
	waitForNodeStatus(t, member, "the operator link", func(status NodeStatus) bool {
		return status.OperatorConnected
	})

	canary := newTestCanaryListener(t)
	canaryAddr := newTestMultiaddr(t, "/ip4/127.0.0.1/tcp/%d/ws", canary.port())
	strangerId, err := PeerIdForExtenderPublicKey(newTestKey(t).publicKey)
	if err != nil {
		t.Fatal(err)
	}
	connectCtx, connectCancel := context.WithTimeout(ctx, testMeshTimeout)
	err = member.host.Connect(connectCtx, peer.AddrInfo{ID: strangerId, Addrs: []ma.Multiaddr{canaryAddr}})
	connectCancel()
	if err == nil {
		t.Fatal("the node connected to a peer its round never chose")
	}
	if !errors.Is(err, swarm.ErrGaterDisallowedConnection) {
		t.Fatalf("a dial to a peer the round never chose failed with %v, expected the node's refusal", err)
	}

	swarmNetwork, ok := member.host.Network().(*swarm.Swarm)
	if !ok {
		t.Fatalf("the host's network is %T, expected the swarm", member.host.Network())
	}
	if swarmNetwork.CanDial(operator.PeerId(), canaryAddr) {
		t.Fatal("an address the operator could announce for itself is dialable")
	}
	if !swarmNetwork.CanDial(operator.PeerId(), operatorAddr) {
		t.Fatal("the operator address the node was given is not dialable")
	}

	if count := canary.acceptedCount.Load(); count != 0 {
		t.Fatalf("the canary saw %d connections", count)
	}
	canaryAddress := net.JoinHostPort("127.0.0.1", strconv.Itoa(canary.port()))
	dialedLock.Lock()
	defer dialedLock.Unlock()
	if slices.Contains(dialedAddresses, canaryAddress) {
		t.Fatalf("the canary reached the space's dial: %v", dialedAddresses)
	}
}
