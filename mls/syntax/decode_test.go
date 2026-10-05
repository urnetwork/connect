// Reader behaviour: big endian integers, copied raw bytes, a cursor that a failed
// read never advances, the full consumption rule, and a limit-taking constructor
// that validates rather than deferring, matching Writer's precedent in encode.go.
package syntax

import (
	"bytes"
	"errors"
	"runtime"
	"testing"
)

// TestReaderIntegersAreBigEndian asserts ReadUint8/16/32/64 each read their value
// most significant byte first, with no padding or alignment between fields, and
// that Done reports no error once every byte has been consumed.
func TestReaderIntegersAreBigEndian(t *testing.T) {
	r := NewReader([]byte{
		0x2a,
		0x01, 0x02,
		0x01, 0x02, 0x03, 0x04,
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
	})
	if v, err := r.ReadUint8(); err != nil || v != 0x2a {
		t.Fatalf("ReadUint8 gave %#x, %v", v, err)
	}
	if v, err := r.ReadUint16(); err != nil || v != 0x0102 {
		t.Fatalf("ReadUint16 gave %#x, %v", v, err)
	}
	if v, err := r.ReadUint32(); err != nil || v != 0x01020304 {
		t.Fatalf("ReadUint32 gave %#x, %v", v, err)
	}
	if v, err := r.ReadUint64(); err != nil || v != 0x0102030405060708 {
		t.Fatalf("ReadUint64 gave %#x, %v", v, err)
	}
	if err := r.Done(); err != nil {
		t.Errorf("Done gave %v, want nil", err)
	}
}

// TestReaderTruncatedReadDoesNotAdvance asserts that when a read has too few bytes
// left to satisfy it, it reports ErrTruncated and leaves Offset unchanged — checked
// across ReadUint8, ReadUint16, ReadUint32, ReadUint64 and ReadRaw, since each
// carries the identical bounds check at a distinct call site.
func TestReaderTruncatedReadDoesNotAdvance(t *testing.T) {
	cases := []struct {
		input []byte
		read  func(*Reader) error
	}{
		{[]byte{}, func(r *Reader) error { _, err := r.ReadUint8(); return err }},
		{[]byte{0x01}, func(r *Reader) error { _, err := r.ReadUint16(); return err }},
		{[]byte{0x01, 0x02, 0x03}, func(r *Reader) error { _, err := r.ReadUint32(); return err }},
		{[]byte{0x01, 0x02, 0x03, 0x04}, func(r *Reader) error { _, err := r.ReadUint64(); return err }},
		{[]byte{0x01}, func(r *Reader) error { _, err := r.ReadRaw(2); return err }},
	}
	for i, c := range cases {
		r := NewReader(c.input)
		if err := c.read(r); !errors.Is(err, ErrTruncated) {
			t.Errorf("case %d gave %v, want ErrTruncated", i, err)
		}
		if r.Offset() != 0 {
			t.Errorf("case %d advanced the cursor to %d on a failed read", i, r.Offset())
		}
	}
}

