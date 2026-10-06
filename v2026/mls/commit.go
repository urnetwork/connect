// RFC 9420 section 12.4's commit rules that are decisions rather than encodings.
//
// The Commit wire struct and its codec are commit_wire.go's, with the rest of the framing types,
// and nothing about the bytes is decided here. What lives here is the rule that says whether a
// commit must carry an UpdatePath at all -- which is a lifecycle decision, made identically by the
// member BUILDING a commit and by every member receiving one, and made from the proposal list
// alone. This file grows as the rest of the commit lifecycle lands; today it holds one rule.
package mls

// CommitPathRequired is RFC 9420 section 12.4's pseudocode, verbatim:
//
//	pathRequiredTypes = [update, remove, external_init, group_context_extensions]
//	pathRequired = false
//	for proposal in commit.proposals:
//	    pathRequired = pathRequired || (proposal.msg_type in pathRequiredTypes)
//	if len(commit.proposals) == 0 || pathRequired:
//	    assert(commit.path != null)
//
// It DELEGATES to (*ProposalList).PathRequired rather than restating the loop, and that is the
// whole reason this function is three lines instead of ten. The sender asks this question when it
// decides whether to build a path and the receiver asks it as ValSem201; a second transcription of
// the same rule is two answers that agree until somebody edits one of them, and the input that
// separates them is a commit this build emits and refuses to receive. proposal_list.go states the
// type set once, over the whole RFC registry rather than over the four types this profile
// implements, and its own header says why.
//
// A NIL LIST IS "PATH REQUIRED", and it is answered here rather than left to a nil dereference
// inside the method. The empty half of the RFC's rule is what makes that the safe answer: a commit
// naming no proposals must carry a path, so a caller that handed over no list at all is asking
// about a commit that names no proposals. The other direction would be a validator that let a
// pathless commit through because its caller forgot an argument, which is the one failure mode
// section 12.4's empty clause exists to prevent -- an epoch that advances over key material every
// member of the previous epoch still holds.
func CommitPathRequired(list *ProposalList) bool {
	if list == nil {
		return true
	}
	return list.PathRequired()
}

// ---------------------------------------------------------------------------
// RFC 9420 section 12.4.1: what a commit generation is allowed to choose, what
// it answers, and the epoch it stages
// ---------------------------------------------------------------------------

// CommitOptions are the committer's discretionary choices. Nothing an exported field here can do
// makes an invalid commit valid: every option below is read BEFORE (*Group).Commit's own doors run,
// and the commit it produces goes through ValidateCommit whatever they say.
type CommitOptions struct {
	// Force populates the update path even when the proposal list does not require one, which
	// buys post compromise security for the committer at the cost of one HPKE seal per node of
	// its filtered direct path. RFC 9420 section 12.4 permits an add-only commit to omit it.
	Force bool

	// ExtraProposals are by-value proposals appended after the byValue argument, attributed to
	// the committer like every other by-value proposal. It is a second by-value channel rather
	// than a different kind of proposal: connect/message assembles the list a user asked for and
	// this profile appends the ones the POLICY requires beside it.
	ExtraProposals []Proposal

	// The three seams below make a commit this package would never send, and they are UNEXPORTED
	// so that no package but this one can set them: p8's ValSem tests need a commit whose
	// confirmation tag is missing, is taken over the wrong transcript, or whose list this
	// package's own doors refuse, and every exported entry point here computes all three itself.
	//
	// THEY ARE FIELDS OF CommitOptions AND NOT OF Commit, which is the plan's own decision and is
	// load bearing: Commit is a wire structure, and a flag on it would change what
	// syntax.Marshal(commit) emits and therefore what every confirmed transcript hash covers.
	//
	// WHAT THE COMPILER HOLDS HERE IS LESS THAN framing_group_seams_test.go GETS, and that is said
	// out loud rather than left to be discovered. Those two seams are declarations of the TEST
	// BINARY, so a production caller does not build at all; these are fields of a production type,
	// so the compiler stops only the packages that are not this one. What stops this one is review.
	skipValidation                         bool
	dropConfirmationTag                    bool
	confirmationTagOverPreCommitTranscript bool
}

