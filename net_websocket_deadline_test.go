// The actual pinned WebSocket writer ignores its underlying deadline error.
// These controls pin the owned adapter's terminal boundary without networking.
package connect

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gorilla/websocket"
)

// Produces a valid synthetic upgrade response, then records delegated I/O.
// A failed installation deliberately leaves I/O usable until Close is called.
type webSocketDeadlineConn struct {
	stateLock    sync.Mutex
	request      bytes.Buffer
	response     *bytes.Reader
	upgraded     bool
	readErr      error
	writeErr     error
	clearErr     error
	readCount    int
	writeCount   int
	closed       bool
	closedOnce   sync.Once
	closedNotify chan struct{}
	writeStarted chan struct{}
}

// The initial request contains only invented host/header data from the dialer.
func (self *webSocketDeadlineConn) Read(message []byte) (int, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		return 0, net.ErrClosed
	}
	if !self.upgraded {
		request, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(self.request.Bytes())))
		if err != nil {
			return 0, err
		}
		accept := sha1.Sum([]byte(request.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		self.response = bytes.NewReader([]byte(fmt.Sprintf("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(accept[:]))))
		self.upgraded = true
	}
	if self.response.Len() != 0 {
		return self.response.Read(message)
	}
	self.readCount++
	return 0, io.EOF
}

// The blocking branch proves deadline failure cannot wait behind writer locks.
func (self *webSocketDeadlineConn) Write(message []byte) (int, error) {
	self.stateLock.Lock()
	if self.closed {
		self.stateLock.Unlock()
		return 0, net.ErrClosed
	}
	if !self.upgraded {
		n, err := self.request.Write(message)
		self.stateLock.Unlock()
		return n, err
	}
	self.writeCount++
	started := self.writeStarted
	self.stateLock.Unlock()
	if started != nil {
		close(started)
		<-self.closedNotify
		return 0, net.ErrClosed
	}
	return len(message), nil
}

// Close joins no worker and can interrupt the already-entered delegated Write.
func (self *webSocketDeadlineConn) Close() error {
	self.stateLock.Lock()
	self.closed = true
	self.stateLock.Unlock()
	self.closedOnce.Do(func() { close(self.closedNotify) })
	return nil
}

// No actual network identities are used.
func (self *webSocketDeadlineConn) LocalAddr() net.Addr { return nil }

// No actual peer identities are used.
func (self *webSocketDeadlineConn) RemoteAddr() net.Addr { return nil }

// Combined and clearing deadline failures are independently injectable.
func (self *webSocketDeadlineConn) SetDeadline(deadline time.Time) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if deadline.IsZero() && self.clearErr != nil {
		return self.clearErr
	}
	if self.readErr != nil {
		return self.readErr
	}
	return self.writeErr
}

// Read errors are returned without changing the synthetic byte source.
func (self *webSocketDeadlineConn) SetReadDeadline(time.Time) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.readErr
}

// Write errors do not by themselves prevent a delegated write in the RED case.
func (self *webSocketDeadlineConn) SetWriteDeadline(time.Time) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.writeErr
}

// Uses the actual dependency's handshake, WriteMessage and WriteControl paths.
func newWebSocketDeadlineFixture(t *testing.T) (*websocket.Conn, *WebSocketWriteBatchConn, *webSocketDeadlineConn) {
	t.Helper()
	raw := &webSocketDeadlineConn{closedNotify: make(chan struct{})}
	adapter := NewWebSocketWriteBatchConn(raw)
	dialer := &websocket.Dialer{HandshakeTimeout: time.Second, NetDialContext: func(context.Context, string, string) (net.Conn, error) { return adapter, nil }}
	ws, _, err := dialer.DialContext(context.Background(), "ws://deadline.example/socket", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	return ws, adapter, raw
}

// Public SetWriteDeadline returns nil, so the actual write is the discriminator.
func TestWebSocketDeadlineActualMessageRejectsInnerError(t *testing.T) {
	ws, _, raw := newWebSocketDeadlineFixture(t)
	raw.writeErr = errors.New("synthetic inner write deadline rejected")
	if err := ws.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	err := ws.WriteMessage(websocket.BinaryMessage, []byte("synthetic payload"))
	if !errors.Is(err, raw.writeErr) || raw.writeCount != 0 || !raw.closed {
		t.Fatalf("inner deadline result=%v writes=%d closed=%t", err, raw.writeCount, raw.closed)
	}
}

// Gorilla's separate WriteControl path must not bypass the same terminal latch.
func TestWebSocketDeadlineActualControlRejectsInnerError(t *testing.T) {
	ws, _, raw := newWebSocketDeadlineFixture(t)
	raw.writeErr = errors.New("synthetic inner control deadline rejected")
	err := ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(time.Second))
	if !errors.Is(err, raw.writeErr) || raw.writeCount != 0 || !raw.closed {
		t.Fatalf("control deadline result=%v writes=%d closed=%t", err, raw.writeCount, raw.closed)
	}
}

