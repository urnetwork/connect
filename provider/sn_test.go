package main

// sn_test.go — vectors for the head-tier bind intent (`provider bind-head`).
// The digest read from the contract is stubbed by recomputing headBindDigest
// locally exactly as the Solidity view does (the 168-byte domain-separated
// preimage pinned by sn/evm/test/HeadBinding.t.sol
// test_headBindDigest_exactBytes), so the test needs no RPC. The signature is
// produced by the provider's client Ed25519 key and must verify under that
// key with the (r,s) byte split the 0x402 precompile / contract uses.

import (
	"crypto/ed25519"
	"encoding/binary"
	"fmt"
	"testing"
)

// snHeadBindDomain is keccak256("UR_ST_HEAD_BIND_V1") — HEAD_BIND_DOMAIN in
// the contract.
func snHeadBindDomain() [32]byte {
	return keccak256([]byte("UR_ST_HEAD_BIND_V1"))
}

// snHeadBindDigestLocal recomputes headBindDigest the way STSubnet.sol does:
//
//	keccak256(HEAD_BIND_DOMAIN ‖ chainid(32) ‖ contract(20) ‖ registrant(20)
//	          ‖ hotkey(32) ‖ clientId(32))   — a 168-byte preimage.
func snHeadBindDigestLocal(chainId uint64, contract [20]byte, registrant [20]byte, hotkey [32]byte, clientId [32]byte) [32]byte {
	domain := snHeadBindDomain()
	var chainWord [32]byte
	binary.BigEndian.PutUint64(chainWord[24:], chainId)
	preimage := make([]byte, 0, 168)
	preimage = append(preimage, domain[:]...)
	preimage = append(preimage, chainWord[:]...)
	preimage = append(preimage, contract[:]...)
	preimage = append(preimage, registrant[:]...)
	preimage = append(preimage, hotkey[:]...)
	preimage = append(preimage, clientId[:]...)
	if len(preimage) != 168 {
		panic("head-bind preimage must be 168 bytes")
	}
	return keccak256(preimage)
}

func TestSnHeadBindDigestCalldataGolden(t *testing.T) {
	// headBindDigest(address,bytes32,bytes32): selector then three head words —
	// registrant left-padded to 32 bytes, hotkey, clientId.
	//   selector  keccak256("headBindDigest(address,bytes32,bytes32)")[:4]
	//             = 850f7cb3 (the abigen method id in sn/stabi)
	// 4 + 32*3 = 100 bytes total.
	if got := fmt.Sprintf("%x", evmSelector(snHeadBindDigestSignature)); got != "850f7cb3" {
		t.Fatalf("headBindDigest selector = %s; expected 850f7cb3 (stabi method id)", got)
	}

	var registrant [20]byte
	for i := range registrant {
		registrant[i] = 0x22
	}
	hotkey := hex32(t, "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1")
	clientId := hex32(t, "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2")

	expectedHex := "850f7cb3" +
		"0000000000000000000000002222222222222222222222222222222222222222" +
		"a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1" +
		"b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
	got := fmt.Sprintf("%x", snHeadBindDigestCalldata(registrant, hotkey, clientId))
	if got != expectedHex {
		t.Fatalf("calldata\n%s\nexpected\n%s", got, expectedHex)
	}
}

func TestSnSignBindHead(t *testing.T) {
	// Deterministic client key seed so the vector is reproducible. This is the
	// `.provider.key` identity; clientId is its Ed25519 public key.
	var seed [32]byte
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	privateKey := ed25519.NewKeyFromSeed(seed[:])
	var clientId [32]byte
	copy(clientId[:], privateKey.Public().(ed25519.PublicKey))

	var contract [20]byte
	for i := range contract {
		contract[i] = 0x33
	}
	var registrant [20]byte
	for i := range registrant {
		registrant[i] = 0x44
	}
	hotkey := hex32(t, "1234123412341234123412341234123412341234123412341234123412341234")

	// Stub the digest read: recompute it locally exactly as the contract view
	// would return it. chainid is folded into the digest by the contract, so
	// the provider signs whatever the eth_call returns.
	const chainId = uint64(945)
	digest := snHeadBindDigestLocal(chainId, contract, registrant, hotkey, clientId)

	intent := snSignBindHead(privateKey, registrant, hotkey, digest)

	if intent.clientId != clientId {
		t.Fatalf("intent client_id 0x%x; expected 0x%x", intent.clientId, clientId)
	}
	if intent.hotkey != hotkey || intent.registrant != registrant || intent.digest != digest {
		t.Fatalf("intent echoed inputs wrong: %+v", intent)
	}
	if len(intent.clientIdSig) != ed25519.SignatureSize {
		t.Fatalf("sig length %d; expected %d", len(intent.clientIdSig), ed25519.SignatureSize)
	}

	// The signature verifies under the client public key over the digest —
	// this is exactly what the contract's 0x402 verify(digest, clientId, r, s)
	// decides on-chain.
	if !ed25519.Verify(clientId[:], digest[:], intent.clientIdSig) {
		t.Fatalf("client_id signature does not verify under the client key")
	}

	// (r,s) mapping: the contract reads r = sig[0:32], s = sig[32:64] and
	// recombines them for the precompile. Reconstructing R‖S from those halves
	// must reproduce a verifying signature — i.e. ed25519.Sign's byte order is
	// the order the contract expects, no swap.
	r := intent.clientIdSig[0:32]
	s := intent.clientIdSig[32:64]
	recombined := append(append([]byte{}, r...), s...)
	if !ed25519.Verify(clientId[:], digest[:], recombined) {
		t.Fatalf("r‖s recombination does not verify — signature byte order mismatch")
	}

	// A signature is bound to the digest: any other digest (different
	// registrant, hotkey, contract, chain, or client_id) must fail, which is
	// why snclaim re-derives the digest under its own sender before submitting.
	otherRegistrant := registrant
	otherRegistrant[0] ^= 0x01
	otherDigest := snHeadBindDigestLocal(chainId, contract, otherRegistrant, hotkey, clientId)
	if ed25519.Verify(clientId[:], otherDigest[:], intent.clientIdSig) {
		t.Fatalf("signature verified for a different registrant's digest")
	}
}

// TestSnBindHeadArgParsers covers the hotkey/registrant/contract argument
// parsers (0x-optional hex, exact length enforced).
func TestSnBindHeadArgParsers(t *testing.T) {
	hk, err := parseBytes32Arg("--hotkey", "0x"+testPayoutColdkeyHex)
	if err != nil || fmt.Sprintf("%x", hk) != testPayoutColdkeyHex {
		t.Fatalf("parseBytes32Arg 0x-prefixed: %x err %v", hk, err)
	}
	if _, err := parseBytes32Arg("--hotkey", testPayoutColdkeyHex); err != nil {
		t.Fatalf("parseBytes32Arg bare hex: %v", err)
	}
	if _, err := parseBytes32Arg("--hotkey", "0x1234"); err == nil {
		t.Fatalf("parseBytes32Arg accepted short hex")
	}

	addr, err := parseEvmAddressArg("--registrant", "0x2222222222222222222222222222222222222222")
	if err != nil || fmt.Sprintf("%x", addr) != "2222222222222222222222222222222222222222" {
		t.Fatalf("parseEvmAddressArg: %x err %v", addr, err)
	}
	if _, err := parseEvmAddressArg("--registrant", "0x22"); err == nil {
		t.Fatalf("parseEvmAddressArg accepted short address")
	}
}
