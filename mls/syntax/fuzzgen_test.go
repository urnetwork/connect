// A structure exercising every primitive the package offers, and a seeded
// generator for it. Spec A section 4.4 puts the structured generator here: go's
// native fuzzer takes byte strings only, so the structured variants of the
// OpenMLS targets are implemented as byte string targets seeded from a generator
// like this one.
//
// The generator exists because of what task 14 measured. CheckRoundTrip returns
// nil for an input that does not decode, and over uniform random bytes only 14 of
// 4096 inputs decoded even for a structure of two octets and one opaque field —
// 0.34 percent, against the easiest target that can be built. For a structure
// with a vector, an optional and a nested region it is indistinguishable from
// zero. So the single most important quality of what is below is that what it
// produces decodes, and the tests here measure that rather than assuming it.
//
// Three things the five generators in the validation and interop plan should copy
// from this one, since a generator that fails any of them makes its property
// vacuous while still reporting green:
//
//  1. Lengths are drawn from a table of the varint width boundaries as well as
//     uniformly, because a uniform draw over a range almost never lands on 63, 64,
//     16383 or 16384, which are the four lengths where the prefix changes width
//     and therefore the only lengths where a prefix bug shows up.
//  2. Every branch is taken in both directions — optional present and absent,
//     vector empty and non-empty, opaque empty and non-empty — and that is
//     asserted below rather than hoped for.
//  3. The seed is the only input, and both halves of determinism are asserted: the
//     same seed gives the same values, and a different seed gives different ones.
//     A generator that ignored its seed would satisfy the first alone.
package syntax

import "testing"

// testRand is the deterministic source every generated value is drawn from. It is
// a 64 bit xorshift over nextXorshift rather than a math/rand Rand for the reason
// marshal_test.go records for the same choice: this package's dependency set is a
// structural gate that a test file's import shows up in just as an implementation
// file's would, and a generated corpus that is byte for byte the same on every
// platform and every toolchain is what makes a failure reproducible from a seed
// alone. The modulo below is biased for a range that does not divide the word
// size, which is irrelevant to a corpus generator and would not be to anything
// with a security property.
type testRand struct {
	state uint64
}

// newTestRand seeds a generator. Zero is xorshift's one fixed point — it would
// produce an endless run of zeros, which is a generator that emits one value
// forever and passes everything — so it is replaced rather than accepted, and any
// other seed is used as given.
func newTestRand(seed uint64) *testRand {
	if seed == 0 {
		seed = 0x9e3779b97f4a7c15
	}
	return &testRand{
		state: seed,
	}
}

// nextUint64 advances the state and returns the whole word.
func (self *testRand) nextUint64() uint64 {
	return nextXorshift(&self.state)
}

// nextUint32 returns the low half of a fresh word.
func (self *testRand) nextUint32() uint32 {
	return uint32(self.nextUint64())
}

// nextIntn returns a value in [0, n). A non positive n returns zero rather than
// panicking, so a caller that computed a range from an empty table degrades to a
// fixed value instead of taking the suite down.
func (self *testRand) nextIntn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(self.nextUint64() % uint64(n))
}

// nextBool returns each value with equal probability.
func (self *testRand) nextBool() bool {
	return self.nextUint64()&1 == 1
}

// nextBytes returns n pseudorandom bytes, eight per state advance so that filling
// a sixteen kilobyte body costs two thousand advances rather than sixteen
// thousand; the byte stream is still a pure function of the seed.
func (self *testRand) nextBytes(n int) []byte {
	bs := make([]byte, n)
	for i := 0; i < n; i += 8 {
		v := self.nextUint64()
		for j := 0; j < 8 && i+j < n; j += 1 {
			bs[i+j] = byte(v >> (8 * j))
		}
	}
	return bs
}

// varintWidthLengths are the lengths where the RFC 9420 section 2.1.2 prefix
// changes width, with the value either side of each change: 63 is the widest one
// octet form and 64 the narrowest two octet one, 16383 the widest two octet form
// and 16384 the narrowest four octet one. A uniform draw over a range reaches
// these with probability one in the range's size, which over any practical number
// of runs is never, and they are the only lengths at which a boundary bug in the
// prefix is observable at all. So they are drawn from a table as well.
var varintWidthLengths = []int{0, 1, 63, 64, 65, 16382, 16383, 16384, 16385}

