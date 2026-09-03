package connect

// This file verifies the client-strategy network injection seams without
// depending on the performance harness that consumes them.

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A direct TLS dial must use ConnectSettings.DialContext when one is supplied,
// even when no proxy is configured. The old direct branch bypassed this seam.
func TestNormalTlsDialUsesInjectedDialContext(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	serverTransport, ok := server.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("unexpected test server transport type %T", server.Client().Transport)
	}
	settings := DefaultClientStrategySettings()
	settings.TlsConfig = serverTransport.TLSClientConfig.Clone()
	var dialCount atomic.Int32
	settings.DialContextSettings = &DialContextSettings{
		DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			dialCount.Add(1)
			return (&net.Dialer{}).DialContext(ctx, network, address)
		},
	}
	dialer := &clientDialer{
		dialTlsContext:     newNormalDialTlsContext(settings, clientWebSocketNextProtos),
		httpDialTlsContext: newNormalDialTlsContext(settings, clientHttpNextProtos),
		settings:           settings,
	}
	client := dialer.HttpClient()
	defer client.CloseIdleConnections()

	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := io.Copy(io.Discard, response.Body)
	closeErr := response.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if got := dialCount.Load(); got != 1 {
		t.Fatalf("injected dial context called %d times, expected one", got)
	}
}

// Explicit extender endpoints are copied into persistent dialers. Discovery
// collapse and custom-discovery replacement must not remove or caller-mutate
// these architectural fixtures.
func TestClientStrategyExactExtenderConfigsAreCopiedAndPersistent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	extenderConfig := &ExtenderConfig{
		Profile: ExtenderProfile{
			ConnectMode: ExtenderConnectModeTcpTls,
			ServerName:  "extender.test",
			Port:        8443,
		},
		Ip:     netip.MustParseAddr("192.0.2.10"),
		Secret: "original-secret",
	}
	settings := DefaultClientStrategySettings()
	settings.EnableNormal = false
	settings.EnableResilient = false
	settings.ExposeServerIps = false
	settings.ExposeServerHostNames = false
	settings.ExtenderDropTimeout = 0
	settings.ExtenderConfigs = []*ExtenderConfig{nil, extenderConfig}
	strategy := NewClientStrategy(ctx, settings)

	strategy.mutex.Lock()
	if len(strategy.dialers) != 1 {
		strategy.mutex.Unlock()
		t.Fatalf("configured strategy has %d dialers, expected one", len(strategy.dialers))
	}
	var configuredDialer *clientDialer
	for dialer := range strategy.dialers {
		configuredDialer = dialer
	}
	strategy.mutex.Unlock()
	if !configuredDialer.persistent {
		t.Fatal("configured extender dialer is not persistent")
	}
	if configuredDialer.extenderConfig == extenderConfig {
		t.Fatal("configured extender retained the caller-owned pointer")
	}
	if configuredDialer.extenderConfig.Secret != "original-secret" {
		t.Fatalf("configured secret = %q, expected original value", configuredDialer.extenderConfig.Secret)
	}

	extenderConfig.Secret = "caller-mutated-secret"
	if configuredDialer.extenderConfig.Secret != "original-secret" {
		t.Fatal("caller mutation changed the installed extender config")
	}
	strategy.collapseExtenderDialers()
	strategy.SetCustomExtenders(map[netip.Addr]string{
		netip.MustParseAddr("192.0.2.20"): "discovered-secret",
	})
	strategy.mutex.Lock()
	_, retained := strategy.dialers[configuredDialer]
	strategy.mutex.Unlock()
	if !retained {
		t.Fatal("discovery maintenance removed the configured extender")
	}
}

// Defaults leave both new strategy seams disabled, preserving host TLS dialing
// and dynamic extender discovery.
func TestClientStrategySeamDefaultsAreDisabled(t *testing.T) {
	settings := DefaultClientStrategySettings()
	if settings.DialContextSettings != nil {
		t.Fatal("default strategy unexpectedly injects a dial context")
	}
	if settings.ExtenderConfigs != nil {
		t.Fatal("default strategy unexpectedly installs exact extenders")
	}
	if settings.TlsConfig == nil || settings.TlsConfig.MinVersion < tls.VersionTLS12 {
		t.Fatal("default strategy lost its production TLS configuration")
	}
}

