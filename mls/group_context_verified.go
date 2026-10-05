// Where a group context's AUTHORITY comes from, answered with a type whose only constructor is a
// signature check.
//
// THE QUESTION THIS FILE EXISTS FOR. The proposal cache binds itself to one group and one epoch,
// and that binding is only worth the authority of the value it was taken from. This package
// decodes a GroupContext straight off peer octets -- (*GroupInfo).UnmarshalMLS calls
// decoded.GroupContext.UnmarshalMLS, and a GroupInfo is what a joiner is handed inside a Welcome
// -- so "the argument's type is *GroupContext" says nothing whatever about whose epoch a cache is
// about to believe it is in. An attacker choosing a group id and an epoch of 1<<40 writes a
// perfectly well typed *GroupContext.
//
// THREE ROUNDS ANSWERED THAT WITH AN AST WALK AND ALL THREE WERE BYPASSED, each time by ordinary
// Go a reader would write on purpose: by any *GroupContext at all, because the walk read the
// argument's TYPE; by one local struct standing between the decode and the bind, which the walk
// LOGGED as acceptable because that local was not a type this package decodes; and twice over by
// an ordinary accessor and by an embedded wire type whose promoted selection the walk never
// consulted the type checker about. Provenance is where a value CAME FROM, and where a value came
// from is not a property of the text it is written in, which is why each round's fix was a shape
// the next reader bypassed by writing the advice down.
//
// THE FOURTH ROUND GOT THE TYPE RIGHT AND THE CONSTRUCTOR WRONG. VerifiedGroupContext -- one
// unexported field, so no declaration outside this package can build one carrying a context, IN
// SAFE GO, however it is spelled -- is the right shape and is kept. Its constructor was
// (*KeySchedule).ConfirmGroupContext, which confirmed a CONFIRMATION TAG against the epoch's own
// key schedule, and that establishes nothing at all:
//
//  1. IT WAS SELF CONFIRMING. The context entered through an exported KeySchedule constructor and
//     ConfirmationTag is exported on the same type, so s.ConfirmGroupContext(s.ConfirmationTag(h))
//     is a tautology any package can write. An external package laundered a decoded GroupInfo
//     naming "ATTACKER-CHOSEN-GROUP" at epoch 1<<40 into a cache in three lines.
//  2. EVEN THE HONEST JOINER FLOW CONFERRED NONE. A Welcome is HPKE-sealed to a PUBLISHED init
//     key, so the party that chose the joiner_secret is the same unauthenticated party that chose
//     the group context. Deriving the schedule from that secret over that context and then finding
//     the tag consistent is one party agreeing with itself.
//
// THE ROOT CAUSE, WHICH IS WHAT ALL FOUR ROUNDS WERE STANDING IN FOR. There was no GroupInfo
// signature verification anywhere in this build. GroupInfo declared toBeSigned, MarshalMLS and
// UnmarshalMLS and nothing else; VerifyWithLabel had production callers for FramedContentTBS,
// KeyPackage and LeafNode and NONE for GroupInfoTBS. So there was nothing in the package for a
// constructor to rest on, and every candidate constructor was a re-spelling of the caller's own
// say-so. welcome.go landed that check, and this file is what rests on it.
//
// SO THE CONSTRUCTOR IS (*GroupInfo).VerifiedContext AND IT IS (*GroupInfo).Verify. A verified
// GroupInfo is a statement by THE MEMBER AT LEAF Signer OF THE TREE THE CALLER HOLDS that this
// group context is the group's, made under the "GroupInfoTBS" label over the whole to-be-signed
// structure. That is a different KIND of fact from a confirmation tag: a tag is checked against a
// key the checker derived from a secret it was handed, and a signature is checked against a key
// the checker got from somewhere else entirely -- the tree. The signer's public key is not a field
// of GroupInfo, which is the whole reason Verify takes a tree and the whole reason this
// constructor does.
//
// WHAT IT DOES NOT ESTABLISH, INHERITED WHOLE FROM Verify AND WORTH MORE THAN THE PART IT DOES.
// Verify's own doc says it: "It establishes nothing about whether that tree is the group's tree".
// This constructor adds no judgement of its own -- every refusal it makes is one of Verify's -- so
// it inherits that gap exactly, and the gap is where the second demonstrated bypass still lives:
//
//   - THE TREE IS THE CALLER'S TRUST ANCHOR AND NOTHING HERE AUTHENTICATES IT. An attacker that
//     supplies the GroupInfo supplies the tree too -- whether the joiner lifted it out of the
//     GroupInfo's ratchet_tree extension or was handed it as its own parameter, which is the shape
//     p7 task 16's JoinFromWelcome writes; both octet strings come over one wire from one sender --
//     and a tree holding the attacker's own leaf verifies the attacker's own signature. So the
//     honest joiner flow over an attacker-chosen joiner_secret is closed ONLY relative to a tree
//     the caller already trusts.
//     What actually closes it is two things this package cannot do on its own account:
//     (*RatchetTree).ValidateAgainstContext, run by whoever obtained the tree, and an identity the
//     joiner ALREADY TRUSTS matched against at least one leaf's credential -- an authentication
//     service, which this build does not have. Until that exists, the caller of this constructor is
//     the party making the authority claim, and the tree in its parameter list is what says so.
//   - NOT THE CONFIRMATION TAG. A GroupInfo's confirmation_tag is inside GroupInfoTBS, so a
//     verified one is the tag that member published; whether it is the tag THIS epoch's schedule
//     computes needs an epoch secret this file never sees and is the key schedule's check.
//   - NOT THAT THE SIGNER WAS ENTITLED. Whether that member may publish a GroupInfo at all is the
//     group policy's, and this type is not a substitute for it.
//
// ONE DOOR, AND WHAT HOLDS IT IS THE COMPILER. The gates beside it are a review aid, and that
// division is written here because five rounds of hardening them had it the other way round.
//
// WHAT THE COMPILER HOLDS, IN SAFE GO, was measured from package mls_test -- outside mls, which
// is where every untrusted octet arrives from. The only exported route to a
// *VerifiedGroupContext is (*GroupInfo).VerifiedContext(crypto, tree); Context answers a COPY; the zero value is refused by
// NewProposalCache with a named error and its Context is nil; and four spellings that would carry a
// context in from out there are refused by the compiler, EACH WITH ITS OWN DIAGNOSTIC:
//
//	mls.VerifiedGroupContext{inner: c}
//	    cannot refer to unexported field inner in struct literal of type mls.VerifiedGroupContext
//	forged.inner = c
//	    forged.inner undefined (cannot refer to unexported field inner)
//	mls.VerifiedGroupContext(shadow{inner: c})
//	    cannot convert shadow{…} (value of struct type shadow) to type mls.VerifiedGroupContext
//	(*mls.VerifiedGroupContext)(&shadow{inner: c})
//	    cannot convert &shadow{…} (value of type *shadow) to type *mls.VerifiedGroupContext
//
// ONE DIAGNOSTIC QUOTED FOR ALL FOUR WOULD BE THE OVERCLAIM this section exists to remove, and an
// earlier version of this paragraph was exactly that. The first two fail on the FIELD REFERENCE.
// The two conversions fail on CROSS-PACKAGE STRUCT IDENTITY -- a struct declared out there with a
// field spelled inner is not this struct type, because an unexported field name carries the package
// that declared it -- which is a different rule answered in different words.
//
// UNSAFE IS OUT OF SCOPE, AND IT IS OUT OF SCOPE BY DECISION RATHER THAN UNNOTICED. Every "cannot"
// in this file is about SAFE Go and is now written that way. Measured from package mls_test, over
// a shadow struct declared out there:
//
//	forged := (*mls.VerifiedGroupContext)(unsafe.Pointer(&shadow{inner: attackersContext}))
//
// builds one carrying ATTACKER-CHOSEN-GROUP at epoch 1<<40, and NewProposalCache over it answers a
// cache and no error. There is no gate here for that and there should not be. An unsafe.Pointer
// conversion defeats every guarantee the type system makes, so a rule banning one spelling of it
// would be exactly the enumeration this header spends four rounds arguing against; and a package
// that can run unsafe in this process can rewrite the context AFTER any check whatever, which no
// shape of this type could notice. The boundary this file is about is the one the COMPILER
// decides, and the compiler is not asked about unsafe.
//
// WHICH OF THOSE A TEST HOLDS: all of them, and the four refusals DIRECTLY.
// TestEachForgedSpellingThisGateCompilesIsRefusedForItsOwnReason in external_provenance_test.go
// type checks a synthetic package outside mls, one file per spelling over a shared prologue,
// beside a file that differs only in the construction and MUST COMPILE, so a harness that had
// stopped resolving this package fails rather than reporting clean refusals. The spellings are
// generated off this type's own field list, so an unexported field renamed or added, or the last
// one exported, changes what is compiled rather than leaving the gate watching a name that has
// moved. An EXPORTED field added beside it is not one of those, and is the older and stronger
// rule TestTheZeroVerifiedGroupContextIsTheOnlyOneAnOutsiderCanSpell holds.
//
// EACH REFUSAL IS HELD TO WORDS NO OTHER ONE OF THEM CARRIES, and that separation is DERIVED there
// rather than trusted: the gate reads every refused spelling's diagnostics against every OTHER
// refused spelling's words and requires them to fall short. The first version of it held both
// field spellings to "unexported field" and the field's own name, which both diagnostics carry, so
// either would have satisfied the other's expectation. The two conversions are also held to the
// SHADOW they convert from -- go/types is asked whether that struct has this type's exact shape,
// field for field, and whether the two are nonetheless neither identical nor convertible -- because
// a shadow that had stopped having this shape is refused too, on the ordinary conversion rule, with
// a diagnostic that reads the same way.
//
// An earlier version of this paragraph said a compile error was not something a test in this build
// could assert, and gave that as the reason the boundary the whole guarantee rests on had no gate;
// go/types is in the standard library, this package already type checks source through it in four
// other gates, and this one costs about two and a half seconds.
//
// external_provenance_test.go also holds, from OUTSIDE, the property the first two refusals rest on
// -- the field is unexported -- together with the single exported door and the zero value's two
// refusals. The copy is held from INSIDE, in group_context_verified_test.go, by the aliasing test
// and by the behavioural gate over the compiled method set. The COMPILER is still what holds this
// boundary and no gate in this package is a substitute for it; what the gate adds is that this
// build notices the day it stops holding.
//
// WHAT THE GATES ARE FOR is the one package the compiler does not close. Within package mls an
// unexported field is visible to the whole package, so &VerifiedGroupContext{inner: x} is ordinary
// Go here and no source walk can enumerate the ways to write it. Every bypass five rounds of review
// turned up needed a DECLARATION ADDED TO THIS PACKAGE -- a defined struct type with the same
// underlying type, a non-empty interface carrying the pointer in an exported field, a type alias no
// matcher was unaliasing, an instantiated-generic collision in a cycle memo -- which is an
// in-package forgery, and that is a code review question rather than an attack. The gates in
// group_context_verified_test.go catch the ordinary spellings and name them against a table, so a
// reviewer notices a new door; both walk EVERY declaration of every file rather than function
// bodies, because a construction at package scope is invisible to a walk that only enters bodies
// and the package scope build was measured leaving the whole suite green. THEY ARE NOT EXHAUSTIVE.
// Five rounds each ended with a reviewer holding a spelling the round before had not enumerated, so
// read a green run here as "a reviewer was helped", never as "no declaration can evade this".
package mls

