// Keeps a platform-requested zero final behind counted reports that still
// belong to the client, including the gap after a sequence releases its index.
package connect

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
// after every preceding report succeeds. Callback callers only update bounded
// state and admit an owned worker; they never send on a shared receive path.
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
