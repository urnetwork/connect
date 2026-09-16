// Synthetic receive-worker checks for cumulative feedback after a data burst.
package connect

import (
	"math"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Delivers one synthetic batch through the production deliver-before-ACK
// handoff. The receive loop is durably blocked on its empty Pack queue.
func deliverBurstTailTestHead(t *testing.T, sequence *ackGapTestSequence, number uint64, byteCount ByteCount) Id {
	t.Helper()
	messageId := NewId()
	deliverBurstTailTestBatch(t, sequence, []*receiveItem{{
		transferItem: transferItem{
			messageId: messageId, sequenceNumber: number, messageByteCount: byteCount,
		},
		ack: true, transportType: TransportTypeH1,
	}})
	return messageId
}

// Uses the production grouped delivery handoff for arbitrary synthetic items.
func deliverBurstTailTestBatch(t *testing.T, sequence *ackGapTestSequence, items []*receiveItem) {
	t.Helper()
	synctest.Wait()
	delivered := false
	items[0].receiveCallback = func(TransferPath, []*protocol.Frame, Peer) { delivered = true }
	sequence.receiveSequence.deliverItems = items
	sequence.receiveSequence.deliverFrames = []*protocol.Frame{{MessageType: protocol.MessageType_IpIpPacketToProvider}}
	sequence.receiveSequence.flushDeliver()
	if !delivered {
		t.Fatal("batch did not reach its application before publishing cumulative progress")
	}
}

// Reads only work made ready by an explicit virtual transition. The worker
// must already have produced this frame; a real-time timeout is not the proof.
func readBurstTailTestAck(t *testing.T, sequence *ackGapTestSequence) *protocol.Ack {
	t.Helper()
	synctest.Wait()
	select {
	case wire := <-sequence.route:
		var frame protocol.TransferFrame
		err := ProtoUnmarshal(wire, &frame)
		size := len(wire)
		MessagePoolReturn(wire)
		if err != nil || size > ackResponseMaxByteCount {
			t.Fatalf("invalid or oversized cumulative response: size=%d err=%v", size, err)
		}
		if 0 < len(frame.EncryptedTransferFrame) {
			if sequence.receiveSequence.session == nil {
				t.Fatal("encrypted response has no synthetic test cipher")
			}
			opened, openErr := sequence.receiveSequence.session.Cipher().Open(frame.EncryptedTransferFrame)
			if openErr != nil {
				t.Fatal(openErr)
			}
			frame.Reset()
			err = ProtoUnmarshal(opened, &frame)
			MessagePoolReturn(opened)
			if err != nil {
				t.Fatal(err)
			}
		}
		if frame.Ack == nil && frame.Frame != nil && frame.Frame.MessageType == protocol.MessageType_TransferAck {
			frame.Ack = &protocol.Ack{}
			if err := ProtoUnmarshal(frame.Frame.MessageBytes, frame.Ack); err != nil {
				t.Fatal(err)
			}
		}
		if frame.Ack == nil {
			t.Fatal("response contained no acknowledgement")
		}
		if len(frame.Ack.EvictedSequenceNumbers) == 0 && size > ackResponseEntryMaxByteCount {
			t.Fatalf("head-only encoded carrier costs %d, above its reserved %d bytes", size, ackResponseEntryMaxByteCount)
		}
		return frame.Ack
	default:
		t.Fatal("delivered burst tail is still waiting for the full compression interval")
		return nil
	}
}

// Repeated head-only turns absorb old selective evidence, while newer SACKs
// and eviction metadata retain the original absolute compression deadline.
func TestReceiveSequenceBurstTailPreservesFeedbackDeadline(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		sequence := newAckGapTestSequence(t, func(settings *ReceiveBufferSettings) {
			settings.AckCompressTimeout = 10 * time.Millisecond
			settings.AckGapWakeSelectiveCount = 0
		})
		sequence.prime(t)
		start := time.Now()
		wantSacks := []Id{sequence.updateSelective(50), sequence.updateSelective(100)}
		sequence.updateSelective(3)
		sequence.receiveSequence.noteEviction(200)
		for _, number := range []uint64{4, 8, 12, 16} {
			want := deliverBurstTailTestHead(t, sequence, number, 100*ackResponseEntryMaxByteCount)
			time.Sleep(time.Millisecond)
			ack := readBurstTailTestAck(t, sequence)
			got, err := IdFromBytes(ack.MessageId)
			if err != nil || got != want || ack.Selective || len(ack.EvictedSequenceNumbers) != 0 {
				t.Fatalf("head-only turn emitted unrelated feedback: %v", ack)
			}
			if len(sequence.route) != 0 {
				t.Fatal("head-only turn emitted more than one carrier")
			}
			time.Sleep(time.Millisecond)
		}
		time.Sleep(time.Until(start.Add(10 * time.Millisecond)))
		for index, want := range wantSacks {
			ack := readBurstTailTestAck(t, sequence)
			got, err := IdFromBytes(ack.MessageId)
			if err != nil || got != want || !ack.Selective {
				t.Fatalf("absolute deadline lost or reordered selective evidence: %v", ack)
			}
			if index == 0 && !slices.Equal(ack.EvictedSequenceNumbers, []uint64{200}) {
				t.Fatalf("absolute deadline lost eviction evidence: %v", ack)
			}
		}
		if len(sequence.route) != 0 {
			t.Fatal("cumulative progress failed to absorb an older selective ACK")
		}
	})
}

