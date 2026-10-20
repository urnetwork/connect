package connect

import (
	"slices"
	"sync"
)

// sendSchedulingKey keeps the exact five-tuple local. The scheduler consumes
// it before assigning sequence numbers; after logical-lane negotiation, only
// its bounded stable lane hash can enter sequence identity and the wire Pack.
type sendSchedulingKey struct {
	ipFlow ipPacketFlowKey
	valid  bool
}

func ipSendSchedulingKey(ipPath *IpPath) sendSchedulingKey {
	key, ok := ipPacketFlowKeyFromPath(ipPath)
	return sendSchedulingKey{ipFlow: key, valid: ok}
}

type sendSchedulingKeyOption struct {
	key sendSchedulingKey
}

func scheduleIpFlow(ipPath *IpPath) sendSchedulingKeyOption {
	return sendSchedulingKeyOption{key: ipSendSchedulingKey(ipPath)}
}

// sendPackAdmission keeps the complete pre-sequence working set bounded even
// while SendSequence drains its channel into per-flow queues. One valid IP
// flow may occupy at most capacity-1 slots, leaving one admission for a newly
// active flow. A capacity of one retains the ordinary one-item behavior.
//
// Blocked senders wait in arrival order, and a release grants its slot to the
// oldest waiter it fits before any woken goroutine runs. Waking every waiter
// to race for the slot let the runtime pick the winner: with no idle
// processor it runs a woken group's oldest waiter first, which took the slot
// and waited again ahead of the siblings it beat, so one of many senders won
// every slot. A waiter whose flow is at its limit keeps its place while a
// later waiter of another flow takes the slot. No queued waiter fits a free
// slot, so a sender that fits one takes it at once without passing anyone.
//
// Safe for concurrent use.
type sendPackAdmission struct {
	mutex    sync.Mutex
	capacity int
	count    int
	byKey    map[sendSchedulingKey]int
	waiters  []*sendPackAdmissionWaiter
	closed   bool
}

// One blocked acquire. A grant sets granted under the admission mutex before
// ready closes; close closes ready without a grant.
type sendPackAdmissionWaiter struct {
	key     sendSchedulingKey
	ready   chan struct{}
	granted bool
}

func newSendPackAdmission(capacity int) *sendPackAdmission {
	if capacity <= 0 {
		return nil
	}
	return &sendPackAdmission{
		capacity: capacity,
		byKey:    map[sendSchedulingKey]int{},
	}
}

// Takes a slot for key when one fits it.
func (self *sendPackAdmission) tryTakeWithLock(key sendSchedulingKey) bool {
	keyLimit := self.capacity
	if key.valid && 1 < keyLimit {
		keyLimit -= 1
	}
	if self.capacity <= self.count || keyLimit <= self.byKey[key] {
		return false
	}
	self.count += 1
	self.byKey[key] += 1
	return true
}

// Takes a slot that fits key. Otherwise, when wait is set and admission is
// open, queues a waiter behind every earlier one, and the caller must either
// receive from its ready channel or abandon it.
func (self *sendPackAdmission) tryAcquire(
	key sendSchedulingKey,
	wait bool,
) (acquired bool, closed bool, waiter *sendPackAdmissionWaiter) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.closed {
		return false, true, nil
	}
	if self.tryTakeWithLock(key) {
		return true, false, nil
	}
	if wait {
		waiter = &sendPackAdmissionWaiter{key: key, ready: make(chan struct{})}
		self.waiters = append(self.waiters, waiter)
	}
	return false, false, waiter
}

// An immediate whole-group write cannot overtake an earlier admitted Pack.
// Its one slot also bounds concurrent caller-side wire materialization.
func (self *sendPackAdmission) tryAcquireEmpty(key sendSchedulingKey) bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.closed || self.count != 0 {
		return false
	}
	self.count = 1
	self.byKey[key] = 1
	return true
}

func (self *sendPackAdmission) release(key sendSchedulingKey) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.releaseWithLock(key)
}

