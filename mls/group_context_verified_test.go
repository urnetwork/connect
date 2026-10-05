// The gates over VerifiedGroupContext, and the two questions about it that ARE properties of
// source shape.
//
// WHAT THIS FILE IS INSTEAD OF. proposal_binding_test.go used to carry an AST walk over every call
// of a binding writer, refusing an argument whose chain of selections reached a type this package
// decodes off the wire. It was written three times and bypassed three times -- by any *GroupContext
// at all, then by one local struct between the decode and the bind, then twice over by an ordinary
// accessor method and by an embedded wire type whose promoted selection the walk never consulted
// the type checker about. All three bypasses are now compile errors, because NewProposalCache and
// Rebind take a *VerifiedGroupContext and none of those shapes produces one.
//
// THE DIFFERENCE BETWEEN THE QUESTION THAT FAILED AND THE ONES HELD HERE is worth stating, because
// this file is an AST gate too and a reader is entitled to ask what makes it different. The deleted
// gate asked WHERE A VALUE CAME FROM, which is a fact about a computation and not about the text:
// the same expression -- a field selected out of a struct -- is the defect when the struct was
// decoded and the remedy when it was the group's own state, and no amount of walking separates
// them. These ask WHICH DECLARATIONS BUILD A NAMED STRUCT TYPE and WHICH DECLARATIONS HAND ITS
// CONTENTS BACK OUT, and both are closed syntactic questions the type checker answers by object.
//
// TWO SCOPES WERE WRONG IN THE ROUND THAT WROTE THE FIRST OF THEM, AND BOTH ARE FIXED HERE, because
// each was a rule 5 failure -- a class stated as a shape narrower than the class:
//
//  1. THE WALK ENTERED FUNCTION BODIES. A construction at PACKAGE SCOPE is not in any body, so
//     var vouchAnything = func(c *GroupContext) *VerifiedGroupContext { return &VerifiedGroupContext{inner: c} }
//     appended to the source left the whole suite green -- measured at 6789 tests. The class is
//     "every construction of this type" and the scope of that class is every declaration of every
//     file, which is what file.Decls is; the walk now starts there and attributes what it finds to
//     the declaration it is inside, whatever kind of declaration that is.
//  2. NOTHING HELD THE TYPE TO HANDING OUT A COPY. The VALUE level half was held -- rewriting
//     Context's body from Clone to a bare return is caught -- and the CLASS was not, so
//     func (self *VerifiedGroupContext) Inner() *GroupContext { return self.inner } survived the
//     full suite. That is historical bypass #2, the ordinary accessor, reappearing one type up. The
//     class is every declaration that is handed one of these and answers a group context, and it is
//     asked twice over: by the source gate below, and behaviourally over the compiled method set,
//     which is the half that fires without anybody having updated a table. Two askings notice more
//     spellings than one; neither closes the class, for the reason under the next heading.
//
// SO THE SCOPE IS THIS PACKAGE AND THAT IS A COMPILER FACT rather than a choice: the field is
// unexported, so no other package can build one or read one, and the gate asserts that rather than
// leaving it as the unstated reason a narrow scan is enough. What an EXTERNAL package can reach is
// held from outside, in external_provenance_test.go, which is where the two demonstrated bypasses
// are run from.
//
// AND WITHIN THIS PACKAGE THESE GATES ARE A REVIEW AID AND NOT A FENCE, which is the honest reading
// and is measured rather than modest. Five rounds of hardening them each ended with a reviewer
// holding a new spelling: a defined struct type with the same underlying type, a non-empty
// interface carrying the pointer in an exported field, a type alias no matcher was unaliasing, an
// instantiated-generic collision in a cycle memo. Every one was real, and every one needed a
// DECLARATION ADDED TO PACKAGE mls -- an in-package forgery, which is a code review question rather
// than an attack, because the compiler closes the same class against every other package and that
// is where untrusted octets come from. Within one Go package an unexported field is visible to the
// whole package, so no source walk can enumerate the ways to write &VerifiedGroupContext{inner: x}.
// What these gates do is catch the ordinary spellings and name them against a table, so a new door
// is something a reviewer is shown rather than something they must think to look for. A green run
// here means a reviewer was helped. It does not mean no declaration can evade them, and a sixth
// round of matcher arms would not make it mean that.
package mls

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/types"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect/mls/syntax"
)

// The two names this file's derivation is written against, checked against the compiler's reading
// below rather than trusted: a rename that left these behind would empty the class, and an empty
// class is what a package that builds no verified context and a broken matcher both look like.
const (
	verifiedGroupContextTypeName  = "VerifiedGroupContext"
	verifiedGroupContextFieldName = "inner"
)

// verifiedGroupContextSite is one declaration's dealing with the type: where it is and how it is
// spelled.
type verifiedGroupContextSite struct {
	where string
	how   string
}

// verifiedGroupContextDeclarationName names ANY top level declaration the way this package's tables
// name declarations.
//
// The kind arm is the fix for the scope defect this file's header records. A gate that could only
// name a *ast.FuncDecl had to skip everything else, and skipping is how a construction at package
// scope became invisible; naming a var, const or type declaration by its own first name means the
// walk can descend into every declaration there is and still report what it found under something a
// reviewer can search for.
func verifiedGroupContextDeclarationName(checked checkedBodies, declaration ast.Decl) string {
	switch shape := declaration.(type) {
	case *ast.FuncDecl:
		return extensionTypeSelectionDeclarationName(checked, shape)
	case *ast.GenDecl:
		for _, spec := range shape.Specs {
			switch named := spec.(type) {
			case *ast.ValueSpec:
				if len(named.Names) > 0 {
					return shape.Tok.String() + " " + named.Names[0].Name
				}
			case *ast.TypeSpec:
				return shape.Tok.String() + " " + named.Name.Name
			case *ast.ImportSpec:
				return shape.Tok.String() + " " + named.Path.Value
			}
		}
		return shape.Tok.String()
	}
	return "an unnamed declaration at " + checked.where(declaration)
}

