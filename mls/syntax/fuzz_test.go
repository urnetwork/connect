// Gate 4 properties 1 and 2 for the codec: no panic on any input, and byte exact
// round tripping of every input that decodes. Property 3, differential agreement
// with OpenMLS, is the validation and interop plan's and runs nightly against the
// out of process oracle; nothing here links or invokes it, and the nine
// OpenMLS mirroring targets over Extension, KeyPackage, MLSMessage, Proposal and
// Welcome are that plan's too.
//
// The canonical encoding property is what these three actually assert: any input
// the decoder accepts must re-encode to exactly the bytes it consumed. A decoder
// that accepted a second encoding of one length would give a signed structure two
// serializations, which is a signature bypass primitive rather than a leniency.
//
// Two measurements from earlier tasks decide the shape of everything below, and
// the five generators in the validation and interop plan should copy both.
//
// The first is reachability. Task 14 counted how often CheckRoundTrip reaches its
// property at all: 6 of 6 valid encodings, 0 of 450 truncations, and 14 of 4096
// uniform random inputs — 0.34 percent, against the easiest structure that can be
// built, since CheckRoundTrip returns nil for an input that does not decode. A
// target seeded with nothing therefore runs green forever without once evaluating
// its own property. So every target here seeds with valid encodings of its own
// type across the structural neighbourhood rather than one specimen, counts how
// many inputs reached the property, and fails at zero. The count is the only thing
// separating these targets from three that assert nothing.
//
// The second is the category boundary. Task 17 found that a round trip property
// over self encoded values misses a lenient ReadOptional — by construction, since
// a value this package encoded can never carry a presence octet of 2, so no round
// trip over generated structures can present one. Only hostile or mutated input
// can. Go's fuzzer mutates its seeds, which is exactly what closes that gap: the
// seeds buy reachability and the mutations buy leniency probing, and neither
// substitutes for the other. That is also why the seeds are kept small and why the
// hostile ones are literal bytes rather than encoder output — a corpus of large
// entries slows the mutation loop, and a seed built by the encoder under test
// moves with the encoder instead of pinning it.
package syntax

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// the corpus the validation and interop plan's seed generator fills, shared by
// every fuzz target in connect/mls so that a case one target found is replayed by
// all of them
const sharedCorpusDir = "../testdata/corpus"

// filler is what every seed body is made of. A repeated non zero octet rather than
// zeros, so that a length prefix bug which reads a body byte as part of the prefix
// changes the decoded length rather than landing on the same value by luck.
var filler = []byte{0xa5}

// readCorpusDir returns every regular file in dir as one seed. A missing directory
// returns nothing rather than failing, because the shared corpus and the seedgen
// that fills it belong to the validation and interop plan: on a fresh checkout of
// this wave the directory is legitimately absent, and a loader that treated that
// as an error would break every target in the package for a file nobody in this
// plan is supposed to have written. An unreadable entry is skipped for the same
// reason — the corpus is an accelerant, not a source of truth.
func readCorpusDir(dir string) [][]byte {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := [][]byte{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		bs, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		out = append(out, bs)
	}
	return out
}

// addSharedCorpus seeds f with the shared corpus if there is one.
func addSharedCorpus(f *testing.F) {
	f.Helper()
	for _, bs := range readCorpusDir(sharedCorpusDir) {
		f.Add(bs)
	}
}

// fuzzingEngineSuppliesInputs reports whether this process takes its inputs from
// the fuzzing engine rather than from the seed corpus, which decides whether the
// reachability gate below can be trusted to mean anything. Under -fuzz the
// coordinator process executes no input itself — the workers do — so its counters
// read zero however much fuzzing happened; a worker's counters cover whatever
// slice of a mutated corpus it was handed, where reaching nothing is an ordinary
// outcome for a run that is working. Either would make the gate fire on a healthy
// fuzz run, so the gate is restricted to the seed corpus pass under plain go test,
// which is the pass the per commit CI job runs and the one where the seeds are
// exactly the ones written here.
func fuzzingEngineSuppliesInputs() bool {
	if fl := flag.Lookup("test.fuzzworker"); fl != nil && fl.Value.String() == "true" {
		return true
	}
	if fl := flag.Lookup("test.fuzz"); fl != nil && fl.Value.String() != "" {
		return true
	}
	return false
}

