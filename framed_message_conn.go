package connect

import (
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// H1MessageConn is the binary-message surface used by the existing H1
// lifecycle. A FramedMessageConn accepts only BinaryMessage; it does not
// implement WebSocket opcodes, control frames, masking, or compression.
// Successful pooled reads are owned by the caller until MessagePoolReturn.
type H1MessageConn interface {
	Close() error
	WriteMessage(int, []byte) error
	ReadMessage() (int, []byte, error)
	NextReader() (int, io.Reader, error)
	SetReadLimit(int64)
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
	UnderlyingConn() net.Conn
}

// FramedMessageConn has one reader and one writer, which may run concurrently.
// Each stream has a fixed, negotiated wire format and explicit admission cap.
// The writer lazily owns 16 KiB scratch. Larger compact frames use Framer's
// split path; larger XL frames use a full frame-local temporary buffer that is
// released on return. Read state never retains an application message.
type FramedMessageConn struct {
	net.Conn
	framer    *Framer
	xl        *FramerXl
	maximum   int
	readLimit atomic.Int64
	storage   []byte
	body      framedMessageBody
	stats     *H1PlusStats
	progress  *h1PhysicalProgress
	// the Sec-WebSocket-Protocol the upgrade selected; carries the delay
	// header offer (transport_delay_h1.go)
	subprotocol string
	// set when the delay header was negotiated; the compact framer only
	delayFramer *Framer
	delayWrite  *transportDelayWriteLane
	delayRead   *transportDelayReadLane
}

func NewFramedMessageConn(conn net.Conn, protocol string, maximum int, stats *H1PlusStats) (*FramedMessageConn, error) {
	if conn == nil || maximum < 0 || maximum > math.MaxInt-4 || !supportedFramedProtocol(protocol) ||
		(protocol == H1FramerProtocol && math.MaxUint16 < maximum) || uint64(maximum) > math.MaxUint32 {
		return nil, errors.New("invalid framed message connection settings")
	}
	c := &FramedMessageConn{Conn: conn, maximum: maximum, stats: stats}
	c.readLimit.Store(int64(maximum))
	if protocol == H1FramerProtocol {
		c.framer = NewFramer(DefaultFramerSettings(maximum))
	} else {
		c.xl = NewFramerXl(DefaultFramerXlSettings(maximum))
	}
	c.body.conn = c
	if upgraded, ok := conn.(*httpUpgradeConn); ok {
		c.subprotocol = upgraded.subprotocol
	}
	return c, nil
}

// Subprotocol is the Sec-WebSocket-Protocol token the upgrade selected, as on
// a WebSocket conn.
func (c *FramedMessageConn) Subprotocol() string { return c.subprotocol }

// enableTransportDelay switches a negotiated stream to delay header frames in
// both directions (DELAY.md §5). Every physical write is stamped, so the header
// lives here rather than in a wrapper. Call before the reader and writer start.
// Only the compact framer carries the header.
func (c *FramedMessageConn) enableTransportDelay(delay *transportDelay) (*transportDelayReadLane, bool) {
	if c.framer == nil {
		return nil, false
	}
	settings := DefaultFramerSettings(c.maximum)
	settings.HeaderByteCount = transportDelayHeaderByteCount
	c.delayFramer = NewFramer(settings)
	c.delayWrite = delay.newWriteLane()
	c.delayRead = delay.newReadLane(transportDelayLaneOrdered)
	return c.delayRead, true
}

func (c *FramedMessageConn) UnderlyingConn() net.Conn { return c.Conn }

// Write delegates encoded bytes and counts actual stream calls, including
// the legacy split path of a compact frame larger than its caller storage.
func (c *FramedMessageConn) Write(p []byte) (int, error) {
	if c.stats != nil {
		c.stats.writes.Add(1)
	}
	return c.Conn.Write(p)
}
func (c *FramedMessageConn) SetReadLimit(limit int64) {
	c.readLimit.Store(max(0, min(limit, int64(c.maximum))))
}

// RPC keeps its gomobile-compatible binary carrier interface. Framed streams
// use serialized zero-length binary heartbeats, never ping/pong emulation.
func (c *FramedMessageConn) SetPongHandler(func(string) error) {}
func (c *FramedMessageConn) WriteControl(int, []byte, time.Time) error {
	return errors.New("framed stream has no WebSocket control frames")
}

func (c *FramedMessageConn) NextReader() (int, io.Reader, error) {
	if c.body.remaining != 0 {
		if _, err := io.Copy(io.Discard, &c.body); err != nil {
			return 0, nil, err
		}
	}
	var length int
	var err error
	headerLen := 0
	if c.xl != nil {
		length, err = c.xl.ReadHeader(c.Conn)
	} else if c.delayFramer != nil {
		headerLen = transportDelayHeaderByteCount
		length, err = c.delayFramer.ReadHeader(c.Conn)
		if err == nil && length < headerLen {
			err = errors.New("framed stream message shorter than its delay header")
		}
	} else {
		length, err = c.framer.ReadHeader(c.Conn)
	}
	if err == nil && c.readLimit.Load() < int64(length-headerLen) {
		err = errors.New("framed stream message limit exceeded")
	}
	if err == nil && 0 < headerLen {
		var header transportDelayHeader
		if _, err = io.ReadFull(c.Conn, header[:]); err == nil {
			c.delayRead.receive(header[:], 4+length)
		}
	}
	if err != nil {
		c.recordReadError(err)
		c.Conn.Close()
		return 0, nil, err
	}
	c.body.remaining = length - headerLen
	return websocket.BinaryMessage, &c.body, nil
}

type framedMessageBody struct {
	conn      *FramedMessageConn
	remaining int
}

func (r *framedMessageBody) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	p = p[:min(len(p), r.remaining)]
	n, err := r.conn.Conn.Read(p)
	r.remaining -= n
	if errors.Is(err, io.EOF) && r.remaining != 0 {
		err = io.ErrUnexpectedEOF
	}
	if err != nil && !(errors.Is(err, io.EOF) && r.remaining == 0) {
		r.conn.recordReadError(err)
		r.conn.Close()
	}
	return n, err
}

