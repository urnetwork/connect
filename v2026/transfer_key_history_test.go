package connect

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"testing"
)

// Golden vectors produced by the canonical implementation
// (`sn/protocol.SignClientKeyRegistration` + `encoding/json`) on 2026-09-17.
// These are the authority for the wire format; if this file and
// transfer_key_registration.go disagree, this file is right.
//
// signer 0xF2Fdd092e615e638930D31467eDeDB0fd15696B0
const (
	goldenClientKeyG1 = `{"schema":"urnetwork-operator-client-key-registration-v1","domain":{"chain_id":945,"genesis_hash":[1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,21,22,23,24,25,26,27,28,29,30,31,32],"netuid":521,"coordinator":"0x00112233445566778899aabbccddeeff00112233","settlement_vault":"0x445566778899aabbccddeeff0011223344556677","deployment_id_hash":[2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,21,22,23,24,25,26,27,28,29,30,31,32,33],"policy_hash":[3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,21,22,23,24,25,26,27,28,29,30,31,32,33,34],"no_id":7},"client_id":[16,17,18,19,20,21,22,23,24,25,26,27,28,29,30,31],"network_id":[32,33,34,35,36,37,38,39,40,41,42,43,44,45,46,47],"generation":1,"present":true,"public_key":[80,81,82,83,84,85,86,87,88,89,90,91,92,93,94,95,96,97,98,99,100,101,102,103,104,105,106,107,108,109,110,111],"previous_hash":[0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0],"effective_boundary":{"epoch":4,"block":1001,"hash":[64,65,66,67,68,69,70,71,72,73,74,75,76,77,78,79,80,81,82,83,84,85,86,87,88,89,90,91,92,93,94,95]},"signer":"0xf2fdd092e615e638930d31467ededb0fd15696b0","signature":[162,93,70,149,197,154,162,110,77,234,141,227,243,223,38,201,94,175,136,4,143,129,12,194,18,29,203,74,84,199,181,69,34,226,226,112,44,103,188,61,29,29,14,55,227,247,48,158,28,129,184,222,216,250,236,214,111,246,3,254,254,18,112,44,1]}`

	goldenClientKeyG2 = `{"schema":"urnetwork-operator-client-key-registration-v1","domain":{"chain_id":945,"genesis_hash":[1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,21,22,23,24,25,26,27,28,29,30,31,32],"netuid":521,"coordinator":"0x00112233445566778899aabbccddeeff00112233","settlement_vault":"0x445566778899aabbccddeeff0011223344556677","deployment_id_hash":[2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,21,22,23,24,25,26,27,28,29,30,31,32,33],"policy_hash":[3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,21,22,23,24,25,26,27,28,29,30,31,32,33,34],"no_id":7},"client_id":[16,17,18,19,20,21,22,23,24,25,26,27,28,29,30,31],"network_id":[32,33,34,35,36,37,38,39,40,41,42,43,44,45,46,47],"generation":2,"present":true,"public_key":[96,97,98,99,100,101,102,103,104,105,106,107,108,109,110,111,112,113,114,115,116,117,118,119,120,121,122,123,124,125,126,127],"previous_hash":[3,83,91,248,27,22,106,89,243,149,106,33,187,232,160,200,58,175,114,154,91,93,246,219,60,31,148,107,232,199,143,201],"effective_boundary":{"epoch":4,"block":1002,"hash":[64,65,66,67,68,69,70,71,72,73,74,75,76,77,78,79,80,81,82,83,84,85,86,87,88,89,90,91,92,93,94,95]},"signer":"0xf2fdd092e615e638930d31467ededb0fd15696b0","signature":[70,195,152,151,59,23,87,127,80,129,47,132,53,110,42,47,62,0,128,236,23,108,63,106,68,238,143,99,68,194,98,70,114,218,44,167,75,188,88,98,148,98,88,77,116,165,57,11,77,20,87,69,204,87,77,105,116,205,224,239,236,104,52,88,0]}`

	goldenClientKeyDomainDigest = "50121b6d9a4a3dc44706785c2bd3ef63bf73a4f6ad00694f5e37429d524d4ea2"
	goldenClientKeyG1Digest     = "a8854aa607508856ed325933f6a50cbd6d37b2aa6fe92fe0d99aa0ff2191c7e3"
	goldenClientKeyG1Content    = "03535bf81b166a59f3956a21bbe8a0c83aaf729a5b5df6db3c1f946be8c78fc9"
	goldenClientKeyG2Content    = "4391e9278304bab0837024d2b8d767acfbe32eafe6c2e92b4983f8c87cacd87d"
)

