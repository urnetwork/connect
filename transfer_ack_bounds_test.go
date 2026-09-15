package connect

import (
	"math"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// An eviction generation is queued independently of the acknowledgements
// carrying it. Long-lived sequences use ten-byte varints, not the two-byte
// values of a freshly started throughput run.
func TestEvictionAcknowledgementsFitEveryCarrier(t *testing.T) {
	assertMessagePoolOwnership(t)
	sequence := &ReceiveSequence{client: &Client{}}
	for i := range evictionNoticeMaxCount {
		sequence.noteEviction(math.MaxUint64 - uint64(i))
	}
	cipher := newFrameCodecTestSequenceCipher(t)
	path := TransferPath{SourceId: NewId(), DestinationId: NewId(), StreamId: NewId()}
	seen := make(map[uint64]bool)
	for {
		evicted := sequence.takeEvictions()
		if len(evicted) == 0 {
			break
		}
		missing := NewId()
		ack := sendAckFrame{
			path: path, messageId: NewId(), sequenceId: NewId(), selective: true,
			tagSet: true, tagSendTime: math.MaxUint64, missingContractId: &missing,
			compactContractRecovery: true, contractAhead: true,
			logicalLaneVersion: math.MaxUint32, receiveWindowSet: true,
			receiveWindowByteCount: math.MaxUint64,
			ackCompressTimeoutSet:  true, ackCompressTimeoutMicros: math.MaxUint32,
			evictedSequenceNumbers: evicted,
		}
		for _, version := range []int{1, 2} {
			var inner []byte
			if version == 2 {
				inner = marshalSendAckTransferFrame(&ack)
			} else {
				body, err := ProtoMarshal(buildEquivalentAckFrame(&ack).Ack)
				if err != nil {
					t.Fatal(err)
				}
				inner, err = ProtoMarshal(&protocol.TransferFrame{
					TransferPath: path.ToProtobuf(),
					Frame:        &protocol.Frame{MessageType: protocol.MessageType_TransferAck, MessageBytes: body},
				})
				MessagePoolReturn(body)
				if err != nil {
					t.Fatal(err)
				}
			}
			wrapped, err := cipher.SealOuterFrame(path, inner, protocol.SequenceRole_SequenceRoleServer, true)
			MessagePoolReturn(inner)
			if err != nil {
				t.Fatal(err)
			}
			size := len(wrapped)
			MessagePoolReturn(wrapped)
			if size > ackResponseEntryMaxByteCount+10*len(evicted) {
				t.Fatalf("version %d exceeds per-entry reservation: %d bytes", version, size)
			}
			if size > int(DefaultClientSettings().MinimumMessageLenLimit()) {
				t.Fatalf("version %d: %d notices produced %d bytes, exceeding minimum carrier limit", version, len(evicted), size)
			}
		}
		for _, number := range evicted {
			if seen[number] {
				t.Fatalf("duplicate eviction %d", number)
			}
			seen[number] = true
		}
	}
	if len(seen) != evictionNoticeMaxCount {
		t.Fatalf("lost overflow notices: delivered %d of %d", len(seen), evictionNoticeMaxCount)
	}
}

func TestAckResponsesKeepOrderedOverflow(t *testing.T) {
	window := newSequenceAckWindow()
	window.Update(sequenceAck{messageId: NewId()})
	for number := uint64(4096); number != 0; number-- {
		window.Update(sequenceAck{sequenceNumber: number, messageId: NewId(), selective: true})
	}
	var scratch [ackResponseMaxCount]sequenceAck
	next := uint64(0)
	for window.Pending() {
		acks, _ := window.takeResponse(scratch[:0], ackResponseMaxCount)
		if len(acks) == 0 || len(acks) > ackResponseMaxCount {
			t.Fatalf("unbounded/empty response: %d", len(acks))
		}
		for _, ack := range acks {
			if ack.sequenceNumber != next {
				t.Fatalf("response reordered %d before %d", ack.sequenceNumber, next)
			}
			next++
		}
	}
	if next != 4097 {
		t.Fatalf("lost overflow: delivered %d", next)
	}
	for number := uint64(4097); number < 4200; number++ {
		window.Update(sequenceAck{sequenceNumber: number, messageId: NewId(), selective: true})
	}
	window.Update(sequenceAck{sequenceNumber: 4199, messageId: NewId()})
	acks, _ := window.takeResponse(scratch[:0], ackResponseMaxCount)
	if len(acks) != 1 || acks[0].selective || window.Pending() {
		t.Fatal("new head did not absorb unsent overflow")
	}
}

func TestAckOverflowDoesNotAddCompressionIntervals(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		sequence := newAckGapTestSequence(t, func(settings *ReceiveBufferSettings) {
			settings.AckCompressTimeout = 10 * time.Millisecond
			settings.AckGapWakeSelectiveCount = 0
		})
		sequence.prime(t)
		for i := uint64(1); i <= 100; i++ {
			sequence.updateSelective(i)
		}
		time.Sleep(10 * time.Millisecond)
		synctest.Wait()
		if len(sequence.route) != 100 {
			t.Fatalf("bounded response chunks added compression residence: %d/100 ACKs after one interval", len(sequence.route))
		}
	})
}

func TestAckWorkerBoundsEveryResponseAndFinalDrain(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		sequence := newAckGapTestSequence(t, func(settings *ReceiveBufferSettings) {
			settings.AckCompressTimeout = 10 * time.Millisecond
			settings.AckGapWakeSelectiveCount = 0
			settings.EvictionNotice = true
		})
		sequence.prime(t)
		synctest.Wait()
		evictions, selective, missing, responses := 0, 0, 0, 0
		sequence.settings.afterAckWriteForTest = func(receiveSequenceId) {
			count, size := 0, 0
			for len(sequence.route) != 0 {
				b := <-sequence.route
				count++
				size += len(b)
				var frame protocol.TransferFrame
				err := ProtoUnmarshal(b, &frame)
				MessagePoolReturn(b)
				if err != nil || frame.Ack == nil {
					t.Error("invalid response entry")
					continue
				}
				evictions += len(frame.Ack.EvictedSequenceNumbers)
				if frame.Ack.Selective {
					selective++
				}
				if len(frame.Ack.MissingContractId) != 0 {
					missing++
				}
			}
			if count > ackResponseMaxCount || size > ackResponseMaxByteCount {
				t.Errorf("response exceeded bounds: %d entries, %d bytes", count, size)
			}
			responses++
			if responses == 1 {
				if evictions != evictionNoticeAckMaxCount || !sequence.receiveSequence.ackWindow.Pending() {
					t.Error("first response lost overflow")
				}
				sequence.receiveSequence.Cancel() // force final drain with overflow pending
			}
		}
		for i := range evictionNoticeMaxCount {
			sequence.receiveSequence.noteEviction(math.MaxUint64 - uint64(i))
		}
		for i := uint64(1); i <= 200; i++ {
			sequence.updateSelective(i)
		}
		for range 20 {
			sequence.receiveSequence.ackWindow.UpdateContractMissing(sequenceAck{messageId: NewId(), missingContractId: NewId()})
		}
		time.Sleep(10 * time.Millisecond)
		synctest.Wait()
		<-sequence.receiveSequence.exit
		if evictions != evictionNoticeMaxCount || selective != 200 || missing != 20 {
			t.Fatalf("final drain lost evidence: notices=%d selective=%d contract=%d", evictions, selective, missing)
		}
	})
}
