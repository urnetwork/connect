// The TLS presentation language of RFC 8446 section 3 as MLS uses it: fixed width
// integers, opaque V with the RFC 9420 section 2.1.2 variable length prefix,
// optional T, and byte length prefixed vectors. The master protocol design's LP(x)
// 32 bit big endian prefix lives here too, because connect/message encodes records
// through this same package and one length prefix implementation means one place
// for a length prefix bug to be.
//
// Four rules, each with a fuzz property behind it (spec A section 5.8):
//
//  1. Canonical encoding only. The variable length prefix has exactly one valid
//     encoding per value; a non minimal prefix is a decode error.
//  2. No allocation before validation. A declared length is checked against the
//     configured maximum and against the bytes that remain before any make.
//  3. Full consumption. A top level decode fails if bytes remain.
//  4. Round trip byte exact. encode(decode(x)) equals x for every accepted x.
//
// Rule 4 is the load bearing one: MLS signs over serialized forms, so a decoder
// that accepts two encodings of one object is a signature bypass primitive.
//
// Writer carries a sticky error, so the leaf writes — WriteUint16, WriteOpaque and
// the rest — return nothing and one check at Bytes covers a whole encoder. Three
// things do return an error: MarshalMLS itself, and the two higher order callbacks
// WriteOptional and WriteVector. An MLS encoder has semantic refusals that are not
// buffer errors — a credential type outside the v1 profile, a content arm that
// disagrees with its discriminant — and a dropped encoder refusal produces wrong
// signed bytes rather than a failure, so those need a channel the sticky error does
// not give them. Marshal joins the two, so the semantic error and the buffer error
// both surface. Reader returns an error per read, because decoding is a branch per
// field and every one of them matters, and Unmarshal joins the decoder's error with
// the full consumption check for the same reason its counterpart joins: a decoder
// that refuses a structure on semantic grounds may also have left a tail behind it,
// and rule 3 is not something a caller should hear about only when nothing else went
// wrong.
package syntax
