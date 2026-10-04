// THE FOURTH ERASE GATE OF THIS PACKAGE, and the reason it exists is a measurement rather than
// an argument: a key material map's ENTRY drops are a shape none of the other five could see.
//
// Ledger item 251's ruling 40 turns GroupSession.pqSecret into a table keyed by epoch, and a
// table drops things two ways the scalar never could -- `delete(m, k)` and `m[k] = v`. Before
// this file existed, the erase in dropPqSecretsBelowWindowOnLoop was deleted as a mutant, leaving
// the delete behind, and SIX gates ran green over it:
//
//	connect/mls  TestEveryPathThatDropsHeldKeyMaterialErasesItFirst     ok
//	connect/mls  TestEveryTypeHoldingErasableKeyMaterialErasesAllOfIt   ok
//	messagegroup TestEveryKeyThisPackageDerivesIsErasedInTheBodyThatDerivedIt      ok
//	messagegroup TestEveryPrimitiveResultThisPackageBindsHasAWrittenDisposition    ok
//	messagegroup TestEveryBufferThisPackageFillsFromEntropyIsErasedOrMovedOut      ok
//	messagegroup TestEveryEraseHelperOfThisPackageCarriesTheNoinlineDirective      ok
//
// WHY, and it is one sentence per gate and the same sentence twice. mls's two readings are seeded
// on an ASSIGNMENT to a field -- `self.f = x` -- and neither `delete(self.f, k)` nor
// `self.f[k] = v` is one, so a table can be emptied entry by entry under a reading that only ever
// asked what happens when the whole field is replaced. The field class DOES reach the map: the
// same mutant applied to the WHOLE-table erase in zeroizeOnLoop turns both mls gates red, by name
// and by field, which is the control for this paragraph and was run. This package's own three are
// about where key material is PRODUCED -- a derivation, a primitive result, an entropy fill --
// and an entry that has been sitting in a table for thirty epochs was produced nowhere near the
// body that drops it. The noinline gate asks a different question again.
//
// **A gate seeded on the whole and a gate seeded on the part are two gates, and this package had
// only the first.** That is the transferable form, and it is the same failure step 2 recorded in
// another dress: a gate whose CLASS is a shape the defect does not have.
//
// WHAT THIS READING IS OVER. Every field of every type this package declares whose type is a MAP
// whose VALUES reach octets -- raw []byte, one of this package's own names for one, or a struct
// this package declares that holds one. That is the class; the two rows excused below are held
// against it in both directions. For each such field, every body that DROPS one of its entries or
// the whole of it must first, or in the same body, have erased what it drops, moved it out to
// its caller, or refused to reach the drop with anything live under it.
//
// THREE LIMITS, STATED RATHER THAN HIDDEN, because a gate that does not say where it stops is a
// gate somebody will believe stops nowhere.
//
//  1. THE READING IS NOT KEY-PRECISE. It asks whether a body takes the obligation seriously, not
//     whether the entry it erased is the entry it dropped: a body that erases the value at key A
//     and overwrites key B is credited. Making it key-precise means resolving two index
//     expressions to the same value, which no reading of this size does honestly. What it does
//     catch is the whole of what the mutants below are: a body that erases NOTHING.
//  2. THE POSITION MATTERS FOR A WHOLE-TABLE DROP AND NOT FOR AN ENTRY DROP, and that asymmetry
//     is derived rather than chosen. `self.f = <new map>` is the LAST reference to what was
//     there, so an erase written after it erases the new table and nothing else -- the control
//     below holds exactly that case and it is REPORTED. `delete(self.f, k)` is not the last
//     reference: the body is holding the value in the local it read it out of, and erasing it on
//     the line below the delete erases the same octets the line above would have.
//  3. IT FOLLOWS A FIELD THROUGH A RECEIVER OR A PARAMETER AND NOT THROUGH A POINTER TO THE
//     FIELD. pastepoch.go's `held := &self.roles` then `*held = map[uint32]epochRole{}` is a
//     whole-table drop this reading does not see. It is on the one field pair the table below
//     excuses, so nothing is uncovered by it today; it is written here because the next such
//     pointer may be taken over a field that is not excused, and a residual named is a residual
//     somebody can find.
package messagegroup

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
)

