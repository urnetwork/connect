// Tests for the RFC 9420 section 8.1 GroupContext codec.
//
// The shape of this file follows from one fact about the structure: MLS signs over
// serialized forms, and a GroupContext is hashed into the confirmed transcript and
// mixed into every derivation of the epoch. So the failure this file exists to catch
// is not "the codec crashes" but "the codec agrees with itself and with nobody else",
// and round trip symmetry is blind to exactly that. An encoder and a decoder that both
// put the cipher suite before the version round trip perfectly, byte for byte, forever.
//
// What sees it is a golden vector that did not come out of this encoder. There are two
// kinds here and both are needed. The hand derived one is written out group by group
// from the RFC's field order and varint rules, so a reader can check it against the
// specification without running anything. The upstream one is the mlswg key schedule
// corpus, pinned by digest in testdata, which is another implementation's output and is
// therefore evidence about interoperability rather than about self consistency. The
// hand derivation is asserted against the upstream bytes first, so a transcription slip
// in the literal fails as a transcription slip rather than as a codec bug.
package mls

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/urnetwork/connect/mls/syntax"
)

// groupContextHex decodes a hex literal, with a malformed one fatal rather than a
// comparison against nothing. Unlike mustDecodeHex it accepts the empty string, because
// an empty opaque field is a case this file tests on purpose.
func groupContextHex(t *testing.T, name string, value string) []byte {
	t.Helper()
	decoded, err := hex.DecodeString(value)
	if err != nil {
		t.Fatalf("%s is not hex: %v", name, err)
	}
	return decoded
}

// joinBytes concatenates byte groups. The hand derived goldens below are written as one
// argument per field group, which is what makes their derivation readable.
func joinBytes(groups ...[]byte) []byte {
	out := []byte{}
	for _, group := range groups {
		out = append(out, group...)
	}
	return out
}

// repeatByte is the filler for a field whose length matters and whose contents do not.
func repeatByte(value byte, count int) []byte {
	out := make([]byte, count)
	for i := range out {
		out[i] = value
	}
	return out
}

// ---------------------------------------------------------------------------
// the upstream corpus
// ---------------------------------------------------------------------------

// keyScheduleVectorEntry is the part of one mlswg key-schedule.json entry this file
// reads. The file is pinned by digest in vectors_pin_test.go, so these bytes cannot
// drift without a visible re-vendoring.
type keyScheduleVectorEntry struct {
	CipherSuite uint16 `json:"cipher_suite"`
	GroupId     string `json:"group_id"`
	Epochs      []struct {
		GroupContext            string `json:"group_context"`
		TreeHash                string `json:"tree_hash"`
		ConfirmedTranscriptHash string `json:"confirmed_transcript_hash"`
	} `json:"epochs"`
}

// loadKeyScheduleVectors reads the pinned corpus. A file that parsed to nothing is
// fatal: every golden below would then be a comparison against an empty list, which is
// the failure mode that reports green having checked nothing.
func loadKeyScheduleVectors(t *testing.T) []keyScheduleVectorEntry {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "vectors", "key-schedule.json"))
	if err != nil {
		t.Fatalf("read key-schedule.json: %v", err)
	}
	var entries []keyScheduleVectorEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		t.Fatalf("parse key-schedule.json: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("key-schedule.json parsed to no entries, so every vector assertion below would compare against nothing")
	}
	for _, entry := range entries {
		if len(entry.Epochs) == 0 {
			t.Fatalf("cipher suite %d carries no epochs, so its group contexts were not read", entry.CipherSuite)
		}
	}
	return entries
}

// upstreamEntry returns the corpus entry for one suite, so a test can read the named
// fields the group context blob was built out of rather than re-deriving them from it.
func upstreamEntry(t *testing.T, suite uint16) keyScheduleVectorEntry {
	t.Helper()
	for _, entry := range loadKeyScheduleVectors(t) {
		if entry.CipherSuite == suite {
			return entry
		}
	}
	t.Fatalf("cipher suite %d is not in key-schedule.json", suite)
	return keyScheduleVectorEntry{}
}

// upstreamGroupContext returns the group context bytes the corpus records for one epoch
// of one suite, which is what the hand derived golden is checked against.
func upstreamGroupContext(t *testing.T, suite uint16, epoch int) []byte {
	t.Helper()
	entry := upstreamEntry(t, suite)
	if epoch >= len(entry.Epochs) {
		t.Fatalf("cipher suite %d has %d epochs, epoch %d asked for", suite, len(entry.Epochs), epoch)
	}
	return groupContextHex(t, "upstream group_context", entry.Epochs[epoch].GroupContext)
}

// ---------------------------------------------------------------------------
// the hand derived goldens
// ---------------------------------------------------------------------------

// The three 32 byte fields of the mlswg key schedule vector for cipher suite 0x0003,
// transcribed from testdata/vectors/key-schedule.json.
const (
	ksVectorGroupId  = "a897b53575b4dd35fed4466e4e714bfa949eaa72e616a9c68a47b39cb7a60d2e"
	ksVectorTreeHash = "9769e302a99c457350a8e636009b12a2fee068664004606d6318eb3a1977d818"
	ksVectorCth      = "5e57c9364dc71f0f71b19ffe561ab77257c490708a47e29f8f73f2b318201d2f"
)

// handDerivedEpoch0GroupContext is the 112 byte encoding of that vector's epoch 0 group
// context, built here from RFC 9420 section 8.1's field order and section 2.1.2's
// varint rules and from nothing this package computes.
//
// A golden captured as want := syntax.Marshal(gc) would prove only that the encoder
// agrees with itself, which is precisely the property that still holds when two
// implementations disagree, so it would pass in the one case worth catching. Written out
// by hand it is a second, independent statement of the encoding, and
// TestGroupContextHandDerivedGoldenMatchesTheUpstreamVector then checks that statement
// against a third one produced by another implementation entirely.
//
// group by group:
//
//	00 01                     ProtocolVersion mls10 = 1, uint16 big endian, 2 octets
//	00 03                     CipherSuite 0x0003, uint16 big endian, 2 octets
//	20                        group_id<V> byte length 32; 32 <= 63 so the varint is one
//	                          octet, prefix 0b00, value 32 = 0x20
//	a8 97 .. 0d 2e            the 32 group id octets, verbatim
//	00 00 00 00 00 00 00 00   epoch 0, uint64 big endian, 8 octets, no length prefix
//	20                        tree_hash<V> byte length 32
//	97 69 .. d8 18            the 32 tree hash octets, verbatim
//	20                        confirmed_transcript_hash<V> byte length 32
//	5e 57 .. 1d 2f            the 32 transcript hash octets, verbatim
//	00                        extensions<V> byte length 0; the prefix counts BYTES, so an
//	                          empty vector is one zero octet and never an omitted field
//
// 2+2+1+32+8+1+32+1+32+1 = 112.
func handDerivedEpoch0GroupContext(t *testing.T) []byte {
	t.Helper()
	return joinBytes(
		[]byte{0x00, 0x01},
		[]byte{0x00, 0x03},
		[]byte{0x20},
		groupContextHex(t, "ksVectorGroupId", ksVectorGroupId),
		[]byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
		[]byte{0x20},
		groupContextHex(t, "ksVectorTreeHash", ksVectorTreeHash),
		[]byte{0x20},
		groupContextHex(t, "ksVectorCth", ksVectorCth),
		[]byte{0x00},
	)
}

// ksVectorEpoch0GroupContext is the struct that encoding describes.
func ksVectorEpoch0GroupContext(t *testing.T) *GroupContext {
	t.Helper()
	return &GroupContext{
		Version:                 ProtocolVersionMls10,
		CipherSuite:             CipherSuiteX25519ChaCha20Sha256Ed25519,
		GroupId:                 groupContextHex(t, "ksVectorGroupId", ksVectorGroupId),
		Epoch:                   0,
		TreeHash:                groupContextHex(t, "ksVectorTreeHash", ksVectorTreeHash),
		ConfirmedTranscriptHash: groupContextHex(t, "ksVectorCth", ksVectorCth),
		Extensions:              nil,
	}
}

