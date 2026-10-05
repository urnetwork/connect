// RFC 9420 section 6 message framing wire types and their codecs.
//
// No crypto lives here. The signed and MACed byte strings are framing_preimage.go and the
// sealing operations are framing_protect.go, so a preimage change stays a one file diff an
// auditor can read. ProtocolVersion and ProtocolVersionMls10 are extension.go's, because the
// interface registry gives every IANA registry enum to the plan that first needed one and
// package mls is one package.
//
// ContentType arrived here by a different route and the route is worth stating, because it is
// the second half of an arrangement the key schedule's own files describe. The secret tree
// implements the MessageKeySource interface this plan declares, that interface is keyed on
// ContentType, and the secret tree landed first -- so ContentType was declared in
// content_type.go, on this plan's behalf, at the signature the interface registry gives it,
// with the standing agreement that this plan's own landing would DELETE that file rather than
// add a second declaration beside it. This is that landing and that is what it did. A second
// declaration of one wire enum disagrees by a NUMBER rather than by a type error, which is the
// one kind of drift nothing in this package could have seen.
//
// None of the three registries here declares its reserved zero as a constant, and that is a
// decision rather than an omission. Every enumeration gate in this package derives a
// registry's members as "the declared constants of that type", so a constant standing at a
// code point the RFC reserves is counted as registered by all of them -- and for ContentType
// specifically, the reserved zero being absent is what makes an unparsed header a refusal
// rather than generation 0 of a real ratchet. The refusal paths therefore name the offending
// value numerically, which is what they have off the wire anyway.
package mls

import (
	"errors"
	"fmt"

	"github.com/urnetwork/connect/mls/syntax"
)

// WireFormat selects which of the five MLSMessage arms a message carries. It is a code point
// from the RFC 9420 section 17 IANA MLS Wire Formats registry and is sixteen bits wide, which
// is wider than the five registered values need: the registry is open, and a decoder that read
// it at one octet would misplace every field after it.
type WireFormat uint16

const (
	WireFormatPublicMessage  WireFormat = 0x0001
	WireFormatPrivateMessage WireFormat = 0x0002
	WireFormatWelcome        WireFormat = 0x0003
	WireFormatGroupInfo      WireFormat = 0x0004
	WireFormatKeyPackage     WireFormat = 0x0005
)

// ContentType is the framing content type a FramedContent and a PrivateMessage header carry,
// from RFC 9420 section 6: it selects which arm of FramedContent is populated, and it selects
// which of a leaf's two ratchets protects the message.
//
// Its underlying type is the octet the wire format gives it, and no wider. A content type
// decoded at two octets moves every field after it in the header.
type ContentType uint8

// The three content types RFC 9420 section 6 registers. The zero value is deliberately none of
// them -- it is the reserved code point, and a zero that silently meant "application" would
// route an unparseable header onto a real ratchet rather than being refused.
const (
	ContentTypeApplication ContentType = 1
	ContentTypeProposal    ContentType = 2
	ContentTypeCommit      ContentType = 3
)

// SenderType says who sent a FramedContent. It selects the signature key, it selects whether
// the group context is part of the signature preimage, and it selects which payload the Sender
// structure carries. Eight bits, per the enum RFC 9420 section 6 writes.
type SenderType uint8

const (
	SenderTypeMember            SenderType = 1
	SenderTypeExternal          SenderType = 2
	SenderTypeNewMemberProposal SenderType = 3
	SenderTypeNewMemberCommit   SenderType = 4
)

// Sender is the sender of a FramedContent, a variant structure over SenderType.
//
// LeafIndex is meaningful only under member and SenderIndex only under external; the two
// new_member arms carry no payload at all. Both fields are uint32 and adjacent, so a codec
// that wrote one where the other belongs round trips perfectly and is byte exact against
// itself -- framing_test.go's hand derived goldens are what separates the two orders, and they
// are written from the RFC rather than read back through this file.
type Sender struct {
	SenderType  SenderType
	LeafIndex   LeafIndex
	SenderIndex uint32
}

// MarshalMLS writes the sender_type octet and then the arm that type selects.
//
// An unregistered sender type is refused rather than written, and the refusal is the encoder's
// semantic one rather than a buffer failure, which is why it is a return value: the sender type
// is inside every signature preimage this layer builds, so an encoder that emitted an
// unregistered one would produce signed bytes no peer can attribute to anybody.
func (self *Sender) MarshalMLS(w *syntax.Writer) error {
	switch self.SenderType {
	case SenderTypeMember:
		w.WriteUint8(uint8(self.SenderType))
		w.WriteUint32(uint32(self.LeafIndex))
	case SenderTypeExternal:
		w.WriteUint8(uint8(self.SenderType))
		w.WriteUint32(self.SenderIndex)
	case SenderTypeNewMemberProposal, SenderTypeNewMemberCommit:
		w.WriteUint8(uint8(self.SenderType))
	default:
		return fmt.Errorf("%w: %d", ErrUnknownSenderType, self.SenderType)
	}
	return nil
}

// UnmarshalMLS reads the discriminant, reads the arm it selects, and only then writes the
// receiver.
//
// The staging is the point and it is not free stylistic tidiness. A decoder that assigned the
// receiver as it read would leave a caller's Sender holding a sender type out of a message this
// package REFUSED -- a value that never existed anywhere, assembled from the first octet of
// somebody else's bytes -- with nothing about the returned error saying so. Credential and
// LeafNode already keep this discipline; a third decoder that did not would be the one place a
// refusal is not clean.
func (self *Sender) UnmarshalMLS(r *syntax.Reader) error {
	senderType, err := r.ReadUint8()
	if err != nil {
		return err
	}
	decoded := Sender{SenderType: SenderType(senderType)}
	switch decoded.SenderType {
	case SenderTypeMember:
		leafIndex, err := r.ReadUint32()
		if err != nil {
			return err
		}
		decoded.LeafIndex = LeafIndex(leafIndex)
	case SenderTypeExternal:
		senderIndex, err := r.ReadUint32()
		if err != nil {
			return err
		}
		decoded.SenderIndex = senderIndex
	case SenderTypeNewMemberProposal, SenderTypeNewMemberCommit:
	default:
		return fmt.Errorf("%w: %d", ErrUnknownSenderType, decoded.SenderType)
	}
	*self = decoded
	return nil
}

