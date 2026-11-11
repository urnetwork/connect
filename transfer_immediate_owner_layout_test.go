package connect

import (
	"context"
	"math"
	"sync"
	"testing"
	"testing/synctest"
	"time"
	"unsafe"
)

// Go's pointer-bearing allocation adds an eight-byte header to this owner.
// Its physical envelope, including that header, must keep the existing charge.
func TestImmediatePackedOwnerKeepsExistingPhysicalEnvelope(t *testing.T) {
	if size := unsafe.Sizeof(decodedPackOwner{}); size+8 > uintptr(decodedPackOwnerQueueByteCount) || decodedPackOwnerQueueByteCount != 1024 {
		t.Fatalf("packed owner=%d plus header8 exceeds unchanged physical charge%d", size, decodedPackOwnerQueueByteCount)
	}
}

func TestImmediatePackedReceiveTagKeepsOptionalZeroAndMaximum(t *testing.T) {
	for _, expected := range []sequenceTag{{}, {set: true}, {set: true, sendTime: math.MaxUint64}} {
		item := receiveItem{tagSendTime: expected.sendTime, tagSet: expected.set}
		if got := item.receiveTag(); got != expected {
			t.Fatalf("packed tag lost exact presence/value: got=%+v expected=%+v", got, expected)
		}
	}
}

// Both real receive reconstruction paths preserve the pool's live marker.
// After actual cancellation/join, inspect only objects owned by the free list,
// then reacquire a previously delivered object through the real pool reset.
func TestImmediatePackedOwnerKeepsLiveFlagThroughReceiveCancelAndReuse(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		var mu sync.Mutex
		owners := map[*decodedPackOwner]bool{}
		armed := false
		expectedTag := sequenceTag{}
		expectedAck := true
		observed := 0
		fixture := newWindowRoundFixture(t, nil, func(settings *ReceiveBufferSettings) {
			settings.receiveOwnerStageForTest = func(_ *ReceiveSequence, owner *decodedPackOwner, stage string) {
				if stage != "head_buffered" {
					return
				}
				mu.Lock()
				defer mu.Unlock()
				if !armed {
					return
				}
				if owner == nil || !owner.inUse || owner.receiveItem.receiveTag() != expectedTag || owner.receiveItem.ack != expectedAck {
					t.Errorf("actual receive rebuild lost pool/tag state: owner=%p ack=%t", owner, expectedAck)
					return
				}
				owners[owner] = true
				observed++
			}
		}, TransportTypeH1)
		warm := fixture.write(16)
		fixture.forward(warm, fixture.receiverIn)
		fixture.acknowledge()
		synctest.Wait()
		const count = 16
		for index := 0; index < count; index++ {
			ack := index%2 == 0
			var wire *windowRoundFrame
			if ack {
				wire = fixture.write(16)
			} else {
				frame := budgetTestFrame(16)
				accepted, err := fixture.sender.SendWithTimeoutDetailed(frame, fixture.receiver.ClientId(), func(error) {}, time.Second, NoAck())
				if !accepted {
					MessagePoolReturn(frame.MessageBytes)
				}
				if !accepted || err != nil {
					t.Fatalf("actual NoAck receive source refused: accepted=%t error=%v", accepted, err)
				}
				wire = fixture.take(fixture.senderOut)
			}
			if wire.pack == nil || wire.pack.Nack == ack {
				t.Fatal("actual source did not exercise both reliable and NoAck reconstruction")
			}
			mu.Lock()
			armed, expectedTag, expectedAck = true, sequenceTagFromProtocol(wire.pack.Tag), ack
			mu.Unlock()
			fixture.forward(wire, fixture.receiverIn)
			if ack {
				fixture.acknowledge()
			}
			synctest.Wait()
		}
		mu.Lock()
		if observed != count {
			t.Errorf("observed%d actual rebuilds, expected%d", observed, count)
		}
		mu.Unlock()
		fixture.cancel()
		joined, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, client := range []*Client{fixture.sender, fixture.receiver} {
			if err := client.CloseAndWait(joined); err != nil {
				t.Fatal(err)
			}
		}
		synctest.Wait()
		freeObserved := 0
		for index := range inboundDecodedPackOwners.shards {
			shard := &inboundDecodedPackOwners.shards[index]
			shard.mu.Lock()
			for _, owner := range shard.free {
				if owners[owner] {
					freeObserved++
					if owner.inUse || owner.receiveItem.tagSet || owner.receiveItem.tagSendTime != 0 || owner.receiveItem.immediateReceipt != nil {
						t.Error("actual returned owner retained live/tag/receipt state")
					}
				}
			}
			shard.mu.Unlock()
		}
		if freeObserved == 0 {
			t.Fatal("no delivered owner reached the actual bounded free list")
		}
		var acquired []*decodedPackOwner
		defer func() {
			for _, owner := range acquired {
				owner.release()
			}
		}()
		reused := false
		for len(acquired) <= decodedPackOwnerPoolCapacity {
			owner := inboundDecodedPackOwners.take()
			acquired = append(acquired, owner)
			if !owner.inUse || owner.receiveItem.tagSet || owner.receiveItem.tagSendTime != 0 || owner.receiveItem.immediateReceipt != nil {
				t.Fatal("actual pool reset lost its live marker or preserved old receive state")
			}
			if owners[owner] {
				reused = true
				break
			}
		}
		if !reused {
			t.Fatal("bounded pool did not reacquire a previously delivered owner")
		}
	})
}
