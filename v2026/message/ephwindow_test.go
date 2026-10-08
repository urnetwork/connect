// eph_window on the wire: the field the owner's ruling of 2026-09-13 added to record_bytes,
// and the four properties it owes that nothing already in this package asserted.
//
// The field is t, the time slice of a record's own K_eph[n][b][t]. It is PLAINTEXT, always
// present, zero on permanent, durable, media and eph bucket 0, sender computed, and never
// recomputed by an opener. It does three different jobs in three places -- the codec, both
// aads, and the write_auth preimage -- and the ruling is explicit that the three are not
// interchangeable, so each is observed here rather than one being taken as evidence for the
// others.
//
// What is asserted, and what each would catch.
//
//	P1  the field round trips at every retention class the wire admits, including the ZERO
//	    that every non eph class carries. A codec that dropped the field on the classes whose
//	    value is zero would round trip every such record perfectly and disagree with a second
//	    implementation about the length of every one of them.
//	P2  the field is COVERED, and by two separate mechanisms that are asserted separately
//	    because they are two mechanisms: flip it on the wire and the write_auth mac fails;
//	    flip it on the wire and the aead fails. The aead half is messagegroup's, in the file
//	    of this name there, because the keys are there. A test that proved one of the two has
//	    proved half.
//	P4  a record at the SUPERSEDED format version is refused rather than parsed with the
//	    window defaulted to zero. The silent zero is the failure this project has named more
//	    than once, and it is the one this version bump exists to make impossible.
//
// P3 -- that no preimage builder acquired a conditional for this field -- is a property of
// the SOURCE rather than of any record, and it is in preimageconditional_test.go.
//
// Nothing in this file takes a vector from the code under test. The offsets are written as
// the sums of codec.go's own table and then CHECKED against the encoder's output at run
// time; the window value the pinned vectors carry is derived from the ruling's arithmetic in
// codec_test.go; and the classes are read out of the parser.
package message

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// ── where the field is, and it is checked rather than trusted ───────────────────────

// The offset eph_window's octets begin at, written as the sum of the fields in front of it
// in codec.go's table -- format_version, group_id, sender_handle, epoch, stream_index,
// is_commit, retention_class_wire -- so a reader checks it against that table term by term
// rather than against a number. ephWindowIsWhereTheTableSaysItIs is what stops the sum being
// merely self consistent: it reads the octets at this offset out of a real encoding and
// requires them to be the header's own window.
const (
	ephWindowOffset = 1 + 32 + 16 + 8 + 8 + 1 + 1
	ephWindowBytes  = 8
)

// The elements of owner decision 60's amended octet list: master section 8's fifteen RECORD
// fields, which is every line of that block except record_id, and format_version in front of
// them. It is written down because decision 60 is the only normative statement of this
// layout in the corpus and the count is part of what it states.
const recordLayoutElements = 16

