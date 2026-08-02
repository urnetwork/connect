//go:build !js

package connect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	mathrand "math/rand"
	"net"
	"os"
	"runtime"
	"runtime/pprof"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/urnetwork/connect/protocol"
)

func TestWebRtc(t *testing.T) {
	// the 1MiB transfer over local ice varies widely in time under -race
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	settingsA := DefaultWebRtcSettings()
	settingsB := DefaultWebRtcSettings()

	// each manager sends signals to each other
	signalPipeA := newSignalPipe(nil)
	signalPipeB := newSignalPipe(nil)

	webRtcManagerA := NewWebRtcManager(ctx, signalPipeA, settingsA)
	webRtcManagerB := NewWebRtcManager(ctx, signalPipeB, settingsB)

	signalPipeA.signalReceiver = webRtcManagerB
	signalPipeB.signalReceiver = webRtcManagerA

	peerIdA := NewId()
	peerIdB := NewId()
	streamId := NewId()

	connA, err := webRtcManagerA.NewP2pConnActive(ctx, NewTransferPath(peerIdA, peerIdB, streamId))
	AssertEqual(t, err, nil)
	defer connA.Close()

	connB, err := webRtcManagerB.NewP2pConnPassive(ctx, NewTransferPath(peerIdB, peerIdA, streamId))
	AssertEqual(t, err, nil)
	defer connB.Close()

	b := make([]byte, 1024*1024)
	mathrand.Read(b)

	received := make(chan []byte)

	// the helpers must not panic on conn errors. reads and writes that race
	// the test teardown see closed-conn errors, and a panic in a test
	// goroutine kills the whole test binary. missing data is detected by
	// the receive loop timeout below.
	//
	// send in transport-sized messages: the detached datachannel is
	// message-oriented, and a single message must fit within the
	// per-connection ReceiveBufferSize to be reassembled (production frames
	// are bounded by the transport MaxMessageByteCount default)
	const sendMessageByteCount = 64 * 1024
	send := func(conn net.Conn) {
		for i := 0; i < len(b); i += sendMessageByteCount {
			end := min(i+sendMessageByteCount, len(b))
			if _, err := conn.Write(b[i:end]); err != nil {
				return
			}
		}
	}
	receive := func(conn net.Conn) {
		b2 := make([]byte, len(b))
		if _, err := io.ReadFull(conn, b2); err != nil {
			return
		}
		select {
		case <-ctx.Done():
		case received <- b2:
		}
	}

	go send(connA)
	go receive(connA)
	go send(connB)
	go receive(connB)

	for range 2 {
		select {
		case <-ctx.Done():
			t.Fatal("timeout")
		case b2 := <-received:
			AssertEqual(t, b, b2)
		}
	}

}

// TestWebRtcMessageRoundTrip verifies the P2P transport's native message
// framing: the detached data channel is message-oriented (one Write becomes one
// SCTP message the peer reads back whole), so consecutive TransferFrames of
// varied sizes must each arrive intact and in order with no length prefix. The
// receive side mirrors P2pReceiveTransport, including detached Pion's
// n=0/io.ErrShortBuffer behavior for messages above the first 4 KiB attempt.
func TestWebRtcMessageRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	settingsA := DefaultWebRtcSettings()
	settingsB := DefaultWebRtcSettings()

	signalPipeA := newSignalPipe(nil)
	signalPipeB := newSignalPipe(nil)

	webRtcManagerA := NewWebRtcManager(ctx, signalPipeA, settingsA)
	webRtcManagerB := NewWebRtcManager(ctx, signalPipeB, settingsB)

	signalPipeA.signalReceiver = webRtcManagerB
	signalPipeB.signalReceiver = webRtcManagerA

	peerIdA := NewId()
	peerIdB := NewId()
	streamId := NewId()

	connA, err := webRtcManagerA.NewP2pConnActive(ctx, NewTransferPath(peerIdA, peerIdB, streamId))
	AssertEqual(t, err, nil)
	defer connA.Close()

	connB, err := webRtcManagerB.NewP2pConnPassive(ctx, NewTransferPath(peerIdB, peerIdA, streamId))
	AssertEqual(t, err, nil)
	defer connB.Close()

	sizes := []int{1, 100, 255, 256, 257, 1000, int(kib(4)), int(kib(6)), int(kib(12))}
	messages := make([][]byte, len(sizes))
	for i, size := range sizes {
		m := make([]byte, size)
		for j := range m {
			m[j] = byte((i*31 + j) % 256)
		}
		messages[i] = m
	}

	readErr := make(chan error, 1)
	go func() {
		for i := range messages {
			got, err := readP2pMessage(
				connB,
				int(kib(4)),
				int(kib(4)),
				int(settingsB.MaxMessageSize),
			)
			if err != nil {
				readErr <- fmt.Errorf("read %d: %w", i, err)
				return
			}
			if !bytes.Equal(got, messages[i]) {
				MessagePoolReturn(got)
				readErr <- fmt.Errorf("frame %d mismatch (got %d bytes, want %d)", i, len(got), len(messages[i]))
				return
			}
			MessagePoolReturn(got)
		}
		readErr <- nil
	}()

	for i := range messages {
		if _, err := connA.Write(messages[i]); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	select {
	case <-ctx.Done():
		t.Fatal("timeout waiting for frames")
	case err := <-readErr:
		AssertEqual(t, err, nil)
	}

	// The SCTP association enforces the same hard message bound as the P2P
	// transport instead of advertising Pion's ~1 GiB default and relying on a
	// later 64 KiB read buffer to reject an already-reassembled message.
	if _, err := connA.Write(make([]byte, int(settingsA.MaxMessageSize)+1)); err == nil {
		t.Fatal("SCTP accepted a message above the bounded transport maximum")
	}
}

func TestDefaultWebRtcDataChannelIsReliableUnordered(t *testing.T) {
	init := webRtcDataChannelInit(DefaultWebRtcSettings())
	if init.Ordered == nil {
		t.Fatal("data channel ordering was left at Pion's ordered default")
	}
	AssertEqual(t, *init.Ordered, false)
	AssertEqual(t, init.MaxRetransmits, (*uint16)(nil))
	AssertEqual(t, init.MaxPacketLifeTime, (*uint16)(nil))
}

type shortBufferMessageConn struct {
	lock            sync.Mutex
	message         []byte
	delivered       bool
	reportSize      bool
	readSizes       []int
	deliveredBuffer []byte
	closed          chan struct{}
	closeOnce       sync.Once
}

func newShortBufferMessageConn(message []byte) *shortBufferMessageConn {
	return &shortBufferMessageConn{
		message:    message,
		reportSize: true,
		closed:     make(chan struct{}),
	}
}

func (self *shortBufferMessageConn) Read(b []byte) (int, error) {
	self.lock.Lock()
	self.readSizes = append(self.readSizes, len(b))
	if !self.delivered {
		if len(b) < len(self.message) {
			requiredByteCount := 0
			if self.reportSize {
				requiredByteCount = len(self.message)
			}
			self.lock.Unlock()
			// SCTP reports the complete queued message length, while detached
			// Pion masks it to zero. Both leave the message available to retry.
			return requiredByteCount, io.ErrShortBuffer
		}
		n := copy(b, self.message)
		self.delivered = true
		self.deliveredBuffer = b[:n]
		self.lock.Unlock()
		return n, nil
	}
	closed := self.closed
	self.lock.Unlock()
	<-closed
	return 0, net.ErrClosed
}

func (self *shortBufferMessageConn) Write([]byte) (int, error) {
	return 0, net.ErrClosed
}

func (self *shortBufferMessageConn) Close() error {
	self.closeOnce.Do(func() {
		close(self.closed)
	})
	return nil
}

func (self *shortBufferMessageConn) LocalAddr() net.Addr {
	return testingNetAddr("local")
}

func (self *shortBufferMessageConn) RemoteAddr() net.Addr {
	return testingNetAddr("remote")
}

func (self *shortBufferMessageConn) SetDeadline(time.Time) error {
	return nil
}

func (self *shortBufferMessageConn) SetReadDeadline(time.Time) error {
	return nil
}

func (self *shortBufferMessageConn) SetWriteDeadline(time.Time) error {
	return nil
}

type testingNetAddr string

func (self testingNetAddr) Network() string {
	return "test"
}

func (self testingNetAddr) String() string {
	return string(self)
}

type queuedMessageConn struct {
	lock       sync.Mutex
	messages   [][]byte
	reportSize bool
	readSizes  []int
	closed     chan struct{}
	closeOnce  sync.Once
}

func newQueuedMessageConn(messages ...[]byte) *queuedMessageConn {
	return &queuedMessageConn{
		messages:   messages,
		reportSize: true,
		closed:     make(chan struct{}),
	}
}

func (self *queuedMessageConn) Read(b []byte) (int, error) {
	self.lock.Lock()
	self.readSizes = append(self.readSizes, len(b))
	if 0 < len(self.messages) {
		message := self.messages[0]
		if len(b) < len(message) {
			requiredByteCount := 0
			if self.reportSize {
				requiredByteCount = len(message)
			}
			self.lock.Unlock()
			return requiredByteCount, io.ErrShortBuffer
		}
		self.messages = self.messages[1:]
		n := copy(b, message)
		self.lock.Unlock()
		return n, nil
	}
	closed := self.closed
	self.lock.Unlock()
	<-closed
	return 0, net.ErrClosed
}

func (self *queuedMessageConn) Write([]byte) (int, error) {
	return 0, net.ErrClosed
}

func (self *queuedMessageConn) Close() error {
	self.closeOnce.Do(func() {
		close(self.closed)
	})
	return nil
}

func (self *queuedMessageConn) LocalAddr() net.Addr {
	return testingNetAddr("local")
}

func (self *queuedMessageConn) RemoteAddr() net.Addr {
	return testingNetAddr("remote")
}

func (self *queuedMessageConn) SetDeadline(time.Time) error {
	return nil
}

func (self *queuedMessageConn) SetReadDeadline(time.Time) error {
	return nil
}

func (self *queuedMessageConn) SetWriteDeadline(time.Time) error {
	return nil
}

func TestP2pReadyHeaderPrefetchesUnorderedDataWithinRouteBound(t *testing.T) {
	earlyA := bytes.Repeat([]byte{0xa1}, 12*1024)
	earlyB := bytes.Repeat([]byte{0xb2}, 5*1024)
	earlyDropped := bytes.Repeat([]byte{0xc3}, 6*1024)
	steady := bytes.Repeat([]byte{0xd4}, 7*1024)
	conn := newQueuedMessageConn(
		earlyA,
		earlyB,
		earlyDropped,
		[]byte(ReadyHeader),
		steady,
	)
	// Match detached Pion, which masks SCTP's required byte count to zero.
	conn.reportSize = false
	defer conn.Close()
	settings := DefaultP2pTransportSettings()
	settings.ChannelBufferSize = 2

	prefetched, err := readP2pReadyHeader(conn, settings)
	AssertEqual(t, err, nil)
	if len(prefetched) != settings.ChannelBufferSize {
		t.Fatalf("prefetched %d messages, expected hard bound %d", len(prefetched), settings.ChannelBufferSize)
	}

	ctx, cancel := context.WithCancel(context.Background())
	_, route := newP2pReceiveTransport(ctx, cancel, conn, NewId(), settings, prefetched)
	defer cancel()
	for i, expected := range [][]byte{earlyA, earlyB, steady} {
		select {
		case received := <-route:
			if !bytes.Equal(received, expected) {
				MessagePoolReturn(received)
				t.Fatalf("message %d changed or reordered at prefetch handoff", i)
			}
			MessagePoolReturn(received)
		case <-time.After(time.Second):
			t.Fatalf("message %d not delivered", i)
		}
	}

	conn.lock.Lock()
	readSizes := append([]int(nil), conn.readSizes...)
	conn.lock.Unlock()
	expectedPrefix := []int{
		len(ReadyHeader), 4 * 1024, 8 * 1024, 16 * 1024,
		len(ReadyHeader), 4 * 1024, 8 * 1024,
		len(ReadyHeader), 4 * 1024, 8 * 1024,
		len(ReadyHeader),
		settings.InitialReadBufferByteCount, 8 * 1024,
	}
	if len(readSizes) < len(expectedPrefix) || !slices.Equal(readSizes[:len(expectedPrefix)], expectedPrefix) {
		t.Fatalf("unexpected adaptive ready/steady reads: got=%v want-prefix=%v", readSizes, expectedPrefix)
	}
}