// TestReaderRawReturnsACopy asserts ReadRaw's result does not alias the input
// slice: mutating the returned bytes must not change the source buffer.
func TestReaderRawReturnsACopy(t *testing.T) {
	input := []byte{0xaa, 0xbb, 0xcc}
	r := NewReader(input)
	out, err := r.ReadRaw(3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out[0] = 0xff
	if input[0] != 0xaa {
		t.Errorf("ReadRaw aliased the input; mutating the result changed the source buffer")
	}
	if !bytes.Equal(out, []byte{0xff, 0xbb, 0xcc}) {
		t.Errorf("copy is %x, want ffbbcc", out)
	}
}

// TestReaderRawRejectsANegativeLength asserts ReadRaw(-1) reports ErrNegativeLength
// rather than panicking or underflowing a slice bound.
func TestReaderRawRejectsANegativeLength(t *testing.T) {
	r := NewReader([]byte{0xaa})
	if _, err := r.ReadRaw(-1); !errors.Is(err, ErrNegativeLength) {
		t.Errorf("ReadRaw(-1) gave %v, want ErrNegativeLength", err)
	}
}

// TestReaderDoneRejectsTrailingBytes asserts Done reports ErrTrailingBytes when
// bytes remain after a decode, and that Remaining and Empty still report the true
// state of the cursor rather than being disturbed by the failed Done call.
func TestReaderDoneRejectsTrailingBytes(t *testing.T) {
	r := NewReader([]byte{0x01, 0x02})
	if _, err := r.ReadUint8(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := r.Done(); !errors.Is(err, ErrTrailingBytes) {
		t.Errorf("Done gave %v, want ErrTrailingBytes", err)
	}
	if r.Remaining() != 1 || r.Empty() {
		t.Errorf("Remaining is %d and Empty is %v, want 1 and false", r.Remaining(), r.Empty())
	}
}

// TestReaderLimitIsCarried asserts NewReader carries the package default
// MaxVectorLength and NewReaderLimit carries whatever limit the caller passes,
// such as MaxRatchetTreeLength.
func TestReaderLimitIsCarried(t *testing.T) {
	if NewReader(nil).MaxVectorLength() != MaxVectorLength {
		t.Errorf("NewReader did not take the default limit")
	}
	if NewReaderLimit(nil, MaxRatchetTreeLength).MaxVectorLength() != MaxRatchetTreeLength {
		t.Errorf("NewReaderLimit did not take the given limit")
	}
}

// TestNewReaderLimitRejectsNegative asserts a negative limit sets ErrNegativeLength
// as a sticky construction time error: every subsequent read reports it
// immediately without consuming input, and Done reports it too. This mirrors the
// precedent set for NewWriterLimit in encode.go — validate at construction, do not
// defer — because ErrNegativeLength's own doc comment claims it fires on exactly
// this misuse, on both halves of the codec. Checked across all five read methods
// (ReadUint8, ReadUint16, ReadUint32, ReadUint64, ReadRaw), since the sticky guard
// sits at a distinct call site in each and any one of them could be missed.
func TestNewReaderLimitRejectsNegative(t *testing.T) {
	r := NewReaderLimit([]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}, -1)
	if _, err := r.ReadUint8(); !errors.Is(err, ErrNegativeLength) {
		t.Errorf("ReadUint8 gave %v, want ErrNegativeLength", err)
	}
	if _, err := r.ReadUint16(); !errors.Is(err, ErrNegativeLength) {
		t.Errorf("ReadUint16 gave %v, want ErrNegativeLength", err)
	}
	if _, err := r.ReadUint32(); !errors.Is(err, ErrNegativeLength) {
		t.Errorf("ReadUint32 gave %v, want ErrNegativeLength", err)
	}
	if _, err := r.ReadUint64(); !errors.Is(err, ErrNegativeLength) {
		t.Errorf("ReadUint64 gave %v, want ErrNegativeLength", err)
	}
	if _, err := r.ReadRaw(1); !errors.Is(err, ErrNegativeLength) {
		t.Errorf("ReadRaw gave %v, want ErrNegativeLength", err)
	}
	if r.Offset() != 0 {
		t.Errorf("Offset is %d, want 0: a read reporting the construction error must not consume input", r.Offset())
	}
	if err := r.Done(); !errors.Is(err, ErrNegativeLength) {
		t.Errorf("Done gave %v, want ErrNegativeLength", err)
	}
}

// TestReaderLatchesAFailureAsSticky asserts that once a read fails, that failure
// is latched into the Reader: a caller that ignores the error and issues a
// second, smaller read against the same still-unconsumed bytes must not silently
// succeed and reinterpret them as a different field. It must instead report the
// same, first error, and Done must report it too rather than treating the
// (partially, wrongly) advanced input as fully consumed.
func TestReaderLatchesAFailureAsSticky(t *testing.T) {
	r := NewReader([]byte{0x01, 0x02})
	if _, err := r.ReadUint32(); !errors.Is(err, ErrTruncated) {
		t.Fatalf("ReadUint32 gave %v, want ErrTruncated", err)
	}
	// The two bytes above are enough to satisfy ReadUint16 on their own: without
	// latching, this call would silently succeed and return 0x0102.
	if v, err := r.ReadUint16(); !errors.Is(err, ErrTruncated) {
		t.Errorf("ReadUint16 after an ignored ErrTruncated gave value %#x, err %v; want the latched ErrTruncated and no value", v, err)
	}
	if r.Offset() != 0 {
		t.Errorf("Offset is %d, want 0: a read blocked by the latched error must not consume input", r.Offset())
	}
	if err := r.Done(); !errors.Is(err, ErrTruncated) {
		t.Errorf("Done gave %v, want the latched ErrTruncated", err)
	}
}

// TestReadOpaqueRoundTripsAndCopies asserts a value ReadOpaque decodes from
// WriteOpaque's own output is byte identical to the original, that Done reports
// full consumption afterward, and that the returned slice does not alias the
// Reader's input: mutating the result must not change the source bytes.
func TestReadOpaqueRoundTripsAndCopies(t *testing.T) {
	body := bytes.Repeat([]byte{0x11}, 100)
	w := NewWriter()
	w.WriteOpaque(body)
	encoded, err := w.Bytes()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r := NewReader(encoded)
	got, err := r.ReadOpaque()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("decoded %x, want %x", got, body)
	}
	if err := r.Done(); err != nil {
		t.Errorf("Done gave %v", err)
	}
	got[0] = 0xff
	if encoded[2] != 0x11 {
		t.Errorf("ReadOpaque aliased the input")
	}
}

// TestReadOpaqueEmptyIsNonNil asserts decoding the single zero length octet gives
// a zero length slice that is not nil: opaque x<V> encodes "zero bytes" but has
// no wire representation for "absent", so ReadOpaque must never hand back nil.
func TestReadOpaqueEmptyIsNonNil(t *testing.T) {
	r := NewReader([]byte{0x00})
	got, err := r.ReadOpaque()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Errorf("empty opaque decoded to nil, want a zero length non nil slice")
	}
	if len(got) != 0 {
		t.Errorf("empty opaque decoded to %x", got)
	}
}

