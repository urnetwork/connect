// The two ratchets of spec A section 5.5: the sender's forward ladder over record_key[i], and
// the receiver's bounded window of the rungs it has skipped.
//
// Section 5.5 is short, and every sentence of it is load bearing:
//
//	a real forward ratchet: the sender overwrites record_key[i] after use. receivers keep a
//	bounded window of skipped keys for out-of-order receipt.
//	Next() advances and zeroes.
//	Window size: 1024 keys per (sender_handle, retention class) [...] capped at 64 senders
//	tracked per group before the oldest is evicted.
//	Beyond the window, a record is undecryptable and surfaces as a gap entry with
//	GapReason == "out_of_window" -- NOT as an error.
//
// THE LADDER POSITION IS THE STREAM INDEX, and that pin is the most consequential decision in
// this file. It is now RULED rather than proposed: ledger item 143 filed it as "the device wrap
// still owes a normative stream_index-to-ratchet-position mapping", and the owner's ruling of
// 2026-09-07 settles items 143 and 169 together as shape A1 -- one class blind stream_index per
// (group_id, sender_handle), which is the counter spec B's schema, spec B's Q7 and the shipped
// message server already keep. streamindex.go carries the ruling and its evidence.
//
// The reasoning, because a later reader will be tempted to undo it for the cost it carries. The
// record nonce is expanded from the record key, so key and nonce uniqueness IS uniqueness of the
// ladder position. If the position were the ratchet's own in-memory counter it would restart at
// zero on every process start, while the stream index -- which is durable -- carried on; every
// rung of the ladder would then be issued twice under one class key, and a repeated (key, nonce)
// pair under XChaCha20-Poly1305 hands out the Poly1305 one time key. connect/mls shipped that
// exact defect once, in a restored member restarting its sender ratchet at generation 0, guarded
// only by a 32 bit reuse_guard. The only state this layer can recover after a restart is the
// reserver's high water, so the position has to BE that number.
//
// Under A1 that pin is held by CONSTRUCTION rather than by discipline. The index comes back from
// the store's allocation, and Next walks the ladder to it before handing anything out, so the
// rung a record is sealed under is record_key[its own stream_index] whatever this process
// remembers. What used to be a rule the sender had to keep is now the only thing the code can
// express.
//
// The price, stated because it is real and because no document carries it. Standing at index n
// costs n HKDF-Expand calls, and a class key is per EPOCH, so the walk is paid again at every
// commit and grows for the life of the group -- and A1 multiplies it, because one shared counter
// makes every one of a sender's k ladders walk the WHOLE sender's stream rather than its own
// class's share of it. Measured on this tree, an epoch change rebuild costs (k+1) x P expansions
// where it cost P; the benchmarks in ratchetrepairs_test.go carry the numbers. Section 5.6's
// interface has no field a client could persist a ladder position in, which is what would make
// the walk unnecessary -- the same interface open item M1-5 is already about. It is filed rather
// than worked around: a bound invented here would be policy this file has no standing to make,
// and a lazy walk moves the cost to the first send rather than removing it.
//
// WHAT THIS PACKAGE'S WINDOW DOES NOT HAVE, and connect/mls's does. mls's peekFor lets a
// too-far-ahead generation MOVE the head, and ledger 2026-09-04 argues that is safe there
// because the generation only reaches peekFor after an AEAD open under sender_data_secret --
// the number has been authenticated before the window sees it. THIS LAYER HAS NO SUCH GATE:
// stream_index arrives in the record's CLEARTEXT header, so anything that can write to the
// network chooses it. So the head does not move on a refusal here, the forward walk is bounded
// by the window size on every call, and the retained keys are bounded across the WHOLE table
// rather than per ratchet. Copy the window and the eviction from mls; do not copy the catch-up.
//
// The bounds are constructor parameters and not constants. Section 14 open item 7 -- open item
// M1-12 here -- has to finalise the memory budget and it BLOCKS the A6 freeze, so a window baked
// in as a const would make that finalisation a signature change. The defaults are connect/mls's
// own two numbers, read off that package rather than restated, because M1-12's labelled
// recommendation is to adopt its shape: a TREE WIDE retained bound, so adding senders adds no
// memory at all, and eviction from the FULLEST window, so a member holding a handful of skipped
// keys never pays for a member holding more than its own share. THE PROPERTY IS THE FAIR SHARE
// AND NOT "a handful never pays for a thousand", which is the looser sentence this file carried
// and which is FALSE: evicting from the fullest equalises, so once the bound is exceeded every
// holder above bound/senders loses rungs. Measured at the shipped defaults, where the table
// bound and one window are the same number: three honest senders, two holding four hundred
// skipped rungs each and a third taking a full window reorder, left the two at 341 -- fifty
// nine and fifty eight honest rungs permanently gone, with no attacker in it. What IS true, and
// what pruneRetainedLocked's own comment states and a three sender case holds, is that a member
// holding fewer than bound/senders never loses a rung at all. Section 5.5's "evict the oldest
// sender" starves whoever went quiet, which is the member most likely to need the window.
package messagegroup

import (
	"errors"
	"fmt"
	"sync"

	"github.com/urnetwork/connect/mls"
)

// The default bounds on a receiver's retained keys, taken from connect/mls rather than written
// down a second time.
//
// DefaultRecordWindowSize bounds ONE ratchet and DefaultRetainedRecordKeys bounds the whole
// table. The second is the first and not a multiple of it, which is the property that makes the
// bound independent of how many senders a group has: mls's own comment gives the argument, and
// the eviction below always lands on the fullest window so the cost falls on whoever created it.
//
// Both are read off mls's exported constants so the two packages cannot drift, and both are
// DEFAULTS rather than the values: the constructors take the numbers, because open item M1-12
// has not finalised them and a constant would make the finalisation a signature change.
const (
	DefaultRecordWindowSize   = mls.RatchetWindowSize
	DefaultRetainedRecordKeys = mls.MaxRetainedWindowKeys
)

