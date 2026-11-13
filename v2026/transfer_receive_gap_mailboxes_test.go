// Gap reconciliation keeps separate finite custody for both native mailboxes.
// These component tests drive the real Pack, Run, callback and ACK writer paths.
package connect

import (
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Both classes retain reliable Pack identity; only the frame classification
// determines which real admission boundary takes the pooled transfer root.
func receiveGapMailboxPackTest(sequence *ReceiveSequence, number uint64, control bool) *ReceivePack {
	pack := receiveGapHandoffPackTest(sequence, number)
	if !control {
		pack.Pack.Frames[0].MessageType = protocol.MessageType_TestSimpleMessage
	}
	return pack
}

// A writable real route makes an unexpected ACK observable after Run's exit.
// Cleanup returns any actual writer-owned root even when an assertion fails.
func receiveGapMailboxAckRouteTest(t *testing.T, sequence *ReceiveSequence) Route {
	t.Helper()
	route := make(Route, 32)
	transport := NewSendGatewayTransport()
	sequence.client.RouteManager().UpdateTransport(transport, []Route{route})
	t.Cleanup(func() {
		sequence.client.RouteManager().RemoveTransport(transport)
		for len(route) != 0 {
			MessagePoolReturn(<-route)
		}
	})
	return route
}

// Two prefixes must alternate while closing a real hole. The original gap
// clock and ordinary callback batching survive each advanced delivery point.
func TestReceiveSequenceGapMailboxesCloseMixedPrefix(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		sequence, log := receiveGapSequenceTest(t)
		t.Cleanup(sequence.Close)
		var delivered atomic.Int32
		sequence.client.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
			delivered.Add(int32(len(frames)))
			sequence.Cancel()
		})
		started := time.Now()
		addReceiveGapTestItem(t, sequence, 4, started.Add(-sequence.receiveBufferSettings.GapTimeout), true)
		for number := uint64(0); number < 4; number++ {
			admitReceiveGapHandoffPackTest(t, sequence, receiveGapMailboxPackTest(sequence, number, number%2 == 0))
		}
		var turns [][2]int
		sequence.receiveBufferSettings.receiveOwnerStageForTest = func(_ *ReceiveSequence, _ *decodedPackOwner, stage string) {
			if stage == "pack_released" {
				turns = append(turns, [2]int{len(sequence.packs), len(sequence.protocolPacks)})
			}
		}
		startReceiveGapHandoffTest(t, sequence)
		waitReceiveGapTestExit(t, sequence)
		want := [][2]int{{2, 1}, {1, 1}, {1, 0}, {0, 0}}
		if len(turns) != len(want) {
			t.Fatalf("mixed prefix did not take four real owners: turns=%v logs=%v", turns, log.messages())
		}
		for i := range want {
			if turns[i] != want[i] {
				t.Fatalf("mixed prefix lost protocol/DATA alternation: turns=%v", turns)
			}
		}
		if delivered.Load() != 5 || sequence.nextSequenceNumber != 5 || sequence.packQueueCount.Load() != 0 ||
			len(log.messages()) != 0 || time.Now() != started {
			t.Fatalf("mixed admitted prefix lost progress: delivered=%d next=%d retained=%d logs=%v", delivered.Load(), sequence.nextSequenceNumber, sequence.packQueueCount.Load(), log.messages())
		}
	})
}