var _ syntax.Codec = (*Sender)(nil)

// ---------------------------------------------------------------------------
// FramedContentAuthData
// ---------------------------------------------------------------------------

// errMissingConfirmationTag is ValSem009 in the validation plan's catalogue, and that plan owns
// the single declaration site for ErrMissingConfirmationTag. Neither that name nor ValSem
// itself has landed in this package yet, so the refusal is carried by this unexported value
// until they do.
//
// The shape is extension.go's errMissingRequiredCapability and psk.go's three ValSem401 to
// ValSem403 stand ins, for the reason those files give. An exported ErrMissingConfirmationTag
// declared here would be a second public declaration site for a name the validation plan also
// declares, the two would not be the same value, and a caller matching one would silently stop
// matching the other; a name that cannot be reached from outside this package cannot be
// depended on from outside it either, so the swap costs nobody else anything, and every
// consumer of this refusal until then is inside package mls.
//
// TWO layers raise it and this comment names both, so that the swap below is not written for
// half of them. The codec returns it for a commit whose confirmation tag is missing on the
// wire, in either direction; framing_protect.go's VerifyAuthenticatedContent returns it as
// ValSem009 for a commit that reached the tag rule with none. "A commit carries no confirmation
// tag" is ONE condition however it is reached, which is why there is one value for it rather
// than a second sentinel next door.
//
// The swap is mechanical and it is not left to anybody's memory:
// TestNoValidationOwnedNameHasLandedBesideItsStandIn derives the owed pair from this package's
// own declarations and fails on the commit that lands the real name. When it does, each return
// in this file AND the one in framing_protect.go becomes ValSem(ValSem009,
// ErrMissingConfirmationTag) -- the code CodeOf reads, with the sentinel still reachable
// through (*ValidationError).Unwrap.
var errMissingConfirmationTag = errors.New("mls: commit carries no confirmation tag")

// FramedContentAuthData is the pair of authenticators over a FramedContent: the signature every
// content type carries, and the confirmation tag only a commit does.
//
// The content type is a PARAMETER of both codec methods rather than a field of this struct, and
// that is why this type is deliberately NOT a syntax.Codec and carries no var _ assertion.
// RFC 9420 section 6 writes FramedContentAuthData as a select() on the ENCLOSING
// FramedContent's content_type, so it carries no discriminant of its own on the wire. A copy
// stored here would be a second statement of one fact, the two could disagree, and the copy
// rather than the message would be what decided which arm was read -- which is a receiver
// verifying a confirmation tag the sender never sent, or skipping one it did.
//
// It is also why the validation plan's requested HasConfirmationTag field is refused: tag
// presence is DERIVED from the content type, not carried beside it. Its requested MembershipTag
// field is refused on the neighbouring ground, that the membership tag lives on PublicMessage
// where RFC 9420 puts it. Registry section 7.2 fixes exactly the two signatures below.
type FramedContentAuthData struct {
	Signature       []byte
	ConfirmationTag []byte
}

// MarshalMLS writes the signature, and then the confirmation tag when the content type is
// commit and only then.
//
// Every refusal happens before a single octet is written, which is Sender.MarshalMLS's
// discipline and is load bearing for the same reason. This method is handed the CALLER's
// Writer -- there is no syntax.Marshal here, because that entry point takes a one argument
// Marshaler and this codec needs the content type -- so a refusal that had already written the
// signature would leave a caller that ignored the return value holding a shorter encoding with
// no sticky error on the Writer to say so. A FramedContentAuthData is the tail of every
// structure that carries it, so "shorter" means precisely "the confirmation tag is gone".
//
// A commit with no confirmation tag is refused rather than written short for the same reason
// the arm check on FramedContent is a refusal: the tag is what binds a commit to the epoch it
// creates, and a commit that encoded without one would be a message every peer rejects at
// ValSem009 having verified its signature first.
func (self *FramedContentAuthData) MarshalMLS(w *syntax.Writer, contentType ContentType) error {
	switch contentType {
	case ContentTypeApplication, ContentTypeProposal:
		w.WriteOpaque(self.Signature)
		return nil
	case ContentTypeCommit:
		if len(self.ConfirmationTag) == 0 {
			return errMissingConfirmationTag
		}
		w.WriteOpaque(self.Signature)
		w.WriteOpaque(self.ConfirmationTag)
		return nil
	}
	return fmt.Errorf("%w: %d", ErrUnknownContentType, contentType)
}

