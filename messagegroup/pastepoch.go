// The multi-epoch open, ledger item 241: a member that was ALREADY IN the group when a commit
// moved it on still opens the records sealed under the epochs it was a member of.
//
// WHAT WAS MISSING WAS ONE SEAM AND NOT A DESIGN. Every key a record at epoch n opens under hangs
// off storage_root[n] = StorageRoot(mls_secret[n], pq_secret), and mls_secret[n] is the exporter
// of the epoch n key schedule -- which (*mls.Group).MergePendingCommit ZEROIZES in place when the
// group moves to n+1. The one copy of that schedule this device still holds is the epoch n state
// blob connect/mls persists at every merge and keeps for PastEpochWindow epochs, and
// GroupEngine.LoadGroup(groupId, n) is the door that rebuilds a group out of it. So there was
// never an ExportAt to write: an exporter over an erased schedule is LoadGroup(n).Export in
// disguise, and Export alone would not have been enough, because the ct_body of an application
// record is an MLS PrivateMessage framed under epoch n's secret tree and (*mls.Group).ProcessMessage
// refuses a frame naming any epoch but its own. A prior epoch's schedule is therefore a whole
// epoch n GroupHandle, and the door that answers one already existed. This file is what the
// session does with it.
//
// THE SESSION STAYS SINGLE-EPOCH. Every field of GroupSession still describes the epoch the
// handle is at; what this file adds is a table of PRIOR epochs' read schedules, each built on
// demand out of a handle the injected loader answered, each holding exactly what an open needs --
// the handle, the three class keys and the receiver ladders -- and nothing a seal needs. There is
// no write key, no read key, no sender ladder and no eph_root for a prior epoch: a record is never
// sealed at an epoch that has closed (spec A section 5.7 discards and re-seals), and eph_root[n]
// was dropped and erased at the change for MASTER section 8.1's reason, so an EPH record of a prior
// epoch refuses with ErrNoEphRoot exactly as one of the current epoch does before its root is
// installed. That is ledger item 186's still-unruled schedule and this file neither creates nor
// worsens it.
//
// WHAT IS RETAINED, FOR HOW LONG, AND WHAT ERASES IT, because this is the sentence the erase
// discipline asks of every type here. A pastEpoch is built by the first open or track that needs
// it and held on the session until the session's epoch moves or the session closes -- the
// "Zeroize-declaring cache" reading of item 241's verified deliverable rather than the per-open
// reading, chosen for two measured reasons. First, mls consumes a generation on every Unprotect
// and (*mls.LoadGroup) rebuilds every PEER'S receiving ratchet at generation zero, because the
// state blob carries only this member's own sender position; a schedule rebuilt per record would
// start every open from zero and refuse the first record more than MaxGenerationSkip generations
// along, while one held across the opens walks the ratchet forward as a live group does. Second,
// the receiver ladders retain skipped rungs, and a ladder rebuilt per record retains nothing. Every
// pastEpoch is erased -- class keys zeroized, ladders zeroized, the handle closed, which erases the
// epoch's schedule and secret tree -- at installEpochOnLoop, which is every epoch change, and at
// Close. Nothing is ever written BACK to the store from a prior epoch's handle: the blob's own
// format carries no receiving position to update, and a seal through one is not a door this file
// opens.
//
// THE BOUND IS THE ONE MLS ALREADY ENFORCES. MergePendingCommit deletes every state older than
// epoch - PastEpochWindow, so a record from below that line has no blob to rebuild from wherever
// this session looks; the refusal is taken here, by arithmetic, before any loader is asked, and it
// is ErrEpochOutOfWindow rather than a store's not-found. A record from an epoch this device was
// not yet admitted at has no blob either -- a device persists the epochs it stood in and no other
// -- and that refusal comes back through the loader as ErrPastEpochUnobtainable wrapping whatever
// the store said. Item 241 calls that half free and it is: nothing here has to decide who was a
// member when, because a device holds a schedule for exactly the epochs it was one.
package messagegroup

