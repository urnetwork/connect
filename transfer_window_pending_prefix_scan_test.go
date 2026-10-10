// Direct owner controls complement the actual-worker masked-tail regressions.
// They exercise the existing entry point on both helper versions, without
// spending route credit, applying ACKs or constructing a second timer owner.
package connect

import (
	"testing"
	"time"
)

// The retained list and timer heap name the same owners. A is the covered
// prefix, B the eligible or legitimately exempt tail, and C the active write.
func newPendingPrefixScanFixture(t *testing.T) (*SendSequence, *sendItem, *sendItem, *sendItem, *recoveryAccountingTestWriter, *int) {
	t.Helper()
	sequence, tail, writer, observations := newRecoveryAccountingFixture(t)
	now := tail.sendTime
	prefix := &sendItem{
		transferItem: transferItem{messageId: NewId(), sequenceNumber: 0},
		sendTime: now, resendTime: now.Add(2 * time.Second),
		ackTimeout: 60 * time.Second, sendCount: 1, expectsAck: true,
	}
	current := &sendItem{
		transferItem: transferItem{messageId: NewId(), sequenceNumber: 3},
		sendTime: now, resendTime: now.Add(3 * time.Second),
		ackTimeout: 60 * time.Second, sendCount: 1, expectsAck: true,
	}
	tail.ackTimeout = 60 * time.Second
	sequence.setResendTime(tail, prefix.resendTime.Add(time.Nanosecond))
	sequence.ackLifetimes.update(tail)
	sequence.sendItems = []*sendItem{prefix, tail, current}
	sequence.addResendItem(prefix)
	sequence.addResendItem(current)
	sequence.sendBuffer = &SendBuffer{}
	return sequence, prefix, tail, current, writer, observations
}

// Cumulative feedback still exempts ordinary tracked datagrams and stable
// H1-only timeouts, but not explicit recovery or a mixed reliable-only tail.
// Equal-time peers and SACK-held lease owners remain eligible timer members.
func TestWindowPacingPendingPrefixScanPreservesEligibility(t *testing.T) {
	assertMessagePoolOwnership(t)
	cases := []struct {
		name string
		cumulative, tracked, h1Only, equal, leased, failedInitial, wantSelection bool
		kind sendRecoveryKind
	}{
		{name: "ordinary_datagram_progress", cumulative: true, tracked: true},
		{name: "ordinary_h1_progress", cumulative: true, h1Only: true},
		{name: "older_failed_initial", failedInitial: true, wantSelection: true},
		{name: "explicit_datagram_gap", cumulative: true, tracked: true, kind: sendRecoverySelectiveGap, wantSelection: true},
		{name: "explicit_h1_gap", cumulative: true, h1Only: true, kind: sendRecoverySelectiveGap, wantSelection: true},
		{name: "equal_deadline_mixed", equal: true, wantSelection: true},
		{name: "retained_selective_lease", leased: true, kind: sendRecoveryCumulativeProbe, wantSelection: true},
	}
	for _, c := range cases {
		sequence, prefix, tail, current, writer, observations := newPendingPrefixScanFixture(t)
		sequence.sendBufferSettings.DeferTimeoutResendWhileCumulativeProgress = true
		writer.policy.h1Only = c.h1Only
		tail.unreliableFlightTracked = c.tracked
		tail.transportWriteObserved = !c.failedInitial
		tail.reliableCarrierObserved = !c.failedInitial && !c.tracked
		tail.unreliableCarrierObserved = !c.failedInitial && c.tracked
		tail.recoveryKind, tail.selectiveAcked = c.kind, c.leased
		wantLifetime := sequence.ackTimeoutForPolicy(tail.unreliableRecoveryPolicy())
		tail.ackTimeout = wantLifetime
		sequence.ackLifetimes.update(tail)
		wantSendTime, wantAckDeadline := tail.sendTime, tail.sendTime.Add(wantLifetime)
		if c.failedInitial && (tail.transportWriteObserved || tail.reliableCarrierObserved || tail.unreliableCarrierObserved) {
			t.Fatal("failed first attempt unexpectedly acquired a physical result or observed carrier")
		}
		if c.equal {
			sequence.setResendTime(tail, prefix.resendTime)
		}
		if sequence.resendQueue.PeekFirst() != prefix {
			t.Fatalf("%s: the covered prefix was not the actual heap head", c.name)
		}
		sequence.ackWindow.Update(sequenceAck{
			messageId: prefix.messageId, sequenceNumber: prefix.sequenceNumber, selective: !c.cumulative,
		})
		start := windowPacingWriteStart{owner: sequence, messageId: current.messageId}
		deadline, err := start.recoveryDeadline(prefix.resendTime)
		if err != nil || c.wantSelection && !c.equal && deadline != tail.resendTime ||
			!c.wantSelection && !deadline.IsZero() {
			t.Fatalf("%s: next eligible deadline=%s want=%s err=%v", c.name, deadline, tail.resendTime, err)
		}
		_, err = start.recoveryDeadline(tail.resendTime)
		selected := sequence.pendingRecovery
		if err != nil || (selected != nil) != c.wantSelection ||
			selected != nil && (selected.messageId != tail.messageId || selected.number != tail.sequenceNumber || selected.kind != c.kind) {
			t.Fatalf("%s: selection=%+v want_tail=%t err=%v", c.name, selected, c.wantSelection, err)
		}
		count, _ := sequence.resendQueue.QueueSize()
		if count != 3 || len(sequence.sendItems) != 3 || writer.writes != 0 || *observations != 0 ||
			!sequence.ackWindow.PendingDispositionFor(prefix.sequenceNumber, prefix.messageId) ||
			sequence.ackWindow.PendingDispositionFor(tail.sequenceNumber, tail.messageId) ||
			sequence.ackWindow.PendingCumulativeProgress() != c.cumulative ||
			tail.sendCount != 1 || tail.sendTime != wantSendTime || tail.sendTime.Add(tail.ackTimeout) != wantAckDeadline {
			t.Fatalf("%s: inspection consumed ACK, wire or retained lifetime ownership", c.name)
		}
	}
}

