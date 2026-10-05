// The v1 proposal profile gate, and the registry the gate's class is derived from.
//
// The plan supplied eight tests for this task. Seven of them state a single code point each --
// psk is refused, reinit is refused, these four are accepted -- and a gate stated that way is the
// defect rule 5 names: the accepted set is written out by hand over a class that GROWS, and this
// project has been walked past that fourteen times, most recently by a table calling itself
// "every rule of the CreateGroup carve-out" that held five rules of six.
//
// So the class is derived here. The ProposalType constants are read out of this package's own
// source -- names AND values, off the syntax tree rather than matched as text -- and the
// production table is held EQUAL to that set in both directions: a code point registered in
// extension.go with no row in proposalTypeProfile fails, and a row for a code point that no
// longer exists fails too. Every per-type assertion below then runs over the derived set rather
// than over a list, so the eighth proposal type registered by anybody is a failure on the commit
// that registers it rather than a silent gap.
//
// The plan's own eight are kept, in their own section, because a derived sweep and a named case
// answer different questions: the sweep says the class is covered, and the named case says what
// the answer for one member is supposed to look like when somebody reads the failure.
package mls

import (
	"bytes"
	"errors"
	"go/ast"
	"go/token"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/urnetwork/connect/mls/syntax"
)

// ---------------------------------------------------------------------------
// the registry, derived
// ---------------------------------------------------------------------------

// proposalTypeRegistryIn reads one parsed file for the proposal type registry it declares,
// answering the code points by name and, separately, every declaration it could not read.
//
// THE UNIT IS THE CONST BLOCK AND NOT THE TYPE ANNOTATION, and that is this derivation's whole
// point. The first version of it skipped every ValueSpec whose Type was nil, which reads as
// "constants of type ProposalType" and is actually "constants whose SOURCE TEXT spells the type
// out". Go does not require it to: a member written as ProposalTypeBranch = 0x0008 beside the
// seven annotated ones is a registration in every sense that matters -- it is inside the
// registry's own declaration, every use of it as a proposal type converts implicitly, and a peer
// sending 0x0008 gets whatever the profile says about that code point -- and it was INVISIBLE.
// Measured: adding exactly that line left TestTheV1ProfileClassifiesEveryRegisteredProposalType,
// TestCheckProposalTypeAnswersTheWholeCodePointSpaceFromTheRegistry,
// TestProposalTypePathRequiredSet and TestProposalTypeNameNamesEveryRegisteredType all passing
// over a registry one code point short. The consequence is safe today -- an unclassified point is
// refused as unregistered -- and the day the profile widens to admit that type, the reason it was
// refused is a table nobody knew was short.
//
// A const block is one declaration and registering a code point means adding a line to it, so the
// block is what survives the spelling. A block that names ProposalType nowhere is not the registry
// and no member of it is a code point, which is what keeps MaxGroupMembers out of the class.
//
// The value is still parsed from the literal, so a code point changed by one is a changed
// derivation and not an agreed one -- a scan that read only the NAMES would agree with 0xF004 as
// happily as 0xF003. A member the literal reading cannot follow is REPORTED rather than skipped:
// an iota'd registry is a legal thing to write and this reader cannot follow one, so it has to
// say so instead of quietly answering a map one code point short, which is the exact failure
// this whole derivation exists to have caught.
func proposalTypeRegistryIn(parsed parsedSource) (map[string]uint64, []string) {
	registered := map[string]uint64{}
	unreadable := []string{}
	for _, declaration := range parsed.file.Decls {
		generic, isGeneric := declaration.(*ast.GenDecl)
		if !isGeneric || generic.Tok != token.CONST {
			continue
		}
		if !proposalTypeConstBlock(parsed, generic) {
			continue
		}
		for _, specification := range generic.Specs {
			value, isValue := specification.(*ast.ValueSpec)
			if !isValue {
				continue
			}
			for i, name := range value.Names {
				if i >= len(value.Values) {
					unreadable = append(unreadable, name.Name+" is declared with no value of its own, and this derivation reads the code point off the literal rather than following an iota")
					continue
				}
				literal, isLiteral := value.Values[i].(*ast.BasicLit)
				if !isLiteral || literal.Kind != token.INT {
					unreadable = append(unreadable, name.Name+" is given a value this derivation cannot read as an integer literal")
					continue
				}
				code, err := strconv.ParseUint(literal.Value, 0, 16)
				if err != nil {
					unreadable = append(unreadable, name.Name+" is given the value "+literal.Value+", which is not a 16 bit code point: "+err.Error())
					continue
				}
				registered[name.Name] = code
			}
		}
	}
	slices.Sort(unreadable)
	return registered, unreadable
}

// proposalTypeConstBlock answers whether one const declaration is the proposal type registry:
// whether ANY member of it carries the registry's type annotation.
//
// One annotated member is enough, because a const block is written as a unit and the annotation
// is what says which registry the unit is. The alternative -- requiring every member to be
// annotated -- is exactly the rule that made an untyped eighth code point invisible.
func proposalTypeConstBlock(parsed parsedSource, generic *ast.GenDecl) bool {
	for _, specification := range generic.Specs {
		value, isValue := specification.(*ast.ValueSpec)
		if isValue && value.Type != nil && parsed.render(value.Type) == "ProposalType" {
			return true
		}
	}
	return false
}

// proposalTypeRegistry is every proposal type code point this package's non test source
// registers, mapped to the value its declaration gives it.
//
// Read off the syntax tree of the whole package rather than out of extension.go by name, which
// is ledger 21: deriving a class and then writing down the file it lives in is the same defect
// one level up, and a registry constant moved to another file of this package would leave a scan
// keyed on the filename reporting a clean run over a class it had stopped reading.
func proposalTypeRegistry(t *testing.T) map[string]ProposalType {
	t.Helper()
	registered := map[string]ProposalType{}
	for path, parsed := range decoderSourceOfThisPackage(t) {
		found, unreadable := proposalTypeRegistryIn(parsed)
		if len(unreadable) != 0 {
			t.Fatalf("%s declares registry members this derivation cannot read: %v", path, unreadable)
		}
		for name, code := range found {
			if held, already := registered[name]; already && uint16(held) != uint16(code) {
				t.Fatalf("%s is declared twice, as %#04x and %#04x; a wire enum that disagrees by a NUMBER is the drift nothing in this package could see",
					name, uint16(held), uint16(code))
			}
			registered[name] = ProposalType(code)
		}
	}
	// the positive control: a derivation that had stopped resolving would answer an empty map,
	// and an empty map satisfies every set comparison below whose other side was emptied with
	// it. These two are certainly declared, and their values are the RFC's.
	for name, want := range map[string]ProposalType{
		"ProposalTypeAdd":                    0x0001,
		"ProposalTypeGroupContextExtensions": 0x0007,
	} {
		if got, found := registered[name]; !found || got != want {
			t.Fatalf("the derivation read %v out of this package, and %s is certainly declared as %#04x there, so it is reading no constant declaration at all",
				registered, name, uint16(want))
		}
	}
	if _, found := registered["ProposalTypeThisCodePointDoesNotExist"]; found {
		t.Fatal("the derivation reports a constant that cannot exist, so it is matching text rather than declarations")
	}
	return registered
}

// proposalTypeValuesOf is the derived registry as a sorted set of code points.
func proposalTypeValuesOf(registered map[string]ProposalType) []ProposalType {
	values := slices.Collect(maps.Values(registered))
	slices.Sort(values)
	return slices.Compact(values)
}

// proposalTypeRegistryControl is one const declaration of each shape the derivation has to tell
// apart, so that what it reads is measured on a body known to hold every case rather than only on
// a package that happens to be written one way today.
//
// The member that matters is ProposalTypeBranch: a registry entry with NO type annotation of its
// own. The whole registry of the real package is annotated, so the real package cannot tell a
// derivation that reads const blocks from one that reads type annotations -- which is exactly why
// the short version of this reader passed for as long as it did. This control is the only place
// the two answer differently.
const proposalTypeRegistryControl = `package control

type ProposalType uint16

const (
	ProposalTypeReserved ProposalType = 0x0000
	ProposalTypeAdd      ProposalType = 0x0001
	// no type annotation, and a registration all the same: Go reads it as an untyped constant,
	// every use of it as a proposal type converts implicitly, and a peer sending 0x0008 is
	// answered by whatever the profile says about this code point
	ProposalTypeBranch = 0x0008
)

// a registry member declared on its own rather than in the block, which the block rule must
// still reach because a declaration is a declaration
const ProposalTypeLater ProposalType = 0x0009

// a const block of this package that is NOT the registry. No member of it names ProposalType,
// so no member of it is a code point, and a derivation that swept every const of the package
// would report these two as proposal types.
const (
	MaxGroupMembers = 500
	someOtherCap    = 10
)

// a constant whose NAME reads like a registry member and whose declaration is not one, which is
// what says this reads declarations rather than matching text
const proposalTypeNameForLogs = "add"

// and a var, which is not a const declaration at all
var ProposalTypeFromAVar ProposalType = 0x000A
`

// TestTheRegistryDerivationReadsAConstBlockRatherThanATypeAnnotation is the narrowing probe the
// real package cannot supply.
//
// Every ProposalType constant this package declares carries its type in the source text, so the
// annotation reading and the block reading agree over the whole real registry and the suite is
// green under both. Measured: adding ProposalTypeBranch = 0x0008 beside the seven annotated
// constants left TestTheV1ProfileClassifiesEveryRegisteredProposalType,
// TestCheckProposalTypeAnswersTheWholeCodePointSpaceFromTheRegistry,
// TestProposalTypePathRequiredSet and TestProposalTypeNameNamesEveryRegisteredType all passing
// over a registry one code point short -- the gate complete downward and short upward, which is
// the direction that matters, since an unclassified point is refused as unregistered today and is
// a table nobody knew was short on the day the profile widens to admit it.
//
// So the reader is run over a body that holds the case, and the answer is pinned exactly in both
// directions: a member it misses is the defect this replaces, and a member it invents -- a
// MaxGroupMembers read as a code point -- is a gate somebody switches off.
func TestTheRegistryDerivationReadsAConstBlockRatherThanATypeAnnotation(t *testing.T) {
	control := mustParseText(t, "the proposal type registry control", proposalTypeRegistryControl)
	found, unreadable := proposalTypeRegistryIn(control)
	if len(unreadable) != 0 {
		t.Fatalf("the reader could not read %v out of the control", unreadable)
	}
	want := map[string]uint64{
		"ProposalTypeReserved": 0x0000,
		"ProposalTypeAdd":      0x0001,
		"ProposalTypeBranch":   0x0008,
		"ProposalTypeLater":    0x0009,
	}
	if !maps.Equal(found, want) {
		t.Fatalf("the derivation read %v out of the control, want %v; the member with no type annotation is the one this reader exists for, and MaxGroupMembers is the one it must not invent",
			found, want)
	}

	// and the reader REPORTS what it cannot follow rather than answering a short map, which is
	// the other way a derivation goes quiet. An iota'd registry is legal Go and this reader
	// cannot read one.
	iotad := mustParseText(t, "an iota'd registry", `package control

type ProposalType uint16

const (
	ProposalTypeReserved ProposalType = iota
	ProposalTypeAdd
)
`)
	found, unreadable = proposalTypeRegistryIn(iotad)
	if len(unreadable) == 0 {
		t.Fatalf("the reader answered %v over an iota'd registry and reported nothing it could not read; a scan that steps over what it cannot judge reports exactly what a complete one reports",
			found)
	}
	if len(found) != 0 {
		t.Errorf("the reader answered %v over an iota'd registry as well as reporting it; the values there are not the ones the literals say", found)
	}

	// the real package is read by the same function, and it holds the registry the profile is
	// stated over. A control that passed while the reader had stopped resolving real source
	// would be a gate over nothing.
	registered := proposalTypeRegistry(t)
	if len(registered) < 8 {
		t.Fatalf("the derivation read %v out of this package, which registers eight proposal types", registered)
	}
}

// TestTheV1ProfileClassifiesEveryRegisteredProposalType is rule 5 over the class this gate is
// stated on.
//
// The comparison runs in both directions on purpose. A missing row is a code point the gate
// answers "not registered" for, which is the quiet direction: the refusal is still a refusal, so
// nothing fails, and the day the profile widens to admit that type the reason it is refused is a
// table nobody knew was short. A row with no constant is a classification that outlived what it
// classified.
func TestTheV1ProfileClassifiesEveryRegisteredProposalType(t *testing.T) {
	registered := proposalTypeRegistry(t)
	derived := proposalTypeValuesOf(registered)
	classified := slices.Collect(maps.Keys(proposalTypeProfile))
	slices.Sort(classified)
	if !slices.Equal(derived, classified) {
		t.Fatalf("this package registers the proposal types %v and proposalTypeProfile classifies %v; a registered code point with no row is one the gate refuses as unregistered, and a row with no code point is a classification of nothing. The declarations: %v",
			derived, classified, registered)
	}
	t.Logf("%d registered proposal types, all classified: %v", len(derived), registered)
}

// TestEveryRegisteredProposalTypeIsAcceptedOrRefusedByItsOwnSentinel runs the gate over the
// derived class and holds each answer to the row that classifies it.
//
// The set equality above would pass over a table whose every verdict was wrong, and over a
// checkProposalType that ignored the table entirely and returned nil. This is what says the table
// is what the gate reads. The negative half -- that a refusal answers to its OWN sentinel and to
// none of the other three -- is the half that matters most: three refusals collapsed into one
// value pass every test that asks only whether an error came back, and that shape has cost this
// project three authentication bypasses.
func TestEveryRegisteredProposalTypeIsAcceptedOrRefusedByItsOwnSentinel(t *testing.T) {
	registered := proposalTypeRegistry(t)
	// the whole refusal class of this file, derived, and not the four the profile happens to
	// use today. A fifth sentinel a later rule answers is swept here the moment it is declared,
	// which is what stops "answers to its own value and to no other" from meaning "and to no
	// other of the four somebody remembered".
	everySentinel := proposalListOwnedErrors
	accepted, refused := 0, 0
	for _, name := range slices.Sorted(maps.Keys(registered)) {
		proposalType := registered[name]
		want := proposalTypeProfile[proposalType]
		got := defaultProfile().checkProposalType(proposalType)
		if want == nil {
			accepted += 1
			if got != nil {
				t.Errorf("%s is classified as implemented and the gate refused it with %v", name, got)
			}
			continue
		}
		refused += 1
		if !errors.Is(got, want) {
			t.Errorf("%s is classified as refused with %v and the gate answered %v", name, want, got)
			continue
		}
		for sentinelName, other := range everySentinel {
			if other == want || !errors.Is(got, other) {
				continue
			}
			t.Errorf("the refusal of %s answers to %s as well as to its own sentinel; a caller asking which narrowing it hit would be told two of them",
				name, sentinelName)
		}
		// the detail names the type, so a peer told its proposal was refused does not have to
		// diff two captures to find out which one
		if got := got.Error(); !strings.Contains(got, proposalTypeName(proposalType)) {
			t.Errorf("the refusal of %s reads %q and does not name the type that was refused", name, got)
		}
	}
	if accepted == 0 || refused == 0 {
		t.Fatalf("the sweep saw %d accepted and %d refused types; a run with nothing on one side states only half the rule",
			accepted, refused)
	}
	t.Logf("%d registered types accepted by the v1 profile, %d refused", accepted, refused)
}

