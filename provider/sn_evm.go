package main

// sn_evm.go — the minimal EVM plumbing behind `provider claim`
// (sn/PLAN.md 7.3, decision D-6): legacy keccak-256, abi word encoding,
// the openzeppelin-style merkle leaf/proof math for the subnet payout
// root (sn/WHITEPAPER.md 8.3, 11.2), calldata builders for the ST
// contract, and a hand-rolled json-rpc eth_call client. Deliberately
// stdlib + x/crypto only — the provider verifies but never signs or
// submits, so it must not grow an ethereum module dependency.
// Transaction submission lives in the separate snclaim tool.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/sha3"
)

// canonical abi signatures of the ST contract entry points used by
// `provider claim` (sn/WHITEPAPER.md 11.2). `claimMiner` is the
// permissionless payout claim; `noCommit` is the public mapping getter
// whose first return word is the epoch's committed payoutRoot for a
// pool.
const snClaimMinerSignature = "claimMiner(uint256,uint256,bytes32,uint256,bytes32[])"
const snNoCommitSignature = "noCommit(uint256,uint256)"

// ethRpcTimeout bounds each individual json-rpc request.
const ethRpcTimeout = 15 * time.Second

// keccak256 hashes the concatenation of the arguments with legacy
// keccak-256 (the EVM hash, not NIST sha3-256).
func keccak256(data ...[]byte) [32]byte {
	d := sha3.NewLegacyKeccak256()
	for _, b := range data {
		d.Write(b)
	}
	var out [32]byte
	d.Sum(out[:0])
	return out
}

// evmSelector returns the 4-byte function selector for a canonical abi
// signature: keccak256(signature)[:4].
func evmSelector(signature string) [4]byte {
	hash := keccak256([]byte(signature))
	var selector [4]byte
	copy(selector[:], hash[:4])
	return selector
}

// evmWordFromUint64 abi-encodes a uint64 as a 32-byte big-endian word.
func evmWordFromUint64(value uint64) [32]byte {
	var word [32]byte
	binary.BigEndian.PutUint64(word[24:], value)
	return word
}

// evmWordFromBytes left-pads big-endian bytes into one 32-byte word,
// erroring when the value cannot fit.
func evmWordFromBytes(valueBytes []byte) ([32]byte, error) {
	var word [32]byte
	if 32 < len(valueBytes) {
		return word, fmt.Errorf("value does not fit a 32-byte word: %d bytes", len(valueBytes))
	}
	copy(word[32-len(valueBytes):], valueBytes)
	return word, nil
}

// snPayoutLeaf computes the openzeppelin StandardMerkleTree leaf for a
// payout entry:
//
//	keccak256(keccak256(abi.encode(bytes32 coldkey, uint256 shareBps)))
//
// where abi.encode is the 32-byte coldkey followed by the 32-byte
// big-endian shareBps. The double hash prevents a 64-byte encoded
// payload from being reinterpreted as an interior node (second-preimage
// hardening), matching the contract side (sn/WHITEPAPER.md 11.2).
func snPayoutLeaf(coldkey [32]byte, shareBps uint64) [32]byte {
	shareWord := evmWordFromUint64(shareBps)
	inner := keccak256(coldkey[:], shareWord[:])
	return keccak256(inner[:])
}

// snMerkleParent hashes a sorted sibling pair, matching openzeppelin
// MerkleProof: keccak256(min(a,b) ‖ max(a,b)) by lexicographic byte
// order, so proofs carry no left/right position bits.
func snMerkleParent(a [32]byte, b [32]byte) [32]byte {
	if bytes.Compare(b[:], a[:]) < 0 {
		a, b = b, a
	}
	return keccak256(a[:], b[:])
}

// snMerkleRootFromProof walks an inclusion proof from leaf to root and
// returns the implied root. Verification is comparing the result
// against a trusted root.
func snMerkleRootFromProof(leaf [32]byte, proof [][32]byte) [32]byte {
	node := leaf
	for _, sibling := range proof {
		node = snMerkleParent(node, sibling)
	}
	return node
}

