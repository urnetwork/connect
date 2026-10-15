package extender

// The feed over websocket (lag.dev/PLAN.md M9), served by the real feed server
// of connect/gossip over a tls httptest server, as the API mounts it. Each
// connection the server accepts reports the remote address the test queued
// for it, so clients can stand at any prefix; the vantage the feed server
// partitions by is read from that address (R2). The directory's clock and
// partition secret are pinned, so a sample is the same however it is asked
// for, over the websocket or over a stream.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/gossip"
	"github.com/urnetwork/connect/protocol"
)

// The network space of the feed's records.
const testLagFeedNetworkHost = "space.example"

// The feed's partition secret, pinned so the test can compute partitions.
var testLagFeedPartitionSecret = bytes.Repeat([]byte{0x5a}, 32)

// An accepted connection that reports the remote address queued for it.
type testLagFeedListener struct {
	net.Listener
	remoteAddrs chan net.Addr
}

func (self *testLagFeedListener) Accept() (net.Conn, error) {
	conn, err := self.Listener.Accept()
	if err != nil {
		return nil, err
	}
	select {
	case remoteAddr := <-self.remoteAddrs:
		return &testLagFeedAddrConn{Conn: conn, remoteAddr: remoteAddr}, nil
	default:
		return conn, nil
	}
}

type testLagFeedAddrConn struct {
	net.Conn
	remoteAddr net.Addr
}

func (self *testLagFeedAddrConn) RemoteAddr() net.Addr {
	return self.remoteAddr
}

// A feed server over a directory of open records, mounted as the API mounts the
// lifecycle's feed handler.
type testLagFeed struct {
	t              *testing.T
	rootPrivateKey ed25519.PrivateKey
	now            time.Time
	directory      *connect.ExtenderDirectory
	feedServer     *gossip.FeedServer
	server         *ExtenderServer
	httpServer     *httptest.Server
	remoteAddrs    chan net.Addr
	// the remote address of each stream the feed server finished serving
	served chan net.Addr
	// the key of each record, hex
	keyHexes []string
}

func newTestLagFeed(t *testing.T, recordCount int, configure func(settings *ExtenderSettings)) *testLagFeed {
	t.Helper()
	rootPublicKey, rootPrivateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	feed := &testLagFeed{
		t:              t,
		rootPrivateKey: rootPrivateKey,
		now:            time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC),
		remoteAddrs:    make(chan net.Addr, 16),
		served:         make(chan net.Addr, 16),
	}

	directorySettings := connect.DefaultExtenderDirectorySettings()
	directorySettings.NetworkHosts = []string{testLagFeedNetworkHost}
	directorySettings.Now = func() time.Time {
		return feed.now
	}
	directorySettings.PartitionSecret = testLagFeedPartitionSecret
	feed.directory = connect.NewExtenderDirectory(ctx, directorySettings)
	feed.directory.SetRootKeys(connect.NewExtenderRootKeySet(rootPublicKey))
	for i := range recordCount {
		publicKey, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		feed.applyRecord(publicKey, fmt.Sprintf("198.51.%d.%d", 1+i/200, 1+i%200))
		feed.keyHexes = append(feed.keyHexes, hex.EncodeToString(publicKey))
	}
	feed.feedServer = gossip.NewFeedServer(ctx, feed.directory, nil, gossip.DefaultFeedServerSettings())

	serverSettings := DefaultExtenderSettings()
	serverSettings.FeedConnHandler = func(conn net.Conn) {
		feed.feedServer.Serve(conn)
		feed.served <- conn.RemoteAddr()
	}
	if configure != nil {
		configure(serverSettings)
	}
	feed.server = NewExtenderServer(ctx, nil, nil, nil, &net.Dialer{}, serverSettings)
	handler := newExtenderLagLifecycle(feed.server, nil).FeedHandler()
	if handler == nil {
		t.Fatal("the lifecycle mounted no feed")
	}
	mux := http.NewServeMux()
	mux.Handle("/ur/feed/v1", handler)
	feed.httpServer = httptest.NewUnstartedServer(mux)
	feed.httpServer.Listener = &testLagFeedListener{
		Listener:    feed.httpServer.Listener,
		remoteAddrs: feed.remoteAddrs,
	}
	feed.httpServer.StartTLS()
	t.Cleanup(func() {
		feed.httpServer.Close()
		feed.server.CloseAndWait()
		feed.feedServer.Close()
		feed.directory.Close()
		cancel()
	})
	return feed
}

