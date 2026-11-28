package connect

import (
	"bytes"
	"math"
	"testing"

	"github.com/urnetwork/connect/v2026/protocol"
	"google.golang.org/protobuf/proto"
)

func TestNativeConnectedRaceWireKeepsBothEncodingsWithinExistingCredit(t *testing.T) {
	path := TransferPath{SourceId: NewId(), DestinationId: NewId(), StreamId: NewId()}
	opening := sendPackFrame{path: path, messageId: NewId(), sequenceId: NewId(), sequenceNumber: math.MaxUint64,
		head: true, tagSendTime: math.MaxUint64, connectRace: true}
	expectedPack := buildEquivalentTransferFrame(&opening)
	expectedPack.Pack.ConnectRace = true
	wire := marshalSendPackTransferFrame(&opening)
	defer MessagePoolReturn(wire)
	want, err := proto.Marshal(expectedPack)
	if err != nil || !bytes.Equal(want, wire) {
		t.Fatalf("solicited Pack disagrees with protobuf: err=%v", err)
	}
	body, err := proto.Marshal(expectedPack.Pack)
	if err != nil {
		t.Fatal(err)
	}
	decodedPack, ok := decodePack(body)
	if !ok || !decodedPack.ConnectRace {
		t.Fatal("ordinary Pack decoder lost solicited native opening")
	}
	ownedPack, ok := decodePackOwned(body)
	if !ok || !ownedPack.pack.ConnectRace {
		t.Fatal("owned Pack decoder lost solicited native opening")
	}
	ownedPack.release()

	ack := sendAckFrame{path: path, messageId: NewId(), sequenceId: NewId(),
		tagSet: true, tagSendTime: math.MaxUint64, logicalLaneVersion: math.MaxUint32,
		compactContractRecovery: true, contractAhead: true, receiveWindowSet: true, receiveWindowByteCount: math.MaxUint64,
		ackCompressTimeoutSet: true, ackCompressTimeoutMicros: math.MaxUint32,
		receiverAckDelaySet: true, receiverAckDelayMicros: math.MaxUint32, connectedRaceReset: math.MaxUint32,
		streamForwardingAcceptance: make([]byte, streamForwardingAnswerByteCount),
		evictedSequenceNumbers:     []uint64{math.MaxUint64, math.MaxUint64 - 1, math.MaxUint64 - 2}}
	expected := buildEquivalentAckFrame(&ack)
	expected.Ack.ConnectedRaceReset = math.MaxUint32
	actual := marshalSendAckTransferFrame(&ack)
	defer MessagePoolReturn(actual)
	want, err = proto.Marshal(expected)
	if err != nil || !bytes.Equal(want, actual) {
		t.Fatalf("connected-reset Ack disagrees with protobuf: err=%v", err)
	}
	ackBody, err := proto.Marshal(expected.Ack)
	if err != nil {
		t.Fatal(err)
	}
	direct, ok := decodeAck(ackBody)
	var owned decodedTransferFrame
	if !ok || direct.ConnectedRaceReset != math.MaxUint32 || !decodeAckOwned(ackBody, &owned) || owned.ack.ConnectedRaceReset != math.MaxUint32 {
		t.Fatal("a decoder lost the connected-reset outcome")
	}
	legacy, err := proto.Marshal(&protocol.TransferFrame{TransferPath: path.ToProtobuf(), Frame: &protocol.Frame{
		MessageType: protocol.MessageType_TransferAck, MessageBytes: ackBody}})
	if err != nil {
		t.Fatal(err)
	}
	cipher := newFrameCodecTestSequenceCipher(t)
	for _, raw := range [][]byte{actual, legacy} {
		sealed, err := cipher.SealOuterFrame(path, raw, protocol.SequenceRole_SequenceRoleServer, true)
		if err != nil {
			t.Fatal(err)
		}
		size := len(sealed)
		MessagePoolReturn(sealed)
		limit := ackResponseEntryMaxByteCount + 10*len(ack.evictedSequenceNumbers)
		if max(size, len(raw)) > limit || !nativeConnectedRaceAckFits(path, len(raw), len(ack.evictedSequenceNumbers)) {
			t.Fatalf("forwarding and eviction coexistence exceeded existing responder credit: plain=%d sealed=%d max=%d", len(raw), size, limit)
		}
		// Increasing only the inner bytes crosses the same bound on both
		// encodings, even at a protobuf length-prefix size transition.
		at := len(raw)
		for nativeConnectedRaceAckFits(path, at, len(ack.evictedSequenceNumbers)) {
			at++
		}
		for _, proposed := range []int{at - 1, at} {
			b, err := cipher.SealOuterFrame(path, make([]byte, proposed), protocol.SequenceRole_SequenceRoleServer, true)
			if err != nil {
				t.Fatal(err)
			}
			fits := len(b) <= limit
			MessagePoolReturn(b)
			if fits != nativeConnectedRaceAckFits(path, proposed, len(ack.evictedSequenceNumbers)) || fits != (proposed < at) {
				t.Fatal("optional outcome did not respect the exact encrypted response boundary")
			}
		}
	}
}

