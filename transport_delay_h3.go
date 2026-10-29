package connect

// transport_delay_h3.go -- the delay header on h3 carriers (DELAY.md §5-§7).
//
// H3, DNS-carried H3 and the DNS pump share one negotiation in the auth
// exchange: the client offers Auth.transport_delay_version and the server's
// auth response sets transport_delay_accepted_version when it accepts. The
// auth frames themselves are never stamped. After the exchange every frame of
// the reliable stream and every DATAGRAM message carries the header in both
// directions. On the stream it sits between the frame length and the message;
// in a DATAGRAM it leads the message inside the envelope, counted by the
// envelope's length, offsets and checksum, so every size budget that fits a
// message to the DATAGRAM lane subtracts it. The stream and the DATAGRAM lane
// have independent writers and readers, so a connection keeps one write lane
// and one read lane per physical lane, sharing one transport clock and one
// echo (DELAY.md §5.1).

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// closes the write lane of a DATAGRAM message that did not enter quic-go; the
// stream lane stamps the message instead
var errTransportDelayH3MovedToStream = errors.New("H3 DATAGRAM message moved to the stream")

// SetTransportDelayH3Offer resets the response-only field and offers this
// build's delay header version when the settings offer it and the connection
// declares no provide intent (DELAY.md §6, D3).
func SetTransportDelayH3Offer(auth *protocol.Auth, settings *TransportDelaySettings, provideIntent bool) {
	auth.TransportDelayVersion = 0
	auth.TransportDelayAcceptedVersion = 0
	if settings.offered() && !provideIntent {
		auth.TransportDelayVersion = TransportDelayVersion
	}
}

// AcceptTransportDelayH3Offer is the server's half of the negotiation. It sets
// the accepted version on response and returns true only when accept allows
// it, the request offered this build's version, and the request declares no
// provide intent; otherwise it clears the accepted version and returns false.
// An accepting server must encode response: an echo of the request's raw
// bytes leaves the accepted field 0.
func AcceptTransportDelayH3Offer(request *protocol.Auth, response *protocol.Auth, accept bool) bool {
	response.TransportDelayAcceptedVersion = 0
	if !accept || request.ProvideIntent || request.TransportDelayVersion != TransportDelayVersion {
		return false
	}
	response.TransportDelayAcceptedVersion = TransportDelayVersion
	return true
}

// Accepted only when the request offered this build's version and the response
// accepted exactly that version. An old server never sets the accepted field:
// its raw echo returns the offer with the accepted field 0, and its
// reconstructed response carries neither field.
func transportDelayH3Accepted(request *protocol.Auth, response *protocol.Auth) bool {
	return request != nil && response != nil &&
		request.TransportDelayVersion == TransportDelayVersion &&
		response.TransportDelayAcceptedVersion == request.TransportDelayVersion
}

// The client's check of the server's answer. Any other nonzero accepted
// version is a protocol error, since the server would then stamp a header the
// client does not strip. The offer is never part of the strict auth echo
// (ValidateH3DatagramAuthResponse), so a server that does not know the field
// still validates.
func negotiateTransportDelayH3AuthResponse(request *protocol.Auth, response *protocol.Auth) (bool, error) {
	if transportDelayH3Accepted(request, response) {
		return true, nil
	}
	if response != nil && response.TransportDelayAcceptedVersion != 0 {
		return false, fmt.Errorf(
			"unsupported H3 transport delay accepted version %d",
			response.TransportDelayAcceptedVersion,
		)
	}
	return false, nil
}

// The framing of the H3 reliable stream: a Framer on the legacy wire, the
// connection's TransportDelayH3 when the auth exchange negotiated the header.
type h3StreamFraming interface {
	Read(r io.Reader) ([]byte, error)
	WriteBatchWithStorage(w io.Writer, messages [][]byte, storage []byte) error
}

// TransportDelayH3 is the delay state of one negotiated H3 connection. Its
// stream methods frame like a Framer and its DATAGRAM methods wrap the
// connection's fragmenter and reassembler, so each lane switches to the header
// wire by reading and writing through it. Each lane keeps the single writer
// and single reader the H3 pumps already have: the stream writer, the
// DATAGRAM dispatcher, the stream reader and the DATAGRAM receive pump. A nil
// value is the legacy wire for HeaderByteCount and NoteHandoff.
type TransportDelayH3 struct {
	delay  *transportDelay
	framer *Framer

	streamWriteLane   *transportDelayWriteLane
	streamReadLane    *transportDelayReadLane
	datagramWriteLane *transportDelayWriteLane
	datagramReadLane  *transportDelayReadLane
}

// The header framer keeps framerSettings' message limit, which bounds the
// message alone.
func newTransportDelayH3(delay *transportDelay, framerSettings *FramerSettings) *TransportDelayH3 {
	headerFramerSettings := *framerSettings
	headerFramerSettings.HeaderByteCount = transportDelayHeaderByteCount
	return &TransportDelayH3{
		delay:             delay,
		framer:            NewFramer(&headerFramerSettings),
		streamWriteLane:   delay.newWriteLane(),
		streamReadLane:    delay.newReadLane(transportDelayLaneOrdered),
		datagramWriteLane: delay.newUnreliableWriteLane(),
		datagramReadLane:  delay.newReadLane(transportDelayLaneUnreliable),
	}
}

