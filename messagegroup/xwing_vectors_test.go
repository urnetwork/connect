// The draft-connolly-cfrg-xwing-kem known answer vectors. MASTER section 7.2 makes passing these
// a precondition of any use of X-Wing, and the reason is that nothing else can hold the
// construction to anything. A KEM that round trips with itself proves only that it agrees with
// itself: the label moved to the front of the combiner, the two shared secrets swapped, ct_X and
// pk_X swapped, the seed expansion taken out of the wrong window -- every one of those still
// produces thirty two bytes and still round trips, because both ends are the same file.
//
// So the vectors are run in BOTH directions and the count that is asserted is the count of
// DISTINCT PUBLISHED ANSWERS the implementation is held against, not the count of calls made.
// This project has already shipped a generate direction that shared its serializer with the
// consume direction and therefore proved nothing; the three directions below share no serializer
// with each other.
//
// Which side of a comparison runs production code is DERIVED here and not typed, and so are the
// DECLARATIONS each side ran. Both were typed once. A bool on each row made the count wrong by
// three: the ct_X rows claimed to hold this package while the value on their got side came out of
// x25519PublicKeyOfScalar, a helper declared in messagegroup/xwing_test.go that reaches
// mls.X25519PrivateKey, and XwingEncapsulate was called nowhere in this file at all. Replacing the
// bool with a derivation over a HAND WRITTEN LIST OF PRODUCER NAMES left the second half of that
// defect standing: a row whose got was changed to read the corpus back out of itself, while its
// list went on naming XwingEncapsulate, still counted towards the nine, and so did a row that
// simply dropped a name from its list. Measured before this pass, all three left every test in
// this file green. The residual was written down here as unobservable from inside a go test
// without instrumenting the build, and that was overstated: this file already parses every .go file in this directory, and
// go/types resolves every call in the collector to the declaration it actually reaches. So the
// producer names are read off the collector's own source, and a gate that counts its own worth
// takes nothing at all from the hand that wrote the rows.
//
// keygen, seed -> pk. A full known answer test. It holds the SHAKE-256 expansion, the ML-KEM key
// generation and this package's own pk_M then pk_X encoder against published octets.
//
// decapsulation, (seed, ct) -> ss. A full known answer test, on a ciphertext this package did not
// produce. It transitively pins the ciphertext split, ML-KEM decapsulation, the x25519 dh, and the
// combiner including the label's position. It does NOT read this package's public key encoder,
// which is why it and the keygen direction are independent rather than two readings of one path.
//
// encapsulation, eseed -> ct_X. A full known answer test for the x25519 half, and it runs
// XwingEncapsulate. The draft fixes the ephemeral, which is what makes encapsulation drivable at
// all: eseed[32:64] is ek_X, and XwingEncapsulate draws ek_X and nothing else from the reader it
// is handed, so a reader over exactly those thirty two octets pins ct[1088:1120] to a value the
// draft published. The peer key is parsed out of the corpus rather than taken from this package's
// own keygen, so this direction shares no serializer with that one either.
//
// encapsulation, eseed -> ct_M. NOT REACHABLE. crypto/mlkem's Encapsulate takes no randomness and
// returns no error, so ML-KEM's derandomized encapsulation is not exposed by the standard library.
// It is covered by round trip in xwing_test.go and by the standard library's own FIPS 203 ACVP
// tests. Re-implementing ML-KEM to close that gap would mean shipping new crypto, which the global
// constraints forbid outright.
//
// The gap above is the one place this file cannot hold the draft, and it is stated rather than
// left for a reader to discover by counting.
package messagegroup

import (
	"bytes"
	"crypto/sha256"
	"crypto/sha3"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/urnetwork/connect/mls"
)

// One vector as the draft's reference implementation publishes it. Every field is hex.
type xwingVector struct {
	Seed  string `json:"seed"`
	Sk    string `json:"sk"`
	Pk    string `json:"pk"`
	Eseed string `json:"eseed"`
	Ct    string `json:"ct"`
	Ss    string `json:"ss"`
}

const (
	xwingVectorPath = "testdata/vectors/rfc/xwing-draft10.json"
	// the attributes file that keeps git's text conversion off the corpus. core.autocrlf is
	// true at system scope on the windows boxes that build this repository, and a smudged
	// vector file verifies against bytes the draft never published.
	xwingVectorAttributesPath = "testdata/vectors/rfc/.gitattributes"
	// the one pin file of the slice, which is mls's rather than this package's
	xwingVectorPinFilePath = "../mls/interop/PINS.md"
)

// The provenance, recorded here and in ../mls/interop/PINS.md, and held to that file field by
// field below. The corpus is vendored whole and unmodified, so the vendored digest and the
// upstream digest are the same value and are named separately anyway: they answer two different
// questions, and a re-vendoring that filtered the file would separate them.
const (
	xwingVectorUpstreamRepository = "dconnolly/draft-connolly-cfrg-xwing-kem"
	xwingVectorUpstreamCommit     = "9b6ce9e614811dba8d46841052f3883cbc4c1a65"
	xwingVectorUpstreamPath       = "spec/test-vectors.json"
	xwingVectorSha256             = "409efe197550b22985b4a0419418a0c5f2c2b193426c55bd998399ec8d3e614d"
	xwingVectorCount              = 3
)

// Where the source reading below starts and what it looks for. These are written down because a
// walk has to start somewhere; everything it FINDS is derived, and a rename of either one fails
// the reading with "declares no ..." rather than quietly reading nothing.
const (
	xwingPackageImportPath = "github.com/urnetwork/connect/messagegroup"
	xwingCollectorName     = "xwingHoldAgainstTheDraft"
	xwingAnswerTypeName    = "xwingPublishedAnswer"
)

// The number of published answers ONE vector can hold this package to, and the reason it is three
// rather than four.
//
// keygen's pk, decapsulation's ss, and encapsulation's ct_X. The fourth, encapsulation's ct_M, is
// not reachable: crypto/mlkem's Encapsulate takes no randomness and the standard library exposes
// no derandomized entry point, so ct_M is fresh on every call and there is nothing published to
// hold it to.
//
// This number is written down rather than derived, and that is deliberate: it is the tripwire, and
// a derivation of how many answers the gate makes is satisfied by whatever the gate happens to
// make. What is derived is WHICH answers count towards it, off where their producers are declared.
const xwingHeldAnswersPerVector = 3

// The number of comparisons ONE vector produces, which is the three above plus the two claims the
// corpus makes about ITSELF: that sk is the seed, and that eseed[32:64] is the scalar behind the
// published ct_X.
//
// This is a floor on ROWS, and it is not the count of calls TestXwingIsHeldAgainstNineDistinct-
// PublishedAnswers argues against asserting. A row count on its own is satisfied by five copies of
// one comparison; it is asserted beside the held count, the distinct count and the direction
// count, and any one of those three fails that copy. What it buys is that a row deleted from the
// collector fails this file -- including one of the two that hold nothing, whose loss the other
// three counts cannot see, because each of them is computed over the rows that were left.
const xwingAnswersPerVector = 5

// The raw file, its digest checked before anything parses it.
func loadXwingVectorFile(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(xwingVectorPath)
	if err != nil {
		t.Fatalf("read %s: %v", xwingVectorPath, err)
	}
	digest := sha256.Sum256(raw)
	if got := hex.EncodeToString(digest[:]); got != xwingVectorSha256 {
		t.Fatalf("%s sha256 = %s, want %s (see %s)", xwingVectorPath, got, xwingVectorSha256, xwingVectorPinFilePath)
	}
	return raw
}