// mapOfKeyMaterial is one field of one type of this package that holds key material BY KEY.
type mapOfKeyMaterial struct {
	holder string
	field  string
}

func (self mapOfKeyMaterial) String() string { return self.holder + "." + self.field }

// The maps this reading's class reaches whose values are NOT key material, with the reason.
//
// Two rows and they are the same row twice, transcribed from connect/mls's own table for the same
// two fields, because the fact is the same fact: a credential identity is published in its own
// leaf node, which every member holds and every joiner is handed in its Welcome, and a role is
// one row of a group context extension the transcript covers. Erasing either destroys a value
// every member of the group already has.
//
// What these two owe is not an erase but a LIFETIME, and it is held elsewhere: the table is keyed
// by LEAF INDEX and a role change moves nothing else about a leaf, so a table that outlived its
// epoch would answer the new epoch's question with the old epoch's role. installEpochOnLoop drops
// both on the line beside the one re-making pastEpochs. Item 242's ruling 18 and ruling 21.
var mapsOfThisPackageWhoseValuesAreNotKeyMaterial = map[string]string{
	"GroupSession.roles": "this epoch's leaf -> (identity, role) table. Both halves of an entry are " +
		"values every member of the group already holds, so an erase would destroy nothing secret " +
		"and would destroy something public. What it owes is a lifetime, and installEpochOnLoop drops it",
	"pastEpoch.roles": "the same table for one PRIOR epoch, held as a field of that epoch's schedule " +
		"precisely so that the schedule's death is its death; see GroupSession.roles",
}

// mapEraseNamesIn is the names this package erases THROUGH, to a fixed point.
//
// The seed is the free helper, which takes the storage as an argument. What grows the set is the
// property connect/mls settled first and this reading borrows verbatim: AN ERASE TAKES NO
// ARGUMENTS. An erase is a total operation on storage the receiver already holds, so a method
// that has to be TOLD what to erase is erasing part of something rather than all of it -- and,
// here, admitting argument-taking declarations would sweep in installEpochOnLoop, then
// AdvanceEpoch that calls it, then everything that calls THAT, until "an erase" means "a function
// somewhere above a zeroize" and the gate credits every body in the package.
func mapEraseNamesIn(sources []messagegroupSource) []string {
	erasers := map[string]bool{"zeroize": true}
	for grew := true; grew; {
		grew = false
		for _, source := range sources {
			for _, declaration := range source.parsed.Decls {
				function, isFunction := declaration.(*ast.FuncDecl)
				if !isFunction || function.Body == nil || function.Recv == nil {
					continue
				}
				if function.Type.Params != nil && len(function.Type.Params.List) != 0 {
					continue
				}
				if erasers[function.Name.Name] {
					continue
				}
				ast.Inspect(function.Body, func(node ast.Node) bool {
					call, isCall := node.(*ast.CallExpr)
					if !isCall {
						return true
					}
					switch callee := call.Fun.(type) {
					case *ast.Ident:
						if erasers[callee.Name] {
							erasers[function.Name.Name], grew = true, true
						}
					case *ast.SelectorExpr:
						if erasers[callee.Sel.Name] {
							erasers[function.Name.Name], grew = true, true
						}
					}
					return true
				})
			}
		}
	}
	return slices.Sorted(maps.Keys(erasers))
}

// mapValueReachesOctets is whether the values of a map type are key material's shape: raw bytes,
// one of this package's own names for a byte slice, or a type this package declares that holds
// one, to a fixed point over the declarations.
func mapValueReachesOctets(structs map[string]*ast.StructType, named []string, expr ast.Expr,
	seen map[string]bool) bool {

	for _, mentioned := range identifiersNamedInType(expr) {
		if mentioned == "byte" || slices.Contains(named, mentioned) {
			return true
		}
		structure, isStruct := structs[mentioned]
		if !isStruct || seen[mentioned] {
			continue
		}
		seen[mentioned] = true
		for _, field := range structure.Fields.List {
			if mapValueReachesOctets(structs, named, field.Type, seen) {
				return true
			}
		}
	}
	return false
}

// identifiersNamedInType collects every bare identifier a type expression mentions, so *T, []T,
// map[K]T and func(T) all report T. A rendered string compared with strings.Contains would answer
// yes for a type whose name merely contains another's.
func identifiersNamedInType(expr ast.Expr) []string {
	named := []string{}
	ast.Inspect(expr, func(node ast.Node) bool {
		if identifier, isIdentifier := node.(*ast.Ident); isIdentifier {
			named = append(named, identifier.Name)
		}
		return true
	})
	return named
}

