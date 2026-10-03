// The RFC 9420 section 12.1 proposal wire types and their codecs.
//
// Codec only. The v1 profile gate that refuses psk, reinit and external_init is
// (*Profile).CheckProposalType, called at the parse boundary by the group lifecycle, and this
// file must round trip every registered type whatever that profile says: the `messages` vector
// family carries all seven, and a codec that refused three of them would fail the family for a
// reason that is a policy rather than an encoding.
//
// ProposalType and its constants are NOT declared here. They are the registry enum file's
// (extension.go), because Capabilities.Proposals names them one wave earlier, and package mls is
// one package -- a second declaration of one wire enum disagrees by a NUMBER rather than by a
// type error, which is the drift nothing in this package could see.
//
// UnknownType and UnknownBody together are the forge's malformed arm. On decode of an
// unregistered type both ProposalType and UnknownType carry the wire value and UnknownBody holds
// the remaining bytes, so the object re-encodes verbatim and GREASE is parsed and ignored, never
// generated. On encode a non-zero UnknownType overrides the discriminant that goes on the wire,
// which is how a registered body is emitted under an unregistered type without a second encoder
// existing anywhere in the package -- and one encoder is the point, because the second one is
// what would sign bytes this one never wrote.
package mls

import (
	"fmt"

	"github.com/urnetwork/connect/mls/syntax"
)

// Add carries the key package of the member being added, by value.
type Add struct {
	KeyPackage KeyPackage
}

// Update carries the sender's replacement leaf node, by value.
type Update struct {
	LeafNode LeafNode
}

// Remove names a leaf by index. The index is the removed member's, not the sender's.
type Remove struct {
	Removed LeafIndex
}

// PreSharedKey injects an external or resumption secret into the next epoch's key schedule.
type PreSharedKey struct {
	Psk PreSharedKeyId
}

// ReInit restarts a group under a new group id, version, ciphersuite and extension set.
type ReInit struct {
	GroupId     []byte
	Version     ProtocolVersion
	CipherSuite CipherSuite
	Extensions  []Extension
}

// ExternalInit carries the KEM output an external joiner commits with.
type ExternalInit struct {
	KemOutput []byte
}

// GroupContextExtensions replaces the group context's extension list wholesale.
type GroupContextExtensions struct {
	Extensions []Extension
}

// Proposal is a variant structure over ProposalType: exactly the arm the type names is
// populated.
//
// An unrecognised type keeps its body in UnknownBody and its discriminant in UnknownType and
// re-encodes verbatim, which is what makes GREASE round trip. Setting UnknownType alongside a
// populated arm is the malformed-arm seam the validation plan's forge needs.
type Proposal struct {
	ProposalType           ProposalType
	Add                    *Add
	Update                 *Update
	Remove                 *Remove
	PreSharedKey           *PreSharedKey
	ReInit                 *ReInit
	ExternalInit           *ExternalInit
	GroupContextExtensions *GroupContextExtensions
	UnknownType            ProposalType
	UnknownBody            []byte
}