// pickLength returns a length in [0, max]: a third of the time from the width
// boundary table, restricted to the entries the cap allows, and uniformly
// otherwise. The mix is the point — the table alone would generate nine lengths
// and nothing between them, and the uniform draw alone would generate everything
// except the nine that matter.
func pickLength(rng *testRand, max int) int {
	if rng.nextIntn(3) == 0 {
		eligible := 0
		for _, n := range varintWidthLengths {
			if n > max {
				break
			}
			eligible += 1
		}
		if eligible > 0 {
			return varintWidthLengths[rng.nextIntn(eligible)]
		}
	}
	return rng.nextIntn(max + 1)
}

// testItem is the vector element and the nested structure both: a fixed width
// field followed by a varint prefixed opaque one, which is the smallest shape
// that puts a length prefix inside a length prefixed region.
type testItem struct {
	Kind uint16
	Data []byte
}

var _ Codec = (*testItem)(nil)

// MarshalMLS writes the two fields in order, leaf writes only, so the sticky error
// carries any failure.
func (self *testItem) MarshalMLS(w *Writer) error {
	w.WriteUint16(self.Kind)
	w.WriteOpaque(self.Data)
	return nil
}

// UnmarshalMLS reads the two fields in order and assigns nothing until both have
// succeeded, so a partially decoded element never reaches the caller.
func (self *testItem) UnmarshalMLS(r *Reader) error {
	kind, err := r.ReadUint16()
	if err != nil {
		return err
	}
	data, err := r.ReadOpaque()
	if err != nil {
		return err
	}
	self.Kind = kind
	self.Data = data
	return nil
}

// testStruct carries one field per primitive the package offers, so that a defect
// in any of them is a defect the round trip property can observe: the four fixed
// width integers, the unprefixed opaque x[N], the varint prefixed opaque x<V>,
// the record layer's LP prefixed opaque, an optional, a vector, and a structure
// nested inside a varint prefixed region. The three Reader methods it does not
// reach are ReadSubLP and ReadNestedLP, which frame a structure inside the record
// layer's prefix and belong to connect/message rather than to any MLS structure,
// and ReadSub, which ReadVector and ReadNested both reach through.
type testStruct struct {
	Version  uint16
	Flags    uint8
	Counter  uint64
	Fixed    [4]byte
	Body     []byte
	Tail     []byte
	HasExtra bool
	Extra    uint32
	Items    []testItem
	Nested   testItem
}

var _ Codec = (*testStruct)(nil)

// MarshalMLS writes the fields in declaration order. The nested structure goes
// through WriteNested, the encode side counterpart to ReadNested, which encodes it
// into a scratch Writer inheriting the outer limit and frames the result with
// WriteOpaque. The five generators modelled on this one should do the same rather
// than hand rolling those lines, since the inherited limit is the part that is
// silent when it is wrong.
func (self *testStruct) MarshalMLS(w *Writer) error {
	w.WriteUint16(self.Version)
	w.WriteUint8(self.Flags)
	w.WriteUint64(self.Counter)
	w.WriteRaw(self.Fixed[:])
	w.WriteOpaque(self.Body)
	w.WriteOpaqueLP(self.Tail)
	err := w.WriteOptional(self.HasExtra, func(w *Writer) error {
		w.WriteUint32(self.Extra)
		return nil
	})
	if err != nil {
		return err
	}
	err = WriteVector(w, self.Items, func(w *Writer, item testItem) error {
		return item.MarshalMLS(w)
	})
	if err != nil {
		return err
	}
	return w.WriteNested(func(w *Writer) error {
		return self.Nested.MarshalMLS(w)
	})
}

// UnmarshalMLS reads the fields in the same order and assigns nothing until every
// one of them has succeeded, so a value built from a read that failed can never
// reach the caller. The nested structure goes through ReadNested rather than
// ReadSub so that a region longer than the structure inside it is refused here
// too, which is the property that keeps one object from having two encodings.
func (self *testStruct) UnmarshalMLS(r *Reader) error {
	version, err := r.ReadUint16()
	if err != nil {
		return err
	}
	flags, err := r.ReadUint8()
	if err != nil {
		return err
	}
	counter, err := r.ReadUint64()
	if err != nil {
		return err
	}
	fixed, err := r.ReadRaw(4)
	if err != nil {
		return err
	}
	body, err := r.ReadOpaque()
	if err != nil {
		return err
	}
	tail, err := r.ReadOpaqueLP()
	if err != nil {
		return err
	}
	extra := uint32(0)
	hasExtra, err := r.ReadOptional(func(r *Reader) error {
		v, err := r.ReadUint32()
		if err != nil {
			return err
		}
		extra = v
		return nil
	})
	if err != nil {
		return err
	}
	items, err := ReadVector(r, func(r *Reader) (testItem, error) {
		item := testItem{}
		if err := item.UnmarshalMLS(r); err != nil {
			return testItem{}, err
		}
		return item, nil
	})
	if err != nil {
		return err
	}
	nested := testItem{}
	err = r.ReadNested(func(r *Reader) error {
		return nested.UnmarshalMLS(r)
	})
	if err != nil {
		return err
	}
	self.Version = version
	self.Flags = flags
	self.Counter = counter
	copy(self.Fixed[:], fixed)
	self.Body = body
	self.Tail = tail
	self.HasExtra = hasExtra
	self.Extra = extra
	self.Items = items
	self.Nested = nested
	return nil
}

