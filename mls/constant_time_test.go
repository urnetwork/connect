// Guardrail 8 at the widest scope it has: nothing this package SHIPS compares data with a
// variable time call, in any file, in any function, and in a package level initializer too.
//
// This file exists because a narrower gate reported clean over a comparison it was not reading.
// tree_test.go's key comparison gate judges the declarations that hold a key and answer over a
// ratchet tree, which is a class with a shape and therefore a class with an outside; treekem.go's
// TreeKEMPrivate.Consistent was outside it, and its own comment said the opposite -- "this
// package's derived comparator gates read every comparison in the source". Measured:
// subtle.ConstantTimeCompare there rewritten as bytes.Equal, with the import swapped so it
// compiles, left every test of this package and of message passing. A gate with a class is worth
// having; a gate with a class is not a gate over the source. This is the one over the source.
//
// Both halves are derived and neither is written down.
//
// The COMPARATORS come from the type checker's reading of the packages this package's production
// source imports, and the shape is the whole of the rule: exported, not a method, answering a
// bool or an int, over parameters that are all byte strings, elements of one, or a callback --
// with two of them either the same type or a string and one of its elements. That is
// bytes.Equal, bytes.Compare, bytes.HasPrefix, bytes.HasSuffix, bytes.Contains, bytes.Index,
// bytes.Cut, bytes.EqualFold, slices.Equal, slices.EqualFunc, slices.Compare, slices.Contains,
// slices.Index, slices.BinarySearch and hmac.Equal, and it is none of sha256.Sum256,
// hkdf.Expand, io.ReadFull, errors.Is, fmt.Errorf or ed25519.Verify. Every list anyone writes
// understates this: the enumeration this project shipped once held six names and did not hold
// bytes.HasPrefix, which leaks strictly more than bytes.Equal does.
//
// The FILES are every non test file of this package's directory, read as whole files rather than
// as a list of declarations, so a comparison in a var initializer is inside the gate rather than
// in the gap between two classes of function.
//
// Where the boundary is, and why it is not a hole. ed25519.Verify answers a bool over two byte
// strings and is deliberately outside the class: its first parameter is a KEY, so what it
// answers is not "are these two strings equal" but "is this signature valid under this key", and
// a rule that swept it in would have to be given an exemption -- which is the shape of gate that
// gets an exemption added rather than a bug fixed. The callers of the primitives are held next
// door and by name: crypto_labels_test.go pins SignatureVerify's body statement for statement,
// TestMacVerifyComparesInConstantTime holds the MAC, and
// TestEveryTagVerifierComparesThroughMacVerifyAndNothingElse holds everything that verifies a tag.
//
// What this cannot see, said out loud: a comparison written as a byte loop in this package's own
// source names no comparator and is in no class derived from imports. The equality rule in
// tree_test.go catches that shape for the declarations that hold a key, and nothing catches it
// anywhere else. A gate that reads every call is not a gate that reads every comparison, and the
// difference is one hand written loop.
package mls

import (
	"go/ast"
	"go/types"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// crypto/subtle is exempt as a whole PACKAGE rather than by the one name the guardrail spells.
// Everything in it answers in a time that depends on the lengths and on nothing else, which is
// the property this is about, so a comparison that reached for ConstantTimeByteEq is reaching for
// the sanctioned tool. Nothing else is exempt from anything.
const theConstantTimePackagePath = "crypto/subtle"

// One import as a call site sees it. The name is what an expression qualifies with and the path
// is where the class reads that package's declarations from, so the two travel together: a
// package imported under an alias is banned under the alias, and a package with no import is a
// package with no call.
type comparatorImport struct {
	name string
	path string
}

// The imports of one set of parsed files, deduplicated, in the order they were read.
//
// This is what makes the class SELF EXTENDING and is the reason it is read off the imports rather
// than off a registry. A comparator cannot be called without its package being imported, so the
// edit that writes the call writes the import, and that package's whole comparator surface enters
// the class on the same run -- including a package nothing here has ever imported and a
// comparator the standard library has not shipped yet.
func importsOfSources(files []parsedSource) []comparatorImport {
	found := []comparatorImport{}
	for _, parsed := range files {
		for _, spec := range parsed.file.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				continue
			}
			one := comparatorImport{name: path[strings.LastIndex(path, "/")+1:], path: path}
			if spec.Name != nil {
				one.name = spec.Name.Name
			}
			if !slices.Contains(found, one) {
				found = append(found, one)
			}
		}
	}
	return found
}

