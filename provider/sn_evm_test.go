package main

// sn_evm_test.go — vectors for the claim-side EVM plumbing. The merkle
// vector is the `payout_5` case of the canonical shared fixtures in
// sn/merkle/testdata/merkle_vectors.json (generated with the
// openzeppelin merkle-tree reference implementation), embedded here as
// hex literals so the test needs no fixture file at runtime. The
// calldata goldens are derived word by word in comments; the selectors
// are keccak256(signature)[:4] and were cross-checked against a keccak
// implementation validated by the known-answer tests below.

import (
	"encoding/hex"
	"fmt"
	"testing"
)

// payout_5 leaf 0 and its proof/root from the shared vectors.
const testPayoutColdkeyHex = "8004d875c16bf38915489d9e97d90dc3f5604c3211846df638c08c15094cc77f"
const testPayoutShareBps = uint64(3571)
const testPayoutLeafHex = "3e68b1b3189ff62edc74153ac1362c885f342fa6f21d7b0591d4782bde3a3425"
const testPayoutRootHex = "d22ceb5e0c422246105a60d4fe2122a7eadb482abc92370038ded3c2878b6248"

var testPayoutProofHex = []string{
	"3e58985b11697ab78b4da3c874a370ea8c109014e701b5a23ac6f339145348b7",
	"2b39287a6f960e9f79a72f2f86e2c5a3e682d8ad4b71643ef99263a84ea6c367",
	"ab97618f9e44c8ca1dc12afd70cbdd533c3a4a02106d64e216d46ae6e5e22be5",
}

// hex32 decodes a 32-byte hex literal.
func hex32(t *testing.T, s string) [32]byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		t.Fatalf("bad hex32 literal %q", s)
	}
	var out [32]byte
	copy(out[:], b)
	return out
}

func TestKeccak256(t *testing.T) {
	// known-answer vectors for legacy keccak-256 (pre-NIST padding).
	// These differ from sha3-256, so they also prove the right variant
	// is in use.
	if got := fmt.Sprintf("%x", keccak256([]byte{})); got != "c5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470" {
		t.Fatalf("keccak256(\"\") = %s", got)
	}
	if got := fmt.Sprintf("%x", keccak256([]byte("abc"))); got != "4e03657aea45a94fc7d47ba826c8d667c0d1e6e33a64a036ec44f58fa12d6c45" {
		t.Fatalf("keccak256(\"abc\") = %s", got)
	}
	// multi-argument hashing concatenates
	if keccak256([]byte("a"), []byte("bc")) != keccak256([]byte("abc")) {
		t.Fatalf("keccak256 concatenation mismatch")
	}
	// the canonical erc20 selector known-answer
	if got := fmt.Sprintf("%x", evmSelector("transfer(address,uint256)")); got != "a9059cbb" {
		t.Fatalf("transfer selector = %s", got)
	}
}

func TestEvmWordFromBytes(t *testing.T) {
	// short values left-pad
	word, err := evmWordFromBytes([]byte{0x01, 0x02})
	if err != nil {
		t.Fatalf("err %s", err)
	}
	if word != evmWordFromUint64(0x0102) {
		t.Fatalf("word 0x%x", word)
	}
	// exact 32 bytes pass through
	full := hex32(t, testPayoutColdkeyHex)
	word, err = evmWordFromBytes(full[:])
	if err != nil || word != full {
		t.Fatalf("word 0x%x err %v", word, err)
	}
	// longer than a word errors
	if _, err := evmWordFromBytes(make([]byte, 33)); err == nil {
		t.Fatalf("expected an error for 33 bytes")
	}
}

func TestSnMerkleParent(t *testing.T) {
	a := evmWordFromUint64(1)
	b := evmWordFromUint64(2)
	// commutative: the pair is sorted before hashing
	if snMerkleParent(a, b) != snMerkleParent(b, a) {
		t.Fatalf("parent is not commutative")
	}
	// and the sorted order is min ‖ max
	if snMerkleParent(a, b) != keccak256(a[:], b[:]) {
		t.Fatalf("parent is not keccak256(min ‖ max)")
	}
}

