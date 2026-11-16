// Terminal candidate promotion preserves charged source-owned connect evidence.
package connect

import (
	"bytes"
	"context"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
	"unsafe"

	"github.com/urnetwork/connect/protocol"
)

const connectPromotionPayload = "synthetic charged opening"

type connectPromotionPeer struct {
	client     *Client
	synCount   atomic.Int64
	resetCount atomic.Int64
	invalid    atomic.Bool
}

// The existing group fixture owns the sender. This real receiver decodes each
// native input and its normal return produces the cumulative Transfer ACK.
func newConnectPromotionPeer(t *testing.T, fixture *groupDispositionQueueFixture) *connectPromotionPeer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	settings := closeWaitClientSettings()
	settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
	settings.EncryptionSettings.beforeEncryptedKeyPublishForTest = func() { <-ctx.Done() }
	self := &connectPromotionPeer{client: NewClient(ctx, fixture.peer, NewNoContractClientOob(), settings)}
	self.client.ContractManager().AddNoContractPeer(fixture.client.ClientId())
	returns := make(Route, 32)
	self.client.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{fixture.route})
	self.client.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(fixture.client.ClientId())), []Route{returns})
	fixture.client.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{returns})
	self.client.AddReceiveCallback(func(source TransferPath, frames []*protocol.Frame, _ Peer) {
		for _, frame := range frames {
			if frame.MessageType != protocol.MessageType_IpIpPacketToProvider {
				continue
			}
			packet, err := ipPacketToProviderBytes(frame)
			if err != nil {
				self.invalid.Store(true)
				continue
			}
			path, payload, err := ParseIpPathWithPayload(packet)
			if err != nil || source.SourceId != fixture.client.ClientId() || path.Version != 4 || path.Protocol != IpProtocolTcp ||
				!path.SourceIp.Equal(net.IPv4(192, 0, 2, 91)) || !path.DestinationIp.Equal(net.IPv4(198, 51, 100, 92)) ||
				path.SourcePort != 44000 || path.DestinationPort != 443 || path.AckSequenceNumber != 0 || path.TcpWindowSize != 64 || path.Fin {
				self.invalid.Store(true)
				continue
			}
			tcp := protocolClassTcpPacket(packet)
			if tcp == nil {
				self.invalid.Store(true)
				continue
			}
			if tcp[13] == tcpFlagSyn && path.Syn && !path.Ack && !path.Rst && path.SequenceNumber == 100 &&
				path.TcpPayloadByteCount == len(connectPromotionPayload) && bytes.Equal(payload, []byte(connectPromotionPayload)) {
				self.synCount.Add(1)
			} else if tcp[13] == tcpFlagRst && path.Rst && !path.Syn && !path.Ack &&
				path.SequenceNumber == 101+uint32(len(connectPromotionPayload)) && len(payload) == 0 {
				self.resetCount.Add(1)
			} else {
				self.invalid.Store(true)
			}
		}
	})
	t.Cleanup(func() {
		fixture.cancel()
		fixture.unpark()
		cancel()
		if err := fixture.client.CloseAndWait(context.Background()); err != nil {
			t.Error("join promotion source: ", err)
		}
		if err := self.client.CloseAndWait(context.Background()); err != nil {
			t.Error("join promotion peer: ", err)
		}
		for len(returns) != 0 {
			MessagePoolReturn(<-returns)
		}
	})
	return self
}

func (self *connectPromotionPeer) await(t *testing.T, fixture *groupDispositionQueueFixture, resets int64) *clientWindowStats {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		synctest.Wait()
		stats, err := fixture.selected.WindowStats()
		if err != nil || self.invalid.Load() || fixture.client.IsDone() || self.client.IsDone() {
			t.Fatal("real promotion carrier failed before its receipt")
		}
		if self.synCount.Load() == 1 && self.resetCount.Load() == resets && stats.sendAckCount == int(1+resets) &&
			stats.sendNackCount == 0 && stats.sendNackByteCount == 0 {
			return stats
		}
		if !time.Now().Before(deadline) {
			t.Fatal("real promotion peer and source did not complete their receipts")
		}
		time.Sleep(defaultAckCompressTimeout)
	}
}

