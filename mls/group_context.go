// The RFC 9420 section 8.1 GroupContext: the epoch binding that the key schedule
// expands over and that framing signs under.
//
// This structure is the reason the codec is byte exact rather than merely
// symmetric. GroupContext is hashed into the confirmed transcript and mixed into
// every derivation of the epoch, so two implementations that serialize it
// differently by one byte derive different secrets and reject each other's
// commits — and nothing about that failure looks like a codec bug from either
// side. Round trip symmetry cannot see it either: an encoder and a decoder that
// both swap two adjacent uint16 fields round trip perfectly and still disagree
// with everybody else. What sees it is the golden vector in the test file, taken
// from another implementation's output.
package mls

import (
	"fmt"

	"github.com/urnetwork/connect/mls/syntax"
)

// GroupContext binds a set of epoch secrets to one group, epoch, tree and
// transcript. Every field is covered by the joiner and epoch derivations, so two
// members who disagree on any one of them derive different secrets and stop being
// able to talk, which is the intended failure mode rather than a silent one.
type GroupContext struct {
	Version                 ProtocolVersion
	CipherSuite             CipherSuite
	GroupId                 []byte
	Epoch                   uint64
	TreeHash                []byte
	ConfirmedTranscriptHash []byte
	Extensions              []Extension
}

// MarshalMLS encodes the context in the RFC 9420 section 8.1 field order, inline,
// into a writer the caller owns and with no framing of its own — GroupInfo and
// every p6 preimage carry a group context inline, so a length prefix added here
// would be invisible to a caller reading only its own bytes and fatal to every
// signature over them.
//
// The two opaque fields and the group id take WriteOpaque, the MLS varint prefix.
// WriteOpaqueLP, the record layer's fixed 32 bit prefix, exists in this tree and is
// never interchangeable with it: both encode the 32 byte tree hash, and only one of
// them is what a peer will hash.
//
// The leaf writes return nothing and are no ops after the first failure (C2); the
// buffer error is collected by syntax.Marshal at Bytes. The error return exists for
// semantic refusals, and WriteExtensions is the only call here that can raise one.
func (self *GroupContext) MarshalMLS(w *syntax.Writer) error {
	w.WriteUint16(uint16(self.Version))
	w.WriteUint16(uint16(self.CipherSuite))
	w.WriteOpaque(self.GroupId)
	w.WriteUint64(self.Epoch)
	w.WriteOpaque(self.TreeHash)
	w.WriteOpaque(self.ConfirmedTranscriptHash)
	return WriteExtensions(w, self.Extensions)
}

// UnmarshalMLS decodes the context from a reader, consuming exactly its own fields
// and no more: GroupInfo carries a GroupContext inline, so eating the tail here
// would eat the confirmation tag. Full consumption of a standalone encoding is
// enforced by syntax.Unmarshal, whose ErrTrailingBytes is what
// ErrGroupContextTrailingBytes wraps.
//
// Nothing is assigned until every field has been read, so a caller handed a partly
// consumed input keeps the zero value it passed in rather than a struct holding
// three real fields and four zero ones that would encode as a different group.
func (self *GroupContext) UnmarshalMLS(r *syntax.Reader) error {
	version, err := r.ReadUint16()
	if err != nil {
		return fmt.Errorf("mls: group context version: %w", err)
	}
	suite, err := r.ReadUint16()
	if err != nil {
		return fmt.Errorf("mls: group context cipher suite: %w", err)
	}
	groupId, err := r.ReadOpaque()
	if err != nil {
		return fmt.Errorf("mls: group context group id: %w", err)
	}
	epoch, err := r.ReadUint64()
	if err != nil {
		return fmt.Errorf("mls: group context epoch: %w", err)
	}
	treeHash, err := r.ReadOpaque()
	if err != nil {
		return fmt.Errorf("mls: group context tree hash: %w", err)
	}
	confirmedTranscriptHash, err := r.ReadOpaque()
	if err != nil {
		return fmt.Errorf("mls: group context confirmed transcript hash: %w", err)
	}
	extensions, err := ReadExtensions(r)
	if err != nil {
		return fmt.Errorf("mls: group context extensions: %w", err)
	}
	self.Version = ProtocolVersion(version)
	self.CipherSuite = CipherSuite(suite)
	self.GroupId = groupId
	self.Epoch = epoch
	self.TreeHash = treeHash
	self.ConfirmedTranscriptHash = confirmedTranscriptHash
	self.Extensions = extensions
	return nil
}

// the C1 pin: drift between this type and the one codec convention fails at build.
var _ syntax.Codec = (*GroupContext)(nil)

// Clone returns a deep copy, so a retained past epoch cannot alias the live one.
// The extension bodies are copied too: an extensions slice shared between two
// epochs would let a commit that rewrites group context extensions reach back into
// the epoch a member is still using to decrypt out of order traffic.
func (self *GroupContext) Clone() *GroupContext {
	clone := &GroupContext{
		Version:                 self.Version,
		CipherSuite:             self.CipherSuite,
		GroupId:                 cloneBytes(self.GroupId),
		Epoch:                   self.Epoch,
		TreeHash:                cloneBytes(self.TreeHash),
		ConfirmedTranscriptHash: cloneBytes(self.ConfirmedTranscriptHash),
	}
	if self.Extensions != nil {
		clone.Extensions = make([]Extension, 0, len(self.Extensions))
		for _, extension := range self.Extensions {
			clone.Extensions = append(clone.Extensions, Extension{
				ExtensionType: extension.ExtensionType,
				ExtensionData: cloneBytes(extension.ExtensionData),
			})
		}
	}
	return clone
}

// cloneBytes copies a field, keeping nil distinct from empty. append to a nil slice
// would collapse an empty non nil slice to nil, and while the two encode
// identically, a clone that changed which one a caller holds is a clone that
// changed the value.
func cloneBytes(bs []byte) []byte {
	if bs == nil {
		return nil
	}
	out := make([]byte, len(bs))
	copy(out, bs)
	return out
}
