//go:build ignore

// A file whose BASE name is on the hkdf confinement's allowed list and whose PATH is not.
//
// The exemption this controls used to be read off the base name, so a file called
// crypto.go was excused wherever it sat -- in a subpackage, in a subdirectory, anywhere
// under the scanned roots -- and a confined call could be moved into one of those and go
// unreported. The control requires this file to be reported, which is what holds the
// allowed list to being a list of paths rather than a list of names.
//
// Derived rather than written out: the control builds one expected path per entry of the
// allowed list, so an allowed path added without a twin here fails rather than arriving
// uncontrolled.
//
// Under testdata and build constrained out, like every file in this tree.
package nested

import (
	"crypto/hkdf"
	"crypto/sha256"
)

func nestedCryptoExtract(salt []byte, ikm []byte) []byte {
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
func nestedCryptoHkdfExpand(prk []byte, info string) []byte {
	out, err := hkdf.Expand(sha256.New, prk, info, 32)
	if err != nil {
		return nil
	}
	return out
}

func nestedCryptoHkdfKey(salt []byte, ikm []byte, info string) []byte {
	out, err := hkdf.Key(sha256.New, ikm, salt, info, 32)
	if err != nil {
		return nil
	}
	return out
}
