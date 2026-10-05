// The client half's sentinels: every refusal a caller of this package can catch by name, in
// one place, so a reader asking what can go wrong reads a file rather than a call graph.
//
// It is a second errors.go rather than a widening of connect/message's, and the split is the
// reason. Spec A section 12.1 publishes a block of names to the message server and
// connect/message's errors.go is written as an argument about which of its sentinels are on
// that block; none of the ones here can be. The server never derives a storage root, never
// holds a class key and never opens a record -- section 12.1 gives it no decryption function
// at all -- so a sentinel it cannot reach would widen its allow list with a name no server
// can match. xwing_errors.go already applies that rule one file over, for the four errors of
// a KEM the server never runs, and this file is the same rule for the rest of the client
// half.
//
// Each one is fatal by construction. Nothing in this package reports a cryptographic failure
// and carries on: a record that does not open is not a warning, it is a message a member
// cannot read, and a layer that answered plaintext beside the error would hand its caller
// octets no key authenticated.
package messagegroup

import "errors"

var (
	// Fires when a record aead key is not the thirty two octets section 5.3's expansion
	// produces. It is checked before the primitive is constructed rather than left to
	// chacha20poly1305.NewX's own length check, so the refusal names the record layer's
	// contract -- key_head and key_body are the first thirty two octets of a fifty six octet
	// expansion -- rather than the library's.
	ErrRecordAeadKeyLength = errors.New("messagegroup: a record aead key is not the thirty two octets the record key expansion produces")
	// Fires when a record aead nonce is not the twenty four octets XChaCha20-Poly1305 takes.
	// This is the width that tells the extended construction from the twelve octet one, and
	// it is the reason this refusal exists at all: a twelve octet nonce is the tail of the
	// expansion silently discarded, and every record so sealed round trips against itself and
	// against nothing else.
	ErrRecordAeadNonceLength = errors.New("messagegroup: a record aead nonce is not the twenty four octets XChaCha20-Poly1305 takes")
	// Fires when a record ciphertext does not authenticate under the key, the nonce and the
	// aad it was opened with. It carries no plaintext with it and never a partial one: the
	// only thing an unauthenticated ciphertext yields is the refusal.
	ErrRecordAeadOpen = errors.New("messagegroup: a record ciphertext did not authenticate")
	// Fires when a group handle key is not the thirty two octets HKDF-Expand(storage_root[0],
	// "gh/v1", 32) produces. A handle derived from a truncated key is a well formed handle,
	// and every member of the group would compute a different one, so the width is refused
	// where it can still be told apart from a value.
	ErrGroupHandleKeyLength = errors.New("messagegroup: a group handle key is not the thirty two octets the epoch zero expansion produces")
	// Fires when a storage root is not the thirty two octets HKDF-Extract produces. It is
	// the same argument ErrGroupHandleKeyLength makes one derivation later: every key of an
	// epoch hangs off this value, a truncated or an over long one expands to well formed
	// keys, and no other member of the group computes them. Without it the refusal came
	// from mls's own expand -- which names mls's contract, and only for a root SHORTER than
	// the hash, so a root of sixty four octets decoded out of durable storage was accepted
	// silently.
	ErrStorageRootLength = errors.New("messagegroup: a storage root is not the thirty two octets HKDF-Extract produces")
	// Fires when a retention class key is not the thirty two octets DeriveClassKeys
	// produces. record_key[0] binds the class key, so a truncated one is a whole ladder no
	// peer reproduces.
	ErrClassKeyLength = errors.New("messagegroup: a class key is not the thirty two octets the class expansion produces")
	// Fires when a record key is not the thirty two octets the ladder produces. Every aead
	// key and every nonce of a record is expanded from it, so a wrong width here is a
	// record nothing opens -- and, on the ratchet's own path, a chain that silently forks.
	ErrRecordKeyLength = errors.New("messagegroup: a record key is not the thirty two octets the record key ladder produces")
	// Fires when a record aead is asked to seal against no additional authenticated data.
	// MASTER invariant I7 makes ct_head aad_head's and ct_body aad_body's, and the header of
	// sealRecordAead used to state that as if something enforced it. Nothing did: a nil aad
	// sealed and returned a ciphertext whose epoch, stream index, sender and retention class
	// were authenticated by nothing at all.
	ErrRecordAeadAadMissing = errors.New("messagegroup: a record aead was asked to seal against an empty aad")
	// Fires when a stream index reservation is asked to go backwards -- a persisted high
	// water behind an index already handed out, or an allocation that came back below the
	// ladder standing on it. Spec A section 5.6 makes the counter write once, so a rewind is a
	// store that lost a flush, and every index above it is a nonce this device may already
	// have used.
	ErrStreamIndexRewound = errors.New("messagegroup: a stream index reservation is behind an index already reserved")
	// Fires when the store cannot allocate the next index of a stream at all: its next
	// position is one it has already handed out and it has no way past it. Under ruling A1 the
	// counter is the store's, so this is the store reporting a state it cannot leave rather
	// than a caller being told no -- and the underlying hazard is the one section 5.6 spells
	// out, that a reused stream index is a reused nonce under a reused record key, which is a
	// total break of both of a record's aeads.
	ErrStreamIndexConsumed = errors.New("messagegroup: a stream index has already been consumed")
	// Fires when a stream index reserver is nil where one is required. The reservation is
	// ordered BEFORE the key, so a ratchet without a sink is a ratchet that cannot make the
	// ordering it exists to make -- and section 5.6 says the constructor takes the sink to
	// make that explicit.
	ErrNilStreamIndexReserver = errors.New("messagegroup: a stream index reserver is required and none was given")
	// Fires when a sender ratchet has produced the last stream index a u64 can hold. A wrap
	// to zero is not a wasted message: it re-issues every record key and every nonce this
	// sender has ever used, under a class key that has not moved.
	ErrSenderRatchetExhausted = errors.New("messagegroup: a sender ratchet has consumed the last stream index")
	// Fires when a receiver is asked for a record key outside its skipped key window --
	// above it, or below a head that has already passed. Spec A section 5.5 makes this
	// visible rather than silent: the caller turns it into a gap entry, never into a
	// dropped message. Open item M1-15 decides how it crosses OpenRecord.
	ErrOutOfWindow = errors.New("messagegroup: a record key is outside this receiver's skipped key window")
	// Fires when a receiver window size or a retained key bound is not positive. A window of
	// zero would refuse every out of order record and a negative one is not a bound at all,
	// so the constructor states it rather than clamping it.
	ErrWindowSize = errors.New("messagegroup: a receiver window size or retained key bound is not positive")
	// Fires when a record key is asked of a sender this table tracks no ratchet for. It is a
	// refusal and not an empty answer because the caller's next move differs: a ratchet that
	// was never installed is a member this session does not know about, and a key of zero
	// octets would open a record with a key every party in the world can compute.
	ErrNoReceiverRatchet = errors.New("messagegroup: no receiver ratchet is tracked for this sender and retention class")
)

