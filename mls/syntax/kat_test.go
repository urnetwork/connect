// The byte exact golden table. Every other test in this package asserts a
// behaviour; this one asserts the bytes. MLS signs over serialized forms and the
// message server rebuilds records through this same encoder, so a change to any row
// here is a wire break rather than a refactor and has to be argued as one.
//
// Every expected byte string below was derived by hand from the encoding rules in
// RFC 9420 section 2.1 and its two sub sections, not captured from a run of the
// encoder. A table captured from the implementation cannot fail on the day it is
// written: it freezes whatever the code happens to do, bug included, and reads in
// review exactly like a derived one. The derivation for each family is written out
// in the comment above it so a reviewer can redo it without the spec in hand.
//
// The rules being applied:
//
//   - a fixed width integer is network byte order, most significant octet first
//     (section 2.1, inherited from the TLS presentation language)
//   - the varint's top two bits of the first octet are the base 2 logarithm of the
//     octet count, and the value occupies the remaining bits in network byte order:
//     prefix 00 carries 0..63 in one octet, 01 carries 64..16383 in two, 10 carries
//     16384..2^30-1 in four, and 11 is invalid. The encoding must be the narrowest
//     that fits, so each width's range starts one past the previous width's top
//     (section 2.1.2)
//   - opaque x<V> is that varint over the byte length, then the bytes verbatim
//   - optional<T> is a presence octet, 0 or 1 and nothing else, then the value
//     itself with no framing of its own when present (section 2.1.1)
//   - T items<V> is a varint over the total byte length of the concatenated
//     elements, then those elements. The prefix counts bytes, never elements
//   - LP(x) is not RFC 9420 at all: it is the record layer's fixed 32 bit big endian
//     length, then the bytes verbatim
//
// Where a row's value could have been chosen so that a plausible wrong rule
// produces the same bytes, it was chosen the other way instead: no fixed width
// value is a palindrome under a byte swap, and the vectors that matter carry multi
// octet elements so a prefix that counted elements would disagree with one that
// counts bytes.
//
// The varint rows overlap the mlswg deserialization vectors vendored under
// mls/testdata/vectors, which state the same headers independently; the rows that
// coincide are marked, and the derivation and the vendored vector agreed on every
// one of them.
package syntax

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// The leaf writes are return free, so this adapts one into the table's row type
// without a four line closure per row.
func kat(write func(w *Writer)) func(w *Writer) error {
	return func(w *Writer) error {
		write(w)
		return nil
	}
}

// A body of this octet repeated is used wherever a row needs length without
// needing content; 0x11 is not 0x00, so a prefix accidentally counted into the body
// or a body accidentally truncated shows up rather than blending in.
const katFill byte = 0x11