// UnmarshalMLS reads the arm the content type selects and only then writes the receiver.
//
// The staging is Sender's and Credential's and LeafNode's, and it has one extra edge here. The
// commit arm reads two fields, so a decoder that assigned as it read would leave a caller's
// value holding the signature out of a message this package REFUSED -- one whose confirmation
// tag was truncated or empty -- beside a confirmation tag from whatever the value held before.
// Nothing in the returned error says so, and the pair is a signature and a tag that were never
// carried together by anything.
//
// The content type is checked before any octet is consumed, so an unregistered one does not
// half consume the caller's Reader on its way to being refused.
//
// This codec cannot tell a commit's bytes from a proposal's on its own and does not try: under
// application and proposal it reads the signature and stops, which leaves a commit's tag
// unconsumed rather than silently swallowing it, and the enclosing decode refuses that tail at
// r.Done(). Guessing here is the failure this shape exists to prevent -- a receiver that read a
// commit's confirmation tag off a proposal's bytes would succeed by accident and authenticate
// the wrong structure.
func (self *FramedContentAuthData) UnmarshalMLS(r *syntax.Reader, contentType ContentType) error {
	switch contentType {
	case ContentTypeApplication, ContentTypeProposal:
		signature, err := r.ReadOpaque()
		if err != nil {
			return err
		}
		*self = FramedContentAuthData{Signature: signature}
		return nil
	case ContentTypeCommit:
		signature, err := r.ReadOpaque()
		if err != nil {
			return err
		}
		confirmationTag, err := r.ReadOpaque()
		if err != nil {
			return err
		}
		// an empty tag is wire legal and is the encoding of "no tag". accepting it would put a
		// commit past the presence check with nothing for MacVerify to compare, so the two
		// halves of this codec agree about it: what the encoder refuses to write, the decoder
		// refuses to read.
		if len(confirmationTag) == 0 {
			return errMissingConfirmationTag
		}
		*self = FramedContentAuthData{Signature: signature, ConfirmationTag: confirmationTag}
		return nil
	}
	return fmt.Errorf("%w: %d", ErrUnknownContentType, contentType)
}

// The two signatures registry section 7.2 fixes, as a compile time statement rather than a
// sentence. syntax.Codec cannot express either of them -- both take the content type -- so the
// var _ syntax.Codec line every other codec in this file carries is absent here on purpose, and
// these two are what stands in its place: a later task that "fixes" this type by storing a
// content type field and narrowing the methods to one argument stops COMPILING here.
var (
	_ func(*FramedContentAuthData, *syntax.Writer, ContentType) error = (*FramedContentAuthData).MarshalMLS
	_ func(*FramedContentAuthData, *syntax.Reader, ContentType) error = (*FramedContentAuthData).UnmarshalMLS
)

// ---------------------------------------------------------------------------
// FramedContent
// ---------------------------------------------------------------------------

// FramedContent is the body of a group message before it is authenticated. Exactly one of
// ApplicationData, Proposal and Commit is populated, selected by ContentType.
//
// This is the structure the signature is taken over and the structure the confirmed transcript
// hash is taken over, which is why the arm check below is a REFUSAL rather than a preference:
// MLS authenticates serialized forms, so an encoder that wrote whichever arm happened to be
// non-nil would sign bytes describing a message the caller did not build, and every peer would
// accept it.
type FramedContent struct {
	GroupId           []byte
	Epoch             uint64
	Sender            Sender
	AuthenticatedData []byte
	ContentType       ContentType
	ApplicationData   []byte
	Proposal          *Proposal
	Commit            *Commit
}

// checkArms reports whether exactly the arm ContentType names is populated.
//
// The application arm is the one that cannot be read by counting. ApplicationData is a []byte
// and its zero value is nil, but an application message carrying no bytes is legal and encodes
// to an empty opaque<V> -- so "populated" for that arm is not a question the other two answer
// the same way, and what is refused there is a proposal or a commit standing beside it.
func (self *FramedContent) checkArms() error {
	populated := 0
	if self.ApplicationData != nil {
		populated += 1
	}
	if self.Proposal != nil {
		populated += 1
	}
	if self.Commit != nil {
		populated += 1
	}
	switch self.ContentType {
	case ContentTypeApplication:
		if self.Proposal != nil || self.Commit != nil {
			return ErrContentArmMismatch
		}
	case ContentTypeProposal:
		if self.Proposal == nil || populated != 1 {
			return ErrContentArmMismatch
		}
	case ContentTypeCommit:
		if self.Commit == nil || populated != 1 {
			return ErrContentArmMismatch
		}
	default:
		return fmt.Errorf("%w: %d", ErrUnknownContentType, self.ContentType)
	}
	return nil
}

// MarshalMLS refuses before it writes, which is Sender.MarshalMLS's discipline and is load
// bearing here for a sharper reason than it is there: this structure is the signature preimage
// and the transcript hash preimage, and a half written one that a caller ignored the error from
// is a preimage shorter than the message it claims to describe.
func (self *FramedContent) MarshalMLS(w *syntax.Writer) error {
	if err := self.checkArms(); err != nil {
		return err
	}
	w.WriteOpaque(self.GroupId)
	w.WriteUint64(self.Epoch)
	if err := self.Sender.MarshalMLS(w); err != nil {
		return err
	}
	w.WriteOpaque(self.AuthenticatedData)
	w.WriteUint8(uint8(self.ContentType))
	switch self.ContentType {
	case ContentTypeApplication:
		w.WriteOpaque(self.ApplicationData)
		return nil
	case ContentTypeProposal:
		return self.Proposal.MarshalMLS(w)
	case ContentTypeCommit:
		return self.Commit.MarshalMLS(w)
	}
	return fmt.Errorf("%w: %d", ErrUnknownContentType, self.ContentType)
}

// UnmarshalMLS reads the five fixed fields, resets the receiver, and reads the arm the content
// type selects.
//
// The reset happens after the fixed fields and before the arm, which is Proposal's arrangement
// and not Sender's: the three arms write three different fields, so a fully staged decode would
// be a second FramedContent built only to be copied over the first. What the reset buys is the
// same thing the staging buys next door -- no arm of a previous value survives into a decode of
// bytes that do not carry one.
func (self *FramedContent) UnmarshalMLS(r *syntax.Reader) error {
	groupId, err := r.ReadOpaque()
	if err != nil {
		return err
	}
	epoch, err := r.ReadUint64()
	if err != nil {
		return err
	}
	sender := Sender{}
	if err := sender.UnmarshalMLS(r); err != nil {
		return err
	}
	authenticatedData, err := r.ReadOpaque()
	if err != nil {
		return err
	}
	contentType, err := r.ReadUint8()
	if err != nil {
		return err
	}
	*self = FramedContent{
		GroupId:           groupId,
		Epoch:             epoch,
		Sender:            sender,
		AuthenticatedData: authenticatedData,
		ContentType:       ContentType(contentType),
	}
	switch self.ContentType {
	case ContentTypeApplication:
		applicationData, err := r.ReadOpaque()
		if err != nil {
			return err
		}
		self.ApplicationData = applicationData
		return nil
	case ContentTypeProposal:
		proposal := &Proposal{}
		if err := proposal.UnmarshalMLS(r); err != nil {
			return err
		}
		self.Proposal = proposal
		return nil
	case ContentTypeCommit:
		commit := &Commit{}
		if err := commit.UnmarshalMLS(r); err != nil {
			return err
		}
		self.Commit = commit
		return nil
	}
	return fmt.Errorf("%w: %d", ErrUnknownContentType, self.ContentType)
}

