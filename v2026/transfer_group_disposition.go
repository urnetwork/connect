// Exact logical-group ownership crosses admission and reliable retention.
package connect

import "github.com/urnetwork/connect/v2026/protocol"

// Local retained-memory refusal says nothing about provider health. Only
// this exact source boundary creates the marker; contract failures keep their
// existing hard-error classification even if another chunk was refused here.
type sendGroupCapacityError struct {
	cause error
}

// Preserve the existing diagnostics.
func (self *sendGroupCapacityError) Error() string { return self.cause.Error() }

// Preserve errors.Is against the original local admission cause.
func (self *sendGroupCapacityError) Unwrap() error { return self.cause }

// A joined group result is local only when every non-nil cause is local.
// errors.Join omits successful chunks; a real failed retained sibling wins.
func sendGroupCapacityFailure(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := err.(*sendGroupCapacityError); ok {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !sendGroupCapacityFailure(cause) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return sendGroupCapacityFailure(wrapped.Unwrap())
	}
	return false
}

// Only the original descriptor implements admission; no raw Pack is read
// after handoff, because its source may already have returned it to its pool.
type sendGroupAdmissionTarget interface {
	sendGroupAdmitted(source *SendSequence)
	sendGroupDequeued(source *SendSequence)
}

// Range reports refer to original frame indexes, never a chunk cursor guess.
type sendGroupDispositionTarget interface {
	sendGroupDisposition(start, end int, materialized bool)
}

// Lives on the synchronous serialization stack, never in a retained item.
type sendGroupDisposition struct {
	target sendGroupDispositionTarget
	start  int
	end    int
}

// Reports before packet-pool release and before terminal callbacks.
func (self sendGroupDisposition) complete(materialized bool) {
	if self.target != nil && self.start < self.end {
		self.target.sendGroupDisposition(self.start, self.end, materialized)
	}
}

// The concrete descriptor is the only source of the extra lazy charge.
func (self sendGroupDisposition) memoryByteCount() ByteCount {
	if group, ok := self.target.(*parsedPacketGroup); ok {
		return group.sendGroupMemoryByteCount()
	}
	return 0
}

// Preserve existing untracked callers and attach provenance only where this
// exact original group was denied the joint retention reservation.
func (self sendGroupDisposition) capacityError(err error) error {
	if group, ok := self.target.(*parsedPacketGroup); ok && group.completionFlags.Load()&groupCompletionTracked != 0 {
		return &sendGroupCapacityError{cause: err}
	}
	return err
}

// The parent reservation already covers item plus range owner. The split
// stays charged continuously until the group's callback and offer both end.
func (self sendGroupDisposition) retainMemory(item *sendItem, bytes ByteCount) {
	if group, ok := self.target.(*parsedPacketGroup); ok {
		if item.memoryBudget != nil {
			item.queueByteCount -= bytes
		}
		group.retainGroupMemory(item.memoryBudget, bytes)
	}
}

// Only logical groups have the original-member numbering used by the target.
func (self *SendPack) dispositionRange(start, end int) sendGroupDisposition {
	if !self.logicalGroup {
		return sendGroupDisposition{}
	}
	target, _ := self.ackTarget.(sendGroupDispositionTarget)
	return sendGroupDisposition{target: target, start: start, end: end}
}

// The producer captures the target before enqueue; source releaseRaw timing
// remains unchanged even when dequeue and callback beat the public return.
func (self *SendSequence) groupAdmitted(target sendGroupAdmissionTarget) {
	if target != nil {
		if hook := self.sendBufferSettings.beforeGroupAdmissionForTest; hook != nil {
			hook()
		}
		target.sendGroupAdmitted(self)
		if hook := self.sendBufferSettings.afterGroupAdmissionForTest; hook != nil {
			hook(target)
		}
	}
}

// Called exactly at each source receive from its original channel, before a
// scheduler reorder or disposal. No caller-side return ordering is inferred.
func (self *SendSequence) groupDequeued(pack *SendPack) {
	if target, ok := pack.ackTarget.(sendGroupAdmissionTarget); ok {
		if hook := self.sendBufferSettings.beforeGroupDequeueForTest; hook != nil {
			hook(self)
		}
		target.sendGroupDequeued(self)
	}
}

// An already-admitted raw group cannot wait while holding a retained range
// charge needed by its suffix. Dispose only raw members; prefix proof stays.
func (self *SendSequence) unfundedRawGroup(pack *SendPack) bool {
	group, tracked := pack.ackTarget.(*parsedPacketGroup)
	if !tracked || group.completionFlags.Load()&groupCompletionTracked == 0 || !pack.logicalGroup ||
		!self.resendQueue.lifetimeBudget || self.resendQueue.budget == nil {
		return false
	}
	required := self.retainedSendPackByteCountWithContractBytes(pack, self.preparedContractByteCount.Load())
	return required > self.resendQueue.budget.Available()
}

// Preserves ordinary completion accounting and reports whether serialization
// acquired an item. A rejected chunk ends the raw remainder immediately.
func (self *SendSequence) sendGroupRecord(
	disposition sendGroupDisposition,
	frames []*protocol.Frame,
	ack sendAckRecord,
	noAckSend noAckSendRecord,
	ackRequired bool,
	forceUnwrapped bool,
	schedulingKey sendSchedulingKey,
) bool {
	var acks sendAckSet
	acks.add(ack)
	var noAckSends noAckSendSet
	noAckSends.add(noAckSend)
	if !ackRequired && self.client != nil {
		self.client.sendNoAckWriteCount.Add(1)
	}
	return self.sendWithSetContractDisposition(frames, acks, noAckSends, ackRequired,
		false, forceUnwrapped, schedulingKey, nil, disposition)
}
