package extender

// The feed over websocket (lag.dev/PLAN.md M9).
//
// A browser cannot open the feed service of an extender carrier (EXTENDER.md
// A8, D4), so the measurement API mounts this handler at its feed path, which
// upgrades to a websocket and serves exactly the same feed: the extender's
// FeedConnHandler -- the gossip feed server -- runs over the websocket as it
// runs over a carrier stream. Nothing of the feed is reimplemented here; the
// websocket is only framed differently. The browser's first binary message is
// the ExtenderFeedRequest, and each ExtenderFeedFrame the server writes leaves
// as one binary message, without the 4-byte length prefix the stream framing
// carries. The vantage is the connection's remote address prefix, which the
// feed server reads from the stream's remote address (R2), and the stream's
// remote address here is the browser's, as on a carrier.
//
// The API checks the origin before the handler is reached, as it checks every
// upgrade's (M2), so the upgrader here admits any. A feed stream is one action
// of its source subnet under the extender's admission limits (EXTENDER.md
// A12), as on the carriers. The handler is safe for concurrent use.

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/urnetwork/connect"
)

// How long the closing handshake of a finished feed may take.
const extenderLagFeedCloseTimeout = 1 * time.Second

type extenderLagFeedHandler struct {
	server          *ExtenderServer
	feedConnHandler func(conn net.Conn)
	upgrader        *websocket.Upgrader
}

func newExtenderLagFeedHandler(server *ExtenderServer, feedConnHandler func(conn net.Conn)) *extenderLagFeedHandler {
	handshakeTimeout := server.settings.HeaderTimeout
	if handshakeTimeout <= 0 {
		handshakeTimeout = DefaultExtenderSettings().HeaderTimeout
	}
	return &extenderLagFeedHandler{
		server:          server,
		feedConnHandler: feedConnHandler,
		upgrader: &websocket.Upgrader{
			HandshakeTimeout: handshakeTimeout,
			// the API admitted the origin before this was reached (M2)
			CheckOrigin: func(*http.Request) bool {
				return true
			},
			Error: func(w http.ResponseWriter, r *http.Request, status int, reason error) {
				w.WriteHeader(status)
			},
		},
	}
}

// Serves one feed stream over a websocket until the feed server is done with
// it (M9).
func (self *extenderLagFeedHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	// one feed stream is one action of its subnet, as on the carriers
	// (EXTENDER.md A12)
	if limit, retryAfter := self.server.admitAction(r.RemoteAddr, false); limit != extenderAdmitted {
		if seconds := int64((retryAfter + time.Second - 1) / time.Second); 0 < seconds {
			w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
		}
		w.WriteHeader(http.StatusTooManyRequests)
		self.server.reportError(extenderAdmissionStage(limit), fmt.Errorf("the source is over its admission limit"))
		return
	}

	// the 101 carries what the API set on the response, and is never stored
	// (M2)
	responseHeader := w.Header().Clone()
	responseHeader.Set("Cache-Control", "no-store")
	ws, err := self.upgrader.Upgrade(w, r, responseHeader)
	if err != nil {
		// the upgrader answered the request
		self.server.reportError("lag feed upgrade", err)
		return
	}
	conn := newExtenderLagFeedConn(ws)
	defer conn.Close()
	// a hijacked connection outlives its http server's close, so the stream
	// ends with the request, the api or the extender, whichever ends first
	defer context.AfterFunc(r.Context(), func() {
		conn.Close()
	})()
	defer context.AfterFunc(self.server.ctx, func() {
		conn.Close()
	})()

	// the feed server owns the stream until it returns (EXTENDER.md A8)
	self.feedConnHandler(conn)
	conn.closeNormally()
}

// One websocket as the byte stream the feed server speaks (D4, M9). The
// client's first binary message reads as the one length-prefixed request frame
// of the stream framing, and each length-prefixed frame the server writes
// leaves as one binary message without its prefix. A message after the request
// reads as an error, which the feed server takes as the client leaving -- on a
// carrier stream the client sends nothing after its request either. Reads and
// writes may run concurrently with each other, as the feed server runs them;
// each is serialized with itself. No error it returns names an address, since
// the feed server may log it (amendment 9).
type extenderLagFeedConn struct {
	ws *websocket.Conn

	readLock    sync.Mutex
	requestRead bool
	// the rest of the request frame not read yet
	readBytes []byte

	writeLock sync.Mutex
	// a frame written in parts, until it is whole
	writeBytes []byte

	closeOnce sync.Once
}

func newExtenderLagFeedConn(ws *websocket.Conn) *extenderLagFeedConn {
	// the request is one frame, bounded as on a carrier (D4)
	ws.SetReadLimit(int64(connect.ExtenderFeedMaxFrameByteCount))
	return &extenderLagFeedConn{
		ws: ws,
	}
}

var errExtenderLagFeedRequestType = errors.New("the feed request must be a binary message")
var errExtenderLagFeedAfterRequest = errors.New("the feed client sent a message after its request")

