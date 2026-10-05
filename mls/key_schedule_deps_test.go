// Compile time pins on every cross plan symbol the key schedule and the secret tree
// consume, at the signatures the canonical interface registry gives them. A signature
// change in another plan breaks the build here rather than three tasks later, and a pin
// that pins the wrong shape catches the drift by failing, which is the whole point of
// the file.
//
// Only two of the producing plans have landed in this package whole: syntax and the crypto
// provider. Tree math, the registry enums and extensions and the validation plan's ValSem
// codes are all still elsewhere, so they cannot be pinned yet — an undefined name is a
// build failure, not a red test, and a build failure takes the whole package down including
// everything already green.
//
// Framing's ContentType is the second exception and is pinned below for a different reason
// from p8's: it did not land from p6 at all, it landed HERE, because this plan's secret tree
// implements the MessageKeySource interface p6 declares and that interface is keyed on it.
// content_type.go was the declaration, on the standing agreement that p6's own landing would
// delete that file rather than add a second one beside it. p6 task 1 has landed and did that:
// the declaration is framing.go's now. The pin is owed either way, because the rule this file
// runs on is that a name package mls declares is a name this file pins, whichever plan wrote it.
//
// p8's vector harness is the other exception and is pinned below. It landed in vectors_test.go
// rather than in a production file, which is where the detector that should have noticed
// had a hole: the scan behind TestEveryCrossPlanSymbolThatHasLandedIsPinnedHere read only
// non test files, so a test-only surface — and the vector harness is test-only by design —
// could land under any plan with all five of its names still listed here as pending. The
// scan reads every go file of the package now.
//
// What stands in for them is TestEveryCrossPlanSymbolThatHasLandedIsPinnedHere. It reads
// this package's own source and fails the moment one of those symbols appears, which is
// exactly the moment its pin has to be written. Landing a producing plan without adding
// its pins is therefore loud rather than silent, and the window in which this plan
// consumes an unpinned signature is one commit wide.
package mls

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect/mls/syntax"
)

// Pinned free functions from the crypto plan and the syntax plan, the two that have
// landed. Each is an assignment to a function type written out in full, so a parameter
// added, a result dropped or a named type swapped for its underlying one all fail here.
var (
	_ func(CipherSuite) (CryptoProvider, error) = NewCryptoProvider

	// registry section 2 — the syntax entry points, not append style free functions.
	// There is no syntax.WriteVarVec and no Bytes() without the error.
	_ func() *syntax.Writer                  = syntax.NewWriter
	_ func([]byte) *syntax.Reader            = syntax.NewReader
	_ func(syntax.Marshaler) ([]byte, error) = syntax.Marshal
	_ func([]byte, syntax.Unmarshaler) error = syntax.Unmarshal

	// the same pair at a caller chosen bound, which p5 task 11 is the first of this
	// package to reach: the ratchet_tree array of RFC 9420 section 12.4.3.3 is the one
	// structure p1 allows past MaxVectorLength, and marshalRatchetTree and
	// UnmarshalRatchetTree are these two with MaxRatchetTreeLength fixed. The limit is
	// the last parameter of each and it is an int, so the pin is what fails if either
	// grows an argument or takes the bound as something else.
	_ func(syntax.Marshaler, int) ([]byte, error) = syntax.MarshalLimit
	_ func([]byte, syntax.Unmarshaler, int) error = syntax.UnmarshalLimit

	// the writer at a caller chosen bound, which p7 task 11's persisted state blob is the
	// first of this package to open directly: a group's state carries the encoded ratchet
	// tree as one opaque field, so the writer that frames it has to be able to hold what
	// MarshalLimit above was allowed to produce. The bound is the only parameter and it is
	// an int, so the pin is what fails if it grows an argument or takes the bound as
	// something else.
	_ func(int) *syntax.Writer = syntax.NewWriterLimit

	// registry section 2 again, the vector pair. extensions<V> is the first structure in
	// this package to reach them, and they are generic, so the pin instantiates them at
	// the element type this package actually uses: a callback shape that moved would
	// still satisfy an uninstantiated mention and stop compiling here.
	_ func(*syntax.Writer, []Extension, func(*syntax.Writer, Extension) error) error     = syntax.WriteVector[Extension]
	_ func(*syntax.Reader, func(*syntax.Reader) (Extension, error)) ([]Extension, error) = syntax.ReadVector[Extension]

	// registry section 6.2 — the extension vector codec is the inline, writer and reader
	// taking pair of override O-4, never the byte returning MarshalExtensions p5 first
	// proposed: GroupContext carries the vector inline, so a byte returning half would
	// put a hand written WriteOpaque at this call site and four others.
	_ func(*syntax.Writer, []Extension) error   = WriteExtensions
	_ func(*syntax.Reader) ([]Extension, error) = ReadExtensions

	// this plan, task 3 — Clone is a method expression here so the pin covers the
	// receiver as well as the result. A Clone that started returning a value rather than
	// a pointer would still satisfy every call site that immediately dereferences it.
	_ func(*GroupContext) *GroupContext = (*GroupContext).Clone
)

