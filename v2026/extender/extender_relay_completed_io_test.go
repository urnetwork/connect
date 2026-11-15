// A completed socket operation survives its caller being descheduled before
// forwarding or starting the next operation. A separate watchdog cannot infer
// that completion from the last operation's start time.
package extender

import (
	"context"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// Holds one successful physical operation after the socket has completed it.
// The barrier models scheduling or TLS processing before the caller resumes.
type relayCompletedIoConn struct {
	net.Conn
	write       bool
	armed       atomic.Bool
	completed   chan struct{}
	release     chan struct{}
	closed      chan struct{}
	releaseOnce sync.Once
	closeOnce   sync.Once
}

// A disarmed barrier lets setup finish before the operation under test.
func newRelayCompletedIoConn(conn net.Conn, write bool) *relayCompletedIoConn {
	return &relayCompletedIoConn{
		Conn:      conn,
		write:     write,
		completed: make(chan struct{}),
		release:   make(chan struct{}),
		closed:    make(chan struct{}),
	}
}

// Completes the underlying read before holding its return to the relay.
func (self *relayCompletedIoConn) Read(p []byte) (int, error) {
	n, err := self.Conn.Read(p)
	if !self.write && 0 < n && err == nil && self.armed.CompareAndSwap(true, false) {
		close(self.completed)
		<-self.release
	}
	return n, err
}

// Completes the underlying write before holding its return to the relay.
func (self *relayCompletedIoConn) Write(p []byte) (int, error) {
	n, err := self.Conn.Write(p)
	if self.write && 0 < n && err == nil && self.armed.CompareAndSwap(true, false) {
		close(self.completed)
		<-self.release
	}
	return n, err
}

// Records closure independently of the deliberately held operation.
func (self *relayCompletedIoConn) Close() error {
	self.closeOnce.Do(func() { close(self.closed) })
	return self.Conn.Close()
}

// Releases the operation, including when an assertion fails.
func (self *relayCompletedIoConn) resume() {
	self.releaseOnce.Do(func() { close(self.release) })
}

// Tests the two directions with the same 29 s completion and 31 s resume.
// The reverse direction moves at 20 s, so its own 30 s read cannot expire.
func testExtenderCompletedIo(t *testing.T, splice bool, write bool) {
	t.Helper()
	for _, reverse := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			settings := DefaultExtenderSettings()
			clientConn, clientPeer := net.Pipe()
			forwardConn, forwardPeer := net.Pipe()
			defer clientPeer.Close()
			defer forwardPeer.Close()
			sourceConn, sourcePeer := clientConn, clientPeer
			destinationConn, destinationPeer := forwardConn, forwardPeer
			if reverse {
				sourceConn, sourcePeer, destinationConn, destinationPeer = forwardConn, forwardPeer, clientConn, clientPeer
			}
			wrappedConn := sourceConn
			if write {
				wrappedConn = destinationConn
			}
			barrier := newRelayCompletedIoConn(wrappedConn, write)
			var relayClientConn net.Conn = clientConn
			var relayForwardConn net.Conn = forwardConn
			if wrappedConn == clientConn {
				relayClientConn = barrier
			} else {
				relayForwardConn = barrier
			}
			ctx, cancel := context.WithCancel(context.Background())
			server := &ExtenderServer{settings: settings}
			done := make(chan struct{})
			go func() {
				defer close(done)
				if splice {
					camouflage := &extenderCamouflage{server: server, settings: settings}
					camouflage.relaySplice(ctx, relayClientConn, relayForwardConn, nil)
				} else {
					server.relay(ctx, cancel, relayClientConn, relayForwardConn)
				}
			}()
			defer func() {
				barrier.resume()
				cancel()
				<-done
			}()
			synctest.Wait()
			barrier.armed.Store(true)
			payload := []byte{0x42}
			if write {
				if _, err := sourcePeer.Write(payload); err != nil {
					t.Fatal(err)
				}
			}
			time.Sleep(20 * time.Second)
			relayTestChunk(t, destinationPeer, sourcePeer, 1, 0x22)
			time.Sleep(9 * time.Second)
			if write {
				if _, err := io.ReadFull(destinationPeer, make([]byte, len(payload))); err != nil {
					t.Fatal(err)
				}
			} else if _, err := sourcePeer.Write(payload); err != nil {
				t.Fatal(err)
			}
			<-barrier.completed
			time.Sleep(2 * time.Second)
			synctest.Wait()
			select {
			case <-barrier.closed:
				t.Fatalf("reverse=%t write=%t: relay closed after I/O completed at 29 s within its 30 s deadline", reverse, write)
			default:
			}
			barrier.resume()
			if !write {
				received := make([]byte, len(payload))
				if _, err := io.ReadFull(destinationPeer, received); err != nil || received[0] != payload[0] {
					t.Fatalf("completed read was not forwarded: %x, %v", received, err)
				}
			}
			relayTestRounds(t, 1, 0, sourcePeer, destinationPeer, 1)
		})
	}
}

