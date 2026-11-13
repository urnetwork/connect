// Original request tests use real nested signatures and exact protobuf frames.
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

// Build originals from independent literal request and reservation values.
func originalContractFixture(t *testing.T, companion bool, mode ProvideMode, intermediaries ...[16]byte) (OriginalContractRequest, OriginalContractAdmission, ed25519.PrivateKey) {
	t.Helper()
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{91}, ed25519.SeedSize))
	request := &CreateContract{DestinationId: bytes.Repeat([]byte{92}, 16), TransferByteCount: 100, Companion: companion}
	for _, id := range intermediaries {
		request.IntermediaryIds = append(request.IntermediaryIds, id[:])
	}
	version := uint32(1)
	request.StreamVersion = &version
	body, err := proto.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := proto.Marshal(&Frame{MessageType: MessageType_TransferCreateContract, MessageBytes: body})
	if err != nil {
		t.Fatal(err)
	}
	original, err := SignOriginalContractRequest(t.Context(), OriginalContractRequest{DomainHash: [32]byte{93}, ClientId: [16]byte{94}, Generation: [16]byte{95}, RequestId: [16]byte{96}, RequestFrame: frame}, key)
	if err != nil {
		t.Fatal(err)
	}
	originalRaw, err := original.Bytes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	stored := &StoredContract{ContractId: bytes.Repeat([]byte{97}, 16), SourceId: original.ClientId[:], DestinationId: request.DestinationId, TransferByteCount: 80}
	if len(intermediaries) != 0 {
		stored.StreamId = bytes.Repeat([]byte{98}, 16)
	}
	storedRaw, err := proto.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	resultBody, err := proto.Marshal(&CreateContractResult{CreateContract: request, Contract: &Contract{StoredContractBytes: storedRaw, ProvideMode: mode}})
	if err != nil {
		t.Fatal(err)
	}
	resultFrame, err := proto.Marshal(&Frame{MessageType: MessageType_TransferCreateContractResult, MessageBytes: resultBody})
	if err != nil {
		t.Fatal(err)
	}
	admission, err := SignOriginalContractAdmission(t.Context(), OriginalContractAdmission{Request: originalRaw, ResultFrame: resultFrame}, key)
	if err != nil {
		t.Fatal(err)
	}
	return original, admission, key
}

func TestOriginalContractCreationRetainsRequestedAndActualCapacity(t *testing.T) {
	request, admission, _ := originalContractFixture(t, false, ProvideMode_Public)
	raw, err := admission.Bytes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeOriginalContractAdmission(t.Context(), raw)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := decoded.Facts(t.Context())
	if err != nil || facts.ClientId != request.ClientId || facts.RequestId != request.RequestId || facts.Generation != request.Generation || facts.ReservedBytes != 80 || !facts.UsageOriginIsSource || facts.StreamId != ([16]byte{}) {
		t.Fatal("admission did not retain actual original facts", facts, err)
	}
	var requested CreateContract
	if err := decodeOriginalContractFrame(request.RequestFrame, MessageType_TransferCreateContract, &requested); err != nil || requested.TransferByteCount != 100 {
		t.Fatal("returned capacity replaced the original request", err)
	}
}

func TestOriginalContractCreationPreservesStreamIntermediaryOrder(t *testing.T) {
	first, second := [16]byte{102}, [16]byte{101}
	_, admission, _ := originalContractFixture(t, false, ProvideMode_Network, first, second)
	facts, err := admission.Facts(t.Context())
	if err != nil || len(facts.IntermediaryIds) != 2 || facts.IntermediaryIds[0] != first || facts.IntermediaryIds[1] != second || facts.StreamId != [16]byte(bytes.Repeat([]byte{98}, 16)) {
		t.Fatal("stream request originals were normalized away", facts, err)
	}
}

func TestOriginalContractCreationRetainsNormalizedCompanionDirection(t *testing.T) {
	_, admission, _ := originalContractFixture(t, true, ProvideMode_Network)
	facts, err := admission.Facts(t.Context())
	if err != nil || facts.UsageOriginIsSource {
		t.Fatal("network billing normalization reversed the original service direction", facts, err)
	}
}

