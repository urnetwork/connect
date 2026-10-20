//go:build !js

package connect

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// Tests of the carrier's stream adapter (EXTENDER.md S, net_extender_webrtc_conn.go).
//
// The fake channel below behaves exactly as a detached pion data channel
// does on a read: one message per Read, and a buffer shorter than the message
// consumes the message and answers io.ErrShortBuffer with nothing copied
// (pion/sctp reassembly_queue.read). That is the root cause the adapter
// exists for.

// testMessageChannel is a message-oriented ReadWriteCloserDeadliner.
type testMessageChannel struct {
	inbound  [][]byte
	outbound [][]byte
	// a message larger than every read buffer, to prove the refusal
	closeCount    int
	readDeadline  time.Time
	writeDeadline time.Time
}

func (self *testMessageChannel) Read(b []byte) (int, error) {
	if len(self.inbound) == 0 {
		return 0, io.EOF
	}
	message := self.inbound[0]
	self.inbound = self.inbound[1:]
	if len(b) < len(message) {
		// pion consumes the message and reports the short buffer
		return 0, io.ErrShortBuffer
	}
	return copy(b, message), nil
}

func (self *testMessageChannel) Write(b []byte) (int, error) {
	self.outbound = append(self.outbound, append([]byte(nil), b...))
	return len(b), nil
}

func (self *testMessageChannel) Close() error {
	self.closeCount += 1
	if self.closeCount == 1 {
		return nil
	}
	return errors.New("closed twice")
}

func (self *testMessageChannel) SetReadDeadline(t time.Time) error {
	self.readDeadline = t
	return nil
}

func (self *testMessageChannel) SetWriteDeadline(t time.Time) error {
	self.writeDeadline = t
	return nil
}

// Root cause: a detached data channel hands back one message per read and
// drops the rest of a message a short buffer did not take, so a stream reader
// that asks for the 4 byte frame length of a 300 byte request loses the
// request (the extender then closes the association and the dialer sees an
// SCTP abort). Observable: a read shorter than the message, then the rest.
func TestWebRtcExtenderStreamReadKeepsTheRestOfAMessage(t *testing.T) {
	message := bytes.Repeat([]byte("extender request "), 20)
	channel := &testMessageChannel{inbound: [][]byte{message}}
	conn := newWebRtcDataChannelConn(channel, nil, nil, len(message))

	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		t.Fatalf("read the first 4 bytes: %v", err)
	}
	if !bytes.Equal(head, message[:4]) {
		t.Fatalf("head = %q, want %q", head, message[:4])
	}
	rest := make([]byte, len(message)-4)
	if _, err := io.ReadFull(conn, rest); err != nil {
		t.Fatalf("read the rest: %v", err)
	}
	if !bytes.Equal(rest, message[4:]) {
		t.Fatalf("rest does not match the message")
	}
	// the message is spent: the next read is the channel's end
	if _, err := conn.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("read past the message = %v, want EOF", err)
	}
}

// A read that spans two messages takes them one at a time, never joining
// bytes of two messages into one short answer that skips any.
func TestWebRtcExtenderStreamReadCrossesMessageBoundariesInOrder(t *testing.T) {
	channel := &testMessageChannel{inbound: [][]byte{[]byte("abc"), []byte("defg"), []byte("h")}}
	conn := newWebRtcDataChannelConn(channel, nil, nil, 16)
	all, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	if string(all) != "abcdefgh" {
		t.Fatalf("stream = %q, want abcdefgh", all)
	}
}

// A peer that sends a message past this side's advertised maximum has
// corrupted the stream: the channel already consumed it, so the read fails
// rather than silently continuing past a hole.
func TestWebRtcExtenderStreamReadRefusesAnOversizedMessage(t *testing.T) {
	channel := &testMessageChannel{inbound: [][]byte{make([]byte, 100)}}
	conn := newWebRtcDataChannelConn(channel, nil, nil, 64)
	n, err := conn.Read(make([]byte, 1024))
	if err == nil || n != 0 {
		t.Fatalf("oversized message read = %d, %v, want an error", n, err)
	}
	if errors.Is(err, io.ErrShortBuffer) {
		t.Fatalf("the channel's short buffer must be reported as a carrier error, got %v", err)
	}
}

// A write larger than the message bound every webrtc implementation accepts
// is cut into messages of at most that bound, in order, with nothing lost.
func TestWebRtcExtenderStreamWriteSplitsAtTheMessageBound(t *testing.T) {
	channel := &testMessageChannel{}
	conn := newWebRtcDataChannelConn(channel, nil, nil, 0)
	payload := make([]byte, 2*webRtcExtenderMaxMessageByteCount+1000)
	for i := range payload {
		payload[i] = byte(i)
	}
	n, err := conn.Write(payload)
	if err != nil || n != len(payload) {
		t.Fatalf("write = %d, %v", n, err)
	}
	expectSizes := []int{webRtcExtenderMaxMessageByteCount, webRtcExtenderMaxMessageByteCount, 1000}
	if len(channel.outbound) != len(expectSizes) {
		t.Fatalf("messages = %d, want %d", len(channel.outbound), len(expectSizes))
	}
	joined := []byte{}
	for i, message := range channel.outbound {
		if len(message) != expectSizes[i] {
			t.Fatalf("message %d = %d bytes, want %d", i, len(message), expectSizes[i])
		}
		joined = append(joined, message...)
	}
	if !bytes.Equal(joined, payload) {
		t.Fatalf("the messages do not join back into the payload")
	}
}