func (self *testLagFeed) applyRecord(publicKey ed25519.PublicKey, ip string) {
	self.t.Helper()
	record, err := connect.SignExtenderRecord(self.rootPrivateKey, &protocol.ExtenderRecordBody{
		PublicKey: publicKey,
		Addresses: []*protocol.ExtenderAddress{
			{
				Ip:        ip,
				IpVersion: 4,
				Carriers:  []string{connect.ExtenderCarrierTcp},
			},
		},
		TcpPort:      443,
		CountryCode:  "us",
		IssueTimeMs:  uint64(self.now.UnixMilli()),
		ExpireTimeMs: uint64(self.now.Add(14 * 24 * time.Hour).UnixMilli()),
		NetworkHost:  testLagFeedNetworkHost,
	})
	if err != nil {
		self.t.Fatal(err)
	}
	if _, err := self.directory.ApplyRecord(record, connect.ExtenderSourceGossip); err != nil {
		self.t.Fatal(err)
	}
}

func (self *testLagFeed) revoke(publicKey []byte) {
	self.t.Helper()
	revocation, err := connect.SignExtenderRevocation(self.rootPrivateKey, &protocol.ExtenderRevocationBody{
		PublicKey:   publicKey,
		IssueTimeMs: uint64(self.now.Add(time.Second).UnixMilli()),
		NetworkHost: testLagFeedNetworkHost,
	})
	if err != nil {
		self.t.Fatal(err)
	}
	if _, err := self.directory.ApplyRevocation(revocation); err != nil {
		self.t.Fatal(err)
	}
}

// Opens the feed over a websocket from `remote`, and returns the connection
// and the upgrade's response.
func (self *testLagFeed) dial(remote string) (*websocket.Conn, *http.Response, error) {
	self.t.Helper()
	self.remoteAddrs <- net.TCPAddrFromAddrPort(netip.MustParseAddrPort(remote))
	dialer := &websocket.Dialer{
		TLSClientConfig:  self.httpServer.Client().Transport.(*http.Transport).TLSClientConfig,
		HandshakeTimeout: 10 * time.Second,
	}
	feedUrl := "wss://" + strings.TrimPrefix(self.httpServer.URL, "https://") + "/ur/feed/v1"
	return dialer.Dial(feedUrl, nil)
}

// Opens the feed from `remote` and sends the request as the browser does: one
// binary message, no length prefix.
func (self *testLagFeed) open(remote string, request *protocol.ExtenderFeedRequest) *websocket.Conn {
	self.t.Helper()
	ws, response, err := self.dial(remote)
	if err != nil {
		self.t.Fatal(err)
	}
	self.t.Cleanup(func() {
		ws.Close()
	})
	if cacheControl := response.Header.Get("Cache-Control"); cacheControl != "no-store" {
		self.t.Fatalf("upgrade cache control = %q, expected no-store", cacheControl)
	}
	requestBytes, err := proto.Marshal(request)
	if err != nil {
		self.t.Fatal(err)
	}
	if err := ws.WriteMessage(websocket.BinaryMessage, requestBytes); err != nil {
		self.t.Fatal(err)
	}
	return ws
}

// Reads one frame: exactly one binary message, no length prefix.
func (self *testLagFeed) readFrame(ws *websocket.Conn) *protocol.ExtenderFeedFrame {
	self.t.Helper()
	ws.SetReadDeadline(time.Now().Add(30 * time.Second))
	messageType, messageBytes, err := ws.ReadMessage()
	if err != nil {
		self.t.Fatal(err)
	}
	if messageType != websocket.BinaryMessage {
		self.t.Fatalf("message type = %d, expected binary", messageType)
	}
	frame := &protocol.ExtenderFeedFrame{}
	if err := proto.Unmarshal(messageBytes, frame); err != nil {
		self.t.Fatal(err)
	}
	return frame
}