// mapStructTypesIn collects every struct type declared in one file, by name.
func mapStructTypesIn(file *ast.File, into map[string]*ast.StructType) {
	for _, declaration := range file.Decls {
		general, isGeneral := declaration.(*ast.GenDecl)
		if !isGeneral || general.Tok != token.TYPE {
			continue
		}
		for _, spec := range general.Specs {
			typed, isTyped := spec.(*ast.TypeSpec)
			if !isTyped {
				continue
			}
			if structure, isStruct := typed.Type.(*ast.StructType); isStruct {
				into[typed.Name.Name] = structure
			}
		}
	}
}

// mapsOfKeyMaterialIn derives the class: every map-typed field whose values reach octets.
func mapsOfKeyMaterialIn(structs map[string]*ast.StructType, named []string) []mapOfKeyMaterial {
	held := []mapOfKeyMaterial{}
	for holder, structure := range structs {
		for _, field := range structure.Fields.List {
			mapped, isMap := field.Type.(*ast.MapType)
			if !isMap {
				continue
			}
			if !mapValueReachesOctets(structs, named, mapped.Value, map[string]bool{}) {
				continue
			}
			for _, name := range field.Names {
				held = append(held, mapOfKeyMaterial{holder: holder, field: name.Name})
			}
		}
	}
	slices.SortFunc(held, func(a mapOfKeyMaterial, b mapOfKeyMaterial) int {
		return strings.Compare(a.String(), b.String())
	})
	return held
}

// mapHolderNamesIn is the names one declaration holds a value of the holder type under: its
// receiver, and every parameter of that type.
//
// The parameter half is not decoration. A reading rooted in the receiver alone asks the
// obligation of methods and of nothing else, and a free function in this package handed a
// *GroupSession drops exactly what a method drops.
func mapHolderNamesIn(function *ast.FuncDecl, holder string) []string {
	names := []string{}
	consider := func(fields []*ast.Field) {
		for _, field := range fields {
			mentioned := identifiersNamedInType(field.Type)
			if !slices.Contains(mentioned, holder) {
				continue
			}
			for _, name := range field.Names {
				if name.Name != "_" {
					names = append(names, name.Name)
				}
			}
		}
	}
	if function.Recv != nil {
		consider(function.Recv.List)
	}
	if function.Type.Params != nil {
		consider(function.Type.Params.List)
	}
	return names
}

// mapFieldExpressionIs answers whether an expression IS the holder's map field: one of the names
// the declaration holds the holder under, selected by the field's name.
func mapFieldExpressionIs(expr ast.Expr, holders []string, field string) bool {
	selector, isSelector := expr.(*ast.SelectorExpr)
	if !isSelector || selector.Sel.Name != field {
		return false
	}
	base, isBare := selector.X.(*ast.Ident)
	return isBare && slices.Contains(holders, base.Name)
}

// mapValueNamesIn is every local name bound to a VALUE of the map: read out at a key, read out at
// a key with the comma ok, or the value of a range over it.
//
// The comma ok shape is the one this package writes nearly everywhere -- `if held, isHeld :=
// self.window[index]; isHeld` -- and a reading without it credits none of the erases that follow.
func mapValueNamesIn(function *ast.FuncDecl, holders []string, field string) []string {
	names := map[string]bool{}
	bind := func(target ast.Expr) {
		if name, isBare := target.(*ast.Ident); isBare && name.Name != "_" {
			names[name.Name] = true
		}
	}
	ast.Inspect(function.Body, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.AssignStmt:
			if len(typed.Rhs) != 1 {
				return true
			}
			index, isIndex := typed.Rhs[0].(*ast.IndexExpr)
			if !isIndex || !mapFieldExpressionIs(index.X, holders, field) {
				return true
			}
			if len(typed.Lhs) != 0 {
				bind(typed.Lhs[0])
			}
		case *ast.RangeStmt:
			if typed.Value == nil || !mapFieldExpressionIs(typed.X, holders, field) {
				return true
			}
			bind(typed.Value)
		}
		return true
	})
	return slices.Sorted(maps.Keys(names))
}

