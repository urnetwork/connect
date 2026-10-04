// The RFC 9420 section 17 IANA registries that the key schedule's GroupContext is
// built out of, and the extensions<V> vector codec that closes it.
//
// This file belongs to the TreeKEM plan (p5 task 3), which the interface registry
// section 11 names as the owner of every registry enum: three plans declared
// ProtocolVersion and its constants and one Go package cannot compile two of them.
// It lands here because p4 task 3 consumes the whole set — GroupContext's first
// field is a ProtocolVersion and its last is an extensions vector — and p5 has not
// landed yet, so the alternative was a second, differently shaped copy that would
// have to be unpicked later. Only the declarations GroupContext needs are here.
// Capabilities, RequiredCapabilities, Credential and the credential and proposal
// registries stay p5's, and when p5 task 3 lands it extends this file rather than
// writing its own.
//
// p5 task 3 has now landed and done exactly that. The proposal and credential registries,
// Capabilities, RequiredCapabilities and the extensions lookup are below, beside the two
// registries p4 needed first. Credential itself is still owed, by p5 task 4A, and is the one
// declaration named in the paragraph above that this file does not yet carry.
//
// None of these registries is a closed set. A GREASE value, or a code point
// registered after this was written, has to parse and be carried unchanged rather
// than error: refusing an unknown extension type at the codec layer would make the
// decoder the thing that decides policy, and policy belongs to validation.
package mls

import (
	"errors"
	"fmt"

	"github.com/urnetwork/connect/mls/syntax"
)

// ProtocolVersion is a code point from the RFC 9420 section 17.1 protocol version
// registry. It is the first field of GroupContext and so of every epoch binding.
type ProtocolVersion uint16

const ProtocolVersionMls10 ProtocolVersion = 0x0001

// CredentialType is a code point from the RFC 9420 section 17.4 credential type
// registry. Only basic is declared because only basic is a credential this profile
// constructs; x509 and every later registration still have to PARSE and be carried, which
// is why the type is a bare uint16 and not an enum with a closed switch behind it.
type CredentialType uint16

const CredentialTypeBasic CredentialType = 0x0001

// ProposalType is a code point from the RFC 9420 section 17.5 proposal type registry.
//
// All eight are declared, including the ones the v1 profile refuses. A refusal you cannot
// name is a refusal you cannot make correctly: ValSem106 and ValSem109 are stated over a
// member's proposal capabilities, and a proposals<V> vector naming external_init has to be
// decoded and compared before anything can decide it is not permitted here.
type ProposalType uint16

const (
	ProposalTypeReserved               ProposalType = 0x0000
	ProposalTypeAdd                    ProposalType = 0x0001
	ProposalTypeUpdate                 ProposalType = 0x0002
	ProposalTypeRemove                 ProposalType = 0x0003
	ProposalTypePreSharedKey           ProposalType = 0x0004
	ProposalTypeReInit                 ProposalType = 0x0005
	ProposalTypeExternalInit           ProposalType = 0x0006
	ProposalTypeGroupContextExtensions ProposalType = 0x0007
)

// ExtensionType is a code point from the RFC 9420 section 17.3 extension type
// registry. The three 0xF00x values are this project's own, from the private use
// range, and are declared beside the registered ones because they travel in the
// same vector and are subject to the same rules.
type ExtensionType uint16

// All five of RFC 9420's initial registrations are declared, including the two this profile
// never constructs, and the reason is the defect this block carried for three tasks: it named
// external_senders at 0x0004, which is external_pub's code point, and the only thing holding
// the value was a hand transcribed table written from the same misreading. A code point with
// no name is a code point nothing can cross check. With all five named, section 7.2's default
// list is transcribed as NAMES and the declared constants are held against it, which is a
// check the swapped pair fails; see
// TestEveryRfc9420DefaultExtensionTypeIsDeclaredAtTheCodePointItAssigns.
const (
	ExtensionTypeApplicationId           ExtensionType = 0x0001
	ExtensionTypeRatchetTree             ExtensionType = 0x0002
	ExtensionTypeRequiredCapabilities    ExtensionType = 0x0003
	ExtensionTypeExternalPub             ExtensionType = 0x0004
	ExtensionTypeExternalSenders         ExtensionType = 0x0005
	ExtensionTypeUrmessageGroupPolicy    ExtensionType = 0xF001
	ExtensionTypeUrmessageLeafKeys       ExtensionType = 0xF002
	ExtensionTypeUrmessageOwnerSuccessor ExtensionType = 0xF003
)

// Extension is one entry of an extensions vector: a registry code point and an
// opaque body whose meaning the code point selects.
type Extension struct {
	ExtensionType ExtensionType
	ExtensionData []byte
}

