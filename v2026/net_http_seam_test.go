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
	"sync/atomic"
	"testing"
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