func loadXwingVectors(t *testing.T) []xwingVector {
	t.Helper()
	var vectors []xwingVector
	if err := json.Unmarshal(loadXwingVectorFile(t), &vectors); err != nil {
		t.Fatalf("parse %s: %v", xwingVectorPath, err)
	}
	if len(vectors) != xwingVectorCount {
		t.Fatalf("%s has %d vectors, want %d", xwingVectorPath, len(vectors), xwingVectorCount)
	}
	return vectors
}

// The same file read as raw objects, so the FIELDS it publishes are read off the file rather
// than off the struct above. A corpus that grew a seventh field would be silently dropped by the
// struct and the coverage rule below would never notice; read this way it fails.
func loadXwingVectorObjects(t *testing.T) []map[string]string {
	t.Helper()
	var objects []map[string]string
	if err := json.Unmarshal(loadXwingVectorFile(t), &objects); err != nil {
		t.Fatalf("parse %s as objects: %v", xwingVectorPath, err)
	}
	return objects
}

// package messagegroup cannot see p8's MustHex: it is declared in mls/vectors_test.go, and a _test.go
// file's symbols are not exported across a package boundary. this is the only hex decoder in the
// slice that is not p8's, and only for that reason.
func mustHexBytes(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("hex: %v", err)
	}
	return b
}

func TestXwingVectorKeyGen(t *testing.T) {
	for i, vector := range loadXwingVectors(t) {
		seed := mustHexBytes(t, vector.Seed)
		if !bytes.Equal(seed, mustHexBytes(t, vector.Sk)) {
			t.Fatalf("vector %d: sk is not the seed, which the draft says it is", i)
		}
		priv, err := XwingKeyGenFromSeed(seed)
		if err != nil {
			t.Fatalf("vector %d keygen: %v", i, err)
		}
		if got, want := priv.Public().Bytes(), mustHexBytes(t, vector.Pk); !bytes.Equal(got, want) {
			t.Errorf("vector %d: pk = %x..., want %x...", i, got[:16], want[:16])
		}
	}
}

func TestXwingVectorDecapsulate(t *testing.T) {
	for i, vector := range loadXwingVectors(t) {
		priv, err := XwingKeyGenFromSeed(mustHexBytes(t, vector.Seed))
		if err != nil {
			t.Fatalf("vector %d keygen: %v", i, err)
		}
		ss, err := XwingDecapsulate(priv, mustHexBytes(t, vector.Ct))
		if err != nil {
			t.Fatalf("vector %d decapsulate: %v", i, err)
		}
		if got, want := ss, mustHexBytes(t, vector.Ss); !bytes.Equal(got, want) {
			t.Errorf("vector %d: ss = %x, want %x", i, got, want)
		}
	}
}

// ct_X as THIS PACKAGE's encapsulation produces it, driven from the corpus.
//
// The draft fixes the ephemeral, which is the whole reason encapsulation is drivable here: eseed
// is sixty four octets, eseed[0:32] is the ML-KEM half's randomness and eseed[32:64] is ek_X.
// XwingEncapsulate draws ek_X and only ek_X from the reader it is handed -- crypto/mlkem's
// Encapsulate takes no randomness, so the ML-KEM half cannot be steered by anything -- so a reader
// over eseed[32:64] pins ct[1088:1120] exactly, and that is a value the draft published.
//
// The reader is exactly thirty two octets long on purpose. An encapsulation that drew ek_X from
// the process source, or out of the wrong window of the eseed, produces a different ct_X and fails
// here; one that read more than thirty two octets runs the reader dry and fails here too, rather
// than succeeding on bytes nobody chose.
//
// The peer key is PARSED OUT OF THE CORPUS rather than taken from this package's own keygen, which
// is what keeps this direction and the keygen direction independent rather than two readings of
// one path.
func xwingEncapsulateFromTheVector(t *testing.T, i int, vector xwingVector) []byte {
	t.Helper()
	eseed := mustHexBytes(t, vector.Eseed)
	if len(eseed) != 2*XwingX25519KeySize {
		t.Fatalf("vector %d: eseed is %d bytes, want %d", i, len(eseed), 2*XwingX25519KeySize)
	}
	pub, err := ParseXwingPublicKey(mustHexBytes(t, vector.Pk))
	if err != nil {
		t.Fatalf("vector %d: parse the published pk: %v", i, err)
	}
	ct, shared, err := XwingEncapsulate(bytes.NewReader(eseed[XwingX25519KeySize:]), pub)
	if err != nil {
		t.Fatalf("vector %d encapsulate: %v", i, err)
	}
	if len(ct) != XwingCiphertextSize {
		t.Fatalf("vector %d: ciphertext is %d bytes, want %d", i, len(ct), XwingCiphertextSize)
	}
	// the shared secret is NOT held to vector.Ss and cannot be. ct_M came out of crypto/mlkem's
	// own randomness on this call, so the secret this encapsulation combined is not the one the
	// draft published for the ciphertext the draft published. Its length is the whole of what
	// there is to say about it here, and saying it stops a later reader assuming otherwise.
	if len(shared) != XwingSharedSize {
		t.Fatalf("vector %d: shared secret is %d bytes, want %d", i, len(shared), XwingSharedSize)
	}
	return ct[XwingMlkemCiphertextSize:]
}

// TestXwingVectorEncapsulateProducesThePublishedCtX is the encapsulation direction, and unlike the
// test that used to carry that claim it calls XwingEncapsulate.
func TestXwingVectorEncapsulateProducesThePublishedCtX(t *testing.T) {
	for i, vector := range loadXwingVectors(t) {
		got := xwingEncapsulateFromTheVector(t, i, vector)
		want := mustHexBytes(t, vector.Ct)[XwingMlkemCiphertextSize:]
		if !bytes.Equal(got, want) {
			t.Errorf("vector %d: ct_X = %x, want %x", i, got, want)
		}
	}
}

// TestXwingVectorEseedTailIsTheEphemeralScalarBehindCtX is a claim about the CORPUS, and it is now
// named for that.
//
// It was called TestXwingVectorEncapsulateX25519Half, and it never called XwingEncapsulate: the
// value on its got side comes out of x25519PublicKeyOfScalar, a helper declared in
// messagegroup/xwing_test.go which reaches mls.X25519PrivateKey, so what it observed was mls's one ECDH
// wrapper and the corpus and none of this package's X-Wing code. It is worth keeping, because it
// says eseed[32:64] really is the scalar behind the published ct_X, and that is the premise the
// encapsulation direction above is driven on -- but under a name that says what it holds.
func TestXwingVectorEseedTailIsTheEphemeralScalarBehindCtX(t *testing.T) {
	for i, vector := range loadXwingVectors(t) {
		eseed := mustHexBytes(t, vector.Eseed)
		if len(eseed) != 2*XwingX25519KeySize {
			t.Fatalf("vector %d: eseed is %d bytes, want %d", i, len(eseed), 2*XwingX25519KeySize)
		}
		got, err := x25519PublicKeyOfScalar(eseed[XwingX25519KeySize:])
		if err != nil {
			t.Fatalf("vector %d ephemeral: %v", i, err)
		}
		ct := mustHexBytes(t, vector.Ct)
		if want := ct[XwingMlkemCiphertextSize:]; !bytes.Equal(got, want) {
			t.Errorf("vector %d: ct_X = %x, want %x", i, got, want)
		}
	}
}

