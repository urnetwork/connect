// One window pacing service paces every logical lane to a destination over
// their one carrier. A receiver that holds one lane's delivered bytes is not
// a queue on that carrier; the receiver wait reported on each cumulative head
// decides which it is, through the real ACK coalescer.
package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

const (
	// 10ms of path, half each way, beside the receiver's 10ms ACK compression
	receiverHoldTestPath         = 10 * time.Millisecond
	receiverHoldTestCompression  = 10 * time.Millisecond
	receiverHoldTestMessageBytes = 2000
	receiverHoldTestRate         = ByteCount(1000000)
)

// One lane of a shared service, driven through its real pacing reservation,
// physical write confirmation and ACK coalescing entry points.
type receiverHoldTestLane struct {
	sequence *SendSequence
	items    []*sendItem
}

func newReceiverHoldTestLane(t *testing.T, service *windowPacingService) *receiverHoldTestLane {
	sequence := newEstimatorFixture(t, nil)
	sequence.ctx, sequence.log = context.Background(), NewNoopLogger()
	sequence.client = &Client{feedbackTimeBase: time.Now()}
	sequence.sequenceId = NewId()
	sequence.ackWindow = newSequenceAckWindow()
	sequence.windowPacer = windowBurstPacer{service: service, serviceSequenceId: sequence.sequenceId, rate: receiverHoldTestRate}
	return &receiverHoldTestLane{sequence: sequence}
}

// Paces and confirms count H1 messages, then leaves them unacknowledged.
func (self *receiverHoldTestLane) write(t *testing.T, count int) {
	t.Helper()
	for range count {
		item := &sendItem{
			transferItem: transferItem{messageId: NewId(), sequenceNumber: uint64(len(self.items) + 1)},
			sendTime:     time.Now(), sendCount: 1, expectsAck: true,
		}
		if err := self.sequence.windowPacer.waitForServiceMessage(context.Background(), receiverHoldTestMessageBytes, false,
			self.sequence.sequenceId, item.messageId, item.sequenceNumber); err != nil {
			t.Fatal(err)
		}
		item.pacingByteCount = receiverHoldTestMessageBytes
		self.sequence.resendQueue.Add(item)
		self.sequence.beginReceiverRttWrite(item, false)
		self.sequence.finishReceiverRttWrite(item, transferWriteDisposition{transportType: TransportTypeH1, reliable: true}, nil)
		self.sequence.windowPacer.service.finishWrite(self.sequence.sequenceId, item.messageId, true)
		self.items = append(self.items, item)
	}
}

// The receiver's reply for the item with this sequence number. The item
// arrives half a path after its write and the receiver holds it at least
// minimumHold; the reply reports the receiver's actual wait and arrives half
// a path after it is written, so the adjusted round trip is always the path.
func (self *receiverHoldTestLane) ack(number int, selective bool, minimumHold time.Duration) {
	self.ackAfterQueue(number, selective, minimumHold, 0)
}

// The same reply for an item that also waited queue in a carrier queue on its
// way to the receiver, so the adjusted round trip is the path plus that queue.
func (self *receiverHoldTestLane) ackAfterQueue(number int, selective bool, minimumHold, queue time.Duration) {
	head := self.items[number-1]
	arrived := time.Unix(0, head.pacingSentAtNanos).Add(receiverHoldTestPath/2 + queue)
	if wait := time.Until(arrived.Add(minimumHold + receiverHoldTestPath/2)); 0 < wait {
		time.Sleep(wait)
	}
	receiverWait := time.Now().Add(-receiverHoldTestPath / 2).Sub(arrived)
	self.sequence.coalesceReceivedAck(self.sequence.ackWindow, receiveAckMessage{
		messageId: head.messageId, sequenceId: self.sequence.sequenceId, selective: selective,
		tag:                 sequenceTag{sendTime: uint64(head.sendTime.UnixMilli()), set: true},
		receivedAtNanos:     self.sequence.client.feedbackArrivalNanos(time.Now()),
		receiverAckDelaySet: true, receiverAckDelayMicros: uint32(receiverWait / time.Microsecond),
		ackCompressTimeoutSet: true, ackCompressTimeoutMicros: uint32(receiverHoldTestCompression / time.Microsecond),
	})
}

