// RFC 9420 section 12.2 proposal-list validation, one named function per ValSem code so the
// negative tests the validation plan owns have a specific target, plus the three list rules
// section 12.2 states with no code of their own, plus the two rules section 12.2's FIRST
// invalidation condition reaches -- "It contains an individual proposal that is invalid as
// specified in Section 12.1" -- for section 12.1.2's Update. Every rule runs on the sender side and
// on the receiver side, from the same input.
//
// THE ORDER THE RULES RUN IN, and the one place it is not the plan's. ValSem113 runs FIRST rather
// than thirteenth, and the two structural rules beside it run before every ValSem code. That is
// not a preference about which refusal a caller sees: every rule below reads an ARM off a
// proposal -- cached.Proposal.Add.KeyPackage, cached.Proposal.Remove.Removed,
// cached.Proposal.Update.LeafNode -- and the check that an Add carries an Add arm lives inside
// ValSem113's gate, while the check that the Adds bucket carries only Adds lives beside it. Run
// thirteenth, as the plan writes it, a list holding an Add with a nil Add arm, or holding a
// Remove in the Adds bucket, is a nil dereference inside ValSem101 -- a panic out of a library,
// which takes the caller's process rather than its call. Every list (*ProposalCache).Resolve
// produces satisfies both preconditions; a ProposalList a caller assembled field by field, which
// is exactly what this plan's own tests and the vector runners build, satisfies neither.
//
// What that costs is a list failing both a structural rule and a duplicate-key rule reporting the
// structural one, and what it buys is that the other twelve rules can be written the way the RFC
// states them. Among the twelve themselves the plan's order stands: a duplicate key is reported
// before a capability mismatch, so the error a caller sees names the closest cause.
package mls

import (
	"crypto/subtle"
	"fmt"
	"time"

	"github.com/urnetwork/connect/mls/syntax"
)

// ProposalValidationInput is the pre-commit state a proposal list is judged against.
//
// Extensions is the POST-GroupContextExtensions extension list, because RFC 9420 section 12.3
// applies the GCE proposal first and requires the new extensions to be used when evaluating the
// rest of the list: a GCE that adds a required_capabilities extension is a requirement every Add
// in the same commit has to meet. A caller that has already applied the list hands the applied
// set here; a caller that has not leaves it nil and effectiveExtensions derives it.
type ProposalValidationInput struct {
	Crypto     CryptoProvider
	Tree       *RatchetTree
	Context    *GroupContext
	Extensions []Extension
	Committer  LeafIndex
	List       *ProposalList
	Now        time.Time
}

// check is the argument rule every entry point of this file runs first.
//
// A method with a nil-receiver guard rather than a nil comparison in each of the twenty rule
// functions, so that one rule is written once: twenty copies of a guard is twenty chances for one
// of them to be the copy that was not updated. Go calls a method on a nil pointer without
// dereferencing it, so this is reachable from every door.
// TestEveryProposalValidationEntryPointRefusesANilInput drives all of them.
//
// FOUR VALUES AND NOT ONE, which is the rule the four arguments of ApplyProposals are held to and
// is here for the same reason: a caller that handed this no tree and a caller that handed it no
// proposals have made different mistakes and repair them in different places, and "the input was
// incomplete" is a sentence that sends both of them to read the whole struct. Three of the four
// already exist for exactly these conditions -- the two the application door answers, and the key
// schedule's own for a missing group context -- so this adds one value rather than four, and no
// condition of this package comes to answer two names.
func (self *ProposalValidationInput) check() error {
	if self == nil {
		return fmt.Errorf("%w: no validation input", errNilProposalValidationInput)
	}
	if self.List == nil {
		return fmt.Errorf("%w: proposal list validation has nothing to judge", errNilProposalList)
	}
	if self.Tree == nil {
		return fmt.Errorf("%w: every membership rule of section 12.2 is stated over it", errNilRatchetTree)
	}
	if self.Context == nil {
		return fmt.Errorf("%w: the version, the suite and the group extensions are its", ErrNilGroupContext)
	}
	if err := checkProposalListStructure(self.List); err != nil {
		return err
	}
	// AND THE LIST CARRIES AT MOST ONE GroupContextExtensions PROPOSAL, which belongs at the door
	// for checkProposalListStructure's own reason one line up. (*ProposalList).Extensions answers
	// GCE[0], so every reader of the installed extension set decides off the FIRST of however many
	// the list carries: effectiveExtensions and through it ValSem106, ValSem109 and the section
	// 12.1.2 update rules, ApplyProposals' first step, and the commit door's extension join. Over a
	// list carrying two that is a choice nothing states, and the rule refusing it ran FIFTH inside
	// one aggregate rather than at the door every one of those readers arrives through -- so a
	// caller reaching any single rule of this file got the first of two extension sets and no
	// refusal at all. A rule the aggregate reaches is not a rule the exported doors beside it ask.
	return checkOneGroupContextExtensions(self.List)
}