// gateOnReachability is how a target refuses to report success without ever
// having evaluated its property. It registers a check that runs after the seed
// corpus pass and fails if not one input reached construct, which is the fuzzing
// shape of the vacuity question every test in this package is asked: a target
// whose seeds all fail to decode passes forever and asserts nothing, and it looks
// identical from the outside to one finding no defects. The counters are read
// through pointers because they are only filled while the target runs.
func gateOnReachability(f *testing.F, construct string, reached *int, executed *int) {
	f.Helper()
	f.Cleanup(func() {
		if fuzzingEngineSuppliesInputs() {
			return
		}
		f.Logf("%d of %d seed inputs reached %s", *reached, *executed, construct)
		if *reached == 0 {
			f.Errorf("no seed input of %d reached %s, so this target asserted nothing", *executed, construct)
		}
	})
}

// summarize renders an input for a failure message without pasting a sixteen
// kilobyte body into the log; the length is what identifies which seed it was and
// the head is what shows the prefix that went wrong.
func summarize(bs []byte) string {
	head := bs[:min(len(bs), 48)]
	return string(head)
}

// varintSeeds are RFC 9420 section 2.1.2 prefixes covering the neighbourhood a
// mutation has to start from: each width at both ends of its range, the non
// minimal encoding of a value that fits narrower, the reserved prefix, and a
// prefix truncated in the middle. The four octet forms matter most — a uniform
// draw over short byte strings reaches a valid four octet varint about one time in
// a thousand, and the minimality rule for that width is the one an attacker would
// use to re-encode a signed structure.
func varintSeeds() [][]byte {
	return [][]byte{
		// nothing at all, the smallest truncation
		{},
		// one octet form at both ends
		{0x00}, {0x01}, {0x3f},
		// two octet form at both ends
		{0x40, 0x40}, {0x40, 0x41}, {0x7f, 0xff},
		// four octet form at both ends
		{0x80, 0x00, 0x40, 0x00}, {0x80, 0x00, 0x40, 0x01}, {0xbf, 0xff, 0xff, 0xff},
		// non minimal: values that fit in a narrower width, encoded wide
		{0x40, 0x00}, {0x40, 0x3f}, {0x80, 0x00, 0x00, 0x00}, {0x80, 0x00, 0x3f, 0xff},
		// the reserved prefix 0b11, which no value may use
		{0xc0}, {0xff, 0xff, 0xff, 0xff},
		// promised more octets than are there
		{0x40}, {0x80}, {0x80, 0x00, 0x40},
		// a valid prefix with a tail behind it, since this target reads one varint
		// and does not require full consumption
		{0x3f, 0xaa, 0xbb}, {0x40, 0x40, 0xaa},
	}
}