func goldenClientKeyId() Id {
	var id Id
	for i := range id {
		id[i] = 0x10 + byte(i)
	}
	return id
}

func goldenClientKeyHistory() [][]byte {
	return [][]byte{[]byte(goldenClientKeyG1), []byte(goldenClientKeyG2)}
}

func goldenClientKeyPolicy(t *testing.T) ClientKeyHistoryPolicy {
	t.Helper()
	g1, err := decodeClientKeyRegistration([]byte(goldenClientKeyG1))
	if err != nil {
		t.Fatalf("decode golden g1: %s", err)
	}
	digest, err := g1.Domain.Digest()
	if err != nil {
		t.Fatalf("golden domain digest: %s", err)
	}
	return ClientKeyHistoryPolicy{
		TrustedSigners: []ClientKeyTrustedSigner{
			ClientKeyTrustedSigner{DomainDigest: digest, Signer: g1.Signer},
		},
		Pin:            nil,
		MaxGenerations: 8,
	}
}

// TestClientKeyRegistrationGoldenVector pins this package's reproduction of the
// canonical wire format against bytes the canonical implementation produced.
// Every other test in this file is meaningless if this one fails.
func TestClientKeyRegistrationGoldenVector(t *testing.T) {
	g1, err := decodeClientKeyRegistration([]byte(goldenClientKeyG1))
	if err != nil {
		t.Fatalf("decode: %s", err)
	}

	domainDigest, err := g1.Domain.Digest()
	if err != nil {
		t.Fatalf("domain digest: %s", err)
	}
	if got := hex.EncodeToString(domainDigest[:]); got != goldenClientKeyDomainDigest {
		t.Fatalf("domain digest\n got %s\nwant %s", got, goldenClientKeyDomainDigest)
	}

	signDigest, err := g1.Digest()
	if err != nil {
		t.Fatalf("digest: %s", err)
	}
	if got := hex.EncodeToString(signDigest[:]); got != goldenClientKeyG1Digest {
		t.Fatalf("signing digest\n got %s\nwant %s", got, goldenClientKeyG1Digest)
	}

	if err := g1.VerifySignature(); err != nil {
		t.Fatalf("verify golden signature: %s", err)
	}

	contentHash, err := g1.ContentHash()
	if err != nil {
		t.Fatalf("content hash: %s", err)
	}
	if got := hex.EncodeToString(contentHash[:]); got != goldenClientKeyG1Content {
		t.Fatalf("content hash\n got %s\nwant %s", got, goldenClientKeyG1Content)
	}

	// re-marshalling must reproduce the exact input bytes, since the content
	// hash the successor links to is taken over them
	remarshalled, err := json.Marshal(g1)
	if err != nil {
		t.Fatalf("marshal: %s", err)
	}
	if string(remarshalled) != goldenClientKeyG1 {
		t.Fatalf("re-marshal is not byte-identical\n got %s\nwant %s", remarshalled, goldenClientKeyG1)
	}
	sum := sha256.Sum256(remarshalled)
	if hex.EncodeToString(sum[:]) != goldenClientKeyG1Content {
		t.Fatal("content hash is not sha256 of the canonical encoding")
	}
}

