// GroupSession: the type the whole of spec A section 5.2 hangs off, and which no document
// declares.
//
// THE DESIGN HERE IS THIS PLAN'S AND NOT THE SPEC'S, and that is said first because everything
// below is a decision somebody may want to revisit. Grepping the whole of spec A for
// GroupSession returns three lines: section 5.2's two method signatures, section 3.6's
// concurrency row, and section 5.6's sentence that "the constructor takes the sink to make it
// explicit". There is no struct, no constructor, no statement of what it holds or how it is
// closed -- while section 5.6 silently adds a StreamIndexReserver to that constructor and
// section 5.3 adds an epoch zero storage root it must have persisted since group creation. Open
// item M1-4.
//
// WHAT THE CONSTRUCTOR IS HANDED FOR EPOCH ZERO IS THE GROUP HANDLE KEY AND NOT THE ROOT, and the
// two are one HKDF-Expand apart and both thirty two octets, so the name is the only thing telling
// them apart. Section 5.3's own sentence is about the ROOT -- it says a session past epoch zero
// must have persisted storage_root[0] -- and this file diverges from it deliberately: MASTER
// section 8 says what a member has to HOLD is group_handle_key ("a member that does not hold it
// cannot compute its own handle and therefore cannot write"), and storage_root[0] is strictly more
// than that. Every class key, the write key and the read key of epoch zero hang off the root, so a
// device that persisted it for the life of the group would be persisting epoch zero's whole key
// schedule forever -- the exact material forward secrecy is about -- to recover a routing
// identifier every member already knows. So the persisted value is the EXPANSION, the parameter is
// named for it, and both branches of installEpochOnLoop end holding the same kind of thing. Open
// item M1-4 carries the divergence.
//
// THE CONCURRENCY CONTRACT IS QUOTED, because its shape is the reason it exists. Section 3.6:
//
//	messagegroup.GroupSession -- Safe for concurrent use. Owns exactly one mls.Group and
//	serializes access through a single-goroutine command loop (run(), started by the
//	constructor, per CODESTYLE goroutine lifecycle).
//
//	The command-loop shape matters: MLS commit construction, message ingest, and epoch rotation
//	all mutate the same tree, and a lock around each public method would not prevent an
//	interleaving where two goroutines both build a commit for epoch n. One goroutine per group,
//	commands on a channel.
//
// So there is no stateLock in this file and there must not be one. A mutex around each public
// method satisfies a race detector and NOT the property: two goroutines can each take the lock,
// each read the epoch, each release it and each build a commit for that epoch. Every field below
// is reached from the loop goroutine only, and session_test.go derives the class of methods that
// touch them off the syntax tree rather than trusting this paragraph.
//
// WHAT IT HOLDS AND WHY IT HOLDS THAT, derived from what its consumers need rather than listed:
// one GroupHandle, because section 3.6's "owns exactly one mls.Group" is satisfied by holding the
// INTERFACE whose one production implementation wraps one -- naming *mls.Group in a field here
// would make Gate 5's swap a type change rather than a factory change; the EPOCH ZERO storage
// root, because group_handle_key is derived from it and must not move when the epoch does; the
// current epoch's root, its class keys and its two auth keys; one injected reserver; one sender
// ratchet per retention class and one receiver ratchet table; this device's leaf and the
// sender_handle computed from it; and an injected clock, because expire_at is a clock read and
// connect/mls, connect/message and this package have no timing sensitive test in them.
package messagegroup

import (
	"errors"
	"fmt"
	"sync"

	"github.com/urnetwork/connect/message"
)

// sessionCommand is one unit of work for the loop goroutine.
//
// The action closes over its own results and the poster reads them after done is closed, which
// is the happens-before this shape rests on: the loop writes, closes the channel, and the poster
// reads only after receiving from it.
type sessionCommand struct {
	action func()
	done   chan struct{}
}

