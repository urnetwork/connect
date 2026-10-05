// The five defects batch B's review reproduced, each held by the property it escaped through
// rather than by the shape of the fix.
//
// They are in a file of their own because each is a REGRESSION case with a measurement behind it,
// and a reader asking "what stopped this" should find the measurement beside the assertion rather
// than three files away.
package messagegroup

import (
	"bytes"
	"errors"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// a zeroized ratchet is dead, and the key it would otherwise hand out
// ---------------------------------------------------------------------------

// Zeroize erased the arrays in place and left both ratchets fully operational, so the next call
// derived from thirty two zeros. Measured: two ratchets on DIFFERENT groups and DIFFERENT leaves
// both answered index 1 with key 00*32 after Zeroize, and RecordAeadBody of that constant is one
// key and one nonce every party in the world can compute. The reservation SUCCEEDED, so the index
// was durably consumed under it.
//
// The case asserts the refusal AND the shape of what the defect produced, so a fix that refused
// for some other reason -- or one that answered a different constant -- is still reported.
func TestAZeroizedSenderRatchetHandsOutNothing(t *testing.T) {
	ratchet, err := NewSenderRatchet(ratchetClassKey(), ratchetLeaf, ratchetGroup, newStreamIndexMemory())
	if err != nil {
		t.Fatalf("build the ratchet: %v", err)
	}
	index, key, err := ratchet.Next()
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if len(key) == 0 {
		t.Fatal("the ratchet answered no key before it was zeroized, so this case would pass over one that answers nothing at all")
	}
	zeroize(key)
	ratchet.Zeroize()
	for attempt := range 3 {
		index, key, err = ratchet.Next()
		if !errors.Is(err, ErrRatchetZeroized) {
			t.Errorf("attempt %d after Zeroize answered %v, want ErrRatchetZeroized", attempt, err)
		}
		if key != nil || index != 0 {
			t.Errorf("attempt %d after Zeroize answered index %d and a %d octet key", attempt, index, len(key))
		}
	}
	// and the index is not burned either: the refusal is BEFORE the reservation, so a call
	// after Zeroize costs the store nothing.
	reserver := newStreamIndexMemory()
	second, err := NewSenderRatchet(ratchetClassKey(), ratchetLeaf, ratchetGroup, reserver)
	if err != nil {
		t.Fatalf("build the second ratchet: %v", err)
	}
	second.Zeroize()
	if _, _, err := second.Next(); !errors.Is(err, ErrRatchetZeroized) {
		t.Fatalf("the second ratchet answered %v", err)
	}
	if high, _ := reserver.HighWater(ratchetGroup); high != 0 {
		t.Errorf("a call on a zeroized ratchet moved the store's high water to %d; the refusal is ordered before the reservation exactly so that it costs no index", high)
	}
}

func TestAZeroizedReceiverRatchetHandsOutNothing(t *testing.T) {
	ratchet, err := NewReceiverRatchet(ratchetClassKey(), ratchetLeaf, 0, 8)
	if err != nil {
		t.Fatalf("build the ratchet: %v", err)
	}
	key, err := ratchet.KeyFor(0)
	if err != nil {
		t.Fatalf("key for 0: %v", err)
	}
	// the constant the defect produced, computed here so the assertion is about the VALUE and
	// not merely about an error being returned.
	worldReadable, err := NewReceiverRatchet(make([]byte, classKeyBytes), ratchetLeaf, 0, 8)
	if err != nil {
		t.Fatalf("build the control: %v", err)
	}
	fromZeros, err := worldReadable.KeyFor(0)
	if err != nil {
		t.Fatalf("the control could not answer: %v", err)
	}
	if bytes.Equal(key, fromZeros) {
		t.Fatal("a live ratchet already answers the key a zeroized class key produces, so this case cannot tell the two apart")
	}
	zeroize(key)
	ratchet.Zeroize()
	for _, refusal := range []struct {
		name string
		call func() ([]byte, error)
	}{
		{name: "KeyFor", call: func() ([]byte, error) { return ratchet.KeyFor(1) }},
		{name: "PeekFor", call: func() ([]byte, error) { return ratchet.PeekFor(1) }},
		{name: "Commit", call: func() ([]byte, error) { return nil, ratchet.Commit(1) }},
	} {
		answer, err := refusal.call()
		if !errors.Is(err, ErrRatchetZeroized) {
			t.Errorf("%s after Zeroize answered %v, want ErrRatchetZeroized", refusal.name, err)
		}
		if answer != nil {
			t.Errorf("%s after Zeroize answered %d octets beside its error", refusal.name, len(answer))
		}
	}
}

// ---------------------------------------------------------------------------
// ruling A1: every retention class of one sender shares one counter
// ---------------------------------------------------------------------------

// TWO LADDERS OF ONE SENDER SHARE ONE COUNTER, INTERLEAVE, AND NEITHER WEDGES THE OTHER.
//
// THIS IS THE OPPOSITE OF THE CASE IT REPLACES AND THE REVERSAL IS THE POINT. Wave 1's
// TestTwoRetentionClassesOfOneGroupDoNotShareACounter required the durable and permanent ladders
// of one group to count independently -- both at 1, both at 2 -- and it was right about the
// hazard it was built for: under the ASSERT shape, two ladders keyed on the group alone left the
// second answering "a stream index has already been consumed" forever with its position stuck at
// 1, so at most one class per group could ever send. The owner's ruling of 2026-09-07 says the
// key was the wrong place to fix that, because a per-class client counter disagrees with the
// server's own (group_id, sender_handle) row and its second class's first record is refused as a
// regression. The wedge is closed in the INTERFACE instead: Reserve allocates.
//
// So what is asserted here is what the ruling actually buys, and every clause of it can fail. The
// two ladders interleave over one counter rather than repeating each other's numbers; the union
// of what they take is a contiguous run with no index handed out twice; each one's own indices
// climb; and -- the clause the wave 1 case shared and the one that matters most -- one index
// under two class keys is never one record key.
func TestTwoRetentionClassesOfOneSenderShareOneCounter(t *testing.T) {
	reserver := newStreamIndexMemory()
	classKeys := DeriveClassKeys(StorageRoot(keyScheduleKatInputs()))
	stream := streamKeyNamed("one sender of one group")
	durable, err := NewSenderRatchet(classKeys.Durable, ratchetLeaf, stream, reserver)
	if err != nil {
		t.Fatalf("the durable ratchet: %v", err)
	}
	permanent, err := NewSenderRatchet(classKeys.Perm, ratchetLeaf, stream, reserver)
	if err != nil {
		t.Fatalf("the permanent ratchet: %v", err)
	}
	taken := map[uint64]string{}
	lastDurable, lastPerm := uint64(0), uint64(0)
	for round := range 4 {
		durableIndex, durableKey, err := durable.Next()
		if err != nil {
			t.Fatalf("round %d, the durable ratchet: %v; two ladders on one counter must not wedge each other", round, err)
		}
		permIndex, permKey, err := permanent.Next()
		if err != nil {
			t.Fatalf("round %d, the permanent ratchet: %v; two ladders on one counter must not wedge each other", round, err)
		}
		for _, got := range []struct {
			class string
			index uint64
		}{{"durable", durableIndex}, {"permanent", permIndex}} {
			if earlier, isRepeat := taken[got.index]; isRepeat {
				t.Errorf("round %d: index %d went to %s after %s; one index under two class keys is a stream the server cannot order",
					round, got.index, got.class, earlier)
			}
			taken[got.index] = got.class
		}
		if durableIndex <= lastDurable && round != 0 {
			t.Errorf("round %d: the durable ladder went from %d to %d", round, lastDurable, durableIndex)
		}
		if permIndex <= lastPerm && round != 0 {
			t.Errorf("round %d: the permanent ladder went from %d to %d", round, lastPerm, permIndex)
		}
		lastDurable, lastPerm = durableIndex, permIndex
		if bytes.Equal(durableKey, permKey) {
			t.Errorf("round %d: the two classes answered one record key", round)
		}
		zeroize(durableKey)
		zeroize(permKey)
	}
	// the union is a contiguous run from 1, which is the shape a single counter makes and the
	// shape two counters cannot: two independent counters would have taken 1..4 twice.
	for want := uint64(1); want <= 8; want += 1 {
		if _, isTaken := taken[want]; !isTaken {
			t.Errorf("index %d was taken by neither ladder; eight sends off one counter are indices 1 through 8", want)
		}
	}
	if len(taken) != 8 {
		t.Errorf("eight sends produced %d distinct indices", len(taken))
	}
}

// A sparse ladder walks the gaps its siblings made, and the rung it hands out is the one its
// OWN index names.
//
// This is the property that makes ruling A1's shared counter safe rather than merely
// non-wedging, and it is the one a plausible wrong implementation fails: a Next that took the
// store's index and handed out its own next rung -- one step per call, ignoring the gap --
// compiles, round-trips against itself, and is undecryptable by every peer, because a receiver
// derives record_key[stream_index] and nothing else. So the ladder's answer is compared against
// an INDEPENDENT walk of the same ladder rather than against itself.
func TestASparseLadderHandsOutTheRungItsOwnIndexNames(t *testing.T) {
	reserver := newStreamIndexMemory()
	classKeys := DeriveClassKeys(StorageRoot(keyScheduleKatInputs()))
	stream := streamKeyNamed("a sparse ladder")
	// the chatty sibling: it is the same stream and a different class key, so every index it
	// takes is a gap in the quiet ladder below.
	chatty, err := NewSenderRatchet(classKeys.Perm, ratchetLeaf, stream, reserver)
	if err != nil {
		t.Fatalf("the chatty ratchet: %v", err)
	}
	quiet, err := NewSenderRatchet(classKeys.Durable, ratchetLeaf, stream, reserver)
	if err != nil {
		t.Fatalf("the quiet ratchet: %v", err)
	}
	// the independent walk: a receiver's view of the quiet ladder, which knows nothing about
	// the sender's own state and derives every rung from record_key[0].
	receiver, err := NewReceiverRatchet(classKeys.Durable, ratchetLeaf, 0, DefaultRecordWindowSize)
	if err != nil {
		t.Fatalf("the receiver: %v", err)
	}
	gaps := 0
	for round := range 6 {
		for burst := range round + 1 {
			index, key, err := chatty.Next()
			if err != nil {
				t.Fatalf("round %d burst %d: %v", round, burst, err)
			}
			gaps += 1
			zeroize(key)
			_ = index
		}
		index, key, err := quiet.Next()
		if err != nil {
			t.Fatalf("round %d, the quiet ladder: %v", round, err)
		}
		want, err := receiver.KeyFor(index)
		if err != nil {
			t.Fatalf("round %d, the receiver at index %d: %v", round, index, err)
		}
		if !bytes.Equal(key, want) {
			t.Fatalf("round %d: the quiet ladder handed out a rung at index %d that a receiver deriving record_key[%d] does not compute; a sparse ladder that stepped once per send rather than once per index would seal records nothing can open",
				round, index, index)
		}
		zeroize(key)
		zeroize(want)
	}
	if gaps == 0 {
		t.Error("the chatty ladder took no index, so the quiet ladder was never sparse and this case judged nothing")
	}
	if quiet.Position() <= uint64(gaps) {
		t.Errorf("the quiet ladder stands at %d after %d indices went to its sibling; a ladder that ignored the gaps would stand at its own send count", quiet.Position(), gaps)
	}
}

// A store the ratchet cannot follow is a PERMANENT stop, and the three ways in are told apart
// from a transient failure and from each other.
//
// Under ruling A1 the ratchet no longer chooses an index, so "an index that has already been
// consumed" is no longer something a caller can offer. What remains is what the STORE can answer
// that a forward ladder cannot serve, and all three wedge: a refusal to allocate at all, an
// allocation at or below where the ladder stands, and one so far ahead that the catch-up walk
// exceeds maxLadderWalk. Each wraps its own sentinel, so a caller can tell them apart, and a
// transient failure is none of them -- the ladder does not move and the next call asks again,
// which is what makes a full disk a retry rather than a hole in the stream.
func TestAStoreTheLadderCannotFollowWedgesItAndATransientFailureDoesNot(t *testing.T) {
	transient := &streamIndexRefusing{err: errors.New("the disk is full")}
	retrying, err := NewSenderRatchet(ratchetClassKey(), ratchetLeaf, ratchetGroup, transient)
	if err != nil {
		t.Fatalf("build the ratchet: %v", err)
	}
	for attempt := range 3 {
		if _, _, err := retrying.Next(); errors.Is(err, ErrSenderRatchetWedged) {
			t.Fatalf("attempt %d treated a transient failure as permanent", attempt)
		}
		if retrying.Position() != 1 {
			t.Errorf("attempt %d moved the ladder to %d; a failed allocation leaves it standing where it was", attempt, retrying.Position())
		}
	}
	if transient.reserves != 3 {
		t.Errorf("a transiently failing store was asked %d times in three calls; a retryable failure is retried", transient.reserves)
	}

	permanentWedges := []string{}
	for _, permanent := range []struct {
		name     string
		reserver StreamIndexReserver
		sentinel error
		asks     int
	}{
		{
			name:     "the store will not allocate",
			reserver: &streamIndexRefusing{err: ErrStreamIndexConsumed},
			sentinel: ErrStreamIndexConsumed,
			asks:     1,
		},
		{
			// a ladder resumed at 1 meeting an allocation of 1 has already passed
			// nothing, so the case that has to wedge is an allocation BELOW the
			// standing position: here the ladder is walked to 3 and then handed 2.
			name:     "the store went backwards under a live ladder",
			reserver: &streamIndexScripted{answers: []uint64{3, 2}},
			sentinel: ErrStreamIndexRewound,
			asks:     2,
		},
		{
			name:     "the walk to the allocation is past the bound",
			reserver: &streamIndexScripted{answers: []uint64{maxLadderWalk + 2}},
			sentinel: ErrLadderWalkTooLong,
			asks:     1,
		},
	} {
		permanentWedges = append(permanentWedges, permanent.name)
		t.Run(permanent.name, func(t *testing.T) {
			ratchet, err := NewSenderRatchet(ratchetClassKey(), ratchetLeaf, ratchetGroup, permanent.reserver)
			if err != nil {
				t.Fatalf("build the ratchet: %v", err)
			}
			// the calls before the wedging one, if the case needs the ladder moved first
			for warm := 1; warm < permanent.asks; warm += 1 {
				if _, key, err := ratchet.Next(); err != nil {
					t.Fatalf("warm-up call %d: %v", warm, err)
				} else {
					zeroize(key)
				}
			}
			for attempt := range 3 {
				index, key, err := ratchet.Next()
				if !errors.Is(err, ErrSenderRatchetWedged) {
					t.Errorf("attempt %d answered %v, want ErrSenderRatchetWedged", attempt, err)
				}
				if !errors.Is(err, permanent.sentinel) {
					t.Errorf("attempt %d answered %v, which does not carry %v; the three ways to wedge want telling apart",
						attempt, err, permanent.sentinel)
				}
				if key != nil || index != 0 {
					t.Errorf("attempt %d answered index %d and a %d octet key", attempt, index, len(key))
				}
			}
			asked := 0
			switch counted := permanent.reserver.(type) {
			case *streamIndexRefusing:
				asked = counted.reserves
			case *streamIndexScripted:
				asked = counted.reserves
			}
			if asked != permanent.asks {
				t.Errorf("the wedged ratchet asked the store %d times, want %d; the point of wedging is that it stops asking, and every ask is a durable write",
					asked, permanent.asks)
			}
		})
	}
	// AND THE TABLE ABOVE IS COMPLETE, checked against the production source rather than
	// against the sentence that introduces it. "Three ways in" is exactly the kind of claim
	// this project has been burned by -- a table named "every rule of the CreateGroup carve
	// out" held five of six -- so the count is DERIVED: every site in Next that sets the wedge
	// is one permanent answer, and a fourth added later leaves this table visibly short.
	if wedges := senderRatchetWedgeSites(t); wedges != len(permanentWedges) {
		t.Errorf("Next wedges the ladder at %d site(s) and this case drives %d of them (%v); a way in that no case drives is a permanent refusal nothing has ever seen",
			wedges, len(permanentWedges), permanentWedges)
	}
}

// senderRatchetWedgeSites is how many places in SenderRatchet.Next set the wedge, read off this
// package's own source.
//
// It counts ASSIGNMENTS to the field and not calls to a helper, because the field is set inline
// at each site; if that ever changes, this reader answers zero and the fatal below is the report.
func senderRatchetWedgeSites(t *testing.T) int {
	t.Helper()
	_, sources := messagegroupProductionSources(t)
	sites := 0
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Name.Name != "Next" || function.Recv == nil {
				continue
			}
			if recordKeyReceiverTypeName(function.Recv.List[0].Type) != "SenderRatchet" {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				assign, isAssign := node.(*ast.AssignStmt)
				if !isAssign {
					return true
				}
				for _, left := range assign.Lhs {
					selector, isSelector := left.(*ast.SelectorExpr)
					if isSelector && selector.Sel.Name == "wedged" {
						sites += 1
					}
				}
				return true
			})
		}
	}
	if sites == 0 {
		t.Fatal("no assignment to the wedge was found in SenderRatchet.Next, so the completeness check above read nothing")
	}
	return sites
}

