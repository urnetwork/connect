//go:build urnetwork_synthetic_fixture

// A future original can actually time out and be rewritten before the real
// cumulative prefix ACK makes it current. Its old backoff is not proof that
// the current owner is still queued on the reliable carrier.
package connect

import (
	"bytes"
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

type retriedCurrentOwnerFact struct {
	known, current, deferred, head, selective, stable bool
	passed, acknowledgedPast                          bool
	number, oldestNumber                              uint64
	sends, defers, originals                          int
	kind                                              sendRecoveryKind
	due, consumedAck, cumulative                      time.Time
	probe                                             time.Duration
}

func TestPublicFutureRetryGetsFreshProbeWhenPrefixMakesItCurrent(t *testing.T) {
	GetMessagePoolAggregateStats()
	var missedProbe, earlierDueChanged bool
	var inheritedDue, recoveredAfter, probe time.Duration
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
					t.Error("retried-current public source retained DATA quota after joins")
				}
			}
		})
		var ownedSender atomic.Pointer[Client]
		var target atomic.Pointer[Id]
		var latest atomic.Pointer[retriedCurrentOwnerFact]
		var targetCumulative, targetSelective atomic.Int64
		var wireLock sync.Mutex
		wireFacts := map[Id]currentDeferredWireFact{}
		progress := func(event TransferProgressEvent) {
			id := target.Load()
			if id == nil || event.MessageId != *id || event.Stage != "receive_ack_begin" {
				return
			}
			if event.Selective {
				targetSelective.Add(1)
			} else {
				targetCumulative.Add(1)
			}
		}
		layout := consumerPaidPackByteLayout{label: "future-retried-current", byteLimit: 174762,
			leafBytes: leafBytes, rootBytes: 4 * leafBytes, countLimit: 85}
		fixture, log, _, _ := newConsumerPaidPackByteFixture(t, layout, rx, head, pack, progress,
			func(receiver bool, settings *ClientSettings) {
				settings.ReceiveBufferSettings.AdvertiseReceiveWindow = true
				if receiver {
					return
				}
				send := settings.SendBufferSettings
				send.ProgressObserver = progress
				send.ResendQueueBudget = tx
				send.ResendQueueRetainedByteAccounting = true
				send.ResendQueueMinByteCount = 0
				send.ResendQueueMaxByteCount = leafBytes
				send.WindowSizing = WindowSizingFromDelivery
				send.ApplyWindowSizing()
				send.TransferWireMessageObserver = func(event TransferWireMessageObservation) {
					wireLock.Lock()
					defer wireLock.Unlock()
					fact := wireFacts[event.MessageId]
					fact.writes++
					fact.sendCount, fact.resend, fact.last = event.SendCount, event.Resend, time.Now()
					wireFacts[event.MessageId] = fact
				}
				send.afterApplyAckSnapshotForTest = func(id sendSequenceId) {
					client, targetId := ownedSender.Load(), target.Load()
					if client == nil || targetId == nil || id.LogicalLane != 1 {
						return
					}
					sequence := client.sendBuffer.lookupSendSequence(id, nil)
					fact := &retriedCurrentOwnerFact{originals: len(sequence.sendItems), cumulative: sequence.lastCumulativeAckTime}
					if len(sequence.sendItems) != 0 {
						fact.oldestNumber = sequence.sendItems[0].sequenceNumber
					}
					// This bounded fixture has only prefix, target and suffix.
					// Only the actual worker reads its mutable owner fields.
					for _, item := range sequence.sendItems {
						if item.messageId != *targetId {
							continue
						}
						fact.known, fact.current = true, sequence.sendItems[0] == item
						fact.number, fact.sends, fact.defers = item.sequenceNumber, item.sendCount, item.timeoutDeferCount
						fact.deferred, fact.head, fact.selective = item.deferralOutstanding, item.head, item.selectiveAcked
						fact.kind, fact.due, fact.consumedAck = item.recoveryKind, item.resendTime, item.timeoutDeferAckTime
						fact.stable, fact.acknowledgedPast = sequence.stablePacedFifo(item), sequence.laneAcknowledgedPast(item)
						passed := sequence.fifoRepair.passed
						fact.passed = passed.set && passed.messageId == item.messageId && passed.number == item.sequenceNumber
						fact.probe = sequence.rttWindow.probeRtt(time.Now())
					}
					latest.Store(fact)
				}
			})
		ownedSender.Store(fixture.sender)
		defaults, configured := DefaultSendBufferSettings(), fixture.sender.settings.SendBufferSettings
		if configured.ReliableLaneProvenRecovery || !configured.DeferTimeoutResendWhileCumulativeProgress ||
			!configured.DeferTimeoutResendBackoff || configured.MinResendInterval != defaults.MinResendInterval ||
			configured.RttMinResendInterval != defaults.RttMinResendInterval || configured.MaxResendInterval != defaults.MaxResendInterval ||
			configured.AckTimeout != defaults.AckTimeout || configured.DeliverySizedWindowScale <= 0 {
			t.Fatal("retried-current fixture changed ordinary H1 recovery policy or timers")
		}
		var done [4]atomic.Bool
		var completed [4]atomic.Int64
		started := time.Now()
		write := func(slot int) consumerPaidPackByteWire {
			return consumerPaidPackByteSend(t, fixture, 1, 1, slot, &done[slot], &completed[slot], started)
		}
		warm := write(0)
		time.Sleep(25 * time.Millisecond)
		fixture.forward(warm.frame, fixture.receiverIn)
		time.Sleep(25 * time.Millisecond)
		fixture.acknowledge()
		synctest.Wait()
		if !done[0].Load() || root.UsedByteCount() != 0 {
			t.Fatal("actual signed warm original did not deliver, ACK and refund")
		}
		prefix, hole, suffix := write(1), write(2), write(3)
		original := hole.frame.pack
		id := RequireIdFromBytes(original.MessageId)
		target.Store(&id)
		if original.Head || original.Nack || original.ContractAhead || original.ContractId != nil || original.ContractFrame != nil ||
			len(original.Frames) != 1 || protocolOnlyPack(original) ||
			prefix.frame.pack.SequenceNumber+1 != original.SequenceNumber || suffix.frame.pack.SequenceNumber != original.SequenceNumber+1 ||
			!bytes.Equal(prefix.frame.pack.SequenceId, original.SequenceId) || !bytes.Equal(suffix.frame.pack.SequenceId, original.SequenceId) {
			t.Fatal("actual public source did not emit contiguous prefix, implicit non-Head DATA and suffix")
		}
		fixture.drop(hole.frame)
		// The prefix remains genuinely unacknowledged. Wait for the public
		// source's normal retry of the later original, rather than changing a
		// retry counter, head flag, sequence item or recovery deadline.
		futureRetry := pressureContractNaturalRetry(t, fixture, original)
		synctest.Wait()
		future := latest.Load()
		wireLock.Lock()
		futureWire := wireFacts[id]
		wireLock.Unlock()
		if future == nil || !future.known || future.current || future.number != original.SequenceNumber ||
			future.sends != 2 || future.deferred || future.head || future.selective || !future.stable || future.kind != sendRecoveryNone ||
			futureWire.writes != 2 || !futureWire.resend || futureRetry.pack.Head || futureRetry.pack.ContractAhead ||
			!bytes.Equal(futureRetry.pack.SequenceId, original.SequenceId) || !bytes.Equal(futureRetry.pack.MessageId, original.MessageId) ||
			futureRetry.pack.SequenceNumber != original.SequenceNumber || len(futureRetry.pack.Frames) != 1 ||
			!bytes.Equal(futureRetry.pack.Frames[0].MessageBytes, original.Frames[0].MessageBytes) ||
			done[1].Load() || done[2].Load() || targetCumulative.Load() != 0 || targetSelective.Load() != 0 {
			t.Fatal("normal future retry did not clear its deferral while retaining the exact undelivered non-Head owner")
		}
		fixture.drop(futureRetry)
		for _, held := range fixture.heldFrames {
			if held.bytes != nil && held != prefix.frame && held != suffix.frame {
				fixture.drop(held)
			}
		}
		// ACK only the genuinely delivered original prefix. In particular its
		// old Tag cannot supply a repaired-prefix physical-passage proof.
		fixture.forward(prefix.frame, fixture.receiverIn)
		fixture.acknowledge()
		synctest.Wait()
		current := latest.Load()
		if current == nil || !current.known || !current.current || current.originals != 2 || current.oldestNumber != original.SequenceNumber ||
			current.sends != 2 || current.head || current.selective || current.kind != sendRecoveryNone || !current.stable ||
			current.passed || current.acknowledgedPast || !done[1].Load() || done[2].Load() || done[3].Load() ||
			targetCumulative.Load() != 0 || targetSelective.Load() != 0 {
			t.Fatal("real original-prefix ACK did not expose the same actually retried current DATA without delivery proof")
		}
		receiver := consumerForeignHeadSequence(t, fixture, warm.frame)
		if receiver.nextSequenceNumber != original.SequenceNumber || receiver.receiveQueue.Len() != 0 ||
			rx.UsedByteCount() != 0 || head.UsedByteCount() != 0 || tx.UsedByteCount() == 0 {
			t.Fatal("retried-current ACK changed the actual missing floor or paid ownership premise")
		}
		becameCurrent := time.Now()
		probe, inheritedDue = current.probe, current.due.Sub(becameCurrent)
		// The actual future retry is already due before a fresh current probe.
		// Prefix retirement must preserve that earlier deadline exactly.
		if !future.due.After(becameCurrent) || future.due.After(becameCurrent.Add(probe)) {
			t.Fatal("actual future retry did not retain an earlier live deadline than the current probe")
		}
		earlierDueChanged = !current.due.Equal(future.due)
		t.Logf("[retried-current] number=%d future_retry_send_count=%d deferrals=%d outstanding=%t inherited_due=%s probe=%s consumed_ack_age=%s",
			original.SequenceNumber, current.sends, current.defers, current.deferred, inheritedDue, probe, becameCurrent.Sub(current.consumedAck))
		// Observe and acknowledge the first actual earlier retry before a
		// second valid timeout can produce another copy of that same owner.
		time.Sleep(time.Until(future.due) + time.Nanosecond)
		synctest.Wait()
		var retry *windowRoundFrame
		for len(fixture.senderOut) != 0 {
			candidate := fixture.take(fixture.senderOut)
			if candidate.pack != nil && bytes.Equal(candidate.pack.MessageId, original.MessageId) &&
				bytes.Equal(candidate.pack.SequenceId, original.SequenceId) && candidate.pack.SequenceNumber == original.SequenceNumber {
				if retry != nil {
					t.Fatal("new current owner emitted more than one probe")
				}
				retry = candidate
			} else {
				fixture.drop(candidate)
			}
		}
		missedProbe = retry == nil
		if targetCumulative.Load() != 0 || targetSelective.Load() != 0 || done[2].Load() || done[3].Load() {
			t.Fatal("undelivered current DATA or suffix acquired a false ACK")
		}
		if retry == nil {
			retry = pressureContractNaturalRetry(t, fixture, original)
		}
		recoveredAfter = time.Since(becameCurrent)
		if !retry.pack.Head || retry.pack.SequenceNumber != original.SequenceNumber ||
			!bytes.Equal(retry.pack.MessageId, original.MessageId) || !bytes.Equal(retry.pack.SequenceId, original.SequenceId) ||
			len(retry.pack.Frames) != 1 || !bytes.Equal(retry.pack.Frames[0].MessageBytes, original.Frames[0].MessageBytes) {
			t.Fatal("natural current retry lost its actual original identity or payload")
		}
		fixture.forward(retry, fixture.receiverIn)
		fixture.acknowledge()
		fixture.forward(suffix.frame, fixture.receiverIn)
		fixture.acknowledge()
		synctest.Wait()
		terminal := latest.Load()
		if !done[2].Load() || !done[3].Load() || targetCumulative.Load() == 0 || targetSelective.Load() != 0 ||
			terminal == nil || terminal.known || terminal.originals != 0 || receiver.nextSequenceNumber != original.SequenceNumber+2 ||
			root.UsedByteCount() != 0 || len(log.messages()) != 0 {
			t.Fatal("natural current retry did not deliver, really ACK, drain and refund its exact owners")
		}
		t.Logf("[retried-current-drained] recovery_after=%s missed_probe=%t", recoveredAfter, missedProbe)
		fixture.cancel()
		for _, client := range []*Client{fixture.sender, fixture.receiver} {
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Errorf("join retried-current public client: %v", err)
			}
		}
		t.Log("[retried-current-joined]")
	})
	t.Log("[retried-current-cleanup-complete]")
	if earlierDueChanged {
		t.Error("EARLIER_FUTURE_RETRY_DEADLINE_CHANGED_BY_PREFIX_ACK: genuine initial-prefix retirement changed the same current owner's already earlier future retry deadline")
	}
	if missedProbe {
		t.Errorf("RETRIED_CURRENT_DATA_INHERITS_FUTURE_BACKOFF: actual future retry cleared its deferral flag before real prefix ACK; current retained original inherited %s due and recovered after %s against unchanged %s probe", inheritedDue, recoveredAfter, probe)
	}
}
