// Guardrail 8's last shape, and the framing structure's committed fuzz seed corpus.
//
// Spec A 5.9 G8 asks that every comparison of a tag go through
// crypto/subtle.ConstantTimeCompare, reached by CryptoProvider.MacVerify. Four gates already
// answer parts of that question, and this file deliberately answers none of them again:
//
//   - TestNothingThisPackageShipsComparesDataOutsideConstantTime derives eighteen comparators
//     from the packages this one imports and bans every CALL to one, in every production file,
//     package level initializers included. That is the half that reads every call site.
//   - TestEveryKeyQuestionOverTheRatchetTreeIsAnsweredInConstantTime holds the declarations that
//     hold a key and answer over a ratchet tree, with an equality rule, a foreign call rule and
//     a reachability rule.
//   - TestEveryTagVerifierComparesThroughMacVerifyAndNothingElse holds the exported bools that
//     reach into the epoch's nine secrets.
//   - TestEveryMembershipTagRefusalIsDecidedByMacVerifyAndNothingElse holds everything that can
//     answer errBadMembershipTag, the shape of the refusal included.
//
// What is left is the shape the FIRST of those names out loud in its own header and does not
// read: "a comparison written as a byte loop in this package's own source names no comparator
// and is in no class derived from imports ... A gate that reads every call is not a gate that
// reads every comparison, and the difference is one hand written loop." The three narrower gates
// close that loop for three CLASSES -- a key plus a tree, an epoch secret plus a bool, an
// errBadMembershipTag -- and the whole framing surface is outside all three but for the one
// membership tag chain.
//
// Measured rather than argued. framing_protect.go's CheckFramedContentContext decides ValSem002
// with subtle.ConstantTimeCompare over a group id, it is in none of those three classes, and
// rewriting that comparison as string(content.GroupId) != string(groupId) leaves every test of
// ./mls/... and ./message/... passing.
//
// So this is the fourth reading of the same guardrail and the first over the shape that names
// nothing: a comparison of octets spelled as ordinary go equality. It is package wide and over
// both roots the forbidden call scan reads, because a comparison of octets is no more allowed in
// ../message than it is here, and it is TYPE DIRECTED rather than textual -- an operand is
// judged by what the compiler says it is, so a tag behind three named types is in the class and
// a comparison of two lengths is not.
//
// It is also PROVENANCE directed, which is the second thing measurement demanded. A rule reading
// one expression sees `a[at] != b[at]` and does not see the same loop with the two octets copied
// into locals first, or delegated to a helper taking two bytes, because either way the comparison
// then holds two identifiers of type byte and a byte is not an octet string. Both were measured
// surviving the whole suite. octetsReadAtAnOffset follows an octet across a rename, an argument
// and a return instead, and stops at a fold, which is the line between the byte loop and
// framing_protect.go's sanctioned padding check.
//
// Three counts are taken rather than one, because each of the first two was measured passing over
// nothing. The FILE count says every production file of the directory was opened. The DECLARATION
// count, taken against a second parse of those same paths, says something was read inside them --
// blanking the loaded files after the type check left this gate green while it logged 39 files
// read and judged zero declarations. And the reports are a SLICE rather than a map keyed by the
// declaration's name, because keyed by name 81 of this package's 377 declarations were overwritten
// by the last file parsed, including both codec halves of every structure in framing.go.
//
// The plan's own version of this test was seven file names and a strings.Contains over three
// comparator spellings. That is strictly weaker than the gate next door in every dimension it
// has -- three comparators against eighteen, seven files against every one, substring against
// syntax -- and there is no edit it could fail for that the existing gate would not already have
// caught. It is replaced rather than added.
//
// The second half of this file is the framing structure's committed fuzz seed corpus, joined to
// the table p4 and p5 built rather than kept in a second one.
package mls

import (
	"bytes"
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
	"strings"
	"testing"

	"github.com/urnetwork/connect/mls/syntax"
)

// ---------------------------------------------------------------------------
// the type checked reading of a root, bodies included
// ---------------------------------------------------------------------------

// checkedBodies is one root's non test source with its function bodies type checked.
//
// A second reading rather than a use of typeCheckedRoot, because that one sets IgnoreFuncBodies
// -- right for every gate that judges signatures, and it leaves every expression inside a body
// with no type at all. Every comparison this file is about is inside a body.
type checkedBodies struct {
	root    string
	paths   []string
	fileSet *token.FileSet
	files   []*ast.File
	info    *types.Info
	source  map[string][]byte
	// offsets is every object of this root that holds an octet read out of an octet string at an
	// offset: a local, a parameter, or a function whose result is one. It is followed across the
	// whole root by octetsReadAtAnOffset rather than recognised one expression at a time, because
	// two assignment statements are enough to put a byte loop outside a rule that reads one.
	offsets map[types.Object]bool
}

// render answers the source text one node was written as, read back off the file it came out of
// rather than printed from the syntax tree, so a report names what a reviewer will find.
func (self checkedBodies) render(node ast.Node) string {
	from, to := self.fileSet.Position(node.Pos()), self.fileSet.Position(node.End())
	body, held := self.source[from.Filename]
	if !held || to.Offset > len(body) {
		return "?"
	}
	return strings.Join(strings.Fields(string(body[from.Offset:to.Offset])), " ")
}

// where names the line a report is about.
func (self checkedBodies) where(node ast.Node) string {
	return self.fileSet.Position(node.Pos()).String()
}

var checkedBodiesCache = map[string]checkedBodies{}

// typeCheckedBodiesOf type checks one root's non test source with its bodies, once.
//
// A refused check is fatal rather than a fall back to reading spellings, for the reason
// typeCheckedRoot gives: a gate that cannot resolve its subject must fail, and one that quietly
// reverted to matching text would go on reporting the clean run of a complete gate while holding
// a narrower class.
func typeCheckedBodiesOf(t *testing.T, root string) checkedBodies {
	t.Helper()
	cryptoTypeCheckMutex.Lock()
	defer cryptoTypeCheckMutex.Unlock()
	if checked, done := checkedBodiesCache[root]; done {
		return checked
	}
	checked := typeCheckBodies(t, root, rootSourcePaths(t, root), nil)
	checkedBodiesCache[root] = checked
	return checked
}

// typeCheckedBodiesOfText is the same over text a control holds, so the matcher runs on a package
// known to violate the rule as well as on the real one.
func typeCheckedBodiesOfText(t *testing.T, name string, source string) checkedBodies {
	t.Helper()
	cryptoTypeCheckMutex.Lock()
	defer cryptoTypeCheckMutex.Unlock()
	return typeCheckBodies(t, name, []string{name}, map[string]string{name: source})
}

// typeCheckBodies is the shared body of the two above. The caller holds the importer mutex: the
// source importer carries a cache and is not safe to enter twice at once.
func typeCheckBodies(t *testing.T, path string, names []string, text map[string]string) checkedBodies {
	t.Helper()
	fileSet := token.NewFileSet()
	files := []*ast.File{}
	source := map[string][]byte{}
	for _, name := range names {
		body, written := text[name]
		if !written {
			read, err := os.ReadFile(name)
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			body = string(read)
		}
		file, err := parser.ParseFile(fileSet, name, body, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, file)
		source[name] = []byte(body)
	}
	// Defs and Uses as well as Types: the provenance walk follows an octet from the index
	// expression that read it into the local it is assigned to and on into the parameter it is
	// handed to, and an object is the only thing that names those the same across a whole root.
	info := &types.Info{
		Types: map[ast.Expr]types.TypeAndValue{},
		Defs:  map[*ast.Ident]types.Object{},
		Uses:  map[*ast.Ident]types.Object{},
	}
	refused := []string{}
	config := types.Config{
		Importer: importer.ForCompiler(fileSet, "source", nil),
		Error:    func(err error) { refused = append(refused, err.Error()) },
	}
	if _, err := config.Check(path, fileSet, files, info); err != nil || len(refused) != 0 {
		t.Fatalf("type check %s with its bodies: %v %s", path, err, strings.Join(refused, "; "))
	}
	if len(info.Types) == 0 {
		t.Fatalf("the type checker recorded no expression for %s, so every rule below would read nothing", path)
	}
	checked := checkedBodies{root: path, paths: names, fileSet: fileSet, files: files, info: info, source: source}
	checked.offsets = octetsReadAtAnOffset(checked)
	return checked
}

// ---------------------------------------------------------------------------
// what an octet comparison is, derived from the compiler's own types
// ---------------------------------------------------------------------------

