// Existing-sequence recovery inherits a consumer role from a paid local owner.
// An absent context can instead prepay its own full-proof opening, without
// borrowing the old context's identity, held custody or verification authority.
package connect

import (
	"bytes"
	"maps"
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
	packMessage := decoded.frame.Pack
	owner := decoded.detachPackOwner()
	decoded.release()
	if owner == nil {
		MessagePoolReturn(wire)
		t.Fatal("bounded consumer adjacent decode has no owner")
	}
	pack := &owner.receivePack
	pack.Pack, pack.decodedOwner = packMessage, owner
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
	refuseConsumerAdjacentWireAtPath(t, fixture, root, child, wire, path, label)
}

func refuseConsumerAdjacentWireAtPath(t *testing.T, fixture *windowRoundFixture, root, child *TransferMemoryBudget, wire []byte, path TransferPath, label string) {
	t.Helper()
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

// These table rows name another absent context, not an identity entitled to
// the original's copied proof or prefix. A successful decode must pay the whole
// C independently, remain unbound, reject binding to the old sequence, and
// return C exactly. It must not create a worker or publish any authority.
func requireConsumerAdjacentFreshPhysical(t *testing.T, fixture *windowRoundFixture, old *ReceiveSequence, root, child *TransferMemoryBudget, wire []byte, label string) {
	t.Helper()
	path, err := FilteredTransferPath(wire)
	if err != nil {
		t.Fatal(err)
	}
	scan, status := scanReceiveMemoryPack(wire, path)
	if status != receiveMemoryPackScanReady || !scan.pathMatches || scan.retainedByteCount <= 0 {
		t.Fatalf("%s has no exact independently payable decode envelope", label)
	}
	role, companion := sequenceTlsRoleServer, false
	if scan.sessionRoleSet {
		senderRole, valid := sequenceTlsRoleFromProtobuf(protocol.SequenceRole(scan.sessionRole))
		if !valid {
			t.Fatal("fresh-context control must retain a supported wire role")
		}
		role = senderRole.complement()
	}
	if scan.sessionCompanionSet {
		companion = scan.sessionCompanion
	}
	id := receiveSequenceId{Source: path.SourceMask(), SequenceId: scan.sequenceId,
		EncryptionRole: role, EncryptionCompanion: companion, LogicalLane: scan.logicalLane}
	buffer := fixture.receiver.receiveBuffer
	buffer.mutex.Lock()
	sequences, heads, rejected := maps.Clone(buffer.receiveSequences), maps.Clone(buffer.headReceiveSequenceIds), maps.Clone(buffer.rejectedReceiveSequenceIds)
	buffer.mutex.Unlock()
	if sequences[id] != nil {
		t.Fatalf("%s does not select a genuinely absent receive context", label)
	}
	type claims struct {
		promise  receiveHoldPromise
		repair   receiveHeldHeadRepair
		pressure receiveMemoryPressure
	}
	prior := make(map[*ReceiveSequence]claims, len(sequences))
	root.admissionLock.Lock()
	for _, sequence := range sequences {
		prior[sequence] = claims{sequence.holdPromise, sequence.heldHeadRepair, sequence.memoryPressure}
	}
	root.admissionLock.Unlock()
	rx := fixture.receiver.settings.ReceiveBufferSettings.ReceiveQueueBudget
	before := root.StatsWithDescendants(rx, child)
	delivered, acked, ackWires := fixture.deliveredCount, fixture.ackedCount, len(fixture.receiverOut)
	decoded := &decodedTransferFrame{}
	defer decoded.release()
	ok, pressure := fixture.receiver.unmarshalReceiveMemoryFrame(wire, decoded, true, path, false, sequenceTlsRoleServer, false)
	owner := decoded.packOwner
	if !ok || pressure || owner == nil || !owner.inUse || !owner.receiveItem.receiveFreshOpening ||
		!owner.receivePack.receiveMemoryOwned || owner.receivePack.ingressSequence != nil ||
		owner.receivePack.ingressLoanByteCount != 0 || owner.receiveItem.memoryBudget != child ||
		owner.receiveItem.QueueByteCount() != scan.retainedByteCount || owner.receiveItem.messageId != scan.messageId ||
		owner.receiveItem.sequenceNumber != scan.sequenceNumber || owner.receivePack.Source != id.Source ||
		owner.receivePack.SequenceId != id.SequenceId || owner.receivePack.EncryptionRole != role ||
		owner.receivePack.EncryptionCompanion != companion || !owner.receivePack.Unwrapped ||
		len(owner.pack.Frames) != scan.frameCount || owner.pack.ContractFrame == nil {
		t.Fatalf("%s did not obtain only its exact independent unbound physical owner", label)
	}
	after := root.StatsWithDescendants(rx, child)
	if after[1] != before[1] {
		t.Fatalf("%s borrowed or changed ordinary RX custody", label)
	}
	for _, index := range []int{0, 2} {
		want := before[index]
		want.UsedByteCount += scan.retainedByteCount
		want.ReservedByteCount += scan.retainedByteCount
		if after[index] != want {
			t.Fatalf("%s did not prepay exactly C=%d in child and ancestor", label, scan.retainedByteCount)
		}
	}
	if old.bindReceiveMemoryOwner(&owner.receivePack) || owner.receivePack.ingressSequence != nil {
		t.Fatalf("%s reused the original sequence's identity or proof permission", label)
	}
	decoded.release()
	synctest.Wait()
	after = root.StatsWithDescendants(rx, child)
	if after[1] != before[1] {
		t.Fatalf("%s refund changed the original RX claim", label)
	}
	for _, index := range []int{0, 2} {
		want := before[index]
		want.ReservedByteCount += scan.retainedByteCount
		want.ReleasedByteCount += scan.retainedByteCount
		if after[index] != want {
			t.Fatalf("%s did not refund exactly its independently paid C", label)
		}
	}
	buffer.mutex.Lock()
	mapsUnchanged := maps.Equal(sequences, buffer.receiveSequences) && maps.Equal(heads, buffer.headReceiveSequenceIds) && maps.Equal(rejected, buffer.rejectedReceiveSequenceIds)
	buffer.mutex.Unlock()
	claimsUnchanged := true
	root.admissionLock.Lock()
	for sequence, original := range prior {
		claimsUnchanged = claimsUnchanged && original == (claims{sequence.holdPromise, sequence.heldHeadRepair, sequence.memoryPressure})
	}
	root.admissionLock.Unlock()
	if !mapsUnchanged || !claimsUnchanged || fixture.deliveredCount != delivered || fixture.ackedCount != acked || len(fixture.receiverOut) != ackWires {
		t.Fatalf("%s created authority or changed existing ingress/escrow/ACK custody", label)
	}
	t.Logf("CONSUMER_ADJACENT_FRESH_CONTEXT_REFUNDED context=%q C=%d old_context_unchanged=true", label, scan.retainedByteCount)
}

// Empty protocol decoding needs no DATA role or byte claim. It must leave
// foreign and returned DATA owners and all receive authority untouched.
func TestReceiveProtocolOpeningAdjacentNeedsNoDataConsumerClaim(t *testing.T) {
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
			requireProtocolOpeningDecodeWithoutDataClaims(t, f.fixture, f.root, f.child, f.opening.bytes, "absent local consumer role")
			if f.sequence.nextSequenceNumber != 0 || f.sequence.receiveContract != nil || len(f.fixture.receiverOut) != 0 {
				t.Fatal("absent local role acquired opening authority")
			}
		})
	}
}

