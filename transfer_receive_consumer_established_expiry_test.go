// Historical receive establishment funds physical storage only. Real signed
// expiration still refuses delivery and ACK in the unchanged receive worker.
package connect

import (
	"context"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The maintained signed handoff fixture uses these same immutable settings,
// routes and joins; only its grant authority supplies a signed deadline.
func newEstablishedCurrentExpiryFixture(t *testing.T, rx, recovery, pack *TransferMemoryBudget, deadline time.Time, ownerStage func(*ReceiveSequence, *decodedPackOwner, string)) (*windowRoundFixture, *receiveGapTestLogger, *expirationLifecycleAuthority, *expirationLifecycleAuthority) {
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
		value.SendBufferSettings.CompactContractHead = false
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

func TestReceiveConsumerEstablishedCurrentPreservesForeignPromiseAndProgresses(t *testing.T) {
	result := testReceiveConsumerForeignCurrentHead(t, true)
	if !t.Failed() && result.blocked {
		t.Fatal("ESTABLISHED_CURRENT_FAILED_INDEPENDENT_FOREIGN_PROMISE_PROGRESS")
	}
}

func TestReceiveConsumerEstablishedPhysicalPermissionDoesNotExtendSignedExpiry(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		const cost = ByteCount(5120)
		root := NewTransferMemoryBudget(2 * cost)
		client := NewTransferMemoryBudgetWithParent(2*cost, root)
		rx := NewTransferMemoryBudgetWithParent(cost, client)
		recovery := NewTransferMemoryBudgetWithParent(cost, client)
		pack := NewTransferMemoryBudgetWithParent(cost, client)
		t.Cleanup(func() {
			for _, budget := range []*TransferMemoryBudget{pack, recovery, rx, client, root} {
				assertRetainedBudgetBalance(t, budget)
				if budget.Stats().QuotaByteCount != 0 {
					t.Error("expired established fixture retained logical permission")
				}
			}
			t.Log("ESTABLISHED_EXPIRED_CURRENT_CLEANUP_JOINED")
		})
		var targetId Id
		var target *ReceiveSequence
		paid := false
		observed := 0
		stage := func(sequence *ReceiveSequence, owner *decodedPackOwner, event string) {
			if event != "pack_released" || owner == nil || targetId == (Id{}) || owner.receiveItem.messageId != targetId {
				return
			}
			observed++
			paid = sequence == target && owner.inUse && owner.receivePack.receiveMemoryOwned &&
				owner.receiveItem.memoryBudget == recovery && owner.receiveItem.QueueByteCount() == cost &&
				owner.receivePack.ingressLoanByteCount == 0 && recovery.UsedByteCount() == cost &&
				rx.UsedByteCount() == cost && root.UsedByteCount() == 2*cost &&
				target.receiveContract != nil && target.receiveContract.expired()
		}
		deadline := time.Now().Add(time.Second)
		fixture, log, forward, reverse := newEstablishedCurrentExpiryFixture(t, rx, recovery, pack, deadline, stage)
		last := map[uint32]uint64{}
		foreignHead := consumerForeignHeadWrite(t, fixture, last, 1)
		head := consumerForeignHeadWrite(t, fixture, last, 0)
		tail := consumerForeignHeadWrite(t, fixture, last, 1)
		foreign := consumerForeignHeadSequence(t, fixture, foreignHead)
		target = consumerForeignHeadSequence(t, fixture, head)
		contract := target.receiveContract
		if contract == nil || !contract.verifiedReceiveProofSet || contract.expired() ||
			contract.expirationTimeUnixMilli == nil || *contract.expirationTimeUnixMilli != deadline.UnixMilli() ||
			!target.holdPromise.establishedContract || target.nextSequenceNumber != head.pack.SequenceNumber ||
			target.holdPromise.ingressFloor != head.pack.SequenceNumber || target.holdPromise.ingressHead != nil ||
			head.pack.Head || head.pack.ContractFrame != nil || head.pack.ContractId != nil ||
			fixture.deliveredCount != 0 || fixture.ackedCount != 0 {
			t.Fatal("expiry witness did not establish actual signed ordered current authority before deadline")
		}
		fixture.forward(tail, fixture.receiverIn)
		held := foreign.receiveQueue.GetByMessageId(RequireIdFromBytes(tail.pack.MessageId))
		if held == nil || held.committed || held.memoryBudget != rx || held.QueueByteCount() != cost ||
			rx.UsedByteCount() != cost || recovery.UsedByteCount() != 0 || foreign.holdPromise.set || len(fixture.receiverOut) != 0 {
			t.Fatal("expiry witness lacks actual full foreign RX custody")
		}
		forward.blockRenewals.Store(true)
		reverse.blockRenewals.Store(true)
		time.Sleep(time.Until(deadline) + time.Millisecond)
		synctest.Wait()
		if !contract.expired() || target.ctx.Err() != nil || foreign.ctx.Err() != nil ||
			!target.holdPromise.establishedContract || rx.UsedByteCount() != cost ||
			recovery.Available() != cost || root.Available() != cost {
			t.Fatal("real signed expiry did not leave the live historical generation and independent physical capacity")
		}
		targetId = RequireIdFromBytes(head.pack.MessageId)
		beforeDrops := fixture.receiver.receivePackHandoffDropCount.Load()
		beforeReserved, beforeReleased := recovery.Counts()
		fixture.forward(head, fixture.receiverIn)
		afterReserved, afterReleased := recovery.Counts()
		if observed != 1 || !paid || fixture.deliveredCount != 0 || fixture.ackedCount != 0 || len(fixture.receiverOut) != 0 ||
			target.nextSequenceNumber != head.pack.SequenceNumber || target.ctx.Err() == nil ||
			target.holdPromise.ingressHead != nil || !target.holdPromise.closed ||
			recovery.UsedByteCount() != 0 || afterReserved-beforeReserved != cost || afterReleased-beforeReleased != cost ||
			fixture.receiver.receivePackHandoffDropCount.Load() != beforeDrops ||
			foreign.receiveQueue.GetByMessageId(held.messageId) != held || held.committed ||
			foreign.ctx.Err() != nil || rx.UsedByteCount() != cost || root.UsedByteCount() != cost {
			t.Fatalf("signed-expired current physical owner escaped ordinary refusal/refund: observed=%d paid=%t delivered=%d acked=%d", observed, paid, fixture.deliveredCount, fixture.ackedCount)
		}
		// receive() records the contract refusal, then Run records the same
		// returned error before closing. Both belong to this one expired item.
		// Require the exact pair; any other worker error still fails the root.
		contractRefusal, workerExit := 0, 0
		for _, message := range log.messages() {
			if strings.Contains(message, "drop queue head no contract ") &&
				strings.HasSuffix(message, "head=false, contract=false, rcontract=true\n") {
				contractRefusal++
			} else if strings.HasSuffix(message, " exit could not receive ack = No contract\n") {
				workerExit++
			} else {
				t.Error("signed-expiry fixture observed an unrelated worker error")
			}
		}
		if contractRefusal != 1 || workerExit != 1 {
			t.Fatalf("signed-expired original did not produce the exact worker refusal pair: contract=%d exit=%d", contractRefusal, workerExit)
		}
		t.Log("ESTABLISHED_EXPIRED_CURRENT_PHYSICAL_ONLY_NO_DELIVERY_NO_ACK")
	})
}