// GroupSession is one member's view of one group at one epoch, serialized through one goroutine.
type GroupSession struct {
	// written once by the constructor and read from every goroutine.
	commands chan *sessionCommand
	// closed by run() when the loop has exited, which is what lets a poster tell "the session
	// is closed" from "the loop is busy" without a lock.
	stopped   chan struct{}
	closeOnce sync.Once
	closeErr  error

	// ------------------------------------------------------------------
	// everything below is reached from the loop goroutine ONLY
	// ------------------------------------------------------------------

	// the one MLS surface this session is allowed to see.
	handle GroupHandle
	// set by the Close command, read by the loop to decide whether to return.
	closing bool

	// group_handle_key: the epoch zero storage root's expansion, PERSISTED and never recomputed
	// from a later epoch. Section 5.3 fixes it at group creation; a session that recomputed it
	// from the current root would give every epoch a different sender_handle, so a member's
	// stream would end at every commit and the server would route the next record nowhere.
	//
	// It is what the constructor takes and what AdvanceEpoch hands back, so the value in this
	// field, the value the parameter names and the value the epoch zero branch derives are one
	// kind of thing. They were not: the parameter was named for the ROOT and used verbatim as
	// this key, so a device persisting what the doc told it to persist computed a different
	// sender_handle than its own group after every restart at epoch > 0.
	groupHandleKey []byte

	groupId       [32]byte
	epoch         uint64
	ownLeaf       uint32
	senderHandle  [16]byte
	storageRoot   []byte
	classKeys     *ClassKeys
	writeKey      []byte
	readKey       []byte
	serverNonce   []byte
	reserver      StreamIndexReserver
	nowMs         func() int64
	windowSize    int
	retainedBound int

	// pq_secret[n], BY EPOCH, and whether this session still holds the PREMISE that there is
	// only ever one of them.
	//
	// It was one scalar until ledger item 251's ruling 40, and the line that proves a scalar is
	// wrong is pastepoch.go's: a PAST epoch's storage root was re-derived from TODAY's secret,
	// which is right only while nothing ever rotates. pqsecret.go is the whole account of what
	// is held, for how long, what erases it, and why a session holding the premise behaves
	// exactly as it did before this field existed.
	//
	// pqLifetime IS A PREMISE THIS SESSION HOLDS AND NOT A FACT ABOUT THE GROUP, which is the
	// distinction its name exists to carry and the one an earlier spelling -- pqRotated -- lost.
	// A session can OBSERVE a rotation only by living through one; a group that rotated before
	// this process started leaves nothing for a fresh session to observe, so the premise survives
	// a restart that has already been refuted in the world. pqsecret.go names that residual, and
	// its two doors -- InstallPqSecret and DeclarePqSecretRotated -- are how a restorer that
	// knows better says so.
	//
	// IT IS THE ONE FIELD HERE THAT SURVIVES AN EPOCH INSTALL, and deliberately: every other key
	// in this struct is re-derived from the epoch the session moved to, and a past epoch's
	// pq_secret is derivable from nothing at all. What bounds it instead is PastEpochWindow, and
	// the entries the window leaves behind are erased as they go.
	pqSecrets  map[uint64][]byte
	pqLifetime bool

	// eph_root[n], or nil.
	//
	// IT IS NOT DERIVED AND IT CANNOT BE. Master invariant I4 and section 8.1 make it thirty
	// two octets of fresh CSPRNG drawn at the commit that opens epoch n, never a function of
	// storage_root, so unlike every other key in this struct it is not something installEpoch
	// can produce -- it is a value that has to arrive. InstallEphRoot is the door and
	// ErrNoEphRoot is what an EPH record meets when nobody has used it.
	//
	// IT IS DROPPED AND ERASED AT EVERY EPOCH INSTALL, which is not the same discipline as the
	// keys beside it. Those are re-derived from the new root, so dropping them is bookkeeping;
	// this one cannot be re-derived at all, so the drop is the whole of what stops epoch n's
	// ephemeral ladder being used to seal an epoch n+1 record -- a record no other member could
	// open, and one whose key would outlive the epoch that promised to destroy it.
	//
	// No document declares this field or the door that fills it: ledger open item 188, filed
	// 2026-09-13 and not ruled.
	ephRoot []byte

	// one sender ladder per (retention class wire byte, eph window), and one table of the
	// receivers'.
	senders   map[senderLadderKey]*SenderRatchet
	receivers *ReceiverRatchets

	// the PRIOR epochs' read schedules this session holds, by epoch, and the door they are
	// rebuilt through. Ledger item 241; pastepoch.go is the whole account of what one holds,
	// how long, and what erases it. The table is dropped and erased at every epoch install and
	// at Close, so every entry is at most PastEpochWindow behind the epoch above.
	pastEpochLoader PastEpochLoader
	pastEpochs      map[uint64]*pastEpoch

	// THIS EPOCH'S leaf -> (identity, role) table, and the session's own half of what every
	// pastEpoch holds in its roles field. Ledger item 242's R4; epochRole in pastepoch.go is the
	// whole account of why nothing in it is key material and why it must still not outlive the
	// epoch it describes. It is nil until the first ask and it is DROPPED -- not erased -- in
	// installEpochOnLoop, on the line beside the one that re-makes pastEpochs, because the leaf
	// index is its key and an install is exactly what changes the role at a leaf.
	roles map[uint32]epochRole
}

// senderLadderKey is what one of this session's own sender ladders is held under.
//
// THE WINDOW IS IN THE KEY AND IT IS NOT DECORATION. record_key[0] binds the CLASS KEY, so one
// ladder per class key is the rule, and for the EPH classes the class key is
// EphKey(eph_root, bucket, window) -- a function of the window as much as of the bucket. A map
// keyed on the wire byte alone would hand a record written in window t+1 the ladder rooted at
// window t's key: a record that encodes a window nothing can derive its key from, that this
// session would happily seal, and that no member of the group including the sender could ever
// open again. It is the same failure ReceiverRatchetKey's own comment describes for two buckets
// on one ladder, one level further in.
//
// For every class but EPH the window is zero on every record -- master section 8's presence rule
// -- so this key collapses to the wire byte for them with no special case anywhere.
//
// No document declares this key's shape either: ledger open item 188, filed 2026-09-13 and not
// ruled, names it beside InstallEphRoot as one of the four symbols the seal lift needed and the
// corpus does not have.
type senderLadderKey struct {
	RetentionWire byte
	EphWindow     uint64
}

