// A relay's reads and writes keep the bounds they had when every chunk
// installed its own deadline: a read that waits ReadTimeout, or a write that
// waits WriteTimeout, ends the relay exactly that long after it started, and
// the camouflage splice does the same with its idle timeout. Neither ends
// earlier, and neither later, however recently an earlier chunk passed. Every
// connection here is an in-memory pipe on the synctest clock, so each instant
// is exact.
package extender

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"
	"testing/synctest"
	"time"
)

// Sends one chunk from fromPeer across a running relay and reads it whole at
// toPeer. On a pipe one write of a relay buffer is exactly one relay read.
func relayTestChunk(t *testing.T, fromPeer net.Conn, toPeer net.Conn, chunkByteCount int, fill byte) {
	t.Helper()
	chunk := bytes.Repeat([]byte{fill}, chunkByteCount)
	if _, err := fromPeer.Write(chunk); err != nil {
		t.Fatalf("chunk write: %v", err)
	}
	received := make([]byte, chunkByteCount)
	if _, err := io.ReadFull(toPeer, received); err != nil || !bytes.Equal(received, chunk) {
		t.Fatalf("chunk read: %v", err)
	}
}

// Sends one chunk each way across a running relay per round, each round after
// the interval.
func relayTestRounds(
	t *testing.T,
	roundCount int,
	interval time.Duration,
	clientPeer net.Conn,
	remotePeer net.Conn,
	chunkByteCount int,
) {
	t.Helper()
	for range roundCount {
		time.Sleep(interval)
		relayTestChunk(t, clientPeer, remotePeer, chunkByteCount, 0x11)
		relayTestChunk(t, remotePeer, clientPeer, chunkByteCount, 0x22)
	}
}

// Runs a relay on its own goroutine. The channel receives how long after start
// it returned.
func relayTestRun(start time.Time, relay func()) <-chan time.Duration {
	ended := make(chan time.Duration, 1)
	go func() {
		relay()
		ended <- time.Since(start)
	}()
	return ended
}

// Checks that a relay is still running a nanosecond before end, after start,
// and has ended at end exactly.
func expectRelayTestEnd(t *testing.T, start time.Time, end time.Duration, ended <-chan time.Duration) {
	t.Helper()
	time.Sleep(time.Until(start.Add(end - time.Nanosecond)))
	synctest.Wait()
	select {
	case endedAt := <-ended:
		t.Fatalf("the relay ended at %s, expected %s", endedAt, end)
	default:
	}
	time.Sleep(time.Nanosecond)
	synctest.Wait()
	select {
	case endedAt := <-ended:
		if endedAt != end {
			t.Fatalf("the relay ended at %s, expected %s", endedAt, end)
		}
	default:
		t.Fatalf("the relay was still running at %s, expected it to end then", end)
	}
}

// A read ends the relay exactly ReadTimeout after it started. Each case relays
// one chunk each way at the given instants and then stops, so both directions
// wait in a read that started with the last chunk.
func TestExtenderRelayEndsExactlyReadTimeoutAfterTheLastReadStarts(t *testing.T) {
	settings := DefaultExtenderSettings()
	if settings.ReadTimeout != 30*time.Second || settings.WriteTimeout != 30*time.Second {
		t.Fatalf("the cases below assume 30 s timeouts, got read %s and write %s", settings.ReadTimeout, settings.WriteTimeout)
	}
	cases := []struct {
		name       string
		chunkTimes []time.Duration
		// after the start of the relay
		expectedEnd time.Duration
	}{
		{name: "idle from the start", chunkTimes: nil, expectedEnd: 30 * time.Second},
		// the last read started with 10 s of the first one's timeout left
		{name: "idle after a chunk at 20 s", chunkTimes: []time.Duration{0, 20 * time.Second}, expectedEnd: 50 * time.Second},
		{name: "idle after a chunk at 30 s", chunkTimes: []time.Duration{0, 20 * time.Second, 30 * time.Second}, expectedEnd: 60 * time.Second},
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
			start := time.Now()
			ended := relayTestRun(start, func() {
				server.relay(ctx, cancel, clientConn, forwardConn)
			})

			for _, chunkTime := range c.chunkTimes {
				time.Sleep(time.Until(start.Add(chunkTime)))
				relayTestRounds(t, 1, 0, clientPeer, forwardPeer, 4096)
			}
			t.Log(c.name)
			expectRelayTestEnd(t, start, c.expectedEnd, ended)
		})
	}
}