// Reads frames through end_of_sample, and returns them without it.
func (self *testLagFeed) readSample(ws *websocket.Conn) []*protocol.ExtenderFeedFrame {
	self.t.Helper()
	frames := []*protocol.ExtenderFeedFrame{}
	for {
		frame := self.readFrame(ws)
		if frame.GetEndOfSample() {
			return frames
		}
		frames = append(frames, frame)
	}
}

// Expects the feed to end with a normal closure.
func (self *testLagFeed) expectNormalClose(ws *websocket.Conn) {
	self.t.Helper()
	ws.SetReadDeadline(time.Now().Add(30 * time.Second))
	_, _, err := ws.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != websocket.CloseNormalClosure {
		self.t.Fatalf("feed end = %v, expected a normal closure", err)
	}
}

// Waits for the feed server to finish the stream from `remote`.
func (self *testLagFeed) waitServed(remote string) {
	self.t.Helper()
	select {
	case remoteAddr := <-self.served:
		if remoteAddr.String() != remote {
			self.t.Fatalf("served %s, expected %s", remoteAddr, remote)
		}
	case <-time.After(30 * time.Second):
		self.t.Fatalf("the stream from %s did not end", remote)
	}
}

// The same request over a carrier stream, a pipe whose remote address is
// `remote`: the reference the websocket must equal.
func (self *testLagFeed) streamSample(remote string, request *protocol.ExtenderFeedRequest) []*protocol.ExtenderFeedFrame {
	self.t.Helper()
	clientConn, serverConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer serverConn.Close()
		self.feedServer.Serve(&testLagFeedAddrConn{
			Conn:       serverConn,
			remoteAddr: net.TCPAddrFromAddrPort(netip.MustParseAddrPort(remote)),
		})
	}()
	defer func() {
		clientConn.Close()
		<-done
	}()
	if err := connect.WriteExtenderFeedRequest(clientConn, request); err != nil {
		self.t.Fatal(err)
	}
	frames := []*protocol.ExtenderFeedFrame{}
	for {
		frame, err := connect.ReadExtenderFeedFrame(clientConn)
		if err != nil {
			self.t.Fatal(err)
		}
		if frame.GetEndOfSample() {
			return frames
		}
		frames = append(frames, frame)
	}
}

// The members of the partition of the vantage at `remote`, sorted.
func (self *testLagFeed) partitionMembers(remote string) []string {
	vantage := connect.ExtenderVantageKey(netip.MustParseAddrPort(remote).Addr())
	members, _, _ := connect.ExtenderPartitionMembers(
		testLagFeedPartitionSecret, connect.ExtenderChannelFeed, vantage, self.keyHexes)
	members = slices.Clone(members)
	slices.Sort(members)
	return members
}

func expectTestLagFeedFrames(t *testing.T, frames []*protocol.ExtenderFeedFrame, expectedFrames []*protocol.ExtenderFeedFrame) {
	t.Helper()
	if len(frames) != len(expectedFrames) {
		t.Fatalf("frames = %d, expected %d", len(frames), len(expectedFrames))
	}
	for i, frame := range frames {
		if !proto.Equal(frame, expectedFrames[i]) {
			t.Fatalf("frame %d differs from the stream's", i)
		}
	}
}

