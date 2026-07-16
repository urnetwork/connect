package connect

import (
	"sync/atomic"
	"time"
)

// contract stats surface per-contract usage from the send and receive sequences
// without per-packet dispatch. open contracts register an entry whose used byte
// count the owning sequence updates with an atomic store on debit/ack. an epoch
// worker (started on the first callback) snapshots the registry every
// `ContractStatsEpoch` and emits events only for contracts that changed.

type ContractStatsEvent struct {
	ContractId Id
	// true for a receive (ingress) contract, false for a send (egress) contract
	Receive bool
	// true when the send sequence opened this as a companion contract.
	// always false on the receive side (the wire contract does not carry it).
	// pair contracts to their companions with the peer client id on `Path`
	Companion bool
	// source -> destination of the contract
	Path              TransferPath
	TransferByteCount ByteCount
	UsedByteCount     ByteCount
	// used bytes since the previous event for this contract
	UsedByteCountDelta ByteCount
	// false when the contract closed. the final event for a contract
	Open bool
}

type ContractStatsFunction func(contractStatsEvents []*ContractStatsEvent)

type contractStatsKey struct {
	contractId Id
	receive    bool
}

type contractStatsEntry struct {
	contractId        Id
	receive           bool
	companion         bool
	path              TransferPath
	transferByteCount ByteCount

	// stored by the owning sequence on debit/ack
	usedByteCount atomic.Int64
	closed        atomic.Bool

	// owned by the epoch worker
	emittedUsedByteCount ByteCount
	emitted              bool
}

func (self *contractStatsEntry) updateUsedByteCount(usedByteCount ByteCount) {
	self.usedByteCount.Store(int64(usedByteCount))
}

// register adds an entry for an open contract and returns it.
// the owning sequence stores the used byte count on the entry
func (self *ContractManager) registerContractStats(
	contractId Id,
	receive bool,
	companion bool,
	path TransferPath,
	transferByteCount ByteCount,
) *contractStatsEntry {
	entry := &contractStatsEntry{
		contractId:        contractId,
		receive:           receive,
		companion:         companion,
		path:              path,
		transferByteCount: transferByteCount,
	}

	self.contractStatsLock.Lock()
	defer self.contractStatsLock.Unlock()
	self.contractStatsEntries[contractStatsKey{contractId: contractId, receive: receive}] = entry
	return entry
}

// called from the contract close paths (`CloseContract`, `CheckpointContract`).
// the entry emits a final closed event and is removed by the epoch worker,
// or removed immediately when the worker never started
func (self *ContractManager) closeContractStats(contractId Id) {
	self.contractStatsLock.Lock()
	defer self.contractStatsLock.Unlock()
	for _, receive := range []bool{false, true} {
		key := contractStatsKey{contractId: contractId, receive: receive}
		if entry, ok := self.contractStatsEntries[key]; ok {
			if self.contractStatsStarted {
				entry.closed.Store(true)
			} else {
				delete(self.contractStatsEntries, key)
			}
		}
	}
}

// AddContractStatsCallback registers a listener for the epoch contract stats
// events. the epoch worker starts on the first callback
func (self *ContractManager) AddContractStatsCallback(contractStatsCallback ContractStatsFunction) func() {
	func() {
		self.contractStatsLock.Lock()
		defer self.contractStatsLock.Unlock()
		if !self.contractStatsStarted {
			self.contractStatsStarted = true
			go HandleError(self.runContractStats, self.client.Cancel)
		}
	}()
	callbackId := self.contractStatsCallbacks.Add(contractStatsCallback)
	return func() {
		self.contractStatsCallbacks.Remove(callbackId)
	}
}

func (self *ContractManager) runContractStats() {
	for {
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(self.settings.ContractStatsEpoch):
		}

		callbacks := self.contractStatsCallbacks.Get()

		var events []*ContractStatsEvent
		func() {
			self.contractStatsLock.Lock()
			defer self.contractStatsLock.Unlock()
			for key, entry := range self.contractStatsEntries {
				usedByteCount := ByteCount(entry.usedByteCount.Load())
				closed := entry.closed.Load()
				if !entry.emitted || usedByteCount != entry.emittedUsedByteCount || closed {
					if 0 < len(callbacks) {
						events = append(events, &ContractStatsEvent{
							ContractId:         entry.contractId,
							Receive:            entry.receive,
							Companion:          entry.companion,
							Path:               entry.path,
							TransferByteCount:  entry.transferByteCount,
							UsedByteCount:      usedByteCount,
							UsedByteCountDelta: usedByteCount - entry.emittedUsedByteCount,
							Open:               !closed,
						})
					}
					entry.emitted = true
					entry.emittedUsedByteCount = usedByteCount
				}
				if closed {
					delete(self.contractStatsEntries, key)
				}
			}
		}()

		if 0 < len(events) {
			for _, callback := range callbacks {
				HandleError(func() {
					callback(events)
				})
			}
		}
	}
}
