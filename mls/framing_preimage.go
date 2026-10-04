// The byte strings RFC 9420 section 6 authenticates or hashes. One structure and one function
// each, one file, no key material: an auditor reading this file sees every preimage in the
// implementation and nothing else.
//
// The isolation is the point. A preimage defect does not produce a wrong ANSWER -- every one of
// these produces a well formed hash or a well formed signature over whatever it was handed. It
// produces two groups: one that agrees with itself and diverges permanently from every peer that
// assembled the same structure differently, from the first commit where they disagree, with no
// recovery path that does not involve somebody noticing. A one file diff is what makes that
// reviewable.
//
// Each preimage is declared as the STRUCTURE the RFC writes it as, with the one codec convention
// C1 allows, rather than as a writer that lays the fields out by hand inside the function that
// hashes them. That is not tidiness. A hand laid preimage is a second encoder over fields that
// already have one, it agrees with the first until the day one of them changes, and no round
// trip property in this package can see the disagreement -- which is exactly the shape
// TestNoTypeOfThisPackageCarriesAByteLevelCodecOfItsOwn exists to refuse.
package mls

import (
	"errors"
	"fmt"

	"github.com/urnetwork/connect/mls/syntax"
)

// confirmedTranscriptHashInput is RFC 9420 section 8.2's structure of that name:
//
//	struct {
//	    WireFormat wire_format;
//	    FramedContent content; /* with content_type == commit */
//	    opaque signature<V>;
//	} ConfirmedTranscriptHashInput;
//
// It is unexported because nothing outside this package needs the structure -- p4's transcript
// chain takes the serialized BYTES, which is the interface registry's boundary and what keeps
// framing types out of transcript.go. What it needs to BE is a type with a codec, so that the
// preimage is written by one encoder rather than by a second hand rolled layout of fields
// AuthenticatedContent already knows how to write.
//
// It carries the signature but NOT the confirmation tag, and that omission is the whole design
// of section 8.2: the tag is a MAC over the confirmed transcript hash, so a confirmed hash that
// covered the tag could not be computed before the tag it needs. Dropping the signature instead,
// or keeping the tag as well, each produces a chain that is self consistent and shared with
// nobody.
type confirmedTranscriptHashInput struct {
	WireFormat WireFormat
	Content    FramedContent
	Signature  []byte
}

// MarshalMLS refuses an unregistered wire format before it writes, for the reason
// AuthenticatedContent.MarshalMLS does: this is a hash preimage, and a preimage whose first
// field is a code point no registry declares is a transcript entry no peer can reproduce.
//
// The signature's length prefix is syntax's opaque<V> varint and NOT the record layer's fixed
// thirty two bit one. The two are never interchangeable and the substitution is invisible from
// inside a group: WriteOpaqueLP here produces a transcript that agrees with itself on both sides
// of every group running this code and with no other implementation in the world. transcript.go
// refuses the same substitution one layer up, for the same reason, and says so at length.
func (self *confirmedTranscriptHashInput) MarshalMLS(w *syntax.Writer) error {
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
	w.WriteOpaque(self.Signature)
	return nil
}

// UnmarshalMLS is the direction nothing in production needs and the tests do: it is what lets
// the published transcript corpus be read back as the structure section 8.2 names, rather than
// as an offset somebody computed. A preimage that only encodes is a preimage no test can hold
// against bytes it did not produce.
func (self *confirmedTranscriptHashInput) UnmarshalMLS(r *syntax.Reader) error {
	wireFormat, err := r.ReadUint16()
	if err != nil {
		return err
	}
	decoded := confirmedTranscriptHashInput{WireFormat: WireFormat(wireFormat)}
	switch decoded.WireFormat {
	case WireFormatPublicMessage, WireFormatPrivateMessage, WireFormatWelcome,
		WireFormatGroupInfo, WireFormatKeyPackage:
	default:
		return fmt.Errorf("%w: %d", ErrUnknownWireFormat, decoded.WireFormat)
	}
	if err := decoded.Content.UnmarshalMLS(r); err != nil {
		return err
	}
	signature, err := r.ReadOpaque()
	if err != nil {
		return err
	}
	decoded.Signature = signature
	*self = decoded
	return nil
}

