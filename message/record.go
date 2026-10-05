// The record as the store holds it, and the two ladders every layer above reads its
// body sizes and its lifetimes out of.
//
// Master section 8 and spec A section 5.1 define the record; this file is the go shape
// of it and nothing more. The key schedule, the codec and the authenticators land
// beside it and read these types, and spec A section 5.2 makes the construction order
// a type rather than a convention, so nothing here should ever grow a public
// constructor for a record: the sealer and the parser own those fields.
//
// The one thing to know before editing. The retention class and the eph bucket are two
// fields in go and a single byte on the wire, and RetentionClassOf and RetentionClassWire
// at the bottom of this file are the only two functions in the entire system that cross
// between those two shapes. Their comments say why. A gate in record_test.go parses this
// package, connect/mls and the sdk repository beside connect, and reports every
// expression that would quietly rebuild either half of that crossing somewhere else.
// record.go is the one path it exempts — the path, not the base name, so a record.go
// arriving in another package is gated like anything else.
package message

import "fmt"

// The retention class of a record, as go carries it. It is a tag, not the wire byte:
// the wire joins it with the eph bucket, and the two are only ever crossed by the pair
// of functions at the bottom of this file.
//
// The class decides which key the body was sealed under (master section 8.1) and how
// the server prunes it (master section 12.2), so it is authenticated in both aads and
// in the write_auth preimage. A record whose class is read wrong is a record that
// cannot be opened at all.
type RetentionClass uint8

const (
	RetentionPermanent RetentionClass = 0
	RetentionDurable   RetentionClass = 1
	RetentionMedia     RetentionClass = 2
	// The eph classes collapse to this one go tag; which of the six windows a record
	// belongs to travels beside it in RecordHeader.EphBucket rather than inside the
	// class. Spec A section 5.1 is explicit about the reason and it is the whole
	// argument for this file's shape: a single u8 whose meaning depends on its own
	// high bits is exactly the field that gets compared with == somewhere and silently
	// treats eph bucket 1 as a different class from eph bucket 0.
	RetentionEph RetentionClass = 3
)

// What the split answers alongside an error. It is deliberately not one of the four
// legal classes: a caller that drops the error gets a value that every later check
// refuses, rather than a permanent record it will happily store forever.
const retentionClassInvalid RetentionClass = 0xFF

// The size ladder a body is padded to before it is sealed. Padding to a rung is what
// keeps the server from reading a message's length, so the rung is authenticated and
// the server checks the stored ciphertext length against it.
type SizeBucket uint8

const (
	SizeBucket256 SizeBucket = 0
	SizeBucket1K  SizeBucket = 1
	SizeBucket4K  SizeBucket = 2
	SizeBucket16K SizeBucket = 3
	SizeBucket64K SizeBucket = 4
	// The blob rung has no body length at all: the body lives in an object named by
	// RecordHeader.BlobId and ct_body is absent (spec A section 5.13).
	SizeBucketBlob SizeBucket = 5
)

// The authenticated header. Every field here is covered by aad_head and by write_auth
// (master invariants 6 and 8). RecordId is in neither — it is server assigned after
// acceptance and is pagination only.
type RecordHeader struct {
	GroupId      [32]byte
	SenderHandle [16]byte
	Epoch        uint64
	// Monotonic per (group_id, sender_handle) and write once. The server enforces
	// monotonicity, not contiguity, so a refused write leaves a legal gap.
	StreamIndex uint64
	// Set on an mls commit record. The server acts on this, which is why it is
	// authenticated rather than inferred from a body the server cannot read.
	IsCommit       bool
	RetentionClass RetentionClass
	// Meaningful only when the class is eph, and required to be zero otherwise: the
	// join refuses to encode a non eph class that carries a bucket, so a stray value
	// here is a refusal rather than a field that is silently dropped.
	EphBucket uint8
	// t, the time slice of this record's own K_eph[n][b][t] (master section 8.1).
	// PLAINTEXT and on the wire, unlike every other key input in the system.
	//
	// ALWAYS ENCODED. Zero on permanent, durable, media and eph bucket 0, non zero on
	// eph buckets 1 through 5 — the presence rule of master section 8, which follows
	// blob_id's precedent in substance and rejects it on its surface: a fixed width
	// field's analogue of a zero length term is a zero VALUED term, so no preimage
	// builder gains a conditional for it and every record carries the eight octets.
	//
	// THE SENDER COMPUTES IT, ONCE, as floor(sent_at_ms / (EphBucketSeconds(b) * 1000))
	// for b in 1..5, origin the unix epoch, off the same wall clock reading it puts in
	// sent_at. AN OPENER TAKES THIS VALUE AND NEVER RECOMPUTES IT: a window derived from
	// the opener's own clock would differ from the sender's on every record that crossed
	// a boundary, and the aead would be the only thing that said so. Ruled 2026-09-13
	// (ledger 152 / 183, m1 open item M1-27), which is also why the format version is
	// 0x02 — this field is an ADDITION to a wire format master section 14 froze.
	EphWindow  uint64
	SizeBucket SizeBucket
	// Unix milliseconds, 0 = unset. An advisory upper bound only: it may shorten
	// retention, never extend it.
	ExpireAt uint64
	// H(ct_body), retained after ct_body is erased, which is what lets a pruned record
	// still say what it carried.
	BodyHash [32]byte
	// Exactly 32 bytes iff the size bucket is the blob rung, else nil. Covered by
	// aad_head and by write_auth like every other header field, and derived from the
	// record's key material (spec A section 5.13), never from content.
	BlobId []byte
	// The only server visible structured field, nil or empty for an ordinary record
	// (spec A section 5.11). The preimage builders encode it as a zero length prefix
	// when it is absent, so there is no conditional in the preimage and no special
	// case for ordinary records.
	ServerAttachment []byte
}

