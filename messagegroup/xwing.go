// X-Wing, the hybrid key encapsulation of draft-connolly-cfrg-xwing-kem, which is X25519 and
// ML-KEM-768 combined under a construction that carries a published security proof.
//
// It is transcribed from the draft rather than reconstructed from a description of it. A
// roughly equivalent combiner forfeits the proof that is the whole reason for choosing X-Wing
// over a hand rolled concatenation, and the failure mode is silent: the label, its position and
// the order of the combiner's inputs can each be wrong while the result is still thirty two
// bytes, still uniform looking and still agreed on by both ends, because both ends are this
// file. That is why xwing_vectors_test.go holds the construction against the draft's own
// published answers in both directions before anything in this package uses it, and why the
// combiner writes its five inputs in the order the draft prints and not in the order this file
// happens to have them in hand.
//
// ML-KEM-768 and not 1024 is deliberate: a construction with a proof at level 3 is worth more
// than a hand rolled one at level 5, and the draft is specified at 768.
//
// Standard library only. crypto/sha3 supplies the SHAKE-256 seed expansion and the SHA3-256
// combiner, crypto/mlkem supplies ML-KEM-768 with the seed construction this needs, and the
// x25519 half goes through mls's one ECDH wrapper rather than through crypto/ecdh directly,
// which is what keeps the single reviewed call site the forbidden primitive gate asserts.
//
// WHAT THIS FILE ERASES AND WHAT IT CANNOT, because a producer that leaves its own copies live
// makes every erase downstream of it worth less than it reads. Six values here are secret, live at
// return and unreachable to the caller, and each is handed to zeroize in the body that derived it:
// the ninety six octet seed expansion, the fresh seed XwingGenerateKey draws, and ss_M and ss_X on
// both sides of the KEM. Three residues remain and are named rather than papered over -- the
// ephemeral x25519 scalar inside crypto/ecdh's *PrivateKey, the SHA3-256 sponge the combiner
// writes into, and the d | z and scalar copies inside the parsed *mlkem.DecapsulationKey768 and
// *ecdh.PrivateKey -- because none of the three is a []byte this package holds a header over, and
// zeroize can only reach an array it is handed. Open item M1-44's line: this removes the obvious
// copy, not every copy.
//
// One deliberate divergence from the draft, stated here because it is a divergence. The draft
// puts no check on the x25519 shared secret. crypto/ecdh refuses an all zero one, mls's wrapper
// turns that refusal into an error, and this file surfaces it as ErrXwingInvalidPoint rather
// than combining a secret the peer chose. Spec A section 5.4 requires exactly that, and it costs
// nothing in conformance: no draft vector carries a low order ct_X, X-Wing is used only in this
// project's own storage layer, and there is no peer implementation to diverge from.
package messagegroup

import (
	"crypto/ecdh"
	"crypto/mlkem"
	"crypto/sha3"
	"io"

	"github.com/urnetwork/connect/mls"
)

// The sizes draft-connolly-cfrg-xwing-kem section 5.1 gives, each named for the thing it
// measures.
//
// The seed and the ML-KEM seed are the pair worth naming apart. The storable X-Wing private key
// is thirty two bytes and the ML-KEM seed it expands to is sixty four, and the two are one
// SHAKE-256 call away from each other, so a literal 32 in the expansion is a key generation that
// still runs, still produces a well formed key pair and agrees with no other implementation.
// Naming both means that mistake has to be made deliberately.
const (
	XwingSeedSize            = 32
	XwingExpandedSize        = 96
	XwingPublicKeySize       = 1216
	XwingCiphertextSize      = 1120
	XwingSharedSize          = 32
	XwingMlkemSeedSize       = 64
	XwingMlkemPublicKeySize  = 1184
	XwingMlkemCiphertextSize = 1088
	XwingX25519KeySize       = 32
)

// The wrap algorithm identifier MASTER section 7.1 registers for X-Wing, carried inside every
// signed body so that it cannot be stripped or downgraded on the way.
const XwingAlgId uint16 = 0x0014

