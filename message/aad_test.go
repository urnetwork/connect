// The two aead preimages, held to master section 8's block rather than to themselves.
//
// Four kinds of test are here and they answer four different questions, which is why none
// of them subsumes another.
//
// The known answer vectors are the anchors that do not move with the code. Everything
// else in this file is written beside the builder and shares its idea of the layout, so a
// permutation applied to the builder and to the test at once passes all of it; a
// hexadecimal string derived by hand from the spec's block, field by field, is the only
// thing here that a permutation cannot follow. Each one is written out with its
// derivation beside it so the next reader can check the string against master section 8
// without running anything.
//
// The independence tests answer what the vectors cannot: that every field really is in
// the preimage, rather than three of them being in it and the vector happening to agree.
// Their field lists are read out of the go structs with reflection, so a field added to
// RecordHeader without a mutator here fails rather than passing unobserved — the whole
// point being that the class of things covered is computed from the tree and never typed
// out. The complement is asserted as well, and it is where guardrail G4 of spec A section
// 5.9 becomes an observation rather than a claim: every header field aad_body does not
// cover must leave aad_body unchanged, body_hash among them.
//
// The reparse tests state the layout a second time, in the reading direction, and answer
// the question a distinctness test cannot: not "do these two preimages differ" but "does
// this preimage still mean what it was built from". A field written at the wrong width, a
// field dropped, or two adjacent fields swapped all survive a distinctness test and none
// of them survives a reparse.
//
// The input gate is the structural half of G4. The guardrail's defence is a signature —
// "aad_body is built by a function that does not take a hash argument" — and a signature
// is checked by reading the syntax tree, not by reading the diff. It is written as a
// general rule because the general rule is the one worth having: no function in aad.go
// that hands back a preimage may be given a field it does not read. On AADBody that rule
// is G4 exactly. On AADHead it points at master invariant I6 from the other side, and it
// is weaker than that invariant: the walk sees a field mentioned rather than a field
// covered, so a field added to RecordHeader and never named in AADHead fails there, while
// one named only to be compared or length checked does not. The gate's own comment says
// which fields that distinction falls on and what covers them instead.
//
// The second structural rule beside it is smaller and has one target. No preimage builder
// may assign a result it is handed back to the blank identifier, because the writer's
// sticky error is the one thing either builder is ever told about its own output, and
// AADBody has no reachable writer failure at all — every field it writes is a fixed width
// array — so for that half of the file there is nothing else the rule could be.
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
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect/mls/syntax"
)

// The alg_id every vector here is built under: 0x0021, XChaCha20-Poly1305 in master
// section 7.1's table. A real value rather than a round number, so a preimage that wrote
// the field at the wrong width lands somewhere visible.
const aadKatAlgId uint16 = 0x0021

// ── the known answer vectors ────────────────────────────────────────────────────────

// A ramp, for a vector's fixed width fields. The bytes are consecutive so that a field
// read or written at the wrong offset lands on values that visibly are not its own, and
// the first byte is the argument so that no two fields of the same width in one vector
// hold the same bytes.
func aadRamp(first byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = first + byte(i)
	}
	return out
}

// The eph blob-rung header both of the first two vectors are built from, and the axes it
// pins: an eph class, so the retention byte is the joined 0x10|bucket and not a go tag; a
// commit, so is_commit is set; the blob rung, so blob_id is present and thirty two bytes;
// an expire_at that is a real millisecond timestamp; and an attachment, so the hash field
// is the hash of something.
func aadKatEphHeader() RecordHeader {
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
	return header
}

// The ordinary header the other two vectors are built from, and it is the opposite of the
// one above on every axis it can be: a class that carries no bucket, not a commit, the
// smallest rung rather than the blob rung, no blob id, expire_at unset, and no attachment
// at all. Between the two headers every field of the preimage is pinned in both of the
// shapes it takes.
func aadKatOrdinaryHeader() RecordHeader {
	header := RecordHeader{
		Epoch:          0x0000000100000000,
		StreamIndex:    0xFFFFFFFFFFFFFFFF,
		IsCommit:       false,
		RetentionClass: RetentionDurable,
		// the presence rule's zero: a durable record carries the field and carries it as
		// eight zero octets, which is what the vectors below pin
		EphWindow:  0,
		SizeBucket: SizeBucket256,
	}
	copy(header.GroupId[:], aadRamp(0x21, 32))
	copy(header.SenderHandle[:], aadRamp(0x80, 16))
	copy(header.BodyHash[:], aadRamp(0x40, 32))
	return header
}

// aad_body, pinned to its exact bytes, in both shapes the retention byte takes.
//
// Derived by hand from master section 8's aad_body block and not by printing what AADBody
// produced: a vector taken from the builder pins the builder to itself. Each line is one
// term of that block, in the order the block writes them.
//
// The label line is the one to read twice. It is twenty one raw ascii octets with no
// length prefix in front of them — "URmessage/v1/aad/body", which is 0x55 0x52 0x6d for
// U, R, m and so on — and the hexadecimal is spelled out here rather than computed from
// the constant, because computing it from the constant is how a test agrees with a
// mistyped label.
func TestAADBodyIsPinnedToItsExactBytes(t *testing.T) {
	const wantEph = "55526d6573736167652f76312f6161642f626f6479" + // "URmessage/v1/aad/body", raw ascii, no prefix
		"0021" + // u16(alg_id): XChaCha20-Poly1305
		"00000020" + "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20" + // LP(group_id)
		"00000010" + "a0a1a2a3a4a5a6a7a8a9aaabacadaeaf" + // LP(sender_handle)
		"0000000000000001" + // u64(epoch)
		"00000000ffffffff" + // u64(stream_index)
		"15" + // u8(retention_class): eph bucket 5, the joined byte and not the go tag 3
		"00000000000002c5" // u64(eph_window): 709, straight after the class it qualifies

	const wantOrdinary = "55526d6573736167652f76312f6161642f626f6479" + // the same label
		"0021" + // u16(alg_id)
		"00000020" + "2122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f40" + // LP(group_id)
		"00000010" + "808182838485868788898a8b8c8d8e8f" + // LP(sender_handle)
		"0000000100000000" + // u64(epoch): the first value that does not fit in 32 bits
		"ffffffffffffffff" + // u64(stream_index): the top of the range
		"01" + // u8(retention_class): durable, a class that carries no bucket
		"0000000000000000" // u64(eph_window): the ZERO a non eph record carries, and still eight octets

	// twenty one label octets, two for alg_id, thirty six and twenty for the two length
	// prefixed handles, sixteen for the two counters, one for the class and eight for the
	// window it qualifies
	const wantLength = 21 + 2 + 36 + 20 + 8 + 8 + 1 + 8

	for _, vector := range []struct {
		name   string
		header RecordHeader
		want   string
	}{
		{name: "the eph blob-rung header", header: aadKatEphHeader(), want: wantEph},
		{name: "the ordinary durable header", header: aadKatOrdinaryHeader(), want: wantOrdinary},
	} {
		if len(vector.want) != 2*wantLength {
			t.Fatalf("%s: the vector is %d bytes and the block adds up to %d", vector.name, len(vector.want)/2, wantLength)
		}
		got := mustAADBody(t, vector.name, aadKatAlgId, vector.header.BodyBinding())
		if hex.EncodeToString(got) != vector.want {
			t.Errorf("%s: aad_body is\n%s\nwant\n%s", vector.name, hex.EncodeToString(got), vector.want)
		}
	}
}

// aad_head, pinned to its exact bytes, on the header that carries every optional field:
// a present blob id and a present attachment.
//
// The last line is the one this vector exists for. LP(H(server_attachment)) is the hash
// of the attachment and never the attachment, so the field is thirty two bytes wide
// whatever the attachment's length is, and 5f78c332…aa813953 is SHA-256 of the four bytes
// de ad be ef. A builder that wrote the attachment itself would produce a four byte field
// here and would still round trip against itself.
func TestAADHeadIsPinnedToItsExactBytes(t *testing.T) {
	const want = "55526d6573736167652f76312f6161642f68656164" + // "URmessage/v1/aad/head", raw ascii, no prefix
		"0021" + // u16(alg_id)
		"00000020" + "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20" + // LP(group_id)
		"00000010" + "a0a1a2a3a4a5a6a7a8a9aaabacadaeaf" + // LP(sender_handle)
		"0000000000000001" + // u64(epoch)
		"00000000ffffffff" + // u64(stream_index)
		"01" + // u8(is_commit)
		"15" + // u8(retention_class): eph bucket 5
		"00000000000002c5" + // u64(eph_window): 709
		"05" + // u8(size_bucket): the blob rung
		"0000018f5cd3a600" + // u64(expire_at): unix milliseconds
		"00000020" + "b0b1b2b3b4b5b6b7b8b9babbbcbdbebfc0c1c2c3c4c5c6c7c8c9cacbcccdcecf" + // LP(body_hash)
		"00000020" + "d0d1d2d3d4d5d6d7d8d9dadbdcdddedfe0e1e2e3e4e5e6e7e8e9eaebecedeeef" + // LP(blob_id)
		"00000020" + "5f78c33274e43fa9de5659265c1d917e25c03722dcb0b8d27db8d5feaa813953" // LP(H(de ad be ef))

	const wantLength = 21 + 2 + 36 + 20 + 8 + 8 + 1 + 1 + 8 + 1 + 8 + 36 + 36 + 36
	if len(want) != 2*wantLength {
		t.Fatalf("the vector is %d bytes and the block adds up to %d", len(want)/2, wantLength)
	}
	header := aadKatEphHeader()
	got := mustAADHead(t, "the eph blob-rung header", aadKatAlgId, &header, header.ServerAttachment)
	if hex.EncodeToString(got) != want {
		t.Errorf("aad_head is\n%s\nwant\n%s", hex.EncodeToString(got), want)
	}
}