// ---------------------------------------------------------------------------
// open item M1-25, made observable rather than described
// ---------------------------------------------------------------------------

// EPH TRANSIENTS SPEND THE ONE COUNTER, AND 1,025 OF THEM PUT THE NEXT DURABLE RECORD PERMANENTLY
// OUT OF A RECEIVER'S WINDOW.
//
// THIS CASE RULES NOTHING. Open item M1-25 asks whether transients get a counter of their own and
// it is not this commit's to answer -- giving them one re-opens the same collision for EPH heads,
// which ledger item 152 ruled onto this root on 2026-09-13. What is owed here is that the hazard is
// EXECUTABLE, because the alternative -- a comment claiming it -- is the thing this project has
// been burned by: a sentence in a header cannot go red.
//
// The mechanism, which is arithmetic and not a guess. Section 5.6 makes an EPH(bucket 0)
// transient consume a stream index locally. Ruling A1 puts every class of one sender on one
// counter. A receiver's window is refused by DISTANCE from its head -- classifyLocked compares
// index - head against windowSize -- and the head of a DURABLE receiver moves only when a DURABLE
// record arrives. So transients between two durable records are pure distance: at windowSize
// there is exactly one more than the window can hold, and the second durable record is
// ErrOutOfWindow, which section 5.5 turns into a gap entry the message never comes back from.
//
// The eph ladder is driven through the reserver rather than through a session, because what the
// transients spend is the COUNTER and that is the part this case is about, and driving it through
// the reserver keeps the case about the counter whatever key the records would take. The sentence
// that stood here gave a second reason that is no longer true: it said the eph classes had no
// class key at all, citing MASTER invariant I4, and that the sealer admitted one class pending
// M1-6 -- ruled 2026-09-07, reversed 2026-09-13. It is DESCRIBED and not quoted, for the reason
// doc.go gives about its own retraction --
// this package holds an inventory gate over prose, and a retracted sentence reproduced verbatim
// is a sentence somebody's grep finds. The half of it that was never true is the first clause: I4
// keeps eph_root out of ClassKeys and has never meant the eph classes have no key. M1-6 was ruled
// 2026-09-07 and reversed 2026-09-13, ledger item 152 was ruled with it, and a session holding an
// eph_root now seals every class.
func TestTransientsOnTheSharedCounterStarveADurableReceiverWindow(t *testing.T) {
	const windowSize = DefaultRecordWindowSize
	reserver := newStreamIndexMemory()
	classKeys := DeriveClassKeys(StorageRoot(keyScheduleKatInputs()))
	stream := streamKeyNamed("a sender that types")
	durable, err := NewSenderRatchet(classKeys.Durable, ratchetLeaf, stream, reserver)
	if err != nil {
		t.Fatalf("the durable ratchet: %v", err)
	}
	receiver, err := NewReceiverRatchet(classKeys.Durable, ratchetLeaf, 0, windowSize)
	if err != nil {
		t.Fatalf("the receiver: %v", err)
	}
	first, firstKey, err := durable.Next()
	if err != nil {
		t.Fatalf("the first durable record: %v", err)
	}
	if _, err := receiver.KeyFor(first); err != nil {
		t.Fatalf("the receiver could not open the first durable record at index %d: %v", first, err)
	}
	zeroize(firstKey)

	// one short of the wall first, so the failure below is attributable to the last transient
	// and not to the window being wrong from the start.
	for typed := 0; typed < windowSize-1; typed += 1 {
		if _, err := reserver.Reserve(stream); err != nil {
			t.Fatalf("transient %d: %v", typed, err)
		}
	}
	survivable, survivableKey, err := durable.Next()
	if err != nil {
		t.Fatalf("the durable record after %d transients: %v", windowSize-1, err)
	}
	if _, err := receiver.KeyFor(survivable); err != nil {
		t.Fatalf("index %d is already out of window after %d transients, so this case is measuring the wrong wall: %v",
			survivable, windowSize-1, err)
	}
	zeroize(survivableKey)

	// and now one more than the window holds. The receiver's head is at the record it just
	// opened, so this is pure distance.
	for typed := 0; typed <= windowSize; typed += 1 {
		if _, err := reserver.Reserve(stream); err != nil {
			t.Fatalf("transient %d of the starving run: %v", typed, err)
		}
	}
	starved, starvedKey, err := durable.Next()
	if err != nil {
		t.Fatalf("the durable record after %d transients: %v", windowSize+1, err)
	}
	zeroize(starvedKey)
	if _, err := receiver.KeyFor(starved); !errors.Is(err, ErrOutOfWindow) {
		t.Fatalf("index %d, %d positions past the receiver's head after %d transients, answered %v; M1-25 is open BECAUSE this is what a typing indicator costs, and a case that cannot see it is a case that cannot report it",
			starved, starved-survivable, windowSize+1, err)
	}
	// and it is permanent: the record is undecryptable and stays that way, which section 5.5
	// surfaces as a gap entry rather than as an error the caller can retry past.
	if _, err := receiver.PeekFor(starved); !errors.Is(err, ErrOutOfWindow) {
		t.Errorf("a second look at index %d answered %v; the refusal is a property of the distance and does not heal", starved, err)
	}
	if starved-survivable != uint64(windowSize+2) {
		t.Errorf("the starved record is %d positions past the last one that opened; %d transients plus the durable record itself is %d",
			starved-survivable, windowSize+1, windowSize+2)
	}
}

