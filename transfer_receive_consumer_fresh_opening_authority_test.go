// Independent DATA storage never substitutes for the ordinary signed
// contract verifier. These real DATA-bearing retry heads require its rejection.
// The pre-verification Head index may equal the offered number; it must never
// advance beyond it, authorize application delivery or publish an ACK.
package connect

import (
	"bytes"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

func testFreshConsumerOpeningRejectedAuthority(t *testing.T, expired, wrongSource bool, protocolOnly ...bool) bool {
	t.Helper()
	GetMessagePoolAggregateStats()
	protocolMode := len(protocolOnly) != 0 && protocolOnly[0]
	exercised := false
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		const cost = ByteCount(5120)
		rootCapacity := 2 * cost
		if protocolMode {
			rootCapacity = cost
		}
		root := NewTransferMemoryBudget(rootCapacity)
		client := NewTransferMemoryBudgetWithParent(2*cost, root)
		rx := NewTransferMemoryBudgetWithParent(cost, client)
		head := NewTransferMemoryBudgetWithParent(cost, client)
		pack := NewTransferMemoryBudgetWithParent(cost, client)
		t.Cleanup(func() {
			for _, budget := range []*TransferMemoryBudget{pack, head, rx, client, root} {
				assertRetainedBudgetBalance(t, budget)
				if budget.Stats().QuotaByteCount != 0 {
					t.Error("fresh authority rejection retained logical permission")
				}
			}
		})
		var messageId Id
		var target *ReceiveSequence
		var targetCost ByteCount
		firstAttempt, paid := true, false
		observed := 0
		stage := func(sequence *ReceiveSequence, owner *decodedPackOwner, event string) {
			if event != "pack_released" || owner == nil || messageId == (Id{}) || !bytes.Equal(owner.pack.MessageId, messageId.Bytes()) {
				return
			}
			target = sequence
			if firstAttempt {
				observed++
				paid = sequence != nil && owner.inUse && owner.receivePack.receiveMemoryOwned &&
					owner.receiveItem.memoryBudget == head && owner.receiveItem.QueueByteCount() == targetCost &&
					owner.receivePack.ingressSequence == sequence && owner.receivePack.ingressLoanByteCount == 0 &&
					head.UsedByteCount() == targetCost && rx.UsedByteCount() == cost && root.UsedByteCount() == cost+targetCost
				if protocolMode {
					paid = sequence != nil && owner.inUse && !owner.receivePack.receiveMemoryOwned &&
						owner.receiveItem.memoryBudget == nil && owner.receiveItem.QueueByteCount() == 0 &&
						owner.receivePack.ingressSequence == nil && owner.receivePack.ingressLoanByteCount == 0 &&
						head.UsedByteCount() == 0 && rx.UsedByteCount() == cost && root.UsedByteCount() == cost
				}
			}
		}
		deadline := time.Now().Add(time.Hour)
		if expired {
			deadline = time.Now().Add(time.Second)
		}
		f, log, forward, reverse := newEstablishedCurrentExpiryFixture(t, rx, head, pack, deadline, stage)
		last := map[uint32]uint64{}
		foreignHead := consumerForeignHeadWrite(t, f, last, 1)
		foreignTail := consumerForeignHeadWrite(t, f, last, 1)
		foreign := consumerForeignHeadSequence(t, f, foreignHead)
		f.forward(foreignTail, f.receiverIn)
		expectedSpare := cost
		if protocolMode {
			expectedSpare = 0
		}
		heldId := RequireIdFromBytes(foreignTail.pack.MessageId)
		held := foreign.receiveQueue.GetByMessageId(heldId)
		if held == nil || held.committed || held.memoryBudget != rx || held.QueueByteCount() != cost ||
			foreign.holdPromise.set || rx.UsedByteCount() != cost || head.Available() != expectedSpare || root.Available() != expectedSpare {
			t.Fatal("authority control did not preserve entirely foreign full RX with independent child capacity")
		}
		var opening *windowRoundFrame
		if protocolMode {
			// Every DATA/root byte is occupied, but empty protocol bootstrap
			// must still reach the unchanged ordinary authority verifier.
			opening, _ = freshConsumerOpeningCapture(t, f)
		} else {
			_, data := freshConsumerDataOpeningCapture(t, f)
			// The paid-storage variant uses the real retained DATA original
			// and ordinary full-proof retry formatter, never a metadata flag.
			opening = freshConsumerOpeningFullDataHead(t, f, data)
		}
		id := RequireIdFromBytes(opening.pack.SequenceId)
		if freshConsumerOpeningSequence(f, id) != nil {
			t.Fatal("authority control already has a target generation")
		}
		var contract protocol.Contract
		if err := proto.Unmarshal(opening.pack.ContractFrame.MessageBytes, &contract); err != nil {
			t.Fatal(err)
		}
		var stored protocol.StoredContract
		if err := proto.Unmarshal(contract.StoredContractBytes, &stored); err != nil || stored.ExpirationTimeUnixMilli == nil ||
			*stored.ExpirationTimeUnixMilli != deadline.UnixMilli() {
			t.Fatal("real grant does not carry its actual signed deadline", err)
		}
		if !f.receiver.ContractManager().Verify(contract.StoredContractHmac, contract.StoredContractBytes, contract.ProvideMode) {
			t.Fatal("actual backend opening was not validly signed before the negative stimulus")
		}
		if wrongSource {
			// Keep the entire signed proof unchanged. Only the presented outer
			// route names another sender, so storage may be exercised but the real
			// registerContracts path check must reject before contract adoption.
			if RequireIdFromBytes(stored.SourceId) != f.sender.ClientId() || RequireIdFromBytes(stored.DestinationId) != f.receiver.ClientId() {
				t.Fatal("signed proof did not authenticate the original route")
			}
			var frame protocol.TransferFrame
			if err := proto.Unmarshal(opening.bytes, &frame); err != nil || frame.TransferPath == nil {
				t.Fatal("actual opening has no route to mutate", err)
			}
			frame.TransferPath.SourceId = NewId().Bytes()
			wire, err := ProtoMarshal(&frame)
			if err != nil {
				t.Fatal(err)
			}
			MessagePoolReturn(opening.bytes)
			opening.bytes = wire
			if !proto.Equal(frame.Pack, opening.pack) ||
				!f.receiver.ContractManager().Verify(contract.StoredContractHmac, contract.StoredContractBytes, contract.ProvideMode) {
				t.Fatal("source-only stimulus changed the actual signed proof or Pack")
			}
		} else if expired {
			forward.blockRenewals.Store(true)
			reverse.blockRenewals.Store(true)
			time.Sleep(time.Until(deadline) + time.Millisecond)
			synctest.Wait()
			if time.Now().UnixMilli() <= *stored.ExpirationTimeUnixMilli || foreign.ctx.Err() != nil {
				t.Fatal("signed deadline did not expire while foreign physical custody remained live")
			}
		} else {
			if len(contract.StoredContractHmac) == 0 {
				t.Fatal("real contract did not provide a MAC to corrupt")
			}
			contract.StoredContractHmac[0] ^= 0x80
			changed := proto.Clone(opening.pack).(*protocol.Pack)
			var err error
			changed.ContractFrame.MessageBytes, err = proto.Marshal(&contract)
			if err != nil {
				t.Fatal(err)
			}
			rewriteHeldHeadContractWire(t, opening, changed)
			if f.receiver.ContractManager().Verify(contract.StoredContractHmac, contract.StoredContractBytes, contract.ProvideMode) {
				t.Fatal("changed MAC was still accepted by the ordinary verifier")
			}
		}
		path, err := FilteredTransferPath(opening.bytes)
		if err != nil {
			t.Fatal(err)
		}
		if wrongSource && (path.SourceId == f.sender.ClientId() || path.DestinationId != f.receiver.ClientId()) {
			t.Fatal("source-only stimulus did not preserve destination and change exactly the presented source")
		}
		scan, status := scanReceiveMemoryPack(opening.bytes, path)
		shape := receiveConsumerOpeningHeadScan(scan)
		if !protocolMode {
			shape = scan.loanSafe && !scan.compatibility && !scan.legacy && scan.head && !scan.nack &&
				!scan.contractAhead && !scan.hasEncryptedControl && scan.contractFrameSet && !scan.contractIdSet &&
				scan.contractFrameType == int32(protocol.MessageType_TransferContract) && scan.sequenceNumber == 1 &&
				scan.frameCount == 1 && scan.consumerFrameCount == 1
		}
		if status != receiveMemoryPackScanReady || protocolOnlyPack(opening.pack) != protocolMode || !shape || scan.retainedByteCount <= 0 ||
			!protocolMode && (scan.retainedByteCount > head.Available() || scan.retainedByteCount > root.Available()) || freshConsumerOpeningSequence(f, id) != nil {
			t.Fatal("negative authority opening lost its exact syntax or independently fundable physical envelope")
		}
		messageId, targetCost = scan.messageId, scan.retainedByteCount
		if protocolMode {
			targetCost = 0
		}
		retry := freshConsumerOpeningClone(f, opening)
		reserved, released := head.Counts()
		f.forward(opening, f.receiverIn)
		firstAttempt = false
		exercised = observed == 1
		if exercised && (!paid || target == nil || target.ctx.Err() == nil || target.receiveContract != nil ||
			len(target.openReceiveContracts) != 0 || target.nextSequenceNumber != opening.pack.SequenceNumber || target.holdPromise.ingressHead != nil) {
			t.Fatal("physical child bypassed proof/expiry, retained authority or failed its ordinary worker refund")
		}
		if exercised && wrongSource && target.source != path.SourceMask() {
			t.Fatal("wrong-source proof did not reach the actual presented source's worker")
		}
		afterReserved, afterReleased := head.Counts()
		if exercised && (afterReserved-reserved != targetCost || afterReleased-released != targetCost) ||
			!exercised && (observed != 0 || afterReserved != reserved || afterReleased != released) ||
			f.deliveredCount != 0 || f.ackedCount != 0 || len(f.receiverOut) != 0 || head.UsedByteCount() != 0 ||
			foreign.receiveQueue.GetByMessageId(heldId) != held || held.committed || foreign.holdPromise.set || rx.UsedByteCount() != cost {
			t.Fatal("authority rejection changed foreign custody, delivered/debited/ACKed the opening or leaked its C")
		}
		// Old source cannot exercise the paid child while RX is full. It still
		// has to prove that this same wire reaches the real verifier once the
		// actual foreign owner closes, before reporting the missed new path.
		foreign.Cancel()
		<-foreign.done
		synctest.Wait()
		if !exercised {
			f.forward(retry, f.receiverIn)
		}
		if target == nil || target.ctx.Err() == nil || target.receiveContract != nil || len(target.openReceiveContracts) != 0 ||
			target.nextSequenceNumber != opening.pack.SequenceNumber || f.deliveredCount != 0 || f.ackedCount != 0 || len(f.receiverOut) != 0 ||
			rx.UsedByteCount() != 0 || head.UsedByteCount() != 0 || root.UsedByteCount() != 0 || pack.Stats().QuotaByteCount != 0 {
			t.Fatal("same invalid/expired signed opening escaped ordinary refusal or exact final ownership cleanup")
		}
		if target.rejectRetransmits == expired {
			t.Fatal("ordinary bad-MAC trust tombstone or ordinary-expiry lifetime distinction changed")
		}
		<-target.done
		synctest.Wait()
		key := receiveSequenceHeadKey{Source: target.source, EncryptionRole: target.encryptionRole,
			EncryptionCompanion: target.encryptionCompanion, ForceStream: target.transferKey.ForceStream,
			CompanionContract: target.transferKey.CompanionContract, LogicalLane: target.transferKey.LogicalLane}
		buffer := f.receiver.receiveBuffer
		buffer.mutex.Lock()
		rejected, exists := buffer.rejectedReceiveSequenceIds[key]
		buffer.mutex.Unlock()
		if !expired && (!exists || rejected != target.sequenceId) || expired && exists {
			t.Fatal("ordinary worker did not publish exactly the appropriate trust tombstone after its real join")
		}
		verification, register, exit := 0, 0, 0
		for _, message := range log.messages() {
			cause := "Contract verification failed.\n"
			if expired {
				cause = errContractExpired.Error() + "\n"
			} else if wrongSource {
				cause = "Contract path does not match receive path.\n"
			}
			if !expired && !wrongSource && strings.Contains(message, " exit contract verification failed (") {
				verification++
			} else if strings.Contains(message, " could not register contracts = ") && strings.HasSuffix(message, cause) {
				register++
			} else if strings.Contains(message, " exit could not receive ack = ") && strings.HasSuffix(message, cause) {
				exit++
			} else {
				t.Error("fresh authority negative observed an unrelated worker error")
			}
		}
		badMAC := !expired && !wrongSource
		if register != 1 || exit != 1 || badMAC && verification != 1 || !badMAC && verification != 0 {
			t.Fatalf("ordinary authority refusal evidence changed: expired=%t wrong_source=%t verifier=%d register=%d exit=%d", expired, wrongSource, verification, register, exit)
		}
		t.Logf("FRESH_OPENING_AUTHORITY_BEHAVIOR_DRAINED expired=%t wrong_source=%t child_funded=%t protocol_only=%t no_app_no_ack_no_debit=true", expired, wrongSource, !protocolMode && exercised, protocolMode)
	})
	return exercised
}

