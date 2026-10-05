// Writer behaviour: big endian integers, unprefixed raw bytes, and the sticky error
// that makes a single check at Bytes sufficient.
package syntax

import (
	"bytes"
	"errors"
	"testing"
)

// TestWriterIntegersAreBigEndian asserts WriteUint8/16/32/64 each append their value
// most significant byte first, with no padding or alignment between fields.
func TestWriterIntegersAreBigEndian(t *testing.T) {
	w := NewWriter()
	w.WriteUint8(0x2a)
	w.WriteUint16(0x0102)
	w.WriteUint32(0x01020304)
	w.WriteUint64(0x0102030405060708)
	out, err := w.Bytes()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []byte{
		0x2a,
		0x01, 0x02,
		0x01, 0x02, 0x03, 0x04,
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
	}
	if !bytes.Equal(out, want) {
		t.Errorf("encoded %x, want %x", out, want)
	}
	if w.Len() != len(want) {
		t.Errorf("Len is %d, want %d", w.Len(), len(want))
	}
}

// TestWriterRawTakesNoPrefix asserts WriteRaw appends exactly the given bytes with no
// length prefix, and that a nil or empty slice writes nothing.
func TestWriterRawTakesNoPrefix(t *testing.T) {
	w := NewWriter()
	w.WriteRaw([]byte{0xaa, 0xbb, 0xcc})
	w.WriteRaw(nil)
	w.WriteRaw([]byte{})
	out, err := w.Bytes()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(out, []byte{0xaa, 0xbb, 0xcc}) {
		t.Errorf("encoded %x, want aabbcc", out)
	}
}

// TestWriterErrorIsStickyAndSuppressesLaterWrites asserts the first error set wins over
// later ones, that Err and Bytes both report it, that Bytes hands back nil bytes
// alongside a non nil error, and that a write after an error is a no op — exercised
// through all six write methods (WriteUint8, WriteUint16, WriteUint32, WriteUint64,
// WriteRaw, WriteVarint), since each carries the identical error guard at a distinct
// call site.
func TestWriterErrorIsStickyAndSuppressesLaterWrites(t *testing.T) {
	w := NewWriter()
	w.WriteUint8(0x01)
	w.setErr(ErrLengthExceedsMax)
	w.setErr(ErrTruncated)
	w.WriteUint16(0xffff)
	w.WriteUint32(0xffffffff)
	w.WriteUint64(0xffffffffffffffff)
	w.WriteRaw([]byte{0xff, 0xff, 0xff})
	w.WriteVarint(0x2a)
	if !errors.Is(w.Err(), ErrLengthExceedsMax) {
		t.Errorf("Err is %v, want the first error ErrLengthExceedsMax", w.Err())
	}
	out, err := w.Bytes()
	if !errors.Is(err, ErrLengthExceedsMax) {
		t.Errorf("Bytes returned %v, want ErrLengthExceedsMax", err)
	}
	if out != nil {
		t.Errorf("Bytes returned %x alongside an error, want nil", out)
	}
	if w.Len() != 1 {
		t.Errorf("Len is %d, want 1: writes after an error must be no ops", w.Len())
	}
}

// TestNewWriterLimitRejectsNegative asserts a negative limit sets ErrNegativeLength as
// the sticky error at construction time, before any write, and that a write on such a
// Writer is a no op just like any other post error write.
func TestNewWriterLimitRejectsNegative(t *testing.T) {
	w := NewWriterLimit(-1)
	if !errors.Is(w.Err(), ErrNegativeLength) {
		t.Errorf("Err is %v, want ErrNegativeLength", w.Err())
	}
	w.WriteUint8(0x01)
	if w.Len() != 0 {
		t.Errorf("Len is %d, want 0: a write on a negative limit Writer must be a no op", w.Len())
	}
	out, err := w.Bytes()
	if !errors.Is(err, ErrNegativeLength) {
		t.Errorf("Bytes returned %v, want ErrNegativeLength", err)
	}
	if out != nil {
		t.Errorf("Bytes returned %x alongside an error, want nil", out)
	}
}

