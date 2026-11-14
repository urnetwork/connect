package connect

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha512"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/hex"
	"io"
	"math/big"
	"net"
	"slices"
	"testing"
	"time"
)

// A reality link with every parameter this client keeps reads into the
// configuration and renders back to a link that reads the same.
func TestVlessLinkRoundTripsReality(t *testing.T) {
	publicKey := bytes.Repeat([]byte{0x42}, 32)
	link := "vless://" + testVlessUserId + "@203.0.113.10:443?encryption=none&flow=xtls-rprx-vision" +
		"&type=tcp&security=reality&sni=www.cover.example&fp=chrome&pbk=" + EncodeVlessPublicKey(publicKey) +
		"&sid=6ba85179e30d4fc2&spx=%2Fcrawl#home%20server"
	config, err := ParseVlessLink(link)
	if err != nil {
		t.Fatal(err)
	}
	expected := &VlessConfig{
		Name:        "home server",
		Address:     "203.0.113.10",
		Port:        443,
		Id:          testVlessUserId,
		Flow:        VlessFlowVision,
		Network:     VlessNetworkTcp,
		Security:    VlessSecurityReality,
		ServerName:  "www.cover.example",
		Fingerprint: "chrome",
		PublicKey:   publicKey,
		ShortId:     []byte{0x6b, 0xa8, 0x51, 0x79, 0xe3, 0x0d, 0x4f, 0xc2},
		SpiderX:     "/crawl",
	}
	assertVlessConfigEqual(t, config, expected)
	again, err := ParseVlessLink(config.Link())
	if err != nil {
		t.Fatalf("rendered link %q: %s", config.Link(), err)
	}
	assertVlessConfigEqual(t, again, expected)
}

// A ws link over tls and an httpupgrade link without security keep their path,
// host, alpn and the insecure flag.
func TestVlessLinkRoundTripsHttpTransports(t *testing.T) {
	cases := []struct {
		link     string
		expected *VlessConfig
	}{
		{
			link: "vless://" + testVlessUserId + "@cdn.example:8443?type=ws&security=tls&path=%2Fws%3Fed%3D2048" +
				"&host=front.example&sni=front.example&alpn=h2,http/1.1&allowInsecure=1&fp=firefox",
			expected: &VlessConfig{
				Address:       "cdn.example",
				Port:          8443,
				Id:            testVlessUserId,
				Network:       VlessNetworkWs,
				Security:      VlessSecurityTls,
				Path:          "/ws?ed=2048",
				Host:          "front.example",
				ServerName:    "front.example",
				Alpns:         []string{"h2", "http/1.1"},
				AllowInsecure: true,
				Fingerprint:   "firefox",
			},
		},
		{
			link: "vless://" + testVlessUserId + "@[2001:db8::7]:80?type=httpupgrade&path=%2Fup&host=up.example",
			expected: &VlessConfig{
				Address:  "2001:db8::7",
				Port:     80,
				Id:       testVlessUserId,
				Network:  VlessNetworkHttpUpgrade,
				Security: VlessSecurityNone,
				Path:     "/up",
				Host:     "up.example",
			},
		},
		{
			// a bare link is raw tcp without security, and "raw" names tcp
			link: "vless://example@192.0.2.1:1080?type=raw",
			expected: &VlessConfig{
				Address:  "192.0.2.1",
				Port:     1080,
				Id:       "example",
				Network:  VlessNetworkTcp,
				Security: VlessSecurityNone,
			},
		},
	}
	for _, c := range cases {
		config, err := ParseVlessLink(c.link)
		if err != nil {
			t.Fatalf("%s: %s", c.link, err)
		}
		assertVlessConfigEqual(t, config, c.expected)
		again, err := ParseVlessLink(config.Link())
		if err != nil {
			t.Fatalf("rendered link %q: %s", config.Link(), err)
		}
		assertVlessConfigEqual(t, again, c.expected)
	}
}

