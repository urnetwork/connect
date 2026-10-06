package messagegroup

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"testing"
)

// RFC 5869's two functions, written here from the RFC rather than called from a library, so
// every vector below is held against an implementation that shares no code with the one under
// test. Extract is written salt FIRST, which is the order every spec text in this project uses
// and the order the thing under test must take.
//
// HKDF-Extract(salt, IKM) = HMAC-Hash(salt, IKM)
// HKDF-Expand(PRK, info, L): T(0) = empty, T(i) = HMAC(PRK, T(i-1) | info | i), output = T(1) |
// T(2) | ... truncated to L.
func keyScheduleReferenceExtract(salt []byte, ikm []byte) []byte {
	mac := hmac.New(sha256.New, salt)
	mac.Write(ikm)
	return mac.Sum(nil)
}

func keyScheduleReferenceExpand(prk []byte, info []byte, length int) []byte {
	out := []byte{}
	block := []byte{}
	for counter := byte(1); len(out) < length; counter++ {
		mac := hmac.New(sha256.New, prk)
		mac.Write(block)
		mac.Write(info)
		mac.Write([]byte{counter})
		block = mac.Sum(nil)
		out = append(out, block...)
	}
	return out[:length]
}

// The known answer test guardrail G1 names, and the whole reason it is a KAT and not a round
// trip.
//
// THE DERIVATION, so a reader can re-derive every octet below without running this package.
// The two inputs are chosen to be the same length and DIFFERENT content, because a vector over
// two equal inputs is a vector a transposition cannot fail:
//
//	mls_secret = 00 01 02 ... 1f            (32 octets, 0x00 through 0x1f)
//	pq_secret  = 20 21 22 ... 3f            (32 octets, 0x20 through 0x3f)
//
//	storage_root = HKDF-Extract(salt = mls_secret, ikm = pq_secret)
//	             = HMAC-SHA-256(key = mls_secret, message = pq_secret)
//	             = 62215de7bddcea7e2c4047ff6bb94f8d18262fc8b3f3648134bb7d44158ff84d
//
//	and the transposition, which is what G1 exists to catch:
//	HKDF-Extract(salt = pq_secret, ikm = mls_secret)
//	             = HMAC-SHA-256(key = pq_secret, message = mls_secret)
//	             = a27b86e7a70a029cba778d6f738d952696d6d8361b95103dd84ae9df6af063af
//
//	perm/v1      = HKDF-Expand(storage_root, "perm/v1",    32)
//	             = d4d8e3b05305896810c29e64c2768eea4dcf01a80137cc0dcb211b3eb2454d06
//	durable/v1   = HKDF-Expand(storage_root, "durable/v1", 32)
//	             = bf987968f45bf2c8cb50c0639616df062dde171c9b7a2394f701a84e3ceb6838
//	media/v1     = HKDF-Expand(storage_root, "media/v1",   32)
//	             = 9a7d78f7e6e705fd87fe1e8474e8d5d9cd723f01b8625cff2fe1eb08f7b6e010
//
// The hex was computed outside this tree, with python's hmac and hashlib, and is checked here
// against the RFC 5869 reference above as well -- so the vector is wrong only if two
// implementations that share no code are wrong in the same way.
const (
	storageRootKatHex           = "62215de7bddcea7e2c4047ff6bb94f8d18262fc8b3f3648134bb7d44158ff84d"
	storageRootTransposedKatHex = "a27b86e7a70a029cba778d6f738d952696d6d8361b95103dd84ae9df6af063af"
	permClassKeyKatHex          = "d4d8e3b05305896810c29e64c2768eea4dcf01a80137cc0dcb211b3eb2454d06"
	durableClassKeyKatHex       = "bf987968f45bf2c8cb50c0639616df062dde171c9b7a2394f701a84e3ceb6838"
	mediaClassKeyKatHex         = "9a7d78f7e6e705fd87fe1e8474e8d5d9cd723f01b8625cff2fe1eb08f7b6e010"
)

// The two inputs of the vector, built the way the comment above describes them.
func keyScheduleKatInputs() (mlsSecret []byte, pqSecret []byte) {
	mlsSecret = make([]byte, 32)
	pqSecret = make([]byte, 32)
	for i := range mlsSecret {
		mlsSecret[i] = byte(0x00 + i)
		pqSecret[i] = byte(0x20 + i)
	}
	return mlsSecret, pqSecret
}

