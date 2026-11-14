// Reliable materialization reserves its complete owner before taking raw frames.
package connect

import (
	"fmt"

	"github.com/urnetwork/connect/v2026/protocol"
	"google.golang.org/protobuf/proto"
)

// Borrows frames and completion records. Failure changes no sequence number,
// callback phase, disposition or raw owner. Success returns one reserved item;
// its caller must either pass it to serialization or return its memory owner.
func (self *SendSequence) prepareSendItem(
	frames []*protocol.Frame,
	acks sendAckSet,
	ack bool,
	aheadContract *sequenceContract,
	disposition sendGroupDisposition,
	queuedInput bool,
) (item *sendItem, effectiveAck bool, groupMemoryBytes ByteCount, err error) {
	groupMemoryBytes = disposition.memoryByteCount()
	effectiveAck = ack || self.sendContract != nil && !self.sendContractAcked &&
		// a headroom debit rides a contract the receiver holds, never the
		// acknowledged lane (FILLFRACTION.md)
		self.noAckHeadroomCharge == nil
	item = takeSendItem()
	*item = sendItem{acks: acks, noBudget: len(frames) == 0 || acks.bypassesBudget(), deliverImmediately: acks.deliversImmediately()}
	prepared := false
	defer func(owner *sendItem) {
		if !prepared {
			// The caller still owns every record, including any overflow.
			owner.acks = sendAckSet{}
			self.returnSendItem(owner)
		}
	}(item)
	if (queuedInput || disposition.target != nil) && self.sendBufferSettings.beforeGroupRetainForTest != nil {
		self.sendBufferSettings.beforeGroupRetainForTest()
	}
	err = item.acks.commitPreparedHandoffs(func() error {
		if err := self.ctx.Err(); err != nil {
			// This optional opening still has no wire ordinal. Losing its
			// source is local retirement, not evidence against the provider.
			if group, ok := disposition.target.(*parsedPacketGroup); ok && group.isSynRetry() && group.synRetry.openingSerial != 0 {
				return newUnwrittenSendPackError(fmt.Errorf("%w: %w", errNativeSynOpeningSuperseded, err))
			}
			return err
		}
		if item.noBudget || !effectiveAck || !self.resendQueue.lifetimeBudget {
			return nil
		}
		frameByteCount := self.retainedSendFrameByteCount(MessageByteCount(frames), len(frames))
		if aheadContract != nil {
			frameByteCount = addReceiveQueueByteCount(frameByteCount, ByteCount(proto.Size(aheadContract.contract)))
		}
		required := addReceiveQueueByteCount(groupMemoryBytes,
			item.retainedMemoryByteCount(frameByteCount, len(frames), self.sendBufferSettings.ProtocolVersion < 2))
		if !item.reservePreparedMemory(self.resendQueue.budget, required, &self.resendMemoryGrant) {
			var credits [sendPackH1GroupMaxFrames]*preparedSendMemory
			if item.acks.preparedMemories(self.resendQueue.budget, &credits) > 0 {
				return errPreparedSendMemoryUnavailable
			}
			return disposition.capacityError(ErrSendPackNotAdmitted)
		}
		return nil
	})
	if err != nil {
		return nil, effectiveAck, groupMemoryBytes, err
	}
	if effectiveAck && !ack && self.client != nil {
		// carried on the acknowledged lane because its contract is not yet
		// acknowledged (FILLFRACTION.md)
		self.client.noAckContractPromotedCount.Add(1)
	}
	prepared = true
	return item, effectiveAck, groupMemoryBytes, nil
}

// Preserves the taking contract for singleton/control callers. Logical groups
// prepare separately so transient pressure can leave their raw cursor intact.
func (self *SendSequence) sendWithSetContractDisposition(
	frames []*protocol.Frame,
	acks sendAckSet,
	noAckSends noAckSendSet,
	ack bool,
	setContract bool,
	forceUnwrapped bool,
	schedulingKey sendSchedulingKey,
	aheadContract *sequenceContract,
	disposition sendGroupDisposition,
) bool {
	ownInput := true
	defer func() {
		if ownInput {
			disposition.complete(false)
			for _, frame := range frames {
				MessagePoolReturn(frame.MessageBytes)
			}
			acks.firstRouteWrite(errSendSequenceUnwound)
			noAckSends.complete(errSendSequenceUnwound)
			acks.invoke(errSendSequenceUnwound)
		}
	}()
	item, effectiveAck, groupMemoryBytes, err := self.prepareSendItem(frames, acks, ack, aheadContract, disposition, false)
	if err != nil {
		ownInput = false
		self.rollbackUnwrittenContractDebit(MessageByteCount(frames))
		disposition.complete(false)
		for _, frame := range frames {
			MessagePoolReturn(frame.MessageBytes)
		}
		acks.firstRouteWrite(err)
		noAckSends.complete(err)
		if noAckSends.count > 0 {
			self.client.sendNoAckDiscardCount.Add(uint64(noAckSends.count))
		}
		acks.invoke(err)
		return false
	}
	ownInput = false
	return self.sendPreparedWithSetContractDisposition(frames, item, noAckSends,
		effectiveAck, setContract, forceUnwrapped, schedulingKey, aheadContract, disposition, groupMemoryBytes)
}
