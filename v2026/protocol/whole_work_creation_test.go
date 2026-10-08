// The whole-owner cut transports exact source creation originals without
// allowing another lifecycle or reservation to borrow their attribution.
package protocol

import (
	"bytes"
	"crypto/ed25519"
	"testing"

	"google.golang.org/protobuf/proto"
)

// Construct the actual signed request and response frames independently of the
// cut verifier, using only synthetic keys and identities from its fixture.
func wholeWorkCreationForCut(t *testing.T, cut OriginalWorkCut, key ed25519.PrivateKey) []byte {
	t.Helper()
	_, destination, err := cut.Contracts[0].Parties()
	if err != nil {
		t.Fatal(err)
	}
	request := &CreateContract{DestinationId: destination[:], TransferByteCount: 100}
	requestMessage, err := proto.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	requestFrame, err := proto.Marshal(&Frame{MessageType: MessageType_TransferCreateContract, MessageBytes: requestMessage})
	if err != nil {
		t.Fatal(err)
	}
	original, err := SignOriginalContractRequest(t.Context(), OriginalContractRequest{DomainHash: cut.DomainHash, ClientId: cut.ClientId, Generation: cut.Generation, RequestId: [16]byte{19}, RequestFrame: requestFrame}, key)
	if err != nil {
		t.Fatal(err)
	}
	requestRaw, err := original.Bytes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	resultMessage, err := proto.Marshal(&CreateContractResult{CreateContract: request, Contract: &Contract{StoredContractBytes: bytes.Clone(cut.Contracts[0].StoredContract), ProvideMode: ProvideMode_Network}})
	if err != nil {
		t.Fatal(err)
	}
	resultFrame, err := proto.Marshal(&Frame{MessageType: MessageType_TransferCreateContractResult, MessageBytes: resultMessage})
	if err != nil {
		t.Fatal(err)
	}
	admission, err := SignOriginalContractAdmission(t.Context(), OriginalContractAdmission{Request: requestRaw, ResultFrame: resultFrame}, key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := admission.Bytes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestWholeWorkCutRetainsExactOriginalCreationAndReservation(t *testing.T) {
	cut, key, _, _ := wholeWorkProtocolFixture(t)
	creation := wholeWorkCreationForCut(t, cut, key)
	cut.Contracts[0].OriginalCreation = creation
	cut, err := SignOriginalWorkCut(t.Context(), cut, key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := cut.Bytes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeOriginalWorkCut(t.Context(), raw)
	if err != nil || len(decoded.Contracts) != 1 || !bytes.Equal(decoded.Contracts[0].OriginalCreation, creation) {
		t.Fatal("cut lost the exact pre-send request and original reservation response", decoded, err)
	}
	var stored StoredContract
	if err := proto.Unmarshal(cut.Contracts[0].StoredContract, &stored); err != nil {
		t.Fatal(err)
	}
	stored.TransferByteCount++
	cut.Contracts[0].StoredContract, err = proto.Marshal(&stored)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SignOriginalWorkCut(t.Context(), cut, key); err == nil {
		t.Fatal("changed reservation borrowed a valid original creation")
	}
}

func TestWholeWorkCutRefusesForeignOriginalCreationDomainGenerationAndKey(t *testing.T) {
	for _, change := range []func(*OriginalWorkCut, *ed25519.PrivateKey){func(c *OriginalWorkCut, _ *ed25519.PrivateKey) {
		c.DomainHash[0]++
	}, func(c *OriginalWorkCut, _ *ed25519.PrivateKey) {
		c.Generation[0]++
	}, func(_ *OriginalWorkCut, key *ed25519.PrivateKey) {
		*key = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{25}, ed25519.SeedSize))
	}} {
		cut, key, _, _ := wholeWorkProtocolFixture(t)
		foreign, foreignKey := cut, key
		change(&foreign, &foreignKey)
		cut.Contracts[0].OriginalCreation = wholeWorkCreationForCut(t, foreign, foreignKey)
		if _, err := SignOriginalWorkCut(t.Context(), cut, key); err == nil {
			t.Fatal("valid foreign creation signature replaced the independently selected SDK owner")
		}
	}
}