import (
	"crypto/subtle"
	"fmt"

	"github.com/urnetwork/connect/message"
	"github.com/urnetwork/connect/mls"
)

// PastEpochWindow is how many epochs back a session will open a record from: the same bound
// connect/mls keeps state for, read from the one declaration rather than written twice. A record
// at epoch n opens while current - n <= PastEpochWindow, and current - PastEpochWindow is the last
// epoch MergePendingCommit's delete leaves standing.
const PastEpochWindow = mls.PastEpochWindow

// PastEpochLoader answers a GroupHandle standing at one PRIOR epoch of the group a session is
// over, rebuilt out of the state this device persisted at that epoch, or an error when that state
// is not obtainable.
//
// It is a function and not a method on GroupHandle because the handle a session holds is a view
// of ONE epoch and has no store, no crypto provider and no signer to rebuild another from; those
// are the engine's, and GroupEngine.LoadGroup is the door that takes them. A caller that holds
// an engine installs `func(epoch) { return engine.LoadGroup(groupId, epoch) }` and nothing
// wider. The handle it answers is the session's from the moment it is returned: the session
// closes it when it drops the epoch.
type PastEpochLoader func(epoch uint64) (GroupHandle, error)

// pastEpoch is one prior epoch's read schedule, and it is the whole of what an open at that epoch
// reaches: the handle whose Unprotect opens the inner frame, the class keys every record key of
// the epoch walks from, and the receiver ladders tracked for it.
//
// It declares Zeroize and names every field that holds key material, which is what connect/mls's
// erase reading asks of a type its class reaches -- and this one is reached twice over, through
// ClassKeys and through ReceiverRatchets. The handle is closed by the same erase because it IS
// key material: an open mls.Group holds the epoch's key schedule and its secret tree, and Close is
// the erase mls declares for both.
//
// AND ONE FIELD OF IT IS NOT KEY MATERIAL AND IS DELIBERATELY NOT IN Zeroize: roles, item 242's
// ruling 18. See epochRole for what it holds, why nothing in it is a secret, and why it must
// still not outlive the handle above.
type pastEpoch struct {
	handle    GroupHandle
	at        uint64
	classKeys *ClassKeys
	receivers *ReceiverRatchets
	// this epoch's leaf -> (identity, role) table, built on the first ask and nil until then.
	// It is erased by NOTHING and dropped WITH THIS WHOLE VALUE at installEpochOnLoop, which is
	// the point: see epochRole.
	roles map[uint32]epochRole
}

// epochRole is one leaf's credential identity and its role AT ONE EPOCH, as the seam spells both.
//
// NOTHING HERE IS KEY MATERIAL AND IT IS NOT IN pastEpoch.Zeroize -- item 242's ruling 18, and the
// reason is the one connect/mls's erase table already writes for Group.cred and Group.context. A
// credential identity is published in its own leaf node, which every member holds and every joiner
// is handed in its Welcome; a role is a row of urmessage_group_policy, which is an entry of the
// group context extension list that the transcript covers and that travels beside every Welcome.
// Erasing either would destroy a value every member of the group already has.
//
// AND YET THE TABLE MUST NOT OUTLIVE ITS HANDLE, which is a LIFETIME rule and not an erase rule.
// The table is a projection of ONE epoch's ratchet tree and ONE epoch's policy; the handle it was
// projected from is the only thing that says which epoch that was. installEpochOnLoop re-makes
// self.pastEpochs wholesale at every epoch install, and a table held anywhere else -- a map on the
// session keyed by epoch, say -- would survive that and answer an epoch's question with a role
// read at another epoch, which is precisely the mistake item 242's ruling 21 exists to prevent.
// So the table is a FIELD OF THE SCHEDULE, and the schedule's death is its death. The session's
// own epoch has the same table in self.roles, dropped in the same body at the same line.
type epochRole struct {
	identityPub []byte
	role        string
}