// effectiveExtensions is the extension list the rest of the list is judged against.
//
// Three sources in one order, and each of the two fallbacks answers a caller that exists. An
// explicit Extensions is a caller that has already applied section 12.3's first step and is
// re-checking the applied state. A list carrying a GroupContextExtensions proposal is the sender
// side, which judges the list before applying anything. Neither is the ordinary case, which is
// the group's own extensions, unchanged.
func (self *ProposalValidationInput) effectiveExtensions() []Extension {
	if self.Extensions != nil {
		return self.Extensions
	}
	if exts, ok := self.List.Extensions(); ok {
		return exts
	}
	return self.Context.Extensions
}

// The three groups of rules ValidateProposalList runs, in the order it runs them.
//
// Slices rather than a straight-line sequence of calls, because what has to be checkable is that
// the aggregate runs EVERY ValSem code and not a subset of them:
// TestValidateProposalListRunsEveryRuleThisFileDeclares reads the rule functions out of this
// file's source -- by SIGNATURE, so the six rules that carry no ValSem code are in the class too
// -- and requires each to appear in exactly one of the three, so a twentieth rule added below is
// unrun until somebody puts it in a group, and a rule deleted from a group fails rather than
// quietly leaving the aggregate smaller.
//
// The groups are three and not one because the ORDER between them is load bearing and the header
// says why: the structural group is what makes the rules of the second group safe to write.
//
// The second group ENDS with the two section 12.1.2 rules rather than beginning with them, and
// that position is load bearing twice over. Both read the sender's leaf index -- one to rebuild
// the LeafNodeTBS the update leaf signed, one to find the leaf it replaces -- so both are written
// against a sender ValSem112 has already said is a member. And ValSem109 states the
// required_capabilities clause at the list level with a value of its own, so it is asked first and
// the leaf validator's broader answer cannot stand in for it.
var (
	proposalListStructuralChecks = []func(*ProposalValidationInput) error{
		ValSem113ProposalTypeSupported,
		validateOneGroupContextExtensions,
		validateNoRepeatedProposalReference,
	}
	proposalListChecks = []func(*ProposalValidationInput) error{
		ValSem101UniqueSignatureKey,
		ValSem102UniqueInitKey,
		ValSem103UniqueEncryptionKey,
		ValSem104InitNotEqualEncryptionKey,
		ValSem105SuiteAndVersionMatch,
		ValSem106RequiredCapabilitiesSatisfied,
		ValSem107UniqueRemove,
		ValSem108RemoveExists,
		ValSem109UpdateRequiredCapabilities,
		ValSem110UpdateUniqueEncryptionKey,
		ValSem111NoCommitterUpdate,
		ValSem112UpdateSenderIsMember,
		validateUpdateLeafNodeIsValidForAnUpdate,
		validateUpdateChangesTheEncryptionKey,
	}
	proposalListCrossChecks = []func(*ProposalValidationInput) error{
		validateSingleUpdateOrRemovePerLeaf,
		validateCommitterIsNotRemoved,
	}
)