func mustKeyScheduleHex(t *testing.T, s string) []byte {
	t.Helper()
	raw, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return raw
}

// Property 1: the arguments are (salt, ikm) and the output is pinned.
func TestStorageRootKAT(t *testing.T) {
	mlsSecret, pqSecret := keyScheduleKatInputs()
	want := mustKeyScheduleHex(t, storageRootKatHex)
	transposed := mustKeyScheduleHex(t, storageRootTransposedKatHex)
	if string(want) == string(transposed) {
		t.Fatal("the vector and its transposition are the same octets, so this KAT cannot fail the defect it exists for")
	}
	// the reference implementation agrees with the pinned hex, in both orders
	if got := keyScheduleReferenceExtract(mlsSecret, pqSecret); string(got) != string(want) {
		t.Fatalf("RFC 5869 written out here gives %x for salt = mls_secret and the pinned vector is %x", got, want)
	}
	if got := keyScheduleReferenceExtract(pqSecret, mlsSecret); string(got) != string(transposed) {
		t.Fatalf("RFC 5869 written out here gives %x for salt = pq_secret and the pinned transposition is %x", got, transposed)
	}
	got := StorageRoot(mlsSecret, pqSecret)
	if string(got) != string(want) {
		t.Errorf("StorageRoot(mls_secret, pq_secret) = %x, want %x", got, want)
	}
	if string(got) == string(transposed) {
		t.Errorf("StorageRoot took its arguments as (ikm, salt): the output is HKDF-Extract(salt = pq_secret, ikm = mls_secret), which is what guardrail G1 exists to catch")
	}
	if len(got) != classKeyBytes {
		t.Errorf("the storage root is %d octets, want %d", len(got), classKeyBytes)
	}
}

// Property 2: the transposition is caught in both directions, over more than the one point the
// KAT pins.
func TestSwappingTheStorageRootArgumentsChangesTheRoot(t *testing.T) {
	pairs := [][2][]byte{}
	base, other := keyScheduleKatInputs()
	pairs = append(pairs, [2][]byte{base, other})
	pairs = append(pairs, [2][]byte{[]byte("mls secret one"), []byte("pq secret two")})
	pairs = append(pairs, [2][]byte{make([]byte, 32), append(make([]byte, 31), 1)})
	pairs = append(pairs, [2][]byte{[]byte{0x01}, []byte{0x02}})
	for i, pair := range pairs {
		forward := StorageRoot(pair[0], pair[1])
		backward := StorageRoot(pair[1], pair[0])
		if string(forward) == string(backward) {
			t.Errorf("pair %d: StorageRoot is symmetric in its two arguments, so a transposition anywhere on the key schedule's path is undetectable", i)
		}
		// and it is the SALT that is first: HMAC keyed by the first argument.
		if want := keyScheduleReferenceExtract(pair[0], pair[1]); string(forward) != string(want) {
			t.Errorf("pair %d: StorageRoot(a, b) = %x and HMAC-SHA-256(key = a, message = b) = %x", i, forward, want)
		}
	}
	// the class keys move with the root, so a transposed root is not merely a different root but
	// a different whole schedule
	forward := DeriveClassKeys(StorageRoot(base, other))
	backward := DeriveClassKeys(StorageRoot(other, base))
	if string(forward.Durable) == string(backward.Durable) {
		t.Error("the durable class key does not depend on the storage root's argument order")
	}
}