// Zeroize erases the class keys and every ladder, closes the handle, and drops all three.
//
// The noinline directive is this package's erase helper class: the stores land in arrays this
// value is not the only holder of, and they are dead in the compiler's reading.
//
//go:noinline
func (self *pastEpoch) Zeroize() {
	if self == nil {
		return
	}
	self.classKeys.Zeroize()
	if self.receivers != nil {
		self.receivers.Zeroize()
	}
	if self.handle != nil {
		self.handle.Close()
	}
	self.classKeys = nil
	self.receivers = nil
	self.handle = nil
}

// InstallPastEpochLoader gives this session the door it rebuilds a prior epoch's schedule through.
//
// WHY A SETTER, for InstallEphRoot's reason one file over: a session is built at every epoch by
// callers that have no loader and need none -- the founding session at epoch zero, every fixture
// that seals and opens within one epoch -- and a required constructor argument would have made
// each of them supply a value it does not have. A session with no loader installed refuses a
// record from a prior epoch exactly as it did before this file existed, with
// ErrRecordNotForThisSession, which is the whole of the old behaviour and is what every caller
// that never installs one keeps.
//
// The loader is refused if nil rather than taken as an uninstall, because a nil handed in by
// mistake would silently return a group to the single-epoch behaviour and nothing downstream
// could tell.
func (self *GroupSession) InstallPastEpochLoader(loader PastEpochLoader) error {
	var err error
	if postErr := self.do(func() {
		if self.closing {
			err = ErrSessionClosed
			return
		}
		if loader == nil {
			err = ErrNilPastEpochLoader
			return
		}
		self.pastEpochLoader = loader
	}); postErr != nil {
		return postErr
	}
	return err
}

// TrackSenderAt is TrackSender for a PRIOR epoch: it installs a receiver ratchet for one peer's
// ladder in one retention class, in that epoch's schedule, positioned at headIndex.
//
// headIndex is the CALLER'S state exactly as TrackSender's is, and for a prior epoch the caller
// has one more thing to get right about it: it is the head this device had authenticated for that
// sender AS OF THAT EPOCH, never a head raised by a later epoch's records. The stream index is
// continuous across epochs, so a head taken from epoch n+1 stands above every record of epoch n
// and a ladder tracked there answers ErrOutOfWindow for all of them.
//
// An epoch at or above the session's own is refused: the current epoch's ladders are TrackSender's
// and a future epoch has no schedule anywhere. The loader is consulted on the first call for an
// epoch and never again while the session stays at its epoch; see the file header for what the
// schedule holds and when it is erased.
func (self *GroupSession) TrackSenderAt(epoch uint64, leaf uint32, class message.RetentionClass,
	ephBucket uint8, ephWindow uint64, headIndex uint64) error {

	var err error
	if postErr := self.do(func() {
		if self.closing {
			err = ErrSessionClosed
			return
		}
		err = self.trackSenderAtOnLoop(epoch, leaf, class, ephBucket, ephWindow, headIndex)
	}); postErr != nil {
		return postErr
	}
	return err
}

// trackSenderAtOnLoop is TrackSenderAt's body. The caller is the loop goroutine.
//
// It is trackSenderOnLoop with the schedule chosen by epoch rather than read off the session, and
// the two bodies are kept apart rather than merged for the reason the file header gives: the
// current epoch's ladder derives its class key through classKeyOnLoop, which reaches eph_root, and
// a prior epoch has no eph_root to reach.
func (self *GroupSession) trackSenderAtOnLoop(epoch uint64, leaf uint32, class message.RetentionClass,
	ephBucket uint8, ephWindow uint64, headIndex uint64) error {

	if self.epoch <= epoch {
		return fmt.Errorf("%w: epoch %d is not a prior epoch of a session at %d", ErrRecordNotForThisSession, epoch, self.epoch)
	}
	past, err := self.pastEpochOnLoop(epoch)
	if err != nil {
		return err
	}
	retentionWire, err := message.RetentionClassWire(class, ephBucket)
	if err != nil {
		return err
	}
	ladderWindow := ephLadderWindow(class, ephWindow)
	classKey, err := classKeyOf(past.classKeys, nil, class, ephBucket, ladderWindow)
	if err != nil {
		return err
	}
	ratchet, err := NewReceiverRatchet(classKey, leaf, headIndex, self.windowSize)
	if err != nil {
		return err
	}
	past.receivers.Track(ReceiverRatchetKey{
		SenderHandle:  SenderHandle(self.groupHandleKey, leaf),
		RetentionWire: retentionWire,
		EphWindow:     ladderWindow,
	}, ratchet)
	return nil
}