// Previously retained bytes cannot escape through a later flush after rejection.
func TestWebSocketDeadlineBufferedFramesAreRetired(t *testing.T) {
	ws, adapter, raw := newWebSocketDeadlineFixture(t)
	adapter.BeginWriteBatch()
	if err := ws.WriteMessage(websocket.BinaryMessage, []byte("first complete frame")); err != nil {
		t.Fatal(err)
	}
	if len(adapter.writeBuffer) == 0 || raw.writeCount != 0 {
		t.Fatal("fixture did not retain a complete ready frame")
	}
	raw.writeErr = errors.New("synthetic second-frame deadline rejected")
	err := ws.WriteMessage(websocket.BinaryMessage, []byte("second complete frame"))
	flushErr := adapter.FlushWriteBatch()
	if !errors.Is(err, raw.writeErr) || !errors.Is(flushErr, raw.writeErr) || raw.writeCount != 0 || len(adapter.writeBuffer) != 0 {
		t.Fatalf("retired batch result=%v flush=%v writes=%d retained=%d", err, flushErr, raw.writeCount, len(adapter.writeBuffer))
	}
}

// A later nil setter cannot revive a socket whose liveness bound was rejected.
func TestWebSocketDeadlineRejectionCannotBeCleared(t *testing.T) {
	_, adapter, raw := newWebSocketDeadlineFixture(t)
	want := errors.New("synthetic sticky deadline error")
	raw.writeErr = want
	if err := adapter.SetWriteDeadline(time.Now()); !errors.Is(err, want) {
		t.Fatal(err)
	}
	raw.writeErr = nil
	setterErr := adapter.SetWriteDeadline(time.Time{})
	n, writeErr := adapter.Write([]byte("must not be delegated"))
	if !errors.Is(setterErr, want) || !errors.Is(writeErr, want) || n != 0 || raw.writeCount != 0 || !raw.closed {
		t.Fatalf("rejection recovered: setter=%v write=%v bytes=%d writes=%d", setterErr, writeErr, n, raw.writeCount)
	}
}

// A rejected reader deadline invalidates this owned full-duplex connection too.
func TestWebSocketDeadlineReadFailurePreventsIo(t *testing.T) {
	_, adapter, raw := newWebSocketDeadlineFixture(t)
	raw.readErr = errors.New("synthetic read deadline rejected")
	if err := adapter.SetReadDeadline(time.Now()); !errors.Is(err, raw.readErr) {
		t.Fatal(err)
	}
	n, err := adapter.Read(make([]byte, 1))
	if !errors.Is(err, raw.readErr) || n != 0 || raw.readCount != 0 || !raw.closed {
		t.Fatalf("read rejection result=%v reads=%d closed=%t", err, raw.readCount, raw.closed)
	}
}

// A dependency may ignore its final deadline clear; no later I/O is allowed.
func TestWebSocketDeadlineCombinedFailurePreventsIo(t *testing.T) {
	_, adapter, raw := newWebSocketDeadlineFixture(t)
	raw.clearErr = errors.New("synthetic deadline clear rejected")
	if err := adapter.SetDeadline(time.Time{}); !errors.Is(err, raw.clearErr) {
		t.Fatal(err)
	}
	n, err := adapter.Write([]byte("must not be delegated"))
	if !errors.Is(err, raw.clearErr) || n != 0 || raw.writeCount != 0 || !raw.closed {
		t.Fatalf("combined rejection result=%v writes=%d closed=%t", err, raw.writeCount, raw.closed)
	}
}

// The setter must interrupt rather than wait on a writer holding stateLock.
func TestWebSocketDeadlineFailureInterruptsBlockedWriter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, adapter, raw := newWebSocketDeadlineFixture(t)
		raw.writeStarted = make(chan struct{})
		writeDone := make(chan error, 1)
		go func() { _, err := adapter.Write([]byte("blocked write")); writeDone <- err }()
		<-raw.writeStarted
		raw.stateLock.Lock()
		raw.writeErr = errors.New("synthetic concurrent deadline rejected")
		raw.stateLock.Unlock()
		setterDone := make(chan error, 1)
		go func() { setterDone <- adapter.SetWriteDeadline(time.Now()) }()
		synctest.Wait()
		select {
		case err := <-setterDone:
			if !errors.Is(err, raw.writeErr) {
				t.Error(err)
			}
		default:
			t.Error("deadline setter waited behind delegated write")
		}
		select {
		case err := <-writeDone:
			if err == nil {
				t.Error("blocked write returned success")
			}
		default:
			t.Error("deadline failure did not interrupt delegated write")
			raw.Close()
			<-writeDone
		}
	})
}

// A nil-error installation preserves actual WebSocket frame and control writes.
func TestWebSocketDeadlineHealthyActualWriter(t *testing.T) {
	ws, _, raw := newWebSocketDeadlineFixture(t)
	if err := ws.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteMessage(websocket.BinaryMessage, []byte("healthy payload")); err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if raw.writeCount != 2 || raw.closed {
		t.Fatalf("healthy writes=%d closed=%t", raw.writeCount, raw.closed)
	}
}

// Successful clear keeps the socket usable and an empty flush performs no I/O.
func TestWebSocketDeadlineHealthyClearAndEmptyBatch(t *testing.T) {
	_, adapter, raw := newWebSocketDeadlineFixture(t)
	if err := adapter.SetDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	adapter.BeginWriteBatch()
	if err := adapter.FlushWriteBatch(); err != nil {
		t.Fatal(err)
	}
	if raw.writeCount != 0 || raw.closed {
		t.Fatal("healthy empty batch changed the socket")
	}
}