// A new selective ACK can arrive either after selection or inside the actual
// due hook. Both paths recheck it without consuming feedback and find D behind
// A/B, retaining all four original owners and issuing no nested physical write.
func TestWindowPacingPendingPrefixScanRechecksAckAndSelection(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, duringDue := range []bool{false, true} {
		sequence, prefix, tail, current, writer, observations := newPendingPrefixScanFixture(t)
		next := &sendItem{
			transferItem: transferItem{messageId: NewId(), sequenceNumber: 2},
			sendTime: tail.sendTime, resendTime: tail.resendTime,
			ackTimeout: 60 * time.Second, sendCount: 1, expectsAck: true,
		}
		sequence.sendItems = []*sendItem{prefix, tail, next, current}
		sequence.addResendItem(next)
		sequence.ackWindow.Update(sequenceAck{messageId: prefix.messageId, sequenceNumber: 0, selective: true})
		publishTailAck := func() {
			sequence.ackWindow.Update(sequenceAck{messageId: tail.messageId, sequenceNumber: 1, selective: true})
		}
		inspected := 0
		if duringDue {
			sequence.sendBuffer.beforeDueResendForTest = func(_ sendSequenceId, number uint64) {
				if number == tail.sequenceNumber {
					inspected++
					publishTailAck()
				}
			}
		}
		start := windowPacingWriteStart{owner: sequence, messageId: current.messageId}
		if _, err := start.recoveryDeadline(tail.resendTime); err != nil {
			t.Fatal(err)
		}
		if !duringDue {
			if selection := sequence.pendingRecovery; selection == nil || selection.messageId != tail.messageId {
				t.Fatal("first uncovered owner was not actually selected before its ACK")
			}
			publishTailAck()
			if _, err := start.recoveryDeadline(tail.resendTime); err != nil {
				t.Fatal(err)
			}
		}
		selected := sequence.pendingRecovery
		if selected == nil || selected.messageId != next.messageId || selected.number != next.sequenceNumber ||
			duringDue && inspected != 1 {
			t.Fatalf("during_due=%t: newly covered B hid D: selection=%+v inspected=%d", duringDue, selected, inspected)
		}
		count, _ := sequence.resendQueue.QueueSize()
		if count != 4 || len(sequence.sendItems) != 4 || writer.writes != 0 || *observations != 0 ||
			!sequence.ackWindow.PendingDispositionFor(0, prefix.messageId) ||
			!sequence.ackWindow.PendingDispositionFor(1, tail.messageId) ||
			sequence.ackWindow.PendingDispositionFor(2, next.messageId) ||
			sequence.ackWindow.PendingCumulativeProgress() || tail.sendCount != 1 || next.sendCount != 1 {
			t.Fatal("ACK recheck consumed feedback, an owner or a physical write")
		}
	}
}

