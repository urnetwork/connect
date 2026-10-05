package connect

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Only the first (sender) client contributes to the payload ledger. The actual
// native-wire peer is explicitly outside the measured owner scope.
func payloadLedgerAckFixture(t *testing.T, ledger *TransferPayloadOwnerLedger, logger Logger, version int) *windowRoundFixture {
	t.Helper()
	n := 0
	f, _, _ := newAckRetirementFixture(t, TransportTypeH1, version, logger, func(s *ClientSettings) {
		s.SendBufferSize, s.ForwardBufferSize = 4096, 4096
		s.SendBufferSettings.SequenceBufferSize, s.SendBufferSettings.AckBufferSize = 4096, 4096
		s.ReceiveBufferSettings.SequenceBufferSize = 4096
		s.ForwardBufferSettings.SequenceBufferSize = 4096
		s.SendBufferSettings.AckTimeout = 60 * time.Second
		if n == 0 {
			s.PayloadOwnerLedger = ledger
		}
		n++
	})
	return f
}

func payloadLedgerSendCharges(t *testing.T, s *SendSequence) (int64, int64) {
	t.Helper()
	synctest.Wait()
	var count, bytes int64
	for _, item := range s.sendItems {
		if item != nil {
			count++
			bytes += int64(cap(item.transferFrameBytes))
		}
	}
	return count, bytes
}

func TestTransferPayloadOwnerNativeAckBlackhole(t *testing.T) {
	assertMessagePoolOwnership(t)
	oldCopy := DebugTransferCopyOnWrite
	DebugTransferCopyOnWrite = false
	defer func() { DebugTransferCopyOnWrite = oldCopy }()
	for _, recovery := range []bool{true, false} {
		t.Run(fmt.Sprintf("recover=%t", recovery), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var ledger TransferPayloadOwnerLedger
				f := payloadLedgerAckFixture(t, &ledger, NewNoopLogger(), 2)
				const count = 256
				terminals := make(chan error, count)
				var latestAck *windowRoundFrame
				for i := range count {
					frame := budgetTestFrame(1200)
					ok, err := f.sender.SendWithTimeoutDetailed(frame, f.receiver.ClientId(), func(err error) { terminals <- err }, 0)
					if !ok || err != nil {
						MessagePoolReturn(frame.MessageBytes)
						t.Fatalf("native send %d admission=%t err=%v", i, ok, err)
					}
					ack := f.receive(f.takePack(uint64(i)))
					if latestAck != nil {
						f.drop(latestAck)
					}
					latestAck = ack
				}
				sequence := f.sequence()
				owners, heldBytes := payloadLedgerSendCharges(t, sequence)
				if owners != count || heldBytes <= 0 || len(terminals) != 0 || f.deliveredCount != count {
					t.Fatalf("native ACK blackout not established: owners=%d bytes=%d callbacks=%d delivered=%d", owners, heldBytes, len(terminals), f.deliveredCount)
				}
				requirePayloadLedger(t, &ledger, count, 0, heldBytes)
				canceled, stop := context.WithCancel(context.Background())
				stop()
				frame := budgetTestFrame(1200)
				if ok, _ := f.sender.SendWithTimeoutDetailed(frame, f.receiver.ClientId(), nil, 0, Ctx(canceled)); ok {
					t.Fatal("pre-canceled Send was admitted")
				}
				MessagePoolReturn(frame.MessageBytes)
				requirePayloadLedger(t, &ledger, count, 0, heldBytes)
				if recovery {
					f.forward(latestAck, f.senderIn)
				} else {
					// Native retries may fill the physical route; the sequence's
					// existing ACK deadline and write budgets own its retirement.
					time.Sleep(2 * time.Minute)
					synctest.Wait()
				}
				if len(terminals) != count {
					t.Fatalf("terminal ACK callbacks=%d want=%d", len(terminals), count)
				}
				for range count {
					if err := <-terminals; (err == nil) != recovery {
						t.Fatalf("terminal outcome changed: recover=%t err=%v", recovery, err)
					}
				}
				final := requirePayloadLedger(t, &ledger, 0, 0, 0)
				if final.SendAck.AdmittedTotal != count || final.SendAck.ReleasedTotal != count {
					t.Fatalf("retry or refusal changed owner count: %+v", final)
				}
				if recovery {
					noAck := make(chan error, 1)
					frame := budgetTestFrame(1200)
					ok, err := f.sender.SendWithTimeoutDetailed(frame, f.receiver.ClientId(), func(err error) { noAck <- err }, 0, NoAck())
					if !ok || err != nil {
						MessagePoolReturn(frame.MessageBytes)
						t.Fatalf("live NoAck admission=%t err=%v", ok, err)
					}
					wire := f.take(f.senderOut)
					f.drop(wire)
					if err := <-noAck; err != nil {
						t.Fatalf("NoAck route write changed: %v", err)
					}
					if got := requirePayloadLedger(t, &ledger, 0, 0, 0); got.SendAck.AdmittedTotal != count {
						t.Fatal("unacknowledged write entered ACK-lifetime accounting")
					}
				}
				t.Logf("measured_senders=1 excluded_native_peers=1 encoded_ack_owners=%d blackout_backing_charges=%d recovered=%t final=%+v", count, heldBytes, recovery, final)
			})
		})
	}
}

