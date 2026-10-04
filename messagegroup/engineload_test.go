// GroupEngine.LoadGroup, the fifth method, and the property it exists for.
//
// WHAT WAS WRONG BEFORE IT. Section 6's block has four methods and none of them opens a group that
// is already on the disk, so a device that had persisted an epoch state had no door back into it
// through this interface. sdk closed that with its own `messagegroup.GroupHandle` over
// `mls.LoadGroup` -- a second implementation of twenty six methods, forced by the visibility rules
// rather than chosen, since `connectMlsHandle` is unexported. Two of those twenty six could not be
// written at all: `EngineProcessed.stagedRef` is unexported, so the only `EngineProcessed` a
// foreign implementation can build is one `ApplyCommit` refuses. sdk's copy refused `Process` and
// `ApplyCommit` by name, and a restored group therefore could not ingest a commit or follow its own
// group into the next epoch. That was open item J1-8.
//
// SO THE CASE THAT MATTERS IS THE ONE THAT REFUSED. TestEngineLoadGroupIngestsACommitAndEntersTheNextEpoch
// drives Process and ApplyCommit on a handle that came out of LoadGroup and nowhere else, and
// asserts the epoch MOVED. Nothing else in this file can fail for that reason.
package messagegroup

import (
	"bytes"
	"testing"

	"github.com/urnetwork/connect/mls"
)

// epochSwappingStore answers ONE named epoch's request with ANOTHER epoch's stored blob, and is
// otherwise the store it wraps.
//
// IT IS A STORE THAT LIES AND NOT A STORE THAT IS BROKEN, which is the distinction the refusal it
// probes exists for. A store that answered an error would be reported by mls.LoadGroup and nothing
// here would be needed; this one answers a blob that DECODES, rebuilds into a group whose tree,
// schedule and exporter are all real, and is at an epoch the caller did not ask for. There is no
// octet anywhere on that path that says so -- which is why the comparison is made by the adapter
// rather than left to mls, whose own header states it reads the row it was handed and rebuilds
// whatever is in it.
type epochSwappingStore struct {
	mls.StateStore
	answerAt uint64
	with     uint64
	swapping bool
}

func (self *epochSwappingStore) GetGroupState(groupId []byte, epoch uint64) ([]byte, error) {
	if self.swapping && epoch == self.answerAt {
		return self.StateStore.GetGroupState(groupId, self.with)
	}
	return self.StateStore.GetGroupState(groupId, epoch)
}

// TestEngineLoadGroupReopensThePersistedGroupAtTheEpochItNames is the door itself: a group founded
// and committed in one engine, closed, and reopened out of the store with nothing carried in memory
// but the group id and the epoch.
//
// THE EXPORTER IS THE ASSERTION AND NOT THE EPOCH NUMBER. Every key the record layer derives hangs
// off `Export`, so a reopen that answered the right epoch and a different exporter would be a member
// that agrees with its peers about which epoch it is in and about nothing else. The two epochs are
// BOTH reopened and their exporters compared, which is what says the epoch parameter selects a row
// rather than being decoration on a call that always answers the latest.
func TestEngineLoadGroupReopensThePersistedGroupAtTheEpochItNames(t *testing.T) {
	fixture := newTestEngine(t)
	groupId := testGroupId("load-reopen")
	handle := fixture.createGroup(t, "load-reopen")

	atZero, err := handle.Export("URmessage/v1/storage", nil, 32)
	if err != nil {
		t.Fatalf("the epoch zero exporter: %v", err)
	}
	if _, _, _, err := handle.Commit(nil); err != nil {
		t.Fatalf("Commit(nil): %v", err)
	}
	if err := handle.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	if epoch := handle.Epoch(); epoch != 1 {
		t.Fatalf("the founded group is at epoch %d after one merge, want 1", epoch)
	}
	atOne, err := handle.Export("URmessage/v1/storage", nil, 32)
	if err != nil {
		t.Fatalf("the epoch one exporter: %v", err)
	}
	// THE PROCESS GOES AWAY, as far as this interface can make it: the live group is closed, so
	// every assertion below is answered out of the store.
	if err := handle.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := fixture.engine.LoadGroup(groupId, 1)
	if err != nil {
		t.Fatalf("LoadGroup at epoch 1: %v", err)
	}
	defer reopened.Close()
	if epoch := reopened.Epoch(); epoch != 1 {
		t.Errorf("the reopened group stands at epoch %d, want 1", epoch)
	}
	if !bytes.Equal(reopened.GroupId(), groupId) {
		t.Errorf("the reopened group names %x and the persisted one is %x", reopened.GroupId(), groupId)
	}
	reopenedAtOne, err := reopened.Export("URmessage/v1/storage", nil, 32)
	if err != nil {
		t.Fatalf("the reopened exporter: %v", err)
	}
	if !bytes.Equal(reopenedAtOne, atOne) {
		t.Errorf("the reopened group exports %x and the live one exported %x at the same epoch; every key of the record layer is a function of this value",
			reopenedAtOne, atOne)
	}

	// AND THE EPOCH PARAMETER SELECTS. The same group id at epoch 0 is a different row and must
	// answer the value that epoch had.
	earlier, err := fixture.engine.LoadGroup(groupId, 0)
	if err != nil {
		t.Fatalf("LoadGroup at epoch 0: %v", err)
	}
	defer earlier.Close()
	if epoch := earlier.Epoch(); epoch != 0 {
		t.Errorf("the group reopened at epoch 0 stands at epoch %d", epoch)
	}
	reopenedAtZero, err := earlier.Export("URmessage/v1/storage", nil, 32)
	if err != nil {
		t.Fatalf("the epoch zero reopened exporter: %v", err)
	}
	if !bytes.Equal(reopenedAtZero, atZero) {
		t.Errorf("reopening at epoch 0 exports %x and the live group exported %x at epoch 0", reopenedAtZero, atZero)
	}
	if bytes.Equal(reopenedAtZero, reopenedAtOne) {
		t.Error("the two reopened epochs export the same octets, so the epoch argument reached no row of the store")
	}
}

