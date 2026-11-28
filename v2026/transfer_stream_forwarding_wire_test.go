// Proves the existing native codecs and delivery owners carry the optional
// forwarding pair while their hard receive and acknowledgement budgets hold.
package connect

import (
	"bytes"
	"context"
	"math"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
	"google.golang.org/protobuf/proto"
)

// Uses the real stream authorization and pending enrollment owners without
// starting unrelated workers or opening network connections.
func testStreamForwardingWireClient(ctx context.Context, clientId Id) *Client {
	client := &Client{clientId: clientId}
	manager := &StreamManager{ctx: ctx, client: client, authorization: newStreamAuthorizationState()}
	manager.streamBuffer = &StreamBuffer{ctx: ctx, streamManager: manager}
	client.streamManager = manager
	return client
}

// The offer is an extension of the wrapper. Signing and the existing contract
// body remain unchanged, including the ordinary absent-field form.
func TestStreamForwardingOfferPreservesSignedContractAndOldEnvelope(t *testing.T) {
	now := time.Now()
	contract := testStreamForwardingContract(t, now)
	old, err := proto.Marshal(contract.contract)
	if err != nil {
		t.Fatal(err)
	}
	offer := newStreamForwardingOffer(contract, NewId(), NewId(), now)
	wire := appendStreamForwardingOffer(old, offer.wire[:])
	defer MessagePoolReturn(wire)
	if !bytes.HasPrefix(wire, old) {
		t.Fatal("the forwarding offer rewrote the existing envelope")
	}
	var oldDecoded, newDecoded protocol.Contract
	if err := proto.Unmarshal(old, &oldDecoded); err != nil {
		t.Fatal(err)
	}
	if err := proto.Unmarshal(wire, &newDecoded); err != nil {
		t.Fatal(err)
	}
	if oldDecoded.StreamForwardingOffer != nil || !bytes.Equal(newDecoded.StreamForwardingOffer, offer.wire[:]) {
		t.Fatal("the optional offer lost exact presence")
	}
	if !bytes.Equal(newDecoded.StoredContractBytes, oldDecoded.StoredContractBytes) ||
		!bytes.Equal(newDecoded.StoredContractHmac, oldDecoded.StoredContractHmac) {
		t.Fatal("stream enrollment changed the authenticated grant")
	}
	newDecoded.StreamForwardingOffer = nil
	if !proto.Equal(&oldDecoded, &newDecoded) {
		t.Fatal("ordinary contract fields changed")
	}
}