// verifiedGroupContextConstructionsIn is the first class: every declaration of one checked package
// that builds a VerifiedGroupContext carrying a context, with the count of nodes it judged at all.
//
// EVERY DECLARATION AND NOT EVERY FUNCTION BODY. The walk is over file.Decls, which is the whole of
// what a Go file holds, and it descends into each declaration entire rather than into a body it
// picked out. A composite literal in a var initializer is a construction of this type exactly as
// much as one inside a function is, and the round that scanned bodies proved that the difference is
// not academic: the package scope spelling left the suite green.
//
// THE TWO WAYS A FIELD IS ORDINARILY FILLED, WHICH ARE NOT ALL THE WAYS. A composite literal is how
// the constructor writes it and an assignment to the field is the other, so a matcher reading only
// one of them would report a clean run over a package that used the other; both arms go through the
// type checker rather than through the spelling, so a literal written with the type elided inside a
// slice of them is a member, and a local named VerifiedGroupContext in some other package is not.
// What they do NOT cover is said here rather than left to be discovered by the next reviewer: a
// conversion from an identically shaped struct type declared in this package builds one too and is
// neither arm. This walk is a review aid over the ordinary spellings -- see this file's header --
// and the compiler is what closes the class against every package but this one.
//
// AN EMPTY LITERAL IS NOT A MEMBER, and that is deliberate rather than an omission.
// VerifiedGroupContext{} carries a nil context, which is the zero value every door of this package
// already refuses with ErrNilGroupContext -- it confers no authority on anything, so counting it
// would make the class about a shape rather than about the thing the class is for. Another package
// can spell that literal too and no gate here could reach it; what makes that safe is the refusal
// and not this scan.
//
// The judged count is carried for the reason every derived gate of this package carries one: a
// matcher that stopped resolving its subject reports an EMPTY class, and an empty class is exactly
// what a package that builds none reports.
func verifiedGroupContextConstructionsIn(checked checkedBodies) (map[string][]verifiedGroupContextSite, int) {
	found := map[string][]verifiedGroupContextSite{}
	judged := 0
	for _, file := range checked.files {
		for _, declaration := range file.Decls {
			name := verifiedGroupContextDeclarationName(checked, declaration)
			record := func(node ast.Node) {
				found[name] = append(found[name], verifiedGroupContextSite{
					where: checked.where(node),
					how:   checked.render(node),
				})
			}
			ast.Inspect(declaration, func(node ast.Node) bool {
				switch built := node.(type) {
				case *ast.CompositeLit:
					if !extensionTypeSelectionNamedAs(checked.info.TypeOf(built),
						verifiedGroupContextTypeName) {
						return true
					}
					judged += 1
					if len(built.Elts) == 0 {
						return true
					}
					record(built)
				case *ast.AssignStmt:
					for _, target := range built.Lhs {
						selector, isSelector := extensionTypeSelectionUnparenthesised(
							target).(*ast.SelectorExpr)
						if !isSelector || selector.Sel.Name != verifiedGroupContextFieldName {
							continue
						}
						if !extensionTypeSelectionNamedAs(checked.info.TypeOf(selector.X),
							verifiedGroupContextTypeName) {
							continue
						}
						judged += 1
						record(built)
					}
				}
				return true
			})
		}
	}
	return found, judged
}

// verifiedGroupContextReadersIn is the second class: every declaration of one checked package that
// is HANDED a VerifiedGroupContext and ANSWERS a group context.
//
// THAT PAIR IS THE WHOLE RULE, and each half is load bearing. A declaration that takes one and
// answers a cache is not handing the context out; a declaration that takes a bare *GroupContext and
// answers one is an ordinary function over a value nobody vouched for. It is the conjunction that
// describes "a caller holding a verified value can reach the context inside it", which is the class
// historical bypass #2 lives in -- and the reason the value level assertion on Context alone was not
// enough, since an accessor added beside it satisfies that assertion by not being it.
//
// THE RECEIVER COUNTS AS BEING HANDED ONE, which is what makes a method of the type a member and is
// the difference between catching Inner and not. go/types puts the receiver outside Params, so a
// rule that read Params alone would report a clean run over exactly the spelling that was measured
// to survive.
//
// FUNCTION LITERALS AS WELL AS FUNCTION DECLARATIONS, for the reason the construction walk enters
// every declaration: var leakIt = func(v *VerifiedGroupContext) *GroupContext { return v.inner } is
// a reader written where no *ast.FuncDecl exists, and a rule that only judged declarations would
// have the same hole in it that the construction rule had.
//
// AND BOTH HALVES ASK THEIR QUESTION TRANSITIVELY, which is the round this file is on now. They
// used to test each tuple element with extensionTypeSelectionNamedAs, which unwraps *types.Pointer
// and nothing else, so a result of []*GroupContext -- or a map of them, or a channel of them, or an
// any holding one -- was read as answering no group context at all. Three such readers were
// measured surviving the whole suite at 6810 passing, and the control below now carries one per
// constructor of typeReachesNamed, so an arm that stops working drops a name out of the reported
// set rather than narrowing in silence. Where the transitive question STOPS -- a struct is entered
// through its exported fields, a named type's methods are not followed, a signature is entered
// through its results -- is argued in type_reach_test.go, and the first and second of those are
// what keep passItOn out of this class.
func verifiedGroupContextReadersIn(checked checkedBodies) (map[string][]verifiedGroupContextSite, int) {
	found := map[string][]verifiedGroupContextSite{}
	judged := 0
	for _, file := range checked.files {
		for _, declaration := range file.Decls {
			name := verifiedGroupContextDeclarationName(checked, declaration)
			ast.Inspect(declaration, func(node ast.Node) bool {
				var signature *types.Signature
				var at *ast.FuncType
				switch shape := node.(type) {
				case *ast.FuncDecl:
					defined, isFunction := checked.info.Defs[shape.Name].(*types.Func)
					if !isFunction {
						return true
					}
					signature, _ = defined.Type().(*types.Signature)
					at = shape.Type
				case *ast.FuncLit:
					signature, _ = checked.info.TypeOf(shape).(*types.Signature)
					at = shape.Type
				default:
					return true
				}
				if signature == nil {
					return true
				}
				judged += 1
				handed := typeReachesTupleNamed(signature.Params(),
					verifiedGroupContextTypeName)
				if signature.Recv() != nil && typeReachesNamed(
					signature.Recv().Type(), verifiedGroupContextTypeName) {
					handed = true
				}
				if !handed {
					return true
				}
				if !typeReachesTupleNamed(signature.Results(), groupContextTypeName) {
					return true
				}
				found[name] = append(found[name], verifiedGroupContextSite{
					where: checked.where(at),
					how:   checked.render(at),
				})
				return true
			})
		}
	}
	return found, judged
}

// the type a reader hands back, named beside the two above so a rename empties no class silently.
const groupContextTypeName = "GroupContext"