// handDerivedExtensionGolden is the second hand derived golden, covering what the
// published vectors cannot: every mlswg key schedule vector has an empty extensions
// vector, a small epoch, three fields of one identical length and a registered version,
// so on its own the corpus leaves the extension codec, the two octet varint, the empty
// opaque field and the high half of the epoch entirely unpinned.
//
// group by group:
//
//	fa fb                     ProtocolVersion 0xfafb; unregistered on purpose, since the
//	                          codec must carry a code point it does not know rather than
//	                          normalise it, and a version that happened to equal 1 would
//	                          not show a field silently replaced by a constant
//	00 03                     CipherSuite 0x0003
//	03                        group_id<V> byte length 3
//	a1 a2 a3                  the 3 group id octets
//	fe dc ba 98 76 54 32 10   epoch 0xfedcba9876543210, uint64 big endian; every octet
//	                          differs, so a narrower field or a swapped endianness moves
//	                          bytes rather than leaving zeroes where zeroes were
//	01                        tree_hash<V> byte length 1
//	b1                        the one tree hash octet
//	00                        confirmed_transcript_hash<V> byte length 0, a field that is
//	                          present and empty rather than omitted
//	40 47                     extensions<V> byte length 71; 71 > 63 so the varint is two
//	                          octets, prefix 0b01: 0x40|(71>>8) = 0x40 then 71&0xff = 0x47
//	  00 02                   entry 1 extension_type 0x0002, ratchet_tree
//	  40 40                   entry 1 extension_data<V> byte length 64; 64 > 63 so two
//	                          octets, 0x40|(64>>8) = 0x40 then 64&0xff = 0x40
//	  c1 x64                  the 64 body octets
//	  f0 01                   entry 2 extension_type 0xF001, this project's private range
//	  00                      entry 2 extension_data<V> byte length 0
//
// the entries are 2+2+64 = 68 and 2+1 = 3, and 68+3 = 71, which is the vector's own
// prefix. 2+2+1+3+8+1+1+1+2+71 = 92.
func handDerivedExtensionGolden() []byte {
	return joinBytes(
		[]byte{0xfa, 0xfb},
		[]byte{0x00, 0x03},
		[]byte{0x03},
		[]byte{0xa1, 0xa2, 0xa3},
		[]byte{0xfe, 0xdc, 0xba, 0x98, 0x76, 0x54, 0x32, 0x10},
		[]byte{0x01},
		[]byte{0xb1},
		[]byte{0x00},
		[]byte{0x40, 0x47},
		[]byte{0x00, 0x02},
		[]byte{0x40, 0x40},
		repeatByte(0xc1, 64),
		[]byte{0xf0, 0x01},
		[]byte{0x00},
	)
}

// extensionGoldenGroupContext is the struct that second golden describes.
func extensionGoldenGroupContext() *GroupContext {
	return &GroupContext{
		Version:                 ProtocolVersion(0xfafb),
		CipherSuite:             CipherSuiteX25519ChaCha20Sha256Ed25519,
		GroupId:                 []byte{0xa1, 0xa2, 0xa3},
		Epoch:                   0xfedcba9876543210,
		TreeHash:                []byte{0xb1},
		ConfirmedTranscriptHash: []byte{},
		Extensions: []Extension{
			{ExtensionType: ExtensionTypeRatchetTree, ExtensionData: repeatByte(0xc1, 64)},
			{ExtensionType: ExtensionTypeUrmessageGroupPolicy, ExtensionData: []byte{}},
		},
	}
}

// TestGroupContextHandDerivedGoldenMatchesTheUpstreamVector checks the hand derivation
// against another implementation's bytes before anything is asserted against it. Both
// are statements of the encoding written without reference to this package — one here
// from the RFC, one by the mlswg reference implementation — and agreeing is what makes
// either worth quoting.
func TestGroupContextHandDerivedGoldenMatchesTheUpstreamVector(t *testing.T) {
	derived := handDerivedEpoch0GroupContext(t)
	if len(derived) != 112 {
		t.Fatalf("the hand derivation is %d bytes, the arithmetic in its comment says 112", len(derived))
	}
	upstream := upstreamGroupContext(t, 0x0003, 0)
	if !bytes.Equal(derived, upstream) {
		t.Fatalf("hand derived golden =\n %x\nupstream key-schedule.json =\n %x", derived, upstream)
	}
}

// TestGroupContextMarshalMatchesTheHandDerivedGolden is the field order and varint
// prefix pin. A reordered field, a length prefix written at the record layer's fixed 32
// bit width instead of MLS's varint, or an enum written at the wrong width all change
// every epoch secret derived from this context, and this is the cheapest place to see it.
func TestGroupContextMarshalMatchesTheHandDerivedGolden(t *testing.T) {
	encoded, err := syntax.Marshal(ksVectorEpoch0GroupContext(t))
	if err != nil {
		t.Fatalf("syntax.Marshal: %v", err)
	}
	want := handDerivedEpoch0GroupContext(t)
	if len(encoded) != len(want) {
		t.Fatalf("encoded %d bytes, want %d", len(encoded), len(want))
	}
	if !bytes.Equal(encoded, want) {
		t.Fatalf("syntax.Marshal =\n %x\nwant\n %x", encoded, want)
	}
}

// TestGroupContextMarshalMatchesTheExtensionGolden pins what the published vectors
// cannot: the extension vector's own byte length prefix, an extension body long enough
// to need the two octet varint, a present but empty opaque field, an unregistered
// version code point carried rather than normalised, and an epoch with every octet
// distinct.
func TestGroupContextMarshalMatchesTheExtensionGolden(t *testing.T) {
	want := handDerivedExtensionGolden()
	if len(want) != 92 {
		t.Fatalf("the hand derivation is %d bytes, the arithmetic in its comment says 92", len(want))
	}
	encoded, err := syntax.Marshal(extensionGoldenGroupContext())
	if err != nil {
		t.Fatalf("syntax.Marshal: %v", err)
	}
	if !bytes.Equal(encoded, want) {
		t.Fatalf("syntax.Marshal =\n %x\nwant\n %x", encoded, want)
	}
}

// TestGroupContextMarshalMatchesEveryUpstreamKeyScheduleVector widens the golden from
// one vector to every group context the pinned corpus publishes. The seven suites carry
// 32, 48 and 64 byte hashes, so the one and two octet varint widths are both covered by
// an independent implementation's bytes rather than only by this file's arithmetic, and
// the five epochs of each move the uint64's low octet.
func TestGroupContextMarshalMatchesEveryUpstreamKeyScheduleVector(t *testing.T) {
	checked := 0
	for _, entry := range loadKeyScheduleVectors(t) {
		for epoch, published := range entry.Epochs {
			context := &GroupContext{
				Version:                 ProtocolVersionMls10,
				CipherSuite:             CipherSuite(entry.CipherSuite),
				GroupId:                 groupContextHex(t, "group_id", entry.GroupId),
				Epoch:                   uint64(epoch),
				TreeHash:                groupContextHex(t, "tree_hash", published.TreeHash),
				ConfirmedTranscriptHash: groupContextHex(t, "confirmed_transcript_hash", published.ConfirmedTranscriptHash),
				Extensions:              nil,
			}
			encoded, err := syntax.Marshal(context)
			if err != nil {
				t.Fatalf("suite %d epoch %d: syntax.Marshal: %v", entry.CipherSuite, epoch, err)
			}
			want := groupContextHex(t, "group_context", published.GroupContext)
			if !bytes.Equal(encoded, want) {
				t.Errorf("suite %d epoch %d:\n got %x\nwant %x", entry.CipherSuite, epoch, encoded, want)
			}
			checked++
		}
	}
	if checked < 7 {
		t.Fatalf("only %d published group contexts were checked, fewer than the corpus holds suites; the loop read nothing useful", checked)
	}
	t.Logf("%d published group contexts re-encoded byte exact", checked)
}

// TestGroupContextMarshalEmptyExtensionsIsOneZeroOctet asserts an empty extension vector
// encodes as a present length prefix of zero rather than as an omitted field. The two
// differ by exactly one byte at the end of the structure, and that byte is one a peer
// hashes.
func TestGroupContextMarshalEmptyExtensionsIsOneZeroOctet(t *testing.T) {
	for name, context := range map[string]*GroupContext{
		"nil extensions":   ksVectorEpoch0GroupContext(t),
		"empty extensions": withExtensions(ksVectorEpoch0GroupContext(t), []Extension{}),
	} {
		encoded, err := syntax.Marshal(context)
		if err != nil {
			t.Fatalf("%s: syntax.Marshal: %v", name, err)
		}
		if len(encoded) != 112 {
			t.Fatalf("%s: encoded %d bytes, want 112", name, len(encoded))
		}
		if encoded[len(encoded)-1] != 0x00 {
			t.Fatalf("%s: last byte = %#x, want 0x00", name, encoded[len(encoded)-1])
		}
	}
}