// TestEngineLoadGroupRefusesAnEpochTheStoreDidNotAnswerAt is the named refusal, driven against a
// store that ANSWERS rather than one that fails.
//
// mls.LoadGroup is handed a key and rebuilds the blob it gets back; it never compares the epoch in
// that blob against the epoch it was asked for, and its own header says as much about the version
// field one line over. So a store that hands back the wrong row produces a whole, valid, internally
// consistent group at an epoch the caller never named -- the exporter is real, the tree is real, the
// schedule is real -- and nothing downstream has any way to tell. This is the check that tells.
func TestEngineLoadGroupRefusesAnEpochTheStoreDidNotAnswerAt(t *testing.T) {
	memory := newMemoryStateStore()
	lying := &epochSwappingStore{StateStore: memory, answerAt: 9, with: 1}
	fixture, err := buildTestEngineOver(lying, memory)
	if err != nil {
		t.Fatalf("build the engine over the swapping store: %v", err)
	}
	groupId := testGroupId("load-swapped")
	handle := fixture.createGroup(t, "load-swapped")
	if _, _, _, err := handle.Commit(nil); err != nil {
		t.Fatalf("Commit(nil): %v", err)
	}
	if err := handle.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	if err := handle.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// THE CONTROL, and it runs FIRST so that a failure below cannot be read as "epoch 9 was never
	// reachable anyway": with the swap OFF, epoch 9 is a row the store does not hold and the
	// refusal comes from mls rather than from this adapter.
	if _, err := fixture.engine.LoadGroup(groupId, 9); err == nil {
		t.Fatal("LoadGroup answered a handle for an epoch this store holds no state at")
	} else if errorIs(err, ErrEngineLoadedEpoch) {
		t.Errorf("a store that holds nothing at epoch 9 answered ErrEngineLoadedEpoch (%v); that sentinel is for a store that ANSWERED the wrong row, and a refusal that cannot tell the two apart names neither",
			err)
	}

	lying.swapping = true
	loaded, err := fixture.engine.LoadGroup(groupId, 9)
	if !errorIs(err, ErrEngineLoadedEpoch) {
		t.Errorf("LoadGroup over a store answering epoch 1's state at epoch 9 answered %v, want ErrEngineLoadedEpoch", err)
	}
	if loaded != nil {
		loaded.Close()
		t.Error("LoadGroup answered a handle beside its refusal; a group at an epoch nobody asked for seals records every peer refuses for a reason this device cannot name")
	}
}

