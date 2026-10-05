// The derived answer to "who clears the proposal cache when the epoch advances".
//
// The rule: a ProposalCache belongs to one epoch of one group, and a cache that outlives that
// epoch answers the closed epoch's references to a commit of the new one. Resolve refuses that at
// the lookup now -- errProposalResolvedOutOfEpoch, proposal_list.go -- so the group cannot apply a
// replayed proposal even when nobody cleared. What is still needed, and what this file derives, is
// that the boundary itself is not left to memory: every declaration that moves a group to another
// epoch has to END the binding, or a member spends the whole of the new epoch unable to resolve
// any commit that names a proposal, and the first fix anybody reaches for is to weaken the door.
//
// Why it is derived rather than written down. The mitigation on offer was `self.proposals.Clear()`
// at two hand written call sites -- Rebind's ancestor, and the shape of the call is not the point --
// which is an enumeration of the paths that advance an epoch --
// the shape rule 5 exists to refuse, and the shape that has understated its class fourteen times
// on this project: a constant time gate banning six comparator names that missed bytes.HasPrefix
// where the derived version finds eighteen; a table calling itself "every rule of the CreateGroup
// carve-out" that held five of six. Two call sites is a list of the epoch boundaries somebody had
// thought of. p7 has fifteen tasks left, and MergePendingCommit, a rollback, a state restore and a
// re-join are all shaped like an epoch boundary.
//
// So the class is what a declaration DOES. A declaration moves a group between epochs when it
// writes a GroupContext, or one of the two fields the cache binds to, into storage that outlives
// the call: `self.context = staged.context` and `self.context.Epoch++` are the two spellings, and
// both are assignments to a SELECTOR. An assignment to a bare identifier is excluded on purpose --
// a local or a parameter is a value the declaration is constructing, and every commit path in this
// plan builds its next GroupContext as a local before anything is allowed near the live group.
// That is the same line the erase helper gate draws between the caller's storage and the
// function's own, for the same reason.
//
// AND ENDING THE BINDING IS A DISCIPLINE, NOT A TOKEN. This gate used to answer "does a call to a
// binding ender appear anywhere in this body", which three measured shapes satisfy while leaving
// the cache bound to the closed epoch:
//
//   - `if false { self.proposals.Rebind(c) }`. The call is in the body and no path runs it.
//   - the write, then `if err := self.persist(); err != nil { return err }`, then the rebind. Every
//     failing persist returns with the group moved and the cache bound to the epoch that closed.
//     That is not hypothetical: p7 writes the rebind and the persist adjacent, and SWAPPING TWO
//     ADJACENT STATEMENTS is the whole of the defect.
//   - `self.pending.Rebind(c)` where the group holds a second cache. The receiver is a
//     ProposalCache, so a rule that resolved the ender by TYPE accepted it; the cache the epoch
//     actually bound went on holding the closed epoch. Latent while Group holds one cache and live
//     the moment p7 task 19 adds a staged or past-epoch one, which is what task 19 is for.
//   - the rebind handed the context that is about to CLOSE, ahead of the write that replaces it.
//     Every path runs it, it is the right cache, and it binds the cache to the epoch the group is
//     leaving. The member is then wedged for the whole of the new epoch: Store answers
//     errProposalCacheNotRebound, Pending answers nothing, and Resolve answers
//     errProposalResolvedOutOfEpoch for every reference a commit names. Because it cannot resolve
//     that commit it never reaches the next boundary, so the next Rebind never runs, and nothing
//     in this package heals it. That is the same permanent lockout the binding was rewritten to
//     remove, reached from the caller's side instead of from a peer's -- not peer triggerable,
//     which is exactly what a gate of this kind is for.
//
// So the rule below is stated over PATHS, over IDENTITY, and over the ARGUMENT. Every path that
// leaves a declaration having made the write must have ended the binding of every cache the moved
// value holds -- the caches read off the value's own struct rather than named, so a second cache
// is a new obligation rather than a new way to pass -- and every ending it is credited with must
// have been handed the group context THIS BODY MOVED, at a point where the move had already
// happened. epochWalk is the reading: an abstract walk of the body carrying "some path here has
// moved a group, and these are the expressions that now name it" as a MAY and "every path here has
// ended this cache" as a MUST, joined at every branch and checked at every exit. It is
// deliberately conservative -- a clear inside a loop, a goroutine, a function literal or a jump is
// one this rule refuses to call guaranteed, and so is one handed a context it cannot name --
// because the direction a gate is allowed to be wrong in is the loud one.
//
// WHAT IS TRUE TODAY, said out loud rather than hidden by a green run. Not one declaration of
// either scanned root is classified as moving a group between epochs: the only member of the
// class is the group context DECODER, which writes an epoch it read out of the caller's own
// octets. So the second half of this gate -- every mover ends the binding, with the context it
// moved, after it moved it -- runs on the control package and on nothing else, and the control is
// therefore not decoration. It is the whole of the evidence that the demand exists at all, and the
// commit that lands MergePendingCommit is the one that will be met by it. Adding the argument half
// did not change that: the mover class over the real source still has exactly one member and it is
// still classified as moving no group, so this gate demands nothing of this package today. Every arm of every matcher below has a control member that changes its
// answer when the arm is removed; an arm with no such member is prose, which is rule 5 one level
// down and is the defect this file was last corrected for.
package mls

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect/mls/syntax"
)

// The three names this file's derivations are written against, and the one place they are
// spelled. Each is checked against the compiler's reading below rather than trusted: a rename that
// left these behind would empty both classes, and an empty class is what a package with no movers
// and a broken matcher both look like.
const (
	epochGroupContextTypeName = "GroupContext"
	epochCacheTypeName        = "ProposalCache"
)

// The fields of a GroupContext the proposal cache binds itself to.
//
// Both, and never the epoch alone: every group this client is a member of runs an epoch 7, so a
// declaration that rewrote a live context's group id in place has moved the group as surely as one
// that bumped its epoch, and the cache bound to the old pair answers references belonging to
// neither. bindingHolds compares exactly these two.
var epochBoundFields = []string{"GroupId", "Epoch"}

// epochMoverRoots is where the rule is stated: it IS forbiddenScanRoots, and not a copy.
//
// An alias for the reason extensionTypeSelectionRoots is one -- a restatement is held by nothing,
// and narrowing forbiddenScanRoots itself fails TestHkdfExtractHasOnlyTwoCallSites, so borrowing
// that value borrows a scope something already pins. ../message declares no GroupContext today and
// cannot: it imports mls/syntax and not mls, and mls must not import it back. That is not a reason
// to leave it out. A scope that covers only what is already written is a scope that stops covering
// the first thing added, and the group lifecycle is the layer most likely to grow a second holder
// of a group's epoch.
var epochMoverRoots = forbiddenScanRoots

// epochMoverFinding is one declaration's membership: where the write is, how it is written, the
// ender calls the rule FOUND over the moved value's own caches, the ones handed a context this
// body had not moved, the ones it accepts as running on every path out, and the paths that leave
// with a cache still bound.
//
// finds, wrong and ends are carried apart because a declaration that fails this gate fails in
// three very different ways, and a reader who cannot tell them apart cannot act. "No ender was
// found at all" is a boundary nobody wrote a rebind for; "an ender was found and no path is
// guaranteed to reach it" is a rebind written in the wrong place, which is a two line fix and is
// the shape p7 has; and "an ender was found and handed something else" is a rebind written with
// the wrong argument, which answers every other question this gate asks correctly and leaves the
// member wedged anyway. skipped names the paths, so the report is a location rather than a
// verdict.
type epochMoverFinding struct {
	where   string
	how     string
	ends    string
	finds   string
	wrong   string
	skipped string
}

// epochMoverScan is what one scan read: the declarations that move a group between epochs, and how
// many field writes were resolved at all.
//
// The write count is carried for the reason extensionTypeSelectionScan carries its read count. A
// matcher that stopped resolving its subject reports an EMPTY class, and an empty class is exactly
// what a package with no movers reports -- so "nothing moves" and "nothing was read" have to be
// distinguishable, and only the second is a broken gate.
type epochMoverScan struct {
	moving map[string]epochMoverFinding
	writes int
}

// epochWriteTarget unwraps an assignment's left hand side to the selector it ultimately writes
// through.
//
// A map insert is a write of what the container holds -- `self.byRef[key] = entry` changes the
// cache as surely as `self.byRef = nil` does -- and so is a store through a pointer. A matcher
// that read only the bare selector form would report a clean run over a method that did all of its
// writing through an index expression, which is the shape (*ProposalCache).Store is half written
// in, and over one that overwrote the context a group already holds rather than replacing the
// pointer to it. Both arms carry a control member that stops being reported when the arm goes:
// (*ProposalCache).storeThroughTheIndex for the index and (*Group).overwriteThroughThePointer for
// the dereference. An arm with no such member is a sentence, not a rule.
func epochWriteTarget(expr ast.Expr) *ast.SelectorExpr {
	for {
		expr = extensionTypeSelectionUnparenthesised(expr)
		switch node := expr.(type) {
		case *ast.IndexExpr:
			expr = node.X
		case *ast.StarExpr:
			expr = node.X
		case *ast.SelectorExpr:
			return node
		default:
			return nil
		}
	}
}

// epochAssignedTargets answers the left hand sides one statement writes, for the two statement
// kinds that write: an assignment and an increment.
//
// `self.context.Epoch++` carries no right hand side and is not an *ast.AssignStmt at all, and a
// rule that read only assignments would walk past the cheapest way there is to advance an epoch.
func epochAssignedTargets(node ast.Node) []ast.Expr {
	switch statement := node.(type) {
	case *ast.AssignStmt:
		return statement.Lhs
	case *ast.IncDecStmt:
		return []ast.Expr{statement.X}
	}
	return nil
}

// epochRootObject answers the object a selector chain is ultimately written through or called on:
// the value that HOLDS the storage, rather than the field being touched.
//
// It is what separates `self.proposals.Clear()` from `other.proposals.Clear()` -- the same field of
// the same type, over a different value, which resolving the ender by type cannot tell apart and
// which (*Group).mergeAndClearAnotherGroupsCache is the control for. The selector arm is what
// carries `self.context.Epoch++` back to `self` rather than to the context, and
// (*Group).bumpTheEpochAndClear is the control that stops being an ender when it goes.
func epochRootObject(checked checkedBodies, expr ast.Expr) types.Object {
	for {
		switch node := expr.(type) {
		case *ast.SelectorExpr:
			expr = node.X
		case *ast.Ident:
			return checked.info.Uses[node]
		default:
			return nil
		}
	}
}

// epochStructOf answers the struct a value's type is, through any number of pointers.
//
// Through the pointers because every holder of a group in this plan is reached as one, and the
// underlying type of *Group is a pointer rather than a struct: a matcher that asked Underlying()
// directly would find no fields on any receiver in the package and would then report that no
// declaration holds a cache to rebind, which reads exactly like a package that keeps none.
func epochStructOf(found types.Type) *types.Struct {
	for {
		if found == nil {
			return nil
		}
		pointer, isPointer := found.(*types.Pointer)
		if !isPointer {
			break
		}
		found = pointer.Elem()
	}
	structure, isStruct := found.Underlying().(*types.Struct)
	if !isStruct {
		return nil
	}
	return structure
}

