// The one question several gates of this package ask about a type -- "can a holder of this reach a
// GroupContext" -- asked TRANSITIVELY, in one place, with a control that fails when an arm of it
// stops working.
//
// WHAT THIS IS INSTEAD OF, AND WHY IT IS ONE HELPER RATHER THAN FOUR FIXES.
// extensionTypeSelectionNamedAs answers "does the compiler read this type AS the type spelled
// name", unwrapping *types.Pointer and nothing else. That is exactly right for a gate identifying
// WHICH expression it is looking at -- the base of a selection, a write target, a composite
// literal's own type -- and it is the wrong question for a gate asking whether a declaration HANDS
// A CONTEXT OUT, because handing one out inside a slice is handing it out. Four gates asked the
// second question with the first question's matcher, and three leaks were measured surviving the
// whole suite at 6810 passing:
//
//	func LeakVerifiedSlice(self *VerifiedGroupContext) []*GroupContext { return []*GroupContext{self.inner} }
//	func (self *VerifiedGroupContext) Leaked() []*GroupContext          { return []*GroupContext{self.inner} }
//	func (self *VerifiedGroupContext) Held() any                        { return self.inner }
//
// Driven end to end from package mls_test, the second of those turned an honest GroupInfo verified
// as "the honest group @ 3" into "ATTACKER-CHOSEN-GROUP @ 1099511627776" in one statement, with
// NewProposalCache binding to that pair. The control proves the gates fire for the shape they were
// written against -- a plain Inner() *GroupContext fails both -- so the CLASS was right and the
// TYPE MATCHER was narrow, by exactly one constructor. That is the seventh instance on this project
// of a gate deriving its class correctly and then narrowing how it matches, so the answer is one
// helper every asker shares rather than a slice arm added to each of them, which is how the four
// would drift apart again.
//
// WHERE "REACH" STOPS, WHICH IS THE WHOLE OF THE JUDGEMENT HERE. Transitive reachability over
// approximates, and a helper that flags everything is a helper the next person deletes. Four lines
// are drawn. The first three are derived from something rather than exempted by name; the fourth is
// an approximation the source side cannot avoid, written down rather than left to be rediscovered:
//
//  1. A STRUCT IS ENTERED THROUGH ITS EXPORTED FIELDS ONLY, because those are what a holder in
//     ANOTHER package can spell. This is not a convenience: it IS the mechanism
//     VerifiedGroupContext rests on, stated as a walk, and it is the compiler's property rather
//     than a gate's -- the type's own doc puts it as "for every package but this one", and
//     TestEveryConstructionOfAVerifiedGroupContextIsClassifiedHere asserts the field is unexported.
//     So a *VerifiedGroupContext does not reach a *GroupContext for any holder outside package mls,
//     and every gate below can go on naming the verified type as the thing that is safe to hand out
//     without an exemption written anywhere. It generalises that far and no further: a second guard
//     type declared tomorrow is sealed against other packages by the same property, and inside its
//     own package its field is ordinary. What it costs is stated in the control -- see
//     notALeakThroughASealedBox -- a box with an unexported field and an accessor of its own is a
//     leak this walk does not see, and that accessor is not in the reader class because it is
//     handed the box rather than the verified value.
//  2. A NAMED TYPE'S METHODS ARE NOT TRAVERSED. A method that answers a group context is a member
//     of the reader class in its own right and is judged as one; traversing them would make every
//     declaration answering a *VerifiedGroupContext a reader by way of Context, which is the one
//     classified reader there is, and passItOn in the control would fail for handing back a value
//     whose guarantee is intact.
//  3. A SIGNATURE IS ENTERED THROUGH ITS RESULTS AND NOT ITS PARAMETERS. What a returned func
//     RETURNS is something the holder receives; what it TAKES is something the holder supplies, and
//     supplying a context to a callback is not being handed the one this value vouches for.
//  4. AN INTERFACE IS ENTERED THROUGH WHAT ITS OWN METHODS ANSWER, and the empty one holds
//     everything, which is what makes Held() any the leak it was measured to be. What that cannot
//     see is a concrete type carrying a context in a field, handed back as some interface that does
//     not answer one: go/types has a static type and the value is not in it. This is the one line
//     here that is an approximation rather than a judgement, and the compiled walk beside it asks
//     the precise question instead -- an interface holds a target exactly when the target
//     implements it. A matcher limit that is written nowhere is one the next round rediscovers,
//     which is the only reason it is a numbered line.
//
// AND THREE GATES DELIBERATELY DO NOT ASK THIS QUESTION, said here so the omission is a decision
// rather than an oversight. verifiedGroupContextConstructionsIn asks whether a composite literal IS
// a VerifiedGroupContext and whether a selector's base IS one; epochGroupMoveIn asks whether a
// write TARGET is a group context; epochEnderCarriesTheContext asks WHICH ONE argument of an ender
// is the context. All three identify a single expression or parameter rather than asking what a
// value can reach, and the last of them is followed by a demand for exactly one -- a transitive
// answer there would name more parameters and make that demand refuse enders that are correct.
package mls