// A record: the authenticated header, the two ciphertexts, and the mac computed last.
// The two ciphertexts use distinct keys and distinct aads (master invariant 7), and
// only the body is erasable.
type Record struct {
	// Server assigned, 1 based, 0 = unassigned. Per group and gapless, used for
	// pagination and hole detection only, and never authenticated: it appears in
	// neither aad, in neither preimage, and it is ignored on submit and populated on
	// read. A since_record_id of 0 is therefore the well defined "from the beginning"
	// cursor for an exclusive lower bound.
	RecordId  uint64
	Header    RecordHeader
	CtHead    []byte
	CtBody    []byte
	WriteAuth [32]byte
}

// The aead tag both ladders account for. Master section 8 requires
// octet_length(ct_body) to equal the rung's body length plus this, exactly.
const aeadTagBytes = 16

// Body bytes per rung, excluding the aead tag, for the five rungs that have a body.
// The blob rung is off the end of this array on purpose rather than holding a zero, so
// the lookup that answers "no length" is the bounds check itself rather than a value a
// reader has to recognise.
var sizeBucketBodyBytes = [...]int{256, 1024, 4096, 16384, 65536}

// Seconds per eph bucket.
//
// BUCKET 0 ANSWERS ZERO AND AN OFF LADDER BUCKET ANSWERS A NEGATIVE, and the two must not
// be one value. Ruled 2026-09-13, m1 open item M1-27, and the paragraph that rules it is
// restated character for character inside the wire block of master section 8, spec A
// section 5.1 and spec B section 3.1:
//
//	The bucket-0 answer and the off-ladder answer MUST DIFFER. EphBucketSeconds MUST
//	answer 0 for bucket 0 -- the true retention window of a rung that is never stored --
//	and a NEGATIVE for 6..255, which is not a bucket at all.
//
// Zero is not a sentinel here, it is the answer: bucket 0 is the transient rung, which is
// never persisted, so the length of time it is retained for is nought seconds. The
// negative is a programmer error marker for a value that is not a bucket at all, and it
// is unreachable through a parsed record because RetentionClassOf refuses every wire byte
// outside 0x10..0x15. That asymmetry -- one a real window a caller may act on, the other
// a value no record can carry -- is the whole of why they must not share an answer.
var ephBucketWindowSeconds = [...]int{ephTransientSeconds, 3600, 28800, 86400, 604800, 2419200}

// The retention window of the transient rung, in seconds.
//
// It is a named constant rather than a bare 0 in the table above so that the table reads
// as six windows rather than as five windows and a hole, and so that the ruling's own
// word for it is at the value. A caller that divides by this to get a window is the
// caller the ruling is about: bucket 0's window is 0 BY DEFINITION and is never computed
// (master section 8.1), so there is no division to do and a divisor of zero is what says
// so.
const ephTransientSeconds = 0

