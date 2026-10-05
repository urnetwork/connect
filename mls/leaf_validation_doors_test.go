// The gate over RFC 9420 section 7.3's DOOR CLASS: for every leaf_node_source the RFC defines,
// some production caller of (*LeafNode).Validate states that expectation, or somebody has written
// down why none does.
//
// It is a file of its own because the class it derives spans four of them -- key_package.go states
// key_package, validate_proposals.go states update, treekem.go states commit, tree_sync.go waives
// the rule -- and every earlier attempt to hold this property lived beside one of those and
// described the others from memory. Three rounds of this defect were each found by hand, one door
// at a time, and each time a comment somewhere else in the package went on asserting that the door
// nobody had built was already there.
//
// So nothing here is a list of doors. The SOURCES come from the package's own LeafNodeSource
// constants through the type checker, and the DOORS come from the call sites of the validator
// itself, read off the type checked syntax: which constant each caller names, and which callers
// waive the rule instead. A fourth source, a door deleted, a door whose expectation is not a
// constant this package declares, a context built somewhere the scan cannot read it -- each is a
// failure of this file rather than a sentence that quietly stops being true.
package mls

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// checkedSource is this package's production source, parsed and type checked WITH function
// bodies, plus the declaration each function object came from.
//
// group_context_test.go's typeCheckedPackage deliberately sets IgnoreFuncBodies, because what it
// derives -- registry constants -- is declared at package level and the bodies are most of what
// that check would cost. Everything in this file is about what happens INSIDE bodies: which
// callers call the validator, and which errors the bodies it delegates to can name. So this is a
// second check rather than a widening of that one, memoised for the same reason.
type checkedSource struct {
	pkg     *types.Package
	info    *types.Info
	fileSet *token.FileSet
	files   []*ast.File
	// bodies is every function and method this package declares, keyed by the object the type
	// checker resolves its name to. A call whose callee is absent from this map is a call out of
	// the package -- an interface method, or the standard library -- and is where a walk stops.
	bodies map[types.Object]*ast.FuncDecl
}

var (
	checkedSourceOnce   sync.Once
	checkedSourceCached checkedSource
	checkedSourceErr    error
)

// typeCheckedPackageWithBodies type checks this package's production source once per process.
//
// A check error is fatal rather than tolerated, for checkPackageSource's reason one file over: a
// partial package is a partial scope and a partial set of resolved calls, which is a derivation
// that silently reaches less than the source does -- the failure this whole approach exists to
// remove, reintroduced one layer down.
func typeCheckedPackageWithBodies(t *testing.T) checkedSource {
	t.Helper()
	checkedSourceOnce.Do(func() {
		checkedSourceCached, checkedSourceErr = checkPackageSourceWithBodies(".",
			"github.com/urnetwork/connect/mls")
	})
	if checkedSourceErr != nil {
		t.Fatalf("type check this package's production source with its bodies: %v", checkedSourceErr)
	}
	if len(checkedSourceCached.files) == 0 {
		t.Fatal("no production go file was read, so every derivation in this file proves nothing")
	}
	return checkedSourceCached
}

func checkPackageSourceWithBodies(directory string, packagePath string) (checkedSource, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return checkedSource{}, err
	}
	fileSet := token.NewFileSet()
	files := []*ast.File{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fileSet, filepath.Join(directory, name), nil,
			parser.SkipObjectResolution)
		if err != nil {
			return checkedSource{}, err
		}
		files = append(files, parsed)
	}
	info := &types.Info{
		Defs: map[*ast.Ident]types.Object{},
		Uses: map[*ast.Ident]types.Object{},
		// the TYPE of every expression, which index_pairing_test.go's class turns on: a loop that
		// indexes a map by a value taken from the other sequence pairs no positions, and only the
		// type says which of the two a name is. Recorded here rather than in a fourth check of this
		// package, for the reason this one is memoised at all.
		Types: map[ast.Expr]types.TypeAndValue{},
	}
	config := types.Config{Importer: importer.ForCompiler(fileSet, "source", nil)}
	pkg, err := config.Check(packagePath, fileSet, files, info)
	if err != nil {
		return checkedSource{}, err
	}
	source := checkedSource{pkg: pkg, info: info, fileSet: fileSet, files: files,
		bodies: map[types.Object]*ast.FuncDecl{}}
	for _, file := range files {
		for _, declaration := range file.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil {
				continue
			}
			if object := info.Defs[function.Name]; object != nil {
				source.bodies[object] = function
			}
		}
	}
	return source, nil
}