// A wide unanswered payload-bearing SYN race retains both paid descriptors.
// Pure controls intentionally use NoBudget and cannot prove this DATA charge.
// Selection below invokes the production promotion operation without inventing
// a native response: its surviving connect debt must retain the original age.
func TestNativeConnectOwnerTerminalPromotionKeepsChargedGeneration(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		parent := NewTransferMemoryBudget(kib(256))
		budgets := [2]*TransferMemoryBudget{NewTransferMemoryBudgetWithParent(kib(128), parent), NewTransferMemoryBudgetWithParent(kib(128), parent)}
		admitted := make(chan *parsedPacketGroup, 2)
		var admissionOverflow atomic.Bool
		configure := func(settings *ClientSettings) {
			settings.SendBufferSettings.afterGroupAdmissionForTest = func(target sendGroupAdmissionTarget) {
				group, ok := target.(*parsedPacketGroup)
				if !ok {
					t.Error("native candidate returned a different target")
					return
				}
				if group.ipPath.Syn {
					select {
					case admitted <- group:
					default:
						admissionOverflow.Store(true)
					}
				}
			}
		}
		first := newGroupDispositionQueueFixture(t, budgets[0], configure)
		second := newGroupDispositionQueueFixture(t, budgets[1], configure)
		fixtures := [2]*groupDispositionQueueFixture{first, second}
		for _, fixture := range fixtures {
			fixture.selected.settings = first.parent.settings
		}
		peers := [2]*connectPromotionPeer{newConnectPromotionPeer(t, first), newConnectPromotionPeer(t, second)}
		first.update.stateLock.Lock()
		first.update.client.Store(nil)
		first.update.stateLock.Unlock()
		first.parent.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel {
			return []*multiClientChannel{first.selected, second.selected}
		}
		t.Cleanup(func() {
			first.update.Close()
			second.update.Close()
			first.selected.closeConnectSynOwners()
			second.selected.closeConnectSynOwners()
		})
		first.unpark()
		second.unpark()
		opening := groupRecoveryPacket(100, 0, 64, tcpFlagSyn, []byte(connectPromotionPayload))
		if protocolClassPureTcpControl(opening) {
			t.Fatal("charged promotion fixture accidentally constructed an exempt control")
		}
		if first.offer(opening) != 1 {
			t.Fatal("real wide native payload-bearing SYN was not admitted")
		}
		stats := [2]*clientWindowStats{peers[0].await(t, first, 0), peers[1].await(t, second, 0)}
		if admissionOverflow.Load() {
			t.Fatal("native producer exceeded its two observed source admissions")
		}
		var groups [2]*parsedPacketGroup
		for range groups {
			var group *parsedPacketGroup
			select {
			case group = <-admitted:
			default:
				t.Fatal("real candidate admission was not observed")
			}
			index := 0
			if group.completionClient == second.selected {
				index = 1
			} else if group.completionClient != first.selected {
				t.Fatal("candidate escaped its two real sources")
			}
			if groups[index] != nil {
				t.Fatal("same source supplied two candidate descriptors")
			}
			groups[index] = group
		}
		var owners [2]*connectSynOwner
		var retained [2]ByteCount
		func() {
			first.update.stateLock.Lock()
			defer first.update.stateLock.Unlock()
			if first.update.client.Load() != nil || first.update.race == nil {
				t.Fatal("unanswered wide race did not retain its real candidates")
			}
			for index, group := range groups {
				flags, offer := group.completionFlags.Load(), group.connectSynOffer
				if flags&(groupCompletionTerminal|groupCompletionReturned|groupCompletionLinked|groupCompletionCandidate) !=
					groupCompletionTerminal|groupCompletionReturned|groupCompletionLinked|groupCompletionCandidate ||
					offer == nil || !offer.publicReturned.Load() || !offer.terminalFinished.Load() || !offer.referencesReleased.Load() ||
					offer.candidate != nil || offer.owner.Load() != nil || group.collapseSource != nil || group.collapseOwner == nil || group.collapseOwner.remaining != 0 {
					t.Fatal("completed candidate did not detach its offer roots while retaining promotion custody")
				}
				minimumGraph := ByteCount(unsafe.Sizeof(parsedPacketGroup{})) + ByteCount(unsafe.Sizeof(connectSynOffer{})) +
					ByteCount(cap(group.packets))*ByteCount(unsafe.Sizeof(parsedPacket{})) + ByteCount(len(group.packets))*ByteCount(unsafe.Sizeof(IpPath{}))
				if group.collapseOwner.budget != budgets[index] {
					t.Fatalf("candidate %d descriptor charge has the wrong DATA budget: nil=%t", index, group.collapseOwner.budget == nil)
				}
				retained[index] = group.collapseOwner.budgetBytes
				if retained[index] < minimumGraph {
					t.Fatal("completed candidate's pointed offer metadata escaped its retained charge")
				}
				for _, packet := range group.packets {
					if packet.packet != nil || packet.payload != nil {
						t.Fatal("terminal candidate retained a native pool buffer")
					}
				}
				key, valid := fixtures[index].selected.affinitySourceKey()
				if !valid {
					t.Fatal("real candidate lost its source identity")
				}
				for owner := first.update.connectSynOwners; owner != nil; owner = owner.flowNext {
					if owner.sourceKey == key {
						if owners[index] != nil {
							t.Fatal("one real source retained duplicate opening owners")
						}
						owners[index] = owner
					}
				}
				if owners[index] == nil || owners[index].retired.Load() || stats[index].firstUnansweredSendSynTime.IsZero() {
					t.Fatal("actual source opening lost its live debt")
				}
				if owners[index].budget != budgets[index] || owners[index].quota == nil ||
					owners[index].quota.budget != budgets[index] || owners[index].charge <= 0 {
					t.Fatalf("candidate %d source owner has no independent charge in its own DATA budget", index)
				}
			}
		}()
		before := [2]ByteCount{budgets[0].UsedByteCount(), budgets[1].UsedByteCount()}
		for index, owner := range owners {
			fixtures[index].selected.stateLock.Lock()
			started := owner.started
			fixtures[index].selected.stateLock.Unlock()
			if !started.Equal(stats[index].firstUnansweredSendSynTime) {
				t.Fatalf("candidate %d source age differs from its own health marker: owner_ns=%d marker_ns=%d", index, started.UnixNano(), stats[index].firstUnansweredSendSynTime.UnixNano())
			}
			if before[index] < retained[index]+owner.charge {
				t.Fatalf("candidate %d lost an independent paid allocation: used=%d descriptor=%d owner=%d", index, before[index], retained[index], owner.charge)
			}
		}
		first.update.stateLock.Lock()
		received, abandoned, returned, connected := first.update.commitRaceClientWithLock(first.selected)
		first.update.stateLock.Unlock()
		first.update.releaseCollapseClaims()
		if len(received) != 0 || len(returned) != 0 || connected || len(abandoned) != 1 || abandoned[0] != second.selected {
			t.Fatal("selection fabricated a response or changed the controlled two-source race")
		}
		func() {
			first.update.stateLock.Lock()
			defer first.update.stateLock.Unlock()
			if first.update.client.Load() != first.selected || first.update.race != nil || first.update.sequenceClaims != nil {
				t.Fatal("real source promotion did not release the completed candidate descriptors")
			}
			for index, group := range groups {
				if group.connectSynOffer.candidate != nil || group.connectSynOffer.owner.Load() != nil || !group.connectSynOffer.referencesReleased.Load() || group.collapseOwner.budgetBytes != 0 {
					t.Fatal("promotion rebound completed offer roots or retained the descriptor charge")
				}
				found := false
				for owner := first.update.connectSynOwners; owner != nil; owner = owner.flowNext {
					if owner == owners[index] {
						found = true
					}
				}
				if !found || owners[index].retired.Load() {
					t.Fatal("promotion replaced or retired an actual unanswered source owner")
				}
			}
		}()
		for index, fixture := range fixtures {
			current, err := fixture.selected.WindowStats()
			if err != nil {
				t.Fatalf("candidate %d health snapshot failed after promotion: %v", index, err)
			}
			if !current.firstUnansweredSendSynTime.Equal(stats[index].firstUnansweredSendSynTime) {
				t.Fatalf("candidate %d promotion changed exact source age: before_ns=%d after_ns=%d", index, stats[index].firstUnansweredSendSynTime.UnixNano(), current.firstUnansweredSendSynTime.UnixNano())
			}
			after := budgets[index].UsedByteCount()
			if before[index]-after != retained[index] {
				t.Fatalf("candidate %d promotion returned the wrong descriptor credit: before=%d after=%d descriptor=%d", index, before[index], after, retained[index])
			}
			if after < owners[index].charge {
				t.Fatalf("candidate %d promotion lost retained source-owner credit: used=%d owner=%d", index, after, owners[index].charge)
			}
		}
		if parent.UsedByteCount() != budgets[0].UsedByteCount()+budgets[1].UsedByteCount() {
			t.Fatal("promotion broke parent budget custody")
		}
		if first.offer(groupDispositionControlPacket(101+uint32(len(connectPromotionPayload)), 0, 64, tcpFlagRst)) != 1 {
			t.Fatal("promoted real source refused its native reset")
		}
		closed := peers[0].await(t, first, 1)
		other, err := second.selected.WindowStats()
		if err != nil || !first.update.IsDone() || !closed.firstUnansweredSendSynTime.IsZero() ||
			!other.firstUnansweredSendSynTime.Equal(stats[1].firstUnansweredSendSynTime) {
			t.Fatal("winner reset did not isolate its exact promoted source obligation")
		}
		first.update.Close()
		second.update.Close()
		first.selected.closeConnectSynOwners()
		second.selected.closeConnectSynOwners()
		synctest.Wait()
		requireConnectOwnerBudgetEmpty(t, budgets[0], budgets[1], parent)
	})
}