// TestWriterLimitIsCarried asserts NewWriter carries the package default MaxVectorLength
// and NewWriterLimit carries whatever limit the caller passes, such as MaxRatchetTreeLength.
func TestWriterLimitIsCarried(t *testing.T) {
	if NewWriter().MaxVectorLength() != MaxVectorLength {
		t.Errorf("NewWriter did not take the default limit")
	}
	if NewWriterLimit(MaxRatchetTreeLength).MaxVectorLength() != MaxRatchetTreeLength {
		t.Errorf("NewWriterLimit did not take the given limit")
	}
}

// TestWriteOpaqueTreatsNilAndEmptyAlike asserts a nil slice and a zero length,
// non nil slice both encode to the single zero length varint octet: opaque x<V>
// has no representation for "absent", only for "zero bytes", so the two Go values
// must collapse to the same wire form.
func TestWriteOpaqueTreatsNilAndEmptyAlike(t *testing.T) {
	for _, in := range [][]byte{nil, {}} {
		w := NewWriter()
		w.WriteOpaque(in)
		out, err := w.Bytes()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !bytes.Equal(out, []byte{0x00}) {
			t.Errorf("empty opaque encoded to %x, want 00", out)
		}
	}
}

// TestWriteOpaqueUsesTheVarintPrefix asserts WriteOpaque prefixes the body with
// exactly the RFC 9420 section 2.1.2 varint encoding of its length, at each width
// the varint format uses: one octet up to 63, two octets from 64, four octets
// from 16384.
func TestWriteOpaqueUsesTheVarintPrefix(t *testing.T) {
	cases := []struct {
		length int
		prefix []byte
	}{
		{1, []byte{0x01}},
		{63, []byte{0x3f}},
		{64, []byte{0x40, 0x40}},
		{16383, []byte{0x7f, 0xff}},
		{16384, []byte{0x80, 0x00, 0x40, 0x00}},
	}
	for _, c := range cases {
		body := bytes.Repeat([]byte{0x11}, c.length)
		w := NewWriter()
		w.WriteOpaque(body)
		out, err := w.Bytes()
		if err != nil {
			t.Fatalf("length %d: unexpected error %v", c.length, err)
		}
		want := append(append([]byte{}, c.prefix...), body...)
		if !bytes.Equal(out, want) {
			t.Errorf("length %d encoded to %x..., want prefix %x", c.length, out[:len(c.prefix)+1], c.prefix)
		}
	}
}

// TestWriteOpaqueRefusesToExceedTheLimit asserts a body longer than the Writer's
// configured maximum vector length sets the sticky ErrLengthExceedsMax instead of
// silently accepting an oversized field.
func TestWriteOpaqueRefusesToExceedTheLimit(t *testing.T) {
	w := NewWriterLimit(16)
	w.WriteOpaque(bytes.Repeat([]byte{0x11}, 17))
	if _, err := w.Bytes(); !errors.Is(err, ErrLengthExceedsMax) {
		t.Errorf("over limit write gave %v, want ErrLengthExceedsMax", err)
	}
}

// TestWriteOpaqueLPUsesAFixedThirtyTwoBitPrefix asserts the length prefix is
// always four big endian octets regardless of how short the body is, unlike the
// varint form whose width tracks the value: a nil body and an empty one both
// encode to four zero octets, so the record layer can locate a field's body at a
// fixed offset without first parsing a variable width prefix.
func TestWriteOpaqueLPUsesAFixedThirtyTwoBitPrefix(t *testing.T) {
	cases := []struct {
		body []byte
		want []byte
	}{
		{nil, []byte{0x00, 0x00, 0x00, 0x00}},
		{[]byte{}, []byte{0x00, 0x00, 0x00, 0x00}},
		{[]byte{0xaa}, []byte{0x00, 0x00, 0x00, 0x01, 0xaa}},
		{[]byte{0xaa, 0xbb}, []byte{0x00, 0x00, 0x00, 0x02, 0xaa, 0xbb}},
	}
	for _, c := range cases {
		w := NewWriter()
		w.WriteOpaqueLP(c.body)
		out, err := w.Bytes()
		if err != nil {
			t.Fatalf("body %x: unexpected error %v", c.body, err)
		}
		if !bytes.Equal(out, c.want) {
			t.Errorf("body %x encoded to %x, want %x", c.body, out, c.want)
		}
	}
}

