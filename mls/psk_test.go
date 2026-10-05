// Tests for the RFC 9420 section 8.4 pre shared key identifier.
//
// The codec here owes what GroupContext's owes and for the same reason: MLS signs and
// hashes over serialized forms, so byte exactness is the property and round trip
// symmetry cannot see a violation of it. An encoder and a decoder that both write the
// psktype at two octets round trip perfectly against each other and disagree with
// everybody else, and the disagreement surfaces as a psk_secret nobody else derives.
//
// So the goldens below are hand derived from the RFC with their arithmetic written out,
// and the external arm is checked against three hundred encodings the mlswg reference
// implementation produced, which are in this tree already, pinned by digest, and were
// written without reference to this package.
package mls

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect/mls/syntax"
)

// pskTestCrypto returns the ciphersuite 0x0003 provider the psk tests are pinned
// against.
func pskTestCrypto(t *testing.T) CryptoProvider {
	t.Helper()
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	return crypto
}

// pskHashSizeProvider is a provider whose KDF output length is whatever the test says,
// and it exists because both registered ciphersuites have Nh = 32. A ValSem401 that
// compared against a literal 32 would pass every test this package can build out of the
// real registry, and would start accepting half length nonces on the day a suite with a
// wider hash is registered — which the still draft post quantum ciphersuites make a near
// certainty. Overriding the one method the rule reads is what makes the derivation
// observable now rather than then.
//
// Everything else is delegated, so this is the real provider in every other respect.
type pskHashSizeProvider struct {
	CryptoProvider
	hashSize int
}

// HashSize reports the length this fixture was built for.
func (self pskHashSizeProvider) HashSize() int { return self.hashSize }

// pskCryptoWithHashSize wraps the suite 0x0003 provider at a chosen KDF.Nh.
func pskCryptoWithHashSize(t *testing.T, hashSize int) CryptoProvider {
	t.Helper()
	return pskHashSizeProvider{CryptoProvider: pskTestCrypto(t), hashSize: hashSize}
}

// clonePreSharedKeyId copies an id so a test that mutates one field cannot reach the
// fixture every other case is built from.
func clonePreSharedKeyId(id *PreSharedKeyId) *PreSharedKeyId {
	clone := *id
	clone.PskId = cloneBytes(id.PskId)
	clone.PskGroupId = cloneBytes(id.PskGroupId)
	clone.PskNonce = cloneBytes(id.PskNonce)
	return &clone
}

// preSharedKeyIdsAgree compares two ids field by field, treating a nil byte slice and an
// empty one as equal because the wire format has one spelling for both and the decoder
// always produces the non nil form.
func preSharedKeyIdsAgree(left *PreSharedKeyId, right *PreSharedKeyId) bool {
	return left.PskType == right.PskType &&
		left.Usage == right.Usage &&
		left.PskEpoch == right.PskEpoch &&
		bytes.Equal(left.PskId, right.PskId) &&
		bytes.Equal(left.PskGroupId, right.PskGroupId) &&
		bytes.Equal(left.PskNonce, right.PskNonce)
}

// describePreSharedKeyId names a corpus case in a failure message, since the cases below
// are generated and there is no table row to point at.
func describePreSharedKeyId(id *PreSharedKeyId) string {
	return fmt.Sprintf("{psktype %d usage %d id %d gid %d epoch %#016x nonce %d}",
		uint8(id.PskType), uint8(id.Usage), len(id.PskId), len(id.PskGroupId),
		id.PskEpoch, len(id.PskNonce))
}

// ---------------------------------------------------------------------------
// the hand derived goldens
// ---------------------------------------------------------------------------

// handDerivedExternalGolden is the external arm written out from RFC 9420 section 8.4
// and the section 2.1 varint, one argument per field:
//
//	psktype           external is code point 1                        -> 01
//	psk_id<V>         two octets, so a one octet prefix of 02          -> 02 aa bb
//	psk_nonce<V>      three octets, so a one octet prefix of 03        -> 03 01 02 03
//
// 1 + (1+2) + (1+3) = 8 octets. There is no usage, no group id and no epoch: the select()
// arm decides which fields exist, and an encoder that wrote a resumption field here would
// be describing a key the receiver cannot look up.
func handDerivedExternalGolden() []byte {
	return joinBytes(
		[]byte{0x01},
		[]byte{0x02, 0xaa, 0xbb},
		[]byte{0x03, 0x01, 0x02, 0x03},
	)
}

// handDerivedExternalGoldenId is the value that golden describes.
func handDerivedExternalGoldenId() *PreSharedKeyId {
	return &PreSharedKeyId{
		PskType:  PskTypeExternal,
		PskId:    []byte{0xaa, 0xbb},
		PskNonce: []byte{0x01, 0x02, 0x03},
	}
}

// handDerivedResumptionGolden is the resumption arm, same derivation:
//
//	psktype           resumption is code point 2                      -> 02
//	usage             application is code point 1                     -> 01
//	psk_group_id<V>   one octet, so a one octet prefix of 01           -> 01 cc
//	psk_epoch         a bare uint64, big endian, no length prefix     -> 00..07
//	psk_nonce<V>      one octet                                        -> 01 09
//
// 1 + 1 + (1+1) + 8 + (1+1) = 14 octets. The epoch is the field a codec is most likely to
// get wrong and least likely to notice: written at four octets it still decodes, still
// round trips against itself, and moves every subsequent field by four.
func handDerivedResumptionGolden() []byte {
	return joinBytes(
		[]byte{0x02},
		[]byte{0x01},
		[]byte{0x01, 0xcc},
		[]byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x07},
		[]byte{0x01, 0x09},
	)
}

// handDerivedResumptionGoldenId is the value that golden describes.
func handDerivedResumptionGoldenId() *PreSharedKeyId {
	return &PreSharedKeyId{
		PskType:    PskTypeResumption,
		Usage:      ResumptionPskUsageApplication,
		PskGroupId: []byte{0xcc},
		PskEpoch:   7,
		PskNonce:   []byte{0x09},
	}
}

// handDerivedWideExternalGolden pins what the two goldens above and the upstream corpus
// all leave out: a present but empty opaque field, and a field long enough to need the
// two octet varint prefix.
//
//	psktype           external                                         -> 01
//	psk_id<V>         empty, so a one octet prefix of 00 and no body    -> 00
//	psk_nonce<V>      64 octets. 64 is past the 63 a one octet prefix
//	                  can carry, so the two octet form 0b01 000000 |
//	                  0x40 is used                                     -> 40 40 then 64 c1
//
// 1 + 1 + (2+64) = 68 octets.
func handDerivedWideExternalGolden() []byte {
	return joinBytes(
		[]byte{0x01},
		[]byte{0x00},
		[]byte{0x40, 0x40},
		repeatByte(0xc1, 64),
	)
}

// handDerivedWideExternalGoldenId is the value that golden describes.
func handDerivedWideExternalGoldenId() *PreSharedKeyId {
	return &PreSharedKeyId{
		PskType:  PskTypeExternal,
		PskId:    []byte{},
		PskNonce: repeatByte(0xc1, 64),
	}
}

// handDerivedWideResumptionGolden is the resumption arm at the same boundaries, plus an
// epoch whose every octet is distinct — the only shape that catches a uint64 written in
// the wrong byte order, which the epoch 7 golden above cannot see — and a usage value
// this profile refuses, since decoding and accepting are separate questions and the
// codec owes the refusal a decodable value to name.
//
//	psktype           resumption                                       -> 02
//	usage             branch is code point 3                           -> 03
//	psk_group_id<V>   64 octets, two octet prefix                      -> 40 40 then 64 b2
//	psk_epoch         fedcba9876543210, big endian                     -> fe dc ba 98 76 54 32 10
//	psk_nonce<V>      empty                                            -> 00
//
// 1 + 1 + (2+64) + 8 + 1 = 77 octets.
func handDerivedWideResumptionGolden() []byte {
	return joinBytes(
		[]byte{0x02},
		[]byte{0x03},
		[]byte{0x40, 0x40},
		repeatByte(0xb2, 64),
		[]byte{0xfe, 0xdc, 0xba, 0x98, 0x76, 0x54, 0x32, 0x10},
		[]byte{0x00},
	)
}

// handDerivedWideResumptionGoldenId is the value that golden describes.
func handDerivedWideResumptionGoldenId() *PreSharedKeyId {
	return &PreSharedKeyId{
		PskType:    PskTypeResumption,
		Usage:      ResumptionPskUsageBranch,
		PskGroupId: repeatByte(0xb2, 64),
		PskEpoch:   0xfedcba9876543210,
		PskNonce:   []byte{},
	}
}

// goldenPskIds pairs every hand derived golden with the value it describes, so the
// encode assertions below and the refusal sweeps further down run over one set rather
// than two lists that can drift apart.
func goldenPskIds() map[string]struct {
	id      *PreSharedKeyId
	encoded []byte
	length  int
} {
	return map[string]struct {
		id      *PreSharedKeyId
		encoded []byte
		length  int
	}{
		"external": {handDerivedExternalGoldenId(), handDerivedExternalGolden(), 8},
		"resumption": {
			handDerivedResumptionGoldenId(), handDerivedResumptionGolden(), 14},
		"wide external": {
			handDerivedWideExternalGoldenId(), handDerivedWideExternalGolden(), 68},
		"wide resumption": {
			handDerivedWideResumptionGoldenId(), handDerivedWideResumptionGolden(), 77},
	}
}

// TestPreSharedKeyIdMarshalMatchesEveryHandDerivedGolden is the field order, arm
// selection and varint width pin. A reordered field, a psk_epoch written at four octets,
// a length prefix written at the record layer's fixed 32 bit width instead of the MLS
// varint, or an arm that writes a field belonging to the other one all change every
// psk_secret derived from these bytes, and this is the cheapest place to see it.
//
// Each golden's length is asserted against the arithmetic in its own comment first. A
// derivation whose comment and body disagree is a golden nobody has actually checked.
func TestPreSharedKeyIdMarshalMatchesEveryHandDerivedGolden(t *testing.T) {
	for name, golden := range goldenPskIds() {
		if len(golden.encoded) != golden.length {
			t.Fatalf("%s: the hand derivation is %d octets, the arithmetic in its comment says %d",
				name, len(golden.encoded), golden.length)
		}
		encoded, err := syntax.Marshal(golden.id)
		if err != nil {
			t.Fatalf("%s: syntax.Marshal: %v", name, err)
		}
		if !bytes.Equal(encoded, golden.encoded) {
			t.Fatalf("%s: syntax.Marshal =\n %x\nwant\n %x", name, encoded, golden.encoded)
		}
	}
}

// TestPreSharedKeyIdMarshalExternal is the plan's own external golden, kept because it
// states the same claim in the plan's words.
func TestPreSharedKeyIdMarshalExternal(t *testing.T) {
	id := &PreSharedKeyId{
		PskType:  PskTypeExternal,
		PskId:    []byte{0xaa, 0xbb},
		PskNonce: []byte{0x01, 0x02, 0x03},
	}
	encoded, err := syntax.Marshal(id)
	if err != nil {
		t.Fatalf("syntax.Marshal: %v", err)
	}
	want := []byte{
		0x01,             // psktype = external
		0x02, 0xaa, 0xbb, // psk_id<V>
		0x03, 0x01, 0x02, 0x03, // psk_nonce<V>
	}
	if !bytes.Equal(encoded, want) {
		t.Fatalf("syntax.Marshal = %x, want %x", encoded, want)
	}
}

// TestPreSharedKeyIdMarshalResumption is the plan's own resumption golden, including the
// usage octet and the uint64 epoch that sit between the type and the nonce.
func TestPreSharedKeyIdMarshalResumption(t *testing.T) {
	id := &PreSharedKeyId{
		PskType:    PskTypeResumption,
		Usage:      ResumptionPskUsageApplication,
		PskGroupId: []byte{0xcc},
		PskEpoch:   7,
		PskNonce:   []byte{0x09},
	}
	encoded, err := syntax.Marshal(id)
	if err != nil {
		t.Fatalf("syntax.Marshal: %v", err)
	}
	want := []byte{
		0x02,       // psktype = resumption
		0x01,       // usage = application
		0x01, 0xcc, // psk_group_id<V>
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x07, // psk_epoch
		0x01, 0x09, // psk_nonce<V>
	}
	if !bytes.Equal(encoded, want) {
		t.Fatalf("syntax.Marshal = %x, want %x", encoded, want)
	}
}

// ---------------------------------------------------------------------------
// the upstream corpus
// ---------------------------------------------------------------------------

// pskProposalVector is the part of one mlswg messages.json entry this file reads. The
// PreSharedKey proposal of RFC 9420 section 12.1.4 is a struct of exactly one field, a
// PreSharedKeyID, so its encoding IS the encoding of an id with no framing around it —
// which makes these three hundred hex strings a statement of this structure's wire form
// written by another implementation. The file is pinned by digest in vectors_pin_test.go
// and checked against its upstream git blob in vectors_upstream_test.go, so it cannot
// drift without a visible re-vendoring.
type pskProposalVector struct {
	PreSharedKeyProposal string `json:"pre_shared_key_proposal"`
}

// loadPskProposalVectors reads the pinned corpus. A file that parsed to nothing is fatal:
// the assertions below would then be a comparison against an empty list, which is the
// failure mode that reports green having checked nothing.
func loadPskProposalVectors(t *testing.T) []pskProposalVector {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "vectors", "messages.json"))
	if err != nil {
		t.Fatalf("read messages.json: %v", err)
	}
	vectors := []pskProposalVector{}
	if err := json.Unmarshal(body, &vectors); err != nil {
		t.Fatalf("parse messages.json: %v", err)
	}
	if len(vectors) == 0 {
		t.Fatal("messages.json parsed to no entries, so every assertion over it would hold vacuously")
	}
	return vectors
}