// Refill both mailboxes synchronously at every real dequeue. Only the six
// first-expiry owners may be parsed; new traffic cannot spend another class's
// census or keep the same missing predecessor alive.
func TestReceiveSequenceGapMailboxesDoNotRefillEitherPrefix(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		sequence, log := receiveGapSequenceTest(t)
		t.Cleanup(sequence.Close)
		var delivered atomic.Int32
		sequence.client.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) { delivered.Add(int32(len(frames))) })
		started := time.Now()
		addReceiveGapTestItem(t, sequence, 1, started.Add(-sequence.receiveBufferSettings.GapTimeout), true)
		for number := uint64(2); number < 8; number++ {
			admitReceiveGapHandoffPackTest(t, sequence, receiveGapMailboxPackTest(sequence, number, number%2 == 0))
		}
		var snapshots, prefix, turns int
		var refillFailed bool
		sequence.receiveBufferSettings.afterGapHandoffSnapshotForTest = func(_ receiveSequenceId, count int) {
			snapshots++
			prefix = count
		}
		sequence.receiveBufferSettings.receiveOwnerStageForTest = func(_ *ReceiveSequence, _ *decodedPackOwner, stage string) {
			if stage != "pack_released" {
				return
			}
			turns++
			if turns > 6 {
				// Stop a broken unbounded implementation deterministically.
				sequence.Cancel()
				return
			}
			control := len(sequence.protocolPacks) == 2
			pack := receiveGapMailboxPackTest(sequence, uint64(turns+7), control)
			if admitted, err := sequence.Pack(pack, 0); !admitted || err != nil {
				pack.messagePoolReturn()
				refillFailed = true
			}
		}
		startReceiveGapHandoffTest(t, sequence)
		waitReceiveGapTestExit(t, sequence)
		entries := log.messages()
		if snapshots != 1 || prefix != 6 || turns != 6 || refillFailed || len(sequence.packs) != 3 || len(sequence.protocolPacks) != 3 ||
			sequence.packQueueCount.Load() != 3 || sequence.nextSequenceNumber != 0 || delivered.Load() != 0 || time.Now() != started ||
			len(entries) != 1 || !strings.Contains(entries[0], "exit gap timeout expected=0 queued=1") {
			t.Fatalf("mailbox prefix was refilled: snapshots=%d prefix=%d turns=%d refillFailed=%t data=%d protocol=%d retained=%d next=%d delivered=%d elapsed=%s logs=%v", snapshots, prefix, turns, refillFailed, len(sequence.packs), len(sequence.protocolPacks), sequence.packQueueCount.Load(), sequence.nextSequenceNumber, delivered.Load(), time.Since(started), entries)
		}
	})
}

// Cancellation at the census leaves both transferred mailbox owners intact
// for Close; it neither dispatches them nor emits an ACK for either packet.
func TestReceiveSequenceGapMailboxesCanceledCensusKeepsBothOwners(t *testing.T) {
	assertMessagePoolOwnership(t)
	sequence, log := receiveGapSequenceTest(t)
	t.Cleanup(sequence.Close)
	route := receiveGapMailboxAckRouteTest(t, sequence)
	var delivered atomic.Int32
	sequence.client.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) { delivered.Add(int32(len(frames))) })
	addReceiveGapTestItem(t, sequence, 2, time.Now().Add(-time.Hour), true)
	admitReceiveGapHandoffPackTest(t, sequence, receiveGapMailboxPackTest(sequence, 0, true))
	admitReceiveGapHandoffPackTest(t, sequence, receiveGapMailboxPackTest(sequence, 1, false))
	var snapshots, prefix int
	sequence.receiveBufferSettings.afterGapHandoffSnapshotForTest = func(_ receiveSequenceId, count int) {
		snapshots++
		prefix = count
		sequence.Cancel()
	}
	startReceiveGapHandoffTest(t, sequence)
	waitReceiveGapTestExit(t, sequence)
	if snapshots != 1 || prefix != 2 || delivered.Load() != 0 || sequence.nextSequenceNumber != 0 ||
		len(sequence.packs) != 1 || len(sequence.protocolPacks) != 1 || sequence.packQueueCount.Load() != 1 ||
		len(route) != 0 || len(log.messages()) != 0 {
		t.Fatalf("canceled census changed custody or ACK truth: snapshots=%d prefix=%d delivered=%d next=%d data=%d protocol=%d retained=%d acks=%d logs=%v", snapshots, prefix, delivered.Load(), sequence.nextSequenceNumber, len(sequence.packs), len(sequence.protocolPacks), sequence.packQueueCount.Load(), len(route), log.messages())
	}
}