// ---------------------------------------------------------------------------
// the group engine and its connect/mls adapter, spec A section 6
// ---------------------------------------------------------------------------

var (
	// Fires when an engine is constructed with no crypto provider. Every secret the engine
	// derives is drawn through it, so there is nothing the constructor could have judged
	// without one -- and a nil provider reached at the first group is a dereference in a
	// caller's founding path rather than a refusal it can report.
	ErrEngineCryptoProvider = errors.New("messagegroup: a group engine requires a crypto provider and none was given")
	// Fires when an engine is constructed with no state store. A group with nowhere to persist
	// an epoch is a group that cannot be reopened, and the first thing that would notice is a
	// restart.
	ErrEngineStateStore = errors.New("messagegroup: a group engine requires a state store and none was given")
	// Fires when an engine is constructed with no signature private key. The leaf of every
	// group this engine founds is signed with it, so an empty signer is a group whose own
	// founding leaf verifies against nothing.
	ErrEngineSigner = errors.New("messagegroup: a group engine requires a signature private key and none was given")
	// Fires when the encoded urmessage_leaf_keys body a device publishes cannot be read. It is
	// refused at construction rather than at the first group because a device that cannot say
	// what its wrap target key is has nothing to fix later: every group it founds would carry
	// a leaf no epoch fan out can address.
	ErrEngineLeafKeys = errors.New("messagegroup: an urmessage_leaf_keys body is not one this engine can read")
	// Fires when the octets handed to JoinFromWelcome are not an MLSMessage carrying a Welcome,
	// or when what the store held under a ref that message names does not decode as a key
	// package. It names a RUNTIME condition and not a gap in another package's exported
	// surface, which is the whole difference from the sentinel it replaces: a join is now
	// possible, and what is left to refuse is a message.
	ErrEngineWelcomeShape = errors.New("messagegroup: these octets are not an MLSMessage carrying a welcome")
	// Fires when LoadGroup is answered an epoch state that stands at a DIFFERENT epoch from the
	// one it was asked for. mls.LoadGroup makes no such comparison -- it reads the blob the store
	// answers at the key it was handed and rebuilds whatever is in it -- so without this refusal a
	// store that answered the wrong row produces a group that is internally consistent in every
	// way: real tree, real schedule, real exporter, at an epoch nobody asked for. Every key the
	// record layer derives over it is then a well formed key at the wrong epoch, the records seal,
	// and every peer refuses them for a reason this device cannot name.
	ErrEngineLoadedEpoch = errors.New("messagegroup: the epoch state this store answered stands at a different epoch from the one that was asked for")
	// Fires when CommitAdd is handed no key packages at all. A commit carrying no proposal and
	// no path is ValSem201's refusal one layer down, and a commit carrying no proposal WITH a
	// path is a legitimate MLS commit that adds nobody -- so an empty vector is refused here,
	// by name, rather than becoming whichever of the two mls answers.
	ErrEngineCommitAddEmpty = errors.New("messagegroup: CommitAdd was handed no key packages")
	// Fires when one of the key packages handed to CommitAdd does not decode, or decodes to a
	// leaf that carries no urmessage_leaf_keys extension. The message names WHICH one. It
	// carries the index because a caller batching several is otherwise told only that one of
	// them is wrong.
	ErrEngineCommitAddKeyPackage = errors.New("messagegroup: a key package handed to CommitAdd is not one this profile can admit")
	// Fires when CommitContextExtensions is handed an empty list. RFC 9420 section 12.1.6
	// replaces the group's extension list WHOLESALE, so an empty list is a group with no policy
	// and no required capabilities -- the one list no caller of this profile can have meant --
	// and it is refused by name, before anything is staged, rather than becoming whichever
	// refusal mls or a receiver answers. AMENDED 2026-09-25: CommitRemoveWithExtensions carries a
	// wholesale list too and refuses an empty one with THIS sentinel rather than a second of its
	// own, because it is the same refusal about the same argument -- a caller matching on it is
	// asking "did I hand a commit an empty extension list", and that question does not become a
	// different one because the commit also carried a Remove.
	ErrEngineCommitContextExtensionsEmpty = errors.New("messagegroup: CommitContextExtensions was handed no extensions")
	// Fires when CommitRemove is handed no leaves, for ErrEngineCommitAddEmpty's reason at the
	// other arm: a commit carrying no proposal and a path is a legitimate MLS commit that
	// removes nobody, and that is never what a caller of this method meant. AMENDED 2026-09-25:
	// CommitRemoveWithExtensions answers it too, for the sibling reason above -- a combining
	// commit with no leaf in it is a policy commit, which CommitContextExtensions already is.
	ErrEngineCommitRemoveEmpty = errors.New("messagegroup: CommitRemove was handed no leaves")
	// Fires when Process has verified a commit's signature against a leaf and the pre-commit
	// tree holds no member there. It is not a state connect/mls can produce today -- a commit
	// is signed by a member's leaf and this profile refuses external commits -- and the refusal
	// is here because the alternative is an EngineProcessed whose CommitterIdentity is nil,
	// which an authorizer keyed on identity would read as an unnamed MEMBER.
	ErrEngineCommitterUnknown = errors.New("messagegroup: the pre-commit tree holds no member at the leaf a commit's signature verified against")
	// Fires when no key package ref the Welcome names is one this device's store holds. It
	// carries the ref count, the refusal count and the last store error VERBATIM, because
	// StateStore.TakeKeyPackage answers a bare error with no declared not-found value: a broken
	// disk and a Welcome addressed to somebody else are indistinguishable to a caller matching
	// on the type, and the only place the difference can survive is the message. The taxonomy
	// that would let the type tell them apart is owed by whoever owns the store interface.
	ErrEngineNoKeyPackageForWelcome = errors.New("messagegroup: no key package this store holds is addressed by this welcome")
	// Fires when MemberAt is asked for an ordinal the membership does not have. It is a
	// refusal and not a zero member because the two are told apart by nothing downstream: a
	// projection that dropped mls.MemberAt's second result would turn a missing member into
	// leaf 0, addressed, wrapped to and counted.
	ErrEngineMemberOrdinal = errors.New("messagegroup: no member of this group stands at that ordinal")
	// Fires when a member's leaf carries no readable urmessage_leaf_keys extension. The epoch
	// fan out wraps to that key, so a nil answer here is a member silently left out of an
	// epoch every other member can open.
	ErrEngineMemberLeafKeys = errors.New("messagegroup: a member's leaf carries no urmessage_leaf_keys extension this engine can read")
	// Fires when RoleAt is asked about a leaf no member of the group stands at, at the epoch the
	// handle is at. It is ErrEngineMemberOrdinal's sibling at the other key and it is a refusal
	// for the same reason: a zero answer here is a nil identity beside an empty role, which the
	// reader of a role -- item 242's R4, deciding whether to render a record collapsed -- cannot
	// tell from an unnamed member, and an unnamed member is a MEMBER that may send.
	//
	// IT IS A SEPARATE SENTINEL FROM THE EPOCH REFUSALS BESIDE IT, and that separation is load
	// bearing: "this device holds no schedule for that epoch" is ErrEpochOutOfWindow or
	// ErrPastEpochUnobtainable and is the same answer an OPEN at that epoch gives, while this one
	// says the epoch was reached and nobody was there.
	ErrEngineMemberLeaf = errors.New("messagegroup: no member of this group stands at that leaf")
	// Fires when ApplyCommit is handed an EngineProcessed this handle did not stage -- one
	// built by a keyed composite literal outside this package, which section 6 says is legal
	// go, or one staged by another handle of this package. It is a typed refusal and never a
	// panic and never a silent no-op, so the guarantee is "the commit THIS handle staged"
	// rather than "some commit some engine staged".
	ErrEngineProcessedForeign = errors.New("messagegroup: this handle did not stage that processed message")
	// Fires when ApplyCommit is handed an EngineProcessed this handle has ALREADY installed. The
	// install releases the value's staged half -- that is what makes a later DiscardProcessed of
	// the same value a no-op rather than the erase of a live epoch -- so a second ApplyCommit
	// finds nothing to install and says so by name, rather than handing mls a value with no
	// commit in it and answering whatever that door says about the shape. DiscardProcessed of
	// the same value answers nil: one value, one install, and the cleanup after it costs nothing.
	ErrEngineProcessedApplied = errors.New("messagegroup: that processed message was already applied by this handle")
	// Fires when connect/mls answers a processed message whose discriminant and whose arms
	// disagree, or an opened application message with no content. Neither is a state mls can
	// produce today; the refusal is here because the alternative to refusing it is a zero
	// plaintext from leaf 0, which reads as an empty message rather than as a fault.
	ErrEngineProcessedArm = errors.New("messagegroup: a processed message's kind and its content disagree")
)

