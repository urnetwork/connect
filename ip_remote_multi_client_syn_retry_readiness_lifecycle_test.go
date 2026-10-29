// Real retained source ACKs drive readiness; public native and receive entries
// exercise stale generations without assigning production capacity or clocks.
package connect

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"unsafe"

	"github.com/urnetwork/connect/protocol"
)

type synRetryReadyControl struct {
	harness  *reraceHarness
	path     *IpPath
	owner    *tcpSynRetry
	update   *multiClientChannelUpdate
	original []byte
}

func newSynRetryReadyControl(t *testing.T, port int) *synRetryReadyControl {
	t.Helper()
	harness, path := synRetryControlHarness(t, port, nil, DefaultProtocolVersion)
	if !privateStaleBoundNativeSyn(t, harness, path) {
		t.Fatal("initial native SYN refused")
	}
	update := harness.update(path)
	owner := synRetryControlOwner(t, update)
	if owner.readiness == nil {
		t.Fatal("initial source could not prepay readiness metadata")
	}
	return &synRetryReadyControl{harness: harness, path: path, owner: owner, update: update, original: bytes.Clone(owner.packet)}
}

func (control *synRetryReadyControl) refuseOn(t *testing.T, credits ...*nativeBatchCreditFixture) {
	t.Helper()
	control.harness.multiClient.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel {
		clients := make([]*multiClientChannel, len(credits))
		for index, credit := range credits {
			clients[index] = credit.base.selected
		}
		return clients
	}
	control.harness.inject("a", reraceProviderSignal(t, control.path))
	synctest.Wait()
	control.update.stateLock.Lock()
	owner, ready := control.update.synRetry, control.owner.readiness
	valid := owner == control.owner && owner.references == 1 && !owner.queued && control.update.client.Load() == nil && ready != nil && ready.indexed.Load() && ready.count == len(credits) && ready.references.Load() == 2
	control.update.stateLock.Unlock()
	if !valid {
		t.Fatal("actual source refusal did not retain one quiet cache and charged readiness index")
	}
	for _, credit := range credits {
		if credit.source.resendCapacityUnavailable.waiterCount.Load() != 1 || credit.base.selected.dialFailureCount() != 0 {
			t.Fatal("refusal did not own one exact healthy source gate")
		}
	}
}

func synRetryReadyHold(t *testing.T, parent *RemoteUserNatMultiClient, arm bool) (<-chan struct{}, func()) {
	t.Helper()
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	var first atomic.Bool
	finish := func() { once.Do(func() { close(release) }) }
	t.Cleanup(finish)
	hook := func(*multiClientChannelUpdate) {
		if !first.CompareAndSwap(false, true) {
			return
		}
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-parent.ctx.Done():
		}
	}
	if arm {
		parent.beforeSynRetryReadinessArmForTest = hook
	} else {
		parent.beforeSynRetryReadinessForTest = hook
	}
	return entered, finish
}

func requireSynRetryReadyRetired(t *testing.T, ready *tcpSynRetryReadiness, credit *transferRetainedControlCredit) {
	t.Helper()
	if ready.references.Load() != 0 || ready.parent != nil || ready.update != nil || ready.credit != nil || ready.slots != nil || ready.indexed.Load() || !credit.released.Load() {
		t.Fatal("joined readiness retained metadata credit or an ownership graph")
	}
}

func (control *synRetryReadyControl) close(t *testing.T) {
	t.Helper()
	if err := control.harness.multiClient.CloseAndWait(context.Background()); err != nil {
		t.Fatal(err)
	}
	synRetryControlReleased(t, control.owner)
}

