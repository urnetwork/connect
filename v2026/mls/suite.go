// The ciphersuite registry. RFC 9420 section 5 fixes a ciphersuite as the tuple of an
// hpke kem, kdf and aead, a hash, a mac and a signature scheme, and this file is the
// only place that tuple is written down. Every length check in the package reads a
// field of it rather than a literal, so a suite added later cannot leave a hardcoded
// 32 behind in code that was only ever exercised at 32.
//
// Two suites are registered on purpose. A registry holding one entry and a hardcoded
// constant are indistinguishable by test, and the difference only surfaces when a
// second suite arrives — which the still draft post quantum MLS ciphersuites make a
// near certainty. 0x0001 is implemented and vector tested here; the policy check in
// group.go refuses it at group creation, so nothing on the wire changes. The refusal
// belongs there rather than here because a registry that omitted the suite could not
// verify its vectors, and an implementation nobody exercises is one that silently
// rots until the day it is needed.
//
// The registry is closed: there is no registration entry point, so the set of suites
// is fixed at compile time and an attacker cannot reach a code path that adds one.
package mls

import "slices"

// A code point from the RFC 9420 section 17.1 ciphersuite registry. The two the
// package implements are below; every other value is unregistered and is refused
// rather than defaulted, since defaulting would let a peer's unsupported suite be
// silently downgraded to one it never asked for.
type CipherSuite uint16

const (
	CipherSuiteX25519AesGcm128Sha256Ed25519 CipherSuite = 0x0001
	CipherSuiteX25519ChaCha20Sha256Ed25519  CipherSuite = 0x0003
)

// Hpke algorithm identifiers, RFC 9180 sections 7.1 to 7.3. Three registries, three
// types: the kdf HKDF-SHA256 and the aead AES-128-GCM are both 0x0001, in two separate
// registries, so on a shared uint16 they compare equal and a declaration that wrote one
// where the other belongs compiled and satisfied every value assertion in this package.
// Declared distinct, that transposition is a compile error instead.
//
// The limit is worth stating, because the types do not say it. hpke.go writes these
// through binary.BigEndian.AppendUint16, which takes a uint16, and the explicit
// conversion that demands is exactly where the typing is discarded — so
// uint16(params.AeadId) in the kdf position of hpkeSuiteId still compiles. These types
// close the registry declaration hole; the encoder hole stays closed by the appendix A
// vectors for both suites, where 0x0003 disagrees on the aead and moves every byte.
type (
	HpkeKemId  uint16
	HpkeKdfId  uint16
	HpkeAeadId uint16
)

const (
	HpkeKemX25519HkdfSha256  HpkeKemId  = 0x0020
	HpkeKdfHkdfSha256        HpkeKdfId  = 0x0001
	HpkeAeadAes128Gcm        HpkeAeadId = 0x0001
	HpkeAeadChaCha20Poly1305 HpkeAeadId = 0x0003
)

// Signature scheme identifier as MLS carries it, from the TLS SignatureScheme
// registry of RFC 8446 section 4.2.3.
const SignatureSchemeEd25519 uint16 = 0x0807

// The sizes and algorithm identifiers a suite fixes. Nh is the kdf output, Nk, Nn and
// Nt the aead key, nonce and tag, Nsecret, Nenc, Npk and Nsk the kem shared secret,
// encapsulated key, public key and private key, and NsigPub and NsigPriv the
// signature keys — where the private form is the 32 byte ed25519 seed rather than the
// 64 byte expanded key, because that is what the crypto-basics vectors carry.
//
// Every field is comparable, so a test can assert a whole entry in one comparison and
// a field added later cannot slip past an assertion that names fields one by one.
type SuiteParams struct {
	Suite       CipherSuite
	Name        string
	KemId       HpkeKemId
	KdfId       HpkeKdfId
	AeadId      HpkeAeadId
	SignatureId uint16
	Nh          int
	Nk          int
	Nn          int
	Nt          int
	Nsecret     int
	Nenc        int
	Npk         int
	Nsk         int
	NsigPub     int
	NsigPriv    int
	// Nsig is the WIDTH OF A SIGNATURE this suite's scheme produces, which is not a key width
	// and is therefore not NsigPub under another name: ed25519 has 32 octet keys and a 64 octet
	// signature.
	//
	// It is here because MASTER section 8.4.6 requires a sealer to compute framed_length BEFORE
	// it reserves a stream index, and the signature is a term of that length -- an application
	// frame's plaintext is the content arm, then opaque signature<V>, then padding. That section
	// requires the length to be DERIVED from the ciphersuite, the group id's width, the
	// signature's width and the 32 octet aad_mls, and never hard coded, so the signature's width
	// has to be a suite parameter rather than a literal 64 sitting beside the arithmetic.
	//
	// Nothing lets the registry disagree with the scheme:
	// TestTheRegisteredSignatureWidthIsTheWidthTheProviderProduces signs with each registered
	// suite's own provider and compares.
	Nsig int
}

