// What the gossip package logs of a connection names no client: not the address the
// extender's carrier took it from, and not the text of an error its socket returned, which
// names both ends (EXTENDER.md G2). Each case is a real client on a real carrier, with
// verbose logging on.

package gossip

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/extender"
)

// Every line the package logs during a test, verbose ones included. Safe for concurrent
// use: the listener and the feed server log from the extender's connection goroutines.
type testGossipLog struct {
	stateLock sync.Mutex
	lines     []string
}

func (self *testGossipLog) add(line string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.lines = append(self.lines, line)
}

func (self *testGossipLog) Info(args ...any) {
	self.add(fmt.Sprint(args...))
}

func (self *testGossipLog) Infof(format string, args ...any) {
	self.add(fmt.Sprintf(format, args...))
}

func (self *testGossipLog) Warningf(format string, args ...any) {
	self.add(fmt.Sprintf(format, args...))
}

func (self *testGossipLog) Errorf(format string, args ...any) {
	self.add(fmt.Sprintf(format, args...))
}

func (self *testGossipLog) V(level int32) connect.Verbose {
	return testGossipVerbose{log: self}
}

func (self *testGossipLog) text() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return strings.Join(self.lines, "")
}

// The first line that holds the text, once the server has logged it on its own goroutine.
func (self *testGossipLog) waitFor(t *testing.T, text string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		for _, line := range func() []string {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			return append([]string{}, self.lines...)
		}() {
			if strings.Contains(line, text) {
				return line
			}
		}
		if deadline.Before(time.Now()) {
			t.Fatalf("nothing logged %q; the log:\n%s", text, self.text())
		}
		// safe as a poll: the line is logged once and stays
		time.Sleep(10 * time.Millisecond)
	}
}

// Verbose logging, always on.
type testGossipVerbose struct {
	log *testGossipLog
}

func (self testGossipVerbose) Enabled() bool {
	return true
}

func (self testGossipVerbose) Info(args ...any) {
	self.log.Info(args...)
}

func (self testGossipVerbose) Infof(format string, args ...any) {
	self.log.Infof(format, args...)
}

// Fails the test if the log holds a port a client dialed from, which is how the server
// knows the client: both ends are on the loopback here.
func testExpectLogHoldsNoClientPort(t *testing.T, gossipLog *testGossipLog, clientPorts []string) {
	t.Helper()
	if len(clientPorts) == 0 {
		t.Fatal("no client dialed")
	}
	text := gossipLog.text()
	for _, port := range clientPorts {
		if strings.Contains(text, ":"+port) {
			t.Fatalf("the log names the client's port %s:\n%s", port, text)
		}
	}
}

// Settings that dial the extender's documentation address on the loopback, and a read of
// the local ports each dial came from.
func newTestClientPortConnectSettings(
	t *testing.T,
	ip netip.Addr,
	loopbackAddr string,
) (*connect.ConnectSettings, func() []string) {
	t.Helper()
	connectSettings := newTestLoopbackConnectSettings(t, ip, loopbackAddr)
	dialContext := connectSettings.DialContextSettings.DialContext
	var stateLock sync.Mutex
	ports := []string{}
	connectSettings.DialContextSettings = &connect.DialContextSettings{
		DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			conn, err := dialContext(ctx, network, address)
			if err == nil {
				_, port, splitErr := net.SplitHostPort(conn.LocalAddr().String())
				if splitErr != nil {
					conn.Close()
					return nil, splitErr
				}
				stateLock.Lock()
				defer stateLock.Unlock()
				ports = append(ports, port)
			}
			return conn, err
		},
	}
	return connectSettings, func() []string {
		stateLock.Lock()
		defer stateLock.Unlock()
		return append([]string{}, ports...)
	}
}