// NewGroupSession opens a session over one group handle at the handle's current epoch.
//
// pqSecret is REQUIRED and has no default. NewPqSecret in epoch.go is what draws it -- task 13
// landed it, and this sentence used to say it did not exist -- and a caller that has one supplies
// it here; which is the shape this project's own rule asks for: "a
// missing key schedule fails closed and looks like what it is; a placeholder one fails open and
// looks like a working messenger." A constructor that defaulted it to thirty two zeros would
// produce a perfectly good storage root, both clients would agree, every test would pass, and the
// PQ half of the design would be silently gone. Its WIDTH is refused here as well as its absence,
// and separately, because until the review of task 13 this door checked only that the value was
// non empty: MASTER section 7 fixes pq_secret[n] at thirty two octets, and a four octet one is the
// ikm of a storage_root that is well formed, agreed by both clients and weaker than the document
// specifies -- which no round trip in this package could ever tell you.
//
// groupHandleKeyEpoch0 IS group_handle_key -- HKDF-Expand(storage_root[0], "gh/v1", 32) -- and it
// is PERSISTED state. A session opened at epoch 0 may leave it nil, because the current root IS
// the epoch zero root and the constructor expands it; a session opened at any later epoch must
// supply it, and is refused if it does not, because the alternative is a handle key recomputed
// from the wrong epoch. It is the KEY and not the root it came from, for the reason the file
// comment gives: the root is epoch zero's whole key schedule and this is a routing identifier.
// A value of any other width is refused with a typed error rather than left to panic out of the
// first expansion that meets it, which is what a value decoded out of durable storage deserves.
//
// The reserver is refused if nil rather than defaulted to an in-memory one. Section 5.6 says the
// constructor takes the sink to make it explicit, and a default in-memory reserver is the exact
// placeholder hazard the CP3a rule forbids: it would lose every reservation at a restart and
// re-issue every stream index under an unmoved class key.
//
// The noinline directive is this package's erase helper class, reached through the epoch install
// it ends with: that install erases the fields it is about to overwrite, and those stores outlive
// this call.
//
//go:noinline
func NewGroupSession(handle GroupHandle, pqSecret []byte, groupHandleKeyEpoch0 []byte,
	reserver StreamIndexReserver, nowMs func() int64, serverNonce []byte) (*GroupSession, error) {

	if handle == nil {
		return nil, fmt.Errorf("%w: the epoch's mls_secret is exported through it", ErrNilGroupHandle)
	}
	if reserver == nil {
		return nil, fmt.Errorf("%w: section 5.6 has the constructor take the sink to make it explicit", ErrNilStreamIndexReserver)
	}
	if nowMs == nil {
		return nil, fmt.Errorf("%w: expire_at is a clock read", ErrNilClock)
	}
	if len(serverNonce) == 0 {
		return nil, fmt.Errorf("%w: write_auth is a mac over it", ErrSessionServerNonce)
	}
	if len(pqSecret) == 0 {
		return nil, fmt.Errorf("%w: NewPqSecret draws one and there is no default", ErrNilPqSecret)
	}
	if len(pqSecret) != PqSecretBytes {
		return nil, fmt.Errorf("%w: %d octets, and it is the ikm of every storage_root this session extracts",
			ErrPqSecretLength, len(pqSecret))
	}
	groupId := handle.GroupId()
	if len(groupId) != len(([32]byte{})) {
		return nil, fmt.Errorf("%w: the group id is %d octets and a record header carries %d",
			ErrRecordNotForThisSession, len(groupId), len([32]byte{}))
	}
	self := &GroupSession{
		commands:      make(chan *sessionCommand),
		stopped:       make(chan struct{}),
		handle:        handle,
		reserver:      reserver,
		nowMs:         nowMs,
		serverNonce:   append([]byte(nil), serverNonce...),
		windowSize:    DefaultRecordWindowSize,
		retainedBound: DefaultRetainedRecordKeys,
		senders:       map[senderLadderKey]*SenderRatchet{},
		pastEpochs:    map[uint64]*pastEpoch{},
		pqSecrets:     map[uint64][]byte{},
		// THE PREMISE IS TAKEN UP HERE AND IT IS THE ONLY PLACE IT IS, which is what makes it
		// a decision rather than a default: every group alive today runs on one pq_secret for
		// its whole life, and a session that refused their past epochs would lose the history
		// of every one of them. What the premise is, what it is defined on, and the restart it
		// does NOT survive are pqsecret.go's header in full. A caller that knows its group
		// rotates says so with DeclarePqSecretRotated or by filing a past epoch's own secret.
		pqLifetime: true,
	}
	self.groupId = [32]byte(groupId)
	self.ownLeaf = handle.OwnLeafIndex()
	self.epoch = handle.Epoch()
	// the secret is recorded AT THE EPOCH THE HANDLE IS AT, which is the epoch this session is
	// about to install, and never at a fixed zero: a device that restarts opens its session at
	// whatever epoch the group has reached, and an entry filed under epoch zero would be a
	// secret nothing could look up. self.epoch is set above precisely so this line can read it.
	self.installPqSecretOnLoop(self.epoch, pqSecret)
	receivers, err := NewReceiverRatchets(self.retainedBound)
	if err != nil {
		return nil, err
	}
	self.receivers = receivers
	if err := self.installEpochOnLoop(groupHandleKeyEpoch0); err != nil {
		return nil, err
	}
	// the loop starts LAST, after every field it will read is written, so there is no window
	// in which the loop goroutine observes a half built session.
	go self.run()
	return self, nil
}

// run is the one goroutine that touches this session's state.
//
// It exits when a command sets closing, and it closes stopped on the way out so that a poster
// blocked on the send in do() is released rather than deadlocked.
func (self *GroupSession) run() {
	defer close(self.stopped)
	for {
		select {
		case command := <-self.commands:
			command.action()
			close(command.done)
			if self.closing {
				return
			}
		case <-self.stopped:
			// unreachable: only this goroutine closes stopped, and it does so on the way
			// out. It is here so that a future second closer of that channel cannot turn
			// this loop into a spin.
			return
		}
	}
}

// do runs one action on the loop goroutine and waits for it.
//
// EVERY PUBLIC METHOD THAT TOUCHES SESSION STATE GOES THROUGH THIS, and session_test.go derives
// that class off the syntax tree rather than listing it. A method that read a field directly
// would be reading state another goroutine is writing, and a mutex around it would satisfy the
// race detector while leaving section 3.6's actual property -- one decision per epoch -- unheld.
func (self *GroupSession) do(action func()) error {
	command := &sessionCommand{action: action, done: make(chan struct{})}
	select {
	case self.commands <- command:
	case <-self.stopped:
		return ErrSessionClosed
	}
	<-command.done
	return nil
}

// Close stops the loop and erases every key this session holds.
//
// It is idempotent, and the second call answers the first call's error rather than a new one: a
// deferred Close beside an explicit one is not a mistake, and a second Close that refused would
// make the ordinary defer a failure.
//
// It returns only after the loop goroutine has exited, so a goroutine accounting test sees a
// leaked loop as a failure rather than as a slow test.
func (self *GroupSession) Close() error {
	self.closeOnce.Do(func() {
		// the handle's own error is carried out on a LOCAL and written to the field here,
		// off the loop, on purpose: closeErr is the only field of this session no command
		// touches, and a field the loop writes is one nothing may read afterwards without
		// posting -- which is what the shape gate in session_test.go derives and what a
		// close that stored its answer from inside the command would quietly break.
		closed := error(nil)
		postErr := self.do(func() {
			self.closing = true
			self.zeroizeOnLoop()
			closed = self.handle.Close()
		})
		self.closeErr = closed
		if postErr != nil && !errors.Is(postErr, ErrSessionClosed) {
			self.closeErr = postErr
		}
	})
	<-self.stopped
	return self.closeErr
}

// Epoch is the epoch this session is at.
//
// It answers an error rather than a zero, because a closed session's zero is indistinguishable
// from epoch 0 -- which is the epoch every group spends its first commit in, so the ambiguity is
// over the value a caller is most likely to meet. SenderHandle one method down already answers
// this shape and for the same reason.
func (self *GroupSession) Epoch() (uint64, error) {
	var epoch uint64
	var err error
	if postErr := self.do(func() {
		if self.closing {
			err = ErrSessionClosed
			return
		}
		epoch = self.epoch
	}); postErr != nil {
		return 0, postErr
	}
	return epoch, err
}

// SenderHandle is the handle this session's own records are routed by.
func (self *GroupSession) SenderHandle() ([16]byte, error) {
	var handle [16]byte
	var err error
	if postErr := self.do(func() {
		if self.closing {
			err = ErrSessionClosed
			return
		}
		handle = self.senderHandle
	}); postErr != nil {
		return [16]byte{}, postErr
	}
	return handle, err
}

