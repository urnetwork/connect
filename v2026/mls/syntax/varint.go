// The RFC 9420 section 2.1.2 variable length integer, and the length limits every
// variable length read in this package is bounded by.
package syntax

const (
	// the largest value the two bit prefixed varint can carry
	MaxVarint uint32 = 1<<30 - 1
	// the default cap on any single variable length field, spec A section 5.8
	MaxVectorLength int = 1 << 20
	// the cap raised for the ratchet tree only; tree_sync and group decode through
	// UnmarshalLimit with this value, everything else uses the default
	MaxRatchetTreeLength int = 1 << 24
)

// WriteVarint appends v as the RFC 9420 section 2.1.2 variable length integer:
// exactly one octet for 0..63, two for 64..16383, four for 16384..MaxVarint. The
// two most significant bits of the first octet are the prefix — the base 2
// logarithm of the octet count — so the widths never overlap and no value has a
// second valid encoding. Values above MaxVarint set the sticky ErrVarintOverflow
// and append nothing, matching every other Writer method's no op after failure.
func (self *Writer) WriteVarint(v uint32) {
	if self.err != nil {
		return
	}
	switch {
	case v <= 0x3f:
		self.bs = append(self.bs, byte(v))
	case v <= 0x3fff:
		self.bs = append(self.bs, byte(v>>8)|0x40, byte(v))
	case v <= MaxVarint:
		self.bs = append(self.bs, byte(v>>24)|0x80, byte(v>>16), byte(v>>8), byte(v))
	default:
		self.setErr(ErrVarintOverflow)
	}
}

// ReadVarint reads the RFC 9420 section 2.1.2 variable length integer WriteVarint
// produces, and rejects everything else: the reserved prefix 0b11 sets
// ErrVarintReserved, a value encoded wider than its minimal form sets
// ErrVarintNotMinimal, and fewer octets remaining than the prefix promises sets
// ErrTruncated. Because the width is fixed entirely by the first octet's top two
// bits, the whole varint is validated and consumed atomically: a failure never
// leaves the cursor partway through a value it refused to accept, matching every
// other Reader method's no-advance-on-failure contract. The minimality check is
// exactly the range check for each width: the two octet form is only valid for
// 64..16383 and the four octet form only for 16384..MaxVarint, so a value that
// fits narrower is refused rather than accepted a second way — this is the
// property the package's central security guarantee rests on, since MLS signs
// over serialized bytes and a second valid encoding of one value would let an
// attacker re-encode a signed structure without changing what it means.
func (self *Reader) ReadVarint() (uint32, error) {
	if self.err != nil {
		return 0, self.err
	}
	if self.Remaining() < 1 {
		self.setErr(ErrTruncated)
		return 0, self.err
	}
	b0 := self.bs[self.pos]
	switch b0 >> 6 {
	case 0:
		self.pos += 1
		return uint32(b0 & 0x3f), nil
	case 1:
		if self.Remaining() < 2 {
			self.setErr(ErrTruncated)
			return 0, self.err
		}
		v := uint32(b0&0x3f)<<8 | uint32(self.bs[self.pos+1])
		if v < 0x40 {
			self.setErr(ErrVarintNotMinimal)
			return 0, self.err
		}
		self.pos += 2
		return v, nil
	case 2:
		if self.Remaining() < 4 {
			self.setErr(ErrTruncated)
			return 0, self.err
		}
		v := uint32(b0&0x3f)<<24 |
			uint32(self.bs[self.pos+1])<<16 |
			uint32(self.bs[self.pos+2])<<8 |
			uint32(self.bs[self.pos+3])
		if v < 0x4000 {
			self.setErr(ErrVarintNotMinimal)
			return 0, self.err
		}
		self.pos += 4
		return v, nil
	default:
		self.setErr(ErrVarintReserved)
		return 0, self.err
	}
}
