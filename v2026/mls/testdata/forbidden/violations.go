//go:build ignore

// The positive control for mls/crypto_forbidden_test.go: one file that commits every
// banned act at once, so the scanner can be shown to catch them rather than assumed to.
// A scanner that finds nothing because it is broken and one that finds nothing because
// the code is clean report the same result, and the four gates next door found nothing
// on the day they were written. This file is what tells the two apart.
//
// It cannot reach the production scan and the production scan cannot reach it. The walk
// skips any directory named testdata unless a root names one outright, which only the
// control does; the go tool never builds a testdata directory at all; and the build
// constraint above says so a second time for a reader who has only this file open.
//
// None of this is real code and none of it compiles against the real packages. Do not
// copy a line of it. The sibling files carry the other half of each control: the same
// calls in the file names that are allowed to make them, so that "not reported" in the
// control means "allowed" rather than "not present".
package forbidden

import (
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/sha256"

	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/nacl/box"
)

// The banned primitives, one call each. The sdk helper and box.Precompute both hand
// back an all zero secret for a low order point instead of an error, and
// curve25519.ScalarMult is the deprecated call underneath them.
func bannedPrimitives(priv []byte, pub []byte) {
	var product, scalar, point [32]byte
	curve25519.ScalarMult(&product, &scalar, &point)
	var shared, peerPublic, ownPrivate [32]byte
	box.Precompute(&shared, &peerPublic, &ownPrivate)
	sink(GenerateSharedSecret(priv, pub))
}

// Stands in for the sdk helper of the same name, so the banned token is present in
// code without this fixture importing the sdk.
func GenerateSharedSecret(priv []byte, pub []byte) []byte { return nil }

// hkdf.Extract in a file that is neither crypto.go nor hpke.go. The argument order is
// the trap the confinement exists for: the standard library takes the input keying
// material first and the salt second, the reverse of every spec text in this project.
func extractOutsideItsCallSites(salt []byte, ikm []byte) []byte {
	prk, _ := hkdf.Extract(sha256.New, ikm, salt)
	return prk
}

// The x25519 call in a file that may not make it, in all four shapes that throw the
// result or its error away. The comment on the last line is documentation rather than
// a call site, and the discard control asserts it is not reported: a gate that fires on
// its own explanation is a gate the next contributor deletes.
func ecdhOutsideItsCallSite(priv *ecdh.PrivateKey, pub *ecdh.PublicKey) {
	_, _ = priv.ECDH(pub)
	_, err := priv.ECDH(pub)
	secret, _ := priv.ECDH(pub)
	var shared []byte
	shared, _ = priv.ECDH(pub)
	sink(secret)
	sink(shared)
	sinkError(err)
	// never write shared, _ := priv.ECDH(pub); the error is the low order point
}

func sink(b []byte) {}

func sinkError(err error) {}

// The other two entry points crypto/hkdf declares, present for the same reason the
// Extract call above is. The confinement gate derives its class off crypto/hkdf rather
// than naming one function, and a control that exercised only Extract would issue the
// clean bill for Expand and for Key however wide the class had become.
//
// Key is the one that matters most. It is Extract and Expand in a single call, so it takes
// the secret and the salt in the same reversed order Extract does, and a transposition
// there produces a whole key schedule that is 32 bytes long, internally consistent and
// wrong -- visible only against another implementation.
func violationsHkdfExpand(prk []byte, info string) []byte {
	out, err := hkdf.Expand(sha256.New, prk, info, 32)
	if err != nil {
		return nil
	}
	return out
}

func violationsHkdfKey(salt []byte, ikm []byte, info string) []byte {
	out, err := hkdf.Key(sha256.New, ikm, salt, info, 32)
	if err != nil {
		return nil
	}
	return out
}
