// The request owner signs the exact outgoing request before transport and the
// exact received reservation before publication. These originals establish
// requested parties and direction; they do not certify an extender census.
package protocol

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math"

	"google.golang.org/protobuf/proto"
)

const OriginalContractRequestSchema = "urnetwork-original-contract-request-v1"
const OriginalContractAdmissionSchema = "urnetwork-original-contract-admission-v1"
const MaximumOriginalContractFrameBytes = 32 * 1024
const MaximumOriginalContractRequestBytes = 48 * 1024
const MaximumOriginalContractAdmissionBytes = 128 * 1024

// The raw frame preserves every requested field, including companion direction,
// stream choice and intermediary order. A generation never reuses a request id.
type OriginalContractRequest struct {
	Schema       string                      `json:"schema"`
	DomainHash   [32]byte                    `json:"domain_hash"`
	ClientId     [16]byte                    `json:"client_id"`
	Generation   [16]byte                    `json:"generation"`
	RequestId    [16]byte                    `json:"request_id"`
	PublicKey    [32]byte                    `json:"public_key"`
	RequestFrame []byte                      `json:"request_frame"`
	Signature    [ed25519.SignatureSize]byte `json:"signature"`
}

// An SDK-signed receipt embeds its already retained request. The raw response
// supplies the actual reserved capacity, which can differ from the request.
type OriginalContractAdmission struct {
	Schema      string                      `json:"schema"`
	PublicKey   [32]byte                    `json:"public_key"`
	Request     []byte                      `json:"request"`
	ResultFrame []byte                      `json:"result_frame"`
	Signature   [ed25519.SignatureSize]byte `json:"signature"`
}

// These facts are original request/response inputs, not a completeness claim.
// A reused stream can carry further original intermediaries, and either endpoint
// can have active extender sessions that are absent from the request.
type OriginalContractCreationFacts struct {
	DomainHash          [32]byte
	ClientId            [16]byte
	Generation          [16]byte
	RequestId           [16]byte
	PublicKey           [32]byte
	ContractId          [16]byte
	StoredContract      []byte
	SourceId            [16]byte
	DestinationId       [16]byte
	IntermediaryIds     [][16]byte
	StreamId            [16]byte
	ReservedBytes       uint64
	UsageOriginIsSource bool
	ProvideMode         ProvideMode
}

// Require an owned finite operation before decoding untrusted original bytes.
func originalContractContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("original contract evidence requires an owner")
	}
	return ctx.Err()
}

// Raw protobuf bytes are retained exactly; unsupported fields stay unavailable
// instead of being silently discarded by this version's interpretation.
func decodeOriginalContractFrame(raw []byte, kind MessageType, message proto.Message) error {
	if len(raw) == 0 || len(raw) > MaximumOriginalContractFrameBytes {
		return errors.New("original contract frame exceeds capacity")
	}
	var frame Frame
	if err := proto.Unmarshal(raw, &frame); err != nil || frame.MessageType != kind || frame.Raw || len(frame.ProtoReflect().GetUnknown()) != 0 {
		return errors.New("original contract frame identity differs")
	}
	if err := proto.Unmarshal(frame.MessageBytes, message); err != nil || len(message.ProtoReflect().GetUnknown()) != 0 {
		return errors.New("original contract message is unsupported")
	}
	return nil
}

// Validate the source-owned identity and requested endpoint before signing.
func (self OriginalContractRequest) signingBytes(ctx context.Context) ([]byte, error) {
	if err := originalContractContext(ctx); err != nil {
		return nil, err
	}
	if self.Schema != OriginalContractRequestSchema || self.DomainHash == ([32]byte{}) || self.ClientId == ([16]byte{}) || self.Generation == ([16]byte{}) || self.RequestId == ([16]byte{}) || self.PublicKey == ([32]byte{}) {
		return nil, errors.New("original contract request identity is incomplete")
	}
	var request CreateContract
	if err := decodeOriginalContractFrame(self.RequestFrame, MessageType_TransferCreateContract, &request); err != nil {
		return nil, err
	}
	if len(request.DestinationId) != 16 || bytes.Equal(request.DestinationId, make([]byte, 16)) || bytes.Equal(request.DestinationId, self.ClientId[:]) || request.TransferByteCount > math.MaxInt64 || len(request.IntermediaryIds) > 32 {
		return nil, errors.New("original contract requested parties or capacity are invalid")
	}
	for _, id := range request.IntermediaryIds {
		if len(id) != 16 || bytes.Equal(id, make([]byte, 16)) {
			return nil, errors.New("original contract intermediary is malformed")
		}
	}
	self.Signature = [ed25519.SignatureSize]byte{}
	raw, err := json.Marshal(self)
	if err != nil || len(raw) > MaximumOriginalContractRequestBytes {
		return nil, errors.New("original contract request encoding exceeds capacity")
	}
	return raw, nil
}