// ── what the vector gate is actually worth, measured rather than claimed ─────────────

// One comparison the gate makes against a value the draft published.
//
// It carries what this package PRODUCED and the corpus field the answer belongs to, and it does
// NOT carry the published bytes. That is deliberate and it is the difference between a gate and
// a gate that can be made vacuous by one line: a collector that also decided what the answer
// should be could set the two equal to each other, and every count below would still read nine.
// The published side is computed from the file by xwingPublishedValue, which has no access to
// anything this package produced, so the only way to satisfy a comparison is to match the file.
type xwingPublishedAnswer struct {
	vector int
	// the field of the vector the answer belongs to
	field string
	// the declarations of this package's NON TEST source that got came out of, sorted, in the
	// spelling entropyDeclaredName prints. NOT SET ON THE LITERALS BELOW: it is filled in from
	// xwingCollectorProducers, which reads it off the collector's own source. The harness that
	// called them -- this file's own loaders, decoders and drivers -- is not a producer, and the
	// walk goes THROUGH one into what it reaches rather than recording it, which is why
	// x25519PublicKeyOfScalar contributes nothing and xwingEncapsulateFromTheVector contributes
	// the two declarations it drives.
	//
	// Whether the answer holds THIS PACKAGE is derived from this set and is not a field: it was a
	// field once, typed row by row, and three of the nine rows it counted were wrong. An empty
	// set is the corpus's claim about itself and is a statement about no implementation at all.
	//
	// WHAT IS STILL NOT OBSERVED is the call actually being made at run time: the reading is of
	// the collector's SOURCE, so a row whose value is produced inside a branch this build never
	// takes would be read as producing it. The collector's only branches are its t.Fatalf guards,
	// which produce no value and leave nothing for such a row to be built from. The reading also
	// cannot see through an indirection -- a value fetched from a func variable or an interface
	// -- and would answer an empty set for one, which understates and cannot restore an
	// overcount. The encapsulation row is held behaviourally as well, for the same reason it
	// always was: ct_X is reproducible from the corpus only if XwingEncapsulate is what produced
	// it, which is what the standalone test above observes.
	producers []string
	got       []byte
}

// Whether one answer holds this package, DERIVED from where its producers are declared.
//
// The producers themselves came from go/types, over the collector's own source; declared came from
// go/ast, over the same directory. This method is where the two readings are reconciled, and they
// have to agree: a name one of them resolves and the other has never heard of means one of them is
// reading something else.
//
// A producer this package declares nowhere is fatal rather than false. The safe reading of an
// unresolvable name -- that it holds nothing -- is also the reading that hides it, and a row
// naming a function that does not exist is a row nobody can check.
func (self xwingPublishedAnswer) holdsThisPackage(t *testing.T, declared map[string]bool) bool {
	t.Helper()
	if len(self.producers) == 0 {
		return false
	}
	holds := true
	for _, producer := range self.producers {
		production, declaredHere := declared[producer]
		if !declaredHere {
			t.Fatalf("vector %d, %s: the gate records %s as having produced its value and this package declares no function of that name, so the classification the count rests on is over a name nobody can resolve",
				self.vector, self.field, producer)
		}
		if !production {
			holds = false
		}
	}
	return holds
}

// The direction an answer belongs to, which is its producer set and not its field.
//
// Two answers produced by the same declarations are two readings of one path however differently
// they are named, and that is the failure this file's opening paragraph is about: a generate
// direction that shared its serializer with the consume direction proved nothing and counted two.
func (self xwingPublishedAnswer) direction() string {
	return strings.Join(self.producers, " -> ")
}

// xwingCollectorRow is one row of the collector as its SOURCE describes it: the field it is about,
// and every declaration of this package's non test source that its got value comes out of.
type xwingCollectorRow struct {
	field     string
	producers []string
}

// xwingTypesDeclaredName spells one resolved function the way entropyDeclaredName spells a parsed
// one, so a name this reading produces and a name that reading produces are the same string.
func xwingTypesDeclaredName(function *types.Func) string {
	signature, isSignature := function.Type().(*types.Signature)
	if !isSignature || signature.Recv() == nil {
		return function.Name()
	}
	receiver := signature.Recv().Type()
	prefix := ""
	if pointer, isPointer := receiver.(*types.Pointer); isPointer {
		prefix, receiver = "*", pointer.Elem()
	}
	named, isNamed := receiver.(*types.Named)
	if !isNamed || named.Obj() == nil {
		return "(" + prefix + "?)." + function.Name()
	}
	return "(" + prefix + named.Obj().Name() + ")." + function.Name()
}

// xwingCalleeOf answers the function a call expression calls, as the compiler resolved it, or nil
// for a builtin, a conversion or a call through a value this reading cannot follow.
func xwingCalleeOf(info *types.Info, callee ast.Expr) *types.Func {
	switch held := callee.(type) {
	case *ast.Ident:
		function, isFunction := info.Uses[held].(*types.Func)
		if isFunction {
			return function
		}
	case *ast.SelectorExpr:
		function, isFunction := info.Uses[held.Sel].(*types.Func)
		if isFunction {
			return function
		}
	}
	return nil
}