// EpochKeys is this session's write_key and read_key for the epoch it is at, copied out of the
// loop's own fields into a value the caller owns and destroys.
//
// THIS IS THE ONLY DOOR ONTO EITHER KEY, and the reason it is a door rather than two fields is in
// epochkeys.go's header: the alternative is a second assembly of one preimage, which is the defect
// this package has already paid for once. Both keys are the MESSAGE SERVER's, not a member's:
// write_auth on a submit is macced under the first and req_auth on a fetch is macced under the
// second, and connect/message's ComputeWriteAuth and ComputeRequestAuth are what spend them.
// Authenticity between MEMBERS is mls's and is not this pair's job at any point -- MASTER section
// 9.2 says so in as many words, and says that the server holds write_key itself.
//
// THE VALUE IS THE CALLER'S AND SO IS THE ERASE. Destroy it, and destroy it in a defer: this
// session's own copies are erased at the next AdvanceEpoch and at Close, and neither of those
// reaches a value this method already handed out.
//
// The copies are taken INSIDE the posted command, which is where they have to be taken: the two
// fields are written and zeroized by the loop goroutine, so a copy made off the loop is a read
// racing a write rather than a copy of anything in particular.
//
// THE self.closing CHECK BELOW IS UNREACHABLE, and it is written anyway, in the shape the six
// siblings of this file and the two of seal.go use. Measured rather than asserted: run exits the
// moment a command sets closing, and Close's command is the only one that sets it, so no second
// command can ever observe the flag -- every later caller is refused by do's own send, which sees
// stopped closed. Deleting this clause survives an unfiltered run of this package, and so does
// deleting Epoch's and SenderHandle's, which is what says the hole is the pattern's and not this
// method's. It stays because it is what fails closed if run ever stops exiting on the first
// closing command, and because a door here that alone omitted it would read as a decision.
func (self *GroupSession) EpochKeys() (*EpochKeys, error) {
	var keys *EpochKeys
	var err error
	if postErr := self.do(func() {
		if self.closing {
			err = ErrSessionClosed
			return
		}
		keys = newEpochKeys(self.epoch, self.readKey, self.writeKey)
	}); postErr != nil {
		return nil, postErr
	}
	return keys, err
}

// AdvanceEpoch installs the epoch the handle is now at, with THAT EPOCH'S pq_secret.
//
// THE PARAMETER IS pq_secret[n+1] AND NOT "the session's pq_secret", which is ledger item 251's
// ruling 40 and is what this line's own doc used to get wrong: it said "with a fresh pq_secret"
// while both production callers handed the group lifetime value in, and the session filed it over
// the one it had. It is now recorded AT THE EPOCH THE HANDLE IS AT, so a past epoch's storage
// root stays derivable from the secret that epoch actually ran on. pqsecret.go carries the table,
// the window and the group-lifetime premise -- which every group that exists today satisfies, and
// which must behave exactly as it did before this change.
//
// THIS CALL IS ALSO THE ONE PLACE A SESSION CAN OBSERVE A ROTATION, and observing is all it does:
// a secret differing from the one standing at this session's epoch drops the premise here and
// nowhere else on the ordinary path. A session that was not running when the rotation happened
// observes nothing, which pqsecret.go's header names as the residual and its two doors answer.
//
// AND IT REFUSES TO OVERWRITE A SECRET THAT IS ALREADY THERE, which is new and which is the one
// thing this method could do that nothing could diagnose. Ruling 37 has the wrap carrying
// pq_secret[n+1] opened at epoch n, BEFORE the merge, so under item 243's step 4 the table already
// holds the authority for the epoch this call is entering; this parameter is the caller's own
// account of the same value. It used to erase the entry and file the argument over the top with no
// comparison -- and both of sdk's call sites pass the group lifetime scalar -- so the wrap's
// secret was destroyed here, the session derived epoch n+1's whole schedule from the wrong half,
// every other member derived it from the right one, and the only symptom anywhere was an AEAD tag.
// That is ruling 38's both-directions blackout arriving through the seam built to prevent it.
// A differing value is now ErrPqSecretEpochConflict and NOTHING is filed or erased; the deliberate
// supersede is InstallPqSecret, which erases what it replaces and says so.
//
// GROUP_HANDLE_KEY DOES NOT MOVE. It was expanded from the epoch zero root ONCE, and what this
// session has held since construction is that answer rather than the root -- so there is nothing
// here to re-expand and the field is handed straight back to the install. The whole reason it is
// persisted is that recomputing it from the current root is a one line "simplification" that
// changes every sender_handle in the group at every commit.
//
// Every ratchet is dropped and zeroized. A ratchet held across an epoch is holding the previous
// epoch's rungs, which are exactly the octets forward secrecy is about, and the class keys it
// was built from have moved.
//
// The noinline directive is this package's erase helper class: the install below erases the entry
// it replaces, and that store is the receiver's own.
//
//go:noinline
func (self *GroupSession) AdvanceEpoch(pqSecret []byte) error {
	var err error
	if postErr := self.do(func() {
		if self.closing {
			err = ErrSessionClosed
			return
		}
		if len(pqSecret) == 0 {
			err = fmt.Errorf("%w: NewPqSecret draws one and there is no default", ErrNilPqSecret)
			return
		}
		if len(pqSecret) != PqSecretBytes {
			err = fmt.Errorf("%w: %d octets, and it is the ikm of the storage_root of the epoch this session is moving into",
				ErrPqSecretLength, len(pqSecret))
			return
		}
		// THE CONFLICT IS REFUSED BEFORE ANYTHING IS TOUCHED, so a refused advance leaves the
		// table exactly as it found it and the secret a wrap filed for this epoch survives the
		// mistake. pqsecret.go owns the rule for the same reason it owns the window bound.
		if err = self.refusePqSecretConflictOnLoop(self.handle.Epoch(), pqSecret); err != nil {
			return
		}
		// AT THE EPOCH THE HANDLE IS NOW AT, read here rather than after the install, because
		// the install is what asks the table for the secret of the epoch it is opening and a
		// value filed afterwards would be a value the install could not see.
		//
		// AN INSTALL THAT FAILS BELOW LEAVES THE ENTRY FILED, and that is the safe direction
		// rather than an oversight: the entry sits ABOVE this session's epoch, so the window
		// drop's `epoch < self.epoch` guard never reaches it, nothing derives from an epoch the
		// session did not enter, and a caller that retries the advance with the SAME secret
		// finds its own value already there and files it again over an erase. A retry with a
		// different secret meets the refusal above, which is the answer two different values for
		// one epoch should get from a path that did not ask to replace anything. The
		// alternative -- filing after the install -- is the one that cannot work, because the
		// install is the reader.
		self.installPqSecretOnLoop(self.handle.Epoch(), pqSecret)
		err = self.installEpochOnLoop(self.groupHandleKey)
	}); postErr != nil {
		return postErr
	}
	return err
}