func TestP2pReceiveTransportGrowsBufferWithoutLosingMessage(t *testing.T) {
	for _, reportSize := range []bool{true, false} {
		t.Run(fmt.Sprintf("required-size=%t", reportSize), func(t *testing.T) {
			message := make([]byte, 12*1024)
			for i := range message {
				message[i] = byte(i)
			}
			conn := newShortBufferMessageConn(message)
			conn.reportSize = reportSize
			ctx, cancel := context.WithCancel(context.Background())
			settings := DefaultP2pTransportSettings()
			settings.ChannelBufferSize = 1
			settings.ReadTimeout = time.Hour

			_, route := NewP2pReceiveTransport(ctx, cancel, conn, NewId(), settings)
			select {
			case received := <-route:
				if !bytes.Equal(received, message) {
					t.Fatalf("adaptive read changed message: got=%d want=%d", len(received), len(message))
				}
				conn.lock.Lock()
				deliveredBuffer := conn.deliveredBuffer
				conn.lock.Unlock()
				if len(received) == 0 || len(deliveredBuffer) == 0 ||
					&received[0] != &deliveredBuffer[0] {
					t.Fatal("receive transport copied instead of transferring read-buffer ownership")
				}
				MessagePoolReturn(received)
			case <-time.After(time.Second):
				t.Fatal("adaptive receive did not retry the queued message")
			}

			conn.lock.Lock()
			readSizes := append([]int(nil), conn.readSizes...)
			conn.lock.Unlock()
			expected := []int{4 * 1024, len(message)}
			if !reportSize {
				expected = []int{4 * 1024, 8 * 1024, 16 * 1024}
			}
			if len(readSizes) < len(expected) || !slices.Equal(readSizes[:len(expected)], expected) {
				t.Fatalf("unexpected adaptive reads: got=%v want-prefix=%v", readSizes, expected)
			}
			for _, size := range readSizes {
				if settings.MaxMessageByteCount < size {
					t.Fatalf("receive buffer exceeded hard maximum: %d", size)
				}
			}

			cancel()
			AssertEqual(t, conn.Close(), nil)
		})
	}
}

func TestP2pReceiveTransportRejectsOversizedShortBufferWithoutPanic(t *testing.T) {
	for _, reportSize := range []bool{true, false} {
		t.Run(fmt.Sprintf("required-size=%t", reportSize), func(t *testing.T) {
			settings := DefaultP2pTransportSettings()
			conn := newShortBufferMessageConn(make([]byte, settings.MaxMessageByteCount+1))
			conn.reportSize = reportSize
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			NewP2pReceiveTransport(ctx, cancel, conn, NewId(), settings)
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
				t.Fatal("oversized message did not terminate the bounded receiver")
			}
			conn.lock.Lock()
			readSizes := append([]int(nil), conn.readSizes...)
			conn.lock.Unlock()
			expected := []int{settings.InitialReadBufferByteCount}
			if !reportSize {
				expected = []int{4 * 1024, 8 * 1024, 16 * 1024, 32 * 1024, 64 * 1024}
			}
			AssertEqual(t, readSizes, expected)
			AssertEqual(t, conn.Close(), nil)
		})
	}
}

func TestWebRtcBlockingWriteBackpressureAndDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	settingsA := DefaultWebRtcSettings()
	settingsB := DefaultWebRtcSettings()
	settingsA.Log = NewNoopLogger()
	settingsB.Log = NewNoopLogger()
	settingsA.IceServerUrls = nil
	settingsB.IceServerUrls = nil
	settingsA.ReceiveBufferSize = kib(128)
	settingsB.ReceiveBufferSize = kib(128)

	signalPipeA := newSignalPipe(nil)
	signalPipeB := newSignalPipe(nil)
	managerA := NewWebRtcManager(ctx, signalPipeA, settingsA)
	managerB := NewWebRtcManager(ctx, signalPipeB, settingsB)
	signalPipeA.signalReceiver = managerB
	signalPipeB.signalReceiver = managerA

	peerIdA := NewId()
	peerIdB := NewId()
	streamId := NewId()
	connA, err := managerA.NewP2pConnActive(ctx, NewTransferPath(peerIdA, peerIdB, streamId))
	AssertEqual(t, err, nil)
	defer connA.Close()
	connB, err := managerB.NewP2pConnPassive(ctx, NewTransferPath(peerIdB, peerIdA, streamId))
	AssertEqual(t, err, nil)
	defer connB.Close()

	connectedDeadline := time.Now().Add(5 * time.Second)
	for !connA.Connected() || !connB.Connected() {
		if time.Now().After(connectedDeadline) {
			t.Fatal("peer connections did not connect")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Never read connB. Its bounded SCTP receive window must eventually
	// propagate all the way back to connA.Write. In non-blocking Pion mode all
	// of these writes merely accumulated in an unbounded pending queue and
	// SetWriteDeadline was explicitly documented as ineffective.
	message := make([]byte, 64*1024)
	var writeErr error
	start := time.Now()
	for range 64 {
		connA.SetWriteDeadline(time.Now().Add(250 * time.Millisecond))
		if _, writeErr = connA.Write(message); writeErr != nil {
			break
		}
	}
	if writeErr == nil {
		t.Fatal("writes did not backpressure after filling the peer receive window")
	}
	if 5*time.Second < time.Since(start) {
		t.Fatalf("write deadline failed to bound backpressure: %s", time.Since(start))
	}
}

func TestWebRtcSctpSnapMixedCompatibility(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	settingsA := DefaultWebRtcSettings()
	settingsB := DefaultWebRtcSettings()
	settingsA.Log = NewNoopLogger()
	settingsB.Log = NewNoopLogger()
	settingsA.IceServerUrls = nil
	settingsB.IceServerUrls = nil
	settingsA.UseEgressOnlyIceInterfaces = true
	settingsB.UseEgressOnlyIceInterfaces = true
	settingsA.EnableSctpSnap = true
	settingsB.EnableSctpSnap = false

	signalPipeA := newSignalPipe(nil)
	signalPipeB := newSignalPipe(nil)
	managerA := NewWebRtcManager(ctx, signalPipeA, settingsA)
	managerB := NewWebRtcManager(ctx, signalPipeB, settingsB)
	signalPipeA.SetSignalReceiver(managerB)
	signalPipeB.SetSignalReceiver(managerA)

	peerIdA := NewId()
	peerIdB := NewId()
	streamId := NewId()
	passive, err := managerB.NewP2pConnPassive(
		ctx,
		NewTransferPath(peerIdB, peerIdA, streamId),
	)
	AssertEqual(t, err, nil)
	defer passive.Close()
	active, err := managerA.NewP2pConnActive(
		ctx,
		NewTransferPath(peerIdA, peerIdB, streamId),
	)
	AssertEqual(t, err, nil)
	defer active.Close()

	payload := []byte("mixed SNAP compatibility")
	AssertEqual(t, active.SetWriteDeadline(time.Now().Add(5*time.Second)), nil)
	received := make(chan []byte, 1)
	go func() {
		b := make([]byte, len(payload))
		if _, readErr := io.ReadFull(passive, b); readErr == nil {
			received <- b
		}
	}()
	_, err = active.Write(payload)
	AssertEqual(t, err, nil)
	select {
	case b := <-received:
		AssertEqual(t, b, payload)
	case <-ctx.Done():
		t.Fatal("SNAP-enabled peer did not fall back with a disabled peer")
	}
}

func TestWebRtcSctpZeroChecksumMixedCompatibility(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	settingsA := DefaultWebRtcSettings()
	settingsB := DefaultWebRtcSettings()
	settingsA.Log = NewNoopLogger()
	settingsB.Log = NewNoopLogger()
	settingsA.IceServerUrls = nil
	settingsB.IceServerUrls = nil
	settingsA.UseEgressOnlyIceInterfaces = true
	settingsB.UseEgressOnlyIceInterfaces = true
	settingsA.EnableSctpZeroChecksum = true
	settingsB.EnableSctpZeroChecksum = false

	signalPipeA := newSignalPipe(nil)
	signalPipeB := newSignalPipe(nil)
	managerA := NewWebRtcManager(ctx, signalPipeA, settingsA)
	managerB := NewWebRtcManager(ctx, signalPipeB, settingsB)
	signalPipeA.SetSignalReceiver(managerB)
	signalPipeB.SetSignalReceiver(managerA)

	peerIdA := NewId()
	peerIdB := NewId()
	streamId := NewId()
	passive, err := managerB.NewP2pConnPassive(
		ctx,
		NewTransferPath(peerIdB, peerIdA, streamId),
	)
	AssertEqual(t, err, nil)
	defer passive.Close()
	active, err := managerA.NewP2pConnActive(
		ctx,
		NewTransferPath(peerIdA, peerIdB, streamId),
	)
	AssertEqual(t, err, nil)
	defer active.Close()

	payload := []byte("mixed RFC 9653 compatibility")
	AssertEqual(t, active.SetWriteDeadline(time.Now().Add(5*time.Second)), nil)
	AssertEqual(t, passive.SetReadDeadline(time.Now().Add(5*time.Second)), nil)
	received := make(chan []byte, 1)
	go func() {
		b := make([]byte, len(payload))
		if _, readErr := io.ReadFull(passive, b); readErr == nil {
			received <- b
		}
	}()
	_, err = active.Write(payload)
	AssertEqual(t, err, nil)
	select {
	case b := <-received:
		AssertEqual(t, b, payload)
	case <-ctx.Done():
		t.Fatal("zero-checksum-enabled peer did not interoperate with a disabled peer")
	}

	metadata := func(conn WebRtcConn) *webrtc.SCTPTransportMetadata {
		for _, stat := range conn.(*peerConn).pc.GetStats() {
			if sctpStat, ok := stat.(webrtc.SCTPTransportStats); ok {
				return sctpStat.Metadata
			}
		}
		return nil
	}
	activeMetadata := metadata(active)
	passiveMetadata := metadata(passive)
	if activeMetadata == nil || passiveMetadata == nil {
		t.Fatal("missing SCTP negotiation metadata")
	}
	// Enabling the extension means "I can receive zero checksums." The mixed
	// peer therefore sends zero toward the enabled endpoint but continues to
	// receive normal CRC32c in the other direction. This directional
	// negotiation is the backwards-compatible behavior required by RFC 9653.
	AssertEqual(t, activeMetadata.ZeroChecksumReceivingEnabled, true)
	AssertEqual(t, activeMetadata.ZeroChecksumSendingEnabled, false)
	AssertEqual(t, passiveMetadata.ZeroChecksumReceivingEnabled, false)
	AssertEqual(t, passiveMetadata.ZeroChecksumSendingEnabled, true)
}

// CONNECT_WEBRTC_LATENCY_MEASURE=1 enables a manual warm-factory
// data-channel-ready comparison. It measures through the first successful
// detached write/read, so ICE, DTLS, SCTP, and DCEP are all complete.
func TestWebRtcSctpSnapReadyLatencyMeasurement(t *testing.T) {
	if os.Getenv("CONNECT_WEBRTC_LATENCY_MEASURE") == "" {
		t.Skip("set CONNECT_WEBRTC_LATENCY_MEASURE=1")
	}

	for _, enableSnap := range []bool{false, true} {
		t.Run(fmt.Sprintf("snap=%t", enableSnap), func(t *testing.T) {
			const pairCount = 25
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			settingsA := DefaultWebRtcSettings()
			settingsB := DefaultWebRtcSettings()
			settingsA.Log = NewNoopLogger()
			settingsB.Log = NewNoopLogger()
			settingsA.IceServerUrls = nil
			settingsB.IceServerUrls = nil
			settingsA.MaxPeerConnectionCount = 0
			settingsB.MaxPeerConnectionCount = 0
			settingsA.UseEgressOnlyIceInterfaces = true
			settingsB.UseEgressOnlyIceInterfaces = true
			if os.Getenv("CONNECT_WEBRTC_ORDERED") != "" {
				settingsA.DataChannelOrdered = true
				settingsB.DataChannelOrdered = true
			}
			settingsA.EnableSctpSnap = enableSnap
			settingsB.EnableSctpSnap = enableSnap

			signalPipeA := newSignalPipe(nil)
			signalPipeB := newSignalPipe(nil)
			managerA := NewWebRtcManager(ctx, signalPipeA, settingsA)
			managerB := NewWebRtcManager(ctx, signalPipeB, settingsB)
			signalPipeA.SetSignalReceiver(managerB)
			signalPipeB.SetSignalReceiver(managerA)

			peerIdA := NewId()
			peerIdB := NewId()
			latencies := make([]time.Duration, 0, pairCount)
			for range pairCount {
				streamId := NewId()
				passive, err := managerB.NewP2pConnPassive(
					ctx,
					NewTransferPath(peerIdB, peerIdA, streamId),
				)
				AssertEqual(t, err, nil)

				start := time.Now()
				active, err := managerA.NewP2pConnActive(
					ctx,
					NewTransferPath(peerIdA, peerIdB, streamId),
				)
				AssertEqual(t, err, nil)
				AssertEqual(t, active.SetWriteDeadline(time.Now().Add(5*time.Second)), nil)
				AssertEqual(t, passive.SetReadDeadline(time.Now().Add(5*time.Second)), nil)
				readDone := make(chan error, 1)
				go func() {
					var b [1]byte
					_, readErr := passive.Read(b[:])
					readDone <- readErr
				}()
				_, err = active.Write([]byte{1})
				AssertEqual(t, err, nil)
				AssertEqual(t, <-readDone, nil)
				latencies = append(latencies, time.Since(start))
				active.Close()
				passive.Close()
			}

			sort.Slice(latencies, func(i, j int) bool {
				return latencies[i] < latencies[j]
			})
			t.Logf(
				"SNAP=%t ready latency: median=%s p95=%s min=%s max=%s",
				enableSnap,
				latencies[len(latencies)/2],
				latencies[(len(latencies)*95-1)/100],
				latencies[0],
				latencies[len(latencies)-1],
			)
		})
	}
}

func TestWebRtcSharedBudgetAdmissionIsExactAcrossManagers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const managerCount = 16
	const admittedCount = 2
	reservationSize := kib(128)
	budget := NewTransferMemoryBudget(admittedCount * reservationSize)

	managers := make([]*WebRtcManager, 0, managerCount)
	for range managerCount {
		settings := DefaultWebRtcSettings()
		settings.Log = NewNoopLogger()
		settings.IceServerUrls = nil
		settings.ReceiveBufferSize = reservationSize
		settings.MemoryBudget = budget
		settings.MaxPeerConnectionCount = 0
		managers = append(managers, NewWebRtcManager(ctx, &testing_noopSignalSender{}, settings))
	}

	start := make(chan struct{})
	results := make(chan WebRtcConn, managerCount)
	var wg sync.WaitGroup
	for _, manager := range managers {
		wg.Add(1)
		go func(manager *WebRtcManager) {
			defer wg.Done()
			<-start
			conn, err := manager.NewP2pConnActive(
				ctx,
				NewTransferPath(NewId(), NewId(), NewId()),
			)
			if err == nil {
				results <- conn
				return
			}
			var admissionErr *peerConnectionAdmissionError
			if !errors.As(err, &admissionErr) {
				t.Errorf("unexpected setup error: %v", err)
			}
		}(manager)
	}
	close(start)
	wg.Wait()
	close(results)

	conns := make([]WebRtcConn, 0, admittedCount)
	for conn := range results {
		conns = append(conns, conn)
	}
	AssertEqual(t, len(conns), admittedCount)
	AssertEqual(t, budget.UsedByteCount(), admittedCount*reservationSize)

	notify := budget.CapacityNotify()
	conns[0].Close()
	select {
	case <-notify:
	case <-ctx.Done():
		t.Fatal("peer teardown did not wake shared-budget waiters")
	}
	for _, conn := range conns[1:] {
		conn.Close()
	}
	for budget.UsedByteCount() != 0 {
		select {
		case <-ctx.Done():
			t.Fatalf("peer reservations leaked: used=%d", budget.UsedByteCount())
		case <-time.After(10 * time.Millisecond):
		}
	}
	reserved, released := budget.Counts()
	AssertEqual(t, reserved, released)
}