// testBufferedMessageChannel is a message channel that also reports an
// unacknowledged amount, as a detached pion channel does, releasing it only
// when the test says the peer acknowledged.
type testBufferedMessageChannel struct {
	testMessageChannel
	stateLock      sync.Mutex
	bufferedAmount uint64
	threshold      uint64
	onLow          func()
	closedWhile    uint64
	// a live association: a read blocks until the channel is closed, then
	// reports the association gone, as a detached pion channel does
	readBlocked chan struct{}
	// a dead association answers every read with this at once
	readErr     error
	readChanged chan struct{}
}

func newTestBufferedMessageChannel(bufferedAmount uint64) *testBufferedMessageChannel {
	return &testBufferedMessageChannel{
		bufferedAmount: bufferedAmount,
		readBlocked:    make(chan struct{}),
		readChanged:    make(chan struct{}),
	}
}

func (self *testBufferedMessageChannel) Read(b []byte) (int, error) {
	for {
		self.stateLock.Lock()
		readErr, deadline, changed := self.readErr, self.readDeadline, self.readChanged
		self.stateLock.Unlock()
		if readErr != nil {
			return 0, readErr
		}
		var expired <-chan time.Time
		if !deadline.IsZero() {
			if !time.Now().Before(deadline) {
				return 0, fmt.Errorf("read deadline exceeded: %w", os.ErrDeadlineExceeded)
			}
			expired = time.After(time.Until(deadline))
		}
		select {
		case <-self.readBlocked:
			return 0, errors.New("association closed")
		case <-changed:
		case <-expired:
		}
	}
}

// Deadline changes wake the same reader, as the real detached channel does.
func (self *testBufferedMessageChannel) SetReadDeadline(deadline time.Time) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.readDeadline = deadline
	close(self.readChanged)
	self.readChanged = make(chan struct{})
	return nil
}

func (self *testBufferedMessageChannel) BufferedAmount() uint64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.bufferedAmount
}

func (self *testBufferedMessageChannel) SetBufferedAmountLowThreshold(threshold uint64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.threshold = threshold
}

func (self *testBufferedMessageChannel) OnBufferedAmountLow(f func()) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.onLow = f
}

func (self *testBufferedMessageChannel) Close() error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.closedWhile = self.bufferedAmount
	err := self.testMessageChannel.Close()
	if self.closeCount == 1 {
		close(self.readBlocked)
	}
	return err
}

// acknowledge releases every buffered byte, as a peer's acknowledgement does.
func (self *testBufferedMessageChannel) acknowledge() {
	var onLow func()
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.bufferedAmount = 0
		onLow = self.onLow
	}()
	if onLow != nil {
		onLow()
	}
}

// Root cause: closing the peer connection aborts the association and drops
// unacknowledged data, so a response written just before a close never
// reached the peer (the dialer read an SCTP abort instead of the hello).
// Observable: the close waits for the channel to report nothing
// unacknowledged before it closes anything.
func TestWebRtcExtenderStreamCloseDrainsBeforeClosing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		channel := newTestBufferedMessageChannel(300)
		conn := newWebRtcDataChannelConn(channel, nil, nil, 0)
		closeDone := make(chan error, 1)
		go func() { closeDone <- conn.Close() }()
		synctest.Wait()
		select {
		case err := <-closeDone:
			t.Fatalf("close returned %v with %d bytes unacknowledged", err, channel.bufferedAmount)
		default:
		}
		channel.acknowledge()
		if err := <-closeDone; err != nil {
			t.Fatal(err)
		}
		if channel.closedWhile != 0 {
			t.Fatalf("the channel was closed with %d bytes unacknowledged", channel.closedWhile)
		}
		if channel.threshold != 0 {
			t.Fatalf("drain threshold = %d, want 0", channel.threshold)
		}
	})
}

// Root cause: an aborted association never acknowledges, and nothing on the
// peer connection says it died, so a close would wait its whole bound (the
// client's own close waited 5 s on the ClientHello the extender had already
// aborted). Observable: a close on a channel whose read fails at once ends
// without any acknowledgement and long before the bound.
func TestWebRtcExtenderStreamCloseGivesUpOnADeadChannel(t *testing.T) {
	channel := newTestBufferedMessageChannel(1544)
	channel.readErr = errors.New("abort chunk")
	conn := newWebRtcDataChannelConn(channel, nil, nil, 0)
	conn.drainTimeout = time.Hour
	closeDone := make(chan error, 1)
	go func() { closeDone <- conn.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the close waited on a dead association")
	}
	if channel.closeCount != 1 {
		t.Fatalf("channel closed %d times", channel.closeCount)
	}
}