// Pinned values, which drift differently from functions: a constant retyped or a
// sentinel replaced by a value of another type still compiles at most call sites and
// changes what errors.Is answers.
var (
	_ int = syntax.MaxVectorLength
	// the ratchet tree exception, and the only raised bound in this package. A constant
	// retyped here would compile at both call sites and change which trees decode.
	_ int   = syntax.MaxRatchetTreeLength
	_ error = syntax.ErrTrailingBytes
	_ error = syntax.ErrLengthExceedsMax

	_ CipherSuite = CipherSuiteX25519AesGcm128Sha256Ed25519
	_ CipherSuite = CipherSuiteX25519ChaCha20Sha256Ed25519

	// registry section 3.2 — both hpke key types are byte slices. The assignment is
	// the pin: a struct or an array behind either name stops compiling here.
	_ []byte = HpkePublicKey(nil)
	_ []byte = HpkePrivateKey(nil)

	// registry section 6.1 — both registry enums are uint16 and nothing narrower. The
	// 0xffff conversion is the pin: it is a compile error the moment either type is
	// retyped to a uint8, which a plain assignment of a small constant would not catch.
	_ ProtocolVersion = ProtocolVersionMls10
	_ ProtocolVersion = ProtocolVersion(0xffff)
	_ ExtensionType   = ExtensionType(0xffff)

	// registry section 6.2 — Extension is the two field struct with a slice body, in the
	// registry's field order and spelling. A field renamed or retyped stops compiling.
	_ Extension = Extension{ExtensionType: ExtensionTypeRatchetTree, ExtensionData: nil}
	_ []byte    = Extension{}.ExtensionData

	// this plan, task 3 — every field of GroupContext at the registry's type. This is
	// the pin the whole key schedule rests on: the structure is hashed into the confirmed
	// transcript and mixed into every epoch derivation, so a field retyped here is two
	// members deriving different secrets, and the field list is written out one line each
	// so that a field ADDED is a line missing rather than a silent widening.
	_ ProtocolVersion = GroupContext{}.Version
	_ CipherSuite     = GroupContext{}.CipherSuite
	_ []byte          = GroupContext{}.GroupId
	_ uint64          = GroupContext{}.Epoch
	_ []byte          = GroupContext{}.TreeHash
	_ []byte          = GroupContext{}.ConfirmedTranscriptHash
	_ []Extension     = GroupContext{}.Extensions

	// this plan, task 13 — every field of PreSharedKeyId at the registry's type, written
	// out one line each for the same reason GroupContext's block is: a field ADDED shows
	// up as a line missing here rather than as a widening nobody reads. The two enums are
	// pinned through a 0xff conversion, which is a compile error the moment either is
	// widened past the octet the wire format gives it — an assignment of a small constant
	// would not catch that, and a psktype written at two octets moves every byte of a
	// psk_secret preimage.
	_ PskType            = PskType(0xff)
	_ ResumptionPskUsage = ResumptionPskUsage(0xff)
	_ PskType            = PreSharedKeyId{}.PskType
	_ []byte             = PreSharedKeyId{}.PskId
	_ ResumptionPskUsage = PreSharedKeyId{}.Usage
	_ []byte             = PreSharedKeyId{}.PskGroupId
	_ uint64             = PreSharedKeyId{}.PskEpoch
	_ []byte             = PreSharedKeyId{}.PskNonce
)

// p8 validation and interop, task 7 — the vector family harness. These five landed in
// vectors_test.go, so they are pinned at the signatures the interface registry gives them
// exactly as a production surface would be: this plan's task 16 runner registers against
// RegisterVectorFamily and reads its corpus through LoadVectorFile, and p8's own landing
// replaces that file rather than adding a second registry beside it.
//
// The struct is pinned field by field for the same reason GroupContext is: a field added is
// a line missing here, and the two function fields are what the registry actually calls, so
// a Verify that grew a return or a Generate that lost its *testing.T stops compiling here
// rather than at the one call site that happens to be written today.
var (
	_ func(VectorFamily)                         = RegisterVectorFamily
	_ func(*testing.T, string) []json.RawMessage = LoadVectorFile
	_ func(*testing.T, string) []byte            = MustHex
	_ func([]byte) string                        = HexOf

	_ int                               = VectorFamily{}.Number
	_ string                            = VectorFamily{}.Name
	_ string                            = VectorFamily{}.File
	_ string                            = VectorFamily{}.Slice
	_ func(*testing.T, json.RawMessage) = VectorFamily{}.Verify
	_ func(*testing.T) json.RawMessage  = VectorFamily{}.Generate
)

// p6 framing, task 1 — ContentType and the three code points it registers. This is the one
// name of p6 the secret tree reaches, because the MessageKeySource interface p6 declares is
// keyed on it, and it is the second cross plan surface to land in package mls ahead of its
// owner. The terms were p8's vector harness's: content_type.go declared it at the signature
// the interface registry gives it, and p6's own landing was to delete that file rather than add
// a second declaration beside it. p6 task 1 has landed and framing.go carries the declaration,
// so this block is now what makes a p6 edit that changes its shape a build failure here.
//
// The width takes two lines, and the 0xff conversion is only one of them. That conversion says
// that 255 FITS, which rules out a signed octet and nothing else: ContentType(0xff) compiles
// unchanged at uint16, at uint32 and at every wider type. So the sentence this block used to
// carry -- that the conversion "is a compile error the moment the type is widened past the
// octet the wire format gives it" -- was false, and it was false in the one file a later plan
// reads to decide what still protects it. Measured, not supposed: with type ContentType uint8
// changed to uint16 this package BUILT and 657 of its 658 tests passed, the single failure
// being TestContentTypeCarriesTheWireValuesTheRegistryGivesIt, which holds the width through
// int(^ContentType(0))+1 != 256 and is therefore what was holding it all along.
//
// The upper half is the constant below the block. ^ContentType(0) is a typed constant equal to
// the type's own maximum, so converting it to a uint8 is exactly the statement "this type is
// no wider than an octet", and a widened type stops COMPILING here rather than failing a test
// somewhere. That is the failure this block wants: a content type read at two octets moves
// every field after it in a PrivateMessage header, and nothing about that is a type error at
// any call site.
//
// The three constants are pinned as members of the type, which holds their spelling and their
// type but NOT their values: a code point that moved is a number and not a type error, so
// TestContentTypeCarriesTheWireValuesTheRegistryGivesIt is what holds those, and it derives
// them off the type rather than reading this block.
var (
	_ ContentType = ContentType(0xff)
	_ ContentType = ContentTypeApplication
	_ ContentType = ContentTypeProposal
	_ ContentType = ContentTypeCommit
)