var _ syntax.Codec = (*FramedContent)(nil)

// ---------------------------------------------------------------------------
// AuthenticatedContent
// ---------------------------------------------------------------------------

// AuthenticatedContent is a FramedContent together with the authenticators over it, independent
// of which wire format carried it. It is the object every validation path works on and the
// object a staged commit holds.
//
// The wire format is a FIELD rather than a parameter, and it is inside the serialization,
// because it is inside two preimages: the signature's FramedContentTBS and the confirmed
// transcript hash's input both begin with it. That is what stops one member's public message
// being replayed as another's private one -- the two produce different preimages, so a signature
// made for one does not verify against the other.
//
// FramedContentAuthData is written through the content type this structure already carries
// rather than through a discriminant of its own, which is why the codec below reaches into
// Content for it. A second copy of the content type stored beside the auth data would be a
// second statement of one fact, and the copy rather than the message would decide which arm was
// read.
type AuthenticatedContent struct {
	WireFormat WireFormat
	Content    FramedContent
	Auth       FramedContentAuthData
}

func (self *AuthenticatedContent) MarshalMLS(w *syntax.Writer) error {
	switch self.WireFormat {
	case WireFormatPublicMessage, WireFormatPrivateMessage, WireFormatWelcome,
		WireFormatGroupInfo, WireFormatKeyPackage:
	default:
		return fmt.Errorf("%w: %d", ErrUnknownWireFormat, self.WireFormat)
	}
	w.WriteUint16(uint16(self.WireFormat))
	if err := self.Content.MarshalMLS(w); err != nil {
		return err
	}
	return self.Auth.MarshalMLS(w, self.Content.ContentType)
}

// UnmarshalMLS reads the wire format, resets the receiver, and reads the content and then the
// auth data the content's own type selects.
//
// The auth data is read with self.Content.ContentType and not with a value read separately,
// which is the whole of what makes this codec correct: the content type came off THESE bytes,
// so the arm the auth data is read at is the arm the sender wrote.
func (self *AuthenticatedContent) UnmarshalMLS(r *syntax.Reader) error {
	wireFormat, err := r.ReadUint16()
	if err != nil {
		return err
	}
	decoded := AuthenticatedContent{WireFormat: WireFormat(wireFormat)}
	switch decoded.WireFormat {
	case WireFormatPublicMessage, WireFormatPrivateMessage, WireFormatWelcome,
		WireFormatGroupInfo, WireFormatKeyPackage:
	default:
		return fmt.Errorf("%w: %d", ErrUnknownWireFormat, decoded.WireFormat)
	}
	if err := decoded.Content.UnmarshalMLS(r); err != nil {
		return err
	}
	if err := decoded.Auth.UnmarshalMLS(r, decoded.Content.ContentType); err != nil {
		return err
	}
	*self = decoded
	return nil
}

var _ syntax.Codec = (*AuthenticatedContent)(nil)

// ---------------------------------------------------------------------------
// PublicMessage
// ---------------------------------------------------------------------------

// PublicMessage is RFC 9420 section 6.2's cleartext wire format: a FramedContent, the
// authenticators over it, and -- for a member sender and only for one -- the membership tag that
// says the sender was inside the group when it wrote the message.
//
// v1 refuses this format by group policy. Spec A's A-ASSUME-4 puts every handshake message in a
// PrivateMessage, and group.go's policyCheck is where that refusal lives. It is implemented in
// full anyway, and the distinction matters to whoever reads this next: it is not dead code, it is
// code the product policy does not reach. The interop harness's nightly -public matrix runs it,
// and ValSem007 and ValSem008 have no other path. Which means a defect here is invisible until
// interop rather than at the first send, so it is written as though it ships.
//
// The membership tag is a FIELD of this structure and not of FramedContentAuthData, which is
// where RFC 9420 puts it and is the reason the validation plan's request to move it was refused:
// the tag is a MAC over the auth data, so a tag stored beside the signature would be part of its
// own preimage.
type PublicMessage struct {
	Content       FramedContent
	Auth          FramedContentAuthData
	MembershipTag []byte
}