var _ syntax.Codec = (*confirmedTranscriptHashInput)(nil)

// transcriptHashInput projects the section 8.2 structure out of the authenticated content the
// transcript is being advanced over.
//
// It answers the STRUCTURE rather than its bytes, which is what keeps the serialization in one
// place: the three fields are named here, the layout is named in MarshalMLS above, and nothing
// does both.
func (self *AuthenticatedContent) transcriptHashInput() *confirmedTranscriptHashInput {
	return &confirmedTranscriptHashInput{
		WireFormat: self.WireFormat,
		Content:    self.Content,
		Signature:  self.Auth.Signature,
	}
}

// ConfirmedTranscriptHashInput is the serialized section 8.2 input p4's ConfirmedTranscriptHash
// consumes: Hash(interim_transcript_hash_[n-1] || these bytes) is epoch n's confirmed hash.
//
// Only a commit has one. Section 8.2 chains the transcript over commits, so asking a proposal or
// an application message for an input is a caller error rather than an encoding this could
// produce, and it is refused rather than answered: a transcript that folded in a non-commit is a
// transcript no peer computes the same way, which is the fork the whole of this file exists to
// prevent.
//
// The transcript arithmetic itself is transcript.go's. It consumes these bytes and never sees a
// framing type.
func (self *AuthenticatedContent) ConfirmedTranscriptHashInput() ([]byte, error) {
	if self.Content.ContentType != ContentTypeCommit {
		return nil, fmt.Errorf("%w: transcript hash input requires a commit", ErrContentArmMismatch)
	}
	return syntax.Marshal(self.transcriptHashInput())
}

// ProposalRef is the HashReference a Commit uses to name a by-reference proposal, RFC 9420
// section 5.2.
//
// The hash is over the SERIALIZED AuthenticatedContent and not over the Proposal, so the ref
// covers the wire format, the sender, the epoch and the signature as well as the proposed
// change. Two members proposing the same removal therefore produce different refs, which is what
// stops one member's proposal being committed under another's name -- and a ref taken over the
// FramedContent instead, or over the Proposal, is stable, self consistent and wrong: a commit
// naming a proposal by a ref no peer computes the same way is a commit nobody can apply.
//
// The label and the hash are the crypto plan's MakeProposalRef, reached rather than reproduced.
// The label is the entire domain separation between a proposal reference and a key package
// reference, and a second spelling of it here would be a second opinion about which of the two a
// given digest is.
//
// THE MARSHAL IS BOUNDED AND THE REFERENCE IS TAKEN FROM WHAT IT ANSWERS, and that order is the
// whole of why this line is not a plain syntax.Marshal. MakeProposalRef wraps these bytes in ONE
// opaque<V> of a RefHashInput, and RefHash cannot report a refusal — it answers a digest by the
// fixed signature the tree and the framing layers call it through, so an over long value stops
// the process instead. An AuthenticatedContent is a COMPOSITION: its group_id, its
// authenticated_data, the arms of the proposal it carries and its signature are each bounded by
// syntax.MaxVectorLength and their SUM is not, and a decoder produces such a value rather than
// refusing it. Measured, by TestTheEnormousAddIsTheOctetCountsTheseDisclosuresState: an Add whose
// key package carries a BasicCredential of MaxVectorLength-64 octets marshals to 1050045 octets and
// decodes back, and on the build that had no bound it signed and verified as an authentic member
// message. THIS PARAGRAPH SAID 1050064 UNTIL SOMEBODY MEASURED IT. That number is also real and is
// the length of something else -- the FramedContentTBS a signature over the same value covers --
// and what this method wraps is the serialized AuthenticatedContent, which is the 1050045. The
// sentence had been copied from crypto_labels.go with the wrong one of the two numbers in it, and
// nothing measured either.
//
// So the refusal is HERE, at the outermost declaration that can carry one, and not at
// (*ProposalCache).Store or at any other caller. Store computes this reference before its
// ceilings on purpose — the reference is the key, so a ceiling asked first would refuse a
// re-delivery of a message the cache already holds — which means a bound written into Store
// would sit downstream of the crash it was written to prevent, and would do nothing for the
// callers of this method that are not Store.
func (self *AuthenticatedContent) ProposalRef(crypto CryptoProvider) (ProposalRef, error) {
	// the provider first, before the receiver is judged. A body that checked the content type
	// first would answer ErrContentArmMismatch to a caller whose actual mistake was passing no
	// provider, and send it to fix a message that was never the problem.
	if crypto == nil {
		return nil, fmt.Errorf("%w: the reference is a hash and the hash is the provider's", ErrNilCryptoProvider)
	}
	if self.Content.ContentType != ContentTypeProposal {
		return nil, fmt.Errorf("%w: a proposal ref requires a proposal", ErrContentArmMismatch)
	}
	encoded, err := marshalBoundedComposition("authenticated content", self)
	if err != nil {
		return nil, err
	}
	reference, err := MakeProposalRef(crypto, encoded)
	if err != nil {
		return nil, err
	}
	return ProposalRef(reference), nil
}

