package mls

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
)

// GATES.md is the artifact this package wrote to stop a class defect from recurring, and the
// round that wrote it put the defect inside it. Its table claimed to hold "every arity- or
// name-shaped narrowing over a reflected class" and held eight of them; its published query --
// offered as the query that finds the next one -- greps the literal symbol `method.Name`, so
// three narrowings spelled with a different receiver were invisible to both. A table that
// claims a class and is written by hand is a list, and a query keyed to a symbol is derived
// from the instance. This file is the answer to both.
//
// WHAT IS DERIVED HERE, said without naming a symbol: a test in these trees that reads, inside a
// PREDICATE, any FACT A MEMBER'S DESCRIPTOR CARRIES about a member drawn from a REFLECTED MEMBER
// SET -- the member's NAME, the ARITY or SIGNATURE SHAPE of its type, or any of the other facts
// that descriptor holds. That is the property the table is about. It is read off the parse tree,
// so the receiver can be spelled `writer`, `reported`, `of`, `verify` or anything else and the
// site is found the same way -- which is exactly what the grep could not do.
//
// "ANY FACT ITS DESCRIPTOR CARRIES", and that phrase is the EIGHTH instance closed. The reading
// was the member's name and the member's type, spelled `Name` and `Type` in two functions under
// no sentence at all -- and a descriptor carries eight exported fields between reflect's two, so
// six facts about every member stood unread and unprinted. Four narrowings over them were planted
// and all five gates below stayed green. The fields are now FOUND by the same sentence that
// admits the descriptor: see gatesDescriptorFields and gatesIsMemberAttribute.
//
// "MEMBER SET", not "method set", and that word is the SEVENTH instance of this file's own
// defect closed. The reading here matched two selector names, `Method` and `MethodByName`,
// under the sentence "the two doors reflect offers onto a method set" -- a scope taken from the
// six findings that raised this file, every one of which happened to be method-shaped. Reflect
// opens onto a type's FIELDS as well, and a name narrowing over a field set is the identical
// defect: seventy occurrences of it stood one door over, unindexed and unprinted, and a planted
// one shipped green through all four gates below. The doors are no longer named here. They are
// DERIVED from reflect's own source -- see gatesDeriveDoors.
//
// WHAT THIS DERIVATION DOES NOT REACH, printed here AND DRIVEN THROUGH THE CONTROL, because a
// derivation is a narrowing of its own, an unstated boundary is the same defect one level up,
// and a boundary asserted only in a comment is the eight-row table again one altitude higher:
//
//   - it OVER-reports by binding an identifier to a member for the whole function it is bound
//     in, rather than for the block Go scopes it to, so a second identifier of the same name
//     later in the same function is reported too;
//   - it OVER-reports by treating any call reached from a member's signature as a reading of
//     that signature, which is deliberate: see gatesIsMemberType;
//   - it OVER-reports the doors themselves in reflect's Value half, where a door is recognised
//     by the arguments it takes and two element readings take the same ones: see
//     gatesDeriveDoors, and the NOT-A-MEMBER verdict that exists for exactly this;
//   - it UNDER-reports in THREE PLACES THAT WANT A TYPE, each DRIVEN through the control and
//     asserted NOT found. A member reached through a parameter declared reflect.Value (nothing
//     syntactic says that value came off a member set); a member held in a STRUCT FIELD whose
//     type is declared in another declaration (no statement in the function binds it); and a
//     predicate answering a DEFINED type whose underlying type is bool (a spelling comparison
//     cannot tell `controlFlag` from any other named type). go/types and a full type-check of
//     these packages would close each of them.
//   - AND IT UNDER-REPORTS IN NINE PLACES THAT WANT NO TYPE AT ALL, WHICH IS THE NINTH INSTANCE,
//     FILED AND NOT FIXED. Two lists, one per half of how a member is reached, and both are
//     DRIVEN through the control and asserted NOT found beside the three above. The CONTAINER
//     SHAPES: gatesAnswers and gatesDoorSet.declares read a descriptor bare or in a slice of
//     them and in no other container, so on Go 1.26 the two ITERATOR doors of the reflect this
//     file parses -- Type.Methods() answering iter.Seq[Method] and Type.Fields() answering
//     iter.Seq[StructField] -- are not doors here and sit in the printed notDoors list, and a
//     pointer, a map, a named slice type and a variadic fall out with them. The STATEMENT FORMS:
//     gatesGather binds a member through an assignment, a range, a value specification and a
//     field declaration, so a member bound by a TYPE ASSERTION or a TYPE SWITCH is invisible to
//     every gate here. Each of the nine spells the descriptor outright, in a parameter, in an
//     assertion or in a case clause, so go/types is not the remedy for any of them -- which is
//     the third round running that the remedy this file named for what it cannot see would have
//     found none of the next instance. GATES.md carries why the line stops here.
//
// THAT LAST LINE USED TO READ "in exactly three places, and all three want the same thing: a
// TYPE", AND IT WAS THE EIGHTH INSTANCE'S COVER. Four narrowings over the fields a member
// descriptor carries besides its name and its type were invisible to this derivation while that
// sentence stood; NONE of them wanted a type -- a struct tag, a package path, an embedding flag
// and an index are selectors the parse tree already holds -- so the go/types rebuild the sentence
// offered as the remedy would have found none of them. A COUNT OF WHAT A DERIVATION CANNOT SEE IS
// A CLAIM ABOUT THE UNSEEN, and this file has now made a wrong one twice: the round before, a
// predicate BOUND TO A NAME and used as a condition was invisible to the derivation, to both
// published greps and to all four gates here, and it was on neither of the two lines that claimed
// to say what could not be seen. It is now read (gatesBindPredicate) rather than listed.
//
// So the list above is what is ASSERTED NOT FOUND in
// TestTheGatesDerivationSeesANarrowingHoweverItsReceiverIsSpelled, against source written to hold
// it. It is not a proof that nothing else is missing, and it no longer says it is. A boundary
// nothing exercises is a boundary nobody measured; a boundary that counts what it has not seen is
// a boundary nobody CAN measure.

// The document this file holds to the tree. It is read at test time rather than embedded, so
// the gate reads what a reviewer reads.
const gatesDocumentPath = "GATES.md"

// ---------------------------------------------------------------------------
// the doors, derived from reflect's own source rather than named here
// ---------------------------------------------------------------------------

// gatesDoorSet is every spelling that opens onto one member of a reflected type, and what
// admitted it. NOTHING IN THIS FILE NAMES ONE OF THEM.
//
// The seventh instance of this file's defect was the sentence this replaces: "the two doors
// reflect offers onto a method set are Method(i) and MethodByName(n)". Two is the number of
// doors onto a METHOD set. It is not the number of doors onto a type's members, and the
// difference was invisible because every finding that raised this file happened to be about a
// method. So the class is derived from two sentences that name no symbol of this tree and no
// symbol of reflect:
//
//   - a MEMBER DESCRIPTOR is a struct reflect exports that NAMES one member of a type and
//     carries THAT MEMBER'S TYPE -- a `Name string` field beside a `Type Type` field. Some of
//     reflect's exported structs answer that description and the rest do not; both sides are
//     printed on every run, because an exclusion nobody prints is an exclusion nobody reads.
//   - a DOOR is an exported function or interface method of reflect that answers a member
//     descriptor, or a slice of them. Reflect's Value half hands back a Value rather than a
//     descriptor, so a door there is an exported method that answers a Value FOR THE SAME
//     ARGUMENTS a descriptor-answering door takes.
//
// The second half of the second sentence over-reports -- an element reading that takes an int
// looks exactly like a field reading that takes an int -- and over-reporting is the safe
// direction this file has chosen everywhere: a site it reports that is not a member gets a row
// saying so, and the NOT-A-MEMBER verdict is in the vocabulary for it. A door Go adds in a
// later release joins this reading on the day the toolchain moves, and that is the whole
// difference between a derivation and a list.
type gatesDoorSet struct {
	doors          map[string]string
	descriptors    []string
	notDescriptors []string
	notDoors       []string
	exported       int
	// THE READINGS, and this half is the EIGHTH instance of this file's own defect closed. The
	// doors were derived and the readings off what a door hands back were still two literals:
	// the selector `Name` and the selector `Type`. A member descriptor carries more than two
	// fields, every one of them a fact about that member, and a predicate deciding by one of the
	// others is the identical defect one altitude down -- four planted over this complement
	// shipped green through all five gates. So the fields are FOUND by the same sentence that
	// admits the descriptor, and nothing below names one: `named` is the field that names the
	// member, `typed` the field carrying its type, and `attributes` is EVERY OTHER exported field
	// of a member descriptor -- the complement, printed on every run.
	named      string
	typed      string
	attributes []string
	attribute  map[string]bool
	// and the complement of the reading above, because "exported" is a narrowing like any other
	// and this file's own rule is that a narrowing prints what it removed. It is EMPTY on Go
	// 1.26 -- both descriptors are exported through and through -- and an empty complement is
	// the case GATES.md's table calls the dangerous one, so it is printed rather than counted:
	// an unexported field appearing in either descriptor would begin removing something here on
	// the day it lands.
	unexported []string
}

func (self gatesDoorSet) isDoor(name string) bool {
	_, opens := self.doors[name]
	return opens
}

// Whether a selector reads a fact a member descriptor carries that is neither the member's name
// nor its type. On Go 1.26 that is six spellings, and this file names none of them.
func (self gatesDoorSet) isAttribute(spelling string) bool {
	return self.attribute[spelling]
}

// The declared spellings a member is bound with -- a parameter written `reflect.Method`, and
// everything else the descriptor sentence admits. Derived from that same sentence, so a helper
// handed a slice of the OTHER descriptor reaches the same reading as one handed a slice of this
// one; the fifth instance sat on exactly that cross-function edge, one door over.
//
// THE DESCRIPTOR IS DERIVED AND THE CONTAINER IT IS CARRIED IN IS A TWO-ENTRY LIST, `X` and
// `[]X`, which is the ninth instance and the second of its two halves -- a parameter written
// *reflect.Method, map[K]reflect.Method, [4]reflect.StructField, ...reflect.Method or a defined
// slice type over one binds nothing here. It is stated rather than closed: see GATES.md, and the
// nine shapes driven through the control in
// TestTheGatesDerivationSeesANarrowingHoweverItsReceiverIsSpelled.
func (self gatesDoorSet) declares(spelling string) (member bool, slice bool) {
	for _, descriptor := range self.descriptors {
		switch spelling {
		case "reflect." + descriptor:
			return true, false
		case "[]reflect." + descriptor:
			return false, true
		}
	}
	return false, false
}

// derived once per process; reflect is PARSED rather than imported, because what is wanted is
// the shape of its API and a program cannot enumerate a package's exported symbols from inside
// itself
var gatesDoorsOnce = sync.OnceValues(gatesDeriveDoors)

func gatesDoorsOf(t *testing.T) gatesDoorSet {
	t.Helper()
	set, err := gatesDoorsOnce()
	if err != nil {
		t.Fatalf("derive reflect's doors: %v -- this gate reads which spellings open onto a member set off reflect's own source, and it refuses to fall back on a list, because a list is the defect it exists to close", err)
	}
	return set
}

// Whether one struct declaration answers the member-descriptor sentence -- AND WHAT THE SENTENCE
// FOUND WHILE ANSWERING IT, which is the whole of the eighth instance's fix.
//
// The sentence itself is unchanged and deliberately so: a member descriptor is a struct that NAMES
// one member and carries THAT MEMBER'S TYPE. What changes is that the two fields it identifies are
// handed back rather than thrown away, together with every other exported field of the struct. The
// readings below consult what this found instead of spelling the two selectors themselves -- and
// the exported fields this finds that those two do NOT cover are the complement the eighth
// instance was: six further facts a descriptor carries about a member, every one of them something
// a predicate can narrow by, none of them read and none of them printed for seven rounds.
func gatesDescriptorFields(structure *ast.StructType) (named string, typed string, exported []string, unexported []string, is bool) {
	if structure.Fields == nil {
		return "", "", nil, nil, false
	}
	for _, field := range structure.Fields.List {
		for _, name := range field.Names {
			if !ast.IsExported(name.Name) {
				// the complement of the exported-field narrowing, HANDED BACK rather than
				// dropped here, because the disposition of it is a REFUSAL one level up: see
				// gatesDeriveDoors, which fatals rather than continuing if this is ever
				// non-empty. On Go 1.26 neither descriptor carries an unexported field, and an
				// empty complement disposed of with `continue` is the one form GATES.md's own
				// table forbids for that case -- it removes nothing today and begins removing
				// real readings on the commit that lands the first one.
				unexported = append(unexported, name.Name)
				continue
			}
			exported = append(exported, name.Name)
			switch {
			case name.Name == "Name" && gatesTypeSpelling(field.Type) == "string":
				named = name.Name
			case name.Name == "Type" && gatesTypeSpelling(field.Type) == "Type":
				typed = name.Name
			}
		}
	}
	return named, typed, exported, unexported, named != "" && typed != ""
}

// One entry per parameter, so two parameters written `a, b int` compare as two.
func gatesFieldSpellings(of *ast.FieldList) []string {
	spellings := []string{}
	if of == nil {
		return spellings
	}
	for _, field := range of.List {
		repeated := len(field.Names)
		if repeated == 0 {
			repeated = 1
		}
		for at := 0; at < repeated; at++ {
			spellings = append(spellings, gatesTypeSpelling(field.Type))
		}
	}
	return spellings
}

// Whether a result list answers one of a set of types, in any position, bare or in a slice.
//
// AND "BARE OR IN A SLICE" IS A TWO-ENTRY LIST, WHICH IS THE NINTH INSTANCE OF THIS FILE'S OWN
// DEFECT -- filed in GATES.md, driven through the control, and deliberately NOT fixed here. Every
// derivation bottoms out in some literal; the questions that decide whether that is safe are
// whether the literal sits where being wrong is visible and whether it fails closed and prints
// its complement, and this one fails both: on Go 1.26 it removes four real doors of the reflect
// parsed below, it is stated nowhere as a narrowing, and it has no complement of its own.
//
// WHY THE go/types HALF READS ONE CONTAINER MORE, AND WHY IT IS NOT CARRIED BACK HERE.
// gatesAnswersObject strips a pointer as well, because go/types hands a member back as a
// *types.Var or a *types.Func while reflect hands one back by value. Measured on this toolchain:
// package reflect declares NO exported symbol answering *Method or *StructField, so stripping a
// pointer here would change the door set by nothing at all. That is precisely the empty-complement
// row of GATES.md's own table -- a narrowing that removes nothing today -- and lengthening this
// list from two entries to three would leave the iterator, the map, the array, the variadic and
// the named slice type outside it while making the sentence read MORE complete than it is. The
// honest disposition is the whole list recorded as the ninth instance and neither half quietly
// widened.
func gatesAnswers(results *ast.FieldList, wanted map[string]bool) bool {
	for _, spelling := range gatesFieldSpellings(results) {
		if wanted[strings.TrimPrefix(spelling, "[]")] {
			return true
		}
	}
	return false
}

// errGatesUnexportedDescriptorField is the refusal the exported-field narrowing takes instead of
// a `continue`. It exists as a value so the refusal can be OBSERVED by a test rather than only
// believed: the complement it guards is empty on Go 1.26, so nothing in these trees would ever
// drive it, and a fail-closed path nothing drives is a fail-closed path nobody has checked.
var errGatesUnexportedDescriptorField = errors.New("a member descriptor of package reflect carries an unexported field, and this derivation reads only the exported ones")

