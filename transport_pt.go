


import (

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



type PacketTranslationSettings struct {

	DnsClient bool
	DnsTlds [][]byte 
	
}




type PACKET struct {
	Data []byte
	Addr netip.Addr
}


// implements PacketConn
type packetTranslation struct {
	ctx context.Context
	cancel context.CancelFunc
	mode PacketTranslationMode

	dnsPumpQueue *pumpQueue
	dnsCombineQueue *combineQueue

	packetConn PacketConnection
	in chan PACKET
	out chan PACKET
	forward chan PACKET
}



// FIXME separate encode and decode

// the caller should close in when done
func NewPacketTranslation(ctx context.Context, ptMode PacketTranslationMode, packetConn net.PacketConnection) (*packetTranslation, error) {
	cancelCtx, cancel := context.WithCancel(ctx)

	pt := &packetTranslation{}

	switch ptMode {
	case PacketTranslationModeDns, PacketTranslationModeDnsPump:
		dnsRequest = true
		go pt.encodeDns()
		go pt.decodeDns()
	case , PacketTranslationModeDecode53:
		dnsRequest = false
		go pt.encodeDns()
		go pt.decodeDns()
	default:
		return nil, fmt.Errorf("Unsupported packet translation mode: %s", ptMode)
	}

	return pt, nil
}


func (self *packetTranslation) encodeDns() {
	var id uint64
	for {
		if dnsRequest {
			writeOne := func(p PACKET) {
				c := encodeDnsRequestCount(self.dnsTld)

				var header [18]byte
				// [uuid][count][index]
				header[16] = uint8(c)

				n := 0
				for i := 0; i < c; i += 1 {
					header[17] = uint8(i)

					m, packet, err := encodeDnsRequest(id, header, p.Data[n:], buf, []byte(self.tld))
					if err != nil {
						// FIXME
					}

					err := self.packetConn.Write(packet, p.Addr)
					if err != nil {
						// FIXME
					}

					n += m
					id += 1
				}
			}

			if PUMP {
				select {
				case <- self.ctx.Done():
					return
				case p := <- self.out:
					writeOne(p)
				case <- Time.After(PumpTimeout):
					_, packet, err := encodeDnsRequest(id, header, make([]byte, 0), buf, []byte(self.tld))
					if err != nil {
						// FIXME
					}

					err := self.packetConn.Write(packet, p.Addr)
					if err != nil {
						// FIXME
					}

					id += 1

				}
			} else {
				select {
				case <- self.ctx.Done():
					return
				case p := <- self.out:
					writeOne(p)
				}
			}
		} else {

			select {
			case <- self.ctx.Done():
				return
			case p := <- self.out:

				c := encodeDnsResponseCount(p.Data, self.tld) 

				pumpHeaders := [][18]byte{}

				func() {
					LOCK()
					defer UNLOCK()

					pumpHosts := dnsPumpHosts[addr]

					m := min(c, len(pumpHosts))
					for range m {
						hosts = append(hosts, pumpHosts.RemoveLast())
					}
					if m < c {
						for range c - m {
							var header [18]byte
							mathrand.Read(header[0:16])
							hosts = append(hosts, header)
						}
					}


				}()

				var header [18]byte
				mathrand.Read(header[0:16])
				header[16] = uint8(c)
				
				n := 1
				for i := 0; i < c; i += 1 {
					header[17] = uint8(i)

					m, packet, err := encodeDnsResponse(id, pumpHeaders[i], header, p.Data[n:], buf, self.tld)
					if err != nil {
						// FIXME
					}
					
					err := self.packetConn.Write(packet, p.Addr)
					if err != nil {
						// FIXME
					}

					n += m
					id += 1
				}

			case p <- RESPONSEPACKETS:
				err := self.packetConn.Write(p.Data, p.Addr)
				if err != nil {
					// FIXME
				}


			}


		}
				
		
	}
}

func (self *packetTranslation) decodeDns() {
	for {
		self.impl.SetReadTimeout(X)
		packet, addr := self.impl.Read()

		// REMOVE STALE PACKETS FROM QUEUE
		// for each addr removed, clean up the dnsPumpHosts for that addr

		if TIMEOUT {
			continue
		}


		var header [18]byte
		var tld []byte


		if dnsRequest {
			// incoming are TXT records

			decodeDnsResponse()


		} else {

			// incoming are TXT requests

			_, header, out, tld, err, otherData := decodeDnsRequest()
			if otherData {
				// FIXME parse the other question types

				continue
			}



			func() {
				LOCK()
				defer UNLOCK()

				pumpHosts := dnsPumpHosts[addr]

				pumpHosts.Add(HEADERHOST)
				for _, extraHeaderHosts := range H {
					pumpHosts.Add(HEADERHOST)
				}
				for MaxPumpHostsPerAddress < len(pumpHosts) {
					pumpHosts.RemoveFirst()
				}
			}()
		}


		// decode header
		// decode content


		if NOHEADER && !dnsRequest {
			// record the pump hosts
			func() {
				LOCK()
				defer UNLOCK()

				pumpHosts := dnsPumpHosts[addr]

				for pumpHosts.TIME.Before(Fresh) {
					pumpHosts.RemoveFirst()
				}

				if MaxPumpHostsPerAddress <= len(pumpHosts) {
					// FIXME if pumpHosts head is still fresh, drop this new host
					//       since this is just churning through pumpHosts
				}

				for _, extraHeaderHosts := range H {
					pumpHosts.Add(HEADERHOST)
				}
				for MaxPumpHostsPerAddress < len(pumpHosts) {
					pumpHosts.RemoveFirst()
				}
			}()

			continue
		}


		if MaxCount < COUNT {
			// bad header, drop
		}
		if INDEX < 0 || COUNT <= INDEX {
			// bad header, drop
		}





		recombiner, ok := recombiners[header]
		if !ok {

			if MaxCountPerAddress < len(activeAddrRecombiners[addr]) {
				// to many per addr, drop
			}

			if MaxRecombinerCount < len(recombiners) {
				// too many active recombiners, drop
			}

			recombiner := make([][]byte, COUNT)
			recombiners[key] = recombiner
			activeAddrRecombiners[addr][key] = true
		}

		if COUNT != len(recombiner) {
			// header mismatch, drop
		}




		recombiner[INDEX] = CONTENT

		recombinerCount := 0
		recombinerByteCount := 0
		for _, p := range recombiner {
			if p != nil {
				recombinerCount += 1
				recombinerByteCount += 1
			}
		}
		if recombinerCount == COUNT {
			delete(recombiners, key)
			delete(activeAddrRecombiners[addr], key)

			QUEUE.Remove(recombiner)

			recombined := make([]byte, 0, recombinerByteCount)
			for _, p := range recombiner {
				recombined = append(recombined, p...)
			}

			select {
			case <- self.ctx.Done():
				return
			case self.in <- recombined:
			}
		} else {
			QUEUE.UpdateLastUpdate(now)
			QUEUE.Add(recombiner)
		}

	}
}



func WriteTo(packet []byte, Addr to) {
	


	select {
	case <- ctx.Done():
		return ERROR
	case self.out <- packet:
	}
}


func ReadPacket() {
	select {
	case <- ctx.Done():
		return ERROR
	case p := <- self.in:
		return p.Content, p.Addr
	}
}



// func keyFromHeader(header [18]byte) (key [17]byte) {
// 	key = [17]byte(header)
// 	return key
// }

type combineItem struct {
	// the header with the index zeroed out
	key [17]byte
	packets []PACKET
	updateAddr Addr
	updateTime time.Time
}

func newRecombinerItem(key [17]byte) *recombinerItem {
	c := uint8(key[16])
	return &recombinerItem{
		key: key,
		packets: make([][]byte, c),
	}
}


// ordered by update time
type combineQueue struct {

}


func RemoveOlder(minUpdateTime time.Time) {

}


func Combine(header [18]byte, packet *PACKET) (out *PACKET, limit bool, err error) {

	c := uint8(header[16])
	i := uint8(header[17])

	if c <= i {
		err = fmt.Errorf("index must be less than count %d <= %d", c, i)
		return
	}

	key := [17]byte(header)

	// FIXME combine

	// FIXME if miss check the limits




}



type pumpQueue struct {

}

func RemoveFirst(addr Addr) (header [18]byte, ok bool) {

}

func RemoveOlder(minUpdateTime time.Time) {

}

func Add(addr Addr, header [18]byte) (limit bool) {

}













