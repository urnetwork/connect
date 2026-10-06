// The stream index reservation: the durability the record aead's nonce uniqueness rests on,
// declared here as an interface and implemented nowhere in this package.
//
// Spec A section 5.6, quoted whole because its two halves are usually collapsed into one:
//
//	stream_index is a single u64 counter per (group_id, sender_handle), write-once, assigned
//	locally. A device MUST durably record "index k consumed" BEFORE encrypting, and MUST NEVER
//	encrypt a second record at a consumed index. The server enforces monotonicity, not
//	contiguity, so a refused write, a crash between reserve and send, or a lost commit leaves a
//	legal gap.
//
//	Nonce reuse under a repeated record_key is a total break of both AEADs for that record,
//	which is why the reservation is durable rather than best-effort.
//
//	SealRecord calls Reserve and refuses to proceed on error. On startup, HighWater is read and
//	the ratchet resumes at highWater + 1, never at a recomputed value.
//
// WHY THERE IS NO IMPLEMENTATION HERE, so that a reader who finds none finds the reason instead
// of writing one. Three facts and together they are the argument.
//
// Neither half of the record layer imports an I/O package at all. Measured over the production
// files of connect/message and connect/messagegroup after m1 wave 1, the whole import set is the
// standard library's crypto, encoding/binary, errors, fmt, io, sync, mls, mls/syntax and -- from
// the client half onto the server-safe one, in that direction only -- connect/message; the
// heaviest is io, for an io.Reader parameter. Adding a file format here would make the client half a storage
// engine, and it is a RECORD layer with a group attached. connect/mls's import gate holds that
// as a test rather than as a sentence: every production import of this directory is pinned in
// mls's own suite, so an os arriving here fails a test over there on the commit that adds it.
//
// Section 8.2 already assigns the persistence, to sdk. MessageStore declares
// ReserveStreamIndex(groupId []byte, index uint64) error and StreamHighWater(groupId []byte)
// (uint64, error) -- this interface's subject, on the fourteen method interface the sqlite
// implementation already owes. A second durable implementation here is the second
// implementation of one thing, which is the shape this plan's first paragraph forbids.
//
// And section 5.6 injects the sink for exactly this reason -- "the constructor takes the sink to
// make it explicit" -- which is why NewSenderRatchet takes one and refuses a nil.
//
// So what ships here is the interface, the two sentinels and the CONTRACT below. The durable
// implementation is sdk's, its plan is unwritten, and every obligation stated here is one that
// plan inherits. A CP3b run over this package's test fake proves the record layer and not the
// client.
//
// THE KEYING QUESTION IS NOT ANSWERED HERE AND MUST NOT BE ANSWERED HERE. Section 5.6's first
// sentence says the counter is per (group_id, sender_handle); the interface it then declares
// takes groupId and NOT senderHandle, in both methods, and so does section 8.2's MessageStore.
// sender_handle is a function of the LEAF (MASTER section 8) and group_handle_key is fixed at
// group creation, so a device removed and re-added at a different leaf has a DIFFERENT
// sender_handle in the SAME group. A reserver keyed on group_id alone either hands the new
// handle the old leaf's high water -- burning indices, benign -- or, on any local state
// divergence, lets a fresh handle start at 1 while a stale row says otherwise. That is open item
// M1-5, it is the highest priority of the non blocking items because this is the one piece of
// durable on-disk state that cannot be migrated by recomputation. Implement the ruling; do not
// choose it in this file.
//
// THE KEY IS (group_id, sender_handle) AND IT IS CLASS BLIND. That is the owner's ruling of
// 2026-09-07 on ledger items 143 and 169 together -- shape A1 -- and the reason the argument is
// recorded here rather than only in the ledger is that it is CHECKABLE. A1 is the counter the
// rest of the system already declares, and the client was the only half that disagreed:
//
//	spec B's schema, message_sender          PRIMARY KEY (group_id, sender_handle)
//	spec B's Q7, run on every submit         WHERE group_id = $1 AND sender_handle = $2
//	the shipped server, msgrepo's store      "per (group_id, sender_handle)", keyed on the
//	                                         record's SenderHandle alone
//	what m1 wave 1 shipped here              StreamKey{GroupId, SenderHandle, RetentionWire}
//
// So the retention byte this file used to carry was not a divergence the documents had left
// open: it was a client/server split, and the server would have REFUSED the second class's
// first record -- REASON_STREAM_INDEX_REGRESSED, because a second class starting again at 1 is a
// stream index that went backwards from the only counter the server keeps. Dropping the byte
// closes that split, costs zero wire octets, breaks nothing already sealed (every record wave 1
// sealed came from a sender using one class, whose class blind counter is identical), keeps
// i = stream_index in every ladder, and owes no change to spec B.
//
// WHAT THAT COSTS, stated because it is real and because the ledger's numbers were taken on
// paper. One counter shared by k ladders makes every ladder SPARSE: a class sends at positions
// it does not choose and walks the gaps its siblings made. Measured on this tree by the
// benchmarks in ratchetrepairs_test.go, the two consequences are an epoch change rebuild that
// costs (k+1) x P expansions where it cost P, and a usable out of order window that falls from
// 1,024 of a class's own records to about 1,024/k shared positions. Both are prices of the
// ruling and neither is a defect of it.
//
// AND WHAT IT DOES NOT ANSWER, which is the part a later reader will be tempted to close in
// passing. Section 5.6 makes EPH(bucket 0) transients consume an index locally, so every typing
// indicator advances this one counter -- and a receiver's window is refused by DISTANCE, so
// 1,025 transients between two DURABLE records make the second permanently out_of_window. That
// is open item M1-25 and it is NOT ruled here. The hazard is executable rather than asserted:
// TestTransientsOnTheSharedCounterStarveADurableReceiverWindow drives it. Nothing here
// forecloses a separate transient counter and nothing here grants one; giving transients their
// own counter re-opens this very collision for EPH heads, which ledger item 152 ruled onto this
// root on 2026-09-13 -- so that day has arrived and the hazard is live rather than prospective.
package messagegroup

