// The server attachment: the layout, the four kinds, and the properties that keep the
// server's static shape check and the client's encoder from drifting apart.
//
// Six things are observed here and they fail in different directions.
//
// The first is that the bytes are the layout spec A section 5.11 states. A round trip
// cannot see this — an encoder and a decoder that agree on a permuted field order round
// trip perfectly and agree with nobody — so the layout is written down a second time, in
// rawAttachment and its four raw bodies below, and every corpus attachment's encoding is
// compared against it. rawAttachment is also how this file reaches the encodings the
// encoder refuses to produce: a 31 octet Ed25519 pub, an expected_wrap_count of zero, the
// absent attachment spelled out as kind 0x0000, a kind nothing defines. But rawAttachment
// lives beside the encoder, so a permutation applied to both at once passes it; the
// anchors that do not move with the code are the four golden vectors, one per kind, each
// a hexadecimal string derived by hand from section 5.11's block with the arithmetic shown
// beside every line. The EpochAttachment vector is the strongest of the four and not by a
// little: it is byte for byte the attachment aad_test.go already pins, whose digest that
// file's comment records as having been derived by a separate program that imports nothing
// from this package. Agreeing with it is agreement with something that is not this
// encoder.
//
// The second is that the kind alphabet is exactly the five codes section 5.11 defines, and
// that each code means what the spec says it means. Both halves are derived: the encodable
// set by offering the encoder all 65536 kind codes crossed with every body shape this
// package has, and the parsable set by taking each kind's own valid encoding and replacing
// its leading u16 with each of the 65536 values in turn. What those derivations are
// compared against is the one place the meanings are written down, specAttachmentKindCodes,
// because a permuted table round trips perfectly and agrees with nobody — swap
// AttachmentEpoch and AttachmentRecovery in the constants and every property in this file
// but that one still holds, while every record either kind rides on carries a kind octet
// pair inside H(server_attachment), inside both aeads and inside the write_auth mac that no
// other implementation reproduces.
//
// The third is that the round trip is byte exact over a corpus that is a cross product
// rather than a list. The axes are every kind, the u64 boundaries on every 64 bit field,
// the u32 boundaries on every 32 bit field — both durable_ttl_seconds sentinels among them
// — and three content rotations across the six length prefixed fields, so that two same
// width fields never hold the same octets and a swapped encode order has somewhere to show.
//
// The fourth is that nothing is silently accepted and changed. Every single octet
// truncation of every valid encoding is refused, every trailing octet is refused, and every
// single octet corruption either is refused or re-encodes to exactly the corrupted bytes.
// That last one is what catches a field read at the wrong width, which is the one defect a
// round trip over well formed attachments cannot see: read expected_wrap_count as a u16 and
// every attachment this package writes still round trips, because the two octets it ignores
// are two octets it also never wrote.
//
// The fifth is the validation spec B section 5.1 check 3 says it will rely on, asserted as
// two claims rather than one. What must be refused: every length but the exact one on every
// length prefixed field — a class read off the go types by reflection rather than listed, so
// a seventh such field added later is covered the day it is declared — every algorithm
// identifier but the one its kind names, an expected_wrap_count of zero, an unknown kind,
// and the absent attachment spelled out. And what must NOT be refused, which is the half a
// hand written range check breaks silently: both durable_ttl_seconds sentinels, 0 and
// 4294967295, and every other value of both retention fields. Spec B section 7.3 case 3
// forbids refusing either sentinel in all cases, and a commit refused here is a group that
// cannot rekey.
//
// The sixth is the absent attachment. EncodeServerAttachment(nil) and an AttachmentNone
// attachment produce the identical zero length bytes, and therefore the identical
// H(server_attachment), which is section 5.11's test obligation stated from this side of
// aad.go's vector. It is asserted on the hash and not only on the bytes, because the hash
// is what actually reaches the mac.
//
// Beneath all six is the fuzz target and the corpus checked in beside it. Every byte string
// above was chosen by something in this file; the corpus carries the near miss framings a
// byte walk does not produce, and a plain go test replays them.
package message

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/urnetwork/connect/mls/syntax"
)

// ── the layout, written down a second time ──────────────────────────────────────────

// The framing every encoding carries whatever its kind: the u16 kind and the 32 bit length
// prefix of the body. Written as that sum so a reader can check it against section 5.11's
// server_attachment line term by term rather than against a number.
const attachmentFramingBytes = 2 + 4

// An attachment as raw values, in section 5.11's field order.
//
// This is the layout stated independently of the code under test, and it is what makes the
// encoding pinnable at all: swap two same width fields in both writeAttachmentBody and
// readAttachmentBody and every round trip in this file still passes, because the package
// would agree with itself perfectly and with no other implementation. It is also the only
// way to build the encodings EncodeServerAttachment will not — a key one octet short, a
// kind nothing defines — and those are exactly the inputs the parser's refusals are about.
type rawAttachment struct {
	kind uint16
	body []byte
}

// The bytes this raw attachment is. It writes what it is given, including a kind and a body
// the codec would refuse, which is what it is for.
func (self rawAttachment) encode(t testing.TB) []byte {
	t.Helper()
	writer := syntax.NewWriter()
	writer.WriteUint16(self.kind)
	writer.WriteOpaqueLP(self.body)
	bs, err := writer.Bytes()
	if err != nil {
		t.Fatalf("the raw attachment does not encode: %v", err)
	}
	return bs
}

// The epoch attachment's body as raw values.
type rawEpochAttachment struct {
	epoch             uint64
	algId             uint16
	writeKey          []byte
	readKey           []byte
	mediaTtlSeconds   uint32
	durableTtlSeconds uint32
	groupContextHash  []byte
	expectedWrapCount uint32
}

func (self rawEpochAttachment) encode(t testing.TB) []byte {
	t.Helper()
	writer := syntax.NewWriter()
	writer.WriteUint64(self.epoch)
	writer.WriteUint16(self.algId)
	writer.WriteOpaqueLP(self.writeKey)
	writer.WriteOpaqueLP(self.readKey)
	writer.WriteUint32(self.mediaTtlSeconds)
	writer.WriteUint32(self.durableTtlSeconds)
	writer.WriteOpaqueLP(self.groupContextHash)
	writer.WriteUint32(self.expectedWrapCount)
	bs, err := writer.Bytes()
	if err != nil {
		t.Fatalf("the raw epoch attachment does not encode: %v", err)
	}
	return bs
}

// The recovery tag's body as raw values.
type rawRecoveryTag struct {
	recoveryHandle    []byte
	recoveryVerifyPub []byte
	algId             uint16
}

func (self rawRecoveryTag) encode(t testing.TB) []byte {
	t.Helper()
	writer := syntax.NewWriter()
	writer.WriteOpaqueLP(self.recoveryHandle)
	writer.WriteOpaqueLP(self.recoveryVerifyPub)
	writer.WriteUint16(self.algId)
	bs, err := writer.Bytes()
	if err != nil {
		t.Fatalf("the raw recovery tag does not encode: %v", err)
	}
	return bs
}

// The wrap tag's body as raw values.
type rawWrapTag struct {
	wrapTargetHandle []byte
	epoch            uint64
}

func (self rawWrapTag) encode(t testing.TB) []byte {
	t.Helper()
	writer := syntax.NewWriter()
	writer.WriteOpaqueLP(self.wrapTargetHandle)
	writer.WriteUint64(self.epoch)
	bs, err := writer.Bytes()
	if err != nil {
		t.Fatalf("the raw wrap tag does not encode: %v", err)
	}
	return bs
}

// The epoch digest attachment's body as raw values.
//
// Written out field by field beside the four above rather than derived from
// rawEpochAttachment with two fields dropped, for the reason rawAttachment exists at all:
// the point of this half of the file is to state the layout independently of the code under
// test, and a raw builder that took its field ORDER from the one it is checking would agree
// with a permutation of it.
type rawEpochDigestAttachment struct {
	epoch             uint64
	algId             uint16
	mediaTtlSeconds   uint32
	durableTtlSeconds uint32
	groupContextHash  []byte
	expectedWrapCount uint32
	epochKeysDigest   []byte
}

func (self rawEpochDigestAttachment) encode(t testing.TB) []byte {
	t.Helper()
	writer := syntax.NewWriter()
	writer.WriteUint64(self.epoch)
	writer.WriteUint16(self.algId)
	writer.WriteUint32(self.mediaTtlSeconds)
	writer.WriteUint32(self.durableTtlSeconds)
	writer.WriteOpaqueLP(self.groupContextHash)
	writer.WriteUint32(self.expectedWrapCount)
	writer.WriteOpaqueLP(self.epochKeysDigest)
	bs, err := writer.Bytes()
	if err != nil {
		t.Fatalf("the raw epoch digest attachment does not encode: %v", err)
	}
	return bs
}

// The marker's body as raw values.
type rawEpochComplete struct {
	epoch     uint64
	wrapCount uint32
}

func (self rawEpochComplete) encode(t testing.TB) []byte {
	t.Helper()
	writer := syntax.NewWriter()
	writer.WriteUint64(self.epoch)
	writer.WriteUint32(self.wrapCount)
	bs, err := writer.Bytes()
	if err != nil {
		t.Fatalf("the raw epoch complete does not encode: %v", err)
	}
	return bs
}

// One go attachment as raw values, so a test can take a valid attachment, change one field
// to something the encoder refuses, and still reach the parser with it.
//
// AttachmentNone answers the empty raw form, which encodes to the six octets that spell the
// absent attachment out — an encoding this package refuses to read and never writes, and
// therefore one only this builder can produce.
func rawAttachmentOf(t testing.TB, a *ServerAttachment) rawAttachment {
	t.Helper()
	raw := rawAttachment{kind: uint16(a.Kind)}
	switch {
	case a.Epoch != nil:
		raw.body = rawEpochAttachment{
			epoch:             a.Epoch.Epoch,
			algId:             a.Epoch.AlgId,
			writeKey:          a.Epoch.WriteKey,
			readKey:           a.Epoch.ReadKey,
			mediaTtlSeconds:   a.Epoch.MediaTtlSeconds,
			durableTtlSeconds: a.Epoch.DurableTtlSeconds,
			groupContextHash:  a.Epoch.GroupContextHash,
			expectedWrapCount: a.Epoch.ExpectedWrapCount,
		}.encode(t)
	case a.Recovery != nil:
		raw.body = rawRecoveryTag{
			recoveryHandle:    a.Recovery.RecoveryHandle,
			recoveryVerifyPub: a.Recovery.RecoveryVerifyPub,
			algId:             a.Recovery.AlgId,
		}.encode(t)
	case a.Wrap != nil:
		raw.body = rawWrapTag{
			wrapTargetHandle: a.Wrap.WrapTargetHandle,
			epoch:            a.Wrap.Epoch,
		}.encode(t)
	case a.Complete != nil:
		raw.body = rawEpochComplete{
			epoch:     a.Complete.Epoch,
			wrapCount: a.Complete.WrapCount,
		}.encode(t)
	case a.EpochDigest != nil:
		raw.body = rawEpochDigestAttachment{
			epoch:             a.EpochDigest.Epoch,
			algId:             a.EpochDigest.AlgId,
			mediaTtlSeconds:   a.EpochDigest.MediaTtlSeconds,
			durableTtlSeconds: a.EpochDigest.DurableTtlSeconds,
			groupContextHash:  a.EpochDigest.GroupContextHash,
			expectedWrapCount: a.EpochDigest.ExpectedWrapCount,
			epochKeysDigest:   a.EpochDigest.EpochKeysDigest,
		}.encode(t)
	}
	return raw
}

// ── the kind alphabet, written down once ────────────────────────────────────────────

// The five kind codes of spec A section 5.11 and what each one names. This is the one place
// in this file the MEANING of a code is written down; every set of codes below is derived
// from the package's own answers and compared against this.
//
// It has to be written down, because nothing derived can pin it. Swap AttachmentEpoch and
// AttachmentRecovery in the constants and every code still round trips, every kind is still
// distinct, the parser still refuses everything it should — the package agrees with itself
// perfectly, and the records it writes carry a kind inside H(server_attachment), inside
// aad_head and inside the write_auth preimage that no other implementation reproduces, so
// every commit and every recovery publication fails at a mac nobody can see into.
var specAttachmentKindCodes = map[ServerAttachmentKind]uint16{
	AttachmentNone:     0x0000,
	AttachmentEpoch:    0x0001,
	AttachmentRecovery: 0x0002,
	AttachmentWrap:     0x0003,
	AttachmentComplete: 0x0004,
}

// The kind codes RULED since section 5.11 was written, and what each one names. It is a
// second table rather than a sixth row of the one above, and the split is the point: the
// five above are what a server built against the published section 5.11 serves, and the
// ones here are what this package defines and that door does not serve yet. A reader who
// wants to know which is which reads the two names.
var ruledAttachmentKindCodes = map[ServerAttachmentKind]uint16{
	AttachmentEpochDigest: 0x0005,
}

// The names section 5.11's table gives the five codes and ruling 27 gives the sixth, for a
// failure message that says which kind is meant rather than which number.
var specAttachmentKindNames = map[ServerAttachmentKind]string{
	AttachmentNone:        "NONE",
	AttachmentEpoch:       "EpochAttachment",
	AttachmentRecovery:    "RecoveryTag",
	AttachmentWrap:        "WrapTag",
	AttachmentComplete:    "EpochComplete",
	AttachmentEpochDigest: "EpochDigest",
}

// The written down codes, sorted, as the one alphabet every derived set is compared against.
func specAttachmentCodes() []int {
	codes := make([]int, 0, len(specAttachmentKindCodes))
	for _, code := range specAttachmentKindCodes {
		codes = append(codes, int(code))
	}
	slices.Sort(codes)
	return codes
}

// Every code this package defines, whichever door serves it: the two written down tables
// joined, sorted.
//
// The walks that are about the PACKAGE's alphabet — the body pointers of ServerAttachment,
// the length prefixed fields of every body, the codes nothing defines — run over this, and
// the walks that are about section 5.11's door run over specAttachmentCodes. Getting that
// distinction wrong in either direction is how the sixth kind would end up pinned by
// nothing while every property in this file went on holding over the five that were already
// here, which is the failure TestEveryKindThatCarriesABodyHasAGoldenVector was written to
// prevent.
func definedAttachmentCodes() []int {
	codes := specAttachmentCodes()
	for _, code := range ruledAttachmentKindCodes {
		codes = append(codes, int(code))
	}
	slices.Sort(codes)
	return codes
}

// The codes RULED since section 5.11 was published, sorted. Derived by subtracting one
// written down table from the other rather than listed a third time.
//
// IT IS A PROVENANCE SET AND IT IS NOT A DOOR'S REACH, and the two were the same set until
// 2026-09-23. This function used to be named for the second meaning — "the codes section
// 5.11's door does not serve" — and four properties in this file walked specAttachmentCodes
// as though THAT meant "served". It did, for exactly as long as the two coincided. When
// ruling 33 put the epoch keys on the request and section 5.1 check 3's door was widened to
// the sixth kind, the coincidence ended: the provenance split did not move at all, and
// three of those four walks would have gone on holding over the five that were already here
// while saying nothing about the sixth. Which kinds a DOOR serves is attachmentDoorServes,
// below, and nothing in this file may read this function for that.
func ruledAttachmentCodes() []int {
	published := map[int]bool{}
	for _, code := range specAttachmentCodes() {
		published[code] = true
	}
	codes := []int{}
	for _, code := range definedAttachmentCodes() {
		if !published[code] {
			codes = append(codes, code)
		}
	}
	slices.Sort(codes)
	return codes
}

// ── which kinds each DOOR serves, written down ──────────────────────────────────────

// The kinds each of this package's two doors serves, as a DISPOSITION: what a reader of
// this file has decided, against which the package's own two maps are held.
//
// It is a third written down table rather than a fourth derivation, and its subject is what
// distinguishes it from the two above. Those are about PROVENANCE — which document ruled
// each code — and they do not move when a door widens. This one is about REACH, and it
// moved on 2026-09-23: section 5.1 check 3's door gained AttachmentEpochDigest when ruling
// 33 put write_key[n+1] and read_key[n+1] on the REQUEST, so that a server accepting the
// kind is a server that was handed the keys.
//
// THE COMPLEMENT MOVED WITH IT, and naming it is the whole reason this table exists rather
// than a boolean somewhere. Before that date the door with a non-empty complement was
// section 5.1 check 3's, which refused one defined kind; after it, that door's complement is
// EMPTY and the epoch digest door carries the whole of it, refusing five. The mechanism
// under test — a door names the kind it will not serve instead of parsing it into something
// — is the same mechanism over the other door, and TestARecordCarriesAKindADoorRefusesByName
// is written over both so that it cannot quietly come to hold over neither.
var attachmentDoorServes = map[string][]ServerAttachmentKind{
	serverAttachmentDoorName: {
		AttachmentNone,
		AttachmentEpoch,
		AttachmentRecovery,
		AttachmentWrap,
		AttachmentComplete,
		AttachmentEpochDigest,
	},
	epochDigestDoorName: {AttachmentEpochDigest},
}

