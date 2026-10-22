// Exact gaps and held sets pin eviction and advertised-capacity behavior.
package connect

import (
	"bytes"
	"sync/atomic"
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

			fixture.forward(middle, fixture.receiverIn)
			acks := fixture.takeAcknowledgements()
			stats := fixture.receiver.ReceiveStats()
			if stats.ReceiveQueueEvictionCount != 1 || stats.ReceiveQueueDropCount != 0 ||
				stats.ReceiveQueueEvictionByteCount != uint64(itemByteCount) {
				t.Fatalf("notice=%t: middle arrival produced evictions=%d/%d bytes, drops=%d", notice, stats.ReceiveQueueEvictionCount, stats.ReceiveQueueEvictionByteCount, stats.ReceiveQueueDropCount)
			}
			if receiveSequence.receiveQueue.GetBySequenceNumber(newestNumber) != nil ||
				receiveSequence.receiveQueue.GetBySequenceNumber(middle.pack.SequenceNumber) == nil {
				t.Fatal("middle admission did not replace the newest held Pack")
			}
			var numbers []uint64
			for _, ack := range acks {
				numbers = append(numbers, ack.ack.EvictedSequenceNumbers...)
			}
			if notice {
				if len(numbers) != 1 || numbers[0] != newestNumber {
					t.Fatalf("eviction notice named %v, want only %d", numbers, newestNumber)
				}
			} else if len(numbers) != 0 {
				t.Fatalf("disabled notice named %v", numbers)
			}
			for _, ack := range acks {
				fixture.forward(ack, fixture.senderIn)
			}
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

// Earlier metadata is not subsumed by a later SACK. Both carrier orders use
// actual ACK wire shares emitted by the receiver, never synthesized feedback.
func TestWindowRoundAcknowledgementsKeepEveryEvictionCarrier(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, noticeFirst := range []bool{true, false} {
		synctest.Test(t, func(t *testing.T) {
			const hold = ByteCount(4 * 1024)
			const payloadByteCount = 1024
			fixture := newWindowRoundFixture(t, nil, func(settings *ReceiveBufferSettings) {
				settings.ReceiveQueueMaxByteCount = hold
				settings.ReceiveHoldPolicy = ReceiveHoldEvict
				settings.EvictionNotice = true
			})
			fixture.forward(fixture.receive(fixture.write(payloadByteCount)), fixture.senderIn)
			head, middle := fixture.write(payloadByteCount), fixture.write(payloadByteCount)
			duplicate := &windowRoundFrame{bytes: MessagePoolShareReadOnly(middle.bytes), pack: middle.pack}
			fixture.heldFrames = append(fixture.heldFrames, duplicate)
			itemByteCount := MessageByteCount(middle.pack.Frames)
			heldCount := int((hold - 1) / itemByteCount)
			var newest *windowRoundFrame
			for range heldCount {
				newest = fixture.write(payloadByteCount)
				fixture.forward(fixture.receive(newest), fixture.senderIn)
			}
			newestNumber := newest.pack.SequenceNumber
			sequence, receiveSequence := fixture.sequence(), fixture.receiveSequence()
			if item := sequence.resendQueue.GetBySequenceNumber(newestNumber); item == nil || !item.selectiveAcked {
				t.Fatal("actual newest source Pack lacks its selective-ACK lease")
			}
			// Setup bypasses the capture helper under test so even its old
			// drain-last implementation cannot erase the source evidence.
			takeActual := func() []*windowRoundFrame {
				var acks []*windowRoundFrame
				for len(fixture.receiverOut) != 0 {
					ack := fixture.take(fixture.receiverOut)
					if ack.ack == nil {
						t.Fatal("receiver emitted non-ACK setup feedback")
					}
					acks = append(acks, ack)
				}
				if len(acks) == 0 {
					t.Fatal("actual receive transition emitted no acknowledgement")
				}
				return acks
			}
			fixture.forward(middle, fixture.receiverIn)
			generated := takeActual()
			stats := fixture.receiver.ReceiveStats()
			if stats.ReceiveQueueEvictionCount != 1 || stats.ReceiveQueueDropCount != 0 ||
				receiveSequence.receiveQueue.GetBySequenceNumber(newestNumber) != nil ||
				receiveSequence.receiveQueue.GetBySequenceNumber(middle.pack.SequenceNumber) == nil {
				t.Fatal("public middle arrival did not produce the exact real eviction")
			}
			// A physical duplicate produces an ordinary selective ACK after
			// that real withdrawal, without another admission or eviction.
			fixture.forward(duplicate, fixture.receiverIn)
			generated = append(generated, takeActual()...)
			var notices, ordinary []*windowRoundFrame
			var numbers []uint64
			for _, ack := range generated {
				if len(ack.ack.EvictedSequenceNumbers) != 0 {
					notices = append(notices, ack)
					numbers = append(numbers, ack.ack.EvictedSequenceNumbers...)
				} else {
					ordinary = append(ordinary, ack)
				}
			}
			if len(numbers) != 1 || numbers[0] != newestNumber || len(ordinary) == 0 {
				t.Fatalf("actual carrier evidence lost: notices=%v ordinary=%d", numbers, len(ordinary))
			}
			ordered := append(append([]*windowRoundFrame{}, ordinary...), notices...)
			if noticeFirst {
				ordered = append(append([]*windowRoundFrame{}, notices...), ordinary...)
			}
			expectedBytes := make([][]byte, len(ordered))
			for index, ack := range ordered {
				expectedBytes[index] = bytes.Clone(ack.bytes)
				fixture.receiverOut <- ack.bytes
				ack.bytes = nil
			}
			acks := fixture.takeAcknowledgements()
			if len(acks) != len(ordered) {
				t.Fatalf("noticeFirst=%t: captured %d/%d actual wire ACKs", noticeFirst, len(acks), len(ordered))
			}
			numbers = numbers[:0]
			for index, ack := range acks {
				if !bytes.Equal(ack.bytes, expectedBytes[index]) {
					t.Fatalf("noticeFirst=%t: capture rewrote or reordered ACK %d", noticeFirst, index)
				}
				numbers = append(numbers, ack.ack.EvictedSequenceNumbers...)
				fixture.forward(ack, fixture.senderIn)
			}
			if len(numbers) != 1 || numbers[0] != newestNumber {
				t.Fatalf("noticeFirst=%t: captured withdrawal named %v, want only %d", noticeFirst, numbers, newestNumber)
			}
			resent := fixture.takePack(newestNumber)
			if !bytes.Equal(resent.pack.MessageId, newest.pack.MessageId) ||
				fixture.sender.SendRecoveryStats().SendEvictionResendCount != 1 {
				t.Fatal("forwarded real notice lost exact one-resend source custody")
			}
			fixture.forward(fixture.receive(head), fixture.senderIn)
			fixture.forward(fixture.receive(resent), fixture.senderIn)
			if fixture.deliveredCount != heldCount+3 || fixture.ackedCount != heldCount+3 {
				t.Fatalf("noticeFirst=%t: delivered/acknowledged %d/%d, want %d", noticeFirst,
					fixture.deliveredCount, fixture.ackedCount, heldCount+3)
			}
			if count, _ := sequence.resendQueue.QueueSize(); count != 0 {
				t.Fatal("completed wire-feedback recovery retained sender owners")
			}
		})
	}
}

// A later ACK does not encode the earlier carrier's advertisement or timing.
// Capture preserves both physical records even when their wire order reverses.
func TestWindowRoundAcknowledgementsKeepEachAdvertisementCarrier(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, reverse := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			fixture := newWindowRoundFixture(t, nil, func(settings *ReceiveBufferSettings) {
				settings.AdvertiseReceiveWindow = true
			})
			var generated []*windowRoundFrame
			for _, capacity := range []ByteCount{64 * 1024, 128 * 1024} {
				synctest.Wait()
				fixture.receiver.settings.ReceiveBufferSettings.ReceiveQueueMaxByteCount = capacity
				fixture.forward(fixture.write(32), fixture.receiverIn)
				ack := fixture.take(fixture.receiverOut)
				if ack.ack == nil || ack.ack.ReceiveWindowByteCount == nil ||
					*ack.ack.ReceiveWindowByteCount != uint64(capacity) || len(fixture.receiverOut) != 0 {
					t.Fatal("actual advertisement setup did not emit one exact carrier")
				}
				generated = append(generated, ack)
			}
			if reverse {
				generated[0], generated[1] = generated[1], generated[0]
			}
			expectedBytes := make([][]byte, len(generated))
			for index, ack := range generated {
				expectedBytes[index] = bytes.Clone(ack.bytes)
				fixture.receiverOut <- ack.bytes
				ack.bytes = nil
			}
			acks := fixture.takeAcknowledgements()
			if len(acks) != len(generated) {
				t.Fatalf("reverse=%t: captured %d/%d actual advertisements", reverse, len(acks), len(generated))
			}
			for index, ack := range acks {
				if !bytes.Equal(ack.bytes, expectedBytes[index]) {
					t.Fatal("advertisement capture merged, rewrote or reordered a carrier")
				}
				fixture.forward(ack, fixture.senderIn)
			}
			if fixture.deliveredCount != 2 || fixture.ackedCount != 2 {
				t.Fatalf("reverse=%t: exact physical ACKs delivered/acknowledged %d/%d", reverse,
					fixture.deliveredCount, fixture.ackedCount)
			}
			if count, _ := fixture.sequence().resendQueue.QueueSize(); count != 0 {
				t.Fatal("advertisement-only exchange retained sender owners")
			}
		})
	}
}

