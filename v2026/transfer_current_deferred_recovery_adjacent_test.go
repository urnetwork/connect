//go:build urnetwork_synthetic_fixture

package connect

import (
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// This is the loss witness's same public signed FIFO with no lost original.
// Every newly current owner arrives within the existing probe interval, so a
// recovery scheduling fix must not turn a slow healthy drain into duplicates.
func TestPublicDeferredCurrentProbeKeepsHealthyFifoDrainSingleWrite(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		const leafBytes = ByteCount(128 * 1024)
		root := NewTransferMemoryBudget(4 * leafBytes)
		tx := NewTransferMemoryBudgetWithParent(leafBytes, root)
		rx := NewTransferMemoryBudgetWithParent(leafBytes, root)
		head := NewTransferMemoryBudgetWithParent(leafBytes, root)
		pack := NewTransferMemoryBudgetWithParent(leafBytes, root)
		t.Cleanup(func() {
			for _, budget := range []*TransferMemoryBudget{pack, head, rx, tx, root} {
				assertRetainedBudgetBalance(t, budget)
				if budget.Stats().QuotaByteCount != 0 {
					t.Error("healthy public FIFO retained DATA quota")
				}
			}
		})
		layout := consumerPaidPackByteLayout{label: "healthy-current-deferral", byteLimit: 174762,
			leafBytes: leafBytes, rootBytes: 4 * leafBytes, countLimit: 85}
		var ownedSender atomic.Pointer[Client]
		var deferredCurrentObserved atomic.Bool
		var pendingOriginals atomic.Int64
		var wireLock sync.Mutex
		wireFacts := map[Id]currentDeferredWireFact{}
		fixture, log, _, _ := newConsumerPaidPackByteFixture(t, layout, rx, head, pack, nil,
			func(receiver bool, settings *ClientSettings) {
				settings.ReceiveBufferSettings.AdvertiseReceiveWindow = true
				if !receiver {
					send := settings.SendBufferSettings
					send.ResendQueueBudget = tx
					send.ResendQueueRetainedByteAccounting = true
					send.ResendQueueMinByteCount = 0
					send.ResendQueueMaxByteCount = leafBytes
					send.WindowSizing = WindowSizingFromDelivery
					send.ApplyWindowSizing()
					send.afterApplyAckSnapshotForTest = func(id sendSequenceId) {
						client := ownedSender.Load()
						if client == nil || id.LogicalLane != 1 {
							return
						}
						// The actual send worker copies its own scalar state.
						// The test never keeps or reads its mutable item pointer.
						sequence := client.sendBuffer.lookupSendSequence(id, nil)
						pendingOriginals.Store(int64(len(sequence.sendItems)))
						if len(sequence.sendItems) != 0 && sequence.sendItems[0].deferralOutstanding {
							deferredCurrentObserved.Store(true)
						}
					}
					send.TransferWireMessageObserver = func(event TransferWireMessageObservation) {
						wireLock.Lock()
						defer wireLock.Unlock()
						fact := wireFacts[event.MessageId]
						fact.writes++
						fact.sendCount, fact.resend = event.SendCount, event.Resend
						wireFacts[event.MessageId] = fact
					}
				}
			})
		ownedSender.Store(fixture.sender)
		const count = 10
		var done [count + 1]atomic.Bool
		var completed [count + 1]atomic.Int64
		started := time.Now()
		warm := consumerPaidPackByteSend(t, fixture, 1, 1, 0, &done[0], &completed[0], started)
		time.Sleep(25 * time.Millisecond)
		fixture.forward(warm.frame, fixture.receiverIn)
		time.Sleep(25 * time.Millisecond)
		fixture.acknowledge()
		synctest.Wait()
		if !done[0].Load() || fixture.sender.settings.SendBufferSettings.ReliableLaneProvenRecovery {
			t.Fatal("healthy public H1 warm original or default lane policy changed")
		}
		var originals [count]consumerPaidPackByteWire
		for index := range originals {
			originals[index] = consumerPaidPackByteSend(t, fixture, 1, 1, index+1, &done[index+1], &completed[index+1], started)
		}
		for index, original := range originals {
			synctest.Wait()
			time.Sleep(250 * time.Millisecond)
			fixture.forward(original.frame, fixture.receiverIn)
			fixture.acknowledge()
			synctest.Wait()
			wireLock.Lock()
			fact := wireFacts[RequireIdFromBytes(original.frame.pack.MessageId)]
			wireLock.Unlock()
			if !done[index+1].Load() || fact.writes != 1 || fact.sendCount != 1 || fact.resend ||
				fixture.sender.SendRecoveryStats().TimeoutResendWriteCount != 0 || len(fixture.senderOut) != 0 {
				t.Fatal("healthy FIFO promotion wrote a duplicate before the original's real ACK")
			}
		}
		if !deferredCurrentObserved.Load() || pendingOriginals.Load() != 0 || fixture.deliveredCount != count+1 ||
			root.UsedByteCount() != 0 || len(log.messages()) != 0 {
			t.Fatal("healthy FIFO did not actually exercise deferred current ownership and drain its paid graph")
		}
		t.Log("[current-deferred-healthy] actual_initial_writes=11 natural_recovery_writes=0 actual_acknowledged=11")
	})
}

