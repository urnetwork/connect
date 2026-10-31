//go:build linux || darwin

package extender

import (
	"context"
	"net"
	"syscall"
	"testing"
	"time"
)

// Reads the actual accepted socket options, independently of settings storage.
func extenderAcceptedKeepAliveConfig(t *testing.T, settings *ExtenderSettings) net.KeepAliveConfig {
	t.Helper()
	server := NewExtenderServer(context.Background(), nil, nil, nil, nil, settings)
	defer server.Close()
	listener, err := server.listenTcp("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	dialedConn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer dialedConn.Close()
	acceptedConn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer acceptedConn.Close()
	tcpConn := acceptedConn.(*net.TCPConn)
	if !extenderTcpKeepAliveEnabled(t, tcpConn) {
		return net.KeepAliveConfig{}
	}
	rawConn, err := tcpConn.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var values [3]int
	var getErr error
	if err := rawConn.Control(func(fd uintptr) {
		for i, option := range extenderTcpKeepAliveOptions() {
			values[i], getErr = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_TCP, option)
			if getErr != nil {
				return
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	if getErr != nil {
		t.Fatal(getErr)
	}
	return net.KeepAliveConfig{
		Enable:   true,
		Idle:     time.Duration(values[0]) * time.Second,
		Interval: time.Duration(values[1]) * time.Second,
		Count:    values[2],
	}
}

// Restore the exact former Go listener profile, not only SO_KEEPALIVE.
func TestExtenderTcpListenerKeepsDefaultKeepAliveCadence(t *testing.T) {
	expected := net.KeepAliveConfig{Enable: true, Idle: 15 * time.Second, Interval: 15 * time.Second, Count: 9}
	if got := extenderAcceptedKeepAliveConfig(t, DefaultExtenderSettings()); got != expected {
		t.Fatalf("accepted keepalive = %+v, want former listener cadence %+v", got, expected)
	}
}

// Hosts can configure the cadence or explicitly disable kernel keepalive.
func TestExtenderTcpListenerUsesConfiguredKeepAlive(t *testing.T) {
	for _, config := range []net.KeepAliveConfig{
		{Enable: true, Idle: 7 * time.Second, Interval: 3 * time.Second, Count: 4},
		{},
	} {
		settings := DefaultExtenderSettings()
		settings.TcpKeepAliveConfig = config
		if got := extenderAcceptedKeepAliveConfig(t, settings); got != config {
			t.Errorf("accepted keepalive = %+v, want configured %+v", got, config)
		}
	}
}
