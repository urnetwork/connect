// Ledger item 257's ruling 51 at the seam: the arm a removal actually ships on, and the
// removed-leaf set derived off the staged commit instead of passed down beside it.
//
// The two halves are one ruling because they close the same class of defect one layer apart. A
// removal is TWO facts -- a leaf leaves the tree and an identity leaves the policy -- and until
// this commit the seam could carry only the first, so a removal of a NAMED identity's last leaf
// produced a group whose policy named somebody with no leaf and every honest receiver refused it
// as an R0c phantom. And the epoch fan-out that must NOT seal the next epoch's post-quantum
// secret to the member being removed took the exclusion as an ARGUMENT, so "does this removal
// shut the member out" was a question about whether an arm remembered to pass a vector.
//
// Nothing here decides who may remove whom. That predicate is the sdk's, on both arms.
package messagegroup

import (
	"bytes"
	"errors"
	"testing"

	"github.com/urnetwork/connect/mls"
	"github.com/urnetwork/connect/mls/syntax"
)

// stagedContextOf stages one commit of the caller's own by-value vector on a handle and answers
// the GroupContext of the epoch it would open, read through the seam's own staged door, then
// clears it. The handle is left exactly where it stood, so two calls read one epoch.
//
// It reaches (*mls.Group).CreateCommit directly because the vector ORDER is the variable under
// test and no exported arm takes one; every other case in this file drives the arm.
func stagedContextOf(t *testing.T, handle GroupHandle, what string, byValue []mls.Proposal) *mls.GroupContext {
	t.Helper()
	adapter, isAdapter := handle.(*connectMlsHandle)
	if !isAdapter {
		t.Fatalf("%s: this handle is not the connect/mls adapter", what)
	}
	if _, err := adapter.group.CreateCommit([][]byte{}, byValue, nil); err != nil {
		t.Fatalf("%s: CreateCommit: %v", what, err)
	}
	contextBytes, err := adapter.group.PendingGroupContext()
	if err != nil {
		t.Fatalf("%s: PendingGroupContext: %v", what, err)
	}
	adapter.group.ClearPendingCommit()
	context := &mls.GroupContext{}
	if err := syntax.Unmarshal(contextBytes, context); err != nil {
		t.Fatalf("%s: the staged context does not decode: %v", what, err)
	}
	return context
}

// addProposalOf is one by-value Add over a freshly minted key package.
func addProposalOf(t *testing.T, engine *testEngine) mls.Proposal {
	t.Helper()
	encoded, err := engine.engine.NewKeyPackage()
	if err != nil {
		t.Fatalf("NewKeyPackage: %v", err)
	}
	var keyPackage mls.KeyPackage
	if err := syntax.Unmarshal(encoded, &keyPackage); err != nil {
		t.Fatalf("a key package this engine just minted does not decode: %v", err)
	}
	return mls.Proposal{ProposalType: mls.ProposalTypeAdd, Add: &mls.Add{KeyPackage: keyPackage}}
}

// removalChain is four members at leaves 0..3 -- founder, joiner, third, fourth -- with the
// member at leaf 2 NAMED in the policy as an ADMIN, which is the state a removal has to be tested
// against: an identity the policy carries an explicit entry for. Every handle stands at the same
// epoch with the same exporter.
type removalChain struct {
	*commitAddChain
	third       GroupHandle
	fourth      GroupHandle
	victim      []byte
	victimLeaf  uint32
	receivers   map[string]GroupHandle
	afterNaming []ExtensionBytes
}

