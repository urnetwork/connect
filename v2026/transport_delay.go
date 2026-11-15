package connect

// transport_delay.go -- per-message transport delay measurement (DELAY.md).
//
// A negotiated connection carries a fixed 12-byte delay header before every
// message in both directions. Each writer stamps the start of the physical
// write that carries the message, the duration of its previous physical write,
// and an echo of the one-way delay of the peer's latest write. The reader
// groups the frames of one physical write by their identical start stamp,
// pairs each group with the next write's reported duration (delayed by one
// write), and measures the standing queue on the path from the one-way delay
// above its running minimum. A client publishes one TransportDelayStats per
// active interval; a server or a provider peer only stamps and echoes.

import (
	"encoding/binary"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// TransportDelayVersion is the delay header version this build speaks.
	TransportDelayVersion = 1

	// transportDelayHeaderByteCount is the v1 header: sendStartMicros,
	// previousWriteMicros and echoOneWayMicros, each a big-endian uint32.
	transportDelayHeaderByteCount = 12

	// TransportDelayH1Subprotocol offers and accepts the v1 header on h1. It
	// rides Sec-WebSocket-Protocol on both the WebSocket and the h1+ upgrade,
	// so an old peer never selects it and the connection keeps the legacy wire.
	TransportDelayH1Subprotocol = "urnetwork-delay.1"

	// a write that waited at least this long blocked on the transport rather
	// than copying into a buffer; the default when settings leave it zero
	transportDelayMinBlockedWrite = 250 * time.Microsecond

	// a wall-clock gap this long between clock reads may hide a device
	// suspend, so the sleep offset is sampled again
	transportDelaySleepResampleGap = 50 * time.Millisecond
)

// TransportDelaySettings configures delay measurement on a connection. A nil
// *TransportDelaySettings, or a zero Version, offers nothing and keeps the
// legacy wire. The zero values of the other fields take the defaults below.
type TransportDelaySettings struct {
	// Version is the delay header version to offer. 0 does not offer.
	Version int
	// Interval is the measurement interval; one TransportDelayStats is
	// published per active interval.
	Interval time.Duration
	// BlockedWriteMinDuration separates a write that waited on the transport
	// from one that copied into a buffer.
	BlockedWriteMinDuration time.Duration
	// MinBusyFraction is the share of an interval a writer must spend blocked
	// before the interval yields a throughput sample (BBR's app-limited rule).
	MinBusyFraction float64
	// Warmup suppresses throughput samples while a connection is in slow start.
	Warmup time.Duration
	// MinDelaySampleCount is the number of delay samples an interval needs
	// before it reports a standing queue.
	MinDelaySampleCount int
	// BaseHistoryCount and BaseHistoryInterval keep the running minimum
	// one-way delay as per-bucket minimums (LEDBAT's base delay history).
	BaseHistoryCount    int
	BaseHistoryInterval time.Duration
	// ReceiverLimitedThreshold censors a delay sample when the reader was
	// held up handing the previous message onward for longer than this.
	ReceiverLimitedThreshold time.Duration
}

func DefaultTransportDelaySettings() *TransportDelaySettings {
	return &TransportDelaySettings{
		Version:                  TransportDelayVersion,
		Interval:                 1 * time.Second,
		BlockedWriteMinDuration:  transportDelayMinBlockedWrite,
		MinBusyFraction:          0.5,
		Warmup:                   2 * time.Second,
		MinDelaySampleCount:      3,
		BaseHistoryCount:         10,
		BaseHistoryInterval:      1 * time.Minute,
		ReceiverLimitedThreshold: 5 * time.Millisecond,
	}
}

// offered reports whether these settings offer the delay header.
func (self *TransportDelaySettings) offered() bool {
	return self != nil && 0 < self.Version
}

