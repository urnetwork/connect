// Unit tests for framing.go and framing_errors.go: the three RFC 9420 section 6 framing
// registries, the Sender codec, and the structural refusals this layer declares.
package mls

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/urnetwork/connect/mls/syntax"
)

// ---------------------------------------------------------------------------
// the widths the wire format gives the three registries
// ---------------------------------------------------------------------------

// The upper half of each width, as a compile time statement. ^T(0) is a typed constant equal
// to the type's own maximum, so converting it to the octet or the pair of octets the wire
// format gives that registry is exactly the sentence "this type is no wider than that", and a
// widened type stops COMPILING here rather than failing a test somewhere else.
//
// A conversion of a literal in the other direction -- SenderType(0xff) -- would say only that
// 255 FITS, which rules out a signed octet and nothing else; it compiles unchanged at uint16
// and at every wider type. That is measured on this project rather than supposed: ContentType
// widened to uint16 built clean and failed exactly one test in this package.
const (
	_ = uint8(^ContentType(0))
	_ = uint8(^SenderType(0))
	_ = uint16(^WireFormat(0))
)

// ---------------------------------------------------------------------------
// the class every table in this file is a claim about
// ---------------------------------------------------------------------------

// framingFile and framingTestFile are named rather than spelled at each call site, because a
// gate whose file no longer exists derives the empty set, and the empty set agrees with an
// empty table.
//
// framingFile is the file the three registry TYPES are declared in, and that is now ALL it is.
// It used to be the derivation root of every gate below as well -- the switches, the codec
// methods, the compile time pins -- which made each of them a claim about one FILE where the
// claim being made is about the package. framing_preimage.go's confirmedTranscriptHashInput
// reads and writes the same wire format registry AuthenticatedContent does, and it was outside
// all of those gates at once: both of its registry switches could be deleted whole with
// ./mls/... and ./message/... byte identically green, while the byte identical switch one file
// over is caught by two of them. Measured, not supposed. The file SET is derived now, by
// framingRegistryFunctions below, off which functions of this package's production source
// actually compute with a framing registry.
const (
	framingFile     = "framing.go"
	framingTestFile = "framing_test.go"
)

// framingRegistryTypesDeclaredIn is every framing registry the named file declares, derived as
// every named type of that file whose underlying type is an unsigned integer.
//
// Derived and not listed, and the hole it closes is measured rather than supposed. Every table
// in this file -- the code points, the section 6.1 arm order, the widths, the compile time pins
// -- is a claim about a SET, and each of the gates joining them iterated the keys of its OWN
// table, so the set of registries was the hand written half of all four at once. A fourth
// registry type declared in framing.go was therefore joined to the RFC by nothing at all: a
// ProposalOrRefType with a deliberately WRONG reference code point, added to framing.go with no
// entry in any table here, left ./mls/... and ./message/... byte identically green.
//
// Unsigned integer rather than extension.go's uint16 exactly, because framing's registries are
// not one width -- WireFormat is the pair of octets section 17 gives it and the other two are
// single octets -- so a filter naming one width would drop two of the three it exists to find.
// Sender is a struct and is not one of these, which is the right cut: a registry is the scalar
// a discriminant is read at, and that is the thing a code point can be wrong about.
func framingRegistryTypesDeclaredIn(t *testing.T, file string) []string {
	t.Helper()
	declared := packageLevelDeclarations(t, ".")
	pkg := typeCheckedPackage(t)
	names := []string{}
	for name, declaredIn := range declared {
		if declaredIn != file {
			continue
		}
		typeName, isType := pkg.Scope().Lookup(name).(*types.TypeName)
		if !isType {
			continue
		}
		basic, isBasic := typeName.Type().Underlying().(*types.Basic)
		if !isBasic || basic.Info()&types.IsUnsigned == 0 {
			continue
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// framingRegistries is that derivation plus the control every caller owes it. An empty result
// is fatal here rather than at each call site: framing.go certainly declares three, so a
// derivation that found none is a reader that broke, and a set join against the empty set is a
// gate reporting green having read nothing.
func framingRegistries(t *testing.T) []string {
	t.Helper()
	declared := framingRegistryTypesDeclaredIn(t, framingFile)
	if len(declared) == 0 {
		t.Fatalf("no registry type was derived from %s, which declares three, so every set join below would compare against nothing",
			framingFile)
	}
	return declared
}

// framingProductionFiles is every production .go file of package mls, read off the directory.
//
// The directory rather than a list, and it is the root of the class every gate below sweeps. A
// class that is "the files somebody remembered" is a class a new file joins by not being
// remembered, which is this project's most repeated failure and was this file's own.
func framingProductionFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	found := []string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		found = append(found, name)
	}
	slices.Sort(found)
	if len(found) == 0 {
		t.Fatal("no production file was read out of the package directory, so every derivation below would run over the empty set")
	}
	return found
}

// framingRegistryIdentifiers is every identifier whose appearance inside a function makes that
// function a user of a framing registry: the registry TYPE names, and every package level
// constant declared at one of those types.
//
// Both halves, because either alone understates the class. A codec can name the type and no
// constant -- a decoder that reads a uint16 and converts it -- and it can name constants and no
// type, which is the shape of every switch in this package. The union is what these gates are
// about.
func framingRegistryIdentifiers(t *testing.T) map[string]bool {
	t.Helper()
	found := map[string]bool{}
	for _, registry := range framingRegistries(t) {
		found[registry] = true
		for name := range registryConstantsOfType(t, registry) {
			found[name] = true
		}
	}
	if len(found) == 0 {
		t.Fatal("no framing registry identifier was derived, so the function set below would be empty and every gate over it would report green having read nothing")
	}
	return found
}

// mentionsAnyIdentifier reports whether a syntax tree names any of the given identifiers.
func mentionsAnyIdentifier(node ast.Node, wanted map[string]bool) bool {
	found := false
	ast.Inspect(node, func(inner ast.Node) bool {
		if found {
			return false
		}
		if name, isName := inner.(*ast.Ident); isName && wanted[name.Name] {
			found = true
			return false
		}
		return true
	})
	return found
}

// framingRegistryFunction is one function of this package's production source that computes
// with a framing registry: its qualified name, the file that declares it, and its declaration.
type framingRegistryFunction struct {
	name string
	file string
	decl *ast.FuncDecl
}

// framingRegistryFunctions is that class, derived over the whole package rather than over one
// file.
//
// The SIGNATURE and the BODY are read and nothing else is -- not the doc comment, not the rest
// of the file -- because what puts a function in this class is that it computes with a registry,
// not that it sits beside something that does. framing_errors.go spends nine lines on
// ErrUnknownContentType in prose and returns it nowhere, which is exactly the false positive a
// file level text search reports and the one filesRaising further down is written against.
func framingRegistryFunctions(t *testing.T) []framingRegistryFunction {
	t.Helper()
	wanted := framingRegistryIdentifiers(t)
	found := []framingRegistryFunction{}
	for _, file := range framingProductionFiles(t) {
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, declaration := range parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil {
				continue
			}
			if !mentionsAnyIdentifier(function.Type, wanted) && !mentionsAnyIdentifier(function.Body, wanted) {
				continue
			}
			name := function.Name.Name
			if function.Recv != nil && len(function.Recv.List) > 0 {
				name = receiverTypeName(function.Recv.List[0].Type) + "." + name
			}
			found = append(found, framingRegistryFunction{name: name, file: file, decl: function})
		}
	}
	slices.SortFunc(found, func(a framingRegistryFunction, b framingRegistryFunction) int {
		return strings.Compare(a.name, b.name)
	})
	if len(found) == 0 {
		t.Fatal("no function of this package's production source computes with a framing registry, and framing.go alone declares nine, so every gate below would run over the empty set")
	}
	return found
}

// framingRegistryBits is the width the type checker reads off a registry's own declaration. It
// is what the compile time pin is held to below, so the pin is joined to the type rather than
// to a fourth transcription of the RFC's octet counts.
func framingRegistryBits(t *testing.T, typeName string) int {
	t.Helper()
	declared, isType := typeCheckedPackage(t).Scope().Lookup(typeName).(*types.TypeName)
	if !isType {
		t.Fatalf("%s is not a type of package mls, so it has no width to read", typeName)
	}
	basic, isBasic := declared.Type().Underlying().(*types.Basic)
	if !isBasic {
		t.Fatalf("%s does not have a basic underlying type, so it is not a registry", typeName)
	}
	switch basic.Kind() {
	case types.Uint8:
		return 8
	case types.Uint16:
		return 16
	case types.Uint32:
		return 32
	case types.Uint64:
		return 64
	}
	t.Fatalf("%s is declared as %s, which is not one of the sized unsigned integers a wire registry can be read at",
		typeName, basic)
	return 0
}

// sizedUnsignedConversionBits is the whole of Go's sized unsigned integer conversions against
// the width each one states. Closed by the language rather than by anybody's judgement, which
// is why it can be written out: uint and uintptr are deliberately absent, since a conversion to
// a platform width bounds a registry at nothing portable.
var sizedUnsignedConversionBits = map[string]int{"uint8": 8, "uint16": 16, "uint32": 32, "uint64": 64}

// widthPinOf recognises exactly the form a width pin has -- uintN(^T(0)) -- and reports T and
// N. Anything else, a bare literal included, is not a pin and is reported as not one.
func widthPinOf(expression ast.Expr) (string, int, bool) {
	conversion, isCall := expression.(*ast.CallExpr)
	if !isCall || len(conversion.Args) != 1 {
		return "", 0, false
	}
	target, isName := conversion.Fun.(*ast.Ident)
	if !isName {
		return "", 0, false
	}
	bits, isSizedUnsigned := sizedUnsignedConversionBits[target.Name]
	if !isSizedUnsigned {
		return "", 0, false
	}
	complement, isUnary := conversion.Args[0].(*ast.UnaryExpr)
	if !isUnary || complement.Op != token.XOR {
		return "", 0, false
	}
	maximum, isInnerCall := complement.X.(*ast.CallExpr)
	if !isInnerCall || len(maximum.Args) != 1 {
		return "", 0, false
	}
	registry, isRegistryName := maximum.Fun.(*ast.Ident)
	if !isRegistryName {
		return "", 0, false
	}
	zero, isLiteral := maximum.Args[0].(*ast.BasicLit)
	if !isLiteral || zero.Kind != token.INT || zero.Value != "0" {
		return "", 0, false
	}
	return registry.Name, bits, true
}

// framingWidthPins reads a test file's own source and returns, for each registry it bounds, the
// width that registry's compile time pin converts to.
//
// Read as a SHAPE rather than counted, and that distinction is this reader's whole reason to
// exist. The package wide guard on the pin block, key_schedule_deps_test.go's
// TestNoPinBlockShrinksWithoutFailing, counts blank identifiers: pinBlockSizes says this file
// holds three and three is what it counts. Rewriting the WireFormat pin to a bare `_ = 0` keeps
// that count at three and leaves the whole package green with the upper bound statement gone --
// measured, not supposed. A pin is its conversion or it is nothing, so this reads the
// conversion.
func framingWidthPins(t *testing.T, file string) map[string]int {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	pins := map[string]int{}
	for _, declaration := range parsed.Decls {
		generic, isGeneric := declaration.(*ast.GenDecl)
		if !isGeneric || (generic.Tok != token.CONST && generic.Tok != token.VAR) {
			continue
		}
		for _, spec := range generic.Specs {
			value, isValue := spec.(*ast.ValueSpec)
			if !isValue || len(value.Names) != 1 || value.Names[0].Name != "_" || len(value.Values) != 1 {
				continue
			}
			registry, bits, isWidthPin := widthPinOf(value.Values[0])
			if !isWidthPin {
				continue
			}
			if other, repeated := pins[registry]; repeated {
				t.Fatalf("%s pins %s twice, at uint%d and at uint%d, so neither line is the statement about its width",
					file, registry, other, bits)
			}
			pins[registry] = bits
		}
	}
	return pins
}

// TestTheWidthPinReaderReadsTheFormAndNotThePresence is the control on that reader. One that
// recognised everything would report three pins out of a file whose pins had all been neutered,
// and one that recognised nothing would report none out of the real file and fail loudly for
// the wrong reason, so both halves are stated over expressions written here.
func TestTheWidthPinReaderReadsTheFormAndNotThePresence(t *testing.T) {
	for _, one := range []struct {
		source   string
		registry string
		bits     int
		isPin    bool
	}{
		{"uint8(^ContentType(0))", "ContentType", 8, true},
		{"uint16(^WireFormat(0))", "WireFormat", 16, true},
		{"uint32(^Something(0))", "Something", 32, true},
		// the neutered pin, which is the survivor this reader exists for
		{"0", "", 0, false},
		// the conversion written the other way round, which says only that the value fits
		{"ContentType(0xff)", "", 0, false},
		{"uint8(^ContentType(1))", "", 0, false},
		{"uint(^ContentType(0))", "", 0, false},
		{"uint8(ContentType(0))", "", 0, false},
		{"^ContentType(0)", "", 0, false},
	} {
		expression, err := parser.ParseExpr(one.source)
		if err != nil {
			t.Fatalf("parse %q: %v", one.source, err)
		}
		registry, bits, isPin := widthPinOf(expression)
		if isPin != one.isPin || registry != one.registry || bits != one.bits {
			t.Errorf("%q read as (%q, %d, %v), want (%q, %d, %v)",
				one.source, registry, bits, isPin, one.registry, one.bits, one.isPin)
		}
	}
}

// TestEveryFramingRegistryCarriesTheCompileTimeWidthPinThatBoundsIt is the derived half of the
// pin block at the top of this file, and it states two things nothing else did.
//
// A registry declared in framing.go with no pin at all is the shape a NEW registry takes, and
// the package wide count cannot see it: pinBlockSizes is one number per file, so a fourth
// registry landing unpinned leaves that number correct. And a pin neutered in place -- the
// conversion replaced by a bare literal -- keeps the count too, which is a survivor this file
// was measured to have rather than a hypothesis about one.
//
// The pin's target width is held to the registry's OWN width rather than to another reading of
// the RFC, because the two ends are already joined:
// TestTheFramingRegistriesAreTheWidthsTheWireFormatGivesThem holds the type's width to the
// number RFC 9420 gives it, and this holds the pin to the type. A pin converting to a width
// wider than its type compiles, says only that the maximum FITS, and goes on compiling through
// the widening it exists to refuse.
func TestEveryFramingRegistryCarriesTheCompileTimeWidthPinThatBoundsIt(t *testing.T) {
	declared := framingRegistries(t)
	pins := framingWidthPins(t, framingTestFile)
	if got := slices.Sorted(maps.Keys(pins)); !slices.Equal(got, declared) {
		t.Fatalf("%s pins the widths of %v and %s declares the registries %v; a registry with no pin is one a widening does not stop compiling, and a pin naming a type that is no registry here is a pin on nothing",
			framingTestFile, got, framingFile, declared)
	}
	for _, name := range declared {
		if want := framingRegistryBits(t, name); pins[name] != want {
			t.Errorf("%s is declared %d bits wide and its compile time pin converts to uint%d; a pin no tighter than its own type still compiles after the widening it exists to refuse",
				name, want, pins[name])
		}
	}
	t.Logf("%d framing registries, each bounded above by a pin of its own width", len(declared))
}

// TestTheFramingRegistriesAreTheWidthsTheWireFormatGivesThem is the lower half, which the
// conversions above cannot give: they hold that the type is no WIDER, and this holds that it is
// no narrower. A registry read at fewer octets than the encoding writes moves every field after
// it in the message.
func TestTheFramingRegistriesAreTheWidthsTheWireFormatGivesThem(t *testing.T) {
	measured := []string{}
	for _, one := range []struct {
		name       string
		codePoints int
		want       int
	}{
		{"ContentType", int(^ContentType(0)) + 1, 256},
		{"SenderType", int(^SenderType(0)) + 1, 256},
		{"WireFormat", int(^WireFormat(0)) + 1, 65536},
	} {
		measured = append(measured, one.name)
		if one.codePoints != one.want {
			t.Errorf("%s holds %d code points and RFC 9420 section 6 gives it %d",
				one.name, one.codePoints, one.want)
		}
	}
	// the sweep above has to name its types, because int(^T(0)) is a conversion the compiler
	// resolves and there is no writing it over a name derived at run time. What CAN be derived
	// is the set it was supposed to cover, so the enumeration is joined to it here: a fourth
	// registry declared in framing.go fails this rather than being a width nothing measures.
	slices.Sort(measured)
	if declared := framingRegistries(t); !slices.Equal(measured, declared) {
		t.Errorf("this sweep measures the widths of %v and %s declares the registries %v; a registry it does not measure is one nothing holds to a lower bound",
			measured, framingFile, declared)
	}
}

// ---------------------------------------------------------------------------
// the framing registries, joined to the RFC by name and out of two readings
// ---------------------------------------------------------------------------

// rfc9420FramingCodePoints is what RFC 9420 assigns the three framing registries, keyed by the
// RFC's OWN name for each code point rather than by the Go identifier that carries it:
// WireFormat out of the section 17 IANA "MLS Wire Formats" registry, ContentType and SenderType
// out of the enum bodies section 6 writes beside FramedContent and Sender.
//
// Keyed by the RFC's name because the join below is BY NAME, and that is the whole of what
// makes this more than a restatement of framing.go. A table keyed by Go identifier can only
// ever agree with whatever the person who wrote the constants believed. This project has the
// receipt: ExtensionTypeExternalSenders shipped at external_pub's 0x0004, and the pin written
// to catch exactly that -- transcribed by the same person out of the same RFC section --
// agreed with the same misreading and passed.
var rfc9420FramingCodePoints = map[string]map[string]uint64{
	"WireFormat": {
		"mls_public_message":  0x0001,
		"mls_private_message": 0x0002,
		"mls_welcome":         0x0003,
		"mls_group_info":      0x0004,
		"mls_key_package":     0x0005,
	},
	"ContentType": {
		"application": 1,
		"proposal":    2,
		"commit":      3,
	},
	"SenderType": {
		"member":              1,
		"external":            2,
		"new_member_proposal": 3,
		"new_member_commit":   4,
	},
}

// rfc9420FramingRegistryPrefix is the prefix the RFC's spelling carries that the Go name does
// not. The wire format registry writes mls_public_message; this package writes
// WireFormatPublicMessage, because the type name already says which protocol it belongs to.
// Stated as data, once, rather than folded into each table entry, so the correspondence is one
// claim that can be read rather than five that can disagree.
var rfc9420FramingRegistryPrefix = map[string]string{"WireFormat": "mls_"}

// rfcNameOfFramingConstant is rfcNameOfRegistryConstant plus that prefix.
func rfcNameOfFramingConstant(typeName string, constantName string) string {
	return rfc9420FramingRegistryPrefix[typeName] + rfcNameOfRegistryConstant(typeName, constantName)
}

// TestEveryFramingRegistryHoldsTheCodePointsRfc9420Assigns joins this package's declared
// constants to the RFC's table by NAME, in both directions at once.
//
// Both directions matter and neither implies the other. A table richer than the source names a
// code point nothing declares; a source richer than the table is a constant no gate checks,
// which is how a swapped pair survives -- and a swapped pair is not a type error, is not a
// round trip failure, and is byte exact against any corpus this implementation produced.
func TestEveryFramingRegistryHoldsTheCodePointsRfc9420Assigns(t *testing.T) {
	// the set of registries first, because the table's KEYS were the hand written part of this
	// gate and a registry absent from them was joined to the RFC by nothing. That is the loud
	// case, in extension_test.go's words, because it is the shape a new registry takes.
	declared := framingRegistries(t)
	if got := slices.Sorted(maps.Keys(rfc9420FramingCodePoints)); !slices.Equal(got, declared) {
		t.Fatalf("rfc9420FramingCodePoints holds %v and %s declares the registries %v; a registry with no entry is one whose code points nothing here reads",
			got, framingFile, declared)
	}
	for _, typeName := range declared {
		derived := registryConstantsOfType(t, typeName)
		byRfcName := map[string]uint64{}
		for _, name := range slices.Sorted(maps.Keys(derived)) {
			rfcName := rfcNameOfFramingConstant(typeName, name)
			if _, clash := byRfcName[rfcName]; clash {
				t.Fatalf("two constants of %s spell the RFC name %s, so the join below cannot tell them apart",
					typeName, rfcName)
			}
			byRfcName[rfcName] = derived[name]
		}
		if want := rfc9420FramingCodePoints[typeName]; !maps.Equal(byRfcName, want) {
			t.Errorf("%s declares\n %v\nand RFC 9420 assigns\n %v", typeName, byRfcName, want)
		}
	}
}

// rfc9420SelectArmOrder is a SECOND reading of two of those registries, out of a different part
// of the RFC and carrying no numbers at all.
//
// RFC 9420 section 6.1 writes the arms of MLSMessage's select and of FramedContent's select in
// registry order, beginning at the first non-reserved code point. That is an ORDER rather than
// a table of values, so it cannot be a second transcription of the same numbers -- which is
// exactly what makes joining it to the table above worth doing. Every transposition of two code
// points in either registry fails here, including the ones no golden in this task can see
// because no codec here writes them yet.
//
// SenderType is deliberately absent, and saying so is more useful than a table that pretended
// otherwise: section 6 writes the Sender select's arms in the order member, external,
// new_member_commit, new_member_proposal, which is NOT the enum's order, so there is no
// ordering statement to join. rfc9420SenderSelectPayloads below is SenderType's second reading
// instead, and it is a weaker one -- it separates the arms that carry a uint32 from the arms
// that carry nothing, so it catches every swap ACROSS that boundary and no swap within it.
// What that leaves open is stated plainly rather than hidden: member against external, and
// new_member_proposal against new_member_commit, rest on one sentence of RFC 9420 in this
// package today, and the vendored message-protection and messages vectors are the outside
// oracle that will close them.
var rfc9420SelectArmOrder = map[string][]string{
	"WireFormat":  {"mls_public_message", "mls_private_message", "mls_welcome", "mls_group_info", "mls_key_package"},
	"ContentType": {"application", "proposal", "commit"},
}

// framingRegistriesWithNoSelectArmOrder is the registries section 6.1 states no ordering for,
// each against the reason there is none.
//
// A waiver rather than an omission, and written where a derivation can see it. The set this
// second reading covered used to be the keys of rfc9420SelectArmOrder alone, so "SenderType is
// deliberately absent" was a sentence in a comment and nothing more -- and a FOURTH registry
// absent for no reason at all read exactly the same to every gate in this file. The two lists
// together must be every registry framing.go declares, which makes leaving one out a sentence
// somebody has to write rather than a line somebody does not.
var framingRegistriesWithNoSelectArmOrder = map[string]string{
	"SenderType": "section 6 writes the Sender select's arms member, external, new_member_commit, " +
		"new_member_proposal, which is not the enum's order, so there is no ordering statement to " +
		"join; rfc9420SenderSelectPayloads is SenderType's second reading instead",
}

// TestTheFramingRegistriesRunInTheOrderSection61WritesTheirArms is that join.
func TestTheFramingRegistriesRunInTheOrderSection61WritesTheirArms(t *testing.T) {
	declared := framingRegistries(t)
	ordered := slices.Sorted(maps.Keys(rfc9420SelectArmOrder))
	waived := slices.Sorted(maps.Keys(framingRegistriesWithNoSelectArmOrder))
	for _, typeName := range ordered {
		if _, alsoWaived := framingRegistriesWithNoSelectArmOrder[typeName]; alsoWaived {
			t.Fatalf("%s is both given an arm order and waived from having one, so the two lists do not partition the registries",
				typeName)
		}
	}
	for _, typeName := range waived {
		if strings.TrimSpace(framingRegistriesWithNoSelectArmOrder[typeName]) == "" {
			t.Errorf("%s is waived from this join with no reason written; a waiver nobody had to justify is an omission wearing a table entry",
				typeName)
		}
	}
	accounted := slices.Concat(ordered, waived)
	slices.Sort(accounted)
	if !slices.Equal(accounted, declared) {
		t.Fatalf("%s declares the registries %v; section 6.1's arm order is joined for %v and waived for %v, and a registry in neither list is one this second reading never sees",
			framingFile, declared, ordered, waived)
	}
	for _, typeName := range ordered {
		arms := rfc9420SelectArmOrder[typeName]
		derived := registryConstantsOfType(t, typeName)
		byRfcName := map[string]uint64{}
		for name, value := range derived {
			byRfcName[rfcNameOfFramingConstant(typeName, name)] = value
		}
		if len(byRfcName) != len(arms) {
			t.Errorf("%s declares %d code points and section 6.1 writes %d arms for it: %v against %v",
				typeName, len(byRfcName), len(arms), slices.Sorted(maps.Keys(byRfcName)), arms)
		}
		for at, arm := range arms {
			value, declared := byRfcName[arm]
			if !declared {
				t.Errorf("section 6.1 writes a %s arm for %s and this package declares no constant that spells it",
					arm, typeName)
				continue
			}
			if want := uint64(at + 1); value != want {
				t.Errorf("%s is the %s %s arm section 6.1 writes, so its code point is %d, and this package declares it at %d",
					arm, ordinalOfSelectArm(at), typeName, want, value)
			}
		}
	}
}

func ordinalOfSelectArm(at int) string {
	for ordinal, name := range []string{"first", "second", "third", "fourth", "fifth"} {
		if ordinal == at {
			return name
		}
	}
	return fmt.Sprintf("%dth", at+1)
}

// rfc9420SenderSelectPayloads is the Sender select of RFC 9420 section 6, read as the number of
// octets each arm carries AFTER the sender_type:
//
//	case member:              uint32 leaf_index      4
//	case external:            uint32 sender_index    4
//	case new_member_commit:                          0
//	case new_member_proposal:                        0
//
// Keyed by the RFC's name and joined against what this package's encoder actually emits, so a
// sender type standing at an arm's code point while carrying a different payload fails here.
var rfc9420SenderSelectPayloads = map[string]int{
	"member":              4,
	"external":            4,
	"new_member_proposal": 0,
	"new_member_commit":   0,
}

// TestEverySenderArmCarriesThePayloadSection6GivesIt measures the payload off the encoder
// rather than reading it out of framing.go, so what it states is a fact about behaviour.
func TestEverySenderArmCarriesThePayloadSection6GivesIt(t *testing.T) {
	derived := registryConstantsOfType(t, "SenderType")
	measured := map[string]int{}
	for _, name := range slices.Sorted(maps.Keys(derived)) {
		encoded, err := syntax.Marshal(testSenderOfType(SenderType(derived[name])))
		if err != nil {
			t.Fatalf("%s: Marshal: %v", name, err)
		}
		if len(encoded) < 1 {
			t.Fatalf("%s: encoded to nothing, so it wrote no discriminant", name)
		}
		if uint64(encoded[0]) != derived[name] {
			t.Errorf("%s: the encoding leads with %#02x and the constant is %#02x",
				name, encoded[0], uint8(derived[name]))
		}
		measured[rfcNameOfFramingConstant("SenderType", name)] = len(encoded) - 1
	}
	if !maps.Equal(measured, rfc9420SenderSelectPayloads) {
		t.Errorf("the Sender encoder writes payloads\n %v\nand RFC 9420 section 6's select gives\n %v",
			measured, rfc9420SenderSelectPayloads)
	}
}

// ---------------------------------------------------------------------------
// the value every golden and every sweep below is built from
// ---------------------------------------------------------------------------

// testSenderOfType is one Sender with BOTH variant fields populated, and populated
// DIFFERENTLY.
//
// Both halves of that matter. Two uint32 fields holding the same octets are encoded identically
// by a codec that wrote one of them twice and by one that swapped them, so the goldens would
// pin nothing -- and LeafIndex and SenderIndex are adjacent fields of the same width, which is
// the one shape five plans of codecs on this project have found no round trip property can see.
// Populating both under EVERY sender type is what the "encoded under the wrong arm" mutation
// needs as well: under new_member_proposal this value carries a leaf index and a sender index
// that must not appear in its encoding at all.
func testSenderOfType(senderType SenderType) *Sender {
	return &Sender{SenderType: senderType, LeafIndex: 0x11223344, SenderIndex: 0x55667788}
}

// senderTypes is the class every sweep below runs over, derived off the type through the
// package's own type checker rather than listed. A fifth sender type declared and left out of a
// hand written list is a sender type nothing here judges.
func senderTypes(t *testing.T) []SenderType {
	t.Helper()
	derived := registryConstantsOfType(t, "SenderType")
	found := []SenderType{}
	for _, value := range derived {
		found = append(found, SenderType(value))
	}
	slices.Sort(found)
	return found
}

// senderVariantPaths is the RFC 9420 section 6 select, written as the field each arm carries.
// The discriminant is not in it: it is written under every arm and belongs to none of them.
var senderVariantPaths = map[SenderType][]string{
	SenderTypeMember:            {"LeafIndex"},
	SenderTypeExternal:          {"SenderIndex"},
	SenderTypeNewMemberProposal: {},
	SenderTypeNewMemberCommit:   {},
}

// senderDiscriminantField is the one field of Sender that is not a variant arm.
const senderDiscriminantField = "SenderType"

// TestTheSenderVariantTableCoversTheTypeAndTheRegistry holds the table above to the two things
// it is a claim about: the declared sender types, and the fields Sender actually has.
//
// Without this the table is a list, and a list is the exemption shape this project keeps
// rediscovering -- a fifth sender type, or a fourth field, silently outside every sweep that
// runs off it.
func TestTheSenderVariantTableCoversTheTypeAndTheRegistry(t *testing.T) {
	declared := senderTypes(t)
	tabled := []SenderType{}
	for senderType := range senderVariantPaths {
		tabled = append(tabled, senderType)
	}
	slices.Sort(tabled)
	if !slices.Equal(declared, tabled) {
		t.Fatalf("the package declares the sender types %v and the variant table covers %v", declared, tabled)
	}
	structType := reflect.TypeOf(Sender{})
	claimed := map[string]int{}
	for _, paths := range senderVariantPaths {
		for _, path := range paths {
			claimed[path] += 1
		}
	}
	for i := 0; i < structType.NumField(); i += 1 {
		name := structType.Field(i).Name
		if name == senderDiscriminantField {
			if claimed[name] != 0 {
				t.Errorf("%s is the discriminant and the variant table lists it as an arm's payload", name)
			}
			delete(claimed, name)
			continue
		}
		switch claimed[name] {
		case 1:
		case 0:
			t.Errorf("Sender.%s is carried by no arm of the variant table, so every sweep run off that table skips it",
				name)
		default:
			t.Errorf("Sender.%s is carried by %d arms of the variant table, and the RFC's select gives each field one",
				name, claimed[name])
		}
		delete(claimed, name)
	}
	for name := range claimed {
		t.Errorf("the variant table names %s and Sender has no such field", name)
	}
}

// ---------------------------------------------------------------------------
// the hand derived goldens
// ---------------------------------------------------------------------------

// handDerivedSenderGolden is one Sender's encoding written out from RFC 9420 section 6 rather
// than read back through the encoder:
//
//	sender_type            uint8                                          1
//	select (member):       uint32 leaf_index    -> 11223344               4
//	select (external):     uint32 sender_index  -> 55667788               4
//	select (new_member_proposal), (new_member_commit):  nothing           0
//
//	member               1 + 4 = 5
//	external             1 + 4 = 5
//	new_member_proposal  1 + 0 = 1
//	new_member_commit    1 + 0 = 1
//
// This is the one test in this file that a symmetric edit cannot survive. Two adjacent
// same-width fields swapped in BOTH halves of the codec round trip perfectly, re-encode byte
// exact, and are invisible to every other property here; so is a field dropped from both
// halves. What separates them is a statement of the encoding written without reference to the
// code, which is what this is.
//
// The octet beside each case name is also what makes an enum transposition visible at the
// codec: the switch is over the CONSTANT and the literal is the RFC's number for that name, so
// two sender types whose values were swapped encode to each other's goldens and fail here.
func handDerivedSenderGolden(senderType SenderType) []byte {
	switch senderType {
	case SenderTypeMember:
		return []byte{0x01, 0x11, 0x22, 0x33, 0x44}
	case SenderTypeExternal:
		return []byte{0x02, 0x55, 0x66, 0x77, 0x88}
	case SenderTypeNewMemberProposal:
		return []byte{0x03}
	case SenderTypeNewMemberCommit:
		return []byte{0x04}
	}
	return nil
}

// handDerivedSenderSizes is the arithmetic in the comment above, stated separately so a
// derivation edited without its comment fails rather than redefining what it is compared to.
var handDerivedSenderSizes = map[SenderType]int{
	SenderTypeMember:            5,
	SenderTypeExternal:          5,
	SenderTypeNewMemberProposal: 1,
	SenderTypeNewMemberCommit:   1,
}

// decodedFormOfSender is what a decode of this sender must produce: the same value with the
// arms the sender type does not carry left at their zero. Derived from the variant table the
// test above holds to the type, so a decoder that filled a field its arm does not carry fails
// without anybody writing a case for it.
func decodedFormOfSender(t *testing.T, sender *Sender) *Sender {
	t.Helper()
	out := *sender
	value := reflect.ValueOf(&out).Elem()
	for senderType, paths := range senderVariantPaths {
		if senderType == sender.SenderType {
			continue
		}
		for _, path := range paths {
			field := value.FieldByName(path)
			if !field.IsValid() {
				t.Fatalf("the variant table names %s and Sender has no such field", path)
			}
			field.Set(reflect.Zero(field.Type()))
		}
	}
	return &out
}

func sameSender(a *Sender, b *Sender) bool {
	return reflect.DeepEqual(a, b)
}

func describeSender(sender *Sender) string {
	return fmt.Sprintf("sender_type=%d leaf_index=%#08x sender_index=%#08x",
		sender.SenderType, uint32(sender.LeafIndex), sender.SenderIndex)
}

// TestSenderMarshalMatchesTheHandDerivedGoldens is the field order and width pin.
func TestSenderMarshalMatchesTheHandDerivedGoldens(t *testing.T) {
	for _, senderType := range senderTypes(t) {
		want := handDerivedSenderGolden(senderType)
		if want == nil {
			t.Fatalf("sender type %d has no hand derived golden, so nothing states its encoding", senderType)
		}
		size, stated := handDerivedSenderSizes[senderType]
		if !stated {
			t.Fatalf("sender type %d has no hand derived size, so its derivation is compared only against itself",
				senderType)
		}
		if len(want) != size {
			t.Fatalf("sender type %d: the hand derivation is %d octets and the arithmetic in its comment says %d",
				senderType, len(want), size)
		}
		encoded, err := syntax.Marshal(testSenderOfType(senderType))
		if err != nil {
			t.Fatalf("sender type %d: Marshal: %v", senderType, err)
		}
		if !bytes.Equal(encoded, want) {
			t.Errorf("sender type %d: Marshal =\n %x\nwant\n %x", senderType, encoded, want)
		}
		decoded := &Sender{}
		if err := syntax.Unmarshal(want, decoded); err != nil {
			t.Fatalf("sender type %d: Unmarshal the golden: %v", senderType, err)
		}
		if expected := decodedFormOfSender(t, testSenderOfType(senderType)); !sameSender(decoded, expected) {
			t.Errorf("sender type %d: the golden decoded to\n %s\nwant\n %s",
				senderType, describeSender(decoded), describeSender(expected))
		}
	}
}

// TestSenderRoundTripEveryType compares the WHOLE decoded value against the original rather
// than the two fields a hand written case would have listed.
//
// The comparison is against the derived expected form and it runs over the derived class, so an
// arm added without a case is swept and a field left standing under an arm that does not carry
// it fails. The whole-value half is what a field dropped from BOTH halves of the codec needs:
// that drop re-encodes byte exact against itself and is simply lost.
func TestSenderRoundTripEveryType(t *testing.T) {
	for _, senderType := range senderTypes(t) {
		in := testSenderOfType(senderType)
		encoded, err := syntax.Marshal(in)
		if err != nil {
			t.Fatalf("sender type %d: Marshal: %v", senderType, err)
		}
		out := &Sender{}
		if err := syntax.Unmarshal(encoded, out); err != nil {
			t.Fatalf("sender type %d: Unmarshal: %v", senderType, err)
		}
		if want := decodedFormOfSender(t, testSenderOfType(senderType)); !sameSender(out, want) {
			t.Errorf("sender type %d: round trip gave\n %s\nwant\n %s",
				senderType, describeSender(out), describeSender(want))
		}
		reencoded, err := syntax.Marshal(out)
		if err != nil {
			t.Fatalf("sender type %d: re-Marshal: %v", senderType, err)
		}
		if !bytes.Equal(reencoded, encoded) {
			t.Errorf("sender type %d: re-encode =\n %x\nwant\n %x", senderType, reencoded, encoded)
		}
	}
}

// TestTheSenderCodecWritesEveryFieldItsArmCarriesAndNoOther sweeps the fields off the TYPE and
// the carriage off the variant table, so neither is a list somebody kept up to date.
//
// This is the property a golden cannot state on its own: the golden says what the bytes are for
// one value, and this says that each field is what those bytes are a function of. A field
// dropped from the encoder is caught by both; a field written under an arm that does not carry
// it is caught here for every arm at once.
func TestTheSenderCodecWritesEveryFieldItsArmCarriesAndNoOther(t *testing.T) {
	structType := reflect.TypeOf(Sender{})
	observed := map[string]bool{}
	// the complement of the sweep, printed rather than left to be read out of the constant. It
	// is not skipped work: the discriminant is held to the goldens at the end of this test.
	removed := []string{}
	for i := 0; i < structType.NumField(); i += 1 {
		if name := structType.Field(i).Name; name == senderDiscriminantField {
			removed = append(removed, name)
		}
	}
	t.Logf("%d field(s) of Sender removed from the variation sweep because they are the discriminant, which the golden assertion at the end of this test covers instead: %v",
		len(removed), removed)
	for _, senderType := range senderTypes(t) {
		base, err := syntax.Marshal(testSenderOfType(senderType))
		if err != nil {
			t.Fatalf("sender type %d: Marshal: %v", senderType, err)
		}
		for i := 0; i < structType.NumField(); i += 1 {
			name := structType.Field(i).Name
			if name == senderDiscriminantField {
				continue
			}
			varied := testSenderOfType(senderType)
			field := reflect.ValueOf(varied).Elem().Field(i)
			field.SetUint(field.Uint() ^ 0x0a0b0c0d)
			encoded, err := syntax.Marshal(varied)
			if err != nil {
				t.Fatalf("sender type %d: Marshal with %s varied: %v", senderType, name, err)
			}
			changed := !bytes.Equal(encoded, base)
			carried := slices.Contains(senderVariantPaths[senderType], name)
			if carried && !changed {
				t.Errorf("sender type %d carries %s and varying it left the encoding at %x, so nothing writes it",
					senderType, name, encoded)
			}
			if !carried && changed {
				t.Errorf("sender type %d does not carry %s and varying it changed the encoding to %x",
					senderType, name, encoded)
			}
			observed[name] = observed[name] || changed
		}
	}
	for i := 0; i < structType.NumField(); i += 1 {
		name := structType.Field(i).Name
		if name == senderDiscriminantField {
			continue
		}
		if !observed[name] {
			t.Errorf("%s changed no encoding under any declared sender type, so nothing in this package writes it",
				name)
		}
	}
	// the discriminant is covered by the goldens rather than by the sweep, and that has to be
	// true rather than assumed: every golden leads with a different octet.
	leading := map[byte]SenderType{}
	for _, senderType := range senderTypes(t) {
		golden := handDerivedSenderGolden(senderType)
		if len(golden) == 0 {
			t.Fatalf("sender type %d has no golden", senderType)
		}
		if other, clash := leading[golden[0]]; clash {
			t.Errorf("sender types %d and %d both lead with %#02x, so the discriminant is not what separates them",
				other, senderType, golden[0])
		}
		leading[golden[0]] = senderType
	}
}

// ---------------------------------------------------------------------------
// the refusals
// ---------------------------------------------------------------------------

// undeclaredSenderTypeOctets is every octet the SenderType registry does not name, derived over
// the width of the type against the declared set rather than over the three values a hand
// written case would have probed. The reserved zero is one of them and is not special: it is
// undeclared, and so is 0x05, and so is 0xff.
func undeclaredSenderTypeOctets(t *testing.T) []byte {
	t.Helper()
	declared := senderTypes(t)
	found := []byte{}
	for candidate := 0; candidate <= 0xff; candidate += 1 {
		if slices.Contains(declared, SenderType(candidate)) {
			continue
		}
		found = append(found, byte(candidate))
	}
	if len(found)+len(declared) != int(^SenderType(0))+1 {
		t.Fatalf("the derivation split %d code points into %d declared and %d undeclared",
			int(^SenderType(0))+1, len(declared), len(found))
	}
	return found
}

// TestSenderRejectsReservedAndUnknownType sweeps the whole octet space rather than the three
// values the plan probed, and states the refusal on BOTH halves of the codec.
//
// The encode half is not decoration. The sender type is inside every signature preimage this
// layer builds, so an encoder that wrote an unregistered one would produce signed bytes no peer
// can attribute to anybody; and an encoder whose default arm fell through to writing the octet
// is exactly the shape that survives a decode-only test.
//
// Each undeclared octet is offered twice, bare and with a four octet tail, so a refusal that was
// really a truncation cannot pass for a refusal of the TYPE: the tail case has bytes to spare
// and must still name ErrUnknownSenderType.
func TestSenderRejectsReservedAndUnknownType(t *testing.T) {
	undeclared := undeclaredSenderTypeOctets(t)
	if !slices.Contains(undeclared, 0x00) {
		t.Fatal("0 is declared as a sender type, and RFC 9420 section 6 reserves it")
	}
	for _, octet := range undeclared {
		for _, encoded := range [][]byte{{octet}, {octet, 0xde, 0xad, 0xbe, 0xef}} {
			decoded := &Sender{}
			err := syntax.Unmarshal(encoded, decoded)
			if !errors.Is(err, ErrUnknownSenderType) {
				t.Fatalf("decoding %x: got %v, want ErrUnknownSenderType", encoded, err)
			}
		}
		unencodable := &Sender{SenderType: SenderType(octet), LeafIndex: 1, SenderIndex: 2}
		if _, err := syntax.Marshal(unencodable); !errors.Is(err, ErrUnknownSenderType) {
			t.Fatalf("encoding sender type %#02x: got %v, want ErrUnknownSenderType", octet, err)
		}
	}
	t.Logf("%d undeclared sender type octets refused on both halves of the codec", len(undeclared))
}

// TestSenderRejectsATruncatedArm states the other refusal the decoder can make, over every
// proper prefix of every golden rather than over the boundaries somebody thought of.
func TestSenderRejectsATruncatedArm(t *testing.T) {
	cuts := 0
	for _, senderType := range senderTypes(t) {
		golden := handDerivedSenderGolden(senderType)
		for cut := 0; cut < len(golden); cut += 1 {
			decoded := &Sender{}
			if err := syntax.Unmarshal(golden[:cut], decoded); err == nil {
				t.Errorf("sender type %d: %d of %d octets decoded rather than being refused",
					senderType, cut, len(golden))
				continue
			}
			cuts += 1
		}
	}
	if cuts == 0 {
		t.Fatal("no truncation was built, so this observed nothing")
	}
	t.Logf("%d truncations refused", cuts)
}

// TestSenderRefusesTrailingBytes is the full consumption half, and Sender was the one codec
// landed in this package with no statement of it: LeafNode, GroupContext, Extension,
// Capabilities and RequiredCapabilities each have one and this file had none.
//
// Neither refusal already here says it. TestSenderRejectsATruncatedArm runs over proper
// PREFIXES, which is the other side of the length; and the four octet tail in
// TestSenderRejectsReservedAndUnknownType is offered only under UNDECLARED discriminants, where
// the decoder's own default arm answers before full consumption is ever reached. What was left
// unstated is the case with teeth: a VALID sender followed by a stray octet. The sender type
// and its arm are inside every signature preimage this layer builds, so a decoder tolerating a
// tail accepts two encodings of one sender while a signature covers only one of them.
//
// The refusal comes from syntax.UnmarshalLimit joining r.Done() rather than from anything in
// framing.go, which is exactly why it is worth stating here: dropping that join left this
// file's seventeen tests green and failed four OTHER codecs' -- so this layer was being carried
// by tests that name somebody else's type.
func TestSenderRefusesTrailingBytes(t *testing.T) {
	stated := 0
	for _, senderType := range senderTypes(t) {
		golden := handDerivedSenderGolden(senderType)
		for _, tail := range [][]byte{{0x00}, {0xff}, {0x00, 0x00}, repeatByte(0x5a, 17)} {
			longer := joinBytes(golden, tail)
			decoded := &Sender{}
			if err := syntax.Unmarshal(longer, decoded); !errors.Is(err, syntax.ErrTrailingBytes) {
				t.Errorf("sender type %d with %d trailing octets (%x): err = %v, want syntax.ErrTrailingBytes",
					senderType, len(tail), longer, err)
				continue
			}
			stated += 1
		}
	}
	if stated == 0 {
		t.Fatal("no tailed encoding was refused, so this observed nothing")
	}
	t.Logf("%d tailed encodings refused over %d sender types", stated, len(senderTypes(t)))
}

// senderPriorContents is every state a receiver can arrive in that this file can build: the
// zero sender, and one of every declared type. Derived over the class rather than over the one
// prior somebody picks, because the failure is per PAIR -- only a receiver that already held an
// arm's payload can leave that payload standing under a type that does not carry it.
func senderPriorContents(t *testing.T) []*Sender {
	t.Helper()
	priors := []*Sender{{}}
	for _, senderType := range senderTypes(t) {
		priors = append(priors, testSenderOfType(senderType))
	}
	return priors
}

// TestASenderDecodesToTheSameValueWhateverItsReceiverHeld is not a round trip property.
//
// The arms of the decoder assign only the field their own type carries, so a decoder that wrote
// through its receiver as it read leaves the PREVIOUS sender's LeafIndex standing under
// external, or its SenderIndex standing under member. The bytes are unaffected -- the stale
// field is not written under the new arm -- so it round trips, re-encodes byte exact and agrees
// with every golden here. What it disagrees with is the same bytes decoded into a fresh
// receiver, which is a sender comparing unequal to itself depending on where it was decoded.
func TestASenderDecodesToTheSameValueWhateverItsReceiverHeld(t *testing.T) {
	priors := senderPriorContents(t)
	for _, senderType := range senderTypes(t) {
		encoded := handDerivedSenderGolden(senderType)
		fresh := &Sender{}
		if err := syntax.Unmarshal(encoded, fresh); err != nil {
			t.Fatalf("sender type %d: Unmarshal into a fresh receiver: %v", senderType, err)
		}
		for at, prior := range priors {
			reused := *prior
			if err := syntax.Unmarshal(encoded, &reused); err != nil {
				t.Fatalf("sender type %d: Unmarshal into a receiver holding prior %d: %v", senderType, at, err)
			}
			if !sameSender(&reused, fresh) {
				t.Errorf("sender type %d: decoding into a receiver that held\n %s\ngave\n %s\nand the same bytes decoded fresh give\n %s",
					senderType, describeSender(prior), describeSender(&reused), describeSender(fresh))
			}
		}
	}
	t.Logf("%d prior receiver contents over %d sender types", len(priors), len(senderTypes(t)))
}

// senderRefusedEncodingsOf is every input this file can build that a decode must refuse: every
// proper prefix of a golden, and every golden under every octet the registry does not name.
func senderRefusedEncodingsOf(t *testing.T, senderType SenderType) [][]byte {
	t.Helper()
	golden := handDerivedSenderGolden(senderType)
	inputs := [][]byte{}
	for cut := 0; cut < len(golden); cut += 1 {
		inputs = append(inputs, golden[:cut])
	}
	for _, octet := range undeclaredSenderTypeOctets(t) {
		altered := bytes.Clone(golden)
		altered[0] = octet
		inputs = append(inputs, altered)
	}
	return inputs
}

// TestARefusedSenderDecodeLeavesItsReceiverUntouched is the discipline Credential and LeafNode
// already keep, stated for the third decoder of this package so the three do not disagree about
// it with only two of them tested.
//
// A decoder that wrote its fields as it read them would leave a receiver holding a sender type
// out of a message this package REFUSED -- a value that never existed anywhere, assembled out of
// the first octet of somebody else's bytes, sitting in a variable the caller may well reuse,
// with nothing in the returned error saying so.
func TestARefusedSenderDecodeLeavesItsReceiverUntouched(t *testing.T) {
	priors := senderPriorContents(t)
	refused, accepted := 0, 0
	for _, senderType := range senderTypes(t) {
		for _, input := range senderRefusedEncodingsOf(t, senderType) {
			for _, prior := range priors {
				held := *prior
				before := *prior
				if err := syntax.Unmarshal(input, &held); err == nil {
					accepted += 1
					continue
				}
				refused += 1
				if sameSender(&held, &before) {
					continue
				}
				t.Errorf("sender type %d: a refused decode of %x into a receiver that held\n %s\nwrote through it, leaving\n %s",
					senderType, input, describeSender(&before), describeSender(&held))
				return
			}
		}
	}
	if accepted != 0 {
		t.Errorf("%d of the inputs this sweep built decoded rather than being refused, so the property was never stated over them",
			accepted)
	}
	if refused == 0 {
		t.Fatal("no input was refused, so this observed nothing")
	}
	t.Logf("%d refused decodes over %d prior receiver contents left the receiver exactly as they found it",
		refused, len(priors))
}

// ---------------------------------------------------------------------------
// the structural framing errors
// ---------------------------------------------------------------------------

// framingErrorsFile is the single file this plan declares its structural errors in. Every gate
// below derives its class from that file rather than from the list, which is the difference
// between sweeping the class and sweeping a copy of it.
const framingErrorsFile = "framing_errors.go"

// framingOwnedErrors is every name framing_errors.go declares, keyed by that name so the
// derivation below can compare the two sets. Nothing here is trusted:
// TestFramingOwnedErrorsIsEveryDeclarationOfItsFile holds it to what the file actually declares,
// in both directions.
var framingOwnedErrors = map[string]error{
	"ErrUnknownWireFormat":        ErrUnknownWireFormat,
	"ErrUnsupportedVersion":       ErrUnsupportedVersion,
	"ErrUnknownSenderType":        ErrUnknownSenderType,
	"ErrContentArmMismatch":       ErrContentArmMismatch,
	"ErrMissingGroupContext":      ErrMissingGroupContext,
	"ErrUnexpectedGroupContext":   ErrUnexpectedGroupContext,
	"ErrWireFormatMismatch":       ErrWireFormatMismatch,
	"ErrSenderNotMember":          ErrSenderNotMember,
	"ErrInvalidPaddingSize":       ErrInvalidPaddingSize,
	"ErrUnknownProposalOrRefType": ErrUnknownProposalOrRefType,
}

// TestFramingOwnedErrorsIsEveryDeclarationOfItsFile derives the class the sweeps below run over
// instead of trusting the transcription of it. An eleventh error declared in that file and left
// off the list is judged by neither sweep, and a count assertion cannot close that because it
// has the wrong polarity: it fails on adding an error and REMEMBERING the list.
func TestFramingOwnedErrorsIsEveryDeclarationOfItsFile(t *testing.T) {
	declared := packageLevelDeclarations(t, ".")
	fromFile := map[string]bool{}
	for name, file := range declared {
		if file == framingErrorsFile {
			fromFile[name] = true
		}
	}
	if len(fromFile) == 0 {
		t.Fatalf("the scan found nothing declared in %s, so this gate compared the list against an empty set",
			framingErrorsFile)
	}
	if !fromFile["ErrUnknownSenderType"] {
		t.Fatalf("the scan did not find ErrUnknownSenderType among the declarations of %s, which certainly declares it, so it is reading something other than that file",
			framingErrorsFile)
	}
	for _, name := range slices.Sorted(maps.Keys(fromFile)) {
		if _, listed := framingOwnedErrors[name]; !listed {
			t.Errorf("%s declares %s and framingOwnedErrors does not list it, so no sweep below judges it; add it there",
				framingErrorsFile, name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(framingOwnedErrors)) {
		if !fromFile[name] {
			t.Errorf("framingOwnedErrors lists %s and %s does not declare it, so the sweeps run over a name this file does not own",
				name, framingErrorsFile)
		}
	}
}

// TestFramingErrorsAreDistinctAndNamed asserts no two of the structural errors alias each other,
// so a caller branching on one cannot be answered yes by another, and that each names the
// package it came from so a caller logging one at a distance can tell.
//
// The count pins the file: a name added to it is a change somebody makes deliberately, in the
// same commit, with a reason. What stops the list shrinking, and what stops it lagging behind
// the file, is the derivation above rather than this number.
//
// Ten, where the interface registry's section 7.6 block names ten structural errors plus the
// p6-private ErrUnknownProposalOrRefType -- eleven. The one that is not here is
// ErrUnknownContentType, which already existed in errors_key_schedule.go before this file did,
// and the test below is where that is held rather than papered over.
func TestFramingErrorsAreDistinctAndNamed(t *testing.T) {
	if len(framingOwnedErrors) != 10 {
		t.Fatalf("this file owns %d errors, want the nine structural ones of registry section 7.6 that were not already declared elsewhere plus the p6-private ErrUnknownProposalOrRefType; if one landed or left, say so here in the same commit",
			len(framingOwnedErrors))
	}
	names := slices.Sorted(maps.Keys(framingOwnedErrors))
	for i, name := range names {
		first := framingOwnedErrors[name]
		if first == nil {
			t.Fatalf("%s is nil", name)
		}
		if first.Error() == "" {
			t.Fatalf("%s has an empty message", name)
		}
		if !strings.HasPrefix(first.Error(), "mls: ") {
			t.Errorf("%s reads %q; every typed error of this package names the package it came from",
				name, first.Error())
		}
		for j, other := range names {
			if i == j {
				continue
			}
			second := framingOwnedErrors[other]
			if errors.Is(first, second) {
				t.Errorf("%s answers to %s (%v), so a caller branching on the two reads one as the other",
					name, other, first)
			}
			if first.Error() == second.Error() {
				t.Errorf("%s and %s both read %q, so the two are indistinguishable in a log",
					name, other, first.Error())
			}
		}
	}
}

// theTenStructuralFramingErrors is registry section 7.6's block, by name, including the one name
// this plan's own file does not declare.
var theTenStructuralFramingErrors = []string{
	"ErrUnknownWireFormat", "ErrUnsupportedVersion", "ErrUnknownContentType", "ErrUnknownSenderType",
	"ErrContentArmMismatch", "ErrMissingGroupContext", "ErrUnexpectedGroupContext",
	"ErrWireFormatMismatch", "ErrSenderNotMember", "ErrInvalidPaddingSize",
}

// TestEveryStructuralFramingErrorHasExactlyOneDeclarationSite is where this task's one deviation
// from its own plan is held.
//
// The plan says framing_errors.go declares all ten. Nine of them it does. The tenth,
// ErrUnknownContentType, was already declared -- in errors_key_schedule.go, by the plan that
// owns the secret tree, because the secret tree is what refuses a content type with no ratchet
// behind it -- and "a content type this package does not register" is ONE condition whether it
// is reached off the wire at the codec or off a header at the ratchet lookup. Two sentinels for
// one condition is precisely what every error roster in this package exists to prevent, so the
// framing layer consumes that declaration rather than shadowing it.
//
// This test is what makes that a decision rather than an accident: each of the ten resolves,
// each has exactly one declaration site, and ErrUnknownContentType's is named. A later framing
// task that adds a second declaration of it fails here instead of quietly splitting the
// condition in two, which is the failure the deviation was taken to avoid.
func TestEveryStructuralFramingErrorHasExactlyOneDeclarationSite(t *testing.T) {
	declared := packageLevelDeclarations(t, ".")
	for _, name := range theTenStructuralFramingErrors {
		file, found := declared[name]
		if !found {
			t.Errorf("registry section 7.6 names %s and this package declares no such error", name)
			continue
		}
		want := framingErrorsFile
		if name == "ErrUnknownContentType" {
			want = "errors_key_schedule.go"
		}
		if file != want {
			t.Errorf("%s is declared in %s, and the single declaration site for it is %s", name, file, want)
		}
	}
	// the sentinel this plan consumes must still be a value of its own against the nine it
	// declares, which is the property the cross-file split could quietly lose.
	for _, name := range slices.Sorted(maps.Keys(framingOwnedErrors)) {
		if errors.Is(framingOwnedErrors[name], ErrUnknownContentType) ||
			errors.Is(ErrUnknownContentType, framingOwnedErrors[name]) {
			t.Errorf("%s and ErrUnknownContentType answer for each other", name)
		}
	}
}

// ---------------------------------------------------------------------------
// FramedContentAuthData
// ---------------------------------------------------------------------------

// contentTypes is the class every auth data sweep below runs over, derived off the type
// through the package's own type checker rather than listed.
//
// Derived for the reason senderTypes is: a fourth content type declared and left out of a hand
// written list is a content type nothing here judges. It matters more here than it does there,
// because the content type is not carried inside a FramedContentAuthData at all -- it is the
// parameter that decides whether a confirmation tag is read -- so an arm nothing sweeps is an
// arm whose tag handling nothing states.
func contentTypes(t *testing.T) []ContentType {
	t.Helper()
	derived := registryConstantsOfType(t, "ContentType")
	found := []ContentType{}
	for _, value := range derived {
		found = append(found, ContentType(value))
	}
	if len(found) == 0 {
		t.Fatal("no ContentType constant was derived, so every sweep below would run over the empty set")
	}
	slices.Sort(found)
	return found
}

// undeclaredContentTypeOctets is every octet the ContentType registry does not name, derived
// over the width of the type against the declared set. The reserved zero is one of them and is
// not special.
func undeclaredContentTypeOctets(t *testing.T) []ContentType {
	t.Helper()
	declared := contentTypes(t)
	found := []ContentType{}
	for candidate := 0; candidate <= 0xff; candidate += 1 {
		if slices.Contains(declared, ContentType(candidate)) {
			continue
		}
		found = append(found, ContentType(candidate))
	}
	if len(found)+len(declared) != int(^ContentType(0))+1 {
		t.Fatalf("the derivation split %d code points into %d declared and %d undeclared",
			int(^ContentType(0))+1, len(declared), len(found))
	}
	return found
}

// testAuthData is the value every sweep below encodes, populated in BOTH fields under EVERY
// content type.
//
// Both halves of that are load bearing. Signature and ConfirmationTag are the same Go type and
// adjacent, which is the shape no round trip property can see: swapped in both halves of the
// codec they round trip perfectly, re-encode byte exact and agree with every structural test
// here. What separates them is the hand derivation below, and it can only separate them because
// the two values have DIFFERENT LENGTHS -- three octets against two -- so a swap moves the
// length prefixes as well as the bodies.
//
// Populating the tag under application and proposal is what the "written under an arm that does
// not carry it" mutation needs: under those two content types this value holds a confirmation
// tag that must not reach the wire at all.
func testAuthData() *FramedContentAuthData {
	return &FramedContentAuthData{
		Signature:       []byte{0x11, 0x22, 0x33},
		ConfirmationTag: []byte{0x44, 0x55},
	}
}

// handDerivedAuthDataGolden is RFC 9420 section 6's FramedContentAuthData written from the wire
// format, not read back out of framing.go.
//
//	struct {
//	    opaque signature<V>;
//	    select (FramedContentAuthData.content_type) {
//	        case commit:
//	            MAC confirmation_tag;   /* opaque<V> */
//	        case application:
//	        case proposal:
//	    };
//	} FramedContentAuthData;
//
// The octet arithmetic, from the varint length prefix p1 implements: a length below 64 has
// prefix bits 00 and occupies one octet.
//
//	signature<V> over 11 22 33   length 3 -> 1 octet 0x03, body 3 octets = 4
//	confirmation_tag<V> over 44 55  length 2 -> 1 octet 0x02, body 2 octets = 3
//
//	application  4 + 0 = 4
//	proposal     4 + 0 = 4
//	commit       4 + 3 = 7
//
// This is the one statement here that a symmetric edit cannot survive, and this structure needs
// it more than Sender did: there is no discriminant octet in these bytes at all, so a codec
// that read the wrong arm produces no type error anywhere and the ONLY thing separating a
// commit's encoding from a proposal's is the three octets at the end.
func handDerivedAuthDataGolden(contentType ContentType) []byte {
	switch contentType {
	case ContentTypeApplication:
		return []byte{0x03, 0x11, 0x22, 0x33}
	case ContentTypeProposal:
		return []byte{0x03, 0x11, 0x22, 0x33}
	case ContentTypeCommit:
		return []byte{0x03, 0x11, 0x22, 0x33, 0x02, 0x44, 0x55}
	}
	return nil
}

// handDerivedAuthDataSizes is the arithmetic in the comment above, stated separately so a
// derivation edited without its comment fails rather than redefining what it is compared to.
var handDerivedAuthDataSizes = map[ContentType]int{
	ContentTypeApplication: 4,
	ContentTypeProposal:    4,
	ContentTypeCommit:      7,
}

// authDataVariantPaths is the section 6 select above, written as the fields each arm carries.
//
// Unlike senderVariantPaths this table has no discriminant field to exclude, and that absence
// is the whole design of the type: the content type is a parameter of the codec rather than a
// field of the struct, so there is nothing in these bytes that says which arm they are.
var authDataVariantPaths = map[ContentType][]string{
	ContentTypeApplication: {"Signature"},
	ContentTypeProposal:    {"Signature"},
	ContentTypeCommit:      {"Signature", "ConfirmationTag"},
}

// registrySection72AuthDataFields is the field list registry section 7.2 fixes, in wire order.
//
// A transcription rather than a derivation, deliberately, because it is a claim ABOUT the
// registry and there is nothing in this tree to derive it from. It is the one place the two
// fields the validation plan asked for and the registry REFUSED -- MembershipTag, which belongs
// to PublicMessage, and HasConfirmationTag, whose answer is derived from the content type -- can
// be kept out by something other than memory.
var registrySection72AuthDataFields = []string{"Signature", "ConfirmationTag"}

// decodedFormOfAuthData is what a decode of this value under this content type must produce:
// the same value with the fields this content type's arm does not carry left at their zero.
// Derived off the variant table the test below holds to the type, so a decoder that filled a
// field its arm does not carry fails without anybody writing a case for it.
func decodedFormOfAuthData(t *testing.T, auth *FramedContentAuthData, contentType ContentType) *FramedContentAuthData {
	t.Helper()
	out := *auth
	value := reflect.ValueOf(&out).Elem()
	carried := authDataVariantPaths[contentType]
	for i := 0; i < value.NumField(); i += 1 {
		name := value.Type().Field(i).Name
		if slices.Contains(carried, name) {
			continue
		}
		value.Field(i).Set(reflect.Zero(value.Field(i).Type()))
	}
	return &out
}

func sameAuthData(a *FramedContentAuthData, b *FramedContentAuthData) bool {
	return reflect.DeepEqual(a, b)
}

// describeAuthData separates a nil field from an empty one, because the two are different
// statements here -- an absent confirmation tag against one this codec refuses -- and %x spells
// both as the empty string.
func describeAuthData(auth *FramedContentAuthData) string {
	return fmt.Sprintf("signature=%s confirmation_tag=%s",
		describeOptionalOctets(auth.Signature), describeOptionalOctets(auth.ConfirmationTag))
}

func describeOptionalOctets(bs []byte) string {
	if bs == nil {
		return "nil"
	}
	return fmt.Sprintf("%x(len %d)", bs, len(bs))
}

// encodeAuthData is the whole encode: a fresh Writer, the codec, and Bytes.
func encodeAuthData(auth *FramedContentAuthData, contentType ContentType) ([]byte, error) {
	w := syntax.NewWriter()
	if err := auth.MarshalMLS(w, contentType); err != nil {
		return nil, err
	}
	return w.Bytes()
}

// decodeAuthData is the whole decode: the codec, and then the full consumption rule.
//
// Done() is part of acceptance rather than an extra assertion at some call sites, and that is
// the decision this structure turns on. A FramedContentAuthData is the tail of every structure
// that carries it, so a tail this codec leaves behind is a tail the enclosing decode refuses --
// and "accepted" has to mean the same thing in the wrong-content-type sweep as it does in the
// round trip, or that sweep would report a commit's confirmation tag silently dropped by a
// proposal decode as a success.
func decodeAuthData(bs []byte, contentType ContentType) (*FramedContentAuthData, error) {
	decoded := &FramedContentAuthData{}
	r := syntax.NewReader(bs)
	if err := decoded.UnmarshalMLS(r, contentType); err != nil {
		return nil, err
	}
	if err := r.Done(); err != nil {
		return nil, err
	}
	return decoded, nil
}

// TestTheAuthDataVariantTableCoversTheTypeAndTheRegistry holds the table to the two things it
// is a claim about: the declared content types, and the fields FramedContentAuthData has.
//
// Without this the table is a list, and a list is the exemption shape this project keeps
// rediscovering -- a fourth content type, or a third field, silently outside every sweep that
// runs off it. The union clause is the one that matters here: there is no discriminant field to
// carve out, so every field of this struct must be carried by some arm, and a field carried by
// none is a field nothing on the wire is a function of.
func TestTheAuthDataVariantTableCoversTheTypeAndTheRegistry(t *testing.T) {
	declared := contentTypes(t)
	tabled := []ContentType{}
	for contentType := range authDataVariantPaths {
		tabled = append(tabled, contentType)
	}
	slices.Sort(tabled)
	if !slices.Equal(declared, tabled) {
		t.Fatalf("the package declares the content types %v and the auth data variant table covers %v",
			declared, tabled)
	}
	structType := reflect.TypeOf(FramedContentAuthData{})
	fields := []string{}
	for i := 0; i < structType.NumField(); i += 1 {
		fields = append(fields, structType.Field(i).Name)
	}
	if !slices.Equal(fields, registrySection72AuthDataFields) {
		t.Fatalf("FramedContentAuthData has the fields %v and registry section 7.2 fixes %v; MembershipTag belongs to PublicMessage and HasConfirmationTag is derived from the content type, so neither may land here",
			fields, registrySection72AuthDataFields)
	}
	claimed := map[string]bool{}
	for contentType, paths := range authDataVariantPaths {
		for _, path := range paths {
			if !slices.Contains(fields, path) {
				t.Errorf("content type %d claims the field %s and FramedContentAuthData has no such field",
					contentType, path)
			}
			claimed[path] = true
		}
	}
	for _, name := range fields {
		if !claimed[name] {
			t.Errorf("%s is carried by no arm of the variant table, so nothing on the wire is a function of it",
				name)
		}
	}
	// the goldens are the other half of the table and are checked here rather than at each
	// call site, so an arm added to the table without one is not swept against nothing.
	for _, contentType := range declared {
		if handDerivedAuthDataGolden(contentType) == nil {
			t.Errorf("content type %d has no hand derived golden, so nothing states its encoding", contentType)
		}
		if _, stated := handDerivedAuthDataSizes[contentType]; !stated {
			t.Errorf("content type %d has no hand derived size, so its golden is compared only against itself",
				contentType)
		}
	}
	if len(handDerivedAuthDataSizes) != len(declared) {
		t.Errorf("the size table holds %d entries and the package declares %d content types",
			len(handDerivedAuthDataSizes), len(declared))
	}
}

// TestFramedContentAuthDataMarshalMatchesTheHandDerivedGoldens is the field order, the field
// width and the arm selection pin, all three at once.
func TestFramedContentAuthDataMarshalMatchesTheHandDerivedGoldens(t *testing.T) {
	for _, contentType := range contentTypes(t) {
		want := handDerivedAuthDataGolden(contentType)
		if want == nil {
			t.Fatalf("content type %d has no hand derived golden", contentType)
		}
		size, stated := handDerivedAuthDataSizes[contentType]
		if !stated {
			t.Fatalf("content type %d has no hand derived size", contentType)
		}
		if len(want) != size {
			t.Fatalf("content type %d: the hand derivation is %d octets and the arithmetic in its comment says %d",
				contentType, len(want), size)
		}
		encoded, err := encodeAuthData(testAuthData(), contentType)
		if err != nil {
			t.Fatalf("content type %d: marshal: %v", contentType, err)
		}
		if !bytes.Equal(encoded, want) {
			t.Errorf("content type %d: marshal =\n %x\nwant\n %x", contentType, encoded, want)
		}
		decoded, err := decodeAuthData(want, contentType)
		if err != nil {
			t.Fatalf("content type %d: decoding the golden: %v", contentType, err)
		}
		if expected := decodedFormOfAuthData(t, testAuthData(), contentType); !sameAuthData(decoded, expected) {
			t.Errorf("content type %d: the golden decoded to\n %s\nwant\n %s",
				contentType, describeAuthData(decoded), describeAuthData(expected))
		}
	}
}

// TestFramedContentAuthDataRoundTripsEveryContentType compares the WHOLE decoded value against
// the derived expected form rather than the one or two fields a hand written case would list,
// and then re-encodes.
//
// The whole-value half is what a field dropped from BOTH halves of the codec needs: that drop
// re-encodes byte exact against itself and is simply lost, and only a comparison that names no
// field can see it.
func TestFramedContentAuthDataRoundTripsEveryContentType(t *testing.T) {
	for _, contentType := range contentTypes(t) {
		encoded, err := encodeAuthData(testAuthData(), contentType)
		if err != nil {
			t.Fatalf("content type %d: marshal: %v", contentType, err)
		}
		decoded, err := decodeAuthData(encoded, contentType)
		if err != nil {
			t.Fatalf("content type %d: unmarshal: %v", contentType, err)
		}
		if want := decodedFormOfAuthData(t, testAuthData(), contentType); !sameAuthData(decoded, want) {
			t.Errorf("content type %d: round trip gave\n %s\nwant\n %s",
				contentType, describeAuthData(decoded), describeAuthData(want))
		}
		reencoded, err := encodeAuthData(decoded, contentType)
		if err != nil {
			t.Fatalf("content type %d: re-marshal: %v", contentType, err)
		}
		if !bytes.Equal(reencoded, encoded) {
			t.Errorf("content type %d: re-encode =\n %x\nwant\n %x", contentType, reencoded, encoded)
		}
	}
}

// TestTheAuthDataCodecWritesEveryFieldItsContentTypeCarriesAndNoOther sweeps the fields off the
// TYPE and the carriage off the variant table, so neither is a list somebody kept up to date.
//
// This is the property a golden cannot state on its own: the golden says what the bytes are for
// one value, and this says that each field is what those bytes are a function of. It is where
// two of this task's named mutations land for every arm at once -- a confirmation tag written
// under proposal, and a confirmation tag omitted under commit -- because both are exactly
// "varying this field changed the encoding when the arm says it should not", or the reverse.
func TestTheAuthDataCodecWritesEveryFieldItsContentTypeCarriesAndNoOther(t *testing.T) {
	structType := reflect.TypeOf(FramedContentAuthData{})
	replacement := []byte{0x7e, 0x7f, 0x80, 0x81}
	observed := map[string]bool{}
	for _, contentType := range contentTypes(t) {
		base, err := encodeAuthData(testAuthData(), contentType)
		if err != nil {
			t.Fatalf("content type %d: marshal: %v", contentType, err)
		}
		for i := 0; i < structType.NumField(); i += 1 {
			name := structType.Field(i).Name
			varied := testAuthData()
			field := reflect.ValueOf(varied).Elem().Field(i)
			if bytes.Equal(field.Bytes(), replacement) {
				t.Fatalf("%s already holds the replacement value, so varying it varies nothing", name)
			}
			field.SetBytes(bytes.Clone(replacement))
			encoded, err := encodeAuthData(varied, contentType)
			if err != nil {
				t.Fatalf("content type %d: marshal with %s varied: %v", contentType, name, err)
			}
			changed := !bytes.Equal(encoded, base)
			carried := slices.Contains(authDataVariantPaths[contentType], name)
			if carried && !changed {
				t.Errorf("content type %d carries %s and varying it left the encoding at %x, so nothing writes it",
					contentType, name, encoded)
			}
			if !carried && changed {
				t.Errorf("content type %d does not carry %s and varying it changed the encoding to %x, so this codec writes a field the select does not give that arm",
					contentType, name, encoded)
			}
			observed[name] = observed[name] || changed
		}
	}
	for i := 0; i < structType.NumField(); i += 1 {
		name := structType.Field(i).Name
		if !observed[name] {
			t.Errorf("%s changed no encoding under any declared content type, so nothing in this package writes it",
				name)
		}
	}
}

// TestTheAuthDataGoldensSeparateExactlyTheArmsThatDifferOnTheWire is the premise the
// wrong-content-type sweep below rests on, stated rather than assumed.
//
// Application and proposal are the SAME encoding of this structure -- section 6's select gives
// both of them the empty arm -- so no decoder can be asked to tell one from the other, and a
// sweep demanding that every wrong content type be refused would be demanding something false.
// Commit is the arm that differs. Deriving the partition off the hand written goldens rather
// than off framing.go is what keeps it a statement about the RFC: a codec that stopped writing
// the confirmation tag would make all three goldens equal, and this fails before the sweep
// below gets the chance to pass vacuously.
func TestTheAuthDataGoldensSeparateExactlyTheArmsThatDifferOnTheWire(t *testing.T) {
	declared := contentTypes(t)
	distinct := map[string][]ContentType{}
	for _, contentType := range declared {
		key := fmt.Sprintf("%x", handDerivedAuthDataGolden(contentType))
		distinct[key] = append(distinct[key], contentType)
	}
	if len(distinct) < 2 {
		t.Fatalf("all %d content types encode this value identically, so no wrong content type could ever be refused and the sweep below states nothing",
			len(declared))
	}
	if !bytes.Equal(handDerivedAuthDataGolden(ContentTypeApplication), handDerivedAuthDataGolden(ContentTypeProposal)) {
		t.Error("application and proposal are given different encodings here; RFC 9420 section 6 gives both of them the empty arm")
	}
	if bytes.Equal(handDerivedAuthDataGolden(ContentTypeCommit), handDerivedAuthDataGolden(ContentTypeProposal)) {
		t.Error("commit and proposal are given the same encoding here; the confirmation tag is what separates them")
	}
	t.Logf("%d content types over %d distinct encodings", len(declared), len(distinct))
}

// TestAnAuthDataUnderTheWrongContentTypeIsRefusedRatherThanMisParsed is this structure's
// signature failure mode, swept over every ordered pair.
//
// FramedContentAuthData carries no discriminant, so the content type handed to the decoder is
// the only thing that says which arm the bytes are. Two ways of getting that wrong are two
// different bugs and both are here. A commit's bytes read as a proposal must NOT quietly stop
// after the signature and report success -- that is a confirmation tag dropped on the floor,
// which is a commit accepted with its binding to the epoch unread. A proposal's bytes read as a
// commit must not manufacture a tag out of whatever follows.
//
// The expectation is derived from the goldens rather than listed: where two content types share
// an encoding the decode must SUCCEED and give the same value, and where they do not it must be
// refused. A test that demanded refusal everywhere would be false for application against
// proposal; a test that demanded it nowhere would be this bug.
func TestAnAuthDataUnderTheWrongContentTypeIsRefusedRatherThanMisParsed(t *testing.T) {
	declared := contentTypes(t)
	refused, shared := 0, 0
	for _, encodedUnder := range declared {
		golden := handDerivedAuthDataGolden(encodedUnder)
		for _, decodedUnder := range declared {
			if encodedUnder == decodedUnder {
				continue
			}
			decoded, err := decodeAuthData(golden, decodedUnder)
			sameEncoding := bytes.Equal(golden, handDerivedAuthDataGolden(decodedUnder))
			if !sameEncoding {
				if err == nil {
					t.Errorf("the content type %d encoding %x decoded under content type %d as\n %s\nand the two arms do not share an encoding, so it must be refused",
						encodedUnder, golden, decodedUnder, describeAuthData(decoded))
					continue
				}
				refused += 1
				continue
			}
			if err != nil {
				t.Errorf("content types %d and %d share one encoding and decoding %x under %d was refused: %v",
					encodedUnder, decodedUnder, golden, decodedUnder, err)
				continue
			}
			want, wantErr := decodeAuthData(golden, encodedUnder)
			if wantErr != nil {
				t.Fatalf("content type %d could not decode its own golden: %v", encodedUnder, wantErr)
			}
			if !sameAuthData(decoded, want) {
				t.Errorf("content types %d and %d share one encoding and it decoded to\n %s\nunder one and\n %s\nunder the other",
					encodedUnder, decodedUnder, describeAuthData(want), describeAuthData(decoded))
			}
			shared += 1
		}
	}
	if refused == 0 {
		t.Fatal("no cross content type decode was refused, so this observed nothing")
	}
	t.Logf("%d cross content type decodes refused, %d passed as arms sharing one encoding", refused, shared)
}

// TestANonCommitAuthDataDecodeNeverProducesAConfirmationTag is the half of the sweep above that
// survives even where the arms share an encoding.
//
// Stated separately because it is the property with the sharpest failure: a decoder that read a
// tag whenever bytes happened to remain would be caught above only by the r.Done() clause, and a
// later task that gave this codec a bounded sub-Reader -- which is what an enclosing structure
// with fields after the auth data would need -- would remove that clause's teeth without
// touching this file. The value is what is asserted here, not the tail.
func TestANonCommitAuthDataDecodeNeverProducesAConfirmationTag(t *testing.T) {
	inputs := [][]byte{}
	for _, contentType := range contentTypes(t) {
		inputs = append(inputs, handDerivedAuthDataGolden(contentType))
	}
	inputs = append(inputs, joinBytes(handDerivedAuthDataGolden(ContentTypeCommit), repeatByte(0x5a, 9)))
	stated := 0
	for _, contentType := range contentTypes(t) {
		if slices.Contains(authDataVariantPaths[contentType], "ConfirmationTag") {
			continue
		}
		for _, input := range inputs {
			decoded := &FramedContentAuthData{ConfirmationTag: []byte{0xde, 0xad}}
			r := syntax.NewReader(input)
			if err := decoded.UnmarshalMLS(r, contentType); err != nil {
				continue
			}
			if decoded.ConfirmationTag != nil {
				t.Errorf("content type %d decoded %x and produced the confirmation tag %x; that arm carries none",
					contentType, input, decoded.ConfirmationTag)
			}
			stated += 1
		}
	}
	if stated == 0 {
		t.Fatal("no non-commit decode succeeded, so this observed nothing")
	}
	t.Logf("%d non-commit decodes left the confirmation tag absent", stated)
}

// TestFramedContentAuthDataRequiresAConfirmationTagOnACommit states ValSem009's structural half
// on BOTH halves of the codec.
//
// The encode half is not decoration. The confirmation tag is what binds a commit to the epoch it
// creates, so an encoder that wrote a commit's auth data without one would produce a message
// every peer rejects having verified its signature first -- and an encoder whose commit arm fell
// through to the signature-only write is exactly the shape a decode-only test cannot see.
//
// The empty tag is here beside the nil one because they are the same statement on the wire and
// a length check written as a nil check would let one of them through.
func TestFramedContentAuthDataRequiresAConfirmationTagOnACommit(t *testing.T) {
	for _, missing := range []*FramedContentAuthData{
		{Signature: []byte{0x11, 0x22, 0x33}},
		{Signature: []byte{0x11, 0x22, 0x33}, ConfirmationTag: []byte{}},
	} {
		if _, err := encodeAuthData(missing, ContentTypeCommit); !errors.Is(err, errMissingConfirmationTag) {
			t.Errorf("marshalling a commit whose confirmation tag is %s gave %v, want errMissingConfirmationTag",
				describeOptionalOctets(missing.ConfirmationTag), err)
		}
	}
	// the decode half: a wire legal zero length tag is the encoding of "no tag", and what the
	// encoder refuses to write the decoder must refuse to read, or a round trip through this
	// package would produce a value this package will not re-encode.
	emptyTag := []byte{0x03, 0x11, 0x22, 0x33, 0x00}
	if _, err := decodeAuthData(emptyTag, ContentTypeCommit); !errors.Is(err, errMissingConfirmationTag) {
		t.Errorf("decoding %x as a commit gave %v, want errMissingConfirmationTag", emptyTag, err)
	}
	// and the neighbouring refusal, so the one above cannot be passing for a truncation: a
	// commit with no tag field at all ends the input early, which is the SYNTAX layer's refusal
	// and not this one. Asserting only that it was refused states nothing about the difference
	// this case exists to draw -- a decoder that answered errMissingConfirmationTag to every
	// short commit passes that form -- so both directions are named.
	noTagAtAll := []byte{0x03, 0x11, 0x22, 0x33}
	_, truncated := decodeAuthData(noTagAtAll, ContentTypeCommit)
	if errors.Is(truncated, errMissingConfirmationTag) {
		t.Errorf("decoding %x as a commit gave errMissingConfirmationTag; the input ends before the tag field, so this is a truncation, and a caller told otherwise is told a sender omitted a field it did send",
			noTagAtAll)
	}
	if !errors.Is(truncated, syntax.ErrTruncated) {
		t.Errorf("decoding %x as a commit gave %v, want syntax.ErrTruncated; the tag field is absent entirely",
			noTagAtAll, truncated)
	}
}

// TestFramedContentAuthDataRejectsAnUnregisteredContentType sweeps the whole octet space rather
// than the value somebody thought of, and states the refusal on both halves.
//
// The encode half matters for the reason Sender's does: a content type outside the registry has
// no arm, so an encoder whose default fell through to writing the signature alone would emit a
// commit's auth data shorn of its tag under a content type nothing can parse.
//
// The decode half is offered against the full commit golden, which has bytes to spare, so a
// refusal that was really a truncation cannot pass for a refusal of the TYPE.
func TestFramedContentAuthDataRejectsAnUnregisteredContentType(t *testing.T) {
	undeclared := undeclaredContentTypeOctets(t)
	if !slices.Contains(undeclared, ContentType(0)) {
		t.Fatal("0 is declared as a content type, and RFC 9420 section 6 reserves it")
	}
	for _, contentType := range undeclared {
		if _, err := encodeAuthData(testAuthData(), contentType); !errors.Is(err, ErrUnknownContentType) {
			t.Fatalf("marshalling under content type %d gave %v, want ErrUnknownContentType", contentType, err)
		}
		for _, input := range [][]byte{
			nil,
			handDerivedAuthDataGolden(ContentTypeApplication),
			handDerivedAuthDataGolden(ContentTypeCommit),
		} {
			decoded := &FramedContentAuthData{}
			r := syntax.NewReader(input)
			if err := decoded.UnmarshalMLS(r, contentType); !errors.Is(err, ErrUnknownContentType) {
				t.Fatalf("decoding %x under content type %d gave %v, want ErrUnknownContentType",
					input, contentType, err)
			}
			// refused before a single octet is consumed, so an unregistered content type does
			// not half consume the caller's Reader on its way to being refused.
			if r.Offset() != 0 {
				t.Fatalf("decoding under content type %d consumed %d octets before refusing",
					contentType, r.Offset())
			}
		}
	}
	t.Logf("%d unregistered content type octets refused on both halves of the codec", len(undeclared))
}

// TestARefusedAuthDataMarshalWritesNothing is the encoder's other half of the same discipline.
//
// There is no syntax.Marshal for this codec -- that entry point takes a one argument Marshaler
// and this one needs the content type -- so the Writer belongs to the CALLER and survives the
// refusal. An encoder that wrote the signature and then refused would leave a caller holding a
// four octet prefix of a seven octet structure with no sticky error on the Writer to say so,
// which for this structure is precisely "the confirmation tag is gone".
//
// The marker byte written first is what separates "wrote nothing" from "was handed an empty
// Writer": a length compared against zero would pass for an encoder that had truncated
// everything.
func TestARefusedAuthDataMarshalWritesNothing(t *testing.T) {
	type refusal struct {
		auth        *FramedContentAuthData
		contentType ContentType
	}
	refusals := []refusal{{auth: &FramedContentAuthData{Signature: []byte{0x11}}, contentType: ContentTypeCommit}}
	for _, contentType := range undeclaredContentTypeOctets(t) {
		refusals = append(refusals, refusal{auth: testAuthData(), contentType: contentType})
	}
	for _, each := range refusals {
		w := syntax.NewWriter()
		w.WriteUint8(0xa5)
		before := w.Len()
		if err := each.auth.MarshalMLS(w, each.contentType); err == nil {
			t.Fatalf("content type %d: the marshal this sweep expects to be refused was accepted",
				each.contentType)
		}
		if w.Len() != before {
			encoded, _ := w.Bytes()
			t.Fatalf("content type %d: a refused marshal left the Writer holding %x, %d octets past the %d it was handed",
				each.contentType, encoded, w.Len()-before, before)
		}
	}
	t.Logf("%d refused marshals wrote nothing", len(refusals))
}

// authDataRefusedDecode is one input this file can build that the CODEC ITSELF must refuse,
// against the content type it is offered under. The tail rule is deliberately not in here: a
// decode that succeeded and left bytes over is refused by r.Done() rather than by this method,
// and the receiver discipline below is a statement about the method.
type authDataRefusedDecode struct {
	input       []byte
	contentType ContentType
}

// authDataRefusedDecodes is every such input: every proper prefix of every golden, the commit
// encoding whose tag length is zeroed, and every golden under every unregistered content type.
func authDataRefusedDecodes(t *testing.T) []authDataRefusedDecode {
	t.Helper()
	found := []authDataRefusedDecode{}
	for _, contentType := range contentTypes(t) {
		golden := handDerivedAuthDataGolden(contentType)
		for cut := 0; cut < len(golden); cut += 1 {
			// a proper prefix is a refusal only where the arm needs the octets it lost.
			// application and proposal are four octets and every prefix of those is short; the
			// commit golden's four octet prefix is a whole signature and is refused because the
			// tag field is then absent entirely.
			found = append(found, authDataRefusedDecode{input: bytes.Clone(golden[:cut]), contentType: contentType})
		}
		for _, unregistered := range undeclaredContentTypeOctets(t) {
			found = append(found, authDataRefusedDecode{input: bytes.Clone(golden), contentType: unregistered})
		}
	}
	found = append(found, authDataRefusedDecode{
		input:       []byte{0x03, 0x11, 0x22, 0x33, 0x00},
		contentType: ContentTypeCommit,
	})
	return found
}

// authDataPriorContents is every state a receiver can arrive in that this file can build.
// Derived over the class rather than over the one prior somebody picks, because the failure is
// per PAIR: only a receiver that already held a confirmation tag can leave that tag standing
// under an arm that carries none.
func authDataPriorContents(t *testing.T) []*FramedContentAuthData {
	t.Helper()
	priors := []*FramedContentAuthData{{}, testAuthData()}
	for _, contentType := range contentTypes(t) {
		priors = append(priors, decodedFormOfAuthData(t, testAuthData(), contentType))
	}
	return priors
}

// TestARefusedAuthDataDecodeLeavesItsReceiverUntouched is the discipline Sender, Credential and
// LeafNode already keep, stated for this codec because it has an edge those three do not.
//
// The commit arm reads TWO fields. A decoder that assigned as it read would leave a caller's
// value holding the signature out of a message this package REFUSED -- one whose confirmation
// tag was truncated, or empty -- beside a confirmation tag left over from whatever the value
// held before. That pair is a signature and a tag nothing ever carried together, sitting in a
// variable the caller may well reuse, with nothing in the returned error saying so.
func TestARefusedAuthDataDecodeLeavesItsReceiverUntouched(t *testing.T) {
	priors := authDataPriorContents(t)
	refused, accepted := 0, 0
	for _, each := range authDataRefusedDecodes(t) {
		for _, prior := range priors {
			held := *prior
			before := *prior
			if err := held.UnmarshalMLS(syntax.NewReader(each.input), each.contentType); err == nil {
				accepted += 1
				continue
			}
			refused += 1
			if sameAuthData(&held, &before) {
				continue
			}
			t.Fatalf("content type %d: a refused decode of %x into a receiver that held\n %s\nwrote through it, leaving\n %s",
				each.contentType, each.input, describeAuthData(&before), describeAuthData(&held))
		}
	}
	if accepted != 0 {
		t.Errorf("%d of the inputs this sweep built decoded rather than being refused, so the property was never stated over them",
			accepted)
	}
	if refused == 0 {
		t.Fatal("no input was refused, so this observed nothing")
	}
	t.Logf("%d refused decodes over %d prior receiver contents left the receiver exactly as they found it",
		refused, len(priors))
}

// TestAnAuthDataDecodesToTheSameValueWhateverItsReceiverHeld is not a round trip property.
//
// The application and proposal arms assign only the signature, so a decoder that wrote through
// its receiver rather than replacing it leaves the PREVIOUS value's confirmation tag standing
// under an arm that carries none. The bytes are unaffected -- the stale tag is not written under
// that arm -- so it round trips, re-encodes byte exact and agrees with every golden here. What
// it disagrees with is the same bytes decoded into a fresh receiver, which is a proposal's
// authenticators comparing unequal to themselves depending on where they were decoded, and one
// of the two carrying a confirmation tag a later confirmation check would happily consume.
func TestAnAuthDataDecodesToTheSameValueWhateverItsReceiverHeld(t *testing.T) {
	priors := authDataPriorContents(t)
	for _, contentType := range contentTypes(t) {
		encoded := handDerivedAuthDataGolden(contentType)
		fresh, err := decodeAuthData(encoded, contentType)
		if err != nil {
			t.Fatalf("content type %d: decoding into a fresh receiver: %v", contentType, err)
		}
		for at, prior := range priors {
			reused := *prior
			r := syntax.NewReader(encoded)
			if err := reused.UnmarshalMLS(r, contentType); err != nil {
				t.Fatalf("content type %d: decoding into a receiver holding prior %d: %v", contentType, at, err)
			}
			if err := r.Done(); err != nil {
				t.Fatalf("content type %d: prior %d: %v", contentType, at, err)
			}
			if !sameAuthData(&reused, fresh) {
				t.Errorf("content type %d: decoding into a receiver that held\n %s\ngave\n %s\nand the same bytes decoded fresh give\n %s",
					contentType, describeAuthData(prior), describeAuthData(&reused), describeAuthData(fresh))
			}
		}
	}
	t.Logf("%d prior receiver contents over %d content types", len(priors), len(contentTypes(t)))
}

// TestFramedContentAuthDataRefusesATruncatedEncoding runs over every proper prefix of every
// golden rather than over the boundaries somebody thought of, and the bound is derived from the
// encoding's own length.
//
// WHICH refusal each prefix earns is derived too, off the field boundaries of the golden it was
// cut from, and that half is not decoration. A prefix ending exactly where a field begins took
// that field's length prefix away entirely, so the read runs out of input: syntax.ErrTruncated.
// A prefix ending inside a field leaves a length declaring more than remains:
// syntax.ErrLengthExceedsInput. Neither of them is errMissingConfirmationTag, and pinning that
// is the point. Every other refusal in this file is pinned exactly -- ErrTrailingBytes, the
// unregistered content type, the missing tag -- and truncation was the one class stated as a
// bare non-nil, which left the commit arm free to report every short message as a commit
// carrying no tag: a transport failure handed to the caller as ValSem009, about a field the
// sender did send. Measured rather than supposed: dropping the error check on the confirmation
// tag's own read left this package green.
func TestFramedContentAuthDataRefusesATruncatedEncoding(t *testing.T) {
	cuts, atAFieldStart, insideAField := 0, 0, 0
	for _, contentType := range contentTypes(t) {
		for _, golden := range authDataGoldens(t, contentType) {
			starts := authDataFieldStarts(t, golden)
			for cut := 0; cut < len(golden); cut += 1 {
				want := syntax.ErrLengthExceedsInput
				if slices.Contains(starts, cut) {
					want = syntax.ErrTruncated
					atAFieldStart += 1
				} else {
					insideAField += 1
				}
				_, err := decodeAuthData(golden[:cut], contentType)
				if err == nil {
					t.Errorf("content type %d: %d of the %d octets of %x decoded rather than being refused",
						contentType, cut, len(golden), golden)
					continue
				}
				if errors.Is(err, errMissingConfirmationTag) {
					t.Errorf("content type %d: %d of the %d octets of %x was refused as a commit carrying no confirmation tag; the input ended before that field, and a truncation reported as a missing field is a transport failure the caller reads as a structural one",
						contentType, cut, len(golden), golden)
					continue
				}
				if !errors.Is(err, want) {
					t.Errorf("content type %d: %d of the %d octets of %x was refused with %v, want %v",
						contentType, cut, len(golden), golden, err, want)
					continue
				}
				cuts += 1
			}
		}
	}
	if atAFieldStart == 0 || insideAField == 0 {
		t.Fatalf("the sweep built %d prefixes ending at a field start and %d ending inside a field; both are needed, or one of the two refusals is stated by nothing",
			atAFieldStart, insideAField)
	}
	t.Logf("%d truncations refused, %d of them cut at a field start and the rest inside a field",
		cuts, atAFieldStart)
}

// TestFramedContentAuthDataRefusesTrailingBytes is the full consumption half, stated through
// r.Done() because this codec is handed the caller's Reader rather than reached through
// syntax.Unmarshal.
//
// It has teeth of its own here rather than being a copy of Sender's. This structure has no
// discriminant, so a tolerated tail is not merely a second encoding of one object: it is
// exactly how a commit's confirmation tag gets read as somebody else's trailing garbage.
func TestFramedContentAuthDataRefusesTrailingBytes(t *testing.T) {
	stated := 0
	for _, contentType := range contentTypes(t) {
		golden := handDerivedAuthDataGolden(contentType)
		for _, tail := range [][]byte{{0x00}, {0xff}, {0x00, 0x00}, repeatByte(0x5a, 17)} {
			longer := joinBytes(golden, tail)
			if _, err := decodeAuthData(longer, contentType); !errors.Is(err, syntax.ErrTrailingBytes) {
				t.Errorf("content type %d with %d trailing octets (%x): err = %v, want syntax.ErrTrailingBytes",
					contentType, len(tail), longer, err)
				continue
			}
			stated += 1
		}
	}
	if stated == 0 {
		t.Fatal("no tailed encoding was refused, so this observed nothing")
	}
	t.Logf("%d tailed encodings refused over %d content types", stated, len(contentTypes(t)))
}

// TestEverySingleOctetCorruptionOfAnAuthDataIsRefusedOrReEncodesToItself is the other half of
// the length sweep: every input one octet away from a golden, at every position, over every
// value that octet could hold.
//
// The property is the one Gate 4 states over the whole codec table: an accepted input has
// exactly one encoding. Anything else is a second encoding of one object, and MLS signs over
// serialized forms -- so two byte strings this package accepts as the same value are a
// signature that covers one of them and a peer that acted on the other.
func TestEverySingleOctetCorruptionOfAnAuthDataIsRefusedOrReEncodesToItself(t *testing.T) {
	refused, accepted := 0, 0
	for _, contentType := range contentTypes(t) {
		golden := handDerivedAuthDataGolden(contentType)
		for at := 0; at < len(golden); at += 1 {
			for value := 0; value <= 0xff; value += 1 {
				if byte(value) == golden[at] {
					continue
				}
				corrupted := bytes.Clone(golden)
				corrupted[at] = byte(value)
				decoded, err := decodeAuthData(corrupted, contentType)
				if err != nil {
					refused += 1
					continue
				}
				accepted += 1
				reencoded, err := encodeAuthData(decoded, contentType)
				if err != nil {
					t.Fatalf("content type %d: %x was accepted and then would not re-encode: %v",
						contentType, corrupted, err)
				}
				if !bytes.Equal(reencoded, corrupted) {
					t.Fatalf("content type %d: %x was accepted as\n %s\nand re-encodes to %x, so this codec holds two encodings of one value",
						contentType, corrupted, describeAuthData(decoded), reencoded)
				}
			}
		}
	}
	if refused == 0 || accepted == 0 {
		t.Fatalf("the sweep refused %d and accepted %d single octet corruptions; both halves must be exercised or it states only one of them",
			refused, accepted)
	}
	t.Logf("%d single octet corruptions refused, %d accepted and byte exact on re-encode", refused, accepted)
}

// TestFramedContentAuthDataIsDeliberatelyNotASyntaxCodec is registry section 7.2's shape, held
// at run time beside the compile time pins framing.go carries.
//
// The registry refuses this type a discriminant field, which is what makes syntax.Codec
// unreachable for it: both methods need the content type, and Codec's do not take one. A later
// task that "tidied" the two argument methods into Codec's shape would have to store the content
// type somewhere, and that copy would then be what decided which arm was read -- the failure the
// parameter exists to prevent. That change stops compiling in framing.go; this is what states
// why, where a reader will find it.
func TestFramedContentAuthDataIsDeliberatelyNotASyntaxCodec(t *testing.T) {
	codec := reflect.TypeOf((*syntax.Codec)(nil)).Elem()
	if reflect.TypeOf((*FramedContentAuthData)(nil)).Implements(codec) {
		t.Error("*FramedContentAuthData satisfies syntax.Codec, so it has one argument MarshalMLS/UnmarshalMLS methods and is carrying a content type of its own somewhere")
	}
	// the control: the sibling codec of this file does satisfy it, so a false above cannot be
	// an interface this test failed to look up.
	if !reflect.TypeOf((*Sender)(nil)).Implements(codec) {
		t.Fatal("*Sender does not satisfy syntax.Codec, so the assertion above is reading the wrong interface")
	}
}

// ---------------------------------------------------------------------------
// the signature field at length zero
// ---------------------------------------------------------------------------

// handDerivedEmptySignatureAuthDataGolden is section 6's FramedContentAuthData with an EMPTY
// signature, written from the wire format rather than read back out of framing.go.
//
// It is here because signature is an opaque<V> like any other, so "no signature" is a length of
// zero and a body of nothing -- a field that is still THERE, occupying the one octet its length
// prefix takes. Every golden above carries a three octet signature, so an encoder that wrote the
// field only when it had something to put in it agrees with all three of them and with every
// property derived from them. That is measured rather than supposed: both WriteOpaque calls for
// this field wrapped in a length test left ./mls/... and ./message/... green.
//
// What such an encoder produces is not a shorter message, it is a DIFFERENT one. Under
// application the whole structure becomes zero octets, which the enclosing decode meets as the
// end of its input; under commit the confirmation tag's length prefix moves to offset zero,
// where a decoder takes it for the signature's -- a commit whose tag is read as its signature
// and whose signature is gone, with every field after it shifted by one.
//
//	signature<V> over nothing       length 0 -> 1 octet 0x00, body 0 octets = 1
//	confirmation_tag<V> over 44 55  length 2 -> 1 octet 0x02, body 2 octets = 3
//
//	application  1 + 0 = 1
//	proposal     1 + 0 = 1
//	commit       1 + 3 = 4
func handDerivedEmptySignatureAuthDataGolden(contentType ContentType) []byte {
	switch contentType {
	case ContentTypeApplication:
		return []byte{0x00}
	case ContentTypeProposal:
		return []byte{0x00}
	case ContentTypeCommit:
		return []byte{0x00, 0x02, 0x44, 0x55}
	}
	return nil
}

// handDerivedEmptySignatureAuthDataSizes is the arithmetic of the comment above, stated
// separately for handDerivedAuthDataSizes' reason: a derivation edited without its comment then
// fails rather than redefining what it is compared against.
var handDerivedEmptySignatureAuthDataSizes = map[ContentType]int{
	ContentTypeApplication: 1,
	ContentTypeProposal:    1,
	ContentTypeCommit:      4,
}

// emptySignatureAuthData is the value those goldens encode: the given signature, which is the
// absent one in both of its spellings, beside the confirmation tag every other sweep here uses.
// The tag is kept so the commit arm still has one to write and its encoding still differs from
// the application arm's by exactly that field.
func emptySignatureAuthData(signature []byte) *FramedContentAuthData {
	return &FramedContentAuthData{Signature: signature, ConfirmationTag: testAuthData().ConfirmationTag}
}

// TestTheAuthDataSignatureFieldIsWrittenAtEveryLengthIncludingZero states the one length no
// other sweep in this file offers either half of the codec.
//
// The signature of testAuthData is three octets, every golden above carries it, and the one
// value here holding an empty signature is only ever a receiver and never an encode input -- so
// the wire shape of this field at length zero was a claim nothing made. It is not an exotic
// input: a signature is opaque<V>, the empty one is legal, and which of "0x00" and "nothing at
// all" it encodes to is the difference between a decoder finding the confirmation tag where it
// belongs and finding it one field early.
//
// The nil and the empty slice are both offered because they are ONE statement on the wire and
// two different values in Go: a presence test written as a nil check passes for one of them and
// not for the other. The decode half then pins which of the two comes back, because that pair
// does not round trip to an equal VALUE -- only to equal bytes -- and describeOptionalOctets
// exists in this file precisely to keep the two apart, so which one a decode produces is a fact
// this file owes a reader rather than one it leaves to whoever reads syntax/decode.go.
func TestTheAuthDataSignatureFieldIsWrittenAtEveryLengthIncludingZero(t *testing.T) {
	declared := contentTypes(t)
	for _, contentType := range declared {
		want := handDerivedEmptySignatureAuthDataGolden(contentType)
		if want == nil {
			t.Fatalf("content type %d has no empty signature golden, so nothing states what this codec writes when the signature is absent",
				contentType)
		}
		size, stated := handDerivedEmptySignatureAuthDataSizes[contentType]
		if !stated {
			t.Fatalf("content type %d has no empty signature size, so its golden is compared only against itself",
				contentType)
		}
		if len(want) != size {
			t.Fatalf("content type %d: the empty signature derivation is %d octets and the arithmetic in its comment says %d",
				contentType, len(want), size)
		}
		for _, signature := range [][]byte{nil, {}} {
			encoded, err := encodeAuthData(emptySignatureAuthData(signature), contentType)
			if err != nil {
				t.Fatalf("content type %d: marshalling a signature of %s: %v",
					contentType, describeOptionalOctets(signature), err)
			}
			if !bytes.Equal(encoded, want) {
				t.Errorf("content type %d: a signature of %s encoded to\n %x\nwant\n %x\nthe signature is an opaque<V>, so a signature of length zero is a length prefix of zero and not a field left out",
					contentType, describeOptionalOctets(signature), encoded, want)
			}
		}
		decoded, err := decodeAuthData(want, contentType)
		if err != nil {
			t.Fatalf("content type %d: decoding %x: %v", contentType, want, err)
		}
		if expected := decodedFormOfAuthData(t, emptySignatureAuthData([]byte{}), contentType); !sameAuthData(decoded, expected) {
			t.Errorf("content type %d: %x decoded to\n %s\nwant\n %s",
				contentType, want, describeAuthData(decoded), describeAuthData(expected))
		}
		if decoded.Signature == nil {
			t.Errorf("content type %d: %x decoded to a nil signature; every arm carries the field, so a nil here would be the decoder saying the field was absent and no encoding of this structure can say that",
				contentType, want)
		}
		reencoded, err := encodeAuthData(decoded, contentType)
		if err != nil {
			t.Fatalf("content type %d: re-marshalling %s: %v", contentType, describeAuthData(decoded), err)
		}
		if !bytes.Equal(reencoded, want) {
			t.Errorf("content type %d: re-encode =\n %x\nwant\n %x", contentType, reencoded, want)
		}
	}
	if len(handDerivedEmptySignatureAuthDataSizes) != len(declared) {
		t.Errorf("the empty signature size table holds %d entries and the package declares %d content types",
			len(handDerivedEmptySignatureAuthDataSizes), len(declared))
	}
}

// authDataGoldens is every hand derived encoding this file holds for one content type: the
// populated one and the one whose signature is empty. Swept together where a property is about
// the LAYOUT rather than about the values, because the two families put their field boundaries
// in different places and a boundary rule stated over one of them is stated over one shape.
func authDataGoldens(t *testing.T, contentType ContentType) [][]byte {
	t.Helper()
	found := [][]byte{}
	for _, golden := range [][]byte{
		handDerivedAuthDataGolden(contentType),
		handDerivedEmptySignatureAuthDataGolden(contentType),
	} {
		if golden == nil {
			t.Fatalf("content type %d is missing one of its hand derived goldens, so a sweep meant to run over both families ran over one",
				contentType)
		}
		found = append(found, golden)
	}
	return found
}

// authDataFieldStarts derives the offsets a golden's length prefixed fields begin at, by walking
// the prefixes rather than by transcribing the layout a second time.
//
// Every field of this structure is an opaque<V> and every length in these goldens is below 64,
// so each prefix is the single octet holding its own body length -- which makes this a reading
// of the encoding rather than a second claim about it. Landing exactly on the end is checked
// rather than assumed: a golden this walk could not account for would hand its caller a set of
// boundaries that are not boundaries, and every expectation derived from them would be arbitrary
// while still looking derived.
func authDataFieldStarts(t *testing.T, golden []byte) []int {
	t.Helper()
	starts := []int{}
	for at := 0; at < len(golden); {
		if golden[at] >= 0x40 {
			t.Fatalf("the octet %#02x at offset %d of %x is not a single octet varint length prefix, so this walk cannot read that golden's layout",
				golden[at], at, golden)
		}
		starts = append(starts, at)
		next := at + 1 + int(golden[at])
		if next > len(golden) {
			t.Fatalf("the field beginning at offset %d of %x declares %d octets and runs past its end",
				at, golden, golden[at])
		}
		at = next
	}
	if len(starts) == 0 {
		t.Fatalf("no field was read out of %x, so no boundary was derived from it", golden)
	}
	return starts
}

// ---------------------------------------------------------------------------
// the refusals that name the code point they refused
// ---------------------------------------------------------------------------

// framingRegistrySwitch is one switch over a framing registry in framing.go, together with the
// refusal it reaches when none of its cases matched.
//
// The class is derived from the file rather than listed, because framing.go's own package
// comment makes the claim about ALL of them -- "the refusal paths therefore name the offending
// value numerically" -- and a list of the ones somebody remembered is how three come to be held
// and the fourth not. That is this project's most repeated failure and it is why the behavioural
// sweep below is keyed on this derivation rather than standing beside it.
type framingRegistrySwitch struct {
	method   string
	file     string
	registry string
	tag      string
	refusal  ast.Expr
}

// registryOfSwitch names the framing registry a switch discriminates on, read off its CASE
// expressions rather than off its tag.
//
// The cases are the better evidence and they are also the only evidence available. A tag is a
// parameter or a field selector, and typing either needs the function bodies the package type
// check deliberately skips; the cases are package level constants, whose types that check
// already holds. They are the code points themselves, which is the thing being claimed about.
func registryOfSwitch(t *testing.T, file string, statement *ast.SwitchStmt, registries []string) string {
	t.Helper()
	scope := typeCheckedPackage(t).Scope()
	found := ""
	for _, clause := range statement.Body.List {
		caseClause, isCase := clause.(*ast.CaseClause)
		if !isCase {
			continue
		}
		for _, expression := range caseClause.List {
			name, isName := expression.(*ast.Ident)
			if !isName {
				continue
			}
			constant, isConstant := scope.Lookup(name.Name).(*types.Const)
			if !isConstant {
				continue
			}
			named, isNamed := types.Unalias(constant.Type()).(*types.Named)
			if !isNamed || !slices.Contains(registries, named.Obj().Name()) {
				continue
			}
			if found != "" && found != named.Obj().Name() {
				t.Fatalf("one switch in %s names constants of both %s and %s, so which registry it discriminates on cannot be derived",
					file, found, named.Obj().Name())
			}
			found = named.Obj().Name()
		}
	}
	return found
}

// returnedExpression is the error a return statement returns, or nil for anything that is not
// one. A `return nil` is reported as none, so a search for the refusal a switch falls through to
// does not stop at the success path of a case clause.
//
// The LAST result rather than the only one, and the difference is measured. Every codec in this
// package returns one value and a reader written for that shape found nothing in
// ratchetTypeOf's `return 0, fmt.Errorf(...)` -- so secret_tree.go's content type refusal was
// read as a switch that falls out of the bottom of itself, which is the failure this gate
// exists to report and was here a failure of the reader. Every function this gate sweeps
// returns its error last, which is the language's own convention, and `return v, nil` still
// reports none because the last result is the nil.
func returnedExpression(statement ast.Stmt) ast.Expr {
	returned, isReturn := statement.(*ast.ReturnStmt)
	if !isReturn || len(returned.Results) == 0 {
		return nil
	}
	last := returned.Results[len(returned.Results)-1]
	if name, isName := last.(*ast.Ident); isName && name.Name == "nil" {
		return nil
	}
	return last
}

// TestTheRefusalReaderReadsTheErrorResultAndNotTheSuccessOne is that reader's control, in both
// directions for the reason every reader in this file carries one. One that read the first
// result would report a case clause's success path as the switch's refusal; one that insisted
// on a single result reads a two result refusal as no refusal at all, which is what it did.
func TestTheRefusalReaderReadsTheErrorResultAndNotTheSuccessOne(t *testing.T) {
	for _, one := range []struct {
		source  string
		refusal string
	}{
		{"return fmt.Errorf(\"%w: %d\", ErrUnknownContentType, contentType)", "fmt.Errorf(\"%w: %d\", ErrUnknownContentType, contentType)"},
		{"return 0, fmt.Errorf(\"%w: content type %d\", ErrUnknownContentType, contentType)", "fmt.Errorf(\"%w: content type %d\", ErrUnknownContentType, contentType)"},
		{"return ErrContentArmMismatch", "ErrContentArmMismatch"},
		// the success paths, which are not refusals however many results they carry
		{"return nil", ""},
		{"return RatchetApplication, nil", ""},
		// and a statement that is not a return at all
		{"self.A = 1", ""},
	} {
		parsed, err := parser.ParseFile(token.NewFileSet(), "control.go",
			"package control\nfunc f() { "+one.source+" }\n", parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %q: %v", one.source, err)
		}
		body := parsed.Decls[0].(*ast.FuncDecl).Body
		got := ""
		if expression := returnedExpression(body.List[0]); expression != nil {
			got = types.ExprString(expression)
		}
		if got != one.refusal {
			t.Errorf("%q read as the refusal %q, want %q", one.source, got, one.refusal)
		}
	}
}

// defaultClauseRefusal is the error a switch returns from its default clause, if it has one.
// Both spellings of an exhaustive refusal are read by the caller -- a default clause, and a
// return standing after the switch -- because framing.go uses one of each and the difference is
// a matter of taste rather than of what is being refused.
func defaultClauseRefusal(statement *ast.SwitchStmt) ast.Expr {
	for _, clause := range statement.Body.List {
		caseClause, isCase := clause.(*ast.CaseClause)
		if !isCase || caseClause.List != nil {
			continue
		}
		for _, inner := range caseClause.Body {
			if expression := returnedExpression(inner); expression != nil {
				return expression
			}
		}
	}
	return nil
}

// framingRegistrySwitches derives every registry switch this package's production source holds,
// keyed by the function that holds it.
//
// The scan set is framingRegistryFunctions and therefore the whole package. It used to be
// framing.go, which is what let framing_preimage.go's two wire format switches be deleted whole
// with the suite green, and which also left secret_tree.go's content type switch -- a refusal a
// peer reaches by sending an undefined content type -- outside every gate here.
func framingRegistrySwitches(t *testing.T) map[string]framingRegistrySwitch {
	t.Helper()
	registries := framingRegistries(t)
	found := map[string]framingRegistrySwitch{}
	for _, function := range framingRegistryFunctions(t) {
		method, file := function.name, function.file
		ast.Inspect(function.decl.Body, func(node ast.Node) bool {
			block, isBlock := node.(*ast.BlockStmt)
			if !isBlock {
				return true
			}
			for at, statement := range block.List {
				switched, isSwitch := statement.(*ast.SwitchStmt)
				if !isSwitch || switched.Tag == nil {
					continue
				}
				registry := registryOfSwitch(t, file, switched, registries)
				if registry == "" {
					continue
				}
				// the refusal is the default clause where there is one, and otherwise the
				// statement the switch falls through to, which is where a switch written
				// without a default puts it.
				refusal := defaultClauseRefusal(switched)
				if refusal == nil && at+1 < len(block.List) {
					refusal = returnedExpression(block.List[at+1])
				}
				if other, repeated := found[method]; repeated {
					t.Fatalf("%s holds a switch over %s and another over %s; this derivation keys one switch per method and cannot say which refusal belongs to which",
						method, other.registry, registry)
				}
				found[method] = framingRegistrySwitch{
					method:   method,
					file:     file,
					registry: registry,
					tag:      types.ExprString(switched.Tag),
					refusal:  refusal,
				}
			}
			return true
		})
	}
	if len(found) == 0 {
		t.Fatal("no switch over a framing registry was derived from this package's production source, which holds one in every framing codec method it declares, so every gate reading this would run over the empty set")
	}
	return found
}

// framingCodecMethodFiles is every MarshalMLS and UnmarshalMLS of this package that computes
// with a framing registry, with the file that declares it.
//
// The file travels with the method because it is what a failure here has to NAME. While the
// derivation root was framingFile every message below said "framing.go", and framing.go was the
// wrong answer for the one codec that was outside the gate -- the message would have been both
// false and reassuring.
func framingCodecMethodFiles(t *testing.T) map[string]string {
	t.Helper()
	found := map[string]string{}
	for _, function := range framingRegistryFunctions(t) {
		_, method, isMethod := strings.Cut(function.name, ".")
		if !isMethod || (method != "MarshalMLS" && method != "UnmarshalMLS") {
			continue
		}
		found[function.name] = function.file
	}
	if len(found) == 0 {
		t.Fatal("no framing codec method was derived from this package's production source, which declares several, so every gate reading this would run over the empty set")
	}
	return found
}

// framingCodecMethods is that class as a sorted list, which is the shape the set joins below
// take.
func framingCodecMethods(t *testing.T) []string {
	t.Helper()
	return slices.Sorted(maps.Keys(framingCodecMethodFiles(t)))
}

// expressionMentions reports whether an expression holds a sub expression written exactly as the
// given source text. Text rather than object identity because both ends come out of one file and
// one parse: the tag of the switch and the arguments of its refusal are the same source.
func expressionMentions(expression ast.Expr, text string) bool {
	found := false
	ast.Inspect(expression, func(node ast.Node) bool {
		if found {
			return false
		}
		inner, isExpression := node.(ast.Expr)
		if isExpression && types.ExprString(inner) == text {
			found = true
			return false
		}
		return true
	})
	return found
}

// TestTheRegistrySwitchReaderSeparatesARefusalThatNamesItsTagFromOneThatDoesNot is the control
// on that reader, and it is the same control the width pin reader carries. One that recognised
// everything would report every refusal as naming its code point, including the bare sentinel
// this gate exists to catch; one that recognised nothing would fail against the real file for
// the wrong reason. Both halves are stated over expressions written here.
func TestTheRegistrySwitchReaderSeparatesARefusalThatNamesItsTagFromOneThatDoesNot(t *testing.T) {
	for _, one := range []struct {
		source   string
		tag      string
		mentions bool
	}{
		{`fmt.Errorf("%w: %d", ErrUnknownContentType, contentType)`, "contentType", true},
		{`fmt.Errorf("%w: %d", ErrUnknownSenderType, self.SenderType)`, "self.SenderType", true},
		{`fmt.Errorf("%w: content type %d", ErrUnknownContentType, contentType)`, "contentType", true},
		// the survivor: a refusal that has stopped saying which code point it refused
		{`ErrUnknownContentType`, "contentType", false},
		{`fmt.Errorf("%w", ErrUnknownContentType)`, "contentType", false},
		// and a refusal naming something else, which is the shape a copied arm produces
		{`fmt.Errorf("%w: %d", ErrUnknownContentType, self.SenderType)`, "contentType", false},
	} {
		expression, err := parser.ParseExpr(one.source)
		if err != nil {
			t.Fatalf("parse %q: %v", one.source, err)
		}
		if got := expressionMentions(expression, one.tag); got != one.mentions {
			t.Errorf("%q read against the tag %s as %v, want %v", one.source, one.tag, got, one.mentions)
		}
	}
}

// TestEveryFramingRegistrySwitchRefusesByNamingTheCodePointItRefused holds framing.go's package
// comment to framing.go.
//
// That comment states the decision this file's registries turn on: none of them declares its
// reserved zero, so an unparsed header is a refusal rather than a real code point, and "the
// refusal paths therefore name the offending value numerically, which is what they have off the
// wire anyway". Nothing counted those paths. Both of the content type refusals could be reduced
// to a bare sentinel with the whole package green -- measured -- which leaves a caller holding
// an error that says only that SOMETHING in the header was unregistered, about a header they
// cannot re-read because the decode refused it.
//
// The class is the switches themselves, and the control is the other direction: every codec
// method framing.go declares must hold one, so a codec that discriminates on a registry without
// refusing the rest of it is reported here rather than being outside the sweep.
func TestEveryFramingRegistrySwitchRefusesByNamingTheCodePointItRefused(t *testing.T) {
	switches := framingRegistrySwitches(t)
	files := framingCodecMethodFiles(t)
	for _, method := range framingCodecMethods(t) {
		if _, held := switches[method]; !held {
			t.Errorf("%s declares %s and no switch over a framing registry was derived from it; every framing codec in this package discriminates on one, so either this method refuses no unregistered code point at all or the derivation has stopped seeing its switch",
				files[method], method)
		}
	}
	for _, method := range slices.Sorted(maps.Keys(switches)) {
		each := switches[method]
		if each.refusal == nil {
			t.Errorf("%s switches on %s and reaches no refusal where none of its cases match, so an unregistered code point leaves that switch by falling out of the bottom of it",
				method, each.registry)
			continue
		}
		if !expressionMentions(each.refusal, each.tag) {
			t.Errorf("%s refuses an unregistered %s with %s, which does not name %s; %s says these paths name the offending value numerically, and one that does not tells the caller only that something in the header was unregistered",
				method, each.registry, types.ExprString(each.refusal), each.tag, each.file)
		}
	}
	t.Logf("%d registry switches, each refusing by naming its own discriminant", len(switches))
}

// framingCodePointRefusals invokes each of those refusals with a code point its registry does
// not declare. It is a table, which is the shape this file distrusts, so the gate below holds
// its keys to the switches DERIVED from framing.go in both directions: a codec added to that
// file with no entry here is a refusal nothing invokes, and an entry here for a method that no
// longer switches on a registry is a sweep running over a claim the file has stopped making.
//
// Each invocation reaches the refusal and nothing else. The two decoders are handed input the
// arm would have consumed had the code point been registered -- a sender arm's octet, a whole
// commit auth data -- so a refusal that was really a truncation cannot pass for a refusal of
// the code point.
// handDerivedCommitTail is a valid AuthenticatedContent carrying an empty commit, MINUS its two
// octet wire format, written out here rather than produced by the encoder under test.
//
// The octets are the RFC's structure read off section 6: an empty group_id<V>, an eight octet
// epoch, a member sender at leaf 0, an empty authenticated_data<V>, the commit content type, an
// empty proposals<V>, an absent optional path, an empty signature<V> and a one octet
// confirmation tag. Hand derived because the decoder refusal it stands behind is being asked to
// refuse the wire format rather than to run out of input, and bytes produced by the encoder
// under test would make that a statement about the encoder.
func handDerivedCommitTail() []byte {
	return []byte{
		0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x01, 0x00, 0x00, 0x00, 0x00,
		0x00,
		0x03,
		0x00,
		0x00,
		0x00,
		0x01, 0xaa,
	}
}

// handDerivedFramedContentHeader is the five fields every FramedContent opens with, ending in
// the content type the caller names, followed by two octets an arm would have consumed.
//
// The trailing octets are the point: without them a decoder that refused nothing would run out
// of input and report a truncation, which reads as a refusal of the code point and is not one.
func handDerivedFramedContentHeader(contentType ContentType) []byte {
	return []byte{
		0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x01, 0x00, 0x00, 0x00, 0x00,
		0x00,
		byte(contentType),
		0x00, 0x00,
	}
}

// handDerivedTranscriptInputTail is a valid ConfirmedTranscriptHashInput carrying an empty
// commit, MINUS its two octet wire format.
//
// RFC 9420 section 8.2 writes wire_format, then the FramedContent, then opaque signature<V> --
// and NOT the confirmation tag, which is that structure's whole design. So this is
// handDerivedCommitTail's FramedContent followed by a signature vector and it stops where an
// AuthenticatedContent would have carried a tag. Hand derived for handDerivedCommitTail's
// reason: the decoder behind it is being asked to refuse a wire format rather than to run out
// of input, and bytes produced by the encoder under test would make that a statement about the
// encoder.
func handDerivedTranscriptInputTail() []byte {
	return []byte{
		0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x01, 0x00, 0x00, 0x00, 0x00,
		0x00,
		0x03,
		0x00,
		0x00,
		0x01, 0xaa,
	}
}

// handDerivedPrivateMessageTail is a whole valid PrivateMessage, written out here rather than
// produced by the encoder under test.
//
// RFC 9420 section 6.3: an empty group_id<V>, an eight octet epoch, the application content type,
// and three empty vectors. It stands behind the two octets the MLSMessage decoder is being asked
// to refuse, for handDerivedCommitTail's reason: a refusal that was really a truncation would read
// as a refusal of the code point and is not one.
func handDerivedPrivateMessageTail() []byte {
	return []byte{
		0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x01,
		0x00,
		0x00,
		0x00,
	}
}

var framingCodePointRefusals = map[string]func(codePoint uint64) error{
	// framing.go's outermost structure, at both ends of the same wire format registry. The
	// encoder is handed the code point and NO arm, because it refuses the discriminant ahead of
	// the arm check; the decoder is handed a whole valid private message behind the two octets
	// it refuses, so a refusal that was really a truncation cannot pass for a refusal of the
	// code point. The version is mls10 in both, because this pair is about the second field and
	// an unsupported version would refuse ahead of it.
	"MLSMessage.MarshalMLS": func(codePoint uint64) error {
		return (&MLSMessage{Version: ProtocolVersionMls10, WireFormat: WireFormat(codePoint)}).MarshalMLS(syntax.NewWriter())
	},
	"MLSMessage.UnmarshalMLS": func(codePoint uint64) error {
		return (&MLSMessage{}).UnmarshalMLS(syntax.NewReader(append(
			[]byte{0x00, 0x01, byte(codePoint >> 8), byte(codePoint)}, handDerivedPrivateMessageTail()...)))
	},
	"Sender.MarshalMLS": func(codePoint uint64) error {
		return (&Sender{SenderType: SenderType(codePoint)}).MarshalMLS(syntax.NewWriter())
	},
	"Sender.UnmarshalMLS": func(codePoint uint64) error {
		return (&Sender{}).UnmarshalMLS(syntax.NewReader([]byte{byte(codePoint), 0x00, 0x00, 0x00, 0x00}))
	},
	// framing.go's wire format registry, which had no raiser in production source until the
	// authenticated content codec grew one. The encoder is handed nothing but the code point,
	// because it refuses before it writes; the decoder is handed the whole of a valid commit's
	// authenticated content behind the two octets it refuses, so a refusal that was really a
	// truncation cannot pass for a refusal of the code point.
	"AuthenticatedContent.MarshalMLS": func(codePoint uint64) error {
		return (&AuthenticatedContent{WireFormat: WireFormat(codePoint)}).MarshalMLS(syntax.NewWriter())
	},
	"AuthenticatedContent.UnmarshalMLS": func(codePoint uint64) error {
		return (&AuthenticatedContent{}).UnmarshalMLS(syntax.NewReader(append(
			[]byte{byte(codePoint >> 8), byte(codePoint)}, handDerivedCommitTail()...)))
	},
	// the content type registry, at all three of the places framing.go discriminates on it.
	// checkArms is reached on its own as well as through the encoder, because the encoder
	// refuses through it and a reader arriving at either has to be told the same number.
	"FramedContent.checkArms": func(codePoint uint64) error {
		return (&FramedContent{ContentType: ContentType(codePoint)}).checkArms()
	},
	"FramedContent.MarshalMLS": func(codePoint uint64) error {
		return (&FramedContent{
			Sender:      Sender{SenderType: SenderTypeMember},
			ContentType: ContentType(codePoint),
		}).MarshalMLS(syntax.NewWriter())
	},
	"FramedContent.UnmarshalMLS": func(codePoint uint64) error {
		return (&FramedContent{}).UnmarshalMLS(syntax.NewReader(handDerivedFramedContentHeader(ContentType(codePoint))))
	},
	"FramedContentAuthData.MarshalMLS": func(codePoint uint64) error {
		return testAuthData().MarshalMLS(syntax.NewWriter(), ContentType(codePoint))
	},
	"FramedContentAuthData.UnmarshalMLS": func(codePoint uint64) error {
		return (&FramedContentAuthData{}).UnmarshalMLS(
			syntax.NewReader(handDerivedAuthDataGolden(ContentTypeCommit)), ContentType(codePoint))
	},
	// framing_preimage.go's section 8.2 structure, which reads and writes the same wire
	// format registry AuthenticatedContent does. These two rows are here because the class
	// this gate sweeps is the PACKAGE's registry switches and no longer framing.go's: while
	// it was one file wide, both of these switches could be deleted whole with the whole of
	// ./mls/... and ./message/... byte identically green, and a transcript preimage could
	// then be built over a wire format no registry declares -- which is a hash every peer
	// computes differently and a fork with no recovery path.
	"confirmedTranscriptHashInput.MarshalMLS": func(codePoint uint64) error {
		return (&confirmedTranscriptHashInput{WireFormat: WireFormat(codePoint)}).MarshalMLS(syntax.NewWriter())
	},
	"confirmedTranscriptHashInput.UnmarshalMLS": func(codePoint uint64) error {
		return (&confirmedTranscriptHashInput{}).UnmarshalMLS(syntax.NewReader(append(
			[]byte{byte(codePoint >> 8), byte(codePoint)}, handDerivedTranscriptInputTail()...)))
	},
	// framing_preimage.go's section 6.1 signature preimage, which reads the same wire format
	// registry the two above do. The encoder is handed nothing but the code point, because it
	// refuses the wire format before it looks at anything else -- which is the order this row
	// depends on and the order that body is written in.
	"framedContentTBS.MarshalMLS": func(codePoint uint64) error {
		return (&framedContentTBS{WireFormat: WireFormat(codePoint)}).MarshalMLS(syntax.NewWriter())
	},
	// the sender type switch that decides whether a FramedContentTBS inlines the group
	// context. It is not a codec either, and it is swept here for the reason ratchetTypeOf is:
	// the class is every place in this package that discriminates on a framing registry, and a
	// peer reaches this one by sending a sender type nobody has defined.
	"senderBindsGroupContext": func(codePoint uint64) error {
		_, err := senderBindsGroupContext(SenderType(codePoint))
		return err
	},
	// secret_tree.go's content type switch. It is not a codec and it is swept here for the
	// same reason the two above are: the class is every place in this package that
	// discriminates on a framing registry, and a peer reaches this one by sending a content
	// type nobody has defined.
	"ratchetTypeOf": func(codePoint uint64) error {
		_, err := ratchetTypeOf(ContentType(codePoint))
		return err
	},
	// p7 task 18's receive path, which discriminates on the same content type registry and is the
	// place an octet that got past every decoder would decide which of section 6's three kinds of
	// message this client acts on.
	//
	// THE GROUP IS THE ZERO VALUE WITH A GROUP CONTEXT AND NOTHING ELSE, and that is what makes
	// this a row rather than a group founding per code point. The content type select stands behind
	// ValSem002 and ValSem003, so the only state this refusal needs is a context the content agrees
	// with -- an empty group id and epoch 0 on both sides -- and every arm that would read the
	// tree, the proposal cache or the key schedule is one an undeclared code point never reaches.
	"Group.processAuthenticatedLocked": func(codePoint uint64) error {
		group := &Group{context: &GroupContext{}}
		_, err := group.processAuthenticatedLocked(&AuthenticatedContent{
			Content: FramedContent{
				Sender:      Sender{SenderType: SenderTypeMember},
				ContentType: ContentType(codePoint),
			},
		}, 0)
		return err
	},
	// framing.go's section 6.3 header, which is the FIRST place a content type off the wire is
	// seen. The encoder is handed nothing but the code point, because it refuses before it
	// writes; the decoder is handed a whole header with two octets standing behind the content
	// type, so a refusal that was really a truncation cannot pass for a refusal of the value.
	// framing.go's section 6.2 select on sender_type, at both ends of it. The encoder's default
	// is reached by this row directly, because that switch stands ahead of every write and ahead
	// of Sender.MarshalMLS. The decoder's own default is unreachable behind Sender.UnmarshalMLS
	// and its comment says so; what this row observes there is that the method refuses the octet
	// and names it, which is the message a peer receives either way.
	"PublicMessage.MarshalMLS": func(codePoint uint64) error {
		return (&PublicMessage{Content: FramedContent{
			Sender: Sender{SenderType: SenderType(codePoint)},
		}}).MarshalMLS(syntax.NewWriter())
	},
	"PublicMessage.UnmarshalMLS": func(codePoint uint64) error {
		return (&PublicMessage{}).UnmarshalMLS(
			syntax.NewReader(handDerivedPublicMessageHeader(SenderType(codePoint))))
	},
	"PrivateMessage.MarshalMLS": func(codePoint uint64) error {
		return (&PrivateMessage{ContentType: ContentType(codePoint)}).MarshalMLS(syntax.NewWriter())
	},
	"PrivateMessage.UnmarshalMLS": func(codePoint uint64) error {
		return (&PrivateMessage{}).UnmarshalMLS(
			syntax.NewReader(handDerivedPrivateMessageHeader(ContentType(codePoint))))
	},
	// framing_preimage.go's section 6.3 sender data AAD. It is not a codec and it is swept
	// here for senderBindsGroupContext's reason: the class is every place in this package
	// that discriminates on a framing registry, and this one is reached with a content type
	// that came off a PrivateMessage header. It is handed nothing but the code point, because
	// it refuses before it writes -- and privateContentAAD is deliberately NOT a row of its
	// own, since it holds no switch: what refuses an unregistered content type on its behalf
	// is the single assembly it delegates to, which is
	// TestBothSectionSixThreeAadsRefuseAContentTypeNoRegistryDeclares' subject.
	"senderDataAAD": func(codePoint uint64) error {
		_, err := senderDataAAD(nil, 0, ContentType(codePoint))
		return err
	},
	// framing_protect.go's section 6.3.1 PrivateMessageContent, at both ends of it. The encoder
	// is handed nothing but the code point, because it refuses before it writes -- through
	// FramedContent.checkArms, which is the single arm rule and is why the encoder's own default
	// is unreachable; what this row observes is that the caller is told the number either way.
	// The decoder reads the content type off the CLEARTEXT header, which is to say off the wire,
	// so this is the shape an attacker chosen header arrives in and the refusal is what stops an
	// unregistered arm being decoded as an application blob.
	"marshalPrivateMessageContentWithPadding": func(codePoint uint64) error {
		_, err := marshalPrivateMessageContentWithPadding(&FramedContent{
			Sender:      Sender{SenderType: SenderTypeMember},
			ContentType: ContentType(codePoint),
		}, &FramedContentAuthData{}, nil)
		return err
	},
	"unmarshalPrivateMessageContent": func(codePoint uint64) error {
		_, _, err := unmarshalPrivateMessageContent(handDerivedAuthDataGolden(ContentTypeCommit),
			&PrivateMessage{ContentType: ContentType(codePoint)},
			Sender{SenderType: SenderTypeMember})
		return err
	},
}

// undeclaredCodePointsOf is every code point of a framing registry's width that the registry
// does not declare, derived off the constants the package holds and off the type's own width
// rather than off either one alone. The reserved zero is one of them and is not special.
func undeclaredCodePointsOf(t *testing.T, registry string) []uint64 {
	t.Helper()
	bits := framingRegistryBits(t, registry)
	if bits > 16 {
		t.Fatalf("%s is %d bits wide, and sweeping its whole code point space is not what this helper is for", registry, bits)
	}
	held := map[uint64]bool{}
	for _, value := range registryConstantsOfType(t, registry) {
		held[value] = true
	}
	found := []uint64{}
	for candidate := uint64(0); candidate < uint64(1)<<bits; candidate += 1 {
		if held[candidate] {
			continue
		}
		found = append(found, candidate)
	}
	if len(found)+len(held) != 1<<bits {
		t.Fatalf("the derivation split the %d code points of %s into %d declared and %d undeclared",
			1<<bits, registry, len(held), len(found))
	}
	return found
}

// namesTheNumber reports whether a message spells value as a decimal number standing on its own
// rather than as part of a longer one.
//
// The boundary check is what makes this an assertion. A plain substring search for "1" is
// answered by a message naming 13, 21 or 100, so a refusal reporting the WRONG code point --
// which is what a loop reading the wrong index produces, and the failure extension_test.go
// already guards its own registry against -- would pass it. The format is deliberately not
// pinned: naming the value is the property, and "%d" against "content type %d" is a difference
// of prose rather than of what the caller is told.
func namesTheNumber(message string, value uint64) bool {
	want := strconv.FormatUint(value, 10)
	for at := 0; at+len(want) <= len(message); at += 1 {
		found := strings.Index(message[at:], want)
		if found < 0 {
			return false
		}
		found += at
		before := found == 0 || !isDecimalDigit(message[found-1])
		after := found+len(want) == len(message) || !isDecimalDigit(message[found+len(want)])
		if before && after {
			return true
		}
		at = found
	}
	return false
}

func isDecimalDigit(octet byte) bool {
	return octet >= '0' && octet <= '9'
}

// TestTheNumberNamingReaderReadsAWholeNumberAndNotASubstring is that reader's control, in both
// directions for the reason every reader in this file carries one.
func TestTheNumberNamingReaderReadsAWholeNumberAndNotASubstring(t *testing.T) {
	for _, one := range []struct {
		message string
		value   uint64
		names   bool
	}{
		{"mls: unknown sender type: 5", 5, true},
		{"mls: no ratchet for this content type: content type 0", 0, true},
		{"5", 5, true},
		{"mls: 5 is not a sender type", 5, true},
		// the refusal that names a neighbouring code point rather than the one it refused
		{"mls: unknown sender type: 15", 5, false},
		{"mls: unknown sender type: 51", 5, false},
		{"mls: unknown sender type: 155", 5, false},
		// and the refusal that has stopped naming any of them
		{"mls: unknown sender type", 5, false},
	} {
		if got := namesTheNumber(one.message, one.value); got != one.names {
			t.Errorf("%q read against %d as %v, want %v", one.message, one.value, got, one.names)
		}
	}
}

// TestEveryFramingCodePointRefusalNamesTheCodePointItRefused is the behavioural half of the gate
// above, swept over every code point each registry does not declare rather than over the one
// somebody thought of.
//
// The two halves are different claims and this project has been caught by the gap between them.
// The derived gate reads the SOURCE and says the refusal mentions its own discriminant; this one
// reads the MESSAGE the caller actually receives, which is where a format verb dropped from one
// of the two, or an argument naming a stale copy of the code point, shows up.
func TestEveryFramingCodePointRefusalNamesTheCodePointItRefused(t *testing.T) {
	switches := framingRegistrySwitches(t)
	derived, invoked := slices.Sorted(maps.Keys(switches)), slices.Sorted(maps.Keys(framingCodePointRefusals))
	if !slices.Equal(derived, invoked) {
		t.Fatalf("this package's production source refuses an unregistered framing code point in %v and this file invokes %v; a refusal nothing invokes is a message nobody has ever read",
			derived, invoked)
	}
	refused := 0
	for _, method := range invoked {
		registry := switches[method].registry
		for _, codePoint := range undeclaredCodePointsOf(t, registry) {
			err := framingCodePointRefusals[method](codePoint)
			if err == nil {
				t.Fatalf("%s accepted the %s code point %d, which the registry does not declare", method, registry, codePoint)
			}
			if !namesTheNumber(err.Error(), codePoint) {
				t.Fatalf("%s refused the unregistered %s %d with %q, which does not name it; the octet is what the caller has off the wire, and a refusal that will not repeat it leaves a rejected header nobody can attribute to a value",
					method, registry, codePoint, err)
			}
			refused += 1
		}
	}
	if refused == 0 {
		t.Fatal("no unregistered code point was refused, so this observed nothing")
	}
	t.Logf("%d unregistered code point refusals over %d methods, each naming the value it refused", refused, len(invoked))
}

// ---------------------------------------------------------------------------
// the compile time pins framing.go carries
// ---------------------------------------------------------------------------

// framingPin is one package level blank declaration of framing.go: the type it declares and the
// value assigned to it, as source text, and whether that type is a signature.
type framingPin struct {
	file     string
	declared string
	value    string
	isFunc   bool
}

// framingPins reads them off the file.
//
// A blank declaration with no declared TYPE is not collected, and that exclusion is the reader's
// whole point. `var _ = (*T).MarshalMLS` states nothing whatever -- it compiles for every
// signature that method could ever have -- so collecting it would let the gate below report a
// pin present where the statement inside it had been emptied out, which is the same survivor
// the width pin reader was written to close one section up.
func framingPins(t *testing.T) []framingPin {
	t.Helper()
	found := []framingPin{}
	for _, file := range slices.Compact(slices.Sorted(maps.Values(framingCodecMethodFiles(t)))) {
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, declaration := range parsed.Decls {
			generic, isGeneric := declaration.(*ast.GenDecl)
			if !isGeneric || (generic.Tok != token.VAR && generic.Tok != token.CONST) {
				continue
			}
			for _, spec := range generic.Specs {
				value, isValue := spec.(*ast.ValueSpec)
				if !isValue || value.Type == nil || len(value.Values) != len(value.Names) {
					continue
				}
				_, isFunc := value.Type.(*ast.FuncType)
				for at, name := range value.Names {
					if name.Name != "_" {
						continue
					}
					found = append(found, framingPin{
						file:     file,
						declared: types.ExprString(value.Type),
						value:    types.ExprString(value.Values[at]),
						isFunc:   isFunc,
					})
				}
			}
		}
	}
	return found
}

// syntaxCodecInterface is syntax.Codec as the type checker sees it, reached through this
// package's own import of it. Read through go/types rather than through reflect because the gate
// below asks about types NAMED in framing.go, which it has as strings off the syntax tree and
// cannot turn into values.
func syntaxCodecInterface(t *testing.T) *types.Interface {
	t.Helper()
	for _, imported := range typeCheckedPackage(t).Imports() {
		if imported.Path() != "github.com/urnetwork/connect/mls/syntax" {
			continue
		}
		declared, isType := imported.Scope().Lookup("Codec").(*types.TypeName)
		if !isType {
			t.Fatal("package syntax declares no Codec, so the pin gate cannot say which codecs can satisfy it")
		}
		asInterface, isInterface := declared.Type().Underlying().(*types.Interface)
		if !isInterface {
			t.Fatal("syntax.Codec is not an interface")
		}
		return asInterface
	}
	t.Fatal("package mls does not import package syntax, so syntax.Codec cannot be read from the type check")
	return nil
}

// TestEveryFramingCodecCarriesTheCompileTimePinItsShapeAllows is the presence guard framing.go's
// own pins never had.
//
// key_schedule_deps_test.go's TestNoPinBlockShrinksWithoutFailing is this package's guard against
// a detector being deleted along with the thing it detected, and its class is TEST files: it
// skips every name that does not end in _test.go. framing.go's pins are production source, one
// file type outside that class, so the two lines standing in for the var _ syntax.Codec this
// codec cannot have were counted by nothing and could both be deleted with the package green.
// That is measured, not supposed.
//
// What is required of each codec is derived from its own SHAPE rather than from a count. A type
// whose methods fit syntax.Codec owes the one interface pin. A type whose methods cannot fit it
// -- because the content type is a PARAMETER, which is registry section 7.2's decision and the
// reason this codec exists in the form it does -- owes a signature pin per method, that being
// the only remaining form a narrowing to Codec's shape stops compiling against.
func TestEveryFramingCodecCarriesTheCompileTimePinItsShapeAllows(t *testing.T) {
	pins := framingPins(t)
	codec := syntaxCodecInterface(t)
	scope := typeCheckedPackage(t).Scope()
	files := framingCodecMethodFiles(t)
	held := 0
	for _, method := range framingCodecMethods(t) {
		receiver, name, _ := strings.Cut(method, ".")
		file := files[method]
		declared, isType := scope.Lookup(receiver).(*types.TypeName)
		if !isType {
			t.Fatalf("%s declares the method %s and package mls holds no type %s", file, method, receiver)
		}
		// the pin has to stand in the file that declares the codec and not merely somewhere
		// in the derived set. A pin pooled across files would let one file's blank
		// declaration answer for a codec declared in another, which is a build time
		// statement the reader of that file cannot see.
		if types.Implements(types.NewPointer(declared.Type()), codec) {
			want := fmt.Sprintf("(*%s)(nil)", receiver)
			if !slices.ContainsFunc(pins, func(pin framingPin) bool {
				return pin.file == file && pin.value == want && pin.declared == "syntax.Codec"
			}) {
				t.Errorf("*%s satisfies syntax.Codec and %s declares no blank syntax.Codec pin assigned %s, so the day its methods drift out of that interface nothing says so at build time",
					receiver, file, want)
				continue
			}
			held += 1
			continue
		}
		want := fmt.Sprintf("(*%s).%s", receiver, name)
		if !slices.ContainsFunc(pins, func(pin framingPin) bool {
			return pin.file == file && pin.value == want && pin.isFunc
		}) {
			t.Errorf("*%s cannot satisfy syntax.Codec -- %s takes the discriminant registry section 7.2 keeps out of the struct -- and %s declares no blank pin of a signature type assigned %s, so a later task that stores that discriminant in a field and narrows this method to one argument compiles clean",
				receiver, name, file, want)
			continue
		}
		held += 1
	}
	if held == 0 {
		t.Fatal("no codec pin was matched, so this gate states nothing")
	}
	t.Logf("%d codec methods, each pinned in the form its own shape allows", held)
}

// ---------------------------------------------------------------------------
// the receiver discipline every framing decoder that publishes whole owes
// ---------------------------------------------------------------------------

// publishesItsReceiverWhole reports whether a decoder writes its receiver ONLY as a whole --
// one or more `*self = ...` and never a `self.Field = ...`.
//
// The cut is the assignment target and not the shape of the right hand side, because the target
// is what the property is about. A decoder that only ever assigns the whole receiver has one
// moment at which the caller's value changes, so "untouched unless the decode succeeded" is a
// statement that can be true of it. FramedContent and Proposal deliberately do the other thing:
// they reset the receiver part way through and then fill an arm into it with `self.Proposal =`,
// because their arms write different fields and a fully staged copy would be a second value
// built only to be copied over the first. Those two make no such promise and are not swept.
//
// A first pass at this cut read the right hand side instead -- a NAMED local staged into the
// receiver -- and it split the class wrongly: FramedContentAuthData publishes
// `*self = FramedContentAuthData{...}` after every read has succeeded, which is the property
// exactly, and a reader looking for an identifier dropped it. What survives the mutation this
// gate is here for is unaffected either way: an `*self = decoded` inserted ahead of the reads
// leaves the method assigning only whole receivers, so it stays in the class and is caught by
// the probe rather than quietly leaving it.
func publishesItsReceiverWhole(body *ast.BlockStmt) bool {
	whole, field := false, false
	ast.Inspect(body, func(node ast.Node) bool {
		assign, isAssign := node.(*ast.AssignStmt)
		if !isAssign {
			return true
		}
		for _, target := range assign.Lhs {
			switch typed := target.(type) {
			case *ast.StarExpr:
				if name, isName := typed.X.(*ast.Ident); isName && name.Name == "self" {
					whole = true
				}
			case *ast.SelectorExpr:
				if name, isName := typed.X.(*ast.Ident); isName && name.Name == "self" {
					field = true
				}
			}
		}
		return true
	})
	return whole && !field
}

// TestTheWholeReceiverReaderSeparatesADecodeThatPublishesOnceFromOneThatFillsFields is that
// reader's control, in both directions for the reason every reader in this file carries one.
// One that recognised everything would put FramedContent in the class and report the sweep
// below as covering a decoder that never promised this; one that recognised nothing would empty
// the class out and the sweep would report green having probed nobody.
func TestTheWholeReceiverReaderSeparatesADecodeThatPublishesOnceFromOneThatFillsFields(t *testing.T) {
	for _, one := range []struct {
		source    string
		publishes bool
	}{
		{"func (self *T) UnmarshalMLS(r *syntax.Reader) error { decoded := T{}; *self = decoded; return nil }", true},
		{"func (self *T) UnmarshalMLS(r *syntax.Reader) error { *self = T{A: 1}; return nil }", true},
		{"func (self *T) UnmarshalMLS(r *syntax.Reader) error { d := T{}; d.A = 1; *self = d; return nil }", true},
		// the reset and fill, which is a different promise and is not this one
		{"func (self *T) UnmarshalMLS(r *syntax.Reader) error { *self = T{A: 1}; self.B = 2; return nil }", false},
		{"func (self *T) UnmarshalMLS(r *syntax.Reader) error { self.A = 1; return nil }", false},
		// and a decoder that fills a local and never publishes it at all
		{"func (self *T) UnmarshalMLS(r *syntax.Reader) error { d := T{}; d.A = 1; return nil }", false},
	} {
		parsed, err := parser.ParseFile(token.NewFileSet(), "control.go",
			"package control\n"+one.source+"\n", parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %q: %v", one.source, err)
		}
		function, isFunction := parsed.Decls[0].(*ast.FuncDecl)
		if !isFunction {
			t.Fatalf("%q did not parse as a function declaration", one.source)
		}
		if got := publishesItsReceiverWhole(function.Body); got != one.publishes {
			t.Errorf("%q read as publishing whole=%v, want %v", one.source, got, one.publishes)
		}
	}
}

// framingDecodersThatPublishWhole is that class over this package's framing decoders, derived
// off the assignments rather than listed.
//
// Two of the four had a behavioural test for the property and two did not, which is rule 5's
// exact shape: the property was observed where somebody had thought of it rather than wherever
// it is promised. AuthenticatedContent.UnmarshalMLS could be made to assign *self BEFORE it read
// the content -- leaving a refused decode holding a wire format out of bytes the package refused,
// over whatever the caller's value held before -- with the whole of ./mls/... and ./message/...
// green. Measured. confirmedTranscriptHashInput.UnmarshalMLS publishes whole, documents that it
// does, and had nothing at all.
func framingDecodersThatPublishWhole(t *testing.T) []string {
	t.Helper()
	found := []string{}
	for _, function := range framingRegistryFunctions(t) {
		_, method, isMethod := strings.Cut(function.name, ".")
		if !isMethod || method != "UnmarshalMLS" || !publishesItsReceiverWhole(function.decl.Body) {
			continue
		}
		found = append(found, function.name)
	}
	slices.Sort(found)
	if len(found) == 0 {
		t.Fatal("no framing decoder was read as publishing its receiver whole, and this package holds several, so the sweep below would run over the empty set")
	}
	return found
}

// framingReceiverProbe is one decoder of the class: what a caller's receiver already holds, a
// complete encoding that decoder accepts, the decode itself, and the refusals it has that are
// not a matter of running out of input.
//
// The refusal POSITIONS are deliberately not listed. The gate below truncates the complete
// encoding at EVERY length short of the whole, which refuses once at every field boundary the
// decoder has and several times inside each -- so a receiver stamped anywhere along the decode
// is seen, wherever along it that is. A hand written list of refusal points is not the same
// claim and this project has the measurement: the first version of this gate handed each
// decoder one refused input, taken from inside the FramedContent, and reported green against
// `*self = decoded` inserted one field LATER, ahead of the auth data read. Every stamp that
// gate could see was a stamp somebody had already thought to look for.
//
// The prior contents matter as much as the refusal. A receiver left holding its own zero value
// is not untouched -- it is a decoder that wiped a caller's value on the way to refusing -- and
// a probe starting from the zero value cannot tell the two apart. The gate checks that the
// complete encoding decodes to something DIFFERENT from what the receiver held, so "stamped" and
// "left alone" are distinguishable states before any refusal is tried.
type framingReceiverProbe struct {
	prior  func() any
	valid  func(t *testing.T) []byte
	decode func(into any, bs []byte) error
	// other is every refusal this decoder has that a truncation cannot produce: an unregistered
	// code point, or a discriminant handed in rather than read. Each is a whole decode rather
	// than an input, because two of these decoders take their discriminant as a PARAMETER and a
	// refusal of it is not expressible as bytes.
	other func(t *testing.T) []func(into any) error
}

// framingProbeCommit is a second authenticated commit, differing from testAuthenticatedCommit in
// every field a stamp could copy across. It is what the two wide probes below decode, so that a
// receiver holding the first and stamped with any part of the second is visibly not the first.
func framingProbeCommit() AuthenticatedContent {
	other := testAuthenticatedCommit()
	other.WireFormat = WireFormatPrivateMessage
	other.Content.GroupId = []byte{0x5a, 0x5b}
	other.Content.Epoch = 9
	other.Content.Sender = Sender{SenderType: SenderTypeExternal, SenderIndex: 4}
	other.Auth.Signature = []byte{0x11, 0x22, 0x33}
	other.Auth.ConfirmationTag = []byte{0x44}
	return other
}

// framingProbeEncoding is that value's encoding. It is produced by the encoder under test, which
// is right here and wrong in the goldens above: nothing is being claimed about these octets --
// they are a supply of prefixes -- and the gate's own control refuses to proceed unless the
// decoder accepts the whole of them.
func framingProbeEncoding(t *testing.T, codec syntax.Codec) []byte {
	t.Helper()
	encoded, err := syntax.Marshal(codec)
	if err != nil {
		t.Fatalf("encode the value this probe truncates: %v", err)
	}
	return encoded
}

var framingWholeReceiverProbes = map[string]framingReceiverProbe{
	// section 6's outermost structure, whose two refusals stand at the FIRST two fields -- which
	// is exactly where a decoder that assigned as it read would have stamped a version and a
	// wire format out of a frame this package refused. The prior value and the encoding differ in
	// the arm as well as in the discriminant, so a receiver stamped with either is visibly not
	// the one it held.
	"MLSMessage.UnmarshalMLS": {
		prior: func() any {
			return &MLSMessage{Version: ProtocolVersionMls10, WireFormat: WireFormatPrivateMessage,
				PrivateMessage: framingTestPrivateMessage()}
		},
		valid: func(t *testing.T) []byte {
			return framingProbeEncoding(t, framingTestMlsMessage(t, WireFormatPublicMessage))
		},
		decode: func(into any, bs []byte) error {
			return into.(*MLSMessage).UnmarshalMLS(syntax.NewReader(bs))
		},
		other: func(t *testing.T) []func(into any) error {
			unregistered := uint64(undeclaredCodePointsOf(t, "WireFormat")[0])
			return []func(into any) error{
				// an unregistered wire format, refused at the second field with a whole valid
				// message standing behind it
				func(into any) error {
					return into.(*MLSMessage).UnmarshalMLS(syntax.NewReader(append(
						[]byte{0x00, 0x01, byte(unregistered >> 8), byte(unregistered)},
						handDerivedPrivateMessageTail()...)))
				},
				// a protocol version this profile does not implement, refused at the FIRST field
				func(into any) error {
					return into.(*MLSMessage).UnmarshalMLS(syntax.NewReader(append(
						[]byte{0x00, 0x02, 0x00, 0x02}, handDerivedPrivateMessageTail()...)))
				},
				// and a registered wire format over a content type no registry declares, refused
				// from strictly behind the four octets this decoder would stamp
				func(into any) error {
					return into.(*MLSMessage).UnmarshalMLS(syntax.NewReader(append(
						[]byte{0x00, 0x01}, framingRefusedContentBehind(t, WireFormatPublicMessage)...)))
				},
			}
		},
	},
	"Sender.UnmarshalMLS": {
		prior:  func() any { return testSenderOfType(SenderTypeMember) },
		valid:  func(t *testing.T) []byte { return handDerivedSenderGolden(SenderTypeExternal) },
		decode: func(into any, bs []byte) error { return into.(*Sender).UnmarshalMLS(syntax.NewReader(bs)) },
		other: func(t *testing.T) []func(into any) error {
			refused := []byte{byte(undeclaredCodePointsOf(t, "SenderType")[0]), 0x00, 0x00, 0x00, 0x00}
			return []func(into any) error{
				func(into any) error { return into.(*Sender).UnmarshalMLS(syntax.NewReader(refused)) },
			}
		},
	},
	"FramedContentAuthData.UnmarshalMLS": {
		prior: func() any { return emptySignatureAuthData([]byte{0x7a}) },
		valid: func(t *testing.T) []byte { return handDerivedAuthDataGolden(ContentTypeCommit) },
		decode: func(into any, bs []byte) error {
			return into.(*FramedContentAuthData).UnmarshalMLS(syntax.NewReader(bs), ContentTypeCommit)
		},
		other: func(t *testing.T) []func(into any) error {
			unregistered := ContentType(undeclaredCodePointsOf(t, "ContentType")[0])
			return []func(into any) error{
				// the discriminant this codec is HANDED rather than reads, which no truncation
				// of the input can produce
				func(into any) error {
					return into.(*FramedContentAuthData).UnmarshalMLS(
						syntax.NewReader(handDerivedAuthDataGolden(ContentTypeCommit)), unregistered)
				},
				// and a commit whose confirmation tag is present, wire legal and empty
				func(into any) error {
					return into.(*FramedContentAuthData).UnmarshalMLS(
						syntax.NewReader([]byte{0x01, 0xaa, 0x00}), ContentTypeCommit)
				},
			}
		},
	},
	"AuthenticatedContent.UnmarshalMLS": {
		prior: func() any { value := testAuthenticatedCommit(); return &value },
		valid: func(t *testing.T) []byte {
			value := framingProbeCommit()
			return framingProbeEncoding(t, &value)
		},
		decode: func(into any, bs []byte) error {
			return into.(*AuthenticatedContent).UnmarshalMLS(syntax.NewReader(bs))
		},
		other: func(t *testing.T) []func(into any) error {
			return []func(into any) error{
				// an unregistered wire format, refused at the first field
				func(into any) error {
					return into.(*AuthenticatedContent).UnmarshalMLS(syntax.NewReader(append(
						[]byte{0x00, 0x00}, handDerivedCommitTail()...)))
				},
				// and a registered one over a content type no registry declares, refused from
				// strictly behind the octets a wire format decoder would stamp
				func(into any) error {
					return into.(*AuthenticatedContent).UnmarshalMLS(
						syntax.NewReader(framingRefusedContentBehind(t, WireFormatPrivateMessage)))
				},
			}
		},
	},
	// section 6.2's cleartext message, whose decode reads a field the SENDER TYPE selects. The
	// prior value and the encoding differ in the arm as well as in every field, so a receiver
	// stamped with either is visibly not the one it held.
	"PublicMessage.UnmarshalMLS": {
		prior: func() any { return framingTestPublicMessage() },
		valid: func(t *testing.T) []byte {
			other := framingTestPublicMessage()
			other.Content = *framingTestCommitContent()
			other.Content.GroupId = []byte{0x5a, 0x5b}
			other.Content.Epoch = 9
			other.Content.Sender = Sender{SenderType: SenderTypeMember, LeafIndex: 4}
			other.Auth = FramedContentAuthData{Signature: []byte{0x11, 0x22, 0x33},
				ConfirmationTag: []byte{0x44}}
			other.MembershipTag = []byte{0x66, 0x77}
			return framingProbeEncoding(t, other)
		},
		decode: func(into any, bs []byte) error {
			return into.(*PublicMessage).UnmarshalMLS(syntax.NewReader(bs))
		},
		other: func(t *testing.T) []func(into any) error {
			unregistered := ContentType(undeclaredCodePointsOf(t, "ContentType")[0])
			return []func(into any) error{
				// a content type no registry declares, refused inside the framed content
				func(into any) error {
					return into.(*PublicMessage).UnmarshalMLS(
						syntax.NewReader(handDerivedFramedContentHeader(unregistered)))
				},
				// and a member's message whose membership tag is present, wire legal and empty,
				// which is a refusal raised at the very last field and so from behind every octet
				// a stamp could copy across
				func(into any) error {
					return into.(*PublicMessage).UnmarshalMLS(
						syntax.NewReader(framingPublicMessageWithEmptyTag(t)))
				},
			}
		},
	},
	// section 6.3's header, whose content type refusal stands after the group id and the epoch
	// have already been read -- which is exactly where a decoder that assigned as it read would
	// have stamped them.
	"PrivateMessage.UnmarshalMLS": {
		prior: func() any { return framingTestPrivateMessage() },
		valid: func(t *testing.T) []byte {
			return framingProbeEncoding(t, &PrivateMessage{
				GroupId:             []byte{0x5a, 0x5b},
				Epoch:               21,
				ContentType:         ContentTypeCommit,
				AuthenticatedData:   []byte{0x11, 0x22},
				EncryptedSenderData: []byte{0x33},
				Ciphertext:          []byte{0x44, 0x55},
			})
		},
		decode: func(into any, bs []byte) error {
			return into.(*PrivateMessage).UnmarshalMLS(syntax.NewReader(bs))
		},
		other: func(t *testing.T) []func(into any) error {
			unregistered := ContentType(undeclaredCodePointsOf(t, "ContentType")[0])
			return []func(into any) error{
				func(into any) error {
					return into.(*PrivateMessage).UnmarshalMLS(
						syntax.NewReader(handDerivedPrivateMessageHeader(unregistered)))
				},
			}
		},
	},
	"confirmedTranscriptHashInput.UnmarshalMLS": {
		prior: func() any { commit := testAuthenticatedCommit(); return commit.transcriptHashInput() },
		valid: func(t *testing.T) []byte {
			commit := framingProbeCommit()
			return framingProbeEncoding(t, commit.transcriptHashInput())
		},
		decode: func(into any, bs []byte) error {
			return into.(*confirmedTranscriptHashInput).UnmarshalMLS(syntax.NewReader(bs))
		},
		other: func(t *testing.T) []func(into any) error {
			return []func(into any) error{
				func(into any) error {
					return into.(*confirmedTranscriptHashInput).UnmarshalMLS(syntax.NewReader(append(
						[]byte{0x00, 0x00}, handDerivedTranscriptInputTail()...)))
				},
				func(into any) error {
					return into.(*confirmedTranscriptHashInput).UnmarshalMLS(
						syntax.NewReader(framingRefusedContentBehind(t, WireFormatPrivateMessage)))
				},
			}
		},
	},
}

// framingRefusedContentBehind is a registered wire format followed by a FramedContent this
// package will not read: the two octets a wire format decoder stamps its receiver with, and
// then a refusal raised strictly after them.
func framingRefusedContentBehind(t *testing.T, wireFormat WireFormat) []byte {
	t.Helper()
	unregistered := ContentType(undeclaredCodePointsOf(t, "ContentType")[0])
	return append([]byte{byte(wireFormat >> 8), byte(wireFormat)},
		handDerivedFramedContentHeader(unregistered)...)
}

// TestEveryFramingDecodeThatPublishesItsReceiverWholeLeavesItUntouchedWhenItRefuses sweeps the
// class rather than the two members of it somebody wrote a test for, and sweeps every refusal
// point of each rather than the one somebody thought of.
//
// What a violation costs is not a mangled struct anybody would notice. It is a well formed value
// describing a message nobody sent: a caller that reused its receiver and checked the error is
// left holding fields read out of input this package refused, in a value that decodes, prints
// and re-encodes as though a peer had sent it.
func TestEveryFramingDecodeThatPublishesItsReceiverWholeLeavesItUntouchedWhenItRefuses(t *testing.T) {
	derived := framingDecodersThatPublishWhole(t)
	probed := slices.Sorted(maps.Keys(framingWholeReceiverProbes))
	if !slices.Equal(derived, probed) {
		t.Fatalf("this package publishes its receiver whole in %v and this file probes %v; a decoder nothing probes is a promise nobody has ever held it to",
			derived, probed)
	}
	observed := 0
	for _, method := range probed {
		probe := framingWholeReceiverProbes[method]
		valid := probe.valid(t)
		// the two controls this sweep owes, both before any refusal is tried. The complete
		// encoding must be ACCEPTED, or every truncation of it below refuses something this
		// probe broke rather than a short input; and it must decode to something OTHER than
		// what the receiver already held, or a stamped receiver and an untouched one are the
		// same value and nothing below could separate them.
		accepted := probe.prior()
		if err := probe.decode(accepted, valid); err != nil {
			t.Errorf("%s refused the complete encoding this sweep truncates: %v", method, err)
			continue
		}
		if reflect.DeepEqual(probe.prior(), accepted) {
			t.Errorf("%s decoded the complete encoding to the value its receiver already held, so this probe cannot tell a stamped receiver from an untouched one",
				method)
			continue
		}
		refusals := []func(into any) error{}
		for at := 0; at < len(valid); at += 1 {
			short := valid[:at]
			refusals = append(refusals, func(into any) error { return probe.decode(into, short) })
		}
		refusals = append(refusals, probe.other(t)...)
		for index, refuse := range refusals {
			before, after := probe.prior(), probe.prior()
			err := refuse(after)
			if err == nil {
				t.Errorf("%s: refusal %d of %d was accepted, so what it left in the receiver states nothing",
					method, index, len(refusals))
				continue
			}
			if !reflect.DeepEqual(before, after) {
				t.Errorf("%s: refusal %d of %d answered %v and left its receiver holding %+v rather than the %+v it held going in; a refused decode that writes the receiver hands the caller a well formed value assembled out of bytes this package would not accept",
					method, index, len(refusals), err, after, before)
				continue
			}
			observed += 1
		}
	}
	if observed == 0 {
		t.Fatal("no refusal was observed, so this gate states nothing")
	}
	t.Logf("%d framing decoders publishing their receiver whole, %d refusals swept, each leaving the receiver untouched",
		len(probed), observed)
}

// ---------------------------------------------------------------------------
// the sentinel this file's codec borrows
// ---------------------------------------------------------------------------

// filesRaising is every production file of this package whose SYNTAX TREE names a symbol, less
// the file that declares it.
//
// Read as a tree rather than as text, so a file that only discusses the name in prose is not
// counted as a raiser of it -- framing_errors.go's package comment spends nine lines on
// ErrUnknownContentType and returns it nowhere, which is exactly the false positive a grep would
// report.
func filesRaising(t *testing.T, name string) []string {
	t.Helper()
	declaredIn := packageLevelDeclarations(t, ".")[name]
	if declaredIn == "" {
		t.Fatalf("package mls declares no %s, so where it is raised cannot be derived", name)
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	found := []string{}
	fileSet := token.NewFileSet()
	for _, entry := range entries {
		fileName := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(fileName, ".go") ||
			strings.HasSuffix(fileName, "_test.go") || fileName == declaredIn {
			continue
		}
		parsed, err := parser.ParseFile(fileSet, fileName, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", fileName, err)
		}
		raises := false
		ast.Inspect(parsed, func(node ast.Node) bool {
			if identifier, isName := node.(*ast.Ident); isName && identifier.Name == name {
				raises = true
			}
			return !raises
		})
		if raises {
			found = append(found, fileName)
		}
	}
	slices.Sort(found)
	return found
}

// docCommentOfDeclaration is the comment written above one package level declaration, taken from
// the spec where a grouped block gives each name its own comment and from the block otherwise.
func docCommentOfDeclaration(t *testing.T, file string, name string) string {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	for _, declaration := range parsed.Decls {
		generic, isGeneric := declaration.(*ast.GenDecl)
		if !isGeneric {
			continue
		}
		for _, spec := range generic.Specs {
			value, isValue := spec.(*ast.ValueSpec)
			if !isValue || !slices.ContainsFunc(value.Names, func(each *ast.Ident) bool { return each.Name == name }) {
				continue
			}
			if value.Doc != nil {
				return value.Doc.Text()
			}
			if generic.Doc != nil {
				return generic.Doc.Text()
			}
			return ""
		}
	}
	t.Fatalf("%s declares no %s", file, name)
	return ""
}

// TestTheBorrowedSentinelIsDocumentedByEveryLayerThatRaisesIt is the other half of
// TestEveryStructuralFramingErrorHasExactlyOneDeclarationSite.
//
// That gate says ErrUnknownContentType has ONE declaration site and names it, which is the
// decision this task took rather than shadowing the secret tree's sentinel. It says nothing
// about what that single site TELLS a reader. The declaration was written when the ratchet
// lookup was the only thing that raised it, and the framing codec now raises it off the wire
// where there is no ratchet in view at all -- so the comment a reader arrives at, and the
// message a caller logs, described one of the two layers and not the other.
//
// The class is derived: every production file whose syntax tree names the sentinel must be named
// by the comment on its declaration. A third layer that starts raising it fails here on the
// commit that does so, which is the only moment anybody has the reason in their head.
func TestTheBorrowedSentinelIsDocumentedByEveryLayerThatRaisesIt(t *testing.T) {
	const borrowed = "ErrUnknownContentType"
	declaredIn := packageLevelDeclarations(t, ".")[borrowed]
	raisers := filesRaising(t, borrowed)
	if len(raisers) < 2 {
		t.Fatalf("%s is declared in %s and raised from %v; this gate exists because it is raised from more than one layer, and a derivation finding fewer is reading the wrong thing",
			borrowed, declaredIn, raisers)
	}
	doc := docCommentOfDeclaration(t, declaredIn, borrowed)
	if doc == "" {
		t.Fatalf("%s is declared in %s with no comment at all", borrowed, declaredIn)
	}
	for _, file := range raisers {
		if !strings.Contains(doc, file) {
			t.Errorf("%s raises %s and the comment on its declaration in %s does not name %s, so a reader arriving at the sentinel is told about some of the layers that return it and not this one",
				file, borrowed, declaredIn, file)
		}
	}
	t.Logf("%s is declared in %s and raised from %v, each of them named by its declaration", borrowed, declaredIn, raisers)
}

// ---------------------------------------------------------------------------
// FramedContent
// ---------------------------------------------------------------------------

func TestFramedContentRoundTripApplication(t *testing.T) {
	content := FramedContent{
		GroupId:           []byte{0xaa, 0xbb},
		Epoch:             5,
		Sender:            Sender{SenderType: SenderTypeMember, LeafIndex: 1},
		AuthenticatedData: []byte{0x01, 0x02, 0x03},
		ContentType:       ContentTypeApplication,
		ApplicationData:   []byte("hello"),
	}
	encoded, err := syntax.Marshal(&content)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	decoded := FramedContent{}
	if err := syntax.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	reencoded, err := syntax.Marshal(&decoded)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if !bytes.Equal(encoded, reencoded) {
		t.Fatalf("re-encoded %x, want %x", reencoded, encoded)
	}
	if !bytes.Equal(decoded.ApplicationData, []byte("hello")) {
		t.Fatalf("application data %q", decoded.ApplicationData)
	}
}

func TestFramedContentRoundTripProposal(t *testing.T) {
	content := FramedContent{
		GroupId:     []byte{0x01},
		Epoch:       0,
		Sender:      Sender{SenderType: SenderTypeMember, LeafIndex: 3},
		ContentType: ContentTypeProposal,
		Proposal: &Proposal{
			ProposalType: ProposalTypeRemove,
			Remove:       &Remove{Removed: 2},
		},
	}
	encoded, err := syntax.Marshal(&content)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	decoded := FramedContent{}
	if err := syntax.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Proposal == nil || decoded.Proposal.Remove == nil ||
		decoded.Proposal.Remove.Removed != 2 {
		t.Fatalf("decoded proposal %+v", decoded.Proposal)
	}
}

func TestFramedContentRejectsArmMismatch(t *testing.T) {
	content := FramedContent{
		GroupId:         []byte{0x01},
		Sender:          Sender{SenderType: SenderTypeMember},
		ContentType:     ContentTypeApplication,
		ApplicationData: []byte("x"),
		Commit:          &Commit{},
	}
	// the refusal must survive syntax.Marshal, which joins the semantic error from MarshalMLS
	// with the Writer's sticky error
	if _, err := syntax.Marshal(&content); !errors.Is(err, ErrContentArmMismatch) {
		t.Fatalf("got %v, want ErrContentArmMismatch", err)
	}
}

func TestFramedContentRejectsUnknownContentType(t *testing.T) {
	content := FramedContent{
		GroupId:     []byte{0x01},
		Sender:      Sender{SenderType: SenderTypeMember},
		ContentType: ContentType(9),
	}
	if _, err := syntax.Marshal(&content); !errors.Is(err, ErrUnknownContentType) {
		t.Fatalf("got %v, want ErrUnknownContentType", err)
	}
}

// The encoder refuses before it writes, which framing.go argues for at length and which nothing
// else in this file observes for FramedContent.
//
// A FramedContent is the signature preimage and the transcript hash preimage, so a half written
// one that a caller ignored the error from is a preimage shorter than the message it claims to
// describe. The Writer is handed to the codec directly rather than through syntax.Marshal,
// because syntax.Marshal builds its own Writer and hands back nothing when it refuses -- which
// is precisely the octets this test is about.
func TestFramedContentWritesNothingBeforeItRefuses(t *testing.T) {
	content := FramedContent{
		GroupId:         bytes.Repeat([]byte{0xaa}, 8),
		Epoch:           5,
		Sender:          Sender{SenderType: SenderTypeMember, LeafIndex: 1},
		ContentType:     ContentTypeApplication,
		ApplicationData: []byte("x"),
		Commit:          &Commit{},
	}
	w := syntax.NewWriter()
	if err := content.MarshalMLS(w); !errors.Is(err, ErrContentArmMismatch) {
		t.Fatalf("got %v, want ErrContentArmMismatch", err)
	}
	if w.Len() != 0 {
		t.Fatalf("the refusal left %d octets on the caller's Writer; a FramedContent is the tail of nothing, so those octets are the front of a preimage that describes no message",
			w.Len())
	}
}

// ---------------------------------------------------------------------------
// AuthenticatedContent, the transcript hash input and the proposal reference
// ---------------------------------------------------------------------------

// newTestCrypto is one crypto provider constructor for this file's framing tests.
//
// It delegates to crypto_test.go's mustProvider rather than calling NewCryptoProvider again:
// two spellings of "the provider these tests run at" is two places for the suite to be chosen,
// and the suite is what decides KDF.Nh and therefore the width of every reference below.
func newTestCrypto(t *testing.T) CryptoProvider {
	t.Helper()
	return mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
}

// testAuthenticatedCommit is a commit carrying AuthenticatedContent, built once so the tests
// below vary one thing each rather than each declaring a slightly different value.
func testAuthenticatedCommit() AuthenticatedContent {
	return AuthenticatedContent{
		WireFormat: WireFormatPublicMessage,
		Content: FramedContent{
			GroupId:     []byte{0x07},
			Epoch:       2,
			Sender:      Sender{SenderType: SenderTypeMember, LeafIndex: 1},
			ContentType: ContentTypeCommit,
			Commit:      &Commit{},
		},
		Auth: FramedContentAuthData{
			Signature:       []byte{0xde, 0xad},
			ConfirmationTag: []byte{0xbe, 0xef},
		},
	}
}

// testAuthenticatedProposal is a proposal carrying AuthenticatedContent, for the reference tests.
func testAuthenticatedProposal() AuthenticatedContent {
	return AuthenticatedContent{
		WireFormat: WireFormatPublicMessage,
		Content: FramedContent{
			GroupId:     []byte{0x07},
			Epoch:       2,
			Sender:      Sender{SenderType: SenderTypeMember, LeafIndex: 1},
			ContentType: ContentTypeProposal,
			Proposal:    &Proposal{ProposalType: ProposalTypeRemove, Remove: &Remove{Removed: 2}},
		},
		Auth: FramedContentAuthData{Signature: []byte{0xde, 0xad}},
	}
}

func TestAuthenticatedContentRoundTrip(t *testing.T) {
	authContent := testAuthenticatedCommit()
	encoded, err := syntax.Marshal(&authContent)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if encoded[0] != 0x00 || encoded[1] != 0x01 {
		t.Fatalf("wire format prefix %x, want 0001", encoded[0:2])
	}

	decoded := AuthenticatedContent{}
	if err := syntax.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	reencoded, err := syntax.Marshal(&decoded)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if !bytes.Equal(encoded, reencoded) {
		t.Fatalf("re-encoded %x, want %x", reencoded, encoded)
	}
}

func TestConfirmedTranscriptHashInputOmitsConfirmationTag(t *testing.T) {
	authContent := testAuthenticatedCommit()
	authContent.WireFormat = WireFormatPrivateMessage
	input, err := authContent.ConfirmedTranscriptHashInput()
	if err != nil {
		t.Fatalf("input: %v", err)
	}

	w := syntax.NewWriter()
	w.WriteUint16(uint16(WireFormatPrivateMessage))
	if err := authContent.Content.MarshalMLS(w); err != nil {
		t.Fatalf("content: %v", err)
	}
	w.WriteOpaque(authContent.Auth.Signature)
	want, err := w.Bytes()
	if err != nil {
		t.Fatalf("bytes: %v", err)
	}
	if !bytes.Equal(input, want) {
		t.Fatalf("input %x, want %x", input, want)
	}
	if bytes.Contains(input, authContent.Auth.ConfirmationTag) {
		t.Fatal("confirmation tag leaked into ConfirmedTranscriptHashInput")
	}
}

func TestConfirmedTranscriptHashInputRefusesNonCommit(t *testing.T) {
	authContent := AuthenticatedContent{
		WireFormat: WireFormatPrivateMessage,
		Content: FramedContent{
			GroupId:         []byte{0x07},
			Sender:          Sender{SenderType: SenderTypeMember},
			ContentType:     ContentTypeApplication,
			ApplicationData: []byte("x"),
		},
		Auth: FramedContentAuthData{Signature: []byte{0x01}},
	}
	if _, err := authContent.ConfirmedTranscriptHashInput(); !errors.Is(err, ErrContentArmMismatch) {
		t.Fatalf("got %v, want ErrContentArmMismatch", err)
	}
}

// TestConfirmedTranscriptHashInputPrefixesTheSignatureAsAnMlsVector separates the two length
// prefixes this package can write, over the one input where the substitution is invisible.
//
// The signature enters the section 8.2 input as opaque<V>, section 2.1.2's varint: for a two
// octet signature that is the single octet 0x02. syntax's WriteOpaqueLP is the record layer's
// fixed thirty two bit prefix and would write 0x00000002 -- four octets where one belongs, three
// of them zero. Every member of a group running that code agrees with every other, the corpus is
// the only thing that disagrees, and the disagreement arrives as a permanent fork at the first
// commit shared with anybody else.
//
// The assertion is over the OCTETS between the framed content and the signature rather than over
// a golden blob, so it says which of the two prefixes was written rather than that the answer
// changed.
func TestConfirmedTranscriptHashInputPrefixesTheSignatureAsAnMlsVector(t *testing.T) {
	authContent := testAuthenticatedCommit()
	input, err := authContent.ConfirmedTranscriptHashInput()
	if err != nil {
		t.Fatalf("input: %v", err)
	}
	w := syntax.NewWriter()
	w.WriteUint16(uint16(authContent.WireFormat))
	if err := authContent.Content.MarshalMLS(w); err != nil {
		t.Fatalf("content: %v", err)
	}
	head, err := w.Bytes()
	if err != nil {
		t.Fatalf("bytes: %v", err)
	}
	if !bytes.HasPrefix(input, head) {
		t.Fatalf("the input %x does not open with the wire format and framed content %x", input, head)
	}
	signature := authContent.Auth.Signature

	varint := syntax.NewWriter()
	varint.WriteOpaque(signature)
	wantTail, err := varint.Bytes()
	if err != nil {
		t.Fatalf("varint bytes: %v", err)
	}
	recordLayer := syntax.NewWriter()
	recordLayer.WriteOpaqueLP(signature)
	unwantedTail, err := recordLayer.Bytes()
	if err != nil {
		t.Fatalf("record layer bytes: %v", err)
	}
	if bytes.Equal(wantTail, unwantedTail) {
		t.Fatalf("both prefixes wrote %x for a %d octet signature, so this test cannot separate them",
			wantTail, len(signature))
	}
	if tail := input[len(head):]; !bytes.Equal(tail, wantTail) {
		t.Fatalf("the signature was written as %x; the MLS opaque<V> is %x and the record layer's fixed prefix is %x",
			tail, wantTail, unwantedTail)
	}
}

// TestConfirmedTranscriptHashInputIsWhatThePublishedTranscriptChainConsumes is the assertion
// nothing in this package was making: that the input THIS plan builds is the input the key
// schedule plan's ConfirmedTranscriptHash was written to consume.
//
// The two halves were written by different plans against the same section and never met. p4 read
// transcript-hashes.json by SPLITTING the published AuthenticatedContent at a byte offset --
// len(blob) - (Nh + the width of that vector's own length prefix) -- because no framing type
// existed to parse it with, and both transcript_test.go and the family 7 runner say in as many
// words that when p6 lands (*AuthenticatedContent).UnmarshalMLS the offset is replaced by the
// parse. This is that landing, and this is the test that holds the two readings to each other
// before the offset stops being computed anywhere.
//
// Four things are compared per case, and they fail differently:
//
//   - the codec's ConfirmedTranscriptHashInput is byte for byte the offset split's head. A
//     preimage one octet longer or shorter than p4's is a transcript that forks.
//   - the confirmation tag the codec recovered is the offset split's tail, verified as
//     MAC(confirmation_key, confirmed_transcript_hash_after) -- the corpus's own step, which is
//     what makes the split honest rather than assumed.
//   - p4's own ConfirmedTranscriptHash over this plan's input is the PUBLISHED confirmed hash,
//     which is the end to end statement: this input, that function, the mlswg's answer.
//   - the serialized AuthenticatedContent re-encodes to the corpus's own octets, which is what
//     says the codec is canonical over bytes it did not produce -- and which drives Commit,
//     ProposalOrRef and the whole FramedContent decode over real published input rather than
//     over values this file built.
func TestConfirmedTranscriptHashInputIsWhatThePublishedTranscriptChainConsumes(t *testing.T) {
	entries := trPublishedEntries(t)
	reached := map[CipherSuite]int{}
	compared := 0
	for index, entry := range entries {
		suite := CipherSuite(entry.CipherSuite)
		if !IsRegisteredSuite(suite) {
			continue
		}
		at := fmt.Sprintf("%s entry %d suite %#04x", transcriptHashKatFile, index, uint16(suite))
		crypto := mustProvider(t, suite)
		blob := MustHex(t, entry.AuthenticatedContent)

		authContent := AuthenticatedContent{}
		if err := syntax.Unmarshal(blob, &authContent); err != nil {
			t.Fatalf("%s: the published authenticated_content did not decode: %v", at, err)
		}
		reencoded, err := syntax.Marshal(&authContent)
		if err != nil {
			t.Fatalf("%s: re-marshal: %v", at, err)
		}
		if !bytes.Equal(reencoded, blob) {
			t.Fatalf("%s: re-encoded %x, want the corpus's own %x", at, reencoded, blob)
		}

		head, tail := trSplitPublishedCommit(t, at, crypto, blob)
		input, err := authContent.ConfirmedTranscriptHashInput()
		if err != nil {
			t.Fatalf("%s: ConfirmedTranscriptHashInput: %v", at, err)
		}
		if !bytes.Equal(input, head) {
			t.Fatalf("%s: this plan builds the section 8.2 input as %x and the key schedule plan reads it out of the same blob as %x; the two halves of one transcript disagree",
				at, input, head)
		}
		if !bytes.Equal(authContent.Auth.ConfirmationTag, tail) {
			t.Fatalf("%s: the codec recovered the confirmation tag %x and the offset split recovered %x",
				at, authContent.Auth.ConfirmationTag, tail)
		}

		confirmed := ConfirmedTranscriptHash(crypto, MustHex(t, entry.InterimTranscriptHashBefore), input)
		if want := MustHex(t, entry.ConfirmedTranscriptHashAfter); !bytes.Equal(confirmed, want) {
			t.Fatalf("%s: ConfirmedTranscriptHash over this plan's input is %x, and the corpus publishes %x",
				at, confirmed, want)
		}
		// guardrail 8: the tag comparison is the provider's MacVerify and nothing spelled out
		// here. A split taken one octet out recovers bytes this refuses.
		if !crypto.MacVerify(MustHex(t, entry.ConfirmationKey), confirmed, authContent.Auth.ConfirmationTag) {
			t.Fatalf("%s: the confirmation tag the codec recovered does not verify as MAC(confirmation_key, confirmed_transcript_hash_after)",
				at)
		}
		reached[suite] += 1
		compared += 1
	}
	if compared == 0 {
		t.Fatalf("no case of %s was at a registered suite, so this compared nothing", transcriptHashKatFile)
	}
	for _, suite := range Suites() {
		if reached[suite] == 0 {
			t.Fatalf("%s carries no case at registered suite %#04x, so nothing above ran at it",
				transcriptHashKatFile, uint16(suite))
		}
	}
	t.Logf("%d published commits parsed, re-encoded and chained through the key schedule plan's own function", compared)
}

// TestTheSectionEightTwoInputDecodesBackToTheStructureItNames is the other direction of the
// preimage, over the corpus's own bytes.
//
// A preimage that only encodes is a preimage no test can hold against bytes it did not produce:
// the encode side is compared against the offset split next door, and both could be laying the
// fields out in an order that agrees with itself. Reading the head back as the section 8.2
// structure and finding the fields the corpus published is what says the layout is the RFC's.
func TestTheSectionEightTwoInputDecodesBackToTheStructureItNames(t *testing.T) {
	entries := trPublishedEntries(t)
	read := 0
	for index, entry := range entries {
		suite := CipherSuite(entry.CipherSuite)
		if !IsRegisteredSuite(suite) {
			continue
		}
		at := fmt.Sprintf("%s entry %d suite %#04x", transcriptHashKatFile, index, uint16(suite))
		crypto := mustProvider(t, suite)
		blob := MustHex(t, entry.AuthenticatedContent)
		head, _ := trSplitPublishedCommit(t, at, crypto, blob)

		input := confirmedTranscriptHashInput{}
		if err := syntax.Unmarshal(head, &input); err != nil {
			t.Fatalf("%s: the section 8.2 input did not decode: %v", at, err)
		}
		if input.Content.ContentType != ContentTypeCommit {
			t.Fatalf("%s: the published transcript entry decoded with content type %d, and section 8.2 chains over commits",
				at, input.Content.ContentType)
		}
		authContent := AuthenticatedContent{}
		if err := syntax.Unmarshal(blob, &authContent); err != nil {
			t.Fatalf("%s: authenticated_content: %v", at, err)
		}
		if input.WireFormat != authContent.WireFormat {
			t.Fatalf("%s: the input decoded wire format %d and the authenticated content %d",
				at, input.WireFormat, authContent.WireFormat)
		}
		if !bytes.Equal(input.Signature, authContent.Auth.Signature) {
			t.Fatalf("%s: the input decoded the signature %x and the authenticated content %x",
				at, input.Signature, authContent.Auth.Signature)
		}
		reencoded, err := syntax.Marshal(&input)
		if err != nil {
			t.Fatalf("%s: re-marshal: %v", at, err)
		}
		if !bytes.Equal(reencoded, head) {
			t.Fatalf("%s: the section 8.2 input re-encoded to %x, want %x", at, reencoded, head)
		}
		read += 1
	}
	if read == 0 {
		t.Fatalf("no case of %s was read back as the section 8.2 structure", transcriptHashKatFile)
	}
}

func TestProposalRefCoversTheWholeAuthenticatedContent(t *testing.T) {
	crypto := newTestCrypto(t)
	authContent := testAuthenticatedProposal()

	ref, err := authContent.ProposalRef(crypto)
	if err != nil {
		t.Fatalf("ref: %v", err)
	}
	if len(ref) != crypto.HashSize() {
		t.Fatalf("ref length %d, want %d", len(ref), crypto.HashSize())
	}
	encoded, err := syntax.Marshal(&authContent)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Equal(ref, mustProposalRef(t, crypto, encoded)) {
		t.Fatal("ProposalRef is not MakeProposalRef over the serialized AuthenticatedContent")
	}

	// the signature is inside the ref, so re-signing changes it
	resigned := authContent
	resigned.Auth.Signature = []byte{0xde, 0xae}
	other, err := resigned.ProposalRef(crypto)
	if err != nil {
		t.Fatalf("ref: %v", err)
	}
	if bytes.Equal(ref, other) {
		t.Fatal("ProposalRef does not cover the signature")
	}
}

func TestProposalRefRefusesNonProposal(t *testing.T) {
	crypto := newTestCrypto(t)
	authContent := AuthenticatedContent{
		WireFormat: WireFormatPrivateMessage,
		Content: FramedContent{
			GroupId:         []byte{0x07},
			Sender:          Sender{SenderType: SenderTypeMember},
			ContentType:     ContentTypeApplication,
			ApplicationData: []byte("x"),
		},
		Auth: FramedContentAuthData{Signature: []byte{0x01}},
	}
	if _, err := authContent.ProposalRef(crypto); !errors.Is(err, ErrContentArmMismatch) {
		t.Fatalf("got %v, want ErrContentArmMismatch", err)
	}
}

// TestProposalRefIsNotTakenOverASmallerStructureOrWithoutItsLabel is the half of "the ref is over
// the right bytes" that no round trip and no length check can see.
//
// A reference taken over the FramedContent rather than the AuthenticatedContent, over the
// Proposal alone, with the label dropped, or with the key package label instead, is a hash of
// exactly the right width, stable across runs, self consistent, and different from what every
// other implementation computes. The commit that names a proposal by one is a commit no peer can
// apply, and nothing about it looks wrong from inside.
//
// The four wrong inputs are held against the real one rather than against each other, and each
// is spelled out here as the input a plausible mistake would produce.
func TestProposalRefIsNotTakenOverASmallerStructureOrWithoutItsLabel(t *testing.T) {
	crypto := newTestCrypto(t)
	authContent := testAuthenticatedProposal()
	ref, err := authContent.ProposalRef(crypto)
	if err != nil {
		t.Fatalf("ref: %v", err)
	}
	wholeStructure, err := syntax.Marshal(&authContent)
	if err != nil {
		t.Fatalf("marshal the authenticated content: %v", err)
	}
	framedContent, err := syntax.Marshal(&authContent.Content)
	if err != nil {
		t.Fatalf("marshal the framed content: %v", err)
	}
	proposal, err := syntax.Marshal(authContent.Content.Proposal)
	if err != nil {
		t.Fatalf("marshal the proposal: %v", err)
	}
	for _, wrong := range []struct {
		why   string
		value []byte
	}{
		{"taken over the FramedContent, so it covers neither the wire format nor the signature",
			mustProposalRef(t, crypto, framedContent)},
		{"taken over the Proposal alone, so two members proposing the same removal share one reference",
			mustProposalRef(t, crypto, proposal)},
		{"taken with the label dropped, so a proposal reference and a key package reference over one blob collide",
			mustRefHash(t, crypto, "", wholeStructure)},
		{"taken with no RefHashInput at all, which is the digest with both length prefixes gone",
			crypto.Hash(wholeStructure)},
		{"taken with the key package label, which is the other half of section 5.2's domain separation",
			mustKeyPackageRef(t, crypto, wholeStructure)},
	} {
		if bytes.Equal(ref, wrong.value) {
			t.Errorf("ProposalRef equals the reference %s", wrong.why)
		}
	}
	if !bytes.Equal(ref, mustRefHash(t, crypto, ProposalRefLabel, wholeStructure)) {
		t.Fatal("ProposalRef is not RefHash over the serialized AuthenticatedContent under the proposal label, so the comparisons above are against the wrong baseline")
	}
}

// TestProposalRefIsTheProvidersWholeHash refuses a truncated reference.
//
// A ref cut short is still a ref: it is stable, it is the same on every run, and it collides
// with every other proposal sharing its prefix. The width is read off the provider rather than
// written down here, because both registered suites fix KDF.Nh at 32 and a literal 32 is right
// for both and wrong for the first suite this package adds.
func TestProposalRefIsTheProvidersWholeHash(t *testing.T) {
	crypto := newTestCrypto(t)
	authContent := testAuthenticatedProposal()
	ref, err := authContent.ProposalRef(crypto)
	if err != nil {
		t.Fatalf("ref: %v", err)
	}
	if len(ref) != crypto.HashSize() {
		t.Fatalf("the reference is %d octets and the provider's hash is %d", len(ref), crypto.HashSize())
	}
	// and a prefix of it is a different value, so a comparison against a truncated reference is
	// a comparison that fails rather than one that happens to agree
	if bytes.Equal(ref, ref[:len(ref)-1]) {
		t.Fatal("a reference one octet shorter compares equal to the whole one")
	}
}

// proposalRefFieldPath renders one field of one structure as the path this file's rows are keyed
// by.
func proposalRefFieldPath(structure reflect.Type, field reflect.StructField) string {
	return structure.Name() + "." + field.Name
}

// proposalRefFieldsOf walks a VALUE and answers every field of it that a row has to account for.
//
// The walk is over the value rather than over the type, and that is what bounds it: it descends
// into a struct field and into a non-nil pointer to a struct, so a proposal carrying a Remove
// reaches AuthenticatedContent, FramedContent, Sender, FramedContentAuthData, Proposal and
// Remove, and stops at the arms that are nil. A type walk would follow Add into KeyPackage into
// LeafNode into Credential and answer the whole package.
//
// A field the walk descends INTO owes no row of its own -- its leaves carry it -- and every other
// field owes one. So the class is what the value is made of, and an eighth field added to any of
// those six structures arrives here without an edit.
func proposalRefFieldsOf(value reflect.Value, found *[]string) {
	structure := value.Type()
	for i := 0; i < structure.NumField(); i += 1 {
		field := structure.Field(i)
		held := value.Field(i)
		if held.Kind() == reflect.Struct {
			proposalRefFieldsOf(held, found)
			continue
		}
		if held.Kind() == reflect.Pointer && !held.IsNil() && held.Elem().Kind() == reflect.Struct {
			proposalRefFieldsOf(held.Elem(), found)
			continue
		}
		*found = append(*found, proposalRefFieldPath(structure, field))
	}
}

// What one field's mutation must do to the reference.
type proposalRefOutcome int

const (
	// the field is on the wire for this shape, so the reference must MOVE
	proposalRefMoves proposalRefOutcome = iota
	// the field is not written for this shape, so the reference must not move. Each row saying
	// this names the reason, and the reason is always a select() that did not select it.
	proposalRefUnchanged
	// the field cannot be varied and leave an encodable proposal, so the reference cannot be
	// computed at all
	proposalRefRefused
)

// One field, how to vary it, and what that must do.
type proposalRefVariation struct {
	outcome proposalRefOutcome
	why     string
	vary    func(authContent *AuthenticatedContent)
}

// proposalRefVariations is the behaviour half: one row per field the walk above finds.
//
// The CLASS is derived and this is the classification of it, which is the division this project
// settled on after fourteen hand written classes turned out to understate what they stood for. A
// field with no row fails; a row naming a field the walk does not reach fails. What each row
// asserts is a real property in both directions -- a field on the wire must move the reference,
// and a field the select() did not select must not, which is the statement that a reference
// covers the message rather than the struct.
func proposalRefVariations() map[string]proposalRefVariation {
	return map[string]proposalRefVariation{
		"AuthenticatedContent.WireFormat": {proposalRefMoves,
			"the wire format is the first field of the serialization",
			func(a *AuthenticatedContent) { a.WireFormat = WireFormatPrivateMessage }},
		"FramedContent.GroupId": {proposalRefMoves,
			"the group id is inside the framed content",
			func(a *AuthenticatedContent) { a.Content.GroupId = []byte{0x08} }},
		"FramedContent.Epoch": {proposalRefMoves,
			"the epoch is what stops a proposal being replayed into another one",
			func(a *AuthenticatedContent) { a.Content.Epoch = 3 }},
		"FramedContent.AuthenticatedData": {proposalRefMoves,
			"the authenticated data is covered by the signature and therefore by the reference",
			func(a *AuthenticatedContent) { a.Content.AuthenticatedData = []byte{0x09} }},
		"FramedContent.ContentType": {proposalRefRefused,
			"a reference is only taken over a proposal",
			func(a *AuthenticatedContent) { a.Content.ContentType = ContentTypeCommit }},
		"FramedContent.ApplicationData": {proposalRefRefused,
			"application data beside a proposal is an arm mismatch",
			func(a *AuthenticatedContent) { a.Content.ApplicationData = []byte{0x0a} }},
		"FramedContent.Commit": {proposalRefRefused,
			"a commit beside a proposal is an arm mismatch",
			func(a *AuthenticatedContent) { a.Content.Commit = &Commit{} }},
		"Sender.SenderType": {proposalRefMoves,
			"the sender type selects which arm of Sender is written",
			func(a *AuthenticatedContent) { a.Content.Sender.SenderType = SenderTypeNewMemberProposal }},
		"Sender.LeafIndex": {proposalRefMoves,
			"the leaf index is what makes two members' identical proposals different references",
			func(a *AuthenticatedContent) { a.Content.Sender.LeafIndex = 9 }},
		"Sender.SenderIndex": {proposalRefUnchanged,
			"SenderIndex is the external arm and this sender is a member, so it is not written",
			func(a *AuthenticatedContent) { a.Content.Sender.SenderIndex = 9 }},
		"FramedContentAuthData.Signature": {proposalRefMoves,
			"the signature is inside the reference, so re-signing renames the proposal",
			func(a *AuthenticatedContent) { a.Auth.Signature = []byte{0xde, 0xae} }},
		"FramedContentAuthData.ConfirmationTag": {proposalRefUnchanged,
			"the confirmation tag is the commit arm of the auth data and this is a proposal, so it is not written",
			func(a *AuthenticatedContent) { a.Auth.ConfirmationTag = []byte{0xbe, 0xef} }},
		"Proposal.ProposalType": {proposalRefRefused,
			"a proposal type naming an arm that is not populated is an arm mismatch",
			func(a *AuthenticatedContent) { a.Content.Proposal.ProposalType = ProposalTypeAdd }},
		"Proposal.Add": {proposalRefRefused,
			"a second arm is refused rather than dropped, or the two values would share a reference",
			func(a *AuthenticatedContent) { a.Content.Proposal.Add = &Add{} }},
		"Proposal.Update": {proposalRefRefused,
			"a second arm is refused rather than dropped",
			func(a *AuthenticatedContent) { a.Content.Proposal.Update = &Update{} }},
		"Proposal.PreSharedKey": {proposalRefRefused,
			"a second arm is refused rather than dropped",
			func(a *AuthenticatedContent) { a.Content.Proposal.PreSharedKey = &PreSharedKey{} }},
		"Proposal.ReInit": {proposalRefRefused,
			"a second arm is refused rather than dropped",
			func(a *AuthenticatedContent) { a.Content.Proposal.ReInit = &ReInit{} }},
		"Proposal.ExternalInit": {proposalRefRefused,
			"a second arm is refused rather than dropped",
			func(a *AuthenticatedContent) { a.Content.Proposal.ExternalInit = &ExternalInit{} }},
		"Proposal.GroupContextExtensions": {proposalRefRefused,
			"a second arm is refused rather than dropped",
			func(a *AuthenticatedContent) { a.Content.Proposal.GroupContextExtensions = &GroupContextExtensions{} }},
		"Proposal.UnknownType": {proposalRefMoves,
			"UnknownType overrides the discriminant that goes on the wire",
			func(a *AuthenticatedContent) { a.Content.Proposal.UnknownType = ProposalType(0x0a0a) }},
		"Proposal.UnknownBody": {proposalRefRefused,
			"a verbatim body beside a registered arm is a second arm",
			func(a *AuthenticatedContent) { a.Content.Proposal.UnknownBody = []byte{0x00} }},
		"Remove.Removed": {proposalRefMoves,
			"the removed leaf is the whole content of the proposal",
			func(a *AuthenticatedContent) { a.Content.Proposal.Remove.Removed = 5 }},
	}
}

// TestProposalRefsAreDistinctAcrossTheGeneratedProposalSpace is the collision property, over a
// space derived from the structures a proposal is built out of rather than from a list of the
// fields somebody thought of.
//
// Two properties, and they are separate claims:
//
//   - every field the encoding covers moves the reference, and no two of those references
//     collide with each other or with the base. A reference that did not move for one field is a
//     field the proposal carries and the reference does not, which is two distinct proposals
//     under one name.
//   - every field the encoding does NOT cover leaves the reference alone. That direction is not
//     a formality: it is the statement that the reference is over the MESSAGE and not over the
//     struct, and the two rows that assert it are the two select() arms this shape does not
//     take.
//
// The rows are held to the derived field set in both directions, so a field added to any of the
// six structures a proposal reaches fails here rather than being quietly outside the sweep.
func TestProposalRefsAreDistinctAcrossTheGeneratedProposalSpace(t *testing.T) {
	crypto := newTestCrypto(t)
	base := testAuthenticatedProposal()
	fields := []string{}
	proposalRefFieldsOf(reflect.ValueOf(base), &fields)
	slices.Sort(fields)
	fields = slices.Compact(fields)
	if len(fields) < 10 {
		t.Fatalf("the walk read %v out of a proposal carrying AuthenticatedContent, which is fewer fields than those structures have", fields)
	}

	variations := proposalRefVariations()
	for name := range variations {
		if !slices.Contains(fields, name) {
			t.Errorf("this file varies %s and no field of a proposal carrying AuthenticatedContent is reached under that name", name)
		}
	}

	baseRef, err := base.ProposalRef(crypto)
	if err != nil {
		t.Fatalf("the base reference: %v", err)
	}
	seen := map[string]string{string(baseRef): "the unvaried proposal"}
	moved, unchanged, refused := 0, 0, 0
	for _, name := range fields {
		variation, written := variations[name]
		if !written {
			t.Errorf("%s is a field of a proposal carrying AuthenticatedContent and nothing varies it, so nothing says whether the reference covers it",
				name)
			continue
		}
		varied := base
		varied.Content.Proposal = &Proposal{}
		*varied.Content.Proposal = *base.Content.Proposal
		varied.Content.Proposal.Remove = &Remove{}
		*varied.Content.Proposal.Remove = *base.Content.Proposal.Remove
		variation.vary(&varied)

		ref, err := varied.ProposalRef(crypto)
		switch variation.outcome {
		case proposalRefRefused:
			if err == nil {
				t.Errorf("%s varied and the reference came back anyway; the row says it is refused because %s", name, variation.why)
				continue
			}
			refused += 1
			continue
		case proposalRefUnchanged:
			if err != nil {
				t.Errorf("%s varied and the reference was refused with %v; the row says it is not written because %s", name, err, variation.why)
				continue
			}
			if !bytes.Equal(ref, baseRef) {
				t.Errorf("%s varied and the reference moved; the row says %s, so it is on the wire after all", name, variation.why)
				continue
			}
			unchanged += 1
			continue
		}
		if err != nil {
			t.Errorf("%s varied and the reference was refused with %v; the row says it is on the wire because %s", name, err, variation.why)
			continue
		}
		if other, collided := seen[string(ref)]; collided {
			t.Errorf("varying %s produced the same reference as %s; two distinct proposals share one name, and a commit naming that reference applies whichever of the two arrived first",
				name, other)
			continue
		}
		seen[string(ref)] = "the proposal with " + name + " varied"
		moved += 1
	}
	if moved == 0 || unchanged == 0 || refused == 0 {
		t.Fatalf("the sweep moved %d references, left %d alone and refused %d; with any of the three empty this gate holds fewer rules than it states",
			moved, unchanged, refused)
	}
	t.Logf("%d fields on the wire and pairwise distinct, %d off it and unchanged, %d refused, over %d derived fields",
		moved, unchanged, refused, len(fields))
}

// ---------------------------------------------------------------------------
// the proposal reference, against bytes this project did not produce
// ---------------------------------------------------------------------------

// TestTheProposalReferenceOfADecodedPublishedProposalIsTheOneItsCommitNames is the only
// assertion in this package that holds (*AuthenticatedContent).ProposalRef -- and the whole
// codec chain underneath it -- against bytes this project did not compute.
//
// Every other reference test above compares the method against MakeProposalRef and RefHash over
// syntax.Marshal of a value built HERE, which is the same two functions the method itself calls
// over an encoding the same encoder wrote. That is self agreement, and self agreement cannot see
// a codec and a preimage that are wrong together -- which is the entire failure mode
// framing_preimage.go's own header says it exists to prevent: not a wrong answer, a group that
// agrees with itself and with nobody else.
//
// The transcript corpus this file already reads is no help there. Every entry of it is a COMMIT
// -- TestTheSectionEightTwoInputDecodesBackToTheStructureItNames makes a non commit fatal -- so
// before this test no proposal shaped AuthenticatedContent had ever been held against published
// bytes, and no computed ProposalRef against a published one.
//
// passive-client-handling-commit.json publishes both halves and is already vendored, already
// loaded in this package and already sliced to exactly these bytes by crypto_labels_test.go's
// TestProposalRefLabelMatchesThePublishedCommits. What that test cannot say is anything about
// this package's FRAMING: it hashes the published slice directly and never decodes it. Running
// the same slice through the codec and back out anchors four things at once against mlswg's own
// bytes -- AuthenticatedContent's codec over a proposal, FramedContent's proposal arm, the whole
// Proposal codec, and the reference itself.
func TestTheProposalReferenceOfADecodedPublishedProposalIsTheOneItsCommitNames(t *testing.T) {
	vectors := []labelKatPassiveClient{}
	loadLabelKat(t, "passive-client-handling-commit.json", &vectors)
	anchored := 0
	for _, vector := range vectors {
		suite := CipherSuite(vector.CipherSuite)
		if !IsRegisteredSuite(suite) {
			continue
		}
		crypto := mustProvider(t, suite)
		// the membership tag is a mac of the suite's hash size carried as a vector, and at
		// thirty two bytes its length prefix is the one octet form
		membershipTag := 1 + crypto.HashSize()
		for epochIndex, epoch := range vector.Epochs {
			commit := mustDecodeHex(t, "the published commit", epoch.Commit)
			for _, published := range epoch.Proposals {
				at := fmt.Sprintf("suite %#04x epoch %d", uint16(suite), epochIndex)
				message := mustDecodeHex(t, "the published proposal", published)
				if len(message) <= len(mlsMessagePublicMessageHeader)+membershipTag ||
					!bytes.HasPrefix(message, mlsMessagePublicMessageHeader) {
					t.Errorf("%s: the proposal is %d bytes headed %x, want an mls10 public message",
						at, len(message), message[:min(len(message), len(mlsMessagePublicMessageHeader))])
					continue
				}
				// MLSMessage = version || AuthenticatedContent || membership_tag<V>, so the
				// content is the message without its two octet version and without the tag
				// (RFC 9420 sections 6.1 and 6.2). The slice is the one crypto_labels_test.go
				// asserts its way to and is not re-derived here.
				authenticatedContent := message[mlsMessageVersionLength : len(message)-membershipTag]
				decoded := AuthenticatedContent{}
				if err := syntax.Unmarshal(authenticatedContent, &decoded); err != nil {
					t.Errorf("%s: the published authenticated content did not decode: %v", at, err)
					continue
				}
				if decoded.Content.ContentType != ContentTypeProposal {
					t.Errorf("%s: the published proposal decoded as content type %d, so this vector is not what this test believes it is",
						at, decoded.Content.ContentType)
					continue
				}
				// the codec is held to the published bytes BEFORE the reference is taken over
				// it. A re-encoding that differed would otherwise arrive as a reference that
				// does not appear in the commit, which reads as a label or a hash defect and is
				// not one.
				reencoded, err := syntax.Marshal(&decoded)
				if err != nil {
					t.Errorf("%s: re-encoding the published authenticated content: %v", at, err)
					continue
				}
				if !bytes.Equal(reencoded, authenticatedContent) {
					t.Errorf("%s: the published authenticated content decoded and re-encoded to %x, want the %x it was decoded from",
						at, reencoded, authenticatedContent)
					continue
				}
				reference, err := decoded.ProposalRef(crypto)
				if err != nil {
					t.Errorf("%s: ProposalRef over the published proposal: %v", at, err)
					continue
				}
				if count := bytes.Count(commit, reference); count != 1 {
					t.Errorf("%s: the reference %x this package computes for the published proposal appears %d times in the published commit, want once",
						at, reference, count)
					continue
				}
				anchored += 1
			}
		}
	}
	// the count is pinned to the same constant crypto_labels_test.go pins its own sweep to, so
	// a corpus that stopped parsing, or a suite filter that stopped matching, fails here rather
	// than reporting green having anchored nothing
	if anchored != labelKatProposalRefs {
		t.Fatalf("anchored %d published proposal references through this package's codec, want %d", anchored, labelKatProposalRefs)
	}
	t.Logf("%d published proposals decoded, re-encoded byte identically and referenced as their own commits name them", anchored)
}

// ---------------------------------------------------------------------------
// PublicMessage, the section 6.2 codec
// ---------------------------------------------------------------------------

// handDerivedShortVector is opaque<V> written out by hand: the one octet varint the syntax
// package spells a length under 64 as, and then the bytes.
//
// Hand derived rather than taken from syntax.Writer, because every golden below is being asked
// whether the field boundaries are where RFC 9420 puts them, and a golden built by the encoder
// under test asserts only that the encoder agrees with itself. The 64 is refused rather than
// handled: a longer vector's prefix is two octets and this helper would silently produce the
// wrong one.
func handDerivedShortVector(t *testing.T, bs []byte) []byte {
	t.Helper()
	if len(bs) >= 64 {
		t.Fatalf("this hand derived vector prefix is the one octet varint, and %d bytes needs two", len(bs))
	}
	return append([]byte{byte(len(bs))}, bs...)
}

// handDerivedUint64 is the eight octet big endian integer RFC 9420's presentation language
// writes a uint64 as, written out by hand for handDerivedShortVector's reason.
func handDerivedUint64(value uint64) []byte {
	found := []byte{}
	for shift := 56; shift >= 0; shift -= 8 {
		found = append(found, byte(value>>uint(shift)))
	}
	return found
}

// framingTestPublicMessage is a member's proposal carried as a PublicMessage.
//
// The membership tag is a stub and its VALUE means nothing here: this file holds the codec, and
// what the codec owns about the tag is that it is written under the member arm, framed as an
// opaque<V>, and last. framing_protect_test.go is where the tag is a MAC.
func framingTestPublicMessage() *PublicMessage {
	return &PublicMessage{
		Content:       *framingTestProposalContent(),
		Auth:          FramedContentAuthData{Signature: []byte{0xde, 0xad}},
		MembershipTag: []byte{0x5a, 0x5b, 0x5c},
	}
}

// framingTestPublicMessageOfSender is that message under a caller's sender type, with the tag
// present exactly where section 6.2 puts one.
func framingTestPublicMessageOfSender(senderType SenderType) *PublicMessage {
	message := framingTestPublicMessage()
	message.Content.Sender = *testSenderOfType(senderType)
	if senderType != SenderTypeMember {
		message.MembershipTag = nil
	}
	return message
}

// framingPublicMessageLayout is the section 6.2 layout written out beside the codec: the framed
// content, the auth data under that content's own type, and -- for a member and only for a member
// -- the membership tag as an opaque<V>.
//
// The two inner encodings are reached through their own codecs rather than hand derived, and that
// is the scope of this file's claim rather than a shortcut. FramedContent and
// FramedContentAuthData have hand derived goldens of their own further up; what PublicMessage
// contributes is the ORDER of the three and the framing of the third, and that is what an
// independent layout can state.
func framingPublicMessageLayout(t *testing.T, message *PublicMessage) []byte {
	t.Helper()
	w := syntax.NewWriter()
	if err := message.Content.MarshalMLS(w); err != nil {
		t.Fatalf("the framed content of the expected layout: %v", err)
	}
	if err := message.Auth.MarshalMLS(w, message.Content.ContentType); err != nil {
		t.Fatalf("the auth data of the expected layout: %v", err)
	}
	if message.Content.Sender.SenderType == SenderTypeMember {
		w.WriteRaw(handDerivedShortVector(t, message.MembershipTag))
	}
	encoded, err := w.Bytes()
	if err != nil {
		t.Fatalf("the expected layout: %v", err)
	}
	return encoded
}

// TestThePublicMessageCodecCarriesTheMembershipTagForAMemberSenderAndForNoOther is section 6.2's
// select, swept over the sender type registry rather than over the one arm somebody wrote a test
// for.
//
// The sweep is what makes this an assertion about the SELECT. A test of the member arm alone is
// passed by a codec that writes the tag unconditionally, which is a message every other
// implementation rejects at the field after it; a test of one other arm alone is passed by a codec
// that never writes it, which strips the only membership authentication a member's public message
// carries. Both survive a round trip property, because both halves of this codec would agree.
//
// The byte equality is against a layout written beside the codec, so the ORDER of the three parts
// is stated by something other than the encoder. A codec that wrote the auth data before the
// content round trips against itself perfectly and against nobody.
func TestThePublicMessageCodecCarriesTheMembershipTagForAMemberSenderAndForNoOther(t *testing.T) {
	registry := registryConstantsOfType(t, "SenderType")
	swept := 0
	for _, name := range slices.Sorted(maps.Keys(registry)) {
		senderType := SenderType(registry[name])
		message := framingTestPublicMessageOfSender(senderType)
		encoded, err := syntax.Marshal(message)
		if err != nil {
			t.Errorf("%s: marshal: %v", name, err)
			continue
		}
		if want := framingPublicMessageLayout(t, message); !bytes.Equal(encoded, want) {
			t.Errorf("%s: encoded %x, want %x", name, encoded, want)
			continue
		}
		decoded := PublicMessage{}
		if err := syntax.Unmarshal(encoded, &decoded); err != nil {
			t.Errorf("%s: unmarshal: %v", name, err)
			continue
		}
		carries := senderType == SenderTypeMember
		if got := len(decoded.MembershipTag) != 0; got != carries {
			t.Errorf("%s: the decoded message carries a membership tag = %v, want %v; section 6.2 gives the field to the member arm and to no other",
				name, got, carries)
			continue
		}
		if carries && !bytes.Equal(decoded.MembershipTag, message.MembershipTag) {
			t.Errorf("%s: the decoded tag is %x, want %x", name, decoded.MembershipTag, message.MembershipTag)
			continue
		}
		reencoded, err := syntax.Marshal(&decoded)
		if err != nil {
			t.Errorf("%s: re-marshal: %v", name, err)
			continue
		}
		if !bytes.Equal(reencoded, encoded) {
			t.Errorf("%s: re-encoded %x, want %x", name, reencoded, encoded)
			continue
		}
		swept += 1
	}
	if swept != len(registry) {
		t.Fatalf("%d of the %d sender types were swept", swept, len(registry))
	}
}

// TestThePublicMessageEncoderRefusesAMemberMessageWithNoMembershipTagAndWritesNothing is ValSem007
// on the send side, over every spelling of an empty byte run rather than over nil.
//
// The spellings are the point and emptyByteSpellings says why: a caller that re-sliced a longer
// buffer to nothing holds a non nil slice with capacity, and a guard written on == nil accepts it
// and emits an empty opaque<V> -- a member's public message carrying a present, wire legal,
// zero length membership tag, which is a message no member could have produced.
//
// The Writer is checked as well as the error. This codec is handed the CALLER's Writer, so a
// refusal raised after the content had been written leaves a caller that ignored the return value
// holding the front of an encoding whose only membership authentication is missing.
func TestThePublicMessageEncoderRefusesAMemberMessageWithNoMembershipTagAndWritesNothing(t *testing.T) {
	for _, empty := range emptyByteSpellings() {
		message := framingTestPublicMessage()
		message.MembershipTag = empty.value
		w := syntax.NewWriter()
		if err := message.MarshalMLS(w); !errors.Is(err, errMissingMembershipTag) {
			t.Errorf("a membership tag that is %s: got %v, want errMissingMembershipTag", empty.what, err)
			continue
		}
		if w.Len() != 0 {
			t.Errorf("a membership tag that is %s: the refusal left %d octets on the caller's Writer",
				empty.what, w.Len())
		}
	}
}

// TestThePublicMessageDecoderRefusesTheEmptyTagItsEncoderRefusesToWrite is the other half of that
// rule, and it is the half a peer reaches.
//
// The encoder's guard protects nobody from an implementation that does not run this code. An empty
// opaque<V> is the wire encoding of "no tag", it is what a hostile peer sends to find out whether
// this receiver's ValSem007 is on the codec or somewhere later, and a decoder that accepted it
// would hand its caller a member's PublicMessage whose refusal depends on somebody downstream
// remembering to look.
func TestThePublicMessageDecoderRefusesTheEmptyTagItsEncoderRefusesToWrite(t *testing.T) {
	decoded := PublicMessage{}
	if err := syntax.Unmarshal(framingPublicMessageWithEmptyTag(t), &decoded); !errors.Is(err, errMissingMembershipTag) {
		t.Fatalf("got %v, want errMissingMembershipTag", err)
	}
}

// framingPublicMessageWithEmptyTag is a member's PublicMessage whose membership_tag field is
// present, wire legal and zero length -- the encoding this package's own encoder refuses to
// produce, which is why it is assembled field by field here.
func framingPublicMessageWithEmptyTag(t *testing.T) []byte {
	t.Helper()
	message := framingTestPublicMessage()
	w := syntax.NewWriter()
	if err := message.Content.MarshalMLS(w); err != nil {
		t.Fatalf("the framed content: %v", err)
	}
	if err := message.Auth.MarshalMLS(w, message.Content.ContentType); err != nil {
		t.Fatalf("the auth data: %v", err)
	}
	w.WriteOpaque(nil)
	encoded, err := w.Bytes()
	if err != nil {
		t.Fatalf("the encoding with an empty tag: %v", err)
	}
	return encoded
}

// TestThePublicMessageDecoderRefusesTrailingBytes states that this codec consumes its whole input.
// A decoder that stopped short would let a peer append anything it liked to a message whose
// authenticators cover none of it.
func TestThePublicMessageDecoderRefusesTrailingBytes(t *testing.T) {
	encoded, err := syntax.Marshal(framingTestPublicMessage())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	decoded := PublicMessage{}
	if err := syntax.Unmarshal(append(encoded, 0x00), &decoded); err == nil {
		t.Fatal("a public message with a trailing octet decoded")
	}
}

// TestThePublicMessageViewStampsThePublicWireFormatAndCarriesNoTag is the AuthenticatedContent
// projection's two claims.
//
// The wire format is STAMPED rather than carried, and that is what puts the section 6.1 preimage
// under the format the bytes were actually read out of: a public message replayed through the
// private codec is stamped with the other value and its signature does not verify. The tag is
// absent because it is a MAC OVER that preimage and not a part of it -- a view that carried one
// would be offering a caller an authenticator with nothing to check it against.
func TestThePublicMessageViewStampsThePublicWireFormatAndCarriesNoTag(t *testing.T) {
	message := framingTestPublicMessage()
	view := message.AuthenticatedContent()
	if view.WireFormat != WireFormatPublicMessage {
		t.Errorf("the view carries wire format %d, want %d", view.WireFormat, WireFormatPublicMessage)
	}
	encoded, err := syntax.Marshal(view)
	if err != nil {
		t.Fatalf("marshal the view: %v", err)
	}
	if bytes.Contains(encoded, message.MembershipTag) {
		t.Errorf("the serialized view %x holds the membership tag %x, so the tag is inside the preimage it is a mac over",
			encoded, message.MembershipTag)
	}
}

// framingTestContentOfType is the member's FramedContent of a caller's content type.
//
// The switch is exhaustive over the registry and its default is a FATAL rather than a fallback,
// which is what makes the sweeps below claims about the registry rather than about the arms
// somebody wrote a fixture for. A fourth content type registered in framing.go arrives here as a
// failure naming itself, and not as a row that quietly runs the proposal fixture a second time.
func framingTestContentOfType(t *testing.T, contentType ContentType) *FramedContent {
	t.Helper()
	switch contentType {
	case ContentTypeApplication:
		return framingTestMemberContent()
	case ContentTypeProposal:
		return framingTestProposalContent()
	case ContentTypeCommit:
		return framingTestCommitContent()
	}
	t.Fatalf("content type %d is registered and this file has no FramedContent for it, so every sweep over the registry runs an arm short",
		contentType)
	return nil
}

// framingTestPublicMessageOfContentType is a member's PublicMessage carrying that content, with
// every authenticator its content type selects and no other.
//
// The confirmation tag stands on the commit row alone, which is section 6's select: a commit
// carries two authenticators and the other two content types carry one.
func framingTestPublicMessageOfContentType(t *testing.T, contentType ContentType) *PublicMessage {
	t.Helper()
	message := framingTestPublicMessage()
	message.Content = *framingTestContentOfType(t, contentType)
	if contentType == ContentTypeCommit {
		message.Auth.ConfirmationTag = []byte{0xc1, 0xc2, 0xc3, 0xc4}
	}
	return message
}

// TestThePublicMessageCodecCarriesEveryAuthenticatorItsContentTypeSelects sweeps the content type
// registry through both halves of the section 6.2 codec.
//
// The commit row is what this exists for and it is the row this file did not have. Every other
// public message test in this package holds the content type FIXED at proposal -- the layout
// golden, the sender type sweep, the membership tag select, the seal and open sweeps one file
// over -- so the confirmation_tag arm of FramedContentAuthData was reached from this codec by
// nothing at all. Measured, not supposed: with both halves of this codec framing the auth data
// under a hardcoded ContentTypeProposal, so that a commit carried as a PublicMessage loses its
// confirmation tag on the wire in both directions and round trips happily without it, the whole
// of ./mls/... and ./message/... stayed green.
//
// Which is why the assertion is against the layout written beside the codec and against the
// tag's own octets, rather than against a round trip. A round trip is symmetric: both halves are
// wrong the same way and it passes. The layout writes the auth data under the message's OWN
// content type, so what is compared is the arm the codec picked against the arm the content type
// selects, and the containment check states the same thing without going through that codec at
// all.
func TestThePublicMessageCodecCarriesEveryAuthenticatorItsContentTypeSelects(t *testing.T) {
	registry := registryConstantsOfType(t, "ContentType")
	swept, tagged := 0, 0
	for _, name := range slices.Sorted(maps.Keys(registry)) {
		contentType := ContentType(registry[name])
		message := framingTestPublicMessageOfContentType(t, contentType)
		encoded, err := syntax.Marshal(message)
		if err != nil {
			t.Errorf("%s: marshal: %v", name, err)
			continue
		}
		if want := framingPublicMessageLayout(t, message); !bytes.Equal(encoded, want) {
			t.Errorf("%s: encoded %x, want the section 6.2 layout %x; the auth data was framed under a content type that is not this message's",
				name, encoded, want)
			continue
		}
		decoded := PublicMessage{}
		if err := syntax.Unmarshal(encoded, &decoded); err != nil {
			t.Errorf("%s: unmarshal: %v", name, err)
			continue
		}
		if !bytes.Equal(decoded.Auth.Signature, message.Auth.Signature) {
			t.Errorf("%s: decoded the signature %x, want %x", name, decoded.Auth.Signature, message.Auth.Signature)
			continue
		}
		if !bytes.Equal(decoded.Auth.ConfirmationTag, message.Auth.ConfirmationTag) {
			t.Errorf("%s: decoded the confirmation tag %x, want %x; a decoder reading the auth data under some other content type reads a commit's second authenticator as something else or not at all",
				name, decoded.Auth.ConfirmationTag, message.Auth.ConfirmationTag)
			continue
		}
		if len(message.Auth.ConfirmationTag) != 0 {
			if !bytes.Contains(encoded, message.Auth.ConfirmationTag) {
				t.Errorf("%s: the encoding %x does not hold the confirmation tag %x, so this message went onto the wire with the authenticator that binds it to the epoch it creates missing",
					name, encoded, message.Auth.ConfirmationTag)
				continue
			}
			tagged += 1
		} else {
			// the other direction of the select. A tag SET on a content type that has no place
			// for one must not reach the wire: a codec writing the field whenever it happened to
			// be populated, rather than when the content type selected it, produces a message
			// every peer misframes at the field after it and round trips against itself
			// perfectly.
			stray := *message
			stray.Auth = FramedContentAuthData{Signature: message.Auth.Signature,
				ConfirmationTag: []byte{0xc1, 0xc2, 0xc3, 0xc4}}
			strayEncoded, err := syntax.Marshal(&stray)
			if err != nil {
				t.Errorf("%s carrying a confirmation tag it has no arm for: marshal: %v", name, err)
				continue
			}
			if !bytes.Equal(strayEncoded, encoded) {
				t.Errorf("%s: setting a confirmation tag changed the encoding to %x from %x, so the arm is selected by which field is populated rather than by the content type",
					name, strayEncoded, encoded)
				continue
			}
		}
		swept += 1
	}
	if swept != len(registry) || tagged == 0 {
		t.Fatalf("%d of the %d registered content types were carried through this codec and %d of them carried a confirmation tag; with either half short this states one arm rather than the select",
			swept, len(registry), tagged)
	}
}

// TestThePublicMessageViewCarriesEveryAuthenticatorItsContentTypeSelects is the same sweep over
// the projection, which is the OTHER place a commit's second authenticator can be dropped.
//
// This view is not a convenience. It is the object SealPublicMessage macs and OpenPublicMessage
// macs and verifies, so every field missing from it is a field missing from the membership tag's
// preimage AND from the signature's. A view that answered a commit without its confirmation tag
// would have both doors of section 6.2 authenticating a commit that carries no binding to the
// transcript it confirms -- and it would do it with nothing in this package disagreeing, because
// the two doors would be wrong together. Measured: the whole suite stayed green over exactly
// that.
//
// The membership tag's absence from the view is the neighbouring test's claim and is not
// restated here.
func TestThePublicMessageViewCarriesEveryAuthenticatorItsContentTypeSelects(t *testing.T) {
	registry := registryConstantsOfType(t, "ContentType")
	swept := 0
	for _, name := range slices.Sorted(maps.Keys(registry)) {
		contentType := ContentType(registry[name])
		message := framingTestPublicMessageOfContentType(t, contentType)
		view := message.AuthenticatedContent()
		if !bytes.Equal(view.Auth.Signature, message.Auth.Signature) {
			t.Errorf("%s: the view carries the signature %x, want the message's %x",
				name, view.Auth.Signature, message.Auth.Signature)
			continue
		}
		if !bytes.Equal(view.Auth.ConfirmationTag, message.Auth.ConfirmationTag) {
			t.Errorf("%s: the view carries the confirmation tag %x, want the message's %x; both authenticators of section 6.2 are taken over this object, so one missing from it is one taken over a preimage it is not in",
				name, view.Auth.ConfirmationTag, message.Auth.ConfirmationTag)
			continue
		}
		encoded, err := syntax.Marshal(view)
		if err != nil {
			t.Errorf("%s: marshal the view: %v", name, err)
			continue
		}
		if len(message.Auth.ConfirmationTag) != 0 &&
			!bytes.Contains(encoded, message.Auth.ConfirmationTag) {
			t.Errorf("%s: the serialized view %x does not hold the confirmation tag %x, so neither preimage taken over it covers that tag",
				name, encoded, message.Auth.ConfirmationTag)
			continue
		}
		swept += 1
	}
	if swept != len(registry) {
		t.Fatalf("%d of the %d registered content types reached the view", swept, len(registry))
	}
}

// ---------------------------------------------------------------------------
// PrivateMessage, the section 6.3 codec
// ---------------------------------------------------------------------------

// framingTestPrivateMessage is the section 6.3 header with every field carrying something, and
// every field carrying something DIFFERENT.
//
// Distinct values in distinct fields is what separates the codec's field order from a permutation
// of it. Three opaque<V> fields stand together at the tail of this structure, so a codec that
// wrote the ciphertext where the sender data belongs round trips against itself byte for byte; the
// golden below is what tells them apart, and it can only do that if the two carry different bytes.
func framingTestPrivateMessage() *PrivateMessage {
	return &PrivateMessage{
		GroupId:             []byte{0x01, 0x02},
		Epoch:               9,
		ContentType:         ContentTypeApplication,
		AuthenticatedData:   []byte{0xaa},
		EncryptedSenderData: []byte{0xbb, 0xcc},
		Ciphertext:          []byte{0xdd, 0xee, 0xff},
	}
}

// handDerivedPrivateMessageGolden is that structure's encoding read off RFC 9420 section 6.3:
//
//	opaque group_id<V>; uint64 epoch; ContentType content_type;
//	opaque authenticated_data<V>; opaque encrypted_sender_data<V>; opaque ciphertext<V>;
//
// Hand derived, which is the only kind of golden that says anything here. The three tail vectors
// are structurally identical on the wire, so any permutation of them is a well formed
// PrivateMessage that this package's own encoder and decoder agree about perfectly and that every
// other implementation reads with the fields swapped -- a receiver handed a ciphertext where the
// sender data belongs, which is a decryption failure nobody can attribute.
func handDerivedPrivateMessageGolden(t *testing.T, message *PrivateMessage) []byte {
	t.Helper()
	golden := handDerivedShortVector(t, message.GroupId)
	golden = append(golden, handDerivedUint64(message.Epoch)...)
	golden = append(golden, byte(message.ContentType))
	golden = append(golden, handDerivedShortVector(t, message.AuthenticatedData)...)
	golden = append(golden, handDerivedShortVector(t, message.EncryptedSenderData)...)
	return append(golden, handDerivedShortVector(t, message.Ciphertext)...)
}

// handDerivedPublicMessageHeader is a FramedContent whose sender_type is the caller's, followed
// by everything a member arm and the fields after it would have consumed.
//
// The tail is handDerivedFramedContentHeader's point: without it a decoder that refused nothing
// runs out of input and reports a truncation, which reads as a refusal of the code point and is
// not one. Four octets for a sender arm, an empty authenticated_data, the proposal content type,
// and two octets standing where a proposal would have begun.
func handDerivedPublicMessageHeader(senderType SenderType) []byte {
	return append(append([]byte{0x00}, handDerivedUint64(0)...),
		byte(senderType), 0x00, 0x00, 0x00, 0x00, 0x00, byte(ContentTypeProposal), 0x00, 0x00)
}

// handDerivedPrivateMessageHeader is the three fields every PrivateMessage opens with, ending in
// the content type the caller names, followed by two octets a field after it would have consumed.
//
// The trailing octets are handDerivedFramedContentHeader's point one structure over: without them
// a decoder that refused nothing runs out of input and reports a truncation, which reads as a
// refusal of the code point and is not one.
func handDerivedPrivateMessageHeader(contentType ContentType) []byte {
	return append(append([]byte{0x00}, handDerivedUint64(0)...), byte(contentType), 0x00, 0x00)
}

func TestPrivateMessageRoundTrip(t *testing.T) {
	message := framingTestPrivateMessage()
	encoded, err := syntax.Marshal(message)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	decoded := PrivateMessage{}
	if err := syntax.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	reencoded, err := syntax.Marshal(&decoded)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if !bytes.Equal(encoded, reencoded) {
		t.Fatalf("re-encoded %x, want %x", reencoded, encoded)
	}
	if !reflect.DeepEqual(&decoded, message) {
		t.Fatalf("decoded %+v, want %+v", decoded, message)
	}
}

// TestPrivateMessageMarshalMatchesTheHandDerivedGolden is the field order, held against an
// encoding written from the RFC rather than read back through the encoder.
//
// The round trip above cannot see any of this. It is a symmetry property, and every permutation of
// the three tail vectors is symmetric.
func TestPrivateMessageMarshalMatchesTheHandDerivedGolden(t *testing.T) {
	message := framingTestPrivateMessage()
	encoded, err := syntax.Marshal(message)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if want := handDerivedPrivateMessageGolden(t, message); !bytes.Equal(encoded, want) {
		t.Fatalf("encoded %x, want %x", encoded, want)
	}
	decoded := PrivateMessage{}
	if err := syntax.Unmarshal(handDerivedPrivateMessageGolden(t, message), &decoded); err != nil {
		t.Fatalf("unmarshal the golden: %v", err)
	}
	if !reflect.DeepEqual(&decoded, message) {
		t.Fatalf("the golden decoded to %+v, want %+v", decoded, message)
	}
}

// TestPrivateMessageRejectsTheReservedContentType is the reserved zero, which is the code point a
// header nothing parsed leaves behind.
//
// The whole of the unregistered space is swept by
// TestEveryFramingCodePointRefusalNamesTheCodePointItRefused, over both directions of this codec.
// This one is here because the zero is the value with a mechanism behind it rather than a value in
// the gap: it is the ContentType a zero valued struct carries and the one an unparsed octet run
// produces, and a codec that admitted it would route both onto the application ratchet.
func TestPrivateMessageRejectsTheReservedContentType(t *testing.T) {
	message := framingTestPrivateMessage()
	message.ContentType = ContentType(0)
	if _, err := syntax.Marshal(message); !errors.Is(err, ErrUnknownContentType) {
		t.Fatalf("marshal: got %v, want ErrUnknownContentType", err)
	}
	decoded := PrivateMessage{}
	if err := syntax.Unmarshal(handDerivedPrivateMessageHeader(ContentType(0)), &decoded); !errors.Is(err, ErrUnknownContentType) {
		t.Fatalf("unmarshal: got %v, want ErrUnknownContentType", err)
	}
}

// TestARefusedPrivateMessageMarshalWritesNothing is FramedContent's and FramedContentAuthData's
// rule for this codec: the refusal happens before an octet reaches the caller's Writer.
func TestARefusedPrivateMessageMarshalWritesNothing(t *testing.T) {
	message := framingTestPrivateMessage()
	message.ContentType = ContentType(undeclaredCodePointsOf(t, "ContentType")[0])
	w := syntax.NewWriter()
	if err := message.MarshalMLS(w); !errors.Is(err, ErrUnknownContentType) {
		t.Fatalf("got %v, want ErrUnknownContentType", err)
	}
	if w.Len() != 0 {
		t.Fatalf("the refusal left %d octets on the caller's Writer", w.Len())
	}
}

// TestPrivateMessageRefusesEveryTruncationAndAnyTrailingByte states that this codec consumes
// exactly its input.
//
// Every prefix rather than one, for the reason every refusal in this package is derived over a
// length rather than sampled: a decoder that read a vector's prefix and then trusted it would be
// refused at some truncations and not at others, and the truncation somebody picked is not
// necessarily one of them.
func TestPrivateMessageRefusesEveryTruncationAndAnyTrailingByte(t *testing.T) {
	encoded, err := syntax.Marshal(framingTestPrivateMessage())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for at := 0; at < len(encoded); at += 1 {
		decoded := PrivateMessage{}
		if err := syntax.Unmarshal(encoded[:at], &decoded); err == nil {
			t.Errorf("the first %d of %d octets decoded as a whole private message", at, len(encoded))
		}
	}
	decoded := PrivateMessage{}
	if err := syntax.Unmarshal(append(encoded, 0x00), &decoded); err == nil {
		t.Fatal("a private message with a trailing octet decoded")
	}
}

// ---------------------------------------------------------------------------
// the two section 6.3 AADs
// ---------------------------------------------------------------------------

// handDerivedSenderDataAAD is RFC 9420 section 6.3.2's SenderDataAAD written out by hand:
// opaque group_id<V>, uint64 epoch, ContentType content_type, and nothing after it.
func handDerivedSenderDataAAD(t *testing.T, groupId []byte, epoch uint64, contentType ContentType) []byte {
	t.Helper()
	golden := handDerivedShortVector(t, groupId)
	golden = append(golden, handDerivedUint64(epoch)...)
	return append(golden, byte(contentType))
}

// handDerivedPrivateContentAAD is section 6.3.1's PrivateContentAAD written out by hand: the same
// three fields, and then opaque authenticated_data<V>.
//
// It is written as the three fields again rather than as "the sender data AAD plus a field", which
// is the opposite of what the implementation does and is deliberate. The implementation builds one
// out of the other so that the shared header has a single assembly; a golden that did the same
// would agree with it about a header both of them got wrong.
func handDerivedPrivateContentAAD(t *testing.T, groupId []byte, epoch uint64, contentType ContentType,
	authenticatedData []byte) []byte {

	t.Helper()
	golden := handDerivedShortVector(t, groupId)
	golden = append(golden, handDerivedUint64(epoch)...)
	golden = append(golden, byte(contentType))
	return append(golden, handDerivedShortVector(t, authenticatedData)...)
}

// section63AadHeader is one cleartext PrivateMessage header the two AADs are taken over.
type section63AadHeader struct {
	what        string
	groupId     []byte
	epoch       uint64
	contentType ContentType
}

// sameHeaderAs reports whether two headers are the same three VALUES, which is what the two AADs
// are allowed to agree about. nil and the empty slice are one value here because opaque<V> has no
// representation for "absent" -- WriteOpaque writes the same zero length prefix for both -- so an
// AAD that separated them would be the thing that is wrong.
func (self section63AadHeader) sameHeaderAs(other section63AadHeader) bool {
	return bytes.Equal(self.groupId, other.groupId) && self.epoch == other.epoch &&
		self.contentType == other.contentType
}

// section63AadHeaders is the corpus the sweep below runs over: headers that differ in one field at
// a time, derived over the epoch's own WIDTH and over the content type registry rather than
// sampled.
//
// The epoch rows are one set bit per octet, which is what separates an AAD that dropped the field
// from one that wrote it narrow. An epoch corpus of 0, 1 and 2 is covered identically by a
// uint64, a uint32 and a uint8, and the third of those is a header a peer can collide by choosing
// its epoch -- so the octets are swept rather than the values.
//
// The group ids differ in LENGTH as well as in content, and two of them are permutations of each
// other, which is what separates a length prefix that is present from one that is not: without a
// prefix, {0x01, 0x02} at epoch 0 and {0x01} at an epoch beginning 0x02 are the same octets.
func section63AadHeaders(t *testing.T) []section63AadHeader {
	t.Helper()
	base := section63AadHeader{what: "the base header", groupId: []byte{0x01, 0x02}, epoch: 9,
		contentType: ContentTypeApplication}
	found := []section63AadHeader{base}
	for shift := 0; shift < 64; shift += 8 {
		found = append(found, section63AadHeader{
			what:        fmt.Sprintf("epoch with the only set bit at %d", shift),
			groupId:     base.groupId,
			epoch:       uint64(1) << uint(shift),
			contentType: base.contentType,
		})
	}
	registry := registryConstantsOfType(t, "ContentType")
	for _, name := range slices.Sorted(maps.Keys(registry)) {
		found = append(found, section63AadHeader{
			what:        "content type " + name,
			groupId:     base.groupId,
			epoch:       base.epoch,
			contentType: ContentType(registry[name]),
		})
	}
	for _, groupId := range [][]byte{
		nil, {}, {0x00}, {0x01}, {0x02}, {0x01, 0x02}, {0x02, 0x01}, {0x01, 0x02, 0x00},
		bytes.Repeat([]byte{0x33}, 40),
	} {
		found = append(found, section63AadHeader{
			what:        fmt.Sprintf("group id %x", groupId),
			groupId:     groupId,
			epoch:       base.epoch,
			contentType: base.contentType,
		})
	}
	return found
}

// TestTheTwoSectionSixThreeAadsAreTheHandDerivedPreimages holds both AADs against encodings written
// from the RFC, over the whole corpus.
//
// This is the assertion the prefix relationship below cannot make. An implementation that dropped
// the epoch from BOTH of them, or wrote the group id with no length prefix in both, satisfies
// every structural property the two have with respect to each other and produces a message no
// other implementation can open -- and, since each AAD is only ever checked against itself, no
// round trip and no seal-then-open in this package can see it.
func TestTheTwoSectionSixThreeAadsAreTheHandDerivedPreimages(t *testing.T) {
	const authenticatedData = "\xa5\xa6"
	for _, header := range section63AadHeaders(t) {
		senderAAD, err := senderDataAAD(header.groupId, header.epoch, header.contentType)
		if err != nil {
			t.Errorf("%s: sender data aad: %v", header.what, err)
			continue
		}
		want := handDerivedSenderDataAAD(t, header.groupId, header.epoch, header.contentType)
		if !bytes.Equal(senderAAD, want) {
			t.Errorf("%s: sender data aad %x, want %x", header.what, senderAAD, want)
			continue
		}
		contentAAD, err := privateContentAAD(header.groupId, header.epoch, header.contentType,
			[]byte(authenticatedData))
		if err != nil {
			t.Errorf("%s: private content aad: %v", header.what, err)
			continue
		}
		want = handDerivedPrivateContentAAD(t, header.groupId, header.epoch, header.contentType,
			[]byte(authenticatedData))
		if !bytes.Equal(contentAAD, want) {
			t.Errorf("%s: private content aad %x, want %x", header.what, contentAAD, want)
		}
	}
}

// TestBothSectionSixThreeAadsSeparateEveryHeaderTheRfcSeparates is the binding property, stated as
// a collision test over the corpus rather than as a list of fields.
//
// A field an AAD stopped covering is a pair of headers it can no longer tell apart, which is what
// this looks for: two headers that are not the same three values must not produce the same AAD,
// and two that ARE the same values must. The second half is not decoration -- an AAD that hashed
// its arguments' addresses, or that appended a counter, would separate everything and bind
// nothing, and it would pass a test that only looked for collisions.
//
// What a collision costs is not a decryption failure. group_id and epoch are the two fields the
// message server routes on and the two an attacker can rewrite in flight, so an AAD that stopped
// separating them is a message that decrypts in a group or an epoch its sender never addressed.
func TestBothSectionSixThreeAadsSeparateEveryHeaderTheRfcSeparates(t *testing.T) {
	const authenticatedData = "\xa5\xa6"
	headers := section63AadHeaders(t)
	for _, aad := range []struct {
		name string
		take func(header section63AadHeader) ([]byte, error)
	}{
		{name: "senderDataAAD", take: func(header section63AadHeader) ([]byte, error) {
			return senderDataAAD(header.groupId, header.epoch, header.contentType)
		}},
		{name: "privateContentAAD", take: func(header section63AadHeader) ([]byte, error) {
			return privateContentAAD(header.groupId, header.epoch, header.contentType,
				[]byte(authenticatedData))
		}},
	} {
		taken := make([][]byte, len(headers))
		for at, header := range headers {
			answer, err := aad.take(header)
			if err != nil {
				t.Fatalf("%s over %s: %v", aad.name, header.what, err)
			}
			taken[at] = answer
		}
		compared := 0
		for i := range headers {
			for j := i + 1; j < len(headers); j += 1 {
				same := headers[i].sameHeaderAs(headers[j])
				if got := bytes.Equal(taken[i], taken[j]); got != same {
					t.Errorf("%s: %s and %s are the same header = %v and produce the same aad = %v; %x against %x",
						aad.name, headers[i].what, headers[j].what, same, got, taken[i], taken[j])
				}
				compared += 1
			}
		}
		if compared == 0 {
			t.Fatalf("%s: no pair of headers was compared, so this states nothing", aad.name)
		}
	}
}

// TestNeitherSectionSixThreeAadCoversAFieldTheOtherOwns is the structural half, and it is the one
// the two AADs' shape exists for.
//
// authenticated_data belongs to the CONTENT AAD alone. The sender data is decrypted before the
// content -- it is what names the leaf whose ratchet holds the content key -- so a sender data AAD
// covering a field belonging to the content step would leave a receiver with no order in which to
// do the two. connect/message has this same pair one layer down and its guardrail G4 is the same
// rule; what that layer found is that a shared builder taking every field lets a field migrate
// between the two preimages without a diff anybody reads, and that the defence has to be
// structural. senderDataAAD cannot see authenticated_data because it is not a parameter of it, and
// what this test adds is that the content AAD extends it by EXACTLY that field and by nothing else.
//
// The empty authenticated_data row is the one that matters most. It is the case where the two AADs
// are closest -- the content's is the sender data's plus a single zero length prefix octet -- and
// an implementation that dropped the field entirely when it was empty would make the two byte
// identical, which is the collision this pair has no other separation against. RFC 9420 gives these
// two structures no label and no domain separation tag of any kind: nothing distinguishes them but
// the fields themselves and the key each is used with, so "the content AAD is never the sender
// data AAD" is a property that has to hold at every input rather than at the interesting ones.
func TestNeitherSectionSixThreeAadCoversAFieldTheOtherOwns(t *testing.T) {
	header := section63AadHeader{groupId: []byte{0x01, 0x02}, epoch: 9, contentType: ContentTypeApplication}
	senderAAD, err := senderDataAAD(header.groupId, header.epoch, header.contentType)
	if err != nil {
		t.Fatalf("sender data aad: %v", err)
	}
	for _, authenticatedData := range [][]byte{
		nil, {}, {0x00}, {0xa5}, {0xa5, 0xa6}, bytes.Repeat([]byte{0xa5}, 40),
	} {
		contentAAD, err := privateContentAAD(header.groupId, header.epoch, header.contentType,
			authenticatedData)
		if err != nil {
			t.Errorf("authenticated_data %x: private content aad: %v", authenticatedData, err)
			continue
		}
		want := append(append([]byte{}, senderAAD...), handDerivedShortVector(t, authenticatedData)...)
		if !bytes.Equal(contentAAD, want) {
			t.Errorf("authenticated_data %x: the content aad is %x, want the sender data aad followed by exactly that field, %x",
				authenticatedData, contentAAD, want)
			continue
		}
		if bytes.Equal(contentAAD, senderAAD) {
			t.Errorf("authenticated_data %x: the two aads are the same %d octets, and RFC 9420 gives them no label to tell them apart",
				authenticatedData, len(senderAAD))
		}
		// the sender data AAD does not move when the field it must not see moves. Stated over the
		// answer rather than over the parameter list, because a widened signature is what this is
		// watching for and a widened signature still compiles here.
		again, err := senderDataAAD(header.groupId, header.epoch, header.contentType)
		if err != nil {
			t.Errorf("authenticated_data %x: sender data aad: %v", authenticatedData, err)
			continue
		}
		if !bytes.Equal(again, senderAAD) {
			t.Errorf("authenticated_data %x: the sender data aad changed to %x from %x",
				authenticatedData, again, senderAAD)
		}
		// the containment reading is meaningful only for an authenticated_data that does not occur
		// in the header by coincidence, and {0x00} does -- seven of the epoch's eight octets are
		// zero. The corpus is filtered against an INDEPENDENT encoding of the header rather than
		// against the answer under test, which would filter out exactly the leak this looks for.
		independent := handDerivedSenderDataAAD(t, header.groupId, header.epoch, header.contentType)
		if len(authenticatedData) != 0 && !bytes.Contains(independent, authenticatedData) &&
			bytes.Contains(senderAAD, authenticatedData) {
			t.Errorf("authenticated_data %x leaked into the sender data aad %x", authenticatedData, senderAAD)
		}
	}
}

// TestSenderDataAADIsPrivateContentAADWithoutAuthenticatedData is the plan's own statement of the
// prefix relationship, kept beside the two above because it is the shape a copy-paste breaks.
//
// It is the weakest of the three and it is worth saying which mutations it cannot see, so nobody
// reads it as the guard: it passes over an implementation that dropped the epoch from both AADs,
// over one that wrote the group id with no length prefix in both, and over one that reordered the
// shared header, because every one of those keeps the prefix relationship exactly.
func TestSenderDataAADIsPrivateContentAADWithoutAuthenticatedData(t *testing.T) {
	groupId := []byte{0x01, 0x02}
	authenticatedData := []byte{0xa5, 0xa6}

	senderAAD, err := senderDataAAD(groupId, 9, ContentTypeApplication)
	if err != nil {
		t.Fatalf("sender aad: %v", err)
	}
	contentAAD, err := privateContentAAD(groupId, 9, ContentTypeApplication, authenticatedData)
	if err != nil {
		t.Fatalf("content aad: %v", err)
	}
	if !bytes.HasPrefix(contentAAD, senderAAD) {
		t.Fatalf("content aad %x does not start with sender aad %x", contentAAD, senderAAD)
	}
	if bytes.Contains(senderAAD, authenticatedData) {
		t.Fatal("authenticated_data leaked into SenderDataAAD")
	}
	if len(contentAAD) <= len(senderAAD) {
		t.Fatal("content aad does not extend sender aad")
	}
}

// ---------------------------------------------------------------------------
// the single assembly of section 6.3's shared header
// ---------------------------------------------------------------------------

// TestBothSectionSixThreeAadsRefuseAContentTypeNoRegistryDeclares is the content type guard the
// two AADs did not have, and it is stated over BOTH of them because only one of them holds the
// switch.
//
// Every other preimage of framing_preimage.go refuses an unregistered code point before it writes
// an octet -- the section 8.2 input and the section 6.1 TBS both refuse a wire format, and the TBS
// routes the sender type through senderBindsGroupContext -- and these two were the exception. The
// package's own registry switch gate could not see it: that class is "codec methods that mention a
// framing registry", and an AAD is not a codec method, which is the rule 5 shape of a class that
// under-approximates.
//
// What an unregistered content type costs here is not a rejected message. Nothing downstream
// re-reads this field, so the AAD is simply built, and the AEAD is sealed under associated data
// no peer computes the same way; the failure arrives at the far end as a decryption that did not
// work, with nothing in it that says which field was wrong.
//
// The privateContentAAD half is the second observer of the single assembly and is the reason this
// test names both. That function holds no content type switch of its own and is not meant to grow
// one: the only thing that can refuse an unregistered content type on its behalf is the assembly
// it delegates to. A copy of the shared header laid out again inside it is byte for byte
// identical at every registered input -- which is why no golden and no collision sweep can see it
// -- and answers an AAD here where this answers ErrUnknownContentType.
func TestBothSectionSixThreeAadsRefuseAContentTypeNoRegistryDeclares(t *testing.T) {
	const authenticatedData = "\xa5\xa6"
	groupId := []byte{0x01, 0x02}
	refused := 0
	for _, codePoint := range undeclaredCodePointsOf(t, "ContentType") {
		contentType := ContentType(codePoint)
		for _, aad := range []struct {
			name string
			take func() ([]byte, error)
		}{
			{name: "senderDataAAD", take: func() ([]byte, error) {
				return senderDataAAD(groupId, 9, contentType)
			}},
			{name: "privateContentAAD", take: func() ([]byte, error) {
				return privateContentAAD(groupId, 9, contentType, []byte(authenticatedData))
			}},
		} {
			answer, err := aad.take()
			if err == nil {
				t.Fatalf("%s built the associated data %x over the content type %d, which the registry does not declare; that message is sealed under data no peer reproduces",
					aad.name, answer, codePoint)
			}
			if !errors.Is(err, ErrUnknownContentType) {
				t.Fatalf("%s refused the content type %d with %v, want ErrUnknownContentType; two sentinels for one condition is what every roster in this package exists to prevent",
					aad.name, codePoint, err)
			}
			if !namesTheNumber(err.Error(), codePoint) {
				t.Fatalf("%s refused the unregistered content type %d with %q, which does not name it",
					aad.name, codePoint, err)
			}
			if answer != nil {
				t.Fatalf("%s answered %x alongside its refusal of the content type %d; a caller that read the first result holds an associated data built out of a code point nothing declares",
					aad.name, answer, codePoint)
			}
			refused += 1
		}
	}
	// the control: every REGISTERED content type is still built by both, so what is being
	// measured above is the refusal and not an AAD that has stopped answering anything.
	registry := registryConstantsOfType(t, "ContentType")
	built := 0
	for _, name := range slices.Sorted(maps.Keys(registry)) {
		contentType := ContentType(registry[name])
		if _, err := senderDataAAD(groupId, 9, contentType); err != nil {
			t.Errorf("%s: senderDataAAD refused a registered content type: %v", name, err)
			continue
		}
		if _, err := privateContentAAD(groupId, 9, contentType, []byte(authenticatedData)); err != nil {
			t.Errorf("%s: privateContentAAD refused a registered content type: %v", name, err)
			continue
		}
		built += 1
	}
	if refused == 0 || built != len(registry) {
		t.Fatalf("%d unregistered content types were refused and %d of the %d registered ones were built; with either half short this states nothing",
			refused, built, len(registry))
	}
	t.Logf("%d refusals over the two section 6.3 aads, %d registered content types still built", refused, built)
}

// section63HeaderParameterTypes is RFC 9420 section 6.3's shared header written as a Go parameter
// list: the three fields SenderDataAAD is, in the order the presentation language writes them.
//
//	opaque group_id<V>;        ->  []byte
//	uint64 epoch;              ->  uint64
//	ContentType content_type;  ->  ContentType
//
// It is the RFC's structure and not a list of function names, which is the difference that makes
// the derivation below a class rather than a roster: a function joins it by TAKING those three, so
// a third AAD, or a helper somebody adds between the two, is judged without anybody editing this
// file.
var section63HeaderParameterTypes = []string{"[]byte", "uint64", "ContentType"}

// section63HeaderTaker is one function whose parameter list opens with that header: its name, the
// file that declares it, the names it knows those three fields by, how many parameters stand after
// them, and its declaration.
type section63HeaderTaker struct {
	name   string
	file   string
	header []string
	extra  int
	decl   *ast.FuncDecl
}

// parameterNamesAndTypes flattens a parameter list into one entry per NAME.
//
// Per name and not per field, because a grouped declaration hides the difference: `func f(a, b
// []byte)` is ONE field carrying two names, and a reader that walked fields would see one
// parameter where the language has two and read the wrong type off every parameter after it.
func parameterNamesAndTypes(list *ast.FieldList) ([]string, []string) {
	names, declared := []string{}, []string{}
	if list == nil {
		return names, declared
	}
	for _, field := range list.List {
		written := types.ExprString(field.Type)
		if len(field.Names) == 0 {
			names = append(names, "")
			declared = append(declared, written)
			continue
		}
		for _, name := range field.Names {
			names = append(names, name.Name)
			declared = append(declared, written)
		}
	}
	return names, declared
}

// section63HeaderTakers derives that class out of already parsed files.
func section63HeaderTakers(parsed map[string]*ast.File) []section63HeaderTaker {
	found := []section63HeaderTaker{}
	for _, file := range slices.Sorted(maps.Keys(parsed)) {
		for _, declaration := range parsed[file].Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil {
				continue
			}
			names, declared := parameterNamesAndTypes(function.Type.Params)
			if len(names) < len(section63HeaderParameterTypes) {
				continue
			}
			if !slices.Equal(declared[:len(section63HeaderParameterTypes)], section63HeaderParameterTypes) {
				continue
			}
			found = append(found, section63HeaderTaker{
				name:   function.Name.Name,
				file:   file,
				header: names[:len(section63HeaderParameterTypes)],
				extra:  len(names) - len(section63HeaderParameterTypes),
				decl:   function,
			})
		}
	}
	return found
}

// headerFieldsSpentOutsideOneCall reports every way a function of that class spends one of the
// three header fields on something other than a single call to the assembly.
//
// The rule is written on the PARAMETER and not on what the function returns, and that is the whole
// of why this can see what no byte comparison can. A second layout of the shared header produces
// the same octets as the first at every input -- that is what makes it a copy rather than a bug --
// so the only place the duplication is visible is in what the function does with the fields it was
// handed. A function that spends group_id, epoch and content_type on one call to the assembly has
// no way to lay them out itself; one that reads any of them anywhere else has already got a second
// encoder of that header, whether or not it agrees with the first today.
//
// The uses are counted by NODE IDENTITY against the argument subtrees of that call rather than by
// position, so a field wrapped in a conversion or passed through an expression still counts as
// spent there, and a field named again anywhere else in the body does not.
func headerFieldsSpentOutsideOneCall(taker section63HeaderTaker, assembly string) []string {
	inside := map[ast.Node]bool{}
	calls := 0
	ast.Inspect(taker.decl.Body, func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if !isCall {
			return true
		}
		name, isName := call.Fun.(*ast.Ident)
		if !isName || name.Name != assembly {
			return true
		}
		calls += 1
		for _, argument := range call.Args {
			ast.Inspect(argument, func(inner ast.Node) bool {
				inside[inner] = true
				return true
			})
		}
		return true
	})
	loose := []string{}
	if calls != 1 {
		loose = append(loose, fmt.Sprintf("calls %s %d times, want exactly once", assembly, calls))
	}
	for _, field := range taker.header {
		read, outside := 0, 0
		ast.Inspect(taker.decl.Body, func(node ast.Node) bool {
			name, isName := node.(*ast.Ident)
			if !isName || name.Name != field {
				return true
			}
			read += 1
			if !inside[node] {
				outside += 1
			}
			return true
		})
		switch {
		case read == 0:
			loose = append(loose, fmt.Sprintf("never reads %s, so the header field is dropped rather than delegated", field))
		case outside != 0:
			loose = append(loose, fmt.Sprintf("reads %s %d time(s) outside that call", field, outside))
		}
	}
	return loose
}

// section63AssemblyControl is this reader's control, in both directions, which every reader in
// this file carries. A reader that reported nothing would pass over the real duplication silently,
// and one that reported everything would fail against the real file for the wrong reason.
//
// It is parsed and never type checked, so the writer it calls need not exist.
const section63AssemblyControl = `
package control

type ContentType uint8

func controlAssembly(groupId []byte, epoch uint64, contentType ContentType) ([]byte, error) {
	w := newWriter()
	w.WriteOpaque(groupId)
	w.WriteUint64(epoch)
	w.WriteUint8(uint8(contentType))
	return w.Bytes()
}

func controlDelegates(groupId []byte, epoch uint64, contentType ContentType, extra []byte) ([]byte, error) {
	header, err := controlAssembly(groupId, epoch, contentType)
	if err != nil {
		return nil, err
	}
	w := newWriter()
	w.WriteRaw(header)
	w.WriteOpaque(extra)
	return w.Bytes()
}

func controlLaysTheHeaderAgain(groupId []byte, epoch uint64, contentType ContentType, extra []byte) ([]byte, error) {
	w := newWriter()
	w.WriteOpaque(groupId)
	w.WriteUint64(epoch)
	w.WriteUint8(uint8(contentType))
	w.WriteOpaque(extra)
	return w.Bytes()
}

func controlTakesSomethingElse(name string, epoch uint64, contentType ContentType) []byte {
	return nil
}
`

// framingParsedProductionFiles is every production file of this package, parsed, keyed by name.
func framingParsedProductionFiles(t *testing.T) map[string]*ast.File {
	t.Helper()
	parsed := map[string]*ast.File{}
	for _, file := range framingProductionFiles(t) {
		tree, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		parsed[file] = tree
	}
	return parsed
}

// TestEverySectionSixThreeAadAssemblesTheSharedHeaderExactlyOnce is the structural half of the two
// AADs' shape, and it is the half that was advisory.
//
// privateContentAAD's own comment says the single assembly is what makes "the content AAD begins
// with the sender data AAD" true by construction rather than by a test that has to keep being
// true. Nothing held it. The duplication -- the same three fields laid out again by hand inside
// the content AAD, no call to the sender data AAD at all -- is byte for byte identical to the
// delegation at every input, so the goldens pass, the collision sweep passes, the prefix property
// passes and the structural pair test passes. It was caught only, and only for a while, by the
// uncalled-declaration gate noticing that senderDataAAD had lost its one caller, and that guard
// expires the moment task 9 gives the sender data seal a production call.
//
// So the property is stated where a copy cannot satisfy it: on the parameter list. Any function of
// this package taking section 6.3's three header fields may spend them on ONE call to the function
// that takes those three and nothing else, and may not read them anywhere else -- which leaves it
// no way to write the header a second time. connect/message's guardrail G4 is the same rule one
// layer down and was closed the same way: structurally, because a convention between two AADs is
// exactly what a copy-paste does not read.
//
// The direction the parameter list already closes is not restated here.
// TestNeitherSectionSixThreeAadCoversAFieldTheOtherOwns owns it: authenticated_data cannot migrate
// into the sender data AAD because it is not a parameter of it.
func TestEverySectionSixThreeAadAssemblesTheSharedHeaderExactlyOnce(t *testing.T) {
	// the control first, so a reader that has stopped reading fails here rather than reporting
	// the real file clean.
	controlTree, err := parser.ParseFile(token.NewFileSet(), "control.go", section63AssemblyControl,
		parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse the control: %v", err)
	}
	control := section63HeaderTakers(map[string]*ast.File{"control.go": controlTree})
	controlNames := []string{}
	for _, taker := range control {
		controlNames = append(controlNames, taker.name)
	}
	slices.Sort(controlNames)
	if want := []string{"controlAssembly", "controlDelegates", "controlLaysTheHeaderAgain"}; !slices.Equal(controlNames, want) {
		t.Fatalf("the class reader found %v in the control, want %v; a function taking some other three parameters is not in this class and one taking these three is",
			controlNames, want)
	}
	for _, taker := range control {
		loose := headerFieldsSpentOutsideOneCall(taker, "controlAssembly")
		switch taker.name {
		case "controlDelegates":
			if len(loose) != 0 {
				t.Fatalf("the reader reported %v against the control that delegates, so it would report the real file for the wrong reason", loose)
			}
		case "controlLaysTheHeaderAgain":
			if len(loose) == 0 {
				t.Fatal("the reader reported nothing against the control that lays the header out a second time, so it would pass over exactly the duplication it exists to find")
			}
		}
	}

	takers := section63HeaderTakers(framingParsedProductionFiles(t))
	if len(takers) < 2 {
		t.Fatalf("this package declares %d function(s) taking section 6.3's header, and it declares two; the class reader is not reading this package",
			len(takers))
	}
	assemblies := []section63HeaderTaker{}
	for _, taker := range takers {
		if taker.extra == 0 {
			assemblies = append(assemblies, taker)
		}
	}
	if len(assemblies) != 1 {
		names := []string{}
		for _, taker := range assemblies {
			names = append(names, taker.file+"'s "+taker.name)
		}
		t.Fatalf("%v take section 6.3's header and nothing else; exactly one of them is the assembly the others extend, and two are two layouts of one header agreeing until the day one of them changes",
			names)
	}
	assembly := assemblies[0]
	extended := 0
	for _, taker := range takers {
		if taker.name == assembly.name {
			continue
		}
		loose := headerFieldsSpentOutsideOneCall(taker, assembly.name)
		if len(loose) != 0 {
			t.Errorf("%s's %s takes section 6.3's header and %v; those three fields are %s's structure, and a second layout of them here is an associated data that agrees with the first until the day one of them changes, with nothing that round trips able to see the disagreement",
				taker.file, taker.name, loose, assembly.name)
			continue
		}
		extended += 1
	}
	if extended == 0 {
		t.Fatalf("no function extends %s, so this gate ran over the assembly alone and stated nothing", assembly.name)
	}
	t.Logf("%s is the single assembly of section 6.3's header, and %d function(s) extending it spend those fields on it and nowhere else",
		assembly.name, extended)
}

// ---------------------------------------------------------------------------
// MLSMessage, RFC 9420 section 6: the outermost object on the wire
// ---------------------------------------------------------------------------

// mlsMessageArmFields is every arm of MLSMessage, read off the STRUCT rather than listed: each
// pointer field of it is an arm and no other field is.
//
// Derived because the alternative is the enumeration this project has been caught by fourteen
// times. An arm added to that struct joins every sweep below without anybody remembering to add
// it, and an arm removed empties out of them the same way.
func mlsMessageArmFields(t *testing.T) []string {
	t.Helper()
	structure := reflect.TypeOf(MLSMessage{})
	found := []string{}
	for at := 0; at < structure.NumField(); at += 1 {
		if structure.Field(at).Type.Kind() == reflect.Pointer {
			found = append(found, structure.Field(at).Name)
		}
	}
	slices.Sort(found)
	if len(found) == 0 {
		t.Fatal("MLSMessage declares no pointer field, so the arm class is empty and every sweep over it below would report green having probed nothing")
	}
	return found
}

// mlsMessageArmOfEachWireFormat joins the wire format registry to those arms by NAME -- the
// constant WireFormatPublicMessage names the arm PublicMessage -- and requires the join to be a
// bijection.
//
// Both directions, because either alone understates the class. A code point declared with no arm
// is a select case this codec cannot carry; an arm declared with no code point is a field nothing
// can ever put on the wire. A table of five rows reports neither, and the wire format registry is
// open: the next code point IANA assigns arrives as a constant somebody adds here.
func mlsMessageArmOfEachWireFormat(t *testing.T) map[WireFormat]string {
	t.Helper()
	fields := mlsMessageArmFields(t)
	armOf := map[WireFormat]string{}
	for name, value := range registryConstantsOfType(t, "WireFormat") {
		arm, named := strings.CutPrefix(name, "WireFormat")
		if !named || !slices.Contains(fields, arm) {
			t.Fatalf("the wire format %s folds to the arm %q and MLSMessage declares the arms %v; a code point whose arm this struct does not hold is a select case nothing can encode",
				name, arm, fields)
		}
		armOf[WireFormat(value)] = arm
	}
	named := slices.Sorted(maps.Values(armOf))
	if !slices.Equal(named, fields) {
		t.Fatalf("the wire format registry names the arms %v and MLSMessage declares %v; an arm no code point names is a field this codec can never write",
			named, fields)
	}
	return armOf
}

// framingWireFormats is the registry itself, in code point order.
func framingWireFormats(t *testing.T) []WireFormat {
	t.Helper()
	found := []WireFormat{}
	for _, value := range registryConstantsOfType(t, "WireFormat") {
		found = append(found, WireFormat(value))
	}
	slices.Sort(found)
	return found
}

// mlsMessageCanonicalArm is one arm value put through its OWN codec once, before the wrapper is
// anywhere in the picture.
//
// A nil vector and an empty one encode to the same octets, so a fixture written with a nil
// extensions vector is a value that no decode of any bytes can produce. Comparing a decoded
// message against such a fixture fails for a reason that has nothing to do with the structure
// under test -- measured here: the GroupInfo and KeyPackage fixtures of this package both carry
// one, and both round tripped BYTE EXACTLY while failing a value comparison. Canonicalizing
// through the arm's own codec keeps the equality claim below about MLSMessage: whatever an arm
// does to its own fields it has already done before this wrapper is reached, and a wrapper that
// dropped or moved a field is still seen.
func mlsMessageCanonicalArm[T any, PT interface {
	*T
	syntax.Codec
}](t *testing.T, value PT) PT {
	t.Helper()
	encoded, err := syntax.Marshal(value)
	if err != nil {
		t.Fatalf("encode a %T arm fixture: %v", value, err)
	}
	decoded := PT(new(T))
	if err := syntax.Unmarshal(encoded, decoded); err != nil {
		t.Fatalf("decode a %T arm fixture: %v", value, err)
	}
	return decoded
}

// mlsMessageArmValues fills one arm each, with a value distinct from every other arm's so that a
// message carrying the wrong one is visibly the wrong one rather than an empty structure.
func mlsMessageArmValues(t *testing.T) map[string]func(*MLSMessage) {
	t.Helper()
	groupInfo, welcome := testGroupInfo(), testWelcome()
	keyPackage := KeyPackage{
		Version:     ProtocolVersionMls10,
		CipherSuite: CipherSuiteX25519ChaCha20Sha256Ed25519,
		InitKey:     HpkePublicKey{0xaa, 0xbb},
		LeafNode:    *testLeafNodeOfSource(LeafNodeSourceKeyPackage),
		Signature:   []byte{0xcc},
	}
	canonicalPublic := mlsMessageCanonicalArm[PublicMessage](t, framingTestPublicMessage())
	canonicalPrivate := mlsMessageCanonicalArm[PrivateMessage](t, framingTestPrivateMessage())
	canonicalWelcome := mlsMessageCanonicalArm[Welcome](t, &welcome)
	canonicalGroupInfo := mlsMessageCanonicalArm[GroupInfo](t, &groupInfo)
	canonicalKeyPackage := mlsMessageCanonicalArm[KeyPackage](t, &keyPackage)
	return map[string]func(*MLSMessage){
		"PublicMessage":  func(message *MLSMessage) { held := *canonicalPublic; message.PublicMessage = &held },
		"PrivateMessage": func(message *MLSMessage) { held := *canonicalPrivate; message.PrivateMessage = &held },
		"Welcome":        func(message *MLSMessage) { held := *canonicalWelcome; message.Welcome = &held },
		"GroupInfo":      func(message *MLSMessage) { held := *canonicalGroupInfo; message.GroupInfo = &held },
		"KeyPackage":     func(message *MLSMessage) { held := *canonicalKeyPackage; message.KeyPackage = &held },
	}
}

// framingTestMlsMessage is a valid message under one wire format: the mls10 version, that wire
// format, and exactly the arm it names.
func framingTestMlsMessage(t *testing.T, wireFormat WireFormat) *MLSMessage {
	t.Helper()
	arm, named := mlsMessageArmOfEachWireFormat(t)[wireFormat]
	if !named {
		t.Fatalf("no arm of MLSMessage is registered at wire format %#04x", wireFormat)
	}
	populate, held := mlsMessageArmValues(t)[arm]
	if !held {
		t.Fatalf("this file has no way to fill the %s arm", arm)
	}
	message := &MLSMessage{Version: ProtocolVersionMls10, WireFormat: wireFormat}
	populate(message)
	return message
}

// framingMlsMessageArm is the arm a message carries, found by walking the pointer fields rather
// than by a switch, so an arm added to the struct is reached here without this file being told.
func framingMlsMessageArm(t *testing.T, message *MLSMessage) (string, syntax.Codec) {
	t.Helper()
	value := reflect.ValueOf(message).Elem()
	found, arm := "", syntax.Codec(nil)
	for _, field := range mlsMessageArmFields(t) {
		held := value.FieldByName(field)
		if held.IsNil() {
			continue
		}
		if found != "" {
			t.Fatalf("this message carries the %s arm and the %s arm at once", found, field)
		}
		codec, isCodec := held.Interface().(syntax.Codec)
		if !isCodec {
			t.Fatalf("the %s arm of MLSMessage is not a syntax.Codec, so its own encoding cannot be taken", field)
		}
		found, arm = field, codec
	}
	if found == "" {
		t.Fatal("this message carries no arm at all")
	}
	return found, arm
}

// mlsMessageHeaderOf is version || wire_format written out beside the codec, off the registry
// rather than off the encoder.
func mlsMessageHeaderOf(wireFormat WireFormat) []byte {
	return []byte{
		byte(ProtocolVersionMls10 >> 8), byte(ProtocolVersionMls10),
		byte(wireFormat >> 8), byte(wireFormat),
	}
}

// TestTheMlsMessageEncodingIsTheVersionThenTheWireFormatThenTheArmAlone holds section 6's field
// order against a layout written beside the codec rather than read back through it.
//
// A round trip sees none of this. Version and wire_format are ADJACENT uint16s, so an encoder
// that wrote them the other way round agrees with its own decoder perfectly, produces a message
// of exactly the right length, and is read by every other implementation as a protocol version
// nobody has defined -- which is the same defect measured on KeyPackage one file over, where the
// two swapped fields survived the whole of mls and message green.
//
// The tail is the arm's OWN encoding, taken through the arm's own codec: what this wrapper
// contributes over its five arms is the four octet header and the fact that it adds nothing else,
// and a wrapper that wrote a length prefix around the arm -- which is what a reader reaching for
// WriteOpaque would produce -- round trips against itself and against nobody.
func TestTheMlsMessageEncodingIsTheVersionThenTheWireFormatThenTheArmAlone(t *testing.T) {
	swept := 0
	for _, wireFormat := range framingWireFormats(t) {
		message := framingTestMlsMessage(t, wireFormat)
		encoded, err := MarshalMLSMessage(message)
		if err != nil {
			t.Fatalf("a message of wire format %#04x did not encode: %v", wireFormat, err)
		}
		header := mlsMessageHeaderOf(wireFormat)
		if !bytes.HasPrefix(encoded, header) {
			t.Errorf("a message of wire format %#04x opens with %x and RFC 9420 section 6 writes %x -- the mls10 version, then the wire format",
				wireFormat, encoded[:min(len(encoded), len(header))], header)
			continue
		}
		field, arm := framingMlsMessageArm(t, message)
		body, err := syntax.Marshal(arm)
		if err != nil {
			t.Fatalf("the %s arm of a %#04x message did not encode on its own: %v", field, wireFormat, err)
		}
		if !bytes.Equal(encoded[len(header):], body) {
			t.Errorf("a message of wire format %#04x carries %x behind its header and the %s arm encodes on its own to %x; this wrapper is the header and nothing else",
				wireFormat, encoded[len(header):], field, body)
			continue
		}
		swept += 1
	}
	if swept != len(framingWireFormats(t)) {
		t.Fatalf("%d of %d wire formats were laid out, so this gate did not cover the registry", swept, len(framingWireFormats(t)))
	}
}

// TestMLSMessageRoundTripsEveryWireFormatThroughItsEntryPoints is the round trip over the whole
// registry, through the two free functions the rest of the system calls.
//
// The arm exclusivity is the half a round trip on its own does not state: a decoder that
// populated two arms, or that left a previous value's arm standing beside the one it read, round
// trips byte exactly, because the encoder writes only the arm the discriminant names. What that
// costs is a value whose Welcome field is a Welcome nobody sent.
func TestMLSMessageRoundTripsEveryWireFormatThroughItsEntryPoints(t *testing.T) {
	armOf := mlsMessageArmOfEachWireFormat(t)
	fields := mlsMessageArmFields(t)
	for _, wireFormat := range framingWireFormats(t) {
		message := framingTestMlsMessage(t, wireFormat)
		encoded, err := MarshalMLSMessage(message)
		if err != nil {
			t.Fatalf("marshal a %#04x message: %v", wireFormat, err)
		}
		decoded, err := ParseMLSMessage(encoded)
		if err != nil {
			t.Fatalf("parse a %#04x message: %v", wireFormat, err)
		}
		if decoded.Version != ProtocolVersionMls10 || decoded.WireFormat != wireFormat {
			t.Errorf("a %#04x message decoded to version %d wire format %#04x", wireFormat, decoded.Version, decoded.WireFormat)
			continue
		}
		held := reflect.ValueOf(decoded).Elem()
		for _, field := range fields {
			populated := !held.FieldByName(field).IsNil()
			if populated != (field == armOf[wireFormat]) {
				t.Errorf("a %#04x message decoded with its %s arm populated=%v; wire format %#04x names the %s arm and exactly that one",
					wireFormat, field, populated, wireFormat, armOf[wireFormat])
			}
		}
		if !reflect.DeepEqual(decoded, message) {
			t.Errorf("a %#04x message decoded to %+v, want %+v", wireFormat, decoded, message)
			continue
		}
		reencoded, err := MarshalMLSMessage(decoded)
		if err != nil {
			t.Fatalf("re-marshal a %#04x message: %v", wireFormat, err)
		}
		if !bytes.Equal(reencoded, encoded) {
			t.Errorf("a %#04x message re-encoded to %x, want %x", wireFormat, reencoded, encoded)
		}
	}
}

// TestEveryWireFormatCarriesExactlyTheArmItNames is section 6's select, stated over the cross
// product of the five code points with the five arms rather than over the diagonal.
//
// The per-case half is what stands between MLSMessage{WireFormat: WireFormatWelcome} and
// self.Welcome.MarshalMLS(w) on a nil pointer, which is a panic raised by a value a caller
// assembled and not by anything off the wire -- but it is raised inside the one function every
// outbound byte of this system leaves through.
//
// The COUNT is the half a per-case check does not give at all, and ProposalOrRef measured what it
// is worth one layer down: a value carrying two arms encodes to the one its discriminant names,
// so two distinct values share one encoding and the arm nobody named is dropped in silence. Here
// the dropped arm is dropped out of the frame a peer receives and stores, and the sender's own
// re-read of its own message finds a field it never sent.
func TestEveryWireFormatCarriesExactlyTheArmItNames(t *testing.T) {
	armOf := mlsMessageArmOfEachWireFormat(t)
	fields := mlsMessageArmFields(t)
	populate := mlsMessageArmValues(t)
	if !slices.Equal(slices.Sorted(maps.Keys(populate)), fields) {
		t.Fatalf("this file fills %v and MLSMessage declares the arms %v; an arm with no way to fill it is one every sweep here skips in silence",
			slices.Sorted(maps.Keys(populate)), fields)
	}
	formats := framingWireFormats(t)

	// the control: the diagonal encodes, or every refusal below is a refusal of a fixture this
	// file built wrong rather than of the mismatch it is about
	for _, wireFormat := range formats {
		matching := &MLSMessage{Version: ProtocolVersionMls10, WireFormat: wireFormat}
		populate[armOf[wireFormat]](matching)
		if _, err := MarshalMLSMessage(matching); err != nil {
			t.Fatalf("a message of wire format %#04x carrying its own %s arm did not encode: %v",
				wireFormat, armOf[wireFormat], err)
		}
	}

	foreign, armless, pairs := 0, 0, 0
	for _, wireFormat := range formats {
		// no arm at all, for every code point rather than for the one a case would have probed
		empty := &MLSMessage{Version: ProtocolVersionMls10, WireFormat: wireFormat}
		encoded, raised, err := marshalCatchingPanic(empty)
		if raised != nil {
			t.Errorf("a message of wire format %#04x with no arm at all panicked in the encoder: %v; the arm the discriminant names was dereferenced without being checked for",
				wireFormat, raised)
		} else if !errors.Is(err, ErrContentArmMismatch) {
			t.Errorf("a message of wire format %#04x with no arm at all encoded to %x with %v, want ErrContentArmMismatch",
				wireFormat, encoded, err)
		} else {
			armless += 1
		}

		for _, field := range fields {
			if field == armOf[wireFormat] {
				continue
			}
			// the foreign arm: the count is satisfied, so the only thing that can refuse this
			// is the per-case check for the arm the discriminant names
			mismatched := &MLSMessage{Version: ProtocolVersionMls10, WireFormat: wireFormat}
			populate[field](mismatched)
			encoded, raised, err := marshalCatchingPanic(mismatched)
			if raised != nil {
				t.Errorf("a message of wire format %#04x carrying the %s arm panicked in the encoder: %v",
					wireFormat, field, raised)
				continue
			}
			if !errors.Is(err, ErrContentArmMismatch) {
				t.Errorf("a message of wire format %#04x carrying the %s arm encoded to %x with %v, want ErrContentArmMismatch; the arm it names is in neither those bytes nor the frame a peer would receive",
					wireFormat, field, encoded, err)
				continue
			}
			foreign += 1
		}

		// and more than one arm, over every PAIR the derivation produces
		for i := 0; i < len(fields); i += 1 {
			for j := i + 1; j < len(fields); j += 1 {
				both := &MLSMessage{Version: ProtocolVersionMls10, WireFormat: wireFormat}
				populate[fields[i]](both)
				populate[fields[j]](both)
				encoded, raised, err := marshalCatchingPanic(both)
				if raised != nil {
					t.Errorf("a message of wire format %#04x carrying both the %s and the %s arm panicked in the encoder: %v",
						wireFormat, fields[i], fields[j], raised)
					continue
				}
				if !errors.Is(err, ErrContentArmMismatch) {
					t.Errorf("a message of wire format %#04x carrying both the %s and the %s arm encoded to %x with %v; one of the two is nowhere in those octets, so it encodes identically to the value that never carried it",
						wireFormat, fields[i], fields[j], encoded, err)
					continue
				}
				pairs += 1
			}
		}
	}
	wantForeign, wantPairs := len(formats)*(len(fields)-1), len(formats)*len(fields)*(len(fields)-1)/2
	if armless != len(formats) || foreign != wantForeign || pairs != wantPairs {
		t.Errorf("%d armless, %d foreign and %d paired refusals over %d code points and %d arms, want %d, %d and %d",
			armless, foreign, pairs, len(formats), len(fields), len(formats), wantForeign, wantPairs)
	}
	t.Logf("%d code points x %d arms: %d foreign arms, %d armless and %d pairs refused",
		len(formats), len(fields), foreign, armless, pairs)
}

// marshalIntoCatchingPanic encodes into a Writer the CALLER holds, and reports a panic rather
// than letting it take the test binary.
//
// The Writer is the point. syntax.Marshal and MarshalLimit drop the buffer whenever the encode
// errors -- deliberately, so that a caller cannot take a partial encoding by accident -- so an
// assertion that a refusal "wrote nothing" made through either of them compares nil against nil
// and cannot fail. Measured here: both of the write-nothing checks below were written that way
// first, and moving the header write to the top of MarshalMLS left them green.
func marshalIntoCatchingPanic(w *syntax.Writer, codec syntax.Marshaler) (raised any, err error) {
	defer func() { raised = recover() }()
	return nil, codec.MarshalMLS(w)
}

// framingMlsMessageBody is a whole valid arm encoding, minus the four octet header, for a decoder
// that is being asked to refuse a discriminant rather than to run out of input.
func framingMlsMessageBody(t *testing.T, wireFormat WireFormat) []byte {
	t.Helper()
	encoded, err := MarshalMLSMessage(framingTestMlsMessage(t, wireFormat))
	if err != nil {
		t.Fatalf("encode the %#04x body this sweep stands behind: %v", wireFormat, err)
	}
	return encoded[len(mlsMessageHeaderOf(wireFormat)):]
}

// TestEveryUnregisteredWireFormatIsRefusedWhateverStandsBehindIt sweeps the whole sixteen bit
// code point space, less the five the registry declares, on both halves of the codec.
//
// The whole space rather than a specimen, for the reason p6 task 1 swept SenderType's 252
// undeclared octets: the width is what the registry is open over, and a decoder that refused
// 0x0063 while accepting 0x0100 is one nothing built from a handful of samples would report.
//
// What the four TAILS state is the property that matters at this layer. This discriminant selects
// which of five parsers runs on attacker controlled bytes, so the refusal has to be the
// discriminant's own and not a consequence of the arm behind it failing to parse: the same code
// point is refused identically with a whole valid message behind it, with garbage behind it, and
// with nothing behind it at all. A decoder that entered an arm first and let the arm decide would
// answer three different errors here, and for the tail that happened to parse it would answer
// none.
func TestEveryUnregisteredWireFormatIsRefusedWhateverStandsBehindIt(t *testing.T) {
	body := framingMlsMessageBody(t, WireFormatPrivateMessage)
	tails := []struct {
		what   string
		octets []byte
	}{
		{"nothing at all", nil},
		{"a whole valid private message", body},
		{"a single octet", []byte{0xff}},
		{"sixty four octets of 0xff", bytes.Repeat([]byte{0xff}, 64)},
	}
	unregistered := undeclaredCodePointsOf(t, "WireFormat")
	if len(unregistered) == 0 {
		t.Fatal("the wire format registry declares every code point of its width, so this sweep has nothing to refuse")
	}
	refused := 0
	for _, codePoint := range unregistered {
		for _, tail := range tails {
			frame := append(mlsMessageHeaderOf(WireFormat(codePoint)), tail.octets...)
			message, err := ParseMLSMessage(frame)
			if !errors.Is(err, ErrUnknownWireFormat) {
				t.Fatalf("a frame naming wire format %d with %s behind it answered %v, want ErrUnknownWireFormat; the discriminant chooses which parser runs on these bytes, so it is refused on its own or it is not refused at all",
					codePoint, tail.what, err)
			}
			if message != nil {
				t.Fatalf("a frame naming wire format %d with %s behind it was refused with %v and handed back %+v; a refused parse that returns a message hands the caller fields out of bytes this package would not accept",
					codePoint, tail.what, err, message)
			}
			refused += 1
		}
		// the encoder half of the same code point, with and without an arm standing beside it.
		// The wire format is refused ahead of the arm check, so a message this package could
		// never have built is still refused as the code point it names.
		for _, populated := range []bool{false, true} {
			message := &MLSMessage{Version: ProtocolVersionMls10, WireFormat: WireFormat(codePoint)}
			if populated {
				message.PrivateMessage = framingTestPrivateMessage()
			}
			writer := syntax.NewWriter()
			raised, err := marshalIntoCatchingPanic(writer, message)
			if raised != nil {
				t.Fatalf("encoding a message at unregistered wire format %d panicked: %v", codePoint, raised)
			}
			if !errors.Is(err, ErrUnknownWireFormat) {
				t.Fatalf("encoding a message at unregistered wire format %d (arm populated=%v) answered %v, want ErrUnknownWireFormat",
					codePoint, populated, err)
			}
			if writer.Len() != 0 {
				t.Fatalf("encoding a message at unregistered wire format %d left %d octets on the caller's Writer before refusing; this header is what every peer parses before it parses anything else",
					codePoint, writer.Len())
			}
			refused += 1
		}
	}
	t.Logf("%d refusals over %d unregistered wire formats, on both halves and behind %d different tails",
		refused, len(unregistered), len(tails))
}

// TestEveryProtocolVersionButMls10IsRefusedAheadOfTheWireFormat sweeps the whole version space on
// both halves, and states the ORDER the two checks stand in.
//
// Spec A's v1 profile pins mls10, and a decoder that ignored the field would accept a message
// from a protocol this package does not implement -- reading its five arm grammar out of bytes
// some later version writes differently. The whole space rather than one wrong value, because the
// field is sixteen bits and its registry is open.
//
// The ORDER is asserted rather than assumed because both orders pass a test of either check
// alone. version stands ahead of wire_format in the structure, so a frame that is wrong in both
// is refused as the version: the reverse order tells a peer running a later protocol that its
// WIRE FORMAT was unknown, which is a true statement about a field this implementation had no
// business reading yet and which sends whoever debugs it to the wrong registry.
func TestEveryProtocolVersionButMls10IsRefusedAheadOfTheWireFormat(t *testing.T) {
	body := framingMlsMessageBody(t, WireFormatPrivateMessage)
	unregistered := WireFormat(undeclaredCodePointsOf(t, "WireFormat")[0])

	// the control: the same body under mls10 and a registered wire format is ACCEPTED, so every
	// refusal below is a refusal of the version rather than of the fixture
	accepted := append(mlsMessageHeaderOf(WireFormatPrivateMessage), body...)
	if _, err := ParseMLSMessage(accepted); err != nil {
		t.Fatalf("the mls10 control frame this sweep varies the version of was refused: %v", err)
	}

	swept := 0
	for value := 0; value <= 0xffff; value += 1 {
		if ProtocolVersion(value) == ProtocolVersionMls10 {
			continue
		}
		header := []byte{byte(value >> 8), byte(value), byte(WireFormatPrivateMessage >> 8), byte(WireFormatPrivateMessage)}
		message, err := ParseMLSMessage(append(header, body...))
		if !errors.Is(err, ErrUnsupportedVersion) {
			t.Fatalf("a frame naming protocol version %d over a registered wire format answered %v, want ErrUnsupportedVersion", value, err)
		}
		if message != nil {
			t.Fatalf("a frame naming protocol version %d was refused with %v and handed back %+v", value, err, message)
		}
		if !namesTheNumber(err.Error(), uint64(value)) {
			t.Fatalf("a frame naming protocol version %d was refused with %q, which does not name it; the octets are what the caller has off the wire",
				value, err)
		}

		// the order: wrong in both fields is refused as the version, which is the field that
		// stands first
		both := []byte{byte(value >> 8), byte(value), byte(unregistered >> 8), byte(unregistered)}
		_, err = ParseMLSMessage(append(both, body...))
		if !errors.Is(err, ErrUnsupportedVersion) || errors.Is(err, ErrUnknownWireFormat) {
			t.Fatalf("a frame naming protocol version %d AND unregistered wire format %d answered %v; version stands ahead of wire_format on the wire and is refused ahead of it here",
				value, unregistered, err)
		}

		// and the encoder, which refuses the version before it looks at the wire format or the
		// arms and writes nothing
		outbound := framingTestMlsMessage(t, WireFormatPrivateMessage)
		outbound.Version = ProtocolVersion(value)
		writer := syntax.NewWriter()
		if _, err := marshalIntoCatchingPanic(writer, outbound); !errors.Is(err, ErrUnsupportedVersion) {
			t.Fatalf("encoding a message at protocol version %d answered %v, want ErrUnsupportedVersion", value, err)
		}
		if writer.Len() != 0 {
			t.Fatalf("encoding a message at protocol version %d left %d octets on the caller's Writer before refusing",
				value, writer.Len())
		}
		outbound.WireFormat = unregistered
		outbound.PrivateMessage = nil
		if _, err := MarshalMLSMessage(outbound); !errors.Is(err, ErrUnsupportedVersion) || errors.Is(err, ErrUnknownWireFormat) {
			t.Fatalf("encoding a message wrong in the version (%d), the wire format (%d) and the arms answered %v; the version is refused first",
				value, unregistered, err)
		}
		swept += 1
	}
	if swept != 0xffff {
		t.Fatalf("%d versions swept, and the field is sixteen bits wide with one value registered", swept)
	}
}

// TestParseMLSMessageRefusesEveryTruncationAndEveryTrailingOctet is syntax rule 3 at the one
// place it is enforced.
//
// Full consumption is this entry point's own obligation rather than the arm decoders': a nested
// codec reads its fields and stops, and the reader it leaves behind is the caller's problem. So a
// frame with a tail is refused HERE or nowhere -- and a decoder that ignored the tail would let
// one frame be read as one message by this implementation and as something else by an
// implementation that framed it differently, over bytes a peer stores and forwards.
//
// Every trailing OCTET rather than one, because a tail of 0x00 is the value a codec that reached
// for a fixed width read is likeliest to swallow.
func TestParseMLSMessageRefusesEveryTruncationAndEveryTrailingOctet(t *testing.T) {
	truncations, tails := 0, 0
	for _, wireFormat := range framingWireFormats(t) {
		encoded, err := MarshalMLSMessage(framingTestMlsMessage(t, wireFormat))
		if err != nil {
			t.Fatalf("encode a %#04x message: %v", wireFormat, err)
		}
		// the control: the whole thing is accepted, or the truncations below refuse something
		// this fixture broke rather than a short frame
		if _, err := ParseMLSMessage(encoded); err != nil {
			t.Fatalf("the complete %#04x message this sweep truncates was refused: %v", wireFormat, err)
		}
		for at := 0; at < len(encoded); at += 1 {
			message, err := ParseMLSMessage(encoded[:at])
			if err == nil {
				t.Fatalf("a %#04x message truncated to %d of %d octets was accepted as %+v", wireFormat, at, len(encoded), message)
			}
			if message != nil {
				t.Fatalf("a %#04x message truncated to %d octets was refused with %v and handed back %+v", wireFormat, at, err, message)
			}
			truncations += 1
		}
		for octet := 0; octet <= 0xff; octet += 1 {
			message, err := ParseMLSMessage(append(slices.Clone(encoded), byte(octet)))
			if err == nil {
				t.Fatalf("a %#04x message with a trailing %#02x was accepted as %+v; the tail is not part of any structure this package encodes",
					wireFormat, octet, message)
			}
			if message != nil {
				t.Fatalf("a %#04x message with a trailing %#02x was refused with %v and handed back %+v", wireFormat, octet, err, message)
			}
			tails += 1
		}
	}
	t.Logf("%d truncations and %d trailing octets refused over %d wire formats", truncations, tails, len(framingWireFormats(t)))
}

// framingHostileMlsMessages is a deterministic corpus of things that are not messages: every
// truncation of every valid encoding, three corruptions at every octet of each, the bare headers,
// and a pseudo random stream.
//
// Deterministic because a randomized corpus that fails once is a corpus nobody can re-run, and
// generated rather than written out because the class this is about is "input", which nobody can
// list. The fuzz target below is the same property under a mutating engine; this is the half that
// runs in every suite.
func framingHostileMlsMessages(t *testing.T) [][]byte {
	t.Helper()
	corpus := [][]byte{nil, {}, {0x00}, {0x00, 0x01}, {0x00, 0x01, 0x00}}
	for _, wireFormat := range framingWireFormats(t) {
		corpus = append(corpus, mlsMessageHeaderOf(wireFormat))
		encoded, err := MarshalMLSMessage(framingTestMlsMessage(t, wireFormat))
		if err != nil {
			t.Fatalf("encode a %#04x message: %v", wireFormat, err)
		}
		corpus = append(corpus, encoded)
		for at := 0; at < len(encoded); at += 1 {
			corpus = append(corpus, slices.Clone(encoded[:at]))
			for _, mask := range []byte{0x01, 0x80, 0xff} {
				corrupted := slices.Clone(encoded)
				corrupted[at] ^= mask
				corpus = append(corpus, corrupted)
			}
		}
	}
	// an xorshift rather than math/rand, so the stream is fixed by this file and cannot move
	// with the standard library's generator
	state := uint32(0x5a5b5c5d)
	next := func() byte {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		return byte(state)
	}
	for size := 0; size < 256; size += 1 {
		noise := make([]byte, size%97)
		for at := range noise {
			noise[at] = next()
		}
		corpus = append(corpus, noise)
		// and the same noise behind a header this package would accept, which is what reaches
		// an arm decoder rather than stopping at the discriminant
		corpus = append(corpus, append(mlsMessageHeaderOf(WireFormatPrivateMessage), noise...))
	}
	return corpus
}

// parseCatchingPanic runs the inbound path and reports a panic as a value rather than letting it
// take the test binary, because "refused" and "crashed" are different answers and the message has
// to say which one happened.
func parseCatchingPanic(data []byte) (message *MLSMessage, raised any, err error) {
	defer func() { raised = recover() }()
	message, err = ParseMLSMessage(data)
	return message, nil, err
}

// TestParseMLSMessageAnswersAnErrorRatherThanAPanicOnAnyInput is gate 4 property 1 at the outermost
// decoder, plus the two things a refusal owes its caller.
//
// A panic here is an availability bug reachable by anyone who can address a frame at this
// process: the input is whatever arrived, nothing has been authenticated yet, and a nil
// dereference inside the parse takes the process rather than the connection.
//
// The two obligations are stated together because the same input states both. A refusal returns
// no message, so there is nothing partially populated for a caller to log, match or forward; and
// an ACCEPTED input re-encodes to exactly the octets it came from, because MLS signs over
// serialized forms and a decoder that accepted two encodings of one message is a signature bypass
// rather than a leniency.
//
// The accepted count is the reachability guard p1 measured the need for: uniform random bytes
// reach a round trip property 14 times in 4096, so a corpus that never decodes reports green
// having evaluated nothing. Corruptions of valid encodings are what buy reachability here.
func TestParseMLSMessageAnswersAnErrorRatherThanAPanicOnAnyInput(t *testing.T) {
	corpus := framingHostileMlsMessages(t)
	accepted, refused := 0, 0
	for _, input := range corpus {
		message, raised, err := parseCatchingPanic(input)
		if raised != nil {
			t.Fatalf("%d octets %x panicked in the inbound path: %v", len(input), input, raised)
		}
		if err != nil {
			if message != nil {
				t.Fatalf("%d octets %x were refused with %v and handed back %+v", len(input), input, err, message)
			}
			refused += 1
			continue
		}
		reencoded, err := MarshalMLSMessage(message)
		if err != nil {
			t.Fatalf("%d octets %x decoded and would not re-encode: %v", len(input), input, err)
		}
		if !bytes.Equal(reencoded, input) {
			t.Fatalf("%d octets %x decoded and re-encoded to %x; a message with two encodings is a message whose signature covers whichever one the sender chose",
				len(input), input, reencoded)
		}
		accepted += 1
	}
	if accepted == 0 {
		t.Fatalf("not one of %d hostile inputs decoded, so the round trip half of this property reached nothing", len(corpus))
	}
	t.Logf("%d hostile inputs: %d refused without a panic and without a message, %d decoded and re-encoded to their own octets",
		len(corpus), refused, accepted)
}

// FuzzParseMLSMessage is that property under a mutating engine.
//
// Seeded with valid encodings of four of the five arms rather than with nothing, for the reason
// p1 measured and syntax/fuzz_test.go records: uniform random bytes almost never decode as a
// structure of this depth, so an unseeded target spends its whole budget rediscovering the four
// octet header and never once evaluates the round trip half of its own property. The hostile
// literals are there because the seeds cannot produce them: no encoding this package writes
// carries a presence octet of 2 or a length prefix longer than its own content, and only a
// mutation or an attacker presents one.
//
// The three counts at the foot are what HOLD that seeding, and their absence is what this target
// was found without: it was the only fuzz target in this package that neither counted what it
// decoded nor failed at zero reachability. Measured rather than argued. Uniform random bytes
// reach a decode of this grammar 0 times in 500000 draws, so an unseeded target here would be
// wholly vacuous; a single bit flipped in a valid encoding still decodes 73% of the time, so the
// f.Add encodings are the entire reason anything is ever evaluated. With them dropped the target
// goes on passing while proving nothing at all, and so does a stricter header that stopped every
// mutant decoding.
//
// The counts are read from the FAR SIDE of the engine, which is fuzzTheCommittedSeedCorpus's
// correction one file over: a counter incremented in the same loop body as the f.Add it would be
// guarding stays green when that f.Add becomes a discard. go test runs each seed of a target as
// a subtest, so the property below is called once per seed, and executed against handed is that
// hand off asserted where a plain go test run reaches it. seedCorpusExecutionIsObservable is the
// same guard for the same two invocations -- under -fuzz the seeds are fanned out to worker
// processes and this arm is not the one taken.
//
// This target's seeds are built here rather than committed under testdata/corpus, which is the
// one place it departs from the four targets that go through fuzzTheCommittedSeedCorpus. They
// are encodings of THIS package's own fixtures taken through its own encoder, so a committed
// copy would be a second transcription of them that goes stale the day an arm gains a field --
// and a stale seed decodes to nothing, which is exactly the silence the counts below exist to
// break. What a committed corpus buys those four targets is inputs no fixture produces; what it
// would cost here is the staleness. The assertion is the part that has to be present either way.
func FuzzParseMLSMessage(f *testing.F) {
	handed := 0
	for _, seed := range [][]byte{
		nil,
		{0x00, 0x01},
		{0x00, 0x01, 0x00, 0x02},
		{0x00, 0x02, 0x00, 0x02},
		{0x00, 0x01, 0x00, 0x63},
		{0x00, 0x01, 0xff, 0xff, 0xff},
	} {
		f.Add(seed)
		handed += 1
	}
	welcome, groupInfo := testWelcome(), testGroupInfo()
	for _, message := range []*MLSMessage{
		{Version: ProtocolVersionMls10, WireFormat: WireFormatPublicMessage, PublicMessage: framingTestPublicMessage()},
		{Version: ProtocolVersionMls10, WireFormat: WireFormatPrivateMessage, PrivateMessage: framingTestPrivateMessage()},
		{Version: ProtocolVersionMls10, WireFormat: WireFormatWelcome, Welcome: &welcome},
		{Version: ProtocolVersionMls10, WireFormat: WireFormatGroupInfo, GroupInfo: &groupInfo},
	} {
		encoded, err := MarshalMLSMessage(message)
		if err != nil {
			f.Fatalf("seed a %#04x message: %v", message.WireFormat, err)
		}
		f.Add(encoded)
		handed += 1
	}
	// plain counters and not atomics, for fuzzTheCommittedSeedCorpus's reason: the arm of
	// testing.F.Fuzz that runs the seeds runs them one at a time in this process and joins each
	// before starting the next, and nothing here calls t.Parallel
	executed, decoded := 0, 0
	f.Fuzz(func(t *testing.T, data []byte) {
		executed += 1
		message, err := ParseMLSMessage(data)
		if err != nil {
			if message != nil {
				t.Fatalf("%d octets %x refused with %v and handed back %+v", len(data), data, err, message)
			}
			return
		}
		reencoded, err := MarshalMLSMessage(message)
		if err != nil {
			t.Fatalf("%d octets %x decoded and would not re-encode: %v", len(data), data, err)
		}
		if !bytes.Equal(reencoded, data) {
			t.Fatalf("%d octets %x re-encoded to %x", len(data), data, reencoded)
		}
		decoded += 1
	})
	if !seedCorpusExecutionIsObservable() {
		return
	}
	// fewer rather than not equal, because testdata/fuzz may hold inputs the engine FOUND, and
	// those are executions this hand off did not supply
	if executed < handed {
		f.Errorf("%d seeds were handed to the engine and it ran the property %d times; the seeds are reaching no engine, and the round trip half of this property is being stated over bytes nothing consumes",
			handed, executed)
	}
	if decoded == 0 {
		f.Errorf("not one of the %d inputs the engine ran decoded, so this target evaluated the refusal half of its property and nothing else; the seeded encodings are what buy reachability here and uniform random bytes reach this grammar 0 times in 500000",
			executed)
	}
	f.Logf("%d seeds handed over, %d engine executions, %d of them decoding and re-encoding to their own octets",
		handed, executed, decoded)
}

// TestParseMLSMessageCannotCarryThisProductsOwnGroupInfoOrWelcome is the ceiling this entry point
// inherits, recorded as a measurement rather than left in a comment.
//
// welcome_wire_test.go measures it one layer down: a ratchet_tree extension travels as an
// opaque<V>, so a GroupInfo carrying this product's own thousand leaf tree is larger than
// MaxVectorLength and neither half of that codec can reach it at the default limit. This is the
// same fact at the function every byte off the wire passes through, and it is worth stating
// separately because ParseMLSMessage takes no limit argument: whoever writes the group lifecycle
// cannot raise this one, they have to add an entry point of their own.
//
// The bound is deliberately not raised here. A Welcome is decoded by a party who is not yet a
// member, with no group state to check it against and every length in it chosen by whoever sent
// it, so a raised limit at the outermost decoder would be an acceptance rule handed to a stranger
// over the largest allocation the structure has.
//
// Behind a size guard, for welcome_wire_test.go's reason: a fixture that shrank below
// MaxVectorLength would leave this passing against either limit while stating nothing.
func TestParseMLSMessageCannotCarryThisProductsOwnGroupInfoOrWelcome(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	groupInfo, treeBody := welcomeWireProductGroupInfo(t, crypto)
	if len(treeBody) <= syntax.MaxVectorLength {
		t.Fatalf("the %d leaf ratchet tree encodes to %d octets and MaxVectorLength is %d; this fixture no longer separates the two limits",
			productGroupLeafCount, len(treeBody), syntax.MaxVectorLength)
	}
	message := &MLSMessage{Version: ProtocolVersionMls10, WireFormat: WireFormatGroupInfo, GroupInfo: &groupInfo}

	if _, err := MarshalMLSMessage(message); !errors.Is(err, syntax.ErrLengthExceedsMax) {
		t.Fatalf("encoding a group info carrying a %d leaf tree through MarshalMLSMessage answered %v, want ErrLengthExceedsMax; this is the ceiling p7 has to raise somewhere else",
			productGroupLeafCount, err)
	}
	encoded, err := syntax.MarshalLimit(message, syntax.MaxRatchetTreeLength)
	if err != nil {
		t.Fatalf("the same message does not encode at MaxRatchetTreeLength either: %v", err)
	}
	if parsed, err := ParseMLSMessage(encoded); !errors.Is(err, syntax.ErrLengthExceedsMax) {
		t.Fatalf("parsing a %d octet group info message answered %v (message %v), want ErrLengthExceedsMax",
			len(encoded), err, parsed != nil)
	}
	t.Logf("a group info message carrying a %d leaf tree is %d octets; MaxVectorLength is %d, so this entry point refuses this product's own group",
		productGroupLeafCount, len(encoded), syntax.MaxVectorLength)
}

// ---------------------------------------------------------------------------
// the published corpus, at the outermost structure
// ---------------------------------------------------------------------------

// framingMessagesVector is the seven fields of one mlswg `messages` case that carry a whole
// MLSMessage rather than a bare structure. The other ten are bodies other codecs own.
type framingMessagesVector struct {
	GroupInfo                string `json:"mls_group_info"`
	KeyPackage               string `json:"mls_key_package"`
	Welcome                  string `json:"mls_welcome"`
	PrivateMessage           string `json:"private_message"`
	PublicMessageApplication string `json:"public_message_application"`
	PublicMessageCommit      string `json:"public_message_commit"`
	PublicMessageProposal    string `json:"public_message_proposal"`
}

// framingPublishedMessage is one of those fields: the name the corpus spells it with, its hex,
// and the wire format the RFC says it is published under.
type framingPublishedMessage struct {
	field      string
	encoded    string
	wireFormat WireFormat
}

func (self framingMessagesVector) messages() []framingPublishedMessage {
	return []framingPublishedMessage{
		{"mls_group_info", self.GroupInfo, WireFormatGroupInfo},
		{"mls_key_package", self.KeyPackage, WireFormatKeyPackage},
		{"mls_welcome", self.Welcome, WireFormatWelcome},
		{"private_message", self.PrivateMessage, WireFormatPrivateMessage},
		{"public_message_application", self.PublicMessageApplication, WireFormatPublicMessage},
		{"public_message_commit", self.PublicMessageCommit, WireFormatPublicMessage},
		{"public_message_proposal", self.PublicMessageProposal, WireFormatPublicMessage},
	}
}

// TestTheMessagesCorpusMlsMessagesRoundTripByteExactly is the interop half of this task: 2100
// published MLSMessages, none of which this package produced, each decoded through the entry
// point every byte off the wire passes through and re-encoded to the same octets.
//
// Byte exactness against bytes this package did not write is the assertion a self round trip
// cannot make. It sees a header this codec writes in the other order, a length prefix around the
// arm that no other implementation puts there, and a wire format read at one octet instead of
// two -- and every one of those is invisible to a codec compared against itself.
//
// The wire format each field is published under is asserted rather than assumed, and the registry
// is joined in the other direction as well: a code point with no published message is a select
// case this corpus does not reach, which is a hole worth reporting rather than a sweep that
// quietly covers four fifths of the registry.
func TestTheMessagesCorpusMlsMessagesRoundTripByteExactly(t *testing.T) {
	entries := LoadVectorFile(t, "messages.json")
	if len(entries) != welcomeWireMessagesCases {
		t.Fatalf("messages.json carries %d cases, want %d", len(entries), welcomeWireMessagesCases)
	}
	armOf := mlsMessageArmOfEachWireFormat(t)
	fields := mlsMessageArmFields(t)
	seen := map[WireFormat]int{}
	for i, raw := range entries {
		vector := framingMessagesVector{}
		if err := json.Unmarshal(raw, &vector); err != nil {
			t.Fatalf("case %d: parse: %v", i, err)
		}
		for _, published := range vector.messages() {
			at := fmt.Sprintf("messages.json case %d field %s", i, published.field)
			if published.encoded == "" {
				t.Fatalf("%s: the corpus carries no such field, so this sweep would run over six of seven and report the same thing", at)
			}
			encoded := MustHex(t, published.encoded)
			message, err := ParseMLSMessage(encoded)
			if err != nil {
				t.Errorf("%s: the published message was refused: %v", at, err)
				continue
			}
			if message.Version != ProtocolVersionMls10 || message.WireFormat != published.wireFormat {
				t.Errorf("%s: decoded as version %d wire format %#04x, want mls10 and %#04x",
					at, message.Version, message.WireFormat, published.wireFormat)
				continue
			}
			held := reflect.ValueOf(message).Elem()
			armed := true
			for _, field := range fields {
				if !held.FieldByName(field).IsNil() != (field == armOf[published.wireFormat]) {
					t.Errorf("%s: decoded with its %s arm populated=%v and wire format %#04x names %s",
						at, field, !held.FieldByName(field).IsNil(), published.wireFormat, armOf[published.wireFormat])
					armed = false
				}
			}
			if !armed {
				continue
			}
			reencoded, err := MarshalMLSMessage(message)
			if err != nil {
				t.Errorf("%s: re-encode: %v", at, err)
				continue
			}
			if !bytes.Equal(reencoded, encoded) {
				t.Errorf("%s: re-encoded to\n  %x\nand the published encoding is\n  %x", at, reencoded, encoded)
				continue
			}
			seen[published.wireFormat] += 1
		}
	}
	covered := slices.Sorted(maps.Keys(seen))
	if !slices.Equal(covered, framingWireFormats(t)) {
		t.Fatalf("the corpus round tripped the wire formats %v and the registry declares %v; a code point no published message reaches is one this interop gate says nothing about",
			covered, framingWireFormats(t))
	}
	total := 0
	for _, count := range seen {
		total += count
	}
	t.Logf("%d published MLSMessages round tripped byte exactly, over %d wire formats", total, len(seen))
}

// ---------------------------------------------------------------------------
// the three narrow statements the plan wrote, kept beside the sweeps above
// ---------------------------------------------------------------------------

// The plan's own three tests for this task, kept rather than replaced, and each one is a real
// statement: every one of them was made to FAIL by a one line edit to the code under test --
// swapping the two header writes for the first, decoding through a bare Reader instead of
// syntax.Unmarshal for the second, and deleting either refusal for the third.
//
// What they are not is the whole property, and that is why the sweeps above exist beside them.
// The first covers one of five wire formats and two of five arms; the second appends one octet
// where a codec that swallowed a trailing 0x00 would be the interesting case; the third names one
// unregistered wire format out of 65531 and one wrong version out of 65535, with nothing standing
// behind either, so it cannot tell a refusal of the discriminant from an arm decoder running out
// of input. Each of those gaps is the shape of a defect this package has already shipped once.

func TestMLSMessageRoundTripPrivate(t *testing.T) {
	message := MLSMessage{
		Version:    ProtocolVersionMls10,
		WireFormat: WireFormatPrivateMessage,
		PrivateMessage: &PrivateMessage{
			GroupId:             []byte{0x01},
			Epoch:               1,
			ContentType:         ContentTypeApplication,
			AuthenticatedData:   []byte{},
			EncryptedSenderData: []byte{0x01, 0x02},
			Ciphertext:          []byte{0x03, 0x04},
		},
	}
	encoded, err := MarshalMLSMessage(&message)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.HasPrefix(encoded, []byte{0x00, 0x01, 0x00, 0x02}) {
		t.Fatalf("prefix %x, want 00010002", encoded[0:min(len(encoded), 4)])
	}
	decoded, err := ParseMLSMessage(encoded)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if decoded.PrivateMessage == nil || decoded.PublicMessage != nil {
		t.Fatalf("decoded %+v", decoded)
	}
	reencoded, err := MarshalMLSMessage(decoded)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if !bytes.Equal(encoded, reencoded) {
		t.Fatalf("re-encoded %x, want %x", reencoded, encoded)
	}
}

func TestParseMLSMessageRejectsTrailingBytes(t *testing.T) {
	message := MLSMessage{
		Version:    ProtocolVersionMls10,
		WireFormat: WireFormatPrivateMessage,
		PrivateMessage: &PrivateMessage{
			GroupId: []byte{0x01}, Epoch: 1, ContentType: ContentTypeApplication,
			AuthenticatedData: []byte{}, EncryptedSenderData: []byte{0x01}, Ciphertext: []byte{0x02},
		},
	}
	encoded, err := MarshalMLSMessage(&message)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := ParseMLSMessage(append(encoded, 0x00)); err == nil {
		t.Fatal("trailing byte accepted")
	}
}

func TestParseMLSMessageRejectsWrongVersionAndUnknownWireFormat(t *testing.T) {
	if _, err := ParseMLSMessage([]byte{0x00, 0x02, 0x00, 0x02}); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("version: got %v, want ErrUnsupportedVersion", err)
	}
	if _, err := ParseMLSMessage([]byte{0x00, 0x01, 0x00, 0x63}); !errors.Is(err, ErrUnknownWireFormat) {
		t.Fatalf("wire format: got %v, want ErrUnknownWireFormat", err)
	}
}

// ---------------------------------------------------------------------------
// what the outermost decoder does with the answer its arm gave it
// ---------------------------------------------------------------------------

// TestEveryArmOfAnMlsMessageHandsBackTheRefusalItsOwnDecoderAnswered decodes through a BARE
// Reader, which is the only place in this package where the property is observable at all.
//
// ParseMLSMessage cannot see it, and that is measured rather than supposed. syntax.Unmarshal
// joins the decoder's answer with Done, and a Reader LATCHES its first failure, so a short read
// inside an arm comes back to the caller out of Done whatever (*MLSMessage).UnmarshalMLS itself
// returned. Every refusal in this file that reaches the codec through the entry point is
// therefore green against a decoder that dropped its arm's error on the floor: `_ =
// arm.UnmarshalMLS(r)` in the Welcome case, in the GroupInfo case and in the KeyPackage case
// each left the whole of ./mls/... and ./message/... byte identically green. The PublicMessage
// case did not, and that is the axis this sweep is written along -- the one probe in this file
// that decodes through a bare Reader carries a PublicMessage fixture, so its truncations only
// ever entered one of the five arms and the coverage was per ARM rather than per method.
//
// The caller this matters to is every caller that is not ParseMLSMessage. MLSMessage is an
// exported syntax.Codec and decoder_publish_test.go pins its staging as a contract, so anything
// holding a Reader of its own -- a nested decode, a framing this plan does not own yet -- is
// handed a nil error over an arm assembled out of bytes that arm itself refused.
//
// Every registered wire format rather than the one somebody thought of, and every truncation
// inside each arm rather than a chosen refusal point, for framingWholeReceiverProbes' reason: a
// hand written list of refusal positions is a list of the positions somebody had already
// thought to look at.
func TestEveryArmOfAnMlsMessageHandsBackTheRefusalItsOwnDecoderAnswered(t *testing.T) {
	armOf := mlsMessageArmOfEachWireFormat(t)
	formats := framingWireFormats(t)
	// what the caller's receiver holds going in: a whole message of its own, so a published
	// half decode is visibly not the value handed in rather than a zero value nobody can read
	prior := func() *MLSMessage {
		return &MLSMessage{Version: ProtocolVersionMls10, WireFormat: WireFormatPrivateMessage,
			PrivateMessage: framingTestPrivateMessage()}
	}
	header := len(mlsMessageHeaderOf(WireFormatPublicMessage))
	refused := map[WireFormat]int{}
	for _, wireFormat := range formats {
		valid := framingProbeEncoding(t, framingTestMlsMessage(t, wireFormat))
		if len(valid) <= header {
			t.Fatalf("a %#04x message encodes to %d octets, which is the four octet header and nothing else; there is no arm body here to truncate and this code point contributes nothing to the sweep",
				wireFormat, len(valid))
		}
		// the control: the WHOLE encoding is accepted through the same bare Reader, so every
		// truncation below refuses a short input rather than a fixture this test built wrong
		if err := prior().UnmarshalMLS(syntax.NewReader(valid)); err != nil {
			t.Fatalf("the complete %#04x message this sweep truncates was refused through a bare Reader: %v",
				wireFormat, err)
		}
		for at := header; at < len(valid); at += 1 {
			into := prior()
			err := into.UnmarshalMLS(syntax.NewReader(valid[:at]))
			if err == nil {
				t.Fatalf("the four octet header and %d of the %d octets of the %s arm decoded to a nil error through a bare Reader; that arm refused these bytes and this decoder answered its caller that all was well",
					at-header, len(valid)-header, armOf[wireFormat])
			}
			if !reflect.DeepEqual(into, prior()) {
				t.Fatalf("the four octet header and %d octets of the %s arm were refused with %v and left the receiver holding %+v rather than the %+v it held going in",
					at-header, armOf[wireFormat], err, into, prior())
			}
			refused[wireFormat] += 1
		}
	}
	total := 0
	for _, wireFormat := range formats {
		if refused[wireFormat] == 0 {
			t.Fatalf("no truncation fell inside the %s arm, so the refusal that arm's own decoder answers is unobserved here",
				armOf[wireFormat])
		}
		total += refused[wireFormat]
	}
	t.Logf("%d wire formats, %d truncations falling inside an arm; each refused through a bare Reader, each refusal reaching the caller and leaving the receiver alone",
		len(formats), total)
}

// TestAnMlsMessageDecodedIntoAReusedReceiverIsTheMessageAndNothingTheReceiverHeld is the SUCCESS
// half of the whole publish, which nothing in this tree observed.
//
// The two halves that did exist are both about something else. framingDecodersThatPublishWhole
// reads the SHAPE of the publish out of the syntax tree -- one assignment to *self and none to a
// field of it -- and
// TestEveryFramingDecodeThatPublishesItsReceiverWholeLeavesItUntouchedWhenItRefuses observes it
// on the REFUSAL path, where its success control asks only that the receiver CHANGED. Neither
// says anything about what the published value holds when the decode succeeds, and the gap is
// measured rather than theoretical: `*self = decoded` left intact with `if self.PublicMessage !=
// nil { decoded.PublicMessage = self.PublicMessage }` inserted above it survives the whole of
// ./mls/... and ./message/... . Every round trip, corpus and arm exclusivity test in this
// package reaches the decoder through ParseMLSMessage, which builds a FRESH &MLSMessage{} on
// every call, so nothing in the tree had ever decoded into a receiver that held anything.
//
// What that produces is the ProposalOrRef defect the encoder's populatedArms count exists to
// prevent, arriving from the other direction: a decoded message carrying two arms, which
// MarshalMLSMessage then refuses with ErrContentArmMismatch. A message that just decoded cannot
// be re-encoded, and the arm it carries is a field no peer sent.
//
// The priors are derived off the arm class -- nothing at all, each arm on its own, and every arm
// at once -- rather than being the single reused value somebody would think to write, and the
// answer each decode is held to is the same decode into a fresh receiver.
func TestAnMlsMessageDecodedIntoAReusedReceiverIsTheMessageAndNothingTheReceiverHeld(t *testing.T) {
	armOf := mlsMessageArmOfEachWireFormat(t)
	fields := mlsMessageArmFields(t)
	populate := mlsMessageArmValues(t)
	formatOf := map[string]WireFormat{}
	for wireFormat, arm := range armOf {
		formatOf[arm] = wireFormat
	}
	type reusedReceiver struct {
		what string
		arms []string
		held func() *MLSMessage
	}
	priors := []reusedReceiver{{
		what: "nothing at all",
		held: func() *MLSMessage { return &MLSMessage{} },
	}}
	for _, field := range fields {
		priors = append(priors, reusedReceiver{
			what: "a whole " + field + " message",
			arms: []string{field},
			held: func() *MLSMessage {
				message := &MLSMessage{Version: ProtocolVersionMls10, WireFormat: formatOf[field]}
				populate[field](message)
				return message
			},
		})
	}
	priors = append(priors, reusedReceiver{
		what: "every arm at once",
		arms: fields,
		held: func() *MLSMessage {
			message := &MLSMessage{Version: ProtocolVersionMls10, WireFormat: formatOf[fields[0]]}
			for _, field := range fields {
				populate[field](message)
			}
			return message
		},
	})

	formats := framingWireFormats(t)
	swept := 0
	for _, wireFormat := range formats {
		encoded := framingProbeEncoding(t, framingTestMlsMessage(t, wireFormat))
		fresh := &MLSMessage{}
		if err := fresh.UnmarshalMLS(syntax.NewReader(encoded)); err != nil {
			t.Fatalf("a %#04x message did not decode into a fresh receiver: %v", wireFormat, err)
		}
		// the vacuity control, per code point: some prior has to hold an arm this code point
		// does NOT name, or an arm carried forward would be indistinguishable from the one the
		// bytes themselves name
		carriable := 0
		for _, prior := range priors {
			for _, arm := range prior.arms {
				if arm != armOf[wireFormat] {
					carriable += 1
					break
				}
			}
		}
		if carriable == 0 {
			t.Fatalf("no prior receiver holds an arm other than the %s one this %#04x sweep decodes, so an arm carried forward could not be told from the arm these bytes name",
				armOf[wireFormat], wireFormat)
		}
		for _, prior := range priors {
			held := prior.held()
			if err := held.UnmarshalMLS(syntax.NewReader(encoded)); err != nil {
				t.Fatalf("a %#04x message was refused by a receiver holding %s: %v", wireFormat, prior.what, err)
			}
			for _, field := range fields {
				populated := !reflect.ValueOf(held).Elem().FieldByName(field).IsNil()
				if populated != (field == armOf[wireFormat]) {
					t.Fatalf("a %#04x message decoded into a receiver holding %s came back with its %s arm populated=%v; that code point names the %s arm and exactly that one, and an arm this decode never read is a field nobody sent",
						wireFormat, prior.what, field, populated, armOf[wireFormat])
				}
			}
			if !reflect.DeepEqual(held, fresh) {
				t.Fatalf("a %#04x message decoded into a receiver holding %s came back as %+v and into a fresh receiver as %+v; the same octets decode to one value, or the decoder is publishing something the caller brought with it",
					wireFormat, prior.what, held, fresh)
			}
			reencoded, err := MarshalMLSMessage(held)
			if err != nil {
				t.Fatalf("a %#04x message decoded into a receiver holding %s and then would not re-encode: %v; a value that decoded and will not encode is one its own sender cannot forward",
					wireFormat, prior.what, err)
			}
			if !bytes.Equal(reencoded, encoded) {
				t.Fatalf("a %#04x message decoded into a receiver holding %s re-encoded to %x, want %x",
					wireFormat, prior.what, reencoded, encoded)
			}
			swept += 1
		}
	}
	if swept != len(formats)*len(priors) {
		t.Fatalf("%d decodes over %d code points and %d prior receivers, want %d",
			swept, len(formats), len(priors), len(formats)*len(priors))
	}
	t.Logf("%d code points x %d prior receivers: every decode published the message the octets name and nothing the receiver held",
		len(formats), len(priors))
}

// framingEncoderRefusals is one refusal MLSMessage.MarshalMLS can answer: the sentinel itself,
// and a sweep of the values that reach it.
//
// The sweep hands its cases to a callback rather than returning a slice because two of the three
// classes are the whole of a sixteen bit code point space, and a slice of those is a quarter of
// a million MLSMessage values built to be refused one at a time.
type framingEncoderRefusals struct {
	sentinel error
	sweep    func(t *testing.T, refuse func(what string, message *MLSMessage))
}

// sentinelErrorsReturnedBy is every package level error variable one function of this package's
// production source names in a return statement.
//
// The type CHECK is what makes this a derivation rather than a name match. A returned identifier
// counts only if this package declares it at package level at the type error, so a local, a
// struct field and a constant whose name happens to open with Err all fall out of the class, and
// a sentinel that does not follow the naming convention falls into it.
func sentinelErrorsReturnedBy(t *testing.T, qualified string) []string {
	t.Helper()
	scope := typeCheckedPackage(t).Scope()
	errorInterface := types.Universe.Lookup("error").Type()
	for _, function := range framingRegistryFunctions(t) {
		if function.name != qualified {
			continue
		}
		found := []string{}
		ast.Inspect(function.decl.Body, func(node ast.Node) bool {
			returned, isReturn := node.(*ast.ReturnStmt)
			if !isReturn {
				return true
			}
			for _, result := range returned.Results {
				ast.Inspect(result, func(inner ast.Node) bool {
					name, isName := inner.(*ast.Ident)
					if !isName {
						return true
					}
					variable, isVariable := scope.Lookup(name.Name).(*types.Var)
					if isVariable && types.Identical(variable.Type(), errorInterface) &&
						!slices.Contains(found, name.Name) {
						found = append(found, name.Name)
					}
					return true
				})
			}
			return true
		})
		slices.Sort(found)
		if len(found) == 0 {
			t.Fatalf("%s returns no package level error variable at all, so the refusal class below is empty and every sweep over it would report green having probed nothing",
				qualified)
		}
		return found
	}
	t.Fatalf("no function of this package's production source that computes with a framing registry is called %s, so this derivation is a claim about nothing",
		qualified)
	return nil
}

// TestEveryRefusalTheMlsMessageEncoderAnswersLeavesTheCallersWriterEmpty is MarshalMLS's stated
// discipline -- refuse the version, then the wire format, then the arms, and write nothing until
// all three have passed -- held over the class of its OWN refusals rather than over the two
// somebody wrote a probe for.
//
// Two of the three already had this assertion, on a Writer the test holds:
// TestEveryProtocolVersionButMls10IsRefusedAheadOfTheWireFormat and
// TestEveryUnregisteredWireFormatIsRefusedWhateverStandsBehindIt each sweep their whole code
// point space through marshalIntoCatchingPanic and read writer.Len(). The ARM check did not, and
// the gap survived a full run: moving both `w.WriteUint16` calls from below the `arm == nil ||
// self.populatedArms() != 1` check to above it left the whole of ./mls/... and ./message/...
// green. TestEveryWireFormatCarriesExactlyTheArmItNames sweeps all seventy five arm mismatches
// and every one of them goes through MarshalMLSMessage -- that is syntax.Marshal, which DROPS
// its buffer whenever the encode errors, deliberately, so a caller cannot take a partial
// encoding by accident. An assertion that a refusal wrote nothing, made through it, compares nil
// against nil and cannot fail. marshalIntoCatchingPanic's own doc comment says exactly that
// about the two checks above; the check one line below them was left sitting in the same trap.
//
// So the sentinels are READ OFF MarshalMLS's syntax tree and joined against the probes here,
// which is rule 5's shape rather than a third table: a fourth refusal added to that method
// arrives with no probe and fails this gate, instead of inheriting the vacuity by default.
//
// What the mutation costs is what framing.go's own comment says it costs. A caller that ignored
// the error passes on a four octet MLSMessage header with no message behind it, and that header
// is the one thing every peer parses before it parses anything else.
func TestEveryRefusalTheMlsMessageEncoderAnswersLeavesTheCallersWriterEmpty(t *testing.T) {
	armOf := mlsMessageArmOfEachWireFormat(t)
	fields := mlsMessageArmFields(t)
	populate := mlsMessageArmValues(t)
	formats := framingWireFormats(t)

	probed := map[string]framingEncoderRefusals{
		// the whole version space less the one value this profile implements, over a message
		// that is valid in every other respect
		"ErrUnsupportedVersion": {sentinel: ErrUnsupportedVersion,
			sweep: func(t *testing.T, refuse func(string, *MLSMessage)) {
				message := framingTestMlsMessage(t, WireFormatPrivateMessage)
				for value := 0; value <= 0xffff; value += 1 {
					if ProtocolVersion(value) == ProtocolVersionMls10 {
						continue
					}
					message.Version = ProtocolVersion(value)
					refuse(fmt.Sprintf("protocol version %d", value), message)
				}
			}},
		// every code point the registry does not declare, with and without an arm standing
		// beside it, because the wire format is refused ahead of the arm check and the write
		// discipline is the same on both sides of that
		"ErrUnknownWireFormat": {sentinel: ErrUnknownWireFormat,
			sweep: func(t *testing.T, refuse func(string, *MLSMessage)) {
				for _, codePoint := range undeclaredCodePointsOf(t, "WireFormat") {
					for _, populated := range []bool{false, true} {
						message := &MLSMessage{Version: ProtocolVersionMls10, WireFormat: WireFormat(codePoint)}
						if populated {
							message.PrivateMessage = framingTestPrivateMessage()
						}
						refuse(fmt.Sprintf("unregistered wire format %d, arm populated=%v", codePoint, populated), message)
					}
				}
			}},
		// the cross product TestEveryWireFormatCarriesExactlyTheArmItNames refuses, swept here
		// for what it leaves BEHIND rather than for what it answers: no arm at all, a foreign
		// arm, and every pair, at every registered code point
		"ErrContentArmMismatch": {sentinel: ErrContentArmMismatch,
			sweep: func(t *testing.T, refuse func(string, *MLSMessage)) {
				for _, wireFormat := range formats {
					refuse(fmt.Sprintf("wire format %#04x with no arm at all", wireFormat),
						&MLSMessage{Version: ProtocolVersionMls10, WireFormat: wireFormat})
					for _, field := range fields {
						if field == armOf[wireFormat] {
							continue
						}
						mismatched := &MLSMessage{Version: ProtocolVersionMls10, WireFormat: wireFormat}
						populate[field](mismatched)
						refuse(fmt.Sprintf("wire format %#04x carrying the %s arm", wireFormat, field), mismatched)
					}
					for i := 0; i < len(fields); i += 1 {
						for j := i + 1; j < len(fields); j += 1 {
							both := &MLSMessage{Version: ProtocolVersionMls10, WireFormat: wireFormat}
							populate[fields[i]](both)
							populate[fields[j]](both)
							refuse(fmt.Sprintf("wire format %#04x carrying the %s and the %s arms",
								wireFormat, fields[i], fields[j]), both)
						}
					}
				}
			}},
	}

	derived := sentinelErrorsReturnedBy(t, "MLSMessage.MarshalMLS")
	if !slices.Equal(derived, slices.Sorted(maps.Keys(probed))) {
		t.Fatalf("MLSMessage.MarshalMLS returns the sentinels %v and this gate probes %v; a refusal nothing probes is one whose write discipline nobody has ever held it to",
			derived, slices.Sorted(maps.Keys(probed)))
	}
	swept := map[string]int{}
	for _, name := range derived {
		probe := probed[name]
		probe.sweep(t, func(what string, message *MLSMessage) {
			// the Writer is the CALLER's, which is the whole point: syntax.Marshal drops its
			// buffer on an error, so the same assertion made through it compares nil to nil
			writer := syntax.NewWriter()
			raised, err := marshalIntoCatchingPanic(writer, message)
			if raised != nil {
				t.Fatalf("encoding a message at %s panicked: %v", what, raised)
			}
			if !errors.Is(err, probe.sentinel) {
				t.Fatalf("encoding a message at %s answered %v, want %s; a case filed under a refusal it does not reach states nothing about that refusal",
					what, err, name)
			}
			if writer.Len() != 0 {
				t.Fatalf("encoding a message at %s was refused with %v and left %d octets on the caller's Writer; a caller that ignored the error can pass on a four octet header with no message behind it, and that header is what every peer parses before it parses anything else",
					what, err, writer.Len())
			}
			swept[name] += 1
		})
		if swept[name] == 0 {
			t.Fatalf("the %s probe produced no case at all, so this gate reports that refusal green having encoded nothing", name)
		}
	}
	t.Logf("%d refusals derived off MLSMessage.MarshalMLS, %v cases swept; every one left the caller's Writer empty",
		len(derived), swept)
}