func newRemovalChain(t *testing.T, name string) *removalChain {
	t.Helper()
	chain := newCommitAddChain(t, name)
	thirdEngine, fourthEngine := newTestEngine(t), newTestEngine(t)
	keyPackages := [][]byte{}
	for _, engine := range []*testEngine{thirdEngine, fourthEngine} {
		keyPackage, err := engine.engine.NewKeyPackage()
		if err != nil {
			t.Fatalf("NewKeyPackage: %v", err)
		}
		keyPackages = append(keyPackages, keyPackage)
	}
	commit, welcome, ratchetTree, err := chain.founded.CommitAdd(keyPackages)
	if err != nil {
		t.Fatalf("CommitAdd of the third and fourth members: %v", err)
	}
	processed, err := chain.joined.Process(commit)
	if err != nil {
		t.Fatalf("the joiner's Process of the add: %v", err)
	}
	if err := chain.joined.ApplyCommit(processed); err != nil {
		t.Fatalf("the joiner's ApplyCommit of the add: %v", err)
	}
	if err := chain.founded.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit of the add: %v", err)
	}
	third, err := thirdEngine.engine.JoinFromWelcome(welcome, ratchetTree)
	if err != nil {
		t.Fatalf("the third member's join: %v", err)
	}
	t.Cleanup(func() { third.Close() })
	fourth, err := fourthEngine.engine.JoinFromWelcome(welcome, ratchetTree)
	if err != nil {
		t.Fatalf("the fourth member's join: %v", err)
	}
	t.Cleanup(func() { fourth.Close() })
	if third.OwnLeafIndex() != 2 || fourth.OwnLeafIndex() != 3 || chain.founded.MemberCount() != 4 {
		t.Fatalf("the chain is not four members at leaves 0..3 (third at %d, fourth at %d, %d members)",
			third.OwnLeafIndex(), fourth.OwnLeafIndex(), chain.founded.MemberCount())
	}

	// NAME THE MEMBER AT LEAF 2, because a removal's whole difficulty is the named identity: any
	// SetRole keeps an explicit entry and nothing calls RemoveRole, so the policy of a group that
	// has ever promoted somebody goes on naming them after a bare Remove blanks their leaf.
	victim := identityAtLeaf(t, chain.founded, 2)
	policy := policyOf(t, contextExtensionsOf(t, chain.founded))
	policy.SetRole(victim, mls.RoleAdmin)
	encoded, err := policy.Encode()
	if err != nil {
		t.Fatalf("encode the policy that names the victim: %v", err)
	}
	naming, _, _, err := chain.founded.CommitPolicy(encoded.ExtensionData)
	if err != nil {
		t.Fatalf("CommitPolicy naming the victim an admin: %v", err)
	}
	receivers := map[string]GroupHandle{"the joiner": chain.joined, "the victim": third, "the fourth member": fourth}
	for who, handle := range receivers {
		processed, err := handle.Process(naming)
		if err != nil {
			t.Fatalf("%s's Process of the naming commit: %v", who, err)
		}
		if err := handle.ApplyCommit(processed); err != nil {
			t.Fatalf("%s's ApplyCommit of the naming commit: %v", who, err)
		}
	}
	if err := chain.founded.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit of the naming commit: %v", err)
	}
	everyone := map[string]GroupHandle{"the founder": chain.founded}
	for who, handle := range receivers {
		everyone[who] = handle
	}
	assertSameEpochSecret(t, 3, everyone)

	afterNaming := contextExtensionsOf(t, chain.founded)
	if _, named := policyOf(t, afterNaming).RoleOf(victim); !named {
		t.Fatal("the policy does not name the victim after the commit that named it, so every case below would be testing an unnamed member and the phantom this arm exists for could not arise")
	}
	return &removalChain{
		commitAddChain: chain,
		third:          third,
		fourth:         fourth,
		victim:         victim,
		victimLeaf:     2,
		receivers:      receivers,
		afterNaming:    afterNaming,
	}
}

// listWithoutTheVictim is the post-commit extension list a removal of the victim ships with: the
// current list with the victim's role entry dropped out of 0xF001 and every other entry standing.
func (self *removalChain) listWithoutTheVictim(t *testing.T) []ExtensionBytes {
	t.Helper()
	policy := policyOf(t, self.afterNaming)
	policy.RemoveRole(self.victim)
	encoded, err := policy.Encode()
	if err != nil {
		t.Fatalf("encode the policy that drops the victim: %v", err)
	}
	replaced, err := mls.ExtensionsWithGroupPolicy(mlsExtensionsOf(self.afterNaming), encoded.ExtensionData)
	if err != nil {
		t.Fatalf("ExtensionsWithGroupPolicy: %v", err)
	}
	return extensionBytesOf(replaced)
}

// ---------------------------------------------------------------------------
// the arm's vector, and the order in it
// ---------------------------------------------------------------------------

