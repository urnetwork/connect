//go:build ignore

// The negative half of every control: a file that names each banned thing in prose and
// does none of them. It stands in for crypto_errors.go, whose file comment explains
// that the banned sdk.GenerateSharedSecret hands back an all zero secret for a low
// order point, and for the x25519 helper, whose comment says the same. A gate that
// fires on those comments bans the sentence that teaches the rule, so the matchers
// strip comments first and this file is what proves they do.
//
// Under testdata and build constrained out, like every file in this directory.
package forbidden

// Every banned token, in a line comment: curve25519.ScalarMult, box.Precompute,
// GenerateSharedSecret, and the import path golang.org/x/crypto/nacl/box.

/*
And again in a block comment, because a block comment is no more executable than a
line comment: curve25519.ScalarMult, box.Precompute, GenerateSharedSecret,
golang.org/x/crypto/nacl/box.

Both confined calls too, so the confinement checks are covered by this file as well:
hkdf.Extract(sha256.New, ikm, salt) outside its two files, and priv.ECDH(pub) outside
its one, in the discarding shapes _, _ = priv.ECDH(pub) and secret, _ := priv.ECDH(pub).
*/

// hkdf.Extract(sha256.New, ikm, salt) and secret, _ := priv.ECDH(pub) once more on a
// line comment, which is the form the real files actually use.
func documentedOnly() {}
