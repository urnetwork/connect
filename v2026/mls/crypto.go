// The whole cryptographic surface of the implementation, in one interface, so an audit
// reads one file and a test can substitute a deterministic instance for all of it at
// once.
//
// Every length comes from the suite parameters rather than from a literal, so a suite
// added later cannot inherit the first one's key size. The hash, the mac and the kdf are
// the deliberate exception and are written against sha256 directly: both registered
// suites name HKDF-SHA256, sha256.Sum256 has no runtime selection to make, and a suite
// naming another hash would need a different implementation rather than a different
// constant here. The guard for that reads the registry rather than this file — see
// TestEverySuiteNamesTheHashTheProviderComputes — so a third suite whose kdf is not
// HKDF-SHA256 fails there instead of quietly deriving every secret under the wrong hash.
//
// crypto/hkdf.Extract takes the input keying material first and the salt second, the
// reverse of the HKDF-Extract(salt, ikm) that RFC 9420, RFC 9180 and every spec text in
// this project write. The swap lives here and in hpke.go and nowhere else (spec A
// section 5.9, guardrail 1), which is exactly what crypto_forbidden_test.go holds to
// these two file names.
//
// The provider NewCryptoProvider returns is stateless and therefore safe for concurrent
// use (spec A section 3.6): it holds the suite parameters and crypto/rand.Reader, and
// both are. A provider built by NewCryptoProviderWithRandom is exactly as concurrency
// safe as the reader it was handed, and the scripted readers the tests use are not —
// they are driven from one goroutine.
package mls

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"io"
	"strconv"
)

// A serialized ed25519 public key, as MLS carries it: the raw 32 bytes, no prefix.
type SignaturePublicKey []byte

// A serialized ed25519 private key, which is the RFC 8032 seed — 32 bytes, not go's 64
// byte seed followed by public key — because that is the form the RFC 9420 crypto-basics
// vectors carry and the form a caller storing one has to round trip. Named distinctly
// from the public half so a signature cannot silently take one where the other belongs.
type SignaturePrivateKey []byte

// Every cryptographic operation the MLS implementation performs. Nothing outside the
// four files named in doc.go computes anything, so an implementation of this interface
// is the entire trusted surface.
//
// Three of the methods have no error return, which is a contract rather than an
// oversight. Extract cannot fail over a compiled in sha256. Expand and Random can, but
// only on inputs that are this process's own bug — a length past the kdf's ceiling, an
// entropy source that stopped — and the honest answer to those is to stop rather than to
// hand back a short key that looks usable. Both panic; see their implementations.
type CryptoProvider interface {
	Suite() CipherSuite
	HashSize() int
	KeySize() int
	NonceSize() int
	Hash(data []byte) []byte
	Mac(key []byte, data []byte) []byte
	MacVerify(key []byte, data []byte, tag []byte) bool
	// Extract takes the salt first, matching the spec text rather than the library.
	Extract(salt []byte, ikm []byte) []byte
	Expand(prk []byte, info []byte, length int) []byte
	ExpandWithLabel(secret []byte, label string, context []byte, length int) []byte
	DeriveSecret(secret []byte, label string) []byte
	DeriveTreeSecret(secret []byte, label string, generation uint32, length int) []byte
	AeadSeal(key []byte, nonce []byte, aad []byte, plaintext []byte) ([]byte, error)
	AeadOpen(key []byte, nonce []byte, aad []byte, ciphertext []byte) ([]byte, error)
	SignWithLabel(priv SignaturePrivateKey, label string, content []byte) ([]byte, error)
	VerifyWithLabel(pub SignaturePublicKey, label string, content []byte, sig []byte) error
	HpkeSeal(pub HpkePublicKey, info []byte, aad []byte, plaintext []byte) (kemOutput []byte, ciphertext []byte, err error)
	HpkeOpen(priv HpkePrivateKey, kemOutput []byte, info []byte, aad []byte, ciphertext []byte) ([]byte, error)
	DeriveKeyPair(ikm []byte) (HpkePrivateKey, HpkePublicKey, error)
	SignatureKeyPair() (SignaturePrivateKey, SignaturePublicKey, error)
	Random(n int) []byte
}

