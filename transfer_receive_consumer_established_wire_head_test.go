// Actual quiet and timeout-promoted consumer Heads must fund their own physical
// decode while foreign traffic owns RX. The worker retains proof/ACK authority.
package connect

import (
	"bytes"
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// Compact policy is immutable before either Client is constructed. Otherwise
// this is the maintained signed-expiry fixture's real OOB, routes and cleanup.
func newConsumerEstablishedWireHeadFixture(t *testing.T, compact bool, rx, recovery, pack *TransferMemoryBudget, deadline time.Time, ownerStage func(*ReceiveSequence, *decodedPackOwner, string)) (*windowRoundFixture, *receiveGapTestLogger, *expirationLifecycleAuthority, *expirationLifecycleAuthority) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	log := &receiveGapTestLogger{Logger: NewNoopLogger()}
	settings := func(receiver bool) *ClientSettings {
		value := DefaultClientSettingsWithBufferSize(64)
		value.Log = log
		value.EncryptionSettings.Mode = EncryptionModeOff
		value.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		value.ContractManagerSettings.NetworkEventTimeEnableContracts = time.Unix(1, 0)
		value.ContractManagerSettings.NetworkEventTimeChangeHmac = time.Unix(1, 0)
		value.ContractManagerSettings.LegacyCreateContract = false
		value.SendBufferSettings.WindowSizing = WindowSizingConstant
		value.SendBufferSettings.ApplyWindowSizing()
		value.SendBufferSettings.SequenceBufferSize = 0
		value.SendBufferSettings.ProtocolVersion = 2
		value.SendBufferSettings.CompactContractHead = compact
		value.ReceiveBufferSettings.WindowSizing = WindowSizingConstant
		value.ReceiveBufferSettings.ApplyWindowSizing()
		value.ReceiveBufferSettings.AckCompressTimeout = 0
		value.ReceiveBufferSettings.ReceiveQueueBudget = nil
		if receiver {
			receive := value.ReceiveBufferSettings
			receive.ReceiveQueueBudget, receive.ReceiveHeadRecoveryBudget = rx, recovery
			receive.ReceiveQueueRetainedByteAccounting = true
			receive.ReceiveQueueMinByteCount = 0
			receive.ReceiveQueueMaxByteCount = kib(64)
			receive.ReceiveHoldPolicy = ReceiveHoldCommittedPrefix
			receive.AdvertiseReceiveWindow = false
			receive.PackQueueBudget = pack
			receive.PackQueueRetainedByteAccounting = false
			receive.receiveOwnerStageForTest = ownerStage
			receive.ReliablePackHandoffTimeout = 0
		}
		return value
	}
	fixture := &windowRoundFixture{t: t, ctx: ctx, cancel: cancel,
		senderOut: make(Route, 128), senderIn: make(Route, 128), receiverOut: make(Route, 128), receiverIn: make(Route, 128)}
	senderId, receiverId := NewId(), NewId()
	forward := &expirationLifecycleAuthority{ctx: ctx, sourceId: senderId,
		peer: func() *Client { return fixture.receiver }, initialDeadline: deadline,
		grantDeadlines: map[Id]time.Time{}, renewalEntered: make(chan struct{})}
	reverse := &expirationLifecycleAuthority{ctx: ctx, sourceId: receiverId,
		peer: func() *Client { return fixture.sender }, initialDeadline: deadline,
		grantDeadlines: map[Id]time.Time{}, renewalEntered: make(chan struct{})}
	fixture.sender = NewClient(ctx, senderId, forward, settings(false))
	fixture.receiver = NewClient(ctx, receiverId, reverse, settings(true))
	fixture.sender.RouteManager().UpdateTransport(newDataGatewayTransport(), []Route{fixture.senderOut})
	fixture.sender.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{fixture.senderIn})
	fixture.receiver.RouteManager().UpdateTransport(newDataGatewayTransport(), []Route{fixture.receiverOut})
	fixture.receiver.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{fixture.receiverIn})
	for _, client := range []*Client{fixture.sender, fixture.receiver} {
		client.ContractManager().SetProvideModes(map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true})
	}
	fixture.receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
		for _, frame := range frames {
			if frame.MessageType == protocol.MessageType_IpIpPacketFromProvider {
				fixture.deliveredCount++
			}
		}
	})
	t.Cleanup(func() {
		cancel()
		owner, release := context.WithTimeout(context.Background(), 5*time.Second)
		defer release()
		for _, client := range []*Client{fixture.sender, fixture.receiver} {
			if err := client.CloseAndWait(owner); err != nil {
				t.Errorf("close signed final handoff consumer client: %v", err)
			}
		}
		for _, frame := range fixture.heldFrames {
			if frame.bytes != nil {
				MessagePoolReturn(frame.bytes)
				frame.bytes = nil
			}
		}
		for _, route := range []Route{fixture.senderOut, fixture.senderIn, fixture.receiverOut, fixture.receiverIn} {
			for len(route) != 0 {
				MessagePoolReturn(<-route)
			}
		}
	})
	return fixture, log, forward, reverse
}

