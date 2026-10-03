// THE RETENTION HALF of "a construction and its caller do not share an array", which is the half
// TestEveryConstructionInThisPackageLeavesItsInputAlone cannot see.
//
// That gate reads what a construction ANSWERS: it records the arrays it handed in and reports a
// result cut from one of them. That is the right question for a function whose whole product is its
// answer, and it is blind to a constructor that files a caller's array away inside a value whose
// storage is unexported. (*Group).GroupId is the two line demonstration: the group context held
// cfg.GroupId itself, and GroupId() copies on the way out, so the answer aliased nothing and the
// group and its caller shared an array for the whole life of the group. ANSWERED CLONED AND
// RETAINED SHARED passes that gate exactly as a correct constructor does.
//
// So the property here is the other half, stated over storage rather than over answers:
//
//	no octet a caller can reach through its own arguments is an octet the constructed value can
//	reach, and no octet a group holds is an octet it hands back out -- to whoever called it, or
//	onward to an object that caller supplied.
//
// The last clause is a third direction and it is the newest, added because BOTH gates read a
// method RESULT and an octet handed outward as an ARGUMENT is invisible to a reader of results.
// See the third section's own header for what that cost.
//
// WHY THE ARRAYS ARE WALKED AND NOT LISTED, WHICH IS THE FIRST HALF OF THIS FILE. The row for group
// creation in crypto_test.go names four arrays -- the group id, the X-Wing public key, the
// signature key and the identity -- and its own comment says "handed a caller's array in four
// places at once". The extension bodies are the FIFTH. Nothing was wrong with the four; the row
// simply never handed a fifth in, so it ran clean over a constructor that copied the Extension
// STRUCTS out of cfg.Extensions and left every ExtensionData pointing at the caller's octets. A
// caller writing into the buffer its policy was encoded out of rewrote what GroupContext()
// answered, GroupPolicy() then refused a group founded under a perfectly good policy, and the key
// schedule went on being derived over the octets as they were -- the group's published context and
// its epoch secrets parted company with nothing in between to point at.
//
// A fifth name would leave the sixth. So the arrays are read off the ARGUMENT TYPES: the walk below
// finds every route from a type to byte storage at any depth, and a row is refused unless it
// supplied an array down every one of them. A field added to GroupConfig tomorrow fails here as
// "carries no storage at", which is this gate declining to pass over it, rather than being covered
// by nobody at all.
//
// AND WHY THE SCOPE IS WALKED TOO, WHICH IS THE SECOND HALF. A gate that derived its class and then
// enumerated the one construction it ran over would be the same defect one level out, so the
// SUBJECTS are derived as well: a construction is in this class when it is handed a caller's bytes
// -- crypto_test.go's own derivation, reused rather than restated -- and answers a type this
// package declares with no exported field at all. That second clause is exactly the blindness this
// file exists for: a value whose every field is unexported is a value the answer side gate can see
// nothing inside, so the two gates' classes are complements rather than neighbours. It is nine
// constructions today; it was one when this file was drafted, and the other eight were already
// correct, which is the point -- they are held now rather than the day one of them stops being.
//
// WHERE THE WALKS STOP, written down rather than left to be rediscovered:
//
//  1. THE ARGUMENT SIDE DOES NOT ENTER AN INTERFACE and the retained side does. An interface
//     parameter hands over an OBJECT and not a buffer -- cfg.Store is a store the group is meant to
//     keep, and a gate that called that shared storage would be reporting the group for doing its
//     job. It is also the line the type walk beside it cannot cross at all, since a static type
//     carries no dynamic value, so entering interfaces on one side only is what keeps the two walks
//     answering the same question. What it costs is a member of the control: a caller's array
//     parked in an `any` field is storage this class does not see, and the day GroupConfig grows
//     one, this paragraph is the reason.
//  2. A STRUCT IS ENTERED THROUGH ITS EXPORTED FIELDS ONLY on the argument and answer sides,
//     because those are what a holder in another package can spell -- type_reach_test.go's first
//     line, for its reason. The RETAINED side enters everything: what a value can reach is not
//     limited to what anybody else can spell, the subjects of this gate are sealed to the last
//     field, and an exported-fields walk over one would reach nothing at all and report the clean
//     run of a gate that had checked something.
//  3. STORAGE IS COMPARED BY OVERLAP AND NOT BY THE POINTER. A constructor that filed away
//     cfg.GroupId[1:] shares every octet but the first with its caller, and two runs starting at
//     different addresses are one piece of storage when their extents meet.
//  4. AN INLINE BYTE ARRAY IS STORAGE ONLY WHERE IT IS ADDRESSABLE, since a [N]byte reached through
//     a copy IS a copy. Every row hands its arguments in through pointers so nothing is silently a
//     copy, and a walk meeting an unaddressable one records it as unfollowed rather than walking
//     past it.
//  5. A RECURSIVE TYPE IS ENTERED ONCE PER BRANCH by both walks, so the type walk declares one
//     level of a self referential field and the value walk follows as many as the value really
//     has. The two therefore agree only over types that are not self referential, which the
//     arguments of these constructions are; a self referential argument type would need this
//     paragraph rewritten rather than the demand quietly dropped.
//  6. THE HAND-OUT HALF IS THE GROUP'S AND NOT THE CLASS'S. "Answers no storage of its own" is a
//     property of the product surface, not of every sealed type: (*KeySchedule).WelcomeSecret is
//     documented next door as answering the schedule's own storage, and holding it to this would be
//     holding it to not doing its job -- zeroizeSecret's exemption, one level out. So the hand-out
//     gate's class is *Group's exported method set, read off the compiled type, and every other
//     sealed type is held by the retention half alone.
package mls

