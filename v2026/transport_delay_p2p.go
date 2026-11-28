package connect

// transport_delay_p2p.go -- the delay header on P2P peer connections (DELAY.md
// §6.1).
//
// Negotiation rides the signal batches of the offer/answer exchange. The
// active side of a client that offers the header puts TransportDelayVersion
// on every ExchangeSignals batch it sends; the passive side records the
// version on the batch of the offer it applies and, when it accepts, puts its
// own version on every later batch, including the answer. The active side
// records the version on the batch of the answer it applies. Each side stores
// the remote version before applying the description, and the data channel
// cannot open before both descriptions apply, so both sides know the outcome
// at "rdy". The "rdy" marker itself is never stamped; every later message in
// both directions is.
//
// A client offers on a new generation only when its settings offer the header
// and the latest edge answer to the offer of one of its platform connections
// accepted it, so never before an edge has answered. The edges never see this
// exchange, so this is how their runtime switch (DELAY.md §11) reaches P2P. A
// generation decides once, when it starts, and keeps what it negotiated.
//
// One PeerConnection generation keeps one transportDelay (one transport clock
// and echo) with a write and read lane per physical lane:
//   - the legacy SCTP data channel is reliable and unordered. Each SCTP message
//     is one physical write, so the header is the first bytes of the message.
//     The send worker and the legacy queue worker share the write lane; their
//     writes never overlap.
//   - the native SRTP fast lane is unreliable. The header is the first bytes of
//     the message inside the 'URD' v2 fragment envelope, so the envelope's
//     length counts it and the envelope version is unchanged. The fragment
//     payload stays 1188 bytes, so a message within 12 bytes of a fragment
//     boundary takes one more fragment.
//
// Classification (endpoint probes, small unordered sends) sees the unstamped
// message on both ends: a sender stamps at the physical write and a receiver
// strips before anything else looks at the message. A header is stripped by
// moving the payload over it in place, so the payload keeps its pooled buffer
// and capacity class.
//
// Relays: a stream intermediary runs one P2pTransport per adjacent hop, each
// with its own PeerConnection and negotiation. The hop's receive transport
// strips the header, the intermediary forwards the bare transfer frame, and
// the next hop's send transport stamps it again only when that hop
// negotiated. The header is per hop and never forwarded, so no hop strips
// twice and a legacy hop next to a negotiated one keeps its legacy wire.
//
// Measurement follows DELAY.md D3: only the active side of a client that
// offers the header (a window client) measures and publishes, to its client's
// route manager. The passive side, such as a provider, stamps and echoes.

import (
	"io"
	"math"
	"net"
	"time"
)

// a handoff that always censors the next delay sample. A message the ready
// header reader prefetched waited behind the marker for a local reason.
const transportDelayP2pReceiverLimitedHandoff = time.Duration(math.MaxInt64)

// The version a passive peer accepts for an offered version: the version
// this build speaks once the offer reaches it, else 0, which declines.
func transportDelayP2pAcceptedVersion(offeredVersion uint32) uint32 {
	if offeredVersion < TransportDelayVersion {
		return 0
	}
	return TransportDelayVersion
}

// The version this generation puts on its signal batches: the active side's
// offer, or the passive side's acceptance of the offer it applies.
func (self *peerConn) localTransportDelayVersion() uint32 {
	if self.active {
		if self.transportDelayOffered {
			return TransportDelayVersion
		}
		return 0
	}
	return transportDelayP2pAcceptedVersion(self.remoteTransportDelayVersion.Load())
}

// Reports whether both sides of this generation put version 1 on the batches
// of the applied offer and answer, so both data lanes carry the header after
// "rdy". Call after the data channel opens.
func (self *peerConn) transportDelayNegotiated() bool {
	remoteVersion := self.remoteTransportDelayVersion.Load()
	if self.active {
		return self.transportDelayOffered && remoteVersion == TransportDelayVersion
	}
	return transportDelayP2pAcceptedVersion(remoteVersion) == TransportDelayVersion
}

// A connection that negotiates the delay header in its signal exchange.
// Browser and test connections do not implement it and keep the legacy wire.
type p2pTransportDelayConn interface {
	transportDelayNegotiated() bool
}

// Writes a header and a message as one fast-path message without joining
// them first. A fast-path connection without it, such as a test double, gets
// one pooled joined copy instead.
type webRtcFastPathHeaderConn interface {
	WriteFastPathMessageWithHeader(header transportDelayHeader, message []byte) (fragmentCount int, err error)
}

// The delay state of one negotiated PeerConnection generation, shared by its
// send and receive transports. Nil is the legacy wire; the methods that size
// messages and flush accept nil.
type p2pTransportDelay struct {
	delay       *transportDelay
	legacyWrite *transportDelayWriteLane
	legacyRead  *transportDelayReadLane
	fastWrite   *transportDelayWriteLane
	fastRead    *transportDelayReadLane
}

func newP2pTransportDelay(delay *transportDelay) *p2pTransportDelay {
	return &p2pTransportDelay{
		delay:       delay,
		legacyWrite: delay.newWriteLane(),
		legacyRead:  delay.newReadLane(transportDelayLaneUnordered),
		fastWrite:   delay.newUnreliableWriteLane(),
		fastRead:    delay.newReadLane(transportDelayLaneUnreliable),
	}
}

