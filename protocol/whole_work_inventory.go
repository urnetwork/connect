// Whole-owner cuts retain every admitted contract independently of payout SQL.
// A cut authenticates one SDK generation; an independently admitted roster must
// still establish that every expected owner supplied both window boundaries.
package protocol

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"

	"google.golang.org/protobuf/proto"
)

const OriginalWorkCutSchema = "urnetwork-sdk-whole-work-cut-v1"
const MaximumOriginalWorkContracts = 8192
const MaximumOriginalWorkCutBytes = 8 * 1024 * 1024
const MaximumOriginalWorkContractBytes = 16 * 1024

// The raw reservation names both parties. The latest signed close inventory
// binds every prior increment; no aggregate count can replace either original.
type OriginalWorkContract struct {
	ContractId       [16]byte `json:"contract_id"`
	StoredContract   []byte   `json:"stored_contract"`
	OriginalCreation []byte   `json:"original_creation,omitempty"`
	LatestInventory  []byte   `json:"latest_inventory,omitempty"`
}

// Restart creates another generation. Missing generations, incomplete cuts and
// unresolved create requests cannot be promoted to an empty complete window.
type OriginalWorkCut struct {
	Schema     string                      `json:"schema"`
	DomainHash [32]byte                    `json:"domain_hash"`
	ClientId   [16]byte                    `json:"client_id"`
	Generation [16]byte                    `json:"generation"`
	PublicKey  [32]byte                    `json:"public_key"`
	Epoch      uint64                      `json:"epoch"`
	Block      uint64                      `json:"block"`
	BlockHash  [32]byte                    `json:"block_hash"`
	Revision   uint64                      `json:"revision"`
	Complete   bool                        `json:"complete"`
	Contracts  []OriginalWorkContract      `json:"contracts"`
	Signature  [ed25519.SignatureSize]byte `json:"signature"`
}

// Validate the original party and terminal head before using its signed owner.
func (self OriginalWorkContract) Parties() ([16]byte, [16]byte, error) {
	var source, destination [16]byte
	if self.ContractId == ([16]byte{}) || len(self.StoredContract) == 0 || len(self.StoredContract) > MaximumOriginalWorkContractBytes {
		return source, destination, errors.New("whole-work reservation is absent or oversized")
	}
	var stored StoredContract
	if err := proto.Unmarshal(self.StoredContract, &stored); err != nil || !bytes.Equal(stored.ContractId, self.ContractId[:]) || len(stored.SourceId) != 16 || len(stored.DestinationId) != 16 {
		return source, destination, errors.New("whole-work reservation identity differs")
	}
	copy(source[:], stored.SourceId)
	copy(destination[:], stored.DestinationId)
	if source == ([16]byte{}) || destination == ([16]byte{}) || source == destination {
		return source, destination, errors.New("whole-work reservation parties are invalid")
	}
	return source, destination, nil
}