// ---------------------------------------------------------------------------
// both ladder walks are bounded
// ---------------------------------------------------------------------------

// Measured at roughly four hundred nanoseconds a rung: a head of 2^32 is about half an hour of one
// core and 2^63 never returns. Neither number is authenticated at the moment it is read -- a
// receiver's head is a position in a peer's stream and a sender's resume is whatever a store hands
// back -- so an unbounded walk is a denial with no ceiling.
func TestNeitherLadderResumeWalksWithoutABound(t *testing.T) {
	if _, err := NewReceiverRatchet(ratchetClassKey(), ratchetLeaf, uint64(maxLadderWalk)+1, 8); !errors.Is(err, ErrLadderWalkTooLong) {
		t.Errorf("a receiver at one past the bound answered %v, want ErrLadderWalkTooLong", err)
	}
	if _, err := NewReceiverRatchet(ratchetClassKey(), ratchetLeaf, ^uint64(0), 8); !errors.Is(err, ErrLadderWalkTooLong) {
		t.Errorf("a receiver at the largest index a u64 holds answered %v, want ErrLadderWalkTooLong", err)
	}
	// and the bound is not zero: a resume AT it is answered, so the refusal is a ceiling rather
	// than a ban.
	if _, err := NewReceiverRatchet(ratchetClassKey(), ratchetLeaf, 4, 8); err != nil {
		t.Errorf("a receiver four rungs along was refused: %v", err)
	}

	resuming := newStreamIndexMemory()
	resuming.image[streamIndexRowKey(ratchetGroup)] = uint64(maxLadderWalk) + 1
	if _, err := NewSenderRatchet(ratchetClassKey(), ratchetLeaf, ratchetGroup, resuming); !errors.Is(err, ErrLadderWalkTooLong) {
		t.Errorf("a sender resuming past the bound answered %v, want ErrLadderWalkTooLong", err)
	}
	short := newStreamIndexMemory()
	short.image[streamIndexRowKey(ratchetGroup)] = 4
	if _, err := NewSenderRatchet(ratchetClassKey(), ratchetLeaf, ratchetGroup, short); err != nil {
		t.Errorf("a sender resuming four rungs along was refused: %v", err)
	}
}

