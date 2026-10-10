//go:build ignore

// Regenerates the security policy fixtures in this directory:
//
//	go run testdata/ipsecurity/generate.go
//
// Every fixture is synthesized from the public protocol facts cited in its
// provenance field (see README.md). None is a capture: no addresses, hosts,
// keys, or identifiers from a real device are retained. Random-looking fields
// (keys, ciphertext, padding) are a SHA-256 counter stream keyed by the fixture
// name, so the output is byte-for-byte reproducible on every platform.
package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"golang.org/x/crypto/sha3"
	"google.golang.org/protobuf/encoding/protowire"
)

type fixture struct {
	Name            string   `json:"name"`
	Protocol        string   `json:"protocol"`
	Provenance      string   `json:"provenance"`
	Transport       string   `json:"transport"`
	DestinationPort int      `json:"destination_port"`
	Payloads        []string `json:"payloads"`
	// the policy result of the last payload with application standards and the
	// privileged-port signatures disabled (the policy before IPSECURITY-UPDATE4)
	ExpectBefore string `json:"expect_before"`
	// the policy result of the last payload with the default policy
	ExpectAfter string `json:"expect_after"`
	Note        string `json:"note,omitempty"`
}

// stream is a deterministic byte source: sha256(name || counter) blocks.
type stream struct {
	name    string
	counter uint64
	buffer  []byte
}

func (self *stream) bytes(n int) []byte {
	out := make([]byte, 0, n)
	for len(out) < n {
		if len(self.buffer) == 0 {
			var counter [8]byte
			binary.BigEndian.PutUint64(counter[:], self.counter)
			self.counter += 1
			sum := sha256.Sum256(append([]byte(self.name), counter[:]...))
			self.buffer = sum[:]
		}
		take := min(n-len(out), len(self.buffer))
		out = append(out, self.buffer[:take]...)
		self.buffer = self.buffer[take:]
	}
	return out
}

func cat(parts ...[]byte) []byte {
	var out []byte
	for _, part := range parts {
		out = append(out, part...)
	}
	return out
}

func zeros(n int) []byte {
	return make([]byte, n)
}

func le32(v uint32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, v)
	return b
}

func le64(v uint64) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, v)
	return b
}

func be16(v uint16) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, v)
	return b
}

// wireguard whitepaper 5.4.2: type 1, reserved zero, sender index, ephemeral (32),
// encrypted static (32+16), encrypted timestamp (12+16), mac1 (16), mac2 (16, zero
// without a cookie) = 148 bytes
func wireGuardInitiation(s *stream) []byte {
	return cat([]byte{1, 0, 0, 0}, s.bytes(4+32+48+28+16), zeros(16))
}

// wireguard whitepaper 5.4.6: type 4, reserved zero, receiver index, 64-bit
// little-endian counter, then the 16-byte padded ciphertext and tag
func wireGuardTransport(s *stream, receiver []byte, counter uint64, length int) []byte {
	return cat([]byte{4, 0, 0, 0}, receiver, le64(counter), s.bytes(length-16))
}

var rakNetOfflineMagic = []byte{0x00, 0xff, 0xff, 0x00, 0xfe, 0xfe, 0xfe, 0xfe, 0xfd, 0xfd, 0xfd, 0xfd, 0x12, 0x34, 0x56, 0x78}

var levinSignature = []byte{0x01, 0x21, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01}

// levin_base.h bucket head: signature, cb (u64), have_to_return (bool),
// command (u32), return_code (i32), flags (u32), protocol_version (u32), all
// little-endian
func levinHead(bodyLength int, command uint32) []byte {
	return cat(levinSignature, le64(uint64(bodyLength)), []byte{1}, le32(command), le32(0), le32(1), le32(1))
}

func bittorrentHandshake(s *stream) []byte {
	return cat([]byte("\x13BitTorrent protocol"), zeros(8), s.bytes(20), []byte("-XX0000-"), s.bytes(12))
}