// mapEraseAtIn is the earliest position at which this body hands one of those names to an erase,
// as an argument or as the receiver of an erase method, and whether it does so at all.
func mapEraseAtIn(function *ast.FuncDecl, values []string, erasers []string) (int, bool) {
	earliest, found := 0, false
	mark := func(at token.Pos) {
		if !found || int(at) < earliest {
			earliest, found = int(at), true
		}
	}
	ast.Inspect(function.Body, func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if !isCall {
			return true
		}
		switch callee := call.Fun.(type) {
		case *ast.Ident:
			if !slices.Contains(erasers, callee.Name) {
				return true
			}
			for _, argument := range call.Args {
				if name, isBare := argument.(*ast.Ident); isBare && slices.Contains(values, name.Name) {
					mark(call.Pos())
				}
			}
		case *ast.SelectorExpr:
			if !slices.Contains(erasers, callee.Sel.Name) {
				return true
			}
			if name, isBare := callee.X.(*ast.Ident); isBare && slices.Contains(values, name.Name) {
				mark(call.Pos())
			}
		}
		return true
	})
	return earliest, found
}

// mapMovedOutIn is whether one of those names leaves this body for its caller.
//
// It is POSITION-FREE by construction: a return is the last thing a body does, so a value handed
// to the caller was handed to it whether the delete stood above or below. consumeLocked is the
// site -- a retained rung answered to the caller is dropped from the window deliberately, because
// erasing it would hand back thirty two zeros.
func mapMovedOutIn(function *ast.FuncDecl, values []string) bool {
	moved := false
	ast.Inspect(function.Body, func(node ast.Node) bool {
		returned, isReturn := node.(*ast.ReturnStmt)
		if !isReturn {
			return true
		}
		for _, result := range returned.Results {
			if name, isBare := result.(*ast.Ident); isBare && slices.Contains(values, name.Name) {
				moved = true
			}
		}
		return true
	})
	return moved
}

// mapRefusalAtIn is the earliest position of a refusal that LEAVES WHEN THERE IS SOMETHING TO
// DROP, which is what says the drop below it can never land on a live value.
//
// A REFUSAL IS A DIRECTION AND NOT A COMPARISON, and that distinction is connect/mls's, measured
// there: `if self.f == nil { return }` leaves when there is NOTHING to drop and then drops a live
// value every time it is reached, which reads the same way to a person and is the opposite claim.
// Two shapes, both of them this package's: the comma ok presence read that returns, which is what
// senderRatchetOnLoop and pastEpochOnLoop write, and the whole-table `!= nil` that returns, which
// is retainLocked's lazy allocation.
func mapRefusalAtIn(function *ast.FuncDecl, holders []string, field string) (int, bool) {
	earliest, found := 0, false
	mark := func(at token.Pos) {
		if !found || int(at) < earliest {
			earliest, found = int(at), true
		}
	}
	ast.Inspect(function.Body, func(node ast.Node) bool {
		branch, isBranch := node.(*ast.IfStmt)
		if !isBranch || !mapBranchLeaves(branch.Body) {
			return true
		}
		// the comma ok presence read: `if v, ok := F[k]; ok { ...leaves... }`
		if initial, isAssign := branch.Init.(*ast.AssignStmt); isAssign && len(initial.Rhs) == 1 {
			index, isIndex := initial.Rhs[0].(*ast.IndexExpr)
			if isIndex && mapFieldExpressionIs(index.X, holders, field) && len(initial.Lhs) == 2 {
				if held, isBare := initial.Lhs[1].(*ast.Ident); isBare {
					if condition, isCondition := branch.Cond.(*ast.Ident); isCondition &&
						condition.Name == held.Name {

						mark(branch.Pos())
					}
				}
			}
		}
		// the whole-table presence read: `if F != nil { ...leaves... }`
		comparison, isComparison := branch.Cond.(*ast.BinaryExpr)
		if !isComparison || comparison.Op != token.NEQ {
			return true
		}
		for _, side := range [][2]ast.Expr{{comparison.X, comparison.Y}, {comparison.Y, comparison.X}} {
			if name, isBare := side[1].(*ast.Ident); !isBare || name.Name != "nil" {
				continue
			}
			if mapFieldExpressionIs(side[0], holders, field) {
				mark(branch.Pos())
			}
		}
		return true
	})
	return earliest, found
}