func assertVlessConfigEqual(t *testing.T, config *VlessConfig, expected *VlessConfig) {
	t.Helper()
	if config.Name != expected.Name ||
		config.Address != expected.Address ||
		config.Port != expected.Port ||
		config.Id != expected.Id ||
		config.Flow != expected.Flow ||
		config.Network != expected.Network ||
		config.Security != expected.Security ||
		config.ServerName != expected.ServerName ||
		config.Fingerprint != expected.Fingerprint ||
		!slices.Equal(config.Alpns, expected.Alpns) ||
		config.AllowInsecure != expected.AllowInsecure ||
		!bytes.Equal(config.PublicKey, expected.PublicKey) ||
		!bytes.Equal(config.ShortId, expected.ShortId) ||
		config.SpiderX != expected.SpiderX ||
		config.Path != expected.Path ||
		config.Host != expected.Host {
		t.Fatalf("config = %+v, expected %+v", config, expected)
	}
}

// Each kind of bad link is refused with the code a user can act on, and a
// feature this client does not implement is never read as something else.
func TestVlessLinkErrors(t *testing.T) {
	publicKey := EncodeVlessPublicKey(bytes.Repeat([]byte{0x42}, 32))
	cases := []struct {
		link string
		code string
	}{
		{link: "", code: VlessErrorLinkInvalid},
		{link: "https://" + testVlessUserId + "@vless.example:443", code: VlessErrorLinkInvalid},
		{link: "vless://vless.example:443", code: VlessErrorLinkInvalid},
		{link: "vless://" + testVlessUserId + "@vless.example:443/extra", code: VlessErrorLinkInvalid},
		{link: "vless://" + testVlessUserId + "@:443", code: VlessErrorAddressInvalid},
		{link: "vless://" + testVlessUserId + "@vless.example", code: VlessErrorPortInvalid},
		{link: "vless://" + testVlessUserId + "@vless.example:0", code: VlessErrorPortInvalid},
		{link: "vless://" + testVlessUserId + "@vless.example:70000", code: VlessErrorPortInvalid},
		{link: "vless://@vless.example:443", code: VlessErrorIdInvalid},
		{link: "vless://this-id-is-longer-than-thirty-bytes@vless.example:443", code: VlessErrorIdInvalid},
		{link: "vless://" + testVlessUserId + "@vless.example:443?type=grpc&serviceName=x", code: VlessErrorNetworkUnsupported},
		{link: "vless://" + testVlessUserId + "@vless.example:443?type=xhttp", code: VlessErrorNetworkUnsupported},
		{link: "vless://" + testVlessUserId + "@vless.example:443?security=xtls", code: VlessErrorSecurityUnsupported},
		{link: "vless://" + testVlessUserId + "@vless.example:443?encryption=mlkem768x25519plus.native.0rtt.x", code: VlessErrorLinkUnsupported},
		{link: "vless://" + testVlessUserId + "@vless.example:443?type=tcp&headerType=http", code: VlessErrorLinkUnsupported},
		{link: "vless://" + testVlessUserId + "@vless.example:443?flow=xtls-rprx-direct&security=tls", code: VlessErrorFlowInvalid},
		{link: "vless://" + testVlessUserId + "@vless.example:443?flow=xtls-rprx-vision", code: VlessErrorFlowInvalid},
		{link: "vless://" + testVlessUserId + "@vless.example:443?flow=xtls-rprx-vision&type=ws&security=tls", code: VlessErrorFlowInvalid},
		{link: "vless://" + testVlessUserId + "@vless.example:443?security=tls&fp=netscape", code: VlessErrorFingerprintUnsupported},
		{link: "vless://" + testVlessUserId + "@vless.example:443?security=reality&pbk=" + publicKey, code: VlessErrorServerNameRequired},
		{link: "vless://" + testVlessUserId + "@vless.example:443?security=reality&sni=cover.example&pbk=short", code: VlessErrorPublicKeyInvalid},
		{link: "vless://" + testVlessUserId + "@vless.example:443?security=reality&sni=cover.example&pbk=" + publicKey + "&sid=0123456789abcdef01", code: VlessErrorShortIdInvalid},
		{link: "vless://" + testVlessUserId + "@vless.example:443?security=reality&sni=cover.example&pbk=" + publicKey + "&sid=xyz", code: VlessErrorShortIdInvalid},
	}
	for _, c := range cases {
		config, err := ParseVlessLink(c.link)
		if code := VlessConfigErrorCode(err); code != c.code {
			t.Errorf("%q: code = %q (config %+v), expected %q", c.link, code, config, c.code)
		}
	}
}