// RebindServerNonce replaces the nonce this session macs write_auth under, which the submitting
// connection chooses afresh at every Hello.
//
// WHY THERE IS A SETTER AT ALL. serverNonce was fixed at construction and there was no way to
// move it, so the first reconnect invalidated every record this session had sealed since: spec A
// section 5.7 has the server draw a fresh thirty two octet nonce per connection and carry it in
// HelloResponse, and write_auth is a mac over it. A session that outlived one connection was
// wrong, and this is S2-2.
//
// THE BLAST RADIUS, MEASURED RATHER THAN ASSERTED, because a setter on a key schedule field
// invites the larger reading. self.serverNonce is read in this package's production source at
// exactly ONE site -- seal.go's authenticate, which hands it to message.ComputeWriteAuth -- and
// that call's answer lands in record.WriteAuth and in nothing else. It is not an input to
// AADHead, to AADBody, to either record aead derivation, to StorageRoot, to DeriveClassKeys, to
// WriteKey, to ReadKey, to SenderHandle or to StreamKey. So ONE sealed value binds it, a rebind
// must recompute that one value on every record not yet submitted -- which is ReauthRecord in
// seal.go -- and NOTHING ALREADY SEALED BECOMES UNOPENABLE: the open path never reads write_auth
// at all, which openRecordOnLoop's own body is the evidence for. connect/message's
// ComputeRequestAuth binds the nonce too, and req_auth has no caller in this package.
//
// THE SUPERSEDED VALUE IS ERASED BEFORE IT IS OVERWRITTEN AND THE NONCE IS NOT A KEY. Both
// halves of that are true and the discipline is the file's rather than the value's: spec A hands
// this nonce to the server in the clear, so nothing here is protecting it, and a field of this
// type dropped unerased is a drop site that reads exactly like the ones that are protecting
// something. AdvanceEpoch erases pq_secret in its own body for the same reason and in the same
// shape, and no property of this package observes either erase.
//
// THE REFUSAL IS THE CONSTRUCTOR'S OWN SENTINEL AND THE CONSTRUCTOR'S OWN RULE. An empty or nil
// nonce is refused with ErrSessionServerNonce, which is what NewGroupSession refuses an empty one
// with; and a nonce of any non-empty WIDTH is accepted here because the constructor accepts one.
// Two doors onto one field with two rules is two rules, and a reader meeting a thirty one octet
// nonce would have to derive which door it came through. MASTER section 7 and spec A section 5.7
// both fix the width at thirty two and this package checks neither -- the disagreement is real,
// is not this method's to rule, and is open item K1-2.
//
// THERE IS NO GETTER, and that is a narrowing rather than an omission. A caller that rebinds
// already holds the nonce: it came out of HelloResponse in the same call that prompted the
// rebind, so a getter answers a question nobody asks. What it would cost is precise --
// keysource_test.go's reproduction is handed server_nonce as one of its three INJECTED values,
// and a getter is the one thing that would let a fixture hand it the nonce the session holds
// instead, at which point the third input stops being independent and the subject starts agreeing
// with itself. noncerebind_test.go derives that class off the syntax tree and prints its
// complement.
//
// THE self.closing CHECK BELOW IS UNREACHABLE, in the shape and for the reason EpochKeys's
// comment measures: run returns on the first command that sets closing and commands is
// unbuffered, so no second command observes the flag and every later caller is refused by do's
// own send. It is written anyway because it fails closed, and no property here claims it is
// driven -- a closed session's refusal comes back out of do.
//
// The noinline directive is this package's erase helper class, reached through the zeroize
// below: that store lands in an array this call does not hold the only reference to.
//
//go:noinline
func (self *GroupSession) RebindServerNonce(serverNonce []byte) error {
	var err error
	if postErr := self.do(func() {
		if self.closing {
			err = ErrSessionClosed
			return
		}
		if len(serverNonce) == 0 {
			err = fmt.Errorf("%w: write_auth is a mac over it", ErrSessionServerNonce)
			return
		}
		// a COPY, and erased before it is overwritten, in this body, for AdvanceEpoch's
		// reason. The argument is the CALLER'S buffer: a session that retained it would seal
		// under whatever that buffer became after this call returned.
		replacement := append([]byte(nil), serverNonce...)
		zeroize(self.serverNonce)
		self.serverNonce = replacement
	}); postErr != nil {
		return postErr
	}
	return err
}

// TrackSender installs a receiver ratchet for one peer's ladder in one retention class.
//
// headIndex is the ladder position this receiver starts at and it is the CALLER'S state, never a
// number read off a record header: NewReceiverRatchet walks one expansion per index below it, so
// a peer that could choose this number could choose how much work this session does. The walk is
// bounded by maxLadderWalk in any case, which is the second half of the same argument.
//
// ephWindow IS THIS PACKAGE'S OWN PARAMETER AND NO SPECIFICATION PUBLISHES A PARAMETER LIST FOR
// THIS METHOD AT ALL. A ladder is identified by the class key it is rooted at, and for an EPH class
// that key is a function of the window, so a receiver naming a ladder has to name the window too.
// Measured against the corpus: no file under docs/specs names TrackSender, m1's plan writes it with
// its parameters elided, and the one document that spelled a list -- a PLAN, not a spec -- had four
// where this has five. That is ledger open item 188, filed 2026-09-13 and not ruled.
func (self *GroupSession) TrackSender(leaf uint32, class message.RetentionClass, ephBucket uint8,
	ephWindow uint64, headIndex uint64) error {

	var err error
	if postErr := self.do(func() {
		if self.closing {
			err = ErrSessionClosed
			return
		}
		err = self.trackSenderOnLoop(leaf, class, ephBucket, ephWindow, headIndex)
	}); postErr != nil {
		return postErr
	}
	return err
}

