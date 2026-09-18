// Healthy cumulative feedback need not repeatedly inspect a retained flight
// for recovery evidence that this sequence has never received.
package connect

import "time"

// The worker calls this only after applying an acknowledgement snapshot.
// Keep the isolated scoreboard available to tests that construct evidence.
func (self *SendSequence) scheduleSelectiveAckRecoveryAfterFeedback(at time.Time) bool {
	// Cumulative progress alone cannot create a selective gap or a missing
	// cumulative reply. Avoid two full-flight walks on every healthy reply.
	// Keep lifetime history: eviction, retry and route changes may clear item
	// marks, and active recovery still needs its later evidence-free tail probe.
	if !self.selectiveAckObserved && !self.selectiveGapRecoveryActive {
		return false
	}
	return self.scheduleSelectiveAckRecovery(at)
}