// A uuid reads as written, with or without its dashes; other short text maps
// to the version 5 uuid of the zero namespace, as Xray maps a custom id.
func TestVlessIdMapping(t *testing.T) {
	cases := []struct {
		id       string
		expected string
	}{
		{id: testVlessUserId, expected: testVlessUserId},
		{id: "5783a3e7e37351cd8642c83782b807c5", expected: testVlessUserId},
		{id: "example", expected: "feb54431-301b-52bb-a6dd-e1e93e81bb9e"},
		{id: "urnetwork-test", expected: "4f15729f-a0f4-55aa-946f-fc7cd074dcb5"},
	}
	for _, c := range cases {
		uuid, err := vlessId(c.id)
		if err != nil {
			t.Fatalf("%q: %s", c.id, err)
		}
		if got := Id(uuid).String(); got != c.expected {
			t.Errorf("%q maps to %s, expected %s", c.id, got, c.expected)
		}
	}
	for _, id := range []string{"", "5783a3e7-e373-51cd-8642-c83782b807cz", "5783a3e7-e373-51cd-8642-c83782b807c5-", "0123456789012345678901234567890"} {
		if _, err := vlessId(id); err == nil {
			t.Errorf("%q is not a valid id", id)
		}
	}
}

// The request header for each address type, with and without the vision
// flow, byte for byte.
func TestVlessRequestHeader(t *testing.T) {
	userId, err := vlessId(testVlessUserId)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		flow     string
		host     string
		port     uint16
		expected string
	}{
		{host: "api.example", port: 443, expected: "00" + "5783a3e7e37351cd8642c83782b807c5" + "00" + "01" + "01bb" + "02" + "0b" + hex.EncodeToString([]byte("api.example"))},
		{host: "192.0.2.5", port: 8080, expected: "00" + "5783a3e7e37351cd8642c83782b807c5" + "00" + "01" + "1f90" + "01" + "c0000205"},
		{host: "::ffff:192.0.2.5", port: 80, expected: "00" + "5783a3e7e37351cd8642c83782b807c5" + "00" + "01" + "0050" + "01" + "c0000205"},
		{host: "2001:db8::1", port: 443, expected: "00" + "5783a3e7e37351cd8642c83782b807c5" + "00" + "01" + "01bb" + "03" + "20010db8000000000000000000000001"},
		{
			flow:     VlessFlowVision,
			host:     "api.example",
			port:     443,
			expected: "00" + "5783a3e7e37351cd8642c83782b807c5" + "12" + "0a10" + hex.EncodeToString([]byte("xtls-rprx-vision")) + "01" + "01bb" + "02" + "0b" + hex.EncodeToString([]byte("api.example")),
		},
	}
	for _, c := range cases {
		header, err := vlessRequestHeader(userId, c.flow, c.host, c.port)
		if err != nil {
			t.Fatal(err)
		}
		if got := hex.EncodeToString(header); got != c.expected {
			t.Errorf("%s %s:%d header = %s, expected %s", c.flow, c.host, c.port, got, c.expected)
		}
	}
	if _, err := vlessRequestHeader(userId, "", string(bytes.Repeat([]byte("a"), 256)), 443); err == nil {
		t.Errorf("a domain longer than 255 bytes cannot be carried")
	}
}