// opaqueSeeds are encodings of both length prefixed forms this package carries:
// MLS's opaque x<V> with the varint prefix, and the record layer's LP(x) with the
// fixed 32 bit big endian one. Bodies sit at 0, 1, 63, 64 and 65 octets because 63
// and 64 are where the varint prefix changes width, which is the only place a
// prefix boundary bug is observable, and both forms are seeded in one corpus
// because the target runs both decoders over every input — an input that is a
// valid opaque is usually a hostile LP prefix and the other way round, which is
// free coverage.
//
// The bytes are literals rather than Writer output on purpose. A seed built by the
// encoder under test moves when the encoder does, so an encoder mutant would
// quietly rewrite its own corpus; a literal pins the wire format independently of
// the code that produces it, which is what lets these seeds catch an encoder bug
// in the seed pass rather than only under mutation.
func opaqueSeeds() [][]byte {
	seeds := [][]byte{
		// empty and truncated
		{}, {0x00}, {0x01},
		// varint prefixed bodies below the width boundary
		{0x01, 0xaa}, {0x03, 0xaa, 0xbb, 0xcc},
		// a varint prefix promising more than is there
		{0x40, 0x40}, {0x02, 0xaa},
		// non minimal and reserved prefixes, which no body makes acceptable
		{0x40, 0x00, 0xaa}, {0xc0, 0x00, 0x00, 0x00},
		// four gibibytes declared, nothing supplied: the allocation before
		// validation case in both forms
		{0xbf, 0xff, 0xff, 0xff}, {0xff, 0xff, 0xff, 0xff},
		// LP prefixed: empty, one octet, and a prefix with no body
		{0x00, 0x00, 0x00, 0x00}, {0x00, 0x00, 0x00, 0x01, 0xaa},
		{0x00, 0x00, 0x00, 0x02, 0xaa},
		// a valid body with a tail behind it, since neither decoder here requires
		// full consumption
		{0x01, 0xaa, 0xbb, 0xcc},
	}
	// the varint width boundary, both sides, in the form that carries a body
	seeds = append(seeds, append([]byte{0x3f}, bytes.Repeat(filler, 63)...))
	seeds = append(seeds, append([]byte{0x40, 0x40}, bytes.Repeat(filler, 64)...))
	seeds = append(seeds, append([]byte{0x40, 0x41}, bytes.Repeat(filler, 65)...))
	// the same lengths under the fixed width prefix, where no boundary exists and
	// the byte order is what can be wrong instead
	seeds = append(seeds, append([]byte{0x00, 0x00, 0x00, 0x3f}, bytes.Repeat(filler, 63)...))
	seeds = append(seeds, append([]byte{0x00, 0x00, 0x00, 0x40}, bytes.Repeat(filler, 64)...))
	seeds = append(seeds, append([]byte{0x00, 0x00, 0x00, 0x41}, bytes.Repeat(filler, 65)...))
	return seeds
}

// minimalStructSeed is a hand written encoding of the zero value testStruct, the
// one struct seed that owes nothing to the encoder: two octets of version, one of
// flags, eight of counter, four fixed, an empty opaque, an empty LP field, an
// absent optional, an empty vector, and a three octet nested region holding a
// testItem whose own opaque field is empty. Everything else in structSeeds goes
// through Marshal, which is unavoidable for a structure of this size and is also
// exactly the circularity this one input breaks: if the encoder's framing drifts,
// the seeds drift with it and this literal does not.
var minimalStructSeed = []byte{
	0x00, 0x00,
	0x00,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00,
	0x00,
	0x00, 0x00, 0x00, 0x00,
	0x00,
	0x00,
	0x03, 0x00, 0x00, 0x00,
}

