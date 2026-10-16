package connect

// A cached HTTP client owns its live native streams until every admitted
// evaluation has consumed or canceled its response. Retirement drains that
// owner, not a snapshot of whichever connections happen to be idle.

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync"
)

// Pool state is separate from the dialer so a reusable reset can publish a
// new pool while responses and detached dials still belong to the old one.
type clientHttpPool struct {
	httpClient *http.Client
	ctx        context.Context
	cancel     context.CancelFunc

	stateLock   sync.Mutex
	activeCount int
	retired     bool
	drained     bool
	conns       map[*clientHttpPoolConn]bool
}

// Allocates once per cached client; request leases require no new allocation.
func newClientHttpPool() *clientHttpPool {
	ctx, cancel := context.WithCancel(context.Background())
	return &clientHttpPool{ctx: ctx, cancel: cancel}
}

// Admission and retirement share the same lock. Existing leases may still
// establish a connection or follow redirects after retirement begins.
func (self *clientHttpPool) acquire() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.retired {
		return false
	}
	self.activeCount++
	return true
}

// A discarded owner's final idle sweep cannot observe asynchronous HTTP/2
// cleanup. Once all response owners finish, its native streams can close.
func (self *clientHttpPool) release() {
	var conns map[*clientHttpPoolConn]bool
	var retired, drained bool
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.activeCount--
		retired = self.retired
		if retired && self.activeCount == 0 {
			self.drained = true
			drained = true
			conns, self.conns = self.conns, nil
		}
	}()
	if retired {
		self.closeRetired(conns, drained)
	}
}

// Marks only this cached client terminal; the containing dialer may reset.
func (self *clientHttpPool) retire() {
	var conns map[*clientHttpPoolConn]bool
	var drained bool
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.retired = true
		if self.activeCount == 0 && !self.drained {
			self.drained = true
			drained = true
			conns, self.conns = self.conns, nil
		}
	}()
	self.closeRetired(conns, drained)
}

// All external cleanup is outside state locks. Closing a stream unregisters
// it, and an alt pool is terminal only after its last response owner finishes.
func (self *clientHttpPool) closeRetired(conns map[*clientHttpPoolConn]bool, drained bool) {
	if drained {
		self.cancel()
	}
	self.httpClient.CloseIdleConnections()
	for conn := range conns {
		conn.Close()
	}
	if drained {
		if transport, ok := self.httpClient.Transport.(*altQuicBoundedTransport); ok {
			transport.Close()
		}
	}
}

// A value is installed only while dialing, never on the hot request path.
type clientHttpPoolContextKey struct{}

// Keeps the ordinary native transport and top-level *tls.Conn unchanged.
// Detached net/http dials are canceled when the old owner actually drains.
func (self *clientHttpPool) dialContext(dialContext DialContextFunction, secure bool) DialContextFunction {
	if dialContext == nil {
		return nil
	}
	return func(ctx context.Context, network string, address string) (net.Conn, error) {
		self.stateLock.Lock()
		drained := self.drained
		self.stateLock.Unlock()
		if drained {
			return nil, errClientDialerRetired
		}
		linkedCtx, cancel := context.WithCancel(ctx)
		stop := context.AfterFunc(self.ctx, cancel)
		defer stop()
		defer cancel()
		if secure {
			linkedCtx = context.WithValue(linkedCtx, clientHttpPoolContextKey{}, self)
		}
		conn, err := dialContext(linkedCtx, network, address)
		if err != nil {
			return nil, err
		}
		if !secure {
			return self.register(conn)
		}
		self.stateLock.Lock()
		drained = self.drained
		self.stateLock.Unlock()
		if drained {
			conn.Close()
			return nil, errClientDialerRetired
		}
		return conn, nil
	}
}

// Registration precedes handing a raw stream to TLS. A concurrent drain
// either owns it in the set or refuses and closes it before publication.
func (self *clientHttpPool) register(conn net.Conn) (net.Conn, error) {
	owned := &clientHttpPoolConn{Conn: conn, pool: self}
	admitted := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.drained {
			return false
		}
		if self.conns == nil {
			self.conns = map[*clientHttpPoolConn]bool{}
		}
		self.conns[owned] = true
		return true
	}()
	if !admitted {
		conn.Close()
		return nil, errClientDialerRetired
	}
	return owned, nil
}

// Native TLS constructors call this immediately below the final TLS layer,
// outside any resilient wrapper that needs the concrete underlying TCP socket.
func ownClientHttpPoolConn(ctx context.Context, conn net.Conn) (net.Conn, error) {
	if pool, ok := ctx.Value(clientHttpPoolContextKey{}).(*clientHttpPool); ok {
		return pool.register(conn)
	}
	return conn, nil
}

// Closed streams unregister, so a long-lived live pool retains only live
// connections rather than every connection it has ever established.
type clientHttpPoolConn struct {
	net.Conn
	pool *clientHttpPool
	once sync.Once
	err  error
}

// Deregistration and the underlying close never hold the same lock scope.
func (self *clientHttpPoolConn) Close() error {
	self.once.Do(func() {
		func() {
			self.pool.stateLock.Lock()
			defer self.pool.stateLock.Unlock()
			delete(self.pool.conns, self)
		}()
		self.err = self.Conn.Close()
	})
	return self.err
}

// Preserve optimized plain TCP uploads without recursing through this wrapper.
func (self *clientHttpPoolConn) ReadFrom(reader io.Reader) (int64, error) {
	return io.Copy(self.Conn, reader)
}

// Preserve the underlying connection's optimized copy-to path when available.
func (self *clientHttpPoolConn) WriteTo(writer io.Writer) (int64, error) {
	return io.Copy(writer, self.Conn)
}