// The door names, sorted, so every walk over the doors runs in one order.
func attachmentDoorNames() []string {
	names := make([]string, 0, len(attachmentDoorServes))
	for name := range attachmentDoorServes {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// The codes the named door is written down as serving, sorted.
func servedAttachmentCodes(t testing.TB, door string) []int {
	t.Helper()
	kinds, named := attachmentDoorServes[door]
	if !named {
		t.Fatalf("no door is written down under %q, so this walk would run over nothing", door)
	}
	codes := make([]int, 0, len(kinds))
	for _, kind := range kinds {
		codes = append(codes, int(kind))
	}
	slices.Sort(codes)
	return codes
}

// THE COMPLEMENT, PRINTED AS A VALUE: the codes this package defines that the named door is
// written down as NOT serving, sorted.
//
// Derived by subtraction rather than listed, so it cannot come to disagree with the table
// above, and it is allowed to be empty — section 5.1 check 3's is, since 2026-09-23. What
// is not allowed is for EVERY door's to be empty, and that is asserted rather than assumed.
func unservedAttachmentCodes(t testing.TB, door string) []int {
	t.Helper()
	served := map[int]bool{}
	for _, code := range servedAttachmentCodes(t, door) {
		served[code] = true
	}
	codes := []int{}
	for _, code := range definedAttachmentCodes() {
		if !served[code] {
			codes = append(codes, code)
		}
	}
	slices.Sort(codes)
	return codes
}

// The package's own served map per door, keyed by door name, with the two ways that
// correspondence can go wrong asserted HERE and asserted once.
//
// It is a function rather than a literal at each call site because there are now two call
// sites — the test that compares each map's CONTENTS against the disposition, and the test
// that narrows each map to witness its CALL SITES — and a second literal would be a second
// place for a door to go missing from.
func attachmentServedMaps(t testing.TB) map[string]map[ServerAttachmentKind]bool {
	t.Helper()
	answer := map[string]map[ServerAttachmentKind]bool{
		serverAttachmentDoorName: serverAttachmentKindServed,
		epochDigestDoorName:      epochDigestKindServed,
	}
	if len(answer) != len(attachmentDoorServes) {
		t.Fatalf("the package has %d served maps and %d doors are written down", len(answer), len(attachmentDoorServes))
	}
	for _, door := range attachmentDoorNames() {
		if _, wired := answer[door]; !wired {
			t.Fatalf("door %q is written down and no map of this package answers for it", door)
		}
	}
	return answer
}

// The two halves of a door: the entry point that WRITES an attachment and the one that READS
// one. Named rather than spelled as string literals at each of the five places they appear.
const (
	attachmentEncodeHalf = "encode"
	attachmentParseHalf  = "parse"
)

// WHICH OF EACH DOOR'S ENTRY POINTS ASK ITS SERVED MAP AT RUN TIME, as a DISPOSITION, and
// the asymmetry in it is the content.
//
// Section 5.1 check 3's door has BOTH halves, and it has to: EncodeServerAttachment takes a
// whole ServerAttachment and ParseServerAttachment takes whole octets, so in each case a kind
// the door does not serve is a value the door can be HANDED, and the only thing that can
// refuse it is a run time check against the map. Those are the two call sites that were
// witnessed by nothing until this test.
//
// The epoch digest door has the PARSE half only. That is not an omission and it is not a
// weaker door — it is a stronger one. EncodeEpochDigestAttachment takes the BODY,
// *EpochDigestAttachment, so an unserved kind cannot be SPELLED at it and its refusal is the
// compiler's rather than a map lookup's. Its parse half has no such luxury: octets carry
// their own kind, so ParseEpochDigestAttachment must ask.
//
// BOTH DIRECTIONS ARE ASSERTED, which is what makes this a disposition rather than a note. A
// half listed here must REFUSE while the map is narrowed; a half not listed must SUCCEED. So
// a served check added to EncodeEpochDigestAttachment fails here until someone writes it
// down, and a served check deleted from either half of section 5.1 check 3's door fails here
// too — which is the whole point, because on 2026-09-23 either one could be deleted outright
// with this package, messagegroup and protocol all green.
var attachmentDoorRuntimeHalves = map[string][]string{
	serverAttachmentDoorName: {attachmentEncodeHalf, attachmentParseHalf},
	epochDigestDoorName:      {attachmentParseHalf},
}

// One encoding offered to the door NAMED, rather than to the door that serves its kind, with
// whatever that door parsed rebuilt into the one shape the comparisons in this file take.
//
// It is the counterpart of parseAtItsDoor and it exists for the opposite purpose: that one
// asks a kind's own door to read it, and this one asks a door that may well refuse.
//
// IT ANSWERS THE ATTACHMENT AND NOT ONLY THE ERROR. It answered the error alone in the single
// commit it existed in before this one — the commit that widened section 5.1 check 3's door
// to the sixth kind — so every walk written over it could assert that a served door said yes
// and nothing about WHAT it handed back, which is half a round trip. A parse arm that zeroed
// two fields of the kind that door had just been widened to satisfied every one of those
// walks, with this package, messagegroup and protocol all green under `go test -run 'Test'`;
// the property was left to FuzzParseServerAttachment's seed corpus, which that filter does
// not run. The second return value is what lets the caller assert it.
func parseAtDoor(t testing.TB, door string, bs []byte) (*ServerAttachment, error) {
	t.Helper()
	switch door {
	case serverAttachmentDoorName:
		return ParseServerAttachment(bs)
	case epochDigestDoorName:
		digest, err := ParseEpochDigestAttachment(bs)
		if err != nil {
			return nil, err
		}
		return &ServerAttachment{Kind: AttachmentEpochDigest, EpochDigest: digest}, nil
	}
	t.Fatalf("no parse entry point is wired for door %q, so a door could be added to the table and gated by nothing", door)
	return nil, nil
}

// One attachment offered to the ENCODE entry point of the door NAMED, the counterpart of the
// parse helper above and of encodeAtItsDoor below.
//
// ITS DOMAIN IS THE PAIRS THE TABLE SAYS ARE SERVED, which is the only place it is called
// from, and the asymmetry is worth stating rather than hiding: section 5.1 check 3's door
// takes a whole ServerAttachment and refuses an unserved kind with a runtime sentinel, while
// the epoch digest door takes the body alone, so an unserved kind cannot be SPELLED at it and
// its refusal is the compiler's. A loop that tried to walk the unserved pairs here would have
// to invent the second door's refusal, and an invented answer is not the package's.
func encodeAtDoor(t testing.TB, door string, a *ServerAttachment) ([]byte, error) {
	t.Helper()
	switch door {
	case serverAttachmentDoorName:
		return EncodeServerAttachment(a)
	case epochDigestDoorName:
		if a.EpochDigest == nil {
			t.Fatalf("the epoch digest door was handed a kind 0x%04x attachment, which its signature cannot express", uint16(a.Kind))
		}
		return EncodeEpochDigestAttachment(a.EpochDigest)
	}
	t.Fatalf("no encode entry point is wired for door %q, so a door could be added to the table and gated by nothing", door)
	return nil, nil
}

// WHERE TWO ENCODINGS FIRST DIFFER, as a phrase for a failure message.
//
// A message that reports two LENGTHS says nothing when the two are equal, and equal is the
// usual case: the mistakes that put one kind on the wire twice — a field written from the
// wrong member, two fields swapped, a constant written in place of a value — all keep the
// width. So the offset and the two octets at it are what this answers.
func firstOctetDifference(left []byte, right []byte) string {
	for i := 0; i < len(left) && i < len(right); i++ {
		if left[i] != right[i] {
			return fmt.Sprintf("first differing octet at offset %d: 0x%02x against 0x%02x", i, left[i], right[i])
		}
	}
	if len(left) != len(right) {
		return fmt.Sprintf("identical for %d octets and then %d octets against %d", min(len(left), len(right)), len(left), len(right))
	}
	return ""
}

// The doors written down as serving the code named, sorted, derived from the one table.
func attachmentDoorsServing(t testing.TB, code int) []string {
	t.Helper()
	doors := []string{}
	for _, door := range attachmentDoorNames() {
		if slices.Contains(servedAttachmentCodes(t, door), code) {
			doors = append(doors, door)
		}
	}
	return doors
}

// ── the doors ───────────────────────────────────────────────────────────────────────

// One attachment encoded at the door that serves its kind.
//
// Every property in this file that is about an ATTACHMENT rather than about one door goes
// through this pair, so that the sixth kind is under the same walks as the other five
// instead of having its own weaker copies of them. Which door a kind is served at is read
// off the package's own answer — the digest kind has a typed door and everything else has
// section 5.11's — and not off a list here.
func encodeAtItsDoor(a *ServerAttachment) ([]byte, error) {
	if a.Kind == AttachmentEpochDigest {
		return EncodeEpochDigestAttachment(a.EpochDigest)
	}
	return EncodeServerAttachment(a)
}

// One encoding parsed at the door that serves the kind named, with the parsed attachment
// rebuilt into the one shape the comparisons in this file take.
func parseAtItsDoor(kind ServerAttachmentKind, bs []byte) (*ServerAttachment, error) {
	if kind == AttachmentEpochDigest {
		digest, err := ParseEpochDigestAttachment(bs)
		if err != nil {
			return nil, err
		}
		return &ServerAttachment{Kind: AttachmentEpochDigest, EpochDigest: digest}, nil
	}
	return ParseServerAttachment(bs)
}

// ── the corpus ──────────────────────────────────────────────────────────────────────

// The tags that make each length prefixed field's filler distinct from every other's. Two
// same width fields filled with the same octets are two fields a swapped encode order
// cannot be seen through, and write_key, read_key, group_context_hash and
// recovery_verify_pub are all 32 octets.
const (
	attachmentWriteKeyTag byte = iota + 0x40
	attachmentReadKeyTag
	attachmentGroupContextTag
	attachmentRecoveryHandleTag
	attachmentRecoveryPubTag
	attachmentWrapTargetTag
)

// How many content rotations the corpus crosses. Three rather than one, so a field written
// or read at the wrong offset lands on octets that are not the ones it should have in more
// than one arrangement.
const attachmentRotations = 3

// Deterministic filler for one field at one rotation. The rotation moves the tag rather
// than the pattern, so two fields of the same width never hold the same octets at any
// rotation.
func attachmentFiller(tag byte, rotation int, n int) []byte {
	return fillBytes(tag+byte(rotation)*0x20, n)
}

// The boundaries every 32 bit field is exercised over: zero, one, the middle of the range,
// the value below the top and the top itself. Zero and 4294967295 are also the two
// durable_ttl_seconds sentinels spec B section 5.4 defines, and both are legal values that
// this layer never refuses — which is why they are in the round trip corpus rather than in
// a refusal test.
func u32Boundaries() []uint32 {
	return []uint32{0, 1, 0x7FFFFFFF, 0xFFFFFFFE, 0xFFFFFFFF}
}

// The same boundaries with zero dropped, for expected_wrap_count, which spec B section 5.1
// check 3 requires to be greater than zero. Derived by filtering rather than written out, so
// a boundary added above reaches this too.
func u32BoundariesAboveZero() []uint32 {
	above := []uint32{}
	for _, value := range u32Boundaries() {
		if value != 0 {
			above = append(above, value)
		}
	}
	return above
}

// One corpus attachment and the name a failure reports it by.
type attachmentCorpusEntry struct {
	name       string
	attachment *ServerAttachment
}

// A valid epoch attachment at one point of the cross product.
func validEpochAttachment(rotation int, epoch uint64, media uint32, durable uint32, count uint32) *ServerAttachment {
	return &ServerAttachment{
		Kind: AttachmentEpoch,
		Epoch: &EpochAttachment{
			Epoch:             epoch,
			AlgId:             attachmentAlgIds[AttachmentEpoch],
			WriteKey:          attachmentFiller(attachmentWriteKeyTag, rotation, epochWriteKeyBytes),
			ReadKey:           attachmentFiller(attachmentReadKeyTag, rotation, epochReadKeyBytes),
			MediaTtlSeconds:   media,
			DurableTtlSeconds: durable,
			GroupContextHash:  attachmentFiller(attachmentGroupContextTag, rotation, epochGroupContextHashBytes),
			ExpectedWrapCount: count,
		},
	}
}

// A valid epoch digest attachment at one point of the cross product.
//
// Its digest is the package's own EpochKeysDigest over the SAME two key ramps
// validEpochAttachment fills write_key and read_key with, which is what makes a corpus
// entry here the corresponding entry there with the keys replaced by their digest — the
// relation the whole amendment is. The vectors are where that relation is pinned against
// arithmetic done outside this package; here it only has to be the same relation everywhere.
func validEpochDigestAttachment(t testing.TB, rotation int, epoch uint64, media uint32, durable uint32, count uint32) *ServerAttachment {
	t.Helper()
	digest, err := EpochKeysDigest(attachmentVectorGroupId(), epoch,
		attachmentFiller(attachmentWriteKeyTag, rotation, epochWriteKeyBytes),
		attachmentFiller(attachmentReadKeyTag, rotation, epochReadKeyBytes))
	if err != nil {
		t.Fatalf("EpochKeysDigest refused two 32 octet keys: %v", err)
	}
	return &ServerAttachment{
		Kind: AttachmentEpochDigest,
		EpochDigest: &EpochDigestAttachment{
			Epoch:             epoch,
			AlgId:             attachmentAlgIds[AttachmentEpochDigest],
			MediaTtlSeconds:   media,
			DurableTtlSeconds: durable,
			GroupContextHash:  attachmentFiller(attachmentGroupContextTag, rotation, epochGroupContextHashBytes),
			ExpectedWrapCount: count,
			EpochKeysDigest:   digest,
		},
	}
}

// A valid recovery tag at one content rotation.
func validRecoveryTag(rotation int) *ServerAttachment {
	return &ServerAttachment{
		Kind: AttachmentRecovery,
		Recovery: &RecoveryTag{
			RecoveryHandle:    attachmentFiller(attachmentRecoveryHandleTag, rotation, recoveryHandleBytes),
			RecoveryVerifyPub: attachmentFiller(attachmentRecoveryPubTag, rotation, recoveryVerifyPubBytes),
			AlgId:             attachmentAlgIds[AttachmentRecovery],
		},
	}
}

// A valid wrap tag at one content rotation and one epoch.
func validWrapTag(rotation int, epoch uint64) *ServerAttachment {
	return &ServerAttachment{
		Kind: AttachmentWrap,
		Wrap: &WrapTag{
			WrapTargetHandle: attachmentFiller(attachmentWrapTargetTag, rotation, wrapTargetHandleBytes),
			Epoch:            epoch,
		},
	}
}

// A valid wrap set marker. Its wrap_count is unbounded here on purpose: the rule it obeys is
// equality against the epoch's own expected_wrap_count, which is an attachment this layer is
// never handed, so zero is in the corpus rather than in a refusal test.
func validEpochComplete(epoch uint64, count uint32) *ServerAttachment {
	return &ServerAttachment{
		Kind:     AttachmentComplete,
		Complete: &EpochComplete{Epoch: epoch, WrapCount: count},
	}
}

// One valid attachment of every kind, keyed by kind, including the absent one.
//
// It is the coverage the tests below index into, and it is asserted to cover every kind the
// encoder admits rather than assumed to: a kind added to the package with no valid
// attachment here would leave every property in this file holding over the four that already
// existed and saying nothing at all about the fifth.
func validAttachmentsByKind(t testing.TB) map[ServerAttachmentKind]*ServerAttachment {
	t.Helper()
	byKind := map[ServerAttachmentKind]*ServerAttachment{
		AttachmentNone:        {Kind: AttachmentNone},
		AttachmentEpoch:       validEpochAttachment(0, 42, 2592000, 0xFFFFFFFF, 1501),
		AttachmentRecovery:    validRecoveryTag(0),
		AttachmentWrap:        validWrapTag(0, 0x100000000),
		AttachmentComplete:    validEpochComplete(42, 1501),
		AttachmentEpochDigest: validEpochDigestAttachment(t, 0, 42, 2592000, 0xFFFFFFFF, 1501),
	}
	if len(byKind) != len(definedAttachmentCodes()) {
		t.Fatalf("there are %d valid attachments and this package defines %d kinds", len(byKind), len(definedAttachmentCodes()))
	}
	for kind, attachment := range byKind {
		if _, err := encodeAtItsDoor(attachment); err != nil {
			t.Fatalf("the valid attachment for kind 0x%04x does not encode: %v", uint16(kind), err)
		}
	}
	return byKind
}

// The corpus: every kind crossed with every boundary the kind's own fields have.
//
// Computed rather than written out, and the axes are the ones section 5.11 gives each body:
// the u64 boundaries on every 64 bit field, the u32 boundaries on every 32 bit field, and
// three content rotations across the length prefixed ones. The 32 bit axis is where both
// durable_ttl_seconds sentinels live, so the round trip is asserted over them rather than
// over the ordinary values alone.
func attachmentCorpus(t testing.TB) []attachmentCorpusEntry {
	t.Helper()
	entries := []attachmentCorpusEntry{{name: "none", attachment: &ServerAttachment{Kind: AttachmentNone}}}
	for rotation := range attachmentRotations {
		for _, epoch := range u64Boundaries() {
			for _, media := range u32Boundaries() {
				for _, durable := range u32Boundaries() {
					for _, count := range u32BoundariesAboveZero() {
						name := fmt.Sprintf("epoch rot=%d epoch=%d media=%d durable=%d wraps=%d",
							rotation, epoch, media, durable, count)
						entries = append(entries, attachmentCorpusEntry{
							name:       name,
							attachment: validEpochAttachment(rotation, epoch, media, durable, count),
						})
					}
				}
			}
		}
		entries = append(entries, attachmentCorpusEntry{
			name:       fmt.Sprintf("recovery rot=%d", rotation),
			attachment: validRecoveryTag(rotation),
		})
		for _, epoch := range u64Boundaries() {
			entries = append(entries, attachmentCorpusEntry{
				name:       fmt.Sprintf("wrap rot=%d epoch=%d", rotation, epoch),
				attachment: validWrapTag(rotation, epoch),
			})
		}
	}
	for _, epoch := range u64Boundaries() {
		for _, count := range u32Boundaries() {
			entries = append(entries, attachmentCorpusEntry{
				name:       fmt.Sprintf("complete epoch=%d wraps=%d", epoch, count),
				attachment: validEpochComplete(epoch, count),
			})
		}
	}
	if len(entries) == 0 {
		t.Fatal("the corpus is empty, so every property asserted over it would hold vacuously")
	}
	kinds := map[ServerAttachmentKind]bool{}
	for _, entry := range entries {
		kinds[entry.attachment.Kind] = true
	}
	// THE SET AND NOT THE COUNT. This compared two numbers, and five kinds is also what a
	// corpus covering four of them and one of them twice would report.
	covered := []int{}
	for kind := range kinds {
		covered = append(covered, int(kind))
	}
	slices.Sort(covered)
	if !slices.Equal(covered, specAttachmentCodes()) {
		t.Fatalf("the corpus covers %v and section 5.11 defines %v", covered, specAttachmentCodes())
	}
	// AND THE COMPLEMENT, ASSERTED RATHER THAN LEFT TO THE READER: the kinds section 5.1
	// check 3's door serves that this cross product does NOT carry.
	//
	// It was empty until 2026-09-23, and holding this corpus against the PROVENANCE table
	// alone went on passing after the door was widened while the sixth kind sat outside every
	// walk written over it — which is how kind 0x0005's only value-level coverage at that door
	// came to be a fuzz seed. The kind is covered by epochDigestCorpus, the same cross product
	// over its own axes, and TestAKindTwoDoorsServeHasOneEncodingAtBoth is what carries that
	// corpus through THIS door. Naming it here fails in both directions: add the sixth kind to
	// this corpus, or narrow the door back, and this line asks for the disposition to be
	// rewritten rather than quietly agreeing.
	missing := []int{}
	for _, code := range servedAttachmentCodes(t, serverAttachmentDoorName) {
		if !kinds[ServerAttachmentKind(code)] {
			missing = append(missing, code)
		}
	}
	if !slices.Equal(missing, ruledAttachmentCodes()) {
		t.Fatalf("this corpus omits %v of the kinds %s serves, and the kinds covered by epochDigestCorpus instead are %v",
			missing, serverAttachmentDoorName, ruledAttachmentCodes())
	}
	return entries
}

// One corpus entry per kind and per 32 bit boundary, for the walks that try all 255
// alternatives at every offset and cannot afford to do it a thousand times over. Derived by
// grouping the corpus, so the subset covers every kind by construction rather than by a
// promise, and the entry chosen for each group is the first the cross product produced.
func attachmentWalkCorpus(t testing.TB) []attachmentCorpusEntry {
	t.Helper()
	seen := map[string]bool{}
	subset := []attachmentCorpusEntry{}
	for _, entry := range attachmentCorpus(t) {
		key := fmt.Sprintf("%d", entry.attachment.Kind)
		if epoch := entry.attachment.Epoch; epoch != nil {
			key = fmt.Sprintf("%s/%d", key, epoch.DurableTtlSeconds)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		subset = append(subset, entry)
	}
	if len(subset) == 0 {
		t.Fatal("the walk subset is empty, so every property asserted over it would hold vacuously")
	}
	return subset
}

// ── helpers ─────────────────────────────────────────────────────────────────────────

// One attachment's encoding, with a refusal fatal: every property below is about the bytes
// of an attachment this package says is valid, and there are none to assert over if the
// encoder refused it.
func mustEncodeAttachment(t testing.TB, what string, a *ServerAttachment) []byte {
	t.Helper()
	bs, err := EncodeServerAttachment(a)
	if err != nil {
		t.Fatalf("%s: EncodeServerAttachment refused a valid attachment: %v", what, err)
	}
	return bs
}

// What differs between two attachments, or the empty string. A field by field comparison
// rather than reflect.DeepEqual, so a failure names the field that moved.
func attachmentDifference(left *ServerAttachment, right *ServerAttachment) string {
	if left.Kind != right.Kind {
		return "Kind"
	}
	if (left.Epoch == nil) != (right.Epoch == nil) {
		return "Epoch presence"
	}
	if left.Epoch != nil {
		switch {
		case left.Epoch.Epoch != right.Epoch.Epoch:
			return "Epoch.Epoch"
		case left.Epoch.AlgId != right.Epoch.AlgId:
			return "Epoch.AlgId"
		case !bytes.Equal(left.Epoch.WriteKey, right.Epoch.WriteKey):
			return "Epoch.WriteKey"
		case !bytes.Equal(left.Epoch.ReadKey, right.Epoch.ReadKey):
			return "Epoch.ReadKey"
		case left.Epoch.MediaTtlSeconds != right.Epoch.MediaTtlSeconds:
			return "Epoch.MediaTtlSeconds"
		case left.Epoch.DurableTtlSeconds != right.Epoch.DurableTtlSeconds:
			return "Epoch.DurableTtlSeconds"
		case !bytes.Equal(left.Epoch.GroupContextHash, right.Epoch.GroupContextHash):
			return "Epoch.GroupContextHash"
		case left.Epoch.ExpectedWrapCount != right.Epoch.ExpectedWrapCount:
			return "Epoch.ExpectedWrapCount"
		}
	}
	if (left.Recovery == nil) != (right.Recovery == nil) {
		return "Recovery presence"
	}
	if left.Recovery != nil {
		switch {
		case !bytes.Equal(left.Recovery.RecoveryHandle, right.Recovery.RecoveryHandle):
			return "Recovery.RecoveryHandle"
		case !bytes.Equal(left.Recovery.RecoveryVerifyPub, right.Recovery.RecoveryVerifyPub):
			return "Recovery.RecoveryVerifyPub"
		case left.Recovery.AlgId != right.Recovery.AlgId:
			return "Recovery.AlgId"
		}
	}
	if (left.Wrap == nil) != (right.Wrap == nil) {
		return "Wrap presence"
	}
	if left.Wrap != nil {
		switch {
		case !bytes.Equal(left.Wrap.WrapTargetHandle, right.Wrap.WrapTargetHandle):
			return "Wrap.WrapTargetHandle"
		case left.Wrap.Epoch != right.Wrap.Epoch:
			return "Wrap.Epoch"
		}
	}
	if (left.Complete == nil) != (right.Complete == nil) {
		return "Complete presence"
	}
	if left.Complete != nil {
		switch {
		case left.Complete.Epoch != right.Complete.Epoch:
			return "Complete.Epoch"
		case left.Complete.WrapCount != right.Complete.WrapCount:
			return "Complete.WrapCount"
		}
	}
	if (left.EpochDigest == nil) != (right.EpochDigest == nil) {
		return "EpochDigest presence"
	}
	if left.EpochDigest != nil {
		switch {
		case left.EpochDigest.Epoch != right.EpochDigest.Epoch:
			return "EpochDigest.Epoch"
		case left.EpochDigest.AlgId != right.EpochDigest.AlgId:
			return "EpochDigest.AlgId"
		case left.EpochDigest.MediaTtlSeconds != right.EpochDigest.MediaTtlSeconds:
			return "EpochDigest.MediaTtlSeconds"
		case left.EpochDigest.DurableTtlSeconds != right.EpochDigest.DurableTtlSeconds:
			return "EpochDigest.DurableTtlSeconds"
		case !bytes.Equal(left.EpochDigest.GroupContextHash, right.EpochDigest.GroupContextHash):
			return "EpochDigest.GroupContextHash"
		case left.EpochDigest.ExpectedWrapCount != right.EpochDigest.ExpectedWrapCount:
			return "EpochDigest.ExpectedWrapCount"
		case !bytes.Equal(left.EpochDigest.EpochKeysDigest, right.EpochDigest.EpochKeysDigest):
			return "EpochDigest.EpochKeysDigest"
		}
	}
	return ""
}

// The body pointer of one attachment as a reflect value, for the reflection driven width
// walk below. Nil for the absent attachment, which has no body to reach into.
func attachmentBodyValue(a *ServerAttachment) reflect.Value {
	switch {
	case a.Epoch != nil:
		return reflect.ValueOf(a.Epoch).Elem()
	case a.Recovery != nil:
		return reflect.ValueOf(a.Recovery).Elem()
	case a.Wrap != nil:
		return reflect.ValueOf(a.Wrap).Elem()
	case a.Complete != nil:
		return reflect.ValueOf(a.Complete).Elem()
	case a.EpochDigest != nil:
		return reflect.ValueOf(a.EpochDigest).Elem()
	}
	return reflect.Value{}
}

// One length prefixed field of one body: the kind it is on, its name, and the width the
// package's own valid attachment gives it.
type attachmentWidthField struct {
	kind  ServerAttachmentKind
	name  string
	width int
}

// Every length prefixed field of every body, read off the go types rather than listed.
//
// Section 5.11 gives six of them an exact width, and spec B section 5.1 check 3 names four
// of the six as things it will rely on this parser having checked. A test that listed them
// would be a list, and this project has been walked past a list twelve times: what is
// derived here is the class — every []byte field of every body — so a seventh such field
// added to any of the four bodies is under the walk the day it is declared, with nobody
// remembering to add it.
//
// The width each one is checked against is the width the package's own valid attachment
// carries, not a number written here, so the walk asks "every length but this one is
// refused" rather than "every length but 32 is refused" — the constant is pinned by the
// golden vectors, and this asserts the property around it.
func attachmentWidthFields(t testing.TB) []attachmentWidthField {
	t.Helper()
	fields := []attachmentWidthField{}
	byKind := validAttachmentsByKind(t)
	for _, code := range definedAttachmentCodes() {
		kind := ServerAttachmentKind(code)
		body := attachmentBodyValue(byKind[kind])
		if !body.IsValid() {
			continue
		}
		for i := range body.NumField() {
			field := body.Type().Field(i)
			if field.Type.Kind() != reflect.Slice || field.Type.Elem().Kind() != reflect.Uint8 {
				continue
			}
			fields = append(fields, attachmentWidthField{kind: kind, name: field.Name, width: body.Field(i).Len()})
		}
	}
	if len(fields) == 0 {
		t.Fatal("no body carries a length prefixed field, so the width walk below would hold vacuously")
	}
	return fields
}

// ── the alphabet ────────────────────────────────────────────────────────────────────

// Every kind code the encoder will write, derived by offering it all 65536 of them crossed
// with every body shape this package has.
//
// The cross with the body shapes is what makes it a derivation of the encoder's answer
// rather than of the kind constants: a code that only encodes when it is handed the right
// body is still a code the encoder writes, and one that encodes with any body at all is a
// package that has stopped checking the two against each other.
func encodableKindCodes(t testing.TB) []int {
	t.Helper()
	bodies := []func(kind ServerAttachmentKind) *ServerAttachment{
		func(kind ServerAttachmentKind) *ServerAttachment { return &ServerAttachment{Kind: kind} },
		func(kind ServerAttachmentKind) *ServerAttachment {
			a := validEpochAttachment(0, 7, 1, 2, 3)
			a.Kind = kind
			return a
		},
		func(kind ServerAttachmentKind) *ServerAttachment {
			a := validRecoveryTag(0)
			a.Kind = kind
			return a
		},
		func(kind ServerAttachmentKind) *ServerAttachment {
			a := validWrapTag(0, 7)
			a.Kind = kind
			return a
		},
		func(kind ServerAttachmentKind) *ServerAttachment {
			a := validEpochComplete(7, 3)
			a.Kind = kind
			return a
		},
		// the sixth body is offered to this door as well, so the answer below is the door's
		// and not an artefact of never having handed it one
		func(kind ServerAttachmentKind) *ServerAttachment {
			a := validEpochDigestAttachment(t, 0, 7, 1, 2, 3)
			a.Kind = kind
			return a
		},
	}
	codes := []int{}
	for code := 0; code <= 0xFFFF; code++ {
		kind := ServerAttachmentKind(code)
		for _, body := range bodies {
			if _, err := EncodeServerAttachment(body(kind)); err == nil {
				codes = append(codes, code)
				break
			}
		}
	}
	if len(codes) == 0 {
		t.Fatal("the encoder wrote no kind at all, so every assertion below would hold vacuously")
	}
	return codes
}

// Each door serves exactly the kinds written down for it, and the two answers are compared
// in BOTH directions.
//
// This is the disposition assertion the rest of the door walks rest on: attachmentDoorServes
// is what a reader decided, serverAttachmentKindServed and epochDigestKindServed are what the
// package does, and either one moving without the other is a failure here rather than a
// silent widening found later. A kind served but not written down is a door that grew reach
// nobody recorded; a kind written down but not served is a table that describes a build that
// does not exist.
//
// THE COMPLEMENTS ARE PRINTED AND THEIR UNION IS ASSERTED NON-EMPTY. Section 5.1 check 3's
// complement is empty as of 2026-09-23, which is legitimate and is the whole of what this
// commit did; every complement being empty is not, because then no door in this package
// refuses a defined kind at all and the mechanism the next test gates has nothing to hold
// over.
func TestEachDoorServesExactlyTheKindsWrittenDownForIt(t *testing.T) {
	packageAnswer := attachmentServedMaps(t)
	unservedSomewhere := 0
	for _, door := range attachmentDoorNames() {
		served, wired := packageAnswer[door]
		if !wired {
			t.Fatalf("door %q is written down and no map of this package answers for it", door)
		}
		written := map[int]bool{}
		for _, code := range servedAttachmentCodes(t, door) {
			written[code] = true
		}
		for _, code := range definedAttachmentCodes() {
			kind := ServerAttachmentKind(code)
			if served[kind] && !written[code] {
				t.Errorf("%s serves kind 0x%04x and nothing writes that down", door, code)
			}
			if written[code] && !served[kind] {
				t.Errorf("%s is written down as serving kind 0x%04x and its map does not", door, code)
			}
		}
		complement := unservedAttachmentCodes(t, door)
		unservedSomewhere += len(complement)
		t.Logf("%s serves %v and refuses %v of the kinds this package defines", door, servedAttachmentCodes(t, door), complement)
	}
	if unservedSomewhere == 0 {
		t.Fatal("no door refuses any kind this package defines, so the by-name refusal is gated over nothing")
	}
	// and the reach of section 5.1 check 3's door, said against provenance rather than
	// against itself: it serves every code this package defines, which is section 5.11's
	// five plus ruling 27's sixth. This is the line that fails if a seventh kind is defined
	// and nobody decides whether that door serves it.
	if want := definedAttachmentCodes(); !slices.Equal(servedAttachmentCodes(t, serverAttachmentDoorName), want) {
		t.Errorf("%s serves %v and this package defines %v", serverAttachmentDoorName,
			servedAttachmentCodes(t, serverAttachmentDoorName), want)
	}
	if want := ruledAttachmentCodes(); !slices.Equal(servedAttachmentCodes(t, epochDigestDoorName), want) {
		t.Errorf("%s serves %v and ruling 27 defines %v", epochDigestDoorName,
			servedAttachmentCodes(t, epochDigestDoorName), want)
	}
}

// EVERY DOOR'S SERVED MAP IS CONSULTED BY THE ENTRY POINTS WRITTEN DOWN FOR IT, MEASURED BY
// NARROWING THE MAP AND WATCHING THE DOOR.
//
// WHAT THIS TEST IS FOR, AND WHY THE TEST ABOVE IS NOT IT. The test above pins the CONTENTS
// of serverAttachmentKindServed and epochDigestKindServed against the written down
// disposition. Nothing pinned the CALL SITES that consult them. Those are two different
// properties and the difference had teeth as of 2026-09-23: with section 5.1 check 3's door
// widened to every kind this package defines, unservedAttachmentCodes for that door is the
// empty slice, so TestARecordCarriesAKindADoorRefusesByName's refusal arm visits ZERO pairs
// at that door and only its acceptance arm runs there. Measured on the tree this test landed
// on: deleting the three line checkAttachmentKindServed block from EncodeServerAttachment
// left `ok message 6.843s`, `ok messagegroup 9.440s` — SURVIVING — and deleting the same
// block from ParseServerAttachment left `ok message 6.754s`, `ok messagegroup 9.157s`. The
// inline positive control, deleting the same block from ParseEpochDigestAttachment, reddened
// seven tests, so the suite CAN see a deleted served check; it simply could not see one at
// the door whose complement had just gone empty. One commit earlier, at 241b9000, each of
// those two deletions reddened four tests. That is a gate that got NARROWER while its commit
// message said it had got wider, and this test is the replacement that is actually stronger:
// it does not depend on any complement being non-empty, because it MAKES one.
//
// HOW. For each (door, kind) pair the disposition table says is served, the kind is deleted
// from that door's package map for the length of one closure and the door is asked to encode
// and to parse it. The refusal has to come back by sentinel, naming the kind and naming the
// door. Then the entry is restored and the same two calls have to succeed again, against the
// octets captured before the narrowing — so the assertion fails in BOTH directions: a call
// site that stops consulting the map fails the narrowed half, and a door that refuses what
// its map holds fails the restored half.
//
// THE HALVES ARE A DISPOSITION AND THE COMPLEMENT IS ASSERTED, NOT PRINTED. Not every door
// has both halves, and the asymmetry is real rather than an omission — see
// attachmentDoorRuntimeHalves. A half the table does NOT list is required to SUCCEED while
// the map is narrowed, which is what keeps the table honest: adding a served check to
// EncodeEpochDigestAttachment without writing it down fails here, and so does a table entry
// for a half that does not consult the map.
//
// THE POSITIVE HALF RUNS INSIDE THE SAME NARROWING, because a door that refused everything
// while the map was narrowed would satisfy every refusal above and say nothing about the
// map. It is taken two ways and at least one is required to exist for every pair: another
// kind THIS door still serves, and the same kind at ANOTHER door whose map was not touched.
func TestEachDoorsServedMapIsConsultedByTheEntryPointsWrittenDownForIt(t *testing.T) {
	byKind := validAttachmentsByKind(t)
	servedMaps := attachmentServedMaps(t)

	// THE HALVES TABLE AGAINST THE DOOR TABLE, BOTH DIRECTIONS, before anything is narrowed:
	// a door with no entry is a door whose call sites nothing below would witness, and an
	// entry for a door that is gone is a sentence about this package that has stopped being
	// true.
	for _, door := range attachmentDoorNames() {
		halves, written := attachmentDoorRuntimeHalves[door]
		if !written {
			t.Fatalf("door %q is written down in attachmentDoorServes and attachmentDoorRuntimeHalves "+
				"says nothing about which of its entry points consult its served map, so this test "+
				"would witness none of them", door)
		}
		if len(halves) == 0 {
			t.Fatalf("attachmentDoorRuntimeHalves gives %q no half at all; a door none of whose entry "+
				"points asks its served map is a door whose map is decoration", door)
		}
	}
	for door := range attachmentDoorRuntimeHalves {
		if _, known := attachmentDoorServes[door]; !known {
			t.Errorf("attachmentDoorRuntimeHalves names %q and no such door is written down in "+
				"attachmentDoorServes", door)
		}
	}

	witnessed, unwitnessed := 0, 0
	for _, door := range attachmentDoorNames() {
		served := servedMaps[door]
		codes := servedAttachmentCodes(t, door)
		for _, code := range codes {
			kind := ServerAttachmentKind(code)
			// the octets this kind's OWN door writes, captured before the narrowing, so the
			// parse half has something to offer the narrowed door and the restored half has
			// something exact to compare against. encodeAtItsDoor is used rather than
			// encodeAtDoor because the door under narrowing is about to refuse.
			slot, err := encodeAtItsDoor(byKind[kind])
			if err != nil {
				t.Fatalf("kind 0x%04x does not encode at its own door before any narrowing: %v", code, err)
			}

			// the positive half's two forms, decided before the narrowing and required to
			// have found at least one between them
			otherKindAtThisDoor := -1
			for _, other := range codes {
				if other != code {
					otherKindAtThisDoor = other
					break
				}
			}
			otherDoorsServingThisKind := []string{}
			for _, other := range attachmentDoorsServing(t, code) {
				if other != door {
					otherDoorsServingThisKind = append(otherDoorsServingThisKind, other)
				}
			}
			if otherKindAtThisDoor < 0 && len(otherDoorsServingThisKind) == 0 {
				t.Fatalf("narrowing kind 0x%04x out of %s leaves nothing in the same run that must "+
					"still be served, so the refusals below would be indistinguishable from a door "+
					"that had stopped working altogether", code, door)
			}

			func() {
				was, present := served[kind]
				defer func() {
					if present {
						served[kind] = was
					} else {
						delete(served, kind)
					}
				}()
				delete(served, kind)

				for _, half := range []string{attachmentEncodeHalf, attachmentParseHalf} {
					var answerErr error
					switch half {
					case attachmentEncodeHalf:
						_, answerErr = encodeAtDoor(t, door, byKind[kind])
					case attachmentParseHalf:
						_, answerErr = parseAtDoor(t, door, slot)
					}
					if !slices.Contains(attachmentDoorRuntimeHalves[door], half) {
						// THE COMPLEMENT, ASSERTED. This half is written down as NOT asking the
						// map, so narrowing the map may not change its answer. The epoch digest
						// door's encode half is the whole of this today: it takes the BODY, so a
						// kind it does not serve cannot be spelled at it.
						if answerErr != nil {
							t.Errorf("attachmentDoorRuntimeHalves says the %s half of %s does not "+
								"consult its served map, and with kind 0x%04x deleted from that map "+
								"it refused: %v. Either it consults the map now and the table has to "+
								"say so, or something else broke.", half, door, code, answerErr)
						}
						unwitnessed++
						continue
					}
					if answerErr == nil {
						t.Errorf("kind 0x%04x was deleted from %s's served map and the %s half served "+
							"it anyway, so nothing in that entry point asks the map and deleting the "+
							"checkAttachmentKindServed call from it would change no test's answer. "+
							"This is the exact regression of 2026-09-23: the map's CONTENTS are pinned "+
							"by TestEachDoorServesExactlyTheKindsWrittenDownForIt and its CALL SITES "+
							"were pinned by nothing.", code, door, half)
						continue
					}
					if !errors.Is(answerErr, ErrServerAttachmentKindNotServed) {
						t.Errorf("the %s half of %s refused the narrowed kind 0x%04x with %v, want "+
							"ErrServerAttachmentKindNotServed; a different sentinel means the refusal "+
							"came from somewhere other than the served check", half, door, code, answerErr)
						continue
					}
					if named := fmt.Sprintf("0x%04x", code); !strings.Contains(answerErr.Error(), named) {
						t.Errorf("the %s half of %s refused the narrowed kind %s with %q, which does "+
							"not name the kind", half, door, named, answerErr.Error())
					}
					if !strings.Contains(answerErr.Error(), door) {
						t.Errorf("the %s half of %s refused the narrowed kind 0x%04x with %q, which "+
							"does not name the door that answered", half, door, code, answerErr.Error())
					}
					witnessed++
				}

				// THE POSITIVE HALF, INSIDE THE NARROWING. What is still in the map is still
				// served, at this door and at any other door that serves this kind, so the
				// refusals above are about the one entry that was deleted.
				if otherKindAtThisDoor >= 0 {
					other := ServerAttachmentKind(otherKindAtThisDoor)
					otherSlot, err := encodeAtItsDoor(byKind[other])
					if err != nil {
						t.Fatalf("kind 0x%04x does not encode at its own door: %v", otherKindAtThisDoor, err)
					}
					if _, err := parseAtDoor(t, door, otherSlot); err != nil {
						t.Errorf("with kind 0x%04x deleted from %s's served map, that door also refused "+
							"kind 0x%04x, which is still in it: %v. The refusals above are then about "+
							"the door being broken and not about the narrowing.",
							code, door, otherKindAtThisDoor, err)
					}
				}
				for _, other := range otherDoorsServingThisKind {
					if _, err := parseAtDoor(t, other, slot); err != nil {
						t.Errorf("kind 0x%04x was deleted from %s's served map and %s — a different "+
							"door, whose map was not touched — refused it too: %v. One door's map is "+
							"then not one door's map.", code, door, other, err)
					}
				}
			}()

			// AND THE RESTORED HALF, WHICH IS THE OTHER DIRECTION: the entry is back and the
			// door serves the kind again, by value and by octet. Without this the whole test
			// would be satisfied by a door that refused everything.
			if !served[kind] {
				t.Fatalf("kind 0x%04x was not restored to %s's served map, so every assertion after "+
					"this one runs against a package this test broke", code, door)
			}
			if slices.Contains(attachmentDoorRuntimeHalves[door], attachmentEncodeHalf) {
				again, err := encodeAtDoor(t, door, byKind[kind])
				if err != nil {
					t.Errorf("%s refused to encode kind 0x%04x after its map entry was restored: %v", door, code, err)
				} else if !bytes.Equal(again, slot) {
					t.Errorf("%s encoded kind 0x%04x differently after its map entry was restored: %s",
						door, code, firstOctetDifference(slot, again))
				}
			}
			parsed, err := parseAtDoor(t, door, slot)
			if err != nil {
				t.Errorf("%s refused to parse kind 0x%04x after its map entry was restored: %v", door, code, err)
			} else if difference := attachmentDifference(byKind[kind], parsed); difference != "" {
				t.Errorf("%s parsed kind 0x%04x after its map entry was restored and handed back an "+
					"attachment whose %s differs", door, code, difference)
			}
		}
	}

	// THE COUNT IS DERIVED FROM THE TWO TABLES AND COMPARED, rather than logged. A walk that
	// silently stopped visiting a door — the failure this whole test exists to catch, one
	// level up — would leave the loop green and the number short.
	wantWitnessed, wantUnwitnessed := 0, 0
	for _, door := range attachmentDoorNames() {
		for range servedAttachmentCodes(t, door) {
			for _, half := range []string{attachmentEncodeHalf, attachmentParseHalf} {
				if slices.Contains(attachmentDoorRuntimeHalves[door], half) {
					wantWitnessed++
				} else {
					wantUnwitnessed++
				}
			}
		}
	}
	if witnessed != wantWitnessed || unwitnessed != wantUnwitnessed {
		t.Errorf("this walk witnessed %d served-map refusals and %d unaffected halves, and the tables "+
			"say there are %d and %d; the difference is (door, kind, half) triples nothing here asked about",
			witnessed, unwitnessed, wantWitnessed, wantUnwitnessed)
	}
	if witnessed == 0 {
		t.Fatal("no served map was witnessed being consulted at all, so every checkAttachmentKindServed " +
			"call site in this package could be deleted with this test still green")
	}
	t.Logf("%d (door, kind, half) triples refused a narrowed served map by name and %d halves are "+
		"written down as not consulting one, over %d doors", witnessed, unwitnessed, len(attachmentDoorNames()))
}

// The alphabet, encode side: the encoder writes exactly the codes section 5.1 check 3's door
// is written down as serving.
//
// It was "exactly the five codes section 5.11 defines" and the number was pinned at five. The
// number is not the property — the door's reach is — and pinning it was what would have made
// this test the thing that had to be edited rather than the thing that reported the edit. It
// now names the door, and the five/six question is asked once, against provenance, in
// TestEachDoorServesExactlyTheKindsWrittenDownForIt.
func TestTheEncoderWritesExactlyTheKindsItsDoorServes(t *testing.T) {
	codes := encodableKindCodes(t)
	want := servedAttachmentCodes(t, serverAttachmentDoorName)
	if !slices.Equal(codes, want) {
		t.Errorf("the encoder writes %v, want exactly %v; every other u16 is a kind this door does not write", codes, want)
	}
	// the complement, in the same test rather than inferred from the equality: what the
	// encoder REFUSES of the codes this package defines
	refused := []int{}
	byKind := validAttachmentsByKind(t)
	for _, code := range definedAttachmentCodes() {
		if _, err := EncodeServerAttachment(byKind[ServerAttachmentKind(code)]); err != nil {
			refused = append(refused, code)
		}
	}
	if !slices.Equal(refused, unservedAttachmentCodes(t, serverAttachmentDoorName)) {
		t.Errorf("the encoder refuses %v of the defined codes and %s is written down as refusing %v",
			refused, serverAttachmentDoorName, unservedAttachmentCodes(t, serverAttachmentDoorName))
	}
}

// The alphabet, parse side: an encoding parses under its own kind code and under no other.
//
// Derived rather than listed. Each kind's valid encoding has its leading u16 replaced by
// each of the 65536 values in turn, and the set that parses has to be the one code that
// encoding was built with. A kind accepted for a body that is not its own is the defect this
// catches — a parser that read the body first and the kind afterwards, or one that fell back
// to a default kind — and so is a kind silently ignored, which spec B section 5.1 check 3
// cannot survive: an attachment the server cannot parse is one it cannot check.
//
// The walk is over the kinds this DOOR serves and not over section 5.11's five. It read the
// latter, which meant the same set until 2026-09-23 and then silently stopped covering the
// sixth kind at the door that had just been widened to it.
func TestAnEncodingParsesUnderItsOwnKindAndNoOther(t *testing.T) {
	byKind := validAttachmentsByKind(t)
	tried := 0
	for _, kind := range servedAttachmentCodes(t, serverAttachmentDoorName) {
		attachment := byKind[ServerAttachmentKind(kind)]
		if attachment == nil || attachment.Kind == AttachmentNone {
			continue
		}
		valid := mustEncodeAttachment(t, specAttachmentKindNames[attachment.Kind], attachment)
		accepted := []int{}
		relabelled := slices.Clone(valid)
		for code := 0; code <= 0xFFFF; code++ {
			relabelled[0] = byte(code >> 8)
			relabelled[1] = byte(code)
			if _, err := ParseServerAttachment(relabelled); err == nil {
				accepted = append(accepted, code)
			}
		}
		if !slices.Equal(accepted, []int{kind}) {
			t.Errorf("the %s encoding parses under %v, want exactly [%d]", specAttachmentKindNames[attachment.Kind], accepted, kind)
		}
		tried++
	}
	if tried == 0 {
		t.Fatal("no kind carried a body, so this walk relabelled nothing")
	}
	t.Logf("%d kinds relabelled across all 65536 codes each", tried)
}

// Each of the five codes is the number section 5.11 gives it.
//
// The one thing no derived property can see. A table permuted in both directions round trips
// perfectly and agrees with nobody, and the kind reaches the write_auth mac and both aeads
// through H(server_attachment), so a permutation is a record every other implementation
// refuses at an authenticator rather than anywhere legible.
func TestEveryKindCodeIsTheOneSectionFiveElevenGives(t *testing.T) {
	for kind, want := range specAttachmentKindCodes {
		if uint16(kind) != want {
			t.Errorf("%s is 0x%04x and section 5.11 gives it 0x%04x", specAttachmentKindNames[kind], uint16(kind), want)
		}
	}
	codes := map[uint16]bool{}
	for kind := range specAttachmentKindCodes {
		codes[uint16(kind)] = true
	}
	if len(codes) != len(specAttachmentKindCodes) {
		t.Errorf("the five kinds take %d distinct codes; two of them are the same number", len(codes))
	}
}

// ── the golden vectors ──────────────────────────────────────────────────────────────

// The epoch attachment, pinned to its exact octets.
//
// Derived by hand from section 5.11's block, one line per field:
//
//	0001                              u16(kind): 0x0001, an EpochAttachment
//	00000082                          LP(body): 130 octets, the sum of the lines below
//	000000000000002a                  u64(epoch): 42, the epoch this attachment OPENS
//	0031                              u16(alg_id): HKDF-SHA-256, master section 7.1
//	00000020 70..8f                   LP(write_key): 32 octets, a ramp from 0x70
//	00000020 90..af                   LP(read_key): 32 octets, a ramp from 0x90
//	00278d00                          u32(media_ttl_seconds): 2592000, thirty days
//	ffffffff                          u32(durable_ttl_seconds): the indefinite sentinel
//	00000020 c0..df                   LP(group_context_hash): 32 octets, a ramp from 0xc0
//	000005dd                          u32(expected_wrap_count): 1501
//
// The body adds to 8 + 2 + 36 + 36 + 4 + 4 + 36 + 4 = 130 = 0x82, and the whole attachment
// to 2 + 4 + 130 = 136.
//
// 1501 is section 5.11's own sizing at the 500 member by 2 device design target: 1000 device
// wraps, 500 recovery wraps and one snapshot. It is in the vector rather than a round number
// because it is the value the spec's own arithmetic produces.
const attachmentEpochVectorHex = "0001" +
	"00000082" +
	"000000000000002a" +
	"0031" +
	"00000020" + "707172737475767778797a7b7c7d7e7f808182838485868788898a8b8c8d8e8f" +
	"00000020" + "909192939495969798999a9b9c9d9e9fa0a1a2a3a4a5a6a7a8a9aaabacadaeaf" +
	"00278d00" +
	"ffffffff" +
	"00000020" + "c0c1c2c3c4c5c6c7c8c9cacbcccdcecfd0d1d2d3d4d5d6d7d8d9dadbdcdddedf" +
	"000005dd"

// The recovery tag, pinned to its exact octets.
//
//	0002                              u16(kind): 0x0002, a RecoveryTag
//	0000003a                          LP(body): 58 octets, the sum of the lines below
//	00000010 a0..af                   LP(recovery_handle): 16 octets, a ramp from 0xa0
//	00000020 10..2f                   LP(recovery_verify_pub): 32 octets, a ramp from 0x10
//	0001                              u16(alg_id): Ed25519, master section 7.1
//
// The body adds to 20 + 36 + 2 = 58 = 0x3a, and the whole attachment to 2 + 4 + 58 = 64.
//
// The alg_id is last, after the two length prefixed fields. That order is section 5.11's and
// it is the one field of this body an implementation is likeliest to move to the front, where
// every other message in the system carries it.
const attachmentRecoveryVectorHex = "0002" +
	"0000003a" +
	"00000010" + "a0a1a2a3a4a5a6a7a8a9aaabacadaeaf" +
	"00000020" + "101112131415161718191a1b1c1d1e1f202122232425262728292a2b2c2d2e2f" +
	"0001"

// The wrap tag, pinned to its exact octets.
//
//	0003                              u16(kind): 0x0003, a WrapTag
//	0000001c                          LP(body): 28 octets, the sum of the lines below
//	00000010 50..5f                   LP(wrap_target_handle): 16 octets, a ramp from 0x50
//	0000000100000000                  u64(epoch): 4294967296
//
// The body adds to 20 + 8 = 28 = 0x1c, and the whole attachment to 2 + 4 + 28 = 34.
//
// The epoch is the first value that does not fit in 32 bits, chosen so the vector says the
// field is 64 bits wide: read as a u32 it is zero, and read with its halves swapped it is 1.
const attachmentWrapVectorHex = "0003" +
	"0000001c" +
	"00000010" + "505152535455565758595a5b5c5d5e5f" +
	"0000000100000000"

// The wrap set marker, pinned to its exact octets.
//
//	0004                              u16(kind): 0x0004, an EpochComplete
//	0000000c                          LP(body): 12 octets, the sum of the lines below
//	000000000000002a                  u64(epoch): 42, the same epoch the vector above opens
//	000005dd                          u32(wrap_count): 1501, that epoch's expected_wrap_count
//
// The body adds to 8 + 4 = 12 = 0x0c, and the whole attachment to 2 + 4 + 12 = 18.
const attachmentCompleteVectorHex = "0004" +
	"0000000c" +
	"000000000000002a" +
	"000005dd"

// ── the epoch digest vectors, and the digest they are about ─────────────────────────
//
// THESE ARE THE INTEROP VECTORS RULING 31 ASKS FOR, and the whole of what makes them worth
// writing is that a second implementation can reach them without this package. The digest
// below was derived by a program that imports nothing from here — the preimage is written
// out octet by octet and the SHA-256 over it is stated — and the two keys it is taken over
// are the SAME two ramps the kind 0x0001 vector above already pins on the wire. So the
// relation ruling 27 is can be checked end to end from two numbers that were both fixed
// outside this encoder: take the 0x0001 vector, lift write_key and read_key out of it,
// hash them with the epoch they open, and land on the field below.

// The group_id every epoch_keys vector in this file is taken over, and it is not a fresh
// ramp: it is aadKatCommitHeader's own GroupId — the group of the commit whose attachment
// the kind 0x0001 vector IS, which this file already asserts is aad_test.go's own octets.
// Ruling 34 put LP(group_id) in the preimage to make the digest a statement about an epoch
// OF A GROUP, and taking the group from anywhere but the record these vectors are about
// would have made it a statement about nothing.
const attachmentEpochKeysGroupHex = "1112131415161718191a1b1c1d1e1f202122232425262728292a2b2c2d2e2f30"

// The same value as the [32]byte the three functions take, which is the record header's own
// type for a group id.
func attachmentVectorGroupId() [32]byte {
	var out [32]byte
	copy(out[:], aadRamp(0x11, 32))
	return out
}

// The epoch_keys preimage of the vector below, pinned separately from its digest.
//
// A digest alone moves as one opaque number whichever term went missing from under it, so
// the preimage is pinned as well and derived by hand, one line per term:
//
//	55526d…6b657973                   "URmessage/v1/epochkeys", raw ascii, no prefix,
//	                                  twenty two octets
//	00000020 11..30                   LP(group_id): 32 octets, the KAT commit's own group
//	000000000000002a                  u64(opens_epoch): 42, the epoch these keys open
//	00000020 70..8f                   LP(write_key): 32 octets, the 0x0001 vector's ramp
//	00000020 90..af                   LP(read_key): 32 octets, the 0x0001 vector's ramp
//
// which adds to 22 + 36 + 8 + 36 + 36 = 138 octets.
//
// THE GROUP IS AHEAD OF THE EPOCH, and this is where that ordering is pinned rather than
// argued: an implementation that wrote the same five terms with LP(group_id) after
// u64(opens_epoch) reproduces neither digest below, at either epoch.
const attachmentEpochKeysPreimageHex = "55526d6573736167652f76312f65706f63686b657973" +
	"00000020" + attachmentEpochKeysGroupHex +
	"000000000000002a" +
	"00000020" + "707172737475767778797a7b7c7d7e7f808182838485868788898a8b8c8d8e8f" +
	"00000020" + "909192939495969798999a9b9c9d9e9fa0a1a2a3a4a5a6a7a8a9aaabacadaeaf"

// SHA-256 of the 138 octets above.
//
// (It was 7c90acd7…f650dc82, over 102 octets with no group term, until ruling 34 — taken
// while the kind was one commit old and nothing consumed the digest. The superseded number
// is written down because a vector that moved is worth more than a vector that was always
// this: an implementation still reproducing it has the pre-ruling preimage.)
const attachmentEpochKeysDigestHex = "73121debb8fa9aed4f70847f1c90314f1f1e99795906acaa7c5b3ee54518eafa"

// The same two keys and the same group at opens_epoch = 1 rather than 42, and its digest.
//
// It is here so that the interop check for the epoch term is a VECTOR and not a property
// over this package's own output: an implementation that left u64(opens_epoch) out of the
// preimage reproduces neither of these two numbers, and an implementation that reproduces
// one and not the other has put the epoch somewhere else in the preimage than here.
const attachmentEpochKeysPreimageEpochOneHex = "55526d6573736167652f76312f65706f63686b657973" +
	"00000020" + attachmentEpochKeysGroupHex +
	"0000000000000001" +
	"00000020" + "707172737475767778797a7b7c7d7e7f808182838485868788898a8b8c8d8e8f" +
	"00000020" + "909192939495969798999a9b9c9d9e9fa0a1a2a3a4a5a6a7a8a9aaabacadaeaf"

// (Was 607906ee…fb8c8123 over 102 octets, for the reason above.)
const attachmentEpochKeysDigestEpochOneHex = "54395ba26817b7a906bf0279ac3ed86447cbb98005e90b8b2164df973fb4172c"

// The epoch digest attachment, pinned to its exact octets.
//
// Derived by hand from the block at the top of attachment.go, one line per field. It is the
// kind 0x0001 vector with its two LP keys struck and one LP digest appended, which is the
// amendment and is why the two vectors share every other octet:
//
//	0005                              u16(kind): 0x0005, an EpochDigest
//	0000005e                          LP(body): 94 octets, the sum of the lines below
//	000000000000002a                  u64(epoch): 42, the epoch this attachment OPENS
//	0031                              u16(alg_id): HKDF-SHA-256, master section 7.1
//	00278d00                          u32(media_ttl_seconds): 2592000, thirty days
//	ffffffff                          u32(durable_ttl_seconds): the indefinite sentinel
//	00000020 c0..df                   LP(group_context_hash): 32 octets, a ramp from 0xc0
//	000005dd                          u32(expected_wrap_count): 1501
//	00000020 7c90..dc82               LP(H(epoch_keys)): the digest pinned above
//
// The body adds to 8 + 2 + 4 + 4 + 36 + 4 + 36 = 94 = 0x5e, and the whole attachment to
// 2 + 4 + 94 = 100 — thirty six octets shorter than the 0x0001 vector, which is exactly the
// two LP framed keys that left.
const attachmentEpochDigestVectorHex = "0005" +
	"0000005e" +
	"000000000000002a" +
	"0031" +
	"00278d00" +
	"ffffffff" +
	"00000020" + "c0c1c2c3c4c5c6c7c8c9cacbcccdcecfd0d1d2d3d4d5d6d7d8d9dadbdcdddedf" +
	"000005dd" +
	"00000020" + attachmentEpochKeysDigestHex

// The second epoch digest vector: the OTHER durable_ttl_seconds sentinel, an
// expected_wrap_count of one, and the digest at opens_epoch = 1.
//
//	0005                              u16(kind): 0x0005, an EpochDigest
//	0000005e                          LP(body): 94 octets, the same 94 — no field of this
//	                                  body has a variable width, so both sentinels and both
//	                                  wrap counts encode to one length
//	0000000000000001                  u64(epoch): 1, the first epoch a commit ever opens
//	0031                              u16(alg_id): HKDF-SHA-256
//	00000000                          u32(media_ttl_seconds): 0
//	00000000                          u32(durable_ttl_seconds): the UNSET sentinel, the one
//	                                  the vector above does not carry
//	00000020 c0..df                   LP(group_context_hash): 32 octets, a ramp from 0xc0
//	00000001                          u32(expected_wrap_count): 1, the smallest legal fan
//	                                  out — an epoch that opens with its own snapshot alone
//	00000020 6079..8123                LP(H(epoch_keys)): the epoch-1 digest pinned above
const attachmentEpochDigestSecondVectorHex = "0005" +
	"0000005e" +
	"0000000000000001" +
	"0031" +
	"00000000" +
	"00000000" +
	"00000020" + "c0c1c2c3c4c5c6c7c8c9cacbcccdcecfd0d1d2d3d4d5d6d7d8d9dadbdcdddedf" +
	"00000001" +
	"00000020" + attachmentEpochKeysDigestEpochOneHex

// The five vectors by kind, so the coverage assertion below can be about the set rather than
// about five function names somebody remembered to write.
var attachmentGoldenVectors = map[ServerAttachmentKind]string{
	AttachmentEpoch:       attachmentEpochVectorHex,
	AttachmentRecovery:    attachmentRecoveryVectorHex,
	AttachmentWrap:        attachmentWrapVectorHex,
	AttachmentComplete:    attachmentCompleteVectorHex,
	AttachmentEpochDigest: attachmentEpochDigestVectorHex,
}

// The attachment each vector is of, built from the same values the derivation above names.
func attachmentGoldenValues() map[ServerAttachmentKind]*ServerAttachment {
	return map[ServerAttachmentKind]*ServerAttachment{
		AttachmentEpoch: {
			Kind: AttachmentEpoch,
			Epoch: &EpochAttachment{
				Epoch:             42,
				AlgId:             0x0031,
				WriteKey:          aadRamp(0x70, 32),
				ReadKey:           aadRamp(0x90, 32),
				MediaTtlSeconds:   2592000,
				DurableTtlSeconds: 0xFFFFFFFF,
				GroupContextHash:  aadRamp(0xc0, 32),
				ExpectedWrapCount: 1501,
			},
		},
		AttachmentRecovery: {
			Kind: AttachmentRecovery,
			Recovery: &RecoveryTag{
				RecoveryHandle:    aadRamp(0xa0, 16),
				RecoveryVerifyPub: aadRamp(0x10, 32),
				AlgId:             0x0001,
			},
		},
		AttachmentWrap: {
			Kind: AttachmentWrap,
			Wrap: &WrapTag{
				WrapTargetHandle: aadRamp(0x50, 16),
				Epoch:            0x100000000,
			},
		},
		AttachmentComplete: {
			Kind:     AttachmentComplete,
			Complete: &EpochComplete{Epoch: 42, WrapCount: 1501},
		},
		AttachmentEpochDigest: {
			Kind: AttachmentEpochDigest,
			EpochDigest: &EpochDigestAttachment{
				Epoch:             42,
				AlgId:             0x0031,
				MediaTtlSeconds:   2592000,
				DurableTtlSeconds: 0xFFFFFFFF,
				GroupContextHash:  aadRamp(0xc0, 32),
				ExpectedWrapCount: 1501,
				// the digest is the hexadecimal string pinned above and not a call to
				// EpochKeysDigest: a vector built from the function it pins is a vector
				// that moves with it, and the whole point of this one is that the number
				// came from outside this package.
				EpochKeysDigest: mustHex(attachmentEpochKeysDigestHex),
			},
		},
	}
}

// One pinned hexadecimal string as octets. It panics rather than reporting, because every
// caller is a constant in this file and a constant that is not hexadecimal is a typo rather
// than an input.
func mustHex(s string) []byte {
	bs, err := hex.DecodeString(s)
	if err != nil {
		panic(fmt.Sprintf("a pinned vector is not hexadecimal: %v", err))
	}
	return bs
}

// Every kind is pinned to its exact octets, and back.
//
// The vectors are the anchors that do not move with the code. rawAttachment states the
// layout a second time but lives beside the encoder, so a field order permuted in the raw
// builder and in writeAttachmentBody together passes every comparison against it; a hand
// derived hexadecimal string does not move at all, and a permutation lands on it.
func TestEveryKindIsPinnedToItsExactBytes(t *testing.T) {
	values := attachmentGoldenValues()
	for _, code := range definedAttachmentCodes() {
		kind := ServerAttachmentKind(code)
		want, pinned := attachmentGoldenVectors[kind]
		if !pinned {
			continue
		}
		name := specAttachmentKindNames[kind]
		attachment := values[kind]
		if attachment == nil {
			t.Fatalf("%s has a vector and no value to build it from", name)
		}
		got, err := encodeAtItsDoor(attachment)
		if err != nil {
			t.Fatalf("%s: the door that serves it refused a valid attachment: %v", name, err)
		}
		if hex.EncodeToString(got) != want {
			t.Fatalf("the %s vector encodes to\n%s\nwant\n%s", name, hex.EncodeToString(got), want)
		}
		// the declared body length is the one number in a vector a typo cannot be seen in
		// by eye, so it is read back out of the octets and checked against the ones that
		// follow it rather than trusted
		declared, err := syntax.NewReader(got[2:attachmentFramingBytes]).ReadUint32()
		if err != nil {
			t.Fatalf("the %s vector carries no body length: %v", name, err)
		}
		if len(got) != attachmentFramingBytes+int(declared) {
			t.Errorf("the %s vector is %d octets and declares a body of %d", name, len(got), declared)
		}
		parsed, err := parseAtItsDoor(kind, got)
		if err != nil {
			t.Fatalf("the %s vector does not parse: %v", name, err)
		}
		if difference := attachmentDifference(attachment, parsed); difference != "" {
			t.Errorf("the %s vector does not round trip: %s differs", name, difference)
		}
	}
}

// Every kind that carries a body has a golden vector.
//
// The set is derived from the encoder rather than counted against four, so a fifth kind
// added to the package fails here instead of being pinned by nothing while every other
// property in this file goes on holding over the four that were already there.
func TestEveryKindThatCarriesABodyHasAGoldenVector(t *testing.T) {
	pinned := []int{}
	for kind := range attachmentGoldenVectors {
		pinned = append(pinned, int(kind))
	}
	slices.Sort(pinned)
	want := []int{}
	for _, code := range encodableKindCodes(t) {
		if ServerAttachmentKind(code) != AttachmentNone {
			want = append(want, code)
		}
	}
	// the kinds section 5.1 check 3's door does not serve carry bodies too, and a vector is
	// owed for each of them for the same reason: the door they ARE served at is the one that
	// writes them, and a kind pinned by nothing is a kind every other property in this file
	// goes on holding around. This set is EMPTY as of 2026-09-23 and the append is kept
	// rather than deleted, because it is what pins a seventh kind that is ruled before its
	// door widens — the situation 0x0005 was in for the whole of steps 1 to 3.
	want = append(want, unservedAttachmentCodes(t, serverAttachmentDoorName)...)
	slices.Sort(want)
	want = slices.Compact(want)
	if !slices.Equal(pinned, want) {
		t.Errorf("the vectors pin %v and the doors write bodies for %v", pinned, want)
	}
	// THE STATEMENT THAT DOES NOT GO VACUOUS WHEN THE APPEND ABOVE IS EMPTY: every kind this
	// package defines except the absent one carries a body and is pinned. Derived from the
	// kind table rather than from the encoder, so "the encoder stopped writing one" and "a
	// vector went missing" are both failures here instead of cancelling out.
	owed := []int{}
	for _, code := range definedAttachmentCodes() {
		if ServerAttachmentKind(code) != AttachmentNone {
			owed = append(owed, code)
		}
	}
	if !slices.Equal(pinned, owed) {
		t.Errorf("the vectors pin %v and this package defines %v kinds that carry a body", pinned, owed)
	}
}

// The epoch attachment vector is the attachment aad_test.go already pins.
//
// This is the one assertion in the file that reaches outside the package's own agreement
// with itself. aad_test.go's commit vector carries these same 136 octets and its comment
// records that the digest over them was derived by a separate program importing nothing from
// this package, so the layout below this line was fixed before there was an encoder to agree
// with. If the two ever differ, the record's LP(H(server_attachment)) is computed over one
// attachment and this encoder writes another, and every commit fails at the mac.
func TestTheEpochVectorIsTheOneAadTestPinsIndependently(t *testing.T) {
	if attachmentEpochVectorHex != aadKatCommitAttachmentHex {
		t.Fatalf("this file's epoch vector is\n%s\nand aad_test.go's is\n%s", attachmentEpochVectorHex, aadKatCommitAttachmentHex)
	}
	attachment := attachmentGoldenValues()[AttachmentEpoch]
	got := mustEncodeAttachment(t, "the epoch vector", attachment)
	if !bytes.Equal(got, aadKatCommitAttachment(t)) {
		t.Fatalf("the encoder produced\n%s\nand aad_test.go's attachment is\n%s",
			hex.EncodeToString(got), hex.EncodeToString(aadKatCommitAttachment(t)))
	}
	// and the length aad_test.go asserts on for its own reasons, restated here so a vector
	// that changed length would fail in both files rather than in one
	if want, got := 136, len(got); got != want {
		t.Errorf("the epoch attachment is %d octets and section 5.11's block is %d", got, want)
	}
}

// ── the layout, and the round trip ──────────────────────────────────────────────────

// Every corpus attachment encodes to the layout stated independently in rawAttachment.
func TestEncodedBytesAreTheLayoutSectionFiveElevenStates(t *testing.T) {
	for _, entry := range attachmentCorpus(t) {
		got := mustEncodeAttachment(t, entry.name, entry.attachment)
		if entry.attachment.Kind == AttachmentNone {
			if len(got) != 0 {
				t.Fatalf("%s: the absent attachment encoded to %d octets", entry.name, len(got))
			}
			continue
		}
		want := rawAttachmentOf(t, entry.attachment).encode(t)
		if !bytes.Equal(got, want) {
			t.Fatalf("%s: the encoder wrote\n%s\nand the layout is\n%s", entry.name, hex.EncodeToString(got), hex.EncodeToString(want))
		}
	}
}

// Byte exact both ways over the whole corpus: the attachment encodes, the encoding parses
// back to the same attachment, and re-encoding that attachment reproduces the same bytes.
// The second half is what catches an encoder and a decoder that disagree about a field —
// there the value survives the first hop and the bytes do not survive the second.
func TestEveryCorpusAttachmentRoundTripsByteExact(t *testing.T) {
	entries := attachmentCorpus(t)
	for _, entry := range entries {
		first := mustEncodeAttachment(t, entry.name, entry.attachment)
		parsed, err := ParseServerAttachment(first)
		if err != nil {
			t.Fatalf("%s: an attachment this package encoded does not parse: %v", entry.name, err)
		}
		if difference := attachmentDifference(entry.attachment, parsed); difference != "" {
			t.Fatalf("%s: the parsed attachment differs from the encoded one: %s", entry.name, difference)
		}
		second := mustEncodeAttachment(t, entry.name, parsed)
		if !bytes.Equal(first, second) {
			t.Fatalf("%s: re-encoding the parsed attachment produced %d octets, want the same %d", entry.name, len(second), len(first))
		}
	}
	t.Logf("%d corpus attachments round tripped", len(entries))
}

// ── the absent attachment ───────────────────────────────────────────────────────────

// A nil attachment and an AttachmentNone attachment are the same attachment, and both are no
// bytes at all.
//
// This is spec A section 5.11's test obligation asserted from the encoder's side. aad.go
// already pins the consequence — an ordinary record contributes LP(SHA-256("")) to aad_head —
// and this pins the cause, on the hash rather than only on the bytes, because the hash is
// what reaches the mac and a difference the bytes hid would surface only there.
func TestTheAbsentAttachmentAndAttachmentNoneEncodeIdentically(t *testing.T) {
	fromNil, err := EncodeServerAttachment(nil)
	if err != nil {
		t.Fatalf("a nil attachment does not encode: %v", err)
	}
	fromNone, err := EncodeServerAttachment(&ServerAttachment{Kind: AttachmentNone})
	if err != nil {
		t.Fatalf("an AttachmentNone attachment does not encode: %v", err)
	}
	if len(fromNil) != 0 {
		t.Errorf("a nil attachment encoded to %s, want no octets at all", hex.EncodeToString(fromNil))
	}
	if !bytes.Equal(fromNil, fromNone) {
		t.Fatalf("a nil attachment gives %s and an AttachmentNone one gives %s",
			hex.EncodeToString(fromNil), hex.EncodeToString(fromNone))
	}
	// the property that actually reaches the mac, so a future encoding that made the two
	// differ in length alone still fails here
	if sha256.Sum256(fromNil) != sha256.Sum256(fromNone) {
		t.Error("H(server_attachment) differs between a nil attachment and an AttachmentNone one")
	}
	if want := sha256.Sum256(nil); sha256.Sum256(fromNone) != want {
		t.Errorf("H of the absent attachment is %x, want the SHA-256 of the empty string %x", sha256.Sum256(fromNone), want)
	}
}

// The same equivalence carried the one hop that matters: through aad_head, on the encoder's
// own bytes.
//
// The test above pins the cause and aad_test.go's vector pins the consequence, but the two
// have never been joined — aad_test.go hands AADHead an attachment it wrote by hand, and
// nothing in this package yet routes EncodeServerAttachment's answer into a RecordHeader.
// The record builder that will is the place this property actually breaks, and until it
// exists this is the join: the encoder's answer for an absent attachment and for an explicit
// AttachmentNone, each carried into the header field the mac is taken over, must land on the
// identical preimage — and on the identical preimage a header holding no attachment at all
// gives, which is the vector aad_test.go pins to its exact bytes.
//
// It observes what the byte comparison above cannot: a change that made either spelling
// contribute bytes of its own reaches the aead through H(server_attachment), and a record
// built by a client with one spelling is then a record the server hashes differently and
// refuses as a bad mac — the intermittent failure spec B section 12.1 A-1 exists to prevent.
func TestBothSpellingsOfTheAbsentAttachmentGiveTheSameAadHead(t *testing.T) {
	// LP(H(server_attachment)) for the absent attachment, from spec A section 5.11 and
	// master section 0's notation line rather than from this package: the 32 bit prefix
	// 00000020 and the SHA-256 of the empty string, which is a value any second
	// implementation holds without running any of this.
	const wantTail = "00000020" + "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	spellings := []struct {
		name       string
		attachment *ServerAttachment
	}{
		{name: "an absent attachment", attachment: nil},
		{name: "an explicit AttachmentNone", attachment: &ServerAttachment{Kind: AttachmentNone}},
	}
	header := aadKatOrdinaryHeader()
	if header.ServerAttachment != nil {
		t.Fatal("the ordinary header already carries an attachment, so it pins no absence")
	}
	want := mustAADHead(t, "a header carrying no attachment", aadKatAlgId, &header, nil)
	if tail := hex.EncodeToString(want); !strings.HasSuffix(tail, wantTail) {
		t.Fatalf("aad_head ends %s, want LP(SHA-256(\"\")) %s", tail[len(tail)-len(wantTail):], wantTail)
	}
	for _, spelling := range spellings {
		encoded, err := EncodeServerAttachment(spelling.attachment)
		if err != nil {
			t.Fatalf("%s does not encode: %v", spelling.name, err)
		}
		// the header field and the argument both, because AADHead compares them and a
		// spelling that answered bytes would otherwise be refused rather than observed
		carried := aadKatOrdinaryHeader()
		carried.ServerAttachment = encoded
		got := mustAADHead(t, spelling.name, aadKatAlgId, &carried, encoded)
		if !bytes.Equal(got, want) {
			t.Errorf("%s encoded to %s and gives aad_head\n%s\nwant\n%s",
				spelling.name, hex.EncodeToString(encoded), hex.EncodeToString(got), hex.EncodeToString(want))
		}
	}
}

// Empty input parses back as the absent attachment with no body, which is what every ordinary
// record carries.
func TestEmptyInputParsesAsTheAbsentAttachment(t *testing.T) {
	for _, input := range [][]byte{nil, {}} {
		parsed, err := ParseServerAttachment(input)
		if err != nil {
			t.Fatalf("a %d octet input does not parse: %v", len(input), err)
		}
		if parsed.Kind != AttachmentNone {
			t.Errorf("a %d octet input parsed as kind 0x%04x, want AttachmentNone", len(input), uint16(parsed.Kind))
		}
		if carried, set := parsed.bodyKind(); set != 0 {
			t.Errorf("the absent attachment came back carrying %d bodies, the last of them kind 0x%04x", set, uint16(carried))
		}
	}
}

// The absent attachment spelled out as kind 0x0000 with an empty body is refused.
//
// Both specs say an ordinary record carries a zero length server_attachment and NOT kind
// 0x0000, and section 5.11's test obligation says why: the two must encode identically or
// H(server_attachment) differs between client and server. Accepting the long form gives one
// attachment two encodings with two different hashes, and the write_auth mac and both aeads
// are over exactly one of them. It is also the only reading under which parsing either fails
// or re-encodes to the identical bytes.
func TestTheAbsentAttachmentSpelledOutIsRefused(t *testing.T) {
	spelled := rawAttachmentOf(t, &ServerAttachment{Kind: AttachmentNone}).encode(t)
	if want := attachmentFramingBytes; len(spelled) != want {
		t.Fatalf("the spelled out absent attachment is %d octets, want %d", len(spelled), want)
	}
	_, err := ParseServerAttachment(spelled)
	if err == nil {
		t.Fatalf("%s was accepted, and an attachment with two encodings has a mac over one of them", hex.EncodeToString(spelled))
	}
	if !errors.Is(err, ErrServerAttachmentNoneEncoded) {
		t.Errorf("refused with %v, want ErrServerAttachmentNoneEncoded", err)
	}
	// and with a body, which is the same mistake with more of it
	withBody := rawAttachment{kind: uint16(AttachmentNone), body: []byte{0x00}}.encode(t)
	if _, err := ParseServerAttachment(withBody); err == nil {
		t.Errorf("%s was accepted", hex.EncodeToString(withBody))
	}
}

// ── nothing is silently accepted and changed ────────────────────────────────────────

// Every prefix of a valid encoding is refused. A parser that stopped early on any of them
// would accept a truncated attachment as a whole one, which is an attachment whose
// H(server_attachment) the mac was computed over bytes the reader never saw.
func TestEverySingleByteTruncationOfAValidAttachmentIsRejected(t *testing.T) {
	walked := 0
	for _, entry := range attachmentCorpus(t) {
		valid := mustEncodeAttachment(t, entry.name, entry.attachment)
		for length := 1; length < len(valid); length++ {
			what := fmt.Sprintf("%s truncated to %d of %d octets", entry.name, length, len(valid))
			if _, err := ParseServerAttachment(valid[:length]); err == nil {
				t.Fatalf("%s: accepted", what)
			}
			walked++
		}
	}
	if walked == 0 {
		t.Fatal("no truncation was walked, so this property holds vacuously")
	}
	t.Logf("%d truncations refused", walked)
}

// An octet after the attachment is a refusal. Without it an attachment has more than one
// encoding, and the write_auth mac is over exactly one of them.
func TestATrailingByteIsRejectedOnEveryKind(t *testing.T) {
	for _, entry := range attachmentWalkCorpus(t) {
		valid := mustEncodeAttachment(t, entry.name, entry.attachment)
		for value := 0; value <= 0xFF; value++ {
			extended := append(slices.Clone(valid), byte(value))
			if _, err := ParseServerAttachment(extended); err == nil {
				t.Fatalf("%s with a trailing 0x%02x: accepted, and an attachment with two encodings has a mac over one of them", entry.name, value)
			}
		}
	}
}

// Every single octet corruption of a valid encoding either is refused or re-encodes to
// exactly the corrupted bytes.
//
// This is the property that catches a field read at the wrong width, and it is the one thing
// a round trip over well formed attachments cannot see: read expected_wrap_count as a u16 and
// every attachment this package writes still round trips, because the two octets it ignores
// are two octets it also never wrote. Corrupt one of them and the attachment parses,
// re-encodes to different bytes, and is caught here.
func TestEverySingleByteCorruptionIsRejectedOrRoundTrips(t *testing.T) {
	walked := 0
	for _, entry := range attachmentWalkCorpus(t) {
		valid := mustEncodeAttachment(t, entry.name, entry.attachment)
		corrupted := slices.Clone(valid)
		for offset := range valid {
			original := corrupted[offset]
			for value := 0; value <= 0xFF; value++ {
				if byte(value) == original {
					continue
				}
				corrupted[offset] = byte(value)
				what := fmt.Sprintf("%s with octet %d of %d set to 0x%02x", entry.name, offset, len(valid), value)
				parsed, err := ParseServerAttachment(corrupted)
				walked++
				if err != nil {
					continue
				}
				again, err := EncodeServerAttachment(parsed)
				if err != nil {
					t.Fatalf("%s: parsed and then refused to re-encode: %v", what, err)
				}
				if !bytes.Equal(again, corrupted) {
					t.Fatalf("%s: parsed and re-encoded to different bytes, so an octet was accepted and silently changed", what)
				}
			}
			corrupted[offset] = original
		}
	}
	if walked == 0 {
		t.Fatal("no corruption was walked, so this property holds vacuously")
	}
	t.Logf("%d single octet corruptions walked", walked)
}

// ── what check 3 relies on being refused ────────────────────────────────────────────

// Every length prefixed field is its exact width and no other, on both sides of the codec.
//
// The class of fields is derived from the go types rather than listed — see
// attachmentWidthFields — so the four widths spec B section 5.1 check 3 names by hand
// (write_key 32, read_key 32, a 32 octet Ed25519 pub on RecoveryTag, a 16 octet target on
// WrapTag) are covered along with the two it does not, and a seventh field added later is
// covered without an edit here.
func TestEveryLengthPrefixedFieldIsItsExactWidthAndNoOther(t *testing.T) {
	fields := attachmentWidthFields(t)
	t.Logf("%d length prefixed fields under the walk: %v", len(fields), fields)
	byKind := validAttachmentsByKind(t)
	for _, field := range fields {
		for length := 0; length <= field.width*2+8; length++ {
			if length == field.width {
				continue
			}
			attachment := byKind[field.kind]
			fresh := validAttachmentsByKind(t)[field.kind]
			body := attachmentBodyValue(fresh)
			body.FieldByName(field.name).Set(reflect.ValueOf(fillBytes(0xFE, length)))
			what := fmt.Sprintf("%s.%s at %d octets, want %d", specAttachmentKindNames[field.kind], field.name, length, field.width)
			if _, err := encodeAtItsDoor(fresh); err == nil {
				t.Fatalf("%s: the encoder accepted it", what)
			} else if !errors.Is(err, ErrServerAttachmentFieldLength) {
				t.Errorf("%s: the encoder refused with %v, want ErrServerAttachmentFieldLength", what, err)
			}
			bs := rawAttachmentOf(t, fresh).encode(t)
			if _, err := parseAtItsDoor(field.kind, bs); err == nil {
				t.Fatalf("%s: the parser accepted it", what)
			} else if !errors.Is(err, ErrServerAttachmentFieldLength) {
				t.Errorf("%s: the parser refused with %v, want ErrServerAttachmentFieldLength", what, err)
			}
			// and the width itself still passes, so the walk cannot be satisfied by a
			// package that refuses every length
			if _, err := encodeAtItsDoor(attachment); err != nil {
				t.Fatalf("%s: the valid attachment stopped encoding: %v", what, err)
			}
		}
	}
}

// Every algorithm identifier but the one its kind names is refused, on both sides.
//
// Derived over all 65536 values rather than over a handful somebody chose, so a check that
// admitted a neighbouring identifier — 0x0030, or the whole of master section 7.1's registry
// — is caught. It is per kind because the identifiers are per kind: an EpochAttachment
// announcing Ed25519 claims its two 32 octet keys came out of a signature algorithm.
func TestEveryAlgorithmIdentifierButTheKindsOwnIsRefused(t *testing.T) {
	byKind := validAttachmentsByKind(t)
	tried := 0
	for kind, want := range attachmentAlgIds {
		attachment := byKind[kind]
		if attachment == nil {
			t.Fatalf("kind 0x%04x names an algorithm identifier and has no valid attachment", uint16(kind))
		}
		body := attachmentBodyValue(attachment)
		field := body.FieldByName("AlgId")
		if !field.IsValid() {
			t.Fatalf("kind 0x%04x names an algorithm identifier and its body has no AlgId field", uint16(kind))
		}
		for value := 0; value <= 0xFFFF; value++ {
			field.Set(reflect.ValueOf(uint16(value)))
			_, encodeErr := encodeAtItsDoor(attachment)
			_, parseErr := parseAtItsDoor(kind, rawAttachmentOf(t, attachment).encode(t))
			if uint16(value) == want {
				if encodeErr != nil || parseErr != nil {
					t.Fatalf("kind 0x%04x refused its own identifier 0x%04x: %v / %v", uint16(kind), value, encodeErr, parseErr)
				}
				continue
			}
			if encodeErr == nil || parseErr == nil {
				t.Fatalf("kind 0x%04x accepted algorithm identifier 0x%04x, and it names 0x%04x", uint16(kind), value, want)
			}
			if !errors.Is(encodeErr, ErrServerAttachmentAlgId) || !errors.Is(parseErr, ErrServerAttachmentAlgId) {
				t.Fatalf("kind 0x%04x refused 0x%04x with %v / %v, want ErrServerAttachmentAlgId", uint16(kind), value, encodeErr, parseErr)
			}
		}
		field.Set(reflect.ValueOf(want))
		tried++
	}
	if tried == 0 {
		t.Fatal("no kind names an algorithm identifier, so this walk asked nothing")
	}
	t.Logf("%d kinds walked across all 65536 identifiers each", tried)
}

// An epoch attachment that expects no wraps is refused, on both sides.
//
// Spec B section 5.1 check 3 names it outright. The epoch it opens has at least its own
// snapshot in the wrap set, so zero is not a small fan out: it names a count no EpochComplete
// marker can ever match, which leaves the group readable and not writable with nothing able
// to close it.
func TestAnEpochAttachmentExpectingNoWrapsIsRefused(t *testing.T) {
	attachment := validEpochAttachment(0, 42, 1, 1, 0)
	_, err := EncodeServerAttachment(attachment)
	if err == nil {
		t.Fatal("the encoder accepted an expected_wrap_count of zero")
	}
	if !errors.Is(err, ErrExpectedWrapCountZero) {
		t.Errorf("the encoder refused with %v, want ErrExpectedWrapCountZero", err)
	}
	bs := rawAttachmentOf(t, attachment).encode(t)
	_, err = ParseServerAttachment(bs)
	if err == nil {
		t.Fatalf("the parser accepted %s, an expected_wrap_count of zero", hex.EncodeToString(bs))
	}
	if !errors.Is(err, ErrExpectedWrapCountZero) {
		t.Errorf("the parser refused with %v, want ErrExpectedWrapCountZero", err)
	}
	// every other value of the field is accepted, so the refusal is about zero and not about
	// the field
	for _, count := range u32BoundariesAboveZero() {
		if _, err := EncodeServerAttachment(validEpochAttachment(0, 42, 1, 1, count)); err != nil {
			t.Errorf("expected_wrap_count %d was refused: %v", count, err)
		}
	}
}

// A kind nothing defines is refused, on both sides, and never silently ignored.
//
// Check 3 is what stands between a record and the database. An attachment the server cannot
// parse is one it cannot check, so a record carrying an undefined kind that parsed to
// "nothing worth looking at" would take the epoch key install, the recovery index and the
// wrap index past every question check 3 asks of them.
func TestAnUnknownKindIsADecodeError(t *testing.T) {
	defined := map[int]bool{}
	for _, code := range definedAttachmentCodes() {
		defined[code] = true
	}
	body := rawEpochComplete{epoch: 42, wrapCount: 1501}.encode(t)
	refusals := 0
	for code := 0; code <= 0xFFFF; code++ {
		if defined[code] {
			continue
		}
		bs := rawAttachment{kind: uint16(code), body: body}.encode(t)
		parsed, err := ParseServerAttachment(bs)
		if err == nil {
			t.Fatalf("kind 0x%04x parsed to %+v, and an attachment this layer cannot parse is one the server cannot check", code, parsed)
		}
		if !errors.Is(err, ErrServerAttachmentKindUnknown) {
			t.Fatalf("kind 0x%04x refused with %v, want ErrServerAttachmentKindUnknown", code, err)
		}
		if _, err := EncodeServerAttachment(&ServerAttachment{Kind: ServerAttachmentKind(code)}); !errors.Is(err, ErrServerAttachmentKindUnknown) {
			t.Fatalf("the encoder refused kind 0x%04x with %v, want ErrServerAttachmentKindUnknown", code, err)
		}
		refusals++
	}
	if refusals != 0x10000-len(defined) {
		t.Fatalf("%d kinds were refused and %d are undefined", refusals, 0x10000-len(defined))
	}
	t.Logf("%d undefined kinds refused on both sides", refusals)

	// THE COMPLEMENT THIS LOOP REMOVED, named rather than left implicit. The walk above is
	// about codes NOTHING defines, and it skips six — so the reader is owed, in the same
	// test, what those six do instead of being refused here.
	//
	// THIS BLOCK USED TO BE THE SIXTH KIND ALONE, walked over ruledAttachmentCodes and
	// asserting it was refused at section 5.11's door. That set is a PROVENANCE set: it is
	// still exactly [5] after 2026-09-23, and the loop over it would have gone on running,
	// asserting a refusal that had just become an acceptance — which is the one shape a
	// complement paragraph must not have. It is now written per door, over the two answers
	// a defined kind can get, and it is the DIFFERENCE BETWEEN THE TWO SENTINELS that is
	// under assertion: "nobody defines this" and "this door does not serve this" are two
	// facts, and a caller that could not tell them apart would read a conforming
	// implementation's commit as a corrupt one.
	served, notServed := 0, 0
	byKind := validAttachmentsByKind(t)
	for _, door := range attachmentDoorNames() {
		for _, code := range definedAttachmentCodes() {
			kind := ServerAttachmentKind(code)
			if kind == AttachmentNone {
				continue
			}
			bs, err := encodeAtItsDoor(byKind[kind])
			if err != nil {
				t.Fatalf("kind 0x%04x does not encode at its own door: %v", code, err)
			}
			parsed, parseErr := parseAtDoor(t, door, bs)
			if slices.Contains(servedAttachmentCodes(t, door), code) {
				if parseErr != nil {
					t.Errorf("%s is written down as serving kind 0x%04x and refused it with %v", door, code, parseErr)
					continue
				}
				// an acceptance is only the other sentinel's counterpart if what came back
				// is the attachment that went in; a door that answered a zeroed body would
				// otherwise count here as the positive half of the distinction
				if difference := attachmentDifference(byKind[kind], parsed); difference != "" {
					t.Errorf("%s accepted kind 0x%04x and handed back an attachment whose %s differs", door, code, difference)
					continue
				}
				served++
				continue
			}
			// the refusal is the OTHER sentinel, never the undefined one the walk above uses
			if !errors.Is(parseErr, ErrServerAttachmentKindNotServed) {
				t.Errorf("%s refused kind 0x%04x with %v, want ErrServerAttachmentKindNotServed", door, code, parseErr)
				continue
			}
			if errors.Is(parseErr, ErrServerAttachmentKindUnknown) {
				t.Errorf("%s refused defined kind 0x%04x as an UNDEFINED one, and the two sentinels are the distinction", door, code)
			}
			notServed++
		}
	}
	if served == 0 || notServed == 0 {
		t.Fatalf("the defined kinds produced %d acceptances and %d not-served refusals across the doors, and the "+
			"distinction between the two sentinels needs one of each", served, notServed)
	}
	t.Logf("across the doors, the %d defined kinds gave %d acceptances and %d ErrServerAttachmentKindNotServed refusals, "+
		"against %d ErrServerAttachmentKindUnknown", len(defined), served, notServed, refusals)
}

// A kind and a body that disagree are refused rather than resolved.
//
// One value carried in two places is a second thing to get wrong, and the plausible mistake
// is not a malicious one: a caller that sets Kind and forgets to set the body, or that swaps
// a body in and leaves the old Kind. Resolving it — encoding whichever half the encoder
// preferred — would put a record on the wire whose author believed it said something else,
// and the server would act on the half this package chose.
//
// This is the one body the tag does not name. The other half of the presence rule — more
// bodies than one, whatever the tag — fails in a direction this loop cannot reach and is
// asserted below over a cross product of its own.
func TestAKindAndABodyThatDisagreeAreRefused(t *testing.T) {
	byKind := validAttachmentsByKind(t)
	for _, code := range definedAttachmentCodes() {
		kind := ServerAttachmentKind(code)
		for _, otherCode := range definedAttachmentCodes() {
			other := ServerAttachmentKind(otherCode)
			if other == kind {
				continue
			}
			mislabelled := *byKind[kind]
			mislabelled.Kind = other
			what := fmt.Sprintf("the %s body under kind %s", specAttachmentKindNames[kind], specAttachmentKindNames[other])
			_, err := EncodeServerAttachment(&mislabelled)
			if err == nil {
				t.Fatalf("%s: accepted", what)
			}
			if !errors.Is(err, ErrServerAttachmentBody) {
				t.Errorf("%s: refused with %v, want ErrServerAttachmentBody", what, err)
			}
		}
	}
}

// One body pointer of ServerAttachment: where it sits in the struct, the name a failure
// reports it by, and the value the package's own valid attachment of that kind carries.
type attachmentBodyField struct {
	index int
	name  string
	value reflect.Value
}

// Every body ServerAttachment can carry, read off the struct rather than listed.
//
// The class the presence rule is about is "more than one body set", and this project has
// been walked past a hand written membership list twelve times: a list of the four body
// fields, or of the pairs of them, is a list that understates the class the day a fifth
// pointer is declared. So the fields are found by walking the type for its pointers — Kind
// is the only field that is not one — and each is paired with the value it holds in the
// valid attachment that sets it, which is how the walk learns what a well formed body of
// that field looks like without a table anyone has to keep in step.
//
// Both directions are asserted rather than assumed. A pointer no valid attachment sets is a
// body every cross product below would skip in silence, and a pointer two of them set is one
// the walk cannot attribute, so each is a fatal here rather than a gap there.
func attachmentBodyFields(t testing.TB) []attachmentBodyField {
	t.Helper()
	byKind := validAttachmentsByKind(t)
	fields := []attachmentBodyField{}
	structType := reflect.TypeOf(ServerAttachment{})
	for i := range structType.NumField() {
		declared := structType.Field(i)
		if declared.Type.Kind() != reflect.Pointer {
			continue
		}
		var value reflect.Value
		for _, code := range definedAttachmentCodes() {
			field := reflect.ValueOf(*byKind[ServerAttachmentKind(code)]).Field(i)
			if field.IsNil() {
				continue
			}
			if value.IsValid() {
				t.Fatalf("two valid attachments set %s, so the walk cannot say which kind's body it is", declared.Name)
			}
			value = field
		}
		if !value.IsValid() {
			t.Fatalf("no valid attachment sets %s, so no cross product below ever puts a body in it", declared.Name)
		}
		fields = append(fields, attachmentBodyField{index: i, name: declared.Name, value: value})
	}
	if len(fields) < 2 {
		t.Fatalf("ServerAttachment carries %d body pointers, so no attachment can carry two and the walk below holds vacuously", len(fields))
	}
	return fields
}

// An attachment carrying more than one body is refused, whichever bodies they are and
// whatever kind it is labelled — and nothing comes out of the encoder when it is.
//
// This is the other half of the rule above and it fails in a direction the mislabelling half
// cannot see. bodyKind reports the LAST body it finds, in the order the struct declares them,
// so `carried != a.Kind` already refuses every arrangement whose EARLIER body is the one Kind
// names. The arrangements it does not refuse are the ones whose later body matches — a
// WrapTag and an EpochComplete under kind EpochComplete, say — and for those the presence
// count is the only thing standing between the caller and an encoding. Without it the encoder
// writes the one body the switch reaches and drops the other on the floor, which is verbatim
// the failure the ServerAttachment doc comment says is refused: an attachment carrying an
// EpochAttachment under the WrapTag tag encoded as a wrap tag with the epoch attachment
// quietly dropped. The record's H(server_attachment) then covers an attachment its author did
// not write, and the server indexes a wrap record as a marker with nothing anywhere refusing
// it.
//
// The space is every subset of the bodies with at least two in it, crossed with every kind
// the alphabet defines, both computed rather than written down. A pair picked by hand covers
// one of twelve orderings and — since six of the twelve are refused by the mislabelling half
// anyway — has a one in two chance of observing nothing at all.
func TestEveryAttachmentCarryingMoreThanOneBodyIsRefusedUnderEveryKind(t *testing.T) {
	fields := attachmentBodyFields(t)
	codes := specAttachmentCodes()
	refused := 0
	for subset := 1; subset < 1<<len(fields); subset++ {
		for _, code := range codes {
			attachment := &ServerAttachment{Kind: ServerAttachmentKind(code)}
			bodies := reflect.ValueOf(attachment).Elem()
			names := []string{}
			for i, field := range fields {
				if subset&(1<<i) == 0 {
					continue
				}
				bodies.Field(field.index).Set(field.value)
				names = append(names, field.name)
			}
			if len(names) < 2 {
				continue
			}
			what := fmt.Sprintf("the %s bodies at once under kind %s",
				strings.Join(names, " and "), specAttachmentKindNames[ServerAttachmentKind(code)])
			bs, err := EncodeServerAttachment(attachment)
			if err == nil {
				t.Fatalf("%s: accepted, and it encoded to %s — every body but one is dropped from the wire",
					what, hex.EncodeToString(bs))
			}
			if !errors.Is(err, ErrServerAttachmentBody) {
				t.Errorf("%s: refused with %v, want ErrServerAttachmentBody", what, err)
			}
			// a refusal that still answered bytes is a refusal a caller ignoring the error
			// puts on the wire, which is the same dropped body by another route
			if len(bs) != 0 {
				t.Errorf("%s: refused and still answered %s", what, hex.EncodeToString(bs))
			}
			refused++
		}
	}
	// the subsets of two or more, crossed with the alphabet, counted from the two derived
	// sets rather than from a number typed here
	if want := (1<<len(fields) - 1 - len(fields)) * len(codes); refused != want {
		t.Fatalf("%d multi body attachments were offered and the cross product has %d in it", refused, want)
	}
	t.Logf("%d multi body attachments refused across %d bodies and %d kinds", refused, len(fields), len(codes))
}

// ── what check 3 relies on NOT being refused ────────────────────────────────────────

// Both durable_ttl_seconds sentinels are legal, and so is every other value of both retention
// fields.
//
// This is the half a hand written range check breaks silently, and it is the reason there is
// no comparison against either sentinel anywhere in attachment.go. Spec B section 5.1 check 3
// says both are "legal values here and are resolved at section 6.1 step (6), never refused",
// and section 7.3 case 3 forbids refusing either in all cases. They mean different things — 0
// is "the group set nothing" and 4294967295 is "the group asked for indefinite" — and both
// are resolved against the server's own advertised policy, which is arithmetic this layer
// cannot do and must not pre-empt. A refusal here refuses a commit, and a refused commit is a
// group that cannot rekey.
func TestBothDurableTtlSentinelsAndEveryOtherRetentionValueAreLegal(t *testing.T) {
	sentinels := []uint32{0, 4294967295}
	for _, sentinel := range sentinels {
		attachment := validEpochAttachment(0, 42, 1, sentinel, 1)
		bs, err := EncodeServerAttachment(attachment)
		if err != nil {
			t.Fatalf("durable_ttl_seconds %d was refused by the encoder: %v", sentinel, err)
		}
		parsed, err := ParseServerAttachment(bs)
		if err != nil {
			t.Fatalf("durable_ttl_seconds %d was refused by the parser: %v", sentinel, err)
		}
		if parsed.Epoch.DurableTtlSeconds != sentinel {
			t.Errorf("durable_ttl_seconds %d came back as %d", sentinel, parsed.Epoch.DurableTtlSeconds)
		}
	}
	// and the whole range, on both retention fields, so the property is about the fields and
	// not about the two values
	for _, media := range u32Boundaries() {
		for _, durable := range u32Boundaries() {
			attachment := validEpochAttachment(0, 42, media, durable, 1)
			bs, err := EncodeServerAttachment(attachment)
			if err != nil {
				t.Fatalf("media %d durable %d was refused by the encoder: %v", media, durable, err)
			}
			parsed, err := ParseServerAttachment(bs)
			if err != nil {
				t.Fatalf("media %d durable %d was refused by the parser: %v", media, durable, err)
			}
			if parsed.Epoch.MediaTtlSeconds != media || parsed.Epoch.DurableTtlSeconds != durable {
				t.Errorf("media %d durable %d came back as %d and %d", media, durable, parsed.Epoch.MediaTtlSeconds, parsed.Epoch.DurableTtlSeconds)
			}
		}
	}
}

// The marker's wrap_count carries no bound of its own, zero included.
//
// Its one rule is equality against the epoch's expected_wrap_count, and that attachment is
// something this layer is never handed: spec B section 5.1 check 3 phrases it as "EpochComplete
// with a matching wrap_count", and matching is a question about state the server holds. A bound
// invented here would be this package guessing at the answer, and a wrong guess refuses a
// marker the server would have accepted, which leaves the group readable and not writable.
func TestTheMarkersWrapCountCarriesNoBoundOfItsOwn(t *testing.T) {
	for _, count := range u32Boundaries() {
		attachment := validEpochComplete(42, count)
		bs, err := EncodeServerAttachment(attachment)
		if err != nil {
			t.Fatalf("wrap_count %d was refused by the encoder: %v", count, err)
		}
		parsed, err := ParseServerAttachment(bs)
		if err != nil {
			t.Fatalf("wrap_count %d was refused by the parser: %v", count, err)
		}
		if parsed.Complete.WrapCount != count {
			t.Errorf("wrap_count %d came back as %d", count, parsed.Complete.WrapCount)
		}
	}
}

// ── the two halves admit the same attachments ───────────────────────────────────────

// The set of attachments EncodeServerAttachment will write and the set ParseServerAttachment
// will read are the same set.
//
// Asserted over a space computed from the package's own alphabet crossed with the edges of
// every rule the checks have: a length either side of every exact width, an algorithm
// identifier either side of the one its kind names, an expected_wrap_count of zero and one, a
// kind past the top of the alphabet. Both halves run the one checkServerAttachment, so what
// this really observes is that neither entry point has grown a check of its own — which is
// the edit that makes the server refuse a record the client considers valid.
func TestTheEncoderAndTheParserAdmitTheSameAttachments(t *testing.T) {
	byKind := validAttachmentsByKind(t)
	candidates := []struct {
		name       string
		attachment *ServerAttachment
	}{}
	add := func(name string, a *ServerAttachment) {
		candidates = append(candidates, struct {
			name       string
			attachment *ServerAttachment
		}{name: name, attachment: a})
	}
	// the kinds THIS DOOR serves, and not section 5.11's five: on 2026-09-23 the two stopped
	// being the same set, and the five would have left the agreement unasserted over exactly
	// the kind the door had just been widened to
	for _, code := range servedAttachmentCodes(t, serverAttachmentDoorName) {
		kind := ServerAttachmentKind(code)
		add(specAttachmentKindNames[kind], byKind[kind])
	}
	for _, field := range attachmentWidthFields(t) {
		for _, length := range []int{field.width - 1, field.width, field.width + 1} {
			if length < 0 {
				continue
			}
			fresh := validAttachmentsByKind(t)[field.kind]
			attachmentBodyValue(fresh).FieldByName(field.name).Set(reflect.ValueOf(fillBytes(0xFD, length)))
			add(fmt.Sprintf("%s.%s at %d", specAttachmentKindNames[field.kind], field.name, length), fresh)
		}
	}
	for kind, want := range attachmentAlgIds {
		for _, value := range []uint16{want - 1, want, want + 1} {
			fresh := validAttachmentsByKind(t)[kind]
			attachmentBodyValue(fresh).FieldByName("AlgId").Set(reflect.ValueOf(value))
			add(fmt.Sprintf("%s alg 0x%04x", specAttachmentKindNames[kind], value), fresh)
		}
	}
	for _, count := range []uint32{0, 1} {
		add(fmt.Sprintf("epoch wraps %d", count), validEpochAttachment(0, 42, 1, 1, count))
	}
	// past the top of every code this PACKAGE defines, not of section 5.11's five: with
	// 0x0005 defined, the old reading named a kind the encoder refuses for a different
	// reason and this candidate would have stopped being about an undefined code at all
	defined := definedAttachmentCodes()
	top := defined[len(defined)-1]
	add("a kind past the top of the alphabet", &ServerAttachment{Kind: ServerAttachmentKind(top + 1)})

	encoded := 0
	refused := 0
	for _, candidate := range candidates {
		bs, encodeErr := EncodeServerAttachment(candidate.attachment)
		// the absent attachment has no raw form to reach the parser with — its encoding is
		// no octets at all — and its two sides are asserted by the absent/empty tests above
		if candidate.attachment.Kind == AttachmentNone && encodeErr == nil {
			encoded++
			continue
		}
		_, parseErr := ParseServerAttachment(rawAttachmentOf(t, candidate.attachment).encode(t))
		if (encodeErr == nil) != (parseErr == nil) {
			t.Errorf("%s: the encoder says %v and the parser says %v; the two halves disagree about whether this attachment exists",
				candidate.name, encodeErr, parseErr)
			continue
		}
		if encodeErr != nil {
			refused++
			continue
		}
		encoded++
		if _, err := ParseServerAttachment(bs); err != nil {
			t.Errorf("%s: the encoder wrote it and the parser refused it: %v", candidate.name, err)
		}
	}
	if encoded == 0 {
		t.Fatal("the space reached no attachment the encoder writes, so the agreement holds vacuously")
	}
	if refused == 0 {
		t.Fatal("the space reached no refusal, so the agreement says nothing about what either half refuses")
	}
	t.Logf("%d candidates, %d encoded and %d refused by both halves", len(candidates), encoded, refused)
}

// ── the sixth kind ──────────────────────────────────────────────────────────────────
//
// Ruling 27 of 2026-09-22 re-opened spec B section 5.4's RULED EpochAttachment block and
// replaced its two cleartext keys with one digest over both of them, under a new kind
// 0x0005. Ruling 31 says where that design is cheapest to find wrong: "the interop vectors
// are where it comes out clean or does not." So what is asserted below is not that the
// encoder agrees with itself. It is
//
//	(1) the preimage, octet by octet, against a derivation done outside this package;
//	(2) the digest over it, twice, at two epochs, so that leaving u64(opens_epoch) out
//	    reproduces neither number;
//	(3) that the kind 0x0005 vector IS the kind 0x0001 vector with its two keys hashed,
//	    computed from the 0x0001 vector's own octets rather than from a second copy of the
//	    values — which is the amendment stated as an equation between two pinned strings;
//	(4) the rollout property, which is what lets this land alone: a record carrying one of
//	    these encodes, parses back with is_commit set and the slot intact, while section
//	    5.11's door refuses the same octets by name.

// The corpus for the sixth kind: every axis its body has, computed rather than listed.
//
// The axes are the ones the block at the top of attachment.go gives it — the u64 boundaries
// on epoch, the u32 boundaries on both retention fields with both durable sentinels among
// them, the u32 boundaries above zero on expected_wrap_count, and three content rotations
// across the two length prefixed fields. It is its own corpus rather than a sixth arm of
// attachmentCorpus because that corpus is section 5.11's door's, and this kind is served at
// another one.
func epochDigestCorpus(t testing.TB) []attachmentCorpusEntry {
	t.Helper()
	entries := []attachmentCorpusEntry{}
	for rotation := range attachmentRotations {
		for _, epoch := range u64Boundaries() {
			for _, media := range u32Boundaries() {
				for _, durable := range u32Boundaries() {
					for _, count := range u32BoundariesAboveZero() {
						name := fmt.Sprintf("digest rot=%d epoch=%d media=%d durable=%d wraps=%d",
							rotation, epoch, media, durable, count)
						entries = append(entries, attachmentCorpusEntry{
							name:       name,
							attachment: validEpochDigestAttachment(t, rotation, epoch, media, durable, count),
						})
					}
				}
			}
		}
	}
	if len(entries) == 0 {
		t.Fatal("the epoch digest corpus is empty, so every property asserted over it would hold vacuously")
	}
	return entries
}

// One corpus entry per durable_ttl_seconds value, for the walks that try all 255
// alternatives at every offset. Derived by grouping the corpus, so both sentinels are in the
// subset by construction.
func epochDigestWalkCorpus(t testing.TB) []attachmentCorpusEntry {
	t.Helper()
	seen := map[uint32]bool{}
	subset := []attachmentCorpusEntry{}
	for _, entry := range epochDigestCorpus(t) {
		durable := entry.attachment.EpochDigest.DurableTtlSeconds
		if seen[durable] {
			continue
		}
		seen[durable] = true
		subset = append(subset, entry)
	}
	if len(subset) == 0 {
		t.Fatal("the epoch digest walk subset is empty, so every property asserted over it would hold vacuously")
	}
	return subset
}

// The preimage is the octets the block at the top of attachment.go states, at two epochs.
//
// The preimage is pinned as well as the digest because a digest alone moves as one opaque
// number whichever term went missing from under it: drop the label, drop the group, drop the
// epoch, put the group after the epoch, swap the two keys, and every one of those answers is
// thirty two plausible octets that this package goes on agreeing with itself about.
func TestTheEpochKeysPreimageIsPinnedToItsExactBytes(t *testing.T) {
	for _, pinned := range []struct {
		epoch    uint64
		preimage string
		digest   string
	}{
		{epoch: 42, preimage: attachmentEpochKeysPreimageHex, digest: attachmentEpochKeysDigestHex},
		{epoch: 1, preimage: attachmentEpochKeysPreimageEpochOneHex, digest: attachmentEpochKeysDigestEpochOneHex},
	} {
		got, err := epochKeysPreimage(attachmentVectorGroupId(), pinned.epoch, aadRamp(0x70, 32), aadRamp(0x90, 32))
		if err != nil {
			t.Fatalf("epoch %d: the preimage does not build: %v", pinned.epoch, err)
		}
		if hex.EncodeToString(got) != pinned.preimage {
			t.Fatalf("epoch %d: the preimage is\n%s\nwant\n%s", pinned.epoch, hex.EncodeToString(got), pinned.preimage)
		}
		// the label is twenty two octets of raw ascii with no length prefix in front of it,
		// so the preimage opens with the label's own bytes and LP(group_id) begins at 22
		if want := []byte(epochKeysLabel); !bytes.Equal(got[:len(want)], want) {
			t.Errorf("epoch %d: the preimage does not open with the label's own octets", pinned.epoch)
		}
		if want, got := 22+36+8+36+36, len(got); got != want {
			t.Errorf("epoch %d: the preimage is %d octets and the block adds to %d", pinned.epoch, got, want)
		}
		// and the group is the term AFTER the label and BEFORE the epoch, checked at the
		// offset rather than as a substring: a preimage carrying the group anywhere else is
		// a preimage that still contains these thirty six octets
		if want := mustHex("00000020" + attachmentEpochKeysGroupHex); !bytes.Equal(got[22:22+len(want)], want) {
			t.Errorf("epoch %d: LP(group_id) does not sit between the label and the epoch", pinned.epoch)
		}
		// and the digest is SHA-256 of exactly those octets, stated as the hash of the
		// pinned string rather than as the package's answer
		sum := sha256.Sum256(mustHex(pinned.preimage))
		if hex.EncodeToString(sum[:]) != pinned.digest {
			t.Fatalf("epoch %d: SHA-256 of the pinned preimage is %s and the pinned digest is %s",
				pinned.epoch, hex.EncodeToString(sum[:]), pinned.digest)
		}
		digest, err := EpochKeysDigest(attachmentVectorGroupId(), pinned.epoch, aadRamp(0x70, 32), aadRamp(0x90, 32))
		if err != nil {
			t.Fatalf("epoch %d: EpochKeysDigest refused two 32 octet keys: %v", pinned.epoch, err)
		}
		if hex.EncodeToString(digest) != pinned.digest {
			t.Fatalf("epoch %d: EpochKeysDigest answered %s, want %s", pinned.epoch, hex.EncodeToString(digest), pinned.digest)
		}
		if len(digest) != epochKeysDigestBytes {
			t.Errorf("epoch %d: the digest is %d octets, want %d", pinned.epoch, len(digest), epochKeysDigestBytes)
		}
	}
}

// Every input of the digest changes it: every octet of the group, the epoch, every octet of
// the write key, and every octet of the read key.
//
// The two vectors above are the interop half of this and they are two points. This is the
// property around them, and it is what catches the term that is present in the pinned case
// and ignored everywhere else — a preimage that wrote the epoch but read it from the wrong
// place, or that LP framed one key and raw wrote the other, agrees with the vectors at
// exactly one value and with nothing else.
//
// The group walk is ruling 34's half and it is the same shape as the other three: thirty two
// flips, each one a group that differs from the vector's in one octet, each one a digest
// nothing else in this walk collides with. A build that took the group term out entirely
// collapses all thirty two into the unaltered digest and fails at the first of them.
func TestEveryInputOfTheEpochKeysDigestChangesIt(t *testing.T) {
	writeKey := aadRamp(0x70, 32)
	readKey := aadRamp(0x90, 32)
	base, err := EpochKeysDigest(attachmentVectorGroupId(), 42, writeKey, readKey)
	if err != nil {
		t.Fatalf("EpochKeysDigest refused two 32 octet keys: %v", err)
	}
	seen := map[string]string{hex.EncodeToString(base): "the unaltered inputs"}
	altered := 0
	note := func(what string, digest []byte) {
		altered++
		key := hex.EncodeToString(digest)
		if already, collided := seen[key]; collided {
			t.Errorf("%s and %s give the same digest, so that input is not in the preimage", what, already)
			return
		}
		seen[key] = what
	}
	for i := 0; i < 32; i++ {
		group := attachmentVectorGroupId()
		group[i] ^= 0xFF
		digest, err := EpochKeysDigest(group, 42, writeKey, readKey)
		if err != nil {
			t.Fatalf("a flipped group_id octet was refused: %v", err)
		}
		note(fmt.Sprintf("group_id octet %d flipped", i), digest)
	}
	for _, epoch := range u64Boundaries() {
		if epoch == 42 {
			continue
		}
		digest, err := EpochKeysDigest(attachmentVectorGroupId(), epoch, writeKey, readKey)
		if err != nil {
			t.Fatalf("epoch %d was refused: %v", epoch, err)
		}
		note(fmt.Sprintf("epoch %d", epoch), digest)
	}
	for i := range writeKey {
		altered := slices.Clone(writeKey)
		altered[i] ^= 0xFF
		digest, err := EpochKeysDigest(attachmentVectorGroupId(), 42, altered, readKey)
		if err != nil {
			t.Fatalf("a flipped write_key octet was refused: %v", err)
		}
		note(fmt.Sprintf("write_key octet %d flipped", i), digest)
	}
	for i := range readKey {
		altered := slices.Clone(readKey)
		altered[i] ^= 0xFF
		digest, err := EpochKeysDigest(attachmentVectorGroupId(), 42, writeKey, altered)
		if err != nil {
			t.Fatalf("a flipped read_key octet was refused: %v", err)
		}
		note(fmt.Sprintf("read_key octet %d flipped", i), digest)
	}
	// and the two keys are not interchangeable: LP frames each of them, so swapping them is
	// a different preimage and not the same octets in a different order
	swapped, err := EpochKeysDigest(attachmentVectorGroupId(), 42, readKey, writeKey)
	if err != nil {
		t.Fatalf("the swapped keys were refused: %v", err)
	}
	note("the two keys swapped", swapped)
	// the count is derived from what the walk actually did rather than written as
	// arithmetic over the boundary tables, because that arithmetic was wrong the first time
	// this test ran — it subtracted an epoch 42 the boundary table does not contain — and a
	// walk whose own size is a guess is a walk that can shrink without saying so
	if want := 1 + altered; len(seen) != want {
		t.Errorf("%d distinct digests over %d altered inputs plus the unaltered one", len(seen), altered)
	}
	if altered == 0 {
		t.Fatal("nothing was altered, so this property held over one digest")
	}
	t.Logf("%d altered inputs, %d distinct digests", altered, len(seen))
}

// A key that is not exactly thirty two octets has no digest.
//
// Both keys, every length either side, and the refusal is the field length sentinel rather
// than a digest over whatever was handed in. The caller that reaches this is a server or a
// committer that looked a key up and got nothing back, and hashing that would produce a
// value a second implementation holding the real key can never reproduce — which surfaces
// as a commit refused at a comparison with nothing to say why.
func TestTheEpochKeysDigestRefusesAKeyThatIsNotThirtyTwoOctets(t *testing.T) {
	good := aadRamp(0x70, 32)
	refusals := 0
	for length := 0; length <= 64; length++ {
		if length == epochWriteKeyBytes {
			continue
		}
		short := fillBytes(0xFB, length)
		if _, err := EpochKeysDigest(attachmentVectorGroupId(), 42, short, good); !errors.Is(err, ErrServerAttachmentFieldLength) {
			t.Fatalf("a write_key of %d octets answered %v, want ErrServerAttachmentFieldLength", length, err)
		}
		if _, err := EpochKeysDigest(attachmentVectorGroupId(), 42, good, short); !errors.Is(err, ErrServerAttachmentFieldLength) {
			t.Fatalf("a read_key of %d octets answered %v, want ErrServerAttachmentFieldLength", length, err)
		}
		refusals++
	}
	if refusals == 0 {
		t.Fatal("no length was refused, so this walk asserted nothing")
	}
	// the positive control, in the same test: the one length it does not refuse
	if _, err := EpochKeysDigest(attachmentVectorGroupId(), 42, good, good); err != nil {
		t.Fatalf("two 32 octet keys were refused: %v", err)
	}
	t.Logf("%d lengths refused on both keys, and 32 accepted", refusals)
}

// The kind 0x0005 vector IS the kind 0x0001 vector with its two keys hashed.
//
// This is ruling 27 written as an equation between two pinned hexadecimal strings, and it is
// the strongest statement in this file about the amendment, because neither side of it is
// this encoder's output: the left is the vector aad_test.go pinned independently before this
// kind existed, and the right is a string derived by a program that imports nothing from
// here. What runs between them is the package, and the six public fields are LIFTED OUT OF
// THE 0x0001 VECTOR'S OWN OCTETS rather than written a second time, so a field that moved in
// one vector and not the other fails here instead of being copied into both.
func TestTheEpochDigestVectorIsTheEpochVectorWithItsKeysHashed(t *testing.T) {
	epoch, err := ParseServerAttachment(mustHex(attachmentEpochVectorHex))
	if err != nil {
		t.Fatalf("the kind 0x0001 vector does not parse: %v", err)
	}
	digest, err := EpochKeysDigest(attachmentVectorGroupId(), epoch.Epoch.Epoch, epoch.Epoch.WriteKey, epoch.Epoch.ReadKey)
	if err != nil {
		t.Fatalf("the kind 0x0001 vector's own keys were refused: %v", err)
	}
	built := &EpochDigestAttachment{
		Epoch:             epoch.Epoch.Epoch,
		AlgId:             epoch.Epoch.AlgId,
		MediaTtlSeconds:   epoch.Epoch.MediaTtlSeconds,
		DurableTtlSeconds: epoch.Epoch.DurableTtlSeconds,
		GroupContextHash:  epoch.Epoch.GroupContextHash,
		ExpectedWrapCount: epoch.Epoch.ExpectedWrapCount,
		EpochKeysDigest:   digest,
	}
	bs, err := EncodeEpochDigestAttachment(built)
	if err != nil {
		t.Fatalf("the lifted attachment does not encode: %v", err)
	}
	if hex.EncodeToString(bs) != attachmentEpochDigestVectorHex {
		t.Fatalf("the kind 0x0001 vector with its keys hashed is\n%s\nand the kind 0x0005 vector is\n%s",
			hex.EncodeToString(bs), attachmentEpochDigestVectorHex)
	}
	// and the size relation the amendment buys, stated as the two LP framed keys that left
	// rather than as two numbers
	left := 2*(4+epochWriteKeyBytes) - (4 + epochKeysDigestBytes)
	if want, got := len(mustHex(attachmentEpochVectorHex))-left, len(bs); got != want {
		t.Errorf("the kind 0x0005 vector is %d octets and the kind 0x0001 vector less two LP keys plus one LP digest is %d", got, want)
	}
}

// The second vector: the other durable sentinel, the smallest legal fan out, and the epoch
// that makes its digest a different number from the first vector's.
func TestTheSecondEpochDigestVectorIsPinnedToItsExactBytes(t *testing.T) {
	built := &EpochDigestAttachment{
		Epoch:             1,
		AlgId:             0x0031,
		MediaTtlSeconds:   0,
		DurableTtlSeconds: 0,
		GroupContextHash:  aadRamp(0xc0, 32),
		ExpectedWrapCount: 1,
		EpochKeysDigest:   mustHex(attachmentEpochKeysDigestEpochOneHex),
	}
	bs, err := EncodeEpochDigestAttachment(built)
	if err != nil {
		t.Fatalf("the second vector does not encode: %v", err)
	}
	if hex.EncodeToString(bs) != attachmentEpochDigestSecondVectorHex {
		t.Fatalf("the second vector encodes to\n%s\nwant\n%s", hex.EncodeToString(bs), attachmentEpochDigestSecondVectorHex)
	}
	parsed, err := ParseEpochDigestAttachment(bs)
	if err != nil {
		t.Fatalf("the second vector does not parse: %v", err)
	}
	if difference := attachmentDifference(
		&ServerAttachment{Kind: AttachmentEpochDigest, EpochDigest: built},
		&ServerAttachment{Kind: AttachmentEpochDigest, EpochDigest: parsed},
	); difference != "" {
		t.Errorf("the second vector does not round trip: %s differs", difference)
	}
	// the two vectors carry the two durable sentinels and no other value, which is the half
	// of spec B section 7.3 case 3 a hand written range check breaks silently
	first, err := ParseEpochDigestAttachment(mustHex(attachmentEpochDigestVectorHex))
	if err != nil {
		t.Fatalf("the first vector does not parse: %v", err)
	}
	if first.DurableTtlSeconds != 0xFFFFFFFF || parsed.DurableTtlSeconds != 0 {
		t.Errorf("the two vectors carry durable_ttl_seconds %d and %d, want the indefinite sentinel and the unset one",
			first.DurableTtlSeconds, parsed.DurableTtlSeconds)
	}
	if parsed.ExpectedWrapCount != 1 {
		t.Errorf("the second vector expects %d wraps, want the smallest legal fan out of 1", parsed.ExpectedWrapCount)
	}
	// and the two digests differ although the two keys are identical, which is u64(epoch)
	// inside the preimage observed on the wire rather than through the helper
	if bytes.Equal(first.EpochKeysDigest, parsed.EpochKeysDigest) {
		t.Error("the two vectors carry the same digest over the same keys at two epochs, so opens_epoch is not in the preimage")
	}
}

// A DOOR REFUSES A KIND IT DOES NOT SERVE BY NAME, rather than parsing it into something,
// and a record carries that kind's octets through regardless.
//
// ── WHICH ASSERTION CHANGED MEANING HERE, AND WHY THAT IS THE ROLLOUT ────────────────
//
// This test was TestARecordCarriesAKindTheServersDoorRefusesByName and it asserted that
// ParseServerAttachment REFUSES kind 0x0005 by name. That was the STALE-SERVER half of
// spec B section 5.4's acceptance window: a server built before the keys could travel
// beside the record refuses such a commit loudly at check 3 instead of installing an epoch
// whose keys it was never handed, and a stale receiver follows the commit anyway because no
// receive path reads a field of an epoch attachment. It is what made the window a rollout
// instead of a flag day, and it was true of every build up to the one that widened the door.
//
// IT CANNOT BE TRUE OF THIS BUILD, because this build is past the widening: section 5.1
// check 3's door serves 0x0005 here, which is the window's step 1. The stale half is now
// held by binaries, not by this package, and a test that went on asserting it would be
// asserting that the change had not been made.
//
// SO THE ASSERTION IS RE-EXPRESSED RATHER THAN DELETED, AND IT IS WIDER THAN IT WAS. The
// mechanism — a door naming the kind it will not serve, instead of parsing those octets
// into something, while the RECORD carries them through untouched — is the thing that made
// the rollout legible, and it is a relation between a door and a kind rather than a fact
// about 0x0005. It is now walked over EVERY (door, kind) pair this package defines, from
// the written down attachmentDoorServes table: one door × one unserved kind before, two
// doors × six kinds now. Today the pairs that refuse are the five section 5.11 kinds at the
// epoch digest door, and that door refusing kind 0x0001 is not a leftover — it is the epoch
// key install path NOT being reachable through the function that exists to take the keys
// out of it.
//
// The positive control is in the same test and is per door, because a door that refused
// everything would satisfy the refusal half on its own, and because with the complement of
// one door now empty the controls are what keep that door under this test at all.
//
// THE CONTROL COMPARES VALUES AND OCTETS, WHICH THE FIRST RE-EXPRESSION DROPPED. The test
// this replaced asserted, in its control loop, that the attachment the door handed back was
// the one that went in; the re-expression kept only "it did not refuse", and so did every
// other walk over parseAtDoor. In the commit that widened the door, NOTHING Test-named in
// this package compared a value or an octet of the newly served kind through section 5.1
// check 3's door: two mutants that forked its wire format — one zeroing two fields on the way
// in, one on the way out — were green on this package, on messagegroup and on protocol under
// `go test -run 'Test'`, and red only in FuzzParseServerAttachment's seed corpus, which that
// filter — the one every command in that commit's evidence used — never ran. Dropping an
// assertion while calling the replacement wider is the failure; restoring it here makes the
// claim true, because the old control covered five kinds at one door and this covers six
// kinds at one and one at the other, by value AND by octet, with the record still in the loop.
func TestARecordCarriesAKindADoorRefusesByName(t *testing.T) {
	byKind := validAttachmentsByKind(t)
	refused, accepted := 0, 0
	for _, door := range attachmentDoorNames() {
		doorRefused, doorAccepted := 0, 0
		for _, code := range definedAttachmentCodes() {
			kind := ServerAttachmentKind(code)
			// the absent attachment has no octets for a record to carry, and its two
			// spellings are asserted by the absent/empty tests of their own
			if kind == AttachmentNone {
				continue
			}
			attachment, err := encodeAtItsDoor(byKind[kind])
			if err != nil {
				t.Fatalf("kind 0x%04x does not encode at its own door: %v", code, err)
			}
			// THE RECORD HALF, run for served and unserved kinds alike: the octets encode,
			// ParseRecord brings them back with is_commit still set and the attachment slot
			// identical. recordSlotRoundTrip asserts all three.
			slot := recordSlotRoundTrip(t, attachment)
			parsed, parseErr := parseAtDoor(t, door, slot)
			if slices.Contains(servedAttachmentCodes(t, door), code) {
				if parseErr != nil {
					t.Fatalf("%s is written down as serving kind 0x%04x and refused it after the record round trip: %v",
						door, code, parseErr)
				}
				// WHAT THE DOOR HANDED BACK, and not merely that it said yes. This is the
				// assertion the old test made in its positive control and the re-expression
				// dropped, and it is restored WIDER than it was: the old one compared values
				// for the five section 5.11 kinds at one door, this compares them for every
				// served (door, kind) pair the table names.
				if difference := attachmentDifference(byKind[kind], parsed); difference != "" {
					t.Errorf("%s accepted kind 0x%04x after the record round trip and handed back an attachment whose %s differs",
						door, code, difference)
				}
				// AND THE OCTETS IT WRITES FOR WHAT IT JUST READ are the octets it read. The
				// comparison above passes an encode arm and a parse arm that agree with each
				// other and with nobody else, which at a door two of them serve is a fork in
				// the wire format of a kind — and H(server_attachment) reaches the write_auth
				// mac and both aeads, so the fork is visible only at a mac nobody can see into.
				again, err := encodeAtDoor(t, door, parsed)
				if err != nil {
					t.Fatalf("%s parsed kind 0x%04x out of the record and then refused to re-encode it: %v", door, code, err)
				}
				if !bytes.Equal(again, slot) {
					t.Errorf("%s read kind 0x%04x out of the record and wrote different octets back, so this kind has two encodings: %s",
						door, code, firstOctetDifference(slot, again))
				}
				doorAccepted++
				continue
			}
			// THE REFUSAL, by sentinel and with the kind in the message
			if parseErr == nil {
				t.Fatalf("%s accepted kind 0x%04x, which it is written down as not serving", door, code)
			}
			if !errors.Is(parseErr, ErrServerAttachmentKindNotServed) {
				t.Fatalf("%s refused kind 0x%04x with %v, want ErrServerAttachmentKindNotServed", door, code, parseErr)
			}
			if named := fmt.Sprintf("0x%04x", code); !strings.Contains(parseErr.Error(), named) {
				t.Errorf("%s's refusal of kind %s reads %q and does not name the kind", door, named, parseErr.Error())
			}
			if !strings.Contains(parseErr.Error(), door) {
				t.Errorf("the refusal of kind 0x%04x reads %q and does not name the door that answered", code, parseErr.Error())
			}
			// and the same octets still parse at the door that DOES serve them, so what the
			// record carried is an attachment and not a malformed field
			if _, err := parseAtItsDoor(kind, slot); err != nil {
				t.Fatalf("kind 0x%04x does not parse at its own door after the record round trip: %v", code, err)
			}
			doorRefused++
		}
		// THE CONTROL IS PER DOOR AND IS REQUIRED OF EVERY DOOR. A door that answered no to
		// everything would satisfy the refusal half above and say nothing.
		if doorAccepted == 0 {
			t.Errorf("%s accepted no kind at all, so its refusals say nothing about the door", door)
		}
		t.Logf("%s: %d kinds refused by name, %d accepted, through the identical record", door, doorRefused, doorAccepted)
		refused += doorRefused
		accepted += doorAccepted
	}
	// THE VACUITY GUARD, and it is about the WHOLE product rather than about one door: with
	// section 5.1 check 3's complement empty since 2026-09-23, a walk keyed on that door
	// alone would now cover nothing at all and pass.
	if refused == 0 {
		t.Fatal("no door refused any kind this package defines, so the by-name refusal held over nothing")
	}
	if accepted == 0 {
		t.Fatal("no door accepted anything, so the refusals above say nothing about any door")
	}
	t.Logf("%d (door, kind) pairs refused by name and %d accepted, over %d doors and %d defined kinds",
		refused, accepted, len(attachmentDoorNames()), len(definedAttachmentCodes()))
}

// One commit record carrying these attachment octets, encoded, parsed back, and its
// attachment slot handed back.
//
// The assertions about the record itself are here rather than at the call site because they
// are the same three every time and each is a different way the property could be satisfied
// vacuously: the record has to ENCODE, it has to come back with is_commit still set — the
// bit a commit is refused for losing — and the slot has to be the identical octets rather
// than something the codec normalised.
func recordSlotRoundTrip(t testing.TB, attachment []byte) []byte {
	t.Helper()
	record := Record{
		Header: RecordHeader{
			Epoch:            1,
			StreamIndex:      7,
			IsCommit:         true,
			RetentionClass:   RetentionPermanent,
			SizeBucket:       SizeBucket256,
			ServerAttachment: attachment,
		},
		CtHead: fillBytes(ctHeadTag, 96),
		CtBody: ctBodyFiller(SizeBucketCtBodyBytes(SizeBucket256)),
	}
	copy(record.Header.GroupId[:], fillBytes(groupIdTag, 32))
	copy(record.Header.SenderHandle[:], fillBytes(senderHandleTag, 16))
	copy(record.Header.BodyHash[:], fillBytes(bodyHashTag, 32))
	copy(record.WriteAuth[:], fillBytes(writeAuthTag, 32))
	bs, err := EncodeRecord(&record)
	if err != nil {
		t.Fatalf("a commit carrying a %d octet attachment does not encode: %v", len(attachment), err)
	}
	parsed, err := ParseRecord(bs)
	if err != nil {
		t.Fatalf("a commit carrying a %d octet attachment does not parse back: %v", len(attachment), err)
	}
	if !parsed.Header.IsCommit {
		t.Fatal("the record came back with is_commit clear, and a commit that loses that bit is a commit the server refuses")
	}
	if !bytes.Equal(parsed.Header.ServerAttachment, attachment) {
		t.Fatalf("the attachment slot came back as %d octets and went in as %d", len(parsed.Header.ServerAttachment), len(attachment))
	}
	return parsed.Header.ServerAttachment
}

// The epoch digest door writes its own kind and reads no other.
//
// Derived the way the alphabet above is: the valid encoding's leading u16 is replaced by
// each of the 65536 values in turn and the set this door accepts has to be the one code it
// writes. A door that read the body first and the kind afterwards, or that fell back to a
// default, lands here — and so does the edit that would undo the whole rollout, because
// accepting kind 0x0001 at this door is the epoch key install path reached through the
// function that exists to take the keys out of it.
func TestTheEpochDigestDoorParsesItsOwnKindAndNoOther(t *testing.T) {
	valid, err := EncodeEpochDigestAttachment(validAttachmentsByKind(t)[AttachmentEpochDigest].EpochDigest)
	if err != nil {
		t.Fatalf("the valid epoch digest does not encode: %v", err)
	}
	kind, err := syntax.NewReader(valid[:2]).ReadUint16()
	if err != nil {
		t.Fatalf("the encoding carries no kind: %v", err)
	}
	if want := ruledAttachmentKindCodes[AttachmentEpochDigest]; kind != want {
		t.Fatalf("the epoch digest door writes kind 0x%04x and ruling 27 gives it 0x%04x", kind, want)
	}
	accepted := []int{}
	relabelled := slices.Clone(valid)
	for code := 0; code <= 0xFFFF; code++ {
		relabelled[0] = byte(code >> 8)
		relabelled[1] = byte(code)
		if _, err := ParseEpochDigestAttachment(relabelled); err == nil {
			accepted = append(accepted, code)
		}
	}
	if !slices.Equal(accepted, []int{int(kind)}) {
		t.Errorf("the epoch digest door accepts %v, want exactly [%d]", accepted, kind)
	}
	// and the refusal of a kind section 5.11 DOES define is the door sentinel and not a
	// decode failure, which is the direction a caller has to be able to tell apart
	if _, err := ParseEpochDigestAttachment(mustHex(attachmentEpochVectorHex)); !errors.Is(err, ErrServerAttachmentKindNotServed) {
		t.Errorf("the epoch digest door refused a real kind 0x0001 attachment with %v, want ErrServerAttachmentKindNotServed", err)
	}
}

// Byte exact both ways over the sixth kind's whole corpus, and the encoding is the layout
// stated independently in rawEpochDigestAttachment.
func TestEveryEpochDigestRoundTripsByteExactAndIsTheStatedLayout(t *testing.T) {
	entries := epochDigestCorpus(t)
	for _, entry := range entries {
		first, err := EncodeEpochDigestAttachment(entry.attachment.EpochDigest)
		if err != nil {
			t.Fatalf("%s: the door refused a valid attachment: %v", entry.name, err)
		}
		want := rawAttachmentOf(t, entry.attachment).encode(t)
		if !bytes.Equal(first, want) {
			t.Fatalf("%s: the encoder wrote\n%s\nand the layout is\n%s", entry.name, hex.EncodeToString(first), hex.EncodeToString(want))
		}
		parsed, err := ParseEpochDigestAttachment(first)
		if err != nil {
			t.Fatalf("%s: an attachment this package encoded does not parse: %v", entry.name, err)
		}
		if difference := attachmentDifference(entry.attachment,
			&ServerAttachment{Kind: AttachmentEpochDigest, EpochDigest: parsed}); difference != "" {
			t.Fatalf("%s: the parsed attachment differs from the encoded one: %s", entry.name, difference)
		}
		again, err := EncodeEpochDigestAttachment(parsed)
		if err != nil {
			t.Fatalf("%s: the parsed attachment does not re-encode: %v", entry.name, err)
		}
		if !bytes.Equal(first, again) {
			t.Fatalf("%s: re-encoding gave %d different octets, so this attachment has two encodings", entry.name, len(again))
		}
	}
	t.Logf("%d epoch digest attachments round tripped byte exact", len(entries))
}

// The corpus of one kind, whichever of this file's two cross products that kind's axes live
// in. Derived by filtering both, so a kind whose corpus moves stays covered, and fatal on an
// empty answer so a walk over it cannot hold vacuously.
func corpusOfKind(t testing.TB, kind ServerAttachmentKind) []attachmentCorpusEntry {
	t.Helper()
	entries := []attachmentCorpusEntry{}
	for _, entry := range append(attachmentCorpus(t), epochDigestCorpus(t)...) {
		if entry.attachment.Kind == kind {
			entries = append(entries, entry)
		}
	}
	if len(entries) == 0 {
		t.Fatalf("no corpus entry carries kind 0x%04x, so a property walked over it would hold over nothing", uint16(kind))
	}
	return entries
}

// A KIND TWO DOORS SERVE HAS ONE ENCODING AT BOTH, and both read it back to the attachment
// that went in.
//
// attachment.go says this in its own voice, at EncodeEpochDigestAttachment: the two doors
// "produce THE SAME OCTETS because the framing and the body table are one set of code behind
// both". That sentence became sayable on 2026-09-23, when section 5.1 check 3's door was
// widened to the sixth kind, and nothing measured it — every walk in this file ran one door,
// or asked the other only whether it said yes. An encode arm that wrote two fields of the
// digest body differently at one door is a fork in the wire format of the only kind two doors
// serve, and it passed `go test -run 'Test'` on this package, on messagegroup and on
// protocol; H(server_attachment) reaches the write_auth mac and both aeads, so on the wire
// that fork surfaces as a mac nobody can see into.
//
// IT IS WRITTEN OVER THE SET OF KINDS TWO DOORS SERVE, not over 0x0005, and it is derived
// from attachmentDoorServes rather than listed: the day a seventh kind is served at a second
// door it is under this walk with nobody remembering it. Today that set is exactly one kind,
// and the guard at the bottom is what says so out loud instead of passing quietly if it
// empties.
func TestAKindTwoDoorsServeHasOneEncodingAtBoth(t *testing.T) {
	compared := 0
	kinds := []ServerAttachmentKind{}
	for _, code := range definedAttachmentCodes() {
		kind := ServerAttachmentKind(code)
		// the absent attachment's encoding is no octets at all, and both its spellings are
		// asserted by the absent/empty tests of their own
		if kind == AttachmentNone {
			continue
		}
		doors := attachmentDoorsServing(t, code)
		if len(doors) < 2 {
			continue
		}
		kinds = append(kinds, kind)
		for _, entry := range corpusOfKind(t, kind) {
			written := map[string][]byte{}
			for _, door := range doors {
				bs, err := encodeAtDoor(t, door, entry.attachment)
				if err != nil {
					t.Fatalf("%s: %s is written down as serving kind 0x%04x and refused to encode it: %v",
						entry.name, door, code, err)
				}
				written[door] = bs
			}
			// THE OCTETS, every door's against the first door's, so a disagreement names both
			first := doors[0]
			for _, door := range doors[1:] {
				if !bytes.Equal(written[door], written[first]) {
					t.Fatalf("%s: %s and %s write different octets for kind 0x%04x, so this kind has two encodings on the wire: %s",
						entry.name, first, door, code, firstOctetDifference(written[first], written[door]))
				}
			}
			// THE VALUES, every door reading every door's octets. This is the half the octet
			// comparison cannot see: two parse arms that drop the same field agree with each
			// other perfectly and hand the caller an attachment that is not the one written.
			for _, writer := range doors {
				for _, reader := range doors {
					parsed, err := parseAtDoor(t, reader, written[writer])
					if err != nil {
						t.Fatalf("%s: %s refused the octets %s wrote for kind 0x%04x: %v", entry.name, reader, writer, code, err)
					}
					if difference := attachmentDifference(entry.attachment, parsed); difference != "" {
						t.Fatalf("%s: %s read what %s wrote for kind 0x%04x and its %s differs",
							entry.name, reader, writer, code, difference)
					}
				}
			}
			compared++
		}
	}
	// THE VACUITY GUARD, over the SET this walk runs on rather than over one door: narrow
	// either door back and this test would have nothing left to compare and would pass.
	if compared == 0 {
		t.Fatalf("no kind this package defines is served at two doors, so the one-encoding property held over nothing; the table reads %v",
			attachmentDoorServes)
	}
	t.Logf("%d corpus entries compared across both doors, over the %d kind(s) two doors serve: %v",
		compared, len(kinds), kinds)
}

// Nothing malformed is silently accepted and changed: every single octet truncation is
// refused, every trailing octet is refused, and every single octet corruption either is
// refused or re-encodes to exactly the corrupted bytes.
//
// The last one is the half a round trip over well formed attachments cannot see. Read
// expected_wrap_count as a u16 and every attachment this package writes still round trips,
// because the two octets it ignores are two it also never wrote.
func TestEveryAlterationOfAnEpochDigestIsRefusedOrReEncodesToItself(t *testing.T) {
	truncations := 0
	trailing := 0
	corruptions := 0
	for _, entry := range epochDigestWalkCorpus(t) {
		valid, err := EncodeEpochDigestAttachment(entry.attachment.EpochDigest)
		if err != nil {
			t.Fatalf("%s: the door refused a valid attachment: %v", entry.name, err)
		}
		for cut := range len(valid) {
			if _, err := ParseEpochDigestAttachment(valid[:cut]); err == nil {
				t.Fatalf("%s: a truncation to %d of %d octets parsed", entry.name, cut, len(valid))
			}
			truncations++
		}
		for _, extra := range []byte{0x00, 0xFF} {
			if _, err := ParseEpochDigestAttachment(append(slices.Clone(valid), extra)); err == nil {
				t.Fatalf("%s: a trailing 0x%02x parsed", entry.name, extra)
			}
			trailing++
		}
		for offset := range valid {
			for delta := 1; delta <= 0xFF; delta++ {
				corrupted := slices.Clone(valid)
				corrupted[offset] = byte(int(corrupted[offset]) + delta)
				parsed, err := ParseEpochDigestAttachment(corrupted)
				if err != nil {
					corruptions++
					continue
				}
				again, err := EncodeEpochDigestAttachment(parsed)
				if err != nil {
					t.Fatalf("%s: octet %d corrupted parsed and then refused to re-encode: %v", entry.name, offset, err)
				}
				if !bytes.Equal(again, corrupted) {
					t.Fatalf("%s: octet %d corrupted parsed and re-encoded to different octets, so those bytes have two readings", entry.name, offset)
				}
				corruptions++
			}
		}
	}
	if truncations == 0 || trailing == 0 || corruptions == 0 {
		t.Fatalf("the walk made %d truncations, %d trailing octets and %d corruptions", truncations, trailing, corruptions)
	}
	t.Logf("%d truncations, %d trailing octets and %d corruptions", truncations, trailing, corruptions)
}

// An epoch digest that expects no wraps is refused, on both sides of its door.
//
// The same clause of spec B section 5.1 check 3 kind 0x0001 answers, and it is asserted here
// rather than inherited: the check is per body, and a body that grew its own arm could have
// dropped it.
func TestAnEpochDigestExpectingNoWrapsIsRefused(t *testing.T) {
	attachment := validEpochDigestAttachment(t, 0, 42, 1, 1, 0)
	_, err := EncodeEpochDigestAttachment(attachment.EpochDigest)
	if !errors.Is(err, ErrExpectedWrapCountZero) {
		t.Fatalf("the encoder answered %v, want ErrExpectedWrapCountZero", err)
	}
	bs := rawAttachmentOf(t, attachment).encode(t)
	if _, err := ParseEpochDigestAttachment(bs); !errors.Is(err, ErrExpectedWrapCountZero) {
		t.Fatalf("the parser answered %v, want ErrExpectedWrapCountZero", err)
	}
	// every other value of the field is accepted, so the refusal is about zero and not about
	// the field
	for _, count := range u32BoundariesAboveZero() {
		if _, err := EncodeEpochDigestAttachment(validEpochDigestAttachment(t, 0, 42, 1, 1, count).EpochDigest); err != nil {
			t.Errorf("expected_wrap_count %d was refused: %v", count, err)
		}
	}
}

// The set of epoch digests the encoder writes and the set the parser reads are one set.
//
// The same property the two halves of section 5.11's door are held to, asserted at this door
// as well, over a space computed from the edges of every rule the checks have: a length
// either side of every exact width, an algorithm identifier either side of the one its kind
// names, an expected_wrap_count of zero and one. Both halves run the one
// checkServerAttachment, so what this observes is that neither has grown a check of its own.
func TestTheEncoderAndTheParserAdmitTheSameEpochDigests(t *testing.T) {
	candidates := []struct {
		name       string
		attachment *ServerAttachment
	}{}
	add := func(name string, a *ServerAttachment) {
		candidates = append(candidates, struct {
			name       string
			attachment *ServerAttachment
		}{name: name, attachment: a})
	}
	add("the valid one", validAttachmentsByKind(t)[AttachmentEpochDigest])
	for _, field := range attachmentWidthFields(t) {
		if field.kind != AttachmentEpochDigest {
			continue
		}
		for _, length := range []int{field.width - 1, field.width, field.width + 1} {
			if length < 0 {
				continue
			}
			fresh := validAttachmentsByKind(t)[AttachmentEpochDigest]
			attachmentBodyValue(fresh).FieldByName(field.name).Set(reflect.ValueOf(fillBytes(0xFD, length)))
			add(fmt.Sprintf("%s at %d", field.name, length), fresh)
		}
	}
	want := attachmentAlgIds[AttachmentEpochDigest]
	for _, value := range []uint16{want - 1, want, want + 1} {
		fresh := validAttachmentsByKind(t)[AttachmentEpochDigest]
		fresh.EpochDigest.AlgId = value
		add(fmt.Sprintf("alg 0x%04x", value), fresh)
	}
	for _, count := range []uint32{0, 1} {
		add(fmt.Sprintf("wraps %d", count), validEpochDigestAttachment(t, 0, 42, 1, 1, count))
	}

	encoded := 0
	refused := 0
	for _, candidate := range candidates {
		_, encodeErr := EncodeEpochDigestAttachment(candidate.attachment.EpochDigest)
		_, parseErr := ParseEpochDigestAttachment(rawAttachmentOf(t, candidate.attachment).encode(t))
		if (encodeErr == nil) != (parseErr == nil) {
			t.Errorf("%s: the encoder says %v and the parser says %v; the two halves disagree about whether this attachment exists",
				candidate.name, encodeErr, parseErr)
			continue
		}
		if encodeErr != nil {
			refused++
			continue
		}
		encoded++
	}
	if encoded == 0 {
		t.Fatal("the space reached no attachment the encoder writes, so the agreement holds vacuously")
	}
	if refused == 0 {
		t.Fatal("the space reached no refusal, so the agreement says nothing about what either half refuses")
	}
	t.Logf("%d candidates, %d encoded and %d refused by both halves", len(candidates), encoded, refused)
}

// ── the two epochs an epoch digest attachment holds, and the one that ties them ──────

// THE FAILING DIRECTION, PINNED AS A VECTOR: the right keys at the wrong epoch.
//
// It is the first vector with u64(epoch) moved from 42 to 43 and NOTHING else touched, so
// its digest is H(epoch_keys) at opens_epoch 42 while the body says it opens 43. The fifteen
// corpus entries do not cover this direction and could not: every one of them is a question
// about octets, and this is a question about two values neither of which is wrong on its own.
//
// It is a vector and not a constructed value because the number that matters here came from
// outside this package. A test that built the mismatch by calling EpochKeysDigest at one
// epoch and setting Epoch to another would move with any edit that changed both, which is
// exactly the edit the whole amendment has to survive.
const attachmentEpochDigestWrongEpochVectorHex = "0005" +
	"0000005e" +
	"000000000000002b" +
	"0031" +
	"00278d00" +
	"ffffffff" +
	"00000020" + "c0c1c2c3c4c5c6c7c8c9cacbcccdcecfd0d1d2d3d4d5d6d7d8d9dadbdcdddedf" +
	"000005dd" +
	"00000020" + attachmentEpochKeysDigestHex

// The two key ramps every vector in this file is over, as the arguments a checker takes.
func attachmentVectorKeys() ([]byte, []byte) {
	return aadRamp(0x70, 32), aadRamp(0x90, 32)
}

// The right keys at the wrong epoch: accepted by every door this package has, and refused by
// the one check that can see it.
//
// This is the whole of what ruling 27 adds to spec B section 5.1 check 3, stated as the pair
// of facts a second implementation has to reproduce. The codec CANNOT see it — it is never
// handed the keys — so a well formed body whose two halves disagree about which epoch the
// keys open round trips byte for byte, and saying so here is what keeps somebody from adding
// a check to the codec that cannot be written.
func TestTheRightKeysAtTheWrongEpochAreRefusedByTheOnlyCheckThatCanSeeThem(t *testing.T) {
	right := mustHex(attachmentEpochDigestVectorHex)
	wrong := mustHex(attachmentEpochDigestWrongEpochVectorHex)

	// the two vectors differ in ONE octet and it is inside u64(epoch), which is what makes
	// this the epoch direction and not some other alteration
	if len(right) != len(wrong) {
		t.Fatalf("the two vectors are %d and %d octets", len(right), len(wrong))
	}
	differing := []int{}
	for at := range right {
		if right[at] != wrong[at] {
			differing = append(differing, at)
		}
	}
	// kind is 2 octets, the LP body prefix 4, and u64(epoch) the 8 after them
	if len(differing) != 1 || differing[0] < 6 || 14 <= differing[0] {
		t.Fatalf("the two vectors differ at %v, want exactly one octet inside u64(epoch) at offsets 6..13", differing)
	}

	parsed, err := ParseEpochDigestAttachment(wrong)
	if err != nil {
		t.Fatalf("the door refused the wrong epoch vector: %v; the codec is never handed the keys and cannot see this", err)
	}
	if parsed.Epoch != 43 {
		t.Fatalf("the wrong epoch vector says it opens epoch %d, want 43", parsed.Epoch)
	}
	writeKey, readKey := attachmentVectorKeys()

	// the inline positive control, in the same test: the SAME keys against the SAME body at
	// the epoch its digest is over are accepted. Without it a checker that refused everything
	// passes the half below.
	control, err := ParseEpochDigestAttachment(right)
	if err != nil {
		t.Fatalf("the door refused the first vector: %v", err)
	}
	if err := CheckEpochKeysDigest(attachmentVectorGroupId(), control, writeKey, readKey); err != nil {
		t.Fatalf("control: the first vector's own keys are refused at its own epoch: %v", err)
	}

	err = CheckEpochKeysDigest(attachmentVectorGroupId(), parsed, writeKey, readKey)
	if err == nil {
		t.Fatal("the right keys at the wrong epoch were accepted, so nothing ties the digest's epoch to the attachment's")
	}
	if !errors.Is(err, ErrEpochKeysDigestMismatch) {
		t.Fatalf("the wrong epoch is refused with %v, want ErrEpochKeysDigestMismatch", err)
	}

	// and the OTHER wrong choice at the same call site: a server reaching for the record
	// header's epoch — the epoch the commit is SEALED at, one below the one it OPENS — lands
	// on a digest that matches, which is why the epoch is not a parameter of the checker
	byHand, err := EpochKeysDigest(attachmentVectorGroupId(), 42, writeKey, readKey)
	if err != nil {
		t.Fatalf("EpochKeysDigest refused the two ramps: %v", err)
	}
	if !bytes.Equal(byHand, parsed.EpochKeysDigest) {
		t.Fatal("the wrong epoch vector's digest is not the one at 42, so this test is not measuring what it says")
	}
	t.Logf("the wrong epoch vector carries H(epoch_keys) at 42 in a body that opens 43; CheckEpochKeysDigest refuses it and a checker taking the epoch as a parameter would not")
}

// The digest of the vector's own two keys, at the vector's own epoch, under A DIFFERENT
// GROUP: aadKatOrdinaryHeader's 0x21 ramp rather than the commit header's 0x11 ramp.
//
// Ruling 34's failing direction, and it is a number rather than a property for the reason the
// wrong epoch vector is octets rather than a construction: an implementation that left
// LP(group_id) out of the preimage answers ONE digest for both groups, and the cheapest way
// for it to find that out is a second number it cannot reproduce. Derived outside this
// package by the same hand method as the other two.
const attachmentEpochKeysDigestOtherGroupHex = "7789a6e53aec247dcd5b0740a3cc885a29c1f08a15cf792a61f4cba78f6f95cf"

// THE OTHER FAILING DIRECTION: the right keys at the right epoch, of the wrong group.
//
// This one cannot be a vector of the attachment's octets the way the wrong epoch one is,
// because the group is deliberately not a field of the body — so the mismatch lives in the
// one argument the server brings from outside, and that is exactly where ruling 34 says the
// exposure was. Before the group joined the preimage, every assertion below held with the
// groups swapped: one digest served every group, and an epoch's key pair from one group
// verified against an attachment submitted in another.
func TestTheRightKeysOfTheWrongGroupAreRefused(t *testing.T) {
	body, err := ParseEpochDigestAttachment(mustHex(attachmentEpochDigestVectorHex))
	if err != nil {
		t.Fatalf("the first vector does not parse: %v", err)
	}
	writeKey, readKey := attachmentVectorKeys()

	// the inline positive control, in the same test: the vector's own group is accepted, so a
	// checker that refused everything cannot pass the half below
	if err := CheckEpochKeysDigest(attachmentVectorGroupId(), body, writeKey, readKey); err != nil {
		t.Fatalf("control: the vector's own group is refused: %v", err)
	}

	var other [32]byte
	copy(other[:], aadRamp(0x21, 32))
	if other == attachmentVectorGroupId() {
		t.Fatal("the two groups are the same value, so this test compares a group with itself")
	}
	err = CheckEpochKeysDigest(other, body, writeKey, readKey)
	if err == nil {
		t.Fatal("the right keys at the right epoch of another group were accepted, so the digest says nothing about which group it is an epoch of")
	}
	if !errors.Is(err, ErrEpochKeysDigestMismatch) {
		t.Fatalf("the wrong group is refused with %v, want ErrEpochKeysDigestMismatch", err)
	}

	// and the other group's number is pinned, so an implementation without the group term
	// meets a second value it cannot reproduce rather than only a refusal it can explain away
	otherDigest, err := EpochKeysDigest(other, body.Epoch, writeKey, readKey)
	if err != nil {
		t.Fatalf("EpochKeysDigest refused the two ramps under the other group: %v", err)
	}
	if hex.EncodeToString(otherDigest) != attachmentEpochKeysDigestOtherGroupHex {
		t.Fatalf("the other group's digest is %s, want %s", hex.EncodeToString(otherDigest), attachmentEpochKeysDigestOtherGroupHex)
	}
	if hex.EncodeToString(body.EpochKeysDigest) == attachmentEpochKeysDigestOtherGroupHex {
		t.Fatal("the two groups give the same digest, so LP(group_id) is not in the preimage")
	}

	// every octet of the group is in it, not just the first: a preimage that framed a
	// truncated or padded group would agree with the vector and disagree with these
	flipped := 0
	for i := 0; i < 32; i++ {
		near := attachmentVectorGroupId()
		near[i] ^= 0xFF
		if err := CheckEpochKeysDigest(near, body, writeKey, readKey); !errors.Is(err, ErrEpochKeysDigestMismatch) {
			t.Fatalf("a group differing from the vector's in octet %d answered %v, want ErrEpochKeysDigestMismatch", i, err)
		}
		flipped++
	}
	if flipped != 32 {
		t.Fatalf("walked %d group octets, want 32", flipped)
	}
	t.Logf("the vector's own group is accepted, another group and all %d one-octet neighbours are refused", flipped)
}

// NewEpochDigestAttachment reads the epoch ONCE, from the body it is building, so the
// mismatch above is not representable through it.
//
// The property is the tie and not the equality: for every epoch in the walk the built digest
// is the one at THAT epoch, and moving the built body's Epoch to any other epoch in the walk
// makes the checker refuse it. A constructor that took a second epoch would pass the first
// half and fail the second on the day a caller passed the wrong one — which is a day no test
// can schedule, so the constructor is built to have no second epoch instead.
func TestNewEpochDigestAttachmentReadsTheEpochOnceFromTheBodyItBuilds(t *testing.T) {
	writeKey, readKey := attachmentVectorKeys()
	epochs := []uint64{0, 1, 2, 42, 43, 0x100000000, 0xFFFFFFFFFFFFFFFF}
	built := map[uint64]*EpochDigestAttachment{}
	for _, epoch := range epochs {
		one, err := NewEpochDigestAttachment(attachmentVectorGroupId(), EpochDigestAttachment{
			Epoch:             epoch,
			AlgId:             attachmentAlgIds[AttachmentEpochDigest],
			MediaTtlSeconds:   2592000,
			DurableTtlSeconds: 0xFFFFFFFF,
			GroupContextHash:  aadRamp(0xc0, 32),
			ExpectedWrapCount: 1501,
		}, writeKey, readKey)
		if err != nil {
			t.Fatalf("epoch %d: the constructor refused a well formed body: %v", epoch, err)
		}
		if one.Epoch != epoch {
			t.Fatalf("epoch %d: the body it built opens %d", epoch, one.Epoch)
		}
		if err := CheckEpochKeysDigest(attachmentVectorGroupId(), one, writeKey, readKey); err != nil {
			t.Fatalf("epoch %d: what the constructor built does not check out against the keys it was handed: %v", epoch, err)
		}
		built[epoch] = one
	}
	crossed := 0
	for _, mine := range epochs {
		for _, other := range epochs {
			if mine == other {
				continue
			}
			moved := *built[mine]
			moved.Epoch = other
			err := CheckEpochKeysDigest(attachmentVectorGroupId(), &moved, writeKey, readKey)
			if err == nil {
				t.Fatalf("the digest built at epoch %d still checks out in a body that opens %d", mine, other)
			}
			if !errors.Is(err, ErrEpochKeysDigestMismatch) {
				t.Fatalf("epoch %d in a body opening %d is refused with %v, want ErrEpochKeysDigestMismatch", mine, other, err)
			}
			crossed++
		}
	}
	if crossed == 0 {
		t.Fatal("no pair of distinct epochs was crossed, so the tie is asserted over nothing")
	}
	t.Logf("%d epochs built and checked, %d distinct pairs refused", len(epochs), crossed)
}

// The digest is the one field of the seven that is not the caller's to fill, and arriving
// with one is refused rather than overwritten.
func TestNewEpochDigestAttachmentRefusesADigestItDidNotCompute(t *testing.T) {
	writeKey, readKey := attachmentVectorKeys()
	public := EpochDigestAttachment{
		Epoch:             42,
		AlgId:             attachmentAlgIds[AttachmentEpochDigest],
		MediaTtlSeconds:   2592000,
		DurableTtlSeconds: 0xFFFFFFFF,
		GroupContextHash:  aadRamp(0xc0, 32),
		ExpectedWrapCount: 1501,
	}
	// the control first: unset, it is built and it is the pinned vector's own digest
	answer, err := NewEpochDigestAttachment(attachmentVectorGroupId(), public, writeKey, readKey)
	if err != nil {
		t.Fatalf("control: the constructor refused a body with no digest on it: %v", err)
	}
	if hex.EncodeToString(answer.EpochKeysDigest) != attachmentEpochKeysDigestHex {
		t.Fatalf("the constructor computed %s, want the pinned %s",
			hex.EncodeToString(answer.EpochKeysDigest), attachmentEpochKeysDigestHex)
	}
	// including a digest that is the RIGHT one: the refusal is about who chose the epoch and
	// not about whether the value happens to agree
	for _, arriving := range [][]byte{
		mustHex(attachmentEpochKeysDigestHex),
		mustHex(attachmentEpochKeysDigestEpochOneHex),
		aadRamp(0x00, 32),
		{0x01},
	} {
		already := public
		already.EpochKeysDigest = arriving
		if _, err := NewEpochDigestAttachment(attachmentVectorGroupId(), already, writeKey, readKey); err == nil {
			t.Fatalf("a body arriving with %d digest octets was accepted", len(arriving))
		} else if !errors.Is(err, ErrEpochKeysDigestPresence) {
			t.Fatalf("a body arriving with %d digest octets is refused with %v, want ErrEpochKeysDigestPresence", len(arriving), err)
		}
	}
	// and the body arrives by value, so the digest is not written back into the caller's own
	// struct: a caller that reused it for the next epoch would otherwise meet its own refusal
	if public.EpochKeysDigest != nil {
		t.Fatal("the constructor wrote the digest back into the caller's body")
	}
}

// Everything the constructor answers, the sixth kind's door encodes; everything the door
// would refuse, the constructor refuses first.
//
// A constructor that skipped the checks would hand a committer a body that fails at the
// encoder one call later, with the diagnosis a layer away from the field that is wrong.
func TestNewEpochDigestAttachmentAnswersNothingItsOwnDoorWouldRefuse(t *testing.T) {
	writeKey, readKey := attachmentVectorKeys()
	good := EpochDigestAttachment{
		Epoch:             42,
		AlgId:             attachmentAlgIds[AttachmentEpochDigest],
		MediaTtlSeconds:   2592000,
		DurableTtlSeconds: 0xFFFFFFFF,
		GroupContextHash:  aadRamp(0xc0, 32),
		ExpectedWrapCount: 1501,
	}
	accepted := 0
	refused := 0
	for _, one := range []struct {
		name   string
		public EpochDigestAttachment
		keys   [2][]byte
	}{
		{name: "well formed", public: good, keys: [2][]byte{writeKey, readKey}},
		{name: "media_ttl at zero", public: func() EpochDigestAttachment { c := good; c.MediaTtlSeconds = 0; return c }(), keys: [2][]byte{writeKey, readKey}},
		{name: "durable_ttl unset sentinel", public: func() EpochDigestAttachment { c := good; c.DurableTtlSeconds = 0; return c }(), keys: [2][]byte{writeKey, readKey}},
		{name: "an Ed25519 alg_id", public: func() EpochDigestAttachment { c := good; c.AlgId = 0x0001; return c }(), keys: [2][]byte{writeKey, readKey}},
		{name: "a 31 octet group_context_hash", public: func() EpochDigestAttachment { c := good; c.GroupContextHash = aadRamp(0xc0, 31); return c }(), keys: [2][]byte{writeKey, readKey}},
		{name: "no wraps at all", public: func() EpochDigestAttachment { c := good; c.ExpectedWrapCount = 0; return c }(), keys: [2][]byte{writeKey, readKey}},
		{name: "a 31 octet write_key", public: good, keys: [2][]byte{aadRamp(0x70, 31), readKey}},
		{name: "an empty read_key", public: good, keys: [2][]byte{writeKey, nil}},
	} {
		answer, err := NewEpochDigestAttachment(attachmentVectorGroupId(), one.public, one.keys[0], one.keys[1])
		if err != nil {
			refused++
			continue
		}
		accepted++
		if _, err := EncodeEpochDigestAttachment(answer); err != nil {
			t.Errorf("%s: the constructor answered a body the door then refused: %v", one.name, err)
		}
		if err := CheckEpochKeysDigest(attachmentVectorGroupId(), answer, one.keys[0], one.keys[1]); err != nil {
			t.Errorf("%s: the constructor answered a body that does not check out against its own keys: %v", one.name, err)
		}
	}
	if accepted == 0 {
		t.Fatal("nothing was accepted, so the encode half of this holds vacuously")
	}
	if refused == 0 {
		t.Fatal("nothing was refused, so the constructor checks nothing")
	}
	t.Logf("%d accepted and encodable, %d refused before reaching the door", accepted, refused)
}

// The checker refuses every alteration of either key, and a key of the wrong width is a
// DIFFERENT sentinel from a key that does not match.
//
// The two mean different things to the server holding them: a mismatch is a record whose
// submitter sent keys the mac does not cover, and a width refusal is a caller that looked a
// key up and got nothing back. A server that could not tell them apart would report an
// attacker where there is a bug of its own.
func TestCheckEpochKeysDigestRefusesEveryAlterationOfEitherKey(t *testing.T) {
	writeKey, readKey := attachmentVectorKeys()
	body, err := ParseEpochDigestAttachment(mustHex(attachmentEpochDigestVectorHex))
	if err != nil {
		t.Fatalf("the first vector does not parse: %v", err)
	}
	// the positive control, first: without it every refusal below is satisfied by a checker
	// that answers no to everything
	if err := CheckEpochKeysDigest(attachmentVectorGroupId(), body, writeKey, readKey); err != nil {
		t.Fatalf("control: the vector's own two keys are refused: %v", err)
	}

	mismatches := 0
	for at := 0; at < epochWriteKeyBytes; at += 1 {
		for _, which := range []int{0, 1} {
			keys := [2][]byte{bytes.Clone(writeKey), bytes.Clone(readKey)}
			keys[which][at] ^= 0x80
			err := CheckEpochKeysDigest(attachmentVectorGroupId(), body, keys[0], keys[1])
			if err == nil {
				t.Fatalf("a bit flipped at octet %d of key %d still checks out", at, which)
			}
			if !errors.Is(err, ErrEpochKeysDigestMismatch) {
				t.Fatalf("a bit flipped at octet %d of key %d is refused with %v, want ErrEpochKeysDigestMismatch", at, which, err)
			}
			mismatches += 1
		}
	}
	// the two keys swapped: the same 64 octets in the other order, which an unframed or
	// order blind preimage would accept
	if err := CheckEpochKeysDigest(attachmentVectorGroupId(), body, readKey, writeKey); err == nil {
		t.Fatal("the two keys swapped still check out, so the preimage does not distinguish them")
	} else if !errors.Is(err, ErrEpochKeysDigestMismatch) {
		t.Fatalf("the two keys swapped are refused with %v, want ErrEpochKeysDigestMismatch", err)
	}
	mismatches += 1

	// a key of the wrong width is the width sentinel and not the mismatch one
	for _, wrong := range [][]byte{nil, {}, aadRamp(0x70, 31), aadRamp(0x70, 33), aadRamp(0x70, 64)} {
		for _, which := range []int{0, 1} {
			keys := [2][]byte{writeKey, readKey}
			keys[which] = wrong
			err := CheckEpochKeysDigest(attachmentVectorGroupId(), body, keys[0], keys[1])
			if !errors.Is(err, ErrServerAttachmentFieldLength) {
				t.Fatalf("a %d octet key %d is refused with %v, want ErrServerAttachmentFieldLength", len(wrong), which, err)
			}
			if errors.Is(err, ErrEpochKeysDigestMismatch) {
				t.Fatalf("a %d octet key %d reports a mismatch, which is an attacker where there is a lookup that returned nothing", len(wrong), which)
			}
		}
	}

	// a truncated or absent digest on the body is a mismatch and never a short comparison
	// that happened to agree
	for _, truncated := range [][]byte{nil, {}, body.EpochKeysDigest[:16], body.EpochKeysDigest[:31]} {
		shortened := *body
		shortened.EpochKeysDigest = truncated
		if err := CheckEpochKeysDigest(attachmentVectorGroupId(), &shortened, writeKey, readKey); !errors.Is(err, ErrEpochKeysDigestMismatch) {
			t.Fatalf("a %d octet digest is refused with %v, want ErrEpochKeysDigestMismatch", len(truncated), err)
		}
	}
	if CheckEpochKeysDigest(attachmentVectorGroupId(), nil, writeKey, readKey) == nil {
		t.Fatal("no body at all checks out")
	}
	t.Logf("%d alterations of the two keys refused, every one of them as a mismatch", mismatches)
}

// The checker decides its equality in constant time, and it is read out of the source rather
// than asserted by a comment.
//
// CheckEpochKeysDigest is deliberately NOT named Verify, so it is outside the class
// guardrail G8 derives for the two functions whose answer IS the authentication decision —
// that decision was already made by VerifyWriteAuth over a preimage covering
// H(server_attachment), which covers this digest, and naming this one Verify would widen a
// class whose rule is "nothing outside this package is called at all" to mean "any
// comparison". Being outside that class is the reason this assertion is written down:
// TestNoProductionFunctionComparesDataOutsideConstantTime bans the wrong comparator here as
// it does everywhere, and nothing else requires the right one of THIS function.
func TestTheEpochKeysDigestCheckerReachesTheConstantTimeComparison(t *testing.T) {
	scan := mustScanAuthSources(t, authOwnScanDir)
	if _, declared := scan.decls["CheckEpochKeysDigest"]; !declared {
		t.Fatalf("the scan of %s does not hold CheckEpochKeysDigest at all, so this gate is reading nothing", authOwnScanDir)
	}
	if !authReachesConstantTimeCompare(t, scan, "CheckEpochKeysDigest") {
		t.Fatalf("CheckEpochKeysDigest does not reach %s; the one check ruling 27 adds decides its equality in variable time",
			authConstantTimeComparator)
	}
	// the inline control on the scan itself: the same walk finds it for the verifier that is
	// known to reach it, so a walk that answered yes to everything is visible here
	if !authReachesConstantTimeCompare(t, scan, "VerifyWriteAuth") {
		t.Fatal("control: the walk cannot see VerifyWriteAuth reach the constant time comparison either, so it is broken")
	}
	if authReachesConstantTimeCompare(t, scan, "EncodeEpochDigestAttachment") {
		t.Fatal("control: the walk claims the encoder reaches a constant time comparison, so it answers yes to everything")
	}
}

// End to end at this layer: a body the constructor builds survives its own door and checks
// out against the keys it was built from, over the corpus rather than over one case.
func TestAnEpochDigestBuiltHereSurvivesItsDoorAndChecksOut(t *testing.T) {
	writeKey, readKey := attachmentVectorKeys()
	walked := 0
	for _, entry := range epochDigestWalkCorpus(t) {
		public := *entry.attachment.EpochDigest
		public.EpochKeysDigest = nil
		built, err := NewEpochDigestAttachment(attachmentVectorGroupId(), public, writeKey, readKey)
		if err != nil {
			t.Fatalf("%s: the constructor refused a corpus body: %v", entry.name, err)
		}
		bs, err := EncodeEpochDigestAttachment(built)
		if err != nil {
			t.Fatalf("%s: the door refused what the constructor built: %v", entry.name, err)
		}
		parsed, err := ParseEpochDigestAttachment(bs)
		if err != nil {
			t.Fatalf("%s: the door refused its own octets back: %v", entry.name, err)
		}
		if err := CheckEpochKeysDigest(attachmentVectorGroupId(), parsed, writeKey, readKey); err != nil {
			t.Fatalf("%s: a body that went out through the door does not check out coming back: %v", entry.name, err)
		}
		// and the record slot carries it: the same octets ParseRecord reads back
		if slot := recordSlotRoundTrip(t, bs); !bytes.Equal(slot, bs) {
			t.Fatalf("%s: the record slot answered %d octets, want the %d it was given", entry.name, len(slot), len(bs))
		}
		walked += 1
	}
	if walked == 0 {
		t.Fatal("the corpus walk reached no body, so this holds vacuously")
	}
	t.Logf("%d corpus bodies built, encoded, parsed and checked", walked)
}

// ── the labels this package composes under ──────────────────────────────────────────

// Every domain separation label this package declares, read out of the SOURCE rather than
// off a list.
//
// A list is what this project has been walked past twelve times. What is derived here is the
// class — every string literal in a non test file of this package whose value begins with
// the protocol's own prefix — so a label declared later is under the rule below the day it
// is written, with nobody remembering to add it.
func packageLabels(t testing.TB) map[string][]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("the label gate cannot read this package's directory: %v", err)
	}
	labels := map[string][]string{}
	files := 0
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		files++
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("the label gate cannot parse %s: %v", name, err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			literal, isLiteral := node.(*ast.BasicLit)
			if !isLiteral || literal.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(literal.Value)
			if err != nil || !strings.HasPrefix(value, labelProtocolPrefix) {
				return true
			}
			labels[value] = append(labels[value], fmt.Sprintf("%s:%d", name, fset.Position(literal.Pos()).Line))
			return true
		})
	}
	if files == 0 {
		t.Fatal("the label gate read no go source in this package, so it would report clean having read nothing")
	}
	if len(labels) == 0 {
		t.Fatalf("the label gate found no label at all across %d files, which cannot be true while aad.go declares two", files)
	}
	return labels
}

