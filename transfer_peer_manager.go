package connect

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/urnetwork/connect/protocol"
)

func DefaultPeerManagerSettings() *PeerManagerSettings {
	return &PeerManagerSettings{
		// matches the platform's recently disconnected peer window
		DisconnectedPeerWindow: 5 * time.Minute,
	}
}

type PeerManagerSettings struct {
	// how long a disconnect marker counts toward the disconnected peer count
	DisconnectedPeerWindow time.Duration
}

// PeerManager tracks the network peers of this client:
// the connected top-level clients in the same network and their identity metadata.
// The platform announces the complete list on each connect
// (`NetworkPeersReset` followed by `NetworkPeersUpdate` messages)
// and diffs after. Disconnected peers arrive as disconnect markers
// and are aged out locally by the disconnected peer window.
type PeerManager struct {
	ctx context.Context

	client *Client

	peerManagerSettings *PeerManagerSettings

	peersMonitor *Monitor

	stateLock       sync.Mutex
	connectedPeers  map[Id]*NetworkPeer
	disconnectTimes map[Id]time.Time
}

func NewPeerManager(ctx context.Context, client *Client, peerManagerSettings *PeerManagerSettings) *PeerManager {
	return &PeerManager{
		ctx:                 ctx,
		client:              client,
		peerManagerSettings: peerManagerSettings,
		peersMonitor:        NewMonitor(),
		connectedPeers:      map[Id]*NetworkPeer{},
		disconnectTimes:     map[Id]time.Time{},
	}
}

func (self *PeerManager) Client() *Client {
	return self.client
}

// PeersMonitor notifies on any change to the peer state
func (self *PeerManager) PeersMonitor() *Monitor {
	return self.peersMonitor
}

// isConnectedNetworkPeer reports whether clientId is currently announced as a
// top-level client in this client's network. StreamOpen does not carry the
// contract's provide mode, so this is the only trustworthy pre-handshake
// discriminator between an allowed Network-provider stream and a stale
// Public/FriendsAndFamily stream after those provide modes are disabled.
func (self *PeerManager) isConnectedNetworkPeer(clientId Id) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	_, ok := self.connectedPeers[clientId]
	return ok
}

// isDisconnectedNetworkPeer reports an explicit, unexpired platform
// disconnect marker. Public providers may still accept arbitrary clients, but
// a stream belonging to a client known to have disconnected is stale and must
// not be resurrected by a delayed StreamOpen/StreamReset.
func (self *PeerManager) isDisconnectedNetworkPeer(clientId Id) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	disconnectTime, ok := self.disconnectTimes[clientId]
	if !ok {
		return false
	}
	return !disconnectTime.Before(
		time.Now().Add(-self.peerManagerSettings.DisconnectedPeerWindow),
	)
}

func (self *PeerManager) disconnectedNetworkPeerIds() map[Id]bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	windowStart := time.Now().Add(-self.peerManagerSettings.DisconnectedPeerWindow)
	peerIds := map[Id]bool{}
	for clientId, disconnectTime := range self.disconnectTimes {
		if !disconnectTime.Before(windowStart) {
			peerIds[clientId] = true
		}
	}
	return peerIds
}

// ReceiveFunction
func (self *PeerManager) Receive(source TransferPath, frames []*protocol.Frame, peer Peer) {
	if source.IsControlSource() {
		changed := false
		for _, frame := range frames {
			frameChanged, err := self.handleControlFrame(frame)
			if err == nil && frameChanged {
				changed = true
			}
		}
		if changed {
			self.peersMonitor.NotifyAll()
			// Peer membership is part of strict Network-provider
			// authorization. Reconcile once after the entire received batch
			// so Reset+Update does not expose a transient empty peer set, and
			// so a disconnect retires its already-open stream immediately
			// instead of leaving P2P admission/retry work until another
			// provide-mode change.
			if self.client != nil {
				allowAny, allowNetwork := self.client.contractManager.inboundProviderStreamPolicy()
				self.client.streamManager.reconcileInboundProviderStreams(allowAny, allowNetwork)
				self.client.streamManager.closeDisconnectedPeerStreams(
					self.disconnectedNetworkPeerIds(),
				)
			}
		}
	}
}

func (self *PeerManager) handleControlFrame(frame *protocol.Frame) (bool, error) {
	switch frame.MessageType {
	case protocol.MessageType_TransferNetworkPeersReset, protocol.MessageType_TransferNetworkPeersUpdate:
		message, err := FromFrame(frame)
		if err != nil {
			return false, err
		}

		changed := false
		switch v := message.(type) {
		case *protocol.NetworkPeersReset:
			changed = self.resetPeers()
		case *protocol.NetworkPeersUpdate:
			changed, err = self.updatePeers(v)
			if err != nil {
				return changed, err
			}
		}
		return changed, nil
	}
	return false, nil
}

func (self *PeerManager) resetPeers() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	changed := 0 < len(self.connectedPeers) || 0 < len(self.disconnectTimes)
	clear(self.connectedPeers)
	clear(self.disconnectTimes)
	return changed
}

func (self *PeerManager) updatePeers(update *protocol.NetworkPeersUpdate) (bool, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	changed := false
	for _, p := range update.Peers {
		clientId, err := IdFromBytes(p.ClientId)
		if err != nil {
			return changed, err
		}
		if p.DisconnectTime != nil {
			if _, ok := self.connectedPeers[clientId]; ok {
				delete(self.connectedPeers, clientId)
				changed = true
			}
			disconnectTime := time.UnixMilli(int64(*p.DisconnectTime))
			if t, ok := self.disconnectTimes[clientId]; !ok || !t.Equal(disconnectTime) {
				self.disconnectTimes[clientId] = disconnectTime
				changed = true
			}
		} else {
			delete(self.disconnectTimes, clientId)
			self.connectedPeers[clientId] = &NetworkPeer{
				ClientId:       clientId,
				ProvideModes:   p.ProvideModes,
				ProvideEnabled: slices.Contains(p.ProvideModes, protocol.ProvideMode_Network),
				Principal:      p.Principal,
				Roles:          p.Roles,
				DeviceName:     p.DeviceName,
				DeviceSpec:     p.DeviceSpec,
			}
			changed = true
		}
	}
	return changed, nil
}

// NetworkPeers enumerates the connected peers and the count of
// peers disconnected within the disconnected peer window
func (self *PeerManager) NetworkPeers() (connected []*NetworkPeer, disconnectedCount int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	// age out disconnect markers older than the window
	windowStart := time.Now().Add(-self.peerManagerSettings.DisconnectedPeerWindow)
	for clientId, disconnectTime := range self.disconnectTimes {
		if disconnectTime.Before(windowStart) {
			delete(self.disconnectTimes, clientId)
		}
	}

	connected = make([]*NetworkPeer, 0, len(self.connectedPeers))
	for _, networkPeer := range self.connectedPeers {
		connected = append(connected, networkPeer)
	}
	disconnectedCount = len(self.disconnectTimes)
	return
}