// TestReadOpaqueChecksTheLimitThenTheInput asserts ReadOpaque rejects a declared
// length with the correct, distinguishable sentinel for each way it can be
// invalid: over the configured maximum, over the bytes actually remaining (both
// when the varint claims more input than exists at all, and when it claims more
// than trails the prefix), and a non minimally encoded varint prefix rejected by
// ReadVarint itself before validateLength ever runs. It also pins, for every
// case, that a rejected call leaves Offset at 0: the mark/restore in ReadOpaque
// must undo a validly decoded varint's cursor advance too, not just leave the
// cursor wherever validateLength's own failure found it, matching the
// no-advance-on-failure precedent TestReaderTruncatedReadDoesNotAdvance and
// TestNewReaderLimitRejectsNegative set elsewhere in this file.
func TestReadOpaqueChecksTheLimitThenTheInput(t *testing.T) {
	cases := []struct {
		name    string
		input   []byte
		wantErr error
	}{
		{"length above the limit", []byte{0xbf, 0xff, 0xff, 0xff}, ErrLengthExceedsMax},
		{"length above the remaining input", []byte{0x40, 0x40, 0x11, 0x11}, ErrLengthExceedsInput},
		{"prefix only", []byte{0x05}, ErrLengthExceedsInput},
		{"non minimal prefix", []byte{0x40, 0x00}, ErrVarintNotMinimal},
	}
	for _, c := range cases {
		r := NewReader(c.input)
		if _, err := r.ReadOpaque(); !errors.Is(err, c.wantErr) {
			t.Errorf("%s gave %v, want %v", c.name, err, c.wantErr)
		}
		if r.Offset() != 0 {
			t.Errorf("%s left Offset at %d, want 0: a rejected ReadOpaque must not consume input", c.name, r.Offset())
		}
	}
}

// TestReadOpaqueRejectsAHostileLengthWithoutAllocating is the security property
// the plan calls out by name: a four byte input can declare a varint length up to
// 0x3fffffff (about 1 GiB), far more than MaxVectorLength (1 MiB) and far more
// than the four bytes actually present. It makes two distinct assertions, each
// catching a different regression:
//
//   - testing.AllocsPerRun counts allocation *events*, not bytes. It reliably
//     catches a loop based over-allocation (repeated append growth costs many
//     events), but a single `out := make([]byte, length)` moved above validation
//     costs exactly one extra event — indistinguishable from noise against a
//     generous bound — so this assertion alone would not catch that regression.
//   - runtime.MemStats.TotalAlloc measures bytes actually allocated, which is
//     what the security property is really about: if ReadOpaque ever allocated
//     the declared length before validating it, this would jump from a few
//     hundred bytes to roughly a gigabyte on a single call, which the 4096 byte
//     bound below catches deterministically — no reliance on an OOM or a timeout
//     to notice.
func TestReadOpaqueRejectsAHostileLengthWithoutAllocating(t *testing.T) {
	input := []byte{0xbf, 0xff, 0xff, 0xff} // declares length 0x3fffffff, ~1 GiB
	if _, err := NewReader(input).ReadOpaque(); !errors.Is(err, ErrLengthExceedsMax) {
		t.Fatalf("got %v, want ErrLengthExceedsMax", err)
	}

	allocs := testing.AllocsPerRun(200, func() {
		r := NewReader(input)
		_, _ = r.ReadOpaque()
	})
	if allocs > 4 {
		t.Errorf("ReadOpaque allocated %.1f times per run rejecting a hostile length, want a small constant event count", allocs)
	}

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, _ = NewReader(input).ReadOpaque()
	runtime.ReadMemStats(&after)
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 4096 {
		t.Errorf("ReadOpaque allocated %d bytes rejecting a hostile length, want well under the declared ~1 GiB", grew)
	}
}

// TestOpaqueRoundTripsAcrossVarintWidthBoundaries asserts that at every length
// where the varint prefix's width changes — 0, 1, 63, 64, 16383, 16384 — a value
// WriteOpaque emits decodes back through ReadOpaque byte identical to the
// original, with zero the boundary case people forget: an empty opaque field is
// not a special case in the encoding, only in whether the result is nil.
func TestOpaqueRoundTripsAcrossVarintWidthBoundaries(t *testing.T) {
	for _, length := range []int{0, 1, 63, 64, 16383, 16384} {
		body := bytes.Repeat([]byte{0x11}, length)
		w := NewWriter()
		w.WriteOpaque(body)
		encoded, err := w.Bytes()
		if err != nil {
			t.Fatalf("length %d: unexpected error %v", length, err)
		}
		r := NewReader(encoded)
		got, err := r.ReadOpaque()
		if err != nil {
			t.Fatalf("length %d: unexpected error %v", length, err)
		}
		if !bytes.Equal(got, body) {
			t.Errorf("length %d: decoded %x, want %x", length, got, body)
		}
		if got == nil {
			t.Errorf("length %d: decoded to nil, want a non nil slice", length)
		}
		if err := r.Done(); err != nil {
			t.Errorf("length %d: Done gave %v", length, err)
		}
	}
}

// TestOpaqueRoundTripsTheEmptyStringFromANilWrite asserts the specific boundary
// the brief calls out by name: writing a nil slice through WriteOpaque and
// reading it back through ReadOpaque must round trip to a zero length, non nil
// slice, not to nil and not to an error.
func TestOpaqueRoundTripsTheEmptyStringFromANilWrite(t *testing.T) {
	w := NewWriter()
	w.WriteOpaque(nil)
	encoded, err := w.Bytes()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r := NewReader(encoded)
	got, err := r.ReadOpaque()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("decoded %#v, want a zero length non nil slice", got)
	}
	if err := r.Done(); err != nil {
		t.Errorf("Done gave %v", err)
	}
}