// The two shapes this gate reports, named so the control can require each half to report
// something of its own: a half that reports nothing can be deleted with the control still
// matching.
const (
	// wholeOctetComparison is a value whose EQUALITY is decided by comparing octets: a string, a
	// byte or rune array, or any aggregate holding one. go compiles each of those to a memequal
	// that returns at the first differing octet.
	wholeOctetComparison = "whole"
	// offsetOctetComparison is a comparison of an octet READ AT AN OFFSET out of such a value,
	// which is the byte loop. What it answers is not whether two strings are equal but WHERE
	// they stopped being equal, and that is the answer per query a timing side channel wants.
	offsetOctetComparison = "offset"
)

// comparesOctets answers whether go's == on this type is a comparison of octets.
//
// It reads the type the compiler assigned rather than the spelling, so a tag behind three named
// types is in the class; and it recurses through arrays and structs, because an aggregate
// holding a tag compares that tag when the aggregate is compared. A slice and a map are not
// comparable in go at all and can never reach an ==, so nothing here has to say anything about
// them.
//
// A struct of nothing but integers is deliberately NOT in the class, and that is the boundary
// rather than an oversight. Comparing two node indices, two epochs or two code points is a
// decision about numbers this package puts on the wire in the clear; a rule that swept them in
// would need an exemption list, which is the shape of gate that gets an exemption added rather
// than a bug fixed. What makes a comparison this gate's business is that an octet string is on
// one side of it.
func comparesOctets(one types.Type, seen map[types.Type]bool) bool {
	if seen[one] {
		return false
	}
	seen[one] = true
	switch spelled := types.Unalias(one).Underlying().(type) {
	case *types.Basic:
		return spelled.Kind() == types.String || spelled.Kind() == types.UntypedString
	case *types.Array:
		if isOctet(spelled.Elem()) {
			return true
		}
		return comparesOctets(spelled.Elem(), seen)
	case *types.Struct:
		for index := range spelled.NumFields() {
			if comparesOctets(spelled.Field(index).Type(), seen) {
				return true
			}
		}
	}
	return false
}

// isOctet is one element of an octet string.
func isOctet(one types.Type) bool {
	basic, isBasic := types.Unalias(one).Underlying().(*types.Basic)
	return isBasic && (basic.Kind() == types.Byte || basic.Kind() == types.Rune)
}

// holdsOctets is a value an octet can be read out of at an offset: a slice or an array of
// octets, or a string. It is the base of the index expression a byte loop is written with.
func holdsOctets(one types.Type) bool {
	switch spelled := types.Unalias(one).Underlying().(type) {
	case *types.Slice:
		return isOctet(spelled.Elem())
	case *types.Array:
		return isOctet(spelled.Elem())
	case *types.Basic:
		return spelled.Kind() == types.String || spelled.Kind() == types.UntypedString
	}
	return false
}

// octetComparison is one comparison the rule reports, with where it was written, because a gate
// that reports a violation has to say which line to open.
type octetComparison struct {
	shape    string
	where    string
	rendered string
}

// octetComparisonsIn reports every comparison of octets written without a comparator inside one
// syntax node.
//
// A SWITCH is read as the comparisons it is. go's expression switch compares its tag against
// each case with ==, so `switch string(tag) { case string(mine): }` is the same comparison as
// `string(tag) == string(mine)`, and a rule that read only a BinaryExpr would let it through.
// That is not thoroughness for its own sake: a comparison spelled as control flow is exactly
// what the earlier version of the tag verifier gate next door was walked past by.
func octetComparisonsIn(checked checkedBodies, node ast.Node) []octetComparison {
	found := []octetComparison{}
	judge := func(at ast.Node, left ast.Expr, right ast.Expr) {
		leftType, rightType := checked.info.Types[left].Type, checked.info.Types[right].Type
		if leftType == nil || rightType == nil {
			return
		}
		shape := ""
		switch {
		case comparesOctets(leftType, map[types.Type]bool{}) && comparesOctets(rightType, map[types.Type]bool{}):
			shape = wholeOctetComparison
		case readsAnOctetAtAnOffset(checked, left) || readsAnOctetAtAnOffset(checked, right):
			shape = offsetOctetComparison
		default:
			return
		}
		found = append(found, octetComparison{
			shape:    shape,
			where:    checked.where(at),
			rendered: checked.render(at),
		})
	}
	ast.Inspect(node, func(one ast.Node) bool {
		switch spelled := one.(type) {
		case *ast.BinaryExpr:
			if spelled.Op == token.EQL || spelled.Op == token.NEQ {
				judge(spelled, spelled.X, spelled.Y)
			}
		case *ast.SwitchStmt:
			if spelled.Tag == nil || spelled.Body == nil {
				return true
			}
			for _, statement := range spelled.Body.List {
				clause, isClause := statement.(*ast.CaseClause)
				if !isClause {
					continue
				}
				for _, value := range clause.List {
					judge(value, spelled.Tag, value)
				}
			}
		}
		return true
	})
	return found
}

// readsAnOctetAtAnOffset answers whether an operand is an octet that was read at an offset out of
// an octet string, wherever in the root that read happened.
//
// Four spellings answer yes, and they are four because the offending shape can be written in all
// four with nothing else changing: the index expression itself; an identifier the provenance walk
// has already reached; a conversion of one, since byte(a[at]) holds what a[at] held; and a call to
// a function whose own result is such a read, which is that read one frame further out.
func readsAnOctetAtAnOffset(checked checkedBodies, one ast.Expr) bool {
	switch spelled := ast.Unparen(one).(type) {
	case *ast.IndexExpr:
		base := checked.info.Types[spelled.X].Type
		return base != nil && holdsOctets(base)
	case *ast.Ident:
		return checked.offsets[objectOf(checked, spelled)]
	case *ast.SelectorExpr:
		return checked.offsets[objectOf(checked, spelled.Sel)]
	case *ast.CallExpr:
		if callee := calleeObject(checked, spelled); callee != nil && checked.offsets[callee] {
			return true
		}
		// a conversion carries its operand's provenance: byte(a[at]) and Octet(a[at]) are the
		// same read of the same offset with a different name written in front of it.
		if len(spelled.Args) == 1 && checked.info.Types[spelled.Fun].IsType() {
			return readsAnOctetAtAnOffset(checked, spelled.Args[0])
		}
	}
	return false
}

// objectOf is the declaration one identifier names, whether the identifier declares it or uses it.
func objectOf(checked checkedBodies, name *ast.Ident) types.Object {
	if declared := checked.info.Defs[name]; declared != nil {
		return declared
	}
	return checked.info.Uses[name]
}

// calleeObject is the function a call expression calls, for the calls a root can see: a plain
// name, a method or package selector, and either of those instantiated at a type argument.
func calleeObject(checked checkedBodies, call *ast.CallExpr) types.Object {
	called := ast.Unparen(call.Fun)
	switch spelled := called.(type) {
	case *ast.IndexExpr:
		called = ast.Unparen(spelled.X)
	case *ast.IndexListExpr:
		called = ast.Unparen(spelled.X)
	}
	switch spelled := called.(type) {
	case *ast.Ident:
		return objectOf(checked, spelled)
	case *ast.SelectorExpr:
		return objectOf(checked, spelled.Sel)
	}
	return nil
}

// functionsByObject is every function this root declares, reachable from the object a call site
// resolves to, which is what lets an argument be followed into the parameter it lands on.
func functionsByObject(checked checkedBodies) map[types.Object]*ast.FuncDecl {
	declared := map[types.Object]*ast.FuncDecl{}
	for _, file := range checked.files {
		for _, declaration := range file.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil {
				continue
			}
			if object := objectOf(checked, function.Name); object != nil {
				declared[object] = function
			}
		}
	}
	return declared
}

// parameterObjects is one declaration's parameters in the order a call site's arguments land on
// them, or nothing for a variadic signature, whose last parameter is not one argument.
func parameterObjects(checked checkedBodies, function *ast.FuncDecl) []types.Object {
	if function.Type.Params == nil {
		return []types.Object{}
	}
	objects := []types.Object{}
	for _, field := range function.Type.Params.List {
		if _, spread := field.Type.(*ast.Ellipsis); spread {
			return nil
		}
		for _, name := range field.Names {
			objects = append(objects, objectOf(checked, name))
		}
	}
	return objects
}

