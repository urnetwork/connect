// The 2026-09-13 sentinel ruling, held as a partition of every uint8 rather than as three
// literals.
//
// THE RULING, quoted from the block master section 8, spec A section 5.1 and spec B section 3.1
// each carry character for character:
//
//	The bucket-0 answer and the off-ladder answer MUST DIFFER. EphBucketSeconds MUST answer 0
//	for bucket 0 -- the true retention window of a rung that is never stored -- and a NEGATIVE
//	for 6..255, which is not a bucket at all.
//
// It answered -1 for both until this commit, which is m1 open item M1-27's second half: two
// meanings under one sentinel, so no caller could ask which of the two it had met.
//
// -- CLASS. The off ladder buckets are DERIVED and never listed. A bucket is on the ladder if and
// only if some retention wire byte names it, which is RetentionClassOf's answer and nothing this
// file decides; everything else is off it. That is where the 250 below comes from and why no
// number in this file is typed as a number: it is 256 minus however many buckets the wire admits,
// computed at run time from the same split the codec uses. A gate that wrote {6, 7, 8} -- the
// three somebody would type -- would pass on a ladder that grew a seventh rung and on one that
// answered a window for bucket 200.
//
// -- SCOPE. Every value a uint8 can hold, all 256 of them, offered one at a time. Not "the
// buckets the tests happen to use" and not a range somebody chose: the argument type is uint8, so
// the scope is uint8, and the complement is printed member by member so that a narrowing is
// visible rather than inferable.
//
// -- PROPERTY. Three cells, each non empty, each pinned, and the partition exhaustive: exactly
// one bucket answers zero, exactly as many answer a positive as the wire admits rungs with a
// window, and every remaining value answers a negative. The DIFFERENCE is asserted before either
// literal, because the property is distinguishability and a table that swapped the two answers
// satisfies each literal check read on its own.
package message

import (
	"fmt"
	"slices"
	"testing"
)

// ephBucketLadder is the set of buckets the wire admits, derived from the retention split.
//
// It is the same walk the wire alphabet cases in record_test.go make and it is repeated here
// rather than shared, because a helper shared between the gate and its subject is a helper that
// can be wrong in both places at once. What it answers is a fact about connect/message's own
// split function and about nothing in this file.
func ephBucketLadder(t *testing.T) []uint8 {
	t.Helper()
	buckets := []uint8{}
	for candidate := 0; candidate <= 0xFF; candidate += 1 {
		class, bucket, err := RetentionClassOf(byte(candidate))
		if err != nil || class != RetentionEph {
			continue
		}
		if !slices.Contains(buckets, bucket) {
			buckets = append(buckets, bucket)
		}
	}
	slices.Sort(buckets)
	return buckets
}

// TestTheBucketZeroAnswerAndTheOffLadderAnswerAreDistinguishable is the ruling's own sentence, in
// the order the ruling makes it: the two must DIFFER first, and only then is each held to its own
// side.
func TestTheBucketZeroAnswerAndTheOffLadderAnswerAreDistinguishable(t *testing.T) {
	ladder := ephBucketLadder(t)
	if len(ladder) == 0 {
		t.Fatal("the retention split names no eph bucket at all, so this gate partitioned nothing and every cell below is empty for a reason that has nothing to do with the ruling")
	}
	if !slices.Contains(ladder, uint8(0)) {
		t.Fatalf("the wire admits eph buckets %v and 0 is not among them, so the transient rung this ruling is about is not reachable and the comparison below is between two off ladder answers", ladder)
	}

	// THE COMPLEMENT, computed and printed. Every uint8 the ladder does not contain.
	offLadder := []uint8{}
	for candidate := 0; candidate <= 0xFF; candidate += 1 {
		if !slices.Contains(ladder, uint8(candidate)) {
			offLadder = append(offLadder, uint8(candidate))
		}
	}
	if len(offLadder) == 0 {
		t.Fatal("every uint8 value is a bucket the wire admits, so the off ladder class is empty and this gate has nothing to tell the transient rung apart from")
	}
	if want := 256 - len(ladder); len(offLadder) != want {
		t.Fatalf("the off ladder class has %d members and the ladder has %d rungs, which do not sum to the 256 values of a uint8; the class and its complement are the whole of the argument type or one of them was narrowed",
			len(offLadder), len(ladder))
	}
	printed := ""
	for _, bucket := range offLadder {
		printed += fmt.Sprintf("%02x", bucket)
	}
	t.Logf("class: the %d eph buckets the wire admits, %v", len(ladder), ladder)
	t.Logf("complement: the %d uint8 values that name no rung, %s", len(offLadder), printed)

	// THE DIFFERENCE, FIRST. Every off ladder answer against the transient rung's, so this is
	// not one comparison that could have been lucky.
	transient := EphBucketSeconds(0)
	for _, bucket := range offLadder {
		if seconds := EphBucketSeconds(bucket); seconds == transient {
			t.Errorf("bucket 0 and bucket %d both answer %d; the transient rung has a real retention window and bucket %d is not a bucket at all, and a caller holding that one answer cannot ask which of the two it met",
				bucket, seconds, bucket)
		}
	}

	// AND EACH ON ITS OWN SIDE, which is what says which way round they differ. A test that
	// asserted only the difference passes on a table with the two swapped.
	if transient != 0 {
		t.Errorf("bucket 0 answers %d; the ruling makes it 0, the true retention window of a rung that is never stored", transient)
	}
	for _, bucket := range offLadder {
		if seconds := EphBucketSeconds(bucket); 0 <= seconds {
			t.Errorf("bucket %d names no rung and answers %d; the ruling makes 6..255 a NEGATIVE, which is a programmer error marker and not a window", bucket, seconds)
		}
	}
}

