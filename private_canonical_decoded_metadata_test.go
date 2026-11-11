// Actual owned decoding and receive admission charge exceptional frame
// metadata before custody/ACKs, then retire every owner through joined workers.
package connect

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"unsafe"
)

// A witness owns only its raw input while handoff is refused. After acceptance,
// the receive worker owns the decoded carrier, frames and embedded pipeline.
// Cleanup runs after that worker joins and tests actual field detachment too.
type canonicalDecodedMetadataWitness struct {
	owner            *decodedPackOwner
	pack             *ReceivePack
	frames           SendPack
	payloadWitnesses [][]byte
	carrierWitness   []byte
	want             ByteCount
	handed           bool
}

// Encodes ordinary FromProvider frames with the real codec, then detaches the
// real owned decoder result exactly as Client.run does. No protocol import or
// synthetic decoded-owner accounting substitute is needed in this test.
func canonicalDecodeMetadataPacket(t *testing.T, source Id, destination Id, sequence Id, message Id, number uint64, head bool, count int) *canonicalDecodedMetadataWitness {
	t.Helper()
	frames, witnesses := transferGroupTestFrames(t, count, DefaultMtu)
	defer func() {
		for _, frame := range frames {
			MessagePoolReturn(frame.MessageBytes)
		}
		releaseTransferGroupTestWitnesses(t, frames, witnesses)
	}()
	model, err := ipPacketFromProviderFrame(nil, DefaultProtocolVersion)
	if err != nil || model == nil {
		t.Fatal("raw provider-frame type unavailable", err)
	}
	for _, frame := range frames {
		frame.MessageType = model.MessageType
	}
	wire := marshalSendPackTransferFrame(&sendPackFrame{path: NewTransferPath(source, destination, Id{}),
		messageId: message, sequenceId: sequence, sequenceNumber: number, head: head, frames: frames, tagSendTime: 1})
	decoded := inboundDecodedTransferFrames.take()
	if !unmarshalOwnedTransferFrame(wire, decoded, true) {
		inboundDecodedTransferFrames.put(decoded)
		MessagePoolReturn(wire)
		t.Fatal("actual owned Transfer decode failed")
	}
	owner := decoded.detachPackOwner()
	inboundDecodedTransferFrames.put(decoded)
	if owner == nil {
		MessagePoolReturn(wire)
		t.Fatal("actual decoder did not create an owned Pack")
	}
	pack := &owner.receivePack
	*pack = ReceivePack{Source: SourceId(source), SequenceId: sequence, Pack: &owner.pack, decodedOwner: owner,
		TransferFrameBytes: wire, MessageByteCount: MessageByteCount(owner.pack.Frames), Unwrapped: true,
		EncryptionRole: sequenceTlsRoleServer, TransportType: TransportTypeH1}
	result := &canonicalDecodedMetadataWitness{owner: owner, pack: pack, frames: SendPack{Frames: owner.pack.Frames}}
	if pooled, _ := MessagePoolCheck(wire); pooled {
		result.carrierWitness = MessagePoolShareReadOnly(wire)
	}
	result.want = MessagePoolRootByteCount(wire) + decodedPackOwnerQueueByteCount
	for _, frame := range owner.pack.Frames {
		result.want += MessagePoolRootByteCount(frame.MessageBytes)
		result.payloadWitnesses = append(result.payloadWitnesses, MessagePoolShareReadOnly(frame.MessageBytes))
	}
	if extra := len(owner.pack.Frames) - len(owner.frames); extra > 0 {
		result.want += ByteCount(extra) * 128
	}
	if capacity := cap(owner.pack.Frames); len(owner.framePointers) < capacity {
		result.want += ByteCount(capacity)*8 + 16
	}
	return result
}