// The refusal itself, kept separate from the walk so it is reachable with a made-up complement.
//
// GATES.md's own table gives two remedies for a narrowing whose complement is empty -- delete it,
// or make it a fail-closed refusal -- and names the third, a `continue`, as the one that reads as
// harmless and is not. This narrowing removes a descriptor field no predicate in these trees can
// spell, which on Go 1.26 is nothing at all, so the day reflect adds one this says so and stops.
func gatesRefuseUnexportedDescriptorFields(descriptors []string, unexported []string) error {
	if len(unexported) == 0 {
		return nil
	}
	return fmt.Errorf("%w: the %d member descriptor(s) %v carry %d unexported field(s) %v, and a predicate in these trees cannot decide by one; that narrowing is refused here rather than taken silently",
		errGatesUnexportedDescriptorField, len(descriptors), descriptors, len(unexported), unexported)
}

// Package reflect's own source, parsed. Both readings of the door sentence ask the same files:
// the one this derivation uses, and the one that measures what its container clause removes.
func gatesReflectSource() (*token.FileSet, []*ast.File, error) {
	root := build.Default.GOROOT
	if root == "" {
		return nil, nil, fmt.Errorf("the toolchain reports no GOROOT, so reflect's own source cannot be read")
	}
	directory := filepath.Join(root, "src", "reflect")
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", directory, err)
	}
	fileSet := token.NewFileSet()
	files := []*ast.File{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		source, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			return nil, nil, fmt.Errorf("read %s: %w", entry.Name(), err)
		}
		parsed, err := parser.ParseFile(fileSet, entry.Name(), source, parser.SkipObjectResolution)
		if err != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", entry.Name(), err)
		}
		if parsed.Name == nil || parsed.Name.Name != "reflect" {
			continue
		}
		files = append(files, parsed)
	}
	if len(files) == 0 {
		return nil, nil, fmt.Errorf("%s holds no source of package reflect", directory)
	}
	return fileSet, files, nil
}

// Every exported struct package reflect declares, with the fields the descriptor sentence found
// in it. Shared for the same reason the parse is.
func gatesReflectStructs(files []*ast.File) map[string]*ast.StructType {
	declared := map[string]*ast.StructType{}
	for _, file := range files {
		for _, declaration := range file.Decls {
			general, isGeneral := declaration.(*ast.GenDecl)
			if !isGeneral || general.Tok != token.TYPE {
				continue
			}
			for _, specification := range general.Specs {
				typed, isType := specification.(*ast.TypeSpec)
				if !isType || !ast.IsExported(typed.Name.Name) {
					continue
				}
				if structure, isStruct := typed.Type.(*ast.StructType); isStruct {
					declared[typed.Name.Name] = structure
				}
			}
		}
	}
	return declared
}

func gatesDeriveDoors() (gatesDoorSet, error) {
	set := gatesDoorSet{doors: map[string]string{}}
	_, files, err := gatesReflectSource()
	if err != nil {
		return set, err
	}

	// the descriptors, what the sentence removed, and -- the eighth instance -- what the
	// descriptors THEMSELVES carry: the field that names a member, the field that carries its
	// type, and every other exported field of one, which is the class of facts a predicate can
	// narrow by and this file read none of for seven rounds.
	descriptors := map[string]bool{}
	rejected := map[string]bool{}
	naming := map[string]bool{}
	carrying := map[string]bool{}
	carried := map[string]bool{}
	unexported := map[string]bool{}
	for name, structure := range gatesReflectStructs(files) {
		if names, carries, fields, hidden, is := gatesDescriptorFields(structure); is {
			descriptors[name] = true
			naming[names] = true
			carrying[carries] = true
			for _, field := range fields {
				carried[field] = true
			}
			for _, field := range hidden {
				unexported[field] = true
			}
		} else {
			rejected[name] = true
		}
	}
	if len(descriptors) == 0 {
		return set, fmt.Errorf("no exported struct of package reflect names a member and carries its type; %d exported structs were read and none admitted, which is a reading that would report every tree clean", len(rejected))
	}
	// the two readings, taken off the descriptors rather than written here. If the descriptors
	// disagreed about which field names a member, that would be TWO readings wearing one name, and
	// a derivation that silently picked one of them would under-report every site spelled the
	// other way -- which is the shape of every instance this file records.
	if len(naming) != 1 || len(carrying) != 1 {
		return set, fmt.Errorf("the %d member descriptors of package reflect name their member by %d different fields (%v) and carry its type in %d (%v); this derivation reads one of each and cannot say which",
			len(descriptors), len(naming), slices.Sorted(gatesKeysOf(naming)), len(carrying), slices.Sorted(gatesKeysOf(carrying)))
	}
	set.named = slices.Sorted(gatesKeysOf(naming))[0]
	set.typed = slices.Sorted(gatesKeysOf(carrying))[0]
	delete(carried, set.named)
	delete(carried, set.typed)
	set.attributes = slices.Sorted(gatesKeysOf(carried))
	set.attribute = carried
	set.unexported = slices.Sorted(gatesKeysOf(unexported))
	// AND THE EXPORTED-FIELD NARROWING FAILS CLOSED, which is the remedy GATES.md's own table
	// names for an empty complement and the one this file was not taking. The reading above
	// removes a descriptor field that no predicate outside reflect can spell; measured on Go
	// 1.26 that removes NOTHING, and a narrowing that removes nothing today, disposed of with a
	// `continue`, is the row that table calls the dangerous one. So the day reflect adds an
	// unexported field to a member descriptor this derivation says so and stops, rather than
	// quietly reading one fact fewer about every member. The three refusals above it -- no
	// descriptor at all, two disagreeing name fields, no door -- are the same shape.
	if err := gatesRefuseUnexportedDescriptorFields(slices.Sorted(gatesKeysOf(descriptors)), set.unexported); err != nil {
		return set, err
	}

	// the doors: reflect's exported functions, and the method lists of its exported interfaces.
	// `removed` is the complement, kept so it can be NAMED and not counted -- a class disposed of
	// by a number is the COUNT instance this file records first, and the door narrowing is a
	// narrowing like any other. (The shape, not the ordinal: a citation naming a total goes
	// stale the round after it is written, which this file has now watched happen twice.)
	arguments := map[string][]string{}
	removed := map[string]bool{}
	answersAValue := map[string]*ast.FuncType{}
	for _, file := range files {
		for _, declaration := range file.Decls {
			switch node := declaration.(type) {
			case *ast.GenDecl:
				if node.Tok != token.TYPE {
					continue
				}
				for _, specification := range node.Specs {
					typed, isType := specification.(*ast.TypeSpec)
					if !isType || !ast.IsExported(typed.Name.Name) {
						continue
					}
					declared, isInterface := typed.Type.(*ast.InterfaceType)
					if !isInterface || declared.Methods == nil {
						continue
					}
					for _, member := range declared.Methods.List {
						signature, isFunction := member.Type.(*ast.FuncType)
						if !isFunction {
							continue
						}
						for _, name := range member.Names {
							if !ast.IsExported(name.Name) {
								continue
							}
							set.exported++
							if gatesAnswers(signature.Results, descriptors) {
								set.doors[name.Name] = "answers a member descriptor"
								arguments[name.Name] = gatesFieldSpellings(signature.Params)
							} else {
								removed[name.Name] = true
							}
						}
					}
				}
			case *ast.FuncDecl:
				if !ast.IsExported(node.Name.Name) {
					continue
				}
				set.exported++
				if gatesAnswers(node.Type.Results, descriptors) {
					set.doors[node.Name.Name] = "answers a member descriptor"
					arguments[node.Name.Name] = gatesFieldSpellings(node.Type.Params)
					continue
				}
				removed[node.Name.Name] = true
				// reflect's Value half hands a member back as a Value rather than as a
				// descriptor, so these are collected and judged below against the arguments
				// the descriptor-answering doors take
				if node.Recv != nil && gatesAnswers(node.Type.Results, map[string]bool{"Value": true}) {
					answersAValue[node.Name.Name] = node.Type
				}
			}
		}
	}
	for name, signature := range answersAValue {
		if set.isDoor(name) {
			continue
		}
		taken := gatesFieldSpellings(signature.Params)
		for _, door := range slices.Sorted(gatesKeysOf(arguments)) {
			if slices.Equal(taken, arguments[door]) {
				set.doors[name] = "answers a reflect.Value for the arguments " + door + " takes"
				break
			}
		}
	}
	if len(set.doors) == 0 {
		return set, fmt.Errorf("no exported symbol of package reflect answers a member descriptor, over %d read", set.exported)
	}
	for name := range set.doors {
		delete(removed, name)
	}
	set.descriptors = slices.Sorted(gatesKeysOf(descriptors))
	set.notDescriptors = slices.Sorted(gatesKeysOf(rejected))
	set.notDoors = slices.Sorted(gatesKeysOf(removed))
	return set, nil
}

// The one key under which this derivation counts rather than names -- see the reject closure in
// gatesNarrowingsIn. It is spelled here so nothing can collide with it by accident.
const gatesUnnamedRemovals = "(result positions reading no member at all)"

// One narrowing: one predicate, in one function, in one test file.
//
// The KEY is the file, the function and the rendered condition -- never the line number. A
// document keyed to line numbers rots on the first edit above it and a gate that goes red for
// a reason nobody caused is a gate that gets bypassed. Keyed this way it goes red on exactly
// one event: the narrowing itself changed, which is the event that needs a fresh reading of
// the complement.
type gatesNarrowing struct {
	file string
	fn   string
	cond string
	kind string
	line int
}

func (self gatesNarrowing) key() string {
	return self.file + " :: " + self.fn + " :: " + self.cond
}

// The markdown row that would carry this site, printed on failure so the document is repaired
// by pasting rather than by transcribing.
func (self gatesNarrowing) row() string {
	return fmt.Sprintf("| `%s` `%s` | %s | `%s` | VERDICT -- ",
		self.file, self.fn, self.kind, strings.ReplaceAll(self.cond, "|", "\\|"))
}

// What one function knows about which of its identifiers hold a reflected method, that
// method's name, that method's signature, or a slice of members.
type gatesFacts struct {
	members map[string]bool
	names   map[string]bool
	types   map[string]bool
	slices  map[string]bool
	answers map[string]bool
	// and every OTHER fact a member's descriptor carries, bound the same way its name is: see
	// gatesIsMemberAttribute, which is the eighth instance
	attributes map[string]bool
	// a membership decision written in two statements rather than one: see gatesBindPredicate
	predicates map[string]gatesPredicate
	// which spellings open onto a member, derived rather than listed: see gatesDeriveDoors
	doors gatesDoorSet
}

// A predicate bound to a name. `drop := strings.HasPrefix(member.Name, "Gamma")` followed by
// `if drop` narrows a class exactly as hard as the one-statement spelling, and it carried no
// member reading in the condition, so the derivation walked past it for a round -- while the
// comment at the top of this file claimed to say what could not be seen and did not name it.
//
// The TEXT is carried because the row this becomes is keyed on the narrowing, and a row keyed
// to the bare word `drop` would go on certifying a predicate somebody rewrote underneath it.
type gatesPredicate struct {
	name  string
	text  string
	kinds map[string]bool
}

func gatesUnparen(of ast.Expr) ast.Expr {
	for {
		parens, wrapped := of.(*ast.ParenExpr)
		if !wrapped {
			return of
		}
		of = parens.X
	}
}

// Whether an expression is a member of a reflected member set.
//
// THE DOORS ARE NOT NAMED HERE. This function read `Method` and `MethodByName` for six rounds,
// under a sentence that called them "the two doors reflect offers onto a method set" -- true,
// and the wrong class: reflect opens onto a type's FIELDS too, and a name narrowing over a
// field set is this file's subject exactly as much as a name narrowing over a method set. The
// spellings come from gatesDeriveDoors, which reads them off reflect's own API.
//
// The receiver is left entirely open: any spelling reaches here, which is the half the grep in
// the document could not do.
func gatesIsMember(of ast.Expr, known gatesFacts) bool {
	switch node := gatesUnparen(of).(type) {
	case *ast.Ident:
		return known.members[node.Name]
	case *ast.CallExpr:
		if selector, isSelector := gatesUnparen(node.Fun).(*ast.SelectorExpr); isSelector {
			return known.doors.isDoor(selector.Sel.Name)
		}
	case *ast.IndexExpr:
		return gatesIsMemberSlice(node.X, known)
	}
	return false
}

// Whether an expression is a slice of members -- an identifier declared as a slice of a member
// descriptor, or a call to a function in these trees that answers one. This is the edge the fifth instance sat
// on: its class was built in one function and narrowed in another, so a reading that only
// looked inside a NumMethod loop would have walked past it.
func gatesIsMemberSlice(of ast.Expr, known gatesFacts) bool {
	switch node := gatesUnparen(of).(type) {
	case *ast.Ident:
		return known.slices[node.Name]
	case *ast.CallExpr:
		if named, isNamed := gatesUnparen(node.Fun).(*ast.Ident); isNamed {
			return known.answers[named.Name]
		}
	}
	return false
}

// Whether an expression is a member's NAME.
//
// THE SELECTOR IS NOT SPELLED HERE, and that sentence is the eighth instance. It read the literal
// `Name` for seven rounds, beside a `Type` one function down, under no sentence at all -- and two
// literals are not a member descriptor. `reflect.Method` carries five exported fields and
// `reflect.StructField` seven; the field this reads is whichever one the descriptor sentence found
// NAMING the member, and the six it does not read are gatesIsMemberAttribute's subject.
func gatesIsMemberName(of ast.Expr, known gatesFacts) bool {
	switch node := gatesUnparen(of).(type) {
	case *ast.Ident:
		return known.names[node.Name]
	case *ast.SelectorExpr:
		return node.Sel.Name == known.doors.named && gatesIsMember(node.X, known)
	}
	return false
}

// Whether an expression is a fact a member's descriptor carries that is NEITHER its name nor its
// type -- and this whole function is the eighth instance of this file's defect closed.
//
// The doors onto a member set were derived in the seventh round and the READINGS off what a door
// hands back were left as two spellings, `Name` and `Type`. A descriptor carries more:
// `reflect.Method` is a name, a package path, a type, a func and an index, and
// `reflect.StructField` adds a tag, an offset and an embedding flag. Every one of them is a fact
// ABOUT THE MEMBER, so a predicate deciding by one narrows a reflected member set exactly as hard
// as a name test does -- `if field.Tag.Get("json") == "" { continue }` removes members and prints
// nothing, which is this file's subject in one line. Four such narrowings were planted over that
// complement and all five gates below stayed green, and a go/types rebuild -- the remedy the
// boundary paragraph proposed -- would have caught none of them, because a struct tag is a
// SPELLING the parse tree already holds and wants no type at all.
//
// The spellings come from gatesDescriptorFields, so a field Go adds to either descriptor joins
// this reading on the day the toolchain moves.
func gatesIsMemberAttribute(of ast.Expr, known gatesFacts) bool {
	switch node := gatesUnparen(of).(type) {
	case *ast.Ident:
		return known.attributes[node.Name]
	case *ast.SelectorExpr:
		return known.doors.isAttribute(node.Sel.Name) && gatesIsMember(node.X, known)
	}
	return false
}

