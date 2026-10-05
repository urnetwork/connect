// The RELATIONS the two doors decide by, and the claim the corpus owes each of them.
//
// THE GATE NEXT DOOR MEASURES EACH DIMENSION ALONE, and that is the hole this file closes. Every
// claim fixture_corpus_test.go states has the shape "no path holds one value across the corpus".
// Separation is not discrimination: a corpus in which two dimensions are separately varied and
// JOINTLY DEGENERATE -- always equal, or never equal -- is one in which a rule comparing them and a
// rule comparing one of them against a constant are the same program, and the dimension claim sees
// nothing wrong with it.
//
// BOTH SURVIVORS OF THE LAST ROUND WERE THAT SHAPE, measured rather than supposed:
//
//	ValSem203PathDecrypt's `if in.Own == in.Committer` -> `if in.Own == LeafIndex(0)`     GREEN
//	                                                   -> `if false`                      GREEN
//	checkExtensionsAreTheSetThisCommitInstalls's `if self.Extensions != nil` -> `if false` GREEN
//
// Own was 0 and Committer 1 in every default fixture, so the two were separated as dimensions and
// pinned as a relation; and every fixture that filled Extensions filled it with the very vector the
// join compares it against, so the join could be deleted outright.
//
// SO THE CLAIM IS STATED OVER THE PAIR. For every pair of paths a rule of this package compares,
// the corpus must hold a fixture where the two are EQUAL and a fixture where they DIFFER. A pair
// that is always equal cannot tell `a == b` from `a == a`; a pair that never is cannot tell it from
// `false`. Both witnesses together are what make the comparison a comparison.
//
// AND THE PAIRS ARE DERIVED, NOT LISTED, which is ledger 21 and is the whole reason this file is
// three hundred lines rather than a table. A written list of pairs understates its class the moment
// a rule grows a clause -- fourteen times on this project -- so the class is read off the AST of
// this package's own source: every equality, every inequality, and every call to a member of the
// comparator class constant_time_test.go already derives from the package's imports, wherever both
// of its operands trace back to a field of a validation input. A rule added tomorrow is in the
// class on the run after it is written, and a rule whose comparison is deleted takes its pair with
// it.
//
// AND THE SCOPE IS DERIVED TOO, which is the round after that one and the same ledger entry pointed
// one level up. The walk used to read a function only where its own signature named a validation
// input, so the helpers a rule is written out of were invisible: joinCachedProposals, which holds
// the vector the sender signed against the list this member resolved; CheckUpdatePathKeyUniqueness,
// which is the whole body of ValSem207; checkProposalProfile, which is where a forged proposal
// discriminant is refused. All three are door logic and none of them takes a door's input. So the
// walk follows a rule into the package's own functions, binding their parameters to the paths the
// caller passed, and comparisonsIn is where that is written.
//
// TWO PLACES IT DELIBERATELY STOPS, each with its reason on the line: a call through an interface,
// because CryptoProvider exists so a deployment can supply its own body and the comparisons inside
// whichever one this repository ships are not comparisons either door makes; and a path through an
// unexported method, because the corpus reaches values by reflection and reflection reaches
// exported methods only, so recording one would state a claim over a value no fixture can produce.
package mls