// Starts the delay state of a generation whose signal exchange negotiated the
// header, or returns nil for the legacy wire. The active side measures with
// settings and publishes; the passive side stamps and echoes only.
func newP2pTransportDelayForConn(
	conn net.Conn,
	active bool,
	settings *TransportDelaySettings,
	clientId Id,
	publish TransportDelayStatsFunction,
) *p2pTransportDelay {
	negotiatedConn, ok := conn.(p2pTransportDelayConn)
	if !ok || !negotiatedConn.transportDelayNegotiated() {
		return nil
	}
	if !active {
		settings = nil
		publish = nil
	}
	return newP2pTransportDelay(newTransportDelay(settings, clientId, TransportTypeP2p, publish))
}

// The delay settings a new active generation of this client offers: the
// client's own while the latest answer of its edges accepted the header, else
// nil, which offers nothing (DELAY.md §6.1).
func (self *Client) transportDelayP2pOffer() *TransportDelaySettings {
	if !self.routeManager.transportDelayEdgeAccepted.Load() {
		return nil
	}
	return self.settings.TransportDelay
}

// Starts the delay state of the generation conn carries. The active side is
// the client that offered (DELAY.md D3); it publishes to its client's route
// manager, which every transport of the client shares.
func (self *P2pTransport) newP2pTransportDelay(conn net.Conn) *p2pTransportDelay {
	var publish TransportDelayStatsFunction
	if routeManager := self.client.RouteManager(); routeManager != nil {
		publish = routeManager.publishTransportDelayStats
	}
	return newP2pTransportDelayForConn(
		conn,
		self.peerType == PeerTypeDestination,
		self.client.settings.TransportDelay,
		self.client.ClientId(),
		publish,
	)
}

// The bytes the header adds to each physical message.
func (self *p2pTransportDelay) headerByteCount() int {
	if self == nil {
		return 0
	}
	return transportDelayHeaderByteCount
}

// Writes one stamped SCTP message. An SCTP message is one contiguous write,
// so the header and message share one pooled copy; the stamp is taken after
// the copy so the write duration is the carrier's alone. Returns the message
// bytes written, excluding the header.
func (self *p2pTransportDelay) writeLegacy(conn net.Conn, message []byte) (int, error) {
	stamped := MessagePoolGet(transportDelayHeaderByteCount + len(message))
	defer MessagePoolReturn(stamped)
	copy(stamped[transportDelayHeaderByteCount:], message)
	write := self.legacyWrite.beginWrite()
	copy(stamped, write.header[:])
	n, err := conn.Write(stamped)
	if n < len(stamped) && err == nil {
		err = io.ErrShortWrite
	}
	self.legacyWrite.endWrite(write, len(stamped), err)
	return max(0, n-transportDelayHeaderByteCount), err
}

// Writes one stamped fast-path message. Each message is its own physical
// write; the lane is lossy, so the reader takes delay samples only.
func (self *p2pTransportDelay) writeFast(conn webRtcFastPathConn, message []byte) (int, error) {
	write := self.fastWrite.beginWrite()
	var fragmentCount int
	var err error
	if headerConn, ok := conn.(webRtcFastPathHeaderConn); ok {
		fragmentCount, err = headerConn.WriteFastPathMessageWithHeader(write.header, message)
	} else {
		stamped := MessagePoolGet(transportDelayHeaderByteCount + len(message))
		copy(stamped, write.header[:])
		copy(stamped[transportDelayHeaderByteCount:], message)
		fragmentCount, err = conn.WriteFastPathMessage(stamped)
		MessagePoolReturn(stamped)
	}
	self.fastWrite.endWrite(
		write,
		transportDelayHeaderByteCount+len(message)+fragmentCount*p2pFastPathFragmentHeaderByteCount,
		err,
	)
	return fragmentCount, err
}

// Accounts and strips the header of one SCTP message read after "rdy".
// Returns the payload in the same pooled buffer, or nil after releasing a
// message that carries no payload.
func (self *p2pTransportDelay) receiveLegacy(message []byte, prefetched bool) []byte {
	if len(message) < transportDelayHeaderByteCount {
		// malformed: the peer negotiated a header on every message
		MessagePoolReturn(message)
		return nil
	}
	if prefetched {
		self.legacyRead.noteHandoff(transportDelayP2pReceiverLimitedHandoff)
	}
	self.legacyRead.receive(message[:transportDelayHeaderByteCount], len(message))
	return stripP2pTransportDelayHeader(message)
}

// Records how long the SCTP reader waited handing the previous message to
// its route. A long wait censors the next delay sample.
func (self *p2pTransportDelay) noteLegacyHandoff(handoff time.Duration) {
	self.legacyRead.noteHandoff(handoff)
}

// Accounts and strips the header of one reassembled fast-path message of
// fragmentCount fragments. Returns the payload in the same pooled buffer, or
// nil after releasing a message that carries no payload.
func (self *p2pTransportDelay) receiveFast(message []byte, fragmentCount int) []byte {
	if len(message) < transportDelayHeaderByteCount {
		MessagePoolReturn(message)
		return nil
	}
	self.fastRead.receive(
		message[:transportDelayHeaderByteCount],
		len(message)+fragmentCount*p2pFastPathFragmentHeaderByteCount,
	)
	return stripP2pTransportDelayHeader(message)
}

// Publishes the interval in progress. Call once the generation's transports
// are done.
func (self *p2pTransportDelay) flush() {
	if self != nil {
		self.delay.flush()
	}
}

// Moves the payload over the header in place. A subslice past the header
// would change the capacity MessagePoolReturn classifies by and leave the
// pool. A message with no payload is released and yields nil.
func stripP2pTransportDelayHeader(message []byte) []byte {
	payloadByteCount := copy(message, message[transportDelayHeaderByteCount:])
	if payloadByteCount == 0 {
		MessagePoolReturn(message)
		return nil
	}
	return message[:payloadByteCount]
}