// The absent attachment, pinned, because it is the cross-implementation decision in this
// file and the one that costs the most to get wrong.
//
// Master section 8 writes LP(H(server_attachment)) with no carve out for the ordinary
// record; spec A section 5.2 says the attachment "MUST then encode zero-length", which a
// reader can take as saying this field of the preimage is empty. It does not. Section
// 5.2 governs the attachment's own encoding, and section 5.11's test obligation — a zero
// length attachment and an AttachmentNone attachment must encode identically "so
// H(server_attachment) cannot differ between client and server for an ordinary record" —
// only means anything if H is then applied to those bytes with no carve out of its own.
// So the field is LP(SHA-256("")), thirty six bytes: the prefix 00000020 and then
// e3b0c442…7852b855, which is the SHA-256 of the empty string and is a value any second
// implementation can check without running this code.
//
// The other half of the same header pins the other unconditional field. Its size bucket
// is not the blob rung, so blob_id is absent, and it is still written: LP of nothing is
// the four octets 00000000 and there is no if in the builder. A conditional there would
// shorten this vector by four bytes.
func TestAnAbsentAttachmentHashesTheEmptyStringAndAnAbsentBlobIdIsStillWritten(t *testing.T) {
	const want = "55526d6573736167652f76312f6161642f68656164" + // "URmessage/v1/aad/head"
		"0021" + // u16(alg_id)
		"00000020" + "2122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f40" + // LP(group_id)
		"00000010" + "808182838485868788898a8b8c8d8e8f" + // LP(sender_handle)
		"0000000100000000" + // u64(epoch)
		"ffffffffffffffff" + // u64(stream_index)
		"00" + // u8(is_commit): clear
		"01" + // u8(retention_class): durable
		"0000000000000000" + // u64(eph_window): the zero of a non eph record, written whole
		"00" + // u8(size_bucket): the 256 B rung
		"0000000000000000" + // u64(expire_at): unset
		"00000020" + "404142434445464748494a4b4c4d4e4f505152535455565758595a5b5c5d5e5f" + // LP(body_hash)
		"00000000" + // LP(blob_id): absent, and still four octets
		"00000020" + "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" // LP(SHA-256(""))

	const wantLength = 21 + 2 + 36 + 20 + 8 + 8 + 1 + 1 + 8 + 1 + 8 + 36 + 4 + 36
	if len(want) != 2*wantLength {
		t.Fatalf("the vector is %d bytes and the block adds up to %d", len(want)/2, wantLength)
	}
	header := aadKatOrdinaryHeader()
	if header.BlobId != nil || header.ServerAttachment != nil {
		t.Fatalf("the ordinary header carries a blob id or an attachment, so it pins neither absence")
	}
	got := mustAADHead(t, "the ordinary durable header", aadKatAlgId, &header, nil)
	if hex.EncodeToString(got) != want {
		t.Errorf("aad_head is\n%s\nwant\n%s", hex.EncodeToString(got), want)
	}
	// and the vector's own claim about SHA-256(""), checked against the standard library
	// rather than against the string above, so a mistyped digest fails here as well
	empty := sha256.Sum256(nil)
	if !strings.HasSuffix(want, hex.EncodeToString(empty[:])) {
		t.Errorf("the vector ends %s, want the SHA-256 of the empty string %s", want[len(want)-64:], hex.EncodeToString(empty[:]))
	}
}

// The attachment vector, in the shape and at the length a real one has.
//
// Every other attachment in this file is the four bytes de ad be ef, and four bytes fit
// inside SHA-256's first block with the padding still to spare. That leaves the whole
// truncation class unobserved: an H that hashed only its first four octets, or only its
// first block, agrees with every short vector here and disagrees with a second
// implementation on every record that carries a real attachment. The records that carry
// one are commits, an EpochAttachment encodes to 136 octets, and 136 is past both
// boundaries — past the 55 a one block SHA-256 message can hold, and past the 64 octet
// block itself.
//
// The bytes are spec A section 5.11's block, and they are a legal attachment rather than
// a ramp with a length: the epoch it opens is the commit's plus one, which is what that
// section requires of it, the alg_id is HKDF-SHA-256, and durable_ttl_seconds carries the
// sentinel that asks for indefinite retention.
//
//	server_attachment := u16(kind) ‖ LP(body)
//	EpochAttachment   := u64(epoch) ‖ u16(alg_id) ‖ LP(write_key) ‖ LP(read_key)
//	                   ‖ u32(media_ttl_seconds) ‖ u32(durable_ttl_seconds)
//	                   ‖ LP(group_context_hash) ‖ u32(expected_wrap_count)
const aadKatCommitAttachmentHex = "0001" + // u16(kind): 0x0001, an EpochAttachment
	"00000082" + // LP(body): the 130 octets of the block below
	"000000000000002a" + // u64(epoch): 42, the epoch it opens, which is the commit's 41 plus one
	"0031" + // u16(alg_id): HKDF-SHA-256
	"00000020" + "707172737475767778797a7b7c7d7e7f808182838485868788898a8b8c8d8e8f" + // LP(write_key)
	"00000020" + "909192939495969798999a9b9c9d9e9fa0a1a2a3a4a5a6a7a8a9aaabacadaeaf" + // LP(read_key)
	"00278d00" + // u32(media_ttl_seconds): thirty days
	"ffffffff" + // u32(durable_ttl_seconds): the sentinel that asks for indefinite retention
	"00000020" + "c0c1c2c3c4c5c6c7c8c9cacbcccdcecfd0d1d2d3d4d5d6d7d8d9dadbdcdddedf" + // LP(group_context_hash)
	"000005dd" // u32(expected_wrap_count): 1501, the 500 member bundle section 5.11 sizes

// The attachment above as octets. Decoded on each call rather than shared, because the
// tests below flip octets in it.
func aadKatCommitAttachment(t testing.TB) []byte {
	t.Helper()
	attachment, err := hex.DecodeString(aadKatCommitAttachmentHex)
	if err != nil {
		t.Fatalf("the attachment vector is not hexadecimal: %v", err)
	}
	return attachment
}

// The commit the attachment rides on: a durable record on the 4 KiB rung, which is the
// third size bucket the vectors pin and the only one of the three whose attachment is a
// length a record really carries.
func aadKatCommitHeader(t testing.TB) RecordHeader {
	t.Helper()
	header := RecordHeader{
		Epoch:            41,
		StreamIndex:      3,
		IsCommit:         true,
		RetentionClass:   RetentionDurable,
		SizeBucket:       SizeBucket4K,
		ServerAttachment: aadKatCommitAttachment(t),
	}
	copy(header.GroupId[:], aadRamp(0x11, 32))
	copy(header.SenderHandle[:], aadRamp(0x50, 16))
	copy(header.BodyHash[:], aadRamp(0x60, 32))
	return header
}

// aad_head over a commit carrying a real EpochAttachment, pinned to its exact octets.
//
// The last line is what this vector exists for, and it is the one number in either
// builder most worth checking against a second implementation: LP(H(server_attachment))
// over 136 octets, which no truncating and no single block H can reproduce. The digest
// was derived from spec A section 5.11's block by a separate program that imports nothing
// from this package, so it pins the builder to the spec and not to itself.
func TestAADHeadHashesTheWholeOfARealisticAttachment(t *testing.T) {
	const want = "55526d6573736167652f76312f6161642f68656164" + // "URmessage/v1/aad/head"
		"0021" + // u16(alg_id)
		"00000020" + "1112131415161718191a1b1c1d1e1f202122232425262728292a2b2c2d2e2f30" + // LP(group_id)
		"00000010" + "505152535455565758595a5b5c5d5e5f" + // LP(sender_handle)
		"0000000000000029" + // u64(epoch): 41, the epoch the commit is submitted at
		"0000000000000003" + // u64(stream_index)
		"01" + // u8(is_commit): set, the only record kind that carries an EpochAttachment
		"01" + // u8(retention_class): durable
		"0000000000000000" + // u64(eph_window): a commit is durable, so the field is zero and still written
		"02" + // u8(size_bucket): the 4 KiB rung, the third rung any vector here pins
		"0000000000000000" + // u64(expire_at): unset
		"00000020" + "606162636465666768696a6b6c6d6e6f707172737475767778797a7b7c7d7e7f" + // LP(body_hash)
		"00000000" + // LP(blob_id): absent off the blob rung, and still four octets
		"00000020" + "e929aff882de8ed3c8aa90dd6bba457e3d22fb0b725909f19afc2c596d3b1ea5" // LP(H(the 136 octet attachment))

	const wantLength = 21 + 2 + 36 + 20 + 8 + 8 + 1 + 1 + 8 + 1 + 8 + 36 + 4 + 36
	if len(want) != 2*wantLength {
		t.Fatalf("the vector is %d octets and the block adds up to %d", len(want)/2, wantLength)
	}
	header := aadKatCommitHeader(t)
	// the length is the whole point of this vector, so it is asserted rather than assumed:
	// an attachment that fitted in one SHA-256 block would leave the truncating H this
	// exists to catch passing
	if want, got := 136, len(header.ServerAttachment); got != want {
		t.Fatalf("the attachment is %d octets and spec A section 5.11's block is %d", got, want)
	}
	if len(header.ServerAttachment) <= sha256.BlockSize {
		t.Fatalf("the attachment is %d octets, which fits in one %d octet SHA-256 block", len(header.ServerAttachment), sha256.BlockSize)
	}
	got := mustAADHead(t, "the commit header", aadKatAlgId, &header, header.ServerAttachment)
	if hex.EncodeToString(got) != want {
		t.Errorf("aad_head is\n%s\nwant\n%s", hex.EncodeToString(got), want)
	}
	// and the vector's own digest, checked against the standard library over the whole
	// attachment, so a mistyped literal above fails here rather than agreeing with itself
	digest := sha256.Sum256(header.ServerAttachment)
	if !strings.HasSuffix(want, hex.EncodeToString(digest[:])) {
		t.Errorf("the vector ends %s, want SHA-256 of the whole attachment %s", want[len(want)-64:], hex.EncodeToString(digest[:]))
	}
}