func (c *FramedMessageConn) ReadPooledMessage() (int, []byte, error) {
	return c.readPooledMessage(c.readLimit.Load())
}

func (c *FramedMessageConn) readPooledMessage(limit int64) (int, []byte, error) {
	kind, _, err := c.NextReader()
	if err != nil {
		return kind, nil, err
	}
	if limit < int64(c.body.remaining) {
		c.Conn.Close()
		return kind, nil, errors.New("framed stream message limit exceeded")
	}
	message := MessagePoolGet(c.body.remaining)
	if _, err = io.ReadFull(&c.body, message); err != nil {
		MessagePoolReturn(message)
		return kind, nil, err
	}
	return kind, message, nil
}

// ReadMessage follows Gorilla's unpooled ownership convention. Hot paths use
// ReadH1PooledMessage or NextReader to avoid the extra copy in this convenience API.
func (c *FramedMessageConn) ReadMessage() (int, []byte, error) {
	kind, message, err := c.ReadPooledMessage()
	if err != nil {
		return kind, nil, err
	}
	defer MessagePoolReturn(message)
	return kind, append([]byte(nil), message...), nil
}

func ReadH1PooledMessage(conn H1MessageConn, limit int64) (int, []byte, error) {
	if delayed, ok := conn.(*transportDelayWebSocketConn); ok {
		return delayed.readPooledMessage(limit)
	}
	if observed, ok := conn.(*strategyDeliveryMessageConn); ok {
		kind, message, err := ReadH1PooledMessage(observed.H1MessageConn, limit)
		observed.info.observeRead(observed.ctx, len(message), err, false)
		return kind, message, err
	}
	if framed, ok := conn.(*FramedMessageConn); ok {
		return framed.readPooledMessage(limit)
	}
	kind, reader, err := conn.NextReader()
	if err != nil || kind != websocket.BinaryMessage {
		return kind, nil, err
	}
	message, err := MessagePoolReadAllLimit(reader, limit)
	return kind, message, err
}

func (c *FramedMessageConn) WriteMessage(kind int, message []byte) error {
	if kind != websocket.BinaryMessage {
		return errors.New("framed stream requires a binary message")
	}
	return c.WriteMessages([][]byte{message})
}