// The 1216 and the 0x0014 are each stated twice in this tree -- once in mls, where LeafNode
// validation range checks a device's wrap key, and once here, where the KEM that produces it
// lives -- because mls must never import messagegroup. The duplication is deliberate and one
// directional; these four lines are what stop it becoming a drift.
//
// An array length is a non negative constant, so declaring the difference in both directions
// fails to BUILD unless the two agree, which is the point: a constant that disagrees across a
// package boundary is otherwise discovered by a device nothing can wrap to, in a group nobody
// can close, rather than by a compiler.
var (
	_ [XwingPublicKeySize - mls.XwingPublicKeyLen]struct{}
	_ [mls.XwingPublicKeyLen - XwingPublicKeySize]struct{}
	_ [XwingAlgId - mls.AlgIdXwing]struct{}
	_ [mls.AlgIdXwing - XwingAlgId]struct{}
)

// XWingLabel, the six ascii bytes 5c 2e 2f 2f 5e 5c, which spell an ascii art X.
//
// It is the LAST input to the combiner and not the first. Spec A section 5.4's stdlib mapping
// table writes it first; that is an error in spec A, and the label first form produces thirty
// two perfectly good looking bytes that match none of the draft's three vectors. Nothing but
// those vectors can tell the two apart.
var xwingLabel = []byte{0x5c, 0x2e, 0x2f, 0x2f, 0x5e, 0x5c}

// The storable private key is the thirty two byte seed and nothing else, which is what makes
// seed only restore possible: the recovery key is reconstructible from the mnemonic alone,
// MASTER section 5.2. The expanded halves are cached beside it so that a decapsulation does not
// re run SHAKE-256 and ML-KEM key generation on every wrap it opens.
type XwingPrivateKey struct {
	seed            []byte
	mlkemPrivate    *mlkem.DecapsulationKey768
	x25519Private   *ecdh.PrivateKey
	mlkemPublicKey  []byte
	x25519PublicKey []byte
}

// The parsed halves of an encapsulation key, kept beside the octets they were parsed out of so
// that the combiner has pk_X in hand without re serializing it.
type XwingPublicKey struct {
	mlkemPublic  *mlkem.EncapsulationKey768
	mlkemBytes   []byte
	x25519Public *ecdh.PublicKey
	x25519Bytes  []byte
}

// XwingKeyGenFromSeed is the draft's section 5.2 expansion, in full:
//
//	expanded = SHAKE256(seed, 96)
//	(pk_M, sk_M) = ML-KEM-768.KeyGen_internal(expanded[0:32], expanded[32:64])
//	sk_X = expanded[64:96]
//
// crypto/mlkem takes the d and z halves as one sixty four byte seed, so expanded[0:64] is handed
// to it whole rather than split here. Taking thirty two bytes there instead of sixty four is the
// hazard the constants above are named for.
func XwingKeyGenFromSeed(seed []byte) (*XwingPrivateKey, error) {
	if len(seed) != XwingSeedSize {
		return nil, ErrXwingBadSeedSize
	}
	expanded := sha3.SumSHAKE256(seed, XwingExpandedSize)
	// THE EXPANSION IS THE WHOLE PRIVATE KEY IN OCTETS -- d | z | sk_X -- and it is erased in the
	// body that derived it, which is the discipline wrap.go keeps one file over. Nothing the caller
	// is handed points at this array: crypto/mlkem copies d and z into the decapsulation key by
	// value and crypto/ecdh clones the scalar, so an erase after both constructors have run blanks
	// a copy no other header reaches. Without it every key generation leaves ninety six octets of
	// private key in the heap for the collector to move around -- and sdk's DecapsulateToOwnLeaf
	// re-expands this device's key on EVERY wrap it opens, which is what made this the copy that
	// mattered.
	defer zeroize(expanded)
	mlkemPrivate, err := mlkem.NewDecapsulationKey768(expanded[0:XwingMlkemSeedSize])
	if err != nil {
		return nil, err
	}
	x25519Private, err := mls.X25519PrivateKey(expanded[XwingMlkemSeedSize:XwingExpandedSize])
	if err != nil {
		return nil, err
	}
	return &XwingPrivateKey{
		// a copy, so a caller that zeroizes the buffer it handed in does not blank the key
		// this returned
		seed:            append([]byte{}, seed...),
		mlkemPrivate:    mlkemPrivate,
		x25519Private:   x25519Private,
		mlkemPublicKey:  mlkemPrivate.EncapsulationKey().Bytes(),
		x25519PublicKey: x25519Private.PublicKey().Bytes(),
	}, nil
}