// MarshalMLS encodes the entry as the RFC 9420 section 6.3.1 struct: a uint16 type
// then opaque extension_data<V>. The leaf writes are return free and no ops after
// the first failure (C2); the error return exists for a semantic refusal and this
// encoder has none.
func (self *Extension) MarshalMLS(w *syntax.Writer) error {
	w.WriteUint16(uint16(self.ExtensionType))
	w.WriteOpaque(self.ExtensionData)
	return nil
}

// UnmarshalMLS decodes one entry, consuming exactly its own two fields, because it
// runs inside a vector region whose remaining bytes belong to the entries after it.
// An unregistered type is accepted and carried: the codec does not decide policy.
func (self *Extension) UnmarshalMLS(r *syntax.Reader) error {
	extensionType, err := r.ReadUint16()
	if err != nil {
		return err
	}
	data, err := r.ReadOpaque()
	if err != nil {
		return err
	}
	self.ExtensionType = ExtensionType(extensionType)
	self.ExtensionData = data
	return nil
}

// the C1 pin: drift between this type and the one codec convention fails at build.
var _ syntax.Codec = (*Extension)(nil)

// WriteExtensions encodes extensions<V> inline into a writer the caller owns.
//
// extensions<V> is never a standalone message — it is always the last field of a
// GroupContext, LeafNode, KeyPackage, GroupInfo or ReInit — so the pair is writer
// and reader taking rather than byte returning, and the length prefix comes from
// syntax.WriteVector rather than from a hand written WriteOpaque at each of those
// five call sites. That prefix counts BYTES and not elements, which is the single
// easiest thing in an MLS codec to get wrong, and writing it once is how it stays
// got right.
func WriteExtensions(w *syntax.Writer, exts []Extension) error {
	return syntax.WriteVector(w, exts, writeOneExtension)
}

// writeOneExtension is WriteVector's element encoder, named rather than written as a
// closure at the call site. The codec entry gate in crypto_labels_test.go pins every
// syntax call this package makes by its source text, and a closure carries its whole
// body into that pin, so a named function keeps the pin readable and keeps an edit to
// the body from reading as a new way of entering the codec.
func writeOneExtension(w *syntax.Writer, ext Extension) error {
	return ext.MarshalMLS(w)
}

// ReadExtensions decodes extensions<V> from a reader the caller owns, consuming
// exactly the declared region. The result is never nil, so an empty vector and an
// absent one stay distinguishable in Go even though the wire has one spelling for
// both.
func ReadExtensions(r *syntax.Reader) ([]Extension, error) {
	return syntax.ReadVector(r, readOneExtension)
}

// readOneExtension is ReadVector's element decoder, named for the reason its encode
// twin is.
func readOneExtension(r *syntax.Reader) (Extension, error) {
	var ext Extension
	err := ext.UnmarshalMLS(r)
	return ext, err
}

// errMissingRequiredCapability is ValSem106 and ValSem109 in the validation plan's catalogue,
// and that plan owns the single declaration site for ErrMissingRequiredCapability. Neither
// that name nor ValSem itself has landed in this package yet, so the refusal is carried by
// this unexported value until they do.
//
// Unexported is the whole point of the shape, and the argument is psk.go's, made for the three
// ValSem401 to ValSem403 sentinels it carries the same way. An exported
// ErrMissingRequiredCapability declared here would be a second public declaration site for a
// name the validation plan also declares, the two would not be the same value, and a caller
// matching one would silently stop matching the other. A name that cannot be reached from
// outside this package cannot be depended on from outside it either, so the swap costs nobody
// else anything -- and every consumer of this refusal in the tasks after this one is inside
// package mls.
//
// It would also be an exported error in a file that no class in
// TestEveryExportedErrorOfThisPackageIsInAMaintainedClass holds, which is that gate's loud
// case -- but that is the right answer to the wrong question. The reason not to export it is
// that the name is somebody else's, not that a sweep would notice.
//
// The swap is mechanical: wrap each detail in ValSem(ValSem106, ...) or ValSem(ValSem109, ...)
// with the catalogue's sentinel as the detail. The moment it is owed is not left to anybody's
// memory either -- TestNoValidationOwnedNameHasLandedBesideItsStandIn derives the owed pair
// from this file's own declarations and fails on the commit that lands the real name.
var errMissingRequiredCapability = errors.New("mls: leaf does not support a required capability")