func TestNativeConnectedRaceCompressedAckPreservesOnlyRepresentableOriginal(t *testing.T) {
	previous := sequenceAck{sequenceNumber: 10, connectedRaceReset: 3}
	window := newSequenceAckWindow()
	window.Update(previous)
	window.Update(sequenceAck{sequenceNumber: 14})
	if got := window.Snapshot(false).headAck.connectedRaceReset; got != 4<<16|3 {
		t.Fatalf("cumulative head lost its opening and reset: %x", got)
	}
	window.Update(sequenceAck{sequenceNumber: 15, connectedRaceReset: 2})
	if got := window.Snapshot(false).headAck.connectedRaceReset; got != 2 {
		t.Fatalf("older optional evidence displaced the new owner: %x", got)
	}
	var scratch [1]sequenceAck
	taken, _ := window.takeResponse(scratch[:0], 1)
	if len(taken) != 1 || taken[0].connectedRaceReset != 2 {
		t.Fatal("the existing response did not own its optional receipt")
	}
	window.Update(sequenceAck{sequenceNumber: 16})
	if got := window.Snapshot(false).headAck.connectedRaceReset; got != 0 {
		t.Fatalf("an ordinary later response repeated an already emitted outcome: %x", got)
	}
	window.Restore(taken)
	if got := window.Snapshot(false).headAck.connectedRaceReset; got != 1<<16|2 {
		t.Fatalf("local write refusal dropped the restored optional outcome: %x", got)
	}
	for _, ack := range []sequenceAck{{sequenceNumber: 9}, {sequenceNumber: 10 + math.MaxUint16 + 1}, {sequenceNumber: 11, selective: true}} {
		ack.inheritConnectedRaceReset(previous)
		if ack.connectedRaceReset != 0 {
			t.Fatalf("unqualified head acquired an original: %x", ack.connectedRaceReset)
		}
	}
}

func TestNativeConnectedRaceSenderRequiresBothExactRetainedOrdinals(t *testing.T) {
	source := &SendSequence{ctx: t.Context()}
	channel := &multiClientChannel{}
	offer := &connectSynOffer{}
	offer.materialized.Store(true)
	group := &parsedPacketGroup{completionClient: channel, nativeRaceReset: &nativeRaceOpening{
		source: source.ctx.Done(), ordinal: 3, resetOrdinal: 8, offer: offer,
	}}
	item := &sendItem{transferItem: transferItem{sequenceNumber: 8}}
	item.acks.add(sendAckRecord{target: group})
	const valid = 4<<16 | 5
	for _, ack := range []sequenceAck{
		{sequenceNumber: 12, connectedRaceReset: 4<<16 | 4},
		{sequenceNumber: 12, connectedRaceReset: 3<<16 | 5},
		{sequenceNumber: 12, connectedRaceReset: 4 << 16},
		{sequenceNumber: 12, connectedRaceReset: valid, selective: true},
		{sequenceNumber: 12, connectedRaceReset: valid, contractMissing: true},
		{sequenceNumber: 12, connectedRaceReset: valid, immediateSequenceNumberSet: true},
	} {
		source.applyConnectedRaceReset(item, ack, 12)
		if offer.orderedAnswer.Load() {
			t.Fatal("wrong source ordinals or noncumulative ACK retired an opening")
		}
	}
	group.nativeRaceReset.source = make(chan struct{})
	ack := sequenceAck{sequenceNumber: 12, connectedRaceReset: valid}
	source.applyConnectedRaceReset(item, ack, 12)
	if offer.orderedAnswer.Load() {
		t.Fatal("another source generation retired an opening")
	}
	group.nativeRaceReset.source = source.ctx.Done()
	source.applyConnectedRaceReset(item, ack, 12)
	if !offer.orderedAnswer.Load() || !channel.connectSynAnswersPending.Load() {
		t.Fatal("exact cumulative original and reset failed to settle their offer")
	}
}