// Equality earns one quiet head. A large previous head, duplicate deliveries,
// and retried heads do not leave reusable credit for later small advances.
func TestReceiveSequenceBurstTailSpendsOnlyNewBytes(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		sequence := newAckGapTestSequence(t, func(settings *ReceiveBufferSettings) {
			settings.AckCompressTimeout = 10 * time.Millisecond
			settings.AckGapWakeSelectiveCount = 0
		})
		sequence.prime(t)
		start := time.Now()
		deliverBurstTailTestHead(t, sequence, 1, 100*ackResponseEntryMaxByteCount-1)
		time.Sleep(time.Millisecond)
		synctest.Wait()
		if len(sequence.route) != 0 {
			t.Fatal("just-below-threshold progress bought a head-only carrier")
		}
		want := deliverBurstTailTestHead(t, sequence, 2, 1)
		time.Sleep(time.Millisecond)
		ack := readBurstTailTestAck(t, sequence)
		got, err := IdFromBytes(ack.MessageId)
		if err != nil || got != want {
			t.Fatal("threshold equality did not acknowledge the newest head")
		}
		for range 4 {
			deliverBurstTailTestHead(t, sequence, 2, 1000*ackResponseEntryMaxByteCount)
		}
		want = deliverBurstTailTestHead(t, sequence, 3, 1)
		time.Sleep(time.Millisecond)
		synctest.Wait()
		if len(sequence.route) != 0 {
			t.Fatal("duplicate progress bought another head-only carrier")
		}
		time.Sleep(time.Until(start.Add(10 * time.Millisecond)))
		ack = readBurstTailTestAck(t, sequence)
		got, err = IdFromBytes(ack.MessageId)
		if err != nil || got != want {
			t.Fatal("ordinary deadline lost the small advance after duplicate heads")
		}
	})
}

// Arrival activity can move the quiet deadline, but never the configured
// compression deadline, even when a large window keeps arrivals continuous.
func TestReceiveSequenceBurstTailContinuousProgressKeepsDeadline(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		sequence := newAckGapTestSequence(t, func(settings *ReceiveBufferSettings) {
			settings.AckCompressTimeout = 10 * time.Millisecond
			settings.AckGapWakeSelectiveCount = 0
		})
		sequence.prime(t)
		var want Id
		for number := uint64(1); number <= 20; number++ {
			want = deliverBurstTailTestHead(t, sequence, number, 1024*1024)
			time.Sleep(500 * time.Microsecond)
			synctest.Wait()
			if number < 20 && len(sequence.route) != 0 {
				t.Fatalf("continuous progress was called quiet after head %d", number)
			}
		}
		ack := readBurstTailTestAck(t, sequence)
		got, err := IdFromBytes(ack.MessageId)
		if err != nil || got != want || len(sequence.route) != 0 {
			t.Fatal("continuous progress postponed or duplicated the original full response")
		}
	})
}

// All bytes in a delivery group are counted once, including an item bigger
// than the threshold. No-ACK traffic and non-H1 deliveries buy no early head.
func TestReceiveSequenceBurstTailCountsGroupedH1DeliveryOnce(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		sequence := newAckGapTestSequence(t, func(settings *ReceiveBufferSettings) {
			settings.AckCompressTimeout = 10 * time.Millisecond
			settings.AckGapWakeSelectiveCount = 0
		})
		sequence.prime(t)
		items := []*receiveItem{}
		for number := uint64(1); number <= 4; number++ {
			items = append(items, &receiveItem{
				transferItem: transferItem{messageId: NewId(), sequenceNumber: number, messageByteCount: 25 * ackResponseEntryMaxByteCount},
				ack:          true, transportType: TransportTypeH1,
			})
		}
		want := items[len(items)-1].messageId
		deliverBurstTailTestBatch(t, sequence, items)
		time.Sleep(time.Millisecond)
		ack := readBurstTailTestAck(t, sequence)
		got, err := IdFromBytes(ack.MessageId)
		if err != nil || got != want {
			t.Fatal("grouped delivery failed to accumulate one cumulative quantum")
		}
		deliverBurstTailTestBatch(t, sequence, []*receiveItem{
			{transferItem: transferItem{messageId: NewId(), sequenceNumber: 5, messageByteCount: 1024 * 1024}, transportType: TransportTypeH1},
			{transferItem: transferItem{messageId: NewId(), sequenceNumber: 6, messageByteCount: 1024 * 1024}, ack: true, transportType: TransportTypeH3},
		})
		time.Sleep(time.Millisecond)
		synctest.Wait()
		if len(sequence.route) != 0 {
			t.Fatal("no-ACK or non-H1 delivery earned H1 burst-tail credit")
		}
	})
}