// A valid withdrawal can name a released head as its ACK carrier. The exact
// source owner must resume without another ACK, admission event or timer.
func TestEvictionNoticeOnReleasedCarrierWakesParkedSource(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, noticeFirst := range []bool{true, false} {
		synctest.Test(t, func(t *testing.T) {
			const hold = ByteCount(2 * 1024)
			const payloadByteCount = 1024
			var ownerPasses atomic.Uint64
			fixture := newWindowRoundFixture(t, func(settings *SendBufferSettings) {
				settings.afterApplyAckSnapshotForTest = func(sendSequenceId) { ownerPasses.Add(1) }
			}, func(settings *ReceiveBufferSettings) {
				settings.ReceiveQueueMaxByteCount = hold
				settings.ReceiveHoldPolicy = ReceiveHoldEvict
				settings.EvictionNotice = true
			})
			delivered := fixture.write(payloadByteCount)
			fixture.forward(fixture.receive(delivered), fixture.senderIn)
			head, middle := fixture.write(payloadByteCount), fixture.write(payloadByteCount)
			duplicate := &windowRoundFrame{bytes: MessagePoolShareReadOnly(middle.bytes), pack: middle.pack}
			fixture.heldFrames = append(fixture.heldFrames, duplicate)
			itemByteCount := MessageByteCount(middle.pack.Frames)
			heldCount := int((hold - 1) / itemByteCount)
			if heldCount != 1 {
				t.Fatal("released-carrier proof requires exactly one leased tail below the default three-SACK gap threshold")
			}
			var newest *windowRoundFrame
			for range heldCount {
				newest = fixture.write(payloadByteCount)
				fixture.forward(fixture.receive(newest), fixture.senderIn)
			}
			sequence, receiver := fixture.sequence(), fixture.receiveSequence()
			newestNumber := newest.pack.SequenceNumber
			if _, retained := sequence.retainedAckSequenceNumber(RequireIdFromBytes(delivered.pack.MessageId)); retained {
				t.Fatal("the already-delivered metadata carrier still has a source owner")
			}
			fixture.forward(middle, fixture.receiverIn)
			withheld := fixture.takeAcknowledgements()
			var originalNotices []uint64
			for _, ack := range withheld {
				originalNotices = append(originalNotices, ack.ack.EvictedSequenceNumbers...)
			}
			if len(originalNotices) != 1 || originalNotices[0] != newestNumber ||
				fixture.receiver.ReceiveStats().ReceiveQueueEvictionCount != 1 ||
				receiver.receiveQueue.GetBySequenceNumber(newestNumber) != nil {
				t.Fatal("actual public middle admission did not withdraw the exact leased tail")
			}
			// Withhold the first physical response. Its duplicate is an
			// ordinary SACK; neither notification loss nor reordering returns
			// the evicted payload to the receiver.
			fixture.forward(duplicate, fixture.receiverIn)
			ordinary := fixture.takeAcknowledgements()
			for _, ack := range ordinary {
				if !ack.ack.Selective || len(ack.ack.EvictedSequenceNumbers) != 0 ||
					!bytes.Equal(ack.ack.MessageId, middle.pack.MessageId) {
					t.Fatal("physical duplicate did not produce only its ordinary middle SACK")
				}
			}
			// Repeat the actual state withdrawal using the receiver's normal
			// metadata producer. With no new delivery it must ride old head0.
			receiver.noteEviction(newestNumber)
			notices := fixture.takeAcknowledgements()
			if len(notices) != 1 || notices[0].ack.Selective ||
				!bytes.Equal(notices[0].ack.MessageId, delivered.pack.MessageId) ||
				len(notices[0].ack.EvictedSequenceNumbers) != 1 ||
				notices[0].ack.EvictedSequenceNumbers[0] != newestNumber {
				t.Fatal("actual ACK worker did not encode the exact released-head withdrawal")
			}
			if !noticeFirst {
				for _, ack := range ordinary {
					fixture.forward(ack, fixture.senderIn)
				}
			}
			now, passes := time.Now(), ownerPasses.Load()
			pending := func() []uint64 {
				return pendingEvictionStateForTest(sequence).numbers
			}
			item, first := sequence.resendQueue.GetBySequenceNumber(newestNumber), sequence.resendQueue.PeekFirst()
			if item == nil || !item.selectiveAcked || first == nil || !first.resendTime.After(now) ||
				len(pending()) != 0 || sequence.ackWindow.Pending() || len(sequence.ackWindow.ackNotify) != 0 ||
				sequence.resendCapacityUnavailable.Load() || len(fixture.senderOut) != 0 {
				targetPresent, targetLeased, targetSends, targetDue := item != nil, false, 0, time.Time{}
				if item != nil {
					targetLeased, targetSends, targetDue = item.selectiveAcked, item.sendCount, item.resendTime
				}
				firstPresent, firstNumber, firstDue := first != nil, uint64(0), time.Time{}
				if first != nil {
					firstNumber, firstDue = first.sequenceNumber, first.resendTime
				}
				for _, ack := range withheld {
					t.Logf("withheld actual ACK: message=%x selective=%t evicted=%v", ack.ack.MessageId, ack.ack.Selective, ack.ack.EvictedSequenceNumbers)
				}
				for _, ack := range ordinary {
					t.Logf("ordinary actual ACK: message=%x selective=%t evicted=%v", ack.ack.MessageId, ack.ack.Selective, ack.ack.EvictedSequenceNumbers)
				}
				t.Logf("released-head actual ACK: message=%x selective=%t evicted=%v",
					notices[0].ack.MessageId, notices[0].ack.Selective, notices[0].ack.EvictedSequenceNumbers)
				for _, retained := range sequence.sendItems {
					if retained != nil {
						t.Logf("source retained: number=%d sends=%d selective=%t due=%s recovery=%d head=%t writeObserved=%t",
							retained.sequenceNumber, retained.sendCount, retained.selectiveAcked, retained.resendTime.Sub(now),
							retained.recoveryKind, retained.head, retained.transportWriteObserved)
					}
				}
				t.Fatalf("source not parked: noticeFirst=%t targetExists=%t targetLeased=%t targetSends=%d targetDue=%s firstExists=%t firstNumber=%d firstDue=%s pending=%v ackPending=%t ackEdge=%d capacityUnavailable=%t senderOut=%d ownerPasses=%d ctx=%v rtt=%+v",
					noticeFirst, targetPresent, targetLeased, targetSends, targetDue.Sub(now),
					firstPresent, firstNumber, firstDue.Sub(now), pending(), sequence.ackWindow.Pending(),
					len(sequence.ackWindow.ackNotify), sequence.resendCapacityUnavailable.Load(),
					len(fixture.senderOut), ownerPasses.Load(), sequence.ctx.Err(), sequence.rttWindow.estimate(now))
			}
			fixture.forward(notices[0], fixture.senderIn)
			if time.Now() != now {
				t.Fatal("metadata rescue advanced virtual time instead of dispatching the parked owner")
			}
			if len(fixture.senderOut) == 0 {
				leased, sendCount, due := false, 0, time.Time{}
				if retained := sequence.resendQueue.GetBySequenceNumber(newestNumber); retained != nil {
					leased, sendCount, due = retained.selectiveAcked, retained.sendCount, retained.resendTime
				}
				policy := sequence.transferFlightPolicy()
				t.Fatalf("noticeFirst=%t: released-head withdrawal stranded: pending=%v leased=%t sends=%d due=%s ownerPasses=%d/%d ackPending=%t admissionBlocked=%t reliable=%t limited=%t rtt=%+v ctx=%v",
					noticeFirst, pending(), leased, sendCount, due.Sub(now), passes, ownerPasses.Load(),
					sequence.ackWindow.Pending(), sequence.resendCapacityUnavailable.Load(),
					policy.reliableRouteAvailable, policy.limited, sequence.rttWindow.estimate(now), sequence.ctx.Err())
			}
			resent := fixture.takePack(newestNumber)
			if !bytes.Equal(resent.pack.MessageId, newest.pack.MessageId) ||
				fixture.sender.SendRecoveryStats().SendEvictionResendCount != 1 ||
				len(pending()) != 0 || ownerPasses.Load() <= passes || fixture.ackedCount != 1 {
				t.Fatal("metadata-only wake lost exact one-resend custody or invented delivery credit")
			}
			if noticeFirst {
				for _, ack := range ordinary {
					fixture.forward(ack, fixture.senderIn)
				}
			}
			fixture.forward(fixture.receive(head), fixture.senderIn)
			fixture.forward(fixture.receive(resent), fixture.senderIn)
			if fixture.deliveredCount != heldCount+3 || fixture.ackedCount != heldCount+3 {
				t.Fatal("released-carrier recovery did not complete every original source owner")
			}
			if count, _ := sequence.resendQueue.QueueSize(); count != 0 {
				t.Fatal("released-carrier recovery retained completed source ownership")
			}
		})
	}
}