import (
	"bytes"
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

// ---------------------------------------------------------------------------
// one run of octets, and what it means for two of them to be the same storage
// ---------------------------------------------------------------------------

// byteStorage is one run of octets a walk reached, named twice: how carries the indices a reader
// needs to find it again, and path is the same route with the indices dropped, which is the
// spelling the type walk answers in.
//
// run is the slice itself, kept so the behavioural gate can read and write THROUGH what the walk
// reached rather than reconstructing an address. A walk that found storage it cannot touch has not
// really found it.
type byteStorage struct {
	how  string
	path string
	at   uintptr
	n    int
	run  reflect.Value
}

// overlaps answers whether a write through one run is visible through the other.
//
// Extents rather than the pointer, for the header's line 3: a sub-slice of a caller's array starts
// somewhere else and is the same storage.
func (self byteStorage) overlaps(other byteStorage) bool {
	return self.at < other.at+uintptr(other.n) && other.at < self.at+uintptr(self.n)
}

// octets reads the run out one byte at a time rather than through Bytes(), which refuses a value
// this walk reached through an unexported field -- and those are most of what the retained side
// reaches.
func (self byteStorage) octets() []byte {
	out := make([]byte, self.n)
	for at := range out {
		out[at] = byte(self.run.Index(at).Uint())
	}
	return out
}

// scribble writes a pattern through the run so that every octet of it changes. reflect rather than
// unsafe: every run the argument walk reached came off a value the caller still holds, so the write
// goes through the slice itself.
func (self byteStorage) scribble(mark byte) {
	for at := range self.n {
		self.run.Index(at).SetUint(uint64(mark ^ byte(at)))
	}
}

// how deep a walk goes and how many runs it will record. Both are guards against a cyclic or fanned
// out value rather than rules -- a gate that hung, or that ate the machine, is no gate -- and
// hitting either is recorded as unfollowed rather than passed over in silence.
const (
	byteStorageDepth = 24
	byteStorageMost  = 20000
)

// byteStorageWalk is which of the two questions a walk is asking; see the header's lines 1 and 2.
type byteStorageWalk struct {
	enterUnexported bool
	enterInterfaces bool
}

var (
	// what a caller owns: what it can spell, and no object it handed over
	byteStorageOutside = byteStorageWalk{}
	// what a value hands back: what a holder can spell, including whatever an interface holds
	byteStorageAnswer = byteStorageWalk{enterInterfaces: true}
	// what a value can reach: everything, because its own fields are its own business
	byteStorageInside = byteStorageWalk{enterUnexported: true, enterInterfaces: true}
)

// byteStorageFinder collects the runs one walk reached, and what it could not follow.
//
// unfollowed is not tidiness. A shape this walk cannot enter is storage nobody looked at, and a
// gate that walked past it would report the clean run of a gate that had checked it.
type byteStorageFinder struct {
	how        byteStorageWalk
	found      []byteStorage
	unfollowed []string
	entered    map[[2]uintptr]bool
}

func newByteStorageFinder(how byteStorageWalk) *byteStorageFinder {
	return &byteStorageFinder{how: how, entered: map[[2]uintptr]bool{}}
}

// paths answers the routes this walk found storage through, deduplicated, so a slice of four
// entries reads as one route rather than four.
func (self *byteStorageFinder) paths() []string {
	out := []string{}
	for _, one := range self.found {
		if !slices.Contains(out, one.path) {
			out = append(out, one.path)
		}
	}
	slices.Sort(out)
	return out
}

// contents answers what this walk reached, route by route, as hex, which is what a before and after
// comparison is made over.
func (self *byteStorageFinder) contents() map[string][]string {
	out := map[string][]string{}
	for _, one := range self.found {
		out[one.path] = append(out[one.path], fmt.Sprintf("%x", one.octets()))
	}
	for route := range out {
		slices.Sort(out[route])
	}
	return out
}

// walk reads every run of octets reachable from one value.
//
// The cycle guard is per BRANCH and not per walk: a type reached twice down two different routes is
// two different runs of storage, and a guard held across the whole walk would drop the second --
// which is a class that quietly shrank, and a gate demanding less while reporting what a complete
// one reports.
func (self *byteStorageFinder) walk(value reflect.Value, how string, path string, depth int) {
	if !value.IsValid() {
		return
	}
	if depth > byteStorageDepth {
		self.unfollowed = append(self.unfollowed, how+", which is deeper than this walk goes")
		return
	}
	if len(self.found) > byteStorageMost {
		self.unfollowed = append(self.unfollowed, how+", reached after this walk had recorded all it will")
		return
	}
	switch value.Kind() {
	case reflect.Slice:
		if value.IsNil() {
			return
		}
		// a byte slice under any name: HpkePublicKey and SignaturePrivateKey are the storage
		// they are, which is the reading packageByteSliceTypeNames takes off the source for
		// the gate next door
		if value.Type().Elem().Kind() == reflect.Uint8 {
			if value.Len() != 0 {
				self.found = append(self.found,
					byteStorage{how: how, path: path, at: value.Pointer(), n: value.Len(), run: value})
			}
			return
		}
		if !self.enter(value) {
			return
		}
		defer self.leave(value)
		for at := range value.Len() {
			self.walk(value.Index(at), fmt.Sprintf("%s[%d]", how, at), path+"[]", depth+1)
		}
	case reflect.Array:
		if value.Type().Elem().Kind() == reflect.Uint8 {
			if value.Len() == 0 {
				return
			}
			if !value.CanAddr() {
				self.unfollowed = append(self.unfollowed,
					how+", an inline byte array reached through a copy, whose storage this walk cannot name")
				return
			}
			self.found = append(self.found, byteStorage{how: how, path: path,
				at: value.Addr().Pointer(), n: value.Len(), run: value.Slice(0, value.Len())})
			return
		}
		for at := range value.Len() {
			self.walk(value.Index(at), fmt.Sprintf("%s[%d]", how, at), path+"[]", depth+1)
		}
	case reflect.Pointer:
		if value.IsNil() {
			return
		}
		if !self.enter(value) {
			return
		}
		defer self.leave(value)
		self.walk(value.Elem(), how+"->", path+"->", depth+1)
	case reflect.Interface:
		if value.IsNil() || !self.how.enterInterfaces {
			return
		}
		self.walk(value.Elem(), how+".()", path+".()", depth+1)
	case reflect.Struct:
		for at := range value.NumField() {
			field := value.Type().Field(at)
			if !field.IsExported() && !self.how.enterUnexported {
				continue
			}
			self.walk(value.Field(at), how+"."+field.Name, path+"."+field.Name, depth+1)
		}
	case reflect.Map:
		if value.IsNil() {
			return
		}
		if !self.enter(value) {
			return
		}
		defer self.leave(value)
		for iterator := value.MapRange(); iterator.Next(); {
			self.walk(iterator.Key(), how+"{key}", path+"{key}", depth+1)
			self.walk(iterator.Value(), how+"{}", path+"{}", depth+1)
		}
	case reflect.Chan, reflect.Func, reflect.UnsafePointer:
		// a channel would have to be drained to be read and a func would have to be called,
		// and either is a side effect this gate is not entitled to have on a live value. Both
		// are recorded rather than passed over.
		if value.IsNil() {
			return
		}
		self.unfollowed = append(self.unfollowed,
			fmt.Sprintf("%s, a %s this walk will not follow", how, value.Kind()))
	}
}

// enter and leave are the per branch cycle guard. The key is the pair (type, address), so a struct
// and its first field -- one address, two types -- are not confused for each other.
func (self *byteStorageFinder) enter(value reflect.Value) bool {
	key := [2]uintptr{reflect.ValueOf(value.Type()).Pointer(), value.Pointer()}
	if self.entered[key] {
		return false
	}
	self.entered[key] = true
	return true
}

func (self *byteStorageFinder) leave(value reflect.Value) {
	delete(self.entered, [2]uintptr{reflect.ValueOf(value.Type()).Pointer(), value.Pointer()})
}

// byteStorageFoundIn is the whole of one walk, since every caller wants the runs and the refusals
// together.
func byteStorageFoundIn(value reflect.Value, how string, walk byteStorageWalk) *byteStorageFinder {
	finder := newByteStorageFinder(walk)
	finder.walk(value, how, how, 0)
	return finder
}

// ---------------------------------------------------------------------------
// and the same question asked of a TYPE, which is what makes the arrays derived
// ---------------------------------------------------------------------------

// byteStoragePathsOf answers every route from a type to byte storage.
//
// The value walk finds what a row HAPPENED to hand in; this finds what the type DECLARES, and the
// gate holds the first to cover the second. A field added to an argument type is a route here on
// the day it is declared, so a row that does not fill it fails rather than passing over it.
func byteStoragePathsOf(found reflect.Type, path string) []string {
	into := []string{}
	byteStoragePathsThrough(found, path, &into, map[reflect.Type]bool{}, 0)
	slices.Sort(into)
	return slices.Compact(into)
}

func byteStoragePathsThrough(found reflect.Type, path string, into *[]string,
	entered map[reflect.Type]bool, depth int) {
	if found == nil || depth > byteStorageDepth || entered[found] {
		return
	}
	// per branch, exactly as the value walk's guard is and for its reason: a self referential
	// type is stopped and a type reached twice down two routes is two routes
	entered[found] = true
	defer delete(entered, found)
	switch found.Kind() {
	case reflect.Slice, reflect.Array:
		if found.Elem().Kind() == reflect.Uint8 {
			*into = append(*into, path)
			return
		}
		byteStoragePathsThrough(found.Elem(), path+"[]", into, entered, depth+1)
	case reflect.Pointer:
		byteStoragePathsThrough(found.Elem(), path+"->", into, entered, depth+1)
	case reflect.Struct:
		for at := range found.NumField() {
			field := found.Field(at)
			if !field.IsExported() {
				continue
			}
			byteStoragePathsThrough(field.Type, path+"."+field.Name, into, entered, depth+1)
		}
	case reflect.Map:
		byteStoragePathsThrough(found.Key(), path+"{key}", into, entered, depth+1)
		byteStoragePathsThrough(found.Elem(), path+"{}", into, entered, depth+1)
	}
}

// ---------------------------------------------------------------------------
// and the same question asked of the TYPE CHECKER's types, which is what makes the SCOPE derived
// ---------------------------------------------------------------------------

// typeReachesByteStorage answers whether a value of this type carries a caller's byte storage, at
// any depth, through the constructors byteStoragePathsOf walks: slices, arrays, pointers, maps and
// exported struct fields.
//
// TWO WALKS OVER ONE QUESTION, and the second one is here because the class this gate runs over
// was read with a NARROWER matcher than the arrays inside it. byteStoragePathsOf answers over
// reflect and can only be asked about a type somebody already has a value of; the SCOPE has to be
// asked about the parameters of a package level function, which reflect cannot enumerate, and the
// reading it used was a match on how the parameter was SPELLED -- []byte, or one of the names this
// package declares for a byte slice. So the arrays half walked to unbounded depth while the half
// choosing what to walk read one hop, and NewProposalList([]CachedProposal) and
// ParseRatchetTreeFrom(Extension) are both handed a caller's octets, both answer a sealed type,
// and both sat outside the class. That is not hypothetical: a real retention in NewProposalList --
// its Ref clone removed -- was caught by a hand written test, which is the enumeration this gate
// was written to replace.
//
// The two are held to ONE class by TestTheByteStorageReadingsAgreeOverEveryConstructor below,
// which runs both over the same list of arms, since two spellings of one question is how one of
// them ends up narrower than the other.
func typeReachesByteStorage(found types.Type) bool {
	return typeReachesByteStorageThrough(found, map[types.Type]bool{})
}

// typeReachesByteStorageThrough is the walk, carrying the types it has already entered.
//
// The guard is per BRANCH, exactly as byteStoragePathsThrough's is: a type reached twice down two
// routes is two routes, and a guard held across the whole walk would answer for the second from
// the first. An interface is NOT entered, which is byteStorageOutside's line: a parameter of
// interface type hands over an OBJECT and not a buffer, and this reading is only ever asked about
// what a construction is HANDED.
func typeReachesByteStorageThrough(found types.Type, entered map[types.Type]bool) bool {
	found = types.Unalias(found)
	if found == nil || entered[found] {
		return false
	}
	entered[found] = true
	defer delete(entered, found)
	switch shape := found.(type) {
	case *types.Named:
		return typeReachesByteStorageThrough(shape.Underlying(), entered)
	case *types.Slice:
		return isByteKind(shape.Elem()) || typeReachesByteStorageThrough(shape.Elem(), entered)
	case *types.Array:
		return isByteKind(shape.Elem()) || typeReachesByteStorageThrough(shape.Elem(), entered)
	case *types.Pointer:
		return typeReachesByteStorageThrough(shape.Elem(), entered)
	case *types.Struct:
		for at := 0; at < shape.NumFields(); at += 1 {
			field := shape.Field(at)
			// the exported fields, which are what a holder can spell; the header's line 2
			if !field.Exported() {
				continue
			}
			if typeReachesByteStorageThrough(field.Type(), entered) {
				return true
			}
		}
	case *types.Map:
		// both halves, since a map keyed by a caller's array hands it over exactly as one
		// valued by it does
		return typeReachesByteStorageThrough(shape.Key(), entered) ||
			typeReachesByteStorageThrough(shape.Elem(), entered)
	}
	return false
}

// isByteKind answers whether this is the element type a run of octets is made of. It is the
// element half of isByteSliceType next door, split out because the walk above asks it of a slice
// element and of an array element alike.
func isByteKind(found types.Type) bool {
	basic, isBasic := found.Underlying().(*types.Basic)
	return isBasic && basic.Kind() == types.Uint8
}

// ---------------------------------------------------------------------------
// the controls for the walks: one member per arm, so an arm that stopped working fails here
// ---------------------------------------------------------------------------

// callerArrayControl carries one member per constructor either walk claims to enter, and one per
// line this file's header says it draws.
//
// A control rather than a second opinion about the real types: a walk that entered nothing reports
// a clean run over every retention there is, and the only way to tell that apart from a value that
// shares nothing is to run it over one known to hold storage down every route.
type callerArrayControl struct {
	Direct    []byte
	Named     SignaturePublicKey
	Fixed     [4]byte
	Pointed   *[]byte
	Nested    callerArrayControlInner
	NestedPtr *callerArrayControlInner
	Entries   []callerArrayControlInner
	Fixture   [1]callerArrayControlInner
	Keyed     map[string][]byte
	KeyedBy   map[*callerArrayControlInner]bool
	Held      any
	hidden    []byte
}

// the deeper member. A body two structures down rather than one is what the extension bodies are --
// cfg.Extensions[].ExtensionData is reached through a slice AND a struct -- so a walk that stopped
// at the first hop would find the group id and miss the thing this file was written for.
type callerArrayControlInner struct {
	Deeper  []byte
	Deepest *callerArrayControlInner
}

func newCallerArrayControl() *callerArrayControl {
	pointed := []byte("through a pointer to a slice")
	inner := func(mark byte) callerArrayControlInner {
		return callerArrayControlInner{Deeper: bytes.Repeat([]byte{mark}, 8)}
	}
	keyed := inner(0x0a)
	return &callerArrayControl{
		Direct:    []byte("direct"),
		Named:     SignaturePublicKey("a byte slice under another name"),
		Fixed:     [4]byte{1, 2, 3, 4},
		Pointed:   &pointed,
		Nested:    inner(0x0c),
		NestedPtr: &callerArrayControlInner{Deeper: []byte("a body two structures down")},
		Entries:   []callerArrayControlInner{inner(0x01)},
		Fixture:   [1]callerArrayControlInner{inner(0x02)},
		Keyed:     map[string][]byte{"it": []byte("a map value")},
		KeyedBy:   map[*callerArrayControlInner]bool{&keyed: true},
		Held:      []byte("inside an interface"),
		hidden:    []byte("behind an unexported field"),
	}
}

// What the TYPE walk must answer over the control: every route but the interface, which no static
// type carries a value in, and every route but the unexported field, which no holder outside the
// declaring package can spell.
var callerArrayControlPaths = []string{
	"it->.Direct",
	"it->.Entries[].Deeper",
	"it->.Fixed",
	"it->.Fixture[].Deeper",
	"it->.Keyed{}",
	"it->.KeyedBy{key}->.Deeper",
	"it->.Named",
	"it->.Nested.Deeper",
	"it->.NestedPtr->.Deeper",
	"it->.Pointed->",
}

// and what the three VALUE walks must answer. They differ from the type walk, and from each other,
// by exactly one member apiece: the interface the argument side does not enter, and the unexported
// field only the retained side can spell. Held exactly rather than as floors in both directions --
// a route missing is an arm that stopped working, and a route extra is a line this file says it
// draws and does not.
var (
	callerArrayControlOutside = callerArrayControlPaths
	callerArrayControlAnswer  = append([]string{"it->.Held.()"}, callerArrayControlPaths...)
	callerArrayControlInside  = append([]string{"it->.Held.()", "it->.hidden"}, callerArrayControlPaths...)
)

// TestTheByteStorageWalksAgreeOverEveryConstructor is the control both walks are read through.
func TestTheByteStorageWalksAgreeOverEveryConstructor(t *testing.T) {
	control := newCallerArrayControl()
	if declared := byteStoragePathsOf(reflect.TypeOf(control), "it"); !slices.Equal(declared,
		sortedStorageRoutes(callerArrayControlPaths)) {
		t.Errorf("the type walk declares storage at %v, want %v",
			declared, sortedStorageRoutes(callerArrayControlPaths))
	}
	for _, arm := range []struct {
		name string
		walk byteStorageWalk
		want []string
	}{
		{name: "the argument side", walk: byteStorageOutside, want: callerArrayControlOutside},
		{name: "the answer side", walk: byteStorageAnswer, want: callerArrayControlAnswer},
		{name: "the retained side", walk: byteStorageInside, want: callerArrayControlInside},
	} {
		finder := byteStorageFoundIn(reflect.ValueOf(control), "it", arm.walk)
		if len(finder.unfollowed) != 0 {
			t.Errorf("%s could not follow %v out of its own control, so it would walk past the same shape in a real value",
				arm.name, finder.unfollowed)
		}
		if got := finder.paths(); !slices.Equal(got, sortedStorageRoutes(arm.want)) {
			t.Errorf("%s reached storage through %v, want %v",
				arm.name, got, sortedStorageRoutes(arm.want))
		}
	}
	// and the runs are readable and writable through what the walk kept, since the behavioural
	// half of this file spends both
	finder := byteStorageFoundIn(reflect.ValueOf(control), "it", byteStorageOutside)
	for _, one := range finder.found {
		if !bytes.Equal(one.octets(), one.run.Slice(0, one.n).Bytes()) {
			t.Errorf("the walk reads %s back as something other than what it holds", one.how)
		}
	}
	control.Direct = bytes.Repeat([]byte{0x11}, 6)
	before := bytes.Clone(control.Direct)
	byteStorage{n: len(control.Direct), run: reflect.ValueOf(control.Direct)}.scribble(0x5a)
	if bytes.Equal(before, control.Direct) {
		t.Error("a scribble through what the walk kept changed nothing, so the behavioural half writes nowhere")
	}
}

// callerArrayControlArms is the arms of the control above, read off the routes the type walk
// answers rather than typed out a second time.
//
// It is what joins the two readings: the reflect walk's own answer over callerArrayControl becomes
// the list the type checked walk is held to, so an arm one of them stopped entering is a
// disagreement rather than two lists somebody kept in step.
func callerArrayControlArms() []string {
	arms := []string{}
	for _, route := range callerArrayControlPaths {
		arm := strings.TrimPrefix(route, "it->.")
		if at := strings.IndexAny(arm, ".[{-"); at >= 0 {
			arm = arm[:at]
		}
		if !slices.Contains(arms, arm) {
			arms = append(arms, arm)
		}
	}
	slices.Sort(arms)
	return arms
}

// The same shapes as callerArrayControl, one per FUNCTION so the type checked reading can be asked
// about a parameter, which is the position it is used at.
//
// The names are the control's field names with takes in front, so the comparison below is against
// the arms the reflect walk answers rather than against a second list. The three at the foot are
// the arms that walk does not answer -- an interface, an unexported field, and a parameter with no
// storage anywhere in it -- so a reading that swept any of them in fails here.
const callerStorageTypeControl = `package control

type Named []byte

type inner struct {
	Deeper  []byte
	Deepest *inner
}

type sealed struct {
	hidden []byte
}

type held interface{ Held() []byte }

func takesDirect(v []byte)           {}
func takesNamed(v Named)             {}
func takesFixed(v [4]byte)           {}
func takesPointed(v *[]byte)         {}
func takesNested(v inner)            {}
func takesNestedPtr(v *inner)        {}
func takesEntries(v []inner)         {}
func takesFixture(v [1]inner)        {}
func takesKeyed(v map[string][]byte) {}
func takesKeyedBy(v map[*inner]bool) {}
func takesHeld(v held)               {}
func takesHidden(v sealed)           {}
func takesNothing(v int)             {}
`

// TestTheByteStorageReadingsAgreeOverEveryConstructor holds the type checked reading to the
// reflect one over the arms the reflect one answers.
//
// It is the fix for the defect one level out: the arrays inside a construction were walked at
// unbounded depth and the choice of WHICH constructions to walk was made on how a parameter was
// spelled. Two readings of one property drift, and the one that drifts narrower feeds a class
// every gate over it then reports a clean run against.
func TestTheByteStorageReadingsAgreeOverEveryConstructor(t *testing.T) {
	want := []string{}
	for _, arm := range callerArrayControlArms() {
		want = append(want, "takes"+arm)
	}
	if len(want) == 0 {
		t.Fatal("the reflect walk answers no arm of its own control, so this comparison holds for a reading that matches nothing")
	}
	got := []string{}
	declared := 0
	for _, function := range declaredFunctionsIn(t,
		typeCheckedText(t, "the caller storage type control", callerStorageTypeControl)) {

		if function.method {
			continue
		}
		declared += 1
		parameters := function.signature.Params()
		for at := 0; at < parameters.Len(); at += 1 {
			if typeReachesByteStorage(parameters.At(at).Type()) {
				got = append(got, function.name)
				break
			}
		}
	}
	// the three the reflect walk does not answer are declared here too, or the comparison would
	// be against a control carrying only its positives
	if declared != len(want)+3 {
		t.Fatalf("the control declares %d functions and the class it must separate is %v plus three negatives; a control narrower than the class it holds agrees with anything",
			declared, want)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("the type checked reading is handed storage by %v, and the reflect walk answers the arms %v; the two readings of one property have parted company",
			got, want)
	}
}

// TestTheByteStorageValueWalkStopsOnACycle is the per branch guard, run rather than argued: a value
// that points at itself terminates and is recorded once.
func TestTheByteStorageValueWalkStopsOnACycle(t *testing.T) {
	loop := &callerArrayControlInner{Deeper: []byte("round and round")}
	loop.Deepest = loop
	finder := byteStorageFoundIn(reflect.ValueOf(loop), "loop", byteStorageInside)
	if got := finder.paths(); !slices.Equal(got, []string{"loop->.Deeper"}) {
		t.Errorf("the value walk read %v out of a value that points at itself, want the one run it holds", got)
	}
	if len(finder.unfollowed) != 0 {
		t.Errorf("the value walk reported %v over a cycle, which the branch guard is supposed to close quietly",
			finder.unfollowed)
	}
}

// TestTheStorageOverlapRuleReadsExtentsAndNotPointers is the header's line 3.
func TestTheStorageOverlapRuleReadsExtentsAndNotPointers(t *testing.T) {
	whole := byteStorage{at: 0x1000, n: 16}
	for _, arm := range []struct {
		name    string
		other   byteStorage
		overlap bool
	}{
		{name: "the same run", other: byteStorage{at: 0x1000, n: 16}, overlap: true},
		{name: "a run one octet in", other: byteStorage{at: 0x1001, n: 15}, overlap: true},
		{name: "the last octet alone", other: byteStorage{at: 0x100f, n: 1}, overlap: true},
		{name: "a run ending where this starts", other: byteStorage{at: 0x0ff0, n: 16}, overlap: false},
		{name: "a run starting where this ends", other: byteStorage{at: 0x1010, n: 16}, overlap: false},
	} {
		if whole.overlaps(arm.other) != arm.overlap || arm.other.overlaps(whole) != arm.overlap {
			t.Errorf("%s reads as overlap %v, want %v", arm.name, !arm.overlap, arm.overlap)
		}
	}
}

func sortedStorageRoutes(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return slices.Compact(out)
}

// ---------------------------------------------------------------------------
// the scope, read off the source: the types the answer side gate cannot see into
// ---------------------------------------------------------------------------

// packageSealedTypeNamesIn answers the exported struct types one file declares whose every field is
// unexported.
//
// A field counts under the name it is SPELLED by, which for an embedded field is the last identifier
// of its type: a struct embedding an exported type has an exported field, however it was written,
// and a walk over what a holder can spell would enter it.
func packageSealedTypeNamesIn(parsed parsedSource) []string {
	names := []string{}
	for _, declaration := range parsed.file.Decls {
		general, isGeneral := declaration.(*ast.GenDecl)
		if !isGeneral || general.Tok != token.TYPE {
			continue
		}
		for _, spec := range general.Specs {
			typeSpec, isType := spec.(*ast.TypeSpec)
			if !isType || !typeSpec.Name.IsExported() || typeSpec.Assign.IsValid() {
				continue
			}
			structType, isStruct := typeSpec.Type.(*ast.StructType)
			if !isStruct || structType.Fields == nil {
				continue
			}
			fields, exported := 0, 0
			for _, field := range structType.Fields.List {
				spelled := []string{}
				for _, name := range field.Names {
					spelled = append(spelled, name.Name)
				}
				if len(spelled) == 0 {
					spelled = append(spelled, embeddedFieldName(parsed.render(field.Type)))
				}
				for _, name := range spelled {
					fields++
					if ast.IsExported(name) {
						exported++
					}
				}
			}
			if fields != 0 && exported == 0 {
				names = append(names, typeSpec.Name.Name)
			}
		}
	}
	return names
}

// embeddedFieldName is the name an embedded field is spelled by: the last identifier of its type,
// with the pointer and the package qualifier stripped.
func embeddedFieldName(rendered string) string {
	rendered = strings.TrimPrefix(rendered, "*")
	if at := strings.LastIndex(rendered, "."); at >= 0 {
		rendered = rendered[at+1:]
	}
	return rendered
}

// packageSealedTypeNames is the same over the whole package's non test source.
//
// Absence is fatal rather than clean, for the reason every derivation in this package is: a scan
// that stopped matching leaves the gate reading it with an empty class, and an empty class reports
// exactly what a complete one reports.
func packageSealedTypeNames(t *testing.T) []string {
	t.Helper()
	names := []string{}
	for _, path := range packageSourcePaths(t) {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		names = append(names, packageSealedTypeNamesIn(mustParseSource(t, path))...)
	}
	if len(names) == 0 {
		t.Fatal("this package declares no sealed type, so the class below has no subjects")
	}
	slices.Sort(names)
	return names
}

// packageLevelFunctionResults answers, per package level function, the identifiers its result list
// names. A construction ANSWERS a type when that type's name is among them, at any depth of pointer,
// slice or tuple, which is the reading a holder takes: a *KeySchedule and a []*KeySchedule are both
// a key schedule in somebody's hands.
func packageLevelFunctionResults(t *testing.T) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, path := range packageSourcePaths(t) {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		parsed := mustParseSource(t, path)
		for _, declaration := range parsed.file.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Recv != nil || function.Type.Results == nil {
				continue
			}
			named := []string{}
			for _, field := range function.Type.Results.List {
				ast.Inspect(field.Type, func(node ast.Node) bool {
					if identifier, isIdentifier := node.(*ast.Ident); isIdentifier {
						named = append(named, identifier.Name)
					}
					return true
				})
			}
			out[function.Name.Name] = named
		}
	}
	if len(out) == 0 {
		t.Fatal("this package declares no package level function answering anything, so the class below has no subjects")
	}
	return out
}