// The eight octets at the layout's own offset are the record's own window, read back big
// endian, on every record the corpus builds.
//
// THIS IS WHAT MAKES THE OFFSET CONSTANT ABOVE EVIDENCE RATHER THAN A RESTATEMENT. The two
// vectors in codec_test.go pin two records to hexadecimal, which catches a field written in
// the wrong place on those two records; this catches it on every record the corpus builds,
// and it is the assertion the rest of this file's byte surgery rests on -- a mutation aimed
// at the wrong eight octets would otherwise look like a covered field.
func TestTheEphWindowIsWhereTheLayoutTableSaysItIs(t *testing.T) {
	checked := 0
	for _, entry := range shortCorpus(t) {
		bs := mustEncode(t, entry.name, &entry.record)
		if len(bs) < ephWindowOffset+ephWindowBytes {
			t.Fatalf("%s: encoded to %d octets, which does not reach the window at %d", entry.name, len(bs), ephWindowOffset)
		}
		got := binary.BigEndian.Uint64(bs[ephWindowOffset : ephWindowOffset+ephWindowBytes])
		if got != entry.record.Header.EphWindow {
			t.Fatalf("%s: the octets at %d read back as %d and the header carries %d",
				entry.name, ephWindowOffset, got, entry.record.Header.EphWindow)
		}
		// the octet before is the retention class and the octet after is the size bucket,
		// which is the position the ruling states: immediately after the field it qualifies
		retentionWire, err := RetentionClassWire(entry.record.Header.RetentionClass, entry.record.Header.EphBucket)
		if err != nil {
			t.Fatalf("%s: the join refused: %v", entry.name, err)
		}
		if bs[ephWindowOffset-1] != retentionWire {
			t.Fatalf("%s: the octet before the window is 0x%02x and the retention byte is 0x%02x",
				entry.name, bs[ephWindowOffset-1], retentionWire)
		}
		if bs[ephWindowOffset+ephWindowBytes] != byte(entry.record.Header.SizeBucket) {
			t.Fatalf("%s: the octet after the window is 0x%02x and the size bucket is 0x%02x",
				entry.name, bs[ephWindowOffset+ephWindowBytes], byte(entry.record.Header.SizeBucket))
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no record was checked, so the offset above is asserted by nothing")
	}
	t.Logf("the window sits at octet %d, between the retention byte and the size bucket, on all %d corpus records", ephWindowOffset, checked)
}

// ── P1: the round trip, at every class the wire admits ──────────────────────────────

// The window values every class below is crossed with: the presence rule's zero, the two
// smallest non zero windows, the window the vectors pin, and the u64 boundaries. Written as
// a computed set rather than a list so the boundaries stay the ones the rest of this package
// uses.
func ephWindowValues() []uint64 {
	values := []uint64{0, 1, 2, pinnedEphWindow}
	for _, boundary := range u64Boundaries() {
		if !slices.Contains(values, boundary) {
			values = append(values, boundary)
		}
	}
	return values
}

// eph_window round trips at every retention class, at every value above, byte exact both
// ways -- and the ZERO on a class that has no window of its own is in the cross rather than
// beside it.
//
// CLASS: the retention wire octets RetentionClassOf admits, read out of the split at run
// time and never written down here, crossed with the window values above.
// SCOPE: all 256 octets a retention byte could be, offered to the split one at a time.
//
// The two are stated separately because they are separately wrong-able: the class could be
// right and the scope a literal 9, and then a split that widened tomorrow would be judged
// over the nine somebody remembered.
func TestTheEphWindowRoundTripsAtEveryRetentionClassTheWireAdmits(t *testing.T) {
	accepted := acceptedWireBytes(t)
	buckets := acceptedSizeBuckets(t)

	// THE COMPLEMENT, NAMED AND COUNTED. The scope is 256 octets and the class is what
	// survived; what was removed is every other octet, and it is printed rather than
	// summarised, because a complement asserted only non-empty is satisfied by 254 members
	// when there are 247.
	refused := []byte{}
	for value := 0; value <= 0xFF; value++ {
		if _, isAccepted := accepted[byte(value)]; !isAccepted {
			refused = append(refused, byte(value))
		}
	}
	if len(accepted)+len(refused) != 256 {
		t.Fatalf("the class has %d octets and the complement %d, which is not the 256 the scope names", len(accepted), len(refused))
	}
	if len(refused) == 0 {
		t.Fatal("the complement is EMPTY: the split accepted all 256 octets, so this cross says nothing about what it refuses")
	}
	if want := 256 - 9; len(refused) != want {
		t.Fatalf("the split removed %d of 256 octets and master section 8's table leaves %d: %s",
			len(refused), want, hex.EncodeToString(refused))
	}
	t.Logf("class: the %d retention octets the split admits, %s", len(accepted), hex.EncodeToString(sortedByteKeys(accepted)))
	t.Logf("complement: the %d octets it removed, %s", len(refused), hex.EncodeToString(refused))

	// and the class's own halves, counted, because "every retention class" means both the
	// six that carry a window and the three that carry the zero
	withWindow, withoutWindow := []byte{}, []byte{}
	for _, wire := range sortedByteKeys(accepted) {
		if accepted[wire].class == RetentionEph {
			withWindow = append(withWindow, wire)
			continue
		}
		withoutWindow = append(withoutWindow, wire)
	}
	if len(withoutWindow) == 0 {
		t.Fatal("no accepted class is a non eph one, so the ZERO half of the presence rule is never exercised")
	}
	if len(withWindow) == 0 {
		t.Fatal("no accepted class is an eph one, so the non zero half of the presence rule is never exercised")
	}
	t.Logf("of those, %d eph octets %s and %d non eph octets %s",
		len(withWindow), hex.EncodeToString(withWindow), len(withoutWindow), hex.EncodeToString(withoutWindow))

	smallest := sortedByteKeys(buckets)[0]
	crossed := 0
	for _, wire := range sortedByteKeys(accepted) {
		for _, window := range ephWindowValues() {
			record := corpusRecord(accepted[wire], smallest, buckets[smallest], true, true, false, 0, u64Triple{})
			record.Header.EphWindow = window
			what := fmt.Sprintf("class 0x%02x with window %d", wire, window)
			first := mustEncode(t, what, &record)
			parsed, err := parseBoth(t, what, first)
			if err != nil {
				t.Fatalf("%s: a record this package encoded does not parse: %v", what, err)
			}
			if parsed.Header.EphWindow != window {
				t.Errorf("%s: parsed back as window %d", what, parsed.Header.EphWindow)
			}
			if difference := recordDifference(&record, parsed); difference != "" {
				t.Fatalf("%s: the parsed record differs: %s", what, difference)
			}
			second := mustEncode(t, what, parsed)
			if !bytes.Equal(first, second) {
				t.Fatalf("%s: re-encoding the parsed record produced different bytes", what)
			}
			crossed++
		}
	}
	if want := len(accepted) * len(ephWindowValues()); crossed != want {
		t.Fatalf("the cross ran %d records and the class times the values is %d", crossed, want)
	}
	t.Logf("%d records: %d classes crossed with %d window values", crossed, len(accepted), len(ephWindowValues()))
}

// Two records differing only in their window are two different byte strings, on every class.
//
// It is the round trip's complement rather than a restatement of it: a codec that parsed the
// field correctly and wrote a constant would round trip nothing, but a codec that wrote the
// field and then let a later field overwrite part of it would round trip whatever survived.
// Two windows one apart is the pair that catches the low octet being eaten; the top of the
// range against zero is the pair that catches the high one.
func TestTwoWindowsNeverShareAnEncoding(t *testing.T) {
	accepted := acceptedWireBytes(t)
	buckets := acceptedSizeBuckets(t)
	smallest := sortedByteKeys(buckets)[0]
	for _, wire := range sortedByteKeys(accepted) {
		seen := map[string]uint64{}
		for _, window := range ephWindowValues() {
			record := corpusRecord(accepted[wire], smallest, buckets[smallest], false, false, false, 0, u64Triple{})
			record.Header.EphWindow = window
			bs := mustEncode(t, fmt.Sprintf("class 0x%02x window %d", wire, window), &record)
			key := hex.EncodeToString(bs)
			if other, collided := seen[key]; collided {
				t.Errorf("class 0x%02x: windows %d and %d encode to the same octets", wire, other, window)
				continue
			}
			seen[key] = window
		}
		if len(seen) != len(ephWindowValues()) {
			t.Errorf("class 0x%02x: %d window values produced %d encodings", wire, len(ephWindowValues()), len(seen))
		}
	}
}

// ── P2, the mac half: flip the field on the wire and write_auth fails ────────────────

// The storage root every tag below is taken under, and it is writeauth_test.go's so that the
// write key is one the vectors in that file already pin.
func ephWindowWriteKey() []byte {
	return WriteKey(writeAuthKatStorageRoot())
}

// Flipping any bit of eph_window ON THE WIRE breaks the write_auth mac.
//
// This is one of the two halves of P2 and it is deliberately not the aead half: write_auth is
// a mac under the group's write key, and it is the term the SERVER's plus or minus one window
// check rests on, so a window outside it is a window anyone in the path may rewrite with no
// consequence at the server. The aead half is in messagegroup, where the record keys are.
//
// The surgery is on the ENCODED record and not on the go struct, which is what makes this a
// statement about the wire: the record is sealed, encoded, mutated octet by octet, parsed
// back, and only then verified. A mutation applied to the struct before encoding would be
// testing that two different headers give two different macs, which is a weaker claim and is
// already TestEveryInputTheWriteAuthPreimageCoversChangesTheTag's.
//
// THE COMPLEMENT IS ASSERTED TOO, and it is what says the surgery is aimed at the right
// octets: ct_body is NOT in the write_auth preimage -- master section 9.2 carries
// LP(body_hash) and never the body -- so flipping an octet of ct_body must leave the mac
// verifying. A test that reported every octet of the record as covered would be a test whose
// mutation never landed anywhere in particular.
func TestFlippingEphWindowOnTheWireBreaksTheWriteAuthMac(t *testing.T) {
	key := ephWindowWriteKey()
	nonce := aadRamp(0x60, 32)

	buckets := acceptedSizeBuckets(t)
	rung := sortedByteKeys(buckets)[0]
	base := corpusRecord(acceptedWireBytes(t)[ephWindowEphWire(t)], rung, buckets[rung], true, false, false,
		SizeBucketCtBodyBytes(SizeBucket(rung)), u64Triple{})
	base.Header.EphWindow = pinnedEphWindow
	base.WriteAuth = ComputeWriteAuth(key, nonce, &base.Header, base.CtHead, base.Header.ServerAttachment)

	valid := mustEncode(t, "the macd record", &base)
	control, err := parseBoth(t, "the macd record", valid)
	if err != nil {
		t.Fatalf("the macd record does not parse: %v", err)
	}
	if !VerifyWriteAuth(key, nonce, control) {
		t.Fatal("the untouched record does not verify, so every mutation below would pass over a broken fixture")
	}

	flipped := 0
	for offset := ephWindowOffset; offset < ephWindowOffset+ephWindowBytes; offset++ {
		for bit := range 8 {
			mutated := slices.Clone(valid)
			mutated[offset] ^= 1 << bit
			what := fmt.Sprintf("bit %d of octet %d of eph_window", bit, offset-ephWindowOffset)
			parsed, err := parseBoth(t, what, mutated)
			if err != nil {
				t.Fatalf("%s: the mutated record does not parse, so the mac is never asked about it: %v", what, err)
			}
			if parsed.Header.EphWindow == base.Header.EphWindow {
				t.Fatalf("%s: the mutation did not move the parsed window off %d", what, base.Header.EphWindow)
			}
			if VerifyWriteAuth(key, nonce, parsed) {
				t.Errorf("%s: the record still verifies, so write_auth does not cover the window; the server's window check is then a check on a field anybody may rewrite",
					what)
			}
			flipped++
		}
	}
	if want := ephWindowBytes * 8; flipped != want {
		t.Fatalf("%d flips were made and the field is %d octets, which is %d bits", flipped, ephWindowBytes, want)
	}
	t.Logf("all %d single bit flips of the window's %d octets break the mac", flipped, ephWindowBytes)

	// the complement: the body's own octets, which the preimage carries only as
	// LP(body_hash) taken over ct_body -- so a flip there moves the record and NOT the mac
	// input, and the mac is expected to keep verifying. It is the negative control the
	// positive half is worth nothing without.
	bodyStart := len(valid) - 32 - len(base.CtBody)
	if len(base.CtBody) == 0 {
		t.Fatal("the fixture record carries no ct_body, so the negative control below reads nothing")
	}
	survived := 0
	for offset := bodyStart; offset < bodyStart+len(base.CtBody); offset++ {
		mutated := slices.Clone(valid)
		mutated[offset] ^= 0xFF
		parsed, err := parseBoth(t, "a flipped ct_body octet", mutated)
		if err != nil {
			t.Fatalf("octet %d of ct_body: the mutated record does not parse: %v", offset-bodyStart, err)
		}
		if !VerifyWriteAuth(key, nonce, parsed) {
			t.Fatalf("octet %d of ct_body breaks write_auth, so this walk is not over ct_body and the positive half above may not be over the window",
				offset-bodyStart)
		}
		survived++
	}
	t.Logf("complement: all %d octets of ct_body leave the mac verifying, which is master section 9.2's LP(body_hash) and not the body", survived)
}

// Flipping eph_window on the wire moves BOTH aads, and the two are asserted separately.
//
// P2's own wording is the reason this is one test with two assertions rather than one
// assertion: "two separate mutations, because they are two separate mechanisms and a test
// that only proves one has proved half". The end to end version of this property lives in
// messagegroup, where the keys are, and it CANNOT make this separation -- OpenRecord opens
// ct_head first and stops, so a build that dropped the window from aad_head alone still fails
// the open, on the body, and reports a covered field. That is a real hole and this is what
// closes it: the two preimages are built from the same parsed header and each is required to
// have moved, with its own message naming which one did not.
func TestFlippingEphWindowOnTheWireMovesBothAads(t *testing.T) {
	buckets := acceptedSizeBuckets(t)
	rung := sortedByteKeys(buckets)[0]
	base := corpusRecord(acceptedWireBytes(t)[ephWindowEphWire(t)], rung, buckets[rung], true, true, true, 0, u64Triple{})
	base.Header.EphWindow = pinnedEphWindow
	valid := mustEncode(t, "the record", &base)

	before, err := parseBoth(t, "the record", valid)
	if err != nil {
		t.Fatalf("the record does not parse: %v", err)
	}
	headBefore := mustAADHead(t, "before", aadKatAlgId, &before.Header, before.Header.ServerAttachment)
	bodyBefore := mustAADBody(t, "before", aadKatAlgId, before.Header.BodyBinding())

	moved := 0
	for offset := ephWindowOffset; offset < ephWindowOffset+ephWindowBytes; offset++ {
		for bit := range 8 {
			mutated := slices.Clone(valid)
			mutated[offset] ^= 1 << bit
			what := fmt.Sprintf("bit %d of octet %d of eph_window", bit, offset-ephWindowOffset)
			after, err := parseBoth(t, what, mutated)
			if err != nil {
				t.Fatalf("%s: the mutated record does not parse: %v", what, err)
			}
			if after.Header.EphWindow == before.Header.EphWindow {
				t.Fatalf("%s: the mutation did not move the parsed window", what)
			}
			if bytes.Equal(mustAADHead(t, what, aadKatAlgId, &after.Header, after.Header.ServerAttachment), headBefore) {
				t.Errorf("%s: AAD_HEAD DID NOT MOVE, so ct_head is not bound to the window it was sealed at", what)
			}
			if bytes.Equal(mustAADBody(t, what, aadKatAlgId, after.Header.BodyBinding()), bodyBefore) {
				t.Errorf("%s: AAD_BODY DID NOT MOVE, so ct_body is not bound to the window it was sealed at", what)
			}
			moved++
		}
	}
	if want := ephWindowBytes * 8; moved != want {
		t.Fatalf("%d flips were made and the field is %d octets", moved, ephWindowBytes)
	}
	t.Logf("all %d single bit flips of the window move both aads", moved)
}

// The eph retention octet this file's fixtures use: the top eph rung, read out of the join
// rather than written as 0x15.
func ephWindowEphWire(t testing.TB) byte {
	t.Helper()
	wire, err := RetentionClassWire(RetentionEph, 5)
	if err != nil {
		t.Fatalf("the join refused eph bucket 5: %v", err)
	}
	return wire
}

// ── P4: the superseded version is refused, never defaulted ──────────────────────────

// The image a v1 encoder would have produced for a record: the same octets with the eight of
// eph_window removed and the superseded version octet in front.
//
// It is built by SUBTRACTION from a v2 encoding rather than by a second encoder, which is
// what makes it the exact byte string the old codec wrote: every other field is at the value
// and the offset that codec put it at, because this package's layout is the old one with one
// element inserted. A hand written v1 encoder here would be a second layout to keep in step.
func ephWindowVersionOneImage(t testing.TB, valid []byte) []byte {
	t.Helper()
	if len(valid) < ephWindowOffset+ephWindowBytes {
		t.Fatalf("a %d octet record does not reach the window", len(valid))
	}
	image := []byte{recordFormatVersionSuperseded}
	image = append(image, valid[1:ephWindowOffset]...)
	image = append(image, valid[ephWindowOffset+ephWindowBytes:]...)
	if len(image) != len(valid)-ephWindowBytes {
		t.Fatalf("the v1 image is %d octets and the v2 record is %d", len(image), len(valid))
	}
	return image
}

// A record at the superseded format version is REFUSED, and the refusal is the version's own
// sentinel rather than whatever a later field happens to trip over.
//
// This is the silent zero, refused. Master section 0's ninth amendment and ledger item 182
// both say a decoder meeting 0x01 refuses rather than mis-parsing, and the reason is
// arithmetic rather than taste: eph_window is eight fixed width octets sitting between two
// others, so EVERY OFFSET PAST retention_class MOVES. A parser that accepted 0x01 and
// defaulted the window to zero would read size_bucket out of the first octet of expire_at,
// the body hash out of the middle of nothing, and would hand its caller a record whose fields
// are all of the right types and none of the right values.
//
// What the assertions separate, because they fail in different directions:
//
//	(1) the v1 image is refused at all, through both entry points;
//	(2) it is refused with ErrRecordFormatVersion and not with a downstream sentinel, which
//	    is what a parser that read on and tripped would produce;
//	(3) it is refused even though a legal v2 record with the window at zero exists and parses
//	    -- so "defaulted to zero" names a real record this parser could have answered and
//	    deliberately does not;
//	(4) the same octets with the version bumped to 0x02 and nothing else changed are ALSO
//	    refused, which is the other shape of the same mistake: tolerating the old layout by
//	    relabelling it.
func TestARecordAtTheSupersededVersionIsRefusedAndNotDefaultedToAZeroWindow(t *testing.T) {
	if recordFormatVersionSuperseded == recordFormatVersion {
		t.Fatal("the superseded version and the current one are the same octet, so there is nothing here to refuse")
	}
	buckets := acceptedSizeBuckets(t)
	rung := sortedByteKeys(buckets)[0]
	record := corpusRecord(acceptedWireBytes(t)[ephWindowEphWire(t)], rung, buckets[rung], false, true, false, 0, u64Triple{})
	record.Header.EphWindow = pinnedEphWindow
	valid := mustEncode(t, "the v2 record", &record)

	image := ephWindowVersionOneImage(t, valid)

	// (1) and (2)
	parsed, err := parseBoth(t, "the v1 image", image)
	if err == nil {
		t.Fatalf("a record at version 0x%02x was ACCEPTED and parsed back with window %d; every field past retention_class is read at the wrong offset",
			recordFormatVersionSuperseded, parsed.Header.EphWindow)
	}
	if !errors.Is(err, ErrRecordFormatVersion) {
		t.Errorf("the v1 image is refused with %v, want ErrRecordFormatVersion: a refusal from further down means the parser read past the version octet", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("0x%02x", recordFormatVersionSuperseded)) {
		t.Errorf("the refusal does not name the version it met: %v", err)
	}
	// and it says something about THIS version that it does not say about an unknown one.
	// The clause being held here is the branch in decodeRecord that names the superseded
	// layout: without this assertion that branch turns nothing red when it is deleted, which
	// is the same as saying it defends nothing. What it is for is the operator meeting the
	// refusal -- "a record from before the field existed" and "not a record this package
	// knows" are different problems -- so the property is that the two refusals DIFFER.
	unknown := slices.Clone(valid)
	for candidate := 0; candidate <= 0xFF; candidate++ {
		if byte(candidate) == recordFormatVersion || byte(candidate) == recordFormatVersionSuperseded {
			continue
		}
		unknown[0] = byte(candidate)
		break
	}
	_, unknownErr := parseBoth(t, "a record at an unknown version", unknown)
	if unknownErr == nil {
		t.Fatalf("a record at version 0x%02x was accepted, so the comparison below reads nothing", unknown[0])
	}
	if !errors.Is(unknownErr, ErrRecordFormatVersion) {
		t.Errorf("an unknown version is refused with %v, want ErrRecordFormatVersion", unknownErr)
	}
	sameShape := strings.ReplaceAll(unknownErr.Error(), fmt.Sprintf("0x%02x", unknown[0]),
		fmt.Sprintf("0x%02x", recordFormatVersionSuperseded))
	if sameShape == err.Error() {
		t.Errorf("the superseded version and version 0x%02x are refused with the same sentence, %q; the superseded one is the layout that came before this package and an operator meeting it is owed the difference",
			unknown[0], err)
	}

	// (3) the zero window record the default would have manufactured is a record this parser
	// really does accept, so the refusal above is about the VERSION and not about the value
	zeroWindow := record
	zeroWindow.Header.EphWindow = 0
	zeroBytes := mustEncode(t, "the v2 record with a zero window", &zeroWindow)
	zeroParsed, err := parseBoth(t, "the v2 record with a zero window", zeroBytes)
	if err != nil {
		t.Fatalf("a v2 record with a zero window does not parse, so clause (3) reads nothing: %v", err)
	}
	if zeroParsed.Header.EphWindow != 0 {
		t.Errorf("the zero window record parsed back with window %d", zeroParsed.Header.EphWindow)
	}
	if bytes.Equal(zeroBytes, image) {
		t.Fatal("the v1 image and the zero window v2 record are the same octets, so the refusal above says nothing about the version")
	}

	// (4) the old layout relabelled as the new one
	relabelled := slices.Clone(image)
	relabelled[0] = recordFormatVersion
	if _, err := parseBoth(t, "the v1 layout relabelled 0x02", relabelled); err == nil {
		t.Errorf("the v1 layout with the version octet bumped to 0x%02x was accepted; the version is then a label and not a layout",
			recordFormatVersion)
	} else {
		t.Logf("the v1 layout relabelled 0x%02x is refused: %v", recordFormatVersion, err)
	}
}

// Every version octet the parser admits, and every one it removes, over a v2 shaped record
// AND over the v1 image.
//
// CLASS: the version octets ParseRecord accepts, derived by offering it all 256 in both
// layouts.
// SCOPE: 0x00 through 0xFF, in the two record SHAPES that exist -- the current one and the
// one it superseded. The second half of that scope is the whole point: a version gate judged
// only over the current layout cannot tell a parser that refuses 0x01 from one that accepts
// it and then trips on the trailing octets, and those are different parsers.
func TestOneVersionOctetIsAcceptedAndTheComplementIsNamed(t *testing.T) {
	buckets := acceptedSizeBuckets(t)
	rung := sortedByteKeys(buckets)[0]
	record := corpusRecord(acceptedWireBytes(t)[ephWindowEphWire(t)], rung, buckets[rung], false, true, false, 0, u64Triple{})
	record.Header.EphWindow = pinnedEphWindow
	valid := mustEncode(t, "the v2 record", &record)
	image := ephWindowVersionOneImage(t, valid)

	for _, shape := range []struct {
		name  string
		bytes []byte
		want  []byte
	}{
		{name: "the current layout", bytes: valid, want: []byte{recordFormatVersion}},
		{name: "the superseded layout", bytes: image, want: nil},
	} {
		accepted, refused := []byte{}, []byte{}
		for value := 0; value <= 0xFF; value++ {
			candidate := slices.Clone(shape.bytes)
			candidate[0] = byte(value)
			if _, err := parseBoth(t, fmt.Sprintf("%s at 0x%02x", shape.name, value), candidate); err == nil {
				accepted = append(accepted, byte(value))
				continue
			}
			refused = append(refused, byte(value))
		}
		if len(accepted)+len(refused) != 256 {
			t.Fatalf("%s: %d accepted and %d refused is not the 256 the scope names", shape.name, len(accepted), len(refused))
		}
		if len(refused) == 0 {
			t.Fatalf("%s: the complement is EMPTY, so this gate narrows nothing", shape.name)
		}
		if !slices.Equal(accepted, shape.want) {
			t.Errorf("%s: accepts %s, want %s", shape.name, hex.EncodeToString(accepted), hex.EncodeToString(shape.want))
		}
		t.Logf("%s: accepts %s; complement is the other %d octets", shape.name, hex.EncodeToString(accepted), len(refused))
	}
}

// ── the layout's element count, as decision 60 states it ────────────────────────────

// The encoder writes exactly the sixteen elements owner decision 60 names, in one straight
// run, and the fields it does NOT write are named.
//
// CLASS: the calls on the syntax writer inside EncodeRecord, read out of codec.go's syntax
// tree.
// SCOPE: codec.go, located from this package's own directory at run time.
//
// The count is asserted twice against two things that were written down independently: the
// constant above, which is decision 60's, and the field count of rawRecord in codec_test.go,
// which is this test tree's own statement of the same layout. A field added to the encoder
// and not to rawRecord fails here as well as in the layout comparison.
//
// THE COMPLEMENT IS THE POINT OF THE SECOND HALF. Record and RecordHeader between them
// declare more fields than the encoder writes, and the ones left out are left out for three
// different reasons -- record_id is never authenticated and is not in these bytes at all,
// and RetentionClass and EphBucket are joined into one octet by a function the encoder calls
// rather than written straight. All three are named, and a fourth arriving is a field
// somebody added to the header and did not put on the wire.
func TestTheEncoderWritesTheSixteenElementsDecisionSixtyNames(t *testing.T) {
	decl := ephWindowFuncDecl(t, "codec.go", "EncodeRecord")
	written := []string{}
	ast.Inspect(decl.Body, func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if !isCall {
			return true
		}
		selector, isSelector := call.Fun.(*ast.SelectorExpr)
		if !isSelector {
			return true
		}
		receiver, isIdent := selector.X.(*ast.Ident)
		if !isIdent || receiver.Name != "writer" || !strings.HasPrefix(selector.Sel.Name, "Write") {
			return true
		}
		written = append(written, selector.Sel.Name+"("+ephWindowExprText(t, call.Args)+")")
		return true
	})
	if len(written) == 0 {
		t.Fatal("no write call was found in EncodeRecord, so this gate read nothing")
	}
	for i, element := range written {
		t.Logf("element %2d: %s", i+1, element)
	}
	if len(written) != recordLayoutElements {
		t.Errorf("EncodeRecord writes %d elements and owner decision 60's amended list has %d", len(written), recordLayoutElements)
	}
	if fields := ephWindowStructFieldCount(t, "codec_test.go", "rawRecord"); fields != len(written) {
		t.Errorf("EncodeRecord writes %d elements and rawRecord, which states the layout a second time, has %d fields", len(written), fields)
	}

	// the complement: every field of Record and RecordHeader that EncodeRecord does not
	// mention ANYWHERE in its body, which is a wider read than the write calls alone on
	// purpose -- a field reached through a local alias, or read to compute another field,
	// is a field the function does handle, and the complement is meant to name the ones it
	// genuinely never touches
	mentioned := map[string]bool{}
	ast.Inspect(decl.Body, func(node ast.Node) bool {
		if selector, isSelector := node.(*ast.SelectorExpr); isSelector {
			mentioned[selector.Sel.Name] = true
		}
		return true
	})
	absent := []string{}
	for _, typeName := range []string{"Record", "RecordHeader"} {
		for _, field := range ephWindowStructFields(t, "record.go", typeName) {
			if !mentioned[field] {
				absent = append(absent, typeName+"."+field)
			}
		}
	}
	slices.Sort(absent)
	if len(absent) == 0 {
		t.Fatal("the complement is EMPTY: every field of both structs appears in a write call, which cannot be true while record_id is off the wire")
	}
	want := []string{"Record.RecordId", "RecordHeader.EphBucket", "RecordHeader.RetentionClass"}
	if !slices.Equal(absent, want) {
		t.Errorf("EncodeRecord names none of %v, want exactly %v: record_id is never authenticated, and the class and the bucket reach the wire through the one join", absent, want)
	}
	t.Logf("complement: %d fields are written by no call here, %v", len(absent), absent)
}

// ── the small ast helpers these two gates share ─────────────────────────────────────

// One function declaration out of one file of this package, located from the package
// directory at run time rather than from a path written down.
func ephWindowFuncDecl(t testing.TB, file string, name string) *ast.FuncDecl {
	t.Helper()
	parsed := ephWindowParse(t, file)
	for _, decl := range parsed.Decls {
		if funcDecl, isFunc := decl.(*ast.FuncDecl); isFunc && funcDecl.Name.Name == name && funcDecl.Body != nil {
			return funcDecl
		}
	}
	t.Fatalf("%s declares no function named %s with a body", file, name)
	return nil
}

func ephWindowParse(t testing.TB, file string) *ast.File {
	t.Helper()
	path := filepath.Join(".", file)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("%s is not in this package's directory: %v", file, err)
	}
	parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("%s does not parse: %v", file, err)
	}
	return parsed
}