// xwingCollectorProducers reads the collector's own source and answers, for each
// xwingPublishedAnswer literal it builds and IN SOURCE ORDER, the field that literal is about and
// the declarations of this package's non test source its got value comes out of.
//
// WHY IT EXISTS. The producers were a hand written list beside each value. The classification over
// them was derived, so a list naming something this package does not declare was fatal and a list
// naming a test helper counted as holding nothing -- but nothing checked that a row's list was
// about the value ON THE ROW. Measured before this pass, three mutations of the collector left
// every count in this file reading nine and every test in it green: the keygen row's got changed to
// read pk back out of the corpus, the encapsulation row's got changed to read ct back out of the
// corpus, and a row that dropped one name from its list. The first two are the ones that matter -- each turns a comparison
// against this package into a comparison of the corpus with itself, which is the exact failure the
// file's opening paragraph says a vector gate is worth nothing without.
//
// HOW THE REACH IS TAKEN. Start at the literal's got expression and walk it.
//
//   - a call whose callee resolves to a function of this package declared in NON TEST source IS a
//     producer, and the walk records it and does not enter it: that declaration is the thing under
//     test, and what it calls is its own business.
//   - a call that resolves to a function of this package declared in a _test.go file is HARNESS.
//     The walk goes through it into its body and keeps looking, which is how
//     xwingEncapsulateFromTheVector contributes ParseXwingPublicKey and XwingEncapsulate, and how
//     x25519PublicKeyOfScalar contributes nothing at all -- everything it reaches is mls's.
//   - a call into any other package is neither, and is skipped.
//   - an identifier naming a variable something was assigned to is followed back to THE
//     ASSIGNMENT THAT PRODUCED THIS USE -- the last right hand side written before it, and not
//     every right hand side the variable ever received -- which is how priv.Public().Bytes()
//     reaches XwingKeyGenFromSeed. See xwingAssignmentBefore for what following all of them
//     costs.
//
// go/types AND NOT NAMES. A reading that matched method calls by NAME would answer
// (*XwingPublicKey).Bytes for the .Bytes() inside x25519PublicKeyOfScalar -- that is mls's X25519
// public key and not this package's at all -- which is an overcount of exactly the kind the typed
// bools produced. Every callee is resolved to the object the compiler resolves it to, and its
// package and its declaring file decide what it is.
//
// It is cached for the test binary: it type checks this package and everything it imports from
// source, which is a couple of seconds, and three tests below want the same answer.
var xwingCollectorProducers = sync.OnceValues(func() ([]xwingCollectorRow, error) {
	entries, err := os.ReadDir(".")
	if err != nil {
		return nil, fmt.Errorf("read this package's directory, which is what the collector is declared in: %w", err)
	}
	fileSet := token.NewFileSet()
	byPackage := map[string][]*ast.File{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		path := filepath.ToSlash(filepath.Join(".", entry.Name()))
		parsed, err := parser.ParseFile(fileSet, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		byPackage[parsed.Name.Name] = append(byPackage[parsed.Name.Name], parsed)
	}
	// the package the collector is declared in, chosen by looking for it rather than by naming a
	// package clause: an external test package in this directory would be a second package here
	// and is not the one the collector runs in.
	files := []*ast.File{}
	for _, group := range byPackage {
		for _, file := range group {
			for _, declaration := range file.Decls {
				function, isFunction := declaration.(*ast.FuncDecl)
				if isFunction && function.Recv == nil && function.Name.Name == xwingCollectorName {
					files = group
				}
			}
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no package in this directory declares %s, so this reading would be over nothing",
			xwingCollectorName)
	}
	info := &types.Info{
		Defs:  map[*ast.Ident]types.Object{},
		Uses:  map[*ast.Ident]types.Object{},
		Types: map[ast.Expr]types.TypeAndValue{},
	}
	refused := []string{}
	config := types.Config{
		Importer: importer.ForCompiler(fileSet, "source", nil),
		Error:    func(err error) { refused = append(refused, err.Error()) },
	}
	checked, err := config.Check(xwingPackageImportPath, fileSet, files, info)
	if err != nil || len(refused) != 0 {
		return nil, fmt.Errorf("type check %s from source: %v %s",
			xwingPackageImportPath, err, strings.Join(refused, "; "))
	}

	// where each function of this package is declared, and its body, so a call can be classified
	// and a harness call can be walked through
	production := map[*types.Func]bool{}
	bodyOf := map[*types.Func]*ast.FuncDecl{}
	var collector *ast.FuncDecl
	for _, file := range files {
		fromTest := strings.HasSuffix(fileSet.Position(file.Pos()).Filename, "_test.go")
		for _, declaration := range file.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction {
				continue
			}
			object, isObject := info.Defs[function.Name].(*types.Func)
			if !isObject {
				continue
			}
			production[object] = !fromTest
			bodyOf[object] = function
			if function.Recv == nil && function.Name.Name == xwingCollectorName {
				collector = function
			}
		}
	}
	if collector == nil || collector.Body == nil {
		return nil, fmt.Errorf("%s has no body in the source this reading parsed", xwingCollectorName)
	}

	// what each variable of this package's source was assigned, so a value can be followed back to
	// the call it came out of. Keyed by the resolved object, so two locals with one name in two
	// functions are two entries.
	assigned := map[*types.Var][]ast.Expr{}
	record := func(into []ast.Expr, from []ast.Expr) {
		for at, one := range into {
			held, isIdent := one.(*ast.Ident)
			if !isIdent {
				continue
			}
			object, isVariable := info.Defs[held].(*types.Var)
			if !isVariable {
				object, isVariable = info.Uses[held].(*types.Var)
			}
			if !isVariable || object == nil {
				continue
			}
			if len(from) == len(into) {
				assigned[object] = append(assigned[object], from[at])
				continue
			}
			// one call answering several values: every name on the left came out of it
			assigned[object] = append(assigned[object], from...)
		}
	}
	for _, file := range files {
		ast.Inspect(file, func(node ast.Node) bool {
			switch held := node.(type) {
			case *ast.AssignStmt:
				record(held.Lhs, held.Rhs)
			case *ast.ValueSpec:
				if len(held.Values) != 0 {
					names := []ast.Expr{}
					for _, one := range held.Names {
						names = append(names, one)
					}
					record(names, held.Values)
				}
			case *ast.RangeStmt:
				// a value that came out of a call through a range still came out of it
				names := []ast.Expr{}
				if held.Key != nil {
					names = append(names, held.Key)
				}
				if held.Value != nil {
					names = append(names, held.Value)
				}
				record(names, []ast.Expr{held.X})
			}
			return true
		})
	}

	reach := func(start ast.Node) []string {
		found := map[string]bool{}
		seenFunction := map[*types.Func]bool{}
		// keyed by the ASSIGNMENT rather than by the variable, because one variable read at two
		// positions can have two producing assignments and a per-variable guard would drop the
		// second of them
		seenAssignment := map[ast.Expr]bool{}
		var walk func(node ast.Node)
		walk = func(node ast.Node) {
			ast.Inspect(node, func(inner ast.Node) bool {
				switch held := inner.(type) {
				case *ast.CallExpr:
					callee := xwingCalleeOf(info, held.Fun)
					if callee == nil || callee.Pkg() != checked {
						return true
					}
					if production[callee] {
						found[xwingTypesDeclaredName(callee)] = true
						return true
					}
					if seenFunction[callee] {
						return true
					}
					seenFunction[callee] = true
					if body := bodyOf[callee]; body != nil && body.Body != nil {
						walk(body.Body)
					}
				case *ast.Ident:
					variable, isVariable := info.Uses[held].(*types.Var)
					if !isVariable {
						return true
					}
					produced := xwingAssignmentBefore(assigned[variable], held.Pos())
					if produced == nil || seenAssignment[produced] {
						return true
					}
					seenAssignment[produced] = true
					walk(produced)
				}
				return true
			})
		}
		walk(start)
		return slices.Sorted(maps.Keys(found))
	}

	rows := []xwingCollectorRow{}
	var failure error
	ast.Inspect(collector.Body, func(node ast.Node) bool {
		literal, isLiteral := node.(*ast.CompositeLit)
		if !isLiteral {
			return true
		}
		named, isNamed := info.Types[literal].Type.(*types.Named)
		if !isNamed || named.Obj() == nil || named.Obj().Name() != xwingAnswerTypeName {
			return true
		}
		field, got := "", ast.Expr(nil)
		for _, element := range literal.Elts {
			pair, isPair := element.(*ast.KeyValueExpr)
			if !isPair {
				continue
			}
			key, isKey := pair.Key.(*ast.Ident)
			if !isKey {
				continue
			}
			switch key.Name {
			case "field":
				text, isText := pair.Value.(*ast.BasicLit)
				if !isText || text.Kind != token.STRING {
					continue
				}
				unquoted, err := strconv.Unquote(text.Value)
				if err != nil {
					failure = fmt.Errorf("%s builds a row whose field is not a string constant: %w",
						xwingCollectorName, err)
					return false
				}
				field = unquoted
			case "got":
				got = pair.Value
			}
		}
		if field == "" || got == nil {
			failure = fmt.Errorf("%s builds a %s at %s naming field %q and got %v; a row this reading cannot read is a row whose producers would come back empty and be counted as holding nothing",
				xwingCollectorName, xwingAnswerTypeName, fileSet.Position(literal.Pos()), field, got != nil)
			return false
		}
		rows = append(rows, xwingCollectorRow{field: field, producers: reach(got)})
		return true
	})
	if failure != nil {
		return nil, failure
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%s builds no %s at all", xwingCollectorName, xwingAnswerTypeName)
	}
	return rows, nil
})