// structSeeds are encodings of testStruct covering the neighbourhood a mutation
// works outward from: the hand written minimum, one structure per branch of every
// construct the type carries, the varint width boundary in the opaque field and in
// the vector's own length, and draws from the task 17 generator so that the corpus
// is not only the cases somebody thought of. The generated draws are capped at four
// kilobytes because a corpus of large entries costs the fuzzer an execution rate
// without buying a construct, and the one deliberately large seed below covers the
// four octet prefix that the cap would otherwise exclude.
func structSeeds(t testing.TB) [][]byte {
	t.Helper()
	encode := func(s testStruct) []byte {
		bs, err := Marshal(&s)
		if err != nil {
			t.Fatalf("encoding a seed structure gave %v", err)
		}
		return bs
	}
	item := func(n int) testItem {
		return testItem{
			Kind: 0x1234,
			Data: bytes.Repeat(filler, n),
		}
	}
	seeds := [][]byte{minimalStructSeed}
	structures := []testStruct{
		// every fixed width field carrying a value the zero one would hide
		{Version: 0xfeed, Flags: 0xff, Counter: ^uint64(0), Fixed: [4]byte{0xde, 0xad, 0xbe, 0xef}},
		// the optional in both directions
		{HasExtra: true, Extra: 0xcafebabe},
		{HasExtra: false, Extra: 0},
		// the opaque field either side of the varint width boundary
		{Body: bytes.Repeat(filler, 63)},
		{Body: bytes.Repeat(filler, 64)},
		{Body: bytes.Repeat(filler, 65)},
		// the LP prefixed field, empty and not
		{Tail: bytes.Repeat(filler, 1)},
		{Tail: bytes.Repeat(filler, 300)},
		// the vector empty, at one element, and at several
		{Items: []testItem{}},
		{Items: []testItem{item(0)}},
		{Items: []testItem{item(0), item(1), item(64)}},
		// the vector's own length prefix either side of the width boundary: a
		// testItem encodes to 3 octets plus its body, so 60 and 61 octet bodies put
		// the region at 63 and 64
		{Items: []testItem{item(60)}},
		{Items: []testItem{item(61)}},
		// the nested structure with an empty and a non empty opaque inside it
		{Nested: item(0)},
		{Nested: item(64)},
		// everything at once, which is the only seed where a mutation can land in
		// one construct while the others stay valid around it
		{
			Version:  0x0102,
			Flags:    0x03,
			Counter:  0x0405060708090a0b,
			Fixed:    [4]byte{0x0c, 0x0d, 0x0e, 0x0f},
			Body:     bytes.Repeat(filler, 64),
			Tail:     bytes.Repeat(filler, 5),
			HasExtra: true,
			Extra:    0x10111213,
			Items:    []testItem{item(1), item(63)},
			Nested:   item(2),
		},
		// the four octet varint prefix, which nothing under the cap below reaches
		{Body: bytes.Repeat(filler, 16384)},
	}
	for _, s := range structures {
		seeds = append(seeds, encode(s))
	}
	rng := newTestRand(roundTripSeed)
	for i := 0; i < 64; i += 1 {
		bs := encode(generateTestStruct(rng))
		if len(bs) > 4096 {
			continue
		}
		seeds = append(seeds, bs)
	}
	return seeds
}

// checkVarintCanonical is gate 4 properties 1 and 2 over one varint, and reports
// whether the input decoded — which is to say whether the assertions ran at all.
// The re-encode is compared against the octets the decoder consumed rather than
// against the whole input, because this reads one varint and leaves any tail
// alone; equality there is the canonical encoding rule, that a value the decoder
// accepted has exactly one encoding and this is it.
func checkVarintCanonical(t *testing.T, bs []byte) bool {
	t.Helper()
	r := NewReader(bs)
	v, err := r.ReadVarint()
	if err != nil {
		return false
	}
	if v > MaxVarint {
		t.Fatalf("decoded %d above MaxVarint from %x", v, bs)
	}
	consumed := r.Offset()
	if consumed < 1 || consumed > 4 {
		t.Fatalf("consumed %d octets decoding %d from %x", consumed, v, bs)
	}
	w := NewWriter()
	w.WriteVarint(v)
	out, err := w.Bytes()
	if err != nil {
		t.Fatalf("re-encoding %d gave %v", v, err)
	}
	if !bytes.Equal(out, bs[:consumed]) {
		t.Fatalf("%x decoded to %d but re-encoded to %x; the encoding is not canonical", bs[:consumed], v, out)
	}
	return true
}

// checkOpaqueCanonical is the same property over one length prefixed form, and
// reports whether the input decoded. The two forms share this body because they
// differ only in how the length is spelled and the property over them is
// identical: whatever the decoder accepted must re-encode to the octets it
// consumed, prefix included. Writing it twice would let the two drift, and the
// prefix is the part that must not be got wrong twice.
func checkOpaqueCanonical(t *testing.T, form string, bs []byte, readOne func(r *Reader) ([]byte, error), writeOne func(w *Writer, body []byte)) bool {
	t.Helper()
	r := NewReader(bs)
	body, err := readOne(r)
	if err != nil {
		return false
	}
	consumed := r.Offset()
	w := NewWriter()
	writeOne(w, body)
	out, err := w.Bytes()
	if err != nil {
		t.Fatalf("%s: re-encoding a %d byte body gave %v", form, len(body), err)
	}
	if !bytes.Equal(out, bs[:consumed]) {
		t.Fatalf("%s: %x decoded to a %d byte body and re-encoded to %x; the encoding is not canonical", form, bs[:consumed], len(body), out)
	}
	return true
}