// epochCachesHeldBy answers the cache fields the values a declaration moved actually hold, read
// off their own struct.
//
// THE OBLIGATION IS DERIVED FROM THE TYPE, so a group that grows a second cache grows a second
// thing an epoch boundary owes, with no edit here. That is the whole of the answer to "clearing a
// different ProposalCache satisfied the gate": there is no "the" cache any more, there is every
// cache the moved value declares, and a boundary that rebinds one of two fails. The consequence is
// deliberate and it is loud rather than silent -- when p7 task 19 adds a staged or past-epoch
// cache, every mover fails here until somebody writes down what the boundary owes that one, which
// is the decision this gate exists to force rather than one it should quietly make.
func epochCachesHeldBy(checked checkedBodies, bases map[types.Object]bool, cacheType string) map[types.Object]bool {
	held := map[types.Object]bool{}
	for base := range bases {
		structure := epochStructOf(base.Type())
		if structure == nil {
			continue
		}
		for at := 0; at < structure.NumFields(); at++ {
			field := structure.Field(at)
			if extensionTypeSelectionNamedAs(field.Type(), cacheType) {
				held[field] = true
			}
		}
	}
	return held
}

// epochGroupMove is what one statement moved: how it is written, the value that HOLDS the storage
// written -- which is the value whose caches the boundary owes a rebind -- and the expressions
// that NAME the moved group context once the write has landed.
//
// The names are what the argument half of the rule is checked against, and there are two of them
// rather than one because a boundary writes `self.context = staged` and may then hand the ender
// either side of that assignment: `self.context` is the storage it wrote and `staged` is the value
// it wrote, and the instant the write lands the two are one context. Both are accepted, everything
// else is refused, and `self.context` handed BEFORE the write is refused by the WALK rather than
// here -- because that is a question about paths and not about spelling.
type epochGroupMove struct {
	how   string
	base  types.Object
	names [][]types.Object
}

// epochDenotationOf answers the chain of objects an expression names a group context through:
// `self.context` is [self, context] and `staged` is [staged].
//
// Over the type checker's OBJECTS and not over the rendered text, for the reason every other
// derivation in this file is over them: two declarations may both spell a field `context`, and a
// rule comparing spellings would read one group's context as another's --
// (*Group).mergeAndClearWithAnotherGroupsContext is the control member that says so, and its
// argument is spelled `other.context` precisely so a text comparison would have to work to tell it
// apart from `self.context`.
//
// A dereference and an address-of name the same storage as their operand, so both are followed:
// `*self.slot` is the context `self.slot` points at, which is what
// (*Group).overwriteThroughThePointer writes and what its clear is handed. Anything else -- a
// call's result, an index, a conversion -- answers nil and is REFUSED rather than guessed at,
// because an ender handed a context this rule cannot name is one a reader cannot check either.
// (*Group).mergeAndClearWithAContextItBuiltAfterwards is that shape: a rebind to a context the
// declaration constructed after the move, which is not the epoch the group is now in.
func epochDenotationOf(checked checkedBodies, expr ast.Expr) []types.Object {
	if expr == nil {
		return nil
	}
	switch node := extensionTypeSelectionUnparenthesised(expr).(type) {
	case *ast.StarExpr:
		return epochDenotationOf(checked, node.X)
	case *ast.UnaryExpr:
		if node.Op != token.AND {
			return nil
		}
		return epochDenotationOf(checked, node.X)
	case *ast.SelectorExpr:
		outer := epochDenotationOf(checked, node.X)
		field := checked.info.Uses[node.Sel]
		if outer == nil || field == nil {
			return nil
		}
		return append(outer, field)
	case *ast.Ident:
		if object := checked.info.Uses[node]; object != nil {
			return []types.Object{object}
		}
	}
	return nil
}

// epochNamesUnion is what a branch does to the contexts two readings of one body moved.
//
// A union, because moved is a MAY: a rebind written after an if that moved on one arm is checked
// against everything either arm could have left the live context named as, and the exit check is
// where a path that moved and did not rebind is caught.
func epochNamesUnion(left [][]types.Object, right [][]types.Object) [][]types.Object {
	joined := append([][]types.Object{}, left...)
	for _, name := range right {
		if !slices.ContainsFunc(joined, func(held []types.Object) bool { return slices.Equal(held, name) }) {
			joined = append(joined, name)
		}
	}
	return joined
}

// epochGroupMoveIn answers whether one statement moves a group between epochs, how it is written,
// the VALUE it wrote through -- which is the value whose caches the boundary owes a rebind -- and
// what the moved context is now called.
func epochGroupMoveIn(checked checkedBodies, node ast.Node) (epochGroupMove, bool) {
	assignment, _ := node.(*ast.AssignStmt)
	for at, target := range epochAssignedTargets(node) {
		selector := epochWriteTarget(target)
		if selector == nil {
			// a bare identifier: a local or a parameter, which is storage of this
			// declaration's own and is what every commit path builds its next context in
			continue
		}
		if checked.info.TypeOf(selector) == nil {
			continue
		}
		// replacing the group context a value HOLDS, or moving one of the bound fields of a
		// group context in place
		//
		// THE TYPE IS READ THROUGH epochEnderCarriesTheContext AND NOT THROUGH AN EXACT NAME,
		// which is this half catching up with the ender half. That function already matches by
		// SUFFIX, and its own paragraph says why: the real ender takes a *VerifiedGroupContext,
		// and matching the exact name would read it as declaring no context at all. The WRITE
		// half went on matching the exact name, so a boundary that moved a group by installing
		// the verified value and handed the ender that same value was reported as handing it
		// something it had never moved -- which is a correct boundary refused for a reason its
		// author cannot act on, the very outcome that paragraph is about. Measured on p7 task
		// 13's MergePendingCommit, which is the first boundary of this package to exist.
		//
		// It WIDENS the class rather than narrowing it: a declaration that writes any wrapper of
		// a group context into storage that outlives the call is now a mover and owes a rebind.
		var context ast.Expr
		switch {
		case epochEnderCarriesTheContext(checked.info.TypeOf(selector)):
			context = selector
		case slices.Contains(epochBoundFields, selector.Sel.Name) &&
			epochEnderCarriesTheContext(checked.info.TypeOf(selector.X)):
			context = selector.X
		default:
			continue
		}
		move := epochGroupMove{how: checked.render(node), base: epochRootObject(checked, selector.X)}
		if named := epochDenotationOf(checked, context); named != nil {
			move.names = append(move.names, named)
		}
		// and the value ASSIGNED, which names the same context the instant the write lands and
		// is what a boundary rebinding from its own staged context hands the ender
		if assignment != nil && len(assignment.Rhs) == len(assignment.Lhs) &&
			epochEnderCarriesTheContext(checked.info.TypeOf(assignment.Rhs[at])) {
			if named := epochDenotationOf(checked, assignment.Rhs[at]); named != nil {
				move.names = epochNamesUnion(move.names, [][]types.Object{named})
			}
		}
		return move, true
	}
	return epochGroupMove{}, false
}

// ---------------------------------------------------------------------------
// what "ends the binding" means, read over paths rather than over presence
// ---------------------------------------------------------------------------

// epochPath is the abstract state of one path through a body.
//
// moved is a MAY and ended is a MUST, and that asymmetry is the rule: a gate that asked whether
// some path clears would accept `if false`, and one that asked whether every path moves would
// walk past a boundary taken on one branch. So a branch joins them the two different ways --
// moved unions, ended intersects -- and every exit is checked against the pair.
type epochPath struct {
	live  bool
	moved bool
	// the expressions that name a group context this path has already moved, which is what an
	// ender's argument is measured against. It is empty until the first write, and that is the
	// whole of the ORDER half: a rebind handed the live context ahead of the write is handed it
	// at a point where this path does not name it yet, so no path counts that rebind.
	names [][]types.Object
	ended map[types.Object]bool
}

// epochPathJoin is what a branch does to two readings of the same body.
//
// A dead path is the identity and not a zero: a clause that returned contributes nothing to what
// the code after it must have done, and joining its empty ended set in would report that nothing
// is guaranteed after any if that returns -- which is most of them.
func epochPathJoin(left epochPath, right epochPath) epochPath {
	if !left.live {
		return right
	}
	if !right.live {
		return left
	}
	joined := epochPath{live: true, moved: left.moved || right.moved,
		names: epochNamesUnion(left.names, right.names), ended: map[types.Object]bool{}}
	for object := range left.ended {
		if right.ended[object] {
			joined.ended[object] = true
		}
	}
	return joined
}

// with answers this path having ended one more cache's binding, over a copy: two branches read
// from one state, and a shared map would let the first one taken decide what the second saw.
func (self epochPath) with(object types.Object) epochPath {
	ended := map[types.Object]bool{}
	maps.Copy(ended, self.ended)
	ended[object] = true
	return epochPath{live: self.live, moved: self.moved, names: self.names, ended: ended}
}

// moving answers this path having moved one more group context, over a copy for with's reason.
func (self epochPath) moving(move epochGroupMove) epochPath {
	return epochPath{live: self.live, moved: true, names: epochNamesUnion(self.names, move.names),
		ended: self.ended}
}

// holds answers whether one denotation names a context this path has already moved.
//
// An empty denotation is never held, so an argument this rule could not read -- a call's result,
// an index, or an ender that declares no context at all -- is refused rather than accepted by
// default. That is the same direction every other reading here is wrong in.
func (self epochPath) holds(named []types.Object) bool {
	if len(named) == 0 {
		return false
	}
	return slices.ContainsFunc(self.names, func(held []types.Object) bool { return slices.Equal(held, named) })
}

// epochWalk is one declaration's reading: what it moved, which caches it owes, what it was found
// calling, and every path that left it owing one.
type epochWalk struct {
	checked  checkedBodies
	enders   []string
	bases    map[types.Object]bool
	required map[types.Object]bool
	saw      map[types.Object]string
	// the ender calls found on the right cache and handed a context this declaration had not
	// moved by then, which is a different report from "no rebind" and from "no path reaches it"
	wrong   []string
	skipped []string
	// the states that left the innermost loop or switch, so a break is a path out rather than
	// a statement that falls through into whatever was written under it
	breaks []epochPath
}

// enderOf answers which cache field a call ends the binding of, the group context it was handed,
// and whether it is an ender call on one of this declaration's own caches at all.
//
// Four questions now, and the gate that asked only the first three accepted a rebind to the epoch
// that had just closed: the method is one the classification table calls a binding ender; the
// receiver is a FIELD whose object is one of the caches the moved value holds; the value that
// field is reached through is one this declaration actually wrote a group context into; and the
// argument carrying the new binding is found in the ender's OWN SIGNATURE. A cache reached through
// anything but a field of the moved value -- a local, a parameter, a call's result -- is refused
// rather than followed, which is the conservative direction: it fails, loudly, and the fix is to
// write the rebind where the reader can see what it rebinds.
//
// WHICH context it was handed is the caller's question and not this one's, because the answer
// depends on the path: the same expression names the closing context before the write and the new
// one after it, and only the walk knows which side of the write this call is on.
func (self *epochWalk) enderOf(call *ast.CallExpr) (types.Object, ast.Expr, bool) {
	method, isMethod := extensionTypeSelectionUnparenthesised(call.Fun).(*ast.SelectorExpr)
	if !isMethod || !slices.Contains(self.enders, method.Sel.Name) {
		return nil, nil, false
	}
	held, isField := extensionTypeSelectionUnparenthesised(method.X).(*ast.SelectorExpr)
	if !isField {
		return nil, nil, false
	}
	field := self.checked.info.Uses[held.Sel]
	if field == nil || !self.required[field] {
		return nil, nil, false
	}
	if root := epochRootObject(self.checked, held.X); root == nil || !self.bases[root] {
		return nil, nil, false
	}
	return field, epochEnderArgument(self.checked, call), true
}