// Every octet of the attachment reaches aad_head, at every offset.
//
// The vector above pins one attachment; this pins the property that vector is evidence
// for, over a class computed from the attachment's own length rather than from offsets
// somebody chose. An H that stopped at four octets, at the first block, or one octet
// short of the end passes every fixed vector built from a shorter attachment, and fails
// here at the first offset it drops.
func TestEveryOctetOfTheAttachmentReachesTheHead(t *testing.T) {
	base := aadKatCommitHeader(t)
	if len(base.ServerAttachment) == 0 {
		t.Fatal("the commit header carries no attachment, so this test varies nothing")
	}
	want := mustAADHead(t, "the commit header", aadKatAlgId, &base, base.ServerAttachment)
	for offset := range base.ServerAttachment {
		mutated := aadKatCommitHeader(t)
		mutated.ServerAttachment[offset] ^= 0xFF
		what := fmt.Sprintf("the attachment with octet %d flipped", offset)
		got := mustAADHead(t, what, aadKatAlgId, &mutated, mutated.ServerAttachment)
		if bytes.Equal(got, want) {
			t.Errorf("flipping octet %d of the %d octet attachment leaves aad_head unchanged", offset, len(base.ServerAttachment))
		}
	}
}

// A nil attachment and an empty one are the same value, because LP has no representation
// that tells them apart and neither does SHA-256. Asserted so that a caller holding
// []byte{} rather than nil is not a caller whose records nobody else can open.
func TestANilAttachmentAndAnEmptyOneAreTheSameAttachment(t *testing.T) {
	nilHeader := aadKatOrdinaryHeader()
	emptyHeader := aadKatOrdinaryHeader()
	emptyHeader.ServerAttachment = []byte{}
	fromNil := mustAADHead(t, "a nil attachment", aadKatAlgId, &nilHeader, nil)
	fromEmpty := mustAADHead(t, "an empty attachment", aadKatAlgId, &emptyHeader, []byte{})
	if !bytes.Equal(fromNil, fromEmpty) {
		t.Errorf("a nil attachment gives\n%s\nand an empty one gives\n%s", hex.EncodeToString(fromNil), hex.EncodeToString(fromEmpty))
	}
}

// The labels, pinned to their bytes and to their lengths, because the file comment makes
// two claims about them that nothing else here would notice being broken.
//
// They are the same length, which is what makes the separation rest on the bytes alone —
// the property the domain separation test below is built around. And they are twenty one
// bytes, which is the length the vectors above were counted with, so a label that grew or
// shrank would move every vector without any of them saying that is what happened.
func TestTheTwoLabelsAreTheSameLengthAndDifferentBytes(t *testing.T) {
	if len(aadBodyLabel) != len(aadHeadLabel) {
		t.Errorf("the labels are %d and %d bytes; the file comment says the separation rests on the bytes and not on a length boundary", len(aadBodyLabel), len(aadHeadLabel))
	}
	if len(aadBodyLabel) != 21 {
		t.Errorf("the labels are %d bytes and every vector in this file was counted with 21", len(aadBodyLabel))
	}
	if aadBodyLabel == aadHeadLabel {
		t.Fatal("the two labels are the same string, so the two preimages are one preimage")
	}
	if want := "URmessage/v1/aad/body"; aadBodyLabel != want {
		t.Errorf("the body label is %q, want %q from master section 8", aadBodyLabel, want)
	}
	if want := "URmessage/v1/aad/head"; aadHeadLabel != want {
		t.Errorf("the head label is %q, want %q from master section 8", aadHeadLabel, want)
	}
}

// ── the field mutators, derived from the structs ────────────────────────────────────

// A header and the attachment argument that goes with it, which is one value in two
// places: AADHead refuses them disagreeing, so anything that varies the attachment has to
// vary both halves at once.
type aadInput struct {
	header     RecordHeader
	attachment []byte
}

func (self aadInput) copy() aadInput {
	return aadInput{header: self.header, attachment: self.attachment}
}

// The header every mutation below starts from.
//
// It is an eph record on purpose. The retention class and the eph bucket are two go
// fields and one wire byte, and a bucket is only legal under the eph class, so a base
// that was durable would have no legal single-field mutation of EphBucket at all — the
// join refuses a non eph class carrying one. Starting at eph bucket 2 leaves the bucket
// free to move on its own, and the class's own mutation carries the bucket back to zero
// with it, which is the one place in this file a mutator touches two fields and it is
// because the wire carries them as one.
func aadBaseInput() aadInput {
	header := RecordHeader{
		Epoch:          7,
		StreamIndex:    9,
		IsCommit:       false,
		RetentionClass: RetentionEph,
		EphBucket:      2,
		// a window the wire can carry, non zero so that its own mutation below is a real
		// change and so that the zero is the OTHER vector's job. The builders validate no
		// field, so this window not belonging to bucket 2 costs nothing here -- and it is
		// the one literal the vectors' 00000000000002c5 is the hexadecimal of, held to the
		// ruling's arithmetic in codec_test.go rather than to anything in this file
		EphWindow:  pinnedEphWindow,
		SizeBucket: SizeBucket256,
		ExpireAt:   0,
	}
	copy(header.GroupId[:], aadRamp(0x01, 32))
	copy(header.SenderHandle[:], aadRamp(0xa0, 16))
	copy(header.BodyHash[:], aadRamp(0xb0, 32))
	return aadInput{header: header}
}

// One mutation per field of RecordHeader, keyed by the field's own name so the table can
// be checked against the struct rather than against a list somebody kept up to date.
//
// Every one of them changes exactly one field, with the single exception the base
// header's comment explains. The value each moves to is a value the wire can carry, so a
// preimage that changed is a preimage of a record somebody could really send.
//
// BlobId is the one that is deliberately not a legal record, and it is worth the
// paragraph, because the obvious tidying breaks three tests silently. The base header is
// on the 256 B rung, so a 32 octet blob id on it is a header ParseRecord refuses:
// ErrBlobIdPresence fires in both directions and spec A section 5.1 says the presence of
// the blob id is the size bucket restated. Moving the size bucket to the blob rung with
// it would make the header legal and the mutation useless, because SizeBucket has a
// mutator of its own and the two changes together no longer isolate either field. Leaving
// them apart is what kills the conditional a reader's instinct writes into the builder:
// with the rung at 256 B and a blob id present, an "if the bucket is the blob rung" drops
// the field and TestEveryFieldAADHeadCoversChangesIt, TestNoTwoDistinctHeadersShareAnAAD-
// Head and TestEveryPreimageReadsBackAsTheHeaderItWasBuiltFrom all fail. The preimage
// builders take whatever header they are handed and validate none of it, which is what
// makes an illegal header a legitimate input here.
var aadHeaderMutators = map[string]func(*aadInput){
	"GroupId":      func(in *aadInput) { in.header.GroupId[31] ^= 0xFF },
	"SenderHandle": func(in *aadInput) { in.header.SenderHandle[15] ^= 0xFF },
	"Epoch":        func(in *aadInput) { in.header.Epoch++ },
	"StreamIndex":  func(in *aadInput) { in.header.StreamIndex++ },
	"IsCommit":     func(in *aadInput) { in.header.IsCommit = !in.header.IsCommit },
	// the class moves off eph, and the bucket has to come back to zero with it: the pair
	// is one wire byte and eph bucket 2 under the durable class is not a byte at all
	"RetentionClass": func(in *aadInput) {
		in.header.RetentionClass = RetentionMedia
		in.header.EphBucket = 0
	},
	"EphBucket":  func(in *aadInput) { in.header.EphBucket++ },
	"EphWindow":  func(in *aadInput) { in.header.EphWindow++ },
	"SizeBucket": func(in *aadInput) { in.header.SizeBucket = SizeBucket1K },
	"ExpireAt":   func(in *aadInput) { in.header.ExpireAt = 0x0000018F5CD3A600 },
	"BodyHash":   func(in *aadInput) { in.header.BodyHash[0] ^= 0xFF },
	"BlobId":     func(in *aadInput) { in.header.BlobId = aadRamp(0xd0, 32) },
	// the attachment is one value in two places and AADHead refuses them disagreeing, so
	// the mutation moves both halves
	"ServerAttachment": func(in *aadInput) {
		in.header.ServerAttachment = []byte{0xde, 0xad, 0xbe, 0xef}
		in.attachment = in.header.ServerAttachment
	},
}

// One mutation, looked up by name rather than indexed straight, so a field with no
// mutator reports itself here instead of calling a nil function and taking the rest of
// the run down with it. TestEveryHeaderFieldHasAMutator is what should catch that first;
// this is what keeps its failure from turning into an abort that hides every gate after
// it — which is exactly what happened the first time a field was added without one.
func aadMutate(t testing.TB, name string, in *aadInput) {
	t.Helper()
	mutate, present := aadHeaderMutators[name]
	if !present {
		t.Fatalf("%s has no mutator, so nothing in this file ever varies it", name)
	}
	mutate(in)
}

