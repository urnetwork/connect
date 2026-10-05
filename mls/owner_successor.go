// The urmessage_owner_successor group context extension, MASTER section 11 and spec A section
// 3.4: the nomination that lets a group survive its owner.
//
// It lives in the GROUP CONTEXT rather than in a leaf, for group_policy.go's reason: the context
// is inside the confirmed transcript hash, so a nomination is covered by every confirmation tag
// the group has ever produced and no server can add one, alter one or drop one without every
// member's transcript diverging from it. A nomination a server could plant is a takeover.
//
// It is accepted and validated by every v1 client and is deliberately NOT in
// required_capabilities (spec A section 3.4): requiring it would exclude a member for a
// governance feature its group may never enable.
//
// WHAT IS HERE AND WHAT IS NOT. MASTER section 11 and spec A section 3.4 make five conditions on
// the promotion of a nominated successor, and errors_lifecycle.go declares one sentinel for each
// -- ErrSuccessionDisabled, ErrSuccessionNotNominee, ErrSuccessionQuorum, ErrSuccessionFloor and
// ErrSuccessionFloorTooShort. All five are conditions on a promotion COMMIT and all five are
// succession.go's, which p7 task 21 writes; this file must not answer any of them on the read
// side, because a refusal returned from the wrong place is a rule that two doors enforce
// differently.
//
// ErrSuccessionFloorTooShort READS ONLY THE NOMINATION, and that is exactly why it was tempting
// to answer it here, at the codec, in both directions. It was, and that was wrong three ways.
// (a) It made spec A section 3.4's condition 5 unreachable on every production path: a nomination
// with a short floor never survives a decode, so the branch that names it in succession.go can
// only ever fire for a struct built in memory. (b) It PRE-EMPTED condition 1. Section 3.4 puts
// the owner's opt out first and states that it is absolute; a nomination that is both
// Enabled=false and short-floored was answered ErrSuccessionFloorTooShort and never reached the
// opt out at all, so the one condition the owner set by hand lost to one they may never have
// looked at. (c) It made the group's own context undecodable: the nomination is deliberately not
// in required_capabilities, so a peer can commit one, and a v1 client that refuses to DECODE a
// short floor cannot read the group context it is a member of -- a refusal about succession
// returned to a caller doing something else entirely.
//
// So this file states the two rules on the nomination in Validate and calls it from Encode, the
// construction door. Nothing on the read side judges. See the note on UnmarshalMLS for the
// package rule that follows from it.
//
// ExtensionTypeUrmessageOwnerSuccessor (0xF003) is declared once with the other registry enums,
// in extension.go, and is checked against the interface registry and spec A there rather than
// restated here.
package mls

import (
	"fmt"

	"github.com/urnetwork/connect/mls/syntax"
)

// SuccessionFloorMinMs is ninety days, MASTER section 11 and spec A section 3.4.
//
// A nomination carrying a shorter floor is one this build never WRITES -- Encode refuses it --
// and never ACTS ON: spec A section 3.4's condition 5 refuses the promotion it would authorise.
// Those are the two directions that matter, and neither of them is the decode. A group therefore
// cannot shorten its own succession delay after the fact and cannot be handed a shortened one by
// a peer, while a peer that commits one is refused at the promotion rather than at the octets --
// which is the difference between a nomination this client will not act on and a group context
// this client cannot read. The floor is what makes the delay a delay: an owner who is merely
// offline for a week has ninety days to come back and say no.
const SuccessionFloorMinMs uint64 = 7776000000

// successionPreimageLabel is the domain separation string of the bytes an admin countersigns,
// spec A section 3.4. It is the label of THIS signature and of no other, which is what stops a
// countersignature being a valid signature over some other structure the same identity key
// signed.
const successionPreimageLabel = "URmessage/v1/succession"

// OwnerSuccessorExtension is the nomination, extension type 0xF003:
//
//	struct {
//	    uint8  enabled;
//	    opaque successor_member_id<V>;
//	    uint64 nominated_at_ms;
//	    uint64 floor_ms;
//	} UrmessageOwnerSuccessor;
//
// SuccessorMemberId is the nominee's Ed25519 identity public key -- the BasicCredential subject,
// the same identity a GroupPolicyExtension role entry names -- and not a leaf index, so a
// nomination survives the nominee replacing every device leaf they hold.
//
// Enabled false disables succession for this group entirely and is the owner's opt out. It is
// carried as a field rather than as the absence of the extension so that the opt out is itself
// inside the transcript hash: a group that never nominated anybody and a group whose owner
// switched succession off are different states, and only the second is one a peer cannot quietly
// undo by dropping an entry.
type OwnerSuccessorExtension struct {
	Enabled           bool
	SuccessorMemberId []byte
	NominatedAtMs     uint64
	FloorMs           uint64
}

