// What the suite identifiers and the labelled kdf have to be, stated against RFC 9180's
// own published bytes rather than against this package's other half.
//
// A labelled kdf returns 32 well formed bytes for every wrong construction there is. Swap
// the salt and the ikm, drop the version label, leave out the two byte length prefix,
// transpose the kdf and aead code points — each one still extracts, still expands, still
// errors nowhere, and still hands back keys of exactly the right length that no other
// implementation will ever reproduce. So nothing here rests on a call having succeeded.
// Every claim about a derived byte is pinned to a value RFC 9180 published, read out of
// the corpus hpke_vectors_test.go vendors, and the round trips and length checks are only
// the frame around that.
//
// Both registered suites are carried because the collision named in hpke.go's file
// comment is invisible on one of them: 0x0001 is simultaneously the HKDF-SHA256 kdf and
// the AES-128-GCM aead, so on suite 0x0001 the two positions hold the same byte and a
// transposition changes nothing anyone could observe. On 0x0003 the aead is 0x0003 and
// the same transposition moves every derived byte in the vector. A single suite corpus
// would therefore be a corpus that cannot see the mistake this file exists to catch, which
// is why loadHpkeVectors refuses to return anything but one entry per registered suite.
//
// The vectors now run the whole of base mode. RFC 9180 publishes the key schedule
// inputs and outputs — shared_secret, key_schedule_context, secret, key, base_nonce,
// exporter_secret — and each of those is a labelled extract or expand away from the
// previous one, so the chain up to the aead is reachable with the four labelled kdf
// functions and no others; the kem key pairs, the encapsulation and the decapsulation are
// published beside them, and past those the corpus carries the sealed message at every
// sequence number from 0 to 256 and three exported values, which is what pins the aead,
// the nonce counter and the secret export.
//
// The values reach this file through loadHpkeVectors and nowhere else. They were
// transcribed inline while tasks 5 to 8 were being written, because there was no corpus to
// check against until this one was vendored; the transcriptions are gone, and the commit
// that removed them asserted first that the vendored file agreed with them on all 94
// published values the two shared. Two corpora that are meant to agree, with nothing
// asserting that they do, is how they drift.
//
// Nothing about DHKEM is reachable by round trip. Encap and Decap that are wrong in the
// same way agree with each other perfectly: transpose enc and pkRm in the kem context on
// both sides, write the ephemeral key into the recipient's slot on both sides, respell
// eae_prk, or return the raw diffie-hellman output in place of the extract-and-expand, and
// every one of those still produces a 32 byte secret both parties reach. Each was applied
// to hpke.go and left the round trip, the length checks and the determinism checks green.
// So the encap and decap tests below assert against published bytes, and the round trip is
// only there to say the two entry points are not separately correct against nothing.
//
// There is no general hex helper declared here. This package's one decoder is the interop
// harness's MustHex, which has not landed, so the two below are the package's own and are
// written to be replaced by it rather than to compete with it: they take the vector and
// field names, so a bad value fails as a named field rather than as an empty slice.
package mls

import (
	"bytes"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"testing"
)

// One field of the vendored corpus, with a bad value fatal rather than an empty slice: an
// odd length or a stray character would otherwise silently turn a known answer into a
// comparison against nothing, which is the one way a corpus like this passes while
// asserting less than it says.
func decodeVectorField(t testing.TB, vectorName string, fieldName string, value string) []byte {
	t.Helper()
	decoded, err := hex.DecodeString(value)
	if err != nil {
		t.Fatalf("%s: %s is not hex: %v", vectorName, fieldName, err)
	}
	if len(decoded) == 0 {
		t.Fatalf("%s: %s decoded to nothing", vectorName, fieldName)
	}
	return decoded
}

// One field that is allowed to decode to nothing, for the exporter context the corpus
// carries as an empty string. decodeVectorField refuses an empty
// decode because an empty expected value is a comparison against nothing; an empty input
// is the opposite case and is published, and it is the only one that can see an exporter
// substituting a default of its own for a caller that supplied none.
func decodePossiblyEmptyVectorField(t testing.TB, vectorName string, fieldName string, value string) []byte {
	t.Helper()
	if value == "" {
		return nil
	}
	return decodeVectorField(t, vectorName, fieldName, value)
}

// TestHpkeSuiteIds pins the two identifiers byte for byte against RFC 9180 section 5.1,
// written out as literals rather than rebuilt from the registry so this test disagrees
// with hpke.go when hpke.go changes. The chacha row is the load bearing one: it is the
// only place a kdf and aead transposition is visible, since the aes suite holds 0x0001
// in both positions.
func TestHpkeSuiteIds(t *testing.T) {
	chacha, err := LookupSuite(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("LookupSuite: %v", err)
	}
	if got, want := hpkeKemSuiteId(chacha), append([]byte("KEM"), 0x00, 0x20); !bytes.Equal(got, want) {
		t.Errorf("kem suite id = %x, want %x", got, want)
	}
	if got, want := hpkeSuiteId(chacha), append([]byte("HPKE"), 0x00, 0x20, 0x00, 0x01, 0x00, 0x03); !bytes.Equal(got, want) {
		t.Errorf("hpke suite id = %x, want %x", got, want)
	}

	aes, err := LookupSuite(CipherSuiteX25519AesGcm128Sha256Ed25519)
	if err != nil {
		t.Fatalf("LookupSuite: %v", err)
	}
	if got, want := hpkeSuiteId(aes), append([]byte("HPKE"), 0x00, 0x20, 0x00, 0x01, 0x00, 0x01); !bytes.Equal(got, want) {
		t.Errorf("aes hpke suite id = %x, want %x", got, want)
	}
}

// TestHpkeKemSuiteIdDerivesThePublishedSharedSecret is the kem suite id's only real
// exercise: the literal comparison above says what the bytes are, and this says they are
// the bytes RFC 9180 actually derived with. The chain is the whole of DHKEM Encap after
// the diffie-hellman — extract to eae_prk, expand to the shared secret over enc || pkRm —
// so a wrong prefix, a wrong label, a swapped salt or a missing length prefix all land on
// a shared secret the RFC never printed.
//
// The diffie-hellman itself comes from X25519DH, which crypto_x25519_test.go already pins
// against RFC 7748, so a failure here is a kdf failure rather than a curve one.
func TestHpkeKemSuiteIdDerivesThePublishedSharedSecret(t *testing.T) {
	for _, vector := range loadHpkeVectors(t) {
		params, err := LookupSuite(vector.suite)
		if err != nil {
			t.Fatalf("%s: LookupSuite: %v", vector.name, err)
		}
		priv, err := X25519PrivateKey(decodeVectorField(t, vector.name, "skEm", vector.SkEm))
		if err != nil {
			t.Fatalf("%s: skEm: %v", vector.name, err)
		}
		pkRm := decodeVectorField(t, vector.name, "pkRm", vector.PkRm)
		pub, err := X25519PublicKey(pkRm)
		if err != nil {
			t.Fatalf("%s: pkRm: %v", vector.name, err)
		}
		dh, err := X25519DH(priv, pub)
		if err != nil {
			t.Fatalf("%s: dh: %v", vector.name, err)
		}
		kemSuiteId := hpkeKemSuiteId(params)
		kemContext := append(decodeVectorField(t, vector.name, "enc", vector.Enc), pkRm...)
		eaePrk := hpkeLabeledExtract(kemSuiteId, nil, "eae_prk", dh)
		got, err := hpkeLabeledExpand(kemSuiteId, eaePrk, "shared_secret", kemContext, params.Nsecret)
		if err != nil {
			t.Fatalf("%s: shared_secret: %v", vector.name, err)
		}
		want := decodeVectorField(t, vector.name, "shared_secret", vector.SharedSecret)
		if !bytes.Equal(got, want) {
			t.Errorf("%s: shared_secret = %x, want %x", vector.name, got, want)
		}
	}
}

// TestHpkeLabeledExtractKat pins the extract half against the two published values it can
// reach on its own. The key schedule context is the concatenation of two extracts that
// differ only in their label and their ikm, so a label that was dropped or a version
// prefix that moved collapses them onto each other; and the secret is an extract whose
// salt is the 32 byte shared secret and whose ikm is empty, which is the one shape where
// swapping crypto/hkdf's two arguments cannot be mistaken for anything else.
func TestHpkeLabeledExtractKat(t *testing.T) {
	for _, vector := range loadHpkeVectors(t) {
		params, err := LookupSuite(vector.suite)
		if err != nil {
			t.Fatalf("%s: LookupSuite: %v", vector.name, err)
		}
		suiteId := hpkeSuiteId(params)
		pskIdHash := hpkeLabeledExtract(suiteId, nil, "psk_id_hash", nil)
		infoHash := hpkeLabeledExtract(suiteId, nil, "info_hash", decodeVectorField(t, vector.name, "info", vector.Info))
		context := append([]byte{hpkeModeBase}, pskIdHash...)
		context = append(context, infoHash...)
		wantContext := decodeVectorField(t, vector.name, "key_schedule_context", vector.KeyScheduleContext)
		if !bytes.Equal(context, wantContext) {
			t.Errorf("%s: key_schedule_context = %x, want %x", vector.name, context, wantContext)
		}

		sharedSecret := decodeVectorField(t, vector.name, "shared_secret", vector.SharedSecret)
		secret := hpkeLabeledExtract(suiteId, sharedSecret, "secret", nil)
		wantSecret := decodeVectorField(t, vector.name, "secret", vector.Secret)
		if !bytes.Equal(secret, wantSecret) {
			t.Errorf("%s: secret = %x, want %x", vector.name, secret, wantSecret)
		}
	}
}

// TestHpkeLabeledExpandKat pins the expand half the same way, from the published secret
// and key schedule context rather than from the ones derived above, so a break in the
// extract half cannot travel here and be reported twice.
//
// The three outputs are 16 or 32, 12 and 32 bytes, and the length is both an argument and
// the first two bytes of the preimage. That is what makes the two byte prefix testable at
// all: an implementation that omitted it still returns the right number of bytes every
// time, and only differs in what those bytes are — differently for each of the three
// lengths here, and differently again between the two suites, whose keys are 16 and 32
// bytes from the same construction.
func TestHpkeLabeledExpandKat(t *testing.T) {
	for _, vector := range loadHpkeVectors(t) {
		params, err := LookupSuite(vector.suite)
		if err != nil {
			t.Fatalf("%s: LookupSuite: %v", vector.name, err)
		}
		suiteId := hpkeSuiteId(params)
		secret := decodeVectorField(t, vector.name, "secret", vector.Secret)
		context := decodeVectorField(t, vector.name, "key_schedule_context", vector.KeyScheduleContext)
		expansions := []struct {
			field  string
			label  string
			length int
			want   string
		}{
			{field: "key", label: "key", length: params.Nk, want: vector.Key},
			{field: "base_nonce", label: "base_nonce", length: params.Nn, want: vector.BaseNonce},
			{field: "exporter_secret", label: "exp", length: params.Nh, want: vector.ExporterSecret},
		}
		for _, expansion := range expansions {
			got, err := hpkeLabeledExpand(suiteId, secret, expansion.label, context, expansion.length)
			if err != nil {
				t.Fatalf("%s: %s: %v", vector.name, expansion.field, err)
			}
			want := decodeVectorField(t, vector.name, expansion.field, expansion.want)
			if len(want) != expansion.length {
				t.Fatalf("%s: %s is %d published bytes, the suite says %d", vector.name, expansion.field, len(want), expansion.length)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s: %s = %x, want %x", vector.name, expansion.field, got, want)
			}
		}
	}
}

// TestHpkeLabeledExtractTreatsNilAndEmptySaltAlike states the equivalence RFC 9180 relies
// on when it writes LabeledExtract("", ...). crypto/hkdf substitutes a zero filled block
// for a nil salt and HMAC zero pads an empty key to the same block, so the two agree —
// but that is the standard library's behaviour rather than this package's, and a wrapper
// that substituted a default salt of its own for nil would break it silently.
func TestHpkeLabeledExtractTreatsNilAndEmptySaltAlike(t *testing.T) {
	params, err := LookupSuite(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("LookupSuite: %v", err)
	}
	suiteId := hpkeKemSuiteId(params)
	chacha := loadHpkeVectors(t)[1]
	ikm := decodeVectorField(t, chacha.name, "shared_secret", chacha.SharedSecret)
	fromNil := hpkeLabeledExtract(suiteId, nil, "eae_prk", ikm)
	fromEmpty := hpkeLabeledExtract(suiteId, []byte{}, "eae_prk", ikm)
	if !bytes.Equal(fromNil, fromEmpty) {
		t.Fatalf("nil and empty salt disagree: %x vs %x", fromNil, fromEmpty)
	}
	if len(fromNil) != hpkeKdfNh {
		t.Fatalf("prk is %d bytes, want %d", len(fromNil), hpkeKdfNh)
	}
}

// TestHpkeLabeledExpandRejectsUnrepresentableLengths covers the lengths that must not
// produce a key. Over 255*Nh there is nothing HKDF-Expand can return. Below zero
// crypto/hkdf.Expand does not return anything at all: its fips140 body opens with
// out := make([]byte, 0, keyLen) and a negative cap is "makeslice: cap out of range", so
// the refusal asserted here is what stands between a caller supplied length and a dead
// process — task 8's Export takes that length from a caller.
//
// The two lengths past 65535 are subsumed by the 8160 ceiling as the constants stand
// today; they are carried against a suite whose Nh would lift the ceiling past what the
// two byte I2OSP prefix can encode, which is the only world where they assert something
// the ceiling does not. The accepting case is asserted beside all of them, because a
// function hardwired to refuse everything satisfies the refusals on its own.
func TestHpkeLabeledExpandRejectsUnrepresentableLengths(t *testing.T) {
	params, err := LookupSuite(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("LookupSuite: %v", err)
	}
	suiteId := hpkeSuiteId(params)
	prk := make([]byte, hpkeKdfNh)
	for _, length := range []int{-1, hpkeMaxExpandLength + 1, 1 << 16, 1<<16 + params.Nk} {
		out, err := hpkeLabeledExpand(suiteId, prk, "key", nil, length)
		if !errors.Is(err, ErrBadKeyLength) {
			t.Errorf("expand of %d bytes returned error %v, want ErrBadKeyLength", length, err)
		}
		if out != nil {
			t.Errorf("expand of %d bytes refused and returned %d bytes anyway", length, len(out))
		}
	}
	for _, length := range []int{0, 1, params.Nn, params.Nk, hpkeMaxExpandLength} {
		out, err := hpkeLabeledExpand(suiteId, prk, "key", nil, length)
		if err != nil {
			t.Fatalf("expand rejected %d bytes: %v", length, err)
		}
		if len(out) != length {
			t.Fatalf("expand of %d bytes returned %d", length, len(out))
		}
	}
}