// The same canonical wire through the receiving client's production scanner
// and paid decoder, with the Pack fields its receive loop sets before handoff.
// With a receive budget that owns decoded memory, this is how Client.run
// decodes; the claim it prepays must be exactly the canonical envelope.
func canonicalDecodeMetadataPaidPacket(t *testing.T, client *Client, source Id, destination Id, sequence Id, message Id, number uint64, head bool, count int) *canonicalDecodedMetadataWitness {
	t.Helper()
	frames, witnesses := transferGroupTestFrames(t, count, DefaultMtu)
	defer func() {
		for _, frame := range frames {
			MessagePoolReturn(frame.MessageBytes)
		}
		releaseTransferGroupTestWitnesses(t, frames, witnesses)
	}()
	model, err := ipPacketFromProviderFrame(nil, DefaultProtocolVersion)
	if err != nil || model == nil {
		t.Fatal("raw provider-frame type unavailable", err)
	}
	for _, frame := range frames {
		frame.MessageType = model.MessageType
	}
	path := NewTransferPath(source, destination, Id{})
	wire := marshalSendPackTransferFrame(&sendPackFrame{path: path,
		messageId: message, sequenceId: sequence, sequenceNumber: number, head: head, frames: frames, tagSendTime: 1})
	decoded := inboundDecodedTransferFrames.take()
	if ok, pressure := client.unmarshalReceiveMemoryFrame(wire, decoded, false, path, false, sequenceTlsRoleServer, false); !ok || pressure {
		decoded.release()
		inboundDecodedTransferFrames.put(decoded)
		MessagePoolReturn(wire)
		t.Fatalf("actual paid Transfer decode failed: pressure=%t", pressure)
	}
	owner := decoded.detachPackOwner()
	inboundDecodedTransferFrames.put(decoded)
	if owner == nil || !owner.receivePack.receiveMemoryOwned || owner.receiveItem.memoryBudget == nil {
		MessagePoolReturn(wire)
		t.Fatal("actual paid decoder did not create a charged Pack owner")
	}
	pack := &owner.receivePack
	root := owner.receiveItem.memoryBudget.admissionRoot()
	root.admissionLock.Lock()
	pack.Source, pack.SequenceId, pack.TransferFrameBytes = SourceId(source), sequence, wire
	pack.MessageByteCount, pack.Unwrapped = MessageByteCount(owner.pack.Frames), true
	pack.EncryptionRole, pack.TransportType = sequenceTlsRoleServer, TransportTypeH1
	pack.receiveConsumerData = receiveHeadRecoveryPack(pack.Pack)
	pack.receivePublishedReliable = !pack.Pack.Nack
	root.admissionLock.Unlock()
	result := &canonicalDecodedMetadataWitness{owner: owner, pack: pack, frames: SendPack{Frames: owner.pack.Frames}}
	if pooled, _ := MessagePoolCheck(wire); pooled {
		result.carrierWitness = MessagePoolShareReadOnly(wire)
	}
	result.want = MessagePoolRootByteCount(wire) + decodedPackOwnerQueueByteCount
	for _, frame := range owner.pack.Frames {
		result.want += MessagePoolRootByteCount(frame.MessageBytes)
		result.payloadWitnesses = append(result.payloadWitnesses, MessagePoolShareReadOnly(frame.MessageBytes))
	}
	if extra := len(owner.pack.Frames) - len(owner.frames); extra > 0 {
		result.want += ByteCount(extra) * 128
	}
	if capacity := cap(owner.pack.Frames); len(owner.framePointers) < capacity {
		result.want += ByteCount(capacity)*8 + 16
	}
	if owner.receiveItem.QueueByteCount() != result.want {
		t.Errorf("paid decode prepaid %d, want the canonical envelope %d", owner.receiveItem.QueueByteCount(), result.want)
	}
	return result
}

// Tests these conservative allocation allowances against the actual compiled
// types, independently of the production arithmetic. No physical RSS claim.
func (self *canonicalDecodedMetadataWitness) checkLayout(t *testing.T) {
	t.Helper()
	if unsafe.Sizeof(*self.owner) > uintptr(decodedPackOwnerQueueByteCount) ||
		unsafe.Sizeof(self.owner.frames[0]) > 128 || unsafe.Sizeof(self.owner.framePointers[0]) > 8 {
		t.Fatal("compiled decoder layout exceeds its conservative metadata envelope")
	}
}

// Refused callers release their own Pack; accepted Packs must already have
// retired through cancellation or delivery before this final witness runs.
func (self *canonicalDecodedMetadataWitness) finish(t *testing.T) {
	t.Helper()
	if !self.handed {
		self.pack.messagePoolReturn()
	}
	if self.owner.inUse {
		t.Error("joined receive worker retained the decoded metadata owner")
		self.pack.messagePoolReturn()
	}
	if self.owner.receivePack.TransferFrameBytes != nil || self.owner.pack.Frames != nil {
		t.Error("decoded carrier or pointer backing was not detached")
	}
	for _, frame := range self.frames.Frames {
		if frame != nil && len(frame.MessageBytes) != 0 {
			t.Error("decoded payload frame remained attached after owner retirement")
		}
	}
	for _, witness := range self.payloadWitnesses {
		if !MessagePoolReturn(witness) {
			t.Error("decoded payload witness was not the final owner")
		}
	}
	if self.carrierWitness != nil && !MessagePoolReturn(self.carrierWitness) {
		t.Error("decoded carrier witness was not the final owner")
	}
}

