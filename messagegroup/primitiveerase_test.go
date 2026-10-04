// The FLOOR under this package's erase obligation: what the cryptographic primitives produced,
// asked from the primitives' side rather than from the package's habits.
//
// WHY A SECOND MECHANISM EXISTS AT ALL, and it is a measured hole rather than belt and braces.
// wrap_test.go's TestEveryKeyThisPackageDerivesIsErasedInTheBodyThatDerivedIt derives its producer
// class from THE ERASES THIS PACKAGE ALREADY SPELLS: a callee is a producer at a result position
// because some body binds that position and erases it. That reading is closed under new call
// sites -- a second body dropping a known producer's result is caught -- and it is structurally
// blind to a producer NOBODY has ever erased. xwing.go was exactly that: six live-at-return
// secrets, zero erases, so nothing in the file seeded the class, so the class did not reach the
// file, so the file read clean. Adding xwing.go's names to that gate would have closed six
// instances and left the next primitive in the same position.
//
// So this file asks the opposite question and answers it from a table. Every local this package
// binds out of a CRYPTOGRAPHIC PRIMITIVE is dispositioned: erased in the body that bound it, moved
// out of it, installed in storage that outlives the frame, or excused by a row somebody wrote. A
// new primitive call is a new row the day it is written, and a row that stops excusing anything is
// reported, so neither direction can rot quietly.
//
// WHAT COUNTS AS A PRIMITIVE is read off the source in the shape connect/mls's own scan-root
// derivation uses -- "an import of crypto, crypto/... or golang.org/x/crypto/... in production
// source" -- plus the one hop that definition cannot make on its own. This package reaches x25519
// through connect/mls's single reviewed ECDH wrapper rather than through crypto/ecdh directly,
// which is the arrangement xwing.go's header argues for and the forbidden primitive gate asserts;
// so a call `mls.F(...)` is a primitive call when F is DECLARED IN AN mls SOURCE FILE THAT ITSELF
// IMPORTS A CRYPTOGRAPHIC PACKAGE. That is what separates mls.X25519DH, which is a Diffie-Hellman,
// from mls.LoadGroup, which is a group handle -- without either being written down, and without
// this package's own engine.go turning into twenty rows about the MLS API.
//
// THAT ONE HOP IS GENEROUS AND THE GENEROSITY IS MEASURED, not assumed. It separates mls's ECDH
// wrapper from mls's CODEC and its key package builder -- framing.go, leaf_keys.go and
// key_package.go import no cryptographic package, so ParseMLSMessage, LeafKeysOf and
// NewKeyPackageWithSigner are outside -- but it does NOT separate it from the group API, because
// group.go imports crypto/subtle for one constant time comparison and every free function declared
// beside it comes in with it. LoadGroup is admitted and this file says so rather than claiming a
// sharper rule than it has. It costs nothing measurable: a group handle is installed in a field or
// answered to a caller, so the reading dispositions it without a row, and the residual over this
// package's whole production source is ten rows rather than the twenty-four a reading rooted in
// the import alone produced.
package messagegroup

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The import paths that make a package CRYPTOGRAPHIC, in the tree's own words: connect/mls's
// crypto_forbidden_test.go derives its scan roots off exactly this test and says so.
func primitiveImportIsCryptographic(path string) bool {
	return path == "crypto" || strings.HasPrefix(path, "crypto/") ||
		strings.HasPrefix(path, "golang.org/x/crypto/")
}

// The import path of the package whose ONE reviewed ECDH wrapper this package's x25519 half goes
// through. mls may not import this package, so the edge is read from this side.
const primitiveMlsImportPath = "github.com/urnetwork/connect/mls"

// primitiveMlsFunctionNames is every free function connect/mls declares in a production source
// file that itself imports a cryptographic package.
//
// It is a reading of ../mls and not a list, for the reason every list in this tree gets replaced:
// a wrapper moved into a crypto file joins on the commit that moves it, and one moved out leaves.
// A reading that found no such function would clear every mls call this package makes, so the
// emptiness is fatal rather than quiet.
func primitiveMlsFunctionNames(t *testing.T) map[string]bool {
	t.Helper()
	root := filepath.Join("..", "mls")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read %s: %v", root, err)
	}
	names := map[string]bool{}
	files := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", filepath.Join(root, name), err)
		}
		cryptographic := false
		for _, spec := range parsed.Imports {
			if primitiveImportIsCryptographic(strings.Trim(spec.Path.Value, `"`)) {
				cryptographic = true
			}
		}
		if !cryptographic {
			continue
		}
		files += 1
		for _, declaration := range parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Recv != nil {
				continue
			}
			names[function.Name.Name] = true
		}
	}
	if files == 0 || len(names) == 0 {
		t.Fatalf("%d file(s) of connect/mls import a cryptographic package and they declare %d free function(s); a reading that finds none clears every mls call this package makes",
			files, len(names))
	}
	return names
}