// FindExtensionEntry returns the one entry carrying t, whether the vector carried one, and a
// refusal if it carried more than one.
//
// This is the package's only lookup of an extension BY TYPE, and it is the door that refuses a
// repeated type. The position is the package's rather than this function's: RFC 9420 forbids two
// entries of one type in an extensions vector, and nothing else in this build refuses one.
// ValSem209 is named by the validation plan and implemented nowhere, and LeafNode.Validate --
// the door the comments here used to hand the refusal to -- range checks EVERY entry it finds
// and accepts a leaf carrying two well formed ones on purpose, because applying a rule to every
// entry is a different rule from refusing the repeat and this package needs both.
//
// So a lookup that answered one of two would be the whole decision, and none of the three things
// it decides can be decided by iteration order. Which entry is reached picks the group's wrap
// target; picks the role list a member enforces, off a list the confirmed transcript hash covers
// so both role sets are equally agreed to; and picks the required_capabilities this client holds
// every leaf against. The last of those is picked by a PEER, because reconcileRequiredCapabilities
// reads it off a *GroupContext that came off the wire through the exported ValidateAgainstContext
// -- the validation entry point whose job is to refuse exactly that. Two members reading two
// different entries agree on every hash and disagree about the group. Refusing is the only answer
// with no arbitrary half in it, and it is also what makes the walk direction stop mattering: at
// most one entry of a type is ever answered, so first and last are the same entry.
//
// When ValSem209 lands it refuses this input EARLIER, at whole group context validation, and this
// refusal becomes a second line of defence rather than the only one. It does not become wrong,
// and it must not be deleted on the strength of ValSem209 arriving: the accessors below reach
// this lookup from paths ValSem209 does not sit in front of -- a LeafNode read out of a Welcome,
// a KeyPackage validated on its own -- so the earlier rule subsumes some of these calls and not
// all of them.
//
// The repeat is refused as an ERROR and not as "not found", and that is what the third result
// exists for. Absence is a legal answer that callers ACT on -- reconcileRequiredCapabilities
// reads an absent required_capabilities as "this group requires nothing" and accepts -- so a
// repeat reported as absence would be ACCEPTED there rather than refused, which is the fail open
// this whole rule is against.
//
// The whole vector is walked before anything is answered, because the refusal is about the
// VECTOR rather than about either entry: a scan that returned at the first match cannot see the
// second, so it cannot refuse the pair.
//
// The refusal names the two POSITIONS and carries no other digits, which is a convention the
// tests read rather than an ornament. groupPolicyPositionsNamedBy pulls every digit run out of
// the message and compares it against the positions the vector holds, because with the repeat
// refused the ORDER the two are named in is the last observable a walk in the wrong direction
// changes. A %#04x extension type spelled into this message would put digits in it that are not
// positions. The type is named by the caller instead, which is also where the human name for it
// is: this lookup has a uint16 and LeafKeysOf has "urmessage_leaf_keys".
//
// ErrMalformedExtension is the sentinel because it is the one the group lifecycle asks this
// question with, and because it is already what both accessors carry for this same repeat.
//
// What comes back is a VIEW of the caller's entry and not a copy of it, so its body shares
// storage with the vector the caller handed in and changes when that vector changes. That is the
// right default for a lookup -- copying every body on the way past charges every caller for the
// one that keeps its answer -- but it means the []byte inside is not safe to hold across a
// mutation of the leaf it was read out of. The copy happens one step later: every
// ParseXExtension of this package copies what it keeps out of the bytes it was given, so the
// PARSED structure is the thing that is safe to hold.
// TestAWrapTargetReadOffALeafSharesNoStorageWithTheLeafsBytes states that over the whole
// documented path rather than over the parse alone, because the doc on LeafKeysExtension sends
// its reader through this lookup first.
func FindExtensionEntry(exts []Extension, t ExtensionType) (Extension, bool, error) {
	// the whole vector before anything is answered: a scan that returned at the first match
	// cannot see the second, so it cannot refuse the pair.
	found := -1
	for i := range exts {
		if exts[i].ExtensionType != t {
			continue
		}
		if found >= 0 {
			return Extension{}, false, fmt.Errorf(
				"%w: the extensions vector carries one type at entry %d and again at entry %d",
				ErrMalformedExtension, found, i)
		}
		found = i
	}
	if found < 0 {
		return Extension{}, false, nil
	}
	return exts[found], true, nil
}

