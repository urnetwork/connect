package connect

import (
	"net"
	"syscall"
	"testing"
)

type packetBufferSpy struct {
	net.PacketConn
	readBufferByteCount  int
	writeBufferByteCount int
}

func (self *packetBufferSpy) SetReadBuffer(byteCount int) error {
	self.readBufferByteCount = byteCount
	return nil
}

func (self *packetBufferSpy) SetWriteBuffer(byteCount int) error {
	self.writeBufferByteCount = byteCount
	return nil
}

func TestPlatformPacketConnClampsQuicSocketRequests(t *testing.T) {
	packetConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer packetConn.Close()
	spy := &packetBufferSpy{PacketConn: packetConn}
	capped := capPlatformPacketConn(spy, 256, 128)
	readSetter := capped.(interface{ SetReadBuffer(int) error })
	writeSetter := capped.(interface{ SetWriteBuffer(int) error })
	if err := readSetter.SetReadBuffer(7 * 1024 * 1024); err != nil {
		t.Fatal(err)
	}
	if err := writeSetter.SetWriteBuffer(7 * 1024 * 1024); err != nil {
		t.Fatal(err)
	}
	if spy.readBufferByteCount != 256 || spy.writeBufferByteCount != 128 {
		t.Fatalf(
			"socket buffers = (%d, %d), want capped (256, 128)",
			spy.readBufferByteCount,
			spy.writeBufferByteCount,
		)
	}
}

func TestPlatformPacketConnPreservesUDPFastPath(t *testing.T) {
	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		t.Fatal(err)
	}
	defer udpConn.Close()

	packetConn := capPlatformPacketConn(udpConn, 256, 128)
	capped, ok := packetConn.(*cappedUDPConn)
	if !ok {
		t.Fatalf("UDP cap wrapper = %T, want *cappedUDPConn", packetConn)
	}
	if capped.readBufferByteCount != 256 || capped.writeBufferByteCount != 128 {
		t.Fatalf(
			"UDP limits = (%d, %d), want (256, 128)",
			capped.readBufferByteCount,
			capped.writeBufferByteCount,
		)
	}
	if _, ok := packetConn.(interface {
		SyscallConn() (syscall.RawConn, error)
		SetReadBuffer(int) error
		ReadMsgUDP([]byte, []byte) (int, int, int, *net.UDPAddr, error)
		WriteMsgUDP([]byte, []byte, *net.UDPAddr) (int, int, error)
	}); !ok {
		t.Fatalf("UDP cap wrapper %T lost QUIC OOB/ECN methods", packetConn)
	}
}