// primitiveBinding is one local one body bound out of one primitive call.
type primitiveBinding struct {
	file     string
	function string
	name     string
	callee   string
	// the disposition the source itself supplies, empty when there is none and a row is owed
	disposition string
}

func (self primitiveBinding) row() string {
	return self.file + ": " + self.function + "." + self.name
}

// The rendered callee, for the report only.
func primitiveCalleeText(expr ast.Expr) string {
	switch typed := expr.(type) {
	case *ast.Ident:
		return typed.Name
	case *ast.SelectorExpr:
		return primitiveCalleeText(typed.X) + "." + typed.Sel.Name
	case *ast.CallExpr:
		return primitiveCalleeText(typed.Fun) + "()"
	case *ast.ParenExpr:
		return primitiveCalleeText(typed.X)
	case *ast.IndexExpr:
		return primitiveCalleeText(typed.X)
	case *ast.StarExpr:
		return primitiveCalleeText(typed.X)
	}
	return "?"
}

// Every identifier and qualified identifier a type expression mentions.
func primitiveTypeMentions(expr ast.Expr) []string {
	mentioned := []string{}
	ast.Inspect(expr, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.SelectorExpr:
			if base, isName := typed.X.(*ast.Ident); isName {
				mentioned = append(mentioned, base.Name+"."+typed.Sel.Name)
			}
		case *ast.Ident:
			mentioned = append(mentioned, typed.Name)
		}
		return true
	})
	return mentioned
}

// primitiveBindingsIn reads one source text and answers every local bound out of a primitive call,
// each carrying the disposition the body gives it.
//
// THREE SHAPES ARE A PRIMITIVE CALL. A call qualified by an import of a cryptographic package; a
// call `mls.F(...)` where F is one of the mls functions declared beside a cryptographic import;
// and a method call on a name whose DECLARED TYPE is one of those packages' -- which is what
// reaches pub.mlkemPublic.Encapsulate and priv.mlkemPrivate.Decapsulate, the two halves of the KEM
// that are not spelled as package calls at all and that the single-file, bare-name reading this
// file backs up could not see.
//
// err at the last result position is not a binding here. It is the package's one name for an
// error and a table of twenty-six rows saying "an error" would bury every row that means
// something; the cost is that a secret named err at the last position is invisible, which is a
// shape nothing in this tree writes and which the reading states rather than hides.
func primitiveBindingsIn(t *testing.T, name string, source string, mlsFunctions map[string]bool) []primitiveBinding {
	t.Helper()
	parsed := mustParseMessagegroupSource(t, name, source)
	qualifiers := map[string]bool{}
	mlsQualifier := ""
	for _, spec := range parsed.Imports {
		path := strings.Trim(spec.Path.Value, `"`)
		qualifier := path[strings.LastIndex(path, "/")+1:]
		if spec.Name != nil {
			qualifier = spec.Name.Name
		}
		if primitiveImportIsCryptographic(path) {
			qualifiers[qualifier] = true
		}
		if path == primitiveMlsImportPath {
			mlsQualifier = qualifier
		}
	}
	if len(qualifiers) == 0 && mlsQualifier == "" {
		return nil
	}
	// the names of this file whose declared type is one a cryptographic package supplies:
	// struct fields, parameters and results alike, since all three are *ast.Field.
	cryptoTyped := map[string]bool{}
	ast.Inspect(parsed, func(node ast.Node) bool {
		field, isField := node.(*ast.Field)
		if !isField || field.Type == nil {
			return true
		}
		qualified := false
		for _, mention := range primitiveTypeMentions(field.Type) {
			if at := strings.Index(mention, "."); 0 < at && qualifiers[mention[:at]] {
				qualified = true
			}
		}
		if !qualified {
			return true
		}
		for _, declared := range field.Names {
			cryptoTyped[declared.Name] = true
		}
		return true
	})
	bindings := []primitiveBinding{}
	for _, declaration := range parsed.Decls {
		function, isFunction := declaration.(*ast.FuncDecl)
		if !isFunction || function.Body == nil {
			continue
		}
		erased := wrapErasedNamesIn(function.Body)
		moved := wrapMovedOutIn(function.Body)
		installed := wrapInstalledNamesIn(function.Body)
		ast.Inspect(function.Body, func(node ast.Node) bool {
			assign, isAssign := node.(*ast.AssignStmt)
			if !isAssign || len(assign.Rhs) != 1 {
				return true
			}
			call, isCall := assign.Rhs[0].(*ast.CallExpr)
			if !isCall {
				return true
			}
			primitive := false
			if base, isName := call.Fun.(*ast.SelectorExpr); isName {
				if qualifier, isBare := base.X.(*ast.Ident); isBare {
					if qualifiers[qualifier.Name] {
						primitive = true
					}
					if mlsQualifier != "" && qualifier.Name == mlsQualifier && mlsFunctions[base.Sel.Name] {
						primitive = true
					}
				}
				for _, mention := range primitiveTypeMentions(base.X) {
					if cryptoTyped[mention] {
						primitive = true
					}
				}
			}
			if !primitive {
				return true
			}
			for position, left := range assign.Lhs {
				bound, isName := left.(*ast.Ident)
				if !isName || bound.Name == "_" {
					continue
				}
				if bound.Name == "err" && position == len(assign.Lhs)-1 {
					continue
				}
				disposition := ""
				switch {
				case erased[bound.Name]:
					disposition = "erased"
				case len(moved[bound.Name]) != 0:
					disposition = "moved out"
				case installed[bound.Name]:
					disposition = "installed"
				}
				bindings = append(bindings, primitiveBinding{
					file: name, function: function.Name.Name, name: bound.Name,
					callee: primitiveCalleeText(call.Fun), disposition: disposition,
				})
			}
			return true
		})
	}
	return bindings
}