// Lanes share a service whose earlier delivery already spans a residence, so
// the flight test applies, and whose path is measured by prompt replies. A
// third lane keeps one write in flight, as busy siblings do, so no later write
// is the empty-flight probe that may confirm a new receiver baseline.
func newReceiverHoldTestLanes(t *testing.T) (*windowPacingService, *receiverHoldTestLane, *receiverHoldTestLane) {
	t.Helper()
	service := newWindowPacingService(DefaultSendBufferSettings())
	service.sent, service.total = mib(10), mib(10)
	stalled, sibling, busy := newReceiverHoldTestLane(t, service), newReceiverHoldTestLane(t, service), newReceiverHoldTestLane(t, service)
	for _, lane := range []*receiverHoldTestLane{stalled, sibling, busy} {
		t.Cleanup(lane.sequence.windowPacer.close)
		lane.write(t, 1)
		lane.ack(1, false, time.Millisecond)
	}
	busy.write(t, 1)
	if timing := service.roundTripEvidence(time.Now()); timing.count == 0 || timing.minimum != receiverHoldTestPath ||
		timing.residence != receiverHoldTestPath+receiverHoldTestCompression {
		t.Fatalf("prompt replies did not measure the path: %+v", timing)
	}
	if service.backlogged(receiverHoldTestRate) {
		t.Fatal("one message in flight started backlogged")
	}
	return service, stalled, sibling
}

// The production collapse: a receiver blocks one lane's delivery for a second
// while its other lanes drain promptly. The stalled lane's unacknowledged
// window waits at its receiver, so it must not read as a queue on the carrier
// every sibling's pace is priced from.
func TestWindowPacingReceiverHeldLaneIsNotCarrierFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service, stalled, sibling := newReceiverHoldTestLanes(t)
		stalled.write(t, 32)
		stalled.ack(2, false, time.Second)
		sibling.write(t, 1)
		sibling.ack(2, false, time.Millisecond)
		if service.backlogged(receiverHoldTestRate) {
			service.stateLock.Lock()
			outstanding := service.outstandingWithLock()
			service.stateLock.Unlock()
			t.Fatalf("a held lane's window at its receiver priced its sibling as a backlogged carrier: outstanding=%.0f", outstanding)
		}
	})
}

// The stall's own resend timeouts retry the held lane's heads, and a retried
// head cannot pair with an exact first write. Its receiver still reports how
// long it held the head, and that wait alone decides the hold.
func TestWindowPacingRetriedHeadStillShowsReceiverHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service, stalled, sibling := newReceiverHoldTestLanes(t)
		stalled.write(t, 32)
		retried := stalled.items[1]
		stalled.sequence.invalidateReceiverRttWrite(retried)
		retried.sendCount = 2
		stalled.ack(2, false, time.Second)
		sibling.write(t, 1)
		sibling.ack(2, false, time.Millisecond)
		if service.backlogged(receiverHoldTestRate) {
			t.Fatal("a retried head's receiver wait did not hold its lane")
		}
	})
}

// A receiver-held lane's retry is explained by its receiver's wait, so it
// neither ends discovery nor releases the pace the service holds for every
// lane. Another lane's retry remains the congestion evidence it was.
func TestWindowPacingReceiverHeldRetryIsNotCongestion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service, stalled, sibling := newReceiverHoldTestLanes(t)
		stalled.write(t, 32)
		stalled.ack(2, false, time.Second)
		service.holdPacing(2 * receiverHoldTestRate)
		if discovery, held := service.pacingHold(); !discovery || held != 2*receiverHoldTestRate {
			t.Fatalf("fixture lost discovery or its held pace: discovery=%t held=%d", discovery, held)
		}
		if err := stalled.sequence.windowPacer.waitForServiceWrite(context.Background(), receiverHoldTestMessageBytes, true); err != nil {
			t.Fatal(err)
		}
		if discovery, held := service.pacingHold(); !discovery || held != 2*receiverHoldTestRate {
			t.Fatalf("a held lane's retry was read as carrier congestion: discovery=%t held=%d", discovery, held)
		}
		if err := sibling.sequence.windowPacer.waitForServiceWrite(context.Background(), receiverHoldTestMessageBytes, true); err != nil {
			t.Fatal(err)
		}
		if discovery, held := service.pacingHold(); discovery || held != 0 {
			t.Fatalf("an unheld lane's retry lost its congestion evidence: discovery=%t held=%d", discovery, held)
		}
	})
}

// A window that no receiver wait explains is still the carrier flight, and
// the queue evidence, that it was before receiver holds were separated.
func TestWindowPacingUnheldLaneFlightRemainsCarrierFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service, stalled, _ := newReceiverHoldTestLanes(t)
		stalled.write(t, 32)
		if !service.backlogged(receiverHoldTestRate) {
			t.Fatal("64 kB of unexplained flight beyond the path's bound lost its queue evidence")
		}
	})
}

