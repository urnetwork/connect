//go:build unix

package connect

import (
	"context"
	"net"
	"syscall"
	"testing"
	"time"
)

// A platform transport runs its own heartbeat, so the sockets it dials leave TCP
// keepalive off. Every other platform dial keeps the default keepalive.
func TestPlatformTransportDialLeavesTcpKeepAliveOff(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	settings := DefaultConnectSettings()
	for _, transport := range []bool{false, true} {
		dialCtx := ctx
		if transport {
			dialCtx = withPlatformTransportDial(ctx)
		}
		conn, err := settings.DialContext(dialCtx, "tcp", listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		enabled := tcpKeepAliveEnabled(t, conn.(*net.TCPConn))
		conn.Close()
		if enabled == transport {
			t.Fatalf("platform transport dial=%t: TCP keepalive=%t", transport, enabled)
		}
	}
}

func TestPlatformTransportDialContextIsMarked(t *testing.T) {
	if !isPlatformTransportDial((&PlatformTransport{}).dialContext(context.Background())) {
		t.Fatal("platform transport dial context is not marked")
	}
}

func tcpKeepAliveEnabled(t *testing.T, tcpConn *net.TCPConn) bool {
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
