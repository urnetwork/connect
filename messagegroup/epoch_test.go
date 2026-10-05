// Task 13's properties: the pq_secret sampler, and the provisional epoch state G10 destroys.
//
// SIX PROPERTIES AND ONE OF THEM IS ONLY HALF HERE, which is said first because the missing half is
// a scheduling fact and not an omission. Property 4 is G10's "there is no path that reads it
// afterwards", and it has two halves: the value refuses every accessor after its destructor has run,
// which is behavioural and lands here; and every in package READER of the value checks the destroyed
// flag first, which is a derived class and lands in task 15 property 6. It cannot land here because
// at this commit that class is EMPTY -- the readers are task 15's fan out and task 21's retry loop
// and neither exists -- and this tree's house style fatals on an empty derived class rather than
// reporting clean over one (aad_test.go:1293, writeauth_test.go:2451). Task 15 property 6 names this
// property back so the pair is not dropped between the two commits.
//
// AND ONE PROPERTY IS DECIDABLE ONLY IN HALF, which is worth stating in the same breath. Property 2
// is "the only producer of a pq_secret is the sampler, and the sampler's only input is an
// io.Reader". Half A -- the signature -- an AST scan decides. Half B -- the body reaches the reader
// it was handed and reaches no derivation -- an AST scan decides. What NO scan in this tree decides
// is "is the value this function returns a pq_secret", because Go reflection sees neither parameter
// names nor the meaning of returned bytes, and two earlier drafts of this property tried to write
// that question as a derived class and produced classes that convicted five functions this plan
// itself specifies. Both drafts are recorded in the plan; the identity of the value stays with the
// author and with section 5's text, and open item M1-17 carries the specification half.
package messagegroup

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The two directories the rules below run over: this package's own production source, and the
// control fixture the same rules are proved against.
const (
	epochOwnScanDir     = "."
	epochControlScanDir = "testdata/epoch"
)

// The root the two walks start from. It is checked for existence before either walk runs, because a
// rule about the call graph of a function that has been renamed is a rule about nothing and it
// reports clean.
const epochSamplerName = "NewPqSecret"

// The type expression an entropy source is written as, which is entropy_test.go's spelling and is
// repeated rather than shared so that a change to either file's reading is visible as a change.
const epochEntropyExpression = "io.Reader"

// ---------------------------------------------------------------------------
// the scan the walks are built on
// ---------------------------------------------------------------------------

// One directory's non test Go source, indexed by declared function name.
type epochScan struct {
	dir       string
	fileSet   *token.FileSet
	fileCount int
	// every function declaration of the directory, keyed by its own name. Methods are keyed by
	// the method name, which is what an edge in the syntax tree names.
	decls map[string][]*ast.FuncDecl
	// every package level name the directory declares, of any kind, which is what the
	// forbidden-file half resolves an edge against.
	names map[string]string
}

// epochScanSources reads one directory's non test Go source.
//
// A directory that yields no file and a directory that yields no function are both FATAL rather
// than empty, because either one clears every rule written over it while reporting a clean run.
func epochScanSources(t *testing.T, dir string) epochScan {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	scan := epochScan{
		dir:     dir,
		fileSet: token.NewFileSet(),
		decls:   map[string][]*ast.FuncDecl{},
		names:   map[string]string{},
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.ToSlash(filepath.Join(dir, name))
		parsed, err := parser.ParseFile(scan.fileSet, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		scan.fileCount += 1
		for _, declaration := range parsed.Decls {
			switch typed := declaration.(type) {
			case *ast.FuncDecl:
				if typed.Body == nil {
					continue
				}
				scan.decls[typed.Name.Name] = append(scan.decls[typed.Name.Name], typed)
				scan.names[typed.Name.Name] = path
			case *ast.GenDecl:
				for _, spec := range typed.Specs {
					switch value := spec.(type) {
					case *ast.ValueSpec:
						for _, ident := range value.Names {
							scan.names[ident.Name] = path
						}
					case *ast.TypeSpec:
						scan.names[value.Name.Name] = path
					}
				}
			}
		}
	}
	if scan.fileCount == 0 {
		t.Fatalf("%s holds no non test go file, so every rule below cleared its subject having read nothing", dir)
	}
	if len(scan.decls) == 0 {
		t.Fatalf("%s holds no function at all, so every walk over it is vacuous", dir)
	}
	return scan
}

// The text of one type expression, so a parameter list is compared as source rather than as a tree.
func epochRendered(scan epochScan, expr ast.Expr) string {
	text := &strings.Builder{}
	if err := printer.Fprint(text, scan.fileSet, expr); err != nil {
		return ""
	}
	return text.String()
}

// Every identifier one function names, across all of its declarations.
//
// Identifiers rather than call expressions, for writeauth_test.go's reason: a function value
// assigned to a variable, passed as an argument or stored in a table reaches its target just as
// well as a call does, and the selector of a method call is an identifier too.
func epochIdentsIn(decls []*ast.FuncDecl) []string {
	named := map[string]bool{}
	for _, decl := range decls {
		ast.Inspect(decl.Body, func(node ast.Node) bool {
			if ident, isIdent := node.(*ast.Ident); isIdent {
				named[ident.Name] = true
			}
			return true
		})
	}
	return slices.Sorted(maps.Keys(named))
}

// The functions of this directory one function names, which are the edges of the call graph.
func epochEdgesOf(scan epochScan, name string) []string {
	edges := []string{}
	for _, ident := range epochIdentsIn(scan.decls[name]) {
		if ident == name {
			continue
		}
		if _, declared := scan.decls[ident]; declared {
			edges = append(edges, ident)
		}
	}
	return edges
}

// Everything reachable from one function, transitively, including itself.
func epochReachableFrom(t *testing.T, scan epochScan, root string) map[string]bool {
	t.Helper()
	if _, declared := scan.decls[root]; !declared {
		t.Fatalf("%s is not declared in %s, so a walk from it would report clean having walked nothing", root, scan.dir)
	}
	reached := map[string]bool{root: true}
	frontier := []string{root}
	for 0 < len(frontier) {
		name := frontier[len(frontier)-1]
		frontier = frontier[:len(frontier)-1]
		for _, edge := range epochEdgesOf(scan, name) {
			if reached[edge] {
				continue
			}
			reached[edge] = true
			frontier = append(frontier, edge)
		}
	}
	return reached
}

// Every identifier named anywhere in a reachable set, which is what the forbidden-file half reads.
func epochIdentsReachedFrom(scan epochScan, reachable map[string]bool) map[string]bool {
	named := map[string]bool{}
	for name := range reachable {
		for _, ident := range epochIdentsIn(scan.decls[name]) {
			named[ident] = true
		}
	}
	return named
}

// ---------------------------------------------------------------------------
// the derivation class, derived off the OPERATION and over a derived scope
// ---------------------------------------------------------------------------

// The directories this package's production source can reach at all, derived off its own imports.
//
// The SCOPE question (R3a), answered without a list. Every github.com/urnetwork/connect/* import
// this package holds names a sibling directory of the module, so the set of packages a function
// here can call into is read off the import specs rather than written down -- and a third urnetwork
// package imported next week is in scope on the commit that adds it. A gate that derives its class
// and then enumerates its scope is not a derived gate, which is the half of rule 5 this file was
// failing before it was rewritten: the earlier reading named keyschedule.go, handle.go and
// ../message/writeauth.go, and a derivation written in a fourth file was invisible to it.
func epochReachableRoots(t *testing.T) []string {
	t.Helper()
	own := epochScanSources(t, epochOwnScanDir)
	roots := []string{epochOwnScanDir}
	entries, err := os.ReadDir(epochOwnScanDir)
	if err != nil {
		t.Fatalf("read this package's directory: %v", err)
	}
	const modulePrefix = `"github.com/urnetwork/connect/`
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(own.fileSet, filepath.ToSlash(filepath.Join(epochOwnScanDir, name)), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, spec := range parsed.Imports {
			path := spec.Path.Value
			if !strings.HasPrefix(path, modulePrefix) {
				continue
			}
			sibling := "../" + strings.TrimSuffix(strings.TrimPrefix(path, modulePrefix), `"`)
			if info, err := os.Stat(sibling); err != nil || !info.IsDir() {
				t.Fatalf("%s imports %s and %s is not a directory, so the scope this rule walks is not the scope the package reaches",
					name, path, sibling)
			}
			if !slices.Contains(roots, sibling) {
				roots = append(roots, sibling)
			}
		}
	}
	if len(roots) < 2 {
		t.Fatal("this package's production source reads as importing no other urnetwork package, so the scope below is this directory alone and every cross package edge is invisible to it")
	}
	slices.Sort(roots)
	return roots
}

// Every function of one directory that reaches a key derivation, transitively.
//
// The class is derived off the OPERATION and not off a named provider: a body that calls Expand or
// Extract on anything, or that names an hkdf entry point at all, is a derivation, and everything
// that reaches one is in the class with it. That reads the same in this package, where every
// derivation goes through mls.CryptoProvider because guardrail G1 forbids spelling crypto/hkdf, and
// in connect/message, where writeauth.go calls hkdf.Expand directly -- so one rule covers both
// without a row per package and without a row per provider.
//
// It is computed by walking the call graph BACKWARDS from the derivation roots, which is what makes
// it affordable over a directory the size of connect/mls.
func epochDerivationClass(t *testing.T, scan epochScan) []string {
	t.Helper()
	roots := map[string]bool{}
	reverse := map[string][]string{}
	for name := range scan.decls {
		for _, edge := range epochEdgesOf(scan, name) {
			reverse[edge] = append(reverse[edge], name)
		}
		if epochDerives(scan, name) {
			roots[name] = true
		}
	}
	// An EMPTY class is not fatal here and is fatal in the caller, because emptiness means two
	// different things at the two altitudes. One root of the scope may honestly hold no
	// derivation -- mls/syntax is a codec and holds none, measured -- while a scope in which
	// NOTHING derives is a reading that has stopped working. epochScanSources has already made
	// a directory that yielded no file and one that yielded no function fatal, so an empty class
	// here is a fact about the package rather than about the walk.
	class := map[string]bool{}
	frontier := slices.Sorted(maps.Keys(roots))
	for name := range roots {
		class[name] = true
	}
	for 0 < len(frontier) {
		name := frontier[len(frontier)-1]
		frontier = frontier[:len(frontier)-1]
		for _, caller := range reverse[name] {
			if class[caller] {
				continue
			}
			class[caller] = true
			frontier = append(frontier, caller)
		}
	}
	return slices.Sorted(maps.Keys(class))
}

// Whether one declaration performs a key derivation itself.
func epochDerives(scan epochScan, name string) bool {
	derives := false
	for _, decl := range scan.decls[name] {
		ast.Inspect(decl.Body, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.Ident:
				// an hkdf entry point under any spelling of the call
				if typed.Name == "hkdf" {
					derives = true
				}
			case *ast.CallExpr:
				selector, isSelector := typed.Fun.(*ast.SelectorExpr)
				if !isSelector {
					return true
				}
				if selector.Sel.Name == "Expand" || selector.Sel.Name == "Extract" {
					derives = true
				}
			}
			return true
		})
	}
	return derives
}

// ---------------------------------------------------------------------------
// the carrier walk: does the sampler read the source it was HANDED
// ---------------------------------------------------------------------------
// ---------------------------------------------------------------------------

// One positional parameter slot's name, with "" where the declaration named none.
func epochParameterNames(function *ast.FuncDecl) []string {
	names := []string{}
	if function.Type.Params == nil {
		return names
	}
	for _, field := range function.Type.Params.List {
		if len(field.Names) == 0 {
			names = append(names, "")
			continue
		}
		for _, ident := range field.Names {
			names = append(names, ident.Name)
		}
	}
	return names
}

// The parameter names of one declaration whose type is written as an entropy source.
func epochEntropyParameterNames(scan epochScan, function *ast.FuncDecl) []string {
	names := []string{}
	if function.Type.Params == nil {
		return names
	}
	for _, field := range function.Type.Params.List {
		if epochRendered(scan, field.Type) != epochEntropyExpression {
			continue
		}
		for _, ident := range field.Names {
			names = append(names, ident.Name)
		}
	}
	return names
}

