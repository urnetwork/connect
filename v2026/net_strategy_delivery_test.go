package connect

// Exercises the real strategy entry points with loopback payloads. Explicit
// handler barriers pin network transitions; clocks pin decay and expiry.

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// A single real route makes evidence attribution unambiguous.
func newDeliveryTestStrategy(t *testing.T, tlsConfig *tls.Config) (*ClientStrategy, *clientDialer) {
	t.Helper()
	settings := DefaultClientStrategySettings()
	settings.EnableNormal = true
	settings.EnableResilient = false
	settings.TlsConfig = tlsConfig
	settings.TlsClientHelloFingerprint = ""
	settings.RequestTimeout = 5 * time.Second
	settings.ReconnectTimeout = 0
	strategy := NewClientStrategy(t.Context(), settings)
	t.Cleanup(strategy.Close)
	if len(strategy.dialers) != 1 {
		t.Fatalf("got %d routes, want one", len(strategy.dialers))
	}
	for dialer := range strategy.dialers {
		return strategy, dialer
	}
	panic("missing test dialer")
}

// A real HTTP/2 payload installs one observation, then its weight and serial
// preference both expire even though the dialer's lifetime handshake succeeded.
func TestStrategyHttpPayloadRanksAndExpires(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), int(deliveryVerifiedByteCount))
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(payload) }))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	config := server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	strategy, dialer := newDeliveryTestStrategy(t, config)
	now := time.Unix(1700000000, 0)
	strategy.scores.now = func() time.Time { return now }
	strategy.SetNetworkId("synthetic-network-a")
	base := strategy.dialerWeights(false)[dialer]
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := strategy.HttpParallel(request)
	if err != nil {
		t.Fatal(err)
	}
	if result.response.ProtoMajor != 2 || !bytes.Equal(result.bodyBytes, payload) {
		t.Fatal("HTTP/2 payload was not preserved")
	}
	if strategy.dialerWeights(false)[dialer] <= base || !strategy.dialerDelivered(dialer) {
		t.Fatal("received payload never reached production ranking")
	}
	now = now.Add(strategyScoreTtl)
	if strategy.dialerWeights(false)[dialer] != base || strategy.dialerDelivered(dialer) {
		t.Fatal("lifetime handshake kept expired delivery evidence preferred")
	}
}

// A clean short response and a large outgoing body are both insufficient to
// prove inbound delivery. A short hello can still enable this serial POST.
func TestStrategyShortHttpResponseAndLargeWriteStayNeutral(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()
	strategy, dialer := newDeliveryTestStrategy(t, nil)
	request, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL, bytes.NewReader(make([]byte, deliveryVerifiedByteCount)))
	hello, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
	result, err := strategy.HttpSerial(request, hello)
	if err != nil || string(result.bodyBytes) != "ok" {
		t.Fatalf("short-response POST failed: %v", err)
	}
	if got := strategy.scores.weight("", dialer.dialerKey()); got != 1 {
		t.Fatalf("short response/write credited or penalized the route: %v", got)
	}
}

// The first response is deterministically truncated at 16 KiB. The next clean
// short response lets the real retry loop finish without changing that verdict.
func TestStrategyTruncatedHttpPayloadPenalizesRoute(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("Content-Length", fmt.Sprint(deliveryVerifiedByteCount))
			_, _ = w.Write(make([]byte, 16*1024))
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()
	strategy, dialer := newDeliveryTestStrategy(t, nil)
	strategy.scores.retainFailedWinner = func() bool { return false }
	request, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
	result, err := strategy.HttpParallel(request)
	if err != nil || string(result.bodyBytes) != "ok" {
		t.Fatalf("retry failed: %v", err)
	}
	if requests.Load() != 2 {
		t.Fatalf("got %d requests, want failure then recovery", requests.Load())
	}
	if got := strategy.scores.weight("", dialer.dialerKey()); got >= 1 {
		t.Fatalf("16 KiB truncation was not recorded: %v", got)
	}
}

