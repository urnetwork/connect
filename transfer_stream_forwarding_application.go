// An enrolled endpoint authenticates complete Transfer frames with an
// integrity-only AES-GCM tag. The fixed trailer preserves the original root
// and bytes for the ordinary Transfer parser, queues and application callbacks.
package connect

import (
	"bytes"
	"context"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/binary"
	"sync"
	"sync/atomic"
	"time"
)

var streamForwardingApplicationPrefix = [...]byte{0, 'u', 'r', 'a', 'p', 'p', 1}

const (
	streamForwardingPack             = byte(1)
	streamForwardingAck              = byte(2)
	streamForwardingWrap             = byte(3)
	streamForwardingClassOffset      = len(streamForwardingApplicationPrefix)
	streamForwardingCounterOffset    = streamForwardingClassOffset + 1
	streamForwardingLengthOffset     = streamForwardingCounterOffset + 8
	streamForwardingHeaderByteCount  = streamForwardingLengthOffset + 4
	streamForwardingTrailerByteCount = streamForwardingHeaderByteCount + streamFastPathAeadTagByteCount
)

// One direction keeps its monotone send counter or authenticated receive window
// across all raw-route replacements using the same enrollment.
type streamForwardingApplicationDirection struct {
	stateLock    sync.Mutex
	aead         cipher.AEAD
	sendCounter  uint64
	replayWindow streamFastPathReplayWindow
	nonce        [12]byte
}

// One registry owner per live enrolled grant, shared by its physical routes.
// Re-enrollment creates new keys before constructing a replacement owner.
type streamForwardingAssociation struct {
	ctx                context.Context
	nativeSequenceDone <-chan struct{}
	grant              streamForwardingGrant
	destination        bool
	readiness          *p2pStreamReadiness
	readinessKeys      p2pStreamReadinessKeys
	sendPath           TransferPath
	receivePath        TransferPath
	send               streamForwardingApplicationDirection
	receive            streamForwardingApplicationDirection
	reciprocal         atomic.Pointer[streamForwardingReciprocal]
}

// The opposite paid native sequence shares this physical association's keys
// and replay windows. Its own admission and expiry are independent.
type streamForwardingReciprocal struct {
	grant              streamForwardingGrant
	transcript         [sha256.Size]byte
	nativeSequenceDone <-chan struct{}
	peerConfirmed      atomic.Bool
}

func (self *streamForwardingReciprocal) live() bool {
	if self == nil || self.grant.expirationTimeUnixMillis <= time.Now().UnixMilli() {
		return false
	}
	select {
	case <-self.nativeSequenceDone:
		return false
	default:
		return true
	}
}

// Primary frames have no reciprocal scope. Recheck the exact scoped owner
// under the direction lock before sending or consuming a replay number.
func (self *streamForwardingAssociation) currentReciprocal(scope *streamForwardingReciprocal) bool {
	return scope == nil || self.reciprocal.Load() == scope && scope.live()
}

// Admission is serialized by the owning stream buffer. A repeat keeps the
// same authorization, and only a new proof can displace a retired grant.
func (self *streamForwardingAssociation) bindReciprocal(enrollment streamForwardingEnrollment) bool {
	if !self.live() || !enrollment.live() || enrollment.grant.path != self.grant.path.Reverse() ||
		enrollment.destination == self.destination || enrollment.grant.sequenceId == self.grant.sequenceId {
		return false
	}
	if current := self.reciprocal.Load(); current != nil {
		if current.transcript == enrollment.keys.transcript {
			return current.live() && current.grant == enrollment.grant
		}
		if current.live() {
			return false
		}
	}
	self.reciprocal.Store(&streamForwardingReciprocal{
		grant: enrollment.grant, transcript: enrollment.keys.transcript,
		nativeSequenceDone: enrollment.ctx.Done(),
	})
	return true
}

// Constructs both authenticators before the registry exposes application
// receipt handling. The caller then publishes the matching readiness keys.
func newStreamForwardingAssociation(ctx context.Context, grant streamForwardingGrant, keys streamForwardingKeys, destination bool, readiness *p2pStreamReadiness) (*streamForwardingAssociation, bool) {
	var zero [sha256.Size]byte
	if readiness == nil || ctx.Err() != nil || grant.expirationTimeUnixMillis <= time.Now().UnixMilli() ||
		grant.path.StreamId == (Id{}) || grant.path.SourceId == ControlId || grant.path.DestinationId == ControlId ||
		grant.path.SourceId == grant.path.DestinationId || grant.sequenceId == (Id{}) || grant.contractId == (Id{}) ||
		keys.transcript == zero || keys.forwardApplication == zero || keys.reverseApplication == zero ||
		keys.forwardReadiness == zero || keys.reverseReadiness == zero || keys.forwardApplication == keys.reverseApplication ||
		keys.forwardReadiness == keys.reverseReadiness {
		return nil, false
	}
	localId, remoteId := grant.path.SourceId, grant.path.DestinationId
	sendKey, receiveKey := &keys.forwardApplication, &keys.reverseApplication
	readinessKeys := p2pStreamReadinessKeys{
		transcript: keys.transcript, sendKey: keys.forwardReadiness,
		receiveKey: keys.reverseReadiness, destination: destination,
		expirationTimeUnixMillis: grant.expirationTimeUnixMillis,
	}
	if destination {
		localId, remoteId = remoteId, localId
		sendKey, receiveKey = receiveKey, sendKey
		readinessKeys.sendKey, readinessKeys.receiveKey = readinessKeys.receiveKey, readinessKeys.sendKey
	}
	sendAead, err := newStreamFastPathAead(sendKey[:])
	if err != nil {
		return nil, false
	}
	receiveAead, err := newStreamFastPathAead(receiveKey[:])
	if err != nil {
		return nil, false
	}
	return &streamForwardingAssociation{
		ctx: ctx, grant: grant, destination: destination, readiness: readiness,
		readinessKeys: readinessKeys,
		sendPath:      sendTransferPath(localId, DestinationId(remoteId)),
		receivePath:   sendTransferPath(remoteId, DestinationId(localId)),
		send:          streamForwardingApplicationDirection{aead: sendAead},
		receive:       streamForwardingApplicationDirection{aead: receiveAead},
	}, true
}