// epochEnderArgument answers the group context an ender call was handed, found by TYPE in the
// ender's own signature.
//
// Not argument zero. A rule stated over a POSITION is one a parameter added in front of it walks
// straight through, and the ender names are already a parameter of this matcher rather than a
// constant here -- so the signature is the only thing that can say which argument carries the
// binding. TestEveryWriterOfTheProposalCacheBindingIsClassifiedHere holds the real ender to
// declaring exactly one, so the two ends of this agree.
//
// An ender declaring NONE answers nil, which the caller reads as an argument naming nothing and
// refuses. That is the loud direction and it has a control member: (*ProposalCache).Release ends a
// binding and takes no context, and (*Group).mergeAndReleaseWithoutAContext is refused because of
// it. Two of them answers nil for the same reason -- an ender taking two contexts is one nothing
// here can say the binding came from -- and (*ProposalCache).ClearBetween is that member.
// (*ProposalCache).ClearAt is the third: it takes a reason first and the context second, so a rule
// reading argument zero answers a string literal there and refuses a boundary that is correct.
func epochEnderArgument(checked checkedBodies, call *ast.CallExpr) ast.Expr {
	signature, isSignature := checked.info.TypeOf(call.Fun).(*types.Signature)
	if !isSignature {
		return nil
	}
	params := signature.Params()
	found := -1
	for at := 0; at < params.Len(); at++ {
		if !epochEnderCarriesTheContext(params.At(at).Type()) {
			continue
		}
		if found != -1 {
			return nil
		}
		found = at
	}
	// the bound is a slice guard and not a rule: a type checked call to a non variadic
	// parameter always supplies it, and a variadic one never resolves to a context at all
	if found == -1 || found >= len(call.Args) {
		return nil
	}
	return call.Args[found]
}

// epochEnderCarriesTheContext reports whether one parameter of an ender is the group context a
// boundary moved to.
//
// The type name is matched by SUFFIX rather than by equality, and that is a correction rather
// than a loosening. The real ender takes a *VerifiedGroupContext -- a group context whose
// authority has been established -- while the control below takes a bare *GroupContext, and both
// are the parameter this rule is about. Matching the exact name would read the real ender as
// declaring none, and the caller reads "declares none" as an ender nothing can say the binding
// came from, so every boundary that is in fact correct would be refused for a reason its author
// could not act on. The suffix is what the two spellings share and what any later wrapper of a
// group context would share too.
func epochEnderCarriesTheContext(found types.Type) bool {
	for {
		pointer, isPointer := found.(*types.Pointer)
		if !isPointer {
			break
		}
		found = pointer.Elem()
	}
	named, isNamed := found.(*types.Named)
	return isNamed && named.Obj() != nil &&
		strings.HasSuffix(named.Obj().Name(), epochGroupContextTypeName)
}

// epochArgumentText names what an ender was handed, for a report a reader can act on rather than
// a verdict.
func epochArgumentText(checked checkedBodies, argument ast.Expr) string {
	if argument == nil {
		return "no group context at all"
	}
	return checked.render(argument)
}

// exit records one path leaving the declaration, and is where the whole rule is spent.
//
// Only a path that MAY have moved a group is asked the question, so a body's early returns taken
// before the write are not findings -- and a path that moved is asked about every cache the moved
// value holds rather than about one.
func (self *epochWalk) exit(state epochPath, at ast.Node) {
	if !state.live || !state.moved {
		return
	}
	for object := range self.required {
		if state.ended[object] {
			continue
		}
		self.skipped = append(self.skipped, fmt.Sprintf("the path leaving at %s holds %s bound",
			self.checked.where(at), object.Name()))
	}
}

// effects reads what one statement's own expressions do, without descending into a function
// literal.
//
// A literal's body runs when something calls the literal, and nothing at the point it is written
// says when or whether that happens -- (*Group).mergeAndClearInALiteral is a rebind returned to a
// caller who may never run it. So it is walked past, which reports the declaration as owing a
// rebind rather than as having made one.
func (self *epochWalk) effects(node ast.Node, state epochPath, counts bool) epochPath {
	if node == nil {
		return state
	}
	ast.Inspect(node, func(inner ast.Node) bool {
		if _, isLiteral := inner.(*ast.FuncLit); isLiteral {
			return false
		}
		call, isCall := inner.(*ast.CallExpr)
		if !isCall {
			return true
		}
		object, argument, isEnder := self.enderOf(call)
		if !isEnder {
			return true
		}
		// recorded whether or not this path counts it, so a report can say "the rebind is
		// there and no path reaches it" rather than only "no rebind"
		self.saw[object] = self.checked.render(call)
		// and the argument is read against what THIS PATH has moved SO FAR, which is the
		// order half and the argument half asked as one question. Ahead of the write the
		// path names no context, so a rebind handed the closing one is a rebind nothing
		// counts; after it the only names that answer are the storage written and the value
		// written into it.
		if !state.holds(epochDenotationOf(self.checked, argument)) {
			self.wrong = append(self.wrong,
				fmt.Sprintf("%s at %s is handed %s, which is not a group context this declaration had moved by then",
					self.checked.render(call), self.checked.where(call),
					epochArgumentText(self.checked, argument)))
			return true
		}
		if counts {
			state = state.with(object)
		}
		return true
	})
	return state
}

func (self *epochWalk) statements(list []ast.Stmt, state epochPath) epochPath {
	for _, statement := range list {
		state = self.statement(statement, state)
	}
	return state
}

// optional walks a statement a syntax node may or may not carry -- an if's init, a for's post.
func (self *epochWalk) optional(node ast.Stmt, state epochPath) epochPath {
	if node == nil {
		return state
	}
	return self.statement(node, state)
}

// statement is the reading of one statement: what it may move, what it must have ended after it,
// and which of its paths left the declaration.
func (self *epochWalk) statement(node ast.Stmt, state epochPath) epochPath {
	switch statement := node.(type) {
	case *ast.BlockStmt:
		return self.statements(statement.List, state)

	case *ast.LabeledStmt:
		// a label is a statement wrapped around a statement, and a rule that treated it as a
		// leaf would read the switch under it as one expression
		return self.statement(statement.Stmt, state)

	case *ast.IfStmt:
		state = self.optional(statement.Init, state)
		state = self.effects(statement.Cond, state, true)
		taken := self.statement(statement.Body, state)
		other := state
		if statement.Else != nil {
			other = self.statement(statement.Else, state)
		}
		return epochPathJoin(taken, other)

	case *ast.ForStmt:
		state = self.optional(statement.Init, state)
		state = self.effects(statement.Cond, state, true)
		self.breaks = append(self.breaks, epochPath{})
		body := self.statements(statement.Body.List, state)
		body = self.optional(statement.Post, body)
		// a loop body runs no times at all when the condition is false on entry, so a
		// rebind inside one is a rebind a path misses
		return self.leaveBreakable(epochPathJoin(state, body))

	case *ast.RangeStmt:
		state = self.effects(statement.X, state, true)
		self.breaks = append(self.breaks, epochPath{})
		body := self.statements(statement.Body.List, state)
		return self.leaveBreakable(epochPathJoin(state, body))

	case *ast.SwitchStmt:
		state = self.optional(statement.Init, state)
		state = self.effects(statement.Tag, state, true)
		return self.clauses(statement.Body.List, state, true)

	case *ast.TypeSwitchStmt:
		state = self.optional(statement.Init, state)
		state = self.optional(statement.Assign, state)
		return self.clauses(statement.Body.List, state, true)

	case *ast.SelectStmt:
		// a select takes one of its clauses rather than none, which is the one way it is not
		// a switch
		return self.clauses(statement.Body.List, state, false)

	case *ast.ReturnStmt:
		for _, result := range statement.Results {
			state = self.effects(result, state, true)
		}
		self.exit(state, node)
		return epochPath{}

	case *ast.BranchStmt:
		switch statement.Tok {
		case token.BREAK, token.CONTINUE, token.FALLTHROUGH:
			// it leaves this statement rather than the declaration, so the state is
			// carried to where the loop or switch ends instead of being checked
			self.recordBreak(state)
		default:
			// a goto: a path this rule will not follow, reported as one that reached
			// the exit owing a rebind rather than guessed at
			self.exit(state, node)
		}
		return epochPath{}

	case *ast.GoStmt:
		// nothing orders a goroutine's rebind against this declaration's return, so the call
		// is recorded and not counted
		return self.effects(statement.Call, state, false)

	default:
		// an assignment, an increment, a send, a declaration, an expression -- and a DEFER:
		// a statement holding no statement of its own, so its calls are read once and here.
		// The defer is among them deliberately, and the argument half is what makes that a
		// measurement rather than a convenience. A deferred call RUNS at the exit, but its
		// arguments are evaluated where it is WRITTEN -- so a rebind deferred ahead of the
		// move is handed the epoch that is about to close no matter that the call itself
		// happens afterwards. Reading it here is therefore the CORRECT place, and the two
		// control members that separate the readings are (*Group).mergeAndClearInADefer,
		// registered after the move and accepted, and
		// (*Group).mergeAndClearInADeferRegisteredBeforeTheMove, which is not and is refused.
		// Before the argument half existed this arm's answer never differed from the one
		// beneath it, which is what that paragraph used to admit.
		state = self.effects(node, state, true)
		if move, moves := epochGroupMoveIn(self.checked, node); moves {
			state = state.moving(move)
		}
		return state
	}
}

// clauses reads a switch or a select: the join over every clause, plus the entry state itself when
// nothing has to match.
//
// A switch with no default is a branch whose other arm is "none of them", which is what makes a
// rebind written in one case a rebind a path misses. The case EXPRESSIONS are not read for calls:
// they are evaluated in order until one matches, so counting all of them would claim a rebind on a
// path that never evaluated it -- and missing one costs a false failure rather than a false pass.
func (self *epochWalk) clauses(list []ast.Stmt, state epochPath, mayMatchNone bool) epochPath {
	self.breaks = append(self.breaks, epochPath{})
	out := epochPath{}
	matchesAll := false
	for _, clause := range list {
		switch one := clause.(type) {
		case *ast.CaseClause:
			if one.List == nil {
				matchesAll = true
			}
			out = epochPathJoin(out, self.statements(one.Body, state))
		case *ast.CommClause:
			if one.Comm == nil {
				matchesAll = true
			}
			out = epochPathJoin(out, self.statements(one.Body, self.optional(one.Comm, state)))
		}
	}
	if mayMatchNone && !matchesAll {
		out = epochPathJoin(out, state)
	}
	return self.leaveBreakable(out)
}

func (self *epochWalk) leaveBreakable(out epochPath) epochPath {
	out = epochPathJoin(out, self.breaks[len(self.breaks)-1])
	self.breaks = self.breaks[:len(self.breaks)-1]
	return out
}

