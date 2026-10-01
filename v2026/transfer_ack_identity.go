package connect

// One send worker may temporarily remove one retained item while rewriting
// its frame or recovery order. Keep only its immutable ACK identity under the
// queue lock: a concurrent ACK must not confuse this interval with retirement.
// There is no extra map, payload reference or allocation per retained message.
type sendAckIdentity struct {
	messageId      Id
	sequenceNumber uint64
	active         bool
}

func (self *SendSequence) detachResendItem(messageId Id) *sendItem {
	queue := self.resendQueue
	queue.stateLock.Lock()
	defer queue.stateLock.Unlock()
	item := queue.messageIdItems[messageId]
	if item == nil {
		return nil
	}
	if self.detachedAck.active {
		panic("another retained send item is already detached")
	}
	self.detachedAck = sendAckIdentity{
		messageId: item.messageId, sequenceNumber: item.sequenceNumber, active: true,
	}
	return queue.remove(item)
}

// Validation remains exact and sequence-scoped. Retired, unknown and other
// messages cannot borrow the one identity whose retained owner is rewriting.
func (self *SendSequence) retainedAckSequenceNumber(messageId Id) (uint64, bool) {
	queue := self.resendQueue
	queue.stateLock.Lock()
	defer queue.stateLock.Unlock()
	if item := queue.messageIdItems[messageId]; item != nil {
		return item.sequenceNumber, true
	}
	if self.detachedAck.active && self.detachedAck.messageId == messageId {
		return self.detachedAck.sequenceNumber, true
	}
	return 0, false
}
