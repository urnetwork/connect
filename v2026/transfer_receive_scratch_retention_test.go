package connect

import (
	"runtime"
	"testing"
	"testing/synctest"
	"unsafe"
	"weak"
)

// A live sequence must stop owning the decoded packets it has delivered.
// Real reordered wire Packs populate the committed-prefix scratch; the gap
// then arrives and real cumulative ACKs retire every sender owner. The bounded
// global decode free-list may keep 256 owners, but a scratch high-water mark
// must not keep the other released owners until this sequence eventually dies.
func TestReceiveCommittedScratchReleasesDeliveredOwners(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, partial := range []bool{false, true} {
		name := "whole_hold"
		if partial {
			name = "partial_prefix"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const heldCount = 1024
				fixture := newWindowRoundFixture(t, nil, func(s *ReceiveBufferSettings) {
					s.ReceiveHoldPolicy = ReceiveHoldCommittedPrefix
					s.ReceiveQueueMaxByteCount = 2 * 1024 * 1024
					if partial {
						s.ReceiveQueueMaxByteCount = heldCount*66 + 1
					}
				})
				first := fixture.write(64)
				if MessageByteCount(first.pack.Frames) != 66 {
					t.Fatal("fixture encoded payload size changed")
				}
				fixture.forward(first, fixture.receiverIn)
				fixture.acknowledge()
				gap := fixture.write(64)
				witnesses := make([]weak.Pointer[decodedPackOwner], 0, heldCount)
				for range heldCount {
					packet := fixture.write(64)
					number := packet.pack.SequenceNumber
					fixture.forward(packet, fixture.receiverIn)
					witnesses = append(witnesses, receiveScratchOwnerWitness(t, fixture.receiveSequence(), number))
					fixture.acknowledge()
				}
				sequence := fixture.receiveSequence()
				if sequence.receiveQueue.Len() != heldCount || fixture.deliveredCount != 1 {
					t.Fatalf("native gap was not held: queued=%d delivered=%d", sequence.receiveQueue.Len(), fixture.deliveredCount)
				}
				last := sequence.receiveQueue.PeekLast()
				if last == nil || last.committed == partial {
					t.Fatal("fixture did not exercise the selected commitment branch")
				}
				last = nil
				fixture.forward(gap, fixture.receiverIn)
				fixture.acknowledge()
				if fixture.deliveredCount != heldCount+2 || fixture.ackedCount != heldCount+2 || sequence.receiveQueue.Len() != 0 || fixture.sequence().resendQueue.Len() != 0 {
					t.Fatalf("native delivery/ACK did not finish: delivered=%d acked=%d receive=%d resend=%d", fixture.deliveredCount, fixture.ackedCount, sequence.receiveQueue.Len(), fixture.sequence().resendQueue.Len())
				}
				if sequence.ctx.Err() != nil {
					t.Fatal("receiver retired instead of releasing its temporary roots")
				}
				runtime.GC()
				runtime.GC()
				live := countReceiveScratchOwners(witnesses)
				t.Logf("delivered=%d live-decoded-owners=%d bounded-free-capacity=%d owner-bytes=%d scratch-len=%d scratch-cap=%d", heldCount, live, decodedPackOwnerPoolCapacity, unsafe.Sizeof(decodedPackOwner{}), len(sequence.heldScratch), cap(sequence.heldScratch))
				if live > decodedPackOwnerPoolCapacity {
					t.Errorf("live idle receiver retains %d delivered decode owners beyond the %d-object free pool", live, decodedPackOwnerPoolCapacity)
				}
				runtime.KeepAlive(sequence)
			})
		})
	}
}

// Keep the last inspected owner out of the collecting goroutine's stack.
//
//go:noinline
func receiveScratchOwnerWitness(t *testing.T, sequence *ReceiveSequence, number uint64) weak.Pointer[decodedPackOwner] {
	t.Helper()
	item := sequence.receiveQueue.GetBySequenceNumber(number)
	if item == nil || item.decodedOwner == nil {
		t.Fatal("real wire decoder did not create the held owner")
	}
	return weak.Make(item.decodedOwner)
}

//go:noinline
func countReceiveScratchOwners(witnesses []weak.Pointer[decodedPackOwner]) int {
	live := 0
	for _, w := range witnesses {
		if w.Value() != nil {
			live++
		}
	}
	return live
}