// xwingAssignmentBefore answers the assignment that produced the value a use at this position
// reads: the LAST right hand side written before it, and nil when the variable received none.
//
// THE LAST ONE AND NOT ALL OF THEM, and that is the whole of this function. assigned is a multi
// map -- a variable assigned twice carries both right hand sides -- so a walk that followed every
// one of them answers what a variable EVER held rather than what it holds at the literal being
// read. Measured: inserting `shared = xwingPublishedValue(t, vector, "ss")` after the decapsulate
// check makes the ss row compare the corpus against itself, and a walk over every assignment goes
// on reporting XwingDecapsulate as that row's producer -- so the row goes on counting towards the
// nine, with the whole tree green. That is the failure this derivation replaced a hand written
// list to prevent, arriving one level further down.
//
// POSITION ORDER is the right reading for the collector, which is straight line code inside one
// loop. Where it is not -- a value assigned below its own use and carried round a loop -- this
// answers the earlier assignment or nothing at all, which shows up as a row holding none of this
// package and FAILS the count of nine. Under-reaching is the safe direction here and over-reaching
// is the defect: a row that stopped being traced is a row that stops counting, and a row that is
// traced to something it no longer came out of is a row that counts wrongly.
func xwingAssignmentBefore(sources []ast.Expr, use token.Pos) ast.Expr {
	var produced ast.Expr
	for _, one := range sources {
		if one.Pos() < use && (produced == nil || one.Pos() > produced.Pos()) {
			produced = one
		}
	}
	return produced
}

// TestTheCollectorsProducersAreReadOffItsOwnSource is the control on the reading above.
//
// TWO FAILURE DIRECTIONS AND BOTH ARE HELD. A walk that answered production for everything is the
// shape of the overcount this file has now replaced twice, and it shows up here as more rows
// holding this package than there are answers per vector; one that answered nothing is the shape
// of a reading that resolved nothing at all, and it shows up as fewer. Neither is left to the
// counts further down, because those are computed over whatever the reading returned.
//
// AND THE TWO READINGS ARE RECONCILED. The names come from go/types over the collector's source
// and xwingDeclarationsOfThisPackage reads the same directory with go/ast; a name one resolves and
// the other has never heard of, or calls a test declaration, means the walk recorded HARNESS as a
// producer instead of going through it.
func TestTheCollectorsProducersAreReadOffItsOwnSource(t *testing.T) {
	spelled, err := xwingCollectorProducers()
	if err != nil {
		t.Fatalf("read the collector's producers off its own source: %v", err)
	}
	if len(spelled) != xwingAnswersPerVector {
		t.Fatalf("the collector builds %d rows per vector and this file is written around %d; the producers below are matched to the rows by position, so a row added or removed without that constant moves every one of them",
			len(spelled), xwingAnswersPerVector)
	}
	declared := xwingDeclarationsOfThisPackage(t)
	holding := 0
	for _, row := range spelled {
		if len(row.producers) == 0 {
			continue
		}
		holding += 1
		for _, producer := range row.producers {
			isProduction, declaredHere := declared[producer]
			if !declaredHere {
				t.Errorf("the source reading answers %q for the %s row and this package's declaration reading has never heard of that name; the two are readings of one directory and a name in one and not the other means one of them is about something else",
					producer, row.field)
				continue
			}
			if !isProduction {
				t.Errorf("the source reading answers %q for the %s row and that name is declared in a _test.go file, so the walk recorded HARNESS as a producer rather than going through it into what it reaches; that is the overcount this derivation exists to make impossible",
					producer, row.field)
			}
		}
		t.Logf("%s <- %v", row.field, row.producers)
	}
	if holding != xwingHeldAnswersPerVector {
		t.Errorf("%d of the collector's %d rows come out of this package's non test source, want %d; a reading that answered production for everything makes every count below read high and one that answered nothing makes them all read zero",
			holding, len(spelled), xwingHeldAnswersPerVector)
	}
}

// Every function name this package declares, and whether the only declarations of it are in this
// directory's non test source.
//
// This is the derivation the classification above rests on. A name resolves to production only if
// a non test file of this package declares it, so a helper declared in a _test.go file -- which is
// what x25519PublicKeyOfScalar is, and what produced the overcount -- cannot be counted as this
// package's own code however a row describes it.
//
// It reuses entropy_test.go's entropyDeclaredName rather than spelling a second reading of a
// receiver. Both files are this package's own test binary, and the two have to agree: a failure
// printed over there and a producer named here must be about the same declaration.
//
// A name in both sets resolves to test, which understates. parser.ParseFile applies no build
// constraints, so two files that never build together can each declare one name, and understating
// is the answer that cannot restore an overcount.
//
// No non test file read, or a file that does not parse, is fatal: either leaves a map in which
// nothing is production, and a gate every one of whose answers holds nothing reports a clean zero.
func xwingDeclarationsOfThisPackage(t *testing.T) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read this package's directory, which is what says where each producer is declared: %v", err)
	}
	fileSet := token.NewFileSet()
	production := map[string]bool{}
	test := map[string]bool{}
	read := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		path := filepath.ToSlash(filepath.Join(".", name))
		parsed, err := parser.ParseFile(fileSet, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		into := test
		if !strings.HasSuffix(name, "_test.go") {
			into = production
			read++
		}
		for _, declaration := range parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction {
				continue
			}
			into[entropyDeclaredName(function)] = true
		}
	}
	if read == 0 {
		t.Fatal("no non test go file was read out of this package, so nothing here is production and every answer below would be counted as holding none of it")
	}
	declared := map[string]bool{}
	for name := range production {
		declared[name] = true
	}
	for name := range test {
		declared[name] = false
	}
	return declared
}

// TestTheProducerDerivationSeparatesProductionFromTestDeclarations is the control on the reader
// above, and it is not decoration.
//
// A reader that answered production for everything is the answer that makes every count read
// high, which is precisely the defect this derivation replaced; a reader that answered test for
// everything makes the gate read zero and would be noticed at once. Only the first needs a
// tripwire, and x25519PublicKeyOfScalar is the name it has to say no to, because it is the one a
// hand written bool said yes to three times.
func TestTheProducerDerivationSeparatesProductionFromTestDeclarations(t *testing.T) {
	declared := xwingDeclarationsOfThisPackage(t)
	for _, name := range []string{
		"XwingKeyGenFromSeed", "XwingEncapsulate", "XwingDecapsulate", "ParseXwingPublicKey",
		"(*XwingPrivateKey).Public", "(*XwingPublicKey).Bytes", "xwingCombine",
	} {
		production, declaredHere := declared[name]
		if !declaredHere {
			t.Errorf("%s is declared in messagegroup/xwing.go and the derivation did not see it, so an answer produced by it would be fatal rather than counted", name)
			continue
		}
		if !production {
			t.Errorf("%s is declared in this package's non test source and the derivation calls it a test declaration, so every answer it produces is counted as holding nothing", name)
		}
	}
	for _, name := range []string{
		"x25519PublicKeyOfScalar", "xwingEncapsulateFromTheVector", "mustHexBytes", "loadXwingVectors",
	} {
		production, declaredHere := declared[name]
		if !declaredHere {
			t.Errorf("%s is declared in a _test.go file of this package and the derivation did not see it", name)
			continue
		}
		if production {
			t.Errorf("%s is declared in a _test.go file of this package and the derivation calls it production, so the classification the count rests on is inverted and the overcount is back", name)
		}
	}
}

