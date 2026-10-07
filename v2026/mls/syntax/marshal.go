// The top level entry points every MLS structure and every record is encoded and
// decoded through, and the two interfaces every wire type in package mls
// implements.
//
// MarshalMLS returns an error and the Writer also carries a sticky one, and
// Marshal joins them. The two carry different things: the sticky error is a buffer
// failure — an over long vector, a varint out of range — and the returned error is
// a semantic refusal, which is an encoder declining to serialize a value the
// profile forbids, such as a credential type outside the v1 profile or a content
// arm that disagrees with its discriminant. This package exports no way to inject
// a refusal into the sticky error, so without the return an encoder would have to
// panic or drop it, and a dropped encoder refusal produces wrong signed bytes
// rather than a failure. Only the leaf writes stay return free, which is most of
// the call sites. UnmarshalMLS returns an error per call, because decoding is a
// branch per field and every branch matters.
//
// Unmarshal joins the same way, against Done rather than the sticky error. A
// decoder that ignores a tail accepts two encodings of one object, and MLS signs
// over serialized forms, so that is a signature bypass primitive rather than a
// leniency — and a decoder that refuses on semantic grounds partway through a
// message has left a tail behind it that the caller is entitled to hear about
// alongside the refusal. Returning early on the decoder's error would report the
// refusal and hide the tail; the join reports both, exactly as the encode half
// does.
package syntax

import (
	"bytes"
	"errors"
)

// Marshaler is the encode half of the one method set every wire type in package
// mls implements. The error is the encoder's own semantic refusal, distinct from
// the buffer failures the Writer accumulates in its sticky error.
type Marshaler interface {
	MarshalMLS(w *Writer) error
}

// Unmarshaler is the decode half. Decoding branches per field, so unlike the leaf
// writes there is no return free form of it.
type Unmarshaler interface {
	UnmarshalMLS(r *Reader) error
}

// Codec is both halves, which is what every MLS structure implements and asserts
// against with a var _ Codec line in its own file, so that a type which drifts out
// of the method set fails at build rather than at the interop gate.
type Codec interface {
	Marshaler
	Unmarshaler
}

// Marshal encodes v under the default vector length limit MaxVectorLength, which
// is correct for every structure but the ratchet tree.
func Marshal(v Marshaler) ([]byte, error) {
	return MarshalLimit(v, MaxVectorLength)
}

// MarshalLimit encodes v under a caller chosen vector length limit; the ratchet
// tree paths pass MaxRatchetTreeLength and nothing else raises the bound. Marshal
// is this with the default limit rather than a second copy of the sequence, so the
// two cannot drift apart in which errors they surface.
//
// The join is the point: a semantic refusal from the encoder and a buffer failure
// from the Writer are different failures, either one alone must fail the encode,
// and an encoder that refuses may well also have left the Writer in a state worth
// reporting. The bytes are dropped whenever either fires, so a caller cannot take
// a partial encoding by accident.
func MarshalLimit(v Marshaler, maxVectorLength int) ([]byte, error) {
	w := NewWriterLimit(maxVectorLength)
	marshalErr := v.MarshalMLS(w)
	bs, writerErr := w.Bytes()
	if err := errors.Join(marshalErr, writerErr); err != nil {
		return nil, err
	}
	return bs, nil
}

// Unmarshal decodes bs into v under the default vector length limit
// MaxVectorLength, enforcing full consumption.
func Unmarshal(bs []byte, v Unmarshaler) error {
	return UnmarshalLimit(bs, v, MaxVectorLength)
}

// UnmarshalLimit decodes bs into v under a caller chosen vector length limit; the
// ratchet tree paths — tree_sync.go, UnmarshalRatchetTree, and the GroupInfo or
// Welcome decodes that carry a tree — pass MaxRatchetTreeLength, and nothing else
// raises the bound. Unmarshal is this with the default limit.
//
// One decode of a GroupInfo and of a Welcome deliberately does NOT take the raised
// bound, and it is named here because the sentence above otherwise reads as though
// every such decode did. mls.ParseMLSMessage is the outermost entry point — every
// byte that arrives off the network or out of the store enters through it — and it
// calls Unmarshal, so this product's own group info and welcome do not fit through
// it at all. That is a decision rather than an oversight, and it is the group
// lifecycle plan's to inherit: a Welcome is decoded by a party who is not yet a
// member, with no group state to check it against and every length in it chosen by
// whoever sent it, so a raised limit at that entry point would be an acceptance
// rule handed to a stranger over the largest allocation the structure has. Whoever
// has to carry such a Welcome owes an entry point of their own wired to
// MaxRatchetTreeLength; they cannot raise this one.
// mls.TestParseMLSMessageCannotCarryThisProductsOwnGroupInfoOrWelcome is that
// ceiling measured rather than asserted, and mls's
// TestEverySyntaxEncoderInThisPackageUsesTheDefaultLimit is what pins the pair of
// calls that inherit it.
//
// The result joins the decoder's error with Done, mirroring MarshalLimit rather
// than returning early on the first of the two. The decoder's error and Done's
// answer are independent facts: a decoder can refuse a structure on semantic
// grounds with every one of its reads having succeeded, and in that case the
// bytes it never reached are still sitting there unconsumed. An early return would
// report the refusal and say nothing about the tail, which is the same asymmetry
// that would let a decoder under consume its input undetected the moment it also
// had something else to complain about. Joining costs a duplicated error in the
// case where a failed read latched and Done reports that same latched error back,
// which is the tradeoff the encode half already makes; errors.Is holds through a
// join either way.
func UnmarshalLimit(bs []byte, v Unmarshaler, maxVectorLength int) error {
	r := NewReaderLimit(bs, maxVectorLength)
	return errors.Join(v.UnmarshalMLS(r), r.Done())
}

