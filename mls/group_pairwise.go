// The preimage (*Group).PairwiseExport derives over, in a declaration of its own.
//
// IT IS NOT AN MLS STRUCTURE AND IT IS NOT ON ANY WIRE. Nothing outside this profile ever parses
// these octets: they are a KDF context, read once by HKDF-Expand and by nothing else, so there is
// no other implementation for this encoding to agree or disagree with. That is what makes it the
// one place in package mls where the RECORD LAYER's length prefix is the right one -- see the
// paragraph below -- and it is why the encoder lives here rather than beside the MLS structures.
//
// WHY THE RECORD LAYER'S LP AND NOT MLS's VARINT OPAQUE. LP(x) is a FIXED 32 bit big endian length
// followed by the bytes; MLS's opaque<V> is a varint whose own width depends on the value it is
// encoding. A fixed width prefix keeps every field boundary of a preimage independent of the
// lengths inside it, which is the property connect/message builds every AAD and every write_auth
// preimage on, and it is the one ledger item 228's ruling names for this context. A varint here
// would be a preimage whose field boundaries move with its contents.
//
// AND THE LP IS REACHED THROUGH mls/syntax RATHER THAN connect/message, which is not a preference:
// the import direction gate in connect's own package forbids package mls importing connect/message
// at all, so the encoder here is the SAME encoder one package down rather than a second hand
// written spelling of it. syntax.Writer.WriteOpaqueLP and connect/message's LP are one
// implementation, and mls/syntax's own suite is what holds it to a fixed 32 bit prefix.
//
// TestNoMlsEncodingReachesTheRecordLayerLengthPrefix is the gate that otherwise forbids this, and
// encodePairwiseContext is the single declaration it sanctions. The exemption is named there, by
// declaration rather than by file, and that gate fails if this declaration ever stops making the
// call -- so the exemption cannot outlive the thing it was written for.
package mls

import "github.com/urnetwork/connect/mls/syntax"

// marshalPairwiseContext is the BOUNDED door onto the encoder below, and the bound is the class
// marshalPskLabel and marshalBoundedComposition belong to rather than a precaution.
//
// PairwiseExport hands these bytes to ExpandWithLabel as the CONTEXT of a KDFLabel, which is one
// opaque<V>, and ExpandWithLabel is a CryptoProvider method whose signature cannot report a
// refusal -- an over long context takes the process down inside crypto_labels.go's mlsLabelBytes.
// The composition is built from a group id and an epoch authenticator this member holds rather
// than from anything a peer chose, so the sum is small at every call this profile makes; the bound
// is here anyway, because this is the outermost declaration on the path whose signature can say no
// and because "no peer controls it today" is a sentence about today.
func marshalPairwiseContext(groupId []byte, epoch uint64, lo uint32, hi uint32,
	pkLo []byte, pkHi []byte, epochAuthenticator []byte) ([]byte, error) {

	encoded, err := encodePairwiseContext(groupId, epoch, lo, hi, pkLo, pkHi, epochAuthenticator)
	if err != nil {
		return nil, err
	}
	if err := checkLabelledFieldLength("pairwise export context", "", len(encoded)); err != nil {
		return nil, err
	}
	return encoded, nil
}

// encodePairwiseContext writes ledger item 228's ruled context:
//
//	LP(group_id) || u64(epoch) || u32(lo) || u32(hi) || LP(pk_lo) || LP(pk_hi) || LP(epoch_authenticator)
//
// Every field is fixed width or LP prefixed, so the preimage is unambiguous: no two distinct field
// assignments encode to one octet string, and there is no tail for a reader to be confused by.
//
// THE SIZE IS 128 + len(group_id) at a 32 octet KEM and KDF -- (4 + len(group_id)) + 8 + 4 + 4 +
// (4 + 32) + (4 + 32) + (4 + 32) -- which is the ruling's 160 at URmessage's 32 octet group id.
// The total is written as that arithmetic rather than as the constant because nothing in package
// mls fixes a group id's width: RFC 9420 makes it an opaque vector and this package's own fixtures
// use several, so a constant here would be right for one caller and silently wrong for the next.
//
// THE PAIR ARRIVES SORTED AND THE POINTS ARRIVE IN THE POSITIONS THEIR INDICES NAME. That is the
// caller's half of the construction and it is stated here because this declaration cannot check
// it: writing lo and hi in the order given is what makes the two members of a pair derive one
// value, and carrying each point beside its own index is what stops a point being moved between
// the two positions.
func encodePairwiseContext(groupId []byte, epoch uint64, lo uint32, hi uint32,
	pkLo []byte, pkHi []byte, epochAuthenticator []byte) ([]byte, error) {

	writer := syntax.NewWriter()
	writer.WriteOpaqueLP(groupId)
	writer.WriteUint64(epoch)
	writer.WriteUint32(lo)
	writer.WriteUint32(hi)
	writer.WriteOpaqueLP(pkLo)
	writer.WriteOpaqueLP(pkHi)
	writer.WriteOpaqueLP(epochAuthenticator)
	return writer.Bytes()
}