// Enables physical readiness only after the registry installed this complete
// application association on both send and receive paths.
func (self *streamForwardingAssociation) installReadiness() bool {
	return self.live() && self.readiness.install(self.readinessKeys)
}

// Physical reconnects retain this deadline. Platform disconnect alone does
// not revoke an enrollment committed before the disconnect.
func (self *streamForwardingAssociation) live() bool {
	return self != nil && self.ctx.Err() == nil && self.grant.expirationTimeUnixMillis > time.Now().UnixMilli()
}

// Identifies the fixed trailer without interpreting application-controlled
// bytes as authorization. Only a successful open issues an origin receipt.
func hasStreamForwardingTrailer(wire []byte) bool {
	if len(wire) <= streamForwardingTrailerByteCount {
		return false
	}
	trailer := wire[len(wire)-streamForwardingTrailerByteCount:]
	return bytes.Equal(trailer[:streamForwardingClassOffset], streamForwardingApplicationPrefix[:]) &&
		uint64(binary.BigEndian.Uint32(trailer[streamForwardingLengthOffset:])) == uint64(len(wire)-streamForwardingTrailerByteCount)
}

// Borrows the original frame and returns a new owned physical buffer. The
// caller retains and returns the original on every outcome. Each successful
// retry emits a fresh counter while its native Transfer identity stays intact.
func (self *streamForwardingAssociation) seal(frame []byte, wireLimit int) ([]byte, bool) {
	if !self.live() || wireLimit <= streamForwardingTrailerByteCount ||
		len(frame) == 0 || len(frame) > wireLimit-streamForwardingTrailerByteCount ||
		uint64(len(frame)) > uint64(^uint32(0)) {
		return nil, false
	}
	class, reciprocal := self.classifyFrame(frame, self.sendPath, nil)
	if class == 0 || class == streamForwardingAck && reciprocal != nil && !reciprocal.peerConfirmed.Load() {
		return nil, false
	}
	direction := &self.send
	direction.stateLock.Lock()
	defer direction.stateLock.Unlock()
	if direction.sendCounter == ^uint64(0) || !self.live() || !self.currentReciprocal(reciprocal) {
		return nil, false
	}
	direction.sendCounter++
	wire := MessagePoolGet(len(frame) + streamForwardingTrailerByteCount)
	copy(wire, frame)
	trailer := wire[len(frame):]
	copy(trailer, streamForwardingApplicationPrefix[:])
	trailer[streamForwardingClassOffset] = class
	binary.BigEndian.PutUint64(trailer[streamForwardingCounterOffset:], direction.sendCounter)
	binary.BigEndian.PutUint32(trailer[streamForwardingLengthOffset:], uint32(len(frame)))
	binary.BigEndian.PutUint64(direction.nonce[4:], direction.sendCounter)
	tagOffset := len(wire) - streamFastPathAeadTagByteCount
	direction.aead.Seal(wire[tagOffset:tagOffset], direction.nonce[:], nil, wire[:tagOffset])
	if messagePoolIsSmallUnordered(frame) {
		messagePoolMarkSmallUnordered(wire)
	}
	return wire, true
}

// Borrows the physical root. The returned inner bytes are a prefix of that
// same root, which remains the caller's one owner on every outcome. Validation
// never allocates generic peer state or advances replay for a rejected frame.
func (self *streamForwardingAssociation) open(wire []byte, wireLimit int) ([]byte, byte, bool) {
	if !self.live() || len(wire) > wireLimit || !hasStreamForwardingTrailer(wire) {
		return nil, 0, false
	}
	payloadLength := len(wire) - streamForwardingTrailerByteCount
	trailer := wire[payloadLength:]
	counter := binary.BigEndian.Uint64(trailer[streamForwardingCounterOffset:])
	direction := &self.receive
	direction.stateLock.Lock()
	defer direction.stateLock.Unlock()
	if !self.live() || !direction.replayWindow.canAccept(counter) {
		return nil, 0, false
	}
	binary.BigEndian.PutUint64(direction.nonce[4:], counter)
	tagOffset := len(wire) - streamFastPathAeadTagByteCount
	if _, err := direction.aead.Open(nil, direction.nonce[:], wire[tagOffset:], wire[:tagOffset]); err != nil {
		return nil, 0, false
	}
	frame := wire[:payloadLength]
	class, reciprocal := self.classifyFrame(frame, self.receivePath, nil)
	if class == 0 || class != trailer[streamForwardingClassOffset] || !self.currentReciprocal(reciprocal) ||
		!direction.replayWindow.accept(counter) {
		return nil, 0, false
	}
	if class == streamForwardingPack && reciprocal != nil {
		reciprocal.peerConfirmed.Store(true)
	}
	return frame, class, true
}