// CommitResult is what a committer sends: the commit itself, the Welcome for the members it adds,
// and the post-commit tree those members are handed out of band.
//
// Three octet strings and no structures, because these three cross a process boundary --
// connect/message publishes them and holds no mls type. Every one of them is freshly encoded
// storage the caller owns.
type CommitResult struct {
	// Commit is an MLSMessage(PrivateMessage) carrying the Commit, sealed under the epoch the
	// commit CLOSES, because that is the epoch every recipient is still in.
	Commit []byte
	// Welcome is an MLSMessage(Welcome), nil when the commit adds nobody.
	Welcome []byte
	// RatchetTree is the post-commit public tree, for out-of-band Welcome delivery.
	RatchetTree []byte
}

// StagedCommit is a commit that has been validated and whose new epoch has been derived, but which
// has not replaced the group's live state.
//
// A commit is staged on BOTH sides and that is why this type is not a detail of the send path. The
// committer stages its own and merges when the delivery service accepts it, because the service
// accepts at most one commit per (group, epoch) and a committer that merged optimistically would
// fork itself off the group (MASTER section 9.3). A receiver stages an inbound one so that a policy
// decision -- and, for a commit that removes this client, a decision about what to keep -- can
// happen before the epoch advances.
//
// EVERY FIELD IS UNEXPORTED AND THE ACCESSORS ANSWER COPIES, for the reason (*Group)'s own
// accessors do: this value holds the tree, the schedule and the transcript a merge is about to
// install, and a caller writing through any of them would be editing the epoch this client is about
// to enter with nothing downstream able to report it.
type StagedCommit struct {
	committer LeafIndex
	epoch     uint64

	// the group and the epoch this commit was STAGED AGAINST, which is what binds a staged epoch
	// to the state that derived it. Both halves and never the epoch alone: every group this client
	// is a member of runs an epoch 7, so an epoch number is not an identity. It is
	// (*ProposalCache).bindingHolds' pair, asked one type over for the same reason.
	//
	// FIELDS RATHER THAN READS OFF context, because the staged value a REMOVED member is handed
	// carries no context at all -- stageInboundCommitLocked answers a report for that case -- and a
	// provenance check written as self.context.GroupId would take the process on exactly the commit
	// whose whole purpose is to close the group.
	groupId    []byte
	priorEpoch uint64

	context *GroupContext
	// the same context with its authority established, which is what an epoch boundary owes the
	// proposal cache. It is built HERE rather than at the merge so that a commit whose own
	// GroupInfo this client cannot sign or verify is refused before anything is sealed --
	// (*ProposalCache).Rebind takes nothing else, and a merge that could not produce one would
	// leave the group in the new epoch with a cache bound to the epoch that closed.
	verified   *VerifiedGroupContext
	tree       *RatchetTree
	schedule   *KeySchedule
	secretTree *SecretTree
	ownPriv    *TreeKEMPrivate
	transcript *TranscriptHashes
	list       *ProposalList
	// the commit this stages, as the structure the sender signed. p7 task 18's
	// (*Group).stageInboundCommitLocked stages an inbound one the same way, and both paths keep it
	// for one reason: what a
	// merge, a re-validation or a report has to be able to name is the commit itself and not a
	// summary somebody took of it.
	commit      *Commit
	added       []LeafIndex
	removed     []LeafIndex
	updated     []LeafIndex
	selfRemoved bool
	hasPath     bool
	confirmTag  []byte
	plan        *UpdatePathPlan

	// erased once Zeroize has run, so a staged epoch whose key material is gone REFUSES to be
	// installed rather than being installed as zeros. (*TreeKEMPrivate).erased and
	// (*SecretTree).Zeroize carry the same flag for the same reason, and here the flag is the ONLY
	// thing that can say it: groupId, priorEpoch, epoch, committer and selfRemoved all survive the
	// erase -- they are not key material -- so the provenance pair ApplyCommit reads passes over an
	// erased value exactly as it passes over a live one. Measured through the exported API alone:
	// processed.Commit.Zeroize() and then receiver.ApplyCommit(processed) advanced the member an
	// epoch and left it answering a 32-zero epoch authenticator.
	erased bool

	// which constructor the new epoch's schedule came from, which is what task 19's LoadGroup
	// needs in order to know what to rebuild it from. THE SECRET ITSELF IS NOT HERE, for the
	// reason (*Group).restoreKind states one file over: the schedule already retains it, its
	// erase is held by the type that DECLARES that storage, and a second copy parked here would
	// be the same secret held by nothing but a hand written Close.
	restoreKind restoreKind
}

