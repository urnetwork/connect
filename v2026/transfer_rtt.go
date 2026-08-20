package connect

import (
	"fmt"
	"sync"
	"time"

	"container/heap"

	"github.com/urnetwork/connect/v2026/protocol"
)

type rttWindowItem struct {
	sendTime    time.Time
	receiveTime time.Time
	rtt         time.Duration

	heapIndex int
}

func newRttWindowItem(sendTime time.Time, receiveTime time.Time) *rttWindowItem {
	return &rttWindowItem{
		sendTime:    sendTime,
		receiveTime: receiveTime,
		rtt:         receiveTime.Sub(sendTime),
	}
}

type RttWindow struct {
	log           Logger
	windowTimeout time.Duration
	rttScale      float32
	// minScaledRtt is the COLD floor: used when the window holds no samples
	// (nothing acked yet, or a long quiet gap aged everything out) — no
	// evidence, so resend conservatively.
	minScaledRtt time.Duration
	// rttMinScaledRtt is the floor once samples exist: the measured path rtt
	// (scaled) governs, bounded below by this, so a lost packet on a fast
	// path retries in hundreds of milliseconds instead of the cold floor.
	// The per-item exponential backoff (see the resend loop) bounds the
	// duplicate cost of a too-eager first retry.
	rttMinScaledRtt time.Duration
	maxScaledRtt    time.Duration

	stateLock       sync.Mutex
	window          []*rttWindowItem
	windowTailIndex int
	windowHeadIndex int

	rtts *rttHeap
}

func NewRttWindow(
	log Logger,
	windowSize int,
	windowTimeout time.Duration,
	rttScale float32,
	minScaledRtt time.Duration,
	rttMinScaledRtt time.Duration,
	maxScaledRtt time.Duration,
) *RttWindow {
	if windowSize == 0 {
		panic(fmt.Errorf("Window size must non-zero: %d", windowSize))
	}
	if rttMinScaledRtt <= 0 {
		// no rtt floor configured: sampled paths floor at the cold value
		// (the historical flat-floor behavior)
		rttMinScaledRtt = minScaledRtt
	}
	window := make([]*rttWindowItem, windowSize)

	return &RttWindow{
		log:             loggerOrDefault(log),
		windowTimeout:   windowTimeout,
		rttScale:        rttScale,
		minScaledRtt:    minScaledRtt,
		rttMinScaledRtt: rttMinScaledRtt,
		maxScaledRtt:    maxScaledRtt,
		window:          window,
		windowTailIndex: 0,
		windowHeadIndex: 0,
		rtts:            newRttHeap(),
	}
}

// Removes expired samples while stateLock is held.
func (self *RttWindow) coalesceWithLock(windowTime time.Time) {
	windowStartTime := windowTime.Add(-self.windowTimeout)
	for self.windowTailIndex != self.windowHeadIndex {
		item := self.window[self.windowTailIndex]
		if !item.receiveTime.Before(windowStartTime) {
			break
		}
		self.rtts.Remove(item)
		self.window[self.windowTailIndex] = nil
		self.windowTailIndex = (self.windowTailIndex + 1) % len(self.window)
	}
}

func (self *RttWindow) OpenTag() *protocol.Tag {
	return self.openTag(time.Now())
}

func (self *RttWindow) openTag(sendTime time.Time) *protocol.Tag {
	// sendTime
	return &protocol.Tag{
		SendTime: uint64(sendTime.UnixMilli()),
	}
}

func (self *RttWindow) CloseTag(tag *protocol.Tag) {
	self.closeSendTime(tag.SendTime, time.Now())
}

func (self *RttWindow) closeTag(tag *protocol.Tag, receiveTime time.Time) {
	self.closeSendTime(tag.SendTime, receiveTime)
}

// CloseSendTime is the allocation-free ACK hot-path form. ACK windows retain
// Tag's scalar wire value instead of copying a generated protobuf message
// (whose internal MessageState must not be copied).
func (self *RttWindow) CloseSendTime(sendTimeUnixMilli uint64) {
	self.closeSendTime(sendTimeUnixMilli, time.Now())
}

func (self *RttWindow) closeSendTime(sendTimeUnixMilli uint64, receiveTime time.Time) {
	sendTime := time.UnixMilli(int64(sendTimeUnixMilli))
	if receiveTime.Before(sendTime) {
		// ignore
		return
	}

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.coalesceWithLock(receiveTime)

	item := newRttWindowItem(
		sendTime,
		receiveTime,
	)
	self.rtts.Add(item)

	if replaceItem := self.window[self.windowHeadIndex]; replaceItem != nil {
		self.rtts.Remove(replaceItem)
	}
	self.window[self.windowHeadIndex] = item
	self.windowHeadIndex = (self.windowHeadIndex + 1) % len(self.window)
	if self.windowTailIndex == self.windowHeadIndex {
		self.windowTailIndex = (self.windowTailIndex + 1) % len(self.window)
	}
}