// trackSenderOnLoop is TrackSender's body. The caller is the loop goroutine.
func (self *GroupSession) trackSenderOnLoop(leaf uint32, class message.RetentionClass,
	ephBucket uint8, ephWindow uint64, headIndex uint64) error {

	retentionWire, err := message.RetentionClassWire(class, ephBucket)
	if err != nil {
		return err
	}
	// the ladder's window and not the argument, so a caller that passed a window with a
	// non-EPH class tracks the ratchet under the key the opener will actually form rather
	// than under one nothing ever looks up. seal.go's ephLadderWindow carries the rule.
	ladderWindow := ephLadderWindow(class, ephWindow)
	classKey, err := self.classKeyOnLoop(class, ephBucket, ladderWindow)
	if err != nil {
		return err
	}
	ratchet, err := NewReceiverRatchet(classKey, leaf, headIndex, self.windowSize)
	if err != nil {
		return err
	}
	self.receivers.Track(ReceiverRatchetKey{
		SenderHandle:  SenderHandle(self.groupHandleKey, leaf),
		RetentionWire: retentionWire,
		EphWindow:     ladderWindow,
	}, ratchet)
	return nil
}

// InstallEphRoot gives this session the eph_root of the epoch it is at.
//
// WHY THERE IS A SETTER AT ALL, AND WHY IT IS NOT A CONSTRUCTOR ARGUMENT. eph_root[n] is the one
// key in this session that no derivation of this package can produce: master invariant I4 makes
// it thirty two octets of fresh CSPRNG drawn at the commit that opens epoch n, deliberately NOT
// a function of storage_root, and MASTER section 8.1 calls a derivation from storage_root "the
// most easily broken property here" because it would compile, pass every test that does not
// look for it, and make every expired message recoverable forever. So it has to ARRIVE. A
// committer draws it with NewEphRoot; a joining or a catching up member gets it out of the
// eph_root device wrap, which is m1 task 14's and does not exist. It is a setter rather than a
// constructor parameter because a session is constructed before it has committed anything, and
// because a required argument with no carrier would have made every existing caller supply a
// value it does not have -- which is the placeholder hazard, arriving in the shape of a
// constructor.
//
// WHAT IT IS NOT: a default. A session that was never handed one refuses to seal or open any
// EPH record, of any bucket, with ErrNoEphRoot. Thirty two zero octets would derive a perfectly
// good ladder that both ends of one implementation agree on and that is identical in every group
// in the world, which is exactly the failure NewGroupSession refuses an empty pq_secret to avoid.
//
// IT IS SCOPED TO THE EPOCH THIS SESSION IS AT. installEpochOnLoop drops and erases the value on
// every epoch change, so a caller that advances an epoch and does not install that epoch's root
// meets ErrNoEphRoot rather than epoch n-1's ladder -- and the drop is what stops an EPH record
// of epoch n+1 being sealed under a key epoch n promised to destroy.
//
// The value is COPIED, because the caller drew it and may erase its own array, and the copy is
// what this session erases at the drop.
//
// NO DOCUMENT OF THE CORPUS DECLARES THIS METHOD, and that is recorded here rather than left for
// a second implementer to discover. Spec A section 5.2 publishes GroupSession's method block and
// there is no installer in it; section 5.11's device wrap is the carrier the corpus does name and
// it does not exist in any tree. The seal lift of 2026-09-13 is unimplementable without a channel,
// so this package built the smallest one and says so: it is ledger open item 188, FILED 2026-09-13
// and NOT RULED, and it names three siblings with this method -- the ephRoot field this fills,
// senderLadderKey's window, and TrackSender's ephWindow parameter, each of which says so where it
// is declared. RebindServerNonce is the precedent and the
// judgement is the same. If a ruling publishes an installer, this name moves to whatever that
// document calls it.
//
// The noinline directive is this package's erase helper class, reached through the zeroize
// below: that store lands in an array this call is not the only holder of.
//
//go:noinline
func (self *GroupSession) InstallEphRoot(ephRoot []byte) error {
	var err error
	if postErr := self.do(func() {
		if self.closing {
			err = ErrSessionClosed
			return
		}
		if len(ephRoot) != EphRootBytes {
			err = fmt.Errorf("%w: %d octets, and it is the root of every ephemeral key of this epoch",
				ErrEphRootLength, len(ephRoot))
			return
		}
		// erased before it is overwritten, in this body, for installEpochOnLoop's reason.
		replacement := append([]byte(nil), ephRoot...)
		zeroize(self.ephRoot)
		self.ephRoot = replacement
	}); postErr != nil {
		return postErr
	}
	return err
}