func tlsClientHello(s *stream) []byte {
	body := cat([]byte{0x03, 0x03}, s.bytes(32), []byte{0x00, 0x00, 0x02, 0x13, 0x01, 0x01, 0x00})
	handshake := cat([]byte{0x01, 0x00}, be16(uint16(len(body))), body)
	return cat([]byte{0x16, 0x03, 0x01}, be16(uint16(len(handshake))), handshake)
}

// rlp (Ethereum yellow paper appendix B): byte strings and lists
func rlpString(b []byte) []byte {
	if len(b) == 1 && b[0] < 0x80 {
		return b
	}
	return cat(rlpHeader(0x80, len(b)), b)
}

func rlpUint(v uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	i := 0
	for i < 8 && b[i] == 0 {
		i += 1
	}
	return rlpString(b[i:])
}

func rlpList(items ...[]byte) []byte {
	payload := cat(items...)
	return cat(rlpHeader(0xc0, len(payload)), payload)
}

func rlpHeader(offset byte, length int) []byte {
	if length <= 55 {
		return []byte{offset + byte(length)}
	}
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(length))
	i := 0
	for b[i] == 0 {
		i += 1
	}
	return cat([]byte{offset + 55 + byte(8-i)}, b[i:])
}

func keccak256(parts ...[]byte) []byte {
	h := sha3.NewLegacyKeccak256()
	for _, part := range parts {
		h.Write(part)
	}
	return h.Sum(nil)
}

// sign is a recoverable secp256k1 signature r || s || recovery id over a
// 32-byte hash (deterministic, RFC 6979)
func sign(key *secp256k1.PrivateKey, hash []byte) []byte {
	compact := ecdsa.SignCompact(key, hash, false)
	return cat(compact[1:65], []byte{compact[0] - 27})
}

func privateKey(s *stream) *secp256k1.PrivateKey {
	return secp256k1.PrivKeyFromBytes(s.bytes(32))
}

// devp2p discv4 "Wire Protocol": hash || signature || packet-type || packet-data,
// signature = sign(keccak256(packet-type || packet-data)),
// hash = keccak256(signature || packet-type || packet-data)
func discv4Packet(key *secp256k1.PrivateKey, packetType byte, data []byte) []byte {
	typed := cat([]byte{packetType}, data)
	signature := sign(key, keccak256(typed))
	return cat(keccak256(signature, typed), signature, typed)
}

// discv4 endpoint = [ip, udp-port, tcp-port]; RFC 5737 documentation addresses
func discv4Endpoint(ip []byte, udpPort uint64, tcpPort uint64) []byte {
	return rlpList(rlpString(ip), rlpUint(udpPort), rlpUint(tcpPort))
}

// NIST SP 800-56 concatenation KDF with SHA-256 (rlpx "ECIES Encryption")
func concatKdf(z []byte, length int) []byte {
	var out []byte
	for counter := uint32(1); len(out) < length; counter += 1 {
		var c [4]byte
		binary.BigEndian.PutUint32(c[:], counter)
		sum := sha256.Sum256(cat(c[:], z))
		out = append(out, sum[:]...)
	}
	return out[:length]
}

// rlpx "ECIES Encryption": R || iv || c || d, where R = r*G, S = x(r*K_B),
// kE || kM = KDF(S, 32), c = AES-128-CTR(kE, iv, m),
// d = HMAC-SHA256(sha256(kM), iv || c || shared-mac-data)
func eciesEncrypt(s *stream, recipient *secp256k1.PublicKey, message []byte, sharedMacData []byte) []byte {
	ephemeral := privateKey(s)
	key := concatKdf(secp256k1.GenerateSharedSecret(ephemeral, recipient), 32)
	block, err := aes.NewCipher(key[:16])
	if err != nil {
		panic(err)
	}
	iv := s.bytes(16)
	c := make([]byte, len(message))
	cipher.NewCTR(block, iv).XORKeyStream(c, message)
	macKey := sha256.Sum256(key[16:32])
	mac := hmac.New(sha256.New, macKey[:])
	mac.Write(iv)
	mac.Write(c)
	mac.Write(sharedMacData)
	return cat(ephemeral.PubKey().SerializeUncompressed(), iv, c, mac.Sum(nil))
}

