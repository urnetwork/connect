package connect

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Name resolution in a space whose egress is a proxy (connect#227): every name
// goes through the proxy, either over DoH dialed through it or as a name the
// proxy resolves itself, and never through this host's resolver or a DoH dial
// of this host's. Canaries stand where a lookup of this host's would land: a
// DoH server's port, and a resolver whose dial is counted.
//
// Names are .example names; addresses are loopback and RFC 5737 documentation
// addresses.

// A socks5 server on loopback that reads one connect request per connection,
// records its target as the client sent it -- a name stays a name -- and
// refuses it.
type testSocksRecorder struct {
	listener net.Listener
	// "<name or ip>:<port>" of every connect request, with "name " or "ip "
	// ahead of it by its address type
	targets chan string
}

func newTestSocksRecorder(t *testing.T) *testSocksRecorder {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	recorder := &testSocksRecorder{
		listener: listener,
		targets:  make(chan string, 16),
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
		if _, err := io.ReadFull(conn, request); err != nil {
			return
		}
		var target string
		switch request[3] {
		case 1:
			ip := make([]byte, 4)
			if _, err := io.ReadFull(conn, ip); err != nil {
				return
			}
			target = "ip " + netip.AddrFrom4([4]byte(ip)).String()
		case 4:
			ip := make([]byte, 16)
			if _, err := io.ReadFull(conn, ip); err != nil {
				return
			}
			target = "ip " + netip.AddrFrom16([16]byte(ip)).String()
		case 3:
			nameLength := make([]byte, 1)
			if _, err := io.ReadFull(conn, nameLength); err != nil {
				return
			}
			name := make([]byte, nameLength[0])
			if _, err := io.ReadFull(conn, name); err != nil {
				return
			}
			target = "name " + string(name)
		default:
			return
		}
		port := make([]byte, 2)
		if _, err := io.ReadFull(conn, port); err != nil {
			return
		}
		recorder.targets <- fmt.Sprintf("%s:%d", target, binary.BigEndian.Uint16(port))
		// connection refused
		conn.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
	}
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
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
		wg.Wait()
	})
	return recorder
}

// A tcp listener on loopback that counts every connection and closes it.
type testTcpCanary struct {
	listener      net.Listener
	acceptedCount atomic.Int64
}

func newTestTcpCanary(t *testing.T) *testTcpCanary {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	canary := &testTcpCanary{
		listener: listener,
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			canary.acceptedCount.Add(1)
			conn.Close()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		<-done
	})
	return canary
}

// DoH settings whose only server is at `serverAddress`, for both families.
func testCanaryDohSettings(serverAddress string) *DohSettings {
	dohSettings := DefaultDohSettings()
	resolverSettings := *dohSettings.DnsResolverSettings
	dohUrl := fmt.Sprintf("https://%s/dns-query", serverAddress)
	resolverSettings.RemoteDohUrlsIpv4 = []string{dohUrl}
	resolverSettings.RemoteDohUrlsIpv6 = []string{dohUrl}
	resolverSettings.LocalDohUrlsIpv4 = nil
	resolverSettings.LocalDohUrlsIpv6 = nil
	dohSettings.DnsResolverSettings = &resolverSettings
	return dohSettings
}

// A resolver whose every dial is counted and refused: a lookup of this host's.
func newTestCanaryResolver(dialCount *atomic.Int64) *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network string, address string) (net.Conn, error) {
			dialCount.Add(1)
			return nil, errors.New("the canary resolver answers nothing")
		},
	}
}

// A proxied strategy resolves none of the platform's names itself: they reach
// the proxy unresolved, and no DoH dial leaves this host. Before, the internal
// DoH cache looked every protected name up from this host -- its DoH dials,
// and its plain dns fallback, were this host's -- and only then sent the
// address through the proxy.
func TestProxiedStrategyPassesNamesUnresolvedToTheProxy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	socks := newTestSocksRecorder(t)
	dohCanary := newTestTcpCanary(t)
	settings := DefaultClientStrategySettings()
	settings.ProxySettings = &ProxySettings{
		Network: "tcp",
		Address: socks.listener.Addr().String(),
	}
	settings.InternalDohDomains = []string{"space.example"}
	settings.DohSettings = testCanaryDohSettings(dohCanary.listener.Addr().String())
	clientStrategy := NewClientStrategy(ctx, settings)
	defer clientStrategy.Close()
	if clientStrategy.internalDohResolver != nil {
		t.Fatal("a proxied strategy resolves the platform's names itself")
	}

	if conn, err := clientStrategy.ConnectSettings().DialContext(ctx, "tcp", "api.space.example:443"); err == nil {
		conn.Close()
		t.Fatal("the recording proxy accepted a connect")
	}
	select {
	case target := <-socks.targets:
		if target != "name api.space.example:443" {
			t.Fatalf("the proxy was asked for %q, expected the name unresolved", target)
		}
	case <-ctx.Done():
		t.Fatal("the dial never reached the proxy")
	}
	if count := dohCanary.acceptedCount.Load(); count != 0 {
		t.Fatalf("this host dialed the DoH server %d times", count)
	}
}

