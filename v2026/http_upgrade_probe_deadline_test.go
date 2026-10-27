// The private probe timer must not replace the socket's deadline outcome with
// an owned close. Gate the real pipe read until the timer callback has run.
package connect

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gorilla/websocket"
)

// The underlying net.Pipe retains its real deadline. Only delivery of its
// first read is gated, so the deadline callback deterministically runs first.
type h1ProbeDeadlineTestConn struct {
	net.Conn
	readStarted chan struct{}
	releaseRead chan struct{}
	readOnce    sync.Once
	releaseOnce sync.Once
	readCause   error
	closeCause  error
	closes      atomic.Int32
}

// Request writes and socket deadlines remain the original net.Pipe operations.
func (self *h1ProbeDeadlineTestConn) Read(buffer []byte) (int, error) {
	self.readOnce.Do(func() {
		close(self.readStarted)
		<-self.releaseRead
	})
	count, err := self.Conn.Read(buffer)
	if err != nil {
		err = errors.Join(err, self.readCause)
	}
	return count, err
}

// A genuine independent close error stays distinct from timer ownership.
func (self *h1ProbeDeadlineTestConn) Close() error {
	self.closes.Add(1)
	return errors.Join(self.Conn.Close(), self.closeCause)
}

// Cleanup can release the read even after an earlier assertion fails.
func (self *h1ProbeDeadlineTestConn) release() {
	self.releaseOnce.Do(func() { close(self.releaseRead) })
}

// Only the first custom connection is gated; fallback requires a fresh real
// pipe and the peer's independently computed WebSocket handshake response.
func newH1ProbeDeadlineTestDialer(t *testing.T, peer *h1DeadlinePeer, timeout time.Duration, readCause, closeCause error) (*websocket.Dialer, *h1ProbeDeadlineTestConn) {
	t.Helper()
	connection := &h1ProbeDeadlineTestConn{readStarted: make(chan struct{}), releaseRead: make(chan struct{}), readCause: readCause, closeCause: closeCause}
	t.Cleanup(connection.release)
	dialer := peer.wsDialer(timeout)
	original := dialer.NetDialTLSContext
	var dials atomic.Int32
	dialer.NetDialTLSContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		physical, err := original(ctx, network, address)
		if err != nil || dials.Add(1) != 1 {
			return physical, err
		}
		connection.Conn = physical
		return connection, nil
	}
	return dialer, connection
}

// The result channel joins the actual public dial before inspecting counters.
type h1ProbeDeadlineTestResult struct {
	connection H1MessageConn
	err        error
}

// A timed-out custom probe must still negotiate a fresh WebSocket using only
// the remaining default, RPC or earlier-caller budget. No wall time is slept.
func TestDialH1MessagesProbeDeadlineFallsBackAfterReadGate(t *testing.T) {
	for _, item := range []struct {
		native time.Duration
		outer  time.Duration
		probe  time.Duration
		total  time.Duration
	}{
		{native: 5 * time.Second, probe: 2500 * time.Millisecond, total: 5 * time.Second},
		{native: 30 * time.Second, probe: 5 * time.Second, total: 30 * time.Second},
		{native: 30 * time.Second, outer: 4 * time.Second, probe: 2 * time.Second, total: 4 * time.Second},
	} {
		synctest.Test(t, func(t *testing.T) {
			resetH1UpgradeTestState(t)
			peer := newH1DeadlinePeer(t, 0, http.StatusSwitchingProtocols)
			peer.customBlackhole = true
			dialer, first := newH1ProbeDeadlineTestDialer(t, peer, item.native, nil, nil)
			ctx := t.Context()
			if item.outer != 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, item.outer)
				defer cancel()
			}
			result := make(chan h1ProbeDeadlineTestResult, 1)
			started := time.Now()
			go func() {
				connection, err := DialH1Messages(ctx, h1DeadlineAddress, nil, dialer, H1FramerXlProtocol, 65535, true, nil)
				result <- h1ProbeDeadlineTestResult{connection: connection, err: err}
			}()
			<-first.readStarted
			// Fake time reaches the actual production timer. Wait joins its
			// callback before the gated physical read can observe an outcome.
			time.Sleep(item.probe)
			synctest.Wait()
			prematureCloses := first.closes.Load()
			first.release()
			observed := <-result
			if observed.connection != nil {
				defer observed.connection.Close()
			}
			attempts := peer.snapshot()
			if prematureCloses != 0 || observed.err != nil || observed.connection == nil || time.Since(started) != item.probe || len(attempts) != 2 || attempts[1].upgrade != "websocket" || attempts[1].started.Sub(started) != item.probe || attempts[1].budget != item.total-item.probe {
				t.Fatal("private timer erased fallback or its remaining budget", item, prematureCloses, time.Since(started), observed.err, attempts)
			}
			if first.closes.Load() != 1 || !FramedUpgradePermitted(h1DeadlineAddress, H1FramerXlProtocol) {
				t.Fatal("expired probe leaked a socket or poisoned protocol capability")
			}
			if err := observed.connection.Close(); err != nil {
				t.Fatal(err)
			}
			assertH1DeadlineSocketsClosed(t, peer)
		})
	}
}

