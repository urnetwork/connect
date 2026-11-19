// Exact shared-parent races exercise borrowing, deferred custody and teardown.
package connect

import (
	"bytes"
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Checks the root after every later client cleanup has joined.
func reliableCustodyRoot(t *testing.T) *TransferMemoryBudget {
	t.Helper()
	root := NewTransferMemoryBudget(2 * 1024 * 1024)
	t.Cleanup(func() {
		stats := root.Stats()
		if stats.UsedByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
			t.Errorf("shared parent did not join and balance: %+v", stats)
		}
	})
	return root
}

// Each real client has an independent leaf under the unchanged provider ceiling.
func reliableCustodyPair(t *testing.T, root *TransferMemoryBudget, hook func()) *windowRoundFixture {
	t.Helper()
	return newWindowRoundFixture(t, func(settings *SendBufferSettings) {
		settings.ResendQueueBudget = NewTransferMemoryBudgetWithParent(2*1024*1024, root)
		settings.ResendQueueRetainedByteAccounting = true
		settings.ResendQueueMaxByteCount = 2 * 1024 * 1024
		settings.ResendQueueMinByteCount = 0
		settings.SequenceBufferSize = 32
		settings.beforeGroupRetainForTest = hook
	}, nil, TransportTypeH1)
}

// Establishes an actual H1 sequence with genuine receiver evidence.
func reliableCustodyWarm(t *testing.T, fixture *windowRoundFixture) *SendSequence {
	t.Helper()
	wire := fixture.write(8)
	generation, err := IdFromBytes(wire.pack.SequenceId)
	if err != nil {
		t.Fatal(err)
	}
	fixture.forward(wire, fixture.receiverIn)
	fixture.acknowledge()
	fixture.sender.sendBuffer.mutex.Lock()
	sequence := fixture.sender.sendBuffer.sendSequencesBySequenceId[generation]
	fixture.sender.sendBuffer.mutex.Unlock()
	if sequence == nil || !sequence.transferFlightPolicy().h1Only {
		t.Fatal("actual established source is not H1")
	}
	return sequence
}

// Retains one real sibling wire until its real peer receives it and ACKs it.
func reliableCustodyHold(t *testing.T, fixture *windowRoundFixture, root *TransferMemoryBudget, size int) (*windowRoundFrame, ByteCount) {
	t.Helper()
	frame := &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketFromProvider, MessageBytes: MessagePoolGet(size), Raw: true}
	before := root.UsedByteCount()
	accepted, err := fixture.sender.sendGroupWithTimeoutDetailed([]*protocol.Frame{frame}, fixture.receiver.ClientId(), func(error) {}, 30*time.Second)
	if !accepted || err != nil {
		MessagePoolReturn(frame.MessageBytes)
		t.Fatalf("sibling admission: accepted=%t err=%v", accepted, err)
	}
	wire := fixture.take(fixture.senderOut)
	cost := root.UsedByteCount() - before
	if wire.pack == nil || wire.pack.Nack || len(wire.pack.Frames) != 1 || len(wire.pack.Frames[0].MessageBytes) != size || cost <= 0 {
		t.Fatal("sibling lacks real reliable wire and retained ownership")
	}
	return wire, cost
}