// Frees one slot of key and grants it to the oldest waiter it fits. A freed
// slot fits at most one waiter, because none fit before it was freed.
func (self *sendPackAdmission) releaseWithLock(key sendSchedulingKey) {
	if self.byKey[key] <= 0 || self.count <= 0 {
		return
	}
	self.count -= 1
	self.byKey[key] -= 1
	if self.byKey[key] == 0 {
		delete(self.byKey, key)
	}
	for index, waiter := range self.waiters {
		if self.tryTakeWithLock(waiter.key) {
			waiter.granted = true
			close(waiter.ready)
			self.waiters = slices.Delete(self.waiters, index, index+1)
			return
		}
	}
}

// Withdraws a waiter whose caller stopped waiting. A slot granted to it in the
// meantime passes on as its release would, so no grant is lost.
func (self *sendPackAdmission) abandon(waiter *sendPackAdmissionWaiter) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if waiter.granted {
		self.releaseWithLock(waiter.key)
		return
	}
	if index := slices.Index(self.waiters, waiter); 0 <= index {
		self.waiters = slices.Delete(self.waiters, index, index+1)
	}
}

func (self *sendPackAdmission) close() {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.closed {
		return
	}
	self.closed = true
	for _, waiter := range self.waiters {
		close(waiter.ready)
	}
	self.waiters = nil
}

type sendPackFlowQueue struct {
	key   sendSchedulingKey
	packs []*SendPack
}

// sendPackScheduler retains FIFO order within each flow and selects active
// flows round-robin. It is owned exclusively by one SendSequence goroutine.
type sendPackScheduler struct {
	flows  map[sendSchedulingKey]*sendPackFlowQueue
	active []*sendPackFlowQueue
	// order retains exact ingress order for carriers that do not opt into flow
	// isolation. Bounded admission keeps the linear removal a small fixed cost.
	order []*SendPack
	count int
}

func newSendPackScheduler() *sendPackScheduler {
	return &sendPackScheduler{flows: map[sendSchedulingKey]*sendPackFlowQueue{}}
}

func (self *sendPackScheduler) Len() int {
	return self.count
}

// FifoHead exposes the carrier-neutral oldest Pack for flow-isolation
// telemetry. The scheduler retains ownership; callers must not mutate it.
func (self *sendPackScheduler) FifoHead() *SendPack {
	if len(self.order) == 0 {
		return nil
	}
	return self.order[0]
}

func (self *sendPackScheduler) Push(sendPack *SendPack) {
	key := sendPack.schedulingKey
	flow := self.flows[key]
	if flow == nil {
		flow = &sendPackFlowQueue{key: key}
		self.flows[key] = flow
		self.active = append(self.active, flow)
	}
	flow.packs = append(flow.packs, sendPack)
	self.order = append(self.order, sendPack)
	self.count += 1
}

func (self *sendPackScheduler) PushFront(sendPack *SendPack) {
	key := sendPack.schedulingKey
	flow := self.flows[key]
	if flow == nil {
		flow = &sendPackFlowQueue{key: key}
		self.flows[key] = flow
		self.active = append(self.active, flow)
	}
	flow.packs = append(flow.packs, nil)
	copy(flow.packs[1:], flow.packs[:len(flow.packs)-1])
	flow.packs[0] = sendPack
	self.order = append(self.order, nil)
	copy(self.order[1:], self.order[:len(self.order)-1])
	self.order[0] = sendPack
	self.count += 1
}

func (self *sendPackScheduler) removeActive(index int) {
	copy(self.active[index:], self.active[index+1:])
	self.active[len(self.active)-1] = nil
	self.active = self.active[:len(self.active)-1]
}

// Removes one selected Pack from the carrier-neutral ingress order.
func (self *sendPackScheduler) removeOrder(sendPack *SendPack) {
	for orderIndex, orderedSendPack := range self.order {
		if orderedSendPack == sendPack {
			copy(self.order[orderIndex:], self.order[orderIndex+1:])
			self.order[len(self.order)-1] = nil
			self.order = self.order[:len(self.order)-1]
			return
		}
	}
}