// Both native wire forms retain all ACK fields across the additional fixed
// record. Required-mode wrapping fits the original maximum at the same cut.
func TestStreamForwardingAcceptanceCodecFitsExistingAckBudget(t *testing.T) {
	ownerId, peerId := NewId(), NewId()
	path := sendTransferPath(ownerId, DestinationId(peerId))
	missingId := NewId()
	wireRecord := bytes.Repeat([]byte{streamForwardingEnrollmentVersion}, streamForwardingAnswerByteCount)
	ack := &sendAckFrame{
		path: path, messageId: NewId(), sequenceId: NewId(),
		selective: true, deliveredImmediately: true,
		immediateSequenceNumber: math.MaxUint64, immediateSequenceNumberSet: true,
		tagSendTime: math.MaxUint64, tagSet: true, missingContractId: &missingId,
		compactContractRecovery: true, logicalLaneVersion: math.MaxUint32,
		receiveWindowByteCount: math.MaxUint64, receiveWindowSet: true,
		ackCompressTimeoutMicros: math.MaxUint32, ackCompressTimeoutSet: true,
		receiverAckDelayMicros: math.MaxUint32, receiverAckDelaySet: true,
		contractAhead: true, evictedSequenceNumbers: []uint64{math.MaxUint64, math.MaxUint64, math.MaxUint64},
		streamForwardingAcceptance: wireRecord,
	}
	assertAckCodecMatches(t, ack)
	v2 := marshalSendAckTransferFrame(ack)
	defer MessagePoolReturn(v2)

	legacyAck := buildEquivalentAckFrame(ack).Ack
	legacyBody, err := proto.Marshal(legacyAck)
	if err != nil {
		t.Fatal(err)
	}
	v1, err := proto.Marshal(&protocol.TransferFrame{
		TransferPath: path.ToProtobuf(),
		Frame:        &protocol.Frame{MessageType: protocol.MessageType_TransferAck, MessageBytes: legacyBody},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, plain := range [][]byte{v1, v2} {
		func() {
			cipher := newFrameCodecTestSequenceCipher(t)
			wrapped, err := cipher.SealOuterFrame(path, plain, protocol.SequenceRole_SequenceRoleServer, true)
			if err != nil {
				t.Fatal(err)
			}
			defer MessagePoolReturn(wrapped)
			if maxByteCount := ackResponseEntryMaxByteCount + 10*len(ack.evictedSequenceNumbers); len(wrapped) > maxByteCount {
				t.Fatalf("wrapped forwarding ACK uses %d bytes with %d reserved", len(wrapped), maxByteCount)
			}
			decoded := inboundDecodedTransferFrames.take()
			defer inboundDecodedTransferFrames.put(decoded)
			if !unmarshalOwnedTransferFrame(plain, decoded, true) {
				t.Fatal("native codec did not decode its forwarding ACK")
			}
			protocolAck := decoded.frame.Ack
			if protocolAck == nil && decoded.frame.Frame != nil {
				protocolAck = &protocol.Ack{}
				if err := proto.Unmarshal(decoded.frame.Frame.MessageBytes, protocolAck); err != nil {
					t.Fatal(err)
				}
			}
			if protocolAck == nil || !bytes.Equal(protocolAck.StreamForwardingAcceptance, wireRecord) {
				t.Fatal("native codec lost the exact acceptance")
			}
		}()
	}
}

// These are the same calls used by normal ACK compression and source
// sequences. Queue-only receipts and noncovering delivery never enroll.
func TestStreamForwardingAcceptanceUsesNativeDeliveryCoverage(t *testing.T) {
	now := time.Now()
	ctx := t.Context()
	contract := testStreamForwardingContract(t, now)
	sequenceId, introductionId := NewId(), NewId()
	offer := newStreamForwardingOffer(contract, sequenceId, introductionId, now)
	offer.sequenceNumber = 12
	destinationId, originId := contract.path.DestinationId, contract.path.SourceId
	origin := newPlatformReceiveOwner(originId, ctx.Done(), nil, true)
	destination := newPlatformReceiveOwner(destinationId, ctx.Done(), nil, true)
	var answer streamForwardingAnswer
	if !acceptStreamForwardingOffer(contract, contract.path.LocalMask(), sequenceId, introductionId, now,
		destination.provenance(platformReceiveH1), offer.wire[:], func(candidate streamForwardingAnswer) bool {
			answer = candidate
			answer.sequenceNumber = offer.sequenceNumber
			return true
		}) {
		t.Fatal("verified introduction did not create an answer")
	}
	receiver := &ReceiveSequence{ctx: ctx, client: testStreamForwardingWireClient(ctx, destinationId)}
	receiver.forwardingAnswer.Store(&answer)
	sender := &SendSequence{ctx: ctx, client: testStreamForwardingWireClient(ctx, originId), sequenceId: sequenceId, destination: destinationId}
	sender.forwardingOffer.Store(offer)
	ingress := []streamForwardingAckIngress{{
		path: contract.path.Reverse().LocalMask(), provenance: origin.provenance(platformReceiveH3Datagram),
		wire: answer.wire[:],
	}}
	// The first two are real native control categories. Neither may publish.
	for _, receipt := range []sequenceAck{
		{messageId: introductionId, sequenceNumber: 12, selective: true},
		{messageId: introductionId, sequenceNumber: 12, contractMissing: true},
		{messageId: NewId(), sequenceNumber: 11},
		{messageId: NewId(), sequenceNumber: 12, selective: true, deliveredImmediately: true},
	} {
		if receiver.streamForwardingDeliveryAcceptance(receipt) != nil {
			t.Fatalf("noncovering receipt produced a forwarding acceptance: %+v", receipt)
		}
		ack := receiveAckMessage{messageId: receipt.messageId, sequenceId: sequenceId, selective: receipt.selective,
			contractMissing: receipt.contractMissing, deliveredImmediately: receipt.deliveredImmediately}
		sender.acceptStreamForwardingAck(ack, receipt.sequenceNumber, true, ingress)
		if offer.published || offer.delivered {
			t.Fatal("source enrolled or retired its introduction on a noncovering receipt")
		}
	}
	// Compression moves this genuine delivery to an ordinal after the offer.
	cumulative := sequenceAck{messageId: NewId(), sequenceNumber: 14}
	if actual := receiver.streamForwardingDeliveryAcceptance(cumulative); !bytes.Equal(actual, answer.wire[:]) {
		t.Fatal("cumulative progress lost the offered answer")
	}
	gotDestination := receiver.client.streamManager.streamBuffer.pendingForwardingEnrollments[contract.path.StreamId]
	if !gotDestination.destination || gotDestination.keys != answer.keys {
		t.Fatal("destination did not stage its exact existing stream authorization")
	}
	// An ordinary duplicate exact delivery may carry enrollment after the
	// sender already applied a bare cumulative ACK to its retained originals.
	sender.acceptStreamForwardingAck(receiveAckMessage{messageId: cumulative.messageId, sequenceId: sequenceId}, 14, true, nil)
	if !offer.delivered || offer.published {
		t.Fatal("ordinary ACK delivery modified enrollment")
	}
	repeated := receiveAckMessage{messageId: introductionId, sequenceId: sequenceId}
	sender.acceptStreamForwardingAck(repeated, 0, false, ingress)
	pending := sender.client.streamManager.streamBuffer.pendingForwardingEnrollments[contract.path.StreamId]
	if !offer.published || pending.destination || pending.keys != gotDestination.keys {
		t.Fatal("late exact authenticated delivery did not enroll the source")
	}
}

// A fresh full signed introduction can enroll on a still-live native
// sequence after the previous grant expires. Late old receipts keep their
// ordinary delivery meaning without authorizing the new association.
func TestStreamForwardingRenewalFollowsSignedGrantExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, now := t.Context(), time.Now()
		path := TransferPath{SourceId: NewId(), DestinationId: NewId(), StreamId: NewId()}
		first := testStreamForwardingContractForPath(t, now, path)
		second := testStreamForwardingContractForPath(t, now.Add(30*time.Second), path)
		sequenceId := NewId()
		origin := newPlatformReceiveOwner(path.SourceId, ctx.Done(), nil, true)
		destination := newPlatformReceiveOwner(path.DestinationId, ctx.Done(), nil, true)
		sender := &SendSequence{
			ctx: ctx, client: testStreamForwardingWireClient(ctx, path.SourceId), destination: path.DestinationId,
			sequenceId: sequenceId, forceStream: true, intermediaryIds: RequireMultiHopId(NewId()),
		}
		receiver := &ReceiveSequence{
			ctx: ctx, client: testStreamForwardingWireClient(ctx, path.DestinationId), sequenceId: sequenceId,
			source: TransferPath{SourceId: path.SourceId},
		}
		owner := &decodedPackOwner{
			receivePack: ReceivePack{carrier: &receivePackCarrier{provenance: destination.provenance(platformReceiveH1)}},
		}
		observeIntroduction := func(contract *sequenceContract, offer *streamForwardingOffer) {
			t.Helper()
			item := &receiveItem{
				transferItem: transferItem{messageId: offer.grant.introductionId, sequenceNumber: offer.sequenceNumber},
				decodedOwner: owner, ack: true,
			}
			receiver.noteStreamForwardingIntroduction(item, contract, offer.wire[:])
		}
		deliverAnswer := func(offer *streamForwardingOffer, acceptance []byte) {
			t.Helper()
			ingress := []streamForwardingAckIngress{{
				path: path.Reverse().LocalMask(), provenance: origin.provenance(platformReceiveH3Datagram),
				wire: acceptance,
			}}
			sender.acceptStreamForwardingAck(receiveAckMessage{messageId: offer.grant.introductionId, sequenceId: sequenceId},
				offer.sequenceNumber, true, ingress)
		}
		receipt := func(offer *streamForwardingOffer) sequenceAck {
			return sequenceAck{messageId: offer.grant.introductionId, sequenceNumber: offer.sequenceNumber}
		}

		firstOffer := sender.streamForwardingIntroduction(first, NewId(), 12, now)
		if firstOffer == nil {
			t.Fatal("initial full grant did not produce an offer")
		}
		observeIntroduction(first, firstOffer)
		firstAnswer := receiver.forwardingAnswer.Load()
		deliverAnswer(firstOffer, receiver.streamForwardingDeliveryAcceptance(receipt(firstOffer)))
		if firstAnswer == nil || !firstAnswer.published || !firstOffer.published {
			t.Fatal("initial delivered grant did not enroll both owners")
		}
		if sender.streamForwardingIntroduction(second, NewId(), 13, now) != nil {
			t.Fatal("unexpired offer was replaced on the same native sequence")
		}
		early := newStreamForwardingOffer(second, sequenceId, NewId(), now)
		early.sequenceNumber = 13
		observeIntroduction(second, early)
		if receiver.forwardingAnswer.Load() != firstAnswer {
			t.Fatal("unexpired answer was replaced on the same native sequence")
		}

		time.Sleep(time.Until(time.UnixMilli(firstOffer.grant.expirationTimeUnixMillis)))
		secondOffer := sender.streamForwardingIntroduction(second, NewId(), 14, time.Now())
		if secondOffer == nil || secondOffer.wire == firstOffer.wire {
			t.Fatal("expired offer blocked a fresh signed introduction on the native sequence")
		}
		observeIntroduction(second, secondOffer)
		secondAnswer := receiver.forwardingAnswer.Load()
		if secondAnswer == nil || secondAnswer == firstAnswer ||
			secondAnswer.keys.transcript == firstAnswer.keys.transcript {
			t.Fatal("expired answer blocked the successor's exact platform proof")
		}
		observeIntroduction(first, firstOffer)
		if receiver.forwardingAnswer.Load() != secondAnswer {
			t.Fatal("late original introduction changed the successor")
		}
		// Keep the new ACK identity but substitute the expired response.
		deliverAnswer(secondOffer, firstAnswer.wire[:])
		if secondOffer.published {
			t.Fatal("old acceptance published the new offer")
		}
		deliverAnswer(secondOffer, receiver.streamForwardingDeliveryAcceptance(receipt(secondOffer)))
		senderPending := sender.client.streamManager.streamBuffer.pendingForwardingEnrollments[path.StreamId]
		receiverPending := receiver.client.streamManager.streamBuffer.pendingForwardingEnrollments[path.StreamId]
		if !secondOffer.published || !secondAnswer.published || senderPending.grant != secondOffer.grant ||
			receiverPending.grant != senderPending.grant || receiverPending.keys != senderPending.keys {
			t.Fatal("successor's ordinary delivery ACK did not publish its new matching grant")
		}
	})
}
