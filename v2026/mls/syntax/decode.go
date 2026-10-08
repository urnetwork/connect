// The decode half of the codec, and the mirror of Writer in encode.go. Reader is a
// cursor over a byte slice: every read is bounds checked against the bytes that
// remain before it advances the cursor, so a failed read leaves the cursor exactly
// where it was. The first failure is sticky: it latches into the Reader, so every
// later read and Done report that same error instead of running their own check
// against bytes the failed read never validated. Without this, a caller that
// ignores one failed read could have a later, smaller read silently succeed
// against the same untouched bytes and reinterpret them as a different field —
// a structurally valid decode of the wrong fields, with no error to catch it. So
// the first error reported is always the cause rather than a downstream symptom
// of it, matching the sticky error Writer carries in encode.go, for the same
// reason: a caller that does not check every return should not get a silently
// wrong decode. This file carries the cursor and the fixed-width portion of the
// contract — ReadUint8/16/32/64, ReadRaw and the full consumption check Done.
// Tasks 6 and 8-11 add the variable length reads (ReadVarint, ReadOpaque,
// ReadOpaqueLP, ReadSub, ReadSubLP, ReadOptional) to this same file; those are
// where a declared length gets checked against the configured maximum and the
// bytes that remain before anything is allocated, so a hostile length prefix can
// never size an allocation. ReadNested and ReadNestedLP sit on top of the sub
// reader pair and are what a nested structure should ordinarily be decoded
// through: they run the region to empty and latch anything that goes wrong inside
// it onto this Reader, so neither obligation depends on a call site remembering.
//
// ReadRaw's result is a copy, never a view into the input: MLS verifies a
// signature over serialized bytes and then uses the decoded fields, so a field
// that aliases a buffer someone else may reuse is a correctness hazard as well as
// a way to pin a large buffer behind a small field.
//
// Not safe for concurrent use.
package syntax

import "encoding/binary"

// Reader is a bounds-checked cursor over a byte slice. Every read checks the bytes
// that remain before it advances the cursor, so a failed read never moves it, and
// the first failure sticks: once any read fails, every subsequent read and Done
// report that same error instead of re-checking bytes the failed read never
// validated, matching Writer's sticky error in encode.go. Not safe for
// concurrent use.
type Reader struct {
	bs              []byte
	pos             int
	maxVectorLength int
	// err is the sticky error. It starts out set only when NewReaderLimit is
	// given a negative limit — the same validate-at-construction precedent
	// NewWriterLimit follows in encode.go — and from then on every read method
	// that fails also calls setErr, first error wins. Checked first by every
	// method that returns an error, so a caller who ignores one failed read
	// cannot have a later read silently reinterpret the same, still-unconsumed
	// bytes as a different, smaller field and succeed.
	err error
}

// NewReader returns a Reader over bs bounded by the default vector length limit
// MaxVectorLength, which is correct for every field but the ratchet tree.
func NewReader(bs []byte) *Reader {
	return &Reader{
		bs:              bs,
		maxVectorLength: MaxVectorLength,
	}
}

// NewReaderLimit returns a Reader over bs bounded by a caller chosen vector length
// limit; the ratchet tree paths pass MaxRatchetTreeLength and nothing else raises
// it. Zero is a legitimate limit: a Reader that accepts no variable length
// content. A negative limit is the API misuse ErrNegativeLength documents: the
// returned Reader carries ErrNegativeLength as its sticky error from
// construction, so every read on it reports that error immediately instead of
// running its ordinary bounds check.
func NewReaderLimit(bs []byte, maxVectorLength int) *Reader {
	r := &Reader{
		bs:              bs,
		maxVectorLength: maxVectorLength,
	}
	if maxVectorLength < 0 {
		r.err = ErrNegativeLength
	}
	return r
}

// Offset returns the current cursor position: the number of bytes consumed so
// far. Like Writer.Len, it does not reflect the sticky construction time error.
func (self *Reader) Offset() int {
	return self.pos
}

