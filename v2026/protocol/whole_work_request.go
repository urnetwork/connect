// Independently authorized window requests drive the SDK's owned cut outbox.
// Transport custody does not select the request approver or a replacement key.
package protocol

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
)

const OriginalWorkRequestSchema = "urnetwork-sdk-whole-work-request-v1"
const OriginalWorkRequestsSchema = "urnetwork-sdk-whole-work-requests-v1"
const OriginalWorkReceiptSchema = "urnetwork-sdk-whole-work-receipt-v1"
const MaximumOriginalWorkRequestBytes = 8 * 1024
const MaximumOriginalWorkRequests = 8
const MaximumOriginalWorkSubmissionBytes = 12 * 1024 * 1024

// Requests name an enrolled SDK generation and one original boundary. A new
// request id cannot reinterpret a previously retained cut for the same window.
type OriginalWorkRequest struct {
	Schema        string                      `json:"schema"`
	RequestId     [16]byte                    `json:"request_id"`
	DomainHash    [32]byte                    `json:"domain_hash"`
	ClientId      [16]byte                    `json:"client_id"`
	Generation    [16]byte                    `json:"generation"`
	PublicKey     [32]byte                    `json:"public_key"`
	Epoch         uint64                      `json:"epoch"`
	Kind          string                      `json:"kind"`
	Block         uint64                      `json:"block"`
	BlockHash     [32]byte                    `json:"block_hash"`
	IssuedAtUnix  int64                       `json:"issued_at_unix"`
	ExpiresAtUnix int64                       `json:"expires_at_unix"`
	Signer        [32]byte                    `json:"signer"`
	Signature     [ed25519.SignatureSize]byte `json:"signature"`
}

// Lists carry original bytes, including their signature and canonical encoding.
type OriginalWorkRequests struct {
	Schema   string   `json:"schema"`
	Requests [][]byte `json:"requests"`
}

// Public receipt intake preserves both originals under their exact hashes.
type OriginalWorkCutSubmission struct {
	Request []byte `json:"request"`
	Cut     []byte `json:"cut"`
}

// An acknowledgment grants no economic authority and never deletes the outbox.
type OriginalWorkCutReceipt struct {
	Schema      string   `json:"schema"`
	RequestHash [32]byte `json:"request_hash"`
	CutHash     [32]byte `json:"cut_hash"`
}

// Fixed fields and a bounded authorization interval prevent open-ended signing.
func (self OriginalWorkRequest) signingBytes() ([]byte, error) {
	if self.Schema != OriginalWorkRequestSchema || self.RequestId == ([16]byte{}) || self.DomainHash == ([32]byte{}) || self.ClientId == ([16]byte{}) || self.Generation == ([16]byte{}) || self.PublicKey == ([32]byte{}) || self.Signer == ([32]byte{}) || self.Block == 0 || self.BlockHash == ([32]byte{}) || self.Kind != "start" && self.Kind != "end" || self.IssuedAtUnix <= 0 || self.ExpiresAtUnix <= self.IssuedAtUnix || self.ExpiresAtUnix-self.IssuedAtUnix > 3600 {
		return nil, errors.New("whole-work boundary request identity or interval is invalid")
	}
	self.Signature = [ed25519.SignatureSize]byte{}
	return json.Marshal(self)
}

// Only the independent window owner supplies this private signing key.
func SignOriginalWorkRequest(request OriginalWorkRequest, key ed25519.PrivateKey) (OriginalWorkRequest, error) {
	if len(key) != ed25519.PrivateKeySize || !bytes.Equal(ed25519.NewKeyFromSeed(key[:ed25519.SeedSize]), key) {
		return OriginalWorkRequest{}, errors.New("whole-work request signing key is invalid")
	}
	request.Schema = OriginalWorkRequestSchema
	copy(request.Signer[:], key[ed25519.SeedSize:])
	raw, err := request.signingBytes()
	if err != nil {
		return OriginalWorkRequest{}, err
	}
	copy(request.Signature[:], ed25519.Sign(key, raw))
	return request, nil
}

// Expected authority is passed from independently owned configuration.
func (self OriginalWorkRequest) Verify(expected [32]byte) error {
	raw, err := self.signingBytes()
	if err != nil {
		return err
	}
	if expected == ([32]byte{}) || self.Signer != expected || !ed25519.Verify(expected[:], raw, self.Signature[:]) {
		return errors.New("whole-work request authority differs")
	}
	return nil
}

// Canonical originals survive retry and server custody without reconstruction.
func (self OriginalWorkRequest) Bytes() ([]byte, error) {
	if err := self.Verify(self.Signer); err != nil {
		return nil, err
	}
	return json.Marshal(self)
}

// Bounds precede parsing; canonical readback rejects duplicate and extra fields.
func DecodeOriginalWorkRequest(raw []byte, expected [32]byte) (OriginalWorkRequest, error) {
	var request OriginalWorkRequest
	if len(raw) == 0 || len(raw) > MaximumOriginalWorkRequestBytes {
		return request, errors.New("whole-work request exceeds capacity")
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		return OriginalWorkRequest{}, err
	}
	if err := request.Verify(expected); err != nil {
		return OriginalWorkRequest{}, err
	}
	canonical, _ := request.Bytes()
	if !bytes.Equal(canonical, raw) {
		return OriginalWorkRequest{}, errors.New("whole-work request is not canonical")
	}
	return request, nil
}

// Reconstruct the same exact request/cut relation in SDK, receipt and validator.
func (self OriginalWorkRequest) Matches(cut OriginalWorkCut) bool {
	return cut.DomainHash == self.DomainHash && cut.ClientId == self.ClientId && cut.Generation == self.Generation && cut.PublicKey == self.PublicKey && cut.Epoch == self.Epoch && cut.Block == self.Block && cut.BlockHash == self.BlockHash
}

// Receipt verification is independent of request freshness: late delivery of a
// previously retained original remains useful after capture permission expired.
func VerifyOriginalWorkSubmission(ctx context.Context, submission OriginalWorkCutSubmission, expected [32]byte) (OriginalWorkCutReceipt, error) {
	request, err := DecodeOriginalWorkRequest(submission.Request, expected)
	if err != nil {
		return OriginalWorkCutReceipt{}, err
	}
	cut, err := DecodeOriginalWorkCut(ctx, submission.Cut)
	if err != nil {
		return OriginalWorkCutReceipt{}, err
	}
	if !request.Matches(cut) {
		return OriginalWorkCutReceipt{}, errors.New("whole-work receipt cut differs from request")
	}
	return OriginalWorkCutReceipt{Schema: OriginalWorkReceiptSchema, RequestHash: sha256.Sum256(submission.Request), CutHash: sha256.Sum256(submission.Cut)}, nil
}
