package connect

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

func TestRetainedHierarchyForcesExactSequenceOwnership(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		root := NewTransferMemoryBudget(kib(128))
		sendBudget := NewTransferMemoryBudgetWithParent(kib(64), root)
		receiveBudget := NewTransferMemoryBudgetWithParent(kib(64), root)
		// Deliberately omit both flags: a parented budget must not reach the
		// legacy check-then-Reserve path even after an incomplete settings copy.
		fixture := newWindowRoundFixture(t, func(settings *SendBufferSettings) {
			settings.ResendQueueBudget = sendBudget
			settings.ResendQueueRetainedByteAccounting = false
		}, func(settings *ReceiveBufferSettings) {
			settings.ReceiveQueueBudget = receiveBudget
			settings.ReceiveQueueRetainedByteAccounting = false
		})
		fixture.forward(fixture.write(100), fixture.receiverIn)
		fixture.acknowledge()
		head, tail := fixture.write(100), fixture.write(100)
		fixture.forward(tail, fixture.receiverIn)
		if sendBudget.UsedByteCount() == 0 || receiveBudget.UsedByteCount() == 0 || root.UsedByteCount() != sendBudget.UsedByteCount()+receiveBudget.UsedByteCount() {
			t.Fatal("live sender/reorder owners were not composed")
		}
		AssertEqual(t, sendBudget.floorByteCount.Load(), int64(0))
		AssertEqual(t, receiveBudget.floorByteCount.Load(), int64(0))
		fixture.forward(head, fixture.receiverIn)
		fixture.acknowledge()
		AssertEqual(t, root.UsedByteCount(), ByteCount(0))
	})
}

func TestRetainedReceiveFanoutHasNoEmptyFlowOrFloorOverdraft(t *testing.T) {
	const count = 32
	const charge = ByteCount(2048)
	budget := NewTransferMemoryBudget(2 * charge)
	var admitted atomic.Int32
	var wg sync.WaitGroup
	items := make([]*receiveItem, count)
	queues := make([]*receiveQueue, count)
	for i := range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			settings := DefaultReceiveBufferSettingsWithBufferSize(1)
			settings.ReceiveQueueBudget = budget
			settings.ReceiveQueueMinByteCount = kib(64)
			settings.ReceiveQueueRetainedByteAccounting = true
			seq := newReceiveSequence(context.Background(), &Client{}, SourceId(NewId()), NewId(), TransferKey{}, settings)
			defer seq.Close()
			queues[i] = seq.receiveQueue
			item := &receiveItem{transferItem: transferItem{
				messageId: NewId(), sequenceNumber: 1, messageByteCount: 1, queueByteCount: charge,
			}}
			if seq.reserveHeldItem(item, false) {
				seq.receiveQueue.Add(item)
				items[i] = item
				admitted.Add(1)
			}
		}()
	}
	wg.Wait()
	if admitted.Load() != 2 || budget.UsedByteCount() != 2*charge || budget.floorByteCount.Load() != 0 {
		t.Fatalf("fanout escaped aggregate: admitted=%d used=%d floors=%d", admitted.Load(), budget.UsedByteCount(), budget.floorByteCount.Load())
	}
	for i, item := range items {
		queues[i].Clear()
		if item != nil {
			// Clearing/popping does not return bytes while an owner is live.
			if budget.UsedByteCount() < charge {
				t.Fatal("queue teardown released a live receive owner")
			}
			item.messagePoolReturn()
		}
	}
	assertRetainedBudgetBalance(t, budget)
}

