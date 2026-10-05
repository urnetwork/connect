// The v1 profile gate for proposals, plus the two small type predicates the commit path needs.
// Spec A sections 3.1 and 3.2.
//
// RFC 9420 registers seven proposal types and the codec in proposal_wire.go decodes all of them
// plus an unregistered arm, because the `messages` vector family cannot be green otherwise and
// because a decoder that refused three of them would be the thing deciding policy. Refusing three
// is a PROFILE decision, so it lives here, at the boundary where a Proposal enters this plan's
// state -- the cache, resolution, and ValSem113 -- which is the difference between a message that
// is dropped and a message whose unimplemented arm is silently skipped.
//
// WHAT THIS FILE OWNS AND WHAT IT IS STANDING IN FOR. `Profile`, `DefaultProfile` and the seven
// `Check*` gates are the VALIDATION plan's (p8 section 9.3, connect/mls/profile.go), and that
// plan owns the single declaration site for each. None of them has landed. So the surface here is
// unexported and lowercase, the shape credential.go's errProfileCredentialType and
// key_package.go's errProfileCiphersuite already take: an exported Profile declared here would be
// a second public declaration site for a name p8 also declares, and the four refusals would be
// four error VALUES that no caller outside this package could match with errors.Is even after the
// real ones landed. Every consumer of this gate in the tasks after this one is inside package mls,
// so the swap costs nobody else anything -- when p8 lands profile.go, `profile` becomes `*Profile`,
// `defaultProfile()` becomes `DefaultProfile()`, `checkProposalType` is deleted in favour of
// `(*Profile).CheckProposalType`, and each err* below is replaced by its exported twin at every
// use. Those are all in-package, so the compiler finds every one of them in that commit, and
// TestNoValidationOwnedNameHasLandedBesideItsStandIn fails until the four errors are swapped.
package mls

import (
	"bytes"
	"crypto/subtle"
	"errors"
	"fmt"
	"slices"

	"github.com/urnetwork/connect/mls/syntax"
)

// The refusals this gate answers, standing in for the validation plan's catalogue.
//
// One value per rule and never one value shared by two, which is errors_lifecycle.go's rule and
// the reason p8's catalogue spells them separately: a caller -- or a test -- asking which of the
// three unimplemented types it was handed cannot tell them apart when all three answer one value,
// and "a set of refusals that all reduce to one comparison" is this project's most repeated
// defect. errProfileExternalCommit rather than errProfileExternalInit is p8's spelling and is
// kept, because external_init is the proposal an external COMMIT carries and the refusal is about
// the commit shape rather than about one arm.
//
// THE RULE ABOVE ONCE DID NOT HOLD IN THIS FILE, which is why the list is longer than the plan's
// four. Five rules of this gate -- a RESERVED code point, a code point OUTSIDE the registry, a
// NIL proposal, a wire DISCRIMINANT that is not the type the proposal names, and an accepted type
// resolution has no BUCKET for -- all answered one value whose message read "proposal type is not
// one this build processes". That is not a spelling question. This gate has two callers and they
// need opposite handling: ProposalCache.Store judges what a PEER sent, where "a code point this
// build does not implement" is routine traffic to drop, and Resolve judges what this build's own
// commit path ASSEMBLED, where a forged discriminant means this build is emitting proposals whose
// ProposalRef every receiver computes over octets naming a different type -- messages that will
// not verify, produced by us. A caller handed one value cannot separate the two, and the forged
// case was reported to it as a sentence about a registry, over a proposal whose type is
// registered and whose octets are the whole of what is wrong.
var (
	errProfilePsk            = errors.New("mls: pre_shared_key proposals are outside the v1 profile")
	errProfileReInit         = errors.New("mls: reinit proposals are outside the v1 profile")
	errProfileExternalCommit = errors.New("mls: external commits are outside the v1 profile")

	errReservedProposalType       = errors.New("mls: the reserved code point names no proposal")
	errUnregisteredProposalType   = errors.New("mls: proposal type is not in this build's proposal type registry")
	errNilProposal                = errors.New("mls: the v1 profile gate was handed no proposal")
	errForgedProposalDiscriminant = errors.New("mls: proposal would be encoded under a discriminant other than the type it names")
	errAcceptedTypeHasNoBucket    = errors.New("mls: the v1 profile accepts a proposal type resolution has no bucket for")
)

// proposalTypeProfile is the v1 disposition of every REGISTERED proposal type: nil for the four
// this build implements, and the sentinel that names why for the three it does not.
//
// A map keyed by the registry's own constants rather than a switch listing four accepted names,
// because the accepted set is the complement of a refused set and only one of the two can be
// written down without understating the other. What holds it to the registry is
// TestTheV1ProfileClassifiesEveryRegisteredProposalType, which reads the ProposalType constants
// out of this package's source and compares the two sets in BOTH directions: an eighth code point
// registered in extension.go with no row here fails, and a row here for a code point that no
// longer exists fails too. That is the shape rule 5 asks for -- the table is an enumeration only
// if nothing derives the class it is supposed to cover.
//
// ProposalTypeReserved is in the map and refused. It is a registry code point, so leaving it out
// would make the two sets unequal for a reason that has nothing to do with the profile, and it is
// never a legal proposal type -- which is a rule of its OWN and not the unregistered one. 0x0000
// IS in the registry; what it is registered as is "not a proposal", and a caller told "this code
// point is not in the registry" about it would go looking for a registration that is right there.
var proposalTypeProfile = map[ProposalType]error{
	ProposalTypeReserved:               errReservedProposalType,
	ProposalTypeAdd:                    nil,
	ProposalTypeUpdate:                 nil,
	ProposalTypeRemove:                 nil,
	ProposalTypePreSharedKey:           errProfilePsk,
	ProposalTypeReInit:                 errProfileReInit,
	ProposalTypeExternalInit:           errProfileExternalCommit,
	ProposalTypeGroupContextExtensions: nil,
}

// profile is the stand-in for the validation plan's Profile: the set of narrowings this build
// applies on top of RFC 9420.
//
// It carries no field yet because the only narrowing this task states is over proposal types, and
// a field nothing reads is a decision nobody made. p8's Profile carries AllowPublicMessage and
// the other six Check* gates; when it lands, this type is deleted rather than extended.
type profile struct{}

// defaultProfile is the v1 profile every caller in this plan runs under.
func defaultProfile() *profile {
	return &profile{}
}

// checkProposalType is the one refusal surface for a proposal type, standing in for
// (*Profile).CheckProposalType.
//
// The two answers it separates are worth separating: a REGISTERED type this profile does not
// implement is refused with the sentinel naming which one, so a peer running a fuller profile can
// be told what this build will not do, and an UNREGISTERED one -- a GREASE value, or a code point
// registered after this was written -- is refused with its own sentinel, because there is no
// narrowing to name and the remedy is a different one: a registered refusal is a decision this
// profile made and could revisit, an unregistered one is a code point nothing here has ever heard
// of. Both are refusals; a v1 client acts on four proposal types and no others.
func (self *profile) checkProposalType(proposalType ProposalType) error {
	refusal, registered := proposalTypeProfile[proposalType]
	if !registered {
		return fmt.Errorf("%w: %s is not a registered proposal type",
			errUnregisteredProposalType, proposalTypeName(proposalType))
	}
	if refusal != nil {
		return fmt.Errorf("%w: %s is registered and outside the v1 profile",
			refusal, proposalTypeName(proposalType))
	}
	return nil
}

// checkProposalProfile refuses any proposal this build will not process.
//
// Four rules, in the order a caller wants to hear them:
//
//  0. that there IS a proposal. A nil one has no type, no discriminant and no arm, so it is not
//     an unsupported type -- it is a caller that reached this gate holding nothing, which is a
//     commit path bug and not a message to drop. It answers its own value for that reason.
//  1. the TYPE, through the one refusal surface above;
//  2. the WIRE DISCRIMINANT, which is not always the type -- see below;
//  3. the ARM, through (*Proposal).checkArm, which is proposal_wire.go's and is not restated
//     here. The plan's own sketch wrote a second arm count in this file; that count is derived
//     off the Proposal struct by reflection in TestEveryArmOfAProposalIsCountedByItsArmCheck, and
//     a hand written copy of it beside the derived one is exactly the second implementation the
//     one-codec rule exists to prevent. The two would agree until an eighth arm was added, and
//     the copy that had not been updated would be the one this gate ran.
//
// Rule 2 is the one the plan does not have. Proposal.MarshalMLS puts UnknownType on the wire when
// it is set and selects the arm by ProposalType, which is deliberate -- it is how the validation
// plan's forge emits a well formed body under a GREASE code point without a second encoder
// existing. But it means the type this gate is asked about and the type the OCTETS carry can
// differ, and every ProposalRef, every transcript hash and every receiver's own gate run on the
// octets. A proposal admitted here as an Add and encoded under 0x0005 is a reference the proposer
// believes is an Add and every receiver refuses as a reinit. This plan never emits one, so it
// refuses to act on one.
//
// RULE 2 IS ABOUT THE DISAGREEMENT AND NOT ABOUT UnknownType BEING SET. The clause that landed
// read "UnknownType is set at all" while this comment and the clause's own test both said "the
// discriminant disagrees", so the file implemented one rule and documented another, and narrowing
// it to the documented one left the whole tree green.
//
// It left the tree green because the two readings differ over EXACTLY ONE input: an accepted type
// whose UnknownType equals it. Every other input is decided before this clause or by both
// readings alike -- a refused or unregistered type is answered by rule 1 above, and a genuine
// disagreement is refused either way -- and both fixtures in the clause's own test forge a
// genuine disagreement, so neither could tell the two apart. Which is also why the narrow rule
// costs the clause a backstop it used to have by accident: a decoded GREASE proposal carries
// UnknownType equal to ProposalType, because that is how proposal_wire.go makes GREASE round
// trip, so under the wide reading this clause refused one even with rule 1 switched off. Rule 1
// is the rule such a proposal breaks and is stated over all 65536 code points in a test of its
// own, and a second clause answering the same input with a different sentinel is not a backstop,
// it is two rules a caller cannot tell apart.
//
// That one input is what decides it. When the two are equal MarshalMLS writes the same
// discriminant it would write with UnknownType unset and selects the same arm, so the octets are
// byte for byte an ordinary add's: there is no second reading for any receiver to take, nothing
// about the value is wrong, and the refusal it earned read "add would be encoded under the
// discriminant add", which is not a sentence about a fault. The rule this clause exists for is
// octets carrying a type other than the one this gate judged, and the equal case carries the one
// it judged.
func checkProposalProfile(active *profile, proposal *Proposal) error {
	if proposal == nil {
		return fmt.Errorf("%w: nothing was handed to the type rule", errNilProposal)
	}
	if active == nil {
		active = defaultProfile()
	}
	if err := active.checkProposalType(proposal.ProposalType); err != nil {
		return err
	}
	if proposal.UnknownType != ProposalTypeReserved && proposal.UnknownType != proposal.ProposalType {
		return fmt.Errorf("%w: %s would be encoded under the discriminant %s",
			errForgedProposalDiscriminant, proposalTypeName(proposal.ProposalType),
			proposalTypeName(proposal.UnknownType))
	}
	return proposal.checkArm()
}

// proposalTypePathRequired is the RFC 9420 section 12.4 pathRequiredTypes set:
// [update, remove, external_init, group_context_extensions].
//
// It is stated over the whole registry rather than over the four this profile implements,
// because it is the RFC's rule and not this profile's: external_init is in the set and is refused
// by the gate above, and a predicate that answered false for it would be a wrong answer to a
// question about RFC 9420 that some later reader would take at face value. Which of the two rules
// a commit meets first is the caller's business.
func proposalTypePathRequired(proposalType ProposalType) bool {
	switch proposalType {
	case ProposalTypeUpdate, ProposalTypeRemove,
		ProposalTypeExternalInit, ProposalTypeGroupContextExtensions:
		return true
	}
	return false
}