// The prefix every label in this protocol carries, which is what makes the class derivable at
// all. It is deliberately not one of the labels: a gate that looked for one of the values it
// judges would find exactly that one.
const labelProtocolPrefix = "URmessage/"

// No label this package declares is a prefix of another, and the sixth kind's is one of them.
//
// aad.go's two labels and writeauth.go's two are each held to this pairwise in their own
// files, and each of those tests is about ONE pair. This is the same rule over the class, and
// it is the rule that matters when a label is added: the separation between two preimages
// rests on the bytes of their labels differing INSIDE the shorter of the two, because nothing
// stands between the label and the first field. A label that is a prefix of another is a
// preimage a choice of the following field can turn into the other protocol's, and both
// preimages still build.
func TestNoLabelThisPackageDeclaresIsAPrefixOfAnother(t *testing.T) {
	labels := packageLabels(t)
	names := []string{}
	for label := range labels {
		names = append(names, label)
	}
	slices.Sort(names)
	t.Logf("%d labels declared in this package: %v", len(names), names)

	// the positive control, inline: the four labels that were here before the sixth kind
	// was, and the one it added. A derivation that quietly stopped matching would report an
	// empty class, and the emptiness above is a Fatal for that reason; this is the stronger
	// half, because a derivation that matched the wrong literals would report a full one.
	for _, want := range []string{aadBodyLabel, aadHeadLabel, writeAuthLabel, requestAuthLabel, epochKeysLabel} {
		if _, declared := labels[want]; !declared {
			t.Errorf("the walk did not find %q, which this package declares as a constant", want)
		}
	}
	if len(names) != 5 {
		t.Errorf("the walk found %d labels and this package declares 5; a label added is a label owed a line in the control above", len(names))
	}

	for _, left := range names {
		for _, right := range names {
			if left == right {
				continue
			}
			if strings.HasPrefix(right, left) {
				t.Errorf("%q (%v) is a prefix of %q (%v), so no choice of the field after the shorter one separates the two preimages",
					left, labels[left], right, labels[right])
			}
		}
	}
	// and each label is declared in exactly one place, so no two preimages can be moved
	// together by one edit
	for label, sites := range labels {
		if 1 < len(sites) {
			t.Errorf("%q is written at %v; a label at two sites is two preimages one edit can make equal", label, sites)
		}
	}
}

