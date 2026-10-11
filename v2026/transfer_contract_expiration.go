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
func (self *SendSequence) discardAheadContract() {
	ahead := self.aheadSendContract
	if ahead == nil {
		return
	}
	self.aheadSendContract = nil
	self.aheadSendContractAttempted = false
	ahead.rollbackUnwritten(0)
	self.retireSendContract(ahead)
}