// checkStructRoundTrip is gate 4 property 2 over a whole structure, and reports
// whether the input decoded. CheckRoundTrip is what asserts, so that this target
// and the five in the validation and interop plan share one definition of "round
// trips"; the separate Unmarshal is what measures, because CheckRoundTrip returns
// nil both for an input that round tripped and for one it never decoded, and those
// two are the difference between a target that works and a target that cannot
// fail. The predicate is the identical call CheckRoundTrip makes internally, so
// the count is what the property saw rather than a model of it.
//
// The bound is the default MaxVectorLength rather than CheckRoundTripLimit's
// raised one, because testStruct carries no ratchet tree: its widest field is a
// 16 kilobyte opaque, three orders of magnitude under the default cap, so nothing
// it can hold decodes only under the raised bound. Raising it here would widen
// what the fuzzer may allocate from one input without making one further input
// reachable.
func checkStructRoundTrip(t *testing.T, bs []byte) bool {
	t.Helper()
	out := testStruct{}
	decoded := Unmarshal(bs, &out) == nil
	if err := CheckRoundTrip[testStruct, *testStruct](bs); err != nil {
		t.Fatalf("round trip failed on %d bytes starting %x: %v", len(bs), summarize(bs), err)
	}
	return decoded
}

// FuzzVarint asserts that every varint the decoder accepts re-encodes to exactly
// the octets it consumed. That is the canonical encoding rule at its narrowest
// point: the varint is the prefix under every opaque field, every vector and every
// nested region in MLS, so a second accepted encoding of one length is a second
// encoding of every structure built on it.
func FuzzVarint(f *testing.F) {
	reached, executed := 0, 0
	gateOnReachability(f, "a decoded varint", &reached, &executed)
	for _, seed := range varintSeeds() {
		f.Add(seed)
	}
	addSharedCorpus(f)
	f.Fuzz(func(t *testing.T, bs []byte) {
		executed += 1
		if checkVarintCanonical(t, bs) {
			reached += 1
		}
	})
}

// FuzzOpaque asserts the same over both length prefixed forms, running each
// decoder over every input. The two are never interchangeable on the wire —
// opaque x<V> is MLS's and LP(x) is the record layer's — but they are one property
// and one corpus here, because an input crafted to be a hostile prefix for one is
// an interesting near miss for the other at no cost.
func FuzzOpaque(f *testing.F) {
	reachedV, reachedLP, executed := 0, 0, 0
	gateOnReachability(f, "a decoded opaque<V>", &reachedV, &executed)
	gateOnReachability(f, "a decoded LP opaque", &reachedLP, &executed)
	for _, seed := range opaqueSeeds() {
		f.Add(seed)
	}
	addSharedCorpus(f)
	f.Fuzz(func(t *testing.T, bs []byte) {
		executed += 1
		varintForm := func(r *Reader) ([]byte, error) { return r.ReadOpaque() }
		lpForm := func(r *Reader) ([]byte, error) { return r.ReadOpaqueLP() }
		writeVarintForm := func(w *Writer, body []byte) { w.WriteOpaque(body) }
		writeLPForm := func(w *Writer, body []byte) { w.WriteOpaqueLP(body) }
		if checkOpaqueCanonical(t, "opaque<V>", bs, varintForm, writeVarintForm) {
			reachedV += 1
		}
		if checkOpaqueCanonical(t, "LP", bs, lpForm, writeLPForm) {
			reachedLP += 1
		}
	})
}