// mapBranchLeaves is whether a branch body ends the path: a return, a break or a continue.
func mapBranchLeaves(block *ast.BlockStmt) bool {
	leaves := false
	ast.Inspect(block, func(node ast.Node) bool {
		switch node.(type) {
		case *ast.ReturnStmt:
			leaves = true
		case *ast.BranchStmt:
			leaves = true
		}
		return true
	})
	return leaves
}

// mapDropSite is one place a body removes something from a key material map.
type mapDropSite struct {
	held  mapOfKeyMaterial
	where string
	// "delete", "overwrite" or "whole", which is what decides whether the position of an erase
	// matters -- see limit 2 in this file's header.
	kind string
	at   int
}

// mapDropSitesIn reads every drop of one field out of one declaration.
func mapDropSitesIn(function *ast.FuncDecl, held mapOfKeyMaterial, holders []string) []mapDropSite {
	sites := []mapDropSite{}
	add := func(kind string, at token.Pos) {
		sites = append(sites, mapDropSite{held: held, where: function.Name.Name, kind: kind, at: int(at)})
	}
	ast.Inspect(function.Body, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.CallExpr:
			callee, isBare := typed.Fun.(*ast.Ident)
			if !isBare || (callee.Name != "delete" && callee.Name != "clear") {
				return true
			}
			if len(typed.Args) == 0 || !mapFieldExpressionIs(typed.Args[0], holders, held.field) {
				return true
			}
			if callee.Name == "delete" {
				add("delete", typed.Pos())
			} else {
				add("whole", typed.Pos())
			}
		case *ast.AssignStmt:
			for _, target := range typed.Lhs {
				if index, isIndex := target.(*ast.IndexExpr); isIndex {
					if mapFieldExpressionIs(index.X, holders, held.field) {
						add("overwrite", typed.Pos())
					}
					continue
				}
				if mapFieldExpressionIs(target, holders, held.field) {
					add("whole", typed.Pos())
				}
			}
		}
		return true
	})
	return sites
}

// mapUnerasedDropsIn is the whole reading over one parsed file: every drop of a key material map
// that neither erased what it dropped, moved it out, nor refused to reach it with a live value
// under it.
func mapUnerasedDropsIn(class []mapOfKeyMaterial, erasers []string, file *ast.File) ([]string, []mapDropSite) {
	reported := []string{}
	sites := []mapDropSite{}
	for _, declaration := range file.Decls {
		function, isFunction := declaration.(*ast.FuncDecl)
		if !isFunction || function.Body == nil {
			continue
		}
		for _, held := range class {
			holders := mapHolderNamesIn(function, held.holder)
			if len(holders) == 0 {
				continue
			}
			found := mapDropSitesIn(function, held, holders)
			if len(found) == 0 {
				continue
			}
			sites = append(sites, found...)
			values := mapValueNamesIn(function, holders, held.field)
			erasedAt, wasErased := mapEraseAtIn(function, values, erasers)
			refusedAt, wasRefused := mapRefusalAtIn(function, holders, held.field)
			moved := mapMovedOutIn(function, values)
			for _, site := range found {
				switch {
				case moved:
				case wasRefused && refusedAt < site.at:
				// the position matters for a whole-table drop and not for an entry drop, for
				// the reason limit 2 gives: a replacement of the field is the last reference,
				// and a delete is not.
				case wasErased && (site.kind != "whole" || erasedAt < site.at):
				default:
					reported = append(reported,
						fmt.Sprintf("%s %s in %s", site.held, site.kind, site.where))
				}
			}
		}
	}
	slices.Sort(reported)
	return reported, sites
}