// Asserts the complete original census through actual decode and receiver ACKs.
func reliableCustodyDrain(t *testing.T, fixture *windowRoundFixture, first *windowRoundFrame, result <-chan error, count int, size int) {
	t.Helper()
	received := make([]int, count)
	fixture.receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
		for _, frame := range frames {
			if len(frame.MessageBytes) != size {
				t.Error("an admitted original changed its size")
				continue
			}
			index := int(frame.MessageBytes[0]) - 1
			if index < 0 || count <= index || !bytes.Equal(frame.MessageBytes, bytes.Repeat(frame.MessageBytes[:1], size)) {
				t.Error("an admitted original changed at the real receive boundary")
				continue
			}
			received[index]++
		}
	})
	for chunks := 0; chunks <= count; chunks++ {
		synctest.Wait()
		select {
		case err := <-result:
			if err != nil {
				t.Fatalf("admitted reliable suffix failed after independent capacity drained: %v", err)
			}
			for index, n := range received {
				if n != 1 {
					t.Fatalf("original %d delivered %d times", index, n)
				}
			}
			return
		default:
		}
		var wire *windowRoundFrame
		if first != nil {
			wire, first = first, nil
		} else {
			wire = fixture.take(fixture.senderOut)
		}
		if wire.pack == nil || wire.pack.Nack {
			t.Fatal("actual data wire is not ACK-required")
		}
		fixture.forward(wire, fixture.receiverIn)
		select {
		case err := <-result:
			t.Fatalf("whole-group completion preceded this chunk's peer ACK: %v", err)
		default:
		}
		fixture.acknowledge()
	}
	t.Fatal("complete original census did not finish within its chunk bound")
}

// The second chunk is already selected when another real client wins the
// shared parent. No reservation outcome, send result or route write is faked.
func reliableCustodySharedRace(t *testing.T, cancelTarget bool) {
	t.Helper()
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
	var boundaries atomic.Int32
	selected, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	resume := func() { releaseOnce.Do(func() { close(release) }) }
	target := reliableCustodyPair(t, root, func() {
		if armed.Load() && boundaries.Add(1) == 2 {
			close(selected)
			<-release
		}
	})
	sibling := reliableCustodyPair(t, root, nil)
	t.Cleanup(resume)
	source := reliableCustodyWarm(t, target)
	reliableCustodyWarm(t, sibling)
	frames, witnesses = transferGroupTestFrames(t, 16, DefaultMtu)
	for _, frame := range frames {
		frame.MessageType = protocol.MessageType_IpIpPacketFromProvider
	}
	observer, events := sendPackLifecycleTestObserver(target.receiver.ClientId())
	var callbacks atomic.Int32
	result := make(chan error, 2)
	armed.Store(true)
	accepted, err := target.sender.sendGroupWithTimeoutDetailed(frames, target.receiver.ClientId(), func(err error) {
		callbacks.Add(1)
		select {
		case result <- err:
		default:
			t.Error("group completion exceeded its callback bound")
		}
	}, 30*time.Second, sendPackLifecycleObserverOption{observer: observer})
	if !accepted || err != nil {
		t.Fatalf("target group admission: accepted=%t err=%v", accepted, err)
	}
	admitted = true
	<-selected
	prefix := target.take(target.senderOut)
	if prefix.pack == nil || prefix.pack.Nack || len(prefix.pack.Frames) != 3 {
		t.Fatal("first genuine H1 chunk is not three retained MTU originals")
	}
	var held []*windowRoundFrame
	wire, cost := reliableCustodyHold(t, sibling, root, DefaultMtu)
	held = append(held, wire)
	for n := 0; root.Available() >= cost; n++ {
		if n == 512 {
			t.Fatal("bounded sibling reservations did not reach parent pressure")
		}
		wire, nextCost := reliableCustodyHold(t, sibling, root, DefaultMtu)
		if nextCost != cost {
			t.Fatal("identical actual sibling wire changed retained class")
		}
		held = append(held, wire)
	}
	resume()
	synctest.Wait()
	if len(target.senderOut) != 0 || callbacks.Load() != 0 || len(events) != 1 {
		t.Fatal("second-chunk pressure lost deferred custody or published premature whole-group lifecycle")
	}
	started := <-events
	requireSendPackLifecycleObservation(t, started, target.sender.ClientId(), target.receiver.ClientId(), started.Token, SendPackLifecyclePhaseStarted, true, false)
	if cancelTarget {
		// Cancel only this fixture's child clients. The shared parent and
		// independent sibling retain their identities and continue to run.
		target.cancel()
		for _, client := range []*Client{target.sender, target.receiver} {
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		if err := <-result; err == nil || callbacks.Load() != 1 || source.resendQueue.budget.UsedByteCount() != 0 {
			t.Fatalf("canceled child did not join its exact custody once: err=%v callbacks=%d retained=%d", err, callbacks.Load(), source.resendQueue.budget.UsedByteCount())
		}
	}
	for _, heldWire := range held {
		sibling.forward(heldWire, sibling.receiverIn)
		sibling.acknowledge()
	}
	if !cancelTarget {
		reliableCustodyDrain(t, target, prefix, result, 16, DefaultMtu)
		if callbacks.Load() != 1 {
			t.Fatal("whole logical group callback was not exactly once")
		}
	}
	for _, phase := range []SendPackLifecyclePhase{SendPackLifecyclePhaseFirstRouteWrite, SendPackLifecyclePhaseTerminal} {
		synctest.Wait()
		event := <-events
		requireSendPackLifecycleObservation(t, event, target.sender.ClientId(), target.receiver.ClientId(), started.Token, phase, true, cancelTarget)
	}
	requireNoSendPackLifecycleObservations(t, events, "shared-parent logical group")
	healthy, _ := reliableCustodyHold(t, sibling, root, 64)
	sibling.forward(healthy, sibling.receiverIn)
	sibling.acknowledge()
	if root.UsedByteCount() != 0 || sibling.sender.Ctx().Err() != nil {
		t.Fatal("child failure or pressure poisoned the live sibling/shared parent")
	}
}

// A competing client reservation cannot destroy an already admitted suffix.
func TestReliableGroupKeepsCustodyAcrossSharedParentReservationRace(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) { reliableCustodySharedRace(t, false) })
}