func TestNativeSynRetryReadinessDoesNotSpinAndPublishesDemand(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		credit := newNativeBatchCreditFixture(t)
		control := newSynRetryReadyControl(t, 51401)
		var demand atomic.Int32
		credit.source.afterRetainedAdmissionDemandForTest = func() { demand.Add(1) }
		started := credit.base.started.Load()
		control.refuseOn(t, credit)
		ready, paid := control.owner.readiness, control.owner.readiness.credit
		if demand.Load() != 1 {
			t.Fatal("directed registration did not notify exact source demand")
		}
		for range 3 {
			control.harness.multiClient.notifyFlowReaper()
			synctest.Wait()
		}
		if credit.base.started.Load() != started+1 || demand.Load() != 1 {
			t.Fatal("unchanged closed readiness spun another producer or demand edge")
		}
		credit.releasePeerCredit()
		if control.harness.boundTo(control.path) != credit.base.selected || credit.base.started.Load() != started+2 {
			t.Fatal("exact ready edge did not resume one original")
		}
		credit.finishPackets(control.original)
		if demand.Load() != 2 || credit.source.resendCapacityUnavailable.waiterCount.Load() != 0 {
			t.Fatal("grant withdrawal did not release exact source demand")
		}
		control.close(t)
		requireSynRetryReadyRetired(t, ready, paid)
	})
}

func TestNativeSynRetryReadinessAckBeforeRegistration(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		credit := newNativeBatchCreditFixture(t)
		control := newSynRetryReadyControl(t, 51402)
		entered, release := synRetryReadyHold(t, control.harness.multiClient, true)
		control.harness.multiClient.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel { return []*multiClientChannel{credit.base.selected} }
		control.harness.inject("a", reraceProviderSignal(t, control.path))
		synRetryControlEntered(t, entered)
		if credit.source.resendCapacityUnavailable.waiterCount.Load() != 0 {
			t.Fatal("arm barrier already installed a source waiter")
		}
		credit.releasePeerCredit()
		if credit.source.resendCapacityUnavailable.Load() {
			t.Fatal("exact ACK did not open source before registration")
		}
		release()
		synctest.Wait()
		if control.harness.boundTo(control.path) != credit.base.selected {
			t.Fatal("ACK-before-registration lost readiness")
		}
		credit.finishPackets(control.original)
		ready, paid := control.owner.readiness, control.owner.readiness.credit
		control.close(t)
		requireSynRetryReadyRetired(t, ready, paid)
	})
}

func TestNativeSynRetryReadinessSourceCancelWithdraws(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		credit := newNativeBatchCreditFixture(t)
		control := newSynRetryReadyControl(t, 51403)
		started := credit.base.started.Load()
		control.refuseOn(t, credit)
		ready, paid := control.owner.readiness, control.owner.readiness.credit
		credit.source.Cancel()
		credit.releaseCapacity()
		synctest.Wait()
		if credit.base.started.Load() != started+1 || ready.indexed.Load() || credit.source.resendCapacityUnavailable.waiterCount.Load() != 0 || ready.count != 0 {
			t.Fatal("source cancellation retried or retained a readiness slot")
		}
		control.close(t)
		requireSynRetryReadyRetired(t, ready, paid)
	})
}

func TestNativeSynRetryReadinessParentCloseWithdraws(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		credit := newNativeBatchCreditFixture(t)
		control := newSynRetryReadyControl(t, 51404)
		started := credit.base.started.Load()
		control.refuseOn(t, credit)
		ready, paid := control.owner.readiness, control.owner.readiness.credit
		control.close(t)
		if credit.source.resendCapacityUnavailable.waiterCount.Load() != 0 {
			t.Fatal("parent close retained a source FIFO place")
		}
		requireSynRetryReadyRetired(t, ready, paid)
		credit.releasePeerCredit()
		if credit.base.started.Load() != started+1 {
			t.Fatal("late ACK resurrected a joined parent")
		}
	})
}

