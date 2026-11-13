// Accepted reliable raw packets keep custody across a genuine parent race.
package connect

import (
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

type reliableRawCustodyResult struct {
	index int
	err   error
}

// Exercises the same typed raw ACK-result entry as provider sendReturnPacket.
type reliableRawCustodyTarget struct {
	results chan reliableRawCustodyResult
	count   atomic.Int32
}

func (self *reliableRawCustodyTarget) sendAckResult(value ByteCount, err error) {
	self.count.Add(1)
	select {
	case self.results <- reliableRawCustodyResult{index: int(value), err: err}:
	default:
		panic("raw custody result overflow")
	}
}

// Hold selection before the actual reservation, then fill its shared parent
// through a different real Client. No send or reserve return value is forced.
func TestReliableRawSingletonKeepsCustodyAcrossSharedReservationRace(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		root := reliableCustodyRoot(t)
		var frames []*protocol.Frame
		var witnesses [][]byte
		admitted := false
		t.Cleanup(func() {
			if !admitted {
				for _, frame := range frames {
					MessagePoolReturn(frame.MessageBytes)
				}
			}
			releaseTransferGroupTestWitnesses(t, frames, witnesses)
		})
		var armed atomic.Bool
		selected, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		resume := func() { releaseOnce.Do(func() { close(release) }) }
		target := reliableCustodyPair(t, root, func() {
			if armed.CompareAndSwap(true, false) {
				close(selected)
				<-release
			}
		})
		sibling := reliableCustodyPair(t, root, nil)
		t.Cleanup(resume)
		reliableCustodyWarm(t, target)
		reliableCustodyWarm(t, sibling)
		frames, witnesses = transferGroupTestFrames(t, 1, DefaultMtu)
		ackTarget := &reliableRawCustodyTarget{results: make(chan reliableRawCustodyResult, 2)}
		armed.Store(true)
		accepted, err := target.sender.sendRawWithTimeoutDetailed(protocol.MessageType_IpIpPacketFromProvider,
			frames[0].MessageBytes, target.receiver.ClientId(), ackTarget, 0, 30*time.Second)
		if !accepted || err != nil {
			t.Fatalf("provider raw entry did not admit original: accepted=%t err=%v", accepted, err)
		}
		admitted = true
		<-selected
		var held []*windowRoundFrame
		wire, cost := reliableCustodyHold(t, sibling, root, DefaultMtu)
		held = append(held, wire)
		for n := 0; root.Available() >= cost; n++ {
			if n == 512 {
				t.Fatal("real bounded sibling reservations did not close raw headroom")
			}
			wire, nextCost := reliableCustodyHold(t, sibling, root, DefaultMtu)
			if nextCost != cost {
				t.Fatal("same sibling wire changed its actual retained cost")
			}
			held = append(held, wire)
		}
		resume()
		synctest.Wait()
		select {
		case result := <-ackTarget.results:
			t.Fatalf("RELIABLE_RAW_CUSTODY_LOST: accepted original completed before capacity/receiver ACK: index=%d err=%v wire=%d received=%d", result.index, result.err, len(target.senderOut), target.deliveredCount-1)
		default:
		}
		if len(target.senderOut) != 0 || target.deliveredCount != 1 {
			t.Fatal("known raw pressure produced wire or receiver delivery")
		}
		for _, heldWire := range held {
			sibling.forward(heldWire, sibling.receiverIn)
			sibling.acknowledge()
		}
		original := target.take(target.senderOut)
		if original.pack == nil || original.pack.Nack || len(original.pack.Frames) != 1 || len(original.pack.Frames[0].MessageBytes) != DefaultMtu || original.pack.Frames[0].MessageBytes[0] != 1 {
			t.Fatal("raw original changed at its actual serialized boundary")
		}
		target.forward(original, target.receiverIn)
		if len(ackTarget.results) != 0 {
			t.Fatal("raw completion preceded genuine receiver ACK")
		}
		target.acknowledge()
		result := <-ackTarget.results
		if result.err != nil || result.index != 0 || ackTarget.count.Load() != 1 || target.deliveredCount != 2 || root.UsedByteCount() != 0 {
			t.Fatalf("raw original did not recover once and drain by peer ACK: %+v count=%d delivered=%d retained=%d", result, ackTarget.count.Load(), target.deliveredCount, root.UsedByteCount())
		}
	})
}
