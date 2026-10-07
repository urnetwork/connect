package connect

import (
	"encoding/base32"
	"encoding/base64"
	// "fmt"
	// mathrand "math/rand"
	"slices"

	// "golang.org/x/net/idna"
	"golang.org/x/net/dns/dnsmessage"
)

// The dns carrier's wire codec. A request is one standard query (opcode 0,
// no flags) with a single TXT question whose labels are the base32hex of
// the 18-byte pt header followed by packet bytes, under one of the carrier
// tlds, plus an edns0 opt record in the additional section. A response is an
// authoritative answer whose TXT records carry the base64 of the pt header
// and packet bytes, under a name that encodes the request header it is
// paired with, plus an opt record when the paired request carried one
// (RFC 6891 section 7).
//
// Until 2026-10 a request set the aa bit and carried a second "pump"
// question that only repeated the header, which RFC 9619 makes malformed
// (qdcount > 1) and which a dpi box can match on. The carrier has no version
// on the wire, so compatibility holds by construction instead: the old
// decoder never read the header flags or the additional section and
// discarded the pump question, so the new request decodes on it unchanged;
// and decodeDnsRequest keeps discarding a header-only question after the
// data question, so the old request decodes here. The opt record doubles as
// the capability signal: a request that carries one is answered with one.

// the edns0 udp payload size a request advertises and a response repeats.
// 1232 is the fragmentation-safe size the dns community settled on (dns flag
// day 2020) and the default of other dns carriers.
const dnsEdnsUdpPayloadByteCount = 1232

// returns the number of passes of encode needed for the packet and tld
func encodeDnsRequestCount(packet []byte, tld []byte) int {

	m := 157 - 18 - len(tld)

	c := len(packet) / m
	if len(packet)%m != 0 {
		c += 1
	}

	return c
}

// returns number of bytes read from packet, output buffer, error
func encodeDnsRequest(id uint16, header [18]byte, packet []byte, buf [1024]byte, tld []byte) (n int, out []byte, err error) {
	enc := base32.HexEncoding.WithPadding(base32.NoPadding)

	// a plain query: opcode 0 and no flags. aa is a response bit and rd stays
	// clear so the exchange reads as an iterative query to an authoritative
	// server, which is what the unchanged response (aa set, rd clear) answers.
	b := dnsmessage.NewBuilder(buf[:0], dnsmessage.Header{
		ID:     id,
		OpCode: 0,
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

	// the one question carries the header in its first label, which is all
	// the decoder pairs a response with. a pump request is this with no
	// packet bytes.
	err = b.Question(dnsmessage.Question{
		Name:  name,
		Type:  dnsmessage.TypeTXT,
		Class: dnsmessage.ClassINET,
	})
	if err != nil {
		return
	}

	err = b.StartAdditionals()
	if err != nil {
		return
	}
	err = b.OPTResource(dnsEdnsResourceHeader(), dnsmessage.OPTResource{})
	if err != nil {
		return
	}

	out, err = b.Finish()
	return
}

// the opt record both ends emit: root name, the advertised udp payload size,
// extended rcode 0, version 0, do clear.
func dnsEdnsResourceHeader() dnsmessage.ResourceHeader {
	var h dnsmessage.ResourceHeader
	h.SetEDNS0(dnsEdnsUdpPayloadByteCount, dnsmessage.RCodeSuccess, false)
	return h
}

// edns reports whether the request carried an opt record, which marks a
// client that expects one in the paired response. A request from before
// the single-question shape carries none.
func decodeDnsRequest(packet []byte, buf [1024]byte, tlds [][]byte) (id uint16, header [18]byte, out []byte, tld []byte, err error, otherData bool, edns bool) {
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
				// a TXT question outside every encoding tld is a forwarder
				// query, not a translation query (A6). Without this it fell
				// through as an empty translation packet and was dropped, so
				// the forwarder never saw it.
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
					// a header-only question after the data question is the
					// pump question of a request from before the
					// single-question shape; it repeats the header already
					// read, so ignore it. keep this while such clients exist.
					break
				}
				n += m
			}
		default:
			otherData = true
		}

	}

	if !otherData {
		edns, err = dnsAdditionalsHaveEdns(p)
		if err != nil {
			return
		}
	}

	if 18 <= n {
		header = [18]byte(out[0:18])
		out = out[18:n]
	}

	return
}

// reports whether the message's additional section holds an opt record. The
// parser must be positioned after the question section; the answer and
// authority sections are skipped, which a request leaves empty.
func dnsAdditionalsHaveEdns(p *dnsmessage.Parser) (edns bool, err error) {
	err = p.SkipAllAnswers()
	if err != nil {
		return
	}
	err = p.SkipAllAuthorities()
	if err != nil {
		return
	}
	for {
		var rh dnsmessage.ResourceHeader
		rh, err = p.AdditionalHeader()
		if err == dnsmessage.ErrSectionDone {
			err = nil
			return
		}
		if err != nil {
			return
		}
		if rh.Type == dnsmessage.TypeOPT {
			edns = true
		}
		err = p.SkipAdditional()
		if err != nil {
			return
		}
	}
}