// at renders one node's position as file:line, so a failure below names the call site a reader has
// to open rather than only the property it broke.
func (self checkedSource) at(node ast.Node) string {
	position := self.fileSet.Position(node.Pos())
	return filepath.Base(position.Filename) + ":" + decimal(position.Line)
}

// fileOf is the base name of the file one node is written in.
func (self checkedSource) fileOf(node ast.Node) string {
	return filepath.Base(self.fileSet.Position(node.Pos()).Filename)
}

// decimal renders a line number without pulling strconv into this file.
func decimal(value int) string {
	if value == 0 {
		return "0"
	}
	digits := []byte{}
	for value > 0 {
		digits = append([]byte{byte(48 + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

// declarationNamed answers the object of one function or method declaration, or fails.
//
// Absence is fatal rather than clean, for parsedSource.declarationOf's reason: a gate that stopped
// finding its subject must fail, not report the subject it never read as compliant.
func (self checkedSource) declarationNamed(t *testing.T, receiver string, name string) types.Object {
	t.Helper()
	for object, function := range self.bodies {
		if function.Name.Name == name && renderReceiver(function) == receiver {
			return object
		}
	}
	t.Fatalf("this package declares no %s %s, so the derivation below has no subject", receiver, name)
	return nil
}

// renderReceiver is the receiver type of a declaration as it is written, or the empty string for a
// plain function. It handles the two spellings this package uses and nothing else: a receiver
// shape no gate has been written against must not be silently rendered as some other one.
func renderReceiver(function *ast.FuncDecl) string {
	if function.Recv == nil || len(function.Recv.List) != 1 {
		return ""
	}
	switch declared := function.Recv.List[0].Type.(type) {
	case *ast.StarExpr:
		if named, isNamed := declared.X.(*ast.Ident); isNamed {
			return "*" + named.Name
		}
	case *ast.Ident:
		return declared.Name
	}
	return ""
}

// declarationName is how one member of a derived closure is reported.
func declarationName(function *ast.FuncDecl) string {
	if receiver := renderReceiver(function); receiver != "" {
		return receiver + "." + function.Name.Name
	}
	return function.Name.Name
}

// ---------------------------------------------------------------------------
// the door class, read off the call sites of the validator
// ---------------------------------------------------------------------------

// leafValidationContextType is the structure a caller of (*LeafNode).Validate fills in, spelled
// once so the two scans below and the struct cannot drift apart.
const leafValidationContextType = "LeafValidationContext"

// leafValidationExpectedSourceField and leafValidationWaiverField are the two fields of that
// structure that decide section 7.3's leaf_node_source rule for a call site. A caller sets exactly
// one of them; see (*LeafNode).Validate, which refuses a context that sets both.
const (
	leafValidationExpectedSourceField = "ExpectedSource"
	leafValidationWaiverField         = "SourceIsNotJudgedHere"
)

// leafValidationDoor is one production call of (*LeafNode).Validate and what its context says
// about the leaf_node_source rule.
type leafValidationDoor struct {
	// file is the base name of the file the call is written in, and position is file:line.
	file     string
	position string
	// callSite is the file and the DECLARATION the call is written in, which is what the waiver
	// table below is keyed by. See leafValidationPositionsThatDoNotJudgeTheSource: keyed by file
	// alone, a second waiving call inside an already-admitted file was admitted with no reason of
	// its own, which is the base-name-exemption shape this project has now shipped four times.
	// It is not the position, because a line number moves whenever anything above it is edited
	// and a waiver that expired on an unrelated commit would be re-approved without being read.
	callSite string
	// expects is the LeafNodeSource constant this position names, empty for a waiver.
	expects string
	// waives is a position that states no expectation at all.
	waives bool
}

// leafValidationDoorScan is one pass over the production source: every call of
// (*LeafNode).Validate classified, and every LeafValidationContext literal the package builds.
//
// The second half is what stops the first from being evaded rather than broken. A door whose
// context was assembled in a variable, or in a helper, and then handed over is a door this scan
// reads nothing out of -- and a scan that quietly skipped it would report the same clean bill a
// complete one reports, which is this gate's own failure mode stated one level up.
type leafValidationDoorScan struct {
	source   checkedSource
	doors    []leafValidationDoor
	literals []*ast.CompositeLit
	consumed map[*ast.CompositeLit]bool
}

func scanLeafValidationDoors(t *testing.T) leafValidationDoorScan {
	t.Helper()
	source := typeCheckedPackageWithBodies(t)
	validate := source.declarationNamed(t, "*LeafNode", "Validate")
	scan := leafValidationDoorScan{source: source, consumed: map[*ast.CompositeLit]bool{}}
	for _, file := range source.files {
		// the literals over the WHOLE file, because the second half of this derivation is about
		// contexts built anywhere at all -- including in a package level initializer, which is
		// not inside any declaration the walk below visits
		ast.Inspect(file, func(node ast.Node) bool {
			literal, isLiteral := node.(*ast.CompositeLit)
			if !isLiteral {
				return true
			}
			if named, isNamed := literal.Type.(*ast.Ident); isNamed &&
				named.Name == leafValidationContextType {
				scan.literals = append(scan.literals, literal)
			}
			return true
		})
		// the CALLS per declaration, so each door knows the call site it is written in. A walk
		// over the whole file cannot say that, and the waiver table has to be keyed by it: keyed
		// by file, a second waiving call in an already-admitted file is admitted with no reason
		// of its own.
		for _, declaration := range file.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil {
				// a call of the validator outside any function body -- in a var initializer,
				// say -- is one no call site names, so the waiver table could not key it.
				// Refused rather than skipped: a scan that walked past it would report the
				// clean bill a complete one reports.
				ast.Inspect(declaration, func(node ast.Node) bool {
					if leafValidationCallOf(source, validate, node) != nil {
						t.Errorf("(*LeafNode).Validate is called at %s, which is not inside a function declaration; this gate keys a door by the declaration it is written in and has nothing to key that one by",
							source.at(node))
					}
					return true
				})
				continue
			}
			site := source.fileOf(function) + " " + declarationName(function)
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call := leafValidationCallOf(source, validate, node)
				if call == nil {
					return true
				}
				literal := leafValidationContextLiteralOf(t, source, call)
				if literal == nil {
					return true
				}
				scan.consumed[literal] = true
				if door, readable := classifyLeafValidationDoor(t, source, literal); readable {
					door.callSite = site
					scan.doors = append(scan.doors, door)
				}
				return true
			})
		}
	}
	return scan
}

// leafValidationCallOf answers the node as a call of the validator, or nil.
func leafValidationCallOf(source checkedSource, validate types.Object, node ast.Node) *ast.CallExpr {
	call, isCall := node.(*ast.CallExpr)
	if !isCall {
		return nil
	}
	selector, isSelector := call.Fun.(*ast.SelectorExpr)
	if !isSelector || source.info.Uses[selector.Sel] != validate {
		return nil
	}
	return call
}

// leafValidationContextLiteralOf is the context a call of the validator states, or nil with the
// failure already reported.
func leafValidationContextLiteralOf(t *testing.T, source checkedSource,
	call *ast.CallExpr) *ast.CompositeLit {
	t.Helper()
	if len(call.Args) == 1 {
		argument := call.Args[0]
		if pointer, isPointer := argument.(*ast.UnaryExpr); isPointer {
			argument = pointer.X
		}
		if literal, isLiteral := argument.(*ast.CompositeLit); isLiteral {
			if named, isNamed := literal.Type.(*ast.Ident); isNamed &&
				named.Name == leafValidationContextType {
				return literal
			}
		}
	}
	t.Errorf("the call of (*LeafNode).Validate at %s does not build its %s at the call, so this gate cannot read which leaf_node_source it expects. A door whose expectation nothing can read is a door nothing holds: build the context at the call site, or teach this scan the shape you used",
		source.at(call), leafValidationContextType)
	return nil
}

// classifyLeafValidationDoor reads one context literal for what it says about the
// leaf_node_source rule: the constant it expects, or the waiver it takes.
//
// An ExpectedSource that is not a LeafNodeSource CONSTANT is refused rather than recorded, and
// that clause is the whole reason this scan reads the value and not merely the key. tree_sync.go
// spelled its waiver `ExpectedSource: leaf.LeafNodeSource` -- the expectation taken from the very
// leaf being judged -- and under that spelling Validate compares a value against itself, so
// ErrLeafNodeSourceMismatch could not fire from that position for any input at all. It read like a
// door, it counted as a door to anybody grepping for the field, and it was a check that reported
// clean having compared nothing.
func classifyLeafValidationDoor(t *testing.T, source checkedSource,
	literal *ast.CompositeLit) (leafValidationDoor, bool) {
	t.Helper()
	door := leafValidationDoor{file: source.fileOf(literal), position: source.at(literal)}
	var expected ast.Expr
	waived := false
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
		case leafValidationExpectedSourceField:
			expected = pair.Value
		case leafValidationWaiverField:
			waived = true
			if named, isNamed := pair.Value.(*ast.Ident); !isNamed || named.Name != "true" {
				t.Errorf("the context at %s sets %s to something other than true; the waiver is a decision a reader has to be able to see at the call site",
					door.position, leafValidationWaiverField)
			}
		}
	}
	switch {
	case expected == nil && !waived:
		t.Errorf("the call of (*LeafNode).Validate at %s states neither %s nor %s. Section 7.3 has a leaf_node_source rule for every position, and a position that has decided it owes none has to say so",
			door.position, leafValidationExpectedSourceField, leafValidationWaiverField)
		return door, false
	case expected != nil && waived:
		t.Errorf("the call of (*LeafNode).Validate at %s both waives the leaf_node_source rule and states an expectation for it; Validate refuses that context at run time and this says so at the call site",
			door.position)
		return door, false
	case waived:
		door.waives = true
		return door, true
	}
	named, isNamed := expected.(*ast.Ident)
	if !isNamed {
		t.Errorf("the call of (*LeafNode).Validate at %s passes an %s this gate cannot read as a constant. An expectation computed from the leaf being judged makes the comparison inside Validate x != x, so ErrLeafNodeSourceMismatch cannot fire from that position for ANY input -- which is the shape tree_sync.go carried, and is a check that reports clean having compared nothing. A position with no expectation sets %s instead",
			door.position, leafValidationExpectedSourceField, leafValidationWaiverField)
		return door, false
	}
	constant, isConstant := source.info.Uses[named].(*types.Const)
	if !isConstant || constant.Parent() != source.pkg.Scope() {
		t.Errorf("the call of (*LeafNode).Validate at %s expects %s, which this package does not declare as a package level constant; the door class is derived off the constants and a name outside them is not in it",
			door.position, named.Name)
		return door, false
	}
	door.expects = named.Name
	return door, true
}

