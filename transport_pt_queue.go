package connect

import (
	"container/heap"
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
}

func newCombineQueue(settings *PacketTranslationSettings) *combineQueue {
	cq := &combineQueue{
		settings:      settings,
		orderedItems:  []*combineItem{},
		keyItems:      map[[17]byte]*combineItem{},
		addrItemCount: map[string]int{},
	}
	heap.Init(cq)
	return cq
}

func (self *combineQueue) RemoveOlder(minUpdateTime time.Time) {
	for 0 < len(self.orderedItems) && self.orderedItems[0].updateTime.Before(minUpdateTime) {
		item := heap.Remove(self, 0).(*combineItem)

		delete(self.keyItems, item.key)

		if c := self.addrItemCount[item.addr.String()]; c <= 1 {
			delete(self.addrItemCount, item.addr.String())
		} else {
			self.addrItemCount[item.addr.String()] = c - 1
		}
	}
}

func (self *combineQueue) Combine(addr net.Addr, header [18]byte, data []byte) (out *packet, limit bool, err error) {
	c := uint8(header[16])
	i := uint8(header[17])

	if c <= i {
		err = fmt.Errorf("index must be less than count %d <= %d", c, i)
		return
	}

	key := [17]byte(header[:])

	item, ok := self.keyItems[key]
	if !ok {
		if self.settings.DnsMaxCombinePerAddress <= self.addrItemCount[addr.String()] {
			// fmt.Printf("LIMIT ADDR (%d)\n", self.addrItemCount[addr.String()])
			limit = true
			return
		}

		if self.settings.DnsMaxCombine <= int64(len(self.orderedItems)) {
			// fmt.Printf("LIMIT ALL\n")
			limit = true
			return
		}

		c := uint8(key[16])
		item = &combineItem{
			key:     key,
			addr:    addr,
			packets: make([]*packet, c),
		}

	}

	if item.packets[i] == nil {
		item.n += 1
	}
	item.packets[i] = &packet{
		data: data,
		addr: addr,
	}
	item.updateAddr = addr
	item.updateTime = time.Now()

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
			heap.Remove(self, item.heapIndex)

			delete(self.keyItems, item.key)

			if c := self.addrItemCount[addr.String()]; c <= 1 {
				delete(self.addrItemCount, addr.String())
			} else {
				self.addrItemCount[addr.String()] = c - 1
			}
		}

	} else if !ok {
		heap.Push(self, item)

		self.keyItems[item.key] = item
		self.addrItemCount[addr.String()] += 1
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
	addr       net.Addr
	id         uint16
	header     [18]byte
	tld        []byte
	updateTime time.Time

	heapIndex    int
	maxHeapIndex int
}

// safe to call from multiple goroutines
// FIXME maintain max heap also
type pumpQueue struct {
	stateLock    sync.Mutex
	orderedItems []*pumpItem

	addrMaxHeap map[string]*pumpQueueMaxHeap

	settings *PacketTranslationSettings
}

func newPumpQueue(settings *PacketTranslationSettings) *pumpQueue {
	pq := &pumpQueue{
		orderedItems: []*pumpItem{},
		addrMaxHeap:  map[string]*pumpQueueMaxHeap{},
		settings:     settings,
	}
	heap.Init(pq)
	return pq
}

func (self *pumpQueue) RemoveLast(addr net.Addr) *pumpItem {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	maxHeap, ok := self.addrMaxHeap[addr.String()]
	if !ok {
		return nil
	}

	item := maxHeap.RemoveFirst()
	if maxHeap.Len() == 0 {
		delete(self.addrMaxHeap, addr.String())
	}
	heap.Remove(self, item.heapIndex)
	return item
}

func (self *pumpQueue) RemoveLastN(addr net.Addr, n int) []*pumpItem {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	maxHeap, ok := self.addrMaxHeap[addr.String()]
	if !ok {
		return nil
	}

	if maxHeap.Len() < n {
		return nil
	}

	items := make([]*pumpItem, n)
	for i := range n {
		item := maxHeap.RemoveFirst()
		if maxHeap.Len() == 0 {
			delete(self.addrMaxHeap, addr.String())
		}
		heap.Remove(self, item.heapIndex)
		items[i] = item
	}
	return items
}

func (self *pumpQueue) RemoveOlder(minUpdateTime time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	for 0 < len(self.orderedItems) && self.orderedItems[0].updateTime.Before(minUpdateTime) {
		item := heap.Remove(self, 0).(*pumpItem)
		maxHeap, ok := self.addrMaxHeap[item.addr.String()]
		if ok {
			heap.Remove(maxHeap, item.maxHeapIndex)
			if maxHeap.Len() == 0 {
				delete(self.addrMaxHeap, item.addr.String())
			}
		}
	}
}

func (self *pumpQueue) Add(item *pumpItem) (limit bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	// note a hard limit is enforced rather than replacing older with newer
	// this is to prevent unlimited abuse where pump headers can be added forever

	if self.settings.DnsMaxPumpHosts <= int64(len(self.orderedItems)) {
		limit = true
		return
	}

	maxHeap, ok := self.addrMaxHeap[item.addr.String()]
	if !ok {
		maxHeap = newPumpQueueMaxHeap()
		self.addrMaxHeap[item.addr.String()] = maxHeap
	}

	if self.settings.DnsMaxPumpHostsPerAddress <= maxHeap.Len() {
		limit = true
		return
	}

	item.updateTime = time.Now()

	heap.Push(self, item)
	heap.Push(maxHeap, item)
	return
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

func (self *pumpQueue) Less(i int, j int) bool {
	return self.orderedItems[i].updateTime.Before(self.orderedItems[j].updateTime)
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

func (self *pumpQueueMaxHeap) Less(i int, j int) bool {
	return self.orderedItems[j].updateTime.Before(self.orderedItems[i].updateTime)
}

func (self *pumpQueueMaxHeap) Swap(i int, j int) {
	a := self.orderedItems[i]
	b := self.orderedItems[j]
	b.maxHeapIndex = i
	self.orderedItems[i] = b
	a.maxHeapIndex = j
	self.orderedItems[j] = a
}