// Every opening completes through the real receiver/ACK path. Application
// bytes remain in the captured, publicly emitted frame until explicitly sent.
func consumerEstablishedWireHeadWrite(t *testing.T, fixture *windowRoundFixture, last map[uint32]uint64) *windowRoundFrame {
	t.Helper()
	packet := ipOosTcpPacketSequence(reliableDeliveryTestPath(4), tcpFlagAck,
		uint32(101+last[0]), bytes.Repeat([]byte{0x6b}, 32))
	frame := &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketFromProvider, Raw: true, MessageBytes: packet}
	accepted, err := fixture.sender.SendWithTimeoutDetailed(frame, fixture.receiver.ClientId(), func(err error) {
		if err == nil {
			fixture.ackedCount++
		}
	}, time.Second, TransferKey{LogicalLane: 0})
	if !accepted || err != nil {
		MessagePoolReturn(frame.MessageBytes)
		t.Fatal("established Head public Send refused its source", err)
	}
	for range cap(fixture.senderOut) {
		wire := fixture.take(fixture.senderOut)
		if wire.pack == nil {
			t.Fatal("established Head writer published a non-Pack")
		}
		if wire.pack.LogicalLane != 0 {
			continue
		}
		for _, payload := range wire.pack.Frames {
			if payload.MessageType == protocol.MessageType_IpIpPacketFromProvider {
				if previous, seen := last[0]; seen && wire.pack.SequenceNumber <= previous {
					break
				}
				last[0] = wire.pack.SequenceNumber
				return wire
			}
		}
		if len(wire.pack.Frames) == 0 {
			fixture.forward(wire, fixture.receiverIn)
			fixture.acknowledge()
		}
	}
	t.Fatal("established Head public Send produced no application wire")
	return nil
}

func consumerEstablishedWireHeadSender(t *testing.T, fixture *windowRoundFixture, frame *windowRoundFrame) *SendSequence {
	t.Helper()
	synctest.Wait()
	fixture.sender.sendBuffer.mutex.Lock()
	defer fixture.sender.sendBuffer.mutex.Unlock()
	for _, sequence := range fixture.sender.sendBuffer.sendSequencesBySequenceId {
		if sequence.sequenceId == RequireIdFromBytes(frame.pack.SequenceId) && sequence.logicalLane == 0 {
			return sequence
		}
	}
	t.Fatal("actual established application has no sender generation")
	return nil
}

type consumerEstablishedWireHeadResult struct {
	blocked  bool
	physical ByteCount
	drops    uint64
}

