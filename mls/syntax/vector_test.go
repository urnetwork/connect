// T items<V>: a byte length prefix, then the concatenated elements. The prefix
// counts bytes rather than elements, which is the single most common way to get an
// MLS codec wrong, so these tests pin the byte count explicitly.
package syntax

import (
	"bytes"
	"errors"
	"testing"
)

func writeUint16Item(w *Writer, item uint16) error {
	w.WriteUint16(item)
	return nil
}

func readUint16Item(r *Reader) (uint16, error) {
	return r.ReadUint16()
}

func TestWriteVectorPrefixesBytesNotElements(t *testing.T) {
	cases := []struct {
		items []uint16
		want  []byte
	}{
		{nil, []byte{0x00}},
		{[]uint16{}, []byte{0x00}},
		{[]uint16{0x0001}, []byte{0x02, 0x00, 0x01}},
		{[]uint16{0x0001, 0x0002}, []byte{0x04, 0x00, 0x01, 0x00, 0x02}},
	}
	for _, c := range cases {
		w := NewWriter()
		if err := WriteVector(w, c.items, writeUint16Item); err != nil {
			t.Fatalf("items %v: unexpected error %v", c.items, err)
		}
		out, err := w.Bytes()
		if err != nil {
			t.Fatalf("items %v: unexpected error %v", c.items, err)
		}
		if !bytes.Equal(out, c.want) {
			t.Errorf("items %v encoded to %x, want %x", c.items, out, c.want)
		}
	}
}

// an element encoder's semantic refusal must reach the caller and stick to the outer
// writer, for the same reason WriteOptional's does
func TestWriteVectorPropagatesAndSticksAnElementRefusal(t *testing.T) {
	refusal := errors.New("mls syntax: probe refusal")
	w := NewWriter()
	err := WriteVector(w, []uint16{1, 2, 3}, func(w *Writer, item uint16) error {
		if item == 2 {
			return refusal
		}
		w.WriteUint16(item)
		return nil
	})
	if !errors.Is(err, refusal) {
		t.Errorf("WriteVector returned %v, want the element encoder's refusal", err)
	}
	if _, err := w.Bytes(); !errors.Is(err, refusal) {
		t.Errorf("Bytes returned %v, want the element encoder's refusal", err)
	}
}

func TestWriteVectorCrossesIntoTheTwoOctetPrefix(t *testing.T) {
	items := make([]uint16, 32)
	w := NewWriter()
	if err := WriteVector(w, items, writeUint16Item); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out, err := w.Bytes()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 32 elements is 64 bytes, which is the first length needing the two octet form
	if !bytes.Equal(out[:2], []byte{0x40, 0x40}) {
		t.Errorf("prefix is %x, want 4040 for a 64 byte vector", out[:2])
	}
	if len(out) != 2+64 {
		t.Errorf("encoded %d bytes, want 66", len(out))
	}
}

func TestReadVectorRoundTrips(t *testing.T) {
	items := []uint16{0x1111, 0x2222, 0x3333}
	w := NewWriter()
	if err := WriteVector(w, items, writeUint16Item); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	encoded, err := w.Bytes()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r := NewReader(encoded)
	got, err := ReadVector(r, readUint16Item)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != len(items) {
		t.Fatalf("decoded %d elements, want %d", len(got), len(items))
	}
	for i := range items {
		if got[i] != items[i] {
			t.Errorf("element %d is %#x, want %#x", i, got[i], items[i])
		}
	}
	if err := r.Done(); err != nil {
		t.Errorf("Done gave %v", err)
	}
}

func TestReadVectorEmptyIsNonNil(t *testing.T) {
	r := NewReader([]byte{0x00})
	got, err := ReadVector(r, readUint16Item)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Errorf("empty vector decoded to nil, want a zero length non nil slice")
	}
	if len(got) != 0 {
		t.Errorf("empty vector decoded to %v", got)
	}
}

// a declared byte length that does not land on an element boundary is malformed
func TestReadVectorRejectsMisalignedElements(t *testing.T) {
	r := NewReader([]byte{0x03, 0x11, 0x11, 0x22})
	if _, err := ReadVector(r, readUint16Item); !errors.Is(err, ErrTruncated) {
		t.Errorf("misaligned vector gave %v, want ErrTruncated", err)
	}
}