// The field names of a struct, read out of the type rather than written down. This is
// what makes every list below a computed class: a field added to RecordHeader or to
// BodyBinding appears here on the next run, without anybody remembering to add it.
func aadFieldNames(value any) []string {
	structType := reflect.TypeOf(value)
	names := []string{}
	for i := range structType.NumField() {
		names = append(names, structType.Field(i).Name)
	}
	slices.Sort(names)
	return names
}

// The mutator table covers RecordHeader exactly: no field without a mutation, and no
// mutation for a field that is gone.
//
// This is the test that keeps every independence assertion below honest. Without it a
// field added to the header is a field no test in this file ever varies, and the first
// anybody hears of it is a second implementation refusing the aead.
func TestEveryHeaderFieldHasAMutator(t *testing.T) {
	fields := aadFieldNames(RecordHeader{})
	mutated := []string{}
	for name := range aadHeaderMutators {
		mutated = append(mutated, name)
	}
	slices.Sort(mutated)
	if !slices.Equal(fields, mutated) {
		t.Fatalf("RecordHeader has %v and the mutator table has %v; every field needs one and no more", fields, mutated)
	}
	if len(fields) == 0 {
		t.Fatal("RecordHeader has no fields, so every independence test below is vacuous")
	}
}

// BodyBinding is a projection of RecordHeader and not a second declaration of the same
// fields: every name in it is a name in the header, with the same type.
//
// It matters because BodyBinding exists only to be narrower. If a field drifted — a
// different type under the same name, or a name the header does not have — then the two
// structs would be two ideas of what a record is, and RecordHeader.BodyBinding would be
// converting between them rather than selecting from one.
func TestBodyBindingIsAStrictProjectionOfTheHeader(t *testing.T) {
	headerType := reflect.TypeOf(RecordHeader{})
	bindingType := reflect.TypeOf(BodyBinding{})
	if bindingType.NumField() == 0 {
		t.Fatal("BodyBinding has no fields, so every aad_body test below is vacuous")
	}
	if headerType.NumField() <= bindingType.NumField() {
		t.Errorf("BodyBinding has %d fields and RecordHeader has %d; the narrowing is the whole point of the type", bindingType.NumField(), headerType.NumField())
	}
	for i := range bindingType.NumField() {
		field := bindingType.Field(i)
		headerField, present := headerType.FieldByName(field.Name)
		if !present {
			t.Errorf("BodyBinding.%s is not a field of RecordHeader, so it is not a projection of one", field.Name)
			continue
		}
		if headerField.Type != field.Type {
			t.Errorf("BodyBinding.%s is %s and RecordHeader.%s is %s", field.Name, field.Type, field.Name, headerField.Type)
		}
	}
}

// RecordHeader.BodyBinding carries the value of every field it projects, rather than the
// zero of one of them.
//
// A projection that dropped a field would leave that field's mutation below changing the
// header and not the binding, and the independence test would report the builder ignoring
// a field the builder in fact writes. This is the assertion that keeps a failure there
// pointing at the builder.
func TestTheProjectionCarriesEveryFieldItSelects(t *testing.T) {
	base := aadBaseInput()
	for _, name := range aadFieldNames(BodyBinding{}) {
		mutated := base.copy()
		aadMutate(t, name, &mutated)
		fromBase := reflect.ValueOf(base.header.BodyBinding()).FieldByName(name).Interface()
		fromMutated := reflect.ValueOf(mutated.header.BodyBinding()).FieldByName(name).Interface()
		if reflect.DeepEqual(fromBase, fromMutated) {
			t.Errorf("mutating %s leaves BodyBinding.%s at %v, so the projection does not carry it", name, name, fromBase)
		}
	}
}

// ── field independence ──────────────────────────────────────────────────────────────

// Every field aad_head covers changes aad_head, one field at a time.
//
// The class is RecordHeader's fields, read off the struct. That is the second half of the
// input gate further down and it is asserted here in the running direction: the gate says
// AADHead reads every field of the header, and this says every one of those reads reaches
// the bytes. A field that were read and then discarded passes the gate and fails here.
func TestEveryFieldAADHeadCoversChangesIt(t *testing.T) {
	base := aadBaseInput()
	want := mustAADHead(t, "the base header", aadKatAlgId, &base.header, base.attachment)
	for _, name := range aadFieldNames(RecordHeader{}) {
		mutated := base.copy()
		aadMutate(t, name, &mutated)
		got := mustAADHead(t, "the header with "+name+" changed", aadKatAlgId, &mutated.header, mutated.attachment)
		if bytes.Equal(got, want) {
			t.Errorf("changing %s leaves aad_head unchanged at\n%s", name, hex.EncodeToString(want))
		}
	}
}

// The size bucket octet, at every value the field can hold.
//
// AADHead validates none of the header — it is a preimage builder over whatever it is
// handed, and the rung is checked where records are parsed — so the fidelity of this
// octet is this file's job alone. A write that masked it, truncated it or ranged it would
// agree with every vector on the rungs those vectors use and put two different headers
// under one preimage everywhere else.
//
// The class is every value the field's own type can hold, computed from its width rather
// than from the ladder, and that is the point rather than thoroughness for its own sake:
// a masking write is invisible on the ladder, because the six legal rungs are 0 through 5
// and a mask of the low three bits moves none of them. It shows on the values above the
// ladder, which are exactly the values a preimage builder that refuses nothing can be
// handed.
func TestAADHeadWritesEverySizeBucketOctetWhole(t *testing.T) {
	if width := reflect.TypeOf(SizeBucket(0)).Size(); width != 1 {
		t.Fatalf("SizeBucket is %d octets wide and this test walks the values of one", width)
	}
	seen := map[string]int{}
	for value := 0; value <= math.MaxUint8; value++ {
		input := aadBaseInput()
		input.header.SizeBucket = SizeBucket(value)
		what := fmt.Sprintf("size bucket %d", value)
		got := mustAADHead(t, what, aadKatAlgId, &input.header, input.attachment)
		// the read back is what says the octet is this value and not some function of it
		aadHeadReadBack(t, what, got, aadKatAlgId, &input.header, input.attachment)
		key := hex.EncodeToString(got)
		if other, collided := seen[key]; collided {
			t.Errorf("size buckets %d and %d share an aad_head:\n%s", value, other, key)
			continue
		}
		seen[key] = value
	}
	if want := math.MaxUint8 + 1; len(seen) != want {
		t.Errorf("the %d size bucket values produced %d distinct preimages", want, len(seen))
	}
}

// Every field aad_body covers changes aad_body, one field at a time. The class is
// BodyBinding's fields, read off the struct.
func TestEveryFieldAADBodyCoversChangesIt(t *testing.T) {
	base := aadBaseInput()
	want := mustAADBody(t, "the base header", aadKatAlgId, base.header.BodyBinding())
	for _, name := range aadFieldNames(BodyBinding{}) {
		mutated := base.copy()
		aadMutate(t, name, &mutated)
		got := mustAADBody(t, "the header with "+name+" changed", aadKatAlgId, mutated.header.BodyBinding())
		if bytes.Equal(got, want) {
			t.Errorf("changing %s leaves aad_body unchanged at\n%s", name, hex.EncodeToString(want))
		}
	}
}

// The complement, and guardrail G4 as a running observation rather than a claim about a
// signature: every header field aad_body does not cover leaves aad_body alone.
//
// body_hash is the field the guardrail is about — hashing the body's own ciphertext into
// the aad the body is sealed under is circular — and it is in this class by construction
// rather than by being named here: the class is RecordHeader's fields minus BodyBinding's,
// computed from the two structs. The input gate makes the defect unwritable; this makes it
// visible if it is ever written anyway, through a route the gate cannot see.
func TestAADBodyIgnoresEveryHeaderFieldItDoesNotCover(t *testing.T) {
	covered := aadFieldNames(BodyBinding{})
	uncovered := []string{}
	for _, name := range aadFieldNames(RecordHeader{}) {
		if !slices.Contains(covered, name) {
			uncovered = append(uncovered, name)
		}
	}
	if len(uncovered) == 0 {
		t.Fatal("aad_body covers every field of the header, so there is nothing here for G4 to be about")
	}
	if !slices.Contains(uncovered, "BodyHash") {
		t.Errorf("BodyHash is not among the fields aad_body leaves out: %v", uncovered)
	}
	base := aadBaseInput()
	want := mustAADBody(t, "the base header", aadKatAlgId, base.header.BodyBinding())
	for _, name := range uncovered {
		mutated := base.copy()
		aadMutate(t, name, &mutated)
		got := mustAADBody(t, "the header with "+name+" changed", aadKatAlgId, mutated.header.BodyBinding())
		if !bytes.Equal(got, want) {
			t.Errorf("changing %s changed aad_body, which covers no such field:\n got: %s\nwant: %s", name, hex.EncodeToString(got), hex.EncodeToString(want))
		}
	}
}

// The alg_id is in both preimages, which is master section 7.1's whole point: the
// algorithm identifier travels inside the authenticated bytes so it cannot be stripped or
// downgraded. It is not a field of either struct, so no mutator above reaches it.
func TestTheAlgIdIsInBothPreimages(t *testing.T) {
	base := aadBaseInput()
	body := mustAADBody(t, "alg 0x0021", 0x0021, base.header.BodyBinding())
	bodyOther := mustAADBody(t, "alg 0x0022", 0x0022, base.header.BodyBinding())
	if bytes.Equal(body, bodyOther) {
		t.Errorf("aad_body is the same under two alg_ids:\n%s", hex.EncodeToString(body))
	}
	head := mustAADHead(t, "alg 0x0021", 0x0021, &base.header, base.attachment)
	headOther := mustAADHead(t, "alg 0x0022", 0x0022, &base.header, base.attachment)
	if bytes.Equal(head, headOther) {
		t.Errorf("aad_head is the same under two alg_ids:\n%s", hex.EncodeToString(head))
	}
}