// WriteMessages validates the complete batch before output and performs one
// payload copy into each bounded ready flush. The caller retains all inputs.
func (c *FramedMessageConn) WriteMessages(messages [][]byte) error {
	for _, message := range messages {
		if c.maximum < len(message) {
			return fmt.Errorf("framed stream message limit exceeded (%d)", c.maximum)
		}
	}
	if len(messages) == 0 {
		return nil
	}
	if c.storage == nil {
		c.storage = make([]byte, 16*1024)
	}
	frameOverhead := 4
	if c.delayWrite != nil {
		frameOverhead += transportDelayHeaderByteCount
	}
	for len(messages) != 0 {
		count, bytes := 1, len(messages[0])+frameOverhead
		for count < len(messages) && bytes <= len(c.storage)-frameOverhead && len(messages[count]) <= len(c.storage)-bytes-frameOverhead {
			bytes += len(messages[count]) + frameOverhead
			count++
		}
		var err error
		if c.progress != nil {
			for _, message := range messages[:count] {
				c.progress.queue(message)
			}
			c.progress.beginWrite()
		}
		if c.delayWrite != nil {
			// one chunk is one physical write: its frames share one stamp
			write := c.delayWrite.beginWrite()
			err = c.delayFramer.WriteBatchWithHeaderStorage(c, write.header[:], messages[:count], c.storage)
			c.delayWrite.endWrite(write, bytes, err)
		} else if c.framer != nil {
			err = c.framer.WriteBatchWithStorage(c, messages[:count], c.storage)
		} else {
			err = c.xl.WriteBatchWithStorage(c, messages[:count], c.storage)
		}
		c.progress.endWrite(err)
		if err != nil {
			if c.stats != nil {
				c.stats.writeErrors.Add(1)
			}
			c.Conn.Close()
			return err
		}
		if c.stats != nil {
			c.stats.flushes.Add(1)
			c.stats.messages.Add(uint64(count))
			c.stats.bytes.Add(uint64(bytes - frameOverhead*count))
		}
		messages = messages[count:]
	}
	return nil
}

func (c *FramedMessageConn) recordReadError(err error) {
	if c.stats != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
		c.stats.readErrors.Add(1)
	}
}

// Bounded numeric counters shared by client/server experiments; there are no
// identity, URL, credential, payload, or peer-controlled metric labels.
type H1PlusStats struct {
	attempts, accepted, fallbacks, authFailures, handshakeNanos  atomic.Uint64
	flushes, writes, messages, bytes, readErrors, writeErrors    atomic.Uint64
	fallbackRejected, fallbackInvalid, fallbackIO, fallbackProxy atomic.Uint64
	webSocketSelected, handshakeFailures                         atomic.Uint64
}
type H1PlusStatsSnapshot struct {
	Attempts, Accepted, Fallbacks, AuthFailures, HandshakeNanos  uint64
	Flushes, Writes, Messages, Bytes, ReadErrors, WriteErrors    uint64
	FallbackRejected, FallbackInvalid, FallbackIO, FallbackProxy uint64
	WebSocketSelected, HandshakeFailures                         uint64
}

func (s *H1PlusStats) Snapshot() H1PlusStatsSnapshot {
	if s == nil {
		return H1PlusStatsSnapshot{}
	}
	return H1PlusStatsSnapshot{
		Attempts: s.attempts.Load(), Accepted: s.accepted.Load(), Fallbacks: s.fallbacks.Load(), AuthFailures: s.authFailures.Load(), HandshakeNanos: s.handshakeNanos.Load(),
		Flushes: s.flushes.Load(), Writes: s.writes.Load(), Messages: s.messages.Load(), Bytes: s.bytes.Load(), ReadErrors: s.readErrors.Load(), WriteErrors: s.writeErrors.Load(),
		FallbackRejected: s.fallbackRejected.Load(), FallbackInvalid: s.fallbackInvalid.Load(), FallbackIO: s.fallbackIO.Load(), FallbackProxy: s.fallbackProxy.Load(),
		WebSocketSelected: s.webSocketSelected.Load(), HandshakeFailures: s.handshakeFailures.Load(),
	}
}

// RecordH1PlusSelection records one completed bounded handshake result.
func RecordH1PlusSelection(stats *H1PlusStats, elapsed time.Duration, err error) {
	if stats == nil {
		return
	}
	stats.attempts.Add(1)
	stats.handshakeNanos.Add(uint64(max(0, elapsed)))
	if err == nil {
		stats.accepted.Add(1)
	} else if HTTPUpgradeAllowsFallback(err) {
		stats.fallbacks.Add(1)
		var upgradeErr *HTTPUpgradeError
		if errors.As(err, &upgradeErr) {
			switch upgradeErr.Reason {
			case "rejected":
				stats.fallbackRejected.Add(1)
			case "response-io":
				stats.fallbackIO.Add(1)
			case "http-proxy":
				stats.fallbackProxy.Add(1)
			default:
				stats.fallbackInvalid.Add(1)
			}
		}
	} else {
		stats.handshakeFailures.Add(1)
		var upgradeErr *HTTPUpgradeError
		if errors.As(err, &upgradeErr) && (upgradeErr.StatusCode == 401 || upgradeErr.StatusCode == 403) {
			stats.authFailures.Add(1)
		}
	}
}