// installEpochOnLoop derives every key of the epoch the handle is at.
//
// groupHandleKeyEpoch0 is nil only when the handle is at epoch 0, and the refusal for every other
// epoch is what makes group_handle_key persisted state rather than a value this function could
// invent. An aged out epoch is reported and never silently zero: mls.ErrEpochErased comes back
// out of Export and travels, because a storage root computed over an empty exporter output is
// thirty two well formed octets that no other member ever reproduces.
//
// BOTH BRANCHES END HOLDING THE SAME KIND OF VALUE, which is the whole of what the switch below
// is for and is what it did not do. One arm took the argument VERBATIM and the other expanded a
// root through GroupHandleKey; both answers are thirty two octets, so nothing refused the
// disagreement, and a device restarted at epoch > 0 with the value its own doc told it to persist
// computed a sender_handle no peer computes and no peer's ReceiverRatchetKey matches. The argument
// is the KEY, so the epoch zero arm is the only one that expands anything.
//
// The caller is the loop goroutine, or the constructor before the loop exists.
//
// The noinline directive is this package's erase helper class: every field this body overwrites is
// erased in it first, and those stores are the receiver's own.
//
//go:noinline
func (self *GroupSession) installEpochOnLoop(groupHandleKeyEpoch0 []byte) error {
	mlsSecret, err := self.handle.Export(mlsSecretLabel, nil, mlsSecretBytes)
	if err != nil {
		return fmt.Errorf("messagegroup: this session could not export its epoch's mls_secret: %w", err)
	}
	defer zeroize(mlsSecret)
	self.epoch = self.handle.Epoch()
	self.ownLeaf = self.handle.OwnLeafIndex()
	// THE SECRET OF THE EPOCH BEING INSTALLED, out of the table, and not the session's latest.
	// For the current epoch the two are the same value; pastEpochOnLoop's call is the one where
	// they are not, and both go through this one door so the two derivations cannot come to
	// disagree about what pq_secret[n] means.
	//
	// IT IS THE TABLE'S ARRAY AND IT IS NOT ERASED HERE, which is the one place this body departs
	// from the discipline every other secret in it is held to. mlsSecret above is erased on the
	// way out because this call is its only holder; this one belongs to the table, and a
	// `defer zeroize(pqSecret)` written to match the line above it would blank pq_secret[n] in
	// place and leave every later derivation of that epoch's root extracting over thirty two
	// zeros. The table's own erase is at the window and at Close. That defer was written as a
	// mutant and ELEVEN cases of this package go red on it, pastepoch_test.go's six among them,
	// so this paragraph is a measurement rather than a warning.
	pqSecret, err := self.pqSecretForOnLoop(self.epoch)
	if err != nil {
		return err
	}
	root := StorageRoot(mlsSecret, pqSecret)
	handleKey := []byte(nil)
	switch {
	case 0 < len(groupHandleKeyEpoch0):
		// the width is refused HERE rather than at the first expansion that meets it. A
		// persisted value comes out of durable storage, so sixty four octets is its plausible
		// wrong shape, and SenderHandle's refusal is a panic carrying the sentinel -- which
		// would surface on the caller's goroutine out of a constructor whose every other
		// refusal is a typed error.
		if len(groupHandleKeyEpoch0) != groupHandleKeyBytes {
			return fmt.Errorf("%w: %d octets, want %d", ErrGroupHandleKeyLength,
				len(groupHandleKeyEpoch0), groupHandleKeyBytes)
		}
		// a COPY, taken before the erase below, because AdvanceEpoch passes this session's own
		// group_handle_key back in: erasing the field first would erase the argument.
		handleKey = append([]byte(nil), groupHandleKeyEpoch0...)
	case self.epoch == 0:
		// the ONE expansion, and the only branch that has a root to expand. What it produces is
		// the same kind of value the branch above is handed, which is what makes the two arms
		// agree about what this parameter is.
		handleKey = GroupHandleKey(root)
	default:
		return fmt.Errorf("%w: this handle is at epoch %d and no epoch zero group handle key was given",
			ErrEpochZeroHandleKeyMissing, self.epoch)
	}
	// EVERY FIELD IS ERASED HERE, IN THIS BODY, IMMEDIATELY BEFORE IT IS OVERWRITTEN. It is
	// spelled out rather than delegated to zeroizeOnLoop for the reason that method's own comment
	// gives: connect/mls reads a drop site as a WRITE to a field holding key material, and it
	// follows no delegation, so an epoch rotation that called a helper would read there as a
	// complete second epoch dropped into the heap for the collector to move around.
	for _, ratchet := range self.senders {
		ratchet.Zeroize()
	}
	if self.receivers != nil {
		self.receivers.Zeroize()
	}
	self.classKeys.Zeroize()
	zeroize(self.storageRoot)
	zeroize(self.writeKey)
	zeroize(self.readKey)
	zeroize(self.groupHandleKey)
	// eph_root is ERASED AND DROPPED and is the one field here that is not replaced. Every
	// other key of the epoch is re-derived two lines below from the new root; this one cannot
	// be derived at all, so there is nothing to put back and a caller that wants epoch n+1's
	// ephemeral ladder installs that epoch's root through InstallEphRoot. Carrying the old
	// value across would seal an epoch n+1 record under epoch n's key -- unopenable by every
	// other member, and alive past the epoch that promised to destroy it.
	zeroize(self.ephRoot)
	self.ephRoot = nil
	// AND EVERY PRIOR EPOCH'S SCHEDULE GOES WITH THEM, erased entry by entry. They were built
	// against the epoch this session is leaving -- the window they sit inside is measured from
	// it -- and a schedule a later record needs is rebuilt on demand out of the store, which is
	// what pastepoch.go's header prices. What this costs is one load per prior epoch per epoch
	// change; what it buys is that nothing here has to decide which of them is now past the
	// window, because none of them survives to be past it. The loop is spelled here and not
	// delegated, for the reason the paragraph above gives about every other field.
	for _, past := range self.pastEpochs {
		past.Zeroize()
	}
	self.pastEpochs = map[uint64]*pastEpoch{}
	// AND THE pq_secret TABLE IS THE ONE THING HERE THAT DOES NOT GO, which is stated on the line
	// that would otherwise be its drop site rather than left to a reader to notice the absence.
	// Everything above is re-derived from the epoch this session moved to; pq_secret[n] is
	// derivable from nothing, so erasing it here would destroy the only copy of the value that
	// makes epoch n's storage root computable and re-introduce ledger item 251 ruling 40's defect
	// from the other end. What bounds it instead is the WINDOW, and the entries the window has
	// moved past are erased and dropped one line down -- after self.epoch has moved, because the
	// window is measured from it. Spelled here and not delegated for the reason above; the loop
	// it calls is pqsecret.go's because the bound and its guard are that file's subject.
	self.dropPqSecretsBelowWindowOnLoop()
	// AND THIS EPOCH'S ROLE TABLE GOES WITH THEM, item 242's ruling 18. It is dropped and not
	// erased -- a credential identity is published in its own leaf node and a role is a row of a
	// group context extension the transcript covers, so there is no secret in it -- but it is
	// dropped HERE, because it is keyed by LEAF INDEX and a commit that changes a role changes
	// nothing else about the leaf. A table that survived this line would answer epoch n+1's
	// question with the role that leaf held at epoch n, which is the one reading item 242's ruling
	// 21 exists to make impossible. The prior epochs' tables die three lines up, as fields of the
	// schedules re-made there.
	self.roles = nil
	self.groupHandleKey = handleKey
	self.storageRoot = root
	self.classKeys = DeriveClassKeys(root)
	self.writeKey = message.WriteKey(root)
	self.readKey = message.ReadKey(root)
	self.senderHandle = SenderHandle(self.groupHandleKey, self.ownLeaf)
	self.senders = map[senderLadderKey]*SenderRatchet{}
	return nil
}

// classKeyOnLoop is the class key one record seals under.
//
// FOUR CLASSES AND FOUR ANSWERS SINCE 2026-09-13. Three are looked up in ClassKeys, which
// expands them from the storage root; the fourth is DERIVED here, because the eph classes are
// deliberately absent from that struct -- MASTER invariant I4, and spec A section 5.3 says a
// field for eph_root there "would make the wrong thing the easy thing". What this used to do
// for an EPH class was refuse, alongside PERMANENT and MEDIA, under the blanket refusal that
// ledger item 152 held -- and 152 was RULED 2026-09-13, so the refusal is lifted in full and
// what is left in its place is a refusal about the VALUE: a session with no eph_root cannot
// derive one.
//
// IT TAKES THE BUCKET AND THE WINDOW BECAUSE THE EPH CLASS KEY IS A FUNCTION OF BOTH.
// K_eph[n][b][t] keys on b and on t, so "the class key of this class" is not a well formed
// question for EPH without them, and the window is the RECORD'S OWN eph_window field rather
// than anything this method reads.
//
// The caller is the loop goroutine.
//
// THE FOUR ARMS LIVE IN classKeyOf SINCE LEDGER ITEM 241, and this body is the current epoch's
// call into them: a prior epoch's schedule holds class keys and no eph_root, and one switch
// over the four classes that both callers reach cannot come to answer a class differently for
// the two. What stays here is the reading of the session's own two fields.
func (self *GroupSession) classKeyOnLoop(class message.RetentionClass, ephBucket uint8,
	ephWindow uint64) ([]byte, error) {

	return classKeyOf(self.classKeys, self.ephRoot, class, ephBucket, ephWindow)
}

