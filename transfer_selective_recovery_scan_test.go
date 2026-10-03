// Recovery scans are driven by lifetime evidence, not ordinary cumulative
// progress or the presence of a selective acknowledgement in one snapshot.
package connect

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Count work directly across deep flights without a host-speed assertion.
func TestSelectiveRecoveryWithoutEvidenceDoesNotScanFlight(t *testing.T) {
	for _, count := range []int{32, 1024, 16384} {
		start := time.Unix(1700000000, 0)
		sequence, items := newSelectiveAckRecoveryTestSequence(count, start)
		scans := 0
		sequence.beforeSelectiveAckRecoveryForTest = func() { scans++ }
		for range 8 {
			if sequence.scheduleSelectiveAckRecoveryAfterFeedback(start.Add(time.Second)) {
				t.Fatal("no selective evidence invented unreliable loss")
			}
		}
		if scans != 0 {
			t.Fatalf("retained=%d: ordinary feedback performed %d full-flight scans without evidence", count, scans)
		}
		for _, item := range items {
			if item.resendTime != start.Add(sequence.sendBufferSettings.SelectiveAckTimeout) || item.recoveryKind != sendRecoveryNone {
				t.Fatal("no-evidence feedback changed a recovery timer")
			}
		}
	}
}

// A missing selective identity and real cumulative progress are not loss proof.
func TestSelectiveRecoveryCumulativeAndStaleFeedbackStayDormant(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		sequence, items, _ := newLaneHeadPromotionSequence(t)
		scans := 0
		sequence.beforeSelectiveAckRecoveryForTest = func() { scans++ }
		sequence.receiveAck(NewId(), true, sequenceTag{}, false)
		sequence.scheduleSelectiveAckRecoveryAfterFeedback(time.Now())
		sequence.receiveAck(items[0].messageId, false, sequenceTag{}, false)
		sequence.scheduleSelectiveAckRecoveryAfterFeedback(time.Now())
		if scans != 0 || sequence.selectiveAckObserved || sequence.selectiveGapRecoveryActive || len(sequence.sendItems) != 3 {
			t.Fatalf("cumulative or stale feedback enabled recovery: scans=%d selective=%t active=%t retained=%d", scans, sequence.selectiveAckObserved, sequence.selectiveGapRecoveryActive, len(sequence.sendItems))
		}
	})
}

// Applying real selective feedback must enable both gap and cumulative probes.
func TestSelectiveRecoveryAppliedFeedbackKeepsGapAndCumulativeProbes(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, firstSelective := range []int{1, 0} {
		synctest.Test(t, func(t *testing.T) {
			sequence, items, _ := newLaneHeadPromotionSequence(t)
			for _, item := range items[firstSelective:] {
				sequence.receiveAck(item.messageId, true, sequenceTag{}, false)
			}
			sequence.scheduleSelectiveAckRecoveryAfterFeedback(time.Now())
			want := sendRecoverySelectiveGap
			if firstSelective == 0 {
				want = sendRecoveryCumulativeProbe
			}
			if !sequence.selectiveAckObserved || items[0].recoveryKind != want {
				t.Fatalf("first-selective=%d: applied evidence lost recovery kind=%d want=%d", firstSelective, items[0].recoveryKind, want)
			}
		})
	}
}

// Eviction clears an item's selective mark, not the sequence's history. An
// already-active gap must also retain its bounded later cumulative tail probe.
func TestSelectiveRecoveryClearedEvidenceKeepsHistoryAndActiveTail(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		sequence, items, _ := newLaneHeadPromotionSequence(t)
		scans := 0
		sequence.beforeSelectiveAckRecoveryForTest = func() { scans++ }
		sequence.receiveAck(items[3].messageId, true, sequenceTag{}, false)
		sequence.resendEvicted([]uint64{items[3].sequenceNumber})
		sequence.scheduleSelectiveAckRecoveryAfterFeedback(time.Now())
		if !sequence.selectiveAckObserved || items[3].selectiveAcked || scans != 1 {
			t.Fatal("cleared selective mark erased lifetime recovery evidence")
		}
		// Honor the independent active-recovery state even for a fixture that
		// predates the lifetime observation flag.
		sequence.selectiveAckObserved = false
		sequence.selectiveGapRecoveryActive = true
		sequence.scheduleSelectiveAckRecoveryAfterFeedback(time.Now())
		if scans != 2 || items[0].recoveryKind != sendRecoveryAckTailProbe || items[0].ackTailProbeCount != 1 {
			t.Fatal("active recovery lost its evidence-free cumulative tail probe")
		}
	})
}

// Real cumulative progress retires all selective marks while leaving one new
// tail. The sequence's established gap history must still solicit its reply.
func TestSelectiveRecoveryCumulativeProgressKeepsActiveHistory(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		sequence, items, route := newLaneHeadPromotionSequence(t)
		tail := &sendItem{
			transferItem: transferItem{messageId: NewId(), sequenceNumber: 5, messageByteCount: 1},
			sendTime:     time.Now(), resendTime: time.Now().Add(sequence.sendBufferSettings.MaxResendInterval),
			sendCount: 1, transferFrameBytes: MessagePoolGet(1),
		}
		sequence.sendItems = append(sequence.sendItems, tail)
		sequence.resendQueue.Add(tail)
		sequence.observeCarrierWrite(tail, transferWriteDisposition{route: route, reliable: true})
		for _, item := range items[1:] {
			sequence.receiveAck(item.messageId, true, sequenceTag{}, false)
		}
		sequence.scheduleSelectiveAckRecoveryAfterFeedback(time.Now())
		if !sequence.selectiveGapRecoveryActive || items[0].recoveryKind != sendRecoverySelectiveGap {
			t.Fatal("fixture did not establish a real selective gap")
		}
		sequence.receiveAck(items[3].messageId, false, sequenceTag{}, false)
		sequence.scheduleSelectiveAckRecoveryAfterFeedback(time.Now())
		if !sequence.selectiveAckObserved || !sequence.selectiveGapRecoveryActive || len(sequence.sendItems) != 1 ||
			sequence.sendItems[0] != tail || tail.selectiveAcked || tail.recoveryKind != sendRecoveryAckTailProbe || tail.ackTailProbeCount != 1 {
			t.Fatal("cumulative progress erased the later tail's recovery history")
		}
	})
}

// Exercise the worker's actual feedback entry, not just the isolated gate. The
// fixture joins each transition before inspecting the send-worker-owned state.
func TestSelectiveRecoveryHealthyWorkerBypassesFlightScan(t *testing.T) {
	scans := 0
	testWindowPacingProbeRecoveryFixture(t, nil, func(sequence *SendSequence) {
		sequence.beforeSelectiveAckRecoveryForTest = func() { scans++ }
	}, func(t *testing.T, sequence *SendSequence, client *Client, _ Route, pack *protocol.Pack, _ time.Time, callbacks <-chan error) {
		acknowledgeSendPackLifecycleWirePack(t, client, sequence.destination, pack)
		synctest.Wait()
		select {
		case err := <-callbacks:
			if err != nil {
				t.Fatalf("healthy cumulative reply failed: %v", err)
			}
		default:
			t.Fatal("worker did not apply its actual cumulative reply")
		}
		if scans != 0 || sequence.selectiveAckObserved || sequence.selectiveGapRecoveryActive || sequence.resendQueue.Len() != 0 {
			t.Fatalf("healthy worker performed %d recovery scans or retained acknowledged work", scans)
		}
	})
}
