// Whole-tree validation: everything RFC 9420 requires of a ratchet tree that arrived from
// somewhere other than this client's own commit -- a Welcome, the ratchet_tree extension, or a
// connect/message epoch snapshot record. group.go calls ValidateAgainstContext on every one of
// them.
//
// Three properties decide the shape of everything below, and each is here because its absence
// is a live attack rather than a tidiness complaint.
//
// It is a SWEEP and never a spot check. Every rule in this file is stated over a set -- every
// non-blank leaf, every node, every parent, every entry of an unmerged_leaves vector, every
// intermediate between an unmerged leaf and the node listing it -- and a version of any one of
// them applied to the first element of its set accepts a tree whose only bad element sits
// anywhere else. That is the shape this project has shipped four times, so every sweep here is
// held by a test whose fixture puts the single offender at a position DERIVED from the tree and
// swept across every position the set has.
//
// It READS and never writes. The argument is MergeUpdatePath's, one door along: the tree handed
// in belongs to a caller who has not agreed to anything yet, and a validator that repaired a
// node on its way to a refusal would leave that caller holding a tree no peer sent and no epoch
// pinned. Nothing in this file calls a mutator, and the atomicity is asserted the way task 21's
// is -- the whole node array, before and after, on the refusing paths as well as the accepting
// one.
//
// It does NOT restate section 7.9.2. The parent hash rule is VerifyParentHashes' and is called
// rather than re-derived, because a second derivation is a second thing that can disagree --
// and the plan this task came from states that rule with one condition where the RFC states
// three, so the version a re-derivation here would most likely reproduce is exactly the one
// that accepts a spliced subtree. See tree_hash.go's own header for the three conditions and
// for what dropping the third costs.
package mls

import (
	"crypto/subtle"
	"errors"
	"fmt"

	"github.com/urnetwork/connect/mls/syntax"
)

// errDuplicateSignatureKey and errDuplicateEncryptionKey are ValSem101 and ValSem103 in the
// validation plan's catalogue, and that plan's errors.go owns the single declaration site for
// ErrDuplicateSignatureKey and ErrDuplicateEncryptionKey. Neither name has landed in this
// package yet, so the two refusals are carried by these unexported values until they do.
//
// Unexported on extension.go's terms and for extension.go's reason, restated because this file
// adds the sixth and seventh stand in rather than the first: an exported
// ErrDuplicateEncryptionKey declared here would be a second public declaration site for a name
// the validation plan also declares, the two would not be the same value, and a caller matching
// one would silently stop matching the other. Every consumer of these two in the tasks after
// this one -- task 24's update path key uniqueness among them -- is inside package mls, so a
// name that cannot be reached from outside costs nobody outside anything.
//
// TestNoValidationOwnedNameHasLandedBesideItsStandIn derives the owed pair from this package's
// own declarations, so it fails on the commit that lands either exported name beside its stand
// in rather than leaving the swap to anybody's memory.
var (
	errDuplicateSignatureKey  = errors.New("mls: two leaves of the tree publish the same signature key")
	errDuplicateEncryptionKey = errors.New("mls: two nodes of the tree publish the same encryption key")
)

// errGroupContextDisagreement is check 0's refusal: the caller handed ValidateAgainstContext the
// same fact twice and the two copies do not agree.
//
// It is NOT one of the validation plan's catalogued names and no ValSem states it, which is why
// it carries no stand in comment of the kind the pair above carry. The rule exists because of a
// shape this profile's own API has rather than a shape the RFC has: no other implementation
// hands a tree validator the group's ciphersuite, group id and extensions a second time, so
// nobody else has two copies that can disagree. This package does, and a second copy that
// nothing compares is a second copy that can be wrong.
//
// Unexported on the same terms as the two above -- every consumer of it in the tasks after this
// one is inside package mls -- so if the validation plan ever does catalogue this rule, the swap
// is a rename rather than a second public declaration site.
var errGroupContextDisagreement = errors.New("mls: the tree validation context and the group context disagree about the group")

// TreeValidationContext is everything a whole tree check needs that is not in the tree.
//
// It is LeafValidationContext minus the per leaf fields, because those are what this file
// supplies: the leaf index is the position the sweep is at, and the expected source is the
// leaf's own -- see validateLeaves for why that one is inferred rather than demanded.
type TreeValidationContext struct {
	// Crypto verifies every leaf signature and recomputes every parent hash and the tree hash.
	// A nil one is refused before any node is read.
	Crypto CryptoProvider

	// Suite is the group's ciphersuite, which every member's capabilities must list.
	Suite CipherSuite

	// GroupId is what the update and commit sourced leaves bind their signature to, together
	// with the leaf index the sweep is at.
	GroupId []byte

	// RequiredCaps is the group's required_capabilities extension body, or nil for a group
	// carrying none. Nil is "no requirement" and is satisfied by anything.
	RequiredCaps *RequiredCapabilities

	// GroupExtensions is the GroupContext's extensions vector, for RFC 9420 section 13.4 as
	// corrected by erratum 8745.
	GroupExtensions []Extension

	// NowMs and ClockSkewMs are the clock the key_package sourced leaves' lifetimes are judged
	// against. Zero is LeafValidationContext's documented opt out of the lifetime check and is
	// passed through unchanged rather than defaulted here, so a caller with no trustworthy
	// clock says so once instead of having one invented for it.
	NowMs       uint64
	ClockSkewMs uint64
}

