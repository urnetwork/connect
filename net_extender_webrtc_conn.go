//go:build !js

package connect

// The byte stream of the webrtc extender carrier (EXTENDER.md S). A detached
// pion data channel is message oriented: every Read hands back exactly one
// SCTP message, and a buffer shorter than the message consumes the message
// and returns io.ErrShortBuffer with the rest gone. The carrier runs the A3
// request and the inner bytes over it as one reliable byte stream, exactly as
// the tcp carrier's terminated tls, so the adapter here turns messages back
// into a stream: a read takes a whole message into a buffer sized to the
// largest message the peer may send and serves it out in whatever pieces the
// reader asks for, and a write is cut into messages no larger than every
// webrtc implementation accepts.
//
// Closing the stream closes the data channel, the peer connection that owns
// it, and the per-connection address resolution of the factory, so a carrier
// stream is one resource to the code that holds it. Safe for concurrent
// net.Conn calls: reads and complete writes each serialize independently.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
)

// webRtcExtenderChannel is what the adapter needs of a detached data channel
// (datachannel.ReadWriteCloserDeadliner satisfies it): message reads and
// writes, close, and the two deadlines.
type webRtcExtenderChannel interface {
	io.ReadWriteCloser
	SetReadDeadline(t time.Time) error
	SetWriteDeadline(t time.Time) error
}

// The largest message the carrier writes on its data channel. 16 KiB is the
// size every webrtc implementation accepts (RFC 8831 §6.6), so a write never
// exceeds what the peer advertised whatever stack it runs.
const webRtcExtenderMaxMessageByteCount = 16 * 1024

// The buffer a carrier read takes a whole message into when the settings
// bound neither side: pion's own default maximum message size.
const webRtcExtenderDefaultReadBufferByteCount = 64 * 1024

// How long a close waits for what was written to be acknowledged before it
// closes the peer connection, which aborts the association and drops whatever
// is still in flight. The tcp carrier's close is a FIN behind its data; this
// is the carrier's equivalent, bounded so a dead peer cannot hold a close.
const webRtcExtenderCloseDrainTimeout = 5 * time.Second

// webRtcExtenderBufferedChannel is the buffered-amount view of a detached
// channel (datachannel.DataChannel has it), which the close drains through.
// The low callback fires when the amount falls to the threshold, which is
// when every written byte has been acknowledged by the peer.
type webRtcExtenderBufferedChannel interface {
	BufferedAmount() uint64
	SetBufferedAmountLowThreshold(threshold uint64)
	OnBufferedAmountLow(f func())
}

// webRtcDataChannelConn is the stream adapter of one open carrier data
// channel. The pending slice is the unread tail of the last message read.
type webRtcDataChannelConn struct {
	channel        webRtcExtenderChannel
	peerConnection *webrtc.PeerConnection
	cancel         context.CancelFunc
	localAddr      net.Addr
	remoteAddr     net.Addr

	readBuffer   []byte
	readPending  []byte
	readLock     sync.Mutex
	writeLock    sync.Mutex
	deadlineLock sync.Mutex
	closing      atomic.Bool
	// the close's bound on waiting for acknowledgement
	// (webRtcExtenderCloseDrainTimeout); tests shorten or lengthen it
	drainTimeout time.Duration

	closeOnce sync.Once
	closeErr  error
}

// newWebRtcDataChannelConn adapts one detached channel of peerConnection.
// readBufferByteCount is the largest message the peer may send, which is this
// side's advertised maximum message size; <= 0 takes the pion default. The
// addresses are the selected ICE pair's when one is selected, which an open
// data channel implies, else the carrier placeholder.
func newWebRtcDataChannelConn(
	channel webRtcExtenderChannel,
	peerConnection *webrtc.PeerConnection,
	cancel context.CancelFunc,
	readBufferByteCount int,
) *webRtcDataChannelConn {
	if readBufferByteCount <= 0 {
		readBufferByteCount = webRtcExtenderDefaultReadBufferByteCount
	}
	localAddr, remoteAddr := webRtcSelectedPairAddrs(peerConnection)
	return &webRtcDataChannelConn{
		channel:        channel,
		peerConnection: peerConnection,
		cancel:         cancel,
		localAddr:      localAddr,
		remoteAddr:     remoteAddr,
		readBuffer:     make([]byte, readBufferByteCount),
		drainTimeout:   webRtcExtenderCloseDrainTimeout,
	}
}