// octetsReadAtAnOffset answers every object of one root that holds an octet read at an offset.
//
// It exists because the first version of this rule read one expression and nothing else, and two
// assignment statements walked past it. content.GroupId[at] != groupId[at] was reported, and
// left := content.GroupId[at]; right := groupId[at]; if left != right was not, because that
// comparison holds two identifiers of type byte and a byte is not an octet STRING. Moving the same
// loop into a helper taking two bytes walked past it for the same reason one call further out, and
// both were measured surviving the whole suite. They are the hand written byte loop this gate is
// here for, so provenance is FOLLOWED rather than spelled: an octet stays an octet across a
// rename, across an argument and across a return.
//
// A FOLD deliberately does not carry provenance, and that is the boundary rather than an
// oversight. accumulated |= octet combines the octet with every octet read before it, so what the
// accumulator holds is a fact about the whole buffer and comparing it reads no offset -- which is
// exactly the sanctioned shape framing_protect.go's padding check is written in, and a rule that
// could not tell the two apart would be a rule this package could not obey. A plain assignment, an
// argument and a return carry the offset; a compound assignment ends it.
//
// The walk is a fixpoint because provenance chains: an index reaches a local, the local reaches an
// argument, the argument reaches a return, and the return reaches another call.
func octetsReadAtAnOffset(checked checkedBodies) map[types.Object]bool {
	carried := map[types.Object]bool{}
	functions := functionsByObject(checked)
	moved := true
	for moved {
		moved = false
		mark := func(one types.Object) {
			if one == nil || carried[one] || !isOctet(one.Type()) {
				return
			}
			carried[one] = true
			moved = true
		}
		reads := func(one ast.Expr) bool {
			checked.offsets = carried
			return readsAnOctetAtAnOffset(checked, one)
		}
		for _, file := range checked.files {
			ast.Inspect(file, func(node ast.Node) bool {
				switch spelled := node.(type) {
				case *ast.AssignStmt:
					// a rename only: := and = hand the value over unchanged, and every compound
					// assignment is a fold whose result is about more than one offset.
					if spelled.Tok != token.ASSIGN && spelled.Tok != token.DEFINE {
						return true
					}
					if len(spelled.Lhs) != len(spelled.Rhs) {
						return true
					}
					for at, right := range spelled.Rhs {
						if name, isName := spelled.Lhs[at].(*ast.Ident); isName && reads(right) {
							mark(objectOf(checked, name))
						}
					}
				case *ast.RangeStmt:
					// the value of a range over an octet string IS the octet at that offset,
					// which is the byte loop written without an index expression.
					if spelled.Value == nil || spelled.X == nil {
						return true
					}
					over := checked.info.Types[spelled.X].Type
					if over == nil || !holdsOctets(over) {
						return true
					}
					if name, isName := spelled.Value.(*ast.Ident); isName {
						mark(objectOf(checked, name))
					}
				case *ast.CallExpr:
					callee := calleeObject(checked, spelled)
					if callee == nil {
						return true
					}
					function, declaredHere := functions[callee]
					if !declaredHere {
						return true
					}
					parameters := parameterObjects(checked, function)
					if parameters == nil || len(parameters) != len(spelled.Args) {
						return true
					}
					for at, argument := range spelled.Args {
						if reads(argument) {
							mark(parameters[at])
						}
					}
				}
				return true
			})
		}
		// a function whose result is an octet read at an offset hands that read to every caller,
		// so the getter shape is the loop with the read moved one frame down.
		for object, function := range functions {
			if carried[object] {
				continue
			}
			if returnsAnOctetReadAtAnOffset(checked, function, reads) {
				carried[object] = true
				moved = true
			}
		}
	}
	checked.offsets = carried
	return carried
}

// returnsAnOctetReadAtAnOffset answers whether a declaration hands an octet read at an offset back
// to its callers. A nested function literal is stepped over, because what it returns is its own
// result and not this declaration's.
func returnsAnOctetReadAtAnOffset(checked checkedBodies, function *ast.FuncDecl,
	reads func(one ast.Expr) bool) bool {
	if function.Type.Results == nil || len(function.Type.Results.List) != 1 {
		return false
	}
	if len(function.Type.Results.List[0].Names) > 1 {
		return false
	}
	// the result has to BE an octet: a function answering a bool about two octets is a comparison
	// and is reported where it is written, not carried out to its callers.
	object := objectOf(checked, function.Name)
	signature, isSignature := object.Type().(*types.Signature)
	if !isSignature || signature.Results().Len() != 1 || !isOctet(signature.Results().At(0).Type()) {
		return false
	}
	found := false
	ast.Inspect(function.Body, func(node ast.Node) bool {
		if _, isLiteral := node.(*ast.FuncLit); isLiteral {
			return false
		}
		returned, isReturn := node.(*ast.ReturnStmt)
		if !isReturn || len(returned.Results) != 1 {
			return true
		}
		if reads(returned.Results[0]) {
			found = true
		}
		return true
	})
	return found
}

// octetDeclaration is one top level declaration the rule read, and the comparisons inside it.
//
// A SLICE of these rather than a map keyed by the declaration's NAME, and that is a correction
// rather than a preference. Keyed by name, every declaration whose name repeats anywhere in the
// root was overwritten by the last file parsed: 81 of this package's 377 top level declarations
// share a name with another one -- all 33 MarshalMLS, all 31 UnmarshalMLS, every Clone and every
// Validate -- so the gate judged one of each and cleared the other 80 without ever reporting them,
// both codec halves of every structure in framing.go included, which is the file this gate is
// named for. A comparison of octets planted in PrivateMessage.UnmarshalMLS was measured surviving
// the entire suite for that reason and no other.
//
// The control fixture could not have shown it: its declarations all have distinct names. What
// shows it is TestTheOctetComparisonGateReadsEveryDeclarationOfARepeatedName below, and the count
// the real gate now takes against a second, independent reading of the same directory.
type octetDeclaration struct {
	name        string
	where       string
	comparisons []octetComparison
}

// declarationsHoldingExpressions calls back once for each top level declaration of one file that
// can hold an expression, with the name it is reported under, the node its position is taken from
// and the node the rule reads.
//
// Per DECLARATION rather than per function, because a package level var initialised with
// string(a) == string(b) is outside every class of functions there is -- the same reason the
// comparator gate next door reads whole files rather than declarations.
//
// One walk, shared by the rule and by the count that checks the rule, so the two cannot drift on
// what a declaration is. What keeps that count independent is not a second walk but a second
// READING: it runs over files parsed straight off disk rather than over the type checked ones.
func declarationsHoldingExpressions(file *ast.File, visit func(name string, at ast.Node, holding ast.Node)) {
	for _, declaration := range file.Decls {
		switch spelled := declaration.(type) {
		case *ast.FuncDecl:
			visit(spelled.Name.Name, spelled.Name, spelled)
		case *ast.GenDecl:
			for _, one := range spelled.Specs {
				value, isValue := one.(*ast.ValueSpec)
				if !isValue || len(value.Values) == 0 {
					continue
				}
				for _, name := range value.Names {
					visit(name.Name, name, value)
				}
			}
		}
	}
}

// octetComparisonsByDeclaration reads one root and answers every top level declaration that can
// hold an expression, with the comparisons reported inside it.
func octetComparisonsByDeclaration(checked checkedBodies) []octetDeclaration {
	found := []octetDeclaration{}
	for _, file := range checked.files {
		declarationsHoldingExpressions(file, func(name string, at ast.Node, holding ast.Node) {
			found = append(found, octetDeclaration{
				name:        name,
				where:       checked.where(at),
				comparisons: octetComparisonsIn(checked, holding),
			})
		})
	}
	return found
}

// declarationNamesOnDisk is that same walk over a SECOND parse of the same files, sorted.
//
// It shares nothing with the type checked reading but the file names: it opens the files itself,
// so a reading that loaded no file at all, or that dropped a declaration on the way, differs from
// it and the gates below fail on the difference. That is the assertion the gate's prose already
// promised and did not hold. Its two self checks both measured the wrong quantity -- one counted
// file PATHS, the other compared rootSourcePaths' answer against a second call to rootSourcePaths
// -- so blanking the loaded files left the gate green while it advertised 39 files read and read
// zero declarations out of them.
func declarationNamesOnDisk(t *testing.T, paths []string) []string {
	t.Helper()
	fileSet := token.NewFileSet()
	names := []string{}
	for _, path := range paths {
		file, err := parser.ParseFile(fileSet, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s for a second reading: %v", path, err)
		}
		declarationsHoldingExpressions(file, func(name string, at ast.Node, holding ast.Node) {
			names = append(names, name)
		})
	}
	slices.Sort(names)
	return names
}

// judgedDeclarationsOf is one root's declarations with the count checked against that second
// reading, which is the only thing standing between this gate and a clean report over nothing.
func judgedDeclarationsOf(t *testing.T, checked checkedBodies) []octetDeclaration {
	t.Helper()
	declarations := octetComparisonsByDeclaration(checked)
	named := []string{}
	for _, one := range declarations {
		named = append(named, one.name)
	}
	slices.Sort(named)
	onDisk := declarationNamesOnDisk(t, checked.paths)
	if !slices.Equal(named, onDisk) {
		t.Fatalf("%s: the rule judged %d top level declarations and a second reading of the same %d files holds %d; a declaration outside this reading is one the rule cleared without opening, and a rule that read every file and nothing inside one reports exactly what a complete rule reports",
			checked.root, len(named), len(checked.paths), len(onDisk))
	}
	if len(declarations) == 0 {
		t.Fatalf("%s: not one top level declaration was judged", checked.root)
	}
	return declarations
}

