package connect

import "fmt"

// This failure is emitted only before serialization/sequence publication.
// A prepared upstream owner can safely retry these exact bytes; a generic
// admission error after an attempted wire write is not that proof.
var errPreparedSendMemoryUnavailable = fmt.Errorf("prepared flight memory unavailable before serialization: %w", ErrSendPackNotAdmitted)

// A prepared send pays for its reliable owner before an irreversible upstream
// read. The ACK target carries this credit through queueing and coalescing;
// serialization moves it into the existing sendItem reservation. No release /
// reacquire gap or second charge is allowed at a full parent.
type preparedSendMemory struct {
	budget *TransferMemoryBudget
	// Protected by budget.admissionRoot().admissionLock. The budget is immutable.
	bytes ByteCount
	// A detached ACK capsule cannot be paid by a NAT flow that retires at
	// first write. Its fixed metadata stays in this reservation until ACK.
	retained    ByteCount
	required    ByteCount
	flightMoved bool
	closed      bool
}

func (self *preparedSendMemory) availableLocked() ByteCount {
	return max(0, self.bytes-self.retained)
}

func (self *preparedSendMemory) require(bytes ByteCount) {
	if self == nil {
		return
	}
	root := self.budget.admissionRoot()
	root.admissionLock.Lock()
	if !self.closed && !self.flightMoved {
		self.required = max(self.required, self.retained+bytes)
	}
	root.admissionLock.Unlock()
}

func (self *preparedSendMemory) growRequired() bool {
	if self == nil {
		return true
	}
	root := self.budget.admissionRoot()
	root.admissionLock.Lock()
	if self.closed || self.flightMoved {
		open := !self.closed
		root.admissionLock.Unlock()
		return open
	}
	additional := max(0, self.required-self.bytes)
	if additional > 0 && !self.budget.tryReserveWithLock(additional) {
		root.admissionLock.Unlock()
		return false
	}
	self.bytes += additional
	root.admissionLock.Unlock()
	if additional > 0 {
		// A queued prepared Pack can now fit even though ordinary Available
		// decreased. Wake its sequence's existing memory-capacity select.
		self.budget.notifyCapacityChanged()
	}
	return true
}

// Carves packet-specific ownership from one worst-case pre-read admission.
// It never releases/reacquires at the full parent or duplicates the claim.
func (self *preparedSendMemory) split(bytes, retained ByteCount) (*preparedSendMemory, bool) {
	if self == nil {
		return nil, true
	}
	root := self.budget.admissionRoot()
	root.admissionLock.Lock()
	defer root.admissionLock.Unlock()
	if retained < 0 || bytes < retained || bytes > self.availableLocked() {
		return nil, false
	}
	self.bytes -= bytes
	return &preparedSendMemory{budget: self.budget, bytes: bytes, retained: retained}, true
}

func (self *preparedSendMemory) releaseUnused() {
	if self == nil {
		return
	}
	root := self.budget.admissionRoot()
	root.admissionLock.Lock()
	bytes := self.availableLocked()
	self.bytes -= bytes
	root.admissionLock.Unlock()
	if bytes > 0 {
		self.budget.Release(bytes)
	}
}

func (self *preparedSendMemory) trim(bytes ByteCount) bool {
	if self == nil {
		return true
	}
	root := self.budget.admissionRoot()
	root.admissionLock.Lock()
	if self.closed || bytes < self.retained || self.bytes < bytes {
		root.admissionLock.Unlock()
		return false
	}
	excess := self.bytes - bytes
	self.bytes = bytes
	root.admissionLock.Unlock()
	if excess > 0 {
		self.budget.Release(excess)
	}
	return true
}

func (self *preparedSendMemory) releaseUnusedAfterTransfer() {
	if self == nil {
		return
	}
	root := self.budget.admissionRoot()
	root.admissionLock.Lock()
	bytes := ByteCount(0)
	if self.flightMoved {
		bytes = self.availableLocked()
		self.bytes -= bytes
	}
	root.admissionLock.Unlock()
	if bytes > 0 {
		self.budget.Release(bytes)
	}
}

type preparedSendMemoryTarget interface {
	preparedSendMemory() *preparedSendMemory
}

