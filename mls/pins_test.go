// Compile assertions on the pinned standard library surface. The master protocol
// design section 7.2 requires slice 1 to pin the go version and to assert that
// mlkem.NewDecapsulationKey768 exists; the assertions below fail at build time on a
// toolchain that moved, which is the point. A build failure here is the intended
// signal, not a broken checkout: read it as "the pin is now a lie".
package mls

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/mlkem"
	"crypto/sha256"
	"crypto/sha3"
	"errors"
	"io"
	"runtime"
	"testing"

	"golang.org/x/crypto/chacha20poly1305"
)

var (
	_ func(seed []byte) (*mlkem.DecapsulationKey768, error) = mlkem.NewDecapsulationKey768
	_ func(data []byte, length int) []byte                  = sha3.SumSHAKE256
	_ func(data []byte) [32]byte                            = sha3.Sum256
	_ func() ecdh.Curve                                     = ecdh.X25519
	_ func(seed []byte) ed25519.PrivateKey                  = ed25519.NewKeyFromSeed

	_ func(key []byte) (interface{ NonceSize() int }, error) = func(key []byte) (interface{ NonceSize() int }, error) {
		return chacha20poly1305.New(key)
	}
)

// The hkdf generic surface, bound to sha256 so a signature change breaks the build.
// The parameter names are the standard library's own and they carry the trap: the
// secret, meaning the input keying material, comes first and the salt second, which
// is the reverse of the HKDF-Extract(salt, ikm) that every spec text in this project
// writes. Every wrapper in this package takes (salt, ikm) and swaps here.
var (
	_ = func(secret, salt []byte) ([]byte, error) { return hkdf.Extract(sha256.New, secret, salt) }
	_ = func(prk []byte, info string, n int) ([]byte, error) {
		return hkdf.Expand(sha256.New, prk, info, n)
	}
)

// TestPinnedToolchain reads the version of the toolchain that built the test binary,
// so the pin holds whatever go.mod says. go.mod's toolchain line selects the
// compiler; this catches the case where it was overridden or ignored.
func TestPinnedToolchain(t *testing.T) {
	if runtime.Version() != "go1.26.5" {
		t.Fatalf("toolchain is %s, want go1.26.5", runtime.Version())
	}
}

// TestMlkemSeedSizeIsSixtyFour pins guardrail 9: the decapsulation key seed is the
// 64 byte d concatenated with z, never the 32 byte X-Wing seed it is derived from.
// Feeding the shorter seed is the confusion the guardrail names, and it must be an
// error rather than a key.
func TestMlkemSeedSizeIsSixtyFour(t *testing.T) {
	if _, err := mlkem.NewDecapsulationKey768(make([]byte, 32)); err == nil {
		t.Fatalf("NewDecapsulationKey768 accepted a 32-byte seed")
	}
	dk, err := mlkem.NewDecapsulationKey768(make([]byte, 64))
	if err != nil {
		t.Fatalf("NewDecapsulationKey768 rejected a 64-byte seed: %v", err)
	}
	if n := len(dk.EncapsulationKey().Bytes()); n != 1184 {
		t.Fatalf("encapsulation key is %d bytes, want 1184", n)
	}
}

// TestCryptoErrorsAreDistinct asserts every crypto layer sentinel is a value of its
// own, so a caller that asks errors.Is for one cannot be answered yes by another.
// Two vars sharing one errors.New would make a length failure indistinguishable from
// a signature failure at every call site that branches on them.
func TestCryptoErrorsAreDistinct(t *testing.T) {
	all := []error{
		ErrUnknownCipherSuite, ErrInvalidPoint, ErrBadKeyLength, ErrBadNonceLength,
		ErrBadKemOutput, ErrBadSignatureKey, ErrCryptoBadSignature, ErrAeadOpen, ErrSequenceOverflow,
	}
	for i, a := range all {
		if a == nil {
			t.Fatalf("error %d is nil", i)
		}
		for j, b := range all {
			if i != j && errors.Is(a, b) {
				t.Errorf("error %d and %d are the same value", i, j)
			}
		}
	}
	var _ io.Reader
}
