// TCP flows of one NAT share its return retention budget. Bytes an inner
// acknowledgement frees belong to the oldest waiting socket reader, so the
// runtime's wake order cannot decide whose origin reads reach Transfer.
package connect

import (
	"context"
	"math"
	"runtime"
	"sync"
	"testing"
	"testing/synctest"
	"unsafe"
)

// One retained origin chunk: the flow that retained it and the inner
// acknowledgement that releases it.
type tcpReturnRetainedChunk struct {
	flow int
	end  uint32
}

// The root cause of one TCP flow keeping every byte of the shared return
// retention budget. Four socket readers retain chunk after chunk, as flows
// with origin data ready do; the first fills the budget and the other three
// wait behind it. Each flow may retain more than the whole budget, so every
// reader waits on the budget itself. The test, standing in for each flow's
// inner TCP, acknowledges the oldest retained chunk one at a time. Each flow
// must get an equal share of the freed bytes. The budget used to wake every
// waiting reader and give the bytes to whichever reserved first: on one
// processor that is the reader that waited first, which retained, read on,
// and waited again ahead of the readers it beat, so the first flow took every
// freed chunk and the other three got none.
func TestTcpReturnRetentionPassesFreedBytesToOldestWaitingFlow(t *testing.T) {
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		payload := make([]byte, 1400)
		charge := 128 + retainedMessageCapacity(ByteCount(len(payload)))
		const flowCount = 4
		const budgetChunkCount = 4
		const ackCount = 8 * flowCount
		budget := NewTransferMemoryBudget(budgetChunkCount * charge)
		settings := &TcpBufferSettings{ReturnQueueBudget: budget, ReturnQueueMaxByteCount: 2 * budget.TotalByteCount()}
		retained := make(chan tcpReturnRetainedChunk, budgetChunkCount+flowCount)
		flows := make([]*TcpSequence, flowCount)
		var readers sync.WaitGroup
		// Readers start, and so first wait, in index order. The first fills
		// the budget before the rest start.
		for index := range flowCount {
			flowCtx, flowCancel := context.WithCancel(ctx)
			flow := &TcpSequence{ctx: flowCtx, cancel: flowCancel, tcpBufferSettings: settings,
				returnWake: make(chan struct{}, 1), returnCapacity: make(chan struct{}, 1),
			}
			flows[index] = flow
			readers.Go(func() {
				for {
					// The socket reader assigns sequence space before retention.
					flow.mutex.Lock()
					start := flow.receiveSeq
					flow.receiveSeq += uint32(len(payload))
					flow.mutex.Unlock()
					if !flow.retainReturnChunk(payload, start, false) {
						return
					}
					retained <- tcpReturnRetainedChunk{flow: index, end: start + uint32(len(payload))}
				}
			})
			synctest.Wait()
		}

		var inFlight []tcpReturnRetainedChunk
		take := func() []tcpReturnRetainedChunk {
			count := len(inFlight)
			for len(retained) != 0 {
				inFlight = append(inFlight, <-retained)
			}
			return inFlight[count:]
		}
		if first := take(); len(first) != budgetChunkCount || first[budgetChunkCount-1].flow != 0 {
			t.Fatalf("first flow retained %v before the others started, want %d chunks", first, budgetChunkCount)
		}
		var order []int
		retainedCounts := make([]int, flowCount)
		for range ackCount {
			oldest := inFlight[0]
			inFlight = inFlight[1:]
			flow := flows[oldest.flow]
			flow.mutex.Lock()
			flow.applySendAckWithLock(&parsedTcp{ack: true, ackNumber: oldest.end, windowSize: math.MaxUint16})
			flow.mutex.Unlock()
			// The freed chunk's bytes admit whoever is next, which retains and
			// queues its next chunk.
			synctest.Wait()
			for _, chunk := range take() {
				order = append(order, chunk.flow)
				retainedCounts[chunk.flow] += 1
			}
		}
		for _, flow := range flows {
			flow.cancel()
		}
		readers.Wait()
		for _, flow := range flows {
			flow.releaseReturnChunks()
		}
		assertRetainedBudgetBalance(t, budget)

		t.Logf("retention order after the first flow filled the budget: %v", order)
		for index, count := range retainedCounts {
			if count != ackCount/flowCount {
				t.Errorf("flow %d retained %d of %d freed chunks, want %d each: %v", index, count, ackCount, ackCount/flowCount, retainedCounts)
			}
		}
	})
}

