package connect

// Cryptographic coverage for the signed client-key registration verifier.
//
// `transfer_key_history_test.go` pins the wire format against golden bytes from
// the canonical implementation. This file tests the primitives underneath it —
// address derivation, signature canonicalisation, what the digest actually
// binds, and every chain invariant separately — using a local signer so that
// arbitrary (including malicious) chains can be constructed.
//
// The signer here is test-only. Nothing in the shipping client signs a
// registration; only operators do.

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"reflect"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"golang.org/x/crypto/sha3"
)

// goldenSignerPrivateKeyHex is the key the golden vectors were signed with. Its
// Ethereum address is a published test vector, which is what makes
// TestClientKeyAddressDerivationMatchesKnownAddress meaningful.
const goldenSignerPrivateKeyHex = "4c0883a69102937d6231471b5dbb6204fe512961708279a2b0a19a7c0f6e1d9c"

// goldenSignerAddress is that key's address, as the canonical implementation
// reports it (lowercased here; the check is on bytes, not on checksum casing).
const goldenSignerAddress = "f2fdd092e615e638930d31467ededb0fd15696b0"

type testClientKeySigner struct {
	privateKey *secp256k1.PrivateKey
	address    ClientKeyAddress
}

func newTestClientKeySigner(t *testing.T, privateKeyHex string) *testClientKeySigner {
	t.Helper()
	raw, err := hex.DecodeString(privateKeyHex)
	if err != nil {
		t.Fatalf("decode private key: %s", err)
	}
	privateKey := secp256k1.PrivKeyFromBytes(raw)
	uncompressed := privateKey.PubKey().SerializeUncompressed()
	var address ClientKeyAddress
	sum := keccak256(uncompressed[1:])
	copy(address[:], sum[len(sum)-clientKeyAddressSize:])
	return &testClientKeySigner{privateKey: privateKey, address: address}
}

// keccak256 mirrors the derivation in recoverClientKeySigner so the test can
// compute an expected address independently of the code under test being
// correct about which bytes it hashes.
func keccak256(data []byte) []byte {
	h := sha3.NewLegacyKeccak256()
	h.Write(data)
	return h.Sum(nil)
}

// sign produces a wire-format signature: R || S || V with a low-s scalar and a
// recovery code of 0 or 1.
func (self *testClientKeySigner) sign(t *testing.T, digest [32]byte) [clientKeySignatureSize]byte {
	t.Helper()
	compact := ecdsa.SignCompact(self.privateKey, digest[:], false)
	if len(compact) != clientKeySignatureSize {
		t.Fatalf("compact signature width = %d", len(compact))
	}
	var signature [clientKeySignatureSize]byte
	copy(signature[:64], compact[1:])
	signature[64] = compact[0] - clientKeyRecoveryMagic
	return signature
}

func (self *testClientKeySigner) signRegistration(t *testing.T, registration ClientKeyRegistration) ClientKeyRegistration {
	t.Helper()
	registration.Schema = clientKeyRegistrationSchema
	registration.Signer = self.address
	digest, err := registration.Digest()
	if err != nil {
		t.Fatalf("digest: %s", err)
	}
	registration.Signature = self.sign(t, digest)
	return registration
}

func testClientKeyDomain() ClientKeyHistoryDomain {
	var genesis, deployment, policy [32]byte
	for i := range genesis {
		genesis[i] = byte(1 + i)
		deployment[i] = byte(2 + i)
		policy[i] = byte(3 + i)
	}
	var coordinator, vault ClientKeyAddress
	for i := range coordinator {
		coordinator[i] = byte(0xa0 + i)
		vault[i] = byte(0xb0 + i)
	}
	return ClientKeyHistoryDomain{
		ChainID:          945,
		GenesisHash:      genesis,
		Netuid:           521,
		Coordinator:      coordinator,
		SettlementVault:  vault,
		DeploymentIDHash: deployment,
		PolicyHash:       policy,
		NoID:             7,
	}
}

func testClientKeyBoundary(block uint64) ClientKeyEffectiveBoundary {
	var hash [32]byte
	for i := range hash {
		hash[i] = byte(0x40 + i)
	}
	return ClientKeyEffectiveBoundary{Epoch: 4, Block: block, Hash: hash}
}

func testClientKeyPublicKey(seed byte) [32]byte {
	var key [32]byte
	for i := range key {
		key[i] = seed + byte(i)
	}
	return key
}

func testClientKeyNetworkId() [16]byte {
	var id [16]byte
	for i := range id {
		id[i] = 0x20 + byte(i)
	}
	return id
}

