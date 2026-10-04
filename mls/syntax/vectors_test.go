// Family 16 driven over every vector in the file p8 task 6 vendors and pins. The
// runner itself is exported from vectors.go and is what the wave 4 vector registry
// calls; it is exercised here as well so a varint defect is red in this package's own
// suite rather than only in a later wave.
package syntax

import (
	"encoding/json"
	"os"
	"testing"
)

// deserializationVectorFile is the path, relative to this package, to the vendored
// family 16 corpus. p8 task 6 vendors and digest-pins the file itself; this package
// only reads it.
const deserializationVectorFile = "../testdata/vectors/deserialization.json"

// TestVectorDeserialization loads the vendored family 16 corpus and runs every
// vector through VerifyDeserializationVector. It also asserts a minimum vector
// count, so a runner that silently iterates zero vectors — wrong JSON shape, wrong
// field name, an empty file — fails loudly here instead of passing vacuously.
func TestVectorDeserialization(t *testing.T) {
	raw, err := os.ReadFile(deserializationVectorFile)
	if err != nil {
		t.Fatalf("reading the vendored family 16 vectors: %v", err)
	}
	vectors := []json.RawMessage{}
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatalf("parsing the vendored family 16 vectors: %v", err)
	}
	if len(vectors) < 14 {
		t.Fatalf("family 16 has %d vectors, want at least the 14 in the pinned corpus", len(vectors))
	}
	for _, vector := range vectors {
		VerifyDeserializationVector(t, vector)
	}
}

// TestVerifyDeserializationVectorRejectsAMismatch confirms the runner checks the
// decoded length against the vector's claimed length rather than only checking that
// decoding succeeded: the runner must reject a header that carries a different
// length than its vector claims, or family 16 is a no op that passes against any
// corpus. The probe runs on its own goroutine so a t.Fatalf inside the runner ends
// the probe rather than this test.
func TestVerifyDeserializationVectorRejectsAMismatch(t *testing.T) {
	probe := &testing.T{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		VerifyDeserializationVector(probe, json.RawMessage(`{"vlbytes_header":"3f","length":62}`))
	}()
	<-done
	if !probe.Failed() {
		t.Errorf("a header carrying 63 was accepted against a claimed length of 62")
	}
}

// TestVerifyDeserializationVectorRejectsANonCanonicalHeader confirms the runner
// surfaces ReadVarint's minimality check rather than masking it: a non minimal
// header must fail the vector rather than pass it. The decoder rejects it
// outright, and if it ever stopped doing so the generate direction would catch it
// anyway, because our encoder emits 4040 for 64 and never the four octet form.
func TestVerifyDeserializationVectorRejectsANonCanonicalHeader(t *testing.T) {
	probe := &testing.T{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		VerifyDeserializationVector(probe, json.RawMessage(`{"vlbytes_header":"80000040","length":64}`))
	}()
	<-done
	if !probe.Failed() {
		t.Errorf("a non minimal header was accepted for the length it carries")
	}
}
