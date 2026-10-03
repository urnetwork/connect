package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

type h1RetirementBarrierWriter struct {
	MultiRouteWriter
	entered          chan struct{}
	release          chan error
	writeBeforePause bool
}

func (w *h1RetirementBarrierWriter) Write(ctx context.Context, wire []byte, timeout time.Duration) error {
	var result error
	if w.writeBeforePause {
		result = w.MultiRouteWriter.Write(ctx, wire, timeout)
	}
	close(w.entered)
	if err := <-w.release; err != nil {
		return err
	}
	if w.writeBeforePause {
		return result
	}
	return w.MultiRouteWriter.Write(ctx, wire, timeout)
}

// Reservation, not a stale pointer read, is the admission boundary. Already
// admitted writes keep the old contract open through their final accounting;
// unreserved retired readers must return untouched to normal queue admission.
func TestH1NoAckContractRetirementWitness(t *testing.T) {
	for _, kind := range []string{"read-before-reserve", "reserved-success", "reserved-failure", "reserved-cancel", "last-ack", "no-contract", "zero-byte"} {
		t.Run(kind, func(t *testing.T) {
			assertMessagePoolOwnership(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			h := newNoAckFastPathHarness(t, ctx, 1)
			sequence, first := h.sequence, h.contract
			if !first.update(60) {
				t.Fatal("initial debit")
			}
			if kind != "last-ack" {
				first.ack(60)
			}
			before := h.client.ContractManager().LocalStats().ReceiveContractCloseByteCount
			var barrier *h1RetirementBarrierWriter
			if kind != "read-before-reserve" {
				barrier = &h1RetirementBarrierWriter{MultiRouteWriter: sequence.contractMultiRouteWriter, entered: make(chan struct{}), release: make(chan error, 1)}
				sequence.contractMultiRouteWriter = barrier
				sequence.publishNoAckFastPath()
			}
			frameSize := 40
			if kind == "zero-byte" {
				frameSize = 0
			}
			frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(frameSize)}
			clear(frame.MessageBytes)
			pack := &SendPack{TransferOptions: TransferOptions{Ack: false}, Frame: frame, Destination: h.destinationId, Ctx: ctx}
			snapshot := sequence.readNoAckFastPath(pack)
			done := make(chan bool, 1)
			if barrier != nil {
				go func() { done <- sequence.writeNoAckFastPath(snapshot, pack) }()
				<-barrier.entered
			}
			if kind == "no-contract" {
				h.client.ContractManager().AddNoContractPeer(h.destinationId)
				if !sequence.updateContract(0) {
					t.Error("no-contract transition failed")
				}
			} else {
				second := newContractAheadTestContract(t, h.client, h.destinationId)
				sequence.setContract(second, sequence.contractMetadata().generation)
				if kind == "last-ack" {
					sequence.ackItem(newContractSendItem(&first.contractId, 0, 60))
				}
			}
			if barrier != nil {
				if closed := h.client.ContractManager().LocalStats().ReceiveContractCloseByteCount - before; closed != 0 {
					t.Errorf("contract closed before accepted caller finished: closed=%d; want0", closed)
				}
				if _, open := sequence.openSendContracts[first.contractId]; !open {
					t.Error("old contract removed while accepted caller still owns reservation")
				}
				if kind == "reserved-failure" {
					barrier.release <- errTransferRouteWriteTimeout
				} else if kind == "reserved-cancel" {
					barrier.release <- context.Canceled
				} else {
					barrier.release <- nil
				}
			} else {
				done <- sequence.writeNoAckFastPath(snapshot, pack)
			}
			written := <-done
			wantWritten := kind != "read-before-reserve" && kind != "reserved-failure" && kind != "reserved-cancel"
			if written != wantWritten {
				t.Errorf("write after retirement: got%t want%t", written, wantWritten)
			}
			if !written {
				pack.disposeUnsentGroup(context.Canceled)
			}
			sequence.applyNoAckFastPathAccounting()
			wantClosed := ByteCount(60)
			if wantWritten {
				wantClosed += ByteCount(frameSize)
			}
			if closed := h.client.ContractManager().LocalStats().ReceiveContractCloseByteCount - before; closed != wantClosed {
				t.Errorf("final manager close lost or duplicated bytes: got%d want%d (old local acked=%d)", closed, wantClosed, first.ackedByteCount)
			}
			if _, open := sequence.openSendContracts[first.contractId]; open {
				t.Error("settled retired contract was not closed")
			}
			sequence.applyNoAckFastPathAccounting()
			if closed := h.client.ContractManager().LocalStats().ReceiveContractCloseByteCount - before; closed != wantClosed {
				t.Error("close was not exactly once")
			}
		})
	}
}

