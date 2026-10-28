package connect

import (
	"testing"
	"time"
)

// testTransportDelay returns measuring delay state on a controlled clock: now
// drives interval and duration time, micros drives the wire transport clock.
func testTransportDelay(settings *TransportDelaySettings) (*transportDelay, *time.Time, *int64, *[]*TransportDelayStats) {
	published := []*TransportDelayStats{}
	delay := newTransportDelay(settings, NewId(), TransportTypeH1, func(stats *TransportDelayStats) {
		published = append(published, stats)
	})
	now := delay.startTime
	micros := int64(0)
	delay.nowForTest = func() time.Time { return now }
	delay.clockForTest = func() int64 { return micros }
	return delay, &now, &micros, &published
}

func testTransportDelaySettings() *TransportDelaySettings {
	settings := DefaultTransportDelaySettings()
	settings.Warmup = 0
	settings.MinDelaySampleCount = 1
	return settings
}

func TestTransportDelayHeaderRoundTrip(t *testing.T) {
	header := newTransportDelayHeader(0xfffffff0, 1234, 1)
	sendStart, previousWrite, echo := parseTransportDelayHeader(header[:])
	if sendStart != 0xfffffff0 || previousWrite != 1234 || echo != 1 {
		t.Fatalf("round trip = %d %d %d", sendStart, previousWrite, echo)
	}
}

func TestTransportDelayWriteLaneNeverRepeatsSendStart(t *testing.T) {
	delay, _, _, _ := testTransportDelay(testTransportDelaySettings())
	lane := delay.newWriteLane()
	first := lane.beginWrite()
	lane.endWrite(first, 100, nil)
	second := lane.beginWrite()
	firstStart, _, _ := parseTransportDelayHeader(first.header[:])
	secondStart, secondPrevious, _ := parseTransportDelayHeader(second.header[:])
	if firstStart == secondStart {
		t.Fatalf("second write repeated send start %d", firstStart)
	}
	// the frozen clock makes the first write take no time; it still reports
	// a duration of at least 1, since 0 means none
	if secondPrevious != 1 {
		t.Fatalf("previous write = %d, expected 1", secondPrevious)
	}
}

func TestTransportDelayWriteLaneFailedWriteReportsNoDuration(t *testing.T) {
	delay, now, _, _ := testTransportDelay(testTransportDelaySettings())
	lane := delay.newWriteLane()
	write := lane.beginWrite()
	*now = now.Add(5 * time.Millisecond)
	lane.endWrite(write, 100, errTransportDelayTest)
	next := lane.beginWrite()
	if _, previous, _ := parseTransportDelayHeader(next.header[:]); previous != 0 {
		t.Fatalf("previous after a failed write = %d, expected 0", previous)
	}
}

var errTransportDelayTest = &HTTPUpgradeError{Reason: "test"}

// The frames of one physical write share a start stamp. The next write's
// reported duration closes the group with the group's bytes.
func TestTransportDelayReadLanePairsWriteGroups(t *testing.T) {
	settings := testTransportDelaySettings()
	// 8 ms blocked in a 1 s interval
	settings.MinBusyFraction = 0.005
	delay, now, _, published := testTransportDelay(settings)
	lane := delay.newReadLane(transportDelayLaneOrdered)

	groupA := newTransportDelayHeader(1000, 0, 0)
	lane.receive(groupA[:], 4000)
	lane.receive(groupA[:], 4000)
	// the next write reports the previous one took 8 ms
	groupB := newTransportDelayHeader(9500, 8000, 0)
	lane.receive(groupB[:], 100)

	*now = now.Add(settings.Interval)
	lane.receive(newTransportDelayHeaderSlice(20000, 0, 0), 100)
	if len(*published) != 1 {
		t.Fatalf("published %d intervals, expected 1", len(*published))
	}
	stats := (*published)[0]
	// 8000 bytes over 8 ms is 8 Mbit/s
	if stats.ReceiveBitsPerSecond != 8_000_000 {
		t.Fatalf("receive rate = %d, expected 8000000", stats.ReceiveBitsPerSecond)
	}
	if stats.ReceiveByteCount != 8100 {
		t.Fatalf("receive bytes = %d, expected 8100", stats.ReceiveByteCount)
	}
}

func newTransportDelayHeaderSlice(sendStart uint32, previousWrite uint32, echo uint32) []byte {
	header := newTransportDelayHeader(sendStart, previousWrite, echo)
	return header[:]
}

// A lossy lane gives delay samples only: a reported duration may describe a
// write whose frames were lost.
func TestTransportDelayUnreliableLaneSkipsDurations(t *testing.T) {
	settings := testTransportDelaySettings()
	settings.MinBusyFraction = 0.01
	delay, now, _, published := testTransportDelay(settings)
	lane := delay.newReadLane(transportDelayLaneUnreliable)
	lane.receive(newTransportDelayHeaderSlice(1000, 0, 0), 4000)
	lane.receive(newTransportDelayHeaderSlice(9500, 8000, 0), 100)
	*now = now.Add(settings.Interval)
	lane.receive(newTransportDelayHeaderSlice(20000, 0, 0), 100)
	if (*published)[0].ReceiveBitsPerSecond != 0 {
		t.Fatalf("unreliable lane produced a rate %d", (*published)[0].ReceiveBitsPerSecond)
	}
}

