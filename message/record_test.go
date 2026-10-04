// The record types, the two ladders, and the gate that keeps the class/bucket crossing
// in one place.
//
// Three things are being observed here and they fail in different directions. The first
// is that the wire alphabet is exactly the nine bytes master section 8 admits and that
// the split and the join are inverses over it — asserted by asking about all 256 bytes
// and all 65536 class/bucket pairs rather than by writing the legal ones down, so a
// parser that widened tomorrow widens the derived set and fails here instead of quietly
// accepting a byte no other implementation will. The second is that each of those nine
// bytes means what the spec says it means, which is the one thing no derived property
// can see: a table permuted in both directions round trips perfectly and agrees with
// nobody, so the meanings are written down once, in masterWireTable, and everything
// compares against that. The third is that no other file rebuilds either half of the
// crossing, which needs a walk of the tree rather than a list of files, and a positive
// control so that "found nothing" cannot mean "the rules stopped matching".
//
// The pinned literals are pinned on purpose. Spec B indexes and CHECKs on the
// ct_body column of the size ladder and restates the eph ladder, so a drift in either
// is a cross spec break that no round trip property would notice: both directions would
// still agree with each other, and with nobody else.
package message

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// A split retention class: the two halves of the wire byte, as go carries them.
type classBucket struct {
	class  RetentionClass
	bucket uint8
}

// The whole legal wire alphabet, derived by asking the split about every one of the 256
// bytes rather than by writing the nine down. Everything below reads its universe out
// of this, so a parser that accepted a tenth byte would carry that byte into every
// assertion that follows and be caught by the one that pins the set.
func acceptedWireBytes(t testing.TB) map[byte]classBucket {
	t.Helper()
	accepted := map[byte]classBucket{}
	for value := 0; value <= 0xFF; value++ {
		wire := byte(value)
		class, bucket, err := RetentionClassOf(wire)
		if err != nil {
			continue
		}
		accepted[wire] = classBucket{class: class, bucket: bucket}
	}
	if len(accepted) == 0 {
		t.Fatal("the split accepted no byte at all, so every assertion below would hold vacuously")
	}
	return accepted
}

// The wire table of master section 8 and spec A section 5.1, written out once: every
// legal byte, and the class and the bucket it means. The set of legal bytes is derived
// from the parser everywhere else in this file; this is the one place the MEANING of a
// byte is written down.
//
// It has to be written down, because nothing derived can pin it. Swap durable and media
// in the split and in the join together and every byte still round trips, every pair is
// still distinct, the join still refuses everything it should — the package agrees with
// itself perfectly, and the records it writes are sealed under K_media where every other
// implementation looks for K_durable (master section 8.1), carry a retention_class byte
// inside aad_body, aad_head and the write_auth preimage that no other reader will
// reproduce, and are pruned by the server on the wrong class. The same holds for any
// permutation of the eph buckets. A self consistent table is the one error a property
// about the table cannot see.
var masterWireTable = map[byte]classBucket{
	0x00: {class: RetentionPermanent, bucket: 0},
	0x01: {class: RetentionDurable, bucket: 0},
	0x02: {class: RetentionMedia, bucket: 0},
	0x10: {class: RetentionEph, bucket: 0},
	0x11: {class: RetentionEph, bucket: 1},
	0x12: {class: RetentionEph, bucket: 2},
	0x13: {class: RetentionEph, bucket: 3},
	0x14: {class: RetentionEph, bucket: 4},
	0x15: {class: RetentionEph, bucket: 5},
}

// The bytes of the table above, sorted, as the one written down alphabet every derived
// set is compared against.
func masterWireBytes() []int {
	bytes := make([]int, 0, len(masterWireTable))
	for wire := range masterWireTable {
		bytes = append(bytes, int(wire))
	}
	slices.Sort(bytes)
	return bytes
}

// The alphabet: the nine bytes of master section 8 and nothing else. The set under test
// is computed by asking the split about all 256 of them; the set it is compared against
// is the table above, so a widening or a narrowing of the parser moves the computed set
// off the one place the spec is written down.
func TestRetentionClassOfAcceptsExactlyTheNineLegalBytes(t *testing.T) {
	accepted := []int{}
	for wire := range acceptedWireBytes(t) {
		accepted = append(accepted, int(wire))
	}
	slices.Sort(accepted)
	want := masterWireBytes()
	if len(want) != 9 {
		t.Fatalf("the wire table names %d bytes, want the 9 of master section 8", len(want))
	}
	if !slices.Equal(accepted, want) {
		t.Errorf("the split accepts %v, want exactly %v; 0x03..0x0f and 0x16..0xff are all errors", accepted, want)
	}
}

// The mapping itself, which is the thing the round trip and the two inverse properties
// do not touch: each of the nine bytes decodes to the class and the bucket the table
// names, and joins back from them. A byte that means something else here is a record no
// other implementation can open and one the server prunes under a class its writer never
// chose.
func TestEveryWireByteMeansWhatMasterSection8Says(t *testing.T) {
	accepted := acceptedWireBytes(t)
	for _, value := range masterWireBytes() {
		wire := byte(value)
		want := masterWireTable[wire]
		got, legal := accepted[wire]
		if !legal {
			t.Errorf("0x%02x is class %d bucket %d in master section 8, and the split refuses it", wire, want.class, want.bucket)
			continue
		}
		if got != want {
			t.Errorf("0x%02x splits to class %d bucket %d, want the class %d bucket %d of master section 8", wire, got.class, got.bucket, want.class, want.bucket)
		}
		joined, err := RetentionClassWire(want.class, want.bucket)
		if err != nil {
			t.Errorf("class %d bucket %d is 0x%02x in master section 8, and the join refused it: %v", want.class, want.bucket, wire, err)
			continue
		}
		if joined != wire {
			t.Errorf("class %d bucket %d joins to 0x%02x, want the 0x%02x master section 8 names", want.class, want.bucket, joined, wire)
		}
	}
	for wire, pair := range accepted {
		if _, named := masterWireTable[wire]; !named {
			t.Errorf("the split accepts 0x%02x as class %d bucket %d, and master section 8 names no such byte", wire, pair.class, pair.bucket)
		}
	}
}

// The go side tags, pinned to the values spec A section 5.1 declares. Every other
// retention test reads its classes back out of the split, so the tags float free of the
// spec unless something says what they are — and one of them says more than its own
// value: RetentionPermanent is 0, which is what makes the zero value of a RecordHeader a
// permanent record rather than a class the join refuses outright.
func TestRetentionClassTagsAreTheValuesSpecASection51Declares(t *testing.T) {
	pinned := []struct {
		name  string
		class RetentionClass
		want  uint8
	}{
		{name: "RetentionPermanent", class: RetentionPermanent, want: 0},
		{name: "RetentionDurable", class: RetentionDurable, want: 1},
		{name: "RetentionMedia", class: RetentionMedia, want: 2},
		{name: "RetentionEph", class: RetentionEph, want: 3},
	}
	for _, tag := range pinned {
		if uint8(tag.class) != tag.want {
			t.Errorf("%s is %d, want %d", tag.name, uint8(tag.class), tag.want)
		}
		// and the value handed back with a refusal is none of them, so a caller that
		// dropped the error is holding something every later check refuses
		if retentionClassInvalid == tag.class {
			t.Errorf("the class the split returns with a refusal is %s", tag.name)
		}
	}
	var header RecordHeader
	if header.RetentionClass != RetentionPermanent {
		t.Errorf("the class of a zero valued header is %d, want permanent", header.RetentionClass)
	}
	if wire, err := RetentionClassWire(header.RetentionClass, header.EphBucket); err != nil {
		t.Errorf("a zero valued header does not encode: %v", err)
	} else if wire != 0x00 {
		t.Errorf("a zero valued header encodes as 0x%02x, want 0x00", wire)
	}
}