// The bytes one field of one vector publishes as an answer, read out of the corpus and nowhere
// else. ct is the one field that is an input AND an answer: the whole 1120 octets go in, and the
// last 32 of them are ct_X, which is the half of encapsulation the standard library lets this
// package be held to.
func xwingPublishedValue(t *testing.T, vector xwingVector, field string) []byte {
	t.Helper()
	switch field {
	case "sk":
		return mustHexBytes(t, vector.Sk)
	case "pk":
		return mustHexBytes(t, vector.Pk)
	case "ss":
		return mustHexBytes(t, vector.Ss)
	case "ct":
		return mustHexBytes(t, vector.Ct)[XwingMlkemCiphertextSize:]
	}
	t.Fatalf("the gate recorded an answer about the field %q and this function publishes no value for it", field)
	return nil
}

// Every published answer the gate holds this package against, together with which of the
// corpus's own fields were read as inputs.
//
// This exists so that the coverage claim in the file comment is a measurement. A vector gate is
// worth the number of DISTINCT published answers it compares against, and every failure mode
// this project has found in one -- a direction that reused the other's serializer, a loop that
// compared a value to itself, a field of the corpus nobody read -- shows up as a number here
// rather than as a green test.
func xwingHoldAgainstTheDraft(t *testing.T) ([]xwingPublishedAnswer, map[string]bool) {
	t.Helper()
	spelled, err := xwingCollectorProducers()
	if err != nil {
		t.Fatalf("read this function's own producers off its source: %v", err)
	}
	answers := []xwingPublishedAnswer{}
	consumed := map[string]bool{}
	for i, vector := range loadXwingVectors(t) {
		seed := mustHexBytes(t, vector.Seed)
		consumed["seed"] = true

		// the corpus's own claim about itself. no declaration of this package ran to produce it,
		// so the derivation counts it as holding nothing, which is what it holds
		answers = append(answers, xwingPublishedAnswer{
			vector: i, field: "sk", got: seed,
		})

		priv, err := XwingKeyGenFromSeed(seed)
		if err != nil {
			t.Fatalf("vector %d keygen: %v", i, err)
		}
		answers = append(answers, xwingPublishedAnswer{
			vector: i, field: "pk",
			got: priv.Public().Bytes(),
		})

		ct := mustHexBytes(t, vector.Ct)
		consumed["ct"] = true
		shared, err := XwingDecapsulate(priv, ct)
		if err != nil {
			t.Fatalf("vector %d decapsulate: %v", i, err)
		}
		answers = append(answers, xwingPublishedAnswer{
			vector: i, field: "ss",
			got: shared,
		})

		// the encapsulation direction, which reads the corpus's eseed and the corpus's pk and runs
		// this package's own encapsulation. that is what the ct_X row below claimed and did not do
		consumed["eseed"] = true
		consumed["pk"] = true
		answers = append(answers, xwingPublishedAnswer{
			vector: i, field: "ct",
			got: xwingEncapsulateFromTheVector(t, i, vector),
		})

		// and the corpus's second claim about itself: eseed[32:64] is the scalar behind ct_X,
		// which is the premise the row above is driven on. the walk goes THROUGH
		// x25519PublicKeyOfScalar -- it is declared in messagegroup/xwing_test.go, so it is harness --
		// and everything it reaches from there is mls's, so this row's producer set comes back
		// empty and it holds none of this package. that is the whole of the first correction,
		// because this row is the one that was counted as three of nine
		eseed := mustHexBytes(t, vector.Eseed)
		ephemeral, err := x25519PublicKeyOfScalar(eseed[XwingX25519KeySize:])
		if err != nil {
			t.Fatalf("vector %d ephemeral: %v", i, err)
		}
		answers = append(answers, xwingPublishedAnswer{
			vector: i, field: "ct",
			got: ephemeral,
		})
	}
	// the producers, attached from the reading of this function's own SOURCE rather than typed
	// beside each value. The rows repeat per vector in the order the literals are written, so the
	// reading's row at at%len(spelled) is this row's literal -- and that correspondence is not
	// assumed, it is checked against the field each of the two says it is about.
	if len(spelled) == 0 || len(answers)%len(spelled) != 0 {
		t.Fatalf("this function built %d rows over %d literals, which do not divide; the producers below are matched to the rows by position and a partial vector would attach one literal's producers to another literal's value",
			len(answers), len(spelled))
	}
	for at := range answers {
		row := spelled[at%len(spelled)]
		if row.field != answers[at].field {
			t.Fatalf("row %d is about %q and literal %d of this function's source is about %q; the two have gone out of step, so every producer set below would be attached to the wrong value",
				at, answers[at].field, at%len(spelled), row.field)
		}
		answers[at].producers = row.producers
	}
	return answers, consumed
}

// TestXwingIsHeldAgainstNineDistinctPublishedAnswers is the count that says the three directions
// above are three directions and not one written three ways.
//
// Nine is three vectors times the three answers the standard library lets this package be held
// to. Every published side is recomputed HERE, out of the corpus, rather than taken from the
// collector, so a comparison cannot be satisfied by anything this package produced. The
// assertion is then on the number of DISTINCT published byte strings: a gate that compared the
// same answer nine times reports fewer than nine and fails. The count of CALLS is not asserted
// anywhere, because a call count is satisfied by nine copies of one comparison.
//
// Which comparisons count towards the nine is DERIVED, and BOTH halves of that are derived now.
// WHICH declarations produced an answer is read off the collector's own source by
// xwingCollectorProducers, and WHERE those declarations live is read off this directory by
// xwingDeclarationsOfThisPackage. The number nine was here before and it was wrong twice. First,
// three of the answers it counted were produced by a helper of this package's test binary, holding
// mls's ECDH wrapper and the corpus rather than any X-Wing code, and the row that closed that gap
// -- an encapsulation this file actually drives -- did not exist. Then, with the classification
// derived but the producer NAMES still typed by hand, a row whose got was changed to read the
// corpus back out of itself went on counting towards the nine with the whole tree green. Nine is
// now nine pk, ss and ct_X answers, three of each, each of them traced through the collector's own
// source to a function declared in messagegroup/xwing.go.
//
// The DIRECTIONS are counted too, and separately. A direction is a producer set, so three answers
// per vector that all came out of one path collapse to one direction and fail here even while the
// count of nine is satisfied -- which is the shape of the generate-shares-the-serializer failure
// this file's opening paragraph records having shipped once.
func TestXwingIsHeldAgainstNineDistinctPublishedAnswers(t *testing.T) {
	// the name carries the number, so the number is held to the name rather than left to drift
	// out of it by an edit to either constant
	if want := xwingHeldAnswersPerVector * xwingVectorCount; want != 9 {
		t.Fatalf("this test is named for nine published answers and the constants make it %d; rename it or fix them", want)
	}
	vectors := loadXwingVectors(t)
	declared := xwingDeclarationsOfThisPackage(t)
	answers, _ := xwingHoldAgainstTheDraft(t)
	distinct := map[string]bool{}
	directions := map[string]bool{}
	held := 0
	for _, answer := range answers {
		if answer.vector < 0 || answer.vector >= len(vectors) {
			t.Fatalf("the gate recorded an answer about vector %d and the corpus has %d", answer.vector, len(vectors))
		}
		want := xwingPublishedValue(t, vectors[answer.vector], answer.field)
		if len(want) == 0 {
			t.Fatalf("vector %d publishes nothing for %s, so that comparison is against an empty string", answer.vector, answer.field)
		}
		if !bytes.Equal(answer.got, want) {
			t.Errorf("vector %d, %s: got %x, want %x", answer.vector, answer.field, answer.got, want)
		}
		if !answer.holdsThisPackage(t, declared) {
			continue
		}
		held++
		distinct[string(want)] = true
		directions[answer.direction()] = true
	}
	if len(answers) != xwingAnswersPerVector*xwingVectorCount {
		t.Errorf("the gate made %d comparisons against the corpus, want %d; a row deleted here is a published answer nothing reads, and the counts below are all computed over the rows that were left",
			len(answers), xwingAnswersPerVector*xwingVectorCount)
	}
	if held != xwingHeldAnswersPerVector*xwingVectorCount {
		t.Errorf("the gate holds this package against %d published answers, want %d; a comparison counts only when every declaration that produced it is declared in this package's non test source",
			held, xwingHeldAnswersPerVector*xwingVectorCount)
	}
	if len(distinct) != xwingHeldAnswersPerVector*xwingVectorCount {
		t.Errorf("those %d comparisons are against %d DISTINCT published values; two of them are the same answer, so one of the two proves nothing",
			held, len(distinct))
	}
	if len(directions) != xwingHeldAnswersPerVector {
		t.Errorf("those %d comparisons come out of %d distinct producer sets, want %d: %v; two directions sharing a producer set are two readings of one path",
			held, len(directions), xwingHeldAnswersPerVector, slices.Sorted(maps.Keys(directions)))
	}
	t.Logf("%d published answers compared, %d of them holding this package over %d directions, %d distinct: %v",
		len(answers), held, len(directions), len(distinct), slices.Sorted(maps.Keys(directions)))
}

