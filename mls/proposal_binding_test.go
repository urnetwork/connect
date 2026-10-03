// Where the proposal cache's epoch binding is allowed to come from, derived off the source.
//
// THE RULE: a ProposalCache's binding -- the group and epoch it belongs to -- may only be written
// from a *GroupContext the writing declaration was HANDED. Not from a message, not from a cache
// entry, not from a constant, and not from anything the declaration read off its own receiver. A
// write that reads nothing the caller named is a violation too, because that is the unbound state
// and the unbound state is where a message used to be able to supply the epoch.
//
// It is a gate rather than a paragraph because this defect has been written twice. The first
// version took the binding from the first entry STORED and checked it against that same entry's
// own epoch, which is self referential and therefore no check at all; the fix for that gave
// Resolve a group context to observe and left the binding exactly where it was, so replaying one
// genuine proposal of a closed epoch into a freshly cleared cache bound the member to the closed
// epoch permanently -- it could then cache nothing of its live epoch and resolve no commit that
// named a proposal, and nothing released it. Both times the defect was one assignment, both times
// its right hand side read content.Content.Epoch, and both times the package was green.
//
// What separates the safe shape from the unsafe one is not visible in the target of the write --
// both spell the same field -- and it is not visible in any signature. It is where the value on
// the RIGHT came from. So that is what this reads, over the compiler's own objects rather than
// over spellings, and it walks the two ways a Go field can be written rather than the one that
// happens to be in the source today: an assignment, and a composite literal.
//
// THE SCOPE IS THIS PACKAGE AND THAT IS A COMPILER FACT rather than a choice. The binding lives
// behind unexported fields of an unexported type, so no declaration outside package mls can write
// it however it is spelled; the gate asserts the fields really are unexported instead of leaving
// that as the reason a narrow scan is enough.
//
// AND THAT RULE IS ABOUT THE WRITE, WHICH IS ONLY HALF OF WHERE THE VALUE CAME FROM. The other
// half -- where the CALLER got the value it handed over -- used to live in this file as a second
// gate, an AST walk over every call of a binding writer that refused an argument whose chain of
// selections reached a type this package decodes. It was written three times and bypassed three
// times: by any *GroupContext at all, then by one local struct between the decode and the bind,
// then twice over by an accessor method returning an inner field and by an embedded wire type
// whose promoted selection the walk could not see. That gate is GONE, and it was deleted rather
// than sharpened. Three bypasses in three rounds is not bad luck; it is evidence that provenance
// -- where a value came from -- is not a property of the source shape a walk can read, and each
// round's remedy was a shape the next reader wrote by following the type doc's own advice.
//
// What answers it instead is the compiler. NewProposalCache and Rebind take a
// *VerifiedGroupContext, a type whose only field is unexported and whose only constructor, for
// every package but this one, is (*GroupInfo).VerifiedContext -- which answers the group context of
// a GroupInfo only once a member of the ratchet tree the CALLER holds has been shown to have signed
// it, under a signature checked against a key that came out of that tree. Every bypass listed above now fails to COMPILE rather than
// failing to be spotted. group_context_verified.go carries the argument, and
// group_context_verified_test.go holds the two questions that ARE properties of source shape:
// which declarations of this package construct that type, and which hand its contents back out.
//
// SO WHAT IS LEFT HERE IS THE WRITE, and it is still worth a gate for the reason it always was.
// The type says the value handed in has authority; it says nothing about which of the fields in
// scope the write actually read, and both times this defect shipped the write read a message's
// epoch with a group context sitting unread in the same signature. That is what the rule below
// refuses, and it refuses it over the compiler's objects rather than over spellings.
package mls