// checkArm reports whether the arm ProposalType names is the one that is populated.
//
// It is separate from MarshalMLS and it runs FIRST, before a single octet is written, which is
// framing.go's discipline and is load bearing for framing.go's reason. A Proposal is written
// into a FramedContent that is then SIGNED, and syntax.Marshal joins a semantic refusal with
// whatever the Writer already holds -- so an encoder that wrote its discriminant and then
// refused would leave a caller that ignored the return value holding two octets of a proposal
// that does not exist. The plan's own sketch writes the discriminant first; this does not, and
// the observable behaviour is the same refusal from one octet earlier.
func (self *Proposal) checkArm() error {
	// exactly one, counted before the discriminant is read. The plan's own sketch checks only
	// that the NAMED arm is present, and that leaves two distinct Proposal values -- one with a
	// second arm hanging off it, one without -- encoding to the same octets and therefore
	// carrying the same ProposalRef. A reference is an IDENTITY: two values sharing one is the
	// defect the reference exists to prevent, and the arm that was silently dropped is a change
	// the proposer believed it had published.
	//
	// UnknownBody is counted with the seven arms rather than beside them, because for an
	// unregistered type it IS the arm. What is NOT counted is UnknownType: that overrides the
	// discriminant rather than supplying a body, which is the forge's seam and the one shape
	// this check must leave open.
	//
	// The list below is held to the type by TestEveryArmOfAProposalIsCountedByItsArmCheck, which
	// derives the arms off reflect over this struct: an eighth arm added here with no line in
	// this count fails there rather than being dropped on the floor.
	populated := 0
	for _, set := range []bool{
		self.Add != nil,
		self.Update != nil,
		self.Remove != nil,
		self.PreSharedKey != nil,
		self.ReInit != nil,
		self.ExternalInit != nil,
		self.GroupContextExtensions != nil,
		self.UnknownBody != nil,
	} {
		if set {
			populated += 1
		}
	}
	if populated != 1 {
		return fmt.Errorf("%w: %04x carries %d populated arms", ErrContentArmMismatch, self.ProposalType, populated)
	}
	switch self.ProposalType {
	case ProposalTypeAdd:
		if self.Add == nil {
			return ErrContentArmMismatch
		}
	case ProposalTypeUpdate:
		if self.Update == nil {
			return ErrContentArmMismatch
		}
	case ProposalTypeRemove:
		if self.Remove == nil {
			return ErrContentArmMismatch
		}
	case ProposalTypePreSharedKey:
		if self.PreSharedKey == nil {
			return ErrContentArmMismatch
		}
	case ProposalTypeReInit:
		if self.ReInit == nil {
			return ErrContentArmMismatch
		}
	case ProposalTypeExternalInit:
		if self.ExternalInit == nil {
			return ErrContentArmMismatch
		}
	case ProposalTypeGroupContextExtensions:
		if self.GroupContextExtensions == nil {
			return ErrContentArmMismatch
		}
	default:
		// an unregistered type has no arm; what stands in for one is the verbatim body, and
		// a nil body is a proposal with neither an arm nor bytes to re-emit.
		if self.UnknownBody == nil {
			return fmt.Errorf("%w: %04x", ErrContentArmMismatch, self.ProposalType)
		}
	}
	return nil
}

func (self *Proposal) MarshalMLS(w *syntax.Writer) error {
	if err := self.checkArm(); err != nil {
		return err
	}
	// the arm is selected by ProposalType; the discriminant that goes ON THE WIRE is
	// UnknownType when it is set. The two differ only for the forge, and keeping them
	// separate is what lets a well formed body be emitted under a GREASE or reserved code
	// point with no second encoder in the package.
	proposalType := self.ProposalType
	if self.UnknownType != ProposalTypeReserved {
		proposalType = self.UnknownType
	}
	w.WriteUint16(uint16(proposalType))
	switch self.ProposalType {
	case ProposalTypeAdd:
		return self.Add.KeyPackage.MarshalMLS(w)
	case ProposalTypeUpdate:
		return self.Update.LeafNode.MarshalMLS(w)
	case ProposalTypeRemove:
		w.WriteUint32(uint32(self.Remove.Removed))
		return nil
	case ProposalTypePreSharedKey:
		return self.PreSharedKey.Psk.MarshalMLS(w)
	case ProposalTypeReInit:
		w.WriteOpaque(self.ReInit.GroupId)
		w.WriteUint16(uint16(self.ReInit.Version))
		w.WriteUint16(uint16(self.ReInit.CipherSuite))
		return WriteExtensions(w, self.ReInit.Extensions)
	case ProposalTypeExternalInit:
		w.WriteOpaque(self.ExternalInit.KemOutput)
		return nil
	case ProposalTypeGroupContextExtensions:
		return WriteExtensions(w, self.GroupContextExtensions.Extensions)
	}
	w.WriteRaw(self.UnknownBody)
	return nil
}

