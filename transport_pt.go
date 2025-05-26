package connect

import (
	"context"
	"fmt"
	"net"
	"time"

	mathrand "math/rand"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/golang/glog"
)

// "whodis" protocol refers to a suite of pre-authentication packet translation techniques
// that use various side channels that are open on networks without authentication

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

// the dns pt header is 18 bytes: [16 random][1 count][1 index]

type PacketTranslationMode string

const (
	PacketTranslationModeNone PacketTranslationMode = ""
	// form packets to look like dns requests/responses on the wire
	PacketTranslationModeDns PacketTranslationMode = "dns"
	// uses a constant amount of upload bandwidth to establish a reply pump via dns zones
	PacketTranslationModeDnsPump                PacketTranslationMode = "dnspump"
	PacketTranslationModeDecode53               PacketTranslationMode = "decode53"
	PacketTranslationModeDecode53RequireDnsPump PacketTranslationMode = "decode53pumpreq"
)

// note the caller must set DnsTld and DnsAddr
func DefaultPacketTranslationSettings() *PacketTranslationSettings {
	return &PacketTranslationSettings{
		DnsTlds: [][]byte{},
		// a good baseline is 200 pumps per second
		DnsPumpTimeout: 1 * time.Second / time.Duration(200),
		// DnsReadTimeout: 1 * time.Second:
		DnsStateTimeout: 5 * time.Second,

		DnsMaxCombinePerAddress: 64 * 1024,
		DnsMaxCombine:           1024 * 1024 * 1024,

		DnsMaxPumpHostsPerAddress: 1024,
		DnsMaxPumpHosts:           1024 * 1024 * 1024,
	}
}

type PacketTranslationSettings struct {
	DnsTlds        [][]byte
	DnsAddr        *net.UDPAddr
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

	dnsClient      bool
	dnsPumpQueue   *pumpQueue
	dnsRequirePump bool

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

	for _, tld := range settings.DnsTlds {
		if tld[len(tld)-1] != '.' || tld[0] == '.' {
			return nil, fmt.Errorf("TLD must be canonical (end with a ., not start with a .): %s", string(tld))
		}
	}

	pt := &packetTranslation{
		ctx:        cancelCtx,
		cancel:     cancel,
		ptMode:     ptMode,
		packetConn: packetConn,
		settings:   settings,
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
	case PacketTranslationModeDecode53RequireDnsPump:
		pt.dnsClient = false
		pt.dnsPumpQueue = newPumpQueue(settings)
		pt.dnsRequirePump = true
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

				// fmt.Printf("WRITE ONE\n")

				tld := self.settings.DnsTlds[mathrand.Intn(len(self.settings.DnsTlds))]

				c := encodeDnsRequestCount(p.data, tld)

				// fmt.Printf("WRITE ONE (%d)\n", c)

				var header [18]byte
				mathrand.Read(header[0:16])
				header[16] = uint8(c)

				n := 0
				for i := 0; i < c; i += 1 {
					header[17] = uint8(i)

					m, packetData, err := encodeDnsRequest(id, header, p.data[n:], buf, tld)
					id += 1
					if err != nil {
						// drop the packet
						// FIXME glog
						// fmt.Printf("WRITE ONE ERR = %s\n", err)
						glog.Errorf("[pt]write err = %s", err)
						return nil
					}

					// fmt.Printf("PACKET WRITE TO: %v\n", string(packetData))

					_, err = self.packetConn.WriteTo(packetData, p.addr)
					if err != nil {
						return err
					}

					n += m
				}
				MessagePoolReturn(p.data)
				return nil
			}

			if self.ptMode == PacketTranslationModeDnsPump {
				select {
				case <-self.ctx.Done():
					return
				case p := <-self.out:
					// each write includes one pump header
					if err := writeOne(p); err != nil {
						glog.Errorf("[pt]write err = %s\n", err)
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

					// fmt.Printf("PACKET WRITE TO: %v\n", string(packetData))

					_, err = self.packetConn.WriteTo(packetData, self.settings.DnsAddr)
					if err != nil {
						glog.Errorf("[pt]write err = %s\n", err)
						return
					}
				}
			} else {
				select {
				case <-self.ctx.Done():
					return
				case p := <-self.out:
					if err := writeOne(p); err != nil {
						glog.Errorf("[pt]write err = %s\n", err)
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

				var pumpItems []*pumpItem
				if self.dnsRequirePump {
					pumpItems = self.dnsPumpQueue.RemoveLastN(p.addr, c)

					if pumpItems == nil {
						// drop the packet since there aren't enough pump headers
						continue
					}
				} else {

					pumpItems = make([]*pumpItem, c)

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
				}

				var header [18]byte
				mathrand.Read(header[0:16])
				header[16] = uint8(c)

				n := 0
				for i := 0; i < c; i += 1 {
					header[17] = uint8(i)

					item := pumpItems[i]

					m, packetData, err := encodeDnsResponse(
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

					// fmt.Printf("PACKET WRITE TO: %v\n", string(packetData))

					_, err = self.packetConn.WriteTo(packetData, p.addr)
					if err != nil {
						return
					}

					n += m
				}
				MessagePoolReturn(p.data)
			case p := <-self.forward:
				// fmt.Printf("PACKET WRITE TO: %v\n", string(p.data))

				_, err := self.packetConn.WriteTo(p.data, p.addr)
				MessagePoolReturn(p.data)
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

		// fmt.Printf("PACKET READ ONE\n")

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

			item := &pumpItem{
				addr:   addr,
				id:     id,
				header: header,
				tld:    tld,
			}

			self.dnsPumpQueue.Add(item)
			// if limit, drop the pump header but continue to process the packet
		}

		if c := uint8(header[16]); c == 0 {
			// just a pump
			continue
		}

		// dataCopy := make([]byte, len(data))
		// copy(dataCopy, data)
		dataCopy := MessagePoolCopy(data)

		out, limit, err := dnsCombineQueue.Combine(addr, header, dataCopy)

		if err != nil {
			// drop the packet
			// fmt.Printf("PACKET READ ONE DROP ERR = %s\n", err)
			glog.Errorf("[pt]read err = %s", err)
			continue
		}

		if limit {
			// drop the packet
			// fmt.Printf("PACKET READ ONE DROP LIMIT\n")
			continue
		}

		if out == nil {
			// packet not combined
			continue
		}

		// fmt.Printf("PACKET COMBINE ONE %s\n", string(out.data))

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

func (self *packetTranslation) WriteTo(packetData []byte, addr net.Addr) (n int, err error) {
	// packetDataCopy := make([]byte, len(packetData))
	// copy(packetDataCopy, packetData)
	packetDataCopy := MessagePoolCopy(packetData)

	p := &packet{
		data: packetDataCopy,
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
		MessagePoolReturn(p.data)
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

func (self *packetTranslation) SetReadBuffer(bytes int) error {
	conn, ok := self.packetConn.(interface{ SetReadBuffer(int) error })
	if !ok {
		return fmt.Errorf("Set read buffer not supporter on underlying packet conn: %T", self.packetConn)
	}
	return conn.SetReadBuffer(bytes)
}

func (self *packetTranslation) SetWriteBuffer(bytes int) error {
	conn, ok := self.packetConn.(interface{ SetWriteBuffer(int) error })
	if !ok {
		return fmt.Errorf("Set write buffer not supporter on underlying packet conn: %T", self.packetConn)
	}
	return conn.SetWriteBuffer(bytes)
}

// func keyFromHeader(header [18]byte) (key [17]byte) {
// 	key = [17]byte(header)
// 	return key
// }
