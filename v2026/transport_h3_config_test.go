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
	if config.Tracer != nil {
		t.Fatal("H3 packet tracing enabled without an explicit stats collector")
	}
	settings.H3QuicPacketStats = &H3QuicPacketStats{}
	if newPlatformQuicConfig(settings, 1).Tracer == nil {
		t.Fatal("H3 packet stats did not enable the QUIC tracer")
	}
	settings.EnableH3Datagrams = false
	if newPlatformQuicConfig(settings, 1).EnableDatagrams {
		t.Fatal("H3 DATAGRAM rollout setting did not disable QUIC advertisement")
	}
}

// The internal edge/server listener is UDP/8053, but client traffic must still
// reach the public DNS service port and rely on ingress DNAT.
func TestPlatformDnsTransportUsesPublicPort(t *testing.T) {
	settings := DefaultPlatformTransportSettings()
	if settings.DnsPort != 53 {
		t.Fatalf("DNS-encoded QUIC destination port=%d want=53", settings.DnsPort)
	}
}
