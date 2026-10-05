// The two authenticators, held to master section 9.2's block rather than to themselves.
//
// The known answer vectors are the anchors that do not move with the code, and this file
// takes the same care aad_test.go takes over where they came from. Every preimage vector is
// written out by hand, one line per term of the block, so that a permutation applied to the
// builder and to the test at once fails here — which is the whole point of a vector and the
// whole reason a golden captured from the implementation is worth nothing. The values a
// hand cannot compute are the digests and the macs, and those were produced by openssl
// rather than by this package, so a defect shared between the builder and a go library
// still has somewhere to show.
//
// Two of them are worth more than the rest. The key derivations are pinned twice: against
// the openssl output, and against RFC 5869's own arithmetic reconstructed in this file out
// of crypto/hmac, which does not go through crypto/hkdf at all. HKDF-Expand to exactly one
// hash length is a single HMAC — T(1) = HMAC(PRK, info ‖ 0x01), with T(0) empty — so the
// whole of what WriteKey and ReadKey do can be restated in three lines that share no code
// with them. The labels are the only difference between the two functions, and that
// reconstruction is what makes a swap of them observable rather than merely improbable.
//
// The independence tests answer what the vectors cannot: that every field really is in the
// preimage rather than three of them being in it and the vector happening to agree. Their
// mutator table is checked against RecordHeader read by reflection, so a field added to the
// header without a mutator fails rather than passing unobserved, and the complement is
// asserted as well — the fields of Record that must leave the tag alone, record_id among
// them, are computed as the ones the covered set does not name.
//
// The rejection tests are the same class turned around, plus the two classes the vectors
// cannot reach at all: every single bit of the tag, flipped, in all two hundred and fifty six
// positions, and the tags an attacker picks rather than computes. That second one is where the
// two verifiers stopped being observed. Every rejection test written first pairs a correct-key
// correct-preimage tag with a refused input and gets a mismatch — which a verifier that had
// discarded the error and computed something else entirely also produces, by luck, because what
// it computed is not what it was offered either. What observes the discarded error is the tag
// that verifier would have computed: the zero tag under a key it refused, and the mac of the
// empty preimage under an input that has none. Both are pinned, and both are what
// TestNoTagVerifiesUnderAKeyThatIsNotThirtyTwoOctets and TestNoTagVerifiesOnAnInputThatHasNoPreimage
// are for.
//
// Four gates read the syntax tree, and every class any of them runs over is computed. Three are
// guardrail G8 of spec A section 5.9. The place the guardrail names is every production file of
// the package rather than the three file names its text happens to list, and the comparators it
// bans there are derived from the source of the packages this code imports rather than written
// down — the enumeration that stood here held six names and did not hold bytes.HasPrefix, which
// leaks more than bytes.Equal does. The functions the other two run over are computed as well:
// every function whose name begins with Verify. For those two the ban has no bounds at all —
// nothing outside this package is called but the constant time comparison — because a class
// derived from signatures still has a shape, and bytes.Cut and fmt.Sprint are outside it.
// The fourth is TestReadAuthNeverUsesWriteKey, section 5.7's own obligation, and that section
// says how it is to be met: by walking the call graph of ComputeRequestAuth. It is a real walk,
// transitive, over edges read out of the syntax tree, and the class of things it looks for is
// computed too — a write key deriver is any function that names the "write/v1" label, by
// literal or through a constant, so a second deriver added later is in the class on the day it
// is written. Every one of the four has a positive control under testdata, because a gate that
// reports nothing because it is broken reports exactly what a gate that reports nothing because
// the package is clean reports.
//
// What the gates do not see is stated where each one is defined rather than here, because a
// gate whose blind spots are only in a summary is a gate whose blind spots are not read.
package message

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/printer"
	"go/token"
	"go/types"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// ── the key derivations ─────────────────────────────────────────────────────────────

// The storage root both key vectors expand from. It is aad_test.go's ramp because there is
// one ramp in this package's tests and a second copy of it would be a second thing to keep
// right; the bytes are consecutive so that a field read at the wrong offset lands on values
// that visibly are not its own.
func writeAuthKatStorageRoot() []byte {
	return aadRamp(0x00, 32)
}

// The two keys HKDF-Expand answers for that root, computed with openssl and not with this
// package. The derivation, which a reader can follow without running anything:
//
//	RFC 5869 section 2.3. OKM = T(1) ‖ T(2) ‖ … truncated to L, where T(0) is empty and
//	T(i) = HMAC(PRK, T(i-1) ‖ info ‖ i). Here L is 32, the hash is SHA-256 and HashLen is
//	therefore 32, so N = ceil(32/32) = 1 and the whole output is T(1):
//
//	    write_key = HMAC-SHA-256(00 01 … 1f, "write/v1" ‖ 0x01)
//	              = HMAC-SHA-256(00 01 … 1f, 77 72 69 74 65 2f 76 31 01)
//	    read_key  = HMAC-SHA-256(00 01 … 1f, "read/v1" ‖ 0x01)
//	              = HMAC-SHA-256(00 01 … 1f, 72 65 61 64 2f 76 31 01)
const (
	writeAuthKatWriteKeyHex = "9902690fdc479609b80e3ec2d769ed4389f60854c7f91e718861f9d71a4738b9"
	writeAuthKatReadKeyHex  = "74f606682b23e852799637a0a830c64f16e9bb9f48039fb705af2fcad803ee66"
)

// RFC 5869 section 2.3's expansion, for the single block case, written out of crypto/hmac.
//
// It is here so that the two derivations are pinned against the arithmetic rather than
// against crypto/hkdf's implementation of it. It shares no code with WriteKey or ReadKey, so
// a defect inside the library, a wrong length, or a swapped label are all observable here,
// and it is short enough that a reader can check it against the RFC line for line.
func writeAuthRfc5869ExpandOneBlock(t testing.TB, prk []byte, info string) []byte {
	t.Helper()
	mac := hmac.New(sha256.New, prk)
	mac.Write([]byte(info))
	mac.Write([]byte{0x01})
	out := mac.Sum(nil)
	if len(out) != authKeyBytes {
		t.Fatalf("the single block expansion produced %d octets, want %d", len(out), authKeyBytes)
	}
	return out
}

// The two keys, pinned against openssl and against RFC 5869's arithmetic, and separated from
// each other.
//
// The separation assertion is the one that earns its place. Both keys are HKDF-Expand of the
// same root to the same length under the same hash, so the label is the entire difference
// between them; swap the two constants and every derivation still answers thirty two
// plausible octets, both paths still agree with themselves, and nothing anywhere else in this
// package can tell. Master section 9.2 requires them to be different keys because the server
// discards a write key within a minute and retains a read key for ninety days, so a swap
// hands the server the wrong one of the two for both purposes.
func TestWriteKeyAndReadKeyArePinnedToRfc5869(t *testing.T) {
	root := writeAuthKatStorageRoot()
	for _, vector := range []struct {
		name string
		got  []byte
		info string
		want string
	}{
		{name: "write_key", got: WriteKey(root), info: "write/v1", want: writeAuthKatWriteKeyHex},
		{name: "read_key", got: ReadKey(root), info: "read/v1", want: writeAuthKatReadKeyHex},
	} {
		if len(vector.got) != authKeyBytes {
			t.Errorf("%s is %d octets, want %d", vector.name, len(vector.got), authKeyBytes)
		}
		if hex.EncodeToString(vector.got) != vector.want {
			t.Errorf("%s is %s, want %s", vector.name, hex.EncodeToString(vector.got), vector.want)
		}
		// the same value a second time, out of the RFC rather than out of crypto/hkdf
		rebuilt := writeAuthRfc5869ExpandOneBlock(t, root, vector.info)
		if !bytes.Equal(vector.got, rebuilt) {
			t.Errorf("%s is %s, and RFC 5869's own expansion under %q is %s",
				vector.name, hex.EncodeToString(vector.got), vector.info, hex.EncodeToString(rebuilt))
		}
	}
	if bytes.Equal(WriteKey(root), ReadKey(root)) {
		t.Fatalf("the write key and the read key of one storage root are both %s; the label is the only thing between them",
			hex.EncodeToString(WriteKey(root)))
	}
}

// The vectors above only mean something if the two functions really are one expansion under
// two labels, so this says it from the other side: each is the one block expansion under the
// constant it names, the two constants differ, and neither function ignores its root.
func TestTheTwoKeysDifferOnlyInTheirLabel(t *testing.T) {
	root := writeAuthKatStorageRoot()
	if !bytes.Equal(WriteKey(root), writeAuthRfc5869ExpandOneBlock(t, root, writeKeyInfo)) {
		t.Errorf("WriteKey is not the one block expansion under %q", writeKeyInfo)
	}
	if !bytes.Equal(ReadKey(root), writeAuthRfc5869ExpandOneBlock(t, root, readKeyInfo)) {
		t.Errorf("ReadKey is not the one block expansion under %q", readKeyInfo)
	}
	if writeKeyInfo == readKeyInfo {
		t.Fatalf("both keys are expanded under %q, so they are one key", writeKeyInfo)
	}
	other := aadRamp(0x50, 32)
	if bytes.Equal(WriteKey(root), WriteKey(other)) {
		t.Error("two different storage roots share a write key")
	}
	if bytes.Equal(ReadKey(root), ReadKey(other)) {
		t.Error("two different storage roots share a read key")
	}
}

// ── the write_auth vectors ──────────────────────────────────────────────────────────

// Everything a write_auth preimage is built from, in one value.
//
// The attachment is not a field of its own. It is an argument to the builder and a field of
// the header, the builder refuses the two disagreeing, and holding it once here is what makes
// a mutation of it unable to move one without the other — which is the failure that shape
// exists to prevent, not a convenience.
type writeAuthInput struct {
	nonce  []byte
	header RecordHeader
	ctHead []byte
}

func (self writeAuthInput) copy() writeAuthInput {
	return writeAuthInput{nonce: self.nonce, header: self.header, ctHead: self.ctHead}
}

// The three call shapes every test below goes through, so the argument order is written once.
func writeAuthPreimageOf(in writeAuthInput) []byte {
	return WriteAuthPreimage(in.nonce, &in.header, in.ctHead, in.header.ServerAttachment)
}

func writeAuthTagOf(key []byte, in writeAuthInput) [32]byte {
	return ComputeWriteAuth(key, in.nonce, &in.header, in.ctHead, in.header.ServerAttachment)
}

// The record a verifier is given, carrying the tag the computing half produced for it. The
// record id is left unassigned, which is what a submitted record carries: the server assigns
// it after acceptance and it is in neither preimage.
func writeAuthRecordOf(key []byte, in writeAuthInput) *Record {
	return &Record{Header: in.header, CtHead: in.ctHead, WriteAuth: writeAuthTagOf(key, in)}
}

// The eph blob-rung commit both of the first vectors are built from, and the axes it pins: an
// eph class, so the retention octet is the joined 0x10|bucket and not the go tag 3; a commit,
// so is_commit is set; the blob rung, so blob_id is present and thirty two octets; an
// expire_at that is a real millisecond timestamp; an attachment, so its hash field is the
// hash of something; and a ct_head of ninety six octets, which is past the fifty five a one
// block SHA-256 message can hold and past the sixty four octet block itself, so an H that
// hashed only its first block would disagree here.
func writeAuthKatEphInput() writeAuthInput {
	header := RecordHeader{
		Epoch:            1,
		StreamIndex:      0xFFFFFFFF,
		IsCommit:         true,
		RetentionClass:   RetentionEph,
		EphBucket:        5,
		EphWindow:        pinnedEphWindow,
		SizeBucket:       SizeBucketBlob,
		ExpireAt:         0x0000018F5CD3A600,
		BlobId:           aadRamp(0xd0, 32),
		ServerAttachment: []byte{0xde, 0xad, 0xbe, 0xef},
	}
	copy(header.GroupId[:], aadRamp(0x01, 32))
	copy(header.SenderHandle[:], aadRamp(0xa0, 16))
	copy(header.BodyHash[:], aadRamp(0xb0, 32))
	return writeAuthInput{nonce: aadRamp(0x60, 32), header: header, ctHead: aadRamp(0x90, 96)}
}

// The ordinary record the other vectors are built from, the opposite of the one above on
// every axis it can be: a class that carries no bucket, not a commit, the smallest rung
// rather than the blob rung, no blob id, expire_at unset, and no attachment at all. Between
// the two, every field of the preimage is pinned in both of the shapes it takes.
//
// Its nonce is eight octets rather than thirty two, and that is the point of it. The nonce is
// thirty two octets on the wire, this layer takes it as an opaque string, and a builder that
// wrote it raw — or that assumed a width and copied it into a fixed field — agrees with every
// thirty two octet vector ever written and disagrees with this one on the length alone.
func writeAuthKatOrdinaryInput() writeAuthInput {
	header := RecordHeader{
		Epoch:          0x0000000100000000,
		StreamIndex:    0xFFFFFFFFFFFFFFFF,
		IsCommit:       false,
		RetentionClass: RetentionDurable,
		// the presence rule's zero half: written whole, at full width, on a class that has
		// no window of its own
		EphWindow:  0,
		SizeBucket: SizeBucket256,
	}
	copy(header.GroupId[:], aadRamp(0x21, 32))
	copy(header.SenderHandle[:], aadRamp(0x80, 16))
	copy(header.BodyHash[:], aadRamp(0x40, 32))
	return writeAuthInput{nonce: aadRamp(0x30, 8), header: header, ctHead: []byte{0xca, 0xfe, 0xba, 0xbe}}
}

// write_auth's preimage, pinned to its exact bytes, in both shapes every optional field takes.
//
// Derived by hand from master section 9.2's block and not by printing what the builder
// produced: a vector taken from the builder pins the builder to itself. Each line is one term
// of that block, in the order the block writes them.
//
// Three lines are the ones to read twice. The label is eighteen raw ascii octets with no
// length prefix in front of them. LP(H(ct_head)) is thirty six octets whatever ct_head's
// length is, because it is the hash and never the ciphertext — 4f2a647f…94d71612 is SHA-256
// of the ninety six octet ramp 90 91 … ef, and a builder that wrote ct_head itself would
// produce a hundred octet field here and fail on the length before it failed on the bytes.
// And LP(server_nonce) carries its own length, so the eight octet nonce of the second vector
// is eight octets with 00000008 in front of it rather than a padded or truncated thirty two.
func TestWriteAuthPreimageIsPinnedToItsExactBytes(t *testing.T) {
	const wantEph = "55526d6573736167652f76312f7772697465" + // "URmessage/v1/write", raw ascii, no prefix
		"00000020" + "606162636465666768696a6b6c6d6e6f707172737475767778797a7b7c7d7e7f" + // LP(server_nonce)
		"00000020" + "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20" + // LP(group_id)
		"00000010" + "a0a1a2a3a4a5a6a7a8a9aaabacadaeaf" + // LP(sender_handle)
		"0000000000000001" + // u64(epoch)
		"00000000ffffffff" + // u64(stream_index)
		"01" + // u8(is_commit): set
		"15" + // u8(retention_class): eph bucket 5, the joined byte and not the go tag 3
		"00000000000002c5" + // u64(eph_window): 709, immediately after the class it qualifies
		"05" + // u8(size_bucket): the blob rung
		"0000018f5cd3a600" + // u64(expire_at): unix milliseconds
		"00000020" + "4f2a647f68974755b85ecf83f409dac55f94d5446953429f2fb2e37194d71612" + // LP(H(ct_head)), 96 octets in
		"00000020" + "b0b1b2b3b4b5b6b7b8b9babbbcbdbebfc0c1c2c3c4c5c6c7c8c9cacbcccdcecf" + // LP(body_hash)
		"00000020" + "d0d1d2d3d4d5d6d7d8d9dadbdcdddedfe0e1e2e3e4e5e6e7e8e9eaebecedeeef" + // LP(blob_id)
		"00000020" + "5f78c33274e43fa9de5659265c1d917e25c03722dcb0b8d27db8d5feaa813953" // LP(H(de ad be ef))

	const wantEphLength = 18 + 36 + 36 + 20 + 8 + 8 + 1 + 1 + 8 + 1 + 8 + 36 + 36 + 36 + 36

	const wantOrdinary = "55526d6573736167652f76312f7772697465" + // the same label
		"00000008" + "3031323334353637" + // LP(server_nonce): eight octets, and the prefix says so
		"00000020" + "2122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f40" + // LP(group_id)
		"00000010" + "808182838485868788898a8b8c8d8e8f" + // LP(sender_handle)
		"0000000100000000" + // u64(epoch): the first value that does not fit in 32 bits
		"ffffffffffffffff" + // u64(stream_index): the top of the range
		"00" + // u8(is_commit): clear
		"01" + // u8(retention_class): durable, a class that carries no bucket
		"0000000000000000" + // u64(eph_window): the zero a non eph record carries, written whole
		"00" + // u8(size_bucket): the 256 B rung
		"0000000000000000" + // u64(expire_at): unset
		"00000020" + "65ab12a8ff3263fbc257e5ddf0aa563c64573d0bab1f1115b9b107834cfa6971" + // LP(H(ca fe ba be))
		"00000020" + "404142434445464748494a4b4c4d4e4f505152535455565758595a5b5c5d5e5f" + // LP(body_hash)
		"00000000" + // LP(blob_id): absent, and still four octets
		"00000020" + "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" // LP(H("")), the absent attachment

	const wantOrdinaryLength = 18 + 12 + 36 + 20 + 8 + 8 + 1 + 1 + 8 + 1 + 8 + 36 + 36 + 4 + 36

	for _, vector := range []struct {
		name   string
		input  writeAuthInput
		want   string
		length int
	}{
		{name: "the eph blob-rung commit", input: writeAuthKatEphInput(), want: wantEph, length: wantEphLength},
		{name: "the ordinary durable record", input: writeAuthKatOrdinaryInput(), want: wantOrdinary, length: wantOrdinaryLength},
	} {
		if len(vector.want) != 2*vector.length {
			t.Fatalf("%s: the vector is %d octets and the block adds up to %d", vector.name, len(vector.want)/2, vector.length)
		}
		got := writeAuthPreimageOf(vector.input)
		if hex.EncodeToString(got) != vector.want {
			t.Errorf("%s: the write_auth preimage is\n%s\nwant\n%s", vector.name, hex.EncodeToString(got), vector.want)
		}
	}
	// the vector's own claims about the two digests a reader cannot compute by eye, checked
	// against the standard library rather than against the strings above
	ctHeadHash := sha256.Sum256(aadRamp(0x90, 96))
	if !strings.Contains(wantEph, hex.EncodeToString(ctHeadHash[:])) {
		t.Errorf("the eph vector does not carry SHA-256 of its own ct_head, %s", hex.EncodeToString(ctHeadHash[:]))
	}
	empty := sha256.Sum256(nil)
	if !strings.HasSuffix(wantOrdinary, hex.EncodeToString(empty[:])) {
		t.Errorf("the ordinary vector ends %s, want the SHA-256 of the empty string %s",
			wantOrdinary[len(wantOrdinary)-64:], hex.EncodeToString(empty[:]))
	}
}