// validateStructure is check 1: the node array is 2n-1 for a power-of-two n, and every occupied
// position holds the kind of node that position takes.
//
// The width is inverted through LeafCountFromNodeWidth rather than through a local %2 test,
// because that function is the tree math plan's own inverse of NodeWidth and a second statement
// of the same arithmetic here is a second thing that can disagree with it. It refuses a zero
// width and an even width under one sentinel, which is why no separate zero check stands in
// front of it: a node array of nothing is not 2n-1 for any n, so the empty tree and the
// truncated tree are one condition and a local check for the first would be a branch no input
// can reach.
//
// It OVERLAPS readNodeArray, and that overlap is the one the header does NOT cover: the
// paragraph about section 7.9.2 says why a rule is not restated here, and this is the one rule
// that is. Both doors invert the width through LeafCountFromNodeWidth and IsFullLeafCount, and
// both refuse a leaf typed node at an odd index -- the same two shared helpers and the same
// clause, so there is no second derivation here that can drift from the decoder's. What the two
// do not share is the half each ADDS, and neither is the authority for the other's:
//
//   - this door adds the BODY half. readNodeArray reads the declared NodeType and never asks
//     which of Leaf and Parent is populated, because it decoded the body that type selected. A
//     tree that reached this package any other way -- built through the setters, cloned, lifted
//     out of a connect/message snapshot record -- can hold a node whose type and body disagree,
//     and every reader of that node resolves it differently depending on which field it
//     consults: the tree hash reads one, Resolution the other.
//   - readNodeArray adds ValSem300's trailing blank rule, which this door deliberately drops.
//     That rule is stated over the ratchet_tree extension's stripped array; by the time a tree
//     is a node array in memory it has been extended to its full width, so a trailing blank
//     here is an ordinary blank leaf and refusing it would refuse every group whose last member
//     was removed.
//
// So a tree that arrived by DECODE has been past both doors and a tree that arrived any other
// way has been past only this one, which is why neither can be deleted in favour of the other.
// TestValidateRefusesANodeThatIsBothKindsOrNeither is the half only this door holds.
func (self *RatchetTree) validateStructure() error {
	width := self.NodeWidth()
	leafWidth, err := LeafCountFromNodeWidth(width)
	if err != nil {
		return fmt.Errorf("%w: a node array of %d entries is not 2n-1 for any leaf count n",
			ErrTreeMalformed, width)
	}
	// fullness is a SEPARATE refusal from oddness and is not implied by it. The ratchet_tree
	// extension travels with its trailing blanks stripped, so an array of 11 nodes inverts
	// cleanly to 6 leaves -- odd width, honest arithmetic, and a shape no group is ever in.
	// Every direct path, root and parent hash below is computed against this leaf width, and
	// section 7.7 keeps a group at a power-of-two width, so a tree admitted here at width 6
	// would surface as an arithmetic refusal attributed to whichever check reached it first.
	if !IsFullLeafCount(leafWidth) {
		return fmt.Errorf("%w: %d nodes describe %d leaves and a tree's leaf width is a power of two",
			ErrTreeMalformed, width, leafWidth)
	}
	for x := uint32(0); x < width; x += 1 {
		node := self.nodes[x]
		if node == nil {
			continue
		}
		// both halves of "matches its position": the declared NodeType, and which of the two
		// bodies is present. A node carrying a parent body under a leaf type is a node every
		// reader of this tree resolves differently depending on which field it consults, and
		// the tree hash consults one while Resolution consults the other. Node's own comment
		// says exactly one of Leaf and Parent is set; this is the door that holds it for a tree
		// that was decoded rather than built through SetLeaf and SetParent.
		if NodeIndex(x).IsLeaf() {
			if node.NodeType != NodeTypeLeaf || node.Leaf == nil || node.Parent != nil {
				return fmt.Errorf("%w: node %d is a leaf position", ErrNodeTypeMismatch, x)
			}
			continue
		}
		if node.NodeType != NodeTypeParent || node.Parent == nil || node.Leaf != nil {
			return fmt.Errorf("%w: node %d is a parent position", ErrNodeTypeMismatch, x)
		}
	}
	return nil
}

