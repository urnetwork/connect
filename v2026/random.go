package connect

// The one constructor of a generator that production code owns (CODESTYLE.md,
// "Randomness").
//
// Secrets, keys, nonces, tokens and unguessable ids come from crypto/rand.
// All other randomness draws from the top-level math/rand/v2 functions, which
// read the runtime's per-thread ChaCha8 generator and are safe for concurrent
// use. A type that needs a generator of its own, to own a stream or to accept
// a deterministic one from a test, builds it with NewSecureRand, so that no
// production generator is seeded from the clock, an id, a counter or a fixed
// value. random_rule_test.go enforces this over the repository.

import (
	cryptorand "crypto/rand"
	mathrand "math/rand/v2"
)

// A ChaCha8 generator seeded from crypto/rand. Its outputs reveal neither its
// state nor its seed, so what it has drawn does not predict what it draws next.
// Not safe for concurrent use; the caller owns it or guards it.
func NewSecureRand() *mathrand.Rand {
	var seed [32]byte
	// never returns an error since go 1.24: a failing source crashes the program
	cryptorand.Read(seed[:])
	return mathrand.New(mathrand.NewChaCha8(seed))
}