// FindExtension returns the body of the entry carrying t, whether one was found, and the refusal
// FindExtensionEntry makes over a vector carrying two.
//
// A wrapper over that lookup and not a walk of its own, which is the whole of what keeps the two
// answers the same. A hand rolled walk written beside this one is a second selection rule, and a
// second selection rule is how a fourth accessor comes to answer by position while these two
// refuse; TestEveryExtensionTypeSelectionOfBothPackagesIsClassifiedHere is what fails on the
// commit that writes one, and it finds them by what they do rather than by their names.
//
// The bool is separate from a nil body because an extension present with an empty body is a
// different statement from an extension that is absent: required_capabilities carrying three
// empty vectors requires nothing of a member, and no required_capabilities at all is what a
// group that never set one looks like. Those are the same nil in Go and different bytes on the
// wire, and the wire is what everything downstream is signed over.
//
// A refusal answers false as well as the error, so a caller that read only the bool and dropped
// the error would be left holding "absent" rather than a body picked out of an illegal vector.
// That is the safe half of a mistake nobody should make; it is not why the error is here, since
// absence and a repeat are different inputs and every caller in this package acts on them
// differently.
func FindExtension(exts []Extension, t ExtensionType) ([]byte, bool, error) {
	entry, found, err := FindExtensionEntry(exts, t)
	if err != nil || !found {
		return nil, false, err
	}
	return entry.ExtensionData, true, nil
}

// The four uint16 registries all encode as one length prefixed vector of uint16, so the pair
// below is written once over ~uint16 rather than eight times by hand. Eight hand written
// copies is eight chances to write the prefix as an element count instead of the byte count
// syntax.WriteVector writes; the two are the same number only for a fixed width element, which
// these happen to be, so seven of the eight could be wrong in a way the eighth's test would
// never see.
func writeUint16Vec[T ~uint16](w *syntax.Writer, values []T) error {
	return syntax.WriteVector(w, values, writeOneUint16[T])
}

// writeOneUint16 is WriteVector's element encoder, named rather than written as a closure for
// the reason writeOneExtension is: the codec entry gate in crypto_labels_test.go pins every
// syntax call this package makes by its source text, and a closure carries its whole body into
// that pin.
func writeOneUint16[T ~uint16](w *syntax.Writer, v T) error {
	w.WriteUint16(uint16(v))
	return nil
}

func readUint16Vec[T ~uint16](r *syntax.Reader) ([]T, error) {
	return syntax.ReadVector(r, readOneUint16[T])
}

// readOneUint16 is ReadVector's element decoder, named for the reason its encode twin is. An
// unregistered code point is converted and carried, never refused: the codec does not decide
// policy, and a GREASE value in a peer's capabilities is one that has to survive being read
// and written back, or every signature over the leaf carrying it stops verifying here and
// nowhere else.
func readOneUint16[T ~uint16](r *syntax.Reader) (T, error) {
	v, err := r.ReadUint16()
	return T(v), err
}

// Capabilities is RFC 9420 section 7.2: what the client behind a leaf node understands.
//
// It is a validation surface rather than a record. Every field is read to answer one of two
// questions -- may this member join, and is this commit acceptable -- and both are wrong in a
// way nobody sees at the time if a comparison is permissive in the wrong direction. A check
// that accepts a member missing a required extension admits a member who cannot process the
// group's messages; a check that rejects a member who has everything is a group nobody can
// join. Neither surfaces as an error at the point of the mistake, which is why the predicates
// below are tested in both directions over a class derived from the type.
type Capabilities struct {
	Versions     []ProtocolVersion
	CipherSuites []CipherSuite
	Extensions   []ExtensionType
	Proposals    []ProposalType
	Credentials  []CredentialType
}

// MarshalMLS encodes the five vectors in the section 7.2 field order, inline, into a writer
// the caller owns: Capabilities is never a standalone message, it is the fourth field of a
// LeafNode, and the LeafNode's signature covers these bytes where they sit.
//
// The field order is essentially the whole content of this encoding, since all five fields
// have the same shape. Two of them swapped round trips perfectly, agrees with itself and
// disagrees with every other implementation, so what holds it is the golden in the test file
// taken from another implementation's output rather than any symmetry property.
func (self *Capabilities) MarshalMLS(w *syntax.Writer) error {
	if err := writeUint16Vec(w, self.Versions); err != nil {
		return err
	}
	if err := writeUint16Vec(w, self.CipherSuites); err != nil {
		return err
	}
	if err := writeUint16Vec(w, self.Extensions); err != nil {
		return err
	}
	if err := writeUint16Vec(w, self.Proposals); err != nil {
		return err
	}
	return writeUint16Vec(w, self.Credentials)
}

// UnmarshalMLS decodes the five vectors, consuming exactly its own fields, because it runs
// inside a LeafNode whose remaining bytes are that leaf's source, extensions and signature.
//
// Each field is assigned as it is read rather than all at the end, which is the opposite of
// what GroupContext does and is safe for the reason GroupContext's is not: these are five
// independent slices, the caller is handed the error, and there is no composite of old and new
// fields here that describes a leaf which never existed. What a partly filled Capabilities
// describes is a member supporting fewer things, and every predicate below answers false for
// what is missing.
func (self *Capabilities) UnmarshalMLS(r *syntax.Reader) error {
	var err error
	if self.Versions, err = readUint16Vec[ProtocolVersion](r); err != nil {
		return err
	}
	if self.CipherSuites, err = readUint16Vec[CipherSuite](r); err != nil {
		return err
	}
	if self.Extensions, err = readUint16Vec[ExtensionType](r); err != nil {
		return err
	}
	if self.Proposals, err = readUint16Vec[ProposalType](r); err != nil {
		return err
	}
	self.Credentials, err = readUint16Vec[CredentialType](r)
	return err
}

