package connect

import (
	mathrand "math/rand/v2"
	"slices"
	"testing"
)

// Two generators from NewSecureRand draw different streams, and neither is the
// stream of an unfilled (zero) seed: each is seeded from crypto/rand, not from
// the clock or a shared value. Four equal 64-bit draws happen with probability
// 2^-256.
func TestNewSecureRandGeneratorsDrawDifferentStreams(t *testing.T) {
	draws := func(random *mathrand.Rand) []uint64 {
		values := []uint64{}
		for range 4 {
			values = append(values, random.Uint64())
		}
		return values
	}
	first := draws(NewSecureRand())
	second := draws(NewSecureRand())
	if slices.Equal(first, second) {
		t.Fatalf("two generators drew the same stream %v", first)
	}
	// the stream a generator would draw if its seed were never filled
	zero := draws(mathrand.New(mathrand.NewChaCha8([32]byte{})))
	if slices.Equal(first, zero) || slices.Equal(second, zero) {
		t.Fatalf("a generator drew the zero seed's stream %v", zero)
	}
}
