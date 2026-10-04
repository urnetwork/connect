package messagegroup

import (
	"bytes"
	"crypto/mlkem"
	"crypto/rand"
	"crypto/sha3"
	"errors"
	"testing"

	"github.com/urnetwork/connect/mls"
)

// The public key of one x25519 scalar, through the one ECDH wrapper this tree has. It is the
// independent computation the expansion and the encapsulation reader are held against: it
// shares no code with xwing.go beyond the wrapper both are entitled to call, so a window into
// the expansion moving would move only one side of each comparison.
func x25519PublicKeyOfScalar(scalar []byte) ([]byte, error) {
	private, err := mls.X25519PrivateKey(scalar)
	if err != nil {
		return nil, err
	}
	return private.PublicKey().Bytes(), nil
}

func TestXwingSizesAreTheDraftSizes(t *testing.T) {
	// G9. the 32 versus 64 seed confusion is the specific hazard: the storable X-Wing private
	// key is 32 bytes and the ML-KEM seed it expands to is 64.
	if XwingSeedSize != 32 {
		t.Errorf("XwingSeedSize = %d, want 32", XwingSeedSize)
	}
	if XwingExpandedSize != 96 {
		t.Errorf("XwingExpandedSize = %d, want 96", XwingExpandedSize)
	}
	if XwingMlkemSeedSize != 64 {
		t.Errorf("XwingMlkemSeedSize = %d, want 64", XwingMlkemSeedSize)
	}
	if XwingMlkemSeedSize+XwingX25519KeySize != XwingExpandedSize {
		t.Errorf("the expansion does not partition: %d + %d != %d", XwingMlkemSeedSize, XwingX25519KeySize, XwingExpandedSize)
	}
	if XwingPublicKeySize != XwingMlkemPublicKeySize+XwingX25519KeySize {
		t.Errorf("XwingPublicKeySize = %d, want %d", XwingPublicKeySize, XwingMlkemPublicKeySize+XwingX25519KeySize)
	}
	if XwingCiphertextSize != XwingMlkemCiphertextSize+XwingX25519KeySize {
		t.Errorf("XwingCiphertextSize = %d, want %d", XwingCiphertextSize, XwingMlkemCiphertextSize+XwingX25519KeySize)
	}
	if XwingPublicKeySize != 1216 || XwingCiphertextSize != 1120 || XwingSharedSize != 32 {
		t.Errorf("sizes are %d/%d/%d, want 1216/1120/32", XwingPublicKeySize, XwingCiphertextSize, XwingSharedSize)
	}
	if XwingAlgId != 0x0014 {
		t.Errorf("XwingAlgId = %#04x, want 0x0014", XwingAlgId)
	}
}

// The ML-KEM halves are held to the standard library's own numbers rather than to the two
// literals above, which are prose this file could copy a digit wrong out of exactly as easily as
// xwing.go could. A real key and a real ciphertext are what say how long each is.
func TestTheMlkemHalvesAreTheLengthsCryptoMlkemProduces(t *testing.T) {
	decapsulation, err := mlkem.GenerateKey768()
	if err != nil {
		t.Fatalf("mlkem generate: %v", err)
	}
	if got := len(decapsulation.EncapsulationKey().Bytes()); got != XwingMlkemPublicKeySize {
		t.Errorf("crypto/mlkem's encapsulation key is %d bytes and XwingMlkemPublicKeySize is %d", got, XwingMlkemPublicKeySize)
	}
	_, ciphertext := decapsulation.EncapsulationKey().Encapsulate()
	if got := len(ciphertext); got != XwingMlkemCiphertextSize {
		t.Errorf("crypto/mlkem's ciphertext is %d bytes and XwingMlkemCiphertextSize is %d", got, XwingMlkemCiphertextSize)
	}
}

