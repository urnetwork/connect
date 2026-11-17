// The tls session cache of the operator links that go through a supplied
// dialer (NodeSettings.DialContext, transport_websocket.go, connect#227).
//
// One cache serves the whole process, by the owner's decision of 2026-11-17. A
// miner runs a node per proxy slot, and every slot's link reaches the same
// operator. With the cache, a link the operator refuses after tls, or one that
// drops and is dialed again, resumes from a session any slot was issued.
// Without it, every attempt is a full handshake that verifies the operator's
// whole P-384 chain, which was most of the gossip cost in the reporter's
// profiles.
//
// Each session is handed out once (RFC 8446 C.4). The slots leave through
// different proxies, and a ticket presented on two connections would let
// anyone who sees both link those proxies. The operator, which issued the
// ticket, can link them either way. The operator's lb issues two tickets per
// full handshake and one per resumption, so dials in flight together each
// still find a ticket of their own.
//
// It keeps sessions for at most `MaxServerCount` servers, dropping the least
// recently used one, and at most `MaxSessionCountPerServer` per server,
// dropping the oldest.
//
// The cache is safe for concurrent use.

package gossip

import (
	"crypto/tls"
	"slices"
	"sync"
)

type tlsSessionCacheSettings struct {
	// Servers whose sessions are kept. A process's operator links reach one
	// host per network space, so a handful is plenty. Non-positive takes the
	// default, 16.
	MaxServerCount int
	// Sessions kept per server. This many dials to one server in flight at
	// once each resume on a ticket of their own. Non-positive takes the
	// default, 64.
	MaxSessionCountPerServer int
}

func defaultTlsSessionCacheSettings() *tlsSessionCacheSettings {
	return &tlsSessionCacheSettings{
		MaxServerCount:           16,
		MaxSessionCountPerServer: 64,
	}
}

// The process's cache, made on first use from the defaults.
var processTlsSessionCache = sync.OnceValue(func() *tlsSessionCache {
	return newTlsSessionCache(defaultTlsSessionCacheSettings())
})

type tlsSessionCache struct {
	settings *tlsSessionCacheSettings

	stateLock sync.Mutex
	// the servers with sessions, least recently used first
	servers []*tlsServerSessions
}

// The sessions one server issued, oldest first.
type tlsServerSessions struct {
	serverKey string
	sessions  []*tls.ClientSessionState
}

var _ tls.ClientSessionCache = (*tlsSessionCache)(nil)

func newTlsSessionCache(settings *tlsSessionCacheSettings) *tlsSessionCache {
	copied := *settings
	defaults := defaultTlsSessionCacheSettings()
	if copied.MaxServerCount <= 0 {
		copied.MaxServerCount = defaults.MaxServerCount
	}
	if copied.MaxSessionCountPerServer <= 0 {
		copied.MaxSessionCountPerServer = defaults.MaxSessionCountPerServer
	}
	return &tlsSessionCache{
		settings: &copied,
	}
}

// Hands out the newest session of a server and forgets it, so no session is
// presented twice. crypto/tls checks what it is handed -- the server name, the
// verified chain, the session's expiry -- before it offers it.
func (self *tlsSessionCache) Get(serverKey string) (*tls.ClientSessionState, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	i := self.serverIndexWithLock(serverKey)
	if i < 0 {
		return nil, false
	}
	server := self.servers[i]
	self.servers = slices.Delete(self.servers, i, i+1)
	lastIndex := len(server.sessions) - 1
	session := server.sessions[lastIndex]
	server.sessions = slices.Delete(server.sessions, lastIndex, lastIndex+1)
	if 0 < len(server.sessions) {
		self.servers = append(self.servers, server)
	}
	return session, true
}

// Keeps a session a server issued. A nil session is crypto/tls asking to forget
// the session it was handed, after a failed resumption or a session it would
// not offer. Get already forgot that one, and the server's other sessions stand
// on their own: an unknown ticket costs a full handshake, not a failure.
func (self *tlsSessionCache) Put(serverKey string, session *tls.ClientSessionState) {
	if session == nil {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	var server *tlsServerSessions
	if i := self.serverIndexWithLock(serverKey); 0 <= i {
		server = self.servers[i]
		self.servers = slices.Delete(self.servers, i, i+1)
	} else {
		server = &tlsServerSessions{
			serverKey: serverKey,
		}
		if self.settings.MaxServerCount <= len(self.servers) {
			self.servers = slices.Delete(self.servers, 0, len(self.servers)-self.settings.MaxServerCount+1)
		}
	}
	server.sessions = append(server.sessions, session)
	if self.settings.MaxSessionCountPerServer < len(server.sessions) {
		server.sessions = slices.Delete(
			server.sessions,
			0,
			len(server.sessions)-self.settings.MaxSessionCountPerServer,
		)
	}
	self.servers = append(self.servers, server)
}

func (self *tlsSessionCache) serverIndexWithLock(serverKey string) int {
	return slices.IndexFunc(self.servers, func(server *tlsServerSessions) bool {
		return server.serverKey == serverKey
	})
}
