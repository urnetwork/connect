// Gate 4 properties 1 and 2, deterministically and over a corpus that is on disk, for the
// two structures the key schedule owns: no panic on adversarial input, and byte exact
// round trip stability. MLS signs over serialized forms, so a decoder that accepts two
// encodings of one object is a signature bypass primitive rather than a leniency.
//
// The randomized form of these properties is FuzzGroupContextRoundTrip and
// FuzzPreSharedKeyIdRoundTrip, declared at the foot of this file. They are declared here, and
// this is the correction of a real defect rather than a preference: the interface registry's
// nine Gate 4 targets cover the five wire structures p8 owns -- extension, key package, mls
// message, proposal, welcome -- and neither of these two is among them. Committing a corpus
// under a target name no plan declares gives you 287 files no fuzz engine ever opens, every
// property below stated over bytes nothing consumes, and a verification step that cannot be
// run. The gate that keeps that from coming back is
// TestEveryCommittedCorpusFolderIsReadByAFuzzTarget.
//
// So this file owns both halves: the committed seed corpus, and the two targets that read it.
//
// The corpus is the load bearing half, and p1 measured why. Uniform random bytes reach the
// round trip property 14 times in 4096 -- 0.34 percent -- against the SIMPLEST type in the
// tree, because the length prefix rejects them first; a structured generator reaches it
// 4096 times in 4096. A target seeded only with random bytes therefore spends its budget
// rediscovering the varint, and a seed corpus is not an optimisation.
//
// Two consequences run through everything below. Every property here counts what it
// actually reached and fails at zero, because a target that decoded nothing reports exactly
// what a target that decoded everything reports. And the corpus is COMMITTED and compared
// against the generator rather than regenerated per run: a corpus regenerated from the same
// encoder it checks agrees with that encoder by construction, so a field dropped from both
// halves of the codec would round trip perfectly and change nothing anybody could see. The
// files on disk were written by yesterday's encoder, which is what makes them evidence.
package mls

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"math"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect/mls/syntax"
)

// The two p8 target names. The folder is named for the TARGET rather than for the structure
// because p8's loader joins testdata/corpus with the target's own name, and the seeds have
// to be where the thing that reads them looks.
const (
	groupContextSeedTarget   = "FuzzGroupContextRoundTrip"
	preSharedKeyIdSeedTarget = "FuzzPreSharedKeyIdRoundTrip"
)

// seedCorpusWriteEnv rewrites the committed corpus from the generator. A run with it set
// never reports success -- see TestTheCommittedSeedCorpusIsExactlyTheGeneratedCorpus --
// because a run that rewrites the artefact it is checking has checked nothing.
const seedCorpusWriteEnv = "URMSG_MLS_WRITE_CORPUS"

// seedWideOpaqueLength is the first length whose varint prefix needs four octets. One seed
// carries it per target rather than a whole axis: every seed carrying it costs 16 KiB in
// the repository, and the prefix width is a property of the prefix rather than of the field
// in front of it.
const seedWideOpaqueLength = 16384

// seedCorpusDirectory is where one target's seeds live.
func seedCorpusDirectory(target string) string {
	return filepath.Join("testdata", "corpus", target)
}

// ---------------------------------------------------------------------------
// the axes
// ---------------------------------------------------------------------------

// seedOpaqueLengths are the lengths the RFC 9420 section 2.1.2 varint length prefix
// branches on: absent, one octet, the last length a one octet prefix can express, and the
// first that needs two. These are the shapes p1's measurement says a fuzzer cannot find on
// its own in any reasonable budget, which is the whole reason the corpus exists.
func seedOpaqueLengths() []int {
	return []int{0, 1, 63, 64}
}

// seedOpaques is that axis as field values. The zero length case is nil rather than empty
// so the generated VALUES carry the distinction the wire format does not; the empty non nil
// form is added separately, since on disk the two are one seed.
func seedOpaques() [][]byte {
	opaques := [][]byte{}
	for index, length := range seedOpaqueLengths() {
		if length == 0 {
			opaques = append(opaques, nil)
			continue
		}
		opaques = append(opaques, repeatByte(byte(0xa0+index), length))
	}
	return opaques
}

// seedEpochs are the uint64 boundaries: zero, one, the last value a uint32 holds, the first
// it does not, the sign bit, and the maximum. Those are the values a field narrowed to 32
// bits, or read as signed, would move -- and both epoch fields here are covered by a
// signature, so a value that moves is a member deriving a different secret.
func seedEpochs() []uint64 {
	return []uint64{0, 1, math.MaxUint32, uint64(math.MaxUint32) + 1, 1 << 63, math.MaxUint64}
}

// seedGroupContexts is the cross product of the registry enums with the varint width
// boundaries of the opaque fields, the uint64 boundaries of the epoch, and the extension
// vector shapes.
//
// The enum axes are DERIVED from the package's own constant declarations rather than
// listed, for the reason rule 5 names: a hand picked table is a claim about which cases
// matter made by whoever also wrote the code, and on this project that claim has understated
// the real class fourteen times. A code point added to any of these registries enters this
// corpus in the commit that declares it, and the coverage gate below turns a corpus that has
// not caught up red.
func seedGroupContexts(t *testing.T) []*GroupContext {
	t.Helper()

	// 0xffff is not a registry member and is here for the opposite reason to the derived
	// ones: the codec does not decide policy, so an unregistered code point has to survive
	// a round trip for validation to be able to refuse it by name.
	versions := append(sortedValues(registryConstantsOfType(t, "ProtocolVersion")), 0xffff)
	suites := append(sortedValues(registryConstantsOfType(t, "CipherSuite")), 0xffff)
	extensionTypes := sortedValues(registryConstantsOfType(t, "ExtensionType"))

	opaques := seedOpaques()
	epochs := seedEpochs()

	// the extension vector shapes: the absent vector, one entry per derived code point,
	// the body lengths the varint branches on, and a two entry vector -- the vector length
	// prefix counts BYTES rather than elements, and a corpus of single element vectors
	// cannot tell those two readings apart.
	first := ExtensionType(extensionTypes[0])
	last := ExtensionType(extensionTypes[len(extensionTypes)-1])
	extensionLists := [][]Extension{nil}
	for _, extensionType := range extensionTypes {
		extensionLists = append(extensionLists, []Extension{
			{ExtensionType: ExtensionType(extensionType), ExtensionData: repeatByte(0xb1, 3)},
		})
	}
	extensionLists = append(extensionLists,
		[]Extension{{ExtensionType: first, ExtensionData: nil}},
		[]Extension{{ExtensionType: first, ExtensionData: repeatByte(0xb2, 63)}},
		[]Extension{{ExtensionType: first, ExtensionData: repeatByte(0xb3, 64)}},
		[]Extension{
			{ExtensionType: first, ExtensionData: repeatByte(0xb4, 3)},
			{ExtensionType: last, ExtensionData: nil},
		},
	)

	corpus := []*GroupContext{}
	rotation := 0
	for _, version := range versions {
		for _, suite := range suites {
			for groupIdIndex, groupId := range opaques {
				for epochIndex, epoch := range epochs {
					// the three opaque fields and the extension vector rotate through
					// their own axes rather than being crossed with each other, which
					// would raise the corpus to a power to buy nothing: they go through
					// the same encoder, so a length that works in one position works in
					// all of them. Rotating puts every value of every axis in every
					// position across the product, which is what the coverage gate below
					// then asserts rather than assumes.
					corpus = append(corpus, &GroupContext{
						Version:                 ProtocolVersion(version),
						CipherSuite:             CipherSuite(suite),
						GroupId:                 groupId,
						Epoch:                   epoch,
						TreeHash:                opaques[(groupIdIndex+epochIndex)%len(opaques)],
						ConfirmedTranscriptHash: opaques[(groupIdIndex+epochIndex+1)%len(opaques)],
						Extensions:              extensionLists[rotation%len(extensionLists)],
					})
					rotation++
				}
			}
		}
	}

	// the empty non nil field. It is a different go value from the absent one and the same
	// bytes on the wire, so it is here for the value direction; on disk the dedupe folds it
	// into the nil case, which is correct -- one encoding is one seed.
	corpus = append(corpus, &GroupContext{
		Version:                 ProtocolVersion(versions[0]),
		CipherSuite:             CipherSuite(suites[0]),
		GroupId:                 []byte{},
		Epoch:                   0,
		TreeHash:                []byte{},
		ConfirmedTranscriptHash: []byte{},
		Extensions:              []Extension{},
	})
	corpus = append(corpus, &GroupContext{
		Version:                 ProtocolVersion(versions[0]),
		CipherSuite:             CipherSuite(suites[0]),
		GroupId:                 repeatByte(0xb5, seedWideOpaqueLength),
		Epoch:                   1 << 40,
		TreeHash:                repeatByte(0xb6, 32),
		ConfirmedTranscriptHash: repeatByte(0xb7, 32),
		Extensions:              []Extension{{ExtensionType: first, ExtensionData: repeatByte(0xb8, 3)}},
	})
	return corpus
}

