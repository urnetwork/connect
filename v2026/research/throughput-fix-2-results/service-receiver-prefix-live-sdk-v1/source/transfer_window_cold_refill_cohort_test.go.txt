// Exact first offers distinguish a newly refilled cold train from data that
// was already available throughout a real slow interval. These component
// roots consume normal sender estimates; they do not run a closed-loop path.
package connect

import (
	"testing"
	"time"
)

// Convert a byte count and exact nanosecond span without introducing a
// floating-point threshold into the deterministic cohort fixture.
func windowColdRefillByteRate(bytes ByteCount, spanNanos int64) ByteCount {
	if bytes <= 0 || spanNanos <= 0 {
		return 0
	}
	return ByteCount((uint64(bytes) * uint64(time.Second)) / uint64(spanNanos))
}

// The normal constrained opening owns all physical and cumulative credit.
// A stored permission bound avoids inserting extra consuming reads per write.
type windowColdRefillFixture struct {
	credit *windowReceiverCreditFixture
	start  time.Time
	window ByteCount
	acked  ByteCount
}

// No service rate or RTT is seeded. Every estimate uses actual fixture ACKs.
func newWindowColdRefillFixture(t *testing.T, phase time.Duration) *windowColdRefillFixture {
	t.Helper()
	sequence, start := newWindowCumulativePacingFixture(t)
	start = start.Add(phase)
	sequence.client = &Client{feedbackTimeBase: start}
	sequence.windowPacer.serviceSequenceId = sequence.sequenceId
	sequence.receiveWindowSetAtNanos.Store(start.UnixNano())
	t.Cleanup(sequence.windowPacer.close)
	return &windowColdRefillFixture{
		credit: &windowReceiverCreditFixture{sequence: sequence, service: sequence.windowPacer.service, ackWindow: newSequenceAckWindow(), compression: 10 * time.Millisecond},
		start:  start, window: sequence.sendWindowEstimate(start).Window,
	}
}

// Each physical item is within the ordinary frame and current lane bounds.
func (self *windowColdRefillFixture) write(t *testing.T, number uint64, at time.Time) *sendItem {
	t.Helper()
	const bytes ByteCount = 2670
	pacer := &self.credit.sequence.windowPacer
	if pacer.serviceSent-pacer.serviceAcked+bytes > self.window {
		t.Fatal("the prescribed physical offer exceeded current lane permission")
	}
	return self.credit.write(number, bytes, at)
}

// The coalescer publishes service once; the worker applies only logical bytes.
// The returned estimate is the single consuming read at this clock edge.
func (self *windowColdRefillFixture) ack(t *testing.T, item *sendItem, bytes ByteCount, at time.Time, wait time.Duration) SendWindowEstimate {
	t.Helper()
	if !item.sendTime.Before(at.Add(-wait)) {
		t.Fatal("a reply preceded its own confirmed physical offer")
	}
	sequence := self.credit.sequence
	before := sequence.windowPacer.serviceAcked
	sequence.coalesceReceivedAck(self.credit.ackWindow, self.credit.ack(item, at, wait))
	if delta := sequence.windowPacer.serviceAcked - before; delta != bytes {
		t.Fatalf("the exact head credited %d, want %d", delta, bytes)
	}
	self.acked += bytes
	sequence.observeAckedBytesWithServiceCredit(bytes, 0, windowServiceAckCredit{}, at)
	if self.credit.service.total != self.acked || sequence.deliveredByteTotal != self.acked {
		t.Fatal("physical and cumulative byte ownership diverged")
	}
	estimate := sequence.sendWindowEstimate(at)
	if estimate.Window < self.window || estimate.Window > estimate.Ceiling || estimate.PacingByteRate > estimate.PacingProbeByteRate {
		t.Fatalf("unchanged hard permission or retained bytes changed: %+v", estimate)
	}
	self.window = estimate.Window
	return estimate
}

// The SDK opening has a legal short prefix, then a refill before its last
// reply. Later preoffered refills keep the physical flight continuously open.
func (self *windowColdRefillFixture) opening(t *testing.T, path time.Duration, refills int) ([]*sendItem, time.Time, ByteCount) {
	t.Helper()
	var opening [123]*sendItem
	for number := range opening {
		opening[number] = self.write(t, uint64(number), self.start)
	}
	firstAt := self.start.Add(path + 21360*time.Nanosecond)
	first := self.ack(t, opening[0], 2670, firstAt, 0)
	if first.ServiceByteRate != 0 {
		t.Fatal("one cold receipt supplied a serialization pair")
	}
	items := make([]*sendItem, refills)
	items[0] = self.write(t, 123, firstAt)
	tailAt := self.start.Add(path + 3627280*time.Nanosecond)
	tail := self.ack(t, opening[122], 122*2670, tailAt, time.Millisecond)
	if tail.ServiceByteRate != 0 || self.credit.service.drained || self.credit.service.latestRoundTrip != tailAt.Sub(self.start) {
		t.Fatalf("the ambiguous opening changed raw flight or acquired exact service: %+v", tail)
	}
	for number := 1; number < refills; number++ {
		items[number] = self.write(t, uint64(123+number), tailAt)
	}
	// This is a conservative already-delivered lower bound, not a claimed
	// 125 MB/s capacity. Every opening byte was offered inside this span.
	supported := windowColdRefillByteRate(123*2670, int64(tailAt.Sub(self.start)))
	if tail.PacingByteRate < supported*9/10 {
		t.Fatalf("opening lacked its supported pacing precondition: %+v lower=%d", tail, supported)
	}
	return items, firstAt.Add(path + 21360*time.Nanosecond), supported
}