// XwingGenerateKey draws a fresh seed out of random and expands it.
//
// The reader is honoured rather than defaulted: an entropy failure is returned unwrapped,
// because a failing randomness source is neither a seed length nor a curve problem and must not
// be reported as one.
//
// A nil reader is REFUSED and never filled in from the process source, which is the position
// mls.X25519GenerateKey argues at length and this is the second entropy taking function in the
// tree to take it. It answers mls's own sentinel rather than declaring a second name for one
// condition, so a caller holding either package can match the refusal with one errors.Is. Without
// the guard io.ReadFull dereferences the nil interface and this function takes the caller's
// process down instead of its call.
func XwingGenerateKey(random io.Reader) (*XwingPrivateKey, error) {
	if random == nil {
		return nil, mls.ErrNilRandomSource
	}
	seed := make([]byte, XwingSeedSize)
	// the draw is a second copy of the private key and the constructor below takes its own, so
	// this one is erased here rather than dropped. A caller cannot reach it to erase it -- it is
	// never returned and never stored -- which is the whole class of residue this file was
	// leaving: a value the producer holds live past the point every consumer of it has copied.
	defer zeroize(seed)
	if _, err := io.ReadFull(random, seed); err != nil {
		return nil, err
	}
	return XwingKeyGenFromSeed(seed)
}

// A copy, for the reason the constructor takes one: a caller that zeroizes what it is handed
// must not blank the key it came from.
func (self *XwingPrivateKey) Seed() []byte {
	return append([]byte{}, self.seed...)
}

func (self *XwingPrivateKey) Public() *XwingPublicKey {
	return &XwingPublicKey{
		mlkemPublic:  self.mlkemPrivate.EncapsulationKey(),
		mlkemBytes:   self.mlkemPublicKey,
		x25519Public: self.x25519Private.PublicKey(),
		x25519Bytes:  self.x25519PublicKey,
	}
}

// pk_M then pk_X, a fresh slice on every call so that a caller mutating what it got back cannot
// corrupt the key it got it from, nor any other slice this package handed out.
func (self *XwingPublicKey) Bytes() []byte {
	encoded := make([]byte, 0, XwingPublicKeySize)
	encoded = append(encoded, self.mlkemBytes...)
	return append(encoded, self.x25519Bytes...)
}

// ParseXwingPublicKey reads an encapsulation key off the wire. The length is settled before any
// parsing, so a truncated or over long key never reaches ML-KEM's own decoder and the refusal
// names the X-Wing key rather than one of its halves.
func ParseXwingPublicKey(b []byte) (*XwingPublicKey, error) {
	if len(b) != XwingPublicKeySize {
		return nil, ErrXwingBadPublicKeySize
	}
	mlkemPublic, err := mlkem.NewEncapsulationKey768(b[0:XwingMlkemPublicKeySize])
	if err != nil {
		return nil, err
	}
	x25519Public, err := mls.X25519PublicKey(b[XwingMlkemPublicKeySize:])
	if err != nil {
		return nil, err
	}
	return &XwingPublicKey{
		mlkemPublic: mlkemPublic,
		// copies, so the caller's buffer and this key are independent in both directions
		mlkemBytes:   append([]byte{}, b[0:XwingMlkemPublicKeySize]...),
		x25519Public: x25519Public,
		x25519Bytes:  append([]byte{}, b[XwingMlkemPublicKeySize:]...),
	}, nil
}

// The combiner, draft section 5.3, which is SHA3-256 over ss_M, then ss_X, then ct_X, then pk_X,
// then the label.
//
// Five inputs in one fixed order under one fixed label, and every one of the mistakes available
// here -- the two shared secrets swapped, ct_X and pk_X swapped, the label moved to the front --
// produces thirty two bytes that round trip against this same function. Only the draft's
// published answers separate them, which is why this function is reached by the vector gate
// before it is reached by anything else.
func xwingCombine(mlkemShared []byte, x25519Shared []byte, x25519Ciphertext []byte, x25519Public []byte) []byte {
	hash := sha3.New256()
	hash.Write(mlkemShared)
	hash.Write(x25519Shared)
	hash.Write(x25519Ciphertext)
	hash.Write(x25519Public)
	hash.Write(xwingLabel)
	return hash.Sum(nil)
}