func TestRetainedResendFanoutRetryResizeAndTeardown(t *testing.T) {
	assertMessagePoolOwnership(t)
	const count = 32
	charge := (&sendItem{}).retainedMemoryByteCount(1400, 1, false)
	budget := NewTransferMemoryBudget(3 * charge)
	var admitted atomic.Int32
	var completed atomic.Int32
	var wg sync.WaitGroup
	sequences := make([]*SendSequence, count)
	for i := range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			queue := newResendQueue(budget, kib(64))
			queue.setLifetimeBudget()
			seq := &SendSequence{resendQueue: queue}
			sequences[i] = seq
			item := takeSendItem()
			*item = sendItem{transferItem: transferItem{messageId: NewId()}, expectsAck: true}
			if !item.reserveMemory(budget, charge) {
				item.messagePoolReturn()
				return
			}
			item.transferFrameBytes = MessagePoolGet(1400)
			item.acks.add(sendAckRecord{callback: func(error) { completed.Add(1) }})
			seq.sendItems = []*sendItem{item}
			queue.Add(item)
			queue.RemoveByMessageId(item.messageId)
			// Deliberately leave every admitted item outside the heap: a retry
			// may fail or cancellation may arrive at exactly this boundary.
			admitted.Add(1)
		}()
	}
	wg.Wait()
	if admitted.Load() != 3 || budget.UsedByteCount() != 3*charge || budget.floorByteCount.Load() != 0 {
		t.Fatalf("retry pop escaped aggregate: admitted=%d used=%d", admitted.Load(), budget.UsedByteCount())
	}
	budget.SetTotalByteCount(charge)
	for _, seq := range sequences {
		if len(seq.sendItems) > 0 {
			item := seq.sendItems[0]
			if !item.reserveFrameRewrite(1400, 1, false) || item.reserveFrameRewrite(6000, 1, false) {
				t.Fatal("rewrite did not preserve its pre-reserved envelope or escaped shrink")
			}
			seq.resendQueue.Add(item)
			seq.resendQueue.RemoveByMessageId(item.messageId)
		}
	}
	for _, seq := range sequences {
		seq.releaseRetainedSendItems(context.Canceled)
		seq.releaseRetainedSendItems(context.Canceled)
	}
	if completed.Load() != 3 {
		t.Fatalf("teardown callbacks=%d, want exactly three", completed.Load())
	}
	assertRetainedBudgetBalance(t, budget)
	// A larger rewrite acquires only growth, and that growth has one owner.
	budget.SetTotalByteCount(kib(64))
	item := &sendItem{}
	if !item.reserveMemory(budget, charge) || !item.reserveFrameRewrite(6000, 1, false) {
		t.Fatal("available rewrite capacity was refused")
	}
	if budget.UsedByteCount() != item.retainedMemoryByteCount(6000, 1, false) {
		t.Fatal("rewrite growth was not charged exactly")
	}
	item.messagePoolReturn()
	assertRetainedBudgetBalance(t, budget)
}

func TestRetainedSendRefusalPrecedesWireAndRollsBackIdentity(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, canceled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		if canceled {
			cancel()
		}
		budget := NewTransferMemoryBudget(1)
		queue := newResendQueue(budget, kib(64))
		queue.setLifetimeBudget()
		frame := budgetTestFrame(1400)
		byteCount := MessageByteCount([]*protocol.Frame{frame})
		contract := &sequenceContract{unackedByteCount: byteCount}
		seq := &SendSequence{ctx: ctx, client: &Client{}, resendQueue: queue,
			nextSequenceNumber: 7, sendContract: contract, sendContractFrameDue: true,
			sendContractAcked: true, sendBufferSettings: DefaultSendBufferSettings()}
		calls := 0
		seq.send([]*protocol.Frame{frame}, func(err error) {
			calls++
			want := ErrSendPackNotAdmitted
			if canceled {
				want = context.Canceled
			}
			if !errors.Is(err, want) {
				t.Errorf("refusal=%v want %v", err, want)
			}
		}, true, false)
		cancel()
		if calls != 1 || seq.nextSequenceNumber != 7 || contract.unackedByteCount != 0 ||
			!seq.sendContractFrameDue || seq.resendQueue.Len() != 0 || len(seq.sendItems) != 0 ||
			seq.client.initialSendWriteCount.Load() != 0 {
			t.Fatal("unpublished refusal left identity, contract debit, or a write")
		}
		assertRetainedBudgetBalance(t, budget)
	}
}

