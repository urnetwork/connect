// The RFC 9420 section 2.1.2 varint, both directions. The encoder emits exactly one
// form per value and the decoder accepts exactly that form, because a decoder that
// accepts a second encoding of one length turns a signed structure into a malleable
// one.
package syntax

import (
	"bytes"
	"errors"
	"testing"
)

// the boundary of every prefix width, from rfc 9420 section 2.1.2
var varintBoundaries = []struct {
	value   uint32
	encoded []byte
}{
	{0, []byte{0x00}},
	{13, []byte{0x0d}},
	{54, []byte{0x36}},
	{63, []byte{0x3f}},
	{64, []byte{0x40, 0x40}},
	{389, []byte{0x41, 0x85}},
	{2730, []byte{0x4a, 0xaa}},
	{4095, []byte{0x4f, 0xff}},
	{16383, []byte{0x7f, 0xff}},
	{16384, []byte{0x80, 0x00, 0x40, 0x00}},
	{48879, []byte{0x80, 0x00, 0xbe, 0xef}},
	{57005, []byte{0x80, 0x00, 0xde, 0xad}},
	{1073741823, []byte{0xbf, 0xff, 0xff, 0xff}},
}

// TestWriteVarintIsMinimal asserts WriteVarint emits exactly the minimal encoding
// at every prefix width boundary: the widest value still using the narrower form
// immediately beside the narrowest value that needs the wider one, on both sides
// of every transition, plus 0 and MaxVarint at the extremes. Bytes are checked
// against concrete literals, not merely for the absence of an error.
func TestWriteVarintIsMinimal(t *testing.T) {
	for _, c := range varintBoundaries {
		w := NewWriter()
		w.WriteVarint(c.value)
		out, err := w.Bytes()
		if err != nil {
			t.Errorf("value %d: unexpected error %v", c.value, err)
			continue
		}
		if !bytes.Equal(out, c.encoded) {
			t.Errorf("value %d encoded to %x, want %x", c.value, out, c.encoded)
		}
	}
}

// TestWriteVarintRejectsValuesAboveTheRange asserts that any value past MaxVarint —
// starting at MaxVarint+1, the first rejected value — sets the sticky
// ErrVarintOverflow and appends zero bytes, never a partial write of the value it
// refused to encode.
func TestWriteVarintRejectsValuesAboveTheRange(t *testing.T) {
	for _, v := range []uint32{MaxVarint + 1, 0x40000000, 0x7fffffff, 0xffffffff} {
		w := NewWriter()
		w.WriteVarint(v)
		out, err := w.Bytes()
		if !errors.Is(err, ErrVarintOverflow) {
			t.Errorf("value %d gave %v, want ErrVarintOverflow", v, err)
		}
		if out != nil {
			t.Errorf("value %d: Bytes returned %x alongside an error, want nil", v, out)
		}
		if w.Len() != 0 {
			t.Errorf("value %d: Len is %d, want 0: an overflow must append nothing", v, w.Len())
		}
	}
}

// TestWriteVarintOverflowDoesNotOverwriteAnEarlierError asserts that when a Writer
// already carries a sticky error, calling WriteVarint with a value past MaxVarint
// does not replace it with ErrVarintOverflow: first error wins, so the reported
// cause stays the original one, not whichever no op write happened to run last.
func TestWriteVarintOverflowDoesNotOverwriteAnEarlierError(t *testing.T) {
	w := NewWriter()
	w.setErr(ErrLengthExceedsMax)
	w.WriteVarint(MaxVarint + 1)
	if w.Len() != 0 {
		t.Errorf("Len is %d, want 0: a write after an error must append nothing", w.Len())
	}
	if !errors.Is(w.Err(), ErrLengthExceedsMax) {
		t.Errorf("Err is %v, want the original ErrLengthExceedsMax, not ErrVarintOverflow", w.Err())
	}
	if errors.Is(w.Err(), ErrVarintOverflow) {
		t.Errorf("Err is %v: the overflowing WriteVarint call overwrote the earlier error", w.Err())
	}
}

// TestReadVarintAcceptsTheMinimalForm asserts ReadVarint decodes every golden
// encoding in varintBoundaries — the same table WriteVarint's minimality test
// uses, reused rather than duplicated so the two directions cannot drift apart —
// back to its original value, and that nothing is left unconsumed afterward.
func TestReadVarintAcceptsTheMinimalForm(t *testing.T) {
	for _, c := range varintBoundaries {
		r := NewReader(c.encoded)
		got, err := r.ReadVarint()
		if err != nil {
			t.Fatalf("%x: unexpected error %v", c.encoded, err)
		}
		if got != c.value {
			t.Errorf("%x decoded to %d, want %d", c.encoded, got, c.value)
		}
		if err := r.Done(); err != nil {
			t.Errorf("%x left %d bytes unconsumed", c.encoded, r.Remaining())
		}
	}
}