// shapesOf is the distinct shapes a set of reports names, sorted, which is what an expectation is
// stated in: a position moves whenever a line above it does and is not the property asserted.
func shapesOf(reports []octetComparison) []string {
	shapes := []string{}
	for _, one := range reports {
		if !slices.Contains(shapes, one.shape) {
			shapes = append(shapes, one.shape)
		}
	}
	slices.Sort(shapes)
	return shapes
}

// ---------------------------------------------------------------------------
// the control fixture
// ---------------------------------------------------------------------------

// octetComparisonControl declares one of every shape the rule has to tell apart, so a matcher
// that stopped matching fails here rather than issuing the real source the clean bill a working
// one issues.
//
// Every member is here because some half of the rule has to be the only thing reporting it, or
// the only thing NOT reporting it:
//
//   - ComparesTwoStringsMadeOfOctets is the rewrite that actually survives on this project:
//     string(a) == string(b) names no comparator, so the derived comparator ban next door reads
//     clean over it, and it was measured surviving in tree.go and in treekem.go.
//   - ComparesTwoTagArrays and ComparesTwoEnvelopesHoldingATag are the same comparison one and
//     two levels of aggregate out. A rule reading only `string` would miss both, and a tag
//     declared as [32]byte is the likeliest way for one to appear.
//   - ComparesOctetByOctet is the hand written loop the comparator gate's own header says it
//     cannot see, and ComparesOneOctetAgainstAConstant is that loop's body with the second
//     operand folded to a literal -- still one answer per query about where the tag differs.
//   - SwitchesOnAStringOfOctets is the same comparison spelled as control flow.
//   - comparesInAWrapper is the byte loop moved out of the function that answers, and
//     LoopsInsideAHelper is that function: the rule must report the WRAPPER and not its caller,
//     because reading whole roots is what makes hiding it in a helper pointless.
//   - HoistsTheOctetsIntoLocalsFirst is that loop with the two octets copied into locals before
//     they are compared, which is two assignment statements and was measured surviving the whole
//     suite: the comparison then holds two identifiers of type byte, and a byte is not an octet
//     STRING. RangesOverTheOctetsAndCompares is the same loop with the index left out.
//   - LoopsThroughAnOctetHelper and octetsDifferOutOfLine are that hoist one call further out --
//     the loop delegating each pair of octets to a helper taking two bytes, which is the second
//     shape measured surviving. The rule must report the HELPER, where the comparison is written,
//     and not the loop, exactly as it does for comparesInAWrapper.
//   - ComparesThroughAnOctetGetter and octetAtOffset are the read itself moved down a frame, so
//     that the comparison names no index expression at all. The rule must report the CALLER here,
//     because the caller is where the comparison is, and say nothing about the getter.
//   - acceptsEverything is the same comparison in a package level initializer, which is what
//     says the rule reads declarations of every kind rather than function bodies.
//
// And the six that must NOT be reported, each of which is a real shape of this package's
// production source:
//
//   - ComparesWithTheSanctionedCall is subtle.ConstantTimeCompare's answer against 1.
//   - AccumulatesAndComparesAgainstZero is framing_protect.go's own padding check: the whole
//     buffer is folded with |= and the ACCUMULATOR is compared, so the comparison reads no
//     offset and the time does not vary with where the first non zero octet sits. It is the
//     sanctioned way to write what ComparesOneOctetAgainstAConstant writes badly, and a rule
//     that could not tell the two apart would be a rule this package could not obey.
//   - ComparesTwoLengths, ComparesTwoCounts and ComparesACodePoint are decisions about numbers
//     that travel in the clear.
//   - LooksUpAMapKeyedByOctets is the shape this gate deliberately does not read, and it is
//     named here so a reader cannot mistake silence for coverage. A map keyed by string is a
//     variable time comparison of octets; this package's sixteen of them are uniqueness scans
//     over PUBLIC tree keys and psk ids, and a rule reporting them would be a rule with sixteen
//     exemptions, which is the gate that stops being read. Nothing catches a secret put through
//     one, and that is written down rather than left implied.
const octetComparisonControl = `package control

import "crypto/subtle"

type Tag [32]byte

type Envelope struct {
	Tag   Tag
	Epoch uint64
}

type CodePoint uint16

var chosen = []byte{0x00}

var chosenOther = []byte{0x01}

var acceptsEverything = string(chosen) == string(chosenOther)

func ComparesTwoStringsMadeOfOctets(a []byte, b []byte) bool {
	return string(a) == string(b)
}

func ComparesTwoTagArrays(a Tag, b Tag) bool {
	return a == b
}

func ComparesTwoEnvelopesHoldingATag(a Envelope, b Envelope) bool {
	return a == b
}

func ComparesOctetByOctet(a []byte, b []byte) bool {
	same := len(a) == len(b)
	for at := range a {
		if at < len(b) && a[at] != b[at] {
			same = false
		}
	}
	return same
}

func ComparesOneOctetAgainstAConstant(tag []byte) bool {
	return len(tag) != 0 && tag[0] == 0x01
}

func SwitchesOnAStringOfOctets(a []byte, b []byte) bool {
	switch string(a) {
	case string(b):
		return true
	}
	return false
}

func LoopsInsideAHelper(a []byte, b []byte) bool {
	return comparesInAWrapper(a, b)
}

func comparesInAWrapper(a []byte, b []byte) bool {
	for at := range a {
		if a[at] != b[at] {
			return false
		}
	}
	return true
}

func HoistsTheOctetsIntoLocalsFirst(a []byte, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for at := range a {
		left := a[at]
		right := b[at]
		if left != right {
			return false
		}
	}
	return true
}

func RangesOverTheOctetsAndCompares(a []byte, b []byte) bool {
	for at, octet := range a {
		if at < len(b) && octet != b[at] {
			return false
		}
	}
	return true
}

func LoopsThroughAnOctetHelper(a []byte, b []byte) bool {
	for at := range a {
		if octetsDifferOutOfLine(a[at], b[at]) {
			return false
		}
	}
	return true
}

func octetsDifferOutOfLine(left byte, right byte) bool {
	return left != right
}

func ComparesThroughAnOctetGetter(a []byte, b []byte) bool {
	return octetAtOffset(a, 0) == octetAtOffset(b, 0)
}

func octetAtOffset(of []byte, at int) byte {
	return of[at]
}

func ComparesWithTheSanctionedCall(a []byte, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}

func AccumulatesAndComparesAgainstZero(padding []byte) bool {
	var accumulated byte
	for _, octet := range padding {
		accumulated |= octet
	}
	return accumulated != 0
}

func ComparesTwoLengths(a []byte, b []byte) bool {
	return len(a) == len(b)
}

func ComparesTwoCounts(a uint32, b uint32) bool {
	return a == b
}

func ComparesACodePoint(a CodePoint) bool {
	switch a {
	case 1, 2:
		return true
	}
	return false
}

func LooksUpAMapKeyedByOctets(seen map[string]bool, key []byte) bool {
	return seen[string(key)]
}
`

// octetComparisonControlReports is the shapes each control declaration must draw, exactly rather
// than as a floor.
//
// A control is the one thing in a derived gate that cannot itself be derived from the rule it
// controls, so it is written out; and it is stated for EVERY declaration that can hold an
// expression rather than only for the offending ones, because a rule that widened to report
// every comparison it reads fails here as surely as one that stopped matching.
var octetComparisonControlReports = map[string][]string{
	"chosen":                            {},
	"chosenOther":                       {},
	"acceptsEverything":                 {wholeOctetComparison},
	"ComparesTwoStringsMadeOfOctets":    {wholeOctetComparison},
	"ComparesTwoTagArrays":              {wholeOctetComparison},
	"ComparesTwoEnvelopesHoldingATag":   {wholeOctetComparison},
	"ComparesOctetByOctet":              {offsetOctetComparison},
	"ComparesOneOctetAgainstAConstant":  {offsetOctetComparison},
	"SwitchesOnAStringOfOctets":         {wholeOctetComparison},
	"LoopsInsideAHelper":                {},
	"comparesInAWrapper":                {offsetOctetComparison},
	"HoistsTheOctetsIntoLocalsFirst":    {offsetOctetComparison},
	"RangesOverTheOctetsAndCompares":    {offsetOctetComparison},
	"LoopsThroughAnOctetHelper":         {},
	"octetsDifferOutOfLine":             {offsetOctetComparison},
	"ComparesThroughAnOctetGetter":      {offsetOctetComparison},
	"octetAtOffset":                     {},
	"ComparesWithTheSanctionedCall":     {},
	"AccumulatesAndComparesAgainstZero": {},
	"ComparesTwoLengths":                {},
	"ComparesTwoCounts":                 {},
	"ComparesACodePoint":                {},
	"LooksUpAMapKeyedByOctets":          {},
}

