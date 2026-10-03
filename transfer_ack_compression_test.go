package connect

import (
	"math"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

func TestAckCompressionEnforcesItsOwnResponseCountLimit(t *testing.T) {
	window := newSequenceAckWindow()
	for number := uint64(1); number <= 100; number++ {
		window.Update(sequenceAck{sequenceNumber: number, messageId: NewId(), selective: true})
	}
	acks, overflow := window.takeResponse(nil, math.MaxInt)
	if len(acks) != ackResponseMaxCount || !overflow {
		t.Fatalf("caller bypassed compression response limit: %d ACKs, overflow=%t", len(acks), overflow)
	}
}

// The common in-order path must not allocate a snapshot map or response slice.
func TestAckCompressionHeadDrainDoesNotAllocate(t *testing.T) {
	window := newSequenceAckWindow()
	ack := sequenceAck{messageId: NewId()}
	var scratch [ackResponseMaxCount]sequenceAck
	allocations := testing.AllocsPerRun(1000, func() {
		ack.sequenceNumber++
		window.Update(ack)
		acks, overflow := window.takeResponse(scratch[:0], ackResponseMaxCount)
		if len(acks) != 1 || overflow {
			t.Fatal("in-order head did not drain alone")
		}
	})
	if allocations != 0 {
		t.Fatalf("in-order ACK compression allocated %.1f objects", allocations)
	}
}

// Reports the common cumulative-head cost separately from a maximum gap burst.
func BenchmarkAckCompressionHead(b *testing.B) {
	window := newSequenceAckWindow()
	ack := sequenceAck{messageId: NewId()}
	var scratch [ackResponseMaxCount]sequenceAck
	b.ReportAllocs()
	for b.Loop() {
		ack.sequenceNumber++
		window.Update(ack)
		window.takeResponse(scratch[:0], ackResponseMaxCount)
	}
}

// Includes bounded selection and all overflow responses, with reverse-order
// input and reused storage. The metric counts the entire 4,096-SACK burst.
func BenchmarkAckCompressionGapBurst(b *testing.B) {
	window := newSequenceAckWindow()
	acks := make([]sequenceAck, 4096)
	for i := range acks {
		acks[i] = sequenceAck{messageId: NewId(), sequenceNumber: uint64(len(acks) - i), selective: true}
	}
	var scratch [ackResponseMaxCount]sequenceAck
	b.ReportAllocs()
	for b.Loop() {
		for _, ack := range acks {
			window.Update(ack)
		}
		for window.Pending() {
			window.takeResponse(scratch[:0], ackResponseMaxCount)
		}
	}
	b.ReportMetric(float64(len(acks)), "acks/op")
}

func TestAckCompressionOneHeadThenOldestSacksAboveHead(t *testing.T) {
	window := newSequenceAckWindow()
	// Interleave cumulative progress with reverse-order SACKs, including
	// redundant evidence below the final head and enough overflow to split.
	for number := uint64(100); number != 0; number-- {
		window.Update(sequenceAck{sequenceNumber: number, messageId: NewId(), selective: true})
	}
	for _, head := range []uint64{3, 7, 5, 10, 10} {
		window.Update(sequenceAck{sequenceNumber: head, messageId: NewId()})
	}
	var scratch [7]sequenceAck
	next, heads := uint64(11), 0
	for window.Pending() {
		acks, overflow := window.takeResponse(scratch[:0], len(scratch))
		if len(acks) == 0 || len(acks) > len(scratch) {
			t.Fatalf("response count %d exceeds bound %d", len(acks), len(scratch))
		}
		for index, ack := range acks {
			if !ack.selective {
				heads++
				if heads != 1 || index != 0 || ack.sequenceNumber != 10 {
					t.Fatalf("compression emitted extra/stale head: %+v", ack)
				}
			} else {
				if ack.sequenceNumber != next {
					t.Fatalf("SACK %d, want oldest unabsorbed %d", ack.sequenceNumber, next)
				}
				next++
			}
		}
		if overflow != window.Pending() {
			t.Fatal("bounded drain lost pending overflow")
		}
	}
	if heads != 1 || next != 101 {
		t.Fatalf("head/SACK compression lost evidence: heads=%d next=%d", heads, next)
	}
}

func TestAckCompressionAdvancingHeadAbsorbsUnsentSacks(t *testing.T) {
	window := newSequenceAckWindow()
	window.Update(sequenceAck{sequenceNumber: 2, messageId: NewId()})
	for number := uint64(20); number > 2; number-- {
		window.Update(sequenceAck{sequenceNumber: number, messageId: NewId(), selective: true})
	}
	var scratch [4]sequenceAck
	window.takeResponse(scratch[:0], len(scratch)) // head 2, SACKs 3, 4, 5
	window.Update(sequenceAck{sequenceNumber: 18, messageId: NewId()})
	acks, overflow := window.takeResponse(scratch[:0], len(scratch))
	if len(acks) != 3 || overflow || window.Pending() || acks[0].selective ||
		acks[0].sequenceNumber != 18 || !acks[1].selective || acks[1].sequenceNumber != 19 ||
		!acks[2].selective || acks[2].sequenceNumber != 20 {
		t.Fatalf("head failed to absorb pending SACKs through 18: %+v, overflow=%t", acks, overflow)
	}
}

// New SACKs arriving just after a compression turn wait for its next deadline.
// Each deadline drains its existing overflow in bounded, oldest-first pieces;
// splitting a large response must not add another interval per piece.
func TestAckCompressionPacesSacksAtCompressionDeadlines(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		sequence := newAckGapTestSequence(t, func(settings *ReceiveBufferSettings) {
			settings.AckCompressTimeout = 10 * time.Millisecond
			settings.AckGapWakeSelectiveCount = 0
		})
		sequence.prime(t)
		for _, first := range []uint64{1, 101} {
			for number := first + 99; number >= first; number-- {
				sequence.updateSelective(number)
			}
			time.Sleep(9 * time.Millisecond)
			synctest.Wait()
			if len(sequence.route) != 0 {
				t.Fatal("new SACK burst escaped its compression deadline")
			}
			time.Sleep(time.Millisecond)
			synctest.Wait()
			if len(sequence.route) != 100 {
				t.Fatalf("SACK overflow added compression intervals: %d/100 at deadline", len(sequence.route))
			}
			for range 100 {
				MessagePoolReturn(<-sequence.route)
			}
		}
	})
}