// the upper half of the width, which the conversions above cannot give. A type wider than an
// octet makes this constant overflow its conversion and the package stops building.
const _ = uint8(^ContentType(0))

// p3 tree math — the index surface registry section 4 gives this plan, pinned at the
// shapes convention C3 fixes: counts are LeafCount and indices are LeafIndex/NodeIndex,
// NodeWidth answers a uint32, Root is two valued, and Level is a METHOD. The secret tree
// descends by these and by nothing else, so a Root that lost its error or a Level that
// became a free function has to stop compiling here rather than be absorbed by a shim at
// the one call site — a shim turning Root's error into node zero builds a tree whose
// descent never terminates, which is the exact failure C3 was written against.
//
// Left and Right are pinned separately and identically on purpose. Their signatures
// cannot tell them apart, so what this block holds is that both exist at that shape; the
// secret tree's own test is what holds that the descent calls the one it means.
var (
	_ func(LeafCount) (NodeIndex, error) = Root
	_ func(NodeIndex) (NodeIndex, error) = Left
	_ func(NodeIndex) (NodeIndex, error) = Right
	_ func(LeafCount) uint32             = NodeWidth
	_ func(LeafCount) uint32             = TreeDepth
	_ func(LeafCount) bool               = IsFullLeafCount

	// the four p3 entry points the secret tree's own tests descend and account by. They are
	// pinned for the same reason the descent's four are: a test that re-implements one of
	// them locally is a second definition of the tree's shape, and this file has watched
	// exactly that happen -- a node to leaf step written as node/2 agrees with p3 on every
	// even index and disagrees on every odd one, where p3 REFUSES a parent index and a local
	// division silently truncates it. DirectPath supplies the independent ancestry every
	// expected node secret is replayed from, and SubtreeSpan supplies the size of the subtree
	// a retained node roots, which the forward secrecy closure counts itself against.
	_ func(NodeIndex, LeafCount) ([]NodeIndex, error) = DirectPath
	_ func(NodeIndex) (NodeIndex, NodeIndex)          = SubtreeSpan

	// method expressions, so the receiver is pinned with the signature. A NodeIndex.Level
	// respelled as a free function Level(NodeIndex) would satisfy neither line.
	_ func(LeafIndex) NodeIndex          = LeafIndex.NodeIndex
	_ func(NodeIndex) uint32             = NodeIndex.Level
	_ func(NodeIndex) (LeafIndex, error) = NodeIndex.LeafIndex

	// the three index types are uint32 underneath and nothing narrower. The 0xffffffff
	// conversion is the pin: it is a compile error the moment one is retyped to a uint16,
	// and an assignment of a small constant would not catch that. It is NOT a claim the
	// value is in range — MaxLeafCount is half of it — only that the width is the one
	// every downstream index computation was written against.
	_ LeafIndex = LeafIndex(0xffffffff)
	_ NodeIndex = NodeIndex(0xffffffff)
	_ LeafCount = LeafCount(0xffffffff)

	// the two p3 sentinels the secret tree's constructor wraps and its tests match on. A
	// sentinel replaced by a value of another type still compiles at the call site and
	// changes what errors.Is answers, which is the whole of what the constructor promises
	// here: a caller can ask either "is this a secret tree range failure" or "was the leaf
	// count not a power of two" and get the true answer.
	_ error = ErrLeafCountRange
	_ error = ErrLeafCountNotFull
)

// pinnedCodec exists only to carry the C1 method set. Declaring the two methods and then
// asserting the interface is what pins their signatures: syntax.Codec satisfied by a
// type this file wrote means MarshalMLS still takes *syntax.Writer and returns error,
// and UnmarshalMLS still takes *syntax.Reader and returns error. Asserting the interface
// against an existing type would only pin that type.
type pinnedCodec struct{}

// MarshalMLS pins the C1 encode half. It encodes nothing; the signature is the assertion.
func (self *pinnedCodec) MarshalMLS(w *syntax.Writer) error { return nil }

// UnmarshalMLS pins the C1 decode half. It decodes nothing; the signature is the assertion.
func (self *pinnedCodec) UnmarshalMLS(r *syntax.Reader) error { return nil }

var (
	_ syntax.Marshaler   = (*pinnedCodec)(nil)
	_ syntax.Unmarshaler = (*pinnedCodec)(nil)
	_ syntax.Codec       = (*pinnedCodec)(nil)

	// the three structures of this plan that have landed carry the same method set. Their
	// own files assert this too; repeating it here is deliberate, because the assertion
	// in a production file is one an author fixing a build failure can delete, and this
	// file is counted.
	_ syntax.Codec = (*Extension)(nil)
	_ syntax.Codec = (*GroupContext)(nil)
	_ syntax.Codec = (*PreSharedKeyId)(nil)
)