import (
	"fmt"
	"go/ast"
	"go/types"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// the question over the type checker's types, for the gates derived off source
// ---------------------------------------------------------------------------

// typeReachesNamed answers whether a holder of a value of this type can reach a value of the type
// spelled name, through any composition of the constructors Go has for putting one value inside
// another: pointers, slices, arrays, map keys and values, channels, function results, interfaces,
// named types and exported struct fields.
//
// The compiler's reading and not the source's, for extensionTypeSelectionNamedAs's reason: an
// alias, a type declared elsewhere and referred to as mls.GroupContext, and a defined pointer type
// all answer the same.
func typeReachesNamed(found types.Type, name string) bool {
	return typeReachesNamedThrough(found, name, map[*types.TypeName]bool{})
}

// typeReachesTupleNamed is the same question over a signature's inputs or its outputs.
//
// A helper over the tuple rather than a loop written at each caller, because the two halves of the
// reader rule ask the same question of a signature's inputs and of its outputs, and two spellings
// of one question is how one of them ends up narrower than the other -- which is the defect this
// whole file is the fix for, one level up.
func typeReachesTupleNamed(tuple *types.Tuple, name string) bool {
	return typeReachesTupleThrough(tuple, name, map[*types.TypeName]bool{})
}

// typeReachesNamedThrough is the walk, carrying the named types it has already entered.
//
// Termination is held at the NAMED arm and nowhere else, because a cycle in a Go type is only
// spellable through a name: type ring struct{ Next *ring } needs the name to refer to itself.
// Marking a name entered before its answer is known is sound -- reaching the same named type by a
// second route asks a question whose answer depends only on the type, and a true answer has already
// been returned up the chain by the time a sibling could ask again.
func typeReachesNamedThrough(found types.Type, name string, entered map[*types.TypeName]bool) bool {
	found = types.Unalias(found)
	if found == nil {
		return false
	}
	switch shape := found.(type) {
	case *types.Named:
		object := shape.Obj()
		if object == nil {
			return false
		}
		if object.Name() == name {
			return true
		}
		if entered[object] {
			return false
		}
		entered[object] = true
		return typeReachesNamedThrough(shape.Underlying(), name, entered)
	case *types.Pointer:
		return typeReachesNamedThrough(shape.Elem(), name, entered)
	case *types.Slice:
		return typeReachesNamedThrough(shape.Elem(), name, entered)
	case *types.Array:
		return typeReachesNamedThrough(shape.Elem(), name, entered)
	case *types.Map:
		// both, because a map keyed by the pointer hands it out exactly as a map valued by
		// one does
		return typeReachesNamedThrough(shape.Key(), name, entered) ||
			typeReachesNamedThrough(shape.Elem(), name, entered)
	case *types.Chan:
		return typeReachesNamedThrough(shape.Elem(), name, entered)
	case *types.Signature:
		return typeReachesTupleThrough(shape.Results(), name, entered)
	case *types.Struct:
		for at := 0; at < shape.NumFields(); at += 1 {
			field := shape.Field(at)
			// the exported fields, which are what a holder can spell. See line 1 of this
			// file's header for why that is the mechanism rather than a convenience.
			if !field.Exported() {
				continue
			}
			if typeReachesNamedThrough(field.Type(), name, entered) {
				return true
			}
		}
		return false
	case *types.Interface:
		// an EMPTY interface holds any value there is, which is what makes Held() any the
		// leak it was measured to be; a non empty one is entered through what its own
		// methods answer. What this cannot see is a concrete type implementing some other
		// interface and carrying a context in a field it does not answer -- that shape is
		// named in this file's header rather than papered over here.
		if shape.Empty() {
			return true
		}
		for at := 0; at < shape.NumMethods(); at += 1 {
			signature, isSignature := shape.Method(at).Type().(*types.Signature)
			if !isSignature {
				continue
			}
			if typeReachesTupleThrough(signature.Results(), name, entered) {
				return true
			}
		}
		return false
	}
	return false
}

// typeReachesTupleThrough is the tuple arm of the same walk.
func typeReachesTupleThrough(tuple *types.Tuple, name string, entered map[*types.TypeName]bool) bool {
	if tuple == nil {
		return false
	}
	for at := 0; at < tuple.Len(); at += 1 {
		if typeReachesNamedThrough(tuple.At(at).Type(), name, entered) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// the same question over the COMPILED types, for the gates derived off a method set
// ---------------------------------------------------------------------------

// reflectTypeReaches answers the same question over the compiled type, for the gates that read a
// method set rather than source.
//
// Two walks over one rule and not one walk, because the two subjects are different things: go/types
// answers about source this package may not even link, and reflect answers about the binary that
// actually runs. They are held to the same class by two controls carrying the same members, which
// is the only way an arm added to one and forgotten in the other fails.
func reflectTypeReaches(found reflect.Type, targets []reflect.Type) bool {
	return reflectTypeReachesThrough(found, targets, map[reflect.Type]bool{})
}

// reflectTypeReachesThrough is the compiled walk. reflect has no alias arm and needs none: an alias
// is gone by the time there is a binary to reflect over.
func reflectTypeReachesThrough(found reflect.Type, targets []reflect.Type,
	entered map[reflect.Type]bool) bool {
	if found == nil {
		return false
	}
	if slices.Contains(targets, found) {
		return true
	}
	if entered[found] {
		return false
	}
	entered[found] = true
	switch found.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Chan:
		return reflectTypeReachesThrough(found.Elem(), targets, entered)
	case reflect.Map:
		return reflectTypeReachesThrough(found.Key(), targets, entered) ||
			reflectTypeReachesThrough(found.Elem(), targets, entered)
	case reflect.Func:
		for at := range found.NumOut() {
			if reflectTypeReachesThrough(found.Out(at), targets, entered) {
				return true
			}
		}
		return false
	case reflect.Struct:
		for at := range found.NumField() {
			field := found.Field(at)
			if !field.IsExported() {
				continue
			}
			if reflectTypeReachesThrough(field.Type, targets, entered) {
				return true
			}
		}
		return false
	case reflect.Interface:
		// the compiled side can ask the precise question the source side has to
		// approximate: an interface holds a target exactly when the target implements it,
		// and every type implements the empty one
		for _, target := range targets {
			if target.Implements(found) {
				return true
			}
		}
		for at := range found.NumMethod() {
			if reflectTypeReachesThrough(found.Method(at).Type, targets, entered) {
				return true
			}
		}
		return false
	}
	return false
}

// ---------------------------------------------------------------------------
// and the same question over a VALUE, which is what a behavioural gate needs
// ---------------------------------------------------------------------------

// the two shapes a group context is answered in, named once. A copy of the struct still aliases the
// octet slices it was copied from, which is why the value shape is a hand-out at all.
var (
	reachGroupContextPointer = reflect.TypeOf((*GroupContext)(nil))
	reachGroupContextValue   = reflect.TypeOf(GroupContext{})
	reachGroupContextTargets = []reflect.Type{reachGroupContextPointer, reachGroupContextValue}
)

// how far the value walk goes, and how many entries it draws from a channel. Both are guards
// against a cyclic value rather than rules: a type that reaches a context reaches it through a
// finite chain of constructors, and a gate that hung would be no gate at all.
const (
	reachValueDepth   = 16
	reachChannelDraws = 8
)

// reachedGroupContext is one group context a holder actually got out of an answer.
//
// identity is the pointer itself when a pointer was reached, and nil when the walk got a copy of
// the struct; context is always something a write can go through. The two are different findings: a
// hand-out of the pointer is a leak on its own, and a copy is a leak only if writing through it
// moves the value that was vouched for.
type reachedGroupContext struct {
	how      string
	identity *GroupContext
	context  *GroupContext
}

// reachGroupContextsIn collects every group context a holder can reach out of one value, and
// records what it could not follow rather than walking past it.
//
// The blocked list is not tidiness. A function this walk cannot call, or one that panics, is an
// answer nobody looked at, and a gate that skipped it would report the clean run of a gate that
// had checked it.
func reachGroupContextsIn(value reflect.Value, how string, into *[]reachedGroupContext,
	blocked *[]string, depth int) {
	if !value.IsValid() || depth > reachValueDepth {
		return
	}
	switch value.Kind() {
	case reflect.Pointer:
		if value.IsNil() {
			return
		}
		// a POINTER to a group context however it is spelled: a defined pointer type
		// converts back, so type ctxPtr *GroupContext is the same hand-out as *GroupContext
		if value.Type().Elem() == reachGroupContextValue &&
			value.Type().ConvertibleTo(reachGroupContextPointer) {
			held, _ := value.Convert(reachGroupContextPointer).Interface().(*GroupContext)
			*into = append(*into, reachedGroupContext{how: how, identity: held, context: held})
			return
		}
		reachGroupContextsIn(value.Elem(), how+", through a pointer", into, blocked, depth+1)
	case reflect.Struct:
		if value.Type().ConvertibleTo(reachGroupContextValue) {
			held, _ := value.Convert(reachGroupContextValue).Interface().(GroupContext)
			*into = append(*into, reachedGroupContext{how: how, context: &held})
			return
		}
		for at := range value.NumField() {
			field := value.Type().Field(at)
			if !field.IsExported() {
				continue
			}
			reachGroupContextsIn(value.Field(at), how+", through field "+field.Name,
				into, blocked, depth+1)
		}
	case reflect.Interface:
		if value.IsNil() {
			return
		}
		reachGroupContextsIn(value.Elem(), how+", through an interface", into, blocked, depth+1)
	case reflect.Slice, reflect.Array:
		if value.Kind() == reflect.Slice && value.IsNil() {
			return
		}
		for at := range value.Len() {
			reachGroupContextsIn(value.Index(at),
				fmt.Sprintf("%s, through element %d", how, at), into, blocked, depth+1)
		}
	case reflect.Map:
		if value.IsNil() {
			return
		}
		for iterator := value.MapRange(); iterator.Next(); {
			reachGroupContextsIn(iterator.Key(), how+", through a map key", into, blocked, depth+1)
			reachGroupContextsIn(iterator.Value(), how+", through a map value", into, blocked, depth+1)
		}
	case reflect.Chan:
		if value.IsNil() || value.Type().ChanDir()&reflect.RecvDir == 0 {
			return
		}
		for range reachChannelDraws {
			held, drawn := value.TryRecv()
			if !drawn {
				return
			}
			reachGroupContextsIn(held, how+", through a channel", into, blocked, depth+1)
		}
	case reflect.Func:
		if value.IsNil() {
			return
		}
		if value.Type().NumIn() != 0 {
			*blocked = append(*blocked, how+", which answers a function this gate cannot call")
			return
		}
		results, refused := reachCallOf(value)
		if refused != "" {
			*blocked = append(*blocked, how+", whose call "+refused)
			return
		}
		for at, result := range results {
			reachGroupContextsIn(result,
				fmt.Sprintf("%s, through result %d of a function", how, at),
				into, blocked, depth+1)
		}
	}
}

// reachCallOf calls a niladic function and answers what it returned, or why it could not be
// followed. A panic is reported rather than allowed to take the run down, so a leak wrapped in a
// function that fails is still a finding a reader can act on.
func reachCallOf(value reflect.Value) (results []reflect.Value, refused string) {
	defer func() {
		if held := recover(); held != nil {
			results, refused = nil, fmt.Sprintf("panicked with %v", held)
		}
	}()
	return value.Call(nil), ""
}

// ---------------------------------------------------------------------------
// the controls: one member per constructor, so a broken arm fails rather than narrowing
// ---------------------------------------------------------------------------

// the name the source control's walk is written against, and the one place it is spelled.
const typeReachControlTargetName = "Target"

// typeReachControl is a package the matcher has never seen, carrying one declaration per arm of the
// walk and one per line the walk draws.
//
// A control rather than a second opinion about the real source: a matcher that resolved nothing
// reports a clean run over mls too, and the only way to tell that apart from a package that hands
// nothing out is to run it over source known to hold both answers. Every member is a function whose
// RESULT is the shape, so the members are named by the compiler rather than by a comment, and the
// derived set is held equal to the table below in both directions -- an arm deleted drops a name
// and an arm loosened adds one.
const typeReachControl = `package control

type Target struct{ Id []byte }
type Other struct{ Id []byte }

type targetPointer *Target
type targetAlias = *Target

type box struct{ Held *Target }

// the line this file draws: a holder outside the declaring package cannot spell this field, which
// is the whole of what makes a guard type a guard
type sealed struct{ held *Target }

type ring struct{ Next *ring }

type chain struct {
	Next *chain
	Held *Target
}

type holder interface{ Held() *Target }

type refuses interface{ Named() string }

// a named type whose METHOD answers one, which the walk does not follow: such a method is a member
// of the reader class in its own right and is judged as one
type carrier struct{ id int }

func (self carrier) Held() *Target { return nil }

func aValue() Target                  { return Target{} }
func aPointer() *Target               { return nil }
func aPointerToAPointer() **Target    { return nil }
func aDefinedPointer() targetPointer  { return nil }
func anAlias() targetAlias            { return nil }
func aSlice() []*Target               { return nil }
func anArray() [2]Target              { return [2]Target{} }
func aMapValue() map[string]*Target   { return nil }
func aMapKey() map[*Target]bool       { return nil }
func aChannel() chan *Target          { return nil }
func aFunctionResult() func() *Target { return nil }
func anExportedField() *box           { return nil }
func anEmptyInterface() any           { return nil }
func anInterfaceMethodResult() holder { return nil }
func aCycleThatCarriesOne() *chain    { return nil }

func nothingAtAll() *Other                  { return nil }
func anUnexportedField() *sealed            { return nil }
func anInterfaceThatCannotHoldOne() refuses { return nil }
func aFunctionParameter() func(*Target)     { return nil }
func aCycleThatCarriesNone() *ring          { return nil }
func aMethodOfANamedType() *carrier         { return nil }
func noResultAtAll()                        {}
`

// What the source walk must report out of the control, and by omission what it must not.
var typeReachControlReaching = []string{
	"aChannel",
	"aCycleThatCarriesOne",
	"aDefinedPointer",
	"aFunctionResult",
	"aMapKey",
	"aMapValue",
	"aPointer",
	"aPointerToAPointer",
	"aSlice",
	"aValue",
	"anAlias",
	"anArray",
	"anEmptyInterface",
	"anExportedField",
	"anInterfaceMethodResult",
}

// TestTheTypeReachWalkEntersEveryConstructorItClaims is the control over the source side.
//
// Held equal in both directions, which is what makes it fail on a narrowing AND on a loosening: an
// arm removed drops its member out of the derived set, and an arm that started following a struct's
// unexported fields or a named type's methods adds one.
func TestTheTypeReachWalkEntersEveryConstructorItClaims(t *testing.T) {
	control := typeCheckedBodiesOfText(t, "the type reach control", typeReachControl)
	judged := 0
	reaching := []string{}
	for _, file := range control.files {
		for _, declaration := range file.Decls {
			// the plain functions, because every member is one; the method on carrier is
			// there to be NOT followed, and judging it would report the arm this file
			// says it does not have
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Recv != nil {
				continue
			}
			defined, isDefined := control.info.Defs[function.Name].(*types.Func)
			if !isDefined {
				continue
			}
			signature, isSignature := defined.Type().(*types.Signature)
			if !isSignature {
				continue
			}
			judged += 1
			if typeReachesTupleNamed(signature.Results(), typeReachControlTargetName) {
				reaching = append(reaching, function.Name.Name)
			}
		}
	}
	if judged == 0 {
		t.Fatal("the walk judged no declaration of the control, so it would answer false for every type there is")
	}
	slices.Sort(reaching)
	want := slices.Sorted(slices.Values(typeReachControlReaching))
	if !slices.Equal(reaching, want) {
		t.Fatalf("the source walk reads %v out of the control as reaching a %s, want %v; a name missing is an arm that stopped working, and a name extra is a line this file says it draws and does not",
			reaching, typeReachControlTargetName, want)
	}
	t.Logf("%d declarations judged, %d reach a %s", judged, len(reaching), typeReachControlTargetName)
}

// The compiled control's own types, mirroring the source control member for member so that an arm
// added to one walk and forgotten in the other fails.
type (
	reachTarget        struct{ Id []byte }
	reachOther         struct{ Id []byte }
	reachTargetPointer *reachTarget
	reachBox           struct{ Held *reachTarget }
	reachSealed        struct{ held *reachTarget }
	reachRing          struct{ Next *reachRing }
	reachChain         struct {
		Next *reachChain
		Held *reachTarget
	}
	reachHolder  interface{ Held() *reachTarget }
	reachRefuses interface{ Named() string }
	reachCarrier struct{ id int }
)

// the method the compiled walk must not follow, for the source control's reason.
func (self reachCarrier) Held() *reachTarget { return nil }

// reachShapes carries one field per constructor the compiled walk enters, and one per line it
// draws. A field rather than a table of reflect.TypeOf calls, so the members are named by the
// compiler and a member whose type no longer compiles is a build failure rather than a silent skip.
type reachShapes struct {
	AValue                       reachTarget
	APointer                     *reachTarget
	APointerToAPointer           **reachTarget
	ADefinedPointer              reachTargetPointer
	ASlice                       []*reachTarget
	AnArray                      [2]reachTarget
	AMapValue                    map[string]*reachTarget
	AMapKey                      map[*reachTarget]bool
	AChannel                     chan *reachTarget
	AFunctionResult              func() *reachTarget
	AnExportedField              *reachBox
	AnEmptyInterface             any
	AnInterfaceMethodResult      reachHolder
	ACycleThatCarriesOne         *reachChain
	NothingAtAll                 *reachOther
	AnUnexportedField            *reachSealed
	AnInterfaceThatCannotHoldOne reachRefuses
	AFunctionParameter           func(*reachTarget)
	ACycleThatCarriesNone        *reachRing
	AMethodOfANamedType          *reachCarrier
}

// What the compiled walk must report out of reachShapes. There is no alias member, and there is no
// arm for one: an alias is gone by the time there is a binary to reflect over.
var reachShapesReaching = []string{
	"AChannel",
	"ACycleThatCarriesOne",
	"ADefinedPointer",
	"AFunctionResult",
	"AMapKey",
	"AMapValue",
	"APointer",
	"APointerToAPointer",
	"ASlice",
	"AValue",
	"AnArray",
	"AnEmptyInterface",
	"AnExportedField",
	"AnInterfaceMethodResult",
}

// TestTheCompiledTypeReachWalkEntersEveryConstructorItClaims is the same control over the reflect
// side, and it is what says the two walks hold one class rather than two that drifted.
func TestTheCompiledTypeReachWalkEntersEveryConstructorItClaims(t *testing.T) {
	shapes := reflect.TypeOf(reachShapes{})
	if shapes.NumField() == 0 {
		t.Fatal("the compiled control carries no member, so this gate judged nothing")
	}
	targets := []reflect.Type{reflect.TypeOf(reachTarget{}), reflect.TypeOf((*reachTarget)(nil))}
	reaching := []string{}
	for at := range shapes.NumField() {
		field := shapes.Field(at)
		if reflectTypeReaches(field.Type, targets) {
			reaching = append(reaching, field.Name)
		}
	}
	slices.Sort(reaching)
	want := slices.Sorted(slices.Values(reachShapesReaching))
	if !slices.Equal(reaching, want) {
		t.Fatalf("the compiled walk reads %v out of the control as reaching a reachTarget, want %v; a name missing is an arm that stopped working, and a name extra is a line this file says it draws and does not",
			reaching, want)
	}
	// and the two walks agree member for member, which is the point of mirroring them: a member
	// present on one side and absent on the other is an arm one of them does not have. The
	// comparison is over the lower cased name because the compiled control's members are fields
	// and have to be exported to be walked at all.
	source := map[string]bool{}
	for _, name := range typeReachControlReaching {
		source[strings.ToLower(name)] = true
	}
	for _, name := range reachShapesReaching {
		if !source[strings.ToLower(name)] {
			t.Errorf("%s reaches a target on the compiled side and its source twin does not, so the two walks hold different classes",
				name)
		}
	}
	t.Logf("%d compiled members judged, %d reach a reachTarget", shapes.NumField(), len(reaching))
}

// the boxes the value control needs, and the defined pointer type the walk has to convert back.
type (
	reachContextPointer  *GroupContext
	reachContextBox      struct{ Held *GroupContext }
	reachContextValueBox struct{ Held GroupContext }
	reachContextSealed   struct{ held *GroupContext }
)

// reachValueControl is one value per constructor, every one of them carrying THE SAME pointer, so
// that the walk over values is held to finding the identity rather than merely finding something.
//
// The by value member is here for the difference it makes: a copy of the struct has no identity to
// find and still aliases the octet slices, which is why the behavioural gate writes through what it
// reached rather than only comparing pointers. The sealed member is the line drawn in this file's
// header, run rather than argued.
func reachValueControl(context *GroupContext) map[string]any {
	channel := make(chan *GroupContext, 1)
	channel <- context
	return map[string]any{
		"a pointer":                    context,
		"a pointer to a pointer":       &context,
		"a defined pointer type":       reachContextPointer(context),
		"a slice":                      []*GroupContext{context},
		"an array":                     [1]*GroupContext{context},
		"a map value":                  map[string]*GroupContext{"it": context},
		"a map key":                    map[*GroupContext]bool{context: true},
		"a channel":                    channel,
		"a function result":            func() *GroupContext { return context },
		"an exported struct field":     &reachContextBox{Held: context},
		"an interface":                 any(context),
		"a slice of interfaces":        []any{context},
		"a struct field held by value": reachContextValueBox{Held: *context},
		"an unexported struct field":   &reachContextSealed{held: context},
	}
}

// what the value walk must find the very pointer through, and what it must find anything at all
// through. The two differ by exactly the by value member, and the unexported field is in neither.
var (
	reachValueControlIdentity = []string{
		"a channel",
		"a defined pointer type",
		"a function result",
		"a map key",
		"a map value",
		"a pointer",
		"a pointer to a pointer",
		"a slice",
		"a slice of interfaces",
		"an array",
		"an exported struct field",
		"an interface",
	}
	reachValueControlFound = append([]string{"a struct field held by value"},
		reachValueControlIdentity...)
)

// TestTheValueWalkFindsAGroupContextThroughEveryConstructor is the third control, over the walk the
// behavioural gate spends.
//
// A type level answer is not enough for that gate: it has to get its hands on the context to
// compare the pointer and to write through it, and a walk that judged the type correctly and then
// found nothing in the value would report a clean run over every leak there is.
func TestTheValueWalkFindsAGroupContextThroughEveryConstructor(t *testing.T) {
	context := &GroupContext{GroupId: []byte("the walked context"), Epoch: 3}
	identity, found, refused := []string{}, []string{}, []string{}
	control := reachValueControl(context)
	for _, name := range slices.Sorted(maps.Keys(control)) {
		reached, blocked := []reachedGroupContext{}, []string{}
		reachGroupContextsIn(reflect.ValueOf(control[name]), name, &reached, &blocked, 0)
		refused = append(refused, blocked...)
		if len(reached) != 0 {
			found = append(found, name)
		}
		for _, one := range reached {
			if one.identity == context {
				identity = append(identity, name)
				break
			}
		}
	}
	if len(refused) != 0 {
		t.Errorf("the value walk could not follow %v out of its own control, so it would walk past the same shape in an answer",
			refused)
	}
	if want := slices.Sorted(slices.Values(reachValueControlIdentity)); !slices.Equal(identity, want) {
		t.Fatalf("the value walk found the very pointer through %v, want %v; a name missing is an arm that stopped working and a name extra is a line this file says it draws and does not",
			identity, want)
	}
	if want := slices.Sorted(slices.Values(reachValueControlFound)); !slices.Equal(found, want) {
		t.Fatalf("the value walk reached a group context through %v, want %v", found, want)
	}
}