// resolved fills the zero fields with the defaults.
func (self *TransportDelaySettings) resolved() *TransportDelaySettings {
	defaults := DefaultTransportDelaySettings()
	if self == nil {
		return defaults
	}
	settings := *self
	if settings.Interval <= 0 {
		settings.Interval = defaults.Interval
	}
	if settings.BlockedWriteMinDuration <= 0 {
		settings.BlockedWriteMinDuration = defaults.BlockedWriteMinDuration
	}
	if settings.MinBusyFraction <= 0 {
		settings.MinBusyFraction = defaults.MinBusyFraction
	}
	if settings.Warmup < 0 {
		settings.Warmup = 0
	}
	if settings.MinDelaySampleCount <= 0 {
		settings.MinDelaySampleCount = defaults.MinDelaySampleCount
	}
	if settings.BaseHistoryCount <= 0 {
		settings.BaseHistoryCount = defaults.BaseHistoryCount
	}
	if settings.BaseHistoryInterval <= 0 {
		settings.BaseHistoryInterval = defaults.BaseHistoryInterval
	}
	if settings.ReceiverLimitedThreshold <= 0 {
		settings.ReceiverLimitedThreshold = defaults.ReceiverLimitedThreshold
	}
	return &settings
}

// TransportDelayStats is one measurement interval of one connection.
type TransportDelayStats struct {
	ClientId      Id
	ConnectionId  Id
	TransportType TransportType
	StartTime     time.Time
	// EndTime is the last activity in the interval, so a consumer can age the
	// sample out even when the interval closes late.
	EndTime time.Time

	SendByteCount    ByteCount
	ReceiveByteCount ByteCount

	// Network-limited throughput (DELAY.md §4.1). 0 means the interval had no
	// limited throughput: the writer was not busy, the connection was warming
	// up, or the carrier cannot block.
	SendBitsPerSecond    int64
	ReceiveBitsPerSecond int64

	// Standing queue: the minimum within the interval of the one-way delay
	// above its running minimum (DELAY.md §4.2).
	SendQueueDelay         time.Duration
	SendQueueDelayValid    bool
	ReceiveQueueDelay      time.Duration
	ReceiveQueueDelayValid bool
}

type TransportDelayStatsFunction = func(stats *TransportDelayStats)

// transportDelayHeader is the wire form of the v1 header.
type transportDelayHeader [transportDelayHeaderByteCount]byte

func newTransportDelayHeader(sendStartMicros uint32, previousWriteMicros uint32, echoOneWayMicros uint32) transportDelayHeader {
	var header transportDelayHeader
	binary.BigEndian.PutUint32(header[0:4], sendStartMicros)
	binary.BigEndian.PutUint32(header[4:8], previousWriteMicros)
	binary.BigEndian.PutUint32(header[8:12], echoOneWayMicros)
	return header
}

func parseTransportDelayHeader(header []byte) (sendStartMicros uint32, previousWriteMicros uint32, echoOneWayMicros uint32) {
	sendStartMicros = binary.BigEndian.Uint32(header[0:4])
	previousWriteMicros = binary.BigEndian.Uint32(header[4:8])
	echoOneWayMicros = binary.BigEndian.Uint32(header[8:12])
	return
}

// transportDelayClock reads microseconds since the connection started on a
// monotonic clock that counts device sleep (DELAY.md §5.2). Go's monotonic
// clock stops while a phone sleeps, so the clock adds the offset between the
// platform's sleep-inclusive clock and Go's monotonic clock, sampled again
// only after a wall-clock gap that could hide a suspend.
type transportDelayClock struct {
	sleepInclusiveNanos func() (int64, bool)

	stateLock sync.Mutex
	origin    time.Time
	// sleep-inclusive nanos at origin; valid when hasSleepClock
	originSleepNanos int64
	hasSleepClock    bool
	offset           time.Duration
	lastWall         time.Time
}

func newTransportDelayClock() *transportDelayClock {
	return newTransportDelayClockWithSource(transportDelaySleepInclusiveNanos)
}

func newTransportDelayClockWithSource(sleepInclusiveNanos func() (int64, bool)) *transportDelayClock {
	now := time.Now()
	clock := &transportDelayClock{
		sleepInclusiveNanos: sleepInclusiveNanos,
		origin:              now,
		lastWall:            now,
	}
	if sleepInclusiveNanos != nil {
		clock.originSleepNanos, clock.hasSleepClock = sleepInclusiveNanos()
	}
	return clock
}

// nowMicros returns the connection's transport clock.
func (self *transportDelayClock) nowMicros() int64 {
	return self.micros(time.Now())
}