// the C1 pin: drift between this type and the one codec convention fails at build.
var _ syntax.Codec = (*OwnerSuccessorExtension)(nil)

// MarshalMLS writes the wire form.
//
// Enabled goes on the wire as a u8 carrying only 0 or 1. A Go bool has no presentation language
// encoding to inherit, and the reason to pin it to two values rather than to "nonzero is true"
// is that this body is inside the group context: two encodings of true would be two group
// contexts, two transcript hashes and two confirmation tags describing one nomination, and a
// peer could hand each half of the group a different one.
//
// It does NOT validate. Encode is the checked entry point, for group_policy.go's reason -- the
// tests that have to produce what a hostile peer would send need an encoder that will write a
// nomination this profile refuses, and a Marshal that validated would leave them hand assembling
// the octets and agreeing with themselves about the layout.
func (self *OwnerSuccessorExtension) MarshalMLS(w *syntax.Writer) error {
	if self.Enabled {
		w.WriteUint8(1)
	} else {
		w.WriteUint8(0)
	}
	w.WriteOpaque(self.SuccessorMemberId)
	w.WriteUint64(self.NominatedAtMs)
	w.WriteUint64(self.FloorMs)
	return nil
}

// UnmarshalMLS reads the wire form and refuses any boolean byte but 0 and 1. It does NOT judge
// the value it read.
//
// THIS IS THE PACKAGE'S RULE AND NOT A LOCAL CHOICE: a codec decodes, it does not judge. What
// UnmarshalMLS refuses is octets that encode no value of this type -- a truncation, and an
// enabled byte of 0x02, which is not a bool in the encoding this type declares. What Validate
// refuses is a value this profile will not act on, and that is a different question asked by a
// different caller. Three things follow from putting the second inside the first, all three of
// which this file had:
//
//   - the declared syntax.Codec stops round tripping. syntax.Marshal(&OwnerSuccessorExtension{})
//     succeeds and syntax.Unmarshal of its own output fails, so the pin two lines above claims a
//     contract the pair does not keep, and every caller that assumes an encode/decode pair is
//     total over the type -- cloneProposal in proposal_list.go is one -- is assuming wrong.
//   - a rule that must run LATER can no longer run at all, because a value refused at the octets
//     never reaches the door that would have ordered it correctly. Spec A section 3.4's opt out
//     is the case: it is condition 1 and the floor is condition 5, and a codec answering the
//     floor decides a nomination the owner switched off before anybody asks whether it is on.
//   - a group context becomes unreadable over a rule about governance. The extension is not in
//     required_capabilities by design, so a peer may commit one this profile refuses, and
//     refusing to DECODE it takes the whole context down with it.
//
// So Validate is called by Encode, the construction door, and by p7 task 21's promotion
// validator, which is the read side door that can ask the five conditions in the order section
// 3.4 states them. group_policy.go answers the same question differently on the read side and
// says why there: its rules are conditions on the STRUCTURE with no later door to own them, so
// they run at the parse entry points. The rule both files share is this one -- the codec never
// judges -- and the difference is only which door does.
//
// The whole value is replaced rather than filled field by field, so a decode that refuses part
// way leaves the receiver as it was rather than holding half of two nominations.
func (self *OwnerSuccessorExtension) UnmarshalMLS(r *syntax.Reader) error {
	enabled, err := r.ReadUint8()
	if err != nil {
		return err
	}
	if enabled > 1 {
		return fmt.Errorf("%w: succession enabled byte is %#02x, which is neither 0 nor 1",
			ErrMalformedExtension, enabled)
	}
	successorMemberId, err := r.ReadOpaque()
	if err != nil {
		return err
	}
	nominatedAtMs, err := r.ReadUint64()
	if err != nil {
		return err
	}
	floorMs, err := r.ReadUint64()
	if err != nil {
		return err
	}
	decoded := OwnerSuccessorExtension{
		Enabled:           enabled == 1,
		SuccessorMemberId: successorMemberId,
		NominatedAtMs:     nominatedAtMs,
		FloorMs:           floorMs,
	}
	*self = decoded
	return nil
}

