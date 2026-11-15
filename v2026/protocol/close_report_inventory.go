// A client-owned report chain proves the complete increments before one terminal
// report. It does not attest a server clock, whole epoch, reliability or eligibility.
package protocol

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"errors"
)

const OriginalCloseInventorySchema = "urnetwork-original-close-inventory-v1"
const OriginalCloseInventoryBytes = len(OriginalCloseInventorySchema) + 1 + 32 + 16 + 16 + 32 + 8 + 8 + 32 + 1 + 32 + ed25519.SignatureSize
const MaximumOriginalCloseInventoryReports = 1024

// Previous hashes the preceding complete inventory envelope. Sequence starts at
// one and a terminal must be last; key rotation cannot reset this contract owner.
type OriginalCloseInventory struct {
	DomainHash           [32]byte
	ClientId             [16]byte
	ContractId           [16]byte
	ReportHash           [32]byte
	Sequence             uint64
	CumulativeAckedBytes uint64
	Previous             [32]byte
	Terminal             bool
	PublicKey            [32]byte
	Signature            [ed25519.SignatureSize]byte
}

// Fixed widths and an independent domain preserve the old report's signed bytes.
func (self OriginalCloseInventory) signingBytes() ([]byte, error) {
	if self.DomainHash == ([32]byte{}) || self.ClientId == ([16]byte{}) || self.ContractId == ([16]byte{}) || self.ReportHash == ([32]byte{}) || self.PublicKey == ([32]byte{}) || self.Sequence == 0 || self.Sequence > MaximumOriginalCloseInventoryReports || (self.Sequence == 1) != (self.Previous == ([32]byte{})) {
		return nil, errors.New("original close inventory identity or predecessor is invalid")
	}
	data := append([]byte(OriginalCloseInventorySchema), 0)
	for _, field := range [][]byte{self.DomainHash[:], self.ClientId[:], self.ContractId[:], self.ReportHash[:]} {
		data = append(data, field...)
	}
	data = binary.BigEndian.AppendUint64(data, self.Sequence)
	data = binary.BigEndian.AppendUint64(data, self.CumulativeAckedBytes)
	data = append(data, self.Previous[:]...)
	if self.Terminal {
		data = append(data, 1)
	} else {
		data = append(data, 0)
	}
	return append(data, self.PublicKey[:]...), nil
}

// One exact key owner signs both the legacy original and its optional chain.
func SignOriginalCloseInventory(value OriginalCloseInventory, key ed25519.PrivateKey) (OriginalCloseInventory, error) {
	if len(key) != ed25519.PrivateKeySize || !bytes.Equal(ed25519.NewKeyFromSeed(key[:ed25519.SeedSize]), key) {
		return OriginalCloseInventory{}, errors.New("original close inventory key is malformed")
	}
	copy(value.PublicKey[:], key[ed25519.SeedSize:])
	data, err := value.signingBytes()
	if err != nil {
		return OriginalCloseInventory{}, err
	}
	copy(value.Signature[:], ed25519.Sign(key, data))
	return value, nil
}

// The signature proves one client's original count, not an operator's census.
func (self OriginalCloseInventory) Bytes() ([]byte, error) {
	data, err := self.signingBytes()
	if err != nil {
		return nil, err
	}
	if !ed25519.Verify(self.PublicKey[:], data, self.Signature[:]) {
		return nil, errors.New("original close inventory signature differs")
	}
	return append(data, self.Signature[:]...), nil
}

// Decode one complete bounded envelope, including canonical booleans.
func DecodeOriginalCloseInventory(raw []byte) (OriginalCloseInventory, error) {
	var value OriginalCloseInventory
	prefix := append([]byte(OriginalCloseInventorySchema), 0)
	if len(raw) != OriginalCloseInventoryBytes || !bytes.Equal(raw[:len(prefix)], prefix) {
		return value, errors.New("original close inventory schema or length differs")
	}
	data := raw[len(prefix):]
	for _, field := range [][]byte{value.DomainHash[:], value.ClientId[:], value.ContractId[:], value.ReportHash[:]} {
		copy(field, data[:len(field)])
		data = data[len(field):]
	}
	value.Sequence, value.CumulativeAckedBytes = binary.BigEndian.Uint64(data), binary.BigEndian.Uint64(data[8:])
	data = data[16:]
	copy(value.Previous[:], data[:32])
	if data[32] > 1 {
		return OriginalCloseInventory{}, errors.New("original close inventory terminal is not canonical")
	}
	value.Terminal = data[32] == 1
	copy(value.PublicKey[:], data[33:65])
	copy(value.Signature[:], data[65:])
	_, err := value.Bytes()
	return value, err
}

// Registration, clock attribution and contract admission remain separate proofs.
func (self OriginalCloseInventory) Matches(original OriginalCloseReport) bool {
	raw, err := original.Bytes()
	return err == nil && self.DomainHash == original.DomainHash && self.ClientId == original.ClientId && self.ContractId == original.ContractId && self.PublicKey == original.PublicKey && self.ReportHash == sha256.Sum256(raw) && self.Terminal != original.Checkpoint && self.CumulativeAckedBytes >= original.AckedByteCount
}