func TestNativeSynRetryReadinessHeldGrantRejectsNewSyn(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		credit := newNativeBatchCreditFixture(t)
		control := newSynRetryReadyControl(t, 51405)
		control.refuseOn(t, credit)
		ready, paid, packetCredit := control.owner.readiness, control.owner.readiness.credit, control.owner.credit
		entered, release := synRetryReadyHold(t, control.harness.multiClient, false)
		credit.releasePeerCredit()
		synRetryControlEntered(t, entered)
		fresh := *control.path
		fresh.SequenceNumber++
		if !privateStaleBoundNativeSyn(t, control.harness, &fresh) {
			t.Fatal("fresh native generation was refused")
		}
		if !packetCredit.released.Load() || paid.released.Load() || ready.references.Load() != 1 || control.owner.packet != nil {
			t.Fatal("stopped readiness worker did not independently retain only metadata credit")
		}
		started := credit.base.started.Load()
		release()
		synctest.Wait()
		if credit.base.started.Load() != started {
			t.Fatal("old ready grant wrote after native generation reset")
		}
		credit.finishPackets(reraceTcpPacket(&fresh, tcpFlagSyn, fresh.SequenceNumber, 0, nil))
		requireSynRetryReadyRetired(t, ready, paid)
		control.close(t)
	})
}

func TestNativeSynRetryReadinessHeldGrantRejectsSameCacheRaceCycle(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		old, first, second := newNativeBatchCreditFixture(t), newNativeBatchCreditFixture(t), newNativeBatchCreditFixture(t)
		control := newSynRetryReadyControl(t, 51406)
		control.refuseOn(t, old)
		ready, paid := control.owner.readiness, control.owner.readiness.credit
		entered, release := synRetryReadyHold(t, control.harness.multiClient, false)
		old.releasePeerCredit()
		synRetryControlEntered(t, entered)
		control.update.stateLock.Lock()
		before, epoch := control.update.sequenceSourceOrder, control.update.sequenceAdmissionEpoch
		control.update.stateLock.Unlock()
		control.harness.multiClient.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel {
			return []*multiClientChannel{first.base.selected, second.base.selected}
		}
		if privateStaleBoundNativeSyn(t, control.harness, control.path) {
			t.Fatal("closed new-race sources unexpectedly accepted original")
		}
		canonical := control.harness.update(control.path)
		control.update.stateLock.Lock()
		premise := control.update == canonical && control.update.synRetry == control.owner && control.update.race == nil && control.update.sequenceSourceOrder != before && control.update.sequenceAdmissionEpoch == epoch
		control.update.stateLock.Unlock()
		if !premise {
			t.Fatal("same-cache nil/new-race/nil cycle was not composed")
		}
		started := old.base.started.Load()
		release()
		synctest.Wait()
		if old.base.started.Load() != started || control.owner.references != 1 || ready.indexed.Load() {
			t.Fatal("old grant crossed a same-cache race generation")
		}
		control.close(t)
		requireSynRetryReadyRetired(t, ready, paid)
	})
}

func synRetryReadyProof(t *testing.T, control *synRetryReadyControl, client *multiClientChannel) {
	t.Helper()
	packet := MessagePoolCopy(reraceTcpPacket(control.path.Reverse(), tcpFlagSyn|tcpFlagAck, 7001, control.path.SequenceNumber+1, nil))
	defer MessagePoolReturn(packet)
	path, err := ParseIpPath(packet)
	if err != nil {
		t.Fatal(err)
	}
	source := SourceId(client.args.Destination.Tail())
	control.harness.multiClient.clientReceivePacket(client, source, protocol.ProvideMode_Network, TransportTypeUnknown, path, packet)
	synctest.Wait()
	if control.harness.boundTo(control.path) != client {
		t.Fatal("actual returned proof did not select admitted wide-race owner")
	}
}

