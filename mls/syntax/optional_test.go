// optional<T> per rfc 9420 section 2.1.1: a presence octet, then the value when
// present. Any octet but 0 or 1 is malformed and must be rejected rather than
// treated as present, because "any non zero means present" would make two encodings
// of one value.
package syntax

import (
	"bytes"
	"errors"
	"testing"
)

func TestWriteOptionalAbsentIsASingleZero(t *testing.T) {
	w := NewWriter()
	if err := w.WriteOptional(false, func(w *Writer) error {
		t.Errorf("the value encoder ran for an absent optional")
		return nil
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out, err := w.Bytes()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(out, []byte{0x00}) {
		t.Errorf("absent optional encoded to %x, want 00", out)
	}
}

func TestWriteOptionalPresentCarriesItsValue(t *testing.T) {
	w := NewWriter()
	if err := w.WriteOptional(true, func(w *Writer) error {
		w.WriteUint16(0xbeef)
		return nil
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out, err := w.Bytes()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(out, []byte{0x01, 0xbe, 0xef}) {
		t.Errorf("present optional encoded to %x, want 01beef", out)
	}
}

// a semantic refusal from a nested encoder — the reason the callback returns an error
// at all — must reach the caller and must also stick to the writer, so a caller that
// drops the return cannot walk away with a truncated encoding
func TestWriteOptionalPropagatesAndSticksAnEncoderRefusal(t *testing.T) {
	refusal := errors.New("mls syntax: probe refusal")
	w := NewWriter()
	w.WriteUint8(0x01)
	err := w.WriteOptional(true, func(w *Writer) error {
		return refusal
	})
	if !errors.Is(err, refusal) {
		t.Errorf("WriteOptional returned %v, want the callback's refusal", err)
	}
	if !errors.Is(w.Err(), refusal) {
		t.Errorf("the writer's sticky error is %v, want the callback's refusal", w.Err())
	}
	if _, err := w.Bytes(); !errors.Is(err, refusal) {
		t.Errorf("Bytes returned %v, want the callback's refusal", err)
	}
}

func TestReadOptionalRoundTrips(t *testing.T) {
	r := NewReader([]byte{0x01, 0xbe, 0xef})
	value := uint16(0)
	present, err := r.ReadOptional(func(r *Reader) error {
		v, err := r.ReadUint16()
		if err != nil {
			return err
		}
		value = v
		return nil
	})
	if err != nil || !present || value != 0xbeef {
		t.Fatalf("gave present=%v value=%#x err=%v; want true, beef, nil", present, value, err)
	}
	if err := r.Done(); err != nil {
		t.Errorf("Done gave %v", err)
	}
}

func TestReadOptionalAbsentSkipsTheValueDecoder(t *testing.T) {
	r := NewReader([]byte{0x00})
	present, err := r.ReadOptional(func(r *Reader) error {
		t.Errorf("the value decoder ran for an absent optional")
		return nil
	})
	if err != nil || present {
		t.Fatalf("gave present=%v err=%v; want false, nil", present, err)
	}
}

func TestReadOptionalRejectsAMalformedPresenceOctet(t *testing.T) {
	for _, b := range []byte{0x02, 0x7f, 0x80, 0xff} {
		r := NewReader([]byte{b, 0xbe, 0xef})
		if _, err := r.ReadOptional(func(r *Reader) error { return nil }); !errors.Is(err, ErrOptionalPresence) {
			t.Errorf("presence octet %#x gave %v, want ErrOptionalPresence", b, err)
		}
		if r.Offset() != 0 {
			t.Errorf("presence octet %#x advanced the cursor on a failed read", b)
		}
	}
	r := NewReader([]byte{})
	if _, err := r.ReadOptional(func(r *Reader) error { return nil }); !errors.Is(err, ErrTruncated) {
		t.Errorf("empty input gave %v, want ErrTruncated", err)
	}
}

// the value decoder for a probe that must never run
func refuseToDecode(t *testing.T) func(r *Reader) error {
	return func(r *Reader) error {
		t.Errorf("the value decoder ran on a reader that had already failed")
		return nil
	}
}

// a reader carrying a latched failure must report it rather than read a presence
// octet out of bytes the failed read never validated. Without the entry check the
// octet below parses cleanly, the cursor advances past it and a value decoder that
// happens not to read reports a present value on a dead reader — the task 4
// vulnerability in a new method.
func TestReadOptionalRefusesAnAlreadyFailedReader(t *testing.T) {
	cases := []struct {
		latch func() *Reader
		want  error
	}{
		{
			// a truncated fixed width read: the failure latches and the cursor
			// stays on the presence octet, which is exactly the shape that lets a
			// later, smaller read succeed against the same bytes
			latch: func() *Reader {
				r := NewReader([]byte{0x01, 0xbe, 0xef})
				r.ReadUint32()
				return r
			},
			want: ErrTruncated,
		},
		{
			// the construction time misuse, latched before any read at all
			latch: func() *Reader {
				return NewReaderLimit([]byte{0x01, 0xbe, 0xef}, -1)
			},
			want: ErrNegativeLength,
		},
	}
	for _, c := range cases {
		r := c.latch()
		before := r.Offset()
		present, err := r.ReadOptional(refuseToDecode(t))
		if !errors.Is(err, c.want) {
			t.Errorf("a latched reader gave %v, want %v", err, c.want)
		}
		if present {
			t.Errorf("a latched reader reported a present value")
		}
		if r.Offset() != before {
			t.Errorf("a latched reader advanced the cursor from %d to %d", before, r.Offset())
		}
	}
}

// a failed presence octet must latch, not merely return. Done is checked before
// any further read, because a following read latches a failure of its own and
// would hide whether this one ever latched. On a version that returns the bare
// sentinel, the malformed octet leaves Done reporting ErrTrailingBytes for a tail
// that is really an unconsumed presence octet, and the following read hands back
// that octet as a uint8 with a nil error — a structurally valid decode of the
// wrong field.
func TestReadOptionalLatchesAFailedPresenceOctet(t *testing.T) {
	cases := []struct {
		input []byte
		want  error
	}{
		{input: []byte{}, want: ErrTruncated},
		{input: []byte{0x02, 0xbe, 0xef}, want: ErrOptionalPresence},
	}
	for _, c := range cases {
		r := NewReader(c.input)
		if _, err := r.ReadOptional(func(r *Reader) error { return nil }); !errors.Is(err, c.want) {
			t.Errorf("input %x gave %v, want %v", c.input, err, c.want)
		}
		if err := r.Done(); !errors.Is(err, c.want) {
			t.Errorf("input %x left Done reporting %v, want the latched %v", c.input, err, c.want)
		}
		if _, err := r.ReadUint8(); !errors.Is(err, c.want) {
			t.Errorf("input %x let a later read give %v, want the latched %v", c.input, err, c.want)
		}
	}
}

// a semantic refusal from a value decoder is the one failure nothing else can
// latch: the nested reads all succeed, so no read sets the sticky error and a
// caller that drops the return is left on a reader parked partway through a
// structure that was never accepted, with Done reporting success. The refusal must
// therefore stick, exactly as the encode half sticks its own.
func TestReadOptionalLatchesAValueDecoderRefusal(t *testing.T) {
	refusal := errors.New("mls syntax: probe refusal")
	r := NewReader([]byte{0x01, 0x07, 0x99})
	present, err := r.ReadOptional(func(r *Reader) error {
		// the read succeeds; the refusal is about what the byte means
		if _, err := r.ReadUint8(); err != nil {
			return err
		}
		return refusal
	})
	if !errors.Is(err, refusal) {
		t.Errorf("gave %v, want the value decoder's refusal", err)
	}
	// the presence octet said present and only the value's decoding failed, so a
	// caller that branches on presence before checking the error must not be told
	// the peer sent an absent optional
	if !present {
		t.Errorf("a refused value was reported absent")
	}
	if err := r.Done(); !errors.Is(err, refusal) {
		t.Errorf("Done gave %v, want the latched refusal", err)
	}
	if _, err := r.ReadUint8(); !errors.Is(err, refusal) {
		t.Errorf("a later read gave %v, want the latched refusal", err)
	}
}

// the encode half's entry check, and the mirror of the reader's: a writer that has
// already failed must not run the value encoder, whose side effects and refusals
// belong to an encoding that will never be handed out
func TestWriteOptionalRefusesAnAlreadyFailedWriter(t *testing.T) {
	w := NewWriterLimit(-1)
	err := w.WriteOptional(true, func(w *Writer) error {
		t.Errorf("the value encoder ran on a writer that had already failed")
		return nil
	})
	if !errors.Is(err, ErrNegativeLength) {
		t.Errorf("gave %v, want ErrNegativeLength", err)
	}
	if w.Len() != 0 {
		t.Errorf("wrote %d bytes on a failed writer", w.Len())
	}
}