// TestEveryUint8FallsInExactlyOneOfTheLaddersThreeCells is the same ruling read as arithmetic: the
// 256 values partition into three cells whose sizes are determined by the ladder, and the whole of
// what the ruling bought is that the middle cell is not folded into the third.
//
// The three sizes are all derived. The positive cell is the rungs that carry a window, the zero
// cell is the rungs that do not, and the negative cell is everything else -- and the sum is 256 by
// construction, so a cell that grew took its members from another cell rather than from nowhere.
func TestEveryUint8FallsInExactlyOneOfTheLaddersThreeCells(t *testing.T) {
	ladder := ephBucketLadder(t)
	positive, zero, negative := []uint8{}, []uint8{}, []uint8{}
	for candidate := 0; candidate <= 0xFF; candidate += 1 {
		bucket := uint8(candidate)
		switch seconds := EphBucketSeconds(bucket); {
		case 0 < seconds:
			positive = append(positive, bucket)
		case seconds == 0:
			zero = append(zero, bucket)
		default:
			negative = append(negative, bucket)
		}
	}
	if len(positive)+len(zero)+len(negative) != 256 {
		t.Fatalf("the three cells hold %d + %d + %d values and a uint8 has 256; the partition is not exhaustive, so a value fell in no cell at all",
			len(positive), len(zero), len(negative))
	}
	for name, cell := range map[string][]uint8{"positive": positive, "zero": zero, "negative": negative} {
		if len(cell) == 0 {
			t.Fatalf("the %s cell is empty, so this partition is a two way split reported as a three way one -- which is precisely the state the ruling of 2026-09-13 exists to end", name)
		}
	}
	// every cell's SIZE derived: the zero cell is the ladder's rungs that are never
	// persisted, the positive cell is the rest of the ladder, and the negative cell is the
	// complement of the ladder in uint8.
	if want := len(ladder) - len(zero); len(positive) != want {
		t.Errorf("%d buckets answer a positive window and the ladder has %d rungs of which %d answer zero; every rung is in exactly one of the two",
			len(positive), len(ladder), len(zero))
	}
	if want := 256 - len(ladder); len(negative) != want {
		t.Errorf("%d values answer a negative and the complement of the ladder in uint8 has %d members; a negative answered by a rung, or a rung answering no negative off the ladder, is the sentinel leaking back across the split",
			len(negative), want)
	}
	// and the one bucket the ruling is about, named. Folding the zero cell into the negative
	// one is what the code did before 2026-09-13, and the cost of it is exactly this many
	// buckets: the number is printed so that "it is only one" is a statement a reader can
	// check rather than a defence.
	t.Logf("positive %d %v, zero %d %v, negative %d (the ladder's complement in uint8)",
		len(positive), positive, len(zero), zero, len(negative))
	t.Logf("collapsing the zero cell back into the negative one, which is what this package did until 2026-09-13, would make the negative cell %d and lose the distinction on %v",
		len(negative)+len(zero), zero)
}