// UnmarshalMLS reads the discriminant, resets the receiver, and reads the arm that discriminant
// selects.
//
// The reset is what the other decoders in this package get from staging into a local: no field
// of a previous value survives into a decode of different bytes. It is done here rather than at
// the end because the seven arms write seven different fields, and a staged copy would be a
// second Proposal built solely to be copied over the first.
func (self *Proposal) UnmarshalMLS(r *syntax.Reader) error {
	proposalType, err := r.ReadUint16()
	if err != nil {
		return err
	}
	*self = Proposal{ProposalType: ProposalType(proposalType)}
	switch self.ProposalType {
	case ProposalTypeAdd:
		add := &Add{}
		if err := add.KeyPackage.UnmarshalMLS(r); err != nil {
			return err
		}
		self.Add = add
		return nil
	case ProposalTypeUpdate:
		update := &Update{}
		if err := update.LeafNode.UnmarshalMLS(r); err != nil {
			return err
		}
		self.Update = update
		return nil
	case ProposalTypeRemove:
		removed, err := r.ReadUint32()
		if err != nil {
			return err
		}
		self.Remove = &Remove{Removed: LeafIndex(removed)}
		return nil
	case ProposalTypePreSharedKey:
		psk := &PreSharedKey{}
		if err := psk.Psk.UnmarshalMLS(r); err != nil {
			return err
		}
		self.PreSharedKey = psk
		return nil
	case ProposalTypeReInit:
		groupId, err := r.ReadOpaque()
		if err != nil {
			return err
		}
		version, err := r.ReadUint16()
		if err != nil {
			return err
		}
		suite, err := r.ReadUint16()
		if err != nil {
			return err
		}
		extensions, err := ReadExtensions(r)
		if err != nil {
			return err
		}
		self.ReInit = &ReInit{
			GroupId:     groupId,
			Version:     ProtocolVersion(version),
			CipherSuite: CipherSuite(suite),
			Extensions:  extensions,
		}
		return nil
	case ProposalTypeExternalInit:
		kemOutput, err := r.ReadOpaque()
		if err != nil {
			return err
		}
		self.ExternalInit = &ExternalInit{KemOutput: kemOutput}
		return nil
	case ProposalTypeGroupContextExtensions:
		extensions, err := ReadExtensions(r)
		if err != nil {
			return err
		}
		self.GroupContextExtensions = &GroupContextExtensions{Extensions: extensions}
		return nil
	}
	// an unregistered type: keep the discriminant and the remaining bytes so the object
	// re-encodes verbatim. ReadRaw(Remaining()) rather than a Rest() accessor, because
	// consuming the tail is a decision and has to be visible as one.
	self.UnknownType = self.ProposalType
	body, err := r.ReadRaw(r.Remaining())
	if err != nil {
		return err
	}
	self.UnknownBody = body
	return nil
}

var _ syntax.Codec = (*Proposal)(nil)

// ProposalOrRefType is eight bits, and the registry it comes from is not extensible: RFC 9420
// section 12.4 registers exactly the two values below and reserves zero.
type ProposalOrRefType uint8

const (
	ProposalOrRefTypeReserved  ProposalOrRefType = 0
	ProposalOrRefTypeProposal  ProposalOrRefType = 1
	ProposalOrRefTypeReference ProposalOrRefType = 2
)

// ProposalRef is a HashReference over the serialized AuthenticatedContent that framed a
// proposal -- not over the Proposal itself. (*AuthenticatedContent).ProposalRef in
// framing_preimage.go is what produces one, and the reason for the wider input is there.
type ProposalRef []byte

// ProposalOrRef is how a Commit names a proposal: inline, or by reference to one already
// published in this epoch.
type ProposalOrRef struct {
	Type      ProposalOrRefType
	Proposal  *Proposal
	Reference ProposalRef
}

