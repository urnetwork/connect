// Gate 4 property 1, spec A section 4.4: a length prefix must never be used to size
// an allocation before the remaining input is checked. These are measurements rather
// than arguments — each case feeds a decoder a prefix that declares far more than the
// input carries and then reads how many bytes the process actually allocated.
//
// Every assertion here is a byte scale one, and that is deliberate. The first version
// of TestReadOpaqueRejectsAHostileLengthWithoutAllocating in decode_test.go bounded
// testing.AllocsPerRun alone, and review found it could not fail on the regression it
// was named for: AllocsPerRun counts allocation events, not bytes, so a single
// make sized by the declared length moved above validation costs exactly one extra
// event and scores about 1 against any generous bound. Measured against a mutated
// ReadOpaque, that event count scored 1.0 and passed while a runtime.MemStats
// TotalAlloc delta caught the same mutant at 1,073,752,016 bytes. So an event count
// may sit alongside a byte bound — it catches loop based over-allocation, which a
// byte bound over a small input can miss — but never in place of one.
//
// TotalAlloc is process wide and cumulative, so every delta below is taken tightly
// around the calls being measured, and nothing in this package calls t.Parallel,
// which would make those deltas race.
//
// The four entry points the gate names are not equally capable of the defect, and
// saying which is which is part of the evidence:
//
//   - ReadOpaque and ReadOpaqueLP each size a make from the declared prefix. These
//     are the genuine cases, and a mutant that allocates before validating is caught
//     here at 1 GiB and 4 GiB per call respectively.
//   - ReadSub and ReadSubLP return a three index view over the parent's buffer and
//     never size an allocation from the prefix at all. A mutant that skips validation
//     there does not over-allocate; it panics with a slice bounds error. An
//     allocation bound over those two would be incapable of failing, so this file
//     asserts what actually holds for them instead.
//   - ReadVector reaches its region through ReadSub, so it inherits that same
//     incapacity for the prefix itself. What it does own is the item slice, whose
//     capacity hint is attacker influenced, and that one is bounded by a constant
//     rather than by the declared length — which is a property that can fail, so it
//     gets a real measurement of its own below.
package syntax

import (
	"errors"
	"runtime"
	"testing"
)

// allocProbeRuns is the number of rejections each byte delta is taken over. A single
// call already separates a correct rejection from a gigabyte make by six orders of
// magnitude; the loop is what keeps the small per call baseline large enough to be
// read reliably against the noise of a running runtime.
const allocProbeRuns = 1000

// allocProbeBudget is the byte ceiling for allocProbeRuns rejections. It is slack
// rather than a target: the measured figure for a correct rejection is 0 bytes over
// all 1000 runs, since the Reader does not escape, and every mutant measured against
// this file cleared the ceiling by at least three orders of magnitude.
const allocProbeBudget = 1 << 18

// errItemRefused stands in for an element decoder's semantic refusal, so a vector
// can be made to fail on its first element after the item slice has been sized.
var errItemRefused = errors.New("mls syntax test: element decoder refused")

// wideItem is an element whose Go size is far larger than its one byte encoded form,
// which is what makes an unbounded capacity hint measurable: the hint is counted in
// elements, so the amplification from a region's byte length to the bytes allocated
// is the element size.
type wideItem [512]byte

// readRefusedWideItem consumes one byte and then refuses, so ReadVector allocates its
// item slice and abandons it, leaving the capacity hint as the only thing measured.
func readRefusedWideItem(r *Reader) (wideItem, error) {
	if _, err := r.ReadUint8(); err != nil {
		return wideItem{}, err
	}
	return wideItem{}, errItemRefused
}