func TestXwingKeyGenIsDeterministicFromTheSeed(t *testing.T) {
	// seed only restore depends on this: the recovery key is reconstructible from the mnemonic
	// alone, MASTER section 5.2.
	seed := bytes.Repeat([]byte{0x20}, XwingSeedSize)
	a, err := XwingKeyGenFromSeed(seed)
	if err != nil {
		t.Fatalf("keygen a: %v", err)
	}
	b, err := XwingKeyGenFromSeed(seed)
	if err != nil {
		t.Fatalf("keygen b: %v", err)
	}
	if !bytes.Equal(a.Public().Bytes(), b.Public().Bytes()) {
		t.Fatalf("keygen is not deterministic")
	}
	if !bytes.Equal(a.Seed(), seed) {
		t.Fatalf("Seed() = %x, want the input seed", a.Seed())
	}
	if len(a.Public().Bytes()) != XwingPublicKeySize {
		t.Fatalf("public key is %d bytes, want %d", len(a.Public().Bytes()), XwingPublicKeySize)
	}

	other, err := XwingKeyGenFromSeed(bytes.Repeat([]byte{0x21}, XwingSeedSize))
	if err != nil {
		t.Fatalf("keygen other: %v", err)
	}
	if bytes.Equal(a.Public().Bytes(), other.Public().Bytes()) {
		t.Fatalf("different seeds produced the same public key")
	}
}

func TestXwingSeedExpansionIsShake256(t *testing.T) {
	// the expansion is asserted against an independent computation, so a swap to SHAKE-128 or a
	// wrong output length cannot pass. draft section 5.2:
	//   expanded = SHAKE256(seed, 96)
	//   (pk_M, sk_M) = ML-KEM-768.KeyGen_internal(expanded[0:32], expanded[32:64])
	//   sk_X = expanded[64:96]
	seed := bytes.Repeat([]byte{0x22}, XwingSeedSize)
	expanded := sha3.SumSHAKE256(seed, XwingExpandedSize)
	decapsulationKey, err := mlkem.NewDecapsulationKey768(expanded[0:XwingMlkemSeedSize])
	if err != nil {
		t.Fatalf("mlkem: %v", err)
	}
	priv, err := XwingKeyGenFromSeed(seed)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	pub := priv.Public().Bytes()
	if !bytes.Equal(pub[0:XwingMlkemPublicKeySize], decapsulationKey.EncapsulationKey().Bytes()) {
		t.Fatalf("the ml-kem half of the public key is not KeyGen(SHAKE256(seed, 96)[0:64])")
	}
	// The x25519 half is held to expanded[64:96] and not merely to "not all zero". The plan's
	// version of this test asserted only that the half was non zero, and every wrong window into
	// the expansion -- expanded[0:32], expanded[32:64] -- is non zero too, so it was an
	// assertion the draft's partition could fail while it passed. The independent computation is
	// the same one the sibling half above gets: mls owns the scalar to public key step, and this
	// reaches it the way any other caller would.
	scalar, err := x25519PublicKeyOfScalar(expanded[XwingMlkemSeedSize:XwingExpandedSize])
	if err != nil {
		t.Fatalf("x25519: %v", err)
	}
	if !bytes.Equal(pub[XwingMlkemPublicKeySize:], scalar) {
		t.Fatalf("the x25519 half of the public key is not the public key of SHAKE256(seed, 96)[64:96]")
	}
	// and the negative half, so the assertion above is known to be discriminating rather than
	// accidentally satisfied by every window
	for _, wrong := range [][]byte{expanded[0:XwingX25519KeySize], expanded[XwingX25519KeySize : 2*XwingX25519KeySize]} {
		other, err := x25519PublicKeyOfScalar(wrong)
		if err != nil {
			t.Fatalf("x25519: %v", err)
		}
		if bytes.Equal(pub[XwingMlkemPublicKeySize:], other) {
			t.Fatalf("a window of the expansion other than [64:96] produced the same x25519 half, so this assertion says nothing")
		}
	}
}

func TestXwingKeyGenRejectsWrongSeedSizes(t *testing.T) {
	for _, n := range []int{0, 31, 33, 64, 96} {
		if _, err := XwingKeyGenFromSeed(make([]byte, n)); !errors.Is(err, ErrXwingBadSeedSize) {
			t.Errorf("XwingKeyGenFromSeed(%d bytes) error = %v, want ErrXwingBadSeedSize", n, err)
		}
	}
}