// The one implementation: a suite's parameters and the source its keys are drawn from,
// and no other state, which is what makes it safe to share.
type suiteCryptoProvider struct {
	params *SuiteParams
	random io.Reader
}

// Drift between the interface and the concrete type fails at build rather than at the
// first caller, which matters most while tasks 12 to 16 are still filling the type in.
var _ CryptoProvider = (*suiteCryptoProvider)(nil)

// A provider over the process entropy source. An unregistered code point is refused
// rather than defaulted, and nothing is returned alongside the error: a provider with no
// parameters would answer every size question with zero.
func NewCryptoProvider(suite CipherSuite) (CryptoProvider, error) {
	return NewCryptoProviderWithRandom(suite, rand.Reader)
}

// A provider over a caller's entropy source. The interop forge and the negative path
// tests need a provider whose randomness they control, so a failing case reproduces byte
// for byte instead of being observed once — and so a test can assert that what draws
// randomness consumes exactly the bytes it was offered, in order, which is the property
// a constant reader cannot see. Production callers use NewCryptoProvider and get
// crypto/rand; nothing in this package reads crypto/rand behind a caller's back.
//
// No source at all is refused here rather than at the first draw, and it is refused rather
// than filled in. A provider that answered a nil reader with the process source would keep
// this signature and break the sentence above silently: every key it produced would be a
// good key, every round trip would still round trip, and the interop case the caller was
// pinning would stop reproducing with nothing to say why. Refusing at construction is what
// makes that a caller's error to handle rather than three separate draws to get right —
// see TestEveryEntropyTakingFunctionRefusesANilSource, which reads the class off the parse
// tree because this package has already shipped that substitution once.
func NewCryptoProviderWithRandom(suite CipherSuite, random io.Reader) (CryptoProvider, error) {
	if random == nil {
		return nil, ErrNilRandomSource
	}
	params, err := LookupSuite(suite)
	if err != nil {
		return nil, err
	}
	return &suiteCryptoProvider{params: params, random: random}, nil
}

// The code point this provider was built for, so a caller holding only the interface can
// still name the suite it is bound to.
func (self *suiteCryptoProvider) Suite() CipherSuite { return self.params.Suite }

// Nh, the kdf output length.
func (self *suiteCryptoProvider) HashSize() int { return self.params.Nh }

// Nk, the aead key length, which is the field the two registered suites disagree on.
func (self *suiteCryptoProvider) KeySize() int { return self.params.Nk }

// Nn, the aead nonce length.
func (self *suiteCryptoProvider) NonceSize() int { return self.params.Nn }

// The suite's hash over the whole message. Unkeyed and unsalted: RFC 9420 uses it for
// references and transcripts, where both sides must agree without sharing anything.
func (self *suiteCryptoProvider) Hash(data []byte) []byte {
	digest := sha256.Sum256(data)
	return digest[:]
}

// The suite's mac, which for both registered suites is HMAC-SHA256. The key is a real
// input: a mac computed without it still verifies against itself and authenticates
// nobody, so the published RFC 4231 answers rather than a round trip are what hold this.
func (self *suiteCryptoProvider) Mac(key []byte, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

// Whether a tag is the one this key and data produce, compared in constant time
// (guardrail 8). The length mismatch is refused ahead of the comparison and is not a
// leak: the length of a tag is public, and the alternative — comparing a prefix — would
// accept every truncation of a valid tag.
func (self *suiteCryptoProvider) MacVerify(key []byte, data []byte, tag []byte) bool {
	expected := self.Mac(key, data)
	if len(tag) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare(expected, tag) == 1
}

// HKDF-Extract with the arguments in the order the spec text writes them: the salt is
// the hmac key and the input keying material is the message. crypto/hkdf.Extract is the
// other way round, and the two are indistinguishable except against a published
// pseudorandom key, which is why RFC 5869's own table is what this is tested with.
//
// The only error crypto/hkdf.Extract returns comes from the fips140-only mode check,
// which refuses an unapproved hash or a short secret. sha256 is approved, so under this
// package's build it cannot fire — and it is a panic rather than an ignored error
// because a caller handed a nil pseudorandom key would derive every subsequent secret
// from nothing and report success.
func (self *suiteCryptoProvider) Extract(salt []byte, ikm []byte) []byte {
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		panic("mls: hkdf extract failed with a compiled-in sha256: " + err.Error())
	}
	return prk
}