// Property 5: every class key is the pinned value, is thirty two octets, and differs from the
// other two and from the root.
func TestTheThreeClassKeysAreDistinctAndPinned(t *testing.T) {
	mlsSecret, pqSecret := keyScheduleKatInputs()
	root := StorageRoot(mlsSecret, pqSecret)
	keys := DeriveClassKeys(root)
	for _, pinned := range []struct {
		name string
		got  []byte
		hex  string
		info string
	}{
		{name: "Perm", got: keys.Perm, hex: permClassKeyKatHex, info: permClassInfo},
		{name: "Durable", got: keys.Durable, hex: durableClassKeyKatHex, info: durableClassInfo},
		{name: "Media", got: keys.Media, hex: mediaClassKeyKatHex, info: mediaClassInfo},
	} {
		want := mustKeyScheduleHex(t, pinned.hex)
		if reference := keyScheduleReferenceExpand(root, []byte(pinned.info), classKeyBytes); string(reference) != string(want) {
			t.Fatalf("%s: RFC 5869 written out here gives %x for label %q and the pinned vector is %x", pinned.name, reference, pinned.info, want)
		}
		if string(pinned.got) != string(want) {
			t.Errorf("%s = %x, want %x (HKDF-Expand(storage_root, %q, 32))", pinned.name, pinned.got, want, pinned.info)
		}
		if len(pinned.got) != classKeyBytes {
			t.Errorf("%s is %d octets, want %d", pinned.name, len(pinned.got), classKeyBytes)
		}
		if string(pinned.got) == string(root) {
			t.Errorf("%s is the storage root itself, so the class it names is not separated from anything", pinned.name)
		}
	}
	for _, pair := range [][2]string{
		{"Perm", "Durable"},
		{"Perm", "Media"},
		{"Durable", "Media"},
	} {
		left := map[string][]byte{"Perm": keys.Perm, "Durable": keys.Durable, "Media": keys.Media}[pair[0]]
		right := map[string][]byte{"Perm": keys.Perm, "Durable": keys.Durable, "Media": keys.Media}[pair[1]]
		if string(left) == string(right) {
			t.Errorf("%s and %s are the same key: two retention classes sharing a key means a record of one class opens under the other's key",
				pair[0], pair[1])
		}
	}
}

// Property 4: three distinct labels, and none of them built from a shared stem.
//
// The value half is arithmetic over the constants. The construction half is read off the
// syntax tree, and its CLASS is derived: it is the info argument of every field DeriveClassKeys
// fills in -- the same class property 3 reads the struct for -- rather than a list of three
// constant names, so a fourth class added to that literal is judged the moment it exists.
func TestTheThreeClassLabelsAreThreeSeparateConstants(t *testing.T) {
	labels := []string{permClassInfo, durableClassInfo, mediaClassInfo}
	for i := range labels {
		for j := range labels {
			if i == j {
				continue
			}
			if labels[i] == labels[j] {
				t.Errorf("%q and %q are the same label", labels[i], labels[j])
			}
			shorter := min(len(labels[i]), len(labels[j]))
			if labels[i][:shorter] == labels[j][:shorter] {
				t.Errorf("%q and %q agree over the whole of the shorter one, so what separates them is only what follows: a truncation makes two class keys equal",
					labels[i], labels[j])
			}
		}
	}
	_, sources := messagegroupProductionSources(t)
	deriving := keyScheduleFunctionNamed(t, sources, "DeriveClassKeys")
	literal := keyScheduleReturnedCompositeLiteral(t, deriving)
	constants := keyScheduleStringConstantsOf(sources)
	seen := map[string]string{}
	for _, element := range literal.Elts {
		field, isField := element.(*ast.KeyValueExpr)
		if !isField {
			t.Fatalf("DeriveClassKeys returns a composite literal with an unkeyed element; the house style names every field and this reading is over named fields")
		}
		name := field.Key.(*ast.Ident).Name
		named := keyScheduleIdentifiersIn(field.Value)
		labelled := []string{}
		for _, identifier := range named {
			if _, isConstant := constants[identifier]; isConstant {
				labelled = append(labelled, identifier)
			}
		}
		if len(labelled) != 1 {
			t.Errorf("%s is derived under %v package level string constants; each class key is expanded under exactly one label of its own", name, labelled)
			continue
		}
		if keyScheduleHasConcatenation(field.Value) {
			t.Errorf("%s's label is built by concatenation; a stem shared between two classes is one edit away from making two class keys equal, which writeauth.go's own two labels refuse for the same reason",
				name)
		}
		value := constants[labelled[0]]
		if previous, repeated := seen[value]; repeated {
			t.Errorf("%s and %s are expanded under the same label %q", previous, name, value)
		}
		seen[value] = name
		if !slices.Contains(labels, value) {
			t.Errorf("%s is expanded under %q, which this test does not hold to a value; every label of this struct is pinned above", name, value)
		}
	}
	if len(seen) != len(labels) {
		t.Errorf("DeriveClassKeys fills %d fields and %d labels are pinned above", len(seen), len(labels))
	}
}