// A write that the destination never takes ends the relay exactly
// WriteTimeout after it started, though an earlier write passed 20 s before.
// Reads are bounded far later here, so only the write can end the relay.
func TestExtenderRelayEndsExactlyWriteTimeoutAfterAStalledWriteStarts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultExtenderSettings()
		settings.ReadTimeout = time.Hour
		if settings.WriteTimeout != 30*time.Second {
			t.Fatalf("the test assumes a 30 s write timeout, got %s", settings.WriteTimeout)
		}
		clientConn, clientPeer := net.Pipe()
		forwardConn, forwardPeer := net.Pipe()
		defer clientPeer.Close()
		defer forwardPeer.Close()
		server := &ExtenderServer{settings: settings}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		start := time.Now()
		ended := relayTestRun(start, func() {
			server.relay(ctx, cancel, clientConn, forwardConn)
		})

		relayTestChunk(t, clientPeer, forwardPeer, 4096, 0x11)
		time.Sleep(20 * time.Second)
		// the relay reads this chunk and blocks writing it
		if _, err := clientPeer.Write(bytes.Repeat([]byte{0x11}, 4096)); err != nil {
			t.Fatal(err)
		}
		expectRelayTestEnd(t, start, 50*time.Second, ended)
	})
}

// A write timeout shorter than the read timeout is kept just as exactly: the
// write that stalls at 4 s, while its direction last read at 3 s and waited in
// a 30 s read before that, ends the relay at 9 s.
func TestExtenderRelayEndsExactlyAtAShorterWriteTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultExtenderSettings()
		settings.WriteTimeout = 5 * time.Second
		if settings.ReadTimeout != 30*time.Second {
			t.Fatalf("the test assumes a 30 s read timeout, got %s", settings.ReadTimeout)
		}
		clientConn, clientPeer := net.Pipe()
		forwardConn, forwardPeer := net.Pipe()
		defer clientPeer.Close()
		defer forwardPeer.Close()
		server := &ExtenderServer{settings: settings}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		start := time.Now()
		ended := relayTestRun(start, func() {
			server.relay(ctx, cancel, clientConn, forwardConn)
		})

		relayTestChunk(t, clientPeer, forwardPeer, 4096, 0x11)
		time.Sleep(3 * time.Second)
		relayTestChunk(t, clientPeer, forwardPeer, 4096, 0x11)
		time.Sleep(time.Second)
		if _, err := clientPeer.Write(bytes.Repeat([]byte{0x11}, 4096)); err != nil {
			t.Fatal(err)
		}
		expectRelayTestEnd(t, start, 9*time.Second, ended)
	})
}

// Starts a splice between two pipes and reads its hello at the site. Returns
// the client and site peers, the instant the splice started, and its end.
func spliceTest(t *testing.T, ctx context.Context, settings *ExtenderSettings) (net.Conn, net.Conn, time.Time, <-chan time.Duration) {
	t.Helper()
	clientConn, clientPeer := net.Pipe()
	siteConn, sitePeer := net.Pipe()
	t.Cleanup(func() {
		clientPeer.Close()
		sitePeer.Close()
	})
	server := &ExtenderServer{settings: settings}
	camouflage, err := newExtenderCamouflage(server, settings)
	if err != nil {
		t.Fatal(err)
	}
	initialBytes := []byte("synthetic initial bytes")
	start := time.Now()
	ended := relayTestRun(start, func() {
		camouflage.relaySplice(ctx, clientConn, siteConn, initialBytes)
	})
	receivedInitialBytes := make([]byte, len(initialBytes))
	if _, err := io.ReadFull(sitePeer, receivedInitialBytes); err != nil || !bytes.Equal(receivedInitialBytes, initialBytes) {
		t.Fatalf("initial bytes: %v", err)
	}
	return clientPeer, sitePeer, start, ended
}

