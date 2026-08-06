package connect

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestClientDialerHttpClientUsesHttp2WithCustomTlsDialer(t *testing.T) {
	connectionCount := atomic.Int32{}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	server.EnableHTTP2 = true
	server.Config.ConnState = func(conn net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connectionCount.Add(1)
		}
	}
	server.StartTLS()
	defer server.Close()

	serverTransport, ok := server.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("unexpected test server transport type %T", server.Client().Transport)
	}
	tlsConfig := serverTransport.TLSClientConfig.Clone()
	settings := DefaultClientStrategySettings()
	settings.TlsConfig = tlsConfig
	dialer := &clientDialer{
		dialTlsContext:     newNormalDialTlsContext(settings, nil),
		httpDialTlsContext: newNormalDialTlsContext(settings, clientHttpNextProtos),
		settings:           settings,
	}
	client := dialer.HttpClient()
	defer client.CloseIdleConnections()

	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("warm request: %s", err)
	}
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		response.Body.Close()
		t.Fatalf("read warm response: %s", err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatalf("close warm response: %s", err)
	}
	if response.ProtoMajor != 2 {
		t.Fatalf("custom TLS dialer negotiated HTTP/%d, expected HTTP/2", response.ProtoMajor)
	}

	const requestCount = 16
	start := make(chan struct{})
	errs := make(chan error, requestCount)
	var waitGroup sync.WaitGroup
	for range requestCount {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			<-start
			request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
			if err != nil {
				errs <- err
				return
			}
			response, err := client.Do(request)
			if err != nil {
				errs <- err
				return
			}
			_, readErr := io.Copy(io.Discard, response.Body)
			closeErr := response.Body.Close()
			if readErr != nil {
				errs <- readErr
				return
			}
			if closeErr != nil {
				errs <- closeErr
			}
		}()
	}
	close(start)
	waitGroup.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("parallel request: %s", err)
		}
	}

	if got := connectionCount.Load(); got != 1 {
		t.Fatalf("HTTP/2 requests used %d TLS connections, expected one", got)
	}
}

func TestClientDialerWebSocketForcesHttp11Alpn(t *testing.T) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		_ = connection.WriteMessage(websocket.TextMessage, []byte("ok"))
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	serverTransport, ok := server.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("unexpected test server transport type %T", server.Client().Transport)
	}
	tlsConfig := serverTransport.TLSClientConfig.Clone()
	// Model a caller that already enabled h2 on its shared base config. The
	// WebSocket-specific derivative must replace this list, not inherit it.
	tlsConfig.NextProtos = []string{"h2", "http/1.1"}
	settings := DefaultClientStrategySettings()
	settings.TlsConfig = tlsConfig
	dialer := &clientDialer{
		dialTlsContext:     newNormalDialTlsContext(settings, clientWebSocketNextProtos),
		httpDialTlsContext: newNormalDialTlsContext(settings, clientHttpNextProtos),
		settings:           settings,
	}

	webSocketUrl := "wss" + strings.TrimPrefix(server.URL, "https")
	connection, response, err := dialer.WsDialer(settings).DialContext(
		context.Background(),
		webSocketUrl,
		nil,
	)
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err != nil {
		t.Fatalf("WebSocket dial against an h2-capable server: %s", err)
	}
	defer connection.Close()

	batchConnection, ok := connection.UnderlyingConn().(*webSocketWriteBatchConn)
	if !ok {
		t.Fatalf("unexpected WebSocket transport type %T", connection.UnderlyingConn())
	}
	tlsConnection, ok := batchConnection.conn.(*tls.Conn)
	if !ok {
		t.Fatalf("unexpected batched WebSocket transport type %T", batchConnection.conn)
	}
	if negotiated := tlsConnection.ConnectionState().NegotiatedProtocol; negotiated == "h2" {
		t.Fatal("WebSocket negotiated h2 underneath its HTTP/1.1 upgrade")
	}
	_, message, err := connection.ReadMessage()
	if err != nil {
		t.Fatalf("read WebSocket message: %s", err)
	}
	if string(message) != "ok" {
		t.Fatalf("WebSocket message = %q, expected ok", message)
	}
}

