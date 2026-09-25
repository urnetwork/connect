// Receive expiry reconciles one finite handoff prefix through normal parsing,
// delivery and ownership before concluding that its predecessor is absent.
package connect

import (
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// A Pack owns its pooled frame until the worker or rejected sender returns it.
func receiveGapHandoffPackTest(sequence *ReceiveSequence, number uint64) *ReceivePack {
	return &ReceivePack{
		Pack: &protocol.Pack{MessageId: NewId().Bytes(), SequenceNumber: number,
			Frames: []*protocol.Frame{{MessageType: protocol.MessageType_TransferExchangeSignals}}},
		MessageByteCount: 16, TransferFrameBytes: MessagePoolGet(16), ReceiveCallback: sequence.client.receiveCallback,
	}
}

// Only the actual Pack boundary may transfer a fixture's ownership to Run.
func admitReceiveGapHandoffPackTest(t *testing.T, sequence *ReceiveSequence, pack *ReceivePack) {
	t.Helper()
	if admitted, err := sequence.Pack(pack, 0); !admitted || err != nil {
		pack.messagePoolReturn()
		t.Fatalf("admit receive handoff: accepted=%t err=%v", admitted, err)
	}
}

// Tests await Run's ownership barrier before closing and draining its channel.
func startReceiveGapHandoffTest(t *testing.T, sequence *ReceiveSequence) {
	t.Helper()
	go sequence.Run()
	t.Cleanup(func() {
		sequence.Cancel()
		waitReceiveGapTestExit(t, sequence)
	})
}

// One or several already-admitted predecessors close the old hole. A newly
// advanced delivery point may reconcile the remaining prefix for its own hole.
func TestReceiveSequenceGapHandoffClosesAdmittedHole(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, missing := range []uint64{1, 2} {
		sequence, log := receiveGapSequenceTest(t)
		t.Cleanup(sequence.Close)
		var delivered atomic.Int32
		sequence.client.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
			delivered.Add(int32(len(frames)))
			sequence.Cancel()
		})
		addReceiveGapTestItem(t, sequence, missing, time.Now().Add(-time.Hour), true)
		for number := uint64(0); number < missing; number++ {
			admitReceiveGapHandoffPackTest(t, sequence, receiveGapHandoffPackTest(sequence, number))
		}
		startReceiveGapHandoffTest(t, sequence)
		waitReceiveGapTestExit(t, sequence)
		if delivered.Load() != int32(missing+1) || sequence.nextSequenceNumber != missing+1 || len(log.messages()) != 0 || sequence.packQueueCount.Load() != 0 {
			t.Fatalf("admitted prefix lost progress: missing=%d delivered=%d next=%d retained=%d logs=%v", missing, delivered.Load(), sequence.nextSequenceNumber, sequence.packQueueCount.Load(), log.messages())
		}
	}
}

// New traffic behind the captured prefix cannot extend an expired hole. This
// includes a duplicate which can refresh a queued item's own arrival time.
func TestReceiveSequenceGapHandoffDoesNotRefillExpiredPrefix(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, first := range []uint64{1, 2} {
		sequence, log := receiveGapSequenceTest(t)
		t.Cleanup(sequence.Close)
		var delivered atomic.Int32
		sequence.client.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) { delivered.Add(int32(len(frames))) })
		addReceiveGapTestItem(t, sequence, 1, time.Now().Add(-time.Hour), true)
		admitReceiveGapHandoffPackTest(t, sequence, receiveGapHandoffPackTest(sequence, first))
		late := receiveGapHandoffPackTest(sequence, 0)
		var snapshots, prefix int
		var admitted bool
		var admissionErr error
		t.Cleanup(func() {
			if snapshots == 0 {
				late.messagePoolReturn()
			}
		})
		sequence.receiveBufferSettings.afterGapHandoffSnapshotForTest = func(_ receiveSequenceId, count int) {
			snapshots++
			if snapshots != 1 {
				return
			}
			prefix = count
			admitted, admissionErr = sequence.Pack(late, 0)
			if !admitted {
				late.messagePoolReturn()
			}
		}
		startReceiveGapHandoffTest(t, sequence)
		waitReceiveGapTestExit(t, sequence)
		entries := log.messages()
		if snapshots != 1 || prefix != 1 || !admitted || admissionErr != nil || sequence.nextSequenceNumber != 0 || delivered.Load() != 0 || len(sequence.packs) != 1 || len(entries) != 1 || !strings.Contains(entries[0], "exit gap timeout expected=0 queued=1") {
			t.Fatalf("expired prefix was refilled: first=%d snapshots=%d prefix=%d admitted=%t err=%v next=%d delivered=%d queued=%d logs=%v", first, snapshots, prefix, admitted, admissionErr, sequence.nextSequenceNumber, delivered.Load(), len(sequence.packs), entries)
		}
	}
}