// A wider actual field must be fully prepaid; only its final source becomes
// ready, which catches silently retaining the first configured quota of gates.
func TestNativeSynRetryReadinessResizesForActualCandidateField(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		control := newSynRetryReadyControl(t, 51407)
		oldReady, oldCredit := control.owner.readiness, control.owner.readiness.credit
		count := len(oldReady.slots) + 1
		credits := make([]*nativeBatchCreditFixture, count)
		for index := range credits {
			credits[index] = newNativeBatchCreditFixture(t, func(settings *ClientSettings) {
				settings.SendBufferSettings.ResendQueueBudget = NewTransferMemoryBudget(64 * 1024)
			})
		}
		control.refuseOn(t, credits...)
		ready, paid := control.owner.readiness, control.owner.readiness.credit
		if ready == oldReady || len(ready.slots) != count || !oldCredit.released.Load() {
			t.Fatal("actual wider field did not acquire full replacement metadata lease")
		}
		expected := addReceiveQueueByteCount(addReceiveQueueByteCount(retainedMessageCapacity(ByteCount(unsafe.Sizeof(tcpSynRetryReadiness{}))), retainedMessageCapacity(ByteCount(cap(ready.slots))*ByteCount(unsafe.Sizeof(tcpSynRetryReadySlot{})))), transferRetainedControlCreditByteCount())
		if paid.byteCount != expected {
			t.Fatal("readiness lease did not charge full backing and owner envelopes")
		}
		last := credits[len(credits)-1]
		last.releasePeerCredit()
		last.finishPackets(control.original)
		synRetryReadyProof(t, control, last.base.selected)
		synRetryControlReleased(t, control.owner)
		requireSynRetryReadyRetired(t, ready, paid)
		control.close(t)
	})
}