func TestRetainedSendResidualWaitsAndSiblingReleaseWakes(t *testing.T) {
	for _, residual := range []ByteCount{1, 8000} {
		t.Run(fmt.Sprintf("residual_%d", residual), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				assertMessagePoolOwnership(t)
				budget := NewTransferMemoryBudget(kib(128))
				fixture := newWindowRoundFixture(t, func(settings *SendBufferSettings) {
					settings.ResendQueueBudget = budget
					settings.ResendQueueMinByteCount = kib(64)
					settings.ResendQueueRetainedByteAccounting = true
				}, nil)
				first := fixture.write(100)
				fixture.forward(first, fixture.receiverIn)
				fixture.acknowledge()
				if budget.UsedByteCount() != 0 {
					t.Fatal("initial ACK did not return its lifetime reservation")
				}
				siblingBytes := budget.TotalByteCount() - residual
				if !budget.TryReserve(siblingBytes) {
					t.Fatal("sibling admission failed")
				}
				completed := make(chan bool, 1)
				go func() {
					admitted, _ := fixture.send(5000)
					completed <- admitted
				}()
				synctest.Wait()
				time.Sleep(100 * time.Millisecond)
				synctest.Wait()
				if got := fixture.sender.initialSendWriteCount.Load(); got != 1 || len(fixture.senderOut) != 0 {
					t.Fatalf("sub-item residual serialized/dropped instead of waiting: writes=%d wire=%d", got, len(fixture.senderOut))
				}
				if budget.UsedByteCount() != siblingBytes {
					t.Fatal("waiting packet escaped retained admission")
				}
				budget.Release(siblingBytes)
				synctest.Wait()
				if !<-completed {
					t.Fatal("sibling release did not admit the blocked packet")
				}
				second := fixture.takePack(1)
				fixture.forward(second, fixture.receiverIn)
				fixture.acknowledge()
				if fixture.ackedCount != 2 {
					t.Fatalf("retained admission lost a reliable Pack: ACKs=%d", fixture.ackedCount)
				}
				assertRetainedBudgetBalance(t, budget)
			})
		})
	}
}

func TestRetainedSendFullBudgetStillPermitsNoAckControl(t *testing.T) {
	// Global pool roots need an exclusive process; the protocol clock owns
	// only this fixture, not asynchronous workers left by other tests.
	if messagePoolSnapshotInFreshProcess(t) {
		return
	}
	// Keep the process-global statistics worker outside every synctest bubble.
	MessagePoolReturn(MessagePoolGet(1))
	for _, warm := range []bool{false, true} {
		t.Run(fmt.Sprintf("warm_%t", warm), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				assertMessagePoolOwnership(t)
				budget := NewTransferMemoryBudget(kib(64))
				fixture := newWindowRoundFixture(t, func(settings *SendBufferSettings) {
					settings.ResendQueueBudget = budget
					settings.ResendQueueRetainedByteAccounting = true
				}, nil)
				if warm {
					fixture.forward(fixture.write(100), fixture.receiverIn)
					fixture.acknowledge()
					// Make the immediate caller-side attempt fail once. Its hook
					// opens the same route before the queued fallback is selected.
					for range cap(fixture.senderOut) {
						fixture.senderOut <- MessagePoolGet(1)
					}
					fixture.sender.sendBuffer.afterNoAckFastPathForTest = func(_ sendSequenceId, attempted, written bool, _ time.Duration) {
						if !attempted || written {
							t.Error("warm fallback did not first fail its immediate write")
						}
						for len(fixture.senderOut) > 0 {
							MessagePoolReturn(<-fixture.senderOut)
						}
					}
				}
				if !budget.TryReserve(budget.TotalByteCount()) {
					t.Fatal("sibling could not fill the released budget")
				}
				frame := budgetTestFrame(100)
				admitted, err := fixture.sender.SendWithTimeoutDetailed(frame,
					fixture.receiver.ClientId(), nil, time.Second, NoAck())
				if !admitted {
					MessagePoolReturn(frame.MessageBytes)
					t.Fatalf("full resend budget blocked no-ACK progress: %v", err)
				}
				wire := fixture.take(fixture.senderOut)
				if wire.pack == nil || !wire.pack.Nack || budget.UsedByteCount() != budget.TotalByteCount() {
					t.Fatal("no-ACK packet borrowed reliable retention")
				}
				fixture.forward(wire, fixture.receiverIn)
				budget.Release(budget.TotalByteCount())
				assertRetainedBudgetBalance(t, budget)
			})
		})
	}
}