// Whether a type is the shape of a byte string: a slice of octets or of runes, a string, an
// unconstrained interface, or a type parameter, which is how a generic comparator spells both of
// its arguments and which cannot be resolved further from the signature alone.
//
// The type ITSELF and not its underlying type, which is the line that keeps a primitive out of
// the class. ed25519.PublicKey is a defined type whose underlying type is []byte; reading through
// the definition would make every function taking a key a function taking a byte string, and the
// class would then hold ed25519.Verify and need an exemption to let this package sign anything.
// A comparator over byte strings is written over byte strings.
func isDataShapedType(one types.Type) bool {
	if _, isParameter := types.Unalias(one).(*types.TypeParam); isParameter {
		return true
	}
	switch spelled := types.Unalias(one).(type) {
	case *types.Slice:
		element, isBasic := spelled.Elem().(*types.Basic)
		return isBasic && (element.Kind() == types.Byte || element.Kind() == types.Rune)
	case *types.Basic:
		return spelled.Kind() == types.String
	case *types.Interface:
		return spelled.NumMethods() == 0
	}
	return false
}

// Whether a type is the shape of ONE element of a byte string, which is what bytes.IndexByte and
// slices.Contains take beside the string they search.
func isScalarShapedType(one types.Type) bool {
	if _, isParameter := types.Unalias(one).(*types.TypeParam); isParameter {
		return true
	}
	basic, isBasic := types.Unalias(one).(*types.Basic)
	return isBasic && (basic.Kind() == types.Byte || basic.Kind() == types.Rune)
}

// Whether a parameter is one a comparison over byte strings is allowed to take at all: the data,
// an element of it, or a callback.
//
// The callback is admitted deliberately. slices.EqualFunc compares its two arguments through a
// function the caller supplies and is a variable time comparison of exactly the kind this bans;
// a rule that refused every function typed parameter would drop it. A parameter of any OTHER type
// -- a key, a hash, a reader, a cipher -- is what says the answer depends on something besides
// the two strings, which is what a primitive is and what a comparator is not.
func isComparisonParameter(one types.Type) bool {
	if isDataShapedType(one) || isScalarShapedType(one) {
		return true
	}
	_, isFunction := types.Unalias(one).(*types.Signature)
	return isFunction
}

// Whether a function answers a question rather than producing a value: a bool or an int somewhere
// in what it hands back. bytes.Equal answers the first and bytes.Compare and bytes.Index the
// second, and an int answer leaks strictly more than a bool one.
func answersAQuestion(signature *types.Signature) bool {
	for i := range signature.Results().Len() {
		basic, isBasic := types.Unalias(signature.Results().At(i).Type()).Underlying().(*types.Basic)
		if isBasic && (basic.Kind() == types.Bool || basic.Kind() == types.Int) {
			return true
		}
	}
	return false
}

// Whether one declaration of another package compares two byte strings, which is the whole of
// what makes a function a member of the class.
//
// It over reports where it is unsure and that is the direction it is meant to fail in. It calls
// hmac.Equal a member although hmac.Equal is constant time, because guardrail 8's text is that
// the comparison is SPELLED with crypto/subtle rather than that it happens to be safe, and it
// calls strconv.UnquoteChar one because a function over a string and an octet answering a bool is
// indistinguishable from a comparator at this depth. A member that should not be one is an
// argument with a reviewer at compile time; a member that is missing is the mutant that lived.
func isDataComparator(function *types.Func) bool {
	signature, isSignature := function.Type().(*types.Signature)
	if !isSignature || signature.Recv() != nil || !function.Exported() {
		return false
	}
	if !answersAQuestion(signature) {
		return false
	}
	parameters := signature.Params()
	for i := range parameters.Len() {
		if !isComparisonParameter(parameters.At(i).Type()) {
			return false
		}
	}
	for i := range parameters.Len() {
		if !isDataShapedType(parameters.At(i).Type()) {
			continue
		}
		for j := range parameters.Len() {
			if i == j {
				continue
			}
			one, other := parameters.At(i).Type(), parameters.At(j).Type()
			if types.Identical(one, other) || isScalarShapedType(other) {
				return true
			}
		}
	}
	return false
}

// One imported package as the type checker reads it, from that package's own source.
//
// A package that cannot be resolved is fatal rather than skipped, because the comparators of a
// package this gate cannot read are comparators it cannot ban, and a silently skipped import is
// an import every call through it is cleared for. It goes through the same source importer the
// crypto's type gates use, which needs no build cache and no go command on the path.
func importedPackageOf(t *testing.T, path string) *types.Package {
	t.Helper()
	cryptoTypeCheckMutex.Lock()
	defer cryptoTypeCheckMutex.Unlock()
	imported, err := cryptoTypeImporter().Import(path)
	if err != nil {
		t.Fatalf("import %s to read its comparators: %v; a package this gate cannot read is a package every call into it is cleared for", path, err)
	}
	return imported
}

