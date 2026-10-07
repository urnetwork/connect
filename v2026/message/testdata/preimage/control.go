//go:build ignore

// The control for the preimage input gate in message/aad_test.go: one file holding a
// function of every shape the gate has an opinion about, so the gate can be shown to
// separate them rather than assumed to.
//
// A gate that reports nothing because it is broken and a gate that reports nothing
// because the tree is clean look identical from the outside, and aad.go is clean, so on
// the day this gate was written it had nothing at all to say. This file is what tells
// the two apart, and it carries the negative half as well as the positive: a function
// that reads everything it is handed must not be reported, or the gate is only a
// complicated way of failing.
//
// It cannot reach the gate and the gate cannot reach it. The go tool never builds a
// testdata directory, the build constraint above says so a second time for a reader who
// has only this file open, and the gate reaches it only because aad_test.go names this
// directory outright as the control.
//
// None of this is a preimage. The two real ones are in message/aad.go and the shapes
// below exist to be judged, not to be copied.
package preimage

// A wide input, in the shape guardrail G4 of spec A section 5.9 bans: a hash sits within
// reach of a builder that has no business with one. This is the defect, written down.
type wideHeader struct {
	GroupId  [32]byte
	Epoch    uint64
	BodyHash [32]byte
}

// A narrow input, in the shape G4 asks for: it holds what the builder covers and there
// is no hash in it to reach.
type narrowBinding struct {
	GroupId [32]byte
	Epoch   uint64
}

// An input holding another struct, which is the one shape the gate refuses to judge. It
// reads one level, so a field that is itself a struct hides its own fields from the
// reachability walk, and a gate that quietly reported such a function clean would be
// under-reporting exactly where a wide input is easiest to hide.
type nestingInput struct {
	Inner narrowBinding
	Tag   uint64
}

// The positive control: every field is reachable, BodyHash is never read, and the gate
// must say so.
func preimageOverAWideHeader(h *wideHeader) ([]byte, error) {
	out := []byte{}
	out = append(out, h.GroupId[:]...)
	out = append(out, byte(h.Epoch))
	return out, nil
}

// The negative control: reachable and read are the same set, and the gate must be silent.
func preimageOverANarrowBinding(b narrowBinding) ([]byte, error) {
	out := []byte{}
	out = append(out, b.GroupId[:]...)
	out = append(out, byte(b.Epoch))
	return out, nil
}

// The coverage control: every field is read, so the unread set is empty, and the gate
// must still refuse it because one of those fields is a struct it cannot see into.
func preimageOverANestedInput(n nestingInput) ([]byte, error) {
	out := []byte{}
	out = append(out, n.Inner.GroupId[:]...)
	out = append(out, byte(n.Tag))
	return out, nil
}

// The class control: the same wide input, ignoring the same field, in a function that
// does not hand back a preimage. The gate is about the functions that produce the bytes
// an aead is sealed against, so this one must not be in the class at all — and if the
// class predicate ever widens to "takes a struct", this is what notices.
func notAPreimageAtAll(h *wideHeader) (uint64, error) {
	return h.Epoch, nil
}