// proposalTypeName names a type for an error message, in the RFC's own spelling.
//
// Deliberately not a String method on ProposalType: the registry enum is extension.go's and this
// plan does not extend it. The fallback names the code point in hex, because a refusal that said
// only "unsupported" would leave a caller diffing two captures to find out which value arrived.
func proposalTypeName(proposalType ProposalType) string {
	switch proposalType {
	case ProposalTypeReserved:
		return "reserved"
	case ProposalTypeAdd:
		return "add"
	case ProposalTypeUpdate:
		return "update"
	case ProposalTypeRemove:
		return "remove"
	case ProposalTypePreSharedKey:
		return "psk"
	case ProposalTypeReInit:
		return "reinit"
	case ProposalTypeExternalInit:
		return "external_init"
	case ProposalTypeGroupContextExtensions:
		return "group_context_extensions"
	}
	return fmt.Sprintf("proposal_type(%#04x)", uint16(proposalType))
}

// ---------------------------------------------------------------------------
// the per epoch proposal cache and commit resolution, RFC 9420 section 12.4
// ---------------------------------------------------------------------------

// What the cache refuses, one value per rule and never one value shared by two, which is
// errors_lifecycle.go's rule and the reason the four profile refusals above are spelled
// separately. A caller told only "the cache said no" cannot tell a reference it never published
// from a reference it published twice from one belonging to an epoch that has closed, and those
// are three different faults with three different remedies.
//
// THE EPOCH RULES ARE THREE VALUES for what a careless reading calls one rule, and they are three
// rules with three remedies. errProposalCacheEpoch is about a MESSAGE: a proposal arrived carrying
// an epoch other than the one the group is in, and the remedy is to drop it. The other two are
// about THIS BUILD, at two different doors. errProposalCacheNotRebound is Store's and CheckEpoch's:
// the cache is bound to an epoch the caller has already left, so the lifecycle advanced a group and
// did not Rebind, and the remedy is in the declaration that advanced it -- there is nothing wrong
// with any message. errProposalResolvedOutOfEpoch is Resolve's: the same staleness caught at the
// lookup, where what it costs is a commit naming proposals the group has already applied, so the
// refusal is a property of the resolution rather than a discipline expected of a caller.
//
// Three and not one because the audiences differ. A caller told only errProposalCacheEpoch cannot
// tell a peer that mislabelled one message from its own lifecycle failing to carry a whole cache
// across a boundary, and ledger 30 is four rules of this file sharing one value.
//
// Unexported, on the shape credential.go's errProfileCredentialType and this file's own
// errProfile* take, for two reasons that are not the same reason. errProposalSenderNotMember and
// errMultipleGroupContextExtensions stand in for names the VALIDATION plan owns -- section 12.2's
// "the sender of an Update is a member" is ValSem112 and "a list contains multiple
// GroupContextExtensions proposals" is one of that section's list rules -- and p7 task 7 declares
// ValSem112UpdateSenderIsMember as the named function those tests call, so an exported spelling
// here would be a second public declaration site for somebody else's rule.
// TestNoValidationOwnedNameHasLandedBesideItsStandIn watches every one of them for an exported
// twin. The other three are this cache's own invariants -- what a cache entry IS, what a
// reference NAMES, and which epoch it belongs to -- and they are unexported because nothing
// outside this package holds a ProposalCache.
var (
	errProposalCacheNotAProposal      = errors.New("mls: a cache entry must be a framed proposal")
	errProposalSenderNotMember        = errors.New("mls: a cached proposal is attributed to a leaf and only a member sender occupies one")
	errProposalCacheEpoch             = errors.New("mls: proposal does not belong to the epoch this cache holds")
	errProposalCacheNotRebound        = errors.New("mls: the proposal cache is bound to an epoch the caller has already left")
	errProposalResolvedOutOfEpoch     = errors.New("mls: a commit names a cached proposal from an epoch that has closed")
	errProposalNotCached              = errors.New("mls: proposal reference is not cached for this epoch")
	errDuplicateProposalReference     = errors.New("mls: a commit names one proposal reference twice")
	errMultipleGroupContextExtensions = errors.New("mls: a proposal list carries more than one group_context_extensions proposal")

	// THE SIX CEILING RULES are six values for what a careless reading calls one rule, and they
	// are six rules with six remedies, in three PAIRS of a group rule and a sender rule.
	//
	// errProposalCacheFull is about the EPOCH: this group has published more proposals than one
	// valid commit list could name, and the remedy is for somebody to commit.
	// errProposalCacheSenderQuota is about ONE SENDER: that member has published more of one type
	// than a valid list could carry from it, and the remedy is to stop trusting the member -- a
	// caller told only "the cache is full" would go looking at the group.
	//
	// errProposalCacheTargetQuota is that sender rule stated over the leaf a proposal APPLIES TO.
	// Section 12.2 invalidates a list carrying two proposals that apply to one leaf, so a member's
	// second remove of the leaf it has already asked to remove is an entry no committer can use
	// beside the first, and admitting it fills the cache with a set no valid commit list names.
	//
	// errProposalCacheOctets and errProposalCacheSenderOctets are that same pair over the BYTES,
	// which are a different resource from the count and the one an entry ceiling alone does not
	// bound. They are a PAIR for the reason the counted ones are: a total with no per sender
	// column beside it is a total ONE sender reaches, and every honest member is then refused for
	// the rest of the epoch -- the starvation, not the exhaustion.
	//
	// errAcceptedTypeHasNoCeiling is about THIS BUILD, exactly as errAcceptedTypeHasNoBucket
	// beside it is: the profile accepts a type nothing gave a ceiling to -- in either column --
	// which is a commit that widened the accepted set and stopped half way.
	errProposalCacheFull         = errors.New("mls: the epoch's proposal cache holds as many proposals as one valid commit list could name")
	errProposalCacheSenderQuota  = errors.New("mls: one sender has cached as many proposals of one type as a valid commit list could carry from it")
	errProposalCacheTargetQuota  = errors.New("mls: one sender has cached a second proposal applying to a leaf it already holds one for, and no valid commit list carries both")
	errProposalCacheOctets       = errors.New("mls: the epoch's cached proposals would exceed the octets one epoch's cache may hold")
	errProposalCacheSenderOctets = errors.New("mls: one sender's cached proposals would exceed the octets one sender may spend of one epoch's cache")
	errAcceptedTypeHasNoCeiling  = errors.New("mls: the v1 profile accepts a proposal type the cache has no ceiling for")
)

// CachedProposal is a proposal plus the provenance validation needs: who sent it, and whether
// the commit carried it inline.
//
// Sender is a LEAF INDEX and not a Sender, which is what fixes the one rule Store has to enforce
// before anything else can. A leaf index attributed to a proposal whose sender was not a member
// is a number the sender never occupied, and ValSem111 -- the committer must not cover its own
// Update -- is then a comparison against a fabricated value. Task 7 re-checks the same property
// over a list assembled in process; this is the door every RECEIVED proposal comes through.
type CachedProposal struct {
	Ref      ProposalRef
	Proposal Proposal
	Sender   LeafIndex
	ByValue  bool
}

// ProposalList is one commit's proposals: the commit ORDER, and the per-type views of it that
// RFC 9420 section 12.2's rules are stated over.
//
// ONE REPRESENTATION, AND THAT IS THE WHOLE OF WHAT THIS TYPE DECIDES. The order is the only
// thing stored. Adds, Updates, Removes and GCE are that order FILTERED, computed at the read, so
// a list whose removes disagree with its commit order cannot be constructed -- there is no second
// field to fill in beside the order, and no index that could fall behind it.
//
// THAT IS THE REPAIR OF A FAULT THAT GOT PAST TWO GATES, not a preference. The views used to be
// exported fields a caller wrote beside All, and the two doors of this package read different
// ones: ApplyProposals walked Updates and Removes while every rule of validate_proposals.go read
// the buckets, and the door held the two together by a per-type COUNT. So a list carrying
// All=[remove(committer)] beside Removes=[remove(3)] was accepted by ValidateCommit and applied
// by removing leaf 3 -- one member applying a different commit from the one the transcript
// covers, with every count equal. Three more of that shape were verified against the counting
// door: an Add colliding with the update path leaf key, an Update publishing it, and a
// GroupContextExtensions installing an extension outside the v1 profile, each hidden behind an
// innocent entry of its own type. A count is a CHECK and leaves the dual representation standing
// for the next reader; deriving the views removes it, and the two rules that used to check for
// the disagreement are gone rather than reduced.
//
// THE DERIVED ORDER IS THE ORDER SECTION 12.3 ASKS FOR, which was confirmed rather than assumed.
// Section 12.3's application order is GroupContextExtensions, then Updates, then Removes, then
// Adds "in the order they appear in the proposals vector". Only the Add clause names an order at
// all -- updates and removes are applied "in any order", which section 12.2's same-leaf rule is
// what makes safe -- so a view answering the commit order filtered gives section 12.3 exactly the
// order it names for adds and a permitted order everywhere else. Nothing here sorts, and nothing
// here would be entitled to.
type ProposalList struct {
	// order is the commit's ProposalOrRef vector resolved, and it is the only proposal storage
	// this type has. Unexported with one constructor for VerifiedGroupContext's reason: a field
	// a caller can write is a second representation of the same fact however many rules are
	// stood over it, and this package has now spent two rounds writing those rules.
	order []CachedProposal
}

// NewProposalList is how a caller outside a resolution builds a list.
//
// IT TAKES THE COMMIT ORDER AND NOTHING ELSE, because there is nothing else to hand it: every
// per-type view is a function of this vector. A caller that wants a list with one remove in it
// passes a vector with one remove in it, and cannot pass a vector and a disagreeing bucket
// because there is no bucket to pass.
//
// THE VECTOR IS CLONED ALL THE WAY DOWN, and the depth is the point rather than a detail.
// slices.Clone copies headers: it stops a caller appending into this list's commit order, and it
// stops nothing else, because every entry it copies carries the SAME *Add, *Update, *Remove and
// *GroupContextExtensions the caller still holds. Measured, before this line was written:
// `order[0].Proposal.Remove.Removed = LeafIndex(99)` after construction moved the list's remove
// target from leaf 3 to leaf 99 -- a list changing under a validator that has already judged it,
// which is the class this type was rebuilt for, one level out from the buckets. So each entry's
// proposal is copied through the codec and each entry's reference is copied byte for byte, and
// what a caller keeps a pointer into afterwards is its own value.
//
// THROUGH THE CODEC AND NOT ARM BY ARM, for cloneProposal's own reason: seven arms copied by hand
// is a copy that silently drops the eighth the day one is registered, into a list whose whole job
// is to say what the group agreed to.
//
// AN ENTRY THIS BUILD CANNOT ENCODE IS CARRIED THROUGH AS IT WAS HANDED, which is the one case
// where the caller keeps its pointers. A proposal with no arm, or with an arm the codec refuses,
// is one checkProposalListStructure refuses at both doors before any rule reads it -- and the
// fixtures that drive those refusals are built here. Dropping it would change the commit order's
// LENGTH, which is the single fact about a list no door can recover and the one thing the vector
// join compares first; refusing it would need an error return on a constructor whose whole
// argument is that there is nothing about a commit order to get wrong.
func NewProposalList(order []CachedProposal) *ProposalList {
	held := slices.Clone(order)
	for i := range held {
		held[i].Ref = bytes.Clone(held[i].Ref)
		copied, _, err := cloneProposal(&held[i].Proposal)
		if err != nil {
			continue
		}
		held[i].Proposal = copied
	}
	return &ProposalList{order: held}
}

