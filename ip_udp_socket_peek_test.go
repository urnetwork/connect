//go:build unix || windows

package connect

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

func newUdpSocketPeekTestSequence(t *testing.T) (*UdpSequence, *net.UDPConn, *net.UDPConn) {
	t.Helper()
	settings := DefaultUdpBufferSettingsWithBufferSize(1)
	settings.Log = NewNoopLogger()
	sequence := NewUdpSequence(context.Background(), nil,
		SourceId(NewId()), protocol.ProvideMode_Network, 4,
		net.IPv4(192, 0, 2, 1).To4(), 42000,
		net.IPv4(127, 0, 0, 1).To4(), 44000, settings)
	if sequence == nil {
		t.Fatal("could not create UDP flow")
	}
	t.Cleanup(sequence.Close)

	peer, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	socket, err := net.DialUDP("udp4", nil, peer.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = socket.Close() })
	return sequence, socket, peer
}

func TestUdpSocketPeekPreservesDatagramBeforeAdmission(t *testing.T) {
	for _, payload := range [][]byte{nil, []byte("short"), []byte("larger-than-peek-buffer")} {
		t.Run(string(payload), func(t *testing.T) {
			sequence, socket, peer := newUdpSocketPeekTestSequence(t)
			readBytes := -1
			sequence.prepareReturnReadCallback = func(_ *UdpSequence, n int, _ func()) (udpReturnReadConsumer, bool) {
				readBytes = n
				return nil, true
			}
			if err := socket.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := peer.WriteToUDP(payload, socket.LocalAddr().(*net.UDPAddr)); err != nil {
				t.Fatal(err)
			}
			peek := make([]byte, 8)
			if _, err := sequence.awaitReturnRead(socket, peek); err != nil {
				t.Fatal(err)
			}
			if want := min(len(payload), len(peek)); readBytes != want {
				t.Fatalf("admission bytes = %d, want %d", readBytes, want)
			}
			if !bytes.Equal(peek[:readBytes], payload[:readBytes]) {
				t.Fatalf("peek = %q, want payload prefix", peek[:readBytes])
			}
			buffer := make([]byte, 64)
			n, err := socket.Read(buffer)
			if err != nil || !bytes.Equal(buffer[:n], payload) {
				t.Fatalf("datagram after peek = %q, err = %v, want %q", buffer[:n], err, payload)
			}
		})
	}
}

func TestUdpSocketPeekIdleDeadlineBeforeAdmission(t *testing.T) {
	sequence, socket, _ := newUdpSocketPeekTestSequence(t)
	sequence.prepareReturnReadCallback = func(*UdpSequence, int, func()) (udpReturnReadConsumer, bool) {
		t.Error("idle socket reached admission before data was ready")
		return nil, true
	}
	if err := socket.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := sequence.awaitReturnRead(socket, make([]byte, 8)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("idle peek error = %v, want read deadline", err)
	}
}