// scheduleForOnLoop answers the handle and the receiver table a record at one epoch opens
// through: the session's own for its current epoch, a prior epoch's schedule for an epoch inside
// the window, and a refusal for everything else.
//
// THIS IS THE RELAXATION OF THE EPOCH CHECK openRecordOnLoop USED TO MAKE WITH ==. The three
// refusals are three different sentences because sdk renders them differently: a record from a
// FUTURE epoch is one this session is not keyed for and a re-fetch after the commit repairs it; a
// record from below the window is a gap no re-fetch ever repairs; and a record from an epoch this
// device holds no state for is a gap too, carrying the store's own answer for whoever wants to
// tell "never a member then" from "the disk is broken".
//
// The caller is the loop goroutine.
func (self *GroupSession) scheduleForOnLoop(epoch uint64) (GroupHandle, *ReceiverRatchets, error) {
	if epoch == self.epoch {
		return self.handle, self.receivers, nil
	}
	if self.epoch < epoch {
		return nil, nil, fmt.Errorf("%w: epoch %d is ahead of this session's %d", ErrRecordNotForThisSession, epoch, self.epoch)
	}
	past, err := self.pastEpochOnLoop(epoch)
	if err != nil {
		return nil, nil, err
	}
	return past.handle, past.receivers, nil
}

// RoleAt answers the credential identity standing at one LEAF at one EPOCH of this group, and the
// role that identity held AT THAT EPOCH.
//
// THE IDENTITY BEHIND A ROLE CHECK IS READ OFF THE RECORD'S OWN EPOCH'S TREE AND NEVER OFF
// walk.leaves. SenderHandle(group_handle_key, leaf) is a function of the leaf index and a
// group-lifetime key and nothing else, so a handle can never distinguish two identities that
// occupied one leaf at different epochs (ledger item 245); walk.leaves stays a cheap pre-filter
// answering "is this handle a leaf of this group at all", and the authentication is R1 inside
// OpenRecord. A caller asks this with the leaf that open AUTHENTICATED and with that record's own
// epoch, never with a leaf it resolved from a header's claimed sender_handle.
//
// IT READS THE HANDLE AN OPEN AT THAT EPOCH READS AND NO OTHER -- item 242's ruling 17, and the
// whole of what that ruling buys is here: the route is scheduleForOnLoop, the same three-armed
// lookup openRecordOnLoop takes, so a door reading the same handle the open read CANNOT FAIL WHERE
// THE OPEN SUCCEEDED, and every refusal it can answer is one that record's own open answered
// first, by the same sentinel. A second LoadGroup on the sdk side would have manufactured an
// "undeterminable role" state this session never produces, and a past epoch's handle is key
// material with an erase owner besides.
//
// AND THE CONDITION ON THAT SENTENCE, MEASURED RATHER THAN ASSUMED: it holds for an ask taken with
// NO EPOCH INSTALL BETWEEN IT AND THE OPEN. While a schedule is held, nothing moves what this
// answers -- the only method an open calls on the epoch's handle is Unprotect, and (*mls.Group)
// writes its tree and its context in the merge and nowhere else, so any number of opens leave
// Members() saying the same thing. installEpochOnLoop is the one thing that breaks it: it closes
// every held handle and re-makes self.pastEpochs wholesale, and it is reachable BETWEEN two
// records of one walk, because the sdk ingests a commit in the same loop that opens application
// records. After one, the next ask at epoch n is a fresh load rather than the open's own handle,
// and that load can refuse where the open did not -- a record that opened at exactly
// PastEpochWindow behind is PastEpochWindow+1 behind after the install, and the state mls keeps
// for it has been deleted by the same merge. That is why item 242's ruling 21 CAPTURES the role
// AT OPEN rather than deriving it at render: the capture is what keeps the ask inside the window
// this paragraph describes.
//
// THE ANSWER IS A FACT ABOUT AN EPOCH AND NOT ABOUT NOW. A member demoted at epoch n+1 held its
// old role at epoch n and everything it sent there was sent with it (item 242's ruling 21, and
// Spec A's "read from the transcript-covered group-context extension of the SENDING EPOCH -- never
// from current membership"). A caller that wants the role now asks at the session's own epoch.
//
// The three refusals are scheduleForOnLoop's, unchanged and by the same sentinels; a leaf nobody
// stands at, at an epoch this session DID reach, is ErrEngineMemberLeaf and is a different thing.
func (self *GroupSession) RoleAt(epoch uint64, leaf uint32) ([]byte, string, error) {
	var identityPub []byte
	var role string
	var err error
	if postErr := self.do(func() {
		if self.closing {
			err = ErrSessionClosed
			return
		}
		identityPub, role, err = self.roleAtOnLoop(epoch, leaf)
	}); postErr != nil {
		return nil, "", postErr
	}
	return identityPub, role, err
}

