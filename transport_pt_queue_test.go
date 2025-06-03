package connect

import (
	"encoding/binary"
	mathrand "math/rand"
	// "slices"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"
)

func TestCombine(t *testing.T) {

	type part struct {
		header [18]byte
		data   []byte
	}

	consecutive := func(n int) []byte {
		out := make([]byte, 4*n)
		for i := range n {
			binary.BigEndian.PutUint32(out[4*i:], uint32(i))
		}
		return out
	}

	// generate all the parts
	// keep one part for each header separate
	// add all the parts from group 1 with a addr
	// add all the parts from group 2 with b addr
	// verify close on all with b addr
	// verify consecutive data on close

	for range 32 {

		as := []*part{}
		bs := []*part{}
		keyLens := map[[17]byte]int{}

		for range 1024 {
			m := 2 + mathrand.Intn(16)
			splits := make([]int, m)
			n := 0
			for i := range m {
				s := 16 + mathrand.Intn(128)
				splits[i] = s
				n += s
			}
			data := consecutive(n)

			var header [18]byte
			mathrand.Read(header[0:16])
			header[16] = uint8(m)
			keyLens[[17]byte(header[:])] = n

			parts := make([]*part, 0, m)
			for i := range m {
				p := &part{
					header: header,
					data:   data[:splits[i]*4],
				}
				p.header[17] = uint8(i)
				parts = append(parts, p)
				data = data[splits[i]*4:]
			}
			mathrand.Shuffle(len(parts), func(i int, j int) {
				parts[i], parts[j] = parts[j], parts[i]
			})

			as = append(as, parts[:m-1]...)
			bs = append(bs, parts[m-1])
		}

		mathrand.Shuffle(len(as), func(i int, j int) {
			as[i], as[j] = as[j], as[i]
		})
		mathrand.Shuffle(len(bs), func(i int, j int) {
			bs[i], bs[j] = bs[j], bs[i]
		})

		addrA := &net.UDPAddr{
			IP:   net.ParseIP("0.0.0.0"),
			Port: 8080,
		}
		addrB := &net.UDPAddr{
			IP:   net.ParseIP("0.0.0.1"),
			Port: 8081,
		}

		cq := newCombineQueue(DefaultPacketTranslationSettings())

		for _, a := range as {
			out, limit, err := cq.Combine(addrA, a.header, MessagePoolCopy(a.data))
			assert.Equal(t, err, nil)
			assert.Equal(t, limit, false)
			assert.Equal(t, out, nil)
		}

		for _, b := range bs {
			out, limit, err := cq.Combine(addrB, b.header, MessagePoolCopy(b.data))
			assert.Equal(t, err, nil)
			assert.Equal(t, limit, false)

			assert.Equal(t, out.addr, addrB)
			m := keyLens[[17]byte(b.header[:])]
			assert.Equal(t, len(out.data), 4*m)
			assert.Equal(t, out.data, consecutive(m))
		}
	}

}

func TestCombineTrim(t *testing.T) {

	settings := DefaultPacketTranslationSettings()
	settings.DnsMaxCombine = 1024 * 1024
	settings.DnsMaxCombinePerAddress = 1024
	cq := newCombineQueue(settings)

	m := 128

	batchTimes := make([]time.Time, m)
	batchCounts := make([]int, m)

	for i := range m {
		n := 128 + mathrand.Intn(1024)
		batchCounts[i] = n

		for range n {
			var header [18]byte
			mathrand.Read(header[0:16])
			c := 8 + mathrand.Intn(128)
			header[16] = uint8(c)
			header[17] = uint8(mathrand.Intn(c))

			addr := &net.UDPAddr{
				IP:   net.ParseIP(fmt.Sprintf("%d.%d.%d.%d", mathrand.Intn(256), mathrand.Intn(256), mathrand.Intn(256), mathrand.Intn(256))),
				Port: 8080 + mathrand.Intn(1024),
			}
			data := make([]byte, 16+mathrand.Intn(1024))
			mathrand.Read(data)
			out, limit, err := cq.Combine(addr, header, data)
			assert.Equal(t, err, nil)
			assert.Equal(t, limit, false)
			assert.Equal(t, out, nil)
		}

		batchTimes[i] = time.Now()
		select {
		case <-time.After(time.Duration(8+mathrand.Intn(32)) * time.Millisecond):
		}
	}

	for i := range m {
		c := 0
		for _, n := range batchCounts[i:] {
			c += n
		}
		assert.Equal(t, cq.Len(), c)

		cq.RemoveOlder(batchTimes[i])
	}

	assert.Equal(t, cq.Len(), 0)

	// pump beyond the limits
	for i := range 4 * settings.DnsMaxCombine {
		var header [18]byte
		mathrand.Read(header[0:16])
		c := 8 + mathrand.Intn(128)
		header[16] = uint8(c)
		header[17] = uint8(mathrand.Intn(c))

		addr := &net.UDPAddr{
			IP:   net.ParseIP(fmt.Sprintf("%d.%d.%d.%d", mathrand.Intn(256), mathrand.Intn(256), mathrand.Intn(256), mathrand.Intn(256))),
			Port: 8080 + mathrand.Intn(1024),
		}
		data := make([]byte, 16+mathrand.Intn(1024))
		mathrand.Read(data)
		out, limit, err := cq.Combine(addr, header, data)
		assert.Equal(t, err, nil)
		assert.Equal(t, limit, settings.DnsMaxCombine <= i)
		assert.Equal(t, out, nil)
	}
	assert.Equal(t, cq.Len(), settings.DnsMaxCombine)

}

