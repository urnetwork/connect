// A signed startup identity lets the independently admitted request authority
// discover a real SDK generation without granting population or work authority.
package protocol

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
)

const OriginalWorkOwnerSchema = "urnetwork-sdk-whole-work-owner-v1"
const OriginalWorkOwnerReceiptSchema = "urnetwork-sdk-whole-work-owner-receipt-v1"
const MaximumOriginalWorkOwnerBytes = 4096

// Key possession authenticates this tuple only. The complete owner roster still
// requires an independent authority, and generations cannot be caller-selected.
type OriginalWorkOwnerEnrollment struct {
	Schema     string   `json:"schema"`
	DomainHash [32]byte `json:"domain_hash"`
	ClientId   [16]byte `json:"client_id"`
	Generation [16]byte `json:"generation"`
	PublicKey  [32]byte `json:"public_key"`
	Signature  [64]byte `json:"signature"`
}

// An immutable receipt binds exactly the canonical original submitted bytes.
type OriginalWorkOwnerReceipt struct {
	Schema    string   `json:"schema"`
	OwnerHash [32]byte `json:"owner_hash"`
}

// The versioned schema is part of the signed payload; no clock is reinterpreted.
func (self OriginalWorkOwnerEnrollment) signingBytes(ctx context.Context) ([]byte, error) {
	if ctx == nil {
		return nil, errors.New("whole-work owner requires an owner context")
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, context.Cause(ctx))
	}
	if self.Schema != OriginalWorkOwnerSchema || self.DomainHash == ([32]byte{}) || self.ClientId == ([16]byte{}) || self.Generation == ([16]byte{}) || self.PublicKey == ([32]byte{}) {
		return nil, errors.New("whole-work owner identity is incomplete")
	}
	self.Signature = [64]byte{}
	return json.Marshal(self)
}

// The SDK signs only its own live generation, never a recovered old lifecycle.
func SignOriginalWorkOwnerEnrollment(ctx context.Context, value OriginalWorkOwnerEnrollment, key ed25519.PrivateKey) (OriginalWorkOwnerEnrollment, error) {
	if len(key) != ed25519.PrivateKeySize || !bytes.Equal(ed25519.NewKeyFromSeed(key[:ed25519.SeedSize]), key) {
		return OriginalWorkOwnerEnrollment{}, errors.New("whole-work owner signing key is invalid")
	}
	value.Schema = OriginalWorkOwnerSchema
	copy(value.PublicKey[:], key[ed25519.SeedSize:])
	raw, err := value.signingBytes(ctx)
	if err != nil {
		return OriginalWorkOwnerEnrollment{}, err
	}
	copy(value.Signature[:], ed25519.Sign(key, raw))
	return value, nil
}

// Self-authentication proves key possession and cannot choose expected owners.
func (self OriginalWorkOwnerEnrollment) Verify(ctx context.Context) error {
	raw, err := self.signingBytes(ctx)
	if err != nil {
		return err
	}
	if !ed25519.Verify(self.PublicKey[:], raw, self.Signature[:]) {
		return errors.New("whole-work owner signature is invalid")
	}
	return errors.Join(ctx.Err(), context.Cause(ctx))
}

// Repeated enrollment uses identical canonical bytes for one identity tuple.
func (self OriginalWorkOwnerEnrollment) Bytes(ctx context.Context) ([]byte, error) {
	if err := self.Verify(ctx); err != nil {
		return nil, err
	}
	return json.Marshal(self)
}

// Exact canonical spelling rejects duplicate, omitted and unknown input fields.
func DecodeOriginalWorkOwnerEnrollment(ctx context.Context, raw []byte) (OriginalWorkOwnerEnrollment, error) {
	var value OriginalWorkOwnerEnrollment
	if ctx == nil {
		return value, errors.New("whole-work owner requires an owner context")
	}
	if err := ctx.Err(); err != nil {
		return value, errors.Join(err, context.Cause(ctx))
	}
	if len(raw) == 0 || len(raw) > MaximumOriginalWorkOwnerBytes {
		return value, errors.New("whole-work owner exceeds finite encoding bounds")
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return OriginalWorkOwnerEnrollment{}, err
	}
	canonical, err := value.Bytes(ctx)
	if err != nil {
		return OriginalWorkOwnerEnrollment{}, err
	}
	if !bytes.Equal(raw, canonical) {
		return OriginalWorkOwnerEnrollment{}, errors.New("whole-work owner encoding is not canonical")
	}
	return value, nil
}