func TestParseXwingPublicKeyRejectsWrongSizes(t *testing.T) {
	for _, n := range []int{0, 32, 1184, 1215, 1217, 2432} {
		if _, err := ParseXwingPublicKey(make([]byte, n)); !errors.Is(err, ErrXwingBadPublicKeySize) {
			t.Errorf("ParseXwingPublicKey(%d bytes) error = %v, want ErrXwingBadPublicKeySize", n, err)
		}
	}
	priv, err := XwingGenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	parsed, err := ParseXwingPublicKey(priv.Public().Bytes())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !bytes.Equal(parsed.Bytes(), priv.Public().Bytes()) {
		t.Fatalf("parse round trip changed the bytes")
	}
}

func TestXwingPublicKeyBytesDoesNotAlias(t *testing.T) {
	// a caller that mutates the returned slice must not corrupt the key it came from, and both
	// ways of getting a public key are covered: the plan's version tested the generated one
	// only, so a ParseXwingPublicKey that kept a view of its argument was outside it.
	priv, err := XwingGenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	parsed, err := ParseXwingPublicKey(priv.Public().Bytes())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for name, pub := range map[string]*XwingPublicKey{"generated": priv.Public(), "parsed": parsed} {
		original := append([]byte{}, pub.Bytes()...)
		first := pub.Bytes()
		first[0] ^= 0xff
		first[XwingPublicKeySize-1] ^= 0xff
		if bytes.Equal(pub.Bytes(), first) {
			t.Errorf("%s: Bytes returns the internal buffer", name)
		}
		if !bytes.Equal(pub.Bytes(), original) {
			t.Errorf("%s: mutating a returned slice changed the key", name)
		}
	}
}

// The parse direction of the same rule, from the other side: a caller that mutates the buffer it
// parsed FROM must not change the key it parsed. ParseXwingPublicKey's own comment claims this
// and nothing observed it.
func TestParseXwingPublicKeyDoesNotViewItsArgument(t *testing.T) {
	priv, err := XwingGenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	encoded := priv.Public().Bytes()
	parsed, err := ParseXwingPublicKey(encoded)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	before := append([]byte{}, parsed.Bytes()...)
	encoded[0] ^= 0xff
	encoded[XwingPublicKeySize-1] ^= 0xff
	if !bytes.Equal(parsed.Bytes(), before) {
		t.Fatalf("mutating the parsed buffer changed the parsed key")
	}
}

// Seed() and the seed the constructor was handed are each claimed to be copies, and neither
// claim was observed by anything the plan supplied: a Seed that returned the internal slice, and
// a constructor that kept the caller's, both passed every test it wrote. The consequence is not
// cosmetic -- a caller that zeroizes its own seed buffer after generating a key would blank the
// only value seed only restore has, MASTER section 5.2.
func TestTheSeedIsCopiedInBothDirections(t *testing.T) {
	seed := bytes.Repeat([]byte{0x24}, XwingSeedSize)
	priv, err := XwingKeyGenFromSeed(seed)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	for i := range seed {
		seed[i] = 0
	}
	if bytes.Equal(priv.Seed(), seed) {
		t.Fatalf("zeroizing the caller's buffer blanked the key's own seed")
	}
	if !bytes.Equal(priv.Seed(), bytes.Repeat([]byte{0x24}, XwingSeedSize)) {
		t.Fatalf("Seed() = %x, want the seed the key was built from", priv.Seed())
	}

	handedOut := priv.Seed()
	for i := range handedOut {
		handedOut[i] = 0
	}
	if !bytes.Equal(priv.Seed(), bytes.Repeat([]byte{0x24}, XwingSeedSize)) {
		t.Fatalf("Seed() hands out the internal buffer: zeroizing it blanked the key")
	}
}