// ---------------------------------------------------------------------------
// FramedContentTBS
// ---------------------------------------------------------------------------

// errNilFramedContent is what FramedContentTBSBytes answers a caller that passed no content
// at all.
//
// A refusal rather than a dereference, for ErrNilCryptoProvider's reason one layer over: a
// panic out of a library takes the caller's process rather than its call, and says nothing
// about which argument was wrong. It is unexported because no sibling plan owns an exported
// twin -- the validation catalogue's ten are structural framing errors and this is an
// argument check -- and the stand in gate watches it for one landing beside it.
var errNilFramedContent = errors.New("mls: framed content tbs requires a framed content")

// senderBindsGroupContext reports whether the FramedContentTBS of a sender of this type
// inlines the group context, which is RFC 9420 section 6.1's select on sender_type.
//
// It is a function rather than two lines inside MarshalMLS because the rule is a fact about
// the SENDER TYPE and is asked twice about one preimage -- once to refuse a caller that
// supplied the wrong thing, once to decide what to write -- and two spellings of "does this
// sender bind the epoch" are exactly the pair that can disagree. A preimage built on one
// answer and validated against the other verifies against itself and against nobody.
//
// The two arms do not cost the same thing when they are wrong, which is why neither is a
// default. member and new_member_commit bind the group context, so a preimage that omitted it
// is a signature valid in EVERY epoch of the group -- a message a peer replays into a later
// epoch and every implementation accepts. external and new_member_proposal do not, so a
// preimage that added one is a signature no other implementation computes.
func senderBindsGroupContext(senderType SenderType) (bool, error) {
	switch senderType {
	case SenderTypeMember, SenderTypeNewMemberCommit:
		return true, nil
	case SenderTypeExternal, SenderTypeNewMemberProposal:
		return false, nil
	}
	return false, fmt.Errorf("%w: %d", ErrUnknownSenderType, senderType)
}

// framedContentTBS is RFC 9420 section 6.1's structure of that name:
//
//	struct {
//	    ProtocolVersion version = mls10;
//	    WireFormat wire_format;
//	    FramedContent content;
//	    select (FramedContentTBS.content.sender.sender_type) {
//	        case member:
//	        case new_member_commit:
//	            GroupContext context;
//	        case external:
//	        case new_member_proposal:
//	            struct{};
//	    };
//	} FramedContentTBS;
//
// It is unexported for confirmedTranscriptHashInput's reason: every caller wants the
// serialized BYTES and takes them through FramedContentTBSBytes, while what this needs to BE
// is a type with a codec, so that the preimage is written by one encoder rather than by a
// second hand rolled layout of fields FramedContent already knows how to write.
//
// GroupContext is the ALREADY SERIALIZED context rather than a *GroupContext, which is the one
// place this structure departs from the RFC's spelling and is worth being exact about. The
// presentation language writes a GroupContext STRUCT here, so the bytes are inlined with no
// length prefix of their own -- and every caller in this package already holds the encoded
// form, because the key schedule expands over exactly those bytes. Taking the struct instead
// would be a second encode of a value the caller has already encoded, and two encodes of one
// value agree until the day one of them changes.
type framedContentTBS struct {
	WireFormat   WireFormat
	Content      FramedContent
	GroupContext []byte
}