// Disabled compression stays immediate; a fractional-nanosecond quiet
// interval rounds up and cannot beat the original one-nanosecond deadline.
func TestReceiveSequenceBurstTailCompressionSettingBoundaries(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, timeout := range []time.Duration{-time.Nanosecond, 0, time.Nanosecond} {
		synctest.Test(t, func(t *testing.T) {
			sequence := newAckGapTestSequence(t, func(settings *ReceiveBufferSettings) {
				settings.AckCompressTimeout = timeout
				settings.AckGapWakeSelectiveCount = 0
			})
			sequence.prime(t)
			want := deliverBurstTailTestHead(t, sequence, 1, 100*ackResponseEntryMaxByteCount)
			if timeout > 0 {
				time.Sleep(timeout)
			}
			ack := readBurstTailTestAck(t, sequence)
			got, err := IdFromBytes(ack.MessageId)
			if err != nil || got != want {
				t.Fatalf("compression %s lost its immediate/deadline head", timeout)
			}
		})
	}
}

// The complete encoded carrier, including encryption and legacy wrapping,
// stays below the response entry bound used to price extra head traffic.
func TestReceiveSequenceBurstTailEncodedCostIncludesWrapping(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{1, 2} {
		for _, encrypted := range []bool{false, true} {
			synctest.Test(t, func(t *testing.T) {
				sequence := newAckGapTestSequence(t, func(settings *ReceiveBufferSettings) {
					settings.AckCompressTimeout = 10 * time.Millisecond
					settings.AckGapWakeSelectiveCount = 0
					settings.ProtocolVersion = version
					settings.AdvertiseReceiveWindow = true
					settings.ReceiveQueueMaxByteCount = math.MaxInt64
				})
				sequence.updateHead(0)
				readBurstTailTestAck(t, sequence)
				if encrypted {
					sequence.receiveSequence.session = &peerEncryptionSession{
						client: sequence.receiveSequence.client, role: sequenceTlsRoleServer, companion: true,
						establishedEpoch: &tlsHandshakeEpoch{peerIdentityVerified: true, derivedTlsCipher: newFrameCodecTestSequenceCipher(t)},
					}
					t.Cleanup(func() { sequence.receiveSequence.session = nil })
				}
				item := &receiveItem{
					transferItem: transferItem{messageId: NewId(), sequenceNumber: math.MaxUint64, messageByteCount: 100 * ackResponseEntryMaxByteCount},
					ack:          true, transportType: TransportTypeH1, tag: sequenceTag{set: true, sendTime: math.MaxUint64},
				}
				want := item.messageId
				deliverBurstTailTestBatch(t, sequence, []*receiveItem{item})
				time.Sleep(time.Millisecond)
				ack := readBurstTailTestAck(t, sequence)
				got, err := IdFromBytes(ack.MessageId)
				if err != nil || got != want || ack.Selective || len(ack.EvictedSequenceNumbers) != 0 {
					t.Fatalf("version=%d encrypted=%t head carrier was not cumulative", version, encrypted)
				}
			})
		}
	}
}

// A data lull remains a zero-wait first-ACK turn. Spent credit from the large
// earlier burst cannot authorize another early ACK for one small new item.
func TestReceiveSequenceBurstTailIdleDoesNotReuseCredit(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		sequence := newAckGapTestSequence(t, func(settings *ReceiveBufferSettings) {
			settings.AckCompressTimeout = 10 * time.Millisecond
			settings.AckGapWakeSelectiveCount = 0
		})
		sequence.prime(t)
		deliverBurstTailTestHead(t, sequence, 1, 1024*1024)
		time.Sleep(time.Millisecond)
		readBurstTailTestAck(t, sequence)
		time.Sleep(20 * time.Millisecond)
		want := deliverBurstTailTestHead(t, sequence, 2, 1)
		ack := readBurstTailTestAck(t, sequence)
		got, err := IdFromBytes(ack.MessageId)
		if err != nil || got != want {
			t.Fatal("first small head after idle did not leave immediately")
		}
		deliverBurstTailTestHead(t, sequence, 3, 1)
		time.Sleep(time.Millisecond)
		synctest.Wait()
		if len(sequence.route) != 0 {
			t.Fatal("large old burst left early-flush credit after idle")
		}
	})
}

