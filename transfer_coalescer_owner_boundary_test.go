// Real Client wire rounds preserve logical-group disposition and prepaid
// ownership when the ordinary ready drain encounters a different owner kind.
package connect

import (
	"bytes"
	"context"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/protocol"
)

// Uses the native admission fixture's pre-Run barrier and an actual receiving
// Client. Only the test owner forwards wire shares; no ACK is fabricated.
type coalescerOwnerFixture struct {
	t              *testing.T
	source         *groupDispositionQueueFixture
	receiver       *Client
	receiverIn     Route
	receiverOut    Route
	senderIn       Route
	expected       [][]byte
	delivered      [][]byte
	physicalFrames []int
	sequenceId     []byte
	ordinaryAcks   []error
}

// All unrelated publishers are parked before construction, and every cleanup
// cancels their owner before joining either real Client and draining its wires.
func newCoalescerOwnerFixture(t *testing.T, budget *TransferMemoryBudget) *coalescerOwnerFixture {
	t.Helper()
	f := &coalescerOwnerFixture{t: t, receiverIn: make(Route, 16),
		receiverOut: make(Route, 16), senderIn: make(Route, 16)}
	f.source = newGroupDispositionQueueFixture(t, budget, func(settings *ClientSettings) {
		settings.SendBufferSettings.WindowSizing = WindowSizingConstant
		settings.SendBufferSettings.ApplyWindowSizing()
		settings.SendBufferSettings.SequenceBufferSize = 8
	})
	ctx, cancel := context.WithCancel(context.Background())
	settings := closeWaitClientSettings()
	settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
	settings.EncryptionSettings.beforeEncryptedKeyPublishForTest = func() { <-ctx.Done() }
	settings.ReceiveBufferSettings.WindowSizing = WindowSizingConstant
	settings.ReceiveBufferSettings.ApplyWindowSizing()
	settings.ReceiveBufferSettings.AckCompressTimeout = 0
	f.receiver = NewClient(ctx, f.source.peer, NewNoContractClientOob(), settings)
	f.receiver.ContractManager().AddNoContractPeer(f.source.client.ClientId())
	f.receiver.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{f.receiverIn})
	f.receiver.RouteManager().UpdateTransport(NewSendGatewayTransport(), []Route{f.receiverOut})
	f.source.client.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{f.senderIn})
	f.receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
		for _, frame := range frames {
			f.delivered = append(f.delivered, bytes.Clone(frame.MessageBytes))
		}
	})
	t.Cleanup(func() {
		cancel()
		if err := f.source.closeClient(context.Background()); err != nil {
			t.Errorf("join coalescer source: %v", err)
		}
		if err := f.receiver.CloseAndWait(context.Background()); err != nil {
			t.Errorf("join coalescer receiver: %v", err)
		}
		for _, route := range []Route{f.receiverIn, f.receiverOut, f.senderIn} {
			for len(route) != 0 {
				MessagePoolReturn(<-route)
			}
		}
	})
	return f
}

// Enqueues a caller-owned ordinary payload, returning it only on refusal.
func (self *coalescerOwnerFixture) sendOrdinary(size int, options ...any) {
	self.t.Helper()
	frame := budgetTestFrame(size)
	self.expected = append(self.expected, bytes.Clone(frame.MessageBytes))
	accepted, err := self.source.client.SendWithTimeoutDetailed(frame, self.source.peer,
		func(err error) { self.ordinaryAcks = append(self.ordinaryAcks, err) }, 0, options...)
	if !accepted || err != nil {
		MessagePoolReturn(frame.MessageBytes)
		self.t.Fatalf("ordinary admission: accepted=%t err=%v", accepted, err)
	}
}