// FuzzSyntaxStruct asserts the property over a structure carrying one field per
// primitive the package offers, which is where the constructs interact: a length
// prefix nested inside a length prefixed region, an optional beside a vector, a
// fixed width prefix beside a varint one. It is also the target that can find
// decoder leniency, and the only one whose seeds cannot: task 17 showed that a
// round trip over self encoded values misses a ReadOptional accepting a presence
// octet of 2, because nothing this package encodes produces one. Mutation of these
// seeds does produce one, and the property then fails on the re-encode.
func FuzzSyntaxStruct(f *testing.F) {
	reached, executed := 0, 0
	gateOnReachability(f, "a decoded testStruct", &reached, &executed)
	for _, seed := range structSeeds(f) {
		f.Add(seed)
	}
	addSharedCorpus(f)
	f.Fuzz(func(t *testing.T, bs []byte) {
		executed += 1
		if checkStructRoundTrip(t, bs) {
			reached += 1
		}
	})
}

// fuzzSeedCorpora is every target's seed corpus paired with the property it feeds,
// so the reachability the targets gate on is also measured here in a plain test
// that runs on every commit whatever mode the fuzzer is in. wantAll separates the
// two kinds of corpus: the struct seeds are all valid encodings and every one of
// them must decode, since a seed of its own type that does not is a generator or
// codec defect either way, while the varint and opaque corpora deliberately carry
// truncations, reserved prefixes and non minimal forms that must be rejected.
var fuzzSeedCorpora = []struct {
	name    string
	seeds   func(t testing.TB) [][]byte
	reaches func(t *testing.T, bs []byte) bool
	wantAll bool
}{
	{
		name:    "FuzzVarint",
		seeds:   func(t testing.TB) [][]byte { return varintSeeds() },
		reaches: checkVarintCanonical,
		wantAll: false,
	},
	{
		name:  "FuzzOpaque, the varint prefixed form",
		seeds: func(t testing.TB) [][]byte { return opaqueSeeds() },
		reaches: func(t *testing.T, bs []byte) bool {
			return checkOpaqueCanonical(t, "opaque<V>", bs,
				func(r *Reader) ([]byte, error) { return r.ReadOpaque() },
				func(w *Writer, body []byte) { w.WriteOpaque(body) })
		},
		wantAll: false,
	},
	{
		name:  "FuzzOpaque, the LP prefixed form",
		seeds: func(t testing.TB) [][]byte { return opaqueSeeds() },
		reaches: func(t *testing.T, bs []byte) bool {
			return checkOpaqueCanonical(t, "LP", bs,
				func(r *Reader) ([]byte, error) { return r.ReadOpaqueLP() },
				func(w *Writer, body []byte) { w.WriteOpaqueLP(body) })
		},
		wantAll: false,
	},
	{
		name:    "FuzzSyntaxStruct",
		seeds:   structSeeds,
		reaches: checkStructRoundTrip,
		wantAll: true,
	},
}

// TestFuzzSeedsReachTheirProperty is the measurement task 14 makes necessary for
// this file, and the number it prints is the one that says whether these targets
// are worth running. Each corpus goes through the identical property function its
// target runs, and the count is of inputs that reached the assertions rather than
// of inputs fed. Zero anywhere means a target that passes forever while asserting
// nothing — which is what a target seeded only with random bytes would be, at 0.34
// percent reachability on a structure far simpler than testStruct.
func TestFuzzSeedsReachTheirProperty(t *testing.T) {
	for _, corpus := range fuzzSeedCorpora {
		seeds := corpus.seeds(t)
		reached := 0
		for _, bs := range seeds {
			if corpus.reaches(t, bs) {
				reached += 1
			}
		}
		t.Logf("%s: %d of %d seeds reached the property", corpus.name, reached, len(seeds))
		if reached == 0 {
			t.Errorf("%s: no seed reached the property, so the target asserts nothing", corpus.name)
		}
		if corpus.wantAll && reached != len(seeds) {
			t.Errorf("%s: %d of %d seeds decoded, want every one; a seed of the target's own type that will not decode is a defect", corpus.name, reached, len(seeds))
		}
		if !corpus.wantAll && reached == len(seeds) {
			t.Errorf("%s: every one of %d seeds decoded, so the corpus carries none of the hostile inputs it claims to", corpus.name, len(seeds))
		}
	}
}