// All is the commit order: this commit's ProposalOrRef vector resolved, in the order the sender
// signed it.
//
// THE ORDER IS WHAT IS STORED AND THE VIEWS ARE WHAT IS DERIVED, rather than the other way round,
// because the order is the half that cannot be recovered. RFC 9420 section 12.1.1 places an added
// member at the leftmost blank leaf in the order the Add proposals appear, so two commits
// carrying the same set of adds in a different order build different trees, different tree hashes
// and different confirmed transcripts. A set of buckets cannot say which order that was; the
// order can always say what the buckets are.
//
// It answers the list's own vector rather than a copy of it. That is a statement about allocation
// and not about representation: a caller writing through the slice it is handed writes the ONE
// place this list keeps its proposals, and every view answers that write on the next read.
func (self *ProposalList) All() []CachedProposal {
	return self.order
}

// Adds is the commit order filtered to Add proposals, in commit order.
func (self *ProposalList) Adds() []CachedProposal {
	return self.viewOf(ProposalTypeAdd)
}

// Updates is the commit order filtered to Update proposals, in commit order.
func (self *ProposalList) Updates() []CachedProposal {
	return self.viewOf(ProposalTypeUpdate)
}

// Removes is the commit order filtered to Remove proposals, in commit order.
func (self *ProposalList) Removes() []CachedProposal {
	return self.viewOf(ProposalTypeRemove)
}

// GCE is the commit order filtered to GroupContextExtensions proposals, in commit order.
//
// Section 12.2 makes a list carrying two of them invalid and both doors of this package refuse
// such a list, so over every list this package accepts this answers nought or one entry.
func (self *ProposalList) GCE() []CachedProposal {
	return self.viewOf(ProposalTypeGroupContextExtensions)
}

// viewOf is the derivation the four accessors above are, written once.
//
// FILTERED AT EVERY READ RATHER THAN INDEXED ONCE, and that is measured rather than a taste. An
// index built at construction would be a second representation again -- an unexported one with no
// setter, which is weaker than none at all, because it can still fall behind an in-package write
// to the order it was built from. What it would buy is the recomputation, and the recomputation
// was measured rather than guessed at:
// TestDerivingTheViewsCostsLessThanTheRulesThatReadThem times the whole of section 12.2 against
// exactly the view reads that aggregate makes -- twenty of them, counted off validate_proposals.go
// rather than by hand -- over a list of 97 proposals in a group of 96, and the filtering is 33 us
// against the aggregate's 3.2 ms. One percent, for a validation a member runs once per epoch. The
// bound that test enforces is a tenth, which an accessor gone worse than linear reaches and
// nothing else does; TestNoReaderOfAPerTypeViewFiltersItInsideItsOwnLoop is the other half, and it
// is what makes "filtered at every read" affordable rather than merely cheap in one fixture.
//
// The allocation is skipped entirely for a type the commit does not carry, which is the ordinary
// case for three of the four.
func (self *ProposalList) viewOf(carries ProposalType) []CachedProposal {
	var out []CachedProposal
	for i := range self.order {
		if self.order[i].Proposal.ProposalType == carries {
			out = append(out, self.order[i])
		}
	}
	return out
}

// proposalBucket is one per-type view of a list: the ACCESSOR that answers it, the type every
// entry it answers carries, and the entries.
type proposalBucket struct {
	accessor string
	carries  ProposalType
	entries  []CachedProposal
}

// proposalBucketsOf is every per-type view of a list, and it is the one place they are enumerated.
//
// Four rows written by hand is the shape that understates its class the moment a fifth view is
// added -- a psk view, when the profile widens -- so it is held to the TYPE rather than to
// memory. TestEveryPerTypeViewOfAProposalListIsNamedByTheViewRule reflects over *ProposalList's
// own method set and requires every method answering []CachedProposal, except the commit order
// one, to appear here; TestEveryPerTypeViewOfAProposalListIsItsCommitOrderFiltered holds each
// row's entries to the filter of All by that row's own type, element by element and not by count.
func proposalBucketsOf(list *ProposalList) []proposalBucket {
	return []proposalBucket{
		{accessor: "Adds", carries: ProposalTypeAdd, entries: list.Adds()},
		{accessor: "Updates", carries: ProposalTypeUpdate, entries: list.Updates()},
		{accessor: "Removes", carries: ProposalTypeRemove, entries: list.Removes()},
		{accessor: "GCE", carries: ProposalTypeGroupContextExtensions, entries: list.GCE()},
	}
}

// proposalListViewedTypes is the set of proposal types this build answers a named view for, read
// off proposalBucketsOf rather than written down a second time.
func proposalListViewedTypes() map[ProposalType]bool {
	viewed := map[ProposalType]bool{}
	for _, bucket := range proposalBucketsOf(&ProposalList{}) {
		viewed[bucket.carries] = true
	}
	return viewed
}

// Len is the total proposal count.
func (self *ProposalList) Len() int {
	return len(self.order)
}

// PathRequired is the RFC 9420 section 12.4 rule: a commit carries an UpdatePath if its proposal
// list is EMPTY or contains any member of pathRequiredTypes.
//
// The empty half is the one that is easy to drop and is the one that matters most. A commit with
// no proposals and no path changes no key material at all, so the epoch advances over a secret
// every member of the previous epoch still holds -- which is the whole of what an update commit
// exists to prevent.
func (self *ProposalList) PathRequired() bool {
	if len(self.order) == 0 {
		return true
	}
	for _, cached := range self.order {
		if proposalTypePathRequired(cached.Proposal.ProposalType) {
			return true
		}
	}
	return false
}

// Extensions answers the replacement group context extensions when the list carries a
// GroupContextExtensions proposal.
//
// GCE[0], and not "the last one wins" or "the first one wins", because Resolve refuses a second
// one outright. RFC 9420 section 12.2 makes a list carrying two of them invalid, and a list that
// silently applied one of the two would be the group agreeing to an extension set no member can
// name. The index is therefore exact for every list this package produces. A ProposalList a
// caller assembled by hand can still hold two, and this answers the first of them -- see the note
// on errMultipleGroupContextExtensions.
//
// The hand assembled list is not a hypothetical and is why the index is held by a test of its
// own. Every list Resolve builds carries at most one GCE, so over those lists GCE()[0] and
// GCE()[len-1] are the same entry and no test that goes through Resolve can tell them apart --
// measured: the whole suite was green with the last one answered. A commit order a caller hands
// NewProposalList can carry two, and it is read through
// (*ProposalValidationInput).effectiveExtensions, where the two differ.
// TestExtensionsAnswersTheFirstOfTwoInAHandAssembledList is what separates them.
func (self *ProposalList) Extensions() ([]Extension, bool) {
	gce := self.GCE()
	if len(gce) == 0 {
		return nil, false
	}
	// a GCE entry carrying no GroupContextExtensions arm is a malformed list and every door that
	// judges one refuses it -- checkProposalListStructure is that rule, and both validation inputs
	// run it before any rule below reads an arm. This is an exported method with no error to answer, so
	// what it owes is the one thing a door must not do: it does not dereference the missing arm.
	// "No extension set this list can name" is also the fail-closed answer for the two callers,
	// which fall back to the group's own extensions rather than to a set read off nothing.
	if gce[0].Proposal.GroupContextExtensions == nil {
		return nil, false
	}
	return gce[0].Proposal.GroupContextExtensions.Extensions, true
}

// Refs rebuilds the ProposalOrRef vector a commit carries, in commit order.
func (self *ProposalList) Refs() []ProposalOrRef {
	out := make([]ProposalOrRef, 0, len(self.order))
	for i := range self.order {
		cached := self.order[i]
		if cached.ByValue {
			proposal := cached.Proposal
			out = append(out, ProposalOrRef{Type: ProposalOrRefTypeProposal, Proposal: &proposal})
			continue
		}
		out = append(out, ProposalOrRef{Type: ProposalOrRefTypeReference, Reference: cached.Ref})
	}
	return out
}

// ---------------------------------------------------------------------------
// what one epoch's cache may hold: RFC 9420 section 12.2 over spec A section 3.1
// ---------------------------------------------------------------------------

// NEITHER RFC 9420 NOR SPEC A STATES A BOUND ON THIS CACHE. That is a spec gap and it is written
// down here rather than resolved in silence, because this project has twice had an implementer
// settle an ambiguity the spec never settled and both times review found it later.
//
// What the two documents do say. RFC 9420 section 12.4 says a committer SHOULD include all valid
// pending proposals and says nothing about how many there may be; section 15 asks applications to
// state exactly this kind of policy for the message ratchet -- "a policy on how long to keep unused
// nonce and key pairs for a sender, and the maximum number to keep" -- and stops there, which is the
// RFC putting this class of bound on the application rather than forgetting it. Spec A section 3.1
// states the v1 profile's caps on members and on devices and states none on proposals. So the
// numbers below are THIS FILE'S, argued from the ones the profile does state, and the argument is
// here because the next reader will need it in order to change them.
//
// WHY THERE HAS TO BE ONE. Store is the door every RECEIVED proposal comes through and it had no
// limit anywhere: measured, 20,000 distinct well formed proposals of one epoch from one sender were
// all accepted. Distinct because a ProposalRef is a hash over the whole AuthenticatedContent, so one
// byte of authenticated_data makes the same removal a new entry -- there is no semantic identity in
// the key at all. Nothing but Rebind empties the cache and Rebind runs at an epoch boundary, so a
// member who simply never commits keeps the epoch open and this map growing without bound. And it is
// not only that member's memory. Pending answers EVERY entry, implementing section 12.4's "include
// all valid pending proposals" unconditionally, so an honest committer over a flooded cache emits a
// commit naming N references that every other member must already hold, and anyone who missed one of
// them answers errProposalNotCached and cannot apply it. One flooding peer degrades the whole group.
//
// WHAT THE BOUND IS. The cache exists to be committed from, so the ceiling is what ONE VALID COMMIT
// LIST could name. An entry that no valid list can carry alongside the entries already held is one
// no committer can use: admitting it buys the group nothing, costs it memory, and lengthens a commit
// no receiver can apply. Section 12.2 states the list rules and spec A section 3.1 states
// MaxGroupMembers, and the two together give a ceiling per accepted proposal type.

// proposalCacheCeiling is the three ceilings one accepted proposal type has: how many entries of it
// one valid commit list could name at all, how many of it that list could carry FROM ONE SENDER,
// and how many of the ones it carried from that sender could apply to ONE LEAF.
//
// MORE THAN ONE COLUMN, because a total alone is satisfied by one peer filling it. A cache capped
// only on the total converts an exhaustion into a STARVATION: the first sender to reach the total
// denies every honest member its own proposal for the rest of the epoch, which is the same
// availability failure one layer up and is strictly cheaper to mount than the memory one. The per
// sender column is what makes a flooder spend its own quota rather than the group's.
//
// AND THE THIRD COLUMN IS THAT ARGUMENT FINISHED. The per sender column counted (sender, type) with
// no regard to what the proposal applied to, so one sender's 500 removes ALL NAMING ONE LEAF sat
// inside its quota -- and section 12.2 invalidates a list carrying two proposals that apply to the
// same leaf, so Pending answered 500 references no valid commit list could carry. That is not
// waste, it is the availability failure again by another route: the flood makes every commit built
// from this cache invalid, an invalid commit never advances the epoch, and the epoch advance is the
// only thing that empties the cache. An entry that no valid list can carry alongside the entries
// already held is one no committer can use, which is this file's own stated rule, and the per
// target column is what makes the code count what that sentence says.
//
// perTarget is ZERO for a type that applies to no existing leaf -- an add makes a leaf rather than
// naming one -- and proposalAppliesToLeaf is what decides which is which, read off the ARM the
// proposal carries rather than off a list of type names.
type proposalCacheCeiling struct {
	perList   int
	perSender int
	perTarget int
}