// verifiedGroupContextControl is a package the matchers have never seen, carrying every shape these
// two rules have an answer for.
//
// A control rather than a second opinion about the real source: a matcher that resolved nothing
// reports a clean run over mls too, and the only way to tell that apart from a package that obeys
// the rules is to run them over source known to hold both answers. It carries the two shapes that
// were MEASURED to survive the previous round -- a construction at package scope and an exported
// accessor answering the inner pointer -- so the control fails if either scope is narrowed again.
const verifiedGroupContextControl = `package control

type GroupContext struct {
	GroupId []byte
	Epoch   uint64
}

type VerifiedGroupContext struct {
	inner *GroupContext
}

// a type with a field of the same name, so the rule is about the TYPE the field belongs to and
// not about the word
type somethingElse struct {
	inner *GroupContext
}

// the shape a constructor is written in
func confirmIt(context *GroupContext) *VerifiedGroupContext {
	return &VerifiedGroupContext{inner: context}
}

// the same build with the field name left off, which a matcher reading only keyed elements walks
// straight past
func confirmItPositionally(context *GroupContext) *VerifiedGroupContext {
	return &VerifiedGroupContext{context}
}

// and the same build written as an assignment, which is the other of the two ways Go fills a field
func fillItAfterwards(context *GroupContext) *VerifiedGroupContext {
	built := &VerifiedGroupContext{}
	built.inner = context
	return built
}

// a literal with the type elided inside a slice of them, which is a build the type checker
// resolves and the source does not spell
func confirmSeveral(context *GroupContext) []VerifiedGroupContext {
	return []VerifiedGroupContext{{inner: context}}
}

// the zero value, which carries no context and confers nothing
func nothingAtAll() *VerifiedGroupContext {
	return &VerifiedGroupContext{}
}

// a COPY of one that already exists, which is not a new authority and is not a read of the
// context either
func passItOn(verified *VerifiedGroupContext) *VerifiedGroupContext {
	held := *verified
	return &held
}

// a read of the field written as a plain function, which is the reader rule's other half
func contextOf(verified *VerifiedGroupContext) *GroupContext {
	return verified.inner
}

// the accessor that was MEASURED to survive the whole suite, written as a method so the rule has
// to read the receiver to see it
func (self *VerifiedGroupContext) Inner() *GroupContext {
	return self.inner
}

// an ordinary function over a context nobody vouched for, which answers a group context and is
// not a reader of this type
func echo(context *GroupContext) *GroupContext {
	return context
}

// and the same field name filled on another type entirely
func fillTheOtherOne(context *GroupContext) *somethingElse {
	other := &somethingElse{}
	other.inner = context
	return other
}

// the construction at PACKAGE SCOPE that was measured to leave the whole suite green, in the
// initializer of a var, where no function body walk can reach it
var vouchAnything = func(context *GroupContext) *VerifiedGroupContext {
	return &VerifiedGroupContext{inner: context}
}

// and the same at package scope with no function around it at all
var alreadyVouched = &VerifiedGroupContext{inner: &GroupContext{Epoch: 1 << 40}}

// a reader at package scope, for the reason the construction above is here
var leakIt = func(verified *VerifiedGroupContext) *GroupContext {
	return verified.inner
}

// THE WRAPPED HAND-OUTS. Each is one constructor away from the shape the two gates were written
// against, and the first three of them were MEASURED to survive the whole suite at 6810 passing --
// driven end to end, Leaked()[0].GroupId = attackersGroupId turned "the honest group @ 3" into
// "ATTACKER-CHOSEN-GROUP @ 1099511627776" with NewProposalCache binding to that pair.
func leakedThroughASlice(verified *VerifiedGroupContext) []*GroupContext {
	return []*GroupContext{verified.inner}
}

func (self *VerifiedGroupContext) Leaked() []*GroupContext {
	return []*GroupContext{self.inner}
}

func (self *VerifiedGroupContext) Held() any {
	return self.inner
}

type contextPointer *GroupContext

type contextBox struct {
	Held *GroupContext
}

// the line the reach helper draws, run rather than argued: no holder can spell this field, which
// is the same property that makes handing back a verified value harmless
type sealedBox struct {
	held *GroupContext
}

func leakedThroughADefinedPointer(verified *VerifiedGroupContext) contextPointer {
	return verified.inner
}

func leakedThroughAMapValue(verified *VerifiedGroupContext) map[string]*GroupContext {
	return map[string]*GroupContext{"it": verified.inner}
}

func leakedThroughAChannel(verified *VerifiedGroupContext) chan *GroupContext {
	held := make(chan *GroupContext, 1)
	held <- verified.inner
	return held
}

func leakedThroughAFunctionResult(verified *VerifiedGroupContext) func() *GroupContext {
	return func() *GroupContext { return verified.inner }
}

func leakedThroughAStructField(verified *VerifiedGroupContext) *contextBox {
	return &contextBox{Held: verified.inner}
}

// and the one that is NOT a reader, which is where the transitive question stops
func notALeakThroughASealedBox(verified *VerifiedGroupContext) *sealedBox {
	return &sealedBox{held: verified.inner}
}
`

// What the construction matcher must report out of the control, and by omission what it must walk
// past.
var verifiedGroupContextControlConstructions = []string{
	"confirmIt",
	"confirmItPositionally",
	"confirmSeveral",
	"fillItAfterwards",
	"var alreadyVouched",
	"var vouchAnything",
}

// What the reader matcher must report out of the same control.
//
// Eleven and not three, because the question is asked transitively now. notALeakThroughASealedBox
// is the omission that carries the judgement: it hands the inner pointer into a struct whose field
// no holder can spell, which is the same property VerifiedGroupContext itself rests on, and a
// matcher that reported it would report every declaration answering a verified context too.
var verifiedGroupContextControlReaders = []string{
	"(*VerifiedGroupContext).Held",
	"(*VerifiedGroupContext).Inner",
	"(*VerifiedGroupContext).Leaked",
	"contextOf",
	"leakedThroughAChannel",
	"leakedThroughADefinedPointer",
	"leakedThroughAFunctionResult",
	"leakedThroughAMapValue",
	"leakedThroughASlice",
	"leakedThroughAStructField",
	"var leakIt",
}

// The declarations of this package that build a VerifiedGroupContext carrying a context, with why
// each is entitled to.
//
// ONE, and the gate below holds the derived class EQUAL to this in both directions, so a second one
// fails here until somebody says what it is. That is the whole point of keeping a gate at all after
// deleting the walk this file replaces: the type makes a bypass a compile error for every package
// but this one, and inside this one the remaining question is which declarations are allowed to
// confer the authority. A row with no declaration is a claim that outlived what it described; a
// declaration with no row is a new door onto the epoch a cache believes it is in, added without the
// conversation.
var verifiedGroupContextConstructionSites = map[string]string{
	"(*GroupInfo).VerifiedContext": "the one place in this package where a group context stops " +
		"being octets somebody sent and becomes a value this client vouches for. It is " +
		"(*GroupInfo).Verify and nothing else: a member of the ratchet tree the CALLER holds signed " +
		"the GroupInfoTBS these octets are, under the GroupInfoTBS label, and the signer's key came " +
		"out of that tree rather than out of the message. Every refusal it makes is one of Verify's, " +
		"and so is every gap -- the tree is the caller's to authenticate",
}

