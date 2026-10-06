// The RFC 9420 section 12.2 proposal-list refusals: ONE VALUE PER RULE, thirteen ValSem codes,
// the three list rules section 12.2 states without a code, and the two rules section 12.1.2 puts
// on an Update's own LeafNode.
//
// One value per rule is the whole content of this file and it is why the file exists at all
// rather than the values sitting beside the functions that raise them. The plan this task comes
// from writes ValSem103 and ValSem110 answering one sentinel, ValSem106 and ValSem109 answering
// another, and the section 12.2 same-leaf rule answering ValSem107's -- five rules reduced to
// three comparisons. errors.Is cannot tell two rules apart when they answer one value, so a
// negative test asserting the broad question passes over a rule that fired for the wrong reason,
// and a caller told "an encryption key is already in use" cannot tell an ADD it should refuse
// from an UPDATE whose sender it should stop trusting. That is this project's most repeated
// defect and errors_lifecycle.go, tree_errors.go and proposal_list.go each say so in their own
// headers. TestEveryProposalListRefusalIsDistinctFromEveryOther holds it over the class derived
// from this file rather than over a list, and TestEachProposalListRuleAnswersOnlyItsOwnSentinel
// builds the input for each rule and requires the other twelve NOT to answer.
//
// THREE NAMES THE INTERFACE REGISTRY SPELLS DIFFERENTLY, and the reason is not cosmetic. The
// registry assigns ErrDuplicateSignatureKey, ErrDuplicateEncryptionKey and
// ErrMissingRequiredCapability to this plan; package mls already carries errDuplicateSignatureKey
// and errDuplicateEncryptionKey in tree_sync.go and errMissingRequiredCapability in extension.go,
// each an unexported stand-in for exactly those exported spellings, and
// TestNoValidationOwnedNameHasLandedBesideItsStandIn fails on the commit that lands either half
// of such a pair. Landing the registry spelling here would therefore force a package-wide swap of
// three names in a task that owns none of their call sites -- and it would put ValSem101 and the
// tree's own duplicate-key check, and ValSem103 and ValSem110, back on one value, which is the
// defect above. So the two ADD rules and the two UPDATE rules are named for the proposal they
// judge, the stand-ins keep the tree-wide and leaf-wide spellings they already answer, and the
// swap stays available to whoever lands the validation plan's catalogue.
//
// ValSem113 declares nothing here, and that absence is deliberate rather than an omission. Its
// rule is "every proposal type in the list is one this build processes", and proposal_list.go
// already owns that refusal surface and splits it into eight values with an argument for each --
// a psk, a reinit, an external commit, the reserved code point, an unregistered code point, a nil
// proposal, a forged wire discriminant, and an accepted type with no bucket. An umbrella sentinel
// declared here and returned alongside them would be the "second clause answering the same input
// with a different sentinel" that file's header rejects, and it would hand a caller two ways to
// spell one question. ValSem113 answers that gate's values unchanged.
package mls

import "errors"