// The original is never rewritten for the positive roots. A quiet root waits
// for its previous application ACK before another public Send. A promotion
// root emits two applications, ACKs only their real prefix and waits for the
// actual resend selector to promote the still-owned oldest successor.
func testConsumerEstablishedWireHead(t *testing.T, compact, promoted, unknown bool) consumerEstablishedWireHeadResult {
	t.Helper()
	GetMessagePoolAggregateStats()
	var result consumerEstablishedWireHeadResult
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
					t.Error("established Head retained quota after Clients joined")
				}
			}
		})
		var target *ReceiveSequence
		var targetId Id
		var targetCost ByteCount
		probe, paid := true, false
		observed := 0
		stage := func(sequence *ReceiveSequence, owner *decodedPackOwner, event string) {
			if !probe || event != "pack_released" || owner == nil || targetId == (Id{}) || owner.receiveItem.messageId != targetId {
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
		fixture, log, _, _ := newConsumerEstablishedWireHeadFixture(t, compact, rx, recovery, packBudget, time.Now().Add(time.Hour), stage)
		last := map[uint32]uint64{}
		warm := consumerEstablishedWireHeadWrite(t, fixture, last)
		fixture.forward(warm, fixture.receiverIn)
		fixture.acknowledge()
		target = consumerForeignHeadSequence(t, fixture, warm)
		sender := consumerEstablishedWireHeadSender(t, fixture, warm)
		contract := target.receiveContract
		if contract == nil || !contract.verifiedReceiveProofSet || contract.expired() ||
			!target.holdPromise.establishedContract || target.holdPromise.ingressHead != nil ||
			target.receiveQueue.Len() != 0 || target.holdPromise.set || target.holdPromise.reservedByteCount != 0 ||
			len(sender.sendItems) != 0 || !sender.sendContractAcked || sender.sendContract == nil ||
			compact && !sender.sendContract.compactContractRecoverySupported ||
			fixture.deliveredCount != 1 || fixture.ackedCount != 1 || root.UsedByteCount() != 0 ||
			fixture.receiver.ContractManager().ReceiveNoContract(fixture.sender.ClientId()) {
			t.Fatal("signed warm application did not fully ACK/drain and publish verified quiet-generation authority")
		}
		var original *windowRoundFrame
		if promoted {
			prefix := consumerEstablishedWireHeadWrite(t, fixture, last)
			original = consumerEstablishedWireHeadWrite(t, fixture, last)
			if !prefix.pack.Head || original.pack.Head || original.pack.ContractId != nil || original.pack.ContractFrame != nil ||
				original.pack.SequenceNumber != prefix.pack.SequenceNumber+1 {
				t.Fatal("actual outstanding prefix did not produce an ordinary nonhead successor")
			}
			fixture.forward(prefix, fixture.receiverIn)
			fixture.acknowledge()
			if len(sender.sendItems) != 1 || sender.sendItems[0].messageId != RequireIdFromBytes(original.pack.MessageId) ||
				sender.sendItems[0].head || target.nextSequenceNumber != original.pack.SequenceNumber {
				t.Fatal("real prefix ACK did not leave exactly the unmodified successor as the oldest sender-owned original")
			}
		}
		foreignHead := consumerForeignHeadWrite(t, fixture, last, 1)
		foreignTail := consumerForeignHeadWrite(t, fixture, last, 1)
		foreign := consumerForeignHeadSequence(t, fixture, foreignHead)
		fixture.forward(foreignTail, fixture.receiverIn)
		foreignId := RequireIdFromBytes(foreignTail.pack.MessageId)
		foreignOwner := foreign.receiveQueue.GetByMessageId(foreignId)
		if foreignOwner == nil || foreignOwner.committed || foreignOwner.memoryBudget != rx || foreignOwner.QueueByteCount() != foreignBytes ||
			foreign.holdPromise.set || foreign.holdPromise.reservedByteCount != 0 || foreign.holdPromise.ingressLoanByteCount != 0 ||
			rx.UsedByteCount() != foreignBytes || root.UsedByteCount() != foreignBytes ||
			recovery.Available() != foreignBytes || root.Available() != foreignBytes || client.Available() != foreignBytes {
			t.Fatal("entire RX is not held by the actual foreign tentative original with independent Head/root capacity")
		}
		var head *windowRoundFrame
		if promoted {
			head = pressureContractNaturalRetry(t, fixture, original.pack)
			if !bytes.Equal(head.pack.MessageId, original.pack.MessageId) || !bytes.Equal(head.pack.SequenceId, original.pack.SequenceId) ||
				head.pack.SequenceNumber != original.pack.SequenceNumber || !proto.Equal(head.pack.Frames[0], original.pack.Frames[0]) {
				t.Fatal("actual timeout promotion changed the sender-owned original identity or payload")
			}
		} else {
			if len(sender.sendItems) != 0 {
				t.Fatal("quiet Head was emitted before the actual prior window fully ACKed")
			}
			head = consumerEstablishedWireHeadWrite(t, fixture, last)
		}
		if !head.pack.Head || head.pack.Nack || head.pack.ContractAhead || len(head.pack.Frames) != 1 ||
			head.pack.Frames[0].MessageType != protocol.MessageType_IpIpPacketFromProvider ||
			RequireIdFromBytes(head.pack.SequenceId) != target.sequenceId || head.pack.SequenceNumber != target.nextSequenceNumber ||
			target.nextSequenceNumber != target.holdPromise.ingressFloor || target.receiveQueue.Len() != 0 ||
			target.holdPromise.ingressHead != nil || target.holdPromise.set || target.holdPromise.reservedByteCount != 0 {
			t.Fatal("real producer did not emit an exact current application Head without its own paid tail/promise")
		}
		if compact {
			if head.pack.ContractFrame != nil || head.pack.ContractId == nil || RequireIdFromBytes(head.pack.ContractId) != contract.contractId {
				t.Fatal("actual negotiated producer did not reference the current verified compact contract")
			}
		} else {
			if head.pack.ContractId != nil || head.pack.ContractFrame == nil || head.pack.ContractFrame.MessageType != protocol.MessageType_TransferContract {
				t.Fatal("actual noncompact producer did not repeat its full signed TransferContract")
			}
			var proof protocol.Contract
			if err := proto.Unmarshal(head.pack.ContractFrame.MessageBytes, &proof); err != nil ||
				!fixture.receiver.ContractManager().Verify(proof.StoredContractHmac, proof.StoredContractBytes, proof.ProvideMode) {
				t.Fatal("actual full Head lost the ordinary authority's valid signature", err)
			}
		}
		valid := head
		missing := Id{}
		if unknown {
			if !compact || promoted {
				t.Fatal("unknown compact control must use the actual quiet compact Head")
			}
			var envelope protocol.TransferFrame
			if err := proto.Unmarshal(valid.bytes, &envelope); err != nil || envelope.Pack == nil {
				t.Fatal("compact original has no canonical Pack", err)
			}
			missing = NewId()
			envelope.Pack.ContractId = missing.Bytes()
			wire, err := ProtoMarshal(&envelope)
			if err != nil {
				t.Fatal(err)
			}
			head = &windowRoundFrame{bytes: wire, pack: envelope.Pack}
			fixture.heldFrames = append(fixture.heldFrames, head)
			if !bytes.Equal(head.pack.MessageId, valid.pack.MessageId) || head.pack.SequenceNumber != valid.pack.SequenceNumber ||
				!proto.Equal(head.pack.Frames[0], valid.pack.Frames[0]) || target.openReceiveContracts[missing] != nil {
				t.Fatal("unknown compact stimulus changed source original or used a known contract")
			}
		}
		path, err := FilteredTransferPath(head.bytes)
		if err != nil {
			t.Fatal(err)
		}
		scan, status := scanReceiveMemoryPack(head.bytes, path)
		if status != receiveMemoryPackScanReady || !scan.loanSafe || !scan.head || scan.consumerFrameCount != 1 ||
			scan.frameCount != 1 || scan.retainedByteCount <= 0 || scan.retainedByteCount > recovery.Available() ||
			scan.retainedByteCount > root.Available() || scan.retainedByteCount > client.Available() ||
			target.receiveContract != contract || foreign.ctx.Err() != nil {
			t.Fatal("actual established Head is not independently payable under exact foreign-only RX pressure")
		}
		targetId, targetCost = scan.messageId, scan.retainedByteCount
		result.physical = targetCost
		expected := bytes.Clone(valid.pack.Frames[0].MessageBytes)
		targetDeliveries := 0
		fixture.receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
			for _, frame := range frames {
				if frame.MessageType == protocol.MessageType_IpIpPacketFromProvider && bytes.Equal(frame.MessageBytes, expected) {
					targetDeliveries++
				}
			}
		})
		beforeDelivered, beforeAcked := fixture.deliveredCount, fixture.ackedCount
		beforeDebit := contract.ackedByteCount + contract.unackedByteCount
		beforeForeign := foreign.holdPromise
		beforeRepair := foreign.heldHeadRepair
		beforeRoot, beforeRx := root.UsedByteCount(), rx.Stats()
		beforeReserved, beforeReleased := recovery.Counts()
		beforeDrop := fixture.receiver.receivePackHandoffDropCount.Load()
		beforeDropBytes := fixture.receiver.receivePackHandoffDropByteCount.Load()
		beforeMissing := fixture.receiver.missingContractRequestCount.Load()
		// forward consumes its caller reference; retain a distinct immutable
		// valid source for exact recovery, including every failed assertion.
		MessagePoolShareReadOnly(valid.bytes)
		retry := &windowRoundFrame{bytes: valid.bytes, pack: valid.pack}
		fixture.heldFrames = append(fixture.heldFrames, retry)
		fixture.forward(head, fixture.receiverIn)
		probe = false
		result.blocked = observed == 0
		result.drops = fixture.receiver.receivePackHandoffDropCount.Load() - beforeDrop
		if result.blocked {
			if paid || result.drops != 1 || fixture.receiver.receivePackHandoffDropByteCount.Load()-beforeDropBytes != uint64(scan.messageByteCount) ||
				fixture.deliveredCount != beforeDelivered || fixture.ackedCount != beforeAcked || len(fixture.receiverOut) != 0 ||
				target.nextSequenceNumber != scan.sequenceNumber || target.holdPromise.ingressHead != nil ||
				contract.ackedByteCount+contract.unackedByteCount != beforeDebit {
				t.Fatal("current Head refusal was not exact predecode pressure without delivery/ACK/debit")
			}
		} else if observed != 1 || !paid || result.drops != 0 {
			t.Fatal("current Head did not use exactly one independently paid physical child owner")
		}
		afterReserved, afterReleased := recovery.Counts()
		wantCharge := targetCost
		if result.blocked {
			wantCharge = 0
		}
		if afterReserved-beforeReserved != wantCharge || afterReleased-beforeReleased != wantCharge || recovery.UsedByteCount() != 0 ||
			root.UsedByteCount() != beforeRoot || rx.Stats() != beforeRx || client.UsedByteCount() != beforeRoot ||
			foreign.receiveQueue.GetByMessageId(foreignId) != foreignOwner || foreignOwner.committed || foreignOwner.memoryBudget != rx ||
			foreign.holdPromise != beforeForeign || foreign.heldHeadRepair != beforeRepair || foreign.ctx.Err() != nil || target.ctx.Err() != nil ||
			target.receiveContract != contract || packBudget.UsedByteCount() != 0 || packBudget.Stats().QuotaByteCount != 0 {
			t.Fatal("current Head spent foreign custody/escrow, changed authority, exceeded caps or leaked its exact C")
		}
		if unknown {
			if !result.blocked {
				if fixture.deliveredCount != beforeDelivered || fixture.ackedCount != beforeAcked || targetDeliveries != 0 ||
					target.nextSequenceNumber != scan.sequenceNumber || contract.ackedByteCount+contract.unackedByteCount != beforeDebit ||
					fixture.receiver.missingContractRequestCount.Load()-beforeMissing != 1 {
					t.Fatal("unknown compact physical owner bypassed the ordinary contract-missing refusal")
				}
				proofRequest := fixture.take(fixture.receiverOut)
				if proofRequest.ack == nil || proofRequest.ack.MissingContractId == nil || proofRequest.ack.Selective ||
					!bytes.Equal(proofRequest.ack.MessageId, head.pack.MessageId) || RequireIdFromBytes(proofRequest.ack.MissingContractId) != missing || len(fixture.receiverOut) != 0 {
					t.Fatal("unknown compact published acceptance rather than one exact missing-proof request")
				}
			}
		} else if !result.blocked {
			if fixture.deliveredCount != beforeDelivered+1 || targetDeliveries != 1 ||
				target.nextSequenceNumber != scan.sequenceNumber+1 || len(fixture.receiverOut) == 0 {
				t.Fatal("independent physical Head did not reach the ordinary delivery/ACK path")
			}
			fixture.acknowledge()
			if fixture.ackedCount != beforeAcked+1 {
				t.Fatal("successful Head did not ACK exactly its retained original")
			}
		}
		fixture.forward(foreignHead, fixture.receiverIn)
		fixture.acknowledge()
		if foreign.receiveQueue.Len() != 0 || rx.UsedByteCount() != 0 || foreign.ctx.Err() != nil {
			t.Fatal("real foreign predecessor did not drain its own held original")
		}
		if result.blocked && unknown {
			// Prove the same unknown reference reaches the real worker after
			// capacity returns before accepting the typed missed-child cause.
			var envelope protocol.TransferFrame
			if err := proto.Unmarshal(retry.bytes, &envelope); err != nil {
				t.Fatal(err)
			}
			envelope.Pack.ContractId = missing.Bytes()
			wire, err := ProtoMarshal(&envelope)
			if err != nil {
				t.Fatal(err)
			}
			invalidRetry := &windowRoundFrame{bytes: wire, pack: envelope.Pack}
			fixture.heldFrames = append(fixture.heldFrames, invalidRetry)
			fixture.forward(invalidRetry, fixture.receiverIn)
			proofRequest := fixture.take(fixture.receiverOut)
			if proofRequest.ack == nil || proofRequest.ack.MissingContractId == nil || proofRequest.ack.Selective ||
				!bytes.Equal(proofRequest.ack.MessageId, head.pack.MessageId) || RequireIdFromBytes(proofRequest.ack.MissingContractId) != missing ||
				fixture.deliveredCount != beforeDelivered+2 || fixture.ackedCount != beforeAcked+2 ||
				contract.ackedByteCount+contract.unackedByteCount != beforeDebit || target.nextSequenceNumber != scan.sequenceNumber {
				t.Fatal("same unknown compact reference escaped normal missing-proof refusal after RX drained")
			}
		}
		if result.blocked || unknown {
			fixture.forward(retry, fixture.receiverIn)
		}
		fixture.acknowledge()
		if fixture.deliveredCount != beforeDelivered+3 || fixture.ackedCount != beforeAcked+3 || targetDeliveries != 1 ||
			target.nextSequenceNumber != scan.sequenceNumber+1 || target.receiveContract != contract ||
			contract.ackedByteCount+contract.unackedByteCount != beforeDebit+max(contract.minUpdateByteCount, scan.messageByteCount) ||
			target.receiveQueue.Len() != 0 || target.holdPromise.ingressHead != nil || target.ctx.Err() != nil ||
			root.UsedByteCount() != 0 || client.UsedByteCount() != 0 || rx.UsedByteCount() != 0 || recovery.UsedByteCount() != 0 ||
			packBudget.UsedByteCount() != 0 || packBudget.Stats().QuotaByteCount != 0 || len(log.messages()) != 0 {
			t.Fatalf("actual current Head retry failed exact delivery/ACK/debit/refund cleanup: delivered=%d acked=%d expected=%d observed=%d", fixture.deliveredCount, fixture.ackedCount, beforeDelivered+3, observed)
		}
		t.Logf("ESTABLISHED_WIRE_HEAD_BEHAVIOR_DRAINED compact=%t promoted=%t unknown=%t blocked=%t C=%d drops=%d", compact, promoted, unknown, result.blocked, result.physical, result.drops)
	})
	return result
}