// Direct transition guards isolate acknowledgement ownership and policy from
// the separate public-wire liveness witness. Synthetic item state here is not
// offered as evidence that a device or public route produced that state.
func TestDeferredCurrentProbePreservesOtherRecoveryAndPolicy(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, row := range []struct {
		name           string
		change         func(*SendSequence, *sendItem)
		probe, keepDue bool
	}{
		{name: "default-generic-current", probe: true},
		{name: "initial-send-not-deferred", change: func(_ *SendSequence, item *sendItem) { item.deferralOutstanding = false }},
		{name: "retried-not-deferred", probe: true, change: func(_ *SendSequence, item *sendItem) {
			item.deferralOutstanding = false
			item.sendCount = 4
		}},
		{name: "deferred-earlier-deadline", probe: true, change: func(sequence *SendSequence, item *sendItem) {
			item.resendTime = time.Now().Add(sequence.rttWindow.probeRtt(time.Now()) / 2)
		}},
		{name: "deferred-equal-deadline", probe: true, change: func(sequence *SendSequence, item *sendItem) {
			item.resendTime = time.Now().Add(sequence.rttWindow.probeRtt(time.Now()))
		}},
		{name: "retried-earlier-deadline", keepDue: true, change: func(sequence *SendSequence, item *sendItem) {
			item.deferralOutstanding = false
			item.sendCount = 4
			item.resendTime = time.Now().Add(sequence.rttWindow.probeRtt(time.Now()) / 2)
		}},
		{name: "retried-equal-deadline", keepDue: true, change: func(sequence *SendSequence, item *sendItem) {
			item.deferralOutstanding = false
			item.sendCount = 4
			item.resendTime = time.Now().Add(sequence.rttWindow.probeRtt(time.Now()))
		}},
		{name: "selectively-retained", change: func(_ *SendSequence, item *sendItem) { item.selectiveAcked = true }},
		{name: "stronger-recovery", change: func(_ *SendSequence, item *sendItem) { item.recoveryKind = sendRecoverySelectiveGap }},
		{name: "deferral-disabled", change: func(sequence *SendSequence, _ *sendItem) {
			sequence.sendBufferSettings.DeferTimeoutResendWhileCumulativeProgress = false
		}},
		{name: "explicit-lane-rule", change: func(sequence *SendSequence, _ *sendItem) {
			sequence.sendBufferSettings.ReliableLaneProvenRecovery = true
		}},
		{name: "constant-window", change: func(sequence *SendSequence, _ *sendItem) { sequence.sendBufferSettings.DeliverySizedWindowScale = 0 }},
		{name: "non-h1", change: func(sequence *SendSequence, _ *sendItem) {
			sequence.contractMultiRouteWriter = &windowPacingPolicyWriter{}
		}},
		{name: "unwritten-reliability", change: func(_ *SendSequence, item *sendItem) { item.reliableCarrierObserved = false }},
		{name: "unreliable-copy", change: func(_ *SendSequence, item *sendItem) { item.unreliableCarrierObserved = true }},
		{name: "changed-carrier", change: func(_ *SendSequence, item *sendItem) { item.carrierChanged = true }},
		{name: "exact-fifo-passage-already-proved", change: func(sequence *SendSequence, item *sendItem) {
			sequence.client.feedbackTimeBase = time.Now().Add(-time.Second)
			sequence.resetFifoRepairGeneration(1)
			item.expectsAck = true
			sequence.fifoRepair.passed = fifoPassedHead{messageId: item.messageId, number: item.sequenceNumber, route: item.carrierRoute, set: true}
		}},
		{name: "same-lane-already-acknowledged-past", change: func(sequence *SendSequence, item *sendItem) {
			sequence.observeLaneAck(&sendItem{transferItem: transferItem{sequenceNumber: item.sequenceNumber + 1}, carrierRoute: item.carrierRoute}, time.Now())
		}},
	} {
		synctest.Test(t, func(t *testing.T) {
			sequence, items, _ := newLaneHeadPromotionSequence(t)
			sequence.sendBufferSettings.ReliableLaneProvenRecovery = false
			sequence.sendBufferSettings.DeliverySizedWindowScale = 2
			sequence.contractMultiRouteWriter = &windowPacingPolicyWriter{policy: transferFlightPolicySnapshot{generation: 1, h1Only: true}}
			current, tail := items[1], items[2]
			current.deferralOutstanding = true
			current.timeoutDeferCount = 3
			current.timeoutDeferAckTime = time.Now().Add(-time.Second)
			if row.change != nil {
				row.change(sequence, current)
			}
			beforeDue, beforeAck := current.resendTime, current.timeoutDeferAckTime
			tailDue := tail.resendTime
			beforeSendTime, beforeCount, beforeDefers := current.sendTime, current.sendCount, current.timeoutDeferCount
			beforeKind, beforeOutstanding := current.recoveryKind, current.deferralOutstanding
			prefixId := items[0].messageId
			probeAt := time.Now().Add(sequence.rttWindow.probeRtt(time.Now()))
			sequence.receiveAck(prefixId, false, sequenceTag{}, false)
			wantDue, wantAck := beforeDue, beforeAck
			if row.probe {
				wantDue = probeAt
			}
			if row.probe || row.keepDue {
				wantAck = time.Now()
			}
			if len(sequence.sendItems) != 3 || sequence.sendItems[0] != current ||
				sequence.resendQueue.GetByMessageId(current.messageId) != current ||
				current.resendTime != wantDue || current.timeoutDeferAckTime != wantAck || tail.resendTime != tailDue ||
				current.sendTime != beforeSendTime || current.sendCount != beforeCount || current.timeoutDeferCount != beforeDefers ||
				current.recoveryKind != beforeKind || current.deferralOutstanding != beforeOutstanding {
				t.Fatalf("%s: cumulative prefix changed another owner, policy or existing recovery authority", row.name)
			}
			if (row.probe || row.keepDue) && sequence.shouldDeferTimeoutResend(current, sequence.rttWindow.scaledRtt(time.Now())) {
				t.Fatalf("%s: the scheduled current probe did not consume its exact prefix ACK", row.name)
			}
			// The retired ID is now unknown. Repeated ACK ingress must not
			// postpone that exact owner or re-credit the same transition.
			time.Sleep(time.Millisecond)
			sequence.receiveAck(prefixId, false, sequenceTag{}, false)
			if current.resendTime != wantDue || current.timeoutDeferAckTime != wantAck || tail.resendTime != tailDue {
				t.Fatalf("%s: duplicate cumulative receipt rearmed the current probe or another owner", row.name)
			}
		})
	}
}
