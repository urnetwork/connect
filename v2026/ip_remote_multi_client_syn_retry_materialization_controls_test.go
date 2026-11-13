// Real grouped Transfer sources pin control-range receipts, capability lane
// replacement and partial materialization beside the public-native causal root.
package connect

import (
	"bytes"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/v2026/protocol"
)

type synCacheMaterializationEvent struct {
	group        *parsedPacketGroup
	sourceId     Id
	start, end   int
	order        uint64
	receiptOrder uint64
	receiptIndex int
}

// The existing post-disposition seam owns no packet here; copy scalar proof
// under the same flow lock, before peer ACK can release the group's range.
func synCacheMaterializationSnapshot(disposition sendGroupDisposition, source *SendSequence) synCacheMaterializationEvent {
	group, ok := disposition.target.(*parsedPacketGroup)
	if !ok || group.collapseAdmission.update == nil {
		return synCacheMaterializationEvent{}
	}
	update := group.collapseAdmission.update
	update.stateLock.Lock()
	defer update.stateLock.Unlock()
	return synCacheMaterializationEvent{group: group, sourceId: source.sequenceId, start: disposition.start, end: disposition.end,
		order: group.collapseOrder(), receiptOrder: update.synRetryMaterializedOrder, receiptIndex: update.synRetryMaterializedIndex}
}

func testSynCacheMixedControlRanges(t *testing.T, flags byte) {
	t.Helper()
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		budget := NewTransferMemoryBudget(kib(64))
		t.Cleanup(func() {
			r, l := budget.Counts()
			if budget.UsedByteCount() != 0 || r != l {
				t.Error("mixed source left transfer permission after join")
			}
		})
		events := make(chan synCacheMaterializationEvent, 4)
		f := newGroupDispositionQueueFixture(t, budget, func(settings *ClientSettings) {
			settings.SendBufferSettings.afterGroupDispositionForTest = func(source *SendSequence, disposition sendGroupDisposition, _ []*protocol.Frame) {
				event := synCacheMaterializationSnapshot(disposition, source)
				if event.group != nil {
					select {
					case events <- event:
					default:
						t.Error("materialization range observer exceeded its exact four-record bound")
					}
				}
			}
		})
		syn := groupDispositionControlPacket(1000, 0, 64, tcpFlagSyn)
		fin := groupDispositionControlPacket(1001, 500, 64, flags)
		next := groupDispositionControlPacket(2000, 0, 64, tcpFlagSyn)
		if sendPackBatchMaxFrames != 2 || f.offer(syn, fin, next) != 3 {
			t.Fatal("actual three-control group was not admitted under its original two-frame chunk bound")
		}
		f.requireParked()
		f.unpark()
		f.acknowledgeAll(syn, fin, next)
		if len(events) != 2 {
			t.Fatal("real group did not materialize exactly two physical ranges")
		}
		first, last := <-events, <-events
		// The public parser starts a new descriptor at the next SYN. Each
		// actual group must preserve its own last written control position.
		if first.group == last.group || first.sourceId != last.sourceId || first.group.synRetry != nil || last.group.synRetry != nil ||
			first.start != 0 || first.end != 2 || last.start != 0 || last.end != 1 || first.order == 0 || last.order <= first.order ||
			first.receiptOrder != first.order || first.receiptIndex != 1 || last.receiptOrder != last.order || last.receiptIndex != 0 {
			t.Fatal("SYN_CACHE_MATERIALIZED_RANGE_INDEX_LOST: real source groups lost their exact control position or dequeue order")
		}
	})
}

func TestNativeSynCacheMixedControlRangesKeepLastWrittenIndex(t *testing.T) {
	testSynCacheMixedControlRanges(t, tcpFlagFin|tcpFlagAck)
}
func TestNativeSynCacheRstWithoutRetryRequestFencesOlderControl(t *testing.T) {
	testSynCacheMixedControlRanges(t, tcpFlagRst|tcpFlagAck)
}