// The tags themselves, pinned. HMAC-SHA-256 of the two preimages above under the write key of
// the storage root 00 01 … 1f, computed outside this module.
//
//	write_key  = 9902690f…1a4738b9, the vector pinned further up
//	write_auth = HMAC-SHA-256(write_key, the preimage the vector above pins)
//
// It is a second anchor rather than a restatement of the first. The preimage vector says the
// bytes are right; this says the mac is HMAC-SHA-256 over exactly those bytes under exactly
// that key, taken whole rather than truncated, and it is the value a second implementation
// checks itself against without reading any of this package.
// Both moved on 2026-09-13 and the reason is worth a line, because a moved KAT is the shape
// a retuned fixture takes. eph_window added eight octets to every preimage, so every tag over
// one changes; the OLD values are not wrong about the old preimage and they are not what this
// package computes any more. They were recomputed the way the originals were -- HMAC-SHA-256
// of the hand built preimage hexadecimal above, under the write key the vector further up
// pins, by a program that imports nothing from this module -- and not by printing what
// ComputeWriteAuth answered.
const (
	writeAuthKatEphTagHex      = "2bb60432d54b16ef555e709156551a0bccfc69a113d46625a774a7a28606dc9f"
	writeAuthKatOrdinaryTagHex = "06b5f63d5c89015115920c6455b0358c54b5257d2e01302551edb8bc24774f0f"
)

func TestWriteAuthTagIsPinnedToItsExactBytes(t *testing.T) {
	key := WriteKey(writeAuthKatStorageRoot())
	for _, vector := range []struct {
		name  string
		input writeAuthInput
		want  string
	}{
		{name: "the eph blob-rung commit", input: writeAuthKatEphInput(), want: writeAuthKatEphTagHex},
		{name: "the ordinary durable record", input: writeAuthKatOrdinaryInput(), want: writeAuthKatOrdinaryTagHex},
	} {
		tag := writeAuthTagOf(key, vector.input)
		if hex.EncodeToString(tag[:]) != vector.want {
			t.Errorf("%s: write_auth is %s, want %s", vector.name, hex.EncodeToString(tag[:]), vector.want)
		}
		// and the tag is the whole mac rather than a truncation padded back out: the same
		// value taken straight from crypto/hmac over the pinned preimage
		mac := hmac.New(sha256.New, key)
		mac.Write(writeAuthPreimageOf(vector.input))
		if !bytes.Equal(tag[:], mac.Sum(nil)) {
			t.Errorf("%s: write_auth is not HMAC-SHA-256 of its own preimage under the write key", vector.name)
		}
	}
}

// LP(H(ct_head)) is the hash and never the ciphertext, asserted on the length before the
// bytes.
//
// This is its own test rather than a line in the vector because the vector cannot say why it
// is thirty six octets. A builder that wrote ct_head straight would still produce a preimage,
// still round trip against itself, and still pass every test written with a short ct_head; it
// fails here on two ct_heads of different lengths producing preimages of different lengths,
// which is the observation that names the defect rather than merely detecting it.
func TestTheWriteAuthPreimageHashesCtHeadRatherThanCarryingIt(t *testing.T) {
	base := writeAuthKatEphInput()
	lengths := []int{0, 1, 32, 96, 4112}
	sizes := map[int]bool{}
	for _, length := range lengths {
		in := base.copy()
		in.ctHead = aadRamp(0x11, length)
		preimage := writeAuthPreimageOf(in)
		sizes[len(preimage)] = true
		// the field is where the block puts it and it is the digest of what was handed in
		digest := sha256.Sum256(in.ctHead)
		if !bytes.Contains(preimage, append([]byte{0x00, 0x00, 0x00, 0x20}, digest[:]...)) {
			t.Errorf("a ct_head of %d octets does not put LP(SHA-256(ct_head)) in the preimage", length)
		}
		if bytes.Contains(preimage, in.ctHead) && 32 < length {
			t.Errorf("a ct_head of %d octets appears in the preimage verbatim", length)
		}
	}
	if len(sizes) != 1 {
		t.Errorf("ct_heads of %v octets produced preimages of %d different lengths; the field is a digest and its width is fixed",
			lengths, len(sizes))
	}
	// and it is really in there: two ct_heads must not share a preimage
	one, other := base.copy(), base.copy()
	one.ctHead = []byte{0x01}
	other.ctHead = []byte{0x02}
	if bytes.Equal(writeAuthPreimageOf(one), writeAuthPreimageOf(other)) {
		t.Error("two different ct_heads share a write_auth preimage")
	}
}

// ── the req_auth vectors ────────────────────────────────────────────────────────────

// req_auth's preimage, pinned to its exact bytes, in both shapes it takes.
//
// Derived by hand from master section 9.2's block, one line per term. The op octets are two
// of the five arms that carry a req_auth — 13 for FetchRequest and 17 for BlobGrantRequest —
// written as the u8 field numbers the block names rather than as an enum of this package's,
// because this package knows nothing about the request beyond its octets.
//
// The second vector's request body is empty, and that is a legal request rather than a
// missing one: a protobuf message holding only defaults marshals to nothing, and LP writes
// the four zero octets for it because there is no representation for absent to tell it apart
// from an empty one.
func TestRequestAuthPreimageIsPinnedToItsExactBytes(t *testing.T) {
	const wantFetch = "55526d6573736167652f76312f726571" + // "URmessage/v1/req", raw ascii, no prefix
		"00000020" + "606162636465666768696a6b6c6d6e6f707172737475767778797a7b7c7d7e7f" + // LP(server_nonce)
		"0d" + // u8(op): 13, FetchRequest
		"00000006" + "f0f1f2f3f4f5" // LP(request_bytes)

	const wantFetchLength = 16 + 36 + 1 + 10

	const wantBlobGrant = "55526d6573736167652f76312f726571" + // the same label
		"00000008" + "3031323334353637" + // LP(server_nonce): eight octets, and the prefix says so
		"11" + // u8(op): 17, BlobGrantRequest
		"00000000" // LP(request_bytes): a body that marshals to nothing, and still four octets

	const wantBlobGrantLength = 16 + 12 + 1 + 4

	for _, vector := range []struct {
		name         string
		nonce        []byte
		op           uint8
		requestBytes []byte
		want         string
		length       int
	}{
		{name: "a fetch", nonce: aadRamp(0x60, 32), op: 13, requestBytes: aadRamp(0xf0, 6), want: wantFetch, length: wantFetchLength},
		{name: "a blob grant with an empty body", nonce: aadRamp(0x30, 8), op: 17, requestBytes: nil, want: wantBlobGrant, length: wantBlobGrantLength},
	} {
		if len(vector.want) != 2*vector.length {
			t.Fatalf("%s: the vector is %d octets and the block adds up to %d", vector.name, len(vector.want)/2, vector.length)
		}
		got := RequestAuthPreimage(vector.nonce, vector.op, vector.requestBytes)
		if hex.EncodeToString(got) != vector.want {
			t.Errorf("%s: the req_auth preimage is\n%s\nwant\n%s", vector.name, hex.EncodeToString(got), vector.want)
		}
	}
}

// The tags, pinned. HMAC-SHA-256 of the two preimages above under the read key of the storage
// root 00 01 … 1f, computed with openssl.
//
//	read_key = 74f60668…d803ee66, the vector pinned further up
//	req_auth = HMAC-SHA-256(read_key, the preimage the vector above pins)
//
// The key is the read key and not the write key, which is the whole of master section 9.2's
// argument about the ninety day window: a member offline across one commit holds a write key
// the server discarded a minute later, and every route out of that condition is itself a
// read. TestReadAuthNeverUsesWriteKey asserts the code has no path to the other key at all;
// this pins that the vector really was taken under this one.
const (
	requestAuthKatFetchTagHex     = "9bfde48be3abdb09121d59426b6e558ef666654b1a68912821ae2b8235bffaba"
	requestAuthKatBlobGrantTagHex = "f2483d3702ecfc08c7e247c2e7901e5234655d3e60b3d633efc65ea05711c888"
)

func TestRequestAuthTagIsPinnedToItsExactBytes(t *testing.T) {
	readKey := ReadKey(writeAuthKatStorageRoot())
	for _, vector := range []struct {
		name         string
		nonce        []byte
		op           uint8
		requestBytes []byte
		want         string
	}{
		{name: "a fetch", nonce: aadRamp(0x60, 32), op: 13, requestBytes: aadRamp(0xf0, 6), want: requestAuthKatFetchTagHex},
		{name: "a blob grant with an empty body", nonce: aadRamp(0x30, 8), op: 17, requestBytes: nil, want: requestAuthKatBlobGrantTagHex},
	} {
		tag := ComputeRequestAuth(readKey, vector.nonce, vector.op, vector.requestBytes)
		if hex.EncodeToString(tag[:]) != vector.want {
			t.Errorf("%s: req_auth is %s, want %s", vector.name, hex.EncodeToString(tag[:]), vector.want)
		}
		if !VerifyRequestAuth(readKey, vector.nonce, vector.op, vector.requestBytes, tag[:]) {
			t.Errorf("%s: the tag req_auth just computed does not verify", vector.name)
		}
		// the tag is taken under the read key and under no other: the write key of the same
		// root must not produce it
		writeKey := WriteKey(writeAuthKatStorageRoot())
		if VerifyRequestAuth(writeKey, vector.nonce, vector.op, vector.requestBytes, tag[:]) {
			t.Errorf("%s: the tag verifies under the write key of the same storage root", vector.name)
		}
	}
}

// ── the mutators, derived from the structs ──────────────────────────────────────────

// The input every mutation below starts from.
//
// It is an eph record on purpose, for the reason aad_test.go's base is: the retention class
// and the eph bucket are two go fields and one wire octet, a bucket is only legal under the
// eph class, and a base that was durable would have no legal single field mutation of
// EphBucket at all. Starting at bucket 2 leaves the bucket free to move on its own, and the
// class's own mutation carries the bucket back to zero with it — the one place a mutator here
// touches two fields, and it is because the wire carries them as one.
func writeAuthBaseInput() writeAuthInput {
	header := RecordHeader{
		Epoch:          7,
		StreamIndex:    9,
		IsCommit:       false,
		RetentionClass: RetentionEph,
		EphBucket:      2,
		SizeBucket:     SizeBucket256,
		ExpireAt:       0,
	}
	copy(header.GroupId[:], aadRamp(0x01, 32))
	copy(header.SenderHandle[:], aadRamp(0xa0, 16))
	copy(header.BodyHash[:], aadRamp(0xb0, 32))
	return writeAuthInput{nonce: aadRamp(0x60, 32), header: header, ctHead: aadRamp(0x90, 96)}
}

// One mutation per thing the preimage is built from, keyed by the field's own name so the
// table can be checked against the structs rather than against a list somebody kept up to
// date.
//
// Every one of them changes exactly one field, with the single exception the base's comment
// explains, and each moves to a value the wire can carry — except BlobId, which deliberately
// does not, for the reason aad_test.go gives at length: the base is on the 256 B rung, so a
// blob id on it is a record ParseRecord refuses, and that is exactly what kills the "if the
// bucket is the blob rung" a reader's instinct writes into the builder. Moving the rung with
// it would make the header legal and the mutation useless. The preimage builders validate
// nothing, which is what makes an illegal header a legitimate input here.
var writeAuthMutators = map[string]func(*writeAuthInput){
	"nonce":        func(in *writeAuthInput) { in.nonce = aadRamp(0x11, 32) },
	"ctHead":       func(in *writeAuthInput) { in.ctHead = aadRamp(0x22, 96) },
	"GroupId":      func(in *writeAuthInput) { in.header.GroupId[31] ^= 0xFF },
	"SenderHandle": func(in *writeAuthInput) { in.header.SenderHandle[15] ^= 0xFF },
	"Epoch":        func(in *writeAuthInput) { in.header.Epoch++ },
	"StreamIndex":  func(in *writeAuthInput) { in.header.StreamIndex++ },
	"IsCommit":     func(in *writeAuthInput) { in.header.IsCommit = !in.header.IsCommit },
	// the class moves off eph, and the bucket has to come back to zero with it: the pair is
	// one wire octet and eph bucket 2 under the media class is not an octet at all
	"RetentionClass": func(in *writeAuthInput) {
		in.header.RetentionClass = RetentionMedia
		in.header.EphBucket = 0
	},
	"EphBucket":        func(in *writeAuthInput) { in.header.EphBucket++ },
	"EphWindow":        func(in *writeAuthInput) { in.header.EphWindow++ },
	"SizeBucket":       func(in *writeAuthInput) { in.header.SizeBucket = SizeBucket1K },
	"ExpireAt":         func(in *writeAuthInput) { in.header.ExpireAt = 0x0000018F5CD3A600 },
	"BodyHash":         func(in *writeAuthInput) { in.header.BodyHash[0] ^= 0xFF },
	"BlobId":           func(in *writeAuthInput) { in.header.BlobId = aadRamp(0xd0, 32) },
	"ServerAttachment": func(in *writeAuthInput) { in.header.ServerAttachment = []byte{0xde, 0xad, 0xbe, 0xef} },
}

// The class every assertion below runs over: everything a write_auth preimage is built from,
// computed from the two structs rather than written down.
//
// It is writeAuthInput's own fields with the header's substituted for the header, found by
// type rather than by name so that renaming the field does not quietly stop the expansion.
// The header is not one input but twelve, and the whole reason the preimage is worth testing
// field by field is that it covers every one of them — master invariant I6, that the server
// acts only on values it can verify.
func writeAuthCoveredNames() []string {
	names := []string{}
	inputType := reflect.TypeOf(writeAuthInput{})
	headerType := reflect.TypeOf(RecordHeader{})
	for i := range inputType.NumField() {
		field := inputType.Field(i)
		if field.Type == headerType {
			names = append(names, aadFieldNames(RecordHeader{})...)
			continue
		}
		names = append(names, field.Name)
	}
	slices.Sort(names)
	return names
}

// One mutation, looked up by name rather than indexed straight, so a field with no mutator
// reports itself here instead of calling a nil function and taking the rest of the run down
// with it.
func writeAuthMutate(t testing.TB, name string, in *writeAuthInput) {
	t.Helper()
	mutate, present := writeAuthMutators[name]
	if !present {
		t.Fatalf("%s has no mutator, so nothing in this file ever varies it", name)
	}
	mutate(in)
}

// The mutator table covers the inputs exactly: nothing without a mutation, and no mutation
// for something that is gone.
//
// This is the test that keeps every independence assertion below honest. Without it a field
// added to RecordHeader or to the input is a field no test in this file ever varies, and the
// first anybody hears of it is a server refusing every record.
func TestEveryWriteAuthInputHasAMutator(t *testing.T) {
	covered := writeAuthCoveredNames()
	mutated := slices.Sorted(maps.Keys(writeAuthMutators))
	if !slices.Equal(covered, mutated) {
		t.Fatalf("the preimage is built from %v and the mutator table has %v; every input needs one and no more", covered, mutated)
	}
	if len(covered) == 0 {
		t.Fatal("the preimage is built from nothing, so every independence test below is vacuous")
	}
}