// TestFuzzSeedsStaySmallEnoughToMutate guards the other half of a corpus being
// worth anything. Seeds buy reachability, mutation buys the leniency probing that
// no round trip over self encoded values can reach, and a corpus of megabyte
// entries buys the first at the cost of the second by dragging the fuzzer's
// execution rate down. The cap is generous — the largest seed here is the one
// deliberately carrying a four octet length prefix — and what it is really
// watching for is a later edit that seeds from a generator without a bound.
func TestFuzzSeedsStaySmallEnoughToMutate(t *testing.T) {
	const seedSizeCap = 1 << 15
	for _, corpus := range fuzzSeedCorpora {
		seeds := corpus.seeds(t)
		total, largest := 0, 0
		for _, bs := range seeds {
			total += len(bs)
			largest = max(largest, len(bs))
		}
		t.Logf("%s: %d seeds, %d bytes total, largest %d", corpus.name, len(seeds), total, largest)
		if largest > seedSizeCap {
			t.Errorf("%s: largest seed is %d bytes, over the %d cap; the fuzzer will spend its budget copying it", corpus.name, largest, seedSizeCap)
		}
	}
}

// TestSharedCorpusLoaderToleratesAnAbsentDirectory covers the loader against the
// two states it is actually in. The shared corpus belongs to the validation and
// interop plan, so on a checkout of this wave it does not exist and every target
// here must still run; once that plan lands it does, and the loader must return
// what is in it. Both are exercised against temporary directories rather than
// against the real path, so this test says the same thing before and after that
// plan lands instead of quietly changing meaning.
func TestSharedCorpusLoaderToleratesAnAbsentDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-corpus")
	if entries := readCorpusDir(missing); entries != nil {
		t.Errorf("an absent corpus gave %d entries, want none; a fresh checkout has no corpus and must still fuzz", len(entries))
	}
	present := t.TempDir()
	written := [][]byte{{}, {0x00}, {0x01, 0xaa, 0xbb}}
	for i, bs := range written {
		name := filepath.Join(present, string(rune('a'+i)))
		if err := os.WriteFile(name, bs, 0o600); err != nil {
			t.Fatalf("writing a corpus entry gave %v", err)
		}
	}
	if err := os.Mkdir(filepath.Join(present, "nested"), 0o700); err != nil {
		t.Fatalf("making a nested directory gave %v", err)
	}
	entries := readCorpusDir(present)
	if len(entries) != len(written) {
		t.Fatalf("a corpus of %d files and one directory gave %d entries, want %d", len(written), len(entries), len(written))
	}
	for i, bs := range written {
		if !bytes.Equal(entries[i], bs) {
			t.Errorf("entry %d read back as %x, want %x", i, entries[i], bs)
		}
	}
}

// TestMinimalStructSeedIsTheHandWrittenEncoding pins the one seed that owes
// nothing to the encoder. Every other struct seed comes out of Marshal, so a
// framing change would move the seeds and the property with it and nothing would
// notice; this compares the literal against what the encoder produces for the zero
// value, which fails on exactly that drift, and separately against the decoder, so
// a literal that stopped being a valid encoding cannot sit in the corpus reaching
// nothing.
func TestMinimalStructSeedIsTheHandWrittenEncoding(t *testing.T) {
	zero := testStruct{}
	encoded, err := Marshal(&zero)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(encoded, minimalStructSeed) {
		t.Errorf("the zero value encoded to %x, want the hand written %x", encoded, minimalStructSeed)
	}
	out := testStruct{}
	if err := Unmarshal(minimalStructSeed, &out); err != nil {
		t.Errorf("the hand written seed does not decode: %v", err)
	}
}