// The round trip over the whole byte space: every byte the split accepts must rejoin to
// itself, and every byte it refuses must leave the caller holding nothing usable.
func TestRetentionClassRoundTripsEveryByteTheSplitAccepts(t *testing.T) {
	roundTripped := 0
	for value := 0; value <= 0xFF; value++ {
		wire := byte(value)
		class, bucket, err := RetentionClassOf(wire)
		if err != nil {
			// a refusal that still hands back an encodable class is a refusal a caller
			// can walk straight past, which is the failure mode the guardrail on fatal
			// errors exists to stop
			if joined, joinErr := RetentionClassWire(class, bucket); joinErr == nil {
				t.Errorf("0x%02x was refused but the class it returned still joins to 0x%02x", wire, joined)
			}
			continue
		}
		joined, err := RetentionClassWire(class, bucket)
		if err != nil {
			t.Errorf("0x%02x split to class %d bucket %d, which the join then refused: %v", wire, class, bucket, err)
			continue
		}
		if joined != wire {
			t.Errorf("0x%02x split to class %d bucket %d and rejoined as 0x%02x", wire, class, bucket, joined)
			continue
		}
		roundTripped++
	}
	if roundTripped != 9 {
		t.Errorf("%d bytes round tripped, want the 9 legal ones", roundTripped)
	}
}

// The other order, and the distinctness the round trip alone does not show. A join that
// dropped the eph bucket would still round trip in the byte to pair to byte direction
// if the split dropped it too; what catches that pair of changes is that nine distinct
// bytes must produce nine distinct (class, bucket) pairs and each pair must come back
// to the byte it came from.
func TestRetentionClassJoinAndSplitAreInversesInBothOrders(t *testing.T) {
	accepted := acceptedWireBytes(t)
	pairs := map[classBucket]byte{}
	for wire, pair := range accepted {
		joined, err := RetentionClassWire(pair.class, pair.bucket)
		if err != nil {
			t.Errorf("class %d bucket %d came out of 0x%02x but the join refused it: %v", pair.class, pair.bucket, wire, err)
			continue
		}
		if joined != wire {
			t.Errorf("class %d bucket %d joined to 0x%02x, want the 0x%02x it was split from", pair.class, pair.bucket, joined, wire)
		}
		class, bucket, err := RetentionClassOf(joined)
		if err != nil {
			t.Errorf("class %d bucket %d joined to 0x%02x, which the split then refused: %v", pair.class, pair.bucket, joined, err)
			continue
		}
		if (classBucket{class: class, bucket: bucket}) != pair {
			t.Errorf("class %d bucket %d joined to 0x%02x and split back to class %d bucket %d", pair.class, pair.bucket, joined, class, bucket)
		}
		if earlier, repeated := pairs[pair]; repeated {
			t.Errorf("0x%02x and 0x%02x both split to class %d bucket %d", earlier, wire, pair.class, pair.bucket)
		}
		pairs[pair] = wire
	}
	if len(pairs) != len(accepted) {
		t.Errorf("%d accepted bytes produced only %d distinct class/bucket pairs", len(accepted), len(pairs))
	}
}

// The join over its whole domain: all 65536 (class, bucket) pairs, of which exactly the
// nine the wire alphabet names may be encodable. The legal set is derived from the
// split, so the two functions are pinned to one another rather than each to a list.
func TestRetentionClassWireAcceptsExactlyTheLegalPairs(t *testing.T) {
	legalWireOfPair := map[classBucket]byte{}
	for wire, pair := range acceptedWireBytes(t) {
		legalWireOfPair[pair] = wire
	}
	disagreements := 0
	samples := []string{}
	note := func(format string, args ...any) {
		disagreements++
		if len(samples) < 8 {
			samples = append(samples, fmt.Sprintf(format, args...))
		}
	}
	for classValue := 0; classValue <= 0xFF; classValue++ {
		for bucketValue := 0; bucketValue <= 0xFF; bucketValue++ {
			pair := classBucket{class: RetentionClass(classValue), bucket: uint8(bucketValue)}
			wire, err := RetentionClassWire(pair.class, pair.bucket)
			want, legal := legalWireOfPair[pair]
			switch {
			case legal && err != nil:
				note("class %d bucket %d is legal but the join refused it: %v", classValue, bucketValue, err)
			case legal && wire != want:
				note("class %d bucket %d joined to 0x%02x, want 0x%02x", classValue, bucketValue, wire, want)
			case !legal && err == nil:
				note("class %d bucket %d is not on the wire but the join produced 0x%02x", classValue, bucketValue, wire)
			case !legal:
				// the byte handed back with a refusal must itself be unusable, or a
				// caller that ignored the error still writes a record
				if _, _, splitErr := RetentionClassOf(wire); splitErr == nil {
					note("class %d bucket %d was refused but returned the legal byte 0x%02x", classValue, bucketValue, wire)
				}
			}
		}
	}
	if 0 < disagreements {
		t.Errorf("%d of the 65536 class/bucket pairs disagree with the wire alphabet; first %d: %s", disagreements, len(samples), strings.Join(samples, "; "))
	}
	if len(legalWireOfPair) != 9 {
		t.Errorf("the wire alphabet named %d distinct pairs, want 9", len(legalWireOfPair))
	}
}

// The leak the split of the two fields exists to prevent. A bucket only means anything
// under eph, and the tempting implementation of the join ignores it everywhere else and
// returns 0x00 for permanent — which stores a record as though the caller's belief
// about the bucket had been right. Every non eph class is crossed with every bucket
// from 1 to 255, and the classes are read out of the wire alphabet rather than listed.
func TestEphBucketNeverLeaksIntoANonEphClass(t *testing.T) {
	nonEphClasses := map[RetentionClass]bool{}
	for _, pair := range acceptedWireBytes(t) {
		if pair.class != RetentionEph {
			nonEphClasses[pair.class] = true
		}
	}
	if len(nonEphClasses) != 3 {
		t.Errorf("the wire alphabet named %d non eph classes, want 3", len(nonEphClasses))
	}
	for class := range nonEphClasses {
		// the zero bucket is the legal one for these classes, so the refusals below
		// are about the bucket and not about the class
		if _, err := RetentionClassWire(class, 0); err != nil {
			t.Errorf("class %d with no bucket was refused: %v", class, err)
		}
		for bucketValue := 1; bucketValue <= 0xFF; bucketValue++ {
			wire, err := RetentionClassWire(class, uint8(bucketValue))
			if err == nil {
				t.Errorf("class %d carrying eph bucket %d was silently encoded as 0x%02x", class, bucketValue, wire)
				continue
			}
			if !errors.Is(err, ErrEphBucketOnNonEphClass) {
				t.Errorf("class %d carrying eph bucket %d was refused with %v, want %v", class, bucketValue, err, ErrEphBucketOnNonEphClass)
			}
		}
	}
}