// roleAtOnLoop is RoleAt's body. The caller is the loop goroutine.
func (self *GroupSession) roleAtOnLoop(epoch uint64, leaf uint32) ([]byte, string, error) {
	handle, _, err := self.scheduleForOnLoop(epoch)
	if err != nil {
		return nil, "", err
	}
	table, err := self.roleTableOnLoop(epoch)
	if err != nil {
		return nil, "", err
	}
	named, stands := table[leaf]
	if !stands {
		identityPub, role, err := handle.RoleAt(leaf)
		if err != nil {
			// A LEAF NOBODY STANDS AT IS NOT CACHED, which is pastEpochOnLoop's own rule for a
			// refusal one line of reasoning up: the table is what this epoch's tree says, and a
			// refusal is what it does not say. At the SESSION'S OWN epoch the tree moves under
			// this table only at an install, which empties it; caching the absence would buy one
			// Members() call and would make the table a record of questions rather than of
			// members.
			return nil, "", err
		}
		named = epochRole{identityPub: identityPub, role: role}
		table[leaf] = named
	}
	// THE IDENTITY IS COPIED ON THE WAY OUT. The table is held across calls, so the slice inside
	// it is one every later caller would be handed too; mls clones the identity into each
	// Members() snapshot for exactly this reason -- a caller handed a window onto the live tree
	// writes through it -- and a cache in front of that call would give the clone back if this
	// line did not.
	identityPub := make([]byte, len(named.identityPub))
	copy(identityPub, named.identityPub)
	return identityPub, named.role, nil
}

