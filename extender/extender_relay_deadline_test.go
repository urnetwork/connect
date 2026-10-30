// The relays re-arm their deadlines lazily (CPUOPT2 §9.2 finding 1): a busy
// direction installs a deadline about once per slack instead of once per 4 KiB
// chunk, and a stalled direction still ends no earlier than its timeout after
// its last operation started. Every connection here is an in-memory pipe on
// the synctest clock, so each count and each instant is exact.
package extender

import (
	"bytes"
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// Counts the deadlines a relay installs on its side of a pipe.
type relayDeadlineCountingConn struct {
	net.Conn
	stateLock          sync.Mutex
	deadlineCount      int
	readDeadlineCount  int
	writeDeadlineCount int
}

func (self *relayDeadlineCountingConn) SetDeadline(t time.Time) error {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.deadlineCount += 1
	}()
	return self.Conn.SetDeadline(t)
}

func (self *relayDeadlineCountingConn) SetReadDeadline(t time.Time) error {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.readDeadlineCount += 1
	}()
	return self.Conn.SetReadDeadline(t)
}

func (self *relayDeadlineCountingConn) SetWriteDeadline(t time.Time) error {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.writeDeadlineCount += 1
	}()
	return self.Conn.SetWriteDeadline(t)
}

// The read and the write deadlines installed so far.
func (self *relayDeadlineCountingConn) counts() (int, int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.readDeadlineCount, self.writeDeadlineCount
}

// Sends one chunk each way across a running relay per round, each round
// after the interval, and checks that each arrives whole. On a pipe a 4 KiB
// write is exactly one 4 KiB read, so every round is one relayed chunk in each
// direction.
func relayDeadlineTestRounds(
	t *testing.T,
	roundCount int,
	interval time.Duration,
	clientPeer net.Conn,
	remotePeer net.Conn,
	chunkByteCount int,
) {
	t.Helper()
	upChunk := bytes.Repeat([]byte{0x11}, chunkByteCount)
	downChunk := bytes.Repeat([]byte{0x22}, chunkByteCount)
	received := make([]byte, chunkByteCount)
	for round := range roundCount {
		time.Sleep(interval)
		if _, err := clientPeer.Write(upChunk); err != nil {
			t.Fatalf("round %d up write: %v", round, err)
		}
		if _, err := io.ReadFull(remotePeer, received); err != nil || !bytes.Equal(received, upChunk) {
			t.Fatalf("round %d up read: %v", round, err)
		}
		if _, err := remotePeer.Write(downChunk); err != nil {
			t.Fatalf("round %d down write: %v", round, err)
		}
		if _, err := io.ReadFull(clientPeer, received); err != nil || !bytes.Equal(received, downChunk) {
			t.Fatalf("round %d down read: %v", round, err)
		}
	}
}

// 64 chunks each way install each deadline at most twice: the first operation
// arms the timeout alone and the second re-arms it with the slack, which 64 ms
// of chunks never use up. Before, every chunk installed a read and a write
// deadline, each a timer change in the runtime poller.
func TestExtenderRelayInstallsDeadlinesPerBurstNotPerChunk(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clientConn, clientPeer := net.Pipe()
		forwardConn, forwardPeer := net.Pipe()
		defer clientPeer.Close()
		defer forwardPeer.Close()
		countedClientConn := &relayDeadlineCountingConn{Conn: clientConn}
		countedForwardConn := &relayDeadlineCountingConn{Conn: forwardConn}
		server := &ExtenderServer{settings: DefaultExtenderSettings()}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		relayDone := make(chan struct{})
		go func() {
			defer close(relayDone)
			server.relay(ctx, cancel, countedClientConn, countedForwardConn)
		}()

		const roundCount = 64
		relayDeadlineTestRounds(t, roundCount, time.Millisecond, clientPeer, forwardPeer, 4096)
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
			if 2 < c.count {
				t.Errorf("%s deadlines installed %d times for %d chunks, expected at most 2", c.name, c.count, roundCount)
			}
		}
	})
}

// The same for the camouflage splice, whose writes also retry short writes.
// The client side's one SetDeadline is the splice clearing the hello budget.
func TestExtenderCamouflageSpliceInstallsDeadlinesPerBurstNotPerChunk(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clientConn, clientPeer := net.Pipe()
		siteConn, sitePeer := net.Pipe()
		defer clientPeer.Close()
		defer sitePeer.Close()
		countedClientConn := &relayDeadlineCountingConn{Conn: clientConn}
		countedSiteConn := &relayDeadlineCountingConn{Conn: siteConn}
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
		receivedInitialBytes := make([]byte, len(initialBytes))
		if _, err := io.ReadFull(sitePeer, receivedInitialBytes); err != nil || !bytes.Equal(receivedInitialBytes, initialBytes) {
			t.Fatalf("initial bytes: %v", err)
		}

		const roundCount = 64
		relayDeadlineTestRounds(t, roundCount, time.Millisecond, clientPeer, sitePeer, extenderCamouflageSpliceBufferByteCount)
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
			if 2 < c.count {
				t.Errorf("%s deadlines installed %d times for %d chunks, expected at most 2", c.name, c.count, roundCount)
			}
		}
	})
}