// The primitive results this package binds that are NOT key material, one row each.
//
// A row is a sentence somebody had to write about octets a cryptographic primitive produced. The
// table is checked in BOTH directions against the reading: a binding with no row fails, and a row
// that names no undispositioned binding fails -- which is what makes a row that was made true by
// an erase, or by the call going away, report itself instead of sitting there.
var primitiveResultsThatAreNotKeyMaterial = map[string]string{
	"engine.go: peekWithGroupSecrets.crypto": "the suite's crypto PROVIDER and not a key: a " +
		"parameter block and an entropy source. connect/mls excuses suiteCryptoProvider in its own " +
		"erase class on the same ground, and there is no byte slice here to erase",
	"engine.go: peekWithGroupSecrets.leaf": "the LEAF INDEX a private message names as its sender. " +
		"It is read out of the cleartext sender-data header that every member of the group decrypts " +
		"and that the delivery service routes on, and it is an integer rather than octets",
	"engine.go: CommitPolicy.replaced": "an EXTENSION LIST carrying the group policy, which is a " +
		"field of the group context every member holds and which the transcript covers. It is " +
		"committed, not held",
	"engine.go: ProposeGroupPolicy.replaced": "the same extension list one proposal earlier, for the " +
		"same reason: it is about to be a proposal on the wire",
	"seal.go: openRecordOnLoop.bodyHash": "SHA-256 OVER THE SEALED BODY, which is octets an " +
		"attacker reading the wire already has. It is a digest of ciphertext and binds the head to " +
		"the body; erasing it would remove nothing anybody lacks",
	"seal.go: sealHead.bound": "the same digest on the sealing side, and the same reason",
	"xwing.go: XwingEncapsulate.ephemeral": "the EPHEMERAL X25519 KEY PAIR, and this row is a " +
		"residue rather than a public value. The scalar inside crypto/ecdh's *PrivateKey is not a " +
		"[]byte this package holds a header over -- PrivateKey.Bytes() answers a COPY, so erasing " +
		"what it returns blanks the copy and leaves the key -- so there is nothing here zeroize can " +
		"reach. It is named in xwing.go's own header as one of the three residues this file cannot " +
		"clear, and the shared secret it produces IS erased one line below",
	"xwing.go: XwingDecapsulate.ephemeralPublic": "the SENDER'S ephemeral public key, parsed out of " +
		"ct_X. It travelled in the clear as half the ciphertext and every relay on the path has it",
	"xwing.go: xwingCombine.hash": "the SHA3-256 sponge the combiner writes its five inputs into. " +
		"The state holds the two shared secrets while it runs and it is not a byte slice either, " +
		"which is the second of xwing.go's three named residues; what the combiner ANSWERS is moved " +
		"out to its caller, and both of its secret inputs are erased by the bodies that derived them",
	"xwing.go: XwingEncapsulate.mlkemCiphertext": "ct_M, the ML-KEM half of the ciphertext. It is " +
		"appended to the ciphertext this function answers and goes on the wire; it is the one result " +
		"of Encapsulate that is not the shared secret",
}