// A context canceled after the channel transfer must return the newly taken
// root without receiving/ACKing it. Exit still delivers the earlier buffered
// head, preserving the existing deliver-then-die callback contract.
func TestReceiveSequenceGapMailboxesCancelDequeuedOwnerKeepsBufferedHead(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, control := range []bool{false, true} {
		sequence, log := receiveGapSequenceTest(t)
		t.Cleanup(sequence.Close)
		route := receiveGapMailboxAckRouteTest(t, sequence)
		var delivered atomic.Int32
		sequence.client.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) { delivered.Add(int32(len(frames))) })
		addReceiveGapTestItem(t, sequence, 0, time.Now().Add(-time.Hour), true)
		admitReceiveGapHandoffPackTest(t, sequence, receiveGapMailboxPackTest(sequence, 1, control))
		var turns, buffered int
		sequence.receiveBufferSettings.receiveOwnerStageForTest = func(_ *ReceiveSequence, _ *decodedPackOwner, stage string) {
			if stage == "pack_released" {
				turns++
				buffered = len(sequence.deliverItems)
				sequence.Cancel()
			}
		}
		startReceiveGapHandoffTest(t, sequence)
		waitReceiveGapTestExit(t, sequence)
		if turns != 1 || buffered != 1 || delivered.Load() != 1 || sequence.nextSequenceNumber != 1 ||
			len(sequence.packs) != 0 || len(sequence.protocolPacks) != 0 || sequence.packQueueCount.Load() != 0 ||
			len(route) != 0 || len(log.messages()) != 0 {
			t.Fatalf("dequeue cancellation changed buffered/new ownership: control=%t turns=%d buffered=%d delivered=%d next=%d data=%d protocol=%d retained=%d acks=%d logs=%v", control, turns, buffered, delivered.Load(), sequence.nextSequenceNumber, len(sequence.packs), len(sequence.protocolPacks), sequence.packQueueCount.Load(), len(route), log.messages())
		}
	}
}

// The protocol rendezvous has no DATA reservation. Its actual waiting Pack
// gets one nonblocking receive, just like the existing DATA rendezvous control.
func TestReceiveSequenceGapMailboxesReconcileReadyProtocolRendezvous(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		sequence, log := receiveGapSequenceTest(t)
		t.Cleanup(sequence.Close)
		sequence.protocolPacks = make(chan *ReceivePack)
		var delivered atomic.Int32
		sequence.client.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
			delivered.Add(int32(len(frames)))
			sequence.Cancel()
		})
		addReceiveGapTestItem(t, sequence, 1, time.Now().Add(-time.Hour), true)
		pack := receiveGapMailboxPackTest(sequence, 0, true)
		type admissionResult struct {
			admitted bool
			err      error
		}
		result := make(chan admissionResult, 1)
		joined := false
		t.Cleanup(func() {
			sequence.Cancel()
			if !joined {
				<-result
			}
		})
		go func() {
			admitted, err := sequence.Pack(pack, -1)
			if !admitted {
				pack.messagePoolReturn()
			}
			result <- admissionResult{admitted: admitted, err: err}
		}()
		synctest.Wait()
		select {
		case outcome := <-result:
			joined = true
			t.Fatalf("protocol producer did not wait at its real rendezvous: %+v", outcome)
		default:
		}
		if sequence.packQueueCount.Load() != 0 || len(sequence.protocolPacks) != 0 {
			t.Fatal("protocol rendezvous acquired DATA custody or became buffered")
		}
		startReceiveGapHandoffTest(t, sequence)
		waitReceiveGapTestExit(t, sequence)
		admission := <-result
		joined = true
		if !admission.admitted || admission.err != nil || delivered.Load() != 2 || sequence.nextSequenceNumber != 2 || len(log.messages()) != 0 {
			t.Fatalf("protocol rendezvous lost its predecessor: admitted=%t err=%v delivered=%d next=%d logs=%v", admission.admitted, admission.err, delivered.Load(), sequence.nextSequenceNumber, log.messages())
		}
	})
}

