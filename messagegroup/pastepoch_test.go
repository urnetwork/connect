// Ledger item 241: a member that was already in the group when a commit moved it on still opens
// the records sealed under the epochs it was a member of. The cases below are the seam's own --
// every one of them runs over two real engines, one of which commits alone and is then asked to
// open what the other sealed at the epoch it left.
package messagegroup

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/connect/message"
	"github.com/urnetwork/connect/mls"
)

// pastEpochPair is a testPair whose OPENER has moved on by itself: the sender is still at the
// epoch the pair was founded at, the opener has committed `epochs` times and advanced its session
// each time, and the opener's session has the loader installed that rebuilds a prior epoch out of
// its own engine's store.
//
// The loader COUNTS. How many times the session asked for a prior epoch is the measurement the
// "once per epoch, never per record" clause is held to, and a count is the only thing that can
// tell the cached reading from a per-record one, because both open every record.
type pastEpochPair struct {
	*testPair
	loads    map[uint64]int
	loadFail error
}

func newPastEpochPair(t *testing.T, name string) *pastEpochPair {
	t.Helper()
	pair := &pastEpochPair{testPair: newTestPair(t, name), loads: map[uint64]int{}}
	groupId := pair.chain.joined.GroupId()
	engine := pair.chain.b.engine
	if err := pair.opener.InstallPastEpochLoader(func(epoch uint64) (GroupHandle, error) {
		pair.loads[epoch] += 1
		if pair.loadFail != nil {
			return nil, pair.loadFail
		}
		return engine.LoadGroup(groupId, epoch)
	}); err != nil {
		t.Fatalf("InstallPastEpochLoader: %v", err)
	}
	return pair
}

// advanceOpener moves the OPENER alone to the next epoch: a commit it merges itself, and the
// session install that follows. The sender is left where it is, so everything it sealed before
// and everything it seals after stands at the epoch the opener has left.
func (self *pastEpochPair) advanceOpener(t *testing.T) {
	t.Helper()
	if _, _, _, err := self.chain.joined.Commit(nil); err != nil {
		t.Fatalf("the opener's Commit(nil): %v", err)
	}
	if err := self.chain.joined.MergePendingCommit(); err != nil {
		t.Fatalf("the opener's MergePendingCommit: %v", err)
	}
	if err := self.opener.AdvanceEpoch(self.chain.pqSecret); err != nil {
		t.Fatalf("the opener's AdvanceEpoch: %v", err)
	}
}

// sealDurable seals one DURABLE record at the sender, which is still at the founding epoch.
func (self *pastEpochPair) sealDurable(t *testing.T, body string) *message.Record {
	t.Helper()
	record, err := self.sender.SealRecord(message.RetentionDurable, 0, false, []byte("head"), []byte(body), 0, nil)
	if err != nil {
		t.Fatalf("the sender's SealRecord(%q): %v", body, err)
	}
	return record
}

// trackAt installs the opener's ladder over the sender's DURABLE stream at one PRIOR epoch.
func (self *pastEpochPair) trackAt(t *testing.T, epoch uint64, head uint64) {
	t.Helper()
	if err := self.opener.TrackSenderAt(epoch, self.senderLeaf, message.RetentionDurable, 0, 0, head); err != nil {
		t.Fatalf("the opener's TrackSenderAt(epoch %d, head %d): %v", epoch, head, err)
	}
}

