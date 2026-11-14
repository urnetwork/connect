// Remote refusal identity survives local channel retirement only inside the
// exact paid unanswered-SYN owner. Real packets establish the ledger premise.
package connect

import (
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"unsafe"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Both actual providers refuse the original SYN. A later source generation
// can make both silent, so no new refusal depends on provider selection order.
func newSynRetryRemoteRefusalFixture(t *testing.T, laterSilent *atomic.Bool, applicationData ...bool) (*reraceHarness, *IpPath, *multiClientChannelUpdate) {
	t.Helper()
	h := newReraceHarness(t, []reraceExitSpec{
		{name: "refused-first", tier: 0, answer: func(*IpPath) reraceAnswer {
			if laterSilent != nil && laterSilent.Load() {
				return reraceSilent
			}
			return reraceSignal
		}},
		{name: "refused-second", tier: 1, answer: func(*IpPath) reraceAnswer {
			if laterSilent != nil && laterSilent.Load() {
				return reraceSilent
			}
			return reraceSignal
		}},
	}, func(settings *MultiClientSettings) {
		settings.WindowSizes[WindowTypeQuality] = WindowSizeSettings{WindowSizeMin: 2, WindowSizeMax: 2, WindowSizeHardMax: 2}
		settings.StandingReserve, settings.EvaluationPoolMultiple = false, 1
	})
	h.settle()
	window := h.multiClient.windows[WindowTypeQuality]
	window.stateLock.Lock()
	initialAcquisitionDone := window.discoveryDemand.owner == nil
	window.stateLock.Unlock()
	if !initialAcquisitionDone {
		t.Fatal("COMMON_REMOTE_FIXTURE_ACQUISITION_NOT_FINISHED: the exact two-provider initial pass must complete before a retirement edge")
	}
	path := unboundFlowTcpPath(52991)
	accepted := false
	if len(applicationData) != 0 && applicationData[0] {
		packet := MessagePoolCopy(reraceTcpPacket(path, tcpFlagSyn, path.SequenceNumber, 0, []byte{0x64}))
		accepted = synRetryControlOffer(t, h, packet)
	} else {
		accepted = privateCrossWindowNativeSyn(h, path)
	}
	if !accepted {
		t.Fatal("original native source was not admitted")
	}
	update := h.update(path)
	if update == nil {
		t.Fatal("native source lost its canonical flow")
	}
	update.stateLock.Lock()
	valid := update.synRetryHasRemoteRefusalWithLock() && update.synRetry != nil &&
		len(update.synRetry.refusedRemoteIds) == 2 && len(update.dialFailedClients) == 2 &&
		len(update.dialFailureSignaledClients) == 2 && !update.receivedInbound.Load()
	update.stateLock.Unlock()
	if !valid || h.metrics().DialFailuresIntercepted != 2 {
		t.Fatal("remote-history control lacks two actual standing provider refusals")
	}
	return h, path, update
}

// Removing a local channel still prunes its old pointer references. The paid
// source retains the remote identity needed to reject a newly minted alias.
func TestSynRetryRemoteRefusalSurvivesLocalChannelRetirement(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		h, _, update := newSynRetryRemoteRefusalFixture(t, nil)
		old := h.channel("refused-first")
		alias := &multiClientChannel{args: &multiClientChannelArgs{Destination: old.Destination()}}
		old.Close()
		h.settle()
		window := h.multiClient.windows[WindowTypeQuality]
		window.stateLock.Lock()
		stillInstalled := window.clients[old.ClientId()] == old
		acquiring := window.discoveryDemand.owner != nil
		window.stateLock.Unlock()
		t.Logf("COMMON_REMOTE_RETIRE_BOUNDARY old_done=%t still_installed=%t acquiring=%t", old.IsDone(), stillInstalled, acquiring)
		update.stateLock.Lock()
		defer update.stateLock.Unlock()
		if slices.Contains(update.dialFailedClients, old) || slices.Contains(update.dialFailureSignaledClients, old) {
			t.Fatal("retired local pointer was retained by ordinary failure lists")
		}
		if !update.synRetryHasRemoteRefusalWithLock() || !update.synRetryRemoteTailRefusedWithLock(alias) {
			t.Fatal("SYN_RETRY_RETIRED_REMOTE_REFUSAL_LOST: a fresh local alias erased the same source's actual remote refusal")
		}
	})
}