// Over a websocket the feed is exactly the stream feed of the same vantage:
// the same frames in the same order, each one binary message, then
// end_of_sample and a normal closure. The vantage is the remote address
// prefix, so two clients of one /24 are one vantage and a client of a prefix
// whose partition differs gets that partition (M9, R2).
func TestLagFeedOverWebsocketIsTheStreamFeed(t *testing.T) {
	feed := newTestLagFeed(t, 64, nil)
	request := &protocol.ExtenderFeedRequest{SampleCount: 8}

	remote := "198.51.100.7:50001"
	sameVantage := "198.51.100.200:50002"
	// a prefix whose partition holds other records
	otherVantage := ""
	for i := 1; i < 255 && otherVantage == ""; i += 1 {
		candidate := fmt.Sprintf("203.0.%d.9:50003", i)
		if !slices.Equal(feed.partitionMembers(candidate), feed.partitionMembers(remote)) {
			otherVantage = candidate
		}
	}
	if otherVantage == "" {
		t.Fatal("no prefix has another partition")
	}

	expectedFrames := feed.streamSample(remote, request)
	if len(expectedFrames) == 0 {
		t.Fatal("the stream sample is empty")
	}
	for _, client := range []string{remote, sameVantage} {
		ws := feed.open(client, request)
		expectTestLagFeedFrames(t, feed.readSample(ws), expectedFrames)
		feed.expectNormalClose(ws)
		feed.waitServed(client)
	}

	otherFrames := feed.streamSample(otherVantage, request)
	ws := feed.open(otherVantage, request)
	frames := feed.readSample(ws)
	expectTestLagFeedFrames(t, frames, otherFrames)
	feed.expectNormalClose(ws)
	feed.waitServed(otherVantage)
	keyHexes := func(frames []*protocol.ExtenderFeedFrame) []string {
		keyHexes := []string{}
		for _, frame := range frames {
			body, err := feed.directory.RootKeys().VerifyRecord(frame.GetRecord())
			if err != nil {
				t.Fatal(err)
			}
			keyHexes = append(keyHexes, hex.EncodeToString(body.PublicKey))
		}
		slices.Sort(keyHexes)
		return keyHexes
	}
	if slices.Equal(keyHexes(frames), keyHexes(expectedFrames)) {
		t.Fatal("two vantages of different partitions got the same records")
	}
}

// A subscriber gets what the directory applies after its sample, each frame one
// message, and its stream ends when it leaves or when the extender closes.
func TestLagFeedOverWebsocketSubscribes(t *testing.T) {
	feed := newTestLagFeed(t, 8, nil)
	request := &protocol.ExtenderFeedRequest{SampleCount: 8, Subscribe: true}

	remote := "198.51.100.7:50001"
	ws := feed.open(remote, request)
	sample := feed.readSample(ws)
	if len(sample) == 0 {
		t.Fatal("the sample is empty")
	}
	// a revocation is streamed to every subscriber
	revokedKey, err := hex.DecodeString(feed.keyHexes[0])
	if err != nil {
		t.Fatal(err)
	}
	feed.revoke(revokedKey)
	frame := feed.readFrame(ws)
	if frame.GetRevocation() == nil {
		t.Fatalf("frame = %v, expected the revocation", frame)
	}
	// a message after the request is the client leaving
	if err := ws.WriteMessage(websocket.BinaryMessage, []byte{0}); err != nil {
		t.Fatal(err)
	}
	feed.waitServed(remote)
	if count := feed.feedServer.SubscriberCount(); count != 0 {
		t.Fatalf("subscribers = %d after the client left", count)
	}

	// the extender's close ends a subscription that is still open
	other := "198.51.100.8:50002"
	ws = feed.open(other, request)
	feed.readSample(ws)
	feed.server.Close()
	feed.waitServed(other)
}

// A request that is not one binary message is refused without a frame, and a
// stream over the extender's admission limit is refused before the upgrade
// (A12).
func TestLagFeedOverWebsocketRefuses(t *testing.T) {
	feed := newTestLagFeed(t, 8, func(settings *ExtenderSettings) {
		settings.AdmissionActionsPerSubnetPerMinute = 2
	})

	remote := "198.51.100.7:50001"
	ws, _, err := feed.dial(remote)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	if err := ws.WriteMessage(websocket.TextMessage, []byte(`{"sampleCount":8}`)); err != nil {
		t.Fatal(err)
	}
	feed.expectNormalClose(ws)
	feed.waitServed(remote)

	// the second action of the subnet is admitted, the third is not
	ws = feed.open("198.51.100.6:50002", &protocol.ExtenderFeedRequest{SampleCount: 1})
	feed.readSample(ws)
	_, response, err := feed.dial("198.51.100.5:50003")
	if err == nil || response == nil || response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("over the limit = %v, expected 429", err)
	}
	if response.Header.Get("Retry-After") == "" {
		t.Fatal("the refusal names no retry")
	}
}