// TestTheCombiningArmBuildsTheRemovesFirstAndTheExtensionsLast is the pin on the one thing this
// arm decides that nothing downstream can see: the order of its proposals.
//
// IT IS STATED OVER THE VECTOR AND NOT OVER A COMMIT, and that is a limit measured rather than a
// preference. RFC 9420 section 12.4 forces an update path on any commit carrying a Remove and the
// path draws fresh secrets on every build, so the same removal built twice from one state already
// signs two different commits --
// TestTheProposalOrderOfACommitIsWhatTheConfirmedTranscriptHashIsTakenOver measures exactly that
// -- and there is no pair of removal commits whose only difference is the order. The vector is
// the last value the order is visible in, so the vector is where it is held.
//
// WHAT THIS CANNOT SEE, said out loud: a reversal applied to this function's ANSWER at the call
// site. The arm hands the answer straight to CreateCommit and nothing between them touches it,
// which is a fact a reader checks and not one this case drives.
func TestTheCombiningArmBuildsTheRemovesFirstAndTheExtensionsLast(t *testing.T) {
	extensions := []ExtensionBytes{
		{Type: 0xF001, Data: []byte("the policy body this case hands the arm")},
		{Type: 0x0003, Data: []byte("the required capabilities this case hands the arm")},
	}
	built := removeWithExtensionsProposals([]uint32{4, 2}, extensions)
	if len(built) != 3 {
		t.Fatalf("the arm's vector is %d proposals over two leaves and one list, want 3", len(built))
	}
	for at, leaf := range []mls.LeafIndex{4, 2} {
		if built[at].ProposalType != mls.ProposalTypeRemove {
			t.Fatalf("proposal %d of the arm's vector is type %#04x and the removes come FIRST; a vector whose extensions lead signs a different commit for the same removal",
				at, built[at].ProposalType)
		}
		if built[at].Remove == nil || built[at].Remove.Removed != leaf {
			t.Fatalf("proposal %d removes %v, want leaf %d in the caller's own order", at, built[at].Remove, leaf)
		}
	}
	last := built[len(built)-1]
	if last.ProposalType != mls.ProposalTypeGroupContextExtensions {
		t.Fatalf("the LAST proposal of the arm's vector is type %#04x, want group_context_extensions; the order is fixed Remove-then-GCE and this is the half a flip moves",
			last.ProposalType)
	}
	if last.GroupContextExtensions == nil {
		t.Fatal("the arm's last proposal carries no extension list at all")
	}
	assertSameExtensions(t, "the list the arm's GroupContextExtensions carries",
		extensionBytesOf(last.GroupContextExtensions.Extensions), extensions)
	// and the bodies are CLONED, which is CommitContextExtensions' property stated for this arm:
	// a caller that overwrites what it handed over has not rewritten what the commit carries
	extensions[0].Data[0] ^= 0xFF
	if bytes.Equal(extensionBytesOf(last.GroupContextExtensions.Extensions)[0].Data, extensions[0].Data) {
		t.Fatal("the arm's extension bodies alias the caller's arrays")
	}
}

// TestTheCombiningArmRefusesEachEmptyWithTheSentinelOfTheArmThatOwnsIt holds both doors, and
// holds that a refusal stages nothing: a handle that staged something on the way to answering an
// error is a handle whose next commit answers ErrPendingCommitExists.
func TestTheCombiningArmRefusesEachEmptyWithTheSentinelOfTheArmThatOwnsIt(t *testing.T) {
	chain := newRemovalChain(t, "removewithextensions-empties")
	list := chain.listWithoutTheVictim(t)
	if _, _, _, err := chain.founded.CommitRemoveWithExtensions(nil, list); !errors.Is(err, ErrEngineCommitRemoveEmpty) {
		t.Fatalf("CommitRemoveWithExtensions(nil, list) answered %v, want ErrEngineCommitRemoveEmpty", err)
	}
	if _, _, _, err := chain.founded.CommitRemoveWithExtensions([]uint32{2}, nil); !errors.Is(err, ErrEngineCommitContextExtensionsEmpty) {
		t.Fatalf("CommitRemoveWithExtensions(leaves, nil) answered %v, want ErrEngineCommitContextExtensionsEmpty", err)
	}
	if _, err := chain.founded.PendingEpoch(); !errors.Is(err, mls.ErrNoPendingCommit) {
		t.Fatalf("after two refusals PendingEpoch answered %v, want ErrNoPendingCommit; a refusal staged an epoch", err)
	}
	// the control: the same handle, the same arguments made whole, builds
	if _, _, _, err := chain.founded.CommitRemoveWithExtensions([]uint32{2}, list); err != nil {
		t.Fatalf("CONTROL FAILED: the arm refuses a well-formed call too (%v), so the two refusals above say nothing about the empties", err)
	}
}