import (
	"go/ast"
	"go/token"
	"go/types"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// a path into a validation input, as the source spells it
// ---------------------------------------------------------------------------

// pStep is one hop of a path a rule walks: a field selection, a slice expansion, or a method call
// whose arguments are themselves paths into the same input.
type pStep struct {
	kind string // field, elem, call
	name string
	args []pPath
}

// pPath is one whole path from a validation input to a value a rule compares, and length says the
// comparison was over len(...) of it rather than over the value.
type pPath struct {
	steps  []pStep
	length bool
}

// String renders a path the way the source spells it, which is what the failure messages below
// name and what a reviewer greps for.
func (self pPath) String() string {
	out := ""
	for _, step := range self.steps {
		switch step.kind {
		case "field":
			if out == "" {
				out = step.name
			} else {
				out = out + "." + step.name
			}
		case "elem":
			out = out + "[]"
		case "call":
			parts := []string{}
			for _, one := range step.args {
				parts = append(parts, one.String())
			}
			if out == "" {
				out = step.name + "(" + strings.Join(parts, ", ") + ")"
			} else {
				out = out + "." + step.name + "(" + strings.Join(parts, ", ") + ")"
			}
		}
	}
	if self.length {
		return "len(" + out + ")"
	}
	return out
}

// arity is how many positions a path spreads over: one per slice expansion it walks, WHEREVER that
// expansion is written.
//
// AN ARGUMENT'S EXPANSION COUNTS AS MUCH AS THE PATH'S OWN. Tree.Leaf(List.Updates()[].Sender)
// .EncryptionKey answers one value per update, exactly as List.Updates()[].Proposal.Update
// .LeafNode.EncryptionKey does, and validateUpdateChangesTheEncryptionKey compares the two update
// by update. A count that read only the top level would call the first of those a single value and
// pair every update's key against every leaf, which is a weaker demand than the rule makes.
func (self pPath) arity() int {
	count := 0
	for _, step := range self.steps {
		if step.kind == "elem" {
			count += 1
		}
		for _, one := range step.args {
			count += one.arity()
		}
	}
	return count
}

// pathsOf answers every path a validation input reaches an expression by, or nothing where the
// expression is not rooted at one.
//
// SEVERAL PATHS AND NOT ONE, because a local may have been assigned from more than one place and a
// rule that compares such a local compares whichever of them the run took.
func pathsOf(expr ast.Expr, roots map[string]bool, locals map[string][]pPath) []pPath {
	switch node := expr.(type) {
	case *ast.Ident:
		if roots[node.Name] {
			return []pPath{{}}
		}
		return locals[node.Name]
	case *ast.ParenExpr:
		return pathsOf(node.X, roots, locals)
	case *ast.StarExpr:
		return pathsOf(node.X, roots, locals)
	case *ast.UnaryExpr:
		if node.Op == token.AND {
			return pathsOf(node.X, roots, locals)
		}
		return nil
	case *ast.SelectorExpr:
		base := pathsOf(node.X, roots, locals)
		out := []pPath{}
		for _, one := range base {
			if one.length {
				continue
			}
			out = append(out, pPath{steps: append(slices.Clone(one.steps),
				pStep{kind: "field", name: node.Sel.Name})})
		}
		return out
	case *ast.IndexExpr:
		base := pathsOf(node.X, roots, locals)
		out := []pPath{}
		for _, one := range base {
			if one.length {
				continue
			}
			out = append(out, pPath{steps: append(slices.Clone(one.steps), pStep{kind: "elem"})})
		}
		return out
	case *ast.CallExpr:
		if ident, isIdent := node.Fun.(*ast.Ident); isIdent && ident.Name == "len" && len(node.Args) == 1 {
			base := pathsOf(node.Args[0], roots, locals)
			out := []pPath{}
			for _, one := range base {
				if one.length {
					continue
				}
				out = append(out, pPath{steps: one.steps, length: true})
			}
			return out
		}
		selector, isSelector := node.Fun.(*ast.SelectorExpr)
		base := []pPath{}
		if isSelector {
			base = pathsOf(selector.X, roots, locals)
		}
		// A CALL WHOSE CALLEE IS NOT ITSELF A PATH IS A FUNCTION OF ITS ARGUMENT, and where
		// there is exactly one argument the value it answers is a value of that argument
		// alone: string(key), slices.Clone(key), proposalOctets(&entry.Proposal), and the
		// octets accessor every field of joinCachedProposals reaches its two entries
		// through. A refactor that put a conversion in front of a comparison would
		// otherwise take that comparison out of the class with nothing saying so.
		//
		// THE CALL IS DROPPED RATHER THAN RECORDED, because a corpus walking the path has
		// no receiver to make it on, and the claim that leaves is stated over the argument.
		// That is the direction that asks for MORE: a == b gives f(a) == f(b) for any f, so
		// an equal witness carries over unchanged, and the differ witness is demanded of
		// the argument itself rather than of the function of it.
		//
		// IT IS NOT ENOUGH FOR joinCachedProposals AND THIS IS WHERE TO READ WHY. That join
		// holds the entry the commit's vector NAMES against the entry the list resolved,
		// and the first of those comes back from entryTheCommitNames -- an unexported
		// method, which the rule below refuses to spell because the corpus makes its calls
		// by reflection and reflection reaches exported methods only. So one side of the
		// join stays unspellable, the pair is never formed, and the join contributes
		// nothing here. What holds it is commit_vector_join_test.go rather than this file.
		if len(base) == 0 {
			if len(node.Args) == 1 {
				return pathsOf(node.Args[0], roots, locals)
			}
			return nil
		}
		// A CALL STEP IS A CALL THE CORPUS HAS TO MAKE, and it makes it by reflection --
		// which reaches a type's EXPORTED methods and no others. So a path through an
		// unexported one is a path no fixture can ever answer, and recording it would state
		// the pair claim over a value nothing can produce: a permanent red that no corpus
		// could go green against. Derived from the name's own case rather than from a list
		// of the two this package writes today.
		if !ast.IsExported(selector.Sel.Name) {
			return nil
		}
		args := []pPath{}
		for _, one := range node.Args {
			resolved := pathsOf(one, roots, locals)
			if len(resolved) != 1 {
				return nil
			}
			args = append(args, resolved[0])
		}
		out := []pPath{}
		for _, one := range base {
			if one.length {
				continue
			}
			out = append(out, pPath{steps: append(slices.Clone(one.steps),
				pStep{kind: "call", name: selector.Sel.Name, args: args})})
		}
		return out
	}
	return nil
}

// localsIn answers, for one function body, every local that holds a path into a validation input,
// starting from the bindings its caller already made for it.
//
// THE SEED IS HOW A RULE'S OWN HELPER IS READ. comparisonsIn follows a call into a function this
// package declares with that function's parameters bound to the paths the caller passed, and those
// bindings arrive here; a body walked with no seed is one reached through its own root.
//
// FOUR ROUNDS TO A FIXED POINT rather than one, because a rule routinely names a value through two
// locals -- adds := in.List.Adds() and then kp := &adds[i].Proposal.Add.KeyPackage -- and a single
// pass over the body resolves the second only if it happens to come after the first in AST order.
// Four is past the deepest chain this package writes and is a bound rather than a loop until
// nothing changes, so a body with a cycle in it terminates.
func localsIn(body *ast.BlockStmt, roots map[string]bool,
	seed map[string][]pPath) map[string][]pPath {

	locals := map[string][]pPath{}
	for name, found := range seed {
		locals[name] = slices.Clone(found)
	}
	remember := func(name string, found []pPath) {
		for _, one := range found {
			if !slices.ContainsFunc(locals[name], func(other pPath) bool {
				return other.String() == one.String()
			}) {
				locals[name] = append(locals[name], one)
			}
		}
	}
	for round := 0; round < 4; round += 1 {
		ast.Inspect(body, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.AssignStmt:
				if len(node.Rhs) == len(node.Lhs) {
					for i := range node.Lhs {
						ident, isIdent := node.Lhs[i].(*ast.Ident)
						if !isIdent || ident.Name == "_" {
							continue
						}
						remember(ident.Name, pathsOf(node.Rhs[i], roots, locals))
					}
				} else if len(node.Rhs) == 1 && len(node.Lhs) > 1 {
					// a two value call: the first result is the value, the rest are
					// the error or the presence flag beside it
					ident, isIdent := node.Lhs[0].(*ast.Ident)
					if isIdent && ident.Name != "_" {
						remember(ident.Name, pathsOf(node.Rhs[0], roots, locals))
					}
				}
			case *ast.DeclStmt:
				// A `var` BINDS EXACTLY AS `:=` DOES, and this arm is here because the
				// walk read only the second. Measured on this tree: rewriting the one
				// `:=` of ValSem206PathLeafEncryptionKeyUnique as a `var` deleted THREE
				// of the fourteen pairs at the commit door and left every claim green,
				// because none of them is a count -- the log moved from "14 of 14" to
				// "11 of 11" and both read as success.
				general, isGeneral := node.Decl.(*ast.GenDecl)
				if !isGeneral || general.Tok != token.VAR {
					return true
				}
				for _, spec := range general.Specs {
					valued, isValued := spec.(*ast.ValueSpec)
					if !isValued || len(valued.Values) != len(valued.Names) {
						continue
					}
					for i := range valued.Names {
						if valued.Names[i].Name == "_" {
							continue
						}
						remember(valued.Names[i].Name,
							pathsOf(valued.Values[i], roots, locals))
					}
				}
			case *ast.RangeStmt:
				over := []pPath{}
				for _, one := range pathsOf(node.X, roots, locals) {
					if one.length {
						continue
					}
					over = append(over, one)
				}
				if node.Value == nil {
					return true
				}
				ident, isIdent := node.Value.(*ast.Ident)
				if !isIdent || ident.Name == "_" {
					return true
				}
				expanded := []pPath{}
				for _, one := range over {
					expanded = append(expanded, pPath{steps: append(slices.Clone(one.steps),
						pStep{kind: "elem"})})
				}
				remember(ident.Name, expanded)
			}
			return true
		})
	}
	return locals
}

