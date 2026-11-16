package connect

import (
	"testing"
	"time"
)

// Eight independently compressed cumulative heads expose almost two complete
// physical service turns over only one turn of raw ACK arrival time. Every
// wire byte is explicitly serialized and every head has its own validated wait.
func TestWindowPacingUnresolvedSharedPrefixesNeedFullSampleInterval(t *testing.T) {
	for _, phase := range []time.Duration{0, 7 * time.Millisecond, 19 * time.Millisecond} {
		t.Run(phase.String(), func(t *testing.T) {
			testWindowPacingUnresolvedSharedPrefixes(t, phase)
		})
	}
}

func testWindowPacingUnresolvedSharedPrefixes(t *testing.T, phase time.Duration) {
	t.Helper()
	const physicalRate ByteCount = 12500000
	const lanes, perBatch, batches = 8, 5, 2
	const bytes ByteCount = 3125
	const path = 1200 * time.Millisecond
	const compression = 10 * time.Millisecond
	service, start := newWindowQualifiedServiceFixture(t, physicalRate, compression, 300*time.Microsecond)
	change := start.Add(4*time.Second + phase)
	service.networkQualityChanged(change)
	fixtures := make([]*windowReceiverCreditFixture, lanes)
	var heads [batches][lanes]*sendItem
	for lane := range fixtures {
		fixtures[lane] = newWindowReceiverCreditFixture(t, service, start, compression)
	}
	// Enroll the complete physical flight before any reply. A lane cannot
	// become an empty-flight RTT probe between the two compressed batches.
	for number := range perBatch * batches {
		for lane, fixture := range fixtures {
			index := number*lanes + lane
			at := change.Add(time.Millisecond + windowPacingSerializationTime(ByteCount(index)*bytes, physicalRate))
			item := fixture.write(uint64(number), bytes, at)
			heads[number/perBatch][lane] = item
		}
	}
	before := service.total
	var lastAt time.Time
	for batch := range batches {
		for lane, fixture := range fixtures {
			head := heads[batch][lane]
			at := change.Add(time.Millisecond + path + time.Duration(batch+1)*compression + time.Duration(lane)*250*time.Microsecond)
			wait := at.Sub(head.sendTime) - path - 250*time.Microsecond
			if wait <= 0 || wait >= compression {
				t.Fatal("fixture needs ordinary positive compression, not a retained-hole release")
			}
			ack := fixture.ack(head, at, wait)
			fixture.sequence.coalesceReceivedAck(fixture.ackWindow, ack)
			fixture.sequence.coalesceReceivedAck(fixture.ackWindow, ack)
			if rate := fixture.measured(at); rate != physicalRate {
				t.Errorf("partial compressed phase changed physical service: batch=%d lane=%d got=%d physical=%d", batch, lane, rate, physicalRate)
			}
			lastAt = at
		}
	}
	if got := service.total - before; got != lanes*perBatch*batches*bytes {
		t.Fatalf("once-only physical credit changed: got=%d", got)
	}
	if service.bucketInterval <= compression || service.receiverHeldPrefixAtNanos != 0 {
		t.Fatal("fixture must use the long-path sampler without retained-prefix recovery evidence")
	}
	// This is not a rate ceiling or a blanket ban on short intervals. Fresh
	// exact receiver endpoints prove faster serialization before one long-path
	// sampler turn, while still spanning the advertised compression interval.
	const fasterRate ByteCount = 25000000
	const freshBytes ByteCount = 8192
	var fresh [40]*sendItem
	fixture := fixtures[0]
	for i := range fresh {
		sent := lastAt.Add(40*time.Millisecond + windowPacingSerializationTime(ByteCount(i)*freshBytes, fasterRate))
		fresh[i] = fixture.write(uint64(perBatch*batches+i), freshBytes, sent)
	}
	var measured ByteCount
	for _, item := range fresh {
		at := item.sendTime.Add(path)
		fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(item, at, 0))
		measured = fixture.measured(at)
	}
	span := fresh[len(fresh)-1].sendTime.Sub(fresh[0].sendTime)
	if span < compression || span >= service.bucketInterval {
		t.Fatalf("fresh control must be shorter than a sampler turn: span=%s interval=%s", span, service.bucketInterval)
	}
	if measured != fasterRate {
		t.Fatalf("fresh exact receiver interval lost faster capacity: got=%d want=%d", measured, fasterRate)
	}
	if got := service.total - before; got != lanes*perBatch*batches*bytes+ByteCount(len(fresh))*freshBytes {
		t.Fatalf("fresh train changed physical credit: got=%d", got)
	}
}