func (self *transportDelayClock) micros(now time.Time) int64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	elapsed := now.Sub(self.origin)
	if self.hasSleepClock {
		wallGap := now.Round(0).Sub(self.lastWall.Round(0))
		if wallGap < 0 || transportDelaySleepResampleGap <= wallGap {
			if sleepNanos, ok := self.sleepInclusiveNanos(); ok {
				self.offset = max(0, time.Duration(sleepNanos-self.originSleepNanos)-elapsed)
			}
		}
	}
	self.lastWall = now
	return int64((elapsed + self.offset) / time.Microsecond)
}

// transportDelay is the delay state of one negotiated connection, shared by
// its write and read lanes.
type transportDelay struct {
	settings      *TransportDelaySettings
	clock         *transportDelayClock
	clientId      Id
	connectionId  Id
	transportType TransportType
	// nil stamps and echoes only: the server and a provider peer measure
	// nothing themselves
	publish   TransportDelayStatsFunction
	startTime time.Time

	// the one-way delay of the peer's latest write group, as the local writers
	// echo it. 0 is no sample.
	echo atomic.Uint32

	stateLock sync.Mutex
	// the peer's send clock, unwrapped from 32 bits
	peerSendStart    int64
	hasPeerSendStart bool
	// the echoed up-direction delays, unwrapped from 32 bits
	lastEcho      uint32
	echoUnwrapped int64
	hasEcho       bool
	downBase      transportDelayBaseFilter
	upBase        transportDelayBaseFilter
	interval      transportDelayInterval
	nowForTest    func() time.Time
	clockForTest  func() int64
}

// newTransportDelay starts delay state for a connection. publish nil makes a
// stamp-and-echo peer that keeps no measurement state.
func newTransportDelay(
	settings *TransportDelaySettings,
	clientId Id,
	transportType TransportType,
	publish TransportDelayStatsFunction,
) *transportDelay {
	now := time.Now()
	resolved := settings.resolved()
	delay := &transportDelay{
		settings:      resolved,
		clock:         newTransportDelayClock(),
		clientId:      clientId,
		connectionId:  NewId(),
		transportType: transportType,
		publish:       publish,
		startTime:     now,
	}
	delay.downBase.init(resolved.BaseHistoryCount, resolved.BaseHistoryInterval)
	delay.upBase.init(resolved.BaseHistoryCount, resolved.BaseHistoryInterval)
	return delay
}

func (self *transportDelay) now() time.Time {
	if self.nowForTest != nil {
		return self.nowForTest()
	}
	return time.Now()
}

func (self *transportDelay) clockMicros() int64 {
	if self.clockForTest != nil {
		return self.clockForTest()
	}
	return self.clock.nowMicros()
}

// measuring reports whether this side measures and publishes.
func (self *transportDelay) measuring() bool {
	return self.publish != nil
}

// newWriteLane returns the write state of one independent writer.
func (self *transportDelay) newWriteLane() *transportDelayWriteLane {
	return &transportDelayWriteLane{delay: self}
}

// newUnreliableWriteLane returns the write state of a lossy lane (DATAGRAM,
// the SRTP fast lane). Its sends queue or drop rather than wait on congestion
// control, so its writes count bytes but never throughput samples (DELAY.md
// §4.1).
func (self *transportDelay) newUnreliableWriteLane() *transportDelayWriteLane {
	return &transportDelayWriteLane{delay: self, unreliable: true}
}

// newReadLane returns the read state of one independent reader. reliability
// selects which writes the lane may pair with a reported duration.
func (self *transportDelay) newReadLane(reliability transportDelayLaneReliability) *transportDelayReadLane {
	return &transportDelayReadLane{delay: self, reliability: reliability}
}

// outOfBandHeader stamps a message written outside the lanes, such as a close
// notice. It reports no previous write, so it never closes a duration sample.
func (self *transportDelay) outOfBandHeader() transportDelayHeader {
	return newTransportDelayHeader(uint32(self.clockMicros()), 0, self.echo.Load())
}

// transportDelayWriteLane stamps the physical writes of one writer. Writers
// that never overlap, such as a writer goroutine and its queue worker, may
// share a lane; the lock keeps the stamps ordered either way.
type transportDelayWriteLane struct {
	delay *transportDelay
	// a lossy lane: bytes only, no blocked-write throughput
	unreliable bool

	stateLock           sync.Mutex
	previousWriteMicros uint32
	lastSendStart       uint32
	hasLastSendStart    bool
}