// The declarations of this package that are handed a VerifiedGroupContext and answer a group
// context, with why each is entitled to.
//
// ONE, held equal in both directions for the construction table's reason and against the bypass
// that was measured: an exported accessor answering self.inner satisfied every value level
// assertion in this file by not being the accessor those assertions name. A second reader is a
// second answer to "what can a holder of a verified context reach", and a verified value that can
// be edited after it was verified is not one.
var verifiedGroupContextReaderSites = map[string]string{
	"(*VerifiedGroupContext).Context": "the one read of a verified value there is, and it answers a " +
		"Clone. The value a cache binds to must not move under it, so what a holder is handed is a " +
		"copy it may do as it likes with; the pointer itself is never handed out, and that is a " +
		"property of the CLASS of readers rather than of this one body, which is why this table is " +
		"held equal in both directions",
}

// TestEveryConstructionOfAVerifiedGroupContextIsClassifiedHere is rule 5 over the class that
// replaced an AST walk three rounds could not make hold, with the scope defect a fourth round left
// in it fixed: the walk is over every declaration and not over every function body.
//
// It runs on the control first, which is what says the matcher reads anything at all, and then on
// the real source. The reflection half in front of both is not decoration: it is what makes the
// narrow scan legitimate. A field that became exported is a field any package can fill, and this
// whole file would then be watching one package out of every package there is.
func TestEveryConstructionOfAVerifiedGroupContextIsClassifiedHere(t *testing.T) {
	// the compiler enforced half of the scope, asserted rather than assumed
	verified := reflect.TypeOf(VerifiedGroupContext{})
	if verified.NumField() != 1 {
		t.Fatalf("%s declares %d fields; it holds exactly one, and a second is a second thing a construction could get wrong",
			verifiedGroupContextTypeName, verified.NumField())
	}
	field := verified.Field(0)
	if field.IsExported() {
		t.Fatalf("%s.%s is exported, so a declaration in ANY package can build a verified context around whatever it decoded, and the type no longer says anything at all",
			verifiedGroupContextTypeName, field.Name)
	}
	if field.Name != verifiedGroupContextFieldName {
		t.Fatalf("%s holds its context in %s and this file's derivation reads %s, so the assignment arm of the class is empty",
			verifiedGroupContextTypeName, field.Name, verifiedGroupContextFieldName)
	}
	if field.Type != reflect.TypeOf((*GroupContext)(nil)) {
		t.Fatalf("%s.%s is a %s; it is a POINTER to a group context so that the zero value carries nil and every door can refuse it, and a value there would make the zero value read as the empty group at epoch 0",
			verifiedGroupContextTypeName, field.Name, field.Type)
	}

	control := typeCheckedBodiesOfText(t, "the verified group context control", verifiedGroupContextControl)
	built, judged := verifiedGroupContextConstructionsIn(control)
	if judged == 0 {
		t.Fatal("the matcher judged no construction in the control, so it would report a clean run over any package at all")
	}
	if reported := slices.Sorted(maps.Keys(built)); !slices.Equal(reported, verifiedGroupContextControlConstructions) {
		t.Fatalf("the rule read %v out of the control as building a verified context, want %v; one that reads only keyed elements walks past the positional build, one that reads only literals walks past the assignment, one that reads the field NAME rather than the type it belongs to reports a build of something else, and one that enters function BODIES walks past both package scope builds -- which is the spelling that was measured to leave the whole suite green",
			reported, verifiedGroupContextControlConstructions)
	}

	checked := typeCheckedBodiesOf(t, ".")
	if len(checked.paths) == 0 {
		t.Fatal("this package holds no non test source, so the gate scanned nothing")
	}
	found, realJudged := verifiedGroupContextConstructionsIn(checked)
	if realJudged == 0 {
		t.Fatalf("no construction of a %s was resolved in this package, and its constructor certainly builds one, so this gate is not reading what it claims to",
			verifiedGroupContextTypeName)
	}
	classified := slices.Sorted(maps.Keys(verifiedGroupContextConstructionSites))
	if names := slices.Sorted(maps.Keys(found)); !slices.Equal(names, classified) {
		t.Fatalf("%v build a %s and this table names %v; a construction with no row is a new door onto the epoch a cache believes it is in, and a row with no construction is a claim that outlived what it described",
			names, verifiedGroupContextTypeName, classified)
	}
	for _, name := range classified {
		if verifiedGroupContextConstructionSites[name] == "" {
			t.Errorf("%s is classified with no account of what entitles it, which is the enumeration this gate exists to not be", name)
		}
		for _, one := range found[name] {
			t.Logf("%s at %s builds %q", name, one.where, one.how)
		}
	}
}

// TestEveryReaderOfAVerifiedGroupContextIsClassifiedHere is the same rule over the other half of
// the type's contract, and it is the gate the last round did not have at all.
//
// The value level property -- Context answers a Clone -- is asserted below and IS caught when
// Context's body is rewritten to a bare return. What it is not is a statement about the class: an
// added func (self *VerifiedGroupContext) Inner() *GroupContext { return self.inner } passed the
// entire suite, because every existing assertion was about Context by name. This asks the class
// question instead, in both directions.
func TestEveryReaderOfAVerifiedGroupContextIsClassifiedHere(t *testing.T) {
	control := typeCheckedBodiesOfText(t, "the verified group context control", verifiedGroupContextControl)
	read, judged := verifiedGroupContextReadersIn(control)
	if judged == 0 {
		t.Fatal("the matcher resolved no signature in the control, so it would report a clean run over any package at all")
	}
	if reported := slices.Sorted(maps.Keys(read)); !slices.Equal(reported, verifiedGroupContextControlReaders) {
		t.Fatalf("the rule read %v out of the control as handing a verified context's group context back, want %v; one that reads Params without the RECEIVER walks past the method -- which is the spelling that was measured to survive the whole suite -- one that reads only *ast.FuncDecl walks past the package scope literal, and one that asks either half of the pair alone reports the copy and the plain echo as readers",
			reported, verifiedGroupContextControlReaders)
	}

	checked := typeCheckedBodiesOf(t, ".")
	found, realJudged := verifiedGroupContextReadersIn(checked)
	if realJudged == 0 {
		t.Fatal("no signature of this package was resolved, so this gate is not reading what it claims to")
	}
	classified := slices.Sorted(maps.Keys(verifiedGroupContextReaderSites))
	if names := slices.Sorted(maps.Keys(found)); !slices.Equal(names, classified) {
		t.Fatalf("%v are handed a %s and answer a %s, and this table names %v; a reader with no row is a second way for a holder to reach the context a cache is bound to, and a row with no reader is a claim that outlived what it described",
			names, verifiedGroupContextTypeName, groupContextTypeName, classified)
	}
	for _, name := range classified {
		if verifiedGroupContextReaderSites[name] == "" {
			t.Errorf("%s is classified with no account of what entitles it, which is the enumeration this gate exists to not be", name)
		}
		for _, one := range found[name] {
			t.Logf("%s at %s reads %q", name, one.where, one.how)
		}
	}
}