func TestRetainedColdReliableBacklogDoesNotHideNoAck(t *testing.T) {
	// The process-wide pool worker must start outside the fixture's bubble.
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		budget := NewTransferMemoryBudget(kib(64))
		budget.TryReserve(budget.TotalByteCount())
		fixture := newWindowRoundFixture(t, nil, nil)
		settings := DefaultSendBufferSettings()
		settings.SequenceBufferSize = 4
		settings.ResendQueueBudget = budget
		settings.ResendQueueRetainedByteAccounting = true
		seq := NewSendSequence(fixture.ctx, fixture.sender, fixture.sender.sendBuffer,
			fixture.receiver.ClientId(), MultiHopId{}, false, false, false,
			sequenceTlsRoleClient, false, settings)
		// Both arrive before the cold worker publishes its capacity flag. They
		// share one flow key, so ordinary flow-head scheduling cannot bypass it.
		for _, ack := range []bool{true, false} {
			frame := budgetTestFrame(100)
			pack := &SendPack{TransferOptions: TransferOptions{Ack: ack}, Frame: frame,
				Ctx: fixture.ctx, Destination: fixture.receiver.ClientId(), MessageByteCount: ByteCount(len(frame.MessageBytes))}
			if admitted, err := seq.Pack(pack, 0); !admitted || err != nil {
				pack.returnFrames()
				t.Fatalf("cold fixture admission: %t %v", admitted, err)
			}
		}
		done := make(chan struct{})
		go func() { defer close(done); seq.Run() }()
		wire := fixture.take(fixture.senderOut)
		if wire.pack == nil || !wire.pack.Nack || budget.UsedByteCount() != budget.TotalByteCount() {
			t.Fatal("reliable backlog hid unordered control or borrowed retention")
		}
		fixture.forward(wire, fixture.receiverIn)
		seq.Close()
		<-done
		budget.Release(budget.TotalByteCount())
		assertRetainedBudgetBalance(t, budget)
	})
}

func TestRetainedHeadRewriteAccountsLegacyAndMultiFrameScratch(t *testing.T) {
	for _, version := range []int{1, 2} {
		for _, frameCount := range []int{1, 8} {
			for _, payloadSize := range []int{1900, 3900} {
				t.Run(fmt.Sprintf("v%d_frames%d_payload%d", version, frameCount, payloadSize), func(t *testing.T) {
					assertMessagePoolOwnership(t)
					budget := NewTransferMemoryBudget(mib(1))
					settings := DefaultSendBufferSettings()
					settings.ProtocolVersion = version
					settings.ResendQueueBudget = budget
					settings.ResendQueueRetainedByteAccounting = true
					seq := NewSendSequence(context.Background(), &Client{log: NewNoopLogger()}, nil,
						NewId(), MultiHopId{}, false, false, false, sequenceTlsRoleClient, false, settings)
					defer seq.Close()
					contractId := NewId()
					contract := &sequenceContract{contractId: contractId,
						contract: &protocol.Contract{StoredContractBytes: make([]byte, 1100)}}
					seq.sendContract = contract
					seq.openSendContracts[contractId] = contract
					frames := make([]*protocol.Frame, frameCount)
					for i := range frames {
						frames[i] = &protocol.Frame{MessageType: protocol.MessageType_TestSimpleMessage, MessageBytes: MessagePoolGet(payloadSize)}
					}
					item := takeSendItem()
					*item = sendItem{transferItem: transferItem{messageId: NewId(), sequenceNumber: 1}, contractId: &contractId, expectsAck: true}
					estimate := seq.retainedSendFrameByteCount(MessageByteCount(frames), frameCount)
					charge := item.retainedMemoryByteCount(estimate, frameCount, version < 2)
					roots := ByteCount(3)
					if version < 2 {
						roots = 5
					}
					if charge < roots*retainedMessageCapacity(estimate)+ByteCount(frameCount)*128 || !item.reserveMemory(budget, charge) {
						t.Fatal("rewrite's encoding/descriptor envelope was not reserved")
					}
					pack := &protocol.Pack{MessageId: item.messageId.Bytes(), SequenceId: seq.sequenceId.Bytes(), SequenceNumber: 1, Frames: frames}
					transferFrame := &protocol.TransferFrame{Pack: pack}
					if version < 2 {
						packBytes, err := ProtoMarshal(pack)
						if err != nil {
							t.Fatal(err)
						}
						transferFrame.Pack = nil
						transferFrame.Frame = &protocol.Frame{MessageType: protocol.MessageType_TransferPack, MessageBytes: packBytes}
					}
					var err error
					item.transferFrameBytes, err = ProtoMarshal(transferFrame)
					if err != nil {
						t.Fatal(err)
					}
					if transferFrame.Frame != nil {
						MessagePoolReturn(transferFrame.Frame.MessageBytes)
					}
					for _, frame := range frames {
						MessagePoolReturn(frame.MessageBytes)
					}
					seq.sendItems = []*sendItem{item}
					seq.resendQueue.Add(item)
					seq.resendQueue.RemoveByMessageId(item.messageId)
					budget.SetTotalByteCount(charge)
					rewritten, fullContract, err := seq.setHead(item, true)
					if err != nil || !fullContract || budget.UsedByteCount() != charge {
						t.Fatalf("full envelope rewrite: contract=%t used=%d/%d err=%v", fullContract, budget.UsedByteCount(), charge, err)
					}
					MessagePoolReturn(item.transferFrameBytes)
					item.transferFrameBytes = rewritten
					seq.releaseRetainedSendItems(context.Canceled)
					assertRetainedBudgetBalance(t, budget)
				})
			}
		}
	}
}