// TestHpkeExpandCeilingIsTheLibrarysOwnBoundary pins hpkeMaxExpandLength to something
// other than itself. Every assertion about the ceiling in the refusal test above is
// written as hpkeMaxExpandLength or hpkeMaxExpandLength+1, so together they say only
// "the guard fires just above wherever the constant sits" — which is true of any value
// the constant could hold, and measurably so: setting it to 1*hpkeKdfNh, a 255 fold
// under estimate, leaves the whole package green, as does 254*hpkeKdfNh. Only 256*
// failed, and only because crypto/hkdf enforces its own limit.
//
// So the constant is asserted here against the library it exists to describe rather than
// against the guard that reads it. RFC 5869 section 2.3 stops the expansion counter at
// 255 and crypto/hkdf implements exactly that, so 255*Nh is the largest length the kdf
// will serve and 255*Nh+1 is the smallest it refuses. A ceiling that drifted tighter
// fails on the reject half, because the kdf still serves that length; one that drifted
// looser fails on the accept half, because the kdf will not serve it.
//
// Calling hkdf here is allowed and was checked against crypto_forbidden_test.go rather
// than assumed: the confinement gate's needle is hkdf.Extract( and not hkdf.Expand(, and
// it runs over productionSources, which drops every _test.go file.
func TestHpkeExpandCeilingIsTheLibrarysOwnBoundary(t *testing.T) {
	prk := make([]byte, hpkeKdfNh)
	out, err := hkdf.Expand(sha256.New, prk, "ceiling", hpkeMaxExpandLength)
	if err != nil {
		t.Errorf("the kdf refused %d bytes, so the ceiling sits above what it will serve: %v", hpkeMaxExpandLength, err)
	} else if len(out) != hpkeMaxExpandLength {
		t.Errorf("the kdf returned %d bytes for a request of %d", len(out), hpkeMaxExpandLength)
	}
	if _, err := hkdf.Expand(sha256.New, prk, "ceiling", hpkeMaxExpandLength+1); err == nil {
		t.Errorf("the kdf served %d bytes, so the ceiling sits below what it will serve", hpkeMaxExpandLength+1)
	}
}

// TestHpkeExpandCeilingMatchesTheRegistry keeps the constant the guard above is written
// against from drifting away from the suites it guards. hpkeMaxExpandLength is 255 times
// the kdf output size, and it is stated once for HMAC-SHA256 rather than read per suite;
// a third suite whose Nh was not 32 would make it wrong for that suite without changing a
// line of hpke.go.
//
// Both halves are needed and neither implies the other. The Nh check catches a suite that
// arrived with a different kdf output; the ceiling check catches the constant being
// restated as anything but 255 times it — which is what this test was named for and did
// not do, since its body mentioned only Nh.
func TestHpkeExpandCeilingMatchesTheRegistry(t *testing.T) {
	suites := Suites()
	if len(suites) == 0 {
		t.Fatal("the registry is empty, so the loop below asserts nothing")
	}
	for _, suite := range suites {
		params, err := LookupSuite(suite)
		if err != nil {
			t.Fatalf("LookupSuite %#04x: %v", uint16(suite), err)
		}
		if params.Nh != hpkeKdfNh {
			t.Errorf("suite %#04x has Nh %d, the expand ceiling assumes %d", uint16(suite), params.Nh, hpkeKdfNh)
		}
		if hpkeMaxExpandLength != 255*params.Nh {
			t.Errorf("suite %#04x has Nh %d, so its expand ceiling is %d, but the constant is %d", uint16(suite), params.Nh, 255*params.Nh, hpkeMaxExpandLength)
		}
	}
}

// TestHpkeDeriveKeyPairMatchesThePublishedKeyPairs is the known answer for DeriveKeyPair.
// Determinism, the sizes and the
// sensitivity to the ikm are all satisfied by any deterministic function of the input, so
// none of them can tell the RFC's derivation from a plausible neighbour: dropping the
// dkp_prk label, respelling it, expanding under "key" instead of "sk", or deriving under
// the whole suite id instead of the kem one each leave a well formed 32 byte scalar and a
// public key on the curve. Only the published pair separates them.
//
// Both key pairs of each vector are derived, because they exercise different halves of
// what follows: skEm is the input to encapsulation and skRm to decapsulation, and a
// derivation that was right for one and wrong for the other is exactly the shape a single
// entry would miss. The ephemeral public key is checked against the corpus's own pkEm
// rather than against its enc, and TestHpkeVectorEncapsulatedKeyIsTheEphemeralPublicKey
// separately asserts the two are the same bytes — which is what RFC 9180 section 4.1 means
// by defining the encapsulated key of a DHKEM as SerializePublicKey(pkE). Split that way,
// a corpus whose two fields disagreed fails as a disagreement about the corpus instead of
// as a derivation failure that blames this function.
func TestHpkeDeriveKeyPairMatchesThePublishedKeyPairs(t *testing.T) {
	for _, vector := range loadHpkeVectors(t) {
		params, err := LookupSuite(vector.suite)
		if err != nil {
			t.Fatalf("%s: LookupSuite: %v", vector.name, err)
		}
		derivations := []struct {
			role       string
			ikm        string
			privateKey string
			publicKey  string
		}{
			{role: "ephemeral", ikm: vector.IkmE, privateKey: vector.SkEm, publicKey: vector.Enc},
			{role: "recipient", ikm: vector.IkmR, privateKey: vector.SkRm, publicKey: vector.PkRm},
		}
		for _, derivation := range derivations {
			ikm := decodeVectorField(t, vector.name, derivation.role+" ikm", derivation.ikm)
			priv, pub, err := HpkeDeriveKeyPair(params, ikm)
			if err != nil {
				t.Fatalf("%s: %s: HpkeDeriveKeyPair: %v", vector.name, derivation.role, err)
			}
			wantPriv := decodeVectorField(t, vector.name, derivation.role+" private key", derivation.privateKey)
			if !bytes.Equal(priv, wantPriv) {
				t.Errorf("%s: %s private key = %x, want %x", vector.name, derivation.role, priv, wantPriv)
			}
			wantPub := decodeVectorField(t, vector.name, derivation.role+" public key", derivation.publicKey)
			if !bytes.Equal(pub, wantPub) {
				t.Errorf("%s: %s public key = %x, want %x", vector.name, derivation.role, pub, wantPub)
			}
		}
	}
}

// TestHpkeDeriveKeyPairIsDeterministic states the properties the known answer above cannot
// reach on its own: that a second call on the same ikm returns the same pair rather than
// reading entropy somewhere, that the lengths are the ones the suite fixes, and that the
// ikm is actually consumed. On its own this test is satisfied by any deterministic
// function, which is why it is written beside the vectors rather than instead of them.
func TestHpkeDeriveKeyPairIsDeterministic(t *testing.T) {
	params, err := LookupSuite(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("LookupSuite: %v", err)
	}
	ikm := bytes.Repeat([]byte{0x42}, 32)
	priv1, pub1, err := HpkeDeriveKeyPair(params, ikm)
	if err != nil {
		t.Fatalf("derive 1: %v", err)
	}
	priv2, pub2, err := HpkeDeriveKeyPair(params, ikm)
	if err != nil {
		t.Fatalf("derive 2: %v", err)
	}
	if !bytes.Equal(priv1, priv2) || !bytes.Equal(pub1, pub2) {
		t.Fatalf("derive is not deterministic")
	}
	if len(priv1) != params.Nsk || len(pub1) != params.Npk {
		t.Fatalf("sizes are %d/%d, want %d/%d", len(priv1), len(pub1), params.Nsk, params.Npk)
	}
	_, pub3, err := HpkeDeriveKeyPair(params, bytes.Repeat([]byte{0x43}, 32))
	if err != nil {
		t.Fatalf("derive 3: %v", err)
	}
	if bytes.Equal(pub1, pub3) {
		t.Fatalf("different ikm produced the same public key")
	}
}

// TestHpkeEncapMatchesThePublishedEncapsulation is the assertion this task turns on. Both
// entry points are driven from the vector's own ephemeral scalar and both have to produce
// the shared secret and the enc RFC 9180 printed.
//
// The randomized one is included rather than assumed equivalent because the plan's own test
// for that only checked the deterministic one against a key pair it derived itself, which
// is true of any pair of functions that return their argument. Driving hpkeEncap from a
// reader scripted with skEm reaches the same published bytes through the code path
// production actually uses, and the equality check afterwards is then a statement about two
// things that were each separately pinned.
//
// What this catches that the round trip cannot: kem_context transposed to pkRm concatenated
// with enc, the recipient's key replaced by the ephemeral one in the second slot, eae_prk or
// shared_secret respelled or dropped, the extract-and-expand skipped so the raw
// diffie-hellman output is returned, and the kem suite id swapped for the whole suite id.
// All six were applied to hpke.go and all six survive every non vector test in this file.
func TestHpkeEncapMatchesThePublishedEncapsulation(t *testing.T) {
	for _, vector := range loadHpkeVectors(t) {
		params, err := LookupSuite(vector.suite)
		if err != nil {
			t.Fatalf("%s: LookupSuite: %v", vector.name, err)
		}
		skEm := decodeVectorField(t, vector.name, "skEm", vector.SkEm)
		pkRm := HpkePublicKey(decodeVectorField(t, vector.name, "pkRm", vector.PkRm))
		wantSecret := decodeVectorField(t, vector.name, "shared_secret", vector.SharedSecret)
		wantEnc := decodeVectorField(t, vector.name, "enc", vector.Enc)

		secret, enc, err := hpkeEncapDeterministic(params, pkRm, HpkePrivateKey(skEm))
		if err != nil {
			t.Fatalf("%s: hpkeEncapDeterministic: %v", vector.name, err)
		}
		if !bytes.Equal(secret, wantSecret) {
			t.Errorf("%s: deterministic shared_secret = %x, want %x", vector.name, secret, wantSecret)
		}
		if !bytes.Equal(enc, wantEnc) {
			t.Errorf("%s: deterministic enc = %x, want %x", vector.name, enc, wantEnc)
		}

		// the same encapsulation through the randomized entry point, whose ephemeral
		// scalar is whatever its reader supplies — which is what makes the vector
		// reachable from the function production calls
		randomizedSecret, randomizedEnc, err := hpkeEncap(bytes.NewReader(skEm), params, pkRm)
		if err != nil {
			t.Fatalf("%s: hpkeEncap: %v", vector.name, err)
		}
		if !bytes.Equal(randomizedSecret, wantSecret) {
			t.Errorf("%s: randomized shared_secret = %x, want %x", vector.name, randomizedSecret, wantSecret)
		}
		if !bytes.Equal(randomizedEnc, wantEnc) {
			t.Errorf("%s: randomized enc = %x, want %x", vector.name, randomizedEnc, wantEnc)
		}
	}
}

// TestHpkeDecapMatchesThePublishedSharedSecret is the other side, and it is not implied by
// the encap vectors. Decap never sees pkRm on the wire — it recomputes that half of the kem
// context from the private key it holds — so it is a second implementation of the same
// concatenation, with its own opportunity to put its own key first, and the round trip
// cannot see the difference because it would agree with an encap transposed to match.
func TestHpkeDecapMatchesThePublishedSharedSecret(t *testing.T) {
	for _, vector := range loadHpkeVectors(t) {
		params, err := LookupSuite(vector.suite)
		if err != nil {
			t.Fatalf("%s: LookupSuite: %v", vector.name, err)
		}
		skRm := HpkePrivateKey(decodeVectorField(t, vector.name, "skRm", vector.SkRm))
		enc := decodeVectorField(t, vector.name, "enc", vector.Enc)
		secret, err := hpkeDecap(params, skRm, enc)
		if err != nil {
			t.Fatalf("%s: hpkeDecap: %v", vector.name, err)
		}
		want := decodeVectorField(t, vector.name, "shared_secret", vector.SharedSecret)
		if !bytes.Equal(secret, want) {
			t.Errorf("%s: shared_secret = %x, want %x", vector.name, secret, want)
		}
	}
}

