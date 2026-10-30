// The relays keep their timeouts on one watchdog timer per direction (CPUOPT2
// §9.2 finding 1): no chunk installs a deadline, and the timer is armed about
// once per timeout however many chunks pass. extender_relay_exact_timeout_test.go
// checks that the bounds themselves are unchanged. Every connection here is an
// in-memory pipe on the synctest clock, so each count and each instant is
// exact.
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

// Counts the deadlines a relay installs on its side of a pipe.
type relayCountingConn struct {
	net.Conn
	stateLock          sync.Mutex
	deadlineCount      int
	readDeadlineCount  int
	writeDeadlineCount int
}

func (self *relayCountingConn) SetDeadline(t time.Time) error {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.deadlineCount += 1
	}()
	return self.Conn.SetDeadline(t)
}

func (self *relayCountingConn) SetReadDeadline(t time.Time) error {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.readDeadlineCount += 1
	}()
	return self.Conn.SetReadDeadline(t)
}

func (self *relayCountingConn) SetWriteDeadline(t time.Time) error {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.writeDeadlineCount += 1
	}()
	return self.Conn.SetWriteDeadline(t)
}

// The read and the write deadlines installed so far.
func (self *relayCountingConn) counts() (int, int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.readDeadlineCount, self.writeDeadlineCount
}

// 64 chunks each way install no read or write deadline at all. Every chunk
// installed one of each before CPUOPT2, 65 reads and 64 writes per side, and
// the lazy re-arm that followed installed up to 2 per side. The one
// SetDeadline per side, which this does not count, clears whatever the setup
// left.
func TestExtenderRelayInstallsDeadlinesPerBurstNotPerChunk(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clientConn, clientPeer := net.Pipe()
		forwardConn, forwardPeer := net.Pipe()
		defer clientPeer.Close()
		defer forwardPeer.Close()
		countedClientConn := &relayCountingConn{Conn: clientConn}
		countedForwardConn := &relayCountingConn{Conn: forwardConn}
		server := &ExtenderServer{settings: DefaultExtenderSettings()}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		relayDone := make(chan struct{})
		go func() {
			defer close(relayDone)
			server.relay(ctx, cancel, countedClientConn, countedForwardConn)
		}()

		const roundCount = 64
		relayTestRounds(t, roundCount, time.Millisecond, clientPeer, forwardPeer, 4096)
		synctest.Wait()
		clientReadCount, clientWriteCount := countedClientConn.counts()
		forwardReadCount, forwardWriteCount := countedForwardConn.counts()
		cancel()
		<-relayDone

		for _, c := range []struct {
			name  string
			count int
		}{
			{name: "client read", count: clientReadCount},
			{name: "client write", count: clientWriteCount},
			{name: "forward read", count: forwardReadCount},
			{name: "forward write", count: forwardWriteCount},
		} {
			if c.count != 0 {
				t.Errorf("%s deadlines installed %d times for %d chunks, expected none", c.name, c.count, roundCount)
			}
		}
	})
}

// The same for the camouflage splice, whose writes also retry short writes.
func TestExtenderCamouflageSpliceInstallsDeadlinesPerBurstNotPerChunk(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clientConn, clientPeer := net.Pipe()
		siteConn, sitePeer := net.Pipe()
		defer clientPeer.Close()
		defer sitePeer.Close()
		countedClientConn := &relayCountingConn{Conn: clientConn}
		countedSiteConn := &relayCountingConn{Conn: siteConn}
		settings := DefaultExtenderSettings()
		server := &ExtenderServer{settings: settings}
		camouflage, err := newExtenderCamouflage(server, settings)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		initialBytes := []byte("synthetic initial bytes")
		spliceDone := make(chan struct{})
		go func() {
			defer close(spliceDone)
			camouflage.relaySplice(ctx, countedClientConn, countedSiteConn, initialBytes)
		}()
		if _, err := io.ReadFull(sitePeer, make([]byte, len(initialBytes))); err != nil {
			t.Fatalf("initial bytes: %v", err)
		}

		const roundCount = 64
		relayTestRounds(t, roundCount, time.Millisecond, clientPeer, sitePeer, extenderCamouflageSpliceBufferByteCount)
		synctest.Wait()
		clientReadCount, clientWriteCount := countedClientConn.counts()
		siteReadCount, siteWriteCount := countedSiteConn.counts()
		cancel()
		<-spliceDone

		for _, c := range []struct {
			name  string
			count int
		}{
			{name: "client read", count: clientReadCount},
			{name: "client write", count: clientWriteCount},
			{name: "site read", count: siteReadCount},
			{name: "site write", count: siteWriteCount},
		} {
			if c.count != 0 {
				t.Errorf("%s deadlines installed %d times for %d chunks, expected none", c.name, c.count, roundCount)
			}
		}
	})
}