// ---------------------------------------------------------------------------
// the eviction policy, stated as the property that is true
// ---------------------------------------------------------------------------

// The claim used to be "a member holding a handful of skipped keys never pays for a member holding
// a thousand", and it is FALSE: evicting from the fullest EQUALISES, so once the bound is exceeded
// every holder above bound/senders loses rungs. Measured at the shipped defaults, where the table
// bound and one window are the same number: three honest senders, two holding four hundred skipped
// rungs each and a third taking a full window reorder, left the two at 341.
//
// The property that IS true is the fair share, and it takes THREE senders to state: a member
// holding fewer than retainedBound/senders never loses a rung at all. Batch B's own case used two
// senders with a bound of 12 and a window of 64, where bound/n = 6 exceeded the quiet member's
// holding of 2 and the property was unfalsifiable.
func TestAMemberBelowItsFairShareNeverLosesARungToAnotherMembersFlood(t *testing.T) {
	const bound = 90
	const window = 64
	table, err := NewReceiverRatchets(bound)
	if err != nil {
		t.Fatalf("build the table: %v", err)
	}
	classKey := ratchetClassKey()
	keys := []ReceiverRatchetKey{}
	for i := range 3 {
		key := ReceiverRatchetKey{RetentionWire: 0x01}
		key.SenderHandle[0] = byte(i + 1)
		ratchet, err := NewReceiverRatchet(classKey, uint32(i), 0, window)
		if err != nil {
			t.Fatalf("build ratchet %d: %v", i, err)
		}
		table.Track(key, ratchet)
		keys = append(keys, key)
	}
	// the fair share, DERIVED from the bound and the number of senders rather than written down.
	fairShare := bound / len(keys)
	quiet := fairShare - 2
	if quiet <= 0 {
		t.Fatalf("the fair share is %d, so there is no holding below it and this case cannot fail", fairShare)
	}
	// two quiet members, each holding fewer than its share, and one flooder taking a full window.
	for _, key := range keys[:2] {
		if _, err := table.KeyFor(key, uint64(quiet)); err != nil {
			t.Fatalf("the quiet member could not skip: %v", err)
		}
	}
	held := map[ReceiverRatchetKey][]uint64{}
	for _, key := range keys[:2] {
		for index := range uint64(quiet) {
			held[key] = append(held[key], index)
		}
	}
	if _, err := table.KeyFor(keys[2], uint64(window)); err != nil {
		t.Fatalf("the flooding member could not skip a full window: %v", err)
	}
	if table.Retained() > bound {
		t.Fatalf("the table holds %d retained rungs and the bound is %d", table.Retained(), bound)
	}
	for i, key := range keys[:2] {
		for _, index := range held[key] {
			answer, err := table.KeyFor(key, index)
			if err != nil {
				t.Errorf("quiet member %d held %d rungs, fewer than its fair share of %d, and lost index %d to another member's flood: %v",
					i, quiet, fairShare, index, err)
				break
			}
			zeroize(answer)
		}
	}
}

