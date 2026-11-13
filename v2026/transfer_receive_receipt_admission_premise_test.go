package connect

import (
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/v2026/protocol"
	"google.golang.org/protobuf/proto"
)

// DATA-pressure controls must contain application bytes. A SYN without bytes
// is deliberately protocol-only and may progress beside a full DATA ledger.
func (self *receiptAdmissionFixture) requireDataPack(pack *ReceivePack) {
	self.t.Helper()
	if protocolOnlyReceivePack(pack) || receiveDeliveryIsSmallControl(&receiveItem{frames: pack.Pack.Frames}) {
		self.t.Fatal("DATA receipt-pressure fixture accidentally offered protocol/control traffic")
	}
}

// Feed the actual canonical wire decoder before the ordinary buffer/worker.
// This supplies the physical owner and ingress identity needed to promise a
// retained future's repair metadata; no committed/floor/credit field is set by
// the fixture. The synthetic input roots remain independently witnessed.
func (self *receiptAdmissionFixture) decodeReceiptAdmissionPack(original *ReceivePack) *ReceivePack {
	self.t.Helper()
	defer original.messagePoolReturn()
	path := TransferPath{SourceId: self.source.SourceId, DestinationId: self.client.ClientId()}
	wireBytes, err := proto.Marshal(&protocol.TransferFrame{
		TransferPath: path.ToProtobuf(), MessageType: protocol.MessageType_TransferPack.Enum(), Pack: original.Pack,
	})
	if err != nil {
		self.t.Fatal(err)
	}
	wire := MessagePoolCopy(wireBytes)
	defer func() {
		MessagePoolReturn(wire)
	}()
	decoded := &decodedTransferFrame{}
	defer decoded.release()
	ok, pressure := self.client.unmarshalReceiveMemoryFrame(wire, decoded, true, path, false, sequenceTlsRoleServer, false)
	if !ok || pressure || decoded.frame.Pack == nil || decoded.packOwner == nil {
		self.t.Fatalf("actual receipt-fixture owned decode failed: decoded=%t pressure=%t", ok, pressure)
	}
	owner := decoded.packOwner
	if !owner.receivePack.receiveMemoryOwned || owner.receiveItem.memoryBudget != self.receiveBudget ||
		owner.receiveItem.QueueByteCount() <= MessageByteCount(decoded.frame.Pack.Frames) ||
		protocolOnlyPack(decoded.frame.Pack) {
		self.t.Fatal("retained DATA fixture has no actual paid decoder envelope")
	}
	message := decoded.frame.Pack
	owner = decoded.detachPackOwner()
	pack := &owner.receivePack
	// The decoder may already have registered this live owner. Mirror the
	// production handoff's root-locked publication while preserving its loan.
	root := owner.receiveItem.memoryBudget.admissionRoot()
	root.admissionLock.Lock()
	pack.Source, pack.SequenceId = original.Source, original.SequenceId
	pack.Pack, pack.decodedOwner = message, owner
	pack.ReceiveCallback, pack.Ctx = original.ReceiveCallback, original.Ctx
	pack.MessageByteCount, pack.TransferFrameBytes = MessageByteCount(message.Frames), wire
	pack.TransportType, pack.Unwrapped = original.TransportType, original.Unwrapped
	pack.EncryptionRole, pack.EncryptionCompanion = original.EncryptionRole, original.EncryptionCompanion
	pack.receiveConsumerData, pack.receivePublishedReliable = receiveHeadRecoveryPack(message), !message.Nack
	root.admissionLock.Unlock()
	wire = nil
	// Observe the actual decoder copies too. Generic unpooled roots use the
	// real frame-detachment oracle, while pooled roots keep a final share.
	for _, frame := range message.Frames {
		if pooled, _ := MessagePoolCheck(frame.MessageBytes); pooled {
			self.witnesses = append(self.witnesses, MessagePoolShareReadOnly(frame.MessageBytes))
		}
		if frame.MessageType == protocol.MessageType_TestSimpleMessage {
			self.genericFrames = append(self.genericFrames, frame)
		}
	}
	return pack
}