// ── the fuzz target ─────────────────────────────────────────────────────────────────

const attachmentFuzzCorpusDir = "testdata/fuzz/FuzzParseServerAttachment"

// The corpus checked in beside the fuzz target is read, and it says something.
//
// A corpus directory whose name no longer matches its target is replayed by nothing and
// reported by nothing, so its contents are asserted rather than assumed: it exists, it holds
// entries, and it holds both accepted and refused ones. An input that is refused exercises
// the refusal and stops, and it is the accepted ones that have to re-encode to themselves, so
// a corpus that drifted into refusals alone would leave the whole re-encode half of the fuzz
// property unreachable from it.
//
// The property itself is asserted here as well, over exactly the bytes the fuzz target would
// see, so a corpus entry that violates it fails an ordinary go test rather than waiting for
// somebody to pass -fuzz.
//
// AND EVERY KIND THIS DOOR SERVES HAS ITS VECTOR HERE, derived from the door table rather
// than from a list. These are this door's interop vectors on disk — the digest door's corpus
// asserts its own the same way, and an implementer checking a section 5.1 check 3 parser
// reads THIS directory. It carried four of the five vectors this file pins, which was all of
// them for the kinds the door served, until the door was widened on 2026-09-23: after that
// the only kind 0x0005 entry here was a malformed one (an EpochComplete body under the digest
// kind, kept because it is a near miss the byte walks do not produce), so the newly served
// kind had no valid octets on disk at all and nothing said so.
func TestTheCheckedInAttachmentFuzzCorpusIsReadAndSaysSomething(t *testing.T) {
	entries, err := os.ReadDir(attachmentFuzzCorpusDir)
	if err != nil {
		t.Fatalf("the checked-in fuzz corpus is unreadable at %s: %v", attachmentFuzzCorpusDir, err)
	}
	// the vectors this door's kinds are pinned to, each marked when an entry carries it
	onDisk := map[ServerAttachmentKind]bool{}
	for _, code := range servedAttachmentCodes(t, serverAttachmentDoorName) {
		kind := ServerAttachmentKind(code)
		if _, pinned := attachmentGoldenVectors[kind]; pinned {
			onDisk[kind] = false
		}
	}
	accepted := 0
	refused := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		bs := fuzzCorpusEntry(t, filepath.Join(attachmentFuzzCorpusDir, entry.Name()))
		for kind := range onDisk {
			if hex.EncodeToString(bs) == attachmentGoldenVectors[kind] {
				onDisk[kind] = true
			}
		}
		attachment, err := ParseServerAttachment(bs)
		if err != nil {
			refused++
			continue
		}
		accepted++
		again, err := EncodeServerAttachment(attachment)
		if err != nil {
			t.Fatalf("%s: parsed and then refused to re-encode: %v", entry.Name(), err)
		}
		if !bytes.Equal(again, bs) {
			t.Fatalf("%s: parsed and re-encoded to different octets, so this attachment has two encodings: %s",
				entry.Name(), firstOctetDifference(bs, again))
		}
	}
	if accepted+refused == 0 {
		t.Fatalf("%s holds no corpus entry, so the fuzz target replays nothing but its own well formed seeds", attachmentFuzzCorpusDir)
	}
	if accepted == 0 {
		t.Fatalf("%s: all %d entries are refused, so no entry ever reaches the re-encode half of the property", attachmentFuzzCorpusDir, refused)
	}
	if refused == 0 {
		t.Fatalf("%s: all %d entries are accepted, so the malformed inputs it exists to carry are gone", attachmentFuzzCorpusDir, accepted)
	}
	if len(onDisk) == 0 {
		t.Fatalf("no kind %s serves is pinned to a vector, so the on-disk check below held over nothing", serverAttachmentDoorName)
	}
	carried := 0
	for kind, found := range onDisk {
		if !found {
			t.Errorf("%s is pinned to a vector in this file, %s serves it, and no entry of %s carries those octets — "+
				"so an implementation reading this directory has no example of the kind",
				specAttachmentKindNames[kind], serverAttachmentDoorName, attachmentFuzzCorpusDir)
			continue
		}
		carried++
	}
	t.Logf("%d corpus entries, %d accepted and %d refused; %d of the %d served kinds pinned to a vector have it on disk",
		accepted+refused, accepted, refused, carried, len(onDisk))
}