// HKDF-Expand over the suite's kdf. The info argument of crypto/hkdf.Expand is typed
// string but is not text; the conversion is byte preserving.
//
// A length outside the kdf's range is a caller's bug rather than a runtime condition:
// the interface spec A section 3.3 fixes has no error return here, and every call site
// in this package asks for a length its suite fixes. Returning fewer bytes than asked
// for would be a silent downgrade, so this stops instead.
//
// The gate is written here because crypto/hkdf.Expand has no low side of its own:
// crypto/internal/fips140/hkdf opens with make([]byte, 0, keyLen), reached before the
// expansion loop, so a negative length is a makeslice panic from inside the library
// rather than an error. The high side is the library's own 255*Nh, which hpke.go names
// as hpkeMaxExpandLength and TestHpkeExpandCeilingIsTheLibrarysOwnBoundary pins against
// crypto/hkdf itself rather than against either file's opinion of it.
//
// A pseudorandom key shorter than the hash is the same downgrade arriving from the other
// side. RFC 5869 section 2.3 requires one of at least HashLen, crypto/hkdf enforces it in
// fips140-only mode and nowhere else, and expanding from a short one derives every
// subsequent secret from less entropy than the suite claims while handing back the full
// count of bytes asked for. A caller that arrives here with one has a bug no length check
// downstream can see, so this stops as well.
func (self *suiteCryptoProvider) Expand(prk []byte, info []byte, length int) []byte {
	if length < 0 || length > hpkeMaxExpandLength {
		panic("mls: hkdf expand length " + strconv.Itoa(length) + " is outside 0.." + strconv.Itoa(hpkeMaxExpandLength))
	}
	if len(prk) < self.params.Nh {
		panic("mls: hkdf expand pseudorandom key of " + strconv.Itoa(len(prk)) +
			" bytes is shorter than the " + strconv.Itoa(self.params.Nh) + " byte hash")
	}
	out, err := hkdf.Expand(sha256.New, prk, string(info), length)
	if err != nil {
		panic("mls: hkdf expand refused the requested length: " + err.Error())
	}
	return out
}

// The suite's aead over a key and a nonce of exactly the lengths it fixes. The key is
// checked by hpkeNewAead, which reads the registry's Nk, and the nonce is checked here
// against Nn rather than against the aead's own opinion of a nonce size, so a suite whose
// Nn disagreed with its aead is caught at the key schedule that built the nonce.
//
// The destination is nil rather than the plaintext's own storage. cipher.AEAD permits an
// exact overlap, so sealing into plaintext[:0] would compile and round trip while
// destroying a caller's buffer — and a caller sealing the same plaintext to a second
// recipient would then encrypt whatever was left behind.
func (self *suiteCryptoProvider) AeadSeal(key []byte, nonce []byte, aad []byte, plaintext []byte) ([]byte, error) {
	aead, err := hpkeNewAead(self.params, key)
	if err != nil {
		return nil, err
	}
	if len(nonce) != self.params.Nn {
		return nil, ErrBadNonceLength
	}
	return aead.Seal(nil, nonce, plaintext, aad), nil
}