// Whether an expression is a member's SIGNATURE, or anything read out of it.
//
// NOTHING IS ENUMERATED HERE, AND THAT SENTENCE IS THIS FUNCTION'S HISTORY. It was first
// written with a list of the readings that decide membership -- NumIn, NumOut, In, Out, Kind,
// IsVariadic, NumField -- and that list is the defect this whole file exists to close, one
// level down and inside the closing of it. reflect.Type answers more than seven things, and a
// narrowing spelled `method.Type.Implements(x)`, `method.Type.AssignableTo(x)` or
// `method.Type.String() == "func(*KeySchedule) []byte"` decides membership exactly as hard as
// an arity test and was invisible to every one of those seven names.
//
// So the property is stated instead: ANY call reached from a member's signature is that
// signature being read. It over-reports -- `method.Type.NumIn()` answers an int and a call on
// an int is not a signature reading -- and over-reporting is the safe direction, because the
// site gets a row saying what it is rather than no row at all.
func gatesIsMemberType(of ast.Expr, known gatesFacts) bool {
	switch node := gatesUnparen(of).(type) {
	case *ast.Ident:
		return known.types[node.Name]
	case *ast.SelectorExpr:
		return node.Sel.Name == known.doors.typed && gatesIsMember(node.X, known)
	case *ast.CallExpr:
		if selector, isSelector := gatesUnparen(node.Fun).(*ast.SelectorExpr); isSelector {
			// a member reached through reflect.Value carries its signature behind a CALL
			// rather than a field -- bound.Type().NumIn() where bound came back from
			// MethodByName is the same reading as method.Type.NumIn(), and a derivation that
			// only knew the field form would report the second and miss the first
			if selector.Sel.Name == known.doors.typed && gatesIsMember(selector.X, known) {
				return true
			}
			return gatesIsMemberType(selector.X, known)
		}
	}
	return false
}

// A condition reads a member's SHAPE when it mentions that member's signature at all, in any
// spelling. See gatesIsMemberType for why there is no list of readings here.
func gatesIsShapeReading(of ast.Expr, known gatesFacts) bool {
	return gatesIsMemberType(of, known)
}

func gatesTypeSpelling(of ast.Expr) string {
	if of == nil {
		return ""
	}
	rendered := &bytes.Buffer{}
	if err := printer.Fprint(rendered, token.NewFileSet(), of); err != nil {
		return ""
	}
	return rendered.String()
}

// Everything one function binds to a member, that member's name or that member's signature.
//
// Run to a fixed point rather than once, because `signature := method.Type` can be written
// above the line that binds `method`, and a single pass would then miss every reading through
// `signature` -- which is how proposal_list's narrowing is spelled.
//
// AND THE STATEMENT FORMS BELOW ARE A FOUR-ENTRY LIST, which is the ninth instance's other
// family: an assignment, a range, a value specification and a field declaration bind a member,
// and a TYPE ASSERTION and a TYPE SWITCH -- both of which spell the descriptor outright, exactly
// as a parameter does -- bind nothing. The range case reads only the value, so a member arriving
// as the KEY of a sequence is invisible too. Filed in GATES.md and driven through the control
// rather than closed here; do not widen one of them without the sentence that says what the
// whole class is.
func gatesGather(scope ast.Node, known gatesFacts) {
	ast.Inspect(scope, func(node ast.Node) bool {
		switch statement := node.(type) {
		case *ast.AssignStmt:
			// method, found := X.MethodByName(n) -- the member is the first result and the
			// call sits alone on the right, so the positional reading below cannot see it.
			// The door is whichever ones reflect offers, not the one this comment names.
			//
			// TWO was itself a number taken off the instance. This read `len(Lhs) == 2` for two
			// rounds, because the shapes it was written for -- a door's comma-ok and a map
			// lookup's -- both answer two, and a call answering THREE bound nothing at all. That
			// is how `key, _, _ := strings.Cut(field.Tag.Get("json"), ",")` came to decide three
			// live narrowings this file could not see: the arity of the result list says nothing
			// about whether the call read a member.
			if len(statement.Rhs) == 1 && len(statement.Lhs) >= 2 {
				opened := false
				if call, isCall := gatesUnparen(statement.Rhs[0]).(*ast.CallExpr); isCall {
					if selector, isSelector := gatesUnparen(call.Fun).(*ast.SelectorExpr); isSelector &&
						known.doors.isDoor(selector.Sel.Name) {
						if bound, isIdent := statement.Lhs[0].(*ast.Ident); isIdent {
							known.members[bound.Name] = true
							opened = true
						}
					}
				}
				// and the comma-ok form of a membership decision: `_, wanted :=
				// set[member.Name]` binds the predicate to the SECOND result, which no
				// positional reading below can reach either
				if !opened {
					for _, side := range statement.Lhs {
						if bound, isIdent := side.(*ast.Ident); isIdent {
							gatesBindPredicate(bound.Name, statement.Rhs[0], known)
						}
					}
				}
			}
			if len(statement.Lhs) != len(statement.Rhs) {
				return true
			}
			for at := range statement.Lhs {
				bound, isIdent := statement.Lhs[at].(*ast.Ident)
				if !isIdent {
					continue
				}
				from := statement.Rhs[at]
				classified := false
				if gatesIsMember(from, known) {
					known.members[bound.Name] = true
					classified = true
				}
				if gatesIsMemberName(from, known) {
					known.names[bound.Name] = true
					classified = true
				}
				if gatesIsMemberType(from, known) {
					known.types[bound.Name] = true
					classified = true
				}
				if gatesIsMemberAttribute(from, known) {
					known.attributes[bound.Name] = true
					classified = true
				}
				if gatesIsMemberSlice(from, known) {
					known.slices[bound.Name] = true
					classified = true
				}
				// what is left is an identifier that is not itself a member, a name, a
				// signature or a slice of members, and whose value READS one. That is a
				// membership decision written above the line that uses it.
				if !classified {
					gatesBindPredicate(bound.Name, from, known)
				}
			}
		case *ast.RangeStmt:
			if statement.Value != nil && gatesIsMemberSlice(statement.X, known) {
				if bound, isIdent := statement.Value.(*ast.Ident); isIdent {
					known.members[bound.Name] = true
				}
			}
		case *ast.ValueSpec:
			gatesBindDeclared(gatesTypeSpelling(statement.Type), statement.Names, known)
		case *ast.Field:
			gatesBindDeclared(gatesTypeSpelling(statement.Type), statement.Names, known)
		}
		return true
	})
}

// A parameter, a field or a var written with the type outright. This is the other half of the
// cross-function edge: a helper taking a slice of members narrows a class it never built. The
// spellings are the descriptors gatesDeriveDoors found, so the field half of reflect reaches
// this the same way the method half does.
func gatesBindDeclared(spelling string, names []*ast.Ident, known gatesFacts) {
	member, slice := known.doors.declares(spelling)
	for _, bound := range names {
		if member {
			known.members[bound.Name] = true
		}
		if slice {
			known.slices[bound.Name] = true
		}
	}
}

// Bind one identifier to the reading its value performs, when the identifier is not itself a
// member, a member's name, a member's signature or a slice of members.
//
// This is the third under-reach of the derivation, closed rather than added to the list of
// under-reaches: a predicate spelled in one statement and consumed in the next was invisible to
// the derivation, to both greps GATES.md publishes, and to all four gates in this file. It is
// bound rather than recorded here, because a bound value is only a NARROWING at the place it
// decides something -- see gatesBoundPredicates, which reads it in condition position only. An
// accumulator built out of member names is bound here too and is never recorded, because
// `len(kept) == 0` does not use `kept` as a predicate.
func gatesBindPredicate(name string, from ast.Expr, known gatesFacts) {
	if name == "_" {
		return
	}
	if _, member := known.members[name]; member {
		return
	}
	kinds := map[string]bool{}
	gatesReadings(from, known, kinds)
	for _, carried := range gatesBoundPredicates(from, known) {
		for kind := range carried.kinds {
			kinds[kind] = true
		}
	}
	if len(kinds) == 0 {
		return
	}
	known.predicates[name] = gatesPredicate{name: name, text: gatesTypeSpelling(from), kinds: kinds}
}

// The bound predicates a condition decides by, reached the way the language reads a boolean:
// through parentheses, negation, and the two logical operators. Nothing else -- `len(kept) == 0`
// consumes a bound identifier as a VALUE and decides nothing about a member by it, and reading
// it here would report every accumulator in these trees as a narrowing.
func gatesBoundPredicates(of ast.Expr, known gatesFacts) []gatesPredicate {
	reached := map[string]gatesPredicate{}
	var walk func(ast.Expr)
	walk = func(node ast.Expr) {
		switch expression := gatesUnparen(node).(type) {
		case *ast.Ident:
			if predicate, bound := known.predicates[expression.Name]; bound {
				reached[expression.Name] = predicate
			}
		case *ast.UnaryExpr:
			if expression.Op == token.NOT {
				walk(expression.X)
			}
		case *ast.BinaryExpr:
			// the two logical operators, and the six comparisons. A comparison was left out for
			// two rounds under the sentence above, and it is the operator a value CUT OUT of a
			// member's descriptor is decided by: `key, _, _ := strings.Cut(field.Tag.Get("json"),
			// ",")` then `if key == ""` narrows a field set exactly as hard as `if field.Tag ==
			// ""`, and three live sites are spelled that way. The accumulator this walk must
			// still not reach is reached through a CALL -- `len(kept) == 0` -- and walk descends
			// into no call, so it stays out for a reason rather than by the operator's accident.
			switch expression.Op {
			case token.LAND, token.LOR, token.EQL, token.NEQ, token.LSS, token.LEQ, token.GTR, token.GEQ:
				walk(expression.X)
				walk(expression.Y)
			}
		}
	}
	walk(of)
	found := []gatesPredicate{}
	for _, name := range slices.Sorted(gatesKeysOf(reached)) {
		found = append(found, reached[name])
	}
	return found
}

// Whether a condition reads a member's name or its shape, walking the expression the way the
// language reads it.
//
// The Sel of a selector is a FIELD NAME and never a value, so it is not visited as an
// identifier. A generic tree walk instead reports every struct field spelled `name` as a
// member read, which measured 63 sites where there are 47 -- and a class that over-reports by
// a third stops being read, which is how a gate comes to be bypassed.
func gatesReadings(of ast.Expr, known gatesFacts, into map[string]bool) {
	if of == nil {
		return
	}
	if gatesIsMemberName(of, known) {
		into["name"] = true
	}
	if gatesIsShapeReading(of, known) {
		into["shape"] = true
	}
	if gatesIsMemberAttribute(of, known) {
		into["attribute"] = true
	}
	switch node := of.(type) {
	case *ast.ParenExpr:
		gatesReadings(node.X, known, into)
	case *ast.UnaryExpr:
		gatesReadings(node.X, known, into)
	case *ast.StarExpr:
		gatesReadings(node.X, known, into)
	case *ast.BinaryExpr:
		gatesReadings(node.X, known, into)
		gatesReadings(node.Y, known, into)
	case *ast.SelectorExpr:
		gatesReadings(node.X, known, into)
	case *ast.IndexExpr:
		gatesReadings(node.X, known, into)
		gatesReadings(node.Index, known, into)
	case *ast.SliceExpr:
		gatesReadings(node.X, known, into)
		gatesReadings(node.Low, known, into)
		gatesReadings(node.High, known, into)
		gatesReadings(node.Max, known, into)
	case *ast.CallExpr:
		gatesReadings(node.Fun, known, into)
		for _, argument := range node.Args {
			gatesReadings(argument, known, into)
		}
	case *ast.TypeAssertExpr:
		gatesReadings(node.X, known, into)
	case *ast.KeyValueExpr:
		gatesReadings(node.Value, known, into)
	case *ast.CompositeLit:
		for _, element := range node.Elts {
			gatesReadings(element, known, into)
		}
	}
}

// Every narrowing in one parsed file. `answers` is the package-wide set of functions whose
// result is a slice of members, so a class built in one file and narrowed in another is
// reached; `doors` is the derived set of spellings that open onto a member.
// `undecided` is caller-owned and accumulated across files: it is the complement of the
// predicate-result narrowing gatesRecordDecided performs, so that the one narrowing this
// derivation makes over its own reading of a DECISION is named at run time like every other.
func gatesNarrowingsIn(fileSet *token.FileSet, parsed *ast.File, path string, answers map[string]bool, doors gatesDoorSet, undecided map[string]bool) []gatesNarrowing {
	found := []gatesNarrowing{}
	for _, declaration := range parsed.Decls {
		function, isFunction := declaration.(*ast.FuncDecl)
		if !isFunction || function.Body == nil {
			continue
		}
		known := gatesFacts{
			members:    map[string]bool{},
			names:      map[string]bool{},
			types:      map[string]bool{},
			slices:     map[string]bool{},
			attributes: map[string]bool{},
			answers:    answers,
			predicates: map[string]gatesPredicate{},
			doors:      doors,
		}
		// four passes reaches a fixed point on every shape in these trees; the fifth would
		// change nothing, and the loop is bounded rather than "until stable" so a pathological
		// file cannot hang the suite
		for pass := 0; pass < 4; pass++ {
			gatesGather(function, known)
		}
		// the reading one expression performs, shared by the recorder and by the complement
		// below so the two cannot drift: a site is removed from the decision by exactly the
		// test that would have admitted it.
		reads := func(of ast.Expr) (map[string]bool, []gatesPredicate) {
			readings := map[string]bool{}
			gatesReadings(of, known, readings)
			// and the same decision written in two statements instead of one
			carried := gatesBoundPredicates(of, known)
			for _, predicate := range carried {
				for kind := range predicate.kinds {
					readings[kind] = true
				}
			}
			return readings, carried
		}
		// what the predicate-result narrowing removed, named. A function whose result DECIDES
		// something about a member and is not spelled `bool` is not read as a decision here,
		// and until this round nothing said so at run time -- the round that widened the arity
		// closed the narrowing and left its print, which is the other half of the same rule.
		// AND WHAT THIS NARROWING ITSELF REMOVES IS COUNTED RATHER THAN NAMED, said out loud
		// because GATES.md's rule is to name and this does not. A result position that reads no
		// member at all is every function in these trees, and printing that is printing the tree
		// rather than a complement -- so the count is carried under one key and the reason is
		// here. It is the one exclusion in this derivation whose members are not named, and it is
		// the property itself: this file indexes narrowings over a reflected MEMBER set.
		reject := func(spelling string, answer ast.Expr) {
			if answer == nil {
				return
			}
			if readings, _ := reads(answer); len(readings) == 0 {
				undecided[gatesUnnamedRemovals] = true
				return
			}
			undecided[path+" :: "+function.Name.Name+" :: answers "+spelling] = true
		}
		record := func(condition ast.Expr) {
			if condition == nil {
				return
			}
			readings, carried := reads(condition)
			if len(readings) == 0 {
				return
			}
			rendered := &bytes.Buffer{}
			if err := printer.Fprint(rendered, fileSet, condition); err != nil {
				return
			}
			text := strings.Join(strings.Fields(rendered.String()), " ")
			for _, predicate := range carried {
				text += " where " + predicate.name + " = " + strings.Join(strings.Fields(predicate.text), " ")
			}
			found = append(found, gatesNarrowing{
				file: path,
				fn:   function.Name.Name,
				cond: text,
				kind: strings.Join(slices.Sorted(gatesKeysOf(readings)), "+"),
				line: fileSet.Position(condition.Pos()).Line,
			})
		}
		ast.Inspect(function, func(node ast.Node) bool {
			switch statement := node.(type) {
			case *ast.IfStmt:
				record(statement.Cond)
			case *ast.ForStmt:
				record(statement.Cond)
			case *ast.SwitchStmt:
				record(statement.Tag)
			case *ast.CaseClause:
				for _, expression := range statement.List {
					record(expression)
				}
			case *ast.FuncDecl:
				gatesRecordDecided(statement.Type, statement.Body, record, reject)
			case *ast.FuncLit:
				gatesRecordDecided(statement.Type, statement.Body, record, reject)
			}
			return true
		})
	}
	return found
}

