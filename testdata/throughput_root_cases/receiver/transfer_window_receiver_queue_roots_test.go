// These isolated roots distinguish the receiver's application observation
// clock from physical serialization. They do not add a production estimator.
package connect

import (
	"testing"
	"time"
)

// A delayed receiver can drain frames faster than the continuously paced
// source supplied them. Repeated reads must not retain that local queue rate.
func TestWindowReceiverQueueCompressionKeepsPacedCapacity(t *testing.T) {
	fixture := newReceiverPhysicalSpanFixture(t, 12500000)
	sequence := fixture.sequence(0)
	first := fixture.offer(sequence, 100000, 0)
	last := fixture.offer(sequence, 100000, 8*time.Millisecond)
	fixture.ingress(first, 100000, 100*time.Millisecond)
	fixture.ingress(last, 100000, 100*time.Millisecond+time.Microsecond)
	fixture.ack(t, sequence, first, 200*time.Millisecond)
	fixture.ack(t, sequence, last, 201*time.Millisecond)
	for _, at := range []time.Duration{201 * time.Millisecond, 202 * time.Millisecond, 2 * time.Second} {
		fixture.requireRate(t, at, 12500000)
	}
}

// A real faster train has sufficient physical offer and receiver elapsed
// evidence. A safeguard against queue compression must keep upward adaptation.
func TestWindowReceiverQueueCompressionAllowsMeasuredCapacityIncrease(t *testing.T) {
	fixture := newReceiverPhysicalSpanFixture(t, 1250000)
	sequence := fixture.sequence(0)
	first := fixture.offer(sequence, 100000, 0)
	last := fixture.offer(sequence, 100000, time.Millisecond)
	fixture.ingress(first, 100000, 50*time.Millisecond)
	fixture.ingress(last, 100000, 58*time.Millisecond)
	fixture.ack(t, sequence, first, 100*time.Millisecond)
	fixture.ack(t, sequence, last, 101*time.Millisecond)
	fixture.requireRate(t, 101*time.Millisecond, 12500000)
}

// Both application clocks may drain buffered bursts. The sender route's
// accepted-write clock alone cannot prove the underlying physical rate.
func TestWindowReceiverQueueCompressionDoesNotPriceBufferedBurst(t *testing.T) {
	fixture := newReceiverPhysicalSpanFixture(t, 12500000)
	sequence := fixture.sequence(0)
	first := fixture.offer(sequence, 100000, 0)
	last := fixture.offer(sequence, 100000, 400*time.Microsecond)
	fixture.ingress(first, 100000, 100*time.Millisecond)
	fixture.ingress(last, 100000, 100*time.Millisecond+time.Microsecond)
	fixture.ack(t, sequence, first, 200*time.Millisecond)
	fixture.ack(t, sequence, last, 201*time.Millisecond)
	fixture.requireRate(t, 201*time.Millisecond, 12500000)
}
