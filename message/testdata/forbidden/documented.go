//go:build ignore

// The negative half of the control: a file that names every banned shape in prose and
// commits none of them. It stands in for message/record.go and for record_test.go, both
// of which write the shapes out in comments — record.go because the wire table is
// restated there in full, and the test file because it has to name what it looks for. A
// gate that fires on those comments bans the sentence that teaches the rule, so the
// rules read the syntax tree, where a comment is not an expression at all, and this file
// is what proves they do.
//
// The banned join shapes, in a line comment: class<<4, class|bucket, 16+bucket and
// class*16. And their mirror images, the split shapes: wire>>4, wire&0x0f, and
// wire-0x10 with the wire/16 and wire%16 that are the same subtraction done twice.
//
// Under testdata and build constrained out, like every file in this directory.
package forbidden

/*
And again in a block comment, because a block comment is no more executable than a line
comment: class<<4, class|bucket, 16+bucket, class*16, wire>>4, wire&0x0f, wire-0x10.

Written out at length too, in the arithmetic a reader would recognise: shifting the
retention class left by four and adding the eph bucket to it, or multiplying the class by
16 and or-ing the bucket in, and taking them apart again by shifting the byte back down
and masking off what is left.
*/

import "github.com/urnetwork/connect/message"

// The legal way to do it, which is to call the one function allowed to join, so that the
// controls are comparing "allowed" against "reported" rather than "absent" against
// "reported".
func joinTheOnlyWay(retentionClass message.RetentionClass, ephBucket uint8) (byte, error) {
	return message.RetentionClassWire(retentionClass, ephBucket)
}

// And the split, the other half of the pair, for the same reason.
func splitTheOnlyWay(wire byte) (message.RetentionClass, uint8, error) {
	return message.RetentionClassOf(wire)
}

// The case a matcher over text cannot get right: a line of working code with a sentence
// about the banned shapes on the end of it. Stripping a comment only when it is the whole
// line leaves this one in the text the matcher reads, and the gate then fires on the
// prose that explains itself — which is the gate the next contributor turns off. The
// parser never sees it.
func splitTheOnlyWayWithATrailingComment(wire byte) (message.RetentionClass, uint8, error) {
	return message.RetentionClassOf(wire) // not class<<4, not 16+bucket, not wire&0x0f
}