// A function or a literal that answers exactly one bool IS a predicate, and the place it
// decides is its RESULT rather than a condition. `slices.ContainsFunc(members, func(one
// reflect.Method) bool { return one.Name == "x" })` narrows a class as hard as any `if` and
// carries no condition at all -- it was the first of the two under-reaches this file used to
// state in a comment and defend with nothing.
//
// The bool is what keeps this from reporting every projection in these trees: a helper whose
// result is a reflect.Kind, a []byte or a member is answering a question ABOUT a member, not
// deciding whether the member is in a class.
func gatesRecordDecided(signature *ast.FuncType, body *ast.BlockStmt, record func(ast.Expr), removed func(string, ast.Expr)) {
	if body == nil || signature.Results == nil {
		return
	}
	// WHICH RESULT POSITIONS ARE THE DECISION, rather than "the function answers exactly one
	// bool". That was an ARITY narrowing over the predicate class -- the very shape GATES.md's
	// own Q1 names as always suspect, sitting inside the file that publishes the warning -- and
	// its complement was unprinted and, measured with a probe, EMPTY: a helper answering
	// (bool, error) or two bools decides membership exactly as hard as one answering a single
	// bool, and no such site exists in these trees today. Empty and unprinted is the case the
	// table in GATES.md calls the dangerous one, because it begins removing real sites on the
	// commit that adds the first helper of that shape.
	// AND THE COMPLEMENT OF THAT NARROWING IS REPORTED RATHER THAN DROPPED, which is the half
	// the round that widened this from "exactly one bool" left unclosed: the arity was fixed and
	// the print was not, so a result position removed from the decision was removed in silence.
	// `bool` is a SPELLING, and the one shape that reaches past it is a DEFINED type whose
	// underlying type is bool -- this file's own third stated under-reach. So every non-bool
	// result position is handed to `removed` together with the expression returned in it, and
	// the caller, which is the half holding what a member is, names the ones that actually READ
	// one. That is the difference between printing a complement and printing the tree.
	spellings := []string{}
	deciding := []bool{}
	for _, result := range signature.Results.List {
		repeated := len(result.Names)
		if repeated == 0 {
			repeated = 1
		}
		spelling := gatesTypeSpelling(result.Type)
		for at := 0; at < repeated; at++ {
			spellings = append(spellings, spelling)
			deciding = append(deciding, spelling == "bool")
		}
	}
	ast.Inspect(body, func(node ast.Node) bool {
		switch statement := node.(type) {
		case *ast.FuncLit:
			// a literal inside this one answers for itself, and the walk above reaches it
			return false
		case *ast.ReturnStmt:
			// a bare return names no expression, and `return f()` hands back a call whose
			// positions this cannot line up against the signature's -- in neither case is
			// there an expression here that IS the decision
			if len(statement.Results) != len(deciding) {
				return true
			}
			for at, answer := range statement.Results {
				if deciding[at] {
					record(answer)
					continue
				}
				removed(spellings[at], answer)
			}
		}
		return true
	})
}

func gatesKeysOf[Value any](of map[string]Value) func(func(string) bool) {
	return func(yield func(string) bool) {
		for key := range of {
			if !yield(key) {
				return
			}
		}
	}
}

// Every test file the document speaks for, keyed the way the document names them: the path
// relative to the module root.
//
// THE SCOPE IS NOT A LIST. It is forbiddenScanRoots, the set crypto_forbidden_test.go derives
// from the module's own import graph and asserts -- the DIRECTORY instance, closed.
// Aliasing it rather than restating it is the whole point: a fourth package joins this gate's
// scope on the commit that joins that one's.
func gatesTestSources(t *testing.T) (paths []string, fileSet *token.FileSet, parsed map[string]*ast.File) {
	t.Helper()
	moduleRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolve the module root: %v", err)
	}
	fileSet = token.NewFileSet()
	parsed = map[string]*ast.File{}
	perRoot := map[string]int{}
	// the scope's own complement, kept so it can be NAMED rather than counted. A class disposed
	// of by a number is the COUNT instance this file records first, and the sentence
	// "all of them production source" is a claim about members nobody could check off a count.
	skipped := []string{}
	for _, root := range forbiddenScanRoots {
		walked := 0
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			// the scope narrows by a NAME, so it prints what the name removed. The predicate
			// is the language's own rule for what a test file is and not a symbol of this
			// tree, but an unprinted complement is unreadable whichever it is.
			if !strings.HasSuffix(entry.Name(), "_test.go") {
				if strings.HasSuffix(entry.Name(), ".go") {
					skipped = append(skipped, filepath.ToSlash(path))
				}
				return nil
			}
			absolute, err := filepath.Abs(path)
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(moduleRoot, absolute)
			if err != nil {
				return err
			}
			key := filepath.ToSlash(relative)
			source, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			file, err := parser.ParseFile(fileSet, key, source, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			paths = append(paths, key)
			parsed[key] = file
			walked++
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
		// a root that yields nothing is a scope that read nothing, and a derivation that read
		// nothing reports the same clean bill a complete one reports
		if walked == 0 {
			t.Fatalf("%s holds no _test.go file, so this gate read none of it and would report clean over every narrowing in it", root)
		}
		perRoot[root] = walked
	}
	slices.Sort(paths)
	slices.Sort(skipped)
	t.Logf("scope: %d test files over the roots %v (%v), derived from the module's import graph rather than listed here",
		len(paths), forbiddenScanRoots, perRoot)
	t.Logf("scope removed: %d .go files under those roots are removed by the name test, all of them production source, which is where a gate cannot live: %v",
		len(skipped), skipped)
	return paths, fileSet, parsed
}

// The class, derived.
func gatesReflectedNarrowings(t *testing.T) []gatesNarrowing {
	t.Helper()
	doors := gatesDoorsOf(t)
	// the doors and their complement, printed on every run. This is the narrowing that was
	// unstated for six rounds and wrong for six rounds, so it is the one this file prints
	// first: which spellings open onto a member, why each was admitted, and which of reflect's
	// exported structs the descriptor sentence removed.
	t.Logf("doors: %d of package reflect's %d exported symbols answer a member descriptor or a value for one -- %v; descriptors %v, admitted out of reflect's exported structs by carrying a Name and a Type, the rest removed being %v",
		len(doors.doors), doors.exported, doors.doors, doors.descriptors, doors.notDescriptors)
	// and the door narrowing's own complement, NAMED. This is the line that lets a reader ask of
	// a spelling whether it should have been a door -- which is the question nobody could ask for
	// six rounds, because the answer was two names in a comment.
	t.Logf("doors removed: %d distinct exported spellings of package reflect answer no member descriptor and no value for one: %v",
		len(doors.notDoors), doors.notDoors)
	// AND THE READINGS OFF WHAT A DOOR HANDS BACK, with their complement, which is the eighth
	// instance. The doors were derived a round ago and this half was still two literals; a
	// descriptor carries more facts about a member than its name and its type, and a predicate
	// deciding by one of the others narrows the same class. This line is what lets a reader ask of
	// a descriptor field whether a predicate reading it should have been indexed.
	t.Logf("readings: a member descriptor carries %d exported field(s) in these %d structs; the one that NAMES the member (%s) is read as a name narrowing, the one that carries its TYPE (%s) as a shape narrowing, and the %d remaining -- %v -- as narrowings over a fact the descriptor carries. Reading only the first two, and printing no complement for the rest, was the eighth instance",
		len(doors.attributes)+2, len(doors.descriptors), doors.named, doors.typed, len(doors.attributes), doors.attributes)
	// and the complement of THAT reading. Empty today, which is the reading GATES.md's own table
	// calls the dangerous one, so it is named rather than assumed away.
	t.Logf("readings removed: %d field(s) of a member descriptor are removed from the reading above because they are unexported and so no predicate in these trees can decide by one: %v",
		len(doors.unexported), doors.unexported)
	paths, fileSet, parsed := gatesTestSources(t)
	answers := map[string]bool{}
	for _, path := range paths {
		for _, declaration := range parsed[path].Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Type.Results == nil {
				continue
			}
			for _, result := range function.Type.Results.List {
				if _, slice := doors.declares(gatesTypeSpelling(result.Type)); slice {
					answers[function.Name.Name] = true
				}
			}
		}
	}
	undecided := map[string]bool{}
	found := []gatesNarrowing{}
	for _, path := range paths {
		found = append(found, gatesNarrowingsIn(fileSet, parsed[path], path, answers, doors, undecided)...)
	}
	// AND THE COMPLEMENT OF THE DERIVATION'S READING OF A DECISION, which had no reporter at
	// all until this round. `bool` is a spelling: a helper answering a DEFINED type whose
	// underlying type is bool decides membership exactly as hard, and this derivation does not
	// read it as the decision. These are the sites where that removal is not hypothetical --
	// the result reads a member and is not spelled `bool` -- and naming them is what lets a
	// reader ask of one whether it should have been read as a predicate.
	named := slices.Sorted(gatesKeysOf(undecided))
	unnamed := slices.Contains(named, gatesUnnamedRemovals)
	named = slices.DeleteFunc(named, func(one string) bool { return one == gatesUnnamedRemovals })
	t.Logf("decisions removed: %d result position(s) in these trees READ a member and are not spelled `bool`, so this derivation does not read them as the decision: %v",
		len(named), named)
	t.Logf("and the one exclusion of this derivation whose members are NOT named: result positions reading no member at all, present=%v over the %d key(s) undecided carries. They are every function in these trees, so naming them prints the tree rather than a complement -- and reading a member is the property this file indexes rather than a narrowing of it",
		unnamed, len(undecided))
	if len(found) == 0 {
		t.Fatal("no narrowing over a reflected member set was derived from these trees; the document below claims a class and this read none of it, which is the vacuous reading every gate here is written to refuse")
	}
	return found
}

// ---------------------------------------------------------------------------
// the document
// ---------------------------------------------------------------------------

// The verdicts a row may carry. A row that carries none of these is unjudged, and an unjudged
// narrowing is the thing this file exists to make impossible.
//
// OPEN is in the vocabulary and is RED. Recording a narrowing as open used to be how it stayed
// open: the table at 81b97ca carried two open rows for a round and both were still open when
// the next reviewer arrived. A verdict that costs nothing is not a verdict.
var gatesVerdicts = map[string]bool{
	"NARROWING/complement": true,
	"NARROWING/refusal":    true,
	"CLASS/results":        true,
	"DRIVER":               true,
	"NOT-A-MEMBER":         true,
	"OPEN":                 true,
}

type gatesIndexRow struct {
	file    string
	fn      string
	kind    string
	cond    string
	verdict string
	reading string
	line    int
}

func (self gatesIndexRow) key() string {
	return self.file + " :: " + self.fn + " :: " + self.cond
}

// Split one markdown table row into its cells, honouring the backslash a pipe inside a cell
// must be written with.
func gatesCellsOf(row string) []string {
	cells := []string{}
	current := &strings.Builder{}
	escaped := false
	for _, character := range row {
		switch {
		case escaped:
			current.WriteRune(character)
			escaped = false
		case character == '\\':
			escaped = true
		case character == '|':
			cells = append(cells, strings.TrimSpace(current.String()))
			current.Reset()
		default:
			current.WriteRune(character)
		}
	}
	cells = append(cells, strings.TrimSpace(current.String()))
	return cells
}

var gatesQuoted = regexp.MustCompile("`([^`]*)`")

// Every row of the document's index, read off the document.
func gatesIndexRows(t *testing.T) ([]gatesIndexRow, []string) {
	t.Helper()
	source, err := os.ReadFile(gatesDocumentPath)
	if err != nil {
		t.Fatalf("read %s: %v -- this gate holds that document to the tree and cannot do it unread", gatesDocumentPath, err)
	}
	lines := strings.Split(strings.ReplaceAll(string(source), "\r\n", "\n"), "\n")
	rows := []gatesIndexRow{}
	inIndex := false
	for at, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "<!-- gates-index:begin -->") {
			inIndex = true
			continue
		}
		if strings.HasPrefix(trimmed, "<!-- gates-index:end -->") {
			inIndex = false
			continue
		}
		if !inIndex || !strings.HasPrefix(trimmed, "|") {
			continue
		}
		cells := gatesCellsOf(trimmed)
		// a markdown row opens and closes with a pipe, so the first and last cells are empty.
		// A row that does not split into six is an ERROR and not a skip: a row that vanishes
		// from this reading is a narrowing this document appears to judge and does not.
		if len(cells) < 6 {
			t.Errorf("%s:%d is inside the index and splits into %d cells rather than six, so it was read as no row at all: %q",
				gatesDocumentPath, at+1, len(cells), trimmed)
			continue
		}
		site, kind, condition, reading := cells[1], cells[2], cells[3], cells[4]
		if strings.HasPrefix(site, "---") || site == "site" {
			continue
		}
		names := gatesQuoted.FindAllStringSubmatch(site, -1)
		if len(names) != 2 {
			t.Errorf("%s:%d names %d backticked things in its site cell and a site is a file and a function: %q",
				gatesDocumentPath, at+1, len(names), site)
			continue
		}
		quotedCondition := gatesQuoted.FindStringSubmatch(condition)
		if quotedCondition == nil {
			t.Errorf("%s:%d carries no backticked condition: %q", gatesDocumentPath, at+1, condition)
			continue
		}
		verdict := strings.Fields(reading)
		if len(verdict) == 0 || !gatesVerdicts[verdict[0]] {
			t.Errorf("%s:%d opens its reading with %q and a reading opens with one of %v",
				gatesDocumentPath, at+1, reading, slices.Sorted(gatesKeysOf(gatesVerdicts)))
			continue
		}
		rows = append(rows, gatesIndexRow{
			file:    names[0][1],
			fn:      names[1][1],
			kind:    kind,
			cond:    strings.Join(strings.Fields(quotedCondition[1]), " "),
			verdict: verdict[0],
			reading: reading,
			line:    at + 1,
		})
	}
	return rows, lines
}

// TestTheGatesTableIsTheDerivedClassAndNotAListOfIt is the sixth instance closed.
//
// GATES.md's table opened with "Every arity- or name-shaped narrowing over a reflected class"
// and was written by hand. A universal claim written by hand is a list wearing a quantifier,
// and this one held eight of the fifty-four that existed when it was measured -- the number
// GATES.md itself records, and not the twenty-five this comment used to say. So the claim is now
// DECIDED
// here: the document's index and the class derived off the parse tree must be the same set,
// in both directions.
//
// BOTH DIRECTIONS, and the second one matters as much as the first. A row the tree no longer
// holds is the document describing a tree that no longer exists -- GATES.md's own closing rule
// -- and it is also how a row comes to certify a narrowing somebody rewrote underneath it.
func TestTheGatesTableIsTheDerivedClassAndNotAListOfIt(t *testing.T) {
	derived := gatesReflectedNarrowings(t)
	rows, _ := gatesIndexRows(t)
	if len(rows) == 0 {
		// an ERROR and not a fatal, so the run still prints every row the document is missing.
		// A gate that stops before saying what is wrong makes its own repair a transcription
		// job, and a transcription job is where a row comes to say something nobody measured.
		t.Errorf("%s carries no index rows between its gates-index markers, so this compared the derived class against nothing",
			gatesDocumentPath)
	}
	indexed := map[string]gatesIndexRow{}
	for _, row := range rows {
		if previous, twice := indexed[row.key()]; twice {
			t.Errorf("%s indexes %s twice, at lines %d and %d; two readings of one narrowing is two places for it to be judged differently",
				gatesDocumentPath, row.key(), previous.line, row.line)
		}
		indexed[row.key()] = row
	}
	inTree := map[string]gatesNarrowing{}
	missing := []gatesNarrowing{}
	for _, narrowing := range derived {
		inTree[narrowing.key()] = narrowing
		row, listed := indexed[narrowing.key()]
		if !listed {
			missing = append(missing, narrowing)
			continue
		}
		if row.kind != narrowing.kind {
			t.Errorf("%s:%d reads %s as %s and it is %s", gatesDocumentPath, row.line, narrowing.key(), row.kind, narrowing.kind)
		}
	}
	for _, narrowing := range missing {
		t.Errorf("%s narrows a reflected member set at %s:%d and %s does not index it. The row to add:\n    %s",
			narrowing.fn, narrowing.file, narrowing.line, gatesDocumentPath, narrowing.row())
	}
	for _, row := range rows {
		if _, held := inTree[row.key()]; !held {
			t.Errorf("%s:%d indexes %s and no narrowing of that shape is in the tree; a row that outlives its narrowing certifies a reading of code nobody can find",
				gatesDocumentPath, row.line, row.key())
		}
	}
	open := []string{}
	for _, row := range rows {
		if row.verdict == "OPEN" {
			open = append(open, row.key())
		}
	}
	if len(open) != 0 {
		t.Errorf("%s carries %d OPEN narrowings: %v. An open row is a narrowing whose complement nobody has measured, and recording it is not closing it -- the two the table carried at 81b97ca were still open a round later",
			gatesDocumentPath, len(open), open)
	}
	byVerdict := map[string]int{}
	for _, row := range rows {
		byVerdict[row.verdict]++
	}
	t.Logf("%d narrowing occurrences derived over %d distinct (file, function, condition) keys; %d indexed; verdicts %v",
		len(derived), len(inTree), len(rows), byVerdict)
}