// Property 3: ClassKeys has exactly three fields and none of them is eph.
//
// The CLASS is the struct's field set read off the syntax tree, not a list of three names, so a
// fourth field of any name fails here on the commit that adds it. The eph refusal is over the
// field NAMES and over the DERIVATION LABELS both, because a field called Transient expanded
// under "eph/v1" is the same defect wearing a different name.
func TestClassKeysHoldsThreeFieldsAndNoEphKey(t *testing.T) {
	_, sources := messagegroupProductionSources(t)
	structure := keyScheduleStructNamed(t, sources, "ClassKeys")
	fields := []string{}
	for _, field := range structure.Fields.List {
		for _, name := range field.Names {
			fields = append(fields, name.Name)
		}
	}
	slices.Sort(fields)
	if !slices.Equal(fields, []string{"Durable", "Media", "Perm"}) {
		t.Errorf("ClassKeys holds %v; spec A section 5.3 gives it three class keys and no others, and a fourth field is a class this schedule was never meant to derive",
			fields)
	}
	for _, name := range fields {
		if strings.Contains(strings.ToLower(name), "eph") {
			t.Errorf("ClassKeys holds a field named %s. MASTER I4: eph_root is 32 octets of fresh CSPRNG at each commit and is NEVER derived from storage_root; a field for it here would make the wrong thing the easy thing",
				name)
		}
	}
	for name, value := range keyScheduleStringConstantsOf(sources) {
		if !strings.Contains(strings.ToLower(name), "classinfo") {
			continue
		}
		if strings.Contains(strings.ToLower(value), "eph") {
			t.Errorf("%s expands a class key under the label %q. MASTER I4: nothing eph is derived from storage_root", name, value)
		}
	}
}

// Property 6: StorageRoot is the only extraction on the key schedule's path.
//
// The CLASS is derived: every production declaration of this package whose body calls something
// named Extract or Key -- the two crypto/hkdf entry points that take a salt and can therefore be
// transposed -- on any receiver at all. The SCOPE, answered separately per R3a, is this
// package's directory alone, and the reason is not symmetry: connect/message reaches this family
// only through Expand, which has no salt argument and so carries no transposition, so widening
// the scope would add a directory the EXTRACTION class never draws from. connect/mls is out of
// scope for the opposite reason -- RFC 9420 and RFC 9180 each require an extraction there, and
// mls's own crypto_forbidden_test.go confines both by path.
//
// It is the client side half of Gate A, held where Gate A cannot express it: Gate A scans the
// text for the crypto/hkdf entry points and cannot see a call that reaches the same primitive
// through mls.CryptoProvider, which is exactly the call this package makes.
//
// The exceptions are a TABLE held in both directions, in the shape mls's
// entropyRefusalsHeldOutsideThisPackage uses: a member with no row fails, and a row naming a
// declaration that no longer extracts fails too. Tasks 22 and 23 add theirs in the commit that
// adds the call, with section 5.14's derivation quoted as the reason.
var keyScheduleExtractionSites = map[string]string{
	"keyScheduleExtract": "spec A section 5.3 and guardrail G1's single reviewed call site: the one extraction " +
		"of this package, delegating to mls.CryptoProvider.Extract, which takes the salt first as every " +
		"spec text in this project writes it. Its CALLERS are held by keyScheduleExtractionCallers " +
		"below, in both directions",
}