func xor(a []byte, b []byte) []byte {
	out := make([]byte, len(a))
	for i := range a {
		out[i] = a[i] ^ b[i]
	}
	return out
}

// rlpx "Initial Handshake" auth from the initiator. EIP-8: auth-size ||
// ecies(auth-body || auth-padding) with auth-size as the shared mac data,
// auth-body = [sig, initiator-pubk, initiator-nonce, auth-vsn = 4]. Pre-EIP-8:
// ecies(sig || keccak256(ephemeral-pubk) || initiator-pubk || nonce || 0x00),
// 307 bytes. sig = sign(ephemeral-privk, static-shared-secret ^ nonce).
func rlpxAuth(s *stream, eip8 bool, padding int) []byte {
	initiator := privateKey(s)
	recipient := privateKey(s).PubKey()
	ephemeral := privateKey(s)
	nonce := s.bytes(32)
	signature := sign(ephemeral, xor(secp256k1.GenerateSharedSecret(initiator, recipient), nonce))
	initiatorPublic := initiator.PubKey().SerializeUncompressed()[1:]
	if !eip8 {
		body := cat(signature, keccak256(ephemeral.PubKey().SerializeUncompressed()[1:]), initiatorPublic, nonce, []byte{0})
		return eciesEncrypt(s, recipient, body, nil)
	}
	body := cat(rlpList(rlpString(signature), rlpString(initiatorPublic), rlpString(nonce), rlpUint(4)), s.bytes(padding))
	// ecies adds R (65), iv (16) and d (32)
	size := be16(uint16(65 + 16 + len(body) + 32))
	return cat(size, eciesEncrypt(s, recipient, body, size))
}

// an RLPx frame after the handshake: header-ciphertext (16) || header-mac (16)
// || frame-ciphertext padded to 16 || frame-mac (16)
func rlpxFrame(s *stream, frameSize int) []byte {
	return s.bytes(16 + 16 + (frameSize+15)/16*16 + 16)
}

// WhatsApp's Noise transport as the public clients write it (whatsmeow,
// Baileys, yowsup and consonance; see whatsAppStream in
// ip_security_appstandard.go): a 3-byte big-endian length before every frame
func whatsAppFrame(data []byte) []byte {
	return cat([]byte{byte(len(data) >> 16), byte(len(data) >> 8), byte(len(data))}, data)
}

// One length-delimited protobuf field: its tag, then the value behind its
// length.
func protobufBytes(field protowire.Number, value []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, field, protowire.BytesType), value)
}

// the client's first frame: HandshakeMessage{clientHello (2)} whose first
// field is the 32-byte ephemeral key (1); Noise IK adds the encrypted static
// key (2, 32 + 16 bytes) and payload (3)
func whatsAppClientHello(s *stream, ik bool) []byte {
	hello := protobufBytes(1, s.bytes(32))
	if ik {
		hello = cat(hello, protobufBytes(2, s.bytes(48)), protobufBytes(3, s.bytes(220)))
	}
	return protobufBytes(2, hello)
}

