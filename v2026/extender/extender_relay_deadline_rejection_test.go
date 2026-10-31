// Rejected socket bounds terminate relay ownership before unbounded I/O.
// Explicit read barriers prove both connections close and workers are joined.
package extender

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// Records a rejected bound and any forbidden operation after it. A held read
// returns only after the test observes connection closure and releases it.
type relayDeadlineRejectConn struct {
	net.Conn
	rejectKind       string
	rejected         atomic.Bool
	ioAfterRejection atomic.Int64
	activeIo         atomic.Int64
	writeCount       atomic.Int64
	zeroWrite        bool
	readStarted      chan struct{}
	readStartedOnce  sync.Once
	holdRead         bool
	readReturned     chan struct{}
	readRelease      chan struct{}
	rejectionEntered chan struct{}
	rejectionRelease chan struct{}
	closed           chan struct{}
	closeOnce        sync.Once
}

// Creates a transparent endpoint until the configured deadline is rejected.
func newRelayDeadlineRejectConn(conn net.Conn) *relayDeadlineRejectConn {
	return &relayDeadlineRejectConn{
		Conn:        conn,
		readStarted: make(chan struct{}),
		closed:      make(chan struct{}),
	}
}

// Rejection can wait for the opposite worker to enter its native read.
func (self *relayDeadlineRejectConn) reject(kind string) error {
	if self.rejectKind != kind {
		return nil
	}
	if self.rejectionEntered != nil {
		close(self.rejectionEntered)
		<-self.rejectionRelease
	}
	self.rejected.Store(true)
	return errors.New("synthetic relay deadline rejection")
}

// Setup clears must fail closed even with the idle budget disabled.
func (self *relayDeadlineRejectConn) SetDeadline(deadline time.Time) error {
	if err := self.reject("clear"); err != nil {
		return err
	}
	return self.Conn.SetDeadline(deadline)
}

// A rejected read bound must never be followed by a read.
func (self *relayDeadlineRejectConn) SetReadDeadline(deadline time.Time) error {
	if err := self.reject("read"); err != nil {
		return err
	}
	return self.Conn.SetReadDeadline(deadline)
}

// A rejected write bound must never be followed by a write.
func (self *relayDeadlineRejectConn) SetWriteDeadline(deadline time.Time) error {
	if err := self.reject("write"); err != nil {
		return err
	}
	return self.Conn.SetWriteDeadline(deadline)
}

// The unexpected I/O returns an error so the unfixed regression stays bounded.
func (self *relayDeadlineRejectConn) Read(message []byte) (int, error) {
	self.activeIo.Add(1)
	defer self.activeIo.Add(-1)
	if self.rejected.Load() {
		self.ioAfterRejection.Add(1)
		return 0, errors.New("synthetic unbounded relay read")
	}
	self.readStartedOnce.Do(func() { close(self.readStarted) })
	n, err := self.Conn.Read(message)
	if self.holdRead {
		close(self.readReturned)
		<-self.readRelease
	}
	return n, err
}

// Zero progress fails on a second attempt only to bound the pre-fix busy loop.
func (self *relayDeadlineRejectConn) Write(message []byte) (int, error) {
	self.activeIo.Add(1)
	defer self.activeIo.Add(-1)
	count := self.writeCount.Add(1)
	if self.rejected.Load() {
		self.ioAfterRejection.Add(1)
		return 0, errors.New("synthetic unbounded relay write")
	}
	if self.zeroWrite {
		if count == 1 {
			return 0, nil
		}
		return 0, io.ErrNoProgress
	}
	return self.Conn.Write(message)
}

// Closure interrupts native I/O but does not release the worker-return gate.
func (self *relayDeadlineRejectConn) Close() error {
	err := self.Conn.Close()
	self.closeOnce.Do(func() { close(self.closed) })
	return err
}