// Every declaration of this package that CALLS the one extraction, with the derivation it is
// making and the document that fixes it.
//
// THIS TABLE REPLACED A SLICE EQUALITY AND IS STRICTLY STRONGER THAN IT WAS. The second half of
// this gate read `slices.Equal(callers, []string{"StorageRoot"})` until 2026-09-23, which is the
// right shape while there is exactly one derivation in the corpus that extracts and the wrong one
// the moment there are two: a builder adding the second has to edit the assertion either way, and
// the slice form lets that edit be made by appending a NAME, with nothing written down about what
// the new extraction is or which document fixes it. A map held in both directions cannot be
// widened without a sentence -- the same shape keyScheduleExtractionSites above already uses, and
// the same one mls's entropyRefusalsHeldOutsideThisPackage uses -- and it still fails BOTH ways: a
// caller with no row fails, and a row naming a declaration that no longer calls the extraction
// fails too, so a row cannot outlive its call site and read as coverage.
//
// WHAT THE OLD ASSERTION WAS DEFENDING, kept because the new one has to defend the same thing: a
// second, unaccounted-for extraction is a second STORAGE ROOT -- thirty two octets, well formed,
// agreed on by both ends of this implementation and by no second one. That is why the reason
// column has to name the block it transcribes and not merely the feature it serves.
var keyScheduleExtractionCallers = map[string]string{
	"StorageRoot": "MASTER section 7 and spec A section 5.3: storage_root[n] = HKDF-Extract(salt = " +
		"mls_secret[n], ikm = pq_secret[n]). The root every class key, every ladder and both record " +
		"aeads of an epoch hang off, and the one whose transposed arguments guardrail G1 exists for",
	"wrapKeyMaterial": "MASTER section 7's wrap KDF, adopted 2026-09-18 from red-team finding M-15: " +
		"prk = HKDF-Extract(salt = \"URmessage/v1/wrap-salt\", ikm = ss), then " +
		"wrap_key | wrap_nonce = HKDF-Expand(prk, info, 56). It is Extract-then-Expand in place of a " +
		"bare Expand off a raw shared secret, under a salt no other construction here uses -- and " +
		"MASTER states what that buys rather than claiming more: X-Wing's ss is already a uniform " +
		"thirty two octet KDF output, so the named salt is DOMAIN SEPARATION and not entropy " +
		"extraction. It is not a second storage root: nothing descends from this prk but one wrap's " +
		"own key and nonce, and wrap_test.go holds that wrap.go reaches neither StorageRoot nor " +
		"DeriveClassKeys",
}

func TestTheKeySchedulesOnlyExtractionIsStorageRoots(t *testing.T) {
	_, sources := messagegroupProductionSources(t)
	extracting := []string{}
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil {
				continue
			}
			if keyScheduleCallsAnExtractionEntryPoint(source.parsed, function.Body) {
				extracting = append(extracting, function.Name.Name)
			}
		}
	}
	slices.Sort(extracting)
	if len(extracting) == 0 {
		t.Fatal("no production declaration of this package extracts, so this gate cleared the whole key schedule having found nothing to judge")
	}
	for _, name := range extracting {
		if _, hasRow := keyScheduleExtractionSites[name]; !hasRow {
			t.Errorf("%s extracts and keyScheduleExtractionSites has no row for it; every extraction on this package's path is one guardrail G1 wants named with its reason",
				name)
		}
	}
	for name := range keyScheduleExtractionSites {
		if !slices.Contains(extracting, name) {
			t.Errorf("keyScheduleExtractionSites has a row for %s, which no longer extracts; a row that outlived its call site excuses nothing and reads as coverage",
				name)
		}
	}
	// and the other half of the word "only": every caller of the one extraction carries a written
	// row naming the block it transcribes, so no second derivation can quietly acquire a root of
	// its own. Held in BOTH directions, which is what the slice equality this replaced could not
	// do -- see keyScheduleExtractionCallers.
	callers := []string{}
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil || function.Name.Name == "keyScheduleExtract" {
				continue
			}
			if slices.Contains(keyScheduleCalleeNames(function.Body), "keyScheduleExtract") {
				callers = append(callers, function.Name.Name)
			}
		}
	}
	slices.Sort(callers)
	if len(callers) == 0 {
		t.Fatal("nothing in this package calls keyScheduleExtract, so the second half of this gate judged an empty class; StorageRoot is the extraction the whole key schedule hangs off and it is not optional")
	}
	for _, name := range callers {
		reason, hasRow := keyScheduleExtractionCallers[name]
		if !hasRow {
			t.Errorf("%s calls keyScheduleExtract and keyScheduleExtractionCallers has no row for it; a second extraction is a second storage root, and one that arrives with no sentence saying which block it transcribes is one nothing in this tree accounts for",
				name)
			continue
		}
		if len(reason) == 0 {
			t.Errorf("%s has an empty row in keyScheduleExtractionCallers, which is a name and not a reason", name)
		}
	}
	for name := range keyScheduleExtractionCallers {
		if !slices.Contains(callers, name) {
			t.Errorf("keyScheduleExtractionCallers has a row for %s, which no longer calls the extraction; a row that outlived its call site excuses nothing and reads as coverage",
				name)
		}
	}
	// and StorageRoot is still one of them, named rather than derived, because a table held in
	// both directions is satisfied by a package in which the key schedule's own extraction has
	// been deleted and replaced by somebody else's.
	if !slices.Contains(callers, "StorageRoot") {
		t.Errorf("keyScheduleExtract is called from %v and StorageRoot is not among them; spec A section 5.3's extraction is the one this package is built on",
			callers)
	}
	t.Logf("%d caller(s) of the one extraction, each with a written row: %v", len(callers), callers)
}