// recordBreak carries a path that left a loop or a switch to where that statement ends.
//
// The innermost one, and a labelled break to an outer one is read as leaving the inner: that is
// the conservative reading, because a state carried to the wrong place is joined into an answer
// that is already the intersection of every clause.
func (self *epochWalk) recordBreak(state epochPath) {
	if len(self.breaks) == 0 {
		return
	}
	self.breaks[len(self.breaks)-1] = epochPathJoin(self.breaks[len(self.breaks)-1], state)
}

// rendered names the ender calls this walk found, in one string a reader can act on.
func (self *epochWalk) rendered() string {
	calls := []string{}
	for _, call := range self.saw {
		calls = append(calls, call)
	}
	slices.Sort(calls)
	return strings.Join(calls, "; ")
}

// covered answers the whole demand: every cache the moved value holds is ended by a call this walk
// found, and no path left the declaration owing one.
//
// A mover whose moved value holds NO cache needs no clause of its own here, and the one that was
// written did not survive its own mutation. Nothing can be found for a cache a value does not
// hold -- enderOf refuses any field that is not one of them -- so finds is empty, ends is the
// calls that were found rather than a verdict, and such a declaration is reported as ending no
// binding. That is the loud answer and the right one: a boundary with no cache in reach fails
// this gate until somebody writes down why it moves a group and owes nothing.
func (self *epochWalk) covered() bool {
	if len(self.skipped) != 0 {
		return false
	}
	for object := range self.required {
		if self.saw[object] == "" {
			return false
		}
	}
	return true
}

// epochMoversIn is the rule: every declaration of one checked package that writes a group context,
// or one of the fields the cache binds to, into storage that outlives the call.
//
// The cache type and the methods that END a binding are parameters rather than constants, because
// this matcher is run on a control package that declares its own of each. A matcher that could
// only read the real names would have nothing to prove itself against, and the half of this gate
// that has no member in the real source is precisely the half the control exists to run.
//
// Two readings of one body, and the order matters: the first says WHAT was moved and through which
// value, which is what makes the set of caches a boundary owes derivable at all; the second walks
// the paths knowing that set. A single pass would have to decide what a call ended before knowing
// which caches were in question.
func epochMoversIn(checked checkedBodies, cacheType string, enders []string) epochMoverScan {
	scan := epochMoverScan{moving: map[string]epochMoverFinding{}}
	for _, file := range checked.files {
		for _, declaration := range file.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil {
				continue
			}
			name := extensionTypeSelectionDeclarationName(checked, function)
			finding := epochMoverFinding{}
			bases := map[types.Object]bool{}
			moves := false
			ast.Inspect(function.Body, func(node ast.Node) bool {
				for _, target := range epochAssignedTargets(node) {
					selector := epochWriteTarget(target)
					if selector == nil || checked.info.TypeOf(selector) == nil {
						continue
					}
					scan.writes++
				}
				move, moved := epochGroupMoveIn(checked, node)
				if !moved {
					return true
				}
				moves = true
				finding.how = move.how
				if move.base != nil {
					bases[move.base] = true
				}
				return true
			})
			if !moves {
				continue
			}
			walk := &epochWalk{
				checked:  checked,
				enders:   enders,
				bases:    bases,
				required: epochCachesHeldBy(checked, bases, cacheType),
				saw:      map[types.Object]string{},
			}
			walk.exit(walk.statements(function.Body.List,
				epochPath{live: true, ended: map[types.Object]bool{}}), function.Body)
			finding.where = checked.where(function)
			finding.finds = walk.rendered()
			finding.wrong = strings.Join(walk.wrong, "; ")
			finding.skipped = strings.Join(walk.skipped, "; ")
			if walk.covered() {
				finding.ends = finding.finds
			}
			scan.moving[name] = finding
		}
	}
	return scan
}