// TestProposalTypeNameNamesEveryRegisteredType holds the error message's vocabulary to the
// derived registry.
//
// A name is not a decoration here: it is what a peer is told when this build refuses its
// proposal, and a type that fell through to the hex fallback would be reported as
// proposal_type(0x0008) to somebody who could have been told "branch".
func TestProposalTypeNameNamesEveryRegisteredType(t *testing.T) {
	registered := proposalTypeRegistry(t)
	named := map[string]ProposalType{}
	for _, constant := range slices.Sorted(maps.Keys(registered)) {
		proposalType := registered[constant]
		name := proposalTypeName(proposalType)
		if strings.Contains(name, "proposal_type(") {
			t.Errorf("%s (%#04x) falls through to the hex fallback and is reported as %q", constant, uint16(proposalType), name)
			continue
		}
		if held, already := named[name]; already && held != proposalType {
			t.Errorf("%s and %#04x are both named %q, so a refusal naming it says nothing about which arrived",
				constant, uint16(held), name)
		}
		named[name] = proposalType
	}
	// and the fallback is reached by something, so it is not dead prose: an unregistered code
	// point has to be reportable
	unregistered := ProposalType(0x0A0A)
	if _, registeredHere := proposalTypeProfile[unregistered]; registeredHere {
		t.Fatalf("%#04x has become a registered type and is no longer the unregistered control", uint16(unregistered))
	}
	if got := proposalTypeName(unregistered); !strings.Contains(got, "0x0a0a") {
		t.Errorf("an unregistered code point is named %q and does not carry its value", got)
	}
}

// TestProposalTypePathRequiredSet is RFC 9420 section 12.4's pathRequiredTypes, stated over the
// derived registry rather than over a list of seven.
//
// The plan's version is a table of seven rows. This is the same table with one clause added: it
// must cover every type the registry declares, so an eighth code point cannot be added without
// somebody deciding whether a commit carrying it needs an update path -- which is a question with
// a security answer, since a commit that skipped the path is a commit that did not rotate the
// committer's key.
func TestProposalTypePathRequiredSet(t *testing.T) {
	registered := proposalTypeRegistry(t)
	// RFC 9420 section 12.4: the path required set is update, remove, external_init and
	// group_context_extensions. add, psk and reinit do not require one, and the reserved code
	// point is not a proposal at all.
	required := map[string]bool{
		"ProposalTypeReserved":               false,
		"ProposalTypeAdd":                    false,
		"ProposalTypeUpdate":                 true,
		"ProposalTypeRemove":                 true,
		"ProposalTypePreSharedKey":           false,
		"ProposalTypeReInit":                 false,
		"ProposalTypeExternalInit":           true,
		"ProposalTypeGroupContextExtensions": true,
	}
	if !slices.Equal(slices.Sorted(maps.Keys(required)), slices.Sorted(maps.Keys(registered))) {
		t.Fatalf("this table decides the path requirement for %v and the registry declares %v; a registered type with no row here is a commit shape nobody ruled on",
			slices.Sorted(maps.Keys(required)), slices.Sorted(maps.Keys(registered)))
	}
	for _, name := range slices.Sorted(maps.Keys(required)) {
		if got := proposalTypePathRequired(registered[name]); got != required[name] {
			t.Errorf("proposalTypePathRequired(%s) = %v, want %v", name, got, required[name])
		}
	}
	// an unregistered code point requires no path, because this build never processes one and a
	// true answer would demand a path for a proposal it refuses
	if proposalTypePathRequired(ProposalType(0x0A0A)) {
		t.Error("an unregistered proposal type is reported as requiring an update path")
	}
}

// ---------------------------------------------------------------------------
// the gate, over the whole class
// ---------------------------------------------------------------------------

// proposalOfRegisteredType builds a well formed Proposal of one registered type: the arm that
// type names, populated, and nothing else.
//
// The switch is held to the derived registry by its caller, so a registered type with no case
// here fails rather than being quietly skipped -- which is the failure mode of every sweep whose
// fixture builder is narrower than the class it sweeps.
func proposalOfRegisteredType(t *testing.T, crypto CryptoProvider, member *testMember,
	proposalType ProposalType) *Proposal {

	t.Helper()
	switch proposalType {
	case ProposalTypeReserved:
		// there is no arm for the reserved code point and there cannot be one; the type rule
		// runs before the arm rule, which is what this fixture makes the sweep observe
		return &Proposal{ProposalType: ProposalTypeReserved}
	case ProposalTypeAdd:
		keyPackage, _, _ := testKeyPackage(t, crypto, member)
		return &Proposal{ProposalType: proposalType, Add: &Add{KeyPackage: *keyPackage}}
	case ProposalTypeUpdate:
		leaf, _ := testLeafNode(t, crypto, member)
		return &Proposal{ProposalType: proposalType, Update: &Update{LeafNode: *leaf}}
	case ProposalTypeRemove:
		return &Proposal{ProposalType: proposalType, Remove: &Remove{Removed: LeafIndex(1)}}
	case ProposalTypePreSharedKey:
		return &Proposal{ProposalType: proposalType, PreSharedKey: &PreSharedKey{}}
	case ProposalTypeReInit:
		return &Proposal{ProposalType: proposalType, ReInit: &ReInit{}}
	case ProposalTypeExternalInit:
		return &Proposal{ProposalType: proposalType, ExternalInit: &ExternalInit{}}
	case ProposalTypeGroupContextExtensions:
		return &Proposal{ProposalType: proposalType, GroupContextExtensions: &GroupContextExtensions{}}
	}
	t.Fatalf("no arm fixture for the registered proposal type %#04x; a sweep whose builder is narrower than its class passes over the members it cannot build",
		uint16(proposalType))
	return nil
}

// TestTheProfileGateAnswersEveryRegisteredProposalTypeAsItIsClassified is the whole gate over the
// whole derived class, on proposals whose arm is the one their type names.
//
// The type rule is observed to run BEFORE the arm rule, which is what the reserved code point's
// fixture is for: it has no arm, both rules would refuse it, and only the ordering decides which
// sentinel a caller sees. An ordering that put the arm rule first would report every refused type
// as an arm mismatch and lose the three names the profile has.
func TestTheProfileGateAnswersEveryRegisteredProposalTypeAsItIsClassified(t *testing.T) {
	crypto := testCrypto(t)
	member := testIdentity(t, crypto, "bob")
	registered := proposalTypeRegistry(t)
	for _, name := range slices.Sorted(maps.Keys(registered)) {
		proposalType := registered[name]
		proposal := proposalOfRegisteredType(t, crypto, member, proposalType)
		got := checkProposalProfile(defaultProfile(), proposal)
		want := proposalTypeProfile[proposalType]
		if want == nil {
			if got != nil {
				t.Errorf("a well formed %s proposal was refused with %v", name, got)
			}
			continue
		}
		if !errors.Is(got, want) {
			t.Errorf("a %s proposal was refused with %v, want %v", name, got, want)
		}
		if errors.Is(got, ErrContentArmMismatch) {
			t.Errorf("a %s proposal was refused as an arm mismatch; the profile rule has to run first or every refused type loses its name",
				name)
		}
	}
}

// ---------------------------------------------------------------------------
// the plan's tests
// ---------------------------------------------------------------------------

func TestProfileGateRefusesPsk(t *testing.T) {
	proposal := &Proposal{ProposalType: ProposalTypePreSharedKey, PreSharedKey: &PreSharedKey{}}
	if err := checkProposalProfile(defaultProfile(), proposal); !errors.Is(err, errProfilePsk) {
		t.Fatalf("checkProposalProfile error = %v, want errProfilePsk", err)
	}
}

func TestProfileGateRefusesReInit(t *testing.T) {
	proposal := &Proposal{ProposalType: ProposalTypeReInit, ReInit: &ReInit{}}
	if err := checkProposalProfile(defaultProfile(), proposal); !errors.Is(err, errProfileReInit) {
		t.Fatalf("checkProposalProfile error = %v, want errProfileReInit", err)
	}
}

func TestProfileGateRefusesExternalInit(t *testing.T) {
	proposal := &Proposal{ProposalType: ProposalTypeExternalInit, ExternalInit: &ExternalInit{}}
	if err := checkProposalProfile(defaultProfile(), proposal); !errors.Is(err, errProfileExternalCommit) {
		t.Fatalf("checkProposalProfile error = %v, want errProfileExternalCommit", err)
	}
}

// TestProfileGateRefusesGreaseType is the plan's, over the GREASE arm the codec decodes into.
//
// The codec decodes an unregistered proposal into UnknownType and UnknownBody so the messages
// vector family can round trip it. This plan still refuses it: a GREASE proposal in a commit is
// ValSem113, and a proposal whose arm this build cannot read is one it must drop rather than skip.
func TestProfileGateRefusesGreaseType(t *testing.T) {
	proposal := &Proposal{ProposalType: ProposalType(0x0A0A), UnknownType: ProposalType(0x0A0A)}
	err := checkProposalProfile(defaultProfile(), proposal)
	if !errors.Is(err, errUnregisteredProposalType) {
		t.Fatalf("checkProposalProfile error = %v, want errUnregisteredProposalType", err)
	}
	// and it is refused as UNREGISTERED and not as a forgery, which is a statement about WHICH
	// RULE owns this input. A GREASE proposal carries UnknownType equal to ProposalType -- that is
	// how proposal_wire.go makes it round trip -- and what is wrong with it is that nothing here
	// knows the code point, not that anything disagrees with anything. While the two rules shared
	// one sentinel this assertion could not be made at all.
	if errors.Is(err, errForgedProposalDiscriminant) {
		t.Errorf("a GREASE proposal decoded as the codec decodes one was answered %v, which names the forged discriminant rule; its discriminant is exactly the type it names",
			err)
	}
	// the same refusal over a value whose UnknownType is NOT set, which is the half the plan's
	// version cannot state. This one is refused by the type rule or by nothing.
	byTypeAlone := &Proposal{ProposalType: ProposalType(0x0A0A), UnknownBody: []byte{0x01}}
	if err := checkProposalProfile(defaultProfile(), byTypeAlone); !errors.Is(err, errUnregisteredProposalType) {
		t.Fatalf("an unregistered proposal type carrying no forged discriminant was answered %v, want errUnregisteredProposalType",
			err)
	}
}

// TestCheckProposalTypeAnswersTheWholeCodePointSpaceFromTheRegistry is the total statement of the
// one refusal surface, over all 65536 code points rather than over the eight the registry names.
//
// The derived sweep next door covers the REGISTERED types, which leaves the complement -- every
// GREASE value, and every code point registered after this was written -- stated by nothing. That
// gap is not theoretical: disabling the "not registered" branch of checkProposalType leaves every
// named test in this file passing, because a decoded GREASE proposal carries UnknownType and is
// caught by the forged discriminant clause instead. Two rules, one of which had silently stopped
// running.
func TestCheckProposalTypeAnswersTheWholeCodePointSpaceFromTheRegistry(t *testing.T) {
	registered := proposalTypeValuesOf(proposalTypeRegistry(t))
	unregistered, accepted := 0, 0
	for value := 0; value <= 0xffff; value += 1 {
		proposalType := ProposalType(value)
		got := defaultProfile().checkProposalType(proposalType)
		if !slices.Contains(registered, proposalType) {
			unregistered += 1
			if !errors.Is(got, errUnregisteredProposalType) {
				t.Fatalf("the unregistered code point %#04x was answered %v, want errUnregisteredProposalType",
					value, got)
			}
			continue
		}
		want := proposalTypeProfile[proposalType]
		if want == nil {
			accepted += 1
			if got != nil {
				t.Fatalf("the registered code point %#04x is classified as implemented and was answered %v",
					value, got)
			}
			continue
		}
		if !errors.Is(got, want) {
			t.Fatalf("the registered code point %#04x was answered %v, want %v", value, got, want)
		}
	}
	if unregistered == 0 || accepted == 0 {
		t.Fatalf("the sweep saw %d unregistered and %d accepted code points; a run with nothing on one side states only half the rule",
			unregistered, accepted)
	}
	t.Logf("%d unregistered code points refused, %d registered and accepted", unregistered, accepted)
}

func TestProfileGateAcceptsTheFourV1Types(t *testing.T) {
	crypto := testCrypto(t)
	bob := testIdentity(t, crypto, "bob")
	keyPackage, _, _ := testKeyPackage(t, crypto, bob)
	leaf, _ := testLeafNode(t, crypto, bob)
	for _, proposal := range []*Proposal{
		{ProposalType: ProposalTypeAdd, Add: &Add{KeyPackage: *keyPackage}},
		{ProposalType: ProposalTypeUpdate, Update: &Update{LeafNode: *leaf}},
		{ProposalType: ProposalTypeRemove, Remove: &Remove{Removed: LeafIndex(1)}},
		{ProposalType: ProposalTypeGroupContextExtensions, GroupContextExtensions: &GroupContextExtensions{}},
	} {
		if err := checkProposalProfile(defaultProfile(), proposal); err != nil {
			t.Fatalf("checkProposalProfile(%s) = %v, want nil", proposalTypeName(proposal.ProposalType), err)
		}
	}
}

// TestProfileGateRefusesAMismatchedArm is the plan's, with the sentinel named.
//
// The plan's version asks only that an error came back, which a gate that refused every proposal
// would satisfy. The rule it is about is proposal_wire.go's -- the discriminant and the populated
// arm have to agree -- so the value it answers is that rule's sentinel, and NOT the profile's:
// two rules that answer one value are two rules a caller cannot tell apart.
func TestProfileGateRefusesAMismatchedArm(t *testing.T) {
	proposal := &Proposal{ProposalType: ProposalTypeAdd, Remove: &Remove{Removed: 1}}
	err := checkProposalProfile(defaultProfile(), proposal)
	if !errors.Is(err, ErrContentArmMismatch) {
		t.Fatalf("checkProposalProfile error = %v, want ErrContentArmMismatch", err)
	}
	// a SECOND arm hanging off a proposal whose named arm is present is the other half of the
	// same rule, and it is the half a reference cares about: two Proposal values encoding to one
	// run of octets carry one ProposalRef, and the arm that was dropped is a change the proposer
	// believed it had published
	crypto := testCrypto(t)
	bob := testIdentity(t, crypto, "bob")
	keyPackage, _, _ := testKeyPackage(t, crypto, bob)
	both := &Proposal{
		ProposalType: ProposalTypeAdd,
		Add:          &Add{KeyPackage: *keyPackage},
		Remove:       &Remove{Removed: 1},
	}
	if err := checkProposalProfile(defaultProfile(), both); !errors.Is(err, ErrContentArmMismatch) {
		t.Fatalf("a proposal carrying two arms was accepted with %v, want ErrContentArmMismatch", err)
	}
}

func TestProposalCodecIsTheFramingPlans(t *testing.T) {
	// This plan declares no Proposal codec. It uses the one the framing plan ships, through the
	// single byte level entry points, and the round trip is asserted here because every cached
	// ProposalRef is over these bytes.
	proposal := &Proposal{ProposalType: ProposalTypeRemove, Remove: &Remove{Removed: LeafIndex(3)}}
	encoded, err := syntax.Marshal(proposal)
	if err != nil {
		t.Fatalf("syntax.Marshal: %v", err)
	}
	var parsed Proposal
	if err := syntax.Unmarshal(encoded, &parsed); err != nil {
		t.Fatalf("syntax.Unmarshal: %v", err)
	}
	if parsed.ProposalType != ProposalTypeRemove || parsed.Remove == nil || parsed.Remove.Removed != 3 {
		t.Fatalf("parsed = %+v", parsed)
	}
	if err := syntax.Unmarshal(append(encoded, 0xFF), &parsed); !errors.Is(err, syntax.ErrTrailingBytes) {
		t.Fatalf("syntax.Unmarshal with a trailing byte = %v, want ErrTrailingBytes", err)
	}
}

// ---------------------------------------------------------------------------
// the seams the plan's gate does not have
// ---------------------------------------------------------------------------