// measureAllocs runs a rejection allocProbeRuns times and returns how many bytes the
// process allocated doing so, failing the test if any run stopped reporting the
// expected sentinel — a probe that has stopped rejecting is measuring the wrong thing
// and would otherwise pass quietly.
func measureAllocs(t *testing.T, run func() error, wantErr error) uint64 {
	t.Helper()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := 0; i < allocProbeRuns; i += 1 {
		if err := run(); !errors.Is(err, wantErr) {
			t.Fatalf("run %d gave %v, want %v", i, err, wantErr)
		}
	}
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// TestHostileVarintLengthAllocatesNothing is one of the two genuine cases: ReadOpaque
// sizes a make from the varint prefix, so moving that make above validateLength is a
// regression this can actually catch. A four byte input declares 0x3fffffff, about a
// gibibyte, which is both over MaxVectorLength and over the bytes present. Verified
// against a mutant that allocates before validating: 1,073,741,860,192 bytes over
// these 1000 runs, against the 262,144 byte bound.
//
// The event count assertion is kept alongside the byte bound, not instead of it. On
// its own it would score about 1 against a mutant and pass; what it adds is a bound
// on a rejection path that started allocating repeatedly rather than once.
func TestHostileVarintLengthAllocatesNothing(t *testing.T) {
	// bfffffff is the varint for 1073741823 over a four byte input
	hostile := []byte{0xbf, 0xff, 0xff, 0xff}
	grown := measureAllocs(t, func() error {
		r := NewReader(hostile)
		_, err := r.ReadOpaque()
		return err
	}, ErrLengthExceedsMax)
	if grown > allocProbeBudget {
		t.Errorf("%d hostile ReadOpaque calls allocated %d bytes; the length prefix sized a make", allocProbeRuns, grown)
	}
	events := testing.AllocsPerRun(200, func() {
		r := NewReader(hostile)
		_, _ = r.ReadOpaque()
	})
	if events > 4 {
		t.Errorf("a rejected ReadOpaque allocated %.1f times per run, want a small constant event count", events)
	}
	r := NewReader(hostile)
	if _, err := r.ReadOpaque(); !errors.Is(err, ErrLengthExceedsMax) {
		t.Errorf("gave %v, want ErrLengthExceedsMax", err)
	}
	if r.Offset() != 0 {
		t.Errorf("left Offset at %d, want 0: a rejected ReadOpaque must not consume input", r.Offset())
	}
}

// TestHostileLPLengthAllocatesNothing is the second genuine case: ReadOpaqueLP sizes
// a make from the fixed 32 bit prefix, which can declare four gibibytes over an input
// of four bytes. Verified against a mutant that allocates before validating, at
// 107,374,202,912 bytes over 25 runs — that run was shortened from 1000 only because
// 1000 rejections of a four gibibyte make is four tebibytes of churn; the per call
// figure is the whole four gibibytes either way, against a 262,144 byte bound.
func TestHostileLPLengthAllocatesNothing(t *testing.T) {
	hostile := []byte{0xff, 0xff, 0xff, 0xff}
	grown := measureAllocs(t, func() error {
		r := NewReader(hostile)
		_, err := r.ReadOpaqueLP()
		return err
	}, ErrLengthExceedsMax)
	if grown > allocProbeBudget {
		t.Errorf("%d hostile ReadOpaqueLP calls allocated %d bytes; the length prefix sized a make", allocProbeRuns, grown)
	}
	events := testing.AllocsPerRun(200, func() {
		r := NewReader(hostile)
		_, _ = r.ReadOpaqueLP()
	})
	if events > 4 {
		t.Errorf("a rejected ReadOpaqueLP allocated %.1f times per run, want a small constant event count", events)
	}
	r := NewReader(hostile)
	if _, err := r.ReadOpaqueLP(); !errors.Is(err, ErrLengthExceedsMax) {
		t.Errorf("gave %v, want ErrLengthExceedsMax", err)
	}
	if r.Offset() != 0 {
		t.Errorf("left Offset at %d, want 0: a rejected ReadOpaqueLP must not consume input", r.Offset())
	}
}

// TestLengthPastTheInputAllocatesNothing covers the second rejection reason: a length
// that clears the configured maximum and is refused only by the check against the
// bytes remaining. That check is the weaker half of the pair to test, because
// whatever it refuses is by definition no larger than MaxVectorLength, so the most a
// mutant can allocate on this path is one mebibyte per call rather than a gibibyte —
// which means the declared length has to sit at the maximum for the measurement to
// separate at all. It does not do that by accident: at a declared 64 bytes the same
// mutant that fails the case above allocates 64,000 bytes over these runs and passes
// this bound comfortably, measured. Declaring exactly MaxVectorLength instead moves
// the same mutant to 1,048,591,872 bytes, four thousand times the bound.
func TestLengthPastTheInputAllocatesNothing(t *testing.T) {
	// 80100000 is the varint for 1048576, exactly MaxVectorLength, over an input of
	// four bytes: it clears the maximum by equalling it and is refused by the check
	// against the input
	hostile := []byte{0x80, 0x10, 0x00, 0x00}
	grown := measureAllocs(t, func() error {
		r := NewReader(hostile)
		_, err := r.ReadOpaque()
		return err
	}, ErrLengthExceedsInput)
	if grown > allocProbeBudget {
		t.Errorf("%d over declaring ReadOpaque calls allocated %d bytes", allocProbeRuns, grown)
	}
}

// TestHostileSubLengthIsRefusedWithoutBuildingAView is where this file deliberately
// stops asserting an allocation bound, because for the sub readers there is no
// allocation for a hostile length to size. ReadSub and ReadSubLP hand back a three
// index view over the parent's own buffer, so a prefix declaring a gibibyte buys a
// slice header, not a gibibyte. Measured on a mutant that takes the declared length
// straight to subReader with no validation: it does not over-allocate, it panics with
// "slice bounds out of range [::1073741827] with capacity 4" on the first call. A
// byte bound over these two would therefore be one of the assertions that looks
// thorough and cannot fail.
//
// So what is asserted is what the property actually reduces to here: the length is
// refused with the sentinel that names the reason, the cursor is left where it stood
// so a caller that ignores the error is not parked inside a rejected field, and the
// call returns rather than panicking — which is the failure mode a skipped
// validation really produces, and a panic fails this test by taking the binary down.
func TestHostileSubLengthIsRefusedWithoutBuildingAView(t *testing.T) {
	cases := []struct {
		name  string
		input []byte
		read  func(r *Reader) (*Reader, error)
	}{
		{"varint prefix declaring about a gibibyte", []byte{0xbf, 0xff, 0xff, 0xff}, (*Reader).ReadSub},
		{"length prefix declaring four gibibytes", []byte{0xff, 0xff, 0xff, 0xff}, (*Reader).ReadSubLP},
	}
	for _, c := range cases {
		r := NewReader(c.input)
		sub, err := c.read(r)
		if !errors.Is(err, ErrLengthExceedsMax) {
			t.Errorf("%s gave %v, want ErrLengthExceedsMax", c.name, err)
		}
		if sub != nil {
			t.Errorf("%s returned a sub reader over a refused region", c.name)
		}
		if r.Offset() != 0 {
			t.Errorf("%s left Offset at %d, want 0", c.name, r.Offset())
		}
	}
}

// TestHostileVectorLengthIsRefusedBeforeTheItemSlice records that ReadVector inherits
// the sub readers' incapacity rather than the opaque reads' exposure: it reaches its
// region through ReadSub, which refuses the hostile prefix and returns before the
// item slice is ever sized, so on this path there is nothing for the declared length
// to inflate. Measured on the same skipped validation mutant, the call panics inside
// subReader exactly as a direct ReadSub does. What is asserted is therefore the
// refusal itself: the sentinel arrives, no slice comes back, and the parent cursor
// has not moved. The allocation property ReadVector does own is the capacity hint,
// which is the test below.
func TestHostileVectorLengthIsRefusedBeforeTheItemSlice(t *testing.T) {
	hostile := []byte{0xbf, 0xff, 0xff, 0xff}
	r := NewReader(hostile)
	items, err := ReadVector(r, readUint16Item)
	if !errors.Is(err, ErrLengthExceedsMax) {
		t.Errorf("gave %v, want ErrLengthExceedsMax", err)
	}
	if items != nil {
		t.Errorf("returned %d items over a refused region", len(items))
	}
	if r.Offset() != 0 {
		t.Errorf("left Offset at %d, want 0", r.Offset())
	}
}

// TestVectorCapacityHintIsBoundedByAConstant is the allocation property ReadVector
// genuinely owns, and the one assertion in this file that can fail on a plausible
// regression to vector.go rather than to decode.go. The region length is a correct
// upper bound on the element count, since no element can be shorter than one byte,
// which makes it tempting to hint the item slice with it — but it is attacker chosen,
// and the hint is counted in elements, so the amplification is the Go size of the
// element type. Here that is 512 bytes against a one byte encoded element: a 64 KiB
// region the attacker has actually supplied would buy 32 MiB of zero values.
//
// The cap at 64 is what stops it, and this measures that it is still there. The
// element decoder refuses its first element, so the abandoned hint is essentially all
// that gets allocated. Verified against a mutant that hints with the region length
// instead: 33,559,592 bytes on one call, against the 262,144 byte bound a correct
// hint clears at a measured 32,832.
func TestVectorCapacityHintIsBoundedByAConstant(t *testing.T) {
	w := NewWriter()
	w.WriteOpaque(make([]byte, 1<<16))
	input, err := w.Bytes()
	if err != nil {
		t.Fatalf("building the region gave %v", err)
	}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	items, err := ReadVector(NewReader(input), readRefusedWideItem)
	runtime.ReadMemStats(&after)

	if !errors.Is(err, errItemRefused) {
		t.Fatalf("gave %v, want the element decoder's refusal", err)
	}
	if items != nil {
		t.Errorf("returned %d items after a refused element", len(items))
	}
	if grown := after.TotalAlloc - before.TotalAlloc; grown > allocProbeBudget {
		t.Errorf("one refused vector over a %d byte region allocated %d bytes; the capacity hint tracked the declared length", 1<<16, grown)
	}
}