// clamp(mean rtt of window * scale, floor, overall max), where the floor is
// rttMinScaledRtt once samples exist and the conservative minScaledRtt when
// the window is empty (cold start / long quiet gap).
func (self *RttWindow) ScaledRtt() time.Duration {
	return self.scaledRtt(time.Now())
}

func (self *RttWindow) scaledRtt(sendTime time.Time) time.Duration {
	self.stateLock.Lock()
	self.coalesceWithLock(sendTime)

	useRtt := self.rtts.MeanRtt()
	floor := self.rttMinScaledRtt
	if useRtt == 0 {
		// no samples: no evidence to be aggressive on
		floor = self.minScaledRtt
	}
	scaledRtt := min(
		max(
			time.Duration(float32(useRtt/time.Millisecond)*self.rttScale)*time.Millisecond,
			floor,
		),
		self.maxScaledRtt,
	)
	self.stateLock.Unlock()
	// guard the V(2) diagnostic: this runs per packet (resend timing), and the
	// disabled-level call would still box the Duration arg into []any and build
	// the variadic slice on the heap. the guard keeps the hot path allocation-free.
	if self.log.V(2).Enabled() {
		self.log.Infof("[rtt]scaled=%dms\n", scaledRtt/time.Millisecond)
	}
	return scaledRtt
}

// Returns a bounded minimum-path RTT for one receiver-paced recovery probe.
// Queue-inflated mean RTT remains the ordinary resend timer; using the minimum
// here prevents one deep serialization queue from turning a tail probe into the
// same multi-second RTO it is meant to precede. Callers bound duplicate cost to
// one probe per item.
func (self *RttWindow) ProbeRtt() time.Duration {
	return self.probeRtt(time.Now())
}

func (self *RttWindow) probeRtt(probeTime time.Time) time.Duration {
	self.stateLock.Lock()
	self.coalesceWithLock(probeTime)

	useRtt := self.rtts.MinRtt()
	floor := self.rttMinScaledRtt
	if useRtt == 0 {
		floor = self.minScaledRtt
	}
	probeRtt := min(
		max(
			time.Duration(float32(useRtt/time.Millisecond)*self.rttScale)*time.Millisecond,
			floor,
		),
		self.maxScaledRtt,
	)
	self.stateLock.Unlock()

	if self.log.V(2).Enabled() {
		self.log.Infof("[rtt]probe=%dms\n", probeRtt/time.Millisecond)
	}
	return probeRtt
}

type rttHeap struct {
	items  []*rttWindowItem
	netRtt time.Duration
}

// `heap` is a min heap
func newRttHeap() *rttHeap {
	h := &rttHeap{
		items:  []*rttWindowItem{},
		netRtt: time.Duration(0),
	}
	heap.Init(h)
	return h
}

func (self *rttHeap) Add(item *rttWindowItem) {
	heap.Push(self, item)
	self.netRtt += item.rtt
}

func (self *rttHeap) Remove(item *rttWindowItem) {
	heap.Remove(self, item.heapIndex)
	self.netRtt -= item.rtt
}

func (self *rttHeap) MinRtt() time.Duration {
	if len(self.items) == 0 {
		return time.Duration(0)
	}
	return self.items[0].rtt
}

func (self *rttHeap) MeanRtt() time.Duration {
	n := len(self.items)
	if n == 0 {
		return 0
	}
	return self.netRtt / time.Duration(n)
}

// `heap.Interface`

func (self *rttHeap) Len() int {
	return len(self.items)
}

func (self *rttHeap) Less(i, j int) bool {
	return self.items[i].rtt < self.items[j].rtt
}

func (self *rttHeap) Swap(i, j int) {
	a := self.items[i]
	b := self.items[j]
	b.heapIndex = i
	self.items[i] = b
	a.heapIndex = j
	self.items[j] = a
}

func (self *rttHeap) Push(x any) {
	item := x.(*rttWindowItem)
	item.heapIndex = len(self.items)
	self.items = append(self.items, item)
}

func (self *rttHeap) Pop() any {
	n := len(self.items)
	item := self.items[n-1]
	self.items[n-1] = nil
	self.items = self.items[0 : n-1]
	return item
}