// seedPreSharedKeyIds is the same product over the section 8.4 id: both arms of the
// select(), every derived usage code point, the varint width boundaries and the uint64
// boundaries of the epoch.
//
// Each entry is in canonical form for its arm -- the fields the arm does not encode are left
// at their zero values -- so that "decoding reproduces every field" is a claim the corpus can
// actually make.
func seedPreSharedKeyIds(t *testing.T) []*PreSharedKeyId {
	t.Helper()

	pskTypes := registryConstantsOfType(t, "PskType")
	external, isExternal := pskTypes["PskTypeExternal"]
	resumption, isResumption := pskTypes["PskTypeResumption"]
	if !isExternal || !isResumption {
		t.Fatalf("the derivation read %v, which does not name both arms of the section 8.4 select(); a corpus built from it would exercise one decoder", pskTypes)
	}

	// the usage axis is the derived registry plus the two ends of the octet, because the
	// codec carries usage opaquely: a value outside the registry has to survive a round trip
	// for ValSem402 to be able to refuse it by name.
	usages := append(sortedValues(registryConstantsOfType(t, "ResumptionPskUsage")), 0x00, 0xff)
	slices.Sort(usages)

	opaques := seedOpaques()
	// KDF.Nh is the only nonce length ValSem401 accepts and therefore the only one that
	// occurs in production, so it sits on the axis beside the varint boundaries.
	nonces := append(append([][]byte{}, opaques...), repeatByte(0xc0, 32))
	epochs := seedEpochs()

	corpus := []*PreSharedKeyId{}
	for _, pskId := range opaques {
		for _, nonce := range nonces {
			corpus = append(corpus, &PreSharedKeyId{
				PskType:  PskType(external),
				PskId:    pskId,
				PskNonce: nonce,
			})
		}
	}
	for _, usage := range usages {
		for groupIdIndex, groupId := range opaques {
			for epochIndex, epoch := range epochs {
				corpus = append(corpus, &PreSharedKeyId{
					PskType:    PskType(resumption),
					Usage:      ResumptionPskUsage(usage),
					PskGroupId: groupId,
					PskEpoch:   epoch,
					PskNonce:   nonces[(groupIdIndex+epochIndex)%len(nonces)],
				})
			}
		}
	}
	corpus = append(corpus,
		&PreSharedKeyId{PskType: PskType(external), PskId: []byte{}, PskNonce: []byte{}},
		&PreSharedKeyId{
			PskType:  PskType(external),
			PskId:    repeatByte(0xc1, seedWideOpaqueLength),
			PskNonce: repeatByte(0xc2, 32),
		},
	)
	return corpus
}

// ---------------------------------------------------------------------------
// the two codecs, as one table
// ---------------------------------------------------------------------------

// seedCodec is one structure's half of this file: its target name, its generator, and the
// three entry points every property below reaches it through. The table exists so each
// property is written once over both structures rather than twice over one, which is how the
// second copy comes to assert less than the first.
type seedCodec struct {
	target string
	// structure is a fresh zero value of the structure this codec encodes. It is what the
	// coverage gates derive their class from: the registries reachable on the wire and the
	// fields the corpus has to vary both come off this type rather than off a list.
	structure func() any
	// project, when non-nil, answers the exported values the derived walks read in place of the
	// decoded structure itself.
	//
	// It exists for one structure and it is not a general escape hatch. RatchetTree's node array is
	// UNEXPORTED, and every walk below fails rather than skips on a field it cannot read -- on
	// purpose, since a field a comparison cannot read is one it reports equal whatever it holds --
	// so without this the tree codec would fail every gate for a reason that has nothing to do with
	// its corpus. What a projection hands over is what the container already publishes through its
	// own accessor, one element per node, and nothing else; it is a READING of the value and never a
	// second codec, which is why encode and decode still go through the container's own pair.
	project        func(value any) []any
	values         func(t *testing.T) []any
	decode         func(bs []byte) (any, error)
	encode         func(value any) ([]byte, error)
	checkRoundTrip func(bs []byte) error
	describe       func(value any) string
}

// seedCorpusReachabilityConstructor is the name of the method below, spelled once because the gate
// that requires every target to reach its reachability answer through it looks for that name.
const seedCorpusReachabilityConstructor = "roundTripProperty"

// roundTripProperty is the property every committed corpus target states, built out of the codec's
// own two entry points rather than written out again at each target.
//
// Both halves come off the table on purpose. The round trip half was already shared, through
// checkRoundTrip; the REACHABILITY half was not, and five targets each answered it with a locally
// written expression that nothing observed. Replacing one of those answers with a bare `return
// true` -- a target reporting that every input it saw reached a decoder, over a corpus that might
// reach none -- left the whole suite green, which is precisely the failure the count was added to
// expose. Answering out of codec.decode makes the answer a property of the table, and
// TestEveryCommittedCorpusCodecAnswersReachabilityFromItsOwnDecoder is the control on it.
func (self seedCodec) roundTripProperty() func(t *testing.T, encoded []byte) bool {
	return func(t *testing.T, encoded []byte) bool {
		if err := self.checkRoundTrip(encoded); err != nil {
			t.Fatalf("%s: %d octets %x: %v", self.target, len(encoded), encoded, err)
		}
		_, err := self.decode(encoded)
		return err == nil
	}
}

// seedCodecFor is one target's entry in the shared table. A target with no entry is a failure and
// not a fallback: every property this package states over a corpus is stated through that table,
// so a target outside it is a corpus nothing but the fuzzing engine ever reads.
func seedCodecFor(t testing.TB, target string) seedCodec {
	t.Helper()
	for _, codec := range seedCodecs() {
		if codec.target == target {
			return codec
		}
	}
	t.Fatalf("%s names no entry of the shared codec table", target)
	return seedCodec{}
}

// seedCodecs is every structure this package states the corpus properties over. p4's two are here,
// p5's two are in tree_roundtrip_test.go and p6's one is in framing_guard_test.go, joined rather
// than kept in three tables: each property below is written once over the table, and a second
// table is a second place for the same property to be stated more weakly.
func seedCodecs() []seedCodec {
	return slices.Concat(keyScheduleSeedCodecs(), treeSeedCodecs(), framingSeedCodecs())
}

func keyScheduleSeedCodecs() []seedCodec {
	return []seedCodec{
		{
			target:    groupContextSeedTarget,
			structure: func() any { return &GroupContext{} },
			values: func(t *testing.T) []any {
				values := []any{}
				for _, value := range seedGroupContexts(t) {
					values = append(values, value)
				}
				return values
			},
			decode: func(bs []byte) (any, error) {
				parsed := &GroupContext{}
				return parsed, syntax.Unmarshal(bs, parsed)
			},
			encode:         func(value any) ([]byte, error) { return syntax.Marshal(value.(*GroupContext)) },
			checkRoundTrip: syntax.CheckRoundTrip[GroupContext, *GroupContext],
			describe:       func(value any) string { return describeGroupContext(value.(*GroupContext)) },
		},
		{
			target:    preSharedKeyIdSeedTarget,
			structure: func() any { return &PreSharedKeyId{} },
			values: func(t *testing.T) []any {
				values := []any{}
				for _, value := range seedPreSharedKeyIds(t) {
					values = append(values, value)
				}
				return values
			},
			decode: func(bs []byte) (any, error) {
				parsed := &PreSharedKeyId{}
				return parsed, syntax.Unmarshal(bs, parsed)
			},
			encode:         func(value any) ([]byte, error) { return syntax.Marshal(value.(*PreSharedKeyId)) },
			checkRoundTrip: syntax.CheckRoundTrip[PreSharedKeyId, *PreSharedKeyId],
			describe:       func(value any) string { return describePreSharedKeyId(value.(*PreSharedKeyId)) },
		},
	}
}

// seedFileName names the index'th seed. Ordinal rather than content addressed, because the
// generator's order is deterministic and a stable name makes a diff of this folder readable.
func seedFileName(index int) string {
	return fmt.Sprintf("seed%03d", index+1)
}

// generatedSeedFiles is the corpus this file would write, as it would appear on disk.
//
// Two values that differ only in nil versus empty are one encoding, and one encoding is one
// seed: a fuzzer handed the same bytes twice learns nothing the second time, and a corpus
// that grew by duplicates would report a count without carrying information.
func generatedSeedFiles(t *testing.T, codec seedCodec) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	seen := map[string]bool{}
	for index, value := range codec.values(t) {
		encoded, err := codec.encode(value)
		if err != nil {
			t.Fatalf("%s: value %d %s: encode: %v", codec.target, index, codec.describe(value), err)
		}
		if seen[string(encoded)] {
			continue
		}
		seen[string(encoded)] = true
		files[seedFileName(len(files))] = encoded
	}
	if len(files) == 0 {
		t.Fatalf("%s: the generator produced no seed, so every property over this corpus would hold vacuously", codec.target)
	}
	return files
}

// readSeedCorpus reads one target's committed seeds. A missing folder, a folder holding a
// subdirectory, and an empty folder are all failures rather than fallbacks: the corpus is
// committed so that a crasher is reproducible from a clean checkout, and a target that
// silently fell back to no seeds is a target that reports green having decoded nothing.
func readSeedCorpus(t testing.TB, target string) ([]string, map[string][]byte) {
	t.Helper()
	directory := seedCorpusDirectory(target)
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("read the committed seed corpus %s: %v; it is committed rather than generated per run, so a missing one is a failure and not a fresh checkout", directory, err)
	}
	names := []string{}
	bodies := map[string][]byte{}
	for _, entry := range entries {
		if entry.IsDir() {
			// p8's loader adds every file in this folder as a seed and does not recurse,
			// so anything under a subdirectory here is a seed nobody reads.
			t.Fatalf("%s holds the directory %q; the loader that reads this folder does not recurse", directory, entry.Name())
		}
		body, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			t.Fatalf("read seed %s: %v", entry.Name(), err)
		}
		names = append(names, entry.Name())
		bodies[entry.Name()] = body
	}
	if len(names) == 0 {
		t.Fatalf("%s holds no seed. an empty corpus is indistinguishable from a complete one at the reporting layer: every target reading it passes having exercised nothing", directory)
	}
	slices.Sort(names)
	return names, bodies
}

// ---------------------------------------------------------------------------
// the derived value comparison
// ---------------------------------------------------------------------------