// epochMoverControl is a package the matcher has never seen, declaring its own group context,
// proposal cache and group, written so that every half of the rule has something to report and
// something to walk past.
//
// It carries the two shapes the real class has no member of -- a merge that ends the binding and a
// merge that does not -- because those are what the second half of this gate demands, and the real
// source will not hold one until p7 task 13 lands MergePendingCommit. A control rather than a
// second opinion about the real source: a matcher that resolved nothing reports an empty class over
// mls too, and the only way to tell that apart from a package with no movers is to run it on source
// known to hold both answers.
//
// EVERY ARM OF EVERY MATCHER ABOVE HAS A MEMBER HERE that changes its answer when the arm is
// removed, which is what stops a stated breadth from being prose. The path arms are the bulk of
// them: an unreachable clear, a clear past a return, a clear on one branch, in a loop, in a range,
// in a switch with no default, in a type switch, in one select clause, past a break, past a jump,
// in a goroutine and inside a function literal are all clears this rule refuses -- and a clear on
// both branches, in every clause of a switch, under a label, and in a defer registered after the
// move are all clears it accepts. The identity arms are the rest: a clear of another type, of a
// field of another type, of another value's cache, and of one of the two caches a group holds. The
// ARGUMENT arms are the newest: a clear handed the context that is about to close, one handed
// another group's context, one handed a context the declaration built after the move, one handed
// nothing at all, one naming two contexts, one whose ender takes its context second, and a defer
// registered before the move -- against a clear handed the storage the write landed in, one handed
// the value that was written into it, one handed the operand of a dereference and one the operand
// of an address-of, which are the spellings of the same context and are all accepted.
const epochMoverControl = `package control

type GroupContext struct {
	GroupId []byte
	Epoch   uint64
	Extras  []byte
}

type ProposalCache struct {
	byRef map[string]int
	epoch uint64
}

// the ender, and it takes the context it is being bound to. That parameter is the whole of what
// makes the argument half measurable: a control whose ender took nothing could not tell a matcher
// that reads the argument from one that does not.
func (self *ProposalCache) Clear(context *GroupContext) {
	self.byRef = map[string]int{}
	self.epoch = context.Epoch
}

// a SECOND classified ender, taking no group context at all, so a matcher that assumed every ender
// carries one has something here to be wrong about
func (self *ProposalCache) Release() {
	self.byRef = map[string]int{}
}

// a THIRD, with the context somewhere other than first, because the argument half is stated over
// the SIGNATURE and a rule stated over argument zero would be right everywhere else by accident
func (self *ProposalCache) ClearAt(reason string, context *GroupContext) {
	self.byRef = map[string]int{}
	self.epoch = context.Epoch
}

// and a FOURTH taking two of them, which is an ender nothing can say the binding came from even
// when both arguments are the context the caller moved to
func (self *ProposalCache) ClearBetween(from *GroupContext, to *GroupContext) {
	self.byRef = map[string]int{}
	self.epoch = to.Epoch
}

func (self *ProposalCache) Store(key string, at uint64) {
	self.byRef[key] = 1
	self.epoch = at
}

// a Clear on something that is not a proposal cache, so the ender is resolved by the compiler's
// reading of the type rather than by the spelling of the call
type Journal struct{ lines []string }

func (self *Journal) Clear() { self.lines = nil }

type Group struct {
	context   *GroupContext
	proposals *ProposalCache
	journal   *Journal
	slot      *GroupContext
	name      string
}

// the shape p7 task 13 and task 19 both write, without the clear: the live context is replaced
// and the cache goes on holding the epoch that just closed
func (self *Group) mergeWithoutClearing(staged *GroupContext) {
	self.context = staged
	self.name = "merged"
}

// the same with the clear, handed the storage the write landed in, which is the shape this gate
// accepts
func (self *Group) mergeAndClear(staged *GroupContext) {
	self.context = staged
	self.proposals.Clear(self.context)
}

// and handed the value that was written instead, which names the same context the instant the
// assignment lands and is what a boundary rebinding from its own staged context writes
func (self *Group) mergeAndClearWithTheStagedContext(staged *GroupContext) {
	self.context = staged
	self.proposals.Clear(staged)
}

// the defect the argument half exists for: every path runs the clear, it is the right cache, and
// the context it is handed is the one that is about to CLOSE
func (self *Group) clearWithTheClosingContextThenMerge(staged *GroupContext) {
	self.proposals.Clear(self.context)
	self.context = staged
}

// the same defect wearing another group's context, which is neither the epoch that closed nor the
// one this group moved to, and which a rule comparing rendered text would have to work to tell
// apart from self.context
func (self *Group) mergeAndClearWithAnotherGroupsContext(staged *GroupContext, other *Group) {
	self.context = staged
	self.proposals.Clear(other.context)
}

// a context this declaration built AFTER the move: an expression the rule cannot name, and one it
// refuses rather than guesses at
func (self *Group) mergeAndClearWithAContextItBuiltAfterwards(staged *GroupContext) {
	self.context = staged
	self.proposals.Clear(self.stageTheNextContext())
}

// an ender that ends the binding and names no epoch to take it to, so there is no argument to read
func (self *Group) mergeAndReleaseWithoutAContext(staged *GroupContext) {
	self.context = staged
	self.proposals.Release()
}

// an ender whose context is not its first argument, handed the storage the write landed in
func (self *Group) mergeAndClearAtWithTheContext(staged *GroupContext) {
	self.context = staged
	self.proposals.ClearAt("boundary", self.context)
}

// an ender naming two contexts, both of them the right one, which still leaves nothing to say
// which of them the binding was taken from
func (self *Group) mergeAndClearBetweenTwoContexts(staged *GroupContext) {
	self.context = staged
	self.proposals.ClearBetween(self.context, self.context)
}

// a context assembled as a VALUE and installed by address, with the clear handed the same value:
// an address-of names its operand for the reason a dereference does
func (self *Group) mergeByAddressAndClearWithTheValue(next GroupContext) {
	self.context = &next
	self.proposals.Clear(&next)
}

// the cheapest advance there is, and it is not an assignment at all
func (self *Group) bumpTheEpochInPlace() {
	self.context.Epoch++
}

// the same advance with the clear the boundary owes: what owes it is the GROUP, and carrying the
// write back to it is a read of the selector chain rather than of the field
func (self *Group) bumpTheEpochAndClear() {
	self.context.Epoch++
	self.proposals.Clear(self.context)
}

// rewriting the group id in place, which moves the group as surely as the epoch does
func (self *Group) rebrand(id []byte) {
	self.context.GroupId = id
}

// overwriting the context the group already holds rather than replacing the pointer to it, which
// is a write through a dereference and is a write nothing else here makes
func (self *Group) overwriteThroughThePointer(staged *GroupContext) {
	*self.slot = *staged
	self.proposals.Clear(self.slot)
}

// the same write with the clear handed the value that was copied IN rather than the storage it was
// copied into, which is the one member the dereference arm carries: *staged names staged
func (self *Group) overwriteThroughThePointerAndClearWithTheSource(staged *GroupContext) {
	*self.slot = *staged
	self.proposals.Clear(staged)
}

// the decoder shape: an epoch written into a context out of the caller's own input
func (self *GroupContext) decode(epoch uint64, id []byte) {
	self.Epoch = epoch
	self.GroupId = id
}

// staging a context of its own, which is what every commit path does before anything is allowed
// near the live group. A local is not storage that outlives the call
func (self *Group) stageTheNextContext() *GroupContext {
	next := &GroupContext{GroupId: self.context.GroupId, Epoch: self.context.Epoch + 1}
	next.Extras = []byte{0x01}
	return next
}

// writing a field of the live context that the cache binds to nothing of
func (self *Group) annotate(extras []byte) {
	self.context.Extras = extras
}

func (self *Group) readsTheEpoch() uint64 {
	return self.context.Epoch
}

// a Clear of that NAME on a value of another type, which this declaration was handed
func (self *Group) mergeAndClearTheWrongThing(staged *GroupContext, journal *Journal) {
	self.context = staged
	journal.Clear()
}

// a Clear on a field of the very group that moved, of a type that is not a cache
func (self *Group) mergeAndClearTheWrongField(staged *GroupContext) {
	self.context = staged
	self.journal.Clear()
}

// the right FIELD of the wrong VALUE: another group's cache is not this one's, and an ender
// resolved by type alone cannot tell the two apart. Handed the right context, so identity is the
// only thing left for it to fail on
func (self *Group) mergeAndClearAnotherGroupsCache(staged *GroupContext, other *Group) {
	self.context = staged
	other.proposals.Clear(self.context)
}

// a plain function moving a group it was handed: an epoch boundary is not always a method, and
// what owes the rebind is the value the write was made through
func mergeIntoTheGroup(group *Group, staged *GroupContext) {
	group.context = staged
	group.proposals.Clear(group.context)
}

// the clear is written and no path runs it
func (self *Group) mergeAndClearUnreachably(staged *GroupContext) {
	self.context = staged
	if false {
		self.proposals.Clear(self.context)
	}
}

// p7's own two statements in the other order: the group has moved, the persist fails, and the
// return leaves the cache bound to the epoch that closed
func (self *Group) mergeReturningBeforeTheClear(staged *GroupContext) bool {
	self.context = staged
	if self.name == "" {
		return false
	}
	self.proposals.Clear(self.context)
	return true
}

// and in the order p7 wrote them, which is the accepted one
func (self *Group) mergeAndClearBeforeTheReturn(staged *GroupContext) bool {
	self.context = staged
	self.proposals.Clear(self.context)
	if self.name == "" {
		return false
	}
	return true
}

// one branch clears and the other does not
func (self *Group) mergeAndClearOnOnePath(staged *GroupContext) {
	self.context = staged
	if self.name == "" {
		self.proposals.Clear(self.context)
	}
}

// both branches clear, so no path out leaves the cache bound
func (self *Group) mergeAndClearOnBothPaths(staged *GroupContext) {
	self.context = staged
	if self.name == "" {
		self.proposals.Clear(self.context)
	} else {
		self.proposals.Clear(self.context)
	}
}

// a deferred clear runs on every exit taken after it is registered, and its ARGUMENT is evaluated
// where the defer is WRITTEN -- so this one is registered after the move and is handed the context
// the group is now in
func (self *Group) mergeAndClearInADefer(staged *GroupContext) bool {
	self.context = staged
	defer self.proposals.Clear(self.context)
	if self.name == "" {
		return false
	}
	return true
}

// and the same defer registered BEFORE the move, which is what a rule reading only where a
// deferred call RUNS would accept: the call happens at the exit, and the context it was handed was
// read at the defer and is the one that closed
func (self *Group) mergeAndClearInADeferRegisteredBeforeTheMove(staged *GroupContext) bool {
	defer self.proposals.Clear(self.context)
	self.context = staged
	return true
}

// a loop body runs no times when the condition is false on entry
func (self *Group) mergeAndClearInALoop(staged *GroupContext, times int) {
	self.context = staged
	for i := 0; i < times; i++ {
		self.proposals.Clear(self.context)
	}
}

// the same over a range, which is the other loop form and is not an *ast.ForStmt
func (self *Group) mergeAndClearOverARange(staged *GroupContext, names []string) {
	self.context = staged
	for range names {
		self.proposals.Clear(self.context)
	}
}

// a switch with no default matches nothing at all when nothing matches
func (self *Group) mergeAndClearInASwitch(staged *GroupContext) {
	self.context = staged
	switch self.name {
	case "merged":
		self.proposals.Clear(self.context)
	}
}

// the same over a type switch, which is a different node with the same hole
func (self *Group) mergeAndClearInATypeSwitch(staged *GroupContext, held any) {
	self.context = staged
	switch held.(type) {
	case string:
		self.proposals.Clear(self.context)
	}
}

// a switch whose every clause clears, default included, leaves no path out
func (self *Group) mergeAndClearInEverySwitchClause(staged *GroupContext) {
	self.context = staged
	switch self.name {
	case "merged":
		self.proposals.Clear(self.context)
	default:
		self.proposals.Clear(self.context)
	}
}

// the same under a label, which is a statement wrapped around a statement
func (self *Group) mergeAndClearInALabelledSwitch(staged *GroupContext) {
	self.context = staged
pick:
	switch self.name {
	case "merged":
		self.proposals.Clear(self.context)
		break pick
	default:
		self.proposals.Clear(self.context)
	}
}

// a labelled loop: the label is a statement wrapped around a statement, and a rule that read it
// as a leaf would find the clear inside the loop and count it unconditionally
func (self *Group) mergeAndClearInALabelledLoop(staged *GroupContext, times int) {
	self.context = staged
spin:
	for i := 0; i < times; i++ {
		self.proposals.Clear(self.context)
		continue spin
	}
}

// a select with no default blocks until one of its clauses is ready, so a clear in every clause is
// one every path takes -- which is the one way a select is not a switch
func (self *Group) mergeAndClearInEverySelectClause(staged *GroupContext, ready chan int, other chan int) {
	self.context = staged
	select {
	case <-ready:
		self.proposals.Clear(self.context)
	case <-other:
		self.proposals.Clear(self.context)
	}
}

// a guard taken before anything moves. The path out of it owes the cache nothing, and a rule that
// asked EVERY path rather than every path that moved a group would refuse every declaration that
// validates its input first -- which is every declaration in this plan
func (self *Group) refuseThenMergeAndClear(staged *GroupContext) bool {
	if staged == nil {
		return false
	}
	self.context = staged
	self.proposals.Clear(self.context)
	return true
}

// a select takes one clause rather than none, so a clear in one of two is one a path misses
func (self *Group) mergeAndClearInOneSelectClause(staged *GroupContext, ready chan int) {
	self.context = staged
	select {
	case <-ready:
		self.proposals.Clear(self.context)
	default:
	}
}

// a clause that leaves the switch before its own clear, in a switch whose other clause clears:
// the break is a path out and the clear under it is not on it
func (self *Group) mergeAndBreakOutBeforeTheClear(staged *GroupContext) {
	self.context = staged
	switch self.name {
	case "merged":
		if self.context == nil {
			break
		}
		self.proposals.Clear(self.context)
	default:
		self.proposals.Clear(self.context)
	}
}

// a jump over the clear, which is a path this rule refuses to follow rather than guess at
func (self *Group) mergeAndJumpPastTheClear(staged *GroupContext) {
	self.context = staged
	goto done
	self.proposals.Clear(self.context)
done:
	self.name = "merged"
}

// nothing orders a goroutine's clear against this declaration's return
func (self *Group) mergeAndClearInAGoroutine(staged *GroupContext) {
	self.context = staged
	go self.proposals.Clear(self.context)
}

// a clear inside a function literal runs when somebody calls the literal, and nothing here says
// whether anybody does
func (self *Group) mergeAndClearInALiteral(staged *GroupContext) func() {
	self.context = staged
	return func() { self.proposals.Clear(self.context) }
}

// two caches, which is the shape p7 task 19 adds and the shape that makes "which cache" a
// question the gate has to answer
type StagingGroup struct {
	context   *GroupContext
	proposals *ProposalCache
	pending   *ProposalCache
}

// a clear of a cache of the right TYPE, held by the very group that moved, that is not the only
// one the group holds
func (self *StagingGroup) mergeAndClearOneOfTwoCaches(staged *GroupContext) {
	self.context = staged
	self.pending.Clear(self.context)
}

// and the shape that owes both and ends both
func (self *StagingGroup) mergeAndClearBothCaches(staged *GroupContext) {
	self.context = staged
	self.proposals.Clear(self.context)
	self.pending.Clear(self.context)
}
`

// What the matcher must report out of the control, and by omission what it must walk past.
var epochMoverControlReports = []string{
	"(*Group).bumpTheEpochAndClear",
	"(*Group).bumpTheEpochInPlace",
	"(*Group).clearWithTheClosingContextThenMerge",
	"(*Group).mergeAndBreakOutBeforeTheClear",
	"(*Group).mergeAndClear",
	"(*Group).mergeAndClearAnotherGroupsCache",
	"(*Group).mergeAndClearAtWithTheContext",
	"(*Group).mergeAndClearBeforeTheReturn",
	"(*Group).mergeAndClearBetweenTwoContexts",
	"(*Group).mergeAndClearInADefer",
	"(*Group).mergeAndClearInADeferRegisteredBeforeTheMove",
	"(*Group).mergeAndClearInAGoroutine",
	"(*Group).mergeAndClearInALabelledLoop",
	"(*Group).mergeAndClearInALabelledSwitch",
	"(*Group).mergeAndClearInALiteral",
	"(*Group).mergeAndClearInALoop",
	"(*Group).mergeAndClearInASwitch",
	"(*Group).mergeAndClearInATypeSwitch",
	"(*Group).mergeAndClearInEverySelectClause",
	"(*Group).mergeAndClearInEverySwitchClause",
	"(*Group).mergeAndClearInOneSelectClause",
	"(*Group).mergeAndClearOnBothPaths",
	"(*Group).mergeAndClearOnOnePath",
	"(*Group).mergeAndClearOverARange",
	"(*Group).mergeAndClearTheWrongField",
	"(*Group).mergeAndClearTheWrongThing",
	"(*Group).mergeAndClearUnreachably",
	"(*Group).mergeAndClearWithAContextItBuiltAfterwards",
	"(*Group).mergeAndClearWithAnotherGroupsContext",
	"(*Group).mergeAndClearWithTheStagedContext",
	"(*Group).mergeAndJumpPastTheClear",
	"(*Group).mergeAndReleaseWithoutAContext",
	"(*Group).mergeByAddressAndClearWithTheValue",
	"(*Group).mergeReturningBeforeTheClear",
	"(*Group).mergeWithoutClearing",
	"(*Group).overwriteThroughThePointer",
	"(*Group).overwriteThroughThePointerAndClearWithTheSource",
	"(*Group).rebrand",
	"(*Group).refuseThenMergeAndClear",
	"(*GroupContext).decode",
	"(*StagingGroup).mergeAndClearBothCaches",
	"(*StagingGroup).mergeAndClearOneOfTwoCaches",
	"mergeIntoTheGroup",
}