// Property 3's behavioural half, and the obligation the erase class in connect/mls reads off
// this type's fields: every key a ClassKeys holds is erased by its own Zeroize.
func TestClassKeysZeroizeErasesEveryKey(t *testing.T) {
	mlsSecret, pqSecret := keyScheduleKatInputs()
	keys := DeriveClassKeys(StorageRoot(mlsSecret, pqSecret))
	// witnesses over the same backing arrays, taken before the erase and never handed to it
	witnesses := [][]byte{keys.Perm[:len(keys.Perm):len(keys.Perm)], keys.Durable[:len(keys.Durable):len(keys.Durable)], keys.Media[:len(keys.Media):len(keys.Media)]}
	nonZero := 0
	for _, witness := range witnesses {
		for _, octet := range witness {
			if octet != 0 {
				nonZero++
			}
		}
	}
	if nonZero < 3*classKeyBytes/2 {
		t.Fatalf("the three class keys hold only %d non zero octets before the erase, so this reading would clear a Zeroize that did nothing", nonZero)
	}
	keys.Zeroize()
	for i, witness := range witnesses {
		for j, octet := range witness {
			if octet != 0 {
				t.Errorf("class key %d holds %#02x at index %d after Zeroize", i, octet, j)
			}
		}
	}
	// a nil receiver and a second erase are both quiet, because a session drops an epoch on
	// paths that cannot know whether it was already dropped
	keys.Zeroize()
	var absent *ClassKeys
	absent.Zeroize()
}

// The declaration of one function, by name, out of this package's production source.
func keyScheduleFunctionNamed(t *testing.T, sources []messagegroupSource, name string) *ast.FuncDecl {
	t.Helper()
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if isFunction && function.Name.Name == name && function.Recv == nil {
				return function
			}
		}
	}
	t.Fatalf("this package declares no function %s, so the reading below has nothing to judge", name)
	return nil
}

// The struct type one name is declared as.
func keyScheduleStructNamed(t *testing.T, sources []messagegroupSource, name string) *ast.StructType {
	t.Helper()
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			general, isGeneral := declaration.(*ast.GenDecl)
			if !isGeneral {
				continue
			}
			for _, spec := range general.Specs {
				typed, isTyped := spec.(*ast.TypeSpec)
				if !isTyped || typed.Name.Name != name {
					continue
				}
				structure, isStruct := typed.Type.(*ast.StructType)
				if isStruct {
					return structure
				}
			}
		}
	}
	t.Fatalf("this package declares no struct type %s", name)
	return nil
}

// The composite literal a function returns, wherever it is wrapped in a unary address-of.
func keyScheduleReturnedCompositeLiteral(t *testing.T, function *ast.FuncDecl) *ast.CompositeLit {
	t.Helper()
	found := (*ast.CompositeLit)(nil)
	ast.Inspect(function.Body, func(node ast.Node) bool {
		if literal, isLiteral := node.(*ast.CompositeLit); isLiteral && found == nil {
			found = literal
		}
		return true
	})
	if found == nil {
		t.Fatalf("%s returns no composite literal, so the per field reading below has nothing to read", function.Name.Name)
	}
	return found
}

// Every package level constant of this package's production source whose value is a plain string
// literal, by name.
func keyScheduleStringConstantsOf(sources []messagegroupSource) map[string]string {
	constants := map[string]string{}
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			general, isGeneral := declaration.(*ast.GenDecl)
			if !isGeneral || general.Tok != token.CONST {
				continue
			}
			for _, spec := range general.Specs {
				value, isValue := spec.(*ast.ValueSpec)
				if !isValue {
					continue
				}
				for i, name := range value.Names {
					if len(value.Values) <= i {
						continue
					}
					literal, isLiteral := value.Values[i].(*ast.BasicLit)
					if !isLiteral || literal.Kind != token.STRING {
						continue
					}
					constants[name.Name] = strings.Trim(literal.Value, "\"`")
				}
			}
		}
	}
	return constants
}