// Cancellation between the census and receive must win over a now-ready
// predecessor, without dispatch or a fabricated missing-packet diagnostic.
func TestReceiveSequenceGapHandoffCancellationPreservesOwnership(t *testing.T) {
	assertMessagePoolOwnership(t)
	sequence, log := receiveGapSequenceTest(t)
	t.Cleanup(sequence.Close)
	var delivered atomic.Int32
	sequence.client.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) { delivered.Add(int32(len(frames))) })
	addReceiveGapTestItem(t, sequence, 1, time.Now().Add(-time.Hour), true)
	admitReceiveGapHandoffPackTest(t, sequence, receiveGapHandoffPackTest(sequence, 0))
	var snapshots int
	sequence.receiveBufferSettings.afterGapHandoffSnapshotForTest = func(_ receiveSequenceId, _ int) {
		snapshots++
		sequence.Cancel()
	}
	startReceiveGapHandoffTest(t, sequence)
	waitReceiveGapTestExit(t, sequence)
	if snapshots != 1 || delivered.Load() != 0 || sequence.nextSequenceNumber != 0 || len(log.messages()) != 0 || len(sequence.packs) != 1 {
		t.Fatalf("canceled census dispatched or timed out: snapshots=%d delivered=%d next=%d retained=%d logs=%v", snapshots, delivered.Load(), sequence.nextSequenceNumber, len(sequence.packs), log.messages())
	}
}

// Reconciliation never bypasses the ordinary Pack parser or trades a malformed
// packet for a benign timeout. Both queues release their exact owned buffers.
func TestReceiveSequenceGapHandoffRetainsPacketValidation(t *testing.T) {
	assertMessagePoolOwnership(t)
	sequence, log := receiveGapSequenceTest(t)
	t.Cleanup(sequence.Close)
	addReceiveGapTestItem(t, sequence, 1, time.Now().Add(-time.Hour), true)
	pack := receiveGapHandoffPackTest(sequence, 0)
	pack.Pack.MessageId = nil
	admitReceiveGapHandoffPackTest(t, sequence, pack)
	startReceiveGapHandoffTest(t, sequence)
	waitReceiveGapTestExit(t, sequence)
	entries := log.messages()
	if sequence.nextSequenceNumber != 0 || sequence.packQueueCount.Load() != 0 || len(entries) != 1 || !strings.Contains(entries[0], "Bad message_id") || strings.Contains(entries[0], "gap timeout") {
		t.Fatalf("handoff bypassed validation: next=%d retained=%d logs=%v", sequence.nextSequenceNumber, sequence.packQueueCount.Load(), entries)
	}
}

// An unbuffered ready producer has no len(channel) census. It gets one immediate
// rendezvous through Pack; the expiry path never waits for a future producer.
func TestReceiveSequenceGapHandoffReconcilesReadyRendezvous(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		sequence, log := receiveGapSequenceTest(t)
		t.Cleanup(sequence.Close)
		sequence.packs = make(chan *ReceivePack)
		var delivered atomic.Int32
		sequence.client.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
			delivered.Add(int32(len(frames)))
			sequence.Cancel()
		})
		addReceiveGapTestItem(t, sequence, 1, time.Now().Add(-time.Hour), true)
		pack := receiveGapHandoffPackTest(sequence, 0)
		type admissionResult struct {
			admitted bool
			err      error
		}
		result := make(chan admissionResult, 1)
		go func() {
			admitted, err := sequence.Pack(pack, -1)
			if !admitted {
				pack.messagePoolReturn()
			}
			result <- admissionResult{admitted: admitted, err: err}
		}()
		synctest.Wait()
		if sequence.packQueueCount.Load() != 1 {
			t.Fatal("unbuffered producer did not reach its reserved send")
		}
		startReceiveGapHandoffTest(t, sequence)
		waitReceiveGapTestExit(t, sequence)
		admission := <-result
		if !admission.admitted || admission.err != nil || delivered.Load() != 2 || sequence.nextSequenceNumber != 2 || len(log.messages()) != 0 {
			t.Fatalf("ready rendezvous lost predecessor: admitted=%t err=%v delivered=%d next=%d logs=%v", admission.admitted, admission.err, delivered.Load(), sequence.nextSequenceNumber, log.messages())
		}
	})
}

// Consuming an irrelevant fixed prefix does not re-arm a gap timer or wait for
// its missing predecessor. Virtual time remains exactly at the original cut.
func TestReceiveSequenceGapHandoffPreservesExactDeadline(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		sequence, log := receiveGapSequenceTest(t)
		t.Cleanup(sequence.Close)
		start := time.Now()
		addReceiveGapTestItem(t, sequence, 1, start.Add(-sequence.receiveBufferSettings.GapTimeout), true)
		admitReceiveGapHandoffPackTest(t, sequence, receiveGapHandoffPackTest(sequence, 2))
		startReceiveGapHandoffTest(t, sequence)
		waitReceiveGapTestExit(t, sequence)
		entries := log.messages()
		if time.Since(start) != 0 || sequence.nextSequenceNumber != 0 || len(entries) != 1 || !strings.Contains(entries[0], "exit gap timeout expected=0 queued=1") {
			t.Fatalf("irrelevant prefix changed expiry: elapsed=%s next=%d logs=%v", time.Since(start), sequence.nextSequenceNumber, entries)
		}
	})
}