// Zeroize erases the key material of the epoch this commit stages, which is a WHOLE second
// epoch and not a fragment of one: its key schedule holds the init secret, the confirmation
// key, the encryption secret, the epoch authenticator, the exporter and the resumption PSK, its
// secret tree holds every message key derived from that encryption secret, and its private tree
// state holds the leaf key the committer drew for it.
//
// IT IS CALLED WHEN THE STAGED EPOCH IS DROPPED, which is the ordinary path and not an edge
// case. MASTER section 9.3's lost-commit race is what (*Group).ClearPendingCommit exists for:
// the delivery service accepts at most one commit per (group, epoch), so a client whose commit
// lost the race drops a fully derived epoch nobody ever entered. (*Group).Close drops one too,
// for a group closed with a commit still in flight.
//
// THE PLAN'S PRIVATE HALF IS ERASED THROUGH ownPriv AND NOT THROUGH plan, because the two are
// the same pointer when the commit carries an update path: (*UpdatePathPlan).Zeroize erases the
// ladder and the commit secret and leaves Private to the holder, and its own comment says why.
//
// ON A VALUE WHOSE EPOCH WAS INSTALLED IT ERASES NOTHING, and that is (*Group).MergePendingCommit's
// doing and not a check made here: the merge moves the schedule, the secret tree, the leaf
// private state and the plan into the group and detaches all four from this value, so the nil
// guards below find nothing and the flag alone is set. It matters because the value a caller
// holds after ApplyCommit is this one, and "erase what I processed" written beside an
// ApplyCommit that succeeded is the ordinary cleanup shape; before the detach (2026-09-21) that
// shape erased the epoch the group had just entered.
//
// The noinline directive is the erase class's rule; see (*TreeKEMPrivate).Zeroize.
//
//go:noinline
func (self *StagedCommit) Zeroize() {
	// nil accepted, for zeroizeSecret's reason: "erase the staged epoch if there is one" is the
	// shape of every drop site, and a guard written at each of them is one that will be missing
	// at the next one somebody adds.
	if self == nil {
		return
	}
	if self.schedule != nil {
		self.schedule.Zeroize()
	}
	if self.secretTree != nil {
		self.secretTree.Zeroize()
	}
	self.ownPriv.Zeroize()
	self.plan.Zeroize()
	self.erased = true
}

// installed reports a SHELL: a value that staged an epoch and no longer holds its key
// schedule, because (*Group).MergePendingCommit moved the schedule into the group and
// detached it. It is the second condition the two install doors read before the provenance
// pair, beside erased, and it is read off the STORAGE rather than off a flag set at the
// detach, for the reason the erase class gives: the hazard is a door that installs a value
// holding no schedule -- the merge assigns nil over the live one and the persist dereferences
// it -- and a predicate written over the storage refuses every value in that state whatever
// path put it there, where a flag would refuse the values somebody remembered to mark.
//
// THE REPORT A REMOVED MEMBER IS HANDED IS NOT A SHELL, and the second clause is what says
// so: stageInboundCommitLocked builds that value with no schedule because a removed member
// derives no epoch, and (*Group).ApplyCommit answers it with ErrRemovedFromGroup on its own
// arm. The report is the ONE legitimate schedule-less value of this type, it is the value
// that carries selfRemoved, and a predicate without this clause refuses every removal --
// TestApplyCommitReadsProvenanceBeforeTheRemovalArm and every self-removal case in
// messagegroup go red on that mutant.
func (self *StagedCommit) installed() bool {
	return self.schedule == nil && !self.selfRemoved
}

