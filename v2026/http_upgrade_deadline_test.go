package connect

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gorilla/websocket"
)

const h1DeadlineAddress = "wss://deadline.example/connect"

type h1DeadlineConn struct {
	net.Conn
	closed atomic.Bool
}

func (c *h1DeadlineConn) Close() error {
	c.closed.Store(true)
	return c.Conn.Close()
}

type h1DeadlineAttempt struct {
	conn    *h1DeadlineConn
	budget  time.Duration
	started time.Time
	upgrade string
}

// The fake TLS dial consumes two RTTs (TCP and TLS), then a net.Pipe peer
// consumes one RTT for the upgrade response. Real TLS/certificate/ALPN
// validation remains covered by TestDialH1MessagesTLSValidationAndHTTP1ALPN.
// No real socket, scheduler tolerance, or wall-clock sleep is involved here.
type h1DeadlinePeer struct {
	t                  *testing.T
	dialDelay          time.Duration
	responseDelay      time.Duration
	customStatus       int
	customBlackhole    bool
	webSocketBlackhole bool
	done               chan struct{}
	wg                 sync.WaitGroup
	mutex              sync.Mutex
	attempts           []*h1DeadlineAttempt
}

func newH1DeadlinePeer(t *testing.T, rtt time.Duration, customStatus int) *h1DeadlinePeer {
	t.Helper()
	p := &h1DeadlinePeer{
		t: t, dialDelay: 2 * rtt, responseDelay: rtt,
		customStatus: customStatus, done: make(chan struct{}),
	}
	t.Cleanup(func() {
		close(p.done)
		for _, attempt := range p.snapshot() {
			if attempt.conn != nil {
				attempt.conn.Close()
			}
		}
		p.wg.Wait()
	})
	return p
}

func (p *h1DeadlinePeer) snapshot() []h1DeadlineAttempt {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	attempts := make([]h1DeadlineAttempt, len(p.attempts))
	for i, attempt := range p.attempts {
		attempts[i] = *attempt
	}
	return attempts
}