func TestRetainedReceivePoppedContractFailureReturnsOwner(t *testing.T) {
	for _, failure := range []string{"malformed", "missing", "exhausted"} {
		t.Run(failure, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				assertMessagePoolOwnership(t)
				budget := NewTransferMemoryBudget(kib(64))
				fixture := newWindowRoundFixture(t, nil, func(settings *ReceiveBufferSettings) {
					settings.ReceiveQueueBudget = budget
					settings.ReceiveQueueRetainedByteAccounting = true
				})
				first := fixture.write(100)
				wake := &windowRoundFrame{bytes: MessagePoolGet(len(first.bytes)), pack: first.pack}
				copy(wake.bytes, first.bytes)
				fixture.heldFrames = append(fixture.heldFrames, wake)
				fixture.forward(first, fixture.receiverIn)
				fixture.acknowledge()
				fixture.write(100) // keep the gap off the receiver
				fixture.forward(fixture.write(100), fixture.receiverIn)
				seq := fixture.receiveSequence()
				held := seq.receiveQueue.GetBySequenceNumber(2)
				if held == nil || budget.UsedByteCount() == 0 {
					t.Fatal("fixture did not retain the future owner")
				}
				if failure == "malformed" {
					held.contractFrame = &protocol.Frame{MessageBytes: []byte{0xff}}
				} else {
					id := NewId()
					held.contractId = &id
					manager := fixture.receiver.ContractManager()
					manager.mutex.Lock()
					manager.receiveNoContractClientIds[fixture.sender.ClientId()] = false
					manager.mutex.Unlock()
					if failure == "exhausted" {
						seq.openReceiveContracts[id] = &sequenceContract{contractId: id, log: NewNoopLogger()}
					}
				}
				// Quiescence gives the fixture sole access. Advance the delivery
				// point, then wake with a harmless past duplicate so the normal
				// worker pops and validates the held owner in its drain loop.
				seq.nextSequenceNumber = 2
				fixture.forward(wake, fixture.receiverIn)
				select {
				case <-seq.done:
				default:
					t.Fatal("held contract failure did not terminate the worker")
				}
				assertRetainedBudgetBalance(t, budget)
			})
		})
	}
}

func TestRetainedSendHeadRewriteFitsAFullLifetimeEnvelope(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		budget := NewTransferMemoryBudget(kib(64))
		fixture := newWindowRoundFixture(t, func(settings *SendBufferSettings) {
			settings.ResendQueueBudget = budget
			settings.ResendQueueRetainedByteAccounting = true
		}, nil)
		first := fixture.write(1400)
		second := fixture.write(1400)
		fixture.forward(first, fixture.receiverIn)
		fixture.acknowledge()
		seq := fixture.sequence()
		item := seq.resendQueue.GetBySequenceNumber(1)
		if item == nil || item.head {
			t.Fatal("fixture did not retain a non-head retry")
		}
		budget.SetTotalByteCount(budget.UsedByteCount())
		used := budget.UsedByteCount()
		seq.resendQueue.RemoveByMessageId(item.messageId)
		frame, hasContract, err := seq.setHead(item, false)
		if err != nil {
			t.Fatalf("full budget blocked pre-reserved head recovery: %v", err)
		}
		MessagePoolReturn(item.transferFrameBytes)
		item.transferFrameBytes = frame
		item.head = true
		item.hasContractFrame = hasContract
		seq.addResendItem(item)
		if budget.UsedByteCount() != used {
			t.Fatal("head rewrite released/reborrowed lifetime capacity")
		}
		fixture.forward(second, fixture.receiverIn)
		fixture.acknowledge()
		assertRetainedBudgetBalance(t, budget)
	})
}