// The most expansions either constructor will pay to walk a ladder to its starting point.
//
// It is a COST CEILING and not a class, which is why it is a number here rather than something
// derived off the tree: what it bounds is how much CPU one call may spend, and the only honest
// derivation of that is a measurement. Measured on this machine at roughly four hundred
// nanoseconds per rung, so this bound is about four tenths of a second in the worst case, and
// the next power of two up is nearly a second.
//
// It exists because BOTH walks are driven by a number nothing has authenticated at the moment
// it is read. A receiver's head index is a position in a peer's stream and the only value
// available for it is the record header's cleartext stream_index; a sender's resume is whatever
// a store hands back, and a store that has been rolled back or corrupted hands back anything. An
// unbounded walk over either is a denial with no ceiling: 2^32 is about half an hour of one core
// and 2^63 never returns.
//
// What the bound COSTS is stated rather than hidden: a stream that has genuinely passed this
// many records cannot be resumed at all, and the refusal is ErrLadderWalkTooLong rather than a
// silent restart at zero -- which would re-issue every record key under an unmoved class key.
// Open item M1-12 is where a real ceiling on a stream's length belongs; until it exists this is
// the ceiling on the WALK, which is the part that is this package's to bound.
const maxLadderWalk = 1 << 20

// SenderRatchet is one sender's ladder over record_key[i] for one retention class, with the
// durable index allocation ordered in front of every key it hands out.
//
// stateLock guards recordKey, position and exhausted. reserver and groupId are written once by
// the constructor and read without it, which is what lets Next hold the lock over the whole
// allocate-then-advance sequence: two concurrent calls that interleaved between the allocation
// and the walk would advance one ladder past the rung the other is about to hand out.
//
// THE POSITION IS NO LONGER THIS RATCHET'S TO CHOOSE, which is ruling A1's whole effect on this
// type. Under wave 1 the field below was "the stream index the next call will reserve": the
// ratchet picked the number and the store asserted it. A1 puts every retention class of one
// sender on ONE class blind counter, and two ladders that each pick their own number out of one
// counter is the permanent wedge ledger item 168 measured. So the counter is the store's, Next
// ASKS for the next index rather than announcing it, and the field below is what it is: the rung
// this ladder is standing on, which the next allocation walks forward from. A ladder is therefore
// SPARSE -- it pays one expansion for each index its siblings took -- and that is the price
// StreamKey's comment records.
//
// THE SIGNATURE IS THE THREE VALUED FORM AND THAT IS PROVISIONAL. Section 5.5 declares
// Next() (index uint64, recordKey []byte) with no error; section 5.6 requires Reserve to
// complete DURABLY before the key is produced and says SealRecord "refuses to proceed on error".
// A no-error Next cannot report a failed fsync, so either the reservation happens outside the
// ratchet -- and the ordering guarantee is back to being a convention, which section 5.2 says
// this layer must not do -- or Next panics on a disk error. Neither is written down; that is
// open item M1-13. The three valued form is taken because it is the one that can be narrowed
// later without losing information.
type SenderRatchet struct {
	stateLock sync.Mutex
	reserver  StreamIndexReserver
	stream    StreamKey
	// record_key[position], this ratchet's own copy, erased by the Next that passes it on.
	recordKey []byte
	// the ladder position this ratchet stands on: the LOWEST index it can still produce a
	// rung for. It is not the index the next call will take -- the store chooses that, and it
	// is at or above this one -- and every index in between is a rung this ladder walks past
	// and erases, which is what makes a shared counter safe rather than merely tolerable.
	position uint64
	// set when position reached the last index a u64 holds, so it cannot wrap to zero.
	exhausted bool
	// set by Zeroize. Without it Zeroize left this ratchet fully operational and the next
	// Next handed out the rung derived from thirty two zeros -- the same key, and so the same
	// (key, nonce) pair, for every zeroized ratchet in the world, with the stream index
	// durably consumed under it. It is checked BEFORE the allocation so that a call after
	// Zeroize costs no index either.
	zeroized bool
	// set when this ladder can never serve another allocation the store makes. Three ways in,
	// and every one of them is permanent for THIS ratchet: the store refused to allocate at
	// all, the store handed back an index this ladder has already passed, or it handed back
	// one so far ahead that walking to it exceeds maxLadderWalk. A transient failure is none
	// of these and does not set it. A caller that wants to go on rebuilds the ratchet from the
	// store's own high water, which is the one thing that puts a ladder back under its counter.
	wedged bool
	// why it wedged, kept so that the SECOND call answers the same classification the first
	// one did. Without it a caller that retried -- which is what a caller does when it cannot
	// tell a permanent answer from a transient one -- was told ErrSenderRatchetWedged with no
	// cause under it, so the one call that carried ErrStreamIndexRewound or
	// ErrLadderWalkTooLong was the call it had already missed.
	wedgeCause error
}

// NewSenderRatchet builds a sender's ladder for one class key and one leaf, resumed from the
// reserver's durable high water.
//
// The reserver is taken by the constructor and refused if nil, which is section 5.6's own
// instruction -- "the constructor takes the sink to make it explicit". A ratchet without one is
// a ratchet that cannot make the ordering it exists to make.
//
// The stream is the group and the sender handle, and it is CLASS BLIND: ruling A1 puts every
// retention class of one sender on one counter, which is what spec B's schema, spec B's Q7 and
// the shipped server already key on. StreamKey's own comment carries the ruling and the price.
//
// The resume is HighWater() + 1 and is never a recomputed value, and under A1 that number is
// bounded from ABOVE rather than pinned. Next takes its index from the store, and the rung it
// hands out is always record_key[that index] because it walks to it -- so a ladder that resumed
// too LOW is merely slow, and (key, nonce) uniqueness now follows from index uniqueness alone
// rather than from this line. What a ladder must never do is resume ABOVE the store's next
// allocation: it would stand past an index the store is about to hand out, and the only honest
// answer to that is the rewind wedge in Next. HighWater() + 1 is exactly the store's next
// allocation, which is why it is still what this constructor reads and why it is still read from
// the store rather than recomputed from anything this process remembers.
//
// The walk is the cost the file comment prices, and it is BOUNDED by maxLadderWalk. It advances
// the ladder once per index below the resume point, erasing each rung as it passes, so the
// ratchet holds record_key[position] and nothing below it when the constructor returns; a high
// water further out than that bound is refused rather than walked, because a store that hands
// back an enormous number is a store that has been rolled back or corrupted and the walk is
// otherwise unbounded work on its say so.
func NewSenderRatchet(classKey []byte, leaf uint32, stream StreamKey, reserver StreamIndexReserver) (*SenderRatchet, error) {
	if reserver == nil {
		return nil, fmt.Errorf("%w: a sender ratchet reserves before it derives", ErrNilStreamIndexReserver)
	}
	highWater, err := reserver.HighWater(stream)
	if err != nil {
		return nil, fmt.Errorf("messagegroup: a sender ratchet could not read its stream index high water: %w", err)
	}
	if highWater == ^uint64(0) {
		return nil, fmt.Errorf("%w: the high water is already %d", ErrSenderRatchetExhausted, highWater)
	}
	position := highWater + 1
	if maxLadderWalk < position {
		return nil, fmt.Errorf("%w: resuming at %d would walk %d rungs and the bound is %d",
			ErrLadderWalkTooLong, position, position, maxLadderWalk)
	}
	recordKey := RecordKeyZero(classKey, leaf)
	for walked := uint64(0); walked < position; walked += 1 {
		recordKey = stepRecordKey(recordKey)
	}
	return &SenderRatchet{
		reserver:  reserver,
		stream:    stream,
		recordKey: recordKey,
		position:  position,
	}, nil
}