// ValidateProposalList runs every rule of RFC 9420 section 12.2 over a regular, non-external
// commit's proposal list and returns the first failure.
//
// EXTERNAL COMMITS ARE NOT JUDGED HERE and that is a profile decision rather than a gap. Section
// 12.2 states a second, disjoint list of rules for a list carrying an ExternalInit, and this
// build refuses external commits outright at the profile gate -- errProfileExternalCommit -- so
// an external commit's list is refused by ValSem113 before any rule below could be asked about
// it. A second procedure here would be an implementation of a shape this build never accepts.
//
// ITS OWN in.check IS REDUNDANT AND IS KEPT, which is worth saying because a mutation measured it:
// every rule of every group opens with the same call, so removing this one leaves the whole of
// ./mls/... and ./message/... green -- the first structural rule answers the identical value a
// moment later. It stays because refusing at the door is what the rest of this package does, and
// because without it the aggregate's refusal would depend on the first element of a slice.
// Nothing here claims a test can tell which of the two guards fired.
func ValidateProposalList(in *ProposalValidationInput) error {
	if err := in.check(); err != nil {
		return err
	}
	for _, group := range [][]func(*ProposalValidationInput) error{
		proposalListStructuralChecks, proposalListChecks, proposalListCrossChecks} {
		for _, check := range group {
			if err := check(in); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// the structural rules the twelve below are written against
// ---------------------------------------------------------------------------

// checkProposalArms is the structural precondition EVERY rule that reads a proposal's arm is
// written against, enforced at the door rather than stated in prose.
//
// IT WAS STATED IN PROSE AND ENFORCED NOWHERE. This file's header and validateCommitterIsNotRemoved's
// both say the structural rules run before the rules that read an arm, and what made that true was
// the ORDER INSIDE ONE AGGREGATE. Every rule of this file is an exported door of its own, and
// validate_commit.go's rules are eleven more that never call ValidateProposalList at all, so a
// caller reaching any of them hands a list nothing has been through: a Remove carrying no Remove
// arm is then `in.List.Removes()[i].Proposal.Remove.Removed` on a nil pointer -- a panic out of a
// library, which takes the caller's process rather than its call and is the one answer a door of
// this package must never give. Both check methods run this, so the precondition holds at every
// door rather than at the two aggregates.
//
// ONE RULE OVER THE COMMIT ORDER, AND THAT IS NOW THE WHOLE CLASS. It is checkProposalProfile,
// which is proposal_list.go's and reaches (*Proposal).checkArm as its last of four -- called WHOLE
// rather than reduced to the arm clause, because those four run in an order the gate's own header
// argues for and a caller wants: a pre_shared_key proposal is told that this profile does not
// implement pre_shared_key, not that its arm is missing.
//
// IT USED TO BE TWO. The second was "an entry of the Removes bucket is a Remove", because the
// buckets were fields a caller filled in and an Add sitting in the Removes field carried an Add
// arm that every rule stated over removes would then dereference. (*ProposalList).Removes is now
// the commit order FILTERED, so an entry it answers carries a Remove type because that is the
// predicate it was selected by, and the profile sweep over the order below has already judged that
// entry's arm. There is no misbucketing left to refuse, and ErrProposalListMisbucketed is gone
// with the rule rather than kept as a value nothing can answer.
//
// It answers checkProposalProfile's eight values and no value of its own, which is
// errors_proposal_validation.go's rule: one rule, one value, however many doors ask it.
// ValSem113ProposalTypeSupported is therefore redundant with this everywhere they both run and is
// kept, on ValidateProposalList's own stated terms -- refusing at the door is what the rest of
// this package does, and nothing here claims a test can tell which of the two guards fired.
func checkProposalListStructure(list *ProposalList) error {
	active := defaultProfile()
	order := list.All()
	for i := range order {
		if err := checkProposalProfile(active, &order[i].Proposal); err != nil {
			return fmt.Errorf("%w: at proposal %d of the commit order", err, i)
		}
	}
	return nil
}

// ValSem113ProposalTypeSupported: every proposal in the list is one this group can process.
//
// It reaches the same gate (*ProposalCache).Store and (*ProposalCache).Resolve reach, which is
// what keeps exactly one table of what the v1 profile accepts. Its answers are that gate's eight
// values unchanged and not an umbrella of this file's own -- see the note in
// errors_proposal_validation.go.
//
// THE SWEEP IS OVER THE COMMIT ORDER AND THAT IS EVERY PROPOSAL THERE IS. It used to sweep the
// order and then each bucket, because the two were separate fields and a proposal reachable by
// application through a bucket while absent from All would otherwise have been applied having been
// judged by nothing. Every view is now the order filtered, so a bucket entry IS an order entry and
// the second sweep judged each of them twice. It is also what makes the twelve rules below safe to
// write, because the arm check is inside this gate.
//
// THIS LOOP IS UNOBSERVABLE AND IS KEPT, which is measured rather than assumed: narrowing it to the
// first proposal of the order leaves the whole of ./mls/... and ./message/... green. Every door of
// this file opens with in.check, which reaches checkProposalListStructure, which sweeps the same
// order a moment earlier and answers the same eight values -- so no input can reach this loop
// having got past that one. It stays on ValidateProposalList's own stated terms: refusing at the
// door is what the rest of this package does, this is the rule the plan names, and nothing here
// claims a test can tell which of the two guards fired.
func ValSem113ProposalTypeSupported(in *ProposalValidationInput) error {
	if err := in.check(); err != nil {
		return err
	}
	active := defaultProfile()
	order := in.List.All()
	for i := range order {
		if err := checkProposalProfile(active, &order[i].Proposal); err != nil {
			return fmt.Errorf("%w: at proposal %d of the commit order", err, i)
		}
	}
	return nil
}

// TWO RULES USED TO STAND HERE AND BOTH ARE GONE, which is the point of the change that removed
// them rather than a loss.
//
// validateProposalBucketsHoldTheirOwnType said "an entry of the Removes bucket is a Remove", and
// validateBucketsAgreeWithTheCommitOrder said "the buckets are the commit order bucketed by type"
// -- by a per-type COUNT, which is as far as a cheap rule could get. Both were stated because
// ProposalList carried All and four buckets as independently writable fields, and both were
// bypassed in count-preserving form: All=[remove(committer)] beside Removes=[remove(3)] satisfied
// the count, was accepted by ValidateCommit, and was applied by removing leaf 3.
//
// (*ProposalList).Removes is now the commit order filtered to removes, so there is no second field
// to disagree, nothing to count against anything, and no input either rule could refuse. A check
// leaves the dual representation in place for the next reader; the derivation removes it. What
// held the two representations together is now proposal_list.go's TYPE, and the gates that hold
// the type to that claim are TestEveryPerTypeViewOfAProposalListIsItsCommitOrderFiltered and
// TestAProposalListKeepsItsProposalsInExactlyOnePlace.

// validateOneGroupContextExtensions is RFC 9420 section 12.2's "It contains multiple
// GroupContextExtensions proposals".
//
// The cache's Resolve refuses the second one as it buckets, under this same value; this is the
// other door, for a list nothing resolved. It is what makes ProposalList.Extensions exact rather
// than a silent choice between two extension sets, and it runs before ValSem106 and ValSem109
// because both of those read the answer.
func validateOneGroupContextExtensions(in *ProposalValidationInput) error {
	if err := in.check(); err != nil {
		return err
	}
	return checkOneGroupContextExtensions(in.List)
}

// checkOneGroupContextExtensions is that rule as a body BOTH doors run, which is the shape
// checkProposalListStructure takes and is here for its reason: this is a precondition every rule
// that reads the installed extension set stands on, and a precondition enforced by the order of one
// aggregate is a precondition every other entry point skips. It answers the same value the rule
// does, because it is the rule.
//
// ValidateProposalList's own note applies unchanged: the rule above is now redundant with the door
// wherever it runs, it is kept because refusing at the door is what the rest of this package does,
// and nothing here claims a test can tell which of the two guards fired.
func checkOneGroupContextExtensions(list *ProposalList) error {
	gce := list.GCE()
	if len(gce) > 1 {
		return fmt.Errorf("%w: the list carries %d", errMultipleGroupContextExtensions, len(gce))
	}
	return nil
}

// validateNoRepeatedProposalReference refuses a commit that names one cached proposal twice.
//
// Resolve refuses it as it walks the ProposalOrRef vector, and this is the same rule over a list
// that vector no longer exists for. It is NOT covered by the duplicate rules below and that is
// the whole reason it is written: two references to one Remove are caught by ValSem107, two to
// one Add by ValSem101, two to one Update by the same-leaf rule -- but each of those is a rule
// about the CONTENT, and a duplicate reference is a fault about IDENTITY. A profile that later
// admits a fifth proposal type would have to add a content rule for it or lose the refusal
// entirely, and the RFC's own transcript is computed over the vector rather than over its
// contents.
//
// By-value entries carry no reference and are skipped: their Ref is the zero value, and a list of
// two inline proposals would otherwise be two entries sharing the empty name.
func validateNoRepeatedProposalReference(in *ProposalValidationInput) error {
	if err := in.check(); err != nil {
		return err
	}
	namedAt := map[string]int{}
	order := in.List.All()
	for i := range order {
		cached := order[i]
		if cached.ByValue || len(cached.Ref) == 0 {
			continue
		}
		key := string(cached.Ref)
		if at, twice := namedAt[key]; twice {
			return fmt.Errorf("%w: entries %d and %d both name %x",
				errDuplicateProposalReference, at, i, cached.Ref)
		}
		namedAt[key] = i
	}
	return nil
}

// ---------------------------------------------------------------------------
// ValSem101 to ValSem112
// ---------------------------------------------------------------------------

// ValSem101UniqueSignatureKey: an Add's signature key is unique among the proposals and among the
// members this commit leaves in the group.
//
// The members' half excludes leaves this list removes, which is RFC 9420 section 12.2's "unless
// there is a Remove proposal in the list removing the matching client from the group" -- the
// clause that makes a device replacing itself in one commit legal.
func ValSem101UniqueSignatureKey(in *ProposalValidationInput) error {
	if err := in.check(); err != nil {
		return err
	}
	adds := in.List.Adds()
	seen := map[string]bool{}
	removed := removedLeaves(in.List)
	for _, leafIndex := range in.Tree.NonBlankLeaves() {
		if removed[leafIndex] {
			continue
		}
		leaf := in.Tree.Leaf(leafIndex)
		if leaf == nil {
			continue
		}
		seen[string(leaf.SignatureKey)] = true
	}
	for i := range adds {
		key := string(adds[i].Proposal.Add.KeyPackage.LeafNode.SignatureKey)
		if seen[key] {
			return fmt.Errorf("%w: adds[%d] publishes %x", ErrAddDuplicateSignatureKey, i, key)
		}
		seen[key] = true
	}
	return nil
}

// ValSem102UniqueInitKey: an Add's init key is unique among the proposals.
//
// Among the proposals only. An init key belongs to a KeyPackage and a KeyPackage is consumed by
// the Welcome; nothing in the ratchet tree publishes one, so there is no members' half to check
// and a loop over the tree here would be enforcing a rule over values it could not read.
func ValSem102UniqueInitKey(in *ProposalValidationInput) error {
	if err := in.check(); err != nil {
		return err
	}
	adds := in.List.Adds()
	seen := map[string]bool{}
	for i := range adds {
		key := string(adds[i].Proposal.Add.KeyPackage.InitKey)
		if seen[key] {
			return fmt.Errorf("%w: adds[%d] publishes %x", ErrDuplicateInitKey, i, key)
		}
		seen[key] = true
	}
	return nil
}

// ValSem103UniqueEncryptionKey: an Add's leaf encryption key is unique among the proposals and
// among the members this commit leaves in the group.
func ValSem103UniqueEncryptionKey(in *ProposalValidationInput) error {
	if err := in.check(); err != nil {
		return err
	}
	adds := in.List.Adds()
	seen := map[string]bool{}
	removed := removedLeaves(in.List)
	for _, leafIndex := range in.Tree.NonBlankLeaves() {
		if removed[leafIndex] {
			continue
		}
		leaf := in.Tree.Leaf(leafIndex)
		if leaf == nil {
			continue
		}
		seen[string(leaf.EncryptionKey)] = true
	}
	for i := range adds {
		key := string(adds[i].Proposal.Add.KeyPackage.LeafNode.EncryptionKey)
		if seen[key] {
			return fmt.Errorf("%w: adds[%d] publishes %x", ErrAddDuplicateEncryptionKey, i, key)
		}
		seen[key] = true
	}
	return nil
}

// ValSem104InitNotEqualEncryptionKey: an Add's init key differs from its leaf's encryption key.
//
// Equal keys would make the Welcome ciphertext and the update path ciphertext openable by one
// private key, which collapses the separation between joining the group and being in it.
//
// The comparison is crypto/subtle.ConstantTimeCompare and not bytes.Equal, for guardrail 8's
// reason as this package states it everywhere else: nothing this package ships compares data with
// a variable-time call, in any file, and the class that holds that is derived off the imports
// rather than off a list of banned names.
func ValSem104InitNotEqualEncryptionKey(in *ProposalValidationInput) error {
	if err := in.check(); err != nil {
		return err
	}
	adds := in.List.Adds()
	for i := range adds {
		kp := &adds[i].Proposal.Add.KeyPackage
		if subtle.ConstantTimeCompare(kp.InitKey, kp.LeafNode.EncryptionKey) == 1 {
			return fmt.Errorf("%w: adds[%d]", ErrInitEqualsEncryptionKey, i)
		}
	}
	return nil
}

// ValSem105SuiteAndVersionMatch: an Add's KeyPackage names this group's ciphersuite and protocol
// version, and is otherwise valid per RFC 9420 section 10.1.
//
// The section 10.1 half answers key_package.go's own refusals rather than a value of this file's:
// "this key package is not well formed" is a different fact from "this key package is for another
// group", a caller repairs the two at opposite ends, and KeyPackage.Validate is the single
// declaration site for the first of them.
func ValSem105SuiteAndVersionMatch(in *ProposalValidationInput) error {
	if err := in.check(); err != nil {
		return err
	}
	adds := in.List.Adds()
	for i := range adds {
		kp := &adds[i].Proposal.Add.KeyPackage
		if kp.CipherSuite != in.Context.CipherSuite || kp.Version != in.Context.Version {
			return fmt.Errorf("%w: adds[%d] is version %#04x suite %#04x, the group is %#04x %#04x",
				ErrSuiteMismatch, i, uint16(kp.Version), uint16(kp.CipherSuite),
				uint16(in.Context.Version), uint16(in.Context.CipherSuite))
		}
		if err := kp.Validate(in.Crypto, in.Context.CipherSuite, in.Now); err != nil {
			return fmt.Errorf("%w: at adds[%d]", err, i)
		}
	}
	return nil
}

// ValSem106RequiredCapabilitiesSatisfied: an added member supports every required capability,
// including any a GroupContextExtensions proposal in this same commit adds.
func ValSem106RequiredCapabilitiesSatisfied(in *ProposalValidationInput) error {
	if err := in.check(); err != nil {
		return err
	}
	adds := in.List.Adds()
	required, err := requiredCapabilitiesOf(in.effectiveExtensions())
	if err != nil || required == nil {
		return err
	}
	for i := range adds {
		caps := &adds[i].Proposal.Add.KeyPackage.LeafNode.Capabilities
		if err := caps.Supports(required); err != nil {
			return fmt.Errorf("%w: adds[%d]: %v", ErrAddMissingRequiredCapability, i, err)
		}
	}
	return nil
}

// ValSem107UniqueRemove: a leaf is removed at most once.
func ValSem107UniqueRemove(in *ProposalValidationInput) error {
	if err := in.check(); err != nil {
		return err
	}
	removes := in.List.Removes()
	seen := map[LeafIndex]bool{}
	for i := range removes {
		leafIndex := removes[i].Proposal.Remove.Removed
		if seen[leafIndex] {
			return fmt.Errorf("%w: leaf %d, at removes[%d]", ErrDuplicateRemove, leafIndex, i)
		}
		seen[leafIndex] = true
	}
	return nil
}

// ValSem108RemoveExists: a removed leaf is a non-blank leaf of the PRE-commit tree.
//
// Pre-commit and not post: the tree this reads is the one every rule of this file reads, and a
// list removing a leaf another entry of the same list also removes is ValSem107's rule rather
// than this one.
func ValSem108RemoveExists(in *ProposalValidationInput) error {
	if err := in.check(); err != nil {
		return err
	}
	removes := in.List.Removes()
	for i := range removes {
		leafIndex := removes[i].Proposal.Remove.Removed
		if in.Tree.Leaf(leafIndex) == nil {
			return fmt.Errorf("%w: leaf %d, at removes[%d]", ErrRemoveNonMember, leafIndex, i)
		}
	}
	return nil
}

// ValSem109UpdateRequiredCapabilities: an updated leaf still supports the group's required
// capabilities.
//
// RFC 9420 as published imposes the group-extension compatibility check on the client ADDING a
// member and on nobody updating one; erratum 8745 is the report that the omission contradicts
// section 13.4's own conclusion about all members of the group. This package acts on the erratum
// -- see ERRATA.md and (*LeafNode).Validate -- and this rule is that decision at the list level.
func ValSem109UpdateRequiredCapabilities(in *ProposalValidationInput) error {
	if err := in.check(); err != nil {
		return err
	}
	updates := in.List.Updates()
	required, err := requiredCapabilitiesOf(in.effectiveExtensions())
	if err != nil || required == nil {
		return err
	}
	for i := range updates {
		caps := &updates[i].Proposal.Update.LeafNode.Capabilities
		if err := caps.Supports(required); err != nil {
			return fmt.Errorf("%w: updates[%d]: %v", ErrUpdateMissingRequiredCapability, i, err)
		}
	}
	return nil
}

// ValSem110UpdateUniqueEncryptionKey: an Update's encryption key is unique among the proposals and
// among current members, so an update cannot reinstate a key another leaf already holds.
//
// The members' half excludes the leaves this list UPDATES as well as the ones it removes, because
// a leaf's own outgoing key is exactly the key it is replacing: without that exclusion a member
// republishing anything at all would be refused for colliding with itself.
func ValSem110UpdateUniqueEncryptionKey(in *ProposalValidationInput) error {
	if err := in.check(); err != nil {
		return err
	}
	adds := in.List.Adds()
	updates := in.List.Updates()
	seen := map[string]bool{}
	removed := removedLeaves(in.List)
	updated := map[LeafIndex]bool{}
	for i := range updates {
		updated[updates[i].Sender] = true
	}
	for _, leafIndex := range in.Tree.NonBlankLeaves() {
		if removed[leafIndex] || updated[leafIndex] {
			continue
		}
		leaf := in.Tree.Leaf(leafIndex)
		if leaf == nil {
			continue
		}
		seen[string(leaf.EncryptionKey)] = true
	}
	for i := range adds {
		seen[string(adds[i].Proposal.Add.KeyPackage.LeafNode.EncryptionKey)] = true
	}
	for i := range updates {
		key := string(updates[i].Proposal.Update.LeafNode.EncryptionKey)
		if seen[key] {
			return fmt.Errorf("%w: updates[%d] publishes %x", ErrUpdateDuplicateEncryptionKey, i, key)
		}
		seen[key] = true
	}
	return nil
}

// ValSem111NoCommitterUpdate: the committer must not cover its own Update. Its leaf is reset by
// the UpdatePath instead, so an Update beside it is a second answer to what that leaf holds.
func ValSem111NoCommitterUpdate(in *ProposalValidationInput) error {
	if err := in.check(); err != nil {
		return err
	}
	updates := in.List.Updates()
	for i := range updates {
		if updates[i].Sender == in.Committer {
			return fmt.Errorf("%w: leaf %d, at updates[%d]", ErrSelfUpdateInCommit, in.Committer, i)
		}
	}
	return nil
}

// ValSem112UpdateSenderIsMember: a standalone Update is attributed to a leaf that is occupied in
// the pre-commit tree.
//
// The cache refuses a non-member SENDER TYPE at Store time; this is the other half of the same
// question and the one a list assembled in process has been through neither door of. The two
// answer different values on purpose -- see ErrUpdateSenderNotMember.
func ValSem112UpdateSenderIsMember(in *ProposalValidationInput) error {
	if err := in.check(); err != nil {
		return err
	}
	updates := in.List.Updates()
	for i := range updates {
		sender := updates[i].Sender
		if in.Tree.Leaf(sender) == nil {
			return fmt.Errorf("%w: leaf %d, at updates[%d]", ErrUpdateSenderNotMember, sender, i)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// RFC 9420 section 12.1.2: what an Update's own LeafNode owes
// ---------------------------------------------------------------------------

// validateUpdateLeafNodeIsValidForAnUpdate is RFC 9420 section 12.2's first invalidation
// condition -- "It contains an individual proposal that is invalid as specified in Section 12.1"
// -- reaching section 12.1.2: "An Update proposal is invalid if the LeafNode is invalid for an
// Update proposal according to Section 7.3."
//
// IT IS THE UPDATE DOOR ONTO (*LeafNode).Validate, and until it existed there was none.
// LeafValidationContext.ExpectedSource names one caller per source -- key_package.go with
// key_package, this with update, treekem.go's ValidateUpdatePathLeafNode with commit -- and when
// this landed only the first of the three had been written. The header said otherwise, which is
// the whole reason it is now derived rather than described:
// TestEveryLeafNodeSourceEitherHasAValidationDoorOrAnAdmittedGap reads the door class off the
// call sites, and the commit door landed one round later against that gate rather than against
// another paragraph. An Update's leaf therefore reached
// (*RatchetTree).UpdateLeaf, through ApplyProposals, with no signature check, no leaf_node_source
// check, no credential check and no section 13.4 group extension check: a member could install any
// leaf at its own index, including one another member signed, and this package would take it.
// ValSem109 next door reads that leaf's Capabilities and ValSem110 reads its EncryptionKey, and
// neither of them validates it.
//
// WHAT AN UPDATE LEAF OWES THAT A KEY PACKAGE LEAF DOES NOT, worked out from section 7.3 rather
// than copied off key_package.go's context:
//
//   - the SOURCE. ExpectedSource is LeafNodeSourceUpdate, so a leaf whose source says key_package
//     is refused HERE rather than installed. That is not a formality: the source selects the
//     section 7.2 variant AND the signature preimage, so a key_package leaf accepted at this
//     position would be one signed with no group id and no leaf index -- and signatureContent's
//     own header says what that is, a leaf that "verifies in whatever group it is pasted into" at
//     "whatever position of the tree it is moved to".
//   - the CONTEXT the signature is bound to. GroupId and LeafIndex are this group's id and the
//     index the update is attributed to, which is what makes the update arm of that select worth
//     anything. Passing them empty would verify every update leaf against a preimage no sender
//     built.
//   - NOTHING in place of the lifetime, at this door. The lifetime is a variant field carried only
//     under key_package, so validateLifetime answers nil for an update leaf whatever clock it is
//     handed, and a NowMs passed here would be an input no branch below could read. What section
//     7.3 puts in its place is the update arm of the leaf_node_source rule -- the encryption key
//     must differ from the one being replaced -- and that is a rule about two leaves, which is why
//     it is the separate door below rather than a field of this context.
//
// RequiredCaps IS PASSED AND IS REDUNDANT, said plainly because a mutation measures it: ValSem109
// runs first over the same leaf against the same effective extensions, so clearing this field to
// nil leaves the whole of ./mls/... and ./message/... green. It stays because handing a validator
// nil for a group that does require something is the shape reconcileRequiredCapabilities exists to
// refuse at the tree door -- "a caller that passed nil ... gets every leaf admitted without the
// requirement being applied at all" -- and a call site that reads that way is one a later reader
// has to re-derive. Nothing here claims a test can tell which of the two asked.
//
// GroupExtensions is NOT redundant and is the erratum 8745 half. ValSem109 asks only about
// required_capabilities; section 13.4 as corrected asks that an updated leaf support every
// extension in the GroupContext, and this is the only place a list-level caller asks it. ERRATA.md
// says this package applies that rule to all three sources -- which was true of
// (*LeafNode).Validate and, for update leaves, reached by nobody.
func validateUpdateLeafNodeIsValidForAnUpdate(in *ProposalValidationInput) error {
	if err := in.check(); err != nil {
		return err
	}
	updates := in.List.Updates()
	extensions := in.effectiveExtensions()
	required, err := requiredCapabilitiesOf(extensions)
	if err != nil {
		return err
	}
	for i := range updates {
		cached := &updates[i]
		err := cached.Proposal.Update.LeafNode.Validate(&LeafValidationContext{
			Crypto:          in.Crypto,
			Suite:           in.Context.CipherSuite,
			GroupId:         in.Context.GroupId,
			LeafIndex:       cached.Sender,
			ExpectedSource:  LeafNodeSourceUpdate,
			RequiredCaps:    required,
			GroupExtensions: extensions,
			// NowMs and ClockSkewMs are left at their zero values, and for an update leaf that
			// is not the documented opt out being taken -- it is the one input this rule has no
			// use for. See the paragraph above.
		})
		if err != nil {
			return fmt.Errorf("%w: updates[%d] for leaf %d: %w",
				ErrUpdateLeafNodeInvalid, i, cached.Sender, err)
		}
	}
	return nil
}

// validateUpdateChangesTheEncryptionKey is the UPDATE arm of section 7.3's leaf_node_source rule:
// "the encryption_key represents a different public key than the encryption_key in the leaf node
// being replaced".
//
// It is here and not in (*LeafNode).Validate because it is a rule about TWO leaves and that
// validator is handed one; LeafValidationContext's header lists it among the rules it cannot
// answer and hands it to proposal validation by name. This is that door.
//
// WHAT IT CLOSES is the one shape ValSem110 is written to let through. ValSem110 refuses an update
// publishing a key another member, another Add or another Update already publishes, and to do that
// it must EXCLUDE the updating leaf's own outgoing key -- "without that exclusion a member
// republishing anything at all would be refused for colliding with itself". The excluded key is
// precisely the key this rule refuses, so an Update that changes a leaf's credential, capabilities
// or extensions while keeping the encryption key it already had passes every other rule of this
// file. What section 7.3 wants of an update is fresh key material at that leaf: a commit built over
// an update that changed nothing re-seals the sender's path to a public key whose private half is
// exactly as old as the epoch the update was supposed to end.
//
// A blank or out of range sender is SKIPPED rather than refused, because ValSem112 owns that
// observation and answers a value of its own for it. Running after ValSem112 in the same group,
// this never sees one; the clause is written anyway so that the rule called on its own reports
// nothing rather than reading through a nil leaf.
//
// crypto/subtle for guardrail 8's reason, which is ValSem104's reason: nothing this package ships
// compares data with a variable-time call, and the class that holds that is derived off the
// imports rather than off a list of banned names.
func validateUpdateChangesTheEncryptionKey(in *ProposalValidationInput) error {
	if err := in.check(); err != nil {
		return err
	}
	updates := in.List.Updates()
	for i := range updates {
		cached := &updates[i]
		replaced := in.Tree.Leaf(cached.Sender)
		if replaced == nil {
			continue
		}
		if subtle.ConstantTimeCompare(replaced.EncryptionKey,
			cached.Proposal.Update.LeafNode.EncryptionKey) == 1 {
			return fmt.Errorf("%w: updates[%d] for leaf %d publishes %x again",
				ErrUpdateEncryptionKeyUnchanged, i, cached.Sender, replaced.EncryptionKey)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// the two section 12.2 rules that carry no ValSem code
// ---------------------------------------------------------------------------

// validateSingleUpdateOrRemovePerLeaf is RFC 9420 section 12.2's "It contains multiple Update
// and/or Remove proposals that apply to the same leaf".
//
// One SET over both kinds and not a set per kind, because the pair the rule is written for is an
// Update and a Remove landing on one leaf: two removes are ValSem107's rule and are refused
// before this runs, and the mixed pair is refused by nothing else. What a list carrying both
// would do is decide by APPLICATION ORDER what the leaf ends up holding -- section 12.3 applies
// updates before removes, so the leaf is blanked, and an implementation that applied in list
// order would leave the member in the group with a fresh key.
func validateSingleUpdateOrRemovePerLeaf(in *ProposalValidationInput) error {
	if err := in.check(); err != nil {
		return err
	}
	updates := in.List.Updates()
	removes := in.List.Removes()
	touched := map[LeafIndex]bool{}
	for i := range updates {
		sender := updates[i].Sender
		if touched[sender] {
			return fmt.Errorf("%w: leaf %d, at updates[%d]", ErrUpdateOrRemoveSameLeaf, sender, i)
		}
		touched[sender] = true
	}
	for i := range removes {
		leafIndex := removes[i].Proposal.Remove.Removed
		if touched[leafIndex] {
			return fmt.Errorf("%w: leaf %d, at removes[%d]", ErrUpdateOrRemoveSameLeaf, leafIndex, i)
		}
		touched[leafIndex] = true
	}
	return nil
}

// validateCommitterIsNotRemoved is RFC 9420 section 12.2's "It contains a Remove proposal that
// removes the committer".
//
// The plan's thirteen do not carry it, and it is not implied by any of them: ValSem108 accepts
// the committer's leaf because the committer is a member, and ValSem107 accepts a single remove.
// A commit carrying one would advance the epoch, seal a welcome and a path from a leaf the same
// commit blanks, and leave every receiver applying a tree whose only writer is no longer in it.
func validateCommitterIsNotRemoved(in *ProposalValidationInput) error {
	if err := in.check(); err != nil {
		return err
	}
	removes := in.List.Removes()
	for i := range removes {
		if removes[i].Proposal.Remove.Removed == in.Committer {
			return fmt.Errorf("%w: leaf %d, at removes[%d]", ErrRemoveCommitter, in.Committer, i)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// the two readers the rules above share
// ---------------------------------------------------------------------------

// removedLeaves is the set of leaves this list removes.
func removedLeaves(list *ProposalList) map[LeafIndex]bool {
	removes := list.Removes()
	out := map[LeafIndex]bool{}
	for i := range removes {
		out[removes[i].Proposal.Remove.Removed] = true
	}
	return out
}

// requiredCapabilitiesOf finds and decodes the required_capabilities extension. It answers a nil
// structure and a nil error when the group requires nothing.
//
// FindExtension and syntax.Unmarshal are the only two calls: there is one extension lookup and
// one decoder in this package, and this file adds neither.
//
// BOTH FAILURES ARE REFUSALS rather than "no requirement", which is the one place this differs
// from the plan's sketch and the difference is a security one. A vector carrying the extension
// twice, or carrying a body that does not decode, read as "the group requires nothing" would
// admit a member who supports none of it -- so a malformed extension would be strictly better for
// an attacker than a well formed one. ErrMalformedExtension is the value both answer because they
// are one rule: the extension the group's own context carries is not readable.
func requiredCapabilitiesOf(exts []Extension) (*RequiredCapabilities, error) {
	data, found, err := FindExtension(exts, ExtensionTypeRequiredCapabilities)
	if err != nil {
		return nil, fmt.Errorf("%w: required_capabilities: %v", ErrMalformedExtension, err)
	}
	if !found {
		return nil, nil
	}
	var required RequiredCapabilities
	if err := syntax.Unmarshal(data, &required); err != nil {
		return nil, fmt.Errorf("%w: required_capabilities: %v", ErrMalformedExtension, err)
	}
	return &required, nil
}