func TestXwingRoundTrip(t *testing.T) {
	priv, err := XwingGenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	ct, ss, err := XwingEncapsulate(rand.Reader, priv.Public())
	if err != nil {
		t.Fatalf("encapsulate: %v", err)
	}
	if len(ct) != XwingCiphertextSize {
		t.Fatalf("ciphertext is %d bytes, want %d", len(ct), XwingCiphertextSize)
	}
	if len(ss) != XwingSharedSize {
		t.Fatalf("shared secret is %d bytes, want %d", len(ss), XwingSharedSize)
	}
	back, err := XwingDecapsulate(priv, ct)
	if err != nil {
		t.Fatalf("decapsulate: %v", err)
	}
	if !bytes.Equal(ss, back) {
		t.Fatalf("encapsulate and decapsulate disagree")
	}
}

func TestXwingEncapsulateIsNotDerandomizable(t *testing.T) {
	// the random argument supplies ek_X only. crypto/mlkem's Encapsulate takes no randomness and
	// reads crypto/rand itself, so two calls with an identical reader still differ. this is
	// asserted rather than documented because a caller who assumed determinism would build a
	// broken known answer test and not notice.
	priv, err := XwingGenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	fixed := bytes.Repeat([]byte{0x23}, 1024)
	first, _, err := XwingEncapsulate(bytes.NewReader(fixed), priv.Public())
	if err != nil {
		t.Fatalf("encapsulate 1: %v", err)
	}
	second, _, err := XwingEncapsulate(bytes.NewReader(fixed), priv.Public())
	if err != nil {
		t.Fatalf("encapsulate 2: %v", err)
	}
	if bytes.Equal(first, second) {
		t.Fatalf("encapsulation was derandomized, which crypto/mlkem does not permit")
	}
	// the x25519 half, which the reader does control, must match.
	if !bytes.Equal(first[XwingMlkemCiphertextSize:], second[XwingMlkemCiphertextSize:]) {
		t.Fatalf("the x25519 half of the ciphertext did not come from the supplied reader")
	}
	// and it must be the public key of the first thirty two bytes the reader supplied, which is
	// what says the reader was READ rather than merely accepted. without this, an encapsulation
	// that ignored the reader and drew ek_X from crypto/rand would fail the equality above and
	// nothing would say which of the two halves had moved.
	want, err := x25519PublicKeyOfScalar(fixed[0:XwingX25519KeySize])
	if err != nil {
		t.Fatalf("x25519: %v", err)
	}
	if !bytes.Equal(first[XwingMlkemCiphertextSize:], want) {
		t.Fatalf("ct_X = %x, want the public key of the reader's first 32 bytes %x", first[XwingMlkemCiphertextSize:], want)
	}
}

// An entropy failure at ek_X is returned rather than filled in from the process source. mls's
// own wrapper is what refuses a nil reader, and this is the assertion that XwingEncapsulate does
// not sit in front of it with a fallback of its own.
func TestXwingEncapsulateReportsAFailingRandomSource(t *testing.T) {
	priv, err := XwingGenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if _, _, err := XwingEncapsulate(bytes.NewReader(make([]byte, XwingX25519KeySize-1)), priv.Public()); err == nil {
		t.Fatalf("a reader with too few bytes produced a ciphertext")
	}
	if _, _, err := XwingEncapsulate(nil, priv.Public()); err == nil {
		t.Fatalf("a nil reader produced a ciphertext")
	}
	if _, err := XwingGenerateKey(bytes.NewReader(make([]byte, XwingSeedSize-1))); err == nil {
		t.Fatalf("a short reader produced a key")
	}
}

func TestXwingDecapsulateRejectsWrongCiphertextSizes(t *testing.T) {
	// length before arithmetic: a truncated or over long ciphertext must be refused before
	// ml-kem or x25519 sees a byte.
	priv, err := XwingGenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, n := range []int{0, 1, 1088, 1119, 1121, 2240} {
		if _, err := XwingDecapsulate(priv, make([]byte, n)); !errors.Is(err, ErrXwingBadCiphertextSize) {
			t.Errorf("XwingDecapsulate(%d bytes) error = %v, want ErrXwingBadCiphertextSize", n, err)
		}
	}
}