// buildTestChain signs a contiguous chain whose generation N carries keys[N-1].
func buildTestChain(t *testing.T, signer *testClientKeySigner, clientId Id, keys [][32]byte) []ClientKeyRegistration {
	t.Helper()
	chain := []ClientKeyRegistration{}
	var previousHash [32]byte
	for index, key := range keys {
		registration := signer.signRegistration(t, ClientKeyRegistration{
			Domain:            testClientKeyDomain(),
			ClientID:          [16]byte(clientId),
			NetworkID:         testClientKeyNetworkId(),
			Generation:        uint64(index + 1),
			Present:           true,
			PublicKey:         key,
			PreviousHash:      previousHash,
			EffectiveBoundary: testClientKeyBoundary(uint64(1000 + index)),
		})
		contentHash, err := registration.ContentHash()
		if err != nil {
			t.Fatalf("generation %d content hash: %s", index+1, err)
		}
		previousHash = contentHash
		chain = append(chain, registration)
	}
	return chain
}

func encodeTestChain(t *testing.T, chain []ClientKeyRegistration) [][]byte {
	t.Helper()
	encoded := [][]byte{}
	for _, registration := range chain {
		registrationBytes, err := json.Marshal(registration)
		if err != nil {
			t.Fatalf("marshal: %s", err)
		}
		encoded = append(encoded, registrationBytes)
	}
	return encoded
}

func testPolicyFor(t *testing.T, signer *testClientKeySigner) ClientKeyHistoryPolicy {
	t.Helper()
	digest, err := testClientKeyDomain().Digest()
	if err != nil {
		t.Fatalf("domain digest: %s", err)
	}
	return ClientKeyHistoryPolicy{
		TrustedSigners: []ClientKeyTrustedSigner{
			ClientKeyTrustedSigner{DomainDigest: digest, Signer: signer.address},
		},
		Pin:            nil,
		MaxGenerations: 16,
	}
}

func testClientId() Id {
	var id Id
	for i := range id {
		id[i] = 0x10 + byte(i)
	}
	return id
}

// ---------------------------------------------------------------------------
// address derivation
// ---------------------------------------------------------------------------

// The address is the trailing 20 bytes of Keccak-256 over the uncompressed
// public key minus its 0x04 tag. Getting the tag handling wrong yields a
// plausible-looking but wrong address, which would make every signature check
// silently compare against the wrong signer.
func TestClientKeyAddressDerivationMatchesKnownAddress(t *testing.T) {
	signer := newTestClientKeySigner(t, goldenSignerPrivateKeyHex)
	if got := hex.EncodeToString(signer.address[:]); got != goldenSignerAddress {
		t.Fatalf("derived address\n got %s\nwant %s", got, goldenSignerAddress)
	}

	// and the code under test must recover the same address from a signature
	digest := [32]byte{}
	copy(digest[:], []byte("a fixed 32-byte digest for tests."))
	signature := signer.sign(t, digest)
	recovered, err := recoverClientKeySigner(digest, signature)
	if err != nil {
		t.Fatalf("recover: %s", err)
	}
	if recovered != signer.address {
		t.Fatalf("recovered %s, want %s", recovered, signer.address)
	}
}

func TestClientKeyAddressJSONRoundTrip(t *testing.T) {
	signer := newTestClientKeySigner(t, goldenSignerPrivateKeyHex)
	encoded, err := json.Marshal(signer.address)
	if err != nil {
		t.Fatalf("marshal: %s", err)
	}
	if string(encoded) != `"0x`+goldenSignerAddress+`"` {
		t.Fatalf("encoding = %s", encoded)
	}
	var decoded ClientKeyAddress
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %s", err)
	}
	if decoded != signer.address {
		t.Fatal("round trip changed the address")
	}

	for _, bad := range []string{
		`"0xf2fdd092e615e638930d31467ededb0fd15696"`,     // short
		`"0xf2fdd092e615e638930d31467ededb0fd15696b0aa"`, // long
		`"f2fdd092e615e638930d31467ededb0fd15696b0"`,     // no 0x
		`"0xzzfdd092e615e638930d31467ededb0fd15696b0"`,   // non-hex
	} {
		var out ClientKeyAddress
		if err := json.Unmarshal([]byte(bad), &out); err == nil {
			t.Fatalf("accepted malformed address %s", bad)
		}
	}
}

// ---------------------------------------------------------------------------
// signature canonicalisation
// ---------------------------------------------------------------------------

