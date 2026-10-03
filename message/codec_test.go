// The record codec: the layout, and the properties that keep two implementations of it
// from drifting apart without anybody noticing.
//
// Four things are observed here and they fail in different directions.
//
// The first is that the bytes are the layout codec.go's table states. A round trip
// cannot see this — an encoder and a decoder that agree on a permuted layout round trip
// perfectly and agree with nobody — so the layout is written down a second time, in
// rawRecord below, and every corpus record's encoding is compared against it. rawRecord
// is also how this file reaches the records the encoder cannot produce: an is_commit
// byte of 2, a blob id one byte short, a ct_body that is not its rung. rawRecord lives
// beside the encoder, though, so a permutation applied to both at once passes it; the two
// anchors that do not move with the code are the pinned records, one on the blob rung and
// one carrying a body, each a hexadecimal string derived by hand from the table and
// asserted byte for byte. Between them every field is pinned in both of the shapes it
// takes.
//
// The second is that the round trip is byte exact over a corpus that is a cross product
// rather than a list. The axes are the retention wire alphabet, the size ladder, the
// presence of a server attachment, a zero and a non zero write_auth, a present and an
// absent ct_body, and the u64 boundaries on the three 64 bit fields — and the first two
// are read out of the parser itself rather than written down, so a parser that widened
// tomorrow widens the corpus and is caught by the one test that pins the alphabets
// against the spec.
//
// The third is that nothing is silently accepted and changed. Every single byte
// truncation of a valid record is refused, every trailing byte is refused, and every
// single byte corruption either is refused or re-encodes to exactly the corrupted bytes —
// over the fixed region across the whole class and ladder cross product, and over the
// variable region, where the four length prefixes and the framing live, on one record per
// rung. That last one is spec A section 5.8 rule 4 and it is the property that catches a
// field read at the wrong width, which is the one defect a round trip over well formed
// records cannot see.
//
// The fourth is that the two entry points are one parser. Every byte string this file
// constructs — corpus, truncation, corruption, hand built refusal — goes through
// parseBoth, which asks both ParseRecord and ParseRecordHeader and fails if they disagree
// about the record's existence or about its header. The agreement is therefore asserted
// over every input any test here ever builds rather than over a list someone remembered
// to extend.
//
// The fifth is the fuzz target and the corpus checked in beside it. Every byte string above
// was chosen by something in this file; the corpus carries the near miss framings a byte
// walk does not produce — a prefix declaring more than the input holds, two prefixes swapped
// between fields — and a plain go test replays them. It is asserted to exist, to hold both
// accepted and refused entries, and to satisfy the same property, so it cannot quietly
// become a directory nothing reads.
//
// One property is deliberately not here. That the set of records EncodeRecord will write and
// the set ParseRecord will read are the same set is codec_agreement_test.go's, asserted over
// a space computed from these same alphabets, with the refusals it has to reach derived from
// the call graph rather than from a list.
package message

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/urnetwork/connect/mls/syntax"
)

// ── the layout, written down a second time ──────────────────────────────────────────

// The offset the first length prefixed field begins at: the sum of the fixed width
// fields at the top of codec.go's table, written as that sum so a reader can check it
// against the table term by term rather than against a number.
const recordFixedRegionBytes = 1 + 32 + 16 + 8 + 8 + 1 + 1 + 8 + 1 + 8 + 32

// The four length prefixes of a record whose four variable fields are all empty, and that
// plus the trailing mac, which is everything after the fixed region on such a record.
const (
	recordEmptyPrefixBytes = 4 + 4 + 4 + 4
	recordEmptyTailBytes   = recordEmptyPrefixBytes + 32
)

// A record as raw wire values, in codec.go's field order.
//
// This is the layout stated independently of the code under test, and it is what makes
// the encoding pinnable at all: swap two same width fields in both EncodeRecord and
// decodeRecord and every round trip in this file still passes, because the package would
// agree with itself perfectly and with no other implementation. It is also the only way
// to build the records EncodeRecord cannot — the whole point of an is_commit byte of 2 is
// that a go bool cannot express it.
type rawRecord struct {
	formatVersion    uint8
	groupId          []byte
	senderHandle     []byte
	epoch            uint64
	streamIndex      uint64
	isCommit         uint8
	retentionWire    uint8
	ephWindow        uint64
	sizeBucket       uint8
	expireAt         uint64
	bodyHash         []byte
	blobId           []byte
	serverAttachment []byte
	ctHead           []byte
	ctBody           []byte
	writeAuth        []byte
}

// The bytes this raw record is. It writes what it is given, including lengths the codec
// would refuse, which is what it is for.
func (self rawRecord) encode(t testing.TB) []byte {
	t.Helper()
	writer := syntax.NewWriter()
	writer.WriteUint8(self.formatVersion)
	writer.WriteRaw(self.groupId)
	writer.WriteRaw(self.senderHandle)
	writer.WriteUint64(self.epoch)
	writer.WriteUint64(self.streamIndex)
	writer.WriteUint8(self.isCommit)
	writer.WriteUint8(self.retentionWire)
	writer.WriteUint64(self.ephWindow)
	writer.WriteUint8(self.sizeBucket)
	writer.WriteUint64(self.expireAt)
	writer.WriteRaw(self.bodyHash)
	writer.WriteOpaqueLP(self.blobId)
	writer.WriteOpaqueLP(self.serverAttachment)
	writer.WriteOpaqueLP(self.ctHead)
	writer.WriteOpaqueLP(self.ctBody)
	writer.WriteRaw(self.writeAuth)
	bs, err := writer.Bytes()
	if err != nil {
		t.Fatalf("the layout encoder refused to write a raw record: %v", err)
	}
	return bs
}

// The raw form of a record, for comparing what EncodeRecord produced against what the
// layout says it should have. The retention byte comes from record.go's join, because
// this file is not one of the two places allowed to compute it.
func rawRecordOf(t testing.TB, r *Record) rawRecord {
	t.Helper()
	retentionWire, err := RetentionClassWire(r.Header.RetentionClass, r.Header.EphBucket)
	if err != nil {
		t.Fatalf("the join refused class %d bucket %d: %v", r.Header.RetentionClass, r.Header.EphBucket, err)
	}
	return rawRecordFields(r, retentionWire)
}

// The same mapping with the retention byte supplied, for a caller that has already asked
// the join for it and has something of its own to say about the answer. One function
// rather than two copies of the field list, because the field list is the layout and a
// second copy of the layout in this file would be a second thing to keep in step with
// codec.go's table.
func rawRecordFields(r *Record, retentionWire byte) rawRecord {
	return rawRecord{
		formatVersion:    recordFormatVersion,
		groupId:          r.Header.GroupId[:],
		senderHandle:     r.Header.SenderHandle[:],
		epoch:            r.Header.Epoch,
		streamIndex:      r.Header.StreamIndex,
		isCommit:         isCommitByte(r.Header.IsCommit),
		retentionWire:    retentionWire,
		ephWindow:        r.Header.EphWindow,
		sizeBucket:       byte(r.Header.SizeBucket),
		expireAt:         r.Header.ExpireAt,
		bodyHash:         r.Header.BodyHash[:],
		blobId:           r.Header.BlobId,
		serverAttachment: r.Header.ServerAttachment,
		ctHead:           r.CtHead,
		ctBody:           r.CtBody,
		writeAuth:        r.WriteAuth[:],
	}
}

// ── the alphabets, derived from the parser ──────────────────────────────────────────

// What a size bucket requires of the blob id, as the parser answers it.
type sizeBucketShape struct {
	needsBlobId bool
}

// The size buckets the parser admits, and which of them requires a blob id, derived by
// offering it all 256 values in both shapes a record can take — with a 32 byte blob id
// and without one — rather than by writing the six rungs down. Everything below reads its
// ladder out of this, so a parser that admitted a seventh rung would carry it into every
// assertion that follows and be caught by the one test that pins the set.
//
// Probing in both shapes is what keeps this from presuming which rung is the blob rung:
// the blob rule is read off the parser's answers rather than assumed and then confirmed.
func acceptedSizeBuckets(t testing.TB) map[byte]sizeBucketShape {
	t.Helper()
	accepted := map[byte]sizeBucketShape{}
	for value := 0; value <= 0xFF; value++ {
		bare := probeRecord(byte(value), nil)
		withBlobId := probeRecord(byte(value), fillBytes(blobIdTag, blobIdBytes))
		_, bareErr := ParseRecord(bare.encode(t))
		_, blobErr := ParseRecord(withBlobId.encode(t))
		switch {
		case bareErr == nil && blobErr == nil:
			t.Fatalf("size bucket %d parses both with and without a blob id, so the blob rule says nothing", value)
		case bareErr == nil:
			accepted[byte(value)] = sizeBucketShape{needsBlobId: false}
		case blobErr == nil:
			accepted[byte(value)] = sizeBucketShape{needsBlobId: true}
		}
	}
	if len(accepted) == 0 {
		t.Fatal("the parser admitted no size bucket at all, so every assertion below would hold vacuously")
	}
	return accepted
}

