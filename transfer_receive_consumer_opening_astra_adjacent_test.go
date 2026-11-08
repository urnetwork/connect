// Opening recovery inherits a consumer role only from an actual paid local
// owner. Exact routing and closure controls keep that physical grant bounded.
package connect

import (
	"bytes"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// Every queue entry is created by the public signed wire path. Only the
// incoming control's physical decode is held at a worker-free test boundary.
type consumerOpeningAdjacentFixture struct {
	fixture           *windowRoundFixture
	root, rx, child   *TransferMemoryBudget
	sequence, foreign *ReceiveSequence
	original          *receiveItem
	opening           *windowRoundFrame
	lastApplication   map[uint32]uint64
}

// Selects the real receiver from its captured source wire, without installing
// a sequence id, routing key, contract or current-head value.
func findConsumerAdjacentSequence(t *testing.T, fixture *windowRoundFixture, frame *windowRoundFrame) *ReceiveSequence {
	t.Helper()
	buffer := fixture.receiver.receiveBuffer
	buffer.mutex.Lock()
	defer buffer.mutex.Unlock()
	for id, sequence := range buffer.receiveSequences {
		if id.SequenceId == RequireIdFromBytes(frame.pack.SequenceId) && id.LogicalLane == frame.pack.LogicalLane {
			return sequence
		}
	}
	t.Fatal("consumer adjacent source has no actual receive sequence")
	return nil
}

// Foreign custody consumes 5120 before the target's genuine first application
// consumes 1536. The target's original empty opening remains off the receiver.
func newConsumerOpeningAdjacentFixture(t *testing.T, kind protocol.MessageType) *consumerOpeningAdjacentFixture {
	t.Helper()
	assertMessagePoolOwnership(t)
	root := NewTransferMemoryBudget(2 * 6656)
	rx := NewTransferMemoryBudgetWithParent(6656, root)
	child := NewTransferMemoryBudgetWithParent(6656, root)
	t.Cleanup(func() {
		for _, budget := range []*TransferMemoryBudget{rx, child, root} {
			assertRetainedBudgetBalance(t, budget)
			if budget.Stats().QuotaByteCount != 0 {
				t.Error("opening adjacent cleanup retained logical quota")
			}
		}
	})
	fixture, _ := newConsumerTentativeHeadFixture(t, rx, child)
	last := map[uint32]uint64{}
	foreignHead := writeConsumerFutureHeldData(t, fixture, 1, 1280, last)
	foreignTail := writeConsumerFutureHeldData(t, fixture, 1, 1280, last)
	foreign := findConsumerAdjacentSequence(t, fixture, foreignHead)
	fixture.forward(foreignTail, fixture.receiverIn)
	if item := foreign.receiveQueue.GetByMessageId(RequireIdFromBytes(foreignTail.pack.MessageId)); item == nil ||
		item.committed || item.QueueByteCount() != 5120 || foreign.holdPromise.set || rx.UsedByteCount() != 5120 {
		t.Fatal("opening adjacent foreign owner premise failed")
	}
	var packet []byte
	if kind == protocol.MessageType_IpIpPacketToProvider {
		// A legal provider UDP packet isolates the absent consumer role.
		// TCP would additionally need downstream custody before this hold.
		packet = ipOosUdpPacket(udpTestPath(4), bytes.Repeat([]byte{0x45}, 44))
	} else {
		packet = ipOosTcpPacketSequence(reliableDeliveryTestPath(4), tcpFlagAck, 31, bytes.Repeat([]byte{0x45}, 32))
	}
	frame := &protocol.Frame{MessageType: kind, Raw: true, MessageBytes: packet}
	accepted, err := fixture.sender.SendWithTimeoutDetailed(frame, fixture.receiver.ClientId(), func(err error) {
		if err == nil {
			fixture.ackedCount++
		}
	}, time.Second, TransferKey{LogicalLane: 0})
	if !accepted || err != nil {
		MessagePoolReturn(frame.MessageBytes)
		t.Fatal("opening adjacent target send failed")
	}
	var opening, tail *windowRoundFrame
	for range cap(fixture.senderOut) {
		wire := fixture.take(fixture.senderOut)
		if wire.pack == nil || wire.pack.LogicalLane != 0 {
			continue
		}
		if wire.pack.Head && wire.pack.SequenceNumber == 0 && len(wire.pack.Frames) == 0 && wire.pack.ContractFrame != nil && !wire.pack.ContractAhead {
			if opening == nil {
				opening = wire
			}
		} else if len(wire.pack.Frames) == 1 && wire.pack.Frames[0].MessageType == kind {
			tail = wire
			break
		}
	}
	if opening == nil || tail == nil || tail.pack.SequenceNumber != 1 || tail.pack.Head || tail.pack.ContractFrame != nil ||
		!bytes.Equal(tail.pack.SequenceId, opening.pack.SequenceId) {
		t.Fatal("opening adjacent actual control/data premise failed")
	}
	last[0] = tail.pack.SequenceNumber
	path := TransferPath{SourceId: fixture.sender.ClientId(), DestinationId: fixture.receiver.ClientId()}
	scan, status := scanReceiveMemoryPack(tail.bytes, path)
	if status != receiveMemoryPackScanReady || scan.retainedByteCount != 1536 {
		t.Fatalf("opening adjacent target physical cost premise failed: kind=%v scan_status=%v retained=%d", kind, status, scan.retainedByteCount)
	}
	fixture.forward(tail, fixture.receiverIn)
	sequence := findConsumerAdjacentSequence(t, fixture, tail)
	original := sequence.receiveQueue.GetByMessageId(RequireIdFromBytes(tail.pack.MessageId))
	if original == nil || original.committed || original.QueueByteCount() != 1536 || original.decodedOwner == nil ||
		original.memoryBudget != rx || !original.decodedOwner.receivePack.receiveMemoryOwned ||
		original.decodedOwner.receivePack.ingressSequence != sequence ||
		original.decodedOwner.receivePack.receiveConsumerData != (kind == protocol.MessageType_IpIpPacketFromProvider) ||
		sequence.receiveContract != nil || sequence.nextSequenceNumber != 0 || sequence.holdPromise.set ||
		sequence.holdPromise.reservedByteCount != 0 || rx.UsedByteCount() != 6656 || root.UsedByteCount() != 6656 ||
		child.UsedByteCount() != 0 || len(fixture.receiverOut) != 0 || fixture.deliveredCount != 0 || fixture.ackedCount != 0 {
		originalBytes, hasOwner, consumer, committed := ByteCount(0), false, false, false
		if original != nil {
			originalBytes, hasOwner, committed = original.QueueByteCount(), original.decodedOwner != nil, original.committed
			if hasOwner {
				consumer = original.decodedOwner.receivePack.receiveConsumerData
			}
		}
		t.Fatalf("opening adjacent actual paid role/full-root premise failed: kind=%v original=%t owner=%t consumer=%t committed=%t original_bytes=%d rx=%d root=%d child=%d queued=%d next=%d promised=%t reserved=%d ack_wires=%d delivered=%d acked=%d",
			kind, original != nil, hasOwner, consumer, committed, originalBytes, rx.UsedByteCount(), root.UsedByteCount(), child.UsedByteCount(),
			sequence.receiveQueue.Len(), sequence.nextSequenceNumber, sequence.holdPromise.set, sequence.holdPromise.reservedByteCount,
			len(fixture.receiverOut), fixture.deliveredCount, fixture.ackedCount)
	}
	return &consumerOpeningAdjacentFixture{fixture: fixture, root: root, rx: rx, child: child,
		sequence: sequence, foreign: foreign, original: original, opening: opening, lastApplication: last}
}

// Test-owned input is copied once. Full decodes retain that paid carrier;
// proof-only copies release it and keep only their logical ingress byte count.
func decodeConsumerAdjacentOwner(t *testing.T, fixture *windowRoundFixture, sequence *ReceiveSequence, source []byte) *ReceivePack {
	t.Helper()
	wire := MessagePoolCopy(source)
	path := TransferPath{SourceId: fixture.sender.ClientId(), DestinationId: fixture.receiver.ClientId()}
	decoded := &decodedTransferFrame{}
	ok, pressure := fixture.receiver.unmarshalReceiveMemoryFrame(wire, decoded, true, path, false, sequenceTlsRoleServer, false)
	if !ok || pressure {
		decoded.release()
		MessagePoolReturn(wire)
		t.Fatalf("bounded consumer adjacent decode refused: ok=%t pressure=%t", ok, pressure)
	}
	owner := decoded.detachPackOwner()
	decoded.release()
	if owner == nil {
		MessagePoolReturn(wire)
		t.Fatal("bounded consumer adjacent decode has no owner")
	}
	pack := &owner.receivePack
	pack.Source, pack.SequenceId = sequence.source, sequence.sequenceId
	pack.ReceiveCallback, pack.Unwrapped, pack.TransportType = fixture.receiver.receiveCallback, true, TransportTypeH1
	if sequence.receiveHeldHeadRepairCopy(pack) {
		pack.sequenceQueueByteCount = ByteCount(len(wire))
		MessagePoolReturn(wire)
	} else {
		pack.TransferFrameBytes = wire
		pack.MessageByteCount = MessageByteCount(pack.Pack.Frames)
	}
	if !sequence.bindReceiveMemoryOwner(pack) {
		pack.messagePoolReturn()
		t.Fatal("bounded consumer adjacent owner failed actual generation binding")
	}
	return pack
}

// Decode refusal must neither allocate a child owner nor change paid custody.
// Source-path mutants use their presented source while destination stays local.
func refuseConsumerAdjacentWire(t *testing.T, fixture *windowRoundFixture, root, child *TransferMemoryBudget, wire []byte, label string) {
	t.Helper()
	path, err := FilteredTransferPath(wire)
	if err != nil {
		t.Fatal(err)
	}
	path.DestinationId = fixture.receiver.ClientId()
	reserved, released := root.Counts()
	used, childUsed := root.UsedByteCount(), child.UsedByteCount()
	decoded := &decodedTransferFrame{}
	ok, _ := fixture.receiver.unmarshalReceiveMemoryFrame(wire, decoded, true, path, false, sequenceTlsRoleServer, false)
	decoded.release()
	synctest.Wait()
	afterReserved, afterReleased := root.Counts()
	if ok || root.UsedByteCount() != used || child.UsedByteCount() != childUsed || reserved != afterReserved || released != afterReleased {
		t.Fatalf("%s acquired physical recovery or changed existing claims", label)
	}
}

// Empty control cannot infer a consumer role from another lane or from a
// same-lane provider owner. Returning the only local consumer removes its role.
func TestReceiveConsumerOpeningAdjacentRequiresLiveLocalConsumer(t *testing.T) {
	GetMessagePoolAggregateStats()
	for _, removeConsumer := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			kind := protocol.MessageType_IpIpPacketToProvider
			if removeConsumer {
				kind = protocol.MessageType_IpIpPacketFromProvider
			}
			f := newConsumerOpeningAdjacentFixture(t, kind)
			if removeConsumer {
				if f.sequence.receiveQueue.RemoveByMessageId(f.original.messageId) != f.original {
					t.Fatal("controlled original return lost its owner")
				}
				f.original.messagePoolReturn()
				foreign := writeConsumerFutureHeldData(t, f.fixture, 1, 32, f.lastApplication)
				f.fixture.forward(foreign, f.fixture.receiverIn)
				if f.rx.UsedByteCount() != 6656 || f.sequence.holdPromise.ingressHead != nil {
					t.Fatal("returned local owner was not replaced by actual foreign custody")
				}
			}
			refuseConsumerAdjacentWire(t, f.fixture, f.root, f.child, f.opening.bytes, "absent local consumer role")
			if f.sequence.nextSequenceNumber != 0 || f.sequence.receiveContract != nil || len(f.fixture.receiverOut) != 0 {
				t.Fatal("absent local role acquired opening authority")
			}
		})
	}
}