import (
	"fmt"

	"github.com/urnetwork/connect/mls/syntax"
)

// VerifiedGroupContext is a group context whose authority has been established, and it is the only
// thing the proposal cache will bind an epoch to.
//
// ONE UNEXPORTED FIELD, AND THAT FIELD IS THE WHOLE MECHANISM -- IN SAFE GO, AND FOR EVERY PACKAGE
// BUT THIS ONE. A struct whose only field is unexported cannot be built, in safe Go, carrying a
// value from any other package: mls.VerifiedGroupContext{} compiles out there and is the zero
// value, which every door refuses; a literal with an element and a write of the field are both
// "cannot refer to unexported field inner"; and a conversion from a struct declared out there with
// the same shape, in its value form and in its pointer form, is "cannot convert", which is a
// DIFFERENT refusal on a different rule -- an unexported field name carries the package that
// declared it, so that struct is not this struct type. That is the whole of the guarantee and it
// belongs to the compiler, and it stops where safe Go stops: an unsafe.Pointer conversion builds
// one from outside in a single line, which this file's header states, measures, and deliberately
// does not gate. All four refusals are asserted directly, by compiling them:
// external_provenance_test.go's TestEachForgedSpellingThisGateCompilesIsRefusedForItsOwnReason
// type checks a synthetic package outside mls, generates the spellings off this type's own
// unexported field list, and holds each to words no other one of them carries, beside a file that
// must compile. The same test file holds from outside the property the first two rest on, that
// this field is unexported.
//
// INSIDE THIS PACKAGE THE FIELD IS ORDINARY, so nothing here is a barrier and the gates over it are
// a review aid rather than a fence. An earlier version of this paragraph said the constructions
// were "a composite literal with an element, or a write of the field, and there is no third
// spelling in Go", and that was wrong twice over: a conversion from an identically shaped struct
// type declared IN this package is a third, and five rounds of review each found a spelling the
// round before had not enumerated. The gates hold the ordinary ones to a table, which is worth
// having and is not the same thing as closing the class. See this file's header.
//
// A POINTER TO A CONTEXT AND NOT A CONTEXT, so that the zero value is telling. A
// VerifiedGroupContext holding a GroupContext by value would have a zero value that reads as a
// perfectly ordinary context -- the empty group at epoch 0 -- and a forged one would bind a cache
// to that pair with no door able to notice. Held behind a pointer, the zero value carries nil and
// every consumer refuses it with ErrNilGroupContext, which is the same refusal a caller that passed
// nothing at all gets.
//
// THE CONTEXT IT HOLDS IS ITS OWN AND NOBODY IS HANDED IT. VerifiedContext decodes the octets the
// signature covered into a fresh structure, so nothing a caller still holds aliases what this value
// vouches for; Context hands out a Clone for the other half of the same statement. Both halves are
// one rule -- a verified value must not be changeable after it was verified -- and the second half
// is a CLASS rather than a line, because an accessor added later that answered self.inner would
// undo it with the existing one untouched. That is why the gate is over every declaration that
// takes one of these and answers a group context, and not over Context alone. Only a declaration of
// THIS package can be that accessor, and the gate over them helps a reviewer see one rather than
// closing the class -- see this file's header.
type VerifiedGroupContext struct {
	inner *GroupContext
}