// Validate applies the two rules that read nothing but the nomination itself.
//
// WHO CALLS IT is the whole of the decision this file records. Encode calls it, so this build
// never writes a nomination it would refuse. Nothing on the READ side calls it, because the two
// rules are conditions on a promotion and spec A section 3.4 orders the promotion's conditions
// with the owner's opt out first -- so the earliest door that can state either of them without
// pre-empting condition 1 is p7 task 21's promotion validator, which asks condition 1 itself and
// reaches this for condition 5 after the other four have passed. A door earlier than that answers
// the floor to a caller who was asking whether succession is even switched on.
//
// The floor rule is stated HERE and only here, so the promotion validator's condition 5 is a call
// rather than a second copy of the comparison. Two statements of one threshold are two things
// that can disagree, and the one that had not been updated would be the one the commit path ran.
//
// The two refusals answer two different sentinels and never one, which is errors_lifecycle.go's
// rule: errors.Is cannot tell two rules apart when they answer the same value, so a test
// asserting the broad question passes over a rule that fired for the wrong reason.
func (self *OwnerSuccessorExtension) Validate() error {
	if self.FloorMs < SuccessionFloorMinMs {
		return fmt.Errorf("%w: floor is %d ms, minimum is %d ms",
			ErrSuccessionFloorTooShort, self.FloorMs, SuccessionFloorMinMs)
	}
	// a nomination time with nobody nominated is a countersignature preimage over an empty
	// successor: the admins would be signing the promotion of no one, and the four commit side
	// conditions would then be judging a claim whose subject the nomination never named
	if len(self.SuccessorMemberId) == 0 && self.NominatedAtMs != 0 {
		return fmt.Errorf("%w: nomination time %d is set with no successor named",
			ErrMalformedExtension, self.NominatedAtMs)
	}
	return nil
}

// Encode serializes to a group context Extension after validating, and answers the whole
// Extension -- the 0xF003 tag and the body together -- rather than the body on its own, for the
// reason GroupPolicyExtension.Encode does: a loose body is a value a caller pairs with a tag of
// its own choosing, and 0xF001 is two identifiers away.
func (self *OwnerSuccessorExtension) Encode() (Extension, error) {
	if err := self.Validate(); err != nil {
		return Extension{}, err
	}
	data, err := syntax.Marshal(self)
	if err != nil {
		return Extension{}, err
	}
	return Extension{ExtensionType: ExtensionTypeUrmessageOwnerSuccessor, ExtensionData: data}, nil
}

// ParseOwnerSuccessorExtension decodes one extension body: the bytes of an entry's
// ExtensionData, not the entry itself.
//
// Bare plumbing, which is what convention C1 asks an entry point to be -- it hands its run to
// the one codec and reports what came back, and adds no second reading of the bytes for the
// codec to disagree with. Every refusal it can make is UnmarshalMLS's, and UnmarshalMLS refuses
// only octets that encode no nomination: this entry point answers whatever nomination the group
// context actually carries, including one whose floor the promotion validator will refuse. See
// the note on Validate for why that is the read side this file wants.
func ParseOwnerSuccessorExtension(data []byte) (*OwnerSuccessorExtension, error) {
	nomination := &OwnerSuccessorExtension{}
	if err := syntax.Unmarshal(data, nomination); err != nil {
		return nil, err
	}
	return nomination, nil
}

// ParseOwnerSuccessorFrom decodes one extensions<V> ENTRY as a nomination body, refusing any
// entry that is not tagged ExtensionTypeUrmessageOwnerSuccessor.
//
// The read side counterpart to Encode, and the reason both exist is the pair rather than either
// half: Encode never emits a body without this tag on it and this never accepts a body under
// anybody else's, so a caller using the package as documented cannot pair a nomination with
// 0xF001. What that would cost is not a message nobody can read -- it is a body that parses
// cleanly under the wrong reader, because a successor id and a server id are both opaque<V> at
// the same offset, and the extension list is inside the confirmed transcript hash.
//
// The refusal is ErrMalformedExtension, the same value this body's content refusals carry,
// because a caller here is asking one question: is there a nomination in this entry I can act on.
func ParseOwnerSuccessorFrom(ext Extension) (*OwnerSuccessorExtension, error) {
	if ext.ExtensionType != ExtensionTypeUrmessageOwnerSuccessor {
		return nil, fmt.Errorf("%w: extension type %#04x is not urmessage_owner_successor",
			ErrMalformedExtension, uint16(ext.ExtensionType))
	}
	return ParseOwnerSuccessorExtension(ext.ExtensionData)
}