// the two prefix forms must never be confusable, because connect/message uses LP
// for records and connect/mls uses <V> for MLS structures and one codec serves both
func TestLPAndVarintPrefixesAreDistinct(t *testing.T) {
	for _, body := range [][]byte{nil, {0xaa}, bytes.Repeat([]byte{0x11}, 200)} {
		lp := NewWriter()
		lp.WriteOpaqueLP(body)
		lpBytes, err := lp.Bytes()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		v := NewWriter()
		v.WriteOpaque(body)
		vBytes, err := v.Bytes()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if bytes.Equal(lpBytes, vBytes) {
			t.Errorf("body %x encoded identically under both prefix forms: %x", body, lpBytes)
		}
	}
}

// nestedWriters is the two framings WriteNested and WriteNestedLP put around the
// same nested encoding, paired with the return free opaque write each one is meant
// to be equivalent to. A table rather than two copies of every case below, because
// every property here holds identically for both forms and a property asserted for
// only one of them is how the two drift apart.
var nestedWriters = []struct {
	name        string
	writeNested func(w *Writer, encodeOne func(w *Writer) error) error
	writeOpaque func(w *Writer, bs []byte)
}{
	{
		name:        "varint framed",
		writeNested: (*Writer).WriteNested,
		writeOpaque: (*Writer).WriteOpaque,
	},
	{
		name:        "LP framed",
		writeNested: (*Writer).WriteNestedLP,
		writeOpaque: (*Writer).WriteOpaqueLP,
	},
}

// TestWriteNestedInheritsTheOuterVectorLimit is the whole reason the helper exists,
// and it is written to fail on a scratch Writer built with NewWriter instead of
// NewWriterLimit(self.MaxVectorLength()). The outer Writer is at
// MaxRatchetTreeLength and the nested body is one byte past MaxVectorLength, so the
// two constructions disagree: inheriting accepts the body, and a fresh default
// limited scratch refuses it with ErrLengthExceedsMax even though the encode it is
// part of is allowed sixteen mebibytes. The second half of the loop asserts that a
// default limited outer Writer really does refuse this body, so the case cannot
// quietly degenerate into one both constructions accept — without that, a body
// under a mebibyte would pass here whatever the scratch was built with.
func TestWriteNestedInheritsTheOuterVectorLimit(t *testing.T) {
	body := bytes.Repeat([]byte{0x5a}, MaxVectorLength+1)
	for _, c := range nestedWriters {
		w := NewWriterLimit(MaxRatchetTreeLength)
		err := c.writeNested(w, func(w *Writer) error {
			w.WriteOpaque(body)
			return nil
		})
		if err != nil {
			t.Fatalf("%s: a nested body of %d bytes under the ratchet tree limit gave %v; only a scratch writer built at the default limit refuses it", c.name, len(body), err)
		}
		out, err := w.Bytes()
		if err != nil {
			t.Fatalf("%s: Bytes gave %v", c.name, err)
		}
		if !bytes.HasSuffix(out, body) {
			t.Errorf("%s: encoding of %d bytes does not end in the nested body", c.name, len(out))
		}
		// four octets of outer prefix, four of the nested opaque's own varint
		if len(out) != 8+len(body) {
			t.Errorf("%s: encoded %d bytes, want %d", c.name, len(out), 8+len(body))
		}
		def := NewWriter()
		if err := c.writeNested(def, func(w *Writer) error {
			w.WriteOpaque(body)
			return nil
		}); !errors.Is(err, ErrLengthExceedsMax) {
			t.Errorf("%s: the same body at the default limit gave %v, want ErrLengthExceedsMax; the two limits do not separate, so this case proves nothing", c.name, err)
		}
	}
}

