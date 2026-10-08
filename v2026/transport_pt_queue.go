package connect

import (
	"container/heap"
	"container/list"
	"fmt"
	"net"
	"sync"
	"time"
)

type combineItem struct {
	// the header with the index zeroed out
	key        [17]byte
	addr       net.Addr
	packets    []*packet
	n          int
	updateAddr net.Addr
	updateTime time.Time

	heapIndex int
	// retainedByteCount includes the item, its fragment pointer table, every
	// packet wrapper, and each pooled fragment backing allocation.
	retainedByteCount ByteCount
	payloadByteCount  ByteCount
}

const (
	combineItemRetainedByteCount    ByteCount = 256
	combinePacketRetainedByteCount  ByteCount = 48
	combinePointerRetainedByteCount ByteCount = 8
)

func newCombineItemRetainedByteCount(fragmentCount int) ByteCount {
	return combineItemRetainedByteCount + ByteCount(fragmentCount)*combinePointerRetainedByteCount
}

func combineFragmentRetainedByteCount(data []byte) ByteCount {
	return combinePacketRetainedByteCount + MessagePoolMetaByteCount + ByteCount(cap(data))
}

// func newCombineItem(key [17]byte) *combineItem {
// 	c := uint8(key[16])
// 	return &combineItem{
// 		key: key,
// 		packets: make([]*packet, c),
// 	}
// }

// not safe to call from multiple goroutines
// ordered by update time
type combineQueue struct {
	settings *PacketTranslationSettings

	orderedItems  []*combineItem
	keyItems      map[[17]byte]*combineItem
	addrItemCount map[string]int
	addrByteCount map[string]ByteCount
	retainedBytes ByteCount
}

func newCombineQueue(settings *PacketTranslationSettings) *combineQueue {
	cq := &combineQueue{
		settings:      settings,
		orderedItems:  []*combineItem{},
		keyItems:      map[[17]byte]*combineItem{},
		addrItemCount: map[string]int{},
		addrByteCount: map[string]ByteCount{},
	}
	heap.Init(cq)
	return cq
}

func (self *combineQueue) remove(item *combineItem, releasePackets bool) {
	heap.Remove(self, item.heapIndex)
	delete(self.keyItems, item.key)
	addrKey := item.addr.String()
	if c := self.addrItemCount[addrKey]; c <= 1 {
		delete(self.addrItemCount, addrKey)
	} else {
		self.addrItemCount[addrKey] = c - 1
	}
	if bytes := self.addrByteCount[addrKey] - item.retainedByteCount; bytes <= 0 {
		delete(self.addrByteCount, addrKey)
	} else {
		self.addrByteCount[addrKey] = bytes
	}
	self.retainedBytes -= item.retainedByteCount
	if releasePackets {
		for _, p := range item.packets {
			if p != nil {
				MessagePoolReturn(p.data)
			}
		}
	}
}

func (self *combineQueue) OldestUpdateTime() (time.Time, bool) {
	if len(self.orderedItems) == 0 {
		return time.Time{}, false
	}
	return self.orderedItems[0].updateTime, true
}

func (self *combineQueue) RetainedByteCount() ByteCount {
	return self.retainedBytes
}

func (self *combineQueue) RemoveOlder(minUpdateTime time.Time) {
	for 0 < len(self.orderedItems) && !self.orderedItems[0].updateTime.After(minUpdateTime) {
		self.remove(self.orderedItems[0], true)
	}
}

