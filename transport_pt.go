package connect

import (
	"container/heap"
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	mathrand "math/rand"

	"golang.org/x/net/dns/dnsmessage"
)

// packet transformation converts a quic packet to other represenations
// pt does not implement any delivery guarantees

// dns:
// convert to dns txt record requests and responses
// each response is paired uniquely with the latest request
// the client pumps empty requests at a constant rate to give the server requests to pair with
// in the case of a dns proxy,
//   the server is set up as a dns zone, so that each request is ultimately sent
//   to the server, via the dns backbone if necessary

// there is one master translation with several modes because all the translations must share the same ports

// the dns pt header is [16 random][1 count][1 index]

// note the caller must set DnsTld
func DefaultPacketTranslationSettings() *PacketTranslationSettings {
	return &PacketTranslationSettings{
		DnsTlds:        [][]byte{},
		DnsPumpTimeout: 5 * time.Second,
		// DnsReadTimeout: 1 * time.Second:
		DnsStateTimeout: 5 * time.Second,

		DnsMaxCombinePerAddress: 1024,
		DnsMaxCombine:           1024 * 1024 * 1024,

		DnsMaxPumpHostsPerAddress: 1024,
		DnsMaxPumpHosts:           1024 * 1024 * 1024,
	}
}

type PacketTranslationSettings struct {
	DnsTlds        [][]byte
	DnsPumpTimeout time.Duration
	// DnsReadTimeout time.Duration
	DnsStateTimeout time.Duration

	DnsMaxCombinePerAddress int
	DnsMaxCombine           int

	DnsMaxPumpHostsPerAddress int
	DnsMaxPumpHosts           int
}

type packet struct {
	data []byte
	addr net.Addr
}

// implements PacketConn
type packetTranslation struct {
	ctx    context.Context
	cancel context.CancelFunc

	ptMode     PacketTranslationMode
	packetConn net.PacketConn

	settings *PacketTranslationSettings

	dnsAddr      *net.UDPAddr
	dnsClient    bool
	dnsPumpQueue *pumpQueue

	in      chan *packet
	out     chan *packet
	forward chan *packet
}

// the caller should close in when done
func NewPacketTranslation(
	ctx context.Context,
	ptMode PacketTranslationMode,
	packetConn net.PacketConn,
	settings *PacketTranslationSettings,
) (*packetTranslation, error) {
	cancelCtx, cancel := context.WithCancel(ctx)

	pt := &packetTranslation{
		ctx:        cancelCtx,
		cancel:     cancel,
		ptMode:     ptMode,
		packetConn: packetConn,
		in:         make(chan *packet),
		out:        make(chan *packet),
		forward:    make(chan *packet),
	}

	switch ptMode {
	case PacketTranslationModeDns, PacketTranslationModeDnsPump:
		pt.dnsClient = true
		go pt.encodeDns()
		go pt.decodeDns()
	case PacketTranslationModeDecode53:
		pt.dnsClient = false
		pt.dnsPumpQueue = newPumpQueue(settings)
		go pt.encodeDns()
		go pt.decodeDns()
	default:
		return nil, fmt.Errorf("Unsupported packet translation mode: %s", ptMode)
	}

	return pt, nil
}

