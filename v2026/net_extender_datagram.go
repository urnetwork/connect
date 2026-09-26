package connect

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"
)

// Datagrams over an extender carrier.
//
// An extender carrier is one reliable byte stream and the extender cannot look
// inside the inner TLS, so it cannot reframe anything it relays. A client
// pinned to an h3 carrier therefore had no extender path at all: quic needs
// packet boundaries, and a byte stream does not have them.
//
// This adds them explicitly. Each datagram travels as a two byte big endian
// length followed by that many bytes, and the extender turns the frames back
// into real udp packets at the far end (ExtenderHeader.Datagram).
//
// What the framing preserves is boundaries, and only boundaries. The carrier
// is reliable and ordered, so on the client-to-extender leg a datagram cannot
// be dropped or reordered the way a real udp path would allow. Quic tolerates
// that: it carries its own loss recovery and simply never exercises it on a
// leg that does not lose anything. Loss past the extender is real and is
// signalled the ordinary way, by the packet not arriving.
//
// Head-of-line blocking is the cost. A carrier stall delays every datagram
// behind it, where a udp path would have delivered the later ones. That is the
// accepted trade for reaching a network that is otherwise unreachable, and it
// is why this path is a fallback rather than a default.

// extenderDatagramMaxSize bounds one datagram on the wire.
//
// The length prefix is two bytes, so the format allows 65535. This is smaller
// on purpose: a quic datagram is an MTU-sized packet, and a bound near the
// real maximum keeps a malformed or hostile length from making either side
// allocate 64 KiB per frame.
const extenderDatagramMaxSize = 2048

// extenderDatagramLenSize is the width of the length prefix.
const extenderDatagramLenSize = 2

var errExtenderDatagramTooLarge = errors.New("extender datagram exceeds the maximum size")

// extenderDatagramAddr retains an unresolved destination name for ReadFrom.
// The address is descriptive only: the extender already received the actual
// destination in its header, so resolving it again would add an unaudited
// network operation on the client.
type extenderDatagramAddr struct {
	network string
	address string
}

func (self extenderDatagramAddr) Network() string { return self.network }
func (self extenderDatagramAddr) String() string  { return self.address }

func newExtenderDatagramAddr(network string, address string) net.Addr {
	if addrPort, err := netip.ParseAddrPort(address); err == nil {
		return net.UDPAddrFromAddrPort(addrPort)
	}
	return extenderDatagramAddr{network: network, address: address}
}

// WriteExtenderDatagram writes one length-prefixed datagram.
//
// One Write, not two: a length that reached the far end without its payload
// would desynchronize the stream permanently, and every later frame would be
// read at the wrong offset.
func WriteExtenderDatagram(w io.Writer, datagram []byte) error {
	if len(datagram) > extenderDatagramMaxSize {
		return fmt.Errorf("%w: %d > %d", errExtenderDatagramTooLarge, len(datagram), extenderDatagramMaxSize)
	}
	frame := make([]byte, extenderDatagramLenSize+len(datagram))
	return writeExtenderDatagram(w, datagram, frame)
}

// ExtenderDatagramWriter owns one bounded frame scratch buffer for a carrier.
// Its zero value is ready to use. Writes must be serialized, and the writer
// must not be copied after use. Reuse avoids allocating a packet-sized frame
// for every datagram on the mobile or server relay path.
type ExtenderDatagramWriter struct {
	frame [extenderDatagramLenSize + extenderDatagramMaxSize]byte
}

func (self *ExtenderDatagramWriter) Write(w io.Writer, datagram []byte) error {
	if len(datagram) > extenderDatagramMaxSize {
		return fmt.Errorf("%w: %d > %d", errExtenderDatagramTooLarge, len(datagram), extenderDatagramMaxSize)
	}
	return writeExtenderDatagram(w, datagram, self.frame[:extenderDatagramLenSize+len(datagram)])
}

func writeExtenderDatagram(w io.Writer, datagram []byte, frame []byte) error {
	binary.BigEndian.PutUint16(frame[:extenderDatagramLenSize], uint16(len(datagram)))
	copy(frame[extenderDatagramLenSize:], datagram)
	n, err := w.Write(frame)
	if err == nil && n != len(frame) {
		return io.ErrShortWrite
	}
	return err
}

// ReadExtenderDatagram reads one length-prefixed datagram into buffer.
//
// A frame longer than the bound is refused rather than skipped: the stream
// cannot be trusted to be in sync after it, so the only safe move is to end
// the connection.
func ReadExtenderDatagram(r io.Reader, buffer []byte) (int, error) {
	var reader ExtenderDatagramReader
	return reader.Read(r, buffer)
}