// senderRatchetOnLoop is this session's own ladder for one retention class, built on first use.
//
// THE LADDER IS PER CLASS KEY AND THE COUNTER IS NOT, which is ruling A1 as it lands on this
// file. The map below is keyed by the retention wire byte AND THE EPH WINDOW because
// record_key[0] binds the CLASS KEY, so each class key is a different ladder and always was --
// and since 2026-09-13 an EPH record's class key is EphKey(eph_root, bucket, window), which
// moves at every window boundary. senderLadderKey's own comment carries what keying on the byte
// alone would have cost. The stream those ladders reserve in carries
// only the group and this session's sender_handle -- no class -- because that is the counter spec
// B's schema, spec B's Q7 and the shipped message server all keep. A stream key that carried the
// class would make this client the only party in the system counting per class, and the server
// would refuse the first record of the second class as a stream index regression.
//
// What used to make that safe here was the retention byte in the key; what makes it safe now is
// that Reserve ALLOCATES, so no two of these ladders can be handed the same index. See
// StreamKey's comment for the ruling and SenderRatchet.Next for the shape.
//
// The caller is the loop goroutine.
func (self *GroupSession) senderRatchetOnLoop(class message.RetentionClass, retentionWire byte,
	ephBucket uint8, ephWindow uint64) (*SenderRatchet, error) {

	ladder := senderLadderKey{RetentionWire: retentionWire, EphWindow: ephLadderWindow(class, ephWindow)}
	if ratchet, isBuilt := self.senders[ladder]; isBuilt {
		return ratchet, nil
	}
	classKey, err := self.classKeyOnLoop(class, ephBucket, ephWindow)
	if err != nil {
		return nil, err
	}
	ratchet, err := NewSenderRatchet(classKey, self.ownLeaf, StreamKey{
		GroupId:      self.groupId,
		SenderHandle: self.senderHandle,
	}, self.reserver)
	if err != nil {
		return nil, err
	}
	self.senders[ladder] = ratchet
	return ratchet, nil
}

// zeroizeOnLoop erases everything this session holds, and it is THE erase of this type.
//
// Every field is named here rather than delegated to the partial erase above, and that is a gate
// rather than a preference: connect/mls reads an erase FIELD BY FIELD off the source and follows
// no delegation, so a body that called zeroizeEpochOnLoop and added the two survivors would read
// there as an erase of two fields. The duplication is what makes the reading true.
//
// The caller is the loop goroutine.
//
//go:noinline
func (self *GroupSession) zeroizeOnLoop() {
	for _, ratchet := range self.senders {
		ratchet.Zeroize()
	}
	if self.receivers != nil {
		self.receivers.Zeroize()
	}
	self.classKeys.Zeroize()
	zeroize(self.storageRoot)
	zeroize(self.writeKey)
	zeroize(self.readKey)
	zeroize(self.groupHandleKey)
	zeroize(self.ephRoot)
	for _, past := range self.pastEpochs {
		past.Zeroize()
	}
	// EVERY EPOCH'S pq_secret, and not only the current one. This is the line the scalar made
	// trivial and the table does not: a closed session that erased one entry of a table of
	// thirty three would leave thirty two post quantum halves of retired storage roots in the
	// heap, which is the drop-without-erase this whole change is about, one level up. The loop is
	// spelled HERE rather than delegated, for the reason this method's own header gives.
	for epoch, secret := range self.pqSecrets {
		zeroize(secret)
		delete(self.pqSecrets, epoch)
	}
	self.classKeys = nil
	self.storageRoot = nil
	self.writeKey = nil
	self.readKey = nil
	self.groupHandleKey = nil
	self.ephRoot = nil
	self.senders = map[senderLadderKey]*SenderRatchet{}
	self.pastEpochs = map[uint64]*pastEpoch{}
	// and the table itself goes, emptied above. A closed session holds no epoch, so it holds no
	// secret for one and pqSecretForOnLoop refuses -- which is the same answer a closed session
	// gives to every other ask.
	self.pqSecrets = map[uint64][]byte{}
	// dropped and not erased, for installEpochOnLoop's reason at the same field: nothing in it is
	// a secret, and what it must not do is outlive the epoch it describes. A closed session has
	// no epoch, so it holds no table.
	self.roles = nil
}

// The exporter label and length MASTER section 7 derives mls_secret at.
//
// They are constants of this file rather than arguments because there is exactly one mls_secret
// per epoch and a second label would be a second storage root: every key of the epoch hangs off
// it, so two callers exporting under two labels would be two members of one group who agree
// about nothing.
const (
	mlsSecretLabel = "URmessage/v1/storage"
	mlsSecretBytes = 32
)

// env_key[k]'s exporter label, MASTER section 8.2, declared HERE beside mls_secret's because
// these two are the whole of what this package exports out of the MLS key schedule.
//
// THE PAIR IS WHY THEY ARE TOGETHER. Two exporter labels at one epoch are two independent secrets
// and the LABEL is all that separates them -- the context is empty and the length is the same
// thirty two -- so a label that was a prefix or a respelling of the other would make
// storage_root's mls_secret and the device wrap's outer root one value, and every device wrap
// would be sealed under a key derived from the material it exists to deliver. Neither is a prefix
// of the other and they disagree at their thirteenth octet, which is inside both.
// m1w1repairs_test.go pins mls_secret's against MASTER's literal; wrap_test.go pins this one and
// the relation between them.
//
// THE WIDTH IS NOT DECLARED HERE. env_key[k] is the head of the device wrap's record ladder, so
// its width is a class key's, and wrap.go declares it as EnvKeyBytes beside the ladder that takes
// it rather than as a second thirty two in this block.
const envKeyLabel = "URmessage/v1/envelope"
