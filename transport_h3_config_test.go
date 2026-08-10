// This file pins H3's path-MTU discovery and conservative startup packet.
package connect

import "testing"

// TestPlatformQuicConfigEnablesPathMtuDiscovery prevents a regression to a
// fixed packet-size ceiling after the underlay or selected endpoint changes.
func TestPlatformQuicConfigEnablesPathMtuDiscovery(t *testing.T) {
	config := newPlatformQuicConfig(DefaultPlatformTransportSettings(), 1)
	if config.DisablePathMTUDiscovery {
		t.Fatal("H3 path MTU discovery is disabled")
	}
	if config.InitialPacketSize != 1400 {
		t.Fatalf("initial packet size=%d want=1400", config.InitialPacketSize)
	}
}