// Next allocates the next stream index of this sender's stream durably, walks the ladder up to
// it, and hands out the rung that goes with it.
//
// THE ORDER IS THE PROPERTY. The allocation is made first, its error is checked first, and the
// function returns on a non-nil error before anything of the key schedule is reached. A body
// that called Reserve and carried on regardless is reachability-identical to this one and is a
// nonce reuse machine, which is why ratchet_test.go asserts the order off the syntax tree as
// well as through an injected failing reserver.
//
// THE INDEX IS THE STORE'S ANSWER AND THE LADDER FOLLOWS IT. Ruling A1 shares one counter across
// every retention class of one sender, so between two of this ladder's own sends its siblings may
// have taken any number of positions. The catch-up walk is what keeps i = stream_index true in
// the face of that: each skipped rung is derived and immediately erased by stepRecordKey, so the
// ratchet ends holding record_key[index] with nothing below it, and the rung it hands out is the
// one the receiver at that index will derive. A body that took the store's index and handed out
// its own next rung would compile, round-trip against itself, and be undecryptable by every peer.
//
// Three answers from the store are PERMANENT for this ladder and all three wedge it, because
// none of them can become true later and a ratchet that retried would burn a durable index per
// attempt. The store refusing to allocate at all is one. An index at or below this ladder's own
// position is a store that went backwards under a live process, and walking backwards is
// impossible for a forward ratchet -- it is also the exact reading that catches a resume set too
// high. And an index further ahead than maxLadderWalk is a walk this package will not pay, for
// the reason maxLadderWalk itself exists: it is unbounded work on the say-so of a number nothing
// has authenticated. A transient failure is none of these; the ladder does not move and the next
// call asks again, which is what makes a full disk a retry rather than a hole in the stream.
//
// What is handed out is a COPY, and the ratchet's own array is erased in place. Section 5.5 asks
// for exactly that -- "Next() overwrites the previous key with zeros before returning" -- and
// gives the reason: the common case, a key still sitting in a live struct field, is entirely
// preventable. The caller owns what it is given and owes it the same erasure after use.
//
// The noinline directive is this package's erase helper class, reached through the hand-off: the
// ratchet's recordKey outlives this call and zeroize is where the stores are.
//
//go:noinline
func (self *SenderRatchet) Next() (uint64, []byte, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	// the three dead states are refused BEFORE the allocation, so a call on any of them costs
	// no index: a zeroized ratchet would otherwise burn a durable index under a key of thirty
	// two zeros, and a wedged or exhausted one would burn one it can never serve.
	if self.zeroized {
		return 0, nil, fmt.Errorf("%w: it holds no ladder to answer from", ErrRatchetZeroized)
	}
	if self.wedged {
		// the cause is carried on every call and not only on the first, so a caller that
		// retried still learns which of the three permanent answers it met.
		return 0, nil, fmt.Errorf("%w: this ladder stands at index %d: %w",
			ErrSenderRatchetWedged, self.position, self.wedgeCause)
	}
	if self.exhausted {
		return 0, nil, fmt.Errorf("%w: index %d was the last", ErrSenderRatchetExhausted, self.position)
	}
	index, err := self.reserver.Reserve(self.stream)
	if err != nil {
		if errors.Is(err, ErrStreamIndexConsumed) {
			// PERMANENT, and told apart from the transient case because the two want
			// opposite answers. A store that cannot allocate will not start being able
			// to, so a ratchet that went on asking would refuse every send forever while
			// reporting a retryable error and would pay a durable write for each attempt.
			// So the ratchet stops and says so.
			self.wedged, self.wedgeCause = true, err
			return 0, nil, fmt.Errorf("%w: the store would not allocate: %w", ErrSenderRatchetWedged, err)
		}
		// no index and no key leave this function on a failed allocation, and the ratchet
		// does not move.
		return 0, nil, fmt.Errorf("messagegroup: a sender ratchet could not allocate its next stream index: %w", err)
	}
	if index < self.position {
		// the store went backwards under a live ladder. Serving it would mean deriving a
		// rung this ladder has already erased -- which it cannot -- or handing out a
		// different rung under an index some record already used, which is the reuse the
		// reservation exists to prevent.
		//
		// This arm is REDUNDANT against the unsigned subtraction below and is kept anyway,
		// which is the argument classifyLocked's "below the head" arm already makes one type
		// over. With it deleted, index-self.position underflows to an enormous number and the
		// bound below wedges the same call -- so no input tells the two spellings apart, and
		// the caller is told its walk was too long when what actually happened is that its
		// store rewound. Measured: deleting it left the wedge intact and changed only which
		// sentinel came back, which is exactly why the sentinel is asserted per cause.
		self.wedged, self.wedgeCause = true, ErrStreamIndexRewound
		return 0, nil, fmt.Errorf("%w: the store allocated index %d and this ladder stands at %d: %w",
			ErrSenderRatchetWedged, index, self.position, ErrStreamIndexRewound)
	}
	if maxLadderWalk < index-self.position {
		// the sparse ladder's own ceiling. Under one shared counter a quiet class walks the
		// gaps its siblings made, so this is reachable without a corrupt store at all -- it
		// is what a class that went silent for maxLadderWalk of its sender's records meets.
		// Either way the answer is the same one the constructor gives: refuse the walk, and
		// let a caller that wants to go on rebuild from the store's own high water.
		self.wedged, self.wedgeCause = true, ErrLadderWalkTooLong
		return 0, nil, fmt.Errorf("%w: the store allocated index %d, this ladder stands at %d and the walk bound is %d: %w",
			ErrSenderRatchetWedged, index, self.position, maxLadderWalk, ErrLadderWalkTooLong)
	}
	// the catch-up. Every rung between where this ladder stood and where the store put it is
	// derived and erased in the same step, so the gap costs cpu and leaves nothing behind.
	//
	// The walk runs on a local ALIASING this ratchet's own array rather than on the field, and
	// that is a reading rather than a style: connect/mls reads a write to a field holding key
	// material as a drop site and decides it POSITIONALLY, so a field walked in place --
	// self.recordKey = stepRecordKey(self.recordKey), inside a loop -- reads there as an
	// overwrite with no erase in front of it. Walking a local and writing the field ONCE puts
	// the erase where the reading can see it, and it is the same shape PeekFor already uses.
	walking := self.recordKey
	for at := self.position; at < index; at += 1 {
		walking = stepRecordKey(walking)
	}
	self.recordKey = walking
	self.position = index
	handed := append([]byte(nil), self.recordKey...)
	self.recordKey = stepRecordKey(self.recordKey)
	if index == ^uint64(0) {
		// the counter does not wrap. A wrap is not a wasted message: it re-issues every
		// record key and every nonce this sender has used under a class key that has not
		// moved, and both of the record's aeads fall to it.
		self.exhausted = true
	} else {
		self.position = index + 1
	}
	return index, handed, nil
}