func TestTransferPayloadOwnerDetachedHeadRewrite(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{1, 2} {
		for _, cancel := range []bool{false, true} {
			t.Run(fmt.Sprintf("v%d/cancel=%t", version, cancel), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					var ledger TransferPayloadOwnerLedger
					release := make(chan struct{})
					defer func() {
						select {
						case <-release:
						default:
							close(release)
						}
					}()
					logger := &ackRewriteBarrierLogger{Logger: NewNoopLogger(), reached: make(chan struct{}), release: release}
					f := payloadLedgerAckFixture(t, &ledger, logger, version)
					first, second := f.write(64), f.write(64)
					sequence := f.sequence()
					f.forward(f.receive(first), f.senderIn)
					secondAck := f.receive(second)
					<-logger.reached
					synctest.Wait()
					owners, heldBytes := payloadLedgerSendCharges(t, sequence)
					if owners != 1 || sequence.resendQueue.Len() != 0 {
						t.Fatal("worker did not hold a detached retry during head rewrite")
					}
					requirePayloadLedger(t, &ledger, 1, 0, heldBytes)
					if cancel {
						sequence.Cancel()
					} else {
						f.forward(secondAck, f.senderIn)
					}
					close(release)
					synctest.Wait()
					requirePayloadLedger(t, &ledger, 0, 0, 0)
					if !cancel && f.ackedCount != 2 {
						t.Fatal("timely native ACK lost its detached retry lifetime")
					}
				})
			})
		}
	}
}

// This directly drives the real encoder and explicit missing-contract recovery.
// A synthetic already-open contract makes its size-class change deterministic;
// it does not claim authenticated backend settlement or create a test peer.
func TestTransferPayloadOwnerContractHeadReplacement(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		var ledger TransferPayloadOwnerLedger
		client, _ := payloadLedgerResident(t, &ledger, nil)
		destination := NewId()
		settings := DefaultSendBufferSettingsWithBufferSize(4096)
		seq := NewSendSequence(client.ctx, client, client.sendBuffer, destination, MultiHopId{}, false, false, false, sequenceTlsRoleClient, false, settings)
		t.Cleanup(func() {
			seq.releaseRetainedSendItems(context.Canceled)
			seq.closeContractMultiRouteWriter()
			seq.Close()
		})
		contract := &sequenceContract{
			log: client.log, contractId: NewId(), transferByteCount: 100000,
			effectiveTransferByteCount: 100000, compactContractRecoverySupported: true,
			path:     NewTransferPath(client.ClientId(), destination, Id{}),
			contract: &protocol.Contract{StoredContractBytes: bytes.Repeat([]byte{0x5a}, 4096)},
		}
		seq.openSendContracts[contract.contractId] = contract
		seq.sendContract, seq.sendContractAcked = contract, true
		route := make(Route, 1)
		client.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(destination)), []Route{route})
		frame := budgetTestFrame(128)
		if !contract.update(MessageByteCount([]*protocol.Frame{frame})) {
			t.Fatal("synthetic open contract rejected the frame")
		}
		seq.sendRecord([]*protocol.Frame{frame}, sendAckRecord{}, noAckSendRecord{}, true, false)
		MessagePoolReturn(<-route)
		item := seq.resendQueue.PeekFirst()
		before := requirePayloadLedger(t, &ledger, 1, 0, int64(cap(item.transferFrameBytes)))
		if !seq.receiveContractMissing(item.messageId, contract.contractId) {
			t.Fatal("real missing-contract recovery did not replace its encoded frame")
		}
		after := requirePayloadLedger(t, &ledger, 1, 0, int64(cap(item.transferFrameBytes)))
		if after.SendAck.BackingByteCharges <= before.SendAck.BackingByteCharges || after.SendAck.AdmittedTotal != 1 {
			t.Fatalf("frame replacement did not resize the same tracked owner: before=%+v after=%+v", before, after)
		}
		seq.receiveAck(item.messageId, false, sequenceTag{}, true)
		requirePayloadLedger(t, &ledger, 0, 0, 0)
	})
}

func TestTransferPayloadOwnerForwardDebugCopyReplacement(t *testing.T) {
	assertMessagePoolOwnership(t)
	oldCopy := DebugTransferCopyOnWrite
	DebugTransferCopyOnWrite = true
	defer func() { DebugTransferCopyOnWrite = oldCopy }()
	synctest.Test(t, func(t *testing.T) {
		var ledger TransferPayloadOwnerLedger
		gate := make(chan struct{})
		client, stop := payloadLedgerResident(t, &ledger, gate)
		destination := NewId()
		pooled := payloadLedgerForwardWire(t, destination)
		wire := make([]byte, len(pooled), 16384)
		copy(wire, pooled)
		MessagePoolReturn(pooled)
		if !client.ForwardWithTimeout(wire, 0) {
			t.Fatal("debug-copy frame was not accepted")
		}
		requirePayloadLedger(t, &ledger, 0, 1, 16384)
		close(gate)
		synctest.Wait()
		// The original unpooled 16 KiB visible backing is replaced with the
		// actual pooled 2 KiB class plus its 12-byte metadata during Write.
		requirePayloadLedger(t, &ledger, 0, 1, 2048+MessagePoolMetaByteCount)
		stop()
		if err := client.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
		requirePayloadLedger(t, &ledger, 0, 0, 0)
	})
}