// TestNoMethodOfAVerifiedGroupContextHandsOutTheStorageItVouchesFor is the behavioural half of the
// reader class, derived off the COMPILED method set rather than off the source.
//
// Two gates over one class on purpose. The source gate above fails when a reader is added without a
// row, which is a conversation somebody can have and then win; this one fails when a METHOD OF THE
// COMPILED TYPE hands out the storage, whatever any table says about it, and it needs nobody to
// have updated anything. Between them the added accessor that survived the last round fails twice.
// Neither closes the class: a plain function of this package that reads the field is not in this
// gate's subject at all, and what the source gate does not see is under this file's header.
//
// EVERY METHOD ANSWERING A CONTEXT IS DRIVEN, not Context by name, which is the whole point. The
// two answer shapes are both judged -- a *GroupContext, whose pointer is compared against the one
// the value holds, and a GroupContext by value, whose byte slices would alias even though the
// struct is a copy. A method this gate cannot call is REPORTED rather than skipped, because a
// skipped method is a clean run over a reader nobody looked at.
//
// AND "ANSWERING A CONTEXT" IS ASKED TRANSITIVELY, which is what this round fixed. The judgement
// used to be out == contextPointer || out == contextValue, so Leaked() []*GroupContext and
// Held() any were not judged at all -- both were measured surviving the whole suite at 6810
// passing, and the second of them was driven end to end into a cache bound to
// "ATTACKER-CHOSEN-GROUP @ 1099511627776". A method is judged now when its result type REACHES a
// group context through any composition of pointers, slices, arrays, maps, channels, function
// results, interfaces and exported struct fields, and what it returned is WALKED for the contexts
// a holder can actually get out of it. Both walks are typeReachesNamed's, controlled in
// type_reach_test.go with a member per constructor.
//
// The walk answers two different findings and the gate spends both. A POINTER reached is compared
// against the one the value holds, which catches every hand-out of the storage however it was
// wrapped; a COPY of the struct is written through, which catches a clone that kept the caller's
// octet slices. What the walk could not follow -- a function it cannot call, one that panicked --
// is reported for the reason an undriveable method is.
//
// The scribble is checked for being observable before it is trusted: a mutation that wrote nothing
// would leave the receiver unchanged for a reason that is this test's own and not the type's.
func TestNoMethodOfAVerifiedGroupContextHandsOutTheStorageItVouchesFor(t *testing.T) {
	verified := testVerifiedContextAt(t, &GroupContext{
		Version:                 ProtocolVersionMls10,
		CipherSuite:             CipherSuiteX25519ChaCha20Sha256Ed25519,
		GroupId:                 []byte("storage that is not handed out"),
		Epoch:                   13,
		ConfirmedTranscriptHash: bytes.Repeat([]byte{0x5c}, 32),
		Extensions: []Extension{{
			ExtensionType: ExtensionTypeRequiredCapabilities,
			ExtensionData: []byte{0x07, 0x08, 0x09},
		}},
	})
	// the receiver's own octets, read independently of Clone so that a broken Clone cannot make
	// this comparison agree with itself
	before, err := syntax.Marshal(verified.inner)
	if err != nil {
		t.Fatalf("the verified context did not marshal: %v", err)
	}

	holder := reflect.TypeOf(&VerifiedGroupContext{})
	judged, observed := 0, 0
	for i := range holder.NumMethod() {
		method := holder.Method(i)
		answers := false
		for at := range method.Type.NumOut() {
			if reflectTypeReaches(method.Type.Out(at), reachGroupContextTargets) {
				answers = true
			}
		}
		if !answers {
			continue
		}
		judged += 1
		if method.Type.NumIn() != 1 {
			t.Errorf("%s.%s answers a group context and takes %d arguments, so this gate cannot drive it; say what it is rather than leaving a reader nobody looked at",
				verifiedGroupContextTypeName, method.Name, method.Type.NumIn()-1)
			continue
		}
		reached, blocked := []reachedGroupContext{}, []string{}
		for at, result := range method.Func.Call([]reflect.Value{reflect.ValueOf(verified)}) {
			reachGroupContextsIn(result, fmt.Sprintf("%s result %d", method.Name, at),
				&reached, &blocked, 0)
		}
		for _, one := range blocked {
			t.Errorf("%s.%s answers something this gate could not follow -- %s -- so it is a reader nobody looked at",
				verifiedGroupContextTypeName, method.Name, one)
		}
		for _, one := range reached {
			observed += 1
			if one.identity == verified.inner {
				t.Errorf("%s.%s hands out the very pointer the value holds, reached as %s, so any holder of a verified context can rewrite the group it vouches for after the vouching",
					verifiedGroupContextTypeName, method.Name, one.how)
				continue
			}
			whole, err := syntax.Marshal(one.context)
			if err != nil {
				t.Fatalf("%s answered a context that did not marshal: %v", method.Name, err)
			}
			scribbleOverAGroupContext(one.context)
			scribbled, err := syntax.Marshal(one.context)
			if err != nil {
				t.Fatalf("%s answered a context that did not marshal after the scribble: %v", method.Name, err)
			}
			if bytes.Equal(whole, scribbled) {
				t.Fatalf("the scribble changed nothing in what %s answered, so the comparison below would agree for this test's own reason rather than the type's",
					method.Name)
			}
		}
	}
	if judged == 0 {
		t.Fatalf("no method of *%s answers a type that reaches a group context, and Context certainly does, so this gate drove nothing",
			verifiedGroupContextTypeName)
	}
	if observed == 0 {
		t.Fatalf("this gate drove %d methods of *%s and got no group context out of what any of them answered, so a leak in every one of them would read as a clean run",
			judged, verifiedGroupContextTypeName)
	}
	after, err := syntax.Marshal(verified.inner)
	if err != nil {
		t.Fatalf("the verified context did not marshal after the scribbles: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("writing through what a method of *%s answered moved the verified value from %x to %x; a value a cache is bound to must not move under it",
			verifiedGroupContextTypeName, before, after)
	}
}