// A real response starts on A and finishes only after the explicit switch to B.
func TestStrategyLateHttpPayloadKeepsAttemptNetwork(t *testing.T) {
	arrived, proceed := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(proceed) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		<-proceed
		_, _ = w.Write(make([]byte, deliveryVerifiedByteCount))
	}))
	defer server.Close()
	defer release()
	strategy, dialer := newDeliveryTestStrategy(t, nil)
	strategy.SetNetworkId("synthetic-network-a")
	completed := make(chan error, 1)
	go func() {
		request, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
		_, err := strategy.HttpParallel(request)
		completed <- err
	}()
	<-arrived
	strategy.SetNetworkId("synthetic-network-b")
	release()
	if err := <-completed; err != nil {
		t.Fatal(err)
	}
	if strategy.scores.weight(deriveNetworkId("synthetic-network-a"), dialer.dialerKey()) <= 1 {
		t.Fatal("old network did not receive its payload evidence")
	}
	if strategy.scores.weight(deriveNetworkId("synthetic-network-b"), dialer.dialerKey()) != 1 {
		t.Fatal("late A payload was credited to B")
	}
}

// The existing platform notification reaches the strategy without requiring a
// new SDK caller. Explicit stable identifiers can still revisit known networks.
func TestStrategyNetworkNotificationIsolatesDelivery(t *testing.T) {
	strategy, dialer := newDeliveryTestStrategy(t, nil)
	strategy.SetNetworkId("synthetic-network-a")
	info := strategy.dialerInfo(dialer)
	strategy.RecordDeliveryOutcome(info, deliveryVerifiedByteCount, false)
	NetworkChanged()
	if strategy.dialerDelivered(dialer) {
		t.Fatal("platform path change reused the old winner")
	}
	strategy.RecordDeliveryOutcome(info, deliveryVerifiedByteCount, false)
	if strategy.dialerDelivered(dialer) {
		t.Fatal("late duplicate completion credited the new path")
	}
	strategy.SetNetworkId("synthetic-network-a")
	if !strategy.dialerDelivered(dialer) {
		t.Fatal("stable network identity lost its own evidence")
	}
}

// Both negotiated H1 and WebSocket fallback aggregate actual framed payload
// across all read entry points. Handshakes alone do not install evidence.
func TestStrategyMessagePayloadKeepsAttemptNetwork(t *testing.T) {
	for _, framed := range []bool{false, true} {
		proceed := make(chan struct{})
		var releaseOnce sync.Once
		releasePeer := func() { releaseOnce.Do(func() { close(proceed) }) }
		serverErrors := make(chan error, 1)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var conn H1MessageConn
			var err error
			if framed {
				raw, upgradeErr := AcceptFramedUpgrade(w, r, H1FramerProtocol, time.Second)
				err = upgradeErr
				if err == nil {
					conn, err = NewFramedMessageConn(raw, H1FramerProtocol, 65535, nil)
				}
			} else {
				upgrader := websocket.Upgrader{}
				conn, err = upgrader.Upgrade(w, r, nil)
			}
			if err != nil {
				serverErrors <- err
				return
			}
			defer conn.Close()
			<-proceed
			for range 4 {
				if err := conn.WriteMessage(websocket.BinaryMessage, make([]byte, 16*1024)); err != nil {
					serverErrors <- err
					return
				}
			}
			serverErrors <- nil
		}))
		t.Cleanup(func() { releasePeer(); server.Close() })
		strategy, dialer := newDeliveryTestStrategy(t, nil)
		strategy.SetNetworkId("synthetic-network-a")
		conn, info, err := strategy.H1DialContextWithDialer(t.Context(), "ws"+strings.TrimPrefix(server.URL, "http"), nil, 65535, framed, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		if info.delivery == nil || strategy.dialerDelivered(dialer) {
			t.Fatalf("framed=%t handshake installed delivery evidence", framed)
		}
		stats := &H1ConnectionStats{}
		release := stats.connected(conn)
		if (stats.Snapshot().H1PlusConnectionCount == 1) != framed {
			t.Fatalf("framed=%t wrapper hid negotiated carrier", framed)
		}
		release()
		strategy.SetNetworkId("synthetic-network-b")
		releasePeer()
		_, first, err := conn.ReadMessage()
		if err != nil || len(first) != 16*1024 {
			t.Fatalf("framed=%t first read: %v", framed, err)
		}
		_, reader, err := conn.NextReader()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, reader); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			_, message, err := ReadH1PooledMessage(conn, 65535)
			if err != nil {
				t.Fatal(err)
			}
			MessagePoolReturn(message)
		}
		if err := <-serverErrors; err != nil {
			t.Fatal(err)
		}
		conn.Close()
		server.Close()
		if strategy.scores.weight(deriveNetworkId("synthetic-network-a"), dialer.dialerKey()) <= 1 {
			t.Fatalf("framed=%t payload was never observed", framed)
		}
		if strategy.scores.weight(deriveNetworkId("synthetic-network-b"), dialer.dialerKey()) != 1 {
			t.Fatalf("framed=%t credited the new network", framed)
		}
	}
}