// The tie between two equally full windows is broken by the order they were TRACKED in, and Track
// is what stamps that order.
//
// Both mutations batch B's review found surviving are refused here: inverting the comparison, and
// deleting the stamp so the victim is decided by go's randomised map iteration. The second is what
// makes this a case rather than a comment -- a tie broken by iteration order passes on most runs.
func TestTheEvictionTieIsBrokenByTheOrderTheRatchetsWereTracked(t *testing.T) {
	const bound = 7
	const window = 16
	table, err := NewReceiverRatchets(bound)
	if err != nil {
		t.Fatalf("build the table: %v", err)
	}
	classKey := ratchetClassKey()
	first := ReceiverRatchetKey{RetentionWire: 0x01}
	first.SenderHandle[0] = 0xA1
	second := ReceiverRatchetKey{RetentionWire: 0x01}
	second.SenderHandle[0] = 0xB2
	for _, key := range []ReceiverRatchetKey{first, second} {
		ratchet, err := NewReceiverRatchet(classKey, 1, 0, window)
		if err != nil {
			t.Fatalf("build a ratchet: %v", err)
		}
		table.Track(key, ratchet)
	}
	// each holds four skipped rungs, which is a tie the bound of seven forces a decision about.
	for _, key := range []ReceiverRatchetKey{first, second} {
		answer, err := table.KeyFor(key, 4)
		if err != nil {
			t.Fatalf("skip to 4: %v", err)
		}
		zeroize(answer)
	}
	if table.Retained() != bound {
		t.Fatalf("the table holds %d retained rungs, want exactly the bound of %d so that one eviction has happened and no more", table.Retained(), bound)
	}
	held := func(key ReceiverRatchetKey) int {
		table.tableLock.Lock()
		defer table.tableLock.Unlock()
		return table.ratchets[key].Retained()
	}
	// the EARLIER tracked of two equally full windows is the one that gives up a rung. It is an
	// order somebody stated, not an order the runtime chose.
	if held(first) != 3 || held(second) != 4 {
		t.Errorf("the earlier tracked ratchet holds %d and the later holds %d; the tie is broken by the tracking order, and a table that decided it by map iteration would answer differently between runs",
			held(first), held(second))
	}
	// and the stamps are distinct and ascending, which is what Track owes.
	table.tableLock.Lock()
	firstStamp, secondStamp := table.ratchets[first].tracked, table.ratchets[second].tracked
	table.tableLock.Unlock()
	if firstStamp == 0 || secondStamp == 0 || firstStamp >= secondStamp {
		t.Errorf("the tracking stamps are %d and %d; without an ascending stamp per ratchet the tie is decided by go's randomised map iteration, which is exactly what the stamp exists to remove",
			firstStamp, secondStamp)
	}
}

