// Gap wakes follow new recovery evidence, rather than the worker's decision
// to take another snapshot. All worker timing below runs in virtual time.
package connect

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

func gapWakePending(window *sequenceAckWindow) bool {
	select {
	case <-window.GapNotify():
		return true
	default:
		return false
	}
}

func TestGapWakeRepairsTheFirstCumulativeHead(t *testing.T) {
	window := newSequenceAckWindowWithGapWake(3)
	for number := uint64(1); number <= 3; number++ {
		window.Update(sequenceAck{sequenceNumber: number, messageId: NewId(), selective: true})
	}
	window.Snapshot(true)
	window.Update(sequenceAck{sequenceNumber: 3, messageId: NewId()})
	if !gapWakePending(window) {
		t.Fatal("repair of initial Pack zero waited for compression because no prior head existed")
	}
}

func TestGapWakeDoesNotReproveTheSameHeadEverySnapshot(t *testing.T) {
	window := newSequenceAckWindowWithGapWake(3)
	window.Update(sequenceAck{sequenceNumber: 0, messageId: NewId()})
	window.Snapshot(true)
	for group := range 8 {
		for offset := range 3 {
			window.Update(sequenceAck{
				sequenceNumber: uint64(2 + 3*group + offset), messageId: NewId(), selective: true,
			})
		}
		if got := gapWakePending(window); got != (group == 0) {
			t.Fatalf("group %d reproved unchanged missing head: wake=%t", group, got)
		}
		window.Snapshot(true)
	}
	window.Update(sequenceAck{sequenceNumber: 1, messageId: NewId()})
	if !gapWakePending(window) {
		t.Fatal("new cumulative progress did not wake recovery")
	}
}

// Evidence already sent in two snapshots is still evidence at the sender.
func TestGapWakeCountsEvidenceAcrossSnapshots(t *testing.T) {
	window := newSequenceAckWindowWithGapWake(3)
	window.Update(sequenceAck{sequenceNumber: 0, messageId: NewId()})
	window.Snapshot(true)
	for number := uint64(2); number <= 4; number++ {
		window.Update(sequenceAck{sequenceNumber: number, messageId: NewId(), selective: true})
		if got := gapWakePending(window); got != (number == 4) {
			t.Fatalf("evidence at sequence %d: wake=%t", number, got)
		}
		window.Snapshot(true)
	}
}

func TestGapWakeWorkerCompressesRepeatedProof(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		sequence := newAckGapTestSequence(t, func(settings *ReceiveBufferSettings) {
			settings.AckCompressTimeout = 10 * time.Millisecond
		})
		sequence.prime(t)
		synctest.Wait()
		readNow := func() int {
			synctest.Wait()
			count := 0
			for {
				select {
				case b := <-sequence.route:
					frame := &protocol.TransferFrame{}
					err := ProtoUnmarshal(b, frame)
					MessagePoolReturn(b)
					if err != nil || frame.Ack == nil {
						t.Fatalf("unexpected captured acknowledgement: %v", err)
					}
					count++
				default:
					return count
				}
			}
		}
		for n := uint64(2); n <= 4; n++ {
			sequence.updateSelective(n)
		}
		if count := readNow(); count != 3 {
			t.Fatalf("first proof wrote %d acknowledgements, want 3", count)
		}
		for n := uint64(5); n <= 7; n++ {
			sequence.updateSelective(n)
		}
		if count := readNow(); count != 0 {
			t.Fatalf("same missing head bypassed compression again: %d acknowledgements", count)
		}
		time.Sleep(10 * time.Millisecond)
		if count := readNow(); count != 3 {
			t.Fatalf("compressed proof lost %d acknowledgements, want 3", count)
		}
	})
}