func prepareSendMemory(budget *TransferMemoryBudget, bytes ByteCount) (*preparedSendMemory, bool) {
	if budget == nil {
		return nil, true
	}
	if bytes <= 0 || !budget.TryReserve(bytes) {
		return nil, false
	}
	return &preparedSendMemory{budget: budget, bytes: bytes}, true
}

func (self *preparedSendMemory) release() {
	if self == nil {
		return
	}
	root := self.budget.admissionRoot()
	root.admissionLock.Lock()
	bytes := self.bytes
	self.bytes = 0
	self.closed = true
	root.admissionLock.Unlock()
	if bytes > 0 {
		self.budget.Release(bytes)
	}
}

func (self sendAckRecord) preparedMemory() *preparedSendMemory {
	if self.group != nil {
		return self.group.ack.preparedMemory()
	}
	if owner, ok := self.target.(preparedSendMemoryTarget); ok {
		return owner.preparedSendMemory()
	}
	return nil
}

// The caller supplies stack storage. A logical group can repeat one credit
// across chunks, and must never count that same claim twice in one wire item.
func (self *sendAckSet) preparedMemories(budget *TransferMemoryBudget, credits *[sendPackH1GroupMaxFrames]*preparedSendMemory) int {
	count := 0
	for index := 0; index < int(self.count); index++ {
		var record sendAckRecord
		if index < len(self.records) {
			record = self.records[index]
		} else {
			record = self.overflow.records[index-len(self.records)]
		}
		credit := record.preparedMemory()
		if credit == nil || credit.budget != budget {
			continue
		}
		duplicate := false
		for _, previous := range credits[:count] {
			duplicate = duplicate || previous == credit
		}
		if !duplicate {
			credits[count] = credit
			count++
		}
	}
	return count
}

func (self *SendPack) preparedMemoryBytes(budget *TransferMemoryBudget) ByteCount {
	credit := self.ackRecord().preparedMemory()
	if credit == nil || credit.budget != budget {
		return 0
	}
	root := budget.admissionRoot()
	root.admissionLock.Lock()
	bytes := credit.availableLocked()
	root.admissionLock.Unlock()
	return bytes
}

// Serialization is the sole consumer of this ACK set. Other sequences may
// reserve or release from the same parent, so the entire move (including any
// necessary growth) shares the budget's exact admission linearization point.
func (self *sendItem) reservePreparedMemory(budget *TransferMemoryBudget, bytes ByteCount) bool {
	if budget == nil {
		return true
	}
	if self.memoryBudget != nil {
		panic("transfer owner already has a memory reservation")
	}
	if bytes < 0 {
		return false
	}
	var credits [sendPackH1GroupMaxFrames]*preparedSendMemory
	count := self.acks.preparedMemories(budget, &credits)
	if count == 0 {
		return self.reserveMemory(budget, bytes)
	}
	root := budget.admissionRoot()
	root.admissionLock.Lock()
	defer root.admissionLock.Unlock()
	available := ByteCount(0)
	for _, credit := range credits[:count] {
		available = addReceiveQueueByteCount(available, credit.availableLocked())
	}
	growth := max(0, bytes-available)
	if growth > 0 && !budget.tryReserveWithLock(growth) {
		credits[0].required = max(credits[0].required, credits[0].bytes+growth)
		return false
	}
	remaining := bytes - growth
	for _, credit := range credits[:count] {
		moved := min(remaining, credit.availableLocked())
		credit.bytes -= moved
		remaining -= moved
		credit.flightMoved = true
	}
	self.memoryBudget = budget
	self.queueByteCount = bytes
	return true
}

// Prepared ownership changes only memory admission. It does not waive the
// per-sequence message window or the carrier's reliable-flight policy.
func (self *SendSequence) preparedPackFits(pack *SendPack) bool {
	credit := pack.ackRecord().preparedMemory()
	if credit == nil || self.resendQueue == nil || !self.resendQueue.lifetimeBudget ||
		credit.budget != self.resendQueue.budget {
		return false
	}
	required := self.retainedSendPackByteCountWithContractBytes(pack, self.preparedContractByteCount.Load())
	credit.require(required)
	return pack.preparedMemoryBytes(self.resendQueue.budget) >= required
}

func (self *transferQueue[T]) preparedWindowAvailable(maxByteCount ByteCount) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.byteCount < maxByteCount
}