// ---------------------------------------------------------------------------
// the class: every pair of paths this package's rules compare
// ---------------------------------------------------------------------------

// comparisonPair is one pair of input paths some rule of this package compares, and the function
// it was read out of.
type comparisonPair struct {
	left  pPath
	right pPath
	in    string
	// viaCall is whether this was read inside a helper the rule calls rather than in a
	// frame of the input's own. See TestEveryDoorComparisonIsReadOffThisPackagesSource,
	// which is what stops the call walk from being deleted green.
	viaCall bool
}

// String is the pair as the failure messages name it, and it is the key the corpus is measured
// under.
func (self comparisonPair) String() string {
	return self.left.String() + "  <=>  " + self.right.String()
}

// positional is whether the two sides are compared POSITION BY POSITION rather than each against
// each.
//
// EQUAL ARITY IS WHAT SAYS SO. Extensions[].ExtensionType against
// Context.Extensions[].ExtensionType is the join's self.Extensions[i] against installed[i], and
// pairing those across the whole cross product would let a vector of two unlike extensions witness
// "these differ" without any fixture ever announcing a set the commit does not install -- the very
// degeneracy this file exists to refuse. Where the arities differ the rule really is a loop against
// a single value, and the cross product is what it does.
//
// ONE PAIR IS COMPARED MORE LOOSELY THAN THIS DEMANDS and that is deliberate: ValSem203PathDecrypt
// crosses the committer's filtered direct path against this member's, so pairing them by position
// asks the corpus for MORE than the rule reads. More is the safe direction for a corpus claim --
// every positional witness is also a cross-product witness, so nothing this admits would fail the
// rule's own pairing.
func (self comparisonPair) positional() bool {
	return self.left.arity() > 0 && self.left.arity() == self.right.arity()
}

