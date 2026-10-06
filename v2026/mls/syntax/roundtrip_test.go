// Rule 4 as a deterministic property, run on every commit rather than only under
// the fuzzer: twenty thousand generated structures must each encode, decode and
// re-encode to the same bytes. The seed is fixed so a failure is reproducible from
// the test name alone.
//
// The property is only worth what reaches it. CheckRoundTrip returns nil for an
// input that does not decode, so a test that fed it inputs which mostly fail to
// decode would report green having asserted almost nothing — which is what task 14
// measured over a fuzzer's corpora, and why the values here come from a structured
// generator rather than from bytes. The reachability that measurement leaves
// unproven for this file is proven in it, by counting.
package syntax

import (
	"bytes"
	"testing"
)

const roundTripSeed = 20260812
const roundTripRuns = 20000

// the same count task 14 drew its uniform random corpus at, so the decode rate
// below sits directly against the 14 of 4096 it recorded there
const generatedDecodeRuns = 4096

func TestRoundTripProperty(t *testing.T) {
	rng := newTestRand(roundTripSeed)
	checked := 0
	for i := 0; i < roundTripRuns; i += 1 {
		in := generateTestStruct(rng)
		encoded, err := Marshal(&in)
		if err != nil {
			t.Fatalf("run %d: encode gave %v", i, err)
		}
		out := testStruct{}
		if err := Unmarshal(encoded, &out); err != nil {
			t.Fatalf("run %d: decode of %d bytes gave %v", i, len(encoded), err)
		}
		again, err := Marshal(&out)
		if err != nil {
			t.Fatalf("run %d: re-encode gave %v", i, err)
		}
		if !bytes.Equal(encoded, again) {
			t.Fatalf("run %d: re-encode produced %d bytes, want the original %d", i, len(again), len(encoded))
		}
		if err := CheckRoundTrip[testStruct, *testStruct](encoded); err != nil {
			t.Fatalf("run %d: CheckRoundTrip gave %v", i, err)
		}
		checked += 1
	}
	// the count is the only thing standing between this and a test that passes by
	// asserting nothing: a loop that never ran, a run count edited to zero, or a
	// generator that stopped producing decodable bytes all leave every assertion
	// above untouched and the test reporting green
	if checked != roundTripRuns {
		t.Errorf("reached the property on %d of %d runs, want every one of them", checked, roundTripRuns)
	}
	if checked == 0 {
		t.Errorf("the property was never reached, so this test asserted nothing")
	}
}

// TestGeneratedEncodingsAllDecode is the measurement task 14 makes necessary. It
// found that 14 of 4096 uniform random inputs decoded as a structure of two octets
// and one opaque field — 0.34 percent against the easiest target that can be
// built — so for anything the size of testStruct a byte oriented corpus reaches
// the round trip property essentially never. A structured generator is what makes
// the property reachable, and the number that says whether it worked is this one.
// Anything below 100 percent is a generator defect rather than a tolerance: the
// generator emits values it encoded itself, so an encoding of its own that will
// not decode is a codec or generator bug either way.
//
// The predicate is Unmarshal under the default limit, which is the identical call
// CheckRoundTrip makes to decide whether it has an obligation at all, so the count
// is what the property saw rather than a model of it. The five generators in the
// validation and interop plan each need this same check over their own type; a
// generator whose output silently stopped decoding would make every one of those
// properties vacuous in exactly the way that is invisible from a passing run.
func TestGeneratedEncodingsAllDecode(t *testing.T) {
	rng := newTestRand(roundTripSeed)
	decoded := 0
	for i := 0; i < generatedDecodeRuns; i += 1 {
		in := generateTestStruct(rng)
		encoded, err := Marshal(&in)
		if err != nil {
			t.Fatalf("run %d: encode gave %v", i, err)
		}
		out := testStruct{}
		if Unmarshal(encoded, &out) == nil {
			decoded += 1
		}
	}
	t.Logf("%d of %d generated encodings decoded", decoded, generatedDecodeRuns)
	if decoded != generatedDecodeRuns {
		t.Errorf("%d of %d generated encodings decoded, want every one; the generator emits bytes the decoder refuses", decoded, generatedDecodeRuns)
	}
	if decoded == 0 {
		t.Errorf("no generated encoding decoded, so every round trip property over this generator is vacuous")
	}
}

// nil and empty are the same value on the wire, in every container, which is what
// makes the round trip byte exact rather than merely value preserving
func TestRoundTripTreatsNilAndEmptyAlike(t *testing.T) {
	nilled := testStruct{Body: nil, Tail: nil, Items: nil}
	emptied := testStruct{Body: []byte{}, Tail: []byte{}, Items: []testItem{}}
	a, err := Marshal(&nilled)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b, err := Marshal(&emptied)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Errorf("nil encoded to %x and empty to %x; they must be one value on the wire", a, b)
	}
	if err := CheckRoundTrip[testStruct, *testStruct](a); err != nil {
		t.Errorf("CheckRoundTrip gave %v", err)
	}
}