// ── distinctness over a corpus ──────────────────────────────────────────────────────

// The base header and one variant per mutator, which is the corpus both distinctness
// tests run over.
func aadCorpus(t testing.TB, fields []string) []struct {
	name  string
	input aadInput
} {
	t.Helper()
	corpus := []struct {
		name  string
		input aadInput
	}{{name: "the base header", input: aadBaseInput()}}
	for _, name := range fields {
		mutated := aadBaseInput()
		aadMutate(t, name, &mutated)
		corpus = append(corpus, struct {
			name  string
			input aadInput
		}{name: "the header with " + name + " changed", input: mutated})
	}
	return corpus
}

// No two distinct headers share an aad_head.
//
// Field independence asks whether each field reaches the bytes; this asks the question
// that matters to the aead, which is whether any two headers a sender could write collide
// — a collision being two records sealed under the same additional authenticated data,
// which is the thing the aad exists to prevent.
func TestNoTwoDistinctHeadersShareAnAADHead(t *testing.T) {
	seen := map[string]string{}
	for _, entry := range aadCorpus(t, aadFieldNames(RecordHeader{})) {
		got := mustAADHead(t, entry.name, aadKatAlgId, &entry.input.header, entry.input.attachment)
		key := hex.EncodeToString(got)
		if other, collided := seen[key]; collided {
			t.Errorf("%s and %s share an aad_head:\n%s", entry.name, other, key)
			continue
		}
		seen[key] = entry.name
	}
	if len(seen) < 2 {
		t.Fatalf("the corpus produced %d distinct preimages, so it distinguishes nothing", len(seen))
	}
}

// No two headers distinct in a field aad_body covers share an aad_body. The corpus is the
// BodyBinding fields only: two headers differing in a field aad_body does not cover are
// two headers aad_body is right to call the same, and the test above pins that separately.
func TestNoTwoDistinctBodyBindingsShareAnAADBody(t *testing.T) {
	seen := map[string]string{}
	for _, entry := range aadCorpus(t, aadFieldNames(BodyBinding{})) {
		got := mustAADBody(t, entry.name, aadKatAlgId, entry.input.header.BodyBinding())
		key := hex.EncodeToString(got)
		if other, collided := seen[key]; collided {
			t.Errorf("%s and %s share an aad_body:\n%s", entry.name, other, key)
			continue
		}
		seen[key] = entry.name
	}
	if len(seen) < 2 {
		t.Fatalf("the corpus produced %d distinct preimages, so it distinguishes nothing", len(seen))
	}
}

// ── domain separation ───────────────────────────────────────────────────────────────

// The header that makes the two preimages hardest to tell apart, and the reason this test
// uses it rather than an ordinary one.
//
// The labels are the same length, so the two preimages agree in shape for their first
// twenty three bytes whatever the header is. After the two handles and the two counters,
// aad_body's next byte is the retention class and aad_head's is is_commit. Set is_commit
// and give the record the durable class and those two bytes are both 0x01 — and then
// aad_head's following byte is the retention class again, also 0x01. So on this header,
// and only on a header like it, aad_body would be an exact prefix of aad_head the moment
// the labels stopped differing. Every other header hides that.
func aadCollidingHeader() RecordHeader {
	header := aadBaseInput().header
	header.IsCommit = true
	header.RetentionClass = RetentionDurable
	header.EphBucket = 0
	return header
}

// The two preimages are different bytes, and neither is a prefix of the other.
//
// A prefix relation is the failure a plain inequality misses. If aad_body were a prefix
// of aad_head then a ciphertext authenticated under one could be presented as the head of
// the other by an attacker who supplies the remaining bytes, which is the whole reason
// master section 8 gives the two preimages different labels rather than distinguishing
// them by length.
func TestTheTwoPreimagesAreDomainSeparated(t *testing.T) {
	header := aadCollidingHeader()
	// the header this test is built around, checked rather than assumed: the two bytes
	// that would coincide have to actually coincide, or the test is asserting a separation
	// nothing was threatening
	retentionWire, err := RetentionClassWire(header.RetentionClass, header.EphBucket)
	if err != nil {
		t.Fatalf("the colliding header has no wire byte: %v", err)
	}
	if retentionWire != isCommitByte(header.IsCommit) {
		t.Fatalf("the colliding header's retention byte is 0x%02x and its is_commit byte is 0x%02x; they must agree or this header does not test the prefix case",
			retentionWire, isCommitByte(header.IsCommit))
	}
	body := mustAADBody(t, "the colliding header", aadKatAlgId, header.BodyBinding())
	head := mustAADHead(t, "the colliding header", aadKatAlgId, &header, header.ServerAttachment)
	if bytes.Equal(body, head) {
		t.Fatalf("aad_body and aad_head are the same bytes:\n%s", hex.EncodeToString(body))
	}
	if bytes.HasPrefix(head, body) {
		t.Errorf("aad_body is a prefix of aad_head:\n body: %s\n head: %s", hex.EncodeToString(body), hex.EncodeToString(head))
	}
	if bytes.HasPrefix(body, head) {
		t.Errorf("aad_head is a prefix of aad_body:\n head: %s\n body: %s", hex.EncodeToString(head), hex.EncodeToString(body))
	}
	// and the separation is where the file comment says it is: in the label bytes, before
	// any field of the record
	if !bytes.HasPrefix(body, []byte(aadBodyLabel)) || !bytes.HasPrefix(head, []byte(aadHeadLabel)) {
		t.Errorf("a preimage does not begin with its own label")
	}
	if bytes.Equal(body[:len(aadBodyLabel)], head[:len(aadHeadLabel)]) {
		t.Errorf("the two preimages begin with the same %d bytes, so nothing separates them", len(aadBodyLabel))
	}
}

// The separation holds over the whole corpus and not only over the header built to break
// it, so a class or a size bucket cannot be the thing that happens to be saving it.
func TestNoHeaderMakesTheTwoPreimagesAgree(t *testing.T) {
	for _, entry := range aadCorpus(t, aadFieldNames(RecordHeader{})) {
		body := mustAADBody(t, entry.name, aadKatAlgId, entry.input.header.BodyBinding())
		head := mustAADHead(t, entry.name, aadKatAlgId, &entry.input.header, entry.input.attachment)
		if bytes.HasPrefix(head, body) || bytes.HasPrefix(body, head) {
			t.Errorf("%s: one preimage is a prefix of the other:\n body: %s\n head: %s", entry.name, hex.EncodeToString(body), hex.EncodeToString(head))
		}
	}
}

// ── the layout, read back ───────────────────────────────────────────────────────────

// aad_head, read back into its fields, and the fields compared with the header it was
// built from.
//
// This states master section 8's block a second time in the reading direction, which is
// the only thing in this file besides the vectors that can see a permutation the builder
// and a distinctness test would both agree on. Two adjacent fields swapped, a u64 written
// as a u32, a field dropped, or a length prefix left off all come back here as a value
// that is not the one that went in or as bytes left over at the end.
func aadHeadReadBack(t testing.TB, what string, preimage []byte, algId uint16, h *RecordHeader, serverAttachment []byte) {
	t.Helper()
	reader := syntax.NewReader(preimage)
	label, _ := reader.ReadRaw(len(aadHeadLabel))
	gotAlgId, _ := reader.ReadUint16()
	groupId, _ := reader.ReadOpaqueLP()
	senderHandle, _ := reader.ReadOpaqueLP()
	epoch, _ := reader.ReadUint64()
	streamIndex, _ := reader.ReadUint64()
	isCommit, _ := reader.ReadUint8()
	retentionWire, _ := reader.ReadUint8()
	ephWindow, _ := reader.ReadUint64()
	sizeBucket, _ := reader.ReadUint8()
	expireAt, _ := reader.ReadUint64()
	bodyHash, _ := reader.ReadOpaqueLP()
	blobId, _ := reader.ReadOpaqueLP()
	attachmentHash, _ := reader.ReadOpaqueLP()
	// the reader is sticky and Done refuses a trailing byte, so this is where a preimage
	// with a field too few or a field too many reports it
	if err := reader.Done(); err != nil {
		t.Errorf("%s: aad_head does not read back as master section 8's block: %v", what, err)
		return
	}
	wantRetentionWire, err := RetentionClassWire(h.RetentionClass, h.EphBucket)
	if err != nil {
		t.Fatalf("%s: the header has no wire byte: %v", what, err)
	}
	// the hash the preimage must carry is the hash of the header's own attachment. it is
	// the argument's octets AADHead hashes, and the two are one value because AADHead
	// refuses them disagreeing — which is asserted here rather than assumed, so this read
	// back is about the header and not about whichever of the two the caller passed
	if !bytes.Equal(h.ServerAttachment, serverAttachment) {
		t.Fatalf("%s: the header carries %d attachment octets and the argument carries %d, which AADHead refuses", what, len(h.ServerAttachment), len(serverAttachment))
	}
	wantAttachmentHash := sha256.Sum256(h.ServerAttachment)
	for _, field := range []struct {
		name string
		got  any
		want any
	}{
		{name: "label", got: string(label), want: aadHeadLabel},
		{name: "alg_id", got: gotAlgId, want: algId},
		{name: "group_id", got: hex.EncodeToString(groupId), want: hex.EncodeToString(h.GroupId[:])},
		{name: "sender_handle", got: hex.EncodeToString(senderHandle), want: hex.EncodeToString(h.SenderHandle[:])},
		{name: "epoch", got: epoch, want: h.Epoch},
		{name: "stream_index", got: streamIndex, want: h.StreamIndex},
		{name: "is_commit", got: isCommit, want: isCommitByte(h.IsCommit)},
		{name: "retention_class", got: retentionWire, want: wantRetentionWire},
		{name: "eph_window", got: ephWindow, want: h.EphWindow},
		{name: "size_bucket", got: sizeBucket, want: byte(h.SizeBucket)},
		{name: "expire_at", got: expireAt, want: h.ExpireAt},
		{name: "body_hash", got: hex.EncodeToString(bodyHash), want: hex.EncodeToString(h.BodyHash[:])},
		{name: "blob_id", got: hex.EncodeToString(blobId), want: hex.EncodeToString(h.BlobId)},
		{name: "H(server_attachment)", got: hex.EncodeToString(attachmentHash), want: hex.EncodeToString(wantAttachmentHash[:])},
	} {
		if field.got != field.want {
			t.Errorf("%s: aad_head reads back %s as %v, want %v", what, field.name, field.got, field.want)
		}
	}
}