// comparatorClassOf is every function of the packages this one imports that compares two byte
// strings, which is the class constant_time_test.go derives for guardrail 8 and is reused here
// unchanged.
//
// REUSED RATHER THAN RESTATED. A pair hidden behind bytes.Equal is a pair this file must see, and
// the set of spellings that hide one is exactly the set that gate already computes off the imports
// -- so a comparator the standard library ships tomorrow enters both classes on the same run.
func comparatorClassOf(t *testing.T, files []parsedSource) []string {
	t.Helper()
	found := []string{}
	for _, one := range importsOfSources(files) {
		scope := importedPackageOf(t, one.path).Scope()
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

// packageFunction is one function this package declares, and the file it was read out of.
type packageFunction struct {
	parsed parsedSource
	decl   *ast.FuncDecl
}

// packageFunctionsByName is every function this package declares, keyed by its own name.
func packageFunctionsByName(sources []parsedSource) map[string][]packageFunction {
	found := map[string][]packageFunction{}
	for _, parsed := range sources {
		for _, declared := range parsed.file.Decls {
			function, isFunction := declared.(*ast.FuncDecl)
			if !isFunction || function.Body == nil {
				continue
			}
			found[function.Name.Name] = append(found[function.Name.Name],
				packageFunction{parsed: parsed, decl: function})
		}
	}
	return found
}

// interfaceMethodsOf is every method name an interface this package declares carries.
//
// WHAT IT IS FOR is one line below: a call through an interface is a call whose body the CALLER
// chooses, and this package ships CryptoProvider precisely so a deployment can supply its own. So
// the comparisons in whatever implementation happens to sit in this repository are not comparisons
// either door makes -- ValSem205 reaching subtle.ConstantTimeCompare inside one provider is that
// provider decision, and the corpus would then be asked to separate a MAC length the build fixes at
// thirty-two octets from the literal 32, which is a fixture no build could produce.
func interfaceMethodsOf(sources []parsedSource) map[string]bool {
	found := map[string]bool{}
	for _, parsed := range sources {
		ast.Inspect(parsed.file, func(n ast.Node) bool {
			declared, isInterface := n.(*ast.InterfaceType)
			if !isInterface || declared.Methods == nil {
				return true
			}
			for _, method := range declared.Methods.List {
				for _, named := range method.Names {
					found[named.Name] = true
				}
			}
			return true
		})
	}
	return found
}

// comparisonWalk is the scope the derivation runs over: this package's functions by name, the input
// types whose own frames are walked separately, and the method names an interface answers.
type comparisonWalk struct {
	functions  map[string][]packageFunction
	inputs     []string
	dispatched map[string]bool
}

// comparisonCallHops is how many CALLS past a rule's own body the walk follows.
//
// A BOUND AND NOT A FIXED POINT, for localsIn's reason: a bound terminates on a call graph with a
// cycle in it, and the recursion guard beside it already refuses a name twice on one chain. Three
// is past the deepest chain of door logic this package writes -- a rule, the join it delegates to,
// and the accessor that join reads its two entries through -- and what lies deeper is a utility
// shared with the rest of the package rather than a decision either door makes.
//
// AND IT IS MEASURED RATHER THAN ASSERTED, because a bound nobody checked is the shape half this
// file exists to refuse: at eight hops the walk finds the same twenty-five pairs it finds at three,
// so this number is past the end of the graph rather than in the middle of it.
const comparisonCallHops = 3

// boundParametersOf binds one call's arguments to the callee's parameters, and its receiver to the
// callee's receiver, as paths in the CALLER's frame.
//
// EVERY POSITION OR NONE. A variadic callee, a call whose arguments are one multi-valued call, and
// a parameter list whose length does not match the call site are all shapes where position i of the
// call is not position i of the signature -- and a binding off by one position is a pair the
// derivation invented rather than read.
//
// ONE PATH OR NO BINDING, for the same reason one level down. A local resolves to EVERY path it was
// ever assigned from, which is the right answer where a rule compares it -- the run took one of
// them -- and the wrong one where it is carried into a callee, because localsIn is flat over a body
// and two sibling loops that name their variable alike are unioned. Measured: binding a
// multiply-resolved argument made the three Supports* helpers answer a nine pair block at the
// commit door and an eighteen pair block at the proposal door, crossing every required capability
// list against every capability vector -- and only the diagonal of each block is a comparison this
// package makes. A parameter the frame could have passed more than one value for is left unbound,
// so the callee's comparison over it spells no pair rather than a cross product of them.
func boundParametersOf(target packageFunction, receiver []pPath, call *ast.CallExpr,
	roots map[string]bool, locals map[string][]pPath) map[string][]pPath {

	bound := map[string][]pPath{}
	if target.decl.Recv != nil && len(receiver) == 1 {
		for _, field := range target.decl.Recv.List {
			for _, named := range field.Names {
				if named.Name != "_" {
					bound[named.Name] = receiver
				}
			}
		}
	}
	names := []*ast.Ident{}
	if target.decl.Type.Params != nil {
		for _, field := range target.decl.Type.Params.List {
			if _, isVariadic := field.Type.(*ast.Ellipsis); isVariadic {
				return bound
			}
			names = append(names, field.Names...)
		}
	}
	if len(names) != len(call.Args) {
		return bound
	}
	for i, named := range names {
		if named.Name == "_" {
			continue
		}
		if reached := pathsOf(call.Args[i], roots, locals); len(reached) == 1 {
			bound[named.Name] = reached
		}
	}
	return bound
}

// doorComparisonPairsMemo holds the derivation, which parses every source of this package and type
// checks every package it imports. Both corpora are measured against it and three tests ask for it,
// so it is computed once per run rather than once per asking.
var doorComparisonPairsMemo struct {
	once  sync.Once
	pairs map[string][]comparisonPair
	class []string
	lost  []string
}

// doorComparisonLosses is every local of a frame this walk enters that holds a path into a
// validation input and that the walk failed to bind one for.
//
// IT IS EMPTY OR THE DERIVATION IS INCOMPLETE, which is what
// TestEveryDoorComparisonIsReadOffThisPackagesSource states over it. See bindingsLostIn for why
// this and not a count.
func doorComparisonLosses(t *testing.T) []string {
	t.Helper()
	doorComparisonPairs(t)
	return doorComparisonPairsMemo.lost
}

// doorComparisonPairs answers, for each validation input type this package declares, every pair of
// that input's paths some rule compares.
//
// ONE PASS PER INPUT TYPE rather than one pass over the functions, so that a rule taking two
// different inputs contributes its pairs to each of them under the roots that belong to it. There
// is no such rule today; deriving it this way is what stops the first one from silently landing all
// of its pairs in whichever corpus the walk happened to name.
func doorComparisonPairs(t *testing.T) (map[string][]comparisonPair, []string) {
	t.Helper()
	doorComparisonPairsMemo.once.Do(func() {
		sources := packageSources(t)
		class := comparatorClassOf(t, sources)
		walk := comparisonWalk{functions: packageFunctionsByName(sources),
			inputs:     validationInputTypesInSource(t),
			dispatched: interfaceMethodsOf(sources)}
		pairs := map[string][]comparisonPair{}
		lost := []string{}
		for _, named := range walk.inputs {
			seen := map[string]bool{}
			for _, parsed := range sources {
				for _, declared := range parsed.file.Decls {
					function, isFunction := declared.(*ast.FuncDecl)
					if !isFunction || function.Body == nil {
						continue
					}
					roots := rootsOfType(parsed, function, named)
					if len(roots) == 0 {
						continue
					}
					read, dropped := comparisonsIn(parsed, function, roots, nil,
						class, walk, comparisonCallHops,
						[]string{function.Name.Name})
					for _, one := range dropped {
						if !slices.Contains(lost, one) {
							lost = append(lost, one)
						}
					}
					for _, pair := range read {
						if seen[pair.String()] {
							continue
						}
						seen[pair.String()] = true
						pairs[named] = append(pairs[named], pair)
					}
				}
			}
			slices.SortFunc(pairs[named], func(one comparisonPair, other comparisonPair) int {
				return strings.Compare(one.String(), other.String())
			})
		}
		slices.Sort(lost)
		doorComparisonPairsMemo.pairs, doorComparisonPairsMemo.class = pairs, class
		doorComparisonPairsMemo.lost = lost
	})
	return doorComparisonPairsMemo.pairs, doorComparisonPairsMemo.class
}

// rootsOfType answers the names one function reaches a validation input of the named type by: its
// receiver where it has one of that type, and every parameter of it.
func rootsOfType(parsed parsedSource, function *ast.FuncDecl, named string) map[string]bool {
	roots := map[string]bool{}
	fields := []*ast.Field{}
	if function.Recv != nil {
		fields = append(fields, function.Recv.List...)
	}
	if function.Type.Params != nil {
		fields = append(fields, function.Type.Params.List...)
	}
	for _, field := range fields {
		if strings.TrimPrefix(parsed.render(field.Type), "*") != named {
			continue
		}
		for _, name := range field.Names {
			roots[name.Name] = true
		}
	}
	return roots
}

// bindingsLostIn answers every local of one frame whose bound expression pathsOf CAN spell and
// that localsIn did not record a path for.
//
// THIS IS WHAT THE CLASS SIZE IS HELD AGAINST, and it is here because no count could be. Every
// claim over these pairs is stated per pair, so a walk that quietly stops finding three of them
// states three fewer claims in exactly the same words: measured on this tree, rewriting the one
// `:=` of ValSem206PathLeafEncryptionKeyUnique as a `var` deleted three of the fourteen pairs at
// the commit door, moved the log from "14 of 14 compared pairs" to "11 of 11", and moved no
// assertion at all. A literal fourteen beside it would be the same hole with a maintenance burden
// attached -- the next person to add a rule edits the number to match.
//
// WHAT DOES NOT MOVE WITH THE CLASS IS THE TWO HALVES OF THE WALK AGREEING. pathsOf says which
// expressions name a value of the input; localsIn says which of a frame's locals hold one. A local
// bound from an expression pathsOf resolves and that localsIn did not bind is a binding FORM the
// walk cannot read -- a `var` where it reads only `:=`, a tuple where it reads only a pair -- and
// every comparison that names such a local silently leaves the class. The claim is zero of them,
// whatever the class size is, and the walk that answers it is the walk under test rather than a
// second opinion about it.
//
// THE SAME STATEMENT SHAPES AND THE SAME ORDER as localsIn, and only the shapes it binds FROM: the
// key of a range is not one localsIn records, so this does not ask for it.
func bindingsLostIn(body *ast.BlockStmt, roots map[string]bool,
	locals map[string][]pPath) []string {

	lost := []string{}
	check := func(name *ast.Ident, value ast.Expr) {
		if name == nil || name.Name == "_" || value == nil {
			return
		}
		if len(locals[name.Name]) != 0 || len(pathsOf(value, roots, locals)) == 0 {
			return
		}
		if !slices.Contains(lost, name.Name) {
			lost = append(lost, name.Name)
		}
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.AssignStmt:
			if len(node.Rhs) == len(node.Lhs) {
				for i := range node.Lhs {
					ident, isIdent := node.Lhs[i].(*ast.Ident)
					if isIdent {
						check(ident, node.Rhs[i])
					}
				}
			} else if len(node.Rhs) == 1 && len(node.Lhs) > 1 {
				ident, isIdent := node.Lhs[0].(*ast.Ident)
				if isIdent {
					check(ident, node.Rhs[0])
				}
			}
		case *ast.DeclStmt:
			general, isGeneral := node.Decl.(*ast.GenDecl)
			if !isGeneral || general.Tok != token.VAR {
				return true
			}
			for _, spec := range general.Specs {
				valued, isValued := spec.(*ast.ValueSpec)
				if !isValued || len(valued.Values) != len(valued.Names) {
					continue
				}
				for i := range valued.Names {
					check(valued.Names[i], valued.Values[i])
				}
			}
		case *ast.RangeStmt:
			ident, isIdent := node.Value.(*ast.Ident)
			if isIdent {
				check(ident, node.X)
			}
		}
		return true
	})
	return lost
}