// A canceled child releases every deferred owner without canceling its sibling.
func TestReliableGroupCancellationPreservesSharedParentAndSibling(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) { reliableCustodySharedRace(t, true) })
}

// One group can exceed the fixed ceiling when each actual chunk fits and its
// genuine peer ACK releases the next chunk. The whole group is never reserved.
func TestReliableGroupLargerThanRetainedLeafDrainsByPeerAcks(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		root := reliableCustodyRoot(t)
		fixture := reliableCustodyPair(t, root, nil)
		reliableCustodyWarm(t, fixture)
		const count, size = 64, 40000
		frames := make([]*protocol.Frame, count)
		for index := range frames {
			frames[index] = &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketFromProvider, MessageBytes: bytes.Repeat([]byte{byte(index + 1)}, size), Raw: true}
		}
		if MessageByteCount(frames) <= root.TotalByteCount() {
			t.Fatal("control group does not exceed the unchanged retained ceiling")
		}
		result := make(chan error, 2)
		accepted, err := fixture.sender.sendGroupWithTimeoutDetailed(frames, fixture.receiver.ClientId(), func(err error) { result <- err }, 30*time.Second)
		if !accepted || err != nil {
			t.Fatalf("large logical group admission: %t %v", accepted, err)
		}
		reliableCustodyDrain(t, fixture, nil, result, count, size)
		if root.UsedByteCount() != 0 || root.TotalByteCount() != 2*1024*1024 {
			t.Fatal("large group escaped or failed to drain its fixed budget")
		}
	})
}