// TestReadOpaqueLPRoundTrips asserts a body past the 16 bit boundary — 70000
// bytes, which no two octet prefix could carry — survives WriteOpaqueLP and
// ReadOpaqueLP unchanged and consumes the input exactly, so the record layer's
// fields are not silently capped at 65535 bytes.
func TestReadOpaqueLPRoundTrips(t *testing.T) {
	body := bytes.Repeat([]byte{0x11}, 70000)
	w := NewWriter()
	w.WriteOpaqueLP(body)
	encoded, err := w.Bytes()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r := NewReader(encoded)
	got, err := r.ReadOpaqueLP()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("decoded %d bytes, want %d", len(got), len(body))
	}
	if err := r.Done(); err != nil {
		t.Errorf("Done gave %v", err)
	}
}

// TestReadOpaqueLPChecksTheLimitThenTheInput asserts each way an LP prefix can be
// invalid surfaces its own sentinel — over the configured maximum, over the bytes
// actually remaining, and a prefix the input ends partway through — and that
// every rejection leaves Offset at 0, so the four octets a rejected read consumed
// to see the length are given back rather than left half consumed for whatever
// reads next.
func TestReadOpaqueLPChecksTheLimitThenTheInput(t *testing.T) {
	cases := []struct {
		name    string
		input   []byte
		wantErr error
	}{
		{"length above the limit", []byte{0xff, 0xff, 0xff, 0xff}, ErrLengthExceedsMax},
		{"length above the remaining input", []byte{0x00, 0x00, 0x00, 0x40, 0x11}, ErrLengthExceedsInput},
		{"prefix truncated", []byte{0x00, 0x00, 0x00}, ErrTruncated},
	}
	for _, c := range cases {
		r := NewReader(c.input)
		if _, err := r.ReadOpaqueLP(); !errors.Is(err, c.wantErr) {
			t.Errorf("%s gave %v, want %v", c.name, err, c.wantErr)
		}
		if r.Offset() != 0 {
			t.Errorf("%s advanced the cursor to %d on a failed read", c.name, r.Offset())
		}
	}
}

// TestReadOpaqueLPLatchesOnFailure defends the property that a rejected LP read
// latches its failure into the Reader, and it exists because nothing else in this
// file can tell a latching implementation from a non latching one. The plan's own
// sample for this method returns bare sentinels — return nil, ErrTruncated — with
// no setErr, and every other test here passes against that version: the round
// trip never fails, and the rejection cases only check the returned error and the
// cursor, both of which the bare sentinel version gets right. What it gets wrong
// is everything after. The input below declares 64 bytes and supplies one, so the
// read fails with the cursor correctly restored to 0 — and then, unlatched, the
// very same four prefix octets are still sitting there for the next read to
// reinterpret: ReadUint32 returns 0x40 with a nil error, a structurally valid
// decode of an entirely different field, and Done then reports ErrTrailingBytes,
// which masks the real failure by describing the leftover body byte instead. That
// is the Task 4 vulnerability reproduced in a new method, in a codec whose
// serialized forms MLS signs over, and round trip tests cannot see it. Do not
// simplify this method back to the plan sample; this test is the reason it
// differs.
func TestReadOpaqueLPLatchesOnFailure(t *testing.T) {
	input := []byte{0x00, 0x00, 0x00, 0x40, 0x11} // declares 64, only 1 byte follows
	r := NewReader(input)
	if _, err := r.ReadOpaqueLP(); err == nil {
		t.Fatalf("expected the read to fail")
	}
	if v, err := r.ReadUint32(); err == nil {
		t.Errorf("after a failed read, ReadUint32 returned %#x with nil error", v)
	}
	if err := r.Done(); err == nil || errors.Is(err, ErrTrailingBytes) {
		t.Errorf("Done reported %v, masking the real failure", err)
	}
}

// TestReadOpaqueLPRefusesALatchedReader defends the other half of the sticky
// contract: the entry guard that makes a read on an already failed Reader report
// that first error rather than running its own bounds check. The plan's sample
// has no such guard, so on a Reader constructed with a negative limit — which
// NewReaderLimit latches as ErrNegativeLength before any read — it would proceed
// to compare the declared length against that negative maximum and report
// ErrLengthExceedsMax instead, burying the construction time misuse under a
// downstream symptom of it and breaking first error wins. The input here is a
// well formed single byte value, so only the missing guard can make this fail.
func TestReadOpaqueLPRefusesALatchedReader(t *testing.T) {
	r := NewReaderLimit([]byte{0x00, 0x00, 0x00, 0x01, 0xaa}, -1) // latched ErrNegativeLength
	if _, err := r.ReadOpaqueLP(); !errors.Is(err, ErrNegativeLength) {
		t.Errorf("latched Reader gave %v, want ErrNegativeLength", err)
	}
}

