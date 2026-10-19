package connect

// Ethereum devp2p application standards. Ethereum nodes talk to many peers on
// user ports with payloads that look random from byte 0, which is the fan-out
// profile the encrypted heuristic exists to stop (encrypted BitTorrent).
// They are admitted only by cryptographic invariants that every Ethereum
// packet carries and no BitTorrent variant can satisfy short of a modified
// client deliberately forging them (the accepted disguise class):
//
//   - discovery v4 (devp2p discv4 "Wire Protocol"): packet = hash || signature
//     || packet-type || packet-data, where hash = keccak256(signature ||
//     packet-type || packet-data), the signature is 65 bytes, the type is
//     1-6 (ping, pong, findnode, neighbors, enrrequest, enrresponse), the data
//     is an RLP list and the packet is at most 1280 bytes. The hash is checked
//     over the full packet: a random or BitTorrent payload matches with
//     probability 2^-256. The hash runs only after the cheap structural checks,
//     which pass only ~1 in 200 random datagrams (type 1-6 and a list prefix).
//   - RLPx (devp2p rlpx "Initial Handshake", EIP-8): the initiator's first
//     message is auth-size (2 bytes, big-endian) || enc-auth-body, where
//     auth-size is the length of enc-auth-body and enc-auth-body is ECIES
//     ciphertext R || iv || c || d whose R is an uncompressed secp256k1 point
//     (0x04 || x || y). The pre-EIP-8 auth is exactly 307 bytes and starts with
//     R. The point must lie on the curve (y^2 = x^3 + 7 mod p, x, y < p): a
//     random 64 bytes is on the curve with probability ~2^-256. RLPx is matched
//     on the flow's first TCP payload only, and the whole auth must be in that
//     segment.
//
// Discovery v5 is not detectable: its header is AES-CTR masked with a key
// derived from the destination node id, which the exit never sees, so a v5
// packet is indistinguishable from random bytes. A v5 datagram on a flow that
// also carries v4 counts toward the heuristic like any unidentified encrypted
// packet; a later v4 packet on the same flow still admits it while inspecting.
//
// Only the client's outbound packets are inspected (dmcaFlowState), so the
// responder's ack/pong cannot be used as confirmation; the invariants above are
// each far stronger than the two-packet confirmations of the other detectors.

import (
	"bytes"
	"encoding/binary"
	"hash"
	"sync"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"golang.org/x/crypto/sha3"
)

const (
	ethereumDiscv4HashLength      = 32
	ethereumDiscv4SignatureLength = 65
	// hash || signature || packet-type
	ethereumDiscv4HeaderLength    = ethereumDiscv4HashLength + ethereumDiscv4SignatureLength + 1
	ethereumDiscv4MaxPacketSize   = 1280
	ethereumDiscv4PacketTypePing  = 0x01
	ethereumDiscv4PacketTypeLast  = 0x06
	secp256k1UncompressedLength   = 65
	secp256k1UncompressedFormat   = 0x04
	ethereumRlpxEip8SizeLength    = 2
	ethereumRlpxPreEip8AuthLength = 307
	// ECIES R (65) || iv (16) || mac (32)
	ethereumRlpxEciesOverhead = secp256k1UncompressedLength + 16 + 32
	// the smallest RLP auth-body: [sig (65), initiator-pubk (64),
	// initiator-nonce (32), auth-vsn] is 2 + 67 + 66 + 33 + 1 bytes
	ethereumRlpxMinAuthBodyLength = 169
	ethereumRlpxEip8MinAuthSize   = ethereumRlpxEciesOverhead + ethereumRlpxMinAuthBodyLength
)

// keccakDigest is a reusable legacy Keccak-256 state and output buffer, so a
// hash check does not allocate on the packet path.
type keccakDigest struct {
	hash   hash.Hash
	digest [32]byte
}

var keccakDigestPool = sync.Pool{
	New: func() any {
		return &keccakDigest{
			hash: sha3.NewLegacyKeccak256(),
		}
	},
}

