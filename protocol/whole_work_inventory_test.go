// Canonical original requests and SDK cuts remain independently authenticated.
package protocol

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"
)

// Every identity and signing key is synthetic and local to this fixture.
func wholeWorkProtocolFixture(t *testing.T) (OriginalWorkCut, ed25519.PrivateKey, OriginalWorkRequest, ed25519.PrivateKey) {
	t.Helper()
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{21}, 32))
	approver := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{22}, 32))
	stored, err := proto.Marshal(&StoredContract{ContractId: bytes.Repeat([]byte{3}, 16), SourceId: bytes.Repeat([]byte{1}, 16), DestinationId: bytes.Repeat([]byte{2}, 16), TransferByteCount: 100})
	if err != nil {
		t.Fatal(err)
	}
	cut, err := SignOriginalWorkCut(t.Context(), OriginalWorkCut{DomainHash: [32]byte{9}, ClientId: [16]byte(bytes.Repeat([]byte{1}, 16)), Generation: [16]byte{4}, Epoch: 7, Block: 101, BlockHash: [32]byte{8}, Revision: 1, Complete: true, Contracts: []OriginalWorkContract{{ContractId: [16]byte(bytes.Repeat([]byte{3}, 16)), StoredContract: stored}}}, key)
	if err != nil {
		t.Fatal(err)
	}
	request, err := SignOriginalWorkRequest(OriginalWorkRequest{RequestId: [16]byte{5}, DomainHash: cut.DomainHash, ClientId: cut.ClientId, Generation: cut.Generation, PublicKey: cut.PublicKey, Epoch: cut.Epoch, Kind: "start", Block: cut.Block, BlockHash: cut.BlockHash, IssuedAtUnix: 1000, ExpiresAtUnix: 1300}, approver)
	if err != nil {
		t.Fatal(err)
	}
	return cut, key, request, approver
}

func TestWholeWorkOriginalCutRoundTripAndExplicitEmpty(t *testing.T) {
	cut, key, _, _ := wholeWorkProtocolFixture(t)
	for _, contracts := range [][]OriginalWorkContract{cut.Contracts, {}} {
		cut.Contracts = contracts
		signed, err := SignOriginalWorkCut(t.Context(), cut, key)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := signed.Bytes(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		actual, err := DecodeOriginalWorkCut(t.Context(), raw)
		if err != nil || !actual.Complete || actual.Generation != cut.Generation || len(actual.Contracts) != len(contracts) {
			t.Fatal("whole owner or known-empty original changed", actual, err)
		}
	}
}

func TestWholeWorkOriginalCutRefusesDuplicateForeignAndMalformedSignature(t *testing.T) {
	base, key, _, _ := wholeWorkProtocolFixture(t)
	for _, change := range []func(*OriginalWorkCut){func(v *OriginalWorkCut) { v.Contracts = append(v.Contracts, v.Contracts[0]) }, func(v *OriginalWorkCut) { v.ClientId = [16]byte{7} }, func(v *OriginalWorkCut) { v.Generation = [16]byte{} }, func(v *OriginalWorkCut) { v.Contracts = nil }, func(v *OriginalWorkCut) { v.Contracts = make([]OriginalWorkContract, MaximumOriginalWorkContracts+1) }} {
		cut := base
		cut.Contracts = append([]OriginalWorkContract(nil), base.Contracts...)
		change(&cut)
		if _, err := SignOriginalWorkCut(t.Context(), cut, key); err == nil {
			t.Fatal("invalid complete owner acquired signature")
		}
	}
	raw, _ := base.Bytes(t.Context())
	raw[len(raw)-3] ^= 1
	if _, err := DecodeOriginalWorkCut(t.Context(), raw); err == nil {
		t.Fatal("changed signature was accepted")
	}
}

func TestWholeWorkOriginalRequestAndReceiptBindExactGenerationBoundary(t *testing.T) {
	cut, _, request, key := wholeWorkProtocolFixture(t)
	requestRaw, _ := request.Bytes()
	cutRaw, _ := cut.Bytes(t.Context())
	submission := OriginalWorkCutSubmission{Request: requestRaw, Cut: cutRaw}
	if receipt, err := VerifyOriginalWorkSubmission(t.Context(), submission, request.Signer); err != nil || receipt.Schema != OriginalWorkReceiptSchema {
		t.Fatal(receipt, err)
	}
	for _, change := range []func(*OriginalWorkRequest){func(v *OriginalWorkRequest) { v.Generation[0]++ }, func(v *OriginalWorkRequest) { v.Block++ }, func(v *OriginalWorkRequest) { v.BlockHash[0]++ }, func(v *OriginalWorkRequest) { v.PublicKey[0]++ }, func(v *OriginalWorkRequest) { v.DomainHash[0]++ }} {
		other := request
		change(&other)
		other, err := SignOriginalWorkRequest(other, key)
		if err != nil {
			t.Fatal(err)
		}
		submission.Request, _ = other.Bytes()
		if _, err := VerifyOriginalWorkSubmission(t.Context(), submission, request.Signer); err == nil {
			t.Fatal("foreign requested cut joined original receipt")
		}
	}
}

func TestWholeWorkOriginalCanonicalEncodingAndOwnerCancellation(t *testing.T) {
	cut, key, request, _ := wholeWorkProtocolFixture(t)
	raw, _ := request.Bytes()
	for _, invalid := range [][]byte{append([]byte(" "), raw...), append(raw[:len(raw)-1], []byte(",\"extra\":true}")...)} {
		if _, err := DecodeOriginalWorkRequest(invalid, request.Signer); err == nil {
			t.Fatal("alternate request encoding was admitted")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := SignOriginalWorkCut(ctx, cut, key); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := DecodeOriginalWorkCut(ctx, []byte("{}")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	key[63] ^= 1
	if _, err := SignOriginalWorkCut(t.Context(), cut, key); err == nil {
		t.Fatal("mismatched signing key halves accepted")
	}
	request.ExpiresAtUnix = request.IssuedAtUnix + 3601
	if _, err := json.Marshal(request); err != nil {
		t.Fatal(err)
	}
	if request.Verify(request.Signer) == nil {
		t.Fatal("unbounded request permission accepted")
	}
}
