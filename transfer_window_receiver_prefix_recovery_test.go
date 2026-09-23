package connect

import (
	"testing"
	"time"
)

// A finite, independently serialized suffix waits behind a lost head in each
// lane. Repair releases those retained bytes over more than one compression
// interval, but that release cadence is not fresh physical service capacity.
func TestWindowPacingRecoveredPrefixCannotRaiseService(t *testing.T) {
	const physicalRate ByteCount = 12500000
	const lanes = 8
	const messages = 128
	const messageBytes ByteCount = 8192
	const path = 1200 * time.Millisecond
	const compression = 10 * time.Millisecond
	service, start := newWindowQualifiedServiceFixture(t, physicalRate, compression, 300*time.Microsecond)
	before := service.total
	change := start.Add(4 * time.Second)
	service.networkQualityChanged(change)
	fixtures := make([]*windowReceiverCreditFixture, lanes)
	heads := make([]*sendItem, lanes)
	holes := make([]*sendItem, lanes)
	for i := range fixtures {
		fixtures[i] = newWindowReceiverCreditFixture(t, service, start, compression)
	}
	// These explicit timing inputs fit a two-MiB opening per lane and the
	// unchanged 48-MiB process permission. This fixture tests credit/timing,
	// not admission; the actual-worker path tests enforce those permissions.
	// No frame exceeds the limit and the shared send clock is 12.5 MB/s.
	for number := range messages {
		for lane, fixture := range fixtures {
			index := number*lanes + lane
			sent := change.Add(time.Millisecond + windowPacingSerializationTime(ByteCount(index)*messageBytes, physicalRate))
			item := fixture.write(uint64(number), messageBytes, sent)
			if number == 0 {
				holes[lane] = item
			}
			heads[lane] = item
		}
	}
	if lanes*messages*messageBytes > mib(48) || messages*messageBytes > mib(2) {
		t.Fatal("fixture exceeds physical permission")
	}
	// The original heads were lost. These are explicit successful recovery
	// writes of those identities, not invented delivery or duplicated credit.
	for lane, fixture := range fixtures {
		hole := holes[lane]
		fixture.sequence.invalidateReceiverRttWrite(hole)
		repaired := change.Add(3*time.Second + time.Duration(lane)*20*time.Millisecond)
		service.beginWrite(fixture.sequence.sequenceId, hole.messageId, hole.sequenceNumber, repaired.Add(-path), true)
		service.finishWrite(fixture.sequence.sequenceId, hole.messageId, true)
	}
	for lane, fixture := range fixtures {
		repaired := change.Add(3*time.Second + time.Duration(lane)*20*time.Millisecond)
		head := heads[lane]
		wait := repaired.Sub(head.sendTime) - path
		if wait <= compression {
			t.Fatal("fixture did not hold the suffix beyond ordinary compression")
		}
		ack := fixture.ack(head, repaired, wait)
		fixture.sequence.coalesceReceivedAck(fixture.ackWindow, ack)
		fixture.sequence.coalesceReceivedAck(fixture.ackWindow, ack)
		rate := fixture.measured(repaired)
		t.Logf("lane=%d receiver-hold=%s measured=%d total=%d", lane, wait, rate, service.total-before)
		if rate > physicalRate {
			t.Errorf("retained prefix release raised physical service: lane=%d got=%d physical=%d", lane, rate, physicalRate)
		}
	}
	if got := service.total - before; got != lanes*messages*messageBytes {
		t.Fatalf("recovery changed once-only physical credit: got=%d want=%d", got, lanes*messages*messageBytes)
	}
	// The guard is not a capacity ceiling. An independent later serialized
	// train must prove a real speed increase without a reset or drain trick.
	const fasterRate ByteCount = 25000000
	fixture := fixtures[0]
	var fresh [messages]*sendItem
	for i := range fresh {
		sent := change.Add(4*time.Second + windowPacingSerializationTime(ByteCount(i)*messageBytes, fasterRate))
		fresh[i] = fixture.write(uint64(messages+i), messageBytes, sent)
	}
	var measured ByteCount
	for _, item := range fresh {
		at := item.sendTime.Add(path)
		fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(item, at, 0))
		measured = fixture.measured(at)
	}
	if measured < fasterRate*99/100 || measured > fasterRate*101/100 || !service.qualityServiceMeasured {
		t.Fatalf("fresh independent serialization could not raise service: measured=%d qualified=%t", measured, service.qualityServiceMeasured)
	}
	if got := service.total - before; got != (lanes+1)*messages*messageBytes {
		t.Fatalf("fresh train changed exact physical credit: got=%d", got)
	}
}