// TestReadSubIsBoundedAndAdvancesTheParent asserts the two halves of the
// sub-reader contract at once: the view sees exactly the declared region and
// refuses a read that would reach past it, and the parent is left positioned
// after the whole region rather than wherever the sub-reader stopped, so the
// field that follows the nested structure decodes correctly.
func TestReadSubIsBoundedAndAdvancesTheParent(t *testing.T) {
	w := NewWriter()
	w.WriteOpaque([]byte{0xaa, 0xbb, 0xcc})
	w.WriteUint16(0xbeef)
	encoded, err := w.Bytes()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r := NewReader(encoded)
	sub, err := r.ReadSub()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sub.Remaining() != 3 {
		t.Fatalf("sub reader sees %d bytes, want 3", sub.Remaining())
	}
	if _, err := sub.ReadRaw(4); !errors.Is(err, ErrTruncated) {
		t.Errorf("sub reader read past its region")
	}
	if v, err := r.ReadUint16(); err != nil || v != 0xbeef {
		t.Errorf("parent gave %#x, %v after ReadSub; want beef, nil", v, err)
	}
	if err := r.Done(); err != nil {
		t.Errorf("Done gave %v", err)
	}
}

// TestReadSubInheritsTheLimit asserts the raised ratchet tree limit survives the
// step into a nested structure. A sub-reader that silently fell back to the
// package default would reject a legitimate tree field with ErrLengthExceedsMax
// at whatever depth the tree happens to be nested, which is a decode failure that
// only shows up on large groups.
func TestReadSubInheritsTheLimit(t *testing.T) {
	w := NewWriterLimit(MaxRatchetTreeLength)
	w.WriteOpaque(bytes.Repeat([]byte{0x11}, 8))
	encoded, err := w.Bytes()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r := NewReaderLimit(encoded, MaxRatchetTreeLength)
	sub, err := r.ReadSub()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sub.MaxVectorLength() != MaxRatchetTreeLength {
		t.Errorf("sub reader limit is %d, want the parent's %d", sub.MaxVectorLength(), MaxRatchetTreeLength)
	}
}

// TestReadSubLPIsBounded asserts the record layer's fixed width prefix produces
// the same bounded view and the same parent advance as the varint form, since
// connect/message nests structures inside LP fields exactly as MLS nests them
// inside opaque ones.
func TestReadSubLPIsBounded(t *testing.T) {
	w := NewWriter()
	w.WriteOpaqueLP([]byte{0xaa, 0xbb})
	w.WriteUint8(0x77)
	encoded, err := w.Bytes()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r := NewReader(encoded)
	sub, err := r.ReadSubLP()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sub.Remaining() != 2 {
		t.Errorf("sub reader sees %d bytes, want 2", sub.Remaining())
	}
	if v, err := r.ReadUint8(); err != nil || v != 0x77 {
		t.Errorf("parent gave %#x, %v; want 77, nil", v, err)
	}
}

// TestReadSubCannotGrowIntoTheParent asserts the three index slice: a sub reader
// is a view rather than a copy, so without the capacity clip an append inside it
// would write into the parent's own backing array, corrupting bytes the parent
// has not read yet.
func TestReadSubCannotGrowIntoTheParent(t *testing.T) {
	r := NewReader([]byte{0x01, 0xaa, 0xbb, 0xcc})
	sub, err := r.ReadSub()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	grown := append(sub.bs, 0xff)
	if len(r.bs) < 4 || r.bs[2] != 0xbb {
		t.Errorf("appending to the sub reader's slice overwrote the parent's bytes: %x", r.bs)
	}
	if grown[1] != 0xff {
		t.Errorf("append did not produce the expected value")
	}
}

// TestReadSubChecksTheLimitThenTheInput asserts a rejected region surfaces the
// same distinguishable sentinel ReadOpaque would give for the same bytes, and
// that Offset is back at 0 afterward: the mark and restore must undo a validly
// decoded varint prefix's advance, or the caller that ignores the error leaves
// the cursor parked inside a region that was never accepted.
func TestReadSubChecksTheLimitThenTheInput(t *testing.T) {
	cases := []struct {
		name    string
		input   []byte
		wantErr error
	}{
		{name: "length above the limit", input: []byte{0xbf, 0xff, 0xff, 0xff}, wantErr: ErrLengthExceedsMax},
		{name: "length above the remaining input", input: []byte{0x40, 0x40, 0x11, 0x11}, wantErr: ErrLengthExceedsInput},
		{name: "prefix only", input: []byte{0x05}, wantErr: ErrLengthExceedsInput},
		{name: "non minimal prefix", input: []byte{0x40, 0x00}, wantErr: ErrVarintNotMinimal},
	}
	for _, c := range cases {
		r := NewReader(c.input)
		if _, err := r.ReadSub(); !errors.Is(err, c.wantErr) {
			t.Errorf("%s gave %v, want %v", c.name, err, c.wantErr)
		}
		if r.Offset() != 0 {
			t.Errorf("%s left Offset at %d, want 0: a rejected ReadSub must not consume input", c.name, r.Offset())
		}
	}
}