// Whether one function reads the entropy source it was handed, following the value through the
// directory's own call graph.
//
// The carrier set is closed to a fixed point: the root's io.Reader parameters seed it; a carrier
// passed as the i-th argument of a call to a function this directory declares adds THAT function's
// i-th parameter; and a carrier assigned to a local name adds the name. A read is io.ReadFull with
// a carrier in the reader position, or a Read call on a carrier.
//
// A body that ignores its argument and answers a constant therefore fails, and so does one that
// draws from a source the caller never named -- which is the fallback shape, and the one every
// behavioural test passes.
func epochReadsTheSourceItWasHanded(t *testing.T, scan epochScan, root string) (bool, int) {
	t.Helper()
	decls, declared := scan.decls[root]
	if !declared {
		t.Fatalf("%s is not declared in %s", root, scan.dir)
	}
	carriers := map[string]map[string]bool{}
	add := func(function string, name string) bool {
		if name == "" || name == "_" {
			return false
		}
		if carriers[function] == nil {
			carriers[function] = map[string]bool{}
		}
		if carriers[function][name] {
			return false
		}
		carriers[function][name] = true
		return true
	}
	seeded := false
	for _, decl := range decls {
		for _, name := range epochEntropyParameterNames(scan, decl) {
			if add(root, name) {
				seeded = true
			}
		}
	}
	if !seeded {
		return false, 0
	}
	reads := false
	followed := 0
	for changed := true; changed; {
		changed = false
		for function := range maps.Clone(carriers) {
			held := maps.Clone(carriers[function])
			for _, decl := range scan.decls[function] {
				ast.Inspect(decl.Body, func(node ast.Node) bool {
					switch typed := node.(type) {
					case *ast.AssignStmt:
						// window := random keeps the same reader, so the name it
						// was given carries it too
						for i, right := range typed.Rhs {
							ident, isIdent := right.(*ast.Ident)
							if !isIdent || !held[ident.Name] || len(typed.Lhs) <= i {
								continue
							}
							if left, isLeft := typed.Lhs[i].(*ast.Ident); isLeft {
								if add(function, left.Name) {
									changed = true
								}
							}
						}
					case *ast.CallExpr:
						switch callee := typed.Fun.(type) {
						case *ast.SelectorExpr:
							qualifier, isIdent := callee.X.(*ast.Ident)
							if !isIdent {
								return true
							}
							// io.ReadFull(random, ...)
							if qualifier.Name == "io" && callee.Sel.Name == "ReadFull" && 0 < len(typed.Args) {
								if arg, isArg := typed.Args[0].(*ast.Ident); isArg && held[arg.Name] {
									reads = true
								}
								return true
							}
							// random.Read(...)
							if callee.Sel.Name == "Read" && held[qualifier.Name] {
								reads = true
							}
						case *ast.Ident:
							target, isDeclared := scan.decls[callee.Name]
							if !isDeclared || len(target) == 0 {
								return true
							}
							parameters := epochParameterNames(target[0])
							for i, argument := range typed.Args {
								ident, isIdent := argument.(*ast.Ident)
								if !isIdent || !held[ident.Name] || len(parameters) <= i {
									continue
								}
								followed += 1
								if add(callee.Name, parameters[i]) {
									changed = true
								}
							}
						}
					}
					return true
				})
			}
		}
	}
	return reads, followed
}

// ---------------------------------------------------------------------------
// property 2, half A: the sampler's parameter list, as a signature
// ---------------------------------------------------------------------------

// What one declaration's parameter list and results read as, for the signature half.
type epochSignature struct {
	parameters []string
	results    []string
}

func epochSignatureOf(t *testing.T, scan epochScan, name string) epochSignature {
	t.Helper()
	decls, declared := scan.decls[name]
	if !declared || len(decls) == 0 {
		t.Fatalf("%s is not declared in %s, so its signature cannot be read and this rule holds nothing", name, scan.dir)
	}
	read := epochSignature{parameters: []string{}, results: []string{}}
	function := decls[0]
	if function.Type.Params != nil {
		for _, field := range function.Type.Params.List {
			count := len(field.Names)
			if count == 0 {
				count = 1
			}
			for i := 0; i < count; i += 1 {
				read.parameters = append(read.parameters, epochRendered(scan, field.Type))
			}
		}
	}
	if function.Type.Results != nil {
		for _, field := range function.Type.Results.List {
			count := len(field.Names)
			if count == 0 {
				count = 1
			}
			for i := 0; i < count; i += 1 {
				read.results = append(read.results, epochRendered(scan, field.Type))
			}
		}
	}
	return read
}

// TestThePqSecretSamplerTakesAnEntropySourceAndNothingElse is property 2 half A.
//
// The defence is the SIGNATURE and not the body, exactly as it is for NewEphRoot and for AADBody
// under G4. A sampler that also took a storage root, an epoch, a class key or a group would compile,
// round trip, agree between two clients and pass every behavioural test in this package while the
// post quantum property was gone -- so the second parameter is refused as a declaration, which a
// syntax tree decides, rather than as a behaviour no test can observe.
func TestThePqSecretSamplerTakesAnEntropySourceAndNothingElse(t *testing.T) {
	scan := epochScanSources(t, epochOwnScanDir)
	signature := epochSignatureOf(t, scan, epochSamplerName)
	if want := []string{epochEntropyExpression}; !slices.Equal(signature.parameters, want) {
		t.Errorf("%s takes %v and section 5.10 E1's sampler takes %v and nothing else: a pq_secret derived from anything durable compiles, round trips and forfeits the PQ property in silence",
			epochSamplerName, signature.parameters, want)
	}
	if want := []string{"[]byte", "error"}; !slices.Equal(signature.results, want) {
		t.Errorf("%s answers %v, want %v: the draw and the refusal, and no third value a caller could mistake for a second secret",
			epochSamplerName, signature.results, want)
	}
	t.Logf("%d files, %d functions, %s%v %v", scan.fileCount, len(scan.decls), epochSamplerName, signature.parameters, signature.results)
}

// The control on the signature reader: a sampler with a second parameter must read as one, and the
// clean one must not, or the rule above is satisfied by a reader that answers the same thing to
// everything.
func TestTheSignatureReaderSeparatesTheControlSamplers(t *testing.T) {
	control := epochScanSources(t, epochControlScanDir)
	clean := epochSignatureOf(t, control, "SamplerThatReads")
	if want := []string{epochEntropyExpression}; !slices.Equal(clean.parameters, want) {
		t.Errorf("the reader read SamplerThatReads as taking %v, want %v", clean.parameters, want)
	}
	tainted := epochSignatureOf(t, control, "SamplerThatDerives")
	if want := []string{epochEntropyExpression, "[]byte"}; !slices.Equal(tainted.parameters, want) {
		t.Errorf("the reader read SamplerThatDerives as taking %v, want %v; a second parameter has to be visible or the rule above holds nothing",
			tainted.parameters, want)
	}
}

// ---------------------------------------------------------------------------
// property 2, half B: the sampler's body reaches the reader and no derivation
// ---------------------------------------------------------------------------

// TestThePqSecretSamplerReachesItsSourceAndNoDerivation is property 2 half B.
//
// Two assertions over the sampler's call graph, and they are independent: a sampler can read its
// source and still derive from a storage root, and the control holds one that does exactly that.
//
//   - it REACHES the entropy source it was handed, so a body that ignores its argument and answers
//     a constant fails, and so does one that draws from a source the caller never named;
//   - it REACHES NOTHING that derives -- not this package's key schedule or handle derivations, not
//     connect/message's write key or read key, not connect/mls's key schedule, and no hkdf entry
//     point at all. That is what refuses a pq_secret computed from storage_root[n], and it refuses
//     it whatever the return type is.
//
// Both the class and the scope are derived. The class is every function that reaches an Expand, an
// Extract or an hkdf entry point, computed backwards from those roots; the scope is this directory
// plus every urnetwork package this one's production source imports, read off the import specs. A
// derivation written in a file nobody has created yet, in any of those packages, is forbidden on
// the commit that writes it.
//
// WHAT THE WALK CANNOT SEE, stated rather than left to be discovered: a derivation reached through
// an interface, through reflection, or out of a package this scan does not read. It sees the source
// where the defect would be written, which is what writeauth_test.go's own walk says of itself.
//
// AND WHAT ITS ANTI-VACUITY PROOF IS. The plan asks for a walk that fatals if it followed no edge at
// all. A CORRECT sampler follows none -- it calls io.ReadFull and returns, and that is the whole of
// it -- so that check would be red against correct code, which is the defect this project keeps
// finding from the other side. What stands in its place is TestTheEpochWalksFlagTheControlFixture,
// which runs the identical rule over a fixture holding five samplers and requires the right two to
// be flagged by each half; a walk that followed no edge fails there rather than passing here.
func TestThePqSecretSamplerReachesItsSourceAndNoDerivation(t *testing.T) {
	scan := epochScanSources(t, epochOwnScanDir)
	reachable := epochReachableFrom(t, scan, epochSamplerName)

	reads, followed := epochReadsTheSourceItWasHanded(t, scan, epochSamplerName)
	if !reads {
		t.Errorf("%s does not read the io.Reader it was handed, so the source a caller names is decoration; a draw that ignores its argument still answers thirty two well formed octets and every round trip still passes",
			epochSamplerName)
	}
	t.Logf("the carrier walk followed %d hand offs out of %s and reached %d functions", followed, epochSamplerName, len(reachable))

	named := epochIdentsReachedFrom(scan, reachable)
	roots := epochReachableRoots(t)
	// the two roots whose derivations this package is actually built on, each named so a reading
	// that stopped working fails loudly rather than clearing everything quietly. They are sanity
	// checks on the READER and not the class, which is computed.
	sanity := map[string]string{epochOwnScanDir: "StorageRoot", "../message": "WriteKey"}
	total := 0
	for _, root := range roots {
		reachedScan := epochScanSources(t, root)
		derivations := epochDerivationClass(t, reachedScan)
		total += len(derivations)
		if expect, isChecked := sanity[root]; isChecked && !slices.Contains(derivations, expect) {
			t.Fatalf("the derived class over %s holds %d functions and does not include %s; the rule is looking for the wrong thing",
				root, len(derivations), expect)
		}
		hit := 0
		for _, deriver := range derivations {
			if !named[deriver] {
				continue
			}
			hit += 1
			t.Errorf("%s reaches %s, declared in %s and in %s's derivation class: spec A section 5.10 E1 has pq_secret arrive under X-Wing, and a pq_secret computed from anything already in the schedule forfeits the post quantum property while every test still passes",
				epochSamplerName, deriver, reachedScan.names[deriver], root)
		}
		t.Logf("%s: %d derivations, %d reached", root, len(derivations), hit)
	}
	if total == 0 {
		t.Fatalf("nothing in %v reaches an Expand, an Extract or an hkdf entry point, so this rule cleared the sampler against an empty class", roots)
	}
}

// The positive control for both halves of the walk. Without it the test above proves nothing: it
// reports clean, and a walk that followed no edge at all reports clean too.
//
// The fixture holds five samplers. Two are clean -- one reading its argument directly and one
// reading it two hops away under two different parameter names, which is what says the carrier is
// followed rather than matched by spelling. Three are tainted, one per shape the rule has to see: a
// body that ignores its reader, a body that reads a package level one, and a body that reads its
// own reader correctly AND derives from a storage root three hops down, which is what says the two
// halves are independent.
func TestTheEpochWalksFlagTheControlFixture(t *testing.T) {
	control := epochScanSources(t, epochControlScanDir)

	derivations := epochDerivationClass(t, control)
	want := []string{"SamplerThatDerives", "derive", "expandFrom", "kdf"}
	if !slices.Equal(derivations, want) {
		t.Fatalf("the derivation class over the control is %v, want %v; the walk is not following the calls", derivations, want)
	}

	for _, entry := range []struct {
		root      string
		reads     bool
		derives   bool
		whyItIsIn string
	}{
		{root: "SamplerThatReads", reads: true, derives: false, whyItIsIn: "it reads its own argument"},
		{root: "SamplerThatReadsViaHelper", reads: true, derives: false, whyItIsIn: "it reads its own argument two hops away"},
		{root: "SamplerThatIgnoresItsReader", reads: false, derives: false, whyItIsIn: "it answers a constant"},
		{root: "SamplerThatReadsAnotherSource", reads: false, derives: false, whyItIsIn: "it draws from a package level source"},
		{root: "SamplerThatDerives", reads: true, derives: true, whyItIsIn: "it reads its own argument and derives as well"},
	} {
		reads, _ := epochReadsTheSourceItWasHanded(t, control, entry.root)
		if reads != entry.reads {
			t.Errorf("the carrier walk read %s as reads=%v, want %v: %s", entry.root, reads, entry.reads, entry.whyItIsIn)
		}
		reachable := epochReachableFrom(t, control, entry.root)
		derives := false
		for _, deriver := range derivations {
			if reachable[deriver] {
				derives = true
				break
			}
		}
		if derives != entry.derives {
			t.Errorf("the derivation walk read %s as derives=%v, want %v: %s", entry.root, derives, entry.derives, entry.whyItIsIn)
		}
	}
}