// ---------------------------------------------------------------------------
// the query the document publishes, measured rather than trusted
// ---------------------------------------------------------------------------

var gatesGrepLine = regexp.MustCompile(`^grep\s+(-[A-Za-z]+)\s+'([^']*)'`)

// The greps GATES.md publishes, read out of the document's own fenced block.
func gatesPublishedGreps(t *testing.T, lines []string) []*regexp.Regexp {
	t.Helper()
	patterns := []*regexp.Regexp{}
	inQuery := false
	for at, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "<!-- gates-query:begin -->") {
			inQuery = true
			continue
		}
		if strings.HasPrefix(trimmed, "<!-- gates-query:end -->") {
			inQuery = false
			continue
		}
		if !inQuery {
			continue
		}
		if !strings.HasPrefix(trimmed, "grep") {
			// the fences and blank lines are not queries; anything else inside these markers
			// is a query this gate did not run, which is a published query nobody measured
			if trimmed != "" && !strings.HasPrefix(trimmed, "```") {
				t.Errorf("%s:%d sits inside the published query and is not a grep this gate can run: %q",
					gatesDocumentPath, at+1, trimmed)
			}
			continue
		}
		parts := gatesGrepLine.FindStringSubmatch(trimmed)
		if parts == nil {
			t.Fatalf("%s:%d is a grep this gate cannot read: %q -- it runs the published query rather than a copy of it, so it has to be able to parse it",
				gatesDocumentPath, at+1, trimmed)
		}
		// extended regular expressions only, because this gate compiles the published pattern
		// with Go's engine and a basic regular expression would be silently mistranslated --
		// which is a query that measures itself against the wrong thing
		if !strings.Contains(parts[1], "E") {
			t.Fatalf("%s:%d publishes a grep without -E: %q. This gate compiles the pattern as an extended regular expression; a basic one would translate wrongly and quietly",
				gatesDocumentPath, at+1, trimmed)
		}
		compiled, err := regexp.Compile(parts[2])
		if err != nil {
			t.Fatalf("%s:%d publishes a pattern Go cannot compile: %v", gatesDocumentPath, at+1, err)
		}
		patterns = append(patterns, compiled)
	}
	if len(patterns) == 0 {
		t.Fatalf("%s publishes no grep between its gates-query markers, so its recall was measured over nothing", gatesDocumentPath)
	}
	return patterns
}

// How many of the derived narrowings a set of line patterns reaches, and which it does not.
func gatesRecallOf(t *testing.T, patterns []*regexp.Regexp, derived []gatesNarrowing) (int, []gatesNarrowing) {
	t.Helper()
	moduleRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolve the module root: %v", err)
	}
	body := map[string][]string{}
	reached, missed := 0, []gatesNarrowing{}
	for _, narrowing := range derived {
		lines, read := body[narrowing.file]
		if !read {
			source, err := os.ReadFile(filepath.Join(moduleRoot, filepath.FromSlash(narrowing.file)))
			if err != nil {
				t.Fatalf("read %s: %v", narrowing.file, err)
			}
			lines = strings.Split(strings.ReplaceAll(string(source), "\r\n", "\n"), "\n")
			body[narrowing.file] = lines
		}
		if narrowing.line < 1 || len(lines) < narrowing.line {
			t.Fatalf("%s has %d lines and a narrowing was derived at %d", narrowing.file, len(lines), narrowing.line)
		}
		text := lines[narrowing.line-1]
		hit := false
		for _, pattern := range patterns {
			if pattern.MatchString(text) {
				hit = true
				break
			}
		}
		if hit {
			reached++
		} else {
			missed = append(missed, narrowing)
		}
	}
	return reached, missed
}

var gatesStatedRecall = regexp.MustCompile(`gates-recall:\s*(\d+)\s*/\s*(\d+)`)

// TestTheQueryGatesPublishesIsMeasuredAgainstTheDerivationRatherThanTrusted is the other half
// of the sixth instance.
//
// The published Q1 was offered as "the query that finds the next one" and it greps a literal
// symbol, so the three sites spelled with another receiver were invisible to it -- and nothing
// said so, because a query's recall is exactly the kind of thing nobody measures. A query
// whose complement is unprinted is the same defect as a gate whose complement is unprinted.
//
// So the document states its greps' recall as a fraction and this decides it. The greps are
// not required to reach everything -- a grep cannot decide "a member of a reflected method
// set", which is the whole reason the derivation above exists. They are required to be HONEST
// about what they reach, and the sites they miss are printed here every run.
func TestTheQueryGatesPublishesIsMeasuredAgainstTheDerivationRatherThanTrusted(t *testing.T) {
	derived := gatesReflectedNarrowings(t)
	_, lines := gatesIndexRows(t)
	patterns := gatesPublishedGreps(t, lines)
	reached, missed := gatesRecallOf(t, patterns, derived)

	stated := gatesStatedRecall.FindStringSubmatch(strings.Join(lines, "\n"))
	if stated == nil {
		t.Fatalf("%s states no gates-recall: <reached>/<derived>, so its greps carry no claim this can decide", gatesDocumentPath)
	}
	if want := fmt.Sprintf("%d/%d", reached, len(derived)); stated[1]+"/"+stated[2] != want {
		t.Errorf("%s states gates-recall: %s/%s and the published greps reach %s of the narrowings derived here",
			gatesDocumentPath, stated[1], stated[2], want)
	}
	for _, narrowing := range missed {
		t.Logf("the published greps do not reach %s:%d (%s) %s", narrowing.file, narrowing.line, narrowing.fn, narrowing.cond)
	}
	t.Logf("the published greps reach %d of %d derived narrowings; %d are reached only by the derivation",
		reached, len(derived), len(missed))
}

// TestAQueryKeyedToOneSpellingOfTheReceiverStillMissesTheSitesItMissed is the sixth instance
// pinned as a regression rather than described in prose.
//
// This is the query GATES.md published at 81b97ca, verbatim. It is kept here so the claim that
// it was insufficient is a measurement anybody can re-run rather than a paragraph, and so that
// reverting the published query to a receiver-keyed one goes red instead of green. The three
// sites the reviewer named are asserted individually, because "it misses some" is a weaker
// statement than "it misses these".
func TestAQueryKeyedToOneSpellingOfTheReceiverStillMissesTheSitesItMissed(t *testing.T) {
	derived := gatesReflectedNarrowings(t)
	published := []*regexp.Regexp{
		regexp.MustCompile(`NumMethod\(\)`),
		regexp.MustCompile(`NumIn\(\)|NumOut\(\)|HasPrefix\(method\.Name|method\.Name ==|\.Kind\(\) ==`),
	}
	reached, missed := gatesRecallOf(t, published, derived)
	if len(missed) == 0 {
		t.Fatalf("the query GATES.md published at 81b97ca reaches all %d derived narrowings; the finding that it missed three was a measurement and this now disagrees with it, so one of the two is wrong",
			reached)
	}
	byFunction := map[string]bool{}
	for _, narrowing := range missed {
		byFunction[narrowing.fn] = true
	}
	for _, named := range []string{"TestNoVectorRunnerCanSkip", "trRecordLayerCodecMethods"} {
		if !byFunction[named] {
			t.Errorf("the receiver-keyed query reaches %s, and the reading that put it in this list said it did not; the two disagree and the code has moved under one of them",
				named)
		}
	}
	// mlsEncodingEmitters was the THIRD site that query missed and it is deliberately NOT
	// asserted above, because closing it moved it into range. Its condition used to read
	// `if name := writer.Method(i).Name; strings.HasPrefix(name, "Write")` and now reads
	// `if strings.HasPrefix(method.Name, "Write")` -- and `HasPrefix(method.Name` is exactly
	// the literal the 81b97ca query greps for. That is not the old query getting better. It is
	// the demonstration of what is wrong with it: its recall is a function of how somebody
	// spelled a receiver, and it moved by nineteen sites without one gate changing what it
	// decides. Asserting it still misses that site would be asserting something the tree no
	// longer says, which is rule 12 pointed at this control.
	if byFunction["mlsEncodingEmitters"] {
		t.Logf("the receiver-keyed query still misses mlsEncodingEmitters")
	} else {
		t.Logf("the receiver-keyed query now REACHES mlsEncodingEmitters: closing that site spelled its condition method.Name, the literal that query is keyed to")
	}
	t.Logf("the receiver-keyed query reaches %d of %d; it misses %d, in %v",
		reached, len(derived), len(missed), slices.Sorted(gatesKeysOf(byFunction)))
}

// TestEveryRowClaimingAPrintedComplementHasOne holds the two verdicts that make a CLAIM about
// run-time behaviour to the source that has to carry it.
//
// The vocabulary above separates NARROWING/complement from OPEN by one thing: whether the gate
// names, at run time, the members it removed. Until this test existed, that separation was
// PROSE. Deleting the t.Logf out of framedContentArmFields -- the site the seventh instance was
// found by, and the one whose complement had never been printed at all -- left every gate in
// this file green, so a row could go on certifying a print that was no longer there. The same
// held for NARROWING/refusal, whose whole claim is that the removed member is REPORTED.
//
// WHAT THIS DECIDES AND WHAT IT DOES NOT. It decides that the function carrying the row calls
// something spelled Log/Logf (for a complement) or Errorf/Fatalf/Error/Fatal (for a refusal). It
// does NOT decide that what is printed IS the removed set -- that needs the run, not the source,
// and reading it off the run means matching a printed set against a derived one, which is a
// bigger machine than this file has. So it is a PROXY, and it is stated as one here rather than
// discovered later: it catches a print deleted, a verdict written onto a site that never printed,
// and a refusal row over a site that only skips. It does not catch a print that says the wrong
// thing.
//
// It fails closed the other way too: a complement printed by the function's CALLER rather than
// by the function itself reads as absent here. That is the right direction -- move the print, or
// change the verdict -- and it is why the failure names both options.
func TestEveryRowClaimingAPrintedComplementHasOne(t *testing.T) {
	paths, _, parsed := gatesTestSources(t)
	byPath := map[string]*ast.File{}
	for _, path := range paths {
		byPath[path] = parsed[path]
	}
	rows, _ := gatesIndexRows(t)
	// the spellings each verdict claims, derived from what the verdict MEANS rather than from
	// the sites: a complement is named in the log, a refusal is reported through the failure.
	claims := map[string][]string{
		"NARROWING/complement": {"Log", "Logf"},
		"NARROWING/refusal":    {"Error", "Errorf", "Fatal", "Fatalf"},
	}
	// the verdicts that claim nothing about run time, said out loud rather than left as the
	// remainder. The two maps together must be the document's whole vocabulary: a verdict added
	// to it and classified in neither is a verdict this gate would pass over in silence, which
	// is the shape of every instance in GATES.md.
	claimsNothing := map[string]bool{
		"CLASS/results": true,
		"DRIVER":        true,
		"NOT-A-MEMBER":  true,
		"OPEN":          true,
	}
	for verdict := range gatesVerdicts {
		_, claimed := claims[verdict]
		if claimed == claimsNothing[verdict] {
			t.Errorf("the vocabulary carries the verdict %q and this gate classifies it as %s, so a row carrying it would be %s. Say what it claims at run time, or say that it claims nothing",
				verdict, map[bool]string{true: "both claiming a reporter and claiming nothing", false: "neither"}[claimed],
				map[bool]string{true: "judged twice", false: "judged by nothing here"}[claimed])
		}
	}
	judged, unheld := 0, 0
	for _, row := range rows {
		wanted, claimsSomething := claims[row.verdict]
		if !claimsSomething {
			continue
		}
		file, read := byPath[row.file]
		if !read {
			t.Errorf("%s:%d indexes a site in %s and this gate's scope did not read that file, so the row's claim was checked against nothing",
				gatesDocumentPath, row.line, row.file)
			continue
		}
		found := false
		for _, declaration := range file.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil || function.Name.Name != row.fn {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, isCall := node.(*ast.CallExpr)
				if !isCall {
					return true
				}
				if selector, isSelector := gatesUnparen(call.Fun).(*ast.SelectorExpr); isSelector {
					if slices.Contains(wanted, selector.Sel.Name) {
						found = true
					}
				}
				return true
			})
		}
		judged++
		if !found {
			unheld++
			t.Errorf("%s:%d reads %s as %s, and that verdict CLAIMS the removed members are named at run time through one of %v. %s calls none of them. Either the narrowing does not print what it removes -- in which case the row is OPEN and not this -- or the print lives in the caller, in which case move it to the narrowing so the row is decided where the narrowing is",
				gatesDocumentPath, row.line, row.key(), row.verdict, wanted, row.fn)
		}
	}
	if judged == 0 {
		t.Errorf("no row of %s carries a verdict that claims run-time behaviour, so this gate judged nothing; the vocabulary has two such verdicts and the index is not empty",
			gatesDocumentPath)
	}
	t.Logf("%d rows claim a print or a refusal at run time; %d of them are not carried by the function they name", judged, unheld)
}