// The request header goes out with the first write and the response header is
// read off before the first read; a read before any write sends the header
// alone first.
func TestVlessConnFraming(t *testing.T) {
	userId, err := vlessId(testVlessUserId)
	if err != nil {
		t.Fatal(err)
	}
	header, err := vlessRequestHeader(userId, "", "api.example", 443)
	if err != nil {
		t.Fatal(err)
	}

	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	conn := newVlessConn(clientConn, header)
	defer conn.Close()

	go func() {
		conn.Write([]byte("hello"))
	}()
	received := make([]byte, len(header)+5)
	if _, err := io.ReadFull(serverConn, received); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(received, append(append([]byte{}, header...), "hello"...)) {
		t.Fatalf("first packet = %x, expected the header and the data together", received)
	}
	go func() {
		// a response with two addon bytes, which the client skips
		serverConn.Write([]byte{0, 2, 0xaa, 0xbb, 'o', 'k'})
	}()
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatal(err)
	}
	if string(reply) != "ok" {
		t.Fatalf("reply = %q", reply)
	}

	readFirstClient, readFirstServer := net.Pipe()
	defer readFirstServer.Close()
	readFirst := newVlessConn(readFirstClient, header)
	defer readFirst.Close()
	readDone := make(chan error, 1)
	go func() {
		b := make([]byte, 1)
		_, err := readFirst.Read(b)
		readDone <- err
	}()
	received = make([]byte, len(header))
	if _, err := io.ReadFull(readFirstServer, received); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(received, header) {
		t.Fatalf("header sent alone = %x", received)
	}
	readFirstServer.Write([]byte{0, 0, 'x'})
	if err := <-readDone; err != nil {
		t.Fatal(err)
	}

	badClient, badServer := net.Pipe()
	defer badServer.Close()
	bad := newVlessConn(badClient, header)
	defer bad.Close()
	go func() {
		io.ReadFull(badServer, make([]byte, len(header)))
		badServer.Write([]byte{1, 0})
	}()
	if _, err := bad.Read(make([]byte, 1)); err == nil {
		t.Fatalf("a response of another version must fail the read")
	}
}

// The record conn hands the tls layer one record at a time and leaves every
// byte after the record on the socket until direct, so the raw bytes a vision
// server sends after its direct command are never swallowed.
func TestVlessRecordConnNeverReadsPastARecord(t *testing.T) {
	record1 := append([]byte{0x17, 0x03, 0x03, 0x00, 0x04}, "abcd"...)
	record2 := append([]byte{0x17, 0x03, 0x03, 0x00, 0x02}, "ef"...)
	raw := []byte("direct bytes")
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	go func() {
		serverConn.Write(append(append(append([]byte{}, record1...), record2...), raw...))
	}()
	recordConn := newVlessRecordConn(clientConn)
	defer recordConn.Close()

	b := make([]byte, 4096)
	n, err := recordConn.Read(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b[:n], record1) {
		t.Fatalf("first read = %x, expected exactly the first record", b[:n])
	}
	// a short buffer gets the record in pieces, still never more
	n, err = recordConn.Read(b[:3])
	if err != nil {
		t.Fatal(err)
	}
	rest := make([]byte, 64)
	m, err := recordConn.Read(rest)
	if err != nil {
		t.Fatal(err)
	}
	if got := append(append([]byte{}, b[:n]...), rest[:m]...); !bytes.Equal(got, record2) {
		t.Fatalf("second record = %x", got)
	}
	recordConn.setDirect()
	direct := make([]byte, len(raw))
	if _, err := io.ReadFull(recordConn, direct); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(direct, raw) {
		t.Fatalf("direct bytes = %q", direct)
	}
}