// withExtensions is the one field mutation the test above needs, kept out of the literal
// so both cases are visibly the same context.
func withExtensions(context *GroupContext, extensions []Extension) *GroupContext {
	context.Extensions = extensions
	return context
}

// TestGroupContextWriteIntoASharedWriter asserts MarshalMLS appends into a writer the
// caller already owns and adds no framing of its own. GroupInfo and every p6 preimage
// inline the group context this way, so a stray length prefix here would be invisible to
// a test that only ever encoded the context standalone, and fatal to every signature.
func TestGroupContextWriteIntoASharedWriter(t *testing.T) {
	w := syntax.NewWriter()
	w.WriteUint8(0xff)
	if err := ksVectorEpoch0GroupContext(t).MarshalMLS(w); err != nil {
		t.Fatalf("MarshalMLS: %v", err)
	}
	w.WriteUint8(0xee)
	encoded, err := w.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	if encoded[0] != 0xff {
		t.Fatalf("the caller's leading byte was overwritten: %#x", encoded[0])
	}
	if encoded[len(encoded)-1] != 0xee {
		t.Fatalf("the caller's trailing byte was overwritten: %#x", encoded[len(encoded)-1])
	}
	inline := encoded[1 : len(encoded)-1]
	if !bytes.Equal(inline, handDerivedEpoch0GroupContext(t)) {
		t.Fatalf("inline encoding =\n %x\nwant\n %x", inline, handDerivedEpoch0GroupContext(t))
	}
}

// ---------------------------------------------------------------------------
// decode
// ---------------------------------------------------------------------------

// TestGroupContextDecodeTakesEveryFieldFromItsOwnBytes is the decode half of the field
// order pin, and it is not the round trip test written twice. Round trip cannot see two
// adjacent fields swapped in both directions at once, which is exactly how two
// implementations come to disagree; this reads the hand derived golden and names the
// value every field must hold, so a decoder that took the tree hash out of the
// transcript hash's bytes fails here whatever its encoder does.
func TestGroupContextDecodeTakesEveryFieldFromItsOwnBytes(t *testing.T) {
	parsed := &GroupContext{}
	if err := syntax.Unmarshal(handDerivedExtensionGolden(), parsed); err != nil {
		t.Fatalf("syntax.Unmarshal: %v", err)
	}
	want := extensionGoldenGroupContext()
	if parsed.Version != want.Version {
		t.Errorf("Version = %#04x, want %#04x", uint16(parsed.Version), uint16(want.Version))
	}
	if parsed.CipherSuite != want.CipherSuite {
		t.Errorf("CipherSuite = %#04x, want %#04x", uint16(parsed.CipherSuite), uint16(want.CipherSuite))
	}
	if !bytes.Equal(parsed.GroupId, want.GroupId) {
		t.Errorf("GroupId = %x, want %x", parsed.GroupId, want.GroupId)
	}
	if parsed.Epoch != want.Epoch {
		t.Errorf("Epoch = %#016x, want %#016x", parsed.Epoch, want.Epoch)
	}
	if !bytes.Equal(parsed.TreeHash, want.TreeHash) {
		t.Errorf("TreeHash = %x, want %x", parsed.TreeHash, want.TreeHash)
	}
	if !bytes.Equal(parsed.ConfirmedTranscriptHash, want.ConfirmedTranscriptHash) {
		t.Errorf("ConfirmedTranscriptHash = %x, want %x", parsed.ConfirmedTranscriptHash, want.ConfirmedTranscriptHash)
	}
	if len(parsed.Extensions) != len(want.Extensions) {
		t.Fatalf("Extensions holds %d entries, want %d", len(parsed.Extensions), len(want.Extensions))
	}
	for i := range want.Extensions {
		if parsed.Extensions[i].ExtensionType != want.Extensions[i].ExtensionType {
			t.Errorf("Extensions[%d].ExtensionType = %#04x, want %#04x",
				i, uint16(parsed.Extensions[i].ExtensionType), uint16(want.Extensions[i].ExtensionType))
		}
		if !bytes.Equal(parsed.Extensions[i].ExtensionData, want.Extensions[i].ExtensionData) {
			t.Errorf("Extensions[%d].ExtensionData = %x, want %x",
				i, parsed.Extensions[i].ExtensionData, want.Extensions[i].ExtensionData)
		}
	}
}

// TestGroupContextDecodeReadsTheVectorsFieldsBackFromTheirOwnOffsets does the same
// against the upstream bytes, at an epoch other than zero, where the tree hash and the
// confirmed transcript hash are two adjacent opaque fields of identical length — the
// pair a swap is invisible in unless something names which is which. The values come
// from the corpus's own named fields rather than from the group_context blob they were
// concatenated into, so this reads a decode against a source that never saw an offset.
func TestGroupContextDecodeReadsTheVectorsFieldsBackFromTheirOwnOffsets(t *testing.T) {
	const suite, epoch = uint16(0x0003), 3
	entry := upstreamEntry(t, suite)
	published := entry.Epochs[epoch]

	parsed := &GroupContext{}
	if err := syntax.Unmarshal(groupContextHex(t, "group_context", published.GroupContext), parsed); err != nil {
		t.Fatalf("syntax.Unmarshal: %v", err)
	}
	if parsed.Version != ProtocolVersionMls10 {
		t.Errorf("Version = %#04x, want %#04x", uint16(parsed.Version), uint16(ProtocolVersionMls10))
	}
	if parsed.CipherSuite != CipherSuite(suite) {
		t.Errorf("CipherSuite = %#04x, want %#04x", uint16(parsed.CipherSuite), suite)
	}
	if parsed.Epoch != epoch {
		t.Errorf("Epoch = %d, want %d", parsed.Epoch, epoch)
	}
	if got := hex.EncodeToString(parsed.GroupId); got != entry.GroupId {
		t.Errorf("GroupId = %s, want %s", got, entry.GroupId)
	}
	if got := hex.EncodeToString(parsed.TreeHash); got != published.TreeHash {
		t.Errorf("TreeHash = %s, want %s", got, published.TreeHash)
	}
	if got := hex.EncodeToString(parsed.ConfirmedTranscriptHash); got != published.ConfirmedTranscriptHash {
		t.Errorf("ConfirmedTranscriptHash = %s, want %s", got, published.ConfirmedTranscriptHash)
	}
	if published.TreeHash == published.ConfirmedTranscriptHash {
		t.Fatal("this vector's tree hash and confirmed transcript hash are the same string, so swapping the two fields would be invisible here")
	}
	if len(parsed.Extensions) != 0 {
		t.Errorf("Extensions holds %d entries, want 0", len(parsed.Extensions))
	}
}

// TestGroupContextUnmarshalLeavesTheTailAlone asserts UnmarshalMLS consumes exactly its
// own fields, which is what lets p6 and p7 decode a group context inline out of a
// GroupInfo. A decoder that ate the tail would take the confirmation tag with it.
func TestGroupContextUnmarshalLeavesTheTailAlone(t *testing.T) {
	data := append(handDerivedEpoch0GroupContext(t), 0xde, 0xad)
	r := syntax.NewReader(data)
	parsed := &GroupContext{}
	if err := parsed.UnmarshalMLS(r); err != nil {
		t.Fatalf("UnmarshalMLS: %v", err)
	}
	if remaining := r.Remaining(); remaining != 2 {
		t.Fatalf("%d bytes left after the context, want 2", remaining)
	}
	tail, err := r.ReadRaw(2)
	if err != nil {
		t.Fatalf("ReadRaw: %v", err)
	}
	if !bytes.Equal(tail, []byte{0xde, 0xad}) {
		t.Fatalf("tail = %x, want dead", tail)
	}
	if err := r.Done(); err != nil {
		t.Fatalf("Done: %v", err)
	}
}