// transportDelayWrite is one physical write in progress.
type transportDelayWrite struct {
	header    transportDelayHeader
	startTime time.Time
}

// beginWrite stamps the header every message of one physical write carries.
func (self *transportDelayWriteLane) beginWrite() transportDelayWrite {
	sendStart := uint32(self.delay.clockMicros())
	startTime := self.delay.now()
	self.stateLock.Lock()
	// a new write never repeats the previous start, so the reader can group
	// the frames of one write by equality
	if self.hasLastSendStart && sendStart == self.lastSendStart {
		sendStart += 1
	}
	self.lastSendStart = sendStart
	self.hasLastSendStart = true
	previousWriteMicros := self.previousWriteMicros
	self.stateLock.Unlock()
	return transportDelayWrite{
		header:    newTransportDelayHeader(sendStart, previousWriteMicros, self.delay.echo.Load()),
		startTime: startTime,
	}
}

// endWrite records a finished physical write of byteCount wire bytes.
func (self *transportDelayWriteLane) endWrite(write transportDelayWrite, byteCount int, err error) {
	endTime := self.delay.now()
	duration := endTime.Sub(write.startTime)
	self.stateLock.Lock()
	if err != nil {
		// a failed write reports no duration; the next write cannot pair with it
		self.previousWriteMicros = 0
	} else {
		self.previousWriteMicros = uint32(min(math.MaxUint32, max(1, duration.Microseconds())))
	}
	self.stateLock.Unlock()
	if err == nil {
		self.delay.recordWrite(byteCount, duration, endTime, !self.unreliable)
	}
}

type transportDelayLaneReliability int

const (
	// a reliable, ordered lane (a TCP or QUIC stream): each new write group
	// closes the previous one with the reported duration
	transportDelayLaneOrdered transportDelayLaneReliability = iota
	// reliable but unordered (an SCTP data channel): a pairing is accepted only
	// when the new group starts after the old one by at least the duration
	transportDelayLaneUnordered
	// lossy (DATAGRAM, the SRTP fast lane): delay samples only
	transportDelayLaneUnreliable
)

// transportDelayReadLane reads the headers of one independent reader.
type transportDelayReadLane struct {
	delay       *transportDelay
	reliability transportDelayLaneReliability

	stateLock      sync.Mutex
	groupOpen      bool
	groupSendStart uint32
	groupByteCount int
	lastHandoff    time.Duration
}

// noteHandoff records how long the reader was held handing the previous
// message onward. A long handoff censors the next delay sample, because the
// next message waited in the receive buffer for a local reason.
func (self *transportDelayReadLane) noteHandoff(handoff time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.lastHandoff = max(self.lastHandoff, handoff)
}

// receive accounts one message of byteCount wire bytes and its header.
func (self *transportDelayReadLane) receive(header []byte, byteCount int) {
	if len(header) < transportDelayHeaderByteCount {
		return
	}
	sendStart, previousWrite, echo := parseTransportDelayHeader(header)
	arrivalMicros := self.delay.clockMicros()

	self.stateLock.Lock()
	receiverLimited := self.delay.settings.ReceiverLimitedThreshold < self.lastHandoff
	self.lastHandoff = 0
	newGroup := !self.groupOpen || sendStart != self.groupSendStart
	closedByteCount := 0
	closedMicros := uint32(0)
	if newGroup {
		if self.groupOpen && previousWrite != 0 {
			switch self.reliability {
			case transportDelayLaneOrdered:
				closedByteCount, closedMicros = self.groupByteCount, previousWrite
			case transportDelayLaneUnordered:
				// int32 serial arithmetic; a reordered older group is rejected
				if gap := int32(sendStart - self.groupSendStart); 0 < gap && previousWrite <= uint32(gap) {
					closedByteCount, closedMicros = self.groupByteCount, previousWrite
				}
			}
		}
		self.groupOpen = true
		self.groupSendStart = sendStart
		self.groupByteCount = byteCount
	} else {
		self.groupByteCount += byteCount
	}
	self.stateLock.Unlock()

	self.delay.recordReceive(newGroup, sendStart, arrivalMicros, echo, byteCount, closedByteCount, closedMicros, receiverLimited)
}