// comparisonsIn answers every pair of input paths one function compares.
//
// TWO SPELLINGS AND BOTH DERIVED. The equality operators are the language's own, and a call to a
// member of the comparator class is the same question written as a function --
// subtle.ConstantTimeCompare is how this package is required to compare every octet string it
// ships, so a walk that read only the operators would miss every key comparison in both doors.
func comparisonsIn(parsed parsedSource, function *ast.FuncDecl, roots map[string]bool,
	seed map[string][]pPath, class []string, walk comparisonWalk, hops int,
	stack []string) ([]comparisonPair, []string) {

	locals := localsIn(function.Body, roots, seed)
	found := []comparisonPair{}
	lost := []string{}
	for _, name := range bindingsLostIn(function.Body, roots, locals) {
		lost = append(lost, function.Name.Name+": "+name)
	}
	seen := map[string]bool{}
	ast.Inspect(function.Body, func(n ast.Node) bool {
		var left, right []pPath
		var leftExpr, rightExpr ast.Expr
		switch node := n.(type) {
		case *ast.BinaryExpr:
			if node.Op != token.EQL && node.Op != token.NEQ {
				return true
			}
			leftExpr, rightExpr = node.X, node.Y
		case *ast.CallExpr:
			if !slices.Contains(class, parsed.render(node.Fun)) || len(node.Args) < 2 {
				return true
			}
			leftExpr, rightExpr = node.Args[0], node.Args[1]
		default:
			return true
		}
		left, right = pathsOf(leftExpr, roots, locals), pathsOf(rightExpr, roots, locals)
		for _, one := range left {
			for _, other := range right {
				first, second := one, other
				if first.String() > second.String() {
					first, second = second, first
				}
				// a path against ITSELF is not a relation, and neither is a
				// comparison one side of which never left the input's root
				if first.String() == "" || second.String() == "" ||
					first.String() == second.String() {
					continue
				}
				pair := comparisonPair{left: first, right: second, in: function.Name.Name}
				if seen[pair.String()] {
					continue
				}
				seen[pair.String()] = true
				found = append(found, pair)
			}
		}
		return true
	})
	if hops <= 0 {
		return found, lost
	}
	// AND THE HELPERS THE RULE IS WRITTEN OUT OF, with their parameters bound to the paths
	// this frame passed them. Without this the scope of the walk is "a function whose
	// signature names a validation input", which is an enumeration wearing a derivation's
	// clothes: joinCachedProposals holds the vector the sender signed against the list this
	// member resolved over EVERY field of a CachedProposal, CheckUpdatePathKeyUniqueness is
	// the whole body of ValSem207, checkProposalProfile is where a forged discriminant is
	// refused -- all three are door logic and none of them takes a validation input.
	//
	// WHAT IT ACTUALLY BROUGHT IN, measured rather than hoped for: checkProposalProfile's
	// pair at both doors, bindingHolds' two at the commit door, and one of SupportsExtension.
	// joinCachedProposals still spells nothing, for the reason recorded on the exported
	// method rule in pathsOf, and CheckUpdatePathKeyUniqueness writes no comparison between
	// two paths at all -- it compares a node index taken out of a map against the leaf the
	// signature key found. Both are IN the scope now and contribute nothing, which is a
	// different fact from being outside it and is the one a reader can check.
	ast.Inspect(function.Body, func(n ast.Node) bool {
		call, isCall := n.(*ast.CallExpr)
		if !isCall {
			return true
		}
		name, receiver := "", []pPath{}
		switch callee := call.Fun.(type) {
		case *ast.Ident:
			name = callee.Name
		case *ast.SelectorExpr:
			name, receiver = callee.Sel.Name, pathsOf(callee.X, roots, locals)
		default:
			return true
		}
		if slices.Contains(stack, name) || walk.dispatched[name] {
			return true
		}
		// AMBIGUITY IS NOT FOLLOWED. Two declarations of one name are two methods on
		// different receivers, and there is no type information here to say which was
		// called; binding the wrong one's parameters would invent a pair rather than
		// read one.
		declared := walk.functions[name]
		if len(declared) != 1 {
			return true
		}
		target := declared[0]
		// A CALLEE THAT TAKES A VALIDATION INPUT IS WALKED IN ITS OWN FRAME, under the
		// door whose input it takes, so following it from here would restate its pairs
		// behind a path prefix and ask the corpus to separate each of them twice.
		for _, named := range walk.inputs {
			if len(rootsOfType(target.parsed, target.decl, named)) != 0 {
				return true
			}
		}
		bound := boundParametersOf(target, receiver, call, roots, locals)
		if len(bound) == 0 {
			return true
		}
		deeper, dropped := comparisonsIn(target.parsed, target.decl, nil, bound, class,
			walk, hops-1, append(slices.Clone(stack), name))
		for _, pair := range deeper {
			if seen[pair.String()] {
				continue
			}
			seen[pair.String()] = true
			pair.viaCall = true
			found = append(found, pair)
		}
		lost = append(lost, dropped...)
		return true
	})
	return found, lost
}

