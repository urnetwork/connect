// Established native fixtures keep only their live channel's quota metadata.
package connect

import (
	"testing"
	"unsafe"
)

// Call from the test goroutine at an existing observation boundary, never
// from a SendSequence callback. This grants no allowance for owner nodes,
// pending SYN offers, deferred releases, an unknown ledger or a closed quota.
// A provider-only fixture with no native channel receives no allowance.
func nativeConnectQuotaByteCount(t *testing.T, budget *TransferMemoryBudget, channel *multiClientChannel) ByteCount {
	t.Helper()
	if budget == nil {
		t.Fatal("native quota census requires an actual DATA budget")
	}
	if channel == nil {
		return 0
	}
	if channel.client == nil || channel.client.settings == nil || channel.client.settings.SendBufferSettings == nil ||
		channel.client.settings.SendBufferSettings.ResendQueueBudget != budget {
		t.Fatal("native quota census does not name the actual source DATA ledger")
	}
	channel.stateLock.Lock()
	quota := channel.connectSynQuota.Load()
	closed := channel.connectSynClosed || channel.connectSynQuotaClosed.Load()
	clear := channel.connectSynOwners == nil && channel.connectSynPending == nil && channel.connectSynReleased.Load() == nil
	channel.stateLock.Unlock()
	if closed || !clear {
		t.Fatalf("native quota census is not a live channel with no SYN owners: closed=%t clear=%t", closed, clear)
	}
	if quota == nil {
		return 0
	}
	bytes := (ByteCount(unsafe.Sizeof(connectSynOwnerQuota{})) + 63) / 64 * 64
	if quota.budget != budget || quota.charge != bytes || quota.state.Load() != 1 ||
		channel.connectSynQuota.Load() != quota || channel.connectSynQuotaClosed.Load() {
		t.Fatalf("native quota is not its sole exact paid channel lease: same_budget=%t charge=%d expected=%d state=%d", quota.budget == budget, quota.charge, bytes, quota.state.Load())
	}
	if budget.UsedByteCount() < bytes {
		t.Fatal("native quota charge exceeds the source DATA ledger")
	}
	return bytes
}