// 64 chunks each way, one a second, arm each direction's watchdog timer three
// times: when it starts, and when it falls due at about 30 s and 60 s to find
// a later operation in progress. That is 6 timer operations for 256 reads and
// writes, where every chunk installed a read and a write deadline before.
func TestExtenderRelayWatchdogArmsPerTimeoutNotPerChunk(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var armCount atomic.Int64
		settings := DefaultExtenderSettings()
		settings.relayWatchdogArmForTest = func() {
			armCount.Add(1)
		}
		clientConn, clientPeer := net.Pipe()
		forwardConn, forwardPeer := net.Pipe()
		defer clientPeer.Close()
		defer forwardPeer.Close()
		server := &ExtenderServer{settings: settings}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		relayDone := make(chan struct{})
		go func() {
			defer close(relayDone)
			server.relay(ctx, cancel, clientConn, forwardConn)
		}()

		const roundCount = 64
		relayTestRounds(t, roundCount, time.Second, clientPeer, forwardPeer, 4096)
		synctest.Wait()
		count := armCount.Load()
		cancel()
		<-relayDone

		if count != 6 {
			t.Fatalf("the watchdog timers were armed %d times for %d chunks each way over %s, expected 6", count, roundCount, roundCount*time.Second)
		}
	})
}

// A non-positive timeout leaves its operations unbounded, and with neither
// timeout positive there is no timer at all. The other timeout still ends its
// operation exactly.
func TestExtenderRelayWatchdogLeavesANonPositiveTimeoutUnbounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var armCount atomic.Int64
		armForTest := func() {
			armCount.Add(1)
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		unbounded := newRelayWatchdog(0, -time.Second, cancel, armForTest)
		defer unbounded.stop()
		unbounded.beginRead()
		time.Sleep(time.Hour)
		unbounded.beginWrite()
		time.Sleep(time.Hour)
		synctest.Wait()
		if ctx.Err() != nil || armCount.Load() != 0 {
			t.Fatalf("without a positive timeout: canceled %v, %d timer arms, expected neither", ctx.Err(), armCount.Load())
		}

		writeOnly := newRelayWatchdog(0, 30*time.Second, cancel, armForTest)
		defer writeOnly.stop()
		writeOnly.beginRead()
		time.Sleep(time.Hour)
		synctest.Wait()
		if ctx.Err() != nil {
			t.Fatalf("an unbounded read was canceled after %s", time.Hour)
		}
		writeOnly.beginWrite()
		time.Sleep(30*time.Second - time.Nanosecond)
		synctest.Wait()
		if ctx.Err() != nil {
			t.Fatal("a write was canceled before its 30 s timeout")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if ctx.Err() == nil {
			t.Fatal("a write outlived its 30 s timeout")
		}
	})
}

// Once stopped, the watchdog never ends the relay, however long the operation
// it last saw runs.
func TestExtenderRelayWatchdogStopsForGood(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		watchdog := newRelayWatchdog(30*time.Second, 30*time.Second, cancel, nil)
		watchdog.beginWrite()
		time.Sleep(20 * time.Second)
		watchdog.stop()
		time.Sleep(time.Hour)
		synctest.Wait()
		if ctx.Err() != nil {
			t.Fatal("a stopped watchdog ended the relay")
		}
	})
}