// an element decoder that consumes nothing would loop forever, so it is an error
func TestReadVectorRejectsAZeroLengthElement(t *testing.T) {
	r := NewReader([]byte{0x04, 0x11, 0x22, 0x33, 0x44})
	consumeNothing := func(r *Reader) (struct{}, error) {
		return struct{}{}, nil
	}
	if _, err := ReadVector(r, consumeNothing); !errors.Is(err, ErrZeroLengthElement) {
		t.Errorf("zero length element gave %v, want ErrZeroLengthElement", err)
	}
}

func TestReadVectorPropagatesAnElementError(t *testing.T) {
	r := NewReader([]byte{0x02, 0x11, 0x22})
	failing := func(r *Reader) (uint16, error) {
		return 0, ErrOptionalPresence
	}
	if _, err := ReadVector(r, failing); !errors.Is(err, ErrOptionalPresence) {
		t.Errorf("gave %v, want the element decoder's error", err)
	}
}

// the decode direction of the byte count rule: eight declared bytes of a two octet
// element is four elements, not eight. The count is never read off the wire, it falls
// out of consuming the region, which is why a vector prefix and an element count can
// never disagree. Named for what it asserts: the earlier name claimed it bounded the
// allocation a hostile input can cause, which it neither does nor can see — it feeds
// a well formed nine byte input and checks a length.
// TestVectorCapacityHintIsBoundedByAConstant in alloc_test.go covers that property.
func TestReadVectorDecodesByteCountNotElementCount(t *testing.T) {
	r := NewReader([]byte{0x08, 0x11, 0x11, 0x22, 0x22, 0x33, 0x33, 0x44, 0x44})
	got, err := ReadVector(r, readUint16Item)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 4 {
		t.Errorf("decoded %d elements from 8 bytes of uint16, want 4", len(got))
	}
}

// the probe refusal these tests share; declared once so a decoder and the assertion
// against it cannot drift apart
var probeVectorRefusal = errors.New("mls syntax: probe refusal")

// a failure inside the region must latch on the reader the caller handed in, not
// only be returned. This is the sharpest form of the sticky error contract in the
// package: ReadSub has already advanced the parent past the whole region before the
// element decoder ever runs, and the failure inside it latches on the sub reader,
// which the caller never receives. So a version that returns a bare error leaves the
// parent clean, parked exactly on the field after the vector, with Done reporting
// nil — the following uint16 below decodes as 3344 with no error and the whole
// message is accepted with a vector nobody ever decoded substituted out of it. The
// refusal case is the one nothing else could latch on its behalf: its own read
// succeeded, so no read set an error anywhere.
func TestReadVectorLatchesAFailureOnTheParent(t *testing.T) {
	cases := []struct {
		name   string
		decode func(r *Reader) (uint16, error)
		want   error
	}{
		{
			name: "a semantic refusal after a successful read",
			decode: func(r *Reader) (uint16, error) {
				if _, err := r.ReadUint16(); err != nil {
					return 0, err
				}
				return 0, probeVectorRefusal
			},
			want: probeVectorRefusal,
		},
		{
			name: "an element decoder that consumed nothing",
			decode: func(r *Reader) (uint16, error) {
				return 0, nil
			},
			want: ErrZeroLengthElement,
		},
	}
	for _, c := range cases {
		// a two byte region, then a following uint16 field that a clean parent
		// would happily decode
		r := NewReader([]byte{0x02, 0x11, 0x22, 0x33, 0x44})
		if _, err := ReadVector(r, c.decode); !errors.Is(err, c.want) {
			t.Errorf("%s: gave %v, want %v", c.name, err, c.want)
		}
		if v, err := r.ReadUint16(); !errors.Is(err, c.want) {
			t.Errorf("%s: the parent read on past the failure and gave %#x, %v; want %v", c.name, v, err, c.want)
		}
		if err := r.Done(); !errors.Is(err, c.want) {
			t.Errorf("%s: the parent's Done gave %v, want the latched %v", c.name, err, c.want)
		}
	}
}