// The MOBILE configuration -- no proxy, no injected dial context -- is the one
// that took the old fast path and bypassed ConnectSettings.DialContext
// entirely. That bypass is why DisableIpv4/DisableIpv6 were dead: the flags
// were honored by the fragment and reorder dialers and ignored by the default
// one, so the strategy raced a forced dialer against an unforced one.
//
// The hook is on ConnectSettings.DialContext -- the seam the family policy
// lives on -- and records the network string AFTER controlDialNetwork has
// resolved it. That placement is what makes this test FAIL on unfixed code:
// with the fast path still present the mobile shape returns a raw tls.Dialer
// and never calls DialContext at all, so the hook never fires and the test
// fails on "the seam is still bypassed".
//
// A hook on the net.Dialer's Control callback could not do this. Control is
// documented to receive an already-family-specific network ("tcp4"/"tcp6"),
// never "tcp", so against an IPv4 literal it records "tcp4" whether or not the
// bypass was ever closed -- a guard that passes on the unfixed code it exists
// to catch.
//
// The target is a NAME, not an address literal. controlDialNetwork
// deliberately leaves a literal's network string alone -- the address already
// fixes the family, so narrowing there can only break a working dial -- and
// the seam this test exists to guard is the one that steers a RESOLUTION.
func TestNormalTlsDialHonorsFamilyPolicyWithNoInjectedDialContext(t *testing.T) {
	SetControlIpFamilyPolicy(IpFamilyForce4)
	defer SetControlIpFamilyPolicy(IpFamilyAuto)

	settings := DefaultClientStrategySettings()
	if settings.ProxySettings != nil || settings.DialContextSettings != nil {
		t.Fatal("the default settings are no longer the mobile shape this test pins")
	}

	var mutex sync.Mutex
	var networks []string
	settings.DialNetworkHook = func(network string, addr string) {
		mutex.Lock()
		defer mutex.Unlock()
		networks = append(networks, network)
	}

	dialTls := newNormalDialTlsContext(settings, clientHttpNextProtos)
	// the host is never reached: .invalid is reserved by RFC 2606 and never
	// resolves. What is under test is the NETWORK STRING the seam resolved,
	// which is recorded before the dial is attempted.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := dialTls(ctx, "tcp", "family-seam.invalid:443")
	if err == nil {
		conn.Close()
	}

	mutex.Lock()
	defer mutex.Unlock()
	if len(networks) == 0 {
		t.Fatal("ConnectSettings.DialContext was never called -- the seam is still bypassed")
	}
	if len(networks) != 1 || networks[0] != "tcp4" {
		t.Fatalf("resolved %v, want exactly [tcp4] under IpFamilyForce4", networks)
	}
}

// A pooled connection that connected cleanly and later went dark is invisible
// to any dial-time logic: http/2 multiplexes every later request onto it, and
// with no health check each one hangs to the request timeout. Go's default for
// HTTP2Config.SendPingTimeout is zero, which its own doc defines as "no health
// check is performed".
//
// This also pins that the config is built on EVERY platform. It used to be
// built only under the mobile memory guard, so desktop had no HTTP2Config at
// all and therefore no health check either.
func TestHttpClientConfiguresHttp2HealthCheck(t *testing.T) {
	settings := DefaultClientStrategySettings()
	dialer := &clientDialer{
		dialTlsContext:     newNormalDialTlsContext(settings, clientWebSocketNextProtos),
		httpDialTlsContext: newNormalDialTlsContext(settings, clientHttpNextProtos),
		settings:           settings,
	}
	client := dialer.HttpClient()
	defer client.CloseIdleConnections()

	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("unexpected transport type %T", client.Transport)
	}
	if transport.HTTP2 == nil {
		t.Fatal("no HTTP2Config: a pooled dead connection is never detected")
	}
	// asserted against the SETTINGS, not against bare literals: the durations
	// are tunable fields, so a test that pinned 10s/5s directly would fail an
	// embedder that legitimately tuned them, and would stop testing that the
	// transport is wired to the settings at all
	if settings.Http2SendPingTimeout <= 0 {
		t.Fatal("the default Http2SendPingTimeout is zero, which disables the health check")
	}
	if settings.Http2PingTimeout <= 0 {
		t.Fatal("the default Http2PingTimeout is zero")
	}
	if transport.HTTP2.SendPingTimeout != settings.Http2SendPingTimeout {
		t.Fatalf("SendPingTimeout is %s, want the settings value %s",
			transport.HTTP2.SendPingTimeout, settings.Http2SendPingTimeout)
	}
	if transport.HTTP2.PingTimeout != settings.Http2PingTimeout {
		t.Fatalf("PingTimeout is %s, want the settings value %s",
			transport.HTTP2.PingTimeout, settings.Http2PingTimeout)
	}
}