// A later source frame can fail at a real contract boundary after the physical
// prefix is retained. Its disposed SYN must not advance successful authority.
func TestNativeSynCacheUnwrittenSuffixCannotAdvanceMaterialization(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		budget := NewTransferMemoryBudget(kib(64))
		t.Cleanup(func() {
			r, l := budget.Counts()
			if budget.UsedByteCount() != 0 || r != l {
				t.Error("partial source left transfer permission after join")
			}
		})
		var updates atomic.Int32
		var event synCacheMaterializationEvent
		var materializedSource *SendSequence
		f := newGroupDispositionQueueFixture(t, budget, func(settings *ClientSettings) {
			settings.SendBufferSettings.forceContractFailureForTest = func(sendSequenceId) bool { return updates.Add(1) == 2 }
			settings.SendBufferSettings.afterGroupDispositionForTest = func(source *SendSequence, disposition sendGroupDisposition, _ []*protocol.Frame) {
				materializedSource = source
				event = synCacheMaterializationSnapshot(disposition, source)
			}
		})
		syn := groupDispositionControlPacket(1000, 0, 64, tcpFlagSyn)
		body := groupRecoveryPacket(1001, 500, 64, tcpFlagAck, []byte{1})
		next := groupDispositionControlPacket(2000, 0, 64, tcpFlagSyn)
		if f.offer(syn, body, next) != 3 {
			t.Fatal("real partial group was not admitted")
		}
		f.requireParked()
		f.update.stateLock.Lock()
		originalHead := f.update.sequenceClaims
		var prefix, suffix *parsedPacketGroup
		for group := f.update.sequenceClaims; group != nil; group = group.collapseNext {
			if group.completionClient != f.selected || group.collapseAdmission.update != f.update {
				continue
			}
			if len(group.packets) == 2 && group.packets[0].ipPath.Syn && group.packets[0].ipPath.SequenceNumber == 1000 &&
				!group.packets[1].ipPath.Syn && group.packets[1].ipPath.SequenceNumber == 1001 && group.packets[1].ipPath.TcpPayloadByteCount == 1 {
				prefix = group
			}
			if len(group.packets) == 1 && group.packets[0].ipPath.Syn && group.packets[0].ipPath.SequenceNumber == 2000 {
				suffix = group
			}
		}
		genuineGroups := prefix != nil && suffix != nil && prefix != suffix
		f.update.stateLock.Unlock()
		if !genuineGroups {
			t.Fatal("public parser did not retain the exact physical-prefix and pending-SYN descriptors")
		}
		f.unpark()
		synctest.Wait()
		if len(f.route) != 1 || materializedSource == nil {
			t.Fatal("contract failure lost its one actual two-frame physical prefix")
		}
		physical, lateAck := false, false
		func() {
			wire := <-f.route
			defer MessagePoolReturn(wire)
			pack := decodeSendPackLifecycleWirePack(t, wire)
			physical = !pack.Nack && len(pack.Frames) == 2 && bytes.Equal(pack.Frames[0].MessageBytes, syn) && bytes.Equal(pack.Frames[1].MessageBytes, body)
			lateAck = f.client.sendBuffer.Ack(f.peer, &protocol.Ack{MessageId: pack.MessageId, SequenceId: pack.SequenceId}, 0)
		}()
		sourceDone := false
		select {
		case <-materializedSource.done:
			sourceDone = true
		default:
		}
		resend, _, _, _ := materializedSource.ResendQueueSizeAndMessageTypes()
		if !physical || lateAck || !sourceDone || materializedSource.ctx.Err() == nil || resend != 0 {
			t.Fatal("real failed sender must retire its physical prefix before a late peer ACK")
		}
		f.update.stateLock.Lock()
		oldValid := originalHead != nil && originalHead.collapseOwner == nil && event.group == originalHead && event.start == 0 && event.end == 2 && event.order != 0 &&
			f.update.synRetryMaterializedOrder == event.order && f.update.synRetryMaterializedIndex == 0
		prefixOwner := prefix.collapseOwner
		prefixFlags := prefix.completionFlags.Load()
		// Retirement leaves scalar metadata available for source proof. Its
		// budget and bytes, rather than the pointer, must have been released.
		prefixRetired := prefixOwner != nil && prefixOwner.remaining == 0 && prefixOwner.materialized == 3 && prefixOwner.disposed == 0 &&
			prefixOwner.budget == nil && prefixOwner.budgetBytes == 0 &&
			prefixFlags&(groupCompletionReturned|groupCompletionDone|groupCompletionTerminal) == groupCompletionReturned|groupCompletionDone|groupCompletionTerminal
		prefixValid := prefixRetired && event.group == prefix && event.start == 0 && event.end == 2 && event.order != 0 &&
			f.update.synRetryMaterializedOrder == event.order && f.update.synRetryMaterializedIndex == 0
		suffixFlags := suffix.completionFlags.Load()
		suffixOrder := suffix.collapseOrder()
		suffixDisposed := suffixOrder > event.order && suffix.affinitySourceState&affinitySourceMaterialized == 0 &&
			suffixFlags&(groupCompletionDone|groupCompletionTerminal) == groupCompletionDone|groupCompletionTerminal
		f.update.stateLock.Unlock()
		reserved, released := budget.Counts()
		used := budget.UsedByteCount()
		// This diagnostic binds the former composite's exact failing clause;
		// it never treats intrusive list position as source authority.
		t.Logf("SYN_CACHE_PARTIAL_CORRECTION_SCALAR original_head_is_prefix=%t original_valid=%t prefix_retired=%t prefix_valid=%t suffix_disposed=%t suffix_order=%d receipt_order=%d receipt_index=%d updates=%d terminal=%d live_used=%d reserved=%d released=%d", originalHead == prefix, oldValid, prefixRetired, prefixValid, suffixDisposed, suffixOrder, event.receiptOrder, event.receiptIndex, updates.Load(), f.terminal.Load(), used, reserved, released)
		if updates.Load() != 2 || f.terminal.Load() != 2 || !prefixValid || !suffixDisposed || used < 0 || reserved-released != used {
			t.Fatal("SYN_CACHE_UNWRITTEN_CONTROL_ADVANCED_RECEIPT: disposed suffix changed the physical-prefix receipt or credit conservation")
		}
		// Parent-owned control proof may remain live here. The existing
		// joined fixture cleanup below must return all bytes and counts.
	})
}

