// Connect-health metadata keeps real budget custody through close and return.
package connect

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

func requireConnectOwnerBudgetEmpty(t *testing.T, budgets ...*TransferMemoryBudget) {
	t.Helper()
	for _, budget := range budgets {
		reserved, released := budget.Counts()
		if budget.UsedByteCount() != 0 || reserved != released {
			t.Fatalf("connect metadata budget did not balance: used=%d reserved=%d released=%d", budget.UsedByteCount(), reserved, released)
		}
	}
}

// This exercises only metadata lifetime admission. No fake Transfer callback
// or health counter is used to claim carrier, native, or performance evidence.
func newConnectOwnerBudgetChannel(t *testing.T, budget *TransferMemoryBudget) *multiClientChannel {
	t.Helper()
	settings := closeWaitClientSettings()
	settings.SendBufferSettings.ResendQueueBudget = budget
	channel := newConnectAdmissionTestChannel(t)
	channel.client = &Client{settings: settings}
	t.Cleanup(channel.closeConnectSynOwners)
	return channel
}

// Both close orderings are explicit. A reserved owner is still charged after
// quota closure, and closure prevents a later owner from entering the quota.
func TestConnectSynOwnerQuotaClosePreservesReservedLifetime(t *testing.T) {
	for _, closeFirst := range []bool{false, true} {
		parent := NewTransferMemoryBudget(kib(64))
		budget := NewTransferMemoryBudgetWithParent(kib(32), parent)
		channel := newConnectOwnerBudgetChannel(t, budget)
		if closeFirst {
			channel.closeConnectSynOwners()
		}
		offer := &connectSynOffer{}
		t.Cleanup(func() {
			if owner := offer.candidate; owner != nil {
				offer.candidate = nil
				owner.release()
			}
		})
		channel.prepareConnectSynOwner(offer)
		if closeFirst {
			if offer.candidate != nil || channel.connectSynQuota.Load() != nil {
				t.Fatal("closed channel published a new metadata owner or quota")
			}
			requireConnectOwnerBudgetEmpty(t, budget, parent)
			continue
		}
		owner, quota := offer.candidate, channel.connectSynQuota.Load()
		if owner == nil || quota == nil || budget.UsedByteCount() <= 0 || parent.UsedByteCount() != budget.UsedByteCount() {
			t.Fatal("actual metadata admission did not reserve its exact parent budget")
		}
		before := budget.UsedByteCount()
		channel.closeConnectSynOwners()
		if budget.UsedByteCount() != before || parent.UsedByteCount() != before {
			t.Fatal("quota close released a still-owned metadata allocation")
		}
		late := &connectSynOffer{}
		t.Cleanup(func() {
			if owner := late.candidate; owner != nil {
				late.candidate = nil
				owner.release()
			}
		})
		channel.prepareConnectSynOwner(late)
		if late.candidate != nil || budget.UsedByteCount() != before {
			t.Fatal("closed quota admitted another owner")
		}
		// This exact direct prepare never acquired a flow or callback. Its
		// test-owned candidate is the sole remaining lease and is returned once.
		offer.candidate = nil
		owner.release()
		requireConnectOwnerBudgetEmpty(t, budget, parent)
		channel.closeConnectSynOwners()
		requireConnectOwnerBudgetEmpty(t, budget, parent)
	}
}

// Preparation pauses after its real budget reservation but before publication.
// Channel close observes no quota; the resumed publisher must return all credit.
func TestConnectSynOwnerQuotaPublicationAfterCloseReturnsCredit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parent := NewTransferMemoryBudget(kib(64))
		budget := NewTransferMemoryBudgetWithParent(kib(32), parent)
		channel := newConnectOwnerBudgetChannel(t, budget)
		entered, release, joined := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var once sync.Once
		resume := func() { once.Do(func() { close(release) }) }
		channel.beforeConnectSynQuotaPublishForTest = func() { close(entered); <-release }
		offer := &connectSynOffer{}
		go func() { defer close(joined); channel.prepareConnectSynOwner(offer) }()
		t.Cleanup(func() {
			resume()
			<-joined
			if owner := offer.candidate; owner != nil {
				offer.candidate = nil
				owner.release()
			}
		})
		synctest.Wait()
		select {
		case <-entered:
		default:
			t.Fatal("prepare did not reach its actual quota-publication boundary")
		}
		if channel.connectSynQuota.Load() != nil || budget.UsedByteCount() <= 0 || parent.UsedByteCount() != budget.UsedByteCount() {
			t.Fatal("publication barrier did not hold a real unpublished reservation")
		}
		channel.closeConnectSynOwners()
		resume()
		<-joined
		if offer.candidate != nil {
			t.Fatal("publisher created an owner after channel close")
		}
		requireConnectOwnerBudgetEmpty(t, budget, parent)
	})
}