// leafValidationSourcesWithNoDoorYet is every LeafNodeSource constant no production call of
// (*LeafNode).Validate expects, with the reason.
//
// IT IS EMPTY, and it is kept declared rather than deleted because the empty map is the claim:
// section 7.3 states a rule for all three sources and all three are now stated by a caller. It
// held LeafNodeSourceCommit until the commit door landed -- an admitted gap rather than a decision,
// written so that the commit closing it would fail this gate rather than leave a stale excuse
// behind, which is exactly what happened. A fourth source added to the enum with no door lands in
// the branch below that has no entry here, and the failure names it.
var leafValidationSourcesWithNoDoorYet = map[string]string{}

// leafValidationPositionsThatDoNotJudgeTheSource is every production CALL SITE of
// (*LeafNode).Validate that WAIVES section 7.3's leaf_node_source rule, with the reason.
//
// A waiver is a decision and this is where it is written down, in both directions: a call site
// that waives and is not named here fails, and a name here that no call site waives fails too. The
// point is that switching a real door off has to be a visible edit to this file rather than one
// extra field in a struct literal nobody diffs.
//
// KEYED BY THE CALL SITE AND NOT BY THE FILE, which is the whole of what this map's header used to
// claim and did not do. `waivers[door.file] = door.position` is last-write-wins, so a SECOND
// waiving call inside an already-admitted file was admitted with no reason of its own -- measured:
// a second waiving call in tree_sync.go ran green while the identical call in
// validate_proposals.go failed immediately. That is the fourth instance of the
// base-name-exemption shape on this project and the first inside the gate written to close that
// class: the join gate that exempted a file by base name, the leaf-validation rule applied at
// element zero, and the ceiling table that enumerated its scope are the other three.
//
// The key is the file plus the DECLARATION and not the file plus the LINE, because a line number
// moves whenever anything above it is edited: a waiver keyed on one would expire on an unrelated
// commit and be re-approved by whoever was fixing the build rather than read. A declaration
// holding two waiving calls is refused outright below, so the key cannot silently cover two.
var leafValidationPositionsThatDoNotJudgeTheSource = map[string]string{
	"tree_sync.go *RatchetTree.validateLeaves": "sweeps a settled tree, which legally holds all three sources at once -- key_package under a member added and not yet committed over, update under one that refreshed itself, commit under whoever last committed a path -- so there is no single source a whole tree sweep could demand. Its own header names the two doors that owe the per position rule instead, and every other clause of section 7.3 still runs at every leaf.",
}

