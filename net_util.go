package connect

import (
	"iter"
	"net"
	"net/netip"
	"sync"
)

func IpsInNet(ipNet *net.IPNet) iter.Seq[net.IP] {
	return func(yield func(net.IP) bool) {
		ip := ipNet.IP.Mask(ipNet.Mask)
		ones, bits := ipNet.Mask.Size()
		n := 0x01 << (bits - ones)

		// big endian counting from lowest bit
		for k := 0; k < n; k += 1 {
			j := len(ip) - 1
			ip[j] += 1
			// propagate the overflow bit up
			for ip[j] == 0 && 0 <= j-1 {
				j -= 1
				ip[j] += 1
			}

			ipCopy := make(net.IP, len(ip))
			copy(ipCopy, ip)

			if !yield(ipCopy) {
				return
			}
		}
	}
}

func AddrsInPrefix(prefix netip.Prefix) iter.Seq[netip.Addr] {
	return func(yield func(netip.Addr) bool) {
		a := prefix.Masked().Addr()
		ip := a.AsSlice()
		n := 0x01 << (a.BitLen() - prefix.Bits())

		// big endian counting from lowest bit
		for k := 0; k < n; k += 1 {
			j := len(ip) - 1
			ip[j] += 1
			// propagate the overflow bit up
			for ip[j] == 0 && 0 <= j-1 {
				j -= 1
				ip[j] += 1
			}

			addr, _ := netip.AddrFromSlice(ip)
			if !yield(addr) {
				return
			}
		}
	}
}

type AddrGenerator struct {
	next      func() (netip.Addr, bool)
	stop      func()
	stateLock sync.Mutex
}

func NewAddrGenerator(prefix netip.Prefix) *AddrGenerator {
	next, stop := iter.Pull(AddrsInPrefix(prefix))
	return &AddrGenerator{
		next: next,
		stop: stop,
	}
}

func (self *AddrGenerator) Next() (netip.Addr, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.next()
}

func (self *AddrGenerator) Close() {
	self.stop()
}