// The inverse, with every failure reported as one sentinel. Which of the key, the nonce,
// the aad or the ciphertext was wrong is not something an attacker gets to learn from an
// error, and a caller cannot act on the difference either.
//
// The nil plaintext on the failure path is load bearing. An authentic message with an
// empty plaintext also comes back as a nil slice, so the error is the only thing that
// separates the two, and a caller that reads the slice instead of the error accepts every
// forgery of exactly tag length.
func (self *suiteCryptoProvider) AeadOpen(key []byte, nonce []byte, aad []byte, ciphertext []byte) ([]byte, error) {
	aead, err := hpkeNewAead(self.params, key)
	if err != nil {
		return nil, err
	}
	if len(nonce) != self.params.Nn {
		return nil, ErrBadNonceLength
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, ErrAeadOpen
	}
	return plaintext, nil
}

// n bytes from this provider's source, all of them read from it and none of them
// invented. io.ReadFull rather than Read because a source that hands back one byte at a
// time is a correct io.Reader and a bare Read would leave the rest of the buffer as the
// zeroes it was allocated with.
//
// A failing or exhausted source panics. The interface has no error return here, and the
// two alternatives — fewer bytes than were asked for, or a tail of zeroes where entropy
// belongs — are both key material that looks usable and is not.
func (self *suiteCryptoProvider) Random(n int) []byte {
	b := make([]byte, n)
	if _, err := io.ReadFull(self.random, b); err != nil {
		panic("mls: the random source failed: " + err.Error())
	}
	return b
}

// ExpandWithLabel, DeriveSecret and DeriveTreeSecret are task 12's, and SignWithLabel,
// VerifyWithLabel and SignatureKeyPair are task 14's; all six live in crypto_labels.go,
// beside the other RFC 9420 labelled constructions. EncryptWithLabel and DecryptWithLabel
// live there too, and reach hpke through the three methods below.
//
// The RFC 9180 base mode single shot, as the interface's three hpke methods. Each is the
// whole of its operation and none of them holds anything: hpke.go builds a context per
// call for the reason recorded there, and a memoized context or a cached ephemeral key
// here would put a second message on the first message's key at the first message's
// sequence number.
//
// The seal's ephemeral key is drawn from self.random, which is the same source
// SignatureKeyPair reads and for the same three reasons. NewCryptoProviderWithRandom
// promises above that nothing in this package reads crypto/rand behind a caller's back, and
// a seal that reached for rand.Reader would be that promise broken by the only other method
// here which draws anything. A provider built over a caller's stream is what lets an
// interop failure reproduce byte for byte, and an encapsulation is the one part of a
// labelled encryption that is not a function of its inputs, so it is the part that has to
// come from somewhere a caller can pin. And it is what makes EncryptWithLabel observable at
// all: over a fixed stream two calls agree, so a construction that ignored the provider it
// was handed answers differently, which is what the routing and immutability gates compare.
//
// Production is unchanged: NewCryptoProvider hands this crypto/rand.Reader.
func (self *suiteCryptoProvider) HpkeSeal(pub HpkePublicKey, info []byte, aad []byte, plaintext []byte) ([]byte, []byte, error) {
	return HpkeSealBase(self.random, self.params, pub, info, aad, plaintext)
}

// The receiving half. There is no fallback: an open that retried under a different info
// after this one accepts a message sealed for another purpose, and hpke's info is exactly
// what the labelled constructions in crypto_labels.go bind their label and context through.
func (self *suiteCryptoProvider) HpkeOpen(priv HpkePrivateKey, kemOutput []byte, info []byte, aad []byte, ciphertext []byte) ([]byte, error) {
	return HpkeOpenBase(self.params, priv, kemOutput, info, aad, ciphertext)
}

// DeriveKeyPair over the suite's kem, which is deterministic in its input keying material
// and draws no randomness of its own — so a caller who wants a fresh key pair brings a
// fresh ikm, from Random or from anywhere else it trusts.
func (self *suiteCryptoProvider) DeriveKeyPair(ikm []byte) (HpkePrivateKey, HpkePublicKey, error) {
	return HpkeDeriveKeyPair(self.params, ikm)
}