// Context answers a COPY of the context this value vouches for, or nil for a value that vouches for
// nothing.
//
// A copy, because the point of the type is that the value cannot be changed after it was verified:
// handing out the pointer would let any holder rewrite the group id of a context a cache has
// already bound to, and the cache's own copy of the binding is not what a later caller would be
// comparing against.
//
// Nil rather than a panic for the zero value, so that a caller that got hold of one has an answer
// it can act on. Nothing in this package builds a zero VerifiedGroupContext; another package can
// spell one, and this is what it gets.
func (self *VerifiedGroupContext) Context() *GroupContext {
	if self == nil || self.inner == nil {
		return nil
	}
	return self.inner.Clone()
}

// VerifiedContext answers this GroupInfo's group context as a value whose authority is established,
// once the member at leaf Signer of this tree is shown to have signed it.
//
// IT IS Verify AND NOTHING ELSE, and that is the design rather than a saving of lines. Every
// refusal this method makes is one of Verify's four rules or one of its two argument refusals, so
// there is no second opinion here about what a good GroupInfo is -- a rule added to Verify is added
// to this door by existing, and a rule weakened there is weakened here in the same breath rather
// than being papered over by a duplicate check that drifted. What that costs is written in this
// file's header: this method inherits every gap Verify has, and the biggest of them is that the
// TREE is the caller's to authenticate.
//
// IT TAKES THE GroupInfo AND NOT A GroupContext. The earlier constructor took no context at all and
// claimed that as its safety -- "there is nothing in its signature to launder" -- and the claim was
// empty, because what it did take was a schedule the same caller had just built over the same
// context. The parameter that matters is not the one carrying the value, it is the one carrying the
// AUTHORITY, and here that is the tree: the signer's public key is not in a GroupInfo, so the tree
// is the only thing in this signature that can say who the members are. A caller that passes a tree
// it got out of the same message it is checking has authenticated nothing, and no shape of this
// signature could stop it.
//
// THE CONTEXT ANSWERED IS DECODED OUT OF ITS OWN SERIALIZATION rather than copied off the receiver,
// for two reasons that are the same reason twice. What the signature covers is the OCTETS
// GroupContext.MarshalMLS writes -- GroupInfoTBS.MarshalMLS writes them first and inline, with no
// framing of its own -- so a field this struct might carry that the codec does not write is a field
// nobody signed, and a copy of the struct would carry it into a value that claims to be verified.
// And decoding leaves the answer owning its own storage, so no later write through the caller's
// GroupInfo can move the group a cache believes it is in.
//
// A NIL RECEIVER IS NOT CHECKED, which is (*GroupInfo).Verify's own convention one line down: a
// method reached on no group info at all is a caller's own defect and not a peer's, and inventing a
// sentinel for it here would put this door in the nil-argument refusal class over a fault that has
// no wire representation.
func (self *GroupInfo) VerifiedContext(crypto CryptoProvider, tree *RatchetTree) (*VerifiedGroupContext, error) {
	if err := self.Verify(crypto, tree); err != nil {
		return nil, err
	}
	// the octets the signature covered, taken back apart. syntax.Marshal here writes exactly what
	// GroupInfoTBS.MarshalMLS wrote first, because a group context is encoded inline and carries
	// no framing of its own.
	signed, err := syntax.Marshal(&self.GroupContext)
	if err != nil {
		return nil, err
	}
	decoded := &GroupContext{}
	if err := syntax.Unmarshal(signed, decoded); err != nil {
		// these bytes were produced by marshalling a GroupContext one statement ago, so a
		// refusal here is this build disagreeing with its own codec rather than anything a peer
		// did
		return nil, fmt.Errorf("mls: the verified group info's own group context did not decode: %w", err)
	}
	return &VerifiedGroupContext{inner: decoded}, nil
}