func (p *h1DeadlinePeer) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	attempt := &h1DeadlineAttempt{started: time.Now()}
	if deadline, ok := ctx.Deadline(); ok {
		attempt.budget = time.Until(deadline)
	}
	p.mutex.Lock()
	p.attempts = append(p.attempts, attempt)
	p.mutex.Unlock()
	if p.dialDelay != 0 {
		timer := time.NewTimer(p.dialDelay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	client, server := net.Pipe()
	conn := &h1DeadlineConn{Conn: client}
	p.mutex.Lock()
	attempt.conn = conn
	p.mutex.Unlock()
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer server.Close()
		request, err := http.ReadRequest(bufio.NewReader(server))
		if err != nil {
			return // A canceled dial may close before writing its request.
		}
		upgrade := request.Header.Get("Upgrade")
		p.mutex.Lock()
		attempt.upgrade = upgrade
		p.mutex.Unlock()
		if upgrade != H1FramerProtocol && upgrade != H1FramerXlProtocol && upgrade != "websocket" {
			p.t.Errorf("unexpected upgrade %q", upgrade)
			return
		}
		blackhole := p.customBlackhole
		if upgrade == "websocket" {
			blackhole = p.webSocketBlackhole
		}
		if !blackhole {
			if p.responseDelay != 0 {
				timer := time.NewTimer(p.responseDelay)
				defer timer.Stop()
				select {
				case <-p.done:
					return
				case <-timer.C:
				}
			}
			var response string
			if upgrade == "websocket" {
				accept := sha1.Sum([]byte(request.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
				response = fmt.Sprintf("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(accept[:]))
			} else if p.customStatus == http.StatusSwitchingProtocols {
				response = fmt.Sprintf("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: %s\r\n\r\n", upgrade)
			} else {
				response = fmt.Sprintf("HTTP/1.1 %d %s\r\nContent-Length: 0\r\n\r\n", p.customStatus, http.StatusText(p.customStatus))
			}
			if _, err = io.WriteString(server, response); err != nil {
				return
			}
		}
		// The client owns the socket lifetime, including rejected custom
		// attempts. Do not let a cooperative server hide a leaked socket.
		_, _ = io.Copy(io.Discard, server)
	}()
	return conn, nil
}

func (p *h1DeadlinePeer) wsDialer(timeout time.Duration) *websocket.Dialer {
	return &websocket.Dialer{HandshakeTimeout: timeout, NetDialTLSContext: p.dial}
}

func newH1DeadlineStrategy(t *testing.T, dialer *websocket.Dialer, preferred bool) *ClientStrategy {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	settings := DefaultClientStrategySettings()
	route := &clientDialer{
		description: "deadline-test", minimumWeight: 1, settings: settings,
		dialTlsContext: dialer.NetDialTLSContext, websocketDialer: dialer,
	}
	if preferred {
		route.Update(ctx, nil)
	}
	return &ClientStrategy{
		ctx: ctx, log: loggerOrDefault(nil), settings: settings,
		dialers: map[*clientDialer]bool{route: true},
	}
}

func TestH1StrategyHandshakeRTTBudgets(t *testing.T) {
	for _, rtt := range []time.Duration{0, 500 * time.Millisecond, time.Second} {
		for _, preferred := range []bool{false, true} {
			for _, legacy := range []bool{false, true} {
				t.Run(fmt.Sprintf("rtt_%s/preferred_%t/legacy_%t", rtt, preferred, legacy), func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						resetH1UpgradeTestState(t)
						status := http.StatusSwitchingProtocols
						if legacy {
							status = http.StatusUpgradeRequired
						}
						peer := newH1DeadlinePeer(t, rtt, status)
						dialer := peer.wsDialer(5 * time.Second)
						strategy := newH1DeadlineStrategy(t, dialer, preferred)
						stats := &H1PlusStats{}
						start := time.Now()
						conn, _, err := strategy.H1DialContextWithDialer(context.Background(), h1DeadlineAddress, nil, 65535, true, stats)
						if err != nil {
							t.Fatalf("H1 dial after %s: %v; attempts: %+v", time.Since(start), err, peer.snapshot())
						}
						defer conn.Close()
						wantAttempts := 1
						if legacy {
							wantAttempts = 2
						}
						if elapsed := time.Since(start); elapsed != time.Duration(wantAttempts)*3*rtt {
							t.Fatalf("elapsed = %s, want %s", elapsed, time.Duration(wantAttempts)*3*rtt)
						}
						attempts := peer.snapshot()
						if len(attempts) != wantAttempts || attempts[0].budget != 5*time.Second {
							t.Fatalf("native handshake budget/attempts: %+v", attempts)
						}
						_, websocketSelected := conn.(*websocket.Conn)
						if websocketSelected != legacy {
							t.Fatalf("carrier %T, legacy=%t", conn, legacy)
						}
						if legacy {
							if !attempts[0].conn.closed.Load() || attempts[0].conn == attempts[1].conn {
								t.Fatal("fallback did not close and replace custom socket")
							}
							wantWSBudget := 5 * time.Second
							if preferred {
								wantWSBudget = min(wantWSBudget, 7500*time.Millisecond-3*rtt)
							}
							if attempts[1].budget != wantWSBudget {
								t.Fatalf("fresh WS budget = %s, want %s", attempts[1].budget, wantWSBudget)
							}
							if got := stats.Snapshot(); got.Accepted != 0 || got.WebSocketSelected != 1 || got.Attempts != 1 || got.Fallbacks != 1 {
								t.Fatalf("legacy selections = %+v", got)
							}
							// Unsupported capability is cached independently from
							// route health; the next dial uses only a fresh WS.
							cachedStart := time.Now()
							cached, _, err := strategy.H1DialContextWithDialer(context.Background(), h1DeadlineAddress, nil, 65535, true, stats)
							if err != nil {
								t.Fatalf("cached legacy dial: %v", err)
							}
							cached.Close()
							attempts = peer.snapshot()
							if time.Since(cachedStart) != 3*rtt || len(attempts) != 3 || attempts[2].upgrade != "websocket" {
								t.Fatalf("cached legacy dial after %s: %+v", time.Since(cachedStart), attempts)
							}
						} else if got := stats.Snapshot(); got.Accepted != 1 || got.WebSocketSelected != 0 || got.Attempts != 1 {
							t.Fatalf("supported selections = %+v", got)
						}
						if dialer.HandshakeTimeout != 5*time.Second {
							t.Fatal("cached dialer's native timeout was mutated")
						}
						// Strategy cancellation and expired handshake timers must
						// not close the successfully transferred socket.
						time.Sleep(20 * time.Second)
						if err := conn.WriteMessage(websocket.BinaryMessage, []byte("still open")); err != nil {
							t.Fatalf("selected socket retained handshake cancellation/deadline: %v", err)
						}
					})
				})
			}
		}
	}
}