// Property: every local this package binds out of a cryptographic primitive is erased, moved out,
// installed, or carries a row.
func TestEveryPrimitiveResultThisPackageBindsHasAWrittenDisposition(t *testing.T) {
	// the control first, over a package holding one of each shape the reading must separate: a
	// primitive result erased, one moved out, one installed, one dropped, a non-primitive call
	// left alone, and a method call on a crypto-typed field -- which is the shape the reading was
	// widened for and the one a bare-name matcher cannot see.
	const controlName = "the primitive disposition control"
	control := "package control\n" +
		"import \"crypto/probe\"\n" +
		"type holder struct {\n\tinner *probe.Key\n\tkept  []byte\n}\n" +
		"func erases() {\n\ts := probe.Derive()\n\tzeroize(s)\n}\n" +
		"func moves() []byte {\n\ts := probe.Derive()\n\treturn s\n}\n" +
		"func (self *holder) installs() {\n\ts := probe.Derive()\n\tself.kept = s\n}\n" +
		"func drops() {\n\ts := probe.Derive()\n\t_ = s\n}\n" +
		"func unrelated() {\n\ts := notAPrimitive()\n\t_ = s\n}\n" +
		"func (self *holder) method() {\n\ts := self.inner.Shared()\n\t_ = s\n}\n" +
		"func errors() {\n\ts, err := probe.Derive2()\n\tzeroize(s)\n\t_ = err\n}\n"
	wantControl := map[string]string{
		"control: erases.s":   "erased",
		"control: moves.s":    "moved out",
		"control: installs.s": "installed",
		"control: drops.s":    "",
		"control: method.s":   "",
		"control: errors.s":   "erased",
	}
	read := map[string]string{}
	for _, binding := range primitiveBindingsIn(t, "control", control, map[string]bool{}) {
		read[binding.row()] = binding.disposition
	}
	if !maps.Equal(read, wantControl) {
		t.Fatalf("the control reads as %v, want %v; the reading is not separating an erased primitive result from a moved, an installed and a dropped one, is not reaching a method call on a crypto-typed field, or is not leaving a non-primitive call and a trailing err alone",
			read, wantControl)
	}

	mlsFunctions := primitiveMlsFunctionNames(t)
	// the positive control on the real reading, in the same query as the verdict: mls's ECDH
	// wrapper is what this package's x25519 half goes through, and a set that had stopped
	// containing it would leave every x25519 shared secret outside this gate.
	if !mlsFunctions["X25519DH"] {
		t.Fatal("connect/mls's X25519DH is not read as a primitive, so every x25519 shared secret this package binds is outside this gate")
	}
	// AND THE NARROWING'S COMPLEMENT, asserted rather than printed. The rule admits the free
	// functions declared beside a cryptographic import and it must LEAVE SOMETHING OUT, or it is
	// the sentence "every mls function is a primitive" written the long way. mls's codec, its
	// leaf-keys reader and its key package builder are what it leaves out, and they are named
	// here because engine.go calls all three.
	for _, outside := range []string{"ParseMLSMessage", "LeafKeysOf", "NewKeyPackageWithSigner"} {
		if mlsFunctions[outside] {
			t.Fatalf("connect/mls's %s is read as a cryptographic primitive; it is declared beside no cryptographic import today, and a rule that reaches it is asking this package for a disposition of the MLS codec",
				outside)
		}
	}

	_, sources := messagegroupProductionSources(t)
	bindings := []primitiveBinding{}
	for _, source := range sources {
		raw, err := os.ReadFile(source.path)
		if err != nil {
			t.Fatalf("read %s: %v", source.path, err)
		}
		bindings = append(bindings, primitiveBindingsIn(t, source.path, string(raw), mlsFunctions)...)
	}
	if len(bindings) == 0 {
		t.Fatal("this reading found no primitive result bound anywhere in this package's production source, so its verdict below is a verdict over nothing")
	}
	// and the positive control on the SUBJECT: the seed expansion is the value this gate exists
	// for, and a reading that stopped seeing it would report the same clean run a complete one
	// reports.
	reached := map[string]string{}
	for _, binding := range bindings {
		reached[binding.row()] = binding.disposition
	}
	for _, wanted := range []string{
		"xwing.go: XwingKeyGenFromSeed.expanded",
		"xwing.go: XwingEncapsulate.x25519Shared",
		"xwing.go: XwingDecapsulate.mlkemShared",
	} {
		if _, isBound := reached[wanted]; !isBound {
			t.Fatalf("this reading does not reach %s, so it is not reading what it claims to: %v",
				wanted, slices.Sorted(maps.Keys(reached)))
		}
	}

	owed := []string{}
	for _, binding := range bindings {
		if binding.disposition != "" {
			continue
		}
		if _, isExcused := primitiveResultsThatAreNotKeyMaterial[binding.row()]; isExcused {
			continue
		}
		owed = append(owed, binding.row()+" <- "+binding.callee)
	}
	slices.Sort(owed)
	if len(owed) != 0 {
		t.Errorf("%v are bound out of a cryptographic primitive and this package neither erases, moves nor installs them, and no row says what they are. Every one of them is a copy of whatever the primitive produced, live until the collector takes it, and erasing downstream of it erases one copy",
			slices.Compact(owed))
	}
	// the other direction, so no row outlives what it excuses -- including a row made true by
	// somebody adding the erase it was written in place of.
	for row := range primitiveResultsThatAreNotKeyMaterial {
		disposition, isBound := reached[row]
		if !isBound {
			t.Errorf("primitiveResultsThatAreNotKeyMaterial excuses %q, which this reading does not bind out of a primitive at all; a row that outlived its call excuses nothing and hides that it does",
				row)
			continue
		}
		if disposition != "" {
			t.Errorf("primitiveResultsThatAreNotKeyMaterial excuses %q and the source %s it; one of the two is wrong and which one holds cannot be read off either",
				row, disposition)
		}
	}
	t.Logf("%d primitive result(s) bound; %d excused by a row", len(bindings), len(primitiveResultsThatAreNotKeyMaterial))
}