// For a DATA-bearing SYN, backing and the descriptor are charged before construction, and a
// terminal source returns the backing together with the exact credit owner.
func TestSynRetryRemoteRefusalBackingIsPaidAndReleased(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		h, _, update := newSynRetryRemoteRefusalFixture(t, nil, true)
		owner := synRetryControlOwner(t, update)
		capacity := synRetryRemoteTailCapacity(h.multiClient, update)
		metadata := ByteCount(unsafe.Sizeof(tcpSynRetry{})) + ByteCount(len(owner.pin.appId))
		metadata = (metadata + 63) / 64 * 64
		metadata = addReceiveQueueByteCount(metadata, retainedMessageCapacity(ByteCount(capacity)*ByteCount(unsafe.Sizeof(Id{}))))
		wanted := addReceiveQueueByteCount(retainedMessageCapacity(ByteCount(len(owner.packet))), metadata)
		wanted = addReceiveQueueByteCount(wanted, transferRetainedControlCreditByteCount())
		credit := owner.credit
		if protocolClassPureTcpControl(owner.packet) || credit.noBudget || cap(owner.refusedRemoteIds) != capacity || credit.byteCount != wanted || credit.released.Load() {
			t.Fatalf("SYN_RETRY_REMOTE_BACKING_UNPAID: capacity=%d credit=%d wanted=%d", cap(owner.refusedRemoteIds), credit.byteCount, wanted)
		}
		update.Close()
		h.settle()
		if owner.refusedRemoteIds != nil || owner.remoteRefusalOverflow || owner.credit != nil || !credit.released.Load() {
			t.Fatal("terminal source retained remote history or its paid credit")
		}
	})
}

// A new source sequence owns a fresh ledger. Both providers become silent
// before its actual packet, so neither old refusal can survive in that ledger.
func TestSynRetryRemoteRefusalResetsWithActualNewSyn(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		var laterSilent atomic.Bool
		h, path, update := newSynRetryRemoteRefusalFixture(t, &laterSilent)
		old := synRetryControlOwner(t, update)
		first, second := h.channel("refused-first"), h.channel("refused-second")
		laterSilent.Store(true)
		next := *path
		next.SequenceNumber++
		if !privateCrossWindowNativeSyn(h, &next) {
			t.Fatal("new native source generation was not admitted")
		}
		update.stateLock.Lock()
		current := update.synRetry
		valid := current != nil && current != old && current.sequence == next.SequenceNumber &&
			len(current.refusedRemoteIds) == 0 && !update.synRetryHasRemoteRefusalWithLock() &&
			!update.synRetryRemoteTailRefusedWithLock(first) && !update.synRetryRemoteTailRefusedWithLock(second)
		present, distinct, sameSequence := current != nil, current != nil && current != old, current != nil && current.sequence == next.SequenceNumber
		refusals, historyCount, currentRefs := update.synRetryHasRemoteRefusalWithLock(), 0, uint32(0)
		if current != nil {
			historyCount, currentRefs = len(current.refusedRemoteIds), current.references
		}
		secondRefused := update.synRetryRemoteTailRefusedWithLock(second)
		update.stateLock.Unlock()
		firstNew, secondNew := 0, 0
		for _, packet := range h.exit("refused-first").syns(&next) {
			if packet.path.SequenceNumber == next.SequenceNumber {
				firstNew++
			}
		}
		for _, packet := range h.exit("refused-second").syns(&next) {
			if packet.path.SequenceNumber == next.SequenceNumber {
				secondNew++
			}
		}
		t.Logf("COMMON_REMOTE_NEW_SYN_BOUNDARY present=%t distinct=%t same_sequence=%t has_refusal=%t history=%d refs=%d second_refused=%t old_history_released=%t old_credit_released=%t first_actual_new_syns=%d second_actual_new_syns=%d", present, distinct, sameSequence, refusals, historyCount, currentRefs, secondRefused, old.refusedRemoteIds == nil, old.credit == nil, firstNew, secondNew)
		if firstNew+secondNew == 0 {
			t.Fatal("COMMON_REMOTE_NEW_SYN_WIRE_PREMISE: the new native generation must actually reach a provider")
		}
		if !valid || old.refusedRemoteIds != nil || old.credit != nil {
			t.Fatal("new actual SYN inherited a retired generation's remote refusal or backing")
		}
	})
}

// Overflow cannot turn unknown remote identity into a fresh automatic route.
// Unit insertions fill the fixed ledger after real packets establish its owner.
func TestSynRetryRemoteRefusalOverflowKeepsItsOriginalBacking(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		h, _, update := newSynRetryRemoteRefusalFixture(t, nil)
		owner := synRetryControlOwner(t, update)
		capacity, paid := cap(owner.refusedRemoteIds), owner.credit.byteCount
		update.stateLock.Lock()
		for index := 0; index <= capacity; index++ {
			remote := &multiClientChannel{args: &multiClientChannelArgs{Destination: RequireMultiHopId(NewId())}}
			update.noteSynRetryRemoteRefusalWithLock(remote)
		}
		fresh := &multiClientChannel{args: &multiClientChannelArgs{Destination: RequireMultiHopId(NewId())}}
		bounded := owner.remoteRefusalOverflow && len(owner.refusedRemoteIds) == capacity &&
			cap(owner.refusedRemoteIds) == capacity && owner.credit.byteCount == paid &&
			update.synRetryRemoteTailRefusedWithLock(fresh)
		update.stateLock.Unlock()
		if !bounded {
			t.Fatal("remote refusal overflow grew backing or silently granted automatic freshness")
		}
		update.Close()
		h.settle()
		if owner.refusedRemoteIds != nil || owner.remoteRefusalOverflow || owner.credit != nil {
			t.Fatal("overflow owner did not clear and release on source cancellation")
		}
	})
}