// MarshalMLS refuses an unregistered wire format before it writes, for the reason
// confirmedTranscriptHashInput.MarshalMLS does and one degree more sharply: the wire format is
// in this preimage precisely so that a PublicMessage cannot be replayed as a PrivateMessage or
// the reverse, and a signature over a code point no registry declares is one no peer can
// attribute to either.
//
// The group context goes through WriteRaw and NOT WriteOpaque, and that is this preimage's
// second trap. An opaque<V> here puts a varint length prefix in front of the context, which
// produces a signature this package verifies perfectly against itself and every other
// implementation rejects. Nothing round trips through this structure, so no symmetry property
// in the package can see the substitution; what sees it is the published corpus.
func (self *framedContentTBS) MarshalMLS(w *syntax.Writer) error {
	switch self.WireFormat {
	case WireFormatPublicMessage, WireFormatPrivateMessage, WireFormatWelcome,
		WireFormatGroupInfo, WireFormatKeyPackage:
	default:
		return fmt.Errorf("%w: %d", ErrUnknownWireFormat, self.WireFormat)
	}
	binds, err := senderBindsGroupContext(self.Content.Sender.SenderType)
	if err != nil {
		return err
	}
	// both directions are refusals rather than corrections, and both happen before an octet
	// is written. A caller that passed a context believes it is being covered, and a preimage
	// one field shorter than the caller thinks is what a cross epoch replay is built out of;
	// a caller that passed none for a sender type that binds one would otherwise sign a
	// message that is valid in every epoch this group ever has.
	switch {
	case binds && len(self.GroupContext) == 0:
		return ErrMissingGroupContext
	case !binds && len(self.GroupContext) != 0:
		return ErrUnexpectedGroupContext
	}
	w.WriteUint16(uint16(ProtocolVersionMls10))
	w.WriteUint16(uint16(self.WireFormat))
	if err := self.Content.MarshalMLS(w); err != nil {
		return err
	}
	w.WriteRaw(self.GroupContext)
	return nil
}

// The signature registry section 7.2 fixes for this codec, as a compile time statement rather
// than a sentence.
//
// A signature pin and not the var _ syntax.Codec line every other structure in this file
// carries, because this type deliberately has no UnmarshalMLS and cannot satisfy that
// interface. A FramedContentTBS appears on no wire: it is assembled by a signer and
// reassembled by a verifier out of a message that was decoded through AuthenticatedContent's
// own codec, and the group context inside it carries no length prefix, so a decoder would have
// to read to the end of its reader and could not tell a truncated context from a whole one. A
// later task that "completes" this codec by adding one stops compiling here.
var _ func(*framedContentTBS, *syntax.Writer) error = (*framedContentTBS).MarshalMLS

// FramedContentTBSBytes is the byte string RFC 9420 section 6.1 signs under the label
// "FramedContentTBS", and the byte string a verifier rebuilds in order to check one.
//
// groupContext is an ALREADY SERIALIZED GroupContext, inlined with no length prefix, present
// only for the two sender types whose signatures are bound to an epoch. Supplying it for the
// other two is refused rather than ignored -- senderBindsGroupContext says what each direction
// costs.
func FramedContentTBSBytes(wireFormat WireFormat, content *FramedContent, groupContext []byte) ([]byte, error) {
	if content == nil {
		return nil, errNilFramedContent
	}
	tbs := &framedContentTBS{
		WireFormat:   wireFormat,
		Content:      *content,
		GroupContext: groupContext,
	}
	return syntax.Marshal(tbs)
}

// ---------------------------------------------------------------------------
// AuthenticatedContentTBM
// ---------------------------------------------------------------------------