// packageLevelFunctionArity answers how many arguments a construction takes, so a row that stopped
// passing one is a row that fails rather than one that quietly hands in less than it used to.
func packageLevelFunctionArity(t *testing.T, name string) int {
	t.Helper()
	for _, function := range packageLevelFunctions(t).functions {
		if function.name == name {
			return len(function.parameters)
		}
	}
	t.Fatalf("this package declares no package level function %s", name)
	return 0
}

// packageLevelFunctionsHandedCallerStorage answers the package level functions whose PARAMETER
// TYPES reach a caller's byte storage, at any depth.
//
// This is not packageLevelFunctionsTakingCallerBytes and the difference is the whole point. That
// reading is a SPELLING: a parameter counts when it is written []byte or as one of the names this
// package declares for a byte slice. It is exactly right for the gate it feeds -- "leaves the
// array it was handed alone" is a statement about an array somebody handed over -- and it is one
// hop deep, while the arrays half of THIS gate walks a type to unbounded depth. A gate whose scope
// is read shallower than its subject understates the class, and that is what happened here:
// NewProposalList takes a []CachedProposal and ParseRatchetTreeFrom takes an Extension, both are
// handed a caller's octets down a route no spelling can see, both answer a type sealed to the last
// field, and both sat outside a class derived to hold exactly that shape. A real retention in the
// first of them -- its Ref clone removed -- was caught by a hand written test, which is the
// enumeration this gate exists to replace.
//
// The type checker and not the parse tree, for extensionTypeSelectionNamedAs's reason: a defined
// type, an alias and a type spelled through another package's name are one type to the compiler
// and three spellings to a matcher.
func packageLevelFunctionsHandedCallerStorage(t *testing.T) []string {
	t.Helper()
	names := []string{}
	for _, function := range declaredFunctionsOf(t, cryptoOwnRoot) {
		if function.method {
			continue
		}
		parameters := function.signature.Params()
		for at := 0; at < parameters.Len(); at += 1 {
			if typeReachesByteStorage(parameters.At(at).Type()) {
				names = append(names, function.name)
				break
			}
		}
	}
	if len(names) == 0 {
		t.Fatal("no package level function of this package is handed a caller's storage, so the class below has no subjects")
	}
	slices.Sort(names)
	return names
}

// packageConstructionsOfSealedStorage is the class this gate runs over: handed a caller's bytes,
// and answering a value nothing outside can see inside.
//
// Both halves are read off the source. The first is the reach reading above rather than
// crypto_test.go's spelling based one -- see there for why the two are not one class -- and that
// gate's exemption table is honoured here too: newKeyScheduleFromParts is excused there with
// "retaining it is the function", which is a RETENTION exemption and belongs to this gate more
// squarely than to that one.
func packageConstructionsOfSealedStorage(t *testing.T) []string {
	t.Helper()
	sealed := packageSealedTypeNames(t)
	results := packageLevelFunctionResults(t)
	names := []string{}
	for _, name := range packageLevelFunctionsHandedCallerStorage(t) {
		if _, isExcused := packageConstructionsOverBorrowedBytes[name]; isExcused {
			continue
		}
		for _, answered := range results[name] {
			if slices.Contains(sealed, answered) {
				names = append(names, name)
				break
			}
		}
	}
	if len(names) == 0 {
		t.Fatal("no construction of this package is handed bytes and answers sealed storage, so this gate demands nothing")
	}
	slices.Sort(names)
	return names
}

// TestTheSealedTypeScanReadsWhatAHolderCannotSeeInto is the control for the scope derivation.
//
// One declaration per arm, so a scan that started reading an exported field as unexported, or that
// stopped entering embedded fields, fails here rather than widening the class in silence. A class
// that GREW is as bad as one that shrank: every extra member is a construction with no row, and the
// repair somebody reaches for is deleting the demand.
func TestTheSealedTypeScanReadsWhatAHolderCannotSeeInto(t *testing.T) {
	found := packageSealedTypeNamesIn(mustParseText(t, "the sealed type control", sealedTypeControl))
	slices.Sort(found)
	if want := []string{"Sealed", "SealedByEmbedding"}; !slices.Equal(found, want) {
		t.Errorf("the sealed type scan read %v out of the control, want %v", found, want)
	}
	// and the real package's own sealed types are read the same way, with the one this file was
	// written about named rather than assumed
	if named := packageSealedTypeNames(t); !slices.Contains(named, "Group") {
		t.Errorf("the sealed type scan read %v out of this package, which declares Group with no exported field", named)
	}
}

// Six type declarations, two of which are sealed and four of which are not. Every arm of the scan
// runs on this, so one that started matching an empty struct, an alias, a non struct or a type
// whose only field is embedded and exported fails here.
const sealedTypeControl = `package control

// sealed: every field unexported
type Sealed struct {
	held  []byte
	count int
}

// sealed as well, and the reason the scan reads an embedded field by its spelled name: this one is
// embedded and unexported
type SealedByEmbedding struct {
	sealedInner
}

// not sealed: one exported field is one a holder can spell
type NotSealed struct {
	Held []byte
	held []byte
}

// not sealed: an embedded EXPORTED type is an exported field however it is written
type NotSealedByEmbedding struct {
	Sealed
	held []byte
}

// not sealed: there is nothing in it to be sealed
type NotSealedBecauseEmpty struct{}

// not a struct, so not in the class at all
type NotAStruct []byte

// an alias rather than a declaration, which names no new type to be sealed
type NotADeclaration = Sealed

// unexported, so outside the surface this gate is about
type sealedInner struct {
	held []byte
}
`

// ---------------------------------------------------------------------------
// the retention half, over every construction of that class
// ---------------------------------------------------------------------------