// legacyKeccak256Equals reports whether keccak256(data) == sum.
func legacyKeccak256Equals(data []byte, sum []byte) bool {
	k := keccakDigestPool.Get().(*keccakDigest)
	defer keccakDigestPool.Put(k)
	k.hash.Reset()
	k.hash.Write(data)
	return bytes.Equal(k.hash.Sum(k.digest[:0]), sum)
}

// rlpListLength returns the encoded length (header and payload) of the
// canonical RLP list that starts b, if it fits in b. Lists longer than 64 KiB
// are not needed here and are rejected.
func rlpListLength(b []byte) (int, bool) {
	if len(b) == 0 || b[0] < 0xc0 {
		return 0, false
	}
	if b[0] <= 0xf7 {
		n := 1 + int(b[0]-0xc0)
		return n, n <= len(b)
	}
	lengthLength := int(b[0] - 0xf7)
	if 2 < lengthLength || len(b) < 1+lengthLength || b[1] == 0 {
		return 0, false
	}
	length := 0
	for _, c := range b[1 : 1+lengthLength] {
		length = length<<8 | int(c)
	}
	if length < 56 {
		// a short list must use the single-byte form
		return 0, false
	}
	n := 1 + lengthLength + length
	return n, n <= len(b)
}

// ethereumDiscv4Packet returns the end of the packet's RLP list, so the caller
// can check any trailing bytes for BitTorrent signatures.
func ethereumDiscv4Packet(b []byte) (int, bool) {
	if len(b) <= ethereumDiscv4HeaderLength || ethereumDiscv4MaxPacketSize < len(b) {
		return 0, false
	}
	packetType := b[ethereumDiscv4HeaderLength-1]
	if packetType < ethereumDiscv4PacketTypePing || ethereumDiscv4PacketTypeLast < packetType {
		return 0, false
	}
	listLength, ok := rlpListLength(b[ethereumDiscv4HeaderLength:])
	if !ok {
		return 0, false
	}
	if !legacyKeccak256Equals(b[ethereumDiscv4HashLength:], b[:ethereumDiscv4HashLength]) {
		return 0, false
	}
	return ethereumDiscv4HeaderLength + listLength, true
}

// isSecp256k1UncompressedPoint reports whether b is 0x04 || x || y with x, y
// field elements on secp256k1. The curve has cofactor 1, so every such point
// is a valid public key.
func isSecp256k1UncompressedPoint(b []byte) bool {
	if len(b) != secp256k1UncompressedLength || b[0] != secp256k1UncompressedFormat {
		return false
	}
	var x, y secp256k1.FieldVal
	if x.SetByteSlice(b[1:33]) || y.SetByteSlice(b[33:65]) {
		// a coordinate >= p
		return false
	}
	var y2, x3 secp256k1.FieldVal
	y2.SquareVal(&y).Normalize()
	x3.SquareVal(&x).Mul(&x).AddInt(7).Normalize()
	return y2.Equals(&x3)
}

// ethereumRlpxAuth recognizes the initiator's complete auth message (EIP-8 or
// pre-EIP-8) as the whole of b. It returns the end of the ephemeral key, so
// the caller checks the bytes after it for BitTorrent signatures.
func ethereumRlpxAuth(b []byte) (int, bool) {
	if ethereumRlpxEip8SizeLength+ethereumRlpxEip8MinAuthSize <= len(b) &&
		int(binary.BigEndian.Uint16(b[0:2])) == len(b)-ethereumRlpxEip8SizeLength {
		end := ethereumRlpxEip8SizeLength + secp256k1UncompressedLength
		if isSecp256k1UncompressedPoint(b[ethereumRlpxEip8SizeLength:end]) {
			return end, true
		}
	}
	if len(b) == ethereumRlpxPreEip8AuthLength && isSecp256k1UncompressedPoint(b[:secp256k1UncompressedLength]) {
		return secp256k1UncompressedLength, true
	}
	return 0, false
}