// ---------------------------------------------------------------------------
// property 1: the draw is the source's, and nothing else's
// ---------------------------------------------------------------------------

// Gate B's own content -- the nil refusal and the exhausted refusal -- is held by
// entropy_test.go's derived class, which this task added a probe row to and which mls's
// TestNoEntropyTakingFunctionLivesWhereThisGateCannotCallIt holds a residual row for. What is here
// is the half those two cannot see: that the value IS the source's bytes.
//
// This is the entropy substitution p5 shipped twice in one task and no correctness test could see,
// because the values were still well formed and still round tripped. A sampler that expanded the
// draw, whitened it, or mixed in a counter would answer thirty two good octets, would still differ
// between two calls, and would still round trip -- and the only assertion that separates it from a
// draw is this one.
func TestAPqSecretIsTheDrawAndNotAnExpansionOfIt(t *testing.T) {
	for _, fill := range []byte{0x00, 0x5a, 0xff} {
		source := bytes.Repeat([]byte{fill}, PqSecretBytes)
		secret, err := NewPqSecret(bytes.NewReader(source))
		if err != nil {
			t.Fatalf("NewPqSecret over a %#02x source: %v", fill, err)
		}
		if !bytes.Equal(secret, source) {
			t.Errorf("NewPqSecret over a %#02x source answered %x, want the source's own %x: MASTER section 7 is a thirty two octet CSPRNG draw and an expansion of it is a different value that passes every round trip",
				fill, secret, source)
		}
	}
	// and the value MOVES with the source, which is what a whitener over a fixed internal seed
	// would fail while the row above passed
	first := make([]byte, PqSecretBytes)
	second := make([]byte, PqSecretBytes)
	for i := range first {
		first[i] = byte(i)
		second[i] = byte(0xff - i)
	}
	a, err := NewPqSecret(bytes.NewReader(first))
	if err != nil {
		t.Fatalf("NewPqSecret: %v", err)
	}
	b, err := NewPqSecret(bytes.NewReader(second))
	if err != nil {
		t.Fatalf("NewPqSecret: %v", err)
	}
	if bytes.Equal(a, b) {
		t.Error("two different sources produced the same pq_secret, so the value does not depend on the source it was drawn from")
	}
}

// A source shorter than the draw is a refusal and never a short secret padded out.
//
// Every prefix from empty to one octet short is tried rather than only the empty one, because the
// empty case is the one entropy_test.go already covers and a fallback triggered by a PARTIAL read is
// the shape it cannot see.
func TestAPqSecretRefusesASourceShorterThanTheDraw(t *testing.T) {
	full := make([]byte, PqSecretBytes)
	for i := range full {
		full[i] = byte(0x10 + i)
	}
	for short := 0; short < PqSecretBytes; short += 1 {
		secret, err := NewPqSecret(bytes.NewReader(full[:short]))
		if err == nil {
			t.Errorf("NewPqSecret over a %d octet source answered %x, so it reached some other source when the caller's ran dry",
				short, secret)
		}
		if secret != nil {
			t.Errorf("NewPqSecret over a %d octet source refused and answered %x alongside the refusal", short, secret)
		}
	}
}

// Two draws from the process source differ.
//
// This is what refuses a memoized sampler -- one that answers the first draw for ever after, which
// is "reuse the previous pq_secret on a retry" written where nothing else in this package can see
// it. Sixteen draws rather than two, because a memo that answered per goroutine or every other call
// would pass a single comparison.
func TestTwoPqSecretDrawsFromTheProcessSourceDiffer(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 16; i += 1 {
		secret, err := NewPqSecret(rand.Reader)
		if err != nil {
			t.Fatalf("NewPqSecret from the process source: %v", err)
		}
		if len(secret) != PqSecretBytes {
			t.Fatalf("NewPqSecret answered %d octets, want %d", len(secret), PqSecretBytes)
		}
		key := fmt.Sprintf("%x", secret)
		if seen[key] {
			t.Fatalf("draw %d repeated an earlier pq_secret, so the sampler is answering a value it kept rather than one it drew", i)
		}
		seen[key] = true
	}
}

// ---------------------------------------------------------------------------
// the width, which was defining its own correctness
// ---------------------------------------------------------------------------

// The width MASTER section 7 fixes for pq_secret[n], transcribed from the document rather than read
// off the constant this file checks.
//
// It is spelled a second time for the reason epochEnvelopeExporterLabel is spelled a second time:
// a rule that reads PqSecretBytes and then states every expectation in terms of PqSecretBytes
// compares this package against itself and holds at any width at all. That was measured rather than
// feared -- narrowing the constant to four octets left mls, mls/syntax, message and messagegroup
// entirely green, because every width assertion in this file was written in terms of the constant it
// was checking, and a four octet draw still extracts to a well formed storage_root that both clients
// agree on.
const masterSection7PqSecretOctets = 32

// TestThePqSecretWidthIsTheOneMasterSectionSevenFixes pins the width from three sides, of which only
// the first is a transcription.
//
// The DERIVED side is the one a second edit cannot move: section 5.12 step 1's four values are the
// ikm and the outputs of this package's own key schedule, and HKDF-Extract's output width is a fact
// about the hash rather than a constant anybody here declares. So the extraction is run and its
// answer measured. A constant narrowed to four octets fails that comparison whatever else is edited
// alongside it.
//
// The BEHAVIOURAL sides are what stop the transcription being satisfied vacuously in either
// direction: the sampler must fill a source of exactly the document's width (so the draw cannot
// grow) and must refuse one octet less (so it cannot shrink), and the constructor -- the door all
// four values of step 1 arrive through -- is held the same way.
func TestThePqSecretWidthIsTheOneMasterSectionSevenFixes(t *testing.T) {
	if PqSecretBytes != masterSection7PqSecretOctets {
		t.Errorf("PqSecretBytes is %d and MASTER section 7 fixes pq_secret[n] at %d octets. A shorter draw still extracts to a well formed storage_root, still round trips and still agrees between two clients, so nothing else in this tree can tell you",
			PqSecretBytes, masterSection7PqSecretOctets)
	}
	// the derived side: what the extraction this value is the ikm of actually produces
	extracted := StorageRoot(epochSecretFilled(0x01), epochSecretFilled(0x02))
	if PqSecretBytes != len(extracted) {
		t.Errorf("PqSecretBytes is %d and the extraction section 5.12 step 1's values feed and come out of produces %d octets. The width of these values is a property of the key schedule and not a number this package is free to choose",
			PqSecretBytes, len(extracted))
	}

	source := make([]byte, masterSection7PqSecretOctets)
	for i := range source {
		source[i] = byte(0xc0 + i)
	}
	secret, err := NewPqSecret(bytes.NewReader(source))
	if err != nil {
		t.Fatalf("NewPqSecret over a source of MASTER section 7's own width: %v -- the draw is WIDER than the document's and takes more entropy than the specification gives it", err)
	}
	if !bytes.Equal(secret, source) {
		t.Errorf("NewPqSecret over a %d octet source answered %d octets, %x", len(source), len(secret), secret)
	}
	if answered, err := NewPqSecret(bytes.NewReader(source[:masterSection7PqSecretOctets-1])); err == nil {
		t.Errorf("NewPqSecret answered %x from a source one octet short of MASTER section 7's width, so the draw is NARROWER than the document's", answered)
	}

	// and the constructor's door, held the same two ways
	engine := newTestEngine(t)
	handle := engine.createGroup(t, "pq-secret-width")
	full := func() []byte {
		value := make([]byte, masterSection7PqSecretOctets)
		for i := range value {
			value[i] = 0x77
		}
		return value
	}
	if _, err := NewProvisionalEpoch(handle, 1, full(), full(), full(), full()); err != nil {
		t.Errorf("NewProvisionalEpoch refused four values of MASTER section 7's own width: %v", err)
	}
	for _, wrong := range []int{masterSection7PqSecretOctets - 1, masterSection7PqSecretOctets + 1} {
		if _, err := NewProvisionalEpoch(handle, 1, make([]byte, wrong), full(), full(), full()); !errors.Is(err, ErrProvisionalEpochValue) {
			t.Errorf("NewProvisionalEpoch answered %v for a %d octet storage_root, want ErrProvisionalEpochValue: the four values of step 1 are %d octets each",
				err, wrong, masterSection7PqSecretOctets)
		}
	}

	// AND THE SESSION'S TWO DOORS, which is where a pq_secret reaches the seal path TODAY --
	// NewProvisionalEpoch has no production caller until task 15, and the review that found this
	// width unpinned named session.go's guard as the other half of the same gap: it checked that
	// the value was non EMPTY and nothing else, so a four octet secret walked into
	// StorageRoot(mls_secret, pq_secret) as the ikm and every test in this package stayed green.
	// The property is one property -- pq_secret[n] is thirty two octets -- so its doors are held
	// in one place rather than one case per file.
	for _, wrong := range []int{masterSection7PqSecretOctets - 1, masterSection7PqSecretOctets + 1} {
		if probe, err := buildProbeSession(make([]byte, wrong)); !errors.Is(err, ErrPqSecretLength) {
			if probe != nil {
				probe.session.Close()
			}
			t.Errorf("NewGroupSession answered %v for a %d octet pq_secret, want ErrPqSecretLength", err, wrong)
		}
	}
	probe, err := buildProbeSession(testPqSecret())
	if err != nil {
		t.Fatalf("buildProbeSession: %v", err)
	}
	defer probe.session.Close()
	for _, wrong := range []int{masterSection7PqSecretOctets - 1, masterSection7PqSecretOctets + 1} {
		if err := probe.session.AdvanceEpoch(make([]byte, wrong)); !errors.Is(err, ErrPqSecretLength) {
			t.Errorf("AdvanceEpoch answered %v for a %d octet pq_secret, want ErrPqSecretLength", err, wrong)
		}
	}
}

// ---------------------------------------------------------------------------
// the fixture the provisional epoch properties run over
// ---------------------------------------------------------------------------

// The label spec A section 5.11 exports env_key[k] under, transcribed from the specification so
// property 6 compares this package against the document and not against itself.
const epochEnvelopeExporterLabel = "URmessage/v1/envelope"

// A real GroupHandle with a counter on the one method the destructor calls.
//
// It EMBEDS the interface rather than standing in for it, so every other method is the real group's
// and a case here cannot pass against a handle that answers made up key material. CP3b's bar is
// "every key real, no test-only key source anywhere on the path", and a stub handle would satisfy
// this file while leaving that bar where it was.
type epochClearCountingHandle struct {
	GroupHandle
	cleared int
}

func (self *epochClearCountingHandle) ClearPendingCommit() {
	self.cleared += 1
	self.GroupHandle.ClearPendingCommit()
}

// One distinguishable thirty two octet value per name, so a failure says which secret survived.
func epochSecretFilled(fill byte) []byte {
	secret := make([]byte, PqSecretBytes)
	for i := range secret {
		secret[i] = fill
	}
	return secret
}

// The four secrets of section 5.12 step 1 and the two wraps, each with a second header over the
// same backing array.
//
// The alias is the whole point, and it is the shape zeroize_test.go uses: a check that read the
// struct's own field after Destroy set it to nil would pass against a destructor that dropped the
// slice and erased nothing, which is a destructor that leaves the epoch's key material live in the
// committer's arrays.
//
// SIX VALUES HERE ARE THE TEST'S AND NOT THE PRODUCT'S, named for the reason sessionfixture_test.go
// names its four: the four secrets and the two wraps are constant fills, because what these cases
// assert is that each one is ERASED and a case comparing six arrays is comparing the destructor
// rather than six draws. None of them is reachable from a production build -- they are declared in a
// _test.go file and NewProvisionalEpoch takes no value of its own -- and the pq_secret of the one
// case that is about the value rather than about the erasure, TestLostCommitResamplesPqSecret, comes
// out of NewPqSecret over crypto/rand. The GROUP HANDLE is real in every case here.
type epochProvisionalFixture struct {
	handle  *epochClearCountingHandle
	value   *ProvisionalEpoch
	aliases map[string][]byte
}