// AuthenticatedContentTBMBytes is the byte string RFC 9420 section 6.1 takes the membership
// tag over:
//
//	struct {
//	    FramedContentTBS content_tbs;
//	    FramedContentAuthData auth;
//	} AuthenticatedContentTBM;
//
// The membership tag is MAC(membership_key, these bytes), and it is the whole of what says a
// PublicMessage came from a member of this group rather than from anybody who can reach the
// transport. p4's (*KeySchedule).MembershipTag and VerifyMembershipTag consume exactly this,
// AS BYTES, because that plan owns the key schedule and never sees a framing type -- so the
// binding of the epoch and of the authenticators into the tag lives HERE and nowhere else,
// and a TBM assembled a field short is a tag that verifies in a place it should not.
//
// It is a concatenation of a preimage and a structure rather than a third structure with a
// codec of its own, which is this file's one departure from the rule its header states, and
// the reason is the one that makes framedContentTBS take an ALREADY SERIALIZED GroupContext.
// The first half of this preimage IS another preimage. A structure declared here would hold a
// framedContentTBS, and whoever built it would assemble those three fields a second time,
// beside the assembly FramedContentTBSBytes already makes -- two assemblies of one preimage,
// agreeing until the day one of them changes, which is exactly the drift this file exists to
// prevent. Written this way there is ONE assembly of the section 6.1 preimage, and "the TBM
// begins with the bytes the signature was checked against" is true by construction rather
// than by a test that has to keep being true.
//
// The auth data is written under the content type of the message's OWN FramedContent, so a
// commit's membership tag covers that commit's confirmation_tag and a proposal's covers its
// signature alone. That is what stops a membership tag being lifted off one commit onto
// another: the confirmation tag is inside what the tag authenticates, and a TBM taken over
// the FramedContent alone -- the shape that reads like the obvious one, since the tag travels
// beside the content on the wire -- authenticates neither authenticator.
//
// Every field is read off the MESSAGE rather than off the caller, the group context excepted:
// that one is the receiver's own epoch and not something an unauthenticated message gets to
// assert. A TBM built under a wire format the caller named rather than the message's own
// would be a tag that survives a PublicMessage replayed as a PrivateMessage, which is the
// substitution the wire format is in the preimage to refuse.
func AuthenticatedContentTBMBytes(authContent *AuthenticatedContent, groupContext []byte) ([]byte, error) {
	// a refusal rather than a dereference, for errNilFramedContent's reason: a panic out of a
	// library takes the caller's process rather than its call, and says nothing about which
	// argument was wrong.
	if authContent == nil {
		return nil, errNilAuthenticatedContent
	}
	tbs, err := FramedContentTBSBytes(authContent.WireFormat, &authContent.Content, groupContext)
	if err != nil {
		return nil, err
	}
	w := syntax.NewWriter()
	// WriteRaw and NOT WriteOpaque, for the reason framedContentTBS.MarshalMLS gives about the
	// group context one layer down. The presentation language writes a FramedContentTBS STRUCT
	// here, so its octets are inlined with no length prefix of their own; an opaque<V> would
	// produce a tag this package verifies perfectly against itself and every other
	// implementation rejects, and nothing round trips through this preimage, so no symmetry
	// property in the package could see it. What sees it is the published corpus.
	w.WriteRaw(tbs)
	if err := authContent.Auth.MarshalMLS(w, authContent.Content.ContentType); err != nil {
		return nil, err
	}
	return w.Bytes()
}

// ---------------------------------------------------------------------------
// the two section 6.3 AADs
// ---------------------------------------------------------------------------

// senderDataAAD is RFC 9420 section 6.3.2's structure of that name:
//
//	struct {
//	    opaque group_id<V>;
//	    uint64 epoch;
//	    ContentType content_type;
//	} SenderDataAAD;
//
// It is the associated data the sender data of a PrivateMessage is sealed under, and it is
// exactly the cleartext header a message server is allowed to read -- which is the point of it.
// Those three fields travel in the clear so the server can order and prune on them; putting them
// in the AAD is what makes altering one a decryption that fails rather than a message that
// arrives attributed to another group or another epoch.
//
// It does NOT carry authenticated_data, and that omission is the whole design of the pair rather
// than a field somebody forgot. The sender data is opened FIRST -- it is what names the leaf whose
// ratchet holds the content key -- so an AAD here that covered a field belonging to the content
// step would make the two steps mutually dependent, and there would be no order in which a
// receiver could do them.
//
// The omission is carried by the SIGNATURE and not by this comment, which is connect/message's
// guardrail G4 one layer down: that layer has the same two-AAD shape, found that a shared builder
// taking every field lets a field migrate between the two preimages silently, and closed it by
// giving each AAD only the fields it is allowed to see. This function cannot reach
// authenticated_data because it is not a parameter of it. A later task that wants it here has to
// widen the signature, which is a diff a reviewer sees.
//
// The content type is REFUSED before an octet is written, which is what every other preimage of
// this file does with the code point it opens on and what this one did not do. An AAD is not a
// decode and that is exactly why it needs the guard rather than inheriting one: nothing
// downstream ever re-reads this field, so an unregistered content type here is not a message
// somebody rejects later, it is an AEAD sealed under associated data no peer can reproduce --
// which arrives at the far end as a decryption failure with nothing in it that says which field
// was wrong. The refusal names the value for framing.go's package comment's reason: the octet is
// what a caller has off the wire anyway. Both section 6.3 AADs are held to it, the content one
// through the assembly below rather than through a second switch.
func senderDataAAD(groupId []byte, epoch uint64, contentType ContentType) ([]byte, error) {
	switch contentType {
	case ContentTypeApplication, ContentTypeProposal, ContentTypeCommit:
	default:
		return nil, fmt.Errorf("%w: %d", ErrUnknownContentType, contentType)
	}
	w := syntax.NewWriter()
	w.WriteOpaque(groupId)
	w.WriteUint64(epoch)
	w.WriteUint8(uint8(contentType))
	return w.Bytes()
}