// TestEveryFieldTheCorpusPublishesIsReadBySomething derives the coverage class off the file
// rather than off a list.
//
// A vendored corpus carries fields, and a gate is only as wide as the fields it touches. Naming
// them here would be an enumeration that a re-vendoring silently outgrows; reading the file's own
// keys means a seventh field arrives as a failure that says which one nobody reads.
func TestEveryFieldTheCorpusPublishesIsReadBySomething(t *testing.T) {
	answers, consumed := xwingHoldAgainstTheDraft(t)
	compared := map[string]bool{}
	for _, answer := range answers {
		compared[answer.field] = true
	}
	published := map[string]bool{}
	for i, object := range loadXwingVectorObjects(t) {
		if len(object) == 0 {
			t.Fatalf("vector %d has no fields at all, so this rule would hold over nothing", i)
		}
		for field := range object {
			published[field] = true
		}
	}
	for _, field := range slices.Sorted(maps.Keys(published)) {
		if !consumed[field] && !compared[field] {
			t.Errorf("the draft publishes %q and this package's vector gate neither consumes it as an input nor compares against it, so that field is vendored and unread", field)
		}
	}
	// and the other direction, so a field this gate believes it reads cannot outlive the corpus
	for _, field := range slices.Sorted(maps.Keys(compared)) {
		if !published[field] {
			t.Errorf("the gate compares against a field %q the corpus does not publish", field)
		}
	}
	for _, field := range slices.Sorted(maps.Keys(consumed)) {
		if !published[field] {
			t.Errorf("the gate consumes a field %q the corpus does not publish", field)
		}
	}
	t.Logf("%d published fields, %d consumed as inputs, %d compared as answers", len(published), len(consumed), len(compared))
}

// TestXwingCombinerOrderMatchesTheDraft is the specific defect this guards: spec A section 5.4's
// table puts XWingLabel FIRST and the draft puts it LAST.
//
// Both halves report with Errorf rather than Fatalf, deliberately. Written with a Fatalf on the
// first half -- which is how the plan supplied it -- the second half is unreachable in every case
// that would make it fire: an empty label makes the two forms identical, but it also makes the
// first half fail, so the assertion that was supposed to catch it never runs. Two independent
// observations are worth having; one observation and one line of dead code are not.
func TestXwingCombinerOrderMatchesTheDraft(t *testing.T) {
	vector := loadXwingVectors(t)[0]
	priv, err := XwingKeyGenFromSeed(mustHexBytes(t, vector.Seed))
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	ct := mustHexBytes(t, vector.Ct)
	published := mustHexBytes(t, vector.Ss)
	ss, err := XwingDecapsulate(priv, ct)
	if err != nil {
		t.Fatalf("decapsulate: %v", err)
	}
	if !bytes.Equal(ss, published) {
		t.Errorf("the combiner does not match the draft: ss = %x, want %x", ss, published)
	}

	// recompute with the label first, the form spec A section 5.4's table describes, and assert
	// it does NOT reproduce the vector. without this the wrong ordering could be reintroduced
	// and only an X-Wing peer would ever notice -- and we have none.
	mlkemShared, err := priv.mlkemPrivate.Decapsulate(ct[0:XwingMlkemCiphertextSize])
	if err != nil {
		t.Fatalf("mlkem decapsulate: %v", err)
	}
	ephemeralPublic, err := mls.X25519PublicKey(ct[XwingMlkemCiphertextSize:])
	if err != nil {
		t.Fatalf("parse ct_X: %v", err)
	}
	x25519Shared, err := mls.X25519DH(priv.x25519Private, ephemeralPublic)
	if err != nil {
		t.Fatalf("x25519: %v", err)
	}
	labelFirst := sha3.New256()
	labelFirst.Write(xwingLabel)
	labelFirst.Write(mlkemShared)
	labelFirst.Write(x25519Shared)
	labelFirst.Write(ct[XwingMlkemCiphertextSize:])
	labelFirst.Write(priv.x25519PublicKey)
	labelFirstDigest := labelFirst.Sum(nil)
	// the claim about the CORPUS: the ordering spec A's table describes is not the ordering
	// these vectors were produced under. It fires if a re-vendoring ever ships label first
	// vectors, which would mean the draft had moved and this file's whole argument with it.
	if bytes.Equal(published, labelFirstDigest) {
		t.Errorf("a label first combiner reproduced the draft's published answer, so either the label is empty or the corpus is no longer the one this file argues about")
	}
	// and the claim about THIS PACKAGE, which is the one an edit can break: our combiner must
	// not agree with the label first form. It fires on exactly the two mutations that matter --
	// the label moved to the front, and the label emptied so that the two orderings collapse --
	// and it fires independently of the comparison against the vector above, which is why both
	// halves report rather than the first one stopping the test.
	if bytes.Equal(ss, labelFirstDigest) {
		t.Errorf("this package's combiner agrees with the label first form, which is the ordering spec A section 5.4's table describes and the draft does not")
	}
}

// ── the corpus's provenance ──────────────────────────────────────────────────────────

// TestXwingVectorFileWasNotSmudgedOnTheWayIn. core.autocrlf is true at system scope on the
// windows boxes that build this repository, and the sixteen mlswg corpora were once vendored
// already smudged with a manifest computed over the smudged bytes, so they verified against
// bytes upstream never published. The digest check above catches that too, but this names it:
// a carriage return in a corpus fetched from a repository that publishes LF is git having
// rewritten the evidence.
// The predicate, split out so the control below runs the same code the rule does rather than a
// second copy of it.
func xwingCarriageReturnAt(raw []byte) int {
	return bytes.IndexByte(raw, '\r')
}