// Pins the exact octets of every primitive encoder for values chosen at the width
// transitions and at the points where a wrong reading of the rule would diverge.
func TestEncodingKAT(t *testing.T) {
	threeHundred := bytes.Repeat([]byte{katFill}, 300)
	cases := []struct {
		name  string
		write func(w *Writer) error
		want  string
	}{
		// fixed width integers, most significant octet first. Each value differs
		// from its own byte reversal, so a little endian encoder fails these rows
		// rather than passing them: 0x0102 reversed is 0x0201, 0xbeef reversed is
		// 0xefbe, and the 32 and 64 bit values are ascending sequences whose
		// reversal and whose 16 bit word swap are both different strings.
		{"uint8 0x2a", kat(func(w *Writer) { w.WriteUint8(0x2a) }), "2a"},
		{"uint8 0xff", kat(func(w *Writer) { w.WriteUint8(0xff) }), "ff"},
		{"uint16 0x0102", kat(func(w *Writer) { w.WriteUint16(0x0102) }), "0102"},
		{"uint16 0xbeef", kat(func(w *Writer) { w.WriteUint16(0xbeef) }), "beef"},
		{"uint32 0x01020304", kat(func(w *Writer) { w.WriteUint32(0x01020304) }), "01020304"},
		{"uint32 0xdeadbeef", kat(func(w *Writer) { w.WriteUint32(0xdeadbeef) }), "deadbeef"},
		{"uint64 0x0102030405060708", kat(func(w *Writer) { w.WriteUint64(0x0102030405060708) }), "0102030405060708"},
		{"uint64 0x0123456789abcdef", kat(func(w *Writer) { w.WriteUint64(0x0123456789abcdef) }), "0123456789abcdef"},

		// opaque x[N]: the bytes and nothing else, no prefix of any width.
		{"raw nil", kat(func(w *Writer) { w.WriteRaw(nil) }), ""},
		{"raw empty", kat(func(w *Writer) { w.WriteRaw([]byte{}) }), ""},
		{"raw two bytes", kat(func(w *Writer) { w.WriteRaw([]byte{0xaa, 0xbb}) }), "aabb"},

		// the one octet varint form, prefix bits 00, value in the low six bits, so
		// the octet is the value itself. 63 is the last value that fits and is the
		// row a wrong comparison at the boundary fails: an encoder testing v < 63
		// rather than v <= 63 emits 403f here.
		{"varint 0", kat(func(w *Writer) { w.WriteVarint(0) }), "00"},                               // vendored vector agrees
		{"varint 1", kat(func(w *Writer) { w.WriteVarint(1) }), "01"},                               //
		{"varint 13", kat(func(w *Writer) { w.WriteVarint(13) }), "0d"},                             // vendored vector agrees
		{"varint 54", kat(func(w *Writer) { w.WriteVarint(54) }), "36"},                             // vendored vector agrees
		{"varint 63 is the last one octet value", kat(func(w *Writer) { w.WriteVarint(63) }), "3f"}, // vendored vector agrees

		// the two octet form, prefix bits 01. 64 is 0x0040, so the first octet is
		// 0x40 or 0x00 and the second is 0x40: an encoder that dropped the width
		// transition emits the single octet 40 here, which decodes as 0 in the two
		// octet form and is caught by the length as well as by the value. 255 is
		// the row that fails an encoder still using one octet for anything under
		// 256. 389 is 0x0185, so the prefix lands in an octet that already carries
		// value bits and a prefix written into the wrong octet shows up. 16383 is
		// 0x3fff and is the last value of this width, so 0x40 or 0x3f is 0x7f.
		{"varint 64 crosses to two octets", kat(func(w *Writer) { w.WriteVarint(64) }), "4040"},             // vendored vector agrees
		{"varint 255", kat(func(w *Writer) { w.WriteVarint(255) }), "40ff"},                                 // vendored vector agrees
		{"varint 389", kat(func(w *Writer) { w.WriteVarint(389) }), "4185"},                                 // vendored vector agrees
		{"varint 2730", kat(func(w *Writer) { w.WriteVarint(2730) }), "4aaa"},                               // vendored vector agrees
		{"varint 4095", kat(func(w *Writer) { w.WriteVarint(4095) }), "4fff"},                               // vendored vector agrees
		{"varint 16383 is the last two octet value", kat(func(w *Writer) { w.WriteVarint(16383) }), "7fff"}, // vendored vector agrees

		// the four octet form, prefix bits 10, so the first octet is 0x80 or the
		// top six value bits. 16384 is 0x00004000, giving 80 00 40 00, and there is
		// no three octet width to fall into. 48879 is 0x0000beef and 57005 is
		// 0x0000dead: both carry value in the low two octets only, so a little
		// endian body would put beef at the front and fail. 2^24 is 0x01000000 and
		// is the row where the octet that shares space with the prefix is non zero,
		// which a shift by the wrong amount gets wrong. MaxVarint is 0x3fffffff, so
		// every value bit is set and the first octet is 0x80 or 0x3f.
		{"varint 16384 crosses to four octets", kat(func(w *Writer) { w.WriteVarint(16384) }), "80004000"}, // vendored vector agrees
		{"varint 16385", kat(func(w *Writer) { w.WriteVarint(16385) }), "80004001"},
		{"varint 48879", kat(func(w *Writer) { w.WriteVarint(48879) }), "8000beef"},      // vendored vector agrees
		{"varint 57005", kat(func(w *Writer) { w.WriteVarint(57005) }), "8000dead"},      // vendored vector agrees
		{"varint 16777216", kat(func(w *Writer) { w.WriteVarint(1 << 24) }), "81000000"}, //
		{"varint max", kat(func(w *Writer) { w.WriteVarint(MaxVarint) }), "bfffffff"},    // vendored vector agrees

		// opaque x<V>: the varint over the byte length, then the bytes. Nil and
		// empty are the same wire form because the format has no way to say absent.
		{"opaque nil", kat(func(w *Writer) { w.WriteOpaque(nil) }), "00"},
		{"opaque empty", kat(func(w *Writer) { w.WriteOpaque([]byte{}) }), "00"},
		{"opaque one byte", kat(func(w *Writer) { w.WriteOpaque([]byte{0xaa}) }), "01aa"},
		{"opaque three bytes", kat(func(w *Writer) { w.WriteOpaque([]byte{0xaa, 0xbb, 0xcc}) }), "03aabbcc"},
		{"opaque four bytes", kat(func(w *Writer) { w.WriteOpaque([]byte{0xde, 0xad, 0xbe, 0xef}) }), "04deadbeef"},

		// LP(x): a fixed 32 bit big endian length whatever the length is, then the
		// bytes. The 300 byte row is 0x0000012c, which has a non zero octet above
		// the lowest, so a prefix truncated to 8 or 16 bits and a little endian
		// prefix both fail it; the topmost octet is exercised separately below,
		// since reaching it needs a body over 2^24 bytes.
		{"lp nil", kat(func(w *Writer) { w.WriteOpaqueLP(nil) }), "00000000"},
		{"lp empty", kat(func(w *Writer) { w.WriteOpaqueLP([]byte{}) }), "00000000"},
		{"lp one byte", kat(func(w *Writer) { w.WriteOpaqueLP([]byte{0xaa}) }), "00000001aa"},
		{"lp four bytes", kat(func(w *Writer) { w.WriteOpaqueLP([]byte{0xde, 0xad, 0xbe, 0xef}) }), "00000004deadbeef"},
		{
			"lp 300 bytes",
			kat(func(w *Writer) { w.WriteOpaqueLP(threeHundred) }),
			"0000012c" + hex.EncodeToString(threeHundred),
		},

		// optional<T>: one presence octet, then the value verbatim when present.
		// The absent row's value encoder writes an octet it never gets to write, so
		// the row fails on the bytes if the encoder is invoked anyway; that the
		// encoder is not invoked at all is asserted separately below. The present
		// and empty row is what distinguishes the presence octet from a length
		// prefix: a length prefix over a zero length value would also be a single
		// zero octet and would collide with absent.
		{
			"optional absent",
			func(w *Writer) error {
				return w.WriteOptional(false, func(w *Writer) error {
					w.WriteUint8(0xff)
					return nil
				})
			},
			"00",
		},
		{
			"optional present with an empty value",
			func(w *Writer) error {
				return w.WriteOptional(true, func(w *Writer) error { return nil })
			},
			"01",
		},
		{
			"optional present uint16",
			func(w *Writer) error {
				return w.WriteOptional(true, func(w *Writer) error {
					w.WriteUint16(0xbeef)
					return nil
				})
			},
			"01beef",
		},
		{
			"optional present opaque",
			func(w *Writer) error {
				return w.WriteOptional(true, func(w *Writer) error {
					w.WriteOpaque([]byte{0xaa, 0xbb})
					return nil
				})
			},
			"0102aabb",
		},

		// T items<V>: the varint over the total byte length of the elements. Every
		// row from the single element one down carries multi octet elements, so the
		// byte count and the element count differ and a prefix that counted
		// elements fails: one uint16 is a 2 octet prefix over 1 element, two are 4
		// over 2, three uint32 are 12 over 3, and the nested rows are 5 over 2 and
		// 8 over 2.
		{"vector nil", func(w *Writer) error { return WriteVector(w, nil, writeUint16Item) }, "00"},
		{"vector empty", func(w *Writer) error { return WriteVector(w, []uint16{}, writeUint16Item) }, "00"},
		{
			"vector one uint16 is prefixed 02 not 01",
			func(w *Writer) error { return WriteVector(w, []uint16{0x0102}, writeUint16Item) },
			"020102",
		},
		{
			"vector two uint16 is prefixed 04 not 02",
			func(w *Writer) error { return WriteVector(w, []uint16{0x0001, 0x0002}, writeUint16Item) },
			"0400010002",
		},
		{
			"vector three uint32 is prefixed 0c not 03",
			func(w *Writer) error {
				return WriteVector(w, []uint32{0x01020304, 0x05060708, 0x090a0b0c}, func(w *Writer, item uint32) error {
					w.WriteUint32(item)
					return nil
				})
			},
			"0c0102030405060708090a0b0c",
		},
		{
			"vector of opaque is prefixed by its own bytes plus theirs",
			func(w *Writer) error {
				return WriteVector(w, [][]byte{{0xaa}, {0xbb, 0xcc}}, func(w *Writer, item []byte) error {
					w.WriteOpaque(item)
					return nil
				})
			},
			"0501aa02bbcc",
		},
		{
			"vector of vector counts the inner prefixes too",
			func(w *Writer) error {
				return WriteVector(w, [][]uint16{{0x0001}, {0x0002, 0x0003}}, func(w *Writer, item []uint16) error {
					return WriteVector(w, item, writeUint16Item)
				})
			},
			"080200010400020003",
		},

		// fields concatenate with nothing between them: no padding, no alignment,
		// no outer framing. The four here are a fixed width integer, an absent
		// optional, an opaque and an LP, which is the shape a record header takes.
		{
			"fields concatenate with no framing between them",
			func(w *Writer) error {
				w.WriteUint16(0x0102)
				if err := w.WriteOptional(false, func(w *Writer) error { return nil }); err != nil {
					return err
				}
				w.WriteOpaque([]byte{0xaa})
				w.WriteOpaqueLP([]byte{0xbb})
				return nil
			},
			"01020001aa00000001bb",
		},
	}
	for _, c := range cases {
		w := NewWriter()
		if err := c.write(w); err != nil {
			t.Errorf("%s: unexpected error %v", c.name, err)
			continue
		}
		out, err := w.Bytes()
		if err != nil {
			t.Errorf("%s: unexpected error %v", c.name, err)
			continue
		}
		want, err := hex.DecodeString(c.want)
		if err != nil {
			t.Fatalf("%s: the golden value %q is not hex", c.name, c.want)
		}
		if !bytes.Equal(out, want) {
			t.Errorf("%s encoded to %x, want %s", c.name, out, c.want)
		}
	}
}