// A package holding one of every shape this reading has to separate.
//
// It is one string rather than a testdata file so that what the control asserts and what the
// control IS sit on the same screen. Each declaration is named for the disposition it stands for,
// and the two expectations below name them, so a matcher that collapsed two shapes into one is a
// failure with both names in it.
const mapEntryEraseControl = `package control

type ControlHeld struct {
	secret []byte
}

func (self *ControlHeld) Zeroize() {
	zeroize(self.secret)
}

type ControlHolder struct {
	window  map[uint64][]byte
	ladders map[uint64]*ControlHeld
	counts  map[uint64]int
	names   map[uint64]string
}

func (self *ControlHolder) entryDroppedWithNoErase(index uint64) {
	delete(self.window, index)
}

func (self *ControlHolder) entryErasedThenDropped(index uint64) {
	secret, isHeld := self.window[index]
	if !isHeld {
		return
	}
	zeroize(secret)
	delete(self.window, index)
}

func (self *ControlHolder) entryDroppedThenErased(index uint64) {
	secret, isHeld := self.window[index]
	if !isHeld {
		return
	}
	delete(self.window, index)
	zeroize(secret)
}

func (self *ControlHolder) entryMovedOutThenDropped(index uint64) []byte {
	retained := self.window[index]
	delete(self.window, index)
	return retained
}

func (self *ControlHolder) entryOverwrittenWithNoErase(index uint64, secret []byte) {
	self.window[index] = secret
}

func (self *ControlHolder) entryOverwrittenAfterErase(index uint64, secret []byte) {
	if held, isHeld := self.window[index]; isHeld {
		zeroize(held)
	}
	self.window[index] = secret
}

func (self *ControlHolder) entryOverwrittenBehindARefusal(index uint64, secret []byte) {
	if held, isHeld := self.window[index]; isHeld {
		_ = held
		return
	}
	self.window[index] = secret
}

func (self *ControlHolder) entryOverwrittenBehindAPresenceGuard(index uint64, secret []byte) {
	if _, isHeld := self.window[index]; !isHeld {
		return
	}
	self.window[index] = secret
}

func (self *ControlHolder) wholeTableDroppedWithNoErase() {
	self.window = map[uint64][]byte{}
}

func (self *ControlHolder) wholeTableErasedThenDropped() {
	for _, secret := range self.window {
		zeroize(secret)
	}
	self.window = map[uint64][]byte{}
}

func (self *ControlHolder) wholeTableDroppedThenErased() {
	for _, secret := range self.window {
		zeroize(secret)
	}
	self.window = map[uint64][]byte{}
	for _, secret := range self.window {
		zeroize(secret)
	}
}

func (self *ControlHolder) wholeTableDroppedBehindARefusal() {
	if self.window != nil {
		return
	}
	self.window = map[uint64][]byte{}
}

func (self *ControlHolder) wholeTableCleared() {
	clear(self.window)
}

func (self *ControlHolder) ladderDroppedWithNoErase(index uint64) {
	delete(self.ladders, index)
}

func (self *ControlHolder) ladderErasedThenDropped(index uint64) {
	if ladder, isHeld := self.ladders[index]; isHeld {
		ladder.Zeroize()
	}
	delete(self.ladders, index)
}

func (self *ControlHolder) countDroppedWithNoErase(index uint64) {
	delete(self.counts, index)
}

func (self *ControlHolder) nameDroppedWithNoErase(index uint64) {
	delete(self.names, index)
}

func freeFunctionDroppingSomebodyElsesTable(holder *ControlHolder, index uint64) {
	delete(holder.window, index)
}

func zeroize(secret []byte) {
	for i := range secret {
		secret[i] = 0
	}
}
`