func testSynCacheTerminalControlWithoutRetryRequest(t *testing.T, flags byte) {
	t.Helper()
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		events := make(chan synCacheMaterializationEvent, 4)
		f := newGroupDispositionQueueFixture(t, nil, func(settings *ClientSettings) {
			settings.SendBufferSettings.afterGroupDispositionForTest = func(source *SendSequence, disposition sendGroupDisposition, _ []*protocol.Frame) {
				event := synCacheMaterializationSnapshot(disposition, source)
				if event.group != nil {
					events <- event
				}
			}
		})
		syn := groupDispositionControlPacket(1000, 0, 64, tcpFlagSyn)
		if f.offer(syn) != 1 {
			t.Fatal("first source SYN refused")
		}
		f.requireParked()
		f.unpark()
		f.acknowledgeAll(syn)
		first := <-events
		terminal := groupDispositionControlPacket(1001, 500, 64, flags)
		if f.offer(terminal) != 1 {
			t.Fatal("actual source close control refused")
		}
		f.acknowledgeAll(terminal)
		last := <-events
		if last.group.synRetry != nil || last.order <= first.order || last.receiptOrder != last.order || last.receiptIndex != 0 {
			t.Fatal("SYN_CACHE_TERMINAL_WITHOUT_RETRY_REQUEST_LOST_RECEIPT: real FIN/RST did not fence delayed earlier retention")
		}
	})
}

func TestNativeSynCacheFinWithoutRetryRequestFencesOlderControl(t *testing.T) {
	testSynCacheTerminalControlWithoutRetryRequest(t, tcpFlagFin|tcpFlagAck)
}