// the C1 pin: drift between this type and the one codec convention fails at build.
var _ syntax.Codec = (*Capabilities)(nil)

// SupportsVersion reports whether the leaf listed this protocol version.
func (self *Capabilities) SupportsVersion(v ProtocolVersion) bool {
	for _, x := range self.Versions {
		if x == v {
			return true
		}
	}
	return false
}

// SupportsCipherSuite reports whether the leaf listed this ciphersuite.
func (self *Capabilities) SupportsCipherSuite(s CipherSuite) bool {
	for _, x := range self.CipherSuites {
		if x == s {
			return true
		}
	}
	return false
}

// SupportsExtension reports whether the leaf listed this extension type. It answers for the
// leaf's own extensions and for the group's required ones, which are the same question asked
// from two sides.
func (self *Capabilities) SupportsExtension(t ExtensionType) bool {
	for _, x := range self.Extensions {
		if x == t {
			return true
		}
	}
	return false
}

// SupportsProposal reports whether the leaf listed this proposal type.
func (self *Capabilities) SupportsProposal(t ProposalType) bool {
	for _, x := range self.Proposals {
		if x == t {
			return true
		}
	}
	return false
}

// SupportsCredential reports whether the leaf can act on this credential type.
//
// The basic credential is mandatory to implement (RFC 9420 section 7.2), so it counts as
// supported whether or not the leaf lists it. Every leaf this profile builds carries a
// BasicCredential and several implementations leave 0x0001 out of the vector as redundant, so
// a strict reading here would let a required_capabilities naming credential type 0x0001
// reject members who are, in fact, using exactly that credential.
//
// This is the one place any of the five predicates answers true for something its vector does
// not contain, which is why it is written out here rather than folded into the loop.
func (self *Capabilities) SupportsCredential(t CredentialType) bool {
	if t == CredentialTypeBasic {
		return true
	}
	for _, x := range self.Credentials {
		if x == t {
			return true
		}
	}
	return false
}

// Supports is the whole required_capabilities check in one call, so the group lifecycle plan's
// ValSem106 and ValSem109 sites cannot each spell the three loops differently -- which is how
// one of them comes to be missing a loop, and a missing loop here is a member admitted who
// cannot read what the group sends.
//
// A nil rc is "no requirement" and is satisfied by anything: a group carrying no
// required_capabilities extension requires nothing, and that differs from a group requiring
// nothing only in Go. On the wire the two are one encoding and both accept.
//
// Three loops and no fourth. Section 11.1 states required_capabilities over extension,
// proposal and credential types only; versions and ciphersuites are agreed by the GroupContext
// rather than required of a member, so a fourth loop here would enforce a rule nobody wrote.
// The detail names the code point that failed, because a caller told only that some capability
// is missing has to diff two vectors to find out which.
func (self *Capabilities) Supports(rc *RequiredCapabilities) error {
	if rc == nil {
		return nil
	}
	for _, t := range rc.ExtensionTypes {
		if !self.SupportsExtension(t) {
			return fmt.Errorf("%w: extension type %#04x", errMissingRequiredCapability, uint16(t))
		}
	}
	for _, t := range rc.ProposalTypes {
		if !self.SupportsProposal(t) {
			return fmt.Errorf("%w: proposal type %#04x", errMissingRequiredCapability, uint16(t))
		}
	}
	for _, t := range rc.CredentialTypes {
		if !self.SupportsCredential(t) {
			return fmt.Errorf("%w: credential type %#04x", errMissingRequiredCapability, uint16(t))
		}
	}
	return nil
}

// RequiredCapabilities is the body of the required_capabilities extension, type 0x0003,
// carried in the group context. Every member of the group must support everything it names,
// which is what makes the three vectors a membership rule rather than a hint.
type RequiredCapabilities struct {
	ExtensionTypes  []ExtensionType
	ProposalTypes   []ProposalType
	CredentialTypes []CredentialType
}