// Epoch is the epoch this commit opens.
func (self *StagedCommit) Epoch() uint64 { return self.epoch }

// Committer is the leaf that authored the commit.
func (self *StagedCommit) Committer() LeafIndex { return self.committer }

// AddedLeaves is where the commit's Add proposals landed, in the order they were applied.
func (self *StagedCommit) AddedLeaves() []LeafIndex { return append([]LeafIndex(nil), self.added...) }

// RemovedLeaves is the leaves the commit blanked.
func (self *StagedCommit) RemovedLeaves() []LeafIndex {
	return append([]LeafIndex(nil), self.removed...)
}

// UpdatedLeaves is the leaves an Update proposal replaced.
func (self *StagedCommit) UpdatedLeaves() []LeafIndex {
	return append([]LeafIndex(nil), self.updated...)
}

// RemovesSelf reports whether this commit removes the processing client.
func (self *StagedCommit) RemovesSelf() bool { return self.selfRemoved }

// GroupContextExtensions is the post-commit extension list, as storage the caller owns.
//
// The BODIES are copied and not the entries alone, which is NewGroup's rule at the other end of the
// same list and is here for its reason: an Extension copied by value goes on pointing at the octets
// the new epoch's group context was built over, so a caller that wrote through one would be
// rewriting the context this client is about to derive an epoch's secrets under.
//
// THE REPORT A REMOVED MEMBER IS HANDED CARRIES NO CONTEXT -- stageInboundCommitLocked says why
// -- and before 2026-09-21 this accessor dereferenced it. What that report can still answer is
// the list the commit's own GroupContextExtensions proposal names, because the resolved list is
// on it; a commit carrying no such proposal installs the extension set the group already had,
// which this value does not hold and answers nil for. The caller holds the pre-commit context
// and is told, here, that nil on a self-removing commit means "unchanged".
func (self *StagedCommit) GroupContextExtensions() []Extension {
	extensions := self.postCommitExtensions()
	if extensions == nil {
		return nil
	}
	out := make([]Extension, 0, len(extensions))
	for _, extension := range extensions {
		out = append(out, Extension{
			ExtensionType: extension.ExtensionType,
			ExtensionData: cloneBytes(extension.ExtensionData),
		})
	}
	return out
}

// postCommitExtensions is the extension list the epoch this commit opens carries, UNCOPIED: the
// staged context's when there is one, and otherwise -- the removed member's report -- what the
// commit's own GroupContextExtensions proposal names, or nil when it names none.
func (self *StagedCommit) postCommitExtensions() []Extension {
	if self.context != nil {
		return self.context.Extensions
	}
	if self.list == nil {
		return nil
	}
	replaced, carried := self.list.Extensions()
	if !carried {
		return nil
	}
	return replaced
}

// OccupiedLeavesAfter is every leaf the POST-commit tree holds a member at, in leaf order.
//
// It reads the STAGED tree -- the one the commit's proposals built and its update path was merged
// into -- and never the group's live one, which is the whole of why it is a method of this type
// rather than of *Group: MASTER section 11 has a receiving client take its authorization decision
// BETWEEN ProcessMessage and ApplyCommit, on what the commit does, and "who is in the group
// afterwards" is a fact only the staged tree can answer at that moment. Together with
// LeafIdentityAfter it is what lets that decision compute the identities a commit adds, check that
// no leaf's identity changed across an Update or the committer's own path, and read the
// post-commit identity set the policy is judged against.
//
// The report a REMOVED member is handed carries the post-proposal tree too, so this answers there
// as well. What that tree lacks is the committer's update path, which a removed member cannot
// open; the leaf it shows at the committer's index is the pre-path one.
func (self *StagedCommit) OccupiedLeavesAfter() []LeafIndex {
	if self.tree == nil {
		return nil
	}
	return self.tree.NonBlankLeaves()
}