func TestNativeSynRetryReadinessCoalescesSeveralReadySources(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		first, second := newNativeBatchCreditFixture(t), newNativeBatchCreditFixture(t)
		control := newSynRetryReadyControl(t, 51408)
		seedOne, seedTwo := first.base.started.Load(), second.base.started.Load()
		var selections, dispatches, arms, grants, beforeFirst, beforeSecond atomic.Int64
		first.base.selected.beforeGroupCompletionForTest = func() { beforeFirst.Add(1) }
		second.base.selected.beforeGroupCompletionForTest = func() { beforeSecond.Add(1) }
		control.harness.multiClient.beforeSynRetryDispatchForTest = func(*multiClientChannelUpdate) { dispatches.Add(1) }
		control.harness.multiClient.beforeSynRetryReadinessForTest = func(*multiClientChannelUpdate) { grants.Add(1) }
		entered, release := synRetryReadyHold(t, control.harness.multiClient, true)
		holdArm := control.harness.multiClient.beforeSynRetryReadinessArmForTest
		control.harness.multiClient.beforeSynRetryReadinessArmForTest = func(update *multiClientChannelUpdate) {
			arms.Add(1)
			holdArm(update)
		}
		control.harness.multiClient.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel {
			selections.Add(1)
			return []*multiClientChannel{first.base.selected, second.base.selected}
		}
		// This snapshot runs only after the existing synctest/barrier boundary.
		// It identifies missing pre-Pack admission versus another dispatcher,
		// a closed-again source gate, or a stale ownership/epoch rejection.
		state := func() string {
			row := map[string]any{
				"selections": selections.Load(), "dispatches": dispatches.Load(), "arms": arms.Load(), "grants": grants.Load(),
				"before_first": beforeFirst.Load(), "before_second": beforeSecond.Load(),
				"first_started": first.base.started.Load(), "second_started": second.base.started.Load(),
				"first_terminal": first.base.terminal.Load(), "second_terminal": second.base.terminal.Load(),
				"first_resend_closed": first.source.resendCapacityUnavailable.Load(), "second_resend_closed": second.source.resendCapacityUnavailable.Load(),
				"first_prepared_closed": first.source.preparedFlightUnavailable.Load(), "second_prepared_closed": second.source.preparedFlightUnavailable.Load(),
				"first_waiters": first.source.resendCapacityUnavailable.waiterCount.Load(), "second_waiters": second.source.resendCapacityUnavailable.waiterCount.Load(),
				"first_source_done": first.source.ctx.Err() != nil, "second_source_done": second.source.ctx.Err() != nil,
				"first_route": len(first.base.route), "second_route": len(second.base.route),
			}
			control.update.stateLock.Lock()
			row["same_cache"], row["owner_refs"], row["queued"] = control.update.synRetry == control.owner, control.owner.references, control.owner.queued
			row["bound"], row["inbound"], row["admission_epoch"], row["source_order"] = control.update.client.Load() != nil, control.update.receivedInbound.Load(), control.update.sequenceAdmissionEpoch, control.update.sequenceSourceOrder
			row["retry_race_order"], row["width"], row["overflow"] = control.owner.raceOrder, control.owner.readinessWidth, control.owner.readinessOverflow
			if ready := control.owner.readiness; ready != nil {
				row["ready_count"], row["ready_indexed"], row["ready_refs"] = ready.count, ready.indexed.Load(), ready.references.Load()
				row["ready_epoch"], row["ready_race_order"], row["ready_source_order"] = ready.epoch, ready.raceOrder, ready.sourceOrder
				row["attempt_epoch"], row["attempt_source_order"] = ready.attemptEpoch, ready.attemptSourceOrder
			}
			if race := control.update.race; race != nil {
				row["race_order"], row["race_single"], row["race_clients"], row["race_pending"], row["race_sent"], row["race_packets"] = race.collapseOrder, race.single, len(race.clientStates), race.pendingSendCount, race.sentPacketCount, race.packetCount
				if client := race.clientStates[first.base.selected]; client != nil {
					row["race_first_pending"], row["race_first_sent"] = client.pendingSendCount, client.sentPacketCount
				}
				if client := race.clientStates[second.base.selected]; client != nil {
					row["race_second_pending"], row["race_second_sent"] = client.pendingSendCount, client.sentPacketCount
				}
			}
			control.update.stateLock.Unlock()
			row["first_queue_count"], row["first_queue_bytes"] = first.source.resendQueue.QueueSize()
			row["second_queue_count"], row["second_queue_bytes"] = second.source.resendQueue.QueueSize()
			return fmt.Sprintf("%v", row)
		}
		control.harness.inject("a", reraceProviderSignal(t, control.path))
		synRetryControlEntered(t, entered)
		one, two := first.base.started.Load(), second.base.started.Load()
		control.update.stateLock.Lock()
		ready := control.owner.readiness
		bothSources := ready != nil && ready.count == 2 &&
			((ready.slots[0].source == first.source && ready.slots[1].source == second.source) ||
				(ready.slots[0].source == second.source && ready.slots[1].source == first.source))
		premise := bothSources && !ready.indexed.Load() && ready.references.Load() == 1 &&
			control.update.synRetry == control.owner && control.owner.references == 2 && !control.owner.queued &&
			control.update.race == nil && control.update.client.Load() == nil
		control.update.stateLock.Unlock()
		if !premise || one != seedOne+1 || two != seedTwo+1 || beforeFirst.Load() != 1 || beforeSecond.Load() != 1 ||
			!first.source.resendCapacityUnavailable.Load() || !second.source.resendCapacityUnavailable.Load() {
			t.Fatalf("COALESCING_TWO_REFUSALS_PREMISE: %s", state())
		}
		first.releasePeerCredit()
		second.releasePeerCredit()
		if first.source.resendCapacityUnavailable.Load() || second.source.resendCapacityUnavailable.Load() ||
			first.base.started.Load() != one || second.base.started.Load() != two {
			t.Fatalf("COALESCING_TWO_ACK_OPEN_PREMISE: %s", state())
		}
		release()
		synctest.Wait()
		if first.base.started.Load() != one+1 || second.base.started.Load() != two+1 {
			t.Fatalf("simultaneous ready sources duplicated or lost one resumed field: before_first=%d before_second=%d %s", one, two, state())
		}
		if dispatches.Load() != 2 || grants.Load() != 1 || arms.Load() != 2 {
			t.Fatalf("COALESCING_DISPATCH_OWNERSHIP: %s", state())
		}
		first.finishPackets(control.original)
		second.finishPackets(control.original)
		synRetryReadyProof(t, control, second.base.selected)
		control.close(t)
	})
}