// TestTheProfileGateRefusesAForgedWireDiscriminant is the rule the plan's sketch has no clause
// for, and it is the one with teeth.
//
// Proposal.MarshalMLS writes UnknownType as the discriminant when it is set and selects the ARM
// by ProposalType. That is deliberate and the codec must keep it: it is how the validation plan's
// forge emits a well formed body under a GREASE code point without a second encoder existing. But
// it means the type this gate is asked about and the type the OCTETS carry can differ, and every
// ProposalRef, every transcript hash and every receiver's own gate run on the octets. Without the
// clause, a proposal is admitted here as an Add and encoded under 0x0005, so the proposer holds a
// reference to an Add and every receiver reads a reinit this profile refuses.
func TestTheProfileGateRefusesAForgedWireDiscriminant(t *testing.T) {
	crypto := testCrypto(t)
	bob := testIdentity(t, crypto, "bob")
	keyPackage, _, _ := testKeyPackage(t, crypto, bob)
	forged := &Proposal{
		ProposalType: ProposalTypeAdd,
		Add:          &Add{KeyPackage: *keyPackage},
		UnknownType:  ProposalTypeReInit,
	}
	// the seam is real: the octets this value encodes to carry the reinit discriminant, and the
	// gate is being asked about an add
	encoded, err := syntax.Marshal(forged)
	if err != nil {
		t.Fatalf("syntax.Marshal the forged proposal: %v", err)
	}
	if len(encoded) < 2 || ProposalType(uint16(encoded[0])<<8|uint16(encoded[1])) != ProposalTypeReInit {
		t.Fatalf("the forged proposal encodes under the discriminant %x, and this test is about the case where that is not the add it claims to be",
			encoded[:min(2, len(encoded))])
	}
	err = checkProposalProfile(defaultProfile(), forged)
	if !errors.Is(err, errForgedProposalDiscriminant) {
		t.Fatalf("a proposal that names an add and encodes as a reinit was answered %v, want errForgedProposalDiscriminant",
			err)
	}
	// and it is NOT the refusal a type outside the profile gets, which is what the collapse cost:
	// the caller was told "proposal type is not one this build processes" about a proposal whose
	// type is add, and add is one this build processes. The two are opposite faults -- a peer sent
	// a type we do not implement, versus our own commit builder produced octets every receiver
	// will read as a different proposal than the one we hold a reference to.
	for name, other := range map[string]error{
		"errProfileReInit":            errProfileReInit,
		"errUnregisteredProposalType": errUnregisteredProposalType,
		"errReservedProposalType":     errReservedProposalType,
		"errNilProposal":              errNilProposal,
	} {
		if errors.Is(err, other) {
			t.Errorf("the forged discriminant refusal answers to %s as well as to its own sentinel; a caller cannot tell a message to drop from a message this build must not have built",
				name)
		}
	}
	// and the same forge under an accepted type, so the clause is about the DISAGREEMENT rather
	// than about which type was forged
	alsoForged := &Proposal{
		ProposalType: ProposalTypeAdd,
		Add:          &Add{KeyPackage: *keyPackage},
		UnknownType:  ProposalTypeRemove,
	}
	if err := checkProposalProfile(defaultProfile(), alsoForged); !errors.Is(err, errForgedProposalDiscriminant) {
		t.Fatalf("a proposal that names an add and encodes as a remove was answered %v, want errForgedProposalDiscriminant",
			err)
	}
}

// TestTheForgedDiscriminantClauseIsAboutTheDisagreementAndNotAboutUnknownTypeBeingSet is the
// test the two fixtures next door cannot be: both of them forge a genuine disagreement, so a
// clause reading "UnknownType is set at all" and one reading "UnknownType is not the type"
// answer identically over every input in that test.
//
// Measured: narrowing the landed clause from the first rule to the second left the whole of
// ./mls/... and ./message/... green, while the clause's own comment and its test both CLAIMED
// the second. So the file implemented one rule and documented another, and nothing could tell.
//
// EXACTLY ONE INPUT separates the two, and finding that out is what makes this test small and
// makes it the whole statement. A refused or unregistered type is answered by the type rule
// before this clause runs; a genuine disagreement is refused by both readings; so the only
// input left is an ACCEPTED type whose UnknownType equals it, and that is the second half below.
// MarshalMLS writes the same octets it would write with UnknownType unset, so refusing it
// refuses a proposal that is not wrong about anything, under a message reading "add would be
// encoded under the discriminant add".
//
// The first half is about ORDER rather than about the clause, and it is here because the split
// of the sentinels is what makes it askable: a GREASE proposal off the wire carries UnknownType
// equal to ProposalType, and the rule it breaks is the registry rule. Under the wide reading
// this clause was a second answer to that same input, reachable whenever the type rule did not
// run first, which is not a backstop -- it is two rules answering one message with two
// sentinels, and the caller told the second one goes looking for a bug in its own commit path.
func TestTheForgedDiscriminantClauseIsAboutTheDisagreementAndNotAboutUnknownTypeBeingSet(t *testing.T) {
	crypto := testCrypto(t)
	bob := testIdentity(t, crypto, "bob")
	keyPackage, _, _ := testKeyPackage(t, crypto, bob)

	// the GREASE proposal the codec itself builds, round tripped rather than hand written, so
	// this is the value that actually arrives off the wire and not one this test invented
	grease := &Proposal{ProposalType: ProposalType(0x0A0A), UnknownType: ProposalType(0x0A0A),
		UnknownBody: []byte{0xde, 0xad}}
	encoded, err := syntax.Marshal(grease)
	if err != nil {
		t.Fatalf("syntax.Marshal the grease proposal: %v", err)
	}
	decoded := &Proposal{}
	if err := syntax.Unmarshal(encoded, decoded); err != nil {
		t.Fatalf("syntax.Unmarshal the grease proposal: %v", err)
	}
	if decoded.UnknownType != decoded.ProposalType {
		t.Fatalf("the decoder answered UnknownType %#04x for ProposalType %#04x; this test is about the case where a decoded proposal carries the two EQUAL and there is nothing to disagree about",
			uint16(decoded.UnknownType), uint16(decoded.ProposalType))
	}
	got := checkProposalProfile(defaultProfile(), decoded)
	if !errors.Is(got, errUnregisteredProposalType) {
		t.Errorf("a GREASE proposal off the wire was answered %v, want errUnregisteredProposalType: the rule it breaks is that nothing here knows the code point",
			got)
	}
	if errors.Is(got, errForgedProposalDiscriminant) {
		t.Errorf("a GREASE proposal off the wire was refused as a forged discriminant; its discriminant is exactly the type it names")
	}
	// and the clause itself is asked directly, with the type rule taken out of the way, so this
	// states what the CLAUSE does rather than what the ordering hides. A profile that classified
	// the GREASE point as accepted would send it straight past rule 1 and into rule 2, and under
	// the wide reading rule 2 answers it. The row is removed by the cleanup whether this passes
	// or fails, because every other test in this file derives its class off the same table.
	widened := decoded.ProposalType
	if _, already := proposalTypeProfile[widened]; already {
		t.Fatalf("%#04x is already classified, so this half is not modelling a profile that admits it", uint16(widened))
	}
	proposalTypeProfile[widened] = nil
	t.Cleanup(func() { delete(proposalTypeProfile, widened) })
	if err := checkProposalProfile(defaultProfile(), decoded); err != nil {
		t.Errorf("with %#04x accepted by the profile, the gate still refused a proposal carrying it with %v; its discriminant is the type it names and its arm is the verbatim body, so nothing about it disagrees",
			uint16(widened), err)
	}

	// and the accepted type carrying its own discriminant, which encodes to the octets a plain
	// add encodes to and is therefore a proposal nothing can read two ways
	agreeing := &Proposal{ProposalType: ProposalTypeAdd, Add: &Add{KeyPackage: *keyPackage},
		UnknownType: ProposalTypeAdd}
	plain := &Proposal{ProposalType: ProposalTypeAdd, Add: &Add{KeyPackage: *keyPackage}}
	withDiscriminant, err := syntax.Marshal(agreeing)
	if err != nil {
		t.Fatalf("syntax.Marshal the agreeing proposal: %v", err)
	}
	withoutDiscriminant, err := syntax.Marshal(plain)
	if err != nil {
		t.Fatalf("syntax.Marshal the plain proposal: %v", err)
	}
	if !bytes.Equal(withDiscriminant, withoutDiscriminant) {
		t.Fatalf("the two encode differently, so this test is not about a proposal that is indistinguishable on the wire from an ordinary add")
	}
	if err := checkProposalProfile(defaultProfile(), agreeing); err != nil {
		t.Errorf("an add whose discriminant is add was refused with %v; the octets it produces are an ordinary add's, so the disagreement this rule is about does not exist here",
			err)
	}
}

// TestTheProfileGateRefusesANilProposal states the one argument shape a caller can reach it with
// that has no type at all, and holds it to a value of its own.
//
// A nil proposal is not an unsupported TYPE: there is no type. It is a caller that reached the
// gate holding nothing, which is a commit path defect rather than a message to drop -- the
// opposite remedy from every other refusal here, which is why sharing a value with them was
// wrong.
func TestTheProfileGateRefusesANilProposal(t *testing.T) {
	err := checkProposalProfile(defaultProfile(), nil)
	if !errors.Is(err, errNilProposal) {
		t.Fatalf("checkProposalProfile(profile, nil) = %v, want errNilProposal", err)
	}
	for name, other := range map[string]error{
		"errUnregisteredProposalType":   errUnregisteredProposalType,
		"errReservedProposalType":       errReservedProposalType,
		"errForgedProposalDiscriminant": errForgedProposalDiscriminant,
	} {
		if errors.Is(err, other) {
			t.Errorf("the nil refusal answers to %s as well; a caller told a peer sent something unsupported would go looking at the wire for a bug in its own commit path",
				name)
		}
	}
}

// TestTheReservedCodePointIsRefusedAsItsOwnRuleAndNotAsAnUnregisteredOne is ledger 17 for
// errReservedProposalType.
//
// 0x0000 IS in the RFC 9420 section 17.5 registry; what it is registered as is "reserved", which
// is not a proposal. A caller told "not in this build's registry" about it would go looking for a
// registration that is right there in extension.go, and a build that later widened its registry
// would have no way to tell the two apart.
func TestTheReservedCodePointIsRefusedAsItsOwnRuleAndNotAsAnUnregisteredOne(t *testing.T) {
	err := defaultProfile().checkProposalType(ProposalTypeReserved)
	if !errors.Is(err, errReservedProposalType) {
		t.Fatalf("checkProposalType(reserved) = %v, want errReservedProposalType", err)
	}
	if errors.Is(err, errUnregisteredProposalType) {
		t.Error("the reserved code point is refused as unregistered; it is registered, and what it is registered as is not a proposal")
	}
	// and the whole gate answers the same value over a proposal carrying it, so the two doors
	// this file has agree about which rule the reserved point breaks
	if err := checkProposalProfile(defaultProfile(), &Proposal{ProposalType: ProposalTypeReserved}); !errors.Is(err, errReservedProposalType) {
		t.Fatalf("checkProposalProfile over a reserved proposal = %v, want errReservedProposalType", err)
	}
}

// TestTheProfileGateWithNoProfileRunsTheDefaultOne holds the nil profile fallback to the same
// answers the default profile gives, rather than to "no error".
//
// A fallback that accepted everything when handed no profile would be a gate a caller switches
// off by forgetting an argument, and every call site in this plan passes the default anyway --
// which is exactly why nothing would notice.
func TestTheProfileGateWithNoProfileRunsTheDefaultOne(t *testing.T) {
	crypto := testCrypto(t)
	member := testIdentity(t, crypto, "bob")
	registered := proposalTypeRegistry(t)
	for _, name := range slices.Sorted(maps.Keys(registered)) {
		proposalType := registered[name]
		proposal := proposalOfRegisteredType(t, crypto, member, proposalType)
		withDefault := checkProposalProfile(defaultProfile(), proposal)
		withNone := checkProposalProfile(nil, proposal)
		if (withDefault == nil) != (withNone == nil) {
			t.Errorf("%s is answered %v under the default profile and %v under none", name, withDefault, withNone)
			continue
		}
		if withDefault != nil && !errors.Is(withNone, proposalTypeProfile[proposalType]) {
			t.Errorf("%s is refused with %v under the default profile and %v under none", name, withDefault, withNone)
		}
	}
}

// ---------------------------------------------------------------------------
// the per epoch proposal cache and commit resolution
// ---------------------------------------------------------------------------

// testProposalContent wraps a proposal in the AuthenticatedContent shape the cache stores,
// without going through a full group. The group id and epoch are the ones every test that is not
// about the epoch binding uses.
//
// The provider is in the signature and unread, because the group lifecycle plan writes this
// fixture with one and the tasks after this one call it that way.
func testProposalContent(t *testing.T, crypto CryptoProvider, sender LeafIndex,
	proposal *Proposal) *AuthenticatedContent {

	t.Helper()
	return testProposalContentAt(t, sender, []byte("group"), 1, proposal)
}

// testProposalContentAt is the same fixture with the group id and epoch named, which is what the
// epoch binding tests need and what makes the binding observable at all.
func testProposalContentAt(t *testing.T, sender LeafIndex, groupId []byte, epoch uint64,
	proposal *Proposal) *AuthenticatedContent {

	t.Helper()
	return &AuthenticatedContent{
		WireFormat: WireFormatPrivateMessage,
		Content: FramedContent{
			GroupId:     groupId,
			Epoch:       epoch,
			Sender:      Sender{SenderType: SenderTypeMember, LeafIndex: sender},
			ContentType: ContentTypeProposal,
			Proposal:    proposal,
		},
		Auth: FramedContentAuthData{Signature: []byte("sig")},
	}
}

// testResolveContextAt is the group context a resolution runs under, with the group and epoch
// named.
//
// Only the two fields the cache compares are filled. Resolve reads GroupId and Epoch and nothing
// else, so a fixture carrying a tree hash would be stating that the rest matters -- and a later
// reader would then be unable to tell which field a refusal came from.
func testResolveContextAt(groupId []byte, epoch uint64) *GroupContext {
	return &GroupContext{
		Version:     ProtocolVersionMls10,
		CipherSuite: CipherSuiteX25519ChaCha20Sha256Ed25519,
		GroupId:     groupId,
		Epoch:       epoch,
	}
}

// testResolveContext is the epoch every test that is NOT about the epoch binding resolves in: the
// group id and epoch testProposalContent stamps on a cached proposal.
//
// It has to agree with that fixture and the agreement is asserted rather than assumed --
// TestTheResolutionFixtureRunsInTheEpochTheCacheFixtureStoresIn is what says so. Two fixtures
// drifting apart would turn every resolution in this file into an out-of-epoch refusal, and the
// tests that assert an error would go on passing.
func testResolveContext() *GroupContext {
	return testResolveContextAt([]byte("group"), 1)
}

// TestTheResolutionFixtureRunsInTheEpochTheCacheFixtureStoresIn joins the two fixtures above, so
// the twenty resolutions in this file are known to be running in the epoch their entries belong
// to rather than passing for some other reason.
func TestTheResolutionFixtureRunsInTheEpochTheCacheFixtureStoresIn(t *testing.T) {
	crypto := testCrypto(t)
	stored := testProposalContent(t, crypto, LeafIndex(1), testRemoveProposal(LeafIndex(4)))
	resolving := testResolveContext()
	if !bytes.Equal(stored.Content.GroupId, resolving.GroupId) || stored.Content.Epoch != resolving.Epoch {
		t.Fatalf("the cache fixture stores in epoch %d of group %x and the resolution fixture runs in epoch %d of group %x; every resolution in this file is then out of epoch",
			stored.Content.Epoch, stored.Content.GroupId, resolving.Epoch, resolving.GroupId)
	}
}