import (
	"go/ast"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The three names this file's derivations are written against, checked against the compiler's
// reading below rather than trusted: a rename that left these behind would empty the class, and an
// empty class is what a package with no binding writes and a broken matcher both look like.
const (
	proposalBindingTypeName    = "proposalCacheBinding"
	proposalBindingFieldName   = "binding"
	proposalBindingContextType = "VerifiedGroupContext"
)

// proposalBindingWrite is one write of a binding: where it is, how it is spelled, and what the
// value it wrote was read out of.
type proposalBindingWrite struct {
	where string
	how   string
	// the parameters and receiver of the enclosing declaration that the written VALUE reads,
	// each with whether the compiler reads it as a group context. A write is accepted when
	// this holds at least one group context and nothing else.
	reads map[string]bool
}

// fromTheCallersGroupContext is the demand, stated over one write.
//
// Both halves. "Reads a group context" alone would accept a body that read a context and a message
// and wrote the message's epoch, which is the defect with a witness standing next to it. "Reads
// nothing else" alone would accept a write of a constant, which is the unbound state.
func (self proposalBindingWrite) fromTheCallersGroupContext() bool {
	held := false
	for _, isContext := range self.reads {
		if !isContext {
			return false
		}
		held = true
	}
	return held
}

// proposalBindingTarget is one written field with the expression that supplied its value.
type proposalBindingTarget struct {
	node  ast.Node
	value ast.Expr
}

// proposalBindingTargetsOf answers the binding writes one statement or expression makes.
//
// TWO WAYS A FIELD IS WRITTEN, not one. An assignment is the shape the defect took both times, and
// a matcher that read only assignments would report a clean run over a constructor that built the
// whole binding in a composite literal -- which is exactly how NewProposalCache builds it, so the
// half this gate exists to hold would be the half it could not see. The increment form carries no
// right hand side at all and is included for the same reason the epoch mover gate includes it: it
// is the cheapest way there is to move an epoch.
func proposalBindingTargetsOf(checked checkedBodies, node ast.Node) []proposalBindingTarget {
	targets := []proposalBindingTarget{}
	writes := func(target ast.Expr) bool {
		selector := epochWriteTarget(target)
		if selector == nil {
			return false
		}
		// a field of the binding itself -- self.binding.epoch = x
		if extensionTypeSelectionNamedAs(checked.info.TypeOf(selector.X), proposalBindingTypeName) {
			return true
		}
		// or the binding a cache holds -- self.binding = x
		return selector.Sel.Name == proposalBindingFieldName &&
			extensionTypeSelectionNamedAs(checked.info.TypeOf(selector.X), epochCacheTypeName)
	}
	switch statement := node.(type) {
	case *ast.AssignStmt:
		for i, target := range statement.Lhs {
			if !writes(target) {
				continue
			}
			// a multi valued right hand side is one expression for every target, so
			// the whole of it is what the write reads
			value := statement.Rhs[0]
			if len(statement.Rhs) == len(statement.Lhs) {
				value = statement.Rhs[i]
			}
			targets = append(targets, proposalBindingTarget{node: statement, value: value})
		}
	case *ast.IncDecStmt:
		if writes(statement.X) {
			// no value at all: an increment reads the field it writes, which is not a
			// group context however the declaration was called
			targets = append(targets, proposalBindingTarget{node: statement, value: nil})
		}
	case *ast.CompositeLit:
		literalOfBinding := extensionTypeSelectionNamedAs(checked.info.TypeOf(statement), proposalBindingTypeName)
		literalOfCache := extensionTypeSelectionNamedAs(checked.info.TypeOf(statement), epochCacheTypeName)
		if !literalOfBinding && !literalOfCache {
			return targets
		}
		for _, element := range statement.Elts {
			keyed, isKeyed := element.(*ast.KeyValueExpr)
			if !isKeyed {
				continue
			}
			key, isName := keyed.Key.(*ast.Ident)
			if !isName {
				continue
			}
			if literalOfCache && key.Name != proposalBindingFieldName {
				continue
			}
			targets = append(targets, proposalBindingTarget{node: keyed, value: keyed.Value})
		}
	}
	return targets
}

// proposalBindingWritesIn is the rule: every declaration of one checked package that writes the
// cache's binding, with what each write read.
//
// The target count is carried for the reason the other derived gates of this package carry a read
// count. A matcher that stopped resolving its subject reports an EMPTY class, and an empty class is
// exactly what a package that writes no binding reports -- so "nothing writes it" and "nothing was
// read" have to be distinguishable, and only the second is a broken gate.
func proposalBindingWritesIn(checked checkedBodies) (map[string][]proposalBindingWrite, int) {
	found := map[string][]proposalBindingWrite{}
	targets := 0
	for _, file := range checked.files {
		for _, declaration := range file.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil {
				continue
			}
			name := extensionTypeSelectionDeclarationName(checked, function)
			handed := extensionTypeSelectionHandedTo(checked, function)
			ast.Inspect(function.Body, func(node ast.Node) bool {
				for _, target := range proposalBindingTargetsOf(checked, node) {
					targets += 1
					write := proposalBindingWrite{
						where: checked.where(target.node),
						how:   checked.render(target.node),
						reads: map[string]bool{},
					}
					if target.value != nil {
						ast.Inspect(target.value, func(inner ast.Node) bool {
							name, isName := inner.(*ast.Ident)
							if !isName {
								return true
							}
							// the compiler's object and not the
							// spelling, so a local shadowing a
							// parameter is a different thing
							object := checked.info.Uses[name]
							if object == nil || !handed[object] {
								return true
							}
							write.reads[name.Name] = extensionTypeSelectionNamedAs(
								object.Type(), proposalBindingContextType)
							return true
						})
					}
					found[name] = append(found[name], write)
				}
				return true
			})
		}
	}
	return found, targets
}

