package connect

// A platform transport dialed through a proxy marks its handshake, so the
// platform counts its client at no address of its own
// (transport_egress_proxy.go). One dialed from this host marks nothing.
// Addresses are loopback.

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A socks5 server on loopback that relays each connect request to its target,
// a name resolved here, and counts the connections it relayed.
type testSocksRelay struct {
	listener net.Listener
	relayed  atomic.Int64
}

func newTestSocksRelay(t *testing.T) *testSocksRelay {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	relay := &testSocksRelay{listener: listener}
	wg := sync.WaitGroup{}
	connsLock := sync.Mutex{}
	conns := map[net.Conn]bool{}
	track := func(conn net.Conn) bool {
		connsLock.Lock()
		defer connsLock.Unlock()
		if conns == nil {
			conn.Close()
			return false
		}
		conns[conn] = true
		return true
	}
	serve := func(conn net.Conn) {
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(10 * time.Second))
		greeting := make([]byte, 2)
		if _, err := io.ReadFull(conn, greeting); err != nil {
			return
		}
		methods := make([]byte, greeting[1])
		if _, err := io.ReadFull(conn, methods); err != nil {
			return
		}
		// no authentication
		if _, err := conn.Write([]byte{5, 0}); err != nil {
			return
		}
		request := make([]byte, 4)
		if _, err := io.ReadFull(conn, request); err != nil || request[1] != 1 {
			return
		}
		var host string
		switch request[3] {
		case 1:
			ip := make([]byte, 4)
			if _, err := io.ReadFull(conn, ip); err != nil {
				return
			}
			host = netip.AddrFrom4([4]byte(ip)).String()
		case 4:
			ip := make([]byte, 16)
			if _, err := io.ReadFull(conn, ip); err != nil {
				return
			}
			host = netip.AddrFrom16([16]byte(ip)).String()
		case 3:
			nameLength := make([]byte, 1)
			if _, err := io.ReadFull(conn, nameLength); err != nil {
				return
			}
			name := make([]byte, nameLength[0])
			if _, err := io.ReadFull(conn, name); err != nil {
				return
			}
			host = string(name)
		default:
			return
		}
		port := make([]byte, 2)
		if _, err := io.ReadFull(conn, port); err != nil {
			return
		}
		target, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port)))), 5*time.Second)
		if err != nil {
			// connection refused
			conn.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
			return
		}
		defer target.Close()
		if !track(target) {
			return
		}
		if _, err := conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
			return
		}
		conn.SetDeadline(time.Time{})
		relay.relayed.Add(1)
		done := make(chan struct{}, 2)
		go func() {
			io.Copy(target, conn)
			done <- struct{}{}
		}()
		go func() {
			io.Copy(conn, target)
			done <- struct{}{}
		}()
		<-done
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			if !track(conn) {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				serve(conn)
			}()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		connsLock.Lock()
		for conn := range conns {
			conn.Close()
		}
		conns = nil
		connsLock.Unlock()
		wg.Wait()
	})
	return relay
}

// A transport whose strategy dials through a proxy marks its handshake, in
// either auth generation: the platform reads HeaderEgressProxy on the
// connection the proxy relayed.
func TestPlatformTransportThroughAProxyMarksItsHandshake(t *testing.T) {
	for _, v2H1Auth := range []bool{true, false} {
		t.Run("v2h1auth="+strconv.FormatBool(v2H1Auth), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// a v1 transport waits for its auth frame's echo
			platform := newTestingFamilyPlatformServer(t, 4, !v2H1Auth)
			relay := newTestSocksRelay(t)
			strategySettings := DefaultClientStrategySettings()
			strategySettings.ConnectSettings.ProxySettings = &ProxySettings{
				Network: "tcp",
				Address: relay.listener.Addr().String(),
			}
			strategy := NewClientStrategy(ctx, strategySettings)
			t.Cleanup(strategy.Close)
			settings := testingFamilyTransportSettings()
			settings.V2H1Auth = v2H1Auth
			transport := NewPlatformTransportWithTargetMode(
				ctx,
				strategy,
				NewRouteManager(ctx, "proxied"),
				platform.url(),
				testingFamilyAuth(),
				TransportModeH1,
				settings,
			)
			t.Cleanup(transport.Close)

			connection := receiveFamilyConnection(t, platform, 15*time.Second)
			if relay.relayed.Load() == 0 {
				t.Fatal("the platform connection did not come through the proxy")
			}
			if connection.egressProxy != EgressProxyDeclared {
				t.Fatalf("a transport dialed through a proxy sent %s = %q, want %q", HeaderEgressProxy, connection.egressProxy, EgressProxyDeclared)
			}
		})
	}
}

// A transport dialed from this host marks nothing, in either auth
// generation.
func TestPlatformTransportFromThisHostMarksNothing(t *testing.T) {
	for _, v2H1Auth := range []bool{true, false} {
		t.Run("v2h1auth="+strconv.FormatBool(v2H1Auth), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			platform := newTestingFamilyPlatformServer(t, 4, !v2H1Auth)
			settings := testingFamilyTransportSettings()
			settings.V2H1Auth = v2H1Auth
			testingPlatformTransport(t, ctx, platform.url(), settings)

			connection := receiveFamilyConnection(t, platform, 15*time.Second)
			if connection.egressProxy != "" {
				t.Fatalf("a transport dialed from this host sent %s = %q", HeaderEgressProxy, connection.egressProxy)
			}
		})
	}
}

// The mark follows the strategy's proxy alone.
func TestApplyEgressProxyHeaderFollowsTheStrategyProxy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	proxied := DefaultClientStrategySettings()
	proxied.ConnectSettings.ProxySettings = &ProxySettings{Network: "tcp", Address: "127.0.0.1:1080"}
	proxiedStrategy := NewClientStrategy(ctx, proxied)
	defer proxiedStrategy.Close()
	directStrategy := NewClientStrategy(ctx, DefaultClientStrategySettings())
	defer directStrategy.Close()

	for _, c := range []struct {
		name     string
		strategy *ClientStrategy
		want     string
	}{
		{"proxied", proxiedStrategy, EgressProxyDeclared},
		{"direct", directStrategy, ""},
		{"no strategy", nil, ""},
	} {
		header := http.Header{}
		applyEgressProxyHeader(header, c.strategy)
		if got := header.Get(HeaderEgressProxy); got != c.want {
			t.Errorf("%s: %s = %q, want %q", c.name, HeaderEgressProxy, got, c.want)
		}
	}
}
