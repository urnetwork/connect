package connect

import (
	"context"
	"fmt"
	"net"
	"slices"
	"time"
	// "runtime/debug"

	mathrand "math/rand"

	"golang.org/x/crypto/sha3"
	"golang.org/x/net/dns/dnsmessage"

	"github.com/urnetwork/glog/v2026"
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

// note the caller must set DnsTld
func DefaultPacketTranslationSettings() *PacketTranslationSettings {
	return &PacketTranslationSettings{
		DnsTlds: [][]byte{},
		// a good baseline is 100 pumps per second
		DnsPumpTimeout: 1 * time.Second / time.Duration(100),
		// DnsReadTimeout: 1 * time.Second:
		DnsStateTimeout: 5 * time.Second,

		DnsMaxCombinePerAddress: 64 * 1024,
		DnsMaxCombine:           64 * 1024 * 1024 * 1024,

		DnsMaxPumpHostsPerAddress: 2 * 1024,
		DnsMaxPumpHosts:           1024 * 1024 * 1024,

		WritePacketsPerSecond: 200,
		SequenceBufferSize:    32,
	}
}

type PacketTranslationSettings struct {
	DnsTlds [][]byte
	// DnsAddr        *net.UDPAddr
	DnsPumpTimeout time.Duration
	// DnsReadTimeout time.Duration
	DnsStateTimeout time.Duration

	DnsMaxCombinePerAddress int
	DnsMaxCombine           int64

	DnsMaxPumpHostsPerAddress int
	DnsMaxPumpHosts           int64

	WritePacketsPerSecond int
	SequenceBufferSize    int
}

type packet struct {
	data []byte
	addr net.Addr
}

// implements PacketConn
type packetTranslation struct {
	ctx    context.Context
	cancel context.CancelFunc

	ptMode       PacketTranslationMode
	packetConn   net.PacketConn
	headerPrefix []byte

	settings *PacketTranslationSettings

	dnsClient      bool
	dnsPumpQueue   *pumpQueue
	dnsRequirePump bool

	in      chan *packet
	out     chan *packet
	forward chan *packet

	readDeadline  time.Time
	writeDeadline time.Time
}

func NewPacketTranslation(
	ctx context.Context,
	ptMode PacketTranslationMode,
	packetConn net.PacketConn,
	settings *PacketTranslationSettings,
) (*packetTranslation, error) {
	return NewPacketTranslationWithPrefix(
		ctx,
		ptMode,
		packetConn,
		settings,
		nil,
	)
}

// the caller should close in when done
func NewPacketTranslationWithPrefix(
	ctx context.Context,
	ptMode PacketTranslationMode,
	packetConn net.PacketConn,
	settings *PacketTranslationSettings,
	headerPrefix []byte,
) (*packetTranslation, error) {
	cancelCtx, cancel := context.WithCancel(ctx)

	for _, tld := range settings.DnsTlds {
		if tld[len(tld)-1] != '.' || tld[0] == '.' {
			return nil, fmt.Errorf("TLD must be canonical (end with a ., not start with a .): %s", string(tld))
		}
	}

	pt := &packetTranslation{
		ctx: cancelCtx,
		cancel: func() {
			// debug.PrintStack()
			cancel()
		},
		ptMode:       ptMode,
		packetConn:   packetConn,
		headerPrefix: headerPrefix,
		settings:     settings,
		in:           make(chan *packet),
		out:          make(chan *packet),
		forward:      make(chan *packet),
	}

	switch ptMode {
	case PacketTranslationModeDns, PacketTranslationModeDnsPump:
		pt.dnsClient = true
		go HandleError(pt.encodeDns, cancel)
		go HandleError(pt.decodeDns, cancel)
	case PacketTranslationModeDecode53:
		pt.dnsClient = false
		pt.dnsPumpQueue = newPumpQueue(settings)
		go HandleError(pt.encodeDns, cancel)
		go HandleError(pt.decodeDns, cancel)
	case PacketTranslationModeDecode53RequireDnsPump:
		pt.dnsClient = false
		pt.dnsPumpQueue = newPumpQueue(settings)
		pt.dnsRequirePump = true
		go HandleError(pt.encodeDns, cancel)
		go HandleError(pt.decodeDns, cancel)
	default:
		return nil, fmt.Errorf("Unsupported packet translation mode: %s", ptMode)
	}

	return pt, nil
}

func (self *packetTranslation) newHeader() [18]byte {
	var header [18]byte
	if 0 < len(self.headerPrefix) {
		copy(header[0:len(self.headerPrefix)], self.headerPrefix)
		mathrand.Read(header[len(self.headerPrefix):16])
	} else {
		mathrand.Read(header[0:16])
	}
	return header
}

func (self *packetTranslation) newHeaderWithContentAddress(p []byte) [18]byte {
	var header [18]byte
	if 0 < len(self.headerPrefix) {
		copy(header[0:len(self.headerPrefix)], self.headerPrefix)
		sha3.ShakeSum128(header[len(self.headerPrefix):16], p)
	} else {
		sha3.ShakeSum128(header[0:16], p)
	}
	return header
}

func (self *packetTranslation) encodeDns() {
	defer self.cancel()

	var buf [1024]byte
	var id uint16
	var mostRecentAddr net.Addr
	for {
		if self.dnsClient {
			writeOne := func(p *packet) error {
				defer MessagePoolReturn(p.data)

				// fmt.Printf("WRITE ONE\n")

				tld := self.settings.DnsTlds[mathrand.Intn(len(self.settings.DnsTlds))]

				c := encodeDnsRequestCount(p.data, tld)

				// fmt.Printf("WRITE ONE (%d)\n", c)

				header := self.newHeaderWithContentAddress(p.data)
				header[16] = uint8(c)

				n := 0
				for i := 0; i < c; i += 1 {
					startTime := time.Now()

					header[17] = uint8(i)
					m, packetData, err := encodeDnsRequest(id, header, p.data[n:], buf, tld)
					id += 1
					n += m
					if err != nil {
						return err
					}

					// fmt.Printf("PACKET WRITE TO: %v\n", string(packetData))

					_, err = self.packetConn.WriteTo(packetData, p.addr)
					if err != nil {
						return err
					}
					endTime := time.Now()
					if 0 < self.settings.WritePacketsPerSecond {
						writeDuration := endTime.Sub(startTime)
						timeout := time.Second/time.Duration(self.settings.WritePacketsPerSecond) - writeDuration
						if 0 < timeout {
							randTimeout := time.Duration(mathrand.Intn(
								2*int(timeout/time.Nanosecond),
							)) * time.Nanosecond
							select {
							case <-time.After(randTimeout):
							case <-self.ctx.Done():
								return nil
							}
						}
					}

					// _, err = self.packetConn.WriteTo(packetData, p.addr)
					// if err != nil {
					// 	glog.Infof("[pt]write err = %s\n", err)
					// 	return err
					// }

					// glog.Infof("[pt]write raw\n")

				}
				if n != len(p.data) {
					return fmt.Errorf("Header count estimate incorrect.")
				}
				return nil
			}

			if self.ptMode == PacketTranslationModeDnsPump {
				select {
				case <-self.ctx.Done():
					return
				case p := <-self.out:
					// each write includes one pump header
					mostRecentAddr = p.addr
					if err := writeOne(p); err != nil {
						select {
						case <-self.ctx.Done():
						default:
							glog.Infof("[pt]write err = %s\n", err)
						}
						return
					}
				case <-time.After(self.settings.DnsPumpTimeout):
					// pump one header the server can use to repsond to
					if mostRecentAddr != nil {
						startTime := time.Now()

						tld := self.settings.DnsTlds[mathrand.Intn(len(self.settings.DnsTlds))]

						header := self.newHeader()

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

						_, err = self.packetConn.WriteTo(packetData, mostRecentAddr)
						if err != nil {
							select {
							case <-self.ctx.Done():
							default:
								glog.Infof("[pt]write err = %s\n", err)
							}
							return
						}
						endTime := time.Now()
						writeDuration := endTime.Sub(startTime)
						timeout := time.Second/time.Duration(self.settings.WritePacketsPerSecond) - writeDuration
						if 0 < timeout {
							randTimeout := time.Duration(mathrand.Intn(
								2*int(timeout/time.Nanosecond),
							)) * time.Nanosecond
							select {
							case <-time.After(randTimeout):
							case <-self.ctx.Done():
								return
							}
						}
					} else {
						glog.Infof("[pt]cannot pump dns due to missing most recent addr\n")
					}
				}
			} else {
				select {
				case <-self.ctx.Done():
					return
				case p := <-self.out:
					if err := writeOne(p); err != nil {
						select {
						case <-self.ctx.Done():
						default:
							glog.Infof("[pt]write err = %s\n", err)
						}
						return
					}
				}
			}
		} else {
			writeOne := func(p *packet) error {
				defer MessagePoolReturn(p.data)

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
						return nil
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
						header := self.newHeader()
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

				header := self.newHeaderWithContentAddress(p.data)
				header[16] = uint8(c)

				// on error, stop sending all since one is dropped
				n := 0
				for i := 0; i < c; i += 1 {
					startTime := time.Now()

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
					n += m
					if err != nil {
						return err
					}

					// fmt.Printf("PACKET WRITE TO: %v\n", string(packetData))

					_, err = self.packetConn.WriteTo(packetData, p.addr)
					if err != nil {
						return err
					}

					endTime := time.Now()
					if 0 < self.settings.WritePacketsPerSecond {
						writeDuration := endTime.Sub(startTime)
						timeout := time.Second/time.Duration(self.settings.WritePacketsPerSecond) - writeDuration
						if 0 < timeout {
							randTimeout := time.Duration(mathrand.Intn(
								2*int(timeout/time.Nanosecond),
							)) * time.Nanosecond
							select {
							case <-time.After(randTimeout):
							case <-self.ctx.Done():
								return nil
							}
						}
					}
				}
				if n != len(p.data) {
					return fmt.Errorf("Header count estimate incorrect.")
				}
				return nil
			}

			select {
			case <-self.ctx.Done():
				return
			case p := <-self.out:
				if err := writeOne(p); err != nil {
					select {
					case <-self.ctx.Done():
					default:
						glog.Infof("[pt]write err = %s\n", err)
					}
					return
				}
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

	type readData struct {
		addr   net.Addr
		header [18]byte
		data   []byte
		tld    []byte
	}

	readPipeline := make(chan *readData, self.settings.SequenceBufferSize)
	pumpPipeline := make(chan *pumpItem, self.settings.SequenceBufferSize)

	go HandleError(func() {
		defer self.cancel()

		dnsCombineQueue := newCombineQueue(self.settings)

		for {
			select {
			case <-self.ctx.Done():
				return
			case r := <-readPipeline:
				minUpdateTime := time.Now().Add(-self.settings.DnsStateTimeout)
				dnsCombineQueue.RemoveOlder(minUpdateTime)

				out, limit, err := dnsCombineQueue.Combine(r.addr, r.header, r.data)
				if err != nil {
					// drop the packet
					MessagePoolReturn(r.data)
					glog.Errorf("[pt]combine err = %s\n", err)
					continue
				}

				if limit {
					// drop the packet
					MessagePoolReturn(r.data)
					// fmt.Printf("PACKET READ ONE DROP LIMIT\n")
					glog.Errorf("[pt]combine limit\n")
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
	}, self.cancel)

	go HandleError(func() {
		defer self.cancel()

		for {
			select {
			case <-self.ctx.Done():
				return
			case item := <-pumpPipeline:
				minUpdateTime := time.Now().Add(-self.settings.DnsStateTimeout)
				self.dnsPumpQueue.RemoveOlder(minUpdateTime)
				// if limit, drop the pump header but continue to process the packet
				self.dnsPumpQueue.Add(item)
			}
		}
	}, self.cancel)

	packetData := make([]byte, 2048)
	var buf [1024]byte

	for {
		// self.packetConn.SetReadDeadline(time.Now().Add(self.settings.DnsReadTimeout))
		n, addr, err := self.packetConn.ReadFrom(packetData)
		if err != nil {
			select {
			case <-self.ctx.Done():
			default:
				glog.Infof("[pt]read err = %s\n", err)
			}
			return
		}
		// glog.Infof("[pt]read raw\n")

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
				// a normal non-pt dns request
				self.handleDnsOther(packetData[:n], addr)
				continue
			}

			if 0 < len(self.headerPrefix) && !slices.Equal(self.headerPrefix, header[0:len(self.headerPrefix)]) {
				// the header does not match the prefix, drop
				continue
			}

			if glog.V(2) {
				glog.Infof("[pt]decode one: %v, %v (%d/%d), (%d), %s, %s, %v\n", id, header, header[17], header[16], len(data), string(tld), err, otherData)
			}

			item := &pumpItem{
				addr:   addr,
				id:     id,
				header: header,
				tld:    tld,
			}

			select {
			case pumpPipeline <- item:
			case <-self.ctx.Done():
				return
			// if limit, drop the pump header but continue to process the packet
			default:
			}
		}

		if c := uint8(header[16]); c == 0 {
			// just a pump
			continue
		}

		// dataCopy := make([]byte, len(data))
		// copy(dataCopy, data)
		r := &readData{
			addr:   addr,
			header: header,
			data:   MessagePoolCopy(data),
			tld:    tld,
		}

		select {
		case readPipeline <- r:
		case <-self.ctx.Done():
			MessagePoolReturn(r.data)
			return
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

	if self.writeDeadline.IsZero() {
		select {
		case <-self.ctx.Done():
			err = fmt.Errorf("Done.")
			return
		case self.out <- p:
			n = len(packetData)
			glog.V(2).Infof("[pt]write packet\n")
			return
		}
	} else {
		select {
		case <-self.ctx.Done():
			err = fmt.Errorf("Done.")
			return
		case self.out <- p:
			n = len(packetData)
			glog.V(2).Infof("[pt]write packet\n")
			return
		case <-time.After(self.writeDeadline.Sub(time.Now())):
			err = fmt.Errorf("Timeout.")
			glog.Infof("[pt]write packet timeout\n")
			return
		}
	}
}

func (self *packetTranslation) ReadFrom(packetData []byte) (n int, addr net.Addr, err error) {
	if self.readDeadline.IsZero() {
		select {
		case <-self.ctx.Done():
			err = fmt.Errorf("Done.")
			return
		case p := <-self.in:
			addr = p.addr
			n = copy(packetData, p.data)
			MessagePoolReturn(p.data)
			glog.V(2).Infof("[pt]read packet\n")
			return
		}
	} else {
		select {
		case <-self.ctx.Done():
			err = fmt.Errorf("Done.")
			return
		case p := <-self.in:
			addr = p.addr
			n = copy(packetData, p.data)
			MessagePoolReturn(p.data)
			glog.V(2).Infof("[pt]read packet\n")
			return
		case <-time.After(self.readDeadline.Sub(time.Now())):
			err = fmt.Errorf("Timeout.")
			glog.Infof("[pt]read packet timeout\n")
			return
		}
	}
}

func (self *packetTranslation) LocalAddr() net.Addr {
	return self.packetConn.LocalAddr()
}

func (self *packetTranslation) SetDeadline(t time.Time) error {
	self.readDeadline = t
	self.writeDeadline = t
	return nil
}

func (self *packetTranslation) SetReadDeadline(t time.Time) error {
	self.readDeadline = t
	return nil
}

func (self *packetTranslation) SetWriteDeadline(t time.Time) error {
	self.writeDeadline = t
	return nil
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