// StreamIndexReserver is the durable sink a sender ratchet allocates its stream indices from.
//
// RESERVE ALLOCATES, IT DOES NOT ASSERT, AND THE CHANGE OF SHAPE IS WHAT MAKES A1 SAFE. Wave 1
// shipped Reserve(stream, index) error -- the caller chose the number and the store said yes or
// no -- and every SenderRatchet kept its own position field to choose it from. That works while
// each ladder owns a counter and WEDGES PERMANENTLY the moment two ladders share one: the second
// ladder offers index 1, the store answers ErrStreamIndexConsumed, and because the refusal is
// permanent the ratchet stops. Ledger item 168 measured exactly that wedge. A1 puts two ladders
// on one counter by construction, so under A1 the assert shape is not merely awkward, it is
// unusable.
//
// So the counter is the STORE'S and there is no second copy of it. A ladder cannot ask for an
// index, therefore it cannot ask for a consumed one; two ladders sharing a stream get two
// different numbers because exactly one place hands numbers out. The alternative shape -- the
// session owns the counter and passes the index into the ratchet -- was not taken, for three
// reasons. It moves the reserve before derive ordering out of Next, where seal_test.go's call
// graph gate can see it, and back into a convention at every caller that is not the session. It
// leaves Next as a second door onto the same ladder that still chooses its own number, so the
// wedge is unreachable only while nobody uses that door. And the store that owes the persistence
// cannot implement it atomically: a read, then a caller's decision, then a write with an fsync
// in it is a window that an allocation done in one statement does not have.
//
// THE CONTRACT, which is the whole of what this file ships. An implementation owes all five,
// and streamindex_test.go holds a file backed fake to every one of them so that the properties
// are executable here rather than deferred to a package that does not exist yet.
//
//  1. Reserve returns only after the reservation SURVIVES A PROCESS DEATH. Not after the write
//     is issued, not after it is buffered: after it is durable. A Reserve that returned before
//     the flush is a nonce reuse machine that passes every round trip test there is, because
//     the reused value is still well formed and the record still opens against itself.
//  2. HighWater never rewinds. After a restart it is at least what it was, for every key, under
//     every interleaving. A persisted state behind an index already handed out is
//     ErrStreamIndexRewound.
//  3. NO INDEX IS EVER HANDED OUT TWICE, for the life of the stream and across every restart.
//     Under the assert shape this clause read "a consumed index is refused"; under allocation
//     the caller has nothing to repeat, so the obligation lands where the counter now is. What
//     ErrStreamIndexConsumed names is the store's PERMANENT refusal to allocate -- the next
//     position is one it has already handed out and it has no way past it, which is what a row
//     that went backwards under a live process looks like from in here, and what a stream that
//     has spent the last index a u64 holds looks like forever. A typed fatal error per section
//     5.9 G7, never a bool and never a log line.
//  4. The store is TOTAL over its key space. A stream never seen answers HighWater 0 with no
//     error, so the first allocation is 1; section 5.1 makes record_id = 0 the "from the
//     beginning" cursor by the same reasoning and the two must not disagree in shape.
//  5. Reserve is not idempotent, and under allocation that is structural rather than a refusal.
//     Two calls are two indices. There is no call that answers an index a previous call
//     answered, because "I already have that one" is not a sentence this interface can be told:
//     the only thing a caller can say is "I am about to encrypt", and every one of those is a
//     fresh position. Clause 5 as wave 1 wrote it -- "reserving an index a second time is
//     condition 3 and not a no-op" -- described a call that no longer exists.
//
// The cost this interface hands its implementer, filed rather than absorbed: section 5.6 makes
// EPH(bucket 0) transients consume an index locally so the counter is never rewound, which
// makes every typing indicator a synchronous flush and the transient send rate the fsync rate.
// That is open item M1-25, and under A1 it carries a second half the file comment above states:
// the transients share the one counter, so they also spend the receiver's out of order window.
// Nothing here forecloses a separate transient counter, and an implementation that wants one
// adds a key rather than changing a method -- but see the file comment for the collision it
// would re-open for EPH heads, which ledger item 152 ruled onto this root on 2026-09-13.
type StreamIndexReserver interface {
	// Reserve allocates the next stream index of this stream, records that this device is
	// about to encrypt at it, and returns it only after that record is durable. The error is
	// fatal to the seal: SealRecord refuses to proceed.
	Reserve(stream StreamKey) (uint64, error)
	// HighWater is the highest index this store has ever allocated for the stream, or 0 for a
	// stream it has never seen. The ratchet resumes at highWater + 1 and never at a
	// recomputed value.
	HighWater(stream StreamKey) (uint64, error)
}