// The smallest legal-shaped record carrying a given size bucket and blob id, for probing
// the ladder. Everything else about it is fixed and legal: a permanent class, a
// non commit, no body.
func probeRecord(sizeBucket byte, blobId []byte) rawRecord {
	return rawRecord{
		formatVersion: recordFormatVersion,
		groupId:       fillBytes(groupIdTag, 32),
		senderHandle:  fillBytes(senderHandleTag, 16),
		isCommit:      0,
		retentionWire: probeRetentionWire,
		sizeBucket:    sizeBucket,
		bodyHash:      fillBytes(bodyHashTag, 32),
		blobId:        blobId,
		writeAuth:     make([]byte, 32),
	}
}

// The retention byte the ladder probe uses. Read out of record.go's join rather than
// written as 0x00, so the probe cannot be the thing that is wrong when the ladder set
// comes back empty.
var probeRetentionWire = func() byte {
	wire, err := RetentionClassWire(RetentionPermanent, 0)
	if err != nil {
		panic(fmt.Sprintf("the join refused the permanent class: %v", err))
	}
	return wire
}()

// The size ladder of master section 8 and spec A section 5.1, written out once: every
// legal rung, and whether it carries a blob id instead of an inline body. The set of
// legal rungs is derived from the parser everywhere else in this file; this is the one
// place the MEANING of a rung is written down, for the same reason record_test.go writes
// the retention table down — a ladder permuted consistently agrees with itself and with
// nobody.
var masterSizeBucketTable = map[byte]sizeBucketShape{
	0: {needsBlobId: false},
	1: {needsBlobId: false},
	2: {needsBlobId: false},
	3: {needsBlobId: false},
	4: {needsBlobId: false},
	5: {needsBlobId: true},
}

// The rungs of the table above, sorted, as the written down alphabet every derived set is
// compared against.
func masterSizeBuckets() []byte {
	buckets := []byte{}
	for bucket := range masterSizeBucketTable {
		buckets = append(buckets, bucket)
	}
	slices.Sort(buckets)
	return buckets
}