// Fails when an error's text names either end of a test connection, by its
// address or by its host.
func expectTestLagFeedErrorNamesNoAddress(t *testing.T, name string, err error, addrs ...net.Addr) {
	t.Helper()
	text := err.Error()
	for _, addr := range addrs {
		host, _, splitErr := net.SplitHostPort(addr.String())
		if splitErr != nil {
			t.Fatal(splitErr)
		}
		if strings.Contains(text, host) {
			t.Errorf("%s err = %q names %s", name, text, addr)
		}
	}
}

// Amendment 9: the feed server logs the error of a request read that fails,
// and that error names no address. A client that never sends its request is a
// timeout under the read deadline, and the error still reads as a timeout.
func TestLagFeedRequestErrorNamesNoAddress(t *testing.T) {
	requestErrs := make(chan error, 1)
	feed := newTestLagFeed(t, 1, func(settings *ExtenderSettings) {
		settings.FeedConnHandler = func(conn net.Conn) {
			conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
			_, err := connect.ReadExtenderFeedRequest(conn)
			requestErrs <- err
		}
	})
	ws, _, err := feed.dial("203.0.113.9:40000")
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	select {
	case err := <-requestErrs:
		if err == nil {
			t.Fatal("a request was read")
		}
		expectTestLagFeedErrorNamesNoAddress(t, "request", err, ws.LocalAddr(), ws.RemoteAddr())
		var netErr net.Error
		if !errors.As(err, &netErr) || !netErr.Timeout() {
			t.Errorf("request err = %v, expected a timeout", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the request read did not end")
	}
}

// Amendment 9: a client that resets its connection leaves the feed server a
// read error and a write error, and neither names an address.
func TestLagFeedSocketErrorsNameNoAddress(t *testing.T) {
	reset := make(chan struct{})
	readErrs := make(chan error, 1)
	writeErrs := make(chan error, 1)
	feed := newTestLagFeed(t, 1, func(settings *ExtenderSettings) {
		settings.FeedConnHandler = func(conn net.Conn) {
			<-reset
			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, err := connect.ReadExtenderFeedRequest(conn)
			readErrs <- err
			// the first writes after a reset may still be taken by the socket
			err = nil
			for deadline := time.Now().Add(5 * time.Second); err == nil && time.Now().Before(deadline); {
				conn.SetWriteDeadline(time.Now().Add(time.Second))
				err = connect.WriteExtenderFeedFrame(conn, &protocol.ExtenderFeedFrame{
					Frame: &protocol.ExtenderFeedFrame_Keepalive{Keepalive: true},
				})
				time.Sleep(10 * time.Millisecond)
			}
			writeErrs <- err
		}
	})
	ws, _, err := feed.dial("203.0.113.9:40000")
	if err != nil {
		t.Fatal(err)
	}
	localAddr := ws.LocalAddr()
	remoteAddr := ws.RemoteAddr()
	tcpConn, ok := ws.NetConn().(*tls.Conn).NetConn().(*net.TCPConn)
	if !ok {
		t.Fatalf("the feed's connection is a %T", ws.NetConn().(*tls.Conn).NetConn())
	}
	// a reset rather than a close
	tcpConn.SetLinger(0)
	tcpConn.Close()
	close(reset)
	for name, errs := range map[string]chan error{"read": readErrs, "write": writeErrs} {
		select {
		case err := <-errs:
			if err == nil {
				t.Fatalf("no %s err after the reset", name)
			}
			expectTestLagFeedErrorNamesNoAddress(t, name, err, localAddr, remoteAddr)
		case <-time.After(15 * time.Second):
			t.Fatalf("the %s did not end", name)
		}
	}
}

// A net.Error that keeps only the text of another, as gorilla's copy of a
// temporary socket error does.
type testLagFeedCopiedError struct {
	text    string
	timeout bool
}

func (self testLagFeedCopiedError) Error() string   { return self.text }
func (self testLagFeedCopiedError) Timeout() bool   { return self.timeout }
func (self testLagFeedCopiedError) Temporary() bool { return true }

// Amendment 9: the feed conn's errors keep what callers read from them,
// Timeout(), errors.Is through them, io.EOF itself and a close's code, and
// their text names no address.
func TestLagFeedConnErrorNamesNoAddress(t *testing.T) {
	localAddr := &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 443}
	remoteAddr := &net.TCPAddr{IP: net.ParseIP("203.0.113.9"), Port: 40000}
	opErr := func(op string, err error) *net.OpError {
		return &net.OpError{Op: op, Net: "tcp", Source: localAddr, Addr: remoteAddr, Err: err}
	}
	for name, c := range map[string]struct {
		err      error
		timeout  bool
		is       error
		expected string
	}{
		"timeout": {
			err:      opErr("read", os.ErrDeadlineExceeded),
			timeout:  true,
			is:       os.ErrDeadlineExceeded,
			expected: "read tcp: i/o timeout",
		},
		"reset": {
			err:      opErr("read", &os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET}),
			is:       syscall.ECONNRESET,
			expected: "read tcp: read: " + syscall.ECONNRESET.Error(),
		},
		"wrapped": {
			err:      fmt.Errorf("tls: %w", opErr("write", net.ErrClosed)),
			is:       net.ErrClosed,
			expected: "write tcp: " + net.ErrClosed.Error(),
		},
		"copied timeout": {
			err:      testLagFeedCopiedError{text: opErr("read", os.ErrDeadlineExceeded).Error(), timeout: true},
			timeout:  true,
			expected: "i/o timeout",
		},
		"copied": {
			err:      testLagFeedCopiedError{text: opErr("write", net.ErrClosed).Error()},
			expected: "network error",
		},
		"close": {
			err:      &websocket.CloseError{Code: websocket.CloseGoingAway, Text: "from " + remoteAddr.String()},
			expected: (&websocket.CloseError{Code: websocket.CloseGoingAway}).Error(),
		},
	} {
		if !strings.Contains(c.err.Error(), remoteAddr.IP.String()) {
			t.Fatalf("%s: the case names no address: %q", name, c.err)
		}
		err := extenderLagFeedConnError(c.err)
		if text := err.Error(); text != c.expected {
			t.Errorf("%s: err = %q, expected %q", name, text, c.expected)
		}
		expectTestLagFeedErrorNamesNoAddress(t, name, err, localAddr, remoteAddr)
		var netErr net.Error
		if timeout := errors.As(err, &netErr) && netErr.Timeout(); timeout != c.timeout {
			t.Errorf("%s: timeout = %t, expected %t", name, timeout, c.timeout)
		}
		if c.is != nil && !errors.Is(err, c.is) {
			t.Errorf("%s: err = %v, expected it to be %v", name, err, c.is)
		}
	}
	closeErr := &websocket.CloseError{}
	if err := extenderLagFeedConnError(&websocket.CloseError{Code: websocket.CloseNormalClosure, Text: "bye"}); !errors.As(err, &closeErr) ||
		closeErr.Code != websocket.CloseNormalClosure || closeErr.Text != "" {
		t.Errorf("close err = %#v, expected the code alone", err)
	}
	for _, err := range []error{nil, io.EOF, errExtenderLagFeedAfterRequest, websocket.ErrCloseSent} {
		if mapped := extenderLagFeedConnError(err); mapped != err {
			t.Errorf("err %v = %v, expected it unchanged", err, mapped)
		}
	}
}
