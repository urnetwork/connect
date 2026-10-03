// The top level entry points. Unmarshal enforces full consumption, because a
// decoder that ignores a tail accepts two encodings of one object and MLS signs
// over serialized forms.
package syntax

import (
	"bytes"
	"errors"
	"testing"
)

type marshalProbe struct {
	Value uint16
	Body  []byte
}

var _ Codec = (*marshalProbe)(nil)

func (self *marshalProbe) MarshalMLS(w *Writer) error {
	w.WriteUint16(self.Value)
	w.WriteOpaque(self.Body)
	return nil
}

func (self *marshalProbe) UnmarshalMLS(r *Reader) error {
	value, err := r.ReadUint16()
	if err != nil {
		return err
	}
	body, err := r.ReadOpaque()
	if err != nil {
		return err
	}
	self.Value = value
	self.Body = body
	return nil
}

func TestMarshalUnmarshalRoundTrips(t *testing.T) {
	in := marshalProbe{Value: 0xbeef, Body: []byte{0xaa, 0xbb}}
	bs, err := Marshal(&in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(bs, []byte{0xbe, 0xef, 0x02, 0xaa, 0xbb}) {
		t.Errorf("encoded %x, want beef02aabb", bs)
	}
	out := marshalProbe{}
	if err := Unmarshal(bs, &out); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Value != in.Value || !bytes.Equal(out.Body, in.Body) {
		t.Errorf("decoded %#x %x, want %#x %x", out.Value, out.Body, in.Value, in.Body)
	}
}

func TestUnmarshalRejectsATrailingByte(t *testing.T) {
	out := marshalProbe{}
	err := Unmarshal([]byte{0xbe, 0xef, 0x02, 0xaa, 0xbb, 0x99}, &out)
	if !errors.Is(err, ErrTrailingBytes) {
		t.Errorf("gave %v, want ErrTrailingBytes", err)
	}
}

func TestUnmarshalPropagatesADecodeError(t *testing.T) {
	out := marshalProbe{}
	if err := Unmarshal([]byte{0xbe}, &out); !errors.Is(err, ErrTruncated) {
		t.Errorf("gave %v, want ErrTruncated", err)
	}
}

// the whole reason MarshalMLS returns an error rather than routing everything into
// the sticky error: an encoder refusal that is not a buffer error — a credential type
// outside the v1 profile, a content arm that disagrees with its discriminant — must
// reach the caller, because dropping it produces wrong signed bytes rather than a
// failure
type refusingProbe struct{}

var _ Codec = (*refusingProbe)(nil)

var errProbeRefusal = errors.New("mls syntax: probe refuses to encode")

func (self *refusingProbe) MarshalMLS(w *Writer) error {
	w.WriteUint16(0xbeef)
	return errProbeRefusal
}

func (self *refusingProbe) UnmarshalMLS(r *Reader) error {
	_, err := r.ReadUint16()
	return err
}

func TestMarshalSurfacesASemanticRefusal(t *testing.T) {
	bs, err := Marshal(&refusingProbe{})
	if !errors.Is(err, errProbeRefusal) {
		t.Errorf("Marshal gave %v, want the encoder's refusal", err)
	}
	if bs != nil {
		t.Errorf("Marshal returned %x alongside a refusal, want nil", bs)
	}
	if _, err := MarshalLimit(&refusingProbe{}, MaxRatchetTreeLength); !errors.Is(err, errProbeRefusal) {
		t.Errorf("MarshalLimit gave %v, want the encoder's refusal", err)
	}
}

func TestMarshalLimitBoundsTheEncoder(t *testing.T) {
	in := marshalProbe{Value: 1, Body: bytes.Repeat([]byte{0x11}, 64)}
	if _, err := MarshalLimit(&in, 32); !errors.Is(err, ErrLengthExceedsMax) {
		t.Errorf("gave %v, want ErrLengthExceedsMax", err)
	}
	if _, err := MarshalLimit(&in, 128); err != nil {
		t.Errorf("gave %v under a sufficient limit, want nil", err)
	}
}

func TestUnmarshalLimitRaisesTheDecoderBound(t *testing.T) {
	in := marshalProbe{Value: 1, Body: bytes.Repeat([]byte{0x11}, 64)}
	bs, err := Marshal(&in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := marshalProbe{}
	if err := UnmarshalLimit(bs, &out, 32); !errors.Is(err, ErrLengthExceedsMax) {
		t.Errorf("gave %v under a low limit, want ErrLengthExceedsMax", err)
	}
	if err := UnmarshalLimit(bs, &out, MaxRatchetTreeLength); err != nil {
		t.Errorf("gave %v under the ratchet tree limit, want nil", err)
	}
}

// funcProbe is an Unmarshaler whose decode is supplied per case, so the tests below
// can pin what Unmarshal does with a decoder that consumes too little, consumes
// exactly, or refuses on grounds no read of its own reported. The encode half is
// never called and exists only so the probe is usable wherever a Codec is.
type funcProbe struct {
	decode func(r *Reader) error
}

var _ Codec = (*funcProbe)(nil)

func (self *funcProbe) MarshalMLS(w *Writer) error {
	return nil
}

func (self *funcProbe) UnmarshalMLS(r *Reader) error {
	return self.decode(r)
}

var errProbeDecodeRefusal = errors.New("mls syntax: probe refuses to decode")

// TestUnmarshalReportsATailAlongsideADecoderRefusal is the test the plan's own
// sample fails. That sample returns the decoder's error and never consults Done, so
// a decoder that refuses on semantic grounds partway through a message reports the
// refusal and hides the four bytes it never reached — and a refusal latches nothing
// on the Reader, since every read it made succeeded, so there is no second place the
// tail could surface from. Joining the two reports both, which is what the encode
// half already does with the Writer's sticky error. The refusal must still be
// reported: this asserts the join is more informative, not that it traded one report
// for another.
func TestUnmarshalReportsATailAlongsideADecoderRefusal(t *testing.T) {
	probe := funcProbe{
		decode: func(r *Reader) error {
			if _, err := r.ReadUint16(); err != nil {
				return err
			}
			return errProbeDecodeRefusal
		},
	}
	err := Unmarshal([]byte{0xbe, 0xef, 0x11, 0x22, 0x33, 0x44}, &probe)
	if !errors.Is(err, errProbeDecodeRefusal) {
		t.Errorf("gave %v, want the decoder's refusal", err)
	}
	if !errors.Is(err, ErrTrailingBytes) {
		t.Errorf("gave %v, want the 4 unconsumed bytes reported alongside the refusal", err)
	}
}

// TestUnmarshalRejectsAnUnderConsumingDecoder covers the full consumption rule from
// the side an appended byte cannot reach. Every case here is fed input that is
// exactly the right length for what was encoded, so nothing is trailing in the
// input's own terms; what varies is how much of it the decoder reads. A decoder that
// stops early and reports success must be ErrTrailingBytes, and the exactly
// consuming case is in the same table so that "stopped early" is measured against a
// decoder that did not, rather than against an assumption.
func TestUnmarshalRejectsAnUnderConsumingDecoder(t *testing.T) {
	input := []byte{0xbe, 0xef, 0x11, 0x22}
	cases := []struct {
		consumed int
		decode   func(r *Reader) error
		want     error
	}{
		{
			consumed: 0,
			decode:   func(r *Reader) error { return nil },
			want:     ErrTrailingBytes,
		},
		{
			consumed: 2,
			decode:   func(r *Reader) error { _, err := r.ReadUint16(); return err },
			want:     ErrTrailingBytes,
		},
		{
			consumed: 3,
			decode:   func(r *Reader) error { _, err := r.ReadRaw(3); return err },
			want:     ErrTrailingBytes,
		},
		{
			consumed: 4,
			decode:   func(r *Reader) error { _, err := r.ReadUint32(); return err },
			want:     nil,
		},
	}
	for _, c := range cases {
		probe := funcProbe{decode: c.decode}
		err := Unmarshal(input, &probe)
		if c.want == nil && err != nil {
			t.Errorf("a decoder consuming all %d bytes gave %v, want nil", c.consumed, err)
		}
		if c.want != nil && !errors.Is(err, c.want) {
			t.Errorf("a decoder consuming %d of %d bytes gave %v, want %v", c.consumed, len(input), err, c.want)
		}
	}
}

// TestMarshalAndUnmarshalCarryTheirLimits pins what each entry point bounds its
// buffer by. The two unlimited forms are implemented as the limit taking forms
// called with the default rather than as second copies of the sequence, so what
// keeps the default from drifting is this rather than a duplicated constant, and the
// raised bound is asserted here too so that the ratchet tree paths have the property
// they depend on pinned at the entry point they call.
func TestMarshalAndUnmarshalCarryTheirLimits(t *testing.T) {
	probe := limitReportingProbe{}
	if _, err := Marshal(&probe); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if probe.writerLimit != MaxVectorLength {
		t.Errorf("Marshal bounded the Writer at %d, want MaxVectorLength", probe.writerLimit)
	}
	if err := Unmarshal(nil, &probe); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if probe.readerLimit != MaxVectorLength {
		t.Errorf("Unmarshal bounded the Reader at %d, want MaxVectorLength", probe.readerLimit)
	}
	if _, err := MarshalLimit(&probe, MaxRatchetTreeLength); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if probe.writerLimit != MaxRatchetTreeLength {
		t.Errorf("MarshalLimit bounded the Writer at %d, want MaxRatchetTreeLength", probe.writerLimit)
	}
	if err := UnmarshalLimit(nil, &probe, MaxRatchetTreeLength); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if probe.readerLimit != MaxRatchetTreeLength {
		t.Errorf("UnmarshalLimit bounded the Reader at %d, want MaxRatchetTreeLength", probe.readerLimit)
	}
}

// limitReportingProbe records the vector length limit of whichever buffer it was
// handed, and writes and reads nothing, so the limit is all that is under test.
type limitReportingProbe struct {
	writerLimit int
	readerLimit int
}

var _ Codec = (*limitReportingProbe)(nil)

func (self *limitReportingProbe) MarshalMLS(w *Writer) error {
	self.writerLimit = w.MaxVectorLength()
	return nil
}

func (self *limitReportingProbe) UnmarshalMLS(r *Reader) error {
	self.readerLimit = r.MaxVectorLength()
	return nil
}

// TestMarshalLimitAndUnmarshalLimitRejectANegativeLimit carries the API misuse
// contract NewWriterLimit and NewReaderLimit already document up to the entry
// points, which is where every caller outside this package meets it.
func TestMarshalLimitAndUnmarshalLimitRejectANegativeLimit(t *testing.T) {
	in := marshalProbe{Value: 1, Body: []byte{0xaa}}
	if _, err := MarshalLimit(&in, -1); !errors.Is(err, ErrNegativeLength) {
		t.Errorf("MarshalLimit gave %v, want ErrNegativeLength", err)
	}
	out := marshalProbe{}
	if err := UnmarshalLimit([]byte{0xbe, 0xef, 0x01, 0xaa}, &out, -1); !errors.Is(err, ErrNegativeLength) {
		t.Errorf("UnmarshalLimit gave %v, want ErrNegativeLength", err)
	}
}

func TestCheckRoundTripAcceptsAValidEncoding(t *testing.T) {
	in := marshalProbe{Value: 0xbeef, Body: []byte{0xaa, 0xbb}}
	bs, err := Marshal(&in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := CheckRoundTrip[marshalProbe, *marshalProbe](bs); err != nil {
		t.Errorf("gave %v on a valid encoding, want nil", err)
	}
}

func TestCheckRoundTripIgnoresARejectedInput(t *testing.T) {
	// a truncated input has no round trip obligation
	if err := CheckRoundTrip[marshalProbe, *marshalProbe]([]byte{0xbe}); err != nil {
		t.Errorf("gave %v on a rejected input, want nil", err)
	}
	if err := CheckRoundTrip[marshalProbe, *marshalProbe](nil); err != nil {
		t.Errorf("gave %v on empty input, want nil", err)
	}
}

// the property that catches the real bug: a decoder that accepts a non canonical
// encoding will decode it, re-encode to the canonical form, and disagree
func TestCheckRoundTripCatchesANonCanonicalEncoding(t *testing.T) {
	// 0x4002 is the two octet varint form of 2, which the decoder must already
	// reject; this asserts the property would catch it if the decoder did not
	nonCanonical := []byte{0xbe, 0xef, 0x40, 0x02, 0xaa, 0xbb}
	probe := marshalProbe{}
	if err := Unmarshal(nonCanonical, &probe); err == nil {
		t.Fatalf("the decoder accepted a non minimal length prefix; rule 1 is broken")
	}
	if err := CheckRoundTrip[marshalProbe, *marshalProbe](nonCanonical); err != nil {
		t.Errorf("gave %v for an input the decoder rejects, want nil", err)
	}
}

// lenientProbe is the accept-two-encodings defect in miniature, and exists so that
// the byte exactness half of CheckRoundTrip can be shown to fire rather than
// assumed to. It decodes any non zero octet as set and encodes set as 0x01, so
// 0x02 is a second wire form of a value its own encoder can only produce one way —
// the shape of a signature bypass primitive, since a verifier would accept bytes a
// signer never produced. Every case in the table below is one octet in and one
// octet out, so the comparison inside CheckRoundTrip is decided by content and
// never by a length difference; that is the regime a prefix comparison can sit in
// and appear to work while testing nothing.
type lenientProbe struct {
	flag bool
}

var _ Codec = (*lenientProbe)(nil)

func (self *lenientProbe) MarshalMLS(w *Writer) error {
	if self.flag {
		w.WriteUint8(0x01)
	} else {
		w.WriteUint8(0x00)
	}
	return nil
}

func (self *lenientProbe) UnmarshalMLS(r *Reader) error {
	v, err := r.ReadUint8()
	if err != nil {
		return err
	}
	self.flag = v != 0
	return nil
}

// TestCheckRoundTripReportsANonByteExactReencode is the non-vacuity proof for the
// first half of the property. The two accepted encodings are in the same table as
// the two rejected ones, so "it fired" is measured against a probe that is shown
// to round trip rather than against an assumption that it would have.
func TestCheckRoundTripReportsANonByteExactReencode(t *testing.T) {
	cases := []struct {
		input []byte
		want  error
	}{
		{input: []byte{0x00}, want: nil},
		{input: []byte{0x01}, want: nil},
		{input: []byte{0x02}, want: ErrRoundTripNotByteExact},
		{input: []byte{0xff}, want: ErrRoundTripNotByteExact},
	}
	for _, c := range cases {
		err := CheckRoundTrip[lenientProbe, *lenientProbe](c.input)
		if c.want == nil && err != nil {
			t.Errorf("%x gave %v, want nil", c.input, err)
		}
		if c.want != nil && !errors.Is(err, c.want) {
			t.Errorf("%x gave %v, want ErrRoundTripNotByteExact", c.input, err)
		}
	}
}

// TestCheckRoundTripReportsAnEncoderThatRefusesAnAcceptedValue covers the other
// way byte exactness fails: the decoder accepted a value its own encoder has no
// serialization for, so there are no bytes to compare at all. The refusal has to
// come back with the sentinel rather than instead of it, because a fuzz target
// only checks the sentinel.
func TestCheckRoundTripReportsAnEncoderThatRefusesAnAcceptedValue(t *testing.T) {
	err := CheckRoundTrip[refusingProbe, *refusingProbe]([]byte{0xbe, 0xef})
	if !errors.Is(err, ErrRoundTripNotByteExact) {
		t.Errorf("gave %v, want ErrRoundTripNotByteExact", err)
	}
	if !errors.Is(err, errProbeRefusal) {
		t.Errorf("gave %v, want the encoder's refusal carried alongside the sentinel", err)
	}
}

// driftingProbe models the only defect class the second pass of CheckRoundTrip can
// catch: a codec that is not a pure function of its input. Once the first
// re-encode is byte exact, the second pass decodes the very same bytes, so a
// deterministic codec cannot disagree with itself there and ErrRoundTripNotStable
// would be unreachable. What makes it reachable is hidden state carried between
// calls — a map ranged during encode, a decoder consulting a registry a later
// registration mutated, a buffer shared across decodes. CheckRoundTrip builds its
// own values with new(T), so that state cannot live on the probe instance and
// lives in the package level vars below instead, which is exactly where the real
// defects live too.
type driftingProbe struct {
	value        uint8
	refuseEncode bool
}

var _ Codec = (*driftingProbe)(nil)

var (
	driftingProbeDecodes int
	driftingProbeDrift   func(self *driftingProbe) error
)

var errProbeEncodeDrift = errors.New("mls syntax: probe refuses to encode on the second pass")

func (self *driftingProbe) MarshalMLS(w *Writer) error {
	if self.refuseEncode {
		return errProbeEncodeDrift
	}
	w.WriteUint8(self.value)
	return nil
}

func (self *driftingProbe) UnmarshalMLS(r *Reader) error {
	v, err := r.ReadUint8()
	if err != nil {
		return err
	}
	self.value = v
	driftingProbeDecodes += 1
	if driftingProbeDecodes >= 2 && driftingProbeDrift != nil {
		return driftingProbeDrift(self)
	}
	return nil
}

// TestCheckRoundTripReportsAnUnstableSecondPass is the non-vacuity proof for the
// second half of the property, and the first case in the table is the control that
// keeps the other three honest: with no drift installed the same probe over the
// same input returns nil, so each failure below is attributable to the drift
// rather than to a probe that could never have passed. Each drift is one of the
// three ways the second pass can end — a value that re-encodes differently, a
// decode that now refuses, an encode that now refuses — and the two refusals must
// carry their own error alongside the sentinel.
func TestCheckRoundTripReportsAnUnstableSecondPass(t *testing.T) {
	cases := []struct {
		drift     func(self *driftingProbe) error
		want      error
		wantCause error
	}{
		{
			drift:     nil,
			want:      nil,
			wantCause: nil,
		},
		{
			drift:     func(self *driftingProbe) error { self.value ^= 0xff; return nil },
			want:      ErrRoundTripNotStable,
			wantCause: nil,
		},
		{
			drift:     func(self *driftingProbe) error { return errProbeDecodeRefusal },
			want:      ErrRoundTripNotStable,
			wantCause: errProbeDecodeRefusal,
		},
		{
			drift:     func(self *driftingProbe) error { self.refuseEncode = true; return nil },
			want:      ErrRoundTripNotStable,
			wantCause: errProbeEncodeDrift,
		},
	}
	for i, c := range cases {
		driftingProbeDecodes = 0
		driftingProbeDrift = c.drift
		err := CheckRoundTrip[driftingProbe, *driftingProbe]([]byte{0x5a})
		if c.want == nil && err != nil {
			t.Errorf("case %d gave %v with no drift installed, want nil", i, err)
		}
		if c.want != nil && !errors.Is(err, c.want) {
			t.Errorf("case %d gave %v, want ErrRoundTripNotStable", i, err)
		}
		if c.wantCause != nil && !errors.Is(err, c.wantCause) {
			t.Errorf("case %d gave %v, want the cause carried alongside the sentinel", i, err)
		}
		if driftingProbeDecodes < 2 {
			t.Errorf("case %d decoded %d times, so the second pass never ran and the case proved nothing", i, driftingProbeDecodes)
		}
	}
	driftingProbeDrift = nil
	driftingProbeDecodes = 0
}

// limitObservingProbe records the vector length limit of every buffer it is
// handed. It exists alongside limitReportingProbe rather than reusing it because
// CheckRoundTrip builds its own values with new(T) and never hands them back, so a
// limit recorded on the instance is a limit nobody can read; these have to be
// package level to be observable at all. It writes and reads nothing, so the limit
// is the only thing under test and empty input round trips.
type limitObservingProbe struct{}

var _ Codec = (*limitObservingProbe)(nil)

var (
	observedWriterLimits []int
	observedReaderLimits []int
)

func (self *limitObservingProbe) MarshalMLS(w *Writer) error {
	observedWriterLimits = append(observedWriterLimits, w.MaxVectorLength())
	return nil
}

func (self *limitObservingProbe) UnmarshalMLS(r *Reader) error {
	observedReaderLimits = append(observedReaderLimits, r.MaxVectorLength())
	return nil
}

// TestCheckRoundTripCarriesItsLimitToBothHalves pins the delegation and the two
// halves at once, and does it by observing the limit rather than by observing an
// outcome the limit happens to change. That distinction is the whole test: over an
// input the default bound rejects, a correct entry point and one that ignored its
// limit argument both return nil — one because there is no obligation and one
// because it checked the wrong thing — so an outcome based assertion here could
// never tell them apart. Reading the number off the buffer can.
//
// Four observations are required per call, two decodes and two encodes, so a
// version that raised only the decoder is caught by the writer half and a version
// that skipped the second pass is caught by the count.
func TestCheckRoundTripCarriesItsLimitToBothHalves(t *testing.T) {
	cases := []struct {
		name string
		run  func(bs []byte) error
		want int
	}{
		{
			name: "the default entry point delegates with MaxVectorLength",
			run:  CheckRoundTrip[limitObservingProbe, *limitObservingProbe],
			want: MaxVectorLength,
		},
		{
			name: "the limit taking form carries the ratchet tree bound",
			run: func(bs []byte) error {
				return CheckRoundTripLimit[limitObservingProbe, *limitObservingProbe](bs, MaxRatchetTreeLength)
			},
			want: MaxRatchetTreeLength,
		},
		{
			name: "the limit taking form carries an arbitrary bound",
			run: func(bs []byte) error {
				return CheckRoundTripLimit[limitObservingProbe, *limitObservingProbe](bs, 32)
			},
			want: 32,
		},
	}
	for _, c := range cases {
		observedWriterLimits = nil
		observedReaderLimits = nil
		if err := c.run(nil); err != nil {
			t.Fatalf("%s: unexpected error: %v", c.name, err)
		}
		if len(observedReaderLimits) != 2 || len(observedWriterLimits) != 2 {
			t.Errorf("%s: %d decodes and %d encodes, want 2 of each so both passes are measured", c.name, len(observedReaderLimits), len(observedWriterLimits))
		}
		for _, got := range observedReaderLimits {
			if got != c.want {
				t.Errorf("%s: bounded a Reader at %d, want %d", c.name, got, c.want)
			}
		}
		for _, got := range observedWriterLimits {
			if got != c.want {
				t.Errorf("%s: bounded a Writer at %d, want %d", c.name, got, c.want)
			}
		}
	}
	observedWriterLimits = nil
	observedReaderLimits = nil
}

// bulkyLenientProbe is lenientProbe's defect — any non zero octet decodes as set,
// set encodes as 0x01 — carried behind an opaque field large enough that the whole
// structure only decodes under the raised bound. It is what the gap looks like in
// practice: a real round trip violation sitting inside a value the default bound
// will not even look at.
type bulkyLenientProbe struct {
	flag bool
	body []byte
}

var _ Codec = (*bulkyLenientProbe)(nil)

func (self *bulkyLenientProbe) MarshalMLS(w *Writer) error {
	if self.flag {
		w.WriteUint8(0x01)
	} else {
		w.WriteUint8(0x00)
	}
	w.WriteOpaque(self.body)
	return nil
}

func (self *bulkyLenientProbe) UnmarshalMLS(r *Reader) error {
	v, err := r.ReadUint8()
	if err != nil {
		return err
	}
	body, err := r.ReadOpaque()
	if err != nil {
		return err
	}
	self.flag = v != 0
	self.body = body
	return nil
}

// TestCheckRoundTripLimitExercisesAValueTheDefaultBoundRejects is the test that
// would have caught the gap. Both inputs are one octet over MaxVectorLength in
// their opaque field, so neither decodes under the default bound and the default
// entry point returns nil for both — the documented no-obligation contract, which
// is correct and completely silent, and is why a ratchet tree target built on it
// would report green having checked nothing at all.
//
// The non-vacuity is carried by the second input. Under the raised bound it must
// come back ErrRoundTripNotByteExact, which a limit taking form that ignored its
// argument could not produce: it would fail to decode and return nil like the
// default one does. So this fails against that version rather than merely passing
// against this one. The first input is the control that keeps the second
// attributable — same size, same raised bound, correct codec, nil — so the
// sentinel below is the leniency being caught and not the size.
//
// The differing octet is the flag rather than a length, so both encodings are the
// same total length and the comparison inside the property is decided by content.
func TestCheckRoundTripLimitExercisesAValueTheDefaultBoundRejects(t *testing.T) {
	oversized := bytes.Repeat([]byte{0x77}, MaxVectorLength+1)

	roundTripping := marshalProbe{Value: 0xbeef, Body: oversized}
	clean, err := MarshalLimit(&roundTripping, MaxRatchetTreeLength)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	lenient := bulkyLenientProbe{flag: true, body: oversized}
	broken, err := MarshalLimit(&lenient, MaxRatchetTreeLength)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 0x02 is the second wire form of a flag the encoder only ever writes as 0x01
	broken[0] = 0x02

	// the gap: neither input decodes under the default bound, so the property is
	// never reached and both come back nil
	if err := CheckRoundTrip[marshalProbe, *marshalProbe](clean); err != nil {
		t.Errorf("the default bound gave %v, want nil on an input it cannot decode", err)
	}
	if err := CheckRoundTrip[bulkyLenientProbe, *bulkyLenientProbe](broken); err != nil {
		t.Errorf("the default bound gave %v, want nil on an input it cannot decode", err)
	}

	// and under the raised bound the property is actually exercised, which is what
	// these two between them measure
	if err := CheckRoundTripLimit[marshalProbe, *marshalProbe](clean, MaxRatchetTreeLength); err != nil {
		t.Errorf("the raised bound gave %v on a value that round trips, want nil", err)
	}
	if err := CheckRoundTripLimit[bulkyLenientProbe, *bulkyLenientProbe](broken, MaxRatchetTreeLength); !errors.Is(err, ErrRoundTripNotByteExact) {
		t.Errorf("the raised bound gave %v on a value that does not round trip, want ErrRoundTripNotByteExact", err)
	}
}

// nextXorshift advances a 64 bit xorshift state and returns it. It is inlined here
// rather than taken from math/rand so this file adds no import to a package whose
// dependency set is a structural gate, and so the corpus below is byte for byte
// the same on every platform and every toolchain — a reachability number nobody
// else can reproduce is not a measurement. Any non zero seed works; zero is the
// one fixed point and never used.
func nextXorshift(state *uint64) uint64 {
	*state ^= *state << 13
	*state ^= *state >> 7
	*state ^= *state << 17
	return *state
}

// TestCheckRoundTripReachabilityOverAFuzzLikeCorpus measures the thing that
// decides whether every downstream fuzz target is worth anything. CheckRoundTrip
// returns nil for input that does not decode, which is the right contract and also
// a trap: a target fed only uniform random bytes returns nil on nearly every call
// and reports green while never once reaching an assertion. So this counts, over
// three corpora a fuzzer would plausibly produce, how many inputs actually reach
// the round trip assertions, and fails if the seeded corpus stops reaching them.
//
// The predicate is Unmarshal under the default limit, which is the identical call
// CheckRoundTrip makes to decide the same question, so the count is what the
// property saw and not a model of it. marshalProbe is deliberately a far easier
// target than any real structure: two octets, then a varint prefixed opaque field.
// A structure with a version, a cipher suite, several nested vectors and an
// enumerated arm is orders of magnitude harder to hit at random, so the random
// number here is an upper bound on what a real target would reach.
func TestCheckRoundTripReachabilityOverAFuzzLikeCorpus(t *testing.T) {
	valid := [][]byte{}
	for _, body := range [][]byte{
		nil,
		{0xaa},
		{0xaa, 0xbb},
		bytes.Repeat([]byte{0x11}, 63),
		bytes.Repeat([]byte{0x22}, 64),
		bytes.Repeat([]byte{0x33}, 300),
	} {
		bs, err := Marshal(&marshalProbe{Value: 0xbeef, Body: body})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		valid = append(valid, bs)
	}

	truncated := [][]byte{}
	for _, bs := range valid {
		for n := 0; n < len(bs); n += 1 {
			truncated = append(truncated, bs[:n])
		}
	}

	random := [][]byte{}
	state := uint64(0x5eed5eed5eed5eed)
	for i := 0; i < 4096; i += 1 {
		n := int(nextXorshift(&state) % 16)
		bs := make([]byte, n)
		for j := 0; j < n; j += 1 {
			bs[j] = uint8(nextXorshift(&state))
		}
		random = append(random, bs)
	}

	corpora := []struct {
		name    string
		inputs  [][]byte
		wantAll bool
	}{
		{name: "valid encodings", inputs: valid, wantAll: true},
		{name: "truncated valid encodings", inputs: truncated, wantAll: false},
		{name: "uniform random bytes", inputs: random, wantAll: false},
	}
	for _, corpus := range corpora {
		reached := 0
		for _, bs := range corpus.inputs {
			out := marshalProbe{}
			if Unmarshal(bs, &out) == nil {
				reached += 1
			}
			// a correct codec must never fail the property, whatever the input
			if err := CheckRoundTrip[marshalProbe, *marshalProbe](bs); err != nil {
				t.Errorf("%s: %x gave %v, want nil", corpus.name, bs, err)
			}
		}
		t.Logf("%s: %d of %d inputs decoded and reached the round trip assertions", corpus.name, reached, len(corpus.inputs))
		if corpus.wantAll && reached != len(corpus.inputs) {
			t.Errorf("%s: only %d of %d reached, so the corpus below it is not a baseline", corpus.name, reached, len(corpus.inputs))
		}
		if !corpus.wantAll && reached == len(corpus.inputs) {
			t.Errorf("%s: every one of %d inputs decoded, which means the corpus is not what it claims to be", corpus.name, len(corpus.inputs))
		}
	}
}