// Exercise Run's real terminal defer while a caller owns an accepted lease.
// The success arm has physically handed off bytes but not yet returned from
// Write; the cancellation arm owns a reservation that ultimately writes none.
func TestH1NoAckContractRunTeardownJoinsWriters(t *testing.T) {
	for _, writtenBeforeCancel := range []bool{false, true} {
		name := "canceled-before-write"
		if writtenBeforeCancel {
			name = "accepted-before-cancel"
		}
		t.Run(name, func(t *testing.T) {
			assertMessagePoolOwnership(t)
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				h := newNoAckFastPathHarness(t, ctx, 1)
				sequence, contract := h.sequence, h.contract
				// synctest starts in 2000, before the default contract rollout.
				// Require this peer explicitly so this is a real leased contract
				// test rather than a contract-free writer with unrelated counters.
				manager := h.client.ContractManager()
				manager.mutex.Lock()
				manager.sendNoContractClientIds[h.destinationId] = false
				manager.mutex.Unlock()
				if !contract.update(60) {
					t.Fatal("initial debit")
				}
				contract.ack(60)
				before := h.client.ContractManager().LocalStats().ReceiveContractCloseByteCount
				barrier := &h1RetirementBarrierWriter{MultiRouteWriter: sequence.contractMultiRouteWriter, entered: make(chan struct{}), release: make(chan error, 1), writeBeforePause: writtenBeforeCancel}
				sequence.contractMultiRouteWriter = barrier
				sequence.publishNoAckFastPath()
				frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(40)}
				clear(frame.MessageBytes)
				pack := &SendPack{TransferOptions: TransferOptions{Ack: false}, Frame: frame, Ctx: ctx, Destination: h.destinationId}
				snapshot := sequence.readNoAckFastPath(pack)
				if snapshot == nil || snapshot.contract != contract {
					pack.returnFrames()
					t.Fatal("fixture lacks the exact contract reservation gate")
				}
				callerDone := make(chan bool, 1)
				go func() { callerDone <- sequence.writeNoAckFastPath(snapshot, pack) }()
				<-barrier.entered
				// Retain the real registered selector as Run's close owner. The
				// paused caller keeps its immutable old wrapper snapshot.
				sequence.contractMultiRouteWriter = barrier.MultiRouteWriter
				sequence.publishNoAckFastPath()
				sequence.cancel()
				runDone := make(chan struct{})
				go func() { sequence.Run(); close(runDone) }()
				synctest.Wait()
				select {
				case <-runDone:
					t.Error("Run released writer/contracts before its accepted caller settled")
				default:
				}
				if closed := h.client.ContractManager().LocalStats().ReceiveContractCloseByteCount - before; closed != 0 {
					t.Errorf("terminal close preceded accepted writer: %d", closed)
				}
				if writtenBeforeCancel {
					barrier.release <- nil
				} else {
					barrier.release <- context.Canceled
				}
				written := <-callerDone
				if written != writtenBeforeCancel {
					t.Errorf("caller disposition: written=%t want=%t", written, writtenBeforeCancel)
				}
				if !written {
					pack.disposeUnsentGroup(context.Canceled)
				}
				<-runDone
				synctest.Wait()
				wantClosed := ByteCount(60)
				if writtenBeforeCancel {
					wantClosed = 100
				}
				sequence.applyNoAckFastPathAccounting()
				if closed := h.client.ContractManager().LocalStats().ReceiveContractCloseByteCount - before; closed != wantClosed || len(sequence.openSendContracts) != 0 {
					t.Errorf("final terminal close: bytes=%d want=%d open=%d", closed, wantClosed, len(sequence.openSendContracts))
				}
				if snapshot.reserve(0) {
					snapshot.release(0)
					t.Error("terminal retirement admitted a zero-byte writer")
				}
			})
		})
	}
}