// Genuine metadata headroom refusal preserves every ordinary provider offer.
// It creates no partial set of subscriptions or authority from a memory wake.
func TestNativeSynRetryReadinessMetadataRefusalDoesNotNarrowField(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		control := newSynRetryReadyControl(t, 51409)
		count := len(control.owner.readiness.slots) + 1
		credits := make([]*nativeBatchCreditFixture, count)
		budgets := make([]*TransferMemoryBudget, count)
		for index := range credits {
			budget := NewTransferMemoryBudget(64 * 1024)
			budgets[index] = budget
			credits[index] = newNativeBatchCreditFixture(t, func(settings *ClientSettings) { settings.SendBufferSettings.ResendQueueBudget = budget })
		}
		held := make([]ByteCount, count)
		var releaseOnce sync.Once
		release := func() {
			releaseOnce.Do(func() {
				for index, bytes := range held {
					if bytes != 0 {
						budgets[index].Release(bytes)
					}
				}
			})
		}
		t.Cleanup(release)
		// Candidate Pack calls run concurrently. Every independent source
		// must lack metadata permission before any can fund the full resize.
		for index, budget := range budgets {
			bytes := budget.Available()
			if bytes <= 0 || !budget.TryReserve(bytes) {
				t.Fatalf("actual source%d metadata budget was not saturated", index)
			}
			held[index] = bytes
		}
		started := make([]int64, count)
		for index, credit := range credits {
			started[index] = credit.base.started.Load()
		}
		control.harness.multiClient.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel {
			clients := make([]*multiClientChannel, count)
			for index, credit := range credits {
				clients[index] = credit.base.selected
			}
			return clients
		}
		control.harness.inject("a", reraceProviderSignal(t, control.path))
		synctest.Wait()
		ready, paid := control.owner.readiness, control.owner.readiness.credit
		if !control.owner.readinessOverflow || ready.indexed.Load() || ready.count != 0 || control.harness.multiClient.synRetryReadinessRefusals.Load() != 1 {
			t.Fatal("metadata refusal did not choose explicit all-or-nothing bounded fallback")
		}
		for index, credit := range credits {
			if credit.base.started.Load() != started[index]+1 || credit.source.resendCapacityUnavailable.waiterCount.Load() != 0 || credit.base.selected.dialFailureCount() != 0 {
				t.Fatal("metadata permission narrowed the actual field, installed partial watchers or struck a source")
			}
		}
		release()
		credits[len(credits)-1].releasePeerCredit()
		synctest.Wait()
		for index, credit := range credits {
			if credit.base.started.Load() != started[index]+1 {
				t.Fatal("a metadata/budget wake manufactured retry permission")
			}
		}
		control.close(t)
		requireSynRetryReadyRetired(t, ready, paid)
	})
}

func testSynRetryReadinessTerminalControl(t *testing.T, port int, flags byte) {
	t.Helper()
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		credit := newNativeBatchCreditFixture(t)
		control := newSynRetryReadyControl(t, port)
		control.refuseOn(t, credit)
		ready, paid, packetCredit := control.owner.readiness, control.owner.readiness.credit, control.owner.credit
		entered, release := synRetryReadyHold(t, control.harness.multiClient, false)
		credit.releasePeerCredit()
		synRetryControlEntered(t, entered)
		packet := reraceTcpPacket(control.path, flags, control.path.SequenceNumber+1, 0, nil)
		if !synRetryControlOffer(t, control.harness, MessagePoolCopy(packet)) {
			t.Fatal("actual terminal control was refused at ready source")
		}
		if !packetCredit.released.Load() || paid.released.Load() || ready.references.Load() != 1 {
			t.Fatal("terminal control did not release packet custody while independently paying stopped readiness")
		}
		started := credit.base.started.Load()
		release()
		synctest.Wait()
		if credit.base.started.Load() != started {
			t.Fatal("terminal control permitted a stale ready SYN")
		}
		credit.finishPackets(packet)
		requireSynRetryReadyRetired(t, ready, paid)
		control.close(t)
	})
}

func TestNativeSynRetryReadinessFinRetiresCache(t *testing.T) {
	testSynRetryReadinessTerminalControl(t, 51410, tcpFlagFin|tcpFlagAck)
}

func TestNativeSynRetryReadinessRstRetiresCache(t *testing.T) {
	testSynRetryReadinessTerminalControl(t, 51411, tcpFlagRst|tcpFlagAck)
}
