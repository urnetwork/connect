//go:build ignore

// The control for the conditional-write gate in message/preimageconditional_test.go: one
// file holding a function of every shape the gate has an opinion about, so the gate can be
// shown to separate them rather than assumed to.
//
// A gate that reports nothing because it is broken and a gate that reports nothing because
// the tree is clean look identical from the outside, and every real builder in this package
// is clean, so on the day this gate was written it had nothing at all to say about them.
// This file is what tells the two apart. It carries the NEGATIVE half as well as the
// positive — a builder whose only branches return rather than write must not be reported, or
// the gate is a complicated way of banning error handling — and it carries the CLASS half,
// a function that writes conditionally and is not in the class at all, because the class
// predicate is the half most likely to be quietly wrong.
//
// It cannot reach the gate and the gate cannot reach it. The go tool never builds a testdata
// directory, the build constraint above says so a second time for a reader who has only this
// file open, and the gate reaches it only because preimageconditional_test.go names this
// directory outright as the control.
//
// None of this is a preimage. The two real aads are in message/aad.go, the two macs in
// message/writeauth.go and the record in message/codec.go; the shapes below exist to be
// judged, not to be copied.
package conditional

import "github.com/urnetwork/connect/mls/syntax"

// The POSITIVE control, and it is the defect master section 8 forbids written down: a field
// whose presence depends on the class it is qualifying. The gate must name this function and
// must name the branch the write sits in.
//
// It is the shape the blob_id precedent invites on its surface reading — "present iff the
// class is eph, absent otherwise" — and the shape that makes a preimage eight octets shorter
// on every record of three of the four classes.
func preimageWithAConditionalField(w *syntax.Writer, class uint8, window uint64) ([]byte, error) {
	w.WriteUint8(class)
	if class == 0x10 {
		w.WriteUint64(window)
	}
	return w.Bytes()
}

// The same defect in a switch rather than an if, because a class predicate that only knows
// about *ast.IfStmt is a predicate that a rewrite walks straight past.
func preimageWithAConditionalFieldInASwitch(w *syntax.Writer, class uint8, window uint64) ([]byte, error) {
	switch class {
	case 0x10, 0x11:
		w.WriteUint64(window)
	default:
		w.WriteUint8(0)
	}
	return w.Bytes()
}

// The same defect in a loop, which is the third shape a write can be reached only sometimes.
func preimageWithAWriteInALoop(w *syntax.Writer, values []uint64) ([]byte, error) {
	for _, value := range values {
		w.WriteUint64(value)
	}
	return w.Bytes()
}

// The NEGATIVE control: every field written straight, with branches that REFUSE rather than
// write. This is what every real builder in this package looks like — a nil check, a
// disagreement check, a join that can fail — and the gate must be silent about it. A gate
// that reported this would be a gate nobody could satisfy, and it would be turned off.
func preimageWithRefusalsAndNoConditionalField(w *syntax.Writer, class uint8, window uint64, ok bool) ([]byte, error) {
	if !ok {
		return nil, errRefused
	}
	w.WriteUint8(class)
	w.WriteUint64(window)
	if window == 0 && class == 0xFF {
		return nil, errRefused
	}
	return w.Bytes()
}

// The CLASS control: the positive control's own defect, in a function that hands back
// nothing. The class is the functions that produce the octets an aead or a mac is taken
// over, so this one must not be judged at all — and if the class predicate ever widens to
// "any function that touches a writer", this is what notices, because message/attachment.go
// has a real function of exactly this shape and its switch is legitimate.
func notABuilderAtAll(w *syntax.Writer, class uint8, window uint64) {
	if class == 0x10 {
		w.WriteUint64(window)
	}
}

var errRefused = errRefusedType{}

type errRefusedType struct{}

func (errRefusedType) Error() string { return "refused" }