// The size ladder, pinned by value. The right hand column is the one spec B indexes and
// CHECKs on, so these five literals are a cross spec contract and not an internal
// detail: a change here that both ladder functions agreed on would still break every
// stored record's length check.
func TestSizeBucketLadderIsPinned(t *testing.T) {
	wantBodyBytes := []int{256, 1024, 4096, 16384, 65536}
	wantCtBodyBytes := []int{272, 1040, 4112, 16400, 65552}
	for bucket := range wantBodyBytes {
		if body := SizeBucketBytes(SizeBucket(bucket)); body != wantBodyBytes[bucket] {
			t.Errorf("size bucket %d has body %d bytes, want %d", bucket, body, wantBodyBytes[bucket])
		}
		if ctBody := SizeBucketCtBodyBytes(SizeBucket(bucket)); ctBody != wantCtBodyBytes[bucket] {
			t.Errorf("size bucket %d has ct_body %d bytes, want %d", bucket, ctBody, wantCtBodyBytes[bucket])
		}
	}
	if SizeBucketBlob != SizeBucket(len(wantBodyBytes)) {
		t.Errorf("the blob rung is %d, want the rung just past the five with a body", SizeBucketBlob)
	}
	// the blob rung and everything past the ladder have no inline body, and neither may
	// answer anything a caller could spend as a length
	for value := int(SizeBucketBlob); value <= 0xFF; value++ {
		if body := SizeBucketBytes(SizeBucket(value)); 0 <= body {
			t.Errorf("size bucket %d has no inline body but reports %d body bytes", value, body)
		}
		if ctBody := SizeBucketCtBodyBytes(SizeBucket(value)); 0 <= ctBody {
			t.Errorf("size bucket %d has no inline body but reports %d ct_body bytes", value, ctBody)
		}
	}
}

// The named rungs of the size ladder, pinned to the values spec A section 5.1 declares
// and to the lengths behind them. The ladder test above indexes with raw integers, which
// leaves the constants a caller actually writes unchecked: SizeBucket256 pointing at the
// 64 KiB rung pads a two hundred byte message to 65536 bytes, and a test that only ever
// asks about SizeBucket(0) stays green while it happens.
func TestSizeBucketConstantsNameTheRungsSpecASection51Declares(t *testing.T) {
	pinned := []struct {
		name      string
		rung      SizeBucket
		want      uint8
		wantBytes int
	}{
		{name: "SizeBucket256", rung: SizeBucket256, want: 0, wantBytes: 256},
		{name: "SizeBucket1K", rung: SizeBucket1K, want: 1, wantBytes: 1024},
		{name: "SizeBucket4K", rung: SizeBucket4K, want: 2, wantBytes: 4096},
		{name: "SizeBucket16K", rung: SizeBucket16K, want: 3, wantBytes: 16384},
		{name: "SizeBucket64K", rung: SizeBucket64K, want: 4, wantBytes: 65536},
		// the blob rung is the one whose name promises no inline body at all
		{name: "SizeBucketBlob", rung: SizeBucketBlob, want: 5, wantBytes: noLadderValue},
	}
	for _, rung := range pinned {
		if uint8(rung.rung) != rung.want {
			t.Errorf("%s is %d, want %d", rung.name, uint8(rung.rung), rung.want)
		}
		body := SizeBucketBytes(rung.rung)
		ctBody := SizeBucketCtBodyBytes(rung.rung)
		if rung.wantBytes < 0 {
			if 0 <= body || 0 <= ctBody {
				t.Errorf("%s has no inline body but reports %d body bytes and %d ct_body bytes", rung.name, body, ctBody)
			}
			continue
		}
		if body != rung.wantBytes {
			t.Errorf("%s is %d body bytes, want %d", rung.name, body, rung.wantBytes)
		}
		if want := rung.wantBytes + aeadTagBytes; ctBody != want {
			t.Errorf("%s is %d ct_body bytes, want %d", rung.name, ctBody, want)
		}
	}
	// and the names cover the whole ladder, so a seventh rung cannot arrive unnamed
	if len(pinned) != int(SizeBucketBlob)+1 {
		t.Errorf("%d rungs are named, want the %d the ladder ends at", len(pinned), int(SizeBucketBlob)+1)
	}
}

// The relation between the two size functions, over every rung, derived rather than
// listed: whatever the ladder says, the ciphertext is the body plus the 16 byte aead
// tag and nothing else. This is what stops the two functions drifting apart in a change
// that updated only one of the pinned columns above.
func TestSizeBucketCtBodyIsTheBodyPlusTheAeadTag(t *testing.T) {
	rungsWithABody := 0
	for value := 0; value <= 0xFF; value++ {
		bucket := SizeBucket(value)
		body := SizeBucketBytes(bucket)
		ctBody := SizeBucketCtBodyBytes(bucket)
		if body < 0 {
			if 0 <= ctBody {
				t.Errorf("size bucket %d has no body length but reports a ct_body of %d", value, ctBody)
			}
			continue
		}
		if ctBody != body+16 {
			t.Errorf("size bucket %d has body %d and ct_body %d; ct_body must be the body plus the 16 byte aead tag", value, body, ctBody)
		}
		rungsWithABody++
	}
	if rungsWithABody != 5 {
		t.Errorf("%d rungs carry a body, want the 5 of the ladder", rungsWithABody)
	}
}

// The eph ladder, pinned by value for the same cross spec reason as the size ladder,
// with bucket 0 held to the transient rung's contract: it is never persisted, so its
// retention window is nought seconds -- a real window and not the absence of one.
//
// THE BUCKET 0 ASSERTION WAS INVERTED BY THE 2026-09-13 RULING AND IS RECORDED RATHER
// THAN QUIETLY SWAPPED. It read "if seconds := EphBucketSeconds(0); 0 <= seconds" --
// that is, bucket 0 had to be NEGATIVE, the same answer as an off ladder bucket, which
// is exactly what m1 open item M1-27 was filed about: two meanings under one sentinel,
// so no caller could ask which of the two it had met. Master section 8, spec A section
// 5.1 and spec B section 3.1 now all carry one paragraph saying they MUST DIFFER.
func TestEphBucketSecondsLadderIsPinned(t *testing.T) {
	wantSeconds := []int{1: 3600, 2: 28800, 3: 86400, 4: 604800, 5: 2419200}
	for bucket := 1; bucket < len(wantSeconds); bucket++ {
		if seconds := EphBucketSeconds(uint8(bucket)); seconds != wantSeconds[bucket] {
			t.Errorf("eph bucket %d is %d seconds, want %d", bucket, seconds, wantSeconds[bucket])
		}
	}
	// THE DISTINGUISHABILITY FIRST, because that is the property and the two literals are
	// only its consequence: a table that swapped them would satisfy both literal checks
	// below read one at a time, and this is the assertion that cannot be satisfied by any
	// single answer at all.
	transient := EphBucketSeconds(0)
	offLadder := EphBucketSeconds(uint8(len(wantSeconds)))
	if transient == offLadder {
		t.Errorf("eph bucket 0 and eph bucket %d both answer %d; the transient rung has a real retention window of nought seconds and a bucket off the ladder is not a bucket at all, and a caller holding one answer cannot ask which of them it met",
			len(wantSeconds), transient)
	}
	// and each on its own side, which is the half that says WHICH way round they differ
	if transient != 0 {
		t.Errorf("eph bucket 0 answers %d seconds; master section 8 rules it 0 -- the true retention window of a rung that is never stored", transient)
	}
	for bucket := len(wantSeconds); bucket <= 0xFF; bucket++ {
		if seconds := EphBucketSeconds(uint8(bucket)); 0 <= seconds {
			t.Errorf("eph bucket %d is not on the ladder but reports a window of %d seconds; master section 8 rules 6..255 a NEGATIVE", bucket, seconds)
		}
	}
	// strictly increasing, so a swap of two rungs is a failure twice over rather than a
	// pair of literals that still add up
	for bucket := 2; bucket < len(wantSeconds); bucket++ {
		if EphBucketSeconds(uint8(bucket)) <= EphBucketSeconds(uint8(bucket-1)) {
			t.Errorf("eph bucket %d is not longer than bucket %d", bucket, bucket-1)
		}
	}
}