// TestWriteNestedIsANoOpAfterAFailure asserts the entry guard on the sticky error:
// a Writer that has already failed reports that error, appends nothing, and never
// runs encodeOne. Running the encoder anyway would let a nested structure's side
// effects and semantic refusals happen inside an encoding that can never be handed
// out, and would let a later failure inside the region displace the real cause in
// the report.
func TestWriteNestedIsANoOpAfterAFailure(t *testing.T) {
	for _, c := range nestedWriters {
		w := NewWriter()
		w.WriteUint8(0x01)
		w.setErr(ErrTruncated)
		ran := false
		err := c.writeNested(w, func(w *Writer) error {
			ran = true
			w.WriteUint8(0x02)
			return nil
		})
		if !errors.Is(err, ErrTruncated) {
			t.Errorf("%s: gave %v on a failed Writer, want the carried ErrTruncated", c.name, err)
		}
		if ran {
			t.Errorf("%s: encodeOne ran on a Writer that had already failed", c.name)
		}
		if w.Len() != 1 {
			t.Errorf("%s: Len is %d, want 1: nothing may be appended after a failure", c.name, w.Len())
		}
	}
}

// TestWriteNestedLatchesAnEncoderRefusal asserts a refusal from encodeOne is both
// returned and set sticky, matching WriteOptional. A caller that drops the return
// would otherwise take an encoding its own encoder refused to produce, and on this
// side of the codec a dropped refusal is wrong signed bytes rather than a failure.
func TestWriteNestedLatchesAnEncoderRefusal(t *testing.T) {
	refused := errors.New("mls syntax test: nested encoder refused")
	for _, c := range nestedWriters {
		w := NewWriter()
		w.WriteUint8(0x01)
		err := c.writeNested(w, func(w *Writer) error {
			w.WriteUint8(0x02)
			return refused
		})
		if !errors.Is(err, refused) {
			t.Errorf("%s: returned %v, want the encoder's own refusal", c.name, err)
		}
		if !errors.Is(w.Err(), refused) {
			t.Errorf("%s: Err is %v, want the refusal latched on the outer Writer", c.name, w.Err())
		}
		out, err := w.Bytes()
		if !errors.Is(err, refused) {
			t.Errorf("%s: Bytes gave %v, want the refusal", c.name, err)
		}
		if out != nil {
			t.Errorf("%s: Bytes returned %x alongside a refusal, want nil", c.name, out)
		}
	}
}

// TestWriteNestedSurfacesAScratchFailure asserts a failure the scratch Writer
// latched but the encoder did not report is still fatal. The encoder here returns
// nil after writing an opaque field past the inherited limit, which is exactly the
// shape of a leaf write failing inside a nested encoder that only ever returns nil
// because the leaf writes are return free. Dropping the error from scratch.Bytes
// would frame the nil that Bytes hands back alongside it as an empty region: a
// well formed encoding of a structure that was never encoded.
func TestWriteNestedSurfacesAScratchFailure(t *testing.T) {
	for _, c := range nestedWriters {
		w := NewWriterLimit(16)
		err := c.writeNested(w, func(w *Writer) error {
			w.WriteOpaque(bytes.Repeat([]byte{0x11}, 20))
			return nil
		})
		if !errors.Is(err, ErrLengthExceedsMax) {
			t.Errorf("%s: returned %v, want the scratch Writer's ErrLengthExceedsMax", c.name, err)
		}
		if !errors.Is(w.Err(), ErrLengthExceedsMax) {
			t.Errorf("%s: Err is %v, want ErrLengthExceedsMax latched on the outer Writer", c.name, w.Err())
		}
		if w.Len() != 0 {
			t.Errorf("%s: Len is %d, want 0: a refused region must not be framed as an empty one", c.name, w.Len())
		}
	}
}