// ---------------------------------------------------------------------------
// the two ratchets, spec A section 5.5
// ---------------------------------------------------------------------------

var (
	// Fires when a ratchet that has been zeroized is asked for a key. Without it Zeroize left
	// both ratchets fully operational and the next call handed out the ladder rung derived
	// from thirty two zeros -- the SAME key, and so the same (key, nonce) pair, for every
	// zeroized ratchet in the world, with the stream index durably consumed under it.
	ErrRatchetZeroized = errors.New("messagegroup: this ratchet has been zeroized and can produce no further keys")
	// Fires when a sender ratchet can never serve another allocation its store makes. Three
	// ways in and every one is permanent for that ratchet: the store refused to allocate at
	// all, the store handed back an index at or below the one the ladder stands on, or it
	// handed back one so far ahead that the catch-up walk exceeds maxLadderWalk -- which under
	// ruling A1's shared counter is what a class that went quiet for a whole sender's stream
	// meets, with no corrupt store in it. It is separated from a transient failure because the
	// two need opposite answers: a full disk is a retry and the ladder does not move, while
	// none of these three becomes true later, so a ratchet that went on asking would refuse
	// every send forever while paying a durable write per attempt. The error wraps the
	// underlying sentinel -- ErrStreamIndexConsumed, ErrStreamIndexRewound or
	// ErrLadderWalkTooLong -- so a caller can tell the three apart with errors.Is.
	ErrSenderRatchetWedged = errors.New("messagegroup: this sender ratchet can no longer serve the stream indices its store allocates")
	// Fires when a ladder resume would cost more expansions than this package will pay. Both
	// constructors walk one HKDF-Expand per index below their starting point, and neither the
	// stream index in a record's cleartext header nor a high water read back out of a store is
	// authenticated by anything at the moment it is read -- so an unbounded walk is a denial
	// with no ceiling. See maxLadderWalk for what the bound is and why it is a cost ceiling
	// rather than a class.
	ErrLadderWalkTooLong = errors.New("messagegroup: a ladder resume would cost more expansions than this package will pay")
)

// ---------------------------------------------------------------------------
// the session, the sealer and its reader, spec A sections 5.2 and 5.5
// ---------------------------------------------------------------------------