// Every identifier named anywhere inside one expression.
func keyScheduleIdentifiersIn(expr ast.Expr) []string {
	named := []string{}
	ast.Inspect(expr, func(node ast.Node) bool {
		if identifier, isIdentifier := node.(*ast.Ident); isIdentifier {
			named = append(named, identifier.Name)
		}
		return true
	})
	return named
}

// Whether an expression concatenates anything, at any depth.
func keyScheduleHasConcatenation(expr ast.Expr) bool {
	found := false
	ast.Inspect(expr, func(node ast.Node) bool {
		if _, isBinary := node.(*ast.BinaryExpr); isBinary {
			found = true
		}
		return true
	})
	return found
}

// Whether a body computes A KEYED HASH OVER A SALT AND AN IKM, which is what an extraction is
// whatever it is spelled as.
//
// THE CLASS IS DERIVED FROM THE PROPERTY AND NOT FROM TWO CALLEE NAMES, and that is this gate's
// history rather than a preference. The version this replaces returned true only for a callee
// literally named Extract or Key. Measured on this package's own source: a second storage root
// spelled as the HMAC it is --
//
//	mac := hmac.New(sha256.New, pqSecret)   // salt and ikm TRANSPOSED
//	mac.Write(mlsSecret)
//	return mac.Sum(nil)
//
// -- landed in production with the whole three tree suite green. It is guardrail G1's exact
// defect, both of its imports were already on connect/mls's pinned crypto list so the import gate
// did not fire, and the escaping spelling is one a reader of this very file has in front of them:
// keyScheduleReferenceExtract at the top is that same three line body, because RFC 5869 section
// 2.2 DEFINES HKDF-Extract(salt, IKM) as HMAC-Hash(salt, IKM).
//
// So two shapes are read, and both are decided from the property:
//
//   - a KDF entry point that takes a salt: a callee named Extract or Key, through any receiver.
//     Key is the worse of the two because it is Extract and Expand in one call, so a
//     transposition there produces a whole key schedule that is internally consistent and wrong.
//     Expand is deliberately absent: it has no salt argument and so carries no transposition,
//     which is what keeps this the EXTRACTION class rather than the kdf class.
//   - any call at all on a package this FILE imports whose job is a keyed hash or a salted kdf,
//     resolved through the file's own import spec so an alias is followed. hmac.New IS an
//     extraction under another name and so is every entry point of crypto/hkdf.
//
// The residual, stated rather than hidden: a keyed hash built by hand out of a plain hash --
// sha3 with a key written into the message, say -- is outside both shapes. What closes that is
// imports_test.go, which pins this package's production import set AS A WHOLE, so a second hash
// package cannot arrive without a row; the two gates are the pair, and neither is complete alone.
func keyScheduleCallsAnExtractionEntryPoint(parsed *ast.File, body ast.Node) bool {
	keyed := keyScheduleKeyedHashImportsOf(parsed)
	found := false
	ast.Inspect(body, func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if !isCall {
			return true
		}
		switch callee := call.Fun.(type) {
		case *ast.Ident:
			if callee.Name == "Extract" || callee.Name == "Key" {
				found = true
			}
		case *ast.SelectorExpr:
			if callee.Sel.Name == "Extract" || callee.Sel.Name == "Key" {
				found = true
			}
			if qualifier, isName := callee.X.(*ast.Ident); isName && keyed[qualifier.Name] {
				found = true
			}
		}
		return true
	})
	return found
}

