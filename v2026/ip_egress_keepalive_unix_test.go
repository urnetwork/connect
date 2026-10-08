//go:build linux || darwin

package connect

import (
	"net"
	"syscall"
	"testing"
	"time"
)

// Reads the flags and timings actually installed on the provider socket.
func providerEgressTestKeepAlive(t *testing.T, conn net.Conn) net.KeepAliveConfig {
	t.Helper()
	raw, err := conn.(syscall.Conn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	options := providerEgressTestKeepAliveOptions()
	values := [4]int{}
	var optionErr error
	err = raw.Control(func(fd uintptr) {
		for i, option := range options {
			level := syscall.IPPROTO_TCP
			if i == 0 {
				level = syscall.SOL_SOCKET
			}
			values[i], optionErr = syscall.GetsockoptInt(int(fd), level, option)
			if optionErr != nil {
				return
			}
		}
	})
	if err != nil || optionErr != nil {
		t.Fatalf("read keepalive: raw=%v option=%v", err, optionErr)
	}
	return net.KeepAliveConfig{Enable: values[0] != 0, Idle: time.Duration(values[1]) * time.Second, Interval: time.Duration(values[2]) * time.Second, Count: values[3]}
}

// Nil preserves the preexisting 5s cadence, an explicit override sets its own
// timings, and Enable:false remains disabled after ordinary upstream setup.
func TestProviderEgressKeepAliveIsIndependentAndOptional(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, mode := range []EgressTtlMode{EgressTtlModeNative, EgressTtlModeMirror} {
		for _, config := range []*net.KeepAliveConfig{nil,
			{Enable: true, Idle: 61 * time.Second, Interval: 13 * time.Second, Count: 4},
			{Enable: false},
		} {
			settings := DefaultLocalUserNatSettingsWithBufferSize(2)
			settings.TcpBufferSettings.EgressTtlMode = mode
			settings.TcpBufferSettings.ProviderKeepAliveConfig = config
			original := settings.TcpBufferSettings.ConnectSettings.KeepAliveConfig
			f := newProviderEgressTestFlow(t, 4, IpProtocolTcp, settings, 37, false)
			got := providerEgressTestKeepAlive(t, f.socket)
			want := original
			if config != nil {
				want = *config
			}
			if got.Enable != want.Enable || want.Enable && got != want {
				t.Fatalf("mode=%s override=%+v got=%+v want=%+v", mode, config, got, want)
			}
			if settings.TcpBufferSettings.ConnectSettings.KeepAliveConfig != original || DefaultConnectSettings().KeepAliveConfig != original {
				t.Fatal("provider override changed control-plane keepalive")
			}
			if stats := f.nat.EgressRealismStats(); stats.KeepAliveApplyErrorCount != 0 {
				t.Fatalf("keepalive failed: %+v", stats)
			}
		}
	}
}

// A host can establish its own cadence. Nil must not replace it after connect.
func TestProviderEgressNilKeepAlivePreservesHostDialSettings(t *testing.T) {
	settings := DefaultLocalUserNatSettingsWithBufferSize(2)
	want := net.KeepAliveConfig{Enable: true, Idle: 73 * time.Second, Interval: 17 * time.Second, Count: 6}
	host := &net.Dialer{KeepAliveConfig: want}
	settings.TcpBufferSettings.DialContextSettings = &DialContextSettings{DialContext: host.DialContext}
	f := newProviderEgressTestFlow(t, 4, IpProtocolTcp, settings, 37, false)
	if got := providerEgressTestKeepAlive(t, f.socket); got != want {
		t.Fatalf("nil override changed host cadence: got=%+v want=%+v", got, want)
	}
}