func TestWebRtcPrioritizedNetworkPeerPreemptsWithoutRaisingAdmissionBounds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	settings := DefaultWebRtcSettings()
	settings.Log = NewNoopLogger()
	settings.IceServerUrls = nil
	settings.ReceiveBufferSize = kib(128)
	settings.MemoryBudget = NewTransferMemoryBudget(settings.ReceiveBufferSize)
	settings.MaxPeerConnectionCount = 1
	manager := NewWebRtcManager(ctx, &testing_noopSignalSender{}, settings)

	backgroundPeerId := NewId()
	backgroundConn, err := manager.NewP2pConnActive(
		ctx,
		NewTransferPath(NewId(), backgroundPeerId, NewId()),
	)
	if err != nil {
		t.Fatal(err)
	}
	background := backgroundConn.(*peerConn)
	if got := settings.MemoryBudget.UsedByteCount(); got != settings.ReceiveBufferSize {
		t.Fatalf("background reservation = %d, want %d", got, settings.ReceiveBufferSize)
	}

	priorityPeerId := NewId()
	manager.PrioritizePeer(priorityPeerId)
	select {
	case <-background.ctx.Done():
	case <-ctx.Done():
		t.Fatal("priority did not retire the bounded non-priority connection")
	}

	// The canceled connection releases asynchronously. While that happens,
	// another speculative waiter must not steal the selected peer's slot.
	if _, err := manager.NewP2pConnActive(
		ctx,
		NewTransferPath(NewId(), NewId(), NewId()),
	); err == nil {
		t.Fatal("non-priority admission stole a pending priority slot")
	}

	var priority *peerConn
	deadline := time.Now().Add(5 * time.Second)
	for priority == nil {
		var priorityConn WebRtcConn
		priorityConn, err = manager.NewP2pConnActive(
			ctx,
			NewTransferPath(NewId(), priorityPeerId, NewId()),
		)
		if err == nil {
			priority = priorityConn.(*peerConn)
			break
		}
		var admissionErr *peerConnectionAdmissionError
		if !errors.As(err, &admissionErr) {
			t.Fatalf("priority admission error = %v", err)
		}
		if deadline.Before(time.Now()) {
			t.Fatalf("priority never consumed released slot: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	defer priority.Close()

	manager.stateLock.Lock()
	priorityUntil := priority.priorityUntil
	liveCount := len(manager.peerConns)
	manager.stateLock.Unlock()
	if !time.Now().Before(priorityUntil) {
		t.Fatal("admitted selected peer did not retain bounded priority")
	}
	if liveCount != 1 {
		t.Fatalf("live peer connections = %d, want hard cap 1", liveCount)
	}
	if got := settings.MemoryBudget.UsedByteCount(); got != settings.ReceiveBufferSize {
		t.Fatalf("priority reservation = %d, want unchanged hard bound %d", got, settings.ReceiveBufferSize)
	}
}

func TestAuthenticatedNetworkSignalPreemptsFullPeerAdmission(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	settings := DefaultWebRtcSettings()
	settings.Log = NewNoopLogger()
	settings.IceServerUrls = nil
	settings.ReceiveBufferSize = kib(128)
	settings.MemoryBudget = NewTransferMemoryBudget(settings.ReceiveBufferSize)
	settings.MaxPeerConnectionCount = 1
	manager := NewWebRtcManager(ctx, &testing_noopSignalSender{}, settings)

	backgroundConn, err := manager.NewP2pConnActive(
		ctx,
		NewTransferPath(NewId(), NewId(), NewId()),
	)
	if err != nil {
		t.Fatal(err)
	}
	background := backgroundConn.(*peerConn)
	defer background.Close()

	dispatcher := newTestingSignalDispatcher(ctx, cancel, manager, 1, 2)
	defer dispatcher.Close()
	source := SourceId(NewId())

	publicFrame := testingSignalFrame(t, NewId())
	dispatcher.Receive(
		source,
		[]*protocol.Frame{publicFrame},
		Peer{ProvideMode: protocol.ProvideMode_Public},
	)
	MessagePoolReturn(publicFrame.MessageBytes)
	select {
	case <-background.ctx.Done():
		t.Fatal("untrusted public signal preempted bounded peer admission")
	default:
	}

	networkFrame := testingSignalFrame(t, NewId())
	dispatcher.Receive(
		source,
		[]*protocol.Frame{networkFrame},
		Peer{ProvideMode: protocol.ProvideMode_Network},
	)
	MessagePoolReturn(networkFrame.MessageBytes)
	select {
	case <-background.ctx.Done():
	case <-ctx.Done():
		t.Fatal("authenticated network signal did not preempt full peer admission")
	}

	manager.stateLock.Lock()
	_, prioritized := manager.prioritizedPeers[source.SourceId]
	_, pending := manager.pendingPrioritizedPeerSlot[source.SourceId]
	manager.stateLock.Unlock()
	if !prioritized || !pending {
		t.Fatal("network signal did not reserve the released slot for its peer")
	}
	if got := settings.MemoryBudget.TotalByteCount(); got != settings.ReceiveBufferSize {
		t.Fatalf("signal priority changed hard memory bound: %d", got)
	}
}

func TestWebRtcPrioritizedNetworkPeerDoesNotEvictWhenCapacityIsFree(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	settings := DefaultWebRtcSettings()
	settings.Log = NewNoopLogger()
	settings.IceServerUrls = nil
	settings.ReceiveBufferSize = kib(128)
	settings.MemoryBudget = NewTransferMemoryBudget(2 * settings.ReceiveBufferSize)
	settings.MaxPeerConnectionCount = 2
	manager := NewWebRtcManager(ctx, &testing_noopSignalSender{}, settings)

	backgroundConn, err := manager.NewP2pConnActive(
		ctx,
		NewTransferPath(NewId(), NewId(), NewId()),
	)
	if err != nil {
		t.Fatal(err)
	}
	background := backgroundConn.(*peerConn)
	defer background.Close()

	priorityPeerId := NewId()
	manager.PrioritizePeer(priorityPeerId)
	select {
	case <-background.ctx.Done():
		t.Fatal("priority evicted a connection despite free bounded capacity")
	default:
	}
	priority, err := manager.NewP2pConnActive(
		ctx,
		NewTransferPath(NewId(), priorityPeerId, NewId()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer priority.Close()
	if got := settings.MemoryBudget.UsedByteCount(); got != 2*settings.ReceiveBufferSize {
		t.Fatalf("reservations = %d, want %d", got, 2*settings.ReceiveBufferSize)
	}
}

func TestWebRtcPeerPriorityStateIsHardBounded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultWebRtcSettings()
	settings.Log = NewNoopLogger()
	settings.MaxPeerConnectionCount = 0
	settings.MemoryBudget = nil
	manager := NewWebRtcManager(ctx, &testing_noopSignalSender{}, settings)

	for range 4 * maxPeerConnectionPriorityCount {
		manager.PrioritizePeer(NewId())
	}
	manager.stateLock.Lock()
	defer manager.stateLock.Unlock()
	if got := len(manager.prioritizedPeers); maxPeerConnectionPriorityCount < got {
		t.Fatalf("prioritized peers retained = %d, max %d", got, maxPeerConnectionPriorityCount)
	}
	if got := len(manager.pendingPrioritizedPeerSlot); maxPeerConnectionPriorityCount < got {
		t.Fatalf("pending priority peers retained = %d, max %d", got, maxPeerConnectionPriorityCount)
	}
}

func TestWebRtcRepeatedConnectCloseReleasesAdmissionWithoutStall(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const cycles = 12
	const reservationSize = ByteCount(128 * 1024)
	settingsA := DefaultWebRtcSettings()
	settingsB := DefaultWebRtcSettings()
	settingsA.Log = NewNoopLogger()
	settingsB.Log = NewNoopLogger()
	settingsA.IceServerUrls = nil
	settingsB.IceServerUrls = nil
	settingsA.UseEgressOnlyIceInterfaces = true
	settingsB.UseEgressOnlyIceInterfaces = true
	settingsA.MaxPeerConnectionCount = 1
	settingsB.MaxPeerConnectionCount = 1
	settingsA.ReceiveBufferSize = reservationSize
	settingsB.ReceiveBufferSize = reservationSize
	settingsA.MemoryBudget = NewTransferMemoryBudget(reservationSize)
	settingsB.MemoryBudget = NewTransferMemoryBudget(reservationSize)

	signalPipeA := newSignalPipe(nil)
	signalPipeB := newSignalPipe(nil)
	managerA := NewWebRtcManager(ctx, signalPipeA, settingsA)
	managerB := NewWebRtcManager(ctx, signalPipeB, settingsB)
	signalPipeA.SetSignalReceiver(managerB)
	signalPipeB.SetSignalReceiver(managerA)

	peerIdA := NewId()
	peerIdB := NewId()
	maxTeardown := time.Duration(0)
	for cycle := range cycles {
		streamId := NewId()
		passive, err := managerB.NewP2pConnPassive(
			ctx,
			NewTransferPath(peerIdB, peerIdA, streamId),
		)
		AssertEqual(t, err, nil)
		active, err := managerA.NewP2pConnActive(
			ctx,
			NewTransferPath(peerIdA, peerIdB, streamId),
		)
		AssertEqual(t, err, nil)

		connectedDeadline := time.Now().Add(5 * time.Second)
		for !active.Connected() || !passive.Connected() {
			if time.Now().After(connectedDeadline) {
				t.Fatalf("cycle %d did not connect", cycle)
			}
			time.Sleep(time.Millisecond)
		}

		start := time.Now()
		AssertEqual(t, active.Close(), nil)
		AssertEqual(t, passive.Close(), nil)
		for {
			managerA.stateLock.Lock()
			countA := len(managerA.peerConns)
			managerA.stateLock.Unlock()
			managerB.stateLock.Lock()
			countB := len(managerB.peerConns)
			managerB.stateLock.Unlock()
			if countA == 0 && countB == 0 &&
				settingsA.MemoryBudget.UsedByteCount() == 0 &&
				settingsB.MemoryBudget.UsedByteCount() == 0 {
				break
			}
			if 5*time.Second < time.Since(start) {
				t.Fatalf(
					"cycle %d teardown stranded admission: peers=%d/%d budgets=%d/%d",
					cycle,
					countA,
					countB,
					settingsA.MemoryBudget.UsedByteCount(),
					settingsB.MemoryBudget.UsedByteCount(),
				)
			}
			time.Sleep(time.Millisecond)
		}
		maxTeardown = max(maxTeardown, time.Since(start))
	}
	t.Logf("%d connect/close cycles: maximum admission-release latency=%s", cycles, maxTeardown)
}

func TestClientSignalReceiverCoalescesAdjacentCandidatesOnly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	source := SourceId(NewId())
	streamId := NewId()
	receiver := &clientSignalReceiver{
		ctx:          ctx,
		cancel:       cancel,
		queueLimit:   8,
		queueMonitor: NewMonitor(),
		spaceMonitor: NewMonitor(),
	}

	candidateFrame := func(candidate string) *protocol.Frame {
		messageBytes, err := ProtoMarshal(&protocol.ExchangeSignals{
			StreamId: streamId.Bytes(),
			Signals: []*protocol.ExchangeSignal{
				{
					SignalType:   protocol.SignalType_IceCandidate,
					IceCandidate: []byte(candidate),
				},
			},
		})
		AssertEqual(t, err, nil)
		return &protocol.Frame{
			MessageType:  protocol.MessageType_TransferExchangeSignals,
			MessageBytes: messageBytes,
		}
	}

	sdpFrame := func(sdp string) *protocol.Frame {
		messageBytes, err := ProtoMarshal(&protocol.ExchangeSignals{
			StreamId: streamId.Bytes(),
			Signals: []*protocol.ExchangeSignal{
				{
					SignalType: protocol.SignalType_SdpOffer,
					Sdp:        []byte(sdp),
				},
			},
		})
		AssertEqual(t, err, nil)
		return &protocol.Frame{
			MessageType:  protocol.MessageType_TransferExchangeSignals,
			MessageBytes: messageBytes,
		}
	}

	frames := []*protocol.Frame{
		candidateFrame("c1"),
		sdpFrame("sdp"),
		candidateFrame("c2"),
	}
	defer func() {
		for _, frame := range frames {
			MessagePoolReturn(frame.MessageBytes)
		}
	}()

	for _, frame := range frames {
		received, err := newReceivedSignalFrame(source, frame)
		AssertEqual(t, err, nil)
		AssertEqual(t, receiver.enqueue(received), true)
	}

	readSignals := func() []*protocol.ExchangeSignal {
		received := receiver.dequeue()
		AssertNotEqual(t, received, nil)
		defer received.Close()
		err := received.prepareFrame()
		AssertEqual(t, err, nil)
		exchangeSignals := &protocol.ExchangeSignals{}
		err = ProtoUnmarshal(received.frame.MessageBytes, exchangeSignals)
		AssertEqual(t, err, nil)
		return exchangeSignals.Signals
	}

	signals := readSignals()
	AssertEqual(t, len(signals), 1)
	AssertEqual(t, signals[0].SignalType, protocol.SignalType_IceCandidate)
	AssertEqual(t, string(signals[0].IceCandidate), "c1")

	signals = readSignals()
	AssertEqual(t, len(signals), 1)
	AssertEqual(t, signals[0].SignalType, protocol.SignalType_SdpOffer)
	AssertEqual(t, string(signals[0].Sdp), "sdp")

	signals = readSignals()
	AssertEqual(t, len(signals), 1)
	AssertEqual(t, signals[0].SignalType, protocol.SignalType_IceCandidate)
	AssertEqual(t, string(signals[0].IceCandidate), "c2")
}

func TestClientSignalReceiverCoalescesAdjacentCandidates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	source := SourceId(NewId())
	streamId := NewId()
	receiver := &clientSignalReceiver{
		ctx:          ctx,
		cancel:       cancel,
		queueLimit:   8,
		queueMonitor: NewMonitor(),
		spaceMonitor: NewMonitor(),
	}

	candidateFrame := func(candidate string) *protocol.Frame {
		messageBytes, err := ProtoMarshal(&protocol.ExchangeSignals{
			StreamId: streamId.Bytes(),
			Signals: []*protocol.ExchangeSignal{
				{
					SignalType:   protocol.SignalType_IceCandidate,
					IceCandidate: []byte(candidate),
				},
			},
		})
		AssertEqual(t, err, nil)
		return &protocol.Frame{
			MessageType:  protocol.MessageType_TransferExchangeSignals,
			MessageBytes: messageBytes,
		}
	}

	frames := []*protocol.Frame{
		candidateFrame("c1"),
		candidateFrame("c2"),
	}
	defer func() {
		for _, frame := range frames {
			MessagePoolReturn(frame.MessageBytes)
		}
	}()

	for _, frame := range frames {
		received, err := newReceivedSignalFrame(source, frame)
		AssertEqual(t, err, nil)
		AssertEqual(t, receiver.enqueue(received), true)
	}

	received := receiver.dequeue()
	AssertNotEqual(t, received, nil)
	defer received.Close()
	err := received.prepareFrame()
	AssertEqual(t, err, nil)
	exchangeSignals := &protocol.ExchangeSignals{}
	err = ProtoUnmarshal(received.frame.MessageBytes, exchangeSignals)
	AssertEqual(t, err, nil)
	AssertEqual(t, len(exchangeSignals.Signals), 2)
	AssertEqual(t, string(exchangeSignals.Signals[0].IceCandidate), "c1")
	AssertEqual(t, string(exchangeSignals.Signals[1].IceCandidate), "c2")
}

func TestClientSignalReceiverCandidateCoalescingRemainsBounded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	source := SourceId(NewId())
	streamId := NewId()
	receiver := &clientSignalReceiver{
		ctx:          ctx,
		cancel:       cancel,
		queueLimit:   1,
		queueMonitor: NewMonitor(),
		spaceMonitor: NewMonitor(),
	}
	makeReceived := func(signals []*protocol.ExchangeSignal) *receivedSignalFrame {
		messageBytes, err := ProtoMarshal(&protocol.ExchangeSignals{
			StreamId: streamId.Bytes(),
			Signals:  signals,
		})
		AssertEqual(t, err, nil)
		received, err := newReceivedSignalFrame(source, &protocol.Frame{
			MessageType:  protocol.MessageType_TransferExchangeSignals,
			MessageBytes: messageBytes,
		})
		AssertEqual(t, err, nil)
		MessagePoolReturn(messageBytes)
		return received
	}

	firstSignals := make([]*protocol.ExchangeSignal, 0, maxBufferedRemoteIceCandidateCount)
	for i := range maxBufferedRemoteIceCandidateCount {
		firstSignals = append(firstSignals, &protocol.ExchangeSignal{
			SignalType:   protocol.SignalType_IceCandidate,
			IceCandidate: []byte(fmt.Sprintf("candidate-%d", i)),
		})
	}
	first := makeReceived(firstSignals)
	AssertEqual(t, receiver.enqueue(first), true)

	second := makeReceived([]*protocol.ExchangeSignal{{
		SignalType:   protocol.SignalType_IceCandidate,
		IceCandidate: []byte("candidate-overflow"),
	}})
	enqueued := make(chan bool, 1)
	go func() {
		enqueued <- receiver.enqueue(second)
	}()
	select {
	case <-enqueued:
		t.Fatal("candidate coalescing bypassed the bounded full-shard backpressure")
	case <-time.After(50 * time.Millisecond):
	}

	dequeued := receiver.dequeue()
	AssertEqual(t, dequeued, first)
	dequeued.Close()
	select {
	case ok := <-enqueued:
		AssertEqual(t, ok, true)
	case <-time.After(time.Second):
		t.Fatal("candidate enqueue did not resume after bounded capacity returned")
	}
	dequeued = receiver.dequeue()
	AssertEqual(t, dequeued, second)
	dequeued.Close()
}