// callerArrayRow is one construction, with the arguments it is handed and the value it made.
//
// arguments are POINTERS to the caller's own variables, in the order the signature takes them, for
// the header's line 4 and so the arity read off the source can be compared against them. made is
// the sealed value; the gate walks the two and demands they share nothing.
type callerArrayRow struct {
	name  string
	build func(t *testing.T) (arguments []any, made any)
}

// callerArrayRows is every construction this gate runs, and it is held equal to the class derived
// from the source at the bottom of the gate.
func callerArrayRows() []callerArrayRow {
	return []callerArrayRow{
		{name: "NewGroup", build: func(t *testing.T) ([]any, any) {
			cfg, signer, cred := callerOwnedGroupArguments(t)
			group, err := NewGroup(cfg, signer, cred)
			if err != nil {
				t.Fatalf("found the group this row follows: %v", err)
			}
			t.Cleanup(func() { group.Close() })
			return []any{cfg, &signer, &cred}, group
		}},
		{name: "JoinFromWelcome", build: func(t *testing.T) ([]any, any) {
			cfg, welcome, ratchetTree, keys := callerOwnedJoinArguments(t)
			group, err := JoinFromWelcome(cfg, welcome, ratchetTree, keys)
			if err != nil {
				t.Fatalf("join the group this row follows: %v", err)
			}
			t.Cleanup(func() { group.Close() })
			return []any{cfg, &welcome, &ratchetTree, keys}, group
		}},
		// p7 task 19's restore, which is a construction of sealed storage over caller bytes in
		// exactly NewGroup's sense and one step further out: the group id it looks the state up
		// by and the signing key it rebuilds a verified context with are both the caller's, and
		// the group it answers signs with that key and derives over that id for the whole of its
		// life. It is founded first, because a restore needs a state to read -- what the row
		// follows is the arrays the RESTORE was handed.
		{name: "LoadGroup", build: func(t *testing.T) ([]any, any) {
			cfg, signer, cred := callerOwnedGroupArguments(t)
			founded, err := NewGroup(cfg, signer, cred)
			if err != nil {
				t.Fatalf("found the group this row restores: %v", err)
			}
			epoch := founded.Epoch()
			founded.Close()
			group, err := LoadGroup(cfg, epoch, signer)
			if err != nil {
				t.Fatalf("restore the group this row follows: %v", err)
			}
			t.Cleanup(func() { group.Close() })
			return []any{cfg, &epoch, &signer}, group
		}},
		{name: "NewKeySchedule", build: func(t *testing.T) ([]any, any) {
			crypto, context, secrets := callerOwnedScheduleArguments(t)
			schedule, err := NewKeySchedule(crypto, secrets["init"], secrets["commit"],
				secrets["psk"], context)
			if err != nil {
				t.Fatalf("NewKeySchedule: %v", err)
			}
			return []any{&crypto, secretPointer(secrets, "init"), secretPointer(secrets, "commit"),
				secretPointer(secrets, "psk"), context}, schedule
		}},
		{name: "NewKeyScheduleFromJoiner", build: func(t *testing.T) ([]any, any) {
			crypto, context, secrets := callerOwnedScheduleArguments(t)
			schedule, err := NewKeyScheduleFromJoiner(crypto, secrets["joiner"], secrets["psk"], context)
			if err != nil {
				t.Fatalf("NewKeyScheduleFromJoiner: %v", err)
			}
			return []any{&crypto, secretPointer(secrets, "joiner"), secretPointer(secrets, "psk"),
				context}, schedule
		}},
		{name: "NewKeyScheduleFromEpochSecret", build: func(t *testing.T) ([]any, any) {
			crypto, context, secrets := callerOwnedScheduleArguments(t)
			schedule, err := NewKeyScheduleFromEpochSecret(crypto, secrets["epoch"], context)
			if err != nil {
				t.Fatalf("NewKeyScheduleFromEpochSecret: %v", err)
			}
			return []any{&crypto, secretPointer(secrets, "epoch"), context}, schedule
		}},
		{name: "NewSecretTree", build: func(t *testing.T) ([]any, any) {
			crypto, _, secrets := callerOwnedScheduleArguments(t)
			leafCount := LeafCount(8)
			tree, err := NewSecretTree(crypto, leafCount, secrets["encryption"])
			if err != nil {
				t.Fatalf("NewSecretTree: %v", err)
			}
			return []any{&crypto, &leafCount, secretPointer(secrets, "encryption")}, tree
		}},
		{name: "hpkeKeySchedule", build: func(t *testing.T) ([]any, any) {
			params := mustSuiteParams(t)
			shared := bytes.Repeat([]byte{0x31}, params.Nsecret)
			info := []byte("the info this row's context is bound to")
			context, err := hpkeKeySchedule(params, shared, info)
			if err != nil {
				t.Fatalf("hpkeKeySchedule: %v", err)
			}
			return []any{params, &shared, &info}, context
		}},
		{name: "HpkeSetupBaseS", build: func(t *testing.T) ([]any, any) {
			params := mustSuiteParams(t)
			_, pub := mustHpkeKeyPair(t, params)
			info := []byte("the info this row's context is bound to")
			random := constantReader{value: 0x77}
			_, context, err := HpkeSetupBaseS(random, params, pub, info)
			if err != nil {
				t.Fatalf("HpkeSetupBaseS: %v", err)
			}
			return []any{&random, params, &pub, &info}, context
		}},
		{name: "HpkeSetupBaseR", build: func(t *testing.T) ([]any, any) {
			params := mustSuiteParams(t)
			priv, pub := mustHpkeKeyPair(t, params)
			info := []byte("the info this row's context is bound to")
			kemOutput, _, err := HpkeSetupBaseS(constantReader{value: 0x77}, params, pub, info)
			if err != nil {
				t.Fatalf("seal the message this row opens: %v", err)
			}
			context, err := HpkeSetupBaseR(params, priv, kemOutput, info)
			if err != nil {
				t.Fatalf("HpkeSetupBaseR: %v", err)
			}
			return []any{params, &priv, &kemOutput, &info}, context
		}},
		{name: "UnmarshalRatchetTree", build: func(t *testing.T) ([]any, any) {
			encoded := callerOwnedEncodedTree(t)
			tree, err := UnmarshalRatchetTree(encoded)
			if err != nil {
				t.Fatalf("UnmarshalRatchetTree: %v", err)
			}
			return []any{&encoded}, tree
		}},
		// the two the spelling based scope could not see. Neither takes a byte slice: one takes a
		// slice of structures and the other a structure, and the caller's octets are a hop below
		// each of them.
		{name: "ParseRatchetTreeFrom", build: func(t *testing.T) ([]any, any) {
			ext := Extension{ExtensionType: ExtensionTypeRatchetTree,
				ExtensionData: callerOwnedEncodedTree(t)}
			tree, err := ParseRatchetTreeFrom(ext)
			if err != nil {
				t.Fatalf("ParseRatchetTreeFrom: %v", err)
			}
			return []any{&ext}, tree
		}},
		{name: "NewProposalList", build: func(t *testing.T) ([]any, any) {
			order := callerOwnedCommitOrder(t)
			return []any{&order}, NewProposalList(order)
		}},
	}
}

// callerOwnedCommitOrder is the commit order NewProposalList's row hands in: one entry per arm a
// Proposal can carry, with every empty vector inside the populated arm occupied.
//
// ONE ENTRY PER ARM because a Proposal carries exactly one -- checkArm counts them -- while the
// argument TYPE declares a storage route through every arm there is. A single entry would leave
// six of the eight routes unsupplied, and this gate refuses a row that supplies fewer routes than
// its argument types declare, which is what stops a retention behind an arm nobody filled from
// reading as compliant.
//
// The arm is found rather than named: the one non nil pointer field of the Proposal is the arm,
// whatever it is called, so an eighth arm registered later is filled by the commit that adds it.
// Ref and UnknownBody are the two routes that are NOT inside an arm and they are filled by hand --
// UnknownBody only on the entry whose type has no arm, since a body set beside a populated arm is
// two arms to checkArm and a proposal that will not encode.
//
// Every entry must CLONE, and that is a demand on this fixture rather than an observation.
// NewProposalList leaves a proposal it could not encode standing as the caller's own, so a
// fixture carrying one would make this gate report a retention that is really a malformed entry.
func callerOwnedCommitOrder(t *testing.T) []CachedProposal {
	t.Helper()
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	member := testIdentity(t, crypto, "the proposer whose arrays this row follows")
	keyPackage, _, _ := testKeyPackage(t, crypto, member)
	leaf, _ := testLeafNode(t, crypto, member)
	order := []CachedProposal{
		{Proposal: Proposal{ProposalType: ProposalTypeAdd, Add: &Add{KeyPackage: *keyPackage}}},
		{Proposal: Proposal{ProposalType: ProposalTypeUpdate, Update: &Update{LeafNode: *leaf}}},
		{Proposal: Proposal{ProposalType: ProposalTypeRemove, Remove: &Remove{Removed: LeafIndex(1)}}},
		{Proposal: Proposal{ProposalType: ProposalTypePreSharedKey,
			PreSharedKey: &PreSharedKey{Psk: PreSharedKeyId{PskType: PskTypeExternal}}}},
		{Proposal: Proposal{ProposalType: ProposalTypeReInit, ReInit: &ReInit{
			Version: ProtocolVersionMls10, CipherSuite: crypto.Suite()}}},
		{Proposal: Proposal{ProposalType: ProposalTypeExternalInit, ExternalInit: &ExternalInit{}}},
		{Proposal: Proposal{ProposalType: ProposalTypeGroupContextExtensions,
			GroupContextExtensions: &GroupContextExtensions{}}},
		{Proposal: Proposal{ProposalType: callerArrayUnregisteredProposalType,
			UnknownBody: []byte("the verbatim body of a type this build does not register")}},
	}
	for at := range order {
		proposal := reflect.ValueOf(&order[at].Proposal).Elem()
		for field := range proposal.NumField() {
			arm := proposal.Field(field)
			if arm.Kind() == reflect.Pointer && !arm.IsNil() {
				growEveryEmptySlice(arm.Elem(), "")
			}
		}
		order[at].Ref = ProposalRef(bytes.Repeat([]byte{byte(0x40 + at)}, 32))
		order[at].Sender = LeafIndex(at)
		// every entry must survive the clone, or the retention this gate reports is this
		// fixture's and not the constructor's
		if _, _, err := cloneProposal(&order[at].Proposal); err != nil {
			t.Fatalf("entry %d of this row's commit order does not encode, so NewProposalList would keep the caller's own copy of it: %v",
				at, err)
		}
	}
	return order
}

// The code point the unknown arm's entry is written under. It is asserted unregistered where it is
// used, so a build that registers it fails rather than filling that route with a known type.
const callerArrayUnregisteredProposalType = ProposalType(0x0A0A)

// TestNoConstructionOfSealedStorageRetainsItsCallersArrays is the retention half, over every member
// of the derived class.
//
// Three demands per row, and the first two are what keep the third from passing vacuously: the
// arguments must cover the arity the source declares, and they must carry an array down every route
// the argument TYPES declare storage at. Only then is "these two share no octet" a statement about
// the construction rather than about what this row happened to hand it.
func TestNoConstructionOfSealedStorageRetainsItsCallersArrays(t *testing.T) {
	covered := []string{}
	for _, row := range callerArrayRows() {
		covered = append(covered, row.name)
		arguments, made := row.build(t)
		if want := packageLevelFunctionArity(t, row.name); len(arguments) != want {
			t.Errorf("%s: this row hands in %d arguments and the source declares %d, so it covers a signature that has moved on",
				row.name, len(arguments), want)
		}
		owned := newByteStorageFinder(byteStorageOutside)
		declared := []string{}
		for at, argument := range arguments {
			how := fmt.Sprintf("%s argument %d", row.name, at)
			owned.walk(reflect.ValueOf(argument), how, how, 0)
			declared = append(declared, byteStoragePathsOf(reflect.TypeOf(argument), how)...)
		}
		slices.Sort(declared)
		declared = slices.Compact(declared)
		if len(declared) == 0 {
			t.Errorf("%s: no argument type of this row declares byte storage, so this row demands nothing", row.name)
		}
		if len(owned.unfollowed) != 0 {
			t.Errorf("%s: the walk over its arguments could not follow %v", row.name, owned.unfollowed)
		}
		supplied := owned.paths()
		for _, route := range declared {
			if !slices.Contains(supplied, route) {
				t.Errorf("%s: the arguments this row hands in carry no storage at %s, which the argument types declare; a construction retaining an array there would pass over unread",
					row.name, route)
			}
		}
		for _, route := range supplied {
			if !slices.Contains(declared, route) {
				t.Errorf("%s: this row hands in storage at %s, which the argument types do not declare; the type walk and the value walk have stopped agreeing",
					row.name, route)
			}
		}
		held := byteStorageFoundIn(reflect.ValueOf(made), row.name+" made", byteStorageInside)
		if len(held.unfollowed) != 0 {
			t.Errorf("%s: the walk over what it made could not follow %v, so it would walk past a retention behind the same shape",
				row.name, held.unfollowed)
		}
		if len(held.found) == 0 {
			t.Errorf("%s: what it made reaches no storage at all, so this row compared nothing", row.name)
		}
		for _, mine := range owned.found {
			for _, theirs := range held.found {
				if mine.overlaps(theirs) {
					t.Errorf("%s retains %s, which is the caller's own %s: a write through one is a write through the other",
						row.name, theirs.how, mine.how)
				}
			}
		}
	}
	// and the rows are the class read off the source rather than the constructions somebody
	// thought of
	declared := packageConstructionsOfSealedStorage(t)
	slices.Sort(covered)
	if !slices.Equal(covered, declared) {
		t.Errorf("this gate runs over %v, and the package's constructions of sealed storage over caller bytes are %v",
			covered, declared)
	}
}

