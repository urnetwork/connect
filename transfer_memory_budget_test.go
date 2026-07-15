package connect

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// essential test 1a: budget accounting semantics
func TestTransferMemoryBudgetAccounting(t *testing.T) {
	budget := NewTransferMemoryBudget(kib(64))

	AssertEqual(t, budget.TotalByteCount(), kib(64))
	AssertEqual(t, budget.Available(), kib(64))
	AssertEqual(t, budget.UsedByteCount(), ByteCount(0))

	budget.Reserve(kib(16))
	AssertEqual(t, budget.Available(), kib(48))
	AssertEqual(t, budget.UsedByteCount(), kib(16))

	// reserve always succeeds; available floors at zero on overdraft
	budget.Reserve(kib(64))
	AssertEqual(t, budget.Available(), ByteCount(0))
	AssertEqual(t, budget.UsedByteCount(), kib(80))

	budget.Release(kib(80))
	AssertEqual(t, budget.Available(), kib(64))
	AssertEqual(t, budget.UsedByteCount(), ByteCount(0))

	reserved, released := budget.Counts()
	AssertEqual(t, reserved, kib(80))
	AssertEqual(t, released, kib(80))
}

// essential test 1a: concurrent reserve/release keeps exact balance (run
// under -race in the suite)
func TestTransferMemoryBudgetConcurrent(t *testing.T) {
	budget := NewTransferMemoryBudget(mib(1))

	var wg sync.WaitGroup
	for g := 0; g < 8; g += 1 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 10000; i += 1 {
				budget.Reserve(1000)
				budget.Available()
				budget.Release(1000)
			}
		}()
	}
	wg.Wait()

	AssertEqual(t, budget.UsedByteCount(), ByteCount(0))
	reserved, released := budget.Counts()
	AssertEqual(t, reserved, ByteCount(8*10000*1000))
	AssertEqual(t, reserved, released)
}

// essential test 1b: the queue maintains borrowed = max(0, byteCount-floor)
// across add, remove, and clear, and CanAdd honors the floor, the max, and
// the budget headroom
func TestTransferQueueBudgetBorrow(t *testing.T) {
	budget := NewTransferMemoryBudget(kib(8))
	queue := newTransferQueue[*transferItem](func(a *transferItem, b *transferItem) int {
		if a.sequenceNumber < b.sequenceNumber {
			return -1
		} else if b.sequenceNumber < a.sequenceNumber {
			return 1
		}
		return 0
	})
	queue.setBudget(budget, kib(4))

	newItem := func(sequenceNumber uint64, byteCount ByteCount) *transferItem {
		return &transferItem{
			messageId:        NewId(),
			messageByteCount: byteCount,
			sequenceNumber:   sequenceNumber,
		}
	}

	// below the floor there is no borrowing
	a := newItem(1, kib(2))
	queue.Add(a)
	AssertEqual(t, budget.UsedByteCount(), ByteCount(0))
	// crossing the floor borrows the excess
	b := newItem(2, kib(4))
	queue.Add(b)
	AssertEqual(t, budget.UsedByteCount(), kib(2))
	// removal below the floor releases everything borrowed
	queue.RemoveByMessageId(b.messageId)
	AssertEqual(t, budget.UsedByteCount(), ByteCount(0))

	// CanAdd: under floor always; above floor requires headroom under max
	AssertEqual(t, queue.CanAdd(kib(1), kib(64)), true)
	// would exceed max
	AssertEqual(t, queue.CanAdd(kib(64), kib(4)), false)
	// zero byte count probe: at the floor, requires headroom
	queue.Add(newItem(3, kib(2)))
	AssertEqual(t, queue.CanAdd(0, kib(64)), true)
	// exhaust the budget elsewhere: above-floor growth is refused, and a
	// same-size add below max but above floor is refused too
	budget.Reserve(kib(8))
	AssertEqual(t, queue.CanAdd(0, kib(64)), false)
	AssertEqual(t, queue.CanAdd(kib(2), kib(64)), false)
	budget.Release(kib(8))
	AssertEqual(t, queue.CanAdd(0, kib(64)), true)

	// grow above the floor, then Clear releases all borrowed bytes
	// (2 + 2 + 4 + 2 KiB queued - 4 KiB floor = 6 KiB borrowed)
	queue.Add(newItem(4, kib(4)))
	queue.Add(newItem(5, kib(2)))
	AssertEqual(t, budget.UsedByteCount(), kib(6))
	items := queue.Clear()
	AssertEqual(t, len(items), 4)
	AssertEqual(t, budget.UsedByteCount(), ByteCount(0))
	AssertEqual(t, queue.Len(), 0)
	_, queueByteCount := queue.QueueSize()
	AssertEqual(t, queueByteCount, ByteCount(0))

	reserved, released := budget.Counts()
	AssertEqual(t, reserved, released)
}