// The whole registry. The two entries differ in their aead and therefore in Nk, which
// is what makes a lookup that discriminates distinguishable from one that returns
// whatever it has; they agree on Nn and Nt because chacha20-poly1305 and aes-128-gcm
// happen to share a 12 byte nonce and a 16 byte tag.
var registeredSuiteParams = map[CipherSuite]SuiteParams{
	CipherSuiteX25519AesGcm128Sha256Ed25519: {
		Suite:       CipherSuiteX25519AesGcm128Sha256Ed25519,
		Name:        "MLS_128_DHKEMX25519_AES128GCM_SHA256_Ed25519",
		KemId:       HpkeKemX25519HkdfSha256,
		KdfId:       HpkeKdfHkdfSha256,
		AeadId:      HpkeAeadAes128Gcm,
		SignatureId: SignatureSchemeEd25519,
		Nh:          32,
		Nk:          16,
		Nn:          12,
		Nt:          16,
		Nsecret:     32,
		Nenc:        32,
		Npk:         32,
		Nsk:         32,
		NsigPub:     32,
		NsigPriv:    32,
		Nsig:        64,
	},
	CipherSuiteX25519ChaCha20Sha256Ed25519: {
		Suite:       CipherSuiteX25519ChaCha20Sha256Ed25519,
		Name:        "MLS_128_DHKEMX25519_CHACHA20POLY1305_SHA256_Ed25519",
		KemId:       HpkeKemX25519HkdfSha256,
		KdfId:       HpkeKdfHkdfSha256,
		AeadId:      HpkeAeadChaCha20Poly1305,
		SignatureId: SignatureSchemeEd25519,
		Nh:          32,
		Nk:          32,
		Nn:          12,
		Nt:          16,
		Nsecret:     32,
		Nenc:        32,
		Npk:         32,
		Nsk:         32,
		NsigPub:     32,
		NsigPriv:    32,
		Nsig:        64,
	},
}

// Every registered code point, ascending, in a slice the caller owns. Map iteration
// order is randomized, so sorting is what lets a caller or a test depend on the order
// at all; ascending by code point is the order the RFC registry itself uses.
func Suites() []CipherSuite {
	suites := make([]CipherSuite, 0, len(registeredSuiteParams))
	for suite := range registeredSuiteParams {
		suites = append(suites, suite)
	}
	slices.Sort(suites)
	return suites
}

// The parameters of one registered suite, or ErrUnknownCipherSuite. The returned
// pointer addresses a fresh copy — the registry is not reachable through it — so a
// caller that mutates the result corrupts only its own copy. On the error path there
// is no params at all rather than a zero valued one, since a zero SuiteParams reads
// as a suite with 0 byte keys and would be a usable looking answer to a question that
// has none.
func LookupSuite(suite CipherSuite) (*SuiteParams, error) {
	params, ok := registeredSuiteParams[suite]
	if !ok {
		return nil, ErrUnknownCipherSuite
	}
	return &params, nil
}

// Whether the code point is one of the two the package implements. This is the
// question a caller asks before it has anything to do with the parameters, and it is
// deliberately not answerable by ignoring an error from a lookup.
func IsRegisteredSuite(suite CipherSuite) bool {
	_, ok := registeredSuiteParams[suite]
	return ok
}
