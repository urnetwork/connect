// Constructor cancellation returns metadata credit before observer callbacks.
package connect

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Reuse the existing real Client constructor and generator-removal barrier.
// Key publication is held until cleanup; this is a metadata lifetime control,
// not a claim about packet delivery, health classification or throughput.
type connectOwnerCleanupTestGenerator struct {
	*cleanupOrderTestGenerator
	ctx    context.Context
	budget *TransferMemoryBudget
}

func (self *connectOwnerCleanupTestGenerator) NewClientSettings() *ClientSettings {
	settings := closeWaitClientSettings()
	settings.SendBufferSettings.ResendQueueBudget = self.budget
	settings.beforeClientKeyPublishForTest = func() { <-self.ctx.Done() }
	settings.EncryptionSettings.beforeEncryptedKeyPublishForTest = func() { <-self.ctx.Done() }
	return settings
}

func testConnectOwnerContextCleanup(t *testing.T, cancelClient bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		parent := NewTransferMemoryBudget(kib(64))
		budget := NewTransferMemoryBudgetWithParent(kib(32), parent)
		generator := &connectOwnerCleanupTestGenerator{
			cleanupOrderTestGenerator: &cleanupOrderTestGenerator{removeCalled: make(chan struct{})},
			ctx:                       ctx,
			budget:                    budget,
		}
		settings := DefaultMultiClientSettings()
		// Keep the existing constructor's optional ping parked. No health
		// deadline or gate is changed and no native workload is claimed.
		settings.startChannelPingForTest = func(func()) {}
		statsGate := newStallGate()
		var channel *multiClientChannel
		offer := &connectSynOffer{}
		t.Cleanup(func() {
			statsGate.Release()
			cancel()
			if owner := offer.candidate; owner != nil {
				offer.candidate = nil
				owner.release()
			}
			if channel != nil {
				channel.closeConnectSynOwners()
			}
			if generator.client != nil {
				if err := generator.client.CloseAndWait(context.Background()); err != nil {
					t.Error("join constructor client: ", err)
				}
			}
			synctest.Wait()
		})
		var err error
		channel, err = newMultiClientChannel(
			ctx,
			&multiClientChannelArgs{
				MultiClientGeneratorClientArgs: MultiClientGeneratorClientArgs{ClientId: NewId()},
				Destination:                    RequireMultiHopId(NewId()),
			},
			generator,
			func(*multiClientChannel, TransferPath, protocol.ProvideMode, TransportType, *IpPath, []byte) {},
			nil,
			DefaultSecurityPolicy(ctx),
			func(*ContractStatus) {},
			func([]*ContractStatsEvent) { statsGate.Wait() },
			func() {},
			nil,
			settings,
			nil, nil, nil, nil, nil, nil, nil, nil, nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		channel.prepareConnectSynOwner(offer)
		owner, quota := offer.candidate, channel.connectSynQuota.Load()
		if owner == nil || quota == nil || budget.UsedByteCount() <= quota.charge {
			t.Fatal("constructor fixture did not reserve actual owner metadata")
		}
		// No flow or carrier owns this direct metadata prepare. Return its
		// sole owner lease, leaving only the real channel quota reservation.
		offer.candidate = nil
		owner.release()
		if budget.UsedByteCount() != quota.charge || parent.UsedByteCount() != quota.charge || quota.charge <= 0 {
			t.Fatal("constructor fixture did not isolate the live channel's quota credit")
		}
		entry := generator.client.ContractManager().registerContractStats(NewId(), false, false, TransferPath{}, 100)
		entry.updateUsedByteCount(10)
		if cancelClient {
			generator.client.Cancel()
		} else {
			cancel()
		}
		// The constructor's context callbacks are the only cleanup trigger;
		// neither channel.Cancel nor channel.Close has run at this point.
		synctest.Wait()
		select {
		case <-generator.removeCalled:
		default:
			t.Fatal("context cancellation did not remove the actual generated client")
		}
		select {
		case <-statsGate.started:
		default:
			t.Fatal("context cleanup did not reach the real blocked stats observer")
		}
		if cancelClient && ctx.Err() != nil {
			t.Fatal("client-context control accidentally canceled its independent parent")
		}
		requireConnectOwnerBudgetEmpty(t, budget, parent)
		statsGate.Release()
	})
}

func TestConnectSynOwnerParentContextCleanupPrecedesBlockedObserver(t *testing.T) {
	testConnectOwnerContextCleanup(t, false)
}

func TestConnectSynOwnerClientContextCleanupPrecedesBlockedObserver(t *testing.T) {
	testConnectOwnerContextCleanup(t, true)
}