// ---------------------------------------------------------------------------
// what one fixture says about one pair
// ---------------------------------------------------------------------------

// corpusPairVerdict is what one fixture witnesses about one pair: whether the pair was reachable in
// it at all, the VALUES the two were equal at, and the values each side carried where they were not.
//
// VALUES AND NOT TWO FLAGS, which is the second constant this file was written to refuse and the
// one it left standing. A pair witnessed equal in one fixture and unequal in another is separated
// from ONE constant; it is separated from EVERY constant only when the value it agrees at moves, or
// when some fixture carries that value on one side while the two disagree. Measured: the previous
// round moved forty-nine call sites off LeafIndex(0) so that ValSem111's
// `updates[i].Sender == in.Committer` would stop being `== LeafIndex(0)` -- and every fixture that
// reaches that rule now carries Committer = 1, so it became `== LeafIndex(1)` instead and the gate
// reported "6 of 6 pairs witnessed both equal and unequal" either way.
//
// BOTH HALVES CAN BE WITNESSED AT ONCE and that is not a contradiction: a pair spread over a vector
// is compared once per position, so a fixture whose first extension matches and whose second does
// not witnesses both by itself. A fixture that reaches neither side witnesses nothing, which is why
// reached is carried rather than inferred from the rest.
type corpusPairVerdict struct {
	reached     bool
	agreed      []string
	stable      []string
	differLeft  []string
	differRight []string
}

// corpusStableAgreementsIn marks, on one fixture's verdicts, the agreement values a SECOND build of
// the same fixture reached too.
//
// A CONSTANT IN THE SOURCE CAN ONLY BE A VALUE THE BUILD REPRODUCES, and that is the whole of what
// this is for. The every-constant claim next door asks a pair that agrees at one value to carry
// that value somewhere the two disagree -- which is the right demand for a leaf index, a protocol
// version or a group id, and an impossible one for a freshly generated encryption key: ValSem206
// and ValSem204 agree exactly where a fixture made two keys collide, and the octets they collide at
// are different every run, so no line anybody could write into validate_commit.go is that value.
//
// MEASURED RATHER THAN TYPED. The alternative is a list of the kinds of value a constant may be,
// which is the enumeration this file exists not to be -- and it would be wrong in both directions,
// since a group id is octets and a MAC length is an integer. Building the fixture twice answers the
// question the claim actually asks.
func corpusStableAgreementsIn(verdicts map[string]corpusPairVerdict,
	again map[string]corpusPairVerdict) {

	for key, verdict := range verdicts {
		for _, value := range verdict.agreed {
			if slices.Contains(again[key].agreed, value) {
				verdict.stable = append(verdict.stable, value)
			}
		}
		verdicts[key] = verdict
	}
}

// corpusWithValue adds one value to a set of them, distinctly and in the order first seen.
func corpusWithValue(held []string, value string) []string {
	if slices.Contains(held, value) {
		return held
	}
	return append(held, value)
}

// corpusShortly is one rendered value cut to what a failure message can carry: these are hex
// octet strings and tree hashes, and a refusal nobody reads to the end is a refusal nobody acts on.
func corpusShortly(value string) string {
	if len(value) > 96 {
		return value[:96] + "..."
	}
	return value
}

// corpusRelationVerdictsOf reduces one built fixture to what it says about every pair its door
// compares.
func corpusRelationVerdictsOf(crypto CryptoProvider, root any,
	pairs []comparisonPair) map[string]corpusPairVerdict {

	out := map[string]corpusPairVerdict{}
	at := reflect.ValueOf(root)
	for _, pair := range pairs {
		left := corpusRenderedAlong(crypto, at, pair.left)
		right := corpusRenderedAlong(crypto, at, pair.right)
		verdict := corpusPairVerdict{}
		witness := func(one string, other string) {
			if one == other {
				verdict.agreed = corpusWithValue(verdict.agreed, one)
				return
			}
			verdict.differLeft = corpusWithValue(verdict.differLeft, one)
			verdict.differRight = corpusWithValue(verdict.differRight, other)
		}
		if len(left) != 0 && len(right) != 0 {
			verdict.reached = true
			if pair.positional() {
				for i := 0; i < min(len(left), len(right)); i += 1 {
					witness(left[i], right[i])
				}
			} else {
				for _, one := range left {
					for _, other := range right {
						witness(one, other)
					}
				}
			}
		}
		out[pair.String()] = verdict
	}
	return out
}