// The one property that has to hold over bytes nobody chose: an input is refused, or it
// re-encodes to itself exactly. Anything else is a second encoding of one attachment, and
// H(server_attachment) — which reaches the write_auth mac and both aeads — is over exactly
// one of them.
//
// The seeds this target adds are mostly well formed, because a mutator wants a valid
// attachment to work outward from. The malformed inputs live in
// testdata/fuzz/FuzzParseServerAttachment, checked in, which is what makes a plain go test
// replay them: the absent attachment spelled out, a kind nothing defines, a key one octet
// short, a body region longer than the fields inside it, an EpochComplete body under the
// digest kind. Those are edits no single octet walk in this file produces, and having them on
// disk is also what gives a finding from an explicit -fuzz run somewhere to land.
//
// NOTHING HERE IS REACHABLE ONLY FROM A Fuzz NAME. The seeds and the property are each their
// own function, and TestEveryAttachmentFuzzSeedIsRefusedOrReEncodesToItself walks them under
// a name `-run 'Test'` matches — because this target's name is not one, and that filter is
// the one the evidence tables for this package have been written with.
func FuzzParseServerAttachment(f *testing.F) {
	for _, bs := range attachmentFuzzSeeds(f) {
		f.Add(bs)
	}
	f.Fuzz(func(t *testing.T, bs []byte) {
		attachmentParsePropertyHolds(t, fmt.Sprintf("%d octets", len(bs)), bs)
	})
}