// With no SACKs to carry overflow, eviction metadata needs another head
// carrier. It must wait for the next compression turn instead of repeating
// the same cumulative head in a burst.
func TestAckCompressionPacesEvictionOnlyHeadCarriers(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		sequence := newAckGapTestSequence(t, func(settings *ReceiveBufferSettings) {
			settings.AckCompressTimeout = 10 * time.Millisecond
			settings.AckGapWakeSelectiveCount = 0
			settings.EvictionNotice = true
		})
		sequence.prime(t)
		for i := range evictionNoticeMaxCount {
			sequence.receiveSequence.noteEviction(math.MaxUint64 - uint64(i))
		}
		head := sequence.updateHead(1)
		seen := make(map[uint64]bool)
		for len(seen) < evictionNoticeMaxCount {
			time.Sleep(9 * time.Millisecond)
			synctest.Wait()
			if len(sequence.route) != 0 {
				t.Fatal("eviction-only head escaped its compression deadline")
			}
			time.Sleep(time.Millisecond)
			synctest.Wait()
			if len(sequence.route) != 1 {
				t.Fatalf("compression turn repeated cumulative head: %d carriers", len(sequence.route))
			}
			wire := <-sequence.route
			var frame protocol.TransferFrame
			err := ProtoUnmarshal(wire, &frame)
			size := len(wire)
			MessagePoolReturn(wire)
			if err != nil || frame.Ack == nil || frame.Ack.Selective ||
				size > ackResponseMaxByteCount || len(frame.Ack.EvictedSequenceNumbers) > evictionNoticeAckMaxCount {
				t.Fatal("invalid or oversized eviction-only head carrier")
			}
			id, err := IdFromBytes(frame.Ack.MessageId)
			if err != nil || id != head {
				t.Fatal("eviction carrier changed cumulative head")
			}
			for _, number := range frame.Ack.EvictedSequenceNumbers {
				if seen[number] {
					t.Fatal("duplicate eviction notice")
				}
				seen[number] = true
			}
		}
	})
}