func TestReceiveConsumerFreshOpeningPaidStorageDoesNotVerifyBadMac(t *testing.T) {
	exercised := testFreshConsumerOpeningRejectedAuthority(t, false, false)
	if t.Failed() {
		return
	}
	t.Log("FRESH_OPENING_BAD_MAC_CLEANUP_JOINED")
	if !exercised {
		t.Fatal("FRESH_OPENING_BAD_MAC_CHILD_PATH_UNEXERCISED: same wire was rejected only after ordinary RX capacity returned")
	}
}

func TestReceiveConsumerFreshOpeningPaidStorageDoesNotExtendSignedExpiry(t *testing.T) {
	exercised := testFreshConsumerOpeningRejectedAuthority(t, true, false)
	if t.Failed() {
		return
	}
	t.Log("FRESH_OPENING_EXPIRED_CLEANUP_JOINED")
	if !exercised {
		t.Fatal("FRESH_OPENING_EXPIRY_CHILD_PATH_UNEXERCISED: same signed-expired wire was rejected after ordinary capacity returned")
	}
}

func TestReceiveConsumerFreshOpeningPaidStorageDoesNotAuthorizeWrongSignedSource(t *testing.T) {
	exercised := testFreshConsumerOpeningRejectedAuthority(t, false, true)
	if t.Failed() {
		return
	}
	t.Log("FRESH_OPENING_WRONG_SIGNED_SOURCE_CLEANUP_JOINED")
	if !exercised {
		t.Fatal("FRESH_OPENING_WRONG_SOURCE_CHILD_PATH_UNEXERCISED: signed path rejection happened only after ordinary capacity returned")
	}
}

// These protocol-only cases occupy the ENTIRE DATA root with a foreign owner.
// No DATA allocation is permitted, while the ordinary verifier still rejects.
func TestReceiveConsumerProtocolOpeningDoesNotVerifyBadMac(t *testing.T) {
	if exercised := testFreshConsumerOpeningRejectedAuthority(t, false, false, true); !t.Failed() && !exercised {
		t.Fatal("PROTOCOL_BAD_MAC_AUTHORITY_PATH_UNEXERCISED")
	}
}

func TestReceiveConsumerProtocolOpeningDoesNotExtendSignedExpiry(t *testing.T) {
	if exercised := testFreshConsumerOpeningRejectedAuthority(t, true, false, true); !t.Failed() && !exercised {
		t.Fatal("PROTOCOL_EXPIRY_AUTHORITY_PATH_UNEXERCISED")
	}
}

func TestReceiveConsumerProtocolOpeningDoesNotAuthorizeWrongSignedSource(t *testing.T) {
	if exercised := testFreshConsumerOpeningRejectedAuthority(t, false, true, true); !t.Failed() && !exercised {
		t.Fatal("PROTOCOL_WRONG_SOURCE_AUTHORITY_PATH_UNEXERCISED")
	}
}