// TestTheProposalOrderOfACommitIsWhatTheConfirmedTranscriptHashIsTakenOver is the measurement the
// arm's fixed order rests on, and it is driven rather than argued from RFC 9420.
//
// THE ISOLATION IS THE WHOLE DIFFICULTY. Two commits built from one state differ in every octet
// their update path touches, so a bare "these two orders hash differently" says nothing: the same
// order twice hashes differently too. What isolates the vector is the one commit shape RFC 9420
// section 12.4 leaves PATHLESS -- Adds only -- where nothing is drawn, the signature is Ed25519's
// deterministic one, and two builds of one vector are the same octets.
//
// SO THE EQUAL PAIR IS THE CONTROL AND IT FIRES FOR ITS OWN REASON: it says this commit carries
// no path, because a path would make the two unequal, and it is therefore what licenses reading
// the unequal pair below it as the ORDER and not the draw. Both halves are in this one case
// because neither is a claim without the other.
//
// AND THE LAST BLOCK IS WHY THE REMOVAL'S OWN COMMIT CANNOT BE THE INSTRUMENT: the arm built
// twice from one state answers two confirmed transcript hashes. "The same removal built twice
// yields the same hash" is false of any commit carrying a Remove, which is why the order is
// fixed inside the arm rather than pinned by a known answer over the removal itself.
func TestTheProposalOrderOfACommitIsWhatTheConfirmedTranscriptHashIsTakenOver(t *testing.T) {
	chain := newCommitAddChain(t, "removewithextensions-order")
	first, second := addProposalOf(t, newTestEngine(t)), addProposalOf(t, newTestEngine(t))

	forward := stagedContextOf(t, chain.founded, "the adds in the caller's order", []mls.Proposal{first, second})
	again := stagedContextOf(t, chain.founded, "the same adds a second time", []mls.Proposal{first, second})
	if !bytes.Equal(forward.ConfirmedTranscriptHash, again.ConfirmedTranscriptHash) {
		t.Fatalf("CONTROL FAILED: one vector built twice from one epoch answers %x and %x, so this commit shape is not deterministic and the comparison below cannot separate the order from the draw",
			forward.ConfirmedTranscriptHash, again.ConfirmedTranscriptHash)
	}
	reversed := stagedContextOf(t, chain.founded, "the adds swapped", []mls.Proposal{second, first})
	if bytes.Equal(forward.ConfirmedTranscriptHash, reversed.ConfirmedTranscriptHash) {
		t.Fatalf("the swapped vector answers the same confirmed transcript hash %x; the commit's own proposal vector is inside the FramedContent RFC 9420 section 8.2 takes that hash over, so an order that did not reach it would mean the preimage had stopped carrying the proposals",
			forward.ConfirmedTranscriptHash)
	}

	// and the removal's own commit, which cannot be held this way and is why the order is fixed
	// in the arm: section 12.4 forces a path on any commit carrying a Remove
	removal := newRemovalChain(t, "removewithextensions-order-removal")
	list := removal.listWithoutTheVictim(t)
	vector := removeWithExtensionsProposals([]uint32{removal.victimLeaf}, list)
	once := stagedContextOf(t, removal.founded, "the removal", vector)
	twice := stagedContextOf(t, removal.founded, "the same removal again", vector)
	if bytes.Equal(once.ConfirmedTranscriptHash, twice.ConfirmedTranscriptHash) {
		t.Fatal("the SAME removal built twice from one epoch answers one confirmed transcript hash. That would make a known answer over the removal itself possible, and the arm's header says the opposite -- if a commit carrying a Remove has stopped drawing an update path, RFC 9420 section 12.4 has been broken and this file's reasoning has to be rewritten")
	}
}

// ---------------------------------------------------------------------------
// the arm end to end
// ---------------------------------------------------------------------------

