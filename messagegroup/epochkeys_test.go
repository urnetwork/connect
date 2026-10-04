// The two keys of one epoch: the value that carries them, and the session door that opens onto it.
//
// WHAT IS OBSERVED THROUGH WHAT, stated once here because four of the nine cases below turn on it.
// An ALIAS defect and an ERASE are observable only through a second header over the array in
// question, never through the far side's own answer: a case that asked a destroyed value for its
// key would be reading a refusal, and a case that asked a live value whether it had copied its
// input would be comparing the value against itself. So the instruments are (a) the caller's own
// array, mutated after the constructor was handed it, and (b) the header an accessor answered,
// taken BEFORE the destroy that is being observed. Both are zeroize_test.go's shape and neither is
// new work.
//
// The session half is observed through newTestSession over a REAL mls group, because "the key the
// session held" has no referent otherwise, and the comparison is against message.WriteKey and
// message.ReadKey re-derived in the test from the same handle's Export and the same injected
// pq_secret -- never against the session's own fields, which is the comparison that would pass
// under a door that copied the wrong field of the right type.
package messagegroup

import (
	"bytes"
	"errors"
	"go/ast"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect/message"
)

// The width MASTER section 8.3 gives write_key and read_key on the wire, transcribed rather than
// read off the package so a truncation is a disagreement with the specification.
const epochKeysWidthFromMaster = 32

// testEpochKeyOctets is one distinct thirty two octet run per seed.
//
// The two the cases below use are far enough apart that a mix-up is not a near miss: an accessor
// answering the wrong field is reported as the wrong field by name rather than as "32 octets
// disagreed".
func testEpochKeyOctets(seed byte) []byte {
	key := make([]byte, epochKeysWidthFromMaster)
	for i := range key {
		key[i] = seed + byte(i)
	}
	return key
}

// The two runs every case in this file builds a value out of.
func testEpochKeysReadOctets() []byte  { return testEpochKeyOctets(0x10) }
func testEpochKeysWriteOctets() []byte { return testEpochKeyOctets(0x80) }

// mustNotPanic runs one call and turns a panic into a named failure rather than a dead binary.
//
// It exists for the destructor cases: a panic out of Destroy takes the whole test process down,
// and what a reader needs is which of the four calls did it.
func mustNotPanic(t *testing.T, what string, call func()) {
	t.Helper()
	defer func() {
		if panicked := recover(); panicked != nil {
			t.Errorf("%s panicked with %v; a destructor is the one method a caller writes in a defer above the construction it destroys, so a panic here lands on the cleanup path of the failure it was cleaning up",
				what, panicked)
		}
	}()
	call()
}

// ---------------------------------------------------------------------------
// Task 1 Property 1: three values, and each is the one that went in
// ---------------------------------------------------------------------------

func TestAnEpochKeysAnswersTheThreeValuesItWasBuiltWith(t *testing.T) {
	readOctets := testEpochKeysReadOctets()
	writeOctets := testEpochKeysWriteOctets()
	// the two must differ, or every clause below passes against a value that answered one key
	// twice.
	if bytes.Equal(readOctets, writeOctets) {
		t.Fatal("the fixture's two keys are the same octets, so no case in this file can tell an accessor answering the wrong one")
	}
	keys := newEpochKeys(7, readOctets, writeOctets)
	defer keys.Destroy()

	epoch, err := keys.Epoch()
	if err != nil {
		t.Fatalf("Epoch of a live value: %v", err)
	}
	if epoch != 7 {
		t.Errorf("Epoch answered %d, want 7", epoch)
	}

	gotRead, err := keys.ReadKey()
	if err != nil {
		t.Fatalf("ReadKey of a live value: %v", err)
	}
	if !bytes.Equal(gotRead, readOctets) {
		if bytes.Equal(gotRead, writeOctets) {
			t.Errorf("ReadKey answered the WRITE key: read_key is macced over req_auth and write_key over write_auth, so a member that swapped them authenticates every fetch under the key the server holds itself")
		} else {
			t.Errorf("ReadKey answered %x, want %x", gotRead, readOctets)
		}
	}
	if len(gotRead) != epochKeysWidthFromMaster {
		t.Errorf("ReadKey answered %d octets, and MASTER section 8.3 fixes read_key at %d",
			len(gotRead), epochKeysWidthFromMaster)
	}

	gotWrite, err := keys.WriteKey()
	if err != nil {
		t.Fatalf("WriteKey of a live value: %v", err)
	}
	if !bytes.Equal(gotWrite, writeOctets) {
		if bytes.Equal(gotWrite, readOctets) {
			t.Errorf("WriteKey answered the READ key: a record submitted under read_key is refused by the server, and a fetch macced under write_key is macced under a key the server can forge")
		} else {
			t.Errorf("WriteKey answered %x, want %x", gotWrite, writeOctets)
		}
	}
	if len(gotWrite) != epochKeysWidthFromMaster {
		t.Errorf("WriteKey answered %d octets, and MASTER section 8.3 fixes write_key at %d",
			len(gotWrite), epochKeysWidthFromMaster)
	}
}

// ---------------------------------------------------------------------------
// Task 1 Property 2: every accessor refuses, destroyed AND zero valued
// ---------------------------------------------------------------------------