// The eph ladder as a record on the wire meets it: the byte, and the window the server
// will prune a record carrying it on.
//
// The ladder test above pins the seconds against the go bucket index, which leaves the
// byte a bucket is reached by unpinned. Permute the buckets in the split and the join
// together and every seconds value is still exactly where that test looks for it, while
// 0x10 has become an hour and the rung that is never persisted has moved somewhere else
// on the wire. K_eph[n][b][t] = HKDF-Expand(eph_root[n], "eph/v1" || u8(b) || u64(t), 32)
// keys on b (master section 8.1), so a permuted bucket is a record nobody can open, and
// a transient that gets stored for an hour is the one thing the transient rung promises
// never to do.
var masterEphWindowSeconds = map[byte]int{
	0x10: transientRungSeconds,
	0x11: 3600,
	0x12: 28800,
	0x13: 86400,
	0x14: 604800,
	0x15: 2419200,
}

// What the table above says about the transient rung: nought seconds, because the record
// is never stored and nought is how long a thing that is never stored is kept for.
//
// It was neverPersisted = -1 until the ruling of 2026-09-13, where -1 was the same answer
// an off ladder bucket gave and the two could not be told apart. Master section 8, spec A
// section 5.1 and spec B section 3.1 now carry one paragraph: 0 for bucket 0, a negative
// for 6..255.
const transientRungSeconds = 0

func TestEveryEphWireByteCarriesTheWindowMasterSection8Names(t *testing.T) {
	for _, value := range masterWireBytes() {
		wire := byte(value)
		want, isEphByte := masterEphWindowSeconds[wire]
		if !isEphByte {
			if masterWireTable[wire].class == RetentionEph {
				t.Errorf("0x%02x is an eph byte in the wire table and the eph ladder gives it no window", wire)
			}
			continue
		}
		class, bucket, err := RetentionClassOf(wire)
		if err != nil {
			t.Errorf("0x%02x carries an eph window and the split refuses it: %v", wire, err)
			continue
		}
		if class != RetentionEph {
			t.Errorf("0x%02x carries an eph window and splits to class %d", wire, class)
			continue
		}
		seconds := EphBucketSeconds(bucket)
		if seconds != want {
			t.Errorf("a record written as 0x%02x expires after %d seconds, want the %d of master section 8", wire, seconds, want)
		}
	}
	if len(masterEphWindowSeconds) != 6 {
		t.Errorf("the eph ladder names %d bytes, want the 6 rungs of master section 8", len(masterEphWindowSeconds))
	}
}

// The two ladders answer over exactly the buckets the wire admits, derived from the
// alphabet: every eph byte on the wire names a rung, and no rung exists that the wire
// cannot name. A ladder that grew a seventh rung would put a window behind a byte no
// reader accepts, which is a record nobody can prune on and everybody refuses.
func TestEphLadderCoversExactlyTheBucketsTheWireAdmits(t *testing.T) {
	wireBuckets := map[uint8]bool{}
	for _, pair := range acceptedWireBytes(t) {
		if pair.class == RetentionEph {
			wireBuckets[pair.bucket] = true
		}
	}
	if len(wireBuckets) != 6 {
		t.Errorf("the wire alphabet named %d eph buckets, want 6", len(wireBuckets))
	}
	for bucket := 0; bucket <= 0xFF; bucket++ {
		onTheWire := wireBuckets[uint8(bucket)]
		seconds := EphBucketSeconds(uint8(bucket))
		// bucket 0 is on the wire and its window is nought on purpose -- it is never
		// persisted -- so it is the one rung where "on the wire" and "has a positive
		// window" legitimately disagree. It is checked, not skipped: the ruling of
		// 2026-09-13 makes the nought an ANSWER, so there is something to assert here
		// where before there was only an absence.
		if bucket == 0 {
			if seconds != 0 {
				t.Errorf("eph bucket 0 is on the wire and answers %d seconds, want the 0 master section 8 rules for the transient rung", seconds)
			}
			continue
		}
		if onTheWire && seconds <= 0 {
			t.Errorf("eph bucket %d is on the wire but has no retention window", bucket)
		}
		if !onTheWire && 0 < seconds {
			t.Errorf("eph bucket %d has a window of %d seconds but no wire byte names it", bucket, seconds)
		}
	}
}

// Prunability, pinned per class against spec B section 3.5's column and asked of every
// value a RetentionClass can hold. Spec B section 7.2 is what the column summarises:
// permanent takes no action at prune_after ever, and every other class loses its body.
//
// A value that is not a class at all answers no, which is the answer that keeps data. It
// is also the answer that cannot be reached honestly — the split refuses the byte long
// before anything holds such a class — so what this pins is that a caller who got there
// dishonestly does not get a delete out of it.
func TestClassIsPrunableAnswersSpecBSection35(t *testing.T) {
	prunable := map[RetentionClass]bool{
		RetentionPermanent: false,
		RetentionDurable:   true,
		RetentionMedia:     true,
		RetentionEph:       true,
	}
	// the classes are read off the wire alphabet rather than listed, so a class the wire
	// grows has to be given an answer here before this test will pass again
	onTheWire := map[RetentionClass]bool{}
	for _, pair := range acceptedWireBytes(t) {
		onTheWire[pair.class] = true
	}
	if len(onTheWire) != len(prunable) {
		t.Errorf("the wire alphabet names %d classes and spec B section 3.5 pins %d", len(onTheWire), len(prunable))
	}
	for class := range onTheWire {
		if _, pinned := prunable[class]; !pinned {
			t.Errorf("the wire alphabet names class %d, which spec B section 3.5 says nothing about", class)
		}
	}
	for value := 0; value <= 0xFF; value++ {
		class := RetentionClass(value)
		answer := ClassIsPrunable(class)
		want, legal := prunable[class]
		switch {
		case legal && answer != want:
			t.Errorf("class %d is prunable=%v, want %v", value, answer, want)
		case !legal && answer:
			t.Errorf("class %d is not a class at all and answers that its body may be erased", value)
		}
	}
	// the one answer that is a promise rather than a policy: master section 12.2 and
	// spec B section 7.2 both say a permanent record is never acted on
	if ClassIsPrunable(RetentionPermanent) {
		t.Error("permanent is prunable, and permanent is the class that is never erased")
	}
}

// ── the gate: the crossing happens in one file and nowhere else ──────────────────
//
// Spec A section 5.1 bans four expression shapes — class<<4, class|bucket, 16+bucket
// and class*16 — anywhere but the two functions in record.go, and the sentence beside
// them says those two functions are the only ones in the system that join OR SPLIT the
// class and the bucket. All four names are join shapes, so a gate that stops at them
// forbids building the byte and allows taking it apart, which is the same conflation
// read backwards: a prune query that recovers the class with wire>>4 treats eph bucket 1
// as a different class from eph bucket 0 exactly as class<<4 does. The split half is
// banned here too, under names of its own.
//
// The rules read the syntax tree rather than the text, which is what keeps them from
// being a list of the spellings somebody thought of. retentionWireByClass[class]+bucket
// is the same join as 16+bucket and no pattern anchored on an identifier spans the
// bracket; a comment on the end of a line of code is prose that a text matcher reads as
// code. Both are free here: an operand is a subtree, so the words in it are found
// wherever they sit, and comments are not expressions at all.
//
// Nothing below rests on a scan having run, for the reason mls/crypto_forbidden_test.go
// gives at length and this file follows: a scanner that finds nothing because it is
// broken reports exactly what one that finds nothing because the tree is clean reports.
// So the scan refuses a root it could not read, that held no go source, or that held a
// file it could not parse; the set of paths the gate iterates is checked against the set
// the scan collected, because a gate handed an empty set reports every root clean having
// examined nothing; the allowance is counted, because an allowance that widened is the
// same clean report; every rule is a function the gates and a positive control both
// call; the control feeds it a fixture committing every banned shape, so a rule that
// stopped matching fails there rather than issuing the tree a clean bill; and a second
// fixture writes every shape in prose, and puts one of those sentences on the end of a
// line of working code, so "not reported" means "allowed or absent" rather than "the
// rules are asleep".