// An extender on the loopback with the given services, and a dial of one of its services.
func newTestServiceExtender(
	t *testing.T,
	ctx context.Context,
	configure func(settings *extender.ExtenderSettings),
) (func(service uint32) net.Conn, func() []string) {
	t.Helper()
	key := newTestKey(t)
	tcpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tcpPort := tcpListener.Addr().(*net.TCPAddr).Port
	newTestExtenderServer(t, ctx, key, tcpListener, tcpPort, configure)
	extenderIp := netip.MustParseAddr("192.0.2.52")
	connectSettings, clientPorts := newTestClientPortConnectSettings(t, extenderIp, tcpListener.Addr().String())
	dial := func(service uint32) net.Conn {
		t.Helper()
		dialCtx, dialCancel := context.WithTimeout(ctx, 30*time.Second)
		defer dialCancel()
		conn, _, err := connect.DialExtender(
			dialCtx,
			connectSettings,
			&connect.ExtenderConfig{
				Profile: connect.ExtenderProfile{
					ConnectMode: connect.ExtenderConnectModeTcpTls,
					ServerName:  extenderIp.String(),
					Port:        tcpPort,
				},
				Ip:        extenderIp,
				PublicKey: key.publicKey,
			},
			&connect.ExtenderDial{Service: service},
		)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		return conn
	}
	return dial, clientPorts
}

// Reads until the extender ends the stream.
func testReadToTheEnd(t *testing.T, conn net.Conn) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(20 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, conn); err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			t.Fatal("the extender did not end the stream")
		}
	}
}

// A gossip connection that finds the accept queue full is refused, and what is logged of it
// says so without saying who it was.
func TestGossipAcceptQueueFullLogsNoClientAddress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	gossipLog := &testGossipLog{}
	listenerSettings := DefaultInProcessListenerSettings()
	listenerSettings.Log = gossipLog
	listenerSettings.AcceptQueueCount = 1
	listener := NewInProcessListener(ctx, listenerSettings)
	t.Cleanup(listener.Close)
	dial, clientPorts := newTestServiceExtender(t, ctx, func(settings *extender.ExtenderSettings) {
		settings.GossipConnHandler = listener.Handle
	})

	// the first waits in the queue, which nothing accepts from
	dial(connect.ExtenderServiceGossip)
	deadline := time.Now().Add(10 * time.Second)
	for len(listener.conns) < 1 {
		if deadline.Before(time.Now()) {
			t.Fatal("the first connection never reached the queue")
		}
		// safe as a poll: the connection stays queued until the listener closes
		time.Sleep(10 * time.Millisecond)
	}
	// the second finds it full, and the extender ends its stream when it is refused
	testReadToTheEnd(t, dial(connect.ExtenderServiceGossip))

	gossipLog.waitFor(t, "accept queue is full")
	testExpectLogHoldsNoClientPort(t, gossipLog, clientPorts())
}

// A feed client that never sends its request is let go at the request timeout, and what is
// logged of it says it timed out without saying who it was. The read that timed out is of the
// carrier's socket, whose error names both ends.
func TestGossipFeedRequestErrorLogsNoClientAddress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	gossipLog := &testGossipLog{}
	feedSettings := DefaultFeedServerSettings()
	feedSettings.Log = gossipLog
	feedSettings.RequestTimeout = 300 * time.Millisecond
	feed := NewFeedServer(ctx, newTestDirectory(t, newTestKey(t)), newTestKey(t).publicKey, feedSettings)
	t.Cleanup(feed.Close)
	dial, clientPorts := newTestServiceExtender(t, ctx, func(settings *extender.ExtenderSettings) {
		settings.FeedConnHandler = feed.Serve
	})

	testReadToTheEnd(t, dial(connect.ExtenderServiceFeed))

	line := gossipLog.waitFor(t, "[gossip]feed request err")
	testExpectLogHoldsNoClientPort(t, gossipLog, clientPorts())
	if !strings.Contains(line, "the request did not arrive in time") {
		t.Fatalf("the line = %q, expected the timeout", line)
	}
}