// The client pads its direction from the first write -- the user id leads,
// the tls client hello is long padded -- and ends the padding with the end
// command at the first application data record, never with direct.
func TestVlessVisionUplinkPadding(t *testing.T) {
	userId, err := vlessId(testVlessUserId)
	if err != nil {
		t.Fatal(err)
	}
	header, err := vlessRequestHeader(userId, VlessFlowVision, "api.example", 443)
	if err != nil {
		t.Fatal(err)
	}
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	conn := newVlessVisionConn(clientConn, newVlessRecordConn(clientConn), userId, header)
	defer conn.Close()

	clientHello := append([]byte{0x16, 0x03, 0x01, 0x00, 0x10, 0x01}, bytes.Repeat([]byte{0x33}, 15)...)
	handshake := append([]byte{0x14, 0x03, 0x03, 0x00, 0x01, 0x01}, 0x17, 0x03, 0x03, 0x00, 0x01, 0x44)
	applicationData := append([]byte{0x17, 0x03, 0x03, 0x00, 0x03}, "GET"...)
	afterwards := append([]byte{0x17, 0x03, 0x03, 0x00, 0x02}, "OK"...)
	writes := [][]byte{clientHello, handshake, applicationData, afterwards}

	type block struct {
		command byte
		content []byte
		padding int
	}
	blocks := make(chan block, 8)
	plain := make(chan []byte, 1)
	go func() {
		defer close(blocks)
		received := make([]byte, len(header)+16)
		if _, err := io.ReadFull(serverConn, received); err != nil {
			return
		}
		if !bytes.Equal(received[:len(header)], header) || !bytes.Equal(received[len(header):], userId[:]) {
			t.Errorf("the first write must be the header and the user id")
			return
		}
		for {
			head := make([]byte, 5)
			if _, err := io.ReadFull(serverConn, head); err != nil {
				return
			}
			contentLength := int(head[1])<<8 | int(head[2])
			paddingLength := int(head[3])<<8 | int(head[4])
			content := make([]byte, contentLength)
			io.ReadFull(serverConn, content)
			io.CopyN(io.Discard, serverConn, int64(paddingLength))
			blocks <- block{command: head[0], content: content, padding: paddingLength}
			if head[0] != vlessVisionCommandContinue {
				break
			}
		}
		rest := make([]byte, len(afterwards))
		io.ReadFull(serverConn, rest)
		plain <- rest
	}()
	for _, write := range writes {
		if _, err := conn.Write(write); err != nil {
			t.Fatal(err)
		}
	}
	var got []block
	for b := range blocks {
		got = append(got, b)
	}
	if len(got) != 3 {
		t.Fatalf("blocks = %d, expected the hello, the handshake and the first application data", len(got))
	}
	for i, write := range writes[:3] {
		if !bytes.Equal(got[i].content, write) {
			t.Errorf("block %d content = %x, expected %x", i, got[i].content, write)
		}
	}
	if got[0].command != vlessVisionCommandContinue || got[1].command != vlessVisionCommandContinue || got[2].command != vlessVisionCommandEnd {
		t.Errorf("commands = %d %d %d, expected continue, continue, end", got[0].command, got[1].command, got[2].command)
	}
	// long padding brings a short handshake block up to at least 900 bytes
	if total := len(got[0].content) + got[0].padding; total < vlessVisionLongPaddingBase {
		t.Errorf("hello block is %d bytes with padding, expected at least %d", total, vlessVisionLongPaddingBase)
	}
	if rest := <-plain; !bytes.Equal(rest, afterwards) {
		t.Errorf("the write after the padding = %x, expected it plain", rest)
	}
}

