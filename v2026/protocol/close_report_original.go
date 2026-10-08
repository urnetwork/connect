// Original close reports retain client-key signatures independently of transport
// authentication. Individual increments do not certify a complete earning window.
package protocol

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"errors"
)

const OriginalCloseReportSchema = "urnetwork-original-close-report-v1"
const OriginalCloseReportBytes = len(OriginalCloseReportSchema) + 1 + 32 + 16 + 16 + 16 + 32 + 8 + 8 + 1 + ed25519.SignatureSize

// The domain is the exact admitted client-key-history domain digest. The
// signature authenticates the tuple, not the key's registration or eligibility.
type OriginalCloseReport struct {
	DomainHash       [32]byte
	ClientId         [16]byte
	ContractId       [16]byte
	ReportId         [16]byte
	PublicKey        [32]byte
	AckedByteCount   uint64
	UnackedByteCount uint64
	Checkpoint       bool
	Signature        [ed25519.SignatureSize]byte
}

// Fixed widths, one version tag and a canonical boolean prevent alternate signed
// spellings. Zero completed bytes are valid; a missing identity is not.
func (self OriginalCloseReport) signingBytes() ([]byte, error) {
	if self.DomainHash == ([32]byte{}) || self.ClientId == ([16]byte{}) || self.ContractId == ([16]byte{}) || self.ReportId == ([16]byte{}) || self.PublicKey == ([32]byte{}) {
		return nil, errors.New("original close report identity or domain is incomplete")
	}
	data := append([]byte(OriginalCloseReportSchema), 0)
	data = append(data, self.DomainHash[:]...)
	data = append(data, self.ClientId[:]...)
	data = append(data, self.ContractId[:]...)
	data = append(data, self.ReportId[:]...)
	data = append(data, self.PublicKey[:]...)
	data = binary.BigEndian.AppendUint64(data, self.AckedByteCount)
	data = binary.BigEndian.AppendUint64(data, self.UnackedByteCount)
	if self.Checkpoint {
		data = append(data, 1)
	} else {
		data = append(data, 0)
	}
	return data, nil
}

// Sign one owned key snapshot. A malformed public half is refused, rather than
// replaced implicitly; no caller-owned report is changed on failure.
func SignOriginalCloseReport(report OriginalCloseReport, key ed25519.PrivateKey) (OriginalCloseReport, error) {
	if len(key) != ed25519.PrivateKeySize {
		return OriginalCloseReport{}, errors.New("original close report signing owner is malformed")
	}
	derived := ed25519.NewKeyFromSeed(key[:ed25519.SeedSize])
	if !bytes.Equal(derived, key) {
		return OriginalCloseReport{}, errors.New("original close report signing key halves differ")
	}
	copy(report.PublicKey[:], derived[ed25519.SeedSize:])
	data, err := report.signingBytes()
	if err != nil {
		return OriginalCloseReport{}, err
	}
	copy(report.Signature[:], ed25519.Sign(derived, data))
	return report, nil
}

// Registration and complete-work coverage are separate caller obligations.
func (self OriginalCloseReport) Verify() error {
	data, err := self.signingBytes()
	if err != nil {
		return err
	}
	if !ed25519.Verify(self.PublicKey[:], data, self.Signature[:]) {
		return errors.New("original close report client signature differs")
	}
	return nil
}

// The immutable identity includes the signature, not only its unsigned payload.
func (self OriginalCloseReport) Bytes() ([]byte, error) {
	if err := self.Verify(); err != nil {
		return nil, err
	}
	data, _ := self.signingBytes()
	return append(data, self.Signature[:]...), nil
}

// Use the complete canonical bytes for retained content references.
func (self OriginalCloseReport) ContentHash() ([32]byte, error) {
	data, err := self.Bytes()
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(data), nil
}

// Decode one bounded version only; unsupported envelopes cannot be interpreted
// as an unsigned original or as another report's evidence.
func DecodeOriginalCloseReport(data []byte) (OriginalCloseReport, error) {
	var report OriginalCloseReport
	prefix := append([]byte(OriginalCloseReportSchema), 0)
	if len(data) != OriginalCloseReportBytes || !bytes.Equal(data[:len(prefix)], prefix) {
		return report, errors.New("original close report schema or length differs")
	}
	remaining := data[len(prefix):]
	for _, field := range [][]byte{report.DomainHash[:], report.ClientId[:], report.ContractId[:], report.ReportId[:], report.PublicKey[:]} {
		copy(field, remaining[:len(field)])
		remaining = remaining[len(field):]
	}
	report.AckedByteCount = binary.BigEndian.Uint64(remaining[:8])
	report.UnackedByteCount = binary.BigEndian.Uint64(remaining[8:16])
	if remaining[16] > 1 {
		return OriginalCloseReport{}, errors.New("original close report checkpoint is not canonical")
	}
	report.Checkpoint = remaining[16] == 1
	copy(report.Signature[:], remaining[17:])
	if err := report.Verify(); err != nil {
		return OriginalCloseReport{}, err
	}
	return report, nil
}

// Transport ownership supplies the client id; the signed bytes never select a
// different authenticated party or change the accounted incremental amounts.
func (self OriginalCloseReport) Matches(clientId [16]byte, report *CloseContract) bool {
	return report != nil && self.ClientId == clientId && bytes.Equal(self.ContractId[:], report.ContractId) && bytes.Equal(self.ReportId[:], report.ReportId) && self.AckedByteCount == report.AckedByteCount && self.UnackedByteCount == report.UnackedByteCount && self.Checkpoint == report.Checkpoint
}
