package connect

import (
	"net"
	"context"
	"time"
	"fmt"

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


type PacketTranslationSettings struct {
	DnsTlds [][]byte
	DnsPumpTimeout time.Duration
	DnsReadTimeout time.Duration
	DnsStateTimeout time.Duration
}


type packet struct {
	data []byte
	addr net.Addr
}


// implements PacketConn
type packetTranslation struct {
	ctx context.Context
	cancel context.CancelFunc

	ptMode PacketTranslationMode
	packetConn net.PacketConn

	settings *PacketTranslationSettings

	dnsAddr *net.UDPAddr
	dnsClient bool
	dnsPumpQueue *pumpQueue
	dnsCombineQueue *combineQueue

	in chan *packet
	out chan *packet
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
		ctx: cancelCtx,
		cancel: cancel,
		ptMode: ptMode,
		packetConn: packetConn,
		in: make(chan *packet),
		out: make(chan *packet),
		forward: make(chan *packet),
	}

	switch ptMode {
	case PacketTranslationModeDns, PacketTranslationModeDnsPump:
		pt.dnsClient = true
		go pt.encodeDns()
		go pt.decodeDns()
	case PacketTranslationModeDecode53:
		pt.dnsClient = false
		pt.dnsPumpQueue = newPumpQueue()
		pt.dnsCombineQueue = newCombineQueue()
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
			writeOne := func(p *packet)(error) {
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
				case <- self.ctx.Done():
					return
				case p := <- self.out:
					// each write includes one pump header
					if err := writeOne(p); err != nil {
						return
					}
				case <- time.After(self.settings.DnsPumpTimeout):
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
				case <- self.ctx.Done():
					return
				case p := <- self.out:
					if err := writeOne(p); err != nil {
						return
					}
				}
			}
		} else {
			select {
			case <- self.ctx.Done():
				return
			case p := <- self.out:
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
						id: id,
						header: header,
						tld: tld,
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
			case p := <- self.forward:
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
	
	packetData := make([]byte, 2048)
	var buf [1024]byte

	for {
		now := time.Now()
		self.packetConn.SetReadDeadline(now.Add(self.settings.DnsReadTimeout))
		n, addr, err := self.packetConn.ReadFrom(packetData)

		now = time.Now()
		minUpdateTime := now.Add(-self.settings.DnsStateTimeout)
		self.dnsPumpQueue.RemoveOlder(minUpdateTime)
		self.dnsCombineQueue.RemoveOlder(minUpdateTime)

		if err != nil {
			continue
		}


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

			item := &pumpItem{
				id: id,
				header: header,
				tld: tld,
			}

			self.dnsPumpQueue.Add(addr, item)
			// if limit, drop the pump header but continue to process the packet
		}

		
		if c := uint8(header[16]); c == 0 {
			// just a pump
			continue
		}

		dataCopy := make([]byte, len(data))
		copy(dataCopy, data)

		out, limit, err := self.dnsCombineQueue.Combine(header, dataCopy, addr)

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
		case <- self.ctx.Done():
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
	case <- self.ctx.Done():
		err = fmt.Errorf("Done.")
		return
	case self.out <- p:
		n = len(packetData)
		return
	}
}

func (self *packetTranslation) ReadFrom(packetData []byte) (n int, addr net.Addr, err error) {
	select {
	case <- self.ctx.Done():
		err = fmt.Errorf("Done.")
		return
	case p := <- self.in:
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
	key [17]byte
	packets []*packet
	n int
	updateAddr net.Addr
	updateTime time.Time
}

// func newCombineItem(key [17]byte) *combineItem {
// 	c := uint8(key[16])
// 	return &combineItem{
// 		key: key,
// 		packets: make([]*packet, c),
// 	}
// }


// ordered by update time
type combineQueue struct {

	orderedItem []*combineItem

	keyItems map[[17]byte]*combineItem

	addrItemCount map[string]int

}

func newCombineQueue() *combineQueue {
	// FIXME
	return nil
}


func (self *combineQueue) RemoveOlder(minUpdateTime time.Time) {

}


func (self *combineQueue) Combine(header [18]byte, data []byte, addr net.Addr) (out *packet, limit bool, err error) {

	c := uint8(header[16])
	i := uint8(header[17])

	if c <= i {
		err = fmt.Errorf("index must be less than count %d <= %d", c, i)
		return
	}

	// FIXME
	// key := [17]byte(header)

	// FIXME combine

	// FIXME if miss check the limits


	self.stateLock.Lock()
	defer self.stateLock.Unlock()


	item, ok := self.headerItems[header]
	if !ok {
		if LIMIT <= addrItemCount[addr.String()] {
			limit = true
			return
		}

		if LIMIT <= len(self.orderedItems) {
			limit = true
			return
		}

		c := uint8(key[16])
		item = &combineItem{
			key: [17]byte(header),
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
	item.updateTime = now
	
	if item.n == len(item.packets) {
		// combine the data
		n := 0
		for _, p := item.packets {
			n += len(p.data)
		}
		data := make([]byte, 0, n)
		for _, p := item.packets {
			data = append(data, p.data...)
		}
		out = &packet{
			data: data,
			addr: item.updateAddr,
		}

		if ok {
			REMOVEHEAP(item)

			delete(headerItems, item.key)

			if c := addrItemCount[addr.String()]; c <= 1 {
				delete(addrItemCount, addr.String())
			} else {
				addrItemCount[addr.String()] = c - 1
			}
		}

	} else if !ok {
		ADDHEAP(item)

		headerItems[item.key] = item
		addrItemCount[addr.String()] += 1
	} else {
		UPDATEHEAP(item)
	}

	return
}



type pumpItem struct {
	id uint16
	header [18]byte
	tld []byte
}


// FIXME maintain max heap also
type pumpQueue struct {

}

func newPumpQueue() *pumpQueue {
	// FIXME
	return nil
}

func (self *pumpQueue) RemoveLast(addr net.Addr) *pumpItem {
	return nil
}

func (self *pumpQueue) RemoveOlder(minUpdateTime time.Time) {
	return
}

func (self *pumpQueue) Add(addr net.Addr, pumpItem *pumpItem) (limit bool) {
	return
}