// budgetTestPeer wires a sender client to one receiver client over direct
// channel routes, optionally without the ack return path so the sender's
// resend queue holds every sent message (deterministic queue depth).
type budgetTestPeer struct {
	receiverClient *Client
	unsub          func()
}

func newBudgetTestSender(ctx context.Context, sendBudget *TransferMemoryBudget, minByteCount ByteCount, maxByteCount ByteCount) *Client {
	clientSettings := DefaultClientSettings()
	// unbuffered pack channel: Send admission mirrors queue admission
	clientSettings.SendBufferSettings.SequenceBufferSize = 0
	clientSettings.SendBufferSettings.AckBufferSize = 0
	clientSettings.SendBufferSettings.AckTimeout = 300 * time.Second
	clientSettings.SendBufferSettings.IdleTimeout = 300 * time.Second
	clientSettings.SendBufferSettings.ResendQueueMinByteCount = minByteCount
	clientSettings.SendBufferSettings.ResendQueueMaxByteCount = maxByteCount
	clientSettings.SendBufferSettings.ResendQueueBudget = sendBudget
	// keep resends quiet during the withheld-ack tests
	clientSettings.SendBufferSettings.MinResendInterval = 300 * time.Second
	clientSettings.SendBufferSettings.MaxResendInterval = 300 * time.Second
	// plaintext, so the one-way (withheld ack) wirings never depend on a
	// handshake round trip
	clientSettings.EncryptionSettings.Encrypt = false
	return NewClient(ctx, NewId(), NewNoContractClientOob(), clientSettings)
}

// attachBudgetTestPeer wires sender->receiver routes. withAcks wires the
// return path so the receiver's acks drain the sender's resend queue.
func attachBudgetTestPeer(ctx context.Context, sender *Client, withAcks bool, receiveCallback ReceiveFunction) *budgetTestPeer {
	receiverSettings := DefaultClientSettings()
	receiverSettings.EncryptionSettings.Encrypt = false
	receiverClient := NewClient(ctx, NewId(), NewNoContractClientOob(), receiverSettings)

	forwardRoute := make(chan []byte)
	sender.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(receiverClient.ClientId())), []Route{forwardRoute})
	receiverClient.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{forwardRoute})

	if withAcks {
		returnRoute := make(chan []byte)
		receiverClient.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(sender.ClientId())), []Route{returnRoute})
		sender.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{returnRoute})
	}

	sender.ContractManager().AddNoContractPeer(receiverClient.ClientId())
	receiverClient.ContractManager().AddNoContractPeer(sender.ClientId())

	var unsub func()
	if receiveCallback != nil {
		unsub = receiverClient.AddReceiveCallback(receiveCallback)
	}

	return &budgetTestPeer{
		receiverClient: receiverClient,
		unsub:          unsub,
	}
}

func budgetTestFrame(payloadByteCount int) *protocol.Frame {
	message := &protocol.SimpleMessage{
		Content: strings.Repeat("x", payloadByteCount),
	}
	return RequireToFrameWithDefaultProtocolVersion(message)
}

// settleBudgetUsed polls until the budget used count is stable (sequences
// unwind asynchronously after cancel)
func settleBudgetUsed(budget *TransferMemoryBudget) ByteCount {
	prev := budget.UsedByteCount()
	stableCount := 0
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		n := budget.UsedByteCount()
		if n == prev {
			stableCount += 1
			if 4 <= stableCount && n == 0 {
				break
			}
			if 8 <= stableCount {
				break
			}
		} else {
			stableCount = 0
			prev = n
		}
	}
	return prev
}

// fillBudgetTestQueue sends messages with a short timeout until the resend
// queue refuses (acks are withheld, so refusal means queue admission paused).
// returns the number of accepted messages.
func fillBudgetTestQueue(sender *Client, destinationId Id, payloadByteCount int, maxMessages int) int {
	accepted := 0
	for i := 0; i < maxMessages; i += 1 {
		success := sender.SendWithTimeout(
			budgetTestFrame(payloadByteCount),
			DestinationId(destinationId),
			func(err error) {},
			500*time.Millisecond,
		)
		if !success {
			break
		}
		accepted += 1
	}
	return accepted
}

