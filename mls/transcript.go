// The RFC 9420 section 8.2 confirmed and interim transcript hashes.
//
// The transcript is what makes a fork visible. Every member's confirmation tag is a MAC
// over the confirmed transcript hash, and that hash is CHAINED: epoch n's value is taken
// over epoch n-1's interim value. So two members who applied different commits hold
// different confirmed hashes from the first divergent commit onward, their confirmation
// tags disagree, and each rejects the next commit the other sends. An implementation that
// computed each epoch from the right inputs and forgot to fold in the previous value
// passes every single epoch comparison and then produces two groups at epoch two, with no
// recovery path that does not involve somebody noticing.
//
// Two encodings meet here and only one of them is MLS's. The confirmation tag enters the
// interim hash as InterimTranscriptHashInput { MAC confirmation_tag; }, and a MAC is an
// opaque<V>: the varint prefix of section 2.1.2, which for a 32 octet tag is the single
// octet 0x20. syntax.WriteOpaqueLP is the record layer's fixed 32 bit prefix and belongs
// to connect/message; substituting it here produces a hash that agrees with itself on both
// sides of a group and with no other implementation in the world. The two are never
// interchangeable, which is why this file reaches the codec rather than writing a length
// byte of its own -- see TestNoMlsEncodingReachesTheRecordLayerLengthPrefix.
//
// Nothing framed crosses into this file. The confirmed hash is taken over the serialized
// ConfirmedTranscriptHashInput, which p6 builds and p7 hands over as bytes, so the
// transcript arithmetic can be read and audited without the framing types. That boundary
// is the interface registry's and not a convenience: these four functions are this plan's,
// and p7 bridges to them.
package mls

import (
	"fmt"

	"github.com/urnetwork/connect/mls/syntax"
)

// TranscriptHashes is the pair a group carries across epochs. Confirmed is what the
// confirmation tag is computed over and what the GroupContext carries; Interim is what the
// NEXT epoch's confirmed hash is chained from. They are two values with two roles and
// neither can stand in for the other -- swapping the two assignments is a one line edit
// that leaves both looking like well formed hashes.
type TranscriptHashes struct {
	Confirmed []byte
	Interim   []byte
}

// InitialTranscriptHashes is the group-creation base case: both hashes are the
// zero-length octet string, and NOT the hash of nothing. The creator's own epoch-0
// confirmation tag is folded in by SetFromGroupInfo or by the first Update, depending on
// which side of the Welcome the member is.
func InitialTranscriptHashes() *TranscriptHashes {
	return &TranscriptHashes{
		Confirmed: []byte{},
		Interim:   []byte{},
	}
}

// Clone returns a deep copy so a retained past epoch cannot alias the live one. A group
// keeps the transcript of epochs it may still have to validate against, and a shallow copy
// would let the live epoch's Update rewrite what a retained one holds.
//
// cloneBytes rather than append to a nil slice, for the reason group_context.go gives:
// append would collapse the empty non nil slices InitialTranscriptHashes hands out into
// nil, and a clone that changed which of the two a caller holds changed the value.
func (self *TranscriptHashes) Clone() *TranscriptHashes {
	return &TranscriptHashes{
		Confirmed: cloneBytes(self.Confirmed),
		Interim:   cloneBytes(self.Interim),
	}
}

// ConfirmedTranscriptHash is
// Hash(interim_transcript_hash_[n-1] || ConfirmedTranscriptHashInput_[n]).
//
// The two operands are concatenated with nothing between them. A separator, a length
// prefix or a label here would be self consistent -- every member of a group running it
// would agree -- and would disagree with every other implementation, which is the same
// outcome as a fork except that it arrives at the first cross-implementation join.
//
// The caller supplies the serialized input; this package never sees framing types.
func ConfirmedTranscriptHash(crypto CryptoProvider, interimBefore []byte, confirmedTranscriptHashInput []byte) []byte {
	buffer := make([]byte, 0, len(interimBefore)+len(confirmedTranscriptHashInput))
	buffer = append(buffer, interimBefore...)
	buffer = append(buffer, confirmedTranscriptHashInput...)
	return crypto.Hash(buffer)
}

