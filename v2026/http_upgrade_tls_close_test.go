package connect

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// A real TLS peer can finish its refusal and close TCP without a TLS alert.
// A clean client close still permits a fresh authenticated WebSocket; a reset
// observed during the client's close remains part of the failed negotiation.
func TestDialH1MessagesTLSRefusalPeerClose(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUpgradeRequired, http.StatusSwitchingProtocols} {
		for _, reset := range []bool{false, true} {
			t.Run(fmt.Sprintf("status_%d/reset_%t", status, reset), func(t *testing.T) {
				resetH1UpgradeTestState(t)
				var probes, webSockets atomic.Int32
				var handlers sync.WaitGroup
				var hijacked sync.Map
				peerClosed := make(chan struct{})
				var peerClosedOnce sync.Once
				upgrader := websocket.Upgrader{}
				server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					handlers.Add(1)
					defer handlers.Done()
					if IsFramedUpgrade(r, H1FramerProtocol) {
						probes.Add(1)
						defer peerClosedOnce.Do(func() { close(peerClosed) })
						conn, buffered, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						// Close the real TCP connection directly, without tls.Close.
						tcp := conn.(*tls.Conn).NetConn().(*net.TCPConn)
						defer tcp.Close()
						if err := conn.SetWriteDeadline(time.Now().Add(3 * time.Second)); err != nil {
							t.Error(err)
							return
						}
						if reset {
							if err := tcp.SetLinger(0); err != nil {
								t.Error(err)
								return
							}
						}
						if status == http.StatusSwitchingProtocols {
							_, err = fmt.Fprint(buffered, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
						} else {
							_, err = fmt.Fprintf(buffered, "HTTP/1.1 %d %s\r\nContent-Length: 0\r\n\r\n", status, http.StatusText(status))
						}
						if err == nil {
							err = buffered.Flush()
						}
						if err != nil {
							t.Error(err)
						}
						return
					}
					webSockets.Add(1)
					conn, err := upgrader.Upgrade(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.Close()
					if err := conn.SetWriteDeadline(time.Now().Add(3 * time.Second)); err != nil {
						t.Error(err)
						return
					}
					if err := conn.WriteMessage(websocket.BinaryMessage, []byte("fresh authenticated fallback")); err != nil {
						t.Error(err)
					}
				}))
				server.Config.ConnState = func(conn net.Conn, state http.ConnState) {
					if state == http.StateHijacked {
						hijacked.Store(conn, struct{}{})
					}
				}
				server.StartTLS()
				defer func() {
					server.Close()
					// httptest stops tracking hijacked sockets. Release those and
					// join their handlers even when an assertion aborts the test.
					hijacked.Range(func(conn, _ any) bool {
						conn.(net.Conn).Close()
						return true
					})
					handlers.Wait()
				}()
				var probe *h1TlsPeerCloseConn
				tlsDialer := &tls.Dialer{Config: server.Client().Transport.(*http.Transport).TLSClientConfig}
				dialer := &websocket.Dialer{HandshakeTimeout: 3 * time.Second, NetDialTLSContext: func(ctx context.Context, network, address string) (net.Conn, error) {
					conn, err := tlsDialer.DialContext(ctx, network, address)
					if err != nil || probe != nil {
						return conn, err
					}
					probe = &h1TlsPeerCloseConn{Conn: conn, peerClosed: peerClosed}
					return probe, nil
				}}
				address := "wss" + strings.TrimPrefix(server.URL, "https")
				conn, err := DialH1Messages(t.Context(), address, nil, dialer, H1FramerProtocol, 1024, true, nil)
				if probe == nil || probe.barrierErr != nil {
					if conn != nil {
						conn.Close()
					}
					t.Fatalf("peer close was not observed: probe=%+v err=%v", probe, err)
				}
				if reset {
					if conn != nil {
						conn.Close()
						t.Fatal("reset cleanup returned a connection")
					}
					if probe.closeErr == nil || !errors.Is(err, probe.closeErr) || HTTPUpgradeAllowsFallback(err) {
						t.Fatalf("lost original TLS close failure or allowed fallback: %v", err)
					}
					var readErr *net.OpError
					var osErr syscall.Errno
					if !errors.As(probe.readErr, &readErr) || readErr.Op != "read" || readErr.Timeout() ||
						!errors.As(probe.readErr, &osErr) || osErr == 0 || errors.Is(probe.readErr, net.ErrClosed) || errors.Is(probe.readErr, io.ErrClosedPipe) ||
						probe.endBytes != 0 || webSockets.Load() != 0 || probes.Load() != 1 {
						t.Fatalf("reset did not stop fresh negotiation: read=%v probes=%d websocket=%d", probe.readErr, probes.Load(), webSockets.Load())
					}
					if !FramedUpgradePermitted(address, H1FramerProtocol) {
						t.Fatal("TLS close failure was cached as unsupported capability")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
					t.Fatal(err)
				}
				_, payload, err := conn.ReadMessage()
				if err != nil || string(payload) != "fresh authenticated fallback" || probes.Load() != 1 || webSockets.Load() != 1 ||
					probe.closeErr != nil || !errors.Is(probe.readErr, io.EOF) || probe.endBytes != 0 {
					t.Fatalf("TLS refusal did not complete a fresh fallback: payload=%q err=%v probes=%d websocket=%d close=%v peer=%v", payload, err, probes.Load(), webSockets.Load(), probe.closeErr, probe.readErr)
				}
			})
		}
	}
}

// Observe the real peer EOF or reset before TLS cleanup, independently of
// loopback packet scheduling, and retain the actual tls.Conn.Close result.
type h1TlsPeerCloseConn struct {
	net.Conn
	peerClosed <-chan struct{}
	closeOnce  sync.Once
	barrierErr error
	endBytes   int
	readErr    error
	closeErr   error
}

func (c *h1TlsPeerCloseConn) Close() error {
	c.closeOnce.Do(func() {
		select {
		case <-c.peerClosed:
		case <-time.After(3 * time.Second):
			c.barrierErr = errors.New("timed out waiting for peer TCP close")
		}
		if c.barrierErr == nil {
			c.barrierErr = c.Conn.SetReadDeadline(time.Now().Add(time.Second))
		}
		if c.barrierErr == nil {
			var b [1]byte
			c.endBytes, c.readErr = c.Conn.Read(b[:])
		}
		c.closeErr = c.Conn.Close()
	})
	return c.closeErr
}