// generateTestStruct draws one structure from rng. Every field is drawn
// independently so that no two branches are correlated, and the lengths go
// through pickLength so the varint width boundaries are reached rather than
// merely approached.
//
// One structure in eight carries a body cap past 16383, which is what lets the
// four octet varint form appear at all; the cap rather than the length is what is
// raised, so most of those structures are still small and the ones that are not
// are rare enough that twenty thousand runs stay under a second. Raising it for
// every structure would multiply the property's wall time by fifty for no extra
// construct reached.
func generateTestStruct(rng *testRand) testStruct {
	bodyMax := 300
	if rng.nextIntn(8) == 0 {
		bodyMax = 16500
	}
	items := make([]testItem, rng.nextIntn(5))
	for i := range items {
		items[i] = testItem{
			Kind: uint16(rng.nextUint32()),
			Data: rng.nextBytes(pickLength(rng, 80)),
		}
	}
	s := testStruct{
		Version:  uint16(rng.nextUint32()),
		Flags:    uint8(rng.nextUint32()),
		Counter:  rng.nextUint64(),
		Fixed:    [4]byte{},
		Body:     rng.nextBytes(pickLength(rng, bodyMax)),
		Tail:     rng.nextBytes(pickLength(rng, 300)),
		HasExtra: rng.nextBool(),
		Extra:    rng.nextUint32(),
		Items:    items,
		Nested: testItem{
			Kind: uint16(rng.nextUint32()),
			Data: rng.nextBytes(pickLength(rng, 80)),
		},
	}
	copy(s.Fixed[:], rng.nextBytes(4))
	return s
}

// structuralBucket is one construct the round trip property has to actually reach
// for its result to mean anything, and the predicate that recognizes a generated
// structure reaching it. A table rather than a map so the counts below report in a
// fixed order, and so a bucket that stops being reached names itself.
type structuralBucket struct {
	name  string
	holds func(s testStruct) bool
}

// anyItem reports whether any element of the vector satisfies holds, which is what
// separates "the vector was non empty" from "the vector contained the thing".
func anyItem(items []testItem, holds func(item testItem) bool) bool {
	for _, item := range items {
		if holds(item) {
			return true
		}
	}
	return false
}