// TestACommitRemoveWithExtensionsTakesTheIdentityOutOfBothTheTreeAndThePolicy is the property the
// arm exists for, at every receiver that matters, WITH the bare removal beside it as the control
// that says the extension list is doing the work.
func TestACommitRemoveWithExtensionsTakesTheIdentityOutOfBothTheTreeAndThePolicy(t *testing.T) {
	// THE CONTROL FIRST, on its own chain: a bare CommitRemove of the same named identity leaves
	// the policy naming an identity with no leaf, which is the R0c phantom this arm exists for.
	bare := newRemovalChain(t, "removewithextensions-control-bare")
	bareRemoval, _, _, err := bare.founded.CommitRemove([]uint32{bare.victimLeaf})
	if err != nil {
		t.Fatalf("CONTROL: CommitRemove: %v", err)
	}
	bareProcessed, err := bare.joined.Process(bareRemoval)
	if err != nil {
		t.Fatalf("CONTROL: the joiner's Process of the bare removal: %v", err)
	}
	if _, named := policyOf(t, bareProcessed.ContextExtensionsAfter).RoleOf(bare.victim); !named {
		t.Fatal("CONTROL FAILED: a BARE CommitRemove already drops the victim from the policy, so the combining arm below would be carrying a list that changes nothing and this whole case would pass over a group the phantom cannot arise in")
	}
	if err := bare.joined.DiscardProcessed(bareProcessed); err != nil {
		t.Fatalf("CONTROL: DiscardProcessed: %v", err)
	}

	chain := newRemovalChain(t, "removewithextensions-endtoend")
	membersBefore := membersOf(t, chain.founded)
	list := chain.listWithoutTheVictim(t)
	removal, welcome, _, err := chain.founded.CommitRemoveWithExtensions([]uint32{chain.victimLeaf}, list)
	if err != nil {
		t.Fatalf("CommitRemoveWithExtensions: %v", err)
	}
	if welcome != nil {
		t.Fatal("a removal answered a welcome")
	}
	wantAfter := []ProcessedMember{}
	for _, member := range membersBefore {
		if member.Leaf != chain.victimLeaf {
			wantAfter = append(wantAfter, member)
		}
	}

	// every survivor follows it cold -- nothing cached -- and sees both halves
	for _, who := range []string{"the joiner", "the fourth member"} {
		handle := chain.receivers[who]
		processed, err := handle.Process(removal)
		if err != nil {
			t.Fatalf("%s's Process of the removal: %v", who, err)
		}
		if len(processed.RemovedLeaves) != 1 || processed.RemovedLeaves[0] != chain.victimLeaf {
			t.Fatalf("%s reads RemovedLeaves %v, want [%d]", who, processed.RemovedLeaves, chain.victimLeaf)
		}
		assertSameMembers(t, who+"'s MembersAfter", processed.MembersAfter, wantAfter)
		if _, named := policyOf(t, processed.ContextExtensionsAfter).RoleOf(chain.victim); named {
			t.Fatalf("%s reads a post-commit policy that still names the removed identity; the group this commit opens names somebody with no leaf, which is the R0c phantom the arm carries the list to prevent", who)
		}
		// the OTHER entries survive: this is a wholesale replacement and 0x0003 is the entry a
		// list assembled from the policy alone would silently drop
		if len(processed.ContextExtensionsAfter) != len(chain.afterNaming) {
			t.Fatalf("%s reads %d post-commit extensions and the group had %d; the replacement is wholesale and an entry it leaves out is gone",
				who, len(processed.ContextExtensionsAfter), len(chain.afterNaming))
		}
		assertSameExtensions(t, who+"'s ContextExtensionsAfter", processed.ContextExtensionsAfter, list)
		if err := handle.ApplyCommit(processed); err != nil {
			t.Fatalf("%s's ApplyCommit of the removal: %v", who, err)
		}
	}

	// and the removed member is told so rather than following it
	victimProcessed, err := chain.third.Process(removal)
	if err != nil {
		t.Fatalf("the victim's Process of the removal: %v", err)
	}
	if err := chain.third.ApplyCommit(victimProcessed); !errors.Is(err, mls.ErrRemovedFromGroup) {
		t.Fatalf("the victim's ApplyCommit answered %v, want mls.ErrRemovedFromGroup", err)
	}
	if err := chain.third.DiscardProcessed(victimProcessed); err != nil {
		t.Fatalf("the victim's DiscardProcessed of the report: %v", err)
	}

	if err := chain.founded.MergePendingCommit(); err != nil {
		t.Fatalf("the committer's MergePendingCommit: %v", err)
	}
	assertSameEpochSecret(t, 4, map[string]GroupHandle{
		"the founder": chain.founded, "the joiner": chain.joined, "the fourth member": chain.fourth})
	// THE ANCHOR: what the authorizer was handed before the apply is what the group holds after it
	assertSameMembers(t, "MembersAfter against the live membership once the commit is applied",
		wantAfter, membersOf(t, chain.founded))
	assertSameExtensions(t, "the list the arm shipped against the live post-commit list",
		contextExtensionsOf(t, chain.founded), list)
	if _, named := policyOf(t, contextExtensionsOf(t, chain.founded)).RoleOf(chain.victim); named {
		t.Fatal("the live policy still names the removed identity after the commit was applied")
	}
}