// A receiver that resumes draining returns the lane to carrier flight on its
// first prompt cumulative head, so the lane's next window counts again.
func TestWindowPacingReceiverHoldEndsWithAPromptHead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service, stalled, _ := newReceiverHoldTestLanes(t)
		stalled.write(t, 32)
		stalled.ack(2, false, time.Second)
		if service.backlogged(receiverHoldTestRate) {
			t.Fatal("the held window was read as carrier flight")
		}
		// the receiver releases the rest of the held window, then drains promptly
		stalled.ack(33, false, 0)
		stalled.write(t, 1)
		stalled.ack(34, false, time.Millisecond)
		stalled.write(t, 32)
		if !service.backlogged(receiverHoldTestRate) {
			t.Fatal("a prompt head did not return the lane's next window to carrier flight")
		}
	})
}

// A selective reply is prompt even while in-order delivery waits behind a
// hole. It can neither start nor end the receiver hold of its lane.
func TestWindowPacingSelectiveReplyLeavesReceiverHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service, stalled, _ := newReceiverHoldTestLanes(t)
		stalled.write(t, 32)
		stalled.ack(3, true, time.Second)
		if !service.backlogged(receiverHoldTestRate) {
			t.Fatal("a selective reply's wait started a receiver hold")
		}
		stalled.ack(2, false, 0)
		if service.backlogged(receiverHoldTestRate) {
			t.Fatal("the held window was read as carrier flight")
		}
		stalled.write(t, 1)
		stalled.ack(34, true, time.Millisecond)
		if service.backlogged(receiverHoldTestRate) {
			t.Fatal("a prompt selective reply ended an in-order receiver hold")
		}
	})
}

// A queued path keeps a receiver-held lane's window and retries as carrier
// evidence. A hole repaired after loss at a full relay also waits at its
// receiver, and the queue that came before that loss must still slow every
// lane.
func TestWindowPacingQueuedPathKeepsReceiverHeldEvidence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service, held, sibling := newReceiverHoldTestLanes(t)
		held.write(t, 32)
		held.ack(2, false, time.Second)
		if service.backlogged(receiverHoldTestRate) {
			t.Fatal("the held window was read as carrier flight on an unqueued path")
		}
		sibling.write(t, 1)
		sibling.ackAfterQueue(2, false, time.Millisecond, 20*time.Millisecond)
		if !service.backlogged(receiverHoldTestRate) {
			t.Fatal("a queued path dropped a held lane's window from its carrier flight")
		}
		service.holdPacing(2 * receiverHoldTestRate)
		if err := held.sequence.windowPacer.waitForServiceWrite(context.Background(), receiverHoldTestMessageBytes, true); err != nil {
			t.Fatal(err)
		}
		if _, pace := service.pacingHold(); pace != 0 {
			t.Fatalf("a held lane's retry on a queued path lost its congestion evidence: held=%d", pace)
		}
	})
}

// Without a path measurement there is no residence to compare a receiver's
// wait against, so no reply can yet declare its lane receiver-held.
func TestWindowPacingReceiverHoldWaitsForAPathMeasurement(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service := newWindowPacingService(DefaultSendBufferSettings())
		lane := newReceiverHoldTestLane(t, service)
		t.Cleanup(lane.sequence.windowPacer.close)
		lane.write(t, 2)
		// a retried head carries no paired round trip
		lane.sequence.invalidateReceiverRttWrite(lane.items[0])
		lane.ack(1, false, time.Second)
		if residence := service.roundTripEvidence(time.Now()).residence; residence != 0 {
			t.Fatalf("fixture measured a path: residence=%s", residence)
		}
		service.stateLock.Lock()
		held := lane.sequence.windowPacer.receiverHeld
		service.stateLock.Unlock()
		if held {
			t.Fatal("a reply without a path measurement declared its lane receiver-held")
		}
	})
}

// Closing a held lane releases it from the service, so neither its counters
// nor its sequence outlive it in a sibling's flight read.
func TestWindowPacingClosedLaneLeavesReceiverHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service, stalled, sibling := newReceiverHoldTestLanes(t)
		stalled.write(t, 32)
		stalled.ack(2, false, time.Second)
		stalled.sequence.windowPacer.close()
		service.stateLock.Lock()
		held := len(service.receiverHeldLanes)
		service.stateLock.Unlock()
		if held != 0 || stalled.sequence.windowPacer.receiverHeld {
			t.Fatalf("a closed lane remained receiver-held: lanes=%d", held)
		}
		sibling.write(t, 32)
		if !service.backlogged(receiverHoldTestRate) {
			t.Fatal("a sibling's window lost its flight after a held lane closed")
		}
	})
}