// The splice's idle timeout bounds each read exactly as the relay's
// ReadTimeout does.
func TestExtenderCamouflageSpliceEndsExactlyTheIdleTimeoutAfterTheLastReadStarts(t *testing.T) {
	settings := DefaultExtenderSettings()
	if settings.ProxyIdleTimeout != 30*time.Second {
		t.Fatalf("the cases below assume a 30 s idle timeout, got %s", settings.ProxyIdleTimeout)
	}
	cases := []struct {
		name       string
		chunkTimes []time.Duration
		// after the start of the splice
		expectedEnd time.Duration
	}{
		{name: "idle from the hello", chunkTimes: nil, expectedEnd: 30 * time.Second},
		{name: "idle after a chunk at 20 s", chunkTimes: []time.Duration{0, 20 * time.Second}, expectedEnd: 50 * time.Second},
		{name: "idle after a chunk at 30 s", chunkTimes: []time.Duration{0, 20 * time.Second, 30 * time.Second}, expectedEnd: 60 * time.Second},
	}
	for _, c := range cases {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			clientPeer, sitePeer, start, ended := spliceTest(t, ctx, settings)
			for _, chunkTime := range c.chunkTimes {
				time.Sleep(time.Until(start.Add(chunkTime)))
				relayTestRounds(t, 1, 0, clientPeer, sitePeer, extenderCamouflageSpliceBufferByteCount)
			}
			t.Log(c.name)
			expectRelayTestEnd(t, start, c.expectedEnd, ended)
		})
	}
}

// A write to a site that stops reading ends the splice exactly the idle
// timeout after it started, though the hello and a chunk passed 20 s before.
// The site keeps sending every 10 s, so the other direction never idles.
func TestExtenderCamouflageSpliceEndsExactlyTheIdleTimeoutAfterAStalledWriteStarts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultExtenderSettings()
		if settings.ProxyIdleTimeout != 30*time.Second {
			t.Fatalf("the test assumes a 30 s idle timeout, got %s", settings.ProxyIdleTimeout)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		clientPeer, sitePeer, start, ended := spliceTest(t, ctx, settings)

		relayTestChunk(t, clientPeer, sitePeer, extenderCamouflageSpliceBufferByteCount, 0x11)
		for _, at := range []time.Duration{10 * time.Second, 20 * time.Second, 30 * time.Second, 40 * time.Second} {
			time.Sleep(time.Until(start.Add(at)))
			relayTestChunk(t, sitePeer, clientPeer, extenderCamouflageSpliceBufferByteCount, 0x22)
			if at == 20*time.Second {
				// the splice reads this chunk and blocks writing it to the site
				if _, err := clientPeer.Write(bytes.Repeat([]byte{0x11}, extenderCamouflageSpliceBufferByteCount)); err != nil {
					t.Fatal(err)
				}
			}
		}
		expectRelayTestEnd(t, start, 50*time.Second, ended)
	})
}

// A site that never reads the hello ends the splice exactly the idle timeout
// after the hello write started.
func TestExtenderCamouflageSpliceEndsExactlyTheIdleTimeoutAfterAStalledHello(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultExtenderSettings()
		clientConn, clientPeer := net.Pipe()
		siteConn, sitePeer := net.Pipe()
		defer clientPeer.Close()
		defer sitePeer.Close()
		server := &ExtenderServer{settings: settings}
		camouflage, err := newExtenderCamouflage(server, settings)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		start := time.Now()
		ended := relayTestRun(start, func() {
			camouflage.relaySplice(ctx, clientConn, siteConn, []byte("synthetic initial bytes"))
		})
		expectRelayTestEnd(t, start, settings.ProxyIdleTimeout, ended)
	})
}
