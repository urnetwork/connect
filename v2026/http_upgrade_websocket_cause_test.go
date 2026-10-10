// WebSocket fallback transfers one socket owner through Gorilla, cancellation
// and the production batching boundary without losing original cleanup causes.
package connect

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
)

// Cancellation begins inside the actual response read, so Gorilla cleanup and
// the cancellation callback must share one underlying close outcome.
func TestDialH1MessagesWebSocketCancellationRetainsOriginalCauses(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	stopCause := errors.New("synthetic websocket caller stop")
	readCause := errors.New("synthetic websocket response read failure")
	closeCause := errors.New("synthetic websocket close failure")
	physical := &httpUpgradeReadFailureTestConn{h1UpgradeScriptConn: newH1UpgradeScriptConn(nil), failure: readCause, cancel: func() { cancel(stopCause) }}
	connection := &httpUpgradeCloseFailureTestConn{Conn: physical, failure: closeCause}
	result, err := DialH1Messages(ctx, "ws://canceled-websocket.example/", nil, h1UpgradeScriptDialer(connection, nil), H1FramerProtocol, 1024, false, nil)
	if result != nil {
		result.Close()
		t.Fatal("canceled websocket returned a connection")
	}
	if !errors.Is(err, context.Canceled) || !errors.Is(err, stopCause) || !errors.Is(err, readCause) || !errors.Is(err, closeCause) || connection.closes.Load() != 1 {
		t.Fatal("websocket cancellation lost an original cause or closed twice", connection.closes.Load())
	}
}

// Ordinary Gorilla failure cleanup returns its original close error even when
// no caller cancellation occurs; the outer owner still closes returned bodies.
func TestDialH1MessagesWebSocketFailureRetainsOriginalCloseCause(t *testing.T) {
	readCause := errors.New("synthetic websocket read failure")
	closeCause := errors.New("synthetic websocket failed-handshake close failure")
	for _, item := range []struct {
		connection net.Conn
		cause      error
	}{
		{connection: &httpUpgradeReadFailureTestConn{h1UpgradeScriptConn: newH1UpgradeScriptConn(nil), failure: readCause}, cause: readCause},
		{connection: newH1UpgradeScriptConn([]byte("HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n")), cause: websocket.ErrBadHandshake},
	} {
		connection := &httpUpgradeCloseFailureTestConn{Conn: item.connection, failure: closeCause}
		result, err := DialH1Messages(t.Context(), "ws://failed-websocket.example/", nil, h1UpgradeScriptDialer(connection, nil), H1FramerProtocol, 1024, false, nil)
		if result != nil {
			result.Close()
			t.Fatal("failed websocket returned a connection")
		}
		if !errors.Is(err, item.cause) || !errors.Is(err, closeCause) || connection.closes.Load() != 1 {
			t.Fatal("websocket failure lost original close custody", connection.closes.Load())
		}
	}
}

// A custom failed dial can return a socket; the guard owns that socket's
// release before Gorilla can return without entering handshake cleanup.
func TestDialH1MessagesWebSocketFailedDialReleasesOriginalConn(t *testing.T) {
	dialCause := errors.New("synthetic websocket dial failure")
	closeCause := errors.New("synthetic websocket failed-dial close failure")
	connection := &httpUpgradeCloseFailureTestConn{Conn: newH1UpgradeScriptConn(nil), failure: closeCause}
	dialer := &websocket.Dialer{NetDialContext: func(context.Context, string, string) (net.Conn, error) {
		return connection, dialCause
	}}
	result, err := DialH1Messages(t.Context(), "ws://failed-websocket-dial.example/", nil, dialer, H1FramerProtocol, 1024, false, nil)
	if result != nil {
		result.Close()
		t.Fatal("failed websocket dial returned a connection")
	}
	if !errors.Is(err, dialCause) || !errors.Is(err, closeCause) || connection.closes.Load() != 1 {
		t.Fatal("websocket failed dial lost original close custody", connection.closes.Load())
	}
}

// The real stream counts physical writes and closes below the strategy's
// batching wrapper, so no test-only substitute can provide batch authority.
type httpWebSocketTransferTestConn struct {
	net.Conn
	writes atomic.Int32
	closes atomic.Int32
}

func (self *httpWebSocketTransferTestConn) Write(raw []byte) (int, error) {
	self.writes.Add(1)
	return self.Conn.Write(raw)
}

func (self *httpWebSocketTransferTestConn) Close() error {
	self.closes.Add(1)
	return self.Conn.Close()
}

// The actual public strategy keeps the concrete batching capability used by
// PlatformTransport and transfers a successful socket before any owned close.
func TestHttpStrategyWebSocketCloseOwnerPreservesWriteBatching(t *testing.T) {
	finished := make(chan error, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		upgrader := websocket.Upgrader{}
		connection, err := upgrader.Upgrade(writer, request, nil)
		if err != nil {
			finished <- err
			return
		}
		defer connection.Close()
		for _, expected := range [][]byte{[]byte("synthetic-first"), []byte("synthetic-second")} {
			kind, raw, err := connection.ReadMessage()
			if err != nil {
				finished <- err
				return
			}
			if kind != websocket.BinaryMessage || !bytes.Equal(raw, expected) {
				finished <- errors.New("synthetic websocket batch changed message boundaries")
				return
			}
		}
		finished <- nil
		<-release
	}))
	defer server.Close()
	defer close(release)
	settings := DefaultClientStrategySettings()
	settings.EnableResilient = false
	var physical atomic.Pointer[httpWebSocketTransferTestConn]
	settings.DialContextSettings = &DialContextSettings{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		connection, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		owned := &httpWebSocketTransferTestConn{Conn: connection}
		physical.Store(owned)
		return owned, nil
	}}
	strategy := NewClientStrategy(t.Context(), settings)
	defer strategy.Close()
	connection, _, err := strategy.H1DialContextWithDialer(t.Context(), "ws"+strings.TrimPrefix(server.URL, "http"), nil, 1024, false, nil)
	if err != nil || connection == nil {
		t.Fatal("public strategy did not transfer successful websocket", err)
	}
	defer connection.Close()
	batch, ok := connection.UnderlyingConn().(*WebSocketWriteBatchConn)
	if !ok || physical.Load() == nil || physical.Load().closes.Load() != 0 {
		t.Fatal("close owner hid batching or closed a transferred socket")
	}
	writes := physical.Load().writes.Load()
	batch.BeginWriteBatch()
	for _, message := range [][]byte{[]byte("synthetic-first"), []byte("synthetic-second")} {
		if err := connection.WriteMessage(websocket.BinaryMessage, message); err != nil {
			t.Fatal(err)
		}
	}
	if physical.Load().writes.Load() != writes {
		t.Fatal("production websocket batching was bypassed")
	}
	if err := batch.FlushWriteBatch(); err != nil || physical.Load().writes.Load() != writes+1 {
		t.Fatal("ready websocket messages were not one physical batch", err)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	if err := connection.Close(); err != nil || physical.Load().closes.Load() != 1 {
		t.Fatal("successful websocket close was not transferred exactly once", err)
	}
	if err := connection.Close(); err != nil || physical.Load().closes.Load() != 1 {
		t.Fatal("repeated websocket cleanup closed the physical socket twice", err)
	}
}
