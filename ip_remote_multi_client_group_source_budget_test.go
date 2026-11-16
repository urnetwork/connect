// Existing source-buffer controls distinguish transfer ownership from charged
// scalar connect-health metadata. These bounded test observations do not prove
// GC reachability, native delivery or new production ownership.
package connect

import (
	"testing"
	"unsafe"
)

// No packet or group is retained by the fixture observer. The update identifies
// the existing lock that protects final offer-pointer detachment.
type groupDispositionConnectReceipt struct {
	offer  *connectSynOffer
	update *multiClientChannelUpdate
}

// Call at the original quiescent synctest/source barrier, while unrelated
// workers cannot change these owners. No time advances or healthy flow closes
// during the census. The original native/pool/source assertions remain separate.
func groupDispositionTransferByteCount(t *testing.T, budget *TransferMemoryBudget, fixtures ...*groupDispositionQueueFixture) ByteCount {
	t.Helper()
	var clientChannels []*multiClientChannel
	var channelUpdates []*multiClientChannelUpdate
	var connectReceipts []groupDispositionConnectReceipt
	for _, fixture := range fixtures {
		if fixture == nil {
			continue
		}
		fixture.connectReceiptLock.Lock()
		receipts, count, overflow := fixture.connectReceipts, fixture.connectReceiptCount, fixture.connectReceiptOverflow
		fixture.connectReceiptLock.Unlock()
		if overflow {
			t.Fatal("bounded SYN metadata receipt census overflowed; source-byte ownership is unknown")
		}
		clientChannels = append(clientChannels, fixture.selected)
		channelUpdates = append(channelUpdates, fixture.update)
		connectReceipts = append(connectReceipts, receipts[:count]...)
	}
	return connectMetadataTransferByteCount(t, budget, clientChannels, channelUpdates, connectReceipts)
}

// Exact physical owner and quota identities account for metadata independently
// of transfer custody. Unknown allocations still fail the quota reconciliation.
// Call only at a quiescent source barrier with every live fixture root supplied.
func connectMetadataTransferByteCount(
	t *testing.T,
	budget *TransferMemoryBudget,
	clientChannels []*multiClientChannel,
	channelUpdates []*multiClientChannelUpdate,
	connectReceipts []groupDispositionConnectReceipt,
) ByteCount {
	t.Helper()
	if budget == nil {
		t.Fatal("source-byte census requires an actual budget")
	}
	owners := map[*connectSynOwner]bool{}
	quotas := map[*connectSynOwnerQuota]bool{}
	updates := map[*multiClientChannelUpdate]bool{}
	add := func(owner *connectSynOwner) {
		if owner != nil {
			owners[owner] = true
			if owner.quota != nil {
				quotas[owner.quota] = true
			}
		}
	}
	for _, channel := range clientChannels {
		if channel == nil {
			t.Fatal("fixture has no native channel")
		}
		channel.stateLock.Lock()
		if quota := channel.connectSynQuota.Load(); quota != nil {
			quotas[quota] = true
		}
		for owner := channel.connectSynOwners; owner != nil; owner = owner.next {
			add(owner)
		}
		channel.stateLock.Unlock()
	}
	for _, update := range channelUpdates {
		updates[update] = true
	}
	for _, receipt := range connectReceipts {
		if receipt.update == nil {
			t.Fatal("SYN receipt has no source update lock")
		}
		updates[receipt.update] = true
		receipt.update.stateLock.Lock()
		add(receipt.offer.candidate)
		add(receipt.offer.owner.Load())
		receipt.update.stateLock.Unlock()
	}
	for update := range updates {
		if update == nil {
			t.Fatal("fixture has no source update")
		}
		update.stateLock.Lock()
		for owner := update.connectSynOwners; owner != nil; owner = owner.flowNext {
			add(owner)
		}
		update.stateLock.Unlock()
	}
	// A child reservation is charged to every actual admission ancestor.
	chargedTo := func(charged *TransferMemoryBudget) bool {
		for ledger := charged; ledger != nil; ledger = ledger.Parent() {
			if ledger == budget {
				return true
			}
		}
		return false
	}
	ownerBytes := (ByteCount(unsafe.Sizeof(connectSynOwner{}))+63)/64*64 +
		(ByteCount(unsafe.Sizeof(connectSynOffer{}))+63)/64*64
	quotaBytes := (ByteCount(unsafe.Sizeof(connectSynOwnerQuota{})) + 63) / 64 * 64
	var metadata ByteCount
	physicalCounts := map[*connectSynOwnerQuota]uint64{}
	for owner := range owners {
		if owner.quota == nil {
			t.Fatal("live health owner has no quota")
		}
		if owner.references.Load() <= 0 {
			t.Fatal("released health owner remains rooted by live source metadata")
		}
		if owner.charge != ownerBytes || owner.budget != owner.quota.budget {
			t.Fatal("health owner charge differs from its independent physical allocation")
		}
		physicalCounts[owner.quota]++
		if chargedTo(owner.budget) {
			metadata += ownerBytes
		}
	}
	for quota := range quotas {
		if quota.charge != quotaBytes {
			t.Fatal("health quota charge differs from its physical allocation")
		}
		state := quota.state.Load()
		count := state &^ connectSynQuotaClosedBit
		liveChannel := state&connectSynQuotaClosedBit == 0
		if liveChannel {
			if count == 0 {
				t.Fatal("open quota lost its channel reference")
			}
			count--
		}
		if count > connectSynOwnerMaxCount || count != physicalCounts[quota] {
			t.Fatalf("unknown health allocation prevents exact source reconciliation: quota_nodes=%d observed_nodes=%d", count, physicalCounts[quota])
		}
		if (liveChannel || count != 0) && chargedTo(quota.budget) {
			metadata += quotaBytes
		}
	}
	used := budget.UsedByteCount()
	if metadata > used {
		t.Fatal("known health credit exceeds the total source budget")
	}
	return used - metadata
}