// TestEngineLoadGroupIngestsACommitAndEntersTheNextEpoch IS THE CASE J1-8 WAS ABOUT.
//
// The exact operation asserted here -- Process(commit) then ApplyCommit(processed) on a RESTORED
// group -- is the pair sdk's own handle refused by name, because the staged commit those two carry
// lives in an unexported field of this package's EngineProcessed and no implementation outside this
// package can write one. Nothing before this method existed could drive it, in either repository.
//
// WHY THERE ARE TWO ENGINES. A commit a group ingests has to come from ANOTHER member: a committer
// merges its own staged commit through MergePendingCommit and never through Process, so a one-member
// group cannot exercise the ingest path at all. The joiner here is a second device with its own
// store, its own identity and its own engine.
//
// AND THE EXPORTERS ARE COMPARED AFTER THE APPLY. An epoch counter that moved is not the property:
// a restored member that entered epoch 2 with a schedule the committer does not share is a member
// that agrees about the number and about nothing else, and every record either side sealed would be
// refused by the other with nothing to say why.
func TestEngineLoadGroupIngestsACommitAndEntersTheNextEpoch(t *testing.T) {
	founder := newTestEngine(t)
	joiner := newTestEngine(t)
	groupId := testGroupId("load-ingest")

	keyPackage, err := joiner.engine.NewKeyPackage()
	if err != nil {
		t.Fatalf("the joiner's NewKeyPackage: %v", err)
	}
	founded := founder.createGroup(t, "load-ingest")
	if _, err := founded.ProposeAdd(keyPackage); err != nil {
		t.Fatalf("ProposeAdd: %v", err)
	}
	_, welcome, ratchetTree, err := founded.Commit(nil)
	if err != nil {
		t.Fatalf("Commit(nil) over the add: %v", err)
	}
	if err := founded.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	joined, err := joiner.engine.JoinFromWelcome(welcome, ratchetTree)
	if err != nil {
		t.Fatalf("JoinFromWelcome: %v", err)
	}
	defer joined.Close()
	if epoch := joined.Epoch(); epoch != 1 {
		t.Fatalf("the joiner stands at epoch %d, want 1", epoch)
	}
	// THE FOUNDER'S PROCESS DIES. Everything below is answered out of the store.
	if err := founded.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	restored, err := founder.engine.LoadGroup(groupId, 1)
	if err != nil {
		t.Fatalf("LoadGroup at epoch 1: %v", err)
	}
	defer restored.Close()

	// the OTHER member commits, and this one has to follow it.
	commit, _, _, err := joined.Commit(nil)
	if err != nil {
		t.Fatalf("the joiner's Commit(nil): %v", err)
	}
	if err := joined.MergePendingCommit(); err != nil {
		t.Fatalf("the joiner's MergePendingCommit: %v", err)
	}
	if epoch := joined.Epoch(); epoch != 2 {
		t.Fatalf("the committer stands at epoch %d after its own merge, want 2", epoch)
	}

	processed, err := restored.Process(commit)
	if err != nil {
		t.Fatalf("the restored group's Process over a commit: %v", err)
	}
	if processed == nil {
		t.Fatal("Process answered no processed message and no error")
	}
	if processed.Kind != EngineProcessedCommit {
		t.Fatalf("Process discriminated the commit as kind %d, want %d", processed.Kind, EngineProcessedCommit)
	}
	if err := restored.ApplyCommit(processed); err != nil {
		t.Fatalf("the restored group's ApplyCommit: %v", err)
	}
	if epoch := restored.Epoch(); epoch != 2 {
		t.Fatalf("the restored group stands at epoch %d after ingesting one commit, want 2; this is the operation open item J1-8 named", epoch)
	}

	restoredSecret, err := restored.Export("URmessage/v1/storage", nil, 32)
	if err != nil {
		t.Fatalf("the restored group's exporter at epoch 2: %v", err)
	}
	committerSecret, err := joined.Export("URmessage/v1/storage", nil, 32)
	if err != nil {
		t.Fatalf("the committer's exporter at epoch 2: %v", err)
	}
	if !bytes.Equal(restoredSecret, committerSecret) {
		t.Errorf("the restored member exports %x at epoch 2 and the committer exports %x; an epoch counter that agrees and a schedule that does not is a member every peer refuses",
			restoredSecret, committerSecret)
	}
}