// A bounded SACK response ends with a selective ACK. Eviction metadata that
// arrives afterward must still ride on the cumulative head, rather than
// turning the metadata-only carrier into another selective response.
func TestAckCompressionEvictionAfterSackRepeatsTheHead(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		sequence := newAckGapTestSequence(t, func(settings *ReceiveBufferSettings) {
			settings.AckCompressTimeout = 10 * time.Millisecond
			settings.AckGapWakeSelectiveCount = 0
			settings.EvictionNotice = true
		})
		sequence.prime(t)
		sequence.receiveSequence.ackWindow.ackLock.Lock()
		head := sequence.receiveSequence.ackWindow.headAck.messageId
		sequence.receiveSequence.ackWindow.ackLock.Unlock()
		sequence.updateSelective(1)
		time.Sleep(10 * time.Millisecond)
		synctest.Wait()
		first, ok := sequence.readAck(t, time.Millisecond)
		if !ok {
			t.Fatal("selective ACK did not arrive at its compression deadline")
		}
		if first == (Id{}) || first == head {
			t.Fatal("compression deadline did not write a selective ACK")
		}
		for i := range evictionNoticeAckMaxCount {
			sequence.receiveSequence.noteEviction(math.MaxUint64 - uint64(i))
		}
		time.Sleep(10 * time.Millisecond)
		synctest.Wait()
		wire := <-sequence.route
		var frame protocol.TransferFrame
		if err := ProtoUnmarshal(wire, &frame); err != nil {
			t.Fatal(err)
		}
		MessagePoolReturn(wire)
		if frame.Ack == nil || frame.Ack.Selective {
			t.Fatal("eviction metadata repeated a SACK instead of the cumulative head")
		}
		got, err := IdFromBytes(frame.Ack.MessageId)
		if err != nil || got != head {
			t.Fatalf("eviction metadata head = %s, want %s (err=%v)", got, head, err)
		}
	})
}

func TestAckCompressionWireHasOneHeadAndBoundedOldestSacks(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		responses := make(chan []*protocol.Ack, 16)
		var sequence *ackGapTestSequence
		sequence = newAckGapTestSequence(t, func(settings *ReceiveBufferSettings) {
			settings.AckCompressTimeout = 10 * time.Millisecond
			settings.AckGapWakeSelectiveCount = 0
			settings.afterAckWriteForTest = func(receiveSequenceId) {
				var acks []*protocol.Ack
				size := 0
				for len(sequence.route) != 0 {
					wire := <-sequence.route
					size += len(wire)
					var frame protocol.TransferFrame
					err := ProtoUnmarshal(wire, &frame)
					MessagePoolReturn(wire)
					if err != nil || frame.Ack == nil {
						t.Error("invalid ACK on wire")
						continue
					}
					acks = append(acks, frame.Ack)
				}
				if len(acks) > ackResponseMaxCount || size > ackResponseMaxByteCount {
					t.Errorf("wire response exceeds bounds: %d ACKs, %d bytes", len(acks), size)
				}
				responses <- acks
			}
		})
		sequence.updateHead(0)
		<-responses // the idle head establishes the compression interval
		ids := map[Id]uint64{}
		for number := uint64(100); number != 0; number-- {
			ids[sequence.updateSelective(number)] = number
		}
		sequence.updateHead(3)
		headId := sequence.updateHead(10)
		time.Sleep(10 * time.Millisecond)
		synctest.Wait()
		heads, batches, next := 0, 0, uint64(11)
		for len(responses) != 0 {
			for index, ack := range <-responses {
				id, err := IdFromBytes(ack.MessageId)
				if err != nil {
					t.Fatal(err)
				}
				if !ack.Selective {
					heads++
					if heads != 1 || batches != 0 || index != 0 || id != headId {
						t.Error("wire response repeated or misplaced the cumulative head")
					}
				} else {
					if number, ok := ids[id]; !ok || number != next {
						t.Errorf("wire SACK %d, want oldest unabsorbed %d", number, next)
					}
					next++
				}
			}
			batches++
		}
		if heads != 1 || next != 101 || batches < 2 {
			t.Fatalf("incomplete compressed wire result: heads=%d next=%d responses=%d", heads, next, batches)
		}
	})
}