func newEpochProvisionalFixture(t *testing.T, name string) *epochProvisionalFixture {
	t.Helper()
	engine := newTestEngine(t)
	handle := &epochClearCountingHandle{GroupHandle: engine.createGroup(t, name)}
	storageRoot := epochSecretFilled(0x11)
	writeKey := epochSecretFilled(0x22)
	ephRoot := epochSecretFilled(0x33)
	pqSecret := epochSecretFilled(0x44)
	value, err := NewProvisionalEpoch(handle, handle.Epoch()+1, storageRoot, writeKey, ephRoot, pqSecret)
	if err != nil {
		t.Fatalf("NewProvisionalEpoch: %v", err)
	}
	firstWrap := epochSecretFilled(0x55)
	secondWrap := epochSecretFilled(0x66)
	if err := value.InstallWraps([][]byte{firstWrap, secondWrap}); err != nil {
		t.Fatalf("InstallWraps: %v", err)
	}
	return &epochProvisionalFixture{
		handle: handle,
		value:  value,
		aliases: map[string][]byte{
			"storage_root":    storageRoot[:],
			"write_key":       writeKey[:],
			"eph_root":        ephRoot[:],
			"pq_secret":       pqSecret[:],
			"the first wrap":  firstWrap[:],
			"the second wrap": secondWrap[:],
		},
	}
}

// A LIVE provisional epoch whose four secrets and wrap are the octets the destructor leaves behind.
//
// It exists for the destroyed flag's exemption and for nothing else. Destroy changes two things
// about a value at once -- it sets the flag and it zeroizes -- so a value that is live and already
// holds zeros is the one probe that tells those two apart, and a bool that reads the secrets rather
// than the flag answers TRUE here while the value is still live.
//
// The handle is a real group, for the reason epochClearCountingHandle gives.
func newEpochProvisionalOfErasedOctets(t *testing.T, name string) *ProvisionalEpoch {
	t.Helper()
	handle := newTestEngine(t).createGroup(t, name)
	value, err := NewProvisionalEpoch(handle, handle.Epoch()+1,
		make([]byte, PqSecretBytes), make([]byte, PqSecretBytes),
		make([]byte, PqSecretBytes), make([]byte, PqSecretBytes))
	if err != nil {
		t.Fatalf("NewProvisionalEpoch: %v", err)
	}
	if err := value.InstallWraps([][]byte{make([]byte, PqSecretBytes)}); err != nil {
		t.Fatalf("InstallWraps: %v", err)
	}
	return value
}

// ---------------------------------------------------------------------------
// property 3: the provisional value is destroyed as ONE THING
// ---------------------------------------------------------------------------

// TestDestroyingAProvisionalEpochErasesEverythingAndClearsTheStagedCommit is property 3.
//
// Both halves are asserted from ONE call, which is the point of the property and the reason the mls
// call lives inside the destructor: section 5.12 step 1 lists six things to discard, connect/mls
// knows one of them, and a caller who has to remember two erasures will one day make one. A
// destructor that zeroized and did not clear, or cleared and did not zeroize, fails here.
//
// The per secret half is asserted through an ALIAS over the same backing array, so a destructor
// that dropped the slices and erased nothing cannot satisfy it, and the class of slice fields is
// derived off the type rather than listed, so a seventh field added next week is checked without
// this test being edited.
func TestDestroyingAProvisionalEpochErasesEverythingAndClearsTheStagedCommit(t *testing.T) {
	fixture := newEpochProvisionalFixture(t, "destroy-one-thing")

	fixture.value.Destroy()

	if fixture.handle.cleared != 1 {
		t.Errorf("the destructor called ClearPendingCommit %d times, want 1: G10 names it as the thing that destroys the provisional state, and a zeroization beside it rather than inside it is a pair of erasures a caller has to remember",
			fixture.handle.cleared)
	}
	for what, alias := range fixture.aliases {
		for i, octet := range alias {
			if octet != 0 {
				t.Errorf("%s survived the destructor: octet %d is %#02x. Section 5.12 step 1 discards storage_root[n+1], write_key[n+1], eph_root[n+1], pq_secret[n+1] and every X-Wing wrap, and a half erase leaves the surviving half looking like a value somebody may use",
					what, i, octet)
				break
			}
		}
	}

	// the derived half: every slice typed field of the type is dropped, whatever it is called
	value := reflect.ValueOf(fixture.value).Elem()
	sliceFields := 0
	for i := 0; i < value.NumField(); i += 1 {
		if value.Field(i).Kind() != reflect.Slice {
			continue
		}
		sliceFields += 1
		if !value.Field(i).IsNil() {
			t.Errorf("the field %s is still held after the destructor ran, so a reader that reaches the struct directly still has it",
				value.Type().Field(i).Name)
		}
	}
	if sliceFields == 0 {
		t.Fatal("ProvisionalEpoch declares no slice typed field, so the rule above cleared a type it read nothing of")
	}
	if fixture.handle.Epoch() != 0 {
		t.Errorf("the group moved to epoch %d, and the destructor must drop a STAGED commit rather than the live epoch", fixture.handle.Epoch())
	}
}

// A second Destroy is the same state as the first, and must not clear a commit staged AFTER it.
//
// Section 5.12 step 5 has the losing committer retry -- which stages another commit on the same
// group -- so a destructor that cleared unconditionally on every call would erase the retry's own
// staged epoch if the value were destroyed twice.
func TestDestroyingAProvisionalEpochTwiceClearsOnce(t *testing.T) {
	fixture := newEpochProvisionalFixture(t, "destroy-twice")
	fixture.value.Destroy()
	fixture.value.Destroy()
	if fixture.handle.cleared != 1 {
		t.Errorf("two Destroy calls cleared %d staged commits, want 1: the second call must not reach a commit the retry staged after this state was destroyed",
			fixture.handle.cleared)
	}
}

// A GroupHandle whose ClearPendingCommit records what the value said about itself and then fails.
//
// It embeds the real handle for the reason epochClearCountingHandle does, and it overrides the one
// method the destructor calls out through, because what it is standing in for is not a stub group --
// it is a group whose ClearPendingCommit did not return. engine.go's open item M1-43 contemplates
// exactly that for a foreign GroupHandle, and it is the ONLY place the destructor can be interrupted:
// zeroize is this package's own leaf and cannot fail, so a value that is already refusing when this
// method runs is a value that is already refusing at every point after the destructor started.
type epochPanickingClearHandle struct {
	GroupHandle
	value               *ProvisionalEpoch
	saw                 bool
	destroyedWhenCalled bool
}

func (self *epochPanickingClearHandle) ClearPendingCommit() {
	self.saw = true
	self.destroyedWhenCalled = self.value.Destroyed()
	panic("this group handle failed inside the destructor")
}

// TestAProvisionalEpochIsAlreadyRefusingWhenItCallsIntoTheGroupHandle is the destructor's ordering,
// which is fail closed and which nothing held.
//
// Destroy sets the flag BEFORE it erases anything and before it calls out of this package. Measured
// before this case existed: moving that assignment to the end of the body passed the whole suite,
// and a destructor ordered that way leaves a value that has been fully erased and is still
// ANSWERING if the call out does not return -- so a caller seals under thirty two zero octets, which
// is the failure ErrProvisionalEpochDestroyed's own doc comment names.
func TestAProvisionalEpochIsAlreadyRefusingWhenItCallsIntoTheGroupHandle(t *testing.T) {
	engine := newTestEngine(t)
	handle := &epochPanickingClearHandle{GroupHandle: engine.createGroup(t, "destroy-fail-closed")}
	value, err := NewProvisionalEpoch(handle, handle.Epoch()+1,
		epochSecretFilled(0x11), epochSecretFilled(0x22), epochSecretFilled(0x33), epochSecretFilled(0x44))
	if err != nil {
		t.Fatalf("NewProvisionalEpoch: %v", err)
	}
	handle.value = value

	recovered := func() (recovered any) {
		defer func() {
			recovered = recover()
		}()
		value.Destroy()
		return nil
	}()

	if !handle.saw {
		t.Fatal("the destructor never reached ClearPendingCommit, so this case observed nothing about the order it does things in")
	}
	if recovered == nil {
		t.Fatal("the fixture handle did not fail, so this case observed nothing")
	}
	if !handle.destroyedWhenCalled {
		t.Error("the provisional epoch was still answering when its destructor called out into the group handle. That call is the one place this destructor can be interrupted, and at the moment it runs the four secrets are already erased -- so a value still answering there hands a caller thirty two zero octets rather than a refusal")
	}
	for _, method := range epochSliceAnsweringAccessors(t) {
		bound := reflect.ValueOf(value).MethodByName(method.Name)
		results := bound.Call(epochZeroArgumentsFor(bound))
		if err := epochErrorResultOf(results); !errors.Is(err, ErrProvisionalEpochDestroyed) {
			t.Errorf("%s answered %v after a destructor that failed part way, want ErrProvisionalEpochDestroyed", method.Name, err)
		}
	}
}

// TestTheZeroValueOfAProvisionalEpochRefusesAndDestroysWithoutPanicking is the value nobody
// constructed.
//
// NewProvisionalEpoch refuses a nil handle, so `var value ProvisionalEpoch` and a deferred Destroy
// written above a construction that then failed are the two ways one of these exists, and both are
// ordinary go. Before this case, the first answered a nil pq_secret and NO error -- a caller that
// checked the error and sealed under what it was handed would seal under nothing -- and the second
// took the process down on a nil GroupHandle inside the destructor, on the cleanup path of the
// failure it was cleaning up.
func TestTheZeroValueOfAProvisionalEpochRefusesAndDestroysWithoutPanicking(t *testing.T) {
	value := &ProvisionalEpoch{}
	for _, method := range epochSliceAnsweringAccessors(t) {
		bound := reflect.ValueOf(value).MethodByName(method.Name)
		results := bound.Call(epochZeroArgumentsFor(bound))
		if err := epochErrorResultOf(results); !errors.Is(err, ErrProvisionalEpochDestroyed) {
			t.Errorf("%s on a zero valued provisional epoch answered %v, want ErrProvisionalEpochDestroyed: it holds no epoch's secrets and a caller handed its zeros with no error beside them would seal under them",
				method.Name, err)
		}
		for _, beside := range results {
			if beside.Kind() == reflect.Slice && !beside.IsNil() {
				t.Errorf("%s answered %d values from a zero valued provisional epoch", method.Name, beside.Len())
			}
		}
	}

	recovered := func() (recovered any) {
		defer func() {
			recovered = recover()
		}()
		value.Destroy()
		return nil
	}()
	if recovered != nil {
		t.Errorf("destroying a zero valued provisional epoch panicked with %v: a destructor is the one method a caller writes in a defer above the thing it destroys, so it must survive the value never having been made",
			recovered)
	}
	if !value.Destroyed() {
		t.Error("a destroyed zero value does not report itself destroyed, so task 15's readers would read it as live")
	}
}

// The X-Wing wraps are installed once, and a second install is refused rather than dropping the
// first set.
//
// This is the hazard connect/mls's TestEveryPathThatDropsHeldKeyMaterialErasesItFirst derives off
// this package's source, asserted here as behaviour so it is held from both sides: that gate reads
// the syntax tree and would be satisfied by a refusal that refused the wrong thing, and this reads
// the values and would be satisfied by a body the gate cannot see. Neither alone is the property.
func TestTheWrapsOfAProvisionalEpochAreInstalledOnce(t *testing.T) {
	fixture := newEpochProvisionalFixture(t, "wraps-write-once")
	installed, err := fixture.value.Wraps()
	if err != nil {
		t.Fatalf("Wraps: %v", err)
	}
	if len(installed) != 2 {
		t.Fatalf("the fixture installed %d wraps, want 2", len(installed))
	}
	second := [][]byte{epochSecretFilled(0x99)}
	if err := fixture.value.InstallWraps(second); !errors.Is(err, ErrProvisionalEpochWraps) {
		t.Errorf("a second install answered %v, want ErrProvisionalEpochWraps: it would drop the first fan out's wraps with nothing erasing them", err)
	}
	held, err := fixture.value.Wraps()
	if err != nil {
		t.Fatalf("Wraps after the refused install: %v", err)
	}
	if len(held) != 2 || !bytes.Equal(held[0], installed[0]) || !bytes.Equal(held[1], installed[1]) {
		t.Error("the refused install moved the set anyway, so the refusal is a message rather than a guard")
	}
	if !bytes.Equal(second[0], epochSecretFilled(0x99)) {
		t.Error("the refused install erased the caller's own set, which it was never handed ownership of")
	}

	// and an empty install is not an install: it would leave the field nil and let a later one
	// land, which is the write once rule satisfied by a value that was never written
	fresh := newEpochProvisionalFixture(t, "wraps-empty-install")
	fresh.value.Destroy()
	untouched := newTestEngine(t).createGroup(t, "wraps-empty")
	bare, err := NewProvisionalEpoch(untouched, 1, epochSecretFilled(0x01), epochSecretFilled(0x02),
		epochSecretFilled(0x03), epochSecretFilled(0x04))
	if err != nil {
		t.Fatalf("NewProvisionalEpoch: %v", err)
	}
	for _, empty := range [][][]byte{nil, {}} {
		if err := bare.InstallWraps(empty); !errors.Is(err, ErrProvisionalEpochWraps) {
			t.Errorf("installing %d wraps answered %v, want ErrProvisionalEpochWraps", len(empty), err)
		}
	}
	if err := bare.InstallWraps([][]byte{epochSecretFilled(0xaa)}); err != nil {
		t.Errorf("the first real install after two empty ones answered %v", err)
	}
}