// checkArm reports whether the arm Type names is populated and is the ONLY one populated.
//
// It is Proposal.checkArm one type up, and it is here for both of that method's reasons.
//
// The per-case half is what stands between a ProposalOrRef whose discriminant names an inline
// proposal and self.Proposal.MarshalMLS(w) on a nil pointer -- the same dereference the seven
// per-case checks one type down exist to prevent, on the sibling type twelve lines further
// along this file.
//
// The COUNT is the other half and it is the one a per-case check alone does not give. Without
// it a value carrying BOTH an inline proposal and a reference encodes to the arm its
// discriminant names and drops the other, so two distinct ProposalOrRef values write the same
// octets and the arm the caller set is nowhere in them. These octets go inside a Commit inside
// a FramedContent that is SIGNED: the signer would sign a commit that does not say what it was
// handed, and the peer would apply the one arm that survived.
//
// The discriminant is judged FIRST, before the count. A type outside the registry names no arm
// at all, so a value under one is refused for its TYPE rather than for whichever arm it happens
// to carry. That is the distinction
// TestProposalOrRefRefusesEveryCodePointThatNamesNoArmOnBothHalves reads, sweeping the
// unregistered octets with BOTH arms populated so a lost discriminant check cannot pass for a
// refusal of the body.
//
// The count is two lines of source and the class it stands for is derived off reflect over this
// struct by TestEveryProposalOrRefTypeRefusesEveryArmThatIsNotItsOwn, so a third arm added here
// with no line beside it fails there rather than being dropped on the floor.
func (self *ProposalOrRef) checkArm() error {
	switch self.Type {
	case ProposalOrRefTypeProposal, ProposalOrRefTypeReference:
	default:
		return fmt.Errorf("%w: %d", ErrUnknownProposalOrRefType, self.Type)
	}
	// counted on nil rather than on length, which is Proposal.checkArm's UnknownBody
	// convention and is here for its reason: an empty but PRESENT reference is a caller who
	// believes it set one, and counting that as absent would let it share an encoding with a
	// value carrying no reference at all. The zero LENGTH is refused below, separately, and
	// says something different -- that the reference names nothing.
	populated := 0
	for _, set := range []bool{
		self.Proposal != nil,
		self.Reference != nil,
	} {
		if set {
			populated += 1
		}
	}
	if populated != 1 {
		return fmt.Errorf("%w: ProposalOrRef %d carries %d populated arms", ErrContentArmMismatch, self.Type, populated)
	}
	switch self.Type {
	case ProposalOrRefTypeProposal:
		if self.Proposal == nil {
			return fmt.Errorf("%w: ProposalOrRef %d names an inline proposal and the arm it carries is not one", ErrContentArmMismatch, self.Type)
		}
	case ProposalOrRefTypeReference:
		// there is deliberately no nil check beside the length check below: one line does both
		// jobs on this arm, because a nil Reference has zero length. The value whose
		// discriminant names a reference and whose populated arm is the inline proposal and
		// the value that set an empty reference are refused by the same comparison. The
		// Proposal arm above needs its own nil check because a pointer has no length to fall
		// through to -- and a guard that cannot fire is a guard no test can hold, which is how
		// this one was found.
		//
		// An empty reference is refused rather than written. A zero length opaque<V> is wire
		// legal and is the encoding of "no reference", so a commit carrying one names a
		// proposal that cannot be looked up -- and it names it identically to every other
		// commit that made the same mistake, which is worse than naming nothing.
		if len(self.Reference) == 0 {
			return fmt.Errorf("%w: a proposal reference of no octets names nothing", ErrContentArmMismatch)
		}
	}
	return nil
}

// MarshalMLS refuses before it writes, for Proposal.checkArm's reason: a ProposalOrRef sits
// inside a Commit inside a FramedContent that is signed, and syntax.Marshal joins a semantic
// refusal with whatever the Writer already holds, so an encoder that wrote its discriminant and
// then refused would leave a caller that ignored the return value holding one octet of a
// ProposalOrRef that does not exist.
func (self *ProposalOrRef) MarshalMLS(w *syntax.Writer) error {
	if err := self.checkArm(); err != nil {
		return err
	}
	w.WriteUint8(uint8(self.Type))
	if self.Type == ProposalOrRefTypeProposal {
		return self.Proposal.MarshalMLS(w)
	}
	w.WriteOpaque(self.Reference)
	return nil
}

// UnmarshalMLS reads the discriminant, reads the arm it selects, and only then writes the
// receiver -- Sender's and Credential's staging, for their reason.
func (self *ProposalOrRef) UnmarshalMLS(r *syntax.Reader) error {
	proposalOrRefType, err := r.ReadUint8()
	if err != nil {
		return err
	}
	decoded := ProposalOrRef{Type: ProposalOrRefType(proposalOrRefType)}
	switch decoded.Type {
	case ProposalOrRefTypeProposal:
		decoded.Proposal = &Proposal{}
		if err := decoded.Proposal.UnmarshalMLS(r); err != nil {
			return err
		}
	case ProposalOrRefTypeReference:
		reference, err := r.ReadOpaque()
		if err != nil {
			return err
		}
		// an empty reference is wire legal and is the encoding of "no reference". The two
		// halves of this codec agree about it, on FramedContentAuthData's terms and for its
		// reason: what the encoder refuses to write, the decoder refuses to read. Accepting
		// one would put a commit past the encoder's own check carrying a name that resolves
		// to no proposal and collides with every other commit that made the same mistake.
		if len(reference) == 0 {
			return fmt.Errorf("%w: a proposal reference of no octets names nothing", ErrContentArmMismatch)
		}
		decoded.Reference = ProposalRef(reference)
	default:
		return fmt.Errorf("%w: %d", ErrUnknownProposalOrRefType, decoded.Type)
	}
	*self = decoded
	return nil
}

var _ syntax.Codec = (*ProposalOrRef)(nil)