func TestEveryMapOfKeyMaterialErasesWhatItDropsBeforeTheTableForgetsIt(t *testing.T) {
	// ------------------------------------------------------------------
	// CONTROL ONE, over the CLASS: which map fields are key material at all
	// ------------------------------------------------------------------
	controlSet := token.NewFileSet()
	control, err := parser.ParseFile(controlSet, "the map entry erase control", mapEntryEraseControl,
		parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse the control: %v", err)
	}
	controlStructs := map[string]*ast.StructType{}
	mapStructTypesIn(control, controlStructs)
	controlClass := mapsOfKeyMaterialIn(controlStructs, nil)
	controlNames := []string{}
	for _, held := range controlClass {
		controlNames = append(controlNames, held.String())
	}
	wantClass := []string{"ControlHolder.ladders", "ControlHolder.window"}
	if !slices.Equal(controlNames, wantClass) {
		t.Fatalf("the class reading derived %v out of the control, want %v; it is not separating a map of raw octets and a map of values that hold them from a map of integers and a map of strings",
			controlNames, wantClass)
	}
	// AND THE COMPLEMENT, ASSERTED rather than left implicit: the two the class does NOT reach
	// are there, they are maps, and they are excluded for a reason a reader can check. A
	// narrowing that stopped removing anything would be a class that swept in every table in the
	// package, and one that removed more than this would be coverage nobody wrote down.
	everyControlMap := []string{}
	for _, field := range controlStructs["ControlHolder"].Fields.List {
		if _, isMap := field.Type.(*ast.MapType); !isMap {
			continue
		}
		for _, name := range field.Names {
			everyControlMap = append(everyControlMap, "ControlHolder."+name.Name)
		}
	}
	slices.Sort(everyControlMap)
	removed := []string{}
	for _, name := range everyControlMap {
		if !slices.Contains(controlNames, name) {
			removed = append(removed, name)
		}
	}
	if want := []string{"ControlHolder.counts", "ControlHolder.names"}; !slices.Equal(removed, want) {
		t.Fatalf("the class narrowing removes %v from the control's four maps, want %v", removed, want)
	}

	// ------------------------------------------------------------------
	// CONTROL TWO, over the OBLIGATION: which dispositions clear a drop
	// ------------------------------------------------------------------
	controlErasers := mapEraseNamesIn([]messagegroupSource{{path: "control", parsed: control}})
	if !slices.Contains(controlErasers, "zeroize") || !slices.Contains(controlErasers, "Zeroize") {
		t.Fatalf("the erase name reading derived %v out of the control and one of the two erases is missing, so every disposition below would read as absent",
			controlErasers)
	}
	controlReported, controlSites := mapUnerasedDropsIn(controlClass, controlErasers, control)
	wantReported := []string{
		"ControlHolder.ladders delete in ladderDroppedWithNoErase",
		"ControlHolder.window delete in entryDroppedWithNoErase",
		"ControlHolder.window delete in freeFunctionDroppingSomebodyElsesTable",
		"ControlHolder.window overwrite in entryOverwrittenBehindAPresenceGuard",
		"ControlHolder.window overwrite in entryOverwrittenWithNoErase",
		"ControlHolder.window whole in wholeTableCleared",
		"ControlHolder.window whole in wholeTableDroppedWithNoErase",
	}
	if !slices.Equal(controlReported, wantReported) {
		t.Fatalf("the obligation reading reports\n  %v\nout of the control, want\n  %v\nit is not separating an erased entry from a dropped one, nor either from one moved out to the caller or standing behind a refusal that leaves when there IS something to drop",
			strings.Join(controlReported, "\n  "), strings.Join(wantReported, "\n  "))
	}
	// the drop shapes the control offers, so a reading that stopped seeing one of the three
	// kinds is a failure rather than a quiet clean run.
	kinds := map[string]int{}
	for _, site := range controlSites {
		kinds[site.kind] += 1
	}
	if kinds["delete"] == 0 || kinds["overwrite"] == 0 || kinds["whole"] == 0 {
		t.Fatalf("the control offers %v drop shapes; a reading that finds none of one kind clears every body written in that kind", kinds)
	}
	// AND THE ONE THAT WOULD BE A FALSE POSITIVE, named: the entry dropped and THEN erased is not
	// reported, because a delete is not the last reference to the value the body is holding --
	// while the whole table dropped and then "erased" IS reported, because that erase ranges over
	// the replacement and reaches nothing. The pair is what limit 2 in this file's header is.
	for _, absent := range []string{
		"ControlHolder.window delete in entryDroppedThenErased",
		"ControlHolder.window whole in wholeTableErasedThenDropped",
	} {
		if slices.Contains(controlReported, absent) {
			t.Errorf("the reading reports %q; limit 2 says it must not", absent)
		}
	}
	if !slices.Contains(controlReported, "ControlHolder.window whole in wholeTableDroppedWithNoErase") {
		t.Error("the reading does not report a whole table replaced with nothing erased, which is the shape connect/mls's own gate already holds and this one must not be weaker than")
	}

	// ------------------------------------------------------------------
	// AND NOW THE REAL SOURCE
	// ------------------------------------------------------------------
	_, sources := messagegroupProductionSources(t)
	structs := map[string]*ast.StructType{}
	for _, source := range sources {
		mapStructTypesIn(source.parsed, structs)
	}
	named := zeroizeByteSliceTypeNames(sources)
	class := mapsOfKeyMaterialIn(structs, named)
	erasers := mapEraseNamesIn(sources)
	if !slices.Contains(erasers, "zeroize") {
		t.Fatalf("this package's erase names read as %v and the free helper is not among them, so every disposition below reads as absent and the run would be clean for the wrong reason",
			erasers)
	}

	// the excuse table, held in BOTH directions against the class.
	held := []mapOfKeyMaterial{}
	excused := []string{}
	for _, one := range class {
		if _, isExcused := mapsOfThisPackageWhoseValuesAreNotKeyMaterial[one.String()]; isExcused {
			excused = append(excused, one.String())
			continue
		}
		held = append(held, one)
	}
	for row := range mapsOfThisPackageWhoseValuesAreNotKeyMaterial {
		if !slices.Contains(excused, row) {
			t.Errorf("mapsOfThisPackageWhoseValuesAreNotKeyMaterial excuses %s, which this reading's class does not reach; a row that outlived its field excuses nothing and hides that it does",
				row)
		}
	}
	if !slices.Equal(slices.Sorted(maps.Keys(mapsOfThisPackageWhoseValuesAreNotKeyMaterial)),
		slices.Sorted(slices.Values(excused))) {

		t.Errorf("the class excuses %v and the table names %v; the two must agree, because a map excused without a row is coverage removed by nobody",
			excused, slices.Sorted(maps.Keys(mapsOfThisPackageWhoseValuesAreNotKeyMaterial)))
	}

	// THE POSITIVE CONTROLS ON THE REAL SOURCE, in the same query as the zero below. A reading
	// that had stopped reaching these fields reports exactly the clean run a complete one reports.
	for _, wanted := range []string{
		"GroupSession.pqSecrets",
		"GroupSession.pastEpochs",
		"GroupSession.senders",
		"ReceiverRatchet.window",
		"ReceiverRatchets.ratchets",
	} {
		found := false
		for _, one := range held {
			if one.String() == wanted {
				found = true
			}
		}
		if !found {
			t.Fatalf("this package's source does not derive %s as a map of key material, so the reading below cleared every body that drops one. The class read: %v",
				wanted, held)
		}
	}
	names := []string{}
	for _, one := range held {
		names = append(names, one.String())
	}
	t.Logf("%d map(s) of key material: %v; excused by a written row: %v", len(held), names, excused)

	reported := []string{}
	sites := []mapDropSite{}
	for _, source := range sources {
		raw, err := os.ReadFile(source.path)
		if err != nil {
			t.Fatalf("read %s: %v", source.path, err)
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), source.path, raw,
			parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", source.path, err)
		}
		fileReported, fileSites := mapUnerasedDropsIn(held, erasers, parsed)
		for _, one := range fileReported {
			reported = append(reported, source.path+": "+one)
		}
		sites = append(sites, fileSites...)
	}

	// AND THE SITES THIS READING MUST BE FINDING, by name and by shape, because a zero below is
	// only worth something if the reading found the drops at all. These three are the whole of
	// the per-entry class the older gates are blind to: the window bound's delete, the table
	// install's overwrite, and the receiver ratchet's own erase loop.
	for _, wanted := range []struct{ where, kind, field string }{
		{where: "dropPqSecretsBelowWindowOnLoop", kind: "delete", field: "pqSecrets"},
		{where: "installPqSecretOnLoop", kind: "overwrite", field: "pqSecrets"},
		{where: "Zeroize", kind: "delete", field: "window"},
	} {
		found := false
		for _, site := range sites {
			if site.where == wanted.where && site.kind == wanted.kind && site.held.field == wanted.field {
				found = true
			}
		}
		if !found {
			t.Fatalf("this reading finds no %s of %s in %s, so it is not reading the shape it exists for",
				wanted.kind, wanted.field, wanted.where)
		}
	}
	shapes := map[string]int{}
	for _, site := range sites {
		shapes[site.kind] += 1
	}
	t.Logf("%d drop site(s) over this package's key material maps: %v", len(sites), shapes)

	if len(reported) != 0 {
		t.Errorf("%d drop(s) of a key material map entry erase nothing, move nothing out and stand behind no refusal:\n  %s\nAn entry a table forgets is a secret with no owner, and it is the shape a gate seeded on a whole-field assignment cannot see",
			len(reported), strings.Join(reported, "\n  "))
	}
}