// The comparator class, computed from the source of the packages the given files import.
func dataComparatorsOf(t *testing.T, where string, files []parsedSource) []string {
	t.Helper()
	imports := importsOfSources(files)
	if len(imports) == 0 {
		t.Fatalf("%s imports nothing, so a class derived from its imports is empty and every call is cleared", where)
	}
	found := []string{}
	for _, one := range imports {
		if one.path == theConstantTimePackagePath {
			continue
		}
		scope := importedPackageOf(t, one.path).Scope()
		if len(scope.Names()) == 0 {
			t.Fatalf("%s (%s) declares nothing, so no comparator of it can be in the class", one.name, one.path)
		}
		for _, name := range scope.Names() {
			function, isFunction := scope.Lookup(name).(*types.Func)
			if isFunction && isDataComparator(function) {
				found = append(found, one.name+"."+name)
			}
		}
	}
	slices.Sort(found)
	return slices.Compact(found)
}

// One call to a member of the class, with where it was written, because a gate that reports a
// violation has to say which line to open.
type comparatorCall struct {
	text  string
	where string
}

// Every call to a member of the class in one whole file.
//
// The FILE and not its declarations. A class of functions has an outside -- that is what this
// file is written against -- and a package level var initialised with bytes.Equal is in the
// outside of every class of functions there is.
func comparatorCallsIn(parsed parsedSource, class []string) []comparatorCall {
	found := []comparatorCall{}
	ast.Inspect(parsed.file, func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if !isCall {
			return true
		}
		text := parsed.render(call.Fun)
		if slices.Contains(class, text) {
			found = append(found, comparatorCall{text: text, where: parsed.fileSet.Position(call.Pos()).String()})
		}
		return true
	})
	return found
}

// The distinct comparators a set of calls names, sorted, which is what an expectation is stated
// in: a position moves whenever a line above it does and is not the property being asserted.
func comparatorsNamedBy(calls []comparatorCall) []string {
	found := []string{}
	for _, one := range calls {
		found = append(found, one.text)
	}
	slices.Sort(found)
	return slices.Compact(found)
}

// A fixture committing one of every shape the rule has to tell apart, so a matcher that stopped
// matching fails here rather than issuing the real source the clean bill a working one issues.
//
// Every member is here because some half of the rule has to be the only thing reporting it, or
// the only thing NOT reporting it:
//
//   - ComparesWithBytesEqual is the comparator an enumeration would have thought of, and
//     ComparesWithAPrefix, ComparesWithACut and ComparesWithSlicesEqual are three that the six
//     name enumeration this project shipped did not hold. bytes.HasPrefix leaks strictly more
//     than bytes.Equal does: one answer per query about how many leading octets matched.
//   - ComparesWithHmacEqual is constant time and is reported anyway, which is what says the rule
//     is about the SPELLING guardrail 8 gives and not about a judgement of safety.
//   - ComparesWithTheSanctionedCall is the negative half: the one call that must not be reported.
//   - VerifiesASignature is the boundary. ed25519.Verify answers a bool over two byte strings and
//     must be outside the class, because it takes a key and so answers a question about something
//     other than the two strings. If it were inside, this package could not sign anything without
//     an exemption written into the gate.
//   - ComparesNothing compares two lengths, which is a variable time decision that names no
//     comparator, and it is NOT reported: it is the shape this gate cannot see and the shape
//     tree_test.go's equality rule exists for.
//   - acceptsEverything is the same call in a package level initializer, which is what says the
//     rule reads files rather than declarations.
const packageWideComparatorControl = `package control

import (
	"bytes"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/subtle"
	"slices"
)

var chosenPrefix = []byte{0x00}

var acceptsEverything = bytes.Equal(chosenPrefix, chosenPrefix)

func ComparesWithTheSanctionedCall(a []byte, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}

func ComparesWithBytesEqual(a []byte, b []byte) bool {
	return bytes.Equal(a, b)
}

func ComparesWithAPrefix(a []byte, b []byte) bool {
	return bytes.HasPrefix(a, b)
}

func ComparesWithACut(a []byte, b []byte) bool {
	_, _, found := bytes.Cut(a, b)
	return found
}

func ComparesWithSlicesEqual(a []byte, b []byte) bool {
	return slices.Equal(a, b)
}

func ComparesWithHmacEqual(a []byte, b []byte) bool {
	return hmac.Equal(a, b)
}

func VerifiesASignature(pub ed25519.PublicKey, message []byte, sig []byte) bool {
	return ed25519.Verify(pub, message, sig)
}

func ComparesNothing(a []byte, b []byte) bool {
	return len(a) == len(b)
}
`