// TestEveryComplementThisDerivationComputesIsPrintedByIt turns this file's own rule on this
// file, and it is the answer to a mutation that survived two rounds running.
//
// GATES.md's operational rule is "a gate that narrows must NAME, at run time, every member it
// removed". This derivation narrows three times before it reads a line of these trees -- the
// exported structs the descriptor sentence removed, the exported spellings the door sentence
// removed, and the descriptor fields that are neither the name nor the type -- and it prints all
// three. Nothing held it there: deleting any one of those t.Logf calls left every gate in this
// file green, which was reported as a surviving mutation when the doors were derived and would
// have been reported again for the readings.
//
// So the CLASS IS DERIVED FROM THE DOOR SET'S OWN DECLARATION rather than listed here: every
// field of gatesDoorSet that carries a set of spellings must be named in an argument of a
// reporter call inside the function that reports the class. A complement added to that struct in
// a later round has to be printed on the commit that adds it, or this goes red.
//
// WHAT IT DECIDES AND WHAT IT DOES NOT, said here rather than discovered later. It decides that
// the field is MENTIONED in a Log/Logf argument, not that what is printed is the set -- the same
// proxy TestEveryRowClaimingAPrintedComplementHasOne is, and stated as one for the same reason.
// It also reads only the fields spelled as a slice of strings, so the door MAP itself, printed
// beside them, is outside this reading; the fields it removes are named on every run beside the
// ones it holds.
//
// AND ITS SCOPE IS TWO NAMES, WHICH IS A NARROWING AND IS SAID SO. It reads this file and the one
// function in it that reports the class. That is not derived and could not usefully be: the
// subject of this gate is one derivation, and the place a derivation's complement belongs is the
// function that hands the class back. What the naming costs is stated instead of hidden -- a
// complement printed by some OTHER function of this file reads as absent here, exactly as a
// complement printed by an index row's caller reads as absent in the row gate. That direction is
// deliberate in both: move the print to where the narrowing is, or say the narrowing does not
// print.
func TestEveryComplementThisDerivationComputesIsPrintedByIt(t *testing.T) {
	fileSet := token.NewFileSet()
	source, err := os.ReadFile("gates_index_test.go")
	if err != nil {
		t.Fatalf("read this file: %v", err)
	}
	parsed, err := parser.ParseFile(fileSet, "gates_index_test.go", source, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse this file: %v", err)
	}
	carried, otherwise := []string{}, []string{}
	for _, declaration := range parsed.Decls {
		general, isGeneral := declaration.(*ast.GenDecl)
		if !isGeneral || general.Tok != token.TYPE {
			continue
		}
		for _, specification := range general.Specs {
			typed, isType := specification.(*ast.TypeSpec)
			if !isType || typed.Name.Name != "gatesDoorSet" {
				continue
			}
			structure, isStruct := typed.Type.(*ast.StructType)
			if !isStruct || structure.Fields == nil {
				continue
			}
			for _, field := range structure.Fields.List {
				spelling := gatesTypeSpelling(field.Type)
				for _, name := range field.Names {
					if spelling == "[]string" {
						carried = append(carried, name.Name)
					} else {
						otherwise = append(otherwise, name.Name+" "+spelling)
					}
				}
			}
		}
	}
	if len(carried) == 0 {
		t.Fatal("gatesDoorSet declares no field carrying a set of spellings, so this gate read nothing and would report clean over a derivation that printed no complement at all")
	}
	reported := map[string]bool{}
	for _, declaration := range parsed.Decls {
		function, isFunction := declaration.(*ast.FuncDecl)
		if !isFunction || function.Body == nil || function.Name.Name != "gatesReflectedNarrowings" {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, isCall := node.(*ast.CallExpr)
			if !isCall {
				return true
			}
			selector, isSelector := gatesUnparen(call.Fun).(*ast.SelectorExpr)
			if !isSelector || (selector.Sel.Name != "Log" && selector.Sel.Name != "Logf") {
				return true
			}
			for _, argument := range call.Args {
				ast.Inspect(argument, func(inner ast.Node) bool {
					switch read := inner.(type) {
					case *ast.SelectorExpr:
						reported[read.Sel.Name] = true
					case *ast.Ident:
						reported[read.Name] = true
					}
					return true
				})
			}
			return true
		})
	}
	if len(reported) == 0 {
		t.Fatal("gatesReflectedNarrowings names nothing in a reporter call, so this derivation narrows three times in silence -- which is the rule GATES.md states, broken by the file that states it")
	}
	for _, field := range carried {
		if !reported[field] {
			t.Errorf("gatesDoorSet carries the set %s and gatesReflectedNarrowings never names it at run time; a narrowing this derivation performs before it reads a line of these trees would then be one nobody can read off a run",
				field)
		}
	}
	t.Logf("the derivation's own complements: %v, each named in a reporter call of gatesReflectedNarrowings", carried)
	// and this gate's own narrowing, printed. It reads the fields spelled as a set of spellings
	// and nothing else, so the door MAP -- printed beside them, and not held here -- and the two
	// single readings are removed by it. Naming them is what lets a reader ask whether one of
	// them should have been held too.
	t.Logf("and what this gate removes: %d field(s) of gatesDoorSet are not a set of spellings and are held by nothing here: %v",
		len(otherwise), otherwise)

	// AND THE COMPLEMENT THE SCAN HALF COMPUTES, held the same way -- because the half above
	// only reaches the three narrowings this derivation makes BEFORE it reads a line of these
	// trees, and gatesRecordDecided makes a fourth while reading them. That one had no reporter
	// at all for two rounds: the round that widened it off "exactly one bool" closed the arity
	// and left the print, which is one half of one rule closed and the other half not.
	//
	// The class is derived rather than named: an accumulator is a parameter the derivation only
	// ever WRITES INTO, so the function it is passed to cannot be the thing that reads it out,
	// and the only place left for it to be read is a reporter in the caller. Two proxies, said
	// out loud the way the halves above say theirs: this decides that every function-typed
	// parameter of the recorder is CALLED, and that the accumulator is NAMED in a reporter --
	// not that what is called reports the removals, nor that what is printed IS them.
	for _, declaration := range parsed.Decls {
		function, isFunction := declaration.(*ast.FuncDecl)
		if !isFunction || function.Body == nil || function.Name.Name != "gatesRecordDecided" {
			continue
		}
		reporters, others := gatesReporterParameters(function)
		if len(reporters) == 0 {
			t.Error("gatesRecordDecided declares no function-typed parameter, so this half of the gate reads nothing and would report clean over a narrowing that hands its removals nowhere")
		}
		t.Logf("the recorder's reporters: %v, each required to be called; and what this reading removes: %v, which are not functions and cannot be a reporter",
			reporters, others)
		for _, parameter := range reporters {
			called := false
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, isCall := node.(*ast.CallExpr)
				if !isCall {
					return true
				}
				if named, isIdent := gatesUnparen(call.Fun).(*ast.Ident); isIdent && named.Name == parameter {
					called = true
				}
				return true
			})
			if !called {
				t.Errorf("gatesRecordDecided declares the reporter %s and never calls it, so the narrowing it performs over which result positions decide would remove members in silence -- which is the rule this file publishes, broken by the file that publishes it",
					parameter)
			}
		}
	}
	for _, declaration := range parsed.Decls {
		function, isFunction := declaration.(*ast.FuncDecl)
		if !isFunction || function.Body == nil || function.Name.Name != "gatesNarrowingsIn" {
			continue
		}
		accumulators, read := gatesWriteOnlyParameters(function)
		if len(accumulators) == 0 {
			t.Error("gatesNarrowingsIn takes no accumulator this gate can read, so the complement the scan half computes is held by nothing here and deleting its print would leave every other gate in this file green")
		}
		for _, accumulator := range accumulators {
			if !reported[accumulator] {
				t.Errorf("gatesNarrowingsIn fills %s and nothing gatesReflectedNarrowings reports names it; an accumulator a derivation only writes into is read in a reporter or it is read nowhere",
					accumulator)
			}
		}
		t.Logf("and the complement the scan half computes, held by name: %v, filled by gatesNarrowingsIn and named in a reporter of gatesReflectedNarrowings; what this reading removes is %v, which the derivation READS as well as writes and so is held by what reads them rather than by a print",
			accumulators, read)
	}
}

// The parameters of one function that are themselves functions -- a reporter a narrowing hands
// its removals to -- AND THE ONES THIS READING REMOVED, because a narrowing inside a gate that
// checks narrowings is still a narrowing. Derived off the declaration so a second reporter added
// in a later round is held on the commit that adds it.
func gatesReporterParameters(function *ast.FuncDecl) (reporters []string, others []string) {
	if function.Type.Params == nil {
		return nil, nil
	}
	for _, parameter := range function.Type.Params.List {
		_, isFunction := parameter.Type.(*ast.FuncType)
		for _, name := range parameter.Names {
			if name.Name == "_" {
				continue
			}
			if isFunction {
				reporters = append(reporters, name.Name)
				continue
			}
			others = append(others, name.Name+" "+gatesTypeSpelling(parameter.Type))
		}
	}
	return reporters, others
}

// The parameters one function only ever WRITES INTO -- every occurrence of the identifier is an
// index expression on the left of an assignment. That is what an accumulator IS, said without
// naming one: a value the callee fills and never reads, so whatever reads it out is somewhere
// else, and for a complement the only somewhere else that satisfies this file's own rule is a
// reporter.
func gatesWriteOnlyParameters(function *ast.FuncDecl) (filled []string, read []string) {
	if function.Type.Params == nil || function.Body == nil {
		return nil, nil
	}
	candidates := map[string]bool{}
	for _, parameter := range function.Type.Params.List {
		for _, name := range parameter.Names {
			if name.Name != "_" {
				candidates[name.Name] = true
			}
		}
	}
	occurrences := map[string]int{}
	writes := map[string]int{}
	ast.Inspect(function.Body, func(node ast.Node) bool {
		if assignment, isAssignment := node.(*ast.AssignStmt); isAssignment {
			for _, side := range assignment.Lhs {
				index, isIndex := gatesUnparen(side).(*ast.IndexExpr)
				if !isIndex {
					continue
				}
				if named, isIdent := gatesUnparen(index.X).(*ast.Ident); isIdent && candidates[named.Name] {
					writes[named.Name] += 1
				}
			}
		}
		if named, isIdent := node.(*ast.Ident); isIdent && candidates[named.Name] {
			occurrences[named.Name] += 1
		}
		return true
	})
	for name := range candidates {
		if writes[name] > 0 && writes[name] == occurrences[name] {
			filled = append(filled, name)
			continue
		}
		read = append(read, name)
	}
	slices.Sort(filled)
	slices.Sort(read)
	return filled, read
}

// ---------------------------------------------------------------------------
// the ninth instance, MEASURED rather than described
// ---------------------------------------------------------------------------

// gatesStatedDoors is the pair GATES.md publishes for the door sentence: how many spellings the
// sentence admits WITH its container clause, and how many the same sentence admits without it.
// The gap is the ninth instance's size, and publishing it as a fraction this file is held to is
// the same device the recall fraction is: a number nobody measures is a claim.
var gatesStatedDoors = regexp.MustCompile(`gates-doors:\s*(\d+)\s*/\s*(\d+)`)

// The door sentence asked with NO container clause at all -- a member descriptor named anywhere
// in the result type answers one -- so the size of the clause `X` or `[]X` can be read off the
// difference rather than argued about.
//
// It deliberately over-reports, by matching the descriptor's name inside the rendered result
// spelling: over-reporting makes the number this publishes LARGER, and larger is the safe
// direction for a measurement whose whole job is to say how much a narrowing removes.
func gatesDoorsWithoutTheContainerClause(files []*ast.File, descriptors []string) map[string][]string {
	mentions := func(results *ast.FieldList) string {
		if results == nil {
			return ""
		}
		for _, spelling := range gatesFieldSpellings(results) {
			for _, descriptor := range descriptors {
				if strings.Contains(spelling, descriptor) {
					return spelling
				}
			}
		}
		return ""
	}
	wide := map[string][]string{}
	for _, file := range files {
		for _, declaration := range file.Decls {
			switch node := declaration.(type) {
			case *ast.GenDecl:
				if node.Tok != token.TYPE {
					continue
				}
				for _, specification := range node.Specs {
					typed, isType := specification.(*ast.TypeSpec)
					if !isType || !ast.IsExported(typed.Name.Name) {
						continue
					}
					declared, isInterface := typed.Type.(*ast.InterfaceType)
					if !isInterface || declared.Methods == nil {
						continue
					}
					for _, member := range declared.Methods.List {
						signature, isFunction := member.Type.(*ast.FuncType)
						if !isFunction {
							continue
						}
						for _, name := range member.Names {
							if !ast.IsExported(name.Name) {
								continue
							}
							if answered := mentions(signature.Results); answered != "" {
								wide[name.Name] = append(wide[name.Name], typed.Name.Name+"."+name.Name+" "+answered)
							}
						}
					}
				}
			case *ast.FuncDecl:
				if !ast.IsExported(node.Name.Name) {
					continue
				}
				if answered := mentions(node.Type.Results); answered != "" {
					on := "func"
					if node.Recv != nil && len(node.Recv.List) > 0 {
						on = gatesTypeSpelling(node.Recv.List[0].Type)
					}
					wide[node.Name.Name] = append(wide[node.Name.Name], on+"."+node.Name.Name+" "+answered)
				}
			}
		}
	}
	return wide
}

// TestTheContainerClauseOnTheDoorSentenceIsANarrowingAndItsSizeIsMeasured is the ninth instance
// made VISIBLE, and it is deliberately not the ninth instance CLOSED.
//
// The door sentence reads a descriptor bare or in a slice of them. That clause is two literals,
// it was written into gatesAnswers and gatesDoorSet.declares under no sentence at all, and unlike
// every other narrowing this derivation performs it printed no complement -- so the spellings it
// removed sat in the 126-name notDoors list reading as ordinary non-doors, which is exactly why
// nobody found it for a round. This gate does the one thing the criterion in GATES.md asks of a
// literal that cannot be moved: it makes being wrong VISIBLE. It asks the same sentence without
// the clause, names every spelling the clause removes, and holds GATES.md's published pair to the
// measurement, so the day the toolchain moves the document goes red with the new list in hand.
//
// WHAT IT DOES NOT DO, and this is the whole of the decision recorded in GATES.md: it does not
// widen the door set. A narrowing spelled `for member := range subject.Methods()` is still
// invisible to every gate in this file, and a container this reading does not know still binds
// nothing. Printing a complement is not the same as removing a narrowing, and pretending
// otherwise is how a boundary comes to read as coverage.
func TestTheContainerClauseOnTheDoorSentenceIsANarrowingAndItsSizeIsMeasured(t *testing.T) {
	doors := gatesDoorsOf(t)
	_, files, err := gatesReflectSource()
	if err != nil {
		t.Fatalf("read reflect's own source: %v", err)
	}
	wide := gatesDoorsWithoutTheContainerClause(files, doors.descriptors)
	removed := []string{}
	declarations := []string{}
	for name, sites := range wide {
		if doors.isDoor(name) {
			continue
		}
		removed = append(removed, name)
		declarations = append(declarations, sites...)
	}
	slices.Sort(removed)
	slices.Sort(declarations)

	// FIRST, that it IS a narrowing. If this ever came back empty the clause would be removing
	// nothing on the toolchain in use, which is the OTHER dangerous reading in GATES.md's table
	// and would want the clause deleted rather than measured.
	if len(removed) == 0 {
		t.Error("the container clause on the door sentence removes no spelling of reflect on this toolchain; a narrowing that removes nothing is the empty-complement row of GATES.md's own table, and it wants deleting or refusing rather than measuring")
	}
	t.Logf("the container clause removes %d spelling(s) of package reflect -- %v -- over %d exported declaration(s): %v. These are doors by the door sentence as GATES.md writes it and are not doors here, and every one of them sits in the notDoors list reading as an ordinary non-door. THIS IS THE NINTH INSTANCE, printed rather than closed",
		len(removed), removed, len(declarations), declarations)

	source, err := os.ReadFile(gatesDocumentPath)
	if err != nil {
		t.Fatalf("read %s: %v", gatesDocumentPath, err)
	}
	stated := gatesStatedDoors.FindStringSubmatch(string(source))
	if stated == nil {
		t.Fatalf("%s states no gates-doors: <admitted>/<without the container clause>, so the size of the clause it now describes is a claim nobody measures -- which is the defect this whole file is about", gatesDocumentPath)
	}
	admitted, without := len(doors.doors), len(doors.doors)+len(removed)
	if stated[1] != fmt.Sprint(admitted) || stated[2] != fmt.Sprint(without) {
		t.Errorf("%s publishes gates-doors: %s/%s and the derivation measures %d/%d; the door sentence admits %d spellings with its container clause and %d without, the difference being %v. Update the document to the measurement",
			gatesDocumentPath, stated[1], stated[2], admitted, without, admitted, without, removed)
	}
}

