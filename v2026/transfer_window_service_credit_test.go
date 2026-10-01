// Cumulative and selective service credit have their own bounded ownership,
// independent of retry-queue residence and worker delivery callbacks.
package connect

import (
	"math"
	"testing"
	"time"
)

// Creates real queue indexes without network workers. Pacing counters use the
// original wire envelope count; the empty encoded buffers own no pool storage.
func testWindowServiceCreditSequence(numbers []uint64) (*SendSequence, []*sendItem) {
	service := newWindowPacingService(DefaultSendBufferSettings())
	sequence := &SendSequence{resendQueue: newResendQueue(nil, 0), windowPacer: windowBurstPacer{service: service}}
	items := make([]*sendItem, 0, len(numbers))
	for i, number := range numbers {
		item := &sendItem{transferItem: transferItem{messageId: NewId(), sequenceNumber: number},
			pacingByteCount: ByteCount(1000 + i), pacingSentAtNanos: time.Unix(1700000000, int64(i)).UnixNano()}
		sequence.resendQueue.Add(item)
		sequence.windowPacer.serviceSent += item.pacingByteCount
		service.sent += item.pacingByteCount
		items = append(items, item)
	}
	return sequence, items
}

// A duplicate SACK and its later cumulative absorption never create a second
// physical delivery, even before the worker can retire any of the envelopes.
func TestWindowPacingServiceCreditDeduplicatesHeadAndSelective(t *testing.T) {
	sequence, items := testWindowServiceCreditSequence([]uint64{0, 1, 2, 3})
	at := time.Unix(1700000001, 0)
	for _, step := range []struct {
		index     int
		selective bool
		want      ByteCount
	}{
		{index: 3, selective: true, want: 1003},
		{index: 3, selective: true, want: 1003},
		{index: 1, want: 3004},
		{index: 3, want: 4006},
		{index: 0, want: 4006},
		{index: 2, selective: true, want: 4006},
	} {
		sequence.publishAckServiceCredit(items[step.index].messageId, step.selective, at)
		if got := sequence.windowPacer.service.total; got != step.want || sequence.windowPacer.serviceAcked != step.want {
			t.Fatalf("index=%d selective=%t: credit=%d ownership=%d want=%d", step.index, step.selective, got, sequence.windowPacer.serviceAcked, step.want)
		}
	}
}

// A retry temporarily removes its item from both queue indexes. The worker
// can publish that missing old envelope later, without repeating the suffix.
func TestWindowPacingServiceCreditWorkerFillsRemovedRetry(t *testing.T) {
	sequence, items := testWindowServiceCreditSequence([]uint64{0, 1, 2})
	at := time.Unix(1700000001, 0)
	if removed := sequence.resendQueue.RemoveByMessageId(items[0].messageId); removed != items[0] {
		t.Fatal("retry did not retain its original item")
	}
	sequence.publishAckServiceCredit(items[2].messageId, false, at)
	if got := sequence.windowPacer.service.total; got != 2003 {
		t.Fatalf("present suffix credit=%d, want2003", got)
	}
	sequence.resendQueue.Add(items[0])
	for _, item := range items {
		sequence.observePacingServiceCredit(sequence.takePacingServiceCredit(item), at)
	}
	sequence.publishAckServiceCredit(items[2].messageId, false, at.Add(time.Second))
	if got := sequence.windowPacer.service.total; got != 3003 || sequence.windowPacer.serviceAcked != got {
		t.Fatalf("worker fallback duplicated or lost old credit: total=%d ownership=%d", got, sequence.windowPacer.serviceAcked)
	}
}

// A synthetic sparse prefix cannot turn one authenticated live identity into
// a huge numeric loop, and the final sequence number cannot wrap the loop.
func TestWindowPacingServiceCreditBoundsSparseAndMaximumHeads(t *testing.T) {
	sequence, items := testWindowServiceCreditSequence([]uint64{0, math.MaxUint64 - 1, math.MaxUint64})
	at := time.Unix(1700000001, 0)
	sequence.publishAckServiceCredit(NewId(), false, at)
	if sequence.serviceAckHeadSet || sequence.windowPacer.service.total != 0 {
		t.Fatal("unknown identity occupied the cumulative credit boundary")
	}
	sequence.publishAckServiceCredit(items[1].messageId, false, at)
	sequence.publishAckServiceCredit(items[2].messageId, false, at)
	sequence.publishAckServiceCredit(items[2].messageId, false, at)
	if !sequence.serviceAckHeadSet || sequence.serviceAckHeadNumber != math.MaxUint64 || sequence.windowPacer.service.total != 3003 {
		t.Fatal("sparse or maximum head did not publish exactly its live prefix")
	}
}

// Closing releases only unacknowledged ownership. A late callback and another
// close are inert even if they retained a valid old item before cancellation.
func TestWindowPacingServiceCreditCloseRejectsLatePublication(t *testing.T) {
	sequence, items := testWindowServiceCreditSequence([]uint64{0, 1})
	at := time.Unix(1700000001, 0)
	sequence.publishAckServiceCredit(items[0].messageId, false, at)
	late := sequence.takePacingServiceCredit(items[1])
	sequence.windowPacer.close()
	sequence.observePacingServiceCredit(late, at)
	sequence.windowPacer.close()
	service := sequence.windowPacer.service
	if service.total != 1000 || service.sent != 1000 {
		t.Fatalf("close or late publication changed released ownership: sent=%d total=%d", service.sent, service.total)
	}
}

// Every byte in a group must have an observed first offer before that group
// can establish a new flight. Neither map iteration order nor zero hides gaps.
func TestWindowPacingServiceCreditRetainsEarliestPhysicalOffer(t *testing.T) {
	for _, times := range [][]int64{{30, 10, 20}, {0, 10, 20}, {30, 0, 20}, {30, 20, 0}} {
		credit := windowServiceAckCredit{}
		want := times[0]
		for _, at := range times {
			want = min(want, at)
			item := &sendItem{pacingByteCount: 1000, pacingSentAtNanos: at}
			credit.addItemWithLock(item)
			credit.addItemWithLock(item)
		}
		if credit.bytes != 3000 || credit.firstSentAtNanos != want {
			t.Fatalf("times=%v: bytes=%d earliest=%d want3000/%d", times, credit.bytes, credit.firstSentAtNanos, want)
		}
	}
}