// The first overflow and a full callback-storage width use real decoder growth;
// the two-inline-frame control retains its exact existing one-KiB metadata.
func TestCanonicalDecodedMetadataActualDecodeBoundaries(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, count := range []int{2, 3, 8, 16} {
		func() {
			witness := canonicalDecodeMetadataPacket(t, NewId(), NewId(), NewId(), NewId(), 0, true, count)
			defer witness.finish(t)
			witness.checkLayout(t)
			got := witness.pack.receiveQueueByteCount()
			if got != witness.want {
				t.Fatalf("actual decoded metadata count%d charge=%d want%d", count, got, witness.want)
			}
			if witness.pack.receiveQueueByteCount() != got {
				t.Fatal("immutable decoded owner changed its memoized charge")
			}
			if count == 2 && witness.owner.metadataQueueByteCount() != 1024 {
				t.Fatal("inline decoder pair gained an exceptional metadata charge")
			}
		}()
	}
}

// Exercises an underfunded real Pack handoff, exact-capacity retry of that same
// decoded owner, and full receive-leaf custody.
//
// The receive budget here owns decoded memory, so the client's receive loop
// decodes paid: the receive leaf is charged at decode, and the Pack leaf, a
// sibling under the same parent, takes a quota for the handoff rather than a
// second physical charge. Since 00d4f62b the held original commits its
// selective ACK only with repair escrow, which for this no-contract peer is the
// inline owner; the full leaf is the owner plus that escrow. A full leaf has
// no room for the drain arm's predecessor to decode, so that arm first funds
// the receive leaf for it explicitly, as the retry funds the Pack leaf.
func runCanonicalDecodedMetadataFullBudget(t *testing.T, cancelHeld bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		var witnesses []*canonicalDecodedMetadataWitness
		t.Cleanup(func() {
			for _, witness := range witnesses {
				witness.finish(t)
			}
		})
		parent := NewTransferMemoryBudget(mib(2))
		packBudget := NewTransferMemoryBudgetWithParent(mib(1), parent)
		receiveBudget := NewTransferMemoryBudgetWithParent(mib(1), parent)
		t.Cleanup(func() {
			for _, budget := range []*TransferMemoryBudget{packBudget, receiveBudget, parent} {
				assertRetainedBudgetBalance(t, budget)
			}
		})
		fixture := newWindowRoundFixture(t, nil, func(settings *ReceiveBufferSettings) {
			settings.SequenceBufferSize, settings.H1SequenceBufferSize = 4, 4
			settings.PackQueueBudget, settings.PackQueueRetainedByteAccounting = packBudget, true
			settings.ReceiveQueueBudget, settings.ReceiveQueueRetainedByteAccounting = receiveBudget, true
			settings.ReceiveQueueMinByteCount = 0
			settings.ReceiveQueueMaxByteCount = kib(64)
			settings.ReceiveHoldPolicy = ReceiveHoldRefuse
		}, TransportTypeH1)
		entered, released := make(chan struct{}), make(chan struct{})
		var enterOnce, releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(released) }) }
		t.Cleanup(release)
		fixture.receiver.receiveBuffer.beforeRunReceiveSequenceForTest = func(receiveSequenceId) {
			enterOnce.Do(func() { close(entered) })
			<-released
		}
		packQuota := func() ByteCount { return packBudget.Stats().QuotaByteCount }
		escrow := fixture.receiver.settings.ReceiveBufferSettings.receiveHeldHeadRepairByteCount(true)
		if escrow != decodedPackOwnerQueueByteCount {
			t.Fatal("no-contract repair escrow is not the inline owner")
		}
		sequenceId, messageId := NewId(), NewId()
		witness := canonicalDecodeMetadataPaidPacket(t, fixture.receiver, fixture.sender.ClientId(), fixture.receiver.ClientId(), sequenceId, messageId, 1, false, 8)
		witnesses = append(witnesses, witness)
		witness.checkLayout(t)
		witness.pack.Ctx, witness.pack.ReceiveCallback = fixture.ctx, fixture.receiver.receiveCallback
		parent.SetTotalByteCount(2*witness.want + escrow)
		packBudget.SetTotalByteCount(witness.want - 1)
		receiveBudget.SetTotalByteCount(witness.want + escrow)
		if receiveBudget.UsedByteCount() != witness.want || parent.UsedByteCount() != witness.want {
			t.Fatal("paid decode did not charge the receive leaf exactly once")
		}
		accepted, err := fixture.receiver.receiveBuffer.Pack(witness.pack, 0)
		witness.handed = accepted
		<-entered
		if accepted || err != nil {
			t.Fatalf("underfunded actual decoded Pack handoff accepted=%t error=%v", accepted, err)
		}
		if packBudget.UsedByteCount() != 0 || packQuota() != 0 || receiveBudget.UsedByteCount() != witness.want ||
			parent.UsedByteCount() != witness.want || len(fixture.receiverOut) != 0 ||
			!witness.owner.inUse || len(witness.pack.Pack.Frames) != 8 || !bytes.Equal(witness.pack.Pack.MessageId, messageId.Bytes()) {
			t.Fatal("refused handoff transferred custody, charged a partial owner or emitted an ACK")
		}
		packBudget.SetTotalByteCount(witness.want)
		accepted, err = fixture.receiver.receiveBuffer.Pack(witness.pack, 0)
		witness.handed = accepted
		if !accepted || err != nil || packBudget.UsedByteCount() != 0 || packQuota() != witness.want ||
			receiveBudget.UsedByteCount() != witness.want || parent.UsedByteCount() != witness.want {
			t.Fatalf("exact decoded handoff retry=%t/%v pack=%d/%d receive=%d parent=%d want%d", accepted, err,
				packBudget.UsedByteCount(), packQuota(), receiveBudget.UsedByteCount(), parent.UsedByteCount(), witness.want)
		}
		release()
		synctest.Wait()
		manager := fixture.receiver.receiveBuffer
		manager.mutex.Lock()
		sequence := manager.receiveSequences[receiveSequenceId{Source: SourceId(fixture.sender.ClientId()), SequenceId: sequenceId, EncryptionRole: sequenceTlsRoleServer}]
		manager.mutex.Unlock()
		if sequence == nil {
			t.Fatal("real receive sequence disappeared before its held owner")
		}
		held := sequence.receiveQueue.GetByMessageId(messageId)
		if held == nil || held.decodedOwner != witness.owner || held.QueueByteCount() != witness.want || !held.committed ||
			packBudget.UsedByteCount() != 0 || packQuota() != 0 || sequence.heldHeadRepair.reservedByteCount != escrow ||
			receiveBudget.UsedByteCount() != witness.want+escrow || receiveBudget.Available() != 0 ||
			parent.UsedByteCount() != witness.want+escrow || fixture.deliveredCount != 0 {
			t.Fatal("full receive leaf lost exact metadata custody across the handoff/reorder boundary")
		}
		selective := fixture.take(fixture.receiverOut)
		if selective.ack == nil || !selective.ack.Selective || !bytes.Equal(selective.ack.MessageId, messageId.Bytes()) {
			t.Fatal("actual held decoded Pack did not acquire its selective ACK lease")
		}
		fixture.drop(selective)
		retired := witness.want + escrow
		if cancelHeld {
			fixture.cancel()
			if err := fixture.receiver.CloseAndWait(context.Background()); err != nil {
				t.Fatal(err)
			}
			if fixture.deliveredCount != 0 {
				t.Fatal("canceled held Pack was delivered")
			}
		} else {
			receiveBudget.SetTotalByteCount(2*witness.want + escrow)
			predecessor := canonicalDecodeMetadataPaidPacket(t, fixture.receiver, fixture.sender.ClientId(), fixture.receiver.ClientId(), sequenceId, NewId(), 0, true, 0)
			witnesses = append(witnesses, predecessor)
			predecessor.pack.Ctx, predecessor.pack.ReceiveCallback = fixture.ctx, fixture.receiver.receiveCallback
			retired += predecessor.want
			if receiveBudget.UsedByteCount() != retired {
				t.Fatal("separately funded predecessor did not pay its own decode")
			}
			accepted, err := fixture.receiver.receiveBuffer.Pack(predecessor.pack, 0)
			predecessor.handed = accepted
			if !accepted || err != nil {
				t.Fatal("separately funded actual predecessor was refused", accepted, err)
			}
			synctest.Wait()
			cumulative := false
			for len(fixture.receiverOut) > 0 {
				ack := fixture.take(fixture.receiverOut)
				if ack.ack != nil && !ack.ack.Selective && bytes.Equal(ack.ack.MessageId, messageId.Bytes()) {
					cumulative = true
				}
				fixture.drop(ack)
			}
			if !cumulative || fixture.deliveredCount != 8 || sequence.nextSequenceNumber != 2 {
				t.Fatal("real predecessor did not drain the original owner through callback and cumulative ACK")
			}
		}
		for _, budget := range []*TransferMemoryBudget{packBudget, receiveBudget, parent} {
			assertRetainedBudgetBalance(t, budget)
		}
		stats := receiveBudget.Stats()
		if stats.ReservedByteCount != retired || stats.ReleasedByteCount != retired || stats.QuotaByteCount != 0 {
			t.Fatalf("held metadata reservation did not retire exactly once: %+v want%d", stats, retired)
		}
	})
}

// The actual empty predecessor drains a full receive leaf without fake ACKs.
func TestCanonicalDecodedMetadataFullBudgetDelivery(t *testing.T) {
	runCanonicalDecodedMetadataFullBudget(t, false)
}

// Cancellation joins the held decoder owner before any witness is retired.
func TestCanonicalDecodedMetadataFullBudgetCancel(t *testing.T) {
	runCanonicalDecodedMetadataFullBudget(t, true)
}