// corpusRenderedAlong walks one derived path over one fixture and answers the canonical rendering
// of every value it reaches, IN THE ORDER the path reaches them -- which is what makes the
// positional pairing above mean what the rule means.
//
// IT RENDERS THROUGH corpusRenderOf, the same rendering the dimension claim is stated in, so a pair
// and a dimension never disagree about whether two values are the same value. A pointer is followed
// rather than printed, an octet string is one hex value, and a ratchet tree is its tree hash.
func corpusRenderedAlong(crypto CryptoProvider, root reflect.Value, path pPath) []string {
	out := []string{}
	for _, value := range corpusReach(root, []reflect.Value{root}, path.steps) {
		if !path.length {
			out = append(out, corpusRenderOf(crypto, value, corpusRenderBudget))
			continue
		}
		holder := corpusIndirect(value)
		if !holder.IsValid() {
			continue
		}
		switch holder.Kind() {
		case reflect.Slice, reflect.Array, reflect.Map, reflect.String:
			out = append(out, "len:"+corpusRenderOf(crypto,
				reflect.ValueOf(holder.Len()), corpusRenderBudget))
		}
	}
	return out
}

// corpusReach walks the steps of one path, spreading over every position a slice expansion opens.
//
// A STEP THAT REACHES NOTHING DROPS ITS BRANCH rather than answering a zero value, because "the
// field is absent" and "the field is the zero value" are different facts and a fixture with no
// adds must witness nothing about the adds rather than witness a comparison against an empty key.
func corpusReach(root reflect.Value, here []reflect.Value, steps []pStep) []reflect.Value {
	values := here
	for _, step := range steps {
		next := []reflect.Value{}
		for _, value := range values {
			switch step.kind {
			case "field":
				base := corpusIndirect(value)
				if !base.IsValid() || base.Kind() != reflect.Struct {
					continue
				}
				if field := base.FieldByName(step.name); field.IsValid() {
					next = append(next, field)
				}
			case "elem":
				base := corpusIndirect(value)
				if !base.IsValid() ||
					(base.Kind() != reflect.Slice && base.Kind() != reflect.Array) {
					continue
				}
				for i := 0; i < base.Len(); i += 1 {
					next = append(next, base.Index(i))
				}
			case "call":
				next = append(next, corpusCallAlong(root, value, step)...)
			}
		}
		values = next
	}
	return values
}

// corpusCallAlong makes one call of a path, once per combination of the values its arguments
// spread over, so that Tree.Leaf(List.Updates()[].Sender) answers one leaf per update IN THE
// UPDATES' OWN ORDER.
//
// A CALL THAT ANSWERS AN ERROR ANSWERS NOTHING HERE. FilteredDirectPath refuses a leaf outside its
// tree, and a fixture that names one has no value at that path rather than an empty vector at it.
func corpusCallAlong(root reflect.Value, on reflect.Value, step pStep) []reflect.Value {
	method := corpusMethodOf(on, step.name)
	if !method.IsValid() || method.Type().NumIn() != len(step.args) {
		return nil
	}
	spreads := [][]reflect.Value{{}}
	for _, arg := range step.args {
		resolved := corpusReach(root, []reflect.Value{root}, arg.steps)
		grown := [][]reflect.Value{}
		for _, prefix := range spreads {
			for _, one := range resolved {
				grown = append(grown, append(slices.Clone(prefix), one))
			}
		}
		spreads = grown
	}
	out := []reflect.Value{}
	for _, args := range spreads {
		usable := true
		for i := range args {
			if !args[i].IsValid() || !args[i].Type().AssignableTo(method.Type().In(i)) {
				usable = false
			}
		}
		if !usable {
			continue
		}
		if answered, ok := corpusCallSafely(method, args); ok {
			out = append(out, answered...)
		}
	}
	return out
}

// corpusCallSafely makes one call and answers its first result, or nothing where the call refused
// or panicked.
//
// A RECOVER RATHER THAN A PRECONDITION PER METHOD, because the methods a derived path reaches are
// whatever this package writes into a rule and a list of which of them tolerate which receiver is
// the enumeration this file exists not to be. A fixture that cannot answer a path witnesses nothing
// about it; it must not take the run down.
func corpusCallSafely(method reflect.Value, args []reflect.Value) (answered []reflect.Value,
	ok bool) {

	defer func() {
		if recover() != nil {
			answered, ok = nil, false
		}
	}()
	results := method.Call(args)
	if len(results) == 0 {
		return nil, false
	}
	for _, result := range results[1:] {
		if failure, isError := result.Interface().(error); isError && failure != nil {
			return nil, false
		}
	}
	return results[:1], true
}

// corpusMethodOf finds one method on a value however the value is held: directly, through the
// pointer or interface it sits behind, or on the address of it.
//
// A NIL POINTER ANSWERS NO METHOD. Several of this package's methods are written to tolerate a nil
// receiver and several are not, and a walk that called into the second kind would be a corpus gate
// that panics on a fixture whose optional field is empty.
func corpusMethodOf(on reflect.Value, name string) reflect.Value {
	if !on.IsValid() {
		return reflect.Value{}
	}
	if (on.Kind() == reflect.Pointer || on.Kind() == reflect.Interface) && on.IsNil() {
		return reflect.Value{}
	}
	if method := on.MethodByName(name); method.IsValid() {
		return method
	}
	if on.CanAddr() {
		if method := on.Addr().MethodByName(name); method.IsValid() {
			return method
		}
	}
	if on.Kind() == reflect.Pointer || on.Kind() == reflect.Interface {
		return corpusMethodOf(on.Elem(), name)
	}
	return reflect.Value{}
}

// corpusIndirect follows pointers and interfaces to the value underneath, and answers an invalid
// value where the chain ends in a nil.
func corpusIndirect(value reflect.Value) reflect.Value {
	for value.IsValid() && (value.Kind() == reflect.Pointer || value.Kind() == reflect.Interface) {
		if value.IsNil() {
			return reflect.Value{}
		}
		value = value.Elem()
	}
	return value
}

