// Protocol exemption is not contract authority. A real signed Required
// session carries a source-owned headed retry with a complete proof; the
// invalid control changes only its MAC before sealing with the real cipher.
// Separate actual DATA custody fills RX, with no protocol byte reservation.
package connect

import (
	"bytes"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/v2026/protocol"
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

// The proof-bearing owner must authenticate before actual NAT dispatch. A
// real accepted successor stays unencoded behind its occupied count-one slot;
// only a valid head ACK permits the remaining ten actual ACK originals.
func testPrivateProviderEncryptedRecoveryProof(t *testing.T, changed bool) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		f := newPrivateProviderEncryptedAckFixture(t)
		f.gate = make(chan struct{})
		rootTotal, parentTotal := f.root.TotalByteCount(), f.parent.TotalByteCount()
		verified := f.sequence.receiveContract
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
		first := head.pack.SequenceNumber
		retry := f.retry(head)
		candidate := privateProviderRecoveryFullProof(t, f, retry, changed)
		if !candidate.pack.DeliverImmediately || !protocolOnlyPack(candidate.pack) {
			t.Fatal("full-proof control changed the actual pure ACK immediate policy")
		}
		pressure := newPrivateProviderProtocolDataPressure(t, f.rx)
		queued := f.enqueueAcks(1, end)
		f.assertQueuedAckOriginal(head.pack, queued)
		before := f.recovery.Stats()
		f.forward(candidate.windowRoundFrame, f.receiverIn)
		pressure.check(t)
		privateProviderAssertImmediateCount(t, f.windowRoundFixture, head.pack, 1)
		if f.recovery.Stats() != before || f.root.TotalByteCount() != rootTotal || f.parent.TotalByteCount() != parentTotal ||
			verified.verifiedReceiveProof != verifiedProof || f.ackedCount != 1 || f.ackCounts[first] != 0 || queued.callbacks != 0 {
			t.Fatal("protocol proof changed byte/count permission, prior verification or actual ACK authority")
		}
		privateProviderAssertOriginalPending(t, f.windowRoundFixture, head.pack, true, true)
		if changed {
			f.feedback()
			if !f.sequence.rejectRetransmits || f.sequence.ctx.Err() == nil || len(f.entered) != 0 ||
				f.tcp.returnByteCount != retained || f.natBudget.UsedByteCount() != natUsed ||
				f.ackedCount != 1 || f.ackCounts[first] != 0 || queued.callbacks != 0 || f.deliveredCount != 1 ||
				f.sequence.nextSequenceNumber != first || f.sequence.holdPromise.ingressHead != nil {
				t.Fatal("changed signed proof used protocol exemption as authority or falsely completed its queued successor")
			}
		} else {
			if f.sequence.ctx.Err() != nil || len(f.entered) == 0 || f.tcp.returnByteCount != retained {
				t.Fatal("valid signed proof did not reach the held actual NAT owner under full DATA pressure")
			}
			privateProviderAssertProtocolReceiptCustody(t, f.sequence, 1)
			f.assertQueuedAckOriginal(head.pack, queued)
			close(f.gate)
			synctest.Wait()
			privateProviderAssertReturnFrontier(t, f.tcp, end)
			privateProviderAssertOriginalPending(t, f.windowRoundFixture, head.pack, true, true)
			f.feedback()
			if f.ackCounts[first] != 1 || f.ackedCount != 2 {
				t.Fatal("the source serialized its successor without the actual valid-proof head ACK")
			}
			privateProviderAssertOriginalRetired(t, f.windowRoundFixture, head.pack)
			originals := []*privateProviderEncryptedAckFrame{head}
			for index := range 10 {
				var tail *privateProviderEncryptedAckFrame
				if index == 0 {
					tail = f.takeAckOriginal(queued)
				} else {
					tail = f.writeAcks(1, end)
				}
				originals = append(originals, tail)
				privateProviderAssertOriginalPending(t, f.windowRoundFixture, tail.pack, true, true)
				privateProviderAssertImmediateCount(t, f.windowRoundFixture, tail.pack, 1)
				f.forward(tail.windowRoundFrame, f.receiverIn)
				privateProviderAssertReturnFrontier(t, f.tcp, end)
				privateProviderAssertOriginalPending(t, f.windowRoundFixture, tail.pack, true, true)
				f.feedback()
				if f.ackCounts[tail.pack.SequenceNumber] != 1 || f.ackedCount != 3+index {
					t.Fatal("full-proof ACK train recycled count one without exactly one actual terminal ACK")
				}
				privateProviderAssertOriginalRetired(t, f.windowRoundFixture, tail.pack)
				privateProviderAssertImmediateCount(t, f.windowRoundFixture, tail.pack, 0)
			}
			pressure.check(t)
			f.assertCompletedOriginals(originals)
			if f.ackedCount != 12 || f.deliveredCount != 13 || f.ackCounts[first] != 1 ||
				f.recovery.Stats() != before || f.sequence.receiveQueue.Len() != 0 {
				t.Fatal("valid signed proof did not retire actual return custody and all eleven real reliable originals")
			}
			for _, original := range originals {
				if f.ackCounts[original.pack.SequenceNumber] != 1 {
					t.Fatal("full-proof delivery skipped or repeated an original's actual terminal callback")
				}
			}
		}
		pressure.close()
		f.close()
		t.Logf("[provider-protocol-proof] changed_mac=%t encrypted=true actual_signed_contract=true immediate_limit=1 encoded_head=1 queued_successor=1 protocol_data_bytes=0 retained_chunk_bytes=%d cumulative_head_ack=%d root=0 rx=0 child=0 nat=0 joined=true", changed, retained, f.ackCounts[first])
	})
}

// The unchanged signed proof authorizes actual encrypted protocol delivery,
// retained origin-chunk retirement and exact final ownership cleanup.
func TestPrivateProviderEncryptedRecoveryAcceptsVerifiedFullProof(t *testing.T) {
	assertMessagePoolOwnership(t)
	testPrivateProviderEncryptedRecoveryProof(t, false)
}

// A changed signed MAC remains uncharged protocol but is rejected by the
// ordinary verifier before NAT delivery, inner ACK or a real Transfer ACK.
func TestPrivateProviderEncryptedRecoveryRejectsChangedSignedProof(t *testing.T) {
	assertMessagePoolOwnership(t)
	testPrivateProviderEncryptedRecoveryProof(t, true)
}