// Drains only already-ready wires, using real receive delivery and its actual
// ACK route. The finite round bound catches a spinning fixture or production.
func (self *coalescerOwnerFixture) deliver() {
	self.t.Helper()
	for range 16 {
		synctest.Wait()
		if len(self.source.route) == 0 && len(self.receiverOut) == 0 {
			return
		}
		for len(self.source.route) != 0 {
			func() {
				wire := <-self.source.route
				defer MessagePoolReturn(wire)
				pack := decodeSendPackLifecycleWirePack(self.t, wire)
				if self.sequenceId == nil {
					self.sequenceId = bytes.Clone(pack.SequenceId)
				} else if !bytes.Equal(self.sequenceId, pack.SequenceId) {
					self.t.Fatal("queued owners escaped onto different send sequences")
				}
				self.physicalFrames = append(self.physicalFrames, len(pack.Frames))
				self.receiverIn <- MessagePoolShareReadOnly(wire)
				synctest.Wait()
			}()
		}
		for len(self.receiverOut) != 0 {
			self.senderIn <- <-self.receiverOut
			synctest.Wait()
		}
	}
	self.t.Fatal("coalescer wire rounds did not quiesce")
}

// Payload and callback oracles are separate from batching and collapse state.
func (self *coalescerOwnerFixture) requireDelivery(ordinaryAcks int) {
	self.t.Helper()
	if len(self.delivered) != len(self.expected) {
		self.t.Fatalf("peer delivered %d payloads, want %d", len(self.delivered), len(self.expected))
	}
	for index, expected := range self.expected {
		if !bytes.Equal(self.delivered[index], expected) {
			self.t.Fatalf("peer payload %d changed or moved out of order", index)
		}
	}
	if len(self.ordinaryAcks) != ordinaryAcks {
		self.t.Fatalf("ordinary callbacks=%d, want %d", len(self.ordinaryAcks), ordinaryAcks)
	}
	for index, err := range self.ordinaryAcks {
		if err != nil {
			self.t.Fatalf("ordinary callback %d failed: %v", index, err)
		}
	}
}

// The queued native group fits the ordinary two-frame coalescing limit. Its
// actual peer ACK must not erase the durable duplicate gate by bypassing the
// group's materialization disposition; reversing selection stays supported.
func testCoalescerTrackedGroup(t *testing.T, groupFirst bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newCoalescerOwnerFixture(t, nil)
		packet := groupRecoveryPacket(100, 500, 64, tcpFlagAck, []byte("synthetic coalescer payload"))
		if !groupFirst {
			f.sendOrdinary(80)
		}
		f.expected = append(f.expected, bytes.Clone(packet))
		if f.source.offer(packet) != 1 {
			t.Fatal("initial native group admission refused")
		}
		if groupFirst {
			f.sendOrdinary(80)
		}
		f.source.requireParked()
		f.source.unpark()
		f.deliver()
		f.requireDelivery(1)
		if f.source.terminal.Load() != 2 {
			t.Fatal("original native and ordinary owners did not both reach terminal completion")
		}
		accepted := f.source.offer(packet)
		f.deliver()
		if accepted != 0 || len(f.delivered) != 2 {
			t.Fatalf("ordinary coalescing lost native collapse after real peer ACK: groupFirst=%t retry admitted=%d peer payloads=%d", groupFirst, accepted, len(f.delivered))
		}
	})
}

// Forces the pre-fix ordinary-first disposition bypass at the public gate.
func TestTransferCoalescerTrackedGroupRetainsCollapse(t *testing.T) {
	testCoalescerTrackedGroup(t, false)
}

// The existing logical-group-first path keeps payload, ACK and collapse.
func TestTransferCoalescerTrackedGroupFirstPreservesDelivery(t *testing.T) {
	testCoalescerTrackedGroup(t, true)
}

// A generic single-frame group has one completion, regardless of queue order;
// it cannot be mistaken for a partially processed logical cursor.
func TestTransferCoalescerGenericGroupCompletion(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, groupFirst := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			f := newCoalescerOwnerFixture(t, nil)
			if !groupFirst {
				f.sendOrdinary(80)
			}
			frame := budgetTestFrame(96)
			f.expected = append(f.expected, bytes.Clone(frame.MessageBytes))
			callbacks := []error{}
			accepted, err := f.source.client.sendGroupWithTimeoutDetailed([]*protocol.Frame{frame}, f.source.peer,
				func(err error) { callbacks = append(callbacks, err) }, 0)
			if !accepted || err != nil {
				MessagePoolReturn(frame.MessageBytes)
				t.Fatalf("generic group admission: accepted=%t err=%v", accepted, err)
			}
			if groupFirst {
				f.sendOrdinary(80)
			}
			f.source.requireParked()
			f.source.unpark()
			f.deliver()
			f.requireDelivery(1)
			if len(callbacks) != 1 || callbacks[0] != nil {
				t.Fatalf("groupFirst=%t generic completion=%v", groupFirst, callbacks)
			}
		})
	}
}