// Preserve the protocol exception explicitly instead of treating its real
// progress as a DATA-refusal failure. Reliable cumulative delivery still waits
// for the earlier provider receipt, and the later protocol callback occurs once.
func TestReceiveReceiptAdmissionProtocolBypassesFullDataLedger(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newReceiptAdmissionFixture(t, 1)
		dataID, protocolID := NewId(), NewId()
		payload := []byte("one pending DATA receipt")
		f.offer(f.sequenceId, 0, 0, dataID, 41000, tcpFlagAck, payload, f.requireDataPack)
		sequence := f.sequence(f.sequenceId, 0)
		if sequence == nil || sequence.deliveryQueue == nil {
			t.Fatal("real pending DATA receipt was not constructed")
		}
		q := sequence.deliveryQueue
		if len(q.items) != 1 || q.maxCount != 1 || q.controlCount != 0 || q.items[0].pending != 1 || q.items[0].secured {
			t.Fatal("DATA ledger was not actually full before protocol arrival")
		}
		first, charged := q.items[0], q.bytes
		f.offer(f.sequenceId, 0, 1, protocolID, 41001, tcpFlagSyn, nil, func(pack *ReceivePack) {
			if !protocolOnlyReceivePack(pack) {
				t.Fatal("protocol exception lost its actual SYN-only classification")
			}
		})
		if sequence.nextSequenceNumber != 2 || sequence.ctx.Err() != nil || len(q.items) != 2 ||
			q.items[0] != first || first.pending != 1 || first.secured || q.bytes != charged || q.controlCount != 1 ||
			f.delivered(41001) != 1 || !f.hasAck(f.sequenceId, protocolID, true) ||
			f.hasAck(f.sequenceId, protocolID, false) || f.hasAck(f.sequenceId, dataID, false) {
			t.Fatal("protocol progress changed the earlier DATA owner or cumulative receipt boundary")
		}
		f.releaseTcp(f.takeTcp(), 41001, nil)
		f.releaseTcp(f.firstTcp, 41000, nil)
		if !f.hasAck(f.sequenceId, protocolID, false) || q.pending() || q.bytes != 0 || q.controlCount != 0 {
			t.Fatal("actual DATA admission did not secure the exact cumulative prefix")
		}
		f.releaseTcp(f.firstTcp, 41000, payload)
		f.offer(f.sequenceId, 0, 1, protocolID, 41001, tcpFlagSyn, nil)
		if f.delivered(41001) != 1 {
			t.Fatal("secured protocol retry delivered twice")
		}
	})
}

// Prove the old fixture's missing premise with identical generic DATA bytes:
// an unowned future is tentative and unACKed; real decoded custody prepays its
// repair before SACK. Both real workers must cancel and return every owner.
func TestReceiveReceiptAdmissionSelectiveCustodyRequiresDecodedOwner(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		for _, decoded := range []bool{false, true} {
			f := newReceiptAdmissionFixtureWithProvider(t, 1, false)
			id := NewId()
			f.offerGenericWithDecodedOwner(decoded, 1, id, 200)
			sequence := f.sequence(f.sequenceId, 0)
			if sequence == nil {
				t.Fatal("actual future DATA receive sequence was not constructed")
			}
			held := sequence.receiveQueue.GetByMessageId(id)
			if held == nil || held.noBudget || held.memoryBudget != f.receiveBudget ||
				held.committed != decoded || f.hasAck(f.sequenceId, id, true) != decoded ||
				f.hasAck(f.sequenceId, id, false) || sequence.nextSequenceNumber != 0 || sequence.deliveryQueue != nil {
				t.Fatalf("decoded-owner premise did not decide the real selective commitment: decoded=%t", decoded)
			}
			if decoded && (held.decodedOwner == nil || !held.decodedOwner.receivePack.receiveMemoryOwned ||
				!held.decodedOwner.receivePack.heldHeadRepairEligible || sequence.heldHeadRepair.reservedByteCount != decodedPackOwnerQueueByteCount) {
				t.Fatal("selective ACK lacks the actual bound owner and paid repair envelope")
			}
			if !decoded && (held.decodedOwner != nil || sequence.heldHeadRepair.eligibleCount != 0 ||
				sequence.heldHeadRepair.reservedByteCount != 0) {
				t.Fatal("unowned fixture acquired physical repair authority")
			}
			sequence.Cancel()
			synctest.Wait()
			f.collectAcks()
			select {
			case <-sequence.done:
			default:
				t.Fatal("actual retained owner did not join on cancellation")
			}
			if sequence.receiveQueue.Len() != 0 || f.receiveBudget.UsedByteCount() != 0 ||
				f.packBudget.UsedByteCount() != 0 || f.hasAck(f.sequenceId, id, false) {
				t.Fatal("premise control leaked custody or invented cumulative delivery on cancel")
			}
		}
	})
}