// epochKeysAccessorCalls invokes each member of the class Property 2 is over, and says whether the
// call handed anything out beside its answer.
//
// The keys of this map are checked in BOTH DIRECTIONS against the class derived off epochkeys.go's
// syntax tree, so an accessor added without a row here fails rather than going unchecked, and a row
// for a method that no longer exists fails too.
//
// door is the phrase the refusal owes: a caller reading a log has to know which door it tried, and
// "an accessor was shut" would leave an implementer guessing which.
var epochKeysAccessorCalls = map[string]struct {
	door string
	call func(*EpochKeys) (handedOutSomething bool, err error)
}{
	"Epoch": {
		door: "the epoch",
		call: func(keys *EpochKeys) (bool, error) {
			epoch, err := keys.Epoch()
			return epoch != 0, err
		},
	},
	"ReadKey": {
		door: "read_key",
		call: func(keys *EpochKeys) (bool, error) {
			key, err := keys.ReadKey()
			return 0 < len(key), err
		},
	},
	"WriteKey": {
		door: "write_key",
		call: func(keys *EpochKeys) (bool, error) {
			key, err := keys.WriteKey()
			return 0 < len(key), err
		},
	},
}

// epochKeysMethodClasses reads epochkeys.go for the exported method set of *EpochKeys and splits it
// in two: the members that ANSWER a value, which is the class Property 2 is over, and the
// complement, which answer nothing at all.
//
// The split is off the SIGNATURE rather than off a list of names, so a fourth accessor joins the
// class by being written.
func epochKeysMethodClasses(t *testing.T) (class []string, complement []string) {
	t.Helper()
	_, sources := messagegroupProductionSources(t)
	read := false
	for _, source := range sources {
		if source.path != "epochkeys.go" {
			continue
		}
		read = true
		for _, declaration := range source.parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || sessionReceiverName(function) != "EpochKeys" {
				continue
			}
			if !function.Name.IsExported() {
				continue
			}
			if function.Type.Results != nil && 0 < len(function.Type.Results.List) {
				class = append(class, function.Name.Name)
				continue
			}
			complement = append(complement, function.Name.Name)
		}
	}
	if !read {
		t.Fatal("epochkeys.go was not read out of this package's production source, so this gate derived its class from nothing")
	}
	if len(class) == 0 {
		t.Fatal("no exported method of EpochKeys answers a value, so either the type has no accessors or this reading is not finding them")
	}
	slices.Sort(class)
	slices.Sort(complement)
	return class, complement
}