// The downlink parser across block and read boundaries: content is returned,
// padding is dropped, and end switches to plain reads.
func TestVlessVisionDownlinkUnpadding(t *testing.T) {
	userId, err := vlessId(testVlessUserId)
	if err != nil {
		t.Fatal(err)
	}
	conn := newVlessVisionConn(nil, nil, userId, nil)
	stream := []byte{}
	stream = append(stream, userId[:]...)
	stream = append(stream, vlessVisionCommandContinue, 0, 3, 0, 2)
	stream = append(stream, "abc"...)
	stream = append(stream, 0, 0)
	stream = append(stream, vlessVisionCommandEnd, 0, 2, 0, 1)
	stream = append(stream, "de"...)
	stream = append(stream, 0)
	stream = append(stream, "fg"...)
	// feed one byte at a time, the worst split
	for _, v := range stream {
		if err := conn.unpad([]byte{v}); err != nil {
			t.Fatal(err)
		}
	}
	if string(conn.readOut) != "abcdefg" {
		t.Fatalf("content = %q", conn.readOut)
	}
	if conn.readMode != vlessVisionReadPlain {
		t.Fatalf("read mode = %d, expected plain after end", conn.readMode)
	}

	// a downlink that does not start with the user id is plain from the start
	unpadded := newVlessVisionConn(nil, nil, userId, nil)
	if err := unpadded.unpad([]byte("0123456789abcdef plain")); err != nil {
		t.Fatal(err)
	}
	if string(unpadded.readOut) != "0123456789abcdef plain" || unpadded.readMode != vlessVisionReadPlain {
		t.Fatalf("unpadded downlink = %q mode %d", unpadded.readOut, unpadded.readMode)
	}

	bad := newVlessVisionConn(nil, nil, userId, nil)
	badStream := append(append([]byte{}, userId[:]...), 9, 0, 0, 0, 0)
	if err := bad.unpad(badStream); err == nil {
		t.Fatalf("an unknown command must fail the stream")
	}
}

// The reality client seals the session id the way a reality server opens it:
// x25519 with the server key, hkdf over the hello random, aes-gcm with the
// hello (session id zeroed) as aad. A fake server captures the hello and opens
// it independently.
func TestVlessRealitySessionIdOpensWithTheServerKey(t *testing.T) {
	serverPrivateKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	shortId := []byte{0x01, 0x23, 0x45, 0x67}
	config := &VlessConfig{
		Address:     "127.0.0.1",
		Port:        443,
		Id:          testVlessUserId,
		Network:     VlessNetworkTcp,
		Security:    VlessSecurityReality,
		ServerName:  "www.cover.example",
		Fingerprint: "chrome",
		PublicKey:   serverPrivateKey.PublicKey().Bytes(),
		ShortId:     shortId,
	}

	clientConn, serverConn := net.Pipe()
	helloRecord := make(chan []byte, 1)
	go func() {
		defer serverConn.Close()
		header := make([]byte, 5)
		if _, err := io.ReadFull(serverConn, header); err != nil {
			return
		}
		body := make([]byte, int(binary.BigEndian.Uint16(header[3:5])))
		if _, err := io.ReadFull(serverConn, body); err != nil {
			return
		}
		helloRecord <- body
	}()
	before := time.Now()
	if _, err := vlessRealityClient(context.Background(), clientConn, config, 5*time.Second); err == nil {
		t.Fatalf("the handshake cannot complete against a server that only reads")
	}
	hello := <-helloRecord

	// the handshake message: type, length, version, random, session id
	if hello[0] != 0x01 || hello[38] != 32 {
		t.Fatalf("not a client hello with a 32-byte session id")
	}
	random := hello[6:38]
	sessionId := append([]byte{}, hello[39:71]...)
	x25519Share := testClientHelloX25519Share(t, hello)

	clientPublicKey, err := ecdh.X25519().NewPublicKey(x25519Share)
	if err != nil {
		t.Fatal(err)
	}
	sharedSecret, err := serverPrivateKey.ECDH(clientPublicKey)
	if err != nil {
		t.Fatal(err)
	}
	authKey, err := vlessRealityAuthKey(sharedSecret, random)
	if err != nil {
		t.Fatal(err)
	}
	aad := append([]byte{}, hello...)
	copy(aad[39:71], make([]byte, 32))
	block, err := aes.NewCipher(authKey)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := aead.Open(nil, random[20:], sessionId, aad)
	if err != nil {
		t.Fatalf("the server key does not open the session id: %s", err)
	}
	if !bytes.Equal(plaintext[0:3], vlessRealityClientVersion[:]) || plaintext[3] != 0 {
		t.Errorf("version bytes = %x", plaintext[0:4])
	}
	sealedTime := time.Unix(int64(binary.BigEndian.Uint32(plaintext[4:8])), 0)
	if sealedTime.Before(before.Add(-time.Second)) || time.Now().Add(time.Second).Before(sealedTime) {
		t.Errorf("sealed time %s is not now", sealedTime)
	}
	if !bytes.Equal(plaintext[8:16], append(append([]byte{}, shortId...), 0, 0, 0, 0)) {
		t.Errorf("short id = %x", plaintext[8:16])
	}
}

