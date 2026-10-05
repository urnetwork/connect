// The GroupInfo, GroupSecrets and Welcome codecs.
//
// Three things are being held here and they are not the same kind of statement.
//
// A round trip says the two halves of one codec agree with each other, and that is ALL it
// says. Swap two length prefixed fields in both halves and the round trip is perfect; drop a
// field from both halves and the round trip is perfect. So every structure in this file also
// has a HAND DERIVED golden: the octets written out from RFC 9420's field list and the
// section 2.1.2 varint rules, arithmetic done here rather than taken from this package's
// answer. The golden is the only assertion below that can see a symmetric edit.
//
// GroupInfo is signed, so its preimage matters more than its round trip. This package makes
// the preimage a view of the object rather than a second field list -- see
// GroupInfo.toBeSigned -- so the golden is stated for BOTH halves independently: if the
// delegation ever becomes two encoders again, the two goldens are what still separates them.
//
// Welcome is parsed by a party who is not yet a member, out of bytes an attacker may have
// shaped, before any group state exists to check the result against. So it gets the treatment
// an unauthenticated parser gets: every truncation refused, a nested length that overruns its
// region refused with the control that says the refusal is about the region boundary and not
// about the bytes, a tail refused, and a hostile length measured rather than argued.
package mls

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"testing"

	"github.com/urnetwork/connect/mls/syntax"
)

// ---------------------------------------------------------------------------
// the values every assertion below is stated over
// ---------------------------------------------------------------------------

// testGroupInfo is the object the hand derived golden describes.
//
// Every field carries a DISTINCT filler and the two multi-octet scalars are asymmetric --
// epoch is 0x0102030405060708 and signer is 0x01020304 -- so a byte order flip and a field
// swap both move the golden. A structure filled with one repeated octet is one where half the
// edits this file exists to catch produce identical bytes.
func testGroupInfo() GroupInfo {
	return GroupInfo{
		GroupContext: GroupContext{
			Version:                 ProtocolVersionMls10,
			CipherSuite:             CipherSuiteX25519ChaCha20Sha256Ed25519,
			GroupId:                 []byte{0x47, 0x49},
			Epoch:                   0x0102030405060708,
			TreeHash:                bytes.Repeat([]byte{0xc0}, 32),
			ConfirmedTranscriptHash: bytes.Repeat([]byte{0xee}, 32),
		},
		Extensions:      []Extension{{ExtensionType: ExtensionTypeRatchetTree, ExtensionData: []byte{0x01}}},
		ConfirmationTag: bytes.Repeat([]byte{0x0a}, 32),
		Signer:          0x01020304,
		Signature:       []byte{0xde, 0xad, 0xbe, 0xef},
	}
}

// testWelcome and testGroupSecrets, same discipline: distinct fillers at distinct LENGTHS, so
// that swapping two opaque<V> fields changes the octets rather than only their contents.
func testWelcome() Welcome {
	return Welcome{
		CipherSuite: CipherSuiteX25519ChaCha20Sha256Ed25519,
		Secrets: []EncryptedGroupSecrets{{
			NewMember: bytes.Repeat([]byte{0x44}, 4),
			EncryptedGroupSecrets: HpkeCiphertext{
				KemOutput:  bytes.Repeat([]byte{0x55}, 3),
				Ciphertext: bytes.Repeat([]byte{0x66}, 5),
			},
		}},
		EncryptedGroupInfo: bytes.Repeat([]byte{0x77}, 6),
	}
}

func testGroupSecrets() GroupSecrets {
	return GroupSecrets{
		JoinerSecret: bytes.Repeat([]byte{0x11}, 4),
		PathSecret:   &PathSecret{PathSecret: bytes.Repeat([]byte{0x22}, 3)},
		Psks: []PreSharedKeyId{{
			PskType:  PskTypeExternal,
			PskId:    []byte{0x01, 0x02},
			PskNonce: bytes.Repeat([]byte{0x33}, 3),
		}},
	}
}

// ---------------------------------------------------------------------------
// the hand derived goldens
// ---------------------------------------------------------------------------

// welcomeWireGroupInfoTBSGolden is GroupInfoTBS over testGroupInfo's fields, written out from
// RFC 9420 section 12.4.3 and section 2.1.2 rather than read off this package.
//
//	GroupContext (section 8.1), inline and unframed -- 82 octets
//	  0001                     ProtocolVersion mls10, uint16
//	  0003                     CipherSuite 0x0003, uint16
//	  02 4749                  group_id<V>, 2 octets: prefix 0x02 (2 < 64, one octet)
//	  0102030405060708         epoch, uint64 big endian
//	  20 c0*32                 tree_hash<V>, 32 octets: prefix 0x20
//	  20 ee*32                 confirmed_transcript_hash<V>, 32 octets
//	  00                       extensions<V>, empty: a zero length prefix and nothing
//	extensions<V> of the GroupInfo itself -- 5 octets
//	  04                       region length 4
//	  0002                     ExtensionType ratchet_tree, uint16
//	  01 01                    extension_data<V>, 1 octet
//	confirmation_tag -- 33 octets
//	  20 0a*32                 MAC is opaque<V>; 32 octets
//	signer -- 4 octets
//	  01020304                 uint32 big endian
//
// 82 + 5 + 33 + 4 = 124 octets, which is what the length assertion below states independently
// of the byte comparison: a dropped field fails the count before it fails the compare, and the
// count is the half a reader can check by adding the numbers in this comment.
const welcomeWireGroupInfoTBSGolden = "0001" + // ProtocolVersion mls10, uint16
	"0003" + // CipherSuite 0x0003, uint16
	"02" + "4749" + // group_id<V>: prefix 0x02 then 2 octets
	"0102030405060708" + // epoch, uint64 big endian
	"20" + "c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0" + // tree_hash<V>: prefix 0x20 then 32 octets
	"20" + "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee" + // confirmed_transcript_hash<V>: prefix 0x20 then 32 octets
	"00" + // the GroupContext's own extensions<V>, empty: a zero length region
	"04" + "0002" + "01" + "01" + // extensions<V>: region 4, ratchet_tree, extension_data<V> of 1 octet
	"20" + "0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a" + // confirmation_tag: MAC is opaque<V>, 32 octets
	"01020304" // signer, uint32 big endian

// welcomeWireGroupInfoGolden is the same object as a GroupInfo: the TBS above followed by
//
//	signature<V> -- 5 octets
//	  04 deadbeef            4 octets, prefix 0x04
//
// 124 + 5 = 129 octets. The signature is written out here rather than concatenated onto the
// constant above, so the two goldens are two readings and not one reading used twice.
const welcomeWireGroupInfoGolden = "0001" + // ProtocolVersion mls10, uint16
	"0003" + // CipherSuite 0x0003, uint16
	"02" + "4749" + // group_id<V>: prefix 0x02 then 2 octets
	"0102030405060708" + // epoch, uint64 big endian
	"20" + "c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0" + // tree_hash<V>: prefix 0x20 then 32 octets
	"20" + "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee" + // confirmed_transcript_hash<V>: prefix 0x20 then 32 octets
	"00" + // the GroupContext's own extensions<V>, empty: a zero length region
	"04" + "0002" + "01" + "01" + // extensions<V>: region 4, ratchet_tree, extension_data<V> of 1 octet
	"20" + "0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a" + // confirmation_tag: MAC is opaque<V>, 32 octets
	"01020304" + // signer, uint32 big endian
	"04" + "deadbeef" // signature<V>: prefix 0x04 then 4 octets

// welcomeWireWelcomeGolden is Welcome over testWelcome's fields, RFC 9420 section 12.4.3.1:
//
//	0003                       CipherSuite, uint16
//	0f                         secrets<V> region, 15 octets
//	  04 44444444              new_member: KeyPackageRef is opaque<V>, 4 octets
//	  03 555555                kem_output<V>, 3 octets
//	  05 6666666666            ciphertext<V>, 5 octets
//	06 777777777777            encrypted_group_info<V>, 6 octets
//
// 2 + 1 + 15 + 7 = 25 octets. The three inner fields are three DIFFERENT lengths on purpose:
// with equal lengths, a codec that emitted them in any order would produce a golden differing
// only in filler, and a filler-only difference is the one a careless reader talks himself out
// of.
const welcomeWireWelcomeGolden = "00030f04444444440355555505666666666606777777777777"