// TestTheOctetComparisonGateFlagsItsControlFixture is the matcher's own control, and it runs
// before the gate over the real source so that a rule which stopped matching fails here rather
// than issuing this package a clean bill.
func TestTheOctetComparisonGateFlagsItsControlFixture(t *testing.T) {
	checked := typeCheckedBodiesOfText(t, "the octet comparison control", octetComparisonControl)
	reported := map[string][]octetComparison{}
	for _, one := range octetComparisonsByDeclaration(checked) {
		if _, twice := reported[one.name]; twice {
			t.Fatalf("the control declares %s at %s and again elsewhere, so the table below would state one expectation over two bodies; the repeated name control is the fixture for that shape",
				one.name, one.where)
		}
		reported[one.name] = one.comparisons
	}
	if len(reported) != len(octetComparisonControlReports) {
		t.Fatalf("the rule read %v out of the control and the control declares %d bodies it must judge; a declaration it does not read is a shape the real source can be written in",
			slices.Sorted(maps.Keys(reported)), len(octetComparisonControlReports))
	}
	for _, name := range slices.Sorted(maps.Keys(octetComparisonControlReports)) {
		got, read := reported[name]
		if !read {
			t.Errorf("the rule did not read %s out of the control", name)
			continue
		}
		if want := octetComparisonControlReports[name]; !slices.Equal(shapesOf(got), want) {
			t.Errorf("the rule reports %s with %v, want %v; a half of it that reports nothing of its own can be deleted with this control still matching",
				name, shapesOf(got), want)
		}
	}
	// both halves have to report something, said as a count rather than left to the table above:
	// a table can be edited to agree with a rule that has gone quiet, and a zero cannot.
	for _, shape := range []string{wholeOctetComparison, offsetOctetComparison} {
		drawn := 0
		for _, reports := range reported {
			for _, one := range reports {
				if one.shape == shape {
					drawn++
				}
			}
		}
		if drawn == 0 {
			t.Errorf("the %s half of the rule reported nothing over a control written to violate it", shape)
		}
	}
}

// octetComparisonRepeatedNameControl is the second control, and it holds the one property the
// first one structurally cannot: two declarations sharing a name, only one of which offends.
//
// The first control's seventeen declarations all have distinct names, so it matched a rule keyed
// by name exactly as well as it matches this one, and 81 of this package's own declarations went
// unreported underneath it. Two methods called UnmarshalMLS on two types is not an invented shape;
// it is what every structure in framing.go looks like, and the one that sorts last is the one the
// keyed rule kept.
const octetComparisonRepeatedNameControl = `package control

type First struct {
	Tag []byte
}

type Second struct {
	Tag []byte
}

func (self *First) UnmarshalMLS(tag []byte) bool {
	return string(self.Tag) == string(tag)
}

func (self *Second) UnmarshalMLS(tag []byte) bool {
	return len(self.Tag) == len(tag)
}
`

// TestTheOctetComparisonGateReadsEveryDeclarationOfARepeatedName is that control's assertion: two
// declarations of one name are two reports, and the offending one is reported wherever it sits.
func TestTheOctetComparisonGateReadsEveryDeclarationOfARepeatedName(t *testing.T) {
	checked := typeCheckedBodiesOfText(t, "the repeated declaration name control", octetComparisonRepeatedNameControl)
	read := 0
	offending := []string{}
	for _, one := range octetComparisonsByDeclaration(checked) {
		if one.name != "UnmarshalMLS" {
			continue
		}
		read++
		if slices.Equal(shapesOf(one.comparisons), []string{wholeOctetComparison}) {
			offending = append(offending, one.where)
		}
	}
	if read != 2 {
		t.Fatalf("the rule read %d declarations called UnmarshalMLS out of a control declaring two; a rule that reports one per NAME clears every repeated declaration this package has, which is 81 of its 377",
			read)
	}
	if len(offending) != 1 {
		t.Errorf("the rule reports %v as comparing whole octet strings and exactly one of the two declarations does", offending)
	}
}

// TestFramingUsesConstantTimeComparison is Spec A 5.9 G8 over the shape that names no comparator,
// across every production file of this package's own directory and of ../message.
//
// The name is the one the interface registry pins for this plan, and the scope is wider than the
// name: framing is where the demand comes from, and a rule that read only the seven framing files
// the plan lists would be the hand written class rule 5 forbids. The framing files are covered
// because they are non test go files of this package's directory and for no other reason, which
// is what the file count below is asserted for.
//
// The scope is stated as the DIRECTORY rather than as "everything this package ships" because
// rootSourcePaths globs <root>/*.go and does not recurse, so mls/syntax -- a separate package, and
// the codec every framing structure decodes through -- is outside this gate.
// TestNoPackageBeneathTheseRootsComparesAWholeOctetStringWithGoEquality below is what covers it,
// at the one strength a codec can obey, and its header says why the two strengths differ.
//
// Two counts are taken and they answer different questions. The FILE count says the rule opened
// every production file of the directory. The DECLARATION count says it read something inside
// them, and it is checked against declarationNamesOnDisk -- a second parse of the same paths --
// because the two self checks this gate had before both measured the wrong thing: one counted file
// paths, the other compared rootSourcePaths against a second call to rootSourcePaths. Blanking the
// loaded files after the type check left this gate green while it logged 39 files read.
//
// This package obeys the rule literally today: it holds no comparison of octets spelled as
// ordinary go equality at all, in either root, and its six comparisons of octets are all
// crypto/subtle.ConstantTimeCompare.
func TestFramingUsesConstantTimeComparison(t *testing.T) {
	roots := forbiddenScanRoots
	if len(roots) == 0 {
		t.Fatal("the scan reads no root at all, so this gate cleared every comparison having read nothing")
	}
	judged, read := 0, 0
	for _, root := range roots {
		checked := typeCheckedBodiesOf(t, root)
		onDisk := rootSourcePaths(t, root)
		if !slices.Equal(checked.paths, onDisk) {
			t.Fatalf("%s: the gate read %v and the directory holds %v; a production file outside this reading is a file the rule cleared without opening",
				root, checked.paths, onDisk)
		}
		judged += len(checked.paths)
		declarations := judgedDeclarationsOf(t, checked)
		read += len(declarations)
		reported := 0
		for _, declaration := range declarations {
			for _, one := range declaration.comparisons {
				reported++
				t.Errorf("%s at %s decides %q, which is a %s comparison of octets in variable time; every comparison of octets in this tree goes through %s, reached by CryptoProvider.MacVerify where a tag is what is being compared",
					declaration.name, one.where, one.rendered, one.shape, theSanctionedComparison())
			}
		}
		t.Logf("%s: %d production files read with their bodies type checked, %d top level declarations judged, %d comparisons of octets spelled as ordinary go equality",
			root, len(checked.paths), len(declarations), reported)
	}
	if judged == 0 {
		t.Fatal("no production file was judged, so this gate reported clean having read nothing")
	}
	if read == 0 {
		t.Fatal("no top level declaration was judged, so this gate reported clean having opened every file and read nothing inside one")
	}
}

// ---------------------------------------------------------------------------
// the production packages beneath these roots, at the strength a codec can obey
// ---------------------------------------------------------------------------

// productionPackagesBeneath answers every directory under one root, the root itself excluded, that
// holds non test go source.
//
// testdata is skipped, and so is every dot or underscore directory, for the reason the go tool
// skips them: what lives under testdata is fixtures -- including the deliberately forbidden source
// the positive controls scan -- and none of it is built.
func productionPackagesBeneath(t *testing.T, root string) []string {
	t.Helper()
	found := []string{}
	var walk func(directory string)
	walk = func(directory string) {
		entries, err := os.ReadDir(directory)
		if err != nil {
			t.Fatalf("read %s: %v", directory, err)
		}
		ships := false
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() {
				if name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
					continue
				}
				walk(filepath.Join(directory, name))
				continue
			}
			if strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
				ships = true
			}
		}
		if ships && directory != root {
			found = append(found, directory)
		}
	}
	walk(root)
	slices.Sort(found)
	return found
}