// Cancellation drains delivered progress promptly even while its quiet-time
// deadline is pending, and does not leave another ACK behind the stop edge.
func TestReceiveSequenceBurstTailCancellationDrainsPendingHead(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		waiting := make(chan struct{})
		release := make(chan struct{})
		var once sync.Once
		sequence := newAckGapTestSequence(t, func(settings *ReceiveBufferSettings) {
			settings.AckCompressTimeout = 10 * time.Millisecond
			settings.AckGapWakeSelectiveCount = 0
			settings.beforeAckCompressWaitForTest = func(receiveSequenceId) {
				once.Do(func() { close(waiting); <-release })
			}
		})
		sequence.prime(t)
		at := time.Now()
		want := deliverBurstTailTestHead(t, sequence, 1, 100*ackResponseEntryMaxByteCount)
		<-waiting
		sequence.receiveSequence.Cancel()
		close(release)
		<-sequence.receiveSequence.exit
		ack := readBurstTailTestAck(t, sequence)
		got, err := IdFromBytes(ack.MessageId)
		if err != nil || got != want || !time.Now().Equal(at) || len(sequence.route) != 0 {
			t.Fatal("canceled quiet wait postponed, lost or repeated its final head")
		}
	})
}

// One plaintext item in a cumulative burst keeps the entire response
// plaintext, even when a later item and the local session support encryption.
func TestReceiveSequenceBurstTailPreservesPlaintextCumulativeHead(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		sequence := newAckGapTestSequence(t, func(settings *ReceiveBufferSettings) {
			settings.AckCompressTimeout = 10 * time.Millisecond
			settings.AckGapWakeSelectiveCount = 0
		})
		sequence.prime(t)
		synctest.Wait()
		sequence.receiveSequence.session = &peerEncryptionSession{
			client: sequence.receiveSequence.client, role: sequenceTlsRoleServer,
			establishedEpoch: &tlsHandshakeEpoch{peerIdentityVerified: true, derivedTlsCipher: newFrameCodecTestSequenceCipher(t)},
		}
		t.Cleanup(func() { sequence.receiveSequence.session = nil })
		items := []*receiveItem{
			{transferItem: transferItem{messageId: NewId(), sequenceNumber: 1, messageByteCount: 50 * ackResponseEntryMaxByteCount}, ack: true, transportType: TransportTypeH1, unwrapped: true},
			{transferItem: transferItem{messageId: NewId(), sequenceNumber: 2, messageByteCount: 50 * ackResponseEntryMaxByteCount}, ack: true, transportType: TransportTypeH1},
		}
		want := items[1].messageId
		deliverBurstTailTestBatch(t, sequence, items)
		time.Sleep(time.Millisecond)
		synctest.Wait()
		select {
		case wire := <-sequence.route:
			var frame protocol.TransferFrame
			err := ProtoUnmarshal(wire, &frame)
			MessagePoolReturn(wire)
			if err != nil || len(frame.EncryptedTransferFrame) != 0 || frame.Ack == nil {
				t.Fatal("mixed cumulative burst upgraded its plaintext ACK")
			}
			got, err := IdFromBytes(frame.Ack.MessageId)
			if err != nil || got != want {
				t.Fatal("plaintext cumulative response lost its newest head")
			}
		default:
			t.Fatal("mixed cumulative burst did not publish its quiet head")
		}
	})
}

// A full data quantum followed by a quiet tenth of the configured interval
// must release the sender's finite flight, without waiting the remaining nine
// tenths. The compression hook proves the ACK worker saw the pending head.
func TestReceiveSequenceBurstTailHeadDoesNotWaitForCompression(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, timeout := range []time.Duration{10 * time.Millisecond, 50 * time.Millisecond} {
		synctest.Test(t, func(t *testing.T) {
			waiting := make(chan struct{})
			release := make(chan struct{})
			var waitingOnce sync.Once
			sequence := newAckGapTestSequence(t, func(settings *ReceiveBufferSettings) {
				settings.AckCompressTimeout = timeout
				settings.AckGapWakeSelectiveCount = 0
				settings.beforeAckCompressWaitForTest = func(receiveSequenceId) {
					waitingOnce.Do(func() { close(waiting); <-release })
				}
			})
			sequence.prime(t)
			messageId := deliverBurstTailTestHead(t, sequence, 1, 100*ackResponseEntryMaxByteCount)
			<-waiting
			close(release)
			time.Sleep(timeout / 10)
			ack := readBurstTailTestAck(t, sequence)
			got, err := IdFromBytes(ack.MessageId)
			if err != nil || got != messageId || ack.Selective || len(ack.EvictedSequenceNumbers) != 0 {
				t.Fatalf("quiet burst emitted the wrong head: %v", ack)
			}
		})
	}
}