// The seeds the target above starts from, as ONE function read by the target and by the test
// below, so neither can drift from the other.
//
// The golden vectors are walked in kind order rather than over the map, so the seed list is
// the same list in the same order every run and a failing seed index names the same input
// twice running.
func attachmentFuzzSeeds(t testing.TB) [][]byte {
	t.Helper()
	seeds := [][]byte{}
	for _, entry := range attachmentWalkCorpus(t) {
		bs, err := EncodeServerAttachment(entry.attachment)
		if err != nil {
			t.Fatalf("%s: EncodeServerAttachment refused a corpus attachment: %v", entry.name, err)
		}
		seeds = append(seeds, bs)
	}
	for _, code := range definedAttachmentCodes() {
		vector, pinned := attachmentGoldenVectors[ServerAttachmentKind(code)]
		if !pinned {
			continue
		}
		bs, err := hex.DecodeString(vector)
		if err != nil {
			t.Fatalf("the kind 0x%04x golden vector is not hexadecimal: %v", code, err)
		}
		seeds = append(seeds, bs)
	}
	seeds = append(seeds, []byte{}, []byte{0x00}, []byte{0x00, 0x01})
	if len(seeds) == 0 {
		t.Fatal("the seed list is empty, so the target starts from nothing and the test below walks nothing")
	}
	return seeds
}