// Only the actual owner calls this before handing its frame to SendControl.
func SignOriginalContractRequest(ctx context.Context, value OriginalContractRequest, key ed25519.PrivateKey) (OriginalContractRequest, error) {
	if len(key) != ed25519.PrivateKeySize || !bytes.Equal(ed25519.NewKeyFromSeed(key[:ed25519.SeedSize]), key) {
		return OriginalContractRequest{}, errors.New("original contract request key is invalid")
	}
	value.Schema = OriginalContractRequestSchema
	copy(value.PublicKey[:], key[ed25519.SeedSize:])
	raw, err := value.signingBytes(ctx)
	if err != nil {
		return OriginalContractRequest{}, err
	}
	copy(value.Signature[:], ed25519.Sign(key, raw))
	return value, nil
}

// The caller independently pins this key and generation to its admitted SDK.
func (self OriginalContractRequest) Verify(ctx context.Context) error {
	raw, err := self.signingBytes(ctx)
	if err != nil {
		return err
	}
	if !ed25519.Verify(self.PublicKey[:], raw, self.Signature[:]) {
		return errors.New("original contract request signature differs")
	}
	return ctx.Err()
}

// The immutable leaf includes the signature, not merely a parsed request.
func (self OriginalContractRequest) Bytes(ctx context.Context) ([]byte, error) {
	if err := self.Verify(ctx); err != nil {
		return nil, err
	}
	return json.Marshal(self)
}

// Canonical JSON refuses duplicate fields and trailing data without rewriting
// the original protobuf message stored within it.
func DecodeOriginalContractRequest(ctx context.Context, raw []byte) (OriginalContractRequest, error) {
	var value OriginalContractRequest
	if err := originalContractContext(ctx); err != nil {
		return value, err
	}
	if len(raw) == 0 || len(raw) > MaximumOriginalContractRequestBytes {
		return value, errors.New("original contract request exceeds capacity")
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return OriginalContractRequest{}, err
	}
	canonical, err := value.Bytes(ctx)
	if err != nil || !bytes.Equal(raw, canonical) {
		return OriginalContractRequest{}, errors.Join(errors.New("original contract request is not canonical"), err)
	}
	return value, nil
}

// The receipt must contain the exact original echoed request and source-owned
// reservation. Neither a later SQL provider vector nor a caller summary enters.
func (self OriginalContractAdmission) facts(ctx context.Context) (OriginalContractCreationFacts, error) {
	var facts OriginalContractCreationFacts
	if err := originalContractContext(ctx); err != nil {
		return facts, err
	}
	request, err := DecodeOriginalContractRequest(ctx, self.Request)
	if err != nil || self.Schema != OriginalContractAdmissionSchema || self.PublicKey != request.PublicKey {
		return facts, errors.Join(errors.New("original contract admission owner differs"), err)
	}
	var requested CreateContract
	if err := decodeOriginalContractFrame(request.RequestFrame, MessageType_TransferCreateContract, &requested); err != nil {
		return facts, err
	}
	var response CreateContractResult
	if err := decodeOriginalContractFrame(self.ResultFrame, MessageType_TransferCreateContractResult, &response); err != nil {
		return facts, err
	}
	if response.Error != nil || response.Contract == nil || response.CreateContract == nil || !proto.Equal(&requested, response.CreateContract) || len(response.Contract.ProtoReflect().GetUnknown()) != 0 {
		return facts, errors.New("original contract response does not echo its request")
	}
	var stored StoredContract
	if len(response.Contract.StoredContractBytes) > MaximumOriginalWorkContractBytes || proto.Unmarshal(response.Contract.StoredContractBytes, &stored) != nil || len(stored.ProtoReflect().GetUnknown()) != 0 || len(stored.ContractId) != 16 || !bytes.Equal(stored.SourceId, request.ClientId[:]) || !bytes.Equal(stored.DestinationId, requested.DestinationId) || stored.TransferByteCount > math.MaxInt64 || len(stored.StreamId) != 0 && len(stored.StreamId) != 16 {
		return facts, errors.New("original contract returned reservation differs")
	}
	mode := response.Contract.ProvideMode
	if mode < ProvideMode_Network || mode > ProvideMode_PublicStream {
		return facts, errors.New("original contract provide mode is unsupported")
	}
	facts = OriginalContractCreationFacts{
		DomainHash: request.DomainHash, ClientId: request.ClientId,
		Generation: request.Generation, RequestId: request.RequestId,
		PublicKey: request.PublicKey, ContractId: [16]byte(stored.ContractId),
		StoredContract: bytes.Clone(response.Contract.StoredContractBytes),
		SourceId:       request.ClientId, DestinationId: [16]byte(stored.DestinationId),
		ReservedBytes: stored.TransferByteCount, ProvideMode: mode,
		UsageOriginIsSource: !requested.Companion && mode != ProvideMode_Stream,
		IntermediaryIds:     make([][16]byte, len(requested.IntermediaryIds)),
	}
	if facts.ContractId == ([16]byte{}) {
		return OriginalContractCreationFacts{}, errors.New("original contract identity is empty")
	}
	copy(facts.StreamId[:], stored.StreamId)
	for index, id := range requested.IntermediaryIds {
		facts.IntermediaryIds[index] = [16]byte(id)
	}
	return facts, ctx.Err()
}