var (
	// Fires when a session is constructed with no group handle. The handle is what the epoch's
	// mls_secret is exported through, so a session without one holds no key material at all.
	ErrNilGroupHandle = errors.New("messagegroup: a group session requires a group handle and none was given")
	// Fires when a session is constructed with no clock. expire_at is a clock read and the
	// house rule forbids a timing sensitive test, so the clock is injected -- and a nil one
	// defaulting to time.Now would put a real clock in a package that has none.
	ErrNilClock = errors.New("messagegroup: a group session requires an injected clock and none was given")
	// Fires when a session is constructed with no server nonce, or when the nonce is replaced
	// with an empty one. write_auth is a mac over the submitting connection's nonce and
	// connect/message refuses an empty one; refusing it here names the session's own missing
	// state rather than the preimage builder's.
	ErrSessionServerNonce = errors.New("messagegroup: a group session requires the submitting connection's server nonce")
	// Fires when a closed session is asked to seal or open. Close zeroizes every key the
	// session holds, so the alternative to this refusal is a record sealed under thirty two
	// zeros.
	ErrSessionClosed = errors.New("messagegroup: this group session is closed")
	// Fires when a session is constructed or advanced with no pq_secret. NewPqSecret in
	// epoch.go draws one and there is no default, which is the point: HKDF-Extract(mls_secret, 32 zero bytes)
	// produces a perfectly good storage root, both clients agree, every test passes, and the
	// post quantum half of the design is silently gone. A missing key schedule fails closed
	// and looks like what it is; a placeholder one fails open and looks like a working
	// messenger.
	ErrNilPqSecret = errors.New("messagegroup: a group session requires a pq_secret and there is no default")

	// A pq_secret of the wrong WIDTH, which is a different mistake from having none and is
	// answered separately so a caller can tell "I passed nothing" from "I passed the wrong
	// thing". It is here because the session was the one door this value reaches the seal path
	// through that checked only that it was non empty: MASTER section 7 fixes pq_secret[n] at
	// thirty two octets, a four octet one extracts to a well formed storage_root that both
	// clients agree on, and every test in this package stayed green over it.
	ErrPqSecretLength = errors.New("messagegroup: a pq_secret is not the thirty two octets MASTER section 7 fixes")

	// Fires when a derivation asks for pq_secret at an epoch this session holds no secret for
	// AND the single-secret compatibility path cannot answer. Ledger item 251's ruling 40;
	// pqsecret.go carries the rule.
	//
	// WHAT REACHES IT, STATED AS THE THREE WAYS AND NOT AS AN IMPOSSIBILITY. A session holding
	// the group-lifetime premise answers every epoch out of the one secret it has, so this
	// sentinel needs the premise to be GONE, and exactly three things take it: a second,
	// different secret arriving at an AdvanceEpoch; a past epoch's own secret arriving at
	// InstallPqSecret and differing from today's; and DeclarePqSecretRotated, which is how a
	// restorer states what a fresh session cannot observe. An earlier version of this comment
	// said the sentinel was unreachable while a session had not rotated, and left out that a
	// RESTART of a rotated group is a session that has not observed a rotation and never will --
	// so the sentinel was not merely hard to reach there, it was the answer that shape needed
	// and did not get. pqsecret.go's header carries that residual by name.
	//
	// It is its own sentinel and not ErrEpochOutOfWindow because the two are different facts
	// with different repairs. Out of window means no device holds a schedule for that epoch and
	// no re-fetch will change it; this one means this session was never handed the octets that
	// epoch's storage root was extracted from, and the repair is to supply them -- which is what
	// the device wrap of item 243's next step delivers. A session that answered instead with the
	// secret it HAPPENS to hold would derive a well formed storage root that no member of the
	// group reproduces, and the failure would surface at an AEAD tag with nothing naming the
	// cause. That is the defect ruling 40 names, and this refusal is the shape that replaces it.
	ErrPqSecretUnknownEpoch = errors.New("messagegroup: this session holds no pq_secret for the epoch a derivation asked for")

	// Fires when a value arrives for an epoch this session ALREADY holds a DIFFERENT secret
	// for, on the path that did not ask to replace anything: AdvanceEpoch's own parameter.
	//
	// WHY THERE IS A REFUSAL HERE AT ALL, and it is ruling 37 that makes it necessary. The
	// wraps that carry pq_secret[n+1] are submitted and opened at epoch n, BEFORE the merge, so
	// under item 243's step 4 the ordinary sequence is InstallPqSecret(n+1, the wrap's secret)
	// and THEN AdvanceEpoch, with the table already holding the authority for the epoch the
	// session is entering. AdvanceEpoch's parameter is the caller's own account of that same
	// value. When the two disagree one of them is wrong, and the one that was ALREADY FILED is
	// the one a wrap put there.
	//
	// WHAT IT REPLACES IS A SILENCE, which is why it is a typed error and not a refutation.
	// Before it, the install erased the standing entry and filed the argument over the top
	// without ever comparing them -- the refutation it did perform was against the entry at the
	// epoch being LEFT, never against the entry it was about to destroy -- so a pq_secret[n+1]
	// delivered by a wrap was silently replaced by the next advance, and BOTH production
	// callers in sdk pass the group lifetime scalar today. The session then derived epoch
	// n+1's whole schedule from the wrong half, every other member derived it from the right
	// one, and the only symptom anywhere was an AEAD tag. That is ruling 38's
	// both-directions blackout produced by the seam built to prevent it, and ruling 38's own
	// requirement is that it be a TYPED refusal separable from the shapes beside it.
	//
	// THE DELIBERATE REPLACE IS A DIFFERENT DOOR AND IS NOT THIS. InstallPqSecret may replace
	// an entry -- a lost CAS race means the winning commit's secret supersedes the one this
	// device optimistically filed -- and it erases what it replaces and drops the group
	// lifetime premise on the difference. So the repair for this refusal is to file the value
	// through the door whose subject is filing, and then advance with the same octets.
	ErrPqSecretEpochConflict = errors.New("messagegroup: a different pq_secret is already filed for the epoch this session is entering")

	// Fires when InstallPqSecret is asked to file a secret at the epoch the session is STANDING
	// at, which that door's own doc excludes and which it used to accept.
	//
	// THE TABLE AND THE LIVE SCHEDULE MAY NOT DISAGREE ABOUT self.epoch. Everything this
	// session seals and opens at its own epoch hangs off self.storageRoot, which
	// installEpochOnLoop extracted ONCE from pq_secret[self.epoch] as the table held it then.
	// Filing a different value at that epoch moves the table and does not move the schedule, so
	// the session is left holding two answers to one question: it keeps sealing under the old
	// root while the table says that epoch ran on the new secret, and one advance later
	// pastEpochOnLoop rebuilds that epoch from the TABLE and every record of it stops opening.
	// That is ruling 40's own defect -- an epoch's root built from a secret that epoch did not
	// run on -- re-entering through the door added to close it.
	//
	// THE ASYMMETRY THAT MAKES IT A DEFECT RATHER THAN A CHOICE, said out loud because it was
	// neither stated nor asserted before: AdvanceEpoch files AND THEN RE-DERIVES, through
	// installEpochOnLoop, so its table and its schedule move together. InstallPqSecret files
	// and does not re-derive, by design -- it is the door for epochs this session is NOT
	// standing at, where there is no live schedule to move.
	//
	// WHY IT IS NOT REPAIRED BY RE-DERIVING INSTEAD. A re-derivation out of this door would
	// drop and erase every ratchet, every prior epoch's schedule and the role table as a side
	// effect of filing one secret, which is a blast radius no caller of a method named "install
	// a secret" would expect, and it would give this package two epoch installs to keep in
	// agreement. The repair is to build the session with the secret its own epoch ran on, and
	// the refusal says so. Ruling 37 needs epoch+1 and never epoch.
	ErrPqSecretEpochIsCurrent = errors.New("messagegroup: this session is standing at that epoch and its key schedule is already derived from the secret it holds")

	// Fires when a session is opened at an epoch after zero with no epoch zero group handle
	// key. group_handle_key is fixed at group creation and is PERSISTED state; a constructor
	// that recomputed it from the current epoch would give every epoch a different
	// sender_handle, so a member's stream would end at every commit.
	ErrEpochZeroHandleKeyMissing = errors.New("messagegroup: a session past epoch zero requires the group handle key it was founded with")
	// Fires when a record is sealed with an expire_at that has already passed. It is advisory
	// and may only shorten retention, so a value in the past is a record the server is entitled
	// to prune before anyone reads it -- which is a caller's mistake and not a policy.
	ErrRecordExpired = errors.New("messagegroup: a record's expire_at has already passed")
	// Fires when a value reaches the sealer or the opener that is not a retention class at
	// all -- a RetentionClass is a uint8 and the four the design has are 0 through 3, so
	// every other value of the type arrives here.
	//
	// IT IS NO LONGER THE BLANKET CLASS REFUSAL AND ITS NAME CHANGED WITH ITS MEANING. It was
	// ErrRetentionClassUnruled, and its message said that one class alone could be sealed,
	// pending m1 open item M1-6's ruling -- ruled 2026-09-07, reversed 2026-09-13 -- on which
	// record key seals ct_head. That message is
	// DESCRIBED and not quoted, for doc.go's reason: this package holds an inventory gate over
	// its own production prose, and a retracted sentence reproduced verbatim is a sentence the
	// gate keeps finding. M1-6 was ruled on 2026-09-07 and the message was stale from that day;
	// the ruling was then REVERSED on 2026-09-13 (ledger items 152 and 128, spec A revision
	// A-25) and the refusal it named was lifted in full -- ct_head takes the record's OWN class
	// key, exactly as ct_body does, so head and body take one ladder at one position and there
	// is nothing left for a class to be unruled about. A sentinel still named Unruled for a
	// ruled item is the pre-amendment comment trap this corpus keeps filing, so the name moved
	// with the text rather than only the text.
	ErrRetentionClassUnknown = errors.New("messagegroup: this value names no retention class this package can key a record under")
	// Fires when a record reaches the sealer or the opener that needs K_eph and this session
	// holds no eph_root.
	//
	// eph_root[n] is thirty two octets of fresh CSPRNG drawn at the commit that opens epoch n
	// (master invariant I4): it is not derivable from storage_root, from the exporter or from
	// anything else this session holds, so a session that was not handed one cannot seal or
	// open an EPH record of any bucket, bucket 0 included. It is a REFUSAL and never a default,
	// for the reason NewGroupSession refuses an empty pq_secret rather than defaulting it: a
	// root of thirty two zero octets would derive a perfectly good ladder that both ends of one
	// implementation agree on and that is the same for every group in the world.
	ErrNoEphRoot = errors.New("messagegroup: this session holds no eph_root, so it can neither seal nor open an ephemeral record")
	// Fires when an eph_root that is not thirty two octets reaches a derivation or the
	// installer. Master section 8.1 fixes the width; a short one expands to a well formed key
	// that no peer reproduces.
	ErrEphRootLength = errors.New("messagegroup: an eph_root is not the thirty two octets MASTER section 8.1 fixes")
	// Fires when a bucket that names no rung of the eph ladder reaches EphKey or the window
	// arithmetic. The ladder has six rungs, 0 through 5, and RetentionClassOf refuses every
	// wire byte outside 0x10..0x15, so this is unreachable through a parsed record and is a
	// programmer error marker -- which is exactly what the 2026-09-13 sentinel ruling separated
	// it from bucket 0 in order to be able to say.
	ErrEphBucketOffLadder = errors.New("messagegroup: this bucket names no rung of the eph ladder")
	// Fires when the window arithmetic is handed a clock reading before the unix epoch. Master
	// section 8.1 makes t "a count of whole buckets since that origin", and a negative reading
	// is a clock wrong by decades rather than a window.
	ErrEphWindowSentAt = errors.New("messagegroup: a sent_at before the unix epoch has no eph window")
	// Fires when an opener meets an EPH(1..5) record whose eph_window is more than ONE window
	// AHEAD of the window the opener's own clock falls in.
	//
	// THE REFUSAL IS ASYMMETRIC AND THE ASYMMETRY IS THE WHOLE CLIENT SIDE DEFENCE. Spec A
	// section 5.3: an opener can derive ANY window's key from eph_root[n], because HKDF-Expand
	// takes whatever t it is handed, so a client that honoured a far future window would keep
	// the record openable long past its timer -- for every record a hostile sender or a hostile
	// server put in front of it, with master section 12.4's required user facing string false
	// and nothing anywhere reporting it. A window BEHIND the opener's own is NOT a refusal in
	// any amount: the opener derives the key for the wire window and either still holds it or
	// has destroyed it on schedule, and a destroyed one is a gap with reason expired.
	//
	// IT IS ITS OWN SENTINEL AND NOT AN AEAD FAILURE, which section 5.3 requires in as many
	// words -- separable by errors.Is from every AEAD failure. A caller renders it as a gap
	// with reason malformed (section 7.4), and it cannot do that if the only thing it can tell
	// is that a tag did not verify.
	ErrEphWindowAhead = errors.New("messagegroup: this record's eph_window is more than one window ahead of this opener's clock")
	// Fires when the eph_root device wrap reaches the sealer, and it REPLACES the blanket class
	// refusal for exactly one record rather than surviving it.
	//
	// LEDGER OPEN ITEM 185, filed 2026-09-13 (second pass of that date) and NOT RULED. Spec A
	// section 5.11 states it as an instruction rather than as a note: "AND THE eph_root WRAP'S
	// OWN eph_window VALUE IS NOT RULED. A builder MUST NOT PUBLISH THAT RECORD UNTIL IT IS."
	// Three landed sentences cannot all be satisfied by it. Master section 8's presence rule
	// makes eph_window non zero on EPH(1..5) and that record is EPH(5). Spec A requirement S19,
	// spec B section 5.1 check 3 and spec B section 7.1 all refuse an EPH(1..5) record whose
	// window differs from its arrival window by more than one, with NO carve out for a wrap
	// anywhere. And the value's own formula divides sent_at, while section 5.11 part 5 states
	// that a wrap head's plaintext is not stated at all -- a wrap carries no MLS frame, so it
	// has no sent_at to divide. A builder writing 0 is refused by the server and the epoch fan
	// out stops with no device ever obtaining eph_root[k]; a builder computing from its own
	// publication clock is equally conforming on the text as it stands. So this is a refusal
	// and not a guess, and what is owed is one sentence from the owner: what eph_window an
	// EPH(1..5) record that is NOT keyed under K_eph carries, and whether S19 applies to it.
	//
	// The pq_secret device wrap is unaffected and seals normally: it is PERMANENT and carries
	// the presence rule's zero.
	ErrEphWrapWindowUnruled = errors.New("messagegroup: the eph_root device wrap's own eph_window is ledger open item 185 and is not ruled, so this record is not published")
	// Fires when a stage of the seal chain is reached with the value the previous stage owed it
	// missing. Section 5.2's title is "Construction order is a type, not a convention" and the
	// staging types are unexported, so the order IS a type to every other package; inside this
	// one a keyed composite literal can assemble a later stage over an earlier stage's zero
	// value, and a head sealed that way produced a record with body_hash all zero that
	// message.EncodeRecord accepted. Each stage carries what the next needs and the next checks
	// it, so a skipped stage is this refusal rather than a wire visible record no reader opens.
	ErrRecordStageOrder = errors.New("messagegroup: a record was assembled out of the order MASTER section 8 fixes")
	// Fires when a body plaintext does not fit any rung of the size ladder. The ladder tops out
	// at the 64 KiB rung and the blob rung carries no body at all, so a longer body is a blob
	// and a blob is task 20's.
	ErrBodyTooLong = errors.New("messagegroup: a record body is longer than the largest rung of the size ladder")
	// Fires when a padded body does not unpad. The length prefix is inside the aead, so
	// reaching this means the plaintext authenticated and is still not a padded body -- which
	// is a sealer and a reader that disagree rather than an attacker.
	ErrBodyPadding = errors.New("messagegroup: a record body did not unpad")
	// ------------------------------------------------------------------
	// MASTER section 8.4, RULED 2026-09-15: the inner MLS frame of an application record
	// ------------------------------------------------------------------

	// Fires when an application record's ct_body plaintext is not an MLS PrivateMessage this
	// group can open. MASTER section 8.4.1 makes ct_body's plaintext LP(inner) | 0*, where
	// inner is one marshalled MLSMessage produced by GroupHandle.Protect, so everything mls
	// refuses about that frame -- octets that are not an MLSMessage, a wire format that is not
	// a PrivateMessage, a signature that does not verify under the signing leaf's credential,
	// an epoch this group is not at, a ratchet generation already consumed -- arrives here
	// wrapped.
	//
	// IT IS WRAPPED AND NOT REPLACED. The mls error is the %w of the second verb, so
	// errors.Is reaches both this sentinel and whatever mls said, and a caller that wants to
	// tell "not a frame at all" from "a frame somebody else signed" still can.
	//
	// THE SEAL PATH REACHES IT TOO. Protect is what the sealer calls, and a group that cannot
	// protect -- a closed handle, an exhausted ratchet -- is a record that was never built.
	ErrRecordInnerFrame = errors.New("messagegroup: an application record's inner MLS frame did not open")
	// MASTER section 8.4.3's R1, the SENDER binding, and it is the refusal that turns "someone
	// in this group wrote this" into "Alice wrote this".
	//
	// The record layer cannot make that statement on its own: every octet it seals under is
	// group-shared by construction -- the class keys expand from a storage root every member
	// derives, record_key[0] takes a leaf index as an INPUT, and sender_handle is computable by
	// every member for every leaf. The inner frame is signed under the sender's own credential,
	// which is the one secret in the system that is not group-shared, so this is where the two
	// answers to "who wrote this" are required to agree.
	ErrRecordSenderBinding = errors.New("messagegroup: the leaf that signed this record's inner frame is not the leaf its sender_handle names")
	// MASTER section 8.4.3's R2, the POSITION binding, and it is what makes aad_mls
	// load-bearing rather than decorative.
	//
	// MLS verifies that the sender signed WHATEVER authenticated_data the frame carries; only
	// this layer knows which aad THIS record's position produces. Without the comparison a
	// member who cannot forge Alice's signature can still re-envelope a frame Alice signed into
	// another stream_index -- a replay into a later conversational position -- or into another
	// retention class, so a DURABLE message self-destructs within the hour or an EPH one never
	// does.
	ErrRecordPositionBinding = errors.New("messagegroup: this record's inner frame was signed for a different record's position")
	// Fires when a record reaches the wrong one of the two open doors: a ceremony record at
	// OpenRecord, or an application record at OpenCeremonyRecord.
	//
	// IT IS WHAT MAKES MASTER SECTION 8.4.3 MANDATORY RATHER THAN OPT-OUT. The predicate that
	// decides whether a record carries an inner frame reads is_commit and the server attachment,
	// and both of those live in AAD_head, which is sealed under a record key EVERY MEMBER
	// DERIVES. So whoever seals a record chooses which arm of MASTER section 8.4.1's table it
	// takes, and before this sentinel existed a member who did not want to be signature-checked
	// simply set is_commit -- and OpenRecord answered that member's chosen octets under the
	// victim's sender_handle with no signature anywhere on the path. A rule an attacker can opt
	// out of is not a rule.
	//
	// What closes it is that the arm now selects a DOOR rather than a policy. OpenRecord serves
	// only the arm that carries a frame, so every body it returns has been signature-checked at
	// R1 and R2; the other arm is refused here, by name, and a caller that genuinely wants those
	// octets asks OpenCeremonyRecord for them and is told in that method's own name and prose
	// that no member signed them. The attacker's choice is therefore between being checked and
	// being refused, which is what a rule is.
	//
	// WHAT IT DOES NOT DO, because the complement is the part a reader has to be told: it does
	// not authenticate the ceremony arm. It cannot -- a wrap, an epoch fan out and a completion
	// marker carry no signature at all by Spec A section 5.11 (5), and a commit record's
	// authentication is the commit's own, which belongs to the epoch machinery and not to this
	// door. Open item MG-5.
	ErrRecordNotAnApplicationRecord = errors.New("messagegroup: this record's arm of MASTER section 8.4.1's table is not the one this door opens")
	// Fires when a record on the blob rung reaches the sealer or the reader. The blob object,
	// its identifier and its padder are task 20's and none of them exists yet; a record whose
	// body lives somewhere this package cannot address is refused rather than opened empty.
	ErrBlobRecordUnsupported = errors.New("messagegroup: a record on the blob rung has no body this package can reach yet")
	// Fires when a record names a group, an epoch or a sender this session is not keyed for.
	// The header is cleartext and authenticated by nothing at the moment it is read, so each
	// of the three is checked against the session's own state before any key is derived --
	// and a record whose sender_handle is not the one the ratchet is keyed by would otherwise
	// be opened under a ladder belonging to somebody else.
	ErrRecordNotForThisSession = errors.New("messagegroup: this record names a group, an epoch or a sender this session is not keyed for")
	// Fires when a member or a device finds no device wrap for its target at an epoch after
	// the marker has landed. Spec A section 5.11 step 5 makes it a VISIBLE failure -- a gap
	// entry with reason no_wrap -- and never a silent skip. It is declared here beside
	// ErrOutOfWindow because open item M1-15 has not decided how either crosses OpenRecord,
	// and the two sentinels are what sdk matches on until it does. Wave 2's fan out is what
	// returns it; nothing in wave 1 does.
	ErrNoWrap = errors.New("messagegroup: no device wrap for this target at this epoch")
	// Fires when a record names an epoch further behind this session's than PastEpochWindow,
	// which is the line connect/mls's MergePendingCommit deletes state below: no device holds a
	// schedule for it and no re-fetch will change that. It is its own sentinel and not
	// ErrRecordNotForThisSession because sdk renders the two differently -- one is a visible gap
	// the walk moves past, the other a record a later fetch may still open. Ledger item 241.
	ErrEpochOutOfWindow = errors.New("messagegroup: this record's epoch is behind the past epoch window")
	// Fires when a prior epoch inside the window could not be rebuilt: the loader answered an
	// error, no handle, a handle at another epoch or over another group, or a handle whose
	// exporter refused. The loader's own error is wrapped beneath it, so a caller that knows its
	// store can tell "this device held no state at that epoch" -- a member admitted later, for
	// whom item 241 says the epoch is not theirs -- from a store that would not read.
	ErrPastEpochUnobtainable = errors.New("messagegroup: this record's epoch is inside the window and its schedule could not be obtained")
	// Fires when InstallPastEpochLoader is handed nil. A nil is refused rather than read as an
	// uninstall because a session that silently went back to single-epoch would produce gaps a
	// caller has no way to trace to the line that caused them.
	ErrNilPastEpochLoader = errors.New("messagegroup: a past epoch loader is required and none was given")
)