// proposalTypeCacheCeiling is the disposition of every ACCEPTED proposal type, derived from RFC 9420
// section 12.2's list rules stated over spec A section 3.1's MaxGroupMembers.
//
// Add: section 12.2 invalidates a list carrying two Adds that represent the same client, and every
// add is a leaf, so a list cannot carry more adds than the membership cap admits -- MaxGroupMembers,
// from any number of senders and from one.
//
// Update and Remove: section 12.2 invalidates a list carrying "multiple Update and/or Remove
// proposals that apply to the same leaf". An Update applies to its OWN sender's leaf, so one sender
// contributes at most ONE committable update however many it publishes -- which is the whole of why
// an update flood is refused at the second one. A Remove applies to the leaf it names, so one sender
// may legitimately hold ONE PER MEMBER: one in the per target column, and the membership cap in the
// per sender column, which is what bounds how many distinct leaves it may name. Both halves are
// needed and neither implies the other -- a per target column alone leaves a sender naming leaf 0,
// leaf 1, leaf 2 and on without bound, because this cache holds no tree and cannot say which of
// those leaves exists, and a per sender column alone is the 500-removes-of-leaf-4 flood.
//
// GroupContextExtensions: section 12.2 invalidates a list carrying two of them at all, so it is one
// per list and therefore one per sender.
//
// The map is keyed by the registry's own constants and is held EQUAL to the ACCEPTED set of
// proposalTypeProfile in both directions by
// TestEveryProposalTypeTheV1ProfileAcceptsHasACeilingOfItsOwn -- the shape the bucket rule already
// takes, and rule 5's shape. A fifth accepted type with no ceiling beside it would be the one type
// this cache held without bound, and a ceiling for a type the profile no longer accepts is a row
// that outlived what it described.
var proposalTypeCacheCeiling = map[ProposalType]proposalCacheCeiling{
	ProposalTypeAdd:                    {perList: MaxGroupMembers, perSender: MaxGroupMembers, perTarget: 0},
	ProposalTypeUpdate:                 {perList: MaxGroupMembers, perSender: 1, perTarget: 1},
	ProposalTypeRemove:                 {perList: MaxGroupMembers, perSender: MaxGroupMembers, perTarget: 1},
	ProposalTypeGroupContextExtensions: {perList: 1, perSender: 1, perTarget: 0},
}

// proposalAppliesToLeaf answers the leaf RFC 9420 section 12.2's same-leaf rule reads one proposal
// as applying to, and whether it applies to an existing leaf at all.
//
// READ OFF THE ARM AND NOT OFF THE DISCRIMINANT, and the difference is that the arm is where the
// leaf actually is: a Remove carries the index it names, an Update carries the leaf node that
// replaces its own sender's. checkProposalProfile has already run checkArm by the time this is
// asked anything, so the two agree -- and reading the arm is what lets the gate over this rule
// DERIVE which accepted types apply to a leaf, by reflecting over the arm structures for a field
// that names a leaf or replaces one, rather than by trusting the switch below.
// TestEveryAcceptedTypeThatAppliesToALeafIsCountedAgainstThatLeaf is that gate: an eighth arm
// carrying a LeafIndex fails there rather than being counted against nothing here.
//
// An Add applies to NO existing leaf: section 12.1.1 places an added member at a blank one, so two
// adds from one sender are two members and not two proposals about one member. What bounds them is
// the membership cap, which is the per sender column.
func proposalAppliesToLeaf(sender LeafIndex, proposal *Proposal) (LeafIndex, bool) {
	switch {
	case proposal.Update != nil:
		return sender, true
	case proposal.Remove != nil:
		return proposal.Remove.Removed, true
	}
	return 0, false
}

// maxCachedProposals is the entry ceiling: the sum of the per list column, which is the largest
// number of proposals one valid commit list could name.
//
// SUMMED RATHER THAN WRITTEN DOWN, so the accepted set and the number cannot drift apart -- a
// written 1501 beside a table that grew a fifth row is a bound nobody updated.
//
// It is an OVER count and deliberately so. Section 12.2's rule is stated over one LEAF, so updates
// and removes share MaxGroupMembers between them rather than having it each, and the tight ceiling
// is one MaxGroupMembers lower than this sum. A ceiling that over counts refuses nothing an honest
// group can produce; one that under counts refuses an honest commit. Only the first of those two
// failures costs nothing, so the derivation is deliberately loose in that direction and says so.
func maxCachedProposals() int {
	total := 0
	for _, ceiling := range proposalTypeCacheCeiling {
		total += ceiling.perList
	}
	return total
}

// maxCachedProposalOctets is the second dimension, and it is the one number here that nothing
// derives.
//
// AN ENTRY CEILING ALONE IS SATISFIED BY ONE ENORMOUS PROPOSAL. syntax.MaxVectorLength caps a single
// field at 1 MiB and a group_context_extensions proposal is a vector of extensions each carrying an
// opaque body, so one accepted entry is worth about a mebibyte and maxCachedProposals of them are
// worth about a gibibyte and a half. The count and the octets are two resources and a bound on one
// is not a bound on the other.
//
// THE NUMBER IS ARGUED AND NOT DERIVED, and the argument is this. The largest set of proposals an
// honest group at the 500 member cap can publish in one epoch is about one add per new member
// carrying a key package each plus one update per member, which spec A section 5.11's own sizing
// puts in the low single digit megabytes; the largest single artefact that same section already asks
// a client to hold for one epoch is a commit's epoch bundle, at about 6.9 MB. Eight mebibytes is the
// next power of two over both, so an honest epoch does not reach it and a flood pays for every octet
// it spends. If a measurement ever shows an honest epoch exceeding it, this is the constant to
// raise -- and the SPEC is the place to state it.
const maxCachedProposalOctets = 8 << 20

// maxCachedProposalOctetsPerSender is the octet ceiling's per sender column, and it exists for the
// reason the counted ceiling has one: a bare total is a total ONE sender reaches.
//
// MEASURED, not argued. With the octets counted as a single number and no attribution, leaf 1
// reached 8,388,605 of the 8,388,608 -- headroom 3 -- out of 27 messages and 15 of its own 500
// entry add quota, because Store applies no key package validation and an add is therefore a half
// megabyte vehicle. Leaf 2, which had cached nothing at all, was then refused a six octet remove.
// That is verbatim the starvation the per sender ENTRY column was added to prevent, left open in
// the dimension where one message is worth half a mebibyte -- and strictly cheaper to mount, since
// it needs one sender where the entry route needs two.
//
// THE SHARE IS DERIVED FROM THE CEILING TABLE, and it is the same share of the octets that the
// entry column already grants one sender of the entries: one sender may hold sum(perSender) of the
// sum(perList) entries a valid commit list could name -- 1002 of 1501 as the table stands -- so it
// may spend that fraction of the octets. Deriving it rather than writing a number down is what
// keeps the two dimensions from stating different policies: a table edit that changed what one
// sender may hold in entries and left an octet constant behind would be a per sender rule in one
// dimension and a free-for-all in the other, which is the defect this closes.
//
// The division comes FIRST so the product cannot overflow a 32 bit int -- this package builds for
// android/arm and js/wasm -- at a cost of at most sum(perSender) octets of the share, which is a
// thousandth of a percent of it and in the refusing direction.
//
// WHAT IT DOES NOT CLAIM. It does not make this cache safe against a COALITION: two senders inside
// their own shares still reach the total, exactly as two senders inside their own entry quotas
// still reach the entry ceiling. That is the bound the entry column already draws, and a cache is
// not the place a sybil is answered.
func maxCachedProposalOctetsPerSender() int {
	perSender, perList := 0, 0
	for _, ceiling := range proposalTypeCacheCeiling {
		perSender += ceiling.perSender
		perList += ceiling.perList
	}
	if perList == 0 {
		return 0
	}
	return maxCachedProposalOctets / perList * perSender
}

// proposalCacheQuota names one sender's holding of one proposal type, which is the unit the per
// sender column is counted in.
//
// A struct key rather than a map of maps, because a read of a map that holds no such key answers
// zero and zero is exactly right for a sender that has cached nothing -- so there is no inner map to
// build before the first store and no second lookup to keep in step with the first.
type proposalCacheQuota struct {
	sender       LeafIndex
	proposalType ProposalType
}

// proposalCacheLeafQuota names one sender's holding of one proposal type THAT APPLIES TO ONE LEAF,
// which is the unit the per target column is counted in.
//
// The leaf is part of the key rather than a second map per sender, for proposalCacheQuota's reason.
// The SENDER is part of it because a rule counted over the whole cache would let one member's
// remove of leaf 4 refuse another member's -- cross member denial, which is the failure the per
// sender column exists to prevent, and section 12.2's list rule is the COMMITTER's to satisfy by
// choosing among what it was offered. And the TYPE is part of it because a sender's own update and
// its remove of somebody else are two different holdings that happen to be counted per leaf.
type proposalCacheLeafQuota struct {
	sender       LeafIndex
	proposalType ProposalType
	leaf         LeafIndex
}

// proposalCacheBinding is the group and epoch one cache belongs to.
//
// A struct behind a POINTER, so that "bound to nothing" is a state this type can be in and can be
// told apart from "bound to the empty group at epoch 0" -- which is not a hypothetical, because
// epoch 0 is where every group starts and a zero valued pair would answer for it. A cache with no
// binding refuses at every door rather than acquiring one later, and that is the whole of what
// makes the zero value safe -- and the zero value is the only unbound cache there is, because the
// constructor refuses to build one.
//
// It is a type of its own rather than two fields of the cache so that the write has ONE target: a
// reader asking where the binding comes from reads the two places this struct is built, and
// TestEveryWriteOfTheCacheBindingReadsTheCallersGroupContext derives that class off the source
// rather than off this sentence.
type proposalCacheBinding struct {
	// a COPY of the caller's group id and not the caller's own array. The cache outlives the
	// buffer a group context was decoded out of, and a binding aliased to that buffer follows
	// it when the caller reuses it -- so the one guard that would say this cache had gone
	// stale would agree with whatever the buffer now holds.
	groupId []byte
	epoch   uint64
}

