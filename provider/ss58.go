package main

// ss58.go — minimal ss58 (substrate) address decoding for the subnet
// claim wallet (sn/PLAN.md 7.3, decision D-2). The claim wallet is a
// bittensor coldkey: an ss58-encoded 32-byte account public key with
// network prefix 42. The provider only ever validates and forwards the
// address — it never signs with this key — so a hand-rolled decoder
// (stdlib + x/crypto blake2b) is used instead of a substrate module
// dependency.
//
// ss58 format for 32-byte account ids:
//
//	base58( prefixBytes ‖ pubkey(32) ‖ checksum(2) )
//
// where prefixBytes is 1 byte for network ids 0..63 (2 bytes above
// that, with the first byte >= 64) and
// checksum = blake2b-512("SS58PRE" ‖ prefixBytes ‖ pubkey)[:2].

import (
	"bytes"
	"fmt"
	"strings"

	"golang.org/x/crypto/blake2b"
)

// ss58ColdkeyPrefix is the generic substrate network id; bittensor
// coldkeys use it. All other prefixes are rejected.
const ss58ColdkeyPrefix = 42

// base58Alphabet is the bitcoin base58 alphabet, which ss58 uses.
const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// base58Decode decodes a bitcoin-alphabet base58 string, including one
// leading zero byte per leading '1' character. The accumulator is a
// big-endian byte bignum; addresses are ~50 characters so the quadratic
// inner loop is fine.
func base58Decode(encoded string) ([]byte, error) {
	decoded := []byte{}
	for i := 0; i < len(encoded); i++ {
		digit := strings.IndexByte(base58Alphabet, encoded[i])
		if digit < 0 {
			return nil, fmt.Errorf("invalid base58 character %q", encoded[i])
		}
		// decoded = decoded*58 + digit
		carry := digit
		for j := len(decoded) - 1; 0 <= j; j -= 1 {
			v := int(decoded[j])*58 + carry
			decoded[j] = byte(v & 0xff)
			carry = v >> 8
		}
		for 0 < carry {
			decoded = append([]byte{byte(carry & 0xff)}, decoded...)
			carry >>= 8
		}
	}
	zeroCount := 0
	for zeroCount < len(encoded) && encoded[zeroCount] == '1' {
		zeroCount += 1
	}
	return append(make([]byte, zeroCount), decoded...), nil
}

// parseSs58Coldkey decodes and validates an ss58 address, returning the
// 32-byte account public key. Only prefix-42 addresses of 32-byte
// account ids are accepted; the blake2b checksum is enforced. Network
// ids >= 64 use a 2-byte prefix whose first byte is >= 64, so they can
// never be prefix 42 and are rejected before length checks.
func parseSs58Coldkey(coldkeySs58 string) ([32]byte, error) {
	var pubkey [32]byte
	raw, err := base58Decode(coldkeySs58)
	if err != nil {
		return pubkey, err
	}
	if len(raw) == 0 {
		return pubkey, fmt.Errorf("empty ss58 address")
	}
	if 64 <= raw[0] {
		return pubkey, fmt.Errorf("unsupported ss58 prefix (2-byte form); expected prefix %d", ss58ColdkeyPrefix)
	}
	if len(raw) != 1+32+2 {
		return pubkey, fmt.Errorf("bad ss58 length %d; expected a 32-byte account id", len(raw))
	}
	if raw[0] != ss58ColdkeyPrefix {
		return pubkey, fmt.Errorf("ss58 prefix %d; expected prefix %d", raw[0], ss58ColdkeyPrefix)
	}
	checksum := blake2b.Sum512(append([]byte("SS58PRE"), raw[:1+32]...))
	if !bytes.Equal(checksum[:2], raw[1+32:]) {
		return pubkey, fmt.Errorf("bad ss58 checksum")
	}
	copy(pubkey[:], raw[1:1+32])
	return pubkey, nil
}