// The trees the ban covers, relative to this package's directory. connect itself is not
// among them and cannot be: it is the parent, it may not import any of these
// packages, and its data path is full of unrelated bit arithmetic these rules would
// report on.
//
// messagegroupRoot joined on the commit that split this package in two. The class and the
// bucket are most naturally at hand together in the sealer, and the sealer is over there, so
// a scope that stopped at this directory would have covered the one place the crossing is
// hardest to make and left the one place it is easiest. Nothing would have reported it:
// scanJoinSources reads the roots it is given and finds no offender in a directory it never
// opened.
const (
	messageRoot      = "."
	mlsRoot          = "../mls"
	messagegroupRoot = "../messagegroup"
	joinControlDir   = "testdata/forbidden"
)

// The one file allowed to cross between the two shapes, as the scan keys it: a path
// under a root, not a base name. A base name exempts every file called record.go in
// every root, and connect/mls is free to grow one tomorrow — the allowance is the whole
// of this gate's surface area, so it names a place and not a filename.
var joinAllowedPaths = []string{"record.go"}

// Where the sdk module sits, derived from where this module ends rather than written
// down. sdk is a sibling repository of connect and not a directory inside it, so a root
// of "../sdk" — which is what this said — resolves to connect/sdk and can never exist,
// and the gate logged that it would cover sdk the day it appeared while that day could
// not come. Walking up to the module root and stepping out of it is the same sentence in
// code, and it cannot resolve to a path inside connect.
func joinSdkRoot(t *testing.T) string {
	t.Helper()
	moduleRoot := messageRoot
	for range 8 {
		if _, err := os.Stat(filepath.Join(moduleRoot, "go.mod")); err == nil {
			return filepath.ToSlash(filepath.Join(moduleRoot, "..", "sdk"))
		}
		moduleRoot = filepath.Join(moduleRoot, "..")
	}
	t.Fatalf("no go.mod above %s, so the sdk root cannot be derived", messageRoot)
	return ""
}

// The scan roots, computed rather than fixed: sdk is a separate checkout and a tree that
// holds only connect is a tree where it is genuinely absent, so its absence is logged
// instead of assumed — and asserted about, in TestTheSdkRootIsASiblingOfThisModule,
// because a root that is present and uncovered is the failure this logging hides.
func joinScanRoots(t *testing.T) []string {
	t.Helper()
	roots := []string{messageRoot, mlsRoot, messagegroupRoot}
	sdkRoot := joinSdkRoot(t)
	if entry, err := os.Stat(sdkRoot); err == nil && entry.IsDir() {
		roots = append(roots, sdkRoot)
	} else {
		t.Logf("%s is not checked out beside connect, so the gate covers %v; it joins the roots the day it appears", sdkRoot, roots)
	}
	return roots
}

// Which half of the crossing a rule names. Both halves are the same defect — a second
// place the class and the bucket meet, and a second place they can be conflated — but
// spec A enumerates only the join shapes, so the join half is pinned to exactly the four
// names spec A gives while the split half is free to grow.
type joinHalf int

const (
	joinsTheByte joinHalf = iota
	splitsTheByte
)

// What a failure says the file did, so the message reads as the rule.
func (self joinHalf) verb() string {
	if self == splitsTheByte {
		return "splits"
	}
	return "joins"
}

// One banned expression shape: the name spec A gives it, or the name this file gives its
// mirror image, the half it belongs to, and the predicate over one binary expression
// that decides it.
type joinShape struct {
	name    string
	half    joinHalf
	commits func(op token.Token, left, right joinOperand) bool
}

// One operand of a binary expression, reduced to the two things the rules ask about: the
// words its identifiers use, and the integer it is when it is a constant.
//
// The words are how a rule tells a join of a class and a bucket from arithmetic that
// happens to use the same operator — identifiers in this tree carry the words the spec
// uses. They are collected from the whole operand subtree rather than from its outermost
// identifier, which is what stops a table lookup, a conversion or a field selector from
// hiding the name: retentionWireByClass[retentionClass] names the class as plainly as
// retentionClass does, and a table lookup exactly like that one walked straight past the
// patterns this replaced.
type joinOperand struct {
	words    string
	constant int64
	isConst  bool
}

// The vocabulary. A rule asks about a word rather than about a spelling, so a name the
// gate has never seen still answers for it.
func (self joinOperand) namesClass() bool {
	return strings.Contains(self.words, "class") || strings.Contains(self.words, "retention")
}

func (self joinOperand) namesBucket() bool {
	return strings.Contains(self.words, "bucket")
}

// The byte itself, which is the thing a split takes apart. Only the split rules ask
// about this word, and only alongside the eph base: connect/mls calls its own encodings
// wire formats all over and none of that is this.
func (self joinOperand) namesWireByte() bool {
	return strings.Contains(self.words, "wire")
}

func (self joinOperand) isValue(value int64) bool {
	return self.isConst && self.constant == value
}

// The numbers the two halves are written with: the eph base, the nibble it sits in, and
// the mask that takes the bucket back out from under it.
const (
	ephBaseValue     int64 = 16
	nibbleShiftValue int64 = 4
	ephBucketMask    int64 = 0x0f
)

// One side names the class and the other names the bucket, with the eph base standing in
// for the class: a 16 beside a bucket is the class, written as the byte it becomes.
func joinMixesClassAndBucket(left, right joinOperand) bool {
	classSide := func(operand joinOperand) bool {
		return operand.namesClass() || operand.isValue(ephBaseValue)
	}
	return (classSide(left) && right.namesBucket()) || (classSide(right) && left.namesBucket())
}

// Either side names the retention vocabulary at all, which is as much as the or shape
// asks: a pipe beside a class or a bucket is the join whatever sits on the other side of
// it, and over reporting is the safe direction for a ban list.
func joinTouchesTheVocabulary(left, right joinOperand) bool {
	return left.namesClass() || left.namesBucket() || right.namesClass() || right.namesBucket()
}

