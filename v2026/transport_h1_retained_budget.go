// Carrier-local byte permission follows queued roots through writer return and
// the client's synchronous receive handoff. It is part of the existing graph
// claim, not an additional process budget or a pooled-root lifetime hook.
package connect

import (
	"context"
	"math"
	"reflect"
	"sync"
	"time"
)

// Keep the count queues intact. A normal pooled 4-KiB root spends one slot's
// byte allowance; larger valid frames spend several. Independent lanes keep
// ordinary data from spending the control or acknowledgement allowance.
const platformH1QueueSlotByteCount = ByteCount(4096 + MessagePoolMetaByteCount)

// Each lane has one immutable ceiling and lazily allocates a wakeup channel
// only under pressure. Reservations include queued and physical-writer-owned
// roots. Receive reservations also span the client's decode/Pack handoff.
type platformH1RetainedLane struct {
	stateLock     sync.Mutex
	byteLimit     ByteCount
	usedByteCount ByteCount
	notify        chan struct{}
}

// Nil preserves the unaccounted desktop/custom-carrier path. The byte count is
// the complete canonical backing capacity, including the pool metadata tail.
func (self *platformH1RetainedLane) tryReserve(byteCount ByteCount) bool {
	if self == nil {
		return true
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.tryReserveWithLock(byteCount)
}

// Both immediate and waiting admission use the same exact capacity check.
func (self *platformH1RetainedLane) tryReserveWithLock(byteCount ByteCount) bool {
	if byteCount < 0 || self.byteLimit < byteCount || self.byteLimit-byteCount < self.usedByteCount {
		return false
	}
	self.usedByteCount += byteCount
	return true
}

// Only a byte refusal subscribes. Reserve and subscribe share one lock, so a
// return cannot slip between failure and registration. Successful admission
// neither allocates a channel nor changes another waiter's notification.
func (self *platformH1RetainedLane) tryReserveOrSubscribe(byteCount ByteCount) (bool, <-chan struct{}) {
	if self == nil {
		return true, nil
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.tryReserveWithLock(byteCount) {
		return true, nil
	}
	if self.notify == nil {
		self.notify = make(chan struct{})
	}
	return false, self.notify
}

// Capacity is only an advisory route choice; actual admission reserves the
// complete root under the same lock before publishing it.
func (self *platformH1RetainedLane) available() bool {
	if self == nil {
		return true
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.usedByteCount < self.byteLimit
}

// Register before rechecking admission, so a return cannot be lost between
// observing a full lane and parking the existing owner on its notification.
func (self *platformH1RetainedLane) notifyChannel() <-chan struct{} {
	if self == nil {
		return nil
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.notify == nil {
		self.notify = make(chan struct{})
	}
	return self.notify
}

// Return permission only after the corresponding root has left this owner.
// Reservations for different shares are conservative permissions; this does
// not claim they are different physical allocations or return either share.
func (self *platformH1RetainedLane) release(byteCount ByteCount) {
	if self == nil {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if byteCount < 0 || self.usedByteCount < byteCount {
		panic("h1 retained lane released unowned bytes")
	}
	self.usedByteCount -= byteCount
	if self.notify != nil {
		close(self.notify)
		self.notify = nil
	}
}

// Final transport completion must not release its graph while an already
// dequeued Client root still waits at the exact Pack ownership boundary.
func (self *platformH1RetainedLane) waitReleased() {
	if self == nil {
		return
	}
	for {
		notify := self.notifyChannel()
		self.stateLock.Lock()
		empty := self.usedByteCount == 0
		self.stateLock.Unlock()
		if empty {
			return
		}
		<-notify
	}
}

// One owner spans reconnect generations. In particular, an old client-held
// root is not granted a second free receive allowance by a replacement socket.
type platformH1RetainedBudget struct {
	send     platformH1RetainedLane
	receive  platformH1RetainedLane
	control  platformH1RetainedLane
	priority platformH1RetainedLane
}

// Saturate malformed custom counts rather than allowing a wrapped graph to
// acquire a small claim. Such a claim cannot fit a finite carrier owner.
func platformH1QueueByteCount(count int) ByteCount {
	if count <= 0 {
		return 0
	}
	if ByteCount(math.MaxInt64)/platformH1QueueSlotByteCount < ByteCount(count) {
		return ByteCount(math.MaxInt64)
	}
	return ByteCount(count) * platformH1QueueSlotByteCount
}

// Even an unbuffered custom route needs permission for its one synchronous
// handoff. Complete-root capacity includes h1+'s metadata tail, not just len.
func (self *PlatformTransportSettings) h1RetainedQueueByteCount(count int) ByteCount {
	maximum := (&PlatformTransport{settings: self, framerSettings: self.FramerSettings}).h1MaxMessageByteCount()
	return max(platformH1QueueByteCount(count), retainedMessageCapacity(ByteCount(maximum)))
}

// The reader can simultaneously hold its 8-KiB read scratch and a grown
// allocation including the one-byte over-limit probe; four times the declared
// maximum bounds the growth/copy overlap. The writer holds one 16-KiB encoded
// batch and a framed copy including its four-byte header. Custom split policy
// may copy the whole message rather than half. Original
// writer roots and the Client-held wire root remain charged to their lanes.
// The existing base additionally retains connection/socket/handshake overhead;
// this minimum prevents a custom tiny base from omitting known live scratch.
func (self *PlatformTransportSettings) h1RetainedScratchMinimum() ByteCount {
	maximum := ByteCount((&PlatformTransport{settings: self, framerSettings: self.FramerSettings}).h1MaxMessageByteCount())
	if (ByteCount(math.MaxInt64)-4-8192-MessagePoolMetaByteCount)/4 < maximum {
		return ByteCount(math.MaxInt64)
	}
	reader := 4*(maximum+1) + 8192 + MessagePoolMetaByteCount
	writer := addReceiveQueueByteCount(webSocketWriteBatchMaxByteCount, retainedMessageCapacity(maximum+4))
	metadata := addReceiveQueueByteCount(platformH1QueueByteCount(self.TransportBufferSize)/platformH1QueueSlotByteCount*96, kib(8))
	return addReceiveQueueByteCount(addReceiveQueueByteCount(reader, writer), metadata)
}

// The base row retains its connection/reader/scratch meaning for raw outer
// tcp/tls extenders. Only an application h1 graph adds all four queue lanes.
func (self *PlatformTransportSettings) H1MemoryReservationByteCount() ByteCount {
	if self == nil {
		return 0
	}
	base := self.H1BudgetByteCount
	if base <= 0 {
		base = MemoryScaledByteCount(kib(512), kib(256))
	}
	if !self.h1RetainedByteAccounting {
		return base
	}
	base = max(base, self.h1RetainedScratchMinimum())
	queue := self.h1RetainedQueueByteCount(self.TransportBufferSize)
	priority := ByteCount(0)
	if count := min(max(0, self.H1AckPriorityBufferSize), max(0, self.TransportBufferSize)); 0 < count {
		priority = self.h1RetainedQueueByteCount(count)
	}
	for _, row := range []ByteCount{queue, queue, queue, priority} {
		if ByteCount(math.MaxInt64)-base < row {
			return ByteCount(math.MaxInt64)
		}
		base += row
	}
	return base
}

// The owner is allocated once with the transport, never on packet admission.
func newPlatformH1RetainedBudget(settings *PlatformTransportSettings) *platformH1RetainedBudget {
	if !settings.h1RetainedByteAccounting {
		return nil
	}
	queue := settings.h1RetainedQueueByteCount(settings.TransportBufferSize)
	priority := ByteCount(0)
	if count := min(max(0, settings.H1AckPriorityBufferSize), max(0, settings.TransportBufferSize)); 0 < count {
		priority = settings.h1RetainedQueueByteCount(count)
	}
	return &platformH1RetainedBudget{
		send:     platformH1RetainedLane{byteLimit: queue},
		receive:  platformH1RetainedLane{byteLimit: queue},
		control:  platformH1RetainedLane{byteLimit: queue},
		priority: platformH1RetainedLane{byteLimit: priority},
	}
}

// A value receipt crosses the private Client read boundary without allocating
// a per-message object or escaping closure. Release clears it before waking a
// producer, including on malformed-frame, cancelled-handoff and panic exits.
type platformH1ReceiveCredit struct {
	lane      *platformH1RetainedLane
	byteCount ByteCount
	done      <-chan struct{}
}

func (self *platformH1ReceiveCredit) release() {
	lane, byteCount := self.lane, self.byteCount
	*self = platformH1ReceiveCredit{}
	lane.release(byteCount)
}

// A successful physical send takes one admitted share and returns its lane's
// permission only after returning that share. No other lane is consulted.
func returnH1RetainedMessage(message []byte, lane *platformH1RetainedLane) {
	byteCount := ByteCount(cap(message))
	MessagePoolReturn(message)
	lane.release(byteCount)
}

// Takes the message on success; a failed immediate offer rolls back only its
// permission and leaves the exact original share with the caller.
func tryWriteH1RetainedRoute(route Route, lane *platformH1RetainedLane, message []byte) bool {
	byteCount := ByteCount(cap(message))
	if !lane.tryReserve(byteCount) {
		return false
	}
	select {
	case route <- message:
		return true
	default:
		lane.release(byteCount)
		return false
	}
}

// Every blocked selector route is either prepaid before the channel send or
// replaced by its credit notification. Unselected permissions are rolled back
// before returning, including timeout, cancellation and route replacement.
type platformH1PendingWrite struct {
	route     Route
	lane      *platformH1RetainedLane
	byteCount ByteCount
	notify    <-chan struct{}
	reserved  bool
}

func prepareH1RetainedWrite(route Route, lane *platformH1RetainedLane, message []byte) platformH1PendingWrite {
	write := platformH1PendingWrite{route: route, lane: lane, byteCount: ByteCount(cap(message))}
	if lane == nil {
		return write
	}
	if lane.byteLimit < write.byteCount {
		write.route = nil
		return write
	}
	write.reserved, write.notify = lane.tryReserveOrSubscribe(write.byteCount)
	if !write.reserved {
		write.route = nil
	}
	return write
}

func (self *platformH1PendingWrite) releaseUnless(selected bool) {
	if self.reserved && !selected {
		self.lane.release(self.byteCount)
	}
	self.reserved = false
}

const (
	h1WriteRetry = iota
	h1WriteCallerClosed
	h1WriteSelectorClosed
	h1WriteTimeout
	h1WriteAccepted
	h1WriteOversized
)

// The common one/two-route case stays on the stack. Larger legacy route sets
// use the existing reflect-select shape with bounded parallel credit entries;
// no worker, callback, or timer is created for a packet.
func waitH1RetainedRouteWrite(ctx context.Context, selectorDone, changed <-chan struct{}, timeout <-chan time.Time,
	routes []Route, properties map[Route]TransferCarrierProperties, message []byte) (selected Route, outcome int) {
	possible := len(routes) == 0
	for _, route := range routes {
		lane := properties[route].h1SendRetained
		if lane == nil || ByteCount(cap(message)) <= lane.byteLimit {
			possible = true
			break
		}
	}
	if !possible {
		return nil, h1WriteOversized
	}
	if len(routes) <= 2 {
		var writes [2]platformH1PendingWrite
		selectedIndex := -1
		for i, route := range routes {
			writes[i] = prepareH1RetainedWrite(route, properties[route].h1SendRetained, message)
		}
		select {
		case <-ctx.Done():
			outcome = h1WriteCallerClosed
		case <-selectorDone:
			outcome = h1WriteSelectorClosed
		case <-changed:
			outcome = h1WriteRetry
		case <-timeout:
			outcome = h1WriteTimeout
		case <-writes[0].notify:
			outcome = h1WriteRetry
		case <-writes[1].notify:
			outcome = h1WriteRetry
		case writes[0].route <- message:
			selected, outcome, selectedIndex = writes[0].route, h1WriteAccepted, 0
		case writes[1].route <- message:
			selected, outcome, selectedIndex = writes[1].route, h1WriteAccepted, 1
		}
		for i := range writes {
			writes[i].releaseUnless(i == selectedIndex)
		}
		return
	}
	writes := make([]platformH1PendingWrite, len(routes))
	cases := make([]reflect.SelectCase, 4, 4+2*len(routes))
	for i, channel := range []<-chan struct{}{ctx.Done(), selectorDone, changed} {
		cases[i] = reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(channel)}
	}
	cases[3] = reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(timeout)}
	for i, route := range routes {
		writes[i] = prepareH1RetainedWrite(route, properties[route].h1SendRetained, message)
		cases = append(cases,
			reflect.SelectCase{Dir: reflect.SelectSend, Chan: reflect.ValueOf(writes[i].route), Send: reflect.ValueOf(message)},
			reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(writes[i].notify)})
	}
	chosen, _, _ := reflect.Select(cases)
	selectedIndex := -1
	switch chosen {
	case 0:
		outcome = h1WriteCallerClosed
	case 1:
		outcome = h1WriteSelectorClosed
	case 2:
		outcome = h1WriteRetry
	case 3:
		outcome = h1WriteTimeout
	default:
		if (chosen-4)%2 == 0 {
			selectedIndex = (chosen - 4) / 2
			selected, outcome = routes[selectedIndex], h1WriteAccepted
		} else {
			outcome = h1WriteRetry
		}
	}
	for i := range writes {
		writes[i].releaseUnless(i == selectedIndex)
	}
	return
}

// The exact reliable reader can retain its single already-read root while
// waiting for bytes or channel capacity. Cancellation returns that root; the
// fixed lane is never enlarged and no reliable message is silently dropped.
func (self *PlatformTransport) offerH1Retained(done <-chan struct{}, route chan<- []byte,
	message []byte, lane *platformH1RetainedLane, control bool) bool {
	byteCount := ByteCount(cap(message))
	if lane.byteLimit < byteCount {
		MessagePoolReturn(message)
		return false
	}
	blocked := false
	for {
		var notify <-chan struct{}
		if blocked {
			notify = lane.notifyChannel()
		}
		if lane.tryReserve(byteCount) {
			switch tryOfferPooledReceive(done, route, message) {
			case pooledReceiveOfferDelivered:
				return true
			case pooledReceiveOfferFull:
				if !blocked {
					if control {
						self.receiveStats.recordH1ControlBackpressure(len(message))
					} else {
						self.receiveStats.recordQueueBackpressure(TransportModeH1, len(message))
					}
				}
				select {
				case <-done:
					returnH1RetainedMessage(message, lane)
					return false
				case route <- message:
					return true
				}
			default:
				returnH1RetainedMessage(message, lane)
				return false
			}
		}
		if !blocked {
			if control {
				self.receiveStats.recordH1ControlBackpressure(len(message))
			} else {
				self.receiveStats.recordQueueBackpressure(TransportModeH1, len(message))
			}
			blocked = true
			continue
		}
		select {
		case <-done:
			MessagePoolReturn(message)
			return false
		case <-notify:
		}
	}
}