func TestSignatureRejectsNonCanonicalValues(t *testing.T) {
	signer := newTestClientKeySigner(t, goldenSignerPrivateKeyHex)
	var digest [32]byte
	copy(digest[:], []byte("another fixed 32-byte test digest"))
	valid := signer.sign(t, digest)

	if _, err := recoverClientKeySigner(digest, valid); err != nil {
		t.Fatalf("baseline signature rejected: %s", err)
	}

	cases := map[string]func(sig [clientKeySignatureSize]byte) [clientKeySignatureSize]byte{
		"recovery code 2": func(sig [clientKeySignatureSize]byte) [clientKeySignatureSize]byte {
			sig[64] = 2
			return sig
		},
		"recovery code 27 (unconverted)": func(sig [clientKeySignatureSize]byte) [clientKeySignatureSize]byte {
			sig[64] = 27
			return sig
		},
		"r = 0": func(sig [clientKeySignatureSize]byte) [clientKeySignatureSize]byte {
			copy(sig[:32], make([]byte, 32))
			return sig
		},
		"s = 0": func(sig [clientKeySignatureSize]byte) [clientKeySignatureSize]byte {
			copy(sig[32:64], make([]byte, 32))
			return sig
		},
		"r = n": func(sig [clientKeySignatureSize]byte) [clientKeySignatureSize]byte {
			copy(sig[:32], clientKeyCurveOrder.Bytes())
			return sig
		},
		"s = n": func(sig [clientKeySignatureSize]byte) [clientKeySignatureSize]byte {
			copy(sig[32:64], clientKeyCurveOrder.Bytes())
			return sig
		},
		"s = halfN + 1 (high s)": func(sig [clientKeySignatureSize]byte) [clientKeySignatureSize]byte {
			high := new(big.Int).Add(clientKeyCurveHalfOrder, big.NewInt(1))
			var padded [32]byte
			b := high.Bytes()
			copy(padded[32-len(b):], b)
			copy(sig[32:64], padded[:])
			return sig
		},
	}
	for name, mutate := range cases {
		if _, err := recoverClientKeySigner(digest, mutate(valid)); !errors.Is(err, ErrClientKeyRegistrationInvalid) {
			t.Fatalf("%s accepted, err = %v", name, err)
		}
	}
}

// s == halfN is the largest legal value; rejecting it would be an off-by-one
// that silently refuses a minority of otherwise valid signatures.
func TestSignatureAcceptsBoundaryLowS(t *testing.T) {
	var digest [32]byte
	copy(digest[:], []byte("boundary low-s digest for testing"))
	var signature [clientKeySignatureSize]byte
	signature[31] = 1 // r = 1
	half := clientKeyCurveHalfOrder.Bytes()
	copy(signature[64-len(half):64], half)
	signature[64] = 0
	_, err := recoverClientKeySigner(digest, signature)
	// It will not recover to any particular signer, but it must NOT be refused
	// by the canonicalisation check.
	if err != nil && !bytes.Contains([]byte(err.Error()), []byte("does not recover")) {
		t.Fatalf("s == halfN refused by canonicalisation: %v", err)
	}
}

// A signature is bound to one digest. Splicing a valid signature onto a
// different record must fail, or a single signed record could authorise any
// other.
func TestSignatureIsNotTransferableBetweenRecords(t *testing.T) {
	signer := newTestClientKeySigner(t, goldenSignerPrivateKeyHex)
	chain := buildTestChain(t, signer, testClientId(), [][32]byte{
		testClientKeyPublicKey(0x50),
		testClientKeyPublicKey(0x60),
	})
	spliced := chain[1]
	spliced.Signature = chain[0].Signature
	if err := spliced.VerifySignature(); !errors.Is(err, ErrClientKeyRegistrationInvalid) {
		t.Fatalf("spliced signature accepted, err = %v", err)
	}
}

// ---------------------------------------------------------------------------
// what the digest binds
// ---------------------------------------------------------------------------