// Which of them the matcher must read as ending the cache binding: every path out of them has
// rebound every cache the value they moved holds, with the context that value moved to.
//
// mergeAndClearTheWrongThing calls a method of that NAME on another type, mergeAndClearTheWrongField
// on a field of another type, mergeAndClearAnotherGroupsCache on the right field of another value,
// and mergeAndClearOneOfTwoCaches on one of the two its group holds -- none of them are here, and
// each is a shape a gate that resolved the ender by type alone accepted.
//
// The argument half takes six more out. clearWithTheClosingContextThenMerge and
// mergeAndClearInADeferRegisteredBeforeTheMove are handed the epoch that is about to close;
// mergeAndClearWithAnotherGroupsContext is handed a context this body never moved;
// mergeAndClearWithAContextItBuiltAfterwards is handed one it constructed after the move;
// mergeAndReleaseWithoutAContext is handed nothing to read at all; and
// mergeAndClearBetweenTwoContexts hands two, which names no one of them.
//
// And four spellings of the RIGHT context are all here, which is what says the rule reads the
// storage written as well as the value written into it: mergeAndClear names the field the write
// landed in, mergeAndClearWithTheStagedContext the value assigned,
// overwriteThroughThePointerAndClearWithTheSource the operand of a dereference, and
// mergeByAddressAndClearWithTheValue the operand of an address-of. mergeAndClearAtWithTheContext
// is the one whose ender takes its context second.
var epochMoverControlEnders = []string{
	"(*Group).bumpTheEpochAndClear",
	"(*Group).mergeAndClear",
	"(*Group).mergeAndClearAtWithTheContext",
	"(*Group).mergeAndClearBeforeTheReturn",
	"(*Group).mergeAndClearInADefer",
	"(*Group).mergeAndClearInALabelledSwitch",
	"(*Group).mergeAndClearInEverySelectClause",
	"(*Group).mergeAndClearInEverySwitchClause",
	"(*Group).mergeAndClearOnBothPaths",
	"(*Group).mergeAndClearWithTheStagedContext",
	"(*Group).mergeByAddressAndClearWithTheValue",
	"(*Group).overwriteThroughThePointer",
	"(*Group).overwriteThroughThePointerAndClearWithTheSource",
	"(*Group).refuseThenMergeAndClear",
	"(*StagingGroup).mergeAndClearBothCaches",
	"mergeIntoTheGroup",
}

// And which of them the matcher must read as CALLING an ender on one of their own caches at all,
// whether or not every path reaches it and whether or not it was handed the right context.
//
// This is what separates the ways of failing, and without it the reachability half of the rule is
// unmeasured: a matcher that never looked inside an if, a loop or a switch would report exactly
// the same set of enders as this one and would be reporting it for the wrong reason. Every entry
// here that is NOT an ender above is a rebind this rule found and refused -- the unreachable one,
// the one past a return, the one on a single branch, in a loop, in a range, in a switch, in a type
// switch, in a select clause, past a break, past a jump, in a goroutine, one of two caches, the
// four handed a context this body had not moved, the one handed none and the one handed two -- and
// the four movers that call an ender and are absent from the enders for a reason of IDENTITY
// rather than of paths or arguments are absent from here too.
var epochMoverControlFinds = []string{
	"(*Group).bumpTheEpochAndClear",
	"(*Group).clearWithTheClosingContextThenMerge",
	"(*Group).mergeAndBreakOutBeforeTheClear",
	"(*Group).mergeAndClear",
	"(*Group).mergeAndClearAtWithTheContext",
	"(*Group).mergeAndClearBeforeTheReturn",
	"(*Group).mergeAndClearBetweenTwoContexts",
	"(*Group).mergeAndClearInADefer",
	"(*Group).mergeAndClearInADeferRegisteredBeforeTheMove",
	"(*Group).mergeAndClearInAGoroutine",
	"(*Group).mergeAndClearInALabelledLoop",
	"(*Group).mergeAndClearInALabelledSwitch",
	"(*Group).mergeAndClearInALoop",
	"(*Group).mergeAndClearInASwitch",
	"(*Group).mergeAndClearInATypeSwitch",
	"(*Group).mergeAndClearInEverySelectClause",
	"(*Group).mergeAndClearInEverySwitchClause",
	"(*Group).mergeAndClearInOneSelectClause",
	"(*Group).mergeAndClearOnBothPaths",
	"(*Group).mergeAndClearOnOnePath",
	"(*Group).mergeAndClearOverARange",
	"(*Group).mergeAndClearUnreachably",
	"(*Group).mergeAndClearWithAContextItBuiltAfterwards",
	"(*Group).mergeAndClearWithAnotherGroupsContext",
	"(*Group).mergeAndClearWithTheStagedContext",
	"(*Group).mergeAndJumpPastTheClear",
	"(*Group).mergeAndReleaseWithoutAContext",
	"(*Group).mergeByAddressAndClearWithTheValue",
	"(*Group).mergeReturningBeforeTheClear",
	"(*Group).overwriteThroughThePointer",
	"(*Group).overwriteThroughThePointerAndClearWithTheSource",
	"(*Group).refuseThenMergeAndClear",
	"(*StagingGroup).mergeAndClearBothCaches",
	"(*StagingGroup).mergeAndClearOneOfTwoCaches",
	"mergeIntoTheGroup",
}

// One classified member of the derived class.
//
// The prose is what a reader gets; movesAGroupBetweenEpochs is the classification a human has to
// make and is what the second half of the gate is stated over; and the probe is what stops the row
// being a label. A row asserting that a declaration only constructs, on a declaration that moves
// the live group, has to fail.
type epochMoverRow struct {
	what                     string
	movesAGroupBetweenEpochs bool
	probe                    func(t *testing.T)
}

// Every declaration of this package and of ../message that writes a group context, or a field the
// proposal cache binds to, into storage that outlives the call.
//
// Held EQUAL to the derived class in both directions by the gate below, so this is a
// classification and not a list: the commit that writes MergePendingCommit either ends the cache
// binding on every path out of the same body or fails here until somebody writes down which of the
// two things it does.
var groupContextEpochMovers = map[string]epochMoverRow{
	"(*Group).MergePendingCommit": {
		what: "the epoch boundary this whole gate was written ahead of: it installs the tree, the context, the " +
			"verified context, the key schedule, the secret tree and the transcript a commit staged, and the " +
			"group is in the new epoch the instant it returns. It moves a group, so it owes the cache the epoch " +
			"it moved TO -- which is why it is handed the staged commit's own verified context and not the one " +
			"the group is leaving",
		movesAGroupBetweenEpochs: true,
		probe: func(t *testing.T) {
			crypto := testCrypto(t)
			owner := testIdentity(t, crypto, "owner")
			group := testNewGroup(t, crypto, owner, "merge-probe")
			defer group.Close()
			if _, err := group.ProposeUpdate(); err != nil {
				t.Fatalf("ProposeUpdate: %v", err)
			}
			if len(group.pendingProposalsForTest()) != 1 {
				t.Fatal("the proposal this probe watches the boundary release was not cached")
			}
			before := group.Epoch()
			if _, err := group.CreateCommit([][]byte{}, nil, nil); err != nil {
				t.Fatalf("CreateCommit: %v", err)
			}
			if group.Epoch() != before {
				t.Fatalf("the epoch moved to %d at the commit, and this row is about the merge", group.Epoch())
			}
			if err := group.MergePendingCommit(); err != nil {
				t.Fatalf("MergePendingCommit: %v", err)
			}
			if group.Epoch() != before+1 {
				t.Fatalf("the merge left the group at epoch %d, want %d", group.Epoch(), before+1)
			}
			// the closed epoch's entries are gone, which is the first half of the boundary
			if held := group.pendingProposalsForTest(); len(held) != 0 {
				t.Fatalf("the merge left %d entry/entries of the closed epoch cached", len(held))
			}
			// and the cache is bound to the epoch the group MOVED TO rather than to the one it
			// left, which is the half a rebind with the wrong context passes the first
			if _, err := group.ProposeUpdate(); err != nil {
				t.Fatalf("the cache refuses a proposal of the epoch the group entered: %v", err)
			}
			if len(group.pendingProposalsForTest()) != 1 {
				t.Fatal("a proposal of the new epoch was not cached, so the cache is bound to another epoch")
			}
		},
	},
	"(*GroupContext).UnmarshalMLS": {
		what: "the group context DECODER, and the epoch it writes is one it read out of the caller's own octets " +
			"rather than one this client advanced to. It moves no group: the value it writes into is one the " +
			"caller is constructing from a message, no proposal cache is bound to it, and the group whose epoch " +
			"this build actually lives in is not touched. This row is the false positive the rule is deliberately " +
			"too narrow to drop -- a rule that exempted the decoder by name would exempt the first commit path " +
			"that wrote its context field by field -- and it is worth the row",
		movesAGroupBetweenEpochs: false,
		probe: func(t *testing.T) {
			encoded, err := syntax.Marshal(&GroupContext{
				Version:                 ProtocolVersionMls10,
				CipherSuite:             CipherSuiteX25519ChaCha20Sha256Ed25519,
				GroupId:                 []byte("decoded"),
				Epoch:                   9,
				TreeHash:                bytes.Repeat([]byte{0xc0}, 32),
				ConfirmedTranscriptHash: bytes.Repeat([]byte{0xee}, 32),
			})
			if err != nil {
				t.Fatalf("encode the context this probe decodes: %v", err)
			}
			decoded := &GroupContext{}
			if err := syntax.Unmarshal(encoded, decoded); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if decoded.Epoch != 9 || !bytes.Equal(decoded.GroupId, []byte("decoded")) {
				t.Fatalf("the decoder answered epoch %d of group %x, want epoch 9 of \"decoded\"; the epoch it writes is the one the octets carry and that is the whole of this row's classification",
					decoded.Epoch, decoded.GroupId)
			}
			// and the other half of "it constructs": a decode that fails writes nothing at
			// all, so no receiver is left holding an epoch that came from half a message
			holding := &GroupContext{GroupId: []byte("held"), Epoch: 4}
			if err := syntax.Unmarshal(encoded[:len(encoded)-3], holding); err == nil {
				t.Fatal("a truncated group context decoded without error, so this probe observes nothing")
			}
			if holding.Epoch != 4 || !bytes.Equal(holding.GroupId, []byte("held")) {
				t.Errorf("a refused decode left the receiver at epoch %d of group %x, want the epoch 4 of \"held\" it was already holding; a partial write here is a group context moved to an epoch no message named",
					holding.Epoch, holding.GroupId)
			}
		},
	},
}

// epochBindingWriterRow is one method of the proposal cache that writes what the cache holds, with
// what it does to the binding.
type epochBindingWriterRow struct {
	what           string
	endsTheBinding bool
	probe          func(t *testing.T)
}