// A stalled relay still ends no earlier than ReadTimeout after its last read
// started, as when every chunk installed its own deadline, and at most the
// slack later. Each case relays one chunk each way at the given instants and
// then stops; both directions stop together, so the relay ends when its reads
// time out.
func TestExtenderRelayStallEndsBetweenTheTimeoutAndTheSlack(t *testing.T) {
	settings := DefaultExtenderSettings()
	timeout := settings.ReadTimeout
	slack := time.Duration(float64(timeout) * settings.RelayDeadlineSlackFraction)
	if timeout != 30*time.Second || slack != 15*time.Second {
		t.Fatalf("the cases below assume a 30 s timeout and a 15 s slack, got %s and %s", timeout, slack)
	}
	cases := []struct {
		name       string
		chunkTimes []time.Duration
		// after the start of the relay
		expectedEnd time.Duration
	}{
		{
			// the first read arms the timeout alone, so an idle relay ends as
			// before
			name:        "idle from the start",
			chunkTimes:  nil,
			expectedEnd: 30 * time.Second,
		},
		{
			// 10 s were left at the last chunk, so its reads re-armed to the
			// timeout and the slack
			name:        "re-armed at the last chunk",
			chunkTimes:  []time.Duration{0, 20 * time.Second},
			expectedEnd: 65 * time.Second,
		},
		{
			// 35 s were left at the last chunk, which is at least the timeout,
			// so its reads kept the deadline
			name:        "kept at the last chunk",
			chunkTimes:  []time.Duration{0, 20 * time.Second, 30 * time.Second},
			expectedEnd: 65 * time.Second,
		},
	}
	for _, c := range cases {
		synctest.Test(t, func(t *testing.T) {
			clientConn, clientPeer := net.Pipe()
			forwardConn, forwardPeer := net.Pipe()
			defer clientPeer.Close()
			defer forwardPeer.Close()
			server := &ExtenderServer{settings: settings}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			relayDone := make(chan struct{})
			start := time.Now()
			go func() {
				defer close(relayDone)
				server.relay(ctx, cancel, clientConn, forwardConn)
			}()

			lastReadStart := time.Duration(0)
			for _, chunkTime := range c.chunkTimes {
				time.Sleep(time.Until(start.Add(chunkTime)))
				relayDeadlineTestRounds(t, 1, 0, clientPeer, forwardPeer, 4096)
				lastReadStart = time.Since(start)
			}
			if c.expectedEnd < lastReadStart+timeout || lastReadStart+timeout+slack < c.expectedEnd {
				t.Fatalf("%s: the expected end %s is outside [%s, %s]", c.name, c.expectedEnd, lastReadStart+timeout, lastReadStart+timeout+slack)
			}

			// never earlier than before: the timeout after the last read started
			time.Sleep(time.Until(start.Add(lastReadStart + timeout - time.Nanosecond)))
			synctest.Wait()
			select {
			case <-relayDone:
				t.Fatalf("%s: the relay ended %s after its last read started, before the %s timeout", c.name, time.Since(start)-lastReadStart, timeout)
			default:
			}
			time.Sleep(time.Until(start.Add(c.expectedEnd - time.Nanosecond)))
			synctest.Wait()
			select {
			case <-relayDone:
				t.Fatalf("%s: the relay ended at %s, expected %s", c.name, time.Since(start), c.expectedEnd)
			default:
			}
			time.Sleep(time.Nanosecond)
			synctest.Wait()
			select {
			case <-relayDone:
			default:
				t.Fatalf("%s: the relay outlived %s", c.name, c.expectedEnd)
			}
		})
	}
}

// The re-arm rule itself: the first deadline and one that has passed are the
// timeout alone; one with at least the timeout left is kept; one with less is
// re-armed to the timeout and the slack; and a non-positive fraction re-arms
// before every operation.
func TestExtenderRelayDeadlineRearmsOnlyWhenLessThanTheTimeoutIsLeft(t *testing.T) {
	start := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	steps := []struct {
		at               time.Duration
		expectedDeadline time.Duration
		expectedChanged  bool
	}{
		// the first operation: the timeout alone
		{at: 0, expectedDeadline: 30 * time.Second, expectedChanged: true},
		// less than the timeout left: the timeout and the slack
		{at: time.Millisecond, expectedDeadline: 45*time.Second + time.Millisecond, expectedChanged: true},
		// the timeout or more left: kept
		{at: 10 * time.Second, expectedDeadline: 45*time.Second + time.Millisecond, expectedChanged: false},
		{at: 15*time.Second + time.Millisecond, expectedDeadline: 45*time.Second + time.Millisecond, expectedChanged: false},
		{at: 15*time.Second + 2*time.Millisecond, expectedDeadline: 60*time.Second + 2*time.Millisecond, expectedChanged: true},
		// passed: the timeout alone again
		{at: 2 * time.Minute, expectedDeadline: 2*time.Minute + 30*time.Second, expectedChanged: true},
	}
	deadline := newRelayDeadline(30*time.Second, 0.5)
	for _, step := range steps {
		next, changed := deadline.next(start.Add(step.at))
		if !next.Equal(start.Add(step.expectedDeadline)) || changed != step.expectedChanged {
			t.Fatalf("at %s: deadline %s changed %t, expected %s changed %t", step.at, next.Sub(start), changed, step.expectedDeadline, step.expectedChanged)
		}
	}

	for _, slackFraction := range []float64{0, -1} {
		deadline := newRelayDeadline(30*time.Second, slackFraction)
		for i := range 4 {
			at := time.Duration(i) * time.Millisecond
			next, changed := deadline.next(start.Add(at))
			if !next.Equal(start.Add(at+30*time.Second)) || !changed {
				t.Fatalf("slack fraction %v at %s: deadline %s changed %t, expected a fresh timeout", slackFraction, at, next.Sub(start), changed)
			}
		}
	}
}