func assertH1DeadlineSocketsClosed(t *testing.T, peer *h1DeadlinePeer) {
	t.Helper()
	synctest.Wait()
	for i, attempt := range peer.snapshot() {
		if attempt.conn != nil && !attempt.conn.closed.Load() {
			t.Errorf("attempt %d (%s) retained its socket", i, attempt.upgrade)
		}
	}
}

func TestH1StrategyLowRTTProbeFallback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		resetH1UpgradeTestState(t)
		peer := newH1DeadlinePeer(t, 0, http.StatusSwitchingProtocols)
		peer.customBlackhole = true
		strategy := newH1DeadlineStrategy(t, peer.wsDialer(5*time.Second), false)
		start := time.Now()
		conn, _, err := strategy.H1DialContextWithDialer(context.Background(), h1DeadlineAddress, nil, 65535, true, nil)
		if err != nil {
			t.Fatal(err)
		}
		conn.Close()
		attempts := peer.snapshot()
		if time.Since(start) != 2500*time.Millisecond || len(attempts) != 2 || attempts[1].started.Sub(start) != 2500*time.Millisecond {
			t.Fatalf("low-RTT fallback after %s: %+v", time.Since(start), attempts)
		}
		if !FramedUpgradePermitted(h1DeadlineAddress, H1FramerProtocol) {
			t.Fatal("probe timeout cached as unsupported capability")
		}
		assertH1DeadlineSocketsClosed(t, peer)
	})
}

func TestH1StrategyLowRTTTerminalRejection(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTemporaryRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				resetH1UpgradeTestState(t)
				peer := newH1DeadlinePeer(t, 0, status)
				strategy := newH1DeadlineStrategy(t, peer.wsDialer(5*time.Second), false)
				start := time.Now()
				conn, _, err := strategy.H1DialContextWithDialer(context.Background(), h1DeadlineAddress, nil, 65535, true, nil)
				var rejected *HTTPUpgradeError
				if conn != nil || !errors.As(err, &rejected) || rejected.StatusCode != status || !rejected.Terminal || time.Since(start) != 0 {
					t.Fatalf("terminal rejection after %s: conn=%T err=%v", time.Since(start), conn, err)
				}
				if len(peer.snapshot()) != 1 || !FramedUpgradePermitted(h1DeadlineAddress, H1FramerProtocol) {
					t.Fatal("terminal rejection caused fallback or poisoned capability")
				}
				assertH1DeadlineSocketsClosed(t, peer)
			})
		})
	}
}

func TestH1StrategyOuterHandshakeBounds(t *testing.T) {
	for _, total := range []time.Duration{15 * time.Second, 4 * time.Second} {
		for _, dialBlackhole := range []bool{false, true} {
			t.Run(fmt.Sprintf("total_%s/dial_blackhole_%t", total, dialBlackhole), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					resetH1UpgradeTestState(t)
					peer := newH1DeadlinePeer(t, 0, http.StatusSwitchingProtocols)
					peer.customBlackhole, peer.webSocketBlackhole = true, true
					if dialBlackhole {
						peer.dialDelay = time.Hour
					}
					strategy := newH1DeadlineStrategy(t, peer.wsDialer(5*time.Second), false)
					ctx, cancel := context.WithTimeout(context.Background(), total)
					defer cancel()
					start := time.Now()
					conn, _, err := strategy.H1DialContextWithDialer(ctx, h1DeadlineAddress, nil, 65535, true, nil)
					if err == nil || conn != nil || time.Since(start) != total {
						t.Fatalf("outer bound after %s: conn=%T err=%v", time.Since(start), conn, err)
					}
					for _, attempt := range peer.snapshot() {
						if attempt.budget <= 0 || attempt.budget > 5*time.Second || attempt.started.Add(attempt.budget).After(start.Add(total)) {
							t.Fatalf("attempt escaped native/outer bounds: %+v", attempt)
						}
					}
					if !FramedUpgradePermitted(h1DeadlineAddress, H1FramerProtocol) {
						t.Fatal("blackhole cached as unsupported capability")
					}
					assertH1DeadlineSocketsClosed(t, peer)
				})
			})
		}
	}
}