func (self *packetTranslation) encodeDns() {
	defer self.cancel()

	var buf [1024]byte
	var id uint16
	for {
		if self.dnsClient {
			writeOne := func(p *packet) error {
				tld := self.settings.DnsTlds[mathrand.Intn(len(self.settings.DnsTlds))]

				c := encodeDnsRequestCount(p.data, tld)

				var header [18]byte
				mathrand.Read(header[0:16])
				header[16] = uint8(c)

				n := 0
				for i := 0; i < c; i += 1 {
					header[17] = uint8(i)

					m, packetData, err := encodeDnsRequest(id, header, p.data[n:], buf, tld)
					id += 1
					if err != nil {
						// drop
						return nil
					}

					_, err = self.packetConn.WriteTo(packetData, p.addr)
					if err != nil {
						return err
					}

					n += m
				}
				return nil
			}

			if self.ptMode == PacketTranslationModeDnsPump {
				select {
				case <-self.ctx.Done():
					return
				case p := <-self.out:
					// each write includes one pump header
					if err := writeOne(p); err != nil {
						return
					}
				case <-time.After(self.settings.DnsPumpTimeout):
					// pump one header the server can use to repsond to
					tld := self.settings.DnsTlds[mathrand.Intn(len(self.settings.DnsTlds))]

					var header [18]byte
					mathrand.Read(header[0:16])

					_, packetData, err := encodeDnsRequest(
						id,
						header,
						make([]byte, 0),
						buf,
						tld,
					)
					id += 1
					if err != nil {
						// drop the packet
						break
					}

					_, err = self.packetConn.WriteTo(packetData, self.dnsAddr)
					if err != nil {
						return
					}
				}
			} else {
				select {
				case <-self.ctx.Done():
					return
				case p := <-self.out:
					if err := writeOne(p); err != nil {
						return
					}
				}
			}
		} else {
			select {
			case <-self.ctx.Done():
				return
			case p := <-self.out:
				minUpdateTime := time.Now().Add(-self.settings.DnsStateTimeout)
				self.dnsPumpQueue.RemoveOlder(minUpdateTime)

				longestTld := self.settings.DnsTlds[0]
				for _, tld := range self.settings.DnsTlds[1:] {
					if len(longestTld) < len(tld) {
						longestTld = tld
					}
				}

				c := encodeDnsResponseCount(p.data, longestTld)

				pumpItems := make([]*pumpItem, c)

				i := 0
				for ; i < c; i += 1 {
					item := self.dnsPumpQueue.RemoveLast(p.addr)
					if item == nil {
						break
					}
					pumpItems[i] = item
				}
				// fill the rest with new headers
				for ; i < c; i += 1 {
					var header [18]byte
					mathrand.Read(header[0:16])
					tld := self.settings.DnsTlds[mathrand.Intn(len(self.settings.DnsTlds))]
					item := &pumpItem{
						id:     id,
						header: header,
						tld:    tld,
					}
					pumpItems[i] = item
					id += 1
				}

				var header [18]byte
				mathrand.Read(header[0:16])
				header[16] = uint8(c)

				n := 0
				for i := 0; i < c; i += 1 {
					header[17] = uint8(i)

					item := pumpItems[i]

					m, packet, err := encodeDnsResponse(
						item.id,
						item.header,
						header,
						p.data[n:],
						buf,
						item.tld,
					)
					if err != nil {
						// stop sending all since one is dropped
						break
					}

					_, err = self.packetConn.WriteTo(packet, p.addr)
					if err != nil {
						return
					}

					n += m
				}
			case p := <-self.forward:
				_, err := self.packetConn.WriteTo(p.data, p.addr)
				if err != nil {
					return
				}
			}
		}
	}
}

func (self *packetTranslation) decodeDns() {
	defer self.cancel()

	dnsCombineQueue := newCombineQueue(self.settings)
	packetData := make([]byte, 2048)
	var buf [1024]byte

	for {
		// self.packetConn.SetReadDeadline(time.Now().Add(self.settings.DnsReadTimeout))
		n, addr, err := self.packetConn.ReadFrom(packetData)
		if err != nil {
			return
		}

		minUpdateTime := time.Now().Add(-self.settings.DnsStateTimeout)
		dnsCombineQueue.RemoveOlder(minUpdateTime)

		var header [18]byte
		var data []byte
		var tld []byte

		if self.dnsClient {
			_, _, header, data, err = decodeDnsResponse(
				packetData[:n],
				buf,
				self.settings.DnsTlds,
			)
		} else {
			self.dnsPumpQueue.RemoveOlder(minUpdateTime)

			var id uint16
			var otherData bool
			id, header, data, tld, err, otherData = decodeDnsRequest(
				packetData[:n],
				buf,
				self.settings.DnsTlds,
			)
			if otherData {
				self.handleDnsOther(packetData[:n], addr)
				continue
			}

			self.dnsPumpQueue.Add(addr, id, header, tld)
			// if limit, drop the pump header but continue to process the packet
		}

		if c := uint8(header[16]); c == 0 {
			// just a pump
			continue
		}

		dataCopy := make([]byte, len(data))
		copy(dataCopy, data)

		out, limit, err := dnsCombineQueue.Combine(header, dataCopy, addr)

		if err != nil {
			// drop the packet
			continue
		}

		if limit {
			// drop the packet
			continue
		}

		if out == nil {
			// packet not combined
			continue
		}

		select {
		case <-self.ctx.Done():
			return
		case self.in <- out:
		}
	}
}

