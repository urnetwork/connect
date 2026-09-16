// Receiver timing uses one monotonic clock per client. Wire delays contain no
// clock epoch and apply only to the Pack named by the acknowledgement.
package connect

import (
	"math"
	"time"
)

// A compact client-relative timestamp preserves zero as unavailable, even
// when ingress happens at the exact client clock origin.
func (self *Client) feedbackTimeNanos(at time.Time) int64 {
	if self == nil || self.feedbackTimeBase.IsZero() {
		return 0
	}
	elapsed := at.Sub(self.feedbackTimeBase)
	if elapsed < 0 || elapsed == time.Duration(math.MaxInt64) {
		return 0
	}
	return int64(elapsed) + 1
}

// Flooring leaves sub-microsecond receiver time in the measured RTT. Invalid
// or unrepresentable durations stay absent rather than inventing a delay.
func (self *Client) receiverAckDelayMicros(receivedAtNanos int64, at time.Time) (uint32, bool) {
	if receivedAtNanos <= 0 || self == nil || self.feedbackTimeBase.IsZero() {
		return 0, false
	}
	elapsed := at.Sub(self.feedbackTimeBase)
	arrival := time.Duration(receivedAtNanos - 1)
	if elapsed < arrival {
		return 0, false
	}
	micros := (elapsed - arrival).Microseconds()
	if math.MaxUint32 < micros {
		return 0, false
	}
	return uint32(micros), true
}