// NewTransportDelayH3Peer is the server's half of a connection whose auth
// response accepted the delay header: it stamps and echoes without measuring
// (DELAY.md §11). framerSettings are those of the connection's stream framer.
func NewTransportDelayH3Peer(clientId Id, framerSettings *FramerSettings) *TransportDelayH3 {
	return newTransportDelayH3(
		newTransportDelay(nil, clientId, TransportTypeH3, nil),
		framerSettings,
	)
}

// The fixed header every stream frame and DATAGRAM message carries; 0 on nil,
// the legacy wire.
func (self *TransportDelayH3) HeaderByteCount() int {
	if self == nil {
		return 0
	}
	return transportDelayHeaderByteCount
}

// WriteBatchWithStorage writes messages in one stamped physical write on the
// stream lane; a heartbeat is the batch of one empty message. Storage and
// ownership follow Framer.WriteBatchWithStorage, except that each frame takes
// HeaderByteCount more storage.
func (self *TransportDelayH3) WriteBatchWithStorage(
	w io.Writer,
	messages [][]byte,
	storage []byte,
) error {
	if len(messages) == 0 {
		return nil
	}
	write := self.streamWriteLane.beginWrite()
	err := self.framer.WriteBatchWithHeaderStorage(w, write.header[:], messages, storage)
	byteCount := 0
	if err == nil {
		for _, message := range messages {
			byteCount += 4 + transportDelayHeaderByteCount + len(message)
		}
	}
	self.streamWriteLane.endWrite(write, byteCount, err)
	return err
}

// Read reads one stream frame and accounts its header on the stream lane. The
// returned message is pooled, as from Framer.Read, and never holds the header.
func (self *TransportDelayH3) Read(r io.Reader) ([]byte, error) {
	var header transportDelayHeader
	message, err := self.framer.ReadWithHeader(r, header[:])
	if err != nil {
		return nil, err
	}
	self.streamReadLane.receive(header[:], 4+transportDelayHeaderByteCount+len(message))
	return message, nil
}

// SendHybrid stamps one routed message on the DATAGRAM lane and sends it
// through fragmenter with the header inside its envelope, as
// H3DatagramFragmenter.SendHybrid does on the legacy wire. A message that
// moves to the stream records no DATAGRAM write. Ownership of message stays
// with the caller.
func (self *TransportDelayH3) SendHybrid(
	fragmenter *H3DatagramFragmenter,
	message []byte,
	maxDatagramByteCount int,
	send func(datagram []byte) error,
) (useStream bool, nextMaxDatagramByteCount int, err error) {
	write := self.datagramWriteLane.beginWrite()
	var fragmentCount int
	useStream, nextMaxDatagramByteCount, fragmentCount, err = fragmenter.sendHybrid(
		message,
		write.header[:],
		maxDatagramByteCount,
		send,
	)
	switch {
	case err != nil:
		self.datagramWriteLane.endWrite(write, 0, err)
	case useStream:
		self.datagramWriteLane.endWrite(write, 0, errTransportDelayH3MovedToStream)
	default:
		self.datagramWriteLane.endWrite(
			write,
			fragmentCount*H3DatagramHeaderByteCount+transportDelayHeaderByteCount+len(message),
			nil,
		)
	}
	return useStream, nextMaxDatagramByteCount, err
}

// Accept accepts one DATAGRAM through reassembler and accounts the header of a
// completed message on the DATAGRAM lane. The returned message is pooled, as
// from H3DatagramReassembler.Accept, and never holds the header.
func (self *TransportDelayH3) Accept(
	reassembler *H3DatagramReassembler,
	datagram []byte,
	now time.Time,
) []byte {
	var header transportDelayHeader
	message := reassembler.AcceptWithHeader(datagram, now, header[:])
	if message == nil {
		return nil
	}
	// the completing envelope declares the message's fragment count
	fragmentCount := int(binary.BigEndian.Uint16(datagram[26:28]))
	self.datagramReadLane.receive(
		header[:],
		fragmentCount*H3DatagramHeaderByteCount+transportDelayHeaderByteCount+len(message),
	)
	return message
}

// NoteHandoff records how long a reader was held handing its previous message
// onward (DELAY.md §4.2): the stream reader for a reliable message, the
// DATAGRAM receive pump for an unreliable one. A long handoff censors the
// lane's next delay sample. Nothing on nil.
func (self *TransportDelayH3) NoteHandoff(reliability CarrierReliability, handoff time.Duration) {
	if self == nil {
		return
	}
	if reliability == CarrierReliabilityUnreliable {
		self.datagramReadLane.noteHandoff(handoff)
	} else {
		self.streamReadLane.noteHandoff(handoff)
	}
}

// Publishes the interval in progress. Call once the connection's lanes have
// stopped.
func (self *TransportDelayH3) flush() {
	self.delay.flush()
}
