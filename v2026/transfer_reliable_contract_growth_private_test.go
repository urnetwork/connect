// A real opening contract can grow an already selected raw packet's bound.
package connect

import (
	"bytes"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
	"google.golang.org/protobuf/proto"
)

// Signed contract creation, head delivery, and its receiver ACK are real. Only
// their ordering is gated; the actual retained reservation is never replaced.
func TestReliableRawContractGrowthRetainsOriginalAndRollsBackOnlyDataDebit(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		root := reliableCustodyRoot(t)
		var frame *protocol.Frame
		var witness []byte
		admitted := false
		t.Cleanup(func() {
			if frame != nil && !admitted {
				MessagePoolReturn(frame.MessageBytes)
			}
			if witness != nil && !MessagePoolReturn(witness) {
				t.Error("signed-contract original kept a raw owner after all clients joined")
			}
		})
		runEntered, runRelease := make(chan struct{}), make(chan struct{})
		dataEntered, dataRelease := make(chan struct{}), make(chan struct{})
		headAckRelease := make(chan struct{})
		var runOnce, runReleaseOnce, dataReleaseOnce, ackReleaseOnce sync.Once
		resumeRun := func() { runReleaseOnce.Do(func() { close(runRelease) }) }
		resumeData := func() { dataReleaseOnce.Do(func() { close(dataRelease) }) }
		resumeAck := func() { ackReleaseOnce.Do(func() { close(headAckRelease) }) }
		var armed atomic.Bool
		var source *SendSequence
		var observedSource atomic.Pointer[SendSequence]
		var content string
		var dataWrites atomic.Int32
		var fixture *expirationLifecycleFixture
		var observedFixture atomic.Pointer[expirationLifecycleFixture]
		fixture = newExpirationLifecycleFixture(t, false, func(settings *ClientSettings) {
			settings.SendBufferSettings.TransferWireMessageObserver = func(observation TransferWireMessageObservation) {
				var transfer protocol.TransferFrame
				if err := proto.Unmarshal(observation.TransferFrameBytes, &transfer); err != nil {
					t.Error("decode actual signed-contract wire observation", err)
					return
				}
				pack := transfer.Pack
				if pack == nil {
					return
				}
				types := make([]protocol.MessageType, 0, len(pack.Frames))
				for _, frame := range pack.Frames {
					types = append(types, frame.MessageType)
					if observed := observedSource.Load(); observed != nil && content != "" && bytes.Equal(pack.SequenceId, observed.sequenceId.Bytes()) && frame.MessageType == protocol.MessageType_TestSimpleMessage {
						var message protocol.SimpleMessage
						if err := proto.Unmarshal(frame.MessageBytes, &message); err != nil {
							t.Error("decode actual original body", err)
						} else if message.Content == content {
							dataWrites.Add(1)
						}
					}
				}
				t.Logf("CONTRACT_WIRE seq=%d types=%v originalDataWrites=%d", pack.SequenceNumber, types, dataWrites.Load())
			}
			settings.SendBufferSettings.WindowSizing = WindowSizingConstant
			settings.SendBufferSettings.ApplyWindowSizing()
			settings.SendBufferSettings.ResendQueueBudget = NewTransferMemoryBudgetWithParent(2*1024*1024, root)
			settings.SendBufferSettings.ResendQueueRetainedByteAccounting = true
			settings.SendBufferSettings.ResendQueueMaxByteCount = 2 * 1024 * 1024
			settings.SendBufferSettings.ResendQueueMinByteCount = 0
			settings.SendBufferSettings.SequenceBufferSize = 32
			settings.SendBufferSettings.beforeRunSendSequenceForTest = func(id sendSequenceId) {
				if ready := observedFixture.Load(); ready != nil && id.Destination == ready.receiver.ClientId() {
					runOnce.Do(func() { close(runEntered) })
					<-runRelease
					// Admission has returned before the test releases this gate.
					// Run and its ACK worker have not started. Install only this
					// worker's private observation, preserving every policy field.
					target := observedSource.Load()
					if target == nil || target.id() != id {
						panic("signed-contract target was not published before worker start")
					}
					privateSettings := *target.sendBufferSettings
					privateSettings.beforeGroupRetainForTest = func() {
						// This callback runs only on this exact target worker.
						// All bootstrap and other-client hooks remain nil.
						if target.sendContract != nil && len(target.sendItems) == 1 &&
							target.sendItems[0].contractControl && armed.CompareAndSwap(true, false) {
							close(dataEntered)
							<-dataRelease
						}
					}
					target.sendBufferSettings = &privateSettings
				}
			}
		})
		observedFixture.Store(fixture)
		t.Cleanup(fixture.close)
		sibling := reliableCustodyPair(t, root, nil)
		t.Cleanup(resumeRun)
		t.Cleanup(resumeData)
		t.Cleanup(resumeAck)
		reliableCustodyWarm(t, sibling)
		barrier := &expirationLifecycleWireBarrier{entered: make(chan struct{}), release: headAckRelease}
		fixture.ackBarrier.Store(barrier)
		content = strings.Repeat("contract-growth-original", 60)
		frame = RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: content})
		witness = MessagePoolShareReadOnly(frame.MessageBytes)
		ackTarget := &reliableRawCustodyTarget{results: make(chan reliableRawCustodyResult, 2)}
		accepted, err := fixture.sender.sendRawWithTimeoutDetailed(protocol.MessageType_TestSimpleMessage,
			frame.MessageBytes, fixture.receiver.ClientId(), ackTarget, 0, 30*time.Second)
		if !accepted || err != nil {
			t.Fatalf("raw contract-bearing input admission: %t %v", accepted, err)
		}
		admitted = true
		<-runEntered
		source = fixture.sender.sendBuffer.lookupSendSequence(sendSequenceId{Destination: fixture.receiver.ClientId()}, nil)
		observedSource.Store(source)
		if source == nil || source.sendContract != nil || source.resendQueue.budget == nil ||
			source.resendQueue.budget.Parent() != root || source.resendQueue.budget.TotalByteCount() != 2*1024*1024 ||
			!source.resendQueue.lifetimeBudget {
			t.Fatal("control did not stop with the actual parented 2-MiB retained budget before its first contract")
		}
		probe := SendPack{Frame: frame, TransferOptions: TransferOptions{Ack: true}}
		beforeContract := source.retainedSendPackByteCount(&probe)
		var held []*windowRoundFrame
		wire, _ := reliableCustodyHold(t, sibling, root, 2*DefaultMtu)
		held = append(held, wire)
		wire, cost := reliableCustodyHold(t, sibling, root, DefaultMtu)
		held = append(held, wire)
		for n := 0; root.Available()+source.resendMemoryGrant-cost >= beforeContract; n++ {
			if n == 512 {
				t.Fatal("bounded actual sibling census did not isolate contract headroom")
			}
			wire, nextCost := reliableCustodyHold(t, sibling, root, DefaultMtu)
			if nextCost != cost {
				t.Fatal("identical sibling shape changed retained cost")
			}
			held = append(held, wire)
		}
		availableBeforeContract := root.Available()
		grantBeforeContract := source.resendMemoryGrant
		t.Logf("CONTRACT_BEFORE available=%d grant=%d totalOwnedAndFree=%d beforeNeed=%d", availableBeforeContract, grantBeforeContract, availableBeforeContract+grantBeforeContract, beforeContract)
		armed.Store(true)
		resumeRun()
		<-dataEntered
		afterContract := source.retainedSendPackByteCount(&probe)
		if afterContract <= beforeContract || availableBeforeContract+grantBeforeContract < beforeContract || availableBeforeContract+grantBeforeContract >= afterContract {
			t.Fatalf("real signed contract did not cross its retained pool boundary: before=%d after=%d available=%d body=%d", beforeContract, afterContract, availableBeforeContract, len(frame.MessageBytes))
		}
		resumeAck()
		resumeData()
		synctest.Wait()
		select {
		case result := <-ackTarget.results:
			fixture.stateLock.Lock()
			delivered := fixture.deliveries[content]
			fixture.stateLock.Unlock()
			if result.err == nil {
				t.Fatalf("FIXTURE_UNEXPECTED_EARLY_SUCCESS: before=%d after=%d available=%d grant=%d actualDelivered=%d actualDataFrames=%d", beforeContract, afterContract, root.Available(), source.resendMemoryGrant, delivered, dataWrites.Load())
			}
			t.Fatalf("RELIABLE_CONTRACT_GROWTH_CUSTODY_LOST: accepted raw original failed before planned capacity release: err=%v before=%d after=%d available=%d grant=%d actualDelivered=%d actualDataFrames=%d", result.err, beforeContract, afterContract, root.Available(), source.resendMemoryGrant, delivered, dataWrites.Load())
		default:
		}
		waitsForExactBound := false
		root.admissionLock.Lock()
		for _, waiter := range root.reserveWaiters {
			if waiter.budget == source.resendQueue.budget && waiter.byteCount == afterContract {
				waitsForExactBound = true
			}
		}
		root.admissionLock.Unlock()
		fixture.stateLock.Lock()
		deliveredBeforeCapacity := fixture.deliveries[content]
		fixture.stateLock.Unlock()
		if !waitsForExactBound || len(source.sendItems) != 0 || source.sendContract.unackedByteCount != 0 || deliveredBeforeCapacity != 0 || dataWrites.Load() != 0 {
			t.Fatal("contract growth failed to await exact bytes or left an unwritten data debit/delivery")
		}
		priorControlDebit := source.sendContract.ackedByteCount
		if priorControlDebit < source.sendContract.minUpdateByteCount {
			t.Fatal("actual opening head ACK was not accounted before data recovery")
		}
		for _, heldWire := range held {
			sibling.forward(heldWire, sibling.receiverIn)
			sibling.acknowledge()
		}
		result := <-ackTarget.results
		synctest.Wait()
		fixture.stateLock.Lock()
		delivered := fixture.deliveries[content]
		fixture.stateLock.Unlock()
		controlOwned := signedContractOtherOwnedBytes(t, []*Client{fixture.sender, fixture.receiver, sibling.sender, sibling.receiver}, source, content)
		wantDebit := priorControlDebit + max(source.sendContract.minUpdateByteCount, ByteCount(len(frame.MessageBytes)))
		if result.err != nil || result.index != 0 || ackTarget.count.Load() != 1 || delivered != 1 || len(source.sendItems) != 0 || source.resendMemoryGrant != 0 || root.UsedByteCount() != controlOwned ||
			source.sendContract.unackedByteCount != 0 || source.sendContract.ackedByteCount != wantDebit || dataWrites.Load() != 1 {
			t.Fatalf("signed-contract original did not recover once with exact debit: result=%+v callbacks=%d received=%d retained=%d acked=%d wanted=%d unacked=%d", result, ackTarget.count.Load(), delivered, root.UsedByteCount(), source.sendContract.ackedByteCount, wantDebit, source.sendContract.unackedByteCount)
		}
	})
}

