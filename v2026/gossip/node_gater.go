// What a node may dial (EXTENDER.md D3, connect#227).
//
// A node dials only what its peering round chose: the operator addresses it was
// given and the addresses the directory's records name, each for its own peer
// (`Node.peer`). Two things would otherwise have it dial what a peer chose:
//   - a peer announces addresses of its own over identify, and the swarm dials
//     every address it holds for a peer, so a redial of an extender or of the
//     operator would also try whatever that peer named;
//   - a gossipsub prune may carry other peers' signed records for the pruned
//     node to connect to (v1.1 peer exchange), which gossipsub dials on its own.
//
// Either way a mesh peer could make a node -- an app, or a miner slot through its
// proxy -- dial an address of the peer's choosing, which reveals where the node
// is and spends its egress on someone else's target. Three layers keep it to
// its own choice:
//   - every incoming rpc has the peer exchange of its prunes taken out before
//     gossipsub reads it (`Node.inspectRpc`); no node here sends any, so only a
//     peer that is not one of ours does;
//   - gossipsub reads no peer exchange record even if one got through
//     (`nodeGossipSubParams`);
//   - the swarm's gater refuses every outbound dial to a peer or an address the
//     current round did not choose, before any transport sees it.
//
// Inbound connections are not dials, and are bounded where they are accepted
// (node.go).
//
// The gater and the inspector are safe for concurrent use.

package gossip

import (
	"slices"

	"github.com/libp2p/go-libp2p/core/connmgr"
	"github.com/libp2p/go-libp2p/core/control"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"

	pubsub "github.com/libp2p/go-libp2p-pubsub"

	ma "github.com/multiformats/go-multiaddr"
)

// The gossipsub parameters of every node: the library's defaults, taking no
// peer exchange. With peer scoring off every score is 0, which is not below
// the default threshold of 0, so the router connected to every peer a prune
// named. PrunePeers is the most records a prune is read for, so 0 reads none.
// It also bounds the records this node would send, which are none already,
// since no node turns peer exchange on. The mesh parameters are the defaults.
func nodeGossipSubParams() pubsub.GossipSubParams {
	params := pubsub.DefaultGossipSubParams()
	params.PrunePeers = 0
	return params
}

// The swarm's gater for one node: an outbound dial to a peer, or to an address
// of a peer, that the node's current peering round did not choose is refused.
type nodeDialGater struct {
	node *Node
}

var _ connmgr.ConnectionGater = (*nodeDialGater)(nil)

func (self *nodeDialGater) InterceptPeerDial(peerId peer.ID) bool {
	return self.node.dialChosen(peerId, nil)
}

func (self *nodeDialGater) InterceptAddrDial(peerId peer.ID, addr ma.Multiaddr) bool {
	return self.node.dialChosen(peerId, addr)
}

func (self *nodeDialGater) InterceptAccept(network.ConnMultiaddrs) bool {
	return true
}

func (self *nodeDialGater) InterceptSecured(network.Direction, peer.ID, network.ConnMultiaddrs) bool {
	return true
}

func (self *nodeDialGater) InterceptUpgraded(network.Conn) (bool, control.DisconnectReason) {
	return true, 0
}

// Whether the current peering round chose `peerId`, and, with `addr` set, that
// address of it.
func (self *Node) dialChosen(peerId peer.ID, addr ma.Multiaddr) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	addrs, ok := self.peerIdDialAddrs[peerId]
	if !ok {
		return false
	}
	if addr == nil {
		return true
	}
	return slices.ContainsFunc(addrs, addr.Equal)
}

// The pubsub rpc inspector of this node: takes the peer exchange out of every
// prune of an incoming rpc before gossipsub reads it, and keeps the rest. A
// prune still ends the mesh link and sets its backoff; only the records of
// other peers to connect to are dropped. It runs on pubsub's event loop, which
// owns the rpc, so the change is seen by nothing else.
func (self *Node) inspectRpc(from peer.ID, rpc *pubsub.RPC) error {
	strippedCount := 0
	for _, prune := range rpc.GetControl().GetPrune() {
		strippedCount += len(prune.GetPeers())
		prune.Peers = nil
	}
	if 0 < strippedCount {
		if self.log.V(1).Enabled() {
			self.log.Infof("[gossip]peer exchange of %d peers from %s dropped\n", strippedCount, from)
		}
		if hook := self.settings.peerExchangeStrippedHook; hook != nil {
			hook(from, strippedCount)
		}
	}
	return nil
}