// ---------------------------------------------------------------------------
// property 4, behavioural half: nothing answers afterwards
// ---------------------------------------------------------------------------

// Every exported method of *ProvisionalEpoch, read off the type rather than listed.
func epochExportedMethodsOfTheProvisionalValue(t *testing.T) []reflect.Method {
	t.Helper()
	subject := reflect.TypeOf(&ProvisionalEpoch{})
	methods := []reflect.Method{}
	for i := 0; i < subject.NumMethod(); i += 1 {
		methods = append(methods, subject.Method(i))
	}
	if len(methods) == 0 {
		t.Fatal("*ProvisionalEpoch declares no exported method, so every rule below cleared a surface it read nothing of")
	}
	return methods
}

// Whether a method type answers an error, and whether it answers anything that is not an error --
// which is the shape half of property 4.
//
// THERE IS NO BOOL EXEMPTION HERE AND THERE USED TO BE, which is worth saying because the exemption
// read as harmless and was not. It exempted every result whose Kind is Bool -- derived from the
// INSTANCE, because Destroyed happens to return one -- while the sentence it was implementing is
// "neither an error nor the destroyed flag". Under it, an exported PqSecretMatches([]byte) bool or a
// WrapsInstalled() bool answered about a destroyed value's secret with no door to refuse through and
// no gate said anything: measured, both passed the whole suite. The exemption is now derived from
// the PROPERTY instead, by watching what a method answers, in epochIsTheDestroyedFlag below.
func epochMethodResults(signature reflect.Type) (answersError bool, answersState bool) {
	errorType := reflect.TypeOf((*error)(nil)).Elem()
	for i := 0; i < signature.NumOut(); i += 1 {
		if signature.Out(i) == errorType {
			answersError = true
			continue
		}
		answersState = true
	}
	return answersError, answersState
}

// Whether one exported method IS the destroyed flag, decided by watching it rather than by its type
// or by its name.
//
// This is the exemption property 4's shape half owes to exactly one method, derived from what that
// method is FOR rather than from what the one method that exists today happens to return. The flag
// answers G10's own question, so it takes nothing, answers one bool, and -- the half that makes it
// the flag -- answers FALSE while the value is live and TRUE once the destructor has run. Nothing
// else earns the exemption: a method taking an argument is answering a question about that argument
// and not about the value's state, and a bool that reads true and then false is reporting some other
// fact and owes a refusal like every other accessor.
//
// It holds Destroyed in BOTH directions as a side effect, which is the point rather than an
// accident. Only one assertion in this file used to touch that method and it ran after Destroy, so a
// body of `return true` passed the whole suite -- and task 15 property 6's derived reader class is
// to be built on this flag, which makes a constant here a foundation rather than a wart.
//
// IT WATCHES A SET OF VALUES AND NOT ONE OF EACH, AND THE SET IS WHAT MAKES THE EXEMPTION THE
// PROPERTY. Watching one live value and one destroyed one asks "does this bool differ between these
// two values", and the destructor changes two things about a value at once: it sets the flag AND it
// leaves the four secrets as zero octets. So a predicate reading the CONTENT rather than the
// lifecycle satisfies that reading too -- the 2026-09-09 verification planted
// PqSecretLeadsWithAZero and reproduced it, exempt from the shape rule and answering a bit OF the
// secret on a live value. The flag reports the DESTRUCTOR, so it must read false on every live
// value whatever the value holds, and the live set therefore includes one whose secrets are already
// the octets the destructor leaves. Any predicate reading erasedness rather than the flag reads
// true there and is refused the exemption.
func epochIsTheDestroyedFlag(method reflect.Method, live []reflect.Value, destroyed []reflect.Value) bool {
	if method.Type.NumIn() != 1 || method.Type.NumOut() != 1 || method.Type.Out(0).Kind() != reflect.Bool {
		return false
	}
	// an empty side would make this vacuous in one direction, which is the shape the whole file
	// fatals on rather than reports clean over
	if len(live) == 0 || len(destroyed) == 0 {
		return false
	}
	for _, one := range live {
		if one.MethodByName(method.Name).Call(nil)[0].Bool() {
			return false
		}
	}
	for _, one := range destroyed {
		if !one.MethodByName(method.Name).Call(nil)[0].Bool() {
			return false
		}
	}
	return true
}