// ── field independence ──────────────────────────────────────────────────────────────

// Every input the preimage covers changes the preimage, one at a time.
//
// The class is computed, so this is the assertion that fails on the day a field is added to
// RecordHeader and not written into the block. Master invariant I6 is what makes that a
// defect rather than an omission: the server acts only on values it can verify, so a header
// field outside write_auth is a field the server reads and cannot trust.
func TestEveryInputTheWriteAuthPreimageCoversChangesIt(t *testing.T) {
	base := writeAuthBaseInput()
	want := writeAuthPreimageOf(base)
	for _, name := range writeAuthCoveredNames() {
		mutated := base.copy()
		writeAuthMutate(t, name, &mutated)
		if got := writeAuthPreimageOf(mutated); bytes.Equal(got, want) {
			t.Errorf("changing %s leaves the write_auth preimage unchanged at\n%s", name, hex.EncodeToString(want))
		}
	}
}

// The same class through the mac, which is a different claim from the one above: a preimage
// that changed is only worth having if the tag over it changed too, and a compute path that
// dropped the preimage on the floor and macd something else would pass the test above and
// fail this one.
func TestEveryInputTheWriteAuthPreimageCoversChangesTheTag(t *testing.T) {
	key := WriteKey(writeAuthKatStorageRoot())
	base := writeAuthBaseInput()
	want := writeAuthTagOf(key, base)
	seen := map[string]string{hex.EncodeToString(want[:]): "the base input"}
	for _, name := range writeAuthCoveredNames() {
		mutated := base.copy()
		writeAuthMutate(t, name, &mutated)
		tag := writeAuthTagOf(key, mutated)
		if other, collided := seen[hex.EncodeToString(tag[:])]; collided {
			t.Errorf("changing %s gives the same write_auth as %s: %s", name, other, hex.EncodeToString(tag[:]))
			continue
		}
		seen[hex.EncodeToString(tag[:])] = "the input with " + name + " changed"
	}
	if want := len(writeAuthCoveredNames()) + 1; len(seen) != want {
		t.Errorf("%d inputs produced %d distinct tags", want-1, len(seen))
	}
}

// The key and the nonce are the two things outside the preimage's own fields that the tag
// must depend on, and neither is a field of any struct above, so no mutator reaches the key
// at all.
func TestTheWriteAuthTagDependsOnTheKeyAndTheNonce(t *testing.T) {
	base := writeAuthBaseInput()
	tag := writeAuthTagOf(WriteKey(writeAuthKatStorageRoot()), base)
	underOtherKey := writeAuthTagOf(WriteKey(aadRamp(0x50, 32)), base)
	if tag == underOtherKey {
		t.Errorf("two storage roots give one write_auth: %s", hex.EncodeToString(tag[:]))
	}
	// the read key of the same root is the sibling that matters most: it is the same length,
	// the same shape, and one label away
	underReadKey := writeAuthTagOf(ReadKey(writeAuthKatStorageRoot()), base)
	if tag == underReadKey {
		t.Errorf("the write key and the read key of one root give one write_auth: %s", hex.EncodeToString(tag[:]))
	}
	other := base.copy()
	other.nonce = aadRamp(0x11, 32)
	if tag == writeAuthTagOf(WriteKey(writeAuthKatStorageRoot()), other) {
		t.Errorf("two connection nonces give one write_auth: %s", hex.EncodeToString(tag[:]))
	}
}

// ── what the tag does not cover ─────────────────────────────────────────────────────

// The role every field of Record plays in write_auth. The class it is checked against is
// Record's own fields read by reflection, so a field added to the record has to be given a
// role here rather than inheriting one by being forgotten.
//
// record_id's role is the one worth saying out loud. Spec A section 5.1: the id is assigned by
// the server after acceptance, which is after write_auth has been verified, so there is
// nothing to authenticate when the mac is taken and nothing a client could reproduce
// afterwards. It is pagination and hole detection only. ct_body's role is the same answer for
// a different reason — body_hash is in the preimage and the ciphertext is not, which is what
// lets a pruned record whose body has been erased still carry a mac that verifies.
var writeAuthRecordFieldRoles = map[string]string{
	"RecordId":  "outside",
	"Header":    "covered",
	"CtHead":    "covered",
	"CtBody":    "outside",
	"WriteAuth": "the tag itself",
}

// One mutation per field the tag must not cover, checked below against the roles above.
var writeAuthOutsideMutators = map[string]func(*Record){
	"RecordId": func(r *Record) { r.RecordId = 0x0123456789ABCDEF },
	"CtBody":   func(r *Record) { r.CtBody = aadRamp(0x33, 272) },
}

// Every field of Record has a role, and every field said to be outside the tag really is.
//
// The two halves are one test because neither is worth much alone: a role table that had
// drifted from the struct would leave a new field unobserved, and a role table nobody
// exercised would be a comment. The fields the roles call covered are exercised by the
// independence tests further up, over a class computed the same way.
func TestTheTagCoversExactlyTheRecordFieldsItShould(t *testing.T) {
	fields := aadFieldNames(Record{})
	roled := slices.Sorted(maps.Keys(writeAuthRecordFieldRoles))
	if !slices.Equal(fields, roled) {
		t.Fatalf("Record has %v and the role table has %v; every field needs a role and no more", fields, roled)
	}
	outside := []string{}
	for _, name := range fields {
		if writeAuthRecordFieldRoles[name] == "outside" {
			outside = append(outside, name)
		}
	}
	if !slices.Equal(outside, slices.Sorted(maps.Keys(writeAuthOutsideMutators))) {
		t.Fatalf("the roles put %v outside the tag and the mutator table has %v", outside, slices.Sorted(maps.Keys(writeAuthOutsideMutators)))
	}
	if len(outside) == 0 {
		t.Fatal("no field of Record is outside the tag, so there is nothing here to observe")
	}
	key := WriteKey(writeAuthKatStorageRoot())
	base := writeAuthBaseInput()
	for _, name := range outside {
		record := writeAuthRecordOf(key, base)
		before := record.WriteAuth
		writeAuthOutsideMutators[name](record)
		after := ComputeWriteAuth(key, base.nonce, &record.Header, record.CtHead, record.Header.ServerAttachment)
		if before != after {
			t.Errorf("changing %s changed write_auth from %s to %s; the tag covers no such field",
				name, hex.EncodeToString(before[:]), hex.EncodeToString(after[:]))
		}
		if !VerifyWriteAuth(key, base.nonce, record) {
			t.Errorf("changing %s made the record's own write_auth stop verifying", name)
		}
	}
}

// record_id, on its own, because spec A section 5.1 names it and because it is the field a
// reader is most likely to add to the block: it is on the record, it is what the server
// indexes by, and it looks like something worth authenticating. It is not. The server assigns
// it after acceptance, so at the moment the client macs there is no value to cover, and on the
// read path spec A section 2.4 has the server rebuild record_bytes from its columns — a
// preimage that carried the id would be one the client could never have produced.
func TestRecordIdDoesNotAffectTheTag(t *testing.T) {
	key := WriteKey(writeAuthKatStorageRoot())
	input := writeAuthKatEphInput()
	record := writeAuthRecordOf(key, input)
	if record.RecordId != 0 {
		t.Fatalf("the record starts with id %d, so this observes nothing", record.RecordId)
	}
	before := record.WriteAuth
	for _, id := range []uint64{1, 2, 0xFFFFFFFF, 0xFFFFFFFFFFFFFFFF} {
		record.RecordId = id
		after := ComputeWriteAuth(key, input.nonce, &record.Header, record.CtHead, record.Header.ServerAttachment)
		if before != after {
			t.Errorf("record id %d changed write_auth from %s to %s", id, hex.EncodeToString(before[:]), hex.EncodeToString(after[:]))
		}
		if !VerifyWriteAuth(key, input.nonce, record) {
			t.Errorf("record id %d made the record stop verifying", id)
		}
	}
}

// ── the refusals ────────────────────────────────────────────────────────────────────

// The positive control the whole section rests on: the tag the computing half produced
// verifies, for every vector this file builds.
//
// Without it every rejection below passes on a VerifyWriteAuth that answers false always,
// which is the one implementation that refuses every forgery and every real record alike.
func TestVerifyWriteAuthAcceptsTheTagComputeProduced(t *testing.T) {
	key := WriteKey(writeAuthKatStorageRoot())
	for _, vector := range []struct {
		name  string
		input writeAuthInput
	}{
		{name: "the eph blob-rung commit", input: writeAuthKatEphInput()},
		{name: "the ordinary durable record", input: writeAuthKatOrdinaryInput()},
		{name: "the base input", input: writeAuthBaseInput()},
	} {
		record := writeAuthRecordOf(key, vector.input)
		if !VerifyWriteAuth(key, vector.input.nonce, record) {
			t.Errorf("%s: the record's own write_auth does not verify", vector.name)
		}
	}
}

// Every single bit of the tag, flipped, in all two hundred and fifty six positions.
//
// The class here is the tag's own width rather than a handful of interesting octets, because
// a comparison that stopped early — at sixteen octets, at the first zero, at the first
// difference — passes on every flip inside the part it reads and fails only outside it.
// Walking every position is what puts the truncation class inside the observation instead of
// beside it.
func TestVerifyWriteAuthRefusesAFlippedBitInEveryTagPosition(t *testing.T) {
	key := WriteKey(writeAuthKatStorageRoot())
	input := writeAuthKatEphInput()
	record := writeAuthRecordOf(key, input)
	correct := record.WriteAuth
	flips := 0
	for index := range correct {
		for bit := range 8 {
			record.WriteAuth = correct
			record.WriteAuth[index] ^= 1 << bit
			if VerifyWriteAuth(key, input.nonce, record) {
				t.Errorf("a tag with octet %d bit %d flipped verifies: %s", index, bit, hex.EncodeToString(record.WriteAuth[:]))
			}
			flips++
		}
	}
	if want := 8 * len(correct); flips != want {
		t.Errorf("%d flips were tried, want %d", flips, want)
	}
	// and the unflipped tag still verifies, so the loop above is refusing forgeries rather
	// than refusing everything
	record.WriteAuth = correct
	if !VerifyWriteAuth(key, input.nonce, record) {
		t.Error("the unflipped tag does not verify")
	}
}

// The key and the nonce, refused from the other side. The read key of the same storage root is
// the one to watch: it is thirty two octets of the same shape, derived from the same secret,
// and only the label between them.
func TestVerifyWriteAuthRefusesAWrongKeyAndAWrongNonce(t *testing.T) {
	root := writeAuthKatStorageRoot()
	key := WriteKey(root)
	input := writeAuthKatEphInput()
	record := writeAuthRecordOf(key, input)
	for _, wrong := range []struct {
		name string
		key  []byte
	}{
		{name: "the write key of another storage root", key: WriteKey(aadRamp(0x50, 32))},
		{name: "the read key of the same storage root", key: ReadKey(root)},
		{name: "the write key with one octet flipped", key: append(append([]byte{}, key[:31]...), key[31]^0xFF)},
	} {
		if VerifyWriteAuth(wrong.key, input.nonce, record) {
			t.Errorf("the record verifies under %s", wrong.name)
		}
	}
	for _, wrong := range []struct {
		name  string
		nonce []byte
	}{
		{name: "another connection's nonce", nonce: aadRamp(0x11, 32)},
		{name: "the nonce with one octet flipped", nonce: append(append([]byte{}, input.nonce[:31]...), input.nonce[31]^0xFF)},
		{name: "the nonce with a trailing octet added", nonce: append(append([]byte{}, input.nonce...), 0x00)},
		{name: "the nonce with its last octet dropped", nonce: input.nonce[:31]},
	} {
		if VerifyWriteAuth(key, wrong.nonce, record) {
			t.Errorf("the record verifies under %s", wrong.name)
		}
	}
}

// Every single input mutation, refused. The class is the computed one, so this is the
// rejection half of the independence tests and it fails on the same day they do.
func TestVerifyWriteAuthRefusesEverySingleInputMutation(t *testing.T) {
	key := WriteKey(writeAuthKatStorageRoot())
	base := writeAuthBaseInput()
	for _, name := range writeAuthCoveredNames() {
		record := writeAuthRecordOf(key, base)
		mutated := base.copy()
		writeAuthMutate(t, name, &mutated)
		record.Header = mutated.header
		record.CtHead = mutated.ctHead
		if VerifyWriteAuth(key, mutated.nonce, record) {
			t.Errorf("a record with %s changed still verifies under the tag of the original", name)
		}
	}
}

// A write_auth of all zeroes verifies as false, which is spec A section 2.4 rather than a
// corner case.
//
// The server rebuilds record_bytes from its stored columns on every read with write_auth left
// zero always, because the mac is over the submitting connection's nonce and there is nothing
// left to reconstruct it from. So every record a client reads back carries this value, and a
// client that treated it as evidence would be treating "the server did not keep this" as "the
// server checked this". False, and not an error and not true.
func TestAZeroWriteAuthVerifiesAsFalse(t *testing.T) {
	key := WriteKey(writeAuthKatStorageRoot())
	for _, vector := range []struct {
		name  string
		input writeAuthInput
	}{
		{name: "the eph blob-rung commit", input: writeAuthKatEphInput()},
		{name: "the ordinary durable record", input: writeAuthKatOrdinaryInput()},
		{name: "the base input", input: writeAuthBaseInput()},
	} {
		record := writeAuthRecordOf(key, vector.input)
		// the assertion is only worth anything if the real tag is not itself zero
		if record.WriteAuth == ([32]byte{}) {
			t.Fatalf("%s: the computed write_auth is all zeroes, so this observes nothing", vector.name)
		}
		record.WriteAuth = [32]byte{}
		if VerifyWriteAuth(key, vector.input.nonce, record) {
			t.Errorf("%s: a record whose write_auth is all zeroes verifies", vector.name)
		}
	}
}

// req_auth's refusals, over the same classes: every bit of the tag, every other op octet, the
// key, the nonce and the request body.
//
// The op is walked over all two hundred and fifty six values rather than the five arms that
// carry a req_auth, and that is deliberate: the field is a u8 the caller supplies, this layer
// validates none of it, and a write that masked or ranged the octet would agree on the five
// legal values and put two different requests under one mac everywhere else.
func TestVerifyRequestAuthRefusesEveryAlteredInput(t *testing.T) {
	root := writeAuthKatStorageRoot()
	readKey := ReadKey(root)
	nonce := aadRamp(0x60, 32)
	requestBytes := aadRamp(0xf0, 6)
	const op uint8 = 13
	tag := ComputeRequestAuth(readKey, nonce, op, requestBytes)
	if !VerifyRequestAuth(readKey, nonce, op, requestBytes, tag[:]) {
		t.Fatal("the tag req_auth just computed does not verify, so every refusal below observes nothing")
	}
	for index := range tag {
		for bit := range 8 {
			forged := tag
			forged[index] ^= 1 << bit
			if VerifyRequestAuth(readKey, nonce, op, requestBytes, forged[:]) {
				t.Errorf("a tag with octet %d bit %d flipped verifies", index, bit)
			}
		}
	}
	for other := range 256 {
		if uint8(other) == op {
			continue
		}
		if VerifyRequestAuth(readKey, nonce, uint8(other), requestBytes, tag[:]) {
			t.Errorf("the tag of op %d verifies under op %d", op, other)
		}
	}
	for _, wrong := range []struct {
		name string
		key  []byte
	}{
		{name: "the read key of another storage root", key: ReadKey(aadRamp(0x50, 32))},
		{name: "the write key of the same storage root", key: WriteKey(root)},
	} {
		if VerifyRequestAuth(wrong.key, nonce, op, requestBytes, tag[:]) {
			t.Errorf("the request verifies under %s", wrong.name)
		}
	}
	for _, wrong := range []struct {
		name  string
		nonce []byte
	}{
		{name: "another connection's nonce", nonce: aadRamp(0x11, 32)},
		{name: "the nonce with a trailing octet added", nonce: append(append([]byte{}, nonce...), 0x00)},
		{name: "the nonce with its last octet dropped", nonce: nonce[:31]},
	} {
		if VerifyRequestAuth(readKey, wrong.nonce, op, requestBytes, tag[:]) {
			t.Errorf("the request verifies under %s", wrong.name)
		}
	}
	for _, wrong := range []struct {
		name  string
		bytes []byte
	}{
		{name: "a request body with one octet flipped", bytes: append(append([]byte{}, requestBytes[:5]...), requestBytes[5]^0xFF)},
		{name: "a request body with a trailing octet added", bytes: append(append([]byte{}, requestBytes...), 0x00)},
		{name: "a request body with its last octet dropped", bytes: requestBytes[:5]},
		{name: "an empty request body", bytes: nil},
	} {
		if VerifyRequestAuth(readKey, nonce, op, wrong.bytes, tag[:]) {
			t.Errorf("the request verifies with %s", wrong.name)
		}
	}
	// the tag as it arrived, at every length it could arrive in. A truncated tag is refused by
	// the comparison itself rather than by a length check in front of it, and a tag that
	// arrived long is refused the same way.
	for _, wrong := range []struct {
		name string
		auth []byte
	}{
		{name: "an empty tag", auth: nil},
		{name: "the first sixteen octets of the tag", auth: tag[:16]},
		{name: "the first thirty one octets of the tag", auth: tag[:31]},
		{name: "the tag with a trailing octet added", auth: append(append([]byte{}, tag[:]...), 0x00)},
		{name: "thirty two zeroes", auth: make([]byte, 32)},
	} {
		if VerifyRequestAuth(readKey, nonce, op, requestBytes, wrong.auth) {
			t.Errorf("the request verifies with %s", wrong.name)
		}
	}
}