// aad_body, read back the same way.
func aadBodyReadBack(t testing.TB, what string, preimage []byte, algId uint16, binding BodyBinding) {
	t.Helper()
	reader := syntax.NewReader(preimage)
	label, _ := reader.ReadRaw(len(aadBodyLabel))
	gotAlgId, _ := reader.ReadUint16()
	groupId, _ := reader.ReadOpaqueLP()
	senderHandle, _ := reader.ReadOpaqueLP()
	epoch, _ := reader.ReadUint64()
	streamIndex, _ := reader.ReadUint64()
	retentionWire, _ := reader.ReadUint8()
	ephWindow, _ := reader.ReadUint64()
	if err := reader.Done(); err != nil {
		t.Errorf("%s: aad_body does not read back as master section 8's block: %v", what, err)
		return
	}
	wantRetentionWire, err := RetentionClassWire(binding.RetentionClass, binding.EphBucket)
	if err != nil {
		t.Fatalf("%s: the binding has no wire byte: %v", what, err)
	}
	for _, field := range []struct {
		name string
		got  any
		want any
	}{
		{name: "label", got: string(label), want: aadBodyLabel},
		{name: "alg_id", got: gotAlgId, want: algId},
		{name: "group_id", got: hex.EncodeToString(groupId), want: hex.EncodeToString(binding.GroupId[:])},
		{name: "sender_handle", got: hex.EncodeToString(senderHandle), want: hex.EncodeToString(binding.SenderHandle[:])},
		{name: "epoch", got: epoch, want: binding.Epoch},
		{name: "stream_index", got: streamIndex, want: binding.StreamIndex},
		{name: "retention_class", got: retentionWire, want: wantRetentionWire},
		{name: "eph_window", got: ephWindow, want: binding.EphWindow},
	} {
		if field.got != field.want {
			t.Errorf("%s: aad_body reads back %s as %v, want %v", what, field.name, field.got, field.want)
		}
	}
}

// Every preimage in the corpus reads back as the header it was built from, with nothing
// left over.
func TestEveryPreimageReadsBackAsTheHeaderItWasBuiltFrom(t *testing.T) {
	for _, entry := range aadCorpus(t, aadFieldNames(RecordHeader{})) {
		head := mustAADHead(t, entry.name, aadKatAlgId, &entry.input.header, entry.input.attachment)
		aadHeadReadBack(t, entry.name, head, aadKatAlgId, &entry.input.header, entry.input.attachment)
		body := mustAADBody(t, entry.name, aadKatAlgId, entry.input.header.BodyBinding())
		aadBodyReadBack(t, entry.name, body, aadKatAlgId, entry.input.header.BodyBinding())
	}
	// the three vectors as well, since they are the headers with the widest fields and the
	// only ones carrying an attachment of a length a record really has
	for _, header := range []RecordHeader{aadKatEphHeader(), aadKatOrdinaryHeader(), aadKatCommitHeader(t)} {
		aadHeadReadBack(t, "a vector header", mustAADHead(t, "a vector header", aadKatAlgId, &header, header.ServerAttachment), aadKatAlgId, &header, header.ServerAttachment)
	}
}

// The field boundaries, attacked explicitly.
//
// blob_id is the only variable length field in either preimage, and the field after it is
// LP(H(server_attachment)). The attempt is the classic one: give one header a blob_id that
// has swallowed the four octets which, in the other header's preimage, are the length
// prefix of the field that follows. If those boundaries were implicit — if either field
// were written raw — the two byte strings would be one reading of the same stream split
// two ways, and a record sealed under one could be opened as the other.
//
// They are not implicit, and the assertion is that the attempt fails: the preimages differ,
// and each still reads back with its own blob_id at its own length. The length prefix of
// the swallowing field is what does it, and it is the reason the file comment insists LP
// means WriteOpaqueLP and never a raw write.
//
// One thing this test cannot claim, and it is worth writing down rather than leaving for
// somebody to rediscover: on aad_head as master section 8 defines it the boundary cannot
// slide even without the prefixes, because the field after blob_id is a SHA-256 output and
// is therefore always exactly thirty two bytes. Given the total length, the split is
// determined. The prefixes are what keep that true of a future block with two variable
// fields beside each other, and the attempt below is what will still be here to catch it.
func TestAFieldCannotSwallowTheLengthPrefixOfTheFieldAfterIt(t *testing.T) {
	base := aadBaseInput()
	base.header.SizeBucket = SizeBucketBlob

	honest := base.copy()
	honest.header.BlobId = aadRamp(0xd0, 32)
	honest.header.ServerAttachment = []byte{0xde, 0xad, 0xbe, 0xef}
	honest.attachment = honest.header.ServerAttachment

	// the same blob id with the next field's own length prefix appended to it: 00 00 00 20,
	// the four octets LP writes in front of a thirty two byte hash
	swallowed := honest.copy()
	swallowed.header.BlobId = append(append([]byte{}, honest.header.BlobId...), 0x00, 0x00, 0x00, 0x20)

	honestBytes := mustAADHead(t, "the honest blob id", aadKatAlgId, &honest.header, honest.attachment)
	swallowedBytes := mustAADHead(t, "the blob id that swallowed the next prefix", aadKatAlgId, &swallowed.header, swallowed.attachment)
	if bytes.Equal(honestBytes, swallowedBytes) {
		t.Fatalf("a blob id that swallowed the following length prefix produced the same preimage:\n%s", hex.EncodeToString(honestBytes))
	}
	// and the boundary is still where each header put it
	aadHeadReadBack(t, "the honest blob id", honestBytes, aadKatAlgId, &honest.header, honest.attachment)
	aadHeadReadBack(t, "the blob id that swallowed the next prefix", swallowedBytes, aadKatAlgId, &swallowed.header, swallowed.attachment)

	// the mirror of the same attempt: a blob id built out of the bytes that follow it in
	// the other header's preimage, so that the two would be one stream split two ways
	hash := sha256.Sum256(honest.attachment)
	absorbing := base.copy()
	absorbing.header.BlobId = append(append([]byte{}, 0x00, 0x00, 0x00, 0x20), hash[:]...)
	absorbingBytes := mustAADHead(t, "the blob id built from the next field", aadKatAlgId, &absorbing.header, absorbing.attachment)
	for _, other := range []struct {
		name  string
		bytes []byte
	}{{name: "the honest blob id", bytes: honestBytes}, {name: "the blob id that swallowed the next prefix", bytes: swallowedBytes}} {
		if bytes.Equal(absorbingBytes, other.bytes) {
			t.Errorf("the blob id built from the next field collided with %s", other.name)
		}
	}
	aadHeadReadBack(t, "the blob id built from the next field", absorbingBytes, aadKatAlgId, &absorbing.header, absorbing.attachment)
}

// ── the refusals ────────────────────────────────────────────────────────────────────

// Both builders go through the one join, so both refuse a class and bucket pair the wire
// has no byte for rather than manufacturing one.
//
// The pairs are the two illegal edges record.go's own tests name, derived the same way:
// the first bucket past the top of the eph ladder, and a bucket carried by a class that
// does not take one.
func TestBothBuildersRefuseAPairTheWireHasNoByteFor(t *testing.T) {
	for _, pair := range []struct {
		name  string
		class RetentionClass
		// the mutation is applied to the base header, so the bucket is set outright
		bucket uint8
	}{
		{name: "an eph bucket past the ladder", class: RetentionEph, bucket: ephBucketMax + 1},
		{name: "a durable class carrying a bucket", class: RetentionDurable, bucket: 1},
	} {
		input := aadBaseInput()
		input.header.RetentionClass = pair.class
		input.header.EphBucket = pair.bucket
		if _, err := AADBody(aadKatAlgId, input.header.BodyBinding()); err == nil {
			t.Errorf("%s: AADBody built a preimage for a pair the wire cannot carry", pair.name)
		}
		if _, err := AADHead(aadKatAlgId, &input.header, input.attachment); err == nil {
			t.Errorf("%s: AADHead built a preimage for a pair the wire cannot carry", pair.name)
		}
	}
}