// TestTheCombiningArmsProposalOrderIsNotObservableInTheAppliedState is the other half of the
// order ruling, and it is the half that says why the order has to be decided in ONE place: RFC
// 9420 section 12.3 applies proposals by TYPE, so no receiver, no authorizer and no tree can tell
// the two orders apart. The only thing that can is the signature over the vector.
func TestTheCombiningArmsProposalOrderIsNotObservableInTheAppliedState(t *testing.T) {
	chain := newRemovalChain(t, "removewithextensions-order-invisible")
	list := chain.listWithoutTheVictim(t)
	removes := removeProposals([]uint32{chain.victimLeaf})
	gce := mls.Proposal{
		ProposalType:           mls.ProposalTypeGroupContextExtensions,
		GroupContextExtensions: &mls.GroupContextExtensions{Extensions: mlsExtensionsOf(list)},
	}
	adapter, isAdapter := chain.founded.(*connectMlsHandle)
	if !isAdapter {
		t.Fatal("this handle is not the connect/mls adapter")
	}

	answers := map[string]*EngineProcessed{}
	for _, subject := range []struct {
		name    string
		byValue []mls.Proposal
	}{
		{"remove first, the order the arm fixes", append(append([]mls.Proposal{}, removes...), gce)},
		{"the extensions first", append([]mls.Proposal{gce}, removes...)},
	} {
		result, err := adapter.group.CreateCommit([][]byte{}, subject.byValue, nil)
		if err != nil {
			t.Fatalf("%s: CreateCommit: %v", subject.name, err)
		}
		processed, err := chain.joined.Process(result.Commit)
		if err != nil {
			t.Fatalf("%s: an honest receiver refused it: %v", subject.name, err)
		}
		answers[subject.name] = processed
		if err := chain.joined.DiscardProcessed(processed); err != nil {
			t.Fatalf("%s: DiscardProcessed: %v", subject.name, err)
		}
		adapter.group.ClearPendingCommit()
	}
	forward := answers["remove first, the order the arm fixes"]
	backward := answers["the extensions first"]
	if len(forward.RemovedLeaves) != 1 || forward.RemovedLeaves[0] != chain.victimLeaf {
		t.Fatalf("CONTROL FAILED: the arm's own order reads RemovedLeaves %v, want [%d], so the comparison below is between two things neither of which removed anybody",
			forward.RemovedLeaves, chain.victimLeaf)
	}
	if len(backward.RemovedLeaves) != len(forward.RemovedLeaves) || backward.RemovedLeaves[0] != forward.RemovedLeaves[0] {
		t.Fatalf("the two orders read RemovedLeaves %v and %v", forward.RemovedLeaves, backward.RemovedLeaves)
	}
	assertSameMembers(t, "the swapped order's MembersAfter", backward.MembersAfter, forward.MembersAfter)
	assertSameExtensions(t, "the swapped order's ContextExtensionsAfter", backward.ContextExtensionsAfter, forward.ContextExtensionsAfter)
	if _, named := policyOf(t, backward.ContextExtensionsAfter).RoleOf(chain.victim); named {
		t.Fatal("the swapped order left the victim named in the policy; section 12.3's apply order would have to be reading the vector rather than the types")
	}
}

// ---------------------------------------------------------------------------
// the derivation
// ---------------------------------------------------------------------------