// TestPreSharedKeyIdMatchesEveryUpstreamPskProposal holds the external arm to bytes this
// encoder did not write.
//
// The fields are cut out of the upstream encoding positionally, under a layout stated
// here from RFC 9420 rather than obtained from this package's decoder: octet zero is the
// psktype, and a vlbytes length below 64 is a single octet whose value is the length
// (section 2.1). Nothing in the slicing consults UnmarshalMLS, so re-encoding those
// fields and getting the upstream bytes back is a statement about this encoder rather
// than a round trip of it against itself. A psktype code point of 2 for external, an
// opaque length written at the record layer's fixed width, or the nonce written before
// the id would all fail here.
//
// The decode direction is asserted on the same bytes, including that the fields the
// external arm does not carry come back as their zero values: a decoder that read a usage
// octet in this arm would consume the first octet of the nonce and everything after it
// would be off by one.
func TestPreSharedKeyIdMatchesEveryUpstreamPskProposal(t *testing.T) {
	checked := 0
	for index, vector := range loadPskProposalVectors(t) {
		raw, err := hex.DecodeString(vector.PreSharedKeyProposal)
		if err != nil {
			t.Fatalf("vector %d: pre_shared_key_proposal is not hex: %v", index, err)
		}
		if len(raw) < 3 {
			t.Fatalf("vector %d: %d octets cannot hold a psktype and two length prefixes", index, len(raw))
		}
		if raw[0] != uint8(PskTypeExternal) {
			t.Fatalf("vector %d: psktype octet is %#02x, want %#02x for external; the corpus carries an arm this test does not cut up",
				index, raw[0], uint8(PskTypeExternal))
		}
		if raw[1] >= 0x40 {
			t.Fatalf("vector %d: psk_id length prefix %#02x is not the single octet form this slicing assumes", index, raw[1])
		}
		idLength := int(raw[1])
		if len(raw) < 2+idLength+1 {
			t.Fatalf("vector %d: %d octets cannot hold a %d octet psk_id and a nonce prefix", index, len(raw), idLength)
		}
		if raw[2+idLength] >= 0x40 {
			t.Fatalf("vector %d: psk_nonce length prefix %#02x is not the single octet form this slicing assumes",
				index, raw[2+idLength])
		}
		nonceLength := int(raw[2+idLength])
		if want := 2 + idLength + 1 + nonceLength; len(raw) != want {
			t.Fatalf("vector %d: the layout accounts for %d octets and the encoding is %d", index, want, len(raw))
		}

		id := &PreSharedKeyId{
			PskType:  PskTypeExternal,
			PskId:    raw[2 : 2+idLength],
			PskNonce: raw[3+idLength : 3+idLength+nonceLength],
		}
		encoded, err := syntax.Marshal(id)
		if err != nil {
			t.Fatalf("vector %d: syntax.Marshal: %v", index, err)
		}
		if !bytes.Equal(encoded, raw) {
			t.Fatalf("vector %d: syntax.Marshal =\n %x\nupstream messages.json =\n %x", index, encoded, raw)
		}

		parsed := &PreSharedKeyId{}
		if err := syntax.Unmarshal(raw, parsed); err != nil {
			t.Fatalf("vector %d: syntax.Unmarshal: %v", index, err)
		}
		if !preSharedKeyIdsAgree(id, parsed) {
			t.Fatalf("vector %d: decoded to %s, want %s", index, describePreSharedKeyId(parsed), describePreSharedKeyId(id))
		}
		if parsed.Usage != 0 || parsed.PskGroupId != nil || parsed.PskEpoch != 0 {
			t.Fatalf("vector %d: the external arm decoded usage %d, a %d octet group id and epoch %d, none of which it carries",
				index, parsed.Usage, len(parsed.PskGroupId), parsed.PskEpoch)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no upstream psk proposal was checked")
	}
	t.Logf("%d upstream psk proposals encoded and decoded byte exact", checked)
}

// ---------------------------------------------------------------------------
// the union: each arm writes its own fields and nobody else's
// ---------------------------------------------------------------------------

// TestPreSharedKeyIdEncodesOnlyTheFieldsOfItsArm asserts the flattened struct behaves as
// the RFC's select() does: the discriminant decides which fields exist, and a field
// belonging to the other arm is not merely ignored on decode but never written.
//
// This is the property the flattening puts at risk and it has a consequence rather than
// being tidiness. A resumption group id leaking into an external id's encoding is extra
// bytes inside a PSKLabel that every member hashes, so the two members who disagree about
// whether to write them derive different psk_secrets and stop being able to talk.
func TestPreSharedKeyIdEncodesOnlyTheFieldsOfItsArm(t *testing.T) {
	for name, pair := range map[string][2]*PreSharedKeyId{
		"external ignores the resumption fields": {
			handDerivedExternalGoldenId(),
			&PreSharedKeyId{
				PskType:    PskTypeExternal,
				PskId:      []byte{0xaa, 0xbb},
				Usage:      ResumptionPskUsageBranch,
				PskGroupId: repeatByte(0x5a, 64),
				PskEpoch:   math.MaxUint64,
				PskNonce:   []byte{0x01, 0x02, 0x03},
			},
		},
		"resumption ignores the external field": {
			handDerivedResumptionGoldenId(),
			&PreSharedKeyId{
				PskType:    PskTypeResumption,
				PskId:      repeatByte(0x5b, 64),
				Usage:      ResumptionPskUsageApplication,
				PskGroupId: []byte{0xcc},
				PskEpoch:   7,
				PskNonce:   []byte{0x09},
			},
		},
	} {
		lean, err := syntax.Marshal(pair[0])
		if err != nil {
			t.Fatalf("%s: syntax.Marshal of the canonical value: %v", name, err)
		}
		loaded, err := syntax.Marshal(pair[1])
		if err != nil {
			t.Fatalf("%s: syntax.Marshal of the loaded value: %v", name, err)
		}
		if !bytes.Equal(lean, loaded) {
			t.Fatalf("%s: setting the other arm's fields changed the encoding\n from %x\n to   %x", name, lean, loaded)
		}
	}
}

// varyPreSharedKeyIdField gives one field of a PreSharedKeyId a different value, in place.
// An unhandled kind is fatal rather than skipped: a field of a new kind that this helper
// quietly ignored would be a field the completeness gate below reports as covered without
// ever having varied it, which is rule 5's failure with the gate's own helper doing it.
func varyPreSharedKeyIdField(t *testing.T, path string, field reflect.Value) {
	t.Helper()
	switch field.Kind() {
	case reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		field.SetUint(field.Uint() + 1)
	case reflect.Slice:
		existing, ok := field.Interface().([]byte)
		if !ok {
			t.Fatalf("%s is a slice of something other than bytes, which this helper cannot vary", path)
		}
		field.Set(reflect.ValueOf(append(append([]byte{}, existing...), 0xff)))
	default:
		t.Fatalf("%s is of kind %s, which this helper does not know how to vary; widen it rather than letting the gate below report the field as covered",
			path, field.Kind())
	}
}

// TestEveryPreSharedKeyIdFieldReachesTheEncodingInSomeArm is the derived completeness gate
// on the codec, and it is the one test here a field added later cannot slip past.
//
// It walks the struct definition with reflection rather than naming the fields, gives each
// one in turn a different value in both arms, and requires that at least one arm noticed —
// either the bytes moved, or the encoder's accept-or-refuse answer moved, which is what
// varying the discriminant off the registry does. A field added to PreSharedKeyId and
// forgotten in MarshalMLS therefore fails on the commit that adds it, rather than in the
// epoch where two members disagree about a field only one of them wrote.
func TestEveryPreSharedKeyIdFieldReachesTheEncodingInSomeArm(t *testing.T) {
	structType := reflect.TypeOf(PreSharedKeyId{})
	if structType.NumField() == 0 {
		t.Fatal("PreSharedKeyId declares no fields, so this gate walked nothing")
	}
	arms := map[string]*PreSharedKeyId{
		"external":   handDerivedExternalGoldenId(),
		"resumption": handDerivedResumptionGoldenId(),
	}
	for i := 0; i < structType.NumField(); i++ {
		name := structType.Field(i).Name
		noticed := []string{}
		for arm, base := range arms {
			before, beforeErr := syntax.Marshal(clonePreSharedKeyId(base))
			if beforeErr != nil {
				t.Fatalf("%s: the %s fixture does not encode: %v", name, arm, beforeErr)
			}
			varied := clonePreSharedKeyId(base)
			varyPreSharedKeyIdField(t, name, reflect.ValueOf(varied).Elem().Field(i))
			after, afterErr := syntax.Marshal(varied)
			if afterErr != nil {
				// an arm the encoder refuses is a change in its answer, which is
				// exactly what varying the discriminant off the registry produces.
				noticed = append(noticed, arm+" (refused)")
				continue
			}
			if !bytes.Equal(before, after) {
				noticed = append(noticed, arm)
			}
		}
		if len(noticed) == 0 {
			t.Errorf("changing %s changed neither arm's encoding, so MarshalMLS does not carry it and two members can disagree about it silently", name)
			continue
		}
		t.Logf("%s reaches the encoding in: %v", name, noticed)
	}
}

// ---------------------------------------------------------------------------
// the generated round trip corpus
// ---------------------------------------------------------------------------

// generatedPreSharedKeyIds is the round trip corpus: the cross product of the two arms
// with the varint width boundaries of every opaque field, the whole octet range of the
// usage discriminant, and the uint64 boundaries of the epoch.
//
// It is generated rather than written out case by case for the reason rule 5 names: a
// hand picked table is a claim about which cases matter, made by whoever also wrote the
// code. The enum axes are read out of the package's own constant declarations, so a
// registry member added later enters this corpus in the commit that declares it rather
// than in the commit somebody remembers to widen a list.
//
// Each entry is in canonical form for its arm — the fields the arm does not encode are
// left at their zero values — so that "decoding reproduces every field" is a statement
// the corpus can actually make. That the encoder ignores the other arm's fields is
// TestPreSharedKeyIdEncodesOnlyTheFieldsOfItsArm, above.
func generatedPreSharedKeyIds(t *testing.T) []*PreSharedKeyId {
	t.Helper()

	pskTypes := registryConstantsOfType(t, "PskType")
	for name, want := range map[string]uint64{"PskTypeExternal": 1, "PskTypeResumption": 2} {
		if got, ok := pskTypes[name]; !ok || got != want {
			t.Fatalf("the derivation read %s = %d (present %v), want %d; it is not reading this package's PskType declarations",
				name, got, ok, want)
		}
	}
	// the usage axis is the derived registry plus the two ends of the octet, because
	// the codec carries usage opaquely: a value outside the registry has to survive a
	// round trip for ValSem402 to be able to refuse it by name.
	usages := map[uint64]bool{0: true, 0xff: true}
	for _, value := range registryConstantsOfType(t, "ResumptionPskUsage") {
		usages[value] = true
	}

	// the opaque field lengths the varint branches on: absent, empty, one octet, the
	// last length a one octet prefix can express, the first that needs two, and one
	// well inside the two octet range.
	opaques := [][]byte{
		nil,
		{},
		repeatByte(0x11, 1),
		repeatByte(0x22, 63),
		repeatByte(0x33, 64),
		repeatByte(0x44, 255),
	}
	// the nonce axis adds KDF.Nh itself, which is the only length ValSem401 accepts and
	// therefore the only one that occurs in production.
	nonces := append(append([][]byte{}, opaques...), repeatByte(0x55, 32))

	// the uint64 boundaries, which are the values a narrower field or a sign would move.
	epochs := []uint64{
		0,
		1,
		math.MaxUint8,
		math.MaxUint16,
		math.MaxUint32 - 1,
		math.MaxUint32,
		uint64(math.MaxUint32) + 1,
		1 << 40,
		1 << 63,
		math.MaxUint64,
	}

	corpus := []*PreSharedKeyId{}
	for _, pskId := range opaques {
		for _, nonce := range nonces {
			corpus = append(corpus, &PreSharedKeyId{
				PskType:  PskTypeExternal,
				PskId:    pskId,
				PskNonce: nonce,
			})
		}
	}
	for usage := range usages {
		for groupIdIndex, groupId := range opaques {
			for epochIndex, epoch := range epochs {
				// the nonce rotates through its own axis rather than being crossed
				// with the group id, which would multiply the corpus to buy nothing:
				// both go through the same opaque encoder, so a length that works in
				// one position works in the other. Rotating puts every length in every
				// position across the product.
				corpus = append(corpus, &PreSharedKeyId{
					PskType:    PskTypeResumption,
					Usage:      ResumptionPskUsage(usage),
					PskGroupId: groupId,
					PskEpoch:   epoch,
					PskNonce:   nonces[(groupIdIndex+epochIndex)%len(nonces)],
				})
			}
		}
	}
	return corpus
}

// TestPreSharedKeyIdRoundTripsByteExactOverTheGeneratedCorpus asserts, for every generated
// id, that it encodes, that decoding recovers every field, and that re-encoding reproduces
// the bytes exactly. syntax.CheckRoundTrip is called on the same input because every later
// fuzz target reaches this codec through it, so a codec that satisfied the property here
// and not there would leave those targets green and empty.
func TestPreSharedKeyIdRoundTripsByteExactOverTheGeneratedCorpus(t *testing.T) {
	corpus := generatedPreSharedKeyIds(t)
	if len(corpus) < 300 {
		t.Fatalf("the generated corpus holds %d ids, far fewer than the product of its axes; the generator produced almost nothing", len(corpus))
	}
	for index, id := range corpus {
		encoded, err := syntax.Marshal(id)
		if err != nil {
			t.Fatalf("case %d %s: syntax.Marshal: %v", index, describePreSharedKeyId(id), err)
		}
		parsed := &PreSharedKeyId{}
		if err := syntax.Unmarshal(encoded, parsed); err != nil {
			t.Fatalf("case %d %s: syntax.Unmarshal: %v", index, describePreSharedKeyId(id), err)
		}
		if !preSharedKeyIdsAgree(id, parsed) {
			t.Fatalf("case %d %s: decoded to %s", index, describePreSharedKeyId(id), describePreSharedKeyId(parsed))
		}
		reencoded, err := syntax.Marshal(parsed)
		if err != nil {
			t.Fatalf("case %d %s: re-encode: %v", index, describePreSharedKeyId(id), err)
		}
		if !bytes.Equal(reencoded, encoded) {
			t.Fatalf("case %d %s: round trip =\n %x\nwant\n %x", index, describePreSharedKeyId(id), reencoded, encoded)
		}
		if err := syntax.CheckRoundTrip[PreSharedKeyId, *PreSharedKeyId](encoded); err != nil {
			t.Fatalf("case %d %s: CheckRoundTrip: %v", index, describePreSharedKeyId(id), err)
		}
	}
	t.Logf("%d generated ids round tripped byte exact", len(corpus))
}

// TestPreSharedKeyIdRoundTrip is the plan's own round trip case, kept because it states
// the claim in the plan's words over its own two values.
func TestPreSharedKeyIdRoundTrip(t *testing.T) {
	for _, id := range []*PreSharedKeyId{
		{PskType: PskTypeExternal, PskId: []byte{1, 2, 3}, PskNonce: []byte{4, 5}},
		{PskType: PskTypeResumption, Usage: ResumptionPskUsageBranch, PskGroupId: []byte{6}, PskEpoch: 1 << 40, PskNonce: []byte{7}},
	} {
		encoded, err := syntax.Marshal(id)
		if err != nil {
			t.Fatalf("syntax.Marshal: %v", err)
		}
		parsed := &PreSharedKeyId{}
		if err := syntax.Unmarshal(encoded, parsed); err != nil {
			t.Fatalf("syntax.Unmarshal: %v", err)
		}
		if !preSharedKeyIdsAgree(id, parsed) {
			t.Fatalf("decoded %s, want %s", describePreSharedKeyId(parsed), describePreSharedKeyId(id))
		}
		reencoded, err := syntax.Marshal(parsed)
		if err != nil {
			t.Fatalf("syntax.Marshal: %v", err)
		}
		if !bytes.Equal(encoded, reencoded) {
			t.Fatalf("round trip = %x, want %x", reencoded, encoded)
		}
	}
}

// TestPreSharedKeyIdRoundTripsAcrossTheFourOctetVarintBoundary covers the lengths the
// corpus above leaves out because carrying them through a cross product would cost
// megabytes per case: the last length the two octet prefix can express and the first that
// needs the four octet one.
func TestPreSharedKeyIdRoundTripsAcrossTheFourOctetVarintBoundary(t *testing.T) {
	for _, length := range []int{16383, 16384, 16385} {
		for name, id := range map[string]*PreSharedKeyId{
			"external": {
				PskType:  PskTypeExternal,
				PskId:    repeatByte(0x99, length),
				PskNonce: repeatByte(0xaa, length),
			},
			"resumption": {
				PskType:    PskTypeResumption,
				Usage:      ResumptionPskUsageApplication,
				PskGroupId: repeatByte(0xbb, length),
				PskEpoch:   math.MaxUint64,
				PskNonce:   repeatByte(0xcc, length),
			},
		} {
			encoded, err := syntax.Marshal(id)
			if err != nil {
				t.Fatalf("%s length %d: syntax.Marshal: %v", name, length, err)
			}
			parsed := &PreSharedKeyId{}
			if err := syntax.Unmarshal(encoded, parsed); err != nil {
				t.Fatalf("%s length %d: syntax.Unmarshal: %v", name, length, err)
			}
			if !preSharedKeyIdsAgree(id, parsed) {
				t.Fatalf("%s length %d did not round trip", name, length)
			}
			reencoded, err := syntax.Marshal(parsed)
			if err != nil {
				t.Fatalf("%s length %d: re-encode: %v", name, length, err)
			}
			if !bytes.Equal(reencoded, encoded) {
				t.Fatalf("%s length %d: re-encoding changed the %d octet encoding", name, length, len(encoded))
			}
		}
	}
}

// TestPreSharedKeyIdRoundTripsAMaximalNonceAndRefusesAnOverlongOne pins the two ends of
// the configured vector length limit. A field of exactly the limit must encode and come
// back; one octet more must be refused by the encoder rather than produce bytes a
// compliant peer would reject.
func TestPreSharedKeyIdRoundTripsAMaximalNonceAndRefusesAnOverlongOne(t *testing.T) {
	maximal := &PreSharedKeyId{
		PskType:  PskTypeExternal,
		PskId:    []byte{0xd0},
		PskNonce: repeatByte(0xd1, syntax.MaxVectorLength),
	}
	encoded, err := syntax.Marshal(maximal)
	if err != nil {
		t.Fatalf("a nonce of exactly MaxVectorLength was refused: %v", err)
	}
	parsed := &PreSharedKeyId{}
	if err := syntax.Unmarshal(encoded, parsed); err != nil {
		t.Fatalf("the maximal encoding did not decode: %v", err)
	}
	if !preSharedKeyIdsAgree(maximal, parsed) {
		t.Fatal("the maximal id did not round trip")
	}

	overlong := clonePreSharedKeyId(maximal)
	overlong.PskNonce = repeatByte(0xd1, syntax.MaxVectorLength+1)
	if _, err := syntax.Marshal(overlong); !errors.Is(err, syntax.ErrLengthExceedsMax) {
		t.Fatalf("a nonce one octet over the limit encoded with err = %v, want syntax.ErrLengthExceedsMax", err)
	}
}

// ---------------------------------------------------------------------------
// decode discipline
// ---------------------------------------------------------------------------

// goldenPskEncodings is the set of valid encodings the refusal properties below are
// stated over: both arms at their narrow widths, both at the two octet varint width, and
// one encoding another implementation wrote.
func goldenPskEncodings(t *testing.T) map[string][]byte {
	t.Helper()
	encodings := map[string][]byte{}
	for name, golden := range goldenPskIds() {
		encodings[name] = golden.encoded
	}
	raw, err := hex.DecodeString(loadPskProposalVectors(t)[0].PreSharedKeyProposal)
	if err != nil {
		t.Fatalf("the first upstream psk proposal is not hex: %v", err)
	}
	encodings["upstream"] = raw
	return encodings
}

// TestPreSharedKeyIdUnmarshalLeavesTheTailAlone asserts UnmarshalMLS consumes exactly one
// id, which is what PSKLabel and GroupSecrets.psks<V> both depend on. Both arms are swept,
// because the resumption arm is where an over consuming decoder is easiest to write: its
// epoch is the one field with no length prefix in front of it.
func TestPreSharedKeyIdUnmarshalLeavesTheTailAlone(t *testing.T) {
	for name, golden := range goldenPskIds() {
		r := syntax.NewReader(append(append([]byte{}, golden.encoded...), 0xde, 0xad))
		parsed := &PreSharedKeyId{}
		if err := parsed.UnmarshalMLS(r); err != nil {
			t.Fatalf("%s: UnmarshalMLS: %v", name, err)
		}
		if !preSharedKeyIdsAgree(golden.id, parsed) {
			t.Fatalf("%s: decoded to %s, want %s", name, describePreSharedKeyId(parsed), describePreSharedKeyId(golden.id))
		}
		tail, err := r.ReadRaw(2)
		if err != nil {
			t.Fatalf("%s: ReadRaw: %v", name, err)
		}
		if !bytes.Equal(tail, []byte{0xde, 0xad}) {
			t.Fatalf("%s: tail = %x, want dead", name, tail)
		}
		if remaining := r.Remaining(); remaining != 0 {
			t.Fatalf("%s: %d octets left after the tail, so the decoder consumed fewer than its own fields", name, remaining)
		}
	}
}

// TestPreSharedKeyIdRejectsTrailingBytes asserts a standalone encoding with anything after
// it is refused. MLS signs over serialized forms, so a decoder tolerating a tail accepts
// two encodings of one object and the signature covers only one of them.
func TestPreSharedKeyIdRejectsTrailingBytes(t *testing.T) {
	for name, full := range goldenPskEncodings(t) {
		if err := syntax.Unmarshal(full, &PreSharedKeyId{}); err != nil {
			t.Fatalf("%s: the untrailed encoding was refused (%v), so the assertion below proves nothing", name, err)
		}
		withTail := append(append([]byte{}, full...), 0x00)
		if err := syntax.Unmarshal(withTail, &PreSharedKeyId{}); !errors.Is(err, syntax.ErrTrailingBytes) {
			t.Errorf("%s: one trailing octet decoded with err = %v, want syntax.ErrTrailingBytes", name, err)
		}
	}
}

// TestPreSharedKeyIdRejectsEverySingleByteTruncation asserts every proper prefix of a
// valid encoding is refused rather than yielding a partly populated id. The set of
// prefixes is derived from each encoding's own length, so a field added to the structure
// widens this test without anybody editing it.
func TestPreSharedKeyIdRejectsEverySingleByteTruncation(t *testing.T) {
	for name, full := range goldenPskEncodings(t) {
		refused := 0
		for n := 0; n < len(full); n++ {
			parsed := &PreSharedKeyId{}
			if err := syntax.Unmarshal(full[:n], parsed); err == nil {
				t.Errorf("%s: the %d octet prefix parsed into %s, want an error", name, n, describePreSharedKeyId(parsed))
				continue
			}
			refused++
		}
		if refused != len(full) {
			t.Errorf("%s: %d of %d prefixes refused", name, refused, len(full))
		}
		if err := syntax.Unmarshal(full, &PreSharedKeyId{}); err != nil {
			t.Errorf("%s: the untruncated encoding was refused too (%v), so the loop above proves nothing", name, err)
		}
	}
}

// TestPreSharedKeyIdEitherRefusesOrExactlyReproducesEverySingleByteCorruption is the
// canonicality property, stated over every one octet change to a valid encoding: the
// decoder may refuse, and if it accepts then re-encoding must give back the corrupted
// bytes exactly. Accepted-and-silently-changed is the outcome that is forbidden, because
// it is a second encoding of a structure whose first encoding somebody signed.
//
// Both outcomes are counted and both are required to occur. A decoder that refused
// everything would satisfy the property vacuously, and so would a corpus that never
// reached the accepting branch.
func TestPreSharedKeyIdEitherRefusesOrExactlyReproducesEverySingleByteCorruption(t *testing.T) {
	for name, full := range goldenPskEncodings(t) {
		accepted, refused := 0, 0
		for position := range full {
			for delta := 1; delta < 256; delta++ {
				corrupted := append([]byte{}, full...)
				corrupted[position] = byte((int(full[position]) + delta) % 256)
				parsed := &PreSharedKeyId{}
				if err := syntax.Unmarshal(corrupted, parsed); err != nil {
					refused++
					continue
				}
				accepted++
				reencoded, err := syntax.Marshal(parsed)
				if err != nil {
					t.Fatalf("%s: octet %d changed to %#02x decoded but would not re-encode: %v",
						name, position, corrupted[position], err)
				}
				if !bytes.Equal(reencoded, corrupted) {
					t.Fatalf("%s: octet %d changed to %#02x was accepted and changed:\n got %x\nwant %x",
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

// sentinelPreSharedKeyId is a receiver whose every field holds a value no golden holds, so
// an assignment made before a decode failed is visible whatever it assigned. A zeroed
// receiver would show an early assignment only when the input's value for that field
// happens not to be zero, which makes the observation depend on the fixture rather than on
// the code.
func sentinelPreSharedKeyId() *PreSharedKeyId {
	return &PreSharedKeyId{
		PskType:    PskType(0x7b),
		PskId:      []byte{0x9a, 0x9b},
		Usage:      ResumptionPskUsage(0x7c),
		PskGroupId: []byte{0x9c},
		PskEpoch:   0x0123456789abcdef,
		PskNonce:   []byte{0x9d, 0x9e, 0x9f},
	}
}

// TestPreSharedKeyIdUnmarshalAssignsNothingWhenItRefuses states the all or nothing rule
// UnmarshalMLS documents: a decode that fails leaves the caller's value exactly as it was
// handed over.
//
// The rule earns a test because the value a decode writes into is not always fresh. Under
// C1 the byte level entry point is syntax.Unmarshal(bs, id) into a value the caller owns,
// which invites reuse across a psks<V> vector, and a decoder that assigned as it read
// would leave a refused decode holding some fields from the new input and the rest from
// the old one. That composite is not a mangled struct anybody would notice: it is a well
// formed PreSharedKeyId naming a key nobody sent, and re-encoding it produces bytes that
// go into a psk_input some member will hash.
//
// Two families of refusable input are swept, both derived. Every proper prefix of every
// golden fails somewhere in the field sequence, which reaches the assignments that happen
// before the failing read. Every single octet change to the goldens reaches what a
// truncation cannot — a length prefix that overruns its region, a psktype off the registry
// — and those fail at points a prefix never stops at.
//
// UnmarshalMLS is called directly rather than through syntax.Unmarshal because the trailing
// byte refusal belongs to the outer layer and happens after every field has legitimately
// been assigned, so routing through it would count a correct decode as a refusal.
func TestPreSharedKeyIdUnmarshalAssignsNothingWhenItRefuses(t *testing.T) {
	inputs := map[string][][]byte{}
	for name, full := range goldenPskEncodings(t) {
		prefixes := [][]byte{}
		for n := 0; n < len(full); n++ {
			prefixes = append(prefixes, full[:n])
		}
		inputs[name+" truncated"] = prefixes

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
			untouched := sentinelPreSharedKeyId()
			parsed := sentinelPreSharedKeyId()
			if err := parsed.UnmarshalMLS(syntax.NewReader(input)); err == nil {
				accepted++
				continue
			}
			familyRefused++
			if !reflect.DeepEqual(parsed, untouched) {
				t.Fatalf("%s: a refused %d octet input changed the caller's id\n from %#v\n to   %#v",
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
	t.Logf("%d refused inputs left the caller's id untouched, %d were accepted", refused, accepted)
}

// TestPreSharedKeyIdUnmarshalClearsTheFieldsItsArmDoesNotCarry is the other half of the
// assignment rule and the one the flattened union makes possible.
//
// A decoder that assigned only the fields of the arm it read would be correct on a fresh
// receiver and wrong on a reused one: decode a resumption id, then an external one into
// the same value, and PskGroupId and PskEpoch still describe the first. Nothing in the
// re-encoding would show it, because the external arm does not write those fields — it
// shows up in whatever reads them, which for a resumption psk is the lookup that decides
// which group's secret to reach for.
func TestPreSharedKeyIdUnmarshalClearsTheFieldsItsArmDoesNotCarry(t *testing.T) {
	external := handDerivedWideExternalGolden()
	resumption := handDerivedWideResumptionGolden()

	reused := &PreSharedKeyId{}
	if err := syntax.Unmarshal(resumption, reused); err != nil {
		t.Fatalf("the resumption golden did not decode: %v", err)
	}
	if reused.PskGroupId == nil || reused.PskEpoch == 0 || reused.Usage == 0 {
		t.Fatalf("the resumption golden decoded to %s, which carries none of the fields the second decode has to clear",
			describePreSharedKeyId(reused))
	}
	if err := syntax.Unmarshal(external, reused); err != nil {
		t.Fatalf("the external golden did not decode into the reused id: %v", err)
	}
	if reused.Usage != 0 || reused.PskGroupId != nil || reused.PskEpoch != 0 {
		t.Fatalf("after decoding an external id the receiver still holds usage %d, a %d octet group id and epoch %#x from the resumption id before it",
			reused.Usage, len(reused.PskGroupId), reused.PskEpoch)
	}

	if err := syntax.Unmarshal(resumption, reused); err != nil {
		t.Fatalf("the resumption golden did not decode into the reused id: %v", err)
	}
	if reused.PskId != nil {
		t.Fatalf("after decoding a resumption id the receiver still holds a %d octet psk_id from the external id before it", len(reused.PskId))
	}
}

// ---------------------------------------------------------------------------
// the psktype registry, derived
// ---------------------------------------------------------------------------

// registeredPskTypes reads the psktype code points out of this package's own constant
// declarations and checks the reading against the two the RFC names, so a derivation that
// found nothing is separated from one that found something. The sweeps below state their
// property over the complement of this set — every octet that is NOT a registered type —
// which is a class no list can understate.
func registeredPskTypes(t *testing.T) map[uint64]bool {
	t.Helper()
	derived := registryConstantsOfType(t, "PskType")
	for name, want := range map[string]uint64{"PskTypeExternal": 1, "PskTypeResumption": 2} {
		if got, ok := derived[name]; !ok || got != want {
			t.Fatalf("the derivation read %s = %d (present %v), want %d", name, got, ok, want)
		}
	}
	registered := map[uint64]bool{}
	for _, value := range derived {
		registered[value] = true
	}
	return registered
}

// TestPreSharedKeyIdUnmarshalRefusesEveryUnregisteredPskType asserts an unknown psktype is
// a decode error rather than a value carried through, over every octet the registry does
// not claim.
//
// The refusal is not pedantry about unknown code points. The psktype selects the arm, the
// arm decides how many fields follow, and an implementation that does not know the arm does
// not know where the id ends — so a decoder that shrugged and read a nonce would be
// guessing at an offset, and in a psks<V> vector that guess desynchronises every id after it.
func TestPreSharedKeyIdUnmarshalRefusesEveryUnregisteredPskType(t *testing.T) {
	registered := registeredPskTypes(t)
	refused, known := 0, 0
	for value := 0; value <= 0xff; value++ {
		err := (&PreSharedKeyId{}).UnmarshalMLS(syntax.NewReader([]byte{byte(value)}))
		if registered[uint64(value)] {
			known++
			// a registered arm runs out of input here, which is a different failure
			// and must not be reported as the type refusal.
			if errors.Is(err, errPskType) {
				t.Errorf("psktype %#02x is registered and was refused as an unknown type: %v", value, err)
			}
			continue
		}
		if !errors.Is(err, errPskType) {
			t.Errorf("psktype %#02x decoded with err = %v, want the ValSem402 refusal", value, err)
			continue
		}
		refused++
	}
	if known == 0 {
		t.Fatal("no registered psktype was reached, so the control half of this sweep evaluated nothing")
	}
	if refused != 0x100-known {
		t.Fatalf("%d of the %d unregistered psktypes were refused", refused, 0x100-known)
	}
	t.Logf("%d unregistered psktypes refused, %d registered arms reached", refused, known)
}

// TestPreSharedKeyIdMarshalRefusesEveryUnregisteredPskType asserts the encoder refuses the
// same arms it cannot decode. This is the semantic refusal MarshalMLS returns an error for
// (C2): dropping it would emit a psktype octet and a nonce with no arm between them, which
// hashes into psk_input as though it were a whole id.
func TestPreSharedKeyIdMarshalRefusesEveryUnregisteredPskType(t *testing.T) {
	registered := registeredPskTypes(t)
	refused, accepted := 0, 0
	for value := 0; value <= 0xff; value++ {
		id := &PreSharedKeyId{PskType: PskType(value), PskNonce: repeatByte(0x01, 32)}
		encoded, err := syntax.Marshal(id)
		if registered[uint64(value)] {
			if err != nil {
				t.Errorf("psktype %#02x is registered and the encoder refused it: %v", value, err)
				continue
			}
			accepted++
			continue
		}
		if !errors.Is(err, errPskType) {
			t.Errorf("psktype %#02x encoded to %x with err = %v, want the ValSem402 refusal", value, encoded, err)
			continue
		}
		if encoded != nil {
			t.Errorf("psktype %#02x was refused and still produced %x", value, encoded)
		}
		refused++
	}
	if accepted == 0 {
		t.Fatal("no registered psktype encoded, so the control half of this sweep evaluated nothing")
	}
	if refused != 0x100-accepted {
		t.Fatalf("%d of the %d unregistered psktypes were refused", refused, 0x100-accepted)
	}
	t.Logf("%d unregistered psktypes refused by the encoder, %d registered arms encoded", refused, accepted)
}

// TestPreSharedKeyIdCarriesEveryResumptionUsageOctetThroughTheCodec asserts the codec is
// opaque about usage over the whole octet range, which is what lets ValSem402 refuse a
// value by name. A decoder that refused ReInit here would make the validation rule
// unreachable and its test vacuous, and would refuse the vectors besides.
func TestPreSharedKeyIdCarriesEveryResumptionUsageOctetThroughTheCodec(t *testing.T) {
	for value := 0; value <= 0xff; value++ {
		id := &PreSharedKeyId{
			PskType:    PskTypeResumption,
			Usage:      ResumptionPskUsage(value),
			PskGroupId: []byte{0xcc},
			PskEpoch:   9,
			PskNonce:   []byte{0x07},
		}
		encoded, err := syntax.Marshal(id)
		if err != nil {
			t.Fatalf("usage %#02x: syntax.Marshal: %v", value, err)
		}
		if encoded[1] != byte(value) {
			t.Fatalf("usage %#02x was written as %#02x", value, encoded[1])
		}
		parsed := &PreSharedKeyId{}
		if err := syntax.Unmarshal(encoded, parsed); err != nil {
			t.Fatalf("usage %#02x: syntax.Unmarshal: %v", value, err)
		}
		if !preSharedKeyIdsAgree(id, parsed) {
			t.Fatalf("usage %#02x decoded to %s", value, describePreSharedKeyId(parsed))
		}
	}
}

// ---------------------------------------------------------------------------
// ValSem401: the nonce length
// ---------------------------------------------------------------------------

// TestPreSharedKeyIdValidateNonceLength is ValSem401 stated the way the plan states it: an
// id that is valid in every other respect, and would therefore be accepted by a Validate
// that never looked at the nonce, is refused for its nonce alone.
func TestPreSharedKeyIdValidateNonceLength(t *testing.T) {
	crypto := pskTestCrypto(t)
	id := &PreSharedKeyId{PskType: PskTypeExternal, PskId: []byte{1}, PskNonce: make([]byte, 31)}
	// the refusal will be ValSem(ValSem401, ...) once the validation plan lands, and it
	// matches the sentinel through Unwrap either way. Asserting the code itself is p8's
	// TestValSem401_PskNonceLength; CodeOf is p8-internal and this plan does not reach
	// for it.
	if err := id.Validate(crypto); !errors.Is(err, errPskNonceLength) {
		t.Fatalf("err = %v, want the ValSem401 refusal", err)
	}
	id.PskNonce = make([]byte, 32)
	if err := id.Validate(crypto); err != nil {
		t.Fatalf("a 32 octet nonce was refused: %v", err)
	}
}

// TestPreSharedKeyIdValidateDerivesTheNonceLengthFromTheProvider is the half the two
// registered ciphersuites cannot state, because both have Nh = 32 and a literal 32 is
// indistinguishable from the derivation against either of them.
//
// The lengths swept are the three the mlswg psk_secret corpus actually carries, so this is
// not a hypothetical: RFC 9420 registers ciphersuites at SHA-384 and SHA-512, their vectors
// are in this tree, and a rule pinned to 32 would accept a 32 octet nonce for all of them —
// a nonce half the length the RFC requires, on the field whose whole job is to keep two uses
// of one pre shared key apart.
func TestPreSharedKeyIdValidateDerivesTheNonceLengthFromTheProvider(t *testing.T) {
	for _, hashSize := range []int{32, 48, 64} {
		crypto := pskCryptoWithHashSize(t, hashSize)
		for _, arm := range []*PreSharedKeyId{
			{PskType: PskTypeExternal, PskId: []byte{1}},
			{PskType: PskTypeResumption, Usage: ResumptionPskUsageApplication, PskGroupId: []byte{1}},
		} {
			exact := clonePreSharedKeyId(arm)
			exact.PskNonce = make([]byte, hashSize)
			if err := exact.Validate(crypto); err != nil {
				t.Errorf("Nh %d: a nonce of exactly Nh was refused: %v", hashSize, err)
			}
			for _, wrong := range []int{0, hashSize - 1, hashSize + 1, 32} {
				if wrong == hashSize {
					continue
				}
				off := clonePreSharedKeyId(arm)
				off.PskNonce = make([]byte, wrong)
				if err := off.Validate(crypto); !errors.Is(err, errPskNonceLength) {
					t.Errorf("Nh %d: a %d octet nonce was accepted with err = %v, want the ValSem401 refusal",
						hashSize, wrong, err)
				}
			}
		}
	}
}

// pskSecretVector is the part of one mlswg psk_secret.json entry this file reads.
type pskSecretVector struct {
	CipherSuite uint16 `json:"cipher_suite"`
	Psks        []struct {
		PskId    string `json:"psk_id"`
		Psk      string `json:"psk"`
		PskNonce string `json:"psk_nonce"`
	} `json:"psks"`
	PskSecret string `json:"psk_secret"`
}

// loadPskSecretVectors reads the pinned psk_secret corpus.
func loadPskSecretVectors(t *testing.T) []pskSecretVector {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "vectors", "psk_secret.json"))
	if err != nil {
		t.Fatalf("read psk_secret.json: %v", err)
	}
	vectors := []pskSecretVector{}
	if err := json.Unmarshal(body, &vectors); err != nil {
		t.Fatalf("parse psk_secret.json: %v", err)
	}
	if len(vectors) == 0 {
		t.Fatal("psk_secret.json parsed to no entries")
	}
	return vectors
}

// TestPreSharedKeyIdValidateAcceptsEveryUpstreamPsk is the positive control ValSem401 needs
// and the negative sweep above cannot be: every pre shared key another implementation put in
// the psk_secret corpus must pass, at its own ciphersuite's Nh.
//
// A rule that refused too much would be as damaging as one that refused too little — it
// would make the vector family unpassable — and a rule tested only against nonces this file
// constructed could not tell the difference. The corpus carries three distinct Nh values,
// which is asserted, so this cannot quietly become a test about 32.
func TestPreSharedKeyIdValidateAcceptsEveryUpstreamPsk(t *testing.T) {
	lengthsSeen := map[int]int{}
	checked := 0
	for index, vector := range loadPskSecretVectors(t) {
		for position, psk := range vector.Psks {
			pskId, err := hex.DecodeString(psk.PskId)
			if err != nil {
				t.Fatalf("vector %d psk %d: psk_id is not hex: %v", index, position, err)
			}
			nonce, err := hex.DecodeString(psk.PskNonce)
			if err != nil {
				t.Fatalf("vector %d psk %d: psk_nonce is not hex: %v", index, position, err)
			}
			id := &PreSharedKeyId{PskType: PskTypeExternal, PskId: pskId, PskNonce: nonce}
			if err := id.Validate(pskCryptoWithHashSize(t, len(nonce))); err != nil {
				t.Errorf("vector %d psk %d (suite %#04x, %d octet nonce) was refused: %v",
					index, position, vector.CipherSuite, len(nonce), err)
				continue
			}
			// and the same value one octet short must not be, so the acceptance above
			// is a decision rather than a rule that says yes to everything.
			short := clonePreSharedKeyId(id)
			short.PskNonce = nonce[:len(nonce)-1]
			if err := short.Validate(pskCryptoWithHashSize(t, len(nonce))); !errors.Is(err, errPskNonceLength) {
				t.Errorf("vector %d psk %d: a nonce one octet short was accepted with err = %v", index, position, err)
			}
			lengthsSeen[len(nonce)]++
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("the psk_secret corpus yielded no pre shared key, so this test validated nothing")
	}
	if len(lengthsSeen) < 2 {
		t.Fatalf("every upstream nonce was the same length (%v), so this test cannot tell the derivation from a literal", lengthsSeen)
	}
	t.Logf("%d upstream psks accepted, nonce lengths %v", checked, lengthsSeen)
}

// ---------------------------------------------------------------------------
// ValSem402: the type and the resumption usage
// ---------------------------------------------------------------------------

// TestPreSharedKeyIdValidateUsage is ValSem402 stated the way the plan states it: only
// Resumption(Application) and External are acceptable. ReInit and Branch usages belong to
// features v1 does not implement, and accepting one here would let them in through the key
// schedule.
func TestPreSharedKeyIdValidateUsage(t *testing.T) {
	crypto := pskTestCrypto(t)
	for _, usage := range []ResumptionPskUsage{ResumptionPskUsageReInit, ResumptionPskUsageBranch} {
		id := &PreSharedKeyId{
			PskType:    PskTypeResumption,
			Usage:      usage,
			PskGroupId: []byte{1},
			PskNonce:   make([]byte, 32),
		}
		if err := id.Validate(crypto); !errors.Is(err, errPskType) {
			t.Fatalf("usage %d err = %v, want the ValSem402 refusal", usage, err)
		}
	}
	ok := &PreSharedKeyId{
		PskType:    PskTypeResumption,
		Usage:      ResumptionPskUsageApplication,
		PskGroupId: []byte{1},
		PskNonce:   make([]byte, 32),
	}
	if err := ok.Validate(crypto); err != nil {
		t.Fatalf("Resumption(Application) was refused: %v", err)
	}
}

// TestPreSharedKeyIdValidateAcceptsOnlyApplicationOfEveryResumptionUsage is the derived
// form of the rule above, over the whole octet range rather than over the two values the
// RFC happens to have named. A usage the registry gains later, or one a peer invents, is
// judged here on the commit it appears rather than on the commit somebody widens a list.
//
// The registry read is checked for the three RFC values first, so the sweep cannot pass by
// having derived nothing.
func TestPreSharedKeyIdValidateAcceptsOnlyApplicationOfEveryResumptionUsage(t *testing.T) {
	derived := registryConstantsOfType(t, "ResumptionPskUsage")
	for name, want := range map[string]uint64{
		"ResumptionPskUsageApplication": 1,
		"ResumptionPskUsageReInit":      2,
		"ResumptionPskUsageBranch":      3,
	} {
		if got, ok := derived[name]; !ok || got != want {
			t.Fatalf("the derivation read %s = %d (present %v), want %d", name, got, ok, want)
		}
	}
	crypto := pskTestCrypto(t)
	accepted, refused := 0, 0
	for value := 0; value <= 0xff; value++ {
		id := &PreSharedKeyId{
			PskType:    PskTypeResumption,
			Usage:      ResumptionPskUsage(value),
			PskGroupId: []byte{1},
			PskNonce:   make([]byte, crypto.HashSize()),
		}
		err := id.Validate(crypto)
		if ResumptionPskUsage(value) == ResumptionPskUsageApplication {
			if err != nil {
				t.Errorf("usage %#02x is Application and was refused: %v", value, err)
				continue
			}
			accepted++
			continue
		}
		if !errors.Is(err, errPskType) {
			t.Errorf("usage %#02x was accepted with err = %v, want the ValSem402 refusal", value, err)
			continue
		}
		refused++
	}
	if accepted != 1 {
		t.Fatalf("%d of the 256 usage octets were accepted, want exactly Application", accepted)
	}
	if refused != 0xff {
		t.Fatalf("%d of the 255 non-Application usage octets were refused", refused)
	}
	// the derived registry members that are not Application are the named half of the
	// class the sweep above covers; naming them is what makes a failure readable.
	for name, value := range derived {
		if ResumptionPskUsage(value) == ResumptionPskUsageApplication {
			continue
		}
		t.Logf("%s (%d) is refused by ValSem402, as the v1 profile requires", name, value)
	}
}

// TestPreSharedKeyIdValidateRefusesEveryUnregisteredPskType asserts the type half of
// ValSem402 over the complement of the derived registry. The nonce is correct in every
// case, so a Validate that never reached the type switch would accept all 254 of them.
func TestPreSharedKeyIdValidateRefusesEveryUnregisteredPskType(t *testing.T) {
	registered := registeredPskTypes(t)
	crypto := pskTestCrypto(t)
	refused, known := 0, 0
	for value := 0; value <= 0xff; value++ {
		id := &PreSharedKeyId{
			PskType:    PskType(value),
			PskId:      []byte{1},
			Usage:      ResumptionPskUsageApplication,
			PskGroupId: []byte{1},
			PskNonce:   make([]byte, crypto.HashSize()),
		}
		err := id.Validate(crypto)
		if registered[uint64(value)] {
			if err != nil {
				t.Errorf("psktype %#02x is registered and Validate refused it: %v", value, err)
				continue
			}
			known++
			continue
		}
		if !errors.Is(err, errPskType) {
			t.Errorf("psktype %#02x was accepted with err = %v, want the ValSem402 refusal", value, err)
			continue
		}
		refused++
	}
	if known == 0 {
		t.Fatal("no registered psktype was accepted, so the control half of this sweep evaluated nothing")
	}
	if refused != 0x100-known {
		t.Fatalf("%d of the %d unregistered psktypes were refused by Validate", refused, 0x100-known)
	}
}

// TestPreSharedKeyIdValidateIgnoresTheUsageFieldOnTheExternalArm asserts what the union
// implies: usage is not part of the external arm, so its value cannot make an external id
// invalid. A rule that read the field regardless would refuse ids that are perfectly well
// formed on the wire, since the octet never travelled and whatever the struct holds there
// came from the caller rather than from a peer.
func TestPreSharedKeyIdValidateIgnoresTheUsageFieldOnTheExternalArm(t *testing.T) {
	crypto := pskTestCrypto(t)
	for value := 0; value <= 0xff; value++ {
		id := &PreSharedKeyId{
			PskType:  PskTypeExternal,
			PskId:    []byte{1},
			Usage:    ResumptionPskUsage(value),
			PskNonce: make([]byte, crypto.HashSize()),
		}
		if err := id.Validate(crypto); err != nil {
			t.Fatalf("an external id with usage %#02x was refused: %v", value, err)
		}
	}
}

// TestPreSharedKeyIdTheTwoRefusalsAreDistinguishable asserts ValSem401 and ValSem402 do not
// answer to each other under errors.Is.
//
// This is the property that makes both of the tests above mean anything. Two refusals
// carrying one sentinel would make a nonce length failure indistinguishable from a usage
// failure at every call site that branches on them, and every assertion in this file that
// asks for one would be answered yes by the other — which is exactly how a sweep that
// checks nothing reports green.
func TestPreSharedKeyIdTheTwoRefusalsAreDistinguishable(t *testing.T) {
	crypto := pskTestCrypto(t)
	nonceFailure := (&PreSharedKeyId{
		PskType:  PskTypeExternal,
		PskId:    []byte{1},
		PskNonce: make([]byte, crypto.HashSize()-1),
	}).Validate(crypto)
	usageFailure := (&PreSharedKeyId{
		PskType:    PskTypeResumption,
		Usage:      ResumptionPskUsageReInit,
		PskGroupId: []byte{1},
		PskNonce:   make([]byte, crypto.HashSize()),
	}).Validate(crypto)

	if nonceFailure == nil || usageFailure == nil {
		t.Fatalf("one of the two refusals did not fire: nonce %v, usage %v", nonceFailure, usageFailure)
	}
	if errors.Is(nonceFailure, errPskType) {
		t.Error("the ValSem401 refusal also answers to the ValSem402 sentinel")
	}
	if errors.Is(usageFailure, errPskNonceLength) {
		t.Error("the ValSem402 refusal also answers to the ValSem401 sentinel")
	}
	if errors.Is(errPskNonceLength, errPskType) || errors.Is(errPskType, errPskNonceLength) {
		t.Error("the two sentinels alias each other, so no test in this file can tell them apart")
	}
	if errPskNonceLength.Error() == errPskType.Error() {
		t.Error("the two sentinels carry the same message, so a log cannot tell them apart")
	}
}

// TestPreSharedKeyIdValidateReportsTheNonceBeforeTheType pins which refusal a caller sees
// when an id violates both rules. It is an ordering claim rather than a correctness one,
// and it is here because the answer has to be deterministic: the validation plan reports a
// ValSem code to the caller and to the interop report, and a rule order that drifted would
// move a vector from one code's coverage row to another's without any test failing.
func TestPreSharedKeyIdValidateReportsTheNonceBeforeTheType(t *testing.T) {
	crypto := pskTestCrypto(t)
	both := &PreSharedKeyId{
		PskType:  PskType(0x7f),
		PskNonce: make([]byte, crypto.HashSize()-1),
	}
	err := both.Validate(crypto)
	if !errors.Is(err, errPskNonceLength) {
		t.Fatalf("an id with a short nonce and an unregistered type reported %v, want the ValSem401 refusal", err)
	}
	if errors.Is(err, errPskType) {
		t.Fatal("the refusal answers to both sentinels, so the order it reports is not observable")
	}
}

// ---------------------------------------------------------------------------
// ValSem403: the same PreSharedKeyID twice
// ---------------------------------------------------------------------------

// pskTestId builds an external id with a distinguishable psk_id and a nonce of the
// provider's KDF.Nh, so nothing this section builds is refused by ValSem401 for a reason
// that has nothing to do with what is being asserted.
func pskTestId(t *testing.T, idByte byte, nonceByte byte) PreSharedKeyId {
	t.Helper()
	return PreSharedKeyId{
		PskType:  PskTypeExternal,
		PskId:    []byte{idByte},
		PskNonce: repeatByte(nonceByte, pskTestCrypto(t).HashSize()),
	}
}

// TestCheckNoDuplicatePsksRefusesARepeatedId asserts an exactly repeated id is refused,
// and that the pair reported is the pair that repeated. The two copies are not adjacent,
// which is the case an implementation comparing only neighbours gets wrong.
func TestCheckNoDuplicatePsksRefusesARepeatedId(t *testing.T) {
	ids := []PreSharedKeyId{pskTestId(t, 1, 0xa1), pskTestId(t, 2, 0xa2), pskTestId(t, 1, 0xa1)}
	err := CheckNoDuplicatePsks(ids)
	if !errors.Is(err, errDuplicatePsk) {
		t.Fatalf("err = %v, want errDuplicatePsk", err)
	}
	if want := "entries 0 and 2"; !strings.Contains(err.Error(), want) {
		t.Errorf("err = %q, want it to name %q; a refusal that cannot say which two entries collided is a refusal nobody can act on", err, want)
	}
}

// TestCheckNoDuplicatePsksAcceptsDistinctIds asserts the check does not over reject. Two
// ids that share a psk_id but carry different nonces are different PreSharedKeyIDs by RFC
// 9420 section 8.4's own definition -- the nonce is a field of the structure and a fresh
// one is required on every injection -- so refusing them would be an interop break rather
// than a safety margin.
func TestCheckNoDuplicatePsksAcceptsDistinctIds(t *testing.T) {
	ids := []PreSharedKeyId{pskTestId(t, 1, 0xa1), pskTestId(t, 1, 0xa2), pskTestId(t, 2, 0xa1)}
	if err := CheckNoDuplicatePsks(ids); err != nil {
		t.Fatalf("distinct ids were refused: %v", err)
	}
}

// TestCheckNoDuplicatePsksAcceptsEmptyAndSingleton asserts the degenerate cases are
// accepted. The singleton is the case that catches the classic shape of this bug, an id
// compared against itself because it was recorded before the comparison rather than after.
func TestCheckNoDuplicatePsksAcceptsEmptyAndSingleton(t *testing.T) {
	if err := CheckNoDuplicatePsks(nil); err != nil {
		t.Fatalf("empty list refused: %v", err)
	}
	if err := CheckNoDuplicatePsks([]PreSharedKeyId{}); err != nil {
		t.Fatalf("zero length list refused: %v", err)
	}
	if err := CheckNoDuplicatePsks([]PreSharedKeyId{pskTestId(t, 1, 0xa1)}); err != nil {
		t.Fatalf("singleton refused: %v", err)
	}
}

// TestCheckNoDuplicatePsksBindsTheEpochField asserts the epoch participates, so the same
// resumption group at two epochs is two ids and the same one twice is one.
func TestCheckNoDuplicatePsksBindsTheEpochField(t *testing.T) {
	nonce := repeatByte(0x5e, pskTestCrypto(t).HashSize())
	first := PreSharedKeyId{
		PskType:    PskTypeResumption,
		Usage:      ResumptionPskUsageApplication,
		PskGroupId: []byte{9},
		PskEpoch:   1,
		PskNonce:   nonce,
	}
	second := first
	second.PskEpoch = 2
	if err := CheckNoDuplicatePsks([]PreSharedKeyId{first, second}); err != nil {
		t.Fatalf("different epochs refused: %v", err)
	}
	if err := CheckNoDuplicatePsks([]PreSharedKeyId{first, first}); !errors.Is(err, errDuplicatePsk) {
		t.Fatalf("err = %v, want errDuplicatePsk", err)
	}
}

// TestCheckNoDuplicatePsksAgreesWithTheEncodingOnEveryFieldOfEveryArm is the rule 5 gate on
// ValSem403, and it is the test a field added to PreSharedKeyId later cannot slip past.
//
// The RFC's rule is over PreSharedKeyIDs -- "multiple PreSharedKey proposals that reference
// the same PreSharedKeyID" -- and a PreSharedKeyID is what its serialization says it is. So
// the property asserted here is exactly that equivalence, in BOTH directions, for every
// field of the struct against every hand derived arm: two ids that encode to the same bytes
// must be a duplicate, and two that do not must not be.
//
// Both directions are load bearing and they fail differently. A check that compared only
// psk_id would wave through a repeat that differs in the nonce -- no, worse: it would REFUSE
// two legitimately distinct ids, a V2 interop break. A check that compared the go fields
// would call two external ids differing only in psk_group_id distinct, when the external arm
// does not encode psk_group_id at all and the two are one id on the wire contributing one
// psk_input.
//
// Neither the field list nor the arm list is written down here. The fields come off the
// struct by reflection and the arms are the hand derived goldens, whose psktype coverage is
// itself checked against the registry read out of the package's own constant declarations.
func TestCheckNoDuplicatePsksAgreesWithTheEncodingOnEveryFieldOfEveryArm(t *testing.T) {
	structType := reflect.TypeOf(PreSharedKeyId{})
	if structType.NumField() == 0 {
		t.Fatal("PreSharedKeyId declares no fields, so this gate walked nothing")
	}
	goldens := goldenPskIds()
	if len(goldens) == 0 {
		t.Fatal("there are no hand derived goldens, so this gate has no arm to run against")
	}
	armsSeen := map[uint64]bool{}
	for _, golden := range goldens {
		armsSeen[uint64(golden.id.PskType)] = true
	}
	for value := range registeredPskTypes(t) {
		if !armsSeen[value] {
			t.Fatalf("no golden carries psktype %d, so this sweep never asked the check about that arm; the registry is read from the package's own declarations, so a new arm arrives here on the commit that declares it",
				value)
		}
	}

	identical, distinct, unencodable := 0, 0, 0
	for armName, golden := range goldens {
		for i := 0; i < structType.NumField(); i++ {
			fieldName := structType.Field(i).Name
			at := armName + "/" + fieldName

			base := clonePreSharedKeyId(golden.id)
			varied := clonePreSharedKeyId(golden.id)
			varyPreSharedKeyIdField(t, at, reflect.ValueOf(varied).Elem().Field(i))

			baseBytes, baseErr := syntax.Marshal(base)
			variedBytes, variedErr := syntax.Marshal(varied)
			answer := CheckNoDuplicatePsks([]PreSharedKeyId{*base, *varied})

			switch {
			case baseErr != nil || variedErr != nil:
				// an id this package cannot encode is one it cannot compare either, and
				// answering "not a duplicate" would put a pair it never looked at past the
				// only check that looks at them.
				if answer == nil || errors.Is(answer, errDuplicatePsk) {
					t.Errorf("%s: one of the pair does not encode (%v / %v) and the check answered %v, want a refusal that is not the duplicate one",
						at, baseErr, variedErr, answer)
				}
				unencodable++
			case bytes.Equal(baseBytes, variedBytes):
				if !errors.Is(answer, errDuplicatePsk) {
					t.Errorf("%s: varying the field left the encoding identical (%x), so the two are ONE PreSharedKeyID on the wire and contribute one psk_input, and the check answered %v",
						at, baseBytes, answer)
				}
				identical++
			default:
				if answer != nil {
					t.Errorf("%s: varying the field changed the encoding (%x -> %x), so the two are different PreSharedKeyIDs, and the check refused them: %v",
						at, baseBytes, variedBytes, answer)
				}
				distinct++
			}
		}
	}

	// a one sided sweep is the failure this whole file exists to avoid: if every pair came
	// out identical, an implementation that refused everything would pass, and if every pair
	// came out distinct, one that accepted everything would.
	if identical == 0 {
		t.Fatal("no field left any arm's encoding unchanged, so this sweep never asked the check to answer duplicate and an accept-everything implementation passes it")
	}
	if distinct == 0 {
		t.Fatal("no field changed any arm's encoding, so this sweep never asked the check to answer distinct and a refuse-everything implementation passes it")
	}
	t.Logf("%d field/arm pairs are one id on the wire, %d are two, %d cannot be encoded", identical, distinct, unencodable)
}

// TestCheckNoDuplicatePsksRefusesAnIdItCannotEncode asserts the marshal refusal is
// propagated rather than read as "not a duplicate". An unregistered psktype has no arm, so
// nothing can say where the id ends or whether two of them are the same, and a check that
// swallowed the error would report a clean list holding two copies of an unnamable id.
func TestCheckNoDuplicatePsksRefusesAnIdItCannotEncode(t *testing.T) {
	registered := registeredPskTypes(t)
	unregistered := PskType(0)
	for value := 0; value <= 0xff; value++ {
		if !registered[uint64(value)] {
			unregistered = PskType(value)
			break
		}
	}
	if registered[uint64(unregistered)] {
		t.Fatal("every psktype octet is registered, so there is no unencodable id to build")
	}
	bad := PreSharedKeyId{
		PskType:  unregistered,
		PskId:    []byte{1},
		PskNonce: repeatByte(0xaa, pskTestCrypto(t).HashSize()),
	}
	for _, ids := range [][]PreSharedKeyId{
		{bad},
		{bad, bad},
		{pskTestId(t, 1, 0xa1), bad},
		{bad, pskTestId(t, 1, 0xa1)},
	} {
		if err := CheckNoDuplicatePsks(ids); !errors.Is(err, errPskType) {
			t.Errorf("%d ids containing psktype %d: err = %v, want errPskType", len(ids), unregistered, err)
		}
	}
}

// ---------------------------------------------------------------------------
// task 15: PSKLabel, psk_secret and the empty case
// ---------------------------------------------------------------------------

// TestMarshalPskLabelMatchesTheHandDerivedGolden pins the PSKLabel preimage byte for byte.
//
// PSKLabel is a serialized structure and owes what every other one in this package owes: a
// golden written out from the RFC rather than captured from this encoder. It is
//
//	struct { PreSharedKeyID id; uint16 index; uint16 count; } PSKLabel;
//
// so the id goes in with no framing of its own, and index and count follow as two bare big
// endian uint16s in that order. Every one of those is a decision that changes psk_input and
// none of them is visible to a round trip: an index written at one octet, a count written
// before the index, a length prefix added around the id, or the index dropped entirely all
// produce a preimage this package agrees with itself about and nobody else does.
//
// The index and count values chosen are 0x0102 and 0x0304, not 0 and 1, because a two octet
// big endian field is indistinguishable from a one octet one, from a little endian one and
// from a uint32 when its value is 0 or 1.
func TestMarshalPskLabelMatchesTheHandDerivedGolden(t *testing.T) {
	for name, expected := range map[string]struct {
		id      *PreSharedKeyId
		index   uint16
		count   uint16
		encoded []byte
	}{
		// 8 octets of external id, then 01 02, then 03 04 -> 12 octets.
		"external at a wide index": {
			handDerivedExternalGoldenId(), 0x0102, 0x0304,
			joinBytes(handDerivedExternalGolden(), []byte{0x01, 0x02}, []byte{0x03, 0x04}),
		},
		// the singleton shape a one psk list uses: index 0 of a count of 1. Both fields are
		// still two octets, which is the whole of what this case adds -- 00 00 00 01 and not
		// 00 01.
		"external singleton": {
			handDerivedExternalGoldenId(), 0, 1,
			joinBytes(handDerivedExternalGolden(), []byte{0x00, 0x00}, []byte{0x00, 0x01}),
		},
		// 14 octets of resumption id, then 00 01, then 00 02 -> 18 octets. The resumption arm
		// is here because the id is written inline: an arm whose length the label encoder
		// assumed would move index and count without moving anything else.
		"resumption second of two": {
			handDerivedResumptionGoldenId(), 1, 2,
			joinBytes(handDerivedResumptionGolden(), []byte{0x00, 0x01}, []byte{0x00, 0x02}),
		},
	} {
		got, err := marshalPskLabel(expected.id, expected.index, expected.count)
		if err != nil {
			t.Errorf("%s: marshalPskLabel: %v", name, err)
			continue
		}
		if want := len(expected.encoded); len(got) != want {
			t.Errorf("%s: label is %d octets, want %d", name, len(got), want)
		}
		if !bytes.Equal(got, expected.encoded) {
			t.Errorf("%s: label = %x, want %x", name, got, expected.encoded)
		}
		// and the id really is inline rather than prefixed: the label starts with exactly the
		// id's own encoding and is four octets longer.
		idBytes, idErr := syntax.Marshal(expected.id)
		if idErr != nil {
			t.Errorf("%s: syntax.Marshal of the id: %v", name, idErr)
			continue
		}
		if len(got) != len(idBytes)+4 {
			t.Errorf("%s: label is %d octets and the id alone is %d; PSKLabel adds two uint16s and nothing else",
				name, len(got), len(idBytes))
		}
	}
}

// TestMarshalPskLabelCarriesIndexAndCountInSeparateFields asserts the two fields are two
// fields. A label that dropped either, wrote them into one octet each, or summed them would
// still be a plausible preimage, and psk_secret would still be 32 bytes of apparent random.
func TestMarshalPskLabelCarriesIndexAndCountInSeparateFields(t *testing.T) {
	id := handDerivedExternalGoldenId()
	label := func(index uint16, count uint16) []byte {
		t.Helper()
		encoded, err := marshalPskLabel(id, index, count)
		if err != nil {
			t.Fatalf("marshalPskLabel(%d, %d): %v", index, count, err)
		}
		return encoded
	}
	base := label(1, 3)
	if bytes.Equal(base, label(2, 3)) {
		t.Error("the index does not reach the label, so every psk of a list has the same preimage")
	}
	if bytes.Equal(base, label(1, 4)) {
		t.Error("the count does not reach the label, so a contribution can be lifted into a list of another length")
	}
	// the pair swapped: equal bytes here would mean the two are written into one field, or
	// combined, rather than kept apart.
	if bytes.Equal(label(1, 3), label(3, 1)) {
		t.Error("index and count are interchangeable in the label, so they are not two distinct fields")
	}
	// and the widths: 0x0100 differs from 0x0001 only above the first octet.
	if bytes.Equal(label(0x0001, 3), label(0x0100, 3)) {
		t.Error("an index of 0x0001 and one of 0x0100 produce the same label, so the field is not two octets wide")
	}
}

// TestPskSecretMatchesEveryUpstreamVector is the check that matters for this task.
//
// Every wrong fold returns KDF.Nh bytes of apparent random, so nothing this package can
// assert about its own output separates the recurrence of section 8.4 from a plausible one:
// not the length, not the avalanche, not that reordering changes it. A published answer
// computed by another implementation is the only thing that can, and psk_secret.json carries
// one for every list length from zero to ten at seven ciphersuites.
//
// What each accounting assertion below is protecting against, since a sweep that ran nothing
// reports exactly what a clean one reports: no vector at all means a corpus or registry
// regression; a single suite means a registry or corpus regression dropped one, which is a
// weaker statement than it looks and the accounting below says why; no empty list
// means the case every epoch of this product actually takes is unchecked; no singleton means
// the "derived psk" label and the PSKLabel encoding are unchecked; and no list of two or more
// means the recurrence was never folded, which is where the accumulator and the index live.
func TestPskSecretMatchesEveryUpstreamVector(t *testing.T) {
	ran, skipped := 0, 0
	suitesSeen := map[uint16]int{}
	nhSeen := map[int]int{}
	countsSeen := map[int]int{}
	for index, vector := range loadPskSecretVectors(t) {
		suite := CipherSuite(vector.CipherSuite)
		if !IsRegisteredSuite(suite) {
			skipped++
			continue
		}
		crypto, err := NewCryptoProvider(suite)
		if err != nil {
			t.Fatalf("vector %d: NewCryptoProvider(%#04x): %v", index, vector.CipherSuite, err)
		}
		psks := make([]PreSharedKeyInput, 0, len(vector.Psks))
		for position, entry := range vector.Psks {
			at := fmt.Sprintf("vector %d psk %d", index, position)
			psks = append(psks, PreSharedKeyInput{
				Id: PreSharedKeyId{
					PskType:  PskTypeExternal,
					PskId:    mustDecodeHex(t, at+" psk_id", entry.PskId),
					PskNonce: mustDecodeHex(t, at+" psk_nonce", entry.PskNonce),
				},
				Secret: mustDecodeHex(t, at+" psk", entry.Psk),
			})
		}
		want := mustDecodeHex(t, fmt.Sprintf("vector %d psk_secret", index), vector.PskSecret)
		if len(want) != crypto.HashSize() {
			t.Fatalf("vector %d: the published psk_secret is %d octets and the suite's KDF.Nh is %d, so this comparison is not the one the corpus intends",
				index, len(want), crypto.HashSize())
		}
		got, err := PskSecret(crypto, psks)
		if err != nil {
			t.Errorf("vector %d (suite %#04x, %d psks): PskSecret: %v", index, vector.CipherSuite, len(psks), err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("vector %d (suite %#04x, %d psks): psk_secret = %x, want %x",
				index, vector.CipherSuite, len(psks), got, want)
		}
		ran++
		suitesSeen[vector.CipherSuite]++
		nhSeen[crypto.HashSize()]++
		countsSeen[len(psks)]++
	}

	if ran == 0 {
		t.Fatalf("no psk_secret vector ran (%d skipped at unimplemented suites), so this test compared against nothing", skipped)
	}
	if len(suitesSeen) < 2 {
		t.Fatalf("every vector that ran was at one ciphersuite (%v), so a registry or corpus regression that dropped one would leave this sweep reporting exactly what a complete one reports", suitesSeen)
	}
	// and what more than one suite does NOT buy, recorded rather than assumed. Both registered
	// suites are HKDF-SHA256, so every vector that runs here is at KDF.Nh 32 and nothing in
	// this sweep separates the recurrence from one pinned to sha256 -- the corpus does carry
	// SHA-384 and SHA-512 suites and every one of those vectors is skipped above. The
	// differential that covers that is TestPskSecretReadsKdfNhFromTheProvider, over a provider
	// whose whole hash and kdf surface is one width up; this stops being a limitation on the
	// day a wider suite is registered, which is why the width is counted rather than asserted.
	if len(nhSeen) < 2 {
		t.Logf("every vector that ran was at KDF.Nh %v, so this sweep cannot tell the recurrence from one pinned to that width; TestPskSecretReadsKdfNhFromTheProvider is what does",
			nhSeen)
	}
	if countsSeen[0] == 0 {
		t.Fatal("no vector with an empty psk list ran, so the empty answer is pinned against nothing but this package's own opinion of it")
	}
	if countsSeen[1] == 0 {
		t.Fatal("no vector with exactly one psk ran, so the \"derived psk\" label and the PSKLabel encoding are unchecked")
	}
	folded := 0
	for count, seen := range countsSeen {
		if count >= 2 {
			folded += seen
		}
	}
	if folded == 0 {
		t.Fatal("no vector with two or more psks ran, so the recurrence was never folded and the accumulator is unchecked")
	}
	t.Logf("psk_secret: %d vectors ran, %d skipped at unimplemented suites; suites %v at KDF.Nh %v; %d of the runs folded two psks or more",
		ran, skipped, suitesSeen, nhSeen, folded)
}

// TestEmptyPskSecretMatchesTheUpstreamEmptyVector pins the empty answer against the corpus
// rather than against this package's own ZeroSecret.
//
// This is the case that matters most and is the easiest to fake. Every v1 epoch has no PSKs,
// so this value goes into every key schedule this product derives; and "KDF.Nh zero bytes"
// is a claim a test written from the implementation would make whatever the implementation
// did, because an implementation that hashed nothing would be compared against a hash of
// nothing. The published vector is written by somebody who was not looking at this code.
func TestEmptyPskSecretMatchesTheUpstreamEmptyVector(t *testing.T) {
	checked := 0
	for index, vector := range loadPskSecretVectors(t) {
		suite := CipherSuite(vector.CipherSuite)
		if !IsRegisteredSuite(suite) || len(vector.Psks) != 0 {
			continue
		}
		crypto, err := NewCryptoProvider(suite)
		if err != nil {
			t.Fatalf("vector %d: NewCryptoProvider(%#04x): %v", index, vector.CipherSuite, err)
		}
		want := mustDecodeHex(t, fmt.Sprintf("vector %d psk_secret", index), vector.PskSecret)
		if got := EmptyPskSecret(crypto); !bytes.Equal(got, want) {
			t.Errorf("vector %d (suite %#04x): EmptyPskSecret = %x, want %x", index, vector.CipherSuite, got, want)
		}
		general, err := PskSecret(crypto, nil)
		if err != nil {
			t.Fatalf("vector %d: PskSecret(nil): %v", index, err)
		}
		if !bytes.Equal(general, want) {
			t.Errorf("vector %d (suite %#04x): PskSecret(nil) = %x, want %x", index, vector.CipherSuite, general, want)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("the corpus carries no empty psk list at a registered suite, so the empty answer was compared against nothing")
	}
	t.Logf("%d published empty psk_secret vectors matched", checked)
}

// TestEmptyPskSecretMatchesPskSecretOfNil pins the convenience form against the general one.
// The lifecycle plan calls EmptyPskSecret at every epoch boundary, so a divergence between
// the two would be a group whose members disagree from epoch 0 and whose only symptom is that
// nothing decrypts.
func TestEmptyPskSecretMatchesPskSecretOfNil(t *testing.T) {
	for _, suite := range Suites() {
		crypto, err := NewCryptoProvider(suite)
		if err != nil {
			t.Fatalf("NewCryptoProvider(%#04x): %v", uint16(suite), err)
		}
		general, err := PskSecret(crypto, nil)
		if err != nil {
			t.Fatalf("suite %#04x: PskSecret(nil): %v", uint16(suite), err)
		}
		empty := EmptyPskSecret(crypto)
		if !bytes.Equal(empty, general) {
			t.Errorf("suite %#04x: EmptyPskSecret = %x, PskSecret(nil) = %x", uint16(suite), empty, general)
		}
		if len(empty) != crypto.HashSize() {
			t.Errorf("suite %#04x: len = %d, want KDF.Nh = %d", uint16(suite), len(empty), crypto.HashSize())
		}
		// the key schedule erases what it has finished with, so a shared array would come
		// back cleared on the next epoch with nothing to say why.
		empty[0] = 1
		if EmptyPskSecret(crypto)[0] != 0 {
			t.Errorf("suite %#04x: EmptyPskSecret returns a shared slice", uint16(suite))
		}
		general[0] = 1
		if again, _ := PskSecret(crypto, nil); again[0] != 0 {
			t.Errorf("suite %#04x: PskSecret(nil) returns a shared slice", uint16(suite))
		}
	}
}

// TestPskSecretReadsKdfNhFromTheProvider is the input the registered suites cannot supply.
//
// KDF.Nh governs four separate things in this recurrence, and both registered suites fix it at
// 32, so a literal 32 answers all four correctly for every input already in this tree: the
// width of psk_secret_[0], the nonce length ValSem401 requires, the number of bytes
// psk_input_[i] is expanded to, and the width of the answer.
//
// Three of the four are lengths a caller can see. The expansion is not, and it is the one that
// hides: psk_secret_[i] = Extract(psk_input_[i-1], psk_secret_[i-1]) answers the provider's Nh
// whatever width psk_input happened to be, so an expansion pinned to a literal moves the VALUE
// of every non empty psk_secret and moves no length at all. The package wide gate in
// key_schedule_test.go compares lengths and is structurally blind to that, which is why the
// requested expansion widths are read here instead. Measured, not supposed: crypto.HashSize()
// -> 32 at that call survived the whole of this package, and ZeroSecret(crypto) ->
// make([]byte, 32) at psk_secret_[0] survived it too.
//
// The provider is the coherent wide one -- SHA-384 and HKDF-SHA384 throughout, not a fake
// answering a bigger number out of HashSize -- because psk_secret is Extract's output, and a
// provider whose declared Nh disagreed with its own kdf would report a width nothing derived.
// pskHashSizeProvider above is the other kind and is deliberately not used here.
func TestPskSecretReadsKdfNhFromTheProvider(t *testing.T) {
	inner := pskTestCrypto(t)
	crypto := &wideKdfProvider{CryptoProvider: inner}
	if inner.HashSize() == crypto.HashSize() {
		t.Fatalf("both providers answer KDF.Nh %d, so nothing below separates a width read off the provider from one written down",
			inner.HashSize())
	}
	nh := crypto.HashSize()

	// psk_secret_[0], and the two spellings of it held against each other at a width where a
	// literal is visible at all. TestEmptyPskSecretMatchesPskSecretOfNil pins the same pair
	// over Suites(), where both entries are Nh 32 and the two agree however either is written.
	empty, err := PskSecret(crypto, nil)
	if err != nil {
		t.Fatalf("PskSecret(nil) over a provider whose KDF.Nh is %d: %v", nh, err)
	}
	if len(empty) != nh {
		t.Errorf("PskSecret(nil) answered %d bytes over a provider whose KDF.Nh is %d", len(empty), nh)
	}
	if convenience := EmptyPskSecret(crypto); !bytes.Equal(convenience, empty) {
		t.Errorf("over a provider whose KDF.Nh is %d, EmptyPskSecret = %x and PskSecret(nil) = %x; a group whose members take the two different spellings of psk_secret_[0] disagrees from epoch 0 and nothing decrypts",
			nh, convenience, empty)
	}
	if slices.ContainsFunc(empty, func(b byte) bool { return b != 0 }) {
		t.Errorf("psk_secret_[0] is %x, which is not the all zero string", empty)
	}

	// the non empty recurrence. Two entries, so the fold runs and not only its first step,
	// and both arms of the select() so neither encoding is the only one expanded over.
	psks := []PreSharedKeyInput{
		{
			Id: PreSharedKeyId{
				PskType:  PskTypeExternal,
				PskId:    repeatByte(0x11, 16),
				PskNonce: repeatByte(0x22, nh),
			},
			Secret: repeatByte(0x33, nh),
		},
		{
			Id: PreSharedKeyId{
				PskType:    PskTypeResumption,
				Usage:      ResumptionPskUsageApplication,
				PskGroupId: repeatByte(0x44, 12),
				PskEpoch:   7,
				PskNonce:   repeatByte(0x55, nh),
			},
			Secret: repeatByte(0x66, nh),
		},
	}
	crypto.expandedLengths = nil
	secret, err := PskSecret(crypto, psks)
	if err != nil {
		t.Fatalf("PskSecret over a provider whose KDF.Nh is %d: %v", nh, err)
	}
	if len(secret) != nh {
		t.Errorf("psk_secret is %d bytes over a provider whose KDF.Nh is %d", len(secret), nh)
	}
	// one expansion per entry, each asked for THIS provider's Nh. The count is derived from
	// the list rather than written down, so a recurrence that grew a step has to say so here.
	wantExpansions := make([]int, len(psks))
	for i := range wantExpansions {
		wantExpansions[i] = nh
	}
	if !slices.Equal(crypto.expandedLengths, wantExpansions) {
		t.Errorf("psk_input was expanded to %v over a provider whose KDF.Nh is %d, want %v; that width is written down rather than read off the provider, and Extract answers Nh either way so no length in the answer can show it",
			crypto.expandedLengths, nh, wantExpansions)
	}

	// and the refusal direction, because a body that read the provider for the expansion and
	// wrote 32 down for ValSem401 passes everything above: a nonce of the NARROW provider's Nh
	// is the wrong length for this one and has to be refused as such.
	narrowNonce := psks[0]
	narrowNonce.Id = *clonePreSharedKeyId(&psks[0].Id)
	narrowNonce.Id.PskNonce = repeatByte(0x22, inner.HashSize())
	if _, err := PskSecret(crypto, []PreSharedKeyInput{narrowNonce}); !errors.Is(err, errPskNonceLength) {
		t.Errorf("a %d byte nonce over a provider whose KDF.Nh is %d gave err = %v, want errPskNonceLength",
			inner.HashSize(), nh, err)
	}
}

// pskSecretTwoInputs returns the two psk inputs of the first two entry vector at a registered
// suite, together with that suite's provider. Reading them out of the corpus rather than
// writing two constants here keeps the fixtures for the tests below the same bytes the KAT
// runs on.
func pskSecretTwoInputs(t *testing.T) (CryptoProvider, []PreSharedKeyInput) {
	t.Helper()
	for index, vector := range loadPskSecretVectors(t) {
		suite := CipherSuite(vector.CipherSuite)
		if !IsRegisteredSuite(suite) || len(vector.Psks) != 2 {
			continue
		}
		crypto, err := NewCryptoProvider(suite)
		if err != nil {
			t.Fatalf("vector %d: NewCryptoProvider(%#04x): %v", index, vector.CipherSuite, err)
		}
		psks := []PreSharedKeyInput{}
		for position, entry := range vector.Psks {
			at := fmt.Sprintf("vector %d psk %d", index, position)
			psks = append(psks, PreSharedKeyInput{
				Id: PreSharedKeyId{
					PskType:  PskTypeExternal,
					PskId:    mustDecodeHex(t, at+" psk_id", entry.PskId),
					PskNonce: mustDecodeHex(t, at+" psk_nonce", entry.PskNonce),
				},
				Secret: mustDecodeHex(t, at+" psk", entry.Psk),
			})
		}
		return crypto, psks
	}
	t.Fatal("the corpus carries no two entry psk list at a registered suite")
	return nil, nil
}

// TestPskSecretChangesWhenTheListIsReordered asserts the fold is not order independent, so a
// peer that reorders the psks of a commit derives a different epoch and is detected.
//
// What this observes and what it does NOT observe are worth separating, because the obvious
// reading of it is wrong. The chain psk_secret_[i] = Extract(psk_input_[i-1], psk_secret_[i-1])
// is already non commutative on its own, so this test would still pass against a PSKLabel
// carrying no index at all. Index and count binding is pinned by the hand derived label golden
// and by the published corpus above; this one is the weaker, separate statement that the
// caller's order reaches the answer.
func TestPskSecretChangesWhenTheListIsReordered(t *testing.T) {
	crypto, psks := pskSecretTwoInputs(t)
	forward, err := PskSecret(crypto, []PreSharedKeyInput{psks[0], psks[1]})
	if err != nil {
		t.Fatalf("PskSecret forward: %v", err)
	}
	reversed, err := PskSecret(crypto, []PreSharedKeyInput{psks[1], psks[0]})
	if err != nil {
		t.Fatalf("PskSecret reversed: %v", err)
	}
	if bytes.Equal(forward, reversed) {
		t.Fatal("psk_secret is order independent, so the caller's order does not reach the answer")
	}
}

// TestPskSecretRejectsInvalidEntries asserts ValSem401, ValSem402 and ValSem403 all fire from
// inside the computation, so no caller reaches a psk_secret over an invalid list by skipping a
// separate validation step it did not know it owed.
//
// Every POSITION of the list is swept rather than the head. The two per entry rules are stated
// per entry, and the loop that checks them is one character away from a loop that checks
// psks[0] on every turn -- which refuses a bad first entry, folds a ValSem401 nonce or a
// ValSem402 ReInit resumption into the answer at every other position, and is indistinguishable
// from the shipped body over a list of one. Measured, not supposed: psks[i].Id.Validate ->
// psks[0].Id.Validate survived the whole of this package while all three cases below were
// singletons.
//
// The positions are derived from the list the sweep builds rather than written down, so a
// wider list carries the rules to its new positions by construction rather than by somebody
// remembering to add a row.
func TestPskSecretRejectsInvalidEntries(t *testing.T) {
	crypto, good := pskSecretTwoInputs(t)
	base := good[0]

	shortNonce := base
	shortNonce.Id = *clonePreSharedKeyId(&base.Id)
	shortNonce.Id.PskNonce = shortNonce.Id.PskNonce[:crypto.HashSize()-1]

	reinit := base
	reinit.Id = PreSharedKeyId{
		PskType:    PskTypeResumption,
		Usage:      ResumptionPskUsageReInit,
		PskGroupId: []byte{1},
		PskNonce:   base.Id.PskNonce,
	}

	// the per entry rules, threaded through every position of a list whose other entries are
	// valid and distinct. Distinct matters: a repeat would be refused by ValSem403 before the
	// per entry loop is reached, and every row here would then pass for the wrong reason.
	for _, invalid := range []struct {
		name  string
		entry PreSharedKeyInput
		want  error
	}{
		{name: "short nonce", entry: shortNonce, want: errPskNonceLength},
		{name: "reinit usage", entry: reinit, want: errPskType},
	} {
		for position := 0; position <= len(good); position++ {
			list := make([]PreSharedKeyInput, 0, len(good)+1)
			list = append(list, good[:position]...)
			list = append(list, invalid.entry)
			list = append(list, good[position:]...)

			secret, err := PskSecret(crypto, list)
			if !errors.Is(err, invalid.want) {
				t.Errorf("%s at position %d of %d: err = %v, want %v",
					invalid.name, position, len(list), err, invalid.want)
			}
			// and the refusal is a refusal: nothing comes back beside the error for a caller
			// that ignored it to mix into an epoch.
			if secret != nil {
				t.Errorf("%s at position %d of %d: %d bytes came back beside %v",
					invalid.name, position, len(list), len(secret), err)
			}
			// and it names the entry it refused. A body that read the head every turn and
			// reported the index of the turn would answer the right sentinel about the wrong
			// psk, and the operator chasing the refusal would be looking at a valid one.
			if err != nil && !strings.Contains(err.Error(), fmt.Sprintf("psk %d:", position)) {
				t.Errorf("%s at position %d of %d was refused with %q, which does not name the entry that was refused",
					invalid.name, position, len(list), err)
			}
		}
	}

	// ValSem403 at every PAIR of positions rather than at the adjacent first two. The
	// duplicate check is a sweep of its own, and a body comparing only neighbours -- or only
	// against the head -- takes a list whose repeat is at the far end.
	const width = 3
	pairs := 0
	for first := 0; first < width; first++ {
		for second := first + 1; second < width; second++ {
			list := make([]PreSharedKeyInput, width)
			for at := range list {
				if at == first || at == second {
					list[at] = base
				} else {
					list[at] = good[1]
				}
			}
			pairs++

			secret, err := PskSecret(crypto, list)
			if !errors.Is(err, errDuplicatePsk) {
				t.Errorf("one id at positions %d and %d of %d: err = %v, want errDuplicatePsk",
					first, second, width, err)
			}
			if secret != nil {
				t.Errorf("one id at positions %d and %d of %d: %d bytes came back beside %v",
					first, second, width, len(secret), err)
			}
			if err != nil && !strings.Contains(err.Error(), fmt.Sprintf("entries %d and %d", first, second)) {
				t.Errorf("one id at positions %d and %d of %d was refused with %q, which does not name the pair that repeated",
					first, second, width, err)
			}
		}
	}
	if pairs != width*(width-1)/2 {
		t.Errorf("%d pairs of positions were swept over a list of %d, want %d", pairs, width, width*(width-1)/2)
	}
}

// TestPskSecretLeavesTheCallersInputsAlone asserts the erasure of the intermediates does not
// reach the caller's psks. PskSecret zeroizes what it derived, and a zeroize aimed one slice
// too far would clear the caller's own psk secrets in place -- which looks like nothing at all
// until the second call over the same list returns a different answer.
func TestPskSecretLeavesTheCallersInputsAlone(t *testing.T) {
	crypto, psks := pskSecretTwoInputs(t)
	before := make([]PreSharedKeyInput, 0, len(psks))
	for _, psk := range psks {
		before = append(before, PreSharedKeyInput{Id: *clonePreSharedKeyId(&psk.Id), Secret: cloneBytes(psk.Secret)})
	}
	first, err := PskSecret(crypto, psks)
	if err != nil {
		t.Fatalf("PskSecret: %v", err)
	}
	for i := range psks {
		if !preSharedKeyIdsAgree(&psks[i].Id, &before[i].Id) {
			t.Errorf("psk %d: the id was modified, now %s, was %s",
				i, describePreSharedKeyId(&psks[i].Id), describePreSharedKeyId(&before[i].Id))
		}
		if !bytes.Equal(psks[i].Secret, before[i].Secret) {
			t.Errorf("psk %d: the secret was modified, now %x, was %x", i, psks[i].Secret, before[i].Secret)
		}
	}
	second, err := PskSecret(crypto, psks)
	if err != nil {
		t.Fatalf("PskSecret again: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("two calls over one list gave %x then %x", first, second)
	}
}

// TestPskSecretRefusesAListLongerThanTheUint16Count pins the boundary of the count field at
// the value it actually is. The list length is written into PSKLabel.count as a uint16, so
// 65535 entries is the largest list that can be labelled and 65536 is not a slightly awkward
// case, it is a list every member labels differently from the sender.
//
// Both sides are asserted. A gate written as >= would refuse a list the protocol permits, and
// nothing but the accepted side of the boundary can see that.
func TestPskSecretRefusesAListLongerThanTheUint16Count(t *testing.T) {
	crypto := pskTestCrypto(t)
	nonce := repeatByte(0x5a, crypto.HashSize())
	secret := repeatByte(0x6b, crypto.HashSize())
	build := func(n int) []PreSharedKeyInput {
		psks := make([]PreSharedKeyInput, n)
		for i := range psks {
			psks[i] = PreSharedKeyInput{
				Id: PreSharedKeyId{
					PskType:  PskTypeExternal,
					PskId:    []byte{byte(i), byte(i >> 8), byte(i >> 16)},
					PskNonce: nonce,
				},
				Secret: secret,
			}
		}
		return psks
	}

	if _, err := PskSecret(crypto, build(math.MaxUint16+1)); !errors.Is(err, ErrPskCount) {
		t.Errorf("a list of %d: err = %v, want ErrPskCount", math.MaxUint16+1, err)
	}
	accepted, err := PskSecret(crypto, build(math.MaxUint16))
	if err != nil {
		t.Fatalf("a list of %d was refused: %v", math.MaxUint16, err)
	}
	if len(accepted) != crypto.HashSize() {
		t.Errorf("a list of %d gave %d octets, want KDF.Nh = %d", math.MaxUint16, len(accepted), crypto.HashSize())
	}
}

// ---------------------------------------------------------------------------
// the psk list's rule for the provider stub gate
// ---------------------------------------------------------------------------

// providerPskInputBytePaths is every byte slice one PreSharedKeyInput reaches, as the field
// index path that locates it and the words that name it, in declaration order.
//
// Read off the type by reflection rather than written down, for rule 5's reason: a field
// added to PreSharedKeyInput or to PreSharedKeyId later is moved by the stub gate on the
// commit that adds it, rather than on the commit somebody remembers to widen a list. A list
// would understate the class exactly the way the ones this project keeps rewriting did.
func providerPskInputBytePaths(structType reflect.Type, prefix string) ([][]int, []string) {
	paths := [][]int{}
	names := []string{}
	for i := 0; i < structType.NumField(); i++ {
		field := structType.Field(i)
		switch {
		case field.Type.Kind() == reflect.Slice && field.Type.Elem().Kind() == reflect.Uint8:
			paths = append(paths, []int{i})
			names = append(names, prefix+field.Name)
		case field.Type.Kind() == reflect.Struct:
			nested, nestedNames := providerPskInputBytePaths(field.Type, prefix+field.Name+".")
			for at, path := range nested {
				paths = append(paths, append([]int{i}, path...))
				names = append(names, nestedNames[at])
			}
		}
	}
	return paths, names
}

// providerClonedPskInputs copies a psk list deeply enough that a perturbation of one byte
// cannot write through into the base argument every other row of the stub gate is built
// from. The byte fields are the only thing shared by a shallow copy, and they are the only
// thing moved, so copying those is copying everything that matters.
func providerClonedPskInputs(argument reflect.Value) reflect.Value {
	clone := reflect.MakeSlice(argument.Type(), argument.Len(), argument.Len())
	reflect.Copy(clone, argument)
	paths, _ := providerPskInputBytePaths(argument.Type().Elem(), "")
	for entry := 0; entry < clone.Len(); entry++ {
		for _, path := range paths {
			field := clone.Index(entry).FieldByIndex(path)
			if field.Len() == 0 {
				continue
			}
			copied := make([]byte, field.Len())
			reflect.Copy(reflect.ValueOf(copied), field)
			field.Set(reflect.ValueOf(copied))
		}
	}
	return clone
}

// providerPskInputContributes answers whether two entries differ in anything psk_secret is
// computed over: the entry's serialized PreSharedKeyID, or its secret bytes.
//
// The serialization rather than the fields, because the select() arms mean the two are not
// the same question. A psk_group_id set on an external id is not part of that id -- the
// external arm does not encode it -- so an implementation that read every byte it was handed
// still answers identically with that field moved, and a stub gate row over it would report a
// defect that is not there.
func providerPskInputContributes(t *testing.T, before reflect.Value, after reflect.Value) bool {
	t.Helper()
	first, ok := before.Interface().(PreSharedKeyInput)
	if !ok {
		t.Fatalf("the base psk entry is a %s rather than a PreSharedKeyInput", before.Type())
	}
	second := after.Interface().(PreSharedKeyInput)
	if !bytes.Equal(first.Secret, second.Secret) {
		return true
	}
	firstId, err := syntax.Marshal(&first.Id)
	if err != nil {
		t.Fatalf("the base psk entry does not encode: %v", err)
	}
	secondId, err := syntax.Marshal(&second.Id)
	if err != nil {
		t.Fatalf("the moved psk entry does not encode: %v", err)
	}
	return !bytes.Equal(firstId, secondId)
}

// providerPskInputPerturbations is the stub gate's rule for a psk list: one byte of one field
// of one entry moved, everything else identical, the list's length and every field's length
// preserved for the reason providerPerturbations gives about shape.
//
// Only the byte fields are moved, and only where moving one changes what that entry
// contributes. Both restrictions are about not asking a question this gate is not about. A
// psktype or a usage moved takes the list from accepted to refused, which is ValSem402's
// question rather than "was the argument read", and it would be answered by a refusal from a
// function that had not looked at anything else. And a field the entry's own arm does not
// encode is not part of that id at all, so it cannot reach the answer however completely the
// implementation reads its inputs; providerPskInputContributes decides that from the wire
// format rather than from this file's opinion of it.
//
// A base argument nothing can be moved in is fatal, not empty: a psk list whose entries carry
// no encodable difference would leave PskSecret with an argument the gate calls twice with the
// identical value, and report it as observed.
func providerPskInputPerturbations(t *testing.T, operation string, parameter providerParameter, argument reflect.Value) []providerPerturbation {
	t.Helper()
	if argument.Len() == 0 {
		t.Fatalf("the base argument for %s.%s is empty, so perturbing it changes nothing", operation, parameter.name)
	}
	paths, names := providerPskInputBytePaths(argument.Type().Elem(), "")
	if len(paths) == 0 {
		t.Fatalf("PreSharedKeyInput reaches no byte field, so nothing in %s.%s can be moved", operation, parameter.name)
	}
	moved := []providerPerturbation{}
	for entry := 0; entry < argument.Len(); entry++ {
		for at, path := range paths {
			if argument.Index(entry).FieldByIndex(path).Len() == 0 {
				continue
			}
			for _, position := range perturbedPositions(argument.Index(entry).FieldByIndex(path).Len()) {
				perturbed := providerClonedPskInputs(argument)
				target := perturbed.Index(entry).FieldByIndex(path)
				replacement := make([]byte, target.Len())
				reflect.Copy(reflect.ValueOf(replacement), target)
				replacement[position] ^= 0xff
				target.Set(reflect.ValueOf(replacement))
				if !providerPskInputContributes(t, argument.Index(entry), perturbed.Index(entry)) {
					continue
				}
				moved = append(moved, providerPerturbation{
					where: fmt.Sprintf("byte %d of %d of psk %d's %s", position, target.Len(), entry, names[at]),
					value: perturbed,
				})
			}
		}
	}
	if len(moved) == 0 {
		t.Fatalf("no byte of the base argument for %s.%s changes what an entry contributes, so the gate would call it twice with the same value",
			operation, parameter.name)
	}
	return moved
}

// TestProviderPskInputPerturbationsMovesEveryEncodedFieldOfBothArms is the positive control on
// the rule above, and it is here because the rule can go quiet in two directions and a quiet
// one issues the stub gate exactly the clean bill a working one issues.
//
// Too few: a rule that skipped a field, or that produced nothing at all, leaves PskSecret's
// only movable argument unprobed and the stub gate silent about it. Too many: a rule that
// moved a field the arm does not encode would report a correct PskSecret as one that ignores
// its input, because no implementation can answer differently to a byte the wire format does
// not carry.
//
// Both fixtures are run, and the second is what makes the first direction's opposite testable
// at all. The stub gate's own psks argument is canonical -- each arm leaves the other arm's
// fields at their zero values -- so every byte field it carries reaches its encoding, and a
// rule that dropped the contributes filter entirely passes over it. The loaded fixture puts a
// psk_group_id on an external id and a psk_id on a resumption one, so the filter has something
// to remove and its absence is a perturbation that does not move psk_secret.
//
// Neither the field list nor the set of encoded fields is written down: the fields come off the
// struct by reflection and which of them an arm carries is decided by re-encoding.
func TestProviderPskInputPerturbationsMovesEveryEncodedFieldOfBothArms(t *testing.T) {
	params, err := LookupSuite(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("look up the suite the fixture is built at: %v", err)
	}
	arguments := providerStubArguments(t, params, pskTestCrypto(t))
	canonical, resolved := arguments["psks"].([]PreSharedKeyInput)
	if !resolved {
		t.Fatal("the stub gate's base arguments hold no psks entry, so PskSecret cannot be called by it")
	}
	if len(canonical) < 2 {
		t.Fatalf("the psks fixture holds %d entries; the recurrence has no accumulator before the second, so a fold that dropped it would not be moved by any perturbation of a shorter list",
			len(canonical))
	}
	paths, names := providerPskInputBytePaths(reflect.TypeOf(PreSharedKeyInput{}), "")
	if len(paths) == 0 {
		t.Fatal("PreSharedKeyInput reaches no byte field, so this control walked nothing")
	}
	crypto := pskTestCrypto(t)

	check := func(label string, base []PreSharedKeyInput) {
		argument := reflect.ValueOf(base)
		perturbations := providerPskInputPerturbations(t, "PskSecret",
			providerParameter{name: "psks", typeName: "[]PreSharedKeyInput"}, argument)
		if len(perturbations) == 0 {
			t.Errorf("%s: the rule produced no perturbation", label)
			return
		}
		answer, answerErr := PskSecret(crypto, base)
		if answerErr != nil {
			t.Errorf("%s: PskSecret over the fixture: %v", label, answerErr)
			return
		}

		// every perturbation really does move psk_secret, which is the property the stub gate
		// reads out of this rule. A perturbation that did not is the false failure the
		// contributes filter exists to prevent.
		for _, perturbation := range perturbations {
			moved, movedErr := PskSecret(crypto, perturbation.value.Interface().([]PreSharedKeyInput))
			if movedErr != nil {
				t.Errorf("%s: %s: PskSecret refused the moved list: %v", label, perturbation.where, movedErr)
				continue
			}
			if bytes.Equal(moved, answer) {
				t.Errorf("%s: %s: psk_secret did not move, so the stub gate would read this row as an argument PskSecret ignores",
					label, perturbation.where)
			}
		}
		// and no perturbation wrote through into the fixture every other row is built from
		if again, againErr := PskSecret(crypto, base); againErr != nil || !bytes.Equal(again, answer) {
			t.Errorf("%s: the fixture answered %x and then %x (%v), so a perturbation wrote through into it",
				label, answer, again, againErr)
		}

		// the coverage half, in both directions: exactly the byte fields that reach an arm's
		// encoding, no more and no fewer.
		want := map[string]bool{}
		for entry := 0; entry < argument.Len(); entry++ {
			for at, path := range paths {
				if argument.Index(entry).FieldByIndex(path).Len() == 0 {
					continue
				}
				probe := providerClonedPskInputs(argument)
				target := probe.Index(entry).FieldByIndex(path)
				replacement := make([]byte, target.Len())
				reflect.Copy(reflect.ValueOf(replacement), target)
				replacement[0] ^= 0xff
				target.Set(reflect.ValueOf(replacement))
				if providerPskInputContributes(t, argument.Index(entry), probe.Index(entry)) {
					want[fmt.Sprintf("psk %d's %s", entry, names[at])] = true
				}
			}
		}
		if len(want) == 0 {
			t.Errorf("%s: no field of either arm reaches its encoding, so the rule has nothing it must cover", label)
			return
		}
		got := map[string]bool{}
		for _, perturbation := range perturbations {
			got[pskPerturbationField(t, perturbation.where)] = true
		}
		for field := range want {
			if !got[field] {
				t.Errorf("%s: %s reaches the encoding and the rule never moves it, so the stub gate probes PskSecret with that field held constant",
					label, field)
			}
		}
		for field := range got {
			if !want[field] {
				t.Errorf("%s: the rule moves %s, which does not reach the encoding, so a correct PskSecret is reported as ignoring its input",
					label, field)
			}
		}
		t.Logf("%s: %d perturbations over %d encoded fields", label, len(perturbations), len(want))
	}

	check("the stub gate's own fixture", canonical)

	// each arm loaded with the other arm's fields. Neither is encoded, so neither may be
	// moved, and without this fixture the filter that decides so is never exercised.
	loaded := providerClonedPskInputs(reflect.ValueOf(canonical)).Interface().([]PreSharedKeyInput)
	loaded[0].Id.PskGroupId = repeatByte(0xc1, 12)
	loaded[0].Id.PskEpoch = 5
	loaded[1].Id.PskId = repeatByte(0xc2, 16)
	if loaded[0].Id.PskType == loaded[1].Id.PskType {
		t.Fatalf("both fixture entries are psktype %d, so loading each with the other arm's fields loads nothing",
			uint8(loaded[0].Id.PskType))
	}
	check("both arms loaded with the other's fields", loaded)
}

// pskPerturbationField reads the field a perturbation's words name back out of them, so the
// coverage comparison above is over what the rule actually reported rather than over a second
// enumeration of what it should have.
func pskPerturbationField(t *testing.T, where string) string {
	t.Helper()
	_, after, found := strings.Cut(where, " of ")
	if !found {
		t.Fatalf("a perturbation is described as %q, which this control cannot attribute to a field", where)
	}
	_, field, found := strings.Cut(after, " of ")
	if !found {
		t.Fatalf("a perturbation is described as %q, which this control cannot attribute to a field", where)
	}
	return field
}