// The thirteen ValSem codes of a proposal list, RFC 9420 sections 12.1 and 12.2, plus the three
// list rules section 12.2 states with no code of their own.
//
// Every message begins "mls: " because a refusal handed to a caller names the package it came
// from, and every one of them names the PROPOSAL KIND it judges -- "an added leaf", "an update",
// "a proposal list" -- because the code number is not in the string and a caller reading the
// message is trying to work out which of its own proposals to withdraw.
var (
	// ValSem101: an Add's signature key is not one the group already publishes and not one
	// another Add in this list publishes. The group's own copy excludes leaves this list
	// removes, which is RFC 9420 section 12.2's "unless there is a Remove proposal in the list
	// removing the matching client".
	ErrAddDuplicateSignatureKey = errors.New("mls: an added key package publishes a signature key the group or another add already publishes")

	// ValSem102: two Adds in one list do not publish the same init key. Among the proposals
	// only: an init key is a key package's and no member of the tree holds one.
	ErrDuplicateInitKey = errors.New("mls: two added key packages publish the same init key")

	// ValSem103: an Add's leaf encryption key is not one the group or another Add publishes.
	// Distinct from ValSem110 because the remedies are opposite -- an add is refused, an
	// update is a member republishing a key somebody else already holds.
	ErrAddDuplicateEncryptionKey = errors.New("mls: an added leaf publishes an encryption key the group or another add already publishes")

	// ValSem104: an Add's init key differs from its leaf's encryption key. Equal keys would
	// make the welcome ciphertext and the path ciphertext openable by one private key.
	ErrInitEqualsEncryptionKey = errors.New("mls: an added key package's init key is its own leaf's encryption key")

	// ValSem105: an Add's KeyPackage names this group's protocol version and ciphersuite.
	ErrSuiteMismatch = errors.New("mls: an added key package names another protocol version or ciphersuite")

	// ValSem106: an added leaf supports every capability the group requires, including one a
	// GroupContextExtensions proposal in this same list adds.
	ErrAddMissingRequiredCapability = errors.New("mls: an added leaf does not support one of the group's required capabilities")

	// ValSem107: a leaf is removed at most once.
	ErrDuplicateRemove = errors.New("mls: a proposal list removes one leaf twice")

	// ValSem108: a removed leaf is a non-blank leaf of the pre-commit tree.
	ErrRemoveNonMember = errors.New("mls: a remove proposal names a leaf that is blank or outside the tree")

	// ValSem109: an updated leaf still supports every capability the group requires. Separate
	// from ValSem106 for ValSem103's reason and for erratum 8745's: the published text imposes
	// this on the client ADDING a member and on nobody updating one, so an implementation whose
	// two halves answered one value could not be asked which of them it had checked.
	ErrUpdateMissingRequiredCapability = errors.New("mls: an updated leaf does not support one of the group's required capabilities")

	// ValSem110: an Update's encryption key is not one another member, another Add or another
	// Update already publishes.
	ErrUpdateDuplicateEncryptionKey = errors.New("mls: an update publishes an encryption key another proposal or another member already publishes")

	// ValSem111: the committer does not cover its own Update. Its leaf is reset by the
	// UpdatePath instead, so an Update beside it is a second answer to what that leaf holds.
	ErrSelfUpdateInCommit = errors.New("mls: the committer covered its own update proposal")

	// ValSem112: an Update is attributed to a leaf that is occupied in the pre-commit tree.
	// The cache refuses a non-member SENDER TYPE at Store time under
	// errProposalSenderNotMember; this is the other observation -- that the leaf index the
	// sender was attributed to is occupied -- and a list assembled in process has been through
	// neither.
	ErrUpdateSenderNotMember = errors.New("mls: an update proposal is attributed to a leaf that is blank or outside the tree")

	// RFC 9420 section 12.2's FIRST invalidation condition -- "It contains an individual proposal
	// that is invalid as specified in Section 12.1" -- reaching section 12.1.2's "An Update
	// proposal is invalid if the LeafNode is invalid for an Update proposal according to Section
	// 7.3".
	//
	// Its own value and NOT ValSem109's, which is the rule this file's header is about. ValSem109
	// asks one question of an updated leaf -- does it still support the group's
	// required_capabilities -- and section 7.3 asks eight, of which that is one. An implementation
	// whose whole section 7.3 answer came back as ValSem109's value could not be asked whether it
	// had checked the SIGNATURE, and a caller told "an updated leaf does not support one of the
	// group's required capabilities" over a forged leaf would go looking at its own capabilities
	// vector. It WRAPS the leaf validator's own refusal rather than replacing it, so a caller may
	// ask either question: which proposal is bad, or what section 7.3 said about it.
	ErrUpdateLeafNodeInvalid = errors.New("mls: an update proposal carries a leaf node that is not valid for an update")

	// Section 7.3's UPDATE arm of the leaf_node_source rule: "the encryption_key represents a
	// different public key than the encryption_key in the leaf node being replaced".
	//
	// (*LeafNode).Validate cannot state it -- it is a rule about two leaves and that validator
	// holds one -- and LeafValidationContext's own header hands it here by name. It is the clause
	// that stands where the lifetime check stands under key_package: the lifetime is a variant
	// field an update leaf does not carry, so section 7.3's freshness obligation for an update is
	// that the key actually CHANGED.
	//
	// Distinct from ValSem110 and the two are near opposites. ValSem110 refuses an update
	// publishing a key SOMEBODY ELSE holds, and to do that it excludes the updating leaf's own
	// outgoing key from the set it compares against -- which is exactly the key this rule refuses.
	// One value for the two would make the exclusion and the refusal indistinguishable to a
	// caller, and the same exclusion is why no other rule of this file can answer for this one.
	ErrUpdateEncryptionKeyUnchanged = errors.New("mls: an update proposal republishes the encryption key of the leaf it replaces")

	// RFC 9420 section 12.2's "It contains a Remove proposal that removes the committer". The
	// plan's thirteen do not carry it and the RFC states it in the same list as ValSem111's
	// rule; a commit that removed its own sender would advance an epoch whose only writer is no
	// longer in the group.
	ErrRemoveCommitter = errors.New("mls: a proposal list removes the committer")

	// RFC 9420 section 12.2's "It contains multiple Update and/or Remove proposals that apply
	// to the same leaf". Its own value and not ValSem107's, because ValSem107 is about two
	// REMOVES and this is about an update and a remove landing on one leaf -- which is the pair
	// whose application order would otherwise decide what the leaf ends up holding.
	ErrUpdateOrRemoveSameLeaf = errors.New("mls: a proposal list carries more than one update or remove applying to one leaf")

	// TWO STRUCTURAL VALUES USED TO STAND HERE AND BOTH ARE GONE, which is written down because a
	// value deleted with no account reads as a value somebody lost.
	//
	// ErrProposalListMisbucketed said a bucket held a proposal of a type it was not named for, and
	// ErrProposalListBucketsDisagree said the buckets were not the commit order bucketed by type.
	// Both were preconditions of a ProposalList that carried its commit order and four per-type
	// buckets as independently writable fields. The type now stores the commit ORDER alone and
	// derives every per-type view from it -- see proposal_list.go -- so an entry of
	// (*ProposalList).Removes is a Remove because that is the predicate it was selected by, and a
	// view that disagrees with the order cannot be constructed. Neither value has an input left
	// that could answer it, and a sentinel no input reaches is a rule that reports a clean bill
	// forever.
)

// errNilProposalValidationInput is what the validation entry points answer instead of
// dereferencing what they were handed.
//
// Unexported for the reason proposal_list.go's stand-ins are: the validation plan owns the
// catalogue of exported ValSem names, this is not one of them but an argument rule of this
// package's own API, and every caller of these functions in the tasks after this one is inside
// package mls. TestEveryProposalValidationEntryPointRefusesANilInput runs all fourteen doors.
var errNilProposalValidationInput = errors.New("mls: proposal list validation was handed no input, no list, no tree or no group context")

// errNilProposalList and errNilRatchetTree are the same rule at the application door. Two values
// and not one, because a caller that passed no tree and a caller that passed no proposals have
// made different mistakes and repair them in different places.
var (
	errNilProposalList = errors.New("mls: applying proposals requires a proposal list")
	errNilRatchetTree  = errors.New("mls: applying proposals requires a ratchet tree")
)