// ProposalCache holds the proposals received in ONE epoch of ONE group, keyed by ProposalRef.
//
// THE KEY IS THE WHOLE REFERENCE, converted to a string. Not a prefix of it, and not a
// comparison: a map over the full octets answers exactly the entry whose reference the commit
// named, and there is no comparator in the lookup at all -- which is how this stays out of the
// class guardrail 8's derived gate reads, rather than by picking a comparator off a list. A key
// cut short is a proposal SUBSTITUTION and not a missed lookup: two references sharing the
// truncated prefix answer the same entry, so a commit naming one member's removal applies
// another member's, under a reference every peer checks and agrees with.
//
// THE BINDING IS WRITTEN OUT OF A *VerifiedGroupContext THE WRITING DECLARATION WAS HANDED. It
// is written in exactly two places -- NewProposalCache and Rebind -- and no method assigns it;
// TestEveryWriteOfTheCacheBindingReadsTheCallersGroupContext derives that class off the source and
// holds every member of it to reading a verified context parameter and nothing else.
//
// THE OTHER HALF -- where the CALLER got that value -- IS THE COMPILER'S NOW, and that is the
// whole of what changed here. It used to be a gate: an AST walk over every call of a binding
// writer, refusing an argument whose chain of selections reached a type this package decodes. It
// was written three times and bypassed three times, each time by ordinary Go a reader would write
// on purpose -- a local struct holding a copy of the decoded context, an accessor method returning
// an inner field, an embedded wire type whose promoted selection the walk could not see. The
// paragraph that used to be here told a reader to copy the context they had verified into their
// own state and bind from that, and the second bypass is that advice written out.
// group_context_verified.go carries the argument in full.
//
// So the demand is stated as a TYPE. A *VerifiedGroupContext is buildable only by
// (*KeySchedule).ConfirmGroupContext, whose own signature carries no group context at all: it
// answers the context THIS EPOCH'S SCHEDULE WAS DERIVED OVER, once a confirmation tag proves a
// holder of the epoch's confirmation key named it. A GroupInfo a joiner decoded cannot be handed
// to either writer, and neither can any laundering of one, because none of them is that type --
// they stop COMPILING rather than stopping a gate. What a caller still owes is the tag, and the
// tag is not something an attacker can supply for a context of its choosing: the confirmation key
// is one DeriveSecret off an epoch_secret expanded over the context itself, so a changed group id
// or epoch derives a different key and the tag it was handed stops verifying.
//
// WHAT THAT DOES NOT REACH is stated here rather than left for the next reviewer, because the
// gate it replaces overstated itself twice. A declaration inside package mls can still write the
// field of a VerifiedGroupContext, since the field is unexported rather than unreachable, so the
// class of declarations that BUILD one is derived off the source and held to a table by
// TestEveryConstructionOfAVerifiedGroupContextIsClassifiedHere. That is a different question from
// the one the deleted gate failed at: who constructs a named type is a property of source shape --
// a composite literal or a write of the field, and Go has no third spelling -- while where a value
// came from is not. And confirmation says nothing about whether the context is CORRECT: the tree
// hash, the transcript and the signer's entitlement are validations of their own.
//
// It was the other way round, and the other way round is a denial of service, which is why this
// paragraph is here. The binding used to be taken from the first entry STORED and checked against
// that same entry's own epoch -- self referential for the first one -- and released only by a
// Clear the lifecycle had to remember. So one replayed genuine proposal of a closed epoch,
// delivered to a cache that had just been cleared, bound the cache to the closed epoch: the
// member could then neither cache the live epoch's proposals nor resolve any commit that named
// one, and nothing released it, because the only release ran off a commit the member could no
// longer resolve. Integrity held -- the stale proposal was never applied -- and availability was
// gone for good, at the cost of replaying one message every peer had already seen. A guard cannot
// fix that, because the value the guard compares against is the attacker's; only taking the value
// from somewhere else can, and the group's own context is the only other place it exists.
//
// What is left is a discipline of OURS rather than a door a peer can push: a lifecycle that
// advances an epoch and does not Rebind leaves this cache refusing the new epoch. That is a bug
// in this build, it is loud at every door, and it is derived rather than trusted --
// TestEveryDeclarationThatMovesAGroupToAnotherEpochEndsTheProposalCacheBinding reads the class of
// declarations that move a group between epochs off the source and holds every one of them to
// ending the binding -- with the group context that same body moved to, at a point after it moved
// there. The third demand is the one a reader would not think to write down and is the one a
// caller gets wrong for free: calling the right ender, on the right cache, on every path, and
// handing it the epoch that just closed wedges this member exactly as the replay it replaced did,
// with no peer involved at all. That gate's mover class is EMPTY over this package today, so it
// demands nothing here yet and runs entirely on its own control -- said out loud there as well.
type ProposalCache struct {
	byRef map[string]CachedProposal
	order []string
	// the encoded octets of every entry held, so that maxCachedProposals is not a bound one
	// enormous proposal satisfies. Counted on insert and released whole at Rebind, never
	// decremented: nothing removes a single entry, so there is no path on which this can drift
	// from what byRef holds.
	octets int
	// the same octets ATTRIBUTED, which is what stops one sender spending the whole total and
	// starving every other member for the rest of the epoch. It is the octet dimension's half of
	// what perSender is for the counted one, and it is keyed by the SENDER ALONE rather than by
	// a sender and a type because the resource is this cache's memory and not any one proposal
	// type's: a sender's half megabyte add and its six octet remove are the same mebibytes.
	octetsPerSender map[LeafIndex]int
	// how many entries each sender holds of each accepted type, which is the column that stops
	// one peer spending the whole cache. Nil until the first store, for byRef's reason: a read
	// of a nil map answers zero, which is the right answer for a sender that has cached nothing.
	perSender map[proposalCacheQuota]int
	// how many entries each sender holds of each accepted type that apply to ONE LEAF, which is
	// what keeps what this cache holds to a set some valid commit list could name. See
	// proposalCacheCeiling for why a cache with only the two columns above holds 500 removes of
	// one leaf, and why that is an availability failure rather than waste.
	perLeaf map[proposalCacheLeafQuota]int
	// nil until the cache is bound. See proposalCacheBinding, and see bindingHolds for what
	// nil answers.
	binding *proposalCacheBinding
}

// NewProposalCache returns an empty cache bound to the group and epoch the CALLER is in.
//
// The context is a parameter rather than something the cache picks up later, because a cache that
// could be built without one has to learn its epoch from somewhere, and the only other thing on
// offer is whatever message arrives first. That is attacker supplied. Making the caller name the
// epoch is what turns "the cache checks that entries match" into "the cache knows which epoch it
// is in", and those are different claims: the first is true of a cache bound to a replay.
//
// A composite literal and not a field assignment, so that this constructor is not a second
// spelling of the write Rebind owns and a reader counting the places the binding is written
// counts two rather than three.
//
// IT TAKES A *VerifiedGroupContext AND NOT A *GroupContext, which is the whole of what says whose
// epoch this is. Every *GroupContext names some epoch and this package decodes one straight off
// peer octets, so the bare type is a claim about a struct's fields rather than about anybody's
// authority; the verified type is buildable only by (*KeySchedule).ConfirmGroupContext, and a
// decoded context cannot reach here in any spelling because it is the wrong type.
//
// IT REFUSES A CONTEXT THAT VOUCHES FOR NOTHING -- no value at all, or the zero value another
// package can spell -- rather than answering a cache bound to nothing. A cache bound to nothing
// is the one state in which the epoch has to come from somewhere else, and a constructor that
// answered one would hand that state to every caller that did not read an error. The zero value
// of the cache is still that state -- Go's convention for a container leaves it usable -- and it
// is safe for the reason bindingHolds gives: it refuses at every door.
func NewProposalCache(verified *VerifiedGroupContext) (*ProposalCache, error) {
	if verified == nil || verified.inner == nil {
		return nil, fmt.Errorf("%w: a cache is built bound to the epoch its group is in, and only a context this client has confirmed says which epoch that is",
			ErrNilGroupContext)
	}
	return &ProposalCache{
		byRef:           map[string]CachedProposal{},
		octetsPerSender: map[LeafIndex]int{},
		perSender:       map[proposalCacheQuota]int{},
		perLeaf:         map[proposalCacheLeafQuota]int{},
		binding: &proposalCacheBinding{
			groupId: bytes.Clone(verified.inner.GroupId),
			epoch:   verified.inner.Epoch,
		},
	}, nil
}

// Rebind empties the cache and binds it to the group and epoch the caller names. It is what an
// epoch boundary owes this cache.
//
// One method for the two halves of a boundary -- release the closed epoch's entries, and take the
// new epoch -- because a cache that could be released WITHOUT being rebound is a cache in the
// unbound state again, and the unbound state is where binding-from-a-message used to be possible.
// This replaces the Clear that used to do only the first half, and the difference is not
// bookkeeping: Clear left behind exactly the state a replay could bind.
//
// It answers a refusal rather than binding to nothing when it is handed no context, for the same
// reason: the one thing a rebind must not be able to do is leave the cache with no epoch and a
// door open. It takes a *VerifiedGroupContext for NewProposalCache's reason, and the two together
// are why no gate over the callers is needed any more -- a context decoded off a peer's octets is
// not that type, so it cannot be handed to either of them however it is spelled.
//
// THE CEILING ARITHMETIC IS RELEASED WITH THE ENTRIES, in the same statement list, because the two
// are one fact. A boundary that emptied byRef and kept the octet total or the per sender quotas
// would open the new epoch with the closed epoch's cache already full: every sender would arrive at
// its quota having cached nothing, which is the wedge this whole file was rewritten to remove,
// reintroduced through the accounting instead of through the binding.
//
// WHAT THE CALLER OWES IS THE CONTEXT IT MOVED TO, AND OWES IT AFTER MOVING THERE -- and this
// signature cannot check either half. Every *GroupContext names some epoch, so a boundary that
// hands over the one that is about to close is making a request this method cannot tell apart from
// the right one, and `self.proposals.Rebind(self.context); self.context = staged` is exactly that
// request: the cache is emptied and bound to the epoch the group is LEAVING. The member is then
// wedged for the whole of the new epoch -- Store answers errProposalCacheNotRebound, Pending
// answers nothing, Resolve answers errProposalResolvedOutOfEpoch for every reference a commit
// names -- and because it cannot resolve that commit it never reaches the next boundary, so this
// method never runs again. Nothing here heals it, which is why the demand is a GATE and not a
// sentence: TestEveryDeclarationThatMovesAGroupToAnotherEpochEndsTheProposalCacheBinding reads
// which context each boundary's rebind is handed, and on which side of the write, off the source.
// The type cannot answer that half and does not try: a verified context of the epoch that is
// CLOSING is exactly as verified as one of the epoch being entered, so which of the two a
// boundary hands over stays a question about the call and not about the value.
func (self *ProposalCache) Rebind(verified *VerifiedGroupContext) error {
	// a cache that does not exist cannot be given an epoch, and this is the one method of this
	// type that WRITES: bindingHolds answers false for a nil receiver and every reader is safe
	// behind it, while this body would dereference one. NewProposalCache is what builds a cache.
	if self == nil {
		return fmt.Errorf("%w: there is no cache here to bind, and a cache is built by NewProposalCache rather than by being rebound",
			errProposalCacheNotRebound)
	}
	if verified == nil || verified.inner == nil {
		return fmt.Errorf("%w: a cache takes its epoch from a context this client has confirmed and from nothing else",
			ErrNilGroupContext)
	}
	self.byRef = map[string]CachedProposal{}
	self.order = nil
	self.octets = 0
	self.octetsPerSender = map[LeafIndex]int{}
	self.perSender = map[proposalCacheQuota]int{}
	self.perLeaf = map[proposalCacheLeafQuota]int{}
	self.binding = &proposalCacheBinding{
		groupId: bytes.Clone(verified.inner.GroupId),
		epoch:   verified.inner.Epoch,
	}
	return nil
}

// cloneProposal answers a copy of a proposal that shares no storage with the one it was handed,
// through the one codec rather than field by field.
//
// A hand written deep copy is a second traversal of a structure that already has one. It agrees
// with the codec until an eighth arm is added, and the arm the copy had not learned about would
// be silently dropped -- into a cache whose entire job is to answer what the group agreed to.
// Round tripping through Marshal and Unmarshal also makes the copy exactly what the octets say,
// which is the same thing the ProposalRef is a hash over.
//
// The caller's array is the reason this exists at all. A proposal arrives inside a buffer the
// caller decoded and still owns; a cache entry cut from that buffer changes underneath the group
// with no error path anywhere, and the commit resolved from it is one no peer can reproduce.
//
// THE OCTET COUNT IS ANSWERED HERE rather than measured again by the caller, because the encode has
// already happened. A second syntax.Marshal to ask what an entry costs would be a second traversal
// of the structure this function exists to traverse once, and the two could disagree the moment an
// arm learned an encoding rule -- with the cache's octet accounting reading one number and its
// entries holding the other.
func cloneProposal(proposal *Proposal) (Proposal, int, error) {
	encoded, err := syntax.Marshal(proposal)
	if err != nil {
		return Proposal{}, 0, err
	}
	copied := Proposal{}
	if err := syntax.Unmarshal(encoded, &copied); err != nil {
		return Proposal{}, 0, err
	}
	return copied, len(encoded), nil
}