// addReceiveByteCount adds bytes to the message the lane last received, for a
// reader that learns the message length only after reading its header.
func (self *transportDelayReadLane) addReceiveByteCount(byteCount int) {
	self.stateLock.Lock()
	self.groupByteCount += byteCount
	self.stateLock.Unlock()
	if !self.delay.measuring() {
		return
	}
	self.delay.stateLock.Lock()
	self.delay.interval.receiveByteCount += ByteCount(byteCount)
	self.delay.stateLock.Unlock()
}

// recordReceive applies one received message to the echo and, when measuring,
// to the interval.
func (self *transportDelay) recordReceive(
	newGroup bool,
	sendStart uint32,
	arrivalMicros int64,
	echo uint32,
	byteCount int,
	closedByteCount int,
	closedMicros uint32,
	receiverLimited bool,
) {
	if newGroup {
		// the first frame of a write group carries its one-way delay. The peer
		// learns its up direction from this echo (DELAY.md §4.2).
		if receiverLimited {
			self.echo.Store(0)
		} else {
			oneWay := uint32(arrivalMicros) - sendStart
			if oneWay == 0 {
				// 0 means no sample on the wire
				oneWay = 1
			}
			self.echo.Store(oneWay)
		}
	}
	if !self.measuring() {
		return
	}

	now := self.now()
	var stats *TransportDelayStats
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		stats = self.rollIntervalWithLock(now)
		self.interval.activeWithLock(now)
		self.interval.receiveByteCount += ByteCount(byteCount)

		if newGroup {
			if self.hasPeerSendStart {
				self.peerSendStart += int64(int32(sendStart - uint32(self.peerSendStart)))
			} else {
				self.peerSendStart = int64(sendStart)
				self.hasPeerSendStart = true
			}
			if !receiverLimited {
				oneWay := arrivalMicros - self.peerSendStart
				self.downBase.add(oneWay, now)
				self.interval.downQueue.add(oneWay - self.downBase.min())
			}
		}
		if echo != 0 && (!self.hasEcho || echo != self.lastEcho) {
			if self.hasEcho {
				self.echoUnwrapped += int64(int32(echo - self.lastEcho))
			} else {
				self.echoUnwrapped = int64(echo)
				self.hasEcho = true
			}
			self.lastEcho = echo
			self.upBase.add(self.echoUnwrapped, now)
			self.interval.upQueue.add(self.echoUnwrapped - self.upBase.min())
		}
		if closedMicros != 0 && self.settings.Warmup <= now.Sub(self.startTime) {
			duration := time.Duration(closedMicros) * time.Microsecond
			if self.settings.BlockedWriteMinDuration <= duration {
				self.interval.receiveBlockedByteCount += ByteCount(closedByteCount)
				self.interval.receiveBlockedDuration += duration
			}
		}
	}()
	self.publishStats(stats)
}

// recordWrite applies one finished physical write to the interval. Only a lane
// whose writes wait on the transport (limiting) feeds the throughput sample.
func (self *transportDelay) recordWrite(byteCount int, duration time.Duration, endTime time.Time, limiting bool) {
	if !self.measuring() {
		return
	}
	var stats *TransportDelayStats
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		stats = self.rollIntervalWithLock(endTime)
		self.interval.activeWithLock(endTime)
		self.interval.sendByteCount += ByteCount(byteCount)
		if limiting && self.settings.Warmup <= endTime.Sub(self.startTime) && self.settings.BlockedWriteMinDuration <= duration {
			self.interval.sendBlockedByteCount += ByteCount(byteCount)
			self.interval.sendBlockedDuration += duration
		}
	}()
	self.publishStats(stats)
}

// flush publishes the interval in progress, if it saw activity. Call when the
// connection ends.
func (self *transportDelay) flush() {
	if !self.measuring() {
		return
	}
	var stats *TransportDelayStats
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		stats = self.closeIntervalWithLock()
	}()
	self.publishStats(stats)
}

func (self *transportDelay) publishStats(stats *TransportDelayStats) {
	if stats != nil {
		HandleError(func() {
			self.publish(stats)
		})
	}
}

// rollIntervalWithLock closes the interval when now has passed its end.
func (self *transportDelay) rollIntervalWithLock(now time.Time) *TransportDelayStats {
	if !self.interval.active || now.Sub(self.interval.startTime) < self.settings.Interval {
		return nil
	}
	return self.closeIntervalWithLock()
}