// The sorted keys of a byte keyed map, so every loop in this file walks its alphabet in
// one order and a failure names the same case run to run.
func sortedByteKeys[V any](byKey map[byte]V) []byte {
	keys := []byte{}
	for key := range byKey {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// ── the corpus ──────────────────────────────────────────────────────────────────────

// The tags that make each fixed width field's filler distinct from every other's. Two
// same width fields filled with the same bytes are two fields a swapped encode order
// cannot be seen through, and group_id, body_hash and write_auth are all 32 bytes.
const (
	groupIdTag byte = iota + 1
	senderHandleTag
	bodyHashTag
	blobIdTag
	writeAuthTag
	serverAttachmentTag
	ctHeadTag
	ctBodyTag
)

// Deterministic filler. A pattern rather than a constant so that a field written or read
// at the wrong offset lands on bytes that are not the ones it should have, and a tag so
// that two fields of the same width never hold the same bytes.
func fillBytes(tag byte, n int) []byte {
	if n <= 0 {
		return nil
	}
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(i*31) ^ tag
	}
	return out
}

// The ct_body fillers, one per length, shared across every corpus record that wants that
// length. The corpus crosses six rungs with everything else and the top rung's body is
// 64 KiB, so building a fresh one per entry costs tens of megabytes for no property at
// all. Nothing mutates a corpus body.
var ctBodyFillers = map[int][]byte{}

func ctBodyFiller(n int) []byte {
	if n <= 0 {
		return nil
	}
	if filler, built := ctBodyFillers[n]; built {
		return filler
	}
	filler := fillBytes(ctBodyTag, n)
	ctBodyFillers[n] = filler
	return filler
}

// The four 64 bit fields, as one assignment. eph_window joined them on 2026-09-13 and is
// the reason there are four: it is a u64 in the fixed region like the other three, so a
// swap between it and expire_at is exactly what the cross below exists to see.
type u64Triple struct {
	epoch       uint64
	streamIndex uint64
	expireAt    uint64
	ephWindow   uint64
}

// The boundaries every 64 bit field is exercised over: zero, one, the last value that
// fits in 32 bits, the first that does not, and the top of the range. A field written or
// read at the wrong width — a u32 where the layout says u64, or the halves the wrong way
// round — disagrees on one of these and on nothing else.
func u64Boundaries() []uint64 {
	return []uint64{0, 1, 0xFFFFFFFF, 0x100000000, 0xFFFFFFFFFFFFFFFF}
}

// The boundaries rotated across the four fields, which is the axis the main corpus
// crosses with everything else. Rotation rather than one value in all three, because
// three fields holding the same number is three fields a swapped encode order round trips
// through unharmed.
func u64Rotations() []u64Triple {
	boundaries := u64Boundaries()
	rotations := []u64Triple{}
	for i := range boundaries {
		rotations = append(rotations, u64Triple{
			epoch:       boundaries[i],
			streamIndex: boundaries[(i+1)%len(boundaries)],
			expireAt:    boundaries[(i+2)%len(boundaries)],
			ephWindow:   boundaries[(i+3)%len(boundaries)],
		})
	}
	return rotations
}

// Every boundary against every boundary in all four fields. This is its own axis rather
// than part of the main cross product because crossing 625 assignments with six rungs,
// nine classes and four flags is half a million records for a property that is about the
// four fields alone.
func u64Crosses() []u64Triple {
	crosses := []u64Triple{}
	for _, epoch := range u64Boundaries() {
		for _, streamIndex := range u64Boundaries() {
			for _, expireAt := range u64Boundaries() {
				for _, ephWindow := range u64Boundaries() {
					crosses = append(crosses, u64Triple{epoch: epoch, streamIndex: streamIndex, expireAt: expireAt, ephWindow: ephWindow})
				}
			}
		}
	}
	return crosses
}

// One corpus record and the name a failure reports it by.
type corpusEntry struct {
	name   string
	record Record
}

// The corpus: the cross product of every axis, computed rather than written out.
//
// The two alphabets — the retention wire bytes and the size ladder — are read out of the
// package under test, so this set widens with the parser rather than lagging behind it.
// The remaining axes are the ones spec A section 5.1 and section 2.4 name as the shapes a
// record actually takes: with and without a server attachment, with a zero and a non zero
// write_auth, with and without a ct_body, commit and not, across the u64 boundaries.
func recordCorpus(t testing.TB) []corpusEntry {
	t.Helper()
	classes := acceptedWireBytes(t)
	buckets := acceptedSizeBuckets(t)
	entries := []corpusEntry{}
	for _, retentionWire := range sortedByteKeys(classes) {
		pair := classes[retentionWire]
		for _, sizeBucket := range sortedByteKeys(buckets) {
			shape := buckets[sizeBucket]
			bodyLengths := []int{0}
			if length := SizeBucketCtBodyBytes(SizeBucket(sizeBucket)); 0 <= length {
				bodyLengths = append(bodyLengths, length)
			}
			for _, attached := range []bool{false, true} {
				for _, authed := range []bool{false, true} {
					for _, isCommit := range []bool{false, true} {
						for _, bodyLength := range bodyLengths {
							for rotation, triple := range u64Rotations() {
								name := fmt.Sprintf("class=0x%02x bucket=%d attached=%v authed=%v commit=%v body=%d u64=%d",
									retentionWire, sizeBucket, attached, authed, isCommit, bodyLength, rotation)
								entries = append(entries, corpusEntry{
									name:   name,
									record: corpusRecord(pair, sizeBucket, shape, attached, authed, isCommit, bodyLength, triple),
								})
							}
						}
					}
				}
			}
		}
	}
	// the three 64 bit fields fully crossed, on the smallest record the ladder admits.
	smallest := sortedByteKeys(buckets)[0]
	for _, triple := range u64Crosses() {
		name := fmt.Sprintf("u64 epoch=%d stream=%d expire=%d window=%d", triple.epoch, triple.streamIndex, triple.expireAt, triple.ephWindow)
		entries = append(entries, corpusEntry{
			name:   name,
			record: corpusRecord(classes[probeRetentionWire], smallest, buckets[smallest], false, true, false, 0, triple),
		})
	}
	if len(entries) == 0 {
		t.Fatal("the corpus is empty, so every property asserted over it would hold vacuously")
	}
	return entries
}

// One corpus record from one point of the cross product.
func corpusRecord(
	pair classBucket,
	sizeBucket byte,
	shape sizeBucketShape,
	attached bool,
	authed bool,
	isCommit bool,
	bodyLength int,
	triple u64Triple,
) Record {
	record := Record{
		Header: RecordHeader{
			Epoch:          triple.epoch,
			StreamIndex:    triple.streamIndex,
			IsCommit:       isCommit,
			RetentionClass: pair.class,
			EphBucket:      pair.bucket,
			EphWindow:      triple.ephWindow,
			SizeBucket:     SizeBucket(sizeBucket),
			ExpireAt:       triple.expireAt,
		},
		CtHead: fillBytes(ctHeadTag, 96),
		CtBody: ctBodyFiller(bodyLength),
	}
	copy(record.Header.GroupId[:], fillBytes(groupIdTag, 32))
	copy(record.Header.SenderHandle[:], fillBytes(senderHandleTag, 16))
	copy(record.Header.BodyHash[:], fillBytes(bodyHashTag, 32))
	if shape.needsBlobId {
		record.Header.BlobId = fillBytes(blobIdTag, blobIdBytes)
	}
	if attached {
		record.Header.ServerAttachment = fillBytes(serverAttachmentTag, 40)
	}
	if authed {
		copy(record.WriteAuth[:], fillBytes(writeAuthTag, 32))
	}
	return record
}

// The corpus entries whose encodings are short enough to walk byte by byte: the ones with
// no inline body. Derived by filtering the corpus rather than by building a second one,
// so an axis added above reaches these tests too. The property they carry is about the
// framing, which the body's length does not change — and walking every prefix of a 64 KiB
// record would be sixty five thousand parses for each of hundreds of entries.
func shortCorpus(t testing.TB) []corpusEntry {
	t.Helper()
	short := []corpusEntry{}
	for _, entry := range recordCorpus(t) {
		if len(entry.record.CtBody) == 0 {
			short = append(short, entry)
		}
	}
	if len(short) == 0 {
		t.Fatal("no corpus record has an absent body, so the byte walks below would hold vacuously")
	}
	return short
}

// One corpus entry per retention byte and size bucket pair, for the tests that try all
// 255 alternatives at every offset of the fixed region and cannot afford to do it
// hundreds of times over. Derived by grouping the corpus, so the subset covers every
// value of both alphabets by construction rather than by a promise.
func fixedRegionCorpus(t testing.TB) []corpusEntry {
	t.Helper()
	seen := map[string]bool{}
	subset := []corpusEntry{}
	for _, entry := range shortCorpus(t) {
		key := fmt.Sprintf("%d/%d/%d", entry.record.Header.RetentionClass, entry.record.Header.EphBucket, entry.record.Header.SizeBucket)
		if seen[key] {
			continue
		}
		seen[key] = true
		subset = append(subset, entry)
	}
	if len(subset) == 0 {
		t.Fatal("the fixed region subset is empty, so every property asserted over it would hold vacuously")
	}
	return subset
}

// One record per rung, with every length prefixed field populated, plus one carrying an
// inline body: the corpus for the walk over the variable region.
//
// Every field of the variable region has to be present in at least one of these or the
// walk never reaches its bytes, which is why the attachment and the write_auth are on and
// why the blob rung is in the set — its blob id is the only 32 byte length prefixed field
// the layout has. The rungs come from the parser like every other alphabet in this file, so
// a rung added tomorrow is walked without anything here being edited.
func variableRegionCorpus(t testing.TB) []corpusEntry {
	t.Helper()
	shapes := acceptedSizeBuckets(t)
	classes := acceptedWireBytes(t)
	entries := []corpusEntry{}
	for _, sizeBucket := range sortedByteKeys(shapes) {
		entries = append(entries, corpusEntry{
			name:   fmt.Sprintf("bucket=%d body=0", sizeBucket),
			record: corpusRecord(classes[probeRetentionWire], sizeBucket, shapes[sizeBucket], true, true, false, 0, u64Triple{}),
		})
	}
	// the smallest rung that carries an inline body, so the walk reaches ct_body's own
	// bytes and the prefix that frames them without walking 64 KiB of them.
	for _, sizeBucket := range sortedByteKeys(shapes) {
		length := SizeBucketCtBodyBytes(SizeBucket(sizeBucket))
		if length < 0 {
			continue
		}
		entries = append(entries, corpusEntry{
			name:   fmt.Sprintf("bucket=%d body=%d", sizeBucket, length),
			record: corpusRecord(classes[probeRetentionWire], sizeBucket, shapes[sizeBucket], true, true, false, length, u64Triple{}),
		})
		break
	}
	if len(entries) == 0 {
		t.Fatal("the variable region corpus is empty, so the walk below would hold vacuously")
	}
	bodies := 0
	for _, entry := range entries {
		if 0 < len(entry.record.CtBody) {
			bodies++
		}
	}
	if bodies == 0 {
		t.Fatal("no record in the variable region corpus carries a body, so ct_body's own bytes are never walked")
	}
	return entries
}

// ── the two entry points, asked together ────────────────────────────────────────────

// Every byte string this file hands to the parser goes through here.
//
// ParseRecord and ParseRecordHeader are one decode routine in codec.go, and this is what
// holds them to it: the agreement is asserted over every input any test in this file
// constructs — every corpus record, every truncation, every corruption, every hand built
// refusal — rather than over a list of inputs someone remembered to extend. A header
// parser that skipped one of ParseRecord's checks would disagree here on the first input
// that check refuses.
func parseBoth(t testing.TB, what string, bs []byte) (*Record, error) {
	t.Helper()
	record, recordErr := ParseRecord(bs)
	header, headerErr := ParseRecordHeader(bs)
	if (recordErr == nil) != (headerErr == nil) {
		t.Fatalf("%s: ParseRecord says %v and ParseRecordHeader says %v; the two entry points disagree about whether this record exists", what, recordErr, headerErr)
	}
	if recordErr != nil {
		if recordErr.Error() != headerErr.Error() {
			t.Fatalf("%s: ParseRecord refused with %q and ParseRecordHeader with %q", what, recordErr, headerErr)
		}
		return nil, recordErr
	}
	if difference := headerDifference(&record.Header, header); difference != "" {
		t.Fatalf("%s: the two entry points parsed different headers: %s", what, difference)
	}
	return record, nil
}

// What differs between two headers, or the empty string. A field by field comparison
// rather than reflect.DeepEqual, so a failure names the field.
func headerDifference(left *RecordHeader, right *RecordHeader) string {
	switch {
	case left.GroupId != right.GroupId:
		return "group_id"
	case left.SenderHandle != right.SenderHandle:
		return "sender_handle"
	case left.Epoch != right.Epoch:
		return "epoch"
	case left.StreamIndex != right.StreamIndex:
		return "stream_index"
	case left.IsCommit != right.IsCommit:
		return "is_commit"
	case left.RetentionClass != right.RetentionClass:
		return "retention_class"
	case left.EphBucket != right.EphBucket:
		return "eph_bucket"
	case left.EphWindow != right.EphWindow:
		return "eph_window"
	case left.SizeBucket != right.SizeBucket:
		return "size_bucket"
	case left.ExpireAt != right.ExpireAt:
		return "expire_at"
	case left.BodyHash != right.BodyHash:
		return "body_hash"
	case !bytes.Equal(left.BlobId, right.BlobId):
		return "blob_id"
	case !bytes.Equal(left.ServerAttachment, right.ServerAttachment):
		return "server_attachment"
	}
	return ""
}

// What differs between two records, or the empty string.
func recordDifference(left *Record, right *Record) string {
	if difference := headerDifference(&left.Header, &right.Header); difference != "" {
		return difference
	}
	switch {
	case !bytes.Equal(left.CtHead, right.CtHead):
		return "ct_head"
	case !bytes.Equal(left.CtBody, right.CtBody):
		return "ct_body"
	case left.WriteAuth != right.WriteAuth:
		return "write_auth"
	}
	return ""
}

// The encoding of a corpus record, with a refusal fatal: a corpus this package cannot
// encode is a broken corpus, not a finding.
func mustEncode(t testing.TB, what string, r *Record) []byte {
	t.Helper()
	bs, err := EncodeRecord(r)
	if err != nil {
		t.Fatalf("%s: EncodeRecord refused a corpus record: %v", what, err)
	}
	return bs
}

// ── the alphabets are the ones the specs name ───────────────────────────────────────

// The size ladder: the six rungs of master section 8 and nothing else, and the blob rung
// is the one that carries a blob id instead of a body. The set under test is computed by
// offering the parser all 256 values in both shapes; the set it is compared against is the
// table above, so a widening or a narrowing of the parser moves the computed set off the
// one place the spec is written down.
func TestSizeBucketAlphabetIsTheSixRungsMasterSection8Names(t *testing.T) {
	accepted := acceptedSizeBuckets(t)
	got := sortedByteKeys(accepted)
	want := masterSizeBuckets()
	if len(want) != 6 {
		t.Fatalf("the size table names %d rungs, want the 6 of master section 8", len(want))
	}
	if !slices.Equal(got, want) {
		t.Errorf("the parser admits size buckets %v, want exactly %v", got, want)
	}
	for _, bucket := range want {
		shape, admitted := accepted[bucket]
		if !admitted {
			t.Errorf("size bucket %d is a rung of master section 8 and the parser refuses it", bucket)
			continue
		}
		if shape != masterSizeBucketTable[bucket] {
			t.Errorf("size bucket %d needs a blob id: %v, want %v", bucket, shape.needsBlobId, masterSizeBucketTable[bucket].needsBlobId)
		}
	}
	for bucket, shape := range accepted {
		if _, named := masterSizeBucketTable[bucket]; !named {
			t.Errorf("the parser admits size bucket %d (blob id: %v) and master section 8 names no such rung", bucket, shape.needsBlobId)
		}
	}
}

// ── the layout ──────────────────────────────────────────────────────────────────────

// Where the fixed region ends, which is the one number every byte walk below is expressed
// in. Derived from the encoder — a record whose four variable fields are all empty is the
// fixed region, four empty length prefixes and the mac — and compared against the sum of
// codec.go's table. A field that changed width moves the two apart.
func TestTheFixedRegionEndsWhereTheLayoutTableSaysItDoes(t *testing.T) {
	buckets := acceptedSizeBuckets(t)
	smallest := sortedByteKeys(buckets)[0]
	record := corpusRecord(acceptedWireBytes(t)[probeRetentionWire], smallest, buckets[smallest], false, false, false, 0, u64Triple{})
	record.CtHead = nil
	bs := mustEncode(t, "the empty record", &record)
	if len(bs) != recordFixedRegionBytes+recordEmptyTailBytes {
		t.Fatalf("a record with no variable field is %d bytes, want %d: %d of fixed region plus %d of empty prefixes and the mac",
			len(bs), recordFixedRegionBytes+recordEmptyTailBytes, recordFixedRegionBytes, recordEmptyTailBytes)
	}
	for offset := recordFixedRegionBytes; offset < recordFixedRegionBytes+recordEmptyPrefixBytes; offset++ {
		if bs[offset] != 0 {
			t.Errorf("byte %d is 0x%02x, want a zero: the four empty length prefixes begin at %d", offset, bs[offset], recordFixedRegionBytes)
		}
	}
}

// The bytes are the layout. Every corpus record's encoding is compared against the same
// record written out by rawRecord, which states the layout independently of the code under
// test — which is the only thing that can see a permutation the encoder and the decoder
// agree on, or a field written with the wrong prefix.
func TestEncodedBytesAreTheLayoutTheTableStates(t *testing.T) {
	for _, entry := range recordCorpus(t) {
		got := mustEncode(t, entry.name, &entry.record)
		want := rawRecordOf(t, &entry.record).encode(t)
		if !bytes.Equal(got, want) {
			t.Fatalf("%s: EncodeRecord produced %d bytes and the layout says %d\n got: %s\nwant: %s",
				entry.name, len(got), len(want), hex.EncodeToString(got), hex.EncodeToString(want))
		}
	}
}

// The instant the pinned eph vector's window is computed from, in unix milliseconds. It is
// the same number that vector's expire_at carries -- 2024-05-09T10:08:45.568Z -- which is
// convenient rather than meaningful: the two fields are independent, and what matters is
// that the instant is written down so the window beside it can be recomputed rather than
// copied.
const pinnedEphSentAtMs uint64 = 0x0000018F5CD3A600

// The window that instant falls in on the eph bucket 5 rung, written out as the number the
// wire carries. TestThePinnedEphWindowIsTheRulingsArithmetic recomputes it from the ruling's
// own formula and from record.go's ladder, so this literal is held against an arithmetic
// statement rather than against the encoder that consumes it.
const pinnedEphWindow uint64 = 709

// The pinned window is what the 2026-09-13 ruling's formula answers, and not a number taken
// off the encoder.
//
// THIS IS WHAT MAKES THE VECTOR EVIDENCE. A golden vector generated by the encoder it checks
// proves nothing, and the two vectors below defend against that in two different ways: the
// OCTET POSITIONS are defended by being written out by hand from owner decision 60's list,
// field by field, with each line naming its field; the VALUE in the new field is defended
// here, by recomputing it. floor(sent_at_ms / (eph_bucket_seconds[b] * 1000)) for b = 5 is
// master section 8.1's own arithmetic, the seconds come from record.go's ladder rather than
// from a 2419200 written down twice, and a ladder that moved would move this answer and fail
// here rather than agreeing with a vector nobody can check.
func TestThePinnedEphWindowIsTheRulingsArithmetic(t *testing.T) {
	const bucket = 5
	seconds := EphBucketSeconds(bucket)
	if seconds <= 0 {
		t.Fatalf("eph bucket %d answers %d seconds, so the window arithmetic below has no divisor", bucket, seconds)
	}
	want := pinnedEphSentAtMs / (uint64(seconds) * 1000)
	if want != pinnedEphWindow {
		t.Errorf("floor(%d / (%d * 1000)) is %d and the vector pins %d", pinnedEphSentAtMs, seconds, want, pinnedEphWindow)
	}
	// and the two neighbouring instants land in the two neighbouring windows, which is what
	// says the divisor is the rung's and not some other number that happens to agree here
	lower := uint64(seconds) * 1000 * pinnedEphWindow
	if lower/(uint64(seconds)*1000) != pinnedEphWindow {
		t.Errorf("the first millisecond of window %d does not fall in it", pinnedEphWindow)
	}
	if (lower-1)/(uint64(seconds)*1000) != pinnedEphWindow-1 {
		t.Errorf("the millisecond before window %d falls in window %d, want %d",
			pinnedEphWindow, (lower-1)/(uint64(seconds)*1000), pinnedEphWindow-1)
	}
}

// One record, pinned to its exact bytes.
//
// The layout comparison above is written in this file beside the encoder, so a
// permutation applied to both at once passes it. This is the anchor that does not move
// with the code: a hexadecimal string, and the record it is the encoding of.
//
// The eph_window line is the one added on 2026-09-13. It sits between retention_class_wire
// and size_bucket, which is owner decision 60's amended position, and it is eight octets
// big endian -- 0x2c5 is 709 -- which is decision 60's generating rule applied to a u64: a
// field whose width is fixed by its go type encodes raw at that width. A builder that wrote
// it little endian produces c502000000000000 here, and one that wrote it after size_bucket
// produces the same octets one position later; neither survives this string.
func TestOneRecordIsPinnedToItsExactBytes(t *testing.T) {
	const want = "02" + // format_version: 0x02 since the eph_window field was added, and 0x01 is refused
		"0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20" + // group_id
		"a0a1a2a3a4a5a6a7a8a9aaabacadaeaf" + // sender_handle
		"0000000000000001" + // epoch
		"00000000ffffffff" + // stream_index
		"01" + // is_commit
		"15" + // retention_class_wire: eph bucket 5
		"00000000000002c5" + // eph_window: 709, big endian in eight octets, straight after the class
		"05" + // size_bucket: the blob rung
		"0000018f5cd3a600" + // expire_at
		"b0b1b2b3b4b5b6b7b8b9babbbcbdbebfc0c1c2c3c4c5c6c7c8c9cacbcccdcecf" + // body_hash
		"00000020" + "d0d1d2d3d4d5d6d7d8d9dadbdcdddedfe0e1e2e3e4e5e6e7e8e9eaebecedeeef" + // LP(blob_id)
		"00000004" + "deadbeef" + // LP(server_attachment)
		"00000008" + "0011223344556677" + // LP(ct_head)
		"00000000" + // LP(ct_body): absent, as it is on the read path
		"f0f1f2f3f4f5f6f7f8f9fafbfcfdfeff000102030405060708090a0b0c0d0e0f" // write_auth

	record := Record{
		Header: RecordHeader{
			Epoch:          1,
			StreamIndex:    0xFFFFFFFF,
			IsCommit:       true,
			RetentionClass: RetentionEph,
			EphBucket:      5,
			EphWindow:      pinnedEphWindow,
			SizeBucket:     SizeBucketBlob,
			ExpireAt:       0x0000018F5CD3A600,
			BlobId: []byte{
				0xd0, 0xd1, 0xd2, 0xd3, 0xd4, 0xd5, 0xd6, 0xd7, 0xd8, 0xd9, 0xda, 0xdb, 0xdc, 0xdd, 0xde, 0xdf,
				0xe0, 0xe1, 0xe2, 0xe3, 0xe4, 0xe5, 0xe6, 0xe7, 0xe8, 0xe9, 0xea, 0xeb, 0xec, 0xed, 0xee, 0xef,
			},
			ServerAttachment: []byte{0xde, 0xad, 0xbe, 0xef},
		},
		CtHead: []byte{0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77},
	}
	for i := range record.Header.GroupId {
		record.Header.GroupId[i] = byte(i + 1)
	}
	for i := range record.Header.SenderHandle {
		record.Header.SenderHandle[i] = byte(0xa0 + i)
	}
	for i := range record.Header.BodyHash {
		record.Header.BodyHash[i] = byte(0xb0 + i)
	}
	for i := range record.WriteAuth {
		record.WriteAuth[i] = byte(0xf0 + i)
	}

	// the layout's own arithmetic, term by term: a field written at the wrong width moves
	// the total even when every octet it wrote looks plausible
	const wantLength = 1 + 32 + 16 + 8 + 8 + 1 + 1 + 8 + 1 + 8 + 32 + (4 + 32) + (4 + 4) + (4 + 8) + (4 + 0) + 32
	if len(want) != 2*wantLength {
		t.Fatalf("the vector is %d octets and the layout adds up to %d", len(want)/2, wantLength)
	}
	got := mustEncode(t, "the pinned record", &record)
	if hex.EncodeToString(got) != want {
		t.Fatalf("the pinned record encodes to\n%s\nwant\n%s", hex.EncodeToString(got), want)
	}
	parsed, err := parseBoth(t, "the pinned record", got)
	if err != nil {
		t.Fatalf("the pinned record does not parse: %v", err)
	}
	if difference := recordDifference(&record, parsed); difference != "" {
		t.Errorf("the pinned record does not round trip: %s differs", difference)
	}
}

// A second record pinned to its exact bytes, and the one thing the first cannot pin: a
// present ct_body.
//
// The record above is on the blob rung, where ct_body is absent by rule, so its LP(ct_body)
// is four zero octets and its hex says nothing about where a body sits or how it is framed.
// A body only exists on the other five rungs, so it takes a second vector, and this is it:
// the smallest rung, its body exactly the rung's ciphertext length, and the other axes
// deliberately the opposite of the first vector's — a non eph class rather than eph, no
// server attachment rather than one, is_commit clear rather than set, expire_at unset
// rather than a timestamp — so between the two every field is pinned in both of the shapes
// it takes.
//
// Derived by hand from codec.go's table, field by field, and not by printing what
// EncodeRecord produced: a vector taken from the encoder pins the encoder to itself. Each
// line below is one row of that table. The body is the 272 bytes 0x00, 0x01 … 0xff, 0x00 …
// 0x0f, which is 256 for the rung plus the 16 byte aead tag, and it is a pattern rather
// than a constant so that a body written at the wrong offset lands on bytes that are not
// the ones it should have.
func TestOneRecordWithABodyIsPinnedToItsExactBytes(t *testing.T) {
	const want = "02" + // format_version
		"2122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f40" + // group_id
		"808182838485868788898a8b8c8d8e8f" + // sender_handle
		"0000000100000000" + // epoch: the first value that does not fit in 32 bits
		"ffffffffffffffff" + // stream_index: the top of the range
		"00" + // is_commit: clear
		"01" + // retention_class_wire: durable, a class that carries no bucket
		"0000000000000000" + // eph_window: the ZERO a non eph record carries, and it is still eight octets
		"00" + // size_bucket: the 256 B rung
		"0000000000000000" + // expire_at: unset
		"4142434445464748494a4b4c4d4e4f505152535455565758595a5b5c5d5e5f60" + // body_hash
		"00000000" + // LP(blob_id): absent, as it is on every rung but the blob one
		"00000000" + // LP(server_attachment): absent, an ordinary record
		"00000010" + // LP(ct_head) declares 16
		"909192939495969798999a9b9c9d9e9f" + // ct_head
		"00000110" + // LP(ct_body) declares 0x110 = 272 = 256 + the 16 byte aead tag
		"000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f" + // ct_body
		"202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f" +
		"404142434445464748494a4b4c4d4e4f505152535455565758595a5b5c5d5e5f" +
		"606162636465666768696a6b6c6d6e6f707172737475767778797a7b7c7d7e7f" +
		"808182838485868788898a8b8c8d8e8f909192939495969798999a9b9c9d9e9f" +
		"a0a1a2a3a4a5a6a7a8a9aaabacadaeafb0b1b2b3b4b5b6b7b8b9babbbcbdbebf" +
		"c0c1c2c3c4c5c6c7c8c9cacbcccdcecfd0d1d2d3d4d5d6d7d8d9dadbdcdddedf" +
		"e0e1e2e3e4e5e6e7e8e9eaebecedeeeff0f1f2f3f4f5f6f7f8f9fafbfcfdfeff" +
		"000102030405060708090a0b0c0d0e0f" + // the body's last 16 bytes, back at 0x00
		"6162636465666768696a6b6c6d6e6f707172737475767778797a7b7c7d7e7f80" // write_auth

	record := Record{
		Header: RecordHeader{
			Epoch:          0x0000000100000000,
			StreamIndex:    0xFFFFFFFFFFFFFFFF,
			IsCommit:       false,
			RetentionClass: RetentionDurable,
			EphBucket:      0,
			// the presence rule's other half, and it is a VALUE and not an absence: a
			// durable record carries eight zero octets here rather than nothing at all
			EphWindow:  0,
			SizeBucket: SizeBucket256,
			ExpireAt:   0,
		},
	}
	for i := range record.Header.GroupId {
		record.Header.GroupId[i] = byte(0x21 + i)
	}
	for i := range record.Header.SenderHandle {
		record.Header.SenderHandle[i] = byte(0x80 + i)
	}
	for i := range record.Header.BodyHash {
		record.Header.BodyHash[i] = byte(0x41 + i)
	}
	for i := range record.WriteAuth {
		record.WriteAuth[i] = byte(0x61 + i)
	}
	record.CtHead = make([]byte, 16)
	for i := range record.CtHead {
		record.CtHead[i] = byte(0x90 + i)
	}
	// the length comes from the ladder rather than from a 272 written here, so a ladder
	// that moved produces a record the pinned bytes no longer describe — which is the
	// failure this vector is for.
	record.CtBody = make([]byte, SizeBucketCtBodyBytes(SizeBucket256))
	for i := range record.CtBody {
		record.CtBody[i] = byte(i)
	}

	const wantLength = 1 + 32 + 16 + 8 + 8 + 1 + 1 + 8 + 1 + 8 + 32 + (4 + 0) + (4 + 0) + (4 + 16) + (4 + 272) + 32
	if len(want) != 2*wantLength {
		t.Fatalf("the vector is %d octets and the layout adds up to %d", len(want)/2, wantLength)
	}
	got := mustEncode(t, "the pinned record with a body", &record)
	if hex.EncodeToString(got) != want {
		t.Fatalf("the pinned record with a body encodes to\n%s\nwant\n%s", hex.EncodeToString(got), want)
	}
	parsed, err := parseBoth(t, "the pinned record with a body", got)
	if err != nil {
		t.Fatalf("the pinned record with a body does not parse: %v", err)
	}
	if difference := recordDifference(&record, parsed); difference != "" {
		t.Errorf("the pinned record with a body does not round trip: %s differs", difference)
	}
}

// ── the round trip ──────────────────────────────────────────────────────────────────

// Byte exact both ways over the whole corpus: the record encodes, the encoding parses
// back to the same record, and re-encoding that record reproduces the same bytes. The
// second half is what catches an encoder and a decoder that disagree about a field —
// there the value survives the first hop and the bytes do not survive the second.
func TestEveryCorpusRecordRoundTripsByteExact(t *testing.T) {
	for _, entry := range recordCorpus(t) {
		first := mustEncode(t, entry.name, &entry.record)
		parsed, err := parseBoth(t, entry.name, first)
		if err != nil {
			t.Fatalf("%s: a record this package encoded does not parse: %v", entry.name, err)
		}
		if difference := recordDifference(&entry.record, parsed); difference != "" {
			t.Fatalf("%s: the parsed record differs from the encoded one: %s", entry.name, difference)
		}
		second := mustEncode(t, entry.name, parsed)
		if !bytes.Equal(first, second) {
			t.Fatalf("%s: re-encoding the parsed record produced %d bytes, want the same %d", entry.name, len(second), len(first))
		}
	}
}

// ── record_id is not on the wire ────────────────────────────────────────────────────

// The id the server assigns changes nothing about the bytes, and parsing never invents
// one. It is in neither aad and in neither preimage (master section 8), and spec B
// section 4.3.3 carries it as a sibling protobuf field beside record_bytes, so a record
// that encoded it would be a record whose write_auth covered a value the spec says is
// never authenticated.
func TestRecordIdIsNotOnTheWire(t *testing.T) {
	ids := append([]uint64{}, u64Boundaries()...)
	for _, entry := range shortCorpus(t) {
		unassigned := mustEncode(t, entry.name, &entry.record)
		for _, id := range ids {
			assigned := entry.record
			assigned.RecordId = id
			bs := mustEncode(t, entry.name, &assigned)
			if !bytes.Equal(unassigned, bs) {
				t.Fatalf("%s: assigning record_id %d changed the encoding, and the id is never authenticated", entry.name, id)
			}
			parsed, err := parseBoth(t, entry.name, bs)
			if err != nil {
				t.Fatalf("%s: %v", entry.name, err)
			}
			if parsed.RecordId != 0 {
				t.Fatalf("%s: the parser answered record_id %d, want 0: the id is not in these bytes", entry.name, parsed.RecordId)
			}
		}
	}
}

// ── nothing is silently accepted and changed ────────────────────────────────────────

// Every prefix of a valid record is refused. A parser that stopped early on any of them
// would be a parser that accepts a truncated record as a whole one, which is a record
// whose write_auth was computed over bytes the reader never saw.
func TestEverySingleByteTruncationOfAValidRecordIsRejected(t *testing.T) {
	for _, entry := range shortCorpus(t) {
		valid := mustEncode(t, entry.name, &entry.record)
		for length := range len(valid) {
			what := fmt.Sprintf("%s truncated to %d of %d bytes", entry.name, length, len(valid))
			if _, err := parseBoth(t, what, valid[:length]); err == nil {
				t.Fatalf("%s: accepted", what)
			}
		}
	}
}

// A byte after the record is a refusal, spec A section 5.8 rule 3. Without it a record
// has more than one encoding, and the write_auth mac is over exactly one of them.
func TestATrailingByteIsRejected(t *testing.T) {
	for _, entry := range fixedRegionCorpus(t) {
		valid := mustEncode(t, entry.name, &entry.record)
		for value := 0; value <= 0xFF; value++ {
			extended := append(slices.Clone(valid), byte(value))
			what := fmt.Sprintf("%s with a trailing 0x%02x", entry.name, value)
			if _, err := parseBoth(t, what, extended); err == nil {
				t.Fatalf("%s: accepted, and a record with two encodings has a mac over one of them", what)
			}
		}
	}
}

// Every single byte corruption of the fixed region either is refused or re-encodes to
// exactly the corrupted bytes. Spec A section 5.8 rule 4.
//
// This is the property that catches a field read at the wrong width, and it is the one
// thing a round trip over well formed records cannot see: read expire_at as a u32 and
// every record this package writes still round trips, because the four bytes it ignores
// are four bytes it also never wrote. Corrupt one of them and the record parses, re-encodes
// to different bytes, and is caught here.
func TestEverySingleByteCorruptionOfTheFixedRegionIsRejectedOrRoundTrips(t *testing.T) {
	for _, entry := range fixedRegionCorpus(t) {
		valid := mustEncode(t, entry.name, &entry.record)
		if len(valid) < recordFixedRegionBytes {
			t.Fatalf("%s: encoded to %d bytes, shorter than the fixed region", entry.name, len(valid))
		}
		corrupted := slices.Clone(valid)
		for offset := range recordFixedRegionBytes {
			original := corrupted[offset]
			for value := 0; value <= 0xFF; value++ {
				if byte(value) == original {
					continue
				}
				corrupted[offset] = byte(value)
				what := fmt.Sprintf("%s with byte %d set to 0x%02x", entry.name, offset, value)
				parsed, err := parseBoth(t, what, corrupted)
				if err != nil {
					continue
				}
				again, err := EncodeRecord(parsed)
				if err != nil {
					t.Fatalf("%s: parsed and then refused to re-encode: %v", what, err)
				}
				if !bytes.Equal(again, corrupted) {
					t.Fatalf("%s: parsed and re-encoded to different bytes, so a byte was accepted and silently changed", what)
				}
			}
			corrupted[offset] = original
		}
	}
}

// The same property over the rest of the record: the four length prefixes, the bytes of
// ct_head and ct_body, and the trailing mac.
//
// Spec A section 5.8 rule 4 is stated over every accepted input, not over the first
// hundred odd bytes of one, and the variable region is where the framing lives. A length
// prefix read at the wrong width, or a field consumed at the wrong offset, moves every
// field after it, and a walk that stops before the first prefix never sees it.
//
// The corpus is one record per rung rather than the whole cross product, because this walk
// costs the record's length times 255 parses and the property is about the framing, which
// the retention class and the u64 fields do not change. The one record carrying an inline
// body is on the smallest rung that has one, for the same reason: the property does not get
// truer with 64 KiB of body to walk.
func TestEverySingleByteCorruptionOfTheVariableRegionIsRejectedOrRoundTrips(t *testing.T) {
	for _, entry := range variableRegionCorpus(t) {
		valid := mustEncode(t, entry.name, &entry.record)
		if len(valid) <= recordFixedRegionBytes {
			t.Fatalf("%s: encoded to %d bytes, so it has no variable region to walk", entry.name, len(valid))
		}
		corrupted := slices.Clone(valid)
		for offset := recordFixedRegionBytes; offset < len(valid); offset++ {
			original := corrupted[offset]
			for value := 0; value <= 0xFF; value++ {
				if byte(value) == original {
					continue
				}
				corrupted[offset] = byte(value)
				what := fmt.Sprintf("%s with byte %d of %d set to 0x%02x", entry.name, offset, len(valid), value)
				parsed, err := parseBoth(t, what, corrupted)
				if err != nil {
					continue
				}
				again, err := EncodeRecord(parsed)
				if err != nil {
					t.Fatalf("%s: parsed and then refused to re-encode: %v", what, err)
				}
				if !bytes.Equal(again, corrupted) {
					t.Fatalf("%s: parsed and re-encoded to different bytes, so a byte was accepted and silently changed", what)
				}
			}
			corrupted[offset] = original
		}
	}
}

// ── the value checks ────────────────────────────────────────────────────────────────

// The format version. Every other value of the leading byte is refused, and refused with
// its own sentinel so a caller can tell "this is a record from a newer client" from "this
// is not a record".
func TestOnlyTheOneFormatVersionIsRead(t *testing.T) {
	for _, entry := range fixedRegionCorpus(t) {
		raw := rawRecordOf(t, &entry.record)
		for value := 0; value <= 0xFF; value++ {
			if uint8(value) == recordFormatVersion {
				continue
			}
			raw.formatVersion = uint8(value)
			what := fmt.Sprintf("%s at format version 0x%02x", entry.name, value)
			_, err := parseBoth(t, what, raw.encode(t))
			if err == nil {
				t.Fatalf("%s: accepted", what)
			}
			if !errors.Is(err, ErrRecordFormatVersion) {
				t.Fatalf("%s: refused with %v, want ErrRecordFormatVersion", what, err)
			}
		}
	}
}

// is_commit is a boolean and not a truthiness. The encoder cannot produce a third value,
// which is exactly why this case is built out of raw bytes: a decoder that read the byte
// as "non zero is true" would accept 2 and 255, and every implementation that wrote one
// of them would be macing a byte the reader turned into a different one.
func TestIsCommitAcceptsOnlyZeroAndOne(t *testing.T) {
	for _, entry := range fixedRegionCorpus(t) {
		raw := rawRecordOf(t, &entry.record)
		for value := 0; value <= 0xFF; value++ {
			raw.isCommit = uint8(value)
			what := fmt.Sprintf("%s with is_commit 0x%02x", entry.name, value)
			parsed, err := parseBoth(t, what, raw.encode(t))
			if 1 < value {
				if err == nil {
					t.Fatalf("%s: accepted, and a u8 read into a go bool with != 0 accepts 255", what)
				}
				if !errors.Is(err, ErrIsCommitNotBoolean) {
					t.Fatalf("%s: refused with %v, want ErrIsCommitNotBoolean", what, err)
				}
				continue
			}
			if err != nil {
				t.Fatalf("%s: refused: %v", what, err)
			}
			if parsed.Header.IsCommit != (value == 1) {
				t.Fatalf("%s: parsed is_commit as %v", what, parsed.Header.IsCommit)
			}
		}
	}
}

// The blob rule in both directions and at both neighbouring lengths. A blob id one byte
// short or one byte long names no object the derivation of spec A section 5.13 could have
// produced, and a blob id on a rung with an inline body is a field the write_auth preimage
// covers and nothing will ever read.
func TestBlobIdPresenceAndLengthMustAgreeWithTheSizeBucket(t *testing.T) {
	buckets := acceptedSizeBuckets(t)
	lengths := []int{0, 1, blobIdBytes - 1, blobIdBytes, blobIdBytes + 1, 64}
	for _, sizeBucket := range sortedByteKeys(buckets) {
		shape := buckets[sizeBucket]
		for _, length := range lengths {
			raw := probeRecord(sizeBucket, fillBytes(blobIdTag, length))
			legal := length == 0
			if shape.needsBlobId {
				legal = length == blobIdBytes
			}
			what := fmt.Sprintf("size bucket %d with a %d byte blob id", sizeBucket, length)
			_, err := parseBoth(t, what, raw.encode(t))
			if legal {
				if err != nil {
					t.Errorf("%s: refused: %v", what, err)
				}
				continue
			}
			if err == nil {
				t.Errorf("%s: accepted", what)
				continue
			}
			if !errors.Is(err, ErrBlobIdPresence) {
				t.Errorf("%s: refused with %v, want ErrBlobIdPresence", what, err)
			}
		}
	}
}

// The encoder refuses what the parser refuses. An encoder that admitted a record its own
// parser rejects writes bytes that come back as an error one hop later, at the server,
// where the client can no longer say what was wrong with them.
func TestTheEncoderRefusesTheRecordsTheParserRefuses(t *testing.T) {
	buckets := acceptedSizeBuckets(t)
	for _, sizeBucket := range sortedByteKeys(buckets) {
		shape := buckets[sizeBucket]
		record := corpusRecord(acceptedWireBytes(t)[probeRetentionWire], sizeBucket, shape, false, false, false, 0, u64Triple{})
		// the blob id the rung does not want, or the absence of the one it does.
		if shape.needsBlobId {
			record.Header.BlobId = fillBytes(blobIdTag, blobIdBytes-1)
		} else {
			record.Header.BlobId = fillBytes(blobIdTag, blobIdBytes)
		}
		if _, err := EncodeRecord(&record); !errors.Is(err, ErrBlobIdPresence) {
			t.Errorf("size bucket %d: the encoder answered %v to a blob id that disagrees with the rung, want ErrBlobIdPresence", sizeBucket, err)
		}
	}
	// a size bucket past the top of the ladder, which is a value the parser refuses and
	// the go type cannot stop a caller from setting.
	past := SizeBucket(sortedByteKeys(buckets)[len(buckets)-1] + 1)
	beyond := corpusRecord(acceptedWireBytes(t)[probeRetentionWire], byte(past), sizeBucketShape{}, false, false, false, 0, u64Triple{})
	beyond.Header.SizeBucket = past
	if _, err := EncodeRecord(&beyond); !errors.Is(err, ErrSizeBucketOutOfRange) {
		t.Errorf("size bucket %d: the encoder answered %v, want ErrSizeBucketOutOfRange", past, err)
	}
	if _, err := EncodeRecord(nil); !errors.Is(err, ErrRecordNil) {
		t.Errorf("EncodeRecord(nil) answered %v, want ErrRecordNil", err)
	}
}

// Every size bucket past the top of the ladder is refused on the wire too, with its own
// sentinel: a record naming a seventh rung has no body length the server could check and
// no padding rung its sender could have padded to.
func TestASizeBucketOffTheLadderIsRejected(t *testing.T) {
	accepted := acceptedSizeBuckets(t)
	for value := 0; value <= 0xFF; value++ {
		if _, admitted := accepted[byte(value)]; admitted {
			continue
		}
		what := fmt.Sprintf("size bucket %d", value)
		for _, blobId := range [][]byte{nil, fillBytes(blobIdTag, blobIdBytes)} {
			_, err := parseBoth(t, what, probeRecord(byte(value), blobId).encode(t))
			if err == nil {
				t.Fatalf("%s: accepted", what)
			}
			if !errors.Is(err, ErrSizeBucketOutOfRange) {
				t.Errorf("%s: refused with %v, want ErrSizeBucketOutOfRange", what, err)
			}
		}
	}
}

// ct_body is absent or exactly its rung's ciphertext length, and nothing in between.
//
// Absent is the read path of spec A section 2.4 — the body erased by retention, or
// heads_only on the fetch — and it is why the parser is deliberately more permissive here
// than spec B section 5.1 check 3 is on submit. Anything else is a body that was not
// padded to a rung, which leaks the length the padding exists to hide.
func TestCtBodyIsEitherAbsentOrExactlyTheRungsLength(t *testing.T) {
	buckets := acceptedSizeBuckets(t)
	for _, sizeBucket := range sortedByteKeys(buckets) {
		shape := buckets[sizeBucket]
		blobId := []byte(nil)
		if shape.needsBlobId {
			blobId = fillBytes(blobIdTag, blobIdBytes)
		}
		wantBytes := SizeBucketCtBodyBytes(SizeBucket(sizeBucket))
		lengths := []int{0, 1, 16}
		if 0 <= wantBytes {
			lengths = append(lengths, wantBytes, wantBytes-1, wantBytes+1)
		}
		for _, length := range lengths {
			raw := probeRecord(sizeBucket, blobId)
			raw.ctBody = ctBodyFiller(length)
			what := fmt.Sprintf("size bucket %d with a %d byte ct_body", sizeBucket, length)
			_, err := parseBoth(t, what, raw.encode(t))
			legal := length == 0 || length == wantBytes
			if legal {
				if err != nil {
					t.Errorf("%s: refused: %v", what, err)
				}
				continue
			}
			if err == nil {
				t.Errorf("%s: accepted", what)
				continue
			}
			if !errors.Is(err, ErrCtBodyLength) {
				t.Errorf("%s: refused with %v, want ErrCtBodyLength", what, err)
			}
		}
	}
}

// An all zero write_auth is ordinary on both sides. Spec A section 2.4: the server
// rebuilds every fetched record with write_auth zero, because the mac is over the
// submitting connection's server_nonce and there is nothing left to reconstruct it from
// and nobody who could verify it. A codec that refused one would refuse every record a
// client ever reads.
func TestAnAllZeroWriteAuthIsAcceptedOnBothSides(t *testing.T) {
	zero := [32]byte{}
	found := 0
	for _, entry := range shortCorpus(t) {
		if entry.record.WriteAuth != zero {
			continue
		}
		found++
		parsed, err := parseBoth(t, entry.name, mustEncode(t, entry.name, &entry.record))
		if err != nil {
			t.Fatalf("%s: a record with a zero write_auth does not parse: %v", entry.name, err)
		}
		if parsed.WriteAuth != zero {
			t.Fatalf("%s: the parser invented a write_auth", entry.name)
		}
	}
	if found == 0 {
		t.Fatal("no corpus record carries a zero write_auth, so the read path's shape is never exercised")
	}
}

// The retention class byte goes through record.go's split, so the codec admits exactly the
// alphabet record_test.go pins and no other byte reaches a record's header.
func TestTheRetentionByteGoesThroughTheOneSplit(t *testing.T) {
	accepted := acceptedWireBytes(t)
	buckets := acceptedSizeBuckets(t)
	smallest := sortedByteKeys(buckets)[0]
	for value := 0; value <= 0xFF; value++ {
		raw := probeRecord(smallest, nil)
		raw.retentionWire = uint8(value)
		what := fmt.Sprintf("retention byte 0x%02x", value)
		parsed, err := parseBoth(t, what, raw.encode(t))
		pair, legal := accepted[byte(value)]
		if !legal {
			if err == nil {
				t.Errorf("%s: accepted, and the split names no such byte", what)
				continue
			}
			if !errors.Is(err, ErrRetentionClassUnknown) {
				t.Errorf("%s: refused with %v, want ErrRetentionClassUnknown", what, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: refused: %v", what, err)
			continue
		}
		if parsed.Header.RetentionClass != pair.class || parsed.Header.EphBucket != pair.bucket {
			t.Errorf("%s: parsed as class %d bucket %d, want class %d bucket %d",
				what, parsed.Header.RetentionClass, parsed.Header.EphBucket, pair.class, pair.bucket)
		}
	}
}

// ── the fuzzer ──────────────────────────────────────────────────────────────────────

// Where the checked-in fuzz corpus lives. Go replays every file under here on a plain go
// test, without -fuzz, which is the whole reason the malformed inputs are on disk rather
// than added as seeds in code: a seed added in code sits beside the tests that already
// cover it, and a file here is replayed by anybody who runs the package.
const fuzzCorpusDir = "testdata/fuzz/FuzzParseRecord"

// The corpus is there, it is read, and it says something.
//
// Two ways a checked-in corpus quietly stops being one, and this test refuses both. It can
// be deleted or renamed — a corpus directory whose name no longer matches the fuzz target
// is replayed by nothing and reported by nothing, so the count is asserted rather than
// assumed. And it can drift into inputs the parser refuses without exception, at which
// point the whole re-encode half of the fuzz property is unreachable from it: an input that
// is refused exercises the refusal and stops, and it is the accepted ones that have to
// re-encode to themselves. So at least one entry has to parse.
//
// The property itself is asserted here as well, over exactly the same bytes the fuzz target
// would see, so a corpus entry that violates it fails the ordinary test run rather than
// waiting for somebody to pass -fuzz.
func TestTheCheckedInFuzzCorpusIsReadAndSaysSomething(t *testing.T) {
	entries, err := os.ReadDir(fuzzCorpusDir)
	if err != nil {
		t.Fatalf("the checked-in fuzz corpus is unreadable at %s: %v", fuzzCorpusDir, err)
	}
	accepted := 0
	refused := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		bs := fuzzCorpusEntry(t, filepath.Join(fuzzCorpusDir, entry.Name()))
		record, err := parseBoth(t, entry.Name(), bs)
		if err != nil {
			refused++
			continue
		}
		accepted++
		again, err := EncodeRecord(record)
		if err != nil {
			t.Fatalf("%s: parsed and then refused to re-encode: %v", entry.Name(), err)
		}
		if !bytes.Equal(again, bs) {
			t.Fatalf("%s: parsed and re-encoded to %d different bytes, so this record has two encodings", entry.Name(), len(again))
		}
	}
	if accepted+refused == 0 {
		t.Fatalf("%s holds no corpus entry, so the fuzz target replays nothing but its own well formed seeds", fuzzCorpusDir)
	}
	if accepted == 0 {
		t.Fatalf("%s: all %d entries are refused, so no entry ever reaches the re-encode half of the property", fuzzCorpusDir, refused)
	}
	if refused == 0 {
		t.Fatalf("%s: all %d entries are accepted, so the malformed inputs it exists to carry are gone", fuzzCorpusDir, accepted)
	}
	t.Logf("%d corpus entries, %d accepted and %d refused", accepted+refused, accepted, refused)
}

// The bytes one corpus file holds. The format is go's own: a version line, then the value
// as a go literal, one value per file for a target that takes one argument.
func fuzzCorpusEntry(t testing.TB, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	lines := strings.Split(strings.ReplaceAll(string(content), "\r\n", "\n"), "\n")
	if len(lines) < 2 || !strings.HasPrefix(lines[0], "go test fuzz v") {
		t.Fatalf("%s is not a go fuzz corpus file: it begins %q", path, lines[0])
	}
	literal := strings.TrimSpace(lines[1])
	literal, isBytes := strings.CutPrefix(literal, "[]byte(")
	if !isBytes {
		t.Fatalf("%s carries a value that is not a []byte, and this target takes one", path)
	}
	literal, isBytes = strings.CutSuffix(literal, ")")
	if !isBytes {
		t.Fatalf("%s carries an unterminated []byte literal", path)
	}
	unquoted, err := strconv.Unquote(literal)
	if err != nil {
		t.Fatalf("%s does not hold a go string literal: %v", path, err)
	}
	return []byte(unquoted)
}

// The one property that has to hold over bytes nobody chose: an input is refused, or it
// re-encodes to itself exactly. Anything else is a second encoding of one record, and the
// write_auth mac is over exactly one of them.
//
// The two entry points are asked together here as well, so the fuzzer explores their
// agreement over inputs no test in this file would have thought to build.
//
// The seeds this function adds are all well formed, because a mutator wants a valid record
// to work outward from. The malformed inputs live in testdata/fuzz/FuzzParseRecord, checked
// in, which is what makes a plain go test replay them: near miss framings, prefixes that
// declare more than the input holds, a body one byte off its rung, prefixes swapped between
// two fields. Those are edits no single byte walk in this file produces, and having them on
// disk is also what gives a finding from an explicit -fuzz run somewhere to land.
func FuzzParseRecord(f *testing.F) {
	for _, entry := range shortCorpus(f) {
		bs, err := EncodeRecord(&entry.record)
		if err != nil {
			f.Fatalf("%s: EncodeRecord refused a corpus record: %v", entry.name, err)
		}
		f.Add(bs)
	}
	// one seed per rung that carries an inline body, so the fuzzer has a record with a
	// long ct_body to mutate without carrying hundreds of them.
	buckets := acceptedSizeBuckets(f)
	for _, sizeBucket := range sortedByteKeys(buckets) {
		length := SizeBucketCtBodyBytes(SizeBucket(sizeBucket))
		if length < 0 {
			continue
		}
		record := corpusRecord(acceptedWireBytes(f)[probeRetentionWire], sizeBucket, buckets[sizeBucket], true, true, true, length, u64Triple{})
		bs, err := EncodeRecord(&record)
		if err != nil {
			f.Fatalf("size bucket %d: EncodeRecord refused a seed: %v", sizeBucket, err)
		}
		f.Add(bs)
	}
	f.Add([]byte{})
	f.Add([]byte{recordFormatVersion})

	f.Fuzz(func(t *testing.T, bs []byte) {
		record, recordErr := ParseRecord(bs)
		header, headerErr := ParseRecordHeader(bs)
		if (recordErr == nil) != (headerErr == nil) {
			t.Fatalf("ParseRecord says %v and ParseRecordHeader says %v", recordErr, headerErr)
		}
		if recordErr != nil {
			return
		}
		if difference := headerDifference(&record.Header, header); difference != "" {
			t.Fatalf("the two entry points parsed different headers: %s", difference)
		}
		if record.RecordId != 0 {
			t.Fatalf("the parser answered record_id %d, want 0", record.RecordId)
		}
		again, err := EncodeRecord(record)
		if err != nil {
			t.Fatalf("accepted %d bytes and then refused to re-encode them: %v", len(bs), err)
		}
		if !bytes.Equal(again, bs) {
			t.Fatalf("accepted %d bytes and re-encoded to %d different ones, so this record has two encodings", len(bs), len(again))
		}
	})
}