// TestConsumedCryptoProviderShape pins the CryptoProvider method set this plan calls,
// and the three lengths every derivation in it is sized by. Extract is pinned at
// (salt, ikm) — guardrail 1 — and that order is not observable from a signature of two
// byte slices, so what a signature can hold is held here and the order itself is the
// crypto plan's own vector tests.
func TestConsumedCryptoProviderShape(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	var (
		_ func() CipherSuite                                  = crypto.Suite
		_ func([]byte, []byte) []byte                         = crypto.Extract
		_ func([]byte, []byte, int) []byte                    = crypto.Expand
		_ func([]byte, string, []byte, int) []byte            = crypto.ExpandWithLabel
		_ func([]byte, string) []byte                         = crypto.DeriveSecret
		_ func([]byte, string, uint32, int) []byte            = crypto.DeriveTreeSecret
		_ func([]byte) []byte                                 = crypto.Hash
		_ func([]byte, []byte) []byte                         = crypto.Mac
		_ func([]byte, []byte, []byte) bool                   = crypto.MacVerify
		_ func() int                                          = crypto.HashSize
		_ func() int                                          = crypto.KeySize
		_ func() int                                          = crypto.NonceSize
		_ func(int) []byte                                    = crypto.Random
		_ func([]byte) (HpkePrivateKey, HpkePublicKey, error) = crypto.DeriveKeyPair
	)
	if crypto.Suite() != CipherSuiteX25519ChaCha20Sha256Ed25519 {
		t.Fatalf("Suite() = %#04x, want the suite it was constructed for", uint16(crypto.Suite()))
	}
	if crypto.HashSize() != 32 {
		t.Fatalf("HashSize = %d, want 32", crypto.HashSize())
	}
	if crypto.KeySize() != 32 {
		t.Fatalf("KeySize = %d, want 32", crypto.KeySize())
	}
	if crypto.NonceSize() != 12 {
		t.Fatalf("NonceSize = %d, want 12", crypto.NonceSize())
	}
	if n := len(crypto.Extract(nil, nil)); n != crypto.HashSize() {
		t.Fatalf("Extract produced %d bytes, want HashSize %d", n, crypto.HashSize())
	}
	if n := len(crypto.DeriveSecret(make([]byte, 32), "pin")); n != crypto.HashSize() {
		t.Fatalf("DeriveSecret produced %d bytes, want HashSize %d", n, crypto.HashSize())
	}
}

// TestConsumedHpkePublicKeyIsASlice pins that HpkePublicKey is a byte slice and carries
// no Bytes method. The Bytes() pin the first draft of this plan carried does not exist
// and never did, so the vector's external_pub is compared against the slice directly.
//
// The absence half is asserted rather than described: a Bytes method appearing later
// would give a caller two spellings of the same bytes, which is how one of them ends up
// hex encoded and the other not.
func TestConsumedHpkePublicKeyIsASlice(t *testing.T) {
	pub := HpkePublicKey([]byte{0x01, 0x02})
	var raw []byte = pub
	if len(raw) != 2 || raw[0] != 0x01 || raw[1] != 0x02 {
		t.Fatalf("HpkePublicKey did not convert to its bytes: %v", raw)
	}
	if _, ok := any(pub).(interface{ Bytes() []byte }); ok {
		t.Fatal("HpkePublicKey grew a Bytes method; registry section 3.2 says it is a slice and nothing else")
	}
	if _, ok := any(HpkePrivateKey(nil)).(interface{ Bytes() []byte }); ok {
		t.Fatal("HpkePrivateKey grew a Bytes method; registry section 3.2 says it is a slice and nothing else")
	}
}

// TestConsumedSyntaxWriterShape pins the syntax reader and writer surface. Every leaf
// write returns nothing (C2): the error is sticky and is collected once, at Bytes().
func TestConsumedSyntaxWriterShape(t *testing.T) {
	w := syntax.NewWriter()
	var (
		_ func(uint8)            = w.WriteUint8
		_ func(uint16)           = w.WriteUint16
		_ func(uint32)           = w.WriteUint32
		_ func(uint64)           = w.WriteUint64
		_ func([]byte)           = w.WriteOpaque
		_ func([]byte)           = w.WriteRaw
		_ func() ([]byte, error) = w.Bytes
		_ func() error           = w.Err
	)
	w.WriteUint16(0x0001)
	w.WriteOpaque([]byte{0x02, 0x03})
	b, err := w.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	if len(b) != 5 {
		t.Fatalf("encoded %d bytes, want 5: two for the uint16, one varint length, two opaque", len(b))
	}
	r := syntax.NewReader(b)
	var (
		_ func() (uint8, error)     = r.ReadUint8
		_ func() (uint16, error)    = r.ReadUint16
		_ func() (uint32, error)    = r.ReadUint32
		_ func() (uint64, error)    = r.ReadUint64
		_ func() ([]byte, error)    = r.ReadOpaque
		_ func(int) ([]byte, error) = r.ReadRaw
		_ func() error              = r.Done
	)
	if _, err := r.ReadUint16(); err != nil {
		t.Fatalf("ReadUint16: %v", err)
	}
	if _, err := r.ReadOpaque(); err != nil {
		t.Fatalf("ReadOpaque: %v", err)
	}
	if err := r.Done(); err != nil {
		t.Fatalf("Done: %v", err)
	}
	if syntax.MaxVectorLength != 1<<20 {
		t.Fatalf("syntax.MaxVectorLength = %d, registry section 2 says 1<<20", syntax.MaxVectorLength)
	}
}

// keyScheduleVectorFamilies is the mlswg families this plan gates: the four of its file
// structure table, and message-protection.json, which task 10 reads. Nothing in the tree
// derives this set today — the runners that would name it are later tasks — so the count
// assertion in the test below is what stops the list shrinking, since a deleted entry would
// otherwise leave a shorter list that still passes every per file check.
//
// message-protection.json is here because the membership tag's known answer comes out of it:
// that corpus publishes an epoch's membership_key and the public messages framed under it, and
// no other family carries a membership_tag at all. A family this plan READS and does not gate
// is a family whose absence would be reported as a decode failure in the middle of a known
// answer test rather than as the missing vendoring it is.
var keyScheduleVectorFamilies = []string{
	"key-schedule.json",
	"psk_secret.json",
	"transcript-hashes.json",
	"secret-tree.json",
	"message-protection.json",
}