// roleTableOnLoop answers one epoch's leaf -> (identity, role) table, ALLOCATED ON THE FIRST ASK
// and held until the schedule it belongs to is dropped.
//
// IT IS FILLED ONE LEAF AT A TIME AND NOT ALL AT ONCE, which is forced by the seam rather than
// chosen: the only enumeration GroupHandle offers is the MemberCount / MemberAt ordinal pair, and
// MemberAt REFUSES a member whose leaf carries no urmessage_leaf_keys -- a leaf the receiving arm's
// R6d refuses a commit for, but one that can stand in a group a hostile mls build committed into.
// A table built by walking that pair would be unable to answer what role such a member holds,
// which would hide the member rather than the defect, and it would cost one Members() snapshot per
// leaf to build besides.
//
// WHERE THE TABLE IS HELD IS WHAT MAKES IT DIE ON TIME (item 242's ruling 18). The session's own
// epoch keeps it in self.roles and a prior epoch keeps it in its own schedule, and
// installEpochOnLoop drops both in the same body -- self.roles to nil beside the fields it
// re-derives, self.pastEpochs re-made wholesale. There is no map keyed by epoch anywhere, because
// a table that outlived an epoch install would answer an epoch's question with a role read at a
// later one: the leaf index is the key, an install does not change it, and the role at it is
// exactly what an install can change.
//
// The caller is the loop goroutine.
func (self *GroupSession) roleTableOnLoop(epoch uint64) (map[uint32]epochRole, error) {
	held := &self.roles
	if epoch != self.epoch {
		past, isHeld := self.pastEpochs[epoch]
		if !isHeld {
			// unreachable through scheduleForOnLoop, which installs the schedule before it
			// answers a prior epoch's handle. It is a refusal rather than a fresh map so that a
			// future caller reaching this body by another route is told rather than cached into.
			return nil, fmt.Errorf("%w: epoch %d: its schedule is not held by this session",
				ErrPastEpochUnobtainable, epoch)
		}
		held = &past.roles
	}
	if *held == nil {
		*held = map[uint32]epochRole{}
	}
	return *held, nil
}

// pastEpochOnLoop answers the schedule of one prior epoch, building it on first use.
//
// THE ORDER OF THE REFUSALS IS THE ORDER OF THEIR COST AND OF THEIR CERTAINTY. The window is
// arithmetic over two numbers the session holds and is decided before anything is looked up; a
// loader that was never installed is the old single-epoch session and answers the old sentinel;
// and only then is the loader asked, whose answer is a store read and a whole key schedule
// rebuilt. A loader that answered a handle at any epoch but the one asked for is refused and the
// handle closed, for LoadGroup's own reason: a schedule at the wrong epoch derives a storage root
// that opens nothing and says nothing about why.
//
// The root is derived from the loaded epoch's exporter and THAT EPOCH'S pq_secret, which is
// ledger item 251's ruling 40 and is the line the ruling names. It read `self.pqSecret` -- the
// session's one scalar -- until 2026-09-23, and what that meant was that a past epoch's storage
// root was re-derived from the secret the session holds TODAY: correct only while nothing ever
// rotates, and wrong at the AEAD tag with no diagnosis the day something does. pqsecret.go is the
// table and the rule; the carrier that delivers a rotated secret to the other members is the
// device wrap and is item 243's next step, not this one.
//
// THE SECRET IS ASKED FOR BEFORE THE LOADER IS, which is this function's own cost-and-certainty
// order applied to a refusal that did not exist when the order was written: whether this session
// holds pq_secret for an epoch is arithmetic over state it already has, and asking the store to
// rebuild a whole key schedule for an epoch whose root cannot be completed is work spent to reach
// the same refusal.
//
// The caller is the loop goroutine.
//
// The noinline directive is this package's erase helper class: the root is erased in this body
// once the class keys are expanded from it, and the mls_secret before that.
//
//go:noinline
func (self *GroupSession) pastEpochOnLoop(epoch uint64) (*pastEpoch, error) {
	if self.epoch-epoch > PastEpochWindow {
		return nil, fmt.Errorf("%w: epoch %d is %d epochs behind this session's %d, and the window is %d",
			ErrEpochOutOfWindow, epoch, self.epoch-epoch, self.epoch, PastEpochWindow)
	}
	if past, isHeld := self.pastEpochs[epoch]; isHeld {
		return past, nil
	}
	// THE TABLE'S OWN ARRAY, and the two defers below are why that is said here. mlsSecret and
	// root are erased on the way out of this body because it is their only holder; pq_secret[n]
	// belongs to the session's table, and a third defer written to match them would blank it in
	// place and leave every later open at this epoch extracting a storage root over thirty two
	// zeros. pqsecret.go is where that erase lives.
	pqSecret, err := self.pqSecretForOnLoop(epoch)
	if err != nil {
		return nil, err
	}
	if self.pastEpochLoader == nil {
		return nil, fmt.Errorf("%w: epoch %d, and this session has no past epoch loader installed",
			ErrRecordNotForThisSession, epoch)
	}
	handle, err := self.pastEpochLoader(epoch)
	if err != nil {
		return nil, fmt.Errorf("%w: epoch %d: %w", ErrPastEpochUnobtainable, epoch, err)
	}
	if handle == nil {
		return nil, fmt.Errorf("%w: epoch %d: the loader answered no handle and no error", ErrPastEpochUnobtainable, epoch)
	}
	if loaded := handle.Epoch(); loaded != epoch {
		handle.Close()
		return nil, fmt.Errorf("%w: epoch %d was asked for and the loader answered a handle at epoch %d",
			ErrPastEpochUnobtainable, epoch, loaded)
	}
	if subtle.ConstantTimeCompare(handle.GroupId(), self.groupId[:]) != 1 {
		handle.Close()
		return nil, fmt.Errorf("%w: epoch %d: the loader answered a handle over group %x and this session is keyed for %x",
			ErrPastEpochUnobtainable, epoch, handle.GroupId(), self.groupId)
	}
	mlsSecret, err := handle.Export(mlsSecretLabel, nil, mlsSecretBytes)
	if err != nil {
		handle.Close()
		return nil, fmt.Errorf("%w: epoch %d: its mls_secret could not be exported: %w", ErrPastEpochUnobtainable, epoch, err)
	}
	defer zeroize(mlsSecret)
	root := StorageRoot(mlsSecret, pqSecret)
	defer zeroize(root)
	receivers, err := NewReceiverRatchets(self.retainedBound)
	if err != nil {
		handle.Close()
		return nil, err
	}
	past := &pastEpoch{
		handle:    handle,
		at:        epoch,
		classKeys: DeriveClassKeys(root),
		receivers: receivers,
	}
	self.pastEpochs[epoch] = past
	return past, nil
}