// The comparators the fixture's own imports must derive, named one at a time, because "the class
// is bigger than a list" is not an observation and "the class holds this member" is.
var packageWideComparatorControlHolds = []string{
	"bytes.Compare",
	"bytes.Contains",
	"bytes.Cut",
	"bytes.Equal",
	"bytes.EqualFold",
	"bytes.HasPrefix",
	"bytes.HasSuffix",
	"bytes.Index",
	"bytes.IndexByte",
	"hmac.Equal",
	"slices.BinarySearch",
	"slices.Compare",
	"slices.Contains",
	"slices.Equal",
	"slices.EqualFunc",
	"slices.Index",
}

// The two it must be seen to LEAVE OUT, so a reader can tell a deliberate boundary from an
// oversight. subtle.ConstantTimeCompare is out by the package exemption; ed25519.Verify is out by
// the shape, and the shape is the sentence about keys above.
var packageWideComparatorControlLeavesOut = []string{
	"ed25519.Verify",
	"subtle.ConstantTimeCompare",
}

// What the rule must report over the fixture, exactly rather than as a floor. A rule that widened
// to flag every call it reads fails here as surely as one that stopped matching.
var packageWideComparatorControlReports = []string{
	"bytes.Cut",
	"bytes.Equal",
	"bytes.HasPrefix",
	"hmac.Equal",
	"slices.Equal",
}

// The number of CALLS, which is one more than the number of distinct comparators: bytes.Equal is
// written twice, once in a function and once in a package level initializer. It is asserted
// because it is the only thing that says the initializer was read.
const packageWideComparatorControlCallCount = 6

// TestThePackageWideComparatorGateFlagsItsControlFixture is the matcher's own control, and it
// runs before the gate over the real source so that a rule which stopped matching fails here
// rather than issuing this package a clean bill.
func TestThePackageWideComparatorGateFlagsItsControlFixture(t *testing.T) {
	control := mustParseText(t, "the package wide comparator control", packageWideComparatorControl)
	files := []parsedSource{control}
	class := dataComparatorsOf(t, "the control fixture", files)
	t.Logf("%d comparators derived from the fixture's imports: %v", len(class), class)
	for _, want := range packageWideComparatorControlHolds {
		if !slices.Contains(class, want) {
			t.Errorf("the class derived from the fixture's imports does not hold %s, so a call to it in this package's source would be cleared", want)
		}
	}
	for _, unwanted := range packageWideComparatorControlLeavesOut {
		if slices.Contains(class, unwanted) {
			t.Errorf("the class holds %s, which is named as being outside it", unwanted)
		}
	}
	calls := comparatorCallsIn(control, class)
	if len(calls) != packageWideComparatorControlCallCount {
		t.Errorf("the rule found %d calls in the control, want %d; the extra one is the package level initializer, and a rule reading declarations rather than files finds %d",
			len(calls), packageWideComparatorControlCallCount, packageWideComparatorControlCallCount-1)
	}
	if got := comparatorsNamedBy(calls); !slices.Equal(got, packageWideComparatorControlReports) {
		t.Errorf("the rule reported %v out of the control, want %v", got, packageWideComparatorControlReports)
	}
}

// TestNothingThisPackageShipsComparesDataOutsideConstantTime is guardrail 8 with both of its
// lists computed: every production file of this package, and every comparator of every package
// they import.
//
// The package obeys it literally. Its six comparisons of data -- crypto.go's MacVerify, three in
// tree.go, one in tree_hash.go and one in treekem.go -- are all spelled
// crypto/subtle.ConstantTimeCompare, which is why this rule needs no exemption written into it
// and why nothing here has to decide which comparison is over a secret.
func TestNothingThisPackageShipsComparesDataOutsideConstantTime(t *testing.T) {
	files := parsedProductionSourcesOfThisPackage(t)
	class := dataComparatorsOf(t, "this package's production source", files)
	t.Logf("%d production files, %d imports, %d comparators in the derived class: %v",
		len(files), len(importsOfSources(files)), len(class), class)
	if len(class) == 0 {
		t.Fatal("the class derived from this package's imports is empty, so the rule below cleared every call having read nothing")
	}
	for _, parsed := range files {
		for _, call := range comparatorCallsIn(parsed, class) {
			t.Errorf("%s at %s compares data outside constant time; every comparison of data in this package goes through %s",
				call.text, call.where, theSanctionedComparison())
		}
	}
}