// ---------------------------------------------------------------------------
// the two erasures that were in uncovered branches
// ---------------------------------------------------------------------------

// eraseLocked's header names both erasures that are NOT it, and one of them -- the duplicate index
// arm of retainIntoWindowLocked -- is reachable only by calling the helper twice at one index. It
// is a defect in this ratchet rather than a state a peer can cause, so nothing through the public
// surface gets there, and deleting the branch survived the whole suite.
func TestRetainingOneIndexTwiceErasesTheWholeWindow(t *testing.T) {
	ratchet, err := NewReceiverRatchet(ratchetClassKey(), ratchetLeaf, 0, 8)
	if err != nil {
		t.Fatalf("build the ratchet: %v", err)
	}
	ratchet.stateLock.Lock()
	defer ratchet.stateLock.Unlock()
	first := bytes.Repeat([]byte{0x11}, 32)
	other := bytes.Repeat([]byte{0x22}, 32)
	ratchet.retainLocked(1, first)
	ratchet.retainLocked(2, other)
	if len(ratchet.window) != 2 {
		t.Fatalf("the window holds %d rungs, want 2", len(ratchet.window))
	}
	ratchet.retainLocked(1, bytes.Repeat([]byte{0x33}, 32))
	if len(ratchet.window) != 1 {
		t.Errorf("the window holds %d rungs after one index was retained twice; what it held is no longer a set of rungs this ratchet can account for, so all of it goes",
			len(ratchet.window))
	}
	for _, dropped := range [][]byte{first, other} {
		for _, octet := range dropped {
			if octet != 0 {
				t.Errorf("a dropped rung was not erased: %x", dropped)
				break
			}
		}
	}
}

// KeyFor's exhausted arm erases the chain array, and it is reachable only by parking a ratchet at
// the last index a u64 holds -- which the walk bound makes unreachable through the constructor, by
// design. The field is set directly here because that is the only way in, and the alternative is
// an erase in production source that nothing holds.
func TestTheLastRungOfTheLadderErasesTheChainArray(t *testing.T) {
	ratchet, err := NewReceiverRatchet(ratchetClassKey(), ratchetLeaf, 0, 8)
	if err != nil {
		t.Fatalf("build the ratchet: %v", err)
	}
	ratchet.stateLock.Lock()
	ratchet.head = ^uint64(0)
	chain := ratchet.secret
	ratchet.stateLock.Unlock()
	if len(chain) == 0 {
		t.Fatal("the ratchet holds no chain array, so this case would pass over one that holds nothing")
	}
	answer, err := ratchet.KeyFor(^uint64(0))
	if err != nil {
		t.Fatalf("the last index answered %v", err)
	}
	if len(answer) == 0 {
		t.Fatal("the last index answered no key")
	}
	zeroize(answer)
	for _, octet := range chain {
		if octet != 0 {
			t.Errorf("the chain array was not erased at the end of the ladder: %x", chain)
			break
		}
	}
	// and the same index is now BELOW the head rather than at it, so a second request is refused
	// rather than answered from a chain array that is all zeros.
	if _, err := ratchet.KeyFor(^uint64(0)); !errors.Is(err, ErrOutOfWindow) {
		t.Errorf("a second request for the last index answered %v, want ErrOutOfWindow", err)
	}
}

// ---------------------------------------------------------------------------
// the reserver gate reads assignability rather than method declarations
// ---------------------------------------------------------------------------

