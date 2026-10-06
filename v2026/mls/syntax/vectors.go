// Test vector family 16, vector deserialization, from the mlswg corpus that the
// validation and interop plan vendors and pins. Both directions per spec A section
// 4.2.1: the supplied header must decode to the supplied length, and encoding that
// length with our own encoder must reproduce the supplied header. Verification alone
// cannot see an encoder and a decoder that are wrong in the same direction.
//
// This is the only implementation of family 16 in the system. The vector registry
// entry in package mls is a shim that assigns this function to VectorFamily.Verify,
// so the family runs against the ReadVarint and WriteVarint methods this package
// ships rather than against a second copy of the length prefix.
//
// It is a non test file on purpose: a symbol declared in a _test.go file is visible
// only inside its own test binary, and the shim that registers this family is in
// package mls. The only cost is testing in this package's import graph, which is
// still standard library and so leaves the layering gate green.
package syntax

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"testing"
)

// deserializationVector is one row of deserialization.json: an encoded vlbytes
// header and the length it carries.
type deserializationVector struct {
	VlbytesHeader string `json:"vlbytes_header"`
	Length        uint32 `json:"length"`
}

// VerifyDeserializationVector verifies one family 16 vector in both directions:
// decoding the given header must reproduce the given length with nothing left
// over, and encoding the given length must reproduce the given header exactly. A
// failure reports which vector, which direction, and the expected and actual bytes
// or values, so a mismatch against the upstream corpus is diagnosable rather than
// a bare "mismatch". The corpus carries well formed headers only, so it exercises
// acceptance; rejection of a reserved prefix and of a non minimal encoding is rule
// 1 of spec A section 5.8 and lives in
// TestReadVarintRejectsEverythingButTheMinimalForm.
func VerifyDeserializationVector(t *testing.T, raw json.RawMessage) {
	t.Helper()
	vector := deserializationVector{}
	if err := json.Unmarshal(raw, &vector); err != nil {
		t.Fatalf("family 16 vector %s does not parse: %v", raw, err)
	}
	header, err := hex.DecodeString(vector.VlbytesHeader)
	if err != nil {
		t.Fatalf("family 16 header %q is not hex: %v", vector.VlbytesHeader, err)
	}
	// verify direction: the vendored header must decode to the vendored length,
	// with the header fully consumed.
	r := NewReader(header)
	got, err := r.ReadVarint()
	if err != nil {
		t.Errorf("header %s: decode gave %v, want length %d", vector.VlbytesHeader, err, vector.Length)
		return
	}
	if got != vector.Length {
		t.Errorf("header %s: decoded %d, want %d", vector.VlbytesHeader, got, vector.Length)
	}
	if err := r.Done(); err != nil {
		t.Errorf("header %s: %d octets left unconsumed", vector.VlbytesHeader, r.Remaining())
	}
	// generate direction: encoding the vendored length must reproduce the
	// vendored header exactly, byte for byte.
	w := NewWriter()
	w.WriteVarint(vector.Length)
	out, err := w.Bytes()
	if err != nil {
		t.Errorf("encoding %d gave %v, want header %s", vector.Length, err, vector.VlbytesHeader)
		return
	}
	if !bytes.Equal(out, header) {
		t.Errorf("encoding %d gave %x, want %s", vector.Length, out, vector.VlbytesHeader)
	}
}
