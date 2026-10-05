// Ledger item 242's R2 at the seam: the two reads off a handle's OWN staged commit that let a
// committer submit before it merges.
//
// MASTER section 9.3 gives the delivery service at most one commit per (group, epoch) and has a
// loser "re-derive against the winner and retry"; Commit's own header says the staged epoch is
// staged and not merged for that reason. What every committer then did was merge FIRST, because
// the record that announces an epoch carries facts of that epoch and the live handle after the
// merge was the only door onto them. PendingEpoch and PendingExport are those facts one epoch
// early, and every case here is the same property stated over a different commit: what the
// staged value answers BEFORE the merge is what the live handle answers AFTER it -- and, for the
// exporter, what every receiver that applies the same commit derives.
package messagegroup

import (
	"bytes"
	"errors"
	"testing"

	"github.com/urnetwork/connect/mls"
)

// pendingFacts is what a committer reads off its staged commit to announce the epoch it opens:
// the value PendingEpoch answers and the storage exporter at the session's own label.
type pendingFacts struct {
	epoch        uint64
	memberCount  int
	groupContext []byte
	mlsSecret    []byte
}

func readPending(t *testing.T, what string, handle GroupHandle) *pendingFacts {
	t.Helper()
	pending, err := handle.PendingEpoch()
	if err != nil {
		t.Fatalf("%s: PendingEpoch: %v", what, err)
	}
	secret, err := handle.PendingExport(mlsSecretLabel, nil, mlsSecretBytes)
	if err != nil {
		t.Fatalf("%s: PendingExport: %v", what, err)
	}
	return &pendingFacts{epoch: pending.Epoch, memberCount: pending.MemberCount, groupContext: pending.GroupContext, mlsSecret: secret}
}

// readLive is the same four facts off the live handle, through the doors every committer used
// to read them after the merge.
func readLive(t *testing.T, what string, handle GroupHandle) *pendingFacts {
	t.Helper()
	contextBytes, err := handle.GroupContextBytes()
	if err != nil {
		t.Fatalf("%s: GroupContextBytes: %v", what, err)
	}
	secret, err := handle.Export(mlsSecretLabel, nil, mlsSecretBytes)
	if err != nil {
		t.Fatalf("%s: Export: %v", what, err)
	}
	return &pendingFacts{epoch: handle.Epoch(), memberCount: handle.MemberCount(), groupContext: contextBytes, mlsSecret: secret}
}

func assertSameFacts(t *testing.T, what string, got *pendingFacts, want *pendingFacts) {
	t.Helper()
	if got.epoch != want.epoch {
		t.Errorf("%s: epoch %d, want %d", what, got.epoch, want.epoch)
	}
	if got.memberCount != want.memberCount {
		t.Errorf("%s: member count %d, want %d", what, got.memberCount, want.memberCount)
	}
	if !bytes.Equal(got.groupContext, want.groupContext) {
		t.Errorf("%s: the group context differs (%d octets against %d)", what, len(got.groupContext), len(want.groupContext))
	}
	if !bytes.Equal(got.mlsSecret, want.mlsSecret) {
		t.Errorf("%s: the storage exporter differs", what)
	}
}

// assertNothingPending holds both doors to ErrNoPendingCommit, which is the refusal that keeps an
// announcement from ever being built out of the epoch the group is already in.
func assertNothingPending(t *testing.T, what string, handle GroupHandle) {
	t.Helper()
	if pending, err := handle.PendingEpoch(); !errors.Is(err, mls.ErrNoPendingCommit) {
		t.Errorf("%s: PendingEpoch answered %+v, %v; want ErrNoPendingCommit", what, pending, err)
	}
	if secret, err := handle.PendingExport(mlsSecretLabel, nil, mlsSecretBytes); !errors.Is(err, mls.ErrNoPendingCommit) {
		t.Errorf("%s: PendingExport answered %d octets, %v; want ErrNoPendingCommit", what, len(secret), err)
	}
}