// The property itself, over one input, as ONE function read by the target and by the test
// below. Answers whether the door accepted the input, so a caller walking a fixed set can
// assert that some of it reached the re-encode half at all.
func attachmentParsePropertyHolds(t *testing.T, what string, bs []byte) bool {
	t.Helper()
	attachment, err := ParseServerAttachment(bs)
	if err != nil {
		return false
	}
	if carried, set := attachment.bodyKind(); carried != attachment.Kind || 1 < set {
		t.Fatalf("%s: accepted an attachment of kind 0x%04x carrying %d bodies, the last of them kind 0x%04x",
			what, uint16(attachment.Kind), set, uint16(carried))
	}
	again, err := EncodeServerAttachment(attachment)
	if err != nil {
		t.Fatalf("%s: accepted %d octets and then refused to re-encode them: %v", what, len(bs), err)
	}
	if !bytes.Equal(again, bs) {
		t.Fatalf("%s: accepted %d octets and re-encoded to different ones, so this attachment has two encodings: %s",
			what, len(bs), firstOctetDifference(bs, again))
	}
	return true
}

// EVERY SEED THE FUZZ TARGET STARTS FROM, UNDER AN ORDINARY go test.
//
// ── WHY THIS TEST EXISTS, AND IT IS ABOUT THE FILTER RATHER THAN ABOUT THE PARSER ────
//
// `go test -run 'Test'` does not run a Fuzz target, not even its seeds: "FuzzParseServer-
// Attachment" contains no "Test", so the filter never matches it. Every command in the
// evidence for the commit that widened section 5.1 check 3's door — its results table and its
// six-mutant table alike — used that filter or a narrower list of Test names, so a property
// asserted only inside f.Fuzz was invisible to all of it, and a surviving mutant is first a
// claim about the query. Two mutants that forked the sixth kind's wire format at that door
// were measured green that way; under `-run 'Test|Fuzz'` each failed in this target's seed
// corpus and nowhere else.
//
// So the seeds and the property are each one function, and this Test walks them. It is not a
// second copy of the fuzz body — it calls the same one — and it is the reason a -run 'Test'
// table can no longer pass over this property. The controls are the two directions the walk
// could go quiet in: a seed list all of whose entries are refused never reaches the re-encode
// half, and one all of whose entries are accepted has lost the malformed inputs it carries.
func TestEveryAttachmentFuzzSeedIsRefusedOrReEncodesToItself(t *testing.T) {
	seeds := attachmentFuzzSeeds(t)
	accepted, refused := 0, 0
	for i, bs := range seeds {
		if attachmentParsePropertyHolds(t, fmt.Sprintf("seed %d", i), bs) {
			accepted++
			continue
		}
		refused++
	}
	if accepted == 0 {
		t.Fatalf("all %d seeds are refused, so no seed ever reaches the re-encode half of the property", refused)
	}
	if refused == 0 {
		t.Fatalf("all %d seeds are accepted, so the malformed seeds the target starts from are gone", accepted)
	}
	// AND THE SEED LIST REACHES THE KIND THE DOOR WAS WIDENED TO, asserted rather than
	// assumed: this test is worth nothing here if the one kind whose coverage was fuzz-only
	// is not among the octets it walks.
	digest, err := hex.DecodeString(attachmentEpochDigestVectorHex)
	if err != nil {
		t.Fatalf("the epoch digest vector is not hexadecimal: %v", err)
	}
	carried := false
	for _, bs := range seeds {
		if bytes.Equal(bs, digest) {
			carried = true
		}
	}
	if !carried {
		t.Fatalf("no seed is the kind 0x%04x golden vector, so this walk says nothing about the kind section 5.1 check 3's door was widened to",
			ruledAttachmentKindCodes[AttachmentEpochDigest])
	}
	t.Logf("%d seeds, %d accepted and re-encoded to themselves, %d refused", len(seeds), accepted, refused)
}