// An unavailable chunk keeps accepted custody until cancellation. Current
// capacity is retunable, so it cannot prove a permanent structural failure.
func TestReliableGroupUnavailableChunkCancelsWithoutStrandingSibling(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		root := reliableCustodyRoot(t)
		target := reliableCustodyPair(t, root, nil)
		sibling := reliableCustodyPair(t, root, nil)
		reliableCustodyWarm(t, target)
		reliableCustodyWarm(t, sibling)
		frame := &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketFromProvider, MessageBytes: make([]byte, 900*1024), Raw: true}
		result := make(chan error, 2)
		var callbacks atomic.Int32
		accepted, err := target.sender.sendGroupWithTimeoutDetailed([]*protocol.Frame{frame}, target.receiver.ClientId(), func(err error) {
			callbacks.Add(1)
			select {
			case result <- err:
			default:
				t.Error("unavailable group completed beyond its exact bound")
			}
		}, 30*time.Second)
		if !accepted || err != nil {
			t.Fatalf("unavailable control did not enter its ordinary group boundary: %t %v", accepted, err)
		}
		synctest.Wait()
		select {
		case err := <-result:
			t.Fatalf("current retunable capacity revoked accepted custody: %v", err)
		default:
		}
		if len(target.senderOut) != 0 || target.deliveredCount != 1 || root.UsedByteCount() != 0 {
			t.Fatal("unavailable input crossed wire, delivery or retained accounting")
		}
		healthy, _ := reliableCustodyHold(t, sibling, root, 64)
		sibling.forward(healthy, sibling.receiverIn)
		sibling.acknowledge()
		target.cancel()
		for _, client := range []*Client{target.sender, target.receiver} {
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		if err := <-result; err == nil || callbacks.Load() != 1 {
			t.Fatalf("explicit child cancellation did not complete the unavailable input once: %v count=%d", err, callbacks.Load())
		}
		healthy, _ = reliableCustodyHold(t, sibling, root, 64)
		sibling.forward(healthy, sibling.receiverIn)
		sibling.acknowledge()
		if root.UsedByteCount() != 0 || sibling.sender.Ctx().Err() != nil {
			t.Fatal("unavailable child or its teardown stranded its live sibling")
		}
	})
}

// A public live resize after admission is transient policy pressure. It must
// preserve original custody and wake on regrowth without any manual reoffer.
func TestReliableGroupRetainsCustodyAcrossLiveBudgetShrinkAndRegrowth(t *testing.T) {
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
		var source *SendSequence
		shrunk := make(chan struct{})
		target := reliableCustodyPair(t, root, func() {
			if armed.CompareAndSwap(true, false) {
				source.resendQueue.budget.SetTotalByteCount(1)
				close(shrunk)
			}
		})
		sibling := reliableCustodyPair(t, root, nil)
		source = reliableCustodyWarm(t, target)
		reliableCustodyWarm(t, sibling)
		frames, witnesses = transferGroupTestFrames(t, 16, DefaultMtu)
		for _, frame := range frames {
			frame.MessageType = protocol.MessageType_IpIpPacketFromProvider
		}
		var callbacks atomic.Int32
		result := make(chan error, 2)
		armed.Store(true)
		accepted, err := target.sender.sendGroupWithTimeoutDetailed(frames, target.receiver.ClientId(), func(err error) {
			callbacks.Add(1)
			select {
			case result <- err:
			default:
				t.Error("resized group completed beyond its exact callback bound")
			}
		}, 30*time.Second)
		if !accepted || err != nil {
			t.Fatalf("live-resize group admission: %t %v", accepted, err)
		}
		admitted = true
		<-shrunk
		synctest.Wait()
		select {
		case err := <-result:
			t.Fatalf("LIVE_BUDGET_SHRINK_REVOKED_ACCEPTED_GROUP: terminal=%v before capacity regrowth or any receiver ACK", err)
		default:
		}
		if len(target.senderOut) != 0 || target.deliveredCount != 1 || source.resendQueue.budget.TotalByteCount() != 1 || root.TotalByteCount() != 2*1024*1024 {
			t.Fatal("public live shrink did not hold the exact known unmaterialized group")
		}
		healthy, _ := reliableCustodyHold(t, sibling, root, 64)
		sibling.forward(healthy, sibling.receiverIn)
		sibling.acknowledge()
		source.resendQueue.budget.SetTotalByteCount(2 * 1024 * 1024)
		reliableCustodyDrain(t, target, nil, result, 16, DefaultMtu)
		if callbacks.Load() != 1 || root.UsedByteCount() != 0 {
			t.Fatal("regrowth did not recover exact original ownership once")
		}
	})
}