// Remaining returns the number of bytes not yet consumed.
func (self *Reader) Remaining() int {
	return len(self.bs) - self.pos
}

// Empty reports whether every byte of the input has been consumed.
func (self *Reader) Empty() bool {
	return self.pos >= len(self.bs)
}

// MaxVectorLength returns the vector length limit this Reader was constructed
// with, either the package default or a caller supplied override.
func (self *Reader) MaxVectorLength() int {
	return self.maxVectorLength
}

// setErr records err as the sticky error if none has been recorded yet; first
// error wins, so a later, unrelated failure never overwrites the cause — the
// same first-error-wins rule Writer.setErr enforces in encode.go.
func (self *Reader) setErr(err error) {
	if self.err == nil {
		self.err = err
	}
}

// Done returns the sticky error if one was set — either the construction time
// misuse or a prior read's latched failure — otherwise ErrTrailingBytes if bytes
// remain unconsumed, otherwise nil. This is the full consumption rule: a decoder
// that ignores a tail accepts two encodings of one object, and MLS signs over
// serialized forms.
func (self *Reader) Done() error {
	if self.err != nil {
		return self.err
	}
	if !self.Empty() {
		return ErrTrailingBytes
	}
	return nil
}

// ReadUint8 reads one byte. Reports ErrTruncated if none remain; the cursor does
// not advance on failure, and the failure latches: every later read and Done on
// this Reader report the same error instead of running their own check.
func (self *Reader) ReadUint8() (uint8, error) {
	if self.err != nil {
		return 0, self.err
	}
	if self.Remaining() < 1 {
		self.setErr(ErrTruncated)
		return 0, self.err
	}
	v := self.bs[self.pos]
	self.pos += 1
	return v, nil
}

// ReadUint16 reads two bytes, most significant byte first. Reports ErrTruncated
// if fewer than two remain; the cursor does not advance on failure, and the
// failure latches: every later read and Done on this Reader report the same
// error instead of running their own check.
func (self *Reader) ReadUint16() (uint16, error) {
	if self.err != nil {
		return 0, self.err
	}
	if self.Remaining() < 2 {
		self.setErr(ErrTruncated)
		return 0, self.err
	}
	v := binary.BigEndian.Uint16(self.bs[self.pos:])
	self.pos += 2
	return v, nil
}

// ReadUint32 reads four bytes, most significant byte first. Reports ErrTruncated
// if fewer than four remain; the cursor does not advance on failure, and the
// failure latches: every later read and Done on this Reader report the same
// error instead of running their own check.
func (self *Reader) ReadUint32() (uint32, error) {
	if self.err != nil {
		return 0, self.err
	}
	if self.Remaining() < 4 {
		self.setErr(ErrTruncated)
		return 0, self.err
	}
	v := binary.BigEndian.Uint32(self.bs[self.pos:])
	self.pos += 4
	return v, nil
}

// ReadUint64 reads eight bytes, most significant byte first. Reports
// ErrTruncated if fewer than eight remain; the cursor does not advance on
// failure, and the failure latches: every later read and Done on this Reader
// report the same error instead of running their own check.
func (self *Reader) ReadUint64() (uint64, error) {
	if self.err != nil {
		return 0, self.err
	}
	if self.Remaining() < 8 {
		self.setErr(ErrTruncated)
		return 0, self.err
	}
	v := binary.BigEndian.Uint64(self.bs[self.pos:])
	self.pos += 8
	return v, nil
}

// ReadRaw reads the next n bytes verbatim: opaque x[N], for a field whose length
// the surrounding structure already fixes. The result is always a copy, never a
// view into the input, so a decoded field cannot be mutated through the buffer
// it came from and cannot pin it. Reports ErrNegativeLength if n is negative;
// reports ErrTruncated if fewer than n bytes remain. The cursor does not advance
// on failure, and the failure latches: every later read and Done on this Reader
// report the same error instead of running their own check.
func (self *Reader) ReadRaw(n int) ([]byte, error) {
	if self.err != nil {
		return nil, self.err
	}
	if n < 0 {
		self.setErr(ErrNegativeLength)
		return nil, self.err
	}
	if n > self.Remaining() {
		self.setErr(ErrTruncated)
		return nil, self.err
	}
	out := make([]byte, n)
	copy(out, self.bs[self.pos:self.pos+n])
	self.pos += n
	return out, nil
}