// ── the sixth kind's fuzz target ────────────────────────────────────────────────────

const epochDigestFuzzCorpusDir = "testdata/fuzz/FuzzParseEpochDigestAttachment"

// The corpus checked in beside the sixth kind's target is read, and it says something.
//
// THESE ARE THE INTEROP VECTORS ON DISK. The two valid entries are the two vectors this file
// pins — the indefinite durable sentinel at expected_wrap_count 1501, and the unset sentinel
// at expected_wrap_count 1 over a digest at a different epoch — so a second implementation
// can read the octets out of this directory without a go toolchain and check its own parser
// and its own digest against them. The rest are the near miss framings a byte walk does not
// produce, including a real kind 0x0001 attachment, which this door must refuse by name.
func TestTheCheckedInEpochDigestFuzzCorpusIsReadAndSaysSomething(t *testing.T) {
	entries, err := os.ReadDir(epochDigestFuzzCorpusDir)
	if err != nil {
		t.Fatalf("the checked-in fuzz corpus is unreadable at %s: %v", epochDigestFuzzCorpusDir, err)
	}
	accepted := 0
	refused := 0
	pinned := map[string]bool{
		attachmentEpochDigestVectorHex:       false,
		attachmentEpochDigestSecondVectorHex: false,
		// the failing direction ruling 27's own check is about. It belongs on disk with the
		// other two BECAUSE it is accepted here: the codec is never handed the keys, so the
		// only thing that can tell it from the first vector is CheckEpochKeysDigest, and an
		// implementation reading this corpus has to meet one input where the door's yes is
		// not the whole answer.
		attachmentEpochDigestWrongEpochVectorHex: false,
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		bs := fuzzCorpusEntry(t, filepath.Join(epochDigestFuzzCorpusDir, entry.Name()))
		digest, err := ParseEpochDigestAttachment(bs)
		if err != nil {
			refused++
			continue
		}
		accepted++
		if _, carried := pinned[hex.EncodeToString(bs)]; carried {
			pinned[hex.EncodeToString(bs)] = true
		}
		again, err := EncodeEpochDigestAttachment(digest)
		if err != nil {
			t.Fatalf("%s: parsed and then refused to re-encode: %v", entry.Name(), err)
		}
		if !bytes.Equal(again, bs) {
			t.Fatalf("%s: parsed and re-encoded to different octets, so this attachment has two encodings: %s",
				entry.Name(), firstOctetDifference(bs, again))
		}
	}
	if accepted+refused == 0 {
		t.Fatalf("%s holds no corpus entry, so the fuzz target replays nothing but its own well formed seeds", epochDigestFuzzCorpusDir)
	}
	if accepted == 0 {
		t.Fatalf("%s: all %d entries are refused, so no entry ever reaches the re-encode half of the property", epochDigestFuzzCorpusDir, refused)
	}
	if refused == 0 {
		t.Fatalf("%s: all %d entries are accepted, so the malformed inputs it exists to carry are gone", epochDigestFuzzCorpusDir, accepted)
	}
	// the vectors are ON DISK and not only in this file, which is the whole of what makes
	// them reachable by an implementation that is not this one
	for vector, onDisk := range pinned {
		if !onDisk {
			t.Errorf("the vector %s… is pinned in this file and is in no corpus entry, so no second implementation can read it", vector[:12])
		}
	}
	t.Logf("%d corpus entries, %d accepted and %d refused", accepted+refused, accepted, refused)
}

// The one property that has to hold over bytes nobody chose, at the sixth kind's door: an
// input is refused, or it re-encodes to itself exactly.
func FuzzParseEpochDigestAttachment(f *testing.F) {
	for _, bs := range epochDigestFuzzSeeds(f) {
		f.Add(bs)
	}
	f.Fuzz(func(t *testing.T, bs []byte) {
		epochDigestParsePropertyHolds(t, fmt.Sprintf("%d octets", len(bs)), bs)
	})
}

// The seeds the target above starts from, as one function, for the reason its sibling has
// one: `go test -run 'Test'` never runs a Fuzz target, so a seed set only the target reads is
// a seed set the filter this package's evidence is written with never reaches.
//
// The kind 0x0001 vector is among them and it is the one that must be REFUSED here, which is
// what makes the refused count below a number rather than a formality.
func epochDigestFuzzSeeds(t testing.TB) [][]byte {
	t.Helper()
	seeds := [][]byte{}
	for _, entry := range epochDigestWalkCorpus(t) {
		bs, err := EncodeEpochDigestAttachment(entry.attachment.EpochDigest)
		if err != nil {
			t.Fatalf("%s: the door refused a corpus attachment: %v", entry.name, err)
		}
		seeds = append(seeds, bs)
	}
	for _, vector := range []string{attachmentEpochDigestVectorHex, attachmentEpochDigestSecondVectorHex, attachmentEpochVectorHex} {
		bs, err := hex.DecodeString(vector)
		if err != nil {
			t.Fatalf("a pinned vector is not hexadecimal: %v", err)
		}
		seeds = append(seeds, bs)
	}
	seeds = append(seeds, []byte{}, []byte{0x00, 0x05})
	if len(seeds) == 0 {
		t.Fatal("the seed list is empty, so the target starts from nothing and the test below walks nothing")
	}
	return seeds
}

// The property itself, over one input, as one function read by the target and by the test
// below. Answers whether this door accepted the input.
func epochDigestParsePropertyHolds(t *testing.T, what string, bs []byte) bool {
	t.Helper()
	digest, err := ParseEpochDigestAttachment(bs)
	if err != nil {
		return false
	}
	if digest == nil {
		t.Fatalf("%s: accepted %d octets and answered no attachment at all", what, len(bs))
	}
	again, err := EncodeEpochDigestAttachment(digest)
	if err != nil {
		t.Fatalf("%s: accepted %d octets and then refused to re-encode them: %v", what, len(bs), err)
	}
	if !bytes.Equal(again, bs) {
		t.Fatalf("%s: accepted %d octets and re-encoded to different ones, so this attachment has two encodings: %s",
			what, len(bs), firstOctetDifference(bs, again))
	}
	return true
}

// Every seed the sixth kind's target starts from, under an ordinary go test, for the reason
// its sibling above has such a test: the filter every evidence table here uses cannot reach a
// Fuzz name.
func TestEveryEpochDigestFuzzSeedIsRefusedOrReEncodesToItself(t *testing.T) {
	seeds := epochDigestFuzzSeeds(t)
	accepted, refused := 0, 0
	for i, bs := range seeds {
		if epochDigestParsePropertyHolds(t, fmt.Sprintf("seed %d", i), bs) {
			accepted++
			continue
		}
		refused++
	}
	if accepted == 0 {
		t.Fatalf("all %d seeds are refused, so no seed ever reaches the re-encode half of the property", refused)
	}
	if refused == 0 {
		t.Fatalf("all %d seeds are accepted, so the kind 0x0001 vector this door must refuse by name is no longer among them", accepted)
	}
	t.Logf("%d seeds, %d accepted and re-encoded to themselves, %d refused", len(seeds), accepted, refused)
}
