// Real full-proof quiet Heads may buy storage without changing the existing
// verifier's MAC, signed deadline, debit, ACK or generation trust decisions.
package connect

import (
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
	"google.golang.org/protobuf/proto"
)

func testConsumerEstablishedFullHeadAuthority(t *testing.T, expired bool) bool {
	t.Helper()
	fault := "bad-mac"
	if expired {
		fault = "expired"
	}
	return testConsumerEstablishedProofAuthority(t, fault, false)
}

// Negative wires retain the public sender's identity, payload and signed
// contract. The non-Head arm changes only the shape before the selected fault;
// the public successor-switch regression separately proves that shape's origin.
func testConsumerEstablishedProofAuthority(t *testing.T, fault string, nonHead bool) bool {
	t.Helper()
	expired, wrongSource := fault == "expired", fault == "source"
	if fault != "bad-mac" && !expired && !wrongSource {
		t.Fatal("unknown signed proof authority fault")
	}
	GetMessagePoolAggregateStats()
	funded := false
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		const foreignBytes = ByteCount(5120)
		root := NewTransferMemoryBudget(2 * foreignBytes)
		client := NewTransferMemoryBudgetWithParent(2*foreignBytes, root)
		rx := NewTransferMemoryBudgetWithParent(foreignBytes, client)
		recovery := NewTransferMemoryBudgetWithParent(foreignBytes, client)
		packBudget := NewTransferMemoryBudgetWithParent(foreignBytes, client)
		t.Cleanup(func() {
			for _, budget := range []*TransferMemoryBudget{packBudget, recovery, rx, client, root} {
				assertRetainedBudgetBalance(t, budget)
				if budget.Stats().QuotaByteCount != 0 {
					t.Error("established full-Head authority control retained quota after join")
				}
			}
		})
		var target *ReceiveSequence
		var targetId Id
		var targetCost ByteCount
		firstAttempt, paid := true, false
		observed := 0
		stage := func(sequence *ReceiveSequence, owner *decodedPackOwner, event string) {
			if !firstAttempt || event != "pack_released" || owner == nil || targetId == (Id{}) || owner.receiveItem.messageId != targetId {
				return
			}
			observed++
			paid = sequence == target && owner.inUse && owner.receivePack.receiveMemoryOwned &&
				owner.receiveItem.memoryBudget == recovery && owner.receiveItem.QueueByteCount() == targetCost &&
				owner.receivePack.ingressSequence == target && owner.receivePack.ingressLoanByteCount == 0 &&
				recovery.UsedByteCount() == targetCost && rx.UsedByteCount() == foreignBytes &&
				root.UsedByteCount() == foreignBytes+targetCost && client.UsedByteCount() == root.UsedByteCount() &&
				packBudget.UsedByteCount() == 0 && packBudget.Stats().QuotaByteCount == 0
		}
		deadline := time.Now().Add(time.Hour)
		if expired {
			deadline = time.Now().Add(time.Second)
		}
		fixture, log, forward, reverse := newConsumerEstablishedWireHeadFixture(t, false, rx, recovery, packBudget, deadline, stage)
		last := map[uint32]uint64{}
		warm := consumerEstablishedWireHeadWrite(t, fixture, last)
		fixture.forward(warm, fixture.receiverIn)
		fixture.acknowledge()
		target = consumerForeignHeadSequence(t, fixture, warm)
		contract := target.receiveContract
		sender := consumerEstablishedWireHeadSender(t, fixture, warm)
		if contract == nil || !contract.verifiedReceiveProofSet || contract.expired() || !target.holdPromise.establishedContract ||
			len(sender.sendItems) != 0 || !sender.sendContractAcked || target.holdPromise.ingressHead != nil ||
			target.receiveQueue.Len() != 0 || fixture.deliveredCount != 1 || fixture.ackedCount != 1 || root.UsedByteCount() != 0 {
			t.Fatal("authority control did not first verify and drain an actual signed generation")
		}
		foreignHead := consumerForeignHeadWrite(t, fixture, last, 1)
		foreignTail := consumerForeignHeadWrite(t, fixture, last, 1)
		foreign := consumerForeignHeadSequence(t, fixture, foreignHead)
		fixture.forward(foreignTail, fixture.receiverIn)
		foreignId := RequireIdFromBytes(foreignTail.pack.MessageId)
		foreignOwner := foreign.receiveQueue.GetByMessageId(foreignId)
		if foreignOwner == nil || foreignOwner.committed || foreignOwner.memoryBudget != rx || foreignOwner.QueueByteCount() != foreignBytes ||
			foreign.holdPromise.set || rx.UsedByteCount() != foreignBytes || recovery.Available() != foreignBytes || root.Available() != foreignBytes {
			t.Fatal("authority control did not fill RX exclusively with live foreign custody")
		}
		head := consumerEstablishedWireHeadWrite(t, fixture, last)
		if !head.pack.Head || head.pack.Nack || head.pack.ContractAhead || head.pack.ContractId != nil ||
			head.pack.ContractFrame == nil || head.pack.ContractFrame.MessageType != protocol.MessageType_TransferContract ||
			len(head.pack.Frames) != 1 || head.pack.Frames[0].MessageType != protocol.MessageType_IpIpPacketFromProvider ||
			head.pack.SequenceNumber != target.nextSequenceNumber || target.holdPromise.ingressFloor != target.nextSequenceNumber {
			t.Fatal("authority control did not capture the real quiet full-proof current application Head")
		}
		var proof protocol.Contract
		if err := proto.Unmarshal(head.pack.ContractFrame.MessageBytes, &proof); err != nil ||
			!fixture.receiver.ContractManager().Verify(proof.StoredContractHmac, proof.StoredContractBytes, proof.ProvideMode) {
			t.Fatal("captured producer full proof did not have its actual valid signature", err)
		}
		var stored protocol.StoredContract
		if err := proto.Unmarshal(proof.StoredContractBytes, &stored); err != nil || stored.ExpirationTimeUnixMilli == nil ||
			*stored.ExpirationTimeUnixMilli != deadline.UnixMilli() {
			t.Fatal("captured full proof did not carry the actual signed deadline", err)
		}
		if nonHead {
			head.pack.Head = false
			rewriteHeldHeadContractWire(t, head, head.pack)
		}
		if expired {
			forward.blockRenewals.Store(true)
			reverse.blockRenewals.Store(true)
			time.Sleep(time.Until(deadline) + time.Millisecond)
			synctest.Wait()
			if !contract.expired() || target.ctx.Err() != nil || foreign.ctx.Err() != nil {
				t.Fatal("signed expiry did not preserve live historical generation and foreign custody")
			}
		} else {
			if wrongSource {
				stored.SourceId = NewId().Bytes()
				var err error
				proof.StoredContractBytes, err = proto.Marshal(&stored)
				if err != nil {
					t.Fatal(err)
				}
				secret, ok := fixture.receiver.ContractManager().GetProvideSecretKey(proof.ProvideMode)
				if !ok {
					t.Fatal("signed source negative lost its authority secret")
				}
				proof.StoredContractHmac = SignStoredContract(fixture.receiver.ContractManager().settings, secret, proof.StoredContractBytes)
			} else {
				if len(proof.StoredContractHmac) == 0 {
					t.Fatal("captured full proof has no MAC to corrupt")
				}
				proof.StoredContractHmac[0] ^= 0x80
			}
			changed := proto.Clone(head.pack).(*protocol.Pack)
			var err error
			changed.ContractFrame.MessageBytes, err = proto.Marshal(&proof)
			if err != nil {
				t.Fatal(err)
			}
			rewriteHeldHeadContractWire(t, head, changed)
			if fixture.receiver.ContractManager().Verify(proof.StoredContractHmac, proof.StoredContractBytes, proof.ProvideMode) != wrongSource {
				t.Fatal("signed source or corrupted MAC did not isolate its intended verifier boundary")
			}
		}
		path, err := FilteredTransferPath(head.bytes)
		if err != nil {
			t.Fatal(err)
		}
		scan, status := scanReceiveMemoryPack(head.bytes, path)
		if status != receiveMemoryPackScanReady || !scan.loanSafe || scan.head == nonHead || scan.consumerFrameCount != 1 || scan.frameCount != 1 ||
			scan.retainedByteCount <= 0 || scan.retainedByteCount > recovery.Available() || scan.retainedByteCount > root.Available() ||
			target.receiveContract != contract || target.holdPromise.ingressHead != nil || target.holdPromise.set {
			t.Fatal("negative actual full Head lost independently fundable current physical shape")
		}
		targetId, targetCost = scan.messageId, scan.retainedByteCount
		beforeDebit := contract.ackedByteCount + contract.unackedByteCount
		beforeForeign, beforeRepair, beforeRx := foreign.holdPromise, foreign.heldHeadRepair, rx.Stats()
		reserved, released := recovery.Counts()
		MessagePoolShareReadOnly(head.bytes)
		retry := &windowRoundFrame{bytes: head.bytes, pack: head.pack}
		fixture.heldFrames = append(fixture.heldFrames, retry)
		fixture.forward(head, fixture.receiverIn)
		firstAttempt = false
		funded = observed == 1
		if funded && (!paid || target.ctx.Err() == nil || target.nextSequenceNumber != scan.sequenceNumber ||
			target.holdPromise.ingressHead != nil || !target.holdPromise.closed) ||
			!funded && (observed != 0 || paid || target.ctx.Err() != nil) {
			t.Fatal("actual negative full Head failed the independently funded ordinary worker boundary")
		}
		afterReserved, afterReleased := recovery.Counts()
		charge := targetCost
		if !funded {
			charge = 0
		}
		if afterReserved-reserved != charge || afterReleased-released != charge || recovery.UsedByteCount() != 0 ||
			fixture.deliveredCount != 1 || fixture.ackedCount != 1 || len(fixture.receiverOut) != 0 ||
			contract.ackedByteCount+contract.unackedByteCount != beforeDebit || rx.Stats() != beforeRx ||
			foreign.receiveQueue.GetByMessageId(foreignId) != foreignOwner || foreignOwner.committed || foreignOwner.memoryBudget != rx ||
			foreign.holdPromise != beforeForeign || foreign.heldHeadRepair != beforeRepair || foreign.ctx.Err() != nil || root.UsedByteCount() != foreignBytes {
			t.Fatal("negative full Head delivered/ACKed/debited, stole foreign custody or leaked C")
		}
		// On the current source, prove the identical invalid/expired wire
		// reaches the ordinary verifier once actual foreign custody cancels.
		foreign.Cancel()
		<-foreign.done
		synctest.Wait()
		if !funded {
			fixture.forward(retry, fixture.receiverIn)
		}
		if target.ctx.Err() == nil {
			t.Fatal("actual negative full Head did not close through the ordinary worker")
		}
		<-target.done
		synctest.Wait()
		if target.nextSequenceNumber != scan.sequenceNumber || target.holdPromise.ingressHead != nil ||
			fixture.deliveredCount != 1 || fixture.ackedCount != 1 || len(fixture.receiverOut) != 0 ||
			contract.ackedByteCount+contract.unackedByteCount != beforeDebit || target.rejectRetransmits == expired ||
			root.UsedByteCount() != 0 || client.UsedByteCount() != 0 || rx.UsedByteCount() != 0 || recovery.UsedByteCount() != 0 ||
			packBudget.UsedByteCount() != 0 || packBudget.Stats().QuotaByteCount != 0 {
			t.Fatal("actual full-Head MAC/expiry refusal changed delivery/debit/trust or failed final refund/join")
		}
		key := receiveSequenceHeadKey{Source: target.source, EncryptionRole: target.encryptionRole,
			EncryptionCompanion: target.encryptionCompanion, ForceStream: target.transferKey.ForceStream,
			CompanionContract: target.transferKey.CompanionContract, LogicalLane: target.transferKey.LogicalLane}
		buffer := fixture.receiver.receiveBuffer
		buffer.mutex.Lock()
		rejected, exists := buffer.rejectedReceiveSequenceIds[key]
		buffer.mutex.Unlock()
		if !expired && (!exists || rejected != target.sequenceId) || expired && exists {
			t.Fatal("actual full Head changed the bad-MAC trust tombstone or ordinary-expiry distinction")
		}
		verification, register, exit := 0, 0, 0
		cause := "Contract verification failed.\n"
		if expired {
			cause = errContractExpired.Error() + "\n"
		} else if wrongSource {
			cause = "Contract path does not match receive path.\n"
		}
		for _, message := range log.messages() {
			if !expired && strings.Contains(message, " exit contract verification failed (") {
				verification++
			} else if strings.Contains(message, " could not register contracts = ") && strings.HasSuffix(message, cause) {
				register++
			} else if strings.Contains(message, " exit could not receive ack = ") && strings.HasSuffix(message, cause) {
				exit++
			} else {
				t.Error("full-Head authority negative observed an unrelated worker error")
			}
		}
		wantVerification := 0
		if !expired && !wrongSource {
			wantVerification = 1
		}
		if register != 1 || exit != 1 || verification != wantVerification {
			t.Fatalf("full-Head ordinary refusal logs changed: expired=%t verification=%d register=%d exit=%d", expired, verification, register, exit)
		}
		t.Logf("ESTABLISHED_FULL_HEAD_AUTHORITY_BEHAVIOR_DRAINED fault=%s non_head=%t child_funded=%t C=%d no_delivery_ack_debit=true", fault, nonHead, funded, targetCost)
	})
	return funded
}

func TestReceiveConsumerEstablishedFullHeadPaidStorageDoesNotVerifyBadMac(t *testing.T) {
	funded := testConsumerEstablishedFullHeadAuthority(t, false)
	if t.Failed() {
		return
	}
	t.Log("ESTABLISHED_FULL_HEAD_BAD_MAC_CLEANUP_JOINED")
	if !funded {
		t.Fatal("ESTABLISHED_FULL_HEAD_BAD_MAC_CHILD_PATH_UNEXERCISED: same invalid proof reached verifier after foreign cancellation")
	}
}

func TestReceiveConsumerEstablishedFullHeadPaidStorageDoesNotExtendSignedExpiry(t *testing.T) {
	funded := testConsumerEstablishedFullHeadAuthority(t, true)
	if t.Failed() {
		return
	}
	t.Log("ESTABLISHED_FULL_HEAD_EXPIRY_CLEANUP_JOINED")
	if !funded {
		t.Fatal("ESTABLISHED_FULL_HEAD_EXPIRY_CHILD_PATH_UNEXERCISED: same expired proof reached verifier after foreign cancellation")
	}
}