// The field names of one struct declared in one file of this package.
func ephWindowStructFields(t testing.TB, file string, name string) []string {
	t.Helper()
	fields := []string{}
	for _, decl := range ephWindowParse(t, file).Decls {
		genDecl, isGen := decl.(*ast.GenDecl)
		if !isGen {
			continue
		}
		for _, spec := range genDecl.Specs {
			typeSpec, isType := spec.(*ast.TypeSpec)
			if !isType || typeSpec.Name.Name != name {
				continue
			}
			structType, isStruct := typeSpec.Type.(*ast.StructType)
			if !isStruct {
				continue
			}
			for _, field := range structType.Fields.List {
				for _, ident := range field.Names {
					fields = append(fields, ident.Name)
				}
			}
		}
	}
	if len(fields) == 0 {
		t.Fatalf("%s declares no struct %s with fields, so the class read nothing", file, name)
	}
	return fields
}

func ephWindowStructFieldCount(t testing.TB, file string, name string) int {
	t.Helper()
	return len(ephWindowStructFields(t, file, name))
}

// The source text of a call's arguments, rendered from the tree so a failure names the field
// rather than an index.
func ephWindowExprText(t testing.TB, args []ast.Expr) string {
	t.Helper()
	parts := []string{}
	for _, arg := range args {
		parts = append(parts, ephWindowRender(arg))
	}
	return strings.Join(parts, ", ")
}

func ephWindowRender(expr ast.Expr) string {
	switch typed := expr.(type) {
	case *ast.Ident:
		return typed.Name
	case *ast.SelectorExpr:
		return ephWindowRender(typed.X) + "." + typed.Sel.Name
	case *ast.CallExpr:
		parts := []string{}
		for _, arg := range typed.Args {
			parts = append(parts, ephWindowRender(arg))
		}
		return ephWindowRender(typed.Fun) + "(" + strings.Join(parts, ", ") + ")"
	case *ast.SliceExpr:
		return ephWindowRender(typed.X) + "[:]"
	case *ast.StarExpr:
		return "*" + ephWindowRender(typed.X)
	}
	return "?"
}