// Same-context identity and invalid/control shapes remain refused. Another
// absent valid context may pay for a fresh full proof, but cannot borrow the
// old context's identity. A source/header mismatch remains a decode refusal.
func TestReceiveProtocolOpeningAdjacentIdentityDoesNotCreateDataAuthority(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		f := newConsumerOpeningAdjacentFixture(t, protocol.MessageType_IpIpPacketFromProvider)
		var original protocol.TransferFrame
		if err := proto.Unmarshal(f.opening.bytes, &original); err != nil {
			t.Fatal(err)
		}
		rows := []struct {
			name         string
			freshContext bool
			change       func(*protocol.TransferFrame)
		}{
			{name: "source", freshContext: true, change: func(v *protocol.TransferFrame) { v.TransferPath.SourceId = NewId().Bytes() }},
			{name: "destination", change: func(v *protocol.TransferFrame) { v.TransferPath.DestinationId = NewId().Bytes() }},
			{name: "generation", freshContext: true, change: func(v *protocol.TransferFrame) { v.Pack.SequenceId = NewId().Bytes() }},
			{name: "lane", freshContext: true, change: func(v *protocol.TransferFrame) { v.Pack.LogicalLane = 1 }},
			{name: "stream", change: func(v *protocol.TransferFrame) { v.Pack.ForceStream = !v.Pack.ForceStream }},
			{name: "contract companion", change: func(v *protocol.TransferFrame) { v.Pack.CompanionContract = !v.Pack.CompanionContract }},
			{name: "session role", freshContext: true, change: func(v *protocol.TransferFrame) {
				role := protocol.SequenceRole_SequenceRoleServer
				v.SessionRole = &role
			}},
			{name: "unsupported role", change: func(v *protocol.TransferFrame) { role := protocol.SequenceRole(99); v.SessionRole = &role }},
			{name: "session companion", freshContext: true, change: func(v *protocol.TransferFrame) { value := true; v.SessionCompanion = &value }},
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
				if row.name == "source" {
					path := TransferPath{SourceId: f.fixture.sender.ClientId(), DestinationId: f.fixture.receiver.ClientId()}
					refuseConsumerAdjacentWireAtPath(t, f.fixture, f.root, f.child, wire, path, "source differs from original route")
				}
				if protocolOnlyPack(frame.Pack) && row.name != "destination" {
					requireProtocolOpeningDecodeWithoutDataClaims(t, f.fixture, f.root, f.child, wire, row.name)
				} else if row.freshContext {
					requireConsumerAdjacentFreshPhysical(t, f.fixture, f.sequence, f.root, f.child, wire, row.name)
				} else {
					refuseConsumerAdjacentWire(t, f.fixture, f.root, f.child, wire, row.name)
				}
			}()
		}
		if f.sequence.receiveQueue.GetByMessageId(f.original.messageId) != f.original || f.original.committed ||
			f.sequence.nextSequenceNumber != 0 || f.sequence.receiveContract != nil || len(f.fixture.receiverOut) != 0 {
			t.Fatal("excluded opening changed original custody or authority")
		}
	})
}