func TestReceiveConsumerEstablishedQuietCompactHeadUnderForeignRx(t *testing.T) {
	result := testConsumerEstablishedWireHead(t, true, false, false)
	if t.Failed() {
		return
	}
	t.Log("ESTABLISHED_QUIET_COMPACT_HEAD_CLEANUP_JOINED")
	if result.blocked {
		t.Fatalf("FOREIGN_RX_STRANDED_ESTABLISHED_QUIET_COMPACT_HEAD: C=%d drops=%d", result.physical, result.drops)
	}
}

func TestReceiveConsumerEstablishedQuietFullProofHeadUnderForeignRx(t *testing.T) {
	result := testConsumerEstablishedWireHead(t, false, false, false)
	if t.Failed() {
		return
	}
	t.Log("ESTABLISHED_QUIET_FULL_HEAD_CLEANUP_JOINED")
	if result.blocked {
		t.Fatalf("FOREIGN_RX_STRANDED_ESTABLISHED_QUIET_FULL_HEAD: C=%d drops=%d", result.physical, result.drops)
	}
}

func TestReceiveConsumerEstablishedPromotedCompactHeadUnderForeignRx(t *testing.T) {
	result := testConsumerEstablishedWireHead(t, true, true, false)
	if t.Failed() {
		return
	}
	t.Log("ESTABLISHED_PROMOTED_COMPACT_HEAD_CLEANUP_JOINED")
	if result.blocked {
		t.Fatalf("FOREIGN_RX_STRANDED_ESTABLISHED_PROMOTED_COMPACT_HEAD: C=%d drops=%d", result.physical, result.drops)
	}
}

func TestReceiveConsumerEstablishedPromotedFullProofHeadUnderForeignRx(t *testing.T) {
	result := testConsumerEstablishedWireHead(t, false, true, false)
	if t.Failed() {
		return
	}
	t.Log("ESTABLISHED_PROMOTED_FULL_HEAD_CLEANUP_JOINED")
	if result.blocked {
		t.Fatalf("FOREIGN_RX_STRANDED_ESTABLISHED_PROMOTED_FULL_HEAD: C=%d drops=%d", result.physical, result.drops)
	}
}

func TestReceiveConsumerEstablishedCompactUnknownIdUsesOrdinaryWorker(t *testing.T) {
	result := testConsumerEstablishedWireHead(t, true, false, true)
	if t.Failed() {
		return
	}
	t.Log("ESTABLISHED_UNKNOWN_COMPACT_HEAD_CLEANUP_JOINED")
	if result.blocked {
		t.Fatalf("ESTABLISHED_UNKNOWN_COMPACT_CHILD_PATH_UNEXERCISED: C=%d drops=%d; same unknown reference reached worker after foreign drain", result.physical, result.drops)
	}
}
