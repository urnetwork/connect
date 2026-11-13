// Reliable residence retains its first lower-ACK protection unless exact
// endpoint-loss evidence supersedes it. Selection never changes payload custody.
package connect

import (
	"bytes"
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

type timeoutProgressFreshnessWriter struct{ MultiRouteWriter }

func (*timeoutProgressFreshnessWriter) transferFlightPolicy() transferFlightPolicySnapshot {
	return transferFlightPolicySnapshot{h1Only: true}
}

// This is a selection-level fixture, not a fabricated public ACK authority.
// It owns valid encoded originals and real retained charges. The signed SDK
// companion supplies the independently authenticated ingress regression.
type timeoutProgressFreshnessFixture struct {
	sequence *SendSequence
	items    []*sendItem
	budget   *TransferMemoryBudget
	started  time.Time
	terminal []int
	success  []int
	closed   bool
}

func newTimeoutProgressFreshnessFixture(t *testing.T, count int, beforeMarshal ...func(*sendItem, *protocol.TransferFrame)) *timeoutProgressFreshnessFixture {
	t.Helper()
	settings := DefaultSendBufferSettings()
	settings.WindowSizing = WindowSizingFromDelivery
	settings.ApplyWindowSizing()
	settings.ResendQueueRetainedByteAccounting = true
	if !settings.DeferTimeoutResendWhileCumulativeProgress || settings.ReliableLaneProvenRecovery ||
		settings.MinResendInterval != 2*time.Second || settings.MaxResendInterval != 8*time.Second {
		t.Fatal("selection witness lost its ordinary deferral and reliable recovery policy")
	}
	budget := NewTransferMemoryBudget(1024 * 1024)
	queue := newResendQueue(budget, 0)
	queue.setLifetimeBudget()
	fixture := &timeoutProgressFreshnessFixture{budget: budget, started: time.Now(), terminal: make([]int, count), success: make([]int, count)}
	sequence := &SendSequence{ctx: context.Background(), client: &Client{}, log: NewNoopLogger(),
		sequenceId: NewId(), sendBufferSettings: settings, resendQueue: queue,
		ackWindow: newSequenceAckWindow(), contractMultiRouteWriter: &timeoutProgressFreshnessWriter{},
		rttWindow: NewRttWindow(NewNoopLogger(), settings.RttWindowSize, settings.RttWindowTimeout,
			settings.RttScale, settings.MinResendInterval, settings.RttMinResendInterval, settings.MaxResendInterval)}
	fixture.sequence = sequence
	t.Cleanup(func() { fixture.close(t) })
	for index := range count {
		item := takeSendItem()
		*item = sendItem{transferItem: transferItem{messageId: NewId(), sequenceNumber: uint64(index)},
			expectsAck: true, sendTime: fixture.started, resendTime: fixture.started.Add(2 * time.Second),
			sendCount: 1, ackTimeout: settings.AckTimeout, head: true, reliableCarrierObserved: true}
		frame := &protocol.TransferFrame{Pack: &protocol.Pack{MessageId: item.messageId.Bytes(),
			SequenceId: sequence.sequenceId.Bytes(), SequenceNumber: item.sequenceNumber, Head: true,
			Frames: []*protocol.Frame{{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: bytes.Repeat([]byte{0x57}, 32)}}}}
		// A component may select its original envelope before serialization
		// and payment. Default callers preserve their unchanged Head fixture.
		for _, configure := range beforeMarshal {
			configure(item, frame)
		}
		wire, err := ProtoMarshal(frame)
		if err != nil {
			item.messagePoolReturn()
			t.Fatal(err)
		}
		item.transferFrameBytes = wire
		charge := item.retainedMemoryByteCount(ByteCount(len(wire)), 1, false)
		if !item.reserveMemory(budget, charge) {
			item.messagePoolReturn()
			t.Fatal("complete encoded owner did not fit the fixed fixture root")
		}
		item.acks.add(sendAckRecord{retainAfterAckTimeout: true, callback: func(err error) {
			fixture.terminal[index]++
			if err == nil {
				fixture.success[index]++
			} else if err != context.Canceled {
				t.Errorf("retained original completed with unexpected error: %v", err)
			}
		}})
		fixture.items = append(fixture.items, item)
		sequence.sendItems = append(sequence.sendItems, item)
		sequence.addResendItem(item)
	}
	sequence.nextSequenceNumber = uint64(count)
	return fixture
}

func (self *timeoutProgressFreshnessFixture) close(t *testing.T) {
	t.Helper()
	if self.closed {
		return
	}
	self.closed = true
	self.sequence.ackLifetimes.clear()
	self.sequence.releaseRetainedSendItems(context.Canceled)
	// A second close cannot redispatch callbacks or return physical owners.
	self.sequence.releaseRetainedSendItems(context.Canceled)
	for index := range self.items {
		count := self.terminal[index]
		if count != 1 {
			t.Errorf("original%d terminal count=%d, want1", index, count)
		}
	}
	assertRetainedBudgetBalance(t, self.budget)
	if self.budget.UsedByteCount() != 0 || self.budget.Stats().QuotaByteCount != 0 || self.sequence.resendQueue.Len() != 0 {
		t.Error("selection fixture retained physical, quota or queue custody after close")
	}
}

// Call the ordinary owner handler with an actual retained identity. It must
// retire only the requested prefix and stamp progress at this fake instant.
func (self *timeoutProgressFreshnessFixture) acknowledge(t *testing.T, index int, selective bool) {
	t.Helper()
	sequence := self.sequence
	item := self.items[index]
	id := item.messageId
	if sequence.resendQueue.GetByMessageId(id) != item {
		t.Fatal("ACK control lost the exact live original")
	}
	before := sequence.lastCumulativeAckTime
	sequence.receiveAck(id, selective, sequenceTag{}, false)
	if selective {
		if !item.selectiveAcked || sequence.lastCumulativeAckTime != before || self.terminal[index] != 0 {
			t.Fatal("selective custody became cumulative delivery or a terminal callback")
		}
	} else if self.success[index] != 1 || sequence.lastCumulativeAckTime != time.Now() || sequence.resendQueue.GetByMessageId(id) != nil {
		t.Fatal("real cumulative handler did not retire and stamp its exact prefix")
	}
}

// Selection may retime a retry, but never dispatch it, settle it, reprice it,
// or abandon its existing exact-ID ACK and payload owner.
func (self *timeoutProgressFreshnessFixture) selectAt(t *testing.T, item *sendItem) (sendRecoverySelection, sendRecoverySelectionOutcome, time.Time) {
	t.Helper()
	sequence := self.sequence
	now := time.Now()
	if now.Before(item.resendTime) {
		t.Fatal("fixture attempted selection before the actual retained deadline")
	}
	id, number, sent, count := item.messageId, item.sequenceNumber, item.sendTime, item.sendCount
	wire := append([]byte(nil), item.transferFrameBytes...)
	before := self.budget.Stats()
	encoded, physical, owners := sequence.resendQueue.byteCount, sequence.resendQueue.queueByteCount, sequence.resendQueue.Len()
	callbacks := append([]int(nil), self.terminal...)
	selection, outcome, err := sequence.selectDueRecovery(item, now, sequence.transferFlightPolicy(), sequence.ackWindow)
	if err != nil {
		t.Fatal("actual recovery selection failed", err)
	}
	after := self.budget.Stats()
	if sequence.resendQueue.GetByMessageId(id) != item || item.messageId != id || item.sequenceNumber != number ||
		item.sendTime != sent || item.sendCount != count || !bytes.Equal(wire, item.transferFrameBytes) ||
		item.deliveryObserved || item.serviceCreditObserved || before != after ||
		sequence.resendQueue.byteCount != encoded || sequence.resendQueue.queueByteCount != physical || sequence.resendQueue.Len() != owners ||
		sequence.detachedAck.active || sequence.pendingRecovery != nil || sequence.client.SendRecoveryStats().TimeoutResendWriteCount != 0 {
		t.Fatal("selection changed encoded/physical/callback/write or exact original custody")
	}
	for index, count := range callbacks {
		if self.terminal[index] != count {
			t.Fatal("selection invoked an original callback")
		}
	}
	return selection, outcome, now
}

// A delayed healthy FIFO may have no recent ACK at its initial deadline.
// Without exact endpoint-loss proof the existing first-residence protection is
// intentional. The rejected generic clock-only experiment remains archived in
// timeout-progress-freshness-controls-r3; this default contract does not adopt it.
func TestTimeoutResidenceFirstDeferralWithoutExactLossProof(t *testing.T) {
	GetMessagePoolAggregateStats()
	for _, mode := range []string{"older_than_RTT", "exactly_RTT", "future_ACK", "no_cumulative", "selective_only"} {
		synctest.Test(t, func(t *testing.T) {
			assertMessagePoolOwnership(t)
			fixture := newTimeoutProgressFreshnessFixture(t, 2)
			sequence, item := fixture.sequence, fixture.items[1]
			if mode != "no_cumulative" {
				fixture.acknowledge(t, 0, mode == "selective_only")
			}
			delay := 2 * time.Second
			if mode == "older_than_RTT" {
				delay += time.Nanosecond
			}
			time.Sleep(delay)
			if mode == "future_ACK" {
				// Defensive decision/ACK ordering adjacency only. This is not
				// a claim that a peer controls the local cumulative clock.
				sequence.lastCumulativeAckTime = time.Now().Add(time.Nanosecond)
			}
			due, deferred := item.resendTime, sequence.client.timeoutResendDeferCount.Load()
			selection, outcome, now := fixture.selectAt(t, item)
			wantProtect := mode != "no_cumulative" && mode != "selective_only"
			if wantProtect {
				if outcome != sendRecoverySelectionDeferred || item.timeoutDeferCount != 1 || !item.deferralOutstanding ||
					item.resendTime != now.Add(2*time.Second) || item.timeoutDeferAckTime != sequence.lastCumulativeAckTime ||
					sequence.client.timeoutResendDeferCount.Load() != deferred+1 {
					t.Fatal("healthy initial residence lost its first deferral without exact receiver-loss proof")
				}
			} else if outcome != sendRecoverySelectionReady || selection.messageId != item.messageId || selection.number != item.sequenceNumber ||
				item.resendTime != due || item.timeoutDeferCount != 0 || item.deferralOutstanding ||
				sequence.client.timeoutResendDeferCount.Load() != deferred {
				t.Fatal("absent cumulative delivery credit granted an unsupported residence deferral")
			}
			fixture.close(t)
		})
	}
	t.Log("[timeout-residence-cleanup] cases=5 first_deferral_without_exact_loss_proof=true callbacks_once=true budgets_balanced=true")
}

// Late real prefix ACKs remain eligible, including age0 and1ns inside RTT.
func TestTimeoutProgressFreshnessSelectionKeepsRecentAck(t *testing.T) {
	GetMessagePoolAggregateStats()
	for _, age := range []time.Duration{0, 100 * time.Millisecond, 2*time.Second - time.Nanosecond} {
		synctest.Test(t, func(t *testing.T) {
			assertMessagePoolOwnership(t)
			fixture := newTimeoutProgressFreshnessFixture(t, 2)
			time.Sleep(2*time.Second - age)
			fixture.acknowledge(t, 0, false)
			time.Sleep(age)
			item, sequence := fixture.items[1], fixture.sequence
			_, outcome, now := fixture.selectAt(t, item)
			if outcome != sendRecoverySelectionDeferred || item.resendTime != now.Add(2*time.Second) ||
				item.timeoutDeferCount != 1 || !item.deferralOutstanding || item.timeoutDeferAckTime != sequence.lastCumulativeAckTime ||
				sequence.client.timeoutResendDeferCount.Load() != 1 || sequence.client.timeoutResendWithRecentCumulativeProgress.Load() != 1 {
				t.Fatalf("fresh actual ACK lost healthy deferral: age=%s outcome=%d next=%s", age, outcome, item.resendTime.Sub(now))
			}
		})
	}
}

// Six fresh advancing ACKs can protect a deep queued owner beyond the generic
// two-deferral limit. Without another ACK, the same progress cannot grant the
// next deferral. This is the per-item since-last-deferral contract, not an age gate.
func TestTimeoutProgressFreshnessQueuedDrainNeedsNewAck(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		fixture := newTimeoutProgressFreshnessFixture(t, 8)
		sequence, tail := fixture.sequence, fixture.items[7]
		for round := range 6 {
			at := tail.resendTime
			time.Sleep(time.Until(at) - time.Nanosecond)
			fixture.acknowledge(t, round, false)
			time.Sleep(time.Nanosecond)
			_, outcome, now := fixture.selectAt(t, tail)
			if outcome != sendRecoverySelectionDeferred || tail.timeoutDeferCount != round+1 || !tail.deferralOutstanding ||
				tail.timeoutDeferAckTime != sequence.lastCumulativeAckTime || !tail.resendTime.After(now) || tail.sendCount != 1 ||
				sequence.client.timeoutResendDeferCount.Load() != uint64(round+1) {
				t.Fatalf("healthy deep FIFO rewrote its queued owner: round=%d outcome=%d", round, outcome)
			}
		}
		if tail.timeoutDeferCount <= sequence.sendBufferSettings.TimeoutResendDeferLimit {
			t.Fatal("healthy control did not exceed the separate mixed-path deferral limit")
		}
		due, count := tail.resendTime, tail.timeoutDeferCount
		time.Sleep(time.Until(due))
		selection, outcome, _ := fixture.selectAt(t, tail)
		if outcome != sendRecoverySelectionReady || selection.messageId != tail.messageId || tail.timeoutDeferCount != count ||
			tail.resendTime != due || tail.deferralOutstanding || sequence.client.timeoutResendDeferCount.Load() != 6 {
			t.Fatal("silence reused old cumulative progress to keep a deep owner deferred")
		}
		fixture.close(t)
		t.Log("[timeout-progress-freshness-healthy] advancing_prefixes=6 stale_next_deadline_runnable=true callbacks_once=true final_budget=0")
	})
}

// A fresh ACK in the existing coalescer wins before retiming or dispatch.
func TestTimeoutProgressFreshnessPendingFeedbackWins(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		fixture := newTimeoutProgressFreshnessFixture(t, 2)
		sequence, prefix, tail := fixture.sequence, fixture.items[0], fixture.items[1]
		time.Sleep(2 * time.Second)
		sequence.ackWindow.Update(sequenceAck{messageId: prefix.messageId, sequenceNumber: prefix.sequenceNumber})
		_, outcome, _ := fixture.selectAt(t, tail)
		if outcome != sendRecoverySelectionFeedback || !sequence.ackWindow.PendingCumulativeProgress() ||
			sequence.client.ackPendingResendPreemptCount.Load() != 1 || sequence.client.timeoutResendDeferCount.Load() != 0 ||
			tail.timeoutDeferCount != 0 || tail.resendTime != fixture.started.Add(2*time.Second) {
			t.Fatal("due selection hid or consumed genuine pending prefix feedback")
		}
		t.Logf("[timeout-progress-freshness-pending] pending=true owners=%d", sequence.resendQueue.Len())
	})
}
