// optional<T> per rfc 9420 section 2.1.1: a presence octet, then the value when
// present. The presence octet is 0 or 1 and nothing else — reading "any non zero
// means present" would give one value many encodings, the same canonicalization
// requirement the minimal varint rule in varint.go exists for, and it matters for
// the same reason: MLS signs over serialized forms, so an encoding a verifier
// accepts but a signer never produced is a signature bypass primitive. A presence
// octet outside {0, 1} is therefore ErrOptionalPresence rather than "present".
//
// Both halves take a callback that returns an error, because a nested MLS encoder
// or decoder has semantic refusals — a credential type it will not accept, a
// content and arm that do not match — that are not buffer errors and that the
// sticky error has no exported way to carry. The refusal is both returned and set
// sticky on the Writer or the Reader it came from, so it surfaces whether the
// caller checks the return or only checks Bytes or Done. A dropped refusal on the
// encode side would produce wrong signed bytes rather than a failure; a dropped
// refusal on the decode side would leave a Reader parked partway through a
// structure it never accepted, reporting success from Done. Neither is an outcome
// this package allows.
package syntax

// WriteOptional is the encode half; encodeOne runs only when present, and appends
// the value with no framing of its own beyond the presence octet already written.
// A refusal from encodeOne is returned and also latched, so the failure is
// unavoidable at Bytes even if the return is dropped. A no op reporting the
// existing failure once the Writer has already failed.
func (self *Writer) WriteOptional(present bool, encodeOne func(w *Writer) error) error {
	if self.err != nil {
		return self.err
	}
	if !present {
		self.WriteUint8(0)
		return nil
	}
	self.WriteUint8(1)
	if err := encodeOne(self); err != nil {
		self.setErr(err)
		return err
	}
	return nil
}

// ReadOptional is the decode half; it reports whether the value was present and
// runs decodeOne only when it was. The presence octet is validated before the
// cursor moves, so a truncated input and a malformed octet both leave the cursor
// exactly where it stood, and every failure latches into the Reader — a caller
// that ignores the error cannot have a later, smaller read reinterpret the same
// unconsumed presence octet as a different field and succeed. That includes a
// refusal from decodeOne, which is the one failure no read of its own can latch:
// a nested decoder can consume its bytes successfully and still refuse on
// semantic grounds, and without latching it the Reader would sit partway through
// a structure that was never accepted while Done reported nil or, worse,
// ErrTrailingBytes for an unrelated tail. This mirrors what the encode half does
// with the same refusal, for the same reason.
//
// The error returned for a decodeOne refusal is the callback's own rather than
// the latched one, since the callback's is at least as specific; where the
// callback failed because a read failed, first error wins keeps the read's cause
// as what Done reports.
//
// present reports what the presence octet said, so on a decodeOne refusal it is
// true: the value was on the wire and only its decoding failed. Reporting false
// there would tell a caller that branches on present before checking err that an
// optional a peer sent was absent, silently downgrading a present value to an
// absent one — for optional<Node> in the ratchet tree, a blank where a node
// stood. It is false only where no valid presence octet was read at all, which is
// every other failure here, and in those cases err is the only meaningful result.
func (self *Reader) ReadOptional(decodeOne func(r *Reader) error) (bool, error) {
	if self.err != nil {
		return false, self.err
	}
	if self.Remaining() < 1 {
		self.setErr(ErrTruncated)
		return false, self.err
	}
	b := self.bs[self.pos]
	if b > 1 {
		self.setErr(ErrOptionalPresence)
		return false, self.err
	}
	self.pos += 1
	if b == 0 {
		return false, nil
	}
	// the cursor is left wherever the nested decode stopped rather than restored:
	// those reads did happen, and the latch above is what keeps the Reader dead
	// afterwards, so the position is informational either way.
	if err := decodeOne(self); err != nil {
		self.setErr(err)
		return true, err
	}
	return true, nil
}