func TestEveryAccessorOfAnEpochKeysRefusesWhenDestroyedAndWhenZeroValued(t *testing.T) {
	class, complement := epochKeysMethodClasses(t)
	// THE NARROWING PRINTS WHAT IT REMOVED. "Every exported method" is narrowed to "every one
	// that answers a value", and the complement is asserted member by member rather than merely
	// non-empty: a complement asserted non-empty is satisfied by one member when there are two.
	t.Logf("the accessors of EpochKeys are %v; the complement -- every exported method that answers nothing -- is %v",
		class, complement)
	if want := []string{"Destroy"}; !slices.Equal(complement, want) {
		t.Fatalf("the exported methods of EpochKeys that answer nothing are %v, want %v; a second one is either a mutator on a value that has none or an accessor whose refusal this case never runs",
			complement, want)
	}
	if invoked := slices.Sorted(maps.Keys(epochKeysAccessorCalls)); !slices.Equal(class, invoked) {
		t.Fatalf("epochkeys.go declares the accessors %v and this case invokes %v; the two are checked in both directions so an accessor added without a row here is a door nothing tries",
			class, invoked)
	}

	destroyed := newEpochKeys(9, testEpochKeysReadOctets(), testEpochKeysWriteOctets())
	destroyed.Destroy()
	subjects := []struct {
		what string
		keys *EpochKeys
	}{
		// BOTH ARMS ARE THE PROPERTY. A case that ran only the first would leave the zero value
		// answering a nil key and no error, which is the exact shape ErrProvisionalEpochDestroyed's
		// own doc comment names one type over.
		{what: "a destroyed EpochKeys", keys: destroyed},
		{what: "a zero valued EpochKeys", keys: &EpochKeys{}},
		// THE THIRD SUBJECT IS WHAT MAKES THE LAST CLAUSE BELOW MEASURE ANYTHING. On the
		// other two the octets are already gone, so "refused and handed nothing out" is
		// satisfied by there being nothing to hand out -- measured: an accessor rewritten to
		// return self.writeKey BESIDE its refusal passes both of them. This one is a shut door
		// with the octets still behind it, which is the state a destructor that set the flag
		// and skipped the erase leaves, and it is the only subject that can tell an accessor
		// that reads the door from one that reports it.
		{what: "a shut door with its octets still behind it", keys: &EpochKeys{
			built:     true,
			destroyed: true,
			epoch:     9,
			readKey:   testEpochKeysReadOctets(),
			writeKey:  testEpochKeysWriteOctets(),
		}},
	}
	for _, member := range class {
		accessor := epochKeysAccessorCalls[member]
		for _, subject := range subjects {
			handedOut, err := accessor.call(subject.keys)
			if !errors.Is(err, ErrEpochKeysDestroyed) {
				t.Errorf("%s of %s answered %v, want ErrEpochKeysDestroyed", member, subject.what, err)
				continue
			}
			if !strings.Contains(err.Error(), accessor.door) {
				t.Errorf("%s of %s refused with %q and never names the door %q it was asked for; a caller reading this log cannot tell which of three it tried",
					member, subject.what, err.Error(), accessor.door)
			}
			if handedOut {
				t.Errorf("%s of %s refused AND handed out its value; a caller that reads the answer before the error is a caller macing under a key nothing authenticated",
					member, subject.what)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Task 1 Property 3: the destructor is idempotent and safe on the zero value
// ---------------------------------------------------------------------------

func TestDestroyingAnEpochKeysIsIdempotentAndSafeOnTheZeroValue(t *testing.T) {
	keys := newEpochKeys(3, testEpochKeysReadOctets(), testEpochKeysWriteOctets())
	mustNotPanic(t, "the first Destroy of a live value", keys.Destroy)
	mustNotPanic(t, "a second Destroy of the same value", keys.Destroy)
	zero := &EpochKeys{}
	mustNotPanic(t, "Destroy of the zero value", zero.Destroy)
	mustNotPanic(t, "a second Destroy of the zero value", zero.Destroy)

	// AND BOTH ARE STILL REFUSING AFTERWARDS. Without this the second call is free to leave a
	// value that has forgotten it was destroyed, which is idempotent by the narrow reading and is
	// a live key by every other one.
	class, _ := epochKeysMethodClasses(t)
	for _, member := range class {
		accessor, isInvoked := epochKeysAccessorCalls[member]
		if !isInvoked {
			// the both-directions check one case up is what this belongs to; here it is a
			// refusal to run rather than a nil call, because a gate that panics reports the
			// crash and not the missing row.
			t.Errorf("epochkeys.go declares the accessor %s and epochKeysAccessorCalls has no row for it, so this case cannot ask it anything",
				member)
			continue
		}
		for _, subject := range []struct {
			what string
			keys *EpochKeys
		}{
			{what: "a twice destroyed value", keys: keys},
			{what: "a twice destroyed zero value", keys: zero},
		} {
			if _, err := accessor.call(subject.keys); !errors.Is(err, ErrEpochKeysDestroyed) {
				t.Errorf("%s of %s answered %v after two Destroys, want ErrEpochKeysDestroyed",
					member, subject.what, err)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Task 1 Property 4: the octets are erased, on the array and not on the header
// ---------------------------------------------------------------------------

// The subject is the ARRAY. A body that set the two fields to nil and nothing else leaves sixty
// four octets of key material on the heap under headers the caller is still holding -- the ones
// every accessor already answered -- and the accessors afterwards report a refusal either way.
func TestDestroyingAnEpochKeysErasesTheOctetsOnTheBackingArray(t *testing.T) {
	keys := newEpochKeys(5, testEpochKeysReadOctets(), testEpochKeysWriteOctets())
	// the second headers, taken BEFORE the destroy, which is the only order that observes an
	// erase rather than a door.
	readAlias, err := keys.ReadKey()
	if err != nil {
		t.Fatalf("ReadKey of a live value: %v", err)
	}
	writeAlias, err := keys.WriteKey()
	if err != nil {
		t.Fatalf("WriteKey of a live value: %v", err)
	}
	for _, held := range []struct {
		what   string
		octets []byte
	}{{what: "read_key", octets: readAlias}, {what: "write_key", octets: writeAlias}} {
		if len(held.octets) != epochKeysWidthFromMaster {
			t.Fatalf("the header this case holds over %s is %d octets, so it is not over the array the erase has to reach",
				held.what, len(held.octets))
		}
		if !slices.ContainsFunc(held.octets, func(octet byte) bool { return octet != 0 }) {
			t.Fatalf("the header this case holds over %s is already all zeros before Destroy, so this case would pass over a value that erased nothing",
				held.what)
		}
	}

	keys.Destroy()

	for _, held := range []struct {
		what   string
		octets []byte
	}{{what: "read_key", octets: readAlias}, {what: "write_key", octets: writeAlias}} {
		for i, octet := range held.octets {
			if octet != 0 {
				t.Errorf("%s is not erased after Destroy: octet %d is %#02x and the whole array reads %x. Dropping the field is not erasing the octets -- the caller still holds this header and so does the collector",
					held.what, i, octet, held.octets)
				break
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Task 1 Property 5: the value holds no alias of anything it did not make
// ---------------------------------------------------------------------------

// The behavioural half, observed through the CALLER'S array: it is mutated after the constructor
// was handed it, and the value is asked what it holds. A case that compared the value's answer
// against the value's own field would be comparing a photograph with itself.
func TestAnEpochKeysCopiesEveryArrayItIsHanded(t *testing.T) {
	readOctets := testEpochKeysReadOctets()
	writeOctets := testEpochKeysWriteOctets()
	keys := newEpochKeys(1, readOctets, writeOctets)
	defer keys.Destroy()
	for i := range readOctets {
		readOctets[i] = 0xEE
	}
	for i := range writeOctets {
		writeOctets[i] = 0xEE
	}
	for _, held := range []struct {
		what     string
		read     func() ([]byte, error)
		caller   []byte
		original []byte
	}{
		{what: "read_key", read: keys.ReadKey, caller: readOctets, original: testEpochKeysReadOctets()},
		{what: "write_key", read: keys.WriteKey, caller: writeOctets, original: testEpochKeysWriteOctets()},
	} {
		got, err := held.read()
		if err != nil {
			t.Fatalf("%s of a live value: %v", held.what, err)
		}
		if bytes.Equal(got, held.caller) {
			t.Errorf("%s moved when the caller's array moved, so the value holds the caller's header rather than a copy of it; the one caller is the session loop, and the array it passes is the one the next AdvanceEpoch zeroizes in place",
				held.what)
			continue
		}
		if !bytes.Equal(got, held.original) {
			t.Errorf("%s is %x and the octets the constructor was handed were %x", held.what, got, held.original)
		}
	}
}

// epochKeysArrayFields splits the fields of the EpochKeys struct in two: the ones that hold an
// ARRAY, which is the class Property 5 is over, and the complement, which hold no octets and
// cannot alias anything.
//
// It is read off the struct declaration rather than listed, so a third key added to the type joins
// the class by being declared.
func epochKeysArrayFields(t *testing.T) (class []string, complement []string) {
	t.Helper()
	_, sources := messagegroupProductionSources(t)
	read := false
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			general, isGeneral := declaration.(*ast.GenDecl)
			if !isGeneral {
				continue
			}
			for _, spec := range general.Specs {
				typeSpec, isType := spec.(*ast.TypeSpec)
				if !isType || typeSpec.Name.Name != "EpochKeys" {
					continue
				}
				structure, isStruct := typeSpec.Type.(*ast.StructType)
				if !isStruct {
					t.Fatal("EpochKeys is not a struct, so this reading has nothing to split")
				}
				read = true
				for _, field := range structure.Fields.List {
					array, isArray := field.Type.(*ast.ArrayType)
					holdsOctets := isArray && array.Len == nil
					for _, name := range field.Names {
						if holdsOctets {
							class = append(class, name.Name)
							continue
						}
						complement = append(complement, name.Name)
					}
				}
			}
		}
	}
	if !read {
		t.Fatal("no EpochKeys struct was read out of this package's production source, so this gate examined nothing")
	}
	if len(class) == 0 {
		t.Fatal("EpochKeys declares no slice field, so either it holds no key material or this reading is not finding it")
	}
	slices.Sort(class)
	slices.Sort(complement)
	return class, complement
}

// epochKeysAssignment is one assignment into a field of an EpochKeys whose value comes from a
// parameter of the enclosing declaration, and whether it went through a copy.
type epochKeysAssignment struct {
	field  string
	file   string
	copied bool
}

// epochKeysFieldAssignmentsIn reads one parsed file for every place a field of an EpochKeys is
// given a value that comes from a parameter, in both shapes a constructor can be written in: a
// composite literal of the type, and an assignment through a *EpochKeys receiver.
//
// "Comes from a parameter" is the only reading that matters here, because a parameter is the one
// value the declaration did not make: everything else it assigns, it derived.
func epochKeysFieldAssignmentsIn(source messagegroupSource, class []string) []epochKeysAssignment {
	found := []epochKeysAssignment{}
	for _, declaration := range source.parsed.Decls {
		function, isFunction := declaration.(*ast.FuncDecl)
		if !isFunction || function.Body == nil {
			continue
		}
		parameters := map[string]bool{}
		if function.Type.Params != nil {
			for _, parameter := range function.Type.Params.List {
				for _, name := range parameter.Names {
					parameters[name.Name] = true
				}
			}
		}
		if len(parameters) == 0 {
			continue
		}
		receiverIsTheType := sessionReceiverName(function) == "EpochKeys"
		ast.Inspect(function.Body, func(node ast.Node) bool {
			switch shape := node.(type) {
			case *ast.CompositeLit:
				named, isNamed := shape.Type.(*ast.Ident)
				if !isNamed || named.Name != "EpochKeys" {
					return true
				}
				for _, element := range shape.Elts {
					pair, isPair := element.(*ast.KeyValueExpr)
					if !isPair {
						continue
					}
					key, isIdentifier := pair.Key.(*ast.Ident)
					if !isIdentifier {
						continue
					}
					if !slices.Contains(class, key.Name) {
						continue
					}
					if assignment, reaches := epochKeysAssignmentOf(key.Name, source.path,
						pair.Value, parameters); reaches {
						found = append(found, assignment)
					}
				}
			case *ast.AssignStmt:
				if !receiverIsTheType {
					return true
				}
				for i, target := range shape.Lhs {
					selector, isSelector := target.(*ast.SelectorExpr)
					if !isSelector || i >= len(shape.Rhs) {
						continue
					}
					if receiver, isIdentifier := selector.X.(*ast.Ident); !isIdentifier || receiver.Name != "self" {
						continue
					}
					if !slices.Contains(class, selector.Sel.Name) {
						continue
					}
					if assignment, reaches := epochKeysAssignmentOf(selector.Sel.Name, source.path,
						shape.Rhs[i], parameters); reaches {
						found = append(found, assignment)
					}
				}
			}
			return true
		})
	}
	return found
}

// epochKeysAssignmentOf classifies one assigned value: whether it reaches a parameter at all, and
// if it does, whether a copying call stands between the parameter and the field.
//
// The copy constructions are named rather than inferred, because a call this reading does not
// recognise must read as an ALIAS: failing safe here means a new spelling of "copy" is a failure
// somebody has to come and look at, and the alternative fails open on the one defect the whole
// case exists for.
func epochKeysAssignmentOf(field string, file string, value ast.Expr,
	parameters map[string]bool) (epochKeysAssignment, bool) {

	reachesParameter := false
	ast.Inspect(value, func(node ast.Node) bool {
		if identifier, isIdentifier := node.(*ast.Ident); isIdentifier && parameters[identifier.Name] {
			reachesParameter = true
		}
		return true
	})
	if !reachesParameter {
		return epochKeysAssignment{}, false
	}
	copied := false
	if call, isCall := value.(*ast.CallExpr); isCall {
		switch callee := call.Fun.(type) {
		case *ast.Ident:
			copied = callee.Name == "append"
		case *ast.SelectorExpr:
			copied = callee.Sel.Name == "Clone"
		}
	}
	return epochKeysAssignment{field: field, file: file, copied: copied}, true
}

// The source half of Property 5, and its SCOPE IS DERIVED: every production file of this package
// that can build an EpochKeys at all, read off the syntax tree rather than named.
//
// It is stated over the construction sites rather than over one file because Task 2 adds the
// second one. At Task 1 the caller did not exist and the scope was epochkeys.go alone; on this
// commit session.go joins it, and a third site -- which is where an alias could re-enter without
// either half of this case noticing -- fails here by name.
func TestNoProductionSiteBuildsAnEpochKeysOutOfSomebodyElsesArray(t *testing.T) {
	_, sources := messagegroupProductionSources(t)
	arrayFields, otherFields := epochKeysArrayFields(t)
	// the first narrowing of this case, and it prints what it removed: the fields that hold no
	// octets cannot alias anything, and naming them is what says the class below is the whole of
	// what can.
	t.Logf("the array fields of EpochKeys are %v; the complement -- fields that hold no octets -- is %v",
		arrayFields, otherFields)
	if want := []string{"readKey", "writeKey"}; !slices.Equal(arrayFields, want) {
		t.Errorf("EpochKeys holds the arrays %v, want %v; a third one is key material with no clause of this case over it",
			arrayFields, want)
	}
	if len(otherFields) == 0 {
		t.Fatal("every field of EpochKeys reads as an array, so this split is not telling the two shapes apart")
	}
	sites := []string{}
	elsewhere := []string{}
	assignments := []epochKeysAssignment{}
	for _, source := range sources {
		builds := false
		ast.Inspect(source.parsed, func(node ast.Node) bool {
			switch shape := node.(type) {
			case *ast.CallExpr:
				if callee, isIdentifier := shape.Fun.(*ast.Ident); isIdentifier && callee.Name == "newEpochKeys" {
					builds = true
				}
			case *ast.CompositeLit:
				if named, isNamed := shape.Type.(*ast.Ident); isNamed && named.Name == "EpochKeys" {
					builds = true
				}
			}
			return true
		})
		if builds {
			sites = append(sites, source.path)
		} else {
			elsewhere = append(elsewhere, source.path)
		}
		assignments = append(assignments, epochKeysFieldAssignmentsIn(source, arrayFields)...)
	}
	slices.Sort(sites)
	slices.Sort(elsewhere)
	// the narrowing prints its complement, and the complement here is every production file that
	// cannot build one. An empty complement would mean the reading matched everything and is
	// telling nothing apart.
	t.Logf("%d production file(s) build an EpochKeys: %v; the complement is the %d that cannot: %v",
		len(sites), sites, len(elsewhere), elsewhere)
	if want := []string{"epochkeys.go", "session.go"}; !slices.Equal(sites, want) {
		t.Errorf("the production files that build an EpochKeys are %v, want %v; the type's whole promise is that its octets came out of one place, and a third builder is a value saying the session held octets no session held",
			sites, want)
	}
	if len(elsewhere) == 0 {
		t.Fatal("every production file of this package reads as a builder of an EpochKeys, so this reading is matching on something other than the construction")
	}

	aliased := map[string]string{}
	copied := map[string]string{}
	for _, assignment := range assignments {
		if assignment.copied {
			copied[assignment.field] = assignment.file
			continue
		}
		aliased[assignment.field] = assignment.file
	}
	// AND SO DOES THIS ONE. The class is every field of an EpochKeys assigned from a parameter;
	// the narrowing is "without a copy"; the complement is the fields that DID go through one, and
	// it is asserted member by member because an assertion that it is non-empty is satisfied by
	// one field when there are two.
	t.Logf("fields of an EpochKeys assigned from a parameter: %v through a copy, %v directly",
		slices.Sorted(maps.Keys(copied)), slices.Sorted(maps.Keys(aliased)))
	if got := slices.Sorted(maps.Keys(copied)); !slices.Equal(got, arrayFields) {
		t.Errorf("the array fields copied out of a parameter are %v and the arrays this type holds are %v; both keys are the loop goroutine's own arrays and neither may reach this type as a header",
			got, arrayFields)
	}
	for field, file := range aliased {
		t.Errorf("%s:%s is assigned a parameter with no copy between them, so an EpochKeys holds a header somebody else owns: the session zeroizes both of its own at every AdvanceEpoch and at Close, and a caller holding this value would watch its key turn into thirty two zeros",
			file, field)
	}
}

// ---------------------------------------------------------------------------
// Task 2 Property 1: the door answers the two keys of the epoch it is at
// ---------------------------------------------------------------------------

// epochKeysDerivedFor re-derives the epoch's two keys from the group's OWN exporter output and the
// injected pq_secret, which is the comparison the door has to meet.
//
// It is deliberately not a read of self.writeKey and self.readKey. A case that compared the door
// against the fields the door copies would pass under a door that copied the wrong field of the
// right type, and read_key had NO production reader at all before this commit, so a defect in
// message.ReadKey's argument has never been observable from this package.
func epochKeysDerivedFor(t *testing.T, fixture *testSession) (readKey []byte, writeKey []byte) {
	t.Helper()
	mlsSecret, err := fixture.handle.Export(mlsSecretLabel, nil, mlsSecretBytes)
	if err != nil {
		t.Fatalf("export the epoch's mls_secret: %v", err)
	}
	if len(mlsSecret) != mlsSecretBytes {
		t.Fatalf("the exporter answered %d octets, want %d", len(mlsSecret), mlsSecretBytes)
	}
	root := StorageRoot(mlsSecret, testPqSecret())
	return message.ReadKey(root), message.WriteKey(root)
}

func TestTheEpochKeysDoorAnswersTheTwoKeysOfTheEpochTheSessionIsAt(t *testing.T) {
	fixture := newTestSession(t, "epoch-keys-door")
	sessionEpoch, err := fixture.session.Epoch()
	if err != nil {
		t.Fatalf("Epoch: %v", err)
	}
	keys, err := fixture.session.EpochKeys()
	if err != nil {
		t.Fatalf("EpochKeys: %v", err)
	}
	defer keys.Destroy()

	epoch, err := keys.Epoch()
	if err != nil {
		t.Fatalf("Epoch of the value: %v", err)
	}
	if epoch != sessionEpoch {
		t.Errorf("the door answered epoch %d and the session is at %d; the number is what a caller compares before it spends a write key the server may already have rotated past",
			epoch, sessionEpoch)
	}

	wantRead, wantWrite := epochKeysDerivedFor(t, fixture)
	gotRead, err := keys.ReadKey()
	if err != nil {
		t.Fatalf("ReadKey of the value: %v", err)
	}
	gotWrite, err := keys.WriteKey()
	if err != nil {
		t.Fatalf("WriteKey of the value: %v", err)
	}
	if bytes.Equal(wantRead, wantWrite) {
		t.Fatal("the re-derivation produced the same octets for read_key and write_key, so no clause below can tell the two apart and writeauth.go's two labels have collapsed")
	}
	if !bytes.Equal(gotWrite, wantWrite) {
		if bytes.Equal(gotWrite, wantRead) {
			t.Errorf("the WRITE KEY clause: the door answered read_key where write_key belongs, so every record this session submits is macced under the wrong one of the two keys of the epoch")
		} else {
			t.Errorf("the WRITE KEY clause: the door answered %x and HKDF-Expand(storage_root, \"write/v1\", 32) over this group's exporter output is %x",
				gotWrite, wantWrite)
		}
	}
	if !bytes.Equal(gotRead, wantRead) {
		if bytes.Equal(gotRead, wantWrite) {
			t.Errorf("the READ KEY clause: the door answered write_key where read_key belongs, so every fetch this session makes is macced under a key the server holds and can forge")
		} else {
			t.Errorf("the READ KEY clause: the door answered %x and HKDF-Expand(storage_root, \"read/v1\", 32) over this group's exporter output is %x",
				gotRead, wantRead)
		}
	}
}

// ---------------------------------------------------------------------------
// Task 2 Property 2: a closed session refuses, with the sentinel every door uses
// ---------------------------------------------------------------------------

func TestAClosedSessionRefusesToOpenTheEpochKeysDoor(t *testing.T) {
	fixture := newTestSession(t, "epoch-keys-closed")
	if err := fixture.session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	keys, err := fixture.session.EpochKeys()
	if !errors.Is(err, ErrSessionClosed) {
		t.Errorf("the door on a closed session answered %v, want ErrSessionClosed; Close zeroizes both keys, so the alternative to this refusal is a value carrying sixty four zero octets that a caller macs under",
			err)
	}
	if keys != nil {
		t.Errorf("the door on a closed session refused AND answered a value")
		keys.Destroy()
	}
}

// ---------------------------------------------------------------------------
// Task 2 Property 3: the door reads the epoch it is called at, never a cached one
// ---------------------------------------------------------------------------

// BOTH CLAUSES ARE THE PROPERTY. The first catches a door that cached; the second catches a door
// that aliased, and it is the only place the alias defect is visible as a VALUE rather than as a
// source read -- after the advance the session's arrays have been zeroized in place, so an aliased
// door A answers thirty two zero octets and no error.
func TestTheEpochKeysDoorReadsTheEpochItIsCalledAtAndLeavesTheEarlierValueAlone(t *testing.T) {
	fixture := newTestSession(t, "epoch-keys-advance")
	doorA, err := fixture.session.EpochKeys()
	if err != nil {
		t.Fatalf("EpochKeys before the advance: %v", err)
	}
	defer doorA.Destroy()
	epochA, err := doorA.Epoch()
	if err != nil {
		t.Fatalf("Epoch of door A: %v", err)
	}
	readA, err := doorA.ReadKey()
	if err != nil {
		t.Fatalf("ReadKey of door A: %v", err)
	}
	writeA, err := doorA.WriteKey()
	if err != nil {
		t.Fatalf("WriteKey of door A: %v", err)
	}
	// the copies this case compares against afterwards. They are taken now because the headers
	// above are the value's own arrays, and a case that compared a header against itself would
	// hold whatever the value holds.
	heldReadA := append([]byte(nil), readA...)
	heldWriteA := append([]byte(nil), writeA...)

	if _, _, _, err := fixture.handle.Commit(nil); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := fixture.handle.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	if err := fixture.session.AdvanceEpoch(testPqSecret()); err != nil {
		t.Fatalf("AdvanceEpoch: %v", err)
	}

	doorB, err := fixture.session.EpochKeys()
	if err != nil {
		t.Fatalf("EpochKeys after the advance: %v", err)
	}
	defer doorB.Destroy()
	epochB, err := doorB.Epoch()
	if err != nil {
		t.Fatalf("Epoch of door B: %v", err)
	}
	readB, err := doorB.ReadKey()
	if err != nil {
		t.Fatalf("ReadKey of door B: %v", err)
	}
	writeB, err := doorB.WriteKey()
	if err != nil {
		t.Fatalf("WriteKey of door B: %v", err)
	}

	// clause one: the door read the epoch it was called at.
	if epochB != epochA+1 {
		t.Errorf("the door answered epoch %d after an advance from epoch %d; a door that cached its answer hands a caller the previous epoch's number beside the previous epoch's keys",
			epochB, epochA)
	}
	if bytes.Equal(writeB, heldWriteA) {
		t.Error("the door answered the same write_key across an advance, so it is not reading the session's current field")
	}
	if bytes.Equal(readB, heldReadA) {
		t.Error("the door answered the same read_key across an advance, so it is not reading the session's current field")
	}

	// clause two: door A is untouched, because it holds copies.
	afterReadA, err := doorA.ReadKey()
	if err != nil {
		t.Fatalf("ReadKey of door A after the advance: %v", err)
	}
	afterWriteA, err := doorA.WriteKey()
	if err != nil {
		t.Fatalf("WriteKey of door A after the advance: %v", err)
	}
	if !bytes.Equal(afterReadA, heldReadA) {
		t.Errorf("door A's read_key moved from %x to %x across an advance it was not present for; the session zeroizes its own array in place at every rotation, so an aliased value watches its key become zeros",
			heldReadA, afterReadA)
	}
	if !bytes.Equal(afterWriteA, heldWriteA) {
		t.Errorf("door A's write_key moved from %x to %x across an advance it was not present for",
			heldWriteA, afterWriteA)
	}
}

// ---------------------------------------------------------------------------
// Task 2 Property 4: the value holds no window onto the session, and the erase shows it
// ---------------------------------------------------------------------------

// The refusal's ABSENCE is the point here: a caller that closed a session and then read a key it
// had already been handed gets the key, not a refusal and not zeros. This is Task 1 Property 5's
// other half stated over a live session -- a constructor that aliased makes both fail.
func TestAnEpochKeysHoldsNoWindowOntoTheSessionItCameFrom(t *testing.T) {
	fixture := newTestSession(t, "epoch-keys-close")
	keys, err := fixture.session.EpochKeys()
	if err != nil {
		t.Fatalf("EpochKeys: %v", err)
	}
	defer keys.Destroy()
	read, err := keys.ReadKey()
	if err != nil {
		t.Fatalf("ReadKey before the close: %v", err)
	}
	write, err := keys.WriteKey()
	if err != nil {
		t.Fatalf("WriteKey before the close: %v", err)
	}
	heldRead := append([]byte(nil), read...)
	heldWrite := append([]byte(nil), write...)
	if !slices.ContainsFunc(heldWrite, func(octet byte) bool { return octet != 0 }) {
		t.Fatal("the door answered an all zero write_key before the session was closed, so this case would pass over a value that held nothing")
	}

	if err := fixture.session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	afterRead, err := keys.ReadKey()
	if err != nil {
		t.Fatalf("ReadKey after the session closed: %v; the value is the caller's and the close is the session's, and a value that refused here would be a value that had a window onto the session's own state",
			err)
	}
	afterWrite, err := keys.WriteKey()
	if err != nil {
		t.Fatalf("WriteKey after the session closed: %v", err)
	}
	if !bytes.Equal(afterRead, heldRead) {
		t.Errorf("read_key moved from %x to %x when the session closed, so the value is a window onto a field zeroizeOnLoop erases",
			heldRead, afterRead)
	}
	if !bytes.Equal(afterWrite, heldWrite) {
		t.Errorf("write_key moved from %x to %x when the session closed, so the value is a window onto a field zeroizeOnLoop erases",
			heldWrite, afterWrite)
	}
}

// ---------------------------------------------------------------------------
// Task 2 Property 5: the door is on the loop, and the class it joins is derived
// ---------------------------------------------------------------------------

// The behavioural half of Property 5 is TestEveryMethodOfAGroupSessionReachesItsStateOnlyOnTheLoop
// one file over, which derives its class off the syntax tree and needs no edit to cover a new
// method. THIS case is the half that gate cannot hold: that the new method actually joined the
// class it is being held to, per file, with the query that produced the numbers written beside
// them.
//
// The query is
//
//	git grep -c 'func (self \*GroupSession)' -- 'messagegroup/*.go' | grep -v _test
//
// and it answers seal.go:9 and session.go:15 on this commit -- eighteen members before k1 task 3,
// nineteen after it, twenty one after task 4's pair, and twenty four after the seal lift of
// 2026-09-13 added InstallEphRoot to session.go and sealEphWindowOnLoop and
// refuseAheadEphWindowOnLoop to seal.go. A reading that answered the same numbers with the door
// absent would be a reading of something else.
//
// THE THREE THAT LANDED WITH THE LIFT ARE WHERE THIS GATE SAYS THEY BELONG. InstallEphRoot is
// epoch state -- it sits beside AdvanceEpoch and installEpochOnLoop, which is the body that drops
// it -- and the other two are record methods, beside SealRecord and OpenRecord, which is the
// same per-file judgement the door itself was held to.
func TestTheEpochKeysDoorJoinsTheDerivedClassOfGroupSessionMethods(t *testing.T) {
	_, sources := messagegroupProductionSources(t)
	perFile := map[string][]string{}
	all := []string{}
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || sessionReceiverName(function) != "GroupSession" {
				continue
			}
			perFile[source.path] = append(perFile[source.path], function.Name.Name)
			all = append(all, function.Name.Name)
		}
	}
	slices.Sort(all)
	// the complement of "every method the loop gate holds" is the two it steps over, and they are
	// the MECHANISM the property is stated over rather than an exemption by name: do is the post
	// and run is the loop.
	complement := []string{}
	for _, name := range all {
		if name == "do" || name == "run" {
			complement = append(complement, name)
		}
	}
	t.Logf("%d method(s) on *GroupSession: %v; the complement the loop gate steps over is %v",
		len(all), all, complement)
	if want := []string{"do", "run"}; !slices.Equal(complement, want) {
		t.Errorf("the methods the loop gate steps over are %v, want %v; anything else stepped over is state reached off the loop with no gate on it",
			complement, want)
	}
	if !slices.Contains(all, "EpochKeys") {
		t.Fatal("no method named EpochKeys is declared on *GroupSession, so the door this file tests is not in the class the loop gate holds")
	}
	if len(all) != 44 {
		t.Errorf("%d methods are declared on *GroupSession and ledger item 251's ruling 40 makes it 44 -- 38 under item 242's R4, which was 35 after item 241's multi-epoch open (30 after MASTER section 8.4's second pass, which was 24 after the 2026-09-13 seal lift plus frameBodyOnLoop and unframeBodyOnLoop for the inner frame plus refuseFrameBindingsOnLoop, OpenCeremonyRecord, openRecordThroughDoor and MessageIdOf for the arm split and the id door, plus the five of pastepoch.go: InstallPastEpochLoader, TrackSenderAt and trackSenderAtOnLoop, scheduleForOnLoop and pastEpochOnLoop), plus R4's three: RoleAt, roleAtOnLoop and roleTableOnLoop -- plus the SIX of pqsecret.go: installPqSecretOnLoop, pqSecretForOnLoop, dropPqSecretsBelowWindowOnLoop and refusePqSecretConflictOnLoop for the table, and InstallPqSecret and DeclarePqSecretRotated for the fact a restarted session cannot observe for itself; the number moves by one per method, and a method that arrived without moving it arrived without a thought about which file it belongs in",
			len(all))
	}
	// AND THE SIX OF THE pq_secret TABLE ARE IN A FILE OF THEIR OWN, which is the same per-file
	// judgement pastepoch.go's five were held to and the same reason. What the table is is not
	// the session's epoch state: it is the one thing on this struct that SURVIVES an epoch
	// install, with a bound of its own (PastEpochWindow), an erase discipline of its own, and a
	// group-lifetime premise that is a paragraph rather than a line. Six methods of that in
	// session.go would be six methods a reader meets while reading about the epoch that is being
	// replaced. The DROP is still not a method of the session for pastepoch.go's reason --
	// connect/mls reads an erase field by field and follows no delegation -- so the whole-table
	// erase is spelled in zeroizeOnLoop, and what dropPqSecretsBelowWindowOnLoop holds is the
	// WINDOW, which is arithmetic and not an erase discipline.
	//
	// THE SIXTH IS refusePqSecretConflictOnLoop AND THE JUDGEMENT IS THE SAME ONE, made for the
	// reason this count exists rather than to accommodate it. It is a PREDICATE OVER THE TABLE --
	// it reads self.pqSecrets and writes nothing anywhere -- and what it decides is which values
	// may overwrite an entry, which is the table's rule and not the epoch install's. It lives
	// beside the writer it constrains, in the same way dropPqSecretsBelowWindowOnLoop lives beside
	// it while being called from installEpochOnLoop, and a reader asking "what can destroy
	// pq_secret[n]" finds the answer in one file. Written in session.go it would be an advance's
	// private arithmetic, which is exactly the reading under which the silence it replaces
	// survived a whole pass: the advance erased the entry a wrap had filed and put its own
	// argument there, and no gate in this package had a way to ask whether it should have.
	//
	// THE TWO PUBLIC ONES ARE HERE AND NOT IN session.go BESIDE InstallEphRoot, and the judgement
	// is the one this block is for. InstallEphRoot fills a field of the CURRENT epoch and is
	// dropped at the next install, so it belongs with the epoch state; these two are about the
	// TABLE and about a premise that survives every install -- and about a fact a restarted
	// session cannot observe, which is a paragraph only this file carries. A reader who meets
	// DeclarePqSecretRotated in session.go has no way to learn why a boolean needs a door.
	if got := len(perFile["pqsecret.go"]); got != 6 {
		t.Errorf("pqsecret.go declares %d methods on *GroupSession and ledger item 251's ruling 40 makes it 6: the install that files a secret at the epoch it belongs to and decides whether the group-lifetime premise still stands, the lookup every derivation of a storage root goes through, the window bound, the refusal that says which values may overwrite an entry, and the two doors a restorer states rotation through -- InstallPqSecret and DeclarePqSecretRotated. A seventh here is either a door nothing asked for or a piece of the epoch install that has drifted out of session.go",
			got)
	}
	if got := len(perFile["pastepoch.go"]); got != 8 {
		t.Errorf("pastepoch.go declares %d methods on *GroupSession and item 242's R4 makes it 8, which was item 241's 5: a prior epoch's schedule is neither epoch state of the session's own epoch nor a record method, so the door that installs its loader, the track for it, the lookup an open routes through and the build belong in the file that says what one holds -- and so do R4's three, because the whole of what RoleAt is is the SAME lookup an open routes through, asked for a different projection of the same handle, and a role door written in session.go would be one nothing stopped from reading self.handle. The DROP is still not a method, because connect/mls reads an erase field by field and follows no delegation, so it is spelled in installEpochOnLoop and zeroizeOnLoop",
			got)
	}
	if got := len(perFile["session.go"]); got != 15 {
		t.Errorf("session.go declares %d methods on *GroupSession and the seal lift makes it 15, which is the per-file half of the same count: the door belongs beside Epoch and AdvanceEpoch and not in seal.go, and so does InstallEphRoot",
			got)
	}
	if got := len(perFile["seal.go"]); got != 12 {
		t.Errorf("seal.go declares %d methods on *GroupSession and the arm split makes it 12: the re-auth, the window the sealer writes, the ahead refusal the opener makes, the two doors, the body they share and the id door are all record methods and belong beside SealRecord and OpenRecord, not beside the epoch keys door",
			got)
	}
}