// An explicit stalled read is a failure; a canceled race loser and an old
// configuration completion are neutral, with all transitions synchronous.
func TestStrategyReadVerdictsRespectCancellationAndConfig(t *testing.T) {
	strategy, dialer := newDeliveryTestStrategy(t, nil)
	strategy.scores.retainFailedWinner = func() bool { return false }
	ctx, cancel := context.WithCancel(t.Context())
	canceledInfo := strategy.dialerInfo(dialer)
	cancel()
	canceledInfo.observeRead(ctx, 16*1024, context.Canceled, false)
	strategy.dialerInfo(dialer).observeRead(ctx, int(deliveryVerifiedByteCount), context.Canceled, false)
	if strategy.scores.weight("", dialer.dialerKey()) != 1 {
		t.Fatal("canceled loser penalized route")
	}
	oldInfo := strategy.dialerInfo(dialer)
	strategy.scores.setConfigHash("synthetic-new-config")
	strategy.RecordDeliveryOutcome(oldInfo, deliveryVerifiedByteCount, false)
	if strategy.scores.weight("", dialer.dialerKey()) != 1 {
		t.Fatal("old configuration repopulated new scores")
	}
	stalledInfo := strategy.dialerInfo(dialer)
	stalledInfo.observeRead(t.Context(), 16*1024, context.DeadlineExceeded, false)
	if strategy.scores.weight("", dialer.dialerKey()) >= 1 {
		t.Fatal("deadline stall was never recorded")
	}
}

// Authentication refusal is terminal and neutral on HTTP and both public
// upgrade entry points, even when an HTTP error body exceeds the payload gate.
func TestStrategyAuthenticationRefusalStaysNeutral(t *testing.T) {
	for _, mode := range []string{"http", "websocket", "h1"} {
		var requests atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write(make([]byte, deliveryVerifiedByteCount))
		}))
		t.Cleanup(server.Close)
		strategy, dialer := newDeliveryTestStrategy(t, nil)
		address := "ws" + strings.TrimPrefix(server.URL, "http")
		switch mode {
		case "http":
			request, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
			result, err := strategy.HttpParallel(request)
			if err != nil || result.response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("HTTP refusal: %v", err)
			}
		case "websocket":
			_, _, _, err := strategy.WsDialContextWithDialer(t.Context(), address, nil)
			if err == nil {
				t.Fatal("WebSocket refusal was accepted")
			}
		case "h1":
			_, _, err := strategy.H1DialContextWithDialer(t.Context(), address, nil, 65535, true, nil)
			if err == nil {
				t.Fatal("H1 refusal was accepted")
			}
		}
		server.Close()
		if requests.Load() != 1 || strategy.scores.weight("", dialer.dialerKey()) != 1 {
			t.Fatalf("%s refusal retried or changed ranking", mode)
		}
	}
}
