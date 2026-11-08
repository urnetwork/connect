// Recovery memory is not contract authority. A real signed Required-encryption
// session carries a source-owned headed retry with a complete proof; the
// invalid control changes only that proof's MAC before sealing with the real
// source cipher. Both controls reach the same bounded recovery child.
package connect

import (
	"bytes"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// Preserve the real headed retry's identity, frames, path and session. The
// controlled replacement remains an actual authenticated encrypted carrier;
// only the independently verified contract proof distinguishes the controls.
func privateProviderRecoveryFullProof(t *testing.T, f *privateProviderEncryptedAckFixture, retry *privateProviderEncryptedAckFrame, changed bool) *privateProviderEncryptedAckFrame {
	t.Helper()
	if retry.bytes == nil || !retry.encrypted || !retry.pack.Head {
		t.Fatal("full-proof control has no owned actual encrypted headed retry")
	}
	contract := proto.Clone(f.sequence.receiveContract.contract).(*protocol.Contract)
	if len(contract.StoredContractHmac) == 0 || !f.receiver.ContractManager().Verify(
		contract.StoredContractHmac, contract.StoredContractBytes, contract.ProvideMode) {
		t.Fatal("full-proof control did not start with a genuinely verified signed contract")
	}
	if changed {
		contract.StoredContractHmac[0] ^= 1
	}
	if f.receiver.ContractManager().Verify(contract.StoredContractHmac, contract.StoredContractBytes, contract.ProvideMode) == changed {
		t.Fatal("full-proof control did not isolate exactly the changed contract MAC")
	}
	proof, err := proto.Marshal(contract)
	if err != nil {
		t.Fatal(err)
	}
	var outer protocol.TransferFrame
	if err := ProtoUnmarshal(retry.bytes, &outer); err != nil {
		t.Fatal(err)
	}
	path, err := TransferPathFromProtobuf(outer.TransferPath)
	if err != nil {
		t.Fatal(err)
	}
	plain, role, companion, err := f.receiver.unwrapFrame(path.SourceId, outer.GetSessionRole(), outer.SessionCompanion, outer.EncryptedTransferFrame)
	if err != nil {
		t.Fatal(err)
	}
	defer MessagePoolReturn(plain)
	var inner protocol.TransferFrame
	if err := ProtoUnmarshal(plain, &inner); err != nil || inner.Pack == nil {
		t.Fatal("full-proof control could not decode the actual authenticated source body")
	}
	inner.Pack.ContractId = nil
	inner.Pack.ContractFrame = &protocol.Frame{MessageType: protocol.MessageType_TransferContract, MessageBytes: proof}
	mutated, err := proto.Marshal(&inner)
	if err != nil {
		t.Fatal(err)
	}
	session := f.sender.EncryptionSessionManager().Lookup(f.receiver.ClientId(), role.complement(), companion)
	if session == nil || session.Cipher() == nil {
		t.Fatal("full-proof control lost the real negotiated source cipher")
	}
	sealed, err := session.Cipher().SealOuterFrame(path, mutated, outer.GetSessionRole(), companion)
	if err != nil {
		t.Fatal(err)
	}
	frame := f.capture(sealed, f.receiver)
	if !frame.encrypted || !frame.pack.Head || !frame.scan.contractFrameSet || !frame.scan.loanSafe ||
		frame.pack.ContractFrame == nil || len(frame.pack.ContractId) != 0 ||
		!bytes.Equal(frame.pack.MessageId, retry.pack.MessageId) || !bytes.Equal(frame.pack.SequenceId, retry.pack.SequenceId) ||
		frame.pack.SequenceNumber != retry.pack.SequenceNumber || !receiveProviderHeadRecoveryFrames(frame.pack.Frames) {
		t.Fatal("full-proof mutation changed source identity or provider recovery class")
	}
	return frame
}

// Ten committed real ACK tails saturate RX while one actual return chunk waits
// for the missing head. Both proof variants must allocate actual child custody;
// only a valid proof may reach NAT or advance cumulative delivery authority.
func testPrivateProviderEncryptedRecoveryProof(t *testing.T, changed bool) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		f := newPrivateProviderEncryptedAckFixture(t)
		f.gate = make(chan struct{})
		rootTotal, parentTotal := f.root.TotalByteCount(), f.parent.TotalByteCount()
		verified, first := f.sequence.receiveContract, f.nextNumber
		verifiedProof := verified.verifiedReceiveProof
		end := uint32(500 + 1280)
		f.tcp.mutex.Lock()
		f.tcp.receiveSeq, f.tcp.receiveWindowEnd = end, end+65535
		f.tcp.mutex.Unlock()
		if !f.tcp.retainReturnChunk(bytes.Repeat([]byte{0x6d}, 1280), 500, false) {
			t.Fatal("full-proof fixture could not retain the actual origin return chunk")
		}
		retained, natUsed := f.tcp.returnByteCount, f.natBudget.UsedByteCount()
		if retained <= 0 || len(f.tcp.returnChunks)-f.tcp.returnHead != 1 {
			t.Fatal("full-proof fixture has no live paid TCP return chunk")
		}
		head := f.writeAcks(2, end)
		tails := make([]*privateProviderEncryptedAckFrame, 10)
		for index := range tails {
			tails[index] = f.writeAcks(1, end)
		}
		cost := func(frame *privateProviderEncryptedAckFrame) ByteCount {
			probe := &receiveItem{transferItem: transferItem{queueByteCount: frame.scan.retainedByteCount}, ack: true, frames: frame.pack.Frames}
			if !f.sequence.prepareReceiveDeliveryCredit(probe) || !probe.deliveryPrepared {
				t.Fatal("full-proof fixture could not estimate actual downstream ownership")
			}
			return probe.QueueByteCount()
		}
		tailFull := cost(tails[0])
		repairMetadata, rxTarget := privateProviderRepairFixtureGeometry(t, f.sequence, tailFull, 10)
		f.rx.SetTotalByteCount(rxTarget)
		f.recovery.SetTotalByteCount(f.rx.TotalByteCount())
		for _, tail := range tails {
			f.forward(tail.windowRoundFrame, f.receiverIn)
			item := f.sequence.receiveQueue.GetBySequenceNumber(tail.pack.SequenceNumber)
			if item == nil || !item.committed || !item.deliveryPrepared || item.QueueByteCount() != tailFull {
				t.Fatal("full-proof fixture did not retain an actual committed ACK original")
			}
			f.feedback()
		}
		privateProviderAssertRepairFixtureCustody(t, f.sequence, repairMetadata, 10)
		if f.rx.UsedByteCount() != f.rx.TotalByteCount() || f.sequence.holdPromise.reservedByteCount != tailFull ||
			f.ackedCount != 1 || f.deliveredCount != 1 || f.tcp.returnByteCount != retained || len(f.entered) != 0 {
			t.Fatal("full-proof fixture did not isolate the missing head under actual RX pressure")
		}
		retry := f.retry(head)
		candidate := privateProviderRecoveryFullProof(t, f, retry, changed)
		full := cost(candidate)
		if full <= tailFull || full > f.recovery.Available() || candidate.scan.retainedByteCount <= 0 {
			t.Fatal("full-proof fixture did not require available bounded child growth")
		}
		before := f.recovery.Stats()
		f.forward(candidate.windowRoundFrame, f.receiverIn)
		after := f.recovery.Stats()
		if after.ReservedByteCount-before.ReservedByteCount != full ||
			f.root.TotalByteCount() != rootTotal || f.parent.TotalByteCount() != parentTotal ||
			verified.verifiedReceiveProof != verifiedProof {
			t.Fatalf("changed=%t: proof control did not reach the exact paid child before changing delivery authority: before=%+v after=%+v full=%d next=%d", changed, before, after, full, f.sequence.nextSequenceNumber)
		}
		if changed {
			f.feedback()
			if !f.sequence.rejectRetransmits || f.sequence.ctx.Err() == nil || len(f.entered) != 0 ||
				f.tcp.returnByteCount != retained || f.natBudget.UsedByteCount() != natUsed ||
				f.ackedCount != 1 || f.ackCounts[first] != 0 || f.deliveredCount != 1 ||
				f.sequence.nextSequenceNumber != first ||
				after.UsedByteCount != 0 || after.ReleasedByteCount-before.ReleasedByteCount != full ||
				f.sequence.holdPromise.ingressHead != nil {
				t.Fatal("changed signed proof used child memory as delivery authority or retained failed custody")
			}
		} else {
			if f.sequence.ctx.Err() != nil || len(f.entered) == 0 || f.recovery.UsedByteCount() <= 0 ||
				f.tcp.returnByteCount != retained || f.ackedCount != 1 || f.ackCounts[first] != 0 {
				t.Fatal("valid signed proof did not reach the held NAT boundary with paid child custody")
			}
			close(f.gate)
			synctest.Wait()
			f.feedback()
			if f.ackedCount != 12 || f.deliveredCount != 13 || f.ackCounts[first] != 1 ||
				f.sequence.nextSequenceNumber != first+11 || f.tcp.returnByteCount != 0 ||
				f.rx.UsedByteCount() != 0 || f.recovery.UsedByteCount() != 0 || f.sequence.receiveQueue.Len() != 0 {
				t.Fatal("valid signed full-proof child did not retire the actual return chunk and all cumulative callbacks")
			}
		}
		f.close()
		t.Logf("[provider-child-proof] changed_mac=%t encrypted=true actual_signed_contract=true head_c=%d child_d=%d retained_chunk_bytes=%d cumulative_head_ack=%d root=0 rx=0 child=0 nat=0 joined=true", changed, candidate.scan.retainedByteCount, full, retained, f.ackCounts[first])
	})
}

// The unchanged signed proof is the positive control for the actual encrypted
// child path, including retained origin-chunk retirement and final cleanup.
func TestPrivateProviderEncryptedRecoveryAcceptsVerifiedFullProof(t *testing.T) {
	assertMessagePoolOwnership(t)
	testPrivateProviderEncryptedRecoveryProof(t, false)
}

// A changed signed MAC reaches bounded child custody but is rejected by the
// ordinary contract verifier before NAT delivery, inner ACK or cumulative ACK.
func TestPrivateProviderEncryptedRecoveryRejectsChangedSignedProof(t *testing.T) {
	assertMessagePoolOwnership(t)
	testPrivateProviderEncryptedRecoveryProof(t, true)
}