// validateLength checks a declared length before the caller may allocate
// anything sized by it: first against self.err, so a Reader that already carries
// a latched failure can never validate n and hand back a clean result, which
// would revive reads the sticky contract says must stay dead — matching every
// other error-returning method in this file. Then against this Reader's
// configured maximum, reporting
// ErrLengthExceedsMax, then — only once that check has passed — against the
// bytes actually remaining in the input, reporting ErrLengthExceedsInput. The two
// failures are kept distinct because downstream callers need to tell "this field
// is simply too large" apart from "this input was truncated or lied about its
// length." n is a uint32 straight from ReadVarint and both comparisons widen to
// int64 before comparing, so a length near the uint32 range on a platform where
// int is 32 bits can never wrap around into looking small and passing either
// check; only once both checks pass is n narrowed to int for the caller to size a
// make with. Named for what it does, not for a cursor motion: it validates and
// deliberately does not touch the cursor, leaving that to the caller.
func (self *Reader) validateLength(n uint32) (int, error) {
	if self.err != nil {
		return 0, self.err
	}
	if int64(n) > int64(self.maxVectorLength) {
		self.setErr(ErrLengthExceedsMax)
		return 0, self.err
	}
	if int64(n) > int64(self.Remaining()) {
		self.setErr(ErrLengthExceedsInput)
		return 0, self.err
	}
	return int(n), nil
}

// takeRegion is the shared tail of every length prefixed read. It validates the
// length a prefix has just declared and, only once that passes, consumes the
// region that length describes, handing back where the region starts so the
// caller can copy it out or build a view over it. mark is the cursor from before
// the prefix was read: a refused length restores it, so a caller who ignores the
// error is never left parked partway inside a field that was rejected — the
// prefix has to be consumed before the length can be checked against the bytes
// remaining after it, so there is always something to give back. Latching is
// validateLength's job and already done by the time this returns, so what is left
// here is only the cursor. It exists because ReadOpaque, ReadOpaqueLP, ReadSub
// and ReadSubLP differ solely in how the prefix is spelled and in what they build
// from the region; four copies of the validate, restore and advance sequence is
// how they would drift apart, and the one that drifted would be the one missing
// the restore.
func (self *Reader) takeRegion(mark int, n uint32) (int, int, error) {
	length, err := self.validateLength(n)
	if err != nil {
		self.pos = mark
		return 0, 0, err
	}
	start := self.pos
	self.pos += length
	return start, length, nil
}

// subReader builds the bounded view an already validated and already consumed
// region describes. The view is deliberately not a copy — it is read only, and a
// nested structure would otherwise cost a copy of every byte at every level of
// nesting — but its capacity is clipped to its length by the three index slice,
// so an append inside it allocates a fresh array instead of writing over the
// bytes the parent has not read yet. It inherits the parent's vector length
// limit, since the ratchet tree's raised maximum has to survive the step into a
// nested structure, and it starts with no sticky error of its own: a failure
// inside the region belongs to the region.
func (self *Reader) subReader(start int, length int) *Reader {
	return &Reader{
		bs:              self.bs[start : start+length : start+length],
		pos:             0,
		maxVectorLength: self.maxVectorLength,
		err:             nil,
	}
}