// webRtcSelectedPairAddrs reads the addresses of the selected ICE candidate
// pair as udp addresses, or the carrier placeholder for a side it cannot
// read. The remote one is what the extender's admission keys on (A12) and
// what the forward family follows (A7).
func webRtcSelectedPairAddrs(peerConnection *webrtc.PeerConnection) (net.Addr, net.Addr) {
	var localAddr net.Addr = webRtcExtenderAddr{}
	var remoteAddr net.Addr = webRtcExtenderAddr{}
	if peerConnection == nil {
		return localAddr, remoteAddr
	}
	sctpTransport := peerConnection.SCTP()
	if sctpTransport == nil {
		return localAddr, remoteAddr
	}
	dtlsTransport := sctpTransport.Transport()
	if dtlsTransport == nil {
		return localAddr, remoteAddr
	}
	iceTransport := dtlsTransport.ICETransport()
	if iceTransport == nil {
		return localAddr, remoteAddr
	}
	pair, err := iceTransport.GetSelectedCandidatePair()
	if err != nil || pair == nil {
		return localAddr, remoteAddr
	}
	candidateAddr := func(candidate *webrtc.ICECandidate) net.Addr {
		if candidate == nil {
			return webRtcExtenderAddr{}
		}
		ip := net.ParseIP(candidate.Address)
		if ip == nil {
			return webRtcExtenderAddr{}
		}
		return &net.UDPAddr{IP: ip, Port: int(candidate.Port)}
	}
	return candidateAddr(pair.Local), candidateAddr(pair.Remote)
}

// Read serves the unread tail of the last message first, then takes the next
// whole message. A message larger than the read buffer is a peer past the
// advertised bound: the channel has already consumed it, so the stream is
// corrupt from here and the read fails rather than skipping bytes.
func (self *webRtcDataChannelConn) Read(b []byte) (int, error) {
	self.readLock.Lock()
	defer self.readLock.Unlock()
	if self.closing.Load() {
		return 0, net.ErrClosed
	}
	if len(b) == 0 {
		return 0, nil
	}
	if 0 < len(self.readPending) {
		n := copy(b, self.readPending)
		self.readPending = self.readPending[n:]
		return n, nil
	}
	for {
		n, err := self.channel.Read(self.readBuffer)
		if errors.Is(err, io.ErrShortBuffer) {
			return 0, fmt.Errorf("webrtc extender carrier message exceeds %d bytes", len(self.readBuffer))
		}
		err = webRtcExtenderDeadlineError(err)
		if 0 < n {
			copied := copy(b, self.readBuffer[:n])
			if copied < n {
				// keep the tail for the next read; the buffer is reused only
				// once the tail is drained, so the copy below is the one copy
				self.readPending = append([]byte(nil), self.readBuffer[copied:n]...)
			}
			return copied, err
		}
		if err != nil {
			return 0, err
		}
		// an empty message carries nothing a stream can see; read on
	}
}