// essential test 3 (parity): with the shared budget larger than the
// per-sequence cap, a single sequence reaches the same depth as with no
// budget at all — one fast peer still gets its full queue
func TestTransferBudgetSingleSequenceParity(t *testing.T) {
	const payloadByteCount = 4 * 1024
	const maxMessages = 200
	minByteCount := kib(16)
	maxByteCount := kib(256)

	run := func(sendBudget *TransferMemoryBudget) (int, ByteCount) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		sender := newBudgetTestSender(ctx, sendBudget, minByteCount, maxByteCount)
		defer sender.Cancel()
		peer := attachBudgetTestPeer(ctx, sender, false, nil)
		defer peer.receiverClient.Cancel()

		accepted := fillBudgetTestQueue(sender, peer.receiverClient.ClientId(), payloadByteCount, maxMessages)
		// sample the live borrow before the deferred teardown releases it
		usedAtFill := ByteCount(0)
		if sendBudget != nil {
			usedAtFill = sendBudget.UsedByteCount()
		}
		return accepted, usedAtFill
	}

	// pool larger than the cap: the cap binds, exactly like nil budget
	budget := NewTransferMemoryBudget(kib(512))
	acceptedWithBudget, usedAtFill := run(budget)
	acceptedNil, _ := run(nil)

	t.Logf("accepted with budget=%d nil=%d usedAtFill=%d", acceptedWithBudget, acceptedNil, usedAtFill)
	if acceptedWithBudget < acceptedNil-2 || acceptedNil+2 < acceptedWithBudget {
		t.Errorf("single sequence depth changed under a roomy budget: %d with vs %d without", acceptedWithBudget, acceptedNil)
	}
	// the sequence borrowed well above the floor
	if usedAtFill < maxByteCount-minByteCount-2*payloadByteCount {
		t.Errorf("expected deep borrow, used at fill = %d", usedAtFill)
	}

	used := settleBudgetUsed(budget)
	AssertEqual(t, used, ByteCount(0))
	reserved, released := budget.Counts()
	AssertEqual(t, reserved, released)
	if reserved == 0 {
		t.Errorf("expected borrowing to have happened")
	}
}

// essential test 4 (ceiling): with many sequences and a small shared pool,
// the aggregate borrow never exceeds the pool (plus the documented one
// message per sequence overdraft), and every sequence still gets its floor
func TestTransferBudgetAggregateCeiling(t *testing.T) {
	const peerCount = 6
	const payloadByteCount = 4 * 1024
	const maxMessages = 200
	minByteCount := kib(8)
	maxByteCount := kib(512)
	totalByteCount := kib(64)
	// each admission that saw headroom can overshoot by about one framed
	// message per sequence
	slopByteCount := ByteCount(peerCount * (payloadByteCount + 2048))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	budget := NewTransferMemoryBudget(totalByteCount)
	sender := newBudgetTestSender(ctx, budget, minByteCount, maxByteCount)
	defer sender.Cancel()

	peers := []*budgetTestPeer{}
	for i := 0; i < peerCount; i += 1 {
		peer := attachBudgetTestPeer(ctx, sender, false, nil)
		defer peer.receiverClient.Cancel()
		peers = append(peers, peer)
	}

	// sample the peak budget usage while the queues fill
	var maxUsed atomic.Int64
	samplerDone := make(chan struct{})
	go func() {
		defer close(samplerDone)
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Millisecond):
			}
			used := int64(budget.UsedByteCount())
			for {
				prev := maxUsed.Load()
				if used <= prev || maxUsed.CompareAndSwap(prev, used) {
					break
				}
			}
		}
	}()

	// fill every sequence concurrently until each refuses
	var wg sync.WaitGroup
	acceptedByteCounts := make([]int64, peerCount)
	for i := 0; i < peerCount; i += 1 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			accepted := fillBudgetTestQueue(sender, peers[i].receiverClient.ClientId(), payloadByteCount, maxMessages)
			acceptedByteCounts[i] = int64(accepted) * payloadByteCount
		}(i)
	}
	wg.Wait()

	totalAccepted := int64(0)
	for i, acceptedByteCount := range acceptedByteCounts {
		t.Logf("sequence %d accepted %d bytes", i, acceptedByteCount)
		// every sequence progressed on at least its floor
		if acceptedByteCount < int64(minByteCount) {
			t.Errorf("sequence %d starved below its floor: %d < %d", i, acceptedByteCount, minByteCount)
		}
		totalAccepted += acceptedByteCount
	}

	// the pool saturated (this test is about the ceiling binding)
	if int64(totalByteCount)/2 > maxUsed.Load() {
		t.Errorf("expected the pool to saturate, peak used = %d of %d", maxUsed.Load(), totalByteCount)
	}
	// the ceiling held: peak borrow within total + one message per sequence
	if int64(totalByteCount+slopByteCount) < maxUsed.Load() {
		t.Errorf("budget ceiling exceeded: peak used %d > total %d + slop %d", maxUsed.Load(), totalByteCount, slopByteCount)
	}
	// the aggregate is flat: floors + pool + slop, far below peerCount x max
	aggregateCeiling := int64(peerCount)*int64(minByteCount) + int64(totalByteCount) + int64(slopByteCount) + int64(peerCount)*2048
	if aggregateCeiling < totalAccepted {
		t.Errorf("aggregate accepted %d exceeds the flat ceiling %d", totalAccepted, aggregateCeiling)
	}

	cancel()
	<-samplerDone
	used := settleBudgetUsed(budget)
	AssertEqual(t, used, ByteCount(0))
	reserved, released := budget.Counts()
	AssertEqual(t, reserved, released)
}