// TestTheExportedFieldNarrowingRefusesRatherThanContinuing drives the refusal that replaced a
// `continue`, because nothing in these trees can drive it: reflect.Method and reflect.StructField
// are exported through and through, so the complement it guards is EMPTY and the refusal is
// unreachable from the real toolchain. A fail-closed path nothing drives is a fail-closed path
// nobody has checked, and this file has already shipped one of those.
func TestTheExportedFieldNarrowingRefusesRatherThanContinuing(t *testing.T) {
	if err := gatesRefuseUnexportedDescriptorFields([]string{"Method", "StructField"}, nil); err != nil {
		t.Errorf("the exported-field narrowing refuses with an empty complement: %v -- it must refuse only when it removes something", err)
	}
	err := gatesRefuseUnexportedDescriptorFields([]string{"Method"}, []string{"hidden"})
	if err == nil {
		t.Fatal("a member descriptor carrying an unexported field is accepted; that narrowing then removes a reading in silence, which is the `continue` this refusal replaced and the one form GATES.md's own table forbids for an empty complement")
	}
	if !errors.Is(err, errGatesUnexportedDescriptorField) {
		t.Errorf("the refusal answers %v, which is not %v; a sentinel is what lets a caller tell this refusal from a parse failure", err, errGatesUnexportedDescriptorField)
	}
	if !strings.Contains(err.Error(), "hidden") {
		t.Errorf("the refusal answers %q and does not name the field it removed; a refusal that does not name its complement is the same silence one message over", err)
	}
	// and the reason it has to be driven here: the real complement is empty, so the real
	// derivation never reaches this path.
	if held := gatesDoorsOf(t).unexported; len(held) != 0 {
		t.Logf("reflect now carries unexported descriptor field(s) %v, so the refusal above is reachable from the toolchain and this control has become redundant rather than necessary", held)
	}
}

// ---------------------------------------------------------------------------
// the OTHER reflection library these trees use, and why nine rows of the index
// are here by coincidence rather than by rule
// ---------------------------------------------------------------------------

// gatesObjectDoorSet is the member-door set of go/types, derived the way reflect's was, so the
// overlap between the two can be MEASURED instead of assumed.
//
// WHY THIS EXISTS. Nine rows of the index below sit over a go/types member set, not a reflect
// one -- eight functions that walk a *types.Struct's fields or a *types.Named's methods. They are
// in the index because go/types happens to spell two of its member doors `Field` and `Method`,
// exactly as reflect does, and the derivation reads a SPELLING. Nothing about the reading admits
// them: the descriptor sentence that produces reflect's doors reads exported STRUCTS carrying a
// name beside a type, and go/types hands a member back as an OBJECT whose name and type are
// METHODS, so that sentence admits nothing at all there. The index reaches those nine rows the
// way a stopped clock reaches the hour.
//
// So the sentence is asked again one shape over: a MEMBER OBJECT is an exported named type of
// go/types whose method set NAMES one member and answers THAT MEMBER'S TYPE -- a `Name() string`
// beside a `Type() Type`, reached through embedding the way Go reaches it -- and a door is an
// exported function or interface method answering one, a pointer to one, or a slice of them.
// It over-reports, and deliberately: go/types calls a package name and a label objects too, so
// spellings that hand back something no gate here would call a member are admitted. Over-reporting
// makes the COMPLEMENT this prints larger rather than smaller, which is the safe direction for a
// line whose whole job is to say what this index does not reach.
type gatesObjectDoorSet struct {
	objects []string
	doors   map[string]string
	// the coincidence, and its complement: which of these spellings reflect's derived door set
	// also carries, and which it does not
	shared   []string
	unshared []string
	// how many exported structs of go/types the FIELD-shaped descriptor sentence admitted. It is
	// carried so the claim "no door here is derived by rule" is a number on the run rather than a
	// sentence in this comment.
	descriptors int
	structs     int
}

var gatesObjectDoorsOnce = sync.OnceValues(gatesDeriveObjectDoors)

// The exported names of one type's method set, resolved through embedding.
func gatesResolveMethodSets(methods map[string]map[string][]string, embeds map[string][]string) {
	// bounded rather than run to a fixed point, because an embedding cycle in a package this
	// reads but does not compile would otherwise hang the suite. The bound is not justified by
	// how deeply go/types happens to embed today: it is justified by its FAILURE DIRECTION. An
	// under-resolved method set finds fewer member objects, fewer doors, and a smaller overlap --
	// and the gate below requires the overlap to be non-empty, so under-resolving turns it RED
	// rather than quietly shrinking the gap it reports.
	for pass := 0; pass < 4; pass++ {
		for named, embedded := range embeds {
			for _, into := range embedded {
				for name, results := range methods[into] {
					if _, own := methods[named][name]; !own {
						if methods[named] == nil {
							methods[named] = map[string][]string{}
						}
						methods[named][name] = results
					}
				}
			}
		}
	}
}

func gatesDeriveObjectDoors() (gatesObjectDoorSet, error) {
	set := gatesObjectDoorSet{doors: map[string]string{}}
	root := build.Default.GOROOT
	if root == "" {
		return set, fmt.Errorf("the toolchain reports no GOROOT, so go/types' own source cannot be read")
	}
	directory := filepath.Join(root, "src", "go", "types")
	entries, err := os.ReadDir(directory)
	if err != nil {
		return set, fmt.Errorf("read %s: %w", directory, err)
	}
	fileSet := token.NewFileSet()
	files := []*ast.File{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		source, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			return set, fmt.Errorf("read %s: %w", entry.Name(), err)
		}
		parsed, err := parser.ParseFile(fileSet, entry.Name(), source, parser.SkipObjectResolution)
		if err != nil {
			return set, fmt.Errorf("parse %s: %w", entry.Name(), err)
		}
		if parsed.Name == nil || parsed.Name.Name != "types" {
			continue
		}
		files = append(files, parsed)
	}
	if len(files) == 0 {
		return set, fmt.Errorf("%s holds no source of package types", directory)
	}

	methods := map[string]map[string][]string{}
	embeds := map[string][]string{}
	declared := map[string]bool{}
	for _, file := range files {
		for _, declaration := range file.Decls {
			switch node := declaration.(type) {
			case *ast.GenDecl:
				if node.Tok != token.TYPE {
					continue
				}
				for _, specification := range node.Specs {
					typed, isType := specification.(*ast.TypeSpec)
					if !isType {
						continue
					}
					declared[typed.Name.Name] = true
					switch shape := typed.Type.(type) {
					case *ast.StructType:
						if ast.IsExported(typed.Name.Name) {
							set.structs++
							if _, _, _, _, is := gatesDescriptorFields(shape); is {
								set.descriptors++
							}
						}
						if shape.Fields == nil {
							continue
						}
						for _, field := range shape.Fields.List {
							// an embedded field carries no name of its own, which is how the
							// language spells "this type's method set is also mine"
							if len(field.Names) != 0 {
								continue
							}
							embeds[typed.Name.Name] = append(embeds[typed.Name.Name],
								strings.TrimPrefix(gatesTypeSpelling(field.Type), "*"))
						}
					case *ast.InterfaceType:
						if shape.Methods == nil {
							continue
						}
						for _, member := range shape.Methods.List {
							signature, isFunction := member.Type.(*ast.FuncType)
							if !isFunction {
								for _, embedded := range member.Names {
									_ = embedded
								}
								if len(member.Names) == 0 {
									embeds[typed.Name.Name] = append(embeds[typed.Name.Name],
										strings.TrimPrefix(gatesTypeSpelling(member.Type), "*"))
								}
								continue
							}
							for _, name := range member.Names {
								if methods[typed.Name.Name] == nil {
									methods[typed.Name.Name] = map[string][]string{}
								}
								methods[typed.Name.Name][name.Name] = gatesFieldSpellings(signature.Results)
							}
						}
					}
				}
			case *ast.FuncDecl:
				if node.Recv == nil || len(node.Recv.List) != 1 {
					continue
				}
				receiver := strings.TrimPrefix(gatesTypeSpelling(node.Recv.List[0].Type), "*")
				if methods[receiver] == nil {
					methods[receiver] = map[string][]string{}
				}
				methods[receiver][node.Name.Name] = gatesFieldSpellings(node.Type.Results)
			}
		}
	}
	gatesResolveMethodSets(methods, embeds)

	// the member objects: the same sentence as the descriptor's, asked of a method set
	objects := map[string]bool{}
	for named, set := range methods {
		if !ast.IsExported(named) || !declared[named] {
			continue
		}
		answersName := len(set["Name"]) == 1 && set["Name"][0] == "string"
		answersType := len(set["Type"]) == 1 && set["Type"][0] == "Type"
		if answersName && answersType {
			objects[named] = true
		}
	}
	if len(objects) == 0 {
		return set, fmt.Errorf("no exported named type of package types names a member and answers its type, over %d types read; this reading would report the whole coincidence empty", len(declared))
	}

	// and the doors onto one
	for _, file := range files {
		for _, declaration := range file.Decls {
			switch node := declaration.(type) {
			case *ast.GenDecl:
				if node.Tok != token.TYPE {
					continue
				}
				for _, specification := range node.Specs {
					typed, isType := specification.(*ast.TypeSpec)
					if !isType || !ast.IsExported(typed.Name.Name) {
						continue
					}
					shape, isInterface := typed.Type.(*ast.InterfaceType)
					if !isInterface || shape.Methods == nil {
						continue
					}
					for _, member := range shape.Methods.List {
						signature, isFunction := member.Type.(*ast.FuncType)
						if !isFunction {
							continue
						}
						for _, name := range member.Names {
							if ast.IsExported(name.Name) && gatesAnswersObject(signature.Results, objects) {
								set.doors[name.Name] = "an interface method answering a member object"
							}
						}
					}
				}
			case *ast.FuncDecl:
				if !ast.IsExported(node.Name.Name) {
					continue
				}
				if gatesAnswersObject(node.Type.Results, objects) {
					set.doors[node.Name.Name] = "answers a member object"
				}
			}
		}
	}
	set.objects = slices.Sorted(gatesKeysOf(objects))
	return set, nil
}

// Whether a result list answers one of a set of named types, bare, behind a pointer, or in a
// slice of either.
//
// THREE ENTRIES HERE AND TWO IN gatesAnswers, IN ONE COMMIT, WRITTEN BY ONE HAND IN ONE SITTING --
// and that is the evidence GATES.md's stopping argument rests on. The difference is defensible
// per library (go/types hands a member back as a pointer, reflect hands one back by value) and
// neither list says it is a narrowing nor prints what it removed, which is the defect. See
// gatesAnswers for why the pointer is not carried back and why doing so would make the sentence
// read more complete rather than be more complete.
func gatesAnswersObject(results *ast.FieldList, wanted map[string]bool) bool {
	for _, spelling := range gatesFieldSpellings(results) {
		bare := strings.TrimPrefix(strings.TrimPrefix(spelling, "[]"), "*")
		if wanted[bare] {
			return true
		}
	}
	return false
}

// TestTheGoTypesRowsOfTheIndexAreACoincidenceOfSPELLINGAndTheCoincidenceIsMeasured decides,
// rather than asserts in prose, the one gap in this index a reviewer named and this round agrees
// with.
//
// Nine rows of GATES.md's index -- eight functions -- narrow a go/types member set and not a
// reflect one. They are indexed because the derivation reads a SELECTOR SPELLING and go/types
// spells two of its member doors the way reflect spells two of its own. That is a coincidence,
// and a coincidence is exactly the kind of thing that reads as coverage until somebody measures
// it, so it is measured here in both directions:
//
//   - the reflect descriptor sentence admits NOTHING in go/types, so no door of go/types is
//     derived by rule. If this ever stops being true the paragraph in GATES.md that calls the
//     overlap a coincidence has to be rewritten, and this fails rather than letting it stand;
//   - the overlap is non-empty, which is what the nine rows are; and
//   - the COMPLEMENT is printed and required to be non-empty: the go/types member doors this
//     index cannot see. That is the honest size of the gap, and it is the number a reader needs
//     to decide whether to build the second derivation for real.
//
// WHAT THIS DOES NOT DO. It does not add those doors to the index. Doing that would put every
// go/types member set in these trees inside this file's claim, which is a second derivation with
// its own scope, its own over-reports and its own complement to print -- a round's work, not a
// line. The decision recorded here is to state the boundary and measure it, not to widen it
// quietly, and GATES.md says so where the nine rows are.
func TestTheGoTypesRowsOfTheIndexAreACoincidenceOfSPELLINGAndTheCoincidenceIsMeasured(t *testing.T) {
	reflected := gatesDoorsOf(t)
	objects, err := gatesObjectDoorsOnce()
	if err != nil {
		t.Fatalf("derive go/types' member doors: %v -- this gate measures the overlap between the two reflection libraries these trees use, and a boundary nobody measured is the defect GATES.md records instance after instance of", err)
	}
	if objects.descriptors != 0 {
		t.Errorf("the member-descriptor sentence admits %d of package types' %d exported structs, so this index's doors are NOT reached there only by coincidence and GATES.md's paragraph about it is wrong",
			objects.descriptors, objects.structs)
	}
	shared, unshared := []string{}, []string{}
	for _, door := range slices.Sorted(gatesKeysOf(objects.doors)) {
		if reflected.isDoor(door) {
			shared = append(shared, door)
		} else {
			unshared = append(unshared, door)
		}
	}
	if len(shared) == 0 {
		t.Errorf("no member door of package types is spelled the way one of reflect's is, and the index carries rows over go/types member sets; those rows have no explanation at all then, and one of the two readings is wrong")
	}
	if len(unshared) == 0 {
		t.Errorf("every member door of package types is spelled the way one of reflect's is, so this index reaches all of them and the boundary GATES.md states -- that the go/types rows are a coincidence and an incomplete one -- overstates the gap")
	}
	t.Logf("the other library: %d exported named type(s) of package types name a member and answer its type (%v), reached through %d exported door(s); the FIELD-shaped descriptor sentence admits %d of its %d exported structs, so none of this is derived by rule",
		len(objects.objects), objects.objects, len(objects.doors), objects.descriptors, objects.structs)
	t.Logf("the coincidence: %d spelling(s) %v are doors in BOTH libraries, which is the whole reason this index carries any row over a go/types member set",
		len(shared), shared)
	t.Logf("and its complement, which this index does NOT reach: %d exported spelling(s) of package types answer a member object and are not spelled the way any reflect door is: %v",
		len(unshared), unshared)
}

// ---------------------------------------------------------------------------
// controls: the derivation is only worth its claim if it can be shown to see
// ---------------------------------------------------------------------------