// validateLeaves is check 2: every non-blank leaf passes RFC 9420 section 7.3 at its own index.
//
// The source is the one rule of section 7.3 this call site does not judge, and it SAYS SO --
// LeafValidationContext.SourceIsNotJudgedHere -- rather than inferring an expectation. A settled
// tree legally holds all three sources at once -- key_package under a member who was added and has
// not committed since, update under one who refreshed itself, commit under whoever last committed
// a path -- so there is no single source a whole tree sweep could expect.
//
// IT USED TO INFER ONE, and the difference is not a matter of spelling. The call below read
// `ExpectedSource: leaf.LeafNodeSource`, the expectation taken from the leaf being judged, so
// Validate's comparison was `x != x` for every input and ErrLeafNodeSourceMismatch could not fire
// from this door at all -- a check that reported clean having compared nothing, at a call site that
// read like a door. Saying nothing is defensible here and saying it that way was not.
//
// What survives the waiver is every other rule, and the signature BINDING is the one to read
// twice, because it is NOT uniform across the three sources. signatureContent puts the group id
// and the leaf index into the preimage under update and commit, so a leaf of either of those
// sources, lifted from another group or from another index of this one, is refused here. Under
// key_package that arm of the section 7.2 select is EMPTY -- no group id and no leaf index -- so
// a key_package leaf verifies at every index of every group, and this door accepts it wherever
// it sits. That is RFC 9420 as written and not a defect of this file; it is written down here
// because it is what makes the waiver a COST rather than a free choice, and because a reader
// deciding whether a per position source rule is still owed would otherwise have to reconstruct
// it from leaf_node.go's select.
// TestTheIndexAndGroupBindingIsUpdateAndCommitsAndNotKeyPackages measures both halves of it.
//
// What holds a key_package leaf in place instead is every rule that is not a signature: check 3
// refuses a repeated signature key, so one signed leaf cannot occupy two positions at once, and
// an expired one is refused by the lifetime clause below. The per position source rule -- "this
// position takes commit" -- is owed by the doors that know the position's history, where a
// key_package leaf turning up at a position that has just committed is refusable. This door
// cannot state it and does not pretend to.
//
// BOTH OF THOSE DOORS EXIST NOW, which is worth saying here because this paragraph is where a
// reader comes to find out. validate_proposals.go's validateUpdateLeafNodeIsValidForAnUpdate is
// section 12.1.2's: it expects the update source at the sender's own index under the group's own
// id. treekem.go's ValidateUpdatePathLeafNode is the commit half: a RECEIVED UpdatePath's leaf,
// expected to carry the commit source at the sender's position, under the group id, the suite and
// the group context extensions the receiver holds.
// TestEveryLeafNodeSourceEitherHasAValidationDoorOrAnAdmittedGap derives the source class and the
// door class off the call sites of the validator and holds them against each other, so a door
// deleted is a failure rather than a sentence that goes quietly out of date.
//
// An unknown source is refused here too, and by the signature rather than by the waived rule:
// marshalCore has no arm for a fourth source, so the preimage cannot be built and VerifySignature
// answers ErrTreeMalformed. That is what this sweep gets for free from verifying the signature at
// every leaf, and it is why waiving the source rule is not the same as accepting any octet.
func (self *RatchetTree) validateLeaves(ctx *TreeValidationContext) error {
	for i := uint32(0); i < uint32(self.LeafWidth()); i += 1 {
		leaf := self.Leaf(LeafIndex(i))
		if leaf == nil {
			continue
		}
		err := leaf.Validate(&LeafValidationContext{
			Crypto:  ctx.Crypto,
			Suite:   ctx.Suite,
			GroupId: ctx.GroupId,
			// the position IS demanded -- it is half of what an update and a commit leaf bind
			// their signature to -- and only the SOURCE is waived. See the header.
			LeafIndex:             LeafIndex(i),
			SourceIsNotJudgedHere: true,
			RequiredCaps:          ctx.RequiredCaps,
			GroupExtensions:       ctx.GroupExtensions,
			NowMs:                 ctx.NowMs,
			ClockSkewMs:           ctx.ClockSkewMs,
		})
		if err != nil {
			return fmt.Errorf("leaf %d: %w", i, err)
		}
	}
	return nil
}

