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
	ahead.rollbackUnwritten(0)
	self.retireSendContract(ahead)
}

// How many successors of one contract may be closed in the queue unused
// before the contract stops requesting one ahead of its end.
const successorContractLostLimit = 2

// The queued counterpart of `discardAheadContract`. A requested successor that
// is neither queued nor still being requested was closed in the queue unused:
// it waited there past `ContractQueueExpireTimeout`, because the sequence went
// idle after the request and before the contract ran low enough to announce
// it. A sequence outlives that wait (`IdleTimeout`), so when it resumes, the
// current contract is left with no successor and may request another, and
// announce that one. Without this the resumed sequence reaches the end of its
// contract with nothing requested.
//
// The queue is consulted only once a successor could have expired in it:
// first a queue expiry after the request, then, while the successor is still
// there or on its way, every eighth of one. A sequence that only makes
// no-acknowledgement writes has no pass of its owner to look in, and learns of
// the loss when the contract refuses a write; the exhaustion path then asks
// at once.
//
// The successor requested next is one more that may go unused, so the losses
// are counted: after `successorContractLostLimit` the contract requests none
// ahead of its end, and a sequence that keeps going quiet short of it costs
// that many unused successors, not one for every queue expiry. Reports
// whether a successor was forgotten.
func (self *SendSequence) forgetLostSuccessorContract() bool {
	if !self.successorContractRequested || self.successorContractEager {
		return false
	}
	now := self.successorContractNow()
	if self.successorContractLostCheckTime.IsZero() || now.Before(self.successorContractLostCheckTime) {
		return false
	}
	contractManager := self.client.ContractManager()
	key := self.contractMetadata().key
	if contractManager.hasPendingCreate(key) || contractManager.hasQueuedContract(key) {
		self.successorContractLostCheckTime = self.successorContractLostCheckAfter(
			contractManager.settings.ContractQueueExpireTimeout / 8,
		)
		return false
	}
	self.successorContractRequested = false
	self.aheadSendContractAttempted = false
	self.successorContractLostCount++
	return true
}

// The time of the next look, or zero where the queue expires nothing by age.
func (self *SendSequence) successorContractLostCheckAfter(wait time.Duration) time.Time {
	if self.client.ContractManager().settings.ContractQueueExpireTimeout <= 0 {
		return time.Time{}
	}
	return self.successorContractNow().Add(max(wait, time.Nanosecond))
}