// The owner boundary does not disable ordinary ready batching or combine
// callbacks: both queued ordinary frames retain one actual physical write.
func TestTransferCoalescerOrdinaryReadyBatch(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newCoalescerOwnerFixture(t, nil)
		f.sendOrdinary(80)
		f.sendOrdinary(96)
		f.source.requireParked()
		f.source.unpark()
		f.deliver()
		f.requireDelivery(2)
		if len(f.physicalFrames) != 1 || f.physicalFrames[0] != 2 {
			t.Fatalf("ordinary ready batching changed: physical frame counts=%v", f.physicalFrames)
		}
	})
}

// Spare capacity used to let the ready drain form an unfundable multi-owner
// item. Full-root rows also prove two prepaid owners and ordinary no-ACK
// traffic progress without borrowing or releasing either recovery envelope.
func TestTransferCoalescerRecyclableOwnersProgress(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, full := range []bool{false, true} {
		for _, ordinaryFirst := range []bool{false, true} {
			synctest.Test(t, func(t *testing.T) {
				total := kib(64)
				if full {
					total = kib(32)
				}
				root := NewTransferMemoryBudget(total)
				budget := NewTransferMemoryBudgetWithParent(total, root)
				f := newCoalescerOwnerFixture(t, budget)
				var owners [2]*recyclableMemoryTestOwner
				for index := range owners {
					credit, ok := prepareRecyclableSendMemory(budget, kib(16), kib(1))
					if !ok {
						t.Fatal("independent recovery envelope admission")
					}
					defer credit.release()
					owners[index] = &recyclableMemoryTestOwner{credit: credit, expected: kib(16)}
				}
				ordinaryOptions := []any{}
				if full {
					ordinaryOptions = append(ordinaryOptions, NoAck())
				}
				if ordinaryFirst {
					f.sendOrdinary(80, ordinaryOptions...)
				}
				for index, owner := range owners {
					frame := budgetTestFrame(96 + index)
					f.expected = append(f.expected, bytes.Clone(frame.MessageBytes))
					accepted, err := f.source.client.SendWithTimeoutDetailed(frame, f.source.peer, nil, 0,
						sendAckTargetOption{target: owner},
						sendPackRecoveryOption{upstreamRecoverable: true, retainAfterAckTimeout: true})
					if !accepted || err != nil {
						MessagePoolReturn(frame.MessageBytes)
						t.Fatalf("owner %d admission: accepted=%t err=%v", index, accepted, err)
					}
				}
				if !ordinaryFirst {
					f.sendOrdinary(80, ordinaryOptions...)
				}
				f.source.requireParked()
				f.source.unpark()
				f.deliver()
				for index, owner := range owners {
					if owner.acks != 1 || owner.err != nil || !owner.recycledAtAck {
						t.Fatalf("full=%t ordinaryFirst=%t owner=%d callbacks=%d err=%v recycled=%t", full, ordinaryFirst, index, owner.acks, owner.err, owner.recycledAtAck)
					}
				}
				f.requireDelivery(1)
				if budget.UsedByteCount() != kib(32) || root.UsedByteCount() != kib(32) {
					t.Fatal("mixed actual ACKs borrowed or released recovery ownership")
				}
				owners[0].credit.release()
				if owners[1].credit.bytes != kib(16) || root.UsedByteCount() != kib(16) {
					t.Fatal("one owner cancellation changed the other envelope")
				}
				owners[1].credit.release()
				assertRetainedBudgetBalance(t, budget)
				assertRetainedBudgetBalance(t, root)
			})
		}
	}
}