// Every method of *ProposalCache that writes a field of its own receiver, with what each does to
// the epoch binding.
//
// This is the other half of the derivation and it is what "or rebinding" means: the gate above
// does not demand a call by NAME, it demands a call to one of the methods classified HERE as ending
// a binding. That is not hypothetical any more -- the method is Rebind, and the Clear it replaced
// was the one that ended a binding without starting the next, which left the cache in the state a
// replayed proposal could then fill. A third method lands here before it can be accepted there.
var proposalCacheBindingWriters = map[string]epochBindingWriterRow{
	"(*ProposalCache).Store": {
		what: "writes what the cache HOLDS and never what it BELONGS TO. It is the door a peer's message comes " +
			"through, which is exactly why it must not be able to move the binding: it took the binding from " +
			"its own first entry once, and one replayed proposal of a closed epoch then bound a whole cache to " +
			"that closed epoch with no way back. It writes no binding and it ends none",
		endsTheBinding: false,
		probe: func(t *testing.T) {
			crypto := testCrypto(t)
			at7 := testResolveContextAt([]byte("group"), 7)
			cache := testCacheAt(t, at7)
			held := cache.binding
			if _, err := cache.Store(crypto, at7, testProposalContentAt(t, 1, []byte("group"), 7,
				testRemoveProposal(LeafIndex(4)))); err != nil {
				t.Fatalf("Store at epoch 7: %v", err)
			}
			if cache.binding != held {
				t.Errorf("an accepted Store replaced the binding; the binding is the group's and a store is a message's")
			}
			if _, err := cache.Store(crypto, at7, testProposalContentAt(t, 1, []byte("group"), 8,
				testRemoveProposal(LeafIndex(5)))); !errors.Is(err, errProposalCacheEpoch) {
				t.Errorf("Store of an epoch 8 entry into a cache holding epoch 7 answered %v, want errProposalCacheEpoch", err)
			}
			if cache.binding != held {
				t.Errorf("a refused Store replaced the binding; a Store that rebinds hands the binding to whatever arrived last, and what arrives is attacker supplied")
			}
			if err := cache.CheckEpoch(at7); err != nil {
				t.Errorf("the cache no longer holds the epoch it was built in: %v", err)
			}
			if err := cache.CheckEpoch(testResolveContextAt([]byte("group"), 8)); !errors.Is(err, errProposalCacheNotRebound) {
				t.Errorf("the cache answered for epoch 8 after storing only in epoch 7 = %v, want errProposalCacheNotRebound", err)
			}
		},
	},
	"(*ProposalCache).Rebind": {
		what: "empties the cache and binds it to the epoch the CALLER names, which is what an epoch boundary " +
			"owes it. It is the one method classified as ending a binding, so it is the one call the gate over " +
			"the epoch movers accepts. It ends a binding by starting the next one and it cannot do only the " +
			"first half: a release with no rebind leaves the cache unbound, and unbound is the state a replayed " +
			"message used to be able to fill",
		endsTheBinding: true,
		probe: func(t *testing.T) {
			crypto := testCrypto(t)
			at1 := testResolveContext()
			cache := testCacheAt(t, at1)
			ref := testStoredRemove(t, crypto, cache, LeafIndex(1), LeafIndex(4))
			at2 := testResolveContextAt([]byte("group"), 2)
			if err := cache.Rebind(testVerifiedContextAt(t, at2)); err != nil {
				t.Fatalf("Rebind: %v", err)
			}
			if err := cache.CheckEpoch(at2); err != nil {
				t.Errorf("a rebound cache does not hold the epoch it was rebound to: %v", err)
			}
			if err := cache.CheckEpoch(at1); !errors.Is(err, errProposalCacheNotRebound) {
				t.Errorf("a rebound cache still answers for the epoch that closed = %v, want errProposalCacheNotRebound", err)
			}
			// the binding FIELDS and not only the answer. A rebind that emptied the map
			// and left the old pair in place answers a closed epoch at every door, and
			// the measured history of this row is that exactly that mutation -- on the
			// Clear this replaces -- survived the whole of ./mls/... and ./message/...
			// because both guards short circuited on an empty cache. There is no such
			// short circuit any more, and this reads the fields regardless.
			if cache.binding == nil || cache.binding.epoch != at2.Epoch ||
				!bytes.Equal(cache.binding.groupId, at2.GroupId) {
				t.Errorf("Rebind left the cache holding %+v, want epoch %d of group %x",
					cache.binding, at2.Epoch, at2.GroupId)
			}
			if got := len(cache.Pending(at2)); got != 0 {
				t.Errorf("Rebind left %d entries behind, so the references of the closed epoch are still nameable", got)
			}
			// and the entry is GONE rather than merely unnamed by Pending, which is what
			// makes the rebind a release of the closed epoch's proposals
			if _, err := cache.Resolve(crypto, at2, LeafIndex(0),
				[]ProposalOrRef{{Type: ProposalOrRefTypeReference, Reference: ref}}); !errors.Is(err, errProposalNotCached) {
				t.Errorf("a reference into a rebound cache answered %v, want errProposalNotCached", err)
			}
		},
	},
}

// epochReceiverObjects answers the object a declaration's RECEIVER names, and nothing else.
//
// Not extensionTypeSelectionHandedTo, which answers the receiver AND the parameters: a method that
// wrote a cache it was HANDED wrote somebody else's cache, and the question this class asks is what
// a method can do to its own. (*ProposalCache).copyInto in the control below is that method, and it
// is not in the class.
//
// Over the type checker's object rather than over the spelling `self`, so a method that named its
// receiver something else is in the class and a LOCAL named `self` is not. That claim was made here
// before and nothing measured it, because every method of this package and every member of every
// control spelled its receiver `self` -- so the two readings agreed everywhere they were looked at.
// The control below spells its receivers `cache` and declares a local named `self`, which is the
// only thing that can separate them.
func epochReceiverObjects(checked checkedBodies, function *ast.FuncDecl) map[types.Object]bool {
	receivers := map[types.Object]bool{}
	if function.Recv == nil {
		return receivers
	}
	for _, field := range function.Recv.List {
		for _, name := range field.Names {
			if object := checked.info.Defs[name]; object != nil {
				receivers[object] = true
			}
		}
	}
	return receivers
}

// proposalCacheBindingWritersIn derives the class above: every method of the cache type whose body
// writes a field of its own receiver.
//
// No field list is involved: a method that can change what this cache holds or which epoch it
// belongs to is one that writes any of its fields, and asking which field would be an enumeration
// inside the derivation.
//
// The root is a parameter for the reason epochMoversIn's cache type is one: a derivation that could
// only read the real package has nothing to prove itself against, and this one is the derivation
// whose two readings -- the receiver's object, and the name `self` -- agree over every method the
// real package holds.
func proposalCacheBindingWritersIn(checked checkedBodies, cacheType string) map[string]string {
	found := map[string]string{}
	for _, file := range checked.files {
		for _, declaration := range file.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil || function.Recv == nil {
				continue
			}
			if !strings.Contains(checked.render(function.Recv.List[0].Type), cacheType) {
				continue
			}
			receivers := epochReceiverObjects(checked, function)
			name := extensionTypeSelectionDeclarationName(checked, function)
			ast.Inspect(function.Body, func(node ast.Node) bool {
				for _, target := range epochAssignedTargets(node) {
					selector := epochWriteTarget(target)
					if selector == nil {
						continue
					}
					base, isIdent := extensionTypeSelectionUnparenthesised(selector.X).(*ast.Ident)
					if !isIdent {
						continue
					}
					if object := checked.info.Uses[base]; object != nil && receivers[object] {
						if _, already := found[name]; !already {
							found[name] = checked.where(node)
						}
					}
				}
				return true
			})
		}
	}
	return found
}

// epochBindingWriterControl is a cache the derivation above has never seen, written so that the
// receiver's OBJECT and the name `self` give different answers.
//
// rebind and storeThroughTheIndex and countThroughThePointer spell their receiver `cache` and are
// in the class; writesALocalSpelledSelf spells a LOCAL `self` and is not; copyInto writes a cache
// it was handed and is not; rebuild is not a method at all. A derivation written over the spelling
// answers the exact opposite on the first three, which is what makes this control the measurement
// the paragraph above used to be an argument for.
const epochBindingWriterControl = `package control

type binding struct {
	epoch uint64
}

type ProposalCache struct {
	byRef   map[string]int
	order   []string
	binding *binding
	slot    *int
}

// the receiver is not spelled self, and it writes what the cache belongs to
func (cache *ProposalCache) rebind(at uint64) {
	cache.binding = &binding{epoch: at}
}

// writes only through an index expression, which is the shape (*ProposalCache).Store is half
// written in and is the only member of this control the index arm carries
func (cache *ProposalCache) storeThroughTheIndex(key string) {
	cache.byRef[key] = 1
}

// writes only through a pointer dereference
func (cache *ProposalCache) countThroughThePointer() {
	*cache.slot = 1
}

// a LOCAL spelled self, which is not this method's receiver
func (cache *ProposalCache) writesALocalSpelledSelf() {
	self := &ProposalCache{}
	self.byRef = map[string]int{}
	self.order = nil
}

// writes a cache it was HANDED rather than the one it is a method of
func (cache *ProposalCache) copyInto(other *ProposalCache) {
	other.byRef = cache.byRef
	other.order = cache.order
}

// reads, and writes nothing of its own
func (cache *ProposalCache) held() int {
	return len(cache.byRef)
}

// not a method of the cache at all
func rebuild(cache *ProposalCache) {
	cache.byRef = nil
}
`

// What the derivation must read out of that control, and by omission what it must walk past.
var epochBindingWriterControlReports = []string{
	"(*ProposalCache).countThroughThePointer",
	"(*ProposalCache).rebind",
	"(*ProposalCache).storeThroughTheIndex",
}

