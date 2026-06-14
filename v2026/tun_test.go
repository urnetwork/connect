package connect

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

func TestTunTCPBridge(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	left, err := CreateTunWithDefaults(ctx)
	if err != nil {
		t.Fatalf("create left tun: %v", err)
	}
	defer left.Close()

	right, err := CreateTunWithDefaults(ctx)
	if err != nil {
		t.Fatalf("create right tun: %v", err)
	}
	defer right.Close()

	bridgeTun(ctx, left, right)
	bridgeTun(ctx, right, left)

	rightIP := net.IP(right.localAddresses[0].AsSlice())
	ln, err := right.ListenTCP(&net.TCPAddr{IP: rightIP, Port: 0})
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	defer ln.Close()

	serverErr := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()
		if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
			serverErr <- err
			return
		}
		buf := make([]byte, 4)
		if _, err := io.ReadFull(conn, buf); err != nil {
			serverErr <- err
			return
		}
		if string(buf) != "ping" {
			serverErr <- io.ErrUnexpectedEOF
			return
		}
		_, err = conn.Write([]byte("pong"))
		serverErr <- err
	}()

	conn, err := left.DialContext(ctx, "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial through tun: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write through tun: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read through tun: %v", err)
	}
	if string(buf) != "pong" {
		t.Fatalf("got %q, want pong", string(buf))
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("server error: %v", err)
	}
}

func bridgeTun(ctx context.Context, dst *Tun, src *Tun) {
	go func() {
		for {
			packet, err := src.Read()
			if err != nil {
				return
			}
			_, _ = dst.Write(packet)
			MessagePoolReturn(packet)
			select {
			case <-ctx.Done():
				return
			default:
			}
		}
	}()
}