// Every semantic field must be covered by the signature. A field left out of
// the signing payload would be freely mutable by whoever relays the record —
// which for `PublicKey` would be the whole attack.
func TestDigestBindsEveryField(t *testing.T) {
	signer := newTestClientKeySigner(t, goldenSignerPrivateKeyHex)
	base := signer.signRegistration(t, ClientKeyRegistration{
		Domain:            testClientKeyDomain(),
		ClientID:          [16]byte(testClientId()),
		NetworkID:         testClientKeyNetworkId(),
		Generation:        2,
		Present:           true,
		PublicKey:         testClientKeyPublicKey(0x50),
		PreviousHash:      testClientKeyPublicKey(0x90),
		EffectiveBoundary: testClientKeyBoundary(1000),
	})
	baseDigest, err := base.Digest()
	if err != nil {
		t.Fatalf("base digest: %s", err)
	}

	mutations := map[string]func(r ClientKeyRegistration) ClientKeyRegistration{
		"ClientID":                func(r ClientKeyRegistration) ClientKeyRegistration { r.ClientID[0] ^= 1; return r },
		"NetworkID":               func(r ClientKeyRegistration) ClientKeyRegistration { r.NetworkID[0] ^= 1; return r },
		"Generation":              func(r ClientKeyRegistration) ClientKeyRegistration { r.Generation += 1; return r },
		"PublicKey":               func(r ClientKeyRegistration) ClientKeyRegistration { r.PublicKey[0] ^= 1; return r },
		"PreviousHash":            func(r ClientKeyRegistration) ClientKeyRegistration { r.PreviousHash[0] ^= 1; return r },
		"Signer":                  func(r ClientKeyRegistration) ClientKeyRegistration { r.Signer[0] ^= 1; return r },
		"Boundary.Epoch":          func(r ClientKeyRegistration) ClientKeyRegistration { r.EffectiveBoundary.Epoch += 1; return r },
		"Boundary.Block":          func(r ClientKeyRegistration) ClientKeyRegistration { r.EffectiveBoundary.Block += 1; return r },
		"Boundary.Hash":           func(r ClientKeyRegistration) ClientKeyRegistration { r.EffectiveBoundary.Hash[0] ^= 1; return r },
		"Domain.ChainID":          func(r ClientKeyRegistration) ClientKeyRegistration { r.Domain.ChainID += 1; return r },
		"Domain.GenesisHash":      func(r ClientKeyRegistration) ClientKeyRegistration { r.Domain.GenesisHash[0] ^= 1; return r },
		"Domain.Netuid":           func(r ClientKeyRegistration) ClientKeyRegistration { r.Domain.Netuid += 1; return r },
		"Domain.Coordinator":      func(r ClientKeyRegistration) ClientKeyRegistration { r.Domain.Coordinator[0] ^= 1; return r },
		"Domain.SettlementVault":  func(r ClientKeyRegistration) ClientKeyRegistration { r.Domain.SettlementVault[0] ^= 1; return r },
		"Domain.DeploymentIDHash": func(r ClientKeyRegistration) ClientKeyRegistration { r.Domain.DeploymentIDHash[0] ^= 1; return r },
		"Domain.PolicyHash":       func(r ClientKeyRegistration) ClientKeyRegistration { r.Domain.PolicyHash[0] ^= 1; return r },
		"Domain.NoID":             func(r ClientKeyRegistration) ClientKeyRegistration { r.Domain.NoID += 1; return r },
	}
	for name, mutate := range mutations {
		mutated := mutate(base)
		mutatedDigest, err := mutated.Digest()
		if err != nil {
			t.Fatalf("%s: digest: %s", name, err)
		}
		if mutatedDigest == baseDigest {
			t.Fatalf("%s is NOT bound by the signing digest", name)
		}
		// and the original signature must no longer verify
		if err := mutated.VerifySignature(); !errors.Is(err, ErrClientKeyRegistrationInvalid) {
			t.Fatalf("%s: mutated record still verifies", name)
		}
	}

	// `Present` is checked separately because it must move with PublicKey:
	// Digest() refuses a record where the two disagree.
	tombstone := signer.signRegistration(t, ClientKeyRegistration{
		Domain:            testClientKeyDomain(),
		ClientID:          [16]byte(testClientId()),
		NetworkID:         testClientKeyNetworkId(),
		Generation:        2,
		Present:           false,
		PublicKey:         [32]byte{},
		PreviousHash:      testClientKeyPublicKey(0x90),
		EffectiveBoundary: testClientKeyBoundary(1000),
	})
	tombstoneDigest, err := tombstone.Digest()
	if err != nil {
		t.Fatalf("tombstone digest: %s", err)
	}
	if tombstoneDigest == baseDigest {
		t.Fatal("Present/PublicKey are not bound by the signing digest")
	}
}

// The reflection check is a guard against a future field being added to the
// struct and silently left out of the signing payload.
func TestDigestCoversEveryStructField(t *testing.T) {
	expected := map[string]bool{
		// signed
		"Domain": true, "ClientID": true, "NetworkID": true, "Generation": true,
		"Present": true, "PublicKey": true, "PreviousHash": true,
		"EffectiveBoundary": true, "Signer": true,
		// not signed, by construction
		"Schema":    true, // bound as the domain-separation tag
		"Signature": true, // is the signature
	}
	registrationType := reflect.TypeFor[ClientKeyRegistration]()
	for i := range registrationType.NumField() {
		name := registrationType.Field(i).Name
		if !expected[name] {
			t.Fatalf("ClientKeyRegistration.%s is new and TestDigestBindsEveryField does not cover it", name)
		}
	}
}