func TestClientSignalReceiverQueueBackingStorageRemainsBounded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const queueLimit = 4
	receiver := &clientSignalReceiver{
		ctx:          ctx,
		cancel:       cancel,
		queueLimit:   queueLimit,
		queueMonitor: NewMonitor(),
		spaceMonitor: NewMonitor(),
	}
	defer receiver.Close()

	makeReceived := func() *receivedSignalFrame {
		return &receivedSignalFrame{
			frame: &protocol.Frame{
				MessageType: protocol.MessageType_TransferExchangeSignals,
			},
		}
	}
	for range queueLimit {
		AssertEqual(t, receiver.enqueue(makeReceived()), true)
	}

	// Keep the queue continuously nonempty while advancing it many times.
	// An append-only slice with a logical head still bounds live entries, but
	// retains one additional nil slot per iteration until the queue happens
	// to drain completely. A fixed ring must keep both len and cap bounded.
	for range 10_000 {
		received := receiver.dequeue()
		AssertNotEqual(t, received, nil)
		received.Close()
		AssertEqual(t, receiver.enqueue(makeReceived()), true)
	}

	receiver.queueLock.Lock()
	backingLen := len(receiver.receiveFrames)
	backingCap := cap(receiver.receiveFrames)
	liveCount := receiver.receiveFrameCount
	receiver.queueLock.Unlock()
	AssertEqual(t, liveCount, queueLimit)
	AssertEqual(t, backingLen, queueLimit)
	AssertEqual(t, backingCap, queueLimit)
}