// A reader that queues to grow its chunk metadata keeps that plan when the
// grant arrives, even though its own acknowledgements compacted the array
// meanwhile, so the bytes reserved for it are exactly the bytes it charges. A
// sibling flow holds part of the NAT budget, so the growing flow waits on the
// budget rather than on its own per-flow limit. Its fifth chunk must grow the
// array, queues, and is granted by the release of its first two chunks.
func TestTcpReturnRetentionGrantKeepsItsQueuedMetadataPlan(t *testing.T) {
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		payload := make([]byte, 1400)
		charge := 128 + retainedMessageCapacity(ByteCount(len(payload)))
		chunkMetadata := ByteCount(unsafe.Sizeof(tcpReturnChunk{}))
		budget := NewTransferMemoryBudget(5*charge + 8*chunkMetadata)
		settings := &TcpBufferSettings{MemoryBudget: budget}
		newFlow := func() *TcpSequence {
			flowCtx, flowCancel := context.WithCancel(ctx)
			return &TcpSequence{ctx: flowCtx, cancel: flowCancel, tcpBufferSettings: settings,
				returnWake: make(chan struct{}, 1), returnCapacity: make(chan struct{}, 1),
			}
		}
		sibling, flow := newFlow(), newFlow()
		retain := func(flow *TcpSequence) <-chan bool {
			flow.mutex.Lock()
			start := flow.receiveSeq
			flow.receiveSeq += uint32(len(payload))
			flow.mutex.Unlock()
			result := make(chan bool, 1)
			go func() { result <- flow.retainReturnChunk(payload, start, false) }()
			synctest.Wait()
			return result
		}
		defer func() {
			sibling.cancel()
			flow.cancel()
			synctest.Wait()
			sibling.releaseReturnChunks()
			flow.releaseReturnChunks()
			assertRetainedBudgetBalance(t, budget)
		}()
		if !<-retain(sibling) {
			t.Fatal("the sibling's chunk did not fit the budget")
		}
		for index := range 4 {
			if !<-retain(flow) {
				t.Fatalf("chunk %d did not fit the budget", index)
			}
		}
		if available := budget.Available(); available != 0 {
			t.Fatalf("five chunks and two metadata arrays left %d bytes free, want none", available)
		}
		growing := retain(flow)
		select {
		case <-growing:
			t.Fatal("the chunk that grows the metadata array was retained in a full budget")
		default:
		}

		// Releasing two chunks frees room for the growth plan, and compacts
		// the array so the flow would no longer need to grow it.
		flow.mutex.Lock()
		flow.applySendAckWithLock(&parsedTcp{ack: true, ackNumber: 2 * uint32(len(payload)), windowSize: math.MaxUint16})
		flow.mutex.Unlock()
		synctest.Wait()
		select {
		case retained := <-growing:
			if !retained {
				t.Fatal("the granted chunk was refused")
			}
		default:
			t.Fatal("the release did not admit the waiting chunk")
		}
		flow.mutex.Lock()
		returnByteCount, metadataByteCount := flow.returnByteCount, flow.returnMetadataByteCount
		flow.mutex.Unlock()
		if returnByteCount != 3*charge || metadataByteCount != 8*chunkMetadata {
			t.Fatalf("flow charges %d chunk bytes and %d metadata bytes, want %d and %d", returnByteCount, metadataByteCount, 3*charge, 8*chunkMetadata)
		}
		if used, charged := budget.UsedByteCount(), returnByteCount+metadataByteCount+charge+4*chunkMetadata; used != charged {
			t.Fatalf("budget holds %d bytes for flows charging %d", used, charged)
		}
	})
}