// validateKeyUniqueness is check 3: no encryption key appears at two nodes of the tree, leaf and
// parent alike, and no signature key appears at two leaves.
//
// A repeated encryption key is not a cosmetic duplicate. A commit seals one path secret per
// entry of a resolution, so two nodes publishing one key means one HPKE private key opens two
// positions, and whoever holds it reads a path secret sealed to a node they are not on. A
// repeated signature key is the same statement about identity: two leaves signing as one member
// make "which member sent this" unanswerable, and removing one of them leaves the other still
// speaking under that key.
//
// Both maps are keyed by the key BYTES rather than by an index, so this is a sweep over every
// ordered pair of the tree without being written as one: the first repeat of a value already
// seen is a refusal wherever in the array the two sit.
func (self *RatchetTree) validateKeyUniqueness() error {
	encryptionKeys := map[string]uint32{}
	signatureKeys := map[string]uint32{}
	for x := uint32(0); x < self.NodeWidth(); x += 1 {
		node := self.nodes[x]
		if node == nil {
			continue
		}
		var encryptionKey []byte
		switch {
		case node.Leaf != nil:
			encryptionKey = node.Leaf.EncryptionKey
			if first, seen := signatureKeys[string(node.Leaf.SignatureKey)]; seen {
				return fmt.Errorf("%w: nodes %d and %d", errDuplicateSignatureKey, first, x)
			}
			signatureKeys[string(node.Leaf.SignatureKey)] = x
		case node.Parent != nil:
			encryptionKey = node.Parent.EncryptionKey
		default:
			// an occupied position with neither body, which validateStructure refuses ahead of
			// this. Written rather than left to a nil dereference, because this is one clause
			// of a sweep and a later caller may reach it by another route.
			return fmt.Errorf("%w: node %d is occupied and holds neither a leaf nor a parent",
				ErrNodeTypeMismatch, x)
		}
		if first, seen := encryptionKeys[string(encryptionKey)]; seen {
			return fmt.Errorf("%w: nodes %d and %d", errDuplicateEncryptionKey, first, x)
		}
		encryptionKeys[string(encryptionKey)] = x
	}
	return nil
}

// validateUnmergedLeaves is check 4: every unmerged_leaves vector is strictly ascending, every
// entry is a non-blank leaf in the subtree of the node listing it, and every node strictly
// between that leaf and that node is blank or lists the same leaf.
//
// The last clause is the one easiest to leave out and the one the rest depends on. A leaf is
// unmerged AT a node when its member does not hold that node's key, and for the member's own
// direct path that has to hold all the way up: an intermediate that is non-blank and does NOT
// list the leaf asserts that the member holds that intermediate's key while not holding its
// ancestor's, which no sequence of commits can produce. What a tree violating it buys an
// attacker is a resolution that differs from the one an honest sender computes, so the next
// commit seals a path secret to a set of keys the two sides do not agree on.
//
// The sweep is over every odd index and no even one, which is exactly the parent positions (RFC
// 9420 appendix C), derived from the width rather than from a list of the parents somebody
// expected to be non-blank -- the same derivation VerifyParentHashes uses next door.
func (self *RatchetTree) validateUnmergedLeaves() error {
	for x := uint32(1); x < self.NodeWidth(); x += 2 {
		node := NodeIndex(x)
		parent := self.ParentAt(node)
		if parent == nil {
			continue
		}
		for i, leaf := range parent.UnmergedLeaves {
			if i > 0 && parent.UnmergedLeaves[i-1] >= leaf {
				return fmt.Errorf("%w: node %d lists leaf %d after leaf %d",
					ErrUnmergedLeavesNotSorted, x, leaf, parent.UnmergedLeaves[i-1])
			}
			if LeafCount(leaf) >= self.LeafWidth() {
				return fmt.Errorf("%w: node %d lists leaf %d and the tree has %d leaves",
					ErrUnmergedLeafInconsistent, x, leaf, self.LeafWidth())
			}
			// a blank leaf is a member who is GONE, and a node still listing them is a node
			// whose resolution the removal was supposed to have changed. tree.go drops such
			// entries as it blanks a leaf, so a tree carrying one was not built here.
			if self.Leaf(leaf) == nil {
				return fmt.Errorf("%w: node %d lists leaf %d, which is blank",
					ErrUnmergedLeafInconsistent, x, leaf)
			}
			if !InSubtree(node, leaf.NodeIndex()) {
				return fmt.Errorf("%w: node %d lists leaf %d, which is not under it",
					ErrUnmergedLeafInconsistent, x, leaf)
			}
			path, err := directPathOf(leaf.NodeIndex(), self.LeafWidth())
			if err != nil {
				return err
			}
			for _, intermediate := range path {
				if intermediate == node {
					break
				}
				between := self.ParentAt(intermediate)
				if between == nil {
					continue
				}
				if !unmergedLeavesListLeaf(between.UnmergedLeaves, leaf) {
					return fmt.Errorf("%w: node %d lists leaf %d and node %d between them does not",
						ErrUnmergedLeafInconsistent, x, leaf, intermediate)
				}
			}
		}
	}
	return nil
}

// unmergedLeavesListLeaf is membership in one unmerged_leaves vector, as a linear scan.
//
// Linear and not a binary search over the ascending order, deliberately: that order is a
// property this file is in the middle of CHECKING, and a search that assumed it would answer
// "absent" for a leaf that is present in an unsorted vector -- turning a sortedness failure at
// one node into a consistency failure reported against another.
func unmergedLeavesListLeaf(leaves []LeafIndex, leaf LeafIndex) bool {
	for _, candidate := range leaves {
		if candidate == leaf {
			return true
		}
	}
	return false
}