// The x25519 key share of a marshaled client hello: the pure x25519 share
// when offered, which is the one the client uses.
func testClientHelloX25519Share(t *testing.T, hello []byte) []byte {
	t.Helper()
	offset := 4 + 2 + 32
	offset += 1 + int(hello[offset])
	offset += 2 + int(binary.BigEndian.Uint16(hello[offset:]))
	offset += 1 + int(hello[offset])
	extensionsEnd := offset + 2 + int(binary.BigEndian.Uint16(hello[offset:]))
	offset += 2
	for offset+4 <= extensionsEnd {
		extensionType := binary.BigEndian.Uint16(hello[offset:])
		extensionLength := int(binary.BigEndian.Uint16(hello[offset+2:]))
		data := hello[offset+4 : offset+4+extensionLength]
		offset += 4 + extensionLength
		if extensionType != 0x0033 {
			continue
		}
		shares := data[2:]
		for 4 <= len(shares) {
			group := binary.BigEndian.Uint16(shares)
			keyLength := int(binary.BigEndian.Uint16(shares[2:]))
			key := shares[4 : 4+keyLength]
			shares = shares[4+keyLength:]
			if group == 0x001d && keyLength == 32 {
				return key
			}
		}
	}
	t.Fatalf("the client hello offers no x25519 key share")
	return nil
}

// A leaf proves the auth key only when its signature field is the hmac of
// its ed25519 public key under that key.
func TestVlessRealityLeafProof(t *testing.T) {
	authKey := bytes.Repeat([]byte{0x07}, 32)
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "www.cover.example"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	if vlessRealityLeafProvesKey(leaf, authKey) {
		t.Fatalf("an ordinary self-signed leaf must not prove the key")
	}
	mac := hmac.New(sha512.New, authKey)
	mac.Write(publicKey)
	leaf.Signature = mac.Sum(nil)
	if !vlessRealityLeafProvesKey(leaf, authKey) {
		t.Fatalf("the hmac signature must prove the key")
	}
	if vlessRealityLeafProvesKey(leaf, bytes.Repeat([]byte{0x08}, 32)) {
		t.Fatalf("another key must not be proven")
	}
}

// The http host of the ws and httpupgrade transports: the configured host,
// else the server name, else the address, with an ipv6 literal bracketed.
func TestVlessHttpHost(t *testing.T) {
	cases := []struct {
		config   VlessConfig
		expected string
	}{
		{config: VlessConfig{Address: "192.0.2.1", Host: "cdn.example", ServerName: "sni.example"}, expected: "cdn.example"},
		{config: VlessConfig{Address: "192.0.2.1", ServerName: "sni.example"}, expected: "sni.example"},
		{config: VlessConfig{Address: "vless.example"}, expected: "vless.example"},
		{config: VlessConfig{Address: "192.0.2.1"}, expected: "192.0.2.1"},
		{config: VlessConfig{Address: "2001:db8::1"}, expected: "[2001:db8::1]"},
		{config: VlessConfig{Address: "2001:db8::1", Host: "2001:db8::2"}, expected: "[2001:db8::2]"},
	}
	for _, c := range cases {
		if host := c.config.httpHost(); host != c.expected {
			t.Errorf("%+v: host = %s, expected %s", c.config, host, c.expected)
		}
	}
}
