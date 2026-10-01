package connect

const noAckContractRetired = uint64(1) << 63

// The reservation CAS, not merely loading a snapshot pointer, admits the
// writer. Retirement is linearized against this same word and never reopens
// a contract when its route snapshot is replaced.
func (self *sequenceContract) acquireNoAckWriter() bool {
	for {
		state := self.noAckWriterState.Load()
		if state&noAckContractRetired != 0 {
			return false
		}
		if self.noAckWriterState.CompareAndSwap(state, state+1) {
			return true
		}
	}
}

func (self *sequenceContract) releaseNoAckWriter() {
	self.noAckWriterState.Add(^uint64(0))
}

func (self *noAckFastPathSnapshot) notifySettled() {
	// Ordinary accounting still rides the next Run pass. Only a retired
	// contract can have a close/join waiting on this settlement, so normal
	// caller writes do not add an extra scheduler wake per packet.
	if self.contract == nil || self.contract.noAckWriterState.Load()&noAckContractRetired == 0 {
		return
	}
	select {
	case self.settled <- struct{}{}:
	default:
	}
}

// Only Run mutates byte counters and finalizes closes. An accepted caller
// retains its lease until this accounting step, not merely until Write returns.
func (self *SendSequence) applyNoAckFastPathWrite(snapshot *noAckFastPathSnapshot, byteCount ByteCount) {
	contract := snapshot.contract
	if contract == nil {
		return
	}
	effective := snapshot.effectiveByteCount(byteCount)
	snapshot.appliedByteCount += effective
	contract.accountWritten(ByteCount(effective))
	contract.releaseNoAckWriter()
	self.tryCloseRetiredSendContract(contract)
}

func (self *SendSequence) retireSendContract(contract *sequenceContract) {
	contract.noAckWriterState.Or(noAckContractRetired)
	self.tryCloseRetiredSendContract(contract)
}

func (self *SendSequence) tryCloseRetiredSendContract(contract *sequenceContract) {
	state := contract.noAckWriterState.Load()
	if state&noAckContractRetired == 0 {
		return
	}
	if state&^noAckContractRetired != 0 {
		if self.pendingNoAckContractCloses == nil {
			self.pendingNoAckContractCloses = make(map[Id]*sequenceContract)
		}
		self.pendingNoAckContractCloses[contract.contractId] = contract
		return
	}
	delete(self.pendingNoAckContractCloses, contract.contractId)
	if contract.unackedByteCount != 0 || self.openSendContracts[contract.contractId] != contract {
		return
	}
	self.client.ContractManager().CloseContract(contract.contractId, contract.ackedByteCount, 0)
	delete(self.openSendContracts, contract.contractId)
}

// Context cancellation stops further ordinary admission before this join.
// Retire every gate first, then drain accepted caller writes/accounting before
// Run emits final contract totals or releases its writer. No caller waits on
// this owner: physical tries are nonblocking, and failures signal settlement.
func (self *SendSequence) joinNoAckContractWriters() {
	for _, contract := range self.openSendContracts {
		self.retireSendContract(contract)
	}
	for {
		self.applyNoAckFastPathAccounting()
		if len(self.pendingNoAckContractCloses) == 0 {
			return
		}
		<-self.noAckFastPathSettled
	}
}