func TestTransferMemoryBudgetResizeSerializesExactAdmission(t *testing.T) {
	budget := NewTransferMemoryBudget(1)
	// Hold the actual linearization boundary while both public operations
	// arrive. Neither reading an old total nor publishing a shrink may escape
	// this boundary; this fails if either method reverts to separate atomics.
	budget.admissionLock.Lock()
	started := make(chan struct{}, 2)
	admitted := make(chan bool, 1)
	resized := make(chan struct{}, 1)
	go func() {
		started <- struct{}{}
		admitted <- budget.TryReserve(1)
	}()
	go func() {
		started <- struct{}{}
		budget.SetTotalByteCount(0)
		resized <- struct{}{}
	}()
	<-started
	<-started
	select {
	case <-admitted:
		budget.admissionLock.Unlock()
		t.Fatal("exact admission escaped the resize boundary")
	case <-resized:
		budget.admissionLock.Unlock()
		t.Fatal("resize escaped the exact-admission boundary")
	case <-time.After(10 * time.Millisecond):
	}
	budget.admissionLock.Unlock()
	oldOwner := <-admitted
	<-resized
	// Either ordering is legal: an already-admitted owner may drain after a
	// shrink, but every later fan-out attempt sees the new total and fails.
	var escaped atomic.Int32
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if budget.TryReserve(1) {
				escaped.Add(1)
				budget.Release(1)
			}
		}()
	}
	wg.Wait()
	if escaped.Load() != 0 {
		t.Fatalf("fanout admitted %d owners after shrink", escaped.Load())
	}
	if oldOwner {
		budget.Release(1)
	}
	assertRetainedBudgetBalance(t, budget)
}

func TestRetainedReceiveDuplicatePreservesCommitAndHeadProgress(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		budget := NewTransferMemoryBudget(kib(64))
		fixture := newWindowRoundFixture(t, nil, func(settings *ReceiveBufferSettings) {
			settings.ReceiveQueueRetainedByteAccounting = true
			settings.ReceiveQueueBudget = budget
			settings.ReceiveHoldPolicy = ReceiveHoldCommittedPrefix
		})
		fixture.forward(fixture.write(100), fixture.receiverIn)
		fixture.acknowledge()
		head := fixture.write(100)
		tail := fixture.write(100)
		duplicate := &windowRoundFrame{bytes: MessagePoolGet(len(tail.bytes)), pack: tail.pack}
		copy(duplicate.bytes, tail.bytes)
		fixture.heldFrames = append(fixture.heldFrames, duplicate)
		fixture.forward(tail, fixture.receiverIn)
		seq := fixture.receiveSequence()
		held := seq.receiveQueue.GetBySequenceNumber(2)
		if held == nil || !held.committed || budget.UsedByteCount() == 0 {
			t.Fatal("future item was not committed under an exact reservation")
		}
		// Owned predecode must fund the missing original before publishing its
		// successor's SACK. The former Refuse fixture relied on an unpaid
		// direct-head path after it deliberately exhausted physical capacity.
		path, err := FilteredTransferPath(head.bytes)
		if err != nil {
			t.Fatal(err)
		}
		scan, status := scanReceiveMemoryPack(head.bytes, path)
		if status != receiveMemoryPackScanReady || !seq.holdPromise.set || seq.holdPromise.boundary != held.sequenceNumber ||
			seq.holdPromise.reservedByteCount < scan.retainedByteCount ||
			budget.UsedByteCount() != held.QueueByteCount()+seq.holdPromise.reservedByteCount+seq.heldHeadRepair.reservedByteCount {
			t.Fatal("selective custody did not prepay the real missing predecessor and repair metadata")
		}
		budget.SetTotalByteCount(budget.UsedByteCount())
		reserved, _ := budget.Counts()
		fixture.forward(duplicate, fixture.receiverIn)
		afterReserved, _ := budget.Counts()
		if seq.receiveQueue.GetBySequenceNumber(2) != held || reserved != afterReserved {
			t.Fatal("duplicate surrendered and reacquired a committed reservation")
		}
		fixture.forward(head, fixture.receiverIn)
		fixture.acknowledge()
		if fixture.deliveredCount != 3 || fixture.ackedCount != 3 {
			t.Fatalf("full hold blocked ordered-head progress: delivered=%d ACKs=%d", fixture.deliveredCount, fixture.ackedCount)
		}
		assertRetainedBudgetBalance(t, budget)
	})
}