// ---------------------------------------------------------------------------
// the arguments the rows hand in
// ---------------------------------------------------------------------------

// callerOwnedGroupArguments builds the three arguments NewGroup takes, filled so that every route
// the argument TYPES declare storage down carries a distinct run of octets.
//
// Distinct rather than convenient: the gate compares extents, so two arguments cut from one buffer
// would make a finding about the wrong one.
func callerOwnedGroupArguments(t *testing.T) (*GroupConfig, SignaturePrivateKey, Credential) {
	t.Helper()
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	identity := []byte("the founder whose arrays this gate follows")
	policy := &GroupPolicyExtension{Roles: []RoleEntry{{MemberId: identity, Role: RoleOwner}}}
	if err := policy.Canonicalize(); err != nil {
		t.Fatalf("canonicalize the policy this group is founded under: %v", err)
	}
	policyExt, err := policy.Encode()
	if err != nil {
		t.Fatalf("encode the policy this group is founded under: %v", err)
	}
	cfg := &GroupConfig{
		Suite:      CipherSuiteX25519ChaCha20Sha256Ed25519,
		GroupId:    []byte("the group id this gate follows"),
		Extensions: []Extension{policyExt},
		Crypto:     crypto,
		Store:      newTestStore(),
		LeafKeys: LeafKeysExtension{
			AlgId:          AlgIdXwing,
			DeviceXwingPub: bytes.Repeat([]byte{0x62}, XwingPublicKeyLen),
		},
	}
	return cfg, SignaturePrivateKey(bytes.Repeat([]byte{0x54}, 32)),
		Credential{CredentialType: CredentialTypeBasic, Identity: bytes.Clone(identity)}
}

// callerOwnedJoinArguments builds the four arguments JoinFromWelcome takes, out of a real commit,
// so that every route the argument TYPES declare storage down carries an array this gate can follow.
//
// TWO OF THOSE ROUTES ARE EMPTY IN AN ORDINARY KEY PACKAGE and they are filled here on purpose,
// because a route this row leaves empty is a route a retention could hide behind:
//
//   - KeyPackage.Extensions is inside the KeyPackageTBS, so it is filled BEFORE the Add is
//     committed and the key package is re-signed over it. Filling it afterwards would change the
//     key package REFERENCE, and the Welcome names the reference this joiner published;
//   - LeafNode.ParentHash is filled after, and can be, because a key_package sourced leaf does not
//     encode it at all -- marshalCore writes the lifetime for that source and the parent hash only
//     for a commit sourced one. So the octets are reachable from the argument and are in neither
//     the reference nor the leaf encoding the join compares against the tree.
func callerOwnedJoinArguments(t *testing.T) (*GroupConfig, []byte, []byte, *JoinKeyMaterial) {
	t.Helper()
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	owner := testIdentity(t, crypto, "the founder whose group this gate joins")
	group := testNewGroup(t, crypto, owner, "the group this gate joins")
	t.Cleanup(func() { group.Close() })

	joiner := testIdentity(t, crypto, "the joiner whose arrays this gate follows")
	keyPackage, initPriv, encPriv := testKeyPackage(t, crypto, joiner)
	keyPackage.Extensions = []Extension{{
		ExtensionType: ExtensionTypeApplicationId,
		ExtensionData: []byte("the key package extension body this gate follows"),
	}}
	content, err := keyPackage.signedPreimage()
	if err != nil {
		t.Fatalf("the preimage of the key package this row publishes: %v", err)
	}
	signature, err := crypto.SignWithLabel(joiner.SigPriv, keyPackageSignatureLabel, content)
	if err != nil {
		t.Fatalf("re-sign the key package this row publishes: %v", err)
	}
	keyPackage.Signature = signature

	result, err := group.CreateCommit(nil,
		[]Proposal{{ProposalType: ProposalTypeAdd, Add: &Add{KeyPackage: *keyPackage}}}, nil)
	if err != nil {
		t.Fatalf("commit the add this row's join follows: %v", err)
	}
	if err := group.MergePendingCommit(); err != nil {
		t.Fatalf("merge the add this row's join follows: %v", err)
	}
	keys := &JoinKeyMaterial{
		KeyPackage:     *keyPackage,
		InitPrivate:    initPriv,
		EncryptPrivate: encPriv,
		SignPrivate:    SignaturePrivateKey(bytes.Clone(joiner.SigPriv)),
	}
	keys.KeyPackage.LeafNode.ParentHash = []byte("the parent hash a key_package leaf does not encode")
	return testGroupConfig(t, crypto, joiner, "the group this gate joins"),
		result.Welcome, result.RatchetTree, keys
}

// callerOwnedScheduleArguments builds the provider, the group context and the six secrets the key
// schedule and secret tree rows draw from, every one of them a distinct run at KDF.Nh so no finding
// can be about the wrong array.
func callerOwnedScheduleArguments(t *testing.T) (CryptoProvider, *GroupContext, map[string][]byte) {
	t.Helper()
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	context := &GroupContext{
		Version:                 ProtocolVersionMls10,
		CipherSuite:             crypto.Suite(),
		GroupId:                 []byte("the group id these schedules are bound to"),
		Epoch:                   3,
		TreeHash:                bytes.Repeat([]byte{0x11}, crypto.HashSize()),
		ConfirmedTranscriptHash: bytes.Repeat([]byte{0x12}, crypto.HashSize()),
		Extensions: []Extension{{ExtensionType: ExtensionTypeUrmessageGroupPolicy,
			ExtensionData: []byte("the extension body these schedules are bound to")}},
	}
	secrets := map[string][]byte{}
	for at, name := range []string{"init", "commit", "psk", "joiner", "epoch", "encryption"} {
		secrets[name] = bytes.Repeat([]byte{byte(0x71 + at)}, crypto.HashSize())
	}
	return crypto, context, secrets
}

// secretPointer hands a row's secret in as a pointer to the map's own storage, so the walk reaches
// the very slice the construction was given rather than a copy of its header.
func secretPointer(secrets map[string][]byte, name string) *[]byte {
	held := secrets[name]
	return &held
}

func mustSuiteParams(t *testing.T) *SuiteParams {
	t.Helper()
	params, err := LookupSuite(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("look up the suite these rows are built over: %v", err)
	}
	return params
}

func mustHpkeKeyPair(t *testing.T, params *SuiteParams) (HpkePrivateKey, HpkePublicKey) {
	t.Helper()
	priv, pub, err := HpkeDeriveKeyPair(params, bytes.Repeat([]byte{0x32}, params.Nsk))
	if err != nil {
		t.Fatalf("derive the key pair these rows are built over: %v", err)
	}
	return priv, pub
}

// callerOwnedEncodedTree is a real one leaf tree encoded, since UnmarshalRatchetTree refuses a tree
// that holds nothing and a row that fed it one would be reporting a refusal rather than a decode.
func callerOwnedEncodedTree(t *testing.T) []byte {
	t.Helper()
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	_, encPub, err := crypto.DeriveKeyPair(crypto.Random(crypto.HashSize()))
	if err != nil {
		t.Fatalf("the encryption key of the leaf this row's tree carries: %v", err)
	}
	signer, _, err := crypto.SignatureKeyPair()
	if err != nil {
		t.Fatalf("the signature key of the leaf this row's tree carries: %v", err)
	}
	keys := LeafKeysExtension{AlgId: AlgIdXwing,
		DeviceXwingPub: bytes.Repeat([]byte{0x63}, XwingPublicKeyLen)}
	keysExt, err := keys.Encode()
	if err != nil {
		t.Fatalf("the leaf keys of the leaf this row's tree carries: %v", err)
	}
	leaf, err := NewLeafNode(crypto, signer,
		Credential{CredentialType: CredentialTypeBasic, Identity: []byte("the leaf in this row's tree")},
		encPub, v1Capabilities(), []Extension{keysExt})
	if err != nil {
		t.Fatalf("the leaf this row's tree carries: %v", err)
	}
	tree := NewRatchetTree()
	if _, err := tree.AddLeaf(leaf); err != nil {
		t.Fatalf("add the leaf this row's tree carries: %v", err)
	}
	encoded, err := marshalRatchetTree(tree)
	if err != nil {
		t.Fatalf("encode this row's tree: %v", err)
	}
	return encoded
}

// ---------------------------------------------------------------------------
// the hand-out half, over what a group ANSWERS -- see the header's line 6 for its scope
// ---------------------------------------------------------------------------

// groupAnswerRow is one exported method of *Group, called for what it answers.
//
// A row supplies the arguments a method needs and NOTHING else -- no refusal is turned into a
// t.Fatal, because both gates below call a row twice and the second call is made over a group whose
// caller has just scribbled on its own arrays. A method that starts refusing there is a finding and
// not a reason to stop reading.
type groupAnswerRow struct {
	name string
	call func(group *Group) []any

	// publishes is the part of what this row answers that a group founded on the SAME caller
	// arguments is entitled to answer identically, and it is nil for every row whose WHOLE answer
	// is that.
	//
	// The four proposal generators are the reason it exists. A proposal is sealed as an RFC 9420
	// section 6.3 PrivateMessage: an AEAD ciphertext under a fresh reuse guard and a spent
	// generation of the sender's own ratchet, so no two answers of one generator are equal and an
	// equality over the whole of one says nothing about anything. What a generator publishes IN
	// CLEARTEXT is the section 6.3 header, and the group id in it is exactly the octets a caller
	// that kept its own array would corrupt. See publishingRowAcrossTwoFoundings.
	publishes func(answers []any) []any
}

// groupProposalHeader is the projection the four generator rows are read through: the CLEARTEXT
// group id of the PrivateMessage a generator answered.
//
// Section 6.3 leaves the group id outside the AEAD so that a receiver can pick the group before it
// has decrypted anything, which is what makes it the one octet run of a proposal message two
// foundings over one group id agree on. Everything else in the answer is ciphertext.
//
// A row that answered no message at all projects to nothing, and the gate below fails such a row
// rather than passing it: a generator driven into a refusal is a row that observed nothing.
func groupProposalHeader(answers []any) []any {
	out := []any{}
	for _, answer := range answers {
		encoded, isBytes := answer.(*[]byte)
		if !isBytes || len(*encoded) == 0 {
			continue
		}
		parsed, err := ParseMLSMessage(*encoded)
		if err != nil || parsed.PrivateMessage == nil {
			continue
		}
		header := parsed.PrivateMessage.GroupId
		out = append(out, &header)
	}
	return out
}

// groupCommitHeader is the projection the commit row is read through: the CLEARTEXT group id of
// the PrivateMessage a commit answered.
//
// groupProposalHeader's reasoning over a different answer shape. A commit answers a *CommitResult
// rather than the octets, so the message has to be reached through the struct -- and the tree
// beside it is deliberately NOT projected: it is a fresh encoding of a tree whose parent hashes and
// leaf keys are drawn per commit, so two foundings do not agree on it and an equality over it would
// state nothing.
func groupCommitHeader(answers []any) []any {
	out := []any{}
	for _, answer := range answers {
		held, isResult := answer.(**CommitResult)
		if !isResult || *held == nil || len((*held).Commit) == 0 {
			continue
		}
		parsed, err := ParseMLSMessage((*held).Commit)
		if err != nil || parsed.PrivateMessage == nil {
			continue
		}
		header := parsed.PrivateMessage.GroupId
		out = append(out, &header)
	}
	return out
}

// groupAnswerKeyPackage mints the key package the ProposeAdd row admits, under the GROUP'S OWN
// provider so that the suite it names is the suite the group runs.
func groupAnswerKeyPackage(t *testing.T, group *Group) []byte {
	t.Helper()
	kp, _, _ := testKeyPackage(t, group.crypto, testIdentity(t, group.crypto, "the joiner this gate adds"))
	encoded, err := syntax.Marshal(kp)
	if err != nil {
		t.Fatalf("marshal the key package this gate adds: %v", err)
	}
	return encoded
}

// groupAnswerSecondLeaf splices one more member into the group's ratchet tree and answers its leaf,
// so that the ProposeRemove row has a member to name.
//
// The RATCHET tree and not the secret tree, because a remove reads only the first: the message is
// sealed under OUR leaf's ratchet whichever leaf it names.
func groupAnswerSecondLeaf(t *testing.T, group *Group) LeafIndex {
	t.Helper()
	leaf, _ := testLeafNode(t, group.crypto, testIdentity(t, group.crypto, "the member this gate removes"))
	at, err := group.tree.AddLeaf(leaf)
	if err != nil {
		t.Fatalf("splice a second leaf into the group this gate follows: %v", err)
	}
	return at
}