func (self *packetTranslation) handleDnsOther(packetData []byte, addr net.Addr) (err error) {
	p := &dnsmessage.Parser{}
	_, err = p.Start(packetData)
	if err != nil {
		return
	}

	var qs []dnsmessage.Question
	qs, err = p.AllQuestions()
	if err != nil {
		return
	}
	for _, q := range qs {
		switch q.Type {
		case dnsmessage.TypeNS:
			// FIXME handle NS and zone requests by writing packets to forward

			// else unknown
		}
	}

	return
}

// packetData cannot be modified after this is called
func (self *packetTranslation) WriteTo(packetData []byte, addr net.Addr) (n int, err error) {
	p := &packet{
		data: packetData,
		addr: addr,
	}

	select {
	case <-self.ctx.Done():
		err = fmt.Errorf("Done.")
		return
	case self.out <- p:
		n = len(packetData)
		return
	}
}

func (self *packetTranslation) ReadFrom(packetData []byte) (n int, addr net.Addr, err error) {
	select {
	case <-self.ctx.Done():
		err = fmt.Errorf("Done.")
		return
	case p := <-self.in:
		addr = p.addr
		n = copy(packetData, p.data)
		return
	}
}

func (self *packetTranslation) LocalAddr() net.Addr {
	return self.packetConn.LocalAddr()
}

func (self *packetTranslation) SetDeadline(t time.Time) error {
	return self.packetConn.SetDeadline(t)
}

func (self *packetTranslation) SetReadDeadline(t time.Time) error {
	return self.packetConn.SetReadDeadline(t)
}

func (self *packetTranslation) SetWriteDeadline(t time.Time) error {
	return self.packetConn.SetWriteDeadline(t)
}

func (self *packetTranslation) Close() error {
	self.cancel()
	return self.packetConn.Close()
}

// func keyFromHeader(header [18]byte) (key [17]byte) {
// 	key = [17]byte(header)
// 	return key
// }

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
	orderedItems  []*combineItem
	keyItems      map[[17]byte]*combineItem
	addrItemCount map[string]int

	settings *PacketTranslationSettings
}

func newCombineQueue(settings *PacketTranslationSettings) *combineQueue {
	cq := &combineQueue{
		orderedItems:  []*combineItem{},
		keyItems:      map[[17]byte]*combineItem{},
		addrItemCount: map[string]int{},
		settings:      settings,
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

func (self *combineQueue) Combine(header [18]byte, data []byte, addr net.Addr) (out *packet, limit bool, err error) {

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
			limit = true
			return
		}

		if self.settings.DnsMaxCombine <= len(self.orderedItems) {
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
		data := make([]byte, 0, n)
		for _, p := range item.packets {
			data = append(data, p.data...)
		}
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

func (self *pumpQueue) Add(addr net.Addr, id uint16, header [18]byte, tld []byte) (limit bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if self.settings.DnsMaxPumpHosts < len(self.orderedItems) {
		limit = true
		return
	}

	maxHeap, ok := self.addrMaxHeap[addr.String()]
	if !ok {
		maxHeap = newPumpQueueMaxHeap()
		self.addrMaxHeap[addr.String()] = maxHeap
	}

	if self.settings.DnsMaxPumpHostsPerAddress < maxHeap.Len() {
		limit = true
		return
	}

	item := &pumpItem{
		addr:       addr,
		id:         id,
		header:     header,
		tld:        tld,
		updateTime: time.Now(),
	}

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
	heap.Remove(self, item.maxHeapIndex)
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