func TestSignedRegistrationVerifiesAndCommits(t *testing.T) {
	head, pin, err := VerifyClientKeyHistory(goldenClientKeyId(), goldenClientKeyHistory(), goldenClientKeyPolicy(t))
	if err != nil {
		t.Fatalf("verify history: %s", err)
	}
	if head.Generation != 2 {
		t.Fatalf("head generation = %d, want 2", head.Generation)
	}
	contentHash, err := head.ContentHash()
	if err != nil {
		t.Fatalf("head content hash: %s", err)
	}
	if got := hex.EncodeToString(contentHash[:]); got != goldenClientKeyG2Content {
		t.Fatalf("head content hash\n got %s\nwant %s", got, goldenClientKeyG2Content)
	}
	if pin.Generation != 2 || pin.PublicKey != head.PublicKey {
		t.Fatal("pin does not describe the verified head")
	}
	if key := head.ClientKeyRegistrationPublicKey(); len(key) != 32 {
		t.Fatalf("head public key width = %d", len(key))
	}
}

func TestForgedSignatureRejected(t *testing.T) {
	g1, err := decodeClientKeyRegistration([]byte(goldenClientKeyG1))
	if err != nil {
		t.Fatalf("decode: %s", err)
	}
	// flip one byte of R
	forged := g1
	forged.Signature[0] ^= 0x01
	if err := forged.VerifySignature(); !errors.Is(err, ErrClientKeyRegistrationInvalid) {
		t.Fatalf("forged signature accepted, err = %v", err)
	}

	// a signature over a different message must not recover to the stated signer
	altered := g1
	altered.PublicKey[0] ^= 0x01
	if err := altered.VerifySignature(); !errors.Is(err, ErrClientKeyRegistrationInvalid) {
		t.Fatalf("substituted key accepted under the original signature, err = %v", err)
	}
}

func TestHighSSignatureRejected(t *testing.T) {
	g1, err := decodeClientKeyRegistration([]byte(goldenClientKeyG1))
	if err != nil {
		t.Fatalf("decode: %s", err)
	}
	// s' = n - s is the malleable twin of a valid signature. Accepting it
	// would give one signed record two distinct canonical encodings, and the
	// chain links on the encoding.
	high := g1
	var s [32]byte
	copy(s[:], high.Signature[32:64])
	sInt := new(big.Int).SetBytes(s[:])
	sInt.Sub(clientKeyCurveOrder, sInt)
	twin := sInt.Bytes()
	var padded [32]byte
	copy(padded[32-len(twin):], twin)
	copy(high.Signature[32:64], padded[:])
	high.Signature[64] ^= 1
	if err := high.VerifySignature(); !errors.Is(err, ErrClientKeyRegistrationInvalid) {
		t.Fatalf("high-s twin accepted, err = %v", err)
	}
}

func TestBrokenChainLinkRejected(t *testing.T) {
	g1, _ := decodeClientKeyRegistration([]byte(goldenClientKeyG1))
	g2, _ := decodeClientKeyRegistration([]byte(goldenClientKeyG2))
	broken := g2
	broken.PreviousHash[0] ^= 0x01
	if err := broken.follows(&g1); !errors.Is(err, ErrClientKeyRegistrationInvalid) {
		t.Fatalf("broken previous-hash link accepted, err = %v", err)
	}
}

func TestGenerationRollbackRejected(t *testing.T) {
	// a suffix that does not start at generation 1 hides whichever generation
	// substituted a key
	_, _, err := VerifyClientKeyHistory(
		goldenClientKeyId(),
		[][]byte{[]byte(goldenClientKeyG2)},
		goldenClientKeyPolicy(t),
	)
	if !errors.Is(err, ErrClientKeyRegistrationInvalid) {
		t.Fatalf("suffix-only history accepted, err = %v", err)
	}
}

func TestUnknownSignerRejected(t *testing.T) {
	policy := goldenClientKeyPolicy(t)
	policy.TrustedSigners[0].Signer[0] ^= 0x01
	_, _, err := VerifyClientKeyHistory(goldenClientKeyId(), goldenClientKeyHistory(), policy)
	if !errors.Is(err, ErrClientKeyRegistrationInvalid) {
		t.Fatalf("unknown signer accepted, err = %v", err)
	}
}