// A real capability ACK creates a new nonzero-lane source while the old
// lane-zero FIN is still queued. Its later dequeue is semantically ignored.
func TestNativeSynCacheStaleLaneControlCannotPoisonCurrentSource(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		cause := ""
		t.Cleanup(func() {
			if cause != "" && !t.Failed() {
				t.Fatal(cause)
			}
		})
		budget := NewTransferMemoryBudget(kib(64))
		t.Cleanup(func() {
			r, l := budget.Counts()
			if budget.UsedByteCount() != 0 || r != l {
				t.Error("lane replacement left transfer permission after join")
			}
		})
		oldEntered, oldRelease, currentRelease := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var oldOnce, currentOnce sync.Once
		releaseOld := func() { oldOnce.Do(func() { close(oldRelease) }) }
		releaseCurrent := func() { currentOnce.Do(func() { close(currentRelease) }) }
		var armOld atomic.Bool
		currentEvents, oldEvents := make(chan synCacheMaterializationEvent, 1), make(chan synCacheMaterializationEvent, 1)
		f := newGroupDispositionQueueFixture(t, budget, func(settings *ClientSettings) {
			settings.SendBufferSettings.LogicalDataLaneCount = 4
			settings.SendBufferSettings.beforeGroupDequeueForTest = func(source *SendSequence) {
				if source.id().LogicalLane == 0 && armOld.CompareAndSwap(true, false) {
					close(oldEntered)
					select {
					case <-oldRelease:
					case <-source.ctx.Done():
					}
				}
			}
			settings.SendBufferSettings.afterGroupDispositionForTest = func(source *SendSequence, disposition sendGroupDisposition, frames []*protocol.Frame) {
				event := synCacheMaterializationSnapshot(disposition, source)
				if event.group == nil {
					return
				}
				for _, frame := range frames {
					if frame == nil || frame.MessageType != protocol.MessageType_IpIpPacketToProvider {
						continue
					}
					packet, err := ipPacketToProviderBytes(frame)
					if err != nil {
						continue
					}
					path, err := ParseIpPath(packet)
					if err != nil {
						continue
					}
					if path.Fin {
						oldEvents <- event
					}
					if path.Syn && path.SequenceNumber == 2000 {
						currentEvents <- event
						select {
						case <-currentRelease:
						case <-source.ctx.Done():
						}
					}
				}
			}
		})
		t.Cleanup(releaseCurrent)
		t.Cleanup(releaseOld)
		first := groupDispositionControlPacket(1000, 0, 64, tcpFlagSyn)
		if f.offer(first) != 1 {
			t.Fatal("lane-zero source SYN refused")
		}
		f.requireParked()
		f.unpark()
		synctest.Wait()
		if len(f.route) != 1 {
			t.Fatal("first lane-zero physical Pack missing")
		}
		wire := <-f.route
		pack := decodeSendPackLifecycleWirePack(t, wire)
		oldSourceId := RequireIdFromBytes(pack.SequenceId)
		fin := groupDispositionControlPacket(1001, 500, 64, tcpFlagFin|tcpFlagAck)
		armOld.Store(true)
		if f.offer(fin) != 1 {
			MessagePoolReturn(wire)
			t.Fatal("old lane FIN refused before capability change")
		}
		synctest.Wait()
		select {
		case <-oldEntered:
		default:
			MessagePoolReturn(wire)
			t.Fatal("old source did not hold its genuine queued FIN before dequeue")
		}
		accepted := f.client.sendBuffer.Ack(f.peer, &protocol.Ack{MessageId: pack.MessageId, SequenceId: pack.SequenceId, LogicalLaneVersion: transferLogicalLaneVersion}, 0)
		MessagePoolReturn(wire)
		if !accepted {
			t.Fatal("real first-Pack capability ACK was refused")
		}
		synctest.Wait()
		next := groupDispositionControlPacket(2000, 0, 64, tcpFlagSyn)
		if f.offer(next) != 1 {
			t.Fatal("negotiated new-lane SYN refused")
		}
		synctest.Wait()
		var current synCacheMaterializationEvent
		select {
		case current = <-currentEvents:
		default:
			t.Fatal("new-lane SYN did not reach actual post-disposition ownership")
		}
		if !oldSourceId.LessThan(current.sourceId) {
			t.Fatal("actual capability transition did not create a newer source identity")
		}
		releaseOld()
		synctest.Wait()
		var old synCacheMaterializationEvent
		select {
		case old = <-oldEvents:
		default:
			t.Fatal("released old lane did not physically materialize its FIN")
		}
		f.update.stateLock.Lock()
		ignored := old.sourceId == oldSourceId && current.sourceId == f.update.sequenceSourceId && old.order > current.order && !f.update.egressFinSeen && f.update.synGenerationNumber == 2000 && !f.update.IsDone()
		owned := current.group.currentSynRetryControlMaterializationWithLock(0)
		f.update.stateLock.Unlock()
		if !ignored {
			t.Fatal("older real source was not semantically ignored despite its larger dequeue order")
		}
		if !owned {
			cause = "SYN_CACHE_STALE_LANE_CONTROL_POISONED_CURRENT_RECEIPT: ignored old source advanced successful-current authority"
		}
		synCacheAcknowledgeNegotiatedFrames(t, f, fin)
		releaseCurrent()
		synCacheAcknowledgeNegotiatedFrames(t, f, next)
	})
}

// These actual ACKs preserve the already-negotiated capability while another
// lane is held. An omitted version would withdraw it and cancel that lane.
func synCacheAcknowledgeNegotiatedFrames(t *testing.T, f *groupDispositionQueueFixture, templates ...[]byte) {
	t.Helper()
	synctest.Wait()
	seen := 0
	for len(f.route) != 0 {
		wire := <-f.route
		pack := decodeSendPackLifecycleWirePack(t, wire)
		if pack.Nack {
			MessagePoolReturn(wire)
			t.Fatal("negotiated source lost reliable physical retention")
		}
		for _, frame := range pack.Frames {
			if seen >= len(templates) || !bytes.Equal(frame.MessageBytes, templates[seen]) {
				MessagePoolReturn(wire)
				t.Fatal("negotiated physical source differs from its original packet order")
			}
			seen++
		}
		accepted := f.client.sendBuffer.Ack(f.peer, &protocol.Ack{MessageId: pack.MessageId, SequenceId: pack.SequenceId, LogicalLaneVersion: transferLogicalLaneVersion}, 0)
		MessagePoolReturn(wire)
		if !accepted {
			t.Fatal("actual negotiated peer ACK was refused")
		}
	}
	if seen != len(templates) {
		t.Fatalf("negotiated physical source wrote %d originals, want %d", seen, len(templates))
	}
	synctest.Wait()
}