// Batch B's review probed the gate that keeps a durable reserver out of this package and found it
// derives its class from METHOD DECLARATIONS: a production type satisfying StreamIndexReserver by
// EMBEDDING it -- a decorator, a cache, a nop reserver -- is invisible to that reading, and the
// probe survived the whole suite.
//
// THIS ASKS GO/TYPES INSTEAD OF READING SHAPES, and that is the repair rather than a third shape
// added to a list of two. A gate that read "declares the methods" and then also read "embeds the
// interface" would be two enumerated shapes with a third waiting -- a named type whose underlying
// type is a function... a struct embedding a struct that embeds the interface... a defined type
// over a pointer to one. Assignability is the property; every shape is a way of having it, and
// types.Implements answers the property.
//
// The scope question (R3a), answered separately from the class question: the SCOPE is this
// package's production files, type checked as the package they are, because a durable reserver
// arriving here is what section 8.2's assignment to sdk forbids and a type in another package is
// that package's own question. The CLASS is every package level named type the checker reports,
// which is total by construction -- the reading IS the class -- and it fatals if the checker
// reports none.
func TestNoProductionTypeOfThisPackageSatisfiesTheReserver(t *testing.T) {
	fileSet := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read this package's directory: %v", err)
	}
	files := []*ast.File{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fileSet, name, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, parsed)
	}
	if len(files) == 0 {
		t.Fatal("no production file was read, so this gate type checked nothing")
	}
	config := types.Config{Importer: importer.ForCompiler(fileSet, "source", nil)}
	checked, err := config.Check("github.com/urnetwork/connect/messagegroup", fileSet, files, nil)
	if err != nil {
		t.Fatalf("type check this package's production source: %v", err)
	}
	reserverObject := checked.Scope().Lookup("StreamIndexReserver")
	if reserverObject == nil {
		t.Fatal("this package declares no StreamIndexReserver, so this gate is holding nothing")
	}
	reserver, isInterface := reserverObject.Type().Underlying().(*types.Interface)
	if !isInterface {
		t.Fatal("StreamIndexReserver is not an interface")
	}
	if reserver.NumMethods() == 0 {
		t.Fatal("StreamIndexReserver declares no method, so every type in this package satisfies it and this gate would report the whole package")
	}
	judged, satisfying := 0, []string{}
	for _, name := range checked.Scope().Names() {
		named, isNamed := checked.Scope().Lookup(name).(*types.TypeName)
		if !isNamed {
			continue
		}
		judged += 1
		subject := named.Type()
		if types.Implements(subject, reserver) || types.Implements(types.NewPointer(subject), reserver) {
			satisfying = append(satisfying, name)
		}
	}
	if judged == 0 {
		t.Fatal("the checker reported no named type in this package, so this gate examined nothing")
	}
	for _, name := range satisfying {
		if name == "StreamIndexReserver" {
			continue
		}
		t.Errorf("%s satisfies StreamIndexReserver in production source; section 8.2 assigns the durable store to sdk's MessageStore, and section 5.6's argument rests on neither half of the record layer implementing one",
			name)
	}
	// and the reading is not vacuous: the interface satisfies itself, which is the one member
	// this class must always have.
	if !slices.Contains(satisfying, "StreamIndexReserver") {
		t.Errorf("types.Implements does not read StreamIndexReserver as satisfying itself, so whatever it is comparing is not the interface and every negative above is a negative about nothing")
	}
	t.Logf("%d named type(s) type checked, %d satisfying the reserver: %v", judged, len(satisfying), satisfying)
}

// ---------------------------------------------------------------------------
// what ruling A1 costs, measured on this tree rather than quoted from the ledger
// ---------------------------------------------------------------------------

// A CLASS'S USABLE OUT-OF-ORDER WINDOW IS THE WINDOW DIVIDED BY THE NUMBER OF CLASSES SHARING THE
// COUNTER, and it is counted here rather than asserted at a number.
//
// The receiver window is refused by DISTANCE in stream index, and under ruling A1 a class's own
// records are spread across every position its siblings also take. So a window of 1,024 positions
// no longer holds 1,024 of a class's records; it holds about 1,024/k of them. That is a price of
// the ruling and not a defect, and it is here so that a later change to either bound moves a
// number somebody has to look at.
//
// The count is DERIVED: the case round-robins k ladders over one counter and asks the receiver
// how far it can still reach, so the answer follows from DefaultRecordWindowSize and k rather than
// from a constant written down beside them.
func TestASharedCounterDividesAClassesOutOfOrderWindowByTheClassCount(t *testing.T) {
	reserver := newStreamIndexMemory()
	classKeys := DeriveClassKeys(StorageRoot(keyScheduleKatInputs()))
	stream := streamKeyNamed("every class key, one counter")
	// k IS THE NUMBER OF CLASS KEYS AND IS READ OFF ClassKeys, not written down beside a
	// hard-coded 3. A fourth FIELD there would make the divisor 4 here without anybody
	// remembering to change it, and a case that went on dividing by 3 would report a cost
	// that is not the one being paid. Ledger item 152's ruling of 2026-09-13 put the eph
	// classes on this root and did NOT add a field: eph_root is not derived from storage_root
	// (MASTER invariant I4) and EphKey takes it as an argument, so ClassKeys still has three
	// and the divisor is still read rather than typed.
	ladderKeys := reflect.ValueOf(*classKeys)
	if ladderKeys.NumField() < 2 {
		t.Fatalf("ClassKeys declares %d field(s), so nothing here could share a counter", ladderKeys.NumField())
	}
	watched, err := NewSenderRatchet(classKeys.Durable, ratchetLeaf, stream, reserver)
	if err != nil {
		t.Fatalf("the watched ratchet: %v", err)
	}
	siblings := []*SenderRatchet{}
	for i := range ladderKeys.NumField() {
		classKey, isOctets := ladderKeys.Field(i).Interface().([]byte)
		if !isOctets {
			t.Fatalf("ClassKeys.%s is not a class key, so this case cannot count the ladders",
				ladderKeys.Type().Field(i).Name)
		}
		if bytes.Equal(classKey, classKeys.Durable) {
			// the watched ladder is already built; every other field is a sibling.
			continue
		}
		sibling, err := NewSenderRatchet(classKey, ratchetLeaf, stream, reserver)
		if err != nil {
			t.Fatalf("the sibling for ClassKeys.%s: %v", ladderKeys.Type().Field(i).Name, err)
		}
		siblings = append(siblings, sibling)
	}
	ladders := len(siblings) + 1
	if ladders != ladderKeys.NumField() {
		t.Fatalf("ClassKeys declares %d class keys and this case built %d ladders", ladderKeys.NumField(), ladders)
	}
	head, headKey, err := watched.Next()
	if err != nil {
		t.Fatalf("the first watched record: %v", err)
	}
	zeroize(headKey)
	receiver, err := NewReceiverRatchet(classKeys.Durable, ratchetLeaf, head, DefaultRecordWindowSize)
	if err != nil {
		t.Fatalf("the receiver: %v", err)
	}
	// PeekFor and not KeyFor, so the head does not move: what is being counted is how far one
	// window reaches from ONE head, which is what a receiver holding a gap actually has.
	reachable := 0
	for sent := 0; sent < DefaultRecordWindowSize; sent += 1 {
		for _, sibling := range siblings {
			_, siblingKey, err := sibling.Next()
			if err != nil {
				t.Fatalf("a sibling could not send: %v", err)
			}
			zeroize(siblingKey)
		}
		index, key, err := watched.Next()
		if err != nil {
			t.Fatalf("the watched ladder could not send: %v", err)
		}
		zeroize(key)
		if _, err := receiver.PeekFor(index); err != nil {
			if !errors.Is(err, ErrOutOfWindow) {
				t.Fatalf("the receiver answered %v at index %d, want ErrOutOfWindow or a key", err, index)
			}
			break
		}
		reachable += 1
	}
	// the shape of the answer rather than the answer: about one window divided by the ladders
	// sharing the counter, and unambiguously not a whole window.
	want := DefaultRecordWindowSize / ladders
	if reachable < want-ladders || want+ladders < reachable {
		t.Errorf("a receiver reached %d of the watched class's records from one head; %d ladders sharing one counter put about %d of them inside a %d position window",
			reachable, ladders, want, DefaultRecordWindowSize)
	}
	if DefaultRecordWindowSize <= reachable {
		t.Errorf("a receiver reached %d records, which is a whole window; under one shared counter a class does not get a whole window and a case that saw one is not measuring A1",
			reachable)
	}
	t.Logf("window %d positions, %d ladders on one counter: %d of the watched class's own records reachable from one head",
		DefaultRecordWindowSize, ladders, reachable)
}

