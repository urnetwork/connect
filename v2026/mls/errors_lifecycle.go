// The group lifecycle refusals: the policy this profile adds on top of RFC 9420, as one
// sentinel per rule, plus the two membership caps every client enforces.
//
// These are NOT ValSem codes. RFC 9420's own validation refusals live beside the structures
// they judge -- framing_errors.go, tree_errors.go, errors_key_schedule.go -- and are owned by
// the plans that wrote those. What is here is the layer above: MASTER sections 6, 8 and 11 say
// how big a group may be, how many device leaves one identity may hold, who may remove whom,
// and how an owner is succeeded, and none of that is in the RFC at all. A caller reading a
// refusal from this file learns that a well formed, correctly signed message was refused by
// this profile's policy rather than by the protocol.
//
// One value per rule and never one value shared by two, for the reason tree_errors.go states:
// errors.Is cannot tell two rules apart when they answer the same value, so a test asserting
// the broad question passes over a rule that fired for the wrong reason. Tasks 3 through 21
// wire these to their call sites; until then each is held by TestLifecycleErrorsAreDistinct,
// which is also what puts them inside the package's refusal roster the moment a production
// error result starts answering them.
//
// PastEpochWindow is NOT declared here. It is the key schedule's, declared once in
// key_schedule.go, and this plan only references it: spec A section 4.3 runs
// DeleteGroupStateBefore(epoch - PastEpochWindow) on every merged commit, which is what makes
// MASTER section 8.1's ephemeral guarantee real. TestPastEpochWindowDropsOlderState is what says
// this build runs it, reading that cutoff in both directions -- the epoch at it survives and the
// one below it is gone -- rather than leaving the arithmetic to this sentence. XwingPublicKeyLen and AlgIdXwing are TreeKEM's
// for the same reason. A second declaration of any of them in package mls is a compile error,
// not a merge inconvenience.
package mls

import "errors"

// The membership and device caps, MASTER sections 6 and 11.
//
// Both are enforced by the committing client AND by every receiving client, which is what a
// cap in a group protocol has to mean: a modified client that skips its own check produces a
// commit every honest member refuses, rather than a group that quietly grew past the bound.
const (
	MaxGroupMembers            = 500
	MaxDeviceLeavesPerIdentity = 10
)

var (
	// membership policy, MASTER section 6
	ErrGroupSizeExceeded      = errors.New("mls: commit would exceed the 500 member group cap")
	ErrDeviceLimitExceeded    = errors.New("mls: commit would exceed the 10 device leaves per identity cap")
	ErrAdminRemovedByNonOwner = errors.New("mls: only an owner may remove an admin or the owner")

	// owner succession, MASTER section 11
	ErrSuccessionDisabled      = errors.New("mls: succession is disabled for this group")
	ErrSuccessionNotNominee    = errors.New("mls: committer is not the nominated successor")
	ErrSuccessionQuorum        = errors.New("mls: succession countersignature quorum not met")
	ErrSuccessionFloor         = errors.New("mls: succession floor has not elapsed since the owner was last active")
	ErrSuccessionFloorTooShort = errors.New("mls: succession floor is shorter than the 90 day minimum")

	// the urmessage group extensions
	ErrNoGroupPolicy      = errors.New("mls: group context carries no urmessage_group_policy extension")
	ErrMalformedExtension = errors.New("mls: extension body is malformed")
	ErrDuplicateRoleEntry = errors.New("mls: duplicate member id in the group policy roles")
	ErrRolesNotCanonical  = errors.New("mls: group policy roles are not sorted by member id")
	ErrNoOwner            = errors.New("mls: group policy names no owner")
	ErrMultipleOwners     = errors.New("mls: group policy names more than one owner")

	// welcome processing, RFC 9420 section 12.4.3 as this profile constrains it
	ErrWelcomeNoMatchingKeyPackage = errors.New("mls: welcome carries no secrets entry for any held key package")
	ErrWelcomeGroupInfoDecrypt     = errors.New("mls: welcome group info did not decrypt")
	ErrWelcomeGroupInfoSignature   = errors.New("mls: welcome group info signature is invalid")
	ErrWelcomeTreeHashMismatch     = errors.New("mls: ratchet tree hash does not match the group info")
	ErrWelcomeLeafNotFound         = errors.New("mls: own leaf node is not present in the ratchet tree")
	ErrWelcomeSuiteMismatch        = errors.New("mls: welcome ciphersuite does not match the key package")

	// ErrWelcomeCarriedTreeMismatch names a GroupInfo whose own ratchet_tree extension
	// describes a tree other than the one it was verified against. Its whole account -- what
	// the rule buys, and the forgery it does NOT stop -- is on (*GroupInfo).Verify under
	// "WHAT RULE 9 BUYS AND WHAT IT DOES NOT", because the rule is only safe to read together
	// with the precondition stated above it.
	ErrWelcomeCarriedTreeMismatch = errors.New("mls: group info carries a ratchet_tree extension for a different tree")

	// the group state machine this client runs
	ErrGroupIdInUse        = errors.New("mls: group id is already in use by this client")
	ErrPendingCommitExists = errors.New("mls: a pending commit is already staged")
	ErrNoPendingCommit     = errors.New("mls: no pending commit is staged")
	ErrEpochStale          = errors.New("mls: message epoch is older than the current epoch")
	ErrRemovedFromGroup    = errors.New("mls: this client was removed by the commit")

	// the pairwise exporter, (*Group).PairwiseExport.
	//
	// They are POLICY refusals, which is why they are here rather than in crypto_errors.go or
	// tree_errors.go: neither names a malformed key or a malformed tree. A caller that asked for
	// a key with its OWN leaf, and a caller that named a position holding no member, each asked
	// for a two party key where there is no second party -- this profile's rule about who a
	// pairwise key may be derived WITH, exactly as the block at the top of this file is its rule
	// about who may be in a group at all.
	//
	// TWO VALUES AND NOT ONE, for this file's own stated reason: a caller triaging "I named my
	// own leaf" against "I named a blank position" is triaging a bug in its own leaf arithmetic
	// against a member that has left, and errors.Is cannot tell those apart through one value.
	ErrPairwiseSelf = errors.New("mls: a pairwise key with one's own leaf is not a pairwise key")
	ErrBlankLeaf    = errors.New("mls: the leaf named holds no member at this epoch")
)
