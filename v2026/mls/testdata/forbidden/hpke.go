//go:build ignore

// The second allowed base name for hkdf.Extract, present for the same reason crypto.go
// is: with only one allowed file in the fixture, dropping the other from the gate's
// allowed list would change nothing the control can see.
//
// Under testdata and build constrained out, like every file in this directory.
package forbidden

import (
	"crypto/hkdf"
	"crypto/sha256"
)

func hpkeExtract(salt []byte, ikm []byte) []byte {
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
func hpkeHkdfExpand(prk []byte, info string) []byte {
	out, err := hkdf.Expand(sha256.New, prk, info, 32)
	if err != nil {
		return nil
	}
	return out
}

func hpkeHkdfKey(salt []byte, ikm []byte, info string) []byte {
	out, err := hkdf.Key(sha256.New, ikm, salt, info, 32)
	if err != nil {
		return nil
	}
	return out
}