// OwnerSuccessorOf finds and parses the nomination in a group context extension list.
//
// ABSENCE IS NOT AN ERROR, and that is what separates this from GroupPolicyOf. Every group this
// profile creates carries a policy, so a context without one is refused with ErrNoGroupPolicy; a
// nomination is optional by design, so a context without one is the ordinary case and is reported
// as a false second result rather than as a refusal every caller has to know to walk past. A
// caller that read absence as an error would make every group that never nominated a successor
// unreadable.
//
// A list carrying 0xF003 twice is refused, and the refusal is the LOOKUP's rather than this
// accessor's: FindExtension is how this package reads an extension by type, and it is one door
// rather than one per accessor precisely so that a fourth accessor cannot be the one that answers
// by position. What answering by position would cost here is a group where half the members
// believe one identity is the successor and half believe another, while every member agrees on
// every transcript hash the group has ever produced.
func OwnerSuccessorOf(exts []Extension) (*OwnerSuccessorExtension, bool, error) {
	entry, found, err := FindExtensionEntry(exts, ExtensionTypeUrmessageOwnerSuccessor)
	if err != nil {
		// the type named here and the positions there, for GroupPolicyOf's reason
		return nil, false, fmt.Errorf("the extension list carries urmessage_owner_successor more than once: %w", err)
	}
	if !found {
		return nil, false, nil
	}
	nomination, err := ParseOwnerSuccessorFrom(entry)
	if err != nil {
		return nil, false, err
	}
	return nomination, true, nil
}

// successionPreimage is the bytes an admin countersigns. MASTER section 11 and spec A section
// 3.4:
//
//	"URmessage/v1/succession" || LP(group_id) || u64(epoch)
//	  || LP(successor_member_id) || u64(nominated_at_ms)
//
// LP(x) is MASTER's notation -- a 32 bit big endian length then x -- and is NOT the presentation
// language's varint header, which is what opaque<V> means and what every MLS structure in this
// package is built out of. This preimage is not an MLS structure: it is a URmessage layer
// signature preimage, and MASTER defines it in the same notation connect/message builds every
// record field and every write_auth preimage with.
//
// syntax.Writer.WriteOpaqueLP is that notation, and it is UNREACHABLE FROM HERE.
// TestNoMlsEncodingReachesTheRecordLayerLengthPrefix forbids every LP suffixed codec method
// inside package mls, deriving the class off the codec's own type and the scope off this
// package's whole non test source, because substituting the record layer prefix for the MLS one
// inside an MLS structure is a one identifier edit that compiles, round trips, and forks the
// group at the first cross implementation join. That gate has no exemption to ask for and should
// not grow one for a judgement call, so the prefix is written out here instead -- ONCE, as a
// closure both fields go through, rather than twice inline, because two copies of a length
// prefix are two things that can disagree.
//
// The bound is the Writer's own MaxVectorLength, which is what WriteOpaqueLP would have applied.
// Without it uint32(len(x)) truncates rather than refuses, and a truncated length is a preimage
// that two different inputs share -- which for a countersignature is an admin who signed a
// promotion they never saw.
//
// The label is written with WriteRaw and carries no length of its own, which is safe only because
// it is a FIXED prefix of every preimage this function makes. A VARIABLE field written without a
// length is what lets two different (group_id, successor_member_id) pairs produce one preimage,
// and both of those carry theirs.
func successionPreimage(groupId []byte, epoch uint64, successorMemberId []byte,
	nominatedAtMs uint64) ([]byte, error) {

	w := syntax.NewWriter()
	lengthPrefixed := func(field []byte) error {
		if len(field) > syntax.MaxVectorLength {
			return fmt.Errorf("%w: succession preimage field is %d octets, the maximum is %d",
				syntax.ErrLengthExceedsMax, len(field), syntax.MaxVectorLength)
		}
		w.WriteUint32(uint32(len(field)))
		w.WriteRaw(field)
		return nil
	}
	w.WriteRaw([]byte(successionPreimageLabel))
	if err := lengthPrefixed(groupId); err != nil {
		return nil, err
	}
	w.WriteUint64(epoch)
	if err := lengthPrefixed(successorMemberId); err != nil {
		return nil, err
	}
	w.WriteUint64(nominatedAtMs)
	return w.Bytes()
}