// proposalBindingControl is a package the matcher has never seen, written so that every half of
// the rule has something to report and something to walk past.
//
// It carries the two defect shapes the real source must never hold again -- the binding built out
// of a message, and one field of it moved out of a message while a group context sits unread in
// the signature -- because those are what this gate demands, and the real source will not hold one
// unless somebody writes it. A control rather than a second opinion about the real source: a
// matcher that resolved nothing reports a clean run over mls too, and the only way to tell that
// apart from a package that obeys the rule is to run it on source known to hold both answers.
const proposalBindingControl = `package control

// the type the real writers take: a group context whose authority has been established, which
// this control declares as its own so that the matcher is read over the same type name the real
// source uses rather than over one this file happens to spell
type VerifiedGroupContext struct {
	GroupId []byte
	Epoch   uint64
}

// what a message carries, and it carries exactly the same two fields, which is the whole reason
// this rule is about where a value came from rather than about what it looks like
type Content struct {
	GroupId []byte
	Epoch   uint64
}

type proposalCacheBinding struct {
	groupId []byte
	epoch   uint64
}

type ProposalCache struct {
	byRef   map[string]int
	binding *proposalCacheBinding
}

// the accepted shape, built in a composite literal
func NewFromTheContext(groupContext *VerifiedGroupContext) *ProposalCache {
	return &ProposalCache{
		byRef:   map[string]int{},
		binding: &proposalCacheBinding{groupId: groupContext.GroupId, epoch: groupContext.Epoch},
	}
}

// the accepted shape, written as an assignment
func (self *ProposalCache) rebindFromTheContext(groupContext *VerifiedGroupContext) {
	self.binding = &proposalCacheBinding{groupId: groupContext.GroupId, epoch: groupContext.Epoch}
}

// the defect: the binding taken from what arrived
func (self *ProposalCache) bindFromTheMessage(content *Content) {
	self.binding = &proposalCacheBinding{groupId: content.GroupId, epoch: content.Epoch}
}

// the defect with a witness beside it: a group context in the signature, unread, and the epoch
// still taken from the message
func (self *ProposalCache) moveTheEpochFromTheMessage(groupContext *VerifiedGroupContext, content *Content) {
	self.binding.epoch = content.Epoch
}

// the release that leaves the cache bound to nothing, which is the state a replay used to fill
func (self *ProposalCache) release() {
	self.binding = nil
}

// the cheapest move there is, and it is not an assignment
func (self *ProposalCache) bumpTheBoundEpoch() {
	self.binding.epoch++
}

// a write of what the cache HOLDS, which is not a write of what it belongs to
func (self *ProposalCache) store(key string, at int) {
	self.byRef[key] = at
}

// and a read of the binding, which is not a write of it
func (self *ProposalCache) boundEpoch() uint64 {
	return self.binding.epoch
}
`

