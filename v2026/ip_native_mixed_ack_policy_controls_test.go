// Held peer ACKs expose the difference between descriptive protocol owners
// and exact DATA retention before any capacity can be recycled.
package connect

import (
	"bytes"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// Alternating classes create sixteen independently funded DATA owners and
// sixteen protocol owners; one old mixed-group estimate cannot fund them.
func TestNativeMixedAckAllocationPartitionBudgetMatchesRetainedData(t *testing.T) {
	checkNativeMixedAckPartitionCapacity(t, false)
}

// A full 32-member protocol flight keeps positive graph metadata and exact
// packet custody while zero DATA permission remains sufficient.
func TestNativeMixedAckAllocationPureProtocolRetainsGraphWithoutDataCharge(t *testing.T) {
	checkNativeMixedAckPartitionCapacity(t, true)
}

// Both cases use the public native entry and the actual serializer. Only the
// fixture-owned source barrier and later decoded peer ACKs control progress.
func checkNativeMixedAckPartitionCapacity(t *testing.T, pureProtocol bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		budget := NewTransferMemoryBudget(128 * 1024)
		var retainCalls atomic.Int64
		var groupsLock sync.Mutex
		var groups []*parsedPacketGroup
		fixture := newGroupDispositionQueueFixture(t, budget, func(settings *ClientSettings) {
			settings.SendBufferSettings.beforeGroupRetainForTest = func() { retainCalls.Add(1) }
			settings.SendBufferSettings.afterGroupAdmissionForTest = func(target sendGroupAdmissionTarget) {
				groupsLock.Lock()
				defer groupsLock.Unlock()
				groups = append(groups, target.(*parsedPacketGroup))
			}
		})
		templates := make([][]byte, 32)
		packets := make([][]byte, len(templates))
		membership := make([]bool, len(templates))
		sequence := uint32(100)
		for index := range templates {
			var payload []byte
			if !pureProtocol && index%2 == 0 {
				payload = []byte{byte(index), 1, 2, 3}
			}
			templates[index] = groupRecoveryPacket(sequence, 500, uint16(64+index), tcpFlagAck, payload)
			sequence += uint32(len(payload))
			packets[index] = MessagePoolCopy(templates[index])
			fixture.witnesses = append(fixture.witnesses, MessagePoolShareReadOnly(packets[index]))
		}
		before := time.Now()
		count := fixture.parent.SendNativePacketBatchWithResults(fixture.source, fixture.parent.provideMode,
			packets, 5*time.Second, membership)
		if count != len(templates) {
			t.Fatalf("partition control lost whole input admission: count=%d membership=%v", count, membership)
		}
		for index, admitted := range membership {
			if !admitted {
				t.Fatalf("partition control lost original membership %d", index)
			}
		}
		fixture.requireParked()
		groupsLock.Lock()
		admittedGroups := append([]*parsedPacketGroup(nil), groups...)
		groupsLock.Unlock()
		wantOwners, wantDataOwners, wantChunks := 32, 16, 32
		if pureProtocol {
			wantOwners, wantDataOwners, wantChunks = 1, 0, 16
		}
		if len(admittedGroups) != wantOwners || budget.UsedByteCount() != 0 {
			t.Fatalf("pre-Run partition custody changed: owners=%d want=%d used=%d", len(admittedGroups), wantOwners, budget.UsedByteCount())
		}
		source := admittedGroups[0].collapseSource
		cost := nativeMixedAckPartitionRetainedCost(t, source, templates)
		if cost.owners != wantOwners || cost.dataOwners != wantDataOwners || len(cost.chunks) != wantChunks || cost.metadata <= 0 ||
			pureProtocol && (cost.total != 0 || cost.dataMetadata != 0) ||
			!pureProtocol && (cost.total <= 0 || cost.dataMetadata <= 0 || cost.dataMetadata*2 != cost.metadata) {
			t.Fatalf("partition calibration lost independent class accounting: pure=%t cost=%+v", pureProtocol, cost)
		}
		graphCharges := make([]ByteCount, len(admittedGroups))
		var independentlyCharged ByteCount
		originalIndex := 0
		for index, group := range admittedGroups {
			if group.collapseSource != source || group.collapseOwner != nil ||
				group.completionFlags.Load()&(groupCompletionTracked|groupCompletionAdmitted|groupCompletionReturned) !=
					groupCompletionTracked|groupCompletionAdmitted|groupCompletionReturned {
				t.Fatalf("owner %d lost pre-Run tracked custody", index)
			}
			wantMembers := 1
			if pureProtocol {
				wantMembers = 32
			}
			if len(group.packets) != wantMembers || cap(group.packets) != wantMembers {
				t.Fatalf("owner %d retained another run's descriptors: len=%d cap=%d", index, len(group.packets), cap(group.packets))
			}
			for _, packet := range group.packets {
				if !bytes.Equal(packet.packet, templates[originalIndex]) || packet.ipPath == nil {
					t.Fatalf("partition lost original source order or path at %d", originalIndex)
				}
				originalIndex++
			}
			graphCharges[index] = group.sendGroupMemoryByteCount()
			if graphCharges[index] <= 0 {
				t.Fatalf("owner %d lost descriptive graph charge", index)
			}
			if !pureProtocol && index%2 == 0 {
				// The input pattern supplies this DATA oracle independently of
				// the protocol classifier used by the projection and producer.
				dataCost := nativeMixedAckFlightRetainedCost(t, source, templates[index:index+1])
				independentlyCharged += dataCost.total
			}
		}
		if originalIndex != len(templates) || cost.total != independentlyCharged {
			t.Fatalf("projection undercounted independent DATA owners: total=%d want=%d members=%d", cost.total, independentlyCharged, originalIndex)
		}
		budget.SetTotalByteCount(cost.total)
		fixture.unpark()
		synctest.Wait()
		if budget.UsedByteCount() != independentlyCharged || budget.Available() != 0 || len(fixture.route) != wantChunks ||
			retainCalls.Load() != int64(wantChunks) || fixture.terminal.Load() != 0 || source.resendMemoryGrant != 0 {
			t.Fatalf("exact partition flight was refused or mischarged: pure=%t used=%d want=%d route=%d retain=%d terminal=%d",
				pureProtocol, budget.UsedByteCount(), independentlyCharged, len(fixture.route), retainCalls.Load(), fixture.terminal.Load())
		}
		for index, group := range admittedGroups {
			owner := group.collapseOwner
			wantMaterialized := uint64(1)<<uint(len(group.packets)) - 1
			if owner == nil || owner.remaining != 0 || owner.materialized != wantMaterialized || owner.disposed != 0 ||
				owner.budgetBytes != graphCharges[index] || group.completionFlags.Load()&groupCompletionTerminal != 0 {
				t.Fatalf("owner %d lost its graph before peer ACK", index)
			}
			if !pureProtocol && index%2 == 0 {
				if owner.budget != budget {
					t.Fatalf("DATA owner %d bypassed retained permission", index)
				}
			} else if owner.budget != nil {
				t.Fatalf("protocol owner %d acquired DATA permission", index)
			}
		}
		queued, _, _ := fixture.client.ResendQueueSize(fixture.peer, MultiHopId{}, false, false)
		if queued != wantChunks || time.Since(before) != 0 {
			t.Fatal("partition control lost retained items or advanced time before peer ACK")
		}
		fixture.acknowledgeAll(templates...)
		reserved, released := budget.Counts()
		queued, queuedBytes, _ := fixture.client.ResendQueueSize(fixture.peer, MultiHopId{}, false, false)
		if fixture.terminal.Load() != int64(wantOwners) || budget.UsedByteCount() != 0 || reserved != released || queued != 0 || queuedBytes != 0 || time.Since(before) != 0 {
			t.Fatal("decoded peer ACKs did not settle all partition owners and exact DATA charges")
		}
		for index, group := range admittedGroups {
			if group.completionFlags.Load()&groupCompletionTerminal == 0 || group.collapseOwner.budgetBytes != 0 {
				t.Fatalf("terminal owner %d retained graph permission", index)
			}
		}
		if err := fixture.closeClient(t.Context()); err != nil {
			t.Fatal(err)
		}
		for index, witness := range fixture.witnesses {
			fixture.witnesses[index] = nil
			if !MessagePoolReturn(witness) {
				t.Errorf("joined partition control kept original packet %d", index)
			}
		}
		t.Logf("NATIVE_MIXED_ACK_PARTITION_CAPACITY pure_protocol=%t owners=%d data_owners=%d wire_members=32 wire_chunks=%d graph_metadata=%d charged_data_metadata=%d charged_data_bytes=%d terminals=%d elapsed=%s",
			pureProtocol, wantOwners, wantDataOwners, wantChunks, cost.metadata, cost.dataMetadata, independentlyCharged, fixture.terminal.Load(), time.Since(before))
	})
}