// proposalOctets is a proposal's wire encoding, which is the one thing two proposals can be
// compared BY.
//
// A PROPOSAL HAS NO OTHER IDENTITY ON THE WIRE. Its type is a discriminant, its arms are pointers
// into whatever buffer decoded them, and a Proposal carrying an Add carries a whole KeyPackage
// below it -- so == is not defined over it, a field-by-field comparison is a hand written list
// that goes stale the moment an eighth arm is registered, and both of those are the enumeration
// rule 5 exists for. The encoding is the value the sender signed, the value the transcript hash
// covers and the value a ProposalRef is a hash OF, so two proposals encoding alike are one
// proposal to every RECEIVER of this protocol.
//
// IT IS NOT AN IDENTITY FOR A Proposal VALUE IN PROCESS, and the sentence that used to stand here
// said it was -- "two proposals encoding alike are one proposal by every measure this protocol
// has". MarshalMLS writes UnknownType as the discriminant when one is set and selects the arm by
// ProposalType, so {ProposalType: remove, Remove: {Removed: 0x03bbccdd}} and
// {ProposalType: external_init, UnknownType: remove, ExternalInit: {KemOutput: bb cc dd}} encode
// to the same 000303bbccdd. That is measured rather than argued, in
// TestTheJoinRefusesTwoProposalsThatEncodeAlikeUnderDifferentTypes.
//
// THREE THINGS KEEP THAT PAIR OFF THE WIRE and none of them is visible from a caller comparing two
// Proposal values by these octets: UnmarshalMLS normalises the two fields, NewProposalList clones
// through the codec, and checkProposalProfile's rule 2 above refuses the disagreement outright. A
// caller that wants the TYPE compared has to compare it -- which is what the commit door's join
// does, by writing the ProposalType in front of these octets rather than reading it out of them.
//
// ONE LINE, AND IT IS HERE RATHER THAN AT THE ONE CALL SITE, for cloneProposal's reason directly
// above: the codec is what makes a copy exactly what the octets say, and it is what makes a
// comparison exactly what the octets say. A second door that has to compare two proposals should
// find the answer beside the clone rather than reach for syntax.Marshal and pick its own spelling.
func proposalOctets(proposal *Proposal) ([]byte, error) {
	return syntax.Marshal(proposal)
}

// bindingHolds answers whether the group and epoch this cache is bound to are the ones named.
//
// ONE comparison for the five rules that ask the question, because five copies of it are five
// places the wrong field can be compared and a reader mutating one would find the others still
// right. CheckEpoch, Store, Cached, Pending and Resolve all ask it; what they share is the question
// and not the answer, and each keeps its own refusal.
//
// A NIL CACHE ANSWERS FALSE, which is holds()'s doctrine one method over and is what lets every
// caller of this be written without a receiver guard of its own. Three exported methods -- Cached,
// Pending and CheckEpoch -- reach this and nothing else on a nil receiver, and
// CommitValidationInput.Pending is legitimately nil, so a guard on the binding alone made those
// three take the caller's process rather than refuse. A cache that does not exist belongs to no
// epoch, which is the same answer an unbound one gives and the fail-closed one for all three.
//
// AN UNBOUND CACHE ANSWERS FALSE, and there is no emptiness clause in front of it. An empty cache
// is not a cache that belongs to no epoch -- it belongs to the epoch it was bound to, and asking
// it about another one is the case CheckEpoch exists for: an epoch that advanced with no proposal
// arriving in the new one. The clause that used to short circuit on an empty map is exactly what
// made a freshly emptied cache answer nil for whatever epoch it was next asked about, and that is
// the door the binding used to walk in through.
//
// crypto/subtle and not == over a string conversion. That is framing_protect.go's
// CheckFramedContentContext one type up, comparing the same field for the same reason: this
// package holds no comparison of octets spelled as ordinary go equality at all, and
// TestFramingUsesConstantTimeComparison reads that off the type checked source rather than off a
// list of comparator names -- so a string conversion is not a way around it, it is the exact
// shape that gate was built after somebody tried.
//
// Both halves, and never the epoch alone: every group this client is a member of runs an epoch 7,
// so an epoch number is not an identity. And never the group alone: that is the whole of the
// replay this file exists to refuse.
func (self *ProposalCache) bindingHolds(groupId []byte, epoch uint64) bool {
	if self == nil || self.binding == nil {
		return false
	}
	return subtle.ConstantTimeCompare(self.binding.groupId, groupId) == 1 && self.binding.epoch == epoch
}

// bindingName names what this cache belongs to, for a refusal a reader can act on.
//
// A cache bound to nothing says so rather than reporting "epoch 0 of group ", which is a sentence
// a reader would spend an afternoon looking for a group id in. A cache that does not exist says the
// same thing: this is reached from CheckEpoch's own refusal, which a nil receiver gets to.
func (self *ProposalCache) bindingName() string {
	if self == nil || self.binding == nil {
		return "no group"
	}
	return fmt.Sprintf("epoch %d of group %x", self.binding.epoch, self.binding.groupId)
}

// checkCacheCeiling refuses a NEW entry that would put this epoch's cache past what one valid commit
// list could name. See proposalTypeCacheCeiling for where the two numbers come from.
//
// IT IS ASKED ONLY OF AN ENTRY THE CACHE DOES NOT ALREADY HOLD, and that is a rule and not an
// optimisation. The key is a hash over the whole message, so a reference already cached is the same
// octets and costs nothing more; a cache that refused a re-delivery at its ceiling would answer a
// sentence about a limit to a caller holding a message this cache already agreed to, and the caller
// would go looking for a flood that is its own duplicate.
//
// THE THREE REFUSALS ARE THREE RULES WITH THREE REMEDIES, which is this file's rule for every pair
// it separates. errProposalCacheFull is about the epoch and the remedy is for somebody to commit;
// errProposalCacheSenderQuota is about one member holding too much of a type and the remedy is to
// stop trusting it; errProposalCacheTargetQuota is about one member holding two proposals about ONE
// LEAF, which is a pair section 12.2 lets no list carry, so the remedy is the same but the sentence
// is not -- a member at that refusal has published nothing excessive in total. A caller told only
// "the cache said no" cannot tell a busy group from a flooding peer from a peer whose second
// proposal simply cannot be committed beside its first.
//
// THE PER TARGET RULE IS ASKED LAST OF THE THREE and only of a proposal that applies to a leaf,
// because it is the narrowest: a sender at its per type quota is over a bound that holds whatever
// the proposals were about, and answering the narrow rule first would report a leaf to a caller
// whose member is flooding every leaf there is.
//
// The type's ceiling is looked up rather than defaulted, and an accepted type with no row is refused
// under a value of its own, for the reason Resolve's bucketless branch is refused rather than
// dropped: the commit that widens the accepted set is what makes that branch reachable, and a fifth
// accepted type admitted with no ceiling would be the one type this cache held without bound. The
// same value answers a row that has a per sender column and no per target one under a type that
// APPLIES to a leaf, because that is the same commit stopping half way one column further in --
// and, like the bucketless branch, it is unreachable in this build:
// TestEveryAcceptedTypeThatAppliesToALeafIsCountedAgainstThatLeaf fails on that commit before this
// line can run.
func (self *ProposalCache) checkCacheCeiling(sender LeafIndex, proposal *Proposal) error {
	proposalType := proposal.ProposalType
	ceiling, accepted := proposalTypeCacheCeiling[proposalType]
	if !accepted {
		return fmt.Errorf("%w: %s has no ceiling", errAcceptedTypeHasNoCeiling,
			proposalTypeName(proposalType))
	}
	if len(self.byRef) >= maxCachedProposals() {
		return fmt.Errorf("%w: %s already holds %d and one valid commit list names at most %d",
			errProposalCacheFull, self.bindingName(), len(self.byRef), maxCachedProposals())
	}
	if held := self.perSender[proposalCacheQuota{sender: sender, proposalType: proposalType}]; held >= ceiling.perSender {
		return fmt.Errorf("%w: leaf %d holds %d %s proposals in %s and one valid commit list carries at most %d of that type from one sender",
			errProposalCacheSenderQuota, sender, held, proposalTypeName(proposalType),
			self.bindingName(), ceiling.perSender)
	}
	leaf, targeted := proposalAppliesToLeaf(sender, proposal)
	if !targeted {
		return nil
	}
	if ceiling.perTarget < 1 {
		return fmt.Errorf("%w: %s applies to leaf %d and its ceiling states none per leaf",
			errAcceptedTypeHasNoCeiling, proposalTypeName(proposalType), leaf)
	}
	if held := self.perLeaf[proposalCacheLeafQuota{
		sender: sender, proposalType: proposalType, leaf: leaf,
	}]; held >= ceiling.perTarget {
		return fmt.Errorf("%w: leaf %d holds %d %s proposals in %s that apply to leaf %d and one valid commit list carries at most %d of them",
			errProposalCacheTargetQuota, sender, held, proposalTypeName(proposalType),
			self.bindingName(), leaf, ceiling.perTarget)
	}
	return nil
}

// checkOctetCeiling refuses a NEW entry that would put this epoch's cache, or the sender's own share
// of it, past the octets it may hold. See maxCachedProposalOctets for where the total comes from and
// maxCachedProposalOctetsPerSender for why the total alone is not a bound.
//
// It is asked of an entry the cache does not already hold, for checkCacheCeiling's reason, and it is
// asked AFTER the copy because what an entry costs is what it encodes to and the encode is the copy.
//
// The two clauses are in the counted pair's order -- the group's rule before the sender's -- so that
// a caller reading two refusals of this file learns them in one order. Which of the two fires is not
// arbitrary either way: a total reached with every sender inside its share is a group that has to
// commit, and a share reached under an unfilled total is one member.
func (self *ProposalCache) checkOctetCeiling(sender LeafIndex, octets int) error {
	if self.octets+octets > maxCachedProposalOctets {
		return fmt.Errorf("%w: %s holds %d octets and this proposal's %d would carry it past the %d one epoch's cache may hold",
			errProposalCacheOctets, self.bindingName(), self.octets, octets, maxCachedProposalOctets)
	}
	if held := self.octetsPerSender[sender]; held+octets > maxCachedProposalOctetsPerSender() {
		return fmt.Errorf("%w: leaf %d holds %d octets in %s and this proposal's %d would carry it past the %d one sender may spend",
			errProposalCacheSenderOctets, sender, held, self.bindingName(), octets,
			maxCachedProposalOctetsPerSender())
	}
	return nil
}

// CheckEpoch refuses when this cache is bound to an epoch other than the one the caller is acting
// in.
//
// It is the boundary question asked without a message in hand: a lifecycle that has advanced an
// epoch and wants to know whether the cache came with it. That question is the one this file was
// rewritten around -- while the binding came from the first entry stored, an emptied cache
// answered nil for every epoch anybody asked about, so the guard could not see the case its own
// comment named.
//
// It is not the only thing standing between a closed epoch's proposals and a commit of the new
// one. Resolve asks the same question of its own group context, under
// errProposalResolvedOutOfEpoch, which is what makes the refusal a property of the RESOLUTION
// rather than a discipline expected of a caller -- see the note there. Both remain, because they
// answer for different callers: this one lets a lifecycle notice the boundary before it has
// anything to resolve, and the one in Resolve is the door no commit gets past.
func (self *ProposalCache) CheckEpoch(groupContext *GroupContext) error {
	if groupContext == nil {
		return fmt.Errorf("%w: the epoch a cache is checked against is the group's own",
			ErrNilGroupContext)
	}
	if !self.bindingHolds(groupContext.GroupId, groupContext.Epoch) {
		return fmt.Errorf("%w: the cache holds %s and the caller is acting in epoch %d of group %x",
			errProposalCacheNotRebound, self.bindingName(), groupContext.Epoch, groupContext.GroupId)
	}
	return nil
}