// ---------------------------------------------------------------------------
// the provisional epoch state, spec A section 5.12 step 1 and guardrail G10
// ---------------------------------------------------------------------------

var (
	// Fires when any accessor of a provisional epoch state is reached after its destructor has
	// run. G10's whole sentence is "the provisional epoch state is a value that
	// ClearPendingCommit destroys; there is no path that reads it afterwards", and a destroyed
	// value that answered zeros rather than refusing would satisfy the letter of it while a
	// caller sealed a record under thirty two zero octets -- which is a working record that no
	// other member can open and which no round trip test can see.
	ErrProvisionalEpochDestroyed = errors.New("messagegroup: this provisional epoch state has been destroyed and answers nothing")
	// Fires when a provisional epoch state is built over a value that is not the thirty two
	// octets its derivation produces. The four values of section 5.12 step 1 arrive from four
	// different places -- an extraction, two expansions and a CSPRNG draw -- and a short one
	// produces a MAC or a wrap that verifies against itself and against nothing else, so the
	// width is settled once here rather than at whichever expansion happens to meet it first.
	ErrProvisionalEpochValue = errors.New("messagegroup: a provisional epoch state was given a value that is not thirty two octets")
	// Fires when the X-Wing wraps of a provisional epoch are installed twice, or installed
	// empty. The set is write once because a second install over a live one drops the first
	// fan out's wraps with nothing erasing them, and an epoch's wraps are exactly the material
	// section 5.12 step 1 orders discarded together.
	ErrProvisionalEpochWraps = errors.New("messagegroup: the X-Wing wraps of a provisional epoch are installed once")
)