// StreamKey is the stream one reservation belongs to: the group and the sender, which is what
// spec A section 5.6, spec B's schema, spec B's Q7 and the shipped message server all key the
// counter by.
//
// IT CARRIES NO RETENTION CLASS, AND THAT IS THE OWNER'S RULING RATHER THAN THIS FILE'S CHOICE.
// Wave 1 shipped a third field here -- the retention class wire byte -- to close a permanent
// wedge that was real: a sender ratchet is per class key, so two classes of one group reserving
// out of one counter under the ASSERT shape left the second refused forever at its first index.
// That repair was right about the wedge and wrong about where the fix goes. It made this client
// the only party in the system counting per class, so the first record of a second class would
// have been refused by the server as a stream index regression, and it owed spec B a schema
// change nobody had agreed to. Ruling A1 keeps the key class blind and repairs the wedge in the
// INTERFACE instead: Reserve allocates, so no ladder ever offers an index another ladder has
// taken. See StreamIndexReserver above for the shape and the file comment for the ruling.
//
// What is lost with the byte, said plainly rather than left for a reader to rediscover: a class
// no longer owns a contiguous run of indices, so every ladder is sparse and pays for the gaps
// its siblings make. The file comment prices that.
//
// Open item M1-5 is still what rules the keying of a store ROW. What this type fixes is which
// stream a RESERVATION belongs to; M1-5 decides how a row for it is identified and migrated, and
// the two questions have been confused once already.
//
// It is a comparable struct with no slice in it, and that is deliberate twice over: a stream key
// can be a map key without a second encoding, and a group id that moved under a ratchet cannot
// reserve indices against one row and use them against another.
type StreamKey struct {
	GroupId      [32]byte
	SenderHandle [16]byte
}