// ── domain separation ───────────────────────────────────────────────────────────────

// The two labels disagree inside the shorter of them, which is the whole of why no request
// can be made to look like a record.
//
// aad.go's pair are the same length, so their separation rests on the bytes alone and a
// length boundary does nothing for it. These two are eighteen octets and sixteen, and the
// separation still does not rest on the difference in length: a preimage is a byte string
// with no boundary between the label and what follows, so a label that were a prefix of the
// other would leave the rest of the fields to make up the difference. What rules that out is
// that they differ at index thirteen, before the shorter one ends.
func TestTheTwoLabelsDisagreeInsideTheShorterOne(t *testing.T) {
	shorter := min(len(writeAuthLabel), len(requestAuthLabel))
	if shorter == 0 {
		t.Fatal("one of the labels is empty")
	}
	first := -1
	for i := range shorter {
		if writeAuthLabel[i] != requestAuthLabel[i] {
			first = i
			break
		}
	}
	if first < 0 {
		t.Fatalf("%q is a prefix of %q; the two protocols are separated by nothing but the fields after the label",
			requestAuthLabel[:shorter], writeAuthLabel)
	}
	t.Logf("%q and %q first disagree at index %d, %q against %q",
		writeAuthLabel, requestAuthLabel, first, writeAuthLabel[first], requestAuthLabel[first])
}

// The cross protocol attempt, constructed rather than argued.
//
// The attacker is a client that may pick op and the request body freely and, for the sake of
// giving it the strongest hand this layer's own signature permits, the nonce too. It holds a
// write_auth preimage W and wants a req_auth preimage equal to it, because equal preimages
// under one key are equal tags — which is what a shared secret between the two protocols
// would mean if the labels did not separate them.
//
// Its whole strategy space is one parameter. A req_auth preimage is
//
//	"URmessage/v1/req" ‖ u32(len(n)) ‖ n ‖ u8(op) ‖ u32(len(r)) ‖ r
//
// so once the length of the nonce is chosen every other octet is forced: the nonce must be
// W's octets at that offset, the op octet is W's next one, and the body must be the whole of
// W's tail, because any other length makes the two strings different lengths. So the search
// below walks every nonce length the attacker could choose and is exhaustive over everything
// it could do — there is no cleverer choice left unexplored.
//
// Every attempt fails, and it fails inside the label. That is what the test records: not
// merely that the strings differ, but that they already differ before the sixteenth octet, so
// no choice of anything after the label was ever in play. The empty nonce is not in the space
// at all, because this layer refuses to compute with one.
func TestNoRequestAuthPreimageCanBeAWriteAuthPreimage(t *testing.T) {
	writePreimage := writeAuthPreimageOf(writeAuthKatEphInput())
	const reqOverhead = len(requestAuthLabel) + 4 + 1 + 4
	if len(writePreimage) <= reqOverhead {
		t.Fatalf("the write_auth preimage is %d octets, too short for the search to have any room", len(writePreimage))
	}
	attempts := 0
	for nonceLength := 1; nonceLength <= len(writePreimage)-reqOverhead; nonceLength++ {
		nonceAt := len(requestAuthLabel) + 4
		nonce := writePreimage[nonceAt : nonceAt+nonceLength]
		op := writePreimage[nonceAt+nonceLength]
		requestBytes := writePreimage[nonceAt+nonceLength+1+4:]
		attempt := RequestAuthPreimage(nonce, op, requestBytes)
		attempts++
		if bytes.Equal(attempt, writePreimage) {
			t.Fatalf("a req_auth preimage with a %d octet nonce reproduces a write_auth preimage:\n%s",
				nonceLength, hex.EncodeToString(attempt))
		}
		if len(attempt) != len(writePreimage) {
			t.Errorf("the attempt with a %d octet nonce is %d octets and the target is %d; the search is not exhaustive",
				nonceLength, len(attempt), len(writePreimage))
			continue
		}
		differsAt := -1
		for i := range attempt {
			if attempt[i] != writePreimage[i] {
				differsAt = i
				break
			}
		}
		if differsAt < 0 || len(requestAuthLabel) <= differsAt {
			t.Errorf("the attempt with a %d octet nonce first differs at index %d, outside the %d octet label; the labels are not what defeated it",
				nonceLength, differsAt, len(requestAuthLabel))
		}
	}
	if attempts == 0 {
		t.Fatal("the search tried nothing")
	}
	t.Logf("%d nonce lengths tried, every one refused inside the %d octet label", attempts, len(requestAuthLabel))
}

// The length prefixes are load bearing, demonstrated by removing them.
//
// The labels are what defeat the cross protocol attempt above. Inside one protocol they do
// nothing, and what keeps two different requests from sharing a preimage is the framing: LP
// writes a field's length before its octets, so the boundary between two adjacent fields does
// not depend on what is in either of them.
//
// req_auth is where that is constructible, because two of its three fields are variable and
// the octet between them is the op. Take a nonce n and a request body r, and take the nonce
// one octet shorter with that octet becoming the op and the old op becoming the first octet of
// the body:
//
//	n ‖ op ‖ r      and      n[:len(n)-1] ‖ n[len(n)-1] ‖ (op ‖ r)
//
// Those two are the same string. Under an encoding that wrote the fields end to end they are
// the same preimage and therefore the same mac, so a request the server refused could be
// replayed as one it accepts. Under LP they are two preimages, because each carries the length
// of its own nonce. This asserts both halves: the unframed encodings, built here, collide; the
// real ones do not.
func TestLengthPrefixFramingIsWhatKeepsTwoRequestsApart(t *testing.T) {
	nonce := aadRamp(0x60, 32)
	const op uint8 = 13
	requestBytes := aadRamp(0xf0, 6)

	shortNonce := nonce[:len(nonce)-1]
	shiftedOp := nonce[len(nonce)-1]
	shiftedBytes := append([]byte{op}, requestBytes...)

	unframed := func(n []byte, o uint8, r []byte) []byte {
		out := []byte(requestAuthLabel)
		out = append(out, n...)
		out = append(out, o)
		return append(out, r...)
	}
	if !bytes.Equal(unframed(nonce, op, requestBytes), unframed(shortNonce, shiftedOp, shiftedBytes)) {
		t.Fatal("the two triples do not concatenate to the same string, so this demonstrates nothing")
	}

	one := RequestAuthPreimage(nonce, op, requestBytes)
	other := RequestAuthPreimage(shortNonce, shiftedOp, shiftedBytes)
	if bytes.Equal(one, other) {
		t.Errorf("two requests that differ only in where the field boundaries fall share a preimage:\n%s", hex.EncodeToString(one))
	}
	readKey := ReadKey(writeAuthKatStorageRoot())
	if ComputeRequestAuth(readKey, nonce, op, requestBytes) == ComputeRequestAuth(readKey, shortNonce, shiftedOp, shiftedBytes) {
		t.Error("the two requests share a req_auth")
	}
	if VerifyRequestAuth(readKey, shortNonce, shiftedOp, shiftedBytes, one[:0]) {
		t.Error("an empty tag verifies")
	}
	tag := ComputeRequestAuth(readKey, nonce, op, requestBytes)
	if VerifyRequestAuth(readKey, shortNonce, shiftedOp, shiftedBytes, tag[:]) {
		t.Error("the tag of one request verifies for the other, so the boundary is not authenticated")
	}
}

// The nonce's own length is inside the write_auth preimage, which is the same property from
// the write side, where no shift is constructible because every field after the nonce is
// fixed width.
//
// The shape it rules out is a builder that treated the nonce as the thirty two octets master
// section 9.2 says a real one is — copied into a fixed array, or written raw with the width
// implied. Such a builder agrees with every vector written at thirty two octets and puts two
// different connections' nonces under one preimage as soon as one of them is not.
func TestTheNonceLengthIsInsideTheWriteAuthPreimage(t *testing.T) {
	base := writeAuthBaseInput()
	seen := map[string]int{}
	for _, length := range []int{1, 8, 31, 32, 33, 64} {
		in := base.copy()
		in.nonce = aadRamp(0x60, length)
		preimage := writeAuthPreimageOf(in)
		if other, collided := seen[hex.EncodeToString(preimage)]; collided {
			t.Errorf("a %d octet nonce and a %d octet nonce share a preimage", length, other)
			continue
		}
		seen[hex.EncodeToString(preimage)] = length
		// and the length is written before the octets, where LP puts it
		want := append([]byte(writeAuthLabel), 0x00, 0x00, 0x00, byte(length))
		if !bytes.HasPrefix(preimage, want) {
			t.Errorf("a %d octet nonce is not preceded by its own length: %s", length, hex.EncodeToString(preimage[:len(want)]))
		}
	}
}

// ── the refusals the signatures cannot report ───────────────────────────────────────

// One call's panic, as an error. Nothing here recovers to carry on: the value is recovered so
// that the test can name what was caught, which is the whole reason the panic carries a
// wrapped sentinel rather than a string.
func writeAuthPanicOf(t testing.TB, what string, call func()) error {
	t.Helper()
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		call()
	}()
	if recovered == nil {
		t.Errorf("%s did not refuse", what)
		return nil
	}
	err, isError := recovered.(error)
	if !isError {
		t.Errorf("%s panicked with %v, which is not an error and cannot be matched on", what, recovered)
		return nil
	}
	return err
}

// The empty nonce, refused on every path that computes and answered false on every path that
// verifies. Spec A section 5.7: this layer takes the nonce as an opaque byte string and
// refuses to compute with an empty one.
//
// The split is the decision this file is here to record, and both halves are asserted because
// each is a different claim. The computing half must not hand back a zero tag, because a zero
// tag is a value a caller can mistake for an authenticator — guardrail G7 arriving as a value
// rather than as a log line — and every record that lost its nonce would carry the same one.
// The verifying half must not panic, because the server's nonce is connection state and a
// client that sends its first request before its Hello reaches a connection that has none: a
// panic there is a client that can stop the process.
func TestNothingComputesUnderAnEmptyNonce(t *testing.T) {
	key := WriteKey(writeAuthKatStorageRoot())
	input := writeAuthKatEphInput()
	header := input.header
	for _, empty := range []struct {
		name  string
		nonce []byte
	}{
		{name: "a nil nonce", nonce: nil},
		{name: "a zero length nonce", nonce: []byte{}},
	} {
		for _, call := range []struct {
			name string
			call func()
		}{
			{name: "WriteAuthPreimage", call: func() {
				WriteAuthPreimage(empty.nonce, &header, input.ctHead, header.ServerAttachment)
			}},
			{name: "ComputeWriteAuth", call: func() {
				ComputeWriteAuth(key, empty.nonce, &header, input.ctHead, header.ServerAttachment)
			}},
			{name: "RequestAuthPreimage", call: func() { RequestAuthPreimage(empty.nonce, 13, aadRamp(0xf0, 6)) }},
			{name: "ComputeRequestAuth", call: func() { ComputeRequestAuth(key, empty.nonce, 13, aadRamp(0xf0, 6)) }},
		} {
			what := call.name + " with " + empty.name
			if err := writeAuthPanicOf(t, what, call.call); err != nil && !errors.Is(err, ErrServerNonceEmpty) {
				t.Errorf("%s refused with %v, want %v", what, err, ErrServerNonceEmpty)
			}
		}
		record := writeAuthRecordOf(key, input)
		if VerifyWriteAuth(key, empty.nonce, record) {
			t.Errorf("VerifyWriteAuth accepts a record under %s", empty.name)
		}
		tag := ComputeRequestAuth(ReadKey(writeAuthKatStorageRoot()), input.nonce, 13, aadRamp(0xf0, 6))
		if VerifyRequestAuth(ReadKey(writeAuthKatStorageRoot()), empty.nonce, 13, aadRamp(0xf0, 6), tag[:]) {
			t.Errorf("VerifyRequestAuth accepts a request under %s", empty.name)
		}
	}
}

// A key that is not the thirty two octets both derivations produce, refused the same way.
//
// The last case is the one this rule exists for. HMAC takes a key of any length, so without a
// refusal a server that looked up a missing write key and passed the nil it got back would
// compute a mac under the empty key — and a client that had derived nothing would compute the
// same one. Two ends holding no key at all would authenticate, and every check either of them
// ran would pass. The tag is built here with crypto/hmac directly, because this package will
// not compute one.
func TestNothingComputesUnderAKeyThatIsNotThirtyTwoOctets(t *testing.T) {
	input := writeAuthKatEphInput()
	header := input.header
	for _, wrong := range []struct {
		name string
		key  []byte
	}{
		{name: "a nil key", key: nil},
		{name: "a zero length key", key: []byte{}},
		{name: "a sixteen octet key", key: aadRamp(0x01, 16)},
		{name: "a thirty one octet key", key: aadRamp(0x01, 31)},
		{name: "a thirty three octet key", key: aadRamp(0x01, 33)},
	} {
		for _, call := range []struct {
			name string
			call func()
		}{
			{name: "ComputeWriteAuth", call: func() {
				ComputeWriteAuth(wrong.key, input.nonce, &header, input.ctHead, header.ServerAttachment)
			}},
			{name: "ComputeRequestAuth", call: func() { ComputeRequestAuth(wrong.key, input.nonce, 13, aadRamp(0xf0, 6)) }},
		} {
			what := call.name + " with " + wrong.name
			if err := writeAuthPanicOf(t, what, call.call); err != nil && !errors.Is(err, ErrAuthKeyLength) {
				t.Errorf("%s refused with %v, want %v", what, err, ErrAuthKeyLength)
			}
		}
		// the bypass, built outside this package and offered to it
		mac := hmac.New(sha256.New, wrong.key)
		mac.Write(WriteAuthPreimage(input.nonce, &header, input.ctHead, header.ServerAttachment))
		record := &Record{Header: header, CtHead: input.ctHead, WriteAuth: [32]byte(mac.Sum(nil))}
		if VerifyWriteAuth(wrong.key, input.nonce, record) {
			t.Errorf("a record macd under %s verifies under the same key; two ends holding nothing would authenticate", wrong.name)
		}
		reqMac := hmac.New(sha256.New, wrong.key)
		reqMac.Write(RequestAuthPreimage(input.nonce, 13, aadRamp(0xf0, 6)))
		if VerifyRequestAuth(wrong.key, input.nonce, 13, aadRamp(0xf0, 6), reqMac.Sum(nil)) {
			t.Errorf("a request macd under %s verifies under the same key", wrong.name)
		}
	}
}

// The verifiers a behavioural test covers, held to the class the syntax tree gate derives.
//
// A test that calls a function has to name it — there is no walking the tree into a call — so
// the class is asserted here rather than iterated: the names a test covers must be exactly the
// Verify prefixed functions the scan finds in this package's production source. Spec A section
// 5.7 already names a third verifier that is not written yet, VerifyRecoveryProof, and on the
// day it is declared every test that goes through this fails until it covers that one too. It
// is the whole of what stands between a rule about verifiers and a rule about the two
// verifiers somebody remembered.
func authAssertVerifiersCovered(t testing.TB, covered ...string) {
	t.Helper()
	derived := authVerifierNames(mustScanAuthSources(t, authOwnScanDir))
	slices.Sort(covered)
	if !slices.Equal(derived, covered) {
		t.Fatalf("this test covers %v and the package declares %v; a verifier outside the covered set is one nothing here observes",
			covered, derived)
	}
}