func (self *combineQueue) Combine(addr net.Addr, header [18]byte, data []byte) (out *packet, limit bool, err error) {
	c := uint8(header[16])
	i := uint8(header[17])

	if c == 0 {
		err = fmt.Errorf("fragment count must be positive")
		return
	}
	if c <= i {
		err = fmt.Errorf("index must be less than count %d <= %d", c, i)
		return
	}

	key := [17]byte(header[:])
	if self.settings.DnsMaxCombineFragmentCount < int(c) {
		limit = true
		return
	}

	item, ok := self.keyItems[key]
	if !ok {
		addrKey := addr.String()
		if self.settings.DnsMaxCombinePerAddress <= self.addrItemCount[addrKey] {
			// fmt.Printf("LIMIT ADDR (%d)\n", self.addrItemCount[addr.String()])
			limit = true
			return
		}

		if self.settings.DnsMaxCombine <= int64(len(self.orderedItems)) {
			// fmt.Printf("LIMIT ALL\n")
			limit = true
			return
		}

		item = &combineItem{
			key:               key,
			addr:              addr,
			packets:           make([]*packet, c),
			retainedByteCount: newCombineItemRetainedByteCount(int(c)),
		}
	}

	oldPacket := item.packets[i]
	oldRetainedByteCount := ByteCount(0)
	oldPayloadByteCount := ByteCount(0)
	if oldPacket != nil {
		oldRetainedByteCount = combineFragmentRetainedByteCount(oldPacket.data)
		oldPayloadByteCount = ByteCount(len(oldPacket.data))
	}
	retainedDelta := combineFragmentRetainedByteCount(data) - oldRetainedByteCount
	payloadDelta := ByteCount(len(data)) - oldPayloadByteCount
	if self.settings.DnsMaxCombinedPacketByteCount < item.payloadByteCount+payloadDelta {
		limit = true
		return
	}
	prospectiveItemBytes := item.retainedByteCount + retainedDelta
	addrKey := item.addr.String()
	prospectiveTotalBytes := self.retainedBytes + retainedDelta
	prospectiveAddressBytes := self.addrByteCount[addrKey] + retainedDelta
	if !ok {
		prospectiveTotalBytes += item.retainedByteCount
		prospectiveAddressBytes += item.retainedByteCount
	}
	if self.settings.DnsMaxCombineBytes < prospectiveTotalBytes ||
		self.settings.DnsMaxCombineBytesPerAddress < prospectiveAddressBytes {
		limit = true
		return
	}

	if oldPacket == nil {
		item.n += 1
	} else {
		// duplicate fragment index; release the prior buffer
		MessagePoolReturn(oldPacket.data)
	}
	item.packets[i] = &packet{
		data: data,
		addr: addr,
	}
	item.updateAddr = addr
	item.updateTime = time.Now()
	item.retainedByteCount = prospectiveItemBytes
	item.payloadByteCount += payloadDelta
	if ok {
		self.retainedBytes += retainedDelta
		self.addrByteCount[addrKey] += retainedDelta
	}

	if item.n == len(item.packets) {
		// combine the data
		n := 0
		for _, p := range item.packets {
			n += len(p.data)
		}
		// data := make([]byte, 0, n)
		data := MessagePoolGet(n)
		i := 0
		for _, p := range item.packets {
			copy(data[i:], p.data)
			// if m != len(p.data) {
			// 	panic("MISMATCH LEN")
			// }
			i += len(p.data)
			MessagePoolReturn(p.data)
		}
		// if i != n {
		// 	panic("MISMATCH")
		// }
		out = &packet{
			data: data,
			addr: item.updateAddr,
		}

		if ok {
			self.remove(item, false)
		}

	} else if !ok {
		heap.Push(self, item)

		self.keyItems[item.key] = item
		self.addrItemCount[addrKey] += 1
		self.addrByteCount[addrKey] += item.retainedByteCount
		self.retainedBytes += item.retainedByteCount
	} else {
		heap.Fix(self, item.heapIndex)
	}

	return
}

// heap.Interface

func (self *combineQueue) Push(x any) {
	item := x.(*combineItem)
	item.heapIndex = len(self.orderedItems)
	self.orderedItems = append(self.orderedItems, item)
}

func (self *combineQueue) Pop() any {
	n := len(self.orderedItems)
	i := n - 1
	item := self.orderedItems[i]
	self.orderedItems[i] = nil
	self.orderedItems = self.orderedItems[:n-1]
	return item
}

// `sort.Interface`

func (self *combineQueue) Len() int {
	return len(self.orderedItems)
}

func (self *combineQueue) Less(i int, j int) bool {
	return self.orderedItems[i].updateTime.Before(self.orderedItems[j].updateTime)
}

func (self *combineQueue) Swap(i int, j int) {
	a := self.orderedItems[i]
	b := self.orderedItems[j]
	b.heapIndex = i
	self.orderedItems[i] = b
	a.heapIndex = j
	self.orderedItems[j] = a
}

type pumpItem struct {
	addr   net.Addr
	id     uint16
	header [18]byte
	tld    []byte
	// the request carried an edns0 opt record, so the response paired with
	// it carries one too (rfc 6891 section 7). A synthesized item takes the
	// latest real request's capability from its peer's bounded state.
	edns       bool
	updateTime time.Time
	// insertion order within the owning pumpQueue, assigned by Add.
	// zero for an item that was never added.
	seq uint64

	heapIndex    int
	maxHeapIndex int
}

// The latest real request's capability outlives its consumable pump header.
// The queue owns this state, its hard peer bound, expiry and shutdown cleanup.
type pumpPeerState struct {
	addrKey    string
	edns       bool
	updateTime time.Time
}

// Safe for concurrent use. Queued headers and recently observed peer
// capabilities have independent bounds of DnsMaxPumpHosts each.
type pumpQueue struct {
	stateLock    sync.Mutex
	orderedItems []*pumpItem

	addrMaxHeap       map[string]*pumpQueueMaxHeap
	peerStateEntries  map[string]*list.Element
	orderedPeerStates list.List

	// counter for pumpItem.seq, which breaks ties between items stamped with the
	// same updateTime. updateTime alone is only a partial order, and a heap over
	// a partial order returns an arbitrary element among ties, so RemoveLast
	// would not reliably return the most recently added item. That is not a rare
	// edge case: time.Now() advances every ~500us on Windows, so a burst of adds
	// lands wholly inside one tick and the ties are the normal case there.
	seq uint64

	settings *PacketTranslationSettings
}