// TestAStagedCommitAnswersTheLeavesItRemovesAndAnAddOnlyCommitAnswersNone is ruling 51's other
// half: the removed-leaf set is READ off the staged commit, and it equals what the caller of the
// arm would otherwise have had to pass down beside it.
//
// THE ADD-ONLY COMMIT IS THE INLINE CONTROL AND IT FIRES FOR ITS OWN REASON: it stages a real
// commit that really does change the membership -- MemberCount moves -- and answers the EMPTY set
// for the leaves it removes, so an implementation that answered "every leaf" or "the member
// count" or the live tree's occupancy is convicted by the same call the removal case is read
// through. It is emphatically NOT held to any equality with MemberCount: an added leaf is not in
// the live tree and gets no wrap, so every arithmetic relation between a wrap set and the member
// count is false on every Add.
func TestAStagedCommitAnswersTheLeavesItRemovesAndAnAddOnlyCommitAnswersNone(t *testing.T) {
	chain := newRemovalChain(t, "removewithextensions-derivation")
	list := chain.listWithoutTheVictim(t)

	// THE CONTROL, first: an Add-only commit stages an epoch, moves the member count, and removes
	// nobody
	if _, _, _, err := chain.founded.CommitAdd([][]byte{keyPackageOf(t, newTestEngine(t))}); err != nil {
		t.Fatalf("CONTROL: CommitAdd: %v", err)
	}
	adding, err := chain.founded.PendingEpoch()
	if err != nil {
		t.Fatalf("CONTROL: PendingEpoch over the add: %v", err)
	}
	if adding.MemberCount != chain.founded.MemberCount()+1 {
		t.Fatalf("CONTROL FAILED: the add stages %d members and the group holds %d, so this commit did not change the membership and its empty removed set says nothing",
			adding.MemberCount, chain.founded.MemberCount())
	}
	if len(adding.RemovedLeaves) != 0 {
		t.Fatalf("an Add-only commit answers RemovedLeaves %v, want none; the fan-out would exclude a leaf nobody removed", adding.RemovedLeaves)
	}
	chain.founded.ClearPendingCommit()

	// and the removal, through the arm, with the answer held to the argument a caller would have
	// passed
	wouldHavePassed := []uint32{chain.victimLeaf}
	if _, _, _, err := chain.founded.CommitRemoveWithExtensions(wouldHavePassed, list); err != nil {
		t.Fatalf("CommitRemoveWithExtensions: %v", err)
	}
	pending, err := chain.founded.PendingEpoch()
	if err != nil {
		t.Fatalf("PendingEpoch over the removal: %v", err)
	}
	if len(pending.RemovedLeaves) != len(wouldHavePassed) {
		t.Fatalf("the staged commit answers RemovedLeaves %v and the caller would have passed %v", pending.RemovedLeaves, wouldHavePassed)
	}
	for at, leaf := range wouldHavePassed {
		if pending.RemovedLeaves[at] != leaf {
			t.Fatalf("the staged commit answers RemovedLeaves %v and the caller would have passed %v", pending.RemovedLeaves, wouldHavePassed)
		}
	}
	// THE LIVE TREE STILL HOLDS THE VICTIM, which is the fact that makes the reading load-bearing:
	// the fan-out is built pre-merge off this tree, so the exclusion is not a tidiness
	if identityAtLeaf(t, chain.founded, chain.victimLeaf) == nil {
		t.Fatal("the live tree has already lost the removed leaf before the merge, so nothing pre-merge could have sealed an epoch to it and the exclusion would have no subject")
	}
	// the vector is the caller's, not the staged commit's own array
	pending.RemovedLeaves[0] = 0xFFFFFFFF
	second, err := chain.founded.PendingEpoch()
	if err != nil {
		t.Fatalf("the second PendingEpoch: %v", err)
	}
	if second.RemovedLeaves[0] != chain.victimLeaf {
		t.Fatalf("writing through the answer's RemovedLeaves changed what the next read answers: %v", second.RemovedLeaves)
	}
	chain.founded.ClearPendingCommit()
	if _, err := chain.founded.PendingEpoch(); !errors.Is(err, mls.ErrNoPendingCommit) {
		t.Fatalf("with nothing staged PendingEpoch answered %v, want ErrNoPendingCommit", err)
	}
}