func TestDomainDigestBindsEveryField(t *testing.T) {
	base := testClientKeyDomain()
	baseDigest, err := base.Digest()
	if err != nil {
		t.Fatalf("base: %s", err)
	}
	mutations := []func(d ClientKeyHistoryDomain) ClientKeyHistoryDomain{
		func(d ClientKeyHistoryDomain) ClientKeyHistoryDomain { d.ChainID += 1; return d },
		func(d ClientKeyHistoryDomain) ClientKeyHistoryDomain { d.GenesisHash[0] ^= 1; return d },
		func(d ClientKeyHistoryDomain) ClientKeyHistoryDomain { d.Netuid += 1; return d },
		func(d ClientKeyHistoryDomain) ClientKeyHistoryDomain { d.Coordinator[0] ^= 1; return d },
		func(d ClientKeyHistoryDomain) ClientKeyHistoryDomain { d.SettlementVault[0] ^= 1; return d },
		func(d ClientKeyHistoryDomain) ClientKeyHistoryDomain { d.DeploymentIDHash[0] ^= 1; return d },
		func(d ClientKeyHistoryDomain) ClientKeyHistoryDomain { d.PolicyHash[0] ^= 1; return d },
		func(d ClientKeyHistoryDomain) ClientKeyHistoryDomain { d.NoID += 1; return d },
	}
	seen := map[[32]byte]bool{baseDigest: true}
	for index, mutate := range mutations {
		digest, err := mutate(base).Digest()
		if err != nil {
			t.Fatalf("mutation %d: %s", index, err)
		}
		if seen[digest] {
			t.Fatalf("domain mutation %d does not change the digest", index)
		}
		seen[digest] = true
	}
}

func TestDigestValidationRejectsMalformedRecords(t *testing.T) {
	valid := ClientKeyRegistration{
		Schema:            clientKeyRegistrationSchema,
		Domain:            testClientKeyDomain(),
		ClientID:          [16]byte(testClientId()),
		NetworkID:         testClientKeyNetworkId(),
		Generation:        2,
		Present:           true,
		PublicKey:         testClientKeyPublicKey(0x50),
		PreviousHash:      testClientKeyPublicKey(0x90),
		EffectiveBoundary: testClientKeyBoundary(1000),
		Signer:            ClientKeyAddress{1},
	}
	if _, err := valid.Digest(); err != nil {
		t.Fatalf("baseline record rejected: %s", err)
	}

	cases := map[string]func(r ClientKeyRegistration) ClientKeyRegistration{
		"wrong schema":                 func(r ClientKeyRegistration) ClientKeyRegistration { r.Schema = "other"; return r },
		"zero client id":               func(r ClientKeyRegistration) ClientKeyRegistration { r.ClientID = [16]byte{}; return r },
		"zero network id":              func(r ClientKeyRegistration) ClientKeyRegistration { r.NetworkID = [16]byte{}; return r },
		"zero generation":              func(r ClientKeyRegistration) ClientKeyRegistration { r.Generation = 0; return r },
		"zero signer":                  func(r ClientKeyRegistration) ClientKeyRegistration { r.Signer = ClientKeyAddress{}; return r },
		"present with zero key":        func(r ClientKeyRegistration) ClientKeyRegistration { r.PublicKey = [32]byte{}; return r },
		"absent with non-zero key":     func(r ClientKeyRegistration) ClientKeyRegistration { r.Present = false; return r },
		"generation 1 with prior hash": func(r ClientKeyRegistration) ClientKeyRegistration { r.Generation = 1; return r },
		"generation 2 without prior":   func(r ClientKeyRegistration) ClientKeyRegistration { r.PreviousHash = [32]byte{}; return r },
		"incomplete domain":            func(r ClientKeyRegistration) ClientKeyRegistration { r.Domain.NoID = 0; return r },
		"incomplete boundary":          func(r ClientKeyRegistration) ClientKeyRegistration { r.EffectiveBoundary.Block = 0; return r },
		"zero boundary hash":           func(r ClientKeyRegistration) ClientKeyRegistration { r.EffectiveBoundary.Hash = [32]byte{}; return r },
	}
	for name, mutate := range cases {
		if _, err := mutate(valid).Digest(); !errors.Is(err, ErrClientKeyRegistrationInvalid) {
			t.Fatalf("%s accepted, err = %v", name, err)
		}
	}
}

// ---------------------------------------------------------------------------
// content hash and chain linkage
// ---------------------------------------------------------------------------

// The signing digest and the content hash are different values over different
// bytes. Linking a successor on the digest instead of the content hash would
// still type-check and would still "work" in the happy path, so this is worth
// pinning explicitly.
func TestContentHashIsNotTheSigningDigest(t *testing.T) {
	signer := newTestClientKeySigner(t, goldenSignerPrivateKeyHex)
	chain := buildTestChain(t, signer, testClientId(), [][32]byte{testClientKeyPublicKey(0x50)})
	digest, err := chain[0].Digest()
	if err != nil {
		t.Fatalf("digest: %s", err)
	}
	contentHash, err := chain[0].ContentHash()
	if err != nil {
		t.Fatalf("content hash: %s", err)
	}
	if digest == contentHash {
		t.Fatal("content hash and signing digest are the same value")
	}
}