// Routing and control shape are fixed before any child allocation. Opening
// identity is not guessed: its message id becomes known only on this arrival.
func TestReceiveConsumerOpeningAdjacentIdentityAndControlExclusions(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		f := newConsumerOpeningAdjacentFixture(t, protocol.MessageType_IpIpPacketFromProvider)
		var original protocol.TransferFrame
		if err := proto.Unmarshal(f.opening.bytes, &original); err != nil {
			t.Fatal(err)
		}
		rows := []struct {
			name   string
			change func(*protocol.TransferFrame)
		}{
			{name: "source", change: func(v *protocol.TransferFrame) { v.TransferPath.SourceId = NewId().Bytes() }},
			{name: "destination", change: func(v *protocol.TransferFrame) { v.TransferPath.DestinationId = NewId().Bytes() }},
			{name: "generation", change: func(v *protocol.TransferFrame) { v.Pack.SequenceId = NewId().Bytes() }},
			{name: "lane", change: func(v *protocol.TransferFrame) { v.Pack.LogicalLane = 1 }},
			{name: "stream", change: func(v *protocol.TransferFrame) { v.Pack.ForceStream = !v.Pack.ForceStream }},
			{name: "contract companion", change: func(v *protocol.TransferFrame) { v.Pack.CompanionContract = !v.Pack.CompanionContract }},
			{name: "session role", change: func(v *protocol.TransferFrame) {
				role := protocol.SequenceRole_SequenceRoleServer
				v.SessionRole = &role
			}},
			{name: "unsupported role", change: func(v *protocol.TransferFrame) { role := protocol.SequenceRole(99); v.SessionRole = &role }},
			{name: "session companion", change: func(v *protocol.TransferFrame) { value := true; v.SessionCompanion = &value }},
			{name: "future number", change: func(v *protocol.TransferFrame) { v.Pack.SequenceNumber++ }},
			{name: "non-head", change: func(v *protocol.TransferFrame) { v.Pack.Head = false }},
			{name: "no ack", change: func(v *protocol.TransferFrame) { v.Pack.Nack = true }},
			{name: "ahead", change: func(v *protocol.TransferFrame) { v.Pack.ContractAhead = true }},
			{name: "missing proof", change: func(v *protocol.TransferFrame) { v.Pack.ContractFrame = nil }},
			{name: "wrong proof type", change: func(v *protocol.TransferFrame) {
				v.Pack.ContractFrame.MessageType = protocol.MessageType_TransferEncryptedControl
			}},
			{name: "encrypted control", change: func(v *protocol.TransferFrame) {
				v.Pack.Frames = []*protocol.Frame{{MessageType: protocol.MessageType_TransferEncryptedControl, MessageBytes: []byte{1}}}
			}},
		}
		for _, row := range rows {
			frame := proto.Clone(&original).(*protocol.TransferFrame)
			row.change(frame)
			wire, err := ProtoMarshal(frame)
			if err != nil {
				t.Fatal(err)
			}
			func() {
				defer MessagePoolReturn(wire)
				refuseConsumerAdjacentWire(t, f.fixture, f.root, f.child, wire, row.name)
			}()
		}
		if f.sequence.receiveQueue.GetByMessageId(f.original.messageId) != f.original || f.original.committed ||
			f.sequence.nextSequenceNumber != 0 || f.sequence.receiveContract != nil || len(f.fixture.receiverOut) != 0 {
			t.Fatal("excluded opening changed original custody or authority")
		}
	})
}