// Write cuts b into messages of at most webRtcExtenderMaxMessageByteCount.
func (self *webRtcDataChannelConn) Write(b []byte) (int, error) {
	self.writeLock.Lock()
	defer self.writeLock.Unlock()
	if self.closing.Load() {
		return 0, net.ErrClosed
	}
	written := 0
	for written < len(b) {
		message := b[written:min(len(b), written+webRtcExtenderMaxMessageByteCount)]
		n, err := self.channel.Write(message)
		written += n
		if err != nil {
			return written, webRtcExtenderDeadlineError(err)
		}
		if n < len(message) {
			// a short message write is a channel that stopped; do not spin
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}

// Close lets what was written reach the peer, then closes the channel and the
// peer connection. Closing the peer connection aborts the SCTP association,
// which drops unacknowledged data on both sides, so the drain comes first: a
// response the extender wrote just before closing, or a request the dialer
// wrote, is otherwise lost behind the abort.
func (self *webRtcDataChannelConn) Close() error {
	self.closeOnce.Do(func() {
		// Wake calls already in the channel before taking their serialization
		// locks. Deadline setters finish before shutdown takes ownership, so
		// an in-flight setter cannot clear the interruption afterward.
		func() {
			self.deadlineLock.Lock()
			defer self.deadlineLock.Unlock()
			self.closing.Store(true)
			_ = self.channel.SetReadDeadline(time.Now())
			_ = self.channel.SetWriteDeadline(time.Now())
		}()
		self.writeLock.Lock()
		defer self.writeLock.Unlock()
		readDone := self.drain()
		self.closeErr = self.channel.Close()
		if self.peerConnection != nil {
			if err := self.peerConnection.Close(); self.closeErr == nil {
				self.closeErr = err
			}
		}
		if self.cancel != nil {
			self.cancel()
		}
		if readDone != nil {
			<-readDone
		}
	})
	return self.closeErr
}

// webRtcExtenderDeadlineError returns a deadline error as the net.Error every
// net.Conn reports one as. pion wraps os.ErrDeadlineExceeded in a plain error,
// which errors.Is still matches but a type assertion to net.Error does not:
// net/http asserts exactly that on the read it interrupts at a hijack and,
// seeing no timeout, cancels the request context -- which is the context the
// extender's forward and relay run on. Any other error passes through.
func webRtcExtenderDeadlineError(err error) error {
	if err != nil && errors.Is(err, os.ErrDeadlineExceeded) {
		return os.ErrDeadlineExceeded
	}
	return err
}

// Waits, bounded, for acknowledgement or association failure. The caller
// closes the channel afterward and joins the returned reader completion.
func (self *webRtcDataChannelConn) drain() <-chan struct{} {
	buffered, ok := self.channel.(webRtcExtenderBufferedChannel)
	if !ok {
		return nil
	}
	drained := make(chan struct{}, 1)
	buffered.SetBufferedAmountLowThreshold(0)
	buffered.OnBufferedAmountLow(func() {
		select {
		case drained <- struct{}{}:
		default:
		}
	})
	if buffered.BufferedAmount() == 0 {
		return nil
	}
	// a dead association never acknowledges, and nothing on the peer
	// connection says it died: a read on it fails at once, where a live one
	// blocks or hands over bytes the closing side no longer wants. The
	// peer's own half-close (EOF) is not death: its acknowledgements still
	// arrive, so the wait goes on for them.
	dead := make(chan struct{})
	readDone := make(chan struct{})
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		defer close(readDone)
		self.readLock.Lock()
		defer self.readLock.Unlock()
		select {
		case <-stop:
			return
		default:
		}
		// A caller's expired deadline is reversible, and Close interrupted
		// any prior reader with one. Neither means the association died.
		func() {
			self.deadlineLock.Lock()
			defer self.deadlineLock.Unlock()
			_ = self.channel.SetReadDeadline(time.Time{})
		}()
		discard := make([]byte, len(self.readBuffer))
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := self.channel.Read(discard); err != nil {
				var netErr net.Error
				if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrDeadlineExceeded) && !(errors.As(err, &netErr) && netErr.Timeout()) {
					close(dead)
				}
				return
			}
		}
	}()
	select {
	case <-drained:
	case <-dead:
	case <-time.After(self.drainTimeout):
	}
	return readDone
}

func (self *webRtcDataChannelConn) LocalAddr() net.Addr  { return self.localAddr }
func (self *webRtcDataChannelConn) RemoteAddr() net.Addr { return self.remoteAddr }

func (self *webRtcDataChannelConn) SetDeadline(t time.Time) error {
	self.deadlineLock.Lock()
	defer self.deadlineLock.Unlock()
	if self.closing.Load() {
		return net.ErrClosed
	}
	if err := self.channel.SetReadDeadline(t); err != nil {
		return err
	}
	return self.channel.SetWriteDeadline(t)
}

func (self *webRtcDataChannelConn) SetReadDeadline(t time.Time) error {
	self.deadlineLock.Lock()
	defer self.deadlineLock.Unlock()
	if self.closing.Load() {
		return net.ErrClosed
	}
	return self.channel.SetReadDeadline(t)
}

func (self *webRtcDataChannelConn) SetWriteDeadline(t time.Time) error {
	self.deadlineLock.Lock()
	defer self.deadlineLock.Unlock()
	if self.closing.Load() {
		return net.ErrClosed
	}
	return self.channel.SetWriteDeadline(t)
}

// webRtcExtenderAddr is the placeholder address of a carrier stream whose ICE
// pair cannot be read: the carrier has no ip:port of its own.
type webRtcExtenderAddr struct{}

func (webRtcExtenderAddr) Network() string { return ExtenderCarrierWebRtc }
func (webRtcExtenderAddr) String() string  { return ExtenderCarrierWebRtc }