// XwingEncapsulate is the draft's section 5.4, and the randomness it takes is half the story.
//
// random supplies ek_X and nothing else. crypto/mlkem's Encapsulate takes no randomness and
// returns no error, so the ML-KEM half always draws from crypto/rand and this function cannot be
// derandomized whatever reader it is handed. That is asserted by
// TestXwingEncapsulateIsNotDerandomizable rather than left as a sentence here, because a caller
// who assumed otherwise would build a known answer test that runs, passes, and proves nothing.
func XwingEncapsulate(random io.Reader, pub *XwingPublicKey) ([]byte, []byte, error) {
	ephemeral, err := mls.X25519GenerateKey(random)
	if err != nil {
		return nil, nil, err
	}
	x25519Shared, err := mls.X25519DH(ephemeral, pub.x25519Public)
	if err != nil {
		return nil, nil, ErrXwingInvalidPoint
	}
	// THE TWO HALVES ARE ERASED HERE AND NOT BY THE CALLER, because the caller never sees them:
	// what leaves this function is the combiner's output, and ss_M and ss_X stay behind. A door
	// that erases only what it is handed erases one copy of a secret its producer already left
	// live. The two together ARE the combiner's input once ct_X, pk_X and the label -- all three
	// public -- are put beside them, so leaving both live hands anybody reading this heap the
	// shared secret itself, and leaving one live collapses the hybrid onto the other half. The
	// defers run after the return value has been computed, so the combiner has read both before
	// either is blanked.
	defer zeroize(x25519Shared)
	mlkemShared, mlkemCiphertext := pub.mlkemPublic.Encapsulate()
	defer zeroize(mlkemShared)
	x25519Ciphertext := ephemeral.PublicKey().Bytes()

	ciphertext := make([]byte, 0, XwingCiphertextSize)
	ciphertext = append(ciphertext, mlkemCiphertext...)
	ciphertext = append(ciphertext, x25519Ciphertext...)
	shared := xwingCombine(mlkemShared, x25519Shared, x25519Ciphertext, pub.x25519Bytes)
	return ciphertext, shared, nil
}

// XwingDecapsulate is the draft's section 5.5.
//
// The length is settled before any arithmetic, so a truncated or over long ciphertext never
// reaches ML-KEM's decoder, and past that point nothing here branches on anything an attacker
// controls the secret half of. ML-KEM's implicit rejection is what makes that possible: a
// ciphertext that was not produced for this key decapsulates successfully to a pseudorandom
// secret rather than failing, so there is no success flag to leak and no oracle to query. The
// only error reachable after the length check is the x25519 half's, and it is a statement about
// a public value the sender chose rather than about this key's secret.
func XwingDecapsulate(priv *XwingPrivateKey, ct []byte) ([]byte, error) {
	if len(ct) != XwingCiphertextSize {
		return nil, ErrXwingBadCiphertextSize
	}
	mlkemCiphertext := ct[0:XwingMlkemCiphertextSize]
	x25519Ciphertext := ct[XwingMlkemCiphertextSize:]

	mlkemShared, err := priv.mlkemPrivate.Decapsulate(mlkemCiphertext)
	if err != nil {
		// unreachable past the length check above, and returned rather than dropped because an
		// ml-kem that grew a second refusal must not have it turned into a shared secret here
		return nil, err
	}
	// the opening side of XwingEncapsulate's erase, for the same reason and with the same
	// ordering: the caller is handed the combiner's output and never these two, so this body is
	// the only one that can blank them.
	defer zeroize(mlkemShared)
	ephemeralPublic, err := mls.X25519PublicKey(x25519Ciphertext)
	if err != nil {
		return nil, ErrXwingInvalidPoint
	}
	x25519Shared, err := mls.X25519DH(priv.x25519Private, ephemeralPublic)
	if err != nil {
		return nil, ErrXwingInvalidPoint
	}
	defer zeroize(x25519Shared)
	return xwingCombine(mlkemShared, x25519Shared, x25519Ciphertext, priv.x25519PublicKey), nil
}
