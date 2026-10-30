//go:build unix

package extender

import (
	"context"
	"net"
	"syscall"
	"testing"
)

// Every connection the tcp carrier accepts is bounded by its own deadlines:
// the header budget (A9), the relay's read deadlines, the splice's idle bound
// and the services' own keepalives. So the listener the extender binds itself
// leaves TCP keepalive off on what it accepts, instead of Go's 15 s default
// probing every idle relay. A Listen seam keeps its own listener as it is.
func TestExtenderTcpListenerLeavesKeepAliveOff(t *testing.T) {
	server := NewExtenderServer(context.Background(), nil, nil, nil, nil, DefaultExtenderSettings())
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
	if extenderTcpKeepAliveEnabled(t, acceptedConn.(*net.TCPConn)) {
		t.Fatal("an accepted carrier connection has TCP keepalive on")
	}

	seamListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer seamListener.Close()
	settings := DefaultExtenderSettings()
	settings.Listen = func(network string, address string) (net.Listener, error) {
		return seamListener, nil
	}
	seamServer := NewExtenderServer(context.Background(), nil, nil, nil, nil, settings)
	defer seamServer.Close()
	if listener, err := seamServer.listenTcp("127.0.0.1:0"); err != nil || listener != seamListener {
		t.Fatalf("the Listen seam was not used: %v", err)
	}
}

// Whether SO_KEEPALIVE is set on the socket.
func extenderTcpKeepAliveEnabled(t *testing.T, tcpConn *net.TCPConn) bool {
	t.Helper()
	rawConn, err := tcpConn.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var enabled int
	var getErr error
	if err := rawConn.Control(func(fd uintptr) {
		enabled, getErr = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_KEEPALIVE)
	}); err != nil {
		t.Fatal(err)
	}
	if getErr != nil {
		t.Fatal(getErr)
	}
	return enabled != 0
}