// TestGroupContextRejectsTrailingBytes asserts the full consumption rule at top level.
// The error matches both syntax.ErrTrailingBytes and this plan's
// ErrGroupContextTrailingBytes, which wraps it, so neither caller has to know which
// layer refused. A decoder tolerating a tail accepts two encodings of one object, and a
// signature covers only one of them.
func TestGroupContextRejectsTrailingBytes(t *testing.T) {
	if !errors.Is(ErrGroupContextTrailingBytes, syntax.ErrTrailingBytes) {
		t.Fatal("ErrGroupContextTrailingBytes no longer names the same condition as syntax.ErrTrailingBytes")
	}
	for name, base := range map[string][]byte{
		"vector golden":    handDerivedEpoch0GroupContext(t),
		"extension golden": handDerivedExtensionGolden(),
	} {
		for _, tail := range [][]byte{{0x00}, {0xff}, {0x00, 0x00}, repeatByte(0x5a, 17)} {
			data := joinBytes(base, tail)
			err := syntax.Unmarshal(data, &GroupContext{})
			if !errors.Is(err, syntax.ErrTrailingBytes) {
				t.Errorf("%s with %d trailing bytes: err = %v, want syntax.ErrTrailingBytes", name, len(tail), err)
			}
		}
	}
}

// goldenGroupContextEncodings is the set of valid encodings the refusal properties below are
// stated over. Both hand derivations are in it, so the extension vector and the present but
// empty opaque field are covered, and two upstream vectors carrying 64 and 48 byte hashes put
// both varint widths in the set as another implementation wrote them.
func goldenGroupContextEncodings(t *testing.T) map[string][]byte {
	t.Helper()
	return map[string][]byte{
		"vector golden":    handDerivedEpoch0GroupContext(t),
		"extension golden": handDerivedExtensionGolden(),
		"upstream 64 byte": upstreamGroupContext(t, 0x0004, 0),
		"upstream 48 byte": upstreamGroupContext(t, 0x0007, 2),
	}
}

// TestGroupContextRejectsEverySingleByteTruncation asserts every proper prefix of a
// valid encoding is refused rather than yielding a partly populated struct. The set of
// prefixes is derived from the encoding's own length, so a field added to the structure
// widens this test without anybody editing it.
func TestGroupContextRejectsEverySingleByteTruncation(t *testing.T) {
	for name, full := range goldenGroupContextEncodings(t) {
		refused := 0
		for n := 0; n < len(full); n++ {
			parsed := &GroupContext{}
			if err := syntax.Unmarshal(full[:n], parsed); err == nil {
				t.Errorf("%s: the %d byte prefix parsed into %s, want an error", name, n, describeGroupContext(parsed))
				continue
			}
			refused++
		}
		if refused != len(full) {
			t.Errorf("%s: %d of %d prefixes refused", name, refused, len(full))
		}
		if err := syntax.Unmarshal(full, &GroupContext{}); err != nil {
			t.Errorf("%s: the untruncated encoding was refused too (%v), so the loop above proves nothing", name, err)
		}
	}
}

// sentinelGroupContext is a receiver whose every field holds a value no golden holds, so an
// assignment made before a decode failed is visible whatever it assigned. A zeroed receiver
// would show an early assignment only when the input's value for that field happens not to be
// zero, which makes the observation depend on the fixture rather than on the code.
func sentinelGroupContext() *GroupContext {
	return &GroupContext{
		Version:                 ProtocolVersion(0x1234),
		CipherSuite:             CipherSuite(0x5678),
		GroupId:                 []byte{0x9a, 0x9b},
		Epoch:                   0x0123456789abcdef,
		TreeHash:                []byte{0x9c},
		ConfirmedTranscriptHash: []byte{0x9d, 0x9e, 0x9f},
		Extensions:              []Extension{{ExtensionType: ExtensionType(0x4321), ExtensionData: []byte{0xa0}}},
	}
}

// TestGroupContextUnmarshalAssignsNothingWhenItRefuses states the all or nothing rule
// UnmarshalMLS documents: a decode that fails leaves the caller's struct exactly as it was
// handed over.
//
// The rule earns a test because the struct a decode writes into is not always fresh. Under C1
// the byte level entry point is syntax.Unmarshal(bs, gc) into a value the caller owns, which
// invites reuse across epochs, and a decoder that assigned as it read would leave a refused
// decode holding some fields from the new input and the rest from the old one. That composite
// is not a mangled struct anybody would notice: it is a well formed GroupContext describing a
// group binding that never existed, and re-encoding it produces bytes some peer will hash.
//
// Two families of refusable input are swept, both derived. Every proper prefix of every
// golden fails somewhere in the field sequence, which reaches the assignments that happen
// before the failing read. Every single byte change to the two hand derived goldens reaches
// what a truncation cannot — a length prefix that overruns its region, a vector whose entries
// do not fill it — and those fail after all six leading fields have been read successfully.
//
// UnmarshalMLS is called directly rather than through syntax.Unmarshal because the trailing
// byte refusal belongs to the outer layer and happens after every field has legitimately been
// assigned, so routing through it would count a correct decode as a refusal.
func TestGroupContextUnmarshalAssignsNothingWhenItRefuses(t *testing.T) {
	inputs := map[string][][]byte{}
	for name, full := range goldenGroupContextEncodings(t) {
		prefixes := [][]byte{}
		for n := 0; n < len(full); n++ {
			prefixes = append(prefixes, full[:n])
		}
		inputs[name+" truncated"] = prefixes
	}
	for name, full := range map[string][]byte{
		"vector golden":    handDerivedEpoch0GroupContext(t),
		"extension golden": handDerivedExtensionGolden(),
	} {
		corruptions := [][]byte{}
		for position := range full {
			for delta := 1; delta < 256; delta++ {
				corrupted := append([]byte{}, full...)
				corrupted[position] = byte((int(full[position]) + delta) % 256)
				corruptions = append(corruptions, corrupted)
			}
		}
		inputs[name+" corrupted"] = corruptions
	}

	refused, accepted := 0, 0
	for name, family := range inputs {
		familyRefused := 0
		for _, input := range family {
			untouched := sentinelGroupContext()
			parsed := sentinelGroupContext()
			if err := parsed.UnmarshalMLS(syntax.NewReader(input)); err == nil {
				accepted++
				continue
			}
			familyRefused++
			if !reflect.DeepEqual(parsed, untouched) {
				t.Fatalf("%s: a refused %d byte input changed the caller's context\n from %#v\n to   %#v",
					name, len(input), untouched, parsed)
			}
		}
		if familyRefused == 0 {
			t.Errorf("%s: none of its %d inputs was refused, so this family evaluated the property zero times",
				name, len(family))
		}
		refused += familyRefused
	}
	if accepted == 0 {
		t.Fatal("every input was refused, so the corruption sweep produced nothing decodable and is not the sweep it claims to be")
	}
	t.Logf("%d refused inputs left the caller's context untouched, %d were accepted", refused, accepted)
}

// TestGroupContextEitherRefusesOrExactlyReproducesEverySingleByteCorruption is the
// canonicality property, stated over every one byte change to a valid encoding: the
// decoder may refuse, and if it accepts then re-encoding must give back the corrupted
// bytes exactly. Accepted-and-silently-changed is the outcome that is forbidden, because
// it is a second encoding of a structure whose first encoding somebody signed.
//
// Both outcomes are counted and both are required to occur. A decoder that refused
// everything would satisfy the property vacuously, and so would a corpus that never
// reached the accepting branch.
func TestGroupContextEitherRefusesOrExactlyReproducesEverySingleByteCorruption(t *testing.T) {
	for name, full := range map[string][]byte{
		"vector golden":    handDerivedEpoch0GroupContext(t),
		"extension golden": handDerivedExtensionGolden(),
	} {
		accepted, refused := 0, 0
		for position := range full {
			for delta := 1; delta < 256; delta++ {
				corrupted := append([]byte{}, full...)
				corrupted[position] = byte((int(full[position]) + delta) % 256)
				parsed := &GroupContext{}
				if err := syntax.Unmarshal(corrupted, parsed); err != nil {
					refused++
					continue
				}
				accepted++
				reencoded, err := syntax.Marshal(parsed)
				if err != nil {
					t.Fatalf("%s: byte %d changed to %#02x decoded but would not re-encode: %v",
						name, position, corrupted[position], err)
				}
				if !bytes.Equal(reencoded, corrupted) {
					t.Fatalf("%s: byte %d changed to %#02x was accepted and changed:\n got %x\nwant %x",
						name, position, corrupted[position], reencoded, corrupted)
				}
			}
		}
		if accepted == 0 {
			t.Errorf("%s: every one of the %d corruptions was refused, so the re-encoding half of this property was never evaluated", name, refused)
		}
		if refused == 0 {
			t.Errorf("%s: every one of the %d corruptions was accepted, which cannot be right for a length prefixed format", name, accepted)
		}
		t.Logf("%s: %d corruptions accepted and reproduced byte exact, %d refused", name, accepted, refused)
	}
}