// ---------------------------------------------------------------------------
// the epoch keys door, spec A section 5.2 seen from the message server's side
// ---------------------------------------------------------------------------

var (
	// Fires when any accessor of an EpochKeys is reached after Destroy, and when one is reached
	// on a value newEpochKeys never made.
	//
	// ONE SENTINEL FOR TWO CONDITIONS, which is ErrProvisionalEpochDestroyed's reasoning applied
	// to the sibling type: a caller's question is the same in both cases -- "is there anything
	// here" -- and it matches it with one errors.Is. The alternative to this refusal is a caller
	// that macs write_auth under a nil key, which the server answers with an auth failure that
	// looks exactly like a rotated epoch.
	ErrEpochKeysDestroyed = errors.New("messagegroup: this epoch keys value has been destroyed and answers nothing")
)

// ---------------------------------------------------------------------------
// the device wrap's door, MASTER section 7 and section 8.2
// ---------------------------------------------------------------------------

var (
	// Fires when a wrap body's eleven octet envelope is not eleven octets, or does not decode.
	// The width is refused rather than a prefix read, because a truncated body read as an
	// envelope answers a content epoch made of whatever followed it -- and the content epoch is
	// one of wrap_key's nine inputs, so the wrong one is a key the sealer never derived and a tag
	// failure that says nothing about what went wrong.
	ErrWrapEnvelope = errors.New("messagegroup: this wrap body's envelope is not the eleven octets master section 7 fixes")
	// Fires when the octets past the envelope are not u16(alg_id) | LP(ct_xwing) | LP(aead_ct),
	// and when anything follows them. hybrid_ct is self-delimiting, so a trailing octet is an
	// octet no field of the grammar names -- and a wrap body's tail sits inside a record AEAD
	// whose key descends from env_key[k], which every member of the epoch holds.
	ErrWrapBody = errors.New("messagegroup: this wrap body is not wrap_envelope | hybrid_ct")
	// Fires when a wrap body names a KEM that is not X-Wing. It is refused rather than carried
	// out to a caller because alg_id is one of wrap_key's nine inputs: a body naming another
	// suite is a body whose key this build would derive under the wrong two octets and then
	// blame on the tag.
	ErrWrapAlgId = errors.New("messagegroup: this wrap body names a kem that is not x-wing")
	// Fires when a wrap's aead_ct does not authenticate.
	//
	// IT IS THE ONLY VERDICT OVER THE KEM AND THAT IS THE WHOLE SHAPE OF THE KEM. ML-KEM-768
	// rejects implicitly: a ciphertext that was not produced for this key decapsulates
	// SUCCESSFULLY to a pseudorandom secret, so every wrap addressed to another leaf reaches the
	// AEAD with thirty two well formed octets and nothing before this point can refuse it. A
	// caller that treated a decapsulation's nil error as "this wrap is mine" would be right about
	// every wrap in the epoch.
	//
	// IT IS NOT A VERDICT ABOUT WHICH WRAP THIS IS. The key is derived from the envelope the body
	// carries, so it convicts an envelope that was EDITED after the seal and never a genuine wrap
	// of another epoch or another payload kind, whose envelope and whose key agree with each
	// other. That second question is ErrWrapEnvelopeMismatch's, and it is answered first.
	ErrWrapOpen = errors.New("messagegroup: this wrap did not open under this leaf's key")
	// Fires when the envelope a wrap body CARRIES is not the wrap its opener asked for.
	//
	// IT IS A DIFFERENT VERDICT FROM ErrWrapOpen AND THE DIFFERENCE IS THE POINT. Measured, on
	// the door as it stood before the expectation argument: a genuine wrap sealed at content
	// epoch 10 opened at a door whose signature had no epoch in it and returned its payload byte
	// for byte, and the two bodies SealDeviceWraps answers for one leaf at one epoch -- which
	// land at ONE wrap_target_handle -- both opened under identical arguments. ErrWrapOpen means
	// the octets were tampered with after the seal; this one means they were not, and the wrap is
	// somebody else's business.
	//
	// The two are separable by errors.Is on purpose, and that separates nothing an attacker
	// chose: the eleven octets of the envelope travel in the clear outside hybrid_ct, so anyone
	// holding the record already knows which of the two a given opener will answer.
	ErrWrapEnvelopeMismatch = errors.New("messagegroup: this wrap's envelope is not the wrap its opener asked for")
	// Fires when a wrap is sealed to no target key, or opened with no private half.
	ErrWrapTargetKey = errors.New("messagegroup: a wrap needs the target leaf's x-wing key and none was given")
	// Fires when a wrap is sealed over an empty payload. A wrap with nothing inside it is a
	// record that occupies a rung, closes a fan-out's count and delivers no secret, which is the
	// omission m1 open item M1-22 is about arriving from the publisher's own side.
	ErrWrapPayload = errors.New("messagegroup: a wrap carries a payload and this one is empty")
	// Fires when the two device wraps of one target take one payload_type.
	//
	// Both records land at the SAME wrap_target_handle by MASTER section 8.3's unchanged
	// derivation, and u8(payload_type) is the only element of wrap_key's nine that separates
	// them, so one octet used twice is a pair of records separable by nothing any key binds --
	// which is precisely the job MASTER section 7's own table gives that octet. What turns that
	// separation into a refusal at the OPENER is the payload_type OpenWrapBody's caller states:
	// two distinct octets give two distinct keys, and an opener honouring one kind refuses the
	// other's body before it reaches either.
	ErrWrapPayloadTypeCollision = errors.New("messagegroup: the two device wraps of one target take two payload types")
)
