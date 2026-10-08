// The STRUCTURAL errors RFC 9420 section 6 framing can produce: a malformed
// discriminant, an arm that disagrees with its content type, a group context supplied
// where the sender type forbids it, a padding length nothing can encode.
//
// Structural is the whole of the scope. The ten ValSem002-011 sentinels -- ErrWrongGroupId,
// ErrWrongEpoch, ErrBlankSenderLeaf, ErrApplicationMustBeCiphertext, ErrDecryptFailed,
// ErrMissingMembershipTag, ErrBadMembershipTag, ErrMissingConfirmationTag, ErrBadSignature
// and ErrNonZeroPadding -- are NOT here. They are the validation plan's errors.go, they are
// the codes gate 3 asserts, and a second declaration of any of them in this package is a
// compile error rather than a difference of opinion.
//
// ErrUnknownContentType is not here either, and that one is a deviation from this plan's own
// task list worth naming. It already exists, in errors_key_schedule.go, declared by the plan
// that owns the secret tree because the secret tree is what refuses a content type with no
// ratchet behind it. "A framing content type this package does not register" is ONE condition
// however you reach it -- off the wire at the codec, or off a header at the ratchet lookup --
// and two sentinels for one condition is the exact failure every roster in this package is
// built to prevent. So the framing layer consumes that declaration rather than shadowing it,
// and framing_test.go asserts the name still resolves to errors_key_schedule.go, so a later
// task that adds a second one fails rather than silently splitting the condition in two.
package mls

import "errors"

var (
	// ErrUnknownWireFormat is returned for an MLSMessage wire format outside the IANA MLS
	// Wire Formats registry. It is a refusal rather than an opaque carry because the wire
	// format selects which of five structures the remaining bytes are, and there is no
	// "unknown" arm to put them in.
	ErrUnknownWireFormat = errors.New("mls: unknown wire format")

	// ErrUnsupportedVersion is returned for a protocol version that is not mls10. Every
	// MLSMessage carries the version ahead of the wire format, so this is the first thing
	// a parse can refuse and the cheapest.
	ErrUnsupportedVersion = errors.New("mls: unsupported protocol version")

	// ErrUnknownSenderType is returned for a sender_type outside RFC 9420 section 6's four,
	// the reserved zero included. The sender type selects both the signature key and whether
	// the group context is part of the signature preimage, so a default arm here would sign
	// or verify over a preimage nobody agreed on.
	ErrUnknownSenderType = errors.New("mls: unknown sender type")

	// ErrContentArmMismatch is returned when a FramedContent's populated arm disagrees with
	// its content_type discriminant. The encoder raises it rather than writing whichever arm
	// happens to be non-nil, because MLS signs over serialized forms and an encoder that
	// guessed would sign bytes the caller did not build.
	ErrContentArmMismatch = errors.New("mls: framed content arm does not match content type")

	// ErrMissingGroupContext is returned when a sender type whose FramedContentTBS inlines
	// the group context was given none.
	ErrMissingGroupContext = errors.New("mls: group context required for this sender type")

	// ErrUnexpectedGroupContext is returned when a group context was supplied for a sender
	// type whose FramedContentTBS does not carry one. Refused rather than ignored: a caller
	// that passes one believes it is being covered, and a preimage silently one field shorter
	// than the caller thinks is the shape a cross-epoch replay is built out of.
	ErrUnexpectedGroupContext = errors.New("mls: group context supplied for a sender type that forbids it")

	// ErrWireFormatMismatch is returned when the wire format a message is being sealed under
	// disagrees with the structure being sealed. The wire format is inside the signature
	// preimage, so the two cannot be allowed to drift.
	ErrWireFormatMismatch = errors.New("mls: wire format does not match the message being sealed")

	// ErrSenderNotMember is returned where the protocol admits only a member sender -- a
	// membership tag covers a member's message and nothing else.
	ErrSenderNotMember = errors.New("mls: sender type must be member")

	// ErrInvalidPaddingSize is returned for a negative padding size. PaddingSizeV1 is zero
	// because connect/message already pads its ciphertext body to a size bucket, and padding
	// inside padding buys nothing; a negative one is a caller error rather than a policy.
	ErrInvalidPaddingSize = errors.New("mls: negative padding size")

	// ErrUnknownProposalOrRefType is the ProposalOrRef discriminant's refusal. No sibling
	// plan names it, so it stays here rather than moving to the shared catalogue.
	ErrUnknownProposalOrRefType = errors.New("mls: unknown ProposalOrRef type")
)