// What both ladders answer for a value that is not a rung of them at all.
//
// Negative rather than zero, because every plausible use of the answer -- make([]byte, n),
// a length equality check, now + n -- fails loudly on a negative and silently on a zero.
// Callers test for a negative, never for this exact number.
//
// IT NO LONGER COVERS TWO MEANINGS, and the paragraph it replaces argued that it should.
// That paragraph read: "One value because the two cases it covers, 'this rung has no
// inline body' and 'this is not a rung at all', mean the same thing to a caller: there is
// nothing here to size or to expire. Giving them separate negatives would invite a switch
// that handles one and falls through on the other." The owner ruled the other way on
// 2026-09-13 for the eph ladder, and the reason the argument failed there is that bucket 0
// IS a rung, with a real retention window of nought seconds, while bucket 6 is not a rung
// at all -- so the two do not mean the same thing to a caller, and a caller holding the
// shared answer could not ask which of them it had met. The size ladder is untouched: the
// blob rung genuinely has no inline body to size, which is the same thing as "not a rung
// of this ladder" for every use a caller has, and no ruling asks it to split.
const noLadderValue = -1

// Body bytes for a rung, excluding the 16 byte aead tag. Negative for the blob rung,
// which has no inline body, and for any value off the ladder.
func SizeBucketBytes(b SizeBucket) int {
	if len(sizeBucketBodyBytes) <= int(b) {
		return noLadderValue
	}
	return sizeBucketBodyBytes[b]
}

// Ciphertext bytes for a rung: the body plus the aead tag, which is the column spec B
// indexes and checks on. Derived from SizeBucketBytes rather than written out a second
// time, so the two cannot drift apart. Negative wherever SizeBucketBytes is negative,
// rather than a lone tag length that would read as a real ciphertext.
func SizeBucketCtBodyBytes(b SizeBucket) int {
	body := SizeBucketBytes(b)
	if body < 0 {
		return body
	}
	return body + aeadTagBytes
}

// EphBucketSeconds is the retention window of an eph bucket, in seconds.
//
// THREE ANSWERS AND NOT TWO. A positive for buckets 1 through 5, which is the rung's own
// window and the divisor of master section 8.1's eph_window arithmetic. ZERO for bucket 0,
// the transient rung: it is never persisted, so nought seconds is its true window and not
// a marker for the absence of one. A NEGATIVE for 6 through 255, which name no rung at
// all. Ruled 2026-09-13 (m1 open item M1-27); before that ruling bucket 0 and bucket 6
// both answered -1 and no caller could ask which of them it had met.
//
// THE SIGNATURE DOES NOT MOVE AND THAT IS DELIBERATE. Spec A section 12.1 and spec B
// section 12.1 both publish this name as "func EphBucketSeconds(bucket uint8) int",
// character for character, as one of the handful of symbols the message server links; a
// second return value here would be a divergence between two read only documents made by
// this package on its own authority. The distinction the ruling asks for is therefore
// carried in the VALUE, which is exactly what the ruling's own wording asks for, and what
// holds a caller to it is that the three answers are three different arithmetic
// situations rather than a flag: a caller that conflates zero with the negative either
// divides by zero on a bucket that is not a bucket or refuses the transient rung, and
// neither is a program that runs.
func EphBucketSeconds(bucket uint8) int {
	if len(ephBucketWindowSeconds) <= int(bucket) {
		return noLadderValue
	}
	return ephBucketWindowSeconds[bucket]
}

const (
	retentionWirePermanent byte = 0x00
	retentionWireDurable   byte = 0x01
	retentionWireMedia     byte = 0x02
	// The eph classes occupy 0x10 through 0x15: the base joined with the bucket.
	retentionWireEphBase byte  = 0x10
	ephBucketMax         uint8 = 5
	// What the join answers alongside an error. Not a legal wire byte, so a caller
	// that drops the error writes a record the split refuses, rather than the
	// permanent record that returning 0x00 here would quietly produce.
	retentionWireInvalid byte = 0xFF
)

// The three classes that carry no bucket, with the byte each takes on the wire. A table
// rather than a byte(class) conversion: the tag values and the wire values happen to
// agree for these three today, and a conversion would silently make that coincidence
// the encoding.
var nonEphWireBytes = map[RetentionClass]byte{
	RetentionPermanent: retentionWirePermanent,
	RetentionDurable:   retentionWireDurable,
	RetentionMedia:     retentionWireMedia,
}