// Sign only a verified response binding, preserving all original bytes.
func (self OriginalContractAdmission) signingBytes(ctx context.Context) ([]byte, error) {
	if _, err := self.facts(ctx); err != nil {
		return nil, err
	}
	self.Signature = [ed25519.SignatureSize]byte{}
	raw, err := json.Marshal(self)
	if err != nil || len(raw) > MaximumOriginalContractAdmissionBytes {
		return nil, errors.New("original contract admission exceeds capacity")
	}
	return raw, nil
}

// The originating SDK signs before publishing the response into its queue.
func SignOriginalContractAdmission(ctx context.Context, value OriginalContractAdmission, key ed25519.PrivateKey) (OriginalContractAdmission, error) {
	if len(key) != ed25519.PrivateKeySize || !bytes.Equal(ed25519.NewKeyFromSeed(key[:ed25519.SeedSize]), key) {
		return OriginalContractAdmission{}, errors.New("original contract admission key is invalid")
	}
	value.Schema = OriginalContractAdmissionSchema
	copy(value.PublicKey[:], key[ed25519.SeedSize:])
	raw, err := value.signingBytes(ctx)
	if err != nil {
		return OriginalContractAdmission{}, err
	}
	copy(value.Signature[:], ed25519.Sign(key, raw))
	return value, nil
}

// Registration, window and complete participant coverage remain caller duties.
func (self OriginalContractAdmission) Verify(ctx context.Context) error {
	raw, err := self.signingBytes(ctx)
	if err != nil {
		return err
	}
	if !ed25519.Verify(self.PublicKey[:], raw, self.Signature[:]) {
		return errors.New("original contract admission signature differs")
	}
	return ctx.Err()
}

// Return original facts only after the full nested signatures have passed.
func (self OriginalContractAdmission) Facts(ctx context.Context) (OriginalContractCreationFacts, error) {
	if err := self.Verify(ctx); err != nil {
		return OriginalContractCreationFacts{}, err
	}
	return self.facts(ctx)
}

// Exact signed bytes are the immutable retained content identity.
func (self OriginalContractAdmission) Bytes(ctx context.Context) ([]byte, error) {
	if err := self.Verify(ctx); err != nil {
		return nil, err
	}
	return json.Marshal(self)
}

// Bounds precede decoding and canonical comparison preserves nested originals.
func DecodeOriginalContractAdmission(ctx context.Context, raw []byte) (OriginalContractAdmission, error) {
	var value OriginalContractAdmission
	if err := originalContractContext(ctx); err != nil {
		return value, err
	}
	if len(raw) == 0 || len(raw) > MaximumOriginalContractAdmissionBytes {
		return value, errors.New("original contract admission exceeds capacity")
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return OriginalContractAdmission{}, err
	}
	canonical, err := value.Bytes(ctx)
	if err != nil || !bytes.Equal(raw, canonical) {
		return OriginalContractAdmission{}, errors.Join(errors.New("original contract admission is not canonical"), err)
	}
	return value, nil
}

// A request digest can be compared across independent retained custody copies.
func (self OriginalContractRequest) ContentHash(ctx context.Context) ([32]byte, error) {
	raw, err := self.Bytes(ctx)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(raw), nil
}