func TestClientSignalReceiverDecodedValueOwnsFrameBytes(t *testing.T) {
	streamId := NewId()
	messageBytes, err := ProtoMarshal(&protocol.ExchangeSignals{
		StreamId:     streamId.Bytes(),
		ResetSignals: true,
		Signals: []*protocol.ExchangeSignal{
			{
				SignalType:   protocol.SignalType_IceCandidate,
				IceCandidate: []byte("candidate"),
			},
		},
	})
	AssertEqual(t, err, nil)

	received, err := newReceivedSignalFrame(SourceId(NewId()), &protocol.Frame{
		MessageType:  protocol.MessageType_TransferExchangeSignals,
		MessageBytes: messageBytes,
	})
	AssertEqual(t, err, nil)
	defer received.Close()

	// The transfer callback owns messageBytes only until it returns. Destroy
	// that storage now and prove protobuf decoding produced an independent
	// value safe for asynchronous/sharded dispatch.
	clear(messageBytes)
	MessagePoolReturn(messageBytes)

	AssertEqual(t, Id(received.exchangeSignals.StreamId), streamId)
	AssertEqual(t, received.exchangeSignals.ResetSignals, true)
	AssertEqual(t, len(received.exchangeSignals.Signals), 1)
	AssertEqual(t, string(received.exchangeSignals.Signals[0].IceCandidate), "candidate")
}

type testingBlockingSignalReceiver struct {
	blockSource Id
	entered     chan struct{}
	release     chan struct{}
	other       chan struct{}
}

func (self *testingBlockingSignalReceiver) ReceiveSignal(TransferPath, *protocol.Frame) error {
	return nil
}

func (self *testingBlockingSignalReceiver) ReceiveExchangeSignals(source TransferPath, _ *protocol.ExchangeSignals) error {
	if source.SourceId == self.blockSource {
		select {
		case self.entered <- struct{}{}:
		default:
		}
		<-self.release
		return nil
	}
	select {
	case self.other <- struct{}{}:
	default:
	}
	return nil
}

func newTestingSignalDispatcher(
	ctx context.Context,
	cancel context.CancelFunc,
	receiver SignalReceiver,
	workerCount int,
	queueLimit int,
) *clientSignalDispatcher {
	client := &Client{log: NewNoopLogger()}
	dispatcher := &clientSignalDispatcher{
		client:   client,
		receiver: receiver,
		ctx:      ctx,
		cancel:   cancel,
		shards:   make([]*clientSignalReceiver, 0, workerCount),
	}
	for range workerCount {
		shard := &clientSignalReceiver{
			client:       client,
			receiver:     receiver,
			ctx:          ctx,
			cancel:       cancel,
			queueLimit:   queueLimit,
			queueMonitor: NewMonitor(),
			spaceMonitor: NewMonitor(),
		}
		dispatcher.shards = append(dispatcher.shards, shard)
		shard.start()
	}
	return dispatcher
}

func testingSignalFrame(t *testing.T, streamId Id) *protocol.Frame {
	messageBytes, err := ProtoMarshal(&protocol.ExchangeSignals{
		StreamId: streamId.Bytes(),
		Signals: []*protocol.ExchangeSignal{
			{SignalType: protocol.SignalType_WaitingForSdpOffer},
		},
	})
	AssertEqual(t, err, nil)
	return &protocol.Frame{
		MessageType:  protocol.MessageType_TransferExchangeSignals,
		MessageBytes: messageBytes,
	}
}

func TestClientSignalDispatcherStalledPeerDoesNotBlockOtherShard(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	streamA := NewId()
	sourceA := SourceId(NewId())
	shardA := receivedSignalShardIndex(&receivedSignalFrame{
		keyValid: true,
		key: receivedSignalFrameKey{
			source:   sourceA,
			streamId: streamA,
		},
	}, 2)
	var sourceB TransferPath
	var streamB Id
	for {
		sourceB = SourceId(NewId())
		streamB = NewId()
		if receivedSignalShardIndex(&receivedSignalFrame{
			keyValid: true,
			key: receivedSignalFrameKey{
				source:   sourceB,
				streamId: streamB,
			},
		}, 2) != shardA {
			break
		}
	}

	receiver := &testingBlockingSignalReceiver{
		blockSource: sourceA.SourceId,
		entered:     make(chan struct{}, 1),
		release:     make(chan struct{}),
		other:       make(chan struct{}, 1),
	}
	dispatcher := newTestingSignalDispatcher(ctx, cancel, receiver, 2, 2)
	defer dispatcher.Close()

	frameA := testingSignalFrame(t, streamA)
	dispatcher.handleControlFrame(sourceA, frameA)
	MessagePoolReturn(frameA.MessageBytes)
	select {
	case <-receiver.entered:
	case <-time.After(time.Second):
		t.Fatal("blocking peer did not enter its signal callback")
	}

	frameB := testingSignalFrame(t, streamB)
	dispatcher.handleControlFrame(sourceB, frameB)
	MessagePoolReturn(frameB.MessageBytes)
	select {
	case <-receiver.other:
	case <-time.After(time.Second):
		t.Fatal("independent peer was blocked behind a stalled signal callback")
	}
	close(receiver.release)
}

func TestClientSignalDispatcherFullShardBackpressuresReceiveCallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	source := SourceId(NewId())
	streamId := NewId()
	receiver := &testingBlockingSignalReceiver{
		blockSource: source.SourceId,
		entered:     make(chan struct{}, 1),
		release:     make(chan struct{}),
		other:       make(chan struct{}, 1),
	}
	dispatcher := newTestingSignalDispatcher(ctx, cancel, receiver, 1, 1)
	defer dispatcher.Close()

	first := testingSignalFrame(t, streamId)
	dispatcher.handleControlFrame(source, first)
	MessagePoolReturn(first.MessageBytes)
	select {
	case <-receiver.entered:
	case <-time.After(time.Second):
		t.Fatal("blocking peer did not enter its signal callback")
	}

	second := testingSignalFrame(t, streamId)
	dispatcher.handleControlFrame(source, second)
	MessagePoolReturn(second.MessageBytes)

	thirdReturned := make(chan struct{})
	third := testingSignalFrame(t, streamId)
	go func() {
		dispatcher.handleControlFrame(source, third)
		MessagePoolReturn(third.MessageBytes)
		close(thirdReturned)
	}()
	select {
	case <-thirdReturned:
		t.Fatal("full signal shard did not preserve receive callback backpressure")
	case <-time.After(50 * time.Millisecond):
	}

	close(receiver.release)
	select {
	case <-thirdReturned:
	case <-time.After(time.Second):
		t.Fatal("receive callback did not resume when shard capacity returned")
	}
}