func TestXwingVectorFileWasNotSmudgedOnTheWayIn(t *testing.T) {
	raw := loadXwingVectorFile(t)
	if i := xwingCarriageReturnAt(raw); i >= 0 {
		t.Fatalf("%s carries a carriage return at offset %d; git rewrote the corpus on checkout and it is no longer the bytes upstream published", xwingVectorPath, i)
	}
	// The control, and it is not decoration here. This corpus is a SINGLE LINE of json with no
	// newline in it at all, so there is nothing in it for git to convert and the rule above
	// cannot fail on the file as vendored. What is load bearing against a smudge today is the
	// digest in loadXwingVectorFile and the attributes file the test below reads; this rule is
	// what would catch a re-vendoring that shipped a pretty printed corpus, and without a control
	// it would be a matcher nothing has ever seen say yes.
	if n := bytes.Count(raw, []byte{'\n'}); n != 0 {
		t.Errorf("%s holds %d newlines; it was a single line when this control was written, so re-read the paragraph above and decide which of the two checks is now the weaker one", xwingVectorPath, n)
	}
	smudged := append(append([]byte{}, raw[:16]...), append([]byte{'\r', '\n'}, raw[16:]...)...)
	if i := xwingCarriageReturnAt(smudged); i != 16 {
		t.Fatalf("the smudge predicate answered %d on a corpus carrying a carriage return at offset 16, so it would report the real file clean whatever git had done to it", i)
	}
}

// TestXwingVectorDirectoryDisablesGitsTextConversion reads the attributes file itself, so the
// rule going missing fails on the commit that removes it rather than on the next person's fresh
// clone.
func TestXwingVectorDirectoryDisablesGitsTextConversion(t *testing.T) {
	raw, err := os.ReadFile(xwingVectorAttributesPath)
	if err != nil {
		t.Fatalf("read %s, which is what keeps git from rewriting the corpus: %v", xwingVectorAttributesPath, err)
	}
	if !strings.Contains(string(raw), "* -text") {
		t.Fatalf("%s does not carry `* -text`, so the corpus is subject to core.autocrlf: %q", xwingVectorAttributesPath, string(raw))
	}
}

// The two column table under one heading of the pin file, as a field to value map.
//
// Scoped to that heading's own section rather than to the file, because the commit appears in
// the fence, in the table and in the fetch url, and a strings.Contains over the whole file is
// answered by whichever copy was not corrupted. That is measured, not supposed: mls's own
// provenance test records 33 of 35 corruptions of the sibling section leaving every test green.
//
// It is a second copy of a parser mls already has, for the same reason mustHexBytes above is a
// second copy of p8's hex decoder: the original is declared in mls/hpke_vectors_test.go, and a
// _test.go file's symbols are not visible across a package boundary. It is not a preference and
// there is no way to share it short of moving the helper into non test source.
func xwingPinFileRows(t *testing.T, text string, heading string) map[string]string {
	t.Helper()
	start := strings.Index(text, heading)
	if start < 0 {
		t.Fatalf("%s has no %q section, so its provenance table cannot be located", xwingVectorPinFilePath, heading)
	}
	section := text[start+len(heading):]
	if next := strings.Index(section, "\n## "); next >= 0 {
		section = section[:next]
	}
	rows := map[string]string{}
	for _, raw := range strings.Split(section, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) != 2 {
			continue
		}
		field := strings.TrimSpace(cells[0])
		if field == "Field" || strings.Trim(field, "-") == "" {
			continue
		}
		rows[field] = strings.Trim(strings.TrimSpace(cells[1]), "`")
	}
	if len(rows) == 0 {
		t.Fatalf("%s's %q section has no table rows, so this rule would hold over nothing", xwingVectorPinFilePath, heading)
	}
	return rows
}

// TestXwingVectorProvenanceIsRecordedInThePinFile ties this file's constants to the one pin file
// of the slice, in both directions and field by field.
//
// The provenance is the deliverable. X-Wing is an Internet-Draft with no IANA code point, so this
// corpus moves when the draft moves, and a digest with no recorded origin is a number nobody can
// re-derive. The row table is compared in both directions so that a row nobody pins fails as
// loudly as a row that disagrees.
func TestXwingVectorProvenanceIsRecordedInThePinFile(t *testing.T) {
	body, err := os.ReadFile(filepath.FromSlash(xwingVectorPinFilePath))
	if err != nil {
		t.Fatalf("read %s: %v", xwingVectorPinFilePath, err)
	}
	text := string(body)

	// the machine readable pin, in the shape the file's own prose requires of every line of its
	// fenced block
	if !strings.Contains(text, "xwing="+xwingVectorUpstreamCommit) {
		t.Errorf("%s has no machine-readable xwing=%s line, so the upstream commit is recorded only in prose",
			xwingVectorPinFilePath, xwingVectorUpstreamCommit)
	}

	rows := xwingPinFileRows(t, text, "## messagegroup/"+xwingVectorPath)
	want := map[string]string{
		"Upstream repository": xwingVectorUpstreamRepository,
		"Upstream commit":     xwingVectorUpstreamCommit,
		"Upstream path":       xwingVectorUpstreamPath,
		"Upstream sha256":     xwingVectorSha256,
		"Vendored sha256":     xwingVectorSha256,
		"Vectors":             "3",
	}
	for _, field := range slices.Sorted(maps.Keys(want)) {
		got, ok := rows[field]
		if !ok {
			t.Errorf("%s provenance table has no %q row", xwingVectorPinFilePath, field)
			continue
		}
		if got != want[field] {
			t.Errorf("%s records %s = %s, this corpus was vendored from %s", xwingVectorPinFilePath, field, got, want[field])
		}
	}
	for _, field := range slices.Sorted(maps.Keys(rows)) {
		if _, ok := want[field]; !ok {
			t.Errorf("%s provenance table carries an unpinned %q row", xwingVectorPinFilePath, field)
		}
	}

	// the claims that occur once, including the url that has to agree with the three fields it
	// is built out of
	for _, claim := range []struct {
		what string
		want string
	}{
		{
			what: "the fetch url",
			want: "https://raw.githubusercontent.com/" + xwingVectorUpstreamRepository +
				"/" + xwingVectorUpstreamCommit + "/" + xwingVectorUpstreamPath,
		},
		{what: "the attributes file", want: "messagegroup/" + xwingVectorAttributesPath},
		{what: "the file holding the second copy of the digest", want: "messagegroup/xwing_vectors_test.go"},
		{what: "the constant holding it", want: "xwingVectorSha256"},
		{what: "the smudge detector", want: "TestXwingVectorFileWasNotSmudgedOnTheWayIn"},
		{what: "the attributes check", want: "TestXwingVectorDirectoryDisablesGitsTextConversion"},
		{what: "this test", want: "TestXwingVectorProvenanceIsRecordedInThePinFile"},
	} {
		if !strings.Contains(text, claim.want) {
			t.Errorf("%s does not record %s: %q", xwingVectorPinFilePath, claim.what, claim.want)
		}
	}

	// the row in the summary table at the top, which records the vendored digest a second time
	row := ""
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "|") && strings.Contains(line, "`../../messagegroup/"+xwingVectorPath+"`") {
			row = line
			break
		}
	}
	if row == "" {
		t.Fatalf("%s summary table has no row for %s", xwingVectorPinFilePath, xwingVectorPath)
	}
	if !strings.Contains(row, xwingVectorSha256) {
		t.Errorf("%s summary row for %s does not carry sha256 %s", xwingVectorPinFilePath, xwingVectorPath, xwingVectorSha256)
	}
	if !strings.Contains(row, "`xwing=` line above") {
		t.Errorf("%s summary row for %s does not point at the machine-readable xwing= line", xwingVectorPinFilePath, xwingVectorPath)
	}
}