const gatesControlSource = `package control

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

func receiverSpelledAnythingAtAll(t *testing.T) []string {
	found := []string{}
	of := reflect.TypeOf((*strings.Builder)(nil))
	for i := 0; i < of.NumMethod(); i++ {
		if name := of.Method(i).Name; strings.HasSuffix(name, "String") {
			found = append(found, name)
		}
	}
	return found
}

func narrowedInAnotherFunction(members []reflect.Method) []reflect.Method {
	kept := []reflect.Method{}
	for _, entry := range members {
		if entry.Type.NumIn() != 1 {
			continue
		}
		kept = append(kept, entry)
	}
	return kept
}

func throughABoundSignature(subject reflect.Type) []reflect.Method {
	kept := []reflect.Method{}
	for i := range subject.NumMethod() {
		member := subject.Method(i)
		signature := member.Type
		if signature.NumOut() != 1 {
			continue
		}
		kept = append(kept, member)
	}
	return kept
}

func aFieldSpelledNameIsNotAMember(entries []struct{ name string }) int {
	count := 0
	for _, entry := range entries {
		if entry.name == "" {
			continue
		}
		count++
	}
	return count
}

func aMemberNameAndAFieldOfTheSameSpelling() int {
	kept := 0
	of := reflect.TypeOf((*strings.Builder)(nil))
	for i := 0; i < of.NumMethod(); i++ {
		name := of.Method(i).Name
		if strings.HasPrefix(name, "Write") {
			kept++
		}
	}
	for _, entry := range []struct{ name string }{{"a"}} {
		if entry.name == "" {
			kept--
		}
	}
	return kept
}

func aDirectoryEntryIsNotAMember(names []string) int {
	count := 0
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		count++
	}
	return count
}

func aFieldSetNarrowedByName(subject reflect.Type) []string {
	kept := []string{}
	for at := range subject.NumField() {
		if !strings.HasSuffix(subject.Field(at).Name, "MLS") {
			continue
		}
		kept = append(kept, subject.Field(at).Name)
	}
	return kept
}

func aFieldSetNarrowedThroughABoundDescriptor(members []reflect.StructField) []reflect.StructField {
	kept := []reflect.StructField{}
	for _, entry := range members {
		if entry.Type.Kind() != reflect.Slice {
			continue
		}
		kept = append(kept, entry)
	}
	return kept
}

func aPredicateBoundToAName(subject reflect.Type) int {
	kept := 0
	for at := range subject.NumMethod() {
		member := subject.Method(at)
		drop := strings.HasPrefix(member.Name, "Gamma")
		if drop {
			continue
		}
		kept++
	}
	return kept
}

func aFieldSetNarrowedByATagTheDescriptorCarries(subject reflect.Type) []string {
	kept := []string{}
	for at := range subject.NumField() {
		if subject.Field(at).Tag.Get("json") == "" {
			continue
		}
		kept = append(kept, subject.Field(at).Name)
	}
	return kept
}

func aFieldSetNarrowedByTheDescriptorsPackagePath(members []reflect.StructField) int {
	kept := 0
	for _, entry := range members {
		if entry.PkgPath != "" {
			continue
		}
		kept++
	}
	return kept
}

func aFieldSetNarrowedByTheDescriptorsEmbeddingFlag(members []reflect.StructField) int {
	kept := 0
	for _, entry := range members {
		if entry.Anonymous {
			continue
		}
		kept++
	}
	return kept
}

func aMethodSetNarrowedByTheDescriptorsIndex(members []reflect.Method) int {
	kept := 0
	for _, entry := range members {
		if len(entry.Index) != 1 {
			continue
		}
		kept++
	}
	return kept
}

func aTagBoundToANameAndDecidedByItsOwnValue(subject reflect.Type) int {
	kept := 0
	for at := range subject.NumField() {
		tag := subject.Field(at).Tag
		if tag == "" {
			continue
		}
		kept++
	}
	return kept
}

func aTagCutOutOfTheDescriptorAndComparedTwoLinesDown(subject reflect.Type) []string {
	kept := []string{}
	for at := range subject.NumField() {
		key, _, _ := strings.Cut(subject.Field(at).Tag.Get("json"), ",")
		if key == "" {
			continue
		}
		kept = append(kept, key)
	}
	return kept
}

func aPredicateSpelledAsAFunctionLiteral(members []reflect.Method) bool {
	return slices.ContainsFunc(members, func(one reflect.Method) bool {
		return one.Name == "x"
	})
}

func aPredicateAnsweringABoolBesideAnError(members []reflect.Method) (bool, error) {
	for _, one := range members {
		return one.Name == "x", nil
	}
	return false, nil
}

func aMemberReachedThroughADeclaredValue(bound reflect.Value) int {
	if bound.Type().NumIn() != 1 {
		return 0
	}
	return 1
}

type controlFlag bool

type controlPair struct {
	member reflect.Method
}

func aMemberHeldInAStructField(pair controlPair) int {
	if strings.HasPrefix(pair.member.Name, "Write") {
		return 1
	}
	return 0
}

func aPredicateAnsweringANamedBool(members []reflect.Method) controlFlag {
	for _, one := range members {
		return controlFlag(one.Name == "x")
	}
	return false
}

// THE NINTH INSTANCE, DRIVEN AND NOT FIXED. Everything below narrows a reflected member set and
// none of it is seen, and the two families are the two lists this derivation still bottoms out
// in. It is recorded in GATES.md and asserted here rather than closed, because closing it is a
// round and the line was stopped on evidence.
//
// FAMILY ONE, the CONTAINER SHAPES. A member is read bare or in a slice of descriptors and in
// no other container, so an iterator door, a pointer, a map, a named slice type and a variadic
// all fall out.
// On Go 1.26 the first two functions here are real doors of the reflect this derivation parses:
// Type.Methods() answers iter.Seq[Method] and Type.Fields() answers iter.Seq[StructField].
func aMemberSetReachedThroughAnIteratorDoor(subject reflect.Type) []string {
	kept := []string{}
	for member := range subject.Methods() {
		if strings.HasPrefix(member.Name, "Write") {
			continue
		}
		kept = append(kept, member.Name)
	}
	return kept
}

func aFieldSetReachedThroughAnIteratorDoor(subject reflect.Type) []string {
	kept := []string{}
	for field := range subject.Fields() {
		if field.Anonymous {
			continue
		}
		kept = append(kept, field.Name)
	}
	return kept
}

func aMemberDeclaredBehindAPointer(member *reflect.Method) bool {
	return strings.HasPrefix(member.Name, "Write")
}

func aMemberDeclaredInAMap(members map[string]reflect.Method) int {
	kept := 0
	for _, member := range members {
		if strings.HasPrefix(member.Name, "Write") {
			continue
		}
		kept += 1
	}
	return kept
}

type controlMembers []reflect.Method

func aMemberDeclaredInANamedSliceType(members controlMembers) int {
	kept := 0
	for _, member := range members {
		if strings.HasPrefix(member.Name, "Write") {
			continue
		}
		kept += 1
	}
	return kept
}

func aMemberDeclaredVariadic(members ...reflect.Method) int {
	kept := 0
	for _, member := range members {
		if strings.HasPrefix(member.Name, "Write") {
			continue
		}
		kept += 1
	}
	return kept
}

// FAMILY TWO, the STATEMENT FORMS. A member is bound through an assignment, a range, a value
// specification or a field declaration and through nothing else, so a type assertion and a type
// switch bind nothing at all. Every one of these spells the descriptor outright, exactly as a
// parameter does; none of them wants a type.
func aMemberBoundByATypeAssertion(carried any) bool {
	member := carried.(reflect.Method)
	return strings.HasPrefix(member.Name, "Write")
}

func aMemberBoundByACommaOkTypeAssertion(carried any) bool {
	if member, ok := carried.(reflect.StructField); ok && member.Anonymous {
		return true
	}
	return false
}

func aMemberBoundByATypeSwitch(carried any) bool {
	switch member := carried.(type) {
	case reflect.StructField:
		if member.Offset == 0 {
			return false
		}
		return true
	}
	return false
}

func anAccumulatorOfMemberNamesIsNotAPredicate(members []reflect.Method) bool {
	kept := []string{}
	for _, member := range members {
		kept = append(kept, member.Name)
	}
	if len(kept) == 0 {
		return false
	}
	return true
}
`

// TestTheGatesDerivationSeesANarrowingHoweverItsReceiverIsSpelled drives the derivation over a
// control holding one of each shape it claims to read, and two it claims not to.
//
// This is the test that makes the gate above worth its universal claim. A derivation that
// reported nothing would agree with any document that indexed nothing; the bijection catches
// that only because the document is non-empty, and that is a property of today's document
// rather than of this gate. So the recognisers are driven individually, on source written for
// the purpose, with the receiver deliberately spelled four different ways.
func TestTheGatesDerivationSeesANarrowingHoweverItsReceiverIsSpelled(t *testing.T) {
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, "control.go", gatesControlSource, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse the control: %v", err)
	}
	answers := map[string]bool{
		"narrowedInAnotherFunction":                true,
		"throughABoundSignature":                   true,
		"aFieldSetNarrowedThroughABoundDescriptor": true,
	}
	undecided := map[string]bool{}
	found := gatesNarrowingsIn(fileSet, parsed, "control.go", answers, gatesDoorsOf(t), undecided)
	seen := map[string]string{}
	sites := map[string]int{}
	for _, narrowing := range found {
		seen[narrowing.fn] = narrowing.kind
		sites[narrowing.fn] += 1
	}
	for function, kind := range map[string]string{
		"receiverSpelledAnythingAtAll": "name",
		"narrowedInAnotherFunction":    "shape",
		"throughABoundSignature":       "shape",
		// the seventh instance, driven: the identical narrowing one reflect door over. Under
		// the reading this file carried for six rounds every one of these three was invisible,
		// and a planted one shipped green through all four gates here.
		"aFieldSetNarrowedByName":                  "name",
		"aFieldSetNarrowedThroughABoundDescriptor": "shape",
		// and the two spellings of a predicate that is not a condition
		"aPredicateBoundToAName":              "name",
		"aPredicateSpelledAsAFunctionLiteral": "name",
		// and one whose decision sits BESIDE another result. "answers exactly one bool" was an
		// arity narrowing over the predicate class, which is the shape this file's own document
		// calls always suspect; its complement was empty and unprinted, so nothing would have
		// gone red on the day the first (bool, error) predicate landed.
		"aPredicateAnsweringABoolBesideAnError": "name",
		// THE EIGHTH INSTANCE, driven: the identical narrowing one DESCRIPTOR FIELD over. Under
		// the reading this file carried for seven rounds every one of these five was invisible,
		// four of them were planted and shipped green through all five gates, and three live
		// sites in mls are spelled like the last of them. None of the five wants a type: every
		// one is a selector the parse tree already holds, which is why the boundary paragraph
		// that offered go/types as the remedy for what this file cannot see was wrong.
		"aFieldSetNarrowedByATagTheDescriptorCarries":      "attribute",
		"aFieldSetNarrowedByTheDescriptorsPackagePath":     "attribute",
		"aFieldSetNarrowedByTheDescriptorsEmbeddingFlag":   "attribute",
		"aMethodSetNarrowedByTheDescriptorsIndex":          "attribute",
		"aTagCutOutOfTheDescriptorAndComparedTwoLinesDown": "attribute",
		// and one BOUND to a name first, which is how a descriptor's own field reaches a
		// condition that mentions no member: the binding half of gatesIsMemberAttribute has
		// nothing in these trees spelled this way today, so without this the recogniser would
		// be machinery nothing observes
		"aTagBoundToANameAndDecidedByItsOwnValue": "attribute",
	} {
		got, sawIt := seen[function]
		if !sawIt {
			t.Errorf("the derivation does not see the narrowing in %s; a class it cannot see is a class the document need not index, which is the whole of the defect this file closes",
				function)
			continue
		}
		if got != kind {
			t.Errorf("the derivation reads %s as %s and it is %s", function, got, kind)
		}
	}
	// and the ones it must NOT see. Two are noise a derivation that reports a third more sites
	// than exist would be read as; two are THE STATED BOUNDARY OF THIS DERIVATION, driven here
	// rather than asserted in the comment at the top of the file. A boundary nothing exercises
	// is a boundary nobody measured -- which is how the list at the top of this file came to be
	// incomplete for a round while reading as complete, and it is GATES.md's own Q3 pointed at
	// this file: name the derivation that produced the rows, or the row you forgot is invisible.
	for function, reads := range map[string]string{
		"aFieldSpelledNameIsNotAMember":             "struct field spelled name",
		"aDirectoryEntryIsNotAMember":               "directory entry name",
		"anAccumulatorOfMemberNamesIsNotAPredicate": "slice of member names consumed by len() and deciding nothing about a member",
		// and THE THREE UNDER-REACHES this file states, driven rather than asserted in a comment.
		// Every one of them needs the same thing to close: a TYPE, which a syntactic reading of
		// one function at a time does not have.
		"aMemberReachedThroughADeclaredValue": "signature read off a parameter declared reflect.Value; nothing in the source says that value came off a member set",
		"aMemberHeldInAStructField":           "member name read through a struct field whose type is declared in another declaration, so no statement in this function binds it",
		"aPredicateAnsweringANamedBool":       "predicate answering a DEFINED type whose underlying type is bool, which a spelling comparison cannot tell from any other named type",
		// AND THE NINTH, DRIVEN HERE AND NOT CLOSED. Two lists, one per half of how a member is
		// reached. FAMILY ONE is the CONTAINER SHAPES: gatesAnswers and gatesDoorSet.declares
		// read a descriptor bare or in a slice and in no other container, so on Go 1.26 the two
		// ITERATOR doors of the reflect this derivation parses -- Type.Methods() answering
		// iter.Seq[Method] and Type.Fields() answering iter.Seq[StructField] -- are not doors
		// here, and both spellings sit in the printed notDoors list. The same two-entry list
		// removes a pointer, a map, a named slice type and a variadic. FAMILY TWO is the
		// STATEMENT FORMS: gatesGather binds a member through an assignment, a range, a value
		// specification and a field declaration, so a type assertion and a type switch bind
		// nothing. NONE OF THESE WANTS A TYPE -- the descriptor is spelled in the parameter, in
		// the assertion or in the case clause -- which is why go/types is not the remedy here
		// either, and it is the third round running that the remedy named for what this cannot
		// see would have found none of the next instance.
		//
		// These entries are a BOUNDARY, not a wish: the day a round closes the ninth they go
		// red, and the correct response is to move them to the list above rather than to delete
		// them. A control that starts failing as a class widens is the control saying so.
		"aMemberSetReachedThroughAnIteratorDoor": "name narrowing over a member set reached through an iterator door, which the two-entry container list `X` and `[]X` removes",
		"aFieldSetReachedThroughAnIteratorDoor":  "attribute narrowing over a field set reached through an iterator door, removed by the same two-entry list",
		"aMemberDeclaredBehindAPointer":          "name narrowing over a member declared *reflect.Method, which the reflect half does not strip although the go/types half strips exactly that",
		"aMemberDeclaredInAMap":                  "name narrowing over members held in a map of descriptors",
		"aMemberDeclaredInANamedSliceType":       "name narrowing over members held in a DEFINED slice type, whose spelling is the type name and not []reflect.Method",
		"aMemberDeclaredVariadic":                "name narrowing over members taken variadically",
		"aMemberBoundByATypeAssertion":           "name narrowing over a member bound by a type assertion, which no statement form gatesGather reads binds",
		"aMemberBoundByACommaOkTypeAssertion":    "attribute narrowing over a member bound by a comma-ok type assertion",
		"aMemberBoundByATypeSwitch":              "attribute narrowing over a member bound by a type switch, whose case clause spells the descriptor outright",
	} {
		if kind, sawIt := seen[function]; sawIt {
			t.Errorf("the derivation reports %s as a %s narrowing over a reflected member set and it reads a %s",
				function, kind, reads)
		}
	}
	// AND THE COUNT, not only the presence, in the one function that pairs a member's name with
	// a struct field spelled the same way. Reading an expression with a generic tree walk visits
	// the Sel of a selector as though it were an identifier, so `entry.name` reads as a member
	// name wherever `name` is bound to one anywhere in the same function -- which measured 63
	// sites where there are 52. Presence alone cannot see that: the real narrowing above it is
	// still found, so every assertion in this test went on passing while the derivation reported
	// a third more sites than exist. A class that over-reports by a third stops being read.
	// AND THE CLASSIFICATION, not only the kind. An identifier bound to a fact the descriptor
	// carries IS that fact, the way an identifier bound to a member's name is that name. Read
	// instead as a PREDICATE over one, the site is still reported and still reads "attribute" --
	// so every assertion above goes on passing -- but the binding is rendered into the row's KEY
	// as a `where` clause, and the document is keyed on that text. A row keyed one way certifies
	// nothing about the same narrowing keyed the other.
	for _, narrowing := range found {
		if narrowing.fn != "aTagBoundToANameAndDecidedByItsOwnValue" {
			continue
		}
		if narrowing.cond != `tag == ""` {
			t.Errorf("the derivation renders %s's narrowing as %q and it is `tag == \"\"`; an identifier bound to a descriptor's own field is that field, and reading it as a predicate over one keys the row to the binding as well",
				narrowing.fn, narrowing.cond)
		}
	}
	if got := sites["aMemberNameAndAFieldOfTheSameSpelling"]; got != 1 {
		t.Errorf("the derivation reads %d narrowings in aMemberNameAndAFieldOfTheSameSpelling and there is one: the HasPrefix over a member's name. A struct field spelled name is not a member of a reflected method set, and counting it makes this class noise",
			got)
	}
	t.Logf("the control's narrowings were read as %v, with %v sites each", seen, sites)
}