func assertRetainedBudgetBalance(t *testing.T, budget *TransferMemoryBudget) {
	t.Helper()
	reserved, released := budget.Counts()
	if budget.UsedByteCount() != 0 || reserved != released {
		t.Fatalf("retained budget imbalance: used=%d reserved=%d released=%d", budget.UsedByteCount(), reserved, released)
	}
}

// A shared old carrier remains immutable while real v1/v2 full-head rewriting
// spends its original three/five-root grant. The synthetic stored bytes scope
// this to encoding and custody; signed proof validation has separate controls.
func TestRetainedExtendedPoolFullBudgetRewriteKeepsCarrier(t *testing.T) {
	if messagePoolSnapshotInFreshProcess(t) {
		return
	}
	assertMessagePoolOwnership(t)
	for _, version := range []int{1, 2} {
		for _, payloadBytes := range []int{1030, 1280} {
			func() {
				before := encodingPoolBoundaryNow()
				budget := NewTransferMemoryBudget(64 * 1024)
				defer func() {
					assertRetainedBudgetBalance(t, budget)
					encodingPoolRequireBalanced(t, before)
				}()
				settings := DefaultSendBufferSettings()
				settings.ProtocolVersion = version
				settings.ResendQueueBudget = budget
				settings.ResendQueueRetainedByteAccounting = true
				sequence := NewSendSequence(context.Background(), &Client{log: NewNoopLogger()}, nil,
					NewId(), MultiHopId{}, false, false, false, sequenceTlsRoleClient, false, settings)
				defer sequence.Close()
				contractId := NewId()
				contract := &sequenceContract{contractId: contractId,
					contract: &protocol.Contract{StoredContractBytes: make([]byte, 128)}}
				sequence.sendContract = contract
				sequence.openSendContracts[contractId] = contract
				frames, witnesses := transferGroupTestFrames(t, 8, payloadBytes)
				defer func() {
					for _, frame := range frames {
						MessagePoolReturn(frame.MessageBytes)
					}
					releaseTransferGroupTestWitnesses(t, frames, witnesses)
				}()
				item := takeSendItem()
				*item = sendItem{transferItem: transferItem{messageId: NewId(), sequenceNumber: 1}, contractId: &contractId, expectsAck: true}
				sequence.sendItems = []*sendItem{item}
				defer sequence.releaseRetainedSendItems(context.Canceled)
				estimate := sequence.retainedSendFrameByteCount(MessageByteCount(frames), len(frames))
				charge := item.retainedMemoryByteCount(estimate, len(frames), version < 2)
				roots := ByteCount(3)
				if version < 2 {
					roots = 5
				}
				if payloadBytes == 1030 {
					if estimate <= 8192 || estimate > 12288 || retainedMessageCapacity(estimate) != 12300 ||
						charge-item.retainedMemoryByteCount(8192, len(frames), version < 2) != roots*(12300-8204) {
						t.Fatal("extended rewrite did not charge every rounded lifetime root")
					}
				} else if estimate <= 8192 || estimate > 12288 || retainedMessageCapacity(estimate) != 12300 {
					t.Fatal("eight1280 full envelope did not acquire its complete pooled charge")
				}
				budget.SetTotalByteCount(charge - 1)
				if item.reserveMemory(budget, charge) || budget.UsedByteCount() != 0 {
					t.Fatal("underfunded lifetime owner was admitted")
				}
				budget.SetTotalByteCount(charge)
				if !item.reserveMemory(budget, charge) {
					t.Fatal("exact lifetime claim was refused")
				}
				extra := budget.TryReserve(1)
				if extra {
					budget.Release(1)
					t.Fatal("full lifetime claim admitted an unrelated byte")
				}
				pack := &protocol.Pack{MessageId: item.messageId.Bytes(), SequenceId: sequence.sequenceId.Bytes(), SequenceNumber: 1, Frames: frames}
				transferFrame := &protocol.TransferFrame{Pack: pack}
				var legacyPackBytes []byte
				defer func() { MessagePoolReturn(legacyPackBytes) }()
				if version < 2 {
					packBytes, err := ProtoMarshal(pack)
					if err != nil {
						t.Fatal(err)
					}
					legacyPackBytes = packBytes
					transferFrame.Pack = nil
					transferFrame.Frame = &protocol.Frame{MessageType: protocol.MessageType_TransferPack, MessageBytes: packBytes}
				}
				var err error
				item.transferFrameBytes, err = ProtoMarshal(transferFrame)
				if err != nil {
					t.Fatal(err)
				}
				MessagePoolReturn(legacyPackBytes)
				legacyPackBytes = nil
				if transferFrame.Frame != nil {
					transferFrame.Frame.MessageBytes = nil
				}
				carrier := MessagePoolShareReadOnly(item.transferFrameBytes)
				defer MessagePoolReturn(carrier)
				original := append([]byte(nil), carrier...)
				pooled, shared := MessagePoolCheck(carrier)
				if payloadBytes == 1030 && (!pooled || !shared || cap(carrier) != 12300) {
					t.Fatal("full-budget rewrite did not retain a real pooled carrier share")
				}
				if payloadBytes == 1280 && (!pooled || !shared || cap(carrier) != 12300) {
					t.Fatal("eight1280 did not retain a real pooled carrier share")
				}
				sequence.resendQueue.Add(item)
				sequence.resendQueue.RemoveByMessageId(item.messageId)
				rewritten, full, err := sequence.setHead(item, true)
				if err != nil {
					t.Fatal(err)
				}
				if !full || budget.UsedByteCount() != charge || !bytes.Equal(carrier, original) || &rewritten[0] == &carrier[0] {
					MessagePoolReturn(rewritten)
					t.Fatal("full-root rewrite borrowed memory or changed the retained carrier")
				}
				MessagePoolReturn(item.transferFrameBytes)
				item.transferFrameBytes = rewritten
				decoded := &decodedTransferFrame{}
				defer decoded.release()
				if !unmarshalOwnedTransferFrame(rewritten, decoded, false) {
					t.Fatal("rewritten full head did not decode")
				}
				var actual *protocol.Pack
				if decoded.frame.Pack != nil {
					actual = decoded.frame.Pack
				} else if decoded.frame.Frame != nil {
					actual = &protocol.Pack{}
					if err := ProtoUnmarshal(decoded.frame.Frame.MessageBytes, actual); err != nil {
						t.Fatal(err)
					}
					defer returnDecodedPackMessageBytes(actual)
				}
				if actual == nil || !actual.Head || actual.ContractFrame == nil || len(actual.Frames) != 8 {
					t.Fatal("rewritten full head lost contract or application frames")
				}
				t.Logf("version=%d packet_bytes=%d estimate=%d retained_charge=%d old_wire=%d rewritten_wire=%d pooled_old=%t", version, payloadBytes, estimate, charge, len(carrier), len(rewritten), pooled)
			}()
		}
	}
}

