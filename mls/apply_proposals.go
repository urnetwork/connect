// RFC 9420 section 12.3, applying a proposal list to a ratchet tree and a group context.
//
// THE ORDER HERE IS NORMATIVE AND IT IS THE WHOLE OF WHAT THIS FILE DECIDES. Section 12.3 fixes
// it: GroupContextExtensions first, then Updates in any order, then Removes in any order, then
// Adds IN THE ORDER THEY APPEAR IN THE LIST. Each of those clauses is a fork if it is got wrong,
// and none of them is visible from a list that happens to be sorted.
//
//   - Adds last, because section 12.1.1 places an added member at the leftmost blank leaf. A
//     Remove applied after an Add fills a different gap than the same Remove applied before it, so
//     the two orders build different trees, different tree hashes and different confirmed
//     transcripts -- a split, not an error.
//   - Adds in LIST order rather than in bucket order, for the same reason at one level down: two
//     commits carrying the same set of adds in a different order place those members on different
//     leaves. This is why ProposalList keeps All beside its buckets at all.
//   - GroupContextExtensions first, because section 12.3 says the new extensions MUST be used when
//     evaluating the rest of the list -- a required_capabilities added by the same commit is a
//     requirement its own Adds have to meet.
//
// An implementation that walked the list in commit order passes every test whose list happens to
// be sorted remove-then-add, which is what most fixtures look like.
// TestApplyProposalsAppliesTheRfcOrderAndNotTheListOrder is built the other way round on purpose.
//
// THIS TAKES NO CryptoProvider, and that is the one place this file's signature is not the plan's.
// The plan pins ApplyProposals(crypto, tree, ctx, own, list) and justifies the provider by
// (*RatchetTree).TreeHash -- which section 12.3 does not compute, and which section 12.4.2 computes
// only AFTER the update path has been merged, so a hash taken here would be a field nothing reads.
// A provider parameter no body reaches is not free in this package: package level constructions
// taking a CryptoProvider are a class the crypto gates derive off the type checker, and every one
// of them is called, perturbed and required to answer differently for each argument it was handed.
// Getting this one through them takes four base arguments, two perturbation rules and two
// exemptions, and the exemptions read "this construction answers nothing the gate can read and
// reaches no provider" -- which is the finding TestProviderHasNoRemainingStubs exists to make,
// written down as an excuse. So the parameter is dropped rather than excused, and the callers the
// plan writes for tasks 22, 23 and 25 pass one argument fewer.
package mls

import (
	"fmt"
	"slices"
)

// ApplyResult is the post-proposal state, on a tree the caller did not own before this call.
//
// The three index vectors are answered in the order the operations happened, which is what makes
// AddedLeaves readable as "the first Add of the commit landed here". A commit's Welcome is
// addressed to them in that order.
type ApplyResult struct {
	Tree          *RatchetTree
	Extensions    []Extension
	AddedLeaves   []LeafIndex
	RemovedLeaves []LeafIndex
	UpdatedLeaves []LeafIndex
	SelfRemoved   bool
}