// groupAnswerPairwiseLeaf splices ONE peer leaf into the ratchet tree and answers its position, so
// that the PairwiseExport row has a member to derive a key with.
//
// IT IS IDEMPOTENT AND groupAnswerSecondLeaf IS NOT, which is the whole reason it exists rather
// than the row calling that one. PairwiseExport carries no projection, so
// TestAGroupGoesOnPublishingWhatItWasFoundedOn asks its row TWICE over one group and requires the
// two answers to be equal; a helper that spliced a freshly drawn leaf on every call would put a
// different public point in front of the second derivation and the row would report a finding
// against a method that is behaving exactly as specified.
//
// A SPLICED LEAF AND NOT A REAL PEER, which is the other half of the same requirement:
// groupAnswerPeer commits, so it moves the epoch -- and the epoch is IN this derivation's context,
// so two calls either side of it answer two different keys for the same reason. What this
// derivation reads off the tree is the peer's public encryption point and nothing else, so a
// spliced leaf is a complete peer as far as this method is concerned.
func groupAnswerPairwiseLeaf(t *testing.T, group *Group) LeafIndex {
	t.Helper()
	for at := LeafIndex(0); LeafCount(at) < group.tree.LeafWidth(); at += 1 {
		if at != group.ownLeaf && group.tree.Leaf(at) != nil {
			return at
		}
	}
	leaf, _ := testLeafNode(t, group.crypto,
		testIdentity(t, group.crypto, "the peer this gate derives a pairwise key with"))
	at, err := group.tree.AddLeaf(leaf)
	if err != nil {
		t.Fatalf("splice a peer leaf into the group this gate follows: %v", err)
	}
	return at
}

// groupAnswerStagedCommit stages a commit on the group this gate follows when none is staged, so
// that the five Pending rows have a staged epoch to read.
//
// IT IS IDEMPOTENT, for groupAnswerPairwiseLeaf's reason: the five rows carry no projection, so
// TestAGroupGoesOnPublishingWhatItWasFoundedOn asks each of them TWICE over one group and requires
// the two answers to be equal, and a helper that staged a fresh commit per call would put a
// second epoch -- fresh path secrets, a fresh tree hash, a fresh exporter -- in front of the
// second read, and the row would report a finding against a method that is behaving exactly as
// specified. The staged commit is the one shape a group with no peers can build: an empty
// proposal list, which RFC 9420 section 12.4 requires a path for.
func groupAnswerStagedCommit(t *testing.T, group *Group) {
	t.Helper()
	group.stateLock.Lock()
	staged := group.pending != nil
	group.stateLock.Unlock()
	if staged {
		return
	}
	if _, err := group.CreateCommit(nil, nil, nil); err != nil {
		t.Fatalf("stage the commit this gate's Pending rows read: %v", err)
	}
}

// groupAnswerPeer adds a real second member to the group this gate follows and answers that
// member's own group, so that the three INBOUND rows have a message their group would accept.
//
// A REAL PEER AND NOT A SPLICED LEAF, which is groupAnswerSecondLeaf's shape and cannot work here.
// An inbound row is driven with a message sealed under the SENDER's ratchet, and only a member that
// actually derived this epoch's encryption secret has one -- so a spliced leaf produces nothing the
// receive path can open, and a row driven into that refusal answers nothing and observes nothing.
//
// The group is advanced by one epoch as a side effect, which every row is allowed to do:
// MergePendingCommit's and ClearPendingCommit's rows commit too, and each row is given a group of
// its own.
func groupAnswerPeer(t *testing.T, group *Group) *Group {
	t.Helper()
	crypto := group.crypto
	peer := testIdentity(t, crypto, "the peer this gate's inbound rows are driven with")
	kp, initPriv, encPriv := testKeyPackage(t, crypto, peer)
	encoded, err := syntax.Marshal(kp)
	if err != nil {
		t.Fatalf("marshal the key package this gate's peer joins with: %v", err)
	}
	if _, err := group.ProposeAdd(encoded); err != nil {
		t.Fatalf("propose the add this gate's peer joins on: %v", err)
	}
	result, err := group.CreateCommit(nil, nil, nil)
	if err != nil {
		t.Fatalf("commit the add this gate's peer joins on: %v", err)
	}
	if err := group.MergePendingCommit(); err != nil {
		t.Fatalf("merge the add this gate's peer joins on: %v", err)
	}
	cfg := testGroupConfig(t, crypto, peer, string(group.GroupId()))
	joined, err := JoinFromWelcome(cfg, result.Welcome, result.RatchetTree, &JoinKeyMaterial{
		KeyPackage:     *kp,
		InitPrivate:    initPriv,
		EncryptPrivate: encPriv,
		SignPrivate:    peer.SigPriv,
	})
	if err != nil {
		t.Fatalf("join this gate's peer to the group it follows: %v", err)
	}
	t.Cleanup(func() { joined.Close() })
	return joined
}

func groupAnswerRows(t *testing.T) []groupAnswerRow {
	return []groupAnswerRow{
		{name: "GroupId", call: func(group *Group) []any {
			answer := group.GroupId()
			return []any{&answer}
		}},
		{name: "Epoch", call: func(group *Group) []any {
			answer := group.Epoch()
			return []any{&answer}
		}},
		{name: "OwnLeafIndex", call: func(group *Group) []any {
			answer := group.OwnLeafIndex()
			return []any{&answer}
		}},
		{name: "OwnLeafNodeCopy", call: func(group *Group) []any {
			answer := group.OwnLeafNodeCopy()
			return []any{&answer}
		}},
		{name: "Members", call: func(group *Group) []any {
			answer := group.Members()
			return []any{&answer}
		}},
		{name: "MemberAt", call: func(group *Group) []any {
			answer, found := group.MemberAt(group.OwnLeafIndex())
			return []any{&answer, &found}
		}},
		{name: "EpochAuthenticator", call: func(group *Group) []any {
			answer := group.EpochAuthenticator()
			return []any{&answer}
		}},
		{name: "Export", call: func(group *Group) []any {
			answer, err := group.Export("the exporter label this gate reads",
				[]byte("the exporter context this gate reads"), 32)
			return []any{&answer, &err}
		}},
		// PairwiseExport needs a SECOND LEAF to name, which the group this gate follows does not
		// have, so the row splices one -- and it splices it through a helper that is IDEMPOTENT
		// rather than through groupAnswerSecondLeaf. See groupAnswerPairwiseLeaf for why: this row
		// carries no projection, so TestAGroupGoesOnPublishingWhatItWasFoundedOn asks it TWICE over
		// one group and compares the two answers, and a row that drew a fresh peer key per call
		// would answer two different keys from a group that is entirely correct.
		{name: "PairwiseExport", call: func(group *Group) []any {
			answer, err := group.PairwiseExport("the pairwise exporter label this gate reads",
				groupAnswerPairwiseLeaf(t, group), 32)
			return []any{&answer, &err}
		}},
		// both names of the closed enum, since the two arms read two different secrets and a row
		// reading one of them would pass over the other
		{name: "EpochSecret", call: func(group *Group) []any {
			senderData, senderErr := group.EpochSecret(EpochSecretSenderData)
			encryption, encryptionErr := group.EpochSecret(EpochSecretEncryption)
			return []any{&senderData, &senderErr, &encryption, &encryptionErr}
		}},
		{name: "RatchetTree", call: func(group *Group) []any {
			answer, err := group.RatchetTree()
			return []any{&answer, &err}
		}},
		{name: "GroupContext", call: func(group *Group) []any {
			answer, err := group.GroupContext()
			return []any{&answer, &err}
		}},
		{name: "GroupPolicy", call: func(group *Group) []any {
			answer, err := group.GroupPolicy()
			return []any{&answer, &err}
		}},
		// THE FOUR GENERATORS, each driven with the arguments a caller would use rather than into
		// a refusal. groupAnswerDeclaresStorage reads their signatures as declaring byte storage,
		// so a row driven into a refusal answers none and is reported as having observed nothing --
		// which is the right report and not a shape to design around.
		{name: "ProposeAdd", call: func(group *Group) []any {
			answer, err := group.ProposeAdd(groupAnswerKeyPackage(t, group))
			return []any{&answer, &err}
		}, publishes: groupProposalHeader},
		{name: "ProposeUpdate", call: func(group *Group) []any {
			answer, err := group.ProposeUpdate()
			return []any{&answer, &err}
		}, publishes: groupProposalHeader},
		{name: "ProposeRemove", call: func(group *Group) []any {
			answer, err := group.ProposeRemove(groupAnswerSecondLeaf(t, group))
			return []any{&answer, &err}
		}, publishes: groupProposalHeader},
		{name: "ProposeGroupContextExtensions", call: func(group *Group) []any {
			answer, err := group.ProposeGroupContextExtensions(testGroupContextOf(t, group).Extensions)
			return []any{&answer, &err}
		}, publishes: groupProposalHeader},
		// THE COMMIT LIFECYCLE, one row per exported method it lands.
		//
		// The commit row is driven with an EMPTY proposal list, which is the one shape a group
		// with no peers can build and which RFC 9420 section 12.4 requires a path for -- so the
		// answer carries the commit message, the post-commit tree, and the update path inside the
		// first of those. It is read through a projection for the four generators' reason: a
		// commit travels as a section 6.3 PrivateMessage, so no two answers of it are equal and
		// the one octet run two foundings over one group id agree on is the cleartext group id.
		//
		// The other two answer no octets at all, which groupAnswerDeclaresStorage reads off their
		// signatures, and both are driven past a commit rather than into ErrNoPendingCommit: a row
		// that took the refusal would leave the group where it started and observe nothing about
		// the state the merge installs.
		{name: "CreateCommit", call: func(group *Group) []any {
			answer, err := group.CreateCommit(nil, nil, nil)
			return []any{&answer, &err}
		}, publishes: groupCommitHeader},
		{name: "MergePendingCommit", call: func(group *Group) []any {
			_, commitErr := group.CreateCommit(nil, nil, nil)
			err := group.MergePendingCommit()
			return []any{&commitErr, &err}
		}},
		{name: "ClearPendingCommit", call: func(group *Group) []any {
			_, commitErr := group.CreateCommit(nil, nil, nil)
			group.ClearPendingCommit()
			return []any{&commitErr}
		}},
		// THE FIVE READS OFF A STAGED COMMIT, ledger item 242's R2 (2026-09-22) and item 257's
		// ruling 51 (2026-09-25): what a committer announces before the delivery service has
		// answered. Each is driven PAST a staged commit rather than into ErrNoPendingCommit, for
		// the lifecycle rows' reason, and through the idempotent stager so that the publishing
		// gate's two calls read one staged epoch. The two that answer octets -- the marshalled
		// staged context and the staged exporter -- are the two a caller could write through, and
		// both are fresh: Marshal copies, and an exporter is a derivation. PendingRemovedLeaves
		// answers a slice of a scalar type and no byte storage at all, and it is swept for the
		// same property the other four are: RemovedLeaves copies, so the vector it hands out is
		// not the staged commit's own.
		{name: "PendingEpoch", call: func(group *Group) []any {
			groupAnswerStagedCommit(t, group)
			answer, err := group.PendingEpoch()
			return []any{&answer, &err}
		}},
		{name: "PendingMemberCount", call: func(group *Group) []any {
			groupAnswerStagedCommit(t, group)
			answer, err := group.PendingMemberCount()
			return []any{&answer, &err}
		}},
		{name: "PendingRemovedLeaves", call: func(group *Group) []any {
			groupAnswerStagedCommit(t, group)
			answer, err := group.PendingRemovedLeaves()
			return []any{&answer, &err}
		}},
		{name: "PendingGroupContext", call: func(group *Group) []any {
			groupAnswerStagedCommit(t, group)
			answer, err := group.PendingGroupContext()
			return []any{&answer, &err}
		}},
		{name: "PendingExport", call: func(group *Group) []any {
			groupAnswerStagedCommit(t, group)
			answer, err := group.PendingExport("the exporter label this gate reads",
				[]byte("the exporter context this gate reads"), 32)
			return []any{&answer, &err}
		}},
		// THE MESSAGE LIFECYCLE, p7 task 18's four exported methods.
		//
		// Protect answers a section 6.3 PrivateMessage, so it is read through the same projection
		// the four generators are and for the same reason: no two seals of one plaintext are equal,
		// and the one octet run two foundings over a group id agree on is the cleartext header.
		//
		// The three inbound rows are driven with a message from a REAL peer, because a group cannot
		// open its own: SealPrivateMessage CONSUMES the generation it seals under, so a group handed
		// back its own message finds the ratchet entry spent. See groupAnswerPeer.
		{name: "Protect", call: func(group *Group) []any {
			answer, err := group.Protect([]byte("the aad this gate reads"),
				[]byte("the plaintext this gate reads"))
			return []any{&answer, &err}
		}, publishes: groupProposalHeader},
		// ProtectBound is Protect for MASTER section 8.4.2 v2's aad, which NAMES the generation
		// the frame is sealed at and therefore cannot be a value the caller passes. The builder's
		// answer is the caller's own array on exactly Protect's terms -- read once into the
		// signature preimage and into the AEAD -- so it is swept the same way and read through the
		// same projection.
		{name: "ProtectBound", call: func(group *Group) []any {
			answer, err := group.ProtectBound(func(generation uint32) ([]byte, error) {
				return []byte("the aad this gate reads"), nil
			}, []byte("the plaintext this gate reads"))
			return []any{&answer, &err}
		}, publishes: groupProposalHeader},
		{name: "ProcessMessage", call: func(group *Group) []any {
			peer := groupAnswerPeer(t, group)
			message, protectErr := peer.Protect([]byte("the aad this gate reads"),
				[]byte("the plaintext this gate reads"))
			answer, err := group.ProcessMessage(message)
			return []any{&answer, &err, &protectErr}
		}},
		{name: "Unprotect", call: func(group *Group) []any {
			peer := groupAnswerPeer(t, group)
			message, protectErr := peer.Protect([]byte("the aad this gate reads"),
				[]byte("the plaintext this gate reads"))
			answer, err := group.Unprotect(message)
			return []any{&answer, &err, &protectErr}
		}},
		// ApplyCommit answers an error alone, which groupAnswerDeclaresStorage reads off its
		// signature, and it is driven PAST a staged commit rather than into its refusal: a row that
		// took the refusal would leave the group where it started and observe nothing about the
		// state the merge installs.
		{name: "ApplyCommit", call: func(group *Group) []any {
			peer := groupAnswerPeer(t, group)
			result, commitErr := peer.CreateCommit(nil, nil, nil)
			if commitErr != nil {
				t.Fatalf("commit the epoch this gate's ApplyCommit row applies: %v", commitErr)
			}
			processed, processErr := group.ProcessMessage(result.Commit)
			if processErr != nil {
				t.Fatalf("process the commit this gate's ApplyCommit row applies: %v", processErr)
			}
			err := group.ApplyCommit(processed)
			return []any{&commitErr, &processErr, &err}
		}},
		// the ender gets a row like every other method, and every row gets a group of its own, so
		// there is no member of the class this gate skipped for being inconvenient
		{name: "Close", call: func(group *Group) []any {
			answer := group.Close()
			return []any{&answer}
		}},
	}
}