func TestWebRtcManagerPeerConnectionFactoryIsLazy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	settings := DefaultWebRtcSettings()
	settings.Log = NewNoopLogger()
	settings.IceServerUrls = nil
	manager := NewWebRtcManager(ctx, &testing_noopSignalSender{}, settings)
	if manager.networkChangeWorker != nil {
		t.Fatal("idle manager eagerly started a network-change worker")
	}

	AssertEqual(t, manager.peerConnectionFactoryInitialized, false)
	conn, err := manager.NewP2pConnActive(
		ctx,
		NewTransferPath(NewId(), NewId(), NewId()),
	)
	AssertEqual(t, err, nil)
	AssertEqual(t, manager.peerConnectionFactoryInitialized, true)
	conn.Close()

	cancel()
	factoryClosed := func() bool {
		manager.peerConnectionFactoryLock.Lock()
		defer manager.peerConnectionFactoryLock.Unlock()
		return manager.peerConnectionFactoryClosed
	}
	deadline := time.Now().Add(time.Second)
	for !factoryClosed() {
		if time.Now().After(deadline) {
			t.Fatal("manager factory did not close with its context")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestWebRtcManagerFactoryFailureRetriesAfterBoundedCooldown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	settings := DefaultWebRtcSettings()
	settings.Log = NewNoopLogger()
	settings.IceServerUrls = nil
	manager := NewWebRtcManager(ctx, &testing_noopSignalSender{}, settings)

	factoryErr := errors.New("transient factory failure")
	factoryCalls := 0
	manager.newPeerConnectionFactory = func(settings *WebRtcSettings) (*webRtcPeerConnectionFactory, error) {
		factoryCalls++
		if factoryCalls == 1 {
			return nil, factoryErr
		}
		return newWebRtcPeerConnectionFactory(settings)
	}
	newActive := func() (WebRtcConn, error) {
		return manager.NewP2pConnActive(
			ctx,
			NewTransferPath(NewId(), NewId(), NewId()),
		)
	}
	if _, err := newActive(); !errors.Is(err, factoryErr) {
		t.Fatalf("first factory error = %v, want %v", err, factoryErr)
	}
	if _, err := newActive(); !errors.Is(err, factoryErr) {
		t.Fatalf("cached factory error = %v, want %v", err, factoryErr)
	}
	AssertEqual(t, factoryCalls, 1)

	manager.peerConnectionFactoryLock.Lock()
	manager.peerConnectionFactoryRetryTime = time.Now().Add(-time.Second)
	manager.peerConnectionFactoryLock.Unlock()
	conn, err := newActive()
	AssertEqual(t, err, nil)
	defer conn.Close()
	AssertEqual(t, factoryCalls, 2)
}

func TestWebRtcManagerCanceledStreamDoesNotAllocatePeerConnection(t *testing.T) {
	managerCtx, managerCancel := context.WithCancel(context.Background())
	defer managerCancel()
	streamCtx, streamCancel := context.WithCancel(managerCtx)
	streamCancel()

	settings := DefaultWebRtcSettings()
	settings.Log = NewNoopLogger()
	settings.IceServerUrls = nil
	budget := NewTransferMemoryBudget(settings.ReceiveBufferSize)
	settings.MemoryBudget = budget
	manager := NewWebRtcManager(managerCtx, &testing_noopSignalSender{}, settings)

	conn, err := manager.NewP2pConnActive(
		streamCtx,
		NewTransferPath(NewId(), NewId(), NewId()),
	)
	AssertEqual(t, conn, nil)
	AssertEqual(t, errors.Is(err, context.Canceled), true)
	AssertEqual(t, manager.peerConnectionFactoryInitialized, false)
	AssertEqual(t, budget.UsedByteCount(), ByteCount(0))
}

func TestWebRtcConnectedCallbackDropsLateAndPostUnsubscribeDelivery(t *testing.T) {
	var lock sync.Mutex
	var states []bool
	callback := &connectedCallback{
		callback: func(connected bool) {
			lock.Lock()
			states = append(states, connected)
			lock.Unlock()
		},
	}

	callback.deliver(2, false)
	callback.deliver(1, true)
	callback.deliver(2, false)
	callback.close()
	callback.deliver(3, true)

	lock.Lock()
	defer lock.Unlock()
	AssertEqual(t, states, []bool{false})
}

func TestWebRtcTerminalStateReleasesPeerInsteadOfStrandingSlot(t *testing.T) {
	newConn := func() (*peerConn, context.CancelFunc) {
		ctx, cancel := context.WithCancel(context.Background())
		return &peerConn{
			ctx:                ctx,
			cancel:             cancel,
			log:                NewNoopLogger(),
			connectedMonitor:   NewMonitor(),
			immediateReconnect: make(chan struct{}),
		}, cancel
	}

	t.Run("ice failed", func(t *testing.T) {
		conn, cancel := newConn()
		defer cancel()
		conn.handleICEConnectionState(webrtc.ICEConnectionStateFailed)
		select {
		case <-conn.ctx.Done():
		default:
			t.Fatal("terminal ICE state did not cancel the peer")
		}
		select {
		case <-conn.ImmediateReconnect():
			t.Fatal("generic ICE failure bypassed reconnect backoff")
		default:
		}
	})

	t.Run("dtls/sctp failed", func(t *testing.T) {
		conn, cancel := newConn()
		defer cancel()
		conn.handlePeerConnectionState(webrtc.PeerConnectionStateFailed)
		select {
		case <-conn.ctx.Done():
		default:
			t.Fatal("terminal peer state did not cancel the peer")
		}
		select {
		case <-conn.ImmediateReconnect():
			t.Fatal("generic peer failure bypassed reconnect backoff")
		default:
		}
	})

	t.Run("local close is not a retry bypass", func(t *testing.T) {
		conn, cancel := newConn()
		cancel()
		conn.handlePeerConnectionState(webrtc.PeerConnectionStateClosed)
		select {
		case <-conn.ImmediateReconnect():
			t.Fatal("local close incorrectly bypassed reconnect backoff")
		default:
		}
	})
}

func TestWebRtcManagerNetworkChangeRetiresConnectionsAndFactory(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	settings := DefaultWebRtcSettings()
	settings.Log = NewNoopLogger()
	settings.IceServerUrls = nil
	settings.MaxPeerConnectionCount = 0
	manager := NewWebRtcManager(ctx, &testing_noopSignalSender{}, settings)

	conn, err := manager.NewP2pConnActive(
		ctx,
		NewTransferPath(NewId(), NewId(), NewId()),
	)
	AssertEqual(t, err, nil)
	oldFactory := func() *webRtcPeerConnectionFactory {
		manager.peerConnectionFactoryLock.Lock()
		defer manager.peerConnectionFactoryLock.Unlock()
		return manager.peerConnectionFactory
	}()
	AssertNotEqual(t, oldFactory, nil)

	reconnect := conn.ImmediateReconnect()
	manager.networkChanged()
	select {
	case <-reconnect:
	default:
		t.Fatal("network change did not persist an immediate-reconnect signal")
	}
	select {
	case <-conn.(*peerConn).ctx.Done():
	default:
		t.Fatal("network change did not retire the old peer connection")
	}

	manager.peerConnectionFactoryLock.Lock()
	AssertEqual(t, manager.peerConnectionFactoryInitialized, false)
	AssertEqual(t, manager.peerConnectionFactory, nil)
	manager.peerConnectionFactoryLock.Unlock()

	replacement, err := manager.NewP2pConnActive(
		ctx,
		NewTransferPath(NewId(), NewId(), NewId()),
	)
	AssertEqual(t, err, nil)
	defer replacement.Close()
	newFactory := func() *webRtcPeerConnectionFactory {
		manager.peerConnectionFactoryLock.Lock()
		defer manager.peerConnectionFactoryLock.Unlock()
		return manager.peerConnectionFactory
	}()
	AssertNotEqual(t, newFactory, nil)
	if newFactory == oldFactory {
		t.Fatal("network change reused ICE state bound to the old interfaces")
	}
}

func TestWebRtcNetworkChangeDispatchDoesNotBlockHostCallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	settings := DefaultWebRtcSettings()
	settings.Log = NewNoopLogger()
	settings.IceServerUrls = nil
	manager := NewWebRtcManager(ctx, &testing_noopSignalSender{}, settings)
	conn, err := manager.NewP2pConnActive(
		ctx,
		NewTransferPath(NewId(), NewId(), NewId()),
	)
	AssertEqual(t, err, nil)
	defer conn.Close()

	// Simulate teardown already holding manager state. The OS path callback
	// must enqueue/coalesce and return instead of blocking its UI/extension
	// thread behind that work.
	manager.stateLock.Lock()
	dispatched := make(chan struct{})
	go func() {
		for range 32 {
			manager.networkChangeWorker.Dispatch()
		}
		close(dispatched)
	}()
	select {
	case <-dispatched:
	case <-time.After(100 * time.Millisecond):
		manager.stateLock.Unlock()
		t.Fatal("network-change dispatch blocked behind peer teardown")
	}
	manager.stateLock.Unlock()

	select {
	case <-conn.ImmediateReconnect():
	case <-time.After(time.Second):
		t.Fatal("coalesced network-change worker did not retire the connection")
	}
}

func TestWebRtcInvalidSdpAndEarlyCandidateDoNotPoisonRetransmit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	settingsA := DefaultWebRtcSettings()
	settingsB := DefaultWebRtcSettings()
	settingsA.Log = NewNoopLogger()
	settingsB.Log = NewNoopLogger()
	settingsA.IceServerUrls = nil
	settingsB.IceServerUrls = nil

	signalPipeA := newSignalPipe(nil)
	signalPipeB := newSignalPipe(nil)
	managerA := NewWebRtcManager(ctx, signalPipeA, settingsA)
	managerB := NewWebRtcManager(ctx, signalPipeB, settingsB)
	signalPipeA.SetSignalReceiver(managerB)
	signalPipeB.SetSignalReceiver(managerA)

	peerIdA := NewId()
	peerIdB := NewId()
	streamId := NewId()
	passiveWebRtcConn, err := managerB.NewP2pConnPassive(
		ctx,
		NewTransferPath(peerIdB, peerIdA, streamId),
	)
	AssertEqual(t, err, nil)
	defer passiveWebRtcConn.Close()
	passive := passiveWebRtcConn.(*peerConn)

	invalidSdp, err := json.Marshal(&webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  "not valid SDP",
	})
	AssertEqual(t, err, nil)
	if err := passive.ReceiveSignalFromPeer(&protocol.ExchangeSignal{
		SignalType: protocol.SignalType_SdpOffer,
		Sdp:        invalidSdp,
	}); err == nil {
		t.Fatal("invalid semantic SDP was accepted")
	}
	AssertEqual(t, passive.offerSignal(), nil)

	earlyCandidate, err := json.Marshal(webrtc.ICECandidateInit{
		Candidate: "candidate:1 1 udp 1 192.0.2.1 9999 typ host",
	})
	AssertEqual(t, err, nil)
	AssertEqual(t, passive.ReceiveSignalFromPeer(&protocol.ExchangeSignal{
		SignalType:   protocol.SignalType_IceCandidate,
		IceCandidate: earlyCandidate,
	}), nil)
	AssertEqual(t, len(passive.remoteIceCandidateBuffer), 1)

	active, err := managerA.NewP2pConnActive(
		ctx,
		NewTransferPath(peerIdA, peerIdB, streamId),
	)
	AssertEqual(t, err, nil)
	defer active.Close()

	deadline := time.Now().Add(5 * time.Second)
	for !active.Connected() || !passive.Connected() {
		if time.Now().After(deadline) {
			t.Fatal("valid retransmit did not recover after invalid SDP/early ICE")
		}
		time.Sleep(10 * time.Millisecond)
	}
	AssertEqual(t, len(passive.remoteIceCandidateBuffer), 0)
	AssertEqual(t, passive.remoteIceCandidateBufferBytes, 0)
}

func TestWebRtcEarlyCandidateBufferIsBounded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	settings := DefaultWebRtcSettings()
	settings.Log = NewNoopLogger()
	settings.IceServerUrls = nil
	manager := NewWebRtcManager(ctx, &testing_noopSignalSender{}, settings)
	webRtcConn, err := manager.NewP2pConnPassive(
		ctx,
		NewTransferPath(NewId(), NewId(), NewId()),
	)
	AssertEqual(t, err, nil)
	defer webRtcConn.Close()
	conn := webRtcConn.(*peerConn)

	for i := range 4 * maxBufferedRemoteIceCandidateCount {
		candidateBytes, marshalErr := json.Marshal(webrtc.ICECandidateInit{
			Candidate: fmt.Sprintf(
				"candidate:%d 1 udp 1 192.0.2.1 %d typ host %s",
				i+1,
				10000+i,
				strings.Repeat("x", 1024),
			),
		})
		AssertEqual(t, marshalErr, nil)
		AssertEqual(t, conn.ReceiveSignalFromPeer(&protocol.ExchangeSignal{
			SignalType:   protocol.SignalType_IceCandidate,
			IceCandidate: candidateBytes,
		}), nil)
	}
	if maxBufferedRemoteIceCandidateCount < len(conn.remoteIceCandidateBuffer) {
		t.Fatalf("candidate count was unbounded: %d", len(conn.remoteIceCandidateBuffer))
	}
	if maxBufferedRemoteIceCandidateBytes < conn.remoteIceCandidateBufferBytes {
		t.Fatalf("candidate bytes were unbounded: %d", conn.remoteIceCandidateBufferBytes)
	}
}

func TestWebRtcMalformedCandidateDoesNotSuppressBatchRemainder(t *testing.T) {
	peerId := NewId()
	streamId := NewId()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn := &peerConn{
		ctx:                ctx,
		cancel:             cancel,
		log:                NewNoopLogger(),
		immediateReconnect: make(chan struct{}),
	}
	key := peerConnKey{PeerId: peerId, StreamId: streamId}
	manager := &WebRtcManager{
		log:       NewNoopLogger(),
		peerConns: map[peerConnKey]*peerConn{key: conn},
	}
	validCandidate, err := json.Marshal(webrtc.ICECandidateInit{
		Candidate: "candidate:1 1 udp 1 192.0.2.1 9999 typ host",
	})
	AssertEqual(t, err, nil)
	err = manager.ReceiveExchangeSignals(
		SourceId(peerId),
		&protocol.ExchangeSignals{
			StreamId: streamId.Bytes(),
			Signals: []*protocol.ExchangeSignal{
				{
					SignalType:   protocol.SignalType_IceCandidate,
					IceCandidate: []byte("{"),
				},
				{
					SignalType:   protocol.SignalType_IceCandidate,
					IceCandidate: validCandidate,
				},
			},
		},
	)
	if err == nil {
		t.Fatal("malformed candidate error was not reported")
	}
	AssertEqual(t, len(conn.remoteIceCandidateBuffer), 1)
}