// checkResolveEpoch refuses a commit that names a cached reference while this cache belongs to
// another epoch.
//
// This is the replay door, and it is Resolve's own rule rather than CheckEpoch's. A ProposalRef
// is a hash over an AuthenticatedContent carrying the group id and the epoch, so an entry cached
// in epoch N is a name no commit of epoch N+1 can legitimately carry -- and a cache nobody
// rebound at the boundary answers exactly those names, which applies a proposal the group has
// already applied under a reference every peer still verifies.
//
// It is asked at the lookup rather than over the whole call, so a commit that carries every
// proposal INLINE is not refused for the state of a cache it never reads. What makes a reference
// a replay is that it names an entry, and this is the statement in front of the only line that
// can answer one.
func (self *ProposalCache) checkResolveEpoch(groupContext *GroupContext) error {
	if !self.bindingHolds(groupContext.GroupId, groupContext.Epoch) {
		return fmt.Errorf("%w: the cache holds %s and this commit is of epoch %d of group %x",
			errProposalResolvedOutOfEpoch, self.bindingName(),
			groupContext.Epoch, groupContext.GroupId)
	}
	return nil
}

// Store caches a proposal received in the caller's epoch and answers its reference.
//
// The order of the refusals is the order a caller wants to hear them, and it is asserted rather
// than assumed. The PROVIDER first, because the reference is a hash and the hash is the
// provider's, so a body that judged the message first would answer "this is not a proposal" to a
// caller whose actual mistake was passing no provider. Then the group CONTEXT, because it is the
// other thing the caller was asked for and it decides which epoch every rule below runs in. Then
// the message itself, then the SENDER, then the v1 profile. Then the two epoch rules, OURS first
// -- and the reference is computed after all seven, because it is the only step that costs a hash
// and there is nothing to hash in a message seven rules have already refused.
//
// THE CEILINGS COME AFTER THE REFERENCE AND NOT BEFORE IT, which is the one place in this body where
// a cheap rule follows an expensive one. A ceiling is asked only of an entry the cache does not
// already hold, and the only thing that says whether it holds one is the reference: the key is a
// hash over the whole message, so the same message is the same key. Asking first would refuse a
// re-delivery of something this cache already agreed to. The hash is therefore what a flooder still
// spends per message, and it is one hash over a body the caller had already decoded and framed.
//
// The COUNTED ceilings run before the copy and the OCTET ones after it, because what an entry costs
// is what it encodes to and the encode is the copy. See proposalTypeCacheCeiling for why there
// are ceilings at all: nothing but Rebind empties this cache, Rebind runs at an epoch boundary, and
// a member who never commits therefore keeps the epoch open and this map growing -- measured at
// 20,000 well formed proposals of one epoch from one sender, every one accepted, and answered by
// Pending as 20,000 references an honest committer would put in a commit nobody else can apply.
//
// THE TWO EPOCH RULES ARE TWO RULES WITH TWO REMEDIES, and they are in that order because a stale
// cache makes every entry in it wrong while a mislabelled message is one message. CheckEpoch asks
// whether THIS CACHE came with the group -- a no means the lifecycle advanced an epoch and did
// not rebind, which is a bug in this build. The clause after it asks whether the MESSAGE belongs
// to the epoch the group is in -- a no means a peer sent, or somebody replayed, a proposal of
// another epoch, and the remedy is to drop it.
//
// EACH OF THEM IS NECESSARY AND THAT IS MEASURED rather than argued. Delete the CheckEpoch and a
// cache still bound to a closed epoch accepts that epoch's proposals while its caller is acting in
// the new one, which TestCheckEpochAnswersTheBindingAndRebindMovesIt catches. Delete the clause
// after it and a proposal stamped with another epoch is cached in this one, which the
// (*ProposalCache).Store row's probe in epoch_advance_test.go catches under errProposalCacheEpoch.
// Neither refusal is reachable through the other.
//
// WHAT IS NOT INDEPENDENT IS THE VALUE THE SECOND RULE IS MEASURED AGAINST, and saying so is worth
// more than a sentence claiming otherwise. By the time it runs, CheckEpoch has established that
// this cache's binding and the caller's group context are the same group and the same epoch -- so
// bindingHolds(message) and a direct comparison of the message against groupContext are the SAME
// TEST, and rewriting the clause as the second spelling leaves the whole of ./mls/... and
// ./message/... green. That was measured, and it is not a hole: the two are equivalent, and a
// mutation between equivalent programs is one no test can be asked to catch. The binding is the
// subject written here because it is what the refusal prints and what the sentinel names, not
// because the two could ever disagree.
//
// What the second rule does NOT do is compare the message against itself. The pair it is measured
// against was taken from the caller's group context at construction or at the last rebind, so a
// replayed proposal of a closed epoch is refused here and changes nothing -- not the entries, and
// above all not the binding, which this method does not write at all.
//
// The sender rule is this cache's own and is not ValSem112. A CachedProposal carries a LeafIndex,
// and a sender that is not a member has none: Sender{SenderType: SenderTypeNewMemberProposal}
// leaves the zero value in LeafIndex, so caching it would attribute somebody's external proposal
// to leaf 0 -- a real member, whose own Update would then read as the committer's under
// ValSem111. The v1 profile refuses external commits, so this build has no legitimate non member
// proposal to cache; a build that grew one would need a CachedProposal that can say "no leaf"
// rather than a wider gate here.
//
// The compiler directive is the package's convention for a member of the class
// TestEveryEraseHelperCarriesTheNoInlineDirective derives, and not a claim that this method
// erases a secret. That class is "writes through storage that outlives the call", read off the
// source; the insert below indexes into the receiver's own map, which is the shape, and tree.go's
// setNode carries the directive under the same reading and says so. Store is held to it because
// the derivation puts it there -- which is the point of deriving a class rather than listing one.
//
//go:noinline
func (self *ProposalCache) Store(crypto CryptoProvider, groupContext *GroupContext,
	content *AuthenticatedContent) (ProposalRef, error) {

	if crypto == nil {
		return nil, fmt.Errorf("%w: the reference an entry is keyed by is a hash and the hash is the provider's", ErrNilCryptoProvider)
	}
	if groupContext == nil {
		return nil, fmt.Errorf("%w: a store is refused unless it can name the epoch it is storing in", ErrNilGroupContext)
	}
	if content == nil {
		return nil, fmt.Errorf("%w: there is nothing to cache", errNilAuthenticatedContent)
	}
	if content.Content.ContentType != ContentTypeProposal || content.Content.Proposal == nil {
		return nil, fmt.Errorf("%w: content type %d", errProposalCacheNotAProposal, content.Content.ContentType)
	}
	if content.Content.Sender.SenderType != SenderTypeMember {
		return nil, fmt.Errorf("%w: sender type %d", errProposalSenderNotMember, content.Content.Sender.SenderType)
	}
	if err := checkProposalProfile(defaultProfile(), content.Content.Proposal); err != nil {
		return nil, err
	}
	// ours before theirs: a cache that did not come with the group is wrong about every entry
	// it holds, and a caller told only that one message was stale would go looking at the
	// message.
	if err := self.CheckEpoch(groupContext); err != nil {
		return nil, err
	}
	if !self.bindingHolds(content.Content.GroupId, content.Content.Epoch) {
		return nil, fmt.Errorf("%w: the proposal is of epoch %d of group %x and the cache holds %s",
			errProposalCacheEpoch, content.Content.Epoch, content.Content.GroupId, self.bindingName())
	}
	ref, err := content.ProposalRef(crypto)
	if err != nil {
		return nil, err
	}
	key := string(ref)
	_, held := self.byRef[key]
	if !held {
		if err := self.checkCacheCeiling(content.Content.Sender.LeafIndex,
			content.Content.Proposal); err != nil {
			return nil, err
		}
	}
	proposal, octets, err := cloneProposal(content.Content.Proposal)
	if err != nil {
		return nil, err
	}
	// the octet ceilings over the totals this entry would make them, and not over the entry
	// alone: a per proposal limit says nothing about how many of them there are, and the
	// resource being bounded is what this cache HOLDS for the length of an epoch.
	if !held {
		if err := self.checkOctetCeiling(content.Content.Sender.LeafIndex, octets); err != nil {
			return nil, err
		}
	}
	// NO LAZY MAP HERE, and its absence is the thing to read. A store reaches this line only
	// past CheckEpoch, which refuses every cache whose binding is nil, and the only two
	// declarations that write a binding -- NewProposalCache and Rebind -- build both maps in
	// the same statement list as the binding. So a BOUND cache always has somewhere to store,
	// and a guard for the nil case is a branch nothing can take.
	//
	// It stood here anyway, and that was measured rather than argued: deleting
	// `byRef: map[string]CachedProposal{}` from NewProposalCache's composite literal left the
	// whole of ./mls/... and ./message/... green, because the unreachable guard was quietly
	// doing the constructor's job. Two initialisations, each covering for the other, are two
	// lines neither of which any test can observe. One initialisation, at the two places that
	// decide which epoch this cache belongs to, is a line every store observes.
	// the order, the octets and the sender's quota move together and only for an entry that is
	// new, because they describe the same set: an entry counted twice is a ceiling reached by
	// re-delivering one message, and an entry counted never is a ceiling nothing reaches.
	if !held {
		self.order = append(self.order, key)
		self.octets += octets
		self.octetsPerSender[content.Content.Sender.LeafIndex] += octets
		self.perSender[proposalCacheQuota{
			sender:       content.Content.Sender.LeafIndex,
			proposalType: content.Content.Proposal.ProposalType,
		}] += 1
		// the per leaf count moves with the rest and only for a proposal that applies to a
		// leaf, which is the same question checkCacheCeiling asked two statements up and is
		// asked again rather than carried down, because a bool threaded through the body
		// would be a second place the two could disagree.
		if leaf, targeted := proposalAppliesToLeaf(content.Content.Sender.LeafIndex,
			content.Content.Proposal); targeted {

			self.perLeaf[proposalCacheLeafQuota{
				sender:       content.Content.Sender.LeafIndex,
				proposalType: content.Content.Proposal.ProposalType,
				leaf:         leaf,
			}] += 1
		}
	}
	// the entry's own Ref is cut from the KEY rather than from the answer, so the reference
	// this cache holds and the reference the caller walks away with share no storage. Storing
	// the caller's array back would let a caller that wrote into what Store returned change the
	// name of an entry it no longer owns.
	self.byRef[key] = CachedProposal{
		Ref:      ProposalRef(key),
		Proposal: proposal,
		Sender:   content.Content.Sender.LeafIndex,
		ByValue:  false,
	}
	return ref, nil
}