// A real pending provider/TCP receipt puts Run in waitDelivery. Cancel exactly
// after that wait has taken the next Pack and released its handoff reservation,
// before Run decides whether to exit. The newly transferred owner must return;
// no callback or ACK can be invented for either unsecured original.
func TestReceiveSequenceGapMailboxesCancelAfterDeliveryWaitReturnsOwner(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		for _, control := range []bool{false, true} {
			fixture := newReceiptAdmissionFixture(t, 1)
			firstId, nextId := NewId(), NewId()
			fixture.offer(fixture.sequenceId, 0, 0, firstId, 41000, tcpFlagAck, []byte("synthetic pending predecessor"))
			sequence := fixture.sequence(fixture.sequenceId, 0)
			if sequence == nil || sequence.deliveryQueue == nil || !sequence.deliveryQueue.pending() ||
				sequence.nextSequenceNumber != 1 || len(fixture.acks) != 0 || fixture.receiveBudget.UsedByteCount() == 0 {
				t.Fatal("post-wait cancellation did not establish its real pending TCP receipt")
			}
			var taken int
			var pending, released bool
			sequence.receiveBufferSettings.receiveOwnerStageForTest = func(actual *ReceiveSequence, _ *decodedPackOwner, stage string) {
				if actual == sequence && stage == "delivery_wait_pack" {
					taken++
					pending = actual.deliveryQueue.pending()
					released = actual.packQueueCount.Load() == 0 && fixture.packBudget.UsedByteCount() == 0
					actual.Cancel()
				}
			}
			payload := []byte("synthetic next DATA")
			if control {
				payload = nil
			}
			var nextPack *ReceivePack
			fixture.offer(fixture.sequenceId, 0, 1, nextId, 41001, tcpFlagAck, payload, func(pack *ReceivePack) {
				nextPack = pack
			})
			if taken != 1 || !pending || !released {
				t.Fatalf("cancellation missed the actual post-wait custody boundary: control=%t taken=%d pending=%t released=%t", control, taken, pending, released)
			}
			select {
			case <-sequence.done:
			default:
				t.Fatal("post-wait cancellation did not join its actual receive sequence")
			}
			if len(fixture.witnesses) != 2 || nextPack == nil {
				t.Fatal("post-wait cancellation lost the exact offered packet witness")
			}
			// The witness is the only remaining share after a correct return.
			// An old failing donor leaked this exact Pack; reclaim it only after
			// the owner joined, retaining the failure without poisoning later roots.
			returned := MessagePoolReturn(fixture.witnesses[1])
			fixture.witnesses = fixture.witnesses[:1]
			if !returned {
				nextPack.messagePoolReturn()
				t.Error("POST_WAIT_CANCELED_PACK_OWNER_LOST: the dequeued Pack outlived its joined receive sequence")
			}
			if sequence.nextSequenceNumber != 1 || fixture.delivered(41001) != 0 || len(fixture.acks) != 0 ||
				sequence.packQueueCount.Load() != 0 || fixture.packBudget.UsedByteCount() != 0 || fixture.receiveBudget.UsedByteCount() != 0 ||
				sequence.deliveryQueue.pending() || len(sequence.deliveryQueue.operations) != 0 {
				t.Fatalf("post-wait cancellation changed delivery/ACK or budget truth: control=%t next=%d callbacks=%d acks=%d handoff=%d pack_bytes=%d receive_bytes=%d", control, sequence.nextSequenceNumber, fixture.delivered(41001), len(fixture.acks), sequence.packQueueCount.Load(), fixture.packBudget.UsedByteCount(), fixture.receiveBudget.UsedByteCount())
			}
			if fixture.client.Ctx().Err() != nil || fixture.provider.ctx.Err() != nil || fixture.nat.ctx.Err() != nil {
				t.Fatal("post-wait sequence cancellation closed a shared owner")
			}
		}
	})
}
