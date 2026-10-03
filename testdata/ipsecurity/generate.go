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
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