// An unordered lane accepts a pairing only when the new group starts after
// the open one by at least the reported duration.
func TestTransportDelayUnorderedLaneRejectsReorderedPairing(t *testing.T) {
	settings := testTransportDelaySettings()
	settings.MinBusyFraction = 0.01
	delay, now, _, published := testTransportDelay(settings)
	lane := delay.newReadLane(transportDelayLaneUnordered)
	lane.receive(newTransportDelayHeaderSlice(10000, 0, 0), 4000)
	// starts only 2 ms later yet claims an 8 ms previous write: reordered
	lane.receive(newTransportDelayHeaderSlice(12000, 8000, 0), 100)
	*now = now.Add(settings.Interval)
	lane.receive(newTransportDelayHeaderSlice(40000, 0, 0), 100)
	if (*published)[0].ReceiveBitsPerSecond != 0 {
		t.Fatalf("reordered pairing produced a rate %d", (*published)[0].ReceiveBitsPerSecond)
	}
}

// The down standing queue is the interval's minimum of one-way delay above the
// running minimum. The constant clock offset between the peers cancels.
func TestTransportDelayStandingQueueIsTheIntervalMinimum(t *testing.T) {
	delay, now, micros, published := testTransportDelay(testTransportDelaySettings())
	lane := delay.newReadLane(transportDelayLaneOrdered)
	const offset = 7_000_000
	send := func(sendStart uint32, oneWay int64) {
		*micros = int64(sendStart) + oneWay + offset
		lane.receive(newTransportDelayHeaderSlice(sendStart, 0, 0), 100)
	}
	// a base of 30 ms establishes the minimum
	send(1_000_000, 30_000)
	*now = now.Add(time.Second)
	// the next interval queues 200, 150 and 400 ms above the base
	send(2_000_000, 230_000)
	send(2_100_000, 180_000)
	send(2_200_000, 430_000)
	*now = now.Add(time.Second)
	send(3_000_000, 30_000)
	if len(*published) != 2 {
		t.Fatalf("published %d intervals, expected 2", len(*published))
	}
	queued := (*published)[1]
	if !queued.ReceiveQueueDelayValid || queued.ReceiveQueueDelay != 150*time.Millisecond {
		t.Fatalf("receive queue = %v (valid %v), expected 150ms", queued.ReceiveQueueDelay, queued.ReceiveQueueDelayValid)
	}
}

// The peer echoes the one-way delay of our writes; the up standing queue is
// measured from the echo with its own base. Repeated echoes count once.
func TestTransportDelayEchoMeasuresTheUpDirection(t *testing.T) {
	delay, now, _, published := testTransportDelay(testTransportDelaySettings())
	lane := delay.newReadLane(transportDelayLaneOrdered)
	echo := func(sendStart uint32, oneWay uint32) {
		lane.receive(newTransportDelayHeaderSlice(sendStart, 0, oneWay), 100)
	}
	echo(1, 50_000)
	*now = now.Add(time.Second)
	echo(2, 300_000)
	echo(3, 300_000)
	echo(4, 250_000)
	*now = now.Add(time.Second)
	echo(5, 50_000)
	queued := (*published)[1]
	if !queued.SendQueueDelayValid || queued.SendQueueDelay != 200*time.Millisecond {
		t.Fatalf("send queue = %v (valid %v), expected 200ms", queued.SendQueueDelay, queued.SendQueueDelayValid)
	}
}

// Our reader echoes the peer's one-way delay; a long local handoff censors it.
func TestTransportDelayReceiverLimitedCensorsTheEcho(t *testing.T) {
	delay, _, micros, _ := testTransportDelay(testTransportDelaySettings())
	lane := delay.newReadLane(transportDelayLaneOrdered)
	*micros = 5000
	lane.receive(newTransportDelayHeaderSlice(1000, 0, 0), 100)
	if echo := delay.echo.Load(); echo != 4000 {
		t.Fatalf("echo = %d, expected 4000", echo)
	}
	lane.noteHandoff(50 * time.Millisecond)
	*micros = 9000
	lane.receive(newTransportDelayHeaderSlice(2000, 0, 0), 100)
	if echo := delay.echo.Load(); echo != 0 {
		t.Fatalf("echo after a long handoff = %d, expected 0", echo)
	}
	header := delay.newWriteLane().beginWrite().header
	if _, _, wire := parseTransportDelayHeader(header[:]); wire != 0 {
		t.Fatalf("stamped echo = %d, expected 0", wire)
	}
}

