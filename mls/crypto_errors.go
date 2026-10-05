// Typed errors for the crypto layer. Every one is fatal by construction: there is no
// path in this package that logs one and continues (spec A section 5.9, guardrail 7).
// In particular a length mismatch and an invalid curve point are refusals, not
// conditions a caller may retry past or substitute a default for.
//
// The RFC 9420 ValSem validation codes live in errors.go, which this file is
// deliberately not. A crypto failure is not a validation semantic: a bad signature
// here means the primitive said no, while ValSem010 means the protocol rejected the
// message, and the two are asserted by different gates.
//
// ErrCryptoBadSignature is deliberately not named ErrBadSignature: that name belongs
// to errors.go, where it is ValSem010 and gate 3 asserts it. errors.go wraps this
// value, so errors.Is holds through both and a caller can ask either question.
package mls

import "errors"

var (
	// ErrUnknownCipherSuite fires when a code point outside the two entry registry
	// reaches a lookup. An unregistered suite is refused, never defaulted to 0x0003.
	ErrUnknownCipherSuite = errors.New("mls: unknown ciphersuite")
	// ErrInvalidPoint fires when x25519 produced an all zero shared secret, which is
	// the low order point case that the banned sdk.GenerateSharedSecret returns
	// successfully. crypto/ecdh reports it and this package refuses on it.
	ErrInvalidPoint = errors.New("mls: x25519 produced an invalid shared secret")
	// ErrBadKeyLength fires when a key is not the length its ciphersuite fixes.
	ErrBadKeyLength = errors.New("mls: key length does not match the ciphersuite")
	// ErrNilRandomSource fires when a caller hands a constructor or a key generator
	// no entropy source at all. Substituting the process source for it is what this
	// refusal exists to prevent: the key that came back would be a good key drawn
	// from a stream the caller cannot reproduce, and the failing case a deterministic
	// provider was built to reproduce would quietly stop reproducing. It is separate
	// from ErrBadKeyLength and ErrInvalidPoint because a caller telling a broken
	// machine from a malformed input reads the wrong answer otherwise.
	ErrNilRandomSource = errors.New("mls: no random source was supplied")
	// ErrBadNonceLength fires when an aead nonce is not the length its ciphersuite
	// fixes, so a truncated or padded nonce can never silently reuse a keystream.
	ErrBadNonceLength = errors.New("mls: nonce length does not match the ciphersuite")
	// ErrBadKemOutput fires when an encapsulated key is not the length its kem fixes.
	ErrBadKemOutput = errors.New("mls: kem output length does not match the ciphersuite")
	// ErrBadSignatureKey fires when a signature key is not the length its scheme
	// fixes. The private form is the 32 byte ed25519 seed, not the 64 byte expanded
	// key, so this also catches passing the wrong one of the two.
	ErrBadSignatureKey = errors.New("mls: signature key length does not match the ciphersuite")
	// ErrCryptoBadSignature fires when the signature primitive rejected a signature.
	// It is the crypto layer half of the pair described in the file comment.
	ErrCryptoBadSignature = errors.New("mls: signature verification failed")
	// ErrAeadOpen fires when an aead open failed. It carries no detail on purpose:
	// which of the key, nonce, aad or ciphertext was wrong is not something an
	// attacker gets to learn from the error.
	ErrAeadOpen = errors.New("mls: aead open failed")
	// ErrSequenceOverflow fires when an hpke context's sequence number would wrap.
	// Wrapping would repeat a nonce under a key already used, so the context refuses
	// to seal or open again rather than rolling over.
	ErrSequenceOverflow = errors.New("mls: hpke context sequence number overflow")
)
