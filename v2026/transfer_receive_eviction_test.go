// Exact gaps and held sets pin eviction and advertised-capacity behavior.
package connect

import (
	"bytes"
	"testing"
	"testing/synctest"
	"time"
)

// A compatibility receiver can withdraw a selective acknowledgement. Pin the
// exact overrun: hold the head and a middle Pack, fill and acknowledge the later
// tail, then admit the middle Pack. That arrival must evict the newest held Pack.
//
// A head loss alone does not establish this boundary: an ordered tail simply
// drains when the head returns. Both arms explicitly use a constant sender that
// ignores the hold; modern advertised sizing would prevent the intended overrun.
// Actual wire acknowledgements establish the lease before its withdrawal.
func TestTheEvictionNoticeStillServesAReceiverThatEvicts(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, notice := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			const hold = ByteCount(256 * 1024)
			const payloadByteCount = 4 * 1024
			fixture := newWindowRoundFixture(t, func(settings *SendBufferSettings) {
				settings.ResendQueueMaxByteCount = 768 * 1024
			}, func(settings *ReceiveBufferSettings) {
				settings.ReceiveQueueMaxByteCount = hold
				settings.ReceiveHoldPolicy = ReceiveHoldEvict
				settings.EvictionNotice = notice
			})
			fixture.forward(fixture.receive(fixture.write(payloadByteCount)), fixture.senderIn)
			head := fixture.write(payloadByteCount)
			middle := fixture.write(payloadByteCount)
			itemByteCount := MessageByteCount(middle.pack.Frames)
			if itemByteCount <= 0 {
				t.Fatal("the middle Pack has no application bytes")
			}
			heldCount := int((hold - 1) / itemByteCount)
			var newest *windowRoundFrame
			for range heldCount {
				newest = fixture.write(payloadByteCount)
				ack := fixture.receive(newest)
				if !ack.ack.Selective {
					t.Fatal("a tail Pack crossed the deliberately held head")
				}
				fixture.forward(ack, fixture.senderIn)
			}
			sequence := fixture.sequence()
			receiveSequence := fixture.receiveSequence()
			count, queued := receiveSequence.receiveQueue.QueueSize()
			if count != heldCount || queued != ByteCount(heldCount)*itemByteCount ||
				receiveSequence.receiveQueue.CanAdd(itemByteCount, hold) {
				t.Fatalf("notice=%t: hold=%d/%d bytes in %d items, want a full tail of %d", notice, queued, hold, count, heldCount)
			}
			newestNumber := newest.pack.SequenceNumber
			leased := sequence.resendQueue.GetBySequenceNumber(newestNumber)
			if leased == nil || !leased.selectiveAcked {
				t.Fatal("the newest held Pack was not selectively acknowledged at the sender")
			}
			if fixture.deliveredCount != 1 || receiveSequence.nextSequenceNumber != head.pack.SequenceNumber {
				t.Fatal("the held head moved before the middle arrival")
			}

			ack := fixture.receive(middle)
			stats := fixture.receiver.ReceiveStats()
			if stats.ReceiveQueueEvictionCount != 1 || stats.ReceiveQueueDropCount != 0 ||
				stats.ReceiveQueueEvictionByteCount != uint64(itemByteCount) {
				t.Fatalf("notice=%t: middle arrival produced evictions=%d/%d bytes, drops=%d", notice, stats.ReceiveQueueEvictionCount, stats.ReceiveQueueEvictionByteCount, stats.ReceiveQueueDropCount)
			}
			if receiveSequence.receiveQueue.GetBySequenceNumber(newestNumber) != nil ||
				receiveSequence.receiveQueue.GetBySequenceNumber(middle.pack.SequenceNumber) == nil {
				t.Fatal("middle admission did not replace the newest held Pack")
			}
			if notice {
				if numbers := ack.ack.EvictedSequenceNumbers; len(numbers) != 1 || numbers[0] != newestNumber {
					t.Fatalf("eviction notice named %v, want only %d", numbers, newestNumber)
				}
			} else if len(ack.ack.EvictedSequenceNumbers) != 0 {
				t.Fatalf("disabled notice named %v", ack.ack.EvictedSequenceNumbers)
			}
			fixture.forward(ack, fixture.senderIn)
			var resent *windowRoundFrame
			if notice {
				resent = fixture.takePack(newestNumber)
				if !bytes.Equal(resent.pack.MessageId, newest.pack.MessageId) {
					t.Fatal("the notice resend changed the evicted Pack's identity")
				}
			}
			resends := fixture.sender.SendRecoveryStats().SendEvictionResendCount
			want := uint64(0)
			if notice {
				want = 1
			}
			if resends != want {
				t.Fatalf("notice=%t: eviction resends=%d, want %d", notice, resends, want)
			}

			// The noticed resend remains on the controlled wire until the head
			// drains the hold, so recovery cannot manufacture another overrun.
			fixture.forward(fixture.receive(head), fixture.senderIn)
			if fixture.deliveredCount != heldCount+2 {
				t.Fatalf("head release delivered %d, want %d before the evicted Pack", fixture.deliveredCount, heldCount+2)
			}
			if notice {
				fixture.forward(fixture.receive(resent), fixture.senderIn)
				if fixture.deliveredCount != heldCount+3 || fixture.ackedCount != heldCount+3 {
					t.Fatalf("notice recovery delivered/acknowledged %d/%d, want %d", fixture.deliveredCount, fixture.ackedCount, heldCount+3)
				}
				if count, _ := sequence.resendQueue.QueueSize(); count != 0 {
					t.Fatalf("notice recovery retained %d sender items", count)
				}
			} else {
				item := sequence.resendQueue.GetBySequenceNumber(newestNumber)
				if item == nil || !item.selectiveAcked || fixture.ackedCount != heldCount+2 {
					t.Fatal("without a notice the evicted Pack did not remain leased")
				}
			}
			t.Logf("notice=%t: %d selectively acknowledged tail Packs, one eviction, zero refusals, %d notice resends; delivered/acknowledged %d/%d", notice, heldCount, resends, fixture.deliveredCount, fixture.ackedCount)
		})
	}
}