func TestH1HandshakeManualCancellation(t *testing.T) {
	for _, strategyCall := range []bool{false, true} {
		for _, stage := range []string{"dial", "custom", "websocket"} {
			t.Run(fmt.Sprintf("strategy_%t/%s", strategyCall, stage), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					resetH1UpgradeTestState(t)
					peer := newH1DeadlinePeer(t, 0, http.StatusUpgradeRequired)
					switch stage {
					case "dial":
						peer.dialDelay = time.Hour
					case "custom":
						peer.customBlackhole = true
					case "websocket":
						peer.webSocketBlackhole = true
					}
					dialer := peer.wsDialer(5 * time.Second)
					strategy := newH1DeadlineStrategy(t, dialer, false)
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					result := make(chan error, 1)
					start := time.Now()
					go func() {
						var conn H1MessageConn
						var err error
						if strategyCall {
							conn, _, err = strategy.H1DialContextWithDialer(ctx, h1DeadlineAddress, nil, 65535, true, nil)
						} else {
							conn, err = DialH1Messages(ctx, h1DeadlineAddress, nil, dialer, H1FramerProtocol, 65535, true, nil)
						}
						if conn != nil {
							conn.Close()
						}
						result <- err
					}()
					synctest.Wait()
					attempts := peer.snapshot()
					wantAttempts := 1
					if stage == "websocket" {
						wantAttempts = 2
					}
					if len(attempts) != wantAttempts {
						t.Fatalf("cancellation did not reach %s: %+v", stage, attempts)
					}
					cancel()
					err := <-result
					if err == nil || time.Since(start) != 0 || (!strategyCall && !errors.Is(err, context.Canceled)) {
						t.Fatalf("cancel after %s: %v", time.Since(start), err)
					}
					if len(peer.snapshot()) != wantAttempts {
						t.Fatal("outer cancellation started another carrier attempt")
					}
					assertH1DeadlineSocketsClosed(t, peer)
				})
			})
		}
	}
}

func TestH1WebSocketProxyHandshakeCancellation(t *testing.T) {
	// The fallback guard must cover the underlying dial, not just Gorilla's
	// GotConn trace hook: CONNECT can block before that hook is invoked.
	synctest.Test(t, func(t *testing.T) {
		resetH1UpgradeTestState(t)
		client, server := net.Pipe()
		defer server.Close()
		conn := &h1DeadlineConn{Conn: client}
		defer conn.Close()
		requestSeen := make(chan struct{})
		serverDone := make(chan struct{})
		go func() {
			defer close(serverDone)
			request, err := http.ReadRequest(bufio.NewReader(server))
			if err != nil {
				t.Errorf("proxy request: %v", err)
				return
			}
			if request.Method != http.MethodConnect {
				t.Errorf("proxy method = %s, want CONNECT", request.Method)
			}
			close(requestSeen)
			_, _ = io.Copy(io.Discard, server)
		}()
		dialer := &websocket.Dialer{
			HandshakeTimeout: 30 * time.Second,
			NetDialContext: func(context.Context, string, string) (net.Conn, error) {
				return conn, nil
			},
			Proxy: func(*http.Request) (*url.URL, error) {
				return url.Parse("http://proxy.example:8080")
			},
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		result := make(chan error, 1)
		start := time.Now()
		go func() {
			_, err := DialH1Messages(ctx, h1DeadlineAddress, nil, dialer, H1FramerXlProtocol, 65535, true, nil)
			result <- err
		}()
		<-requestSeen
		cancel()
		if err := <-result; !errors.Is(err, context.Canceled) || time.Since(start) != 0 {
			t.Fatalf("proxy cancel after %s: %v", time.Since(start), err)
		}
		<-serverDone
		if !conn.closed.Load() {
			t.Fatal("canceled CONNECT retained its socket")
		}
	})
}

func TestDialH1MessagesStandaloneTotalBudget(t *testing.T) {
	// RPC callers use a cancel-only context and a 30s HandshakeTimeout. The
	// custom probe must not turn that into 5s + 30s, nor erase an earlier caller
	// deadline. A caller choosing the default 5s standalone total keeps it.
	for _, test := range []struct {
		name             string
		native, outer    time.Duration
		wantProbe, total time.Duration
	}{
		{"default", 5 * time.Second, 0, 2500 * time.Millisecond, 5 * time.Second},
		{"rpc", 30 * time.Second, 0, 5 * time.Second, 30 * time.Second},
		{"earlier_caller", 30 * time.Second, 4 * time.Second, 2 * time.Second, 4 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				resetH1UpgradeTestState(t)
				peer := newH1DeadlinePeer(t, 0, http.StatusSwitchingProtocols)
				peer.customBlackhole, peer.webSocketBlackhole = true, true
				ctx := context.Background()
				if test.outer != 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, test.outer)
					defer cancel()
				}
				start := time.Now()
				conn, err := DialH1Messages(ctx, h1DeadlineAddress, nil, peer.wsDialer(test.native), H1FramerXlProtocol, 65535, true, nil)
				if err == nil || conn != nil || time.Since(start) != test.total {
					t.Fatalf("standalone total after %s: conn=%T err=%v", time.Since(start), conn, err)
				}
				attempts := peer.snapshot()
				if len(attempts) != 2 || attempts[1].started.Sub(start) != test.wantProbe || attempts[1].budget != test.total-test.wantProbe {
					t.Fatalf("standalone probe/fallback budget: %+v", attempts)
				}
				if !FramedUpgradePermitted(h1DeadlineAddress, H1FramerXlProtocol) {
					t.Fatal("standalone timeout cached as unsupported capability")
				}
				assertH1DeadlineSocketsClosed(t, peer)
			})
		})
	}
}