var classBucketJoinShapes = []joinShape{
	// class<<4. Matched on the shift alone rather than on an operand, because neither
	// package packs bits: there is no other reason to shift a value by exactly four
	// here, and a legitimate shift by four arriving later is a review conversation,
	// which is the point.
	{name: "class<<4", half: joinsTheByte, commits: func(op token.Token, left, right joinOperand) bool {
		return op == token.SHL && right.isValue(nibbleShiftValue)
	}},
	// class|bucket, in either operand order, with either half named or the eph base
	// written as the literal it is. The exclusive or is the same join once more — the
	// two nibbles cannot overlap, so it carries the identical byte — and is matched
	// here rather than given a name of its own that spec A does not use.
	{name: "class|bucket", half: joinsTheByte, commits: func(op token.Token, left, right joinOperand) bool {
		if op != token.OR && op != token.XOR {
			return false
		}
		return joinTouchesTheVocabulary(left, right) || left.isValue(ephBaseValue) || right.isValue(ephBaseValue)
	}},
	// 16+bucket, in either order, with the base written as a literal, named, or looked
	// up in a table. The base has to appear as itself or as the class beside a named
	// bucket, because adding sixteen to something is otherwise ordinary arithmetic:
	// mls/hpke_fuzz_test.go widens a key length with params.Nsk+16 and means nothing by
	// it.
	{name: "16+bucket", half: joinsTheByte, commits: func(op token.Token, left, right joinOperand) bool {
		return op == token.ADD && joinMixesClassAndBucket(left, right)
	}},
	// class*16, in either order, which is the shift written for a reader who dislikes
	// shifts. The class multiplied by the base is the whole join on its own, with the
	// bucket added somewhere else or not yet added at all, so unlike the addition this
	// one does not need a bucket beside it.
	{name: "class*16", half: joinsTheByte, commits: func(op token.Token, left, right joinOperand) bool {
		if op != token.MUL {
			return false
		}
		return joinMixesClassAndBucket(left, right) ||
			(left.namesClass() && right.isValue(ephBaseValue)) ||
			(right.namesClass() && left.isValue(ephBaseValue))
	}},
	// wire>>4, the class taken back out of the high nibble, matched on the shift alone
	// for the reason class<<4 is.
	{name: "wire>>4", half: splitsTheByte, commits: func(op token.Token, left, right joinOperand) bool {
		return op == token.SHR && right.isValue(nibbleShiftValue)
	}},
	// wire&0x0f, the bucket taken out from under the base. The mask is what makes it
	// this rather than a flag test, so the rule is the mask and not the operator.
	{name: "wire&0x0f", half: splitsTheByte, commits: func(op token.Token, left, right joinOperand) bool {
		return op == token.AND && (left.isValue(ephBucketMask) || right.isValue(ephBucketMask))
	}},
	// wire-0x10, the bucket recovered by subtracting the base, and wire/16 and wire%16,
	// the two halves recovered by dividing by it. Unlike the two shifts these need a
	// name on the other side: subtracting sixteen from a length is how an aead tag is
	// stripped, and a remainder mod sixteen is how mls/syntax/marshal_test.go picks a
	// small index. Neither is this.
	{name: "wire-0x10", half: splitsTheByte, commits: func(op token.Token, left, right joinOperand) bool {
		if op != token.SUB && op != token.QUO && op != token.REM {
			return false
		}
		baseSide := func(operand joinOperand) bool {
			return operand.isValue(ephBaseValue) || operand.namesClass()
		}
		byteSide := func(operand joinOperand) bool {
			return operand.namesClass() || operand.namesBucket() || operand.namesWireByte()
		}
		return (baseSide(left) && byteSide(right)) || (baseSide(right) && byteSide(left))
	}},
}

// The shape names of one half, for a message that has to say what was looked for and for
// the assertion that spec A's four are all still here.
func shapeNames(half joinHalf) []string {
	names := []string{}
	for _, shape := range classBucketJoinShapes {
		if shape.half == half {
			names = append(names, shape.name)
		}
	}
	slices.Sort(names)
	return names
}

// One walk's result: the text and the syntax tree of every go file found, keyed by slash
// separated path, and the root each one came from. The per root attribution is what
// separates "the roots are clean" from "a root was never read", and it is one map rather
// than a count beside a map so the two cannot disagree.
type joinScan struct {
	fileSet     *token.FileSet
	sourceTexts map[string]string
	syntax      map[string]*ast.File
	rootOf      map[string]string
}

// How many files each root contributed, derived from the attribution.
func (self joinScan) countsByRoot() map[string]int {
	counts := map[string]int{}
	for _, root := range self.rootOf {
		counts[root]++
	}
	return counts
}

// Walks each root, reads and parses every go file under it. A directory named testdata
// or interop is skipped unless a root names it outright, which is how the controls reach
// their fixture and nothing else reaches it.
//
// A root that cannot be walked, a root that yielded no go file, and a file that does not
// parse are all errors, because each one produces a scan that reports every gate clean
// having read no code, or having read it and understood none of it. The error is
// returned rather than failed on so that the refusal can be tested directly instead of
// asserted about.
func scanJoinSources(roots []string) (joinScan, error) {
	scan := joinScan{
		fileSet:     token.NewFileSet(),
		sourceTexts: map[string]string{},
		syntax:      map[string]*ast.File{},
		rootOf:      map[string]string{},
	}
	if len(roots) == 0 {
		return scan, fmt.Errorf("no roots to scan")
	}
	for _, root := range roots {
		found := 0
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if path != root && (entry.Name() == "testdata" || entry.Name() == "interop") {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			text := string(body)
			syntax, err := parser.ParseFile(scan.fileSet, path, text, parser.SkipObjectResolution)
			if err != nil {
				return fmt.Errorf("parse %s: %w", path, err)
			}
			slashPath := filepath.ToSlash(path)
			if earlier, repeated := scan.rootOf[slashPath]; repeated {
				return fmt.Errorf("%s was contributed by both %s and %s, so the per root attribution is not a partition", slashPath, earlier, root)
			}
			scan.sourceTexts[slashPath] = text
			scan.syntax[slashPath] = syntax
			scan.rootOf[slashPath] = root
			found++
			return nil
		})
		if err != nil {
			return scan, fmt.Errorf("walk %s: %w", root, err)
		}
		if found == 0 {
			return scan, fmt.Errorf("walk %s read no go files; the scan is broken, not the source", root)
		}
	}
	return scan, nil
}

// The scan every gate starts from, with a failed walk fatal rather than reported: every
// assertion downstream is meaningless if the source was never read.
func mustScanJoinSources(t *testing.T, roots []string) joinScan {
	t.Helper()
	scan, err := scanJoinSources(roots)
	if err != nil {
		t.Fatalf("scanning %v: %v", roots, err)
	}
	return scan
}

// The paths the gate iterates, with the accounting that keeps an empty set from reading
// as a clean tree.
//
// The scan refuses a root it could not read, but the set the gate walks is a step past
// the scan, and a set that lost its contents between the two reports every root clean
// having examined nothing — silently, because there is nothing left to report on. So the
// arithmetic is here, in the function that hands the gate its work: every file the scan
// collected is in the set, the set is not empty, and every root contributed to it.
func joinPathsUnderGate(t *testing.T, scan joinScan, roots []string) []string {
	t.Helper()
	paths := joinScannedPaths(scan.syntax)
	if len(paths) == 0 {
		t.Fatal("the gate was handed no file to examine, so every rule below would hold vacuously")
	}
	if len(paths) != len(scan.sourceTexts) {
		t.Fatalf("the gate would examine %d files while the scan collected %d", len(paths), len(scan.sourceTexts))
	}
	counts := scan.countsByRoot()
	for _, root := range roots {
		if counts[root] == 0 {
			t.Fatalf("nothing from %s reached the gate, so that root is uncovered", root)
		}
	}
	return paths
}

// The source text of one node, whitespace collapsed, so a failure prints the expression
// a reader will find rather than a line number they have to go and look up.
func joinSourceOf(scan joinScan, text string, node ast.Node) string {
	from := scan.fileSet.Position(node.Pos()).Offset
	to := scan.fileSet.Position(node.End()).Offset
	if from < 0 || len(text) < to || to <= from {
		return "?"
	}
	return strings.Join(strings.Fields(text[from:to]), " ")
}

// Reduces one operand to what the rules ask about. Parentheses and conversions are
// unwrapped on the way, so byte(0x10) is the same constant as 0x10 and (class) is the
// same operand as class.
func joinOperandOf(expr ast.Expr) joinOperand {
	operand := joinOperand{}
	words := strings.Builder{}
	ast.Inspect(expr, func(node ast.Node) bool {
		if ident, isIdent := node.(*ast.Ident); isIdent {
			words.WriteString(strings.ToLower(ident.Name))
			words.WriteByte(' ')
		}
		return true
	})
	operand.words = words.String()
	if value, isConst := joinConstantOf(expr); isConst {
		operand.constant = value
		operand.isConst = true
	}
	return operand
}