// A paid in-flight opening excludes another copy. Cancellation returns its
// retained tail, while the child claim survives until the decoder owner returns.
func TestReceiveConsumerOpeningAdjacentDuplicateCloseAndRefund(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		f := newConsumerOpeningAdjacentFixture(t, protocol.MessageType_IpIpPacketFromProvider)
		pack := decodeConsumerAdjacentOwner(t, f.fixture, f.sequence, f.opening.bytes)
		defer func() {
			if pack != nil {
				pack.messagePoolReturn()
			}
		}()
		cost := pack.decodedOwner.receiveItem.QueueByteCount()
		if pack.decodedOwner.receiveItem.memoryBudget != f.child || f.child.UsedByteCount() != cost ||
			f.rx.UsedByteCount() != 6656 || f.root.UsedByteCount() != 6656+cost || pack.ingressLoanByteCount != 0 {
			t.Fatal("opening copy did not retain its whole independent child claim")
		}
		refuseConsumerAdjacentWire(t, f.fixture, f.root, f.child, f.opening.bytes, "occupied opening identity")
		f.sequence.Cancel()
		<-f.sequence.done
		synctest.Wait()
		path := TransferPath{SourceId: f.fixture.sender.ClientId(), DestinationId: f.fixture.receiver.ClientId()}
		scan, status := scanReceiveMemoryPack(f.opening.bytes, path)
		if status != receiveMemoryPackScanReady {
			t.Fatal("opening control lost its scan")
		}
		if owner := f.fixture.receiver.receiveBuffer.takeReceiveHeadRecoveryOwner(f.sequence, scan); owner != nil {
			owner.release()
			t.Fatal("closed generation acquired another child owner")
		}
		if f.rx.UsedByteCount() != 5120 || f.child.UsedByteCount() != cost || f.root.UsedByteCount() != 5120+cost ||
			f.sequence.bindReceiveMemoryOwner(pack) || f.fixture.deliveredCount != 0 || f.fixture.ackedCount != 0 {
			t.Fatal("closure released live opening bytes or accepted a closed generation")
		}
		pack.messagePoolReturn()
		pack = nil
		if f.child.UsedByteCount() != 0 || f.root.UsedByteCount() != 5120 || f.sequence.holdPromise.ingressHead != nil {
			t.Fatal("last opening return did not refund only its child")
		}
	})
}