// What the matcher must report out of the control, and by omission what it must walk past.
var proposalBindingControlWriters = []string{
	"(*ProposalCache).bindFromTheMessage",
	"(*ProposalCache).bumpTheBoundEpoch",
	"(*ProposalCache).moveTheEpochFromTheMessage",
	"(*ProposalCache).rebindFromTheContext",
	"(*ProposalCache).release",
	"NewFromTheContext",
}

// And which of them it must refuse. rebindFromTheContext and NewFromTheContext are the two shapes
// the real source is written in, so a rule that refused either would refuse the fix as well as the
// defect.
var proposalBindingControlRefused = []string{
	"(*ProposalCache).bindFromTheMessage",
	"(*ProposalCache).bumpTheBoundEpoch",
	"(*ProposalCache).moveTheEpochFromTheMessage",
	"(*ProposalCache).release",
}

// The declarations of this package that write the binding.
//
// Two, and the gate below holds the derived class EQUAL to this in both directions, so a third
// write fails here until somebody says what it is. That is the point: the defect was a write added
// to a method that had every reason to be writing something, and a class that only checked the
// writes it already knew about would not have seen it.
var proposalCacheBindingWriteSites = map[string]string{
	"NewProposalCache": "builds the binding out of the context the caller is in. It is a composite literal " +
		"rather than a field assignment so that the constructor is not a second spelling of Rebind's write",
	"(*ProposalCache).Rebind": "moves the binding to the context the caller names, which is what an epoch " +
		"boundary owes the cache. It is the only write after construction, and it cannot release without " +
		"rebinding -- a cache bound to nothing is where taking the epoch from a message used to be possible",
}