// TestHpkeEncapDecapAgree is the frame around the vectors rather than an assertion in its
// own right: it says the two entry points are the same computation over a key pair neither
// vector supplies, across both registered suites and with a real entropy source. Every
// mutation listed in this file's comment passes it. What it does add is the sensitivity
// check at the end — a decapsulation under a different private key has to reach different
// bytes, which is the one thing here that a constant returning kem would fail.
func TestHpkeEncapDecapAgree(t *testing.T) {
	suites := Suites()
	if len(suites) == 0 {
		t.Fatal("the registry is empty, so the loop below asserts nothing")
	}
	for _, suite := range suites {
		params, err := LookupSuite(suite)
		if err != nil {
			t.Fatalf("LookupSuite %#04x: %v", uint16(suite), err)
		}
		priv, pub, err := HpkeDeriveKeyPair(params, bytes.Repeat([]byte{0x01}, 32))
		if err != nil {
			t.Fatalf("derive: %v", err)
		}
		sharedSecret, kemOutput, err := hpkeEncap(rand.Reader, params, pub)
		if err != nil {
			t.Fatalf("encap: %v", err)
		}
		if len(kemOutput) != params.Nenc {
			t.Fatalf("kem output is %d bytes, want %d", len(kemOutput), params.Nenc)
		}
		if len(sharedSecret) != params.Nsecret {
			t.Fatalf("shared secret is %d bytes, want %d", len(sharedSecret), params.Nsecret)
		}
		back, err := hpkeDecap(params, priv, kemOutput)
		if err != nil {
			t.Fatalf("decap: %v", err)
		}
		if !bytes.Equal(sharedSecret, back) {
			t.Fatalf("suite %#04x: encap and decap disagree", uint16(suite))
		}
		strangerPriv, _, err := HpkeDeriveKeyPair(params, bytes.Repeat([]byte{0x02}, 32))
		if err != nil {
			t.Fatalf("derive a stranger: %v", err)
		}
		// DHKEM does not authenticate the recipient, so the wrong key is not an error;
		// it is a different secret, and a kem that ignored its inputs would be caught
		// here and nowhere else in this test
		stranger, err := hpkeDecap(params, strangerPriv, kemOutput)
		if err != nil {
			t.Fatalf("decap under a stranger's key: %v", err)
		}
		if bytes.Equal(sharedSecret, stranger) {
			t.Fatalf("suite %#04x: a different private key reached the same secret %x", uint16(suite), stranger)
		}
	}
}

// TestHpkeEncapDeterministicMatchesEncap states the equivalence the vector gate depends on,
// over an ephemeral key that is not a published one: task 9 drives encapsulation through
// hpkeEncapDeterministic, so that entry point has to be the computation hpkeEncap performs
// rather than a second one written for the tests. The reader is scripted with the ephemeral
// scalar, which is the only way to hold the two sides to the same key.
//
// The plan's version of this test compared nothing: it checked that the deterministic
// encapsulation's kem output was the ephemeral public key and that the secret had the right
// length, and never called hpkeEncap at all, so the name was the whole of the claim.
func TestHpkeEncapDeterministicMatchesEncap(t *testing.T) {
	params, err := LookupSuite(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("LookupSuite: %v", err)
	}
	_, pub, err := HpkeDeriveKeyPair(params, bytes.Repeat([]byte{0x03}, 32))
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	ephemeralPriv, ephemeralPub, err := HpkeDeriveKeyPair(params, bytes.Repeat([]byte{0x04}, 32))
	if err != nil {
		t.Fatalf("derive ephemeral: %v", err)
	}
	sharedSecret, kemOutput, err := hpkeEncapDeterministic(params, pub, ephemeralPriv)
	if err != nil {
		t.Fatalf("encap deterministic: %v", err)
	}
	if !bytes.Equal(kemOutput, ephemeralPub) {
		t.Fatalf("kem output %x is not the ephemeral public key %x", kemOutput, ephemeralPub)
	}
	if len(sharedSecret) != params.Nsecret {
		t.Fatalf("shared secret is %d bytes, want %d", len(sharedSecret), params.Nsecret)
	}
	randomizedSecret, randomizedKemOutput, err := hpkeEncap(bytes.NewReader(ephemeralPriv), params, pub)
	if err != nil {
		t.Fatalf("encap from a scripted reader: %v", err)
	}
	if !bytes.Equal(randomizedSecret, sharedSecret) {
		t.Fatalf("the two entry points disagree on the secret: %x vs %x", randomizedSecret, sharedSecret)
	}
	if !bytes.Equal(randomizedKemOutput, kemOutput) {
		t.Fatalf("the two entry points disagree on the kem output: %x vs %x", randomizedKemOutput, kemOutput)
	}
}

// TestHpkeDecapRejectsWrongLengths covers the lengths that must not reach the curve. The
// two sentinels are distinguished on purpose: a kem output is a peer's bytes and a private
// key of the wrong length is this process's own bug, and a caller triaging the two reads
// the wrong answer if they collapse. The accepting case is asserted beside them because a
// function hardwired to refuse everything satisfies every refusal on its own.
//
// The case where both lengths are wrong is asserted too, because the two loops below leave
// it free: each holds one length right, so neither can see which check runs first. hpkeDecap's
// own comment states a precedence — the peer's bytes are reported ahead of the local bug —
// and an order nothing pins is an order a later edit reverses without a failing test.
func TestHpkeDecapRejectsWrongLengths(t *testing.T) {
	params, err := LookupSuite(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("LookupSuite: %v", err)
	}
	priv, _, err := HpkeDeriveKeyPair(params, bytes.Repeat([]byte{0x02}, 32))
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	for _, n := range []int{0, 31, 33, 1024} {
		secret, err := hpkeDecap(params, priv, make([]byte, n))
		if !errors.Is(err, ErrBadKemOutput) {
			t.Errorf("decap(%d bytes) error = %v, want ErrBadKemOutput", n, err)
		}
		if secret != nil {
			t.Errorf("decap(%d bytes) refused and returned %d bytes anyway", n, len(secret))
		}
	}
	for _, n := range []int{0, 31, 33, 64} {
		secret, err := hpkeDecap(params, make(HpkePrivateKey, n), make([]byte, params.Nenc))
		if !errors.Is(err, ErrBadKeyLength) {
			t.Errorf("decap with a %d byte private key error = %v, want ErrBadKeyLength", n, err)
		}
		if secret != nil {
			t.Errorf("decap with a %d byte private key refused and returned %d bytes anyway", n, len(secret))
		}
	}
	bothWrong, err := hpkeDecap(params, make(HpkePrivateKey, 31), make([]byte, 31))
	if !errors.Is(err, ErrBadKemOutput) {
		t.Errorf("decap with both lengths wrong error = %v, want ErrBadKemOutput", err)
	}
	if bothWrong != nil {
		t.Errorf("decap with both lengths wrong refused and returned %d bytes anyway", len(bothWrong))
	}
	_, kemOutput, err := hpkeEncap(rand.Reader, params, mustDeriveHpkePublicKey(t, params, 0x02))
	if err != nil {
		t.Fatalf("encap: %v", err)
	}
	if _, err := hpkeDecap(params, priv, kemOutput); err != nil {
		t.Fatalf("decap refused a well formed kem output: %v", err)
	}
}

// TestHpkeEncapRejectsWrongLengths is the same statement on the other side, and it needs
// its own test because the two length checks there guard different arguments: a recipient
// key off the wire and an ephemeral scalar the caller chose.
func TestHpkeEncapRejectsWrongLengths(t *testing.T) {
	params, err := LookupSuite(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("LookupSuite: %v", err)
	}
	pub := mustDeriveHpkePublicKey(t, params, 0x05)
	ephemeral := HpkePrivateKey(bytes.Repeat([]byte{0x06}, params.Nsk))
	for _, n := range []int{0, 31, 33, 64} {
		secret, enc, err := hpkeEncapDeterministic(params, make(HpkePublicKey, n), ephemeral)
		if !errors.Is(err, ErrBadKeyLength) {
			t.Errorf("encap to a %d byte public key error = %v, want ErrBadKeyLength", n, err)
		}
		if secret != nil || enc != nil {
			t.Errorf("encap to a %d byte public key refused and returned %d/%d bytes anyway", n, len(secret), len(enc))
		}
		secret, enc, err = hpkeEncapDeterministic(params, pub, make(HpkePrivateKey, n))
		if !errors.Is(err, ErrBadKeyLength) {
			t.Errorf("encap from a %d byte ephemeral scalar error = %v, want ErrBadKeyLength", n, err)
		}
		if secret != nil || enc != nil {
			t.Errorf("encap from a %d byte ephemeral scalar refused and returned %d/%d bytes anyway", n, len(secret), len(enc))
		}
	}
	if _, _, err := hpkeEncapDeterministic(params, pub, ephemeral); err != nil {
		t.Fatalf("encap refused well formed keys: %v", err)
	}
}

// TestHpkeKemReadsTheFieldItNames is the assertion the two tests above cannot make. Every
// length they check comes from a registered suite, and the registry gives Nh, Nsecret,
// Nenc, Npk, Nsk, NsigPub and NsigPriv the same 32 in both of its entries — so a function
// that read a neighbouring field instead of the one RFC 9180 names would satisfy every
// vector, every round trip and every refusal in this file. Seven fields holding one value
// are seven names for the same assertion. Nk, Nn and Nt are not in that set: the registry
// gives them 16 or 32, 12 and 16, so a kem gate that read one of those is already refused
// by the known answers above and needs nothing here.
//
// Separating them does not need a registered suite, which is the reason this is testable
// at all. None of the four kem entry points consults the registry: each reads the
// *SuiteParams its caller hands it, and LookupSuite already returns a fresh copy rather
// than the registry's own entry. So the probes below are handed a SuiteParams whose length
// fields disagree, and such a value does not have to be registered, implementable or even
// coherent — nothing is derived from it but the length under test.
//
// The keys handed to each probe are well formed, so a probe that fails says the length
// gate did not fire rather than that some later gate fired in its place: a kem that drops
// or misdirects one of these checks reaches the curve and succeeds, and succeeding is what
// each assertion below denies.
//
// hpke.go reads a length in six places and all six are covered here — the Nsecret the
// shared secret expands to, the Nsk DeriveKeyPair expands to, and the four gates encap and
// decap open with. The two scalar gates need a probe of their own rather than riding on
// the recipient key one: under a suite that widens Npk, a scalar gate that wrongly read
// Npk refuses exactly what the right one refuses, so widening Nsk instead is what tells
// them apart.
func TestHpkeKemReadsTheFieldItNames(t *testing.T) {
	registered, err := LookupSuite(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("LookupSuite: %v", err)
	}
	priv, pub, err := HpkeDeriveKeyPair(registered, bytes.Repeat([]byte{0x0e}, 32))
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	ephemeral := HpkePrivateKey(bytes.Repeat([]byte{0x0f}, registered.Nsk))
	_, kemOutput, err := hpkeEncapDeterministic(registered, pub, ephemeral)
	if err != nil {
		t.Fatalf("encap: %v", err)
	}

	// the shared secret expands to Nsecret, not to any of the lengths beside it
	wideNsecret := hpkeFieldProbeParams(32)
	wideNsecret.Nsecret = 48
	secret, err := hpkeExtractAndExpand(wideNsecret, make([]byte, 32), make([]byte, 64))
	if err != nil {
		t.Fatalf("extract and expand under Nsecret 48: %v", err)
	}
	if len(secret) != 48 {
		t.Errorf("shared secret is %d bytes under a suite whose Nsecret is 48 and whose every other length is 32, want 48", len(secret))
	}

	// the derived scalar expands to Nsk, not to any of the lengths around it. This probe
	// runs the other way round — narrow where the others are wide — because a derivation
	// has no refusal to assert: what says the right field was read is that the key pair
	// came out usable, and only the right field is a length x25519 accepts.
	narrowNsk := hpkeFieldProbeParams(48)
	narrowNsk.Nsk = 32
	probePriv, probePub, err := HpkeDeriveKeyPair(narrowNsk, bytes.Repeat([]byte{0x11}, 32))
	if err != nil {
		t.Fatalf("derive under a suite whose only 32 byte length is Nsk: %v", err)
	}
	if len(probePriv) != 32 || len(probePub) != 32 {
		t.Errorf("derived %d/%d bytes under Nsk 32, want 32/32", len(probePriv), len(probePub))
	}

	// encap gates the recipient key on Npk, and gates it itself rather than leaving the
	// refusal to X25519PublicKey, whose own 32 byte length check would accept this key
	wideNpk := hpkeFieldProbeParams(32)
	wideNpk.Npk = 48
	if gotSecret, gotEnc, err := hpkeEncapDeterministic(wideNpk, pub, ephemeral); !errors.Is(err, ErrBadKeyLength) {
		t.Errorf("encap to a %d byte recipient key under Npk 48 returned %d/%d bytes and error %v, want ErrBadKeyLength",
			len(pub), len(gotSecret), len(gotEnc), err)
	}

	// both scalar gates read Nsk — encap's ephemeral scalar and decap's private key
	wideNsk := hpkeFieldProbeParams(32)
	wideNsk.Nsk = 48
	if gotSecret, gotEnc, err := hpkeEncapDeterministic(wideNsk, pub, ephemeral); !errors.Is(err, ErrBadKeyLength) {
		t.Errorf("encap from a %d byte ephemeral scalar under Nsk 48 returned %d/%d bytes and error %v, want ErrBadKeyLength",
			len(ephemeral), len(gotSecret), len(gotEnc), err)
	}
	if gotSecret, err := hpkeDecap(wideNsk, priv, kemOutput); !errors.Is(err, ErrBadKeyLength) {
		t.Errorf("decap with a %d byte private key under Nsk 48 returned %d bytes and error %v, want ErrBadKeyLength",
			len(priv), len(gotSecret), err)
	}

	// decap gates the kem output on Nenc, not on the Npk that holds the same 32
	wideNenc := hpkeFieldProbeParams(32)
	wideNenc.Nenc = 48
	if gotSecret, err := hpkeDecap(wideNenc, priv, kemOutput); !errors.Is(err, ErrBadKemOutput) {
		t.Errorf("decap of a %d byte kem output under Nenc 48 returned %d bytes and error %v, want ErrBadKemOutput",
			len(kemOutput), len(gotSecret), err)
	}
}