// scribbleOverAGroupContext writes into every byte a context holds and moves its epoch.
//
// Every field rather than one, because aliasing is per slice: a Clone that copied the group id and
// kept the caller's transcript hash would pass a test that only wrote through the group id.
func scribbleOverAGroupContext(context *GroupContext) {
	context.Epoch ^= 0xffff
	for _, field := range [][]byte{context.GroupId, context.TreeHash, context.ConfirmedTranscriptHash} {
		for at := range field {
			field[at] ^= 0xff
		}
	}
	for _, extension := range context.Extensions {
		for at := range extension.ExtensionData {
			extension.ExtensionData[at] ^= 0xff
		}
	}
}

// TestNeitherWriterOfTheCacheBindingTakesABareGroupContext is the compile time property written
// down, over the class the write gate already derives rather than over a list.
//
// The three bypasses this design replaces are compile errors now. A compile error IS reportable --
// external_provenance_test.go compiles the external forging spellings and observes each refusal in
// TestEachForgedSpellingThisGateCompilesIsRefusedForItsOwnReason -- but that gate
// stands at the PACKAGE BOUNDARY, and what makes those spellings compile errors in the first place
// is the signature this test reads: no declaration that writes the cache binding takes a
// *GroupContext at all, and every one of them takes the verified type. A widening back to
// *GroupContext -- the one edit that would let a decoded context reach the binding again in any
// spelling -- fails here, and the external compile gate would not see it at all, because a widened
// signature compiles perfectly and that gate would go on reporting clean refusals over it.
func TestNeitherWriterOfTheCacheBindingTakesABareGroupContext(t *testing.T) {
	written, _ := proposalBindingWritesIn(typeCheckedBodiesOf(t, "."))
	if len(written) == 0 {
		t.Fatal("no declaration of this package writes the cache binding, so this gate demands nothing")
	}
	cache := reflect.TypeOf(&ProposalCache{})
	bare := reflect.TypeOf((*GroupContext)(nil))
	confirmed := reflect.TypeOf((*VerifiedGroupContext)(nil))
	for _, name := range slices.Sorted(maps.Keys(written)) {
		// the writers are a plain function and methods of the cache, which are the two
		// shapes this package's binding writers take; a writer somewhere else is reported
		// rather than skipped, because a signature nothing here can read is one nothing
		// here holds
		signature, held := bindingWriterSignature(cache, name)
		if !held {
			t.Errorf("%s writes the cache binding and is neither a package level function this gate can find nor a method of *%s, so nothing checks what it takes",
				name, epochCacheTypeName)
			continue
		}
		takesConfirmed := false
		for at := 0; at < signature.NumIn(); at++ {
			// TRANSITIVELY on both halves. A writer taking []*GroupContext is the same
			// door as one taking a bare pointer, and it is the door three measured
			// leaks went through one type up. A *VerifiedGroupContext reaches no group
			// context: its field is unexported, which is the line type_reach_test.go
			// draws and the reason this gate can go on demanding that very type.
			if reflectTypeReaches(signature.In(at), reachGroupContextTargets) {
				t.Errorf("%s writes the cache binding and takes %s, which reaches a %s; every group context names some epoch and this package decodes one straight off peer octets, so that parameter is a claim about a struct's fields and not about anybody's authority -- take a %s, whose only constructor verified a member's signature over it",
					name, signature.In(at), bare, confirmed)
			}
			if reflectTypeReaches(signature.In(at), []reflect.Type{confirmed}) {
				takesConfirmed = true
			}
		}
		if !takesConfirmed {
			t.Errorf("%s writes the cache binding and takes no %s, so nothing says where the epoch it writes came from",
				name, confirmed)
		}
	}
}

// bindingWriterSignature answers the compiled signature of one binding writer, named the way this
// package's tables name declarations.
//
// The method arm is read off the cache's own method set and the function arm off the one exported
// constructor there is, because those are the two shapes a binding writer takes here. A name it
// cannot resolve answers false rather than being skipped, which is what the caller reports.
func bindingWriterSignature(cache reflect.Type, name string) (reflect.Type, bool) {
	if bare, isMethod := strings.CutPrefix(name, "(*"+epochCacheTypeName+")."); isMethod {
		method, held := cache.MethodByName(bare)
		if !held {
			return nil, false
		}
		return method.Type, true
	}
	if name == "NewProposalCache" {
		return reflect.TypeOf(NewProposalCache), true
	}
	return nil, false
}

// ---------------------------------------------------------------------------
// what the constructor actually establishes
// ---------------------------------------------------------------------------

// testSignedGroupInfoOver is a GroupInfo naming this tree at the caller's context, signed by the
// member at the leaf it names.
//
// IT OVERWRITES THE CALLER'S TREE HASH, in place, and that is not a convenience. A group info whose
// context named some other tree is refused by rule 1 of Verify before anything else is looked at,
// so a fixture that left the caller's tree hash alone would make every test below refuse for the
// fixture's reason rather than its own -- and a caller comparing what it passed in against what
// came out would then be comparing against a context this client never vouched for.
func testSignedGroupInfoOver(t *testing.T, crypto CryptoProvider, tree *RatchetTree,
	members []*testTreeMember, signer LeafIndex, context *GroupContext) *GroupInfo {
	t.Helper()
	treeHash, err := tree.TreeHash(crypto)
	if err != nil {
		t.Fatalf("TreeHash: %v", err)
	}
	context.TreeHash = treeHash
	info := &GroupInfo{
		GroupContext: *context,
		Extensions: []Extension{{
			ExtensionType: ExtensionTypeRequiredCapabilities,
			ExtensionData: []byte{0x04, 0x05},
		}},
		ConfirmationTag: bytes.Repeat([]byte{0x32}, crypto.HashSize()),
		Signer:          signer,
	}
	if err := info.Sign(crypto, members[signer].SignaturePriv); err != nil {
		t.Fatalf("Sign at leaf %d: %v", signer, err)
	}
	return info
}

// testVerifiedContextAt is the group context of one epoch with its authority established the one
// way this package establishes it, for the tests that need a cache bound somewhere.
//
// It goes through the whole door -- a real two member tree, a real signature by member 0, a real
// (*GroupInfo).Verify -- rather than reaching for the field, because a fixture that built the value
// some shorter way would leave every test that uses it passing over a constructor that had stopped
// checking anything.
func testVerifiedContextAt(t *testing.T, groupContext *GroupContext) *VerifiedGroupContext {
	t.Helper()
	crypto := testCrypto(t)
	tree, members := newTestTree(t, crypto, 2)
	info := testSignedGroupInfoOver(t, crypto, tree, members, LeafIndex(0), groupContext)
	verified, err := info.VerifiedContext(crypto, tree)
	if err != nil {
		t.Fatalf("VerifiedContext over epoch %d of group %x: %v",
			groupContext.Epoch, groupContext.GroupId, err)
	}
	return verified
}

