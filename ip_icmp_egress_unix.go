//go:build !windows && !js

package connect

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"math"
	"net"
	"slices"
	"sync"
	"time"

	"golang.org/x/net/icmp"
)

// the unix echo backend: one unprivileged datagram icmp socket per flow
// (`SOCK_DGRAM` with `IPPROTO_ICMP`/`IPPROTO_ICMPV6`).
//
// identifier semantics differ per os. linux ping sockets bind the identifier
// as the local port, rewrite outgoing ids to it, and deliver only matching
// replies. darwin passes the id through unmodified, and delivery to datagram
// icmp sockets is promiscuous: every such socket on the host receives every
// echo reply, another process's included. so where the kernel assigns no
// identifier, the flow chooses its own, drawn at random (crypto/rand) and
// skipping any a live flow of this process holds, and keeps a reply only when
// it carries that identifier and a sequence number the flow sent and has not
// yet seen answered (QD-1159). two processes then share an identifier only by
// chance, 1 in 65,535 for a pair of flows, and even then a reply to the other
// process's echo drops unless this flow awaits the same sequence number. a
// sequence number sent twice awaits two replies; a duplicate beyond them
// drops, so ping shows no DUP! through a darwin provider, as through a
// windows one. both paths also filter by the peer address.
//
// linux gates these sockets by `net.ipv4.ping_group_range` (open by default
// under systemd >= 239, and always on android); a refused socket fails the
// flow soft (see IcmpSequence.Run).
//
// the socket is made through the egress machinery (listenEgressIcmp), as every
// socket connect sends from is: the forced interface on macOS and the egress
// mark on linux land on it before its first echo, so an echo this provider
// serves for a peer never follows a tunnel this process provides back into it,
// and a socket that cannot carry them fails the flow soft rather than loop
// (QD-810). windows has no socket to bind; its file pins the source instead.

// the echo identifiers held by the flows of this process that chose their
// own. It is the process's rather than an owner's: the identifier space is
// the host's, and only a set every flow of the process shares keeps two of
// them off one identifier.
var icmpEgressProcessIdentifiers = newIcmpEchoIdentifierSet(randomIcmpEchoIdentifier)

// draws an echo identifier uniformly at random
func randomIcmpEchoIdentifier() (uint16, error) {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(b[:]), nil
}

// the echo identifiers live flows hold, so a new flow's draw skips them. Safe
// for concurrent use.
type icmpEchoIdentifierSet struct {
	draw func() (uint16, error)

	stateLock       sync.Mutex
	heldIdentifiers map[uint16]bool
}

func newIcmpEchoIdentifierSet(draw func() (uint16, error)) *icmpEchoIdentifierSet {
	return &icmpEchoIdentifierSet{
		draw:            draw,
		heldIdentifiers: map[uint16]bool{},
	}
}

// draws an identifier and holds it until release. A held draw moves on to the
// next identifier no flow holds, so a take fails only when all 65,535 are
// held. Zero is never taken: it reads as a socket the kernel left unbound.
func (self *icmpEchoIdentifierSet) take() (uint16, error) {
	identifier, err := self.draw()
	if err != nil {
		return 0, err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	// each identifier once, from the draw on, wrapping
	for range math.MaxUint16 + 1 {
		if identifier != 0 && !self.heldIdentifiers[identifier] {
			self.heldIdentifiers[identifier] = true
			return identifier, nil
		}
		identifier += 1
	}
	return 0, errors.New("Every icmp echo identifier is held by a live flow.")
}

func (self *icmpEchoIdentifierSet) release(identifier uint16) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delete(self.heldIdentifiers, identifier)
}

// the sequence numbers of a flow's echoes that await a reply, oldest first.
// The writer adds each before its echo is sent, and the reader takes one for
// each reply it keeps, so a sequence number sent twice awaits two replies.
// Past its capacity (InFlightEchoLimit) the oldest is forgotten. Safe for
// concurrent use.
type icmpEchoesInFlight struct {
	stateLock sync.Mutex
	// allocated once at the capacity, never grown
	sequenceNumbers []uint16
}

func newIcmpEchoesInFlight(settings *IcmpBufferSettings) *icmpEchoesInFlight {
	return &icmpEchoesInFlight{
		sequenceNumbers: make([]uint16, 0, max(1, settings.InFlightEchoLimit)),
	}
}

// an echo with this sequence number is about to be sent
func (self *icmpEchoesInFlight) add(sequenceNumber uint16) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if len(self.sequenceNumbers) == cap(self.sequenceNumbers) {
		// forget the oldest; a reply to it now drops
		self.sequenceNumbers = slices.Delete(self.sequenceNumbers, 0, 1)
	}
	self.sequenceNumbers = append(self.sequenceNumbers, sequenceNumber)
}