// THE DROP IS SPELLED IN installEpochOnLoop AND IN zeroizeOnLoop AND NOWHERE ELSE, and there is
// deliberately no helper for it: connect/mls reads an erase field by field off the source and
// follows no delegation, so a body that called a helper here would read there as a session whose
// close leaves every prior epoch's schedule in the heap. The two loops are the same three lines,
// and the duplication is what makes the reading true. Neither runs per open and neither runs per
// walk, for the reason the file header gives.

// classKeyOf is the class key one record seals or opens under, given the keys of the epoch it
// belongs to: the three durable classes looked up, the EPH class derived from the eph_root when
// there is one and refused when there is not.
//
// It is the body classKeyOnLoop had, lifted to take the epoch's keys as arguments so that a prior
// epoch -- which holds class keys and no eph_root -- goes through the same four arms as the current
// one and cannot come to differ from it. classKeyOnLoop is the current epoch's caller.
func classKeyOf(classKeys *ClassKeys, ephRoot []byte, class message.RetentionClass, ephBucket uint8,
	ephWindow uint64) ([]byte, error) {

	if classKeys == nil {
		return nil, ErrSessionClosed
	}
	switch class {
	case message.RetentionPermanent:
		return classKeys.Perm, nil
	case message.RetentionDurable:
		return classKeys.Durable, nil
	case message.RetentionMedia:
		return classKeys.Media, nil
	case message.RetentionEph:
		// the ONE branch that derives rather than looks up, and the one that can fail on
		// state rather than on its argument. eph_root is not in ClassKeys and never will
		// be -- master invariant I4, and spec A section 5.3 says a field for it "would make
		// the wrong thing the easy thing" -- so the absence of a fourth field is what sends
		// this branch to a value that had to arrive from outside.
		if len(ephRoot) == 0 {
			return nil, fmt.Errorf("%w: this record is EPH bucket %d window %d",
				ErrNoEphRoot, ephBucket, ephWindow)
		}
		return EphKey(ephRoot, ephBucket, ephWindow), nil
	}
	return nil, fmt.Errorf("%w: retention class %d", ErrRetentionClassUnknown, class)
}