// TestEveryLeafNodeSourceEitherHasAValidationDoorOrAnAdmittedGap derives both halves and holds
// them against each other.
//
// The class of sources is the package's own LeafNodeSource constants, so a fourth source declared
// later is swept on the commit that declares it. The class of doors is the CALL SITES of the
// validator, so a door deleted, a door whose expectation is not a constant, and a context this
// gate cannot read are three separate failures rather than three ways of reporting nothing.
// Neither class is a list. The two lists here -- the admitted gap and the admitted waivers -- are
// each required to name something that exists and to name something the derivation did not
// otherwise account for, so both expire by FAILING rather than by somebody remembering them.
func TestEveryLeafNodeSourceEitherHasAValidationDoorOrAnAdmittedGap(t *testing.T) {
	declared := registryConstantsOfType(t, "LeafNodeSource")
	scan := scanLeafValidationDoors(t)
	doors := map[string]string{}
	waivers := map[string][]string{}
	for _, door := range scan.doors {
		if door.waives {
			waivers[door.callSite] = append(waivers[door.callSite], door.position)
			continue
		}
		if first, twice := doors[door.expects]; twice {
			t.Logf("%s is stated at %s and again at %s", door.expects, first, door.position)
			continue
		}
		doors[door.expects] = door.position
	}
	// the positive control: a scan that resolved nothing reports exactly the clean bill a
	// complete one reports, and key_package.go certainly states its own expectation
	if doors["LeafNodeSourceKeyPackage"] == "" {
		t.Fatalf("the scan found the doors %v and the waivers %v, and key_package.go certainly expects LeafNodeSourceKeyPackage, so it is reading something other than this package",
			doors, waivers)
	}
	for _, name := range slices.Sorted(maps.Keys(declared)) {
		door, hasDoor := doors[name]
		reason, admitted := leafValidationSourcesWithNoDoorYet[name]
		switch {
		case hasDoor && admitted:
			t.Errorf("%s is expected at the door in %s and is also written down as having none, with the reason %q; one of the two is stale",
				name, door, reason)
		case !hasDoor && !admitted:
			t.Errorf("no production caller of (*LeafNode).Validate ever expects %s of a leaf, and nothing is written down about it. RFC 9420 section 7.3 states a rule for every source, and a source nothing expects is a leaf installed with no signature check, no leaf_node_source check and no section 13.4 check -- which is the defect the update door and then the commit door each closed",
				name)
		case hasDoor:
			t.Logf("%s is stated at %s", name, door)
		default:
			t.Logf("%s has no door: %s", name, reason)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(doors)) {
		if _, isConstant := declared[name]; !isConstant {
			t.Errorf("%s expects %s of a leaf and this package declares no LeafNodeSource constant of that name",
				doors[name], name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(leafValidationSourcesWithNoDoorYet)) {
		if _, isConstant := declared[name]; !isConstant {
			t.Errorf("the admitted gap names %s and this package declares no LeafNodeSource constant of that name",
				name)
		}
	}
	leafValidationWaiversAreAdmitted(t, waivers)
	leafValidationContextsAreAllBuiltAtADoor(t, scan)
}

// leafValidationWaiversAreAdmitted holds the waiving call sites against the written reasons in
// both directions.
//
// THREE conditions and not two, and the third is the one the file-keyed version had no way to
// state: a call site that waives with no reason fails, a reason for a call site that no longer
// waives fails, and a DECLARATION holding more than one waiving call fails. Without the third the
// key still covers two decisions with one reason -- the same last-write-wins hole one level in --
// and the remedy is to split the second call out or to widen the reason on purpose.
func leafValidationWaiversAreAdmitted(t *testing.T, waivers map[string][]string) {
	t.Helper()
	for _, site := range slices.Sorted(maps.Keys(waivers)) {
		at := waivers[site]
		reason, admitted := leafValidationPositionsThatDoNotJudgeTheSource[site]
		if !admitted {
			t.Errorf("the call of (*LeafNode).Validate at %v waives section 7.3's leaf_node_source rule and no reason is written down for %s. A position that judges no source accepts a key_package leaf, lifetime and all, exactly where a commit leaf belongs -- so the waiver is a decision, and it is made here",
				at, site)
			continue
		}
		if len(at) > 1 {
			t.Errorf("%s holds %d waiving calls of (*LeafNode).Validate, at %v, and one reason is written down for it. One reason covering two decisions is the last-write-wins hole this map was re-keyed to close: split the second call into a declaration of its own, or say in the reason why both waive",
				site, len(at), at)
			continue
		}
		t.Logf("%s waives the source rule at %s: %s", site, at[0], reason)
	}
	for _, site := range slices.Sorted(maps.Keys(leafValidationPositionsThatDoNotJudgeTheSource)) {
		if _, waives := waivers[site]; !waives {
			t.Errorf("a waiver is written down for %s and no call of (*LeafNode).Validate there waives the source rule; the reason has outlived what it excused",
				site)
		}
	}
}

// leafValidationContextsAreAllBuiltAtADoor is the second half of the derivation, and it is what
// stops the first from being evaded rather than broken.
//
// Every LeafValidationContext the production source builds is built AT a call of the validator, so
// the classification above sees all of them. A context assembled in a variable, or handed back by
// a helper, is one this scan reads nothing out of -- and a scan that skipped it silently would
// report the same clean bill a complete one reports. That is this gate's own failure mode, and it
// is the failure mode of every hand written class this project has replaced.
func leafValidationContextsAreAllBuiltAtADoor(t *testing.T, scan leafValidationDoorScan) {
	t.Helper()
	if len(scan.literals) == 0 {
		t.Fatalf("the scan found no %s literal in this package's production source, so it read something other than this package",
			leafValidationContextType)
	}
	for _, literal := range scan.literals {
		if scan.consumed[literal] {
			continue
		}
		t.Errorf("the %s built at %s is not the argument of a call of (*LeafNode).Validate, so the door class above never classified it. Build it at the call, or this gate cannot tell a door from a structure somebody filled in",
			leafValidationContextType, scan.source.at(literal))
	}
}

// ---------------------------------------------------------------------------
// the refusal class, derived through the validator's delegates rather than named
// ---------------------------------------------------------------------------

// leafValidationReach is the transitive closure of the calls (*LeafNode).Validate makes, within
// this package: every declaration whose body can decide what that validator answers.
//
// A CLOSURE and not a list of names, and that correction is the whole reason this exists. The
// version it replaces bounded the class with `[]string{"Validate", "VerifySignature",
// "validateLifetime"}` -- three bodies typed out beside a comment claiming they were "every
// refusal (*LeafNode).Validate can answer" -- while Validate delegates to five. Two refusals
// escaped it and both were then measured reachable through a real door with real inputs:
// errProfileCredentialType, which Credential.MarshalMLS raises from inside the signature
// preimage, and errMissingRequiredCapability, which is Capabilities.Supports' own value.
//
// The walk stops at any callee this package does not declare -- an interface method, the standard
// library -- because there is no body to read. That is the conservative direction: what a
// CryptoProvider answers is the provider's business, and the gate that consumes this class asks
// only about refusals this package can name.
func leafValidationReach(t *testing.T) map[types.Object]*ast.FuncDecl {
	t.Helper()
	source := typeCheckedPackageWithBodies(t)
	root := source.declarationNamed(t, "*LeafNode", "Validate")
	reached := map[types.Object]*ast.FuncDecl{}
	var walk func(object types.Object)
	walk = func(object types.Object) {
		declaration, declared := source.bodies[object]
		if !declared || reached[object] != nil {
			return
		}
		reached[object] = declaration
		ast.Inspect(declaration.Body, func(node ast.Node) bool {
			identifier, isIdentifier := node.(*ast.Ident)
			if !isIdentifier {
				return true
			}
			if called, isFunction := source.info.Uses[identifier].(*types.Func); isFunction {
				walk(called)
			}
			return true
		})
	}
	walk(root)
	return reached
}

// leafValidationRefusalSites answers every package level error value the closure NAMES, mapped to
// the declarations that name it.
//
// An identifier is a refusal when the package scope resolves it to a value of type error, which is
// the shape every sentinel of this package has. That is the derivation and not a prefix match:
// several members are unexported and two of those carry no Err prefix at all.
func leafValidationRefusalSites(t *testing.T) map[string][]string {
	t.Helper()
	source := typeCheckedPackageWithBodies(t)
	found := map[string]map[string]bool{}
	for _, declaration := range leafValidationReach(t) {
		ast.Inspect(declaration.Body, func(node ast.Node) bool {
			identifier, isIdentifier := node.(*ast.Ident)
			if !isIdentifier {
				return true
			}
			named, isValue := source.info.Uses[identifier].(*types.Var)
			if !isValue || named.Parent() != source.pkg.Scope() ||
				named.Type().String() != "error" {
				return true
			}
			if found[identifier.Name] == nil {
				found[identifier.Name] = map[string]bool{}
			}
			found[identifier.Name][source.fileOf(declaration)+" "+declarationName(declaration)] = true
			return true
		})
	}
	sites := map[string][]string{}
	for name, where := range found {
		sites[name] = slices.Sorted(maps.Keys(where))
	}
	return sites
}

// leafValidationRefusalNames is that class as a sorted list of names.
func leafValidationRefusalNames(t *testing.T) []string {
	t.Helper()
	return slices.Sorted(maps.Keys(leafValidationRefusalSites(t)))
}

// TestTheLeafValidatorsRefusalClassReachesPastTheFileItIsDeclaredIn is the control on the closure
// above, and it states the defect it replaced rather than describing it.
//
// The class was bounded by three method names, all declared in leaf_node.go, under a comment
// calling them "every refusal (*LeafNode).Validate can answer". So the property that separates the
// two derivations is this: some refusal of the class is named ONLY in a body declared in another
// file, which a scan of leaf_node.go cannot see however carefully it is written. Both halves are
// derived -- the class, and which of its members escape the file -- so this fails if the closure
// ever silently narrows back to one file's declarations.
func TestTheLeafValidatorsRefusalClassReachesPastTheFileItIsDeclaredIn(t *testing.T) {
	source := typeCheckedPackageWithBodies(t)
	root := source.declarationNamed(t, "*LeafNode", "Validate")
	reach := leafValidationReach(t)
	if reach[root] == nil {
		t.Fatal("the closure does not contain (*LeafNode).Validate itself, so it walked something other than the validator")
	}
	sites := leafValidationRefusalSites(t)
	// the positive control: a scan that resolved nothing reports the clean bill a complete one
	// reports, and Validate certainly names the source rule's own value
	if len(sites["ErrLeafNodeSourceMismatch"]) == 0 {
		t.Fatalf("the closure named the refusals %v, and (*LeafNode).Validate certainly names ErrLeafNodeSourceMismatch, so it is reading something other than the validator",
			slices.Sorted(maps.Keys(sites)))
	}
	declaredIn := "leaf_node.go"
	escaped := []string{}
	for _, name := range slices.Sorted(maps.Keys(sites)) {
		outside := true
		for _, site := range sites[name] {
			if strings.HasPrefix(site, declaredIn+" ") {
				outside = false
				break
			}
		}
		if outside {
			escaped = append(escaped, name+" ("+strings.Join(sites[name], ", ")+")")
		}
	}
	if len(escaped) == 0 {
		t.Fatalf("every refusal of the derived class is named somewhere in %s, so this derivation and a scan bounded to that one file would agree -- and the class this replaced was exactly such a scan, which understated it by two",
			declaredIn)
	}
	t.Logf("the closure reads %d declarations and %d refusals; %d of them are named only outside %s: %v",
		len(reach), len(sites), len(escaped), declaredIn, escaped)
}
