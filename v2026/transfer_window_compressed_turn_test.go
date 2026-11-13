// A compressed ACK turn ends on the receiver's timer, not on an arrival.
// Since packets grew to 1280 bytes, one three-packet H1 message (3961 wire
// bytes) serializes in 31.688 ms at 125 kB/s, so a busy path's 50 ms turns
// alternate one and two messages. A window between two such turns is
// quantized to whole messages; exact receiver and head clocks are not.
package connect

import (
	"testing"
	"time"
)

// One H1 message carrying three 1280-byte packets and its Transfer framing.
const windowCompressedTurnMessageByteCount = ByteCount(3961)

// A backlogged FIFO on a 400 ms path finishes one message per serialization.
// Every message is offered before the first finishes, so the queue never
// empties and every reply's newest head and wait are exact.
type windowCompressedTurnPath struct {
	fixture       *windowReceiverCreditFixture
	offered       []*sendItem
	offeredAt     time.Time
	serialization time.Duration
	acknowledged  int
}

// The previously measured serializer survives a path-change epoch, as it does
// after a drain probe; only fresh delivery may reprice it.
func newWindowCompressedTurnPath(t *testing.T, compression, serialization time.Duration, messages int) *windowCompressedTurnPath {
	t.Helper()
	start := time.Unix(1700000000, 0)
	fixture := newWindowReceiverCreditFixture(t, nil, start, compression)
	t.Cleanup(func() { fixture.sequence.resendQueue.Clear() })
	fixture.service.serviceHoldRate = 125000
	// A paced sender reserves each message, recording its indivisible size.
	fixture.service.maxMessageByteCount = windowCompressedTurnMessageByteCount
	changedAt := start.Add(time.Second)
	fixture.service.networkQualityChangedOnPath(changedAt, 1, 0)
	if fixture.service.serviceHoldRate != 125000 || !fixture.service.serviceEpochAt.Equal(changedAt) {
		t.Fatal("the path-change epoch did not keep the held serializer")
	}
	path := &windowCompressedTurnPath{fixture: fixture, offeredAt: changedAt.Add(time.Millisecond),
		serialization: serialization, acknowledged: -1}
	for number := range messages {
		path.offered = append(path.offered, fixture.write(uint64(number), windowCompressedTurnMessageByteCount, path.offeredAt))
	}
	return path
}

// The exact receiver arrival of one message, in the sender's ACK clock.
func (self *windowCompressedTurnPath) head(number int) time.Time {
	return self.offeredAt.Add(400*time.Millisecond + time.Duration(number+1)*self.serialization)
}

// Periodic replies each name the newest arrived message and its own wait.
// Returns the last reply time and the number of messages each reply credited.
func (self *windowCompressedTurnPath) acknowledgeTurns(firstAt time.Time, turn time.Duration, count int) (time.Time, []int) {
	at := firstAt
	var credited []int
	for i := range count {
		at = firstAt.Add(time.Duration(i) * turn)
		newest := self.acknowledged
		for newest+1 < len(self.offered) && !self.head(newest+1).After(at) {
			newest++
		}
		if newest == self.acknowledged {
			continue
		}
		ack := self.fixture.ack(self.offered[newest], at, at.Sub(self.head(newest)))
		self.fixture.sequence.coalesceReceivedAck(self.fixture.ackWindow, ack)
		credited = append(credited, newest-self.acknowledged)
		self.acknowledged = newest
	}
	return at, credited
}

// After the epoch, a two-message turn is followed by a one-message turn. The
// single message's 50 ms window is the receiver timer; a serializer busy at
// the held 125 kB/s completes one or two messages in it. Pricing that window
// reads one message per turn, 79,220 B/s, and a paced sender at 1.1 times that
// never fills the path again to re-measure it. Slower serializers must still
// lower the hold: exact single-message arrivals a full turn apart, and raw
// two-message turns slow enough to survive one more message.
func TestWindowPacingCompressedSingleMessageTurnKeepsHeldService(t *testing.T) {
	for _, test := range []struct {
		name          string
		compression   time.Duration
		serialization time.Duration
		firstWait     time.Duration
		credited      []int
		want          ByteCount
	}{
		{name: "held-alternating", compression: 50 * time.Millisecond, serialization: 31688 * time.Microsecond,
			firstWait: 41688 * time.Microsecond, credited: []int{2, 1}, want: 125000},
		{name: "slower-exact", compression: 50 * time.Millisecond, serialization: 50 * time.Millisecond,
			firstWait: 30 * time.Millisecond, credited: []int{1, 1}, want: 79220},
		{name: "slower-raw", compression: 100 * time.Millisecond, serialization: 50 * time.Millisecond,
			firstWait: 80 * time.Millisecond, credited: []int{2, 2}, want: 79220},
	} {
		path := newWindowCompressedTurnPath(t, test.compression, test.serialization, 8)
		at, credited := path.acknowledgeTurns(path.head(0).Add(test.firstWait), test.compression, len(test.credited))
		if len(credited) != len(test.credited) || credited[0] != test.credited[0] || credited[1] != test.credited[1] {
			t.Fatalf("%s: replies credited %v messages per turn, want %v", test.name, credited, test.credited)
		}
		service := path.fixture.service
		if got := path.fixture.measured(at); got != test.want || service.serviceHoldRate != test.want {
			t.Errorf("%s: turn window priced service=%d hold=%d, want %d", test.name, got, service.serviceHoldRate, test.want)
		}
	}
}

// A queued path averages a full feedback interval before lowering service.
// Its raw endpoints are still timer turns: with 100 ms turns of three or four
// messages, five turns can hold fifteen messages, 118,830 B/s, although a
// serializer busy at the held 125 kB/s completes 15.78 messages in 500 ms.
// A real slower serializer still lowers the average.
func TestWindowPacingQueuedAverageKeepsHeldService(t *testing.T) {
	for _, test := range []struct {
		serialization time.Duration
		want          ByteCount
	}{
		{serialization: 31688 * time.Microsecond, want: 125000},
		{serialization: 50 * time.Millisecond, want: 79220},
	} {
		// Enough offered flight remains outstanding to exceed the queued
		// average's own flight bound at either rate.
		path := newWindowCompressedTurnPath(t, 100*time.Millisecond, test.serialization, 48)
		// The first reply follows the third arrival by 3 ms. That phase puts
		// fifteen 31.688 ms arrivals in the next five turns, not sixteen.
		at, credited := path.acknowledgeTurns(path.head(2).Add(3*time.Millisecond), 100*time.Millisecond, 6)
		window := 0
		for _, count := range credited[1:] {
			window += count
		}
		service := path.fixture.service
		queued := func() bool {
			service.stateLock.Lock()
			defer service.stateLock.Unlock()
			timing := service.roundTripEvidenceWithLock(at)
			return timing.latest > timing.minimum+2*time.Millisecond &&
				service.outstandingWithLock() > service.flightBoundAtWithLock(125000, timing.residence)
		}()
		if len(credited) != 6 || !queued {
			t.Fatalf("serialization=%s: the path did not prove six replies, a queue and an excess flight: %v", test.serialization, credited)
		}
		if test.want == 125000 && window != 15 {
			t.Fatalf("serialization=%s: the five-turn window credited %d messages, want 15", test.serialization, window)
		}
		if got := path.fixture.measured(at); got != test.want || service.serviceHoldRate != test.want {
			t.Errorf("serialization=%s: queued timer turns priced service=%d hold=%d, want %d", test.serialization, got, service.serviceHoldRate, test.want)
		}
	}
}