// TestVerifiedContextAnswersTheContextTheSignatureCovers is the accepting half.
//
// Every field is compared and not only the two the cache binds to, because the value this hands
// back is the whole context and a constructor that answered the right group at the wrong tree hash
// would be handing a caller an epoch nobody published.
//
// The fixture carries an EXTENSION and fills both hashes rather than leaving them nil, and that is
// not decoration. The codec has one spelling for an absent vector and an empty one, so a field left
// nil comes back empty and the comparison would fail for a reason that is the codec convention
// rather than this constructor. Carrying them also says the whole context survives the serialize
// and decode this door answers through, which is the claim being made.
func TestVerifiedContextAnswersTheContextTheSignatureCovers(t *testing.T) {
	crypto := testCrypto(t)
	tree, members := newTestTree(t, crypto, 4)
	context := &GroupContext{
		Version:                 ProtocolVersionMls10,
		CipherSuite:             CipherSuiteX25519ChaCha20Sha256Ed25519,
		GroupId:                 []byte("a group"),
		Epoch:                   7,
		ConfirmedTranscriptHash: bytes.Repeat([]byte{0x32}, crypto.HashSize()),
		Extensions: []Extension{{
			ExtensionType: ExtensionTypeRequiredCapabilities,
			ExtensionData: []byte{0x00, 0x00, 0x00},
		}},
	}
	info := testSignedGroupInfoOver(t, crypto, tree, members, LeafIndex(2), context)
	verified, err := info.VerifiedContext(crypto, tree)
	if err != nil {
		t.Fatalf("VerifiedContext over a group info its own member signed: %v", err)
	}
	answered := verified.Context()
	if answered == nil {
		t.Fatal("a verified context answered nothing, so every door below would refuse it")
	}
	if !reflect.DeepEqual(answered, &info.GroupContext) {
		t.Errorf("VerifiedContext answered %+v, want %+v; it decodes the octets the signature covered, so a difference here is the answer describing an epoch nobody signed",
			answered, &info.GroupContext)
	}
}

// TestVerifiedContextRefusesEveryGroupInfoVerifyRefuses is the refusing half, and it is the
// property the whole design rests on.
//
// EVERY ROW ASKS FOR ITS OWN SENTINEL and not merely for an error, which is this project's most
// repeated defect asked at a new door: errors.Is cannot tell two rules apart when they answer one
// value, so a row asserting "some refusal" passes over a constructor that refused for a reason
// nobody meant. The six are Verify's four rules and its two argument refusals, and each is built
// adversarially rather than described.
//
// Every row also asserts that NOTHING was answered. A constructor that returned a usable verified
// context beside a non nil error would be read as "err != nil" by a careful caller and as a
// verified epoch by everybody else, and the value that would then be bound is the attacker's.
func TestVerifiedContextRefusesEveryGroupInfoVerifyRefuses(t *testing.T) {
	crypto := testCrypto(t)
	tree, members := newTestTree(t, crypto, 4)
	other, _ := newTestTree(t, crypto, 2)
	contextAt := func(epoch uint64) *GroupContext {
		return &GroupContext{
			Version:                 ProtocolVersionMls10,
			CipherSuite:             CipherSuiteX25519ChaCha20Sha256Ed25519,
			GroupId:                 []byte("a group"),
			Epoch:                   epoch,
			ConfirmedTranscriptHash: bytes.Repeat([]byte{0x33}, crypto.HashSize()),
		}
	}

	// the positive control, so that nothing below passes because this door refuses everything
	control := testSignedGroupInfoOver(t, crypto, tree, members, LeafIndex(1), contextAt(1))
	if _, err := control.VerifiedContext(crypto, tree); err != nil {
		t.Fatalf("the honest group info was refused with %v, so every refusal below says nothing", err)
	}

	rows := []struct {
		name     string
		sentinel error
		call     func(t *testing.T) (*VerifiedGroupContext, error)
	}{{
		name:     "no crypto provider, which is how the signature would be checked",
		sentinel: ErrNilCryptoProvider,
		call: func(t *testing.T) (*VerifiedGroupContext, error) {
			return testSignedGroupInfoOver(t, crypto, tree, members, LeafIndex(0),
				contextAt(2)).VerifiedContext(nil, tree)
		},
	}, {
		name:     "no tree, which is the only thing that can say who the members are",
		sentinel: ErrTreeMalformed,
		call: func(t *testing.T) (*VerifiedGroupContext, error) {
			return testSignedGroupInfoOver(t, crypto, tree, members, LeafIndex(0),
				contextAt(3)).VerifiedContext(crypto, nil)
		},
	}, {
		name:     "a signed context naming a DIFFERENT tree than the one it is checked against",
		sentinel: ErrWelcomeTreeHashMismatch,
		call: func(t *testing.T) (*VerifiedGroupContext, error) {
			// signed over the other tree's hash, so the signature is perfectly good and
			// the context is about a group this tree is not
			info := testSignedGroupInfoOver(t, crypto, other, members, LeafIndex(0), contextAt(4))
			return info.VerifiedContext(crypto, tree)
		},
	}, {
		name:     "a signer index past the end of this tree",
		sentinel: ErrLeafIndexOutOfRange,
		call: func(t *testing.T) (*VerifiedGroupContext, error) {
			info := testSignedGroupInfoOver(t, crypto, tree, members, LeafIndex(0), contextAt(5))
			info.Signer = LeafIndex(1 << 20)
			if err := info.Sign(crypto, members[0].SignaturePriv); err != nil {
				t.Fatalf("re-sign at the out of range index: %v", err)
			}
			return info.VerifiedContext(crypto, tree)
		},
	}, {
		name:     "a signer index naming a position no member occupies",
		sentinel: errBlankSenderLeaf,
		call: func(t *testing.T) (*VerifiedGroupContext, error) {
			blanked, blankedMembers := newTestTree(t, crypto, 4)
			if err := blanked.Blank(LeafIndex(2).NodeIndex()); err != nil {
				t.Fatalf("Blank leaf 2: %v", err)
			}
			// the info is built AFTER the blanking, so its tree hash is the blanked
			// tree's and rule 1 is not what refuses this row
			info := testSignedGroupInfoOver(t, crypto, blanked, blankedMembers, LeafIndex(0),
				contextAt(6))
			info.Signer = LeafIndex(2)
			if err := info.Sign(crypto, blankedMembers[0].SignaturePriv); err != nil {
				t.Fatalf("re-sign at the blank leaf: %v", err)
			}
			return info.VerifiedContext(crypto, blanked)
		},
	}, {
		name:     "a signature by a key that sits in no leaf of this tree",
		sentinel: ErrWelcomeGroupInfoSignature,
		call: func(t *testing.T) (*VerifiedGroupContext, error) {
			stranger, _, err := crypto.SignatureKeyPair()
			if err != nil {
				t.Fatalf("SignatureKeyPair: %v", err)
			}
			info := testSignedGroupInfoOver(t, crypto, tree, members, LeafIndex(0), contextAt(7))
			if err := info.Sign(crypto, stranger); err != nil {
				t.Fatalf("Sign as the stranger: %v", err)
			}
			return info.VerifiedContext(crypto, tree)
		},
	}}

	for _, row := range rows {
		verified, err := row.call(t)
		if verified != nil {
			t.Errorf("a group info with %s was VOUCHED FOR; the cache would bind to epoch %d of group %x on an attacker's say so",
				row.name, verified.Context().Epoch, verified.Context().GroupId)
		}
		if !errors.Is(err, row.sentinel) {
			t.Errorf("a group info with %s answered %v, want %v", row.name, err, row.sentinel)
		}
	}
}

