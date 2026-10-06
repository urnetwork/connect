// The encode half of the codec. Writer accumulates bytes and carries a sticky
// error: the first failure is retained, every write after it is a no op, and Bytes
// reports it. Encoding a value is a straight line of writes, so one check at the
// end is both sufficient and impossible to skip, because Bytes will not hand back
// the bytes without also handing back the error.
//
// Not safe for concurrent use.
package syntax

import "encoding/binary"

// Writer accumulates the TLS presentation language encoding of an MLS structure.
// Not safe for concurrent use.
type Writer struct {
	bs              []byte
	err             error
	maxVectorLength int
}

// NewWriter returns a Writer bounded by the default vector length limit
// MaxVectorLength, which is correct for every field but the ratchet tree.
func NewWriter() *Writer {
	return &Writer{
		bs:              nil,
		err:             nil,
		maxVectorLength: MaxVectorLength,
	}
}

// NewWriterLimit returns a Writer bounded by a caller chosen vector length limit;
// the ratchet tree paths pass MaxRatchetTreeLength and nothing else raises it. Zero
// is a legitimate limit: a Writer that accepts no variable length content. A
// negative limit is the API misuse ErrNegativeLength documents: the returned
// Writer carries ErrNegativeLength as its sticky error from construction, so
// every write on it is a no op and Bytes reports the error.
func NewWriterLimit(maxVectorLength int) *Writer {
	w := &Writer{
		bs:              nil,
		err:             nil,
		maxVectorLength: maxVectorLength,
	}
	if maxVectorLength < 0 {
		w.setErr(ErrNegativeLength)
	}
	return w
}

// Bytes returns the accumulated encoding, or the first error seen. The returned
// bytes are nil whenever the error is non nil, so a caller cannot take a
// truncated encoding by accident: bs is never mutated once err is set, by any
// method in this package. The returned slice aliases the Writer's internal
// buffer and is only valid until the next Write* call on this Writer; a caller
// that needs to keep it must copy.
func (self *Writer) Bytes() ([]byte, error) {
	if self.err != nil {
		return nil, self.err
	}
	return self.bs, nil
}

// Err returns the first error set on this Writer, or nil if no write has failed.
func (self *Writer) Err() error {
	return self.err
}

// Len returns the number of bytes accumulated so far. It does not reflect
// pending writes suppressed by a sticky error, since those never happened.
func (self *Writer) Len() int {
	return len(self.bs)
}

// MaxVectorLength returns the vector length limit this Writer was constructed
// with, either the package default or a caller supplied override.
func (self *Writer) MaxVectorLength() int {
	return self.maxVectorLength
}

// setErr records err as the sticky error if none has been recorded yet; first
// error wins, so the reported failure is the cause rather than a downstream
// symptom of it.
func (self *Writer) setErr(err error) {
	if self.err == nil {
		self.err = err
	}
}

// WriteUint8 appends v as a single byte. A no op once the Writer has failed.
func (self *Writer) WriteUint8(v uint8) {
	if self.err != nil {
		return
	}
	self.bs = append(self.bs, v)
}

// WriteUint16 appends v as two big endian bytes. A no op once the Writer has failed.
func (self *Writer) WriteUint16(v uint16) {
	if self.err != nil {
		return
	}
	self.bs = binary.BigEndian.AppendUint16(self.bs, v)
}

// WriteUint32 appends v as four big endian bytes. A no op once the Writer has failed.
func (self *Writer) WriteUint32(v uint32) {
	if self.err != nil {
		return
	}
	self.bs = binary.BigEndian.AppendUint32(self.bs, v)
}

// WriteUint64 appends v as eight big endian bytes. A no op once the Writer has failed.
func (self *Writer) WriteUint64(v uint64) {
	if self.err != nil {
		return
	}
	self.bs = binary.BigEndian.AppendUint64(self.bs, v)
}

// WriteRaw appends bs verbatim with no length prefix: opaque x[N], for a field
// whose length the surrounding structure already fixes. A nil or empty bs writes
// nothing. A no op once the Writer has failed.
func (self *Writer) WriteRaw(bs []byte) {
	if self.err != nil {
		return
	}
	self.bs = append(self.bs, bs...)
}

// WriteOpaque appends bs as opaque x<V> — the RFC 9420 section 2.1.2 varint
// length prefix, then the bytes verbatim — and is the encoder every opaque field
// in this codec goes through. A nil and an empty slice both encode to the single
// zero length prefix octet, since the wire format has no separate representation
// for "absent": len(bs) is what WriteVarint sees either way. The length is
// checked against this Writer's configured maximum before either write happens,
// so a caller cannot silently produce a field a compliant reader would refuse; a
// no op once the Writer has already failed.
func (self *Writer) WriteOpaque(bs []byte) {
	if self.err != nil {
		return
	}
	if len(bs) > self.maxVectorLength {
		self.setErr(ErrLengthExceedsMax)
		return
	}
	self.WriteVarint(uint32(len(bs)))
	self.WriteRaw(bs)
}