// TestWriteNestedReportsAnOverLongRegion asserts the framing write's own refusal
// reaches the return, not only the sticky error. The nested encoding here is inside
// the inherited limit for every field it contains — the raw write carries no length
// prefix and so no limit check — and only the assembled region is too long, which
// the return free WriteOpaque reports through the sticky error alone. Returning a
// bare nil at the end would hide it from a caller that checks the return, the same
// reason WriteVector ends in the sticky error rather than nil.
func TestWriteNestedReportsAnOverLongRegion(t *testing.T) {
	for _, c := range nestedWriters {
		w := NewWriterLimit(16)
		err := c.writeNested(w, func(w *Writer) error {
			w.WriteRaw(bytes.Repeat([]byte{0x11}, 20))
			return nil
		})
		if !errors.Is(err, ErrLengthExceedsMax) {
			t.Errorf("%s: returned %v for a region of 20 bytes at a limit of 16, want ErrLengthExceedsMax", c.name, err)
		}
		if !errors.Is(w.Err(), ErrLengthExceedsMax) {
			t.Errorf("%s: Err is %v, want ErrLengthExceedsMax", c.name, w.Err())
		}
	}
}

// TestWriteNestedFramesExactlyTheHandRolledEncoding asserts the helper is a faithful
// replacement for the scratch-and-WriteOpaque idiom it exists to remove rather than
// merely a plausible one: byte for byte the same output, over an empty nested
// structure and over one whose own opaque field crosses the varint width boundary.
// The two framings are also asserted to differ from each other, since one codec
// serves both the MLS varint prefix and the record layer's fixed 32 bit one and a
// helper that confused them would pass every other case here.
func TestWriteNestedFramesExactlyTheHandRolledEncoding(t *testing.T) {
	items := []testItem{
		{Kind: 0x0000, Data: nil},
		{Kind: 0x0102, Data: []byte{0xaa}},
		{Kind: 0xffff, Data: bytes.Repeat([]byte{0x5a}, 63)},
		{Kind: 0x0001, Data: bytes.Repeat([]byte{0x5a}, 64)},
	}
	for _, item := range items {
		framed := make([][]byte, len(nestedWriters))
		for i, c := range nestedWriters {
			w := NewWriter()
			w.WriteUint8(0x7f)
			if err := c.writeNested(w, func(w *Writer) error {
				return item.MarshalMLS(w)
			}); err != nil {
				t.Fatalf("%s: item %x gave %v", c.name, item.Data, err)
			}
			out, err := w.Bytes()
			if err != nil {
				t.Fatalf("%s: item %x: Bytes gave %v", c.name, item.Data, err)
			}
			hand := NewWriter()
			hand.WriteUint8(0x7f)
			scratch := NewWriterLimit(hand.MaxVectorLength())
			if err := item.MarshalMLS(scratch); err != nil {
				t.Fatalf("%s: item %x: hand rolled encode gave %v", c.name, item.Data, err)
			}
			region, err := scratch.Bytes()
			if err != nil {
				t.Fatalf("%s: item %x: hand rolled Bytes gave %v", c.name, item.Data, err)
			}
			c.writeOpaque(hand, region)
			want, err := hand.Bytes()
			if err != nil {
				t.Fatalf("%s: item %x: hand rolled outer Bytes gave %v", c.name, item.Data, err)
			}
			if !bytes.Equal(out, want) {
				t.Errorf("%s: item %x encoded to %x, want the hand rolled %x", c.name, item.Data, out, want)
			}
			framed[i] = append([]byte{}, out...)
		}
		if bytes.Equal(framed[0], framed[1]) {
			t.Errorf("item %x encoded identically under both nested framings: %x", item.Data, framed[0])
		}
	}
}

