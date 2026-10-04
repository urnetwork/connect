//go:build ignore

// The allowed half of both x25519 controls. crypto_x25519.go is the one base name that
// may make the call, and taking the error and refusing on it is the one shape the
// guardrail asks for, so this file must be reported by neither the confinement check
// nor the discard check.
//
// Under testdata and build constrained out, like every file in this directory.
package forbidden

import "crypto/ecdh"

// The error is the low order point case: crypto/ecdh refuses an all zero shared secret
// where the banned helpers return one, so returning the error is the refusal.
func allowedDh(priv *ecdh.PrivateKey, pub *ecdh.PublicKey) ([]byte, error) {
	secret, err := priv.ECDH(pub)
	if err != nil {
		return nil, err
	}
	return secret, nil
}