// THROUGHPUTFIX §37.16's correction to §37.3: the advertised figure is the
// hold's capacity from the delivered point, not its free space.
//
// Capacity less what is held double counts. A selective acknowledgement does
// not release the item at the sender, so held bytes are already inside the
// sender's outstanding count; subtracting them would shrink the window by the
// held amount for nothing and, as the hold fills after a route death, pull the
// right edge of the window inward, which a window must never do.
//
// Prediction, recorded before the run: the advertised figure does not fall as
// the hold fills.
func TestTheAdvertisedWindowIsCapacityRatherThanFreeSpace(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		const hold = ByteCount(256 * 1024)
		const ceiling = ByteCount(16 * 1024 * 1024)
		const payloadByteCount = 4 * 1024
		fixture := newWindowRoundFixture(t, func(settings *SendBufferSettings) {
			settings.DeliverySizedWindowScale = deliverySizedWindowScale
			settings.DeliverySizedWindowCeilingByteCount = ceiling
			settings.ResendQueueBudget = NewTransferMemoryBudget(ceiling)
		}, func(settings *ReceiveBufferSettings) {
			settings.ReceiveQueueMaxByteCount = hold
			settings.AdvertiseReceiveWindow = true
		})
		fixture.forward(fixture.receive(fixture.write(payloadByteCount)), fixture.senderIn)
		head := fixture.write(payloadByteCount)
		heldCount := int(hold/ByteCount(len(head.bytes))) - 2
		fixture.drop(head)
		receiveSequence := fixture.receiveSequence()
		for index := range heldCount {
			ack := fixture.receive(fixture.write(payloadByteCount))
			count, queued := receiveSequence.receiveQueue.QueueSize()
			if count != index+1 || queued <= 0 || receiveSequence.nextSequenceNumber != head.pack.SequenceNumber {
				t.Fatalf("held tail has %d Packs/%d bytes with head %d, want %d Packs beyond missing head %d", count, queued, receiveSequence.nextSequenceNumber, index+1, head.pack.SequenceNumber)
			}
			if advertised := ack.ack.ReceiveWindowByteCount; advertised == nil || *advertised != uint64(hold) {
				t.Fatalf("occupied hold %d advertised %v, want capacity %d", queued, advertised, hold)
			}
			fixture.forward(ack, fixture.senderIn)
			estimate := fixture.sequence().sendWindowEstimate(time.Now())
			if estimate.Ceiling != hold {
				t.Fatalf("occupied hold %d reduced sender ceiling to %d, want capacity %d", queued, estimate.Ceiling, hold)
			}
		}
		fixture.forward(fixture.receive(fixture.recovery(head)), fixture.senderIn)
		if fixture.deliveredCount != heldCount+2 || fixture.ackedCount != heldCount+2 {
			t.Fatalf("capacity round delivered/acknowledged %d/%d, want %d", fixture.deliveredCount, fixture.ackedCount, heldCount+2)
		}
		t.Logf("capacity stayed %d through %d distinct nonempty hold states and exact Pack %d recovery", hold, heldCount, head.pack.SequenceNumber)
	})
}