// Check finite canonical state without accepting a caller's completeness claim.
func (self OriginalWorkCut) signingBytes(ctx context.Context) ([]byte, error) {
	if ctx == nil {
		return nil, errors.New("whole-work cut requires an owner")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if self.Schema != OriginalWorkCutSchema || self.DomainHash == ([32]byte{}) || self.ClientId == ([16]byte{}) || self.Generation == ([16]byte{}) || self.PublicKey == ([32]byte{}) || self.Block == 0 || self.BlockHash == ([32]byte{}) || self.Contracts == nil || len(self.Contracts) > MaximumOriginalWorkContracts {
		return nil, errors.New("whole-work cut identity or capacity is invalid")
	}
	var previous [16]byte
	used := 0
	for i, contract := range self.Contracts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		source, destination, err := contract.Parties()
		if err != nil || self.ClientId != source && self.ClientId != destination || i > 0 && bytes.Compare(previous[:], contract.ContractId[:]) >= 0 {
			return nil, errors.New("whole-work cut contract ownership or order differs")
		}
		previous = contract.ContractId
		used += len(contract.StoredContract) + len(contract.OriginalCreation) + len(contract.LatestInventory)
		if used > MaximumOriginalWorkCutBytes/2 {
			return nil, errors.New("whole-work cut exceeds original byte capacity")
		}
		if len(contract.OriginalCreation) != 0 {
			admission, err := DecodeOriginalContractAdmission(ctx, contract.OriginalCreation)
			if err != nil {
				return nil, err
			}
			facts, err := admission.Facts(ctx)
			if err != nil || facts.DomainHash != self.DomainHash || facts.ClientId != source || self.ClientId != source || facts.Generation != self.Generation || facts.PublicKey != self.PublicKey || facts.ContractId != contract.ContractId || !bytes.Equal(facts.StoredContract, contract.StoredContract) {
				return nil, errors.New("whole-work cut original creation differs")
			}
		}
		if len(contract.LatestInventory) != 0 {
			inventory, err := DecodeOriginalCloseInventory(contract.LatestInventory)
			if err != nil || inventory.DomainHash != self.DomainHash || inventory.ClientId != self.ClientId || inventory.ContractId != contract.ContractId {
				return nil, errors.New("whole-work cut close head differs")
			}
		}
	}
	self.Signature = [ed25519.SignatureSize]byte{}
	raw, err := json.Marshal(self)
	if err != nil || len(raw) > MaximumOriginalWorkCutBytes {
		return nil, errors.New("whole-work cut encoding exceeds capacity")
	}
	return raw, nil
}

// Caller ownership must already pin the signing key to this SDK generation.
func SignOriginalWorkCut(ctx context.Context, cut OriginalWorkCut, key ed25519.PrivateKey) (OriginalWorkCut, error) {
	if len(key) != ed25519.PrivateKeySize || !bytes.Equal(ed25519.NewKeyFromSeed(key[:ed25519.SeedSize]), key) {
		return OriginalWorkCut{}, errors.New("whole-work cut signing key is invalid")
	}
	cut.Schema = OriginalWorkCutSchema
	copy(cut.PublicKey[:], key[ed25519.SeedSize:])
	raw, err := cut.signingBytes(ctx)
	if err != nil {
		return OriginalWorkCut{}, err
	}
	copy(cut.Signature[:], ed25519.Sign(key, raw))
	return cut, nil
}

// Completeness remains a separate roster and two-boundary join obligation.
func (self OriginalWorkCut) Verify(ctx context.Context) error {
	raw, err := self.signingBytes(ctx)
	if err != nil {
		return err
	}
	if !ed25519.Verify(self.PublicKey[:], raw, self.Signature[:]) {
		return errors.New("whole-work cut signature differs")
	}
	return ctx.Err()
}

// Only canonical full bytes identify a retained original cut.
func (self OriginalWorkCut) Bytes(ctx context.Context) ([]byte, error) {
	if err := self.Verify(ctx); err != nil {
		return nil, err
	}
	return json.Marshal(self)
}

// Bound allocation before decoding and reject duplicate/unknown spellings by
// requiring the single canonical encoding of the authenticated typed object.
func DecodeOriginalWorkCut(ctx context.Context, raw []byte) (OriginalWorkCut, error) {
	var cut OriginalWorkCut
	if ctx == nil {
		return cut, errors.New("whole-work cut requires an owner")
	}
	if err := ctx.Err(); err != nil {
		return cut, err
	}
	if len(raw) == 0 || len(raw) > MaximumOriginalWorkCutBytes {
		return cut, errors.New("whole-work cut bytes exceed capacity")
	}
	if err := json.Unmarshal(raw, &cut); err != nil {
		return OriginalWorkCut{}, err
	}
	canonical, err := cut.Bytes(ctx)
	if err != nil {
		return OriginalWorkCut{}, err
	}
	if !bytes.Equal(canonical, raw) {
		return OriginalWorkCut{}, errors.New("whole-work cut is not canonical")
	}
	return cut, nil
}

// Hashes preserve original signed custody across public transport and replay.
func (self OriginalWorkCut) ContentHash(ctx context.Context) ([32]byte, error) {
	raw, err := self.Bytes(ctx)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(raw), nil
}