// testRemoveProposal is a well formed remove of one leaf, which is the cheapest proposal that
// carries a value a test can tell two of apart.
func testRemoveProposal(removed LeafIndex) *Proposal {
	return &Proposal{ProposalType: ProposalTypeRemove, Remove: &Remove{Removed: removed}}
}

// testCacheAt is a cache bound to one epoch, with the constructor's refusal taken here rather
// than checked at forty call sites.
//
// The refusal itself is held by TestACacheBoundToNothingRefusesRatherThanBindingItselfLater and by
// the derived nil argument sweep, so a helper that swallowed it would be swallowing something two
// gates already read. What it must not do is hide it: t.Fatalf and not a discarded error, because
// a nil cache handed on from here would fail every later line as a dereference rather than as the
// constructor refusing.
func testCacheAt(t *testing.T, groupContext *GroupContext) *ProposalCache {
	t.Helper()
	cache, err := NewProposalCache(testVerifiedContextAt(t, groupContext))
	if err != nil {
		t.Fatalf("NewProposalCache(epoch %d of group %x): %v", groupContext.Epoch, groupContext.GroupId, err)
	}
	return cache
}

// testCache is the cache every test that is NOT about the epoch binding stores into: bound to the
// epoch testProposalContent stamps on a proposal and testResolveContext resolves in.
func testCache(t *testing.T) *ProposalCache {
	t.Helper()
	return testCacheAt(t, testResolveContext())
}

// testStoredRemove caches one remove sent by one member and answers its reference.
func testStoredRemove(t *testing.T, crypto CryptoProvider, cache *ProposalCache,
	sender LeafIndex, removed LeafIndex) ProposalRef {

	t.Helper()
	ref, err := cache.Store(crypto, testResolveContext(),
		testProposalContent(t, crypto, sender, testRemoveProposal(removed)))
	if err != nil {
		t.Fatalf("Store(remove %d from leaf %d): %v", removed, sender, err)
	}
	return ref
}

// ---------------------------------------------------------------------------
// what a reference names
// ---------------------------------------------------------------------------