func (self *sendPackScheduler) takeFromFlow(
	flow *sendPackFlowQueue,
	activeIndex int,
	rotate bool,
) *SendPack {
	sendPack := flow.packs[0]
	copy(flow.packs, flow.packs[1:])
	flow.packs[len(flow.packs)-1] = nil
	flow.packs = flow.packs[:len(flow.packs)-1]
	self.removeOrder(sendPack)
	self.count -= 1
	if len(flow.packs) == 0 {
		delete(self.flows, flow.key)
		self.removeActive(activeIndex)
	} else if rotate && activeIndex < len(self.active)-1 {
		self.removeActive(activeIndex)
		self.active = append(self.active, flow)
	}
	return sendPack
}

// Selects the oldest Pack without changing cross-flow order. This preserves
// the former channel behavior for carriers without a dedicated flow reserve.
func (self *sendPackScheduler) TakeFifoEligible(
	eligible func(*SendPack) bool,
) *SendPack {
	if len(self.order) == 0 {
		return nil
	}
	sendPack := self.order[0]
	if !eligible(sendPack) {
		return nil
	}
	flow := self.flows[sendPack.schedulingKey]
	if flow == nil || len(flow.packs) == 0 || flow.packs[0] != sendPack {
		return nil
	}
	for activeIndex, activeFlow := range self.active {
		if activeFlow == flow {
			return self.takeFromFlow(flow, activeIndex, false)
		}
	}
	return nil
}

// TakeEligible selects the first eligible active flow, then rotates that flow
// behind every other currently active flow.
func (self *sendPackScheduler) TakeEligible(
	eligible func(*SendPack) bool,
) *SendPack {
	for activeIndex, flow := range self.active {
		if eligible(flow.packs[0]) {
			return self.takeFromFlow(flow, activeIndex, true)
		}
	}
	return nil
}

// TakeSameFlow supports the existing zero-wait two-frame coalescer without
// combining unrelated latency and bulk flows into one Transfer item.
func (self *sendPackScheduler) TakeSameFlow(key sendSchedulingKey) *SendPack {
	flow := self.flows[key]
	if flow == nil {
		return nil
	}
	for activeIndex, activeFlow := range self.active {
		if activeFlow == flow {
			return self.takeFromFlow(flow, activeIndex, false)
		}
	}
	return nil
}

func (self *sendPackScheduler) HasEligible(
	eligible func(*SendPack) bool,
) bool {
	for _, flow := range self.active {
		if eligible(flow.packs[0]) {
			return true
		}
	}
	return false
}

// NoAck data is explicitly unordered. A memory-blocked reliable head cannot
// hold it behind retention capacity it never consumes, even in the same flow.
// The caller restricts eligible to packets guaranteed to remain NoAck.
// Prepared cancellation also uses this removal to dispose an unwritten Pack;
// it never sends ordered data around an earlier reliable owner.
func (self *sendPackScheduler) TakeUnorderedEligible(eligible func(*SendPack) bool) *SendPack {
	for _, pack := range self.order {
		if !eligible(pack) {
			continue
		}
		flow := self.flows[pack.schedulingKey]
		for i, candidate := range flow.packs {
			if candidate == pack {
				copy(flow.packs[i:], flow.packs[i+1:])
				flow.packs[len(flow.packs)-1] = nil
				flow.packs = flow.packs[:len(flow.packs)-1]
				break
			}
		}
		self.removeOrder(pack)
		self.count--
		if len(flow.packs) == 0 {
			delete(self.flows, flow.key)
			for i, candidate := range self.active {
				if candidate == flow {
					self.removeActive(i)
					break
				}
			}
		}
		return pack
	}
	return nil
}

func (self *sendPackScheduler) Drain(dispose func(*SendPack)) {
	for _, flow := range self.active {
		for _, sendPack := range flow.packs {
			dispose(sendPack)
		}
	}
	self.flows = map[sendSchedulingKey]*sendPackFlowQueue{}
	self.active = nil
	self.order = nil
	self.count = 0
}
