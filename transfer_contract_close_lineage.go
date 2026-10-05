// Optional inventory ownership is bounded for the complete client lifecycle.
// Exhaustion leaves evidence unknown; it never prevents ordinary close delivery.
package connect

import (
	"crypto/sha256"
	"errors"
	"math"
	"sync"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
)

const maximumOriginalCloseInventoryContracts = 8192

type originalCloseInventoryState struct {
	sequence uint64
	total    uint64
	previous [32]byte
	terminal bool
	broken   bool
}

// Terminal tombstones prevent a later caller from restarting a completed chain.
// They are released with the client; a finite full owner emits legacy originals.
type originalCloseInventoryOwner struct {
	stateLock   sync.Mutex
	contractKVs map[Id]originalCloseInventoryState
}

// Only logical creation advances the chain. Native/OOB retries own the already
// serialized pair and never call this method again. Cleanup may finish after cancel.
func (self *ContractManager) signOriginalCloseInventory(report *protocol.CloseContract) (originalRaw []byte, inventoryRaw []byte, resultErr error) {
	manager := self.client.ClientKeyManager()
	if manager == nil || report == nil || len(report.ContractId) != 16 || len(report.ReportId) != 16 {
		return nil, nil, errors.New("original close key owner or tuple is unavailable")
	}
	owner := &self.closeInventory
	owner.stateLock.Lock()
	defer owner.stateLock.Unlock()
	// Capture while the logical per-contract sequence still owns this lock.
	// A concurrent checkpoint cannot publish its whole-owner head out of order.
	defer func() {
		// a protobuf message must not be copied by value (it carries its own
		// state lock), so retain a clone with the inventory attached
		owned := proto.Clone(report).(*protocol.CloseContract)
		owned.OriginalInventory = inventoryRaw
		self.retainOriginalWorkClose(owned)
	}()
	manager.stateLock.RLock()
	defer manager.stateLock.RUnlock()
	original, err := protocol.SignOriginalCloseReport(protocol.OriginalCloseReport{DomainHash: self.closeReportDomainHash, ClientId: [16]byte(self.client.ClientId()), ContractId: [16]byte(report.ContractId), ReportId: [16]byte(report.ReportId), AckedByteCount: report.AckedByteCount, UnackedByteCount: report.UnackedByteCount, Checkpoint: report.Checkpoint}, manager.privateKey)
	if err != nil {
		return nil, nil, err
	}
	raw, err := original.Bytes()
	if err != nil {
		return nil, nil, err
	}
	if owner.contractKVs == nil {
		owner.contractKVs = map[Id]originalCloseInventoryState{}
	}
	id := Id(original.ContractId)
	prior, exists := owner.contractKVs[id]
	if !exists && len(owner.contractKVs) >= maximumOriginalCloseInventoryContracts {
		return raw, nil, nil
	}
	if prior.broken || prior.terminal || prior.sequence >= protocol.MaximumOriginalCloseInventoryReports || report.AckedByteCount > math.MaxUint64-prior.total {
		prior.broken = true
		owner.contractKVs[id] = prior
		return raw, nil, nil
	}
	next := protocol.OriginalCloseInventory{DomainHash: original.DomainHash, ClientId: original.ClientId, ContractId: original.ContractId, ReportHash: sha256.Sum256(raw), Sequence: prior.sequence + 1, CumulativeAckedBytes: prior.total + report.AckedByteCount, Previous: prior.previous, Terminal: !report.Checkpoint}
	next, err = protocol.SignOriginalCloseInventory(next, manager.privateKey)
	if err != nil {
		prior.broken = true
		owner.contractKVs[id] = prior
		return raw, nil, nil
	}
	inventory, err := next.Bytes()
	if err != nil {
		prior.broken = true
		owner.contractKVs[id] = prior
		return raw, nil, nil
	}
	owner.contractKVs[id] = originalCloseInventoryState{sequence: next.Sequence, total: next.CumulativeAckedBytes, previous: sha256.Sum256(inventory), terminal: next.Terminal}
	return raw, inventory, nil
}