// Independent protocol decodes take no DATA claim. Cancellation still rejects
// the real handoff, and returning each inline owner returns its pooled roots.
func TestReceiveProtocolOpeningAdjacentDuplicateCloseAndRefund(t *testing.T) {
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
		if cost != 0 || pack.receiveMemoryOwned || pack.decodedOwner.receiveItem.memoryBudget != nil || f.child.UsedByteCount() != 0 ||
			f.rx.UsedByteCount() != 6656 || f.root.UsedByteCount() != 6656 || pack.ingressLoanByteCount != 0 {
			t.Fatal("opening copy did not retain its whole independent child claim")
		}
		requireProtocolOpeningDecodeWithoutDataClaims(t, f.fixture, f.root, f.child, f.opening.bytes, "parallel protocol decode")
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
		accepted, admissionErr := f.sequence.Pack(pack, 0)
		if f.rx.UsedByteCount() != 5120 || f.child.UsedByteCount() != 0 || f.root.UsedByteCount() != 5120 ||
			accepted || admissionErr == nil || f.fixture.deliveredCount != 0 || f.fixture.ackedCount != 0 {
			t.Fatal("closure released live opening bytes or accepted a closed generation")
		}
		pack.messagePoolReturn()
		pack = nil
		if f.child.UsedByteCount() != 0 || f.root.UsedByteCount() != 5120 || f.sequence.holdPromise.ingressHead != nil {
			t.Fatal("last opening return did not refund only its child")
		}
	})
}