// Legacy ACK workers pass a local window rather than publishing self.ackWindow.
// Empty metadata must not wake; valid state feedback wakes only that passed owner.
func TestEvictionMetadataWakeUsesThePassedOwnerWindow(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		window, other := newSequenceAckWindow(), newSequenceAckWindow()
		sequence := &SendSequence{resendQueue: newResendQueue(nil, 0)}
		// The ACK carrier is unknown, but its withdrawal names a real owner.
		sequence.resendQueue.Add(&sendItem{transferItem: transferItem{messageId: NewId(), sequenceNumber: 7}, expectsAck: true})
		if !legacy {
			sequence.ackWindow = other
		}
		for _, evictions := range []*ackEvictionNotice{nil, {}} {
			sequence.coalesceReceivedAck(window, receiveAckMessage{messageId: NewId(), evictions: evictions})
			if window.Pending() || len(window.ackNotify) != 0 || len(pendingEvictionStateForTest(sequence).numbers) != 0 {
				t.Fatal("empty metadata fabricated acknowledgement progress or a wake")
			}
		}
		sequence.coalesceReceivedAck(window, receiveAckMessage{
			messageId: NewId(), evictions: &ackEvictionNotice{sequenceNumbers: []uint64{7}},
		})
		if len(window.ackNotify) != 1 || len(other.ackNotify) != 0 || window.Pending() ||
			len(pendingEvictionStateForTest(sequence).numbers) != 1 || pendingEvictionStateForTest(sequence).numbers[0] != 7 {
			t.Fatalf("legacy=%t: unretained-carrier metadata did not wake only the actual ACK owner", legacy)
		}
	}
}