func TestOriginalContractCreationRetainsStreamFallbackDirection(t *testing.T) {
	_, admission, _ := originalContractFixture(t, false, ProvideMode_Stream)
	facts, err := admission.Facts(t.Context())
	if err != nil || facts.UsageOriginIsSource {
		t.Fatal("implicit companion fallback used only the request flag", facts, err)
	}
}

// Mutations are made before a fresh SDK receipt signature is attempted; the
// nested pre-send request must still prevent a response from changing its party.
func TestOriginalContractCreationRejectsResignedChangedEndpoint(t *testing.T) {
	_, admission, key := originalContractFixture(t, false, ProvideMode_Public)
	var frame Frame
	var result CreateContractResult
	if err := proto.Unmarshal(admission.ResultFrame, &frame); err != nil {
		t.Fatal(err)
	}
	if err := proto.Unmarshal(frame.MessageBytes, &result); err != nil {
		t.Fatal(err)
	}
	var stored StoredContract
	if err := proto.Unmarshal(result.Contract.StoredContractBytes, &stored); err != nil {
		t.Fatal(err)
	}
	stored.DestinationId = bytes.Repeat([]byte{111}, 16)
	var err error
	result.Contract.StoredContractBytes, err = proto.Marshal(&stored)
	if err != nil {
		t.Fatal(err)
	}
	frame.MessageBytes, err = proto.Marshal(&result)
	if err != nil {
		t.Fatal(err)
	}
	admission.ResultFrame, err = proto.Marshal(&frame)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SignOriginalContractAdmission(t.Context(), admission, key); err == nil {
		t.Fatal("fresh receipt signature authorized a different original destination")
	}
}

func TestOriginalContractCreationRejectsChangedEchoAndForeignKey(t *testing.T) {
	_, admission, key := originalContractFixture(t, false, ProvideMode_Public)
	var frame Frame
	var result CreateContractResult
	if err := proto.Unmarshal(admission.ResultFrame, &frame); err != nil {
		t.Fatal(err)
	}
	if err := proto.Unmarshal(frame.MessageBytes, &result); err != nil {
		t.Fatal(err)
	}
	result.CreateContract.Companion = true
	var err error
	frame.MessageBytes, err = proto.Marshal(&result)
	if err != nil {
		t.Fatal(err)
	}
	admission.ResultFrame, err = proto.Marshal(&frame)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SignOriginalContractAdmission(t.Context(), admission, key); err == nil {
		t.Fatal("changed response echo replaced the pre-send original")
	}
	_, admission, _ = originalContractFixture(t, false, ProvideMode_Public)
	foreign := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{112}, ed25519.SeedSize))
	if _, err := SignOriginalContractAdmission(t.Context(), admission, foreign); err == nil {
		t.Fatal("different signer claimed custody of the original request")
	}
}

func TestOriginalContractCreationRejectsTamperedNestedOriginalAndDuplicateJson(t *testing.T) {
	_, admission, _ := originalContractFixture(t, false, ProvideMode_Public)
	raw, err := admission.Bytes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	duplicate := append([]byte(`{"schema":"urnetwork-original-contract-admission-v1",`), raw[1:]...)
	if _, err := DecodeOriginalContractAdmission(t.Context(), duplicate); err == nil {
		t.Fatal("duplicate JSON field accepted as an immutable original")
	}
	var request OriginalContractRequest
	if err := json.Unmarshal(admission.Request, &request); err != nil {
		t.Fatal(err)
	}
	request.DomainHash[0]++
	admission.Request, err = json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admission.Facts(t.Context()); err == nil {
		t.Fatal("tampered inner domain survived nested signature replay")
	}
}

func TestOriginalContractCreationHonorsCanceledOwner(t *testing.T) {
	_, admission, _ := originalContractFixture(t, false, ProvideMode_Public)
	raw, err := admission.Bytes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := DecodeOriginalContractAdmission(ctx, raw); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled replay returned original facts", err)
	}
}
