// Signed deadlines bound new admission. Acknowledgements and settlement keep
// their original contract attribution after the deadline.
package connect

import (
	"errors"
	"time"
)

var errContractExpired = errors.New("contract expired")

// Deadline equality rejects admission, independent of remaining byte capacity.
// Keep explicit presence: even the Unix value of Go's zero time is a deadline.
func (self *sequenceContract) expired() bool {
	return self.expirationTimeUnixMilli != nil && *self.expirationTimeUnixMilli <= time.Now().UnixMilli()
}

// When a contract this sequence took at `setTime` stops taking new messages:
// ContractExpirationMargin ahead of its signed deadline, capped at a quarter
// of the life left at `setTime` (zero `setTime` leaves the margin whole). False
// for a contract with no deadline, which never retires by time.
func (self *SendSequence) contractRetireTime(contract *sequenceContract, setTime time.Time) (time.Time, bool) {
	if contract == nil || contract.expirationTimeUnixMilli == nil {
		return time.Time{}, false
	}
	deadline := time.UnixMilli(*contract.expirationTimeUnixMilli)
	margin := self.sendBufferSettings.ContractExpirationMargin
	if !setTime.IsZero() {
		margin = min(margin, deadline.Sub(setTime)/4)
	}
	return deadline.Add(-max(0, margin)), true
}

// The current send contract is within its margin of its signed deadline
// (ContractExpirationMargin): it takes no new message, while the messages it
// already carries can still be acknowledged before the receiver stops
// admitting it.
func (self *SendSequence) sendContractRetiring(now time.Time) bool {
	retireTime, ok := self.contractRetireTime(self.sendContract, self.sendContractSetTime)
	return ok && !now.Before(retireTime)
}

// Whether the current send contract can carry `byteCount` more, by the rule
// the debit applies: it fits, and it is not retiring. The coalescing and
// envelope checks must agree with the debit, or a batch formed for the current
// contract lands on its successor with a contract frame it was not sized for.
func (self *SendSequence) sendContractCanUpdate(byteCount ByteCount) bool {
	return !self.sendContractRetiring(time.Now()) && self.sendContract.canUpdate(byteCount)
}

// An announcement's wire debit belongs to the current contract. The unused
// successor owns only an opening reservation, which has no pending data ack.
// The current contract is left with no successor, so it may request another.
func (self *SendSequence) discardAheadContract() {
	ahead := self.aheadSendContract
	if ahead == nil {
		return
	}
	self.aheadSendContract = nil
	self.aheadSendContractAttempted = false
	self.successorContractRequested = false
	ahead.openingReserved = false
	ahead.rollbackUnwritten(0)
	self.retireSendContract(ahead)
}