func TestDialH1MessagesRPCHandshakeRTTBudgets(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy_%t", legacy), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				resetH1UpgradeTestState(t)
				status := http.StatusSwitchingProtocols
				wantDuration := 3 * time.Second
				if legacy {
					status, wantDuration = http.StatusUpgradeRequired, 6*time.Second
				}
				peer := newH1DeadlinePeer(t, time.Second, status)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				start := time.Now()
				conn, err := DialH1Messages(ctx, h1DeadlineAddress, nil, peer.wsDialer(30*time.Second), H1FramerXlProtocol, 65535, true, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				if time.Since(start) != wantDuration {
					t.Fatalf("RPC handshake took %s, want %s", time.Since(start), wantDuration)
				}
				cancel()
				time.Sleep(31 * time.Second)
				if err := conn.WriteMessage(websocket.BinaryMessage, []byte("still open")); err != nil {
					t.Fatalf("RPC socket retained handshake cancellation/deadline: %v", err)
				}
			})
		})
	}
}

func TestDialH1MessagesLegacyCannotExtendStandaloneTotal(t *testing.T) {
	for _, earlierCaller := range []bool{false, true} {
		t.Run(fmt.Sprintf("earlier_caller_%t", earlierCaller), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				resetH1UpgradeTestState(t)
				peer := newH1DeadlinePeer(t, time.Second, http.StatusUpgradeRequired)
				ctx := context.Background()
				native := 5 * time.Second
				if earlierCaller {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, 5*time.Second)
					defer cancel()
					native = 30 * time.Second
				}
				start := time.Now()
				conn, err := DialH1Messages(ctx, h1DeadlineAddress, nil, peer.wsDialer(native), H1FramerXlProtocol, 65535, true, nil)
				// Two healthy 3s handshakes cannot fit in an explicit 5s total.
				// Only the strategy entry point has its own larger outer budget.
				if err == nil || conn != nil || time.Since(start) != 5*time.Second {
					t.Fatalf("legacy dial extended total: elapsed=%s conn=%T err=%v", time.Since(start), conn, err)
				}
				attempts := peer.snapshot()
				if len(attempts) != 2 || attempts[1].started.Sub(start) != 3*time.Second || attempts[1].budget != 2*time.Second {
					t.Fatalf("legacy fallback exceeded remaining total: %+v", attempts)
				}
				assertH1DeadlineSocketsClosed(t, peer)
			})
		})
	}
}