// Both endpoint positions and both relay implementations have the same
// failure contract. Read and write cases hold the other worker after Close.
func testRelayDeadlineRejection(t *testing.T, splice bool, kind string) {
	t.Helper()
	for _, rejectClient := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			client, clientPeer := net.Pipe()
			forward, forwardPeer := net.Pipe()
			defer clientPeer.Close()
			defer forwardPeer.Close()
			clientConn := newRelayDeadlineRejectConn(client)
			forwardConn := newRelayDeadlineRejectConn(forward)
			target, other := forwardConn, clientConn
			sourcePeer := clientPeer
			if rejectClient {
				target, other = clientConn, forwardConn
				sourcePeer = forwardPeer
			}
			target.rejectKind = kind
			target.zeroWrite = kind == "zero"
			settings := DefaultExtenderSettings()
			if kind == "clear" {
				settings.ReadTimeout = 0
				settings.WriteTimeout = 0
				settings.ProxyIdleTimeout = 0
			}
			var held *relayDeadlineRejectConn
			if kind != "clear" {
				held = target
				if kind == "read" {
					held = other
					target.rejectionEntered = make(chan struct{})
					target.rejectionRelease = make(chan struct{})
				}
				held.holdRead = true
				held.readReturned = make(chan struct{})
				held.readRelease = make(chan struct{})
			}
			releaseRejection := sync.OnceFunc(func() {
				if target.rejectionRelease != nil {
					close(target.rejectionRelease)
				}
			})
			releaseRead := sync.OnceFunc(func() {
				if held != nil {
					close(held.readRelease)
				}
			})
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			server := &ExtenderServer{settings: settings}
			go func() {
				defer close(done)
				if splice {
					camouflage := &extenderCamouflage{server: server, settings: settings}
					camouflage.relaySplice(ctx, clientConn, forwardConn, nil)
				} else {
					server.relay(ctx, cancel, clientConn, forwardConn)
				}
			}()
			defer func() {
				releaseRejection()
				releaseRead()
				cancel()
				clientConn.Close()
				forwardConn.Close()
				<-done
			}()
			if held != nil {
				<-held.readStarted
				if kind == "read" {
					<-target.rejectionEntered
					releaseRejection()
				} else if _, err := sourcePeer.Write([]byte{0x42}); err != nil {
					t.Fatal(err)
				}
				<-held.readReturned
				synctest.Wait()
				select {
				case <-done:
					t.Fatal("relay returned before its interrupted worker was joined")
				default:
				}
			}
			synctest.Wait()
			for _, conn := range []*relayDeadlineRejectConn{clientConn, forwardConn} {
				select {
				case <-conn.closed:
				default:
					t.Fatal("failed relay retained an endpoint")
				}
			}
			releaseRead()
			<-done
			if !splice && ctx.Err() == nil {
				t.Fatal("failed relay did not cancel its lifetime")
			}
			if clientConn.activeIo.Load() != 0 || forwardConn.activeIo.Load() != 0 {
				t.Fatal("relay returned with active socket I/O")
			}
			if kind == "zero" {
				if got := target.writeCount.Load(); got != 1 {
					t.Fatalf("client=%t: zero-progress write retried %d times, want one call", rejectClient, got)
				}
			} else if !target.rejected.Load() || target.ioAfterRejection.Load() != 0 {
				t.Fatalf("client=%t kind=%s: rejected=%t I/O after rejection=%d", rejectClient, kind, target.rejected.Load(), target.ioAfterRejection.Load())
			}
		})
	}
}

// Setup rejection owns closure even before any copy worker starts.
func TestExtenderRelayRejectsSetupDeadlineClear(t *testing.T) {
	testRelayDeadlineRejection(t, false, "clear")
}

// A failed read setter cancels and joins the healthy reverse read.
func TestExtenderRelayRejectsReadDeadline(t *testing.T) {
	testRelayDeadlineRejection(t, false, "read")
}

// A failed write setter never forwards the already-read payload.
func TestExtenderRelayRejectsWriteDeadline(t *testing.T) {
	testRelayDeadlineRejection(t, false, "write")
}

// The forward relay must terminate the same invalid writer as the splice.
func TestExtenderRelayRejectsZeroProgressWrite(t *testing.T) {
	testRelayDeadlineRejection(t, false, "zero")
}

// A splice setup failure closes both endpoints with no workers to join.
func TestExtenderCamouflageRejectsSetupDeadlineClear(t *testing.T) {
	testRelayDeadlineRejection(t, true, "clear")
}

// Splice reads require their native deadline to be accepted.
func TestExtenderCamouflageRejectsReadDeadline(t *testing.T) {
	testRelayDeadlineRejection(t, true, "read")
}

// Splice payload writes require the same fail-closed deadline handling.
func TestExtenderCamouflageRejectsWriteDeadline(t *testing.T) {
	testRelayDeadlineRejection(t, true, "write")
}

// The initial replay uses the writer before copy workers exist.
func TestExtenderCamouflageRejectsHelloWriteDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, clientPeer := net.Pipe()
		site, sitePeer := net.Pipe()
		defer clientPeer.Close()
		defer sitePeer.Close()
		clientConn := newRelayDeadlineRejectConn(client)
		siteConn := newRelayDeadlineRejectConn(site)
		defer clientConn.Close()
		defer siteConn.Close()
		siteConn.rejectKind = "write"
		camouflage := &extenderCamouflage{settings: DefaultExtenderSettings()}
		camouflage.relaySplice(context.Background(), clientConn, siteConn, []byte("synthetic hello"))
		if !siteConn.rejected.Load() || siteConn.ioAfterRejection.Load() != 0 {
			t.Fatalf("hello write followed rejected deadline: %d", siteConn.ioAfterRejection.Load())
		}
		for _, conn := range []*relayDeadlineRejectConn{clientConn, siteConn} {
			select {
			case <-conn.closed:
			default:
				t.Fatal("failed hello retained an endpoint")
			}
		}
	})
}
