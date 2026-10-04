// The only place in mls or message that calls ECDH. Master section 7.2 and spec A
// section 5.9, guardrail 3: every x25519 operation goes through crypto/ecdh and a
// returned error is a hard validation failure, never logged and continued.
//
// crypto/ecdh already refuses an all zero shared secret, which is exactly the low order
// point case the banned sdk.GenerateSharedSecret returns successfully. Routing every
// caller through one wrapper turns that refusal into ErrInvalidPoint, so there is
// exactly one line in the tree that could ignore it and that line is reviewed.
//
// crypto_forbidden_test.go asserts both halves of that, and only over the roots it walks,
// which are these two packages: no other file in either calls ECDH, and no call site in
// either discards its result. sdk is a separate module outside those roots and still
// carries the banned GenerateSharedSecret, which is its own migration — the claim above
// is exactly as wide as the gate that proves it and no wider.
//
// Nothing here accepts a nil key. crypto/ecdh dereferences both operands before it looks
// at either, so a nil reaching the exchange is a panic rather than a refusal, and a
// caller holding nil is a caller that already ignored a parse error. The guards below
// give it the same sentinel a zero length key gets, which is also what parsing that nil
// would have returned.
package mls

import (
	"crypto/ecdh"
	"io"
)

// The byte length of an x25519 scalar, public key and shared secret alike, fixed by
// RFC 7748 section 5 rather than by a ciphersuite. It is written here rather than read
// from SuiteParams because this file is about the curve: a suite that later names a
// different kem gets a different helper, not a different length in this one.
const x25519KeySize = 32

// Parses a scalar into a key crypto/ecdh will use. The length is checked here rather
// than left to the standard library so the failure is one of this package's sentinels,
// which is what lets a caller tell a malformed key apart from a key that was fine but
// produced no usable secret.
func X25519PrivateKey(b []byte) (*ecdh.PrivateKey, error) {
	if len(b) != x25519KeySize {
		return nil, ErrBadKeyLength
	}
	priv, err := ecdh.X25519().NewPrivateKey(b)
	if err != nil {
		// Past the length just checked, the standard library refuses an x25519
		// scalar only under GODEBUG=fips140=only, where the curve is unavailable
		// outright rather than this scalar being bad. Refusing is still the only
		// right answer, and mapping it keeps a nil key from ever leaving here with
		// a nil error.
		return nil, ErrInvalidPoint
	}
	return priv, nil
}

// Parses a peer's u coordinate. Note that crypto/ecdh accepts every 32 byte string here,
// low order points included, and refuses them at the exchange instead; this function is
// therefore a length gate, and the refusal callers depend on is X25519DH's.
func X25519PublicKey(b []byte) (*ecdh.PublicKey, error) {
	if len(b) != x25519KeySize {
		return nil, ErrBadKeyLength
	}
	pub, err := ecdh.X25519().NewPublicKey(b)
	if err != nil {
		return nil, ErrInvalidPoint
	}
	return pub, nil
}

// Draws a fresh key pair, reading the scalar out of random rather than delegating to
// ecdh.GenerateKey. Since Go 1.26 that function ignores the reader it is handed unless
// GODEBUG=cryptocustomrand=1 is set: a reader that fails yields a good key and a nil
// error, and a fixed reader yields a different key every call. Delegating would make
// every randomness parameter this plan's contract declares — HpkeSetupBaseS,
// NewCryptoProviderWithRandom — quietly decorative, and would leave the hpke encap
// direction with no way to reproduce a published vector. Thirty two bytes read here is
// what RFC 7748 section 6.1 says an x25519 private key is.
//
// A nil reader is refused rather than filled in. Falling back to the process source is
// what ecdh.GenerateKey does with one, and inheriting that here made every randomness
// parameter above this function decorative in exactly the case that matters: a provider
// built by NewCryptoProviderWithRandom over a nil reader sealed twice under two different
// ephemeral keys, because this was the draw underneath its HpkeSeal. The keys were good
// keys, which is why nothing else could see it. Any reader a caller actually passed is
// honoured, and its error is returned unwrapped: a failing entropy source is not a key
// length or a curve problem and must not be reported as one.
func X25519GenerateKey(random io.Reader) (*ecdh.PrivateKey, error) {
	if random == nil {
		return nil, ErrNilRandomSource
	}
	scalar := make([]byte, x25519KeySize)
	if _, err := io.ReadFull(random, scalar); err != nil {
		return nil, err
	}
	return X25519PrivateKey(scalar)
}

// The diffie-hellman itself. The error is never a warning, and a nil secret always
// accompanies it, so a caller that ignores the error still cannot proceed with bytes
// that look usable.
func X25519DH(priv *ecdh.PrivateKey, pub *ecdh.PublicKey) ([]byte, error) {
	if priv == nil || pub == nil {
		return nil, ErrBadKeyLength
	}
	secret, err := priv.ECDH(pub)
	if err != nil {
		return nil, ErrInvalidPoint
	}
	return secret, nil
}