// InterimTranscriptHash is
// Hash(confirmed_transcript_hash_[n] || InterimTranscriptHashInput_[n]),
// where InterimTranscriptHashInput is the single field MAC confirmation_tag, and a MAC is
// an opaque<V>. The length prefix is not optional and it is MLS's varint, not the record
// layer's fixed width one: the codec is asked for it rather than a byte being written
// here, so the one place that decides how an MLS vector is spelled stays syntax.
func InterimTranscriptHash(crypto CryptoProvider, confirmedAfter []byte, confirmationTag []byte) ([]byte, error) {
	if crypto == nil {
		return nil, fmt.Errorf("%w: the interim hash is the provider's", ErrNilCryptoProvider)
	}
	w := syntax.NewWriter()
	w.WriteOpaque(confirmationTag)
	input, err := w.Bytes()
	if err != nil {
		return nil, err
	}
	buffer := make([]byte, 0, len(confirmedAfter)+len(input))
	buffer = append(buffer, confirmedAfter...)
	buffer = append(buffer, input...)
	return crypto.Hash(buffer), nil
}

// Update advances both hashes for one commit.
//
// The interim hash the confirmed one is chained from is the receiver's own, read before it
// is overwritten. That single read is the whole of the chaining: replace self.Interim with
// nil here and every one-epoch test in this package still passes, while the group forks at
// epoch two.
//
// Neither field is written until both values exist, so a writer failure leaves the epoch
// where it was rather than half advanced.
func (self *TranscriptHashes) Update(crypto CryptoProvider, confirmedTranscriptHashInput []byte, confirmationTag []byte) error {
	if crypto == nil {
		return fmt.Errorf("%w: both hashes it advances are the provider's", ErrNilCryptoProvider)
	}
	confirmed := ConfirmedTranscriptHash(crypto, self.Interim, confirmedTranscriptHashInput)
	interim, err := InterimTranscriptHash(crypto, confirmed, confirmationTag)
	if err != nil {
		return err
	}
	self.Confirmed = confirmed
	self.Interim = interim
	return nil
}

// SetFromGroupInfo seeds a joiner from the confirmed transcript hash the Welcome's
// GroupInfo carries in its GroupContext and the confirmation tag beside it. Without this a
// new member holds no interim hash and cannot compute the confirmed hash of the next
// commit -- which is to say it would fork from the group it just joined.
//
// Both lengths are refused rather than accepted, because a GroupInfo is somebody else's
// bytes. A short confirmed hash seeds a member with an interim value no peer agrees with,
// and that surfaces one commit later as a confirmation tag mismatch naming nothing.
func (self *TranscriptHashes) SetFromGroupInfo(crypto CryptoProvider, confirmedTranscriptHash []byte, confirmationTag []byte) error {
	if crypto == nil {
		return fmt.Errorf("%w: the interim hash it seeds is the provider's", ErrNilCryptoProvider)
	}
	nh := crypto.HashSize()
	if len(confirmedTranscriptHash) != nh {
		return fmt.Errorf("%w: confirmed transcript hash is %d bytes, want %d",
			ErrTranscriptHashLength, len(confirmedTranscriptHash), nh)
	}
	if len(confirmationTag) != nh {
		return fmt.Errorf("%w: confirmation tag is %d bytes, want %d",
			ErrTranscriptHashLength, len(confirmationTag), nh)
	}
	interim, err := InterimTranscriptHash(crypto, confirmedTranscriptHash, confirmationTag)
	if err != nil {
		return err
	}
	// a copy, so the joiner's transcript does not alias the GroupInfo buffer its caller
	// still owns and may decode further fields out of
	self.Confirmed = cloneBytes(confirmedTranscriptHash)
	self.Interim = interim
	return nil
}
