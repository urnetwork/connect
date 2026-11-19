package connect

// A proxy slot's space on the integrated tree: the pinned family transports
// take the egress strategy, which keeps the space's proxy (EXTENDER.md J4), so
// each one marks its handshake (transport_egress_proxy.go). The direct
// strategy of the same settings, which an extender's activation dials,
// reaches the platform from this host and marks nothing.

import (
	"context"
	"testing"
	"time"
)

func TestProxySlotPinnedTransportMarksItsHandshake(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	relay := newTestSocksRelay(t)
	clientSettings := DefaultClientStrategySettings()
	clientSettings.ConnectSettings.ProxySettings = &ProxySettings{
		Network: "tcp",
		Address: relay.listener.Addr().String(),
	}
	// one platform per strategy: a dial races several upgrades, so a racer of
	// one transport must not be read as the other's connection
	connect := func(strategy *ClientStrategy) testingFamilyConnection {
		t.Helper()
		t.Cleanup(strategy.Close)
		platform := newTestingFamilyPlatformServer(t, 4, false)
		settings := testingFamilyTransportSettings()
		settings.IpFamily = 4
		transport := NewPlatformTransportWithTargetMode(ctx, strategy, NewRouteManager(ctx, "proxy slot"), platform.url(), testingFamilyAuth(), TransportModeH1, settings)
		t.Cleanup(transport.Close)
		return receiveFamilyConnection(t, platform, 15*time.Second)
	}

	if connection := connect(NewEgressClientStrategy(ctx, clientSettings, 4)); connection.egressProxy != EgressProxyDeclared {
		t.Fatalf("a proxy slot's pinned transport sent %s = %q, want %q", HeaderEgressProxy, connection.egressProxy, EgressProxyDeclared)
	}
	if relay.relayed.Load() == 0 {
		t.Fatal("the pinned transport did not reach the platform through the space's proxy")
	}
	if connection := connect(NewDirectClientStrategy(ctx, clientSettings, 4)); connection.egressProxy != "" {
		t.Fatalf("a direct transport sent %s = %q", HeaderEgressProxy, connection.egressProxy)
	}
}
