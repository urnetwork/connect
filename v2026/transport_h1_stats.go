package connect

import "sync"

// H1ConnectionStats counts live, authenticated H1 connections whose routes are
// registered. Share one collector across a client's window or a provider's
// transport generations. It does not count pending upgrades or remember closed
// connections, and adds no work to packet reads or writes.
type H1ConnectionStats struct {
	mu       sync.Mutex
	snapshot H1ConnectionStatsSnapshot
}

type H1ConnectionStatsSnapshot struct {
	WebSocketConnectionCount int64
	H1PlusConnectionCount    int64
}

func (s *H1ConnectionStats) Snapshot() H1ConnectionStatsSnapshot {
	if s == nil {
		return H1ConnectionStatsSnapshot{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshot
}

func (s *H1ConnectionStats) connected(conn H1MessageConn) func() {
	if s == nil {
		return func() {}
	}
	// The compact framed connection only exists after the dialer validates the
	// custom 101 response. A rejected/mismatched upgrade returns a WebSocket.
	_, framed := conn.(*FramedMessageConn)
	s.mu.Lock()
	if framed {
		s.snapshot.H1PlusConnectionCount++
	} else {
		s.snapshot.WebSocketConnectionCount++
	}
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if framed {
			s.snapshot.H1PlusConnectionCount--
		} else {
			s.snapshot.WebSocketConnectionCount--
		}
	}
}
