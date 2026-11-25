package connect

import (
	"context"
	"math"
	"sync"
	"sync/atomic"

	"github.com/urnetwork/connect/protocol"
)

// The race owns this receipt from the original source ordinal to the generated
// reset. Its source channel identifies the worker without retaining its graph.
type nativeRaceOpening struct {
	source       <-chan struct{}
	ordinal      uint64
	serial       uint64
	isn          uint32
	offer        *connectSynOffer
	winner       *multiClientAffinitySelection
	epoch        uint64
	resetOrdinal uint64
}

type nativeAbandonedCandidate struct {
	client  *multiClientChannel
	opening *nativeRaceOpening
}

// Called at the original ordinal assignment under the same flow lock as the
// winner. Other SYN offers, including retransmissions, keep independent debt.
func (group *parsedPacketGroup) noteConnectedRaceOpeningWithLock(source *SendSequence, ordinal uint64) bool {
	if !group.nativeRaceOpening() || group.connectSynOffer == nil || !group.connectSynOffer.orderedReturnEligible {
		return false
	}
	request := group.synRetry
	update := request.update
	race := update.race
	if race == nil || race.single || group.completionFlags.Load()&groupCompletionAbandoned != 0 ||
		request.openingSerial != update.synOpeningSerial || !update.dialFailedSynSeen ||
		group.packets[0].ipPath.SequenceNumber != update.dialFailedSyn {
		return false
	}
	state := race.clientStates[group.completionClient]
	if state == nil || state.connectedOpening != nil || state.collapseControl.closed || state.collapseControl.ambiguousSyn ||
		!state.collapseControl.synSeen || state.collapseControl.synNumber != update.dialFailedSyn ||
		state.collapseControl.sourceId != source.sequenceId ||
		state.collapseControl.synOrder == 0 || state.collapseControl.synOrder != group.collapseOrder() {
		return false
	}
	state.connectedOpening = &nativeRaceOpening{
		source: source.ctx.Done(), ordinal: ordinal, serial: request.openingSerial,
		isn: update.dialFailedSyn, offer: group.connectSynOffer,
	}
	return true
}

func (group *parsedPacketGroup) solicitConnectedRaceReset(source *SendSequence, ordinal uint64) bool {
	opening := group.nativeRaceReset
	update := group.collapseAdmission.update
	if opening == nil || update == nil {
		return false
	}
	update.stateLock.Lock()
	defer update.stateLock.Unlock()
	winner := opening.winner
	opening.winner = nil
	if opening.source != source.ctx.Done() || ordinal <= opening.ordinal || ordinal-opening.ordinal > math.MaxUint16 ||
		!opening.offer.materialized.Load() || winner == nil || update.client.loadSelection() != winner ||
		winner.client == group.completionClient ||
		update.sequenceAdmissionEpoch != opening.epoch || update.synOpeningSerial != opening.serial ||
		!update.dialFailedSynSeen || update.dialFailedSyn != opening.isn || !update.receivedInbound.Load() ||
		update.race != nil || update.sourceReset.Load() || update.IsDone() || group.completionClient.IsDone() {
		return false
	}
	opening.resetOrdinal = ordinal
	return true
}

// The provider records only a solicited, installed TCP generation. A matching
// source identity or allocated socket alone is insufficient; the dial must
// actually return before that same live generation processes its reset.
type providerRaceOpening struct {
	source     <-chan struct{}
	ordinal    uint64
	connected  atomic.Bool
	cancelOnce sync.Once
	cancel     context.CancelFunc
}

func (owner *providerReliablePacket) connectedRaceReceipt() *receiveDeliveryReceipt {
	if owner == nil || owner.operation == nil || owner.operation.claim == nil {
		return nil
	}
	receipt := owner.operation.claim.receipt
	if receipt == nil || !receipt.connectRace || !receipt.required {
		return nil
	}
	return receipt
}

func (opening *providerRaceOpening) cancelNormally() {
	opening.cancelOnce.Do(opening.cancel)
}

