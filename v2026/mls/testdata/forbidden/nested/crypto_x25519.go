//go:build ignore

// The x25519 confinement's one allowed base name, one directory deeper. Guardrail 3 has
// the same shape as guardrail 1 and had the same defect, so it gets the same control.
//
// The error is taken and returned, so the discard matcher has nothing to say about this
// file either way: what is being controlled here is the confinement, and a discard would
// be a second reason for a report that is meant to have exactly one.
//
// Under testdata and build constrained out, like every file in this tree.
package nested

import "crypto/ecdh"

func nestedDh(priv *ecdh.PrivateKey, pub *ecdh.PublicKey) ([]byte, error) {
	secret, err := priv.ECDH(pub)
	if err != nil {
		return nil, err
	}
	return secret, nil
}