func TestClientDialerWebSocketBatchPreservesMessageBoundaries(t *testing.T) {
	const messageCount = platformWebSocketWriteBatchMaxMessages
	received := make(chan [][]byte, 1)
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer connection.Close()

		messages := make([][]byte, 0, messageCount)
		for range messageCount {
			messageType, message, err := connection.ReadMessage()
			if err != nil {
				return
			}
			if messageType != websocket.BinaryMessage {
				return
			}
			messages = append(messages, message)
		}
		received <- messages
	}))
	server.StartTLS()
	defer server.Close()

	serverTransport, ok := server.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("unexpected test server transport type %T", server.Client().Transport)
	}
	settings := DefaultClientStrategySettings()
	settings.TlsConfig = serverTransport.TLSClientConfig.Clone()
	dialer := &clientDialer{
		dialTlsContext: newNormalDialTlsContext(
			settings,
			clientWebSocketNextProtos,
		),
		settings: settings,
	}
	webSocketUrl := "wss" + strings.TrimPrefix(server.URL, "https")
	connection, response, err := dialer.WsDialer(settings).DialContext(
		context.Background(),
		webSocketUrl,
		nil,
	)
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()

	batchConnection, ok := connection.UnderlyingConn().(*webSocketWriteBatchConn)
	if !ok {
		t.Fatalf("unexpected WebSocket transport type %T", connection.UnderlyingConn())
	}
	expected := make([][]byte, 0, messageCount)
	batchConnection.beginWriteBatch()
	for i := range messageCount {
		message := []byte{byte(i), byte(i + 1), byte(i + 2)}
		expected = append(expected, message)
		if err := connection.WriteMessage(websocket.BinaryMessage, message); err != nil {
			t.Fatal(err)
		}
	}
	if err := batchConnection.flushWriteBatch(); err != nil {
		t.Fatal(err)
	}

	select {
	case actual := <-received:
		if len(actual) != len(expected) {
			t.Fatalf("received %d messages, expected %d", len(actual), len(expected))
		}
		for i := range expected {
			if !bytes.Equal(actual[i], expected[i]) {
				t.Fatalf("message %d = %x, expected %x", i, actual[i], expected[i])
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not receive the coalesced WebSocket messages")
	}
}

func TestClientTlsConfigsUseIndependentBoundedSessionCaches(t *testing.T) {
	callerCache := tls.NewLRUClientSessionCache(1)
	base := &tls.Config{
		ClientSessionCache: callerCache,
		NextProtos:         []string{"caller"},
	}

	httpConfig := newClientTlsConfig(base, clientHttpNextProtos)
	webSocketConfig := newClientTlsConfig(base, clientWebSocketNextProtos)

	if httpConfig.ClientSessionCache == nil || webSocketConfig.ClientSessionCache == nil {
		t.Fatal("derived TLS configs must carry bounded session caches")
	}
	if httpConfig.ClientSessionCache == webSocketConfig.ClientSessionCache {
		t.Fatal("HTTP and WebSocket TLS paths unexpectedly share a session cache")
	}
	if httpConfig.ClientSessionCache == callerCache || webSocketConfig.ClientSessionCache == callerCache {
		t.Fatal("derived TLS path reused the caller's cross-path session cache")
	}
	if base.ClientSessionCache != callerCache {
		t.Fatal("newClientTlsConfig mutated the caller's session cache")
	}
	if got := strings.Join(httpConfig.NextProtos, ","); got != "h2,http/1.1" {
		t.Fatalf("HTTP ALPN list = %q, expected h2,http/1.1", got)
	}
	if got := strings.Join(webSocketConfig.NextProtos, ","); got != "http/1.1" {
		t.Fatalf("WebSocket ALPN list = %q, expected http/1.1", got)
	}
	if got := strings.Join(base.NextProtos, ","); got != "caller" {
		t.Fatalf("newClientTlsConfig mutated caller ALPN list to %q", got)
	}
}