// The integer an operand is, when it is one. A conversion of a literal counts, because
// byte(16) and 16 are the same sixteen; a call to anything else does not, because this
// gate cannot know what it answers.
func joinConstantOf(expr ast.Expr) (int64, bool) {
	switch node := expr.(type) {
	case *ast.ParenExpr:
		return joinConstantOf(node.X)
	case *ast.CallExpr:
		if _, isConversion := node.Fun.(*ast.Ident); isConversion && len(node.Args) == 1 {
			return joinConstantOf(node.Args[0])
		}
	case *ast.BasicLit:
		if node.Kind == token.INT {
			if value, err := strconv.ParseInt(node.Value, 0, 64); err == nil {
				return value, true
			}
		}
	}
	return 0, false
}

// Every expression in one file that commits shape, with its position and its source, so
// a failure is actionable. The gates and the controls both call this, so a rule that
// stopped matching fails the control instead of passing every file in the tree.
func joinShapeExpressions(scan joinScan, path string, shape joinShape) []string {
	found := []string{}
	syntax, scanned := scan.syntax[path]
	if !scanned {
		return found
	}
	text := scan.sourceTexts[path]
	ast.Inspect(syntax, func(node ast.Node) bool {
		binary, isBinary := node.(*ast.BinaryExpr)
		if !isBinary {
			return true
		}
		left := joinOperandOf(binary.X)
		right := joinOperandOf(binary.Y)
		if shape.commits(binary.Op, left, right) {
			found = append(found, fmt.Sprintf("%s: %s", scan.fileSet.Position(binary.Pos()), joinSourceOf(scan, text, binary)))
		}
		return true
	})
	return found
}

// The scanned paths whose code commits shape and that are not allowed to, each with the
// expressions that did it.
func joinViolations(scan joinScan, paths []string, shape joinShape, allowedPaths []string) map[string][]string {
	violations := map[string][]string{}
	for _, path := range paths {
		if slices.Contains(allowedPaths, path) {
			continue
		}
		if expressions := joinShapeExpressions(scan, path, shape); 0 < len(expressions) {
			violations[path] = expressions
		}
	}
	return violations
}

// Which of the paths the allowance let past, sorted. Compared against the allowance
// itself rather than trusted to be it: an allowance that widened — to a base name, to a
// directory, to a pattern that matches more than it names — is a gate that reports clean
// over the files it stopped looking at, and this count is the only evidence.
func joinAllowanceUsed(paths []string, allowedPaths []string) []string {
	used := []string{}
	for _, path := range paths {
		if slices.Contains(allowedPaths, path) {
			used = append(used, path)
		}
	}
	slices.Sort(used)
	return used
}

