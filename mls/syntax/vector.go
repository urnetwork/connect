// T items<V>: a byte length prefix, then the concatenated elements.
//
// The prefix counts bytes, not elements. That is the single most common way to get
// an MLS codec wrong, so decoding never counts down an element count: it takes a
// sub reader over exactly the declared region and runs the element decoder until
// that sub reader is empty. A declared length that does not land on an element
// boundary therefore surfaces as a truncated final element rather than as a
// silently accepted short vector.
//
// The element count is bounded: every element must consume at least one byte, so
// the count cannot exceed the declared byte length, which cannot exceed the
// reader's limit, which cannot exceed the input. An element decoder that consumes
// nothing is rejected rather than allowed to loop.
//
// These are free generics over a typed slice rather than methods over an index
// loop, because the element type is what makes the decode side safe. Both halves
// take a callback that returns an error, because a nested MLS encoder or decoder
// has semantic refusals that are not buffer errors, and the refusal is both
// returned and latched on the Writer or the Reader the caller handed in — the same
// contract optional.go documents, and it matters more here than there. On the
// decode side a failure inside the region latches on the sub reader, which the
// caller never sees, and the parent has already been advanced past the whole
// region by ReadSub; so without latching on the parent a caller that drops the
// return is left with a parent that is clean, positioned at the next field, and
// reporting nil from Done, having silently skipped a vector whose contents were
// never accepted. In a codec whose serialized forms MLS signs over that is a
// structurally valid decode of an object the peer did not send.
package syntax

// WriteVector encodes items as T items<V>: the elements are encoded first into a
// scratch Writer so their combined byte length is known, then that buffer goes out
// through WriteOpaque, which is where the varint prefix and the vector length
// limit are applied. The scratch Writer inherits the outer limit so a nested
// opaque field inside an element is bounded exactly as it would be if it had been
// written directly.
//
// A nil and an empty slice both encode to the single zero length prefix octet,
// since the wire format has no representation for "absent" — the same rule
// WriteOpaque follows for a nil body.
//
// Every failure is both returned and latched on the outer Writer, so it is
// unavoidable at Bytes even if the return is dropped: an element encoder's
// semantic refusal, a buffer failure raised while encoding into the scratch, and
// the combined length exceeding the limit. That last one is why the return is
// w.err rather than a bare nil at the end; WriteOpaque is return free and reports
// an over long vector only through the sticky error, and a caller checking the
// return of this call deserves to hear about it there too. A no op reporting the
// existing failure once the Writer has already failed, which also keeps an element
// encoder's side effects and refusals out of an encoding that will never be handed
// out.
func WriteVector[T any](w *Writer, items []T, encodeOne func(w *Writer, item T) error) error {
	if w.err != nil {
		return w.err
	}
	scratch := NewWriterLimit(w.maxVectorLength)
	for _, item := range items {
		if err := encodeOne(scratch, item); err != nil {
			w.setErr(err)
			return err
		}
	}
	bs, err := scratch.Bytes()
	if err != nil {
		w.setErr(err)
		return err
	}
	w.WriteOpaque(bs)
	return w.err
}

// ReadVector decodes T items<V> by taking a bounded view over the declared region
// with ReadSub and running decodeOne against that view until it is empty, rather
// than by counting elements — the prefix counts bytes, and the two are only the
// same thing for a fixed width element. The result is never nil, so an empty
// vector and an absent one stay distinguishable in Go despite encoding
// identically on the wire.
//
// The capacity hint is bounded by a constant rather than by the declared length:
// an element cannot be shorter than one byte, so the region length is a correct
// upper bound on the element count, but it is attacker chosen, so the hint is
// capped at 64. What a hostile input can therefore buy is at most 64 elements'
// worth of zero values per call, a compile time constant multiple of the element
// size, and since each call consumes at least the one prefix octet the total stays
// linear in the input.
//
// Every failure inside the region is latched on the Reader the caller handed in,
// not merely returned. This is the one place where returning alone would be worse
// than it is anywhere else in this package: ReadSub has already advanced the
// parent past the entire region by the time decodeOne runs, and a failure inside
// the region latches on the sub reader, which is a separate Reader the caller
// never receives. A semantic refusal from decodeOne latches nothing at all, since
// its own reads all succeeded. So a caller that drops the return would hold a
// parent that is clean, positioned at the next field, and reporting nil from Done,
// having accepted a structure whose vector was never decoded. Latching on the
// parent is what makes that impossible, and it mirrors what ReadOptional does with
// the same refusal for the same reason.
//
// The returned error is the parent's sticky error, which is always the callback's
// own or the sentinel just recorded: ReadSub's entry check has already established
// that the parent carried no error when the loop began, so first error wins has
// nothing older to prefer.
//
// The trailing Done on the sub reader is the obligation ReadSub hands its caller,
// discharged here rather than passed on, and it is not redundant with the loop
// condition. The loop already runs the region to empty, so there are never
// leftover bytes; what Done still catches is an element decoder that had a read
// fail, ignored its own error and returned successfully with the region happening
// to end there. That read latched on the sub reader and nothing else would ever
// look at it, so the last element would be built from bytes that were never there.
func ReadVector[T any](r *Reader, decodeOne func(r *Reader) (T, error)) ([]T, error) {
	// no entry check on r.err is needed or observable: ReadSub is the first thing
	// this touches and it checks r.err itself, so a latched parent fails here with
	// nothing read and decodeOne never runs. ReadOptional needs its own check only
	// because it indexes the byte slice directly.
	sub, err := r.ReadSub()
	if err != nil {
		return nil, err
	}
	items := make([]T, 0, min(sub.Remaining(), 64))
	for !sub.Empty() {
		before := sub.Offset()
		item, err := decodeOne(sub)
		if err != nil {
			r.setErr(err)
			return nil, r.err
		}
		if sub.Offset() == before {
			r.setErr(ErrZeroLengthElement)
			return nil, r.err
		}
		items = append(items, item)
	}
	if err := sub.Done(); err != nil {
		r.setErr(err)
		return nil, r.err
	}
	return items, nil
}
