package extender

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// The datagram relay, end to end over a real carrier: a client frames
// datagrams on a reliable stream, the extender turns them into udp, and the
// replies come back framed.
//
// This is the capability that lets quic traverse an extender at all, so the
// tests below are about the properties quic depends on: boundaries survive,
// order survives, and nothing silently coalesces two packets into one.

// echoUdp answers every datagram with a transform of it, until ctx ends.
func echoUdp(t *testing.T, ctx context.Context, transform func([]byte) []byte) *net.UDPAddr {
	t.Helper()
	packetConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	go func() {
		<-ctx.Done()
		packetConn.Close()
	}()
	go func() {
		buffer := make([]byte, 4096)
		for {
			n, from, err := packetConn.ReadFrom(buffer)
			if err != nil {
				return
			}
			if _, err := packetConn.WriteTo(transform(buffer[:n]), from); err != nil {
				return
			}
		}
	}()
	return packetConn.LocalAddr().(*net.UDPAddr)
}

// newDatagramRelay runs one relayDatagram over an in-memory carrier and
// returns the client end of it.
func newDatagramRelay(t *testing.T, ctx context.Context, destination *net.UDPAddr) net.Conn {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	server := NewExtenderServerWithDefaults(
		ctx, nil, []string{"127.0.0.1"}, nil, &net.Dialer{},
	)
	relayCtx, relayCancel := context.WithCancel(ctx)
	t.Cleanup(relayCancel)
	go func() {
		defer serverConn.Close()
		server.relayDatagram(relayCtx, relayCancel, serverConn, extenderDatagramHeader(destination))
	}()
	t.Cleanup(func() { clientConn.Close() })
	return clientConn
}

func TestDatagramRelayPreservesBoundariesAndOrder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Echo with a marker, so a reply cannot be confused with the request that
	// produced it and a coalesced pair is visible rather than plausible.
	destination := echoUdp(t, ctx, func(b []byte) []byte {
		return append([]byte("echo:"), b...)
	})
	clientConn := newDatagramRelay(t, ctx, destination)

	// Sizes that would be indistinguishable on a raw stream: three datagrams
	// whose concatenation is one plausible message.
	payloads := []string{"a", "bb", "ccc", strings.Repeat("d", 1200)}
	for _, payload := range payloads {
		if err := connect.WriteExtenderDatagram(clientConn, []byte(payload)); err != nil {
			t.Fatalf("write %q: %v", payload, err)
		}
	}

	buffer := make([]byte, 4096)
	for _, payload := range payloads {
		if err := clientConn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
			t.Fatalf("deadline: %v", err)
		}
		n, err := connect.ReadExtenderDatagram(clientConn, buffer)
		if err != nil {
			t.Fatalf("read reply to %q: %v", payload, err)
		}
		want := "echo:" + payload
		if got := string(buffer[:n]); got != want {
			// A boundary loss shows up here as a reply carrying more than one
			// datagram, which is exactly what quic cannot tolerate.
			t.Fatalf("reply = %q (%d bytes), want %q", got, n, want)
		}
	}
}

// Quic sends MTU-sized packets, so the relay has to carry them whole.
func TestDatagramRelayCarriesAnMtuSizedPacket(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	destination := echoUdp(t, ctx, func(b []byte) []byte { return b })
	clientConn := newDatagramRelay(t, ctx, destination)

	payload := make([]byte, 1400)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	if err := connect.WriteExtenderDatagram(clientConn, payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := clientConn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("deadline: %v", err)
	}
	buffer := make([]byte, 4096)
	n, err := connect.ReadExtenderDatagram(clientConn, buffer)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if n != len(payload) {
		t.Fatalf("echoed %d bytes, want %d", n, len(payload))
	}
	if string(buffer[:n]) != string(payload) {
		t.Fatal("echoed payload differs")
	}
}

// A destination the extender is not allowed to reach must not become reachable
// by asking for datagrams instead of a stream.
func TestDatagramRelayRefusesADisallowedDestination(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server := NewExtenderServerWithDefaults(
		ctx, nil, []string{"allowed.example"}, nil, &net.Dialer{},
	)
	if server.IsAllowedHost("127.0.0.1") {
		t.Fatal("a host outside the whitelist must not be allowed for datagrams either")
	}
	if !server.IsAllowedHost("allowed.example") {
		t.Fatal("the whitelist should still admit its own entry")
	}
}

// The framing is the contract between the two halves, so its edges are pinned
// here rather than left to the relay tests to imply.
func TestExtenderDatagramFramingRoundTrip(t *testing.T) {
	for _, payload := range [][]byte{
		{},
		[]byte("x"),
		[]byte(strings.Repeat("y", 1500)),
	} {
		var carrier netPipeBuffer
		if err := connect.WriteExtenderDatagram(&carrier, payload); err != nil {
			t.Fatalf("write %d bytes: %v", len(payload), err)
		}
		buffer := make([]byte, 4096)
		n, err := connect.ReadExtenderDatagram(&carrier, buffer)
		if err != nil {
			t.Fatalf("read %d bytes: %v", len(payload), err)
		}
		if n != len(payload) || string(buffer[:n]) != string(payload) {
			t.Fatalf("round trip of %d bytes produced %d", len(payload), n)
		}
	}
}

func TestExtenderDatagramFramingRefusesAnOversizedFrame(t *testing.T) {
	var carrier netPipeBuffer
	err := connect.WriteExtenderDatagram(&carrier, make([]byte, 64*1024))
	if err == nil {
		t.Fatal("an oversized datagram must be refused, not truncated")
	}
	if !connect.IsExtenderDatagramTooLarge(err) {
		t.Errorf("error should be recognizable as too-large, got %v", err)
	}
}

// A truncated frame must not be reported as a short datagram: the stream is
// out of sync and the only safe answer is an error.
func TestExtenderDatagramFramingRefusesATruncatedFrame(t *testing.T) {
	var carrier netPipeBuffer
	if err := connect.WriteExtenderDatagram(&carrier, []byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	carrier.buffer = carrier.buffer[:len(carrier.buffer)-2]
	buffer := make([]byte, 64)
	if _, err := connect.ReadExtenderDatagram(&carrier, buffer); err == nil {
		t.Fatal("a truncated frame must be an error")
	} else if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("want unexpected EOF, got %v", err)
	}
}

// extenderDatagramHeader builds the header the relay reads its destination
// from.
func extenderDatagramHeader(destination *net.UDPAddr) *protocol.ExtenderHeader {
	return &protocol.ExtenderHeader{
		DestinationHost: destination.IP.String(),
		DestinationPort: uint32(destination.Port),
		Datagram:        true,
	}
}

// netPipeBuffer is a trivial in-memory stream for the framing tests.
type netPipeBuffer struct {
	buffer []byte
	read   int
}

func (self *netPipeBuffer) Write(p []byte) (int, error) {
	self.buffer = append(self.buffer, p...)
	return len(p), nil
}

func (self *netPipeBuffer) Read(p []byte) (int, error) {
	if self.read >= len(self.buffer) {
		return 0, io.EOF
	}
	n := copy(p, self.buffer[self.read:])
	self.read += n
	return n, nil
}