// TestVectorFilesPresent asserts the vector families this plan gates were vendored
// by the validation plan's single vendoring task, are not empty, and are covered by the
// digest manifest beside them. This plan reads them and never writes them.
//
// The manifest half matters more than the presence half: a file that is present but
// unlisted is a file nothing pins, and a file whose digest disagrees with the manifest is
// a file that changed after it was pinned. TestVectorFilesArePinned in vectors_pin_test.go
// checks all sixteen families; this checks the five this plan cannot run without, so a
// change to that file's list cannot quietly drop one of them.
//
// The manifest half is worth exactly what the manifest is worth, and on its own that is
// less than it reads: VECTORS.sha256 is computed over the bytes in this checkout, so a
// file edited together with its manifest line verifies. The claim "a file whose digest
// disagrees with the manifest is a file that changed after it was pinned" is true; the
// claim it invites — that a file agreeing with the manifest is the file upstream published
// — is not, and this plan should not have leaned on it. What makes the second claim true
// is vectors_upstream_test.go, which holds every vendored family to the digest mlswg
// published at the commit interop/PINS.md pins. Read the two together.
//
// The known smudge, kept here because this plan reads these four files: all sixteen were
// vendored through a checkout with core.autocrlf on and none is byte-identical to upstream.
// Normalised, all sixteen match, so no KAT is wrong; re-vendoring at LF belongs to the task
// that vendored them.
func TestVectorFilesPresent(t *testing.T) {
	if len(keyScheduleVectorFamilies) != 5 {
		t.Fatalf("this plan gates %d vector families, want the 4 of its file structure table and the message protection corpus task 10 reads",
			len(keyScheduleVectorFamilies))
	}
	manifest := readVectorManifest(t)
	for _, name := range keyScheduleVectorFamilies {
		path := filepath.Join("testdata", "vectors", name)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("vector %s: %v — the validation plan's task 6 vendors these; it has not landed", name, err)
			continue
		}
		if len(body) == 0 {
			t.Errorf("vector %s is empty", name)
			continue
		}
		want, listed := manifest[name]
		if !listed {
			t.Errorf("vector %s is not listed in VECTORS.sha256, so nothing pins the bytes this plan reads", name)
			continue
		}
		sum := sha256.Sum256(body)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("vector %s digest %s, VECTORS.sha256 says %s", name, got, want)
		}
	}
}

// readVectorManifest parses VECTORS.sha256 into name to digest. It fails rather than
// returning an empty map, because an unreadable manifest would make every coverage check
// above report "not listed" for the wrong reason.
func readVectorManifest(t *testing.T) map[string]string {
	t.Helper()
	handle, err := os.Open(filepath.Join("testdata", "vectors", "VECTORS.sha256"))
	if err != nil {
		t.Fatalf("open VECTORS.sha256: %v", err)
	}
	defer handle.Close()
	manifest := map[string]string{}
	scanner := bufio.NewScanner(handle)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 {
			manifest[fields[1]] = strings.TrimPrefix(fields[0], "*")
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read VECTORS.sha256: %v", err)
	}
	if len(manifest) == 0 {
		t.Fatal("VECTORS.sha256 parsed to no entries, so every coverage check above would fail for the wrong reason")
	}
	return manifest
}

// crossPlanSymbolsNotYetLanded is every symbol from this plan's "what this plan consumes"
// section whose producing plan is not in this package yet, mapped to the plan that owns
// it. The names are transcribed from the interface registry because that document is
// normative and there is nothing in this tree to derive them from — the code that will
// call them is written by later tasks. What IS derived is whether each has landed, which
// is read from the package's own syntax tree below rather than kept as a boolean here.
//
// GroupContext and PreSharedKeyId are this plan's own, from tasks 3 and 13, and both have
// now landed and been answered with the var _ syntax.Codec pin the plan's task 1 block
// carries plus a field by field pin above. They were in this list for the same reason
// every cross plan name is: the moment a type exists, this file owes it that pin.
//
// p8's five vector harness names left this map the same way and on the same terms: they
// landed in vectors_test.go, so they are pinned above. Landing in a TEST file is what this
// map's detector used to be blind to, and the fix is in the scan rather than here — a name
// that lands test-only is landed, and owes its pin.
//
// Framing's ContentType and its three code points left it on stricter terms still: they were
// declared BY this plan, in content_type.go, because task 23a implements the interface p6
// declares and that interface names them. "Not landed" was never a reason to leave the
// wrapper unwritten and it was never a licence to spell a private copy either — a second
// declaration of one wire enum disagrees by a NUMBER rather than by a type error, which is
// the one kind of drift nothing in this file could see. One declaration, pinned above, and
// p6 task 1 has now moved it into framing.go and deleted content_type.go, exactly as agreed.
var crossPlanSymbolsNotYetLanded = map[string]string{
	"ValSemCode":        "p8 validation and interop",
	"ValSem":            "p8 validation and interop",
	"ValSem401":         "p8 validation and interop",
	"ValSem402":         "p8 validation and interop",
	"ValSem403":         "p8 validation and interop",
	"ErrPskNonceLength": "p8 validation and interop",
	"ErrPskType":        "p8 validation and interop",
	"ErrDuplicatePsk":   "p8 validation and interop",
}