// ---------------------------------------------------------------------------
// the derivation, held to being a derivation
// ---------------------------------------------------------------------------

// TestTheDroppedBindingClaimSeesADroppedBinding is the control the claim in
// TestEveryDoorComparisonIsReadOffThisPackagesSource is worth nothing without.
//
// A CHECKER THAT REPORTS NOTHING PASSES EVERYTHING, which is the defect one level up from the one
// bindingsLostIn exists to catch: if its statement walk stopped matching, the class could shrink
// silently again and this file would say so in exactly the same words. So it is run over a body
// this test holds -- a rule binding two locals out of a commit input, one with `var` and one with
// `:=` -- and held to BOTH answers: nothing lost against the bindings localsIn really makes, and
// every one of them lost against a walk that made none.
//
// THE CONTROL IS PARSED TEXT rather than a fixture of the package, which is crypto_test.go's own
// idiom one file over: a body known to violate the rule, held beside the real one, so the matcher
// is exercised in both directions on every run.
func TestTheDroppedBindingClaimSeesADroppedBinding(t *testing.T) {
	control := mustParseText(t, "control_relations.go", `package mls

func controlRuleBindingTwoLocals(in *CommitValidationInput) error {
	var key = in.Commit.Path.LeafNode.EncryptionKey
	adds := in.List.Adds()
	for i := range adds {
		if subtle.ConstantTimeCompare(adds[i].Proposal.Add.KeyPackage.InitKey, key) == 1 {
			return errDuplicateEncryptionKey
		}
	}
	return nil
}
`)
	function := control.declarationOf(t, "", "controlRuleBindingTwoLocals")
	roots := rootsOfType(control, function, reflect.TypeFor[CommitValidationInput]().Name())
	if len(roots) == 0 {
		t.Fatalf("the control body names no commit validation input, so neither half below reads anything")
	}
	held := localsIn(function.Body, roots, nil)
	if lost := bindingsLostIn(function.Body, roots, held); len(lost) != 0 {
		t.Errorf("the walk binds every local of the control and the claim reports %v lost, so it fires on bodies it has no complaint about",
			lost)
	}
	// EVERY local the control binds out of its input, named here because the control is written
	// three lines up: this is the answer a walk that bound nothing owes, and a claim that
	// answered less than it would be one that reports only some of what it loses.
	blind := bindingsLostIn(function.Body, roots, map[string][]pPath{})
	slices.Sort(blind)
	if want := []string{"adds", "key"}; !slices.Equal(blind, want) {
		t.Errorf("against a walk that bound nothing the claim reports %v lost and the control binds %v out of its input, so it is not seeing what it says it sees",
			blind, want)
	}
}

// TestEveryDoorComparisonIsReadOffThisPackagesSource is what stops the class above from quietly
// becoming empty.
//
// A DERIVED CLASS THAT DERIVES NOTHING PASSES EVERY CLAIM STATED OVER IT, which is how a gate goes
// green while measuring nothing at all -- so the walk is held to finding pairs at both doors, to
// finding them through the comparator class as well as through the operators, and to reaching a
// pair whose two sides are spread over a vector.
func TestEveryDoorComparisonIsReadOffThisPackagesSource(t *testing.T) {
	pairs, class := doorComparisonPairs(t)
	if len(class) == 0 {
		t.Fatalf("the comparator class derived off this package's imports is empty, so every comparison written as a call is invisible here")
	}
	t.Logf("comparator class (%d): %v", len(class), class)
	declared := validationInputTypesInSource(t)
	if len(declared) < 2 {
		t.Fatalf("this package's source declares %v validation input types; the derivation read something other than the package",
			declared)
	}
	spelledAsACall, spelledAsALength, spreadOverAVector := false, false, false
	readInsideAHelper := false
	for _, named := range declared {
		if len(pairs[named]) == 0 {
			t.Errorf("no rule of this package was read as comparing two paths of a %s, so the relation claim over that door's corpus holds vacuously",
				named)
		}
		for _, pair := range pairs[named] {
			t.Logf("%s: %-72s in %s", named, pair, pair.in)
			// the three shapes the walk has to keep. Each is a separate piece of
			// machinery above and each would leave the walk passing if it broke: the
			// comparator class, the len() arm of pathsOf, and the arity count
			if strings.Contains(pair.String(), "EncryptionKey") {
				spelledAsACall = true
			}
			if pair.left.length || pair.right.length {
				spelledAsALength = true
			}
			if pair.positional() {
				spreadOverAVector = true
			}
			readInsideAHelper = readInsideAHelper || pair.viaCall
		}
	}
	if !spelledAsACall {
		t.Errorf("the walk found no encryption key comparison, so the comparator class is reaching nothing and every subtle.ConstantTimeCompare of these doors is unseen here")
	}
	if !spelledAsALength {
		t.Errorf("the walk found no length comparison, so len(a) != len(b) is being read as something other than a pair")
	}
	if !spreadOverAVector {
		t.Errorf("the walk found no pair whose two sides spread over a vector, so the positional pairing above is dead code")
	}
	if !readInsideAHelper {
		t.Errorf("every pair the walk found was written in a frame that names a validation input, so the call walk is reaching nothing and the scope of this derivation is again 'a function whose signature names an input'. checkProposalProfile refuses a forged discriminant, bindingHolds decides whether this member's cache belongs to the epoch being judged, and neither takes a validation input")
	}
	// AND THE CLASS IS HELD TO THE SOURCE RATHER THAN TO A NUMBER. Every claim over these pairs
	// is stated per pair, so a walk that quietly stops finding three of them states three fewer
	// claims in exactly the same words -- "11 of 11" reads as well as "14 of 14". This is the
	// half that does not move with the class: a comparison one side of which resolved while the
	// other is a local this frame bound out of the same input is a pair the walk DROPPED, and
	// the number of them is zero however many pairs there are. See spellableLocalsIn.
	for _, one := range doorComparisonLosses(t) {
		t.Errorf("%s holds a path into its input that pathsOf can spell and localsIn did not bind, so every comparison that rule writes over it leaves this class silently. localsIn does not read the binding form it was written with",
			one)
	}
}
