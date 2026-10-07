package connect

import (
	"encoding/base32"
	"slices"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

// Frozen copies of the dns carrier request codec as deployed before the
// single-question shape (connect origin/main 0a349da5, transport_pt_codec.go).
// They stand in for an old client and an old extender in the interop tests,
// so they must stay byte-for-byte what shipped: do not edit them when the
// live codec changes. legacyEncodeDnsRequest sets the aa bit, emits the data
// question and a second header-only "pump" question, and adds no opt record;
// legacyDecodeDnsRequest reads only the question section.

func legacyEncodeDnsRequest(id uint16, header [18]byte, packet []byte, buf [1024]byte, tld []byte) (n int, out []byte, err error) {
	enc := base32.HexEncoding.WithPadding(base32.NoPadding)

	b := dnsmessage.NewBuilder(buf[:0], dnsmessage.Header{
		ID:            id,
		OpCode:        0,
		Authoritative: true,
	})
	b.EnableCompression()
	err = b.StartQuestions()
	if err != nil {
		return
	}
	name := dnsmessage.Name{}

	nameBuf := name.Data[:0]

	m := min(len(packet), 157-len(header)-len(tld))

	copy(buf[len(buf)-30:], header[:])
	j := min(30-len(header), m)
	copy(buf[len(buf)-30+len(header):], packet[0:j])
	nameBuf = enc.AppendEncode(nameBuf, buf[len(buf)-30:len(buf)-30+len(header)+j])
	nameBuf = append(nameBuf, []byte(".")...)

	for n = j; n < m; n = j {
		j = min(n+30, m)
		nameBuf = enc.AppendEncode(nameBuf, packet[n:j])
		nameBuf = append(nameBuf, []byte(".")...)
	}

	nameBuf = append(nameBuf, tld...)
	name.Length = uint8(len(nameBuf))

	err = b.Question(dnsmessage.Question{
		Name:  name,
		Type:  dnsmessage.TypeTXT,
		Class: dnsmessage.ClassINET,
	})
	if err != nil {
		return
	}

	pumpName := dnsmessage.Name{}
	pumpNameBuf := pumpName.Data[:0]
	pumpNameBuf = enc.AppendEncode(pumpNameBuf, header[:])
	pumpNameBuf = append(pumpNameBuf, []byte(".")...)
	pumpNameBuf = append(pumpNameBuf, tld...)
	pumpName.Length = uint8(len(pumpNameBuf))
	err = b.Question(dnsmessage.Question{
		Name:  pumpName,
		Type:  dnsmessage.TypeTXT,
		Class: dnsmessage.ClassINET,
	})
	if err != nil {
		return
	}

	out, err = b.Finish()
	return
}

func legacyDecodeDnsRequest(packet []byte, buf [1024]byte, tlds [][]byte) (id uint16, header [18]byte, out []byte, tld []byte, err error, otherData bool) {
	enc := base32.HexEncoding.WithPadding(base32.NoPadding)

	p := &dnsmessage.Parser{}
	var h dnsmessage.Header
	h, err = p.Start(packet)
	if err != nil {
		return
	}
	id = h.ID

	out = buf[:]
	n := 0

	var qs []dnsmessage.Question
	qs, err = p.AllQuestions()
	if err != nil {
		return
	}
	for _, q := range qs {
		switch q.Type {
		case dnsmessage.TypeTXT:
			tld = func() []byte {
				for _, tld := range tlds {
					if len(tld) < int(q.Name.Length) &&
						slices.Equal(tld, q.Name.Data[int(q.Name.Length)-len(tld):int(q.Name.Length)]) &&
						q.Name.Data[int(q.Name.Length)-len(tld)-1] == '.' {
						return tld
					}
				}
				return nil
			}()
			if tld == nil {
				otherData = true
				continue
			}

			var j int
			for i := 0; i < int(q.Name.Length)-len(tld); i = j + 1 {
				for j = i; j < int(q.Name.Length)-len(tld); j += 1 {
					if q.Name.Data[j] == '.' {
						break
					}
				}

				var m int
				m, err = enc.Decode(out[n:], q.Name.Data[i:j])
				if err != nil {
					return
				}
				if 0 < n && i == 0 && m <= 18 {
					break
				}
				n += m
			}
		default:
			otherData = true
		}

	}

	if 18 <= n {
		header = [18]byte(out[0:18])
		out = out[18:n]
	}

	return
}

// dnsCarrierTestPacket fills a deterministic packet of the given size.
func dnsCarrierTestPacket(size int, seed byte) []byte {
	packet := make([]byte, size)
	for i := range packet {
		packet[i] = byte(i*7) ^ seed
	}
	return packet
}

// dnsCarrierTestHeader builds a pt header with a recognizable random part
// and the given fragment count and index.
func dnsCarrierTestHeader(seed byte, count uint8, index uint8) [18]byte {
	var header [18]byte
	for i := range 16 {
		header[i] = byte(0x10*i) ^ seed
	}
	header[16] = count
	header[17] = index
	return header
}

// A new client against an old extender: the single-question request must
// decode on the decoder that shipped before it, which never read the header
// flags or the additional section, with the same id, header, bytes and tld.
// This guards the question itself: a change to its first label or its tld
// placement would strand every deployed extender.
func TestDnsRequestNewShapeDecodesOnTheLegacyDecoder(t *testing.T) {
	var encodeBuf [1024]byte
	var decodeBuf [1024]byte

	tld := []byte("pt.example.")
	tlds := [][]byte{[]byte("other.example."), tld}
	packet := dnsCarrierTestPacket(1000, 0x5a)
	c := encodeDnsRequestCount(packet, tld)

	offset := 0
	for i := range c {
		header := dnsCarrierTestHeader(0x33, uint8(c), uint8(i))
		n, encoded, err := encodeDnsRequest(uint16(0x4000+i), header, packet[offset:], encodeBuf, tld)
		if err != nil {
			t.Fatalf("fragment %d: encode: %v", i, err)
		}
		if n <= 0 {
			t.Fatalf("fragment %d: encoded no bytes", i)
		}

		id, decodedHeader, decoded, decodedTld, err, otherData := legacyDecodeDnsRequest(encoded, decodeBuf, tlds)
		if err != nil {
			t.Fatalf("fragment %d: the legacy decoder rejected the new request: %v", i, err)
		}
		if otherData {
			t.Fatalf("fragment %d: the legacy decoder took the new request for a forwarder query", i)
		}
		if id != uint16(0x4000+i) {
			t.Fatalf("fragment %d: id = %#x, expected %#x", i, id, 0x4000+i)
		}
		if decodedHeader != header {
			t.Fatalf("fragment %d: header = %x, expected %x", i, decodedHeader, header)
		}
		if !slices.Equal(decodedTld, tld) {
			t.Fatalf("fragment %d: tld = %q, expected %q", i, decodedTld, tld)
		}
		if !slices.Equal(decoded, packet[offset:offset+n]) {
			t.Fatalf("fragment %d: the legacy decoder read different bytes", i)
		}
		offset += n
	}
	if offset != len(packet) {
		t.Fatalf("decoded %d bytes, expected %d", offset, len(packet))
	}

	// a pump request, header only
	header := dnsCarrierTestHeader(0x44, 0, 0)
	_, encoded, err := encodeDnsRequest(7, header, nil, encodeBuf, tld)
	if err != nil {
		t.Fatal(err)
	}
	id, decodedHeader, decoded, decodedTld, err, otherData := legacyDecodeDnsRequest(encoded, decodeBuf, tlds)
	if err != nil {
		t.Fatalf("the legacy decoder rejected the new pump request: %v", err)
	}
	if otherData || id != 7 || decodedHeader != header || len(decoded) != 0 || !slices.Equal(decodedTld, tld) {
		t.Fatalf("pump: id %d header %x data %d bytes tld %q other %t", id, decodedHeader, len(decoded), decodedTld, otherData)
	}
}