// TestEveryWriteOfTheCacheBindingReadsTheCallersGroupContext is the root cause stated as a gate.
//
// Not "Store does not write the binding" -- that is a fact about one method, and the next method
// is the one that will. The class is every write of the binding there is, and the demand on each
// is about where its value came from.
func TestEveryWriteOfTheCacheBindingReadsTheCallersGroupContext(t *testing.T) {
	// the compiler enforced half of the scope: unexported fields of an unexported type cannot
	// be written from another package however the write is spelled, which is why scanning this
	// package alone is enough rather than merely convenient
	cache, held := reflect.TypeOf(ProposalCache{}).FieldByName(proposalBindingFieldName)
	if !held {
		t.Fatalf("%s no longer declares a %s field, so this gate is written against a struct that has changed shape",
			epochCacheTypeName, proposalBindingFieldName)
	}
	if cache.IsExported() {
		t.Fatalf("%s.%s is exported, so a declaration in any package can write it and a scan of this one is not the class",
			epochCacheTypeName, proposalBindingFieldName)
	}
	binding := reflect.TypeOf(proposalCacheBinding{})
	if binding.NumField() == 0 {
		t.Fatalf("%s declares no field, so there is no binding for this gate to be about", proposalBindingTypeName)
	}
	for i := 0; i < binding.NumField(); i += 1 {
		if binding.Field(i).IsExported() {
			t.Fatalf("%s.%s is exported, so the binding can be written from outside this package",
				proposalBindingTypeName, binding.Field(i).Name)
		}
	}

	// the control first, which is what says the matcher reads anything at all
	control := typeCheckedBodiesOfText(t, "the proposal binding control", proposalBindingControl)
	written, targets := proposalBindingWritesIn(control)
	if targets == 0 {
		t.Fatal("the matcher resolved no binding write in the control, so it would report a clean run over any package at all")
	}
	if reported := slices.Sorted(maps.Keys(written)); !slices.Equal(reported, proposalBindingControlWriters) {
		t.Fatalf("the rule read %v out of the control as writing the binding, want %v; one that reads only assignments walks past the constructor, and one that reads only the cache's own field walks past a write of the binding's",
			reported, proposalBindingControlWriters)
	}
	refused := []string{}
	for name, writes := range written {
		for _, write := range writes {
			if !write.fromTheCallersGroupContext() {
				refused = append(refused, name)
				break
			}
		}
	}
	slices.Sort(refused)
	if !slices.Equal(refused, proposalBindingControlRefused) {
		t.Fatalf("the rule refused %v of the control, want %v; one that accepts a value read off a message accepts the defect, and one that refuses a value read off the caller's context refuses the fix",
			refused, proposalBindingControlRefused)
	}

	// and then the real source
	checked := typeCheckedBodiesOf(t, ".")
	if len(checked.paths) == 0 {
		t.Fatal("this package holds no non test source, so the gate scanned nothing")
	}
	found, realTargets := proposalBindingWritesIn(checked)
	if realTargets == 0 {
		t.Fatalf("no write of the cache binding was resolved in this package, and %s certainly holds several, so this gate is not reading what it claims to",
			epochCacheTypeName)
	}
	classified := slices.Sorted(maps.Keys(proposalCacheBindingWriteSites))
	if names := slices.Sorted(maps.Keys(found)); !slices.Equal(names, classified) {
		t.Fatalf("%v write the cache binding and this table names %v; a write with no row is one nobody said where the value comes from, and a row with no write is a claim that outlived what it described",
			names, classified)
	}
	for _, name := range slices.Sorted(maps.Keys(found)) {
		for _, write := range found[name] {
			if write.fromTheCallersGroupContext() {
				t.Logf("%s at %s writes %q out of %v", name, write.where, write.how,
					slices.Sorted(maps.Keys(write.reads)))
				continue
			}
			t.Errorf("%s at %s writes the cache binding as %q, out of %v; a binding is only worth the authority of the value it was taken from, and the only value with that authority is a *%s the caller handed in -- which is a type whose only constructor confirmed it. Reading it off a message is the defect this gate exists for, and reading it off nothing leaves the cache unbound, which is the state a message could then fill",
				name, write.where, write.how, slices.Sorted(maps.Keys(write.reads)),
				proposalBindingContextType)
		}
	}
}

// TestNoDeclarationOfThisPackageReadsAStoredEpochIntoTheCacheBinding is the same rule read from the
// other end, and it is here because the gate above can only refuse what it can attribute.
//
// The write it refuses is one it found. A body that copied a message's epoch into a local first and
// wrote the local would read no parameter at all at the write itself, so the gate above would call
// it an unbound write -- which it would still refuse, but for the wrong reason and with a report a
// reader could not act on. This states the fact the fix actually rests on in the plainest form
// there is: the type that owns the binding names no message type at all in any of the declarations
// that write it.
func TestNoDeclarationOfThisPackageReadsAStoredEpochIntoTheCacheBinding(t *testing.T) {
	checked := typeCheckedBodiesOf(t, ".")
	found, _ := proposalBindingWritesIn(checked)
	if len(found) == 0 {
		t.Fatal("no declaration writes the cache binding, so this reads nothing")
	}
	for _, file := range checked.files {
		for _, declaration := range file.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil {
				continue
			}
			name := extensionTypeSelectionDeclarationName(checked, function)
			if _, writes := found[name]; !writes {
				continue
			}
			// every parameter of a declaration that writes the binding, by the
			// compiler's reading of its type. A body cannot read what it was not handed,
			// so a signature holding nothing but a group context cannot take an epoch off
			// a message however it is written inside.
			for handed := range extensionTypeSelectionHandedTo(checked, function) {
				rendered := handed.Type().String()
				if strings.Contains(rendered, proposalBindingContextType) ||
					strings.Contains(rendered, epochCacheTypeName) {
					continue
				}
				t.Errorf("%s writes the cache binding and is handed %s of type %s; the binding is only as good as the authority of the values in scope where it is written, and a message in that scope is one local away from being the epoch this cache believes it is in",
					name, handed.Name(), rendered)
			}
		}
	}
}
