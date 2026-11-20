// Independent ACK-required raw admissions retain order across aggregate pressure.
package connect

import (
	"bytes"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// All sixteen originals are ready before Run. A competing real Client wins
// the shared parent after selection; the complete cohort must remain queued.
func TestReliableReadyCoalescerRestoresEveryOriginalInOrderAfterReservationRace(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		root := reliableCustodyRoot(t)
		var frames []*protocol.Frame
		var witnesses [][]byte
		admitted := 0
		t.Cleanup(func() {
			for _, frame := range frames[admitted:] {
				MessagePoolReturn(frame.MessageBytes)
			}
			releaseTransferGroupTestWitnesses(t, frames, witnesses)
		})
		runEntered, runRelease := make(chan struct{}), make(chan struct{})
		selected, prepareRelease := make(chan struct{}), make(chan struct{})
		var runOnce, runReleaseOnce, prepareReleaseOnce sync.Once
		var armed atomic.Bool
		resumeRun := func() { runReleaseOnce.Do(func() { close(runRelease) }) }
		resumePrepare := func() { prepareReleaseOnce.Do(func() { close(prepareRelease) }) }
		target := newWindowRoundFixture(t, func(settings *SendBufferSettings) {
			settings.ResendQueueBudget = NewTransferMemoryBudgetWithParent(2*1024*1024, root)
			settings.ResendQueueRetainedByteAccounting = true
			settings.ResendQueueMaxByteCount = 2 * 1024 * 1024
			settings.ResendQueueMinByteCount = 0
			settings.SequenceBufferSize = 32
			settings.beforeRunSendSequenceForTest = func(sendSequenceId) {
				runOnce.Do(func() { close(runEntered) })
				<-runRelease
			}
			settings.beforeGroupRetainForTest = func() {
				if armed.CompareAndSwap(true, false) {
					close(selected)
					<-prepareRelease
				}
			}
		}, nil, TransportTypeH1)
		sibling := reliableCustodyPair(t, root, nil)
		t.Cleanup(resumeRun)
		t.Cleanup(resumePrepare)
		reliableCustodyWarm(t, sibling)
		const count, size = 16, 64
		frames, witnesses = transferGroupTestFrames(t, count, size)
		ackTarget := &reliableRawCustodyTarget{results: make(chan reliableRawCustodyResult, 2*count)}
		observer, events := sendPackLifecycleTestObserver(target.receiver.ClientId())
		for index, frame := range frames {
			accepted, err := target.sender.sendRawWithTimeoutDetailed(protocol.MessageType_IpIpPacketFromProvider,
				frame.MessageBytes, target.receiver.ClientId(), ackTarget, ByteCount(index), 30*time.Second,
				sendPackLifecycleObserverOption{observer: observer})
			if !accepted || err != nil {
				t.Fatalf("raw original %d admission: %t %v", index, accepted, err)
			}
			admitted++
			if index == 0 {
				<-runEntered
			}
		}
		armed.Store(true)
		resumeRun()
		<-selected
		var held []*windowRoundFrame
		wire, cost := reliableCustodyHold(t, sibling, root, DefaultMtu)
		held = append(held, wire)
		for n := 0; root.Available() >= cost; n++ {
			if n == 512 {
				t.Fatal("bounded genuine sibling reservations did not fill the parent")
			}
			wire, nextCost := reliableCustodyHold(t, sibling, root, DefaultMtu)
			if nextCost != cost {
				t.Fatal("same sibling shape changed retained cost")
			}
			held = append(held, wire)
		}
		resumePrepare()
		synctest.Wait()
		select {
		case result := <-ackTarget.results:
			t.Fatalf("RELIABLE_COALESCED_CUSTODY_LOST: accepted original %d completed before capacity/receiver ACK: err=%v terminalCount=%d wire=%d received=%d", result.index, result.err, ackTarget.count.Load(), len(target.senderOut), target.deliveredCount)
		default:
		}
		if len(target.senderOut) != 0 || target.deliveredCount != 0 || len(events) != count {
			t.Fatal("aggregate pressure fabricated delivery or original lifecycle progress")
		}
		tokens := make([]uint64, count)
		for index := range count {
			started := <-events
			tokens[index] = started.Token
			requireSendPackLifecycleObservation(t, started, target.sender.ClientId(), target.receiver.ClientId(), started.Token, SendPackLifecyclePhaseStarted, true, false)
		}
		for _, heldWire := range held {
			sibling.forward(heldWire, sibling.receiverIn)
			sibling.acknowledge()
		}
		received := make([]int, count)
		target.receiver.AddReceiveCallback(func(_ TransferPath, decoded []*protocol.Frame, _ Peer) {
			for _, frame := range decoded {
				if len(frame.MessageBytes) != size || frame.MessageBytes[0] < 1 || frame.MessageBytes[0] > count || !bytes.Equal(frame.MessageBytes, bytes.Repeat(frame.MessageBytes[:1], size)) {
					t.Error("an original changed at actual receive")
					continue
				}
				received[int(frame.MessageBytes[0])-1]++
			}
		})
		original := target.take(target.senderOut)
		if original.pack == nil || original.pack.Nack || len(original.pack.Frames) != count {
			t.Fatal("coalesced cohort lost its sixteen-original actual wire shape")
		}
		for index, frame := range original.pack.Frames {
			if !bytes.Equal(frame.MessageBytes, bytes.Repeat([]byte{byte(index + 1)}, size)) {
				t.Fatalf("raw original %d changed payload or FIFO order", index)
			}
		}
		target.forward(original, target.receiverIn)
		if len(ackTarget.results) != 0 {
			t.Fatal("coalesced originals completed before genuine receiver ACK")
		}
		target.acknowledge()
		for index := range count {
			result := <-ackTarget.results
			if result.err != nil || result.index != index || received[index] != 1 {
				t.Fatalf("original %d did not ACK once in original order: %+v delivered=%d", index, result, received[index])
			}
		}
		for _, phase := range []SendPackLifecyclePhase{SendPackLifecyclePhaseFirstRouteWrite, SendPackLifecyclePhaseTerminal} {
			for index := range count {
				event := <-events
				requireSendPackLifecycleObservation(t, event, target.sender.ClientId(), target.receiver.ClientId(), tokens[index], phase, true, false)
			}
		}
		if ackTarget.count.Load() != count || len(target.senderOut) != 0 || root.UsedByteCount() != 0 {
			t.Fatal("original callbacks, wire or aggregate ownership did not drain exactly")
		}
		requireNoSendPackLifecycleObservations(t, events, "coalesced raw cohort")
	})
}
