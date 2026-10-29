package connect

// transport_delay_h1.go -- the delay header on h1 carriers (DELAY.md §5, §6).
//
// Both h1 carriers offer the header with the Sec-WebSocket-Protocol token
// TransportDelayH1Subprotocol and switch to it only when the server's 101
// selects the token. h1+ stamps inside FramedMessageConn, where each physical
// write is visible. A WebSocket carries the header as the first bytes of each
// binary message: transportDelayWebSocketConn stamps single writes itself and
// a ready batch through WebSocketWriteBatchConn's observer, so every message
// coalesced into one TLS write shares one stamp.

import (
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var errWebSocketWriteBatchAborted = errors.New("websocket write batch aborted")

// applyTransportDelayH1Offer offers the delay header on an h1 request.
func applyTransportDelayH1Offer(header http.Header) {
	header.Add("Sec-WebSocket-Protocol", TransportDelayH1Subprotocol)
}

// TransportDelayH1Offered reports whether an h1 upgrade request offers the
// delay header, on either the WebSocket or the h1+ upgrade.
func TransportDelayH1Offered(r *http.Request) bool {
	for _, line := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, token := range strings.Split(line, ",") {
			if strings.TrimSpace(token) == TransportDelayH1Subprotocol {
				return true
			}
		}
	}
	return false
}

// TransportDelayH1AcceptHeader is the 101 response header that accepts the
// delay header. A WebSocket server passes it to Upgrader.Upgrade; an h1+
// server passes it to AcceptFramedUpgradeWithHeader.
func TransportDelayH1AcceptHeader() http.Header {
	return http.Header{"Sec-Websocket-Protocol": []string{TransportDelayH1Subprotocol}}
}

// h1MessageConnSubprotocol is the Sec-WebSocket-Protocol the connection's
// upgrade selected.
func h1MessageConnSubprotocol(conn H1MessageConn) string {
	if selected, ok := unobservedH1MessageConn(conn).(interface{ Subprotocol() string }); ok {
		return selected.Subprotocol()
	}
	return ""
}

// transportDelayH1 is the delay state of one negotiated h1 connection.
type transportDelayH1 struct {
	delay    *transportDelay
	conn     H1MessageConn
	readLane *transportDelayReadLane
}

// enableTransportDelayH1 switches a connection whose upgrade selected the
// delay subprotocol to the header wire, and returns the conn to use from now
// on. It returns nil when the connection did not negotiate the header.
// Call before the connection's reader and writer start.
func enableTransportDelayH1(conn H1MessageConn, delay *transportDelay) *transportDelayH1 {
	if h1MessageConnSubprotocol(conn) != TransportDelayH1Subprotocol {
		return nil
	}
	if framed, ok := unobservedH1MessageConn(conn).(*FramedMessageConn); ok {
		readLane, enabled := framed.enableTransportDelay(delay)
		if !enabled {
			return nil
		}
		return &transportDelayH1{delay: delay, conn: conn, readLane: readLane}
	}
	webSocket := &transportDelayWebSocketConn{
		H1MessageConn: conn,
		writeLane:     delay.newWriteLane(),
		readLane:      delay.newReadLane(transportDelayLaneOrdered),
	}
	if batchConn, ok := conn.UnderlyingConn().(*WebSocketWriteBatchConn); ok {
		batchConn.setWriteBatchObserver(webSocket)
	}
	return &transportDelayH1{delay: delay, conn: webSocket, readLane: webSocket.readLane}
}

// NewTransportDelayH1PeerConn is the server's half: when the request offered
// the delay header and the 101 accepted it, it returns the conn to read and
// write through, stamping and echoing without measuring. ok is false when the
// connection did not negotiate the header; the caller keeps conn.
func NewTransportDelayH1PeerConn(conn H1MessageConn, clientId Id) (delayed H1MessageConn, ok bool) {
	delayH1 := enableTransportDelayH1(conn, newTransportDelay(nil, clientId, TransportTypeH1, nil))
	if delayH1 == nil {
		return conn, false
	}
	return delayH1.conn, true
}

// H1WebSocketConn returns the WebSocket under conn's transport wrappers, for
// control frames such as close. Control frames carry no delay header, and
// WriteControl may run concurrently with the wrapper's writer.
func H1WebSocketConn(conn H1MessageConn) (*websocket.Conn, bool) {
	ws, ok := unobservedH1MessageConn(conn).(*websocket.Conn)
	return ws, ok
}

// NoteTransportDelayHandoff records how long a reader was held handing the
// previous message of conn onward (DELAY.md §4.2 receiver-limited censoring).
// It does nothing on a conn without the delay header.
func NoteTransportDelayHandoff(conn H1MessageConn, handoff time.Duration) {
	if readLane := h1TransportDelayReadLane(conn); readLane != nil {
		readLane.noteHandoff(handoff)
	}
}

func h1TransportDelayReadLane(conn H1MessageConn) *transportDelayReadLane {
	for {
		switch c := conn.(type) {
		case *transportDelayWebSocketConn:
			return c.readLane
		case *strategyDeliveryMessageConn:
			conn = c.H1MessageConn
		case *FramedMessageConn:
			return c.delayRead
		default:
			return nil
		}
	}
}

func (self *transportDelayH1) noteHandoff(handoff time.Duration) {
	self.readLane.noteHandoff(handoff)
}