// Validate answers nil only if this tree satisfies every RFC 9420 rule that can be decided from
// the tree and this context alone. The one that cannot -- the binding to the epoch that pinned
// it -- is ValidateAgainstContext's.
//
// The order is structural, then per node, then whole tree, and it is load bearing rather than
// tidy. validateStructure is what makes the node array indexable and every node's body the one
// its position takes, so every check after it may read self.nodes[x] without asking again; and
// the parent hash sweep is LAST because it is the only check that hashes, so a tree refused for
// any cheaper reason never pays for it.
//
// A tree with NO non-blank leaf at all is ACCEPTED here, at every width, and that is recorded
// rather than left to be found. Every check above is vacuous on it: the width still inverts, the
// leaf sweep has nothing to judge, both key maps stay empty, no parent slot is occupied and
// VerifyParentHashes has no claimant to find. It is not a gap in the five -- "a group has at
// least one member" is a rule about a GROUP and not about a node array, and the tree a removal
// empties is a legal intermediate that a refusal here would turn into a decode failure. The door
// that owes the refusal is the one that turns a tree into a group, which is group.go's Welcome
// and snapshot paths, and it does not exist yet.
// TestValidateAcceptsATreeWithNoMembersAtEveryWidth is what makes that acceptance a decision
// somebody wrote down rather than a hole nothing names.
func (self *RatchetTree) Validate(ctx *TreeValidationContext) error {
	if err := usableValidationContext(ctx); err != nil {
		return err
	}
	if err := self.validateStructure(); err != nil {
		return err
	}
	if err := self.validateLeaves(ctx); err != nil {
		return err
	}
	if err := self.validateKeyUniqueness(); err != nil {
		return err
	}
	if err := self.validateUnmergedLeaves(); err != nil {
		return err
	}
	return self.VerifyParentHashes(ctx.Crypto)
}

// usableValidationContext is the refusal a missing provider gets, in the one place both doors
// reach it from.
//
// One spelling and not two. ValidateAgainstContext has to make this refusal BEFORE it reads
// ctx.Suite for check 0, so it can no longer inherit Validate's; and a second copy of the
// refusal is a second thing that can come to answer differently about the same missing thing,
// which is exactly the failure LeafNode.Validate's own nil check is written to avoid one layer
// down. A nil context is a context whose provider is nil and gets the same answer, so
// Validate(nil) and Validate(&TreeValidationContext{}) cannot diverge.
func usableValidationContext(ctx *TreeValidationContext) error {
	if ctx == nil || ctx.Crypto == nil {
		return fmt.Errorf("%w: every leaf signature and every parent hash of this tree is taken through it",
			ErrNilCryptoProvider)
	}
	return nil
}

// reconcileWithGroupContext is check 0: the facts these two structures BOTH carry agree.
//
// ValidateAgainstContext is handed the group twice. GroupContext carries the group id, the
// ciphersuite and the extensions vector; TreeValidationContext restates all three, and it is the
// restatement the leaves are judged against -- validateLeaves passes ctx.GroupId into every
// signature preimage, ctx.Suite into every capabilities check and ctx.GroupExtensions into
// section 13.4's clause. Nothing compared the two copies, and the TREE HASH CANNOT: it is a hash
// of nodes and covers none of those three fields, so the two could disagree arbitrarily and
// check 6 would still answer nil. A tree whose leaves were validated under one group id while
// the epoch pinned another was accepted with every check having answered yes.
//
// That is ValidateAgainstContext's own argument turned on the other input. Its doc says a tree
// that validates on its own and hashes to something else is a different group's tree; a tree
// whose leaves were judged against a different group's facts is the same statement, and this
// function is the only place in the package holding both values at once.
//
// It runs BEFORE Validate, which is the order the rest of this file keeps: three comparisons and
// one vector walk against a signature verification per leaf and a hash per parent, and a caller
// that handed in two disagreeing structures has a bug no amount of verifying changes the answer
// to.
func reconcileWithGroupContext(ctx *TreeValidationContext, gc *GroupContext) error {
	if ctx.Suite != gc.CipherSuite {
		return fmt.Errorf("%w: the leaves are judged against ciphersuite %#04x and the epoch pins %#04x",
			errGroupContextDisagreement, uint16(ctx.Suite), uint16(gc.CipherSuite))
	}
	// through subtle for the reason the tree hash comparison below is written that way: every
	// comparison in this package that decides whether a structure is ADOPTED is spelled the one
	// way, so no later reader has to work out which of them were the safe ones.
	if subtle.ConstantTimeCompare(ctx.GroupId, gc.GroupId) != 1 {
		return fmt.Errorf("%w: the leaves are judged under group id %x and the epoch pins %x",
			errGroupContextDisagreement, ctx.GroupId, gc.GroupId)
	}
	// EVERY entry and in order, because ctx.GroupExtensions is not a set resembling the group's
	// vector -- it IS that vector, and section 13.4's clause is stated over the whole of it. A
	// comparison of lengths alone, or of the first entry, would let a swapped pair or an altered
	// body through, and a body is what carries required_capabilities.
	//
	// The length refusal is also what the loop below STANDS ON: it indexes both vectors through
	// one range, so it is in bounds because the lengths were equal three lines earlier and for
	// no other reason. Moving or weakening this comparison is not a laxer check, it is a panic.
	if len(ctx.GroupExtensions) != len(gc.Extensions) {
		return fmt.Errorf("%w: the leaves are judged against %d group extension(s) and the epoch pins %d",
			errGroupContextDisagreement, len(ctx.GroupExtensions), len(gc.Extensions))
	}
	for i := range gc.Extensions {
		if ctx.GroupExtensions[i].ExtensionType != gc.Extensions[i].ExtensionType {
			return fmt.Errorf("%w: group extension %d is %#04x to the leaves and %#04x to the epoch",
				errGroupContextDisagreement, i, uint16(ctx.GroupExtensions[i].ExtensionType),
				uint16(gc.Extensions[i].ExtensionType))
		}
		if subtle.ConstantTimeCompare(ctx.GroupExtensions[i].ExtensionData,
			gc.Extensions[i].ExtensionData) != 1 {
			return fmt.Errorf("%w: the body of group extension %d is %x to the leaves and %x to the epoch",
				errGroupContextDisagreement, i, ctx.GroupExtensions[i].ExtensionData,
				gc.Extensions[i].ExtensionData)
		}
	}
	return reconcileRequiredCapabilities(ctx.RequiredCaps, gc.Extensions)
}