// No tag at all verifies under a key that is not the thirty two octets both derivations
// produce, and the all zero tag is the one that had to be written down.
//
// This is the other half of the test above, and it is the half that observes the refusal rather
// than the key that was refused. That one builds the correct mac under a short key and asserts
// false, which a verifier that dropped authTag's error answers too: the tag such a verifier
// computes on a refused key is the zero tag, and the zero tag is not the mac it was offered.
// What observes the dropped error is an attacker's own tag against a refused key — and spec A
// section 2.4 is what hands the attacker the tag to pick, because the server rebuilds
// record_bytes from its stored columns with write_auth left zero on every read, so an all zero
// tag is the value every record any client has ever read back already carries. A verifier that
// answered true to that under a key it had itself refused would accept all of them, from a
// caller holding no key at all.
//
// The lengths are walked rather than listed, out to twice the width. "Not thirty two octets" is
// the class; five examples of it are five examples.
func TestNoTagVerifiesUnderAKeyThatIsNotThirtyTwoOctets(t *testing.T) {
	authAssertVerifiersCovered(t, "VerifyRequestAuth", "VerifyWriteAuth")
	root := writeAuthKatStorageRoot()
	writeKey := WriteKey(root)
	readKey := ReadKey(root)
	input := writeAuthKatEphInput()
	requestBytes := aadRamp(0xf0, 6)
	const op uint8 = 13

	// the positive control, first: both verifiers say true under the key the derivations
	// produce. Without it every refusal below is satisfied by a verifier that answers false to
	// everything it is ever given.
	record := writeAuthRecordOf(writeKey, input)
	if !VerifyWriteAuth(writeKey, input.nonce, record) {
		t.Fatal("the record does not verify under its own write key, so every refusal below observes nothing")
	}
	requestTag := ComputeRequestAuth(readKey, input.nonce, op, requestBytes)
	if !VerifyRequestAuth(readKey, input.nonce, op, requestBytes, requestTag[:]) {
		t.Fatal("the request does not verify under its own read key, so every refusal below observes nothing")
	}

	// the tags an attacker gets to choose. What matters is that the choice is the attacker's
	// and not that any of them is plausible: a verifier that computes nothing under a refused
	// key compares against whatever its own failure left behind, and the first of these is that
	// value.
	tags := []struct {
		name string
		tag  [32]byte
	}{
		{name: "an all zero tag, which spec A section 2.4 makes the read path's own value", tag: [32]byte{}},
		{name: "a tag of thirty two 0xff octets", tag: [32]byte(bytes.Repeat([]byte{0xFF}, authTagBytes))},
		{name: "a ramp", tag: [32]byte(aadRamp(0x00, authTagBytes))},
		{name: "the tag the real write key takes over this record", tag: record.WriteAuth},
		{name: "the tag the real read key takes over this request", tag: requestTag},
	}
	type refusedKey struct {
		name string
		key  []byte
	}
	refused := []refusedKey{{name: "a nil key", key: nil}}
	for length := range 2*authKeyBytes + 1 {
		if length == authKeyBytes {
			continue
		}
		refused = append(refused, refusedKey{name: fmt.Sprintf("a %d octet key", length), key: aadRamp(0x01, length)})
	}
	for _, wrong := range refused {
		for _, chosen := range tags {
			forged := &Record{Header: input.header, CtHead: input.ctHead, WriteAuth: chosen.tag}
			if VerifyWriteAuth(wrong.key, input.nonce, forged) {
				t.Errorf("VerifyWriteAuth accepts %s under %s", chosen.name, wrong.name)
			}
			if VerifyRequestAuth(wrong.key, input.nonce, op, requestBytes, chosen.tag[:]) {
				t.Errorf("VerifyRequestAuth accepts %s under %s", chosen.name, wrong.name)
			}
		}
		// and the tag at the lengths a verifier that had computed nothing makes easy to hit,
		// the empty one among them
		for _, arrived := range [][]byte{nil, {}, make([]byte, authTagBytes)} {
			if VerifyRequestAuth(wrong.key, input.nonce, op, requestBytes, arrived) {
				t.Errorf("VerifyRequestAuth accepts a %d octet tag under %s", len(arrived), wrong.name)
			}
		}
	}
}

// The mac of the empty preimage under each of the two keys the pinned storage root derives.
//
// It is the value a verifier that dropped the preimage builder's error would be checking
// against, because a refused build hands back no bytes at all. It is pinned rather than only
// computed for the reason every other vector here is: a test anchored to a number says
// something a test anchored to whatever this package just produced does not. Taken the same way
// as the rest — HMAC-SHA-256 under the pinned keys over a message of zero length, outside this
// package:
//
//	HMAC-SHA-256(9902690f…1a4738b9, "") = 698fb81c…253b7234
//	HMAC-SHA-256(74f60668…d803ee66, "") = b132d1ce…039410eb
const (
	writeAuthEmptyPreimageTagHex   = "698fb81c23a7a18e8371f9030f11b374f46cf15bf0a838b430f09af3253b7234"
	requestAuthEmptyPreimageTagHex = "b132d1cec040fe093e1c26ca1241388a636b028fe90fc3247bc426fe039410eb"
)

// No tag verifies on an input that has no preimage, and the tag of the empty preimage least of
// all.
//
// Every refusal the builders can reach is an input there are no bytes to mac for: an empty
// nonce, and a class and eph bucket pair the wire has no octet for. A verifier that dropped the
// builder's error would mac the nothing it got back instead, and the consequence is not a wrong
// answer on an odd input — it is a second authenticator this package never meant to have.
// HMAC(write_key, "") names no nonce, no group, no epoch and no record. Every member of the
// group holds write_key, so any of them builds that tag offline, and a server that accepted it
// would accept it on every connection that has not yet issued a nonce, for every record at
// once. The nonce is in the preimage precisely so that a tag from one connection cannot be
// replayed onto another, and this is that defence deleted.
//
// The refused headers are the second half of it. A class and bucket pair with no wire octet has
// no preimage either, so a verifier that macd the empty one would collapse every such record
// onto a single tag: one forgery that fits all of them.
//
// None of it is observed by the refusal tests that already exist. They offer a correct-key
// correct-preimage tag and get a mismatch, which the collapsed verifier produces as well —
// its tag is over the empty preimage rather than over theirs, so it disagrees with them by
// luck. What observes it is the empty preimage's own tag, which is why that one is built here
// out of crypto/hmac and pinned above.
func TestNoTagVerifiesOnAnInputThatHasNoPreimage(t *testing.T) {
	authAssertVerifiersCovered(t, "VerifyRequestAuth", "VerifyWriteAuth")
	root := writeAuthKatStorageRoot()
	writeKey := WriteKey(root)
	readKey := ReadKey(root)
	input := writeAuthKatEphInput()
	requestBytes := aadRamp(0xf0, 6)
	const op uint8 = 13

	// the tag of no bytes at all, taken with crypto/hmac because this package will not build
	// it, and checked against the pinned value so the test is anchored to a number rather than
	// to what it just computed
	emptyPreimageTag := func(key []byte) [32]byte {
		mac := hmac.New(sha256.New, key)
		mac.Write(nil)
		return [32]byte(mac.Sum(nil))
	}
	writeCollapsed := emptyPreimageTag(writeKey)
	readCollapsed := emptyPreimageTag(readKey)
	if hex.EncodeToString(writeCollapsed[:]) != writeAuthEmptyPreimageTagHex {
		t.Fatalf("the empty preimage's tag under the write key is %s, want %s",
			hex.EncodeToString(writeCollapsed[:]), writeAuthEmptyPreimageTagHex)
	}
	if hex.EncodeToString(readCollapsed[:]) != requestAuthEmptyPreimageTagHex {
		t.Fatalf("the empty preimage's tag under the read key is %s, want %s",
			hex.EncodeToString(readCollapsed[:]), requestAuthEmptyPreimageTagHex)
	}

	// the positive control: the same verifier and the same key, on an input that does have a
	// preimage
	if !VerifyWriteAuth(writeKey, input.nonce, writeAuthRecordOf(writeKey, input)) {
		t.Fatal("the record does not verify under its own write key, so every refusal below observes nothing")
	}

	// every header the builder refuses, which is every one the join has no wire octet for
	illegalBucket := input.header
	illegalBucket.RetentionClass = RetentionDurable
	illegalBucket.EphBucket = 3
	pastTheLadder := input.header
	pastTheLadder.EphBucket = ephBucketMax + 1
	unknownClass := input.header
	unknownClass.RetentionClass = RetentionClass(9)
	unknownClass.EphBucket = 0

	for _, refused := range []struct {
		name   string
		nonce  []byte
		header RecordHeader
	}{
		{name: "a nil nonce", nonce: nil, header: input.header},
		{name: "a zero length nonce", nonce: []byte{}, header: input.header},
		{name: "an eph bucket on a durable class", nonce: input.nonce, header: illegalBucket},
		{name: "an eph bucket past the ladder", nonce: input.nonce, header: pastTheLadder},
		{name: "a retention class the wire has no octet for", nonce: input.nonce, header: unknownClass},
	} {
		// the builder really does refuse this input, or the assertion under it is about an
		// input that has a preimage after all
		if _, err := writeAuthPreimage(refused.nonce, &refused.header, input.ctHead, refused.header.ServerAttachment); err == nil {
			t.Errorf("the builder accepts %s, so the refusal below observes nothing", refused.name)
		}
		record := &Record{Header: refused.header, CtHead: input.ctHead, WriteAuth: writeCollapsed}
		if VerifyWriteAuth(writeKey, refused.nonce, record) {
			t.Errorf("VerifyWriteAuth accepts the empty preimage's tag on %s; write_auth is then a value any member builds offline and replays onto every connection at once",
				refused.name)
		}
	}

	// the read path's own shape of it. A req_auth over no preimage is independent of the nonce,
	// of the op and of the request body all three, so one tag would authorise every read on
	// every nonce-less connection — and the loop is what says that rather than a single call.
	for _, empty := range [][]byte{nil, {}} {
		if _, err := requestAuthPreimage(empty, op, requestBytes); err == nil {
			t.Errorf("the builder accepts a %d octet nonce, so the refusals below observe nothing", len(empty))
		}
		for _, other := range []uint8{0, 13, 14, 16, 17, 19, 255} {
			for _, body := range [][]byte{nil, requestBytes, aadRamp(0x01, 1), aadRamp(0x77, 64)} {
				if VerifyRequestAuth(readKey, empty, other, body, readCollapsed[:]) {
					t.Errorf("VerifyRequestAuth accepts the empty preimage's tag for op %d over a %d octet body under a %d octet nonce",
						other, len(body), len(empty))
				}
			}
		}
	}
}

// The other two refusals the builder can reach, and the corresponding false on the verifying
// side.
//
// The attachment disagreement is the one worth having. It is one value carried in two places —
// the argument spec A section 12.1 publishes, and the header field the parser fills in — and a
// caller that lets them differ macs a record under a preimage the server will not reproduce.
// It is refused rather than resolved for the reason AADHead refuses it: preferring one of the
// two would make a record's fate depend on which one the function happened to pick, and the
// two preimages over one record would then disagree with each other.
func TestTheWriteAuthBuilderRefusesAHeaderItCannotUse(t *testing.T) {
	key := WriteKey(writeAuthKatStorageRoot())
	input := writeAuthKatEphInput()
	header := input.header

	err := writeAuthPanicOf(t, "WriteAuthPreimage with a nil header", func() {
		WriteAuthPreimage(input.nonce, nil, input.ctHead, nil)
	})
	if err != nil && !errors.Is(err, ErrRecordHeaderNil) {
		t.Errorf("a nil header was refused with %v, want %v", err, ErrRecordHeaderNil)
	}
	if VerifyWriteAuth(key, input.nonce, nil) {
		t.Error("VerifyWriteAuth accepts a nil record")
	}

	for _, wrong := range []struct {
		name       string
		attachment []byte
	}{
		{name: "no attachment while the header carries one", attachment: nil},
		{name: "a different attachment from the header's", attachment: []byte{0xde, 0xad, 0xbe, 0xee}},
		{name: "a longer attachment than the header's", attachment: []byte{0xde, 0xad, 0xbe, 0xef, 0x00}},
	} {
		err := writeAuthPanicOf(t, "WriteAuthPreimage with "+wrong.name, func() {
			WriteAuthPreimage(input.nonce, &header, input.ctHead, wrong.attachment)
		})
		if err != nil && !errors.Is(err, ErrServerAttachmentMismatch) {
			t.Errorf("%s was refused with %v, want %v", wrong.name, err, ErrServerAttachmentMismatch)
		}
	}

	// a class and bucket pair the wire has no octet for has no preimage either, and the
	// refusal is the join's rather than this file's
	illegal := header
	illegal.RetentionClass = RetentionDurable
	illegal.EphBucket = 3
	err = writeAuthPanicOf(t, "WriteAuthPreimage with a bucket on a non eph class", func() {
		WriteAuthPreimage(input.nonce, &illegal, input.ctHead, illegal.ServerAttachment)
	})
	if err != nil && !errors.Is(err, ErrEphBucketOnNonEphClass) {
		t.Errorf("an eph bucket on a durable class was refused with %v, want %v", err, ErrEphBucketOnNonEphClass)
	}
	if VerifyWriteAuth(key, input.nonce, &Record{Header: illegal, CtHead: input.ctHead}) {
		t.Error("VerifyWriteAuth accepts a record whose class and bucket the wire has no octet for")
	}

	// and a nil attachment and an empty one are the one value LP cannot tell apart, on both
	// sides of the builder
	ordinary := writeAuthKatOrdinaryInput()
	withNil := WriteAuthPreimage(ordinary.nonce, &ordinary.header, ordinary.ctHead, nil)
	empty := ordinary
	empty.header.ServerAttachment = []byte{}
	withEmpty := WriteAuthPreimage(empty.nonce, &empty.header, empty.ctHead, []byte{})
	if !bytes.Equal(withNil, withEmpty) {
		t.Errorf("a nil attachment and an empty one give different preimages:\n%s\n%s",
			hex.EncodeToString(withNil), hex.EncodeToString(withEmpty))
	}
}

// ── the syntax tree gates ───────────────────────────────────────────────────────────

// This package's own directory, and the control fixture's. The fixture is under testdata,
// which is what keeps it out of every other scan in this repository and unbuildable by the
// go tool.
const (
	authOwnScanDir     = "."
	authControlScanDir = "testdata/writeauth"
)

// Every directory the constant time rules run over, which is a LIST because the split of
// connect/message made it one and because a single directory cannot be added to.
//
// The scope question, answered separately from the class question as R3a requires. The CLASS
// is derived twice over -- the comparators off the scanned code's own imports, the verifiers
// off every function whose name begins with Verify -- and neither derivation says anything
// about WHERE to look. Until the split the answer was one directory, written as authScanDir,
// and it was right by accident: there was one package. After it there are two halves of the
// record layer, connect/messagegroup holds the key schedule, both ratchets, the sealer and
// the epoch fan-out, and a scope of "." would have covered the half with two comparisons in
// it and left the half where every new comparison of this plan is going to be written.
// Nothing would have reported that: this file's rules find no offender in a directory they
// never open, which is the same clean report a genuinely clean tree gets.
//
// Each root is scanned SEPARATELY and never merged into one authScan, which matters for one
// rule in particular. TestAVerifierReachesOutOfItsPackageOnlyForTheConstantTimeComparison is
// about calls leaving a verifier's own package, so a merged scan would read a call from
// connect/message into connect/messagegroup as a local one and stop reporting it. Merging
// would have widened the coverage claim and narrowed the rule at the same time.
//
// The list is written down and the check on it is derived:
// TestEveryPackageBuiltOnThisOneIsUnderTheConstantTimeGate walks this module for the
// production packages that import this one and requires each to be a root here. That half is
// silent today on purpose and it says so -- connect/messagegroup does not import this package
// until the first file that calls message.AADHead -- and it arms itself on that commit.
//
// The honest consequence of that, measured and written down rather than left to be
// rediscovered: narrowing this back to {authOwnScanDir} is SILENT today. Nothing in this
// suite goes red for it, because the derived check above has no importer to demand a root
// for and because connect/messagegroup contributes no verifier and no comparator to either
// rule yet. What the narrowing would remove is coverage of the half of the record layer that
// every new comparison of M1 is going to be written in. So this root stands on the argument
// above and not on a test's say-so, and it stops being unfalsifiable on the same commit that
// arms the derived check.
var authScanRoots = []string{authOwnScanDir, "../messagegroup"}

// Every root, scanned on its own.
func mustScanAuthRoots(t testing.TB) []authScan {
	t.Helper()
	if len(authScanRoots) == 0 {
		t.Fatal("the gates read no root at all, so every rule below cleared every function having read nothing")
	}
	scans := []authScan{}
	for _, root := range authScanRoots {
		scans = append(scans, mustScanAuthSources(t, root))
	}
	return scans
}

// One directory's declarations: every function and method in it, the package level constants
// beside them, and where each came from.
//
// A name maps to a list of declarations rather than to one, because a function and a method
// can share a name, and every declaration under a name is read. Over reporting is the safe
// direction for a ban list: a gate that merges two unrelated functions reports a violation
// that is not there, which is loud, while one that picks the wrong declaration reports nothing
// and is silent.
type authScan struct {
	dir          string
	fileSet      *token.FileSet
	decls        map[string][]*ast.FuncDecl
	pathOf       map[string][]string
	stringConsts map[string]string
	constNames   map[string]bool
	typeNames    map[string]bool
	imports      []authImport
	fileCount    int
}