// answerStorageOf walks what one row answered.
func answerStorageOf(row groupAnswerRow, group *Group) *byteStorageFinder {
	finder := newByteStorageFinder(byteStorageAnswer)
	for at, answer := range row.call(group) {
		how := fmt.Sprintf("%s() result %d", row.name, at)
		finder.walk(reflect.ValueOf(answer), how, how, 0)
	}
	return finder
}

// TestNoAnswerOfAGroupSharesStorageWithIt is the hand-out half, and Members() is what it was written
// for: IdentityPub was cloned and SignatureKey, two fields of one struct three lines apart, was a
// window onto the LIVE ratchet tree. A caller writing through that window edits the tree this
// epoch's tree hash was taken over, and nothing else in this package reports it, because the tree
// goes on being self consistent with the octets it now holds.
//
// A row is refused as vacuous when its method's RESULT TYPES declare byte storage and the call
// produced none, so a method whose answer stopped carrying anything cannot read as compliant. That
// demand is derived from the signature rather than from a list of which methods answer bytes.
//
// THE GROUP IS WALKED TWICE, BEFORE THE CALL AND AFTER IT, and the second walk is the one this
// test was reopened for. Taking the snapshot only beforehand asks "is this answer storage the
// group ALREADY held", which is silent about storage the group STARTS holding at answer time: a
// method that clones its answer and then files the clone away on itself hands out an array that
// was in neither the group nor the caller when the walk ran, and the finding was measured -- such
// a method left 7286 tests green. It is not a hypothetical shape either. Memoising the marshalled
// bytes GroupContext() builds is the obvious next optimisation for the tasks that follow this
// one, and it lands exactly here: the first call would answer fresh storage and keep it, and every
// call after that would hand out the group's own.
func TestNoAnswerOfAGroupSharesStorageWithIt(t *testing.T) {
	for _, row := range groupAnswerRows(t) {
		group := mustFoundedGroup(t)
		held := byteStorageFoundIn(reflect.ValueOf(group), "group", byteStorageInside)
		if len(held.unfollowed) != 0 {
			t.Errorf("%s: the walk over the group could not follow %v", row.name, held.unfollowed)
		}
		if len(held.found) == 0 {
			t.Fatalf("%s: the group reached no storage at all, so this row compared nothing", row.name)
		}
		answered := answerStorageOf(row, group)
		// what the group holds once the answer has been made. Close() is a row like any other
		// and it drops the schedule and the secret tree, so this snapshot is allowed to be
		// smaller than the one above -- what it must not be is empty, or the second comparison
		// would be the clean run of a comparison that ran over nothing.
		kept := byteStorageFoundIn(reflect.ValueOf(group), "group after the call", byteStorageInside)
		if len(kept.unfollowed) != 0 {
			t.Errorf("%s: the walk over the group after the call could not follow %v", row.name, kept.unfollowed)
		}
		if len(kept.found) == 0 {
			t.Fatalf("%s: the group reached no storage after the call, so the second comparison observed nothing",
				row.name)
		}
		if len(answered.unfollowed) != 0 {
			t.Errorf("%s: the walk over its answer could not follow %v", row.name, answered.unfollowed)
		}
		if len(answered.found) == 0 && groupAnswerDeclaresStorage(t, row.name) {
			t.Errorf("%s answers types that declare byte storage and this call produced none, so this row observed nothing",
				row.name)
		}
		for _, given := range answered.found {
			for _, snapshot := range []*byteStorageFinder{held, kept} {
				for _, theirs := range snapshot.found {
					if given.overlaps(theirs) {
						t.Errorf("%s hands out %s, which is the group's own %s: a caller writing through what it was handed writes into the group",
							row.name, given.how, theirs.how)
					}
				}
			}
		}
	}
	// and the rows are *Group's method set rather than the methods somebody thought of
	covered := []string{}
	for _, row := range groupAnswerRows(t) {
		covered = append(covered, row.name)
	}
	declared := []string{}
	groupType := reflect.TypeOf((*Group)(nil))
	for at := range groupType.NumMethod() {
		declared = append(declared, groupType.Method(at).Name)
	}
	slices.Sort(declared)
	slices.Sort(covered)
	if len(declared) == 0 {
		t.Fatal("*Group exports no method, so this gate demands nothing")
	}
	if !slices.Equal(covered, declared) {
		t.Errorf("this gate calls %v, and *Group exports %v", covered, declared)
	}
}

// TestAGroupGoesOnPublishingWhatItWasFoundedOn is the same property read through what a group SAYS
// rather than through what it holds, and it is here because it is the failure a person meets.
//
// Every octet the caller owns is overwritten after the group is founded, and then the group is
// asked again. A group that copied what it was handed answers exactly what it answered before; the
// group this file was written against answered a context whose policy body was the scribble, and
// GroupPolicy() refused it with "varint prefix 0b11 is reserved" -- over a group founded under a
// policy that was perfectly good, with its epoch secrets still derived over the octets as they
// were.
//
// The class of things asked is the SAME rows the hand-out gate uses, so there is one list of what a
// group publishes rather than two that can drift apart.
func TestAGroupGoesOnPublishingWhatItWasFoundedOn(t *testing.T) {
	for _, row := range groupAnswerRows(t) {
		// a row whose answer is fresh on every call cannot be read by asking one group twice; see
		// publishingRowAcrossTwoFoundings for what replaces the two calls and why
		if row.publishes != nil {
			publishingRowAcrossTwoFoundings(t, row)
			continue
		}
		cfg, signer, cred := callerOwnedGroupArguments(t)
		owned := newByteStorageFinder(byteStorageOutside)
		owned.walk(reflect.ValueOf(cfg), "cfg", "cfg", 0)
		owned.walk(reflect.ValueOf(&signer), "signer", "signer", 0)
		owned.walk(reflect.ValueOf(&cred), "cred", "cred", 0)
		if len(owned.found) == 0 {
			t.Fatalf("%s: the walk over this group's arguments reached no storage, so this row scribbled nothing", row.name)
		}
		group, err := NewGroup(cfg, signer, cred)
		if err != nil {
			t.Fatalf("%s: found the group this row follows: %v", row.name, err)
		}
		before := answerStorageOf(row, group).contents()
		for at, mine := range owned.found {
			mine.scribble(byte(at) | 0x80)
		}
		after := answerStorageOf(row, group).contents()
		for _, route := range slices.Sorted(maps.Keys(before)) {
			if !slices.Equal(before[route], after[route]) {
				t.Errorf("%s answered %v at %s before its caller wrote into its own arrays and %v afterwards",
					row.name, before[route], route, after[route])
			}
		}
		for _, route := range slices.Sorted(maps.Keys(after)) {
			if _, answeredBefore := before[route]; !answeredBefore {
				t.Errorf("%s answered nothing at %s before its caller wrote into its own arrays and %v afterwards",
					row.name, route, after[route])
			}
		}
		group.Close()
	}
}

// publishedStorageOf walks what one row PUBLISHES: its whole answer for a row with no projection,
// and the projection for a row that carries one.
func publishedStorageOf(row groupAnswerRow, group *Group) *byteStorageFinder {
	answers := row.call(group)
	if row.publishes != nil {
		answers = row.publishes(answers)
	}
	finder := newByteStorageFinder(byteStorageAnswer)
	for at, answer := range answers {
		how := fmt.Sprintf("%s() publishes %d", row.name, at)
		finder.walk(reflect.ValueOf(answer), how, how, 0)
	}
	return finder
}

// publishingRowAcrossTwoFoundings reads "a group goes on publishing what it was founded on" for a
// row whose method answers something FRESH on every call.
//
// TWO GROUPS AND NOT TWO CALLS, and the reason is the methods rather than a convenience. Three of
// the four generators may be asked only once per epoch -- RFC 9420 section 12.2 admits one update
// and one group_context_extensions proposal per sender and one remove per target, and the proposal
// cache enforces all three -- so a second call on one group answers a REFUSAL, and a gate that read
// the difference between a message and a refusal as a finding would report one against a group that
// is entirely correct.
//
// So the two readings come from two groups founded from two argument sets callerOwnedGroupArguments
// builds identically, and the comparison is over the row's PROJECTION: the part of the answer two
// such foundings are entitled to agree on, which for a proposal is the cleartext group id and
// nothing else. One caller then writes into its own arrays before its group is asked. A group that
// KEPT what it was handed publishes the scribble; a group that copied it publishes what it was
// founded on. Measured against NewGroup with its group id clone removed, this row fails.
//
// The pristine reading is required to be non-empty, so a row driven into a refusal -- which
// projects to nothing -- cannot report the clean run a correct row reports.
func publishingRowAcrossTwoFoundings(t *testing.T, row groupAnswerRow) {
	t.Helper()
	pristineCfg, pristineSigner, pristineCred := callerOwnedGroupArguments(t)
	pristine, err := NewGroup(pristineCfg, pristineSigner, pristineCred)
	if err != nil {
		t.Fatalf("%s: found the group this row reads unscribbled: %v", row.name, err)
	}
	defer pristine.Close()
	before := publishedStorageOf(row, pristine).contents()
	if len(before) == 0 {
		t.Fatalf("%s: the row published nothing at all, so this comparison ran over nothing", row.name)
	}

	cfg, signer, cred := callerOwnedGroupArguments(t)
	owned := newByteStorageFinder(byteStorageOutside)
	owned.walk(reflect.ValueOf(cfg), "cfg", "cfg", 0)
	owned.walk(reflect.ValueOf(&signer), "signer", "signer", 0)
	owned.walk(reflect.ValueOf(&cred), "cred", "cred", 0)
	if len(owned.found) == 0 {
		t.Fatalf("%s: the walk over this group's arguments reached no storage, so this row scribbled nothing", row.name)
	}
	scribbled, err := NewGroup(cfg, signer, cred)
	if err != nil {
		t.Fatalf("%s: found the group this row reads scribbled: %v", row.name, err)
	}
	defer scribbled.Close()
	for at, mine := range owned.found {
		mine.scribble(byte(at) | 0x80)
	}
	after := publishedStorageOf(row, scribbled).contents()
	for _, route := range slices.Sorted(maps.Keys(before)) {
		if !slices.Equal(before[route], after[route]) {
			t.Errorf("%s published %v at %s from a group whose caller left its own arrays alone, and %v from one whose caller wrote into them",
				row.name, before[route], route, after[route])
		}
	}
	for _, route := range slices.Sorted(maps.Keys(after)) {
		if _, publishedBefore := before[route]; !publishedBefore {
			t.Errorf("%s published nothing at %s from a group whose caller left its own arrays alone, and %v from one whose caller wrote into them",
				row.name, route, after[route])
		}
	}
}

// ---------------------------------------------------------------------------
// the third direction: what a group hands OUTWARD, to an object its caller supplied
// ---------------------------------------------------------------------------

// THE HALF BOTH GATES ABOVE ARE BLIND TO, and the reason generalises past the one call it was
// found on.
//
// Each of them reads a method RESULT: one walks what a construction ANSWERS, the other what a
// group answers. An octet handed outward as an ARGUMENT is invisible to both, and a group hands
// octets outward on every call it makes to an object its caller supplied. persist was the
// measurement: it passed self.context.GroupId -- the live one, not a copy -- to
// StateStore.PutGroupState, while GroupId() clones on the way out for exactly the reason persist
// did not. The sdk writes these StateStore implementations, and one that keeps the slice it was
// handed, to key a map or to build a path with later, shares an array with the group for the
// group's lifetime; a write through it rewrites the group id every epoch secret was derived over,
// with the context, the tree hash and the transcript all going on agreeing with each other over
// the wrong id.
//
// THE SCOPE IS READ OFF GroupConfig and not off the one interface this was found on. A caller
// supplies a group its objects through that structure, so its interface typed fields ARE the
// class, and a third one added tomorrow fails the equality below rather than going uncovered.

