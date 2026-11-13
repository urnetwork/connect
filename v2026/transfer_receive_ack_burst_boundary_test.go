// A compression deadline can bisect a small-window refill without consuming
// the delivered-byte allowance for a later extra cumulative response.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// Two sub-quantum halves straddle an ordinary compression response. The real
// delivery handoff and ACK worker must still release the quiet refill tail.
func TestReceiveSequenceBurstTailCompressionBoundaryKeepsCredit(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		const compression = 50 * time.Millisecond
		const frameBytes ByteCount = 3961
		sequence := newAckGapTestSequence(t, func(settings *ReceiveBufferSettings) {
			settings.AckCompressTimeout = compression
			settings.AckGapWakeSelectiveCount = 0
		})
		sequence.prime(t)
		start := time.Now()
		deliver := func(first uint64) Id {
			items := make([]*receiveItem, 8)
			for i := range items {
				items[i] = &receiveItem{
					transferItem: transferItem{messageId: NewId(), sequenceNumber: first + uint64(i), messageByteCount: frameBytes},
					ack:          true, transportType: TransportTypeH1,
				}
			}
			last := items[len(items)-1].messageId
			deliverBurstTailTestBatch(t, sequence, items)
			return last
		}
		time.Sleep(49 * time.Millisecond)
		first := deliver(1)
		time.Sleep(time.Until(start.Add(compression)))
		ack := readBurstTailTestAck(t, sequence)
		got, err := IdFromBytes(ack.MessageId)
		if err != nil || got != first || time.Since(start) != compression {
			t.Fatal("the first half did not keep its ordinary compression deadline")
		}
		time.Sleep(time.Millisecond)
		last := deliver(9)
		time.Sleep(compression / 10)
		ack = readBurstTailTestAck(t, sequence)
		got, err = IdFromBytes(ack.MessageId)
		if err != nil || got != last || time.Since(start) != 56*time.Millisecond {
			t.Fatal("the split refill did not publish its quiet cumulative tail")
		}
		if len(sequence.route) != 0 || 100*ackResponseEntryMaxByteCount > 16*frameBytes {
			t.Fatal("the split refill duplicated a head or exceeded its extra-response byte allowance")
		}
	})
}

// An unspent timed-response allowance cannot turn later duplicate, restored
// or non-H1 feedback into a newly delivered H1 burst or consume its wake.
func TestSequenceAckCompressionCreditRequiresNewH1Delivery(t *testing.T) {
	for _, control := range []string{"duplicate", "restore", "non-h1"} {
		window := newSequenceAckWindow()
		head := sequenceAck{sequenceNumber: 1, messageId: NewId(), transportType: TransportTypeH1}
		window.UpdateDeliveredPrefix(func() { window.updateDeliveredWithLock(head, ackBurstTailByteCount) })
		var scratch [ackResponseMaxCount]sequenceAck
		acks, overflow := window.takeResponseKeepingBurstCredit(scratch[:0], len(scratch), true)
		if len(acks) != 1 || overflow || window.PendingCumulativeProgress() {
			t.Fatalf("%s: timed response did not consume its exact pending head", control)
		}
		switch control {
		case "duplicate":
			window.Update(head)
		case "restore":
			window.Restore(acks)
		case "non-h1":
			head.sequenceNumber, head.messageId, head.transportType = 2, NewId(), TransportTypeH3
			window.UpdateDeliveredPrefix(func() { window.updateDeliveredWithLock(head, ackBurstTailByteCount) })
		}
		if _, ready, _ := window.takeQuietHead(time.Now(), 0); ready || !window.Pending() {
			t.Fatalf("%s: old allowance authorized an early head or discarded its pending response", control)
		}
		select {
		case <-window.HeadQuietNotify():
			t.Fatalf("%s: old allowance signaled without a fresh H1 delivery", control)
		default:
		}
		head.sequenceNumber, head.messageId, head.transportType = 3, NewId(), TransportTypeH1
		window.UpdateDeliveredPrefix(func() { window.updateDeliveredWithLock(head, 1) })
		select {
		case <-window.HeadQuietNotify():
		default:
			t.Fatalf("%s: fresh H1 delivery did not wake a previously funded quiet head", control)
		}
		if ack, ready, _ := window.takeQuietHead(time.Now(), 0); !ready || ack.messageId != head.messageId {
			t.Fatalf("%s: fresh H1 delivery did not retain the exact newest head", control)
		}
	}
}

