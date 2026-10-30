//go:build unix

package connect

import (
	"context"
	"net"
	"syscall"
	"testing"
	"time"
)

// Every platform dial keeps TCP keepalive at the ConnectSettings cadence,
// including a platform transport's. The transport's own heartbeat notices a
// dead peer only at its read deadline (30 s after the last frame), while
// keepalive notices it about 10 s after the last packet, so the heartbeat is
// not a substitute.
func TestPlatformTransportDialKeepsTcpKeepAlive(t *testing.T) {
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
			dialCtx = (&PlatformTransport{}).dialContext(ctx)
		}
		conn, err := settings.DialContext(dialCtx, "tcp", listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		enabled := tcpKeepAliveEnabled(t, conn.(*net.TCPConn))
		conn.Close()
		if !enabled {
			t.Fatalf("platform transport dial=%t: TCP keepalive is off", transport)
		}
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