// MarshalMLS encodes the three vectors in the section 11.1 field order. These bytes are an
// extension_data<V> inside the group context, so they are covered by the confirmed transcript
// hash and by every epoch secret derived after it: a field order disagreement here is a group
// that splits, rather than a message that fails to parse.
func (self *RequiredCapabilities) MarshalMLS(w *syntax.Writer) error {
	if err := writeUint16Vec(w, self.ExtensionTypes); err != nil {
		return err
	}
	if err := writeUint16Vec(w, self.ProposalTypes); err != nil {
		return err
	}
	return writeUint16Vec(w, self.CredentialTypes)
}

// UnmarshalMLS decodes the three vectors and consumes exactly them. It makes no trailing byte
// check of its own: syntax.Unmarshal is the byte level entry point and is what raises
// ErrTrailingBytes, and a second check here would be a second place for one rule to be spelled
// two ways.
func (self *RequiredCapabilities) UnmarshalMLS(r *syntax.Reader) error {
	var err error
	if self.ExtensionTypes, err = readUint16Vec[ExtensionType](r); err != nil {
		return err
	}
	if self.ProposalTypes, err = readUint16Vec[ProposalType](r); err != nil {
		return err
	}
	self.CredentialTypes, err = readUint16Vec[CredentialType](r)
	return err
}

// the C1 pin: drift between this type and the one codec convention fails at build.
var _ syntax.Codec = (*RequiredCapabilities)(nil)

// AlgIdXwing is the wrap KEM code point urmessage_leaf_keys carries: 0x0014, X-Wing
// (X25519 + ML-KEM-768), MASTER section 7.1. It is the only one this profile implements.
//
// 0x0012 and 0x0013 are registered in that section for other hybrids and are unimplemented in
// v1, so both are refused rather than carried, on the encode side and the decode side alike.
// That is the opposite of what this file does for every registry above it, and the difference
// is what the code point is FOR. An unknown extension type is carried because something else
// decides what to do with it; an alg_id names the KEM a sender must wrap a commit secret to,
// so a leaf carrying one this package cannot wrap to is a device nothing can ever send to.
// Refusing it at the codec makes that a parse failure at the leaf, which names the leaf, and
// not an unexplained failure at the first Commit after it joined.
const AlgIdXwing uint16 = 0x0014

// XwingPublicKeyLen is the X-Wing encapsulation key size: 1216 bytes, being the 1184 byte
// ML-KEM-768 encapsulation key followed by the 32 byte X25519 public key.
//
// draft-connolly-cfrg-xwing-kem-06 section 5.1, "Encoding and sizes", gives the number
// directly, and Spec A A-ASSUME-3 pins this project to that draft. It is checked against the
// standard library rather than against either document by
// TestXwingPublicKeyLenIsTheMlKem768AndX25519KeySizesAdded, because both documents are prose
// this package could copy a digit wrong out of and neither fails when it does.
//
// It is duplicated in ../messagegroup on purpose and in one direction only, because mls must not
// import it. That copy landed with p2 tasks 19 and 20 as message.XwingPublicKeySize -- in
// connect/message, which the 2026-09-06 split then divided, sending the X-Wing half to
// connect/messagegroup because it was the only thing giving connect/message an import of this
// package. It carries its own pin: messagegroup/xwing.go declares the difference between the two
// constants as an array length in BOTH directions, so a tree in which they disagree does not
// build. The same pair of lines pins messagegroup.XwingAlgId to AlgIdXwing above, one
// registry over and was under nothing until then. p2 task 22 adds the test that names the first
// of those two pins; the compile assertion is what actually holds it, and it holds without any
// test running at all.
//
// The derivation above is stated here anyway, against crypto/mlkem and crypto/ecdh, because a
// compile assertion says the two copies AGREE and says nothing about whether either is right.
//
// The pin is not left to anybody's memory either.
// TestNoXwingNamedDeclarationLandsInEitherPackageWithoutBeingClassifiedHere derives every
// X-Wing named declaration of this package and of ../messagegroup and fails on the commit that lands
// a third statement of this size, which is the commit where a third pin would have to be
// written.
const XwingPublicKeyLen = 1216