// The real legacy protobuf decoder allocates ordinary message bytes. Its
// cleanup must detach them without treating normal allocator cap as a pool id.
func TestRetainedExtendedPoolLegacyDecodeDoesNotAdoptHeapBytes(t *testing.T) {
	if messagePoolSnapshotInFreshProcess(t) {
		return
	}
	before := encodingPoolBoundaryNow()
	t.Cleanup(func() { encodingPoolRequireBalanced(t, before) })
	for _, size := range []int{9472, 9000} {
		func() {
			want := bytes.Repeat([]byte{0x5a}, size)
			encoded, err := ProtoMarshal(&protocol.Frame{MessageType: protocol.MessageType_TestSimpleMessage, MessageBytes: want})
			if err != nil {
				t.Fatal(err)
			}
			defer MessagePoolReturn(encoded)
			decoded := &protocol.Frame{}
			if err := ProtoUnmarshal(encoded, decoded); err != nil {
				t.Fatal(err)
			}
			ordinary := decoded.MessageBytes
			if cap(ordinary) != 9472 {
				t.Fatalf("legacy decoder premise changed: size=%d cap=%d", size, cap(ordinary))
			}
			pooled, shared := MessagePoolCheck(ordinary)
			returnDecodedFrameMessageBytes(decoded)
			if pooled || shared || decoded.MessageBytes != nil || !bytes.Equal(ordinary, want) {
				t.Fatalf("NONPOOL_PROTOBUF_CAPACITY_COLLISION: size=%d cap=%d pooled=%t mutated=%t", size, cap(ordinary), pooled, !bytes.Equal(ordinary, want))
			}
		}()
	}
}