// ExtenderDatagramReader retains the small length header instead of letting
// io.ReadFull's interface escape allocate it for every packet. Its zero value
// is ready to use; reads must be serialized and it must not be copied after use.
type ExtenderDatagramReader struct {
	header [extenderDatagramLenSize]byte
}

func (self *ExtenderDatagramReader) Read(r io.Reader, buffer []byte) (int, error) {
	if _, err := io.ReadFull(r, self.header[:]); err != nil {
		return 0, err
	}
	size := int(binary.BigEndian.Uint16(self.header[:]))
	if size > extenderDatagramMaxSize {
		return 0, fmt.Errorf("%w: %d > %d", errExtenderDatagramTooLarge, size, extenderDatagramMaxSize)
	}
	if size > len(buffer) {
		// The caller's buffer is smaller than the datagram. Draining it keeps
		// the stream in sync, and the short read is reported the way a udp
		// socket reports one.
		if _, err := io.CopyN(io.Discard, r, int64(size)); err != nil {
			return 0, err
		}
		return 0, io.ErrShortBuffer
	}
	if _, err := io.ReadFull(r, buffer[:size]); err != nil {
		return 0, err
	}
	return size, nil
}

// extenderPacketConn presents one extender carrier as a net.PacketConn, so
// quic can run over it unchanged.
//
// The connection is point to point: every datagram goes to the destination the
// extender header named, and every datagram read came from it. WriteTo ignores
// the address for that reason, and ReadFrom always reports the same remote.
// That is what quic expects of a connected socket.
type extenderPacketConn struct {
	conn   net.Conn
	remote net.Addr

	readMutex  sync.Mutex
	writeMutex sync.Mutex
	reader     ExtenderDatagramReader
	writer     ExtenderDatagramWriter

	closeOnce sync.Once
	closeErr  error
}

// newExtenderPacketConn wraps a relayed extender stream.
//
// remote is what ReadFrom reports and is the destination the extender was
// asked to relay to. It is informational: the extender decides where the
// datagrams actually go, from the header it already accepted.
func newExtenderPacketConn(conn net.Conn, remote net.Addr) *extenderPacketConn {
	return &extenderPacketConn{conn: conn, remote: remote}
}

func (self *extenderPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	// Serialized because a frame is a length and a payload: two concurrent
	// readers would interleave halves and desynchronize the stream.
	self.readMutex.Lock()
	defer self.readMutex.Unlock()

	for {
		n, err := self.reader.Read(self.conn, p)
		if err == io.ErrShortBuffer {
			// Oversized for this caller's buffer, already drained. A udp
			// socket would truncate; quic sizes its buffers to its own
			// maximum, so this should not happen, and looping keeps one
			// undersized read from ending the connection.
			continue
		}
		if err != nil {
			return 0, nil, err
		}
		return n, self.remote, nil
	}
}

func (self *extenderPacketConn) WriteTo(p []byte, _ net.Addr) (int, error) {
	self.writeMutex.Lock()
	defer self.writeMutex.Unlock()

	if err := self.writer.Write(self.conn, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (self *extenderPacketConn) Close() error {
	self.closeOnce.Do(func() {
		self.closeErr = self.conn.Close()
	})
	return self.closeErr
}

func (self *extenderPacketConn) LocalAddr() net.Addr {
	return self.conn.LocalAddr()
}

func (self *extenderPacketConn) SetDeadline(t time.Time) error {
	return self.conn.SetDeadline(t)
}

func (self *extenderPacketConn) SetReadDeadline(t time.Time) error {
	return self.conn.SetReadDeadline(t)
}

func (self *extenderPacketConn) SetWriteDeadline(t time.Time) error {
	return self.conn.SetWriteDeadline(t)
}

// IsExtenderDatagramTooLarge reports a datagram that does not fit the frame
// bound. The extender drops such a reply rather than ending a working relay
// for one bad packet.
func IsExtenderDatagramTooLarge(err error) bool {
	return errors.Is(err, errExtenderDatagramTooLarge)
}

// ExtenderPacketConnRemoteAddr is the peer a relayed packet connection reports,
// or nil when the connection is not one.
//
// Quic dials a peer and then matches arriving packets against it, so a caller
// that supplies its own idea of the address gets every packet discarded. An
// extender connection knows the only peer it has.
func ExtenderPacketConnRemoteAddr(packetConn net.PacketConn) net.Addr {
	if extenderConn, ok := packetConn.(*extenderPacketConn); ok {
		return extenderConn.remote
	}
	return nil
}