// seedValuesAgree compares two values by walking the struct definition rather than naming
// its fields.
//
// The field list is derived because a hand written one is exactly the defect the task 13
// review found on this project: a round trip test that compared encoded against re-encoded
// and never compared the decoded VALUE with the original, so a field silently dropped by both
// halves of the codec round tripped byte exact and nothing said so. A comparison that names
// three of a structure's seven fields has the same shape one field later.
//
// A nil slice and an empty one compare equal, because the wire format has one spelling for
// both and the decoder always produces the non nil form; that is a property of the encoding
// and not a leniency in the comparison. An unexported field is fatal rather than skipped: a
// field this cannot read is a field it would report equal whatever it held.
func seedValuesAgree(t *testing.T, path string, left reflect.Value, right reflect.Value) bool {
	if left.Type() != right.Type() {
		t.Fatalf("%s: comparing a %s with a %s", path, left.Type(), right.Type())
	}
	switch left.Kind() {
	case reflect.Pointer, reflect.Interface:
		if left.IsNil() != right.IsNil() {
			return false
		}
		if left.IsNil() {
			return true
		}
		return seedValuesAgree(t, path, left.Elem(), right.Elem())
	case reflect.Struct:
		if left.NumField() == 0 {
			t.Fatalf("%s: %s declares no field, so this comparison walked nothing", path, left.Type())
		}
		for index := 0; index < left.NumField(); index++ {
			field := left.Type().Field(index)
			if !field.IsExported() {
				t.Fatalf("%s.%s is unexported, so this comparison cannot read it and would report two values that differ in it equal",
					path, field.Name)
			}
			if !seedValuesAgree(t, path+"."+field.Name, left.Field(index), right.Field(index)) {
				return false
			}
		}
		return true
	case reflect.Slice:
		if left.Type().Elem().Kind() == reflect.Uint8 {
			// the same comparison the general arm makes, in one call rather than one per
			// octet; bytes.Equal already treats nil and empty as equal.
			return bytes.Equal(left.Bytes(), right.Bytes())
		}
		if left.Len() != right.Len() {
			return false
		}
		for index := 0; index < left.Len(); index++ {
			if !seedValuesAgree(t, fmt.Sprintf("%s[%d]", path, index), left.Index(index), right.Index(index)) {
				return false
			}
		}
		return true
	default:
		if !left.Comparable() {
			t.Fatalf("%s: a %s is not comparable, so this walk has no answer for it", path, left.Type())
		}
		return left.Equal(right)
	}
}

// ---------------------------------------------------------------------------
// reading a value through its codec's projection
// ---------------------------------------------------------------------------

// seedCodecParts is what a walk over one decoded value reads: the value itself, or the codec's
// projection of it.
func seedCodecParts(codec seedCodec, value any) []any {
	if codec.project == nil {
		return []any{value}
	}
	return codec.project(value)
}