// MarshalMLS writes the content, the auth data its content type selects, and then the membership
// tag when the sender is a member and only then.
//
// The tagless member message is refused BEFORE an octet is written, which is
// FramedContentAuthData.MarshalMLS's discipline and is load bearing for the same reason: this
// method is handed the caller's Writer, so a refusal raised after the content had been written
// would leave a caller that ignored the return value holding an encoding of a message whose only
// membership authentication is gone. The tag is the tail of this structure, so "shorter" means
// exactly that.
//
// It is a refusal rather than a short write for ValSem007's reason. A member's PublicMessage
// carries two authenticators and this is one of them; an encoder that wrote the message without
// it would produce a message every peer rejects having verified its signature first.
//
// The guard is spelled on the LENGTH and not on == nil, which is emptyByteSpellings' rule: a
// caller that built its tag by re-slicing a longer buffer to nothing holds a non nil slice with
// capacity, and a guard spelled == nil writes an empty opaque<V> for it -- wire legal, and a
// message no member could have produced.
func (self *PublicMessage) MarshalMLS(w *syntax.Writer) error {
	// section 6.2's select on sender_type, ahead of every write. It is a switch and not an
	// equality because the fourth arm is a REFUSAL: an unregistered sender type has no answer to
	// "does this message carry a membership tag", and an encoder that guessed would either strip
	// a member's only membership authentication or attach one to a sender that has no
	// membership_key to have taken it under. Sender.MarshalMLS refuses the same value one field
	// in; this stands ahead of it so that no octet has been written when the refusal is raised.
	carriesMembershipTag := false
	switch self.Content.Sender.SenderType {
	case SenderTypeMember:
		carriesMembershipTag = true
	case SenderTypeExternal, SenderTypeNewMemberProposal, SenderTypeNewMemberCommit:
	default:
		return fmt.Errorf("%w: %d", ErrUnknownSenderType, self.Content.Sender.SenderType)
	}
	if carriesMembershipTag && len(self.MembershipTag) == 0 {
		return errMissingMembershipTag
	}
	if err := self.Content.MarshalMLS(w); err != nil {
		return err
	}
	if err := self.Auth.MarshalMLS(w, self.Content.ContentType); err != nil {
		return err
	}
	if carriesMembershipTag {
		w.WriteOpaque(self.MembershipTag)
	}
	return nil
}

// UnmarshalMLS reads the content, reads the auth data under the content's OWN content type, reads
// the membership tag the sender type selects, and only then writes the receiver.
//
// The staging is Sender's and AuthenticatedContent's, and it is the property
// TestEveryFramingDecodeThatPublishesItsReceiverWholeLeavesItUntouchedWhenItRefuses holds every
// decoder of this package that makes it. A decoder that assigned as it read would leave a caller
// that reused its receiver holding a FramedContent out of a message this package REFUSED, beside
// a membership tag from whatever the value held before -- a pair nothing ever carried together,
// in a value that re-encodes as though a peer had sent it.
//
// The auth data is read with the content type that came off THESE bytes, which is
// AuthenticatedContent.UnmarshalMLS's rule and the whole of what makes either codec correct.
//
// The empty membership tag is refused rather than accepted, so that the two halves of this codec
// agree about it exactly as FramedContentAuthData's two halves agree about the empty confirmation
// tag: what the encoder refuses to write, the decoder refuses to read. An empty opaque<V> is the
// wire encoding of "no tag", and a decoder that accepted it would hand its caller a member's
// PublicMessage whose ValSem007 refusal is deferred to whether somebody downstream remembered to
// look.
func (self *PublicMessage) UnmarshalMLS(r *syntax.Reader) error {
	decoded := PublicMessage{}
	if err := decoded.Content.UnmarshalMLS(r); err != nil {
		return err
	}
	if err := decoded.Auth.UnmarshalMLS(r, decoded.Content.ContentType); err != nil {
		return err
	}
	// the same select the encoder makes, and its default stands BEHIND Sender.UnmarshalMLS
	// rather than in front of it: that codec refuses an unregistered sender type at the octet it
	// reads it from, so nothing reaches this arm today and the refusal a peer receives is its
	// one. It is written as a refusal anyway, for what the alternative silently does. A default
	// that fell through to the no-tag arm would, the day anything in this package carries an
	// unregistered sender type instead of refusing it, read a member's membership_tag as trailing
	// bytes and a tagless sender's next field as a tag -- a message misframed rather than
	// rejected, which is the failure this whole file is arranged to make impossible.
	switch decoded.Content.Sender.SenderType {
	case SenderTypeMember:
		membershipTag, err := r.ReadOpaque()
		if err != nil {
			return err
		}
		if len(membershipTag) == 0 {
			return errMissingMembershipTag
		}
		decoded.MembershipTag = membershipTag
	case SenderTypeExternal, SenderTypeNewMemberProposal, SenderTypeNewMemberCommit:
	default:
		return fmt.Errorf("%w: %d", ErrUnknownSenderType, decoded.Content.Sender.SenderType)
	}
	*self = decoded
	return nil
}

// AuthenticatedContent is the wire format independent view of this message, which is the object
// every validation path and every staged commit works on.
//
// The wire format is STAMPED as WireFormatPublicMessage rather than carried, because it is a fact
// about the structure the bytes were read out of and not something the message asserts. That is
// what makes the wire format's presence in the signature preimage worth having: a PublicMessage
// replayed as a PrivateMessage arrives through the other codec, is stamped with the other value,
// and produces a preimage the signature does not verify against.
//
// The membership tag is deliberately absent from the answer. It is not part of any preimage the
// signature or the transcript is taken over -- it is a MAC over the whole of the signature's
// preimage -- so a view that carried it would be offering a caller an authenticator with nothing
// to check it against. OpenPublicMessage checks it before it builds this view.
func (self *PublicMessage) AuthenticatedContent() *AuthenticatedContent {
	return &AuthenticatedContent{
		WireFormat: WireFormatPublicMessage,
		Content:    self.Content,
		Auth:       self.Auth,
	}
}

var _ syntax.Codec = (*PublicMessage)(nil)

// ---------------------------------------------------------------------------
// PrivateMessage
// ---------------------------------------------------------------------------

