//go:build ignore

// The positive control for the join gate in message/record_test.go: one file that
// commits every shape the gate bans, so the rules can be shown to catch them rather than
// assumed to. A rule that finds nothing because it is broken and one that finds nothing
// because the tree is clean report the same result, and all four of the original
// matchers found nothing on the day they were written. This file is what tells the two
// apart.
//
// It cannot reach the gate and the gate cannot reach it. The walk skips any directory
// named testdata unless a root names one outright, which only the controls do; the go
// tool never builds a testdata directory at all; and the build constraint above says so
// a second time for a reader who has only this file open.
//
// None of this is real code and none of it is how a record is written. The class and the
// bucket are joined in exactly one place, message/record.go, and every shape below is a
// second implementation of that crossing — which is a second place the two can be
// conflated, silently, in a comparison that then covers half the eph records it reads as
// covering. The sibling file documented.go carries the other half of the control: the
// same shapes in prose, and the legal call beside them, so that "not reported" means
// "allowed or absent" rather than "the rules are asleep".
package forbidden

// ── the join: building the wire byte out of the two halves ──

// The shift, which is how the wire byte is usually rebuilt by hand.
func joinByShift(retentionClass uint8) uint8 { return retentionClass << 4 }

// The or, in the operand order a contributor reaches for first.
func joinByOr(retentionClass uint8, ephBucket uint8) uint8 { return retentionClass | ephBucket }

// The addition, with the eph base written as a decimal literal.
func joinByAdd(ephBucket uint8) uint8 { return 16 + ephBucket }

// The multiplication, which is the shift written for a reader who dislikes shifts.
func joinByMultiply(retentionClass uint8) uint8 { return retentionClass * 16 }

// The wire bytes of the three classes that carry no bucket, and the eph base: a second
// copy of the table record.go owns.
var retentionWireByClass = [...]uint8{0x00, 0x01, 0x02, 0x10}

// The addition again, with the base looked up in that table rather than written out.
// This is the shape that walked straight past the regular expressions this gate used to
// be: the operand to the left of the plus ends in a bracket, and a pattern anchored on an
// identifier cannot span it. Nothing about it is less of a join than the four above.
func joinByTable(retentionClass uint8, ephBucket uint8) uint8 {
	return retentionWireByClass[retentionClass] + ephBucket
}

// ── the split: taking the two halves back out of the wire byte ──
//
// Spec A section 5.1 says the two functions in record.go are the only places in the
// system where the class and the bucket are joined or split, and a split is the same
// conflation read backwards — in a prune query, in a key lookup, in a switch, rather
// than in an encoder. Every shape below is one.

// The shift, taking the class back out of the high nibble.
func splitByShift(wire uint8) uint8 { return wire >> 4 }

// The mask, taking the bucket out from under the base.
func splitByMask(wire uint8) uint8 { return wire & 0x0f }

// The subtraction, which is what a reader writes with the table in front of them.
func splitBySubtract(wire uint8) uint8 { return wire - 0x10 }

// The division, which is both halves at once.
func splitByDivide(wire uint8) (uint8, uint8) { return wire / 16, wire % 16 }