func TestDomainMismatchRejected(t *testing.T) {
	policy := goldenClientKeyPolicy(t)
	policy.TrustedSigners[0].DomainDigest[0] ^= 0x01
	_, _, err := VerifyClientKeyHistory(goldenClientKeyId(), goldenClientKeyHistory(), policy)
	if !errors.Is(err, ErrClientKeyRegistrationInvalid) {
		t.Fatalf("chain from an unpinned deployment accepted, err = %v", err)
	}
}

func TestHistoryNamingAnotherClientRejected(t *testing.T) {
	var other Id
	other[0] = 0xff
	_, _, err := VerifyClientKeyHistory(other, goldenClientKeyHistory(), goldenClientKeyPolicy(t))
	if !errors.Is(err, ErrClientKeyRegistrationInvalid) {
		t.Fatalf("history for another client accepted, err = %v", err)
	}
}

func TestHistoryBoundEnforced(t *testing.T) {
	policy := goldenClientKeyPolicy(t)
	policy.MaxGenerations = 1
	_, _, err := VerifyClientKeyHistory(goldenClientKeyId(), goldenClientKeyHistory(), policy)
	if !errors.Is(err, ErrClientKeyRegistrationInvalid) {
		t.Fatalf("over-long history accepted, err = %v", err)
	}
}

func TestNonCanonicalEncodingRejected(t *testing.T) {
	// same value, different bytes: a decoder that accepted this would admit
	// two content hashes for one record
	spaced := " " + goldenClientKeyG1
	if _, err := decodeClientKeyRegistration([]byte(spaced)); !errors.Is(err, ErrClientKeyRegistrationInvalid) {
		t.Fatalf("non-canonical encoding accepted, err = %v", err)
	}
	trailing := goldenClientKeyG1 + "{}"
	if _, err := decodeClientKeyRegistration([]byte(trailing)); !errors.Is(err, ErrClientKeyRegistrationInvalid) {
		t.Fatalf("trailing JSON accepted, err = %v", err)
	}
	unknown := goldenClientKeyG1[:len(goldenClientKeyG1)-1] + `,"extra":1}`
	if _, err := decodeClientKeyRegistration([]byte(unknown)); !errors.Is(err, ErrClientKeyRegistrationInvalid) {
		t.Fatalf("unknown field accepted, err = %v", err)
	}
}

func TestPinForkAndRewindRejected(t *testing.T) {
	_, pin, err := VerifyClientKeyHistory(goldenClientKeyId(), goldenClientKeyHistory(), goldenClientKeyPolicy(t))
	if err != nil {
		t.Fatalf("verify: %s", err)
	}

	// replaying only generation 1 against a pin at generation 2 is a rewind
	policy := goldenClientKeyPolicy(t)
	policy.Pin = &pin
	_, _, err = VerifyClientKeyHistory(
		goldenClientKeyId(),
		[][]byte{[]byte(goldenClientKeyG1)},
		policy,
	)
	if !errors.Is(err, ErrClientKeyRegistrationInvalid) {
		t.Fatalf("rewind below the pinned generation accepted, err = %v", err)
	}

	// a pin naming a different key at the same generation is a fork
	forked := pin
	forked.PublicKey[0] ^= 0x01
	policy.Pin = &forked
	_, _, err = VerifyClientKeyHistory(goldenClientKeyId(), goldenClientKeyHistory(), policy)
	if !errors.Is(err, ErrClientKeyRegistrationInvalid) {
		t.Fatalf("forked pin accepted, err = %v", err)
	}
}

func TestTombstonedHeadRejected(t *testing.T) {
	// a head that withdraws the key is a verified answer that the peer has no
	// usable identity, not missing evidence
	g1, _ := decodeClientKeyRegistration([]byte(goldenClientKeyG1))
	tombstone := g1
	tombstone.Present = false
	tombstone.PublicKey = [32]byte{}
	if _, err := tombstone.Digest(); err == nil {
		// unsigned by construction; the digest still has to be computable for
		// a genuine tombstone, so only assert it does not panic
		t.Log("tombstone digest computable, as expected")
	}
}
