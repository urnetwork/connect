// The no-contract native fixture must pause an actually reachable raw consumer,
// not the contract acquisition branch it deliberately bypasses.
package connect

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

// Positive channel admission/current raw custody precede both release and
// cancellation. An unchanged old barrier fails only after the real peer proves
// that the no-contract source ran past it; a missing hook is never quiescence proof.
func TestNativePendingFixtureConsumerOwnsRawBeforeReleaseOrCancel(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	for _, cancelSource := range []bool{false, true} {
		func() {
			synctest.Test(t, func(t *testing.T) {
				var armed atomic.Bool
				var peer Id
				var client *Client
				var contractTakes atomic.Int32
				entered := make(chan *SendSequence, 1)
				release := make(chan struct{})
				var enterOnce, releaseOnce sync.Once
				finish := func() { releaseOnce.Do(func() { close(release) }) }
				configure := func(settings *ClientSettings) {
					settings.SendBufferSettings.beforeTakeContractForTest = func(sendSequenceId) { contractTakes.Add(1) }
					nativePendingConfigureSourceConsumer(settings, func() *Client { return client }, func(source *SendSequence) {
						id := source.id()
						if !armed.Load() || source.client != client || id.Destination != peer || id.LogicalLane != 0 {
							return
						}
						enterOnce.Do(func() { entered <- source })
						select {
						case <-release:
						case <-source.ctx.Done():
						}
					})
				}
				harness := newPrivateCrossWindowHarness(t, []privateCrossWindowExitSpec{
					{exitSpec: reraceExitSpec{name: "held", answer: reraceAlways(reraceSilent), clientSettings: configure}, windowType: WindowTypeQuality},
				}, nil)
				t.Cleanup(finish)
				channel := privateCrossWindowChannel(t, harness, "held")
				client, peer = channel.client, harness.exit("held").provider.ClientId()
				if !client.ContractManager().SendNoContract(peer) {
					t.Fatal("fixture did not actually configure the source's no-contract peer")
				}
				armed.Store(true)
				path := privateCrossWindowFlow(4, 62, 48362)
				if !privateCrossWindowNativeSyn(harness, path) {
					t.Fatal("fixture control's actual native source admission was refused")
				}
				var source *SendSequence
				select {
				case source = <-entered:
				default:
					if client.ContractManager().SendNoContract(peer) && len(harness.exit("held").syns(path)) == 1 {
						t.Fatal("NO_CONTRACT_FIXTURE_CONSUMER_BARRIER_BYPASSED: actual peer received the native SYN without entering the configured consumer barrier")
					}
					t.Fatal("fixture control had neither a held raw consumer nor positive peer progress")
				}
				update := harness.update(path)
				update.stateLock.Lock()
				ownedRaw := 0
				for group := update.sequenceClaims; group != nil; group = group.collapseNext {
					flags := group.completionFlags.Load()
					const required = groupCompletionAck | groupCompletionTracked | groupCompletionAdmitted | groupCompletionLinked
					if group.completionClient == channel && group.collapseSource == source &&
						group.collapseAdmission.update == update && group.collapseAdmission.epoch == update.sequenceAdmissionEpoch &&
						flags&required == required && flags&(groupCompletionDequeued|groupCompletionAbandoned|groupCompletionDisposed|groupCompletionDone) == 0 &&
						len(group.packets) == 1 && reraceSameFlow(group.ipPath, path) && group.packets[0].ipPath.SequenceNumber == path.SequenceNumber {
						ownedRaw++
					}
				}
				cacheAbsent := update.synRetry == nil
				update.stateLock.Unlock()
				if ownedRaw != 1 || !cacheAbsent || source.client != client || source.id().Destination != peer || source.id().LogicalLane != 0 ||
					source.ctx.Err() != nil || contractTakes.Load() != 0 || len(harness.exit("held").syns(path)) != 0 {
					t.Fatalf("raw consumer did not hold its current admitted source before serialization: raw=%d cache_absent=%t contract_takes=%d", ownedRaw, cacheAbsent, contractTakes.Load())
				}
				if cancelSource {
					if err := client.CloseAndWait(context.Background()); err != nil {
						t.Fatal(err)
					}
					finish()
					harness.settle()
					update.stateLock.Lock()
					liveRaw := false
					for group := update.sequenceClaims; group != nil; group = group.collapseNext {
						flags := group.completionFlags.Load()
						if group.collapseSource == source && flags&groupCompletionLinked != 0 && flags&(groupCompletionDequeued|groupCompletionAbandoned|groupCompletionDisposed|groupCompletionDone) == 0 {
							liveRaw = true
						}
					}
					cacheAbsent = update.synRetry == nil
					update.stateLock.Unlock()
					if source.ctx.Err() == nil || liveRaw || !cacheAbsent || len(harness.exit("held").syns(path)) != 0 {
						t.Fatal("source cancellation failed to join/release the held original or wrote it after cancellation")
					}
					return
				}
				finish()
				harness.settle()
				if contractTakes.Load() != 0 || len(harness.exit("held").syns(path)) != 1 {
					t.Fatal("released actual no-contract source did not materialize exactly one peer SYN")
				}
				owner := synRetryControlOwner(t, update)
				harness.exit("held").sendTo(channel.ClientId(), reraceSynAck(path))
				harness.settle()
				if harness.boundTo(path) != channel || !update.receivedInbound.Load() || len(harness.delivered(path)) != 1 {
					t.Fatal("actual peer reply did not complete the released public source")
				}
				synRetryControlReleased(t, owner)
			})
		}()
	}
}