// The stable capacity-one edge coalesces a burst without creating cumulative
// or selective acceptance credit. Every pending number remains with its owner.
func TestEvictionMetadataWakeCoalescesWithoutDeliveryCredit(t *testing.T) {
	window := newSequenceAckWindow()
	sequence := &SendSequence{resendQueue: newResendQueue(nil, 0)}
	for _, number := range []uint64{3, 5} {
		sequence.resendQueue.Add(&sendItem{transferItem: transferItem{messageId: NewId(), sequenceNumber: number}, expectsAck: true})
	}
	for _, number := range []uint64{3, 5} {
		sequence.coalesceReceivedAck(window, receiveAckMessage{
			messageId: NewId(), evictions: &ackEvictionNotice{sequenceNumbers: []uint64{number}},
		})
	}
	if len(window.ackNotify) != 1 || window.Pending() {
		t.Fatal("metadata burst did not coalesce its wake independently of delivery credit")
	}
	pending := pendingEvictionStateForTest(sequence).numbers
	sequence.applyPendingEvictions(sequence.takePendingEvictions())
	if len(pending) != 2 || pending[0] != 3 || pending[1] != 5 ||
		sequence.takePendingEvictions() != (pendingEvictionBatch{}) {
		t.Fatal("metadata wake lost state feedback or retained it after owner extraction")
	}
}