// seedCodecValuesAgree compares an original value with the decode of its encoding, through the
// projection where the codec has one.
//
// The projection's LENGTH is compared before its elements, and for the ratchet tree that comparison
// is load bearing rather than defensive: the node width is the one property of that structure the
// wire does not carry explicitly -- readNodeArray derives it by extending the entry count to the
// next complete tree -- so a decode that lost or gained a node is a tree with a different root, a
// different direct path for every leaf and a different tree hash, and an elementwise comparison
// over the entries that survived would report the two equal.
func seedCodecValuesAgree(t *testing.T, codec seedCodec, left any, right any) bool {
	t.Helper()
	if codec.project == nil {
		return seedValuesAgree(t, codec.target, reflect.ValueOf(left), reflect.ValueOf(right))
	}
	leftParts, rightParts := codec.project(left), codec.project(right)
	if len(leftParts) == 0 {
		t.Fatalf("%s: the projection of %s is empty, so this comparison walked nothing",
			codec.target, codec.describe(left))
	}
	if len(leftParts) != len(rightParts) {
		return false
	}
	for index := range leftParts {
		at := fmt.Sprintf("%s[%d]", codec.target, index)
		if !seedValuesAgree(t, at, reflect.ValueOf(leftParts[index]), reflect.ValueOf(rightParts[index])) {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// asking the codec whether a field CAN vary
// ---------------------------------------------------------------------------

// seedFieldIsPinnedByTheCodec answers whether a field the corpus never varies is one the CODEC will
// not let it vary, and it is what keeps the variation gate below from needing a table of
// exceptions.
//
// The distinction is real rather than a loophole. That gate's claim about a single valued field is
// that the codec could drop it from BOTH halves without moving one committed byte -- true of a
// field the generator never varied, and false of one the codec pins to a constant.
// Credential.CredentialType is the second kind: both halves of credential.go refuse anything but
// CredentialTypeBasic, so no decodable seed can carry another value and no generator can produce
// one. Dropping that field WOULD move every committed byte, and the corpus comparison says so.
//
// DERIVED by running the codec rather than by naming the field, which is rule 5 and is the whole
// point: a field that stops being pinned -- a second credential type admitted to the v1 profile,
// say -- rejoins the gate in the commit that admits it, with nobody having to remember to delete a
// name from a list. The probe is to decode one committed seed, move that one field to a different
// value, and re-encode: if the encoder or the decoder refuses the result, the codec admits no other
// value there. If both accept it, the field is carried on the wire and the corpus is simply not
// varying it, which is exactly what the gate says.
//
// Conservative in the one direction that matters: a field this cannot move at all -- an unsupported
// kind, or one that is nil in every seed -- is reported NOT pinned, so the gate's error stands and
// somebody has to look at it.
func seedFieldIsPinnedByTheCodec(t *testing.T, codec seedCodec, fieldPath string) (bool, string) {
	t.Helper()
	names, onDisk := readSeedCorpus(t, codec.target)
	seeds := make([][]byte, 0, len(names))
	for _, name := range names {
		seeds = append(seeds, onDisk[name])
	}
	return seedFieldIsPinnedOverSeeds(codec, seeds, fieldPath)
}

// seedFieldIsPinnedOverSeeds is the probe itself, over seeds a caller supplies rather than over the
// corpus on disk. Split out so TestTheFieldPinProbeSeparatesAPinnedFieldFromAFreeOne can drive it
// against two structures built to differ in exactly the thing it claims to detect -- which is the
// only control that can tell this helper from one that answers "pinned" to everything, since the
// gate that consults it only ever asks about fields that are already single valued.
func seedFieldIsPinnedOverSeeds(codec seedCodec, seeds [][]byte, fieldPath string) (bool, string) {
	for _, seed := range seeds {
		value, err := codec.decode(seed)
		if err != nil {
			continue
		}
		moved := false
		for _, part := range seedCodecParts(codec, value) {
			if seedMoveFieldAt(reflect.ValueOf(part), codec.target, fieldPath) {
				moved = true
				break
			}
		}
		if !moved {
			continue
		}
		encoded, err := codec.encode(value)
		if err != nil {
			return true, fmt.Sprintf("the encoder refuses any other value: %v", err)
		}
		if _, err := codec.decode(encoded); err != nil {
			return true, fmt.Sprintf("the decoder refuses any other value: %v", err)
		}
		return false, ""
	}
	return false, ""
}

// seedMoveFieldAt walks a value to one field path and moves the value it finds there, answering
// whether it moved anything.
//
// The walk mirrors seedObservations.observe step for step, including how a path is spelled, because
// the path it is handed came out of that walk: a walk that named a slice element ".Nodes[0]" while
// observe named it ".Nodes[]" would find nothing, report every field unmovable, and
// seedFieldIsPinnedByTheCodec would read that as "not pinned" for every field in the package.
func seedMoveFieldAt(value reflect.Value, at string, want string) bool {
	switch value.Kind() {
	case reflect.Pointer, reflect.Interface:
		if value.IsNil() {
			return false
		}
		return seedMoveFieldAt(value.Elem(), at, want)
	case reflect.Struct:
		for index := 0; index < value.NumField(); index++ {
			field := value.Type().Field(index)
			if !field.IsExported() {
				continue
			}
			if seedMoveFieldAt(value.Field(index), at+"."+field.Name, want) {
				return true
			}
		}
		return false
	case reflect.Slice:
		if at == want {
			return seedMoveValue(value)
		}
		if value.Type().Elem().Kind() == reflect.Uint8 {
			return false
		}
		for index := 0; index < value.Len(); index++ {
			if seedMoveFieldAt(value.Index(index), at+"[]", want) {
				return true
			}
		}
		return false
	default:
		if at == want {
			return seedMoveValue(value)
		}
		return false
	}
}

// seedMoveValue changes one value to a different one of the same type. An unsupported kind answers
// false rather than being coerced, so the probe above says "no answer" instead of "pinned".
func seedMoveValue(value reflect.Value) bool {
	if !value.CanSet() {
		return false
	}
	switch value.Kind() {
	case reflect.Bool:
		value.SetBool(!value.Bool())
		return true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		value.SetUint(value.Uint() + 1)
		return true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		value.SetInt(value.Int() + 1)
		return true
	case reflect.Slice:
		value.Set(reflect.Append(value, reflect.Zero(value.Type().Elem())))
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// the properties
// ---------------------------------------------------------------------------

// TestTheCommittedSeedCorpusIsExactlyTheGeneratedCorpus is what makes every other property
// in this file evidence rather than a tautology.
//
// The seeds on disk were written by the encoder as it stood when they were committed. A
// corpus regenerated from the encoder it is checking agrees with that encoder by
// construction, so a field dropped from BOTH halves of the codec -- the mutation this file
// exists to catch -- would round trip perfectly, regenerate consistently, and change nothing
// anybody could see. Comparing the two is what turns that mutation into a diff.
//
// A deliberate generator change is therefore expected to fail here, and the fix is to
// regenerate in the same commit rather than to relax the comparison.
func TestTheCommittedSeedCorpusIsExactlyTheGeneratedCorpus(t *testing.T) {
	rewrite := os.Getenv(seedCorpusWriteEnv) != ""
	for _, codec := range seedCodecs() {
		generated := generatedSeedFiles(t, codec)
		if rewrite {
			directory := seedCorpusDirectory(codec.target)
			if err := os.RemoveAll(directory); err != nil {
				t.Fatalf("clear %s: %v", directory, err)
			}
			if err := os.MkdirAll(directory, 0o755); err != nil {
				t.Fatalf("create %s: %v", directory, err)
			}
			for _, name := range slices.Sorted(maps.Keys(generated)) {
				if err := os.WriteFile(filepath.Join(directory, name), generated[name], 0o600); err != nil {
					t.Fatalf("write %s: %v", name, err)
				}
			}
			t.Logf("%s: wrote %d seeds", codec.target, len(generated))
			continue
		}

		names, onDisk := readSeedCorpus(t, codec.target)
		for _, name := range names {
			want, expected := generated[name]
			if !expected {
				t.Errorf("%s/%s is on disk and the generator does not produce it; a seed nothing generates is a seed nothing keeps honest",
					codec.target, name)
				continue
			}
			if !bytes.Equal(onDisk[name], want) {
				t.Errorf("%s/%s differs from the generated seed:\n on disk %x\ngenerated %x\nthe codec moved under a committed corpus; regenerate with %s=1 in the commit that moved it",
					codec.target, name, onDisk[name], want, seedCorpusWriteEnv)
			}
		}
		for _, name := range slices.Sorted(maps.Keys(generated)) {
			if _, present := onDisk[name]; !present {
				t.Errorf("%s/%s is generated and is not committed; regenerate with %s=1",
					codec.target, name, seedCorpusWriteEnv)
			}
		}
		t.Logf("%s: %d committed seeds, %d generated", codec.target, len(names), len(generated))
	}
	if rewrite {
		t.Fatalf("the corpus was rewritten from the generator because %s is set; unset it and re-run. A run that rewrites the artefact it checks has checked nothing, and reporting success for it is how a stale corpus survives",
			seedCorpusWriteEnv)
	}
}

// TestEverySeedInTheCommittedCorpusReEncodesToItsOwnBytes is gate 4 property 2 -- encode of
// decode of x equals x -- over every committed seed, plus the same claim through
// syntax.CheckRoundTrip, which is the helper every one of p8's targets reaches this codec
// through. A codec that satisfied the property here and not there would leave those targets
// green and empty.
//
// The decoded count is asserted rather than logged. A loop whose body never ran reports
// exactly what a loop that checked every seed reports, and on this project that is not a
// hypothetical: three CheckRoundTrip tests once passed against a helper that evaluated the
// comparison, discarded the result and returned nil.
func TestEverySeedInTheCommittedCorpusReEncodesToItsOwnBytes(t *testing.T) {
	for _, codec := range seedCodecs() {
		names, onDisk := readSeedCorpus(t, codec.target)
		decoded := 0
		for _, name := range names {
			body := onDisk[name]
			value, err := codec.decode(body)
			if err != nil {
				t.Errorf("%s/%s: the committed seed did not decode: %v", codec.target, name, err)
				continue
			}
			decoded++
			reencoded, err := codec.encode(value)
			if err != nil {
				t.Errorf("%s/%s: %s decoded and would not re-encode: %v", codec.target, name, codec.describe(value), err)
				continue
			}
			if !bytes.Equal(reencoded, body) {
				t.Errorf("%s/%s: %s round tripped to different bytes:\n got %x\nwant %x",
					codec.target, name, codec.describe(value), reencoded, body)
				continue
			}
			if err := codec.checkRoundTrip(body); err != nil {
				t.Errorf("%s/%s: syntax.CheckRoundTrip: %v", codec.target, name, err)
			}
		}
		if decoded == 0 {
			t.Fatalf("%s: not one of the %d committed seeds decoded, so this property reached nothing; a corpus of bytes no decoder accepts makes every target trivially green",
				codec.target, len(names))
		}
		if decoded != len(names) {
			t.Errorf("%s: %d of %d committed seeds decoded", codec.target, decoded, len(names))
		}
		t.Logf("%s: %d seeds re-encoded to their own bytes", codec.target, decoded)
	}
}

// TestEveryGeneratedSeedValueIsRecoveredByDecodingItsEncoding is the OTHER direction, and it
// is a different claim: decode of encode of v equals v. A codec can satisfy one and not the
// other, and the task 13 review found exactly that shape here -- the plan's round trip test
// compared encoded against re-encoded and never compared the decoded value with the original,
// so a field dropped by both halves round tripped byte exact.
//
// The comparison walks the struct definition rather than naming fields, so a field added to
// either structure later is VISITED by the commit that adds it. It is not thereby COVERED, and
// the difference is the whole of what this test cannot do on its own: the generator's per field
// axes are hand written, so a newly added field is generated at its zero value in every seed and
// this walk then compares zero against zero. Confirmed by mutation, twice: an opaque field added
// to GroupContext and carried by both halves left every property in this file green, and
// dropping it from both halves again left them green.
// TestEveryFieldOfBothStructuresVariesAcrossTheCommittedCorpus states the missing half.
func TestEveryGeneratedSeedValueIsRecoveredByDecodingItsEncoding(t *testing.T) {
	for _, codec := range seedCodecs() {
		values := codec.values(t)
		if len(values) == 0 {
			t.Fatalf("%s: the generator produced no value, so this property compared nothing", codec.target)
		}
		compared := 0
		for index, value := range values {
			encoded, err := codec.encode(value)
			if err != nil {
				t.Errorf("%s: value %d %s: encode: %v", codec.target, index, codec.describe(value), err)
				continue
			}
			parsed, err := codec.decode(encoded)
			if err != nil {
				t.Errorf("%s: value %d %s: our own encoding was refused by our decoder: %v",
					codec.target, index, codec.describe(value), err)
				continue
			}
			compared++
			if !seedCodecValuesAgree(t, codec, value, parsed) {
				t.Errorf("%s: value %d %s decoded as %s", codec.target, index,
					codec.describe(value), codec.describe(parsed))
			}
		}
		if compared != len(values) {
			t.Errorf("%s: %d of %d generated values reached the comparison", codec.target, compared, len(values))
		}
		t.Logf("%s: %d generated values recovered from their own encodings", codec.target, compared)
	}
}

// ---------------------------------------------------------------------------
// what the corpus was seen to carry, derived rather than listed
// ---------------------------------------------------------------------------

// seedMlsPackagePath is this package's import path, read off a type in it rather than typed
// out, so the derivations below cannot drift from the package they are about.
var seedMlsPackagePath = reflect.TypeOf(GroupContext{}).PkgPath()

// seedRegistryTypeName answers whether a type is one of this package's registries: a NAMED
// integer declared in package mls. That is the derivation, and it is the whole point of it --
// the previous version of the gate below held a hand written list of five type names, and a
// sixth registry declared in this package and carried by GroupContext was invisible to every
// property in this file. The corpus carried its zero code point in all 146 seeds, every test
// stayed green, and the fuzzer would have started blind to every arm the new enum selects.
// That is the table-holding-five-of-six shape rule 5 names, in the one file whose whole subject
// is derived coverage.
func seedRegistryTypeName(candidate reflect.Type) (string, bool) {
	if candidate.PkgPath() != seedMlsPackagePath || candidate.Name() == "" {
		return "", false
	}
	switch candidate.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return candidate.Name(), true
	}
	return "", false
}

// seedCodePoint reads a registry value as the unsigned code point the wire carries.
func seedCodePoint(value reflect.Value) (uint64, bool) {
	switch value.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return value.Uint(), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return uint64(value.Int()), true
	}
	return 0, false
}

// seedRegistryTypesReachable collects the registry class of one structure: every named integer
// this package declares that a value of that structure can carry on the wire. It walks the TYPE
// rather than the values, because a registry no seed happens to reach is exactly the case the
// gate has to fail on.
func seedRegistryTypesReachable(structure reflect.Type, into map[string]bool, seen map[reflect.Type]bool) {
	if seen[structure] {
		return
	}
	seen[structure] = true
	if name, isRegistry := seedRegistryTypeName(structure); isRegistry {
		into[name] = true
		return
	}
	switch structure.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array:
		seedRegistryTypesReachable(structure.Elem(), into, seen)
	case reflect.Map:
		seedRegistryTypesReachable(structure.Key(), into, seen)
		seedRegistryTypesReachable(structure.Elem(), into, seen)
	case reflect.Struct:
		for index := 0; index < structure.NumField(); index++ {
			seedRegistryTypesReachable(structure.Field(index).Type, into, seen)
		}
	}
}

// seedFieldPathsOf is the same walk over the type, collecting the field paths a walk over a
// VALUE of it will visit. The two walks have to agree on the spelling of a path, which is why
// they are written beside each other: a path the type walk names and the value walk never
// produces reads as an unreached field, and the gate below says so rather than passing.
func seedFieldPathsOf(structure reflect.Type, at string, into *[]string, seen map[reflect.Type]bool) {
	switch structure.Kind() {
	case reflect.Pointer:
		if seen[structure] {
			return
		}
		seen[structure] = true
		seedFieldPathsOf(structure.Elem(), at, into, seen)
	case reflect.Struct:
		if seen[structure] {
			return
		}
		seen[structure] = true
		for index := 0; index < structure.NumField(); index++ {
			field := structure.Field(index)
			if !field.IsExported() {
				continue
			}
			seedFieldPathsOf(field.Type, at+"."+field.Name, into, seen)
		}
	case reflect.Slice:
		*into = append(*into, at)
		if structure.Elem().Kind() == reflect.Uint8 {
			return
		}
		seedFieldPathsOf(structure.Elem(), at+"[]", into, seen)
	default:
		*into = append(*into, at)
	}
}

// seedObservations is what a walk over the committed corpus actually saw, keyed by the field
// path it saw it at rather than by a name somebody wrote down.
type seedObservations struct {
	// fieldValues maps a field path to the DISTINCT decoded values the corpus carries there.
	// The count is the load bearing part rather than the values: a field the codec does not
	// carry decodes to one value across the whole corpus, whatever the generator meant to put
	// in it.
	fieldValues map[string]map[string]bool
	// registryValues maps a registry type name to the code points the corpus carries.
	registryValues map[string]map[uint64]bool
	// opaqueLengths and epochValues are the varint width and uint64 boundary axes, collected
	// from every []byte and every unnamed uint64 field the walk reaches rather than from a
	// list of the three fields per structure somebody remembered.
	opaqueLengths map[int]bool
	epochValues   map[uint64]bool
}

func newSeedObservations() *seedObservations {
	return &seedObservations{
		fieldValues:    map[string]map[string]bool{},
		registryValues: map[string]map[uint64]bool{},
		opaqueLengths:  map[int]bool{},
		epochValues:    map[uint64]bool{},
	}
}

func (self *seedObservations) record(fieldPath string, value string) {
	values, seen := self.fieldValues[fieldPath]
	if !seen {
		values = map[string]bool{}
		self.fieldValues[fieldPath] = values
	}
	values[value] = true
}

// observe walks one decoded seed and records every field it holds.
//
// An unexported field is fatal rather than skipped, for the same reason it is fatal in
// seedValuesAgree: a field this walk cannot read is a field it would report covered whatever
// the corpus put in it.
func (self *seedObservations) observe(t *testing.T, fieldPath string, value reflect.Value) {
	t.Helper()
	switch value.Kind() {
	case reflect.Pointer, reflect.Interface:
		if value.IsNil() {
			self.record(fieldPath, "nil")
			return
		}
		self.observe(t, fieldPath, value.Elem())
	case reflect.Struct:
		if value.NumField() == 0 {
			t.Fatalf("%s: %s declares no field, so this walk observed nothing", fieldPath, value.Type())
		}
		for index := 0; index < value.NumField(); index++ {
			field := value.Type().Field(index)
			if !field.IsExported() {
				t.Fatalf("%s.%s is unexported, so this walk cannot read it and would report a field the corpus never varies as covered",
					fieldPath, field.Name)
			}
			self.observe(t, fieldPath+"."+field.Name, value.Field(index))
		}
	case reflect.Slice:
		if value.Type().Elem().Kind() == reflect.Uint8 {
			self.record(fieldPath, fmt.Sprintf("%x", value.Bytes()))
			self.opaqueLengths[value.Len()] = true
			return
		}
		// a vector is observed by its arity as well as by its entries: the length prefix counts
		// BYTES rather than elements, and those two readings agree on every one entry vector
		// and part company at the first two entry one.
		self.record(fieldPath, fmt.Sprintf("arity %d", value.Len()))
		for index := 0; index < value.Len(); index++ {
			self.observe(t, fieldPath+"[]", value.Index(index))
		}
	default:
		if !value.CanInterface() {
			t.Fatalf("%s: this walk cannot read a %s", fieldPath, value.Type())
		}
		self.record(fieldPath, fmt.Sprintf("%v", value.Interface()))
		if registry, isRegistry := seedRegistryTypeName(value.Type()); isRegistry {
			code, readable := seedCodePoint(value)
			if !readable {
				t.Fatalf("%s: %s is a registry type this walk cannot read as a code point", fieldPath, value.Type())
			}
			if self.registryValues[registry] == nil {
				self.registryValues[registry] = map[uint64]bool{}
			}
			self.registryValues[registry][code] = true
		}
		if value.Kind() == reflect.Uint64 && value.Type().PkgPath() == "" {
			self.epochValues[value.Uint()] = true
		}
	}
}

// observeCommittedCorpus decodes every committed seed of one target and returns what they hold.
func observeCommittedCorpus(t *testing.T, codec seedCodec) ([]string, *seedObservations) {
	t.Helper()
	names, onDisk := readSeedCorpus(t, codec.target)
	observations := newSeedObservations()
	for _, name := range names {
		parsed, err := codec.decode(onDisk[name])
		if err != nil {
			t.Fatalf("%s/%s: the committed seed did not decode, so nothing below observed it: %v", codec.target, name, err)
		}
		for _, part := range seedCodecParts(codec, parsed) {
			observations.observe(t, codec.target, reflect.ValueOf(part))
		}
	}
	if len(observations.fieldValues) == 0 {
		t.Fatalf("%s: the walk over %d committed seeds recorded no field at all", codec.target, len(names))
	}
	return names, observations
}

// TestTheCommittedSeedCorpusCoversEveryRegistryCodePoint is the rule 5 gate on the corpus
// itself, and it reads the SEEDS ON DISK rather than the generator's output.
//
// Both sides are derived. The class of registries is every named integer this package declares
// that the structure can carry on the wire, taken from the structure's own type; the code points
// of each are taken from the package's own constant declarations. So a code point added to any
// registry, and a whole new registry added to either structure, both turn this red until the
// corpus catches up -- which is the only mechanism that keeps a committed corpus from ageing
// into a list of the code points that existed when somebody wrote it.
func TestTheCommittedSeedCorpusCoversEveryRegistryCodePoint(t *testing.T) {
	for _, codec := range seedCodecs() {
		names, observations := observeCommittedCorpus(t, codec)

		structure := reflect.TypeOf(codec.structure())
		registries := map[string]bool{}
		seedRegistryTypesReachable(structure, registries, map[reflect.Type]bool{})
		if len(registries) == 0 {
			t.Fatalf("%s: no registry type is reachable from %s, so this gate would assert nothing over %d seeds",
				codec.target, structure, len(names))
		}
		for _, typeName := range slices.Sorted(maps.Keys(registries)) {
			derived := namedTypeConstants(t, typeCheckedPackage(t), typeName)
			if len(derived) == 0 {
				// a named integer on the wire that declares no code point is not a registry, so
				// there is nothing here to cover. It is logged rather than passed over in
				// silence, and the field gate below is what keeps the corpus honest about it.
				t.Logf("%s carries %s, which declares no code point", codec.target, typeName)
				continue
			}
			for _, name := range slices.Sorted(maps.Keys(derived)) {
				if !observations.registryValues[typeName][derived[name]] {
					t.Errorf("no committed %s seed carries %s = %#x; a registry member the corpus does not encode is a decoder arm the fuzzer starts blind to, and regenerating with %s=1 is the fix",
						codec.target, name, derived[name], seedCorpusWriteEnv)
				}
			}
			t.Logf("%s: %s declares %d code points, all present among the %d distinct values the corpus carries",
				codec.target, typeName, len(derived), len(observations.registryValues[typeName]))
		}
	}
}

// TestTheCommittedSeedCorpusCoversEveryVarintWidthAndEpochBoundary asserts the two non registry
// axes over the seeds on disk: the lengths the varint prefix branches on, and the uint64
// boundaries a narrowed or signed epoch would move.
//
// The fields it collects them from are derived too -- every []byte and every unnamed uint64 the
// walk reaches -- rather than the three opaque fields per structure the previous version named.
func TestTheCommittedSeedCorpusCoversEveryVarintWidthAndEpochBoundary(t *testing.T) {
	for _, codec := range seedCodecs() {
		names, observations := observeCommittedCorpus(t, codec)
		for _, length := range append(seedOpaqueLengths(), seedWideOpaqueLength) {
			if !observations.opaqueLengths[length] {
				t.Errorf("%s: no committed seed carries a variable length field of %d octets, which is one of the widths the varint prefix branches on",
					codec.target, length)
			}
		}
		for _, epoch := range seedEpochs() {
			if !observations.epochValues[epoch] {
				t.Errorf("%s: no committed seed carries the epoch boundary %#016x", codec.target, epoch)
			}
		}
		t.Logf("%s: %d distinct field lengths and %d distinct uint64 values across %d seeds",
			codec.target, len(observations.opaqueLengths), len(observations.epochValues), len(names))
	}
}

// TestEveryFieldOfBothStructuresVariesAcrossTheCommittedCorpus is the gate the derived
// comparison in seedValuesAgree cannot be, and the distinction is worth stating because that
// comparison's own doc comment used to overstate it.
//
// seedValuesAgree walks the struct definition, so it VISITS a field added later. It compares
// that field's value in the original against its value in the decode of its encoding -- and the
// generator's axes are hand written, so a newly added field is generated at its zero value in
// every seed, the walk compares zero against zero, and the mutation this whole file exists to
// catch (drop the field from BOTH halves of the codec) is fully green. Both halves of that were
// confirmed by mutation on this file: a new opaque field added to GroupContext and encoded left
// every property here passing, and dropping it from both halves again left them passing too.
//
// This states the missing half as a property of the CORPUS rather than of the generator: every
// field of these structures takes at least two distinct values across the committed seeds. A
// field the codec does not carry decodes to one value in every seed. A field the codec carries
// and the generator never varies decodes to one value in every seed. Those are the same defect
// seen from the fuzzer, which is handed an axis it can never move, and both are one count away
// here. The class of fields is derived from the structure, so the field added tomorrow is in it.
func TestEveryFieldOfBothStructuresVariesAcrossTheCommittedCorpus(t *testing.T) {
	for _, codec := range seedCodecs() {
		names, observations := observeCommittedCorpus(t, codec)

		fieldPaths := []string{}
		seedFieldPathsOf(reflect.TypeOf(codec.structure()), codec.target, &fieldPaths, map[reflect.Type]bool{})
		if len(fieldPaths) == 0 {
			t.Fatalf("%s: the field derivation named no field of %s, so this gate would assert nothing",
				codec.target, reflect.TypeOf(codec.structure()))
		}
		varying := 0
		pinned := []string{}
		for _, fieldPath := range fieldPaths {
			values, reached := observations.fieldValues[fieldPath]
			if !reached {
				t.Errorf("%s: %s is a field of the structure and the walk over %d committed seeds never reached it, so nothing here says what the corpus puts in it",
					codec.target, fieldPath, len(names))
				continue
			}
			if len(values) < 2 {
				if isPinned, why := seedFieldIsPinnedByTheCodec(t, codec, fieldPath); isPinned {
					// not a gap: this codec admits no other value here, so the corpus not varying
					// it is a property of the format rather than a thinness in the generator.
					pinned = append(pinned, fmt.Sprintf("%s (%s)", fieldPath, why))
					continue
				}
				t.Errorf("%s: all %d committed seeds decode to the same %s (%s), and the codec accepts other values there. a field the corpus never varies is a field the codec could drop from BOTH halves without moving one committed byte, which is the mutation this corpus exists to catch; give it an axis in the generator and regenerate with %s=1",
					codec.target, len(names), fieldPath, describeSoleSeedValue(values), seedCorpusWriteEnv)
				continue
			}
			varying++
		}
		t.Logf("%s: %d of %d derived fields carry at least two distinct values across %d seeds; %d are pinned to one value by the codec itself %v",
			codec.target, varying, len(fieldPaths), len(names), len(pinned), pinned)
	}
}

// describeSoleSeedValue names the one value a field was found to hold, truncated, so the failure
// above says WHICH constant the corpus is stuck on rather than only that it is stuck.
func describeSoleSeedValue(values map[string]bool) string {
	for _, value := range slices.Sorted(maps.Keys(values)) {
		if value == "" {
			return "the empty value"
		}
		if len(value) > 64 {
			return value[:64] + "..."
		}
		return value
	}
	return "no value at all"
}

// TestEverySeedInTheCommittedCorpusRefusesEveryTruncationAndAnyExtension is gate 4 property 1
// over a bounded input set, on every run, without waiting for the fuzzer: every prefix of a
// valid encoding and every one octet extension of it is refused rather than accepted as a
// second encoding of the same object.
//
// The extension half is the one that matters most. A decoder that ignores a tail accepts two
// encodings of one object, and MLS signs over serialized forms, so that is a signature bypass
// primitive and not a leniency.
func TestEverySeedInTheCommittedCorpusRefusesEveryTruncationAndAnyExtension(t *testing.T) {
	for _, codec := range seedCodecs() {
		names, onDisk := readSeedCorpus(t, codec.target)
		prefixes, extensions := 0, 0
		for _, name := range names {
			body := onDisk[name]
			for n := 0; n < len(body); n++ {
				if _, err := codec.decode(body[:n]); err == nil {
					t.Errorf("%s/%s: the first %d of %d octets parsed as a whole structure",
						codec.target, name, n, len(body))
					continue
				}
				prefixes++
			}
			extended := append(append([]byte(nil), body...), 0x00)
			if _, err := codec.decode(extended); err == nil {
				t.Errorf("%s/%s: a trailing octet was accepted, so this structure has two encodings",
					codec.target, name)
				continue
			}
			extensions++
		}
		if prefixes == 0 || extensions == 0 {
			t.Fatalf("%s: %d prefixes and %d extensions were refused across %d seeds; this property reached nothing",
				codec.target, prefixes, extensions, len(names))
		}
		t.Logf("%s: %d prefixes and %d extensions refused", codec.target, prefixes, extensions)
	}
}

// ---------------------------------------------------------------------------
// the control on the shared helper
// ---------------------------------------------------------------------------

// canonicalSeedProbe, nonCanonicalSeedProbe and unstableSeedProbe are three one octet codecs
// that exist only to ask syntax.CheckRoundTrip whether it can still say no.
//
// They are here because nothing else in this package can ask that question. Both real codecs
// are canonical, so CheckRoundTrip returns nil for every input they are given -- and it also
// returns nil for every input if it evaluates its comparisons and throws the results away.
// That is not a hypothetical defect: a version of this helper that did all the work, computed
// the comparison, discarded it and returned nil passed all three of p1's round trip tests,
// and every one of p8's nine targets calls it.
type canonicalSeedProbe struct{ value uint8 }

func (self *canonicalSeedProbe) MarshalMLS(w *syntax.Writer) error {
	w.WriteUint8(self.value)
	return nil
}

func (self *canonicalSeedProbe) UnmarshalMLS(r *syntax.Reader) error {
	value, err := r.ReadUint8()
	if err != nil {
		return err
	}
	self.value = value
	return nil
}

// nonCanonicalSeedProbe re-encodes what it decoded as different bytes, which is exactly the
// shape gate 4 property 2 forbids.
type nonCanonicalSeedProbe struct{ value uint8 }

func (self *nonCanonicalSeedProbe) MarshalMLS(w *syntax.Writer) error {
	w.WriteUint8(self.value ^ 0xff)
	return nil
}

func (self *nonCanonicalSeedProbe) UnmarshalMLS(r *syntax.Reader) error {
	value, err := r.ReadUint8()
	if err != nil {
		return err
	}
	self.value = value
	return nil
}

// unstableSeedProbeEncodes is the hidden state unstableSeedProbe carries between calls -- a
// map ranged during encode, a registry consulted at decode, a buffer shared across calls.
// That defect is invisible to the byte exactness check, which sees one encode, and the second
// pass inside CheckRoundTrip is the only thing that looks for it.
var unstableSeedProbeEncodes int

type unstableSeedProbe struct{ value uint8 }

func (self *unstableSeedProbe) MarshalMLS(w *syntax.Writer) error {
	unstableSeedProbeEncodes++
	if unstableSeedProbeEncodes > 1 {
		w.WriteUint8(self.value ^ 0xff)
		return nil
	}
	w.WriteUint8(self.value)
	return nil
}

func (self *unstableSeedProbe) UnmarshalMLS(r *syntax.Reader) error {
	value, err := r.ReadUint8()
	if err != nil {
		return err
	}
	self.value = value
	return nil
}

// TestCheckRoundTripReportsTheViolationsItIsHanded is the positive control on the helper the
// two properties above and all nine of p8's targets are stated through.
func TestCheckRoundTripReportsTheViolationsItIsHanded(t *testing.T) {
	if err := syntax.CheckRoundTrip[canonicalSeedProbe, *canonicalSeedProbe]([]byte{0x2a}); err != nil {
		t.Errorf("a codec that reproduces its own bytes was reported as a violation: %v", err)
	}
	// an input that does not decode carries no obligation; reporting one would make every
	// target noisy on exactly the inputs a fuzzer spends most of its budget on.
	if err := syntax.CheckRoundTrip[canonicalSeedProbe, *canonicalSeedProbe](nil); err != nil {
		t.Errorf("an input that does not decode was reported as a round trip violation: %v", err)
	}
	if err := syntax.CheckRoundTrip[nonCanonicalSeedProbe, *nonCanonicalSeedProbe]([]byte{0x00}); !errors.Is(err, syntax.ErrRoundTripNotByteExact) {
		t.Errorf("CheckRoundTrip answered %v for a codec whose re-encoding is not its input, want syntax.ErrRoundTripNotByteExact; every seed in this corpus is checked through this helper, so a helper that computes the comparison and discards it reports the whole corpus clean having compared nothing",
			err)
	}
	unstableSeedProbeEncodes = 0
	if err := syntax.CheckRoundTrip[unstableSeedProbe, *unstableSeedProbe]([]byte{0x00}); !errors.Is(err, syntax.ErrRoundTripNotStable) {
		t.Errorf("CheckRoundTrip answered %v for a codec that encodes the same value differently on its second call, want syntax.ErrRoundTripNotStable", err)
	}
}

// ---------------------------------------------------------------------------
// the pin that keeps the corpus the bytes that were committed
// ---------------------------------------------------------------------------

// gitAttributesTextRule is one .gitattributes line that has an opinion about the text attribute:
// the pattern, and whether it turns text OFF. `-text` and the `binary` macro both do; `text`
// and `text eol=lf` both turn it on. A line that says nothing about text is not a rule here at
// all, which is why the type carries no third state.
type gitAttributesTextRule struct {
	line    string
	pattern string
	pinned  bool
}

// gitAttributesTextRules reads the rules in FILE ORDER, because order is the whole of the
// semantics: git resolves an attribute by the LAST matching line, so a scan that stopped at the
// first match would report a pin a later line had already undone.
func gitAttributesTextRules(body string) []gitAttributesTextRule {
	rules := []gitAttributesTextRule{}
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		for _, attribute := range fields[1:] {
			switch {
			case attribute == "-text" || attribute == "binary":
				rules = append(rules, gitAttributesTextRule{line: strings.TrimSpace(line), pattern: fields[0], pinned: true})
			case attribute == "text" || strings.HasPrefix(attribute, "text="):
				rules = append(rules, gitAttributesTextRule{line: strings.TrimSpace(line), pattern: fields[0], pinned: false})
			}
		}
	}
	return rules
}

// gitAttributesPinsAsBinary answers, for one repository relative path, whether the rule set
// leaves end of line conversion off for it, and names the line that decided.
func gitAttributesPinsAsBinary(rules []gitAttributesTextRule, filePath string) (bool, string) {
	pinned, decidedBy := false, ""
	for _, rule := range rules {
		if gitAttributesPatternMatches(rule.pattern, filePath) {
			pinned, decidedBy = rule.pinned, rule.line
		}
	}
	return pinned, decidedBy
}

// gitAttributesPatternMatches answers gitattributes' own question -- does this pattern apply to
// this path -- rather than the question a prefix comparison answers, which is whether somebody
// spelled the pattern the way this test's author expected.
//
// That distinction was a real false positive here. The gate used to accept a rule only when its
// pattern string started with "mls/testdata/corpus", so `/mls/testdata/corpus/** -text`, which
// is the same rule to git and arguably the more correct spelling of it, failed. A gate that
// fails on a correct spelling is a gate that will one day be silenced by rewriting the rule
// instead of by fixing what it is complaining about.
//
// The semantics implemented are gitignore's, which gitattributes borrows: a pattern holding no
// slash matches the base name at any depth; any other pattern is anchored at the directory
// holding the .gitattributes file, and a leading slash carries no meaning beyond that anchoring;
// `*`, `?` and a character class match within one path component; `**` stands for any number of
// components, including none.
func gitAttributesPatternMatches(pattern string, filePath string) bool {
	pattern = strings.TrimSuffix(pattern, "/")
	if pattern == "" {
		return false
	}
	anchored := strings.HasPrefix(pattern, "/") || strings.Contains(strings.TrimPrefix(pattern, "/"), "/")
	pattern = strings.TrimPrefix(pattern, "/")
	segments := strings.Split(filePath, "/")
	if !anchored {
		for _, segment := range segments {
			if matched, err := path.Match(pattern, segment); err == nil && matched {
				return true
			}
		}
		return false
	}
	return gitAttributesSegmentsMatch(strings.Split(pattern, "/"), segments)
}

// gitAttributesSegmentsMatch is the anchored half, component by component so that `*` cannot
// cross a separator and `**` can.
func gitAttributesSegmentsMatch(patternSegments []string, pathSegments []string) bool {
	if len(patternSegments) == 0 {
		return len(pathSegments) == 0
	}
	if patternSegments[0] == "**" {
		// any number of components, including none, so every suffix of the remaining path is a
		// candidate.
		for skip := 0; skip <= len(pathSegments); skip++ {
			if gitAttributesSegmentsMatch(patternSegments[1:], pathSegments[skip:]) {
				return true
			}
		}
		return false
	}
	if len(pathSegments) == 0 {
		return false
	}
	matched, err := path.Match(patternSegments[0], pathSegments[0])
	if err != nil || !matched {
		return false
	}
	return gitAttributesSegmentsMatch(patternSegments[1:], pathSegments[1:])
}

// TestTheGitAttributesPatternMatcherAnswersGitsQuestion is the control on the matcher, and it is
// not optional: a matcher that returned true for everything would report the whole corpus pinned
// by whatever line happened to be last, and a matcher that returned false for everything would
// fail the gate below on a repository that is correctly configured. Both halves are stated.
func TestTheGitAttributesPatternMatcherAnswersGitsQuestion(t *testing.T) {
	const seed = "mls/testdata/corpus/FuzzGroupContextRoundTrip/seed001"
	for _, probe := range []struct {
		pattern string
		path    string
		matches bool
		why     string
	}{
		{"mls/testdata/corpus/**", seed, true, "the spelling this repository uses"},
		{"/mls/testdata/corpus/**", seed, true, "the same rule anchored at the root, which is what the old prefix comparison rejected"},
		{"**/corpus/**", seed, true, "a leading ** skips any number of components"},
		{"mls/testdata/corpus/*", seed, false, "a single star does not cross a separator, so this rule reaches the folders and not the seeds"},
		{"mls/testdata/corpus/", seed, false, "a directory pattern does not recursively cover the paths inside it"},
		{"message/testdata/corpus/**", seed, false, "another package's corpus"},
		{"seed001", seed, true, "a pattern with no slash matches the base name at any depth"},
		{"seed001", "mls/testdata/corpus/FuzzGroupContextRoundTrip/seed002", false, "and only that base name"},
		{"*.proto", "protocol/message.proto", true, "the repository's other rule, on a path it covers"},
		{"*.proto", "protocol/message.pb.go", false, "and one it does not"},
	} {
		if matched := gitAttributesPatternMatches(probe.pattern, probe.path); matched != probe.matches {
			t.Errorf("%q against %q answered %v, want %v: %s", probe.pattern, probe.path, matched, probe.matches, probe.why)
		}
	}
}

// TestTheCommittedSeedCorpusIsPinnedAsBinary is the one property in this file that is not about
// the codec, and it is here because the corpus stops being evidence the moment a checkout is
// allowed to rewrite it.
//
// core.autocrlf=true is set at system scope on the windows boxes that build this repository, and
// git decides text from binary by looking for a NUL octet in a file's first 8000. The corpus is
// REGENERATED whenever an axis moves, and a seed that happens to hold no NUL and does hold an
// 0x0a would be rewritten on checkout into bytes no decoder accepts. That failure arrives as a
// corpus that stops decoding on somebody else's machine, which reads as a codec bug.
//
// The question is asked once per COMMITTED SEED rather than once about the folder they live in,
// because those are different questions and only the first one is the one that matters: a rule
// that names the folder and does not reach the files inside it pins nothing at all, and
// `mls/testdata/corpus/*` is exactly such a rule.
func TestTheCommittedSeedCorpusIsPinnedAsBinary(t *testing.T) {
	attributes := filepath.Join("..", ".gitattributes")
	body, err := os.ReadFile(attributes)
	if err != nil {
		t.Fatalf("read %s: %v", attributes, err)
	}
	rules := gitAttributesTextRules(string(body))
	if len(rules) == 0 {
		t.Fatalf("%s carries no rule that mentions the text attribute at all", attributes)
	}

	unpinned := []string{}
	pins := map[string]bool{}
	convertible, seeds := 0, 0
	for _, codec := range seedCodecs() {
		names, onDisk := readSeedCorpus(t, codec.target)
		for _, name := range names {
			seeds++
			seedPath := strings.Join([]string{"mls", "testdata", "corpus", codec.target, name}, "/")
			isPinned, decidedBy := gitAttributesPinsAsBinary(rules, seedPath)
			if !isPinned {
				unpinned = append(unpinned, seedPath)
				continue
			}
			pins[decidedBy] = true

			// measured rather than asserted, so the log says whether the pin is currently
			// carrying weight or is prophylactic.
			seed := onDisk[name]
			head := seed
			if len(head) > 8000 {
				head = head[:8000]
			}
			if !bytes.Contains(head, []byte{0x00}) && bytes.ContainsAny(seed, "\r\n") {
				convertible++
			}
		}
	}
	if len(unpinned) > 0 {
		t.Fatalf("%d of %d committed seeds are not left binary by %s (%s is one); with core.autocrlf on, a seed git reads as text is rewritten on checkout and the committed corpus stops being what was committed",
			len(unpinned), seeds, attributes, unpinned[0])
	}

	// the negative control on the rule set as it actually stands. A .gitattributes that marked
	// everything binary would satisfy the loop above having said nothing about the corpus, and
	// this package's own source is the nearest file that must NOT be pinned.
	if isPinned, decidedBy := gitAttributesPinsAsBinary(rules, "mls/key_schedule_roundtrip_test.go"); isPinned {
		t.Errorf("%s marks this package's own source binary as well (%q), so the assertion above holds for a reason that has nothing to do with the corpus",
			attributes, decidedBy)
	}

	t.Logf("%d seeds pinned by %d rule(s) %v; %d of them would otherwise be eligible for end of line conversion",
		seeds, len(pins), slices.Sorted(maps.Keys(pins)), convertible)
}

// ---------------------------------------------------------------------------
// the targets that read the corpus
// ---------------------------------------------------------------------------

// seedCorpusRunner is the name of the function below, as a string, because the gate that checks
// a corpus folder has a reader looks for a call to it in the target's body and a literal spelled
// twice is a literal that drifts.
//
// It names the RUNNER rather than the loader it wraps, and that is the correction: a target that
// loaded the seeds and then ran the engine itself would satisfy a gate looking for the loader while
// handing nothing over, which is the defect fuzzTheCommittedSeedCorpus exists to observe.
const seedCorpusRunner = "fuzzTheCommittedSeedCorpus"

// addSeedCorpus hands one target's committed seeds to the fuzzing engine and answers how many it
// handed over.
//
// Go's own corpus directory is testdata/fuzz/<Target>, and these seeds deliberately do not live
// there: testdata/fuzz is where the engine WRITES the inputs it finds, and a directory the tool
// rewrites is not a directory a corpus can be evidence in. Seeds committed under
// testdata/corpus/<Target> are read into the target explicitly instead.
//
// The count below is NOT the check that the hand off happened, and reading it as one is the defect
// that was found here: it is incremented in the same loop body as the f.Add it would be guarding,
// so replacing that call with a discard leaves the counter at 141 and this function silent. What
// observes the hand off is the caller, from the far side, by counting what the ENGINE ran.
func addSeedCorpus(f *testing.F, target string) int {
	f.Helper()
	names, bodies := readSeedCorpus(f, target)
	added := 0
	for _, name := range names {
		f.Add(bodies[name])
		added++
	}
	// readSeedCorpus already refuses an empty folder, so this fires only for a folder that held
	// files none of which were read -- which is a different failure and still a silent one.
	if added == 0 {
		f.Fatalf("%s: not one committed seed reached the fuzzing corpus", target)
	}
	return added
}

// fuzzTheCommittedSeedCorpus is the whole hand off, and every target in this package with a
// committed corpus goes through it: load the seeds, give them to the engine, state the property
// over whatever the engine hands back, and then assert that the engine ACTUALLY RAN them.
//
// The last clause is the one that was missing, and its absence was invisible from everywhere. With
// f.Add replaced by a discard the whole committed corpus stopped reaching any engine: all 401 seed
// subtests across the four targets vanished, addSeedCorpus's own zero check stayed silent because
// its counter lives in the same loop body, TestEveryCommittedCorpusFolderIsReadByAFuzzTarget stayed
// green because it asks whether a target NAMES its loader rather than whether a seed moved, and the
// only trace in the whole suite was a pass count falling from 6242 to 5841 that nothing asserts on.
//
// So the number is taken from the far side of the engine. go test runs each seed of a target as a
// subtest of it -- testing.F.Fuzz's default arm is that loop -- so the property below is called once
// per seed, and a run that called it fewer times than seeds were handed over is a run in which the
// hand off did not happen. That is the same fact the 401 vanishing subtests were, asserted where a
// plain go test run reaches it.
//
// The property answers a BOOL, and that is the second half of the same correction. Every target
// here states its property through syntax.CheckRoundTrip, which returns nil for an input that does
// not decode -- correct against its contract, and silent. So a target handed a corpus of octets
// none of which decode evaluates the refusal half of its property, never reaches the round trip
// half, and reports exactly what a complete run reports: the hand off happened, the engine ran,
// every input passed. p1 measured uniform random bytes reaching the round trip property 14 times
// in 4096 against the SIMPLEST structure in this tree, so that is not a hypothetical corpus, it is
// what an unseeded or a stale one looks like. What each target answers is whether THAT input
// reached the substance of its property, and a run in which none did is a failure here rather than
// a green run nobody can tell from a real one.
func fuzzTheCommittedSeedCorpus(f *testing.F, target string, property func(t *testing.T, encoded []byte) bool) {
	f.Helper()
	added := addSeedCorpus(f, target)
	// plain counters and not atomics: the default arm runs the seeds one at a time and joins
	// each before starting the next, and nothing here calls t.Parallel.
	executed, reached := 0, 0
	f.Fuzz(func(t *testing.T, encoded []byte) {
		executed += 1
		if property(t, encoded) {
			reached += 1
		}
	})
	if !seedCorpusExecutionIsObservable() {
		return
	}
	// fewer rather than not equal, because testdata/fuzz may hold inputs the engine FOUND, and
	// those are executions this hand off did not supply.
	if executed < added {
		f.Errorf("%s: %d committed seeds were handed to the engine and it ran the property %d times; the corpus is reaching no engine, and every property this package states over it is stated over bytes nothing consumes",
			target, added, executed)
	}
	if reached == 0 {
		f.Errorf("%s: the engine ran the property over %d inputs and not one of them decoded, so this target evaluated the refusal half of its property and nothing else; a corpus that reaches no decoder is indistinguishable at the reporting layer from one that reaches every arm",
			target, executed)
	}
	f.Logf("%s: %d committed seeds handed over, %d engine executions, %d of them reaching the round trip property",
		target, added, executed, reached)
}

// seedCorpusExecutionIsObservable reports whether THIS invocation of go test is one in which the
// engine runs every committed seed in this process, which is the only invocation the count above
// can be asserted on.
//
// Two invocations are not. Under -fuzz the corpus is fanned out to worker PROCESSES and the arm of
// testing.F.Fuzz that runs seeds in the caller's process is not the arm taken, so the coordinator
// and each worker would both count zero. And a -run pattern carrying a subtest element selects
// individual seeds by name, which makes a partial count the correct outcome rather than a defect.
//
// Both are read off the test binary's own flags rather than guessed, and both are the narrowest
// thing that can be said: a -run pattern without a separator selects whole targets, and every seed
// of a selected target runs.
func seedCorpusExecutionIsObservable() bool {
	if fuzzing := flag.Lookup("test.fuzz"); fuzzing != nil && fuzzing.Value.String() != "" {
		return false
	}
	if worker := flag.Lookup("test.fuzzworker"); worker != nil && worker.Value.String() == "true" {
		return false
	}
	if run := flag.Lookup("test.run"); run != nil && strings.Contains(run.Value.String(), "/") {
		return false
	}
	return true
}

// FuzzGroupContextRoundTrip is gate 4 properties 1 and 2 on the section 8.1 group context in
// their randomized form: no panic on adversarial input, and an encoding that decodes must
// re-encode to the bytes it came from.
//
// The property is stated through syntax.CheckRoundTrip rather than open coded, which is
// deliberate: that helper is the one every Gate 4 target in this tree reaches its codec through,
// TestCheckRoundTripReportsTheViolationsItIsHanded below is the positive control on it, and a
// target that restated the comparison locally would be green against a helper that had stopped
// making it.
//
// Seeds that do not decode are not failures. A fuzzer spends most of its budget on inputs no
// decoder accepts, and an obligation on those would drown the one that matters.
func FuzzGroupContextRoundTrip(f *testing.F) {
	fuzzTheCommittedSeedCorpus(f, groupContextSeedTarget, func(t *testing.T, encoded []byte) bool {
		if err := syntax.CheckRoundTrip[GroupContext, *GroupContext](encoded); err != nil {
			t.Fatalf("%d octets %x: %v", len(encoded), encoded, err)
		}
		return syntax.Unmarshal(encoded, &GroupContext{}) == nil
	})
}

// FuzzPreSharedKeyIdRoundTrip is the same two properties on the section 8.4 pre shared key id.
// It is a separate target rather than a second case of one, because the fuzzing engine keeps its
// coverage feedback and its found corpus per target, and one target over two grammars spends
// half its budget on each while reporting one.
func FuzzPreSharedKeyIdRoundTrip(f *testing.F) {
	fuzzTheCommittedSeedCorpus(f, preSharedKeyIdSeedTarget, func(t *testing.T, encoded []byte) bool {
		if err := syntax.CheckRoundTrip[PreSharedKeyId, *PreSharedKeyId](encoded); err != nil {
			t.Fatalf("%d octets %x: %v", len(encoded), encoded, err)
		}
		return syntax.Unmarshal(encoded, &PreSharedKeyId{}) == nil
	})
}

// declaredFuzzTargets reads this package's test source and returns, for every fuzz target it
// declares, the set of identifiers that target's body names.
//
// The target set is derived from the SIGNATURE rather than from the name, because that is what
// the go tool derives it from: a func(*testing.F) is a fuzz target and nothing else is, whatever
// it is called.
func declaredFuzzTargets(t *testing.T, directory string) map[string]map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("read %s: %v", directory, err)
	}
	fileSet := token.NewFileSet()
	targets := map[string]map[string]bool{}
	read := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fileSet, filepath.Join(directory, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		read++
		for _, declaration := range parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || !isFuzzTargetSignature(function) {
				continue
			}
			identifiers := map[string]bool{}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				if identifier, isIdentifier := node.(*ast.Ident); isIdentifier {
					identifiers[identifier.Name] = true
				}
				return true
			})
			targets[function.Name.Name] = identifiers
		}
	}
	if read == 0 {
		t.Fatalf("no _test.go file was read from %s, so this derivation proves nothing", directory)
	}
	return targets
}

