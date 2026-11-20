// Keeps a platform-requested zero final behind counted reports that still
// belong to the client, including the gap after a sequence releases its index,
// and answers a request for a contract the client holds no record of without
// a count.
package connect

import (
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// The manager lock protects this state. Its lifetime is bounded by existing
// outstanding reports; repeated close requests add only one boolean obligation.
// It retains no serialized frames or callbacks waiting to be sent.
type contractCloseReports struct {
	contractId   Id
	pendingCount int
	final        bool
	requested    bool
	failed       bool
}

// Contracts whose checkpoints were reported and acknowledged, so a close
// request that arrives after their reports retire still finalizes them on
// their counts. The manager lock protects it. A ring bounds it to
// `CloseRequestCheckpointRecordCount`: past that the oldest is forgotten. A
// later final leaves its record in place; a request then answers with a zero
// final that the platform refuses as already closed.
type contractCheckpointRecords struct {
	recordTimes map[Id]time.Time
	// insertion order, full once it holds the bound
	contractIds []Id
	// the oldest entry of a full ring
	nextIndex int
}

// Records a checkpointed contract, or renews its time, while the caller holds
// the manager lock.
func (self *contractCheckpointRecords) addWithLock(contractId Id, recordTime time.Time, maxCount int) {
	if maxCount <= 0 {
		return
	}
	if self.recordTimes == nil {
		self.recordTimes = map[Id]time.Time{}
	}
	if _, ok := self.recordTimes[contractId]; ok {
		self.recordTimes[contractId] = recordTime
		return
	}
	if len(self.contractIds) < maxCount {
		self.contractIds = append(self.contractIds, contractId)
	} else {
		delete(self.recordTimes, self.contractIds[self.nextIndex])
		self.contractIds[self.nextIndex] = contractId
		self.nextIndex = (self.nextIndex + 1) % len(self.contractIds)
	}
	self.recordTimes[contractId] = recordTime
}

// Whether a checkpoint of the contract was acknowledged within the timeout,
// while the caller holds the manager lock.
func (self *contractCheckpointRecords) hasWithLock(contractId Id, now time.Time, timeout time.Duration) bool {
	recordTime, ok := self.recordTimes[contractId]
	return ok && now.Sub(recordTime) < timeout
}

// Registers report ownership before removing a send contract's open entry.
func (self *ContractManager) newContractCloseReport(contractId Id, checkpoint bool) *contractCloseReports {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.newContractCloseReportWithLock(contractId, checkpoint)
}

// Adds a report to the existing ownership interval while the caller holds mutex.
func (self *ContractManager) newContractCloseReportWithLock(contractId Id, checkpoint bool) *contractCloseReports {
	pending := self.pendingCloseReportKVs[contractId]
	if pending == nil {
		pending = &contractCloseReports{contractId: contractId}
		if self.pendingCloseReportKVs == nil {
			self.pendingCloseReportKVs = map[Id]*contractCloseReports{}
		}
		self.pendingCloseReportKVs[contractId] = pending
	}
	pending.pendingCount += 1
	pending.final = pending.final || !checkpoint
	return pending
}

// Retires existing report ownership and admits a coalesced zero final only
// after every preceding report succeeds. An interval of checkpoints that all
// succeeded is remembered for later close requests. Callback callers only
// update bounded state and admit an owned worker; they never send on a shared
// receive path.
func (self *ContractManager) completeContractCloseReport(pending *contractCloseReports, succeeded bool) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	pending.pendingCount -= 1
	pending.failed = pending.failed || !succeeded
	if pending.pendingCount != 0 {
		return
	}
	if pending.requested && !pending.final && !pending.failed &&
		!self.closed && self.ctx.Err() == nil && !self.client.IsDone() {
		// Reserve the final before admission so concurrent requests coalesce.
		// This worker owns construction and initial handoff, including a close
		// racing admission. Shutdown may drop the synthetic zero obligation;
		// counted reports retain their existing cleanup and expiry backstop.
		pending.pendingCount = 1
		pending.final = true
		pending.requested = false
		if self.startWorker("contract close request", func() {
			self.closeContractWithPendingReport(pending.contractId, 0, 0, false, pending)
		}) {
			return
		}
	}
	if !pending.final && !pending.failed {
		// the platform holds these counts as the client's checkpoint
		self.checkpointRecords.addWithLock(pending.contractId, time.Now(), self.settings.CloseRequestCheckpointRecordCount)
	}
	if self.pendingCloseReportKVs[pending.contractId] == pending {
		delete(self.pendingCloseReportKVs, pending.contractId)
	}
}