// The unit every cost in this package's comments is quoted in: one rung of the ladder.
//
// It is a benchmark rather than a constant because the only honest derivation of a cpu bound is a
// measurement, which is the argument maxLadderWalk's own comment makes.
func BenchmarkSenderLadderRung(b *testing.B) {
	recordKey := RecordKeyZero(ratchetClassKey(), ratchetLeaf)
	b.ResetTimer()
	for range b.N {
		recordKey = stepRecordKey(recordKey)
	}
	b.StopTimer()
	zeroize(recordKey)
}

// What ruling A1 costs at an epoch change, which is the one cost the ruling was taken knowing.
//
// installEpochOnLoop drops every sender ratchet at a commit -- they hold the previous epoch's
// rungs, which is what forward secrecy is about -- and each one is rebuilt by walking from
// record_key[0] to the store's high water. Under per-class counters a sender that had sent P
// records across k classes had k counters at about P/k each, so the rebuild was P rungs in total.
// Under A1 there is ONE counter at P, and every one of the k ladders walks all of it: k x P, plus
// the P the epoch's own sends walk, which is the (k+1) x P the ledger prices.
//
// The two shapes are benchmarked side by side so the ratio is measured rather than asserted, and
// the last shape is the wall: a stream one rung past maxLadderWalk cannot be resumed at all.
//
// MEASURED 2026-09-07, Intel Core Ultra 9 275HX, windows/amd64, -benchtime 1x:
//
//	one rung (BenchmarkSenderLadderRung, 2e6 iterations)              417.7 ns
//	per-class counters, k=3, P=100,000 (100,000 rungs)                41.7 ms
//	one shared counter, k=3, P=100,000 (300,000 rungs)               131.5 ms
//	one shared counter, k=3, P=maxLadderWalk-1 (3,145,725 rungs)       1.21 s
//
// AND THE MULTIPLIER IS k, NOT k+1, which is recorded because the ledger prices this at
// (k+1) x P. The rebuild is k ladders each walking the whole shared counter and that is all it
// is: 3 x 100,000 rungs, measured at 131.5 ms against 41.7 ms for the P rungs the per-class
// shape paid. The ledger's 148 ms and 1.55 s are within a fifth of these because it quoted
// 368.7 ns a rung against the 417.7 ns this machine measures, so the ARITHMETIC agreed by
// accident where the FORMULA does not. The extra P the ledger counts is real work, but it is the
// epoch's own sends walking their gaps rather than anything installEpochOnLoop does -- and that
// half is k x P as well, for the same reason, so the honest per-epoch total is 2k x P against
// the 2P a per-class counter paid.
func BenchmarkEpochChangeRebuild(b *testing.B) {
	classKeys := DeriveClassKeys(StorageRoot(keyScheduleKatInputs()))
	ladderKeys := [][]byte{classKeys.Durable, classKeys.Perm, classKeys.Media}
	for _, shape := range []struct {
		name      string
		highWater uint64
	}{
		{name: "per-class counters, k=3, P=100000", highWater: 100000/3 - 1},
		{name: "one shared counter A1, k=3, P=100000", highWater: 100000 - 1},
		{name: "one shared counter A1, k=3, P=maxLadderWalk-1", highWater: maxLadderWalk - 2},
	} {
		b.Run(shape.name, func(b *testing.B) {
			stream := streamKeyNamed("a rebuilt sender")
			reserver := newStreamIndexMemory()
			reserver.image[streamIndexRowKey(stream)] = shape.highWater
			b.ResetTimer()
			for range b.N {
				for _, classKey := range ladderKeys {
					ratchet, err := NewSenderRatchet(classKey, ratchetLeaf, stream, reserver)
					if err != nil {
						b.Fatalf("rebuild: %v", err)
					}
					ratchet.Zeroize()
				}
			}
		})
	}
}