// ---------------------------------------------------------------------------
// the entropy half: a buffer filled from a random source is key material too
// ---------------------------------------------------------------------------

// Property: every buffer this package fills from an entropy source is erased in the body that
// filled it, or moved out of it.
//
// IT IS THE THIRD MECHANISM AND IT COVERS WHAT NEITHER OF THE OTHER TWO CAN. A draw is not a call
// whose RESULT is the secret -- io.ReadFull answers a count and an error, and the octets land in a
// buffer the body allocated with make, which the producer reading strikes as an allocation rather
// than a derivation and which no cryptographic package qualifies. XwingGenerateKey's seed is
// exactly that shape: thirty two octets of X-Wing private key, drawn here, copied by the
// constructor it is handed to, and live at return with no caller able to reach it.
//
// The CLASS is entropy_test.go's -- every function of this package's production source that takes
// an io.Reader -- so a second function that draws is judged the day it is declared.
func TestEveryBufferThisPackageFillsFromEntropyIsErasedOrMovedOut(t *testing.T) {
	const controlName = "the entropy fill control"
	control := "package control\n" +
		"func erases(random io.Reader) {\n\tb := make([]byte, 32)\n\tio.ReadFull(random, b)\n\tzeroize(b)\n}\n" +
		"func moves(random io.Reader) []byte {\n\tb := make([]byte, 32)\n\tio.ReadFull(random, b)\n\treturn b\n}\n" +
		"func drops(random io.Reader) {\n\tb := make([]byte, 32)\n\tio.ReadFull(random, b)\n\t_ = b\n}\n" +
		"func readsDirectly(random io.Reader) {\n\tb := make([]byte, 32)\n\trandom.Read(b)\n\t_ = b\n}\n" +
		"func takesNone() {\n\tb := make([]byte, 32)\n\t_ = b\n}\n"
	held := entropyFillsHeldIn(t, controlName, control)
	if want := []string{"drops.b", "readsDirectly.b"}; !slices.Equal(held, want) {
		t.Fatalf("the control reads as holding %v, want %v; the reading is not separating a drawn buffer that is erased from one that is dropped, is not following a direct Read on the source, or is judging a function that takes no source",
			held, want)
	}

	_, sources := messagegroupProductionSources(t)
	fills := 0
	for _, source := range sources {
		raw, err := os.ReadFile(source.path)
		if err != nil {
			t.Fatalf("read %s: %v", source.path, err)
		}
		fills += entropyFillsCountIn(t, source.path, string(raw))
		if held := entropyFillsHeldIn(t, source.path, string(raw)); len(held) != 0 {
			t.Errorf("%s fills %v out of an entropy source and neither erases them in the body that drew them nor moves them out; a draw the caller cannot reach is a secret this package alone can clear",
				source.path, held)
		}
	}
	// the positive control in the same query as the zero above: this package certainly draws, and
	// a reading that had stopped finding the draw would report the same clean run.
	if fills == 0 {
		t.Fatal("this reading found no buffer filled from an entropy source anywhere in this package's production source, so its zero above is a zero over nothing")
	}
	t.Logf("%d buffer(s) filled from an entropy source, every one erased or moved out", fills)
}

