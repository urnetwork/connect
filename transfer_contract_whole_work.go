// A client-owned inventory observes the production admission and logical close
// paths. It never blocks an ordinary contract obligation to obtain evidence.
package connect

import (
	"bytes"
	"context"
	"errors"
	"math"
	"sort"
	"sync"

	"github.com/urnetwork/connect/protocol"
)

// Tombstones last for this complete SDK generation. Restart cannot silently
// reuse a generation whose missing cut still belongs to an approved roster.
type originalWorkInventoryOwner struct {
	stateLock   sync.Mutex
	generation  Id
	contractKVs map[Id]protocol.OriginalWorkContract
	revision    uint64
	pending     uint64
	used        int
	broken      bool
}

// The independent request owner can enroll this signed actual lifecycle identity
// before requesting a boundary. Callers cannot supply or restore its generation.
func (self *ContractManager) OriginalWorkIdentity(ctx context.Context) (protocol.OriginalWorkOwnerEnrollment, error) {
	if ctx == nil {
		return protocol.OriginalWorkOwnerEnrollment{}, errors.New("whole-work identity requires an owner")
	}
	if err := ctx.Err(); err != nil {
		return protocol.OriginalWorkOwnerEnrollment{}, errors.Join(err, context.Cause(ctx))
	}
	if self == nil || self.client == nil || self.closeReportDomainHash == ([32]byte{}) {
		return protocol.OriginalWorkOwnerEnrollment{}, errors.New("whole-work identity is unavailable")
	}
	manager := self.client.ClientKeyManager()
	if manager == nil {
		return protocol.OriginalWorkOwnerEnrollment{}, errors.New("whole-work client key owner unavailable")
	}
	manager.stateLock.RLock()
	defer manager.stateLock.RUnlock()
	value := protocol.OriginalWorkOwnerEnrollment{DomainHash: self.closeReportDomainHash, ClientId: [16]byte(self.client.ClientId()), Generation: [16]byte(self.wholeWorkInventory.generation)}
	return protocol.SignOriginalWorkOwnerEnrollment(ctx, value, manager.privateKey)
}

// Revision exhaustion is permanent for this generation, never a fresh zero.
func (self *originalWorkInventoryOwner) advanceWithLock() {
	if self.revision == math.MaxUint64 {
		self.broken = true
		return
	}
	self.revision++
}

// Every returned reservation is retained before publication, including a late
// result retired by its original queue generation. Incoming admission uses the
// same hook after authenticating the destination's original reservation bytes.
func (self *ContractManager) admitOriginalWork(stored []byte) {
	if self.closeReportDomainHash == ([32]byte{}) {
		return
	}
	owner := &self.wholeWorkInventory
	owner.stateLock.Lock()
	defer owner.stateLock.Unlock()
	var contract protocol.StoredContract
	if len(stored) > protocol.MaximumOriginalWorkContractBytes || ProtoUnmarshal(stored, &contract) != nil || len(contract.ContractId) != 16 {
		owner.broken = true
		return
	}
	id := Id(contract.ContractId)
	record := protocol.OriginalWorkContract{ContractId: [16]byte(id), StoredContract: stored}
	source, destination, err := record.Parties()
	if err != nil || [16]byte(self.client.ClientId()) != source && [16]byte(self.client.ClientId()) != destination {
		owner.broken = true
		return
	}
	if prior, ok := owner.contractKVs[id]; ok {
		if !bytes.Equal(prior.StoredContract, stored) {
			owner.broken = true
		}
		return
	}
	if len(owner.contractKVs) >= protocol.MaximumOriginalWorkContracts || len(stored) > protocol.MaximumOriginalWorkCutBytes/2-owner.used {
		owner.broken = true
		return
	}
	if owner.contractKVs == nil {
		owner.contractKVs = map[Id]protocol.OriginalWorkContract{}
	}
	record.StoredContract = bytes.Clone(stored)
	owner.contractKVs[id] = record
	owner.used += len(stored)
	owner.advanceWithLock()
}

// A close without an admitted original, including a legacy carried contract,
// makes the prospective owner unknown instead of inventing its prior history.
func (self *ContractManager) retainOriginalWorkClose(report *protocol.CloseContract) {
	if self.closeReportDomainHash == ([32]byte{}) {
		return
	}
	owner := &self.wholeWorkInventory
	owner.stateLock.Lock()
	defer owner.stateLock.Unlock()
	inventory, err := protocol.DecodeOriginalCloseInventory(report.OriginalInventory)
	if err != nil || len(report.ContractId) != 16 || inventory.DomainHash != self.closeReportDomainHash || inventory.ClientId != [16]byte(self.client.ClientId()) || !bytes.Equal(inventory.ContractId[:], report.ContractId) {
		owner.broken = true
		return
	}
	id := Id(report.ContractId)
	prior, exists := owner.contractKVs[id]
	if !exists {
		owner.broken = true
		return
	}
	if len(prior.LatestInventory) != 0 {
		previous, err := protocol.DecodeOriginalCloseInventory(prior.LatestInventory)
		if err != nil || previous.Terminal || previous.Sequence+1 != inventory.Sequence {
			owner.broken = true
			return
		}
	} else if inventory.Sequence != 1 {
		owner.broken = true
		return
	}
	added := len(report.OriginalInventory) - len(prior.LatestInventory)
	if added > protocol.MaximumOriginalWorkCutBytes/2-owner.used {
		owner.broken = true
		return
	}
	prior.LatestInventory = bytes.Clone(report.OriginalInventory)
	owner.contractKVs[id] = prior
	owner.used += added
	owner.advanceWithLock()
}