func TestPump(t *testing.T) {
	settings := DefaultPacketTranslationSettings()
	pq := newPumpQueue(settings)

	addr := &net.UDPAddr{
		IP:   net.ParseIP("0.0.0.0"),
		Port: 8080,
	}

	added := []*pumpItem{}

	for i := range settings.DnsMaxPumpHostsPerAddress {
		var header [18]byte
		mathrand.Read(header[0:16])
		header[16] = uint8(1)
		header[17] = uint8(0)
		tld := []byte(fmt.Sprintf("foo%d.com", i))
		item := &pumpItem{
			addr:   addr,
			id:     uint16(i),
			header: header,
			tld:    tld,
		}
		limit := pq.Add(item)
		assert.Equal(t, limit, false)
		added = append(added, item)
	}

	for j := range 32 {
		otherAddr := &net.UDPAddr{
			IP:   net.ParseIP("0.0.0.0"),
			Port: 8081 + j,
		}
		lastItem := pq.RemoveLast(otherAddr)
		assert.Equal(t, lastItem, nil)
	}

	for i := range settings.DnsMaxPumpHostsPerAddress {
		var header [18]byte
		mathrand.Read(header[0:16])
		header[16] = uint8(1)
		header[17] = uint8(0)
		tld := []byte(fmt.Sprintf("foo%d.com", i))
		item := &pumpItem{
			addr:   addr,
			id:     uint16(i),
			header: header,
			tld:    tld,
		}
		limit := pq.Add(item)
		assert.Equal(t, limit, true)
		// not added
	}

	for i := len(added) - 1; 0 <= i; i -= 1 {
		item := added[i]
		lastItem := pq.RemoveLast(addr)
		assert.Equal(t, lastItem.addr, item.addr)
		assert.Equal(t, lastItem.id, item.id)
		assert.Equal(t, lastItem.header, item.header)
		assert.Equal(t, lastItem.tld, item.tld)
	}

	// add n random items
	// remove n and verify the items are in reverse order of added

	// add items more than the pump limit
	// test that the limit flag is set
	// remove all and verify up to the limit is returned in reverse order
}

func TestPumpTrim(t *testing.T) {

	settings := DefaultPacketTranslationSettings()
	settings.DnsMaxPumpHosts = 1024 * 1024
	settings.DnsMaxPumpHostsPerAddress = 1024
	pq := newPumpQueue(settings)

	m := 128

	batchTimes := make([]time.Time, m)
	batchCounts := make([]int, m)

	for i := range m {
		n := 128 + mathrand.Intn(1024)
		batchCounts[i] = n

		for range n {
			var header [18]byte
			mathrand.Read(header[0:16])
			c := 1 + mathrand.Intn(128)
			header[16] = uint8(c)
			header[17] = uint8(mathrand.Intn(c))
			tld := []byte(fmt.Sprintf("foo%d.com", mathrand.Intn(1024)))

			addr := &net.UDPAddr{
				IP:   net.ParseIP(fmt.Sprintf("%d.%d.%d.%d", mathrand.Intn(256), mathrand.Intn(256), mathrand.Intn(256), mathrand.Intn(256))),
				Port: 8080 + mathrand.Intn(1024),
			}
			item := &pumpItem{
				addr:   addr,
				id:     uint16(mathrand.Intn(32 * 1024 * 1024)),
				header: header,
				tld:    tld,
			}
			limit := pq.Add(item)
			assert.Equal(t, limit, false)
		}

		batchTimes[i] = time.Now()
		select {
		case <-time.After(time.Duration(8+mathrand.Intn(32)) * time.Millisecond):
		}
	}

	for i := range m {
		c := 0
		for _, n := range batchCounts[i:] {
			c += n
		}
		assert.Equal(t, pq.Len(), c)

		pq.RemoveOlder(batchTimes[i])
	}

	assert.Equal(t, pq.Len(), 0)

	// pump beyond the limits
	for i := range 4 * settings.DnsMaxPumpHosts {
		var header [18]byte
		mathrand.Read(header[0:16])
		header[16] = uint8(1 + mathrand.Intn(128))
		header[17] = uint8(mathrand.Intn(128))
		tld := []byte(fmt.Sprintf("foo%d.com", mathrand.Intn(1024)))

		addr := &net.UDPAddr{
			IP:   net.ParseIP(fmt.Sprintf("%d.%d.%d.%d", mathrand.Intn(256), mathrand.Intn(256), mathrand.Intn(256), mathrand.Intn(256))),
			Port: 8080 + mathrand.Intn(1024),
		}
		item := &pumpItem{
			addr:   addr,
			id:     uint16(mathrand.Intn(32 * 1024 * 1024)),
			header: header,
			tld:    tld,
		}
		limit := pq.Add(item)
		// fmt.Printf("[%d]\n", i)
		assert.Equal(t, limit, settings.DnsMaxPumpHosts <= i)
	}
	assert.Equal(t, pq.Len(), settings.DnsMaxPumpHosts)
}