// PrivateMessage is RFC 9420 section 6.3's encrypted wire format, and under A-ASSUME-4 it is the
// only one v1 puts on the wire.
//
// The header is CLEARTEXT and that is the design rather than a leak: the message server orders,
// routes and prunes on group_id and epoch, and it cannot decrypt anything. What keeps the header
// honest is that every one of these four cleartext fields is inside an AAD -- group_id, epoch and
// content_type inside both of section 6.3's, authenticated_data inside the content's -- so a
// header altered in flight is a decryption that fails rather than a message that arrives
// misattributed.
//
// The sender is NOT here. It lives in the encrypted sender data, which is what stops the transport
// learning which member of a group sent which message, and it is why this format's open cannot be
// handed a signature key by its caller: nothing knows whose key it is until the sender data has
// been decrypted. That is the whole reason SignatureKeyResolver exists.
//
// ContentType is a field of the header rather than something read out of the plaintext, because it
// selects which of a leaf's two ratchets protects the message -- so a receiver has to know it
// before it has anything to decrypt with.
type PrivateMessage struct {
	GroupId             []byte
	Epoch               uint64
	ContentType         ContentType
	AuthenticatedData   []byte
	EncryptedSenderData []byte
	Ciphertext          []byte
}

// MarshalMLS refuses an unregistered content type before it writes, which is the discipline every
// encoder in this file keeps and which has a sharper edge here than it does elsewhere.
//
// This header is the AAD of two AEAD operations. An encoder that wrote a content type no registry
// declares would produce a message whose sender data and whose content were sealed under an AAD
// naming a ratchet that does not exist -- and since the same wrong value is read back on the
// receiving side, the message decrypts perfectly against this implementation and against nothing
// else. Nothing that round trips can see it.
func (self *PrivateMessage) MarshalMLS(w *syntax.Writer) error {
	switch self.ContentType {
	case ContentTypeApplication, ContentTypeProposal, ContentTypeCommit:
	default:
		return fmt.Errorf("%w: %d", ErrUnknownContentType, self.ContentType)
	}
	w.WriteOpaque(self.GroupId)
	w.WriteUint64(self.Epoch)
	w.WriteUint8(uint8(self.ContentType))
	w.WriteOpaque(self.AuthenticatedData)
	w.WriteOpaque(self.EncryptedSenderData)
	w.WriteOpaque(self.Ciphertext)
	return nil
}

// UnmarshalMLS reads the six fields, refuses an unregistered content type at the octet it was read
// at, and only then writes the receiver.
//
// The content type is refused HERE and not left to the ratchet lookup, though secret_tree.go's
// ratchetTypeOf refuses the same value: this is the first place the octet is seen, refusing it
// here means no key is ever derived for it, and "a content type this package does not register" is
// one condition with one sentinel however it is reached. What the two do differently is only where
// the caller is standing when they are answered.
//
// The staging is Sender's and AuthenticatedContent's, for the reason PublicMessage.UnmarshalMLS
// gives: a caller that reused its receiver must not be left holding a group_id and an epoch out of
// a header this package refused, over ciphertext fields from whatever it held before.
func (self *PrivateMessage) UnmarshalMLS(r *syntax.Reader) error {
	groupId, err := r.ReadOpaque()
	if err != nil {
		return err
	}
	epoch, err := r.ReadUint64()
	if err != nil {
		return err
	}
	contentType, err := r.ReadUint8()
	if err != nil {
		return err
	}
	switch ContentType(contentType) {
	case ContentTypeApplication, ContentTypeProposal, ContentTypeCommit:
	default:
		return fmt.Errorf("%w: %d", ErrUnknownContentType, ContentType(contentType))
	}
	authenticatedData, err := r.ReadOpaque()
	if err != nil {
		return err
	}
	encryptedSenderData, err := r.ReadOpaque()
	if err != nil {
		return err
	}
	ciphertext, err := r.ReadOpaque()
	if err != nil {
		return err
	}
	*self = PrivateMessage{
		GroupId:             groupId,
		Epoch:               epoch,
		ContentType:         ContentType(contentType),
		AuthenticatedData:   authenticatedData,
		EncryptedSenderData: encryptedSenderData,
		Ciphertext:          ciphertext,
	}
	return nil
}

var _ syntax.Codec = (*PrivateMessage)(nil)

// ---------------------------------------------------------------------------
// SenderData, RFC 9420 section 6.3.2
// ---------------------------------------------------------------------------

// SenderData is who sent a PrivateMessage and at which ratchet generation, RFC 9420 section
// 6.3.2:
//
//	struct {
//	    uint32 leaf_index;
//	    uint32 generation;
//	    opaque reuse_guard[4];
//	} SenderData;
//
// It travels ENCRYPTED, under a key derived from the content ciphertext, and that is the whole
// reason it is a structure of its own rather than three more fields of PrivateMessage's cleartext
// header. The header a message server reads has to name the group and the epoch so the server can
// order, route and prune; it must not name the member who sent the message, or the transport
// learns the group's traffic graph while holding no key at all.
//
// The reuse guard is FOUR OCTETS AND FIXED WIDTH, and it is the field a codec is likeliest to get
// subtly wrong: it is opaque[4] and not opaque<V>, so it carries no length prefix, and an encoder
// that reached for WriteOpaque writes a sender data this implementation agrees with itself about
// perfectly -- both halves would add the prefix -- and no other implementation reads. Nothing that
// round trips can see that, which is why the codec below spells the raw write out and says why.
//
// What the guard is FOR is not this codec's job and is what decides the field's shape: section
// 6.3.2 xors it into the ratchet nonce before the content is sealed, so two senders that reach one
// generation of one ratchet -- which a forked group state makes possible -- do not seal two
// messages under a single nonce. It is CARRIED rather than derived because the receiver has no way
// to recompute a value the sender drew at random.
type SenderData struct {
	LeafIndex  LeafIndex
	Generation uint32
	ReuseGuard [senderDataReuseGuardSize]byte
}