// THE CASE THAT CARRIES THE RULING. Three records sealed at epoch one by a member who is still
// there; the opener commits alone to epoch two; every one of the three opens at the opener,
// under epoch one's schedule, with one load of that schedule for the three.
//
// THE CONTROL IS THE SAME THREE RECORDS AT A SESSION WITH NO LOADER, which is the session this
// package shipped before item 241 and is the refusal the milestone counted 602 of. Both halves
// are asserted on the same records so that the open is measured against the refusal it replaces
// rather than against nothing.
func TestAMemberThatWasThereOpensRecordsSealedAtAnEpochItHasLeft(t *testing.T) {
	pair := newPastEpochPair(t, "past-epoch-open")
	records := []*message.Record{}
	for i := range 3 {
		records = append(records, pair.sealDurable(t, fmt.Sprintf("epoch one, line %d", i)))
	}
	pair.advanceOpener(t)
	if epoch, _ := pair.opener.Epoch(); epoch != 2 {
		t.Fatalf("the opener is at epoch %d after its own commit, want 2", epoch)
	}
	if records[0].Header.Epoch != 1 {
		t.Fatalf("the sender sealed at epoch %d, want 1", records[0].Header.Epoch)
	}

	// THE CONTROL: the same records at a session that has no loader are the old refusal.
	control := newTestPair(t, "past-epoch-control")
	control.trackDurable(t)
	stale, err := control.sender.SealRecord(message.RetentionDurable, 0, false, []byte("head"), []byte("body"), 0, nil)
	if err != nil {
		t.Fatalf("the control's SealRecord: %v", err)
	}
	if _, _, _, err := control.chain.joined.Commit(nil); err != nil {
		t.Fatalf("the control opener's Commit: %v", err)
	}
	if err := control.chain.joined.MergePendingCommit(); err != nil {
		t.Fatalf("the control opener's MergePendingCommit: %v", err)
	}
	if err := control.opener.AdvanceEpoch(control.chain.pqSecret); err != nil {
		t.Fatalf("the control opener's AdvanceEpoch: %v", err)
	}
	if _, _, err := control.opener.OpenRecord(stale); !errors.Is(err, ErrRecordNotForThisSession) {
		t.Fatalf("a session with no loader answered %v for a record one epoch behind it, want ErrRecordNotForThisSession -- the control is not the old refusal", err)
	}

	// THE OPEN. The ladder is tracked at epoch ONE, at the head this opener had authenticated for
	// the sender as of that epoch, which is nothing.
	pair.trackAt(t, 1, 0)
	for i, record := range records {
		head, body, err := pair.opener.OpenRecord(record)
		if err != nil {
			t.Fatalf("record %d, sealed at epoch 1, did not open at a member now at epoch 2: %v", i, err)
		}
		if string(head) != "head" || string(body) != fmt.Sprintf("epoch one, line %d", i) {
			t.Errorf("record %d opened to %q / %q", i, head, body)
		}
	}
	// ONCE PER EPOCH AND NOT PER RECORD: three opens and one track cost one load.
	if pair.loads[1] != 1 {
		t.Errorf("epoch one's schedule was loaded %d time(s) for one track and three opens, want exactly 1; a schedule rebuilt per record starts every peer at generation zero and refuses the first record past MaxGenerationSkip",
			pair.loads[1])
	}
	// AND A RECORD FROM THE FUTURE IS STILL REFUSED with the old sentinel: the relaxation is
	// downward only.
	future := *records[0]
	future.Header.Epoch = 3
	if _, _, err := pair.opener.OpenRecord(&future); !errors.Is(err, ErrRecordNotForThisSession) {
		t.Errorf("a record naming epoch 3 at a session at epoch 2 answered %v, want ErrRecordNotForThisSession", err)
	}
	// AND A RE-DELIVERY IS REFUSED, which is the ladder's replay guard holding at a prior epoch
	// exactly as it does at the current one.
	if _, _, err := pair.opener.OpenRecord(records[0]); !errors.Is(err, ErrOutOfWindow) {
		t.Errorf("a second open of an epoch-one record answered %v, want ErrOutOfWindow", err)
	}
}

// THE WINDOW, EXACTLY. An epoch-one record opens while the opener is at most PastEpochWindow
// epochs past it -- at epoch 1+32 -- and is refused with ErrEpochOutOfWindow one epoch later, at
// 1+33. Both edges are driven so that a bound one off in either direction is red: at 33 the
// store still holds epoch one (MergePendingCommit's cutoff is 33-32 = 1 and it deletes BELOW the
// cutoff), and at 34 it does not, so the arithmetic here and mls's delete agree about the line.
func TestTheWindowIsExactlyPastEpochWindowEpochsAndNotOneMore(t *testing.T) {
	pair := newPastEpochPair(t, "past-epoch-window")
	atTheEdge := pair.sealDurable(t, "opens at the edge")
	pastTheEdge := pair.sealDurable(t, "refused past the edge")
	for range PastEpochWindow {
		pair.advanceOpener(t)
	}
	if epoch, _ := pair.opener.Epoch(); epoch != 1+PastEpochWindow {
		t.Fatalf("the opener is at epoch %d, want %d", epoch, 1+PastEpochWindow)
	}
	pair.trackAt(t, 1, 0)
	if _, body, err := pair.opener.OpenRecord(atTheEdge); err != nil || string(body) != "opens at the edge" {
		t.Fatalf("at epoch %d an epoch-one record -- exactly PastEpochWindow behind -- answered %v / %q; the window is one epoch too short",
			1+PastEpochWindow, err, body)
	}
	pair.advanceOpener(t)
	if _, _, err := pair.opener.OpenRecord(pastTheEdge); !errors.Is(err, ErrEpochOutOfWindow) {
		t.Fatalf("at epoch %d an epoch-one record -- PastEpochWindow+1 behind -- answered %v, want ErrEpochOutOfWindow; the window is one epoch too long",
			2+PastEpochWindow, err)
	}
	if err := pair.opener.TrackSenderAt(1, pair.senderLeaf, message.RetentionDurable, 0, 0, 0); !errors.Is(err, ErrEpochOutOfWindow) {
		t.Errorf("tracking at an epoch below the window answered %v, want ErrEpochOutOfWindow", err)
	}
	// and the refusal was taken by arithmetic, before the loader was asked: the loads of epoch
	// one are the ones the epoch changes cost while it was inside the window, and not one more.
	if pair.loads[1] != 1 {
		t.Errorf("epoch one was loaded %d time(s); it should have been loaded once, at epoch %d, and refused by arithmetic afterwards", pair.loads[1], 1+PastEpochWindow)
	}
}

