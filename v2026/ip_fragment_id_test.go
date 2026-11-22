package connect

import (
	"testing"
)

// connect#222: fragment ids came from a process-wide sequential counter
func TestIpv6FragmentIdentificationIsNotSequential(t *testing.T) {
	a := nextIpv6FragmentIdentification()
	b := nextIpv6FragmentIdentification()
	c := nextIpv6FragmentIdentification()
	if b-a == 1 && c-b == 1 {
		t.Fatalf("sequential ipv6 fragment ids: %d %d %d", a, b, c)
	}
}

func TestIpv4FragmentIdentificationIsNotSequential(t *testing.T) {
	a := nextIpv4FragmentIdentification()
	b := nextIpv4FragmentIdentification()
	c := nextIpv4FragmentIdentification()
	if b-a == 1 && c-b == 1 {
		t.Fatalf("sequential ipv4 fragment ids: %d %d %d", a, b, c)
	}
}

func testFragmentIdKey() [4]uint64 {
	return [4]uint64{0x0123456789abcdef, 0x1111111111111111, 0x2222222222222222, 0x3333333333333333}
}

// a full 16-bit cycle yields every nonzero id exactly once, in a non-sequential order
func TestIpv4FragmentIdGeneratorIsUniqueNonzeroAndUnordered(t *testing.T) {
	generator := newFragmentIdGenerator(testFragmentIdKey(), 8, 0)
	seen := map[uint32]bool{}
	sequentialSteps := 0
	var previous uint32
	for i := 0; i < 1<<16-1; i++ {
		identification := generator.next()
		if identification == 0 || 1<<16 <= identification {
			t.Fatalf("id out of range: %d", identification)
		}
		if seen[identification] {
			t.Fatalf("duplicate id %d at %d", identification, i)
		}
		seen[identification] = true
		if 0 < i && identification == previous+1 {
			sequentialSteps += 1
		}
		previous = identification
	}
	if 16 < sequentialSteps {
		t.Fatalf("too many sequential steps: %d", sequentialSteps)
	}
}

func TestIpv6FragmentIdGeneratorIsUniqueAndNonzero(t *testing.T) {
	generator := newFragmentIdGenerator(testFragmentIdKey(), 16, 0xffff0000)
	seen := map[uint32]bool{}
	for i := 0; i < 1<<17; i++ {
		identification := generator.next()
		if identification == 0 || seen[identification] {
			t.Fatalf("bad id %d at %d", identification, i)
		}
		seen[identification] = true
	}
}

// different secrets give different id streams for the same counter
func TestFragmentIdGeneratorDependsOnKey(t *testing.T) {
	a := newFragmentIdGenerator(testFragmentIdKey(), 16, 0)
	otherKey := testFragmentIdKey()
	otherKey[0] ^= 1
	b := newFragmentIdGenerator(otherKey, 16, 0)
	same := 0
	for i := 0; i < 64; i++ {
		if a.next() == b.next() {
			same += 1
		}
	}
	if 1 < same {
		t.Fatalf("keys do not change ids: %d of 64 equal", same)
	}
}