// returns the number of passes of encode needed for the packet and tld
func encodeDnsResponseCount(packet []byte, tld []byte) int {

	m := 191 - 18 + 160 - len(tld)

	c := len(packet) / m
	if len(packet)%m != 0 {
		c += 1
	}

	return c
}

// https://www.iana.org/assignments/dns-parameters/dns-parameters.xhtml#dns-parameters-5

// returns number of bytes read from packet, output buffer, error.
// edns adds the opt record a request that carried one must be answered with;
// a response to a request without one is unchanged from before, so a client
// from before the single-question shape sees what it always saw.
func encodeDnsResponse(id uint16, pumpHeader [18]byte, header [18]byte, packet []byte, buf [1024]byte, tld []byte, edns bool) (n int, out []byte, err error) {
	enc := base32.StdEncoding.WithPadding(base64.NoPadding)
	rEnc := base64.StdEncoding.WithPadding(base64.NoPadding)

	b := dnsmessage.NewBuilder(buf[:0], dnsmessage.Header{
		ID:            id,
		Response:      true,
		Authoritative: true,
		RCode:         dnsmessage.RCodeSuccess,
	})
	b.EnableCompression()
	err = b.StartAnswers()
	if err != nil {
		return
	}

	name := dnsmessage.Name{}
	nameBuf := name.Data[:0]
	nameBuf = enc.AppendEncode(nameBuf, pumpHeader[:])
	nameBuf = append(nameBuf, []byte(".")...)
	nameBuf = append(nameBuf, tld...)
	name.Length = uint8(len(nameBuf))

	n = min(len(packet), 191-len(header)-len(tld))

	t := buf[512:512]
	t = rEnc.AppendEncode(t, header[:])
	t = rEnc.AppendEncode(t, packet[:n])

	err = b.TXTResource(
		dnsmessage.ResourceHeader{
			Name:  name,
			Type:  dnsmessage.TypeTXT,
			Class: dnsmessage.ClassINET,
			TTL:   0,
		},
		dnsmessage.TXTResource{
			TXT: []string{string(t)},
		},
	)
	if err != nil {
		return
	}

	if n < len(packet) {
		m := min(len(packet)-n, 160)

		t = buf[768:768]
		t = rEnc.AppendEncode(t, packet[n:n+m])

		err = b.TXTResource(
			dnsmessage.ResourceHeader{
				Name:  name,
				Type:  dnsmessage.TypeTXT,
				Class: dnsmessage.ClassINET,
				TTL:   0,
			},
			dnsmessage.TXTResource{
				TXT: []string{string(t)},
			},
		)
		if err != nil {
			return
		}

		n += m
	}

	if edns {
		err = b.StartAdditionals()
		if err != nil {
			return
		}
		err = b.OPTResource(dnsEdnsResourceHeader(), dnsmessage.OPTResource{})
		if err != nil {
			return
		}
	}

	out, err = b.Finish()
	// fmt.Printf("F (%d) %d = %s\n", n, len(out), string(out))
	return
}

// The client decoder reads the answers only, so a response with or without
// the opt record decodes the same; a server from before the single-question
// shape answers without one.
func decodeDnsResponse(packet []byte, buf [1024]byte, tlds [][]byte) (id uint16, pumpHeader [18]byte, header [18]byte, out []byte, err error) {
	enc := base32.StdEncoding.WithPadding(base32.NoPadding)
	rEnc := base64.StdEncoding.WithPadding(base64.NoPadding)

	p := &dnsmessage.Parser{}
	var h dnsmessage.Header
	h, err = p.Start(packet)
	if err != nil {
		// fmt.Printf("ERROR 0\n")
		return
	}
	id = h.ID

	p.SkipAllQuestions()

	var as []dnsmessage.Resource
	as, err = p.AllAnswers()
	if err != nil {
		// fmt.Printf("ERROR 1\n")
		return
	}

	out = buf[:]
	n := 0

	for _, a := range as {

		tld := func() []byte {
			for _, tld := range tlds {
				if len(tld) < int(a.Header.Name.Length) &&
					slices.Equal(tld, a.Header.Name.Data[int(a.Header.Name.Length)-len(tld):int(a.Header.Name.Length)]) &&
					a.Header.Name.Data[int(a.Header.Name.Length)-len(tld)-1] == '.' {
					return tld
				}
			}
			return nil
		}()
		if tld == nil {
			continue
		}

		var j int
		for j = 0; j < int(a.Header.Name.Length)-len(tld); j += 1 {
			if a.Header.Name.Data[j] == '.' {
				break
			}
		}

		_, err = enc.Decode(pumpHeader[0:], a.Header.Name.Data[0:j])
		if err != nil {
			return
		}

		switch a.Header.Type {
		case dnsmessage.TypeTXT:
			r := a.Body.(*dnsmessage.TXTResource)

			for _, txt := range r.TXT {
				var m int
				m, err = rEnc.Decode(out[n:], []byte(txt))
				if err != nil {
					// fmt.Printf("ERROR 3\n")
					return
				}
				n += m
			}
			// else ignore
		}

	}

	if 18 <= n {
		header = [18]byte(out[0:18])
		out = out[18:n]
	}

	return
}