// The peer's half-close is not death: the close keeps waiting for the
// acknowledgement, which still arrives on a live association.
func TestWebRtcExtenderStreamCloseWaitsPastThePeersHalfClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		channel := newTestBufferedMessageChannel(300)
		channel.readErr = io.EOF
		conn := newWebRtcDataChannelConn(channel, nil, nil, 0)
		closeDone := make(chan error, 1)
		go func() { closeDone <- conn.Close() }()
		synctest.Wait()
		select {
		case err := <-closeDone:
			t.Fatalf("close returned %v on the peer's half-close with bytes unacknowledged", err)
		default:
		}
		channel.acknowledge()
		if err := <-closeDone; err != nil {
			t.Fatal(err)
		}
		if channel.closedWhile != 0 {
			t.Fatalf("the channel was closed with %d bytes unacknowledged", channel.closedWhile)
		}
	})
}

// Root cause: pion reports a passed deadline as a plain error wrapping
// os.ErrDeadlineExceeded, which is not a net.Error; net/http asserts
// net.Error on the read it interrupts at a hijack and, seeing no timeout,
// cancels the request context, so the extender's forward dial ran on a
// context already canceled. Observable: a deadline error read or written
// through the stream is the net.Error timeout every net.Conn reports.
func TestWebRtcExtenderStreamReportsDeadlinesAsNetTimeouts(t *testing.T) {
	pionDeadlineErr := fmt.Errorf("read deadline exceeded: %w", os.ErrDeadlineExceeded)
	channel := &testErrorMessageChannel{readErr: pionDeadlineErr, writeErr: pionDeadlineErr}
	conn := newWebRtcDataChannelConn(channel, nil, nil, 0)

	_, readErr := conn.Read(make([]byte, 16))
	netErr, ok := readErr.(net.Error)
	if !ok || !netErr.Timeout() {
		t.Fatalf("read deadline error = %T %v, want a net.Error timeout", readErr, readErr)
	}
	_, writeErr := conn.Write([]byte("x"))
	netErr, ok = writeErr.(net.Error)
	if !ok || !netErr.Timeout() {
		t.Fatalf("write deadline error = %T %v, want a net.Error timeout", writeErr, writeErr)
	}
	// any other error passes through untouched
	other := errors.New("stream closed")
	channel.readErr = other
	if _, err := conn.Read(make([]byte, 16)); !errors.Is(err, other) {
		t.Fatalf("other read error = %v, want it unchanged", err)
	}
}

// testErrorMessageChannel answers every read and write with its error.
type testErrorMessageChannel struct {
	testMessageChannel
	readErr  error
	writeErr error
}

func (self *testErrorMessageChannel) Read(b []byte) (int, error) { return 0, self.readErr }

func (self *testErrorMessageChannel) Write(b []byte) (int, error) { return 0, self.writeErr }

// A deadline on the stream reaches the channel on both sides.
func TestWebRtcExtenderStreamDeadlinesReachTheChannel(t *testing.T) {
	channel := &testMessageChannel{}
	conn := newWebRtcDataChannelConn(channel, nil, nil, 0)
	deadline := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	if err := conn.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	if !channel.readDeadline.Equal(deadline) || !channel.writeDeadline.Equal(deadline) {
		t.Fatalf("deadlines = %v / %v, want %v on both", channel.readDeadline, channel.writeDeadline, deadline)
	}
	later := deadline.Add(time.Minute)
	if err := conn.SetReadDeadline(later); err != nil {
		t.Fatal(err)
	}
	if !channel.readDeadline.Equal(later) || !channel.writeDeadline.Equal(deadline) {
		t.Fatalf("a read deadline must not move the write deadline")
	}
}

// Close releases the channel once however often it is called, and repeats
// its first result.
func TestWebRtcExtenderStreamCloseReleasesOnce(t *testing.T) {
	channel := &testMessageChannel{}
	conn := newWebRtcDataChannelConn(channel, nil, nil, 0)
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("second close = %v, want the first result", err)
	}
	if channel.closeCount != 1 {
		t.Fatalf("channel closed %d times, want once", channel.closeCount)
	}
}

// Without a selected ICE pair the stream carries the carrier placeholder
// address, which names the carrier and nothing else.
func TestWebRtcExtenderStreamPlaceholderAddressNamesTheCarrier(t *testing.T) {
	conn := newWebRtcDataChannelConn(&testMessageChannel{}, nil, nil, 0)
	if conn.RemoteAddr().Network() != ExtenderCarrierWebRtc || conn.RemoteAddr().String() != ExtenderCarrierWebRtc {
		t.Fatalf("placeholder = %s/%s", conn.RemoteAddr().Network(), conn.RemoteAddr().String())
	}
	if conn.LocalAddr().Network() != ExtenderCarrierWebRtc {
		t.Fatalf("local placeholder = %s", conn.LocalAddr().Network())
	}
}
