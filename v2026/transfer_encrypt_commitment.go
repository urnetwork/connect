package connect

// Contract certificate commitments are checked against the selected epoch's presented leaf.
import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
)

// parsePemCertificates parses PEM-encoded certificate blocks into
// `*x509.Certificate` values. Each list entry is one PEM block; only PEM
// is accepted.
func parsePemCertificates(pems [][]byte) ([]*x509.Certificate, error) {
	out := make([]*x509.Certificate, 0, len(pems))
	for _, b := range pems {
		if len(b) == 0 {
			continue
		}
		block, rest := pem.Decode(b)
		if block == nil {
			return nil, errors.New("certificate is not PEM-encoded")
		}
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("unexpected PEM block type %q (expected CERTIFICATE)", block.Type)
		}
		if 0 < len(bytes.TrimSpace(rest)) {
			return nil, errors.New("certificate has trailing content after PEM block")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("invalid PEM certificate: %w", err)
		}
		out = append(out, cert)
	}
	return out, nil
}

// verifyPeerCertificateAgainstContract checks that the peer's leaf
// certificate matches one of the certificates committed to in the contract.
// `expectedPems` is the PEM-encoded chain from
// `Contract.ProvideTlsCertificate` / `StoredContract.ProvideTlsCertificate`.
// When empty (no contract commitment), verification is skipped.
//
// On mismatch, the returned error carries the peer leaf's subject and the
// list of expected subjects so the SendSequence's `[s]` log line is
// self-contained — useful when triaging encrypted-send rejections from
// production logs.
func verifyPeerCertificateAgainstContract(peer []*x509.Certificate, expectedPems [][]byte) (bool, error) {
	if len(expectedPems) == 0 {
		return true, nil
	}
	if len(peer) == 0 || peer[0] == nil {
		return false, errors.New("peer presented no certificate but contract committed to a TLS identity")
	}
	expected, err := parsePemCertificates(expectedPems)
	if err != nil {
		return false, fmt.Errorf("contract certificate parse error: %w", err)
	}
	peerLeaf := peer[0]
	for _, e := range expected {
		if bytes.Equal(peerLeaf.Raw, e.Raw) {
			return true, nil
		}
	}
	expectedSubjects := make([]string, 0, len(expected))
	for _, e := range expected {
		expectedSubjects = append(expectedSubjects, e.Subject.String())
	}
	return false, fmt.Errorf(
		"peer cert subject=%q does not match any of %d contract-committed cert(s): [%s]",
		peerLeaf.Subject.String(),
		len(expected),
		strings.Join(expectedSubjects, ", "),
	)
}