// TestARemovalWhoseLeafIsRefilledInTheSameCommitIsStillNamedByTheStagedCommit is why the
// derivation reads the staged commit and not a comparison of the two trees, held rather than
// written down: it is the one input on which the tree comparison and the truth disagree.
//
// One commit removes leaf 2 and adds a newcomer. RFC 9420 section 12.3 applies Removes before
// Adds and an Add fills the leftmost blank, so the newcomer lands on the leaf the removal just
// blanked -- and the LIVE tree's occupied leaves and the STAGED tree's are then the same set,
// with the same member count, while a member was removed. A derivation off that comparison
// answers nothing removed; the fan-out is built pre-merge off the live tree, where the removed
// member's own X-Wing key is still standing, and it would seal the next epoch's post-quantum
// secret straight to the member the commit exists to shut out.
//
// The equal sets are the measurement and the non-empty answer is the property, and both are in
// this one case because the second says nothing without the first.
func TestARemovalWhoseLeafIsRefilledInTheSameCommitIsStillNamedByTheStagedCommit(t *testing.T) {
	chain := newRemovalChain(t, "removewithextensions-refill")
	adapter, isAdapter := chain.founded.(*connectMlsHandle)
	if !isAdapter {
		t.Fatal("this handle is not the connect/mls adapter")
	}
	liveOccupied := map[uint32]bool{}
	for _, member := range membersOf(t, chain.founded) {
		liveOccupied[member.Leaf] = true
	}
	if !liveOccupied[chain.victimLeaf] {
		t.Fatalf("the live tree does not hold the leaf this case removes; occupied %v", liveOccupied)
	}

	var keyPackage mls.KeyPackage
	if err := syntax.Unmarshal(keyPackageOf(t, newTestEngine(t)), &keyPackage); err != nil {
		t.Fatalf("a key package this engine just minted does not decode: %v", err)
	}
	byValue := append(removeProposals([]uint32{chain.victimLeaf}),
		mls.Proposal{ProposalType: mls.ProposalTypeAdd, Add: &mls.Add{KeyPackage: keyPackage}})
	result, err := adapter.group.CreateCommit([][]byte{}, byValue, nil)
	if err != nil {
		t.Fatalf("CreateCommit of the remove-and-refill: %v", err)
	}

	pending, err := chain.founded.PendingEpoch()
	if err != nil {
		t.Fatalf("PendingEpoch: %v", err)
	}
	processed, err := chain.joined.Process(result.Commit)
	if err != nil {
		t.Fatalf("a survivor refused the remove-and-refill: %v", err)
	}
	stagedOccupied := map[uint32]bool{}
	for _, member := range processed.MembersAfter {
		stagedOccupied[member.Leaf] = true
	}

	// THE MEASUREMENT: the two trees' occupied sets agree, and so do the counts
	if len(stagedOccupied) != len(liveOccupied) {
		t.Fatalf("MEASUREMENT FAILED: the staged tree holds %v and the live tree held %v. This case exists because an Add refills the leaf a Remove blanked in the same commit; if that has stopped being true the reason the derivation reads the staged commit has to be rewritten rather than this assertion relaxed",
			stagedOccupied, liveOccupied)
	}
	for leaf := range liveOccupied {
		if !stagedOccupied[leaf] {
			t.Fatalf("MEASUREMENT FAILED: leaf %d was occupied before and is not after; see above", leaf)
		}
	}
	if pending.MemberCount != chain.founded.MemberCount() {
		t.Fatalf("MEASUREMENT FAILED: the staged tree holds %d members and the live tree holds %d; see above",
			pending.MemberCount, chain.founded.MemberCount())
	}

	// THE PROPERTY: and the removal is named anyway
	if len(pending.RemovedLeaves) != 1 || pending.RemovedLeaves[0] != chain.victimLeaf {
		t.Fatalf("the staged commit answers RemovedLeaves %v over a commit that removed leaf %d and refilled it. Every reading of this fact taken by comparing the two trees answers the empty set here, and the fan-out then seals the epoch this commit opens to the removed member's own key",
			pending.RemovedLeaves, chain.victimLeaf)
	}
	if len(processed.RemovedLeaves) != 1 || processed.RemovedLeaves[0] != chain.victimLeaf {
		t.Fatalf("a receiver reads RemovedLeaves %v over the same commit", processed.RemovedLeaves)
	}
	if err := chain.joined.DiscardProcessed(processed); err != nil {
		t.Fatalf("DiscardProcessed: %v", err)
	}
	chain.founded.ClearPendingCommit()
}

// TestTheStagedCommitNamesTheLeavesInTheOrderTheProposalsDid holds the other half of the
// accessor's own sentence, which would otherwise be a claim about mls's walk that nothing reads:
// the vector is the proposals' order and not an ascending one.
func TestTheStagedCommitNamesTheLeavesInTheOrderTheProposalsDid(t *testing.T) {
	chain := newRemovalChain(t, "removewithextensions-leaforder")
	descending := []uint32{3, chain.victimLeaf}
	if descending[0] <= descending[1] {
		t.Fatalf("this case needs two leaves named out of ascending order and was handed %v, so a sorted answer would pass it", descending)
	}
	list := chain.listWithoutTheVictim(t)
	if _, _, _, err := chain.founded.CommitRemoveWithExtensions(descending, list); err != nil {
		t.Fatalf("CommitRemoveWithExtensions over two leaves: %v", err)
	}
	pending, err := chain.founded.PendingEpoch()
	if err != nil {
		t.Fatalf("PendingEpoch: %v", err)
	}
	if len(pending.RemovedLeaves) != 2 || pending.RemovedLeaves[0] != descending[0] || pending.RemovedLeaves[1] != descending[1] {
		t.Fatalf("the staged commit answers RemovedLeaves %v over proposals naming %v", pending.RemovedLeaves, descending)
	}
	chain.founded.ClearPendingCommit()
}

// keyPackageOf is one engine's freshly minted key package as octets.
func keyPackageOf(t *testing.T, engine *testEngine) []byte {
	t.Helper()
	keyPackage, err := engine.engine.NewKeyPackage()
	if err != nil {
		t.Fatalf("NewKeyPackage: %v", err)
	}
	return keyPackage
}
