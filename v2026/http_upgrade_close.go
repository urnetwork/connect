// Handshake cleanup and cancellation share one physical close result. A
// successful handshake transfers this connection without closing it.
package connect

import (
	"net"
	"sync"
)

// Concurrent Close callers observe the same original outcome after Once has
// joined the physical close; later cleanup cannot manufacture a second error.
type httpUpgradeCloseConn struct {
	net.Conn
	closeOnce sync.Once
	closeErr  error
}

func (self *httpUpgradeCloseConn) Close() error {
	self.closeOnce.Do(func() { self.closeErr = self.Conn.Close() })
	return self.closeErr
}