// One import as a call site sees it. The name is what an expression qualifies with, and the
// path is where the comparator class reads that package's own declarations from, so the two
// have to travel together: a package imported under an alias is banned under the alias, and a
// package with no import is a package with no call.
type authImport struct {
	name string
	path string
}

// Walks one directory and parses every go file in it, without comments.
//
// Comments are not parsed at all, which is the property the documented.go fixture asserts
// rather than assumes: the comment that teaches a rule is the comment that names what the rule
// bans, and a gate that fires on the sentence teaching it is a gate the next contributor
// deletes.
//
// Test files are out of scope. Every rule here is about what ships — a test that calls
// bytes.Equal to assert something about a tag is not a second comparison in the code an
// auditor reads — and this file itself would otherwise be the loudest violation of both gates.
//
// A directory that cannot be read, one holding no go source, and a file that does not parse
// are all errors rather than empty results, because each one produces a scan that reports
// every rule clean having read nothing. The error is returned rather than failed on so the
// refusal itself can be tested.
func scanAuthSources(dir string) (authScan, error) {
	scan := authScan{
		dir:          dir,
		fileSet:      token.NewFileSet(),
		decls:        map[string][]*ast.FuncDecl{},
		pathOf:       map[string][]string{},
		stringConsts: map[string]string{},
		constNames:   map[string]bool{},
		typeNames:    map[string]bool{},
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return scan, fmt.Errorf("read %s: %w", dir, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.ToSlash(filepath.Join(dir, name))
		file, err := parser.ParseFile(scan.fileSet, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return scan, fmt.Errorf("parse %s: %w", path, err)
		}
		scan.fileCount++
		for _, spec := range file.Imports {
			imported, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return scan, fmt.Errorf("parse %s: an import path that is not a string: %s", path, spec.Path.Value)
			}
			name := imported[strings.LastIndex(imported, "/")+1:]
			if spec.Name != nil {
				name = spec.Name.Name
			}
			if !slices.Contains(scan.imports, authImport{name: name, path: imported}) {
				scan.imports = append(scan.imports, authImport{name: name, path: imported})
			}
		}
		for _, decl := range file.Decls {
			switch typed := decl.(type) {
			case *ast.FuncDecl:
				if typed.Body == nil {
					continue
				}
				scan.decls[typed.Name.Name] = append(scan.decls[typed.Name.Name], typed)
				scan.pathOf[typed.Name.Name] = append(scan.pathOf[typed.Name.Name], path)
			case *ast.GenDecl:
				switch typed.Tok {
				case token.CONST:
					collectAuthConsts(scan, typed)
				case token.TYPE:
					collectAuthTypes(scan, typed)
				}
			}
		}
	}
	if scan.fileCount == 0 {
		return scan, fmt.Errorf("%s holds no go source; the scan is broken, not the code", dir)
	}
	if len(scan.decls) == 0 {
		return scan, fmt.Errorf("%s holds no function at all, so every rule over it is vacuous", dir)
	}
	return scan, nil
}

// The package level constants of one declaration, with the string valued ones kept by value so
// that a label named through a constant is as visible to the gates as one written out.
func collectAuthConsts(scan authScan, decl *ast.GenDecl) {
	for _, spec := range decl.Specs {
		valueSpec, isValue := spec.(*ast.ValueSpec)
		if !isValue {
			continue
		}
		for i, name := range valueSpec.Names {
			scan.constNames[name.Name] = true
			if len(valueSpec.Values) <= i {
				continue
			}
			literal, isLiteral := valueSpec.Values[i].(*ast.BasicLit)
			if !isLiteral || literal.Kind != token.STRING {
				continue
			}
			value, err := strconv.Unquote(literal.Value)
			if err != nil {
				continue
			}
			scan.stringConsts[name.Name] = value
		}
	}
}

// The type names of one declaration, so that a conversion through one is not mistaken for a
// call to something this package does not declare.
func collectAuthTypes(scan authScan, decl *ast.GenDecl) {
	for _, spec := range decl.Specs {
		if typeSpec, isType := spec.(*ast.TypeSpec); isType {
			scan.typeNames[typeSpec.Name.Name] = true
		}
	}
}

// The scan every gate starts from, with a failed walk fatal rather than reported: every
// assertion downstream is meaningless if the source was never read.
func mustScanAuthSources(t testing.TB, dir string) authScan {
	t.Helper()
	scan, err := scanAuthSources(dir)
	if err != nil {
		t.Fatalf("scanning %s: %v", dir, err)
	}
	return scan
}

// Every identifier one function names, across all of its declarations.
//
// Identifiers rather than call expressions, on purpose. A call is the common way to reach a
// function but not the only one: a function value assigned to a variable, passed as an
// argument or stored in a table reaches it just as well, and a walk that only followed
// CallExpr would miss all three. The selector of a method call is an identifier too, so a
// method that reaches a banned function is followed as well.
func authIdentsIn(decls []*ast.FuncDecl) []string {
	named := map[string]bool{}
	for _, decl := range decls {
		ast.Inspect(decl.Body, func(node ast.Node) bool {
			if ident, isIdent := node.(*ast.Ident); isIdent {
				named[ident.Name] = true
			}
			return true
		})
	}
	return slices.Sorted(maps.Keys(named))
}

// Every string literal one function holds.
func authStringsIn(decls []*ast.FuncDecl) []string {
	found := []string{}
	for _, decl := range decls {
		ast.Inspect(decl.Body, func(node ast.Node) bool {
			literal, isLiteral := node.(*ast.BasicLit)
			if !isLiteral || literal.Kind != token.STRING {
				return true
			}
			if value, err := strconv.Unquote(literal.Value); err == nil {
				found = append(found, value)
			}
			return true
		})
	}
	return found
}

// The functions of this package one function names, which are the edges of the call graph.
func authEdgesOf(scan authScan, name string) []string {
	edges := []string{}
	for _, ident := range authIdentsIn(scan.decls[name]) {
		if ident == name {
			continue
		}
		if _, declared := scan.decls[ident]; declared {
			edges = append(edges, ident)
		}
	}
	return edges
}

// Everything reachable from one function, transitively, including itself.
//
// A root that is not declared is fatal rather than an empty set: a rule about the call graph of
// a function that has been renamed is a rule about nothing, and it would report clean.
func authReachableFrom(t testing.TB, scan authScan, root string) map[string]bool {
	t.Helper()
	if _, declared := scan.decls[root]; !declared {
		t.Fatalf("%s is not declared in the scanned source, so a walk from it would report clean having walked nothing", root)
	}
	reached := map[string]bool{root: true}
	frontier := []string{root}
	for 0 < len(frontier) {
		name := frontier[len(frontier)-1]
		frontier = frontier[:len(frontier)-1]
		for _, edge := range authEdgesOf(scan, name) {
			if reached[edge] {
				continue
			}
			reached[edge] = true
			frontier = append(frontier, edge)
		}
	}
	return reached
}

// Every function that names the write key's label, by literal or through a package level
// constant.
//
// This is the class TestReadAuthNeverUsesWriteKey forbids, and it is computed rather than
// written down: a second write key derivation added later is in the class the day it is
// written, whatever it is called, because what puts it there is that it names the label. The
// label itself is the argument rather than a constant of this file, so the same rule can be
// run against the control fixture, whose labels are its own.
func authWriteKeyDerivers(scan authScan, label string) []string {
	derivers := []string{}
	for name := range scan.decls {
		if slices.Contains(authStringsIn(scan.decls[name]), label) {
			derivers = append(derivers, name)
			continue
		}
		for _, ident := range authIdentsIn(scan.decls[name]) {
			if value, isConst := scan.stringConsts[ident]; isConst && value == label {
				derivers = append(derivers, name)
				break
			}
		}
	}
	slices.Sort(derivers)
	return derivers
}

// Spec A section 5.7's own test, met the way that section says to meet it: by walking the call
// graph of ComputeRequestAuth.
//
// The rule is that req_auth is macd under a read key and that this package has no code path
// that macs a request under a write key. A grep would answer a different question — whether the
// name appears — and would miss a deriver reached through three helpers, which is the shape the
// defect actually takes. So the walk is transitive, over edges read out of the syntax tree, and
// the forbidden class is computed from the label rather than from a list of names.
//
// Both entry points of the read path are walked, because either one macing under the wrong key
// is the same failure: a member offline across a single commit for more than sixty seconds
// holds a write key the server has already discarded, and every route out of that condition —
// GroupStatus, Fetch, WrapFetch — is itself a read.
//
// What the walk does not see: a key reached through an interface, through reflection, or out of
// a package this scan does not read. It sees this package's own source, which is where the
// defect would be written.
func TestReadAuthNeverUsesWriteKey(t *testing.T) {
	scan := mustScanAuthSources(t, authOwnScanDir)
	derivers := authWriteKeyDerivers(scan, writeKeyInfo)
	if !slices.Contains(derivers, "WriteKey") {
		t.Fatalf("the derived class of write key derivations is %v, which does not include WriteKey; the rule is looking for the wrong thing", derivers)
	}
	t.Logf("%d go files scanned, %d functions, write key derivations %v", scan.fileCount, len(scan.decls), derivers)
	for _, entry := range []struct {
		root string
		// the chain the walk must have followed for its verdict on this root to mean
		// anything, checked rather than assumed: a walk that reached nothing would clear
		// every root it is ever given
		reaches []string
	}{
		{root: "ComputeRequestAuth", reaches: []string{"RequestAuthPreimage", "requestAuthPreimage", "authTag"}},
		{root: "VerifyRequestAuth", reaches: []string{"requestAuthPreimage", "authTag"}},
	} {
		reachable := authReachableFrom(t, scan, entry.root)
		for _, want := range entry.reaches {
			if !reachable[want] {
				t.Fatalf("the walk from %s reached %v, which does not include %s; it is not following the calls",
					entry.root, slices.Sorted(maps.Keys(reachable)), want)
			}
		}
		for _, deriver := range derivers {
			if reachable[deriver] {
				t.Errorf("%s reaches %s, which derives a key under %q; req_auth is macd under the read key and there is no path to the other one",
					entry.root, deriver, writeKeyInfo)
			}
		}
	}
	// and the write path does reach it, so the rule above is about where the derivation is and
	// not about it having been deleted
	if !slices.Contains(authWriteKeyDerivers(scan, writeKeyInfo), "WriteKey") {
		t.Error("nothing in the package derives a write key at all")
	}
}

// The positive control for the walk. Without it the test above proves nothing: it reports
// clean, and a walk that followed no edges at all reports clean too.
//
// The fixture holds three tainted roots and one clean one. The tainted ones reach the deriver
// through three hops, name the label inline, and name it through a constant — the three shapes
// the rule has to see — and the clean one has the same three hop shape ending at the read key,
// which is what says a root is reported for what it reaches rather than for being a root.
func TestTheCallGraphWalkFlagsTheControlFixture(t *testing.T) {
	control := mustScanAuthSources(t, authControlScanDir)
	derivers := authWriteKeyDerivers(control, "write/v1")
	want := []string{"ComputeRequestAuthByConst", "ComputeRequestAuthByLabel", "WriteKey"}
	if !slices.Equal(derivers, want) {
		t.Fatalf("the derived class over the fixture is %v, want %v", derivers, want)
	}
	for _, root := range []string{"ComputeRequestAuthViaHelper", "ComputeRequestAuthByLabel", "ComputeRequestAuthByConst"} {
		reachable := authReachableFrom(t, control, root)
		reached := []string{}
		for _, deriver := range derivers {
			if reachable[deriver] {
				reached = append(reached, deriver)
			}
		}
		if len(reached) == 0 {
			t.Errorf("the walk clears %s, which reaches a write key derivation; the rule is asleep", root)
		}
	}
	for _, root := range []string{"ComputeRequestAuthClean", "ComputeRequestAuthDocumented"} {
		reachable := authReachableFrom(t, control, root)
		for _, deriver := range derivers {
			if reachable[deriver] {
				t.Errorf("the walk reports %s reaching %s; it flags roots that reach nothing", root, deriver)
			}
		}
	}
	// the three hop case is the one a one level check misses, so the walk is asserted to have
	// actually gone three hops rather than to have guessed
	viaHelper := authReachableFrom(t, control, "ComputeRequestAuthViaHelper")
	for _, hop := range []string{"fixtureMac", "fixturePickKey", "WriteKey", "fixtureExpand"} {
		if !viaHelper[hop] {
			t.Errorf("the walk from ComputeRequestAuthViaHelper did not reach %s", hop)
		}
	}
}

// ── guardrail G8: every comparison of data is constant time ─────────────────────────

// The one comparison this package decides anything with, and the package it comes from.
//
// crypto/subtle is exempt from the derived class below as a whole package rather than by this
// one name. Everything in it answers in a time that depends on the lengths and on nothing else,
// which is the property the guardrail is about, so exempting the package is what keeps a
// verifier that reached for ConstantTimeByteEq from being reported for using the sanctioned
// tool. Nothing else is exempt from anything.
const (
	authConstantTimeComparator = "subtle.ConstantTimeCompare"
	authConstantTimePackage    = "crypto/subtle"
)

// The spellings of "a byte string" and of "one of its elements" in go, which is what the
// classifier below reads a signature for.
//
// These are language spellings and not a list of functions: a comparator is any function of the
// right shape over them, and the shape is what the rule matches. A type parameter is in both
// sets because a generic comparator's arguments are always type parameters — slices.Equal takes
// two S and slices.Index takes an S and an E — and there is no way to tell which of the two it
// is from the signature alone without resolving the constraint.
var (
	authDataShapes   = []string{"[]byte", "[]rune", "string", "any", "interface{}"}
	authScalarShapes = []string{"byte", "rune", "uint8", "int32"}
)

// Whether a parameter's type is the shape of a byte string, or of one of its elements.
func authIsDataShaped(text string, typeParams map[string]bool) bool {
	return typeParams[text] || slices.Contains(authDataShapes, text)
}

func authIsScalarShaped(text string, typeParams map[string]bool) bool {
	return typeParams[text] || slices.Contains(authScalarShapes, text)
}

// The text of one node under the file set it was parsed with.
func authNodeText(fileSet *token.FileSet, node ast.Node) string {
	var out strings.Builder
	if err := printer.Fprint(&out, fileSet, node); err != nil {
		return "an expression that could not be printed"
	}
	return out.String()
}

// One parameter type per parameter, with a group like (a, b []byte) counted twice.
func authParamTypes(fileSet *token.FileSet, decl *ast.FuncDecl) []string {
	found := []string{}
	if decl.Type.Params == nil {
		return found
	}
	for _, field := range decl.Type.Params.List {
		text := authNodeText(fileSet, field.Type)
		for range max(len(field.Names), 1) {
			found = append(found, text)
		}
	}
	return found
}

// The names one function declares as its own type parameters.
func authTypeParamNames(decl *ast.FuncDecl) map[string]bool {
	names := map[string]bool{}
	if decl.Type.TypeParams == nil {
		return names
	}
	for _, field := range decl.Type.TypeParams.List {
		for _, name := range field.Names {
			names[name.Name] = true
		}
	}
	return names
}

// Whether a function answers a question rather than producing a value: a bool or an int is
// somewhere in what it hands back.
func authAnswersAQuestion(fileSet *token.FileSet, decl *ast.FuncDecl) bool {
	if decl.Type.Results == nil {
		return false
	}
	for _, field := range decl.Type.Results.List {
		switch authNodeText(fileSet, field.Type) {
		case "bool", "int":
			return true
		}
	}
	return false
}

// Whether one declaration of another package answers a question about two byte strings, which
// is the whole of what makes a function a comparator here.
//
// The shape: exported, not a method, answering with a bool or an int somewhere, over two
// arguments of the same data shaped type — or over one data shaped argument and a scalar of the
// kind that data holds, which is bytes.IndexByte and slices.Index. It is the shape of
// bytes.Equal, bytes.Compare, bytes.HasPrefix, bytes.HasSuffix, bytes.Contains, bytes.Index,
// bytes.Cut, bytes.EqualFold, slices.Equal, slices.Compare, slices.Contains, maps.Equal,
// reflect.DeepEqual and every strings twin of them, and of none of sha256.Sum256, hkdf.Expand,
// hmac.New, syntax.NewWriter, errors.New or fmt.Errorf.
//
// It over reports where it is unsure and that is the direction it is meant to fail in. It calls
// fmt.Sscanf a comparator, because a function taking two strings and answering an int is
// indistinguishable from one at this depth, and it calls hmac.Equal one although hmac.Equal is
// constant time, because guardrail G8's text is that the comparison is spelled with
// crypto/subtle rather than that it happens to be safe. A member that should not be one is an
// argument with a reviewer at compile time; a member that is missing is the mutant that lived.
func authIsDataComparator(fileSet *token.FileSet, decl *ast.FuncDecl) bool {
	if decl.Recv != nil || decl.Type == nil || !decl.Name.IsExported() {
		return false
	}
	if !authAnswersAQuestion(fileSet, decl) {
		return false
	}
	typeParams := authTypeParamNames(decl)
	params := authParamTypes(fileSet, decl)
	for i, one := range params {
		if !authIsDataShaped(one, typeParams) {
			continue
		}
		for j, other := range params {
			if i == j {
				continue
			}
			if one == other || authIsScalarShaped(other, typeParams) {
				return true
			}
		}
	}
	return false
}

