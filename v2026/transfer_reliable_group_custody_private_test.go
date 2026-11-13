// Retained pressure must not revoke an already admitted reliable provider group.
package connect

import (
	"bytes"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Production provider returns use this same logical-group entry. This test
// observes its completion with a callback. A real retained sibling leaves
// one-MTU headroom, less than the
// H1 chunk selected only after eligibility. No send/reservation failure is faked.
func TestReliableH1GroupKeepsCustodyAcrossChunkPlanningPressure(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		root := NewTransferMemoryBudget(2 * 1024 * 1024)
		var frames []*protocol.Frame
		var witnesses [][]byte
		framesOwned := true
		t.Cleanup(func() {
			if framesOwned {
				for _, frame := range frames {
					MessagePoolReturn(frame.MessageBytes)
				}
			}
			for _, witness := range witnesses {
				if !MessagePoolReturn(witness) {
					t.Error("logical group kept a raw owner after both clients joined")
				}
			}
			if stats := root.Stats(); stats.UsedByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
				t.Errorf("shared retained parent did not drain: %+v", stats)
			}
		})
		pair := func() *windowRoundFixture {
			leaf := NewTransferMemoryBudgetWithParent(2*1024*1024, root)
			f := newWindowRoundFixture(t, func(settings *SendBufferSettings) {
				settings.ResendQueueBudget = leaf
				settings.ResendQueueRetainedByteAccounting = true
				// A fixed logical window isolates the retained-byte admission
				// boundary. Its 2-MiB ceiling cannot enlarge the shared budget.
				settings.ResendQueueMaxByteCount = 2 * 1024 * 1024
				settings.ResendQueueMinByteCount = 0
				settings.SequenceBufferSize = 32
			}, nil, TransportTypeH1)
			return f
		}
		target, sibling := pair(), pair()
		warm := func(f *windowRoundFixture) *SendSequence {
			wire := f.write(8)
			generation, err := IdFromBytes(wire.pack.SequenceId)
			if err != nil {
				t.Fatal(err)
			}
			f.forward(wire, f.receiverIn)
			f.acknowledge()
			f.sender.sendBuffer.mutex.Lock()
			sequence := f.sender.sendBuffer.sendSequencesBySequenceId[generation]
			f.sender.sendBuffer.mutex.Unlock()
			if sequence == nil || !sequence.transferFlightPolicy().h1Only {
				t.Fatal("actual warmed writer is not H1")
			}
			return sequence
		}
		sequence := warm(target)
		warm(sibling)
		if root.UsedByteCount() != 0 {
			t.Fatal("real warmup ACKs did not release retained ownership")
		}
		frames, witnesses = transferGroupTestFrames(t, 16, DefaultMtu)
		for _, frame := range frames {
			frame.MessageType = protocol.MessageType_IpIpPacketFromProvider
		}
		// This local probe measures the production two planning surfaces. It
		// never enters a worker, reserves memory, or changes an admitted group.
		probe := SendPack{Frames: frames, logicalGroup: true, MessageByteCount: MessageByteCount(frames)}
		beforePlan := sequence.retainedSendPackByteCount(&probe)
		sequence.pinLogicalGroupChunkLimits(&probe, sequence.transferFlightPolicy())
		afterPlan := sequence.retainedSendPackByteCount(&probe)
		if beforePlan >= afterPlan {
			t.Fatalf("control did not span an H1 planning increase: before=%d after=%d", beforePlan, afterPlan)
		}
		var held []*windowRoundFrame
		retainSibling := func(size int) ByteCount {
			filler := &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketFromProvider, MessageBytes: MessagePoolGet(size), Raw: true}
			before := root.UsedByteCount()
			accepted, err := sibling.sender.sendGroupWithTimeoutDetailed([]*protocol.Frame{filler}, sibling.receiver.ClientId(), func(error) {}, 30*time.Second)
			if !accepted || err != nil {
				MessagePoolReturn(filler.MessageBytes)
				t.Fatalf("genuine sibling group admission: accepted=%t err=%v", accepted, err)
			}
			wire := sibling.take(sibling.senderOut)
			if wire.pack == nil || wire.pack.Nack || len(wire.pack.Frames) != 1 || len(wire.pack.Frames[0].MessageBytes) != size {
				t.Fatal("genuine sibling did not retain one reliably serialized frame")
			}
			held = append(held, wire)
			cost := root.UsedByteCount() - before
			if cost <= 0 {
				t.Fatal("sibling wire has no retained parent reservation")
			}
			return cost
		}
		// One real larger wire changes the retained class residue while
		// keeping the parent ceiling fixed. Subsequent one-MTU reservations
		// stop before they can cross the measured lower planning bound.
		retainSibling(2 * DefaultMtu)
		largeCost := retainSibling(DefaultMtu)
		for n := 0; root.Available()-largeCost >= beforePlan; n++ {
			if n == 512 {
				t.Fatal("bounded sibling census did not approach the retained ceiling")
			}
			if cost := retainSibling(DefaultMtu); cost != largeCost {
				t.Fatalf("same actual sibling shape changed retained cost: %d vs %d", cost, largeCost)
			}
		}

		available := root.Available()
		if available < beforePlan || available >= afterPlan || root.TotalByteCount() != 2*1024*1024 {
			t.Fatalf("real retained siblings did not isolate the planning boundary: available=%d before=%d after=%d", available, beforePlan, afterPlan)
		}
		var callbacks atomic.Int64
		result := make(chan error, 2)
		accepted, err := target.sender.sendGroupWithTimeoutDetailed(frames, target.receiver.ClientId(), func(err error) {
			callbacks.Add(1)
			select {
			case result <- err:
			default:
				t.Error("reliable group callback exceeded its exact completion bound")
			}
		}, 30*time.Second)
		if !accepted || err != nil {
			t.Fatalf("real provider logical group was not admitted: accepted=%t err=%v", accepted, err)
		}
		framesOwned = false
		synctest.Wait()

		select {
		case err := <-result:
			t.Fatalf("admitted reliable group terminated before capacity/peer ACKs: %v (materialized=%d queuedWire=%d receiverFrames=%d)", err, target.sender.SendRecoveryStats().InitialFrameCount-1, len(target.senderOut), target.deliveredCount-1)
		default:
		}
		if len(target.senderOut) != 0 || target.deliveredCount != 1 {
			t.Fatal("group crossed the known retained headroom before sibling release")
		}
		// Only genuine receiver ACKs release sibling ownership. The already
		// admitted target group must recover itself; its originals are not sent again.
		for _, wire := range held {
			sibling.forward(wire, sibling.receiverIn)
			sibling.acknowledge()
		}
		received := make([]int, 16)
		target.receiver.AddReceiveCallback(func(_ TransferPath, decoded []*protocol.Frame, _ Peer) {
			for _, frame := range decoded {
				if len(frame.MessageBytes) != DefaultMtu || frame.MessageBytes[0] < 1 || frame.MessageBytes[0] > 16 || !bytes.Equal(frame.MessageBytes, bytes.Repeat(frame.MessageBytes[:1], DefaultMtu)) {
					t.Error("reliable group changed an original payload")
					continue
				}
				received[int(frame.MessageBytes[0])-1]++
			}
		})
		for chunks := 0; ; chunks++ {
			synctest.Wait()
			select {
			case err := <-result:
				if err != nil || callbacks.Load() != 1 {
					t.Fatalf("recovered reliable group did not complete once: err=%v count=%d", err, callbacks.Load())
				}
				for index, count := range received {
					if count != 1 {
						t.Fatalf("original frame %d delivered %d times", index, count)
					}
				}
				if root.UsedByteCount() != 0 {
					t.Fatal("successful real peer ACKs did not drain the shared parent")
				}
				return
			default:
			}
			if chunks == 16 {
				t.Fatal("admitted group did not drain its complete frame census")
			}
			wire := target.take(target.senderOut)
			if wire.pack == nil || wire.pack.Nack {
				t.Fatal("reliable group lost actual codec/reliable wire semantics")
			}
			target.forward(wire, target.receiverIn)
			if chunks == 0 {
				select {
				case err := <-result:
					t.Fatalf("whole group ACK preceded genuine chunk peer feedback: %v", err)
				default:
				}
			}
			target.acknowledge()
		}
	})
}
