// Failed hold admission returns only newly acquired legacy credit. Real paid
// decoder roots remain charged until their caller returns the refused owner.
package connect

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
)

// Drives the real handoff worker with an allocating decoder that has no paid
// ingress owner. Strict repair admission refuses it without acquiring a lease.
func TestCanonicalDecodedMetadataRepairRefusalRefundsLegacyHold(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		for _, count := range []int{2, 8} {
			parent := NewTransferMemoryBudget(mib(2))
			packBudget := NewTransferMemoryBudgetWithParent(mib(1), parent)
			receiveBudget := NewTransferMemoryBudgetWithParent(mib(1), parent)
			t.Cleanup(func() {
				for _, budget := range []*TransferMemoryBudget{packBudget, receiveBudget, parent} {
					assertRetainedBudgetBalance(t, budget)
					if budget.Stats().QuotaByteCount != 0 {
						t.Error("refused legacy owner retained handoff quota")
					}
				}
			})
			var witness *canonicalDecodedMetadataWitness
			witnessFinished := false
			t.Cleanup(func() {
				if witness != nil && !witnessFinished {
					witness.finish(t)
				}
			})
			fixture := newWindowRoundFixture(t, nil, func(settings *ReceiveBufferSettings) {
				settings.SequenceBufferSize, settings.H1SequenceBufferSize = 4, 4
				settings.PackQueueBudget, settings.PackQueueRetainedByteAccounting = packBudget, true
				settings.ReceiveQueueBudget, settings.ReceiveQueueRetainedByteAccounting = receiveBudget, true
				settings.ReceiveQueueMinByteCount = 0
				settings.ReceiveQueueMaxByteCount = kib(64)
				settings.ReceiveHoldPolicy = ReceiveHoldRefuse
			}, TransportTypeH1)
			entered, released := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(released) }) }
			t.Cleanup(release)
			fixture.receiver.receiveBuffer.beforeRunReceiveSequenceForTest = func(receiveSequenceId) {
				close(entered)
				<-released
			}
			sequenceId, messageId := NewId(), NewId()
			witness = canonicalDecodeMetadataPacket(t, fixture.sender.ClientId(), fixture.receiver.ClientId(), sequenceId, messageId, 1, false, count)
			witness.pack.Ctx, witness.pack.ReceiveCallback = fixture.ctx, fixture.receiver.receiveCallback
			parent.SetTotalByteCount(2 * witness.want)
			packBudget.SetTotalByteCount(witness.want)
			receiveBudget.SetTotalByteCount(witness.want)
			accepted, err := fixture.receiver.receiveBuffer.Pack(witness.pack, 0)
			witness.handed = accepted
			<-entered
			if !accepted || err != nil || witness.pack.receiveMemoryOwned || witness.owner.receiveItem.memoryBudget != nil ||
				packBudget.UsedByteCount() != witness.want || receiveBudget.UsedByteCount() != 0 {
				t.Fatal("legacy refusal did not enter with only its real handoff owner", count, accepted, err)
			}
			release()
			synctest.Wait()
			manager := fixture.receiver.receiveBuffer
			manager.mutex.Lock()
			sequence := manager.receiveSequences[receiveSequenceId{Source: SourceId(fixture.sender.ClientId()), SequenceId: sequenceId, EncryptionRole: sequenceTlsRoleServer}]
			manager.mutex.Unlock()
			if sequence == nil || sequence.receiveQueue.GetByMessageId(messageId) != nil || sequence.ackWindow.Pending() ||
				sequence.heldHeadRepair.eligibleCount != 0 || sequence.heldHeadRepair.reservedByteCount != 0 ||
				fixture.receiver.receiveQueueDropCount.Load() != 1 || len(fixture.receiverOut) != 0 || fixture.deliveredCount != 0 ||
				witness.owner.inUse || packBudget.UsedByteCount() != 0 {
				t.Fatal("failed repair admission retained or acknowledged an unowned original", count)
			}
			stats := receiveBudget.Stats()
			t.Logf("legacy_repair_refusal frames=%d C=%d used=%d reserved=%d released=%d held=0 ack=0 ownerReturned=true", count,
				witness.want, stats.UsedByteCount, stats.ReservedByteCount, stats.ReleasedByteCount)
			if stats.UsedByteCount != 0 || stats.ReservedByteCount != witness.want || stats.ReleasedByteCount != witness.want || parent.UsedByteCount() != 0 {
				t.Errorf("failed repair admission leaked legacy decoded hold credit: frames=%d C=%d stats=%+v parent=%d", count, witness.want, stats, parent.UsedByteCount())
				// The counterfactual is already recorded. Return only the exact
				// orphan for fixture teardown; no retry or progress uses it.
				if stats.UsedByteCount == witness.want && stats.ReservedByteCount == witness.want && stats.ReleasedByteCount == 0 {
					receiveBudget.Release(witness.want)
				}
			}
			// A failed bubble stops its outer test. Keep both shapes inside
			// this bubble, but join each real worker before the next shape.
			fixture.cancel()
			for _, client := range []*Client{fixture.sender, fixture.receiver} {
				if err := client.CloseAndWait(context.Background()); err != nil {
					t.Fatal("legacy refusal worker did not join", count, err)
				}
			}
			witness.finish(t)
			witnessFinished = true
			for _, budget := range []*TransferMemoryBudget{packBudget, receiveBudget, parent} {
				assertRetainedBudgetBalance(t, budget)
			}
		}
	})
}