// TestNoPackageBeneathTheseRootsComparesAWholeOctetStringWithGoEquality is the same rule over the
// production packages the two scanned roots do not read, at the one strength a codec can obey.
//
// mls/syntax is the package this exists for: nine production files, the codec every framing
// structure decodes through, and outside both scanned roots for no reason anybody chose --
// rootSourcePaths globs <root>/*.go and does not recurse, and the roots were inherited from the
// forbidden call scan. Nothing in it compares octets with go equality today, so the gap is latent,
// and latent is what a gate is for.
//
// It demands the WHOLE half of the rule and not the offset half, and that split is derived from
// what these packages are rather than conceded to them. A decoder's job IS to read octets at
// offsets: mls/syntax reads the presence octet of an optional out of the buffer, compares it
// against zero and branches on it, and that is the wire format rather than a timing leak. A gate
// demanding the offset half here would be demanding a rule the package cannot obey, and would earn
// an exemption list inside a release -- which is the gate that stops being read. What no codec has
// any business doing is deciding two whole octet STRINGS equal with ==, because that is the shape a
// tag comparison hides in, and that half is demanded here in full.
func TestNoPackageBeneathTheseRootsComparesAWholeOctetStringWithGoEquality(t *testing.T) {
	beneath := []string{}
	for _, root := range forbiddenScanRoots {
		beneath = append(beneath, productionPackagesBeneath(t, root)...)
	}
	if len(beneath) == 0 {
		t.Fatalf("no production package was found beneath %v, so this gate judged nothing; mls/syntax is the one it was written over",
			forbiddenScanRoots)
	}
	for _, directory := range beneath {
		checked := typeCheckedBodiesOf(t, directory)
		declarations := judgedDeclarationsOf(t, checked)
		reported := 0
		for _, declaration := range declarations {
			for _, one := range declaration.comparisons {
				if one.shape != wholeOctetComparison {
					continue
				}
				reported++
				t.Errorf("%s at %s decides %q, which compares two whole octet strings with go equality; this package is outside %v and is judged on that half alone, and go compiles it to a memequal that returns at the first differing octet",
					declaration.name, one.where, one.rendered, forbiddenScanRoots)
			}
		}
		t.Logf("%s: %d production files read with their bodies type checked, %d top level declarations judged, %d whole octet string comparisons",
			directory, len(checked.paths), len(declarations), reported)
	}
}

// ---------------------------------------------------------------------------
// the framing structure's committed seed corpus
// ---------------------------------------------------------------------------

// privateMessageSeedTarget is the target name the seeds live under, because p8's loader and the
// one in key_schedule_roundtrip_test.go both join testdata/corpus with the target's own name.
//
// They join it with different SECOND elements, and that is the collision this project has to make
// one decision about rather than two. p8's is a CodecKind followed by a bytes or structured
// subfolder; this one's is the target. Whichever wins, the two gates over testdata/corpus both
// read the folder names directly under it -- the fuzz target one in key_schedule_roundtrip_test.go
// and the shared codec table one at the foot of this file -- so a layout only one of them knows
// about turns both of them red.
//
// It is NOT one of the nine names the interface registry gives p8 -- FuzzExtensionDecode,
// FuzzKeyPackageDecode, FuzzMlsMessageDecode, FuzzProposalDecode and FuzzWelcomeDecode with their
// Bytes twins -- and that is the whole reason this structure is the one seeded here. A corpus
// committed under a target this plan is forbidden to declare is 287 files no fuzz engine ever
// opens, which is what p5's review found and what the plan's own version of this task would have
// repeated: it writes four seeds flat into testdata/corpus, where nothing looks, for the
// MLSMessage and Proposal grammars, whose targets belong to p8 and to task 18.
const privateMessageSeedTarget = "FuzzPrivateMessageRoundTrip"

// seedPrivateMessages is the generator for that corpus: the registry axis crossed with the varint
// width boundaries of the three opaque fields and the uint64 boundaries of the epoch, plus the
// structure an mlswg implementation actually wrote.
//
// PrivateMessage is the framing structure this corpus is over, and the choice is not arbitrary.
// Under A-ASSUME-4 it is the only format v1 puts on the wire, so it is the structure every octet
// a stranger can send passes through; its cleartext header is four fields a message server reads
// without holding a key; and its registry closure is one enum, which is what makes the derived
// coverage gates over this table satisfiable at all. The other framing structures are either one
// of p8's nine, or reachable only through a codec that takes an argument (FramedContentAuthData
// needs the content type), or carry neither a varint prefixed field nor a uint64 and so cannot
// meet the two axes those gates state unconditionally.
//
// The content type axis is DERIVED from the package's own constant declarations rather than
// listed, for the reason rule 5 names. It carries no unregistered code point, and that is a
// property of this codec rather than a thinness here: MarshalMLS refuses one before it writes and
// UnmarshalMLS refuses one at the octet it is read at, so an unregistered value has no encoding
// to seed with.
func seedPrivateMessages(t *testing.T) []*PrivateMessage {
	t.Helper()

	contentTypes := sortedValues(registryConstantsOfType(t, "ContentType"))
	if len(contentTypes) == 0 {
		t.Fatal("the derivation read no ContentType code point, so this corpus would exercise one decoder arm")
	}
	opaques := seedOpaques()
	epochs := seedEpochs()

	corpus := []*PrivateMessage{}
	for contentIndex, contentType := range contentTypes {
		for groupIdIndex, groupId := range opaques {
			for epochIndex, epoch := range epochs {
				corpus = append(corpus, &PrivateMessage{
					GroupId:             groupId,
					Epoch:               epoch,
					ContentType:         ContentType(contentType),
					AuthenticatedData:   opaques[(groupIdIndex+epochIndex)%len(opaques)],
					EncryptedSenderData: opaques[(groupIdIndex+contentIndex)%len(opaques)],
					Ciphertext:          opaques[(epochIndex+contentIndex)%len(opaques)],
				})
			}
		}
	}
	// the empty non nil forms, which are one seed with the nil ones on disk and two distinct
	// values in the generator, and the one seed carrying the first length whose varint prefix
	// needs four octets -- one rather than an axis, because every seed carrying it costs 16 KiB
	// in the repository and the prefix width is a property of the prefix.
	corpus = append(corpus,
		&PrivateMessage{
			GroupId:             []byte{},
			Epoch:               1,
			ContentType:         ContentType(contentTypes[0]),
			AuthenticatedData:   []byte{},
			EncryptedSenderData: []byte{},
			Ciphertext:          []byte{},
		},
		&PrivateMessage{
			GroupId:             repeatByte(0xd0, 32),
			Epoch:               1,
			ContentType:         ContentType(contentTypes[len(contentTypes)-1]),
			AuthenticatedData:   repeatByte(0xd1, 1),
			EncryptedSenderData: repeatByte(0xd2, 32),
			Ciphertext:          repeatByte(0xd3, seedWideOpaqueLength),
		},
	)
	corpus = append(corpus, privateMessageFromTheVectors(t))
	return corpus
}

// privateMessageFromTheVectors is the seed this plan actually owns the knowledge of: the
// PrivateMessage an mlswg implementation published in the pinned messages vector.
//
// Every other seed above is this package's encoder writing out this package's own axes, and p5's
// header says what that is worth on its own -- a corpus generated from the encoder it checks
// agrees with that encoder by construction. This one is the other kind of evidence: octets
// written by an implementation that has never seen this codec, decoded here and put back on the
// axis list so the committed corpus carries a structure nothing in this tree invented.
//
// TestTheCommittedFramingSeedFromTheVectorsIsTheVectorsOwnOctets is what holds that claim, and it
// is a separate property because this function alone cannot: a decode that silently dropped a
// field would still produce a value, and a value re encoded is not the octets it came from.
func privateMessageFromTheVectors(t *testing.T) *PrivateMessage {
	t.Helper()
	raws := LoadVectorFile(t, "messages.json")
	if len(raws) == 0 {
		t.Fatal("messages.json holds no vector, so the seed taken from it is taken from nothing")
	}
	vector := messagesVector{}
	if err := json.Unmarshal(raws[0], &vector); err != nil {
		t.Fatalf("parse the messages vector: %v", err)
	}
	if vector.PrivateMessage == "" {
		t.Fatal("the messages vector publishes no private_message, so this seed would be this package's own encoder again")
	}
	message, err := ParseMLSMessage(MustHex(t, vector.PrivateMessage))
	if err != nil {
		t.Fatalf("the vector's private_message did not parse: %v", err)
	}
	if message.PrivateMessage == nil {
		t.Fatalf("the vector's private_message parsed as a %#04x message carrying no PrivateMessage", message.WireFormat)
	}
	return message.PrivateMessage
}

