// This file pins H3's path-MTU discovery, conservative startup packet, and
// public DNS transport port.
package connect

import "testing"

// TestPlatformQuicConfigEnablesPathMtuDiscovery prevents a regression to a
// fixed packet-size ceiling after the underlay or selected endpoint changes.
// It also pins connection-level keepalive: the application writer can block
// behind the bounded DATAGRAM queue and cannot own hybrid liveness.
func TestPlatformQuicConfigEnablesPathMtuDiscovery(t *testing.T) {
	settings := DefaultPlatformTransportSettings()
	config := newPlatformQuicConfig(settings, 1)
	if config.DisablePathMTUDiscovery {
		t.Fatal("H3 path MTU discovery is disabled")
	}
	if config.InitialPacketSize != H3InitialPacketByteCount {
		t.Fatalf(
			"initial packet size=%d want=%d",
			config.InitialPacketSize,
			H3InitialPacketByteCount,
		)
	}
	const (
		minimumCellularPathMtu = 1280
		ipv4HeaderByteCount    = 20
		udpHeaderByteCount     = 8
	)
	if minimumCellularPathMtu <
		int(config.InitialPacketSize)+ipv4HeaderByteCount+udpHeaderByteCount {
		t.Fatalf(
			"initial QUIC packet fragments at the cellular path floor: UDP payload=%d IPv4 packet=%d path MTU=%d",
			config.InitialPacketSize,
			int(config.InitialPacketSize)+ipv4HeaderByteCount+udpHeaderByteCount,
			minimumCellularPathMtu,
		)
	}
	if !config.EnableDatagrams {
		t.Fatal("H3 DATAGRAM capability is not advertised by default")
	}
	if config.KeepAlivePeriod != settings.PingTimeout {
		t.Fatalf(
			"H3 keepalive period=%s want=%s",
			config.KeepAlivePeriod,
			settings.PingTimeout,
		)
	}
	if config.Tracer == nil {
		t.Fatal("default H3 PTO/no-response tracing is disabled")
	}
	settings.H3QuicPacketStats = nil
	if newPlatformQuicConfig(settings, 1).Tracer != nil {
		t.Fatal("explicitly disabled H3 packet tracing remained enabled")
	}
	settings.EnableH3Datagrams = false
	if newPlatformQuicConfig(settings, 1).EnableDatagrams {
		t.Fatal("H3 DATAGRAM rollout setting did not disable QUIC advertisement")
	}
}

// The private edge/server listener is UDP/4053 (with 8053 retained only during
// migration), but client traffic must still reach public DNS and rely on DNAT.
func TestPlatformDnsTransportUsesPublicPort(t *testing.T) {
	settings := DefaultPlatformTransportSettings()
	if settings.DnsPort != 53 {
		t.Fatalf("DNS-encoded QUIC destination port=%d want=53", settings.DnsPort)
	}
}

func TestPlatformDnsPumpUsesStableWhoDisHost(t *testing.T) {
	settings := DefaultPlatformTransportSettings()
	if settings.DnsPumpHost != DefaultDnsPumpHost {
		t.Fatalf("DNS pump host=%q want %q", settings.DnsPumpHost, DefaultDnsPumpHost)
	}
	if settings.DnsPumpHost != "whodis.bringyour.com" {
		t.Fatalf("default DNS pump host=%q want whodis.bringyour.com", settings.DnsPumpHost)
	}
}