// snClaimMinerCalldata abi-encodes a ready-to-submit
// claimMiner(uint256 e, uint256 noId, bytes32 coldkey, uint256 shareBps, bytes32[] proof)
// call. Head/tail layout: five head words — epoch, noId, coldkey,
// shareBps, and the byte offset of the dynamic proof tail (5*32 =
// 0xa0) — then the tail: the proof length word followed by one word per
// proof element.
func snClaimMinerCalldata(epoch uint64, noId [32]byte, coldkey [32]byte, shareBps uint64, proof [][32]byte) []byte {
	selector := evmSelector(snClaimMinerSignature)
	calldata := make([]byte, 0, 4+32*(6+len(proof)))
	calldata = append(calldata, selector[:]...)
	appendWord := func(word [32]byte) {
		calldata = append(calldata, word[:]...)
	}
	appendWord(evmWordFromUint64(epoch))
	appendWord(noId)
	appendWord(coldkey)
	appendWord(evmWordFromUint64(shareBps))
	appendWord(evmWordFromUint64(5 * 32))
	appendWord(evmWordFromUint64(uint64(len(proof))))
	for _, sibling := range proof {
		appendWord(sibling)
	}
	return calldata
}

// snNoCommitCalldata abi-encodes a noCommit(uint256 e, uint256 noId)
// getter call. The first 32 bytes of the return data are the epoch's
// committed payoutRoot for the pool (zero when nothing is committed).
func snNoCommitCalldata(epoch uint64, noId [32]byte) []byte {
	selector := evmSelector(snNoCommitSignature)
	calldata := make([]byte, 0, 4+32*2)
	calldata = append(calldata, selector[:]...)
	epochWord := evmWordFromUint64(epoch)
	calldata = append(calldata, epochWord[:]...)
	calldata = append(calldata, noId[:]...)
	return calldata
}

// ethRpcHexResult performs one json-rpc 2.0 request against an EVM
// endpoint over http and returns the string-typed result. Both methods
// used here (eth_chainId, eth_call) return 0x-hex strings.
func ethRpcHexResult(ctx context.Context, rpcUrl string, method string, params []any) (string, error) {
	requestBodyBytes, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return "", err
	}
	requestCtx, requestCancel := context.WithTimeout(ctx, ethRpcTimeout)
	defer requestCancel()
	request, err := http.NewRequestWithContext(requestCtx, "POST", rpcUrl, bytes.NewReader(requestBodyBytes))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return "", fmt.Errorf("%s: http %d", method, response.StatusCode)
	}
	responseBodyBytes, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var rpcResponse struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(responseBodyBytes, &rpcResponse); err != nil {
		return "", fmt.Errorf("%s: bad json-rpc response: %w", method, err)
	}
	if rpcResponse.Error != nil {
		return "", fmt.Errorf("%s: rpc error %d: %s", method, rpcResponse.Error.Code, rpcResponse.Error.Message)
	}
	var hexResult string
	if err := json.Unmarshal(rpcResponse.Result, &hexResult); err != nil {
		return "", fmt.Errorf("%s: non-string result", method)
	}
	return hexResult, nil
}

// parseEthHexQuantity parses a 0x-prefixed json-rpc quantity such as
// the eth_chainId result.
func parseEthHexQuantity(hexQuantity string) (uint64, error) {
	s := strings.TrimPrefix(hexQuantity, "0x")
	if s == "" {
		return 0, fmt.Errorf("empty hex quantity")
	}
	return strconv.ParseUint(s, 16, 64)
}

// parseEthHexBytes parses 0x-prefixed hex data such as the eth_call
// return data.
func parseEthHexBytes(hexData string) ([]byte, error) {
	return hex.DecodeString(strings.TrimPrefix(hexData, "0x"))
}
