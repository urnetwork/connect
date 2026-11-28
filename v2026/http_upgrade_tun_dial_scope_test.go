//go:build !js

package connect

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestH1FramedTcpDialMarkerStopsAtWebSocketFallbackAndOrdinaryHttp(t *testing.T) {
	resetH1UpgradeTestState(t)
	observed := &h1UpgradeObservedRequests{}
	upgrade := h1UpgradeTestEchoServer(H1FramerProtocol, observed, http.StatusUpgradeRequired)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ordinary" {
			observed.record(r)
			if r.Method != http.MethodGet || r.TLS == nil || r.ProtoMajor != 1 {
				http.Error(w, "expected HTTPS GET", http.StatusBadRequest)
				return
			}
			io.WriteString(w, "ok")
			return
		}
		upgrade.ServeHTTP(w, r)
	}))
	defer server.Close()
	type rawDial struct{ local, remote, marker string }
	var stateLock sync.Mutex
	var dials []rawDial
	settings := DefaultClientStrategySettings()
	settings.EnableResilient = false
	settings.ConnectSettings.TlsConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	settings.ConnectSettings.DialContextSettings = &DialContextSettings{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		marker, _ := ctx.Value(h1FramedTcpDialKey{}).(string)
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err == nil {
			stateLock.Lock()
			dials = append(dials, rawDial{conn.LocalAddr().String(), address, marker})
			stateLock.Unlock()
		}
		return conn, err
	}}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	strategy := NewClientStrategy(ctx, settings)
	defer strategy.Close()
	address := "wss" + strings.TrimPrefix(server.URL, "https")
	headers := http.Header{"Authorization": {"Bearer synthetic-test"}}
	stats := &H1PlusStats{}
	for range 2 {
		conn, _, err := strategy.H1DialContextWithDialer(ctx, address, headers, 65535, true, stats)
		if err != nil {
			t.Fatal(err)
		}
		h1UpgradeTestEcho(t, conn)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/ordinary", nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := strategy.HttpParallel(req)
	if err != nil || result == nil || result.response.StatusCode != http.StatusOK || string(result.bodyBytes) != "ok" {
		t.Fatalf("ordinary HTTPS request failed: %v", err)
	}
	if got := stats.Snapshot(); got.Attempts != 1 || got.Fallbacks != 1 || got.Accepted != 0 {
		t.Fatalf("unexpected framed upgrade counters: %+v", got)
	}
	observed.Lock()
	defer observed.Unlock()
	stateLock.Lock()
	defer stateLock.Unlock()
	if !slices.Equal(observed.upgrades, []string{H1FramerProtocol, "websocket", "websocket", ""}) || len(dials) != 4 {
		t.Fatalf("unexpected request roles %v or raw dial count %d", observed.upgrades, len(dials))
	}
	for i, want := range []string{server.Listener.Addr().String(), "", "", ""} {
		if dials[i].local != observed.peers[i] || dials[i].remote != server.Listener.Addr().String() || slices.Contains(observed.peers[:i], dials[i].local) {
			t.Fatalf("request %d lacks its distinct matching raw socket: dial=%+v peers=%v", i, dials[i], observed.peers)
		}
		if dials[i].marker != want {
			t.Fatalf("H1_FRAMED_TCP_MARKER_ESCAPED_OR_ABSENT: request %d saw %q, want %q", i, dials[i].marker, want)
		}
	}
	t.Log("H1_FRAMED_TCP_MARKER_SCOPED compact_426=1 websocket=2 ordinary_http=1 raw_dials=4")
}
