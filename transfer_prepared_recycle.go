// Recyclable send credit keeps an exact pre-admitted envelope across successive
// physical flights. It neither lends the permission on ACK nor grows it later.
package connect

// Reserves the compact original/capsule plus a fixed serializer envelope before
// ownership is accepted. Nil or disabled budgets never mean unbounded recovery.
func prepareRecyclableSendMemory(budget *TransferMemoryBudget, bytes, retained ByteCount) (*preparedSendMemory, bool) {
	if budget == nil || bytes <= 0 || retained < 0 || bytes <= retained || !budget.TryReserve(bytes) {
		return nil, false
	}
	return &preparedSendMemory{budget: budget, bytes: bytes, retained: retained, recyclable: true}, true
}

// Logical groups already carry their owner through one immutable ACK record;
// finding it there avoids adding a pointer to every retained sendItem.
func (self *sendItem) recyclablePreparedMemory() *preparedSendMemory {
	if self.acks.count != 1 || self.memoryBudget == nil {
		return nil
	}
	credit := self.acks.records[0].preparedMemory()
	if credit == nil || !credit.recyclable || credit.budget != self.memoryBudget {
		return nil
	}
	return credit
}

// Returns one released physical claim after its wire root has been returned.
// Cancellation keeps the retained capsule charged until the final flight joins.
func (self *preparedSendMemory) recycleFlight(bytes ByteCount) {
	root := self.budget.admissionRoot()
	root.admissionLock.Lock()
	if !self.recyclable || bytes < 0 || self.inFlight < bytes {
		root.admissionLock.Unlock()
		panic("invalid recyclable send credit return")
	}
	self.inFlight -= bytes
	release := ByteCount(0)
	if self.closed {
		release = bytes
		if self.inFlight == 0 {
			release = addReceiveQueueByteCount(release, self.bytes)
			self.bytes = 0
		}
	} else {
		self.bytes += bytes
	}
	root.admissionLock.Unlock()
	if release > 0 {
		self.budget.Release(release)
	} else {
		// Ordinary Available is unchanged, but an already-paid pending chunk
		// can now materialize through its existing capacity notification.
		self.budget.notifyCapacityChanged()
	}
}

// Returns exclusive item credit to its recovery owner instead of lending it
// through the shared root. Ordinary single-use owners keep their exact release.
func (self *sendItem) releaseMemory() {
	if credit := self.recyclablePreparedMemory(); credit != nil {
		if self.transferFrameBytes != nil {
			panic("recycling send credit before returning its wire root")
		}
		bytes := self.queueByteCount
		self.memoryBudget = nil
		credit.recycleFlight(bytes)
		return
	}
	self.transferItem.releaseMemory()
}
