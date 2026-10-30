// This file pins H3's path-MTU discovery, conservative startup packet, and
// public DNS transport port.
package connect

import (
	"testing"
	"time"
)

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
	if config.KeepAlivePeriod != 2*settings.PingTimeout {
		t.Fatalf(
			"H3 keepalive period=%s want=%s",
			config.KeepAlivePeriod,
			2*settings.PingTimeout,
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

// QUIC sends a keepalive PING once the peer has been quiet for its keepalive
// period, which quic-go caps at half the idle timeout, and closes the
// connection one idle timeout after the first ack-eliciting packet the peer
// leaves unanswered. At twice PingTimeout the app's own ping carries an idle
// connection's liveness, one ack-eliciting packet per ping interval instead of
// a QUIC PING between every two app pings, and a dead peer is still detected
// within the keepalive period plus the idle timeout: 30s with the defaults.
func TestPlatformQuicConfigKeepAliveIsTwicePingTimeout(t *testing.T) {
	defaultPingTimeout := DefaultPlatformTransportSettings().PingTimeout
	for _, pingTimeout := range []time.Duration{defaultPingTimeout, 250 * time.Millisecond, 30 * time.Second} {
		settings := DefaultPlatformTransportSettings()
		settings.PingTimeout = pingTimeout
		config := newPlatformQuicConfig(settings, 1)
		if config.KeepAlivePeriod != 2*pingTimeout || config.MaxIdleTimeout != 4*pingTimeout {
			t.Fatalf("ping timeout %s: QUIC keepalive=%s idle timeout=%s, want %s and %s",
				pingTimeout, config.KeepAlivePeriod, config.MaxIdleTimeout, 2*pingTimeout, 4*pingTimeout)
		}
		if config.MaxIdleTimeout/2 < config.KeepAlivePeriod {
			t.Fatalf("ping timeout %s: quic-go caps the %s keepalive at half the %s idle timeout",
				pingTimeout, config.KeepAlivePeriod, config.MaxIdleTimeout)
		}
	}
	config := newPlatformQuicConfig(DefaultPlatformTransportSettings(), 1)
	if detection := config.KeepAlivePeriod + config.MaxIdleTimeout; 30*time.Second < detection {
		t.Fatalf("default dead-peer detection can take %s, want at most 30s", detection)
	}
}

// TestPlatformQuicConfigDefaultsZeroReceiveWindows preserves compatibility
// with callers that construct partial PlatformTransportSettings literals.
func TestPlatformQuicConfigDefaultsZeroReceiveWindows(t *testing.T) {
	settings := DefaultPlatformTransportSettings()
	settings.H3InitialStreamReceiveWindowByteCount = 0
	settings.H3MaxStreamReceiveWindowByteCount = 0
	settings.H3InitialConnectionReceiveWindowByteCount = 0
	settings.H3MaxConnectionReceiveWindowByteCount = 0
	config := newPlatformQuicConfig(settings, 1)

	if config.InitialStreamReceiveWindow != uint64(kib(256)) ||
		config.MaxStreamReceiveWindow != uint64(defaultH3MaxStreamReceiveWindowByteCount()) ||
		config.InitialConnectionReceiveWindow != uint64(kib(512)) ||
		config.MaxConnectionReceiveWindow != uint64(defaultH3MaxConnectionReceiveWindowByteCount()) {
		t.Fatalf(
			"zero-value H3 receive windows resolved to stream=%d/%d connection=%d/%d",
			config.InitialStreamReceiveWindow,
			config.MaxStreamReceiveWindow,
			config.InitialConnectionReceiveWindow,
			config.MaxConnectionReceiveWindow,
		)
	}
}

// TestPlatformQuicConfigUsesDeviceMemoryTargetReceiveWindows proves the
// per-device target reaches quic-go instead of being replaced by process
// defaults at carrier construction.
func TestPlatformQuicConfigUsesDeviceMemoryTargetReceiveWindows(t *testing.T) {
	settings := DefaultPlatformTransportSettingsWithMemoryTarget(24 * 1024 * 1024)
	config := newPlatformQuicConfig(settings, 1)
	if config.InitialStreamReceiveWindow != uint64(settings.H3InitialStreamReceiveWindowByteCount) ||
		config.MaxStreamReceiveWindow != uint64(settings.H3MaxStreamReceiveWindowByteCount) ||
		config.InitialConnectionReceiveWindow != uint64(settings.H3InitialConnectionReceiveWindowByteCount) ||
		config.MaxConnectionReceiveWindow != uint64(settings.H3MaxConnectionReceiveWindowByteCount) {
		t.Fatalf(
			"device-target H3 receive windows = stream=%d/%d connection=%d/%d, want settings %+v",
			config.InitialStreamReceiveWindow,
			config.MaxStreamReceiveWindow,
			config.InitialConnectionReceiveWindow,
			config.MaxConnectionReceiveWindow,
			settings,
		)
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