// MarshalMLS writes the three fields in section 6.3.2's order.
//
// There is no code point refusal here and that is the structure rather than an omission every
// other encoder of this file makes: all three fields are fixed width, so there is no discriminant
// that could be unregistered and no vector whose length could fail to encode. What can still fail
// is the Writer, and syntax.Marshal is what answers that to the caller.
func (self *SenderData) MarshalMLS(w *syntax.Writer) error {
	w.WriteUint32(uint32(self.LeafIndex))
	w.WriteUint32(self.Generation)
	// WriteRaw and NOT WriteOpaque, which is privateContentAAD's rule for the same reason:
	// reuse_guard is a fixed width array and not a nested vector, so a length prefix in front
	// of it is an encoding this package would also read back and no peer computes.
	w.WriteRaw(self.ReuseGuard[:])
	return nil
}

// UnmarshalMLS reads the three fields and publishes the receiver ONCE, after the last of them has
// succeeded.
//
// The staging is PrivateMessage.UnmarshalMLS's and it has a sharper edge here than it does there.
// What a decoder that assigned as it read would leave in a caller's receiver is an ATTRIBUTION: a
// leaf index and a generation are what a receiver picks a signature key and a message key by, so a
// receiver stamped out of input this package refused names a member who sent nothing, at a
// generation nobody reached, and it decodes and re-encodes as though a peer had sent it.
func (self *SenderData) UnmarshalMLS(r *syntax.Reader) error {
	leafIndex, err := r.ReadUint32()
	if err != nil {
		return err
	}
	generation, err := r.ReadUint32()
	if err != nil {
		return err
	}
	reuseGuard, err := r.ReadRaw(senderDataReuseGuardSize)
	if err != nil {
		return err
	}
	decoded := SenderData{LeafIndex: LeafIndex(leafIndex), Generation: generation}
	copy(decoded.ReuseGuard[:], reuseGuard)
	*self = decoded
	return nil
}

// senderDataReuseGuardSize is the width of section 6.3.2's reuse_guard, written once so the read
// and the array declaration cannot disagree: a ReadRaw of some other length against a [4]byte
// compiles, copies whatever the two have in common and silently drops or zero fills the rest.
const senderDataReuseGuardSize = 4

var _ syntax.Codec = (*SenderData)(nil)

// ---------------------------------------------------------------------------
// MLSMessage, RFC 9420 section 6
// ---------------------------------------------------------------------------

// MLSMessage is the outermost object on the wire, RFC 9420 section 6:
//
//	struct {
//	    ProtocolVersion version = mls10;
//	    WireFormat wire_format;
//	    select (MLSMessage.wire_format) {
//	        case mls_public_message:  PublicMessage public_message;
//	        case mls_private_message: PrivateMessage private_message;
//	        case mls_welcome:         Welcome welcome;
//	        case mls_group_info:      GroupInfo group_info;
//	        case mls_key_package:     KeyPackage key_package;
//	    };
//	} MLSMessage;
//
// This is the object an attacker reaches first, and the wire_format field is what makes it that:
// it decides which of five decoders runs on the rest of the bytes. So the discriminant is refused
// on its own, ahead of every arm, rather than by whichever arm happens to fail on the input --
// and the version is refused ahead of the discriminant, because that is the order the fields
// stand in and a message from a protocol this package does not implement is not a message whose
// wire format means anything.
//
// The arms are named by direct type rather than through a registry with an init(), because every
// one of them is package mls: there is no import edge to break, and a registry would hide which
// plan owns which type behind a map that says nothing at build time.
//
// Exactly one arm is populated, and the encoder COUNTS rather than trusting the discriminant to
// pick one out. ProposalOrRef measured what the other reading costs one layer down: a value
// carrying two arms encodes to the one its discriminant names, so two distinct values share one
// encoding and the arm nobody named is dropped in silence. Here that arm is dropped out of the
// bytes a peer receives, stores and replays.
type MLSMessage struct {
	Version        ProtocolVersion
	WireFormat     WireFormat
	PublicMessage  *PublicMessage
	PrivateMessage *PrivateMessage
	Welcome        *Welcome
	GroupInfo      *GroupInfo
	KeyPackage     *KeyPackage
}

// populatedArms counts the arms this value carries, whatever its wire format names.
//
// It is separate from the select below because the two ask different questions: the select asks
// which arm the discriminant names, and this asks whether anything was left standing beside it.
func (self *MLSMessage) populatedArms() int {
	populated := 0
	for _, present := range []bool{
		self.PublicMessage != nil,
		self.PrivateMessage != nil,
		self.Welcome != nil,
		self.GroupInfo != nil,
		self.KeyPackage != nil,
	} {
		if present {
			populated += 1
		}
	}
	return populated
}

// MarshalMLS refuses the version, then the wire format, then the arms, and writes nothing until
// all three have passed.
//
// The order is the wire's own, and stating it is the point: a caller holding a message this
// package would not send under any wire format is told it is the VERSION that is wrong, rather
// than being told about whichever field was inspected first. The reverse order refuses a well
// formed mls10 message for the wrong reason the moment a later profile adds a wire format.
//
// Nothing is written before the checks, which is FramedContent.MarshalMLS's discipline: a caller
// that ignored the error must not be able to pass on a four octet header with no message behind
// it, and this header is the one thing every peer parses before it parses anything else.
func (self *MLSMessage) MarshalMLS(w *syntax.Writer) error {
	if self.Version != ProtocolVersionMls10 {
		return fmt.Errorf("%w: %d", ErrUnsupportedVersion, self.Version)
	}
	var arm syntax.Marshaler
	switch self.WireFormat {
	case WireFormatPublicMessage:
		if self.PublicMessage != nil {
			arm = self.PublicMessage
		}
	case WireFormatPrivateMessage:
		if self.PrivateMessage != nil {
			arm = self.PrivateMessage
		}
	case WireFormatWelcome:
		if self.Welcome != nil {
			arm = self.Welcome
		}
	case WireFormatGroupInfo:
		if self.GroupInfo != nil {
			arm = self.GroupInfo
		}
	case WireFormatKeyPackage:
		if self.KeyPackage != nil {
			arm = self.KeyPackage
		}
	default:
		return fmt.Errorf("%w: %d", ErrUnknownWireFormat, self.WireFormat)
	}
	// the arm the discriminant names has to be present, and it has to be the only one
	if arm == nil || self.populatedArms() != 1 {
		return ErrContentArmMismatch
	}
	w.WriteUint16(uint16(self.Version))
	w.WriteUint16(uint16(self.WireFormat))
	return arm.MarshalMLS(w)
}

