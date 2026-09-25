package connect

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
)

// The carrier itself owns its buffers. Framing may copy one packet while a
// synchronous Write is in progress, but must not allocate another packet for
// every read or write on a long-lived mobile connection.
type extenderDatagramMemoryConn struct {
	net.Conn
	reader     bytes.Reader
	writeCount int
	shortWrite bool
}

func (self *extenderDatagramMemoryConn) Read(p []byte) (int, error) {
	return self.reader.Read(p)
}

func (self *extenderDatagramMemoryConn) Write(p []byte) (int, error) {
	self.writeCount++
	if self.shortWrite {
		return len(p) - 1, nil
	}
	return len(p), nil
}

func TestExtenderPacketConnFramingDoesNotAllocate(t *testing.T) {
	payload := make([]byte, 1400)
	frame := make([]byte, extenderDatagramLenSize+len(payload))
	binary.BigEndian.PutUint16(frame, uint16(len(payload)))
	copy(frame[extenderDatagramLenSize:], payload)
	carrier := &extenderDatagramMemoryConn{}
	conn := newExtenderPacketConn(carrier, newExtenderDatagramAddr("udp4", "192.0.2.1:443"))
	readBuffer := make([]byte, extenderDatagramMaxSize)
	t.Run("write", func(t *testing.T) {
		allocs := testing.AllocsPerRun(1000, func() {
			if n, err := conn.WriteTo(payload, nil); n != len(payload) || err != nil {
				t.Fatalf("write = %d, %v", n, err)
			}
		})
		if allocs != 0 {
			t.Fatalf("framing allocated %.0f objects per datagram write", allocs)
		}
	})
	t.Run("read", func(t *testing.T) {
		allocs := testing.AllocsPerRun(1000, func() {
			carrier.reader.Reset(frame)
			if n, _, err := conn.ReadFrom(readBuffer); n != len(payload) || err != nil {
				t.Fatalf("read = %d, %v", n, err)
			}
		})
		if allocs != 0 {
			t.Fatalf("framing allocated %.0f objects per datagram read", allocs)
		}
	})
}

func TestExtenderDatagramShortWriteIsNotSuccessful(t *testing.T) {
	carrier := &extenderDatagramMemoryConn{shortWrite: true}
	if err := WriteExtenderDatagram(carrier, []byte("payload")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short frame write = %v, want %v", err, io.ErrShortWrite)
	}
	conn := newExtenderPacketConn(carrier, nil)
	if n, err := conn.WriteTo([]byte("payload"), nil); n != 0 || !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short packet write = %d, %v", n, err)
	}
}

func BenchmarkExtenderPacketConnFraming(b *testing.B) {
	payload := make([]byte, 1400)
	carrier := &extenderDatagramMemoryConn{}
	conn := newExtenderPacketConn(carrier, nil)
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := conn.WriteTo(payload, nil); err != nil {
			b.Fatal(err)
		}
	}
}

func TestExtenderDatagramRemoteAddressDoesNotRequireResolution(t *testing.T) {
	for _, test := range []struct {
		name    string
		network string
		address string
		udp     bool
	}{
		{"ipv4 literal", "udp4", "192.0.2.1:443", true},
		{"ipv6 literal", "udp6", "[2001:db8::1]:443", true},
		{"unresolved hostname", "udp", "destination.invalid:443", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			addr := newExtenderDatagramAddr(test.network, test.address)
			if got := addr.String(); got != test.address {
				t.Fatalf("address = %q, want %q", got, test.address)
			}
			_, udp := addr.(*net.UDPAddr)
			if udp != test.udp {
				t.Fatalf("address type = %T, want UDP address = %t", addr, test.udp)
			}
			if !test.udp && addr.Network() != test.network {
				t.Fatalf("network = %q, want %q", addr.Network(), test.network)
			}
		})
	}
}