// A MEMBER ADMITTED AT EPOCH TWO HOLDS NO EPOCH-ONE SCHEDULE, and the seam answers that as a
// loader refusal wrapping the store's own, never as an open. This is item 241's "the joiner half
// is free" as a measurement: nothing in this package decides who was a member when, and the
// device that was not there has nothing on its disk to rebuild the epoch from.
func TestAMemberAdmittedLaterCannotObtainTheEpochBeforeItsAdmission(t *testing.T) {
	pair := newPastEpochPair(t, "past-epoch-admitted-later")
	before := pair.sealDurable(t, "sealed before the third member existed")

	// the OPENER (B) adds a third member C, which is the commit that opens epoch two.
	c := newTestEngine(t)
	keyPackage, err := c.engine.NewKeyPackage()
	if err != nil {
		t.Fatalf("C's NewKeyPackage: %v", err)
	}
	_, welcome, ratchetTree, err := pair.chain.joined.CommitAdd([][]byte{keyPackage})
	if err != nil {
		t.Fatalf("B's CommitAdd: %v", err)
	}
	if err := pair.chain.joined.MergePendingCommit(); err != nil {
		t.Fatalf("B's MergePendingCommit: %v", err)
	}
	if err := pair.opener.AdvanceEpoch(pair.chain.pqSecret); err != nil {
		t.Fatalf("B's AdvanceEpoch: %v", err)
	}
	admitted, err := c.engine.JoinFromWelcome(welcome, ratchetTree)
	if err != nil {
		t.Fatalf("C's JoinFromWelcome: %v", err)
	}
	defer admitted.Close()
	if admitted.Epoch() != 2 {
		t.Fatalf("C was admitted at epoch %d, want 2", admitted.Epoch())
	}
	cSession, err := NewGroupSession(admitted, pair.chain.pqSecret, pair.chain.groupHandleKey,
		newStreamIndexMemory(), testClock(), testServerNonce())
	if err != nil {
		t.Fatalf("C's session: %v", err)
	}
	defer cSession.Close()
	groupId := admitted.GroupId()
	cLoads := 0
	if err := cSession.InstallPastEpochLoader(func(epoch uint64) (GroupHandle, error) {
		cLoads += 1
		return c.engine.LoadGroup(groupId, epoch)
	}); err != nil {
		t.Fatalf("C's InstallPastEpochLoader: %v", err)
	}

	// B, who WAS there, opens the record.
	pair.trackAt(t, 1, 0)
	if _, body, err := pair.opener.OpenRecord(before); err != nil || string(body) != "sealed before the third member existed" {
		t.Fatalf("B, a member at epoch one, answered %v / %q for an epoch-one record", err, body)
	}
	// C, who was NOT, is refused at the loader -- at the track and at the open alike -- and the
	// refusal carries the store's own answer.
	err = cSession.TrackSenderAt(1, pair.senderLeaf, message.RetentionDurable, 0, 0, 0)
	if !errors.Is(err, ErrPastEpochUnobtainable) {
		t.Fatalf("C's TrackSenderAt(1) answered %v, want ErrPastEpochUnobtainable", err)
	}
	if _, _, err := cSession.OpenRecord(before); !errors.Is(err, ErrPastEpochUnobtainable) {
		t.Fatalf("C's OpenRecord of an epoch-one record answered %v, want ErrPastEpochUnobtainable", err)
	}
	if cLoads != 2 {
		t.Errorf("C's loader was asked %d time(s) for two refusals, want 2: a refusal is not cached, because a store that answers not-found today may answer a state tomorrow", cLoads)
	}
	// the control for "the store's own answer is carried": the memory store's not-found text
	// is inside the refusal, which is what lets sdk tell "never a member then" from a disk
	// that would not read.
	if !strings.Contains(err.Error(), "no group state for") {
		t.Errorf("the refusal does not carry the store's own not-found answer: %v", err)
	}
}