func TestChainLinksOnContentHashNotDigest(t *testing.T) {
	signer := newTestClientKeySigner(t, goldenSignerPrivateKeyHex)
	first := buildTestChain(t, signer, testClientId(), [][32]byte{testClientKeyPublicKey(0x50)})[0]
	digest, err := first.Digest()
	if err != nil {
		t.Fatalf("digest: %s", err)
	}
	// a successor that links on the signing digest rather than the content hash
	wrongLink := signer.signRegistration(t, ClientKeyRegistration{
		Domain:            testClientKeyDomain(),
		ClientID:          [16]byte(testClientId()),
		NetworkID:         testClientKeyNetworkId(),
		Generation:        2,
		Present:           true,
		PublicKey:         testClientKeyPublicKey(0x60),
		PreviousHash:      digest,
		EffectiveBoundary: testClientKeyBoundary(1001),
	})
	if err := wrongLink.follows(&first); !errors.Is(err, ErrClientKeyRegistrationInvalid) {
		t.Fatalf("successor linked on the signing digest accepted, err = %v", err)
	}
}

func TestFollowsInvariants(t *testing.T) {
	signer := newTestClientKeySigner(t, goldenSignerPrivateKeyHex)
	chain := buildTestChain(t, signer, testClientId(), [][32]byte{
		testClientKeyPublicKey(0x50),
		testClientKeyPublicKey(0x60),
	})
	prior, next := chain[0], chain[1]
	if err := next.follows(&prior); err != nil {
		t.Fatalf("baseline successor rejected: %s", err)
	}

	resign := func(mutate func(r ClientKeyRegistration) ClientKeyRegistration) ClientKeyRegistration {
		return signer.signRegistration(t, mutate(next))
	}

	cases := map[string]ClientKeyRegistration{
		"generation skipped": resign(func(r ClientKeyRegistration) ClientKeyRegistration {
			r.Generation = 3
			return r
		}),
		"domain changed": resign(func(r ClientKeyRegistration) ClientKeyRegistration {
			r.Domain.NoID = 9
			return r
		}),
		"client id changed": resign(func(r ClientKeyRegistration) ClientKeyRegistration {
			r.ClientID[0] ^= 1
			return r
		}),
		"network id changed": resign(func(r ClientKeyRegistration) ClientKeyRegistration {
			r.NetworkID[0] ^= 1
			return r
		}),
		"no value change": resign(func(r ClientKeyRegistration) ClientKeyRegistration {
			r.PublicKey = prior.PublicKey
			r.Present = prior.Present
			return r
		}),
		"epoch regression": resign(func(r ClientKeyRegistration) ClientKeyRegistration {
			r.EffectiveBoundary.Epoch = prior.EffectiveBoundary.Epoch - 1
			return r
		}),
		"block regression": resign(func(r ClientKeyRegistration) ClientKeyRegistration {
			r.EffectiveBoundary.Block = prior.EffectiveBoundary.Block - 1
			return r
		}),
		"same block, different boundary": resign(func(r ClientKeyRegistration) ClientKeyRegistration {
			r.EffectiveBoundary.Block = prior.EffectiveBoundary.Block
			r.EffectiveBoundary.Hash[0] ^= 1
			return r
		}),
	}
	for name, candidate := range cases {
		if err := candidate.follows(&prior); !errors.Is(err, ErrClientKeyRegistrationInvalid) {
			t.Fatalf("%s accepted, err = %v", name, err)
		}
	}

	// a chain root must be generation 1 with no predecessor
	if err := prior.follows(nil); err != nil {
		t.Fatalf("genuine root rejected: %s", err)
	}
	if err := next.follows(nil); !errors.Is(err, ErrClientKeyRegistrationInvalid) {
		t.Fatal("generation 2 accepted as a chain root")
	}
}

// ---------------------------------------------------------------------------
// whole-chain verification
// ---------------------------------------------------------------------------