// TestReadSubLPChecksTheLimitThenTheInput is the fixed width counterpart, and the
// cursor assertion matters more here: the four prefix octets are consumed before
// the length can be validated at all, so a rejected read that skipped the restore
// would hand the next read four bytes of body as if they were a new field.
func TestReadSubLPChecksTheLimitThenTheInput(t *testing.T) {
	cases := []struct {
		name    string
		input   []byte
		wantErr error
	}{
		{name: "length above the limit", input: []byte{0xff, 0xff, 0xff, 0xff}, wantErr: ErrLengthExceedsMax},
		{name: "length above the remaining input", input: []byte{0x00, 0x00, 0x00, 0x40, 0x11}, wantErr: ErrLengthExceedsInput},
		{name: "prefix truncated", input: []byte{0x00, 0x00, 0x00}, wantErr: ErrTruncated},
	}
	for _, c := range cases {
		r := NewReader(c.input)
		if _, err := r.ReadSubLP(); !errors.Is(err, c.wantErr) {
			t.Errorf("%s gave %v, want %v", c.name, err, c.wantErr)
		}
		if r.Offset() != 0 {
			t.Errorf("%s advanced the cursor to %d on a failed read", c.name, r.Offset())
		}
	}
}

// TestReadSubLatchesOnFailure defends the sticky contract on the varint form. The
// input declares a 64 byte region and supplies two bytes, so the read fails with
// the cursor correctly restored to 0 — and the four octets it refused are then
// still sitting there for the next read to reinterpret. Unlatched, ReadUint32
// returns 0x40401111 with a nil error, a structurally valid decode of an entirely
// different field, and Done then reports ErrTrailingBytes, describing leftovers
// instead of the real failure. Round trip tests cannot see any of that.
func TestReadSubLatchesOnFailure(t *testing.T) {
	input := []byte{0x40, 0x40, 0x11, 0x11} // declares 64, only 2 bytes follow
	r := NewReader(input)
	if _, err := r.ReadSub(); !errors.Is(err, ErrLengthExceedsInput) {
		t.Fatalf("ReadSub gave %v, want ErrLengthExceedsInput", err)
	}
	if v, err := r.ReadUint32(); err == nil {
		t.Errorf("after a failed read, ReadUint32 returned %#x with nil error", v)
	}
	if err := r.Done(); err == nil || errors.Is(err, ErrTrailingBytes) {
		t.Errorf("Done reported %v, masking the real failure", err)
	}
}

// TestReadSubLPLatchesOnFailure is the same defence for the fixed width form, and
// it is the one the plan's own sample fails: that sample open codes the two
// length comparisons and returns bare sentinels, so nothing latches. The input
// declares 64 bytes and supplies one; on the unlatched version the following
// ReadUint32 returns 0x40 with a nil error and Done reports ErrTrailingBytes.
func TestReadSubLPLatchesOnFailure(t *testing.T) {
	input := []byte{0x00, 0x00, 0x00, 0x40, 0x11} // declares 64, only 1 byte follows
	r := NewReader(input)
	if _, err := r.ReadSubLP(); !errors.Is(err, ErrLengthExceedsInput) {
		t.Fatalf("ReadSubLP gave %v, want ErrLengthExceedsInput", err)
	}
	if v, err := r.ReadUint32(); err == nil {
		t.Errorf("after a failed read, ReadUint32 returned %#x with nil error", v)
	}
	if err := r.Done(); err == nil || errors.Is(err, ErrTrailingBytes) {
		t.Errorf("Done reported %v, masking the real failure", err)
	}
}

// TestReadSubLPRefusesALatchedReader defends the entry guard. The plan's sample
// has none, so on a Reader that NewReaderLimit already latched with
// ErrNegativeLength it would compare the declared length against that negative
// maximum and report ErrLengthExceedsMax instead, burying the construction time
// misuse under a downstream symptom of it and breaking first error wins. The
// input is a well formed single byte region, so only the missing guard can make
// this fail.
func TestReadSubLPRefusesALatchedReader(t *testing.T) {
	r := NewReaderLimit([]byte{0x00, 0x00, 0x00, 0x01, 0xaa}, -1) // latched ErrNegativeLength
	if _, err := r.ReadSubLP(); !errors.Is(err, ErrNegativeLength) {
		t.Errorf("latched Reader gave %v, want ErrNegativeLength", err)
	}
}

// TestReadSubRefusesALatchedReader pins the same contract on the varint form.
// Unlike its LP sibling this one cannot distinguish a present entry guard from an
// absent one, because ReadVarint carries its own guard and runs first, so the
// error surfaces either way; it is here so the contract is asserted rather than
// inferred from a call it happens to delegate to today.
func TestReadSubRefusesALatchedReader(t *testing.T) {
	r := NewReaderLimit([]byte{0x01, 0xaa}, -1) // latched ErrNegativeLength
	if _, err := r.ReadSub(); !errors.Is(err, ErrNegativeLength) {
		t.Errorf("latched Reader gave %v, want ErrNegativeLength", err)
	}
}