func TestXwingDecapsulateRejectsALowOrderX25519Half(t *testing.T) {
	// MASTER section 7.2 and spec A section 5.4 mandate an error rather than a zero shared
	// secret.
	priv, err := XwingGenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	ct, _, err := XwingEncapsulate(rand.Reader, priv.Public())
	if err != nil {
		t.Fatalf("encapsulate: %v", err)
	}
	copy(ct[XwingMlkemCiphertextSize:], make([]byte, XwingX25519KeySize))
	ss, err := XwingDecapsulate(priv, ct)
	if !errors.Is(err, ErrXwingInvalidPoint) {
		t.Fatalf("error = %v, want ErrXwingInvalidPoint", err)
	}
	if ss != nil {
		t.Fatalf("returned a shared secret alongside the error: %x", ss)
	}
}

// The same refusal on the encapsulation side, which the plan left unobserved: a peer's key whose
// x25519 half is a low order point must be refused rather than wrapped to under an all zero
// shared secret that the peer chose and can therefore reproduce.
func TestXwingEncapsulateRejectsALowOrderPeerKey(t *testing.T) {
	priv, err := XwingGenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	encoded := priv.Public().Bytes()
	copy(encoded[XwingMlkemPublicKeySize:], make([]byte, XwingX25519KeySize))
	pub, err := ParseXwingPublicKey(encoded)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ct, ss, err := XwingEncapsulate(rand.Reader, pub)
	if !errors.Is(err, ErrXwingInvalidPoint) {
		t.Fatalf("error = %v, want ErrXwingInvalidPoint", err)
	}
	if ct != nil || ss != nil {
		t.Fatalf("returned %d ciphertext bytes and %d shared bytes alongside the error", len(ct), len(ss))
	}
}

func TestXwingDecapsulateUnderTheWrongKeyDiffers(t *testing.T) {
	// ml-kem's implicit rejection means decapsulation under the wrong key succeeds and returns a
	// different secret rather than failing. that is correct FIPS 203 behaviour, and it means a
	// wrap that opens to the wrong key must be caught by the AEAD above it, not by an error
	// here.
	a, err := XwingGenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate a: %v", err)
	}
	b, err := XwingGenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate b: %v", err)
	}
	ct, ss, err := XwingEncapsulate(rand.Reader, a.Public())
	if err != nil {
		t.Fatalf("encapsulate: %v", err)
	}
	other, err := XwingDecapsulate(b, ct)
	if err != nil {
		t.Fatalf("decapsulate under the wrong key returned an error: %v", err)
	}
	if bytes.Equal(ss, other) {
		t.Fatalf("decapsulation under the wrong key produced the right secret")
	}
}

// Every single octet of the ciphertext reaches the shared secret. It is the sweep that says the
// ct split is the draft's and not an off by one: a decapsulation that read ct[0:1087] as the
// ML-KEM half, or that combined ct_X from the wrong window, leaves at least one octet with no
// effect on the answer, and no round trip and no wrong key test can see that.
func TestEveryCiphertextOctetChangesTheSharedSecret(t *testing.T) {
	priv, err := XwingGenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	ct, ss, err := XwingEncapsulate(rand.Reader, priv.Public())
	if err != nil {
		t.Fatalf("encapsulate: %v", err)
	}
	for i := range ct {
		mutated := append([]byte{}, ct...)
		mutated[i] ^= 0x01
		other, err := XwingDecapsulate(priv, mutated)
		if err != nil {
			// only the x25519 half can refuse, and only for a low order point
			if i < XwingMlkemCiphertextSize {
				t.Fatalf("flipping ciphertext octet %d produced an error: %v", i, err)
			}
			continue
		}
		if bytes.Equal(ss, other) {
			t.Errorf("flipping ciphertext octet %d left the shared secret unchanged", i)
		}
	}
}