// TestReadVarintRejectsEverythingButTheMinimalForm asserts each of the three
// rejection sentinels fires on exactly the input shape it owns and never on
// another: a shorter valid encoding exists (ErrVarintNotMinimal), the prefix is
// the reserved 0b11 (ErrVarintReserved), or fewer octets remain than the prefix
// promises (ErrTruncated) — and that every rejection leaves the cursor at 0.
// deserialization.json carries only well formed headers, so the rejection half of
// rule 1 is ours to write. Every row here is a length that has a shorter encoding,
// a reserved prefix, or no body at all.
func TestReadVarintRejectsEverythingButTheMinimalForm(t *testing.T) {
	cases := []struct {
		name    string
		input   []byte
		wantErr error
	}{
		{"two octet form carrying zero", []byte{0x40, 0x00}, ErrVarintNotMinimal},
		{"two octet form carrying 63", []byte{0x40, 0x3f}, ErrVarintNotMinimal},
		{"four octet form carrying zero", []byte{0x80, 0x00, 0x00, 0x00}, ErrVarintNotMinimal},
		{"four octet form carrying 63", []byte{0x80, 0x00, 0x00, 0x3f}, ErrVarintNotMinimal},
		{"four octet form carrying 16383", []byte{0x80, 0x00, 0x3f, 0xff}, ErrVarintNotMinimal},
		{"reserved prefix, low", []byte{0xc0}, ErrVarintReserved},
		{"reserved prefix, high", []byte{0xff, 0xff, 0xff, 0xff}, ErrVarintReserved},
		{"empty input", []byte{}, ErrTruncated},
		{"two octet form missing its second octet", []byte{0x40}, ErrTruncated},
		{"four octet form missing its last octet", []byte{0x80, 0x00, 0x40}, ErrTruncated},
	}
	for _, c := range cases {
		r := NewReader(c.input)
		got, err := r.ReadVarint()
		if !errors.Is(err, c.wantErr) {
			t.Errorf("%s (%x) gave %d, %v; want %v", c.name, c.input, got, err, c.wantErr)
		}
		if r.Offset() != 0 {
			t.Errorf("%s advanced the cursor to %d on a failed read", c.name, r.Offset())
		}
	}
}

// TestReadVarintConsumesOnlyItsOwnOctets asserts ReadVarint stops exactly at the
// end of its own encoding and leaves whatever follows untouched, so a varint
// composes with a subsequent read instead of swallowing or starving it — a
// regression guard: the width is fixed by the prefix bits alone, so ReadVarint has
// no path that could read the trailing bytes even if this test were absent.
func TestReadVarintConsumesOnlyItsOwnOctets(t *testing.T) {
	r := NewReader([]byte{0x40, 0x40, 0xde, 0xad})
	v, err := r.ReadVarint()
	if err != nil || v != 64 {
		t.Fatalf("ReadVarint gave %d, %v; want 64, nil", v, err)
	}
	if r.Offset() != 2 {
		t.Fatalf("consumed %d octets, want 2", r.Offset())
	}
	rest, err := r.ReadRaw(2)
	if err != nil || !bytes.Equal(rest, []byte{0xde, 0xad}) {
		t.Errorf("tail is %x, %v; want dead, nil", rest, err)
	}
}

// TestReadVarintFailureLatchesTheStickyError asserts that after each of the three
// rejection paths, the Reader's error is sticky: a second ReadVarint call on the
// same Reader reports the identical error instead of re-deriving it (or a
// different one) from bytes the first call never validated, matching every other
// Reader method's first-error-wins contract from decode.go.
func TestReadVarintFailureLatchesTheStickyError(t *testing.T) {
	cases := []struct {
		name    string
		input   []byte
		wantErr error
	}{
		{"non minimal", []byte{0x40, 0x00}, ErrVarintNotMinimal},
		{"reserved prefix", []byte{0xc0}, ErrVarintReserved},
		{"truncated", []byte{0x40}, ErrTruncated},
	}
	for _, c := range cases {
		r := NewReader(c.input)
		if _, err := r.ReadVarint(); !errors.Is(err, c.wantErr) {
			t.Fatalf("%s: first read gave %v, want %v", c.name, err, c.wantErr)
		}
		v, err := r.ReadVarint()
		if !errors.Is(err, c.wantErr) {
			t.Errorf("%s: second read gave %d, %v; want the same latched %v", c.name, v, err, c.wantErr)
		}
		if r.Offset() != 0 {
			t.Errorf("%s: cursor is %d after two failed reads, want 0", c.name, r.Offset())
		}
	}
}
