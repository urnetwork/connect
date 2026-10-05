package connect

import (
	"context"
	"errors"
	"slices"
	"time"
)

const (
	sendAckTimeoutOrdinary uint32 = iota
	sendAckTimeoutRetained
	sendAckTimeoutClosing
)

// A writer yields without canceling or releasing borrowed item/frame pointers.
// Only the stable send-owner boundary may perform item-local retirement.
var errSendAckLifetime = errors.New("send owner must retire expired ACK item")

// Register before handing a retained Pack to the channel, not when it reaches
// the resend queue. The sticky generation state avoids undoing another
// concurrent admission if this caller later times out at the channel. Such a
// refused handoff may conservatively select item-local expiry until idle close;
// it creates no extra memory ownership and cannot extend an item's lifetime.
func (self *SendSequence) protectRetainedAdmission() bool {
	return self.ackTimeoutDisposition.CompareAndSwap(sendAckTimeoutOrdinary, sendAckTimeoutRetained) ||
		self.ackTimeoutDisposition.Load() == sendAckTimeoutRetained
}

// Arbitration prevents a retained Pack being accepted between the timeout's
// last ownership check and ordinary sequence cancellation. A losing retained
// caller keeps its input and retries admission through the normal SendBuffer.
func (self *SendSequence) ackLifetimeDisposition(item *sendItem, deadline time.Time) error {
	if !self.ackTimeoutDisposition.CompareAndSwap(sendAckTimeoutOrdinary, sendAckTimeoutClosing) &&
		self.ackTimeoutDisposition.Load() == sendAckTimeoutRetained {
		return errSendAckLifetime
	}
	self.recordSendSequenceExit("ack_lifetime", item, deadline, context.DeadlineExceeded)
	return context.DeadlineExceeded
}

// Called only between writes, before applying a pending ACK snapshot. The
// index reconciles delivery and arrival-anchored renewals before each expiry.
func (self *SendSequence) retireAckLifetimes(now time.Time) error {
	for {
		_, err := self.nextAckLifetime(now)
		if err != errSendAckLifetime {
			return err
		}
		self.expireSendItem(self.ackLifetimes.items[0].item, now)
	}
}

func (self *SendSequence) expireSendItem(item *sendItem, now time.Time) {
	index := slices.Index(self.sendItems, item)
	if index < 0 || item.acks.retainPastAckTimeout() {
		panic("ACK expiry without discardable send ownership")
	}
	// Removal linearizes against ACK ingress before per-item debt is inspected.
	// A publisher that already claimed real delivery may finish its credit;
	// no later ACK can find or credit the expired identity.
	if self.resendQueue.RemoveByMessageId(item.messageId) != item {
		panic("ACK expiry without resend ownership")
	}
	self.ackLifetimes.remove(item)
	self.forgetUnreliableFlight(item)
	self.abandonPacingService(item)
	self.sendItems = slices.Delete(self.sendItems, index, index+1)
	if item.contractId != nil {
		if contract := self.openSendContracts[*item.contractId]; contract != nil {
			contract.abandonedByteCount += max(contract.minUpdateByteCount, item.messageByteCount)
			if contract != self.sendContract {
				self.retireSendContract(contract)
			}
		}
	}
	item.acks.invoke(context.DeadlineExceeded)
	item.messagePoolReturn()
	if index == 0 {
		self.scheduleAckTimeoutHead(now)
	}
}

// Head means every earlier position is retired, not merely that this item is
// due. An interior gap cannot promote across an older retained owner. If the
// new oldest was SACK-held, a prompt duplicate Head releases that same receiver
// identity from its hold instead of waiting for its selective-ACK lease.
func (self *SendSequence) scheduleAckTimeoutHead(now time.Time) {
	if len(self.sendItems) == 0 {
		return
	}
	item := self.sendItems[0]
	if self.detachResendItem(item.messageId) != item {
		panic("ACK timeout head without resend ownership")
	}
	item.selectiveAcked = false
	item.resendTime = now
	item.recoveryKind = sendRecoveryCumulativeProbe
	self.addResendItem(item)
}

// Expiry releases ownership, never delivery credit or a physical drain proof.
// Queue removal has already excluded new ingress credit. A prior publisher's
// claimed credit must remain charged until its real ACK publication finishes.
func (self *SendSequence) abandonPacingService(item *sendItem) {
	pacer := &self.windowPacer
	service := pacer.service
	if service == nil {
		return
	}
	service.invalidateMessageProbe(self.sequenceId, item.messageId)
	service.stateLock.Lock()
	defer service.stateLock.Unlock()
	if !item.serviceCreditObserved && item.pacingByteCount > 0 {
		pacer.serviceSent -= item.pacingByteCount
		service.sent -= item.pacingByteCount
		service.drainedSent = 0
	}
	if write, ok := service.writes[self.sequenceId]; ok && write.messageId == item.messageId {
		if write.pending {
			service.pendingWrites--
			service.drainGeneration++
			service.unprovableWriteCount = service.pendingWrites
			service.abortDrainWithLock()
			service.drained = false
			service.sourceIdleAt = time.Time{}
		}
		delete(service.writes, self.sequenceId)
	}
	service.notifyDrainWithLock()
}