// Requests one best-effort zero final without overtaking counted work. Existing
// finals already satisfy the request; failed cleanup or shutdown drops only
// the synthetic obligation and leaves platform expiry as the backstop.
func (self *ContractManager) requestContractClose(contractId Id) {
	self.mutex.Lock()
	if self.closed || self.ctx.Err() != nil || self.client.IsDone() {
		self.mutex.Unlock()
		return
	}
	if pending := self.pendingCloseReportKVs[contractId]; pending != nil {
		if !pending.final {
			pending.requested = true
		}
		self.mutex.Unlock()
		return
	}
	pending := self.newContractCloseReportWithLock(contractId, false)
	self.mutex.Unlock()
	self.closeContractWithPendingReport(contractId, 0, 0, false, pending)
}

// Answers the platform's close request (CloseSequences) for contracts that no
// live sequence, open entry or queue holds. A contract with reports still
// pending, or a remembered checkpoint, closes with a zero final behind its
// counts, as requestContractClose does. Any other contract has no record
// here: whatever counts it had were lost with the state that held them, for
// example to a hard kill or a crash before its first checkpoint. It is
// answered with ContractsNoRecord, never with a count. A zero final would
// claim the client counted nothing, and the platform would settle at the mean
// of the two counts or dispute them; no record leaves the contract to the
// platform's expiry sweep, as for a client that never answers.
func (self *ContractManager) closeUnheldContracts(contractIds []Id) {
	recordedContractIds := []Id{}
	noRecordContractIds := []Id{}
	func() {
		self.mutex.Lock()
		defer self.mutex.Unlock()
		if self.closed || self.ctx.Err() != nil || self.client.IsDone() {
			return
		}
		now := time.Now()
		for _, contractId := range contractIds {
			if self.pendingCloseReportKVs[contractId] != nil ||
				self.checkpointRecords.hasWithLock(contractId, now, self.settings.CloseRequestCheckpointRecordTimeout) {
				recordedContractIds = append(recordedContractIds, contractId)
			} else {
				noRecordContractIds = append(noRecordContractIds, contractId)
			}
		}
	}()
	for _, contractId := range recordedContractIds {
		self.requestContractClose(contractId)
	}
	self.sendContractsNoRecord(noRecordContractIds)
}

// Sends the no-record answers to the control id, at most
// `ContractsNoRecordMaxCount` ids a frame. Runs on the close worker, which
// waits up to `ContractsNoRecordSendTimeout` for each frame's admission. A
// refused frame is dropped: the answer changes nothing at the platform, which
// treats a client that never answers the same way.
func (self *ContractManager) sendContractsNoRecord(contractIds []Id) {
	maxCount := max(1, self.settings.ContractsNoRecordMaxCount)
	for 0 < len(contractIds) {
		count := min(len(contractIds), maxCount)
		message := &protocol.ContractsNoRecord{
			ContractIds: make([][]byte, 0, count),
		}
		for _, contractId := range contractIds[:count] {
			message.ContractIds = append(message.ContractIds, contractId.Bytes())
		}
		contractIds = contractIds[count:]
		if self.beforeContractsNoRecordSendForTest != nil {
			self.beforeContractsNoRecordSendForTest(message)
		}
		frame, err := ToFrame(message, self.settings.ProtocolVersion)
		if err != nil {
			self.client.log.Infof("[contract]could not create no record frame = %s\n", err)
			return
		}
		if !self.client.SendControlWithTimeout(frame, nil, max(0, self.settings.ContractsNoRecordSendTimeout)) {
			MessagePoolReturn(frame.MessageBytes)
			if self.client.log.V(1).Enabled() {
				self.client.log.Infof("[contract]drop no record of %d contracts: the control send was refused\n", count)
			}
			continue
		}
		if self.client.log.V(1).Enabled() {
			self.client.log.Infof("[contract]no record of %d contracts the platform asked to close\n", count)
		}
	}
}