// LeafKeysExtension is the body of urmessage_leaf_keys, extension type 0xF002, MASTER
// section 5.3:
//
//	struct {
//	    uint16 alg_id;                  // 0x0014, X-Wing
//	    opaque device_xwing_pub<V>;     // XwingPublicKeyLen bytes
//	} UrmessageLeafKeys;
//
// It rides in the LeafNode, so it is covered by the leaf signature and by the tree hash, it is
// validated by RFC 9420 section 7.3 along with the rest of the leaf, and Remove takes it away
// with the leaf rather than leaving a stale wrap target behind.
//
// FOR THE READER ARRIVING WITH wrap.go IN HAND. This is where a device's X-Wing wrap target
// comes from, and there are four things to know before reading one off a leaf:
//
//   - Find the ENTRY with FindExtensionEntry(leaf.Extensions, ExtensionTypeUrmessageLeafKeys)
//     and hand the whole Extension to ParseLeafKeysFrom. Not its body to
//     ParseLeafKeysExtension: ParseLeafKeysFrom is the only one of the two that is given the
//     tag and so the only one that can check it. FindExtensionEntry answers the ONE entry of
//     that type and REFUSES a vector carrying two, so found does mean unique -- the repeat is
//     refused at the lookup because nothing else in this build refuses it, and that lookup's
//     own doc is where the argument for that position is written out.
//   - A parsed DeviceXwingPub is a fresh copy, never a view into the leaf's bytes, so wrapping
//     to it cannot be perturbed by whoever owns that buffer. The reverse also holds: a
//     DeviceXwingPub handed to Encode is copied into the extension body, so mutating that
//     slice afterwards does not change an Extension already produced. The LOOKUP in front of
//     the parse copies nothing, though -- it answers a view of the caller's vector -- so what
//     is safe to hold is the parsed structure and not the []byte it came from.
//   - Every value that comes back has already been refused unless alg_id is AlgIdXwing and
//     len(DeviceXwingPub) is exactly XwingPublicKeyLen. Neither needs re-checking, and a
//     length check written against a different constant is how the two come to drift. What is
//     still owed is the KEM's own validation of the key: a 1216 byte string is not necessarily
//     a well formed X-Wing encapsulation key.
//   - The tag and the body are paired by this package on both sides, and neither pairing is
//     enforced by the type system. Extension is a plain struct with two exported fields, so
//     ext.ExtensionData is one field access away and
//     Extension{ExtensionTypeUrmessageGroupPolicy, thatBody} is a composite literal built out
//     of exported API alone -- it encodes, it signs, it travels, and it decodes at the far end
//     as a group policy nobody can read. No result type prevents that. What this package
//     promises is narrower and is what is actually worth relying on: it never HANDS OUT a
//     loose body, since Encode answers the tag and the body already assembled and
//     TestNoExportedSymbolOfThisPackageHandsOutAnExtensionBodyOnItsOwn keeps a later
//     convenience from adding one; and it refuses to READ a body back under any other tag,
//     which is ParseLeafKeysFrom. Pull an untagged body out of an Extension yourself and both
//     promises are off, because ParseLeafKeysExtension parses whatever it is handed and never
//     sees a tag at all.
//
// Extension.ExtensionData is opaque, so a concrete extension body converts bytes to and from a
// struct rather than implementing MarshalMLS and UnmarshalMLS. That is one of the two
// sanctioned exceptions to convention C1, and the sanctioned spelling is exactly this pair:
// Encode returning an Extension, and ParseXExtension taking the bytes.
// TestNoTypeOfThisPackageCarriesAByteLevelCodecOfItsOwn derives the exception from the Encode
// method's own signature rather than exempting this file, so a second extension body written
// to this shape is sanctioned by being written to it, and one written to any other shape is
// not.
type LeafKeysExtension struct {
	AlgId          uint16
	DeviceXwingPub []byte
}

// Encode returns the whole Extension: the 0xF002 tag and the encoded body together.
//
// Returning the Extension rather than the body's bytes is what this shape exists to give, and
// the reason is that the alternative is reachable by ACCIDENT. A body returned as a byte slice
// is a value a caller pairs with a tag of its own choosing, and that choice is one identifier
// away from ExtensionTypeUrmessageGroupPolicy -- which encodes, signs and travels, and is
// refused by the first peer that tries to read a group policy out of an X-Wing key. Handing
// back the pair already assembled keeps that mistake off the path of a caller doing the
// ordinary thing, and TestNoExportedSymbolOfThisPackageHandsOutAnExtensionBodyOnItsOwn keeps a
// later convenience from putting it back on that path.
//
// It is not, and cannot be, a guarantee that the two never come apart. Extension's fields are
// exported because the codec needs them to be, so the loose body is a field access and the
// mismatched pair is a three line composite literal; a result type cannot prevent that and
// this one does not. What closes the loop is the read side refusing what the write side would
// never produce: ParseLeafKeysFrom is handed the whole Extension and refuses every tag but
// this one, and TestEveryExtensionBodyRefusesAnEntryCarryingAnyTagButItsOwn holds it to that
// over the whole uint16 space rather than over the neighbours somebody thought of.
//
// Both refusals are ErrLeafKeysExtensionInvalid and both are made before anything is written,
// so a refused body never reaches a Writer and can never be half encoded into one the caller
// shares.
func (self *LeafKeysExtension) Encode() (Extension, error) {
	if self.AlgId != AlgIdXwing {
		return Extension{}, fmt.Errorf("%w: alg_id %#04x is not X-Wing", ErrLeafKeysExtensionInvalid, self.AlgId)
	}
	if len(self.DeviceXwingPub) != XwingPublicKeyLen {
		return Extension{}, fmt.Errorf("%w: device_xwing_pub is %d bytes, want %d",
			ErrLeafKeysExtensionInvalid, len(self.DeviceXwingPub), XwingPublicKeyLen)
	}
	body, err := marshalBytes(func(w *syntax.Writer) error {
		w.WriteUint16(self.AlgId)
		w.WriteOpaque(self.DeviceXwingPub)
		return nil
	})
	if err != nil {
		return Extension{}, err
	}
	return Extension{
		ExtensionType: ExtensionTypeUrmessageLeafKeys,
		ExtensionData: body,
	}, nil
}