// framingSeedCodecs is this plan's row of the shared table. Joining that table rather than
// restating its properties is the point: the nine properties p4 and p5 state over seedCodecs --
// the corpus equals the generator, every seed re encodes to its own octets, every generated value
// survives its encoding, every registry code point is carried, every varint width and epoch
// boundary is carried, every field varies, every truncation and extension is refused, the folder
// is pinned as binary, and the folder is read by a declared target -- all hold over this corpus
// the moment it is in the table, and a second table is a second place for the same property to be
// stated more weakly.
func framingSeedCodecs() []seedCodec {
	return []seedCodec{
		{
			target:    privateMessageSeedTarget,
			structure: func() any { return &PrivateMessage{} },
			values: func(t *testing.T) []any {
				values := []any{}
				for _, value := range seedPrivateMessages(t) {
					values = append(values, value)
				}
				return values
			},
			decode: func(bs []byte) (any, error) {
				parsed := &PrivateMessage{}
				return parsed, syntax.Unmarshal(bs, parsed)
			},
			encode:         func(value any) ([]byte, error) { return syntax.Marshal(value.(*PrivateMessage)) },
			checkRoundTrip: syntax.CheckRoundTrip[PrivateMessage, *PrivateMessage],
			describe:       describePrivateMessage,
		},
	}
}

// describePrivateMessage names one seed value in a failure, since an index into a generated cross
// product says nothing about which case broke.
func describePrivateMessage(value any) string {
	one := value.(*PrivateMessage)
	return fmt.Sprintf("content type %d, epoch %#016x, group id %d octets, authenticated data %d, sender data %d, ciphertext %d",
		one.ContentType, one.Epoch, len(one.GroupId), len(one.AuthenticatedData),
		len(one.EncryptedSenderData), len(one.Ciphertext))
}

// TestTheCommittedFramingSeedFromTheVectorsIsTheVectorsOwnOctets is what makes one seed of this
// corpus independent evidence rather than a second copy of this package's encoder.
//
// It states the claim at the octet level in both directions: the PrivateMessage this package
// decodes out of the pinned vector re encodes to exactly the octets the vector publishes inside
// its MLSMessage framing, and the committed corpus holds those octets. A decoder that dropped a
// field would produce a value that re encodes to something shorter, and a corpus regenerated from
// a drifted encoder would hold that shorter form; either way the two comparisons below part
// company with the vector.
func TestTheCommittedFramingSeedFromTheVectorsIsTheVectorsOwnOctets(t *testing.T) {
	fromVector := privateMessageFromTheVectors(t)
	encoded, err := syntax.Marshal(fromVector)
	if err != nil {
		t.Fatalf("re encode the vector's private message: %v", err)
	}

	// the octets the vector publishes for this structure, cut out of the MLSMessage framing it
	// travels in rather than taken from a second hex literal here: the framing is a version, a
	// wire format and the body, so the body is the tail, and a tail that did not match would be
	// a codec disagreeing with the vector rather than a transcription error.
	raws := LoadVectorFile(t, "messages.json")
	vector := messagesVector{}
	if err := json.Unmarshal(raws[0], &vector); err != nil {
		t.Fatalf("parse the messages vector: %v", err)
	}
	published := MustHex(t, vector.PrivateMessage)
	if len(published) <= len(encoded) {
		t.Fatalf("the vector's private_message is %d octets and the PrivateMessage this package re encodes is %d; the body cannot be the tail of its own framing",
			len(published), len(encoded))
	}
	body := published[len(published)-len(encoded):]
	if !bytes.Equal(body, encoded) {
		t.Fatalf("this package re encodes the vector's private message to %x and the vector publishes %x", encoded, body)
	}

	_, onDisk := readSeedCorpus(t, privateMessageSeedTarget)
	carried := false
	for _, seed := range onDisk {
		if bytes.Equal(seed, encoded) {
			carried = true
			break
		}
	}
	if !carried {
		t.Errorf("no committed %s seed holds the %d octets the messages vector publishes for its private_message; the corpus is this package's own encoder end to end, and regenerating with %s=1 is the fix",
			privateMessageSeedTarget, len(encoded), seedCorpusWriteEnv)
	}
	t.Logf("%s: the messages vector's own %d octet private message is among the %d committed seeds",
		privateMessageSeedTarget, len(encoded), len(onDisk))
}

// FuzzPrivateMessageRoundTrip is gate 4 properties 1 and 2 on the section 6.3 PrivateMessage in
// their randomized form: no panic on adversarial input, and an encoding that decodes must re
// encode to the octets it came from.
//
// Through fuzzTheCommittedSeedCorpus, which loads the committed seeds, hands them to the engine
// and then asserts from the FAR SIDE that the engine ran them -- and through syntax.CheckRoundTrip
// rather than an open coded comparison, because that helper is the one every gate 4 target in
// this tree reaches its codec through and a target restating the comparison locally would be
// green against a helper that had stopped making it.
//
// The bool is the reachability count and it is the half this task adds. CheckRoundTrip returns
// nil for an input that does not decode -- correct against its contract, and silent -- so a
// target handed a corpus of octets none of which decode states its property over nothing and
// reports exactly what a complete run reports. p1 measured uniform random bytes reaching the
// round trip property 14 times in 4096 against the simplest structure in the tree, so that is
// not a hypothetical corpus; it is what an unseeded run of this target looks like.
//
// Both halves come off the shared table rather than being written out here, and the reachability
// half is why. Written out, it was an expression nothing observed: replacing it with a bare
// `return true` -- this target reporting that every input reached a decoder whatever decoded --
// left all 6591 tests of ./mls/... and ./message/... passing. Answered out of the codec's own
// decode it is controlled by TestEveryCommittedCorpusCodecAnswersReachabilityFromItsOwnDecoder,
// and TestNoCommittedCorpusTargetAnswersReachabilityWithoutReadingItsInput is what stops the next
// target from writing its own again.
func FuzzPrivateMessageRoundTrip(f *testing.F) {
	fuzzTheCommittedSeedCorpus(f, privateMessageSeedTarget,
		seedCodecFor(f, privateMessageSeedTarget).roundTripProperty())
}

// ---------------------------------------------------------------------------
// what the reachability answer is answered from
// ---------------------------------------------------------------------------

// anOctetStringItsDecoderRefuses is an input one codec does not decode, DERIVED from a committed
// seed by cutting octets off the end rather than written down as a literal.
//
// A literal would be a guess about a grammar, and a grammar that grew to accept it would leave the
// control below asserting nothing while still passing. A truncation is refused for a reason this
// package already states elsewhere -- every seed's truncation is refused by
// TestEverySeedInTheCommittedCorpusRefusesTruncationAndExtension -- so the shortest one that this
// codec's own decode rejects is the honest negative case.
func anOctetStringItsDecoderRefuses(t *testing.T, codec seedCodec, seed []byte) []byte {
	t.Helper()
	for cut := len(seed) - 1; cut >= 0; cut-- {
		if _, err := codec.decode(seed[:cut]); err != nil {
			return seed[:cut]
		}
	}
	t.Fatalf("%s: this codec decodes every one of the %d truncations of a committed seed, so there is no input left to state the refusal half of its property over",
		codec.target, len(seed))
	return nil
}

// TestEveryCommittedCorpusCodecAnswersReachabilityFromItsOwnDecoder is the control on the bool
// fuzzTheCommittedSeedCorpus counts, and it is the control that did not exist.
//
// The count exists to catch a corpus that reaches no decoder. Nothing pinned the ANSWER to a
// decode, so the count was as easy to satisfy by answering true as by decoding, and answering true
// is the exact defect it was added to expose: `return true` in place of the decode left the whole
// suite green over 80 seeds. So the property is stated in both directions here, over every entry
// of the shared table rather than over the one this file owns -- a committed seed, which this
// codec encoded and must therefore decode, answers true; and an input this codec's own decode
// refuses answers false.
func TestEveryCommittedCorpusCodecAnswersReachabilityFromItsOwnDecoder(t *testing.T) {
	codecs := seedCodecs()
	if len(codecs) == 0 {
		t.Fatal("the shared codec table is empty, so this control asserted nothing")
	}
	for _, codec := range codecs {
		names, bodies := readSeedCorpus(t, codec.target)
		property := codec.roundTripProperty()
		reached := 0
		for _, name := range names {
			if property(t, bodies[name]) {
				reached++
			}
		}
		if reached != len(names) {
			t.Errorf("%s: the property answers that %d of the %d committed seeds reached no decoder, and every committed seed is this codec's own encoding of a value it generated",
				codec.target, len(names)-reached, len(names))
		}
		refused := anOctetStringItsDecoderRefuses(t, codec, bodies[names[0]])
		if property(t, refused) {
			t.Errorf("%s: the property answers true over the %d octets its own decode refuses, so the reachability number is a count of executions and says nothing about whether one of them reached a decoder",
				codec.target, len(refused))
		}
		t.Logf("%s: %d committed seeds all answered as reaching the decoder, and %d octets it refuses answered as not",
			codec.target, len(names), len(refused))
	}
}