// LeafIdentityAfter is the credential identity the POST-commit tree carries at one leaf, as
// storage the caller owns, and false for a leaf that is blank or outside the tree.
//
// THE COPY IS THIS TYPE'S RULE and not a courtesy: the credential is inside the staged tree, that
// tree is the one MergePendingCommit installs, and a caller writing through the identity would be
// rewriting the leaf the new epoch's tree hash was taken over. See OccupiedLeavesAfter for which
// tree this reads and why.
func (self *StagedCommit) LeafIdentityAfter(leaf LeafIndex) ([]byte, bool) {
	if self.tree == nil {
		return nil, false
	}
	node := self.tree.Leaf(leaf)
	if node == nil {
		return nil, false
	}
	return cloneBytes(node.Credential.Identity), true
}

// LeafHasKeysAfter reports whether the POST-commit tree's leaf carries a urmessage_leaf_keys
// (0xF002) extension this profile can wrap to, and false for a leaf that is blank or outside
// the tree. See OccupiedLeavesAfter for which tree this reads and why.
//
// IT IS LeafKeysOf'S ANSWER AND NOT A LOOKUP OF THE TAG, because the question a receiving
// client asks is "can an epoch wrap reach this leaf" and that is what LeafKeysOf decides: a
// leaf carrying the type twice, or carrying a body that does not parse, is one no wrap
// addresses, and both send doors -- ProposeAdd and messagegroup's CommitAdd -- refuse a key
// package by that same call. This is its receiving-side twin, and it exists because the
// receiving side had no twin: mls's list validation does not require the extension (a leaf
// need only LIST the type in its capabilities), the sdk's authorizer could not see whether an
// added leaf carried one, and a hostile mls build that never ran the send door admits a leaf
// every honest member's next epoch wrap silently skips. Ledger item 242's R1 carried that into
// R2 as the one hardening owed; this is the fact it needs, answered off the tree the group is
// about to enter. The extension body is not answered, because no caller of this decision
// wraps anything; the wrap reads the LIVE tree after the merge.
func (self *StagedCommit) LeafHasKeysAfter(leaf LeafIndex) bool {
	if self.tree == nil {
		return false
	}
	node := self.tree.Leaf(leaf)
	if node == nil {
		return false
	}
	_, err := LeafKeysOf(node)
	return err == nil
}

// EpochAuthenticator is the new epoch's fork-detection value, as storage the caller owns.
//
// An ERASED staged commit answers nothing rather than the KDF.Nh zero bytes its erased schedule
// holds, which is (*TreeKEMPrivate).NodePrivateKey's rule at the one other exit door of this type
// that reads key material. The value this method answers is the one two members compare to decide
// whether they have forked, and zeros are what every erased staged commit in every process on
// earth answers -- so without this line the erase turns the fork detector into a function that
// says "no fork" about two members who have nothing in common. Nothing else this type exports
// reads erased storage: the four leaf vectors, the two provenance fields and the context are
// public facts about the commit and survive the erase on purpose.
//
// AN INSTALLED staged commit answers nothing for the same reason, and the second condition is
// that case: (*Group).MergePendingCommit moves the schedule into the group and detaches it from
// this value, so a value whose epoch has been entered holds no schedule to answer from. The
// epoch's authenticator is then the GROUP's to answer, and a caller still holding the staged
// value after the merge is holding a shell.
func (self *StagedCommit) EpochAuthenticator() []byte {
	if self.erased || self.schedule == nil {
		return nil
	}
	return cloneBytes(self.schedule.Secrets().EpochAuthenticator)
}