type recordingSignalSender struct {
	lock    sync.Mutex
	batches []*protocol.ExchangeSignals
}

func (self *recordingSignalSender) SendSignal(
	_ TransferPath,
	frame *protocol.Frame,
	_ ...any,
) {
	exchangeSignals := &protocol.ExchangeSignals{}
	if err := ProtoUnmarshal(frame.MessageBytes, exchangeSignals); err != nil {
		panic(err)
	}
	self.lock.Lock()
	self.batches = append(self.batches, exchangeSignals)
	self.lock.Unlock()
}

func TestWebRtcSendsGatheredCandidatesInOneFrame(t *testing.T) {
	sender := &recordingSignalSender{}
	conn := &peerConn{
		key: peerConnKey{
			PeerId:   NewId(),
			StreamId: NewId(),
		},
		sourceId:     NewId(),
		active:       true,
		signalSender: sender,
	}
	candidates := make([]*webrtc.ICECandidate, 0, 2)
	for i := range 2 {
		candidates = append(candidates, &webrtc.ICECandidate{
			Foundation: fmt.Sprintf("%d", i+1),
			Priority:   1,
			Address:    fmt.Sprintf("192.0.2.%d", i+1),
			Protocol:   webrtc.ICEProtocolUDP,
			Port:       uint16(10000 + i),
			Typ:        webrtc.ICECandidateTypeHost,
			Component:  1,
		})
	}
	conn.sendIceCandidates(candidates)

	sender.lock.Lock()
	defer sender.lock.Unlock()
	AssertEqual(t, len(sender.batches), 1)
	AssertEqual(t, len(sender.batches[0].Signals), 2)
}

func TestWebRtcCandidateSendFramesRemainBounded(t *testing.T) {
	sender := &recordingSignalSender{}
	conn := &peerConn{
		key: peerConnKey{
			PeerId:   NewId(),
			StreamId: NewId(),
		},
		sourceId:     NewId(),
		active:       true,
		signalSender: sender,
	}
	candidates := make([]*webrtc.ICECandidate, 0, 2*maxIceCandidatesPerSignalFrame+1)
	for i := range 2*maxIceCandidatesPerSignalFrame + 1 {
		candidates = append(candidates, &webrtc.ICECandidate{
			Foundation: fmt.Sprintf("%d", i+1),
			Priority:   1,
			Address:    fmt.Sprintf("192.0.2.%d", i%254+1),
			Protocol:   webrtc.ICEProtocolUDP,
			Port:       uint16(10000 + i),
			Typ:        webrtc.ICECandidateTypeHost,
			Component:  1,
		})
	}
	conn.sendIceCandidates(candidates)

	sender.lock.Lock()
	defer sender.lock.Unlock()
	AssertEqual(t, len(sender.batches), 3)
	AssertEqual(t, len(sender.batches[0].Signals), maxIceCandidatesPerSignalFrame)
	AssertEqual(t, len(sender.batches[1].Signals), maxIceCandidatesPerSignalFrame)
	AssertEqual(t, len(sender.batches[2].Signals), 1)
}

func TestWebRtcEgressOnlyInterfaceViewIsBounded(t *testing.T) {
	iceNet, ok := newIceInterfaceNet(NewNoopLogger(), true)
	if !ok {
		t.Skip("host has no default-route IPv4 or IPv6 address")
	}
	interfaces, err := iceNet.Interfaces()
	AssertEqual(t, err, nil)
	if len(interfaces) == 0 || 2 < len(interfaces) {
		t.Fatalf("egress-only interface count is not bounded: %d", len(interfaces))
	}
	for _, ifc := range interfaces {
		addrs, addrErr := ifc.Addrs()
		AssertEqual(t, addrErr, nil)
		AssertEqual(t, len(addrs), 1)
	}
}

type signalPipe struct {
	stateLock      sync.Mutex
	signalReceiver SignalReceiver
	verbose        bool
}

func newSignalPipe(signalReceiver SignalReceiver) *signalPipe {
	return &signalPipe{
		signalReceiver: signalReceiver,
	}
}

func (self *signalPipe) SetSignalReceiver(signalReceiver SignalReceiver) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.signalReceiver = signalReceiver
}

func (self *signalPipe) SignalReceiver() SignalReceiver {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.signalReceiver
}

func (self *signalPipe) SendSignal(path TransferPath, signal *protocol.Frame, opts ...any) {
	signalReceiver := self.SignalReceiver()
	if signalReceiver != nil {
		if self.verbose {
			fmt.Printf("[signal][%s]%s\n", signal.MessageType, path)
		}
		signalReceiver.ReceiveSignal(path.SourceMask(), signal)
	} else if self.verbose {
		fmt.Printf("[signal][%s]drop %s\n", signal.MessageType, path)
	}
}

type delayedSignalFrame struct {
	path  TransferPath
	frame *protocol.Frame
	due   time.Time
}

// delayedSignalPipe models a propagation delay without serializing a burst:
// each frame is due one delay after its own send time, and adjacent due frames
// are delivered FIFO. This is closer to a signaling link than sleeping once
// per frame, which would incorrectly charge a full RTT for adjacent offer and
// candidate frames.
type delayedSignalPipe struct {
	ctx      context.Context
	delay    time.Duration
	receiver SignalReceiver
	queue    chan delayedSignalFrame
}

func newDelayedSignalPipe(
	ctx context.Context,
	delay time.Duration,
	receiver SignalReceiver,
) *delayedSignalPipe {
	pipe := &delayedSignalPipe{
		ctx:      ctx,
		delay:    delay,
		receiver: receiver,
		queue:    make(chan delayedSignalFrame, 256),
	}
	go pipe.run()
	return pipe
}

func (self *delayedSignalPipe) SetSignalReceiver(receiver SignalReceiver) {
	self.receiver = receiver
}

func (self *delayedSignalPipe) SendSignal(path TransferPath, frame *protocol.Frame, _ ...any) {
	owned := &protocol.Frame{
		MessageType:  frame.MessageType,
		Raw:          frame.Raw,
		MessageBytes: slices.Clone(frame.MessageBytes),
	}
	select {
	case <-self.ctx.Done():
	case self.queue <- delayedSignalFrame{
		path:  path.SourceMask(),
		frame: owned,
		due:   time.Now().Add(self.delay),
	}:
	}
}

func (self *delayedSignalPipe) run() {
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	pending := make([]delayedSignalFrame, 0, 16)
	for {
		if len(pending) == 0 {
			select {
			case <-self.ctx.Done():
				return
			case first := <-self.queue:
				pending = append(pending, first)
			}
		}
		timerC := resetOrCreateTimer(&timer, time.Until(pending[0].due))
		select {
		case <-self.ctx.Done():
			return
		case next := <-self.queue:
			timer.Stop()
			pending = append(pending, next)
			continue
		case <-timerC:
		}
		now := time.Now()
		dueCount := 0
		for dueCount < len(pending) && !now.Before(pending[dueCount].due) {
			dueCount++
		}
		for _, signal := range pending[:dueCount] {
			if self.receiver != nil {
				_ = self.receiver.ReceiveSignal(signal.path, signal.frame)
			}
		}
		copy(pending, pending[dueCount:])
		pending = pending[:len(pending)-dueCount]
	}
}

// CONNECT_WEBRTC_SIGNAL_LATENCY_MEASURE=1 enables a manual setup measurement
// with a controlled 25 ms one-way signaling delay. It captures candidate/SDP
// serialization that a zero-delay in-process signal pipe hides.
func TestWebRtcSignalingReadyLatencyMeasurement(t *testing.T) {
	if os.Getenv("CONNECT_WEBRTC_SIGNAL_LATENCY_MEASURE") == "" {
		t.Skip("set CONNECT_WEBRTC_SIGNAL_LATENCY_MEASURE=1")
	}

	const pairCount = 15
	const signalDelay = 25 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	settingsA := DefaultWebRtcSettings()
	settingsB := DefaultWebRtcSettings()
	settingsA.Log = NewNoopLogger()
	settingsB.Log = NewNoopLogger()
	settingsA.IceServerUrls = nil
	settingsB.IceServerUrls = nil
	settingsA.MaxPeerConnectionCount = 0
	settingsB.MaxPeerConnectionCount = 0
	settingsA.UseEgressOnlyIceInterfaces = true
	settingsB.UseEgressOnlyIceInterfaces = true

	signalPipeA := newDelayedSignalPipe(ctx, signalDelay, nil)
	signalPipeB := newDelayedSignalPipe(ctx, signalDelay, nil)
	managerA := NewWebRtcManager(ctx, signalPipeA, settingsA)
	managerB := NewWebRtcManager(ctx, signalPipeB, settingsB)
	signalPipeA.SetSignalReceiver(managerB)
	signalPipeB.SetSignalReceiver(managerA)

	peerIdA := NewId()
	peerIdB := NewId()
	latencies := make([]time.Duration, 0, pairCount)
	for range pairCount {
		streamId := NewId()
		passive, err := managerB.NewP2pConnPassive(
			ctx,
			NewTransferPath(peerIdB, peerIdA, streamId),
		)
		AssertEqual(t, err, nil)

		start := time.Now()
		active, err := managerA.NewP2pConnActive(
			ctx,
			NewTransferPath(peerIdA, peerIdB, streamId),
		)
		AssertEqual(t, err, nil)
		AssertEqual(t, active.SetWriteDeadline(time.Now().Add(5*time.Second)), nil)
		AssertEqual(t, passive.SetReadDeadline(time.Now().Add(5*time.Second)), nil)
		readDone := make(chan error, 1)
		go func() {
			var b [1]byte
			_, readErr := passive.Read(b[:])
			readDone <- readErr
		}()
		_, err = active.Write([]byte{1})
		AssertEqual(t, err, nil)
		AssertEqual(t, <-readDone, nil)
		latencies = append(latencies, time.Since(start))
		active.Close()
		passive.Close()
	}

	sort.Slice(latencies, func(i, j int) bool {
		return latencies[i] < latencies[j]
	})
	t.Logf(
		"25ms one-way signaling ready latency: median=%s p95=%s min=%s max=%s",
		latencies[len(latencies)/2],
		latencies[(len(latencies)*95-1)/100],
		latencies[0],
		latencies[len(latencies)-1],
	)
}