// Position is the ladder position this ratchet stands on -- the lowest stream index it can still
// produce a rung for -- for a caller that has to report where a sender is without consuming an
// index to find out.
//
// It is NOT "the index the next call will take". Under ruling A1 the store chooses that, and it
// is at or above this number by however many positions this sender's other retention classes
// have taken in between.
func (self *SenderRatchet) Position() uint64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.position
}

// Zeroize erases the rung this ratchet is holding.
//
// It is owed for the reason ClassKeys.Zeroize is: a session drops its ratchets at a commit, and
// the octets it drops are what forward secrecy is about.
//
//go:noinline
func (self *SenderRatchet) Zeroize() {
	if self == nil {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	zeroize(self.recordKey)
	// AND THE RATCHET IS DEAD, which is the half that was missing. Erasing the array in place
	// and leaving the ratchet live left Next handing out the rung expanded from thirty two
	// zeros -- a value every party in the world can compute, identical across every zeroized
	// ratchet, with the stream index durably consumed under it.
	self.zeroized = true
}

// ReceiverRatchet is one sender's ladder as a receiver walks it, with the rungs it has skipped
// retained in a bounded window.
//
// stateLock guards secret, head, exhausted and window. windowSize is written once by the
// constructor.
//
// WHAT IT IS KEYED BY is not stated by any document. Section 5.5's prose scopes the window to
// (sender_handle, retention class); the constructor here is keyed by (class key, leaf index)
// because that is what record_key[0] binds, and sender_handle and leaf_index are different
// identifiers with different lifetimes -- section 5.3 makes the handle deliberately epoch stable
// and a leaf index is not. Which one keys the table decides whether a member's stream survives an
// epoch change. That is open item M1-11, and ReceiverRatchets below tracks by the handle and the
// retention wire byte because those are what a record carries, while a ratchet is BUILT from the
// leaf because that is what the derivation takes.
type ReceiverRatchet struct {
	stateLock sync.Mutex
	// record_key[head], the next rung this ratchet has not yet produced.
	secret []byte
	head   uint64
	// set when head reached the last index a u64 holds, so "below the head" can still
	// classify it.
	exhausted bool
	// the skipped rungs, by index. nil until the first skip, so an in-order receipt
	// allocates no window at all.
	window     map[uint64][]byte
	windowSize int
	// set by Zeroize, for SenderRatchet.zeroized's reason: a receiver whose window and whose
	// chain array had been erased in place went on answering, and what it answered was the
	// rung expanded from thirty two zeros.
	zeroized bool
	// the order this ratchet was tracked in, which is how the table below breaks a tie
	// between two equally full windows. It is a counter and not a comparison of the two
	// senders octets: guardrail G8 sends every comparison of octets in this tree through
	// subtle.ConstantTimeCompare, an ORDERING cannot be spelled that way, and a handle is
	// public but the rule is derived over the whole tree rather than argued case by case.
	tracked uint64
}

// NewReceiverRatchet builds a receiver's view of one sender's ladder, positioned at headIndex.
//
// headIndex is the ladder position -- and so the stream index, per the pin the file comment
// argues -- of the first rung this ratchet will produce. It is a parameter and not zero because
// a receiver that first hears from a sender part way along its stream has to be able to say so:
// with the window bounded, a ratchet parked at zero can never reach a sender already past the
// window, and no document names who supplies the number. The walk to headIndex costs one
// expansion per index, which is the same price the sender pays to resume.
//
// windowSize bounds BOTH the retained skipped rungs of this ratchet and the forward walk one
// call may make, and the second is what makes an unauthenticated index safe to read: the stream
// index arrives in the record's cleartext header, so a peer that picks one out of the air can
// cost this receiver at most windowSize expansions and windowSize retained keys before it is
// refused.
func NewReceiverRatchet(classKey []byte, leaf uint32, headIndex uint64, windowSize int) (*ReceiverRatchet, error) {
	if windowSize <= 0 {
		return nil, fmt.Errorf("%w: window size %d", ErrWindowSize, windowSize)
	}
	if maxLadderWalk < headIndex {
		return nil, fmt.Errorf("%w: a head of %d would walk %d rungs and the bound is %d",
			ErrLadderWalkTooLong, headIndex, headIndex, maxLadderWalk)
	}
	secret := RecordKeyZero(classKey, leaf)
	for walked := uint64(0); walked < headIndex; walked += 1 {
		secret = stepRecordKey(secret)
	}
	return &ReceiverRatchet{
		secret:     secret,
		head:       headIndex,
		windowSize: windowSize,
	}, nil
}

// PeekFor answers the record key for one stream index WITHOUT moving this ratchet.
//
// IT IS THE FORM AN UNAUTHENTICATED INDEX IS READ THROUGH, and that is the whole reason it
// exists. The stream index arrives in a record's cleartext header; write_auth is a mac under the
// group's write key, which spec A hands to the SERVER, and aad_head binds stream_index only when
// the aead opens -- which is AFTER a key exists. So at the moment this ratchet is asked for a
// key, nothing has authenticated the number it is being asked about. Measured on the committing
// form: two forged headers at head+window moved a window-16 ratchet's head from 0 to 34 and made
// honest indices 1, 2 and 3 permanently undecryptable, and one forged header against a 64 sender
// table at the shipped bounds destroyed 1008 honest retained rungs across the other 63 senders.
// A peek costs at most windowSize expansions and changes NOTHING, so the residual an attacker
// buys with a forged index is cpu and never another member's messages.
//
// The three cases are KeyFor's, decided in the same order, and every one of them answers without
// a write. A retained rung is COPIED rather than handed over, because the window must go on
// holding it until the record it belongs to has actually opened. An index the head has passed is
// refused, because this ratchet no longer holds it and cannot re-derive it -- that is the
// forward secrecy of the ladder and not a lookup failure. An index further ahead than the window
// is refused, and the head does not move on a refusal here for the same reason it does not move
// on an acceptance.
//
// The noinline directive is the convention connect/mls's peekFor states: the walk below erases
// every temporary rung it passes, and those stores are dead in the compiler's reading.
//
//go:noinline
func (self *ReceiverRatchet) PeekFor(index uint64) ([]byte, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.zeroized {
		return nil, fmt.Errorf("%w: it holds no ladder to answer from", ErrRatchetZeroized)
	}
	if retained, isRetained := self.window[index]; isRetained {
		return append([]byte(nil), retained...), nil
	}
	if err := self.classifyLocked(index); err != nil {
		return nil, err
	}
	// a temporary walk over a COPY. The ratchet's own chain array is not touched, so a peek
	// that is never committed leaves this ratchet exactly where it was; the temporaries are
	// erased as the walk passes them, which is stepRecordKey's whole reason for existing.
	walking := append([]byte(nil), self.secret...)
	for at := self.head; at < index; at += 1 {
		walking = stepRecordKey(walking)
	}
	handed := append([]byte(nil), walking...)
	zeroize(walking)
	return handed, nil
}

// Commit applies the movement PeekFor described, once the record at that index has authenticated.
//
// It is the second half of the two phase read and it is where every write lives: the retained
// rung is erased and dropped, or the ladder walks forward, filling the window with the rungs it
// passes and pruning afterwards. A caller that never commits has cost this ratchet nothing.
//
// It answers the same classification PeekFor does, so a commit for an index that has since gone
// out of window is a refusal rather than a walk -- which is what makes the pair safe to call
// with a lock released in between, even though today's caller does not.
//
// The noinline directive is KeyFor's: the prune at the end erases through storage that outlives
// this call.
//
//go:noinline
func (self *ReceiverRatchet) Commit(index uint64) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.zeroized {
		return fmt.Errorf("%w: it holds no ladder to commit against", ErrRatchetZeroized)
	}
	handed, err := self.consumeLocked(index)
	// the rung consumeLocked answers is the one PeekFor already handed the caller, and this
	// caller does not want a second live copy of it: it is erased rather than dropped.
	zeroize(handed)
	return err
}

