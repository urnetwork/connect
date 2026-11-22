// A verified platform connection owns provenance for its physical receive lanes.
// Ordinary receives carry it without locking. Only an enrollment decision may
// pin that connection for a bounded, synchronous local commit.
package connect

import (
	"crypto/tls"
	"sync"
)

// The reliability of H3 DATAGRAM differs from the stream on the same connection.
type platformReceiveLane uint8

const (
	platformReceiveH1 platformReceiveLane = iota + 1
	platformReceiveH3Stream
	platformReceiveH3Datagram
)

// The platform has accepted this exact physical connection's client login.
type platformReceiveOwner struct {
	clientId       Id
	done           <-chan struct{}
	connectionDone <-chan struct{}
	stateLock      sync.Mutex
	retired        bool
}

// Created only after login on a connection with verified platform TLS.
func newPlatformReceiveOwner(clientId Id, done, connectionDone <-chan struct{}, verified bool) *platformReceiveOwner {
	if !verified || clientId == (Id{}) || done == nil {
		return nil
	}
	return &platformReceiveOwner{clientId: clientId, done: done, connectionDone: connectionDone}
}

// Revokes all lanes before the transport removes their routes. A decision
// which already pinned the owner can finish before retirement.
func (self *platformReceiveOwner) retire() {
	if self != nil {
		self.stateLock.Lock()
		self.retired = true
		self.stateLock.Unlock()
	}
}

// An immutable receipt issued to one route channel of its connection owner.
type platformReceiveProvenance struct {
	owner *platformReceiveOwner
	lane  platformReceiveLane
}

// Only the actual platform carrier attaches a receipt to a physical lane.
func (self *platformReceiveOwner) provenance(lane platformReceiveLane) *platformReceiveProvenance {
	if self == nil {
		return nil
	}
	return &platformReceiveProvenance{owner: self, lane: lane}
}

// Runs a bounded local commit against the exact live physical owner and its
// authenticated client id. The commit must not wait on transport I/O or close.
func (self *platformReceiveProvenance) withLiveOwner(clientId Id, commit func()) bool {
	if self == nil || self.owner.clientId != clientId {
		return false
	}
	owner := self.owner
	owner.stateLock.Lock()
	defer owner.stateLock.Unlock()
	if owner.retired {
		return false
	}
	select {
	case <-owner.done:
		return false
	case <-owner.connectionDone:
		return false
	default:
	}
	commit()
	return true
}

// The configured platform name must match the leaf from the actual verified
// handshake. Existing dialers may use a different name in their TLS config.
func verifiedPlatformTlsState(serverName string, state tls.ConnectionState) bool {
	return serverName != "" && state.HandshakeComplete && len(state.VerifiedChains) != 0 &&
		len(state.PeerCertificates) != 0 && state.PeerCertificates[0].VerifyHostname(serverName) == nil
}

// Follows only the platform dialer's underlying TCP/TLS wrappers. The strategy
// delivery and H1 delay message wrappers delegate UnderlyingConn unchanged.
func verifiedH1PlatformTls(platformUrl string, messageConn H1MessageConn) bool {
	serverName, err := connectHost(platformUrl)
	if err != nil {
		return false
	}
	conn := messageConn.UnderlyingConn()
	for {
		switch wrapped := conn.(type) {
		case *WebSocketWriteBatchConn:
			conn = wrapped.conn
		case *httpUpgradeCloseConn:
			conn = wrapped.Conn
		case *httpUpgradeConn:
			conn = wrapped.Conn
		case *tls.Conn:
			return verifiedPlatformTlsState(serverName, wrapped.ConnectionState())
		case *chromeTlsConn:
			return verifiedPlatformTlsState(serverName, wrapped.ConnectionState())
		default:
			return false
		}
	}
}
