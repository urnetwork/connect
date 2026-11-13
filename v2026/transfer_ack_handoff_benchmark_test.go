// Separate receive-caller admission cost from the same completed ACK work.
// The fixture runs unchanged with queued and synchronous publication sources.
package connect

import (
	"context"
	"math/bits"
	"testing"
	"time"
)

// One immutable retained identity exercises real validation/coalescing without
// network, background workers, packet buffers, or a mutable send-item owner.
func newAckHandoffBenchmark(kind string) (*SendSequence, receiveAckMessage, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	sequence := &SendSequence{
		ctx: ctx, cancel: cancel, client: &Client{}, sequenceId: NewId(),
		acks: make(chan receiveAckMessage, 1), ackWindow: newSequenceAckWindow(),
		resendQueue: newResendQueue(nil, 0),
	}
	item := &sendItem{transferItem: transferItem{messageId: NewId(), sequenceNumber: 7}}
	sequence.resendQueue.Add(item)
	ack := receiveAckMessage{sequenceId: sequence.sequenceId, messageId: item.messageId}
	switch kind {
	case "selective":
		ack.selective = true
	case "contract":
		ack.contractMissing, ack.missingContractId = true, NewId()
		item.contractId = &ack.missingContractId
	case "unknown":
		ack.messageId = NewId()
	}
	return sequence, ack, cancel
}

// Complete precisely the accepted ACK's processing in either implementation.
// Queue draining happens after the independently measured admission span.
func finishAckHandoffBenchmark(sequence *SendSequence, ack receiveAckMessage) bool {
	select {
	case queued := <-sequence.acks:
		sequence.coalesceReceivedAck(sequence.ackWindow, queued)
	default:
	}
	snapshot := sequence.ackWindow.Snapshot(true)
	if ack.contractMissing {
		feedback, ok := snapshot.contractMissingAcks[ack.messageId]
		return ok && feedback.missingContractId == ack.missingContractId && snapshot.ackUpdateCount == 0
	}
	if ack.selective {
		_, ok := snapshot.selectiveAcks[ack.messageId]
		return ok && snapshot.ackUpdateCount == 0
	}
	if _, retained := sequence.retainedAckSequenceNumber(ack.messageId); !retained {
		return snapshot.ackUpdateCount == 0 && len(snapshot.selectiveAcks) == 0 && len(snapshot.contractMissingAcks) == 0
	}
	return snapshot.ackUpdateCount == 1 && snapshot.headAck.messageId == ack.messageId
}

// Pin identical completed work across both source arms before timing them.
func TestTransferAckHandoffBenchmarkFixture(t *testing.T) {
	for _, kind := range []string{"cumulative", "selective", "contract", "unknown"} {
		sequence, ack, cancel := newAckHandoffBenchmark(kind)
		for range 3 {
			result, err := sequence.ackMessageDetailed(ack, 0)
			if result != receiveAckHandoffAccepted || err != nil || !finishAckHandoffBenchmark(sequence, ack) || len(sequence.acks) != 0 {
				t.Fatalf("%s did not complete exactly one accepted ACK: result=%d err=%v", kind, result, err)
			}
		}
		cancel()
	}
}

// The common cumulative path must not allocate merely to move publication
// into the receiving caller. The legacy drain is included for source parity.
func TestTransferAckHandoffCumulativeNoSteadyAllocations(t *testing.T) {
	sequence, ack, cancel := newAckHandoffBenchmark("cumulative")
	defer cancel()
	valid := true
	allocations := testing.AllocsPerRun(1000, func() {
		result, err := sequence.ackMessageDetailed(ack, 0)
		valid = valid && result == receiveAckHandoffAccepted && err == nil && finishAckHandoffBenchmark(sequence, ack)
	})
	if !valid || allocations != 0 {
		t.Fatalf("cumulative handoff valid=%t allocations=%g, want true/0", valid, allocations)
	}
}

// Total ns/op includes admission plus equivalent coalescing/snapshot work.
// Admission mean and finite log2 quantile upper bounds isolate the work moved
// onto the receiving caller. Clock reads affect both arms equally; this is a
// minimal no-service fixture, not an end-to-end latency or throughput result.
func BenchmarkTransferAckHandoff(b *testing.B) {
	for _, kind := range []string{"cumulative", "selective", "contract", "unknown"} {
		b.Run(kind, func(b *testing.B) {
			sequence, ack, cancel := newAckHandoffBenchmark(kind)
			defer cancel()
			var buckets [64]uint64
			var admissionNanos uint64
			b.ReportAllocs()
			for b.Loop() {
				started := time.Now()
				result, err := sequence.ackMessageDetailed(ack, 0)
				elapsed := uint64(max(0, time.Since(started).Nanoseconds()))
				admissionNanos += elapsed
				buckets[min(bits.Len64(elapsed), len(buckets)-1)] += 1
				if result != receiveAckHandoffAccepted || err != nil || !finishAckHandoffBenchmark(sequence, ack) {
					b.Fatalf("%s handoff result=%d err=%v", kind, result, err)
				}
			}
			b.ReportMetric(float64(admissionNanos)/float64(b.N), "admission-mean-ns")
			for _, quantile := range []struct {
				percent uint64
				label   string
			}{{percent: 50, label: "admission-p50-upper-ns"}, {percent: 99, label: "admission-p99-upper-ns"}} {
				threshold := (uint64(b.N)*quantile.percent + 99) / 100
				var count uint64
				for bucket, samples := range buckets {
					count += samples
					if count >= threshold {
						b.ReportMetric(float64(uint64(1)<<bucket), quantile.label)
						break
					}
				}
			}
		})
	}
}