// TestNoCommittedCorpusTargetAnswersReachabilityWithoutReadingItsInput is the same property over
// the targets rather than over the table, because the control above holds the CONSTRUCTOR and a
// target is free to hand the runner something else.
//
// Two shapes are allowed and they are the two that can be checked. A target may reach its answer
// through the shared constructor, which the control above holds. Or it may write the property out,
// in which case every answer it returns has to READ the input it is answering about -- followed
// through the literal's own assignments, since a target that decodes into an err and returns
// `err == nil` reads the input by way of that err. What neither shape permits is a constant: a
// `return true` names nothing the engine handed in, and that is the whole mutation.
func TestNoCommittedCorpusTargetAnswersReachabilityWithoutReadingItsInput(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read this package's directory: %v", err)
	}
	fileSet := token.NewFileSet()
	judged := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fileSet, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, declaration := range file.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || !isFuzzTargetSignature(function) {
				continue
			}
			for _, handed := range propertiesHandedToTheSeedCorpusRunner(function) {
				judged++
				judgeTheReachabilityAnswer(t, fileSet, function.Name.Name, handed)
			}
		}
	}
	if judged == 0 {
		t.Fatalf("no fuzz target of this package hands a property to %s, so this gate judged nothing", seedCorpusRunner)
	}
	t.Logf("%d committed corpus targets, each answering reachability from something that reads its input", judged)
}

// propertiesHandedToTheSeedCorpusRunner is the property argument of every call one target makes to
// the shared runner.
func propertiesHandedToTheSeedCorpusRunner(function *ast.FuncDecl) []ast.Expr {
	handed := []ast.Expr{}
	ast.Inspect(function.Body, func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if !isCall {
			return true
		}
		named, isNamed := ast.Unparen(call.Fun).(*ast.Ident)
		if !isNamed || named.Name != seedCorpusRunner || len(call.Args) != 3 {
			return true
		}
		handed = append(handed, call.Args[2])
		return true
	})
	return handed
}

// judgeTheReachabilityAnswer reports a property whose answer cannot depend on the input.
func judgeTheReachabilityAnswer(t *testing.T, fileSet *token.FileSet, target string, handed ast.Expr) {
	t.Helper()
	literal, isLiteral := ast.Unparen(handed).(*ast.FuncLit)
	if !isLiteral {
		if !slices.Contains(identifiersOf(handed), seedCorpusReachabilityConstructor) {
			t.Errorf("%s at %s hands %s a property that is neither written out here nor built by %s, so nothing holds what its reachability answer is answered from",
				target, fileSet.Position(handed.Pos()), seedCorpusRunner, seedCorpusReachabilityConstructor)
		}
		return
	}
	input := ""
	if literal.Type.Params != nil {
		for _, field := range literal.Type.Params.List {
			for _, name := range field.Names {
				input = name.Name
			}
		}
	}
	if input == "" || input == "_" {
		t.Errorf("%s at %s writes its property out and does not name the input it is handed, so no answer it gives can depend on one",
			target, fileSet.Position(literal.Pos()))
		return
	}
	depends := namesReachingFrom(literal.Body, input)
	for _, returned := range answersOf(literal.Body) {
		reads := false
		for _, name := range identifiersOf(returned) {
			if depends[name] {
				reads = true
			}
		}
		if !reads {
			t.Errorf("%s at %s answers reachability with an expression naming none of %v, so it reports that the engine's input reached a decoder without reading that input; a corpus reaching no decoder reports exactly what a corpus reaching every arm reports",
				target, fileSet.Position(returned.Pos()), slices.Sorted(maps.Keys(depends)))
		}
	}
}

// namesReachingFrom is the input's name plus every local the input flows into, as a fixpoint: a
// target that decodes into an err and answers on that err reads its input through the err.
func namesReachingFrom(body *ast.BlockStmt, input string) map[string]bool {
	depends := map[string]bool{input: true}
	carry := func(from []ast.Expr, to []ast.Expr) bool {
		named := false
		for _, one := range from {
			for _, name := range identifiersOf(one) {
				if depends[name] {
					named = true
				}
			}
		}
		if !named {
			return false
		}
		moved := false
		for _, one := range to {
			name, isName := one.(*ast.Ident)
			if !isName || name.Name == "_" || depends[name.Name] {
				continue
			}
			depends[name.Name] = true
			moved = true
		}
		return moved
	}
	for moved := true; moved; {
		moved = false
		ast.Inspect(body, func(node ast.Node) bool {
			switch spelled := node.(type) {
			case *ast.AssignStmt:
				if carry(spelled.Rhs, spelled.Lhs) {
					moved = true
				}
			case *ast.RangeStmt:
				if carry([]ast.Expr{spelled.X}, []ast.Expr{spelled.Key, spelled.Value}) {
					moved = true
				}
			}
			return true
		})
	}
	return depends
}

// answersOf is every value one function literal returns, its own nested literals excluded.
func answersOf(body *ast.BlockStmt) []ast.Expr {
	answers := []ast.Expr{}
	ast.Inspect(body, func(node ast.Node) bool {
		if _, isLiteral := node.(*ast.FuncLit); isLiteral {
			return false
		}
		returned, isReturn := node.(*ast.ReturnStmt)
		if isReturn {
			answers = append(answers, returned.Results...)
		}
		return true
	})
	return answers
}

// identifiersOf is every name one expression spells.
func identifiersOf(one ast.Expr) []string {
	names := []string{}
	ast.Inspect(one, func(node ast.Node) bool {
		if name, isName := node.(*ast.Ident); isName {
			names = append(names, name.Name)
		}
		return true
	})
	return names
}

// ---------------------------------------------------------------------------
// the corpus folders, and that each of them has somewhere to be read from
// ---------------------------------------------------------------------------

// TestEveryCommittedCorpusFolderIsCarriedByTheSharedCodecTable is the other half of
// TestEveryCommittedCorpusFolderIsReadByAFuzzTarget, and it is a different question.
//
// That one asks whether a folder has a fuzz TARGET; this one asks whether it has a CODEC in the
// shared table, which is what every property stated over the corpus is stated through. A folder
// with a target and no codec is a folder the engine reads and no test on a plain go test run
// does: the corpus equality, the re encode, the registry coverage, the field variation and the
// truncation refusal all iterate seedCodecs, so a corpus outside it is exercised only under
// -fuzz, which CI does not run on every commit.
//
// Both sides are derived -- the folders on disk, and the table -- because a list of "the corpora
// we have" is the enumeration rule 5 forbids and it is a list that stays green after the entry it
// names has been deleted.
//
// One layout is known to be coming and it is written down here rather than left to be discovered.
// p8's loader joins testdata/corpus with a CodecKind name and then a bytes or structured
// subfolder, so a p8 corpus lands at testdata/corpus/<kind>/{bytes,structured}: a folder this
// table does not name, AND a folder no func <kind>(f *testing.F) is declared for. It turns this
// gate and TestEveryCommittedCorpusFolderIsReadByAFuzzTarget red together, measured by renaming
// this task's own corpus folder to a kind name and watching both fail. So the amendment p8 has to
// make is two sided -- a codec entry per kind here and a target per kind there -- or its loader
// keeps the folder name and the target name the same thing, the way this one does.
func TestEveryCommittedCorpusFolderIsCarriedByTheSharedCodecTable(t *testing.T) {
	table := map[string]bool{}
	for _, codec := range seedCodecs() {
		if table[codec.target] {
			t.Errorf("%s is in the shared codec table twice, so one of the two entries states every property over the other's corpus", codec.target)
		}
		table[codec.target] = true
	}
	if len(table) == 0 {
		t.Fatal("the shared codec table is empty, so every property stated over it holds vacuously")
	}

	root := filepath.Join("testdata", "corpus")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read %s: %v", root, err)
	}
	folders := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		folders++
		if !table[entry.Name()] {
			// a folder of folders is p8's layout, and saying so here is the difference between
			// one gate reporting a name it does not know and a reader being told that the other
			// gate is red for the same reason and needs the same amendment.
			nested := ""
			if !seedFolderHoldsFilesDirectly(t, filepath.Join(root, entry.Name())) {
				nested = fmt.Sprintf("; %s holds folders rather than seeds, which is p8's testdata/corpus/<kind>/{bytes,structured} layout, and that layout also fails TestEveryCommittedCorpusFolderIsReadByAFuzzTarget, so the amendment is two sided",
					entry.Name())
			}
			t.Errorf("%s/%s holds committed seeds and no entry of the shared codec table names %s; every property this package states over a corpus iterates that table, so those seeds are read by nothing a plain go test run executes%s",
				root, entry.Name(), entry.Name(), nested)
		}
	}
	if folders == 0 {
		t.Fatalf("%s holds no corpus folder, so this gate asserted nothing", root)
	}
	if folders != len(table) {
		t.Errorf("%d corpus folders are on disk and the shared codec table holds %d entries; an entry with no folder is a property stated over a corpus that is not committed",
			folders, len(table))
	}
	t.Logf("%d committed corpus folders, each carried by one of the %d entries of the shared codec table", folders, len(table))
}