// the echo just added with this sequence number was not sent, so it awaits no
// reply
func (self *icmpEchoesInFlight) withdraw(sequenceNumber uint16) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for i := len(self.sequenceNumbers) - 1; 0 <= i; i -= 1 {
		if self.sequenceNumbers[i] == sequenceNumber {
			self.sequenceNumbers = slices.Delete(self.sequenceNumbers, i, i+1)
			return
		}
	}
}

// a reply with this sequence number arrived: reports whether the flow sent it
// and has not yet seen it answered, taking the oldest such echo when it has
func (self *icmpEchoesInFlight) answer(sequenceNumber uint16) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	i := slices.Index(self.sequenceNumbers, sequenceNumber)
	if i < 0 {
		return false
	}
	self.sequenceNumbers = slices.Delete(self.sequenceNumbers, i, i+1)
	return true
}

type icmpEgressConn struct {
	conn          *icmp.PacketConn
	ipVersion     int
	destinationIp net.IP
	identifier    uint16
	// where the flow chose its identifier (darwin), the set it holds it from
	// and the echoes that await a reply; nil where the kernel assigned it
	// (linux), whose delivery is already this socket's alone
	identifiers    *icmpEchoIdentifierSet
	echoesInFlight *icmpEchoesInFlight
	closeOnce      sync.Once

	// writer-goroutine state
	lastTtl     int
	writeBuffer []byte
	// installed on the socket, so an unchanged deadline is not installed again
	writeDeadline time.Time

	// reader-goroutine state
	readBuffer   []byte
	readDeadline time.Time

	// Tests see each echo reply the read drops for its identifier, its peer or
	// its sequence number. Runs on the reader goroutine; nil is a production
	// no-op.
	afterReplyDropForTest func(identifier uint16, sequenceNumber uint16)
}

func newIcmpEgress(ctx context.Context, ipVersion int, destinationIp net.IP, settings *IcmpBufferSettings) (icmpEgress, error) {
	egress, err := listenIcmpEgress(ipVersion, destinationIp, settings, icmpEgressProcessIdentifiers)
	if err != nil {
		return nil, err
	}
	return egress, nil
}

// opens the flow's socket through the egress machinery. A flow the kernel
// leaves without an identifier draws one from identifiers: the process's, or
// in a test one standing in for another process's.
func listenIcmpEgress(ipVersion int, destinationIp net.IP, settings *IcmpBufferSettings, identifiers *icmpEchoIdentifierSet) (*icmpEgressConn, error) {
	var network string
	switch ipVersion {
	case 4:
		network = "udp4"
	case 6:
		network = "udp6"
	default:
		return nil, icmpEgressUnsupportedError(ipVersion)
	}
	conn, err := listenEgressIcmp(network, destinationIp)
	if err != nil {
		return nil, err
	}
	kernelIdentifier := uint16(0)
	if udpAddr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		// linux ping sockets assign the echo identifier as the local port
		kernelIdentifier = uint16(udpAddr.Port)
	}
	return newIcmpEgressConn(conn, ipVersion, destinationIp, kernelIdentifier, settings, identifiers)
}

// the egress over conn, which it takes, closing it on an error. A nonzero
// kernelIdentifier is the one the kernel bound the socket to; with none the
// flow chooses its own.
func newIcmpEgressConn(
	conn *icmp.PacketConn,
	ipVersion int,
	destinationIp net.IP,
	kernelIdentifier uint16,
	settings *IcmpBufferSettings,
	identifiers *icmpEchoIdentifierSet,
) (*icmpEgressConn, error) {
	egress := &icmpEgressConn{
		conn:          conn,
		ipVersion:     ipVersion,
		destinationIp: destinationIp,
		identifier:    kernelIdentifier,
		lastTtl:       -1,
		writeBuffer:   make([]byte, settings.ReadBufferByteCount),
		readBuffer:    make([]byte, settings.ReadBufferByteCount),
	}
	if kernelIdentifier == 0 {
		// no kernel assignment (darwin): draw our own; reads filter by it and
		// by the sequence numbers sent
		identifier, err := identifiers.take()
		if err != nil {
			conn.Close()
			return nil, err
		}
		egress.identifier = identifier
		egress.identifiers = identifiers
		egress.echoesInFlight = newIcmpEchoesInFlight(settings)
	}
	return egress, nil
}