// The local names one file binds to a package whose job is a keyed hash or a salted kdf.
//
// Resolved through the import spec, so an alias -- import mac "crypto/hmac" -- is followed, and
// the local name is the spec's own when it names one and the path's last segment otherwise. The
// paths are recognised by that last segment, which is the property "this package computes a
// keyed hash" as the standard library spells it.
func keyScheduleKeyedHashImportsOf(parsed *ast.File) map[string]bool {
	keyed := map[string]bool{}
	for _, imported := range parsed.Imports {
		path := strings.Trim(imported.Path.Value, "\"`")
		segments := strings.Split(path, "/")
		last := segments[len(segments)-1]
		if last != "hmac" && last != "hkdf" {
			continue
		}
		local := last
		if imported.Name != nil {
			local = imported.Name.Name
		}
		keyed[local] = true
	}
	return keyed
}

// One file holding one of each shape, so a matcher that stopped matching fails HERE rather than
// reporting the package clean.
//
// The last three are the negative half: an expansion carries no salt and so no transposition, a
// hash with no key is not an extraction, and a LOCAL named extract is not a call to one.
const keyScheduleExtractionControl = "package control\n" +
	"\n" +
	"import (\n" +
	"\tmac \"crypto/hmac\"\n" +
	"\t\"crypto/hkdf\"\n" +
	"\t\"crypto/sha256\"\n" +
	")\n" +
	"\n" +
	"func extractsThroughTheProvider(crypto Provider, salt []byte, ikm []byte) []byte {\n" +
	"\treturn crypto.Extract(salt, ikm)\n" +
	"}\n" +
	"\n" +
	"func extractsThroughTheOneCallKdf(salt []byte, ikm []byte) []byte {\n" +
	"\tout, _ := hkdf.Key(sha256.New, ikm, salt, \"\", 32)\n" +
	"\treturn out\n" +
	"}\n" +
	"\n" +
	"func extractsThroughHkdfDirectly(salt []byte, ikm []byte) []byte {\n" +
	"\tout, _ := hkdf.Extract(sha256.New, ikm, salt)\n" +
	"\treturn out\n" +
	"}\n" +
	"\n" +
	"func extractsWithARawHmacUnderAnAlias(salt []byte, ikm []byte) []byte {\n" +
	"\tkeyed := mac.New(sha256.New, salt)\n" +
	"\tkeyed.Write(ikm)\n" +
	"\treturn keyed.Sum(nil)\n" +
	"}\n" +
	"\n" +
	"func expandsOnly(crypto Provider, prk []byte) []byte {\n" +
	"\treturn crypto.Expand(prk, []byte(\"label\"), 32)\n" +
	"}\n" +
	"\n" +
	"func hashesWithoutAKey(message []byte) []byte {\n" +
	"\tsum := sha256.Sum256(message)\n" +
	"\treturn sum[:]\n" +
	"}\n" +
	"\n" +
	"func namesALocalExtract(secret []byte) int {\n" +
	"\textract := len(secret)\n" +
	"\treturn extract\n" +
	"}\n"

// The control, held in both directions: the matcher must read the four extractions and must read
// none of the three shapes that are not one.
func TestTheExtractionMatcherSeparatesTheControlShapes(t *testing.T) {
	fileSet := token.NewFileSet()
	control, err := parser.ParseFile(fileSet, "the extraction control", keyScheduleExtractionControl, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse the control: %v", err)
	}
	extracting := []string{}
	for _, declaration := range control.Decls {
		function, isFunction := declaration.(*ast.FuncDecl)
		if !isFunction || function.Body == nil {
			continue
		}
		if keyScheduleCallsAnExtractionEntryPoint(control, function.Body) {
			extracting = append(extracting, function.Name.Name)
		}
	}
	want := []string{
		"extractsThroughTheProvider",
		"extractsThroughTheOneCallKdf",
		"extractsThroughHkdfDirectly",
		"extractsWithARawHmacUnderAnAlias",
	}
	if !slices.Equal(extracting, want) {
		t.Fatalf("the matcher read %v out of the control as extractions, want %v; it is not telling a keyed hash over a salt and an ikm from an expansion, from an unkeyed hash, or from a local that merely shares a name with one",
			extracting, want)
	}
}

// The name of every function called in a body, whether called bare or through a selector.
func keyScheduleCalleeNames(body ast.Node) []string {
	names := []string{}
	ast.Inspect(body, func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if !isCall {
			return true
		}
		switch callee := call.Fun.(type) {
		case *ast.Ident:
			names = append(names, callee.Name)
		case *ast.SelectorExpr:
			names = append(names, callee.Sel.Name)
		}
		return true
	})
	return names
}