// KeyFor is PeekFor and Commit in one call, for a caller that has ALREADY authenticated the
// index it is asking about.
//
// It is kept because that caller exists -- a sender's own replay of its own stream, and every
// case in this package's tests -- and because the two phase form is the same walk written twice
// when the index is known good. What it must not be used for is a stream index straight off a
// record header: PeekFor's comment carries the measurement of what that costs.
//
//go:noinline
func (self *ReceiverRatchet) KeyFor(index uint64) ([]byte, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.zeroized {
		return nil, fmt.Errorf("%w: it holds no ladder to answer from", ErrRatchetZeroized)
	}
	return self.consumeLocked(index)
}

// classifyLocked decides whether an index is answerable at all, without touching anything.
//
// Both refusals are ErrOutOfWindow, and the conflation is deliberate but is not free. A rung
// erased because it was answered and a rung evicted because the window filled are the same state
// here -- an index below the head that is not retained -- and telling them apart needs a
// tombstone per index. Section 5.5 asks for one visible outcome, a gap, and both of these are
// one; what a caller CANNOT do with this error is tell a replay from a loss, and open item M1-15
// is where that has to be settled if sdk needs to.
//
// The "below the head" arm is redundant against an unsigned subtraction and is kept anyway: with
// it deleted, index-self.head underflows to an enormous number and the window check refuses the
// same index with the same sentinel, so no input tells the two spellings apart -- which is
// exactly why the arm has to say what it means rather than be left to arithmetic. What it is NOT
// redundant for is the exhausted case, where index == head is below and not ahead.
//
// The caller holds stateLock.
func (self *ReceiverRatchet) classifyLocked(index uint64) error {
	if index < self.head || (self.exhausted && index == self.head) {
		return fmt.Errorf("%w: index %d is below this receiver's head %d", ErrOutOfWindow, index, self.head)
	}
	if uint64(self.windowSize) < index-self.head {
		return fmt.Errorf("%w: index %d is %d ahead of head %d, and the window is %d",
			ErrOutOfWindow, index, index-self.head, self.head, self.windowSize)
	}
	return nil
}