// structuralCoverage is every construct the generator claims to reach. The four
// prefix width buckets are the reason the table exists: a generator drawing only
// uniform lengths satisfies "produces opaque fields of many sizes" while never
// once crossing a width boundary, so a prefix bug at 63 or at 16384 would sit
// under a property test reporting green over any number of runs.
var structuralCoverage = []structuralBucket{
	{
		name:  "opaque of length 0, the empty one octet prefix",
		holds: func(s testStruct) bool { return len(s.Body) == 0 },
	},
	{
		name:  "opaque of length 63, the widest one octet prefix",
		holds: func(s testStruct) bool { return len(s.Body) == 63 },
	},
	{
		name:  "opaque of length 64, the narrowest two octet prefix",
		holds: func(s testStruct) bool { return len(s.Body) == 64 },
	},
	{
		name:  "opaque of length 16383, the widest two octet prefix",
		holds: func(s testStruct) bool { return len(s.Body) == 16383 },
	},
	{
		name:  "opaque of length 16384, the narrowest four octet prefix",
		holds: func(s testStruct) bool { return len(s.Body) == 16384 },
	},
	{
		name:  "opaque above 16384, inside the four octet prefix",
		holds: func(s testStruct) bool { return len(s.Body) > 16384 },
	},
	{
		name:  "optional present",
		holds: func(s testStruct) bool { return s.HasExtra },
	},
	{
		name:  "optional absent",
		holds: func(s testStruct) bool { return !s.HasExtra },
	},
	{
		name:  "vector empty",
		holds: func(s testStruct) bool { return len(s.Items) == 0 },
	},
	{
		name:  "vector of one element",
		holds: func(s testStruct) bool { return len(s.Items) == 1 },
	},
	{
		name:  "vector of more than one element",
		holds: func(s testStruct) bool { return len(s.Items) > 1 },
	},
	{
		name: "vector element carrying a non empty opaque, so a prefix nests inside a prefixed region",
		holds: func(s testStruct) bool {
			return anyItem(s.Items, func(item testItem) bool { return len(item.Data) > 0 })
		},
	},
	{
		name: "vector element carrying an empty opaque",
		holds: func(s testStruct) bool {
			return anyItem(s.Items, func(item testItem) bool { return len(item.Data) == 0 })
		},
	},
	{
		name: "vector element crossing into the two octet prefix",
		holds: func(s testStruct) bool {
			return anyItem(s.Items, func(item testItem) bool { return len(item.Data) >= 64 })
		},
	},
	{
		name:  "nested structure carrying a non empty opaque",
		holds: func(s testStruct) bool { return len(s.Nested.Data) > 0 },
	},
	{
		name:  "nested structure carrying an empty opaque",
		holds: func(s testStruct) bool { return len(s.Nested.Data) == 0 },
	},
	{
		name:  "LP prefixed field empty",
		holds: func(s testStruct) bool { return len(s.Tail) == 0 },
	},
	{
		name:  "LP prefixed field non empty",
		holds: func(s testStruct) bool { return len(s.Tail) > 0 },
	},
}

// TestGeneratorReachesEveryStructuralBoundary is the non-vacuity gate on the round
// trip property, and it runs the same seed and the same number of draws as
// TestRoundTripProperty does, so what it counts is the population that property
// actually saw rather than a model of it. A generator that drifted into emitting
// mostly empty structures would keep the property passing and stop it meaning
// anything; this fails instead, naming the construct that went missing.
func TestGeneratorReachesEveryStructuralBoundary(t *testing.T) {
	rng := newTestRand(roundTripSeed)
	counts := make([]int, len(structuralCoverage))
	for i := 0; i < roundTripRuns; i += 1 {
		s := generateTestStruct(rng)
		for j, bucket := range structuralCoverage {
			if bucket.holds(s) {
				counts[j] += 1
			}
		}
	}
	for j, bucket := range structuralCoverage {
		t.Logf("%6d of %d reached %s", counts[j], roundTripRuns, bucket.name)
		if counts[j] == 0 {
			t.Errorf("no structure in %d reached %s; the round trip property no longer covers it", roundTripRuns, bucket.name)
		}
	}
}

// TestGeneratorIsDeterministicInItsSeed asserts both halves of determinism,
// because only the pair of them is worth anything. The same seed giving the same
// values is what makes a failure reproducible from the test name; different seeds
// giving different values is what says the seed is being used at all, and a
// generator that ignored its argument entirely would satisfy the first assertion
// perfectly. The comparison is over encoded bytes rather than over the structures,
// so it fails on a field the encoder covers and the eye does not.
func TestGeneratorIsDeterministicInItsSeed(t *testing.T) {
	const runs = 256
	encodeRun := func(seed uint64) [][]byte {
		rng := newTestRand(seed)
		out := make([][]byte, 0, runs)
		for i := 0; i < runs; i += 1 {
			s := generateTestStruct(rng)
			bs, err := Marshal(&s)
			if err != nil {
				t.Fatalf("seed %d run %d: encode gave %v", seed, i, err)
			}
			out = append(out, bs)
		}
		return out
	}
	first := encodeRun(roundTripSeed)
	repeat := encodeRun(roundTripSeed)
	other := encodeRun(roundTripSeed + 1)
	for i := 0; i < runs; i += 1 {
		if string(first[i]) != string(repeat[i]) {
			t.Fatalf("run %d differed between two generators on seed %d; the generator is not a function of its seed", i, roundTripSeed)
		}
	}
	differing := 0
	for i := 0; i < runs; i += 1 {
		if string(first[i]) != string(other[i]) {
			differing += 1
		}
	}
	if differing != runs {
		t.Errorf("%d of %d runs differed between seed %d and seed %d, want all of them; the seed is not driving every draw", differing, runs, roundTripSeed, roundTripSeed+1)
	}
}