// reconcileRequiredCapabilities is the fourth fact both structures carry, and the only one that
// is a BODY inside the vector rather than a field beside it.
//
// Pinning the extensions vector byte for byte above makes the two agree about the required
// capabilities BYTES. It says nothing about the structure the caller parsed those bytes into,
// and that structure is what every leaf is actually held to: ctx.RequiredCaps is what reaches
// Capabilities.Supports, so a caller that passed nil for a group whose context requires an
// extension gets every leaf admitted without the requirement being applied at all -- a member
// admitted who cannot read what the group sends, which is the consequence section 11.1 states
// the rule for.
//
// Reconciled by ENCODING the structure and comparing bytes rather than by decoding the body,
// because encoding is total and decoding is not: a malformed body would otherwise turn a
// disagreement into a decode error attributed to this check, and a required_capabilities carried
// with an empty body -- which is not three empty vectors, it is nothing -- would need a case of
// its own. Absence is reconciled too and in both directions: a nil rc is "requires nothing" and
// an absent extension is the same statement, so the two must not be able to differ.
//
// The lookup REFUSES a group context carrying two required_capabilities rather than answering
// the first, and that refusal is worth more here than at either accessor. This runs under
// reconcileWithGroupContext, under the exported (*RatchetTree).ValidateAgainstContext, whose
// *GroupContext came off the wire: a peer that sent two would otherwise choose which one this
// client reconciles against, and so which one every leaf is held to, at the validation entry
// point whose job is to refuse exactly that. Answering the first is not safe by accident here
// either, because an absent required_capabilities is read three lines down as "this group
// requires nothing" -- so a repeat reported as absence would be ACCEPTED rather than refused.
//
// The refusal is not a disagreement between the two structures and so does not carry
// errGroupContextDisagreement: nothing here disagrees, the group context is malformed.
func reconcileRequiredCapabilities(required *RequiredCapabilities, groupExtensions []Extension) error {
	body, carried, err := FindExtension(groupExtensions, ExtensionTypeRequiredCapabilities)
	if err != nil {
		return fmt.Errorf("the group context carries required_capabilities more than once: %w", err)
	}
	if !carried {
		if required != nil {
			return fmt.Errorf("%w: the leaves are held to a required_capabilities the epoch does not carry",
				errGroupContextDisagreement)
		}
		return nil
	}
	if required == nil {
		return fmt.Errorf("%w: the epoch carries a required_capabilities and the leaves are held to none",
			errGroupContextDisagreement)
	}
	encoded, err := syntax.Marshal(required)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(encoded, body) != 1 {
		return fmt.Errorf("%w: the leaves are held to a required_capabilities of %x and the epoch carries %x",
			errGroupContextDisagreement, encoded, body)
	}
	return nil
}