// TestThePendingReadsAnswerWhatTheMergeInstalls is the property over the two shapes the sdk
// commits by value -- an Add, which changes the membership, and a policy, which changes the
// context -- with the control that makes it a property about the STAGED value: before the merge
// the pending facts differ from the live ones in every field the commit touches, and after the
// merge the live handle answers exactly the pending facts. The exporter is anchored a second way,
// at the receiver: the member that applies the same commit derives the same storage secret, which
// is what makes an announcement keyed off PendingExport one every member can open.
func TestThePendingReadsAnswerWhatTheMergeInstalls(t *testing.T) {
	chain := newCommitAddChain(t, "pending-reads")
	assertNothingPending(t, "a handle with nothing staged", chain.founded)

	// ── an Add: the membership and the epoch move ──────────────────────────────────────────────
	third := newTestEngine(t)
	keyPackage, err := third.engine.NewKeyPackage()
	if err != nil {
		t.Fatalf("NewKeyPackage: %v", err)
	}
	liveBefore := readLive(t, "the founder before CommitAdd", chain.founded)
	commit, _, _, err := chain.founded.CommitAdd([][]byte{keyPackage})
	if err != nil {
		t.Fatalf("CommitAdd: %v", err)
	}
	pending := readPending(t, "the founder after CommitAdd", chain.founded)
	if pending.epoch != liveBefore.epoch+1 {
		t.Fatalf("the staged commit opens epoch %d from %d, want the next", pending.epoch, liveBefore.epoch)
	}
	if pending.memberCount != liveBefore.memberCount+1 {
		t.Fatalf("the staged tree holds %d members after an add to %d", pending.memberCount, liveBefore.memberCount)
	}
	// THE CONTROL: the live handle has not moved, and the staged value is not the live one
	assertSameFacts(t, "the live handle while a commit is staged", readLive(t, "staged", chain.founded), liveBefore)
	if bytes.Equal(pending.mlsSecret, liveBefore.mlsSecret) {
		t.Fatal("PendingExport answered the LIVE epoch's exporter; the read is off the wrong schedule")
	}
	if bytes.Equal(pending.groupContext, liveBefore.groupContext) {
		t.Fatal("PendingEpoch answered the LIVE group context; the read is off the wrong value")
	}
	// the value is the caller's: writing through it changes nothing a second reading sees
	contextCopy := append([]byte(nil), pending.groupContext...)
	for i := range pending.groupContext {
		pending.groupContext[i] ^= 0xff
	}
	if again := readPending(t, "a second read", chain.founded); !bytes.Equal(again.groupContext, contextCopy) {
		t.Fatal("writing through PendingEpoch's GroupContext rewrote what the next read answers")
	}
	pending.groupContext = contextCopy

	// THE ANCHOR at the committer and at a receiver
	processed, err := chain.joined.Process(commit)
	if err != nil {
		t.Fatalf("the joiner's Process: %v", err)
	}
	if err := chain.joined.ApplyCommit(processed); err != nil {
		t.Fatalf("the joiner's ApplyCommit: %v", err)
	}
	if err := chain.founded.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	assertSameFacts(t, "the live handle after merging an Add against the pending facts read before it", readLive(t, "merged", chain.founded), pending)
	assertSameFacts(t, "a receiver after applying the same Add against the committer's pending facts", readLive(t, "receiver", chain.joined), pending)
	assertNothingPending(t, "a handle after the merge", chain.founded)

	// ── a policy: the context moves and the membership does not ───────────────────────────────
	policy := policyOf(t, contextExtensionsOf(t, chain.founded))
	policy.SetRole(chain.joiner.identityPub, mls.RoleAdmin)
	encoded, err := policy.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	liveBefore = readLive(t, "the founder before CommitPolicy", chain.founded)
	commit, _, _, err = chain.founded.CommitPolicy(encoded.ExtensionData)
	if err != nil {
		t.Fatalf("CommitPolicy: %v", err)
	}
	pending = readPending(t, "the founder after CommitPolicy", chain.founded)
	if pending.epoch != liveBefore.epoch+1 || pending.memberCount != liveBefore.memberCount {
		t.Fatalf("a policy commit stages epoch %d with %d members from epoch %d with %d", pending.epoch, pending.memberCount, liveBefore.epoch, liveBefore.memberCount)
	}
	if bytes.Equal(pending.groupContext, liveBefore.groupContext) || bytes.Equal(pending.mlsSecret, liveBefore.mlsSecret) {
		t.Fatal("the pending reads over a policy commit answered the live epoch's values")
	}
	processed, err = chain.joined.Process(commit)
	if err != nil {
		t.Fatalf("the joiner's Process of the policy commit: %v", err)
	}
	if err := chain.joined.ApplyCommit(processed); err != nil {
		t.Fatalf("the joiner's ApplyCommit of the policy commit: %v", err)
	}
	if err := chain.founded.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit of the policy commit: %v", err)
	}
	assertSameFacts(t, "the live handle after merging a policy commit against the pending facts", readLive(t, "merged", chain.founded), pending)
	assertSameFacts(t, "a receiver after applying the same policy commit", readLive(t, "receiver", chain.joined), pending)
}

// TestAClearedCommitLeavesNothingToRead is the losing committer's path through the seam: a
// staged commit whose epoch the delivery service gave to somebody else is cleared, and after the
// clear both reads refuse by name, the live handle stands where it stood, and the next commit
// stages against the SAME epoch -- which is what "re-derive against the winner and retry" costs
// at this layer, and all it costs.
func TestAClearedCommitLeavesNothingToRead(t *testing.T) {
	chain := newCommitAddChain(t, "pending-cleared")
	liveBefore := readLive(t, "before", chain.founded)
	policy := policyOf(t, contextExtensionsOf(t, chain.founded))
	policy.SetRole(chain.joiner.identityPub, mls.RoleObserver)
	encoded, err := policy.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if _, _, _, err := chain.founded.CommitPolicy(encoded.ExtensionData); err != nil {
		t.Fatalf("CommitPolicy: %v", err)
	}
	first := readPending(t, "the first staged commit", chain.founded)

	chain.founded.ClearPendingCommit()
	assertNothingPending(t, "a handle after ClearPendingCommit", chain.founded)
	assertSameFacts(t, "the live handle after a clear", readLive(t, "cleared", chain.founded), liveBefore)

	// the retry stages against the same epoch, and it is a fresh epoch and not the cleared one:
	// a new commit draws new secrets, so the exporter differs while the number is the same
	if _, _, _, err := chain.founded.CommitPolicy(encoded.ExtensionData); err != nil {
		t.Fatalf("CommitPolicy after a clear: %v", err)
	}
	second := readPending(t, "the second staged commit", chain.founded)
	if second.epoch != first.epoch {
		t.Fatalf("the retry stages epoch %d, want %d: a cleared commit moved the handle", second.epoch, first.epoch)
	}
	if bytes.Equal(second.mlsSecret, first.mlsSecret) {
		t.Fatal("the retry's staged exporter is the cleared commit's; a cleared epoch was reused")
	}
	chain.founded.ClearPendingCommit()
}