// privateContentAAD is RFC 9420 section 6.3.1's structure of that name:
//
//	struct {
//	    opaque group_id<V>;
//	    uint64 epoch;
//	    ContentType content_type;
//	    opaque authenticated_data<V>;
//	} PrivateContentAAD;
//
// It is the associated data the FramedContentTBS ciphertext is sealed under, and it is the sender
// data's AAD followed by one more field.
//
// It is built by CALLING senderDataAAD rather than by writing those three fields again, which is
// AuthenticatedContentTBMBytes' arrangement and it is here for that function's reason. The first
// three fields of section 6.3.1's structure are section 6.3.2's structure, field for field and
// octet for octet. Written out a second time they would be two assemblies of one header, agreeing
// until the day one of them changes -- and nothing that round trips could see the disagreement,
// because each AAD is only ever checked against itself. Written this way there is ONE assembly of
// the shared header, so "the content AAD begins with the sender data AAD" is true by construction
// rather than by a test that has to keep being true, and a field added to the header lands in both
// because there is only one place to add it.
//
// The direction that is NOT structural is the one that matters, and it is stated here so the next
// reader knows which way the guard points: a field can never migrate OUT of the content AAD into
// the sender data's by accident, since this function's extra field is written here and
// senderDataAAD cannot see it. The reverse -- widening senderDataAAD, which would put
// authenticated_data into both -- is prevented by that function's parameter list rather than by
// this one, and TestNeitherSectionSixThreeAadCoversAFieldTheOtherOwns is what observes both.
//
// The SINGLE ASSEMBLY has two observers of its own, because the paragraph above it was for a
// while the whole of what held it. This function rewritten to lay the three shared fields out
// again by hand is byte for byte identical to this one at every input, so no golden, no
// collision sweep and no prefix property in this package can see the difference -- measured, the
// duplication passed the whole suite. What sees it now is
// TestEverySectionSixThreeAadAssemblesTheSharedHeaderExactlyOnce, which reads the parameter list
// rather than the answer: a function of this package taking section 6.3's three header fields
// may spend them on ONE call to the assembly below it and nowhere else. And behaviourally,
// TestBothSectionSixThreeAadsRefuseAContentTypeNoRegistryDeclares -- the content AAD holds no
// content type switch of its own, so the only thing that can refuse an unregistered one on its
// behalf is the assembly it delegates to, and a hand laid copy of the header answers an AAD
// where this answers ErrUnknownContentType.
func privateContentAAD(groupId []byte, epoch uint64, contentType ContentType,
	authenticatedData []byte) ([]byte, error) {

	header, err := senderDataAAD(groupId, epoch, contentType)
	if err != nil {
		return nil, err
	}
	w := syntax.NewWriter()
	// WriteRaw and NOT WriteOpaque, for the reason AuthenticatedContentTBMBytes gives one
	// structure up. These octets are the FIELDS of section 6.3.1's struct and not a nested
	// vector inside it, so a length prefix in front of them produces an AAD this package agrees
	// with itself about perfectly -- both halves would add it -- and no other implementation
	// computes. Nothing round trips through an AAD, so no symmetry property in this package
	// could see it.
	w.WriteRaw(header)
	w.WriteOpaque(authenticatedData)
	return w.Bytes()
}