// AADHead refuses an attachment argument that disagrees with the header's own field, in
// both directions, and treats a nil and an empty slice as the same value.
//
// The refusal is what makes the second source for one value safe. A caller that passes nil
// while the header carries an attachment is the plausible mistake — Record.Header.
// ServerAttachment is right there and forgetting it costs nothing at compile time — and
// without this it seals ct_head under a preimage nothing will ever reproduce.
func TestAADHeadRefusesAnAttachmentThatDisagreesWithTheHeader(t *testing.T) {
	attachment := []byte{0xde, 0xad, 0xbe, 0xef}
	for _, disagreement := range []struct {
		name       string
		onHeader   []byte
		asArgument []byte
	}{
		{name: "the header carries one and the argument is nil", onHeader: attachment, asArgument: nil},
		{name: "the header carries none and the argument does", onHeader: nil, asArgument: attachment},
		{name: "the two differ in a byte", onHeader: attachment, asArgument: []byte{0xde, 0xad, 0xbe, 0xee}},
		{name: "the two differ in length", onHeader: attachment, asArgument: attachment[:3]},
	} {
		input := aadBaseInput()
		input.header.ServerAttachment = disagreement.onHeader
		got, err := AADHead(aadKatAlgId, &input.header, disagreement.asArgument)
		if err == nil {
			t.Errorf("%s: AADHead built a preimage anyway:\n%s", disagreement.name, hex.EncodeToString(got))
			continue
		}
		if !errors.Is(err, ErrServerAttachmentMismatch) {
			t.Errorf("%s: AADHead refused with %v, want ErrServerAttachmentMismatch", disagreement.name, err)
		}
	}
	// and the one pair that is not a disagreement: LP cannot tell nil from empty, so
	// neither may this
	input := aadBaseInput()
	input.header.ServerAttachment = []byte{}
	if _, err := AADHead(aadKatAlgId, &input.header, nil); err != nil {
		t.Errorf("AADHead called an empty attachment and a nil one a disagreement: %v", err)
	}
}

// AADHead reports a nil header rather than dereferencing it. Nothing in this package
// panics; errors.go says why.
func TestAADHeadRefusesANilHeader(t *testing.T) {
	got, err := AADHead(aadKatAlgId, nil, nil)
	if err == nil {
		t.Fatalf("AADHead built a preimage from no header at all:\n%s", hex.EncodeToString(got))
	}
	if !errors.Is(err, ErrRecordHeaderNil) {
		t.Errorf("AADHead refused with %v, want ErrRecordHeaderNil", err)
	}
}

// AADHead reports the writer's failure rather than handing back an empty preimage.
//
// The writer latches its first failure and every later call is a no op, so the whole build
// is asked whether it worked exactly once, at the return. Dropping that answer is a one
// character edit and its consequence is not local: Bytes hands back nil beside the error,
// so a builder that dropped it would return an empty preimage and nil, and a sealer would
// aead a record against no additional data at all — which every reader recomputes as the
// real preimage and refuses, on a record that is otherwise entirely well formed.
//
// The reachable failure is the length bound. WriteOpaqueLP refuses a field longer than the
// writer's maximum, and blob_id is the one field either builder writes whose length a
// caller chooses: AADHead validates no part of the header, which is what leaves the bound
// reachable from a legal call. So this is the header that reaches the latch.
func TestAADHeadReportsAWriterFailureRatherThanAnEmptyPreimage(t *testing.T) {
	input := aadBaseInput()
	input.header.SizeBucket = SizeBucketBlob
	input.header.BlobId = make([]byte, syntax.MaxVectorLength+1)
	got, err := AADHead(aadKatAlgId, &input.header, input.attachment)
	if err == nil {
		t.Fatalf("AADHead built a %d octet preimage over a blob id of %d octets, past the writer's maximum of %d",
			len(got), len(input.header.BlobId), syntax.MaxVectorLength)
	}
	if !errors.Is(err, syntax.ErrLengthExceedsMax) {
		t.Errorf("AADHead refused with %v, want the writer's %v", err, syntax.ErrLengthExceedsMax)
	}
	if got != nil {
		t.Errorf("AADHead handed back %d octets beside the error; a caller that seals under them seals under a preimage nothing reproduces", len(got))
	}
	// and the bound is where the writer says it is rather than where this test guessed, so
	// the header above is one octet over it and not merely large
	within := aadBaseInput()
	within.header.SizeBucket = SizeBucketBlob
	within.header.BlobId = make([]byte, syntax.MaxVectorLength)
	if _, err := AADHead(aadKatAlgId, &within.header, within.attachment); err != nil {
		t.Errorf("AADHead refused a blob id of exactly the writer's maximum %d: %v", syntax.MaxVectorLength, err)
	}
}

// ── the input gate ──────────────────────────────────────────────────────────────────

// The file that owns the preimages, and the control directory, both relative to this
// package. The gate's class is every function in the first that hands back a preimage —
// computed by reading the file, not by naming the functions — and the second is what
// proves the gate can tell a violation from a clean function at all.
const (
	preimageSourceFile = "aad.go"
	preimageControlDir = "testdata/preimage"
)

// A struct field, with the package level type name of its own type when it has one. An
// empty typeName means the field's type is an array, a slice, a basic type or something
// declared elsewhere — anything the reachability walk stops at.
type preimageField struct {
	name     string
	typeName string
}

// A function that hands back a preimage, and what it was given.
type preimageBuilder struct {
	name string
	path string
	// the receiver and every parameter whose type is a struct declared in the scanned
	// files, by the identifier the function knows it as
	inputs map[string]string
	// every "Type.Field" the body selects off one of those identifiers
	reads map[string]bool
}

// A scan of one directory: the struct declarations of every go file in it, and the
// builders declared in the files asked for.
type preimageScan struct {
	structs  map[string][]preimageField
	builders []preimageBuilder
}

// The verdict on one builder. Two ways to fail rather than one, because they are two
// different failures: unread names a field the function was handed and never looked at,
// which is the guardrail; uncovered names a field the walk cannot see into, which is the
// gate admitting it would be under-reporting rather than reporting clean.
type preimageVerdict struct {
	unread    []string
	uncovered []string
}

// The package level type name of a type expression, or empty for anything the walk stops
// at. A pointer is followed because *RecordHeader and RecordHeader hand a function the
// same fields; an array or a slice is not, because [32]byte and []byte have no fields to
// reach.
func preimageTypeName(expr ast.Expr) string {
	switch typed := expr.(type) {
	case *ast.Ident:
		return typed.Name
	case *ast.StarExpr:
		return preimageTypeName(typed.X)
	}
	return ""
}

// Whether a function's results are exactly ([]byte, error), which is what "hands back a
// preimage" means here. It is a shape and not a list of names, so a third builder added
// to aad.go is gated on the day it is written.
func preimageResultsAreBytesAndError(decl *ast.FuncDecl) bool {
	results := decl.Type.Results
	if results == nil {
		return false
	}
	types := []string{}
	for _, field := range results.List {
		names := max(len(field.Names), 1)
		for range names {
			types = append(types, preimageResultString(field.Type))
		}
	}
	return slices.Equal(types, []string{"[]byte", "error"})
}

// The two result type spellings this predicate has to tell apart, rendered from the tree.
func preimageResultString(expr ast.Expr) string {
	switch typed := expr.(type) {
	case *ast.Ident:
		return typed.Name
	case *ast.ArrayType:
		if typed.Len == nil {
			return "[]" + preimageResultString(typed.Elt)
		}
	}
	return "?"
}

// Read one directory: every struct declared in it, and the builders declared in the named
// files. Test files are skipped, because a gate that read its own control fixtures out of
// the package under test would be judging itself.
func scanPreimages(t testing.TB, dir string, funcFiles []string) preimageScan {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("the preimage gate cannot read %s: %v", dir, err)
	}
	scan := preimageScan{structs: map[string][]preimageField{}}
	files := []string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		files = append(files, name)
	}
	if len(files) == 0 {
		t.Fatalf("the preimage gate found no go source in %s, so it would report clean having read nothing", dir)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		path := filepath.Join(dir, name)
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("the preimage gate cannot parse %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			switch typed := decl.(type) {
			case *ast.GenDecl:
				collectPreimageStructs(scan.structs, typed)
			case *ast.FuncDecl:
				if !slices.Contains(funcFiles, name) || !preimageResultsAreBytesAndError(typed) {
					continue
				}
				scan.builders = append(scan.builders, preimageBuilderOf(typed, name))
			}
		}
	}
	slices.SortFunc(scan.builders, func(a preimageBuilder, b preimageBuilder) int { return strings.Compare(a.name, b.name) })
	return scan
}

// Every struct type declared by one declaration, with its fields.
func collectPreimageStructs(structs map[string][]preimageField, decl *ast.GenDecl) {
	for _, spec := range decl.Specs {
		typeSpec, isType := spec.(*ast.TypeSpec)
		if !isType {
			continue
		}
		structType, isStruct := typeSpec.Type.(*ast.StructType)
		if !isStruct {
			continue
		}
		fields := []preimageField{}
		for _, field := range structType.Fields.List {
			for _, name := range field.Names {
				fields = append(fields, preimageField{name: name.Name, typeName: preimageTypeName(field.Type)})
			}
		}
		structs[typeSpec.Name.Name] = fields
	}
}