// TestEveryCrossPlanSymbolThatHasLandedIsPinnedHere fails when a producing plan merges
// and its pins were not written. That failure is the point: the pin block above is this
// plan's drift detector, and a detector that is silently incomplete detects nothing.
//
// A scan that read nothing would report every symbol as still pending and pass, so the
// positive control below is what separates "not landed" from "not looked". It names
// symbols this package certainly declares, one of each kind the collector handles, and a
// name that certainly does not exist.
func TestEveryCrossPlanSymbolThatHasLandedIsPinnedHere(t *testing.T) {
	// the count is what stops the map shrinking. an entry deleted rather than answered
	// removes the only thing that will notice its symbol landing, and deleting it is the
	// cheapest way to quieten this test. answering an entry properly is a pin written
	// above, the entry deleted, and this number decremented in the same commit.
	if len(crossPlanSymbolsNotYetLanded) != 8 {
		t.Fatalf("crossPlanSymbolsNotYetLanded holds %d symbols, this plan's consumes section names 8 that have not landed; if a producing plan landed, write the pin and decrement this number, and if one was added, increment it",
			len(crossPlanSymbolsNotYetLanded))
	}
	fileSet := token.NewFileSet()
	control, err := parser.ParseFile(fileSet, "map_literal_control.go", mapLiteralDeclarationControl,
		parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse the control: %v", err)
	}
	fromControl := map[string]string{}
	declarationsIn(control, "map_literal_control.go", fromControl)
	if got := slices.Sorted(maps.Keys(fromControl)); !slices.Equal(got, []string{"DeclaredFunc", "DeclaredType", "declaredVar"}) {
		t.Fatalf("the collector read %v out of the control; it must report the three declarations and none of the three map literal keys, or reading test files would report every entry of the map below as landed",
			got)
	}

	declared := packageLevelDeclarations(t, ".")
	t.Logf("%d package level declarations read from this package's source, test files included", len(declared))

	for _, control := range []string{
		"CryptoProvider",                         // a type
		"NewCryptoProvider",                      // a func
		"HpkePublicKey",                          // a type from another file
		"CipherSuiteX25519ChaCha20Sha256Ed25519", // a const
		"ErrSecretLength",                        // a var, from this task
		"zeroizeSecret",                          // an unexported func, from this task
		"suiteCryptoProvider.Suite",              // a method, the shape the two tree math methods have
		"RegisterVectorFamily",                   // a func declared in a TEST file, the case the scan used to be blind to
		"VectorFamily",                           // and a type declared in one
	} {
		if _, ok := declared[control]; !ok {
			t.Fatalf("the scan did not find %s, which this package certainly declares, so it is reporting every symbol below as pending having read nothing useful", control)
		}
	}
	if _, ok := declared["ThisSymbolDoesNotExistAnywhereInPackageMls"]; ok {
		t.Fatal("the scan reports a symbol that cannot exist, so it is matching text rather than declarations")
	}

	for _, name := range slices.Sorted(maps.Keys(crossPlanSymbolsNotYetLanded)) {
		if file, ok := declared[name]; ok {
			t.Errorf("%s has landed in %s (%s) but this file still lists it as not yet pinned; write its compile time pin in the block at the top of this file at the signature the interface registry gives it, then delete this entry",
				name, file, crossPlanSymbolsNotYetLanded[name])
		}
	}
	t.Logf("%d cross plan symbols still pending a pin", len(crossPlanSymbolsNotYetLanded))
}

// packageLevelDeclarations reads every go file of a directory, test files included, and
// returns the package level names it declares, mapped to the file that declares them.
// Methods appear as Receiver.Name, because that is the shape the two tree math methods the
// registry pins have.
//
// Test files were excluded here once, on the stated grounds that this file is a test file
// and names every pending symbol in a map literal, so including it would report them all as
// landed. That reason is not true -- declarationsIn reads declared NAMES and a map literal's
// keys are values, which mapLiteralDeclarationControl below is the proof of -- and the
// exclusion cost something real: p8's vector harness landed in vectors_test.go with all five
// of its names still listed as pending, and nothing could ever have reported it, because a
// test-only surface is invisible to a scan that skips test files. A symbol declared in a
// test file of package mls has landed in package mls.
func packageLevelDeclarations(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	declared := map[string]string{}
	files := 0
	fileSet := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		parsed, err := parser.ParseFile(fileSet, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files++
		declarationsIn(parsed, name, declared)
	}
	if files == 0 {
		t.Fatalf("no go file found in %s, so this scan proves nothing", dir)
	}
	return declared
}

// declarationsIn collects one parsed file's package level declarations into declared. It is
// split out from the directory walk so a synthetic control can be run through the same code
// the real scan uses rather than through a second copy of it.
func declarationsIn(parsed *ast.File, name string, declared map[string]string) {
	for _, declaration := range parsed.Decls {
		switch typed := declaration.(type) {
		case *ast.FuncDecl:
			if typed.Recv == nil || len(typed.Recv.List) == 0 {
				declared[typed.Name.Name] = name
				continue
			}
			declared[receiverTypeName(typed.Recv.List[0].Type)+"."+typed.Name.Name] = name
		case *ast.GenDecl:
			for _, spec := range typed.Specs {
				switch typedSpec := spec.(type) {
				case *ast.TypeSpec:
					declared[typedSpec.Name.Name] = name
				case *ast.ValueSpec:
					for _, ident := range typedSpec.Names {
						if ident.Name != "_" {
							declared[ident.Name] = name
						}
					}
				}
			}
		}
	}
}