// BenchmarkCreateWebRtcPeerConnection tracks the setup cost paid before ICE
// gathering begins. Network-peer churn can create several peer connections at
// once, so constructor latency and allocation volume directly affect setup
// tail latency and mobile GC frequency.
func BenchmarkCreateWebRtcPeerConnection(b *testing.B) {
	settings := DefaultWebRtcSettings()
	settings.Log = NewNoopLogger()

	b.ReportAllocs()
	for range b.N {
		factory, err := newWebRtcPeerConnectionFactory(settings)
		if err != nil {
			b.Fatal(err)
		}
		pc, err := factory.NewPeerConnection()
		if err != nil {
			b.Fatal(err)
		}
		if err := pc.Close(); err != nil {
			b.Fatal(err)
		}
		if err := factory.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWebRtcPeerConnectionFactoryReuse(b *testing.B) {
	settings := DefaultWebRtcSettings()
	settings.Log = NewNoopLogger()
	factory, err := newWebRtcPeerConnectionFactory(settings)
	if err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		pc, err := factory.NewPeerConnection()
		if err != nil {
			b.Fatal(err)
		}
		if err := pc.Close(); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	if err := factory.Close(); err != nil {
		b.Fatal(err)
	}
}

func BenchmarkP2pReceiveBufferOwnership(b *testing.B) {
	payload := bytes.Repeat([]byte{0x5a}, sendPackBatchMaxMessageByteCount)
	b.SetBytes(int64(len(payload)))

	b.Run("scratch-then-copy", func(b *testing.B) {
		scratch := make([]byte, len(payload))
		b.ReportAllocs()
		for range b.N {
			copy(scratch, payload)
			owned := MessagePoolCopy(scratch)
			MessagePoolReturn(owned)
		}
	})
	b.Run("direct-owned-read", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			owned := MessagePoolGet(len(payload))
			copy(owned, payload)
			MessagePoolReturn(owned)
		}
	})
}

// CONNECT_WEBRTC_MEASURE=1 enables a manual live-resource probe. It keeps
// multiple connections on the same two managers so changes such as API,
// certificate, candidate filtering, and ICE socket behavior show up in heap,
// goroutine, and candidate-pair deltas. CONNECT_WEBRTC_EGRESS_ONLY=1 selects
// the device-client interface profile.
func TestWebRtcLiveResourceMeasurement(t *testing.T) {
	if os.Getenv("CONNECT_WEBRTC_MEASURE") == "" {
		t.Skip("set CONNECT_WEBRTC_MEASURE=1")
	}
	const pairCount = 8
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	settingsA := DefaultWebRtcSettings()
	settingsB := DefaultWebRtcSettings()
	settingsA.Log = NewNoopLogger()
	settingsB.Log = NewNoopLogger()
	switch os.Getenv("CONNECT_WEBRTC_STUN") {
	case "":
		settingsA.IceServerUrls = nil
		settingsB.IceServerUrls = nil
	case "cloudflare":
		settingsA.IceServerUrls = []string{"stun:stun.cloudflare.com:3478"}
		settingsB.IceServerUrls = []string{"stun:stun.cloudflare.com:3478"}
	case "google":
		settingsA.IceServerUrls = []string{"stun:stun.l.google.com:19302"}
		settingsB.IceServerUrls = []string{"stun:stun.l.google.com:19302"}
	}
	settingsA.MaxPeerConnectionCount = 0
	settingsB.MaxPeerConnectionCount = 0
	egressOnly := os.Getenv("CONNECT_WEBRTC_EGRESS_ONLY") != ""
	settingsA.UseEgressOnlyIceInterfaces = egressOnly
	settingsB.UseEgressOnlyIceInterfaces = egressOnly
	if keepAliveValue := os.Getenv("CONNECT_WEBRTC_KEEPALIVE"); keepAliveValue != "" {
		keepAlive, err := time.ParseDuration(keepAliveValue)
		AssertEqual(t, err, nil)
		settingsA.KeepAliveTimeout = keepAlive
		settingsB.KeepAliveTimeout = keepAlive
	}

	signalPipeA := newSignalPipe(nil)
	signalPipeB := newSignalPipe(nil)
	managerA := NewWebRtcManager(ctx, signalPipeA, settingsA)
	managerB := NewWebRtcManager(ctx, signalPipeB, settingsB)
	signalPipeA.SetSignalReceiver(managerB)
	signalPipeB.SetSignalReceiver(managerA)

	fdCount := func() int {
		if runtime.GOOS != "linux" {
			return -1
		}
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			return -1
		}
		return len(entries)
	}
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	beforeGoroutines := runtime.NumGoroutine()
	beforeFds := fdCount()

	peerIdA := NewId()
	peerIdB := NewId()
	conns := make([]WebRtcConn, 0, 2*pairCount)
	for range pairCount {
		streamId := NewId()
		passive, err := managerB.NewP2pConnPassive(ctx, NewTransferPath(peerIdB, peerIdA, streamId))
		AssertEqual(t, err, nil)
		conns = append(conns, passive)
		active, err := managerA.NewP2pConnActive(ctx, NewTransferPath(peerIdA, peerIdB, streamId))
		AssertEqual(t, err, nil)
		conns = append(conns, active)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		connected := 0
		for _, conn := range conns {
			if conn.Connected() {
				connected++
			}
		}
		if connected == len(conns) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d/%d peer connections connected", connected, len(conns))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if idleValue := os.Getenv("CONNECT_WEBRTC_IDLE"); idleValue != "" {
		idle, err := time.ParseDuration(idleValue)
		AssertEqual(t, err, nil)
		select {
		case <-ctx.Done():
			t.Fatal("context ended during idle measurement")
		case <-time.After(idle):
		}
	}

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	localCandidates := 0
	remoteCandidates := 0
	candidatePairs := 0
	for _, stat := range conns[0].(*peerConn).pc.GetStats() {
		switch stat := stat.(type) {
		case webrtc.ICECandidateStats:
			switch stat.Type {
			case webrtc.StatsTypeLocalCandidate:
				localCandidates++
			case webrtc.StatsTypeRemoteCandidate:
				remoteCandidates++
			}
		case webrtc.ICECandidatePairStats:
			candidatePairs++
		}
	}
	fdDelta := -1
	if afterFds := fdCount(); 0 <= beforeFds && 0 <= afterFds {
		fdDelta = afterFds - beforeFds
	}
	t.Logf(
		"%d live peer connections (egress_only=%t keepalive=%s): heap=%d bytes, heap_objects=%d, goroutines=%d, fds=%d, first_pc_candidates=%d/%d pairs=%d",
		len(conns),
		egressOnly,
		settingsA.KeepAliveTimeout,
		int64(after.HeapAlloc)-int64(before.HeapAlloc),
		int64(after.HeapObjects)-int64(before.HeapObjects),
		runtime.NumGoroutine()-beforeGoroutines,
		fdDelta,
		localCandidates,
		remoteCandidates,
		candidatePairs,
	)
	if profilePath := os.Getenv("CONNECT_WEBRTC_GOROUTINE_PROFILE"); profilePath != "" {
		profile, createErr := os.Create(profilePath)
		AssertEqual(t, createErr, nil)
		AssertEqual(t, pprof.Lookup("goroutine").WriteTo(profile, 0), nil)
		AssertEqual(t, profile.Close(), nil)
	}
	for _, conn := range conns {
		conn.Close()
	}
}

// CONNECT_WEBRTC_ROUTE_THROUGHPUT_MEASURE=1 enables a manual comparison of
// the bounded route-channel depth around a real detached data channel. It
// normally exercises production's 3 KiB transfer batching. Setting
// CONNECT_WEBRTC_ROUTE_MESSAGE_SIZES=1 also measures larger messages to
// quantify the ceiling available to a future capability-negotiated P2P
// aggregate without pretending those sizes fit the platform fallback route.
// This exists to prevent reducing queue memory based on intuition at the
// expense of scheduler-bound throughput.
func TestWebRtcP2pRouteThroughputMeasurement(t *testing.T) {
	if os.Getenv("CONNECT_WEBRTC_ROUTE_THROUGHPUT_MEASURE") == "" {
		t.Skip("set CONNECT_WEBRTC_ROUTE_THROUGHPUT_MEASURE=1")
	}

	messageByteCounts := []int{sendPackBatchMaxMessageByteCount}
	channelBufferSizes := []int{1, 4, 8, 32}
	if os.Getenv("CONNECT_WEBRTC_ROUTE_MESSAGE_SIZES") != "" {
		messageByteCounts = []int{3 * 1024, 6 * 1024, 12 * 1024, 24 * 1024, 48 * 1024, 60 * 1024}
		channelBufferSizes = []int{4}
	}
	for _, channelBufferSize := range channelBufferSizes {
		for _, messageByteCount := range messageByteCounts {
			t.Run(fmt.Sprintf("queue=%d/message=%d", channelBufferSize, messageByteCount), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()

				settingsA := DefaultWebRtcSettings()
				settingsB := DefaultWebRtcSettings()
				settingsA.Log = NewNoopLogger()
				settingsB.Log = NewNoopLogger()
				settingsA.IceServerUrls = nil
				settingsB.IceServerUrls = nil
				settingsA.MaxPeerConnectionCount = 0
				settingsB.MaxPeerConnectionCount = 0
				settingsA.UseEgressOnlyIceInterfaces = true
				settingsB.UseEgressOnlyIceInterfaces = true
				if os.Getenv("CONNECT_WEBRTC_ORDERED") != "" {
					settingsA.DataChannelOrdered = true
					settingsB.DataChannelOrdered = true
				}

				signalPipeA := newSignalPipe(nil)
				signalPipeB := newSignalPipe(nil)
				managerA := NewWebRtcManager(ctx, signalPipeA, settingsA)
				managerB := NewWebRtcManager(ctx, signalPipeB, settingsB)
				signalPipeA.SetSignalReceiver(managerB)
				signalPipeB.SetSignalReceiver(managerA)

				peerIdA := NewId()
				peerIdB := NewId()
				streamId := NewId()
				passive, err := managerB.NewP2pConnPassive(
					ctx,
					NewTransferPath(peerIdB, peerIdA, streamId),
				)
				AssertEqual(t, err, nil)
				defer passive.Close()
				active, err := managerA.NewP2pConnActive(
					ctx,
					NewTransferPath(peerIdA, peerIdB, streamId),
				)
				AssertEqual(t, err, nil)
				defer active.Close()

				transportSettings := DefaultP2pTransportSettings()
				transportSettings.ChannelBufferSize = channelBufferSize
				transportCtx, transportCancel := context.WithCancel(ctx)
				defer transportCancel()
				sendTransport, sendRoute := NewP2pSendTransport(
					transportCtx,
					transportCancel,
					active,
					streamId,
					transportSettings,
				)
				receiveTransport, receiveRoute := NewP2pReceiveTransport(
					transportCtx,
					transportCancel,
					passive,
					streamId,
					transportSettings,
				)
				// Keep both transport owners live for the duration of the route
				// measurement; the routes alone do not express that relationship.
				_ = sendTransport
				_ = receiveTransport

				const totalByteCount = 32 * 1024 * 1024
				messageCount := totalByteCount / messageByteCount
				payload := bytes.Repeat([]byte{0x5a}, messageByteCount)
				receiveDone := make(chan error, 1)
				go func() {
					for range messageCount {
						select {
						case <-transportCtx.Done():
							receiveDone <- transportCtx.Err()
							return
						case received := <-receiveRoute:
							if len(received) != len(payload) || received[0] != payload[0] {
								MessagePoolReturn(received)
								receiveDone <- errors.New("received payload mismatch")
								return
							}
							MessagePoolReturn(received)
						}
					}
					receiveDone <- nil
				}()

				start := time.Now()
				for range messageCount {
					message := MessagePoolCopy(payload)
					select {
					case <-transportCtx.Done():
						MessagePoolReturn(message)
						t.Fatal("transport stopped during throughput measurement")
					case sendRoute <- message:
					}
				}
				AssertEqual(t, <-receiveDone, nil)
				elapsed := time.Since(start)
				t.Logf(
					"queue=%d message=%d ordered=%t: %d bytes in %s = %.1f MiB/s",
					channelBufferSize,
					messageByteCount,
					settingsA.DataChannelOrdered,
					messageCount*messageByteCount,
					elapsed,
					float64(messageCount*messageByteCount)/(1024*1024)/elapsed.Seconds(),
				)
			})
		}
	}
}