// WriteOpaqueLP appends bs as LP(x) in the master protocol design's notation: a
// fixed 32 bit big endian length, then the bytes verbatim. This is the record
// layer's prefix and not MLS's — connect/message builds every record field and
// every AAD and write_auth preimage with it, where a fixed width prefix keeps a
// preimage's field boundaries independent of the lengths inside it. It never
// appears inside an MLS structure, where the form is WriteOpaque, and the two are
// never interchangeable. A nil and an empty slice both encode to the four zero
// octets, since the wire format has no representation for "absent". The length is
// checked against this Writer's configured maximum before either write happens,
// so a caller cannot silently produce a field a compliant reader would refuse; a
// no op once the Writer has already failed.
func (self *Writer) WriteOpaqueLP(bs []byte) {
	if self.err != nil {
		return
	}
	if len(bs) > self.maxVectorLength {
		self.setErr(ErrLengthExceedsMax)
		return
	}
	self.WriteUint32(uint32(len(bs)))
	self.WriteRaw(bs)
}

// nestedRegion encodes a nested structure into a scratch Writer and hands back the
// bytes it produced, for a caller that will then frame them. It is the shared body
// of WriteNested and WriteNestedLP, which differ only in how the region's length
// prefix is spelled, and it exists as one copy for the same reason takeRegion does
// on the decode side: the sequence is short, and the line inside it that is easy to
// get wrong is the one that must not be got wrong twice.
//
// That line is the scratch Writer's construction. The limit is inherited from this
// Writer rather than defaulted, because the limit belongs to the encode as a whole
// and not to the depth at which a field happens to sit. A plain NewWriter here would
// cap every nested field at MaxVectorLength even inside a ratchet tree encode
// running at MaxRatchetTreeLength, and the failure would be silent on every small
// input and appear only on a large tree — a refusal to encode a structure the
// protocol allows, in the case that is hardest to reach in a test and likeliest to
// arrive in production. The same inheritance is what WriteVector does for its
// elements and what subReader does for a nested decode.
//
// Both failures are latched on this Writer as well as returned: encodeOne's own
// semantic refusal, and any failure the scratch Writer latched that encodeOne did
// not report, which is the ordinary shape of a leaf write failing inside an encoder
// whose leaf writes are return free and which therefore returns nil. The returned
// error is the callback's own where there is one, matching WriteOptional; this
// Writer carried no error when the nested encode began, since the callers check that
// on entry, so first error wins has nothing older to prefer either way.
func (self *Writer) nestedRegion(encodeOne func(w *Writer) error) ([]byte, error) {
	scratch := NewWriterLimit(self.maxVectorLength)
	if err := encodeOne(scratch); err != nil {
		self.setErr(err)
		return nil, err
	}
	region, err := scratch.Bytes()
	if err != nil {
		self.setErr(err)
		return nil, err
	}
	return region, nil
}

// WriteNested encodes a structure into a region of its own and appends that region
// as opaque x<V>, and is the encode side counterpart to ReadNested: the form a
// structure carried inside a varint prefixed region should ordinarily be written
// through. The region has to be built before it can be framed, since its length is
// the prefix, so the structure is encoded into a scratch Writer and the result goes
// out through WriteOpaque, which is where the prefix and the vector length limit are
// applied.
//
// The scratch Writer inherits this Writer's limit. That is the whole point of the
// helper: the alternative every call site would otherwise hand roll is a plain
// NewWriter, which caps the nested field at MaxVectorLength no matter what limit the
// surrounding encode is running at, so a ratchet tree encode at
// MaxRatchetTreeLength would refuse a nested field it is entitled to write. Nothing
// smaller than a multi mebibyte tree shows the difference, which is why the line is
// here once rather than in every encoder.
//
// A refusal from encodeOne, a failure the scratch Writer latched, and a region too
// long for this Writer's limit are all both returned and latched, so the failure is
// unavoidable at Bytes even if the return is dropped. A dropped refusal on this side
// of the codec produces wrong signed bytes rather than a failure, and MLS signs over
// serialized forms, so that is the one outcome this package does not allow. That
// last case is also why the successful path returns the sticky error rather than a
// bare nil: WriteOpaque is return free and reports an over long region through the
// sticky error alone, and a caller checking this return deserves to hear about it
// there too, exactly as WriteVector does. A no op reporting the existing failure once
// this Writer has already failed, which also keeps a nested encoder's side effects
// and refusals out of an encoding that will never be handed out.
func (self *Writer) WriteNested(encodeOne func(w *Writer) error) error {
	if self.err != nil {
		return self.err
	}
	region, err := self.nestedRegion(encodeOne)
	if err != nil {
		return err
	}
	self.WriteOpaque(region)
	return self.err
}

// WriteNestedLP is WriteNested framed as LP(x): the fixed 32 bit big endian prefix
// connect/message uses for a record field that carries a structure, rather than the
// varint prefix MLS structures use. The two are never interchangeable, and a
// structure written with the wrong one is a record no reader of either layer will
// accept. Every property WriteNested documents holds here unchanged, including the
// inherited scratch limit and the latching of every failure.
func (self *Writer) WriteNestedLP(encodeOne func(w *Writer) error) error {
	if self.err != nil {
		return self.err
	}
	region, err := self.nestedRegion(encodeOne)
	if err != nil {
		return err
	}
	self.WriteOpaqueLP(region)
	return self.err
}