// Live provide/control work has a separate retry lifetime. Account for every
// such owner rather than confusing global occupancy with this data item's ACK.
func signedContractOtherOwnedBytes(t *testing.T, clients []*Client, target *SendSequence, content string) ByteCount {
	t.Helper()
	owned := ByteCount(0)
	seen := map[*SendSequence]bool{}
	for _, client := range clients {
		client.sendBuffer.mutex.Lock()
		sequences := make([]*SendSequence, 0, len(client.sendBuffer.sendSequencesBySequenceId))
		for _, sequence := range client.sendBuffer.sendSequencesBySequenceId {
			sequences = append(sequences, sequence)
		}
		client.sendBuffer.mutex.Unlock()
		for _, sequence := range sequences {
			if seen[sequence] {
				continue
			}
			seen[sequence] = true
			if sequence.resendQueue.budget == nil || sequence.resendQueue.budget.admissionRoot() != target.resendQueue.budget.admissionRoot() {
				continue
			}
			if sequence == target {
				if len(sequence.sendItems) != 0 || sequence.resendMemoryGrant != 0 {
					t.Fatal("original data sequence retained an item or grant after its real ACK")
				}
				continue
			}
			owned += sequence.resendMemoryGrant
			for _, item := range sequence.sendItems {
				pack := decodeSendPackLifecycleWirePack(t, item.transferFrameBytes)
				types := make([]protocol.MessageType, 0, len(pack.Frames))
				for _, frame := range pack.Frames {
					types = append(types, frame.MessageType)
					if frame.MessageType == protocol.MessageType_TestSimpleMessage {
						var message protocol.SimpleMessage
						if err := proto.Unmarshal(frame.MessageBytes, &message); err != nil || message.Content == content {
							t.Fatal("the original or undecodable data remained owned by another sequence")
						}
					}
					if frame.MessageType != protocol.MessageType_TransferProvide {
						t.Fatalf("unexpected live non-target owner type %v", frame.MessageType)
					}
				}
				if len(pack.Frames) == 0 && !item.contractControl {
					t.Fatal("unclassified live non-target retained owner")
				}
				owned += item.queueByteCount
				t.Logf("CONTRACT_OTHER_OWNER client=%s sequence=%s message=%s bytes=%d types=%v", client.ClientId(), sequence.sequenceId, item.messageId, item.queueByteCount, types)
			}
		}
	}
	return owned
}