// entropyFillDestinationsIn answers, per entropy-taking declaration, the names it fills from its
// own source: io.ReadFull(source, name) and source.Read(name).
func entropyFillDestinationsIn(function *ast.FuncDecl) []string {
	sources := map[string]bool{}
	if function.Type.Params != nil {
		for _, field := range function.Type.Params.List {
			mentions := primitiveTypeMentions(field.Type)
			if !slices.Contains(mentions, entropySourceExpression) {
				continue
			}
			for _, name := range field.Names {
				sources[name.Name] = true
			}
		}
	}
	if len(sources) == 0 || function.Body == nil {
		return nil
	}
	filled := []string{}
	ast.Inspect(function.Body, func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if !isCall || len(call.Args) == 0 {
			return true
		}
		selector, isSelector := call.Fun.(*ast.SelectorExpr)
		if !isSelector {
			return true
		}
		base, isBare := selector.X.(*ast.Ident)
		if !isBare {
			return true
		}
		destination := ast.Expr(nil)
		switch {
		case base.Name == "io" && (selector.Sel.Name == "ReadFull" || selector.Sel.Name == "ReadAtLeast") && 2 <= len(call.Args):
			if reader, isName := call.Args[0].(*ast.Ident); !isName || !sources[reader.Name] {
				return true
			}
			destination = call.Args[1]
		case sources[base.Name] && selector.Sel.Name == "Read":
			destination = call.Args[0]
		default:
			return true
		}
		if named := wrapMovedOutName(destination); named != "" {
			filled = append(filled, named)
		}
		return true
	})
	slices.Sort(filled)
	return slices.Compact(filled)
}

func entropyFillsCountIn(t *testing.T, name string, source string) int {
	t.Helper()
	parsed := mustParseMessagegroupSource(t, name, source)
	count := 0
	for _, declaration := range parsed.Decls {
		function, isFunction := declaration.(*ast.FuncDecl)
		if !isFunction {
			continue
		}
		count += len(entropyFillDestinationsIn(function))
	}
	return count
}

func entropyFillsHeldIn(t *testing.T, name string, source string) []string {
	t.Helper()
	parsed := mustParseMessagegroupSource(t, name, source)
	held := []string{}
	for _, declaration := range parsed.Decls {
		function, isFunction := declaration.(*ast.FuncDecl)
		if !isFunction || function.Body == nil {
			continue
		}
		erased := wrapErasedNamesIn(function.Body)
		moved := wrapMovedOutIn(function.Body)
		installed := wrapInstalledNamesIn(function.Body)
		for _, filled := range entropyFillDestinationsIn(function) {
			if erased[filled] || len(moved[filled]) != 0 || installed[filled] {
				continue
			}
			held = append(held, function.Name.Name+"."+filled)
		}
	}
	slices.Sort(held)
	return slices.Compact(held)
}