func newPumpQueue(settings *PacketTranslationSettings) *pumpQueue {
	pq := &pumpQueue{
		orderedItems:     []*pumpItem{},
		addrMaxHeap:      map[string]*pumpQueueMaxHeap{},
		peerStateEntries: map[string]*list.Element{},
		settings:         settings,
	}
	heap.Init(pq)
	return pq
}

func (self *pumpQueue) RemoveLast(addr net.Addr) *pumpItem {
	addrKey := addr.String()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.removeLastWithLock(addrKey)
}

// Removes one paired header without forgetting the latest peer capability.
func (self *pumpQueue) removeLastWithLock(addrKey string) *pumpItem {
	maxHeap, ok := self.addrMaxHeap[addrKey]
	if !ok {
		return nil
	}

	item := maxHeap.RemoveFirst()
	if maxHeap.Len() == 0 {
		delete(self.addrMaxHeap, addrKey)
	}
	heap.Remove(self, item.heapIndex)
	return item
}

func (self *pumpQueue) RemoveLastN(addr net.Addr, n int) []*pumpItem {
	addrKey := addr.String()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	maxHeap, ok := self.addrMaxHeap[addrKey]
	if !ok {
		return nil
	}

	if maxHeap.Len() < n {
		return nil
	}

	items := make([]*pumpItem, n)
	for i := range n {
		items[i] = self.removeLastWithLock(addrKey)
	}
	return items
}

// Takes up to n paired headers and one coherent latest-capability snapshot for
// every synthesized tail. An older queued request retains its own edns bit.
func (self *pumpQueue) RemoveAvailable(addr net.Addr, n int) (items []*pumpItem, edns bool) {
	addrKey := addr.String()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if entry := self.peerStateEntries[addrKey]; entry != nil {
		edns = entry.Value.(*pumpPeerState).edns
	}
	for range n {
		item := self.removeLastWithLock(addrKey)
		if item == nil {
			break
		}
		items = append(items, item)
	}
	return items, edns
}

// removes every item that was updated at or before minUpdateTime.
// see combineQueue.RemoveOlder for why the boundary is inclusive.
func (self *pumpQueue) RemoveOlder(minUpdateTime time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	for 0 < len(self.orderedItems) && !self.orderedItems[0].updateTime.After(minUpdateTime) {
		item := heap.Remove(self, 0).(*pumpItem)
		maxHeap, ok := self.addrMaxHeap[item.addr.String()]
		if ok {
			heap.Remove(maxHeap, item.maxHeapIndex)
			if maxHeap.Len() == 0 {
				delete(self.addrMaxHeap, item.addr.String())
			}
		}
	}
	for entry := self.orderedPeerStates.Front(); entry != nil; entry = self.orderedPeerStates.Front() {
		if entry.Value.(*pumpPeerState).updateTime.After(minUpdateTime) {
			break
		}
		self.removePeerWithLock(entry)
	}
}

// Includes drained peers, so the existing expiry worker keeps their timer armed.
func (self *pumpQueue) OldestUpdateTime() (time.Time, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	var oldest time.Time
	ok := false
	if len(self.orderedItems) != 0 {
		oldest, ok = self.orderedItems[0].updateTime, true
	}
	if entry := self.orderedPeerStates.Front(); entry != nil {
		updated := entry.Value.(*pumpPeerState).updateTime
		if !ok || updated.Before(oldest) {
			oldest, ok = updated, true
		}
	}
	return oldest, ok
}

func (self *pumpQueue) Add(item *pumpItem) (limit bool) {
	addrKey := item.addr.String()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	// Peer list order and timestamp order must agree even for concurrent adds.
	now := time.Now()
	self.rememberPeerWithLock(addrKey, item.edns, now)

	// note a hard limit is enforced rather than replacing older with newer
	// this is to prevent unlimited abuse where pump headers can be added forever

	if self.settings.DnsMaxPumpHosts <= int64(len(self.orderedItems)) {
		limit = true
		return
	}

	maxHeap, ok := self.addrMaxHeap[addrKey]
	if !ok {
		maxHeap = newPumpQueueMaxHeap()
		self.addrMaxHeap[addrKey] = maxHeap
	}

	if self.settings.DnsMaxPumpHostsPerAddress <= maxHeap.Len() {
		limit = true
		return
	}

	item.updateTime = now
	self.seq += 1
	item.seq = self.seq

	heap.Push(self, item)
	heap.Push(maxHeap, item)
	return
}

