//go:build unix

package extender

import (
	"context"
	"net"
	"syscall"
	"testing"
)

// Accepted sockets also serve feeds and other services whose write deadlines
// do not detect a silently disconnected peer while writes fit the socket
// buffer. Keep the listener's pre-optimization kernel liveness backstop.
func TestExtenderTcpListenerKeepsKeepAliveOn(t *testing.T) {
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
	if !extenderTcpKeepAliveEnabled(t, acceptedConn.(*net.TCPConn)) {
		t.Fatal("an accepted carrier connection has no kernel liveness backstop")
	}
}

// A caller-provided listener retains its own socket policy and ownership.
func TestExtenderTcpListenerPreservesInjectedKeepAlivePolicy(t *testing.T) {
	listenConfig := net.ListenConfig{KeepAlive: -1}
	seamListener, err := listenConfig.Listen(context.Background(), "tcp", "127.0.0.1:0")
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
	dialedConn, err := net.Dial("tcp", seamListener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer dialedConn.Close()
	acceptedConn, err := seamListener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer acceptedConn.Close()
	if extenderTcpKeepAliveEnabled(t, acceptedConn.(*net.TCPConn)) {
		t.Fatal("the extender overrode the injected listener's socket policy")
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