// ParseLeafKeysExtension decodes an urmessage_leaf_keys body: the bytes of one entry's
// ExtensionData, not the entry itself.
//
// It consumes them in full. An extension body is the last thing on this path that a length
// prefix delimits, so a decoder here that ignored a tail would accept two encodings of one
// leaf's wrap target -- and the leaf signature and the tree hash are taken over the bytes,
// which makes two accepted spellings two groups that agree they are one.
//
// The alg_id and length refusals come after the full consumption check rather than before it,
// so a body that is both malformed and unimplemented is reported as malformed. That ordering
// is deliberate: ErrTrailingBytes says the sender and this package disagree about the
// encoding, ErrLeafKeysExtensionInvalid says they agree about it and this profile cannot act
// on what it says, and the second is only meaningful once the first has been ruled out.
//
// It is handed the body and not the entry, so it never sees the extension type those bytes
// arrived under and cannot refuse a wrong one: a urmessage_group_policy entry whose body
// happens to be a well formed leaf keys body parses cleanly here and answers a wrap target.
// That is what ParseLeafKeysFrom is for, and why the briefing on LeafKeysExtension sends its
// reader there instead. This entry point stays exported because the decode side's own two
// refusals have to be reachable from a body a PEER sent -- which arrives as bytes, from a
// profile this one does not implement, and not as an Extension this package built.
func ParseLeafKeysExtension(data []byte) (*LeafKeysExtension, error) {
	r := syntax.NewReader(data)
	algId, err := r.ReadUint16()
	if err != nil {
		return nil, err
	}
	pub, err := r.ReadOpaque()
	if err != nil {
		return nil, err
	}
	if err := r.Done(); err != nil {
		return nil, err
	}
	if algId != AlgIdXwing {
		return nil, fmt.Errorf("%w: alg_id %#04x is not X-Wing", ErrLeafKeysExtensionInvalid, algId)
	}
	if len(pub) != XwingPublicKeyLen {
		return nil, fmt.Errorf("%w: device_xwing_pub is %d bytes, want %d",
			ErrLeafKeysExtensionInvalid, len(pub), XwingPublicKeyLen)
	}
	return &LeafKeysExtension{AlgId: algId, DeviceXwingPub: pub}, nil
}

// ParseLeafKeysFrom decodes one extensions<V> entry as an urmessage_leaf_keys body, refusing
// any entry that is not tagged ExtensionTypeUrmessageLeafKeys.
//
// This is the read side counterpart to Encode, and the pair is the whole of what this package
// can say about the tag: Encode never emits a body without its own tag on it, and this never
// accepts a body under anybody else's. Neither half is enforced by the type system -- Extension
// carries two exported fields and a caller can assemble any pair it likes -- so what the pair
// buys is that the mistake cannot be made by a caller using the package as documented, and
// cannot survive being read back by one.
//
// ParseLeafKeysExtension is the half with no tag to check, and a caller who reaches for it with
// bytes it pulled out of an Extension itself has stepped outside both halves. What that
// produces is not a parse error anywhere: it is a wrap target read out of whatever extension
// happened to be sitting in that slot, and a commit secret wrapped to it goes to nobody.
//
// The refusal is ErrLeafKeysExtensionInvalid, the same sentinel the body's own two refusals
// carry, because every caller here is asking one question -- is there a leaf keys body I can
// act on -- and an entry of the wrong type answers it exactly as an alg_id this profile cannot
// wrap to does. The detail names the type that was found, since a caller told only that the
// entry was wrong has to go and look.
func ParseLeafKeysFrom(ext Extension) (*LeafKeysExtension, error) {
	if ext.ExtensionType != ExtensionTypeUrmessageLeafKeys {
		return nil, fmt.Errorf("%w: extension type %#04x is not urmessage_leaf_keys",
			ErrLeafKeysExtensionInvalid, uint16(ext.ExtensionType))
	}
	return ParseLeafKeysExtension(ext.ExtensionData)
}