// The real peer ACK completes while Transfer's public native handoff is held.
// Flow and channel closure cannot release the allocation still held by that
// caller, and the eventual return must detach its metadata roots and balance.
func TestNativeConnectOwnerPeerAckBeforePublicReturnKeepsCredit(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		parentBudget := NewTransferMemoryBudget(kib(128))
		budget := NewTransferMemoryBudgetWithParent(kib(64), parentBudget)
		admitted := make(chan *parsedPacketGroup, 1)
		release := make(chan struct{})
		var once sync.Once
		resume := func() { once.Do(func() { close(release) }) }
		fixture := newGroupDispositionQueueFixture(t, budget, func(settings *ClientSettings) {
			settings.SendBufferSettings.afterGroupAdmissionForTest = func(target sendGroupAdmissionTarget) {
				group, ok := target.(*parsedPacketGroup)
				if !ok {
					t.Error("actual native source returned a different target")
					return
				}
				admitted <- group
				<-release
			}
		})
		fixture.selected.settings = fixture.parent.settings
		ctx, cancel := context.WithCancel(context.Background())
		settings := closeWaitClientSettings()
		settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		settings.EncryptionSettings.beforeEncryptedKeyPublishForTest = func() { <-ctx.Done() }
		peer := NewClient(ctx, fixture.peer, NewNoContractClientOob(), settings)
		peer.ContractManager().AddNoContractPeer(fixture.client.ClientId())
		returns := make(Route, 32)
		peer.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{fixture.route})
		peer.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(fixture.client.ClientId())), []Route{returns})
		fixture.client.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{returns})
		var received atomic.Int64
		var invalid atomic.Bool
		peer.AddReceiveCallback(func(source TransferPath, frames []*protocol.Frame, _ Peer) {
			for _, frame := range frames {
				if frame.MessageType != protocol.MessageType_IpIpPacketToProvider {
					continue
				}
				packet, err := ipPacketToProviderBytes(frame)
				if err != nil {
					invalid.Store(true)
					continue
				}
				path, payload, err := ParseIpPathWithPayload(packet)
				if err != nil || source.SourceId != fixture.client.ClientId() || path.Version != 4 || path.Protocol != IpProtocolTcp ||
					!path.Syn || path.Ack || path.Rst || path.Fin || len(payload) != 0 || path.SequenceNumber != 100 {
					invalid.Store(true)
				}
				received.Add(1)
			}
			// Returning permits this actual receiver's normal cumulative Pack ACK.
		})
		joined := make(chan struct{})
		result := make(chan bool, 1)
		t.Cleanup(func() {
			resume()
			fixture.cancel()
			fixture.unpark()
			cancel()
			<-joined
			fixture.update.Close()
			fixture.selected.closeConnectSynOwners()
			if err := fixture.client.CloseAndWait(context.Background()); err != nil {
				t.Error("join source: ", err)
			}
			if err := peer.CloseAndWait(context.Background()); err != nil {
				t.Error("join peer: ", err)
			}
			for len(returns) != 0 {
				MessagePoolReturn(<-returns)
			}
		})
		fixture.unpark()
		packet := MessagePoolCopy(groupDispositionControlPacket(100, 0, 64, tcpFlagSyn))
		fixture.witnesses = append(fixture.witnesses, MessagePoolShareReadOnly(packet))
		go func() {
			defer close(joined)
			accepted := [1]bool{}
			count := fixture.parent.SendNativePacketBatchWithResults(fixture.source, protocol.ProvideMode_Network, [][]byte{packet}, 0, accepted[:])
			result <- count == 1 && accepted[0]
		}()
		synctest.Wait()
		var group *parsedPacketGroup
		select {
		case group = <-admitted:
		default:
			t.Fatal("native source never reached its held successful handoff")
		}
		deadline := time.Now().Add(time.Second)
		for {
			synctest.Wait()
			stats, err := fixture.selected.WindowStats()
			if err != nil || invalid.Load() || fixture.client.IsDone() || peer.IsDone() {
				t.Fatal("genuine carrier failed before its peer ACK")
			}
			if received.Load() == 1 && stats.sendAckCount == 1 && stats.sendNackCount == 0 && stats.sendNackByteCount == 0 &&
				group.completionFlags.Load()&groupCompletionTerminal != 0 {
				break
			}
			if !time.Now().Before(deadline) {
				t.Fatal("actual peer ACK did not complete while public return was held")
			}
			time.Sleep(defaultAckCompressTimeout)
		}
		offer := group.connectSynOffer
		if offer == nil || offer.publicReturned.Load() || !offer.terminalFinished.Load() || group.completionFlags.Load()&groupCompletionReturned != 0 {
			t.Fatal("actual callback-before-public-return premise was absent")
		}
		fixture.update.stateLock.Lock()
		owner := offer.candidate
		bound := offer.owner.Load() == owner && owner != nil
		fixture.update.stateLock.Unlock()
		if !bound || budget.UsedByteCount() <= 0 {
			t.Fatal("real admitted SYN did not retain its charged metadata owner")
		}
		fixture.update.Close()
		fixture.selected.closeConnectSynOwners()
		if budget.UsedByteCount() < owner.charge || parentBudget.UsedByteCount() != budget.UsedByteCount() || offer.referencesReleased.Load() {
			t.Fatal("close released metadata still owned by the held public offer")
		}
		resume()
		<-joined
		if !<-result {
			t.Fatal("held native admission lost its original successful result")
		}
		synctest.Wait()
		fixture.update.stateLock.Lock()
		detached := offer.candidate == nil && offer.owner.Load() == nil
		fixture.update.stateLock.Unlock()
		if !detached || !offer.referencesReleased.Load() {
			t.Fatal("completed public return retained released metadata roots")
		}
		requireConnectOwnerBudgetEmpty(t, budget, parentBudget)
	})
}