// An error of the feed's websocket as this conn returns it: with no address in
// its text (amendment 9). The feed server logs what a read of the request
// returns, and a socket error's text names both ends of the connection, the
// browser's address among them. A *net.OpError's text does, and so does the
// copy gorilla makes of a temporary one. Any net.Error comes back as an
// extenderLagFeedNetError, which keeps Timeout(), and errors.Is and errors.As
// through it. io.EOF comes back unchanged. A close from the browser keeps
// its code but not its reason, which is the page's own text. Every other
// error, gorilla's protocol errors and this conn's own, names no address and
// comes back unchanged.
func extenderLagFeedConnError(err error) error {
	if err == nil || err == io.EOF {
		return err
	}
	closeErr := &websocket.CloseError{}
	if errors.As(err, &closeErr) {
		return &websocket.CloseError{Code: closeErr.Code}
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return &extenderLagFeedNetError{err: err}
	}
	return err
}

// A socket error of the feed's websocket with the addresses left out of its
// text (amendment 9). The text keeps a *net.OpError's operation, network and
// cause, as in "read tcp: i/o timeout"; without one, it says only whether the
// error is a timeout.
type extenderLagFeedNetError struct {
	err error
}

func (self *extenderLagFeedNetError) Error() string {
	opErr := &net.OpError{}
	if errors.As(self.err, &opErr) && opErr.Err != nil {
		return (&net.OpError{Op: opErr.Op, Net: opErr.Net, Err: opErr.Err}).Error()
	}
	if self.Timeout() {
		return "i/o timeout"
	}
	return "network error"
}

func (self *extenderLagFeedNetError) Timeout() bool {
	var netErr net.Error
	return errors.As(self.err, &netErr) && netErr.Timeout()
}

// The deprecated half of net.Error, kept as the error it wraps has it.
func (self *extenderLagFeedNetError) Temporary() bool {
	var netErr net.Error
	return errors.As(self.err, &netErr) && netErr.Temporary()
}

func (self *extenderLagFeedNetError) Unwrap() error {
	return self.err
}

func (self *extenderLagFeedConn) Read(b []byte) (int, error) {
	self.readLock.Lock()
	defer self.readLock.Unlock()

	if len(self.readBytes) == 0 {
		messageType, messageBytes, err := self.ws.ReadMessage()
		if err != nil {
			return 0, extenderLagFeedConnError(err)
		}
		if self.requestRead {
			return 0, errExtenderLagFeedAfterRequest
		}
		if messageType != websocket.BinaryMessage {
			return 0, errExtenderLagFeedRequestType
		}
		// the request as the stream framing carries it: a 4-byte big-endian
		// length and the message
		frameBytes := make([]byte, 4+len(messageBytes))
		binary.BigEndian.PutUint32(frameBytes[0:4], uint32(len(messageBytes)))
		copy(frameBytes[4:], messageBytes)
		self.readBytes = frameBytes
		self.requestRead = true
	}
	n := copy(b, self.readBytes)
	self.readBytes = self.readBytes[n:]
	if len(self.readBytes) == 0 {
		self.readBytes = nil
	}
	return n, nil
}

func (self *extenderLagFeedConn) Write(b []byte) (int, error) {
	self.writeLock.Lock()
	defer self.writeLock.Unlock()

	self.writeBytes = append(self.writeBytes, b...)
	for 4 <= len(self.writeBytes) {
		frameByteCount := int(binary.BigEndian.Uint32(self.writeBytes[0:4]))
		if connect.ExtenderFeedMaxFrameByteCount < frameByteCount {
			return 0, fmt.Errorf(
				"extender feed frame is %d bytes, at most %d",
				frameByteCount,
				connect.ExtenderFeedMaxFrameByteCount,
			)
		}
		if len(self.writeBytes) < 4+frameByteCount {
			// the rest of the frame is in a later write
			break
		}
		if err := self.ws.WriteMessage(websocket.BinaryMessage, self.writeBytes[4:4+frameByteCount]); err != nil {
			return 0, extenderLagFeedConnError(err)
		}
		self.writeBytes = self.writeBytes[4+frameByteCount:]
	}
	if len(self.writeBytes) == 0 {
		self.writeBytes = nil
	}
	return len(b), nil
}

// Ends a finished feed with a normal closure, which a browser reads as the
// feed ending rather than failing. A client already gone makes it fail
// harmlessly.
func (self *extenderLagFeedConn) closeNormally() {
	self.ws.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(extenderLagFeedCloseTimeout),
	)
}

func (self *extenderLagFeedConn) Close() error {
	var err error
	self.closeOnce.Do(func() {
		err = extenderLagFeedConnError(self.ws.Close())
	})
	return err
}

func (self *extenderLagFeedConn) LocalAddr() net.Addr {
	return self.ws.LocalAddr()
}

// The browser's address, which is the connection the websocket was upgraded
// on: the feed server's vantage (R2).
func (self *extenderLagFeedConn) RemoteAddr() net.Addr {
	return self.ws.RemoteAddr()
}

func (self *extenderLagFeedConn) SetDeadline(t time.Time) error {
	if err := self.ws.SetReadDeadline(t); err != nil {
		return extenderLagFeedConnError(err)
	}
	return extenderLagFeedConnError(self.ws.SetWriteDeadline(t))
}

func (self *extenderLagFeedConn) SetReadDeadline(t time.Time) error {
	return extenderLagFeedConnError(self.ws.SetReadDeadline(t))
}

func (self *extenderLagFeedConn) SetWriteDeadline(t time.Time) error {
	return extenderLagFeedConnError(self.ws.SetWriteDeadline(t))
}
