// Ordinary reliable Pack and delivery ACK owners carry forwarding enrollment.
// The optional public records add no new send, contract, or retry obligation.
package connect

import (
	"time"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/encoding/protowire"
)

// A route allocates this value once. Moving its pointer through the decoded
// owner preserves the existing one-word carrier cancellation slot.
type receivePackCarrier struct {
	done       <-chan struct{}
	provenance *platformReceiveProvenance
}

// Only queue admission observes retirement of its original H1 receive lane.
func (self *ReceivePack) carrierDone() <-chan struct{} {
	if self.carrier == nil {
		return nil
	}
	return self.carrier.done
}

// Carries optional borrowed bytes through the synchronous normal ACK handler.
// No ordinary receiveAckMessage or asynchronous compatibility handoff grows.
type streamForwardingAckIngress struct {
	path       TransferPath
	provenance *platformReceiveProvenance
	wire       []byte
}

// Preserves the immutable stored grant and any unrelated envelope fields.
func appendStreamForwardingOffer(contract []byte, offer []byte) []byte {
	const fieldNumber = 15
	length := len(contract) + protowire.SizeTag(fieldNumber) + protowire.SizeBytes(len(offer))
	wire := MessagePoolGet(length)
	copy(wire, contract)
	wire = protowire.AppendTag(wire[:len(contract)], fieldNumber, protowire.BytesType)
	return protowire.AppendBytes(wire, offer)
}

// A speculative early return offer cannot enlarge an ordinary H1 head past
// its actual envelope. Keep the unchanged head when there is no offer room.
func (self *SendSequence) reciprocalOfferFitsH1(frames []*protocol.Frame, contractByteCount int) bool {
	policy := self.transferFlightPolicy()
	if !policy.h1Only || policy.h1MaxMessageByteCount <= 0 {
		return true
	}
	contractByteCount += protowire.SizeTag(15) + protowire.SizeBytes(streamForwardingOfferByteCount)
	return h1FullHeadEnvelopeByteCount(h1FrameWireByteCount(frames), contractByteCount, true,
		self.sendBufferSettings.ProtocolVersion < 2) <= policy.h1MaxMessageByteCount
}

// Each verified grant keeps one ephemeral offer across ordinary retries.
// A successor can use the same native sequence after the old grant expires.
func (self *SendSequence) streamForwardingIntroduction(
	contract *sequenceContract,
	messageId Id,
	sequenceNumber uint64,
	now time.Time,
) *streamForwardingOffer {
	if self.client == nil || self.client.streamManager == nil || self.companionContract ||
		!self.forceStream || !contract.path.IsStream() {
		return nil
	}
	reciprocal := self.contractIntermediaryIds().Len() == 0
	if reciprocal && (self.encryptionRole != sequenceTlsRoleServer || self.encryptionCompanion ||
		!self.client.streamManager.streamBuffer.canOfferReciprocal(contract.path.StreamId)) {
		return nil
	}
	previous := self.forwardingOffer.Load()
	if previous != nil && previous.grant.expirationTimeUnixMillis > now.UnixMilli() {
		return nil
	}
	offer := newStreamForwardingOffer(contract, self.sequenceId, messageId, now, reciprocal)
	if offer != nil {
		offer.sequenceNumber = sequenceNumber
		if self.forwardingOffer.CompareAndSwap(previous, offer) {
			return offer
		}
	}
	return nil
}

// The existing receive owner has verified this exact contract and applied the
// switch. Only its own original physical delivery can supply the candidate.
func (self *ReceiveSequence) noteStreamForwardingIntroduction(
	item *receiveItem,
	contract *sequenceContract,
	wire []byte,
) {
	if len(wire) != streamForwardingOfferByteCount || !item.ack || item.decodedOwner == nil || self.client.streamManager == nil {
		return
	}
	now := time.Now()
	previous := self.forwardingAnswer.Load()
	if previous != nil && previous.grant.expirationTimeUnixMillis > now.UnixMilli() {
		return
	}
	carrier := item.decodedOwner.receivePack.carrier
	if carrier == nil {
		return
	}
	path := self.source.AddDestination(self.client.ClientId()).LocalMask()
	acceptStreamForwardingOffer(contract, path, self.sequenceId, item.messageId, now, carrier.provenance,
		wire, func(answer streamForwardingAnswer) bool {
			answer.sequenceNumber = item.sequenceNumber
			return self.forwardingAnswer.CompareAndSwap(previous, &answer)
		})
}

// A queue receipt cannot establish keys. A cumulative delivery ACK may cover
// this introduction or an exact immediate delivery may name it.
func streamForwardingDeliveryCovers(selective, immediate, contractMissing bool,
	sequenceNumber, introductionNumber uint64, messageId, introductionId Id,
) bool {
	if contractMissing {
		return false
	}
	if selective {
		return immediate && sequenceNumber == introductionNumber && messageId == introductionId
	}
	return introductionNumber <= sequenceNumber
}

// Keeps acceptance on the normal delivery ACK, including a cumulative ACK
// compressed past the original message and repeated ordinary receipts.
func (self *ReceiveSequence) streamForwardingDeliveryAcceptance(ack sequenceAck) []byte {
	answer := self.forwardingAnswer.Load()
	if answer == nil || self.ctx.Err() != nil ||
		answer.grant.expirationTimeUnixMillis <= time.Now().UnixMilli() ||
		!streamForwardingDeliveryCovers(ack.selective, ack.deliveredImmediately, ack.contractMissing,
			ack.sequenceNumber, answer.sequenceNumber, ack.messageId, answer.grant.introductionId) {
		return nil
	}
	if !answer.published {
		answer.published = self.client.streamManager.enrollStreamForwarding(self.ctx, answer.grant, answer.keys, true)
	}
	if !answer.published {
		return nil
	}
	return answer.wire[:]
}

// Only the normal retained ACK identity rules establish prior delivery. An
// already retired exact introduction can still return its acceptance without
// re-applying ordinary ACK state. A missing or selective queue receipt cannot.
func (self *SendSequence) acceptStreamForwardingAck(
	ack receiveAckMessage,
	sequenceNumber uint64,
	retained bool,
	ingress []streamForwardingAckIngress,
) {
	offer := self.forwardingOffer.Load()
	if offer == nil || offer.published {
		return
	}
	if retained && streamForwardingDeliveryCovers(ack.selective, ack.deliveredImmediately, ack.contractMissing,
		sequenceNumber, offer.sequenceNumber, ack.messageId, offer.grant.introductionId) {
		offer.delivered = true
	} else if !offer.delivered || ack.contractMissing || ack.selective && !ack.deliveredImmediately ||
		ack.messageId != offer.grant.introductionId {
		return
	}
	if len(ingress) != 1 {
		return
	}
	receipt := ingress[0]
	offer.acceptAnswer(receipt.path, ack.sequenceId, time.Now(), receipt.provenance, receipt.wire, func(keys streamForwardingKeys) bool {
		offer.published = self.client.streamManager.enrollStreamForwarding(self.ctx, offer.grant, keys, false)
		return offer.published
	})
}

// Enroll only a complete optional response carried by an actual verified route;
// old or malformed optional fields leave the ordinary ACK path unchanged.
func streamForwardingIngress(path TransferPath, carrier *receivePackCarrier, ack *protocol.Ack) []streamForwardingAckIngress {
	if carrier == nil || carrier.provenance == nil || len(ack.StreamForwardingAcceptance) != streamForwardingAnswerByteCount {
		return nil
	}
	return []streamForwardingAckIngress{{path: path, provenance: carrier.provenance, wire: ack.StreamForwardingAcceptance}}
}