func TestLongChainVerifiesEveryLink(t *testing.T) {
	signer := newTestClientKeySigner(t, goldenSignerPrivateKeyHex)
	keys := [][32]byte{}
	for i := range 6 {
		keys = append(keys, testClientKeyPublicKey(byte(0x50+0x10*i)))
	}
	chain := buildTestChain(t, signer, testClientId(), keys)
	encoded := encodeTestChain(t, chain)

	head, pin, err := VerifyClientKeyHistory(testClientId(), encoded, testPolicyFor(t, signer))
	if err != nil {
		t.Fatalf("verify: %s", err)
	}
	if head.Generation != 6 || pin.Generation != 6 {
		t.Fatalf("head generation = %d, pin = %d, want 6", head.Generation, pin.Generation)
	}

	// Corrupt each NON-HEAD generation in turn; every one must be caught,
	// not just the root. The head is deliberately excluded and gets its own
	// test below: re-signing the head produces a chain that is genuinely
	// valid, and catching that is not this layer's job.
	for index := range len(chain) - 1 {
		corrupted := encodeTestChain(t, chain)
		mutated := chain[index]
		mutated.PublicKey[0] ^= 1
		mutated = signer.signRegistration(t, mutated)
		mutatedBytes, err := json.Marshal(mutated)
		if err != nil {
			t.Fatalf("marshal: %s", err)
		}
		corrupted[index] = mutatedBytes
		if _, _, err := VerifyClientKeyHistory(testClientId(), corrupted, testPolicyFor(t, signer)); !errors.Is(err, ErrClientKeyRegistrationInvalid) {
			t.Fatalf("corruption at generation %d accepted, err = %v", index+1, err)
		}
	}
}

// Every generation's signer is checked, not only the head's. A chain whose
// middle was signed by someone else must not be accepted because its ends look
// right.
func TestIntermediateUnauthorizedSignerRejected(t *testing.T) {
	signer := newTestClientKeySigner(t, goldenSignerPrivateKeyHex)
	// a second, unauthorized signer
	other := newTestClientKeySigner(t, "1111111111111111111111111111111111111111111111111111111111111111")
	if other.address == signer.address {
		t.Fatal("test signers collide")
	}

	chain := buildTestChain(t, signer, testClientId(), [][32]byte{
		testClientKeyPublicKey(0x50),
		testClientKeyPublicKey(0x60),
		testClientKeyPublicKey(0x70),
	})
	// re-sign generation 2 with the other key, then relink generation 3
	middle := other.signRegistration(t, chain[1])
	middleHash, err := middle.ContentHash()
	if err != nil {
		t.Fatalf("middle content hash: %s", err)
	}
	last := chain[2]
	last.PreviousHash = middleHash
	last = signer.signRegistration(t, last)

	encoded := encodeTestChain(t, []ClientKeyRegistration{chain[0], middle, last})
	if _, _, err := VerifyClientKeyHistory(testClientId(), encoded, testPolicyFor(t, signer)); !errors.Is(err, ErrClientKeyRegistrationInvalid) {
		t.Fatalf("chain with an unauthorized intermediate signer accepted, err = %v", err)
	}
}

// A head that withdraws the key is a verified statement that the peer has no
// usable identity — the session must not proceed on the contract's word.
func TestSignedTombstoneHeadRejected(t *testing.T) {
	signer := newTestClientKeySigner(t, goldenSignerPrivateKeyHex)
	first := buildTestChain(t, signer, testClientId(), [][32]byte{testClientKeyPublicKey(0x50)})[0]
	firstHash, err := first.ContentHash()
	if err != nil {
		t.Fatalf("content hash: %s", err)
	}
	tombstone := signer.signRegistration(t, ClientKeyRegistration{
		Domain:            testClientKeyDomain(),
		ClientID:          [16]byte(testClientId()),
		NetworkID:         testClientKeyNetworkId(),
		Generation:        2,
		Present:           false,
		PublicKey:         [32]byte{},
		PreviousHash:      firstHash,
		EffectiveBoundary: testClientKeyBoundary(1001),
	})
	if err := tombstone.follows(&first); err != nil {
		t.Fatalf("a genuine tombstone must be a valid successor: %s", err)
	}
	encoded := encodeTestChain(t, []ClientKeyRegistration{first, tombstone})
	if _, _, err := VerifyClientKeyHistory(testClientId(), encoded, testPolicyFor(t, signer)); !errors.Is(err, ErrClientKeyRegistrationInvalid) {
		t.Fatalf("tombstoned head accepted as a usable identity, err = %v", err)
	}
}

// Trust-on-first-use only applies with nothing to check against. Once either a
// pinned peer or a build-pinned signer set exists, an unknown signer must be
// refused — otherwise the "first use" branch would be a permanent bypass.
func TestTrustOnFirstUseDoesNotBypassAPinnedSet(t *testing.T) {
	signer := newTestClientKeySigner(t, goldenSignerPrivateKeyHex)
	other := newTestClientKeySigner(t, "1111111111111111111111111111111111111111111111111111111111111111")
	chain := buildTestChain(t, other, testClientId(), [][32]byte{testClientKeyPublicKey(0x50)})
	encoded := encodeTestChain(t, chain)

	// with a pinned set that does not include `other`
	if _, _, err := VerifyClientKeyHistory(testClientId(), encoded, testPolicyFor(t, signer)); !errors.Is(err, ErrClientKeyRegistrationInvalid) {
		t.Fatalf("unknown signer accepted despite a pinned set, err = %v", err)
	}

	// with no pinned set and no pin at all, first use establishes
	empty := ClientKeyHistoryPolicy{TrustedSigners: nil, Pin: nil, MaxGenerations: 16}
	if _, _, err := VerifyClientKeyHistory(testClientId(), encoded, empty); err != nil {
		t.Fatalf("trust on first use rejected: %s", err)
	}
}