// The paths of anything keyed by path, sorted, for a failure message that has to show
// what was read. Generic over the value so the scan and the violation report are read
// out by the same function and cannot sort differently.
func joinScannedPaths[V any](byPath map[string]V) []string {
	paths := make([]string, 0, len(byPath))
	for path := range byPath {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	return paths
}

// One fixture file, missing being fatal rather than empty: an absent fixture would make
// every control assertion below trivially true, which is the failure this half of the
// file exists to rule out.
func joinControlPath(t *testing.T, control joinScan, name string) string {
	t.Helper()
	path := joinControlDir + "/" + name
	if _, scanned := control.syntax[path]; !scanned {
		t.Fatalf("control fixture %s is missing; the scan read %v", name, joinScannedPaths(control.syntax))
	}
	return path
}

// The gate. Test files are in scope as well as production ones, this one included: a
// test that rebuilds the join is a second implementation of it, and the assertion it
// then makes about the wire is an assertion about itself. Nothing is exempt but the file
// that is allowed to cross, and that exemption is counted.
func TestClassBucketJoinIsConfinedToRecordGo(t *testing.T) {
	roots := joinScanRoots(t)
	scan := mustScanJoinSources(t, roots)
	paths := joinPathsUnderGate(t, scan, roots)
	if used := joinAllowanceUsed(paths, joinAllowedPaths); !slices.Equal(used, slices.Sorted(slices.Values(joinAllowedPaths))) {
		t.Errorf("the allowance let %v past, want exactly %v", used, joinAllowedPaths)
	}
	t.Logf("%d files under the gate, %v across the roots %v", len(paths), scan.countsByRoot(), roots)
	for _, shape := range classBucketJoinShapes {
		violations := joinViolations(scan, paths, shape, joinAllowedPaths)
		for _, path := range joinScannedPaths(violations) {
			t.Errorf("%s %s the retention class and the eph bucket in the shape %s; only %s may: %v",
				path, shape.half.verb(), shape.name, strings.Join(joinAllowedPaths, " and "), violations[path])
		}
	}
}

// The allowance is only worth having if the crossing really is in the file it names. A
// record.go that had stopped joining — because the crossing moved to a helper the rules
// do not look at, or because it was written in some shape none of them cover — would
// leave the gate above passing over a tree where the rule no longer holds. Both halves
// are required: the file allowed to join is the file allowed to split, and a split that
// moved out from under the allowance is as silent as a join that did.
func TestTheAllowedFileActuallyCrossesBothWays(t *testing.T) {
	scan := mustScanJoinSources(t, []string{messageRoot})
	for _, allowed := range joinAllowedPaths {
		if _, scanned := scan.syntax[allowed]; !scanned {
			t.Fatalf("%s is allowed to cross but the scan did not read it; it read %v", allowed, joinScannedPaths(scan.syntax))
		}
		committed := map[joinHalf][]string{}
		for _, shape := range classBucketJoinShapes {
			if 0 < len(joinShapeExpressions(scan, allowed, shape)) {
				committed[shape.half] = append(committed[shape.half], shape.name)
			}
		}
		for _, half := range []joinHalf{joinsTheByte, splitsTheByte} {
			if len(committed[half]) == 0 {
				t.Errorf("%s is the only file allowed to cross between the class and the bucket, and nothing in it %s them in any of the shapes %v; either that half moved or the rules no longer see it",
					allowed, half.verb(), shapeNames(half))
			}
		}
		t.Logf("%s joins in %v and splits in %v", allowed, committed[joinsTheByte], committed[splitsTheByte])
	}
}

// The allowance names a path and not a base name, exercised rather than asserted.
//
// This is one line of the gate, the line that decides whether a file is exempt, and it
// is the whole of the gate's surface area, so it is tested against a file that is not
// there rather than trusted to be right. connect/mls is free to grow a record.go
// tomorrow; on a base name it would inherit the exemption of a file in another package
// and every rule would stop looking at it, silently, because an exemption reports
// nothing. The same fixture is asked about twice, under the path the allowance names and
// under a path that merely shares its base name, and the two answers have to differ.
func TestTheAllowanceIsAPathAndNotABaseName(t *testing.T) {
	control := mustScanJoinSources(t, []string{joinControlDir})
	fixture := joinControlPath(t, control, "join.go")
	for _, allowed := range joinAllowedPaths {
		elsewhere := mlsRoot + "/" + filepath.Base(allowed)
		for _, path := range []string{allowed, elsewhere} {
			control.sourceTexts[path] = control.sourceTexts[fixture]
			control.syntax[path] = control.syntax[fixture]
			control.rootOf[path] = messageRoot
		}
		for _, shape := range classBucketJoinShapes {
			if violations := joinViolations(control, []string{allowed}, shape, joinAllowedPaths); 0 < len(violations) {
				t.Errorf("%s is the file the allowance names and it was reported for %s: %v", allowed, shape.name, violations)
			}
			if violations := joinViolations(control, []string{elsewhere}, shape, joinAllowedPaths); len(violations) == 0 {
				t.Errorf("%s commits %s and the allowance let it past; only %v is exempt, and %s is another package's file that happens to share a name",
					elsewhere, shape.name, joinAllowedPaths, elsewhere)
			}
		}
	}
}

// The positive control. Every rule must fire on the fixture that commits its shape, and
// the confinement check must report that fixture and nothing else, so a rule that
// stopped matching fails here rather than issuing the tree a clean bill.
func TestJoinRulesFlagTheControlFixture(t *testing.T) {
	// spec A section 5.1 enumerates the join half, so that half is pinned to its four
	// names; the split half is this file's own reading of the same sentence and only has
	// to exist
	if want := []string{"16+bucket", "class*16", "class<<4", "class|bucket"}; !slices.Equal(shapeNames(joinsTheByte), want) {
		t.Fatalf("the join half bans %v, want the %v of spec A section 5.1", shapeNames(joinsTheByte), want)
	}
	if len(shapeNames(splitsTheByte)) == 0 {
		t.Fatal("nothing bans the split half, and the split is the join read backwards")
	}
	control := mustScanJoinSources(t, []string{joinControlDir})
	paths := joinScannedPaths(control.syntax)
	fixture := joinControlPath(t, control, "join.go")
	for _, shape := range classBucketJoinShapes {
		expressions := joinShapeExpressions(control, fixture, shape)
		if len(expressions) == 0 {
			t.Errorf("the rule for %s found nothing in the control fixture, so it is no longer a gate", shape.name)
			continue
		}
		t.Logf("%s matched %v", shape.name, expressions)
		violations := joinViolations(control, paths, shape, joinAllowedPaths)
		if !slices.Equal(joinScannedPaths(violations), []string{fixture}) {
			t.Errorf("the confinement check reported %v for %s, want only the fixture that commits it", joinScannedPaths(violations), shape.name)
		}
	}
}

// The negative half of the control: a fixture that writes every shape in prose, puts one
// of those sentences on the end of a line of working code, and commits none of them,
// must be reported by nothing. Without it, a rule that answered yes to every expression
// would pass the positive control above.
func TestJoinRulesIgnoreTheDocumentedFixture(t *testing.T) {
	control := mustScanJoinSources(t, []string{joinControlDir})
	fixture := joinControlPath(t, control, "documented.go")
	for _, shape := range classBucketJoinShapes {
		if expressions := joinShapeExpressions(control, fixture, shape); 0 < len(expressions) {
			t.Errorf("the rule for %s fired on %v in the fixture that only writes about the shapes", shape.name, expressions)
		}
	}
	// and it has to actually contain them, or it controls nothing
	text := control.sourceTexts[fixture]
	for _, shape := range classBucketJoinShapes {
		if !strings.Contains(text, shape.name) {
			t.Errorf("the documented fixture does not mention %s, so it controls nothing", shape.name)
		}
	}
}

// The coverage guarantee, exercised rather than assumed. A root that is not there and a
// root holding no go source both have to be refused: either one hands every gate above a
// clean result it did not earn. The last two are the case that actually bites — a second
// root that reads nothing while the first reads plenty, which a scan wide total would
// never notice.
func TestJoinScanRefusesARootItCannotCover(t *testing.T) {
	uncoveredRootSets := [][]string{
		{},
		{"../this-package-does-not-exist"},
		{"../mls/testdata/vectors"},
		{messageRoot, "../this-package-does-not-exist"},
		{messageRoot, "../mls/testdata/vectors"},
	}
	for _, roots := range uncoveredRootSets {
		if _, err := scanJoinSources(roots); err == nil {
			t.Errorf("scanning %v succeeded; a root that contributes no source must be refused", roots)
		}
	}
	// and the real roots must pass it, or the refusal above is just "everything fails"
	if _, err := scanJoinSources(joinScanRoots(t)); err != nil {
		t.Errorf("scanning the real roots failed: %v", err)
	}
}

// A file the parser cannot read is refused too. Every rule below the scan reads a syntax
// tree, so a file that produced none is a file nothing was asked about — and skipping it
// quietly is how a whole root becomes invisible one file at a time.
//
// The root holds a file that parses as well as the one that does not, and the error has
// to name the broken one. Without the file that parses this answers for the wrong
// reason: a skipped file leaves the root contributing nothing, the empty root rule fires,
// and the scan is refused whether or not anything ever looked at the parse. With it, the
// parse is the only thing left that can fail.
//
// The broken file is written here rather than committed: a go file that does not parse,
// sitting in the tree, is something every formatter and every editor reports forever.
func TestJoinScanRefusesAFileItCannotParse(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "good.go"), []byte("package good\n"), 0o600); err != nil {
		t.Fatalf("writing the fixture that parses: %v", err)
	}
	if _, err := scanJoinSources([]string{root}); err != nil {
		t.Fatalf("a root holding one file that parses was refused: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "broken.go"), []byte("package good\n\nfunc ("), 0o600); err != nil {
		t.Fatalf("writing the unparseable fixture: %v", err)
	}
	_, err := scanJoinSources([]string{root})
	if err == nil {
		t.Fatal("a root holding a file the parser refused scanned clean")
	}
	if !strings.Contains(err.Error(), "broken.go") {
		t.Errorf("the scan was refused with %v, which does not name the file that would not parse", err)
	}
}

// What the gate actually read, reported rather than trusted. The bookkeeping check is the
// part the scan itself does not do: a per root attribution that no longer adds up to the
// collected set means files are being counted for a root that did not supply them.
func TestJoinScanCoversEveryRoot(t *testing.T) {
	roots := joinScanRoots(t)
	scan := mustScanJoinSources(t, roots)
	counts := scan.countsByRoot()
	total := 0
	for _, root := range roots {
		t.Logf("root %s contributed %d go files", root, counts[root])
		total += counts[root]
	}
	if len(scan.syntax) != total {
		t.Errorf("the scan holds %d files while the roots counted %d", len(scan.syntax), total)
	}
	if len(counts) != len(roots) {
		t.Errorf("%d roots contributed files, want %d", len(counts), len(roots))
	}
}

// The sdk root has to be a path a checkout can actually put sdk in. It is a sibling
// repository of connect, so a root that resolves inside connect is one sdk will never be
// at — and because a missing root is logged rather than failed, an unreachable one is a
// gate that promises to cover sdk in a message and never does. This is the assertion the
// log line cannot make for itself.
func TestTheSdkRootIsASiblingOfThisModule(t *testing.T) {
	sdkRoot := joinSdkRoot(t)
	moduleRoot, err := filepath.Abs(filepath.Join(messageRoot, ".."))
	if err != nil {
		t.Fatalf("resolving the module root: %v", err)
	}
	sdkAbsolute, err := filepath.Abs(sdkRoot)
	if err != nil {
		t.Fatalf("resolving %s: %v", sdkRoot, err)
	}
	if strings.HasPrefix(sdkAbsolute, moduleRoot+string(filepath.Separator)) {
		t.Errorf("the sdk root %s resolves inside %s; sdk is a sibling repository and no checkout puts it there", sdkAbsolute, moduleRoot)
	}
	// and when the sibling really is checked out, the gate has to be covering it
	entry, err := os.Stat(sdkRoot)
	if err != nil || !entry.IsDir() {
		t.Skipf("sdk is not checked out beside connect at %s, so there is nothing to cover", sdkAbsolute)
	}
	if !slices.Contains(joinScanRoots(t), sdkRoot) {
		t.Errorf("%s is checked out and the gate does not cover it", sdkAbsolute)
	}
}

// The fixture is a file full of real joins and splits, so the gate must be unable to see
// it. If a directory named testdata ever stopped being skipped, the gate would fail on
// the control instead of on the code, which is loud but misleading; this names the
// reason.
func TestJoinScanSkipsTestdata(t *testing.T) {
	scan := mustScanJoinSources(t, joinScanRoots(t))
	for _, path := range joinScannedPaths(scan.syntax) {
		if strings.HasPrefix(path, "testdata/") || strings.Contains(path, "/testdata/") {
			t.Errorf("the gate read %s; the control fixture and vendored corpora must stay out of scope", path)
		}
	}
}