// CheckRoundTrip asserts gate 4 property 2 (spec A section 4.4) against one
// input: if bs decodes, then encode(decode(bs)) must equal bs and
// decode(encode(decode(bs))) must re-encode identically. An input that does not
// decode carries no obligation and returns nil, because rejection is a legitimate
// outcome and the differential oracle compares accept and reject separately.
//
// Every fuzz target in connect/mls and connect/message calls this rather than
// writing the property again, so there is one definition of "round trips" in the
// system. Call it as CheckRoundTrip[KeyPackage, *KeyPackage](bs).
//
// Two things a caller has to know, because both are ways of holding this that
// look like coverage and are not.
//
// The nil on a rejected input is a vacuity trap for a fuzzer. Uniform random
// bytes almost never decode as a structure of any complexity, so a target that
// feeds only random bytes returns nil on nearly every call and passes without
// ever reaching an assertion. The measurement is in marshal_test.go and the
// conclusion is that a fuzz corpus must be seeded with valid encodings; the
// property is only worth what the corpus reaching it is worth.
//
// The second pass is a nondeterminism check and nothing else. Once the first
// re-encode is byte exact, decoding those same bytes again is the same call on
// the same input, so for a codec that is a pure function of its input it cannot
// disagree — ErrRoundTripNotStable is unreachable and the second pass is dead
// weight. It fires only when a codec carries hidden state between calls, which is
// the shape of a real defect worth a second pass to find: a map ranged during
// encode, a decoder consulting a package level registry that a later
// registration mutates, a buffer shared across decodes. That defect is invisible
// to the byte exactness check, which sees one encode.
//
// The bound is the default MaxVectorLength, which is correct for every structure
// but the ratchet tree; CheckRoundTripLimit is the form the tree paths call, and
// this is that with the default limit rather than a second copy of the sequence,
// so the two cannot drift apart in what they check.
func CheckRoundTrip[T any, PT interface {
	*T
	Codec
}](bs []byte) error {
	return CheckRoundTripLimit[T, PT](bs, MaxVectorLength)
}

// CheckRoundTripLimit is the same property under a caller chosen vector length
// limit; the ratchet tree paths pass MaxRatchetTreeLength and nothing else raises
// the bound, matching MarshalLimit and UnmarshalLimit above. Everything
// CheckRoundTrip documents holds here unchanged, including the nil for an input
// that does not decode and the second pass being a nondeterminism check rather
// than a second round trip.
//
// A structure that only decodes under the raised bound does not decode under the
// default one, so through the default entry point it takes the no-obligation path
// and returns nil — correct against the contract, and silent. A ratchet tree fuzz
// target built on the default bound would therefore be not merely mostly vacuous
// but entirely so, indistinguishable from one finding nothing wrong. That is why
// this exists here rather than being left for the call site to discover.
//
// The limit goes to both halves, and that is the point of threading it through
// rather than only raising the decoder. A tree that decoded under
// MaxRatchetTreeLength and then re-encoded through a Writer still capped at
// MaxVectorLength would fail byte exactness with ErrLengthExceedsMax, reporting a
// round trip violation for a reason that has nothing to do with canonicality — a
// false positive arriving in the one situation where somebody is chasing a real
// one.
func CheckRoundTripLimit[T any, PT interface {
	*T
	Codec
}](bs []byte, maxVectorLength int) error {
	first := PT(new(T))
	if err := UnmarshalLimit(bs, first, maxVectorLength); err != nil {
		return nil
	}
	reencoded, err := MarshalLimit(first, maxVectorLength)
	if err != nil {
		return errors.Join(ErrRoundTripNotByteExact, err)
	}
	if !bytes.Equal(reencoded, bs) {
		return ErrRoundTripNotByteExact
	}
	second := PT(new(T))
	if err := UnmarshalLimit(reencoded, second, maxVectorLength); err != nil {
		return errors.Join(ErrRoundTripNotStable, err)
	}
	again, err := MarshalLimit(second, maxVectorLength)
	if err != nil {
		return errors.Join(ErrRoundTripNotStable, err)
	}
	if !bytes.Equal(again, reencoded) {
		return ErrRoundTripNotStable
	}
	return nil
}