// ---------------------------------------------------------------------------
// the generated round trip corpus
// ---------------------------------------------------------------------------

// The registry constant derivation.
//
// The corpus below is built from what this returns rather than from a list typed out beside
// it, because a hand written list of a registry's members is a list that is already one member
// short the day somebody adds one — and the member it is short of is the one nothing has ever
// encoded. Deriving it means a new ExtensionType constant enters the round trip corpus in the
// commit that declares it.
//
// That claim holds only if the derivation reads every legal way to declare one, and the first
// version of it did not. It walked the syntax tree and took a constant only when the spec
// carried the type name and the value was a bare integer literal; every other shape hit a
// silent continue. An ExtensionType written as 1 << 4, one written as an offset from another
// code point, an iota block, and a spec that inherits its type from the line above are all
// ordinary Go, all four declare a real registry member, and all four were dropped without a
// word — a derived gate that had quietly become an enumeration of one spelling, which is rule
// 5's failure with extra steps. So the value comes from go/types now, which is the thing that
// decides what a constant is, and the class is every constant the package declares rather than
// every constant somebody thought to spell simply.

var (
	packageTypeCheckOnce  sync.Once
	packageTypeChecked    *types.Package
	packageTypeCheckFiles int
	packageTypeCheckErr   error
)

// typeCheckedPackage type checks this package's production source, once per process. The check
// costs a second or two and several tests read registries out of it, so the memo is what keeps
// the derivation cheap enough to be used everywhere it belongs.
func typeCheckedPackage(t *testing.T) *types.Package {
	t.Helper()
	packageTypeCheckOnce.Do(func() {
		packageTypeChecked, packageTypeCheckFiles, packageTypeCheckErr =
			checkPackageSource(".", "github.com/urnetwork/connect/mls")
	})
	if packageTypeCheckErr != nil {
		t.Fatalf("type check this package's production source: %v", packageTypeCheckErr)
	}
	if packageTypeCheckFiles == 0 {
		t.Fatal("no production go file was read from the package directory, so this derivation proves nothing")
	}
	return packageTypeChecked
}

// checkPackageSource parses every production file of one directory and type checks them
// together.
//
// A check error is returned rather than tolerated. go/types hands back a partial package when
// it can, and a partial package is a partial scope, which is a derivation that silently
// enumerates less than the source declares — the failure this whole approach exists to remove,
// reintroduced one layer down.
func checkPackageSource(directory string, packagePath string) (*types.Package, int, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, 0, fmt.Errorf("read %s: %w", directory, err)
	}
	fileSet := token.NewFileSet()
	files := []*ast.File{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fileSet, filepath.Join(directory, name), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, 0, fmt.Errorf("parse %s: %w", name, err)
		}
		files = append(files, parsed)
	}
	config := types.Config{
		// from source rather than from the compiler's export data, so the check reads the tree
		// as it is on disk and does not need the package to have been installed.
		Importer: importer.ForCompiler(fileSet, "source", nil),
		// no constant is declared inside a function body, and the bodies are most of what the
		// check would otherwise cost.
		IgnoreFuncBodies: true,
	}
	checked, err := config.Check(packagePath, fileSet, files, nil)
	if err != nil {
		return nil, len(files), err
	}
	return checked, len(files), nil
}

// namedTypeConstants returns the value of every package level constant in a checked package
// whose declared type is the named one. It is split from registryConstantsOfType so the
// completeness control below can run the same reader over a package written to hold the
// awkward spellings — which cannot be added to production source, since a probe constant in
// extension.go is a code point in a live registry and this file's own corpus would encode it.
func namedTypeConstants(t *testing.T, pkg *types.Package, typeName string) map[string]uint64 {
	t.Helper()
	found := map[string]uint64{}
	scope := pkg.Scope()
	for _, name := range scope.Names() {
		declared, isConstant := scope.Lookup(name).(*types.Const)
		if !isConstant {
			continue
		}
		named, isNamed := types.Unalias(declared.Type()).(*types.Named)
		if !isNamed || named.Obj().Name() != typeName || named.Obj().Pkg() != pkg {
			continue
		}
		value, exact := constant.Uint64Val(constant.ToInt(declared.Val()))
		if !exact {
			t.Fatalf("%s = %s is not a uint64, so the registry it belongs to cannot be enumerated as code points",
				name, declared.Val())
		}
		found[name] = value
	}
	return found
}

// registryConstantsOfType is what the corpus generator calls. An empty result is fatal here
// rather than at the call site: the generators append their own boundary values to whatever
// this returns, so a derivation that found nothing would shrink the corpus to those two values
// and report green having encoded no registered code point at all.
func registryConstantsOfType(t *testing.T, typeName string) map[string]uint64 {
	t.Helper()
	found := namedTypeConstants(t, typeCheckedPackage(t), typeName)
	if len(found) == 0 {
		t.Fatalf("the derivation found no %s constant, so any corpus built from it holds only its hardcoded boundary values", typeName)
	}
	return found
}

// TestTheRegistryConstantDerivationReadsTheSource is the positive control on the derivation
// above, run against the real package. A scan that read nothing returns an empty map, and an
// empty map silently shrinks the corpus to whatever boundary values are hardcoded beside it —
// a derived gate reporting green having derived nothing.
func TestTheRegistryConstantDerivationReadsTheSource(t *testing.T) {
	for typeName, mustHold := range map[string]map[string]uint64{
		"ProtocolVersion": {"ProtocolVersionMls10": 0x0001},
		"CipherSuite": {
			"CipherSuiteX25519AesGcm128Sha256Ed25519": 0x0001,
			"CipherSuiteX25519ChaCha20Sha256Ed25519":  0x0003,
		},
		"ExtensionType": {
			"ExtensionTypeRatchetTree":             0x0002,
			"ExtensionTypeUrmessageOwnerSuccessor": 0xF003,
		},
	} {
		derived := registryConstantsOfType(t, typeName)
		for name, want := range mustHold {
			got, ok := derived[name]
			if !ok {
				t.Errorf("the derivation did not find %s, which %s certainly declares", name, typeName)
				continue
			}
			if got != want {
				t.Errorf("%s = %#x, want %#x", name, got, want)
			}
		}
		t.Logf("%s: %d constants derived", typeName, len(derived))
	}
	if found := namedTypeConstants(t, typeCheckedPackage(t), "ThisTypeDoesNotExistAnywhereInPackageMls"); len(found) != 0 {
		t.Fatalf("the derivation reported %d constants of a type that cannot exist, so it is matching text rather than declarations", len(found))
	}
}

// TestTheRegistryConstantDerivationReadsEveryLegalConstantForm is the control the test above
// cannot be. A list of names known to be declared separates "found nothing" from "found
// something" and nothing more: it cannot tell six of six from six of ten, which is exactly the
// shortfall a value reader restricted to bare literals produces. This states the completeness
// half instead, over a throwaway package whose registry members are written in every form the
// language allows.
func TestTheRegistryConstantDerivationReadsEveryLegalConstantForm(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "probe.go"), []byte(probeRegistrySource), 0o600); err != nil {
		t.Fatalf("write the probe package: %v", err)
	}
	checked, read, err := checkPackageSource(directory, "probe")
	if err != nil {
		t.Fatalf("type check the probe package: %v", err)
	}
	if read != 1 {
		t.Fatalf("the probe package read %d files, want 1", read)
	}
	want := map[string]uint64{
		"ProbeLiteral":   0x0002,
		"ProbeShifted":   0x0010,
		"ProbeOffset":    0x0066,
		"ProbeParen":     0x000f,
		"ProbeConverted": 0x0700,
		"ProbeIota":      0x0800,
		"ProbeIotaNext":  0x0801,
		"ProbeIotaLast":  0x0802,
	}
	got := namedTypeConstants(t, checked, "ProbeRegistry")
	if !maps.Equal(got, want) {
		t.Fatalf("the derivation read\n %v\nwant\n %v", got, want)
	}
}