// An unavailable fallback consumes the remainder of the original total; it
// cannot end at the probe boundary or mint another complete handshake budget.
func TestDialH1MessagesProbeDeadlinePreservesWholeBudget(t *testing.T) {
	for _, item := range []struct {
		native time.Duration
		outer  time.Duration
		probe  time.Duration
		total  time.Duration
	}{
		{native: 5 * time.Second, probe: 2500 * time.Millisecond, total: 5 * time.Second},
		{native: 30 * time.Second, probe: 5 * time.Second, total: 30 * time.Second},
		{native: 30 * time.Second, outer: 4 * time.Second, probe: 2 * time.Second, total: 4 * time.Second},
	} {
		synctest.Test(t, func(t *testing.T) {
			resetH1UpgradeTestState(t)
			peer := newH1DeadlinePeer(t, 0, http.StatusSwitchingProtocols)
			peer.customBlackhole, peer.webSocketBlackhole = true, true
			dialer, first := newH1ProbeDeadlineTestDialer(t, peer, item.native, nil, nil)
			ctx := t.Context()
			if item.outer != 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, item.outer)
				defer cancel()
			}
			result := make(chan h1ProbeDeadlineTestResult, 1)
			started := time.Now()
			go func() {
				connection, err := DialH1Messages(ctx, h1DeadlineAddress, nil, dialer, H1FramerXlProtocol, 65535, true, nil)
				result <- h1ProbeDeadlineTestResult{connection: connection, err: err}
			}()
			<-first.readStarted
			time.Sleep(item.probe)
			synctest.Wait()
			first.release()
			observed := <-result
			if observed.connection != nil {
				observed.connection.Close()
			}
			attempts := peer.snapshot()
			if observed.connection != nil || !errors.Is(observed.err, context.DeadlineExceeded) || time.Since(started) != item.total || len(attempts) != 2 || attempts[1].started.Sub(started) != item.probe || attempts[1].budget != item.total-item.probe || first.closes.Load() != 1 {
				t.Fatal("probe/fallback lost the original finite total", item, time.Since(started), observed.err, attempts)
			}
			if !FramedUpgradePermitted(h1DeadlineAddress, H1FramerXlProtocol) {
				t.Fatal("owned timeout became cached protocol rejection")
			}
			assertH1DeadlineSocketsClosed(t, peer)
		})
	}
}