// The probe suite the test above builds on: a SuiteParams no registry holds, in which
// every length field carries the same background value. The caller then widens the single
// field it means to separate, so the field under test is the only one the probe can be
// answered by and every other length says the opposite.
//
// Nothing is passed per field. A helper taking the lengths one by one — positionally or by
// name — can be called with one left out, and a length left out is zero: a gate comparing
// against zero refuses every input, which is exactly what these probes expect to see and
// would make one pass while asserting nothing. Setting all of them from one argument is
// what makes that unrepresentable, and it is why NsigPub and NsigPriv are here at all
// despite no kem function naming them — they are 32 in both registered entries too, so a
// gate that read one would be invisible everywhere else in this file.
func hpkeFieldProbeParams(background int) *SuiteParams {
	return &SuiteParams{
		Suite:    CipherSuiteX25519ChaCha20Sha256Ed25519,
		KemId:    HpkeKemX25519HkdfSha256,
		KdfId:    HpkeKdfHkdfSha256,
		AeadId:   HpkeAeadChaCha20Poly1305,
		Nh:       background,
		Nk:       background,
		Nn:       background,
		Nt:       background,
		Nsecret:  background,
		Nenc:     background,
		Npk:      background,
		Nsk:      background,
		NsigPub:  background,
		NsigPriv: background,
	}
}

// TestHpkeRefusesALowOrderPeerKey carries guardrail 3's refusal through the kem. X25519DH
// reports a low order point as ErrInvalidPoint and crypto_x25519_test.go pins that; what is
// unproven until here is that the kem propagates it rather than deriving a shared secret
// from an all zero diffie-hellman output, which is what the whole guardrail exists to stop
// and which would look exactly like a working key exchange to both parties.
//
// Both directions are covered because the two functions reach the curve through different
// arguments: encap takes the peer key as a recipient public key, decap takes it as a kem
// output off the wire. Nothing else in the package covers either: with the kem swallowing
// X25519DH's error and deriving from an all zero output, in encap, in decap, or in both,
// this is the only test in mls that fails.
//
// The count is what keeps the loop honest, and it counts the same quantity
// crypto_x25519_test.go counts: points that reached the exchange, not points that were
// refused. Those are different, and only the first is a measure of coverage. crypto/ecdh
// accepts every 32 byte u coordinate today and refuses at the exchange, so all four points
// reach X25519DH; a parser that started refusing them at parse would satisfy every
// assertion in the body — ErrInvalidPoint is ErrInvalidPoint whichever gate returned it —
// and a counter of refusals would go on reporting four while the exchange refusal this
// test exists for went unexercised. Counting refusals here was measurably wrong, not
// theoretically: with a parse blacklist and the swallow applied together, the refusal
// counter left this test green while the kem derived keys from a zero shared secret.
func TestHpkeRefusesALowOrderPeerKey(t *testing.T) {
	params, err := LookupSuite(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("LookupSuite: %v", err)
	}
	priv, _, err := HpkeDeriveKeyPair(params, bytes.Repeat([]byte{0x08}, 32))
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	ephemeral := HpkePrivateKey(bytes.Repeat([]byte{0x09}, params.Nsk))
	reachedTheExchange := 0
	for i, point := range x25519LowOrderPoints {
		// the gate both entry points below parse this peer key through. Refusing here is
		// also not a zero secret and is acceptable, but it says nothing about the kem, so
		// the point is not counted and its assertions are not run.
		if _, err := X25519PublicKey(point); err != nil {
			if !errors.Is(err, ErrInvalidPoint) {
				t.Errorf("point %d: parse error = %v, want ErrInvalidPoint", i, err)
			}
			continue
		}
		reachedTheExchange++
		secret, enc, err := hpkeEncapDeterministic(params, HpkePublicKey(point), ephemeral)
		if !errors.Is(err, ErrInvalidPoint) {
			t.Errorf("point %d: encap error = %v, want ErrInvalidPoint", i, err)
			continue
		}
		if secret != nil || enc != nil {
			t.Errorf("point %d: encap refused and returned %d/%d bytes anyway", i, len(secret), len(enc))
			continue
		}
		secret, err = hpkeDecap(params, priv, point)
		if !errors.Is(err, ErrInvalidPoint) {
			t.Errorf("point %d: decap error = %v, want ErrInvalidPoint", i, err)
			continue
		}
		if secret != nil {
			t.Errorf("point %d: decap refused and returned %d bytes anyway", i, len(secret))
		}
	}
	if reachedTheExchange == 0 {
		t.Errorf("none of the %d low order points reached the exchange, so this test covered nothing", len(x25519LowOrderPoints))
	}
}

// TestHpkeEncapFailsWhenRandomFails is the key draw's own failure path. An encapsulation
// that answered a dead entropy source with an ephemeral key from somewhere else is worse
// than one that fails, because the peer would decapsulate it happily. The error has to be
// the reader's own rather than one of this package's sentinels, for the same reason
// X25519GenerateKey's is.
func TestHpkeEncapFailsWhenRandomFails(t *testing.T) {
	params, err := LookupSuite(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("LookupSuite: %v", err)
	}
	pub := mustDeriveHpkePublicKey(t, params, 0x0a)
	entropyIsDown := errors.New("entropy source is down")
	secret, enc, err := hpkeEncap(failingReader{err: entropyIsDown}, params, pub)
	if !errors.Is(err, entropyIsDown) {
		t.Errorf("error = %v, want the reader's own error", err)
	}
	if secret != nil || enc != nil {
		t.Errorf("a failing reader still produced %d/%d bytes", len(secret), len(enc))
	}
	secret, enc, err = hpkeEncap(&shortReader{remaining: params.Nsk - 1}, params, pub)
	if err == nil {
		t.Errorf("a short reader still produced an encapsulation")
	}
	if secret != nil || enc != nil {
		t.Errorf("a short reader still produced %d/%d bytes", len(secret), len(enc))
	}
}

// One key pair's public half from a constant ikm, so a test that needs a well formed
// recipient key and nothing else does not spell out the derivation and its error handling
// each time. It is deliberately not a vector key: the tests using it are about lengths,
// entropy and refusals, and reaching for a published key there would suggest the value
// mattered.
func mustDeriveHpkePublicKey(t *testing.T, params *SuiteParams, ikmByte byte) HpkePublicKey {
	t.Helper()
	_, pub, err := HpkeDeriveKeyPair(params, bytes.Repeat([]byte{ikmByte}, params.Nsk))
	if err != nil {
		t.Fatalf("HpkeDeriveKeyPair: %v", err)
	}
	return pub
}

func TestHpkeContextSequenceAdvances(t *testing.T) {
	// each Seal must use base_nonce XOR seq. a context that reused nonce zero would
	// still decrypt correctly under a matching receiver, so the only way to catch it
	// is to assert the ciphertexts differ for identical plaintext.
	params, err := LookupSuite(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("LookupSuite: %v", err)
	}
	sender, err := hpkeKeySchedule(params, bytes.Repeat([]byte{0x05}, 32), []byte("info"))
	if err != nil {
		t.Fatalf("key schedule: %v", err)
	}
	receiver, err := hpkeKeySchedule(params, bytes.Repeat([]byte{0x05}, 32), []byte("info"))
	if err != nil {
		t.Fatalf("key schedule: %v", err)
	}
	plaintext := []byte("the same plaintext every time")
	var previous []byte
	for i := 0; i < 4; i++ {
		ciphertext, err := sender.Seal([]byte("aad"), plaintext)
		if err != nil {
			t.Fatalf("seal %d: %v", i, err)
		}
		if bytes.Equal(ciphertext, previous) {
			t.Fatalf("seal %d repeated the previous ciphertext: the sequence did not advance", i)
		}
		previous = ciphertext
		back, err := receiver.Open([]byte("aad"), ciphertext)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		if !bytes.Equal(back, plaintext) {
			t.Fatalf("open %d returned %q", i, back)
		}
	}
}