// the trailing Done on the sub reader, which the loop condition does not make
// redundant. An element decoder that ignores a failed read of its own and returns
// successfully leaves that failure latched on the sub reader alone; if the region
// happens to end there, the loop exits with nothing to notice and the last element
// was built from bytes that were never on the wire. The read below fails because the
// region ends after the uint16, so nothing but Done on the sub reader can see it.
func TestReadVectorRejectsASwallowedElementReadFailure(t *testing.T) {
	r := NewReader([]byte{0x02, 0x11, 0x22})
	swallowATruncatedRead := func(r *Reader) (uint16, error) {
		v, err := r.ReadUint16()
		if err != nil {
			return 0, err
		}
		// an optional trailing octet the region does not carry, whose failure this
		// decoder wrongly ignores
		r.ReadUint8()
		return v, nil
	}
	if _, err := ReadVector(r, swallowATruncatedRead); !errors.Is(err, ErrTruncated) {
		t.Errorf("gave %v, want ErrTruncated from the sub reader's Done", err)
	}
	if err := r.Done(); !errors.Is(err, ErrTruncated) {
		t.Errorf("the parent's Done gave %v, want the latched ErrTruncated", err)
	}
}

// the element decoder for a probe that must never run
func refuseToDecodeElement(t *testing.T) func(r *Reader) (uint16, error) {
	return func(r *Reader) (uint16, error) {
		t.Errorf("the element decoder ran on a reader that had already failed")
		return 0, nil
	}
}

// a reader carrying a latched failure must report it rather than decode a vector out
// of bytes the failed read never validated. There is no entry check inside ReadVector
// for this: ReadSub is the first thing it touches and it checks for itself, so the
// guard would be unobservable. This pins the behaviour rather than the guard, so a
// later refactor that reads the byte slice directly fails here.
func TestReadVectorRefusesAnAlreadyFailedReader(t *testing.T) {
	cases := []struct {
		latch func() *Reader
		want  error
	}{
		{
			latch: func() *Reader {
				r := NewReader([]byte{0x02, 0x11, 0x22})
				r.ReadUint32()
				return r
			},
			want: ErrTruncated,
		},
		{
			latch: func() *Reader {
				return NewReaderLimit([]byte{0x02, 0x11, 0x22}, -1)
			},
			want: ErrNegativeLength,
		},
	}
	for _, c := range cases {
		r := c.latch()
		before := r.Offset()
		if _, err := ReadVector(r, refuseToDecodeElement(t)); !errors.Is(err, c.want) {
			t.Errorf("a latched reader gave %v, want %v", err, c.want)
		}
		if r.Offset() != before {
			t.Errorf("a latched reader advanced the cursor from %d to %d", before, r.Offset())
		}
	}
}

// the encode half's entry check, and the mirror of WriteOptional's: a writer that has
// already failed must not run the element encoder, whose side effects and refusals
// belong to an encoding that will never be handed out
func TestWriteVectorRefusesAnAlreadyFailedWriter(t *testing.T) {
	w := NewWriterLimit(-1)
	err := WriteVector(w, []uint16{1, 2, 3}, func(w *Writer, item uint16) error {
		t.Errorf("the element encoder ran on a writer that had already failed")
		return nil
	})
	if !errors.Is(err, ErrNegativeLength) {
		t.Errorf("gave %v, want ErrNegativeLength", err)
	}
	if w.Len() != 0 {
		t.Errorf("wrote %d bytes on a failed writer", w.Len())
	}
}

// the combined element bytes are checked against the writer's vector limit by
// WriteOpaque, which is return free and reports only through the sticky error. A
// version that ends in a bare nil hands a caller who checks the return a clean result
// for a vector it refused to encode; the failure is still unavoidable at Bytes, but a
// caller has no reason to expect the two to disagree, so the return carries it too.
func TestWriteVectorReportsAnOverLongVector(t *testing.T) {
	w := NewWriterLimit(4)
	err := WriteVector(w, []uint16{0x1111, 0x2222, 0x3333}, writeUint16Item)
	if !errors.Is(err, ErrLengthExceedsMax) {
		t.Errorf("a 6 byte vector under a 4 byte limit returned %v, want ErrLengthExceedsMax", err)
	}
	if !errors.Is(w.Err(), ErrLengthExceedsMax) {
		t.Errorf("the writer's sticky error is %v, want ErrLengthExceedsMax", w.Err())
	}
	if _, err := w.Bytes(); !errors.Is(err, ErrLengthExceedsMax) {
		t.Errorf("Bytes returned %v, want ErrLengthExceedsMax", err)
	}
}