// TestWriteNestedRoundTripsThroughReadNested asserts the region the encode half
// frames is exactly the region the decode half runs to empty. ReadNested reports
// ErrTrailingBytes for a region longer than the structure inside it and ErrTruncated
// for a shorter one, so a framing that was off by any amount in either direction
// fails here rather than at some later call site.
func TestWriteNestedRoundTripsThroughReadNested(t *testing.T) {
	items := []testItem{
		{Kind: 0x0000, Data: nil},
		{Kind: 0x0102, Data: []byte{0xaa, 0xbb}},
		{Kind: 0xffff, Data: bytes.Repeat([]byte{0x5a}, 200)},
	}
	cases := []struct {
		name  string
		write func(w *Writer, encodeOne func(w *Writer) error) error
		read  func(r *Reader, decodeOne func(r *Reader) error) error
	}{
		{name: "varint framed", write: (*Writer).WriteNested, read: (*Reader).ReadNested},
		{name: "LP framed", write: (*Writer).WriteNestedLP, read: (*Reader).ReadNestedLP},
	}
	for _, c := range cases {
		for _, item := range items {
			w := NewWriter()
			if err := c.write(w, func(w *Writer) error {
				return item.MarshalMLS(w)
			}); err != nil {
				t.Fatalf("%s: item %x gave %v", c.name, item.Data, err)
			}
			out, err := w.Bytes()
			if err != nil {
				t.Fatalf("%s: item %x: Bytes gave %v", c.name, item.Data, err)
			}
			r := NewReader(out)
			decoded := testItem{}
			if err := c.read(r, func(r *Reader) error {
				return decoded.UnmarshalMLS(r)
			}); err != nil {
				t.Fatalf("%s: item %x: decode gave %v", c.name, item.Data, err)
			}
			if err := r.Done(); err != nil {
				t.Errorf("%s: item %x: Done gave %v", c.name, item.Data, err)
			}
			if decoded.Kind != item.Kind || !bytes.Equal(decoded.Data, item.Data) {
				t.Errorf("%s: decoded %x %x, want %x %x", c.name, decoded.Kind, decoded.Data, item.Kind, item.Data)
			}
		}
	}
}

// the property the three case check above only spot checks, swept across every
// varint width boundary. The three bodies it uses are 0, 1 and 200 bytes, all of
// which fall in the regime where the two encodings differ in total length — one or
// two prefix octets against LP's four — so the check cannot fail whatever the
// prefixes say, and it never reaches the interesting case. From 16384 bytes up both
// forms are four octets and the encodings are the same total length, and
// distinctness rests entirely on the first octet: LP's is the length's top byte,
// which cannot exceed 0x3f because WriteOpaque refuses anything above MaxVarint, so
// its top two bits are 00, while the four octet varint's first octet carries the
// prefix 10. This asserts distinctness directly and also asserts that the equal
// length regime was actually reached, so the sweep cannot quietly degenerate into
// the trivial one.
func TestLPAndVarintPrefixesNeverCoincideAcrossWidths(t *testing.T) {
	lengths := []int{}
	// every length up to just past the one octet boundary, then each width boundary
	// and its neighbours, then the top of the range WriteOpaque accepts
	for n := 0; n <= 300; n += 1 {
		lengths = append(lengths, n)
	}
	lengths = append(lengths,
		16382, 16383, 16384, 16385, 16386,
		65535, 65536, 100000,
		MaxVectorLength-1, MaxVectorLength,
	)
	sameLengthCases := 0
	for _, n := range lengths {
		body := bytes.Repeat([]byte{0x5a}, n)
		lp := NewWriter()
		lp.WriteOpaqueLP(body)
		lpBytes, err := lp.Bytes()
		if err != nil {
			t.Fatalf("length %d: unexpected error %v", n, err)
		}
		v := NewWriter()
		v.WriteOpaque(body)
		vBytes, err := v.Bytes()
		if err != nil {
			t.Fatalf("length %d: unexpected error %v", n, err)
		}
		if bytes.Equal(lpBytes, vBytes) {
			t.Errorf("length %d encoded identically under both prefix forms", n)
		}
		if len(lpBytes) == len(vBytes) {
			sameLengthCases += 1
			// the encodings agree on every byte but the prefix here, so the whole
			// property rests on the first octet's top two bits
			if lpBytes[0]>>6 == vBytes[0]>>6 {
				t.Errorf("length %d gave both forms the same two prefix bits %#02b", n, lpBytes[0]>>6)
			}
		}
	}
	if sameLengthCases == 0 {
		t.Errorf("no length produced two encodings of equal total length, so the interesting regime was never reached")
	}
}
