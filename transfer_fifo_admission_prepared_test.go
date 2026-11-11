// Prepared DATA keeps its paid credit and cancellation capsule while a fixed
// recovery cohort closes ordinary serialization, independently of protocol.
package connect

import "testing"

func TestFifoRepairAdmissionPreparedQueuedDeadline(t *testing.T) {
	testFifoRepairAdmissionPublic(t, "prepared_deadline")
}

func TestFifoRepairAdmissionPreparedQueuedCancellation(t *testing.T) {
	testFifoRepairAdmissionPublic(t, "prepared_cancel")
}

// Owns one bounded pre-read reservation until the actual terminal callback.
type fifoAdmissionPreparedOwner struct {
	credit   *preparedSendMemory
	handoff  preparedSendHandoff
	terminal AckFunction
}

func (self *fifoAdmissionPreparedOwner) preparedSendMemory() *preparedSendMemory {
	return self.credit
}

func (self *fifoAdmissionPreparedOwner) preparedHandoff() *preparedSendHandoff {
	return &self.handoff
}

func (self *fifoAdmissionPreparedOwner) sendAckResult(_ ByteCount, err error) {
	self.credit.release()
	self.terminal(err)
}

// Root locking copies ownership facts without borrowing a mutable credit.
func (self *fifoAdmissionPreparedOwner) snapshot() (ByteCount, bool, bool) {
	root := self.credit.budget.admissionRoot()
	root.admissionLock.Lock()
	defer root.admissionLock.Unlock()
	return self.credit.bytes, self.credit.flightMoved, self.credit.closed
}
