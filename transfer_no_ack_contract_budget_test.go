package connect

import (
	"context"
	"strconv"
	"testing"

	"github.com/urnetwork/connect/protocol"
)

// This fixture publishes only after pinning the allowance; it has no Run,
// pacing cooperator, observer, real socket, or scheduled timing dependency.
func newNoAckBudgetHarness(t *testing.T, ctx context.Context, configure ...func(*sequenceContract)) *noAckFastPathHarness {
	t.Helper()
	client, sequence, peer, contract := newSendNoContractHarness(t, ctx)
	for _, change := range configure {
		change(contract)
	}
	h := &noAckFastPathHarness{client: client, sequence: sequence, destinationId: peer, contract: contract, route: make(chan []byte, 1)}
	sequence.sendBuffer = client.sendBuffer
	client.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(peer)), []Route{h.route})
	sequence.openContractMultiRouteWriter()
	t.Cleanup(func() {
		sequence.cancel()
		sequence.closeContractMultiRouteWriter()
		h.drainQueue()
		for len(h.route) > 0 {
			MessagePoolReturn(<-h.route)
		}
	})
	return h
}

func TestNoAckContractBudgetUsesOrdinaryDebits(t *testing.T) {
	for _, bytes := range []int{40, 80} {
		t.Run(strconv.Itoa(bytes), func(t *testing.T) {
			assertMessagePoolOwnership(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			h := newNoAckBudgetHarness(t, ctx, func(c *sequenceContract) { c.transferByteCount, c.effectiveTransferByteCount = 100, 100 })
			sequence, contract := h.sequence, h.contract
			snapshot := sequence.noAckFastPath.Load()
			if !contract.update(60) {
				t.Fatal("ordinary debit refused")
			}
			for attempt := 0; attempt < 2; attempt++ {
				frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(bytes)}
				clear(frame.MessageBytes)
				pack := &SendPack{TransferOptions: TransferOptions{Ack: false}, Frame: frame, Ctx: ctx, Destination: h.destinationId}
				written := sequence.writeNoAckFastPath(snapshot, pack)
				want := bytes == 40 && attempt == 0
				if written != want {
					t.Errorf("write %d size %d: got %t want %t", attempt, bytes, written, want)
				}
				if written {
					MessagePoolReturn(<-h.route)
				} else {
					pack.disposeUnsentGroup(context.Canceled)
				}
			}
			sequence.applyNoAckFastPathAccounting()
			wantUsed := ByteCount(60)
			if bytes == 40 {
				wantUsed = 100
			}
			if used := contract.ackedByteCount + contract.unackedByteCount; used != wantUsed || used > 100 {
				t.Fatalf("contract used=%d want=%d allowance=100", used, wantUsed)
			}
		})
	}
}