// TestAVerifiedContextIsNotChangeableThroughWhatItHandsBack is the aliasing half, asked at both
// ends: what a holder is handed, and what the caller that supplied the group info still holds.
//
// The value a cache binds to must not move under it. Context answers a Clone, so a caller that
// rewrites the group id of what it was handed rewrites its own copy; and the constructor decodes
// the octets the signature covered, so the caller's own GroupInfo is not the storage the verified
// value vouches for either. A design missing the second half would be one where an attacker that
// got a GroupInfo verified could then edit the group out from under the cache that trusted it.
func TestAVerifiedContextIsNotChangeableThroughWhatItHandsBack(t *testing.T) {
	crypto := testCrypto(t)
	tree, members := newTestTree(t, crypto, 2)
	context := &GroupContext{
		Version:                 ProtocolVersionMls10,
		CipherSuite:             CipherSuiteX25519ChaCha20Sha256Ed25519,
		GroupId:                 []byte("a group"),
		Epoch:                   5,
		ConfirmedTranscriptHash: bytes.Repeat([]byte{0x71}, crypto.HashSize()),
	}
	info := testSignedGroupInfoOver(t, crypto, tree, members, LeafIndex(0), context)
	signedTreeHash := bytes.Clone(info.GroupContext.TreeHash)
	verified, err := info.VerifiedContext(crypto, tree)
	if err != nil {
		t.Fatalf("VerifiedContext: %v", err)
	}

	handed := verified.Context()
	handed.Epoch = 99
	handed.GroupId[0] ^= 0xff
	handed.TreeHash[0] ^= 0xff
	again := verified.Context()
	if again.Epoch != 5 {
		t.Errorf("a write through what Context answered moved the verified epoch to %d", again.Epoch)
	}
	if !bytes.Equal(again.GroupId, []byte("a group")) {
		t.Errorf("a write through what Context answered moved the verified group id to %x", again.GroupId)
	}
	if !bytes.Equal(again.TreeHash, signedTreeHash) {
		t.Errorf("a write through what Context answered moved the verified tree hash to %x", again.TreeHash)
	}

	// and the group info the caller still holds is not the storage either, since the
	// constructor decoded the octets the signature covered rather than keeping the argument
	info.GroupContext.Epoch = 98
	info.GroupContext.GroupId[0] ^= 0xff
	if verified.Context().Epoch != 5 {
		t.Error("a write through the caller's own group info moved the verified epoch, so the verified value aliases the structure it was built from")
	}
	if !bytes.Equal(verified.Context().GroupId, []byte("a group")) {
		t.Errorf("a write through the caller's own group info moved the verified group id to %x",
			verified.Context().GroupId)
	}
}

// TestTheZeroVerifiedGroupContextIsRefusedAtEveryDoor is the one shape another package can spell.
//
// mls.VerifiedGroupContext{} compiles anywhere, because a composite literal with no elements needs
// no access to the field. It carries nil, so what makes that harmless is not the type system but
// the refusal at each door, and this is what says every door has one.
func TestTheZeroVerifiedGroupContextIsRefusedAtEveryDoor(t *testing.T) {
	zero := &VerifiedGroupContext{}
	if got := zero.Context(); got != nil {
		t.Errorf("the zero verified context answered %+v, want nil; a value that vouches for nothing must say so rather than describing the empty group at epoch 0",
			got)
	}
	if _, err := NewProposalCache(zero); !errors.Is(err, ErrNilGroupContext) {
		t.Errorf("NewProposalCache(the zero verified context) = %v, want ErrNilGroupContext; a cache built from it would be bound to nothing, which is the one state in which a message can supply the epoch",
			err)
	}
	if err := testCache(t).Rebind(zero); !errors.Is(err, ErrNilGroupContext) {
		t.Errorf("Rebind(the zero verified context) = %v, want ErrNilGroupContext; a rebind that accepted it would empty the cache and leave it bound to nothing",
			err)
	}
	if got := (*VerifiedGroupContext)(nil).Context(); got != nil {
		t.Errorf("Context on no value at all answered %+v, want nil", got)
	}
}

// TestACacheBindsToTheEpochItsVerifiedContextNames joins this type to the thing it exists for.
//
// The cache's binding is unexported and this is in the same package, so the two fields it compares
// are read directly rather than inferred from a refusal: a test that only observed CheckEpoch would
// pass over a constructor that bound to the right epoch of the wrong group.
func TestACacheBindsToTheEpochItsVerifiedContextNames(t *testing.T) {
	context := testResolveContextAt([]byte("bound here"), 11)
	cache, err := NewProposalCache(testVerifiedContextAt(t, context))
	if err != nil {
		t.Fatalf("NewProposalCache over a verified context: %v", err)
	}
	if cache.binding == nil {
		t.Fatal("a cache built over a verified context is bound to nothing")
	}
	if !bytes.Equal(cache.binding.groupId, context.GroupId) || cache.binding.epoch != context.Epoch {
		t.Errorf("the cache bound itself to epoch %d of group %x, want epoch %d of group %x",
			cache.binding.epoch, cache.binding.groupId, context.Epoch, context.GroupId)
	}
	// and the binding does not alias the verified value's own storage
	verified := testVerifiedContextAt(t, context)
	rebound, err := NewProposalCache(verified)
	if err != nil {
		t.Fatalf("NewProposalCache over a second verified context: %v", err)
	}
	verified.inner.GroupId[0] ^= 0xff
	if !bytes.Equal(rebound.binding.groupId, context.GroupId) {
		t.Errorf("the binding followed a write into the verified context's own array to %x; a binding aliased to storage somebody else holds agrees with whatever that storage later says",
			rebound.binding.groupId)
	}
}