// Whether a value of this type could be a place an epoch's octets come back out through.
//
// It is a WALK DOWN the type rather than a test of its outermost Kind, because "the shape the five
// accessors happen to have today" is the instance and "can carry an octet" is the property. []byte
// and [][]byte are what this type answers now; a [32]byte, a *[]byte, a struct holding a wrap, a
// map of them and a string are each a place pq_secret could leave through, and every one of them is
// outside a Kind == reflect.Slice reading.
//
// An INTERFACE or a FUNC answers yes rather than being followed, which is the fail closed
// direction: what a value of either carries is not decidable from the type, and a class that
// guessed no would exclude the shape a leak is easiest to hide in. error is removed by the caller
// before this is asked, because every accessor here answers one.
//
// THE COMPLEMENT IS STATED RATHER THAN LEFT TO BE INFERRED. A bool, a rune, an int and a uint64 are
// answered no. They are not octet sequences, and the two rules that hold them are the ones that do
// not care what shape state is in: property 4's shape half wants an error beside anything that is
// not the destroyed flag, and its behavioural half wants the ZERO value of whatever it is beside
// the refusal. A uint64 packing eight octets of pq_secret is therefore held there and not here --
// it cannot be answered by a destroyed value at all, and it can never BE the live storage, which is
// the only thing this class's two rules are about.
func epochTypeCarriesOctets(carrier reflect.Type, seen map[reflect.Type]bool) bool {
	// a type reached twice on one walk is a cycle rather than an octet, and a struct holding
	// itself would otherwise recur until the stack ended
	if seen[carrier] {
		return false
	}
	seen[carrier] = true
	switch carrier.Kind() {
	case reflect.Uint8, reflect.String, reflect.Interface, reflect.Func, reflect.UnsafePointer:
		return true
	case reflect.Slice, reflect.Array, reflect.Pointer, reflect.Chan:
		return epochTypeCarriesOctets(carrier.Elem(), seen)
	case reflect.Map:
		return epochTypeCarriesOctets(carrier.Key(), seen) || epochTypeCarriesOctets(carrier.Elem(), seen)
	case reflect.Struct:
		for i := 0; i < carrier.NumField(); i += 1 {
			if epochTypeCarriesOctets(carrier.Field(i).Type, seen) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// The octet answering members of a method set, and -- said out loud -- the ones that are not.
//
// It takes the methods rather than reading *ProvisionalEpoch itself so that the derivation can be
// proved against a shape this type does not have yet, which is the only way the narrowing below
// could have been observed at the commit that wrote it.
// TestTheOctetAnsweringClassIsReadOffResultsAndNotArity is that proof.
func epochOctetAnsweringMethodsIn(methods []reflect.Method) (answering []reflect.Method, excluded []string) {
	errorType := reflect.TypeOf((*error)(nil)).Elem()
	for _, method := range methods {
		carries := false
		for i := 0; i < method.Type.NumOut(); i += 1 {
			if method.Type.Out(i) == errorType {
				continue
			}
			if epochTypeCarriesOctets(method.Type.Out(i), map[reflect.Type]bool{}) {
				carries = true
				break
			}
		}
		if carries {
			answering = append(answering, method)
			continue
		}
		excluded = append(excluded, method.Name+" "+method.Type.String())
	}
	return answering, excluded
}

// Every exported method of the provisional value that can hand this value's octets back, read off
// the type.
//
// The class is what makes the rules over it rules rather than lists: task 15's fan out will add
// readers, and an accessor added for one of them is in the class on the commit that adds it.
//
// IT NARROWS BY THE PROPERTY AND NOT BY THE ARITY, AND THAT SENTENCE IS THIS FUNCTION'S WHOLE
// HISTORY. It used to open with a skip of every method whose NumIn is not one, and it argued that
// exclusion in its own comment from the one argument-taking method that exists: "a method that
// takes an argument is not an accessor -- InstallWraps is this type's writer". That is derived from
// the INSTANCE. The property these rules defend is "an exported method that can hand back this
// value's live octets", and a method that takes an argument can do exactly that: a
// PqSecretFor(purpose string) ([]byte, error) answering a copy of self.pqSecret was planted, passed
// the whole suite, and escaped both halves of the ownership rule and every transposition row.
//
// MEASURED AT 81b97ca, THAT NARROWING'S COMPLEMENT WAS EMPTY. InstallWraps answers only an error,
// so the result reading already removed it and the arity line removed nothing at all -- a filter
// written for members that do not exist, which would have begun removing real ones on the commit
// that added the first argument-taking accessor. An exclusion whose complement is empty and an
// exclusion whose complement is one named method are two different defects that look identical from
// outside, and neither is visible unless the complement is PRINTED. This one prints it.
//
// So the reading is off the RESULTS, in whatever shape they are answered, and an argument-taking
// member is DRIVEN with the zero value of each argument rather than skipped -- the same driver the
// refusal half already uses. A member the zero row cannot get octets out of is FATAL in
// epochBytesAnsweredBy rather than passed over, so this class fails closed on the shape it has not
// met yet instead of reporting clean over it.
//
// mls/GATES.md carries the rule this function is the fifth instance of, the query that finds the
// next one, and the index of every narrowing that query reaches. It carries NO open narrowing:
// OPEN is a verdict in its vocabulary and it is RED, so a narrowing recorded there and not closed
// fails the run. This comment used to send a reader to "the two narrowings in mls that the same
// query still reports open", which the document has not carried since those two were closed.
func epochSliceAnsweringAccessors(t *testing.T) []reflect.Method {
	t.Helper()
	accessors, excluded := epochOctetAnsweringMethodsIn(epochExportedMethodsOfTheProvisionalValue(t))
	if len(accessors) == 0 {
		t.Fatal("no exported method of *ProvisionalEpoch answers anything that can carry an octet, so every rule below read an empty class and reported clean over it")
	}
	names := []string{}
	for _, method := range accessors {
		names = append(names, method.Name)
	}
	t.Logf("the octet answering class of *ProvisionalEpoch is %v; excluded because no result of theirs can carry an octet: %v",
		names, excluded)
	return accessors
}

// The arguments this class is driven with: the zero value of each.
//
// It is a DRIVER and not a filter, which is the distinction the arity narrowing above got wrong. A
// member the zero row cannot get octets out of turns the rules red in epochBytesAnsweredBy rather
// than dropping out of the class, so an accessor whose argument means something has to be given a
// row here rather than quietly ceasing to be swept. It is the same construction
// TestEveryAccessorOfAProvisionalEpochRefusesOnceItHasBeenDestroyed already drives its own half
// with, which is where the shape of it comes from.
func epochZeroArgumentsFor(bound reflect.Value) []reflect.Value {
	arguments := []reflect.Value{}
	for i := 0; i < bound.Type().NumIn(); i += 1 {
		arguments = append(arguments, reflect.Zero(bound.Type().In(i)))
	}
	return arguments
}

// The octets one accessor answered, whatever shape it answered them in.
//
// A shape this cannot read is FATAL rather than skipped, because a rule that quietly ignores the one
// accessor it does not recognise stops holding on the commit that adds it -- and the accessor a
// future task adds is the one these rules exist for. That fatal is now the whole of the class's
// fail closed door: epochSliceAnsweringAccessors admits an argument-taking member, and a member
// answering octets in a shape nothing here has met, and both arrive at this fatal rather than at a
// skip inside the derivation.
//
// The member is driven with the ZERO value of each of its arguments. A member for which that row is
// the wrong one refuses, or answers nothing, and both land on a fatal here -- which is a demand for
// a row rather than a silent narrowing of the class.
func epochBytesAnsweredBy(t *testing.T, value *ProvisionalEpoch, method reflect.Method) [][]byte {
	t.Helper()
	bound := reflect.ValueOf(value).MethodByName(method.Name)
	results := bound.Call(epochZeroArgumentsFor(bound))
	if err := epochErrorResultOf(results); err != nil {
		t.Fatalf("%s on a live provisional epoch: %v", method.Name, err)
	}
	errorType := reflect.TypeOf((*error)(nil)).Elem()
	answered := [][]byte{}
	for _, result := range results {
		// the ERROR is skipped, and nothing else is skipped by its type. A reader that
		// passed over every result whose Kind is not Slice would be the instance shaped
		// exemption this file has already been caught by once: an accessor answering octets
		// AND a counter would have the counter read by nothing and rowed by nothing.
		if result.Type() == errorType {
			continue
		}
		// the Kind is read BEFORE Elem is asked for, because Elem panics on a uint64 and a
		// panic is not a reading. The class above admits any shape that can carry an octet, so
		// what arrives here is wider than the two shapes this rule knows how to read.
		switch {
		case result.Kind() == reflect.Slice && result.Type().Elem().Kind() == reflect.Uint8:
			answered = append(answered, result.Bytes())
		case result.Kind() == reflect.Slice && result.Type().Elem().Kind() == reflect.Slice &&
			result.Type().Elem().Elem().Kind() == reflect.Uint8:
			for i := 0; i < result.Len(); i += 1 {
				answered = append(answered, result.Index(i).Bytes())
			}
		default:
			t.Fatalf("%s answers %s, which this rule cannot read as octets and therefore holds nothing about",
				method.Name, result.Type())
		}
	}
	return answered
}

// The error among a call's results, or nil.
func epochErrorResultOf(results []reflect.Value) error {
	errorType := reflect.TypeOf((*error)(nil)).Elem()
	for _, result := range results {
		if result.Type() != errorType || result.IsNil() {
			continue
		}
		return result.Interface().(error)
	}
	return nil
}

// TestEveryAccessorOfAProvisionalEpochRefusesOnceItHasBeenDestroyed is property 4's behavioural
// half, which is the half that has a member at this commit.
//
// G10's own words are "there is no path that reads it afterwards". The derived class of in package
// READERS is empty here -- task 15's fan out and task 21's retry loop are the readers and neither
// exists -- so what lands is the value's own refusal, held over the whole exported surface by
// reflection so an accessor added later is in the class without this test being edited. The derived
// class of readers lands in task 15 property 6, which names this property back.
//
// Both directions, because a method that always refused would satisfy the after half vacuously:
// every error answering accessor must ANSWER before the destructor and REFUSE after it.
func TestEveryAccessorOfAProvisionalEpochRefusesOnceItHasBeenDestroyed(t *testing.T) {
	fixture := newEpochProvisionalFixture(t, "refuse-after-destroy")
	methods := epochExportedMethodsOfTheProvisionalValue(t)

	if fixture.value.Destroyed() {
		t.Fatal("a freshly constructed provisional epoch already reports itself destroyed, so the assertion at the end of this case says nothing about the destructor")
	}

	answered := 0
	for i, method := range methods {
		answersError, _ := epochMethodResults(method.Type)
		if !answersError {
			continue
		}
		answered += 1
		bound := reflect.ValueOf(fixture.value).Method(i)
		arguments := []reflect.Value{}
		for j := 0; j < bound.Type().NumIn(); j += 1 {
			arguments = append(arguments, reflect.Zero(bound.Type().In(j)))
		}
		// the DESTROYED refusal specifically, and not any refusal at all. A method called
		// with zero valued arguments may legitimately refuse them -- InstallWraps refuses an
		// empty set and refuses a second install -- and what this half has to rule out is a
		// method that answers G10's sentinel whether or not the destructor has run, which is
		// the shape that would satisfy the half below vacuously.
		if err := epochErrorResultOf(bound.Call(arguments)); errors.Is(err, ErrProvisionalEpochDestroyed) {
			t.Errorf("%s answered ErrProvisionalEpochDestroyed BEFORE the destructor ran, so its refusal afterwards would say nothing about the destructor", method.Name)
		}
	}
	if answered == 0 {
		t.Fatal("no exported method of *ProvisionalEpoch answers an error, so G10's typed refusal has nowhere to be observed")
	}

	fixture.value.Destroy()

	errorType := reflect.TypeOf((*error)(nil)).Elem()
	for i, method := range methods {
		answersError, _ := epochMethodResults(method.Type)
		if !answersError {
			continue
		}
		bound := reflect.ValueOf(fixture.value).Method(i)
		arguments := []reflect.Value{}
		for j := 0; j < bound.Type().NumIn(); j += 1 {
			arguments = append(arguments, reflect.Zero(bound.Type().In(j)))
		}
		results := bound.Call(arguments)
		err := epochErrorResultOf(results)
		if err == nil {
			t.Errorf("%s answered after the destructor ran; G10 is that there is no path that reads it afterwards, and a destroyed value that answered zeros rather than refusing would have a caller seal under thirty two zero octets",
				method.Name)
			continue
		}
		if !errors.Is(err, ErrProvisionalEpochDestroyed) {
			t.Errorf("%s refused with %v, want ErrProvisionalEpochDestroyed; one condition gets one sentinel so a caller matches it with one errors.Is",
				method.Name, err)
		}
		for _, beside := range results {
			if beside.Type() == errorType {
				continue
			}
			// the ZERO value of whatever it answers, rather than a nil SLICE. The
			// narrower reading examined slice results only, so Epoch could hand back
			// the live epoch beside its refusal and nothing noticed -- measured -- while
			// the sentinel's own text is "has been destroyed and answers nothing". A
			// number, a bool or a struct is a thing answered just as much as octets are.
			if !beside.IsZero() {
				t.Errorf("%s answered %v alongside its refusal, and ErrProvisionalEpochDestroyed's own text is that a destroyed value answers nothing",
					method.Name, beside)
			}
		}
	}

	if !fixture.value.Destroyed() {
		t.Error("Destroyed answered false after the destructor ran, so the flag task 15's readers are held to says the opposite of the truth")
	}
}

// Property 4's shape half: no exported method hands back state without a door to refuse through.
//
// This is what stops the rule above being routed around rather than broken. An accessor declared as
// PqSecret() []byte answers no error, so the refusal test skips it, and it would hand a destroyed
// value's bytes to a caller in silence. The rule is read off the RESULT TYPES and enumerates
// nothing: a method answering anything that is neither an error nor the destroyed flag must answer
// an error beside it.
func TestNoExportedAccessorOfAProvisionalEpochAnswersStateWithoutARefusal(t *testing.T) {
	live := newEpochProvisionalFixture(t, "shape-live")
	gone := newEpochProvisionalFixture(t, "shape-destroyed")
	gone.value.Destroy()
	// the second live value holds the octets the DESTRUCTOR leaves, so an exemption granted to a
	// predicate reading erasedness rather than the flag is refused here
	erased := newEpochProvisionalOfErasedOctets(t, "shape-live-erased-octets")
	goneErased := newEpochProvisionalOfErasedOctets(t, "shape-destroyed-erased-octets")
	goneErased.Destroy()
	liveValues := []reflect.Value{reflect.ValueOf(live.value), reflect.ValueOf(erased)}
	destroyedValues := []reflect.Value{reflect.ValueOf(gone.value), reflect.ValueOf(goneErased)}

	// and the live set SPANS the octets the destructor leaves, checked rather than assumed. It
	// is the same reading TestEveryAccessorOfAProvisionalEpochAnswersTheValueItWasBuiltFrom
	// makes of its own fixture: without a live value already holding zeros, the exemption below
	// is decided by one value's content, and every predicate that reads the SECRETS rather than
	// the flag reads false-then-true and is exempted. Measured: with this probe out of the set
	// and nothing else changed, an exported PqSecretLeadsWithAZero() bool passes this case.
	spanning := []string{}
	for at, one := range liveValues {
		value := one.Interface().(*ProvisionalEpoch)
		erasedOctets := true
		for _, method := range epochSliceAnsweringAccessors(t) {
			for _, octets := range epochBytesAnsweredBy(t, value, method) {
				if len(octets) == 0 || !bytes.Equal(octets, make([]byte, len(octets))) {
					erasedOctets = false
				}
			}
		}
		if erasedOctets {
			spanning = append(spanning, fmt.Sprintf("live value %d", at))
		}
	}
	if len(spanning) == 0 {
		t.Fatal("no LIVE value in this case's probe set holds the octets the destructor leaves, so the exemption below cannot tell a bool that reports the destructor from a bool that reports the secrets being zero. A predicate reading a bit of pq_secret is exempted by any set that does not span erased content, and it then answers about a destroyed value with no door to refuse through")
	}

	flags := []string{}
	for _, method := range epochExportedMethodsOfTheProvisionalValue(t) {
		answersError, answersState := epochMethodResults(method.Type)
		if epochIsTheDestroyedFlag(method, liveValues, destroyedValues) {
			flags = append(flags, method.Name)
			continue
		}
		if answersState && !answersError {
			t.Errorf("%s answers state and no error, so it has no way to refuse once the destructor has run and G10's rule has no door to close on it. The one method exempt from this is the destroyed flag itself, and it earns the exemption by reading false while the value is live and true once it has been destroyed -- which this one does not",
				method.Name)
		}
	}
	if len(flags) == 0 {
		t.Error("no exported method of *ProvisionalEpoch reads false while the value is live and true once it has been destroyed, so G10's flag either does not exist or answers the same thing in both states. Task 15 property 6's derived reader class is to be held to that flag, and a constant is not a flag")
	}
	t.Logf("the destroyed flag is %v", flags)
}

// ---------------------------------------------------------------------------
// the two classes above, proved on shapes this type does not have yet
// ---------------------------------------------------------------------------

// A method set *ProvisionalEpoch does not have, so the octet answering class can be proved against
// the shape it will meet next rather than only against the five accessors that exist.
//
// THE HOLE THIS CONTROL CLOSES IS INVISIBLE FROM THE REAL TYPE, which is why the arity narrowing
// survived five reviews and a verification: the type declares exactly one argument-taking method
// and it answers no octets, so the narrowing removed nothing and every rule over it stayed green
// whichever way it was written. A class with no member of the shape it excludes is a class nothing
// can measure, and the answer to that is a shape, not another reading of the same five methods.
//
// Five members, each deciding one reading. An accessor that takes an argument and hands octets back;
// an accessor answering octets in an ARRAY, which a Kind == reflect.Slice reading cannot see; a
// writer that takes an argument and answers only an error, which is out on its results and needs no
// arity rule to exclude it; and two methods answering state no octet can hide in.
//
// THE TWO STATE MEMBERS ARE NAMED ProbeEpoch AND ProbeDestroyed RATHER THAN Epoch AND Destroyed,
// which is not cosmetic and is not this control being tuned. keysource_test.go's
// TestNothingOnTheReproductionsSideOfTheComparisonComesFromTheModule resolves the edges of this
// package's TEST source by bare name and cannot see a receiver, so a test-only method sharing a name
// with a production one makes a selector that used to dangle resolve into somebody else's closure --
// measured: with these named Epoch and Destroyed, that gate reports its exclusion as swallowing
// scope, over an edge that is a name collision and nothing else. Renaming them costs this control
// nothing, because both classes here are decided by results and by behaviour and neither reads a
// name.
type epochArgumentTakingProbe struct {
	pqSecret []byte
}

func (self *epochArgumentTakingProbe) PqSecretFor(purpose string) ([]byte, error) {
	return self.pqSecret, nil
}

func (self *epochArgumentTakingProbe) Fingerprint() ([32]byte, error) {
	return [32]byte{}, nil
}

func (self *epochArgumentTakingProbe) InstallSomething(wraps [][]byte) error {
	return nil
}

func (self *epochArgumentTakingProbe) ProbeEpoch() (uint64, error) {
	return 0, nil
}

func (self *epochArgumentTakingProbe) ProbeDestroyed() bool {
	return false
}

// TestTheOctetAnsweringClassIsReadOffResultsAndNotArity is the fifth instance of this project's
// oldest defect, held.
//
// The defect is deriving a class from the INSTANCE rather than from the PROPERTY, and this is it one
// layer inside the fix for the fourth: epochSliceAnsweringAccessors excluded every method whose
// NumIn is not one and argued that exclusion from the one method that takes an argument today. The
// property is "an exported method that can hand back this value's live octets" and an argument
// takes nothing away from it.
//
// BOTH DIRECTIONS ARE ASSERTED, because a class that admitted everything would satisfy the first
// half and hold nothing: the two members that answer octets are in, the three that cannot are out
// by name, and the real type's own class and complement are pinned beside them so that an accessor
// added by task 15 lands in one of the two lists on the commit that adds it rather than silently
// outside both.
func TestTheOctetAnsweringClassIsReadOffResultsAndNotArity(t *testing.T) {
	probe := reflect.TypeOf(&epochArgumentTakingProbe{})
	methods := []reflect.Method{}
	for i := 0; i < probe.NumMethod(); i += 1 {
		methods = append(methods, probe.Method(i))
	}
	if len(methods) != 5 {
		t.Fatalf("the control declares %d exported methods and this case decides five readings", len(methods))
	}

	answering, excluded := epochOctetAnsweringMethodsIn(methods)
	if want := []string{"Fingerprint", "PqSecretFor"}; !slices.Equal(epochNamesOf(answering), want) {
		t.Errorf("the octet answering class of the control is %v, want %v: PqSecretFor takes an argument and hands octets back, and Fingerprint answers them in an array, so a class narrowed by arity or by the outermost Kind loses one of the two",
			epochNamesOf(answering), want)
	}
	if want := []string{"InstallSomething", "ProbeDestroyed", "ProbeEpoch"}; !slices.Equal(epochExcludedNamesOf(excluded), want) {
		t.Errorf("the control's complement is %v, want %v: a class that admitted these would put a bool and a uint64 through a rule that reads octets out of them",
			epochExcludedNamesOf(excluded), want)
	}

	// and the real type, whose class and complement are the numbers the narrowing was measured
	// by: the arity line it used to open with excluded NOTHING here, which is what an exclusion
	// written for members that do not exist looks like from the outside
	real, realExcluded := epochOctetAnsweringMethodsIn(epochExportedMethodsOfTheProvisionalValue(t))
	if want := []string{"EphRoot", "PqSecret", "StorageRoot", "Wraps", "WriteKey"}; !slices.Equal(epochNamesOf(real), want) {
		t.Errorf("the octet answering class of *ProvisionalEpoch is %v, want %v; if task 15 added an accessor, give it a row in epochAccessorAnswers and name it here",
			epochNamesOf(real), want)
	}
	if want := []string{"Destroy", "Destroyed", "Epoch", "InstallWraps"}; !slices.Equal(epochExcludedNamesOf(realExcluded), want) {
		t.Errorf("the complement of that class is %v, want %v; every member of it is a method no octet can be answered through, and the two rules that hold them are property 4's other halves",
			epochExcludedNamesOf(realExcluded), want)
	}
}

// The names of a class, sorted, so a failure reads as a set rather than as reflection order.
func epochNamesOf(methods []reflect.Method) []string {
	names := []string{}
	for _, method := range methods {
		names = append(names, method.Name)
	}
	slices.Sort(names)
	return names
}

// The names of a complement, which carries each member's signature beside it.
func epochExcludedNamesOf(excluded []string) []string {
	names := []string{}
	for _, one := range excluded {
		names = append(names, one[:strings.Index(one, " ")])
	}
	slices.Sort(names)
	return names
}

// A bool answering type carrying the exact accessor the 2026-09-09 verification planted against the
// destroyed flag's exemption, and reproduced.
//
// It is a control and not a stub: the flag's exemption cannot be measured on *ProvisionalEpoch,
// because the only bool that type declares IS the flag, so a reading that exempted every bool and a
// reading that exempted the flag agree on every member the real type has.
//
// Its flag is ProbeDestroyed rather than Destroyed for the reason epochArgumentTakingProbe's members
// are renamed, and the rename also says something worth having said: epochIsTheDestroyedFlag decides
// by WATCHING and not by name, so the control it is proved against must not be recognisable by one.
type epochFlagProbe struct {
	pqSecret  []byte
	destroyed bool
}

func (self *epochFlagProbe) ProbeDestroyed() bool {
	return self.destroyed
}

// The leak, written as the verification wrote it. It reads false while the secret does not lead
// with a zero octet and true once the destructor has dropped the slice, so an exemption watching one
// live value and one destroyed one admits it -- and what it answers on a live value is a bit OF the
// secret rather than a fact about the value's lifecycle.
func (self *epochFlagProbe) PqSecretLeadsWithAZero() bool {
	return len(self.pqSecret) == 0 || self.pqSecret[0] == 0x00
}

// TestTheDestroyedFlagExemptionReadsTheLifecycleAndNotTheSecret closes verify2's LOW.
//
// The exemption is the one door property 4's shape half leaves open, so what earns it decides what
// can answer about a destroyed value with no refusal. Watching one live value and one destroyed one
// asks whether a bool DIFFERS between two values, and the destructor changes two things at once --
// the flag, and the four secrets, which become zeros. The live set therefore holds a value whose
// secrets are already zeros.
//
// THE THIRD ASSERTION IS WHAT STOPS THIS CASE PASSING VACUOUSLY. Against the non-zero live value
// alone the leak IS exempted, so the case fails if the probe set stops spanning the erased content
// -- which is the edit that would put the hole back.
func TestTheDestroyedFlagExemptionReadsTheLifecycleAndNotTheSecret(t *testing.T) {
	live := []reflect.Value{
		reflect.ValueOf(&epochFlagProbe{pqSecret: epochSecretFilled(0x44)}),
		reflect.ValueOf(&epochFlagProbe{pqSecret: make([]byte, PqSecretBytes)}),
	}
	destroyed := []reflect.Value{
		reflect.ValueOf(&epochFlagProbe{destroyed: true}),
		reflect.ValueOf(&epochFlagProbe{destroyed: true}),
	}
	probe := reflect.TypeOf(&epochFlagProbe{})

	flag, declared := probe.MethodByName("ProbeDestroyed")
	if !declared {
		t.Fatal("the control declares no ProbeDestroyed, so this case decided nothing")
	}
	if !epochIsTheDestroyedFlag(flag, live, destroyed) {
		t.Error("the control's own destroyed flag is not recognised as one, so the exemption has been narrowed until it exempts nothing and the real flag would fail property 4's shape half")
	}

	leak, declared := probe.MethodByName("PqSecretLeadsWithAZero")
	if !declared {
		t.Fatal("the control declares no PqSecretLeadsWithAZero, so this case decided nothing")
	}
	if epochIsTheDestroyedFlag(leak, live, destroyed) {
		t.Error("a bool answering a bit OF pq_secret is exempted from property 4's shape half, so it may answer about a destroyed value with no door to refuse through. The flag reports the DESTRUCTOR, and a predicate that reads the secrets rather than the flag reads true on a live value that already holds zeros")
	}
	if !epochIsTheDestroyedFlag(leak, live[:1], destroyed) {
		t.Error("the leak is refused the exemption even against a live set that does not span the erased content, so the assertion above passes for some other reason and this case observes nothing about the probe set")
	}
}

// ---------------------------------------------------------------------------
// property 4, ownership half: the answers are the LIVE octets
// ---------------------------------------------------------------------------

// TestEveryAccessorOfAProvisionalEpochHandsBackTheLiveSliceAndNotACopy holds the sentence the type's
// own doc comment calls load-bearing, which nothing held before.
//
// "The accessors hand back the LIVE slice rather than a copy" is not a style note. Task 15's fan out
// builds the device wraps out of these bytes, and if the accessor it reads them through hands back a
// copy, that copy is a second home for pq_secret[n+1] in the committer's own frame that this
// destructor never reaches -- so section 5.12 step 2's "MUST NOT be reused" becomes satisfiable
// again by a caller that did nothing wrong. Measured before this case existed: four separate
// mutations making PqSecret, StorageRoot, WriteKey, EphRoot and Wraps return copies each passed the
// whole messagegroup suite.
//
// It is asserted two independent ways because either alone has a hole. The WRITE THROUGH says the
// answer and the field are the same octets NOW; the ERASURE says an answer taken before the
// destructor is dead after it, which is the property G10 actually promises and the one a future
// accessor that copied on some paths and not others would fail. Both run over the derived class, so
// an accessor added by task 15 is held without this case being edited.
func TestEveryAccessorOfAProvisionalEpochHandsBackTheLiveSliceAndNotACopy(t *testing.T) {
	fixture := newEpochProvisionalFixture(t, "accessors-hand-back-live")
	accessors := epochSliceAnsweringAccessors(t)

	// the write through: the next reader of the same door sees what the last one wrote
	for _, method := range accessors {
		answered := epochBytesAnsweredBy(t, fixture.value, method)
		if len(answered) == 0 {
			t.Fatalf("%s answered no octets at all on a live value, so nothing here reads anything", method.Name)
		}
		if len(answered[0]) == 0 {
			t.Fatalf("%s answered an empty slice on a live value", method.Name)
		}
		answered[0][0] ^= 0xff
		again := epochBytesAnsweredBy(t, fixture.value, method)
		if len(again) != len(answered) {
			t.Fatalf("%s answered %d values and then %d", method.Name, len(answered), len(again))
		}
		if !bytes.Equal(again[0], answered[0]) {
			t.Errorf("a write through %s's answer is invisible to the next call of it -- it answered %x and now answers %x -- so it hands back a COPY. The destructor erases the fields, and a caller holding a copy holds an epoch's key material this type's whole promise says has stopped existing",
				method.Name, answered[0], again[0])
		}
	}

	// the erasure: what was handed out before the destructor is dead after it
	held := map[string][][]byte{}
	for _, method := range accessors {
		answered := epochBytesAnsweredBy(t, fixture.value, method)
		for i, one := range answered {
			if bytes.Equal(one, make([]byte, len(one))) {
				t.Fatalf("%s answered %d zero octets at position %d BEFORE the destructor ran, so the erasure check below would pass against any implementation at all",
					method.Name, len(one), i)
			}
		}
		held[method.Name] = answered
	}

	fixture.value.Destroy()

	for _, name := range slices.Sorted(maps.Keys(held)) {
		for i, one := range held[name] {
			for at, octet := range one {
				if octet == 0 {
					continue
				}
				t.Errorf("the octets %s handed out before the destructor ran are still live afterwards -- position %d of its answer %d is %#02x -- so that accessor handed back a COPY. Task 15's fan out would hold pq_secret[n+1] in a buffer G10's destructor never sees",
					name, at, i, octet)
				break
			}
		}
	}
}

// ---------------------------------------------------------------------------
// the constructor's four indistinguishable secrets
// ---------------------------------------------------------------------------

// Which value of section 5.12 step 1 each accessor answers, named by the fixture key the constructor
// was handed it under.
//
// The CLASS is derived -- it is every exported accessor that answers octets, read off the type --
// and only the ANSWERS are written down, which is the shape epochProvisionalFields pins the field
// set with. An accessor with no row is a failure and a row with no accessor is a failure, so a
// reader added by task 15 has to say which of step 1's values it is handing out.
var epochAccessorAnswers = map[string][]string{
	"StorageRoot": {"storage_root"},
	"WriteKey":    {"write_key"},
	"EphRoot":     {"eph_root"},
	"PqSecret":    {"pq_secret"},
	"Wraps":       {"the first wrap", "the second wrap"},
}

// TestEveryAccessorOfAProvisionalEpochAnswersTheValueItWasBuiltFrom closes the transposition.
//
// NewProvisionalEpoch takes four thirty two octet slices in a row, and to the compiler they are one
// type repeated four times: a caller that passes eph_root where storage_root goes builds, runs, and
// derives an entire epoch's class keys off the wrong root. Measured before this case existed: the
// constructor could file storageRoot and ephRoot into each other's fields and the whole suite stayed
// green, because exactly one place in this file compared an accessor's answer to a value handed in.
//
// The fixture's four fills are distinguishable, and that is checked here rather than assumed --
// against four equal fills every row below is satisfied by every value and the case reports clean.
func TestEveryAccessorOfAProvisionalEpochAnswersTheValueItWasBuiltFrom(t *testing.T) {
	fixture := newEpochProvisionalFixture(t, "accessors-answer-their-own")
	for _, one := range slices.Sorted(maps.Keys(fixture.aliases)) {
		for _, other := range slices.Sorted(maps.Keys(fixture.aliases)) {
			if one >= other {
				continue
			}
			if bytes.Equal(fixture.aliases[one], fixture.aliases[other]) {
				t.Fatalf("the fixture handed the same octets to %s and to %s, so a transposition between the two is unobservable and every row below passes vacuously",
					one, other)
			}
		}
	}

	answered := []string{}
	for _, method := range epochSliceAnsweringAccessors(t) {
		rows, isRowed := epochAccessorAnswers[method.Name]
		if !isRowed {
			t.Errorf("%s answers octets and this rule carries no row saying WHICH of section 5.12 step 1's values they are, so a constructor filing it under the wrong field would answer here unchallenged",
				method.Name)
			continue
		}
		answered = append(answered, method.Name)
		got := epochBytesAnsweredBy(t, fixture.value, method)
		if len(got) != len(rows) {
			t.Errorf("%s answered %d values and its row names %d", method.Name, len(got), len(rows))
			continue
		}
		for i, want := range rows {
			expected, isHeld := fixture.aliases[want]
			if !isHeld {
				t.Fatalf("this rule's row for %s names %q and the fixture handed the constructor no such value", method.Name, want)
			}
			if !bytes.Equal(got[i], expected) {
				t.Errorf("%s answered %x at position %d and the fixture handed %s = %x in: the four values of step 1 are one type repeated four times, so a transposition between two of them is a program that builds and a key schedule that is somebody else's",
					method.Name, got[i], i, want, expected)
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(epochAccessorAnswers)) {
		if !slices.Contains(answered, name) {
			t.Errorf("this rule carries a row for %s and *ProvisionalEpoch has no such octet answering accessor, so the pin describes a surface that no longer exists", name)
		}
	}
}

// The reflected surface and the declared surface are the same surface.
//
// Two readings of "the exported methods of this type" that disagree is one rule holding less than
// the one beside it while both report clean, and the narrower one is invisible from inside itself.
func TestTheReflectedAndDeclaredSurfacesOfAProvisionalEpochAgree(t *testing.T) {
	reflected := []string{}
	for _, method := range epochExportedMethodsOfTheProvisionalValue(t) {
		reflected = append(reflected, method.Name)
	}
	slices.Sort(reflected)

	scan := epochScanSources(t, epochOwnScanDir)
	declared := []string{}
	for name, decls := range scan.decls {
		if !ast.IsExported(name) {
			continue
		}
		for _, decl := range decls {
			if decl.Recv == nil || len(decl.Recv.List) == 0 {
				continue
			}
			if epochRendered(scan, decl.Recv.List[0].Type) != "*ProvisionalEpoch" {
				continue
			}
			declared = append(declared, name)
		}
	}
	slices.Sort(declared)
	if !slices.Equal(reflected, declared) {
		t.Errorf("reflection reads %v off *ProvisionalEpoch and the source declares %v", reflected, declared)
	}
}

// ---------------------------------------------------------------------------
// property 5: a lost commit resamples
// ---------------------------------------------------------------------------

// TestLostCommitResamplesPqSecret is the test spec A section 5.9 G10 and section 11.2 name.
//
// Section 5.12 step 2: the committer MUST NOT reuse the pq_secret it sampled, because it was
// encapsulated to a ratchet tree that no longer exists and carrying it into the real epoch n+1 binds
// one PQ secret across two distinct epochs. Task 21 supplies the retry loop; what is assertable here
// is the state that loop is built on, and it is asserted in three ways rather than one, because "two
// random draws differ" is true of a broken implementation too:
//
//   - the destroyed value REFUSES its pq_secret, so a retry has no door to reuse it through;
//   - the bytes it held are GONE, so a retry holding a stale reference gets zeros rather than the
//     old secret;
//   - and the second commit's value is a different secret from the first's.
func TestLostCommitResamplesPqSecret(t *testing.T) {
	engine := newTestEngine(t)
	handle := &epochClearCountingHandle{GroupHandle: engine.createGroup(t, "lost-commit")}
	epoch := handle.Epoch() + 1

	firstSecret, err := NewPqSecret(rand.Reader)
	if err != nil {
		t.Fatalf("NewPqSecret: %v", err)
	}
	firstAsSampled := append([]byte(nil), firstSecret...)
	first, err := NewProvisionalEpoch(handle, epoch,
		epochSecretFilled(0x01), epochSecretFilled(0x02), epochSecretFilled(0x03), firstSecret)
	if err != nil {
		t.Fatalf("NewProvisionalEpoch: %v", err)
	}
	held, err := first.PqSecret()
	if err != nil {
		t.Fatalf("PqSecret before the loss: %v", err)
	}
	if !bytes.Equal(held, firstAsSampled) {
		t.Fatalf("the provisional value holds %x and the sampler drew %x", held, firstAsSampled)
	}

	// the commit is lost
	first.Destroy()

	if _, err := first.PqSecret(); !errors.Is(err, ErrProvisionalEpochDestroyed) {
		t.Errorf("the losing committer's pq_secret is still reachable, err = %v: section 5.12 step 2 forbids reusing it and the refusal is what leaves a retry unable to",
			err)
	}
	if !bytes.Equal(firstSecret, make([]byte, PqSecretBytes)) {
		t.Errorf("the losing committer's pq_secret survives in the array it was drawn into, %x: a retry holding a stale reference would carry one PQ secret across two epochs",
			firstSecret)
	}

	// the retry, at the SAME epoch
	secondSecret, err := NewPqSecret(rand.Reader)
	if err != nil {
		t.Fatalf("NewPqSecret for the retry: %v", err)
	}
	second, err := NewProvisionalEpoch(handle, epoch,
		epochSecretFilled(0x04), epochSecretFilled(0x05), epochSecretFilled(0x06), secondSecret)
	if err != nil {
		t.Fatalf("NewProvisionalEpoch for the retry: %v", err)
	}
	retried, err := second.PqSecret()
	if err != nil {
		t.Fatalf("PqSecret after the retry: %v", err)
	}
	if bytes.Equal(retried, firstAsSampled) {
		t.Errorf("the retry at epoch %d carries the pq_secret the lost commit sampled, %x: MASTER section 7's per epoch PQ independence is exactly this",
			epoch, firstAsSampled)
	}
	if at, err := second.Epoch(); err != nil || at != epoch {
		t.Errorf("the retry's provisional state is for epoch %d (err %v), and the retry is at %d", at, err, epoch)
	}
}

// ---------------------------------------------------------------------------
// property 6: the destructor does not reach the epoch's cached env_key
// ---------------------------------------------------------------------------

// Every field of ProvisionalEpoch, with the item of section 5.12 step 1 it is.
//
// The CLASS is derived -- it is every field the type declares, read off the type rather than listed
// -- and only the ANSWERS are written down, which is the shape imports_test.go pins this package's
// import set with. That is what makes a cached env_key added here fail on the commit that adds it
// whatever it is called: a field with no row is a failure, and so is a row with no field.
var epochProvisionalFields = map[string]string{
	"handle":      "the MLS surface whose staged epoch is step 1's TreeKEM path secrets, and which the destructor calls ClearPendingCommit on from inside",
	"epoch":       "n+1, the epoch this state was built for and which it may never reach",
	"storageRoot": "storage_root[n+1], step 1",
	"writeKey":    "write_key[n+1], step 1",
	"ephRoot":     "eph_root[n+1], step 1",
	"pqSecret":    "pq_secret[n+1], step 1, and step 2's MUST NOT be reused",
	"wraps":       "every X-Wing wrap it built, step 1's last clause, installed write once",
	"destroyed":   "G10's there is no path that reads it afterwards, as a value rather than as a sentence",
}

// TestAProvisionalEpochDeclaresNoFieldAbleToHoldACachedEnvKey is property 6's SHAPE half, and it is
// the half that must fail first.
//
// Spec A section 5.11 makes caching env_key[k] = MLS-Exporter("URmessage/v1/envelope", "", 32) a
// normative obligation, because (*Group).Export reads the current schedule and connect has no
// ExportAt, so the key is computable only while the group stands at epoch k. That cache is NOT
// provisional committer state: it belongs to an epoch that may already be OPEN, and destroying it
// because a LATER commit was rejected would discard the only route into that epoch's storage_root --
// the same shape as ledger open item 134's conforming client hazard, arrived at from the other side.
//
// The behavioural half below says the destructor leaves a cached env_key alone. This says the type
// cannot hold one, and it is what stops the behavioural half being re-broken by somebody who finds
// it convenient to keep the two together.
func TestAProvisionalEpochDeclaresNoFieldAbleToHoldACachedEnvKey(t *testing.T) {
	subject := reflect.TypeOf(ProvisionalEpoch{})
	if subject.NumField() == 0 {
		t.Fatal("ProvisionalEpoch declares no field, so this rule pinned an empty set")
	}
	declared := []string{}
	for i := 0; i < subject.NumField(); i += 1 {
		name := subject.Field(i).Name
		declared = append(declared, name)
		if _, isRowed := epochProvisionalFields[name]; isRowed {
			continue
		}
		t.Errorf("ProvisionalEpoch declares %s of type %s and section 5.12 step 1 has no such item. If it is the cached env_key[k], it may NOT live here: section 5.11 makes the cache a normative obligation for an epoch that may already be open, and G10's destructor would discard the only route into that epoch's storage_root",
			name, subject.Field(i).Type)
	}
	for name := range epochProvisionalFields {
		if !slices.Contains(declared, name) {
			t.Errorf("this rule carries a row for %s and ProvisionalEpoch declares no such field, so the pin describes a type that no longer exists", name)
		}
	}
	t.Logf("%d fields pinned: %v", len(declared), declared)
}

// TestDestroyingAProvisionalEpochLeavesTheEpochsCachedEnvKeyIntact is property 6's behavioural half.
//
// The group is at epoch k and the provisional state is for k+1. A caller that has done what section
// 5.11 tells it to do -- exported and retained env_key[k] while the group stood at k -- must still
// hold it after a rejected k+1 commit is destroyed, and must still be able to recompute it, because
// the epoch it belongs to never moved.
func TestDestroyingAProvisionalEpochLeavesTheEpochsCachedEnvKeyIntact(t *testing.T) {
	fixture := newEpochProvisionalFixture(t, "env-key-intact")
	cached, err := fixture.handle.Export(epochEnvelopeExporterLabel, nil, 32)
	if err != nil {
		t.Fatalf("export env_key at the live epoch: %v", err)
	}
	if len(cached) != 32 {
		t.Fatalf("env_key is %d octets, want 32", len(cached))
	}
	held := append([]byte(nil), cached...)

	fixture.value.Destroy()

	if !bytes.Equal(cached, held) {
		t.Errorf("the destructor reached the cached env_key: it now reads %x and was %x. Section 5.11's cache belongs to an epoch that may already be open, and it is the only route into that epoch's storage_root once the group has moved",
			cached, held)
	}
	again, err := fixture.handle.Export(epochEnvelopeExporterLabel, nil, 32)
	if err != nil {
		t.Fatalf("export env_key after the destructor: %v", err)
	}
	if !bytes.Equal(again, held) {
		t.Errorf("the group's own env_key moved across the destructor, from %x to %x: Destroy drops a STAGED commit and must not move the live epoch",
			held, again)
	}
}

// ---------------------------------------------------------------------------
// the constructor's refusals
// ---------------------------------------------------------------------------

// Every value of section 5.12 step 1 is thirty two octets, and a short one is refused here rather
// than at whichever expansion happens to meet it first.
func TestAProvisionalEpochRefusesAValueThatIsNotThirtyTwoOctets(t *testing.T) {
	engine := newTestEngine(t)
	handle := engine.createGroup(t, "provisional-widths")
	full := func() []byte { return epochSecretFilled(0x77) }
	for _, wrong := range [][]byte{nil, {}, make([]byte, 31), make([]byte, 33), make([]byte, 64)} {
		for position := 0; position < 4; position += 1 {
			values := [][]byte{full(), full(), full(), full()}
			values[position] = wrong
			value, err := NewProvisionalEpoch(handle, 1, values[0], values[1], values[2], values[3])
			if !errors.Is(err, ErrProvisionalEpochValue) {
				t.Errorf("NewProvisionalEpoch with a %d octet value at position %d answered %v, want ErrProvisionalEpochValue",
					len(wrong), position, err)
			}
			if value != nil {
				t.Errorf("NewProvisionalEpoch refused a %d octet value at position %d and answered a state alongside the refusal",
					len(wrong), position)
			}
		}
	}
	if _, err := NewProvisionalEpoch(nil, 1, full(), full(), full(), full()); !errors.Is(err, ErrNilGroupHandle) {
		t.Errorf("NewProvisionalEpoch with no handle answered %v, want ErrNilGroupHandle: the destructor clears the staged commit from inside and a nil handle would panic there rather than here",
			err)
	}
}