// groupInjectedObjects answers the objects a caller supplies to a group: the interface typed
// fields of GroupConfig, read off the compiled type.
//
// Interface typed and not "everything a caller passes", because that is the line the argument
// walk already draws: a slice field is a buffer the group is meant to copy, and the gates above
// hold it to that. A field the caller supplies an OBJECT through is a field the group calls back
// into, and the octets that go out on those calls are what nothing else here reads.
func groupInjectedObjects(t *testing.T) []string {
	t.Helper()
	names := []string{}
	config := reflect.TypeOf(GroupConfig{})
	for at := range config.NumField() {
		field := config.Field(at)
		if field.IsExported() && field.Type.Kind() == reflect.Interface {
			names = append(names, field.Name)
		}
	}
	if len(names) == 0 {
		t.Fatal("GroupConfig carries no interface typed field, so this gate has no subjects")
	}
	slices.Sort(names)
	return names
}

// The injected objects this gate does NOT hold, named with the reason, on
// packageConstructionsOverBorrowedBytes's terms: an object that cannot be held is a line somebody
// writes on purpose rather than one left out of a table, and the gate below refuses an entry
// naming a field GroupConfig does not carry, so an entry cannot outlive what it excuses.
var groupOutwardObjectsHandedWhatTheyCompute = map[string]string{
	// the provider is handed the very secret it derives from: ExpandWithLabel takes the epoch
	// secret the schedule holds, MacSign takes the membership key, and an AEAD takes the key it
	// seals under. Holding those calls to "no octet of the group's own goes out" would be
	// holding the group to not deriving anything, which is zeroizeSecret's exemption one level
	// out. What makes the store different is not that it is less trusted but what it is FOR: it
	// is handed octets to KEEP, so the slice it was given outlives the call.
	"Crypto": "is handed the secrets it derives from and the keys it tags with; the octets are the argument",
}

// recordingStore is a StateStore that records the storage of every argument it is handed and then
// delegates to a real one.
//
// It records rather than retains, and the difference matters: a double that KEPT what it was
// handed would be one implementation of a caller's store out of many, and this gate is about the
// slice being reachable at all rather than about what any particular caller does with it.
type recordingStore struct {
	inner      StateStore
	recorded   []byteStorage
	unfollowed []string
	calls      []string
}

func newRecordingStore() *recordingStore {
	return &recordingStore{inner: newTestStore()}
}

// record walks one call's arguments and keeps every run of octets they reach.
//
// byteStorageAnswer is the walk, which is the one the hand-out gate uses: what a store can spell
// is what any holder can spell, plus whatever an interface it was handed carries.
func (self *recordingStore) record(call string, arguments ...any) {
	self.calls = append(self.calls, call)
	for at, argument := range arguments {
		how := fmt.Sprintf("%s argument %d", call, at)
		finder := byteStorageFoundIn(reflect.ValueOf(argument), how, byteStorageAnswer)
		self.recorded = append(self.recorded, finder.found...)
		self.unfollowed = append(self.unfollowed, finder.unfollowed...)
	}
}

// paths answers the routes this store was handed storage down, which is what the coverage control
// compares against the interface's own signatures.
func (self *recordingStore) paths() []string {
	out := []string{}
	for _, one := range self.recorded {
		if !slices.Contains(out, one.path) {
			out = append(out, one.path)
		}
	}
	slices.Sort(out)
	return out
}

func (self *recordingStore) PutGroupState(groupId []byte, epoch uint64, state []byte) error {
	self.record("PutGroupState", groupId, epoch, state)
	return self.inner.PutGroupState(groupId, epoch, state)
}

func (self *recordingStore) GetGroupState(groupId []byte, epoch uint64) ([]byte, error) {
	self.record("GetGroupState", groupId, epoch)
	return self.inner.GetGroupState(groupId, epoch)
}

func (self *recordingStore) DeleteGroupStateBefore(groupId []byte, epoch uint64) error {
	self.record("DeleteGroupStateBefore", groupId, epoch)
	return self.inner.DeleteGroupStateBefore(groupId, epoch)
}

func (self *recordingStore) PutPrivateKey(pub []byte, priv []byte) error {
	self.record("PutPrivateKey", pub, priv)
	return self.inner.PutPrivateKey(pub, priv)
}

func (self *recordingStore) GetPrivateKey(pub []byte) ([]byte, error) {
	self.record("GetPrivateKey", pub)
	return self.inner.GetPrivateKey(pub)
}

func (self *recordingStore) DeletePrivateKey(pub []byte) error {
	self.record("DeletePrivateKey", pub)
	return self.inner.DeletePrivateKey(pub)
}

func (self *recordingStore) PutKeyPackage(ref []byte, kp []byte, initPriv []byte, encPriv []byte) error {
	self.record("PutKeyPackage", ref, kp, initPriv, encPriv)
	return self.inner.PutKeyPackage(ref, kp, initPriv, encPriv)
}

func (self *recordingStore) TakeKeyPackage(ref []byte) ([]byte, []byte, []byte, error) {
	self.record("TakeKeyPackage", ref)
	return self.inner.TakeKeyPackage(ref)
}

// fillByteStorage builds a value of one type carrying a run of octets down every route that type
// declares storage at, so the coverage control below hands each method something to record.
//
// Derived off the type rather than written per method: a method whose signature grows a parameter
// is filled by the commit that adds it, and a shape this cannot fill shows up as a route declared
// and not recorded rather than as a route nobody asked about.
func fillByteStorage(found reflect.Type, mark byte, depth int) reflect.Value {
	if depth > byteStorageDepth {
		return reflect.Zero(found)
	}
	switch found.Kind() {
	case reflect.Slice:
		if found.Elem().Kind() == reflect.Uint8 {
			return reflect.ValueOf(bytes.Repeat([]byte{mark}, 8)).Convert(found)
		}
		filled := reflect.MakeSlice(found, 1, 1)
		filled.Index(0).Set(fillByteStorage(found.Elem(), mark, depth+1))
		return filled
	case reflect.Array:
		filled := reflect.New(found).Elem()
		for at := range found.Len() {
			filled.Index(at).Set(fillByteStorage(found.Elem(), mark+byte(at), depth+1))
		}
		return filled
	case reflect.Pointer:
		filled := reflect.New(found.Elem())
		filled.Elem().Set(fillByteStorage(found.Elem(), mark, depth+1))
		return filled
	case reflect.Struct:
		filled := reflect.New(found).Elem()
		for at := range found.NumField() {
			if !found.Field(at).IsExported() {
				continue
			}
			filled.Field(at).Set(fillByteStorage(found.Field(at).Type, mark+byte(at), depth+1))
		}
		return filled
	case reflect.Map:
		filled := reflect.MakeMap(found)
		filled.SetMapIndex(fillByteStorage(found.Key(), mark, depth+1),
			fillByteStorage(found.Elem(), mark+1, depth+1))
		return filled
	}
	return reflect.Zero(found)
}

// TestTheRecordingStoreReadsEveryArgumentItsInterfaceDeclares is the coverage control for the
// double, and the class it is held to is StateStore's own method set.
//
// A hand written double is nine wrappers, and a wrapper that forgot to record is a call the gate
// below walks past in silence -- which is the clean run of a gate that had checked it. So every
// method is driven through reflect with a run of octets down every route its signature declares,
// and what the double recorded is compared against those routes. A tenth method, or a parameter
// added to one of the nine, fails here on the commit that adds it.
func TestTheRecordingStoreReadsEveryArgumentItsInterfaceDeclares(t *testing.T) {
	storeType := reflect.TypeOf((*StateStore)(nil)).Elem()
	if storeType.NumMethod() == 0 {
		t.Fatal("StateStore declares no method, so this control drives nothing")
	}
	for at := range storeType.NumMethod() {
		method := storeType.Method(at)
		store := newRecordingStore()
		arguments := []reflect.Value{}
		declared := []string{}
		for in := range method.Type.NumIn() {
			how := fmt.Sprintf("%s argument %d", method.Name, in)
			arguments = append(arguments, fillByteStorage(method.Type.In(in), byte(0x10+in), 0))
			declared = append(declared, byteStoragePathsOf(method.Type.In(in), how)...)
		}
		if len(declared) == 0 {
			t.Errorf("%s declares no byte storage in any parameter, so the double has nothing to record there and this gate would pass over a call that carried some",
				method.Name)
			continue
		}
		reflect.ValueOf(store).MethodByName(method.Name).Call(arguments)
		if len(store.unfollowed) != 0 {
			t.Errorf("%s: the double could not follow %v, so it would walk past the same shape on a real call",
				method.Name, store.unfollowed)
		}
		if got := store.paths(); !slices.Equal(got, sortedStorageRoutes(declared)) {
			t.Errorf("%s handed the double storage at %v and its signature declares %v; a wrapper that stopped recording is a call this gate reads as carrying nothing",
				method.Name, got, sortedStorageRoutes(declared))
		}
	}
}

// TestNoOctetAGroupHandsOutwardIsStorageItKeeps is the third direction, over the objects the
// caller supplied.
//
// The group is walked BEFORE the outward calls and AFTER them, for the reason the hand-out gate
// is: a snapshot taken on one side only is silent about storage that arrives on the other. The
// calls this observes today all happen inside NewGroup -- persist is the only one -- so the first
// snapshot is the one that matters now, and the second is what covers a method that persists.
func TestNoOctetAGroupHandsOutwardIsStorageItKeeps(t *testing.T) {
	// the class, read off GroupConfig, minus what is excused with a reason
	covered := []string{"Store"}
	injected := groupInjectedObjects(t)
	demanded := []string{}
	for _, name := range injected {
		if _, isExcused := groupOutwardObjectsHandedWhatTheyCompute[name]; isExcused {
			continue
		}
		demanded = append(demanded, name)
	}
	slices.Sort(covered)
	if !slices.Equal(covered, demanded) {
		t.Errorf("this gate follows what a group hands to %v, and the objects its caller supplies that are not excused are %v; an injected object with no double is a call nobody reads",
			covered, demanded)
	}
	for name := range groupOutwardObjectsHandedWhatTheyCompute {
		if !slices.Contains(injected, name) {
			t.Errorf("the gate excuses %s, which GroupConfig carries no interface typed field for", name)
		}
	}

	for _, row := range groupAnswerRows(t) {
		cfg, signer, cred := callerOwnedGroupArguments(t)
		store := newRecordingStore()
		cfg.Store = store
		group, err := NewGroup(cfg, signer, cred)
		if err != nil {
			t.Fatalf("%s: found the group this row follows: %v", row.name, err)
		}
		founded := byteStorageFoundIn(reflect.ValueOf(group), "group as founded", byteStorageInside)
		row.call(group)
		kept := byteStorageFoundIn(reflect.ValueOf(group), "group after the call", byteStorageInside)
		if len(store.recorded) == 0 {
			t.Fatalf("%s: the group handed its store no octet at all, so this row compared nothing; the store is written to on the creation path",
				row.name)
		}
		if len(store.unfollowed) != 0 {
			t.Errorf("%s: the walk over what went outward could not follow %v", row.name, store.unfollowed)
		}
		if len(founded.found) == 0 || len(kept.found) == 0 {
			t.Fatalf("%s: the group reached no storage, so this row compared nothing", row.name)
		}
		for _, given := range store.recorded {
			for _, snapshot := range []*byteStorageFinder{founded, kept} {
				for _, theirs := range snapshot.found {
					if given.overlaps(theirs) {
						t.Errorf("%s: the group handed its store %s, which is the group's own %s: a store that keeps what it was handed writes into the group",
							row.name, given.how, theirs.how)
					}
				}
			}
		}
		group.Close()
	}
}

// mustFoundedGroup is one group over the arguments above, for the rows that do not need the
// arguments back.
func mustFoundedGroup(t *testing.T) *Group {
	t.Helper()
	cfg, signer, cred := callerOwnedGroupArguments(t)
	group, err := NewGroup(cfg, signer, cred)
	if err != nil {
		t.Fatalf("found the group this gate follows: %v", err)
	}
	t.Cleanup(func() { group.Close() })
	return group
}

// groupAnswerDeclaresStorage answers whether a method of *Group can answer byte storage at all,
// read off its signature. A method that cannot is allowed to produce none; one that can and did not
// is a row that observed nothing.
func groupAnswerDeclaresStorage(t *testing.T, name string) bool {
	t.Helper()
	method, found := reflect.TypeOf((*Group)(nil)).MethodByName(name)
	if !found {
		t.Fatalf("*Group declares no method %s, so this row names something that is not in the class", name)
	}
	for at := range method.Type.NumOut() {
		if len(byteStoragePathsOf(method.Type.Out(at), name)) != 0 {
			return true
		}
	}
	return false
}