// The module the scanned directory belongs to: where its go.mod is, and what it calls itself.
func authModuleOf(t testing.TB, dir string) (string, string) {
	t.Helper()
	current, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("resolving %s: %v", dir, err)
	}
	for {
		contents, err := os.ReadFile(filepath.Join(current, "go.mod"))
		if err == nil {
			for line := range strings.SplitSeq(string(contents), "\n") {
				if path, isModule := strings.CutPrefix(strings.TrimSpace(line), "module "); isModule {
					return current, strings.TrimSpace(path)
				}
			}
			t.Fatalf("the go.mod above %s names no module", dir)
		}
		parent := filepath.Dir(current)
		if parent == current {
			t.Fatalf("no go.mod above %s, so an import of this module resolves to no directory", dir)
		}
		current = parent
	}
}

// Where one import path's source is read from: the standard library under the toolchain's own
// GOROOT, this module's own tree for a path under its module line, or the module cache for a
// requirement of this module.
//
// A path that resolves to none of the three is fatal rather than skipped. A skipped package is a
// package every call into it is cleared for, silently, which is the shape of gate this file
// exists to not be.
//
// The third arm arrived with connect/messagegroup's first import outside both of the first two.
// It is a WIDENING and not an exemption, and the difference is the whole of why it is written
// this way: this function refused an unresolvable path by fatalling, so the day a scanned root
// imported golang.org/x/crypto/chacha20poly1305 the gate went red and said what it could not
// read. The repair available then was either to teach it to read that package or to step over
// the path -- and stepping over it would have cleared every call into that package for ever,
// which is exactly the silence the fatal exists to prevent. So the resolution is widened, that
// package's comparator surface enters the derived class like every other import's, and nothing
// is excused. The go tool is what resolves it, because a module requirement's directory is the
// go command's answer and no path this file could assemble is: the cache layout, the version
// suffix and any replace directive are all its business. mls/crossplatform_test.go already runs
// the go tool from a test of this tree for the same reason.
func authImportedPackageDir(t testing.TB, scan authScan, path string) string {
	t.Helper()
	candidate := filepath.Join(build.Default.GOROOT, "src", filepath.FromSlash(path))
	if info, err := os.Stat(candidate); err == nil && info.IsDir() {
		return candidate
	}
	root, module := authModuleOf(t, scan.dir)
	if path == module || strings.HasPrefix(path, module+"/") {
		candidate = filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(strings.TrimPrefix(path, module), "/")))
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}
	if resolved := authModuleRequirementDir(scan, path); resolved != "" {
		return resolved
	}
	t.Fatalf("the import %q resolves to no directory this gate can read; the comparators of a package it cannot read are comparators it cannot ban", path)
	return ""
}

// The directory the go command reads one module requirement's source out of, or "".
//
// It answers "" rather than fatalling, so the caller above states the one refusal and a reader
// finds it in one place. Nothing here interprets the module cache's layout: the go command is
// asked where the package is and the answer is used only if it is a directory that exists, so a
// tool that is missing, a module that is not required, or an answer that is not a path all end
// as the caller's fatal rather than as a package quietly cleared.
func authModuleRequirementDir(scan authScan, path string) string {
	command := exec.Command("go", "list", "-f", "{{.Dir}}", "--", path)
	command.Dir = scan.dir
	out, err := command.Output()
	if err != nil {
		return ""
	}
	resolved := strings.TrimSpace(string(out))
	if resolved == "" {
		return ""
	}
	if info, err := os.Stat(resolved); err != nil || !info.IsDir() {
		return ""
	}
	return resolved
}

// Every function declaration of one imported package, read out of that package's own source.
func authImportedPackageDecls(t testing.TB, scan authScan, path string) (*token.FileSet, []*ast.FuncDecl) {
	t.Helper()
	dir := authImportedPackageDir(t, scan, path)
	fileSet := token.NewFileSet()
	decls := []*ast.FuncDecl{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s for %s: %v", dir, path, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fileSet, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parsing %s of %s: %v", name, path, err)
		}
		for _, decl := range file.Decls {
			if function, isFunction := decl.(*ast.FuncDecl); isFunction {
				decls = append(decls, function)
			}
		}
	}
	if len(decls) == 0 {
		t.Fatalf("%s (%s) yielded no declaration at all, so no comparator of it can be in the class", path, dir)
	}
	return fileSet, decls
}

// The comparator class guardrail G8 is about, computed from the source of the packages the
// scanned code imports.
//
// It is derived and not written down, because every list anyone writes understates it. The
// enumeration this replaced held six names, bytes.Equal and bytes.Compare among them, and did
// not hold bytes.HasPrefix — which leaks strictly more than bytes.Equal does, one answer per
// query about how many leading octets matched, and recovers a forged tag in about thirty two
// times two hundred and fifty six tries. bytes.HasSuffix, bytes.Contains, bytes.Index,
// bytes.IndexByte, bytes.Cut, slices.Equal, slices.Compare, slices.Index, slices.Contains,
// strings.HasPrefix, strings.Contains and maps.Equal were all outside it as well. A ban list
// that holds bytes.Compare and not bytes.HasPrefix is the signature of an enumeration; the
// class is the shape, and the shape is what this reads.
//
// The class is derived from the imports of the source being scanned, which is what makes it
// self extending. A comparator cannot be called without its package being imported, so the edit
// that adds the call adds the import, and that package's whole comparator surface enters the
// class on the same run — including a package nothing here has ever imported, and including a
// comparator the standard library has not shipped yet.
func authDataComparators(t testing.TB, scan authScan) []string {
	t.Helper()
	if len(scan.imports) == 0 {
		t.Fatalf("%s imports nothing, so a class derived from its imports is empty and every call is cleared", scan.dir)
	}
	found := []string{}
	for _, imported := range scan.imports {
		if imported.path == authConstantTimePackage {
			continue
		}
		fileSet, decls := authImportedPackageDecls(t, scan, imported.path)
		for _, decl := range decls {
			if authIsDataComparator(fileSet, decl) {
				found = append(found, imported.name+"."+decl.Name.Name)
			}
		}
	}
	slices.Sort(found)
	return slices.Compact(found)
}

// Every name the language itself declares, read out of go/types' universe scope.
//
// It is what a function is allowed to call without leaving its package: len, copy, append,
// panic and the rest of the builtins, and the predeclared type names, which are conversions
// rather than calls. It comes from the toolchain so that a builtin a later go release adds is
// admitted by the release that adds it rather than by an edit here. The only two that compare
// anything are min and max, and they compare ordered numbers rather than tags — a verifier that
// tried to decide a tag with them would still need the equality that the rule below catches.
func authUniverseNames() map[string]bool {
	names := map[string]bool{}
	for _, name := range types.Universe.Names() {
		names[name] = true
	}
	return names
}

// Whether an expression is a constant as far as the rule below is concerned: a literal, nil,
// true, false, a composite literal, or a package level constant of the scanned source.
//
// It is what lets the rule ban an equality between two values without banning the equalities a
// verifier legitimately writes — err != nil, r == nil, and the == 1 that reads the answer out
// of a constant time comparison. A comparison with a constant on one side is a control flow
// decision; a comparison with a value on both sides is a decision about data, and in a verifier
// the data is a tag.
func authIsConstantExpr(scan authScan, expr ast.Expr) bool {
	switch typed := expr.(type) {
	case *ast.BasicLit:
		return true
	case *ast.CompositeLit:
		return true
	case *ast.ParenExpr:
		return authIsConstantExpr(scan, typed.X)
	case *ast.UnaryExpr:
		return authIsConstantExpr(scan, typed.X)
	case *ast.Ident:
		return typed.Name == "nil" || typed.Name == "true" || typed.Name == "false" || scan.constNames[typed.Name]
	}
	return false
}

// The text of one expression, for a failure message that has to show what it found.
func authExprText(scan authScan, expr ast.Expr) string {
	return authNodeText(scan.fileSet, expr)
}

// Every variable time equality decision inside one function's own body.
//
// Two shapes, because the derived comparator class and the general rule each miss what the
// other catches. A named comparator is the shape guardrail G8 names, and the class it is
// matched against is the computed one. An == between two values is the shape no class of
// function names ever sees at all: a tag is a [32]byte, go compares arrays with == for free,
// and tag == record.WriteAuth is a variable time comparison that mentions nothing.
//
// The rule reads the function's own body and not everything it reaches. What covers a
// comparison moved into a helper is the pair of rules either side of it: the comparator class
// is banned across every function this package ships, wherever the helper is, and a verifier
// must reach a constant time comparison transitively, so a tag comparison delegated to a helper
// is still required to be the right one. What none of the three sees is a hand written loop
// inside a helper that returns early on the first octet that differs — no comparator is named,
// no == is written in a verifier, and the constant time comparison is still reached.
func authVariableTimeComparisons(scan authScan, name string, comparators []string) []string {
	found := append(authComparatorCalls(scan, name, comparators), authValueEqualities(scan, name)...)
	slices.Sort(found)
	return found
}

// The first shape: a call to a member of the derived comparator class.
func authComparatorCalls(scan authScan, name string, comparators []string) []string {
	found := []string{}
	for _, decl := range scan.decls[name] {
		ast.Inspect(decl.Body, func(node ast.Node) bool {
			call, isCall := node.(*ast.CallExpr)
			if !isCall {
				return true
			}
			text := authExprText(scan, call.Fun)
			if slices.Contains(comparators, text) {
				found = append(found, text+" at "+scan.fileSet.Position(call.Pos()).String())
			}
			return true
		})
	}
	slices.Sort(found)
	return found
}

// The second: an equality between two values, which names no function at all.
func authValueEqualities(scan authScan, name string) []string {
	found := []string{}
	for _, decl := range scan.decls[name] {
		ast.Inspect(decl.Body, func(node ast.Node) bool {
			binary, isBinary := node.(*ast.BinaryExpr)
			if !isBinary {
				return true
			}
			if binary.Op != token.EQL && binary.Op != token.NEQ {
				return true
			}
			if authIsConstantExpr(scan, binary.X) || authIsConstantExpr(scan, binary.Y) {
				return true
			}
			found = append(found, authExprText(scan, binary)+" at "+scan.fileSet.Position(binary.Pos()).String())
			return true
		})
	}
	slices.Sort(found)
	return found
}

// Every call in one function's own body that leaves this package for anything other than the
// constant time comparison.
//
// This is the comparator class with its bounds taken off, and it is here because a class
// derived from signatures still has a shape and anything outside that shape is outside it.
// bytes.Cut answers "does this tag begin with that one" through three return values;
// strings.Builder answers it through a method; a helper in a package nothing has classified
// answers it however it likes. So for the two functions whose answer is the authentication
// decision, the rule is not that no comparator is called — it is that nothing outside this
// package is called at all, with one exception, and the exception is the comparison the
// guardrail names.
//
// A call to a method on a value is not a call out of the package by this rule, because the
// receiver had to come from somewhere and everything that could produce one here is either a
// call this rule already sees or a value of this package's own. A conversion through a
// predeclared or package level type name is not a call at all.
func authForeignCalls(scan authScan, name string) []string {
	imported := map[string]bool{}
	for _, one := range scan.imports {
		imported[one.name] = true
	}
	universe := authUniverseNames()
	found := []string{}
	for _, decl := range scan.decls[name] {
		ast.Inspect(decl.Body, func(node ast.Node) bool {
			call, isCall := node.(*ast.CallExpr)
			if !isCall {
				return true
			}
			switch callee := call.Fun.(type) {
			case *ast.Ident:
				if _, declared := scan.decls[callee.Name]; declared {
					return true
				}
				if scan.typeNames[callee.Name] || universe[callee.Name] {
					return true
				}
				found = append(found, callee.Name+" at "+scan.fileSet.Position(call.Pos()).String())
			case *ast.SelectorExpr:
				text := authExprText(scan, call.Fun)
				if text == authConstantTimeComparator {
					return true
				}
				qualifier, isIdent := callee.X.(*ast.Ident)
				if !isIdent || !imported[qualifier.Name] {
					return true
				}
				found = append(found, text+" at "+scan.fileSet.Position(call.Pos()).String())
			}
			return true
		})
	}
	slices.Sort(found)
	return found
}