// An expired probe cannot turn an independent read or close failure into
// transport permission. Unproven closed-pipe leaves stay hard as well.
func TestDialH1MessagesProbeDeadlineRetainsHardReadAndCloseCauses(t *testing.T) {
	hard := errors.New("synthetic independent probe integrity failure")
	for _, item := range []struct {
		read  error
		close error
	}{
		{read: hard},
		{read: errors.Join(io.ErrUnexpectedEOF, hard)},
		{read: &os.PathError{Op: "read", Path: "synthetic-probe-custody", Err: context.DeadlineExceeded}},
		{read: io.ErrClosedPipe},
		{close: hard},
		{close: &os.LinkError{Op: "rename", Old: "synthetic-before", New: "synthetic-after", Err: context.DeadlineExceeded}},
	} {
		synctest.Test(t, func(t *testing.T) {
			resetH1UpgradeTestState(t)
			peer := newH1DeadlinePeer(t, 0, http.StatusSwitchingProtocols)
			peer.customBlackhole = true
			dialer, first := newH1ProbeDeadlineTestDialer(t, peer, 30*time.Second, item.read, item.close)
			result := make(chan h1ProbeDeadlineTestResult, 1)
			started := time.Now()
			go func() {
				connection, err := DialH1Messages(t.Context(), h1DeadlineAddress, nil, dialer, H1FramerXlProtocol, 65535, true, nil)
				result <- h1ProbeDeadlineTestResult{connection: connection, err: err}
			}()
			<-first.readStarted
			time.Sleep(5 * time.Second)
			synctest.Wait()
			first.release()
			observed := <-result
			if observed.connection != nil {
				observed.connection.Close()
			}
			if observed.connection != nil || !errors.Is(observed.err, context.DeadlineExceeded) || HTTPUpgradeAllowsFallback(observed.err) || len(peer.snapshot()) != 1 || time.Since(started) != 5*time.Second || first.closes.Load() != 1 {
				t.Fatal("private deadline erased an independent hard cause", item, observed.err)
			}
			if item.read != nil && !errors.Is(observed.err, item.read) || item.close != nil && !errors.Is(observed.err, item.close) {
				t.Fatal("probe lost the exact original read or close cause", item, observed.err)
			}
			if !FramedUpgradePermitted(h1DeadlineAddress, H1FramerXlProtocol) {
				t.Fatal("independent hard cause became protocol capability evidence")
			}
			assertH1DeadlineSocketsClosed(t, peer)
		})
	}
}

// Manual cancellation still closes immediately, joins its callback, retains
// every original cause and ends without spending the private probe timeout.
func TestDialH1MessagesProbeReadCancellationPreservesCallerCause(t *testing.T) {
	for _, independent := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			resetH1UpgradeTestState(t)
			peer := newH1DeadlinePeer(t, 0, http.StatusSwitchingProtocols)
			peer.customBlackhole = true
			var readCause, closeCause error
			if independent {
				readCause, closeCause = errors.New("synthetic canceled read failure"), errors.New("synthetic canceled close failure")
			}
			dialer, first := newH1ProbeDeadlineTestDialer(t, peer, 30*time.Second, readCause, closeCause)
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			callerCause := errors.New("synthetic caller stopped negotiation")
			result := make(chan h1ProbeDeadlineTestResult, 1)
			started := time.Now()
			go func() {
				connection, err := DialH1Messages(ctx, h1DeadlineAddress, nil, dialer, H1FramerXlProtocol, 65535, true, nil)
				result <- h1ProbeDeadlineTestResult{connection: connection, err: err}
			}()
			<-first.readStarted
			cancel(callerCause)
			synctest.Wait()
			closesBeforeRelease := first.closes.Load()
			first.release()
			observed := <-result
			if observed.connection != nil {
				observed.connection.Close()
			}
			if observed.connection != nil || !errors.Is(observed.err, context.Canceled) || !errors.Is(observed.err, callerCause) || HTTPUpgradeAllowsFallback(observed.err) || time.Since(started) != 0 || len(peer.snapshot()) != 1 || closesBeforeRelease != 1 || first.closes.Load() != 1 {
				t.Fatal("manual cancellation lost its physical close or caller cause", independent, observed.err)
			}
			if independent && (!errors.Is(observed.err, readCause) || !errors.Is(observed.err, closeCause)) {
				t.Fatal("cancellation erased independent read/close causes", observed.err)
			}
			assertH1DeadlineSocketsClosed(t, peer)
		})
	}
}