// UnmarshalMLS reads the version, refuses anything but mls10, reads the wire format, refuses
// anything the registry does not declare, and only then enters an arm.
//
// Both refusals stand AHEAD of every arm decoder, and that is this method's security property
// rather than a tidiness: the discriminant chooses which parser runs on attacker controlled
// bytes, so a decoder that entered an arm first and let the arm's own parse decide would be
// running one of five grammars on the strength of a field it had not checked. The refusal of an
// unregistered wire format therefore cannot depend on what stands behind it -- a registered arm's
// bytes, garbage, or nothing at all are refused identically.
//
// The receiver is published whole and last, which is welcome_wire.go's convention and is here for
// its reason with the blast radius one layer wider: a caller that reused its MLSMessage and
// checked the error must not be left holding a version and a wire format read out of a frame this
// package refused, over an arm from whatever it held before -- which is a well formed message
// describing something nobody sent.
func (self *MLSMessage) UnmarshalMLS(r *syntax.Reader) error {
	version, err := r.ReadUint16()
	if err != nil {
		return err
	}
	if ProtocolVersion(version) != ProtocolVersionMls10 {
		return fmt.Errorf("%w: %d", ErrUnsupportedVersion, version)
	}
	wireFormat, err := r.ReadUint16()
	if err != nil {
		return err
	}
	decoded := MLSMessage{Version: ProtocolVersion(version), WireFormat: WireFormat(wireFormat)}
	switch decoded.WireFormat {
	case WireFormatPublicMessage:
		arm := &PublicMessage{}
		if err := arm.UnmarshalMLS(r); err != nil {
			return err
		}
		decoded.PublicMessage = arm
	case WireFormatPrivateMessage:
		arm := &PrivateMessage{}
		if err := arm.UnmarshalMLS(r); err != nil {
			return err
		}
		decoded.PrivateMessage = arm
	case WireFormatWelcome:
		arm := &Welcome{}
		if err := arm.UnmarshalMLS(r); err != nil {
			return err
		}
		decoded.Welcome = arm
	case WireFormatGroupInfo:
		arm := &GroupInfo{}
		if err := arm.UnmarshalMLS(r); err != nil {
			return err
		}
		decoded.GroupInfo = arm
	case WireFormatKeyPackage:
		arm := &KeyPackage{}
		if err := arm.UnmarshalMLS(r); err != nil {
			return err
		}
		decoded.KeyPackage = arm
	default:
		return fmt.Errorf("%w: %d", ErrUnknownWireFormat, decoded.WireFormat)
	}
	*self = decoded
	return nil
}

var _ syntax.Codec = (*MLSMessage)(nil)

// MarshalMLSMessage and ParseMLSMessage are the one sanctioned pair of byte level free functions
// outside the validation plan's codec table, registry C1 section 7.2. Every byte this system puts
// on the wire leaves through the first and every byte that arrives enters through the second, so
// there is one place to read for what is emitted and one for what is accepted.
// errNilMLSMessage is what MarshalMLSMessage answers a caller that passed no message, for
// errNilFramedContent's reason: a nil dereference out of a library takes the caller's process
// rather than its call, and says nothing about which argument was wrong. It matters here because
// this is the function every outbound byte of this system leaves through.
var errNilMLSMessage = errors.New("mls: marshalling a message requires a message")

func MarshalMLSMessage(message *MLSMessage) ([]byte, error) {
	if message == nil {
		return nil, errNilMLSMessage
	}
	return syntax.Marshal(message)
}

// ParseMLSMessage is the single entry point for every byte that arrives from the network or out
// of the store.
//
// syntax.Unmarshal is what makes it an entry point rather than a decode: it joins the decoder's
// answer with Done, so a frame carrying trailing bytes is refused instead of being silently
// truncated to whatever the decoder happened to consume. That matters more here than anywhere
// below it -- an MLSMessage is what a peer stores and forwards, and a decoder that ignored a tail
// would let one frame be read as two different messages by two implementations.
//
// Nothing is returned beside an error. A partially populated message handed back with a refusal
// is a value a caller can log, forward or match against, and every field in it would have come
// out of bytes this package would not accept.
//
// The DEFAULT vector length limit, which is a ceiling this records rather than hides. A Welcome or
// a GroupInfo carrying this product's own ratchet_tree extension is larger than
// syntax.MaxVectorLength -- welcome_wire_test.go measures it over a real thousand leaf tree in
// TestAGroupInfoAndAWelcomeCarryingThisProductsTreeNeedTheRaisedLimitInBothDirections -- so those
// two arms do not fit through this function at this bound, and the lifecycle plan that carries
// them needs an entry point of its own wired to syntax.MaxRatchetTreeLength.
// TestParseMLSMessageCannotCarryThisProductsOwnGroupInfoOrWelcome is that measurement stated at
// this layer, so the ceiling is a failing test away from being noticed rather than a remark in a
// comment. The bound is NOT raised here, and that is the decision: a Welcome is decoded by a party
// who is not yet a member, with no group state to check it against and every length in it chosen
// by whoever sent it, so a raised limit at this entry point would be an acceptance rule handed to
// a stranger over the largest allocation the structure has.
func ParseMLSMessage(data []byte) (*MLSMessage, error) {
	message := &MLSMessage{}
	if err := syntax.Unmarshal(data, message); err != nil {
		return nil, err
	}
	return message, nil
}