// A LOADER THAT ANSWERS THE WRONG EPOCH, THE WRONG GROUP OR NOTHING IS REFUSED AND ITS HANDLE
// CLOSED. Each is a loader bug and each would otherwise derive a storage root that opens nothing
// and says nothing about why -- LoadGroup's own reason, held at this seam too.
func TestALoaderAnsweringTheWrongThingIsRefusedAndItsHandleClosed(t *testing.T) {
	pair := newPastEpochPair(t, "past-epoch-wrong-loader")
	record := pair.sealDurable(t, "a line")
	pair.advanceOpener(t)
	groupId := pair.chain.joined.GroupId()
	engine := pair.chain.b.engine

	// the wrong epoch: a loader that answers the CURRENT epoch's state when asked for epoch one.
	var handed GroupHandle
	if err := pair.opener.InstallPastEpochLoader(func(epoch uint64) (GroupHandle, error) {
		handle, err := engine.LoadGroup(groupId, 2)
		handed = handle
		return handle, err
	}); err != nil {
		t.Fatalf("InstallPastEpochLoader: %v", err)
	}
	if _, _, err := pair.opener.OpenRecord(record); !errors.Is(err, ErrPastEpochUnobtainable) {
		t.Errorf("a loader answering epoch 2 for epoch 1 was answered %v, want ErrPastEpochUnobtainable", err)
	}
	if handed == nil {
		t.Fatal("the loader was never asked")
	}
	if handed.EpochAuthenticator() != nil {
		t.Error("the handle at the wrong epoch was refused and not closed: its epoch authenticator is still answered")
	}

	// the wrong group.
	other := pair.chain.b.createGroup(t, "past-epoch-other-group")
	t.Cleanup(func() { other.Close() })
	if err := pair.opener.InstallPastEpochLoader(func(epoch uint64) (GroupHandle, error) {
		return other, nil
	}); err != nil {
		t.Fatalf("InstallPastEpochLoader: %v", err)
	}
	if _, _, err := pair.opener.OpenRecord(record); !errors.Is(err, ErrPastEpochUnobtainable) {
		t.Errorf("a loader answering another group was answered %v, want ErrPastEpochUnobtainable", err)
	}
	if other.EpochAuthenticator() != nil {
		t.Error("the handle over the wrong group was refused and not closed")
	}

	// nothing at all.
	if err := pair.opener.InstallPastEpochLoader(func(epoch uint64) (GroupHandle, error) {
		return nil, nil
	}); err != nil {
		t.Fatalf("InstallPastEpochLoader: %v", err)
	}
	if _, _, err := pair.opener.OpenRecord(record); !errors.Is(err, ErrPastEpochUnobtainable) {
		t.Errorf("a loader answering (nil, nil) was answered %v, want ErrPastEpochUnobtainable", err)
	}
	// and nil is not an uninstall.
	if err := pair.opener.InstallPastEpochLoader(nil); !errors.Is(err, ErrNilPastEpochLoader) {
		t.Errorf("InstallPastEpochLoader(nil) answered %v, want ErrNilPastEpochLoader", err)
	}
}