// Only a newly credited multi-item prefix with a validated head wait beyond
// ordinary compression carries the ambiguity. Old providers remain unchanged.
func TestWindowPacingRecoveredPrefixProvenance(t *testing.T) {
	for _, test := range []struct {
		name                                                       string
		wait                                                       time.Duration
		legacy, wrongTag, priorGeneration, sackedPrefix, selective bool
		want                                                       bool
	}{
		{name: "held", wait: 11 * time.Millisecond, want: true},
		{name: "compression", wait: 10 * time.Millisecond},
		{name: "zero"},
		{name: "legacy", wait: 20 * time.Millisecond, legacy: true},
		{name: "unvalidated", wait: 20 * time.Millisecond, wrongTag: true},
		{name: "old-and-new-generation", wait: 20 * time.Millisecond, priorGeneration: true},
		{name: "already-sacked-prefix", wait: 20 * time.Millisecond, sackedPrefix: true},
		{name: "selective-only", wait: 20 * time.Millisecond, selective: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			start := time.Unix(1700000000, 0)
			fixture := newWindowReceiverCreditFixture(t, nil, start, 10*time.Millisecond)
			first := fixture.write(0, 8192, start)
			if test.priorGeneration {
				fixture.service.networkQualityChanged(start.Add(time.Millisecond))
			}
			head := fixture.write(1, 8192, start.Add(2*time.Millisecond))
			if test.sackedPrefix {
				sack := fixture.ack(first, start.Add(50*time.Millisecond), 0)
				sack.selective = true
				fixture.sequence.coalesceReceivedAck(fixture.ackWindow, sack)
			}
			at := start.Add(100 * time.Millisecond)
			ack := fixture.ack(head, at, test.wait)
			ack.selective = test.selective
			if test.legacy {
				ack.receiverAckDelaySet = false
			}
			if test.wrongTag {
				ack.tag.sendTime++
			}
			fixture.sequence.coalesceReceivedAck(fixture.ackWindow, ack)
			fixture.sequence.coalesceReceivedAck(fixture.ackWindow, ack)
			wantMarker := int64(0)
			if test.want {
				wantMarker = at.UnixNano()
			}
			if got := fixture.service.receiverHeldPrefixAtNanos; got != wantMarker {
				t.Fatalf("held-prefix provenance=%d want=%d", got, wantMarker)
			}
			wantBytes := ByteCount(16384)
			if test.selective {
				wantBytes = 8192
			}
			if fixture.service.total != wantBytes || fixture.sequence.windowPacer.serviceAcked != wantBytes {
				t.Fatalf("provenance changed ownership: service=%d sequence=%d want=%d", fixture.service.total, fixture.sequence.windowPacer.serviceAcked, wantBytes)
			}
		})
	}
}

// Held-prefix evidence is bounded state and remains monotonic when sibling
// ACK callbacks arrive out of order; it neither drops delivery nor blocks a
// measured slowdown through the same interval.
func TestWindowPacingRecoveredPrefixAcceptsSlowService(t *testing.T) {
	const original ByteCount = 12500000
	service, start := newWindowQualifiedServiceFixture(t, original, 10*time.Millisecond, 100*time.Millisecond)
	fixture := newWindowReceiverCreditFixture(t, service, start, 10*time.Millisecond)
	before := service.total
	first := start.Add(300 * time.Millisecond)
	last := first.Add(100 * time.Millisecond)
	for _, at := range []time.Time{last, first} {
		credit := windowServiceAckCredit{bytes: 1000, firstSentAtNanos: start.UnixNano(), receiverHeldPrefix: true}
		fixture.sequence.windowPacer.serviceSent += credit.bytes
		service.sent += credit.bytes
		fixture.sequence.observePacingServiceCredit(credit, at)
	}
	if service.receiverHeldPrefixAtNanos != last.UnixNano() || service.total-before != 2000 {
		t.Fatal("late sibling moved the ambiguity boundary or changed delivery")
	}
	if got := fixture.measured(last); got <= 0 || got > 10000 {
		t.Fatalf("held prefix prevented slow delivery measurement: got=%d want<=10000", got)
	}
}

// The recovery guard can reject many candidate intervals. Measure that bounded
// history scan separately from the ordinary unambiguous-service fast path.
func BenchmarkWindowPacingRecoveredPrefixMeasurement(b *testing.B) {
	const physicalRate ByteCount = 12500000
	service := newWindowPacingService(DefaultSendBufferSettings())
	at := time.Unix(1700000000, 0)
	for range 8 {
		at = at.Add(10 * time.Millisecond)
		service.sent += 125000
		service.observeRoundTrip(100*time.Millisecond, 10*time.Millisecond, at)
		service.observe(125000, at)
	}
	if rate, _, _ := service.measured(time.Second, at); rate != physicalRate {
		b.Fatal("fixture did not establish physical service")
	}
	for range deliveredBytesRingSize {
		at = at.Add(10 * time.Millisecond)
		service.sent += 1048576
		service.observeRoundTrip(100*time.Millisecond, 10*time.Millisecond, at)
		service.receiverHeldPrefixAtNanos = at.UnixNano()
		service.observe(1048576, at)
	}
	var rate, latest ByteCount
	b.ReportAllocs()
	for b.Loop() {
		rate, _, latest = service.measured(time.Second, at)
	}
	if max(rate, latest) != physicalRate {
		b.Fatalf("held-prefix train repriced service: %d/%d", rate, latest)
	}
}