func (opening *providerRaceOpening) cancelByReset(parent context.Context, owner *providerReliablePacket, tcp *parsedTcp) uint32 {
	receipt := owner.connectedRaceReceipt()
	if receipt == nil || !tcp.rst || tcp.syn || tcp.ack || tcp.fin || tcp.psh || tcp.seq != 0 || len(tcp.payload) != 0 ||
		!opening.connected.Load() || receipt.queue.sequence.ctx.Done() != opening.source ||
		receipt.ack.sequenceNumber <= opening.ordinal || receipt.ack.sequenceNumber-opening.ordinal > math.MaxUint16 {
		return 0
	}
	var outcome uint32
	// Normal close and this reset call the original CancelFunc through the
	// same once. The buffer parent can close independently, so check its
	// state after cancellation before crediting this reset with the action.
	opening.cancelOnce.Do(func() {
		opening.cancel()
		if parent.Err() == nil {
			outcome = uint32(receipt.ack.sequenceNumber - opening.ordinal)
		}
	})
	return outcome
}

// Carry at most one outcome with an ordinary cumulative prefix. The newer
// outcome wins. An unrepresentable distance drops optional evidence only.
func (ack *sequenceAck) inheritConnectedRaceReset(previous sequenceAck) {
	if ack.connectedRaceReset != 0 || uint16(previous.connectedRaceReset) == 0 || previous.selective ||
		ack.selective || ack.sequenceNumber < previous.sequenceNumber {
		return
	}
	delta := ack.sequenceNumber - previous.sequenceNumber
	distance := uint64(previous.connectedRaceReset >> 16)
	if delta <= math.MaxUint16-distance {
		ack.connectedRaceReset = uint32(delta+distance)<<16 | uint32(uint16(previous.connectedRaceReset))
	}
}

// The ordinary peer and retained message checks have resolved this cumulative
// prefix. Match both ordinal distances to its real retained local reset and
// its own SYN offer before retiring any health evidence.
func (source *SendSequence) applyConnectedRaceReset(item *sendItem, ack sequenceAck, head uint64) {
	distance := uint64(ack.connectedRaceReset >> 16)
	originalDistance := uint64(uint16(ack.connectedRaceReset))
	if originalDistance == 0 || ack.selective || ack.contractMissing || ack.deliveredImmediately ||
		ack.immediateSequenceNumberSet || ack.sequenceNumber != head || distance > head ||
		head-distance != item.sequenceNumber || originalDistance > item.sequenceNumber ||
		item.deliverImmediately || item.acks.count != 1 {
		return
	}
	record := &item.acks.records[0]
	target := record.target
	if record.group != nil {
		target = record.group.ack.target
	}
	group, ok := target.(*parsedPacketGroup)
	if !ok || group.nativeRaceReset == nil {
		return
	}
	opening := group.nativeRaceReset
	if opening.source != source.ctx.Done() || opening.resetOrdinal != item.sequenceNumber ||
		opening.ordinal != item.sequenceNumber-originalDistance || !opening.offer.materialized.Load() {
		return
	}
	opening.offer.orderedAnswer.Store(true)
	group.completionClient.connectSynAnswersPending.Store(true)
}

// The ordinary response keeps its entire reservation. Size the optional result
// against the largest outer envelope permitted by the actual AES-GCM wire.
// Current nonce and tag widths are fixed by newSequenceCipher. This also
// protects a plaintext ACK if a session becomes ready before the actual write.
func nativeConnectedRaceAckFits(path TransferPath, plaintext, evictions int) bool {
	const gcmTagByteCount = 16
	ciphertext := sequenceTlsAeadNonceSize + plaintext + gcmTagByteCount
	largest := sizeEncryptedOuterTransferFrame(path, ciphertext, protocol.SequenceRole_SequenceRoleServer)
	return largest <= ackResponseEntryMaxByteCount+10*evictions
}