// A pin authorises its own signer, so a peer pinned under signer A must not be
// silently re-verified under signer B.
func TestPinnedSignerIsNotInterchangeable(t *testing.T) {
	signer := newTestClientKeySigner(t, goldenSignerPrivateKeyHex)
	other := newTestClientKeySigner(t, "1111111111111111111111111111111111111111111111111111111111111111")

	chain := buildTestChain(t, signer, testClientId(), [][32]byte{testClientKeyPublicKey(0x50)})
	_, pin, err := VerifyClientKeyHistory(testClientId(), encodeTestChain(t, chain), testPolicyFor(t, signer))
	if err != nil {
		t.Fatalf("setup verify: %s", err)
	}

	otherChain := buildTestChain(t, other, testClientId(), [][32]byte{testClientKeyPublicKey(0x50)})
	policy := ClientKeyHistoryPolicy{TrustedSigners: nil, Pin: &pin, MaxGenerations: 16}
	if _, _, err := VerifyClientKeyHistory(testClientId(), encodeTestChain(t, otherChain), policy); !errors.Is(err, ErrClientKeyRegistrationInvalid) {
		t.Fatalf("a different signer accepted against an existing pin, err = %v", err)
	}
}

// Re-signing the HEAD of a chain produces a chain that verifies — and it must,
// because that is indistinguishable from a legitimate key rotation. This is
// the exact move a substituting operator makes, so it is worth stating where
// it IS caught:
//
//   - `applyPeerClientKeyHistory` compares the verified head against the
//     contract-supplied key, so a head naming a key the contract did not name
//     is terminal (TestSignedIdentitySubstitutionRejected);
//   - the pin refuses a head that rewinds or forks a generation already seen
//     (TestPinRefusesForkAtTheSameGeneration).
//
// Chain verification alone cannot tell a rotation from a substitution. Nothing
// in the documentation may claim otherwise.
func TestResignedHeadVerifiesAsChainAndIsCaughtElsewhere(t *testing.T) {
	signer := newTestClientKeySigner(t, goldenSignerPrivateKeyHex)
	chain := buildTestChain(t, signer, testClientId(), [][32]byte{
		testClientKeyPublicKey(0x50),
		testClientKeyPublicKey(0x60),
	})
	substituted := chain[1]
	substituted.PublicKey = testClientKeyPublicKey(0x70)
	substituted = signer.signRegistration(t, substituted)
	encoded := encodeTestChain(t, []ClientKeyRegistration{chain[0], substituted})

	head, pin, err := VerifyClientKeyHistory(testClientId(), encoded, testPolicyFor(t, signer))
	if err != nil {
		t.Fatalf("a re-signed head must still verify as a chain: %s", err)
	}
	if head.PublicKey != testClientKeyPublicKey(0x70) {
		t.Fatal("head is not the substituted key")
	}
	if pin.Generation != 2 {
		t.Fatalf("pin generation = %d, want 2", pin.Generation)
	}
}

// The pin is what makes a same-generation substitution visible.
func TestPinRefusesForkAtTheSameGeneration(t *testing.T) {
	signer := newTestClientKeySigner(t, goldenSignerPrivateKeyHex)
	chain := buildTestChain(t, signer, testClientId(), [][32]byte{
		testClientKeyPublicKey(0x50),
		testClientKeyPublicKey(0x60),
	})
	_, pin, err := VerifyClientKeyHistory(testClientId(), encodeTestChain(t, chain), testPolicyFor(t, signer))
	if err != nil {
		t.Fatalf("setup: %s", err)
	}

	// the operator re-signs generation 2 with a different key and serves it
	forked := chain[1]
	forked.PublicKey = testClientKeyPublicKey(0x70)
	forked = signer.signRegistration(t, forked)
	encoded := encodeTestChain(t, []ClientKeyRegistration{chain[0], forked})

	policy := testPolicyFor(t, signer)
	policy.Pin = &pin
	if _, _, err := VerifyClientKeyHistory(testClientId(), encoded, policy); !errors.Is(err, ErrClientKeyRegistrationInvalid) {
		t.Fatalf("same-generation fork accepted against a pin, err = %v", err)
	}
}