// Every function in the scan whose name begins with Verify, which is the class the verifier
// rules run over.
//
// It is computed from the tree rather than written down, so a fourth verifier — spec A section
// 5.7's VerifyRecoveryProof is the one already named and not yet written — is under the rules
// the day it is declared, with nobody remembering to add it. Guardrail G8's own text names
// three file names instead; a file name is a place, and these rules are about a kind of
// function. The rule below that is about the place is the one G8 spells, and it covers every
// file rather than three.
func authVerifierNames(scan authScan) []string {
	names := []string{}
	for name := range scan.decls {
		if strings.HasPrefix(name, "Verify") {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// Whether one function's body calls a named function, by the text of the call's own callee.
func authCalls(scan authScan, name string, callee string) bool {
	called := false
	for _, decl := range scan.decls[name] {
		ast.Inspect(decl.Body, func(node ast.Node) bool {
			call, isCall := node.(*ast.CallExpr)
			if isCall && authExprText(scan, call.Fun) == callee {
				called = true
			}
			return true
		})
	}
	return called
}

// Whether a constant time comparison is reachable from one function, transitively.
func authReachesConstantTimeCompare(t testing.TB, scan authScan, name string) bool {
	t.Helper()
	for reached := range authReachableFrom(t, scan, name) {
		if authCalls(scan, reached, authConstantTimeComparator) {
			return true
		}
	}
	return false
}

// The verifiers under the gate, per root, and the class they are, reported rather than
// trusted.
//
// The emptiness refusal is asked of the UNION and not of each root, and that is the whole
// difference between this shape and copying the gate into the other package. The class is
// the scope's, not one directory's: connect/messagegroup declares no Verify function and,
// per spec A section 12.1, may never declare one while every published verifier stays here,
// so a per-root refusal would fatal on arrival on correct code -- twice, once on the empty
// set and once on VerifyWriteAuth being absent -- and the only repair available would have
// been to retune the refusal that measures this gate. A root with no verifier contributes
// none and is judged over nothing, which is the truth about it; the day it declares its
// first Verify function it is judged, with nobody remembering to add it anywhere.
func authVerifiersUnderGate(t testing.TB, scans []authScan) map[string][]string {
	t.Helper()
	byRoot := map[string][]string{}
	union := []string{}
	for _, scan := range scans {
		found := authVerifierNames(scan)
		byRoot[scan.dir] = found
		union = append(union, found...)
	}
	if len(union) == 0 {
		t.Fatalf("no root of %v declares a verifier at all, so this gate is reporting clean having read nothing", authScanRoots)
	}
	// the coverage claim, checked rather than assumed: the two this file is about have to be
	// among the ones being judged
	for _, want := range []string{"VerifyWriteAuth", "VerifyRequestAuth"} {
		if !slices.Contains(union, want) {
			t.Fatalf("the gate is judging %v, which does not include %s", union, want)
		}
	}
	return byRoot
}

// authReportAnEmptyComparatorClass says which KIND of empty a root's comparator class is.
//
// The round that built this gate was told not to ship one that passes by reading nothing, and
// the bare line above -- "0 comparators in the derived class: []" -- is indistinguishable
// from one that does. The design behind it is deliberate and is not being undone: the class
// is DERIVED from the scanned code's own imports, which is the property that makes it self
// extending, and a derivation over a package that imports no comparator honestly yields
// none. But a reader has no way to tell that from a scan that broke, and "it is fine, I
// checked" is not a thing a log line can say a year later. So the empty case states what
// kind of empty it is, what will populate it, and whether any half of guardrail G8 is a
// rule in force over that directory today.
//
// It prints the imports it read rather than asserting that it read some, because that count
// is the number the honesty rests on: a class derived from six imports and finding nothing
// is a measurement, while a class derived from zero imports is a broken scan -- which
// authDataComparators refuses one step earlier, and which this line makes visible without
// having to trust that refusal.
//
// AND IT DOES NOT EXECUTE TODAY, which is worth stating where the line is rather than leaving a
// reader to discover it by grepping a -v run for a sentence that never printed. Every root this
// gate scans now has a non-empty comparator class -- connect/message's own, and connect/messagegroup's
// three since m1 wave 1's SealRecord and OpenRecord became the first callers of AADHead, AADBody
// and the write_auth mac -- so the early return above is taken for both and this Logf is
// unreachable. It is kept because the class it describes is the class of a root ADDED LATER, and
// what it must therefore not do is name a directory: a sentence that named connect/messagegroup
// stood here while being about, if it ever fired at all, some other root entirely.
func authReportAnEmptyComparatorClass(t testing.TB, scan authScan, comparators []string) {
	t.Helper()
	if len(comparators) > 0 {
		return
	}
	paths := []string{}
	for _, imported := range scan.imports {
		paths = append(paths, imported.path)
	}
	slices.Sort(paths)
	paths = slices.Compact(paths)
	t.Logf("%s: that class is empty BY CONSTRUCTION and not because the scan read nothing. Its %d imported packages were read (%v) and none of them exports a function that answers a question about two data shaped arguments, which is the whole of what the class is. That makes this an ARMED TRIPWIRE and not a dead gate: a comparator cannot be called without its package being imported, so the edit that first compares data over there brings that package's entire comparator surface into this class on the same run, with nobody remembering to add it. Nothing over THIS directory is a rule in force today, the derived scope check included: TestEveryPackageBuiltOnThisOneIsUnderTheConstantTimeGate walks this module for the production packages that import connect/message, and a check whose derived class is empty never reaches its assertion either -- it arms on the first production file under this root that calls into connect/message. This line says nothing about any OTHER root, and it names none: every root the gate scans reports its own comparator class one line up, so which of them have armed is read there rather than asserted here by a sentence that goes stale. The residual is recorded as M1-50.",
		scan.dir, len(paths), paths)
}

// authReportAnEmptyVerifierSet is the same sentence for the other half of guardrail G8.
//
// authVerifiersUnderGate already refuses an empty UNION, so a scan that read nothing cannot
// reach this line at all. What that refusal does not say is why one ROOT of the union is
// empty, and "0 verifiers under the gate: []" reads like a rule with nothing left to judge
// rather than like a rule waiting for its first subject.
func authReportAnEmptyVerifierSet(t testing.TB, scan authScan, verifiers []string) {
	t.Helper()
	if len(verifiers) > 0 {
		return
	}
	t.Logf("%s: that set is empty BY CONSTRUCTION and not because the scan read nothing -- %d declarations were read there and none of them is named Verify... . Per spec A section 12.1 every published verifier stays in connect/message, so this root is EXPECTED to contribute none, which is why the emptiness refusal is asked of the union over all roots and never of one root: asked per root it would fatal on arrival, on correct code, and the only repair available would have been to retune the thing that measures this gate. The day that directory declares its first Verify function it is judged, with nobody remembering to add it anywhere.",
		scan.dir, len(scan.decls))
}

// Guardrail G8 at the scope its own text gives it: nothing this package ships compares data
// with a variable time call, in any file and in any function.
//
// G8's mechanical half is a ban on a place — "grep gate forbids bytes.Equal in validation.go,
// writeauth.go, framing.go" — and this is that ban with both of its lists computed. The files
// are every production file of the package, because the scan reads the directory rather than
// three names; the comparators are the derived class, because bytes.Equal is one member of it.
// The package obeys it literally: the two attachment comparisons that were bytes.Equal are
// spelled with crypto/subtle, which is why this rule needs no exemption written into it and why
// nothing here has to decide which comparison is over a tag.
func TestNoProductionFunctionComparesDataOutsideConstantTime(t *testing.T) {
	for _, scan := range mustScanAuthRoots(t) {
		comparators := authDataComparators(t, scan)
		t.Logf("%s: %d go files, %d functions, %d imports, %d comparators in the derived class: %v",
			scan.dir, scan.fileCount, len(scan.decls), len(scan.imports), len(comparators), comparators)
		authReportAnEmptyComparatorClass(t, scan, comparators)
		// the comparator half only. The == between two values is the other shape of the same
		// defect and it is asserted over the verifiers rather than here, because two values
		// compared with == are ordinary in a codec and a header and everything else these
		// packages ship, and a gate that fired on all of them would be a gate with exemptions.
		for _, name := range slices.Sorted(maps.Keys(scan.decls)) {
			for _, comparison := range authComparatorCalls(scan, name, comparators) {
				t.Errorf("%s in %s compares data outside constant time: %s; every comparison of data in these packages goes through %s",
					name, scan.dir, comparison, authConstantTimeComparator)
			}
		}
	}
}

// Guardrail G8, the ban half over the verifiers: no verifier decides equality in variable time.
func TestNoVerifierDecidesEqualityInVariableTime(t *testing.T) {
	scans := mustScanAuthRoots(t)
	verifiers := authVerifiersUnderGate(t, scans)
	for _, scan := range scans {
		comparators := authDataComparators(t, scan)
		t.Logf("%s: %d verifiers under the gate: %v", scan.dir, len(verifiers[scan.dir]), verifiers[scan.dir])
		authReportAnEmptyVerifierSet(t, scan, verifiers[scan.dir])
		for _, name := range verifiers[scan.dir] {
			for _, comparison := range authVariableTimeComparisons(scan, name, comparators) {
				t.Errorf("%s in %s decides equality in variable time: %s; every tag comparison goes through %s",
					name, scan.dir, comparison, authConstantTimeComparator)
			}
		}
	}
}

// The same ban with its bounds taken off: a verifier reaches out of this package for the
// constant time comparison and for nothing else.
//
// It is the rule that catches what a class derived from signatures cannot. The mutant this was
// written for kept the constant time comparison and put a fast path in front of it —
// bytes.HasPrefix(carried, tag) && subtle.ConstantTimeCompare(tag, carried) == 1 — which is
// byte for byte the same answer, so no vector, no bit flip walk and no independence test can
// see it, and it leaks the number of leading octets that matched to anyone who can time it.
// Under this rule the fast path is not reported for being a comparator. It is reported for
// being a call out of the package that is not the one exception.
func TestAVerifierReachesOutOfItsPackageOnlyForTheConstantTimeComparison(t *testing.T) {
	scans := mustScanAuthRoots(t)
	verifiers := authVerifiersUnderGate(t, scans)
	for _, scan := range scans {
		for _, name := range verifiers[scan.dir] {
			for _, foreign := range authForeignCalls(scan, name) {
				t.Errorf("%s in %s calls %s; a verifier's answer is decided by %s and by nothing else, and what it needs from elsewhere belongs behind a function of its own package",
					name, scan.dir, foreign, authConstantTimeComparator)
			}
		}
	}
}

// Guardrail G8, the half that says what must be there instead. Banning the wrong comparator is
// not the same as requiring the right one: a verifier that compared nothing at all, or that
// moved its comparison into a helper and then wrote the helper wrong, passes the bans and fails
// here.
func TestEveryVerifierReachesAConstantTimeComparison(t *testing.T) {
	scans := mustScanAuthRoots(t)
	verifiers := authVerifiersUnderGate(t, scans)
	for _, scan := range scans {
		for _, name := range verifiers[scan.dir] {
			if !authReachesConstantTimeCompare(t, scan, name) {
				t.Errorf("%s in %s reaches no %s; a verifier that compares nothing in constant time is not a verifier",
					name, scan.dir, authConstantTimeComparator)
			}
		}
	}
}

// The positive control for all three halves, and the negative one beside them.
//
// The expected sets are exact, so a matcher that widened to flag every function it reads fails
// here as surely as one that stopped matching. The fixture's clean verifiers are the negative
// half — one comparing directly and one through a helper, which is what says the transitive
// requirement really is transitive — and documented.go is the third half again: it names every
// banned act in prose alone, and the gates parse without comments, so a matcher that read the
// text rather than the tree would flag it.
//
// Three of the fixture's violations are the ones the enumeration this replaced let through:
// bytes.HasPrefix in front of a constant time comparison, slices.Equal, and strings.Contains.
// None of the three was on the six name list. All three are in the class the fixture's own
// imports derive, and the assertions below say so by name, because "the class is bigger" is not
// an observation and "the class holds this member" is.
func TestTheConstantTimeGateFlagsTheControlFixture(t *testing.T) {
	control := mustScanAuthSources(t, authControlScanDir)
	comparators := authDataComparators(t, control)
	t.Logf("%d comparators derived from the fixture's %d imports: %v", len(comparators), len(control.imports), comparators)
	// the derived class holds the members the enumeration missed, named one at a time. The six
	// it did hold are bytes.Equal, bytes.Compare, bytes.EqualFold, reflect.DeepEqual,
	// strings.Compare and strings.EqualFold, and every name below is a comparator that was
	// outside it.
	for _, want := range []string{
		"bytes.HasPrefix", "bytes.HasSuffix", "bytes.Contains", "bytes.Index", "bytes.IndexByte",
		"bytes.Cut", "slices.Equal", "slices.Compare", "slices.Index", "slices.Contains",
		"strings.HasPrefix", "strings.Contains",
	} {
		if !slices.Contains(comparators, want) {
			t.Errorf("the derived class does not hold %s, which answers how many leading octets matched", want)
		}
	}
	// and it holds what the enumeration did hold, so the derivation is a superset of it rather
	// than a different set
	for _, want := range []string{"bytes.Equal", "bytes.Compare", "bytes.EqualFold", "strings.Compare", "strings.EqualFold"} {
		if !slices.Contains(comparators, want) {
			t.Errorf("the derived class does not hold %s, which the enumeration it replaced did hold", want)
		}
	}
	// and it does not hold everything it read: the packages the fixture imports are full of
	// functions that are not comparators. The second row is the one that holds the classifier
	// to answering a question rather than to taking two arguments: every one of them takes two
	// byte strings and hands back a third, which is a transformation and not a decision, and a
	// classifier that stopped reading the result type would swallow the lot.
	for _, notAComparator := range []string{
		"bytes.NewReader", "bytes.Repeat", "strings.Join", "slices.Sort", "fmt.Errorf", "fmt.Sprint",
		"bytes.Replace", "bytes.Split", "bytes.TrimPrefix", "bytes.Join", "strings.Replace", "strings.Split", "strings.TrimSuffix",
	} {
		if slices.Contains(comparators, notAComparator) {
			t.Errorf("the derived class holds %s, which answers no question about two byte strings; a class that flags everything bans nothing",
				notAComparator)
		}
	}

	flagged := []string{}
	foreign := []string{}
	uncovered := []string{}
	verifiers := authVerifierNames(control)
	if len(verifiers) == 0 {
		t.Fatal("the fixture declares no verifier, so it controls nothing")
	}
	for _, name := range verifiers {
		if 0 < len(authVariableTimeComparisons(control, name, comparators)) {
			flagged = append(flagged, name)
		}
		if 0 < len(authForeignCalls(control, name)) {
			foreign = append(foreign, name)
		}
		if !authReachesConstantTimeCompare(t, control, name) {
			uncovered = append(uncovered, name)
		}
	}
	wantFlagged := []string{"VerifyByArrayComparison", "VerifyByBytesEqual", "VerifyByPrefixFastPath", "VerifyBySlicesEqual", "VerifyByStringsContains"}
	if !slices.Equal(flagged, wantFlagged) {
		t.Errorf("the variable time rule flagged %v in the fixture, want %v", flagged, wantFlagged)
	}
	wantForeign := []string{"VerifyByBytesEqual", "VerifyByForeignHelper", "VerifyByPrefixFastPath", "VerifyBySlicesEqual", "VerifyByStringsContains"}
	if !slices.Equal(foreign, wantForeign) {
		t.Errorf("the rule over calls out of the package flagged %v in the fixture, want %v", foreign, wantForeign)
	}
	wantUncovered := []string{"VerifyByArrayComparison", "VerifyByBytesEqual", "VerifyBySlicesEqual", "VerifyByStringsContains", "VerifyWithNoComparisonAtAll"}
	if !slices.Equal(uncovered, wantUncovered) {
		t.Errorf("the constant time requirement flagged %v in the fixture, want %v", uncovered, wantUncovered)
	}
	// the two that only one rule sees, stated as the difference rather than left to be read out
	// of the three sets above: the prefix fast path reaches a constant time comparison and is
	// still a violation, and the verifier that asks another package for a second opinion is one
	// though it calls no comparator at all
	if slices.Contains(uncovered, "VerifyByPrefixFastPath") {
		t.Error("VerifyByPrefixFastPath is reported as reaching no constant time comparison; it reaches one, which is why the other two rules have to see it")
	}
	if slices.Contains(flagged, "VerifyByForeignHelper") {
		t.Error("VerifyByForeignHelper is reported as a comparator call; it calls none, and the rule that has to see it is the one about leaving the package")
	}
	// and the clean ones are clean under all three, which is what says the rules do not fire on
	// everything they read
	for _, name := range []string{"VerifyClean", "VerifyCleanThroughAHelper", "VerifyDocumented"} {
		if slices.Contains(flagged, name) || slices.Contains(foreign, name) || slices.Contains(uncovered, name) {
			t.Errorf("%s is flagged, and it compares in constant time", name)
		}
	}
}

// The coverage guarantee, exercised rather than assumed. A directory that is not there and one
// holding no go source both have to be refused: either one hands both gates a clean result they
// did not earn, and the two look identical from the outside to a directory that is genuinely
// clean.
func TestTheAuthScanRefusesADirectoryItCannotCover(t *testing.T) {
	for _, dir := range []string{
		"this-directory-does-not-exist",
		"testdata/fuzz/FuzzParseRecord",
	} {
		if _, err := scanAuthSources(dir); err == nil {
			t.Errorf("scanning %s succeeded; a directory that contributes no source must be refused", dir)
		}
	}
	// and the real ones must pass it, or the refusal above is just "everything fails"
	for _, dir := range append(slices.Clone(authScanRoots), authControlScanDir) {
		scan, err := scanAuthSources(dir)
		if err != nil {
			t.Errorf("scanning %s failed: %v", dir, err)
			continue
		}
		t.Logf("%s: %d go files, %d functions, %d string constants", dir, scan.fileCount, len(scan.decls), len(scan.stringConsts))
	}
	// the gates read production source and not tests, which is why this file is not the
	// loudest violation of both of them
	for _, scan := range mustScanAuthRoots(t) {
		for name, paths := range scan.pathOf {
			for _, path := range paths {
				if strings.HasSuffix(path, "_test.go") {
					t.Errorf("the scan read %s from %s; the rules are about what ships", name, path)
				}
			}
		}
	}
}

// The scope's derived half: every production package of this module that is built on this
// one is under the constant time gate.
//
// authScanRoots is written down, and a written down scope is the defect ledger 21 names, so
// it is not left as the only statement. The reason it cannot simply BE this derivation is
// measured rather than argued: at the commit that created connect/messagegroup that package
// imports crypto/ecdh, crypto/mlkem, crypto/sha3, io and connect/mls and does not import this
// package at all, so a scope derived from the import graph alone would have returned exactly
// this directory and covered nothing new -- while the whole point of the split is that the
// key schedule lands over there. So the list leads and this check follows it, and it starts
// biting on the first file of connect/messagegroup that calls into this package.
//
// Non test source only. A test that imports this package is not code an auditor reads for a
// timing leak, and the rules above read production files for the same reason.
//
// It reports what it walked. A walk that found no directory at all would clear every package
// in the module having read nothing, which is the failure mode every gate in this file is
// written against, so an empty walk is fatal and the counts are logged either way.
func TestEveryPackageBuiltOnThisOneIsUnderTheConstantTimeGate(t *testing.T) {
	root, module := authModuleOf(t, authOwnScanDir)
	self := module + "/message"
	covered := map[string]bool{}
	for _, named := range authScanRoots {
		resolved, err := filepath.Abs(named)
		if err != nil {
			t.Fatalf("resolving the root %s: %v", named, err)
		}
		covered[filepath.Clean(resolved)] = true
	}
	directories, importers := 0, []string{}
	fileSet := token.NewFileSet()
	walked := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			// testdata holds fixtures that are deliberately not buildable, and .git is not
			// source at all
			if entry.Name() == "testdata" || entry.Name() == ".git" {
				return filepath.SkipDir
			}
			directories++
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		parsed, err := parser.ParseFile(fileSet, path, nil, parser.ImportsOnly)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		for _, spec := range parsed.Imports {
			imported, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return fmt.Errorf("%s: an import path that is not a string: %s", path, spec.Path.Value)
			}
			if imported != self {
				continue
			}
			directory := filepath.Clean(filepath.Dir(path))
			if walked[directory] {
				continue
			}
			walked[directory] = true
			importers = append(importers, directory)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s for the packages built on this one: %v", root, err)
	}
	if directories == 0 {
		t.Fatalf("the walk of %s entered no directory, so it would clear every package in the module having read nothing", root)
	}
	slices.Sort(importers)
	for _, directory := range importers {
		if covered[directory] {
			continue
		}
		t.Errorf("%s imports %s and is not one of authScanRoots %v, so nothing holds its comparisons to constant time; add it as a root rather than copying these rules into it",
			directory, self, authScanRoots)
	}
	t.Logf("%d directories walked under %s, %d production packages import %s: %v; authScanRoots covers %v",
		directories, root, len(importers), self, importers, authScanRoots)
}