func TestProposalCacheResolvesByReference(t *testing.T) {
	crypto := testCrypto(t)
	cache := testCache(t)

	ref := testStoredRemove(t, crypto, cache, LeafIndex(1), LeafIndex(4))
	cached, ok := cache.Cached(testResolveContext(), ref)
	if !ok || cached.Sender != 1 || cached.ByValue {
		t.Fatalf("Cached = %+v %v", cached, ok)
	}

	list, err := cache.Resolve(crypto, testResolveContext(), LeafIndex(0),
		[]ProposalOrRef{{Type: ProposalOrRefTypeReference, Reference: ref}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(list.Removes()) != 1 || list.Removes()[0].Proposal.Remove.Removed != 4 {
		t.Fatalf("Removes = %+v", list.Removes())
	}
	if list.Removes()[0].Sender != 1 {
		t.Fatal("the resolved proposal must keep its original sender, not the committer")
	}
	if list.Removes()[0].ByValue {
		t.Fatal("a proposal named by reference is not one the commit carried by value")
	}
}

// TestTheCacheAnswersTheReferenceItWasAskedForAndNotTheFirstOneItHolds is the substitution
// property, and it is the one a single entry cache cannot state.
//
// Three entries, each looked up in turn and each required to answer ITS OWN proposal. A lookup
// that answered the first entry it holds -- or the last, or whichever the map iterated to first
// -- passes every test that stores one proposal and gets it back, and it is a proposal
// substitution: the commit names one member's removal and the group applies another's, under a
// reference every peer verifies and agrees with.
func TestTheCacheAnswersTheReferenceItWasAskedForAndNotTheFirstOneItHolds(t *testing.T) {
	crypto := testCrypto(t)
	cache := testCache(t)
	senders := []LeafIndex{1, 2, 3}
	removals := []LeafIndex{7, 8, 9}
	refs := []ProposalRef{}
	for i := range senders {
		refs = append(refs, testStoredRemove(t, crypto, cache, senders[i], removals[i]))
	}
	for i, ref := range refs {
		cached, ok := cache.Cached(testResolveContext(), ref)
		if !ok {
			t.Fatalf("Cached(entry %d) missed a reference this cache answered", i)
		}
		if cached.Proposal.Remove.Removed != removals[i] || cached.Sender != senders[i] {
			t.Errorf("Cached(entry %d) answered a removal of leaf %d sent by leaf %d, want %d sent by %d",
				i, cached.Proposal.Remove.Removed, cached.Sender, removals[i], senders[i])
		}
		list, err := cache.Resolve(crypto, testResolveContext(), LeafIndex(0),
			[]ProposalOrRef{{Type: ProposalOrRefTypeReference, Reference: ref}})
		if err != nil {
			t.Fatalf("Resolve(entry %d): %v", i, err)
		}
		if len(list.Removes()) != 1 || list.Removes()[0].Proposal.Remove.Removed != removals[i] ||
			list.Removes()[0].Sender != senders[i] {
			t.Errorf("Resolve(entry %d) answered %+v, want a removal of leaf %d sent by leaf %d",
				i, list.Removes(), removals[i], senders[i])
		}
	}
}

// TestTheCacheKeyIsTheWholeReferenceAndNotAPrefixOfIt is the truncation property, stated over
// every octet of the reference rather than over the two ends of it.
//
// A key cut short is not a missed lookup, it is a SUBSTITUTION: two references sharing the
// retained prefix answer the same entry, so a commit naming one proposal resolves to another and
// every peer computing the same truncated key agrees. What separates a whole key from any
// truncation of it is a reference that differs from a stored one at exactly one position -- if
// that position is inside the part the key dropped, the lookup hits. Sweeping every position is
// what makes the statement independent of WHERE the cut is, which an assertion about the last
// byte alone is not: a key of ref[8:] is invisible to it.
func TestTheCacheKeyIsTheWholeReferenceAndNotAPrefixOfIt(t *testing.T) {
	crypto := testCrypto(t)
	cache := testCache(t)
	ref := testStoredRemove(t, crypto, cache, LeafIndex(1), LeafIndex(4))
	if len(ref) == 0 {
		t.Fatal("the stored reference is empty, so every neighbour below is the same reference")
	}
	for i := range ref {
		neighbour := bytes.Clone(ref)
		neighbour[i] ^= 0xFF
		if _, ok := cache.Cached(testResolveContext(), ProposalRef(neighbour)); ok {
			t.Errorf("the lookup answered an entry for a reference differing from the stored one at octet %d of %d; the lookup is not keyed on the whole reference",
				i, len(ref))
		}
		if _, err := cache.Resolve(crypto, testResolveContext(), LeafIndex(0),
			[]ProposalOrRef{{Type: ProposalOrRefTypeReference, Reference: ProposalRef(neighbour)}}); err == nil {
			t.Errorf("Resolve accepted a reference differing from the stored one at octet %d of %d",
				i, len(ref))
		}
	}
	// and a reference that is a genuine PREFIX of a stored one, which is the shape a caller
	// sends when it truncates on the wire rather than in the map
	for cut := 1; cut < len(ref); cut += 1 {
		if _, ok := cache.Cached(testResolveContext(), ProposalRef(ref[:cut])); ok {
			t.Errorf("the lookup answered an entry for the first %d octets of a %d octet reference", cut, len(ref))
		}
	}
}

func TestProposalCacheResolveUnknownReference(t *testing.T) {
	crypto := testCrypto(t)
	cache := testCache(t)
	unknown := ProposalRef(bytes.Repeat([]byte{9}, 32))
	_, err := cache.Resolve(crypto, testResolveContext(), LeafIndex(0),
		[]ProposalOrRef{{Type: ProposalOrRefTypeReference, Reference: unknown}})
	if !errors.Is(err, errProposalNotCached) {
		t.Fatalf("Resolve error = %v, want errProposalNotCached", err)
	}
}

// TestResolveRefusesOneReferenceNamedTwice is RFC 9420 section 12.2 at the layer that owns
// identity.
//
// A reference IS a name. One name resolving to two entries of one list means a member's single
// published proposal is applied twice -- two removals of one leaf, or two adds of one key package
// -- and every section 12.2 rule that would catch the result belongs to task 7, over a list this
// one has already built. Refusing it here is what keeps the list task 7 validates free of a
// duplicate that the cache itself manufactured.
func TestResolveRefusesOneReferenceNamedTwice(t *testing.T) {
	crypto := testCrypto(t)
	cache := testCache(t)
	ref := testStoredRemove(t, crypto, cache, LeafIndex(1), LeafIndex(4))
	_, err := cache.Resolve(crypto, testResolveContext(), LeafIndex(0), []ProposalOrRef{
		{Type: ProposalOrRefTypeReference, Reference: ref},
		{Type: ProposalOrRefTypeReference, Reference: ref},
	})
	if !errors.Is(err, errDuplicateProposalReference) {
		t.Fatalf("Resolve error = %v, want errDuplicateProposalReference", err)
	}
	// and the duplicate is refused wherever in the vector it appears, not only when the two are
	// adjacent: a scan comparing each entry with the one before it passes the interleaved shape
	other := testStoredRemove(t, crypto, cache, LeafIndex(2), LeafIndex(5))
	_, err = cache.Resolve(crypto, testResolveContext(), LeafIndex(0), []ProposalOrRef{
		{Type: ProposalOrRefTypeReference, Reference: ref},
		{Type: ProposalOrRefTypeReference, Reference: other},
		{Type: ProposalOrRefTypeReference, Reference: ref},
	})
	if !errors.Is(err, errDuplicateProposalReference) {
		t.Fatalf("Resolve error over an interleaved duplicate = %v, want errDuplicateProposalReference", err)
	}
}

// ---------------------------------------------------------------------------
// who a proposal is attributed to
// ---------------------------------------------------------------------------

func TestProposalCacheByValueSenderIsCommitter(t *testing.T) {
	crypto := testCrypto(t)
	cache := testCache(t)
	list, err := cache.Resolve(crypto, testResolveContext(), LeafIndex(5), []ProposalOrRef{{
		Type:     ProposalOrRefTypeProposal,
		Proposal: testRemoveProposal(LeafIndex(2)),
	}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(list.Removes()) != 1 || list.Removes()[0].Sender != 5 || !list.Removes()[0].ByValue {
		t.Fatalf("by-value proposal must be attributed to the committer: %+v", list.Removes())
	}
}

// TestTheCacheAcceptsExactlyOneSenderTypeOfTheWholeCodePointSpace is the attribution rule stated
// over all 256 values a sender type can hold rather than over the four the registry names.
//
// A CachedProposal carries a LeafIndex and a sender that is not a member has none, so caching a
// proposal from any other sender type attributes it to whatever LeafIndex the message happened to
// carry -- zero, for the Sender arms that have no leaf index at all. Leaf 0 is a real member, and
// an Update attributed to it reads as the committer's own under ValSem111.
//
// Every value rather than the three non member constants, because the class here is not the
// registry's: it is "not member", and an enumeration of the three named alternatives says nothing
// about the 252 a forged or GREASE sender carries.
func TestTheCacheAcceptsExactlyOneSenderTypeOfTheWholeCodePointSpace(t *testing.T) {
	crypto := testCrypto(t)
	accepted := []SenderType{}
	for code := 0; code < 256; code += 1 {
		senderType := SenderType(code)
		cache := testCache(t)
		content := testProposalContent(t, crypto, LeafIndex(1), testRemoveProposal(LeafIndex(4)))
		content.Content.Sender = Sender{SenderType: senderType, LeafIndex: 1}
		_, err := cache.Store(crypto, testResolveContext(), content)
		if err == nil {
			accepted = append(accepted, senderType)
			continue
		}
		if !errors.Is(err, errProposalSenderNotMember) {
			t.Errorf("Store with sender type %d answered %v, want errProposalSenderNotMember", code, err)
		}
	}
	if !slices.Equal(accepted, []SenderType{SenderTypeMember}) {
		t.Fatalf("the cache accepts the sender types %v, and a CachedProposal has a leaf index for exactly one of them", accepted)
	}
}

// ---------------------------------------------------------------------------
// the epoch an entry belongs to
// ---------------------------------------------------------------------------

// TestTheCacheIsBoundToTheGroupsOwnEpochAndNotToWhateverArrivedFirst is the replay property, and
// the direction it is stated in is the whole of the fix.
//
// A ProposalRef is a hash over an AuthenticatedContent that carries the group id and the epoch,
// so an entry from another epoch is not merely stale -- it is a name no commit of THIS epoch can
// legitimately carry. A cache that took entries from two epochs would answer the older one's
// references to the newer one's commit, which applies a proposal the group has already applied
// under a reference that still verifies.
//
// What the earlier reading could not say is WHICH epoch is this cache's. It took that from the
// first entry stored and then held every later entry to it, so the four refusals below all held --
// and all of them held against a binding a peer had chosen. Here the binding is the group context
// the cache was constructed with, and the FIRST store is judged by exactly the same rule as the
// fourth, which is what the first case below is for and what no ordering of the old rule could
// state.
func TestTheCacheIsBoundToTheGroupsOwnEpochAndNotToWhateverArrivedFirst(t *testing.T) {
	crypto := testCrypto(t)
	live := testResolveContextAt([]byte("group"), 7)
	cache := testCacheAt(t, live)
	if _, err := cache.Store(crypto, live, testProposalContentAt(t, 1, []byte("group"), 8,
		testRemoveProposal(LeafIndex(5)))); !errors.Is(err, errProposalCacheEpoch) {
		t.Errorf("the FIRST store into a cache bound to epoch 7, carrying epoch 8, answered %v, want errProposalCacheEpoch; a cache that took its epoch from whatever arrived first would have accepted this and bound itself to it",
			err)
	}
	if _, err := cache.Store(crypto, live, testProposalContentAt(t, 1, []byte("group"), 7,
		testRemoveProposal(LeafIndex(4)))); err != nil {
		t.Fatalf("Store at epoch 7: %v", err)
	}
	// the epoch after
	if _, err := cache.Store(crypto, live, testProposalContentAt(t, 1, []byte("group"), 8,
		testRemoveProposal(LeafIndex(5)))); !errors.Is(err, errProposalCacheEpoch) {
		t.Errorf("Store at epoch 8 into a cache holding epoch 7 answered %v, want errProposalCacheEpoch", err)
	}
	// and the epoch before, because a cache that only refused a HIGHER epoch would take a
	// replayed proposal from a closed one
	if _, err := cache.Store(crypto, live, testProposalContentAt(t, 1, []byte("group"), 6,
		testRemoveProposal(LeafIndex(5)))); !errors.Is(err, errProposalCacheEpoch) {
		t.Errorf("Store at epoch 6 into a cache holding epoch 7 answered %v, want errProposalCacheEpoch", err)
	}
	// and another GROUP at the same epoch, because the epoch number alone is not an identity:
	// every group in this client runs an epoch 7
	if _, err := cache.Store(crypto, live, testProposalContentAt(t, 1, []byte("other"), 7,
		testRemoveProposal(LeafIndex(5)))); !errors.Is(err, errProposalCacheEpoch) {
		t.Errorf("Store for another group at epoch 7 answered %v, want errProposalCacheEpoch", err)
	}
	if got := len(cache.Pending(live)); got != 1 {
		t.Errorf("the cache holds %d entries after four refusals, want 1", got)
	}
}

// TestCheckEpochAnswersTheBindingAndRebindMovesIt is the half Store cannot see: an epoch that
// advanced with no proposal arriving in the new one.
//
// THE EMPTY CACHE IS THE CASE THAT MATTERS, and it is the one the earlier reading could not see.
// While the binding came from the first entry stored, an empty cache answered nil for every epoch
// anybody named -- and an empty cache is precisely what a boundary leaves behind, so the one guard
// written to notice a boundary was blind to exactly the state a boundary produces. A cache bound
// at construction belongs to its epoch whether or not anything has been stored in it.
func TestCheckEpochAnswersTheBindingAndRebindMovesIt(t *testing.T) {
	crypto := testCrypto(t)
	at7 := testResolveContextAt([]byte("group"), 7)
	cache := testCacheAt(t, at7)
	if err := cache.CheckEpoch(at7); err != nil {
		t.Fatalf("CheckEpoch for the epoch this cache was built in = %v, want nil", err)
	}
	if err := cache.CheckEpoch(testResolveContextAt([]byte("group"), 8)); !errors.Is(err, errProposalCacheNotRebound) {
		t.Fatalf("CheckEpoch on an EMPTY cache, for the epoch after the one it was bound to, = %v, want errProposalCacheNotRebound; an empty cache belongs to the epoch it was bound to, and an emptied cache that answered for any epoch is the whole of what this guard could not see",
			err)
	}
	if _, err := cache.Store(crypto, at7, testProposalContentAt(t, 1, []byte("group"), 7,
		testRemoveProposal(LeafIndex(4)))); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if err := cache.CheckEpoch(at7); err != nil {
		t.Errorf("CheckEpoch for the epoch this cache holds = %v, want nil", err)
	}
	if err := cache.CheckEpoch(testResolveContextAt([]byte("group"), 8)); !errors.Is(err, errProposalCacheNotRebound) {
		t.Errorf("CheckEpoch for the next epoch = %v, want errProposalCacheNotRebound", err)
	}
	if err := cache.CheckEpoch(testResolveContextAt([]byte("other"), 7)); !errors.Is(err, errProposalCacheNotRebound) {
		t.Errorf("CheckEpoch for another group = %v, want errProposalCacheNotRebound", err)
	}
	at8 := testResolveContextAt([]byte("group"), 8)
	if err := cache.Rebind(testVerifiedContextAt(t, at8)); err != nil {
		t.Fatalf("Rebind to epoch 8: %v", err)
	}
	if err := cache.CheckEpoch(at8); err != nil {
		t.Errorf("CheckEpoch after Rebind = %v, want nil; Rebind is what carries the cache across a boundary", err)
	}
	if err := cache.CheckEpoch(at7); !errors.Is(err, errProposalCacheNotRebound) {
		t.Errorf("CheckEpoch for the epoch that just closed = %v, want errProposalCacheNotRebound; a rebind MOVES the binding and does not release it, and a released binding is the one a message could fill",
			err)
	}
	if got := len(cache.Pending(at8)); got != 0 {
		t.Errorf("Rebind left %d entries behind", got)
	}
	// and Store asks the same question of the same caller. A caller still acting in the epoch
	// that closed is refused as OUR fault and not as the message's, even when the message it
	// brings is a genuine proposal of that closed epoch: the cache and the caller disagreeing
	// about which epoch this is means nobody carried the cache over the boundary, and a caller
	// told the message was stale would go looking at the message.
	if _, err := cache.Store(crypto, at7, testProposalContentAt(t, 1, []byte("group"), 7,
		testRemoveProposal(LeafIndex(4)))); !errors.Is(err, errProposalCacheNotRebound) {
		t.Errorf("Store by a caller acting in the epoch that closed = %v, want errProposalCacheNotRebound", err)
	}
	if err := cache.Rebind(nil); !errors.Is(err, ErrNilGroupContext) {
		t.Errorf("Rebind(nil) = %v, want ErrNilGroupContext", err)
	}
	if err := cache.CheckEpoch(at8); err != nil {
		t.Errorf("the refused rebind moved the binding: %v", err)
	}
}

// TestTheCachedGroupIdIsCutFromTheCallersArrayAndNotAliasedToIt is the retention property for the
// one caller array the binding keeps.
//
// The group id arrives inside a buffer the caller decoded and still owns. A binding aliased to it
// changes when the caller reuses that buffer, so a cache that was holding group A silently starts
// answering for group B with no error path anywhere -- and CheckEpoch, the one guard that would
// have said so, agrees with whatever the buffer now holds.
//
// BOTH WRITE SITES, because two places that build a binding are two places the clone can be
// dropped, and the one that is dropped is whichever a later reader did not have in front of them.
func TestTheCachedGroupIdIsCutFromTheCallersArrayAndNotAliasedToIt(t *testing.T) {
	groupId := []byte("group")
	cache := testCacheAt(t, testResolveContextAt(groupId, 7))
	for i := range groupId {
		groupId[i] = 0x5A
	}
	if err := cache.CheckEpoch(testResolveContextAt([]byte("group"), 7)); err != nil {
		t.Errorf("CheckEpoch for the group this cache was built in = %v after the caller overwrote its own array, want nil", err)
	}
	if err := cache.CheckEpoch(testResolveContextAt(groupId, 7)); !errors.Is(err, errProposalCacheNotRebound) {
		t.Errorf("CheckEpoch for what the caller's array now holds = %v, want errProposalCacheNotRebound", err)
	}
	rebound := []byte("second")
	if err := cache.Rebind(testVerifiedContextAt(t, testResolveContextAt(rebound, 9))); err != nil {
		t.Fatalf("Rebind: %v", err)
	}
	for i := range rebound {
		rebound[i] = 0x5A
	}
	if err := cache.CheckEpoch(testResolveContextAt([]byte("second"), 9)); err != nil {
		t.Errorf("CheckEpoch after a Rebind = %v after the caller overwrote its own array, want nil", err)
	}
	if err := cache.CheckEpoch(testResolveContextAt(rebound, 9)); !errors.Is(err, errProposalCacheNotRebound) {
		t.Errorf("CheckEpoch for what the caller's array now holds = %v, want errProposalCacheNotRebound", err)
	}
}

// TestAReplayedProposalOfAClosedEpochNeitherBindsTheCacheNorLocksOutTheLiveEpoch is the defect,
// written as the sequence that produced it.
//
// The sequence: a member caches a genuine proposal in epoch N, the group commits and moves to N+1,
// and somebody replays that same genuine epoch N proposal at the freshly emptied cache. It is a
// message every peer has already seen and it verifies, so nothing upstream of this cache drops it.
//
// What used to happen is that the cache took its binding from that replay, because the binding came
// from the first entry stored and was checked against that entry's own epoch. The member was then
// bound to a closed epoch: it could cache nothing of the live one, resolve no commit that named a
// proposal, and never recover, because the only thing that released the binding ran off a commit it
// could no longer resolve. Integrity held the whole time -- the replayed proposal was never applied
// -- and the member was permanently unable to take part in its own group, at the cost of replaying
// one message.
//
// So the assertions after the replay are about AVAILABILITY and not about the refusal. The refusal
// is one line; the five under it are the group carrying on, and they are the half the earlier fix
// did not have.
func TestAReplayedProposalOfAClosedEpochNeitherBindsTheCacheNorLocksOutTheLiveEpoch(t *testing.T) {
	crypto := testCrypto(t)
	closed := testResolveContextAt([]byte("group"), 7)
	live := testResolveContextAt([]byte("group"), 8)

	cache := testCacheAt(t, closed)
	stale := testProposalContentAt(t, 1, []byte("group"), 7, testRemoveProposal(LeafIndex(4)))
	staleRef, err := cache.Store(crypto, closed, stale)
	if err != nil {
		t.Fatalf("Store in epoch 7: %v", err)
	}

	// the boundary
	if err := cache.Rebind(testVerifiedContextAt(t, live)); err != nil {
		t.Fatalf("Rebind to epoch 8: %v", err)
	}

	// the replay, delivered before anything of the live epoch has arrived, which is the one
	// moment at which the old cache had no opinion of its own to check it against
	if _, err := cache.Store(crypto, live, stale); !errors.Is(err, errProposalCacheEpoch) {
		t.Fatalf("the replayed epoch 7 proposal answered %v, want errProposalCacheEpoch", err)
	}

	fresh := testProposalContentAt(t, 2, []byte("group"), 8, testRemoveProposal(LeafIndex(5)))
	freshRef, err := cache.Store(crypto, live, fresh)
	if err != nil {
		t.Fatalf("a genuine epoch 8 proposal, stored after the replay, answered %v; the replay took the binding and this member can no longer take part in its own group", err)
	}
	if err := cache.CheckEpoch(live); err != nil {
		t.Errorf("after the replay the cache reports %v for its own live epoch", err)
	}
	if _, held := cache.Cached(live, freshRef); !held {
		t.Error("the live epoch's proposal is not answered by the lookup after the replay")
	}
	if got := len(cache.Pending(live)); got != 1 {
		t.Errorf("Pending answers %d entries of the live epoch after the replay, want 1; a committer reading this commits nothing", got)
	}
	list, err := cache.Resolve(crypto, live, LeafIndex(0),
		[]ProposalOrRef{{Type: ProposalOrRefTypeReference, Reference: freshRef}})
	if err != nil {
		t.Fatalf("a commit of the live epoch naming the live epoch's own proposal answered %v; this is the member that can no longer merge a commit, which is what made the lockout permanent", err)
	}
	if len(list.Removes()) != 1 || list.Removes()[0].Proposal.Remove.Removed != 5 {
		t.Errorf("the live commit resolved to %+v", list.Removes())
	}
	// and the replayed name is GONE rather than merely refused: the entry left with the epoch
	if _, err := cache.Resolve(crypto, live, LeafIndex(0),
		[]ProposalOrRef{{Type: ProposalOrRefTypeReference, Reference: staleRef}}); !errors.Is(err, errProposalNotCached) {
		t.Errorf("a commit of epoch 8 naming the epoch 7 reference answered %v, want errProposalNotCached", err)
	}
}

// TestCachedAndPendingAnswerNothingForAnEpochThisCacheIsNotBoundTo closes the two doors that read the
// map and consulted no binding at all.
//
// Neither is the door a replay comes through, and that is exactly why they are worth closing. p7's
// CheckErrata8815 calls this and reports "proposal reference %x is not cached for this epoch" on a
// miss, which is a claim about an epoch that a lookup consulting only the map cannot make; what
// saved it was the ORDER of two call sites, Resolve running first at both, and the order of two
// call sites is a property of those call sites rather than of this method. Pending is worse: what
// it answers goes straight into a commit, so a committer over a cache nobody rebound would name the
// closed epoch's references in the new epoch's commit.
func TestCachedAndPendingAnswerNothingForAnEpochThisCacheIsNotBoundTo(t *testing.T) {
	crypto := testCrypto(t)
	at7 := testResolveContextAt([]byte("group"), 7)
	cache := testCacheAt(t, at7)
	ref, err := cache.Store(crypto, at7, testProposalContentAt(t, 1, []byte("group"), 7,
		testRemoveProposal(LeafIndex(4))))
	if err != nil {
		t.Fatalf("Store at epoch 7: %v", err)
	}
	if _, held := cache.Cached(at7, ref); !held {
		t.Fatal("the lookup missed the reference Store answered in the epoch the cache is bound to, so every refusal below is that miss")
	}
	if got := len(cache.Pending(at7)); got != 1 {
		t.Fatalf("Pending answers %d entries in the epoch the cache is bound to, want 1", got)
	}
	for _, one := range []struct {
		what    string
		context *GroupContext
	}{
		{"the epoch after, which is what a member reads with after a boundary nobody rebound over",
			testResolveContextAt([]byte("group"), 8)},
		{"the epoch before", testResolveContextAt([]byte("group"), 6)},
		{"another group at the same epoch number", testResolveContextAt([]byte("other"), 7)},
	} {
		if cached, held := cache.Cached(one.context, ref); held {
			t.Errorf("the lookup in %s answered %+v, so \"not cached for this epoch\" is a sentence about a lookup that read no epoch",
				one.what, cached)
		}
		if got := cache.Pending(one.context); len(got) != 0 {
			t.Errorf("Pending in %s answered %d entries, which a committer of that epoch would name in its commit",
				one.what, len(got))
		}
	}
	// and no group context at all is not a way past either of them
	if _, held := cache.Cached(nil, ref); held {
		t.Error("a lookup with no group context answered an entry")
	}
	if got := cache.Pending(nil); len(got) != 0 {
		t.Errorf("Pending with no group context answered %d entries", len(got))
	}
}

// TestACacheBoundToNothingRefusesRatherThanBindingItselfLater is the fail-closed half, over the
// one unbound cache this package can still produce.
//
// A cache with no binding is the state in which "take the epoch from whatever arrives" is a
// tempting thing to write, and it is the state the old Clear left behind after every epoch
// boundary. The constructor no longer produces one. What still does is the ZERO VALUE, because Go's
// convention for a container leaves it usable and the nil provider gate drives Store on one -- so
// the zero value is what this is stated over.
//
// The empty group at epoch 0 is checked by name because it is not a hypothetical: epoch 0 is where
// every group starts, so a binding held as two zero valued fields with no way to say "bound to
// nothing" would answer for it.
func TestACacheBoundToNothingRefusesRatherThanBindingItselfLater(t *testing.T) {
	crypto := testCrypto(t)
	if _, err := NewProposalCache(nil); !errors.Is(err, ErrNilGroupContext) {
		t.Fatalf("NewProposalCache(nil) = %v, want ErrNilGroupContext; a constructor that answered an unbound cache hands the one state the epoch can come from a message in to every caller that did not read an error",
			err)
	}
	cache := &ProposalCache{}
	if err := cache.CheckEpoch(testResolveContext()); !errors.Is(err, errProposalCacheNotRebound) {
		t.Errorf("CheckEpoch on a zero valued cache = %v, want errProposalCacheNotRebound", err)
	}
	zero := &GroupContext{}
	if err := cache.CheckEpoch(zero); !errors.Is(err, errProposalCacheNotRebound) {
		t.Errorf("CheckEpoch for the empty group at epoch 0 = %v, want errProposalCacheNotRebound; a zero valued binding answers for the epoch every group starts in",
			err)
	}
	if _, err := cache.Store(crypto, zero, testProposalContentAt(t, 1, nil, 0,
		testRemoveProposal(LeafIndex(4)))); !errors.Is(err, errProposalCacheNotRebound) {
		t.Errorf("Store into a cache bound to nothing = %v, want errProposalCacheNotRebound", err)
	}
	if _, held := cache.Cached(zero, ProposalRef("anything")); held {
		t.Error("a cache bound to nothing answered a lookup")
	}
	if got := len(cache.Pending(zero)); got != 0 {
		t.Errorf("Pending on a cache bound to nothing answered %d entries", got)
	}
	// and it becomes usable the one way it can
	live := testResolveContext()
	if err := cache.Rebind(testVerifiedContextAt(t, live)); err != nil {
		t.Fatalf("Rebind: %v", err)
	}
	if _, err := cache.Store(crypto, live, testProposalContent(t, crypto, LeafIndex(1),
		testRemoveProposal(LeafIndex(4)))); err != nil {
		t.Errorf("Store after Rebind = %v", err)
	}
}

// ---------------------------------------------------------------------------
// order, buckets and the path requirement
// ---------------------------------------------------------------------------

func TestProposalCacheBucketsAndOrder(t *testing.T) {
	crypto := testCrypto(t)
	cache := testCache(t)
	refs := []ProposalOrRef{
		{Type: ProposalOrRefTypeProposal, Proposal: testRemoveProposal(LeafIndex(1))},
		{Type: ProposalOrRefTypeProposal, Proposal: testRemoveProposal(LeafIndex(2))},
	}
	list, err := cache.Resolve(crypto, testResolveContext(), LeafIndex(0), refs)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(list.All()) != 2 || list.Len() != 2 {
		t.Fatalf("All = %d, Len = %d, want 2", len(list.All()), list.Len())
	}
	if list.All()[0].Proposal.Remove.Removed != 1 || list.All()[1].Proposal.Remove.Removed != 2 {
		t.Fatal("All must preserve commit order, because Add placement depends on it")
	}
	if !list.PathRequired() {
		t.Fatal("a list containing a remove requires a path")
	}
}

// TestResolveKeepsCommitOrderInAllAndInEveryBucket is the order property over a vector that
// separates the three ways it can be lost.
//
// The commit order is 1, 2, 3 and the entries alternate between by-value and by-reference, so a
// resolution that reversed the vector, that appended the by-reference entries after the by-value
// ones, or that read the cache's own reception order instead of the commit's, each answers a
// different sequence. A two entry vector of one kind, which is what the plan supplies, is
// satisfied by all three.
func TestResolveKeepsCommitOrderInAllAndInEveryBucket(t *testing.T) {
	crypto := testCrypto(t)
	cache := testCache(t)
	// cached in the OPPOSITE order to the one the commit names them in, so a resolution reading
	// the cache's reception order answers 4, 2 rather than 2, 4
	fourth := testStoredRemove(t, crypto, cache, LeafIndex(9), LeafIndex(4))
	second := testStoredRemove(t, crypto, cache, LeafIndex(8), LeafIndex(2))
	list, err := cache.Resolve(crypto, testResolveContext(), LeafIndex(0), []ProposalOrRef{
		{Type: ProposalOrRefTypeProposal, Proposal: testRemoveProposal(LeafIndex(1))},
		{Type: ProposalOrRefTypeReference, Reference: second},
		{Type: ProposalOrRefTypeProposal, Proposal: testRemoveProposal(LeafIndex(3))},
		{Type: ProposalOrRefTypeReference, Reference: fourth},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := []LeafIndex{1, 2, 3, 4}
	got := []LeafIndex{}
	for _, cached := range list.All() {
		got = append(got, cached.Proposal.Remove.Removed)
	}
	if !slices.Equal(got, want) {
		t.Errorf("All removes leaves %v, want %v in the order the commit names them", got, want)
	}
	got = got[:0]
	for _, cached := range list.Removes() {
		got = append(got, cached.Proposal.Remove.Removed)
	}
	if !slices.Equal(got, want) {
		t.Errorf("Removes holds %v, want %v; the bucket carries the commit order too, because it is what an applier walks", got, want)
	}
	// and Refs rebuilds the same vector, by value where the commit carried a value and by
	// reference where it named one
	rebuilt := list.Refs()
	if len(rebuilt) != 4 {
		t.Fatalf("Refs rebuilt %d entries, want 4", len(rebuilt))
	}
	for i, entry := range rebuilt {
		byValue := i%2 == 0
		if byValue && (entry.Type != ProposalOrRefTypeProposal || entry.Proposal == nil) {
			t.Errorf("Refs entry %d is %+v, want an inline proposal", i, entry)
		}
		if !byValue && (entry.Type != ProposalOrRefTypeReference || len(entry.Reference) == 0) {
			t.Errorf("Refs entry %d is %+v, want a reference", i, entry)
		}
	}
}

func TestProposalListPathRequiredAddOnly(t *testing.T) {
	crypto := testCrypto(t)
	kp, _, _ := testKeyPackage(t, crypto, testIdentity(t, crypto, "bob"))
	cache := testCache(t)
	list, err := cache.Resolve(crypto, testResolveContext(), LeafIndex(0), []ProposalOrRef{{
		Type:     ProposalOrRefTypeProposal,
		Proposal: &Proposal{ProposalType: ProposalTypeAdd, Add: &Add{KeyPackage: *kp}},
	}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if list.PathRequired() {
		t.Fatal("an add-only list does not require a path")
	}
}

// TestAnEmptyProposalListRequiresAPath is the half of RFC 9420 section 12.4's rule that is easy
// to drop, and the half with the security answer: a commit with no proposals and no path changes
// no key material at all, so the epoch advances over a secret every member of the previous epoch
// still holds.
func TestAnEmptyProposalListRequiresAPath(t *testing.T) {
	crypto := testCrypto(t)
	cache := testCache(t)
	list, err := cache.Resolve(crypto, testResolveContext(), LeafIndex(0), nil)
	if err != nil {
		t.Fatalf("Resolve of an empty vector: %v", err)
	}
	if list.Len() != 0 {
		t.Fatalf("Len = %d over an empty vector", list.Len())
	}
	if !list.PathRequired() {
		t.Fatal("an empty proposal list requires a path; a commit with neither changes no key material")
	}
}

// proposalListBucketNames is every bucket field ProposalList declares, derived off the type
// rather than written down.
//
// All is the one exclusion and it is named rather than filtered by shape: it is the commit-order
// record and holds every proposal, so it is not a bucket and a gate that treated it as one would
// report every proposal as landing in two. A fifth bucket added to the struct is a member of this
// class the moment it is declared, which is what the sweep below needs it to be.
func proposalListBucketNames(t *testing.T) []string {
	t.Helper()
	names := []string{}
	for _, method := range proposalListViewMethods(t) {
		names = append(names, method.Name)
	}
	if len(names) == 0 {
		t.Fatal("ProposalList answers no per-type view of cached proposals, so the sweep below compares nothing")
	}
	slices.Sort(names)
	return names
}

// proposalListViewMethods is the derived class every gate over the per-type views runs on: every
// method of *ProposalList taking no argument and answering exactly []CachedProposal, less the one
// that answers the commit order.
//
// OFF THE METHOD SET AND NOT OFF THE FIELDS, because the views are no longer fields. A list stores
// its commit order and derives each view from it, so "every []CachedProposal field except All" now
// names one thing -- the order itself -- and a gate written that way would sweep nothing while
// reporting a clean bill. A fifth view added by a later task, a psk view when the profile widens,
// is in this class on the commit that declares it.
func proposalListViewMethods(t *testing.T) []reflect.Method {
	t.Helper()
	const commitOrder = "All"
	entries := reflect.TypeOf([]CachedProposal{})
	listType := reflect.TypeOf(&ProposalList{})
	found := []reflect.Method{}
	removed := []string{}
	for i := 0; i < listType.NumMethod(); i += 1 {
		method := listType.Method(i)
		signature := method.Type
		// THE READING IS OFF THE RESULTS, in whatever position they sit and whatever else sits
		// beside them. This used to open with signature.NumIn() != 1 || signature.NumOut() != 1,
		// and measured at 81b97ca the complement of those two clauses was EMPTY -- Refs, Len,
		// PathRequired and Extensions are all removed by the result TYPE test, which is the
		// property. An arity clause whose complement is empty removes nothing today and starts
		// removing real members the day one appears: a view answering ([]CachedProposal, error)
		// once a filter can fail, or a Filtered(ProposalType) []CachedProposal, would have left
		// this class in silence, and every rule stated over it would have gone on reporting a
		// clean bill over a class one member short.
		answersEntries := false
		for at := 0; at < signature.NumOut(); at += 1 {
			if signature.Out(at) == entries {
				answersEntries = true
			}
		}
		if !answersEntries {
			removed = append(removed, method.Name+" "+signature.String())
			continue
		}
		if method.Name == commitOrder {
			continue
		}
		found = append(found, method)
	}
	// and the exclusion is real rather than a name nobody declares
	if _, answers := listType.MethodByName(commitOrder); !answers {
		t.Fatalf("*ProposalList answers no %s, so the one exclusion this class makes excludes nothing",
			commitOrder)
	}
	if len(removed) == 0 {
		t.Fatal("every exported method of *ProposalList answers cached proposals, so the result reading removes nothing and this class is the method set under another name")
	}
	t.Logf("%d per-type views; %s is removed by name because it answers the commit order rather than a view of it, and the %d exported methods removed because no result of theirs is a []CachedProposal are %v",
		len(found), commitOrder, len(removed), removed)
	return found
}

// proposalListViewAnswer reads one view, driven with the zero value of each of its arguments.
//
// It is a DRIVER and not a filter, which is the distinction the arity clause above got wrong. A
// view that takes a filter is in the class the moment it is declared, and it arrives HERE rather
// than dropping out of the sweep: a member the zero row cannot get a view out of turns these
// rules red, which is a demand for a row to be written here and not a silent narrowing.
func proposalListViewAnswer(t *testing.T, list *ProposalList, view string) []CachedProposal {
	t.Helper()
	bound := reflect.ValueOf(list).MethodByName(view)
	if !bound.IsValid() {
		t.Fatalf("*ProposalList answers no %s", view)
	}
	arguments := []reflect.Value{}
	for at := 0; at < bound.Type().NumIn(); at += 1 {
		arguments = append(arguments, reflect.Zero(bound.Type().In(at)))
	}
	answered := [][]CachedProposal{}
	for _, result := range bound.Call(arguments) {
		if failure, isError := result.Interface().(error); isError && failure != nil {
			t.Fatalf("the %s view driven with the zero value of each argument: %v -- a view that needs a row rather than a zero is a row to write here",
				view, failure)
		}
		if entries, isView := result.Interface().([]CachedProposal); isView {
			answered = append(answered, entries)
		}
	}
	if len(answered) != 1 {
		t.Fatalf("the %s view answers %d slices of cached proposals and a view answers one", view, len(answered))
	}
	return answered[0]
}

// proposalListBucketLength reads one per-type view of a list by the name of the method that
// answers it.
func proposalListBucketLength(t *testing.T, list *ProposalList, bucket string) int {
	t.Helper()
	return len(proposalListViewAnswer(t, list, bucket))
}

// TestEveryProposalTypeTheV1ProfileAcceptsLandsInAViewOfItsOwn holds the profile table to the
// per-type views ProposalList answers, in both directions.
//
// Neither side is written down here. The accepted set is the rows of proposalTypeProfile whose
// refusal is nil -- the same table TestTheV1ProfileClassifiesEveryRegisteredProposalType holds
// equal to the registry -- and the views are the method set of *ProposalList. What the two
// directions catch are different faults. An accepted type with no view sits in the commit order,
// is read by no rule stated over a view and is applied by nothing, which is a commit the group
// agrees to and does not perform; and a view no accepted type fills is a filter that can never
// select anything, which is the shape a view added for a type that was then refused leaves
// behind. Two accepted types sharing one view is the third, and it is why the fill is recorded per
// view rather than counted.
func TestEveryProposalTypeTheV1ProfileAcceptsLandsInAViewOfItsOwn(t *testing.T) {
	crypto := testCrypto(t)
	member := testIdentity(t, crypto, "bob")
	registered := proposalTypeRegistry(t)
	buckets := proposalListBucketNames(t)
	accepted := []string{}
	filledBy := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(registered)) {
		proposalType := registered[name]
		refusal, classified := proposalTypeProfile[proposalType]
		if !classified || refusal != nil {
			continue
		}
		accepted = append(accepted, name)
		list, err := testCache(t).Resolve(crypto, testResolveContext(), LeafIndex(0), []ProposalOrRef{{
			Type:     ProposalOrRefTypeProposal,
			Proposal: proposalOfRegisteredType(t, crypto, member, proposalType),
		}})
		if err != nil {
			t.Errorf("Resolve of a %s the v1 profile accepts: %v", name, err)
			continue
		}
		if len(list.All()) != 1 {
			t.Errorf("a single %s resolved to %d entries in All", name, len(list.All()))
			continue
		}
		landed := []string{}
		for _, bucket := range buckets {
			if proposalListBucketLength(t, list, bucket) != 0 {
				landed = append(landed, bucket)
			}
		}
		if len(landed) != 1 {
			t.Errorf("a %s landed in the buckets %v, want exactly one of %v", name, landed, buckets)
			continue
		}
		if held, already := filledBy[landed[0]]; already {
			t.Errorf("%s and %s both land in %s, so one of the two is applied under the other's rules",
				held, name, landed[0])
			continue
		}
		filledBy[landed[0]] = name
	}
	if len(accepted) == 0 {
		t.Fatal("the v1 profile accepts no proposal type, so this sweep ran over nothing")
	}
	if !slices.Equal(slices.Sorted(maps.Keys(filledBy)), buckets) {
		t.Errorf("the accepted types %v fill the buckets %v and ProposalList declares %v; an accepted type in no bucket is counted in All and applied by nothing, and a bucket nothing fills is a route Resolve never takes",
			accepted, slices.Sorted(maps.Keys(filledBy)), buckets)
	}
	t.Logf("%d accepted proposal types over %d buckets: %v", len(accepted), len(buckets), filledBy)
}

// TestTheListPathRequirementFollowsTheRfcSetForEveryAcceptedType is section 12.4's rule read off
// a resolved list rather than off the predicate, over every type the profile lets through.
//
// The predicate has its own gate next door. What this adds is that PathRequired READS it: a list
// method that answered true for everything, or that consulted a set of its own, agrees with the
// predicate's gate and disagrees with the RFC on exactly the add-only commit.
func TestTheListPathRequirementFollowsTheRfcSetForEveryAcceptedType(t *testing.T) {
	crypto := testCrypto(t)
	member := testIdentity(t, crypto, "bob")
	registered := proposalTypeRegistry(t)
	swept := 0
	for _, name := range slices.Sorted(maps.Keys(registered)) {
		proposalType := registered[name]
		if refusal, classified := proposalTypeProfile[proposalType]; !classified || refusal != nil {
			continue
		}
		list, err := testCache(t).Resolve(crypto, testResolveContext(), LeafIndex(0), []ProposalOrRef{{
			Type:     ProposalOrRefTypeProposal,
			Proposal: proposalOfRegisteredType(t, crypto, member, proposalType),
		}})
		if err != nil {
			t.Errorf("Resolve of a %s: %v", name, err)
			continue
		}
		if got, want := list.PathRequired(), proposalTypePathRequired(proposalType); got != want {
			t.Errorf("a list holding one %s reports PathRequired %v, want %v", name, got, want)
		}
		swept += 1
	}
	if swept == 0 {
		t.Fatal("no accepted proposal type was swept, so this gate states nothing")
	}
}

// ---------------------------------------------------------------------------
// the group context extensions arm
// ---------------------------------------------------------------------------

// TestResolveRefusesASecondGroupContextExtensionsProposal is what makes ProposalList.Extensions
// exact.
//
// RFC 9420 section 12.2 makes a list carrying two of them invalid, and task 7's catalogue
// (ValSem101 to ValSem113) has no code for it -- checked against the plan rather than assumed.
// Extensions answers GCE[0], so without this refusal a commit carrying two would apply one of two
// extension sets with nothing anywhere saying which, and the group would agree to a context no
// member can name.
func TestResolveRefusesASecondGroupContextExtensionsProposal(t *testing.T) {
	crypto := testCrypto(t)
	first := []Extension{{ExtensionType: ExtensionTypeRequiredCapabilities, ExtensionData: []byte{0, 0, 0}}}
	second := []Extension{{ExtensionType: ExtensionTypeUrmessageGroupPolicy, ExtensionData: []byte{1}}}
	_, err := testCache(t).Resolve(crypto, testResolveContext(), LeafIndex(0), []ProposalOrRef{
		{Type: ProposalOrRefTypeProposal, Proposal: &Proposal{
			ProposalType:           ProposalTypeGroupContextExtensions,
			GroupContextExtensions: &GroupContextExtensions{Extensions: first}}},
		{Type: ProposalOrRefTypeProposal, Proposal: &Proposal{
			ProposalType:           ProposalTypeGroupContextExtensions,
			GroupContextExtensions: &GroupContextExtensions{Extensions: second}}},
	})
	if !errors.Is(err, errMultipleGroupContextExtensions) {
		t.Fatalf("Resolve error = %v, want errMultipleGroupContextExtensions", err)
	}
}

// TestExtensionsAnswersTheProposedSetAndNothingWhenThereIsNone
func TestExtensionsAnswersTheProposedSetAndNothingWhenThereIsNone(t *testing.T) {
	crypto := testCrypto(t)
	cache := testCache(t)
	empty, err := cache.Resolve(crypto, testResolveContext(), LeafIndex(0), []ProposalOrRef{{
		Type: ProposalOrRefTypeProposal, Proposal: testRemoveProposal(LeafIndex(1))}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if exts, ok := empty.Extensions(); ok || exts != nil {
		t.Errorf("a list with no group_context_extensions proposal answered %v %v", exts, ok)
	}
	proposed := []Extension{{ExtensionType: ExtensionTypeRequiredCapabilities, ExtensionData: []byte{0, 0, 0}}}
	list, err := cache.Resolve(crypto, testResolveContext(), LeafIndex(0), []ProposalOrRef{{
		Type: ProposalOrRefTypeProposal, Proposal: &Proposal{
			ProposalType:           ProposalTypeGroupContextExtensions,
			GroupContextExtensions: &GroupContextExtensions{Extensions: proposed}}}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	exts, ok := list.Extensions()
	if !ok || len(exts) != 1 || exts[0].ExtensionType != ExtensionTypeRequiredCapabilities {
		t.Fatalf("Extensions = %v %v, want the proposed set", exts, ok)
	}
}

// ---------------------------------------------------------------------------
// what the cache keeps, and what it does not
// ---------------------------------------------------------------------------

// TestTheCacheKeepsNothingTheCallerCanStillWriteInto is the retention property over a proposal's
// own body.
//
// A proposal arrives inside a buffer the caller decoded and still owns. An entry cut from that
// buffer changes underneath the group with no error path anywhere, and the commit resolved from
// it is one no peer can reproduce -- the reference was taken over the octets that arrived, and
// what the cache would then answer is whatever the caller wrote afterwards.
func TestTheCacheKeepsNothingTheCallerCanStillWriteInto(t *testing.T) {
	crypto := testCrypto(t)
	cache := testCache(t)
	body := []byte{0x11, 0x22, 0x33}
	proposal := &Proposal{
		ProposalType: ProposalTypeGroupContextExtensions,
		GroupContextExtensions: &GroupContextExtensions{Extensions: []Extension{
			{ExtensionType: ExtensionTypeRequiredCapabilities, ExtensionData: body}}},
	}
	ref, err := cache.Store(crypto, testResolveContext(),
		testProposalContent(t, crypto, LeafIndex(1), proposal))
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	// the caller goes on owning the arrays it was decoded out of, and reuses them
	for i := range body {
		body[i] = 0xFF
	}
	proposal.GroupContextExtensions.Extensions[0].ExtensionType = ExtensionTypeUrmessageGroupPolicy
	cached, ok := cache.Cached(testResolveContext(), ref)
	if !ok {
		t.Fatal("the lookup missed the reference Store answered")
	}
	held := cached.Proposal.GroupContextExtensions.Extensions[0]
	if held.ExtensionType != ExtensionTypeRequiredCapabilities {
		t.Errorf("the cached extension type is %#04x after the caller edited its own proposal, want %#04x",
			uint16(held.ExtensionType), uint16(ExtensionTypeRequiredCapabilities))
	}
	if !slices.Equal(held.ExtensionData, []byte{0x11, 0x22, 0x33}) {
		t.Errorf("the cached extension body is %x after the caller overwrote its own array, want 112233", held.ExtensionData)
	}
}

// TestResolveHandsBackNothingTheCacheStillHolds is the same property in the other direction: what
// Resolve answers must not be the cache's own storage, or an applier walking the list writes into
// the entries every later commit of this epoch resolves.
func TestResolveHandsBackNothingTheCacheStillHolds(t *testing.T) {
	crypto := testCrypto(t)
	cache := testCache(t)
	ref := testStoredRemove(t, crypto, cache, LeafIndex(1), LeafIndex(4))
	list, err := cache.Resolve(crypto, testResolveContext(), LeafIndex(0),
		[]ProposalOrRef{{Type: ProposalOrRefTypeReference, Reference: ref}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	list.All()[0].Proposal.Remove.Removed = 99
	list.All()[0].Ref[0] ^= 0xFF
	cached, ok := cache.Cached(testResolveContext(), ref)
	if !ok {
		t.Fatal("the caller's edit to the resolved reference reached the key this cache is holding")
	}
	if cached.Proposal.Remove.Removed != 4 {
		t.Errorf("the cached proposal removes leaf %d after the caller edited the resolved list, want 4",
			cached.Proposal.Remove.Removed)
	}
}

// TestStoreHandsBackNothingTheCacheStillHolds is the third direction: the reference Store answers
// is the caller's, and writing into it must change nothing the cache is holding.
//
// Both halves, because the cache holds the reference TWICE -- once as the map key and once as the
// entry's own Ref, which is what Get answers and what a committer names the proposal by. A key
// cut from a string conversion is safe whatever the caller does to the answer, so a probe that
// only looked the entry up would report clean over an entry whose own Ref the caller had just
// rewritten -- and that Ref is the one that ends up in a commit.
func TestStoreHandsBackNothingTheCacheStillHolds(t *testing.T) {
	crypto := testCrypto(t)
	cache := testCache(t)
	ref := testStoredRemove(t, crypto, cache, LeafIndex(1), LeafIndex(4))
	name := bytes.Clone(ref)
	ref[0] ^= 0xFF
	cached, ok := cache.Cached(testResolveContext(), ProposalRef(name))
	if !ok {
		t.Fatal("writing into the reference Store answered renamed the entry the cache is holding")
	}
	if !slices.Equal([]byte(cached.Ref), name) {
		t.Errorf("the entry names itself %x after the caller edited the reference Store answered, want %x",
			cached.Ref, name)
	}
}

// ---------------------------------------------------------------------------
// what the cache refuses
// ---------------------------------------------------------------------------

// TestStoreJudgesItsProviderBeforeAnythingElse records the order the refusals run in, which is
// the difference between a caller told to fix the argument it got wrong and one sent to fix a
// message that was never the problem.
func TestStoreJudgesItsProviderBeforeAnythingElse(t *testing.T) {
	if _, err := testCache(t).Store(nil, nil, nil); !errors.Is(err, ErrNilCryptoProvider) {
		t.Errorf("Store(nil, nil, nil) = %v, want ErrNilCryptoProvider", err)
	}
	crypto := testCrypto(t)
	// the group context before the message, because it is the other thing the caller was
	// asked for and it decides which epoch every rule after it runs in
	if _, err := testCache(t).Store(crypto, nil, nil); !errors.Is(err, ErrNilGroupContext) {
		t.Errorf("Store(crypto, nil, nil) = %v, want ErrNilGroupContext", err)
	}
	if _, err := testCache(t).Store(crypto, testResolveContext(), nil); !errors.Is(err, errNilAuthenticatedContent) {
		t.Errorf("Store(crypto, groupContext, nil) = %v, want errNilAuthenticatedContent", err)
	}
	if _, err := testCache(t).Resolve(nil, nil, 0, nil); !errors.Is(err, ErrNilCryptoProvider) {
		t.Errorf("Resolve(nil, ...) = %v, want ErrNilCryptoProvider", err)
	}
}

// TestStoreRefusesAnythingThatIsNotAFramedProposal covers the two shapes a caller reaches this
// with: a content type naming another arm, and a proposal content type carrying no proposal.
func TestStoreRefusesAnythingThatIsNotAFramedProposal(t *testing.T) {
	crypto := testCrypto(t)
	commit := testProposalContent(t, crypto, LeafIndex(1), testRemoveProposal(LeafIndex(4)))
	commit.Content.ContentType = ContentTypeCommit
	if _, err := testCache(t).Store(crypto, testResolveContext(), commit); !errors.Is(err, errProposalCacheNotAProposal) {
		t.Errorf("Store of a commit = %v, want errProposalCacheNotAProposal", err)
	}
	empty := testProposalContent(t, crypto, LeafIndex(1), nil)
	if _, err := testCache(t).Store(crypto, testResolveContext(), empty); !errors.Is(err, errProposalCacheNotAProposal) {
		t.Errorf("Store of a proposal content with no proposal = %v, want errProposalCacheNotAProposal", err)
	}
}

// TestTheCacheRunsTheV1ProfileGateOnBothDoors is the parse boundary this plan gives the profile:
// the three registered types v1 does not implement, and every unregistered one, stop at the cache
// rather than at the codec.
func TestTheCacheRunsTheV1ProfileGateOnBothDoors(t *testing.T) {
	crypto := testCrypto(t)
	psk := &Proposal{ProposalType: ProposalTypePreSharedKey, PreSharedKey: &PreSharedKey{}}
	if _, err := testCache(t).Store(crypto, testResolveContext(),
		testProposalContent(t, crypto, LeafIndex(1), psk)); !errors.Is(err, errProfilePsk) {
		t.Errorf("Store of a psk proposal = %v, want errProfilePsk", err)
	}
	if _, err := testCache(t).Resolve(crypto, testResolveContext(), LeafIndex(0), []ProposalOrRef{{
		Type: ProposalOrRefTypeProposal, Proposal: psk}}); !errors.Is(err, errProfilePsk) {
		t.Errorf("Resolve of an inline psk proposal = %v, want errProfilePsk", err)
	}
	grease := &Proposal{ProposalType: ProposalType(0x0A0A), UnknownBody: []byte{1, 2}}
	if _, err := testCache(t).Resolve(crypto, testResolveContext(), LeafIndex(0), []ProposalOrRef{{
		Type: ProposalOrRefTypeProposal, Proposal: grease}}); !errors.Is(err, errUnregisteredProposalType) {
		t.Errorf("Resolve of an inline GREASE proposal = %v, want errUnregisteredProposalType", err)
	}
}

// TestResolveRefusesEveryProposalOrRefShapeTheCodecRefuses is what says the cache reaches
// ProposalOrRef.checkArm rather than restating a subset of it. The empty reference is the one
// that matters most here: it is wire legal, it is the encoding of "no reference", and a cache
// that looked it up would key every commit that made the same mistake to one entry.
func TestResolveRefusesEveryProposalOrRefShapeTheCodecRefuses(t *testing.T) {
	crypto := testCrypto(t)
	cache := testCache(t)
	ref := testStoredRemove(t, crypto, cache, LeafIndex(1), LeafIndex(4))
	for name, entry := range map[string]ProposalOrRef{
		"an unregistered discriminant": {Type: ProposalOrRefType(7), Reference: ref},
		"the reserved discriminant":    {Type: ProposalOrRefTypeReserved, Reference: ref},
		"both arms populated":          {Type: ProposalOrRefTypeReference, Reference: ref, Proposal: testRemoveProposal(1)},
		"no arm at all":                {Type: ProposalOrRefTypeProposal},
		"a reference of no octets":     {Type: ProposalOrRefTypeReference, Reference: ProposalRef{}},
	} {
		if _, err := cache.Resolve(crypto, testResolveContext(), LeafIndex(0), []ProposalOrRef{entry}); err == nil {
			t.Errorf("Resolve accepted %s", name)
		}
	}
}

// ---------------------------------------------------------------------------
// what the cache offers a committer
// ---------------------------------------------------------------------------

// TestPendingAnswersEveryEntryOnceInReceptionOrder holds the SHOULD of section 12.4 and the
// order Add placement depends on. Storing one proposal twice is the idempotence half: a reference
// is an identity, so the second store is the same entry rather than a second one.
func TestPendingAnswersEveryEntryOnceInReceptionOrder(t *testing.T) {
	crypto := testCrypto(t)
	cache := testCache(t)
	first := testStoredRemove(t, crypto, cache, LeafIndex(1), LeafIndex(4))
	second := testStoredRemove(t, crypto, cache, LeafIndex(2), LeafIndex(5))
	again := testStoredRemove(t, crypto, cache, LeafIndex(1), LeafIndex(4))
	if !slices.Equal([]byte(first), []byte(again)) {
		t.Fatal("one proposal stored twice answered two references")
	}
	pending := cache.Pending(testResolveContext())
	if len(pending) != 2 {
		t.Fatalf("Pending answers %d entries after three stores of two proposals, want 2", len(pending))
	}
	for i, want := range []ProposalRef{first, second} {
		if pending[i].Type != ProposalOrRefTypeReference {
			t.Errorf("Pending entry %d is type %d, want a reference", i, pending[i].Type)
		}
		if !slices.Equal([]byte(pending[i].Reference), []byte(want)) {
			t.Errorf("Pending entry %d names %x, want %x", i, pending[i].Reference, want)
		}
	}
	// and what Pending answers resolves, which is the whole of what a committer does with it
	list, err := cache.Resolve(crypto, testResolveContext(), LeafIndex(0), pending)
	if err != nil {
		t.Fatalf("Resolve of the pending vector: %v", err)
	}
	if len(list.Removes()) != 2 || list.Removes()[0].Proposal.Remove.Removed != 4 ||
		list.Removes()[1].Proposal.Remove.Removed != 5 {
		t.Errorf("the pending vector resolved to %+v", list.Removes())
	}
}

// ---------------------------------------------------------------------------
// the epoch a RESOLUTION runs in
// ---------------------------------------------------------------------------

// TestResolveRefusesAReferenceCachedInAnEpochThatHasClosed is the replay property at the door the
// pinned signature left open.
//
// A ProposalRef is a hash over an AuthenticatedContent carrying the group id and the epoch, so an
// entry cached in epoch N is a name no commit of epoch N+1 can legitimately carry. A cache nobody
// cleared at the boundary still answers every one of those names, and the commit that names one
// applies a proposal the group has already applied under a reference every peer verifies and
// agrees with. Resolve read neither the epoch nor the group id -- it was handed neither -- so this
// resolved, unconditionally, in every direction.
//
// Every direction is what is asserted. A guard that refused only a HIGHER epoch takes a replay
// out of a closed one; a guard that compared only the epoch number takes another group's entry,
// because every group this client is in runs an epoch 7.
func TestResolveRefusesAReferenceCachedInAnEpochThatHasClosed(t *testing.T) {
	crypto := testCrypto(t)
	at7 := testResolveContextAt([]byte("group"), 7)
	cache := testCacheAt(t, at7)
	ref, err := cache.Store(crypto, at7, testProposalContentAt(t, 1, []byte("group"), 7,
		testRemoveProposal(LeafIndex(4))))
	if err != nil {
		t.Fatalf("Store at epoch 7: %v", err)
	}
	named := []ProposalOrRef{{Type: ProposalOrRefTypeReference, Reference: ref}}

	// the epoch the entry belongs to resolves, so every refusal below is the epoch and not a
	// reference this cache never held
	list, err := cache.Resolve(crypto, testResolveContextAt([]byte("group"), 7), LeafIndex(0), named)
	if err != nil {
		t.Fatalf("Resolve in the epoch the entry was cached in: %v", err)
	}
	if len(list.Removes()) != 1 || list.Removes()[0].Proposal.Remove.Removed != 4 {
		t.Fatalf("the entry resolved to %+v in its own epoch", list.Removes())
	}

	for _, one := range []struct {
		what    string
		context *GroupContext
	}{
		{"the epoch after, which is the replay a commit of the new epoch performs",
			testResolveContextAt([]byte("group"), 8)},
		{"the epoch before, which is a commit of a closed epoch naming a proposal of the live one",
			testResolveContextAt([]byte("group"), 6)},
		{"another group at the same epoch number, which is not the same epoch at all",
			testResolveContextAt([]byte("other"), 7)},
	} {
		refused, err := cache.Resolve(crypto, one.context, LeafIndex(0), named)
		if !errors.Is(err, errProposalResolvedOutOfEpoch) {
			t.Errorf("resolving into %s answered %v, want errProposalResolvedOutOfEpoch", one.what, err)
		}
		if refused != nil {
			t.Errorf("resolving into %s answered a list as well as an error, and a caller that reads the list applies the replayed proposal", one.what)
		}
		// two rules, two values. Store refuses an ENTRY that arrived carrying another
		// epoch and the remedy is to drop that message; this refuses a COMMIT of another
		// epoch and the remedy is that the lifecycle did not clear the cache. A caller
		// that could not tell them apart would be told to look at the wrong thing.
		if errors.Is(err, errProposalCacheEpoch) {
			t.Errorf("resolving into %s answered Store's errProposalCacheEpoch as well, so the two rules of this file share one value", one.what)
		}
		if errors.Is(err, errProposalNotCached) {
			t.Errorf("resolving into %s answered errProposalNotCached, which is untrue -- the reference IS cached, in an epoch that has closed, and that is the whole of the fault", one.what)
		}
	}
}

// TestAResolutionThatNamesNothingCachedIsNotJudgedByTheBindingOfACacheItNeverReads is the other
// half of rule 4: what it does NOT refuse.
//
// A commit carrying every proposal by value reads no cache entry, so no entry of a closed epoch
// can reach the list it produces and there is nothing to replay. Refusing it would be a rule
// about the caller's housekeeping wearing the name of a replay refusal, and the first thing
// anybody would do about it is clear the cache in the one path that showed the error rather than
// in the path that advanced the epoch.
func TestAResolutionThatNamesNothingCachedIsNotJudgedByTheBindingOfACacheItNeverReads(t *testing.T) {
	crypto := testCrypto(t)
	at7 := testResolveContextAt([]byte("group"), 7)
	cache := testCacheAt(t, at7)
	if _, err := cache.Store(crypto, at7, testProposalContentAt(t, 1, []byte("group"), 7,
		testRemoveProposal(LeafIndex(4)))); err != nil {
		t.Fatalf("Store at epoch 7: %v", err)
	}
	inline := []ProposalOrRef{{Type: ProposalOrRefTypeProposal, Proposal: testRemoveProposal(LeafIndex(9))}}
	list, err := cache.Resolve(crypto, testResolveContextAt([]byte("group"), 8), LeafIndex(3), inline)
	if err != nil {
		t.Fatalf("an inline only commit of epoch 8, over a cache still holding epoch 7, answered %v; it reads no entry of that cache", err)
	}
	if len(list.Removes()) != 1 || list.Removes()[0].Proposal.Remove.Removed != 9 || list.Removes()[0].Sender != 3 {
		t.Fatalf("the inline proposal resolved to %+v", list.Removes())
	}
}

// TestAnEmptyCacheStillBelongsToTheEpochItWasBoundToAndNamesTheRightRefusal is the boundary
// between the two refusals a lookup can earn.
//
// A cache emptied by a rebind holds no entry, so a reference into it FROM THE EPOCH IT IS NOW BOUND
// TO is not a replay -- it is a name nothing was ever stored under, and errProposalNotCached is the
// true account. A guard that reported a replay there would say so to every member that received a
// commit before the proposal it names.
//
// What the emptiness short circuit ALSO did was answer nil for every other epoch, so an emptied
// cache reported itself to be in whatever epoch it was next asked about. That is the same "the
// first thing that arrives decides" the binding itself used to have, one door along, and it is why
// there is no emptiness clause in front of the comparison any more. An empty cache belongs to the
// epoch it was bound to, and a resolution in another one is this build failing to rebind.
func TestAnEmptyCacheStillBelongsToTheEpochItWasBoundToAndNamesTheRightRefusal(t *testing.T) {
	crypto := testCrypto(t)
	cache := testCache(t)
	ref := testStoredRemove(t, crypto, cache, LeafIndex(1), LeafIndex(4))
	at2 := testResolveContextAt([]byte("group"), 2)
	if err := cache.Rebind(testVerifiedContextAt(t, at2)); err != nil {
		t.Fatalf("Rebind: %v", err)
	}
	named := []ProposalOrRef{{Type: ProposalOrRefTypeReference, Reference: ref}}
	_, err := cache.Resolve(crypto, at2, LeafIndex(0), named)
	if !errors.Is(err, errProposalNotCached) {
		t.Errorf("a reference into a rebound cache, resolving in the epoch it was rebound to, answered %v, want errProposalNotCached", err)
	}
	if errors.Is(err, errProposalResolvedOutOfEpoch) {
		t.Errorf("a rebound cache reported itself out of the epoch it had just been rebound to")
	}
	for _, at := range []*GroupContext{
		testResolveContextAt([]byte("group"), 1),
		testResolveContextAt([]byte("group"), 99),
		testResolveContextAt([]byte("other"), 2),
	} {
		_, err := cache.Resolve(crypto, at, LeafIndex(0), named)
		if !errors.Is(err, errProposalResolvedOutOfEpoch) {
			t.Errorf("a reference into an EMPTY cache bound to epoch 2, resolving in epoch %d of group %x, answered %v, want errProposalResolvedOutOfEpoch; an empty cache that answers for any epoch is a binding taken from whoever asks",
				at.Epoch, at.GroupId, err)
		}
	}
	// and an inline commit resolves against an empty cache in any epoch, which is what keeps
	// the rule about the references a commit reads rather than about anybody's housekeeping
	if _, err := cache.Resolve(crypto, testResolveContextAt([]byte("group"), 99), LeafIndex(0),
		[]ProposalOrRef{{Type: ProposalOrRefTypeProposal, Proposal: testRemoveProposal(LeafIndex(2))}}); err != nil {
		t.Errorf("an inline commit against an empty cache answered %v", err)
	}
}

// TestResolveRefusesANilGroupContextRatherThanDereferencingIt is the argument rule over the
// parameter this task added.
//
// nil_argument_test.go derives the class of nil refusals off the source and carries the row that
// sweeps this one; what is here is the ORDER. The provider is judged first, because a caller that
// passed neither should be sent to the one it is asked for first rather than to whichever the
// body happened to read.
func TestResolveRefusesANilGroupContextRatherThanDereferencingIt(t *testing.T) {
	crypto := testCrypto(t)
	if _, err := testCache(t).Resolve(crypto, nil, LeafIndex(0), nil); !errors.Is(err, ErrNilGroupContext) {
		t.Errorf("Resolve with no group context = %v, want ErrNilGroupContext", err)
	}
	if _, err := testCache(t).Resolve(nil, nil, LeafIndex(0), nil); !errors.Is(err, ErrNilCryptoProvider) {
		t.Errorf("Resolve with neither a provider nor a group context = %v, want ErrNilCryptoProvider; the provider is the first thing this body asks for", err)
	}
}

// ---------------------------------------------------------------------------
// the two survivors this file carried
// ---------------------------------------------------------------------------

// TestExtensionsAnswersTheFirstOfTwoInAHandAssembledList holds the index Extensions reads.
//
// Every list Resolve produces carries at most one GroupContextExtensions proposal, because Resolve
// refuses the second -- so over those lists GCE[0] and GCE[len-1] are the same entry and no test
// that goes through Resolve can tell them apart. Measured: the whole of ./mls/... and
// ./message/... was green with the LAST one answered.
//
// A hand assembled list is not a hypothetical. p7 task 7 builds ProposalList values field by
// field and reads this through (*ProposalValidationInput).effectiveExtensions, so a list carrying
// two is a value that reaches this accessor, and which of the two it answers decides the
// extension set every leaf of the group is then validated against.
func TestExtensionsAnswersTheFirstOfTwoInAHandAssembledList(t *testing.T) {
	first := []Extension{{ExtensionType: ExtensionTypeRequiredCapabilities, ExtensionData: []byte{0, 0, 0}}}
	second := []Extension{{ExtensionType: ExtensionTypeUrmessageGroupPolicy, ExtensionData: []byte{0x02}}}
	entry := func(extensions []Extension) CachedProposal {
		return CachedProposal{Proposal: Proposal{
			ProposalType:           ProposalTypeGroupContextExtensions,
			GroupContextExtensions: &GroupContextExtensions{Extensions: extensions},
		}}
	}
	list := NewProposalList([]CachedProposal{entry(first), entry(second)})
	answered, ok := list.Extensions()
	if !ok || len(answered) != 1 {
		t.Fatalf("Extensions over a list carrying two group_context_extensions answered %v %v", answered, ok)
	}
	if answered[0].ExtensionType != first[0].ExtensionType {
		t.Errorf("Extensions answered extension type %#04x, want the FIRST entry's %#04x; the index is exact and not whichever end of the slice reads the same over the one entry lists Resolve produces",
			uint16(answered[0].ExtensionType), uint16(first[0].ExtensionType))
	}
}

// TestABucketlessAcceptedTypeIsRefusedRatherThanSilentlyDropped executes Resolve's default bucket
// branch, which is unreachable over the profile this build runs.
//
// The branch used to carry a comment calling itself reachable. It is not: every value that
// reaches the switch has been through checkProposalProfile, which refuses every type
// proposalTypeProfile does not classify as accepted, and the four cases are exactly the four it
// does. So the refusal was defensive code no test executed, and turning it into a silent drop --
// which leaves the proposal counted in All and applied by nothing -- changed no test's answer.
//
// What makes it reachable is the commit that widens the accepted set, and that is what this test
// performs: one row added to the profile table for the length of this test, which is the smallest
// faithful model of that commit. The row is removed by the cleanup whether this passes or fails,
// because every other test in this file derives its class off the same table.
func TestABucketlessAcceptedTypeIsRefusedRatherThanSilentlyDropped(t *testing.T) {
	crypto := testCrypto(t)
	const widened = ProposalType(0x0B0B)
	if _, already := proposalTypeProfile[widened]; already {
		t.Fatalf("%#04x is already classified, so this test is not modelling a widening at all", uint16(widened))
	}
	proposalTypeProfile[widened] = nil
	t.Cleanup(func() { delete(proposalTypeProfile, widened) })

	accepted := &Proposal{ProposalType: widened, UnknownBody: []byte{0xde, 0xad}}
	if err := checkProposalProfile(defaultProfile(), accepted); err != nil {
		t.Fatalf("the widened profile refused %#04x with %v, so this never reaches the bucket switch and the branch is still unobserved",
			uint16(widened), err)
	}
	list, err := testCache(t).Resolve(crypto, testResolveContext(), LeafIndex(0),
		[]ProposalOrRef{{Type: ProposalOrRefTypeProposal, Proposal: accepted}})
	if !errors.Is(err, errAcceptedTypeHasNoBucket) {
		t.Fatalf("an accepted type with no bucket resolved with err = %v, want errAcceptedTypeHasNoBucket; a proposal counted in All and put in no bucket is one the group agreed to and no member applies",
			err)
	}
	// and it is not the refusal an unsupported type gets. The two are opposite: this type IS
	// supported -- the profile was just widened to accept it -- and what is missing is a bucket
	// on THIS side. A caller told the type was unsupported would go and look at the peer.
	if errors.Is(err, errUnregisteredProposalType) || errors.Is(err, errReservedProposalType) {
		t.Errorf("the bucketless refusal answers to a type refusal as well: %v", err)
	}
	if list != nil {
		t.Errorf("the refusal answered a list as well: %+v", list)
	}
}

// ---------------------------------------------------------------------------
// one value per rule, over the class this file declares
// ---------------------------------------------------------------------------

// proposalListOwnedErrors is every sentinel proposal_list.go declares, keyed by its name.
//
// Nothing here is trusted. TestProposalListOwnedErrorsIsEveryRefusalItsFileDeclares holds it to
// what the file actually declares in BOTH directions, so a twelfth sentinel added with no row is
// judged by no sweep and a row for a name the file no longer declares fails rather than outliving
// it. This is the framing_errors.go shape, over the file ledger 30 is about.
var proposalListOwnedErrors = map[string]error{
	"errProfilePsk":                     errProfilePsk,
	"errProfileReInit":                  errProfileReInit,
	"errProfileExternalCommit":          errProfileExternalCommit,
	"errReservedProposalType":           errReservedProposalType,
	"errUnregisteredProposalType":       errUnregisteredProposalType,
	"errNilProposal":                    errNilProposal,
	"errForgedProposalDiscriminant":     errForgedProposalDiscriminant,
	"errAcceptedTypeHasNoBucket":        errAcceptedTypeHasNoBucket,
	"errProposalCacheNotAProposal":      errProposalCacheNotAProposal,
	"errProposalSenderNotMember":        errProposalSenderNotMember,
	"errProposalCacheEpoch":             errProposalCacheEpoch,
	"errProposalCacheNotRebound":        errProposalCacheNotRebound,
	"errProposalResolvedOutOfEpoch":     errProposalResolvedOutOfEpoch,
	"errProposalNotCached":              errProposalNotCached,
	"errDuplicateProposalReference":     errDuplicateProposalReference,
	"errMultipleGroupContextExtensions": errMultipleGroupContextExtensions,
	"errProposalCacheFull":              errProposalCacheFull,
	"errProposalCacheSenderQuota":       errProposalCacheSenderQuota,
	"errProposalCacheTargetQuota":       errProposalCacheTargetQuota,
	"errProposalCacheOctets":            errProposalCacheOctets,
	"errProposalCacheSenderOctets":      errProposalCacheSenderOctets,
	"errAcceptedTypeHasNoCeiling":       errAcceptedTypeHasNoCeiling,
}

// TestProposalListOwnedErrorsIsEveryRefusalItsFileDeclares derives the class the sweep below runs
// over rather than trusting the transcription of it.
func TestProposalListOwnedErrorsIsEveryRefusalItsFileDeclares(t *testing.T) {
	declared := packageSentinelTextsIn(mustParseSource(t, "proposal_list.go"))
	if len(declared) == 0 {
		t.Fatal("the scan found no errors.New declaration in proposal_list.go, which certainly holds several, so this gate compared the table against an empty set")
	}
	for _, name := range slices.Sorted(maps.Keys(declared)) {
		if _, listed := proposalListOwnedErrors[name]; !listed {
			t.Errorf("proposal_list.go declares %s and proposalListOwnedErrors does not list it, so no sweep judges it; add it there",
				name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(proposalListOwnedErrors)) {
		if _, held := declared[name]; !held {
			t.Errorf("proposalListOwnedErrors lists %s and proposal_list.go does not declare it, so the sweep runs over a name this file does not own",
				name)
		}
	}
}

// TestEveryRefusalOfThisFileIsItsOwnValue is ledger 30 stated as a test.
//
// Four rules of this file once shared one error value, and errors.Is cannot tell two rules apart
// when they answer the same one: a test asserting the broad question passes over a rule that fired
// for the wrong reason, and a caller branching on one is answered yes by another. The message is
// held distinct as well, because two rules reading the same sentence are indistinguishable in a
// log even when the values differ.
func TestEveryRefusalOfThisFileIsItsOwnValue(t *testing.T) {
	names := slices.Sorted(maps.Keys(proposalListOwnedErrors))
	for i, name := range names {
		first := proposalListOwnedErrors[name]
		if first == nil {
			t.Fatalf("%s is nil", name)
		}
		if !strings.HasPrefix(first.Error(), "mls: ") {
			t.Errorf("%s reads %q; every typed error of this package names the package it came from",
				name, first.Error())
		}
		for j, other := range names {
			if i == j {
				continue
			}
			second := proposalListOwnedErrors[other]
			if errors.Is(first, second) {
				t.Errorf("%s answers to %s (%v), so a caller branching on the two reads one as the other",
					name, other, first)
			}
			if first.Error() == second.Error() {
				t.Errorf("%s and %s both read %q, so the two are indistinguishable in a log",
					name, other, first.Error())
			}
		}
		// and every one of them is a distinct value from the framing sentinel the ARM rule
		// answers, because the type rule and the arm rule are two rules and this file owns
		// only one of them
		if errors.Is(first, ErrContentArmMismatch) || errors.Is(ErrContentArmMismatch, first) {
			t.Errorf("%s and ErrContentArmMismatch answer to each other", name)
		}
	}
}
