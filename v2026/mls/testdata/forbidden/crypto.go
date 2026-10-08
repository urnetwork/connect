//go:build ignore

// The allowed half of the hkdf confinement control. crypto.go is one of the two base
// names the gate permits, so this file has to make the call and has to go unreported.
// Without it, "the control reported only violations.go" would mean "nothing else calls
// it" rather than "everything else that calls it is allowed to", and widening the
// allowed list to every file would still pass.
//
// Under testdata and build constrained out, like every file in this directory.
package forbidden

import (
	"crypto/hkdf"
	"crypto/sha256"
)

// Takes (salt, ikm) and swaps at the boundary, which is the whole reason the call is
// confined to two reviewable files.
func cryptoExtract(salt []byte, ikm []byte) []byte {
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil
	}
	return prk
}

// The other two entry points crypto/hkdf declares, present for the same reason the
// Extract call above is. The confinement gate derives its class off crypto/hkdf rather
// than naming one function, and a control that exercised only Extract would issue the
// clean bill for Expand and for Key however wide the class had become.
//
// Key is the one that matters most. It is Extract and Expand in a single call, so it takes
// the secret and the salt in the same reversed order Extract does, and a transposition
// there produces a whole key schedule that is 32 bytes long, internally consistent and
// wrong -- visible only against another implementation.
func cryptoHkdfExpand(prk []byte, info string) []byte {
	out, err := hkdf.Expand(sha256.New, prk, info, 32)
	if err != nil {
		return nil
	}
	return out
}

func cryptoHkdfKey(salt []byte, ikm []byte, info string) []byte {
	out, err := hkdf.Key(sha256.New, ikm, salt, info, 32)
	if err != nil {
		return nil
	}
	return out
}
