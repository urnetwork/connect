package connect

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Drive the production PlatformTransport lifecycle, including a successful
// custom upgrade followed by an old-provider/mismatched-101 fallback on the
// same transport. Counts must follow the registered connection, not opt-in or
// cumulative accepted-upgrade counters.
func TestPlatformTransportH1ConnectionStatsNegotiationAndFallback(t *testing.T) {
	for _, fallback := range []string{"unsupported", "mismatched-101"} {
		t.Run(fallback, func(t *testing.T) {
			resetH1UpgradeTestState(t)
			var rejectCustom atomic.Bool
			connections := make(chan H1MessageConn, 4)
			attempts := make(chan struct{}, 4)
			releaseUpgrade := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var conn H1MessageConn
				var err error
				if r.Header.Get("Upgrade") == H1FramerProtocol {
					attempts <- struct{}{}
					select {
					case <-releaseUpgrade:
					case <-r.Context().Done():
						return
					}
					if rejectCustom.Load() {
						if fallback == "unsupported" {
							http.Error(w, "unsupported", http.StatusUpgradeRequired)
						} else {
							raw, _, hijackErr := w.(http.Hijacker).Hijack()
							if hijackErr == nil {
								fmt.Fprint(raw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: wrong/1\r\n\r\n")
								raw.Close()
							}
						}
						return
					}
					raw, upgradeErr := AcceptFramedUpgrade(w, r, H1FramerProtocol, time.Second)
					if upgradeErr != nil {
						return
					}
					conn, err = NewFramedMessageConn(raw, H1FramerProtocol, 65535, nil)
				} else {
					upgrader := websocket.Upgrader{}
					conn, err = upgrader.Upgrade(w, r, nil)
				}
				if err != nil {
					return
				}
				defer conn.Close()
				connections <- conn
				for {
					if _, _, err := conn.ReadMessage(); err != nil {
						return
					}
				}
			}))
			defer server.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			strategySettings := DefaultClientStrategySettings()
			strategySettings.EnableNormal = true
			strategySettings.EnableResilient = false
			strategy := NewClientStrategy(ctx, strategySettings)
			defer strategy.Close()
			stats := &H1ConnectionStats{}
			settings := testingPlatformTransportSettings()
			settings.EnableH1Plus = true
			settings.H1ConnectionStats = stats
			settings.ReconnectTimeout = time.Millisecond
			transport := NewPlatformTransportWithTargetMode(ctx, strategy, NewRouteManager(ctx, "h1-stats"),
				"ws"+strings.TrimPrefix(server.URL, "http"), &ClientAuth{ByJwt: "testing", InstanceId: NewId()}, TransportModeH1, settings)
			defer transport.Close()
			receivePlatformAuthWitness(t, attempts, "custom upgrade was not attempted")
			if got := stats.Snapshot(); got != (H1ConnectionStatsSnapshot{}) {
				t.Fatalf("pending upgrade was reported active: %+v", got)
			}
			close(releaseUpgrade)
			first := receivePlatformAuthWitness(t, connections, "custom upgrade did not connect")
			if !waitForCondition(5*time.Second, func() bool { return transport.IsConnected() }) {
				t.Fatal("custom connection did not register its routes")
			}
			if got := stats.Snapshot(); got != (H1ConnectionStatsSnapshot{H1PlusConnectionCount: 1}) {
				t.Fatalf("custom connection stats: %+v", got)
			}
			rejectCustom.Store(true)
			first.Close()
			second := receivePlatformAuthWitness(t, connections, "fallback did not connect")
			if _, ok := second.(*websocket.Conn); !ok {
				t.Fatal("fallback did not select a WebSocket")
			}
			if !waitForCondition(5*time.Second, func() bool {
				return stats.Snapshot() == (H1ConnectionStatsSnapshot{WebSocketConnectionCount: 1})
			}) {
				t.Fatalf("fallback left stale H1+ activity: %+v", stats.Snapshot())
			}
			closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer closeCancel()
			if err := transport.CloseAndWait(closeCtx); err != nil {
				t.Fatal(err)
			}
			if got := stats.Snapshot(); got != (H1ConnectionStatsSnapshot{}) {
				t.Fatalf("closed connection remains active: %+v", got)
			}
		})
	}
}

func TestH1ConnectionStatsMixedWindowAndSharedGenerations(t *testing.T) {
	stats := &H1ConnectionStats{}
	framed, err := NewFramedMessageConn(newH1UpgradeScriptConn(nil), H1FramerProtocol, 1200, nil)
	if err != nil {
		t.Fatal(err)
	}
	closePlain := stats.connected(&websocket.Conn{})
	closeFirst := stats.connected(framed)
	closeReplacement := stats.connected(framed)
	if got := stats.Snapshot(); got != (H1ConnectionStatsSnapshot{WebSocketConnectionCount: 1, H1PlusConnectionCount: 2}) {
		t.Fatal(got)
	}
	closeFirst()
	if got := stats.Snapshot(); got != (H1ConnectionStatsSnapshot{WebSocketConnectionCount: 1, H1PlusConnectionCount: 1}) {
		t.Fatal(got)
	}
	closeReplacement()
	closePlain()
	if got := stats.Snapshot(); got != (H1ConnectionStatsSnapshot{}) {
		t.Fatal(got)
	}
}
