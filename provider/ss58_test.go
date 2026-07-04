package main

// ss58_test.go — vectors for the hand-rolled ss58 decoder. The positive
// vector is the canonical substrate "Alice" dev account on the generic
// prefix 42 network; corruption vectors are built at runtime with a
// test-local base58 encoder so each structural check (prefix, length,
// checksum) is exercised independently.

import (
	"bytes"
	"encoding/hex"
	"math/big"
	"testing"

	"golang.org/x/crypto/blake2b"
)

// base58EncodeForTest is a test-only big.Int-based encoder, deliberately
// a different algorithm than the byte-bignum decoder it is used to
// exercise.
func base58EncodeForTest(b []byte) string {
	n := new(big.Int).SetBytes(b)
	base := big.NewInt(58)
	mod := new(big.Int)
	encoded := []byte{}
	for 0 < n.Sign() {
		n.DivMod(n, base, mod)
		encoded = append([]byte{base58Alphabet[mod.Int64()]}, encoded...)
	}
	for _, v := range b {
		if v != 0 {
			break
		}
		encoded = append([]byte{'1'}, encoded...)
	}
	return string(encoded)
}

// ss58EncodeForTest builds an ss58 address from raw prefix bytes and a
// pubkey, with a correct checksum unless `corruptChecksum` is set.
func ss58EncodeForTest(prefixBytes []byte, pubkey []byte, corruptChecksum bool) string {
	payload := append(append([]byte{}, prefixBytes...), pubkey...)
	checksum := blake2b.Sum512(append([]byte("SS58PRE"), payload...))
	if corruptChecksum {
		checksum[0] ^= 0xff
	}
	return base58EncodeForTest(append(payload, checksum[:2]...))
}

func TestBase58Decode(t *testing.T) {
	// canonical bitcoin-suite vector
	decoded, err := base58Decode("StV1DL6CwTryKyV")
	if err != nil {
		t.Fatalf("decode: %s", err)
	}
	if !bytes.Equal(decoded, []byte("hello world")) {
		t.Fatalf("decoded %q", decoded)
	}

	// leading '1' characters decode to leading zero bytes
	decoded, err = base58Decode("1111")
	if err != nil {
		t.Fatalf("decode: %s", err)
	}
	if !bytes.Equal(decoded, []byte{0, 0, 0, 0}) {
		t.Fatalf("decoded %x", decoded)
	}

	// empty input decodes to empty output
	decoded, err = base58Decode("")
	if err != nil || len(decoded) != 0 {
		t.Fatalf("decoded %x err %v", decoded, err)
	}

	// characters outside the bitcoin alphabet are rejected
	for _, invalid := range []string{"0", "O", "I", "l", "hello world"} {
		if _, err := base58Decode(invalid); err == nil {
			t.Errorf("expected error for %q", invalid)
		}
	}
}

func TestParseSs58Coldkey(t *testing.T) {
	// substrate dev account "Alice" on the generic prefix 42 network
	pubkey, err := parseSs58Coldkey("5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY")
	if err != nil {
		t.Fatalf("parse: %s", err)
	}
	expectedPubkey, _ := hex.DecodeString("d43593c715fdd31c61141abd04a99fd6822c8558854ccde39a5684e7a56da27d")
	if !bytes.Equal(pubkey[:], expectedPubkey) {
		t.Fatalf("pubkey 0x%x", pubkey)
	}

	// round trip: re-encoding the decoded pubkey with prefix 42 must
	// reproduce the input address
	if encoded := ss58EncodeForTest([]byte{42}, pubkey[:], false); encoded != "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY" {
		t.Fatalf("re-encoded %q", encoded)
	}
}

func TestParseSs58ColdkeyCorruption(t *testing.T) {
	alicePubkey, _ := hex.DecodeString("d43593c715fdd31c61141abd04a99fd6822c8558854ccde39a5684e7a56da27d")

	cases := []struct {
		name        string
		coldkeySs58 string
	}{
		{"flipped last character", "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQZ"},
		{"flipped middle character", "5GrwvaEF5zXb26Fz9rcQpDXS57CtERHpNehXCPcNoHGKutQY"},
		{"invalid base58 character", "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQ0"},
		{"empty", ""},
		{"truncated", "5Grwva"},
		{"wrong prefix 0 with valid checksum", ss58EncodeForTest([]byte{0}, alicePubkey, false)},
		{"wrong prefix 1 with valid checksum", ss58EncodeForTest([]byte{1}, alicePubkey, false)},
		{"prefix 42 with corrupted checksum", ss58EncodeForTest([]byte{42}, alicePubkey, true)},
		{"2-byte prefix form", ss58EncodeForTest([]byte{0x50, 0x00}, alicePubkey, false)},
		{"31-byte account id", ss58EncodeForTest([]byte{42}, alicePubkey[:31], false)},
		{"33-byte account id", ss58EncodeForTest([]byte{42}, append(alicePubkey, 0x01), false)},
	}
	for _, c := range cases {
		if _, err := parseSs58Coldkey(c.coldkeySs58); err == nil {
			t.Errorf("%s: expected an error for %q", c.name, c.coldkeySs58)
		}
	}
}
