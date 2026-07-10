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

// ReceiveFunction
func (self *PeerManager) Receive(source TransferPath, frames []*protocol.Frame, peer Peer) {
	if source.IsControlSource() {
		for _, frame := range frames {
			// ignore error
			self.handleControlFrame(frame)
		}
	}
}

func (self *PeerManager) handleControlFrame(frame *protocol.Frame) error {
	switch frame.MessageType {
	case protocol.MessageType_TransferNetworkPeersReset, protocol.MessageType_TransferNetworkPeersUpdate:
		message, err := FromFrame(frame)
		if err != nil {
			return err
		}

		changed := false
		switch v := message.(type) {
		case *protocol.NetworkPeersReset:
			changed = self.resetPeers()
		case *protocol.NetworkPeersUpdate:
			changed, err = self.updatePeers(v)
			if err != nil {
				return err
			}
		}
		if changed {
			self.peersMonitor.NotifyAll()
		}
	}
	return nil
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