// A registry whose members are declared in every form the language permits: a bare literal, a
// shift, an offset from another member, a parenthesised expression, a conversion on a spec
// carrying no type, and an iota block whose second and third members carry neither a type nor
// a value of their own. The syntax tree scan this replaced read one of those six spellings and
// dropped the other five in silence.
const probeRegistrySource = `package probe

type ProbeRegistry uint16

const ProbeLiteral ProbeRegistry = 0x0002

const (
	ProbeShifted   ProbeRegistry = 1 << 4
	ProbeOffset    ProbeRegistry = ProbeLiteral + 100
	ProbeParen     ProbeRegistry = (3 * 5)
	ProbeConverted               = ProbeRegistry(0x0700)
)

const (
	ProbeIota ProbeRegistry = iota + 0x0800
	ProbeIotaNext
	ProbeIotaLast
)

// a constant of another type entirely, so a filter keying on the name rather than on the
// declared type fails here
const ProbeNotInTheRegistry uint16 = 0xdead
`

// sortedValues returns a derived constant set as a stable, deduplicated value list.
func sortedValues(constants map[string]uint64) []uint64 {
	values := map[uint64]bool{}
	for _, value := range constants {
		values[value] = true
	}
	return slices.Sorted(maps.Keys(values))
}

// generatedGroupContexts is the round trip corpus: the cross product of the registry
// enums with the varint width boundaries of the opaque fields, the uint64 boundaries of
// the epoch, and the extension vector shapes.
//
// It is generated rather than written out case by case for the reason rule 5 names: a
// hand picked table is a claim about which cases matter, made by whoever also wrote the
// code, and on this project that claim has understated the real class fourteen times.
// The axes here are the ones the encoding actually branches on — the varint width, the
// empty field, the enum value, the vector arity — so the product covers every
// combination of them rather than the combinations somebody thought of.
func generatedGroupContexts(t *testing.T) []*GroupContext {
	t.Helper()

	versions := append(sortedValues(registryConstantsOfType(t, "ProtocolVersion")), 0x0000, 0xffff)
	suites := append(sortedValues(registryConstantsOfType(t, "CipherSuite")), 0x0000, 0xffff)
	extensionTypes := append(sortedValues(registryConstantsOfType(t, "ExtensionType")), 0x0000, 0xffff)

	// the opaque field lengths the varint branches on: absent, empty, one octet, the
	// last length a one octet prefix can express, the first that needs two, and one well
	// inside the two octet range.
	opaques := [][]byte{
		nil,
		{},
		repeatByte(0x11, 1),
		repeatByte(0x22, 63),
		repeatByte(0x33, 64),
		repeatByte(0x44, 255),
	}

	// the uint64 boundaries, which are the values a narrower field or a sign would move.
	epochs := []uint64{
		0,
		1,
		math.MaxUint8,
		math.MaxUint16,
		math.MaxUint32 - 1,
		math.MaxUint32,
		uint64(math.MaxUint32) + 1,
		1 << 63,
		math.MaxUint64,
	}

	// the extension vector arities, plus one entry per derived registry code point.
	extensionLists := [][]Extension{nil, {}}
	for _, extensionType := range extensionTypes {
		extensionLists = append(extensionLists, []Extension{
			{ExtensionType: ExtensionType(extensionType), ExtensionData: nil},
		})
	}
	extensionLists = append(extensionLists,
		[]Extension{{ExtensionType: ExtensionTypeRatchetTree, ExtensionData: repeatByte(0x55, 63)}},
		[]Extension{{ExtensionType: ExtensionTypeRatchetTree, ExtensionData: repeatByte(0x66, 64)}},
		[]Extension{
			{ExtensionType: ExtensionTypeRatchetTree, ExtensionData: repeatByte(0x77, 3)},
			{ExtensionType: ExtensionTypeRatchetTree, ExtensionData: repeatByte(0x88, 3)},
			{ExtensionType: ExtensionTypeExternalSenders, ExtensionData: nil},
		},
	)

	corpus := []*GroupContext{}
	for _, version := range versions {
		for _, suite := range suites {
			for opaqueIndex, groupId := range opaques {
				for epochIndex, epoch := range epochs {
					for listIndex, extensions := range extensionLists {
						// the three opaque fields rotate through one axis rather than
						// being crossed with each other, which would cube the corpus to
						// buy nothing: they go through the same encoder, so a length
						// that works in one position works in all three. Rotating puts
						// every length in every position across the product.
						corpus = append(corpus, &GroupContext{
							Version:                 ProtocolVersion(version),
							CipherSuite:             CipherSuite(suite),
							GroupId:                 groupId,
							Epoch:                   epoch,
							TreeHash:                opaques[(opaqueIndex+epochIndex)%len(opaques)],
							ConfirmedTranscriptHash: opaques[(opaqueIndex+listIndex)%len(opaques)],
							Extensions:              extensions,
						})
					}
				}
			}
		}
	}
	return corpus
}

// groupContextsAgree compares two contexts field by field, treating a nil byte slice and
// an empty one as equal because the wire format has one spelling for both and the
// decoder always produces the non nil form.
func groupContextsAgree(left *GroupContext, right *GroupContext) bool {
	if left.Version != right.Version || left.CipherSuite != right.CipherSuite || left.Epoch != right.Epoch {
		return false
	}
	if !bytes.Equal(left.GroupId, right.GroupId) ||
		!bytes.Equal(left.TreeHash, right.TreeHash) ||
		!bytes.Equal(left.ConfirmedTranscriptHash, right.ConfirmedTranscriptHash) {
		return false
	}
	if len(left.Extensions) != len(right.Extensions) {
		return false
	}
	for i := range left.Extensions {
		if left.Extensions[i].ExtensionType != right.Extensions[i].ExtensionType {
			return false
		}
		if !bytes.Equal(left.Extensions[i].ExtensionData, right.Extensions[i].ExtensionData) {
			return false
		}
	}
	return true
}

// TestGroupContextRoundTripsByteExactOverTheGeneratedCorpus asserts, for every generated
// context, that it encodes, that decoding recovers it, and that re-encoding reproduces
// the bytes exactly. syntax.CheckRoundTrip is called on the same input because every
// later fuzz target reaches this codec through it, so a codec that satisfied the
// property here and not there would leave those targets green and empty.
func TestGroupContextRoundTripsByteExactOverTheGeneratedCorpus(t *testing.T) {
	corpus := generatedGroupContexts(t)
	if len(corpus) < 1000 {
		t.Fatalf("the generated corpus holds %d contexts, far fewer than the product of its axes; the generator produced almost nothing", len(corpus))
	}
	for index, context := range corpus {
		encoded, err := syntax.Marshal(context)
		if err != nil {
			t.Fatalf("case %d %s: syntax.Marshal: %v", index, describeGroupContext(context), err)
		}
		parsed := &GroupContext{}
		if err := syntax.Unmarshal(encoded, parsed); err != nil {
			t.Fatalf("case %d %s: syntax.Unmarshal: %v", index, describeGroupContext(context), err)
		}
		if !groupContextsAgree(context, parsed) {
			t.Fatalf("case %d %s: decoded to %s", index, describeGroupContext(context), describeGroupContext(parsed))
		}
		reencoded, err := syntax.Marshal(parsed)
		if err != nil {
			t.Fatalf("case %d %s: re-encode: %v", index, describeGroupContext(context), err)
		}
		if !bytes.Equal(reencoded, encoded) {
			t.Fatalf("case %d %s: round trip =\n %x\nwant\n %x", index, describeGroupContext(context), reencoded, encoded)
		}
		if err := syntax.CheckRoundTrip[GroupContext, *GroupContext](encoded); err != nil {
			t.Fatalf("case %d %s: CheckRoundTrip: %v", index, describeGroupContext(context), err)
		}
	}
	t.Logf("%d generated contexts round tripped byte exact", len(corpus))
}

// describeGroupContext names a corpus case in a failure message, since the case is
// generated and there is no table row to point at.
func describeGroupContext(context *GroupContext) string {
	sizes := make([]string, 0, len(context.Extensions))
	for _, extension := range context.Extensions {
		sizes = append(sizes, fmt.Sprintf("%#04x/%d", uint16(extension.ExtensionType), len(extension.ExtensionData)))
	}
	return fmt.Sprintf("{version %#04x suite %#04x gid %d epoch %#016x tree %d cth %d exts[%s]}",
		uint16(context.Version), uint16(context.CipherSuite), len(context.GroupId), context.Epoch,
		len(context.TreeHash), len(context.ConfirmedTranscriptHash), strings.Join(sizes, " "))
}