func TestNoAckContractBudgetConcurrentWithOrdinaryDebit(t *testing.T) {
	for _, kind := range []string{"caller-first", "owner-first", "caller-first-republish", "owner-first-republish", "canceled-caller-republish"} {
		t.Run(kind, func(t *testing.T) {
			assertMessagePoolOwnership(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			h := newNoAckBudgetHarness(t, ctx, func(c *sequenceContract) { c.transferByteCount, c.effectiveTransferByteCount = 100, 100 })
			sequence, contract := h.sequence, h.contract
			writer := sequence.contractMultiRouteWriter
			defer func() { sequence.contractMultiRouteWriter = writer }()
			barrier := &h1RetirementBarrierWriter{MultiRouteWriter: writer, entered: make(chan struct{}), release: make(chan error, 1)}
			sequence.contractMultiRouteWriter = barrier
			sequence.publishNoAckFastPath()
			snapshot := sequence.noAckFastPath.Load()
			if !contract.update(60) {
				t.Fatal("initial ordinary debit refused")
			}
			ownerFirst := kind == "owner-first" || kind == "owner-first-republish"
			canceled := kind == "canceled-caller-republish"
			if ownerFirst && !contract.update(40) {
				t.Fatal("owner failed to consume remaining allowance")
			}
			if kind != "caller-first" && kind != "owner-first" {
				sequence.retireNoAckFastPath()
				sequence.contractMultiRouteWriter = writer
				sequence.publishNoAckFastPath()
				if sequence.noAckFastPath.Load() == snapshot {
					t.Fatal("fixture did not republish")
				}
			}
			frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(40)}
			clear(frame.MessageBytes)
			completed := 0
			pack := &SendPack{TransferOptions: TransferOptions{Ack: false}, Frame: frame, Ctx: ctx, Destination: h.destinationId, AckCallback: func(err error) {
				if err == nil {
					completed++
				}
			}}
			done := make(chan bool, 1)
			go func() { done <- sequence.writeNoAckFastPath(snapshot, pack) }()
			if !ownerFirst {
				<-barrier.entered
				if contract.canUpdate(40) || contract.update(40) {
					t.Error("ordinary debit spent the accepted caller reservation")
				}
			}
			if canceled {
				barrier.release <- context.Canceled
			} else {
				barrier.release <- nil
			}
			written := <-done
			wantWritten := !ownerFirst && !canceled
			if written != wantWritten {
				t.Errorf("caller written=%t want=%t", written, wantWritten)
			}
			if written {
				MessagePoolReturn(<-h.route)
			} else {
				pack.disposeUnsentGroup(context.Canceled)
			}
			sequence.applyNoAckFastPathAccounting()
			if canceled && !contract.update(40) {
				t.Error("canceled caller did not restore owner headroom")
			}
			wantCompleted := 0
			if wantWritten {
				wantCompleted = 1
			}
			if used := contract.ackedByteCount + contract.unackedByteCount; used != 100 || completed != wantCompleted || len(h.route) != 0 {
				t.Errorf("final ownership: used=%d completed=%d want=%d wire=%d", used, completed, wantCompleted, len(h.route))
			}
		})
	}
}

func TestNoAckContractContractMinimumChargeAndRollback(t *testing.T) {
	assertMessagePoolOwnership(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	harness := newNoAckBudgetHarness(t, ctx, func(contract *sequenceContract) {
		contract.transferByteCount, contract.effectiveTransferByteCount = 100, 100
		contract.minUpdateByteCount = 32
	})
	sequence, contract := harness.sequence, harness.contract
	snapshot := sequence.noAckFastPath.Load()
	if !contract.update(60) || snapshot.remainingByteCount.Load() != 40 {
		t.Fatal("ordinary debit did not reserve caller-visible headroom")
	}
	contract.rollbackUnwritten(60)
	if snapshot.remainingByteCount.Load() != 100 || !contract.update(60) {
		t.Fatal("unwritten ordinary rollback did not restore its reservation")
	}
	completed := 0
	newPack := func() *SendPack {
		frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(1)}
		clear(frame.MessageBytes)
		return &SendPack{TransferOptions: TransferOptions{Ack: false}, Frame: frame, Ctx: ctx, Destination: harness.destinationId, AckCallback: func(err error) {
			if err == nil {
				completed++
			}
		}}
	}
	first := newPack()
	harness.route <- MessagePoolGet(1)
	if sequence.writeNoAckFastPath(snapshot, first) || completed != 0 || snapshot.remainingByteCount.Load() != 40 || snapshot.reservedByteCount.Load() != 0 {
		t.Fatal("failed physical write consumed minimum-charge reservation or source ownership")
	}
	MessagePoolReturn(<-harness.route)
	if !sequence.writeNoAckFastPath(snapshot, first) || completed != 1 {
		t.Fatal("first tiny write failed after pressure cleared")
	}
	MessagePoolReturn(<-harness.route)
	sequence.applyNoAckFastPathAccounting()
	if contract.ackedByteCount != 32 || contract.unackedByteCount != 60 || snapshot.remainingByteCount.Load() != 8 {
		t.Fatalf("minimum charge mismatch: acked=%d unacked=%d remaining=%d", contract.ackedByteCount, contract.unackedByteCount, snapshot.remainingByteCount.Load())
	}
	second := newPack()
	if sequence.writeNoAckFastPath(snapshot, second) {
		t.Error("one-byte source ignored the 32-byte minimum charge")
	} else {
		second.disposeUnsentGroup(context.Canceled)
	}
	contract.rollbackUnwritten(60)
	if snapshot.remainingByteCount.Load() != 68 || contract.unackedByteCount != 0 || contract.ackedByteCount != 32 {
		t.Fatal("ordinary rollback returned another writer's charge")
	}
	sequence.retireNoAckFastPath()
	sequence.publishNoAckFastPath()
	republished := sequence.noAckFastPath.Load()
	if republished.remainingByteCount != snapshot.remainingByteCount || republished.remainingByteCount.Load() != 68 {
		t.Fatal("retirement/republish created a second independent allowance")
	}
	if completed != 1 || len(harness.route) != 0 {
		t.Fatal("failed budget try transferred source ownership or fired success")
	}
}