// isFuzzTargetSignature holds the go tool's definition: a top level function taking exactly one
// *testing.F and returning nothing.
func isFuzzTargetSignature(function *ast.FuncDecl) bool {
	if function.Recv != nil || function.Body == nil || function.Type.Params == nil {
		return false
	}
	if len(function.Type.Params.List) != 1 || len(function.Type.Params.List[0].Names) != 1 {
		return false
	}
	if function.Type.Results != nil && len(function.Type.Results.List) != 0 {
		return false
	}
	pointer, isPointer := function.Type.Params.List[0].Type.(*ast.StarExpr)
	if !isPointer {
		return false
	}
	selector, isSelector := pointer.X.(*ast.SelectorExpr)
	if !isSelector || selector.Sel.Name != "F" {
		return false
	}
	packageName, isIdentifier := selector.X.(*ast.Ident)
	return isIdentifier && packageName.Name == "testing"
}

// TestEveryCommittedCorpusFolderIsReadByAFuzzTarget is why the corpus is evidence rather than
// 287 files.
//
// The first version of this file committed both folders and declared neither target, on the
// belief that another plan owned the two names. It did not: no plan in the tree named either, so
// no fuzz engine had ever opened one seed, the verification step written for it could not be
// run, and every property stated over the corpus was stated over bytes nothing consumed. That is
// invisible from every other property in this file, all of which read the folder directly and
// none of which cares whether a fuzz target exists.
//
// Both sides are derived -- the folders that are on disk, and the func(*testing.F) declarations
// this package's test source actually holds -- because a table naming "the targets we have" is
// the enumeration rule 5 forbids, and it is a table that stays green after the target it names
// has been deleted.
func TestEveryCommittedCorpusFolderIsReadByAFuzzTarget(t *testing.T) {
	targets := declaredFuzzTargets(t, ".")
	if len(targets) == 0 {
		t.Fatal("this package's test source declares no func(*testing.F) at all, so this gate cannot tell a corpus with a reader from a corpus without one")
	}

	root := filepath.Join("testdata", "corpus")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read %s: %v", root, err)
	}
	folders := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			t.Errorf("%s holds the file %q; every seed lives under a folder named for the target that reads it", root, entry.Name())
			continue
		}
		folders++
		body, declared := targets[entry.Name()]
		if !declared {
			t.Errorf("%s/%s holds committed seeds and this package declares no func %s(f *testing.F); a corpus no target names is a corpus no fuzz engine ever reads, and every property stated over it is stated over files nothing consumes",
				root, entry.Name(), entry.Name())
			continue
		}
		if !seedFolderHoldsFilesDirectly(t, filepath.Join(root, entry.Name())) {
			// a folder of folders belongs to a loader with its own layout, and the one in this
			// file refuses to recurse; there is nothing here for this half of the gate to say.
			continue
		}
		if !body[seedCorpusRunner] {
			t.Errorf("%s/%s holds seeds directly and func %s does not call %s, so those seeds reach the fuzzing engine by no route this gate can see and nothing counts what the engine ran",
				root, entry.Name(), entry.Name(), seedCorpusRunner)
		}
	}
	if folders == 0 {
		t.Fatalf("%s holds no corpus folder, so this gate asserted nothing", root)
	}
	t.Logf("%d committed corpus folders, each read by a declared fuzz target; %d fuzz targets in the package",
		folders, len(targets))
}

// seedFolderHoldsFilesDirectly reports whether a corpus folder holds seeds itself rather than
// holding further folders that do.
func seedFolderHoldsFilesDirectly(t *testing.T, directory string) bool {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("read %s: %v", directory, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			return true
		}
	}
	return false
}