// The worker's false return precedes the caller's actual root return. Observe
// that boundary directly so a broad rollback cannot hide premature refunds.
func TestCanonicalDecodedMetadataRepairRefusalRetainsPaidOwner(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, count := range []int{2, 8} {
		synctest.Test(t, func(t *testing.T) {
			parent := NewTransferMemoryBudget(mib(2))
			receiveBudget := NewTransferMemoryBudgetWithParent(mib(1), parent)
			t.Cleanup(func() {
				assertRetainedBudgetBalance(t, receiveBudget)
				assertRetainedBudgetBalance(t, parent)
			})
			fixture := newWindowRoundFixture(t, nil, func(settings *ReceiveBufferSettings) {
				settings.ReceiveQueueBudget, settings.ReceiveQueueRetainedByteAccounting = receiveBudget, true
				settings.ReceiveQueueMinByteCount = 0
				settings.ReceiveQueueMaxByteCount = kib(64)
				settings.ReceiveHoldPolicy = ReceiveHoldRefuse
			}, TransportTypeH1)
			sequenceId, messageId := NewId(), NewId()
			witness := canonicalDecodeMetadataPacket(t, fixture.sender.ClientId(), fixture.receiver.ClientId(), sequenceId, messageId, 1, false, count, fixture.receiver)
			defer witness.finish(t)
			witness.pack.Ctx, witness.pack.ReceiveCallback = fixture.ctx, fixture.receiver.receiveCallback
			sequence := NewReceiveSequence(fixture.ctx, fixture.receiver, SourceId(fixture.sender.ClientId()), sequenceId,
				sequenceTlsRoleServer, false, fixture.receiver.settings.ReceiveBufferSettings)
			sequence.peerAudit = NewSequencePeerAudit(fixture.receiver, sequence.source, 0)
			defer func() {
				sequence.Close()
				for _, item := range sequence.receiveQueue.Clear() {
					item.messagePoolReturn()
				}
				sequence.releaseHoldPromise()
				sequence.peerAudit.Complete()
			}()
			if !sequence.bindReceiveMemoryOwner(witness.pack) || !witness.pack.receiveMemoryOwned ||
				witness.owner.receiveItem.memoryBudget != receiveBudget || witness.owner.receiveItem.queueByteCount != witness.want {
				t.Fatal("paid refusal lacks actual decoder and ingress custody", count)
			}
			parent.SetTotalByteCount(witness.want)
			receiveBudget.SetTotalByteCount(witness.want)
			accepted, err := sequence.receive(witness.pack)
			witness.handed = accepted
			if accepted || err != nil || !witness.owner.inUse || !witness.pack.receiveMemoryOwned ||
				witness.owner.receiveItem.memoryBudget != receiveBudget || receiveBudget.UsedByteCount() != witness.want ||
				parent.UsedByteCount() != witness.want || receiveBudget.Stats().ReleasedByteCount != 0 ||
				sequence.receiveQueue.GetByMessageId(messageId) != nil || sequence.ackWindow.Pending() ||
				sequence.heldHeadRepair.eligibleCount != 0 || sequence.heldHeadRepair.reservedByteCount != 0 ||
				fixture.receiver.receiveQueueDropCount.Load() != 1 || fixture.deliveredCount != 0 {
				t.Fatal("repair refusal returned live paid roots or acquired a selective lease", count, accepted, err)
			}
			t.Logf("paid_repair_refusal frames=%d C=%d used=%d released=0 ownerAlive=true ack=0", count, witness.want, receiveBudget.UsedByteCount())
			witness.pack.messagePoolReturn()
			witness.returned = true
			stats := receiveBudget.Stats()
			if stats.UsedByteCount != 0 || stats.ReservedByteCount != witness.want || stats.ReleasedByteCount != witness.want ||
				parent.UsedByteCount() != 0 || sequence.holdPromise.ingressHead != nil {
				t.Fatal("actual paid-root return did not refund exactly one owner", count, stats)
			}
		})
	}
}