// The relay must not turn a successful read into a timeout before forwarding.
func TestExtenderRelayCompletedReadSurvivesSchedulingDelay(t *testing.T) {
	testExtenderCompletedIo(t, false, false)
}

// The relay must renew the next operation's deadline after a successful write.
func TestExtenderRelayCompletedWriteSurvivesSchedulingDelay(t *testing.T) {
	testExtenderCompletedIo(t, false, true)
}

// The splice has the same completion boundary in both directions.
func TestExtenderCamouflageCompletedReadSurvivesSchedulingDelay(t *testing.T) {
	testExtenderCompletedIo(t, true, false)
}

// Successful splice writes remain successful while their caller is suspended.
func TestExtenderCamouflageCompletedWriteSurvivesSchedulingDelay(t *testing.T) {
	testExtenderCompletedIo(t, true, true)
}

// The initial hello completes before its deadline even though the relay only
// resumes after that deadline, before either copy worker has started.
func TestExtenderCamouflageCompletedHelloSurvivesSchedulingDelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clientConn, clientPeer := net.Pipe()
		siteConn, sitePeer := net.Pipe()
		defer clientPeer.Close()
		defer sitePeer.Close()
		barrier := newRelayCompletedIoConn(siteConn, true)
		barrier.armed.Store(true)
		ctx, cancel := context.WithCancel(context.Background())
		settings := DefaultExtenderSettings()
		server := &ExtenderServer{settings: settings}
		camouflage := &extenderCamouflage{server: server, settings: settings}
		done := make(chan struct{})
		go func() {
			defer close(done)
			camouflage.relaySplice(ctx, clientConn, barrier, []byte{0x42})
		}()
		defer func() {
			barrier.resume()
			cancel()
			<-done
		}()
		synctest.Wait()
		time.Sleep(29 * time.Second)
		if _, err := io.ReadFull(sitePeer, make([]byte, 1)); err != nil {
			t.Fatal(err)
		}
		<-barrier.completed
		time.Sleep(2 * time.Second)
		synctest.Wait()
		select {
		case <-barrier.closed:
			t.Fatal("splice closed after the hello completed within its deadline")
		default:
		}
		barrier.resume()
		relayTestRounds(t, 1, 0, clientPeer, sitePeer, 1)
	})
}

// Disabling the read budget does not disable a blocked write's own deadline.
func TestExtenderRelayDisabledReadTimeoutKeepsWriteBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultExtenderSettings()
		settings.ReadTimeout = 0
		clientConn, clientPeer := net.Pipe()
		forwardConn, forwardPeer := net.Pipe()
		defer clientPeer.Close()
		defer forwardPeer.Close()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		server := &ExtenderServer{settings: settings}
		start := time.Now()
		ended := relayTestRun(start, func() { server.relay(ctx, cancel, clientConn, forwardConn) })
		if _, err := clientPeer.Write([]byte{0x42}); err != nil {
			t.Fatal(err)
		}
		expectRelayTestEnd(t, start, settings.WriteTimeout, ended)
	})
}

// With both budgets disabled, setup deadlines are cleared and later traffic
// still relays. Explicit cancellation continues to close and join both sides.
func TestExtenderRelayNonPositiveTimeoutsStayUnbounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultExtenderSettings()
		settings.ReadTimeout = 0
		settings.WriteTimeout = -time.Second
		clientConn, clientPeer := net.Pipe()
		forwardConn, forwardPeer := net.Pipe()
		defer clientPeer.Close()
		defer forwardPeer.Close()
		clientConn.SetDeadline(time.Now().Add(time.Second))
		forwardConn.SetDeadline(time.Now().Add(time.Second))
		ctx, cancel := context.WithCancel(context.Background())
		server := &ExtenderServer{settings: settings}
		done := make(chan struct{})
		go func() {
			defer close(done)
			server.relay(ctx, cancel, clientConn, forwardConn)
		}()
		defer func() { cancel(); <-done }()
		synctest.Wait()
		time.Sleep(time.Hour)
		relayTestRounds(t, 1, 0, clientPeer, forwardPeer, 1)
	})
}