// TestGroupContextRoundTripsAcrossTheFourOctetVarintBoundary covers the lengths the
// corpus above leaves out because carrying them through a cross product would cost
// megabytes per case: the last length the two octet prefix can express and the first
// that needs the four octet one.
func TestGroupContextRoundTripsAcrossTheFourOctetVarintBoundary(t *testing.T) {
	for _, length := range []int{16383, 16384, 16385} {
		context := &GroupContext{
			Version:                 ProtocolVersionMls10,
			CipherSuite:             CipherSuiteX25519ChaCha20Sha256Ed25519,
			GroupId:                 repeatByte(0x99, length),
			Epoch:                   7,
			TreeHash:                repeatByte(0xaa, 32),
			ConfirmedTranscriptHash: repeatByte(0xbb, 32),
			Extensions: []Extension{
				{ExtensionType: ExtensionTypeRatchetTree, ExtensionData: repeatByte(0xcc, length)},
			},
		}
		encoded, err := syntax.Marshal(context)
		if err != nil {
			t.Fatalf("length %d: syntax.Marshal: %v", length, err)
		}
		parsed := &GroupContext{}
		if err := syntax.Unmarshal(encoded, parsed); err != nil {
			t.Fatalf("length %d: syntax.Unmarshal: %v", length, err)
		}
		if !groupContextsAgree(context, parsed) {
			t.Fatalf("length %d did not round trip", length)
		}
		reencoded, err := syntax.Marshal(parsed)
		if err != nil {
			t.Fatalf("length %d: re-encode: %v", length, err)
		}
		if !bytes.Equal(reencoded, encoded) {
			t.Fatalf("length %d: re-encoding changed the %d byte encoding", length, len(encoded))
		}
	}
}

// TestGroupContextRoundTripsAMaximalFieldAndRefusesAnOverlongOne pins the two ends of
// the configured vector length limit. A field of exactly the limit must encode and come
// back; one octet more must be refused by the encoder rather than produce bytes a
// compliant peer would reject.
func TestGroupContextRoundTripsAMaximalFieldAndRefusesAnOverlongOne(t *testing.T) {
	maximal := &GroupContext{
		Version:                 ProtocolVersionMls10,
		CipherSuite:             CipherSuiteX25519ChaCha20Sha256Ed25519,
		GroupId:                 repeatByte(0xd1, syntax.MaxVectorLength),
		Epoch:                   math.MaxUint64,
		TreeHash:                repeatByte(0xd2, 32),
		ConfirmedTranscriptHash: repeatByte(0xd3, 32),
	}
	encoded, err := syntax.Marshal(maximal)
	if err != nil {
		t.Fatalf("a group id of exactly MaxVectorLength was refused: %v", err)
	}
	parsed := &GroupContext{}
	if err := syntax.Unmarshal(encoded, parsed); err != nil {
		t.Fatalf("the maximal encoding did not decode: %v", err)
	}
	if !groupContextsAgree(maximal, parsed) {
		t.Fatal("the maximal context did not round trip")
	}

	overlong := maximal.Clone()
	overlong.GroupId = repeatByte(0xd1, syntax.MaxVectorLength+1)
	if _, err := syntax.Marshal(overlong); !errors.Is(err, syntax.ErrLengthExceedsMax) {
		t.Fatalf("a group id one octet over the limit encoded with err = %v, want syntax.ErrLengthExceedsMax", err)
	}
}

// TestEveryGroupContextFieldChangesTheEncoding is the derived completeness gate on the
// codec, and it is the one test here that a field added later cannot slip past.
//
// It walks the struct definition with reflection rather than naming the fields, gives
// each one in turn a different value, and requires both that the encoding moves and that
// the decoder gives the changed value back. A field added to GroupContext and forgotten
// in MarshalMLS therefore fails on the commit that adds it, rather than in the epoch
// where two members disagree about a field only one of them wrote.
func TestEveryGroupContextFieldChangesTheEncoding(t *testing.T) {
	structType := reflect.TypeOf(GroupContext{})
	if structType.NumField() == 0 {
		t.Fatal("GroupContext declares no fields, so this gate walked nothing")
	}
	for i := 0; i < structType.NumField(); i++ {
		field := structType.Field(i)
		base := baseGroupContextForFieldGate()
		variant := baseGroupContextForFieldGate()
		if !varyGroupContextField(reflect.ValueOf(variant).Elem().Field(i)) {
			t.Fatalf("this gate does not know how to vary %s of type %s; teach it that shape rather than dropping the field from the walk",
				field.Name, field.Type)
		}
		if groupContextsAgree(base, variant) {
			t.Fatalf("%s: varying the field did not change the value, so the rest of this case proves nothing", field.Name)
		}
		baseEncoded, err := syntax.Marshal(base)
		if err != nil {
			t.Fatalf("%s: encode the base: %v", field.Name, err)
		}
		variantEncoded, err := syntax.Marshal(variant)
		if err != nil {
			t.Fatalf("%s: encode the variant: %v", field.Name, err)
		}
		if bytes.Equal(baseEncoded, variantEncoded) {
			t.Errorf("%s: two contexts differing only in this field encode identically to %x, so MarshalMLS does not write it",
				field.Name, baseEncoded)
			continue
		}
		parsed := &GroupContext{}
		if err := syntax.Unmarshal(variantEncoded, parsed); err != nil {
			t.Errorf("%s: the variant did not decode: %v", field.Name, err)
			continue
		}
		if !groupContextsAgree(variant, parsed) {
			t.Errorf("%s: the variant decoded to %s, want %s", field.Name,
				describeGroupContext(parsed), describeGroupContext(variant))
		}
	}
}

// baseGroupContextForFieldGate is a context whose every field holds a value the gate
// above can move off, built fresh per case so the two copies share no backing array.
func baseGroupContextForFieldGate() *GroupContext {
	return &GroupContext{
		Version:                 ProtocolVersion(0x0001),
		CipherSuite:             CipherSuite(0x0001),
		GroupId:                 []byte{0x01},
		Epoch:                   1,
		TreeHash:                []byte{0x01},
		ConfirmedTranscriptHash: []byte{0x01},
		Extensions:              []Extension{{ExtensionType: ExtensionType(0x0001), ExtensionData: []byte{0x01}}},
	}
}