// mapLiteralDeclarationControl is the control on the scan's one debatable claim: that a name
// appearing only as a map literal key, which is how every entry of
// crossPlanSymbolsNotYetLanded appears, is not a declaration. If that were false, reading
// test files would report every pending symbol as landed and this file would be unusable.
//
// It names a declared type, a declared func, a declared var, a blank pin that must not be
// collected, and three keys that are declared nowhere.
var mapLiteralDeclarationControl = strings.Join([]string{
	"package control",
	"",
	"type DeclaredType struct{}",
	"",
	"func DeclaredFunc() {}",
	"",
	"var _ int = 1",
	"",
	"var declaredVar = map[string]string{",
	"\t" + "\"NamedOnlyAsAKey\": \"p3 tree math\",",
	"\t" + "\"AlsoOnlyAKey\": \"p6 framing\",",
	"\t" + "\"AndAThird\": \"p8 validation and interop\",",
	"}",
	"",
}, "\n")

// receiverTypeName reduces a method receiver to the bare type name, so *T, T and a
// generic T[P] all report T.
func receiverTypeName(expr ast.Expr) string {
	switch typed := expr.(type) {
	case *ast.StarExpr:
		return receiverTypeName(typed.X)
	case *ast.IndexExpr:
		return receiverTypeName(typed.X)
	case *ast.IndexListExpr:
		return receiverTypeName(typed.X)
	case *ast.Ident:
		return typed.Name
	}
	return ""
}

// keyScheduleDepsFile is this file, named so the gates below can read it. Two of the ways a
// pin stops being a pin are covered from here: it can be deleted, and it can never have
// been written for a symbol this package already consumes.
const keyScheduleDepsFile = "key_schedule_deps_test.go"

// pinBlockSizes is how many package level blank identifier declarations each test file of
// this package carries. That form -- var _ T = X -- is what a compile time pin is.
//
// A pin that pins the wrong shape catches drift by failing, which is the whole point of
// this file. A pin that is DELETED catches nothing, and deleting the pin that just failed
// is the cheapest way to make a drift induced build failure green. Nothing counted them, so
// three could be removed from the block at the top of this file with the whole package
// still green -- measured, not supposed. The two lists in this file already make exactly
// this bargain (keyScheduleVectorFamilies, and in key_schedule_test.go the owned errors);
// the pin block was the one enumeration left out of it.
//
// The class is derived rather than listed: the gate reads the directory and requires every
// test file that holds pins to appear here and every entry here to be such a file. A fourth
// pin file therefore cannot land uncounted, and the cost is the intended one -- adding a
// pin means bumping a number in the same commit.
var pinBlockSizes = map[string]int{
	"crypto_test.go": 1,
	// the three framing registry widths, p6 task 1. Each is a conversion of ^T(0) to the
	// octet or the pair of octets RFC 9420 gives that registry, which is the only form that
	// can say a type is no WIDER than its wire field; a literal conversion the other way
	// says only that the value fits and compiles unchanged at every wider type.
	"framing_test.go":           3,
	"key_schedule_deps_test.go": 78,
	"pins_test.go":              8,
	// keyRecordingCryptoProvider, the wrapper the update path erasure gate reads private
	// keys through. It is written out method by method for taggingCryptoProvider's reason
	// and carries the same pin: drift between the interface and the wrapper has to fail at
	// build rather than at the gate reading it.
	"treekem_test.go": 1,
	// framingKeySource, the MessageKeySource this file drives section 6.3.1's seal and open
	// with. It carries the same pin for the same reason, one interface over: the double is
	// the instrument every nonce and generation property in that file is measured through,
	// so drift between MessageKeySource and it has to fail at BUILD rather than leave the
	// double implementing a shape the production callers no longer take.
	"framing_protect_test.go": 1,
}

// blankPinControl holds two package level pins and one blank assignment inside a function
// body. A counter that had drifted into counting statements reads three out of it and fails
// here, rather than reporting every file's pins present and intact.
var blankPinControl = strings.Join([]string{
	"package control",
	"",
	"var _ int = 1",
	"",
	"var (",
	"\t_ string = \"two\"",
	"\tnamed    = 3",
	")",
	"",
	"func control() {",
	"\t_ = named",
	"}",
	"",
}, "\n")

// packageLevelBlankPins counts the blank identifier names a parsed file declares at package
// level. Const is counted beside var because either spelling holds a pin, and a pin moved
// from one to the other should not read as a pin deleted.
func packageLevelBlankPins(file *ast.File) int {
	pins := 0
	for _, declaration := range file.Decls {
		generic, isGeneric := declaration.(*ast.GenDecl)
		if !isGeneric || (generic.Tok != token.VAR && generic.Tok != token.CONST) {
			continue
		}
		for _, spec := range generic.Specs {
			value, isValue := spec.(*ast.ValueSpec)
			if !isValue {
				continue
			}
			for _, name := range value.Names {
				if name.Name == "_" {
					pins++
				}
			}
		}
	}
	return pins
}