// Cached looks up a proposal of the caller's epoch by its whole reference.
//
// NOT SPELLED Get, AND THE NAME IS LOAD BEARING. tree.go declares (*RatchetTree).Get, and
// guardrail 8's reachability walk over the ratchet tree's key questions -- tree_test.go's
// TestEveryKeyQuestionOverTheRatchetTreeIsAnsweredInConstantTime -- is keyed by the bare
// declaration NAME, because it reads parsed source with no type information in it. Two methods
// called Get are one node in that graph. So a lookup here spelled Get would put this body's
// constant time comparison inside the call graph of every tree method that reads a node, which
// makes an excused member of that class read as reaching the sanctioned comparison -- measured,
// that is exactly what happened -- and, worse, makes the POSITIVE half of that rule satisfiable by
// any key question that happens to call something named Get. The rename keeps that gate as precise
// as it was rather than working around it.
//
// THE GROUP CONTEXT IS NOT DECORATION HERE. p7's CheckErrata8815 reads this and reports "proposal
// reference %x is not cached for this epoch" on a miss, and a Get that consulted only the map
// could not make that claim: it would answer an entry of a closed epoch to a caller that had
// asked about the live one. The ordering that saves it today -- Resolve running first at both
// call sites -- is a property of two call sites and not of this method, and those are different
// claims.
//
// A missing entry and an entry of another epoch are ONE answer, because to the caller they are
// one fact: nothing of your epoch is cached under that reference. A caller that needs the two
// apart is resolving a commit, and Resolve separates them under two sentinels of its own.
//
// The value it answers is the cache's own and is not copied: this has no error to report a copy
// failing with, and a copy on every lookup would be a marshal per resolved reference. A caller
// that writes into what it gets back corrupts the entry. Resolve, which is what the commit path
// actually uses, copies.
func (self *ProposalCache) Cached(groupContext *GroupContext, ref ProposalRef) (CachedProposal, bool) {
	if groupContext == nil {
		return CachedProposal{}, false
	}
	if !self.bindingHolds(groupContext.GroupId, groupContext.Epoch) {
		return CachedProposal{}, false
	}
	cached, ok := self.byRef[string(ref)]
	return cached, ok
}

// holds answers whether this cache carries an entry under that reference.
//
// The question erratum 8815 asks -- "was this proposal previously received by the group member" --
// and nothing else. It is separate from Cached because it answers it WITHOUT the epoch binding:
// CheckErrata8815 judges a vector rather than resolving one, and the binding is CheckEpoch's rule
// with CheckEpoch's own sentinel, so re-deciding it here would give one condition two names.
//
// A nil cache holds nothing, which is what makes CheckErrata8815 fail closed under one rather than
// having to guard for it.
func (self *ProposalCache) holds(ref ProposalRef) bool {
	if self == nil {
		return false
	}
	_, cached := self.byRef[string(ref)]
	return cached
}

// Resolve turns a commit's ProposalOrRef vector into one bucketed list, so that validation and
// application have exactly one path whether the commit carried a proposal inline or by name.
//
// Five rules that are the cache's rather than validation's, in the order they are applied:
//
//  1. every entry goes through ProposalOrRef.checkArm first, which is proposal_wire.go's and is
//     not restated here. It refuses a discriminant outside the two the registry defines, an entry
//     carrying both arms, and a reference of no octets -- the last of which would otherwise be a
//     lookup on the empty key that every commit making the same mistake shares.
//  2. a by-value proposal is attributed to the COMMITTER and a by-reference one keeps the sender
//     that cached it. That is the whole of what makes ValSem111 checkable: a committer whose own
//     Update could be read as somebody else's would be covering the one update RFC 9420
//     section 12.4 says its UpdatePath replaces.
//  3. a reference named TWICE is refused. RFC 9420 section 12.2 invalidates every list a
//     duplicate could produce -- two removes of one leaf, two adds of one client, two
//     GroupContextExtensions -- but those are task 7's rules over a list, and this one is about
//     identity: a reference IS a name, and one name resolving to two entries of one list means a
//     member's single published proposal is applied twice.
//  4. a reference into a cache bound to ANOTHER epoch is refused, under this cache's own
//     errProposalResolvedOutOfEpoch and not under Store's errProposalCacheEpoch -- see
//     checkResolveEpoch, and see the note beside the values for why the epoch rules are three.
//  5. a second GroupContextExtensions proposal is refused, which is what makes
//     ProposalList.Extensions exact rather than a silent choice between two extension sets.
//
// THE GROUP CONTEXT IS WHY RULE 4 CAN BE STATED AT ALL, and it is what this signature was
// changed to carry. The shape the group lifecycle plan first pinned took a committer and a
// vector and nothing that names an epoch, so this body could not ask which epoch it was
// resolving in -- and a proposal cached in epoch N therefore resolved in epoch N+1
// unconditionally, which is the one direction a cache must not fail. A group context is what
// every caller of this method already holds -- Commit and the inbound commit path both resolve
// against self.context -- so asking for it costs the callers nothing and moves the refusal from a
// discipline to a door.
//
// It is the same object the binding itself comes from, which is the second half of the fix and
// the more important one. Rule 4 was landed first, over a binding still taken from the first
// entry stored; that made a stale entry unusable but left the binding attacker supplied, so a
// replay of one genuine old proposal into a freshly emptied cache locked the member out of its
// own live epoch. A door is worth what the value behind it is worth.
//
// The provider is not reached. It is in the signature because the group lifecycle plan pins it
// there and every caller already holds one; the refusal below is what stops the parameter being a
// promise the body does not keep. The group context is reached, at rule 4, which is the
// difference between the two parameters and the reason only one of them needs that excuse.
func (self *ProposalCache) Resolve(crypto CryptoProvider, groupContext *GroupContext,
	committer LeafIndex, refs []ProposalOrRef) (*ProposalList, error) {
	if crypto == nil {
		return nil, fmt.Errorf("%w: resolution runs under the caller's provider", ErrNilCryptoProvider)
	}
	// before the vector is walked, because a resolution with no epoch to observe is the defect
	// this parameter was added to close and answering it per entry would make an empty commit
	// the one call that still has none.
	if groupContext == nil {
		return nil, fmt.Errorf("%w: resolution is refused unless it can name the epoch it runs in", ErrNilGroupContext)
	}
	list := &ProposalList{}
	viewed := proposalListViewedTypes()
	namedAt := map[string]int{}
	for i := range refs {
		entry := refs[i]
		if err := entry.checkArm(); err != nil {
			return nil, fmt.Errorf("%w: at proposal_or_ref %d", err, i)
		}
		// the source proposal, the sender it is attributed to and the name it was resolved
		// under, decided before either is judged so that the profile gate below runs once
		// over both arms rather than once per branch.
		var source *Proposal
		var sender LeafIndex
		var name ProposalRef
		byValue := entry.Type == ProposalOrRefTypeProposal
		if byValue {
			// checkArm has already refused every type that is not one of the two, and
			// refused a nil Proposal arm under this one, so there is no third branch here
			// and no dereference to guard.
			source = entry.Proposal
			sender = committer
		} else {
			// the epoch BEFORE the lookup and before the duplicate bookkeeping. A cache
			// belonging to a closed epoch still answers every key it holds, so the lookup
			// below succeeds on a replayed reference and reports nothing; and a stale cache
			// makes every reference of this vector wrong, which is a larger fault than one
			// of them being named twice.
			if err := self.checkResolveEpoch(groupContext); err != nil {
				return nil, fmt.Errorf("%w: at proposal_or_ref %d", err, i)
			}
			key := string(entry.Reference)
			if at, twice := namedAt[key]; twice {
				return nil, fmt.Errorf("%w: entries %d and %d both name %x",
					errDuplicateProposalReference, at, i, entry.Reference)
			}
			namedAt[key] = i
			found, ok := self.byRef[key]
			if !ok {
				return nil, fmt.Errorf("%w: %x", errProposalNotCached, entry.Reference)
			}
			// a copy of the map's value, so the pointer below is to this loop's own
			// header rather than to the cache's; what it POINTS AT is still the cache's,
			// which is what the clone two statements down is for.
			source = &found.Proposal
			sender = found.Sender
			name = ProposalRef(key)
		}
		// the profile BEFORE the copy, and that order is load bearing rather than tidy. The
		// copy runs through the codec, and the codec refuses the arms of the three types v1
		// does not implement for reasons of its own -- a psk with no usage, a reinit with no
		// group id -- so a copy taken first answers whichever encoding rule the arm happened
		// to break instead of naming the profile rule that actually refused it. A by-value
		// proposal has been through no gate at all before this line, and an entry cached
		// under an earlier profile is judged here under the one running now.
		if err := checkProposalProfile(defaultProfile(), source); err != nil {
			return nil, err
		}
		// the octet count is the CACHE's accounting and a resolution retains nothing,
		// so it is dropped here rather than carried into a ProposalList field nothing
		// would read. What bounds a resolution is the vector its caller was handed.
		proposal, _, err := cloneProposal(source)
		if err != nil {
			return nil, err
		}
		cached := CachedProposal{Ref: name, Proposal: proposal, Sender: sender, ByValue: byValue}
		// AND THE TYPE HAS A NAMED VIEW OF ITS OWN, asked before the entry joins the order.
		//
		// UNREACHABLE TODAY, and this is not the branch's excuse for existing. Every value
		// reaching here has been through checkProposalProfile, which refuses every type
		// proposalTypeProfile does not classify as accepted, and proposalListViewedTypes is
		// exactly the four it does accept -- read off proposalBucketsOf rather than listed
		// here, so the two cannot drift apart.
		//
		// It stays, and it REFUSES rather than dropping, because the commit that widens the
		// accepted set is what makes it reachable: a fifth accepted type with no view beside
		// it would sit in the commit order, be read by no rule stated over a view, be applied
		// by nothing, and be named by no error -- a proposal the group agreed to and no member
		// acted on. Deriving the views closed the DISAGREEMENT between a view and the order;
		// it does not close a type the order carries and no view answers.
		// TestEveryProposalTypeTheV1ProfileAcceptsLandsInAViewOfItsOwn fails on that commit
		// before this line ever runs, and
		// TestABucketlessAcceptedTypeIsRefusedRatherThanSilentlyDropped performs the widening
		// for the length of one test so that this line is executed rather than reasoned about.
		if !viewed[cached.Proposal.ProposalType] {
			return nil, fmt.Errorf("%w: %s has no bucket", errAcceptedTypeHasNoBucket,
				proposalTypeName(cached.Proposal.ProposalType))
		}
		list.order = append(list.order, cached)
	}
	// and section 12.2's one-GroupContextExtensions rule over the order this walk built.
	//
	// AFTER THE LOOP AND THROUGH THE RULE'S OWN BODY, which is two decisions. It is
	// checkOneGroupContextExtensions and not a second spelling, because two bodies stating one
	// rule is how the two come to disagree -- the shape this file's own header argues for
	// everywhere else. And it is outside the walk because a view is FILTERED at every read: a
	// call inside the loop is a sweep of the order per entry, which is quadratic in what a peer
	// can send, and TestNoReaderOfAPerTypeViewFiltersItInsideItsOwnLoop holds every reader of a
	// view to that.
	//
	// What it costs is the index: the refusal no longer names the proposal_or_ref that broke the
	// rule. Nothing asserted that index, section 12.2 states the rule over the LIST rather than
	// over an entry, and a caller told "this commit carries two" is told the thing it can act on.
	if err := checkOneGroupContextExtensions(list); err != nil {
		return nil, err
	}
	return list, nil
}

// Pending answers every proposal cached in the caller's epoch as a by-reference entry, in the
// order it was received.
//
// A committer includes all valid pending proposals (RFC 9420 section 12.4 SHOULD), and the ORDER
// is the reception order rather than the map's, because Add placement depends on it and a map
// range answers a different tree on every run.
//
// The group context is here for Cached's reason and it matters more here, because what this answers
// goes straight into a commit: a Pending that read only the map would hand a committer of the new
// epoch the references of the closed one, and the commit built from them names proposals no
// receiver can resolve. Nothing of another epoch is answered, so a committer over a cache nobody
// rebound commits nothing rather than committing a replay.
func (self *ProposalCache) Pending(groupContext *GroupContext) []ProposalOrRef {
	if groupContext == nil || !self.bindingHolds(groupContext.GroupId, groupContext.Epoch) {
		return []ProposalOrRef{}
	}
	out := make([]ProposalOrRef, 0, len(self.order))
	for _, key := range self.order {
		out = append(out, ProposalOrRef{Type: ProposalOrRefTypeReference, Reference: ProposalRef(key)})
	}
	return out
}