// A request is recorded before crossing the asynchronous transport boundary.
func (self *ContractManager) beginOriginalWorkCreate() {
	if self.closeReportDomainHash == ([32]byte{}) {
		return
	}
	owner := &self.wholeWorkInventory
	owner.stateLock.Lock()
	defer owner.stateLock.Unlock()
	if owner.pending >= protocol.MaximumOriginalWorkContracts {
		owner.broken = true
		return
	}
	owner.pending++
	owner.advanceWithLock()
}

// An ambiguous response can conceal an admitted server contract. Only explicit
// parsed result/error frames resolve the request; later success cannot erase it.
func (self *ContractManager) finishOriginalWorkCreate(frames []*protocol.Frame, resultErr error) {
	if self.closeReportDomainHash == ([32]byte{}) {
		return
	}
	complete := resultErr == nil && len(frames) != 0
	for _, frame := range frames {
		if frame == nil || frame.MessageType != protocol.MessageType_TransferCreateContractResult {
			complete = false
			continue
		}
		var result protocol.CreateContractResult
		if ProtoUnmarshal(frame.MessageBytes, &result) != nil || (result.Contract == nil) == (result.Error == nil) {
			complete = false
			continue
		}
		if result.Error != nil {
			if _, known := protocol.ContractError_name[int32(*result.Error)]; !known {
				complete = false
			}
		} else {
			var stored protocol.StoredContract
			if ProtoUnmarshal(result.Contract.StoredContractBytes, &stored) != nil || len(stored.ContractId) != 16 || !bytes.Equal(stored.SourceId, self.client.ClientId().Bytes()) {
				complete = false
				continue
			}
			original := protocol.OriginalWorkContract{ContractId: [16]byte(stored.ContractId), StoredContract: result.Contract.StoredContractBytes}
			if _, _, err := original.Parties(); err != nil {
				complete = false
			}
		}
	}
	owner := &self.wholeWorkInventory
	owner.stateLock.Lock()
	defer owner.stateLock.Unlock()
	if owner.pending == 0 {
		owner.broken = true
	} else {
		owner.pending--
	}
	if !complete {
		owner.broken = true
	}
	owner.advanceWithLock()
}

// Callers retain the returned canonical signed cut outside the SDK lifecycle.
// The exact boundary is supplied by the independently owned epoch observer; a
// cut alone never authorizes its own roster, timestamp or chain boundary.
func (self *ContractManager) OriginalWorkCut(ctx context.Context, epoch, block uint64, blockHash [32]byte) (protocol.OriginalWorkCut, error) {
	if ctx == nil {
		return protocol.OriginalWorkCut{}, errors.New("whole-work cut requires an owner")
	}
	if err := ctx.Err(); err != nil {
		return protocol.OriginalWorkCut{}, err
	}
	manager := self.client.ClientKeyManager()
	if manager == nil || self.closeReportDomainHash == ([32]byte{}) {
		return protocol.OriginalWorkCut{}, errors.New("whole-work signing domain is unavailable")
	}
	manager.stateLock.RLock()
	defer manager.stateLock.RUnlock()
	owner := &self.wholeWorkInventory
	cut := func() protocol.OriginalWorkCut {
		owner.stateLock.Lock()
		defer owner.stateLock.Unlock()
		cut := protocol.OriginalWorkCut{DomainHash: self.closeReportDomainHash, ClientId: [16]byte(self.client.ClientId()), Generation: [16]byte(owner.generation), Epoch: epoch, Block: block, BlockHash: blockHash, Revision: owner.revision, Complete: !owner.broken && owner.pending == 0, Contracts: make([]protocol.OriginalWorkContract, 0, len(owner.contractKVs))}
		for _, original := range owner.contractKVs {
			cut.Contracts = append(cut.Contracts, protocol.OriginalWorkContract{ContractId: original.ContractId, StoredContract: bytes.Clone(original.StoredContract), OriginalCreation: bytes.Clone(original.OriginalCreation), LatestInventory: bytes.Clone(original.LatestInventory)})
		}
		return cut
	}()
	sort.Slice(cut.Contracts, func(i, j int) bool {
		return bytes.Compare(cut.Contracts[i].ContractId[:], cut.Contracts[j].ContractId[:]) < 0
	})
	return protocol.SignOriginalWorkCut(ctx, cut, manager.privateKey)
}