// TestNoPinBlockShrinksWithoutFailing is the presence guard the pin block never had.
//
// Every other assertion in this file is about a pin being WRONG. This one is about a pin
// being GONE, which is the failure mode that looks like success: the drift lands, the pin
// fails the build, and the fastest way past a failing build is to delete the line that
// failed. Nothing then reports that the detector was removed along with the thing it was
// detecting.
func TestNoPinBlockShrinksWithoutFailing(t *testing.T) {
	fileSet := token.NewFileSet()
	control, err := parser.ParseFile(fileSet, "blank_pin_control.go", blankPinControl,
		parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse the control: %v", err)
	}
	if got := packageLevelBlankPins(control); got != 2 {
		t.Fatalf("the counter read %d pins out of the control, want 2; it is not counting package level blank declarations", got)
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	counted := map[string]int{}
	read := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fileSet, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		read++
		if pins := packageLevelBlankPins(parsed); pins != 0 {
			counted[name] = pins
		}
	}
	if read == 0 {
		t.Fatalf("no test file read from the package directory, so this gate counted nothing")
	}
	for _, name := range slices.Sorted(maps.Keys(counted)) {
		want, listed := pinBlockSizes[name]
		if !listed {
			t.Errorf("%s declares %d compile time pins and pinBlockSizes does not count them, so they can be deleted one at a time with nothing failing; add the file and its count",
				name, counted[name])
			continue
		}
		if counted[name] != want {
			t.Errorf("%s declares %d compile time pins, pinBlockSizes says %d; a pin added is a number to bump here, a pin removed is a detector removed and needs saying out loud",
				name, counted[name], want)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(pinBlockSizes)) {
		if _, holds := counted[name]; !holds {
			t.Errorf("pinBlockSizes counts %d pins in %s and it declares none; either every pin in it was deleted or the file was",
				pinBlockSizes[name], name)
		}
	}
}

// firstPartyImportPrefix is the module this repository publishes. A symbol reached through
// an import under it crosses a plan boundary inside this slice, which is the class this
// file exists to pin. Anything from the standard library or a third party module is pinned
// by pins_test.go instead.
const firstPartyImportPrefix = "github.com/urnetwork/connect"

// firstPartyConsumptionControl names one first party symbol in a comment, one through a
// value and one through a call, and one standard library symbol. A collector that had
// started reading text rather than the syntax tree reports the comment; one that had lost
// its import filter reports strings.TrimSpace. Both are shapes this package actually has:
// errors_key_schedule.go's doc names syntax.Unmarshal in prose without calling it.
var firstPartyConsumptionControl = strings.Join([]string{
	"package control",
	"",
	"import (",
	"\t\"strings\"",
	"",
	"\t\"github.com/urnetwork/connect/mls/syntax\"",
	")",
	"",
	"// syntax.NamedOnlyInAComment is prose and not a consumer.",
	"func control(text string) error {",
	"\tif strings.TrimSpace(text) == \"\" {",
	"\t\treturn syntax.ErrTruncated",
	"\t}",
	"\tw := syntax.NewWriter()",
	"\t_ = w",
	"\treturn nil",
	"}",
	"",
}, "\n")

// firstPartyQualifiedNames returns every Package.Symbol a parsed file reaches through a
// first party import, sorted and deduplicated. It reads the syntax tree, so a name that
// appears only in a comment is not a consumer of it.
func firstPartyQualifiedNames(file *ast.File) []string {
	locals := map[string]bool{}
	for _, imported := range file.Imports {
		path := strings.Trim(imported.Path.Value, "\"")
		if !strings.HasPrefix(path, firstPartyImportPrefix) {
			continue
		}
		local := path[strings.LastIndex(path, "/")+1:]
		if imported.Name != nil {
			local = imported.Name.Name
		}
		locals[local] = true
	}
	if len(locals) == 0 {
		return nil
	}
	found := map[string]bool{}
	ast.Inspect(file, func(node ast.Node) bool {
		selector, isSelector := node.(*ast.SelectorExpr)
		if !isSelector {
			return true
		}
		qualifier, isIdentifier := selector.X.(*ast.Ident)
		if !isIdentifier || !locals[qualifier.Name] {
			return true
		}
		found[qualifier.Name+"."+selector.Sel.Name] = true
		return true
	})
	return slices.Sorted(maps.Keys(found))
}

// TestEveryFirstPartySymbolTheSourceConsumesIsNamedHere derives the consumed set from the
// consuming code instead of from a transcription of the registry.
//
// crossPlanSymbolsNotYetLanded above cannot be derived, and the reason is honest: the code
// that will call those symbols is written by later tasks, so there is nothing in this tree
// to read them out of. That argument holds only for what has NOT landed. Everything this
// package's production source already reaches across a plan boundary is in the tree and can
// be read, and this is that half: every syntax.X the source names must be named in the pin
// block, so a call to an unpinned entry point fails on the commit that writes the call
// rather than on the commit that changes the signature.
//
// What it does not close, said plainly rather than left to be discovered: a symbol from
// another plan landing in package mls itself is not qualified by a package name, so nothing
// here distinguishes it from this package's own declarations. That hole is what
// crossPlanSymbolsNotYetLanded covers by hand, and it stays a hand written list until a
// consumer exists to derive it from.
func TestEveryFirstPartySymbolTheSourceConsumesIsNamedHere(t *testing.T) {
	fileSet := token.NewFileSet()
	control, err := parser.ParseFile(fileSet, "first_party_control.go", firstPartyConsumptionControl,
		parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse the control: %v", err)
	}
	want := []string{"syntax.ErrTruncated", "syntax.NewWriter"}
	if got := firstPartyQualifiedNames(control); !slices.Equal(got, want) {
		t.Fatalf("the collector read %v out of the control, want %v; it is either reading comments or ignoring the import filter",
			got, want)
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	consumed := map[string]string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fileSet, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, qualified := range firstPartyQualifiedNames(parsed) {
			consumed[qualified] = name
		}
	}
	if len(consumed) == 0 {
		t.Fatalf("this package's production source reaches no first party symbol at all, which cannot be right while crypto_labels.go builds a syntax.Writer; the collector read nothing")
	}

	pins, err := parser.ParseFile(fileSet, keyScheduleDepsFile, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", keyScheduleDepsFile, err)
	}
	pinned := firstPartyQualifiedNames(pins)
	for _, qualified := range slices.Sorted(maps.Keys(consumed)) {
		if !slices.Contains(pinned, qualified) {
			t.Errorf("%s reaches %s and %s never names it, so nothing in this package fails when that signature moves; write its pin in the block at the top of this file",
				consumed[qualified], qualified, keyScheduleDepsFile)
		}
	}
	t.Logf("%d first party symbols reached by this package's production source, each held to a pin in %s",
		len(consumed), keyScheduleDepsFile)
}