// ReadOpaque decodes opaque x<V>: a varint length prefix read by ReadVarint,
// then that many bytes. The declared length is validated by validateLength —
// against the configured maximum and then against the bytes actually remaining —
// before any allocation, so a hostile length prefix can never size a make; the
// two rejections surface as the distinct sentinels ErrLengthExceedsMax and
// ErrLengthExceedsInput. The result is always a copy, never a view into the
// input, matching ReadRaw, and is never nil even for a zero length value, so an
// empty opaque field and an absent one stay distinguishable in Go despite
// encoding identically on the wire. On any failure the cursor is restored to
// where it stood before this call — not left partway past a validly decoded
// varint whose length then failed — and the failure latches: every later read
// and Done on this Reader report the same error.
func (self *Reader) ReadOpaque() ([]byte, error) {
	if self.err != nil {
		return nil, self.err
	}
	mark := self.pos
	n, err := self.ReadVarint()
	if err != nil {
		return nil, err
	}
	start, length, err := self.takeRegion(mark, n)
	if err != nil {
		return nil, err
	}
	out := make([]byte, length)
	copy(out, self.bs[start:start+length])
	return out, nil
}

// ReadOpaqueLP decodes LP(x): a fixed 32 bit big endian length read by
// ReadUint32, then that many bytes. This is the record layer's form that
// connect/message uses for every record field and every AAD and write_auth
// preimage, not the varint prefixed form MLS structures use — that one is
// ReadOpaque, and the two are never interchangeable. The declared length is
// validated by validateLength — against the configured maximum and then against
// the bytes actually remaining — before any allocation, so a prefix declaring up
// to four gibibytes can never size a make; the two rejections surface as the
// distinct sentinels ErrLengthExceedsMax and ErrLengthExceedsInput. The prefix
// must be consumed before validateLength runs, since that check is against the
// bytes remaining after it, so on a failure the cursor is restored to where it
// stood before this call rather than left four bytes past a prefix whose length
// was then refused. The result is always a copy, never a view into the input,
// matching ReadRaw, and is never nil even for a zero length value. Every failure
// latches: later reads and Done report the same error.
func (self *Reader) ReadOpaqueLP() ([]byte, error) {
	if self.err != nil {
		return nil, self.err
	}
	mark := self.pos
	n, err := self.ReadUint32()
	if err != nil {
		return nil, err
	}
	start, length, err := self.takeRegion(mark, n)
	if err != nil {
		return nil, err
	}
	out := make([]byte, length)
	copy(out, self.bs[start:start+length])
	return out, nil
}

// ReadSub returns a bounded view over the next opaque x<V> region, for a
// structure carried inside an opaque field: the extension bodies and GroupInfo
// paths of later waves, and ReadVector. It is the raw escape hatch, not the
// default — reach for ReadNested unless the raw view is what is actually wanted,
// because ReadNested discharges the obligation described below instead of handing
// it back. The two carry deliberately different contracts and this one is for a
// caller that genuinely needs the independent Reader: one that means to tolerate
// an unparseable region rather than fail on it, or to consume part of a region
// and decide the rest is not its business.
//
// The prefix and its length go through the same ReadVarint and validateLength
// this file's other variable length reads use, so a hostile length is refused
// before anything is sized by it, and on a rejection the cursor is restored to
// where it stood before the call and the failure latches into this Reader.
//
// The parent advances past the whole region however much of it the sub reader
// consumes, so a caller that stops early — or that abandons the nested structure
// after a failure inside it — cannot desynchronise the parent, and a failure
// inside the region latches on the sub reader alone rather than taking the rest
// of the message down with it.
//
// That is also the one obligation this hands back to the caller: because the
// parent skips the region wholesale, bytes the sub reader leaves behind are
// invisible to the parent, whose own Done reports success. The caller must call
// Done on the returned Reader once the nested structure is finished. Without
// it, a region longer than the structure inside it is silently accepted, which is
// a second valid encoding of one object — the same class of defect the varint's
// minimal form rule exists to prevent, and it matters for the same reason: MLS
// signs over serialized bytes, so an encoding a verifier accepts but a signer
// never produced is a signature bypass primitive. An obligation stated in a doc
// comment is only as good as the call sites that remember it, which is why
// ReadNested exists and why it, rather than this, is what a nested structure
// should ordinarily be decoded through.
func (self *Reader) ReadSub() (*Reader, error) {
	if self.err != nil {
		return nil, self.err
	}
	mark := self.pos
	n, err := self.ReadVarint()
	if err != nil {
		return nil, err
	}
	start, length, err := self.takeRegion(mark, n)
	if err != nil {
		return nil, err
	}
	return self.subReader(start, length), nil
}