// Carrying an allowance across scheduled responses neither lowers its byte
// quantum nor banks more than one turn from a large previous delivery.
func TestSequenceAckCompressionCreditKeepsItsByteBound(t *testing.T) {
	for _, firstBytes := range []ByteCount{1, ackBurstTailByteCount / 2, ackBurstTailByteCount - 1, 100 * ackBurstTailByteCount} {
		window := newSequenceAckWindow()
		head := sequenceAck{sequenceNumber: 1, messageId: NewId(), transportType: TransportTypeH1}
		deliver := func(bytes ByteCount) {
			window.UpdateDeliveredPrefix(func() { window.updateDeliveredWithLock(head, bytes) })
		}
		deliver(firstBytes)
		var scratch [ackResponseMaxCount]sequenceAck
		window.takeResponseKeepingBurstCredit(scratch[:0], len(scratch), true)
		if firstBytes < ackBurstTailByteCount {
			head.sequenceNumber, head.messageId = 2, NewId()
			deliver(ackBurstTailByteCount - firstBytes - 1)
			if _, ready, _ := window.takeQuietHead(time.Now(), 0); ready {
				t.Fatalf("first-bytes=%d: a sub-quantum split bought an extra response", firstBytes)
			}
		}
		head.sequenceNumber, head.messageId = 3, NewId()
		deliver(1)
		if ack, ready, _ := window.takeQuietHead(time.Now(), 0); !ready || ack.messageId != head.messageId {
			t.Fatalf("first-bytes=%d: the complete byte quantum lost its extra response", firstBytes)
		}
		head.sequenceNumber, head.messageId = 4, NewId()
		deliver(1)
		if _, ready, _ := window.takeQuietHead(time.Now(), 0); ready {
			t.Fatalf("first-bytes=%d: consumed credit bought a second extra response", firstBytes)
		}
	}
}

// A lull still starts an immediate response. Any allowance preserved from
// the earlier timed response must expire at that new idle boundary.
func TestReceiveSequenceBurstTailCompressionCreditExpiresAtIdle(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		const compression = 50 * time.Millisecond
		sequence := newAckGapTestSequence(t, func(settings *ReceiveBufferSettings) {
			settings.AckCompressTimeout = compression
			settings.AckGapWakeSelectiveCount = 0
		})
		sequence.prime(t)
		start := time.Now()
		time.Sleep(49 * time.Millisecond)
		deliverBurstTailTestHead(t, sequence, 1, ackBurstTailByteCount)
		time.Sleep(time.Until(start.Add(compression)))
		readBurstTailTestAck(t, sequence)
		time.Sleep(compression + time.Millisecond)
		first := deliverBurstTailTestHead(t, sequence, 2, 1)
		ack := readBurstTailTestAck(t, sequence)
		if RequireIdFromBytes(ack.MessageId) != first {
			t.Fatal("the first small head after idle did not leave immediately")
		}
		last := deliverBurstTailTestHead(t, sequence, 3, 1)
		time.Sleep(compression / 10)
		synctest.Wait()
		if len(sequence.route) != 0 {
			t.Fatal("an old timed response left extra-head credit beyond idle")
		}
		time.Sleep(time.Until(start.Add(3*compression + time.Millisecond)))
		ack = readBurstTailTestAck(t, sequence)
		if RequireIdFromBytes(ack.MessageId) != last || len(sequence.route) != 0 {
			t.Fatal("the new compression deadline lost or duplicated its small pending head")
		}
	})
}
