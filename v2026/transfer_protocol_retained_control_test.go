package connect

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
)

// This direct queue fixture tests retained metadata permission, not wire
// delivery. The separate public pressure root owns the real Transfer proof.
func protocolRetainedFullSource(t *testing.T, shared bool) (*SendSequence, *TransferMemoryBudget, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	queue := retainedControlTestSendQueue()
	var budget *TransferMemoryBudget
	if shared {
		budget = NewTransferMemoryBudget(64)
		queue.setBudget(budget, 0)
	}
	item := &sendItem{transferItem: transferItem{messageId: NewId(), sequenceNumber: 1}, transferFrameBytes: MessagePoolGet(64)}
	queue.Add(item)
	source := &SendSequence{ctx: ctx, resendQueue: queue,
		sendBufferSettings: &SendBufferSettings{ResendQueueMaxByteCount: 64}, retainedAdmissionWake: make(chan struct{}, 1)}
	t.Cleanup(func() {
		cancel()
		for _, owned := range queue.Clear() {
			MessagePoolReturn(owned.transferFrameBytes)
		}
		if budget != nil {
			assertRetainedBudgetBalance(t, budget)
		}
	})
	return source, budget, cancel
}

func TestNoBudgetRetainedProtocolMetadataDoesNotUseDataPermission(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, shared := range []bool{false, true} {
		source, budget, cancel := protocolRetainedFullSource(t, shared)
		_, beforeBytes := source.resendQueue.QueueSize()
		var before TransferMemoryBudgetStats
		if budget != nil {
			before = budget.Stats()
		}
		control := protocolClassTestPacket(false, 0x02, 12, 0)
		data := protocolClassTestPacket(false, 0x02, 12, 1)
		if token, ok := source.reserveRetainedControlMemory(100, protocolClassPureTcpControl(data)); ok || token != nil {
			t.Fatal("payload-bearing SYN metadata bypassed full DATA permission")
		}
		token, ok := source.reserveRetainedControlMemory(100, protocolClassPureTcpControl(control))
		if !ok || token == nil || !token.noBudget || token.budget != nil || token.local != nil || token.byteCount != 0 {
			t.Fatal("pure protocol state did not take exactly one uncharged lifetime token")
		}
		_, afterBytes := source.resendQueue.QueueSize()
		if afterBytes != beforeBytes || source.resendQueue.CanAdd(1, 64) || budget != nil && budget.Stats() != before {
			t.Fatal("protocol token changed DATA window or byte permission")
		}
		cancel()
		if _, ok := source.reserveRetainedControlMemory(1, true); ok {
			t.Fatal("canceled source acquired new protocol lifetime")
		}
		var joined sync.WaitGroup
		for range 2 {
			joined.Go(token.Release)
		}
		joined.Wait()
		if !token.released.Load() || len(source.retainedAdmissionWake) != 1 || budget != nil && budget.Stats() != before {
			t.Fatal("concurrent final release changed DATA custody or repeated its wake")
		}
	}
}

func TestNoBudgetSynCacheReadinessPreservesExemptionThroughReplacement(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		control := newSynRetryReadyControl(t, 51490)
		owner, old := control.owner, control.owner.readiness
		packetToken, oldToken := owner.credit, old.credit
		if !protocolClassPureTcpControl(owner.packet) || !packetToken.noBudget || !oldToken.noBudget || packetToken.byteCount != 0 || oldToken.byteCount != 0 {
			t.Fatal("actual native SYN/cache/readiness inherited DATA byte ownership")
		}
		source, budget, _ := protocolRetainedFullSource(t, true)
		before := budget.Stats()
		group := &parsedPacketGroup{synRetry: &tcpSynRetryRequest{update: control.update, serial: owner.serial}}
		width := len(old.slots) + 1
		group.noteSynRetryReadinessWidth(width)
		group.prepareSynRetryReadinessCapacity(source)
		control.update.stateLock.Lock()
		fresh := owner.readiness
		exact := control.update.synRetry == owner && owner.references == 1 && fresh != nil && fresh != old &&
			len(fresh.slots) == width && fresh.credit != nil && fresh.credit.noBudget && fresh.credit.byteCount == 0 &&
			!owner.readinessOverflow && !fresh.indexed.Load() && fresh.count == 0
		control.update.stateLock.Unlock()
		if !exact || !oldToken.released.Load() || old.references.Load() != 0 || budget.Stats() != before {
			t.Fatal("readiness replacement used DATA capacity, lost its bounded field or retained the prior owner")
		}
		freshToken := fresh.credit
		control.close(t)
		if !packetToken.released.Load() || !freshToken.released.Load() || fresh.references.Load() != 0 || fresh.slots != nil || budget.Stats() != before {
			t.Fatal("joined SYN/cache/readiness teardown lost exact uncharged lifetime cleanup")
		}
	})
}

func TestNoBudgetRetainedProtocolRejectsRetiredSource(t *testing.T) {
	assertMessagePoolOwnership(t)
	source, budget, _ := protocolRetainedFullSource(t, true)
	token, ok := source.reserveRetainedControlMemory(1, true)
	if !ok {
		t.Fatal("live protocol source did not admit its lifetime token")
	}
	for _, item := range source.resendQueue.Clear() {
		MessagePoolReturn(item.transferFrameBytes)
	}
	if _, ok := source.reserveRetainedControlMemory(1, true); ok {
		t.Fatal("retired queue reopened protocol ownership")
	}
	token.Release()
	if budget.UsedByteCount() != 0 || !token.released.Load() {
		t.Fatal("late protocol token release changed retired DATA ownership")
	}
}