// One receipt from data offered only after the opening's first ACK cannot
// turn its propagation gap into a newly measured slow serializer. Abstention
// and another independently supported pacing source are both valid outcomes.
func TestWindowPacingColdRefillKeepsSupportedPacing(t *testing.T) {
	for _, path := range []time.Duration{100 * time.Millisecond, 400 * time.Millisecond} {
		for _, phase := range []time.Duration{0, 8 * time.Millisecond} {
			fixture := newWindowColdRefillFixture(t, phase)
			items, at, supported := fixture.opening(t, path, 2)
			estimate := fixture.ack(t, items[0], 2670, at, 0)
			if fixture.credit.service.drained || fixture.credit.service.latestRoundTrip != at.Sub(items[0].sendTime) {
				t.Fatal("the first refill altered raw timing or claimed a drained flight")
			}
			if estimate.PacingByteRate < supported*9/10 {
				t.Errorf("path=%s phase=%s: one new-cohort receipt repriced propagation: service=%d pacing=%d supported=%d", path, phase, estimate.ServiceByteRate, estimate.PacingByteRate, supported)
			}
		}
	}
}

// An older physical offer was available throughout the long interval. Its
// genuine slow delivery must remain usable without any connection signal.
func TestWindowPacingColdPreofferedTrainKeepsSlowPacing(t *testing.T) {
	fixture := newWindowColdRefillFixture(t, 0)
	first := fixture.write(t, 0, fixture.start)
	second := fixture.write(t, 1, fixture.start)
	fixture.write(t, 2, fixture.start)
	firstAt := fixture.start.Add(100 * time.Millisecond)
	fixture.ack(t, first, 2670, firstAt, 0)
	estimate := fixture.ack(t, second, 2670, firstAt.Add(100*time.Millisecond), 0)
	if estimate.PacingByteRate <= 0 || estimate.PacingByteRate > 29370 || fixture.credit.service.drained {
		t.Fatalf("preoffered slow delivery lost its pacing evidence: %+v", estimate)
	}
}

// Fresh preoffered pairs can replace the opening after its first refill.
// Slow delivery is measured again; the older opening is no permanent floor.
func TestWindowPacingColdRefillFreshSlowPairLowersPacing(t *testing.T) {
	fixture := newWindowColdRefillFixture(t, 0)
	items, at, _ := fixture.opening(t, 100*time.Millisecond, 10)
	var estimate SendWindowEstimate
	for number := range 9 {
		estimate = fixture.ack(t, items[number], 2670, at.Add(time.Duration(number)*100*time.Millisecond), 0)
	}
	if estimate.PacingByteRate <= 0 || estimate.PacingByteRate > 29370 || fixture.credit.service.drained {
		t.Fatalf("independent fresh slow pairs retained the opening: %+v", estimate)
	}
}

// A later ordinary fresh pair can also prove faster delivery. The cohort
// boundary cannot leave service at zero or the gap rate. An unloaded path's
// discovery pace may exceed observed delivery while it tests spare capacity.
func TestWindowPacingColdRefillFreshFasterPairMeasuresService(t *testing.T) {
	fixture := newWindowColdRefillFixture(t, 0)
	items, at, _ := fixture.opening(t, 100*time.Millisecond, 3)
	fixture.ack(t, items[0], 2670, at, 0)
	estimate := fixture.ack(t, items[1], 2670, at.Add(10*time.Millisecond), 0)
	if estimate.ServiceByteRate != 267000 || estimate.PacingByteRate < 267000*9/10 || fixture.credit.service.drained {
		t.Fatalf("a fresh 10ms delivery pair did not measure service: %+v", estimate)
	}
}

// A new offer alone does not split a continuous feedback interval. These
// ordinary nearby receipts still supply useful serialization evidence.
func TestWindowPacingColdContiguousRefillKeepsPair(t *testing.T) {
	fixture := newWindowColdRefillFixture(t, 0)
	first := fixture.write(t, 0, fixture.start)
	// An independent confirmed tail prevents a global physical drain; its
	// bytes remain outstanding and cannot join either measured checkpoint.
	sibling := newWindowReceiverCreditFixture(t, fixture.credit.service, fixture.start, 10*time.Millisecond)
	sibling.write(0, 2670, fixture.start)
	firstAt := fixture.start.Add(100 * time.Millisecond)
	fixture.ack(t, first, 2670, firstAt, 0)
	second := fixture.write(t, 1, firstAt)
	fixture.write(t, 2, firstAt)
	estimate := fixture.ack(t, second, 2670, firstAt.Add(10*time.Millisecond), 0)
	if estimate.ServiceByteRate != 267000 || estimate.PacingByteRate < 267000*9/10 {
		t.Fatalf("a contiguous refill discarded its ordinary pair: %+v", estimate)
	}
}