// THROUGHPUTFIX §37.16's primary field: a sender that knows the receiver's hold
// capacity never makes an eviction necessary.
//
// The rule, and why capacity is the right quantity. A selective acknowledgement
// does not release the item at the sender, so the sender's outstanding bytes
// measured from the delivered point include everything the receiver holds. Held
// bytes are at most outstanding bytes. So a sender that keeps
// outstanding-from-delivered at or below the advertised capacity can never
// force an eviction, whatever the gap structure — one gap or a thousand —
// because the hold would have to contain more than the sender has outstanding.
// That is why gap structure does not need advertising, and it is TCP's rule.
//
// This is the row that makes the notice safe rather than a rotation: the cell
// above shows a notice under an overrun evicting a second item to readmit the
// first. With the advertisement there is no overrun to recover from.
//
// Prediction, recorded before the run: with the sender clamped to the
// receiver's advertised capacity, an induced loss and the out-of-order hold it
// opens produce zero evictions and zero refused arrivals, and everything
// arrives.
func TestAnAdvertisedCapacityRemovesTheEvictionEntirely(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		const messageCount = 2000
		const payloadByteCount = 4 * 1024
		const hold = ByteCount(2 * 1024 * 1024)
		const ceiling = ByteCount(16 * 1024 * 1024)
		const propagation = 25 * time.Millisecond
		const dropAt = 100
		fixture := newWindowRoundFixture(t, func(settings *SendBufferSettings) {
			settings.ResendQueueMaxByteCount = hold
			settings.ResendQueueMinByteCount = 64 * 1024
			settings.DeliverySizedWindowScale = deliverySizedWindowScale
			settings.DeliverySizedWindowCeilingByteCount = ceiling
			settings.ResendQueueBudget = NewTransferMemoryBudget(ceiling)
		}, func(settings *ReceiveBufferSettings) {
			settings.ReceiveQueueMaxByteCount = hold
			settings.AdvertiseReceiveWindow = true
		})
		for range dropAt {
			fixture.forward(fixture.receive(fixture.write(payloadByteCount)), fixture.senderIn)
		}
		head := fixture.write(payloadByteCount)
		heldCount := int(hold/ByteCount(len(head.bytes))) - 2
		fixture.drop(head)
		for range heldCount {
			fixture.forward(fixture.receive(fixture.write(payloadByteCount)), fixture.senderIn)
		}
		receiveSequence := fixture.receiveSequence()
		if count, queued := receiveSequence.receiveQueue.QueueSize(); count != heldCount || count <= 0 ||
			queued != ByteCount(heldCount)*MessageByteCount(head.pack.Frames) || receiveSequence.nextSequenceNumber != dropAt ||
			fixture.deliveredCount != dropAt {
			t.Fatalf("advertised gap: held=%d/%d bytes in %d Packs, head=%d delivered=%d; wanted an occupied hold behind Pack %d", queued, hold, count, receiveSequence.nextSequenceNumber, fixture.deliveredCount, dropAt)
		}
		fixture.forward(fixture.receive(fixture.recovery(head)), fixture.senderIn)
		if fixture.sender.DestinationSendStats(fixture.receiver.ClientId()).ResendWriteByteCount == 0 {
			t.Fatal("the induced application loss caused no actual resend")
		}
		fixture.startWire(propagation, DefaultReceiveBufferSettings().AckCompressTimeout)
		fixture.offer(messageCount-int(fixture.nextNumber), payloadByteCount)
		time.Sleep(30 * time.Second)
		synctest.Wait()
		stats := fixture.receiver.ReceiveStats()
		estimate := fixture.sender.DestinationSendStats(fixture.receiver.ClientId()).SendWindow
		if estimate.Ceiling != hold {
			t.Fatalf("sender ceiling=%d, want advertised hold %d", estimate.Ceiling, hold)
		}
		if stats.ReceiveQueueEvictionCount != 0 || stats.ReceiveQueueDropCount != 0 {
			t.Fatalf("advertised hold lost work: evictions=%d drops=%d", stats.ReceiveQueueEvictionCount, stats.ReceiveQueueDropCount)
		}
		if fixture.deliveredCount != messageCount || fixture.ackedCount != messageCount {
			t.Fatalf("advertised loss recovery delivered/acknowledged %d/%d, want %d within 30 seconds", fixture.deliveredCount, fixture.ackedCount, messageCount)
		}
		t.Logf("Pack %d loss held %d later Packs under capacity %d; %d/%d delivered and acknowledged with zero evictions/refusals", head.pack.SequenceNumber, heldCount, hold, fixture.deliveredCount, messageCount)
	})
}