// The four opaque length boundaries, where an off by one in the width comparison
// changes the octet count rather than the value and so survives a round trip test.
// 63 is the last length that fits the one octet form and 64 is the first that does
// not; 16383 is the last that fits the two octet form and 16384 is the first that
// does not. The goldens are the same varint derivation as the table above, checked
// against the total encoded length so a prefix of the wrong width fails on the size
// even if its leading octet happened to match. These are separate from the table
// only because their bodies are too long to write as hex literals.
func TestEncodingKATOpaqueLengthBoundaries(t *testing.T) {
	cases := []struct {
		length int
		prefix string
	}{
		{63, "3f"},
		{64, "4040"},
		{16383, "7fff"},
		{16384, "80004000"},
	}
	for _, c := range cases {
		body := bytes.Repeat([]byte{katFill}, c.length)
		w := NewWriter()
		w.WriteOpaque(body)
		out, err := w.Bytes()
		if err != nil {
			t.Errorf("length %d: unexpected error %v", c.length, err)
			continue
		}
		prefix, err := hex.DecodeString(c.prefix)
		if err != nil {
			t.Fatalf("length %d: the golden prefix %q is not hex", c.length, c.prefix)
		}
		if len(out) != len(prefix)+c.length {
			t.Errorf("length %d encoded to %d bytes, want %d", c.length, len(out), len(prefix)+c.length)
			continue
		}
		if !bytes.Equal(out[:len(prefix)], prefix) {
			t.Errorf("length %d got prefix %x, want %s", c.length, out[:len(prefix)], c.prefix)
		}
		if !bytes.Equal(out[len(prefix):], body) {
			t.Errorf("length %d did not append the body verbatim", c.length)
		}
	}
}