// consumeLocked answers one rung and applies every write that goes with it.
//
// A retained rung is handed over and dropped from the window, so a second request for it is
// refused: a window that hands the same key out twice is a window that survives a replay.
//
// The common case allocates nothing: an index equal to the head takes one step and never touches
// the window, which is section 5.5's first requirement of this function.
//
// The caller holds stateLock.
//
//go:noinline
func (self *ReceiverRatchet) consumeLocked(index uint64) ([]byte, error) {
	if retained, isRetained := self.window[index]; isRetained {
		// the caller owns it from here, so it is dropped rather than erased: erasing it
		// would hand back thirty two zeros, which is a key every party in the world can
		// compute. It is this ratchet own copy and never the chain array, so nothing the
		// ratchet still holds is handed away with it.
		delete(self.window, index)
		return retained, nil
	}
	if err := self.classifyLocked(index); err != nil {
		return nil, err
	}
	// Every rung leaves this ratchet as a COPY and the chain array is erased as the ladder
	// passes it, which is the sender discipline applied on this side too. Without the copy the
	// window and the caller would hold the very array the next step erases, so the erasure
	// would have to be dropped -- and a ladder that never erases the rung it has passed is a
	// ladder an attacker who takes the process reads backwards to the epoch start.
	for self.head < index {
		self.retainLocked(self.head, append([]byte(nil), self.secret...))
		self.secret = stepRecordKey(self.secret)
		self.head += 1
	}
	handed := append([]byte(nil), self.secret...)
	if self.head == ^uint64(0) {
		// there is no successor, and the head stays where it is so that a later request for
		// this index is classified as consumed rather than as future. The chain array is
		// erased anyway: there is nothing left to derive from it.
		self.exhausted = true
		zeroize(self.secret)
	} else {
		self.secret = stepRecordKey(self.secret)
		self.head += 1
	}
	self.pruneLocked()
	return handed, nil
}

// Retained is how many skipped rungs this ratchet is holding, which is what the table's global
// bound is computed over.
func (self *ReceiverRatchet) Retained() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.window)
}