// Impossible configured geometry refuses optional history before allocation.
func TestSynRetryRemoteRefusalRejectsOverflowedCapacity(t *testing.T) {
	maximum := int(^uint(0) >> 1)
	parent := &RemoteUserNatMultiClient{settings: &MultiClientSettings{
		WindowSizeLimit: maximum,
	}, windows: map[WindowType]*multiClientWindow{WindowTypeQuality: {}, WindowTypeSpeed: {}}}
	if synRetryRemoteTailCapacity(parent, &multiClientChannelUpdate{}) != 0 {
		t.Fatal("overflowed source geometry was accepted for backing allocation")
	}
}

// Actual materialization must pay the enlarged owner and backing before copy
// or make. A shared parent one byte short refuses without a queue-floor waiver.
func TestSynRetryRemoteRefusalBackingRejectsOneByteShortParent(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		root := NewTransferMemoryBudget(kib(64))
		budget := NewTransferMemoryBudgetWithParent(kib(64), root)
		t.Cleanup(func() {
			for _, ledger := range []*TransferMemoryBudget{budget, root} {
				r, l := ledger.Counts()
				if ledger.UsedByteCount() != 0 || r != l {
					t.Error("actual source left retained permission after join")
				}
			}
		})
		type snapshot struct {
			base, total, reserved ByteCount
			capacity              int
		}
		events := make(chan snapshot, 2)
		var stage atomic.Int32
		f := newGroupDispositionQueueFixture(t, budget, func(settings *ClientSettings) {
			settings.SendBufferSettings.afterGroupDispositionForTest = func(source *SendSequence, disposition sendGroupDisposition, frames []*protocol.Frame) {
				group, ok := disposition.target.(*parsedPacketGroup)
				if !ok || group.synRetry == nil {
					return
				}
				update := group.collapseAdmission.update
				update.stateLock.Lock()
				parent := update.sequenceParentWithLock()
				capacity := synRetryRemoteTailCapacity(parent, update)
				update.stateLock.Unlock()
				metadata := ByteCount(unsafe.Sizeof(tcpSynRetry{}))
				metadata = (metadata + 63) / 64 * 64
				metadata = addReceiveQueueByteCount(metadata, retainedMessageCapacity(ByteCount(capacity)*ByteCount(unsafe.Sizeof(Id{}))))
				packet, err := ipPacketToProviderBytes(frames[0])
				if err != nil {
					t.Error(err)
					return
				}
				total := addReceiveQueueByteCount(retainedMessageCapacity(ByteCount(len(packet))), metadata)
				total = addReceiveQueueByteCount(total, transferRetainedControlCreditByteCount())
				base := root.UsedByteCount()
				if base+total > kib(64) {
					t.Error("single real group plus complete optional descriptor exceeds the existing bounded fixture envelope")
					return
				}
				budget.SetTotalByteCount(base + total)
				if stage.Add(1) == 1 {
					root.SetTotalByteCount(base + total - 1)
				} else {
					root.SetTotalByteCount(base + total)
				}
				reserved, _ := root.Counts()
				events <- snapshot{base: base, total: total, reserved: reserved, capacity: capacity}
			}
		})
		// Enable the existing cache path on this real selected-source fixture;
		// no reaper work or materialization token is fabricated by the test.
		f.parent.flowReaperDone = make(chan struct{})
		first := groupRecoveryPacket(1000, 0, 64, tcpFlagSyn, []byte{0x64})
		if f.offer(first) != 1 {
			t.Fatal("short-cache source lacked actual packet admission")
		}
		f.requireParked()
		f.unpark()
		synctest.Wait()
		if len(events) != 1 || len(f.route) != 1 {
			t.Fatal("short-cache source did not really materialize its Pack")
		}
		short := <-events
		f.update.stateLock.Lock()
		refused := f.update.synRetry == nil
		f.update.stateLock.Unlock()
		if !refused || root.UsedByteCount() != short.base || budget.UsedByteCount() != short.base {
			t.Fatal("SYN_RETRY_REMOTE_BACKING_ESCAPED_SHORT_PARENT: owner/backing acquired without full permission")
		}
		f.acknowledgeAll(first)
		if root.UsedByteCount() != 0 || budget.UsedByteCount() != 0 {
			t.Fatal("real first Pack ACK did not return its separate source owners")
		}
		root.SetTotalByteCount(kib(64))
		budget.SetTotalByteCount(kib(64))
		synctest.Wait()
		next := groupRecoveryPacket(1001, 0, 64, tcpFlagSyn, []byte{0x64})
		if f.offer(next) != 1 {
			t.Fatal("exact-cache source lacked actual packet admission")
		}
		synctest.Wait()
		if len(events) != 1 || len(f.route) != 1 {
			t.Fatal("exact-cache source did not really materialize its Pack")
		}
		exact := <-events
		f.update.stateLock.Lock()
		owner := f.update.synRetry
		valid := owner != nil && cap(owner.refusedRemoteIds) == exact.capacity && owner.credit != nil && owner.credit.byteCount == exact.total && owner.references == 1 && owner.readiness == nil
		f.update.stateLock.Unlock()
		reserved, _ := root.Counts()
		if !valid || reserved-exact.reserved != exact.total || root.UsedByteCount() != exact.base+exact.total || budget.UsedByteCount() != exact.base+exact.total {
			t.Fatal("exact enlarged-owner permission did not fund the whole descriptor and fixed backing")
		}
		// No extra permission was granted for the independent optional readiness
		// owner. This boundary tests the cache graph; that supplement declines.
		f.acknowledgeAll(next)
		if root.UsedByteCount() != exact.total || budget.UsedByteCount() != exact.total {
			t.Fatal("cache permission was confused with the separately ACKed source Pack")
		}
		f.update.stateLock.Lock()
		f.update.clearSynRetryWithLock()
		f.update.stateLock.Unlock()
		f.update.releaseSynRetryMemory()
		if owner.refusedRemoteIds != nil || owner.credit != nil || root.UsedByteCount() != 0 || budget.UsedByteCount() != 0 {
			t.Fatal("exact enlarged-owner admission did not return all backing and permission")
		}
		for _, ledger := range []*TransferMemoryBudget{budget, root} {
			r, l := ledger.Counts()
			if r != l {
				t.Fatal("short/exact actual-source admission changed credit conservation")
			}
		}
	})
}