// The record layer's fixed width prefix at the one length where its topmost octet
// is non zero. Every length under 2^24 leaves that octet zero, so a prefix written
// as three octets, or with the octets in the wrong order, or truncated to 16 bits
// is indistinguishable from a correct one everywhere else in this file. 2^24 is
// 0x01000000, which is MaxRatchetTreeLength exactly and therefore the largest body
// any Writer in this package will accept, so it is also the largest LP prefix that
// can occur on the wire.
func TestEncodingKATOpaqueLPTopmostOctet(t *testing.T) {
	body := bytes.Repeat([]byte{katFill}, MaxRatchetTreeLength)
	w := NewWriterLimit(MaxRatchetTreeLength)
	w.WriteOpaqueLP(body)
	out, err := w.Bytes()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 4+MaxRatchetTreeLength {
		t.Fatalf("encoded to %d bytes, want %d", len(out), 4+MaxRatchetTreeLength)
	}
	want := []byte{0x01, 0x00, 0x00, 0x00}
	if !bytes.Equal(out[:4], want) {
		t.Errorf("got prefix %x, want %x", out[:4], want)
	}
	if !bytes.Equal(out[4:], body) {
		t.Errorf("the body was not appended verbatim")
	}
}

// The vector prefix crossing the same one to two octet boundary, expressed in
// elements so that the byte count and the element count are as far apart as the
// boundary allows. 31 uint16 elements are 62 bytes and take the one octet prefix
// 0x3e; 32 are 64 bytes and take the two octet 0x4040. A prefix counting elements
// would emit 0x1f and 0x20 here, both one octet, so this row fails on the width as
// well as on the value.
func TestEncodingKATVectorLengthBoundary(t *testing.T) {
	cases := []struct {
		elements int
		prefix   string
	}{
		{31, "3e"},
		{32, "4040"},
	}
	for _, c := range cases {
		items := make([]uint16, c.elements)
		for i := range items {
			items[i] = 0x1111
		}
		w := NewWriter()
		if err := WriteVector(w, items, writeUint16Item); err != nil {
			t.Errorf("%d elements: unexpected error %v", c.elements, err)
			continue
		}
		out, err := w.Bytes()
		if err != nil {
			t.Errorf("%d elements: unexpected error %v", c.elements, err)
			continue
		}
		want := c.prefix + hex.EncodeToString(bytes.Repeat([]byte{0x11, 0x11}, c.elements))
		if hex.EncodeToString(out) != want {
			t.Errorf("%d elements encoded to %x, want %s", c.elements, out, want)
		}
	}
}

// An absent optional is exactly one zero octet and the value encoder never runs.
// The bytes alone cannot establish the second half, because an encoder that writes
// nothing produces the same output whether or not it was called, and a nested MLS
// encoder with a side effect or a semantic refusal would then fire on a field that
// is not on the wire. So this counts the invocations rather than inspecting the
// output.
func TestEncodingKATAbsentOptionalNeverRunsTheValueEncoder(t *testing.T) {
	invocations := 0
	w := NewWriter()
	if err := w.WriteOptional(false, func(w *Writer) error {
		invocations += 1
		return nil
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out, err := w.Bytes()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if invocations != 0 {
		t.Errorf("the value encoder ran %d times for an absent optional, want 0", invocations)
	}
	if !bytes.Equal(out, []byte{0x00}) {
		t.Errorf("an absent optional encoded to %x, want 00", out)
	}
}