func main() {
	var fixtures []fixture
	add := func(f fixture, payloads ...[]byte) {
		for _, payload := range payloads {
			f.Payloads = append(f.Payloads, hex.EncodeToString(payload))
		}
		fixtures = append(fixtures, f)
	}
	newStream := func(name string) *stream {
		return &stream{name: name}
	}

	{
		s := newStream("wireguard-handshake")
		receiver := s.bytes(4)
		add(fixture{
			Name:            "wireguard-handshake",
			Protocol:        "wireguard",
			Provenance:      "WireGuard whitepaper 5.4.2 (initiation, 148 bytes) and 5.4.6 (transport data)",
			Transport:       "udp",
			DestinationPort: 51820,
			ExpectBefore:    "drop",
			ExpectAfter:     "allow",
		},
			wireGuardInitiation(s),
			wireGuardTransport(s, receiver, 0, 96),
			wireGuardTransport(s, receiver, 1, 128),
			wireGuardTransport(s, receiver, 2, 144),
		)
	}
	{
		s := newStream("wireguard-midstream")
		receiver := s.bytes(4)
		add(fixture{
			Name:            "wireguard-midstream",
			Protocol:        "wireguard",
			Provenance:      "WireGuard whitepaper 5.4.6 (transport data); a flow re-inspected after idle eviction",
			Transport:       "udp",
			DestinationPort: 41641,
			ExpectBefore:    "drop",
			ExpectAfter:     "allow",
		},
			wireGuardTransport(s, receiver, 17, 112),
			wireGuardTransport(s, receiver, 18, 144),
			wireGuardTransport(s, receiver, 19, 128),
		)
	}
	{
		s := newStream("openvpn-udp")
		sid := s.bytes(8)
		add(fixture{
			Name:            "openvpn-udp",
			Protocol:        "openvpn",
			Provenance:      "OpenVPN protocol: P_CONTROL_HARD_RESET_CLIENT_V2 (7), P_CONTROL_V1 (4), P_ACK_V1 (5), key id 0, 8-byte session id",
			Transport:       "udp",
			DestinationPort: 1194,
			ExpectBefore:    "drop",
			ExpectAfter:     "allow",
		},
			cat([]byte{7 << 3}, sid, s.bytes(48)),
			cat([]byte{4 << 3}, sid, s.bytes(96)),
			cat([]byte{5 << 3}, sid, s.bytes(64)),
			cat([]byte{4 << 3}, sid, s.bytes(160)),
		)
	}
	{
		s := newStream("openvpn-tcp")
		sid := s.bytes(8)
		framed := func(packet []byte) []byte {
			return cat(be16(uint16(len(packet))), packet)
		}
		add(fixture{
			Name:            "openvpn-tcp",
			Protocol:        "openvpn",
			Provenance:      "OpenVPN protocol over TCP: 2-byte packet length prefix, then the same reliability-layer header as UDP",
			Transport:       "tcp",
			DestinationPort: 1194,
			ExpectBefore:    "drop",
			ExpectAfter:     "allow",
		},
			framed(cat([]byte{10 << 3}, sid, s.bytes(48))),
			framed(cat([]byte{4 << 3}, sid, s.bytes(96))),
			framed(cat([]byte{4 << 3}, sid, s.bytes(160))),
		)
	}
	{
		s := newStream("rtmp-publish")
		add(fixture{
			Name:            "rtmp-publish",
			Protocol:        "rtmp",
			Provenance:      "Adobe RTMP 1.0 5.2.1-5.2.3: C0 version 3; C1 time (4), zero (4), random (1528); C2 random (1536)",
			Transport:       "tcp",
			DestinationPort: 1935,
			ExpectBefore:    "drop",
			ExpectAfter:     "allow",
		},
			// C0 + the first 1459 bytes of C1 in one MSS-sized segment
			cat([]byte{3}, s.bytes(4), zeros(4), s.bytes(1451)),
			// the rest of C1
			s.bytes(77),
			// C2
			s.bytes(1536),
		)
	}
	{
		s := newStream("rtmp-digest")
		add(fixture{
			Name:            "rtmp-digest",
			Protocol:        "rtmp",
			Provenance:      "RTMP C0/C1 whose bytes 5-8 carry a version instead of the 1.0 zero word; not matched by design",
			Transport:       "tcp",
			DestinationPort: 1935,
			ExpectBefore:    "drop",
			ExpectAfter:     "drop",
			Note:            "documents that only the RTMP 1.0 handshake is recognized",
		},
			cat([]byte{3}, s.bytes(4), []byte{0x80, 0x00, 0x07, 0x02}, s.bytes(1451)),
			s.bytes(77),
			s.bytes(1536),
		)
	}
	{
		// epee portable storage: storage signature, format version 1, a section
		// with string keys; mostly small integers and ascii
		body := cat(
			[]byte{0x01, 0x11, 0x01, 0x01, 0x01, 0x01, 0x02, 0x01, 0x01},
			[]byte{0x08},
			[]byte{0x09}, []byte("node_data"), []byte{0x0c, 0x10},
			[]byte{0x07}, []byte("my_port"), []byte{0x06}, le32(18080),
			[]byte{0x0a}, []byte("network_id"), []byte{0x0a, 0x40}, zeros(16),
			[]byte{0x07}, []byte("peer_id"), []byte{0x05}, zeros(8),
			[]byte{0x0c}, []byte("payload_data"), []byte{0x0c, 0x10},
			[]byte{0x0e}, []byte("current_height"), []byte{0x05}, le64(3000000),
			[]byte{0x0e}, []byte("top_version"), []byte{0x08, 0x10},
		)
		add(fixture{
			Name:            "levin-handshake",
			Protocol:        "levin",
			Provenance:      "Monero levin_base.h bucket head (signature 0x0101010101012101, protocol_version 1) and portable_storage_base.h body",
			Transport:       "tcp",
			DestinationPort: 18080,
			ExpectBefore:    "allow",
			ExpectAfter:     "allow",
			Note:            "plaintext before (allow-plaintext); recognized as levin after",
		},
			cat(levinHead(len(body), 1001), body),
		)
	}
	{
		s := newStream("levin-dense")
		add(fixture{
			Name:            "levin-dense",
			Protocol:        "levin",
			Provenance:      "Monero levin bucket heads carrying dense (binary) bodies",
			Transport:       "tcp",
			DestinationPort: 18080,
			ExpectBefore:    "drop",
			ExpectAfter:     "allow",
		},
			cat(levinHead(400, 2002), s.bytes(400)),
			cat(levinHead(400, 2002), s.bytes(400)),
			cat(levinHead(400, 2002), s.bytes(400)),
		)
	}
	{
		add(fixture{
			Name:            "raknet-open-connection-zero-padded",
			Protocol:        "raknet",
			Provenance:      "RakNet ID_OPEN_CONNECTION_REQUEST_1 (0x05), offline message magic, protocol byte, zero padding to the probed MTU",
			Transport:       "udp",
			DestinationPort: 19132,
			ExpectBefore:    "allow",
			ExpectAfter:     "allow",
		},
			cat([]byte{0x05}, rakNetOfflineMagic, []byte{0x0b}, zeros(1385)),
		)
	}
	{
		s := newStream("raknet-open-connection-random-padded")
		add(fixture{
			Name:            "raknet-open-connection-random-padded",
			Protocol:        "raknet",
			Provenance:      "RakNet ID_OPEN_CONNECTION_REQUEST_1 retried with decreasing MTU, padding not zero",
			Transport:       "udp",
			DestinationPort: 49152,
			ExpectBefore:    "drop",
			ExpectAfter:     "allow",
		},
			cat([]byte{0x05}, rakNetOfflineMagic, []byte{0x06}, s.bytes(1400)),
			cat([]byte{0x05}, rakNetOfflineMagic, []byte{0x06}, s.bytes(1100)),
			cat([]byte{0x05}, rakNetOfflineMagic, []byte{0x06}, s.bytes(560)),
		)
	}
	{
		s := newStream("monero-rpc-http")
		_ = s
		add(fixture{
			Name:            "monero-rpc-http",
			Protocol:        "http",
			Provenance:      "Monero wallet RPC over plain HTTP (POST /json_rpc)",
			Transport:       "tcp",
			DestinationPort: 18081,
			ExpectBefore:    "allow",
			ExpectAfter:     "allow",
		},
			[]byte("POST /json_rpc HTTP/1.1\r\nHost: node.example\r\nContent-Type: application/json\r\nContent-Length: 46\r\n\r\n{\"jsonrpc\":\"2.0\",\"id\":\"0\",\"method\":\"get_info\"}"),
		)
	}
	{
		s := newStream("simplex-tls")
		add(fixture{
			Name:            "simplex-tls",
			Protocol:        "tls",
			Provenance:      "TLS 1.3 ClientHello (RFC 8446) on the SimpleX SMP port",
			Transport:       "tcp",
			DestinationPort: 5223,
			ExpectBefore:    "allow",
			ExpectAfter:     "allow",
		},
			tlsClientHello(s),
			s.bytes(400),
			s.bytes(400),
			s.bytes(400),
		)
	}
	{
		s := newStream("tls-443")
		add(fixture{
			Name:            "tls-443",
			Protocol:        "tls",
			Provenance:      "TLS 1.3 ClientHello (RFC 8446) then application data on 443",
			Transport:       "tcp",
			DestinationPort: 443,
			ExpectBefore:    "allow",
			ExpectAfter:     "allow",
		},
			tlsClientHello(s),
			cat([]byte{0x17, 0x03, 0x03, 0x01, 0x90}, s.bytes(400)),
			cat([]byte{0x17, 0x03, 0x03, 0x01, 0x90}, s.bytes(400)),
		)
	}
	for _, padding := range []int{0, 52} {
		name := fmt.Sprintf("mse-tcp-pad%d", padding)
		s := newStream(name)
		add(fixture{
			Name:            name,
			Protocol:        "bittorrent-mse",
			Provenance:      "BitTorrent Message Stream Encryption: Ya (96 bytes) + PadA (0-512 random bytes), then encrypted messages",
			Transport:       "tcp",
			DestinationPort: 50321,
			ExpectBefore:    "drop",
			ExpectAfter:     "drop",
		},
			s.bytes(96+padding),
			s.bytes(120),
			s.bytes(300),
		)
	}
	{
		s := newStream("mse-udp-wireguard-prefixed")
		add(fixture{
			Name:            "mse-udp-wireguard-prefixed",
			Protocol:        "bittorrent-mse",
			Provenance:      "encrypted datagrams whose first 148-byte blob happens to start 01 00 00 00 (the accepted one-packet leak)",
			Transport:       "udp",
			DestinationPort: 50322,
			ExpectBefore:    "drop",
			ExpectAfter:     "drop",
		},
			cat([]byte{1, 0, 0, 0}, s.bytes(144)),
			s.bytes(120),
			s.bytes(300),
			s.bytes(200),
		)
	}
	{
		s := newStream("utp-encrypted")
		utpHeader := func(packetType byte, sequence uint16) []byte {
			return cat([]byte{packetType<<4 | 1, 0}, s.bytes(2), s.bytes(4), s.bytes(4), le32(0x00100000), be16(sequence), be16(0))
		}
		add(fixture{
			Name:            "utp-encrypted",
			Protocol:        "bittorrent-utp",
			Provenance:      "BEP 29 uTP ST_SYN then ST_DATA carrying encrypted peer wire",
			Transport:       "udp",
			DestinationPort: 50323,
			ExpectBefore:    "drop",
			ExpectAfter:     "drop",
		},
			utpHeader(4, 1),
			cat(utpHeader(0, 2), s.bytes(400)),
			cat(utpHeader(0, 3), s.bytes(400)),
			cat(utpHeader(0, 4), s.bytes(400)),
		)
	}
	for _, port := range []int{443, 80, 51413} {
		name := fmt.Sprintf("bittorrent-tcp-%d", port)
		expectBefore := "allow"
		if 1024 <= port {
			expectBefore = "incident"
		}
		add(fixture{
			Name:            name,
			Protocol:        "bittorrent",
			Provenance:      "BEP 3 peer wire handshake",
			Transport:       "tcp",
			DestinationPort: port,
			ExpectBefore:    expectBefore,
			ExpectAfter:     "incident",
		},
			bittorrentHandshake(newStream(name)),
		)
	}
	add(fixture{
		Name:            "dht-udp-443",
		Protocol:        "bittorrent-dht",
		Provenance:      "BEP 5 KRPC ping query",
		Transport:       "udp",
		DestinationPort: 443,
		ExpectBefore:    "allow",
		ExpectAfter:     "incident",
	},
		[]byte("d1:ad2:id20:abcdefghij0123456789e1:q4:ping1:t2:aa1:y1:qe"),
	)

	{
		s := newStream("ethereum-discv4")
		key := privateKey(s)
		local := discv4Endpoint([]byte{192, 0, 2, 10}, 30303, 30303)
		remote := discv4Endpoint([]byte{203, 0, 113, 40}, 30303, 30303)
		expiration := rlpUint(1790000000)
		ping := discv4Packet(key, 0x01, rlpList(rlpUint(4), local, remote, expiration, rlpUint(7)))
		add(fixture{
			Name:            "ethereum-discv4",
			Protocol:        "ethereum-discv4",
			Provenance:      "devp2p discv4 Wire Protocol: hash || signature || packet-type || packet-data; Ping (1), Pong (2), FindNode (3), ENRRequest (5)",
			Transport:       "udp",
			DestinationPort: 30303,
			ExpectBefore:    "drop",
			ExpectAfter:     "allow",
		},
			ping,
			discv4Packet(key, 0x02, rlpList(remote, rlpString(ping[:32]), expiration, rlpUint(7))),
			discv4Packet(key, 0x05, rlpList(expiration)),
			discv4Packet(key, 0x03, rlpList(rlpString(s.bytes(64)), expiration)),
		)
	}
	{
		s := newStream("ethereum-discv4-bad-hash")
		key := privateKey(s)
		expiration := rlpUint(1790000000)
		corrupt := func(packet []byte) []byte {
			packet[0] ^= 0x01
			return packet
		}
		add(fixture{
			Name:            "ethereum-discv4-bad-hash",
			Protocol:        "ethereum-discv4",
			Provenance:      "devp2p discv4 packets whose hash does not match keccak256 of the rest; not matched by design",
			Transport:       "udp",
			DestinationPort: 30303,
			ExpectBefore:    "drop",
			ExpectAfter:     "drop",
			Note:            "a discv4-shaped header without the hash invariant is judged by the encrypted heuristic",
		},
			corrupt(discv4Packet(key, 0x01, rlpList(rlpUint(4), discv4Endpoint([]byte{192, 0, 2, 10}, 30303, 30303), discv4Endpoint([]byte{203, 0, 113, 40}, 30303, 30303), expiration, rlpUint(7)))),
			corrupt(discv4Packet(key, 0x05, rlpList(expiration))),
			corrupt(discv4Packet(key, 0x03, rlpList(rlpString(s.bytes(64)), expiration))),
		)
	}
	{
		s := newStream("ethereum-rlpx-eip8")
		add(fixture{
			Name:            "ethereum-rlpx-eip8",
			Protocol:        "ethereum-rlpx",
			Provenance:      "devp2p RLPx Initial Handshake with EIP-8 auth (auth-size || ECIES R || iv || c || d), then framed Hello and Status",
			Transport:       "tcp",
			DestinationPort: 30303,
			ExpectBefore:    "drop",
			ExpectAfter:     "allow",
		},
			rlpxAuth(s, true, 150),
			rlpxFrame(s, 140),
			rlpxFrame(s, 100),
			rlpxFrame(s, 3),
		)
	}
	{
		s := newStream("ethereum-rlpx-pre-eip8")
		add(fixture{
			Name:            "ethereum-rlpx-pre-eip8",
			Protocol:        "ethereum-rlpx",
			Provenance:      "devp2p RLPx Initial Handshake with the pre-EIP-8 307-byte auth (ECIES R || iv || c || d), then frames",
			Transport:       "tcp",
			DestinationPort: 30303,
			ExpectBefore:    "drop",
			ExpectAfter:     "allow",
		},
			rlpxAuth(s, false, 0),
			rlpxFrame(s, 140),
			rlpxFrame(s, 100),
		)
	}
	{
		s := newStream("ethereum-rlpx-off-curve")
		auth := rlpxAuth(s, true, 150)
		// replace the ephemeral key's y with bytes that are not on the curve
		copy(auth[2+33:2+65], s.bytes(32))
		add(fixture{
			Name:            "ethereum-rlpx-off-curve",
			Protocol:        "ethereum-rlpx",
			Provenance:      "an EIP-8-shaped auth whose ephemeral key is not a secp256k1 point; not matched by design",
			Transport:       "tcp",
			DestinationPort: 30303,
			ExpectBefore:    "drop",
			ExpectAfter:     "drop",
		},
			auth,
			rlpxFrame(s, 140),
			rlpxFrame(s, 100),
		)
	}
	{
		// MSE with PadA = 211 is exactly the 307-byte length of a pre-EIP-8 auth
		s := newStream("mse-tcp-pad211")
		add(fixture{
			Name:            "mse-tcp-pad211",
			Protocol:        "bittorrent-mse",
			Provenance:      "BitTorrent Message Stream Encryption: Ya (96 bytes) + PadA (211 random bytes, the pre-EIP-8 RLPx auth length), then encrypted messages",
			Transport:       "tcp",
			DestinationPort: 30303,
			ExpectBefore:    "drop",
			ExpectAfter:     "drop",
		},
			s.bytes(96+211),
			s.bytes(120),
			s.bytes(300),
		)
	}
	{
		s := newStream("whatsapp-noise-web")
		hello := whatsAppClientHello(s, false)
		finish := protobufBytes(4, cat(protobufBytes(1, s.bytes(48)), protobufBytes(2, s.bytes(180))))
		add(fixture{
			Name:            "whatsapp-noise-web",
			Protocol:        "whatsapp-noise",
			Provenance:      "WhatsApp Noise transport as the public web clients write it (whatsmeow socket WAConnHeader and SendFrame, Baileys noise-handler): header 'WA' 6 3 and the Noise XX clientHello frame in one write, then the clientFinish frame and transport frames, each behind a 3-byte big-endian length",
			Transport:       "tcp",
			DestinationPort: 5222,
			ExpectBefore:    "drop",
			ExpectAfter:     "allow",
		},
			cat([]byte{'W', 'A', 6, 3}, whatsAppFrame(hello)),
			whatsAppFrame(finish),
			whatsAppFrame(s.bytes(240)),
			whatsAppFrame(s.bytes(320)),
		)
	}
	{
		s := newStream("whatsapp-noise-native-segmented")
		routing := cat([]byte{0x08}, s.bytes(3))
		add(fixture{
			Name:            "whatsapp-noise-native-segmented",
			Protocol:        "whatsapp-noise",
			Provenance:      "WhatsApp mobile-protocol opening written as yowsup's noise layer writes it: edge header 'ED' 0 1, a 4-byte routing info starting 08 behind its 3-byte length (the prefix nDPI's whatsapp.c matches) and header 'WA' 4 0, each in its own segment; then the Noise IK clientHello frame (consonance) and transport frames",
			Transport:       "tcp",
			DestinationPort: 5222,
			ExpectBefore:    "drop",
			ExpectAfter:     "allow",
		},
			[]byte{'E', 'D', 0, 1},
			whatsAppFrame(routing),
			[]byte{'W', 'A', 4, 0},
			whatsAppFrame(whatsAppClientHello(s, true)),
			whatsAppFrame(s.bytes(260)),
			whatsAppFrame(s.bytes(300)),
			whatsAppFrame(s.bytes(200)),
		)
	}
	{
		s := newStream("whatsapp-noise-bad-length")
		hello := whatsAppClientHello(s, true)
		length := len(hello) + 1
		add(fixture{
			Name:            "whatsapp-noise-bad-length",
			Protocol:        "whatsapp-noise",
			Provenance:      "a WhatsApp-shaped opening whose 3-byte frame length is one more than the clientHello protobuf it carries; not matched by design",
			Transport:       "tcp",
			DestinationPort: 5222,
			ExpectBefore:    "drop",
			ExpectAfter:     "drop",
			Note:            "the frame length must equal the length of the protobuf it carries",
		},
			cat([]byte{'W', 'A', 6, 3, byte(length >> 16), byte(length >> 8), byte(length)}, hello, s.bytes(1)),
			whatsAppFrame(s.bytes(240)),
			whatsAppFrame(s.bytes(320)),
		)
	}

	for _, f := range fixtures {
		out, err := json.MarshalIndent(f, "", "  ")
		if err != nil {
			panic(err)
		}
		out = append(out, '\n')
		path := filepath.Join("testdata", "ipsecurity", f.Name+".json")
		if err := os.WriteFile(path, out, 0o644); err != nil {
			panic(err)
		}
	}
}