// The extender bootstrap of a proxied space resolves through the proxy alone:
// its DoH dials go through the space's dial, and a name DoH does not answer
// does not fall back to this host's resolver. Before, the DoH dials took the
// DoH settings' own dial, from this host, and the system resolver answered
// whatever DoH did not.
func TestProxiedExtenderBootstrapResolvesOnlyThroughTheProxy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dohCanary := newTestTcpCanary(t)
	resolverDialCount := &atomic.Int64{}
	spaceDialsLock := sync.Mutex{}
	spaceDials := []string{}
	settings := DefaultClientStrategySettings()
	settings.ProxySettings = testProxySettings()
	// the space's dial, which records and refuses: the proxy as this test
	// sees it
	settings.DialContextSettings = &DialContextSettings{
		DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			spaceDialsLock.Lock()
			defer spaceDialsLock.Unlock()
			spaceDials = append(spaceDials, address)
			return nil, errors.New("the recording dial refuses")
		},
	}
	settings.Resolver = newTestCanaryResolver(resolverDialCount)
	settings.DohSettings = testCanaryDohSettings(dohCanary.listener.Addr().String())
	clientStrategy := NewClientStrategy(ctx, settings)
	defer clientStrategy.Close()

	networkClientSettings := DefaultExtenderNetworkClientSettings()
	networkClientSettings.IpVersionSupported = func(ipVersion int) bool {
		return true
	}
	networkClient := &ExtenderNetworkClient{
		ctx:            ctx,
		log:            NewNoopLogger(),
		clientStrategy: clientStrategy,
		settings:       networkClientSettings,
	}

	if txts, err := networkClient.resolveDnsTxt(ctx, "extender.space.example"); err == nil || 0 < len(txts) {
		t.Fatalf("the bootstrap answered %v, %v with every path refused", txts, err)
	}
	if ips, err := networkClient.resolveDnsProgress(ctx, "extender.space.example", nil); err == nil || 0 < len(ips) {
		t.Fatalf("the bootstrap answered %v, %v with every path refused", ips, err)
	}

	if count := dohCanary.acceptedCount.Load(); count != 0 {
		t.Fatalf("this host dialed the DoH server %d times", count)
	}
	if count := resolverDialCount.Load(); count != 0 {
		t.Fatalf("this host's resolver was asked %d times", count)
	}
	spaceDialsLock.Lock()
	defer spaceDialsLock.Unlock()
	if !slices.Contains(spaceDials, dohCanary.listener.Addr().String()) {
		t.Fatalf("the DoH queries did not go through the space's dial, it saw %v", spaceDials)
	}
}

// A space with no proxy resolves as before: DoH from this host, and the
// system resolver for what DoH does not answer.
func TestUnproxiedExtenderBootstrapFallsBackToTheSystemResolver(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dohCanary := newTestTcpCanary(t)
	resolverDialCount := &atomic.Int64{}
	settings := DefaultClientStrategySettings()
	settings.Resolver = newTestCanaryResolver(resolverDialCount)
	settings.DohSettings = testCanaryDohSettings(dohCanary.listener.Addr().String())
	clientStrategy := NewClientStrategy(ctx, settings)
	defer clientStrategy.Close()
	networkClientSettings := DefaultExtenderNetworkClientSettings()
	networkClientSettings.IpVersionSupported = func(ipVersion int) bool {
		return true
	}
	networkClient := &ExtenderNetworkClient{
		ctx:            ctx,
		log:            NewNoopLogger(),
		clientStrategy: clientStrategy,
		settings:       networkClientSettings,
	}

	networkClient.resolveDnsTxt(ctx, "extender.space.example")
	if count := dohCanary.acceptedCount.Load(); count == 0 {
		t.Fatal("a space with no proxy did not query DoH from this host")
	}
	if count := resolverDialCount.Load(); count == 0 {
		t.Fatal("a space with no proxy did not fall back to the system resolver")
	}
}