// TestEveryWriterOfTheProposalCacheBindingIsClassifiedHere is rule 5 over the smaller of the two
// classes: what can change which epoch a cache belongs to.
//
// It is what makes "or rebinding" mean something in the gate below. That gate does not look for a
// call to Clear; it looks for a call to a method this table classifies as ending a binding, and a
// third method of this cache lands here before it can be accepted there.
//
// It runs on the control first, which is the half that was missing: the derived class is compared
// against a table with rows in it, so a matcher that resolved nothing fails on the emptiness -- but
// a matcher that resolved the RIGHT NAMES FOR THE WRONG REASON passes that comparison every time,
// and over this package the receiver's object and the name `self` are the wrong reason and the
// right one for the same two answers.
func TestEveryWriterOfTheProposalCacheBindingIsClassifiedHere(t *testing.T) {
	if _, held := reflect.TypeOf(ProposalCache{}).FieldByName("byRef"); !held {
		t.Fatalf("%s no longer declares byRef, so the derivation below is written against a struct that has changed shape",
			epochCacheTypeName)
	}
	control := proposalCacheBindingWritersIn(
		typeCheckedBodiesOfText(t, "the proposal cache binding writer control", epochBindingWriterControl),
		epochCacheTypeName)
	if reported := slices.Sorted(maps.Keys(control)); !slices.Equal(reported, epochBindingWriterControlReports) {
		t.Fatalf("the derivation read %v out of the control, want %v; one written over the name `self` rather than over the receiver's own object answers the exact opposite on the three methods that spell theirs `cache`, and one that took a parameter for a receiver reports the method that writes the cache it was handed",
			reported, epochBindingWriterControlReports)
	}

	derived := proposalCacheBindingWritersIn(typeCheckedBodiesOf(t, "."), epochCacheTypeName)
	classified := slices.Sorted(maps.Keys(proposalCacheBindingWriters))
	if found := slices.Sorted(maps.Keys(derived)); !slices.Equal(found, classified) {
		t.Fatalf("%v write a field of the cache they are a method of and this table classifies %v; a method with no row is one nobody decided the binding question for, and a row with no method is a classification that outlived what it classified. Locations: %v",
			found, classified, derived)
	}
	ends := []string{}
	for name, one := range proposalCacheBindingWriters {
		if one.endsTheBinding {
			ends = append(ends, name)
		}
		if one.what == "" || one.probe == nil {
			t.Errorf("%s is classified with no account of what it does or no probe of it; a row that states nothing is the enumeration this gate exists to not be", name)
		}
	}
	slices.Sort(ends)
	if !slices.Equal(ends, []string{"(*ProposalCache).Rebind"}) {
		t.Errorf("the methods classified as ending the epoch binding are %v, want exactly [(*ProposalCache).Rebind]; the gate over the epoch movers accepts a call to any of them, so a name added here widens that gate",
			ends)
	}
	// and every one of them takes exactly one group context, because the gate over the epoch
	// movers reads WHICH context an ender was handed. An ender declaring none is one that half
	// of the rule cannot read at all, so every mover calling it would be refused for a reason
	// its author could not act on; an ender declaring two is one nothing can say the binding
	// came from. The demand belongs here, where the classification is made, rather than being
	// discovered at the call sites -- and (*ProposalCache).Release in the mover control is the
	// shape it refuses.
	for _, name := range epochBindingEnders() {
		method, held := reflect.TypeOf(&ProposalCache{}).MethodByName(name)
		if !held {
			t.Errorf("%s is classified as ending the epoch binding and is not a method of *%s the compiler can find, so the gate over the epoch movers accepts a call nothing here describes",
				name, epochCacheTypeName)
			continue
		}
		contexts := 0
		for at := 1; at < method.Type.NumIn(); at++ {
			// the VERIFIED context and not the bare one. An ender takes a group context
			// whose authority has been established -- the type whose only constructor is
			// (*GroupInfo).VerifiedContext -- because a binding is only worth the
			// authority of the value it was taken from, and every *GroupContext names some
			// epoch whoever wrote the octets chose. An ender that widened back to the bare
			// type is a door onto the epoch this cache believes it is in, and it fails here.
			// both halves TRANSITIVELY, for the reason
			// TestNeitherWriterOfTheCacheBindingTakesABareGroupContext asks it that
			// way: an ender taking []*GroupContext is the same door as one taking a
			// bare pointer, and a *VerifiedGroupContext reaches no group context
			// because its field is unexported
			if reflectTypeReaches(method.Type.In(at),
				[]reflect.Type{reflect.TypeOf(&VerifiedGroupContext{})}) {
				contexts++
			}
			if reflectTypeReaches(method.Type.In(at), reachGroupContextTargets) {
				t.Errorf("%s is classified as ending the epoch binding and takes %s, which reaches a bare %s; that is a claim about a struct's fields and not about anybody's authority, and this package decodes one straight off peer octets",
					name, method.Type.In(at), epochGroupContextTypeName)
			}
		}
		if contexts != 1 {
			t.Errorf("%s takes %d *Verified%s parameters and the argument half of the mover gate reads exactly one, which is the context it holds a boundary to having moved to; with none, or with two, that half is vacuous",
				name, contexts, epochGroupContextTypeName)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(proposalCacheBindingWriters)) {
		one := proposalCacheBindingWriters[name]
		t.Run(name, func(t *testing.T) { one.probe(t) })
	}
}

// epochBindingEnders is the method names the gate below accepts, taken from the table above rather
// than written out: exactly the methods a human has classified as ending a binding.
func epochBindingEnders() []string {
	names := []string{}
	for name, one := range proposalCacheBindingWriters {
		if !one.endsTheBinding {
			continue
		}
		// the bare method name, which is what a call site spells
		names = append(names, name[strings.LastIndex(name, ".")+1:])
	}
	slices.Sort(names)
	return names
}

// epochMoversOfEveryRoot is the derived class over both scanned roots, with what each was found
// doing.
func epochMoversOfEveryRoot(t *testing.T) map[string]epochMoverFinding {
	t.Helper()
	found := map[string]epochMoverFinding{}
	writes := 0
	enders := epochBindingEnders()
	for _, root := range epochMoverRoots {
		checked := typeCheckedBodiesOf(t, root)
		if len(checked.paths) == 0 {
			t.Fatalf("%s holds no non test source, so this gate scanned nothing there", root)
		}
		scan := epochMoversIn(checked, epochCacheTypeName, enders)
		writes += scan.writes
		for name, finding := range scan.moving {
			if held, already := found[name]; already {
				t.Fatalf("%s moves a group between epochs at %s and at %s; this table is keyed by name, so the two would share one row and one of them would go unclassified",
					name, held.where, finding.where)
			}
			found[name] = finding
		}
	}
	if writes == 0 {
		t.Fatalf("no write through a selector was resolved across %v, so this gate would report a clean run over a package that advanced its epoch in every method it has",
			epochMoverRoots)
	}
	t.Logf("%d field writes across %v, of which %d move a group between epochs", writes, epochMoverRoots, len(found))
	return found
}

// TestEveryDeclarationThatMovesAGroupToAnotherEpochEndsTheProposalCacheBinding is rule 5 over the
// question the plan answered with two hand written Clear() calls.
//
// Two call sites is an enumeration of the epoch boundaries somebody thought of, and the thing an
// enumeration cannot do is fail on the third one. This does: a declaration that writes a group
// context, or one of the two fields the cache binds to, into storage that outlives the call either
// carries a row saying it does not move a group between epochs, or ends the binding of every cache
// the value it moved holds, on every path that leaves it.
//
// The matcher runs on the control first, which is what says it reads anything at all -- and here
// that is load bearing rather than customary, because the real source has no member of the class
// this gate's demand is stated over. An empty report over mls and an empty report from a matcher
// that resolved nothing are the same value.
func TestEveryDeclarationThatMovesAGroupToAnotherEpochEndsTheProposalCacheBinding(t *testing.T) {
	for _, field := range epochBoundFields {
		if _, held := reflect.TypeOf(GroupContext{}).FieldByName(field); !held {
			t.Fatalf("%s declares no %s field, so the half of the rule stated over the bound fields matches nothing",
				epochGroupContextTypeName, field)
		}
	}
	// the scope, held to the one the crypto guardrails walk rather than left as an alias a
	// later edit can quietly replace with a literal. Measured on the gate next door: written
	// as a restatement and narrowed to []string{"."}, the whole of ./mls/... and ./message/...
	// stayed green, because ../message declares no group context today -- so the paragraph
	// beside epochMoverRoots would be an argument no test could lose. Narrowing
	// forbiddenScanRoots itself fails TestHkdfExtractHasOnlyTwoCallSites, which is G1 and
	// predates this file, so this borrows a scope something already pins.
	if !slices.Equal(epochMoverRoots, forbiddenScanRoots) {
		t.Fatalf("this gate walks %v and the package's guardrails walk %v; a scope of its own is a scope nothing holds, and the root it would drop first is the one that declares no group context yet",
			epochMoverRoots, forbiddenScanRoots)
	}

	// the control's enders are spelled Clear and Release and the real one is spelled Rebind,
	// deliberately: the ender names are a PARAMETER of the matcher, and a control that shared
	// the real spelling could not tell a matcher that reads the parameter from one that
	// hardcodes the name it expects to find. Two of them rather than one because the argument
	// half is stated over the ender's own SIGNATURE: Clear takes the context it binds to,
	// Release takes none, ClearAt takes it second and ClearBetween takes two. A matcher that
	// assumed every ender carries exactly one context, or that read argument zero, answers
	// wrongly on (*Group).mergeAndReleaseWithoutAContext,
	// (*Group).mergeAndClearBetweenTwoContexts and (*Group).mergeAndClearAtWithTheContext.
	control := epochMoversIn(typeCheckedBodiesOfText(t, "the epoch mover control", epochMoverControl),
		epochCacheTypeName, []string{"Clear", "ClearAt", "ClearBetween", "Release"})
	if reported := slices.Sorted(maps.Keys(control.moving)); !slices.Equal(reported, epochMoverControlReports) {
		t.Fatalf("the rule reported %v out of the control, want %v; a rule that reports the staging of a local demands a row for every commit path in this plan, and one that misses the increment form reports a clean run over the cheapest epoch advance there is",
			reported, epochMoverControlReports)
	}
	controlEnders := []string{}
	controlFinds := []string{}
	for name, finding := range control.moving {
		if finding.ends != "" {
			controlEnders = append(controlEnders, name)
		}
		if finding.finds != "" {
			controlFinds = append(controlFinds, name)
		}
	}
	slices.Sort(controlEnders)
	slices.Sort(controlFinds)
	if !slices.Equal(controlFinds, epochMoverControlFinds) {
		t.Fatalf("the rule found a call ending one of their own caches in %v of the control, want %v; this is the set the reachability half of the rule is measured against, and a matcher that never looked inside a branch would report the same enders as one that does",
			controlFinds, epochMoverControlFinds)
	}
	if !slices.Equal(controlEnders, epochMoverControlEnders) {
		t.Fatalf("the rule read %v of the control as ending the cache binding, want %v; one that asks only whether the call APPEARS accepts it under `if false`, after an early return, and on a second cache the group holds, and one that finds none would refuse every merge ever written",
			controlEnders, epochMoverControlEnders)
	}

	derived := epochMoversOfEveryRoot(t)
	classified := slices.Sorted(maps.Keys(groupContextEpochMovers))
	if found := slices.Sorted(maps.Keys(derived)); !slices.Equal(found, classified) {
		t.Fatalf("%v write a group context or one of its bound fields into storage that outlives the call, and this table classifies %v; a declaration with no row is an epoch boundary nobody decided the cache question for, and a row with no declaration is a classification that outlived what it classified. Locations: %v",
			found, classified, derived)
	}
	// the positive control on the real source. The decoder certainly writes an epoch into a
	// group context, so a scan that had stopped reading this package would report the same
	// clean run a complete one reports over a package with no movers.
	if _, held := derived["(*GroupContext).UnmarshalMLS"]; !held {
		t.Fatalf("the scan read %v as the declarations writing a group context epoch and the decoder is not among them, so it is not reading what it claims to",
			slices.Sorted(maps.Keys(derived)))
	}

	for _, name := range slices.Sorted(maps.Keys(groupContextEpochMovers)) {
		one := groupContextEpochMovers[name]
		finding := derived[name]
		if one.what == "" || one.probe == nil {
			t.Errorf("%s is classified with no account of what it does or no probe of it; a row that states nothing is the enumeration this gate exists to not be", name)
		}
		if one.movesAGroupBetweenEpochs && finding.ends == "" {
			t.Errorf("%s moves a group to another epoch at %s (%s) and does not end every cache binding the value it moved holds, on every path out of it, with the context it moved to. Found: %q. Handed a context it had not moved: %q. Left bound: %s. A cache left behind belongs to the epoch that just closed and every reference in it is a proposal the group has already applied; a cache rebound to that same closing epoch is worse, because it also refuses every proposal of the new one and nothing in this package releases it",
				name, finding.where, finding.how, finding.finds, finding.wrong, finding.skipped)
		}
		if !one.movesAGroupBetweenEpochs && finding.ends != "" {
			t.Errorf("%s is classified as moving no group between epochs and ends a cache binding at %s; one of the two is wrong and a reader cannot tell which",
				name, finding.ends)
		}
		t.Logf("%s at %s writes %q: %s", name, finding.where, finding.how, one.what)
	}
}

// TestEveryClassifiedEpochMoverBehavesAsItIsClassified runs each row's probe, so a row is an
// assertion rather than a label.
//
// The gate above compares two sets of names and would pass over a table whose every account was
// wrong. What separates "constructs a group context out of a message" from "moves the live group
// to another epoch" is not visible in any name or signature: both write the same field of the same
// type. So each row states its claim as an input and an answer.
func TestEveryClassifiedEpochMoverBehavesAsItIsClassified(t *testing.T) {
	if len(groupContextEpochMovers) == 0 {
		t.Fatal("the classification table is empty, so this runs nothing")
	}
	for _, name := range slices.Sorted(maps.Keys(groupContextEpochMovers)) {
		one := groupContextEpochMovers[name]
		t.Run(name, func(t *testing.T) { one.probe(t) })
	}
}