// welcomeWireGroupSecretsGolden is GroupSecrets over testGroupSecrets' fields:
//
//	04 11111111                joiner_secret<V>, 4 octets
//	01                         optional<PathSecret> presence octet: present
//	  03 222222                PathSecret.path_secret<V>, 3 octets
//	08                         psks<V> region, 8 octets
//	  01                       PSKType external
//	  02 0102                  psk_id<V>, 2 octets
//	  03 333333                psk_nonce<V>, 3 octets
//
// 5 + 1 + 4 + 9 = 19 octets. The presence octet is a bare 0x01 with no length in front of it,
// which is the half of optional<T> a golden can see and a round trip cannot.
const welcomeWireGroupSecretsGolden = "04111111110103222222080102010203333333"

// welcomeWireGolden decodes one of the constants above, and states its own length. The length
// is not decoration: it is the arithmetic in each comment, checked separately from the byte
// comparison so that a failure says whether a field went missing or moved.
func welcomeWireGolden(t *testing.T, name string, golden string, octets int) []byte {
	t.Helper()
	decoded, err := hex.DecodeString(golden)
	if err != nil {
		t.Fatalf("%s: the golden in this file is not hex: %v", name, err)
	}
	if len(decoded) != octets {
		t.Fatalf("%s: the golden is %d octets and the arithmetic beside it adds to %d",
			name, len(decoded), octets)
	}
	return decoded
}

// ---------------------------------------------------------------------------
// GroupInfo
// ---------------------------------------------------------------------------

func TestGroupInfoEncodesTheHandDerivedGolden(t *testing.T) {
	want := welcomeWireGolden(t, "GroupInfo", welcomeWireGroupInfoGolden, 129)
	groupInfo := testGroupInfo()
	encoded, err := syntax.Marshal(&groupInfo)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Equal(encoded, want) {
		t.Fatalf("GroupInfo encoded to\n  %x\nand the hand derived golden is\n  %x", encoded, want)
	}
	// and the golden decodes back to the object it was written for, so the golden is a
	// statement about this codec's DECODE half as well
	decoded := GroupInfo{}
	if err := syntax.Unmarshal(want, &decoded); err != nil {
		t.Fatalf("the hand derived golden does not decode: %v", err)
	}
	if decoded.Signer != groupInfo.Signer {
		t.Errorf("the golden decoded signer = %#08x, want %#08x", uint32(decoded.Signer), uint32(groupInfo.Signer))
	}
	if !bytes.Equal(decoded.ConfirmationTag, groupInfo.ConfirmationTag) {
		t.Errorf("the golden decoded confirmation_tag = %x, want %x",
			decoded.ConfirmationTag, groupInfo.ConfirmationTag)
	}
	if !bytes.Equal(decoded.Signature, groupInfo.Signature) {
		t.Errorf("the golden decoded signature = %x, want %x", decoded.Signature, groupInfo.Signature)
	}
	if decoded.GroupContext.Epoch != groupInfo.GroupContext.Epoch {
		t.Errorf("the golden decoded epoch = %#016x, want %#016x",
			decoded.GroupContext.Epoch, groupInfo.GroupContext.Epoch)
	}
}

func TestGroupInfoTBSEncodesTheHandDerivedGolden(t *testing.T) {
	want := welcomeWireGolden(t, "GroupInfoTBS", welcomeWireGroupInfoTBSGolden, 124)
	groupInfo := testGroupInfo()
	tbs := GroupInfoTBS{
		GroupContext:    groupInfo.GroupContext,
		Extensions:      groupInfo.Extensions,
		ConfirmationTag: groupInfo.ConfirmationTag,
		Signer:          groupInfo.Signer,
	}
	encoded, err := syntax.Marshal(&tbs)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Equal(encoded, want) {
		t.Fatalf("GroupInfoTBS encoded to\n  %x\nand the hand derived golden is\n  %x", encoded, want)
	}
}

func TestGroupInfoRoundTrip(t *testing.T) {
	groupInfo := testGroupInfo()
	encoded, err := syntax.Marshal(&groupInfo)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	decoded := GroupInfo{}
	if err := syntax.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Signer != groupInfo.Signer || !bytes.Equal(decoded.Signature, groupInfo.Signature) {
		t.Fatalf("decoded %+v", decoded)
	}
	reencoded, err := syntax.Marshal(&decoded)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if !bytes.Equal(encoded, reencoded) {
		t.Fatalf("re-encoded %x, want %x", reencoded, encoded)
	}
}

// The signed bytes are the GroupInfo minus its own signature, and nothing more.
//
// This package holds that by construction -- GroupInfo.MarshalMLS encodes a GroupInfoTBS and
// appends the signature -- so the prefix half of this test cannot fail while that delegation
// stands, and it is here for the day somebody replaces it with a second field list. What the
// test still separates on its own is the OTHER direction: a GroupInfoTBS that included the
// signature would be unsignable, and no round trip of either structure can see it.
func TestGroupInfoTBSIsGroupInfoWithoutTheSignature(t *testing.T) {
	groupInfo := testGroupInfo()
	tbs := GroupInfoTBS{
		GroupContext:    groupInfo.GroupContext,
		Extensions:      groupInfo.Extensions,
		ConfirmationTag: groupInfo.ConfirmationTag,
		Signer:          groupInfo.Signer,
	}
	tbsBytes, err := syntax.Marshal(&tbs)
	if err != nil {
		t.Fatalf("tbs: %v", err)
	}
	full, err := syntax.Marshal(&groupInfo)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.HasPrefix(full, tbsBytes) {
		t.Fatalf("GroupInfo\n  %x\ndoes not begin with its own GroupInfoTBS\n  %x", full, tbsBytes)
	}
	if bytes.Contains(tbsBytes, groupInfo.Signature) {
		t.Fatal("the signature leaked into GroupInfoTBS, so the structure signs over its own signature")
	}
	// the remainder is exactly the signature as an opaque<V> and nothing else, which is the
	// statement "minus its own signature AND NOTHING MORE" -- a prefix check alone is
	// satisfied by a GroupInfo that appended a second copy of any field after the tbs
	rest, err := syntax.Marshal(&PathSecret{PathSecret: groupInfo.Signature})
	if err != nil {
		t.Fatalf("encode the expected remainder: %v", err)
	}
	if got := full[len(tbsBytes):]; !bytes.Equal(got, rest) {
		t.Fatalf("what follows the GroupInfoTBS is %x, want the signature as an opaque<V>, %x", got, rest)
	}
}

// GroupInfoTBS is encode only on purpose, and this is the statement of it: offering a decoder
// would invite a verifier to reconstruct the signed bytes from parsed fields instead of
// re-serializing the object it received.
func TestGroupInfoTBSIsEncodeOnly(t *testing.T) {
	var asMarshaler syntax.Marshaler = (*GroupInfoTBS)(nil)
	if _, isCodec := asMarshaler.(syntax.Codec); isCodec {
		t.Fatal("GroupInfoTBS has grown an UnmarshalMLS, so a verifier can now rebuild the signed bytes rather than re-serializing what it received")
	}
}

// ---------------------------------------------------------------------------
// GroupSecrets
// ---------------------------------------------------------------------------

func TestGroupSecretsEncodesTheHandDerivedGolden(t *testing.T) {
	want := welcomeWireGolden(t, "GroupSecrets", welcomeWireGroupSecretsGolden, 19)
	groupSecrets := testGroupSecrets()
	encoded, err := syntax.Marshal(&groupSecrets)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Equal(encoded, want) {
		t.Fatalf("GroupSecrets encoded to\n  %x\nand the hand derived golden is\n  %x", encoded, want)
	}
	decoded := GroupSecrets{}
	if err := syntax.Unmarshal(want, &decoded); err != nil {
		t.Fatalf("the hand derived golden does not decode: %v", err)
	}
	if decoded.PathSecret == nil {
		t.Fatal("the golden's presence octet is 0x01 and the decode answered no path secret")
	}
	if !bytes.Equal(decoded.PathSecret.PathSecret, groupSecrets.PathSecret.PathSecret) {
		t.Errorf("the golden decoded path_secret = %x, want %x",
			decoded.PathSecret.PathSecret, groupSecrets.PathSecret.PathSecret)
	}
	if len(decoded.Psks) != 1 || !bytes.Equal(decoded.Psks[0].PskId, groupSecrets.Psks[0].PskId) {
		t.Errorf("the golden decoded psks %+v", decoded.Psks)
	}
}

