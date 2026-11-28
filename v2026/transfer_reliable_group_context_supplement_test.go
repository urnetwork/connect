// Admission contexts do not revoke accepted logical-group retry custody.
package connect

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// The provider closes its admission-only child after enqueue. Hold the actual
// shared parent first so every target frame is still raw at that cancellation.
func TestReliableGroupCanceledAdmissionContextKeepsDeferredCustody(t *testing.T) {
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
		target := reliableCustodyPair(t, root, nil)
		sibling := reliableCustodyPair(t, root, nil)
		reliableCustodyWarm(t, target)
		reliableCustodyWarm(t, sibling)
		var held []*windowRoundFrame
		wire, cost := reliableCustodyHold(t, sibling, root, DefaultMtu)
		held = append(held, wire)
		for n := 0; root.Available() >= cost; n++ {
			if n == 512 {
				t.Fatal("bounded genuine sibling census did not close target headroom")
			}
			wire, nextCost := reliableCustodyHold(t, sibling, root, DefaultMtu)
			if nextCost != cost {
				t.Fatal("same physical sibling shape changed retained class")
			}
			held = append(held, wire)
		}
		frames, witnesses = transferGroupTestFrames(t, 16, DefaultMtu)
		for _, frame := range frames {
			frame.MessageType = protocol.MessageType_IpIpPacketFromProvider
		}
		admissionCtx, cancelAdmission := context.WithCancel(target.ctx)
		defer cancelAdmission()
		observer, events := sendPackLifecycleTestObserver(target.receiver.ClientId())
		var callbacks atomic.Int32
		result := make(chan error, 2)
		accepted, err := target.sender.sendGroupWithTimeoutDetailed(frames, target.receiver.ClientId(), func(err error) {
			callbacks.Add(1)
			select {
			case result <- err:
			default:
				t.Error("admission-context group exceeded its callback bound")
			}
		}, 30*time.Second, Ctx(admissionCtx), sendPackLifecycleObserverOption{observer: observer})
		if !accepted || err != nil {
			t.Fatalf("group admission: accepted=%t err=%v", accepted, err)
		}
		admitted = true
		cancelAdmission()
		synctest.Wait()
		if admissionCtx.Err() != context.Canceled || target.sender.Ctx().Err() != nil || len(target.senderOut) != 0 || target.deliveredCount != 1 || callbacks.Load() != 0 || len(events) != 1 {
			t.Fatal("canceling admission-only context revoked accepted deferred custody or fabricated wire/delivery/lifecycle")
		}
		started := <-events
		requireSendPackLifecycleObservation(t, started, target.sender.ClientId(), target.receiver.ClientId(), started.Token, SendPackLifecyclePhaseStarted, true, false)
		for _, heldWire := range held {
			sibling.forward(heldWire, sibling.receiverIn)
			sibling.acknowledge()
		}
		reliableCustodyDrain(t, target, nil, result, 16, DefaultMtu)
		if callbacks.Load() != 1 || root.UsedByteCount() != 0 {
			t.Fatal("accepted group did not recover its exact bytes once after sibling ACKs")
		}
		for _, phase := range []SendPackLifecyclePhase{SendPackLifecyclePhaseFirstRouteWrite, SendPackLifecyclePhaseTerminal} {
			synctest.Wait()
			event := <-events
			requireSendPackLifecycleObservation(t, event, target.sender.ClientId(), target.receiver.ClientId(), started.Token, phase, true, false)
		}
		requireNoSendPackLifecycleObservations(t, events, "admission-only group context")
	})
}