// Refreshes capability even when header admission is full: the latest real
// request, including a transition back to legacy, governs synthetic replies.
// Oldest peer metadata is evicted independently of consumable queued headers.
func (self *pumpQueue) rememberPeerWithLock(addrKey string, edns bool, now time.Time) {
	if entry := self.peerStateEntries[addrKey]; entry != nil {
		state := entry.Value.(*pumpPeerState)
		state.edns, state.updateTime = edns, now
		self.orderedPeerStates.MoveToBack(entry)
		return
	}
	if self.settings.DnsMaxPumpHosts <= 0 {
		return
	}
	if self.settings.DnsMaxPumpHosts <= int64(len(self.peerStateEntries)) {
		self.removePeerWithLock(self.orderedPeerStates.Front())
	}
	state := &pumpPeerState{addrKey: addrKey, edns: edns, updateTime: now}
	self.peerStateEntries[addrKey] = self.orderedPeerStates.PushBack(state)
}

// Removes one bounded peer entry from both indexes.
func (self *pumpQueue) removePeerWithLock(entry *list.Element) {
	delete(self.peerStateEntries, entry.Value.(*pumpPeerState).addrKey)
	self.orderedPeerStates.Remove(entry)
}

// Drops all non-pooled state after the owning translation joins its workers.
func (self *pumpQueue) Clear() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.orderedItems = nil
	clear(self.addrMaxHeap)
	clear(self.peerStateEntries)
	self.orderedPeerStates.Init()
}

// heap.Interface

func (self *pumpQueue) Push(x any) {
	item := x.(*pumpItem)
	item.heapIndex = len(self.orderedItems)
	self.orderedItems = append(self.orderedItems, item)
}

func (self *pumpQueue) Pop() any {
	n := len(self.orderedItems)
	i := n - 1
	item := self.orderedItems[i]
	self.orderedItems[i] = nil
	self.orderedItems = self.orderedItems[:n-1]
	return item
}

// `sort.Interface`

func (self *pumpQueue) Len() int {
	return len(self.orderedItems)
}

// ordered by update time ascending, ties broken by insertion order so that the
// order is total. see pumpQueue.seq
func (self *pumpQueue) Less(i int, j int) bool {
	a := self.orderedItems[i]
	b := self.orderedItems[j]
	if a.updateTime.Equal(b.updateTime) {
		return a.seq < b.seq
	}
	return a.updateTime.Before(b.updateTime)
}

func (self *pumpQueue) Swap(i int, j int) {
	a := self.orderedItems[i]
	b := self.orderedItems[j]
	b.heapIndex = i
	self.orderedItems[i] = b
	a.heapIndex = j
	self.orderedItems[j] = a
}

// ordered by update time descending
type pumpQueueMaxHeap struct {
	orderedItems []*pumpItem
}

func newPumpQueueMaxHeap() *pumpQueueMaxHeap {
	pqm := &pumpQueueMaxHeap{
		orderedItems: []*pumpItem{},
	}
	heap.Init(pqm)
	return pqm
}

func (self *pumpQueueMaxHeap) PeekFirst() *pumpItem {
	if len(self.orderedItems) == 0 {
		return nil
	}
	return self.orderedItems[0]
}

func (self *pumpQueueMaxHeap) RemoveFirst() *pumpItem {
	if len(self.orderedItems) == 0 {
		return nil
	}

	item := heap.Remove(self, 0).(*pumpItem)
	return item
}

// heap.Interface

func (self *pumpQueueMaxHeap) Push(x any) {
	item := x.(*pumpItem)
	item.maxHeapIndex = len(self.orderedItems)
	self.orderedItems = append(self.orderedItems, item)
}

func (self *pumpQueueMaxHeap) Pop() any {
	n := len(self.orderedItems)
	i := n - 1
	item := self.orderedItems[i]
	self.orderedItems[i] = nil
	self.orderedItems = self.orderedItems[:n-1]
	return item
}

// `sort.Interface`

func (self *pumpQueueMaxHeap) Len() int {
	return len(self.orderedItems)
}

// ordered by update time descending, ties broken by insertion order so that the
// order is total. see pumpQueue.seq. RemoveLast reads the head of this heap, so
// without the tiebreak it returns an arbitrary item from the newest tick rather
// than the most recently added one.
func (self *pumpQueueMaxHeap) Less(i int, j int) bool {
	a := self.orderedItems[i]
	b := self.orderedItems[j]
	if a.updateTime.Equal(b.updateTime) {
		return b.seq < a.seq
	}
	return b.updateTime.Before(a.updateTime)
}

func (self *pumpQueueMaxHeap) Swap(i int, j int) {
	a := self.orderedItems[i]
	b := self.orderedItems[j]
	b.maxHeapIndex = i
	self.orderedItems[i] = b
	a.maxHeapIndex = j
	self.orderedItems[j] = a
}