// Zeroize erases the rung this ratchet is parked on and every rung in its window.
//
// The window is walked here rather than handed to eraseLocked one index at a time, and the
// reason is a gate rather than a preference: connect/mls reads erasure FIELD BY FIELD off the
// source, and an erase that reaches a field only through a helper taking an index is an erase
// that reading cannot follow. eraseLocked stays the single site for one entry leaving the window
// while the ratchet is running -- answered, evicted -- and this is the whole map going at once.
//
//go:noinline
func (self *ReceiverRatchet) Zeroize() {
	if self == nil {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	zeroize(self.secret)
	for index, secret := range self.window {
		zeroize(secret)
		delete(self.window, index)
	}
	// and the ratchet is dead, for SenderRatchet.Zeroize's reason.
	self.zeroized = true
}

// retainLocked puts one skipped rung in the window, allocating the window on first use.
//
// The window is allocated lazily, so an in-order receipt -- which retains nothing -- never builds
// one at all: section 5.5 first requirement of KeyFor is that the common case costs one step and
// no window.
//
// The odd looking shape of that guard is deliberate and is a gate rather than a preference. The
// assignment that allocates the window REPLACES whatever the field held, and connect/mls reads
// every write to a field holding key material as a drop site -- satisfied by a refusal that
// LEAVES when there is something to drop, which is this != nil arm, and NOT by the == nil
// presence guard that reads the same way to a person. That distinction was measured over there:
// a production holder dropping a live value behind a presence guard left the whole gate green.
//
// The caller holds stateLock.
//
//go:noinline
func (self *ReceiverRatchet) retainLocked(index uint64, secret []byte) {
	if self.window != nil {
		self.retainIntoWindowLocked(index, secret)
		return
	}
	self.window = map[uint64][]byte{}
	self.retainIntoWindowLocked(index, secret)
}

// retainIntoWindowLocked stores one rung in a window that already exists, erasing anything at
// that index rather than dropping it.
//
// The caller holds stateLock.
//
//go:noinline
func (self *ReceiverRatchet) retainIntoWindowLocked(index uint64, secret []byte) {
	if _, wasRetained := self.window[index]; wasRetained {
		// Reaching this at all means the ladder produced one index twice, which is a defect in
		// this ratchet and not a state a peer can cause. The whole window goes rather than the
		// one entry: what is held is no longer a set of rungs this ratchet can account for, and
		// erasing all of it is both the safe answer and the one connect/mls read off the
		// source, which follows an erase through the FIELD and not through a helper taking an
		// index.
		for existing, dropped := range self.window {
			zeroize(dropped)
			delete(self.window, existing)
		}
	}
	self.window[index] = secret
}

// eraseLocked zeroizes one retained rung and drops it.
//
// It is the one erase site for a rung leaving the window WHILE THE RATCHET IS RUNNING:
// evictOldestLocked chooses which rung goes and then comes here, so the two ways an entry is
// dropped for room cannot have the erasure on one path and not the other. A bare delete leaves
// live record keys wherever the allocator puts them next and nothing this ratchet still reaches
// can see the difference, which is a shape connect/mls has already measured passing every test it
// had.
//
// The two erasures that are NOT here, named rather than left for a reader to find contradicting
// the sentence above. Zeroize walks the whole map itself, because connect/mls reads erasure FIELD
// BY FIELD off the source and cannot follow an erase that reaches a field only through a helper
// taking an index. retainIntoWindowLocked erases what it would overwrite, for the same reading.
// Both are the whole map or one entry going for a reason this one does not cover.
// retainIntoWindowLocked's arm is held by a case that calls it twice at one index directly, which
// is the only way to reach it: it is a defect in this ratchet rather than a state a peer can
// cause, so no behaviour through the public surface gets there.
//
// Total by design: erasing an index that was never retained is a no-op.
//
// The caller holds stateLock.
//
//go:noinline
func (self *ReceiverRatchet) eraseLocked(index uint64) {
	secret, isRetained := self.window[index]
	if !isRetained {
		return
	}
	zeroize(secret)
	delete(self.window, index)
}

// evictOldestLocked drops the oldest retained rung, erasing it in place.
//
// The oldest is what goes, because a skipped index that has not arrived yet grows less likely to
// arrive the older it gets.
//
// The caller holds stateLock. The noinline directive is the same convention KeyFor's is: the
// erasure is eraseLocked's, reached through a method call on this receiver.
//
//go:noinline
func (self *ReceiverRatchet) evictOldestLocked() {
	// deleting this guard changes nothing an input can observe -- eraseLocked on an index that
	// was never retained is already a no-op, and over an empty map the loop below leaves oldest
	// at the sentinel -- so it is a statement of the precondition rather than a check that
	// catches anything. It is kept for that, and it is recorded here rather than claimed as a
	// refusal something holds.
	if len(self.window) == 0 {
		return
	}
	oldest := ^uint64(0)
	for index := range self.window {
		if index < oldest {
			oldest = index
		}
	}
	self.eraseLocked(oldest)
}

// pruneLocked holds THIS ratchet to its own window size.
//
// It is a bound on memory and the party who decides how much of it gets used is whoever writes
// the stream indices. ReceiverRatchets.pruneRetainedLocked is the other half, and it is the half
// that matters when the number of ratchets is not this receiver's choice.
//
// The caller holds stateLock.
func (self *ReceiverRatchet) pruneLocked() {
	for self.windowSize < len(self.window) {
		self.evictOldestLocked()
	}
}

// ReceiverRatchetKey is what one receiver ratchet is tracked under: the handle the server routes
// on, the retention class wire byte the record carries, and the eph window it names.
//
// The retention byte and not the parsed class, because section 5.1 encodes the class and the eph
// bucket in ONE octet -- 0x10 given a bucket, for the eph classes -- and a table keyed on the
// parsed class alone would put two eph buckets on one ladder. It is the wire byte for a second
// reason too: connect/message declares the parsed type, and this package does not put a
// production call across that boundary until task 11.
//
// AND THE WINDOW, FOR THE SAME REASON ONE LEVEL IN, SINCE 2026-09-13. A ratchet is rooted at a
// CLASS KEY, and an EPH record's class key is EphKey(eph_root, bucket, window): two windows of
// one bucket are two class keys and therefore two ladders, exactly as two buckets are. A table
// keyed on the byte alone would walk a record written in window t+1 up the ladder rooted at
// window t's key, derive a rung nothing else in the system holds, and report an AEAD failure
// that says nothing about which of the two values disagreed. For every class but EPH the window
// is zero on every record -- master section 8's presence rule is a zero VALUE, on every
// non-EPH class -- so this field collapses to nothing for them with no special case anywhere.
type ReceiverRatchetKey struct {
	SenderHandle  [16]byte
	RetentionWire byte
	EphWindow     uint64
}

// ReceiverRatchets is the table of one group's receiver ratchets, and the owner of the bound that
// holds regardless of how many senders there are.
//
// Section 5.5 caps the tracked senders at 64 and evicts the OLDEST sender. That is not what this
// does, and the divergence is open item M1-12's labelled recommendation rather than an oversight:
// connect/mls solves the same problem with a bound on the retained keys of the WHOLE table, so
// adding senders adds no memory, and evicts from the FULLEST window, so a member holding FEWER
// THAN ITS FAIR SHARE of the bound never pays for a member holding a thousand -- which is the
// property that is true, and not the looser "a handful never pays for a thousand" this file used
// to claim in both places. pruneRetainedLocked carries the measurement. Section 5.5's rule starves
// whoever went quiet, which is the member most likely to need the window. Section 14 open item 7
// is what has to finalise this and it blocks the A6 freeze; the number is a constructor parameter
// for that reason.
//
// A ratchet is TRACKED and never auto-created, which is what keeps the table's size a fact about
// the group rather than a choice an attacker makes. connect/mls's own pruneRetained exists
// because its receiverKey materialises a ratchet for any leaf a forged header names; here a
// sender this session has not installed a ratchet for is refused with ErrNoReceiverRatchet, so
// the only unbounded quantity left is the retained keys, which is what the bound below is on.
type ReceiverRatchets struct {
	tableLock sync.Mutex
	ratchets  map[ReceiverRatchetKey]*ReceiverRatchet
	// the number of ratchets ever tracked, which is what stamps each one order so a tie
	// between two equally full windows is broken by something stated rather than by go
	// randomised map iteration.
	tracked       uint64
	retainedBound int
}

// NewReceiverRatchets builds an empty table held to a retained key bound.
func NewReceiverRatchets(retainedBound int) (*ReceiverRatchets, error) {
	if retainedBound <= 0 {
		return nil, fmt.Errorf("%w: retained bound %d", ErrWindowSize, retainedBound)
	}
	return &ReceiverRatchets{
		ratchets:      map[ReceiverRatchetKey]*ReceiverRatchet{},
		retainedBound: retainedBound,
	}, nil
}

// Track installs a ratchet under one key, replacing and erasing whatever was there.
//
// The replaced ratchet is zeroized rather than dropped, because a ratchet replaced at an epoch
// change is holding the previous epoch's rungs and those are exactly the octets forward secrecy
// is about.
func (self *ReceiverRatchets) Track(key ReceiverRatchetKey, ratchet *ReceiverRatchet) {
	self.tableLock.Lock()
	defer self.tableLock.Unlock()
	if replaced, wasTracked := self.ratchets[key]; wasTracked {
		replaced.Zeroize()
	}
	self.tracked += 1
	ratchet.tracked = self.tracked
	self.ratchets[key] = ratchet
}

// PeekFor answers one tracked sender's record key for one stream index without moving anything.
//
// It is the form the open path reads an UNAUTHENTICATED stream index through, and
// (*ReceiverRatchet).PeekFor's comment carries the measurement of what the committing form costs
// when the index turns out to be forged. Nothing is retained, nothing is evicted and no head
// moves, so the whole table is unchanged when this returns -- which is why the prune the
// committing form owes is not here.
func (self *ReceiverRatchets) PeekFor(key ReceiverRatchetKey, index uint64) ([]byte, error) {
	self.tableLock.Lock()
	defer self.tableLock.Unlock()
	ratchet, isTracked := self.ratchets[key]
	if !isTracked {
		return nil, fmt.Errorf("%w: sender %x retention %#02x", ErrNoReceiverRatchet, key.SenderHandle, key.RetentionWire)
	}
	return ratchet.PeekFor(index)
}

// Commit applies the movement PeekFor described, once the record has authenticated, and holds
// the whole table to its retained bound afterwards.
//
// The noinline directive is the convention the two ratchets keep: the prune at the end erases
// through storage that outlives this call.
//
//go:noinline
func (self *ReceiverRatchets) Commit(key ReceiverRatchetKey, index uint64) error {
	self.tableLock.Lock()
	defer self.tableLock.Unlock()
	ratchet, isTracked := self.ratchets[key]
	if !isTracked {
		return fmt.Errorf("%w: sender %x retention %#02x", ErrNoReceiverRatchet, key.SenderHandle, key.RetentionWire)
	}
	if err := ratchet.Commit(index); err != nil {
		return err
	}
	self.pruneRetainedLocked()
	return nil
}

// KeyFor answers one tracked sender's record key for one stream index, and holds the whole table
// to its retained bound afterwards.
//
// It is PeekFor and Commit in one call and it carries their warning: this is the form for an
// index the caller has already authenticated, and the open path uses the two phase pair instead.
//
// The noinline directive is the convention the two ratchets keep: the prune at the end erases
// through storage that outlives this call. It is also what the derived class demands, because
// that class closes over BARE NAMES and (*ReceiverRatchet).KeyFor is a member -- a widening the
// gate states, and one that costs a pragma rather than accuracy.
//
//go:noinline
func (self *ReceiverRatchets) KeyFor(key ReceiverRatchetKey, index uint64) ([]byte, error) {
	self.tableLock.Lock()
	defer self.tableLock.Unlock()
	ratchet, isTracked := self.ratchets[key]
	if !isTracked {
		return nil, fmt.Errorf("%w: sender %x retention %#02x", ErrNoReceiverRatchet, key.SenderHandle, key.RetentionWire)
	}
	answer, err := ratchet.KeyFor(index)
	if err != nil {
		return nil, err
	}
	self.pruneRetainedLocked()
	return answer, nil
}

// Retained is how many skipped rungs the whole table is holding.
func (self *ReceiverRatchets) Retained() int {
	self.tableLock.Lock()
	defer self.tableLock.Unlock()
	return self.retainedLocked()
}

// Zeroize erases every ratchet in the table.
//
// The noinline directive is the convention again: the erasure is each ratchet's own, reached
// through a method call on a value ranged out of this table.
//
//go:noinline
func (self *ReceiverRatchets) Zeroize() {
	self.tableLock.Lock()
	defer self.tableLock.Unlock()
	for key, ratchet := range self.ratchets {
		ratchet.Zeroize()
		delete(self.ratchets, key)
	}
}

// retainedLocked sums the windows of every ratchet in the table.
//
// The caller holds tableLock.
func (self *ReceiverRatchets) retainedLocked() int {
	retained := 0
	for _, ratchet := range self.ratchets {
		retained += ratchet.Retained()
	}
	return retained
}

// pruneRetainedLocked holds the skipped rungs retained across EVERY ratchet of this table to one
// bound, evicting from the fullest window.
//
// The choice of WHICH window is the half that matters. Evicting the globally oldest rung would
// let one flooding sender push out the handful of keys an honest out of order sender is holding,
// which turns a memory bound into a way to drop other members' messages; taking from the largest
// holder puts the pressure on whoever created it.
//
// WHAT THAT BUYS, STATED AS THE PROPERTY IT ACTUALLY IS. Eviction only ever touches the fullest
// window, so a ratchet holding fewer rungs than every other holder is never the victim; and a
// ratchet holding fewer than retainedBound/len(ratchets) is never the victim at all, because for
// it to be the fullest every other window would have to be no larger and the total would then
// already be under the bound. That is the sentence this file used to write as "a handful never
// pays for a thousand", which is a DIFFERENT and false claim: above the fair share everyone
// pays, and at the shipped defaults -- where the table bound equals one window -- a single full
// window reorder takes rungs from every other honest sender. The fair share form is what a three
// sender case can falsify and the loose form is what two senders cannot.
//
// The tie between two equally full windows is
// broken by the order the two were TRACKED in rather than by go's randomised map iteration, so the
// behaviour can be stated and tested rather than merely bounded -- and it IS tested: the earlier
// tracked of two equally full windows is the one that gives up a rung, and Track's stamp is what
// makes that an order rather than a coin toss. It is not broken by comparing
// the two senders handles: guardrail G8 sends every comparison of octets in this tree through
// subtle.ConstantTimeCompare, which answers equality and cannot answer an ordering, and the rule
// is derived over the whole tree rather than argued case by case.
//
// The caller holds tableLock.
func (self *ReceiverRatchets) pruneRetainedLocked() {
	for self.retainedBound < self.retainedLocked() {
		var fullest *ReceiverRatchet
		fullestHeld := 0
		for _, ratchet := range self.ratchets {
			held := ratchet.Retained()
			if fullest == nil || fullestHeld < held ||
				(fullestHeld == held && ratchet.tracked < fullest.tracked) {
				fullest, fullestHeld = ratchet, held
			}
		}
		if fullest == nil || fullestHeld == 0 {
			// unreachable: the total is the sum of the windows, so a total over the bound
			// means some window is non empty and the fullest is one of those. It is a
			// return and not an assertion because the alternative for a loop whose only
			// exit is the bound would be to spin forever on a state it cannot fix.
			return
		}
		fullest.stateLock.Lock()
		fullest.evictOldestLocked()
		fullest.stateLock.Unlock()
	}
}

// stepRecordKey advances one rung of the ladder and erases the rung it came from.
//
// It is one helper for both resume walks, because the derivation and the erasure belong together:
// RecordKeyNext deliberately does not erase its input -- a receiver filling its window has to
// KEEP the rungs it passes -- so every caller that is walking PAST a rung rather than retaining
// it owes the erasure, and a body that spelled the two lines itself is a body one edit away from
// leaving a whole ladder in the heap.
//
// The noinline directive is this package's erase helper class, reached through the hand-off.
//
//go:noinline
func stepRecordKey(recordKey []byte) []byte {
	successor := RecordKeyNext(recordKey)
	zeroize(recordKey)
	return successor
}