// The wire byte table, restated from master section 8 and spec A section 5.1 because
// spec B section 3.1 restates it too and a divergence between the three makes every eph
// record fail both the aead and the mac:
//
//	0x00  permanent
//	0x01  durable
//	0x02  media
//	0x10 | bucket   eph(bucket), bucket in 0..5  ->  0x10, 0x11, 0x12, 0x13, 0x14, 0x15
//	                                                (decimal 16, 17, 18, 19, 20, 21)
//
// No other value is legal. 0x03 through 0x0f and 0x16 through 0xff are all errors.
//
// This function and RetentionClassWire are the only two in the system that split or
// join the class and the eph bucket, and the ban is mechanical: record_test.go reads the
// syntax tree of every go file under this package, connect/mls and sdk, and reports the
// expressions that would rebuild the join or the split elsewhere. The reason is not
// tidiness. A single u8 whose meaning depends on its own high bits is exactly the field
// that gets compared with == somewhere — in a prune query, in a key lookup, in a switch
// — and that comparison silently treats eph bucket 1 as a different class from eph
// bucket 0, so half the eph records fall out of a rule that reads as though it covered
// all of them. With the split confined here, every
// caller above holds a class it can compare and a bucket it can compare, and neither
// comparison can accidentally be about the other.
//
// The class returned alongside an error is not a legal class, so a caller that drops
// the error cannot carry on with a plausible looking one.
func RetentionClassOf(wire byte) (RetentionClass, uint8, error) {
	switch wire {
	case retentionWirePermanent:
		return RetentionPermanent, 0, nil
	case retentionWireDurable:
		return RetentionDurable, 0, nil
	case retentionWireMedia:
		return RetentionMedia, 0, nil
	}
	if bucket := wire - retentionWireEphBase; retentionWireEphBase <= wire && bucket <= ephBucketMax {
		return RetentionEph, bucket, nil
	}
	return retentionClassInvalid, 0, fmt.Errorf("%w: wire byte 0x%02x", ErrRetentionClassUnknown, wire)
}

// The join half of the pair above, and the whole of that comment applies here as well.
//
// It refuses rather than normalises, in both directions. A non eph class carrying a
// bucket is an error and not a 0x00 with the bucket dropped, because a caller that got
// here with a bucket believes the bucket means something, and dropping it would store
// the record as though the caller were right. An eph bucket past 5 is an error and not
// a truncation, because 0x16 is not a legal byte and manufacturing one would put a
// record on the wire that every reader, the sender's own other devices included,
// refuses.
//
// The byte returned alongside an error is not a legal wire byte, for the same reason
// the split's class is not a legal class.
//
// The signature returns an error where spec A section 12.1 first published a bare byte,
// and the error is what makes the two refusals above possible at all: a function that
// cannot refuse has to normalise, which is the silent mis-storage those two paragraphs
// exist to prevent. Master section 8 gives no go signature, so it did not settle it.
// Both section 12.1 blocks — spec A's and the character for character copy spec B
// restates — now publish this arity, as amendments A-8 and B-8 of 2026-08-25. It is an
// arity and not a spelling, so a server written against the old block does not compile
// rather than misbehaving, which is why it was worth amending rather than working around.
func RetentionClassWire(class RetentionClass, ephBucket uint8) (byte, error) {
	if class == RetentionEph {
		if ephBucketMax < ephBucket {
			return retentionWireInvalid, fmt.Errorf("%w: eph bucket %d, want 0..%d", ErrEphBucketOutOfRange, ephBucket, ephBucketMax)
		}
		return retentionWireEphBase | ephBucket, nil
	}
	wire, ok := nonEphWireBytes[class]
	if !ok {
		return retentionWireInvalid, fmt.Errorf("%w: class %d", ErrRetentionClassUnknown, class)
	}
	if ephBucket != 0 {
		return retentionWireInvalid, fmt.Errorf("%w: class %d carries eph bucket %d", ErrEphBucketOnNonEphClass, class, ephBucket)
	}
	return wire, nil
}

// Whether the server may ever erase the body of a record of this class. Spec B section
// 7.2 gives the action each class takes at prune_after and spec B section 3.5 sums it up
// in a column: permanent is never prunable, and every other class has a deadline past
// which the body goes.
//
// The question is about the class alone. A durable group that publishes no ttl has an
// infinite deadline, but it is still a class the sweep acts on, and the deadline itself
// is the server's arithmetic over the group's policy rather than anything this package
// can answer.
//
// A value this package does not define as a class answers no. Spec A section 12.1
// publishes this without an error, so there is nowhere to report one, and no is the
// answer that keeps data: a byte that decoded to nothing legal was refused at the split
// long before it reached here, and a caller that walked past that refusal must not have
// the walk turned into a delete. The bool is a property of a legal value rather than a
// report of a failure, which is why it does not break the rule errors.go states.
func ClassIsPrunable(class RetentionClass) bool {
	prunable, known := classPrunable[class]
	return known && prunable
}

// The prunable column of spec B section 3.5. A table rather than a comparison against
// permanent, so a class added later has to be given an answer here instead of inheriting
// one from whichever side of a != it happens to fall.
var classPrunable = map[RetentionClass]bool{
	RetentionPermanent: false,
	RetentionDurable:   true,
	RetentionMedia:     true,
	RetentionEph:       true,
}