func TestHpkeContextOpenRejectsTamper(t *testing.T) {
	params, err := LookupSuite(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("LookupSuite: %v", err)
	}
	sender, err := hpkeKeySchedule(params, bytes.Repeat([]byte{0x06}, 32), nil)
	if err != nil {
		t.Fatalf("key schedule: %v", err)
	}
	ciphertext, err := sender.Seal([]byte("aad"), []byte("plaintext"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	for i := range ciphertext {
		receiver, err := hpkeKeySchedule(params, bytes.Repeat([]byte{0x06}, 32), nil)
		if err != nil {
			t.Fatalf("key schedule: %v", err)
		}
		tampered := bytes.Clone(ciphertext)
		tampered[i] ^= 0x01
		if _, err := receiver.Open([]byte("aad"), tampered); !errors.Is(err, ErrAeadOpen) {
			t.Fatalf("flipping byte %d: error = %v, want ErrAeadOpen", i, err)
		}
	}
	receiver, err := hpkeKeySchedule(params, bytes.Repeat([]byte{0x06}, 32), nil)
	if err != nil {
		t.Fatalf("key schedule: %v", err)
	}
	if _, err := receiver.Open([]byte("different aad"), ciphertext); !errors.Is(err, ErrAeadOpen) {
		t.Fatalf("wrong aad: error = %v, want ErrAeadOpen", err)
	}

	// an empty aad is bound as tightly as a present one, and it is the shape MLS itself
	// uses: RFC 9420 section 5.1.2 has EncryptWithLabel call SealBase with no additional
	// data at all, so it is the aad every message this package goes on to carry will
	// have. An Open that retried with a nil aad when the tag failed would leave every
	// such message opening under any aad an attacker picked, and nothing above sees it —
	// the flip loop and the wrong aad case both seal under "aad", so the retry fails
	// there too. Measured: that Open survived the whole package before this block.
	unbound, err := hpkeKeySchedule(params, bytes.Repeat([]byte{0x06}, 32), nil)
	if err != nil {
		t.Fatalf("key schedule: %v", err)
	}
	sealedWithNoAad, err := unbound.Seal(nil, []byte("plaintext"))
	if err != nil {
		t.Fatalf("seal with no aad: %v", err)
	}
	receiver, err = hpkeKeySchedule(params, bytes.Repeat([]byte{0x06}, 32), nil)
	if err != nil {
		t.Fatalf("key schedule: %v", err)
	}
	if _, err := receiver.Open([]byte("an aad the sender never used"), sealedWithNoAad); !errors.Is(err, ErrAeadOpen) {
		t.Errorf("a message sealed with no aad opened under one: error = %v, want ErrAeadOpen", err)
	}
	// beside it, because a receiver that refused everything would satisfy the refusal
	receiver, err = hpkeKeySchedule(params, bytes.Repeat([]byte{0x06}, 32), nil)
	if err != nil {
		t.Fatalf("key schedule: %v", err)
	}
	if back, err := receiver.Open(nil, sealedWithNoAad); err != nil {
		t.Errorf("the aad the sender did use was refused: %v", err)
	} else if !bytes.Equal(back, []byte("plaintext")) {
		t.Errorf("the message sealed with no aad opened to %q", back)
	}
}

func TestHpkeContextExportIsLabelSeparated(t *testing.T) {
	params, err := LookupSuite(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("LookupSuite: %v", err)
	}
	ctx, err := hpkeKeySchedule(params, bytes.Repeat([]byte{0x07}, 32), nil)
	if err != nil {
		t.Fatalf("key schedule: %v", err)
	}
	a, err := ctx.Export([]byte("context a"), 32)
	if err != nil {
		t.Fatalf("export a: %v", err)
	}
	b, err := ctx.Export([]byte("context b"), 32)
	if err != nil {
		t.Fatalf("export b: %v", err)
	}
	if bytes.Equal(a, b) {
		t.Fatalf("different exporter contexts produced the same value")
	}
	// export must not consume a sequence number
	again, err := ctx.Export([]byte("context a"), 32)
	if err != nil {
		t.Fatalf("export a again: %v", err)
	}
	if !bytes.Equal(a, again) {
		t.Fatalf("export is not stable across calls")
	}
}

// TestHpkeAeadKeyLengthIsSuiteBound holds the aead key gate to the registry rather than
// to a constant, which is the whole of what separates a provider that reads Nk from one
// that hardcoded 32: every chacha test passes either way and only the aes entry disagrees.
//
// The refusals run both sides of the length. One byte over on its own leaves a gate
// written as an upper bound passing — len(key) > Nk refuses nk+1 and accepts every short
// key beneath it — and that was measured, not supposed.
func TestHpkeAeadKeyLengthIsSuiteBound(t *testing.T) {
	// neither registry entry can reach the constructor's default, so a version that fell
	// open there — handing an unregistered aead id a working chacha20-poly1305 instead
	// of refusing it — passes every row below. Measured: it did. The probe's Nk is 32
	// and the key is 32 bytes so the length gate passes and the switch is what answers.
	unknown := hpkeFieldProbeParams(32)
	unknown.AeadId = HpkeAeadId(0xffff)
	if aead, err := hpkeNewAead(unknown, make([]byte, 32)); !errors.Is(err, ErrUnknownCipherSuite) {
		t.Errorf("an unregistered aead id: error = %v, want ErrUnknownCipherSuite", err)
	} else if aead != nil {
		t.Errorf("the refused constructor returned an aead anyway")
	}

	// 0x0003 is a 32-byte key, 0x0001 is 16. a provider that hardcoded 32 would pass
	// every chacha test and silently fail on the aes suite.
	for _, testCase := range []struct {
		suite CipherSuite
		nk    int
	}{
		{suite: CipherSuiteX25519AesGcm128Sha256Ed25519, nk: 16},
		{suite: CipherSuiteX25519ChaCha20Sha256Ed25519, nk: 32},
	} {
		params, err := LookupSuite(testCase.suite)
		if err != nil {
			t.Fatalf("LookupSuite: %v", err)
		}
		if _, err := hpkeNewAead(params, make([]byte, testCase.nk)); err != nil {
			t.Errorf("suite %#04x rejected a %d-byte key: %v", uint16(testCase.suite), testCase.nk, err)
		}
		// 2*nk is not idle on the aes row: 32 bytes is a valid aes-256 key, so a gate
		// that let it through would build a working aead of a suite nobody asked for
		// rather than failing to build one at all.
		for _, wrong := range []int{0, testCase.nk - 1, testCase.nk + 1, 2 * testCase.nk} {
			if _, err := hpkeNewAead(params, make([]byte, wrong)); !errors.Is(err, ErrBadKeyLength) {
				t.Errorf("suite %#04x accepted a %d-byte key: error = %v, want ErrBadKeyLength", uint16(testCase.suite), wrong, err)
			}
		}
	}
}

// TestHpkeKeyScheduleContextMatchesThePublishedContext is the whole defence of the one
// field order in this file that produces a well formed answer when it is wrong. mode,
// psk_id_hash and info_hash concatenate to 65 bytes in that order, and the two hashes are
// the same 32 bytes out of the same kdf — so transposing them is a context of exactly the
// right length that the key, the base nonce and the exporter secret all follow
// consistently, and that a sender and a receiver transposed alike agree on byte for byte.
// The mode is the same shape: 0x01 in place of 0x00, or no mode byte at all, moves every
// derived value and breaks nothing a round trip can see. All three were applied to
// hpke.go and left the whole package green except this test.
//
// It asserts hpkeKeyScheduleContext rather than rebuilding the concatenation, and that is
// the difference between it and the assertion the labelled extract known answer makes
// above: that one builds the context in the test and compares it, so it holds the extract
// and nothing else, and it passes against a key schedule that assembled the same three
// pieces in any order at all.
func TestHpkeKeyScheduleContextMatchesThePublishedContext(t *testing.T) {
	for _, vector := range loadHpkeVectors(t) {
		params, err := LookupSuite(vector.suite)
		if err != nil {
			t.Fatalf("%s: LookupSuite: %v", vector.name, err)
		}
		info := decodeVectorField(t, vector.name, "info", vector.Info)
		got := hpkeKeyScheduleContext(hpkeSuiteId(params), info)
		want := decodeVectorField(t, vector.name, "key_schedule_context", vector.KeyScheduleContext)
		if !bytes.Equal(got, want) {
			t.Errorf("%s: key_schedule_context = %x, want %x", vector.name, got, want)
		}
	}
}

// TestHpkeKeyScheduleMatchesThePublishedSchedule drives the key schedule itself from the
// published shared secret and info and compares what the context kept. It is not the same
// assertion as the labelled expand known answer above, which expands from the published
// secret with the published context and so says nothing about how the key schedule
// arrived at either: extracting the secret with crypto/hkdf's two arguments the wrong way
// round, or building the context under the kem suite id, leaves that test green and this
// one red.
//
// The key is not a field of the context and is not compared here. It is pinned by the
// published ciphertexts in the test below, which is a stronger statement about it than
// any length would be: a key that is the right size and the wrong bytes seals nothing the
// RFC would recognise.
//
// Both registered suites, because the aes entry is where several of these become visible
// at all. Nk is 16 there and 32 here, so a key expanded to any of the seven suite fields
// that hold 32 is a key the aead refuses on the aes row and accepts silently on this one.
func TestHpkeKeyScheduleMatchesThePublishedSchedule(t *testing.T) {
	for _, vector := range loadHpkeVectors(t) {
		params, err := LookupSuite(vector.suite)
		if err != nil {
			t.Fatalf("%s: LookupSuite: %v", vector.name, err)
		}
		sharedSecret := decodeVectorField(t, vector.name, "shared_secret", vector.SharedSecret)
		info := decodeVectorField(t, vector.name, "info", vector.Info)
		ctx, err := hpkeKeySchedule(params, sharedSecret, info)
		if err != nil {
			t.Fatalf("%s: key schedule: %v", vector.name, err)
		}
		wantBaseNonce := decodeVectorField(t, vector.name, "base_nonce", vector.BaseNonce)
		if !bytes.Equal(ctx.baseNonce, wantBaseNonce) {
			t.Errorf("%s: base_nonce = %x, want %x", vector.name, ctx.baseNonce, wantBaseNonce)
		}
		wantExporterSecret := decodeVectorField(t, vector.name, "exporter_secret", vector.ExporterSecret)
		if !bytes.Equal(ctx.exporterSecret, wantExporterSecret) {
			t.Errorf("%s: exporter_secret = %x, want %x", vector.name, ctx.exporterSecret, wantExporterSecret)
		}
		if ctx.sequence != 0 {
			t.Errorf("%s: a fresh context is at sequence %d, want 0", vector.name, ctx.sequence)
		}
	}
}

// TestHpkeContextSealMatchesThePublishedEncryptions is where the aead, the nonce counter
// and the derived key are pinned, and it is the only test in this package that can see
// most of what the counter can get wrong. base_nonce xor I2OSP(seq, Nn) written little
// endian, written at the front of the nonce, added instead of xored, or advanced by two
// per message is in every case an implementation that agrees with itself, decrypts its
// own traffic, produces a different ciphertext for every message, and matches nobody.
// Each of those was applied to hpke.go and left the sequence, tamper and export tests
// green.
//
// The published sequence numbers are the RFC's own and they are the point. The corpus
// carries every one from 0 to 256: the low ones separate the counter from a constant, and
// 255 and 256 are where it crosses a byte, which is the only place a little endian counter
// and a big endian one disagree about which byte of the nonce to move. Reaching 256 is
// asserted rather than assumed, because a corpus re-vendored under a filter that truncated
// the encryptions would otherwise leave every nonce assertion here passing against an
// implementation that matches nobody.
//
// The context is walked to each published number with messages that are sealed and thrown
// away, exactly as a sender that had sent them would be. That walk is a no-op on this
// corpus, whose sequence numbers are consecutive, and it is kept because the appendix A
// excerpt these vectors used to be transcribed from is sparse and a later corpus may be
// too; the position is counted here rather than read off the context so that a context
// which never advances fails an assertion instead of spinning.
//
// The receiver opens the published ciphertext rather than the one the sender just
// produced, so the two directions are pinned against the RFC independently instead of
// against each other.
func TestHpkeContextSealMatchesThePublishedEncryptions(t *testing.T) {
	for _, vector := range loadHpkeVectors(t) {
		params, err := LookupSuite(vector.suite)
		if err != nil {
			t.Fatalf("%s: LookupSuite: %v", vector.name, err)
		}
		if last := vector.Encryptions[len(vector.Encryptions)-1]; last.sequence < hpkeVectorHighestSequence {
			t.Fatalf("%s stops at sequence %d, so the counter never crosses a byte", vector.name, last.sequence)
		}
		sharedSecret := decodeVectorField(t, vector.name, "shared_secret", vector.SharedSecret)
		info := decodeVectorField(t, vector.name, "info", vector.Info)
		sender, err := hpkeKeySchedule(params, sharedSecret, info)
		if err != nil {
			t.Fatalf("%s: sender key schedule: %v", vector.name, err)
		}
		receiver, err := hpkeKeySchedule(params, sharedSecret, info)
		if err != nil {
			t.Fatalf("%s: receiver key schedule: %v", vector.name, err)
		}
		position := uint64(0)
		for _, encryption := range vector.Encryptions {
			for position < encryption.sequence {
				filler, err := sender.Seal(nil, nil)
				if err != nil {
					t.Fatalf("%s: walking to sequence %d, seal at %d: %v", vector.name, encryption.sequence, position, err)
				}
				if _, err := receiver.Open(nil, filler); err != nil {
					t.Fatalf("%s: walking to sequence %d, open at %d: %v", vector.name, encryption.sequence, position, err)
				}
				position++
			}
			if sender.sequence != position || receiver.sequence != position {
				t.Fatalf("%s: after %d messages the sender is at sequence %d and the receiver at %d",
					vector.name, position, sender.sequence, receiver.sequence)
			}
			wantNonce := decodeVectorField(t, vector.name, "nonce", encryption.Nonce)
			if got := sender.nonce(); !bytes.Equal(got, wantNonce) {
				t.Errorf("%s: nonce at sequence %d = %x, want %x", vector.name, encryption.sequence, got, wantNonce)
			}
			aad := decodeVectorField(t, vector.name, "aad", encryption.Aad)
			plaintext := decodeVectorField(t, vector.name, "pt", encryption.Pt)
			wantCiphertext := decodeVectorField(t, vector.name, "ct", encryption.Ct)
			if len(wantCiphertext) != len(plaintext)+params.Nt {
				t.Fatalf("%s: the published ciphertext at sequence %d is %d bytes for %d of plaintext and a %d byte tag",
					vector.name, encryption.sequence, len(wantCiphertext), len(plaintext), params.Nt)
			}
			ciphertext, err := sender.Seal(aad, plaintext)
			if err != nil {
				t.Fatalf("%s: seal at sequence %d: %v", vector.name, encryption.sequence, err)
			}
			if !bytes.Equal(ciphertext, wantCiphertext) {
				t.Errorf("%s: ciphertext at sequence %d = %x, want %x", vector.name, encryption.sequence, ciphertext, wantCiphertext)
			}
			back, err := receiver.Open(aad, wantCiphertext)
			if err != nil {
				t.Fatalf("%s: open at sequence %d: %v", vector.name, encryption.sequence, err)
			}
			if !bytes.Equal(back, plaintext) {
				t.Errorf("%s: open at sequence %d returned %x, want %x", vector.name, encryption.sequence, back, plaintext)
			}
			position++
		}
	}
}

// TestHpkeContextExportMatchesThePublishedExports pins the exporter against the values
// the appendix prints. Nothing else in this package can: a respelled label, the base
// nonce in place of the exporter secret, or the kem suite id in place of the whole one
// each produce 32 well formed bytes that differ per exporter context and are stable
// across calls, which is everything the label separation test asks for. All three were
// applied to hpke.go and left it green.
//
// The empty exporter context is one of the three the RFC publishes and is carried for a
// reason of its own: it is the only input that can see an implementation substituting a
// default of its own for a caller that supplied none.
func TestHpkeContextExportMatchesThePublishedExports(t *testing.T) {
	for _, vector := range loadHpkeVectors(t) {
		params, err := LookupSuite(vector.suite)
		if err != nil {
			t.Fatalf("%s: LookupSuite: %v", vector.name, err)
		}
		if len(vector.Exports) != 3 {
			t.Fatalf("%s carries %d exported values, want the three RFC 9180 publishes", vector.name, len(vector.Exports))
		}
		sharedSecret := decodeVectorField(t, vector.name, "shared_secret", vector.SharedSecret)
		info := decodeVectorField(t, vector.name, "info", vector.Info)
		ctx, err := hpkeKeySchedule(params, sharedSecret, info)
		if err != nil {
			t.Fatalf("%s: key schedule: %v", vector.name, err)
		}
		for _, export := range vector.Exports {
			exporterContext := decodePossiblyEmptyVectorField(t, vector.name, "exporter_context", export.ExporterContext)
			want := decodeVectorField(t, vector.name, "exported_value", export.ExportedValue)
			if len(want) != export.Length {
				t.Fatalf("%s: the published export for context %q is %d bytes, the table says %d",
					vector.name, export.ExporterContext, len(want), export.Length)
			}
			got, err := ctx.Export(exporterContext, export.Length)
			if err != nil {
				t.Fatalf("%s: export for context %q: %v", vector.name, export.ExporterContext, err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s: export for context %q = %x, want %x", vector.name, export.ExporterContext, got, want)
			}
		}
	}
}

// TestHpkeContextExportLeavesTheSequenceAlone states what an export costs the sender, and
// it is here because the stability check in the label separation test above cannot state
// it. Export does not read the sequence number, so a version that advanced it returns
// exactly the same bytes as one that does not: applied to hpke.go, an Export that
// consumed a sequence number left every assertion in that test green, including the one
// whose comment says the sequence must not be consumed. What sees it is the next message
// — after an export, the seal at sequence 1 must still be the sequence 1 the RFC printed.
func TestHpkeContextExportLeavesTheSequenceAlone(t *testing.T) {
	vector := loadHpkeVectors(t)[1]
	params, err := LookupSuite(vector.suite)
	if err != nil {
		t.Fatalf("LookupSuite: %v", err)
	}
	if len(vector.Encryptions) < 2 || vector.Encryptions[0].sequence != 0 || vector.Encryptions[1].sequence != 1 {
		t.Fatalf("%s does not open with sequences 0 and 1, so the assertion below is about other messages", vector.name)
	}
	ctx, err := hpkeKeySchedule(params,
		decodeVectorField(t, vector.name, "shared_secret", vector.SharedSecret),
		decodeVectorField(t, vector.name, "info", vector.Info))
	if err != nil {
		t.Fatalf("key schedule: %v", err)
	}
	first := vector.Encryptions[0]
	if _, err := ctx.Seal(decodeVectorField(t, vector.name, "aad", first.Aad), decodeVectorField(t, vector.name, "pt", first.Pt)); err != nil {
		t.Fatalf("seal at sequence 0: %v", err)
	}
	if _, err := ctx.Export([]byte("an export between two messages"), 32); err != nil {
		t.Fatalf("export: %v", err)
	}
	if ctx.sequence != 1 {
		t.Errorf("the context is at sequence %d after one message and one export, want 1", ctx.sequence)
	}
	second := vector.Encryptions[1]
	ciphertext, err := ctx.Seal(decodeVectorField(t, vector.name, "aad", second.Aad), decodeVectorField(t, vector.name, "pt", second.Pt))
	if err != nil {
		t.Fatalf("seal at sequence 1: %v", err)
	}
	want := decodeVectorField(t, vector.name, "ct", second.Ct)
	if !bytes.Equal(ciphertext, want) {
		t.Errorf("the message after an export sealed to %x, want the published sequence 1 ciphertext %x", ciphertext, want)
	}
}

// TestHpkeContextOpenDoesNotAdvanceOnAFailure covers the receiver that an attacker could
// otherwise push past its sender with one injected packet: every genuine message after it
// would open under the wrong nonce and be refused, which is a denial of service available
// to anyone who can write to the transport.
//
// The tamper test above cannot see this. It builds a fresh receiver for every byte it
// flips, so no receiver in it is ever asked to open a genuine message after refusing a
// forged one — which is the only observation the property has. Applied to hpke.go, an
// Open that advanced on failure left that test green.
func TestHpkeContextOpenDoesNotAdvanceOnAFailure(t *testing.T) {
	params, err := LookupSuite(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("LookupSuite: %v", err)
	}
	sharedSecret := bytes.Repeat([]byte{0x14}, 32)
	sender, err := hpkeKeySchedule(params, sharedSecret, []byte("info"))
	if err != nil {
		t.Fatalf("sender key schedule: %v", err)
	}
	receiver, err := hpkeKeySchedule(params, sharedSecret, []byte("info"))
	if err != nil {
		t.Fatalf("receiver key schedule: %v", err)
	}
	aad := []byte("aad")
	messages := [][]byte{[]byte("the first message"), []byte("the second message")}
	sealed := make([][]byte, 0, len(messages))
	for i, message := range messages {
		ciphertext, err := sender.Seal(aad, message)
		if err != nil {
			t.Fatalf("seal %d: %v", i, err)
		}
		sealed = append(sealed, ciphertext)
	}
	forgery := bytes.Clone(sealed[0])
	forgery[0] ^= 0x01
	if _, err := receiver.Open(aad, forgery); !errors.Is(err, ErrAeadOpen) {
		t.Fatalf("the forgery opened: error = %v, want ErrAeadOpen", err)
	}
	if receiver.sequence != 0 {
		t.Errorf("the receiver is at sequence %d after refusing a forgery, want 0", receiver.sequence)
	}
	for i, ciphertext := range sealed {
		back, err := receiver.Open(aad, ciphertext)
		if err != nil {
			t.Fatalf("the genuine message %d was refused after a forgery: %v", i, err)
		}
		if !bytes.Equal(back, messages[i]) {
			t.Errorf("message %d opened to %q, want %q", i, back, messages[i])
		}
	}
}

// TestHpkeContextRefusesToWrapTheSequence covers the one arithmetic failure in this file
// that is a total break rather than a wrong answer. A counter that wrapped would put a
// second message on a nonce already used, and a repeated nonce hands an observer the xor
// of the two plaintexts and, under both of these aeads, the material to forge under that
// key for the rest of the context's life.
//
// The boundary is asserted from both sides because only that says where it is. A guard
// that never fires is the break itself; a guard one message early silently drops the last
// message a context was entitled to send, and neither an equality against the constant
// nor a refusal on its own separates the two. The RFC's own limit is 2^(8*Nn)-1 and is
// unreachable in a uint64, so what is asserted here is the counter's own width.
//
// Open's half is not reachable by sealing: at the last sequence number Seal computes a
// ciphertext and then drops it, so this package can never produce the message that would
// provoke it. The message a peer which did not stop would send is built from the
// context's own aead, and the assertion is that it decrypts and is still not handed back.
func TestHpkeContextRefusesToWrapTheSequence(t *testing.T) {
	params, err := LookupSuite(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("LookupSuite: %v", err)
	}
	sharedSecret := bytes.Repeat([]byte{0x15}, 32)
	aad := []byte("aad")
	plaintext := []byte("the last message this context may send")

	sender, err := hpkeKeySchedule(params, sharedSecret, nil)
	if err != nil {
		t.Fatalf("sender key schedule: %v", err)
	}
	sender.sequence = math.MaxUint64 - 1
	last, err := sender.Seal(aad, plaintext)
	if err != nil {
		t.Fatalf("the last representable message was refused: %v", err)
	}
	if sender.sequence != math.MaxUint64 {
		t.Fatalf("the sender is at sequence %d after its last message, want %d", sender.sequence, uint64(math.MaxUint64))
	}
	beyond, err := sender.Seal(aad, plaintext)
	if !errors.Is(err, ErrSequenceOverflow) {
		t.Errorf("sealing past the last sequence number: error = %v, want ErrSequenceOverflow", err)
	}
	if beyond != nil {
		t.Errorf("the refused seal returned %d bytes anyway", len(beyond))
	}
	if sender.sequence != math.MaxUint64 {
		t.Errorf("the sequence moved to %d on the refusal, so it wrapped rather than stopping", sender.sequence)
	}

	receiver, err := hpkeKeySchedule(params, sharedSecret, nil)
	if err != nil {
		t.Fatalf("receiver key schedule: %v", err)
	}
	receiver.sequence = math.MaxUint64 - 1
	back, err := receiver.Open(aad, last)
	if err != nil {
		t.Fatalf("the last representable message did not open: %v", err)
	}
	if !bytes.Equal(back, plaintext) {
		t.Errorf("the last message opened to %q, want %q", back, plaintext)
	}
	fromANonconformingPeer := receiver.aead.Seal(nil, receiver.nonce(), plaintext, aad)
	if opened, err := receiver.Open(aad, fromANonconformingPeer); !errors.Is(err, ErrSequenceOverflow) || opened != nil {
		t.Errorf("opening past the last sequence number returned %q and error %v, want nothing and ErrSequenceOverflow", opened, err)
	}
}

// TestHpkeContextNonceCarriesEveryBitOfTheSequence pins the counter's width and its
// placement above the range any published vector reaches. The corpus stops at sequence
// 256, so every known answer in this file exercises the counter's low nine bits and
// nothing over them, and the wrap test above sets sender and receiver to the same sequence number by
// hand, so a truncation applies to both alike and they go on agreeing with each other.
// Measured: a ComputeNonce writing self.sequence&0x1ff, &0xffff, &0xffffffff,
// &0xffffffffffff or the low 63 bits left the entire package green. The 32 bit one
// recomputes the sequence zero nonce at sequence 2^32 under a key still in use, which is
// the repeat the overflow guard exists to prevent, arriving 2^32 messages before the
// guard can see it.
//
// What is asserted is the whole of I2OSP(seq, Nn) at one bit a time: setting bit b of the
// sequence number must flip exactly the bit of the nonce that a big endian counter in the
// low Nn bytes puts it in, and nothing else. The counter enters by xor, so nonce(1<<b)
// xor nonce(0) is I2OSP(1<<b, Nn) with the base nonce cancelled out and no assumption
// about it. That the 65 nonces are then pairwise distinct — the security half, since a
// collision is a repeated nonce under a live key — follows from the 64 deltas being
// distinct and nonzero.
//
// Distinctness on its own is weaker, and measurably so: a counter that or-folded its
// high half onto its low one puts sequence 2^32 on sequence 1's nonce, which is not
// sequence zero's, so a comparison against the sequence zero nonce alone passes it. A
// counter that xor-folded instead is a bijection and passes pairwise distinctness too —
// no repeat, but a divergence from every conforming peer above 2^32. Both die here.
func TestHpkeContextNonceCarriesEveryBitOfTheSequence(t *testing.T) {
	params, err := LookupSuite(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("LookupSuite: %v", err)
	}
	ctx, err := hpkeKeySchedule(params, bytes.Repeat([]byte{0x19}, 32), nil)
	if err != nil {
		t.Fatalf("key schedule: %v", err)
	}
	ctx.sequence = 0
	atZero := ctx.nonce()
	for bit := 0; bit < 64; bit++ {
		ctx.sequence = uint64(1) << bit
		delta := ctx.nonce()
		for i := range delta {
			delta[i] ^= atZero[i]
		}
		want := make([]byte, params.Nn)
		want[params.Nn-1-bit/8] = 1 << (bit % 8)
		if !bytes.Equal(delta, want) {
			t.Errorf("bit %d of the sequence number moves the nonce by %x, want %x", bit, delta, want)
		}
	}
}

// TestHpkeContextExportRefusesUnrepresentableLengths is about the process rather than
// about a value. The export length is the only one in this package that comes from a
// caller — every other expansion takes a suite field — and crypto/hkdf.Expand does not
// refuse a negative one, it dies on it: its fips140 body opens with make([]byte, 0,
// keyLen) and a negative cap is "makeslice: cap out of range". So this says an export
// length reaches the guard rather than the kdf. Applied to hpke.go, an Export that called
// hkdf.Expand directly took the test binary down here.
//
// The accepting lengths are asserted beside the refusals, because a function hardwired to
// refuse everything satisfies refusals on its own.
func TestHpkeContextExportRefusesUnrepresentableLengths(t *testing.T) {
	params, err := LookupSuite(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("LookupSuite: %v", err)
	}
	ctx, err := hpkeKeySchedule(params, bytes.Repeat([]byte{0x16}, 32), nil)
	if err != nil {
		t.Fatalf("key schedule: %v", err)
	}
	for _, length := range []int{-1, hpkeMaxExpandLength + 1} {
		out, err := ctx.Export([]byte("context"), length)
		if !errors.Is(err, ErrBadKeyLength) {
			t.Errorf("export of %d bytes returned error %v, want ErrBadKeyLength", length, err)
		}
		if out != nil {
			t.Errorf("export of %d bytes refused and returned %d bytes anyway", length, len(out))
		}
	}
	for _, length := range []int{0, 1, params.Nk, hpkeMaxExpandLength} {
		out, err := ctx.Export([]byte("context"), length)
		if err != nil {
			t.Fatalf("export rejected %d bytes: %v", length, err)
		}
		if len(out) != length {
			t.Fatalf("export of %d bytes returned %d", length, len(out))
		}
	}
}

// TestHpkeKeyScheduleReadsTheFieldItNames is the key schedule's half of what
// TestHpkeKemReadsTheFieldItNames does for the kem, and it exists for exactly one of the
// three lengths the schedule expands to.
//
// Two of the three are separated by the registry itself and were measured to be. Nk is 16
// on the aes entry and 32 on the chacha one, so a key expanded to any other field is a
// key the aead refuses on at least one of the two rows: all nine interchanges died
// against the published vectors. Nn is 12 while every other field is 16 or 32, so a base
// nonce expanded to any of them is the wrong length against a published base_nonce: all
// nine died there too.
//
// Nh is separated by nothing. It is 32 in both entries, and so are Nsecret, Nenc, Npk,
// Nsk, NsigPub and NsigPriv — seven fields holding one value — so an exporter secret
// expanded to any of the other six is byte for byte the published one. Each of the six
// was applied to hpke.go and left the entire package green, this test excepted, and a
// constant 32 in place of the field is the same case and dies here for the same reason.
//
// The probe suite is the kem probes' own, which is what keeps a length from being left
// out and read as zero. Nn is pinned to 12 in both rows rather than left at the
// background because the key schedule refuses a suite whose Nn disagrees with its aead,
// and a probe that could not build a context would assert nothing.
func TestHpkeKeyScheduleReadsTheFieldItNames(t *testing.T) {
	sharedSecret := bytes.Repeat([]byte{0x17}, 32)

	// the exporter secret expands to Nh and not to the six other fields holding the same
	// 32 that both registered suites give it
	wideNh := hpkeFieldProbeParams(32)
	wideNh.Nn = 12
	wideNh.Nh = 48
	ctx, err := hpkeKeySchedule(wideNh, sharedSecret, nil)
	if err != nil {
		t.Fatalf("key schedule under Nh 48: %v", err)
	}
	if len(ctx.exporterSecret) != 48 {
		t.Errorf("the exporter secret is %d bytes under a suite whose Nh is 48 and whose every other length is 32 or 12, want 48", len(ctx.exporterSecret))
	}

	// and the key and the base nonce expand to Nk and Nn under a background that agrees
	// with neither. The key is asserted by the schedule completing at all: the aead gate
	// reads the same Nk, so a key of any other length is refused there.
	wideBackground := hpkeFieldProbeParams(48)
	wideBackground.Nk = 32
	wideBackground.Nn = 12
	ctx, err = hpkeKeySchedule(wideBackground, sharedSecret, nil)
	if err != nil {
		t.Fatalf("key schedule under Nk 32 and Nn 12 with every other length 48, so the key was not expanded to Nk: %v", err)
	}
	if len(ctx.baseNonce) != 12 {
		t.Errorf("the base nonce is %d bytes under a suite whose Nn is 12 and whose every other length is 32 or 48, want 12", len(ctx.baseNonce))
	}
	if len(ctx.exporterSecret) != 48 {
		t.Errorf("the exporter secret is %d bytes under a suite whose Nh is 48, want 48", len(ctx.exporterSecret))
	}
}

// TestHpkeKeyScheduleRefusesANonceLengthTheAeadWillNotTake covers a mismatch that is not
// a wrong answer but a dead process. ComputeNonce sizes its output from Nn, and both
// aeads panic on a nonce that is not their own length rather than returning an error, so
// a suite whose Nn disagreed with its aead would take the program down at the first Seal.
// The refusal happens at construction, before a key exists.
//
// Nothing in the registry can reach it — both entries agree with their aead — so a probe
// suite is the only way to state it, and the accepting case is asserted beside it so the
// refusal is not just "a probe suite fails".
//
// This guard is also why the two Nn reads inside ComputeNonce need no probe of their own:
// every context that exists has Nn equal to its aead's nonce size, so a nonce built to
// any other length is unreachable rather than merely detectable.
func TestHpkeKeyScheduleRefusesANonceLengthTheAeadWillNotTake(t *testing.T) {
	sharedSecret := bytes.Repeat([]byte{0x18}, 32)
	probe := hpkeFieldProbeParams(32)
	probe.Nn = 13
	ctx, err := hpkeKeySchedule(probe, sharedSecret, nil)
	if !errors.Is(err, ErrBadNonceLength) {
		t.Fatalf("a suite whose Nn is 13 against a 12 byte aead: error = %v, want ErrBadNonceLength", err)
	}
	if ctx != nil {
		t.Errorf("the refused key schedule returned a context anyway")
	}
	probe.Nn = 12
	if _, err := hpkeKeySchedule(probe, sharedSecret, nil); err != nil {
		t.Errorf("the aead's own nonce length was refused: %v", err)
	}
}

// TestHpkeSealBaseRoundTrip is the composition agreeing with itself across both
// registered suites, which is worth less than it looks like: a sender and a receiver
// wrong the same way agree perfectly, and the published vector further down is what says
// the composition is the RFC's rather than merely self consistent. What this one carries
// that nothing else does is the length. It is the only place a ciphertext HpkeSealBase
// produced is measured against the plaintext plus the suite's own Nt, and since both
// returned slices are []byte a transposed return compiles, so that measurement is half of
// what holds their order.
func TestHpkeSealBaseRoundTrip(t *testing.T) {
	for _, suite := range Suites() {
		params, err := LookupSuite(suite)
		if err != nil {
			t.Fatalf("LookupSuite: %v", err)
		}
		priv, pub, err := HpkeDeriveKeyPair(params, bytes.Repeat([]byte{0x08}, 32))
		if err != nil {
			t.Fatalf("derive: %v", err)
		}
		info := []byte("the info")
		aad := []byte("the aad")
		plaintext := []byte("the plaintext")
		kemOutput, ciphertext, err := HpkeSealBase(rand.Reader, params, pub, info, aad, plaintext)
		if err != nil {
			t.Fatalf("seal: %v", err)
		}
		if len(ciphertext) != len(plaintext)+params.Nt {
			t.Fatalf("ciphertext is %d bytes, want %d", len(ciphertext), len(plaintext)+params.Nt)
		}
		back, err := HpkeOpenBase(params, priv, kemOutput, info, aad, ciphertext)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		if !bytes.Equal(back, plaintext) {
			t.Fatalf("suite %#04x round trip returned %q", uint16(suite), back)
		}
	}
}

// TestHpkeOpenBaseRejectsWrongInfo is the only one of the three tests here that sees
// info at all. It is bound through the key schedule rather than checked, so a wrong info
// is an open failure and not a silently different plaintext — and a sender and a receiver
// that both dropped it round trip, while the wrong recipient below refuses for a reason
// that has nothing to do with info. Opening under an info the message was not sealed
// under is what separates a key schedule that binds it from one that does not. MLS puts
// the label and the group context here, which is why it has to bind.
func TestHpkeOpenBaseRejectsWrongInfo(t *testing.T) {
	params, err := LookupSuite(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("LookupSuite: %v", err)
	}
	priv, pub, err := HpkeDeriveKeyPair(params, bytes.Repeat([]byte{0x09}, 32))
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	kemOutput, ciphertext, err := HpkeSealBase(rand.Reader, params, pub, []byte("info a"), nil, []byte("plaintext"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := HpkeOpenBase(params, priv, kemOutput, []byte("info b"), nil, ciphertext); !errors.Is(err, ErrAeadOpen) {
		t.Fatalf("wrong info: error = %v, want ErrAeadOpen", err)
	}
}

// TestHpkeOpenBaseRejectsWrongRecipient is the only one of the three tests here that
// separates the decapsulated shared secret from the encapsulated key that carries it.
// Both are 32 bytes under the registered kem, so a key schedule fed the encapsulation
// instead of the secret compiles, round trips, and refuses a wrong info exactly as it
// should — while being a total break, since the encapsulation travels in the clear and
// every holder of it then derives the same context. A recipient who holds the wrong
// private key is where that stops agreeing.
func TestHpkeOpenBaseRejectsWrongRecipient(t *testing.T) {
	params, err := LookupSuite(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("LookupSuite: %v", err)
	}
	_, pub, err := HpkeDeriveKeyPair(params, bytes.Repeat([]byte{0x0a}, 32))
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	otherPriv, _, err := HpkeDeriveKeyPair(params, bytes.Repeat([]byte{0x0b}, 32))
	if err != nil {
		t.Fatalf("derive other: %v", err)
	}
	kemOutput, ciphertext, err := HpkeSealBase(rand.Reader, params, pub, nil, nil, []byte("plaintext"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := HpkeOpenBase(params, otherPriv, kemOutput, nil, nil, ciphertext); !errors.Is(err, ErrAeadOpen) {
		t.Fatalf("wrong recipient: error = %v, want ErrAeadOpen", err)
	}
}

// TestHpkeSealBaseMatchesThePublishedSingleShot is the single shot's own known answer,
// and it is what says the composition is the RFC's rather than merely self consistent.
// RFC 9180 section 6.1 defines SealBase as SetupBaseS followed by exactly one Seal, so
// the published enc and the published sequence zero ciphertext are together a statement
// about the whole composition: that the ephemeral key came from the reader the caller
// handed in, that the info reached the key schedule, that the aad reached the aead, and
// that the one message was sealed at sequence zero and not at any other.
//
// The ephemeral key is the vector's own skEm, fed in as the reader. X25519GenerateKey
// reads exactly thirty two bytes and uses them as the scalar, so a bytes.Reader over
// skEm reproduces the published encapsulation and the enc that comes back has to be the
// published one — which is the only way to check the randomized entry point against a
// vector at all.
//
// Both halves of the aad are load bearing here and nowhere else. A sender and a receiver
// that both ignored the aad agree with each other on every message and pass the round
// trip, the wrong info test and the wrong recipient test alike; that mutation was applied
// to hpke.go and survived the whole package. The published ciphertext covers the aad in
// its tag, so it is the assertion that sees it.
func TestHpkeSealBaseMatchesThePublishedSingleShot(t *testing.T) {
	for _, vector := range loadHpkeVectors(t) {
		params, err := LookupSuite(vector.suite)
		if err != nil {
			t.Fatalf("%s: LookupSuite: %v", vector.name, err)
		}
		if len(vector.Encryptions) == 0 || vector.Encryptions[0].sequence != 0 {
			t.Fatalf("%s does not open at sequence 0, so a single shot cannot be compared against it", vector.name)
		}
		first := vector.Encryptions[0]
		info := decodePossiblyEmptyVectorField(t, vector.name, "info", vector.Info)
		aad := decodeVectorField(t, vector.name, "aad", first.Aad)
		plaintext := decodeVectorField(t, vector.name, "pt", first.Pt)
		wantEnc := decodeVectorField(t, vector.name, "enc", vector.Enc)
		wantCiphertext := decodeVectorField(t, vector.name, "ct", first.Ct)
		ephemeral := decodeVectorField(t, vector.name, "skEm", vector.SkEm)
		recipient := decodeVectorField(t, vector.name, "pkRm", vector.PkRm)
		recipientPriv := decodeVectorField(t, vector.name, "skRm", vector.SkRm)

		kemOutput, ciphertext, err := HpkeSealBase(bytes.NewReader(ephemeral), params, HpkePublicKey(recipient), info, aad, plaintext)
		if err != nil {
			t.Fatalf("%s: seal: %v", vector.name, err)
		}
		if !bytes.Equal(kemOutput, wantEnc) {
			t.Errorf("%s: seal encapsulated to %x, want the published %x", vector.name, kemOutput, wantEnc)
		}
		if !bytes.Equal(ciphertext, wantCiphertext) {
			t.Errorf("%s: seal produced %x, want the published sequence 0 ciphertext %x", vector.name, ciphertext, wantCiphertext)
		}
		back, err := HpkeOpenBase(params, HpkePrivateKey(recipientPriv), wantEnc, info, aad, wantCiphertext)
		if err != nil {
			t.Fatalf("%s: open of the published ciphertext: %v", vector.name, err)
		}
		if !bytes.Equal(back, plaintext) {
			t.Errorf("%s: open returned %x, want the published plaintext %x", vector.name, back, plaintext)
		}
	}
}

// TestHpkeSetupBaseMatchesThePublishedSetup pins the two setup entry points the single
// shots are built from, at the one point a caller can still see the context: the sending
// side has to hand back the published enc and a context holding the published base nonce
// and exporter secret, and both sides have to start at sequence zero, because that is
// where every single shot message is sealed and opened.
//
// Sealing the published sequence zero message through the returned context is what pins
// the key, which cipher.AEAD does not expose. Without it the aead key could be expanded
// from anything and the base nonce and exporter secret would still match.
//
// The two values the setup passes on are separately confusable: the shared secret and the
// encapsulated key are both thirty two bytes under this kem, so a setup that fed the
// encapsulated key into the key schedule, or returned the shared secret as enc, compiles
// and produces a working looking context. Both were applied to hpke.go; the published
// base nonce is what refuses them.
func TestHpkeSetupBaseMatchesThePublishedSetup(t *testing.T) {
	for _, vector := range loadHpkeVectors(t) {
		params, err := LookupSuite(vector.suite)
		if err != nil {
			t.Fatalf("%s: LookupSuite: %v", vector.name, err)
		}
		if len(vector.Encryptions) == 0 || vector.Encryptions[0].sequence != 0 {
			t.Fatalf("%s does not open at sequence 0, so a fresh context cannot be compared against it", vector.name)
		}
		first := vector.Encryptions[0]
		info := decodePossiblyEmptyVectorField(t, vector.name, "info", vector.Info)
		aad := decodeVectorField(t, vector.name, "aad", first.Aad)
		plaintext := decodeVectorField(t, vector.name, "pt", first.Pt)
		wantCiphertext := decodeVectorField(t, vector.name, "ct", first.Ct)
		wantEnc := decodeVectorField(t, vector.name, "enc", vector.Enc)
		wantBaseNonce := decodeVectorField(t, vector.name, "base_nonce", vector.BaseNonce)
		wantExporterSecret := decodeVectorField(t, vector.name, "exporter_secret", vector.ExporterSecret)
		ephemeral := decodeVectorField(t, vector.name, "skEm", vector.SkEm)
		recipient := decodeVectorField(t, vector.name, "pkRm", vector.PkRm)
		recipientPriv := decodeVectorField(t, vector.name, "skRm", vector.SkRm)

		kemOutput, sender, err := HpkeSetupBaseS(bytes.NewReader(ephemeral), params, HpkePublicKey(recipient), info)
		if err != nil {
			t.Fatalf("%s: setup base s: %v", vector.name, err)
		}
		if !bytes.Equal(kemOutput, wantEnc) {
			t.Errorf("%s: setup base s encapsulated to %x, want the published %x", vector.name, kemOutput, wantEnc)
		}
		receiver, err := HpkeSetupBaseR(params, HpkePrivateKey(recipientPriv), wantEnc, info)
		if err != nil {
			t.Fatalf("%s: setup base r: %v", vector.name, err)
		}
		for _, side := range []struct {
			name string
			ctx  *HpkeContext
		}{
			{name: "setup base s", ctx: sender},
			{name: "setup base r", ctx: receiver},
		} {
			if side.ctx.sequence != 0 {
				t.Errorf("%s: %s returned a context at sequence %d, want 0", vector.name, side.name, side.ctx.sequence)
			}
			if !bytes.Equal(side.ctx.baseNonce, wantBaseNonce) {
				t.Errorf("%s: %s base nonce is %x, want the published %x", vector.name, side.name, side.ctx.baseNonce, wantBaseNonce)
			}
			if !bytes.Equal(side.ctx.exporterSecret, wantExporterSecret) {
				t.Errorf("%s: %s exporter secret is %x, want the published %x", vector.name, side.name, side.ctx.exporterSecret, wantExporterSecret)
			}
		}
		ciphertext, err := sender.Seal(aad, plaintext)
		if err != nil {
			t.Fatalf("%s: seal through the returned context: %v", vector.name, err)
		}
		if !bytes.Equal(ciphertext, wantCiphertext) {
			t.Errorf("%s: the context from setup base s sealed to %x, want the published sequence 0 ciphertext %x", vector.name, ciphertext, wantCiphertext)
		}
		back, err := receiver.Open(aad, wantCiphertext)
		if err != nil {
			t.Fatalf("%s: open through the returned context: %v", vector.name, err)
		}
		if !bytes.Equal(back, plaintext) {
			t.Errorf("%s: the context from setup base r opened to %x, want %x", vector.name, back, plaintext)
		}
	}
}

// TestHpkeSealBaseBuildsAFreshContextPerCall is the one this file exists for. A single
// shot has to set up a context, use it once and drop it; an implementation that kept one
// — memoized on the recipient key, hoisted to a package variable, or reused to save an
// encapsulation — has two ways to be wrong and only one of them is visible to a round
// trip.
//
// If the kept context advances, the second message is sealed at sequence one and no
// single shot receiver can open it, because a receiver builds its own context at zero.
// That one a round trip catches, but only a round trip that seals twice: the plan's seals
// once per suite, and a cache keyed on the suite survived it and the entire package. An
// unkeyed package variable is the one shape of it the plan does catch, and only by
// accident — the round trip's second suite finds the first suite's context waiting.
//
// If the kept context is reset instead, or if the key schedule is fed something that is
// not the encapsulation's own shared secret, then two plaintexts are sealed under one key
// at one nonce and everything round trips perfectly. Nothing about a round trip can see
// it. What can is that both aeads here are a stream cipher with an authenticator over the
// result, so under a repeated key and nonce the two ciphertexts differ by exactly what
// the two plaintexts differ by. That xor is asserted directly below. It and the repeated
// plaintext's ciphertext equality are two detectors of one property: on a fresh-ephemeral
// implementation whose key schedule ignores the shared secret — a mutant whose enc values
// are all distinct, so every freshness check beside these two is satisfied — either one
// alone still fails. Both are kept so an edit that drops one leaves the property standing
// on the other. Dropping both is what lets that mutant through.
//
// The repeated plaintext is the third row. Two seals of one plaintext to one recipient
// must still differ, which is the same property stated where an implementation that
// cached on the plaintext rather than on the key would land.
func TestHpkeSealBaseBuildsAFreshContextPerCall(t *testing.T) {
	for _, suite := range Suites() {
		params, err := LookupSuite(suite)
		if err != nil {
			t.Fatalf("LookupSuite: %v", err)
		}
		priv, pub, err := HpkeDeriveKeyPair(params, bytes.Repeat([]byte{0x1a}, 32))
		if err != nil {
			t.Fatalf("derive: %v", err)
		}
		info := []byte("one info for every message")
		aad := []byte("one aad for every message")
		plaintexts := [][]byte{
			[]byte("the first plaintext of an identical pair"),
			[]byte("a second plaintext, of the same length!!"),
			[]byte("the first plaintext of an identical pair"),
		}
		if len(plaintexts[0]) != len(plaintexts[1]) {
			t.Fatalf("the first two plaintexts are %d and %d bytes, so the keystream comparison below is not the one it says it is",
				len(plaintexts[0]), len(plaintexts[1]))
		}
		if bytes.Equal(plaintexts[0], plaintexts[1]) {
			t.Fatalf("the first two plaintexts are equal, so the keystream comparison below asserts nothing")
		}
		kemOutputs := make([][]byte, 0, len(plaintexts))
		ciphertexts := make([][]byte, 0, len(plaintexts))
		for i, plaintext := range plaintexts {
			kemOutput, ciphertext, err := HpkeSealBase(rand.Reader, params, pub, info, aad, plaintext)
			if err != nil {
				t.Fatalf("suite %#04x seal %d: %v", uint16(suite), i, err)
			}
			// every message is sealed at sequence zero, so every message opens under a
			// receiving context that is also at sequence zero. A sender that kept its
			// context past the first call fails here on the second.
			back, err := HpkeOpenBase(params, priv, kemOutput, info, aad, ciphertext)
			if err != nil {
				t.Fatalf("suite %#04x open %d: %v", uint16(suite), i, err)
			}
			if !bytes.Equal(back, plaintext) {
				t.Fatalf("suite %#04x open %d returned %q, want %q", uint16(suite), i, back, plaintext)
			}
			kemOutputs = append(kemOutputs, kemOutput)
			ciphertexts = append(ciphertexts, ciphertext)
		}
		for i := range kemOutputs {
			for j := i + 1; j < len(kemOutputs); j++ {
				if bytes.Equal(kemOutputs[i], kemOutputs[j]) {
					t.Errorf("suite %#04x seals %d and %d encapsulated to the same key %x, so both messages are under one key",
						uint16(suite), i, j, kemOutputs[i])
				}
				if bytes.Equal(ciphertexts[i], ciphertexts[j]) {
					t.Errorf("suite %#04x seals %d and %d produced the same ciphertext", uint16(suite), i, j)
				}
			}
		}
		// the bytes two slices of one length differ by. It refuses unequal lengths
		// rather than truncating, because a comparison over a prefix is a comparison
		// that could pass by saying less than it meant to.
		xorOf := func(a []byte, b []byte) []byte {
			if len(a) != len(b) {
				t.Fatalf("xor of %d and %d bytes", len(a), len(b))
			}
			delta := make([]byte, len(a))
			for i := range delta {
				delta[i] = a[i] ^ b[i]
			}
			return delta
		}
		body := len(plaintexts[0])
		if delta := xorOf(ciphertexts[0][:body], ciphertexts[1][:body]); bytes.Equal(delta, xorOf(plaintexts[0], plaintexts[1])) {
			t.Errorf("suite %#04x sealed two plaintexts to ciphertexts differing by exactly the plaintexts: one key and one nonce for both messages",
				uint16(suite))
		}
	}
}

// TestHpkeOpenBaseRefusesEveryAlteredInput walks what a peer can change on the wire. Each
// row is a refusal, and the unaltered case asserted before them is what keeps the table
// from being satisfied by an open that refuses everything.
//
// The aad rows are the ones nothing else in the file reaches by property. A sender and a
// receiver that both dropped the aad round trip, so the only two things that see it are
// the published ciphertext in the known answer above and a wrong aad refused here.
//
// Those rows all alter a message sealed under a non-empty info and a non-empty aad, which
// walks the binding in one direction only: sealed under a value, opened under a different
// one. The three rows over the bare message and the transposed pair walk the other
// direction, and it is the direction an attacker picks. An open that retried under a nil
// info, a nil aad, or the other argument when the caller's failed is invisible in the
// first direction, because its retry fails there too; and every such retry is a receiver
// that accepts a message carrying no group label at all from anyone holding the
// recipient's public key, which is the unforgeability HpkeSetupBaseR's own comment
// claims. Twelve retries of that shape were applied to hpke.go and all twelve survived
// the whole package before these rows existed.
//
// The other suite's parameters are a row because a single shot takes its suite from a
// caller rather than from the ciphertext: nothing on the wire says which suite sealed a
// message, and opening under the wrong one has to fail closed rather than return whatever
// key schedule it reached.
//
// Every row asserts a nil plaintext beside the sentinel. An open that refused and handed
// back the aead's scratch buffer anyway would satisfy an error-only assertion.
func TestHpkeOpenBaseRefusesEveryAlteredInput(t *testing.T) {
	params, err := LookupSuite(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("LookupSuite: %v", err)
	}
	otherParams, err := LookupSuite(CipherSuiteX25519AesGcm128Sha256Ed25519)
	if err != nil {
		t.Fatalf("LookupSuite: %v", err)
	}
	priv, pub, err := HpkeDeriveKeyPair(params, bytes.Repeat([]byte{0x1b}, 32))
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	info := []byte("the info this message was sealed under")
	aad := []byte("the aad this message was sealed under")
	plaintext := []byte("the plaintext")
	kemOutput, ciphertext, err := HpkeSealBase(rand.Reader, params, pub, info, aad, plaintext)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if back, err := HpkeOpenBase(params, priv, kemOutput, info, aad, ciphertext); err != nil || !bytes.Equal(back, plaintext) {
		t.Fatalf("the unaltered message returned %q and %v, so every refusal below would be one this open owes to nothing", back, err)
	}
	// a second message, sealed under neither an info nor an aad, for the rows that claim
	// a value over a message that carried none. Its own unaltered open is asserted for
	// the same reason the one above is.
	bareKemOutput, bareCiphertext, err := HpkeSealBase(rand.Reader, params, pub, nil, nil, plaintext)
	if err != nil {
		t.Fatalf("seal under neither an info nor an aad: %v", err)
	}
	if back, err := HpkeOpenBase(params, priv, bareKemOutput, nil, nil, bareCiphertext); err != nil || !bytes.Equal(back, plaintext) {
		t.Fatalf("the unaltered bare message returned %q and %v, so the rows claiming a value over it would be refusals owed to nothing", back, err)
	}
	flipped := func(bs []byte, i int) []byte {
		altered := bytes.Clone(bs)
		altered[i] ^= 0x01
		return altered
	}
	for _, row := range []struct {
		name       string
		params     *SuiteParams
		kemOutput  []byte
		info       []byte
		aad        []byte
		ciphertext []byte
	}{
		{name: "a different aad", params: params, kemOutput: kemOutput, info: info, aad: []byte("some other aad entirely"), ciphertext: ciphertext},
		{name: "no aad at all", params: params, kemOutput: kemOutput, info: info, aad: nil, ciphertext: ciphertext},
		{name: "one byte appended to the aad", params: params, kemOutput: kemOutput, info: info, aad: append(bytes.Clone(aad), 0x00), ciphertext: ciphertext},
		{name: "one bit flipped in the aad", params: params, kemOutput: kemOutput, info: info, aad: flipped(aad, 0), ciphertext: ciphertext},
		{name: "one bit flipped in the info", params: params, kemOutput: kemOutput, info: flipped(info, 0), aad: aad, ciphertext: ciphertext},
		{name: "an aad claimed over a message sealed with none", params: params, kemOutput: bareKemOutput, info: nil, aad: aad, ciphertext: bareCiphertext},
		{name: "an info claimed over a message sealed with none", params: params, kemOutput: bareKemOutput, info: info, aad: nil, ciphertext: bareCiphertext},
		{name: "the info and the aad transposed", params: params, kemOutput: kemOutput, info: aad, aad: info, ciphertext: ciphertext},
		{name: "one bit flipped in the kem output", params: params, kemOutput: flipped(kemOutput, 0), info: info, aad: aad, ciphertext: ciphertext},
		{name: "one bit flipped in the ciphertext body", params: params, kemOutput: kemOutput, info: info, aad: aad, ciphertext: flipped(ciphertext, 0)},
		{name: "one bit flipped in the tag", params: params, kemOutput: kemOutput, info: info, aad: aad, ciphertext: flipped(ciphertext, len(ciphertext)-1)},
		{name: "the ciphertext one byte short", params: params, kemOutput: kemOutput, info: info, aad: aad, ciphertext: ciphertext[:len(ciphertext)-1]},
		{name: "an empty ciphertext", params: params, kemOutput: kemOutput, info: info, aad: aad, ciphertext: nil},
		{name: "the other registered suite", params: otherParams, kemOutput: kemOutput, info: info, aad: aad, ciphertext: ciphertext},
	} {
		back, err := HpkeOpenBase(row.params, priv, row.kemOutput, row.info, row.aad, row.ciphertext)
		if !errors.Is(err, ErrAeadOpen) {
			t.Errorf("%s: error = %v, want ErrAeadOpen", row.name, err)
		}
		if back != nil {
			t.Errorf("%s: refused and returned %d bytes anyway", row.name, len(back))
		}
	}
}

// TestHpkeSealBaseAndOpenBaseReportTheFailuresBeneathThem says the single shots pass their
// kem's and their key schedule's refusals through rather than replacing them. The
// sentinels are the interesting part: an implementation that swallowed an error and
// carried on would build a whole key schedule over a nil shared secret and return a
// context that works against nobody, and one that mapped everything to ErrAeadOpen would
// tell a caller its own malformed key was a bad message.
//
// The setup entry points are asserted beside the single shots because that is where a
// swallow would live. A setup that dropped its key schedule error returns a nil context
// with a nil error, and the single shot in front of it then panics rather than failing,
// which is a kill by accident and not the statement wanted here.
//
// The nonce length row is the only refusal the key schedule itself owns, and no
// registered suite can reach it — both agree with their aead — so it uses the probe suite
// the field tests build, with the same all-fields-from-one-argument discipline that keeps
// a length from being left out and read as zero.
func TestHpkeSealBaseAndOpenBaseReportTheFailuresBeneathThem(t *testing.T) {
	params, err := LookupSuite(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("LookupSuite: %v", err)
	}
	priv, pub, err := HpkeDeriveKeyPair(params, bytes.Repeat([]byte{0x1c}, 32))
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	kemOutput, ciphertext, err := HpkeSealBase(rand.Reader, params, pub, nil, nil, []byte("the plaintext"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	badNonce := hpkeFieldProbeParams(32)
	badNonce.Nn = 13
	entropyIsDown := errors.New("entropy source is down")

	// the reader is built per call rather than shared between the two below. shortReader
	// carries its remaining count, so one instance handed to both entry points is drained
	// by the first and answers the second with a plain EOF — a row that would then be
	// asserting a different failure than the one it names.
	for _, row := range []struct {
		name   string
		random func() io.Reader
		params *SuiteParams
		pub    HpkePublicKey
		want   error
	}{
		{name: "a dead entropy source", random: func() io.Reader { return failingReader{err: entropyIsDown} }, params: params, pub: pub, want: entropyIsDown},
		{name: "an entropy source that runs dry", random: func() io.Reader { return &shortReader{remaining: params.Nsk - 1} }, params: params, pub: pub, want: io.ErrUnexpectedEOF},
		{name: "a recipient key one byte short", random: func() io.Reader { return rand.Reader }, params: params, pub: pub[:len(pub)-1], want: ErrBadKeyLength},
		{name: "a recipient key one byte long", random: func() io.Reader { return rand.Reader }, params: params, pub: append(bytes.Clone(pub), 0x00), want: ErrBadKeyLength},
		{name: "a suite whose nonce length the aead will not take", random: func() io.Reader { return rand.Reader }, params: badNonce, pub: pub, want: ErrBadNonceLength},
	} {
		gotEnc, gotCiphertext, err := HpkeSealBase(row.random(), row.params, row.pub, nil, nil, []byte("the plaintext"))
		if !errors.Is(err, row.want) {
			t.Errorf("seal with %s: error = %v, want %v", row.name, err, row.want)
		}
		if gotEnc != nil || gotCiphertext != nil {
			t.Errorf("seal with %s: refused and returned %d/%d bytes anyway", row.name, len(gotEnc), len(gotCiphertext))
		}
		gotEnc, ctx, err := HpkeSetupBaseS(row.random(), row.params, row.pub, nil)
		if !errors.Is(err, row.want) {
			t.Errorf("setup base s with %s: error = %v, want %v", row.name, err, row.want)
		}
		if gotEnc != nil || ctx != nil {
			t.Errorf("setup base s with %s: refused and returned %d bytes and a context anyway", row.name, len(gotEnc))
		}
	}

	for _, row := range []struct {
		name      string
		params    *SuiteParams
		priv      HpkePrivateKey
		kemOutput []byte
		want      error
	}{
		{name: "a kem output one byte short", params: params, priv: priv, kemOutput: kemOutput[:len(kemOutput)-1], want: ErrBadKemOutput},
		{name: "a kem output one byte long", params: params, priv: priv, kemOutput: append(bytes.Clone(kemOutput), 0x00), want: ErrBadKemOutput},
		{name: "an empty kem output", params: params, priv: priv, kemOutput: nil, want: ErrBadKemOutput},
		{name: "a private key one byte short", params: params, priv: priv[:len(priv)-1], kemOutput: kemOutput, want: ErrBadKeyLength},
		{name: "a suite whose nonce length the aead will not take", params: badNonce, priv: priv, kemOutput: kemOutput, want: ErrBadNonceLength},
	} {
		back, err := HpkeOpenBase(row.params, row.priv, row.kemOutput, nil, nil, ciphertext)
		if !errors.Is(err, row.want) {
			t.Errorf("open with %s: error = %v, want %v", row.name, err, row.want)
		}
		if back != nil {
			t.Errorf("open with %s: refused and returned %d bytes anyway", row.name, len(back))
		}
		ctx, err := HpkeSetupBaseR(row.params, row.priv, row.kemOutput, nil)
		if !errors.Is(err, row.want) {
			t.Errorf("setup base r with %s: error = %v, want %v", row.name, err, row.want)
		}
		if ctx != nil {
			t.Errorf("setup base r with %s: refused and returned a context anyway", row.name)
		}
	}
}