func (self *icmpEgressConn) WriteEcho(deadline time.Time, ttl int, sequenceNumber uint16, payload []byte) error {
	if 0 < ttl && ttl != self.lastTtl {
		// ttl passthrough, so ttl-limited probes do not dishonestly reach
		// the destination. best-effort: a platform that refuses the option
		// still sends with its default.
		switch self.ipVersion {
		case 4:
			if p := self.conn.IPv4PacketConn(); p != nil {
				if err := p.SetTTL(ttl); err == nil {
					self.lastTtl = ttl
				}
			}
		case 6:
			if p := self.conn.IPv6PacketConn(); p != nil {
				if err := p.SetHopLimit(ttl); err == nil {
					self.lastTtl = ttl
				}
			}
		}
	}

	messageByteCount := IcmpHeaderSize + len(payload)
	if len(self.writeBuffer) < messageByteCount {
		return icmpEgressUnsupportedError(self.ipVersion)
	}
	message := self.writeBuffer[:messageByteCount]
	switch self.ipVersion {
	case 4:
		message[0] = icmp4TypeEchoRequest
	case 6:
		message[0] = icmp6TypeEchoRequest
	}
	message[1] = 0
	// checksum, set below
	message[2] = 0
	message[3] = 0
	binary.BigEndian.PutUint16(message[4:6], self.identifier)
	binary.BigEndian.PutUint16(message[6:8], sequenceNumber)
	copy(message[IcmpHeaderSize:], payload)
	if self.ipVersion == 4 {
		// icmpv4 has no pseudo header. linux rewrites the id and fixes the
		// checksum; darwin sends the message as-is, so it must be correct
		// here. the icmpv6 checksum is always kernel-computed.
		binary.BigEndian.PutUint16(message[2:4], checksumFinish(checksumAdd(0, message)))
	}

	if !deadline.Equal(self.writeDeadline) {
		if self.conn.SetWriteDeadline(deadline) == nil {
			self.writeDeadline = deadline
		}
	}
	if self.echoesInFlight != nil {
		// before the send, so a reply that beats the send's return finds it
		self.echoesInFlight.add(sequenceNumber)
	}
	_, err := self.conn.WriteTo(message, &net.UDPAddr{
		IP: self.destinationIp,
	})
	if err != nil && self.echoesInFlight != nil {
		// a failed datagram send sent nothing, and a retry adds it again
		self.echoesInFlight.withdraw(sequenceNumber)
	}
	return err
}

func (self *icmpEgressConn) ReadEcho(deadline time.Time) (sequenceNumber uint16, payload []byte, err error) {
	if !deadline.Equal(self.readDeadline) {
		if self.conn.SetReadDeadline(deadline) == nil {
			self.readDeadline = deadline
		}
	}
	for {
		n, peer, readErr := self.conn.ReadFrom(self.readBuffer)
		if readErr != nil {
			return 0, nil, readErr
		}
		message := self.readBuffer[:n]

		if self.ipVersion == 4 && IcmpHeaderSize <= len(message) && message[0]>>4 == 4 {
			// the darwin v4 read includes the ip header (an echo reply's
			// first byte is type 0, so a leading version-4 nibble is
			// unambiguous); strip it
			ipHeaderByteCount := int(message[0]&0xf) * 4
			if Ipv4HeaderSizeWithoutExtensions <= ipHeaderByteCount && ipHeaderByteCount < len(message) {
				message = message[ipHeaderByteCount:]
			}
		}

		if len(message) < IcmpHeaderSize {
			continue
		}
		switch self.ipVersion {
		case 4:
			if message[0] != icmp4TypeEchoReply {
				continue
			}
		case 6:
			if message[0] != icmp6TypeEchoReply {
				continue
			}
		}
		identifier := binary.BigEndian.Uint16(message[4:6])
		sequenceNumber := binary.BigEndian.Uint16(message[6:8])
		if identifier != self.identifier {
			self.dropReplyForTest(identifier, sequenceNumber)
			continue
		}
		if udpAddr, ok := peer.(*net.UDPAddr); !ok || !udpAddr.IP.Equal(self.destinationIp) {
			self.dropReplyForTest(identifier, sequenceNumber)
			continue
		}
		// after every other check, so a reply this flow would drop anyway
		// does not take an awaited echo's place
		if self.echoesInFlight != nil && !self.echoesInFlight.answer(sequenceNumber) {
			// a reply to an echo this flow never sent (another process's,
			// under the same identifier), one it forgot, or a duplicate of
			// one already answered
			self.dropReplyForTest(identifier, sequenceNumber)
			continue
		}

		return sequenceNumber, message[IcmpHeaderSize:], nil
	}
}

func (self *icmpEgressConn) dropReplyForTest(identifier uint16, sequenceNumber uint16) {
	if self.afterReplyDropForTest != nil {
		self.afterReplyDropForTest(identifier, sequenceNumber)
	}
}

// Closes the socket and then gives its identifier back, once however often it
// is called, so a second close cannot release the identifier a later flow has
// taken.
func (self *icmpEgressConn) Close() {
	self.closeOnce.Do(func() {
		self.conn.Close()
		if self.identifiers != nil {
			self.identifiers.release(self.identifier)
		}
	})
}