func TestSnPayoutLeafProofVector(t *testing.T) {
	coldkey := hex32(t, testPayoutColdkeyHex)
	expectedLeaf := hex32(t, testPayoutLeafHex)
	expectedRoot := hex32(t, testPayoutRootHex)
	proof := make([][32]byte, len(testPayoutProofHex))
	for i, proofElementHex := range testPayoutProofHex {
		proof[i] = hex32(t, proofElementHex)
	}

	// the double-hashed leaf matches the openzeppelin reference value
	// (it also appears verbatim in the shared vectors, as the sibling
	// hash in leaf 2's proof)
	leaf := snPayoutLeaf(coldkey, testPayoutShareBps)
	if leaf != expectedLeaf {
		t.Fatalf("leaf 0x%x; expected 0x%x", leaf, expectedLeaf)
	}

	// walking the proof reproduces the committed root
	if root := snMerkleRootFromProof(leaf, proof); root != expectedRoot {
		t.Fatalf("root 0x%x; expected 0x%x", root, expectedRoot)
	}

	// a claim for a different share must not verify
	tamperedLeaf := snPayoutLeaf(coldkey, testPayoutShareBps+1)
	if snMerkleRootFromProof(tamperedLeaf, proof) == expectedRoot {
		t.Fatalf("tampered share verified")
	}

	// a claim for a different coldkey must not verify
	tamperedColdkey := coldkey
	tamperedColdkey[0] ^= 0x01
	tamperedLeaf = snPayoutLeaf(tamperedColdkey, testPayoutShareBps)
	if snMerkleRootFromProof(tamperedLeaf, proof) == expectedRoot {
		t.Fatalf("tampered coldkey verified")
	}

	// a tampered proof element must not verify
	tamperedProof := make([][32]byte, len(proof))
	copy(tamperedProof, proof)
	tamperedProof[1][31] ^= 0x01
	if snMerkleRootFromProof(leaf, tamperedProof) == expectedRoot {
		t.Fatalf("tampered proof verified")
	}

	// a truncated proof must not verify
	if snMerkleRootFromProof(leaf, proof[:2]) == expectedRoot {
		t.Fatalf("truncated proof verified")
	}
}

func TestSnClaimMinerCalldataGolden(t *testing.T) {
	// claimMiner(uint256,uint256,bytes32,uint256,bytes32[]) with
	// epoch=42, noId=3, the payout_5 leaf-0 (coldkey, shareBps=3571),
	// and its 3-element proof. Expected encoding, word by word:
	//   selector  keccak256("claimMiner(uint256,uint256,bytes32,uint256,bytes32[])")[:4]
	//             = 4c207962
	//   head[0]   epoch 42 = 0x2a, left-padded to 32 bytes
	//   head[1]   noId 3
	//   head[2]   coldkey (bytes32, verbatim)
	//   head[3]   shareBps 3571 = 0xdf3
	//   head[4]   offset of the dynamic bytes32[] tail relative to the
	//             start of the arguments = 5 words * 32 = 160 = 0xa0
	//   tail[0]   proof length 3
	//   tail[1..] the 3 proof words, verbatim
	// 4 + 32*9 = 292 bytes total.
	expectedCalldataHex := "4c207962" +
		"000000000000000000000000000000000000000000000000000000000000002a" +
		"0000000000000000000000000000000000000000000000000000000000000003" +
		testPayoutColdkeyHex +
		"0000000000000000000000000000000000000000000000000000000000000df3" +
		"00000000000000000000000000000000000000000000000000000000000000a0" +
		"0000000000000000000000000000000000000000000000000000000000000003" +
		testPayoutProofHex[0] +
		testPayoutProofHex[1] +
		testPayoutProofHex[2]

	proof := make([][32]byte, len(testPayoutProofHex))
	for i, proofElementHex := range testPayoutProofHex {
		proof[i] = hex32(t, proofElementHex)
	}
	calldata := snClaimMinerCalldata(
		42,
		evmWordFromUint64(3),
		hex32(t, testPayoutColdkeyHex),
		testPayoutShareBps,
		proof,
	)
	if got := fmt.Sprintf("%x", calldata); got != expectedCalldataHex {
		t.Fatalf("calldata\n%s\nexpected\n%s", got, expectedCalldataHex)
	}
}

func TestSnClaimMinerCalldataEmptyProof(t *testing.T) {
	// a single-leaf tree has an empty proof; the dynamic array encodes
	// as offset 0xa0 then a zero length word. 4 + 32*6 = 196 bytes.
	calldata := snClaimMinerCalldata(
		1,
		evmWordFromUint64(2),
		evmWordFromUint64(4),
		5,
		nil,
	)
	expectedCalldataHex := "4c207962" +
		"0000000000000000000000000000000000000000000000000000000000000001" +
		"0000000000000000000000000000000000000000000000000000000000000002" +
		"0000000000000000000000000000000000000000000000000000000000000004" +
		"0000000000000000000000000000000000000000000000000000000000000005" +
		"00000000000000000000000000000000000000000000000000000000000000a0" +
		"0000000000000000000000000000000000000000000000000000000000000000"
	if got := fmt.Sprintf("%x", calldata); got != expectedCalldataHex {
		t.Fatalf("calldata\n%s\nexpected\n%s", got, expectedCalldataHex)
	}
}

func TestSnNoCommitCalldataGolden(t *testing.T) {
	// noCommit(uint256,uint256) with epoch=42, noId=3:
	//   selector  keccak256("noCommit(uint256,uint256)")[:4] = c477c60f
	//   word[0]   epoch 42 = 0x2a
	//   word[1]   noId 3
	// 4 + 32*2 = 68 bytes total.
	expectedCalldataHex := "c477c60f" +
		"000000000000000000000000000000000000000000000000000000000000002a" +
		"0000000000000000000000000000000000000000000000000000000000000003"
	calldata := snNoCommitCalldata(42, evmWordFromUint64(3))
	if got := fmt.Sprintf("%x", calldata); got != expectedCalldataHex {
		t.Fatalf("calldata\n%s\nexpected\n%s", got, expectedCalldataHex)
	}
}
