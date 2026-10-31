//go:build linux || darwin

package connect

import (
	"testing"
)

// Every default profile keeps kernel keepalive on the provider's upstream TCP
// sockets, at the dial's cadence: a probe after 5 s idle and a reset after one
// unanswered probe. A relayed flow carries arbitrary TCP with no heartbeat of
// ours, so an upstream that vanishes while the flow is idle is noticed only by
// keepalive. Without it the flow lingers until ReadTimeout or IdleTimeout
// (300 s, see ip_tcp_flow_reap_test.go).
func TestProviderEgressKeepAliveKeepsTheDialCadence(t *testing.T) {
	assertMessagePoolOwnership(t)
	dialCadence := DefaultConnectSettings().KeepAliveConfig
	for _, version := range []int{4, 6} {
		for _, settings := range []*LocalUserNatSettings{
			DefaultLocalUserNatSettingsWithBufferSize(2),
			DefaultProviderLocalUserNatSettings(),
			DefaultProviderLocalUserNatSettingsWithMemoryTarget(mib(4)),
		} {
			original := settings.TcpBufferSettings.ConnectSettings.KeepAliveConfig
			f := newProviderEgressTestFlow(t, version, IpProtocolTcp, settings, 37, false)
			if got := providerEgressTestKeepAlive(t, f.socket); got != dialCadence {
				t.Fatalf("ipv%d provider upstream keepalive is %+v, expected the dial cadence %+v", version, got, dialCadence)
			}
			if settings.TcpBufferSettings.ConnectSettings.KeepAliveConfig != original ||
				DefaultConnectSettings().KeepAliveConfig != original {
				t.Fatal("the provider default changed the control-plane keepalive")
			}
			if stats := f.nat.EgressRealismStats(); stats.KeepAliveApplyErrorCount != 0 {
				t.Fatalf("keeping the dial keepalive failed: %+v", stats)
			}
		}
	}
}