// THE ERASE. A prior epoch's schedule is class keys, ladders and an open mls group, and every one
// of the three is gone -- zeroized, zeroized, closed -- at the next epoch change and at Close.
// The arrays are ALIASED before the drop, for the reason every erase case in this package aliases:
// "all zero afterwards" over a copy is a photograph.
func TestAPriorEpochsScheduleIsErasedAtTheNextEpochChangeAndAtClose(t *testing.T) {
	for _, drop := range []struct {
		name string
		do   func(t *testing.T, pair *pastEpochPair)
	}{
		{name: "the next epoch change", do: func(t *testing.T, pair *pastEpochPair) { pair.advanceOpener(t) }},
		{name: "Close", do: func(t *testing.T, pair *pastEpochPair) {
			if err := pair.opener.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
		}},
	} {
		t.Run(drop.name, func(t *testing.T) {
			pair := newPastEpochPair(t, "past-epoch-erase-"+drop.name)
			record := pair.sealDurable(t, "a line")
			pair.advanceOpener(t)
			pair.trackAt(t, 1, 0)
			if _, _, err := pair.opener.OpenRecord(record); err != nil {
				t.Fatalf("OpenRecord: %v", err)
			}
			// the aliases, read off the loop so that the read is not a race.
			var held *pastEpoch
			var keys [][]byte
			if err := pair.opener.do(func() {
				held = pair.opener.pastEpochs[1]
				if held != nil {
					keys = [][]byte{held.classKeys.Perm, held.classKeys.Durable, held.classKeys.Media}
				}
			}); err != nil {
				t.Fatalf("do: %v", err)
			}
			if held == nil {
				t.Fatal("the session holds no schedule for epoch one after opening a record at it, so this case would be asserting an erase over nothing")
			}
			handle := held.handle
			for i, key := range keys {
				if len(key) == 0 || !containsNonZero(key) {
					t.Fatalf("class key %d is empty or all zero before the drop, so an all zero reading afterwards would say nothing", i)
				}
			}
			if handle.EpochAuthenticator() == nil {
				t.Fatal("the loaded handle is already closed before the drop")
			}
			drop.do(t, pair)
			for i, key := range keys {
				if containsNonZero(key) {
					t.Errorf("class key %d of epoch one survived %s: %x", i, drop.name, key)
				}
			}
			if handle.EpochAuthenticator() != nil {
				t.Errorf("the loaded epoch-one group survived %s open: its epoch authenticator is still answered", drop.name)
			}
			if _, err := handle.Export(mlsSecretLabel, nil, mlsSecretBytes); err == nil {
				t.Errorf("the loaded epoch-one group still exports after %s", drop.name)
			}
			var remaining int
			if err := pair.opener.do(func() { remaining = len(pair.opener.pastEpochs) }); err == nil && remaining != 0 {
				t.Errorf("%d prior epoch(s) remain on the session after %s", remaining, drop.name)
			}
		})
	}
}

// THE DISCRIMINATOR FOR "ONCE PER EPOCH": a sender past MaxGenerationSkip in the epoch it is
// still at. Every record opens at the opener one epoch on, because the ONE schedule it rebuilt
// walks the sender's ratchet forward as a live group would; a schedule rebuilt per record would
// stand at generation zero for each and refuse the 1,026th with ErrRatchetGenerationTooFarAhead.
// The small cases above stay green under that mutation, which is why this one exists.
func TestASenderPastTheGenerationSkipBoundStillOpensAtAPriorEpoch(t *testing.T) {
	if testing.Short() {
		t.Skip("this case seals past MaxGenerationSkip records; skipped under -short")
	}
	pair := newPastEpochPair(t, "past-epoch-generation-skip")
	count := int(mls.MaxGenerationSkip) + 2
	records := make([]*message.Record, 0, count)
	for i := range count {
		records = append(records, pair.sealDurable(t, fmt.Sprintf("%d", i)))
	}
	pair.advanceOpener(t)
	pair.trackAt(t, 1, 0)
	for i, record := range records {
		if _, body, err := pair.opener.OpenRecord(record); err != nil || string(body) != fmt.Sprintf("%d", i) {
			t.Fatalf("record %d of %d did not open at the prior epoch: %v / %q", i, count, err, body)
		}
	}
	if pair.loads[1] != 1 {
		t.Errorf("epoch one was loaded %d time(s) for %d records, want 1", pair.loads[1], count)
	}
}

// AND THE HEAD IS THE CALLER'S: a ladder tracked at a prior epoch at the head the opener had
// authenticated opens the records above it and refuses the ones below, exactly as TrackSender's
// does at the current epoch. This is the shape sdk's per-(sender, epoch) head persistence feeds.
func TestAPriorEpochLadderIsTrackedAtTheCallersHead(t *testing.T) {
	pair := newPastEpochPair(t, "past-epoch-head")
	records := []*message.Record{}
	for i := range 4 {
		records = append(records, pair.sealDurable(t, fmt.Sprintf("%d", i)))
	}
	pair.advanceOpener(t)
	// tracked at the third record's index: the first two are below the head and refused, the
	// last two open.
	pair.trackAt(t, 1, records[2].Header.StreamIndex)
	for i := range 2 {
		if _, _, err := pair.opener.OpenRecord(records[i]); !errors.Is(err, ErrOutOfWindow) {
			t.Errorf("record %d, below the head, answered %v, want ErrOutOfWindow", i, err)
		}
	}
	for i := 2; i < 4; i += 1 {
		if _, body, err := pair.opener.OpenRecord(records[i]); err != nil || string(body) != fmt.Sprintf("%d", i) {
			t.Errorf("record %d, at or above the head, answered %v / %q", i, err, body)
		}
	}
}

// containsNonZero is the live control every erase assertion here needs.
func containsNonZero(octets []byte) bool {
	for _, octet := range octets {
		if octet != 0 {
			return true
		}
	}
	return false
}