// Inference is the existing silence path's policy verdict, not a provider's
// actual refusal. It cannot grant the new automatic remote-failure authority.
func TestSynRetryRemoteRefusalDoesNotRecordInference(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		h := newReraceHarness(t, []reraceExitSpec{
			{name: "waiting", tier: 0, answer: reraceAlways(reraceSilent)},
			{name: "other", tier: 1, answer: reraceAlways(reraceSilent)},
		}, nil)
		path := unboundFlowTcpPath(52993)
		if !privateCrossWindowNativeSyn(h, path) {
			t.Fatal("inference control's actual native source was not admitted")
		}
		update, waiting := h.update(path), h.channel("waiting")
		h.multiClient.clientDialFailure(waiting, path, false)
		h.settle()
		update.stateLock.Lock()
		defer update.stateLock.Unlock()
		if !slices.Contains(update.dialFailedClients, waiting) || update.synRetryHasRemoteRefusalWithLock() ||
			len(update.synRetry.refusedRemoteIds) != 0 {
			t.Fatal("inferred local verdict fabricated actual remote refusal authority")
		}
	})
}

// A real signal from an unrelated local provider has no standing on this SYN.
// The existing signaled-pointer observation alone must not enter paid history.
func TestSynRetryRemoteRefusalDoesNotRecordStaleProviderSignal(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		h := newReraceHarness(t, []reraceExitSpec{
			{name: "waiting", tier: 0, answer: reraceAlways(reraceSilent)},
			{name: "unrelated", tier: 1, answer: reraceAlways(reraceSilent)},
		}, nil)
		path := unboundFlowTcpPath(52994)
		if !privateCrossWindowNativeSyn(h, path) {
			t.Fatal("stale-signal control's actual native source was not admitted")
		}
		update, unrelated := h.update(path), h.channel("unrelated")
		h.inject("unrelated", reraceProviderSignal(t, path))
		update.stateLock.Lock()
		defer update.stateLock.Unlock()
		if !slices.Contains(update.dialFailureSignaledClients, unrelated) ||
			slices.Contains(update.dialFailedClients, unrelated) || update.synRetryHasRemoteRefusalWithLock() ||
			len(update.synRetry.refusedRemoteIds) != 0 {
			t.Fatal("stale provider signal entered current SYN's actual standing remote-refusal history")
		}
	})
}