// TestSubReaderFailureDoesNotLatchOntoTheParent asserts the isolation that makes
// the unconditional parent advance safe. A malformed nested structure must latch
// on the sub reader alone: the parent already skipped the whole region, so it is
// still positioned on a field boundary and must stay usable, letting a caller
// decide for itself whether a bad nested structure is fatal or a field it can
// ignore. If the failure propagated to the parent instead, one unparseable
// extension body would take the rest of the message down with it.
func TestSubReaderFailureDoesNotLatchOntoTheParent(t *testing.T) {
	w := NewWriter()
	w.WriteOpaque([]byte{0xaa, 0xbb})
	w.WriteUint16(0xbeef)
	encoded, err := w.Bytes()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r := NewReader(encoded)
	sub, err := r.ReadSub()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := sub.ReadUint32(); !errors.Is(err, ErrTruncated) {
		t.Fatalf("sub reader gave %v, want ErrTruncated", err)
	}
	if err := sub.Done(); !errors.Is(err, ErrTruncated) {
		t.Errorf("sub reader Done gave %v, want the latched ErrTruncated", err)
	}
	if v, err := r.ReadUint16(); err != nil || v != 0xbeef {
		t.Errorf("parent gave %#x, %v after the sub reader failed; want beef, nil", v, err)
	}
	if err := r.Done(); err != nil {
		t.Errorf("parent Done gave %v, want nil: a sub reader's failure must not latch onto the parent", err)
	}
}

// TestUnderConsumingSubReaderIsCaughtOnlyByDone pins the one place this design
// leaves open, so the next reader of this file finds it asserted rather than
// discovers it. The parent skips the whole region however little of it the sub
// reader consumes, which is what keeps the parent synchronised — but it also
// means bytes left over inside the region are invisible to the parent: its own
// Done reports success. Only the sub reader's Done can see them, and the caller
// is what obliges the codec to ask. Two encodings of one object would otherwise
// both be accepted, in a codec whose serialized forms MLS signs over, so
// ReadSub's doc comment requires the call and this test shows what it catches.
func TestUnderConsumingSubReaderIsCaughtOnlyByDone(t *testing.T) {
	w := NewWriter()
	w.WriteOpaque([]byte{0xaa, 0xbb, 0xcc})
	w.WriteUint16(0xbeef)
	encoded, err := w.Bytes()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r := NewReader(encoded)
	sub, err := r.ReadSub()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v, err := sub.ReadUint8(); err != nil || v != 0xaa {
		t.Fatalf("sub reader gave %#x, %v; want aa, nil", v, err)
	}
	if err := sub.Done(); !errors.Is(err, ErrTrailingBytes) {
		t.Errorf("sub reader Done gave %v, want ErrTrailingBytes for the 2 bytes it left", err)
	}
	if v, err := r.ReadUint16(); err != nil || v != 0xbeef {
		t.Errorf("parent gave %#x, %v; want beef, nil", v, err)
	}
	if err := r.Done(); err != nil {
		t.Errorf("parent Done gave %v, want nil: the parent cannot see inside the region, which is why the caller must call the sub reader's Done", err)
	}
}

// nestedForms drives every ReadNested case over both prefixes. The varint form and
// the fixed 32 bit form differ only in how the region is framed and carry the same
// contract, so each property below is asserted against both rather than against
// the varint one with the record layer's form left to inference.
var nestedForms = []struct {
	name   string
	encode func(w *Writer, body []byte)
	nested func(r *Reader, decodeOne func(r *Reader) error) error
}{
	{
		name:   "varint",
		encode: func(w *Writer, body []byte) { w.WriteOpaque(body) },
		nested: func(r *Reader, decodeOne func(r *Reader) error) error { return r.ReadNested(decodeOne) },
	},
	{
		name:   "lp",
		encode: func(w *Writer, body []byte) { w.WriteOpaqueLP(body) },
		nested: func(r *Reader, decodeOne func(r *Reader) error) error { return r.ReadNestedLP(decodeOne) },
	},
}

// errNestedRefusal stands in for a semantic refusal from a nested decoder: every
// read it made succeeded, and it declined the structure anyway. That is the one
// failure no read of its own can latch, so it is what the latching contract has to
// be measured against.
var errNestedRefusal = errors.New("mls syntax: nested probe refuses to decode")

// TestReadNestedRunsTheRegionToEmpty is the positive case: a decoder that consumes
// exactly its region succeeds, the parent is left on the next field boundary, and
// the parent's own Done reports success. Without this the failure cases below could
// all be satisfied by a method that never succeeded at all.
func TestReadNestedRunsTheRegionToEmpty(t *testing.T) {
	for _, form := range nestedForms {
		w := NewWriter()
		form.encode(w, []byte{0x01, 0x02})
		w.WriteUint16(0xbeef)
		encoded, err := w.Bytes()
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", form.name, err)
		}
		r := NewReader(encoded)
		var got uint16
		err = form.nested(r, func(sub *Reader) error {
			v, err := sub.ReadUint16()
			got = v
			return err
		})
		if err != nil {
			t.Errorf("%s gave %v, want nil", form.name, err)
		}
		if got != 0x0102 {
			t.Errorf("%s decoded %#x, want 0x102", form.name, got)
		}
		if v, err := r.ReadUint16(); err != nil || v != 0xbeef {
			t.Errorf("%s: parent gave %#x, %v after the region; want beef, nil", form.name, v, err)
		}
		if err := r.Done(); err != nil {
			t.Errorf("%s: parent Done gave %v, want nil", form.name, err)
		}
	}
}

