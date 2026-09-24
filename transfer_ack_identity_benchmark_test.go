package connect

import (
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