// ApplyProposals clones the tree and applies the list in the RFC 9420 section 12.3 order.
//
// THE CALLER'S TREE IS NEVER MUTATED, which is the second thing this file decides and is why the
// clone is the first statement rather than an optimisation the caller could skip. A commit is
// applied and then judged -- section 12.4.2 validates the update path, the tree and the
// confirmation tag against the tree this produces -- so a version that worked in place would
// leave a member's live state half-way through a commit it went on to reject, with no way back to
// the epoch it was in.
func ApplyProposals(tree *RatchetTree, ctx *GroupContext, own LeafIndex,
	list *ProposalList) (*ApplyResult, error) {

	if tree == nil {
		return nil, fmt.Errorf("%w: there is nothing to apply them to", errNilRatchetTree)
	}
	if ctx == nil {
		return nil, fmt.Errorf("%w: the extensions a GroupContextExtensions proposal replaces are its", ErrNilGroupContext)
	}
	if list == nil {
		return nil, fmt.Errorf("%w: there is nothing to apply", errNilProposalList)
	}

	// the structural rule of validate_proposals.go, ahead of every dereference below. A proposal
	// whose named arm is not the one populated is a nil dereference at
	// `cached.Proposal.Remove.Removed` -- and a caller that applies before it validates is the
	// ordinary shape rather than a mistake, because section 12.4.2 validates the tree this
	// produces. The rule is called rather than restated so that there is one statement of it.
	//
	// IT USED TO BE THREE AND THE OTHER TWO ARE GONE WITH THE FIELDS THEY GUARDED. A list once
	// carried All and four buckets as independently writable fields, so this door also had to ask
	// that each bucket held its own type and that the buckets were the commit order bucketed --
	// the second by a per-type count, which every count-preserving disagreement satisfied.
	// (*ProposalList).Removes is now the commit order filtered, so step 3 below walks the same
	// entries step 4 does: the divergence this door used to check for cannot be constructed, and
	// the two rules have no input left to refuse.
	structural := &ProposalValidationInput{Tree: tree, Context: ctx, Committer: own, List: list}
	if err := ValSem113ProposalTypeSupported(structural); err != nil {
		return nil, err
	}
	// and the GCE bucket holds AT MOST ONE, because step 1 below decides the whole of the next
	// epoch's extension set off GCE[0]. Section 12.2 makes a list carrying two invalid and
	// (*ProposalCache).Resolve refuses the second as it buckets, so nothing this package resolves
	// reaches here with two -- but a list a caller assembled field by field does, and over that
	// list this door would install one of two extension sets with nothing saying which. That is
	// the same fault the three rules above close one field lower down: a step decided off a
	// source the door has not established.
	//
	// REDUNDANT WITH THE DOOR AND KEPT, and that is measured rather than assumed:
	// (*ProposalValidationInput).check asks this same rule, so the three calls above reach it
	// a moment earlier and deleting this line leaves the whole of ./mls/... and ./message/...
	// green. It stays for the reason those three are called rather than restated -- this file
	// names the preconditions its four numbered steps stand on, and a reader asking which
	// extension set step 1 installs should find the answer here rather than inside another
	// rule's argument check. Nothing here claims a test can tell which of the two guards fired.
	if err := validateOneGroupContextExtensions(structural); err != nil {
		return nil, err
	}

	// slices.Clone of the extension vector and not the caller's own backing array, for the
	// reason the tree is cloned: an ApplyResult is a candidate state, and a candidate that
	// shared storage with the live one could not be discarded. The bodies inside each Extension
	// are still shared, which is what every extension accessor of this package already
	// documents -- what is copied is the vector a later append would otherwise write through.
	result := &ApplyResult{Tree: tree.Clone(), Extensions: slices.Clone(ctx.Extensions)}

	// 1. GroupContextExtensions, WHOLESALE replacement rather than a merge. Section 12.1.7 is a
	//    replacement, and a merge would make an extension impossible to remove.
	if exts, ok := list.Extensions(); ok {
		result.Extensions = slices.Clone(exts)
	}

	// 2. Updates, any order. The sender's leaf is replaced and its direct path is blanked, which
	//    section 12.1.2 requires and RatchetTree.UpdateLeaf does.
	updates := list.Updates()
	for i := range updates {
		cached := &updates[i]
		if err := result.Tree.UpdateLeaf(cached.Sender, &cached.Proposal.Update.LeafNode); err != nil {
			return nil, fmt.Errorf("mls: applying the update at updates[%d] for leaf %d: %w",
				i, cached.Sender, err)
		}
		result.UpdatedLeaves = append(result.UpdatedLeaves, cached.Sender)
	}

	// 3. Removes, any order. RemoveLeaf blanks the path, blanks the leaf and truncates.
	removes := list.Removes()
	for i := range removes {
		leafIndex := removes[i].Proposal.Remove.Removed
		if err := result.Tree.RemoveLeaf(leafIndex); err != nil {
			return nil, fmt.Errorf("mls: applying the remove at removes[%d] for leaf %d: %w",
				i, leafIndex, err)
		}
		result.RemovedLeaves = append(result.RemovedLeaves, leafIndex)
		if leafIndex == own {
			result.SelfRemoved = true
		}
	}

	// 4. Adds, IN COMMIT ORDER, and the walk is over All rather than over Adds. The two now hold
	//    the same entries in the same order -- Adds is All filtered -- so this is a statement
	//    about what section 12.3 says rather than a defence against a field a caller filled in:
	//    "in the order they appear in the proposals vector" is the vector, and reading it here is
	//    what a later edit of the views cannot quietly change.
	//
	//    MEASURED: rewriting this walk to range over list.Adds() leaves the whole of ./mls/... and
	//    ./message/... green, and that is the derivation's own guarantee rather than a gap in the
	//    suite. Under the fields this type used to carry the same rewrite was a fork -- the bucket
	//    was whatever order a caller appended in, and
	//    TestApplyProposalsTakesTheAddOrderFromTheCommitOrder existed to build exactly that
	//    conflict. There is no longer an input that separates the two programs, so no test can.
	//
	//    AddLeaf places at the leftmost blank leaf and extends the tree to the right when there
	//    is none.
	order := list.All()
	for i := range order {
		cached := &order[i]
		if cached.Proposal.ProposalType != ProposalTypeAdd {
			continue
		}
		leafIndex, err := result.Tree.AddLeaf(&cached.Proposal.Add.KeyPackage.LeafNode)
		if err != nil {
			return nil, fmt.Errorf("mls: applying the add at proposal %d of the commit order: %w", i, err)
		}
		result.AddedLeaves = append(result.AddedLeaves, leafIndex)
	}

	return result, nil
}