// TestReadNestedRejectsBytesLeftInTheRegion is the whole reason this method exists
// on top of ReadSub. The region carries four bytes and the decoder reads two and
// reports success, which ReadSub would accept silently — the parent has already
// skipped the whole region, so its own Done sees nothing wrong inside it, and two
// encodings of one object would both be accepted in a codec whose serialized forms
// MLS signs over. A version that hands the Done obligation back to the caller
// returns nil here. The parent read afterwards is what separates latching from
// merely returning: unlatched, it hands back beef with a nil error, so a caller who
// drops the return carries on against a structure that was never accepted.
func TestReadNestedRejectsBytesLeftInTheRegion(t *testing.T) {
	for _, form := range nestedForms {
		w := NewWriter()
		form.encode(w, []byte{0x01, 0x02, 0x03, 0x04})
		w.WriteUint16(0xbeef)
		encoded, err := w.Bytes()
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", form.name, err)
		}
		r := NewReader(encoded)
		err = form.nested(r, func(sub *Reader) error {
			_, err := sub.ReadUint16()
			return err
		})
		if !errors.Is(err, ErrTrailingBytes) {
			t.Errorf("%s gave %v for the 2 bytes the decoder left, want ErrTrailingBytes", form.name, err)
		}
		if v, err := r.ReadUint16(); err == nil {
			t.Errorf("%s: parent returned %#x with a nil error after an unfinished region; the failure did not latch", form.name, v)
		}
		if err := r.Done(); !errors.Is(err, ErrTrailingBytes) {
			t.Errorf("%s: parent Done gave %v, want the latched ErrTrailingBytes", form.name, err)
		}
	}
}

// TestReadNestedLatchesADecoderRefusal covers the failure that latches nowhere on
// its own: the decoder consumed its region exactly and refused on semantic grounds,
// so neither the sub reader nor the parent carries anything. Returning alone would
// leave a caller who drops the return holding a clean parent positioned at the next
// field, having skipped a region it never accepted — which is what the parent read
// here measures, since unlatched it yields beef with a nil error.
func TestReadNestedLatchesADecoderRefusal(t *testing.T) {
	for _, form := range nestedForms {
		w := NewWriter()
		form.encode(w, []byte{0x01, 0x02})
		w.WriteUint16(0xbeef)
		encoded, err := w.Bytes()
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", form.name, err)
		}
		r := NewReader(encoded)
		err = form.nested(r, func(sub *Reader) error {
			if _, err := sub.ReadUint16(); err != nil {
				return err
			}
			return errNestedRefusal
		})
		if !errors.Is(err, errNestedRefusal) {
			t.Errorf("%s gave %v, want the decoder's refusal", form.name, err)
		}
		if v, err := r.ReadUint16(); err == nil {
			t.Errorf("%s: parent returned %#x with a nil error after a refused region; the refusal did not latch", form.name, v)
		}
		if err := r.Done(); !errors.Is(err, errNestedRefusal) {
			t.Errorf("%s: parent Done gave %v, want the latched refusal", form.name, err)
		}
	}
}

// TestReadNestedCatchesASwallowedReadFailure is the case the loop-to-empty
// reasoning alone does not cover, and it is why the Done fold is not redundant with
// consuming the region. The decoder asks for four bytes from a two byte region,
// ignores the failure and reports success. That read latched on the sub reader,
// which nothing else would ever look at, so the structure would be built from bytes
// that were never there. Done on the sub reader reports the latched ErrTruncated
// rather than ErrTrailingBytes, which is how this stays distinguishable from the
// leftover byte case above.
func TestReadNestedCatchesASwallowedReadFailure(t *testing.T) {
	for _, form := range nestedForms {
		w := NewWriter()
		form.encode(w, []byte{0x01, 0x02})
		w.WriteUint16(0xbeef)
		encoded, err := w.Bytes()
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", form.name, err)
		}
		r := NewReader(encoded)
		err = form.nested(r, func(sub *Reader) error {
			sub.ReadUint32() // deliberately ignored, as a careless decoder would
			return nil
		})
		if !errors.Is(err, ErrTruncated) {
			t.Errorf("%s gave %v for a swallowed read failure, want ErrTruncated", form.name, err)
		}
		if v, err := r.ReadUint16(); err == nil {
			t.Errorf("%s: parent returned %#x with a nil error; the swallowed failure did not latch", form.name, v)
		}
	}
}

// TestReadNestedRefusesALatchedReader pins the entry guard, which is delegated to
// the sub reader pair rather than written twice: a Reader that NewReaderLimit
// latched at construction must report that misuse and must not run the nested
// decoder against a limit it already refused.
func TestReadNestedRefusesALatchedReader(t *testing.T) {
	for _, form := range nestedForms {
		w := NewWriter()
		form.encode(w, []byte{0xaa, 0xbb})
		encoded, err := w.Bytes()
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", form.name, err)
		}
		r := NewReaderLimit(encoded, -1) // latched ErrNegativeLength
		err = form.nested(r, func(sub *Reader) error {
			t.Errorf("%s: the nested decoder ran on a latched Reader", form.name)
			return nil
		})
		if !errors.Is(err, ErrNegativeLength) {
			t.Errorf("%s: latched Reader gave %v, want ErrNegativeLength", form.name, err)
		}
	}
}