func TestNoAckContractOrdinaryDebitSeesInFlightCaller(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	harness := newNoAckBudgetHarness(t, ctx, func(contract *sequenceContract) {
		contract.transferByteCount, contract.effectiveTransferByteCount = 100, 100
	})
	sequence, contract := harness.sequence, harness.contract
	snapshot := sequence.noAckFastPath.Load()
	if !snapshot.reserve(60) {
		t.Fatal("caller reservation failed")
	}
	if contract.canUpdate(60) || contract.update(60) {
		t.Fatal("ordinary send spent the paused caller's reservation")
	}
	if !contract.update(40) || snapshot.remainingByteCount.Load() != 0 {
		t.Fatal("ordinary send could not reserve the exact remainder")
	}
	sequence.recordNoAckFastPathWrite(snapshot, 60)
	sequence.applyNoAckFastPathAccounting()
	if contract.ackedByteCount != 60 || contract.unackedByteCount != 40 || snapshot.remainingByteCount.Load() != 0 {
		t.Fatal("applying the caller accounting debited its reservation twice")
	}
	contract.ack(40)
	if contract.ackedByteCount != 100 || contract.unackedByteCount != 0 || snapshot.remainingByteCount.Load() != 0 || snapshot.reserve(1) {
		t.Fatal("reliable ACK incorrectly replenished spent contract headroom")
	}
}

func TestNoAckContractBudgetRemainsLazyAndPreservesExistingDebits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, sequence, _, contract := newSendNoContractHarness(t, ctx)
	contract.transferByteCount, contract.effectiveTransferByteCount = 100, 100
	if contract.noAckBudgetPublished || !contract.update(60) || !contract.canUpdate(40) {
		t.Fatal("unpublished contract lost ordinary owner-only accounting")
	}
	contract.rollbackUnwritten(60)
	if contract.noAckBudgetPublished || contract.noAckRemainingByteCount.Load() != 0 || !contract.update(60) {
		t.Fatal("unpublished update/rollback activated or mutated the shared budget")
	}
	// First publication must subtract earlier ordinary debits, not start a
	// fresh full allowance; later ACKs retain their original debit semantics.
	sequence.contractMultiRouteWriter = &windowPacingPolicyWriter{}
	sequence.publishNoAckFastPath()
	snapshot := sequence.noAckFastPath.Load()
	if snapshot == nil || snapshot.remainingByteCount.Load() != 40 || !contract.noAckBudgetPublished {
		t.Fatal("first publication forgot preexisting reliable bytes")
	}
	contract.ack(60)
	if contract.ackedByteCount != 60 || contract.unackedByteCount != 0 || snapshot.remainingByteCount.Load() != 40 {
		t.Fatal("ordinary acknowledgement changed total used capacity")
	}
	if !contract.update(40) || contract.canUpdate(1) || contract.update(1) {
		t.Fatal("ordinary acknowledged contract exceeded its established boundary")
	}
}