// One builder: the identifiers it was handed, and every field selected off one of them.
// The receiver is an input like any parameter — a builder written as a method on the
// header would otherwise walk straight past this gate.
func preimageBuilderOf(decl *ast.FuncDecl, path string) preimageBuilder {
	builder := preimageBuilder{name: decl.Name.Name, path: path, inputs: map[string]string{}, reads: map[string]bool{}}
	fields := []*ast.Field{}
	if decl.Recv != nil {
		fields = append(fields, decl.Recv.List...)
	}
	fields = append(fields, decl.Type.Params.List...)
	for _, field := range fields {
		typeName := preimageTypeName(field.Type)
		if typeName == "" {
			continue
		}
		for _, name := range field.Names {
			builder.inputs[name.Name] = typeName
		}
	}
	ast.Inspect(decl.Body, func(node ast.Node) bool {
		selector, isSelector := node.(*ast.SelectorExpr)
		if !isSelector {
			return true
		}
		ident, isIdent := selector.X.(*ast.Ident)
		if !isIdent {
			return true
		}
		if typeName, isInput := builder.inputs[ident.Name]; isInput {
			builder.reads[typeName+"."+selector.Sel.Name] = true
		}
		return true
	})
	return builder
}

// The verdict on one builder: every field reachable from its inputs that it never reads,
// and every field the walk cannot see into.
func judgePreimageBuilder(scan preimageScan, builder preimageBuilder) preimageVerdict {
	verdict := preimageVerdict{}
	for _, typeName := range builder.inputs {
		for _, field := range scan.structs[typeName] {
			qualified := typeName + "." + field.name
			if _, nested := scan.structs[field.typeName]; nested {
				verdict.uncovered = append(verdict.uncovered, qualified)
			}
			if !builder.reads[qualified] {
				verdict.unread = append(verdict.unread, qualified)
			}
		}
	}
	slices.Sort(verdict.unread)
	slices.Sort(verdict.uncovered)
	return verdict
}

// How many fields a builder was actually handed, which is what tells a clean verdict from
// a vacuous one.
func preimageReachableCount(scan preimageScan, builder preimageBuilder) int {
	count := 0
	for _, typeName := range builder.inputs {
		count += len(scan.structs[typeName])
	}
	return count
}

// No function that hands back a preimage is given a field it does not read.
//
// On AADBody this is guardrail G4 of spec A section 5.9 as a structural fact rather than
// as a habit: the guardrail's defence is "aad_body is built by a function that does not
// take a hash argument", and the only way to check a signature is to read it. AADBody
// takes a BodyBinding, which has no hash in it, so the circular preimage is not something
// a future edit can write by accident — it would have to widen the signature first, and
// widening it to *RecordHeader fails here with body_hash named.
//
// On AADHead the rule points at master invariant I6 from the other side — AADHead is
// handed the whole header, so the rule says it must read all of it — and what the walk
// observes is weaker than that invariant. It records a selector off an input, so a field
// satisfies it by being mentioned: compared, logged or length checked all count as a read.
// A field added to RecordHeader and never named in AADHead does fail here, which is the
// case this gate is for; a field named without being written does not.
//
// ServerAttachment is the field that distinction falls on, and it is fine rather than a
// hole, which is why it is written down here instead of being called coverage. The octets
// AADHead hashes are the argument's; the header's own field appears only in the comparison
// that refuses the two disagreeing. So what makes aad_head cover the header's attachment
// is that refusal, which leaves the two one value in every call that builds anything, and
// it is asserted three more times: by the refusal's own test in both directions, by the
// read back, which derives the hash it wants from the header's field rather than from the
// argument, and by the vectors.
func TestNoPreimageBuilderIsHandedAFieldItDoesNotRead(t *testing.T) {
	scan := scanPreimages(t, ".", []string{preimageSourceFile})
	if len(scan.builders) == 0 {
		t.Fatalf("the gate found no preimage builder in %s, so it is reporting clean having read nothing", preimageSourceFile)
	}
	names := []string{}
	for _, builder := range scan.builders {
		names = append(names, builder.name)
	}
	// the coverage claim, checked rather than assumed: the two builders this file is about
	// have to be among the ones the gate is judging
	for _, want := range []string{"AADBody", "AADHead"} {
		if !slices.Contains(names, want) {
			t.Fatalf("the gate is judging %v, which does not include %s", names, want)
		}
	}
	for _, builder := range scan.builders {
		reachable := preimageReachableCount(scan, builder)
		if reachable == 0 {
			t.Errorf("%s is handed no struct at all, so the gate says nothing about it", builder.name)
			continue
		}
		verdict := judgePreimageBuilder(scan, builder)
		if 0 < len(verdict.uncovered) {
			t.Errorf("%s is handed %v, which are structs this gate reads one level and cannot see into", builder.name, verdict.uncovered)
		}
		if 0 < len(verdict.unread) {
			t.Errorf("%s is handed %v and never reads them; a preimage builder takes what it covers and nothing else", builder.name, verdict.unread)
		}
		t.Logf("%s reads all %d fields it is handed", builder.name, reachable)
	}
}

// The control, in all four of its shapes. Without it the gate above proves nothing: it
// reports clean, and a gate that is broken reports clean too.
func TestThePreimageGateSeparatesTheControlShapes(t *testing.T) {
	scan := scanPreimages(t, preimageControlDir, []string{"control.go"})
	verdicts := map[string]preimageVerdict{}
	for _, builder := range scan.builders {
		verdicts[builder.name] = judgePreimageBuilder(scan, builder)
	}
	judged := []string{}
	for name := range verdicts {
		judged = append(judged, name)
	}
	slices.Sort(judged)
	// the class predicate is part of the control: the fixture's non preimage function
	// ignores the same field as the positive control and must still not be judged
	if want := []string{"preimageOverANarrowBinding", "preimageOverANestedInput", "preimageOverAWideHeader"}; !slices.Equal(judged, want) {
		t.Fatalf("the gate judged %v in the control, want %v", judged, want)
	}
	if positive := verdicts["preimageOverAWideHeader"]; !slices.Equal(positive.unread, []string{"wideHeader.BodyHash"}) {
		t.Errorf("the gate reported %v for the control that ignores a hash, want the hash", positive.unread)
	}
	if negative := verdicts["preimageOverANarrowBinding"]; 0 < len(negative.unread)+len(negative.uncovered) {
		t.Errorf("the gate reported %v and %v for the control that reads everything it is handed", negative.unread, negative.uncovered)
	}
	if nested := verdicts["preimageOverANestedInput"]; !slices.Equal(nested.uncovered, []string{"nestingInput.Inner"}) {
		t.Errorf("the gate reported %v for the control holding a struct field, want the struct field", nested.uncovered)
	} else if 0 < len(nested.unread) {
		t.Errorf("the gate reported %v unread for the control that reads every field it has", nested.unread)
	}
}

// No preimage builder assigns a result it is handed back to the blank identifier.
//
// aad.go's comment at the return — "the writer is sticky: the first failure latches and
// every later call is a no op, so this is the one place the build is asked whether it
// worked" — is a claim about a value not being discarded, and the only way to check that
// is to read the code. The live half is above, on AADHead, whose blob_id can reach the
// writer's length bound. There is no live half for AADBody: every field it writes is a
// fixed width array, so its writer has no reachable failure at all, and a discarded error
// there would stay invisible until the day somebody gives aad_body a variable length
// field — at which point it is a sealer aeading against an empty preimage.
//
// So the rule is structural, over the same computed class the gate above judges: every
// function in aad.go whose results are ([]byte, error). Nothing in one of them may be
// assigned to the blank identifier. It is a blunt rule and it is the right blunt rule
// here, because in a function whose whole job is to be octet exact there is nothing worth
// discarding: a result is used or it is refused.
func TestNoPreimageBuilderDiscardsAResultItIsHandedBack(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, preimageSourceFile, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("the discard gate cannot parse %s: %v", preimageSourceFile, err)
	}
	judged := []string{}
	for _, decl := range file.Decls {
		funcDecl, isFunc := decl.(*ast.FuncDecl)
		if !isFunc || funcDecl.Body == nil || !preimageResultsAreBytesAndError(funcDecl) {
			continue
		}
		judged = append(judged, funcDecl.Name.Name)
		ast.Inspect(funcDecl.Body, func(node ast.Node) bool {
			assign, isAssign := node.(*ast.AssignStmt)
			if !isAssign {
				return true
			}
			for _, target := range assign.Lhs {
				ident, isIdent := target.(*ast.Ident)
				if isIdent && ident.Name == "_" {
					t.Errorf("%s discards a result at %s; a preimage builder uses what it is handed back or refuses on it, and the writer's sticky error is the one it is handed",
						funcDecl.Name.Name, fset.Position(ident.Pos()))
				}
			}
			return true
		})
	}
	slices.Sort(judged)
	if len(judged) == 0 {
		t.Fatalf("the discard gate found no preimage builder in %s, so it is reporting clean having read nothing", preimageSourceFile)
	}
	// the coverage claim, checked rather than assumed, the same way the gate above checks it
	for _, want := range []string{"AADBody", "AADHead"} {
		if !slices.Contains(judged, want) {
			t.Fatalf("the discard gate is judging %v, which does not include %s", judged, want)
		}
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────────────

// The two builders, with the error turned into a failure, for the tests whose subject is
// the bytes and not the refusal.
func mustAADBody(t testing.TB, what string, algId uint16, binding BodyBinding) []byte {
	t.Helper()
	preimage, err := AADBody(algId, binding)
	if err != nil {
		t.Fatalf("%s: AADBody refused: %v", what, err)
	}
	return preimage
}

func mustAADHead(t testing.TB, what string, algId uint16, h *RecordHeader, serverAttachment []byte) []byte {
	t.Helper()
	preimage, err := AADHead(algId, h, serverAttachment)
	if err != nil {
		t.Fatalf("%s: AADHead refused: %v", what, err)
	}
	return preimage
}
