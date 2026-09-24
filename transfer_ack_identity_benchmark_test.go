package connect

import (
	"fmt"
	"runtime"
	"testing"
	"unsafe"
)

// Measure the static-window hot coalescer with the same retained identities
// and feedback before and after the recovery-lookup change. The live-item and
// sequence sizes make fixed owner cost distinct from per-message growth.
func BenchmarkRetainedAckHeadCoalescing(b *testing.B) {
	sequence := &SendSequence{client: &Client{}, resendQueue: newResendQueue(nil, 0)}
	item := &sendItem{transferItem: transferItem{messageId: NewId(), sequenceNumber: 7}}
	sequence.resendQueue.Add(item)
	window := newSequenceAckWindow()
	ack := receiveAckMessage{messageId: item.messageId}
	sequence.coalesceReceivedAck(window, ack)
	window.Snapshot(true)
	b.ReportAllocs()
	for b.Loop() {
		sequence.coalesceReceivedAck(window, ack)
		window.Snapshot(true)
	}
	b.ReportMetric(float64(unsafe.Sizeof(SendSequence{})), "sequence-B")
	b.ReportMetric(float64(unsafe.Sizeof(sendItem{})), "item-B")
}

// Allocate only the sequence envelope, with all owners kept live together.
// This separates allocator size-class cost from unchanged routes, queues and
// retained messages; it is not a complete-client memory qualification.
func BenchmarkRetainedAckSequenceFanout(b *testing.B) {
	for _, count := range []int{1, 64, 1024} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			owners := make([]*SendSequence, count)
			b.ReportAllocs()
			for b.Loop() {
				for i := range owners {
					owners[i] = &SendSequence{}
				}
			}
			runtime.KeepAlive(owners)
			b.ReportMetric(float64(unsafe.Sizeof(SendSequence{}))*float64(count), "live-owner-B")
			b.ReportMetric(float64(count), "owners/op")
		})
	}
}