// essential test 2 (liveness): many sequences over a pool much smaller than
// their demand, with acks flowing — every message is eventually delivered
// and acked, so an empty pool can never deadlock the sequences (run under
// -race in the suite)
func TestTransferBudgetLiveness(t *testing.T) {
	const peerCount = 6
	const messagesPerPeer = 40
	const payloadByteCount = 2 * 1024

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// pool far below demand: peers mostly run at their floors
	budget := NewTransferMemoryBudget(kib(16))
	sender := newBudgetTestSender(ctx, budget, kib(4), kib(256))
	defer sender.Cancel()

	var receiveCount atomic.Int64
	receiveNotify := make(chan struct{}, peerCount*messagesPerPeer)
	peers := []*budgetTestPeer{}
	for i := 0; i < peerCount; i += 1 {
		peer := attachBudgetTestPeer(ctx, sender, true, func(source TransferPath, frames []*protocol.Frame, peer Peer) {
			receiveCount.Add(int64(len(frames)))
			select {
			case receiveNotify <- struct{}{}:
			default:
			}
		})
		defer peer.receiverClient.Cancel()
		peers = append(peers, peer)
	}

	var ackCount atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < peerCount; i += 1 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < messagesPerPeer; j += 1 {
				success := sender.SendWithTimeout(
					budgetTestFrame(payloadByteCount),
					DestinationId(peers[i].receiverClient.ClientId()),
					func(err error) {
						if err == nil {
							ackCount.Add(1)
						}
					},
					// block until the sequence accepts (liveness under test)
					-1,
				)
				if !success {
					t.Errorf("send %d/%d refused", i, j)
					return
				}
			}
		}(i)
	}
	wg.Wait()

	// every message delivers and acks despite the exhausted pool
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if int64(peerCount*messagesPerPeer) <= ackCount.Load() {
			break
		}
		select {
		case <-receiveNotify:
		case <-time.After(100 * time.Millisecond):
		}
	}
	AssertEqual(t, ackCount.Load(), int64(peerCount*messagesPerPeer))
	AssertEqual(t, receiveCount.Load(), int64(peerCount*messagesPerPeer))

	cancel()
	used := settleBudgetUsed(budget)
	AssertEqual(t, used, ByteCount(0))
	reserved, released := budget.Counts()
	AssertEqual(t, reserved, released)
}

// essential test 1c (churn balance): repeated build/load/teardown cycles
// against one shared budget pair return every borrowed byte — teardown with
// non-empty queues exercises the wholesale Clear release path
func TestTransferBudgetChurnBalance(t *testing.T) {
	sendBudget := NewTransferMemoryBudget(kib(64))

	for cycle := 0; cycle < 4; cycle += 1 {
		func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			sender := newBudgetTestSender(ctx, sendBudget, kib(8), kib(256))
			peer := attachBudgetTestPeer(ctx, sender, false, nil)

			// fill the queue deep (no acks), then tear down with the queue
			// non-empty so Clear must release the borrow
			accepted := fillBudgetTestQueue(sender, peer.receiverClient.ClientId(), 4*1024, 50)
			if accepted == 0 {
				t.Errorf("cycle %d accepted nothing", cycle)
			}
			if sendBudget.UsedByteCount() == 0 {
				t.Errorf("cycle %d expected a live borrow before teardown", cycle)
			}

			sender.Cancel()
			peer.receiverClient.Cancel()

			used := settleBudgetUsed(sendBudget)
			if used != 0 {
				reserved, released := sendBudget.Counts()
				t.Fatalf("cycle %d leaked budget: used=%d reserved=%d released=%d", cycle, used, reserved, released)
			}
		}()
	}

	reserved, released := sendBudget.Counts()
	AssertEqual(t, reserved, released)
	if reserved == 0 {
		t.Errorf("expected borrowing across the churn cycles")
	}
	fmt.Printf("churn balance: reserved=released=%d across 4 cycles\n", reserved)
}