// ValidateAgainstContext is Validate plus check 0 and check 6: the facts the caller holds twice
// agree, and this tree is the tree the GroupContext pinned.
//
// The tree hash is what every epoch secret and every signature of the epoch is bound to, so a
// tree that validates on its own and hashes to something else is a DIFFERENT group's tree,
// however sound it is. Without this a joiner handed a well formed tree from a fork would derive
// an epoch nobody else is in and report nothing at all.
//
// Check 0 is the same argument made about the facts rather than about the nodes, and it runs
// first; see reconcileWithGroupContext for why the tree hash cannot stand in for it.
func (self *RatchetTree) ValidateAgainstContext(ctx *TreeValidationContext, gc *GroupContext) error {
	// no context is no pin, and a tree with nothing to be checked against has not passed this
	// check, it has skipped it. Refused rather than dereferenced, so a missing argument cannot
	// read as a match.
	if gc == nil {
		return fmt.Errorf("%w: there is no group context for this tree to be checked against",
			ErrTreeHashMismatch)
	}
	if err := usableValidationContext(ctx); err != nil {
		return err
	}
	if err := reconcileWithGroupContext(ctx, gc); err != nil {
		return err
	}
	if err := self.Validate(ctx); err != nil {
		return err
	}
	treeHash, err := self.TreeHash(ctx.Crypto)
	if err != nil {
		return err
	}
	// through subtle for guardrail 8's reason. A tree hash is a public value, but every
	// comparison in this package that decides whether a structure is ADOPTED is written the one
	// way, so no later reader has to work out which of them were the safe ones. Unequal lengths
	// answer zero here, which is what makes an absent or truncated tree_hash a mismatch rather
	// than a panic.
	if subtle.ConstantTimeCompare(treeHash, gc.TreeHash) != 1 {
		return fmt.Errorf("%w: the tree hashes to %x and the group context carries %x",
			ErrTreeHashMismatch, treeHash, gc.TreeHash)
	}
	return nil
}

// ---------------------------------------------------------------------------
// ValSem206 and ValSem207: the keys an UpdatePath introduces
// ---------------------------------------------------------------------------

