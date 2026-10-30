//go:build linux || darwin

package connect

import (
	"testing"
)

// Every default profile leaves kernel keepalive off on the provider's
// upstream TCP sockets. The inherited control-plane cadence probed each idle
// flow every 5 s, which keeps a phone's radio up for every flow it carries.
// The flow timeouts reap a dead upstream instead (see ip_tcp_flow_reap_test.go).
func TestProviderEgressKeepAliveDefaultsOff(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{4, 6} {
		for _, settings := range []*LocalUserNatSettings{
			DefaultLocalUserNatSettingsWithBufferSize(2),
			DefaultProviderLocalUserNatSettings(),
			DefaultProviderLocalUserNatSettingsWithMemoryTarget(mib(4)),
		} {
			original := settings.TcpBufferSettings.ConnectSettings.KeepAliveConfig
			f := newProviderEgressTestFlow(t, version, IpProtocolTcp, settings, 37, false)
			if got := providerEgressTestKeepAlive(t, f.socket); got.Enable {
				t.Fatalf("ipv%d provider upstream keepalive is on by default: %+v", version, got)
			}
			if settings.TcpBufferSettings.ConnectSettings.KeepAliveConfig != original ||
				DefaultConnectSettings().KeepAliveConfig != original {
				t.Fatal("the provider default changed the control-plane keepalive")
			}
			if stats := f.nat.EgressRealismStats(); stats.KeepAliveApplyErrorCount != 0 {
				t.Fatalf("turning keepalive off failed: %+v", stats)
			}
		}
	}
}