// The 32-bit send clock wraps every 71.6 minutes; serial arithmetic keeps the
// one-way delay continuous across the wrap.
func TestTransportDelaySendClockWrap(t *testing.T) {
	delay, now, micros, published := testTransportDelay(testTransportDelaySettings())
	lane := delay.newReadLane(transportDelayLaneOrdered)
	send := func(sendStart uint32, arrival int64) {
		*micros = arrival
		lane.receive(newTransportDelayHeaderSlice(sendStart, 0, 0), 100)
	}
	send(0xffff0000, 1_000_000)
	*now = now.Add(time.Second)
	// 0x20000 microseconds later on the peer, and 0x20000 later here
	send(0x00010000, 1_000_000+0x20000)
	*now = now.Add(time.Second)
	send(0x00020000, 1_000_000+0x30000)
	queued := (*published)[1]
	if !queued.ReceiveQueueDelayValid || queued.ReceiveQueueDelay != 0 {
		t.Fatalf("queue across the wrap = %v (valid %v), expected 0", queued.ReceiveQueueDelay, queued.ReceiveQueueDelayValid)
	}
}

// A throughput sample needs the writer blocked for the busy share of the
// interval; a stall alone, such as a GC pause, gives none.
func TestTransportDelayBusyGate(t *testing.T) {
	settings := testTransportDelaySettings()
	delay, now, _, published := testTransportDelay(settings)
	lane := delay.newWriteLane()
	write := func(byteCount int, duration time.Duration) {
		w := lane.beginWrite()
		*now = now.Add(duration)
		lane.endWrite(w, byteCount, nil)
	}
	// one 30 ms stall in an otherwise idle interval
	write(1000, 30*time.Millisecond)
	*now = now.Add(time.Second)
	// a saturated interval: 16 KiB writes blocked 100 ms each
	for range 9 {
		write(16*1024, 100*time.Millisecond)
	}
	*now = now.Add(200 * time.Millisecond)
	write(100, 0)
	if len(*published) != 2 {
		t.Fatalf("published %d intervals, expected 2", len(*published))
	}
	if stall := (*published)[0]; stall.SendBitsPerSecond != 0 {
		t.Fatalf("stall produced a rate %d", stall.SendBitsPerSecond)
	}
	saturated := (*published)[1]
	expected := int64(8 * 16 * 1024 / 0.1)
	if saturated.SendBitsPerSecond < expected*99/100 || expected*101/100 < saturated.SendBitsPerSecond {
		t.Fatalf("saturated rate = %d, expected about %d", saturated.SendBitsPerSecond, expected)
	}
}

// Slow start is excluded from throughput samples.
func TestTransportDelayWarmupSuppressesThroughput(t *testing.T) {
	settings := testTransportDelaySettings()
	settings.Warmup = 2 * time.Second
	delay, now, _, published := testTransportDelay(settings)
	lane := delay.newWriteLane()
	for range 9 {
		w := lane.beginWrite()
		*now = now.Add(100 * time.Millisecond)
		lane.endWrite(w, 16*1024, nil)
	}
	*now = now.Add(time.Second)
	lane.endWrite(lane.beginWrite(), 100, nil)
	if (*published)[0].SendBitsPerSecond != 0 {
		t.Fatalf("warmup produced a rate %d", (*published)[0].SendBitsPerSecond)
	}
}

// A stamp-and-echo peer echoes but keeps no measurement state.
func TestTransportDelayStampOnlyPeer(t *testing.T) {
	delay := newTransportDelay(nil, NewId(), TransportTypeH1, nil)
	micros := int64(9000)
	delay.clockForTest = func() int64 { return micros }
	lane := delay.newReadLane(transportDelayLaneOrdered)
	lane.receive(newTransportDelayHeaderSlice(1000, 0, 0), 100)
	if echo := delay.echo.Load(); echo != 8000 {
		t.Fatalf("echo = %d, expected 8000", echo)
	}
	if delay.interval.active {
		t.Fatalf("a stamp-only peer accumulated an interval")
	}
}

// The base keeps per-bucket minimums; a rise is learned once old buckets age.
func TestTransportDelayBaseFilterAges(t *testing.T) {
	var base transportDelayBaseFilter
	base.init(3, time.Minute)
	start := time.Now()
	base.add(100, start)
	base.add(500, start.Add(time.Minute))
	base.add(500, start.Add(2*time.Minute))
	if base.min() != 100 {
		t.Fatalf("base = %d, expected 100", base.min())
	}
	base.add(500, start.Add(3*time.Minute))
	if base.min() != 500 {
		t.Fatalf("base after aging = %d, expected 500", base.min())
	}
}

// After a device suspend the transport clock includes the sleep, so the peer's
// one-way delay for our writes does not jump by the suspended time.
func TestTransportDelayClockCountsSleep(t *testing.T) {
	sleepNanos := int64(1_000_000_000)
	clock := newTransportDelayClockWithSource(func() (int64, bool) {
		return sleepNanos, true
	})
	origin := clock.origin
	// one second awake, then a 30 second suspend that Go's monotonic clock
	// does not see but the wall and sleep-inclusive clocks do
	sleepNanos += int64(31 * time.Second)
	now := origin.Add(time.Second).Round(0).Add(30 * time.Second)
	micros := clock.micros(now)
	if micros != int64(31*time.Second/time.Microsecond) {
		t.Fatalf("clock after suspend = %d us, expected %d", micros, int64(31*time.Second/time.Microsecond))
	}
}