// CheckUpdatePathKeyUniqueness is ValSem206 and ValSem207 in one walk: every encryption key an
// UpdatePath introduces -- its leaf node's and every path node's -- is new to the tree and new
// within the path.
//
// Run it against the PRE-merge tree. The keys the path publishes are the keys the merge is about
// to install, so after MergeUpdatePath every one of them is in the tree and this would refuse
// every honest commit; before it, the tree holds exactly the keys the commit must not collide
// with. The group lifecycle plan's call site is the one right before the merge for that reason.
//
// A repeated encryption key is not a cosmetic duplicate, and validateKeyUniqueness' note next
// door is the same argument made about a whole tree: a commit seals one path secret per entry of
// a resolution, so two nodes publishing one key means one HPKE private key opens two positions,
// and whoever holds it reads a path secret sealed to a node they are not on. The path is where
// that gets INTRODUCED, which is why the check is owed here and not only over a tree that has
// already been assembled.
//
// The committer is RECOVERED from the path rather than passed in, and that is a safety decision
// rather than a convenience. The sender's own outgoing leaf key is the one key the path is
// replacing, so it must not count as a collision; a commit never changes the committer's
// signature key, so the leaf carrying path.LeafNode.SignatureKey is the leaf being replaced.
// Taking the sender as an argument would give every call site one more thing to get wrong, and
// getting it wrong points the exemption at somebody ELSE's leaf -- which turns a real ValSem206
// into a pass, silently, for exactly the leaf whose key was stolen. There is no argument here to
// get wrong.
//
// A path whose signature key matches no leaf is not from a member. Nothing is being replaced
// then, so no key is exempt and every key in it must be new -- including one copied from the
// leaf the stranger is pretending to be.
//
// THE EXEMPTION IS ONE KEY AT ONE POSITION, and both halves of that are load bearing.
//
// The key is the committer's outgoing leaf key, and the leaf it sits at is located by NODE
// index: leaf L lives at node 2L, so an exemption comparing against L raw points at somebody
// else's node for every committer except leaf 0 -- exempting a leaf whose key was NOT replaced
// while refusing the honest commit of the leaf that was. A fixture whose committer is always the
// group's first member cannot tell the two apart, because for leaf 0 the two numbers are equal,
// and this function shipped with that blind spot for exactly that reason.
// TestUpdatePathKeyUniquenessExemptsTheCommittersLeafAndNoOther commits from every occupied leaf
// of its tree rather than from one.
//
// The position is the path's LEAF NODE, which is the only position that replaces anything. A
// path NODE is a parent position: it installs nothing at the committer's leaf, so the outgoing
// key appearing there is a retired key republished somewhere it never lived, and it is refused
// like any other collision. That is why the exemption is written at the COMPARISON rather than
// as a hole in the tree sweep -- a hole in the sweep takes the key out of the set entirely, so
// it silently exempts the path's other len(Nodes) positions too, and no errors.Is assertion
// anywhere can see the difference. TestUpdatePathKeyUniquenessExemptsOnlyThePathsLeafPosition is
// the half that names it.
//
// Both halves are sweeps over a SET, in the shape this file's header argues for: the tree side
// reads every occupied position, leaf and parent alike, and the path side reads every node it
// publishes rather than its first. A version of either bounded to the first element accepts a
// path whose only stolen key sits anywhere else, which is the defect this project has shipped
// four times.
//
// The tree side reading PARENTS is wider than Spec A's own wording for these two rows, which say
// "unique among proposals and members", and the widening is deliberate: a path key that is
// already some parent's is the one-private-key-two-positions defect the paragraph above
// describes, and refusing it costs an honest commit nothing, since every key a path publishes
// was derived from a fresh path secret. The PROPOSALS axis is not visible from in here at all --
// this function sees one tree and no proposal list. It is the CALLER that puts the proposals in
// range, by handing over the post-proposal tree, which is what the group lifecycle plan's call
// site does. So the class stated here is "every key in the tree it was given", and the caller's
// choice of tree is what makes that the spec's class.
//
// THREE SENTINELS come out of here and they are deliberately not one: errDuplicateEncryptionKey
// for a collision, ErrTreeMalformed for a missing tree, ErrNodeTypeMismatch for an occupied node
// carrying no body. The last two are faults of the TREE and the first is a fault of the PATH, so
// a caller funnelling this into a ValSem-named error must wrap the inner error with %w and not
// %v: %v flattens the chain, and then a malformed tree is reported to the group as a commit that
// duplicated an encryption key, which names the wrong party for a fault that is not the
// committer's. TestUpdatePathKeyUniquenessKeepsItsRefusalsDistinct derives the set of sentinels
// from this function's own body and holds them apart.
func CheckUpdatePathKeyUniqueness(tree *RatchetTree, path *UpdatePath) error {
	// refused rather than dereferenced, so a missing argument cannot read as "nothing collided".
	// The tree is what the path is judged AGAINST, and a nil one has not passed this check, it
	// has skipped it -- ValidateAgainstContext refuses a missing group context for the same
	// reason and answers the sentinel of the thing that was absent.
	if tree == nil {
		return fmt.Errorf("%w: there is no tree for this update path's keys to be checked against",
			ErrTreeMalformed)
	}
	if path == nil {
		return errNilUpdatePath
	}
	replaced, isMember := tree.FindLeafBySignatureKey(path.LeafNode.SignatureKey)
	// keyed by the key BYTES rather than by an index, which is validateKeyUniqueness' spelling
	// and makes this a sweep over every pair without being written as one. The value is EVERY
	// node holding that key rather than the last one to write it: a tree that already carries a
	// duplicate would otherwise hide one occurrence behind the other, and the hidden one could
	// be the occurrence the exemption below does not cover -- so a stolen key would ride into
	// the group behind the committer's own leaf.
	existing := map[string][]uint32{}
	for x := uint32(0); x < tree.NodeWidth(); x += 1 {
		node := tree.Get(NodeIndex(x))
		if node == nil {
			continue
		}
		var encryptionKey []byte
		switch {
		case node.Leaf != nil:
			encryptionKey = node.Leaf.EncryptionKey
		case node.Parent != nil:
			encryptionKey = node.Parent.EncryptionKey
		default:
			// an occupied position with neither body. validateStructure refuses it ahead of
			// every caller that has validated its tree, and this function is reachable from
			// callers that have not, so it is answered rather than left to a nil dereference.
			return fmt.Errorf("%w: node %d is occupied and holds neither a leaf nor a parent",
				ErrNodeTypeMismatch, x)
		}
		existing[string(encryptionKey)] = append(existing[string(encryptionKey)], x)
	}
	// the leaf first and then the path nodes in published order, so the position a refusal names
	// is the position the encoder wrote rather than an index into a reordered copy. atTheLeaf
	// rather than a comparison against the where string, because the exemption below is a rule
	// about a POSITION and a rule keyed on prose breaks silently the day the prose is reworded.
	type introducedKey struct {
		where     string
		atTheLeaf bool
		key       []byte
	}
	introduced := make([]introducedKey, 0, len(path.Nodes)+1)
	introduced = append(introduced, introducedKey{
		where:     "the path's leaf node",
		atTheLeaf: true,
		key:       path.LeafNode.EncryptionKey,
	})
	for i := range path.Nodes {
		introduced = append(introduced, introducedKey{
			where: fmt.Sprintf("path node %d", i),
			key:   path.Nodes[i].EncryptionKey,
		})
	}
	seen := map[string]string{}
	for _, one := range introduced {
		// every node holding this key, ascending, so the refusal names the lowest position the
		// key really sits at rather than whichever one happened to be written last.
		for _, at := range existing[string(one.key)] {
			// the one exemption, decided here rather than while the tree was read so that it
			// covers exactly the pair it is an exemption for: the path's leaf node
			// republishing the key at the committer's own leaf.
			if isMember && one.atTheLeaf && NodeIndex(at) == replaced.NodeIndex() {
				continue
			}
			return fmt.Errorf("%w: %s publishes the key already at node %d",
				errDuplicateEncryptionKey, one.where, at)
		}
		if first, repeated := seen[string(one.key)]; repeated {
			return fmt.Errorf("%w: %s publishes the same key as %s",
				errDuplicateEncryptionKey, one.where, first)
		}
		seen[string(one.key)] = one.where
	}
	return nil
}