func (self *transportDelay) closeIntervalWithLock() *TransportDelayStats {
	interval := &self.interval
	if !interval.active {
		return nil
	}
	defer interval.reset()

	elapsed := max(interval.endTime.Sub(interval.startTime), self.settings.Interval)
	busy := func(blocked time.Duration) bool {
		return self.settings.MinBusyFraction*float64(elapsed) <= float64(blocked)
	}
	bitsPerSecond := func(byteCount ByteCount, blocked time.Duration) int64 {
		if blocked <= 0 || !busy(blocked) {
			return 0
		}
		return max(1, int64(float64(8*byteCount)/blocked.Seconds()))
	}
	stats := &TransportDelayStats{
		ClientId:             self.clientId,
		ConnectionId:         self.connectionId,
		TransportType:        self.transportType,
		StartTime:            interval.startTime,
		EndTime:              interval.endTime,
		SendByteCount:        interval.sendByteCount,
		ReceiveByteCount:     interval.receiveByteCount,
		SendBitsPerSecond:    bitsPerSecond(interval.sendBlockedByteCount, interval.sendBlockedDuration),
		ReceiveBitsPerSecond: bitsPerSecond(interval.receiveBlockedByteCount, interval.receiveBlockedDuration),
	}
	if self.settings.MinDelaySampleCount <= interval.upQueue.count {
		stats.SendQueueDelay = time.Duration(max(0, interval.upQueue.minimum)) * time.Microsecond
		stats.SendQueueDelayValid = true
	}
	if self.settings.MinDelaySampleCount <= interval.downQueue.count {
		stats.ReceiveQueueDelay = time.Duration(max(0, interval.downQueue.minimum)) * time.Microsecond
		stats.ReceiveQueueDelayValid = true
	}
	return stats
}

// transportDelayInterval accumulates one measurement interval.
type transportDelayInterval struct {
	active    bool
	startTime time.Time
	endTime   time.Time

	sendByteCount           ByteCount
	sendBlockedByteCount    ByteCount
	sendBlockedDuration     time.Duration
	receiveByteCount        ByteCount
	receiveBlockedByteCount ByteCount
	receiveBlockedDuration  time.Duration

	downQueue transportDelayQueueMinimum
	upQueue   transportDelayQueueMinimum
}

func (self *transportDelayInterval) activeWithLock(now time.Time) {
	if !self.active {
		self.active = true
		self.startTime = now
	}
	self.endTime = now
}

func (self *transportDelayInterval) reset() {
	*self = transportDelayInterval{}
}

// transportDelayQueueMinimum is CoDel's standing queue: the minimum of the
// interval's samples, in microseconds.
type transportDelayQueueMinimum struct {
	count   int
	minimum int64
}

func (self *transportDelayQueueMinimum) add(queueMicros int64) {
	if self.count == 0 || queueMicros < self.minimum {
		self.minimum = queueMicros
	}
	self.count += 1
}

// transportDelayBaseFilter keeps the running minimum one-way delay as a
// minimum per bucket over the last bucketCount buckets (LEDBAT's base delay).
// A fall in the true base is learned at once; a rise only as old buckets age.
type transportDelayBaseFilter struct {
	bucketCount    int
	bucketDuration time.Duration
	minimums       []int64
	bucketStart    time.Time
}

func (self *transportDelayBaseFilter) init(bucketCount int, bucketDuration time.Duration) {
	self.bucketCount = max(1, bucketCount)
	self.bucketDuration = bucketDuration
	self.minimums = make([]int64, 0, self.bucketCount)
}

func (self *transportDelayBaseFilter) add(oneWayMicros int64, now time.Time) {
	if len(self.minimums) == 0 || self.bucketDuration <= now.Sub(self.bucketStart) {
		if len(self.minimums) == self.bucketCount {
			copy(self.minimums, self.minimums[1:])
			self.minimums = self.minimums[:len(self.minimums)-1]
		}
		self.minimums = append(self.minimums, oneWayMicros)
		self.bucketStart = now
		return
	}
	last := len(self.minimums) - 1
	self.minimums[last] = min(self.minimums[last], oneWayMicros)
}

func (self *transportDelayBaseFilter) min() int64 {
	base := int64(math.MaxInt64)
	for _, minimum := range self.minimums {
		base = min(base, minimum)
	}
	return base
}