// ReadSubLP is ReadSub over an LP(x) region, for a record field that carries a
// structure: connect/message nests inside the fixed 32 bit prefix exactly as MLS
// nests inside the varint one, and the two prefixes are never interchangeable.
// Every property ReadSub documents holds here unchanged, including its status as
// the escape hatch rather than the default and the caller's obligation to call
// Done on the returned Reader — the parent skips the whole region either way, so
// only the sub reader can see bytes left over inside it. ReadNestedLP is the form
// that discharges that obligation for the caller.
func (self *Reader) ReadSubLP() (*Reader, error) {
	if self.err != nil {
		return nil, self.err
	}
	mark := self.pos
	n, err := self.ReadUint32()
	if err != nil {
		return nil, err
	}
	start, length, err := self.takeRegion(mark, n)
	if err != nil {
		return nil, err
	}
	return self.subReader(start, length), nil
}

// ReadNested decodes a structure carried inside an opaque x<V> region, and is the
// form a nested structure should ordinarily go through. It takes the region with
// ReadSub, runs decodeOne against the bounded view, and then calls Done on that
// view itself, folding the answer into what it returns. Full consumption of the
// region is therefore enforced structurally rather than by convention: a region
// longer than the structure inside it is a second valid encoding of one object,
// and in a codec whose serialized forms MLS signs over an encoding a verifier
// accepts but a signer never produced is a signature bypass primitive. ReadSub
// hands that obligation back to its caller and every one of those call sites is a
// place it can be forgotten; here there is no call site to forget it.
//
// Every failure — decodeOne's own refusal and the region's leftover bytes alike —
// is latched on this Reader rather than only returned, and this is the same
// reasoning ReadVector documents, which is where the rule was first established.
// ReadSub has already advanced this Reader past the entire region by the time
// decodeOne runs, and a failure inside the region latches on the sub reader, which
// is a separate Reader the caller never receives; a semantic refusal from
// decodeOne latches nothing at all, since its own reads all succeeded. So a caller
// that drops the return would hold a Reader that is clean, positioned at the next
// field, and reporting nil from Done, having silently skipped a region that was
// never accepted. Latching is what makes that impossible.
//
// The returned error is this Reader's sticky error, which is always the failure
// just recorded: ReadSub's entry check has already established that no error was
// carried when the nested decode began, so first error wins has nothing older to
// prefer.
func (self *Reader) ReadNested(decodeOne func(r *Reader) error) error {
	// no entry check on self.err is needed or observable: ReadSub is the first
	// thing this touches and it checks self.err itself, so a latched Reader fails
	// here with nothing read and decodeOne never runs — the same reasoning
	// ReadVector records for the same delegation.
	sub, err := self.ReadSub()
	if err != nil {
		return err
	}
	if err := decodeOne(sub); err != nil {
		self.setErr(err)
		return self.err
	}
	if err := sub.Done(); err != nil {
		self.setErr(err)
		return self.err
	}
	return nil
}

// ReadNestedLP is ReadNested over an LP(x) region, for a record field that carries
// a structure: connect/message nests inside the fixed 32 bit prefix exactly as MLS
// nests inside the varint one, and the two prefixes are never interchangeable.
// Every property ReadNested documents holds here unchanged — the region is run to
// empty, and both a decoder refusal and leftover bytes latch on this Reader rather
// than only being returned.
func (self *Reader) ReadNestedLP(decodeOne func(r *Reader) error) error {
	sub, err := self.ReadSubLP()
	if err != nil {
		return err
	}
	if err := decodeOne(sub); err != nil {
		self.setErr(err)
		return self.err
	}
	if err := sub.Done(); err != nil {
		self.setErr(err)
		return self.err
	}
	return nil
}