// transportDelayWebSocketConn carries the delay header in each binary
// WebSocket message. Control frames and text messages pass through unstamped.
type transportDelayWebSocketConn struct {
	H1MessageConn
	writeLane *transportDelayWriteLane
	readLane  *transportDelayReadLane

	stateLock      sync.Mutex
	batching       bool
	batchWrite     transportDelayWrite
	batchByteCount int
}

// SetReadLimit bounds the payload of a message. The header rides inside each
// WebSocket message, so the wrapped conn's limit counts it too.
func (self *transportDelayWebSocketConn) SetReadLimit(limit int64) {
	if 0 < limit {
		// Gorilla treats a negative limit as unlimited; saturate the wire cap.
		limit += min(int64(transportDelayHeaderByteCount), math64MaxInt-limit)
	}
	self.H1MessageConn.SetReadLimit(limit)
}

// webSocketMessageWireByteCount estimates the bytes a binary message of n
// payload bytes takes on the wire: the frame header plus a client mask.
func webSocketMessageWireByteCount(n int) int {
	switch {
	case n < 126:
		return n + 2 + 4
	case n < 65536:
		return n + 4 + 4
	default:
		return n + 10 + 4
	}
}

func (self *transportDelayWebSocketConn) WriteMessage(kind int, message []byte) error {
	if kind != websocket.BinaryMessage {
		return self.H1MessageConn.WriteMessage(kind, message)
	}
	self.stateLock.Lock()
	batching := self.batching
	write := self.batchWrite
	self.stateLock.Unlock()
	if !batching {
		write = self.writeLane.beginWrite()
	}
	err := writeH1MessageWithHeader(self.H1MessageConn, write.header[:], message)
	byteCount := webSocketMessageWireByteCount(transportDelayHeaderByteCount + len(message))
	if batching {
		// a failure inside a batch surfaces at its flush
		self.stateLock.Lock()
		if self.batching {
			self.batchByteCount += byteCount
		}
		self.stateLock.Unlock()
	} else {
		self.writeLane.endWrite(write, byteCount, err)
	}
	return err
}

// beginWriteBatch stamps the ready batch WebSocketWriteBatchConn is starting.
func (self *transportDelayWebSocketConn) beginWriteBatch() {
	write := self.writeLane.beginWrite()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.batching = true
	self.batchWrite = write
	self.batchByteCount = 0
}

// endWriteBatch records the batch's one physical write.
func (self *transportDelayWebSocketConn) endWriteBatch(err error) {
	self.stateLock.Lock()
	if !self.batching {
		self.stateLock.Unlock()
		return
	}
	self.batching = false
	write := self.batchWrite
	byteCount := self.batchByteCount
	self.stateLock.Unlock()
	self.writeLane.endWrite(write, byteCount, err)
}

// writeH1MessageWithHeader writes header and message as one binary message,
// with no extra copy when the WebSocket exposes a message writer.
func writeH1MessageWithHeader(conn H1MessageConn, header []byte, message []byte) error {
	if writerConn, ok := unobservedH1MessageConn(conn).(interface {
		NextWriter(int) (io.WriteCloser, error)
	}); ok {
		w, err := writerConn.NextWriter(websocket.BinaryMessage)
		if err != nil {
			return err
		}
		if _, err := w.Write(header); err != nil {
			w.Close()
			return err
		}
		if _, err := w.Write(message); err != nil {
			w.Close()
			return err
		}
		return w.Close()
	}
	combined := MessagePoolGet(len(header) + len(message))
	defer MessagePoolReturn(combined)
	copy(combined, header)
	copy(combined[len(header):], message)
	return conn.WriteMessage(websocket.BinaryMessage, combined)
}

// readPooledMessage strips the header of one binary message. The returned
// message is a pooled buffer of the payload alone.
func (self *transportDelayWebSocketConn) readPooledMessage(limit int64) (int, []byte, error) {
	kind, reader, err := self.H1MessageConn.NextReader()
	if err != nil || kind != websocket.BinaryMessage {
		return kind, nil, err
	}
	var header transportDelayHeader
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return kind, nil, err
	}
	self.readLane.receive(header[:], 0)
	message, err := MessagePoolReadAllLimit(reader, limit)
	if err == nil {
		self.readLane.addReceiveByteCount(webSocketMessageWireByteCount(transportDelayHeaderByteCount + len(message)))
	}
	return kind, message, err
}

// ReadMessage follows Gorilla's unpooled ownership convention.
func (self *transportDelayWebSocketConn) ReadMessage() (int, []byte, error) {
	kind, message, err := self.readPooledMessage(math64MaxInt)
	if err != nil || message == nil {
		return kind, nil, err
	}
	defer MessagePoolReturn(message)
	return kind, slices.Clone(message), nil
}

// NextReader strips the header and returns a reader of the payload.
func (self *transportDelayWebSocketConn) NextReader() (int, io.Reader, error) {
	kind, reader, err := self.H1MessageConn.NextReader()
	if err != nil || kind != websocket.BinaryMessage {
		return kind, reader, err
	}
	var header transportDelayHeader
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return kind, nil, err
	}
	self.readLane.receive(header[:], 0)
	return kind, reader, nil
}

const math64MaxInt = int64(^uint64(0) >> 1)