// varyGroupContextField sets a field to something the base does not hold, choosing by
// the field's shape so a new field of a shape already handled needs no edit here.
// Reporting false rather than silently skipping is what makes the walk complete.
func varyGroupContextField(field reflect.Value) bool {
	switch field.Kind() {
	case reflect.Uint16, reflect.Uint32, reflect.Uint64:
		field.SetUint(field.Uint() + 1)
		return true
	case reflect.Slice:
		switch field.Type().Elem().Kind() {
		case reflect.Uint8:
			field.SetBytes([]byte{0x02, 0x02})
			return true
		case reflect.Struct:
			if field.Type().Elem() == reflect.TypeOf(Extension{}) {
				field.Set(reflect.ValueOf([]Extension{
					{ExtensionType: ExtensionType(0x0002), ExtensionData: []byte{0x02}},
				}))
				return true
			}
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Clone
// ---------------------------------------------------------------------------

// contextLeaf is one location inside a group context that a holder of that context can write
// to: a scalar field, or the backing array of one opaque field.
type contextLeaf struct {
	path  string
	value reflect.Value
}

// isBytes reports whether the leaf is an opaque field rather than a scalar.
func (self contextLeaf) isBytes() bool {
	return self.value.Kind() == reflect.Slice
}

// write changes the leaf. For an opaque field that means changing a byte of the backing array
// rather than reassigning the field, because a clone that shares the array is only visible
// through the array.
func (self contextLeaf) write() {
	target := self.value
	if self.isBytes() {
		target = self.value.Index(0)
	}
	target.SetUint(target.Uint() ^ 0xff)
}

// contextLeavesOf walks a group context and returns every location a holder can write to,
// derived from the shape of the structure rather than listed beside it.
//
// Listing them is what broke the clone gate before. The version that shipped wrote to GroupId,
// TreeHash and the first extension body and left ConfirmedTranscriptHash out, so a Clone that
// aliased the confirmed transcript hash — the one field the confirmation tag is computed over,
// and therefore the aliasing that matters most — passed. A walk cannot leave a field out, and
// a field whose shape it does not recognise is fatal rather than skipped, so the next field
// added to this structure either enters the gate or stops it.
func contextLeavesOf(t *testing.T, path string, value reflect.Value) []contextLeaf {
	t.Helper()
	switch value.Kind() {
	case reflect.Pointer:
		if value.IsNil() {
			t.Fatalf("%s is nil, so nothing under it is reachable", path)
		}
		return contextLeavesOf(t, path, value.Elem())
	case reflect.Struct:
		leaves := []contextLeaf{}
		for i := range value.NumField() {
			field := value.Type().Field(i)
			if !field.IsExported() {
				t.Fatalf("%s.%s is unexported, so this walk cannot reach it and cannot say whether Clone copies it",
					path, field.Name)
			}
			leaves = append(leaves, contextLeavesOf(t, path+"."+field.Name, value.Field(i))...)
		}
		return leaves
	case reflect.Slice:
		if value.Type().Elem().Kind() == reflect.Uint8 {
			return []contextLeaf{{path: path, value: value}}
		}
		leaves := []contextLeaf{}
		for i := range value.Len() {
			leaves = append(leaves, contextLeavesOf(t, fmt.Sprintf("%s[%d]", path, i), value.Index(i))...)
		}
		return leaves
	case reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return []contextLeaf{{path: path, value: value}}
	}
	t.Fatalf("%s is a %s, a shape this walk cannot write to, which leaves Clone's treatment of it unknown rather than absent",
		path, value.Kind())
	return nil
}

// cloneGateGroupContext is the fixture the gate below walks: every field carries a value and
// every opaque field carries at least one byte.
//
// extensionGoldenGroupContext is the wrong fixture for this, and was the one used. Its
// confirmed transcript hash is empty on purpose — that golden exists partly to pin the
// encoding of a field that is present and empty — and an empty opaque field has no byte to
// write through, so the gate simply stopped asserting anything about that field.
func cloneGateGroupContext() *GroupContext {
	return &GroupContext{
		Version:                 ProtocolVersion(0xfafb),
		CipherSuite:             CipherSuiteX25519ChaCha20Sha256Ed25519,
		GroupId:                 []byte{0xa1, 0xa2, 0xa3},
		Epoch:                   0xfedcba9876543210,
		TreeHash:                []byte{0xb1, 0xb2},
		ConfirmedTranscriptHash: []byte{0xc1, 0xc2, 0xc3, 0xc4},
		Extensions: []Extension{
			{ExtensionType: ExtensionTypeRatchetTree, ExtensionData: []byte{0xd1, 0xd2}},
			{ExtensionType: ExtensionTypeUrmessageGroupPolicy, ExtensionData: []byte{0xe1}},
		},
	}
}

// TestGroupContextCloneIsDeepAtEveryWritableLocation asserts that writing anywhere inside a
// cloned context leaves the original exactly as it was, over every location the walk finds.
// That is the property Clone exists for: an epoch retained to decrypt out of order traffic
// must not be reachable from the live one, and a shared transcript hash or a shared extensions
// slice makes it reachable.
func TestGroupContextCloneIsDeepAtEveryWritableLocation(t *testing.T) {
	fixture := cloneGateGroupContext()
	leaves := contextLeavesOf(t, "GroupContext", reflect.ValueOf(fixture))
	if len(leaves) == 0 {
		t.Fatal("the walk found no writable location, so the sweep below would assert nothing")
	}
	for _, leaf := range leaves {
		if leaf.isBytes() && leaf.value.Len() == 0 {
			t.Fatalf("the fixture leaves %s empty, so no byte of it can be written and any aliasing of it would go unseen — which is exactly how a shared confirmed transcript hash passed this gate before",
				leaf.path)
		}
	}

	written := map[string]bool{}
	for index := range len(leaves) {
		original := cloneGateGroupContext()
		untouched := cloneGateGroupContext()
		before, err := syntax.Marshal(original)
		if err != nil {
			t.Fatalf("syntax.Marshal: %v", err)
		}
		clone := original.Clone()
		if !groupContextsAgree(original, clone) {
			t.Fatal("the clone does not hold the original's value")
		}
		cloneLeaves := contextLeavesOf(t, "GroupContext", reflect.ValueOf(clone))
		if len(cloneLeaves) != len(leaves) {
			t.Fatalf("the clone exposes %d writable locations and the original %d, so the two are not the same shape",
				len(cloneLeaves), len(leaves))
		}
		leaf := cloneLeaves[index]
		written[leaf.path] = true
		leaf.write()

		if !reflect.DeepEqual(original, untouched) {
			t.Errorf("writing to the clone's %s changed the original:\n original %#v\n want     %#v",
				leaf.path, original, untouched)
			continue
		}
		after, err := syntax.Marshal(original)
		if err != nil {
			t.Fatalf("syntax.Marshal after writing to the clone's %s: %v", leaf.path, err)
		}
		if !bytes.Equal(before, after) {
			t.Errorf("writing to the clone's %s changed the original's encoding:\n before %x\n after  %x",
				leaf.path, before, after)
		}
	}

	// the coverage half, derived from the type rather than asserted as a count: every field of
	// the structure must have contributed at least one location, so a fixture that left
	// Extensions empty — and therefore walked no extension body — fails here instead of
	// reporting a clean sweep of the fields it happened to reach.
	structure := reflect.TypeOf(GroupContext{})
	for i := range structure.NumField() {
		name := structure.Field(i).Name
		covered := false
		for path := range written {
			if strings.HasPrefix(path, "GroupContext."+name) {
				covered = true
				break
			}
		}
		if !covered {
			t.Errorf("no writable location under GroupContext.%s was exercised, so Clone's treatment of that field is untested", name)
		}
	}
	t.Logf("%d writable locations checked: %s", len(written), strings.Join(slices.Sorted(maps.Keys(written)), " "))
}

// TestGroupContextCloneReproducesTheValueExactlyIncludingNilness asserts a clone is the same
// Go value as its original and not merely the same encoding — which for the opaque fields
// means nil clones to nil and empty non nil clones to empty non nil.
//
// This needs an observer of its own because everything else in the file is blind to it by
// design. The wire has one spelling for both, groupContextsAgree equates them on purpose since
// the decoder only ever produces the non nil form, and the encoding comparison in the corpus
// clone test cannot see a difference the encoding does not carry. cloneBytes states the
// distinction in its comment and, until this, nothing watched it.
//
// The corpus is reused rather than a case picked by hand, because its opaque axis already
// carries nil, empty and four lengths in each of the three positions, and its extension axis
// carries a nil vector, an empty one, and bodies of both kinds.
func TestGroupContextCloneReproducesTheValueExactlyIncludingNilness(t *testing.T) {
	corpus := generatedGroupContexts(t)
	nils, empties := 0, 0
	for _, context := range corpus {
		for _, leaf := range contextLeavesOf(t, "GroupContext", reflect.ValueOf(context)) {
			if !leaf.isBytes() {
				continue
			}
			if leaf.value.IsNil() {
				nils++
			} else if leaf.value.Len() == 0 {
				empties++
			}
		}
	}
	if nils == 0 || empties == 0 {
		t.Fatalf("the corpus carries %d nil and %d empty opaque fields; both are needed or this property is evaluated in one direction only",
			nils, empties)
	}
	for index, context := range corpus {
		if clone := context.Clone(); !reflect.DeepEqual(context, clone) {
			t.Fatalf("case %d %s: the clone is not the same value as the original:\n original %#v\n clone    %#v",
				index, describeGroupContext(context), context, clone)
		}
	}
	t.Logf("%d clones reproduced their original exactly, across %d nil and %d empty opaque fields",
		len(corpus), nils, empties)
}

// TestGroupContextCloneEncodesIdenticallyForEveryCorpusCase asserts the clone of every
// generated context encodes to the same bytes as the context it came from, which is the
// property callers actually depend on: a retained epoch must hash the same as the live
// one did.
func TestGroupContextCloneEncodesIdenticallyForEveryCorpusCase(t *testing.T) {
	corpus := generatedGroupContexts(t)
	if len(corpus) == 0 {
		t.Fatal("the generated corpus is empty")
	}
	for index, context := range corpus {
		want, err := syntax.Marshal(context)
		if err != nil {
			t.Fatalf("case %d: syntax.Marshal: %v", index, err)
		}
		got, err := syntax.Marshal(context.Clone())
		if err != nil {
			t.Fatalf("case %d: syntax.Marshal the clone: %v", index, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("case %d %s: the clone encodes differently:\n got %x\nwant %x",
				index, describeGroupContext(context), got, want)
		}
	}
}