// The absent arm's golden, which is where optional<T>'s other encoding lives: one 0x00 octet
// with no value behind it.
//
//	04 11111111   joiner_secret<V>
//	00            optional<PathSecret>: absent
//	00            psks<V>: empty region
func TestGroupSecretsWithNoPathSecretEncodesTheAbsentOptional(t *testing.T) {
	want := welcomeWireGolden(t, "GroupSecrets absent arm", "0411111111"+"00"+"00", 7)
	groupSecrets := GroupSecrets{JoinerSecret: bytes.Repeat([]byte{0x11}, 4)}
	encoded, err := syntax.Marshal(&groupSecrets)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Equal(encoded, want) {
		t.Fatalf("GroupSecrets with no path secret encoded to %x, want %x", encoded, want)
	}
	decoded := GroupSecrets{}
	if err := syntax.Unmarshal(want, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.PathSecret != nil {
		t.Fatalf("an absent path secret decoded as present: %+v", decoded.PathSecret)
	}
}

func TestGroupSecretsRoundTripWithAndWithoutPathSecret(t *testing.T) {
	cases := []GroupSecrets{
		{JoinerSecret: bytes.Repeat([]byte{0x11}, 32)},
		{
			JoinerSecret: bytes.Repeat([]byte{0x11}, 32),
			PathSecret:   &PathSecret{PathSecret: bytes.Repeat([]byte{0x22}, 32)},
		},
		{
			JoinerSecret: bytes.Repeat([]byte{0x11}, 32),
			Psks: []PreSharedKeyId{{
				PskType:  PskTypeExternal,
				PskId:    []byte{0x01, 0x02},
				PskNonce: bytes.Repeat([]byte{0x33}, 32),
			}},
		},
		// the resumption arm as well, which is the psk shape the external one cannot stand
		// in for: it carries three more fields, so an element decoder reading the wrong arm
		// lands at the wrong offset inside the psks region rather than merely mis-reading
		// one value
		{
			JoinerSecret: bytes.Repeat([]byte{0x11}, 32),
			PathSecret:   &PathSecret{PathSecret: bytes.Repeat([]byte{0x22}, 32)},
			Psks: []PreSharedKeyId{
				{
					PskType:    PskTypeResumption,
					Usage:      ResumptionPskUsageApplication,
					PskGroupId: []byte{0x09, 0x08},
					PskEpoch:   0x1122334455667788,
					PskNonce:   bytes.Repeat([]byte{0x44}, 32),
				},
				{
					PskType:  PskTypeExternal,
					PskId:    []byte{0x05},
					PskNonce: bytes.Repeat([]byte{0x55}, 32),
				},
			},
		},
	}
	for i := range cases {
		encoded, err := syntax.Marshal(&cases[i])
		if err != nil {
			t.Fatalf("case %d: marshal: %v", i, err)
		}
		decoded := GroupSecrets{}
		if err := syntax.Unmarshal(encoded, &decoded); err != nil {
			t.Fatalf("case %d: unmarshal: %v", i, err)
		}
		if (decoded.PathSecret == nil) != (cases[i].PathSecret == nil) {
			t.Fatalf("case %d: path secret presence flipped", i)
		}
		if len(decoded.Psks) != len(cases[i].Psks) {
			t.Fatalf("case %d: decoded %d psks, want %d", i, len(decoded.Psks), len(cases[i].Psks))
		}
		reencoded, err := syntax.Marshal(&decoded)
		if err != nil {
			t.Fatalf("case %d: re-marshal: %v", i, err)
		}
		if !bytes.Equal(encoded, reencoded) {
			t.Fatalf("case %d: re-encoded %x, want %x", i, reencoded, encoded)
		}
	}
}

// The presence octet has exactly two encodings, and a third would be a second encoding of one
// object -- which is the signature bypass primitive, since a signature covers one
// serialization. Commit's codec makes the same statement for the same reason.
func TestGroupSecretsRejectsInvalidOptionalPresenceByte(t *testing.T) {
	groupSecrets := GroupSecrets{JoinerSecret: bytes.Repeat([]byte{0x11}, 4)}
	valid, err := syntax.Marshal(&groupSecrets)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, octet := range []byte{0x02, 0x7f, 0xff} {
		encoded := bytes.Clone(valid)
		// the presence octet sits immediately after joiner_secret<V>: one prefix octet and
		// four content octets
		encoded[5] = octet
		decoded := GroupSecrets{}
		if err := syntax.Unmarshal(encoded, &decoded); !errors.Is(err, syntax.ErrOptionalPresence) {
			t.Errorf("presence octet %#02x through Unmarshal gave %v, want syntax.ErrOptionalPresence", octet, err)
		}
		// and the same refusal through the method, which is how every enclosing codec in
		// this package reaches it -- there is no Done() behind those calls to report a
		// refusal this decoder swallowed and latched
		fresh := GroupSecrets{}
		if err := fresh.UnmarshalMLS(syntax.NewReader(encoded)); !errors.Is(err, syntax.ErrOptionalPresence) {
			t.Errorf("presence octet %#02x through UnmarshalMLS gave %v, want syntax.ErrOptionalPresence", octet, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Welcome
// ---------------------------------------------------------------------------

func TestWelcomeEncodesTheHandDerivedGolden(t *testing.T) {
	want := welcomeWireGolden(t, "Welcome", welcomeWireWelcomeGolden, 25)
	welcome := testWelcome()
	encoded, err := syntax.Marshal(&welcome)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Equal(encoded, want) {
		t.Fatalf("Welcome encoded to\n  %x\nand the hand derived golden is\n  %x", encoded, want)
	}
	decoded := Welcome{}
	if err := syntax.Unmarshal(want, &decoded); err != nil {
		t.Fatalf("the hand derived golden does not decode: %v", err)
	}
	if decoded.CipherSuite != welcome.CipherSuite {
		t.Errorf("the golden decoded cipher_suite = %#04x, want %#04x",
			uint16(decoded.CipherSuite), uint16(welcome.CipherSuite))
	}
	if len(decoded.Secrets) != 1 {
		t.Fatalf("the golden decoded %d secrets, want 1", len(decoded.Secrets))
	}
	if !bytes.Equal(decoded.Secrets[0].NewMember, welcome.Secrets[0].NewMember) ||
		!bytes.Equal(decoded.Secrets[0].EncryptedGroupSecrets.KemOutput, welcome.Secrets[0].EncryptedGroupSecrets.KemOutput) ||
		!bytes.Equal(decoded.Secrets[0].EncryptedGroupSecrets.Ciphertext, welcome.Secrets[0].EncryptedGroupSecrets.Ciphertext) {
		t.Errorf("the golden decoded secrets[0] = %+v", decoded.Secrets[0])
	}
	if !bytes.Equal(decoded.EncryptedGroupInfo, welcome.EncryptedGroupInfo) {
		t.Errorf("the golden decoded encrypted_group_info = %x, want %x",
			decoded.EncryptedGroupInfo, welcome.EncryptedGroupInfo)
	}
}

func TestWelcomeRoundTrip(t *testing.T) {
	welcome := testWelcome()
	encoded, err := syntax.Marshal(&welcome)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	decoded := Welcome{}
	if err := syntax.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(decoded.Secrets) != 1 ||
		!bytes.Equal(decoded.Secrets[0].NewMember, welcome.Secrets[0].NewMember) {
		t.Fatalf("decoded %+v", decoded.Secrets)
	}
	reencoded, err := syntax.Marshal(&decoded)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if !bytes.Equal(encoded, reencoded) {
		t.Fatalf("re-encoded %x, want %x", reencoded, encoded)
	}
}

// A Welcome with two entries, which the one entry case cannot stand in for: the secrets vector
// is read to the END of its region rather than by an element count, so a decoder that stopped
// after the first element is right about a one entry Welcome and silently drops every joiner
// but one from a real one.
func TestWelcomeCarriesEveryEntryOfItsSecretsVector(t *testing.T) {
	welcome := testWelcome()
	welcome.Secrets = append(welcome.Secrets, EncryptedGroupSecrets{
		NewMember: bytes.Repeat([]byte{0x88}, 7),
		EncryptedGroupSecrets: HpkeCiphertext{
			KemOutput:  bytes.Repeat([]byte{0x99}, 2),
			Ciphertext: bytes.Repeat([]byte{0xaa}, 9),
		},
	})
	encoded, err := syntax.Marshal(&welcome)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	decoded := Welcome{}
	if err := syntax.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(decoded.Secrets) != 2 {
		t.Fatalf("a two entry Welcome decoded to %d entries", len(decoded.Secrets))
	}
	if !bytes.Equal(decoded.Secrets[1].NewMember, welcome.Secrets[1].NewMember) {
		t.Errorf("secrets[1].new_member = %x, want %x",
			decoded.Secrets[1].NewMember, welcome.Secrets[1].NewMember)
	}
	if !bytes.Equal(decoded.Secrets[1].EncryptedGroupSecrets.Ciphertext,
		welcome.Secrets[1].EncryptedGroupSecrets.Ciphertext) {
		t.Errorf("secrets[1].ciphertext = %x, want %x",
			decoded.Secrets[1].EncryptedGroupSecrets.Ciphertext,
			welcome.Secrets[1].EncryptedGroupSecrets.Ciphertext)
	}
}

// ---------------------------------------------------------------------------
// what a decoder handed hostile bytes owes
// ---------------------------------------------------------------------------

// welcomeWireRefusesEveryTruncation sweeps every proper prefix of a valid encoding and requires
// each to be refused, through BOTH entry points.
//
// The two entry points are not the same statement, and running only the first is how a
// swallowed error on the LAST field passes. syntax.Unmarshal joins the decoder's answer with
// Done, and a read that failed latches on the Reader, so Done reports it even if the decoder
// dropped its own error -- which means a decoder that ignored the failure of its final read
// still fails here. Every enclosing codec in this package reaches these decoders by METHOD
// call instead, with no Done behind it, and there a swallowed error is a structure built out
// of bytes that were never there and a nil error handed back.
func welcomeWireRefusesEveryTruncation(t *testing.T, name string, encoded []byte, fresh func() syntax.Codec) {
	t.Helper()
	if len(encoded) == 0 {
		t.Fatalf("%s: nothing to truncate, so this sweep states nothing", name)
	}
	for cut := 0; cut < len(encoded); cut++ {
		truncated := encoded[:cut]
		if err := syntax.Unmarshal(truncated, fresh()); err == nil {
			t.Errorf("%s truncated to %d of %d octets was accepted by syntax.Unmarshal", name, cut, len(encoded))
		}
		if err := fresh().UnmarshalMLS(syntax.NewReader(truncated)); err == nil {
			t.Errorf("%s truncated to %d of %d octets was accepted by UnmarshalMLS, which is how every enclosing codec in this package reaches it and where there is no Done() to report a swallowed read failure",
				name, cut, len(encoded))
		}
	}
}

func TestWelcomeRefusesEveryTruncation(t *testing.T) {
	welcome := testWelcome()
	encoded, err := syntax.Marshal(&welcome)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	welcomeWireRefusesEveryTruncation(t, "Welcome", encoded, func() syntax.Codec { return &Welcome{} })
}

func TestGroupInfoRefusesEveryTruncation(t *testing.T) {
	groupInfo := testGroupInfo()
	encoded, err := syntax.Marshal(&groupInfo)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	welcomeWireRefusesEveryTruncation(t, "GroupInfo", encoded, func() syntax.Codec { return &GroupInfo{} })
}

func TestGroupSecretsRefusesEveryTruncation(t *testing.T) {
	groupSecrets := testGroupSecrets()
	encoded, err := syntax.Marshal(&groupSecrets)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	welcomeWireRefusesEveryTruncation(t, "GroupSecrets", encoded, func() syntax.Codec { return &GroupSecrets{} })
}

// A nested length that overruns its parent region is refused, and the CONTROL is what makes
// this a statement about the region boundary rather than about the bytes.
//
// The two inputs below differ in exactly one octet -- the secrets<V> region length -- and
// nothing else. With the honest length the whole thing decodes and re-encodes byte exactly.
// With the short one, the element's kem_output<V> declares more octets than the region holds
// while the enclosing MESSAGE still has plenty left, so a decoder that ran the element against
// the parent reader instead of the region's sub reader would read straight through the
// boundary into encrypted_group_info and hand back a Welcome assembled from the wrong fields.
// That is precisely the defect a joiner cannot detect: it holds no group state yet, and there
// is no signature over a Welcome.
func TestWelcomeRefusesANestedLengthThatOverrunsTheSecretsRegion(t *testing.T) {
	// new_member<V> 4 octets, kem_output<V> 8 octets, ciphertext<V> 5 octets = 20 octets
	element := []byte{0x04, 0x44, 0x44, 0x44, 0x44,
		0x08, 0x55, 0x55, 0x55, 0x55, 0x55, 0x55, 0x55, 0x55,
		0x05, 0x66, 0x66, 0x66, 0x66, 0x66}
	if len(element) != 20 {
		t.Fatalf("the hand written element is %d octets and the arithmetic beside it says 20", len(element))
	}
	tail := append([]byte{0x20}, bytes.Repeat([]byte{0x77}, 32)...)
	build := func(regionLength byte) []byte {
		out := []byte{0x00, 0x03, regionLength}
		out = append(out, element...)
		return append(out, tail...)
	}

	honest := build(20)
	decoded := Welcome{}
	if err := syntax.Unmarshal(honest, &decoded); err != nil {
		t.Fatalf("the control input, whose region length covers its one element, was refused: %v", err)
	}
	reencoded, err := syntax.Marshal(&decoded)
	if err != nil {
		t.Fatalf("the control input did not re-encode: %v", err)
	}
	if !bytes.Equal(reencoded, honest) {
		t.Fatalf("the control input re-encoded to %x, want %x", reencoded, honest)
	}

	// nine octets is new_member<V> and the kem_output<V> prefix and three of the eight
	// octets that prefix claims. The remaining five are outside the region and inside the
	// message.
	overrunning := build(9)
	if len(overrunning) != len(honest) {
		t.Fatalf("the two inputs are %d and %d octets; they must differ in the region length alone",
			len(overrunning), len(honest))
	}
	fresh := Welcome{}
	err = syntax.Unmarshal(overrunning, &fresh)
	if !errors.Is(err, syntax.ErrLengthExceedsInput) {
		t.Fatalf("a kem_output<V> declaring 8 octets inside a 9 octet region that has 3 left gave %v, want syntax.ErrLengthExceedsInput",
			err)
	}
	if len(fresh.Secrets) != 0 || fresh.EncryptedGroupInfo != nil {
		t.Errorf("the refused decode published a value into the caller's Welcome: %+v", fresh)
	}
}

// A Welcome with a tail is refused. MLS signs over serialized forms, so a decoder that ignores
// bytes after the value it read accepts two encodings of one object -- and for the structure a
// stranger is handed before any state exists to check it against, that is the whole ballgame.
func TestWelcomeRefusesATail(t *testing.T) {
	welcome := testWelcome()
	encoded, err := syntax.Marshal(&welcome)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, tail := range [][]byte{{0x00}, {0xff}, bytes.Repeat([]byte{0x41}, 64)} {
		withTail := append(bytes.Clone(encoded), tail...)
		decoded := Welcome{}
		if err := syntax.Unmarshal(withTail, &decoded); !errors.Is(err, syntax.ErrTrailingBytes) {
			t.Errorf("a Welcome followed by %d octets of tail gave %v, want syntax.ErrTrailingBytes", len(tail), err)
		}
	}
}

// welcomeWireHostileRuns and welcomeWireHostileBudget mirror the measurement discipline in
// package syntax's alloc_test.go: a byte scale bound, taken tightly, over enough runs that the
// per-call baseline is readable against the noise of a running runtime.
//
// An event count is deliberately NOT the assertion. A single make sized by a declared length
// costs exactly one extra allocation event and scores about 1 against any generous bound,
// which is how the first version of that gate could not fail; a TotalAlloc delta catches the
// same defect at a gigabyte.
const welcomeWireHostileRuns = 200

const welcomeWireHostileBudget = 1 << 18

// A hostile length prefix in a Welcome allocates nothing before it is checked.
//
// Both refusals are swept because they are different checks: a length above the configured
// maximum is ErrLengthExceedsMax and one merely above the bytes remaining is
// ErrLengthExceedsInput, and a decoder can validate against one and size a make before the
// other. The declared sizes -- a gibibyte and a mebibyte -- are three and four orders of
// magnitude above the budget, so a make moved ahead of either check is unmissable.
func TestWelcomeRefusesAHostileLengthWithoutAllocating(t *testing.T) {
	for _, one := range []struct {
		what    string
		welcome []byte
		want    error
	}{
		{
			// encrypted_group_info<V> declaring 2^30-1 octets, the largest a varint can
			// carry, in a message that carries none of them
			what:    "encrypted_group_info claiming a gibibyte",
			welcome: append([]byte{0x00, 0x03, 0x00}, 0xbf, 0xff, 0xff, 0xff),
			want:    syntax.ErrLengthExceedsMax,
		},
		{
			// and one just above MaxVectorLength's mebibyte, which clears no check by
			// being absurd
			what:    "encrypted_group_info claiming a mebibyte and one",
			welcome: append([]byte{0x00, 0x03, 0x00}, 0x80, 0x10, 0x00, 0x01),
			want:    syntax.ErrLengthExceedsMax,
		},
		{
			// the secrets<V> region, which is where a stranger's bytes first choose a
			// length. ReadVector reaches its region through ReadSub, so this one is a
			// statement about the ITEM slice's capacity hint rather than about the
			// region: the hint is bounded by a constant and not by the declared length.
			what:    "the secrets region claiming a gibibyte",
			welcome: append([]byte{0x00, 0x03}, 0xbf, 0xff, 0xff, 0xff),
			want:    syntax.ErrLengthExceedsMax,
		},
		{
			// a length inside the maximum and beyond the input, which is the other
			// sentinel and therefore the other check
			what:    "encrypted_group_info claiming more than the message holds",
			welcome: append([]byte{0x00, 0x03, 0x00}, 0x7f, 0xff),
			want:    syntax.ErrLengthExceedsInput,
		},
	} {
		decoded := Welcome{}
		if err := syntax.Unmarshal(one.welcome, &decoded); !errors.Is(err, one.want) {
			t.Errorf("%s gave %v, want %v", one.what, err, one.want)
			continue
		}
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		for i := 0; i < welcomeWireHostileRuns; i++ {
			sink := Welcome{}
			_ = syntax.Unmarshal(one.welcome, &sink)
		}
		runtime.ReadMemStats(&after)
		if grew := after.TotalAlloc - before.TotalAlloc; grew > welcomeWireHostileBudget {
			t.Errorf("%s: %d refusals allocated %d bytes, budget %d -- a declared length is sizing a make before it is checked",
				one.what, welcomeWireHostileRuns, grew, welcomeWireHostileBudget)
		}
	}
}

// ---------------------------------------------------------------------------
// the published corpus
// ---------------------------------------------------------------------------

// welcomeWireMessagesVector is the three fields of one mlswg `messages` case that this task
// owns. The other fourteen belong to other codecs and are not read here.
type welcomeWireMessagesVector struct {
	GroupInfo    string `json:"mls_group_info"`
	Welcome      string `json:"mls_welcome"`
	GroupSecrets string `json:"group_secrets"`
}

// welcomeWireMessagesCases is how many cases messages.json carries. Asserted rather than
// assumed, for the reason every count in this package is: a corpus that shrank, or a field
// that stopped being present, turns the sweep below into a loop that runs zero times and
// reports exactly what a complete sweep reports.
const welcomeWireMessagesCases = 300

// welcomeWireMlsMessageHeader is `version | wire_format` in front of a GroupInfo or a Welcome
// inside an MLSMessage. MLSMessage itself is another task's type, so the header is stripped
// here by hand and CHECKED rather than skipped: a wire format that stopped matching would slide
// every field of the structure by two octets and the round trip would still be byte exact.
func welcomeWireMlsMessageHeader(t *testing.T, wireFormat uint16) []byte {
	t.Helper()
	writer := syntax.NewWriter()
	writer.WriteUint16(uint16(ProtocolVersionMls10))
	writer.WriteUint16(wireFormat)
	header, err := writer.Bytes()
	if err != nil {
		t.Fatalf("build the mls message header: %v", err)
	}
	return header
}

// welcomeWireStripHeader takes the structure out of an MLSMessage, refusing anything that is
// not headed the way this wire format says it is.
func welcomeWireStripHeader(t *testing.T, at string, message []byte, wireFormat uint16) []byte {
	t.Helper()
	header := welcomeWireMlsMessageHeader(t, wireFormat)
	if len(message) <= len(header) || !bytes.HasPrefix(message, header) {
		t.Fatalf("%s: the published message is %d octets headed %x, want the mls10 header %x for wire format %#04x",
			at, len(message), message[:min(len(message), len(header))], header, wireFormat)
	}
	return message[len(header):]
}

// welcomeWireRoundTripsPublished decodes one published encoding and requires the re-encoding to
// be byte exact.
//
// Byte exactness against bytes THIS PACKAGE DID NOT WRITE is the assertion the goldens above
// scale up: it sees a field order this file and the RFC disagree about, a length prefix width
// this codec chose differently, and a field the encoder emits that the corpus does not carry.
// A self round trip sees none of those.
func welcomeWireRoundTripsPublished(t *testing.T, at string, encoded []byte, value syntax.Codec) bool {
	t.Helper()
	if err := syntax.Unmarshal(encoded, value); err != nil {
		t.Errorf("%s: the published encoding was refused: %v", at, err)
		return false
	}
	reencoded, err := syntax.Marshal(value)
	if err != nil {
		t.Errorf("%s: re-encode: %v", at, err)
		return false
	}
	if !bytes.Equal(reencoded, encoded) {
		t.Errorf("%s: re-encoded to\n  %x\nand the published encoding is\n  %x", at, reencoded, encoded)
		return false
	}
	return true
}

// TestTheMessagesCorpusGroupInfoWelcomeAndGroupSecretsRoundTripByteExactly is the interop half
// of this task: 300 published GroupInfos, 300 Welcomes and 300 GroupSecrets, none of which
// this package produced, each decoded and re-encoded to the same octets.
//
// The corpus is read directly rather than through a registered vector family: families 8 and 12
// are owned by later tasks, and installing a runner here would move a decision that belongs to
// them.
func TestTheMessagesCorpusGroupInfoWelcomeAndGroupSecretsRoundTripByteExactly(t *testing.T) {
	entries := LoadVectorFile(t, "messages.json")
	if len(entries) != welcomeWireMessagesCases {
		t.Fatalf("messages.json carries %d cases, want %d", len(entries), welcomeWireMessagesCases)
	}
	groupInfos, welcomes, groupSecrets := 0, 0, 0
	for i, raw := range entries {
		vector := welcomeWireMessagesVector{}
		if err := json.Unmarshal(raw, &vector); err != nil {
			t.Fatalf("case %d: parse: %v", i, err)
		}
		at := fmt.Sprintf("messages.json case %d", i)

		encoded := welcomeWireStripHeader(t, at+" mls_group_info",
			MustHex(t, vector.GroupInfo), welcomeWireGroupInfoWireFormat)
		if welcomeWireRoundTripsPublished(t, at+" GroupInfo", encoded, &GroupInfo{}) {
			groupInfos++
		}

		encoded = welcomeWireStripHeader(t, at+" mls_welcome",
			MustHex(t, vector.Welcome), welcomeWireWelcomeWireFormat)
		if welcomeWireRoundTripsPublished(t, at+" Welcome", encoded, &Welcome{}) {
			welcomes++
		}

		if welcomeWireRoundTripsPublished(t, at+" GroupSecrets",
			MustHex(t, vector.GroupSecrets), &GroupSecrets{}) {
			groupSecrets++
		}
	}
	if groupInfos != welcomeWireMessagesCases || welcomes != welcomeWireMessagesCases ||
		groupSecrets != welcomeWireMessagesCases {
		t.Fatalf("round tripped %d group infos, %d welcomes and %d group secrets, want %d of each",
			groupInfos, welcomes, groupSecrets, welcomeWireMessagesCases)
	}
}

// The two RFC 9420 section 6.1 wire formats this file strips. Spelled here rather than reused
// from another file's constant so that a change to one of them is visible in the test that
// depends on it.
const (
	welcomeWireWelcomeWireFormat   uint16 = 0x0003
	welcomeWireGroupInfoWireFormat uint16 = 0x0004
)

// A published GroupSecrets carries fields the round trip alone cannot tell apart from each
// other, so this reads them.
//
// The whole corpus encodes joiner_secret at KDF.Nh and a present path_secret at the same
// length, which means the two are ADJACENT SAME WIDTH FIELDS on the wire with only a presence
// octet between them. A codec that emitted them in the other order round trips perfectly and is
// byte exact against the corpus in both directions. What separates them is that the corpus
// itself puts them in one order and this test names which value it expects where.
func TestTheMessagesCorpusGroupSecretsCarriesItsFieldsInSectionTwelveFourOrder(t *testing.T) {
	entries := LoadVectorFile(t, "messages.json")
	read := 0
	for i, raw := range entries {
		vector := welcomeWireMessagesVector{}
		if err := json.Unmarshal(raw, &vector); err != nil {
			t.Fatalf("case %d: parse: %v", i, err)
		}
		published := MustHex(t, vector.GroupSecrets)
		decoded := GroupSecrets{}
		if err := syntax.Unmarshal(published, &decoded); err != nil {
			t.Fatalf("case %d: the published group secrets were refused: %v", i, err)
		}
		// joiner_secret is the FIRST field, so it is the one whose content begins at
		// offset 1 of the published encoding. Read straight out of the bytes rather than
		// out of the decode, which is what makes this a statement about field order.
		if len(published) < 2 {
			t.Fatalf("case %d: the published group secrets are %d octets", i, len(published))
		}
		length := int(published[0])
		if length == 0 || length >= 64 || 1+length > len(published) {
			t.Fatalf("case %d: the leading opaque<V> prefix is %#02x, which this reader cannot follow",
				i, published[0])
		}
		if !bytes.Equal(decoded.JoinerSecret, published[1:1+length]) {
			t.Errorf("case %d: joiner_secret decoded as %x and the first field of the published encoding is %x",
				i, decoded.JoinerSecret, published[1:1+length])
		}
		if decoded.PathSecret != nil && bytes.Equal(decoded.PathSecret.PathSecret, decoded.JoinerSecret) {
			t.Errorf("case %d: joiner_secret and path_secret decoded to the same octets, so this case cannot tell the two apart",
				i)
		}
		read++
	}
	if read != welcomeWireMessagesCases {
		t.Fatalf("read %d published group secrets, want %d", read, welcomeWireMessagesCases)
	}
}

// ---------------------------------------------------------------------------
// the encrypted-group-info seam
// ---------------------------------------------------------------------------

// TestThisWelcomeCodecHandsWelcomeKeyNonceTheCiphertextItExpects is the seam nothing had
// checked.
//
// p4's WelcomeKeyNonce derives the key and the nonce that open a Welcome's
// encrypted_group_info, and this task defines where those octets sit in a Welcome. The two were
// written by different plans against the same RFC section, and until now the only reader that
// had ever fed one to the other was a HAND ROLLED parser inside key_schedule_test.go, written
// deliberately because this type did not exist yet. So there was no statement anywhere that the
// field this codec calls EncryptedGroupInfo is the ciphertext that derivation opens.
//
// It is stated here in the strongest form available: the published Welcome is parsed by THIS
// codec, the group secrets are opened with the published init key, the walk down to
// welcome_secret is key_schedule_test.go's hand written reference rather than the key schedule,
// and the AEAD tag over encrypted_group_info is what decides the row. A field boundary off by
// one octet, the two ciphertexts transposed, or the group info taken from the wrong place all
// fail the tag; nothing this package computes for itself could say that.
//
// And the last step closes the loop back onto this file: what the AEAD returns is decoded as a
// GroupInfo by this task's codec and re-encoded, byte exactly. So the same test covers the
// Welcome codec, the GroupInfo codec, the GroupSecrets codec and the derivation between them.
func TestThisWelcomeCodecHandsWelcomeKeyNonceTheCiphertextItExpects(t *testing.T) {
	vectors := []ksWelcomeVector{}
	loadLabelKat(t, "welcome.json", &vectors)
	opened := 0
	for _, vector := range vectors {
		suite := CipherSuite(vector.CipherSuite)
		if !IsRegisteredSuite(suite) {
			continue
		}
		at := fmt.Sprintf("suite %#04x", uint16(suite))
		crypto := mustProvider(t, suite)
		published := mustDecodeHex(t, "the published welcome "+at, vector.Welcome)

		welcome := Welcome{}
		encoded := welcomeWireStripHeader(t, at, published, welcomeWireWelcomeWireFormat)
		if err := syntax.Unmarshal(encoded, &welcome); err != nil {
			t.Fatalf("%s: this package's Welcome codec refused the published welcome: %v", at, err)
		}
		if welcome.CipherSuite != suite {
			t.Fatalf("%s: the decoded welcome names suite %#04x", at, uint16(welcome.CipherSuite))
		}

		// the differential against the hand rolled reader key_schedule_test.go still
		// carries. Two independent parses of the same octets, and the fields they hand back
		// have to be the same fields -- which is what says this codec's field NAMES line up
		// with the structure that reader was written from.
		handRolled, handRolledGroupInfo := ksWelcomeMessage(t, at, suite, published)
		if len(handRolled) != len(welcome.Secrets) {
			t.Fatalf("%s: the hand rolled reader found %d encrypted group secrets and this codec found %d",
				at, len(handRolled), len(welcome.Secrets))
		}
		for i := range handRolled {
			if !bytes.Equal(handRolled[i].newMember, welcome.Secrets[i].NewMember) ||
				!bytes.Equal(handRolled[i].kemOutput, welcome.Secrets[i].EncryptedGroupSecrets.KemOutput) ||
				!bytes.Equal(handRolled[i].ciphertext, welcome.Secrets[i].EncryptedGroupSecrets.Ciphertext) {
				t.Fatalf("%s: entry %d differs between the hand rolled reader and this codec", at, i)
			}
		}
		if !bytes.Equal(handRolledGroupInfo, welcome.EncryptedGroupInfo) {
			t.Fatalf("%s: the hand rolled reader read encrypted_group_info as %d octets and this codec as %d",
				at, len(handRolledGroupInfo), len(welcome.EncryptedGroupInfo))
		}

		// the entry this Welcome is addressed to, found by the reference of the published
		// key package rather than by taking the first
		message := mustDecodeHex(t, "the published key package "+at, vector.KeyPackage)
		if !bytes.HasPrefix(message, mlsMessageKeyPackageHeader) {
			t.Fatalf("%s: the published key package is not headed with the mls10 key package header %x",
				at, mlsMessageKeyPackageHeader)
		}
		reference := mustKeyPackageRef(t, crypto, message[len(mlsMessageKeyPackageHeader):])
		addressed := -1
		for i, one := range welcome.Secrets {
			if bytes.Equal(one.NewMember, reference) {
				addressed = i
			}
		}
		if addressed < 0 {
			t.Fatalf("%s: none of the %d entries this codec decoded is addressed to the published key package",
				at, len(welcome.Secrets))
		}

		// EncryptWithLabel(init_key, "Welcome", encrypted_group_info, GroupSecrets), RFC 9420
		// section 12.4.3.1: the context is the OTHER ciphertext of the same Welcome, which is
		// what binds the two halves together -- and it is this codec's EncryptedGroupInfo
		// field that is being handed over as that context.
		plaintext, err := DecryptWithLabel(crypto,
			HpkePrivateKey(mustDecodeHex(t, "init_priv "+at, vector.InitPriv)),
			"Welcome", welcome.EncryptedGroupInfo,
			welcome.Secrets[addressed].EncryptedGroupSecrets.KemOutput,
			welcome.Secrets[addressed].EncryptedGroupSecrets.Ciphertext)
		if err != nil {
			t.Fatalf("%s: open the encrypted group secrets this codec decoded, with the published init_priv: %v",
				at, err)
		}

		// and the plaintext is a GroupSecrets by this task's codec, byte exactly
		groupSecrets := GroupSecrets{}
		if err := syntax.Unmarshal(plaintext, &groupSecrets); err != nil {
			t.Fatalf("%s: this package's GroupSecrets codec refused the decrypted plaintext: %v", at, err)
		}
		reencoded, err := syntax.Marshal(&groupSecrets)
		if err != nil {
			t.Fatalf("%s: re-encode the decrypted group secrets: %v", at, err)
		}
		if !bytes.Equal(reencoded, plaintext) {
			t.Fatalf("%s: the decrypted group secrets re-encoded to %x, want %x", at, reencoded, plaintext)
		}
		if len(groupSecrets.Psks) != 0 {
			t.Fatalf("%s: the published welcome names %d psks, so psk_secret is not the zero secret and this row cannot reach welcome_secret",
				at, len(groupSecrets.Psks))
		}
		if len(groupSecrets.JoinerSecret) != crypto.HashSize() {
			t.Fatalf("%s: the decoded joiner_secret is %d octets, want KDF.Nh = %d",
				at, len(groupSecrets.JoinerSecret), crypto.HashSize())
		}

		// the two hand written steps down to welcome_secret, key_schedule_test.go's
		// reference rather than the key schedule, so a failure here names this seam
		memberSecret := ksHandExtract(groupSecrets.JoinerSecret, make([]byte, crypto.HashSize()))
		welcomeSecret := ksHandDeriveSecret(t, memberSecret, "welcome")
		key, nonce, err := WelcomeKeyNonce(crypto, welcomeSecret)
		if err != nil {
			t.Fatalf("%s: WelcomeKeyNonce: %v", at, err)
		}
		groupInfoBytes, err := crypto.AeadOpen(key, nonce, nil, welcome.EncryptedGroupInfo)
		if err != nil {
			t.Fatalf("%s: the encrypted_group_info THIS CODEC decoded does not open under the key and nonce WelcomeKeyNonce derived: %v",
				at, err)
		}

		// the loop closes: what came out is a GroupInfo by this task's codec, and it
		// re-encodes to the same octets the AEAD produced
		groupInfo := GroupInfo{}
		if err := syntax.Unmarshal(groupInfoBytes, &groupInfo); err != nil {
			t.Fatalf("%s: this package's GroupInfo codec refused the opened group info: %v", at, err)
		}
		reencoded, err = syntax.Marshal(&groupInfo)
		if err != nil {
			t.Fatalf("%s: re-encode the opened group info: %v", at, err)
		}
		if !bytes.Equal(reencoded, groupInfoBytes) {
			t.Fatalf("%s: the opened group info re-encoded to\n  %x\nwant\n  %x", at, reencoded, groupInfoBytes)
		}
		if groupInfo.GroupContext.Version != ProtocolVersionMls10 || groupInfo.GroupContext.CipherSuite != suite {
			t.Errorf("%s: the opened group info carries version %#04x and suite %#04x",
				at, uint16(groupInfo.GroupContext.Version), uint16(groupInfo.GroupContext.CipherSuite))
		}
		if len(groupInfo.ConfirmationTag) != crypto.HashSize() {
			t.Errorf("%s: the opened group info's confirmation_tag is %d octets, want KDF.Nh = %d",
				at, len(groupInfo.ConfirmationTag), crypto.HashSize())
		}
		if len(groupInfo.Signature) == 0 {
			t.Errorf("%s: the opened group info carries no signature", at)
		}

		// the control on the comparison itself: what decides this row is the AEAD tag, so a
		// key or a nonce one bit out has to be refused. Without it, the open above is
		// satisfied by an AEAD that ignored both.
		for _, wrong := range []struct {
			what  string
			key   []byte
			nonce []byte
		}{
			{what: "a welcome key one bit out", key: ksFlippedFirstByte(key), nonce: nonce},
			{what: "a welcome nonce one bit out", key: key, nonce: ksFlippedFirstByte(nonce)},
		} {
			if _, err := crypto.AeadOpen(wrong.key, wrong.nonce, nil, welcome.EncryptedGroupInfo); err == nil {
				t.Errorf("%s: the encrypted_group_info opened under %s, so the open above pins nothing",
					at, wrong.what)
			}
		}
		opened++
	}
	if opened != ksWelcomeKatVectors {
		t.Fatalf("opened %d published welcomes through this codec, want %d", opened, ksWelcomeKatVectors)
	}
}

// TestARefusedDecodeLeavesTheCallersValueAlone is the property four comments in
// welcome_wire.go claim and nothing observed.
//
// Every decoder in that file stages into a local and publishes the receiver whole. What it
// costs when that stops being true is not tidiness. A decoder that assigned as it read would
// leave a REFUSED GroupInfo holding a group context out of the sender's bytes and a
// confirmation tag out of whatever the caller's value held before -- a well formed object
// describing an epoch nobody published, and one whose signature was taken over a different
// one. For EncryptedGroupSecrets it is worse, because a joiner SCANS that vector for the entry
// its own key package reference matches: a half filled entry is one that can still be matched,
// pairing a reference out of these bytes with a ciphertext out of the previous decode.
//
// The sweep is every truncation rather than one, because which field a decoder publishes early
// decides which truncations can see it, and a single cut chosen here would be a guess at that.
// The decode goes through UnmarshalMLS directly: syntax.Unmarshal's contract is about the
// bytes, and this is a statement about the receiver.
//
// The TABLE is four rows and this package declares thirty one decoders, which is the shape rule
// 5 exists for and which duly understated the class: the same edit that fails GroupInfo here
// survived the whole suite in Commit, because Commit had no row. What decides membership now is
// decoder_publish_test.go, which derives the verdict for every UnmarshalMLS off the source and
// pins it; this file is what says what the verdict is worth for the four structures it owns, and
// commit_wire_test.go does the same for Commit.
func TestARefusedDecodeLeavesTheCallersValueAlone(t *testing.T) {
	for _, one := range []struct {
		name  string
		valid func() syntax.Codec
		held  func() syntax.Codec
	}{
		{
			name:  "GroupInfo",
			valid: func() syntax.Codec { value := testGroupInfo(); return &value },
			held: func() syntax.Codec {
				value := testGroupInfo()
				value.GroupContext.GroupId = []byte{0xb0, 0xb1, 0xb2}
				value.ConfirmationTag = bytes.Repeat([]byte{0xbb}, 32)
				value.Signer = 0x0b0b0b0b
				value.Signature = []byte{0xbe, 0xef}
				return &value
			},
		},
		{
			name:  "GroupSecrets",
			valid: func() syntax.Codec { value := testGroupSecrets(); return &value },
			held: func() syntax.Codec {
				return &GroupSecrets{JoinerSecret: bytes.Repeat([]byte{0xab}, 6)}
			},
		},
		{
			name:  "Welcome",
			valid: func() syntax.Codec { value := testWelcome(); return &value },
			held: func() syntax.Codec {
				return &Welcome{
					CipherSuite:        CipherSuiteX25519AesGcm128Sha256Ed25519,
					EncryptedGroupInfo: bytes.Repeat([]byte{0xcc}, 3),
				}
			},
		},
		{
			name: "EncryptedGroupSecrets",
			valid: func() syntax.Codec {
				value := testWelcome().Secrets[0]
				return &value
			},
			held: func() syntax.Codec {
				return &EncryptedGroupSecrets{
					NewMember: []byte{0xd0, 0xd1},
					EncryptedGroupSecrets: HpkeCiphertext{
						KemOutput:  []byte{0xd2},
						Ciphertext: []byte{0xd3, 0xd4},
					},
				}
			},
		},
	} {
		encoded, err := syntax.Marshal(one.valid())
		if err != nil {
			t.Fatalf("%s: encode the input the truncations are cut from: %v", one.name, err)
		}
		heldBytes, err := syntax.Marshal(one.held())
		if err != nil {
			t.Fatalf("%s: encode the value the caller already holds: %v", one.name, err)
		}
		if bytes.Equal(heldBytes, encoded) {
			t.Fatalf("%s: the held value and the decoded one encode identically, so this case cannot tell an untouched receiver from a clobbered one",
				one.name)
		}
		for cut := 0; cut < len(encoded); cut++ {
			receiver := one.held()
			if err := receiver.UnmarshalMLS(syntax.NewReader(encoded[:cut])); err == nil {
				t.Fatalf("%s truncated to %d of %d octets was accepted", one.name, cut, len(encoded))
			}
			after, err := syntax.Marshal(receiver)
			if err != nil {
				t.Fatalf("%s: re-encode the receiver after a refused decode at %d: %v", one.name, cut, err)
			}
			if !bytes.Equal(after, heldBytes) {
				t.Fatalf("%s: a decode refused at %d of %d octets left the caller's value as\n  %x\nand it was\n  %x",
					one.name, cut, len(encoded), after, heldBytes)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// the limit this product's own group needs, measured one level up from the tree
// ---------------------------------------------------------------------------

// welcomeWireProductGroupInfo is a GroupInfo carrying the ratchet_tree extension of the group
// MASTER sizes this product for, built out of a real tree rather than out of a byte count.
//
// Out of a real tree because the number is the whole point: 500 members with two devices each is
// productGroupLeafCount leaves, every leaf of this profile carries a 1216 octet X-Wing key in an
// urmessage_leaf_keys extension, and the array that produces is about 1.33 MiB. A fixture that
// stated 1394000 as a literal would go on passing after the leaf grew or the cap moved, which is
// exactly when this measurement stops being true.
func welcomeWireProductGroupInfo(t *testing.T, crypto CryptoProvider) (GroupInfo, []byte) {
	t.Helper()
	tree, _ := newTestTree(t, crypto, productGroupLeafCount)
	extension, err := tree.Encode()
	if err != nil {
		t.Fatalf("encode a %d leaf ratchet tree as an extension: %v", productGroupLeafCount, err)
	}
	groupInfo := testGroupInfo()
	groupInfo.Extensions = []Extension{extension}
	return groupInfo, extension.ExtensionData
}

// TestAGroupInfoAndAWelcomeCarryingThisProductsTreeNeedTheRaisedLimitInBothDirections is the
// obligation p7 inherits, recorded as a measurement rather than left in a comment.
//
// tree.go wires the ratchet_tree ARRAY to MaxRatchetTreeLength, and
// TestTheRatchetTreeCodecIsHandedTheRaisedLimitAtTheProductsGroupSize holds that. What neither
// of them reaches is the structure the array TRAVELS IN. A ratchet_tree extension is carried as
// an Extension whose ExtensionData is a plain opaque<V>, so a GroupInfo decode meets that length
// prefix through ReadOpaque and refuses it against whatever limit ITS caller opened, before
// tree.go's raised entry point is anywhere in the call. The same again one level up: the Welcome
// that seals the GroupInfo carries it as encrypted_group_info, another opaque<V>.
//
// So the two codecs are correct -- both take the caller's Reader and Writer, which is what lets
// a caller open them at the bound the payload needs -- and the DEFAULT limit refuses this
// product's own group in all four directions. Nothing in this tree opens either at
// MaxRatchetTreeLength today: this package enters the codec at the raised bound in exactly two
// places, tree.go's marshalRatchetTree and UnmarshalRatchetTree, and neither is reachable from a
// GroupInfo decode. Whoever writes the group lifecycle owes those entry points, and this is the
// measurement that says what they owe. The refusals are asserted as ErrLengthExceedsMax rather
// than as any error, because a refusal for some other reason would be a fixture that had stopped
// being the thing under test.
//
// The four directions are failed SEPARATELY for the reason the tree test states: a raise wired
// into the decode alone still refuses to PUBLISH this product's own group info, and a Welcome
// that encodes and cannot be read is the same defect wearing the other shoe. Each is asserted
// behind a size guard: a fixture that shrank below MaxVectorLength would leave everything here
// passing against either limit while reporting a clean bill over the one decision it exists to
// make.
func TestAGroupInfoAndAWelcomeCarryingThisProductsTreeNeedTheRaisedLimitInBothDirections(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	groupInfo, treeBody := welcomeWireProductGroupInfo(t, crypto)

	groupInfoBytes, err := syntax.MarshalLimit(&groupInfo, syntax.MaxRatchetTreeLength)
	if err != nil {
		t.Fatalf("a group info carrying a %d leaf ratchet tree does not encode at MaxRatchetTreeLength: %v; this is the encode the raised bound exists for",
			productGroupLeafCount, err)
	}
	t.Logf("a group info carrying a %d leaf tree (%d members x 2 devices) encodes to %d octets, the tree body alone to %d; MaxVectorLength is %d and MaxRatchetTreeLength is %d",
		productGroupLeafCount, productGroupLeafCount/2, len(groupInfoBytes), len(treeBody),
		syntax.MaxVectorLength, syntax.MaxRatchetTreeLength)

	// the Welcome that seals it. Sealed rather than padded to a chosen length, so the
	// ciphertext measured here is the one a joiner is handed and its size is the AEAD's answer
	// rather than a number this test picked.
	key := bytes.Repeat([]byte{0x5c}, crypto.KeySize())
	nonce := bytes.Repeat([]byte{0x3d}, crypto.NonceSize())
	sealed, err := crypto.AeadSeal(key, nonce, nil, groupInfoBytes)
	if err != nil {
		t.Fatalf("seal a %d octet group info under a welcome key: %v", len(groupInfoBytes), err)
	}
	welcome := testWelcome()
	welcome.EncryptedGroupInfo = sealed
	welcomeBytes, err := syntax.MarshalLimit(&welcome, syntax.MaxRatchetTreeLength)
	if err != nil {
		t.Fatalf("a welcome carrying a %d octet encrypted_group_info does not encode at MaxRatchetTreeLength: %v",
			len(sealed), err)
	}

	for _, one := range []struct {
		what    string
		encoded []byte
		value   syntax.Codec
		fresh   func() syntax.Codec
	}{
		{
			what:    "a group info carrying this product's ratchet_tree extension",
			encoded: groupInfoBytes,
			value:   &groupInfo,
			fresh:   func() syntax.Codec { return &GroupInfo{} },
		},
		{
			what:    "the welcome that seals it",
			encoded: welcomeBytes,
			value:   &welcome,
			fresh:   func() syntax.Codec { return &Welcome{} },
		},
	} {
		// the guards that keep the two limits apart. Without the first, everything below
		// holds under either bound and states nothing; without the second, this product does
		// not fit the bound p1 raised for it, which is the regression worth failing on.
		if len(one.encoded) <= syntax.MaxVectorLength {
			t.Fatalf("%s encodes to %d octets, which the default limit of %d accepts, so nothing below can tell the two limits apart",
				one.what, len(one.encoded), syntax.MaxVectorLength)
		}
		if len(one.encoded) > syntax.MaxRatchetTreeLength {
			t.Fatalf("%s encodes to %d octets and the ratchet tree bound is %d, so this product's own group does not fit the limit p1 raised for it",
				one.what, len(one.encoded), syntax.MaxRatchetTreeLength)
		}

		// the ENCODE at the default limit refuses it, which is the direction a raise wired
		// only into the decode would leave broken
		if _, err := syntax.Marshal(one.value); !errors.Is(err, syntax.ErrLengthExceedsMax) {
			t.Errorf("%s: syntax.Marshal answered %v, want syntax.ErrLengthExceedsMax; a member running the default bound cannot publish this legal group at all",
				one.what, err)
		}

		// and the DECODE at the default limit refuses it, at the length prefix of the
		// opaque<V> the payload travels in rather than anywhere inside the tree codec
		if err := one.fresh().UnmarshalMLS(syntax.NewReader(one.encoded)); !errors.Is(err, syntax.ErrLengthExceedsMax) {
			t.Errorf("%s: the codec answered %v to a reader opened at the default limit, want syntax.ErrLengthExceedsMax; a peer running that bound reports this legal group as a corrupt message",
				one.what, err)
		}
		if err := syntax.Unmarshal(one.encoded, one.fresh()); !errors.Is(err, syntax.ErrLengthExceedsMax) {
			t.Errorf("%s: syntax.Unmarshal answered %v, want syntax.ErrLengthExceedsMax", one.what, err)
		}

		// at the raised bound both directions succeed, and the round trip is byte exact
		decoded := one.fresh()
		if err := syntax.UnmarshalLimit(one.encoded, decoded, syntax.MaxRatchetTreeLength); err != nil {
			t.Fatalf("%s: decoding at MaxRatchetTreeLength answered %v; this is the decode the raised bound exists for",
				one.what, err)
		}
		reencoded, err := syntax.MarshalLimit(decoded, syntax.MaxRatchetTreeLength)
		if err != nil {
			t.Fatalf("%s: re-encoding at MaxRatchetTreeLength answered %v", one.what, err)
		}
		if !bytes.Equal(reencoded, one.encoded) {
			t.Errorf("%s: the re-encoding at the raised bound differs from the encoding", one.what)
		}
	}

	// and what came back out of the raised decode is this product's tree rather than an opaque
	// of the right length
	out := GroupInfo{}
	if err := syntax.UnmarshalLimit(groupInfoBytes, &out, syntax.MaxRatchetTreeLength); err != nil {
		t.Fatalf("decode the group info at MaxRatchetTreeLength: %v", err)
	}
	if len(out.Extensions) != 1 || out.Extensions[0].ExtensionType != ExtensionTypeRatchetTree {
		t.Fatalf("the decoded group info carries %d extensions, want one ratchet_tree", len(out.Extensions))
	}
	if !bytes.Equal(out.Extensions[0].ExtensionData, treeBody) {
		t.Errorf("the ratchet_tree body that came back is %d octets and the one that went in is %d",
			len(out.Extensions[0].ExtensionData), len(treeBody))
	}
	// the body still needs tree.go's own raised entry point to become a tree, which is the
	// distinction this whole test is about: the extension travelled as an opaque and nothing
	// in the GroupInfo decode parsed it
	if _, err := UnmarshalRatchetTree(out.Extensions[0].ExtensionData); err != nil {
		t.Errorf("the ratchet_tree body carried through a group info does not decode: %v", err)
	}
}
