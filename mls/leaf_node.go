// The LeafNode of RFC 9420 section 7.2: one device's presence in the ratchet tree, and the
// variant codec that both halves of its signature are taken over.
//
// Everything in this file is field ORDER and prefix WIDTH, and neither is visible from a round
// trip. A pair of fields swapped in both halves round trips perfectly, agrees with itself, and
// disagrees with every other implementation; a field dropped from both halves round trips
// byte exact while being lost. What holds those is the hand derived golden in the test file and
// the field assignment cross check against the vendored vectors, not any symmetry property of
// this code.
package mls

import (
	"errors"
	"fmt"
	"time"

	"github.com/urnetwork/connect/mls/syntax"
)

// LeafNodeSource is RFC 9420 section 7.2: which of the three ways this leaf entered the tree,
// and therefore which variant fields are present and what the signature covers.
//
// It is the discriminant of a `select`, so it decides the LENGTH of the encoding as well as its
// content: a leaf whose source says update carries no lifetime and no parent hash, and the same
// bytes read under key_package would take the sixteen octets of the extensions vector and the
// signature that follow it as a Lifetime.
type LeafNodeSource uint8

const (
	LeafNodeSourceKeyPackage LeafNodeSource = 1
	LeafNodeSourceUpdate     LeafNodeSource = 2
	LeafNodeSourceCommit     LeafNodeSource = 3
)

// Lifetime is present only when the source is key_package. Seconds since the unix epoch.
type Lifetime struct {
	NotBefore uint64
	NotAfter  uint64
}

// LeafNode is RFC 9420 section 7.2: one device's presence in the ratchet tree.
//
// Lifetime and ParentHash are variant fields and are meaningful only under the source that
// carries them. A value holding both is not malformed -- it is what a struct with no sum type
// looks like in Go -- but only the one the source selects is encoded, and a decode leaves the
// others at their zero value. That last part is a property of UnmarshalMLS rather than of the
// switch inside it: the decode is staged and assigned whole, so it holds for a receiver that
// already held a leaf of another source as well as for a fresh one.
//
// A NIL vector and an EMPTY one are the same leaf. Every variable length field here --
// EncryptionKey, SignatureKey, Credential.Identity, the five Capabilities vectors, ParentHash,
// Extensions and Signature -- is written with a length prefix that has one spelling for zero,
// so nil and empty encode to identical bytes, and a decode of those bytes answers the EMPTY
// one: syntax.ReadOpaque allocates and syntax.ReadVector allocates, and neither ever answers
// nil. A leaf built by hand with a nil vector therefore does not survive a round trip under
// reflect.DeepEqual even though its encoding does, which matters wherever two leaves are
// compared as values rather than as bytes. TestANilVectorAndAnEmptyOneAreOneLeafOnTheWire
// states it over every vector of the structure, derived off the type.
type LeafNode struct {
	EncryptionKey  HpkePublicKey
	SignatureKey   SignaturePublicKey
	Credential     Credential
	Capabilities   Capabilities
	LeafNodeSource LeafNodeSource
	Lifetime       Lifetime
	ParentHash     []byte
	Extensions     []Extension
	Signature      []byte
}

// marshalCore writes the fields common to LeafNode and LeafNodeTBS, up to and including
// extensions.
//
// It exists so that task 6's LeafNodeTBS cannot drift from this one. The signature is taken
// over exactly these bytes plus the TBS suffix, so a field written here and not there -- or in
// a different order -- is a signature that verifies against a leaf nobody sent.
func (self *LeafNode) marshalCore(w *syntax.Writer) error {
	w.WriteOpaque(self.EncryptionKey)
	w.WriteOpaque(self.SignatureKey)
	if err := self.Credential.MarshalMLS(w); err != nil {
		return err
	}
	if err := self.Capabilities.MarshalMLS(w); err != nil {
		return err
	}
	w.WriteUint8(uint8(self.LeafNodeSource))
	switch self.LeafNodeSource {
	case LeafNodeSourceKeyPackage:
		w.WriteUint64(self.Lifetime.NotBefore)
		w.WriteUint64(self.Lifetime.NotAfter)
	case LeafNodeSourceUpdate:
		// the empty struct arm. nothing is written, which is the whole of the variant
	case LeafNodeSourceCommit:
		w.WriteOpaque(self.ParentHash)
	default:
		// a semantic refusal rather than a buffer failure, so it reaches the caller as a
		// returned error rather than being dropped into the Writer: an unknown source would
		// otherwise be encoded as its octet and no variant at all, which is a leaf every
		// other implementation reads as a different structure
		return ErrTreeMalformed
	}
	return WriteExtensions(w, self.Extensions)
}

// MarshalMLS encodes the whole leaf: the core, then the signature over the TBS of that core.
func (self *LeafNode) MarshalMLS(w *syntax.Writer) error {
	if err := self.marshalCore(w); err != nil {
		return err
	}
	w.WriteOpaque(self.Signature)
	return nil
}

// unmarshalCore reads the same fields in the same order as marshalCore, so that LeafNodeTBS and
// LeafNode cannot come apart at the one place it would not be noticed.
//
// It writes into its receiver AS IT READS, and the variant arms assign only the field their own
// source carries, so it must be called on a value nobody else holds and that holds nothing:
// UnmarshalMLS below stages a fresh LeafNode for exactly that reason, and task 6's LeafNodeTBS
// owes the same. Called on a receiver that already held a leaf, this leaves the previous value's
// ParentHash or Lifetime standing under a source that does not carry it, and called on a
// receiver whose decode is then refused, it leaves that receiver half written. Both make the
// decoded value depend on something besides the bytes it was read from, which is the assumption
// every comparison of two leaves and every parent hash computed off one rests on.
func (self *LeafNode) unmarshalCore(r *syntax.Reader) error {
	encryptionKey, err := r.ReadOpaque()
	if err != nil {
		return err
	}
	signatureKey, err := r.ReadOpaque()
	if err != nil {
		return err
	}
	if err := self.Credential.UnmarshalMLS(r); err != nil {
		return err
	}
	if err := self.Capabilities.UnmarshalMLS(r); err != nil {
		return err
	}
	source, err := r.ReadUint8()
	if err != nil {
		return err
	}
	self.EncryptionKey = HpkePublicKey(encryptionKey)
	self.SignatureKey = SignaturePublicKey(signatureKey)
	self.LeafNodeSource = LeafNodeSource(source)
	switch self.LeafNodeSource {
	case LeafNodeSourceKeyPackage:
		if self.Lifetime.NotBefore, err = r.ReadUint64(); err != nil {
			return err
		}
		if self.Lifetime.NotAfter, err = r.ReadUint64(); err != nil {
			return err
		}
	case LeafNodeSourceUpdate:
		// the empty struct arm, matching the encode half
	case LeafNodeSourceCommit:
		if self.ParentHash, err = r.ReadOpaque(); err != nil {
			return err
		}
	default:
		// the same refusal the encode half makes, and for the stronger reason: an unknown
		// source read leniently would have this decoder guess how many bytes the variant
		// occupies, and every guess accepts a second encoding of some other leaf
		return ErrTreeMalformed
	}
	self.Extensions, err = ReadExtensions(r)
	return err
}

// UnmarshalMLS decodes the whole leaf. It consumes exactly its own fields, because a LeafNode is
// also read inside a ratchet tree and inside a KeyPackage, where the bytes after it are not its.
//
// The decode is STAGED and assigned whole, and that is the property rather than a tidiness: the
// leaf this answers is a function of the bytes it read and of nothing the receiver arrived
// holding. Two things follow from it, and neither holds if the fields are written as they are
// read:
//
//   - a refused decode leaves the receiver untouched. Credential.UnmarshalMLS already keeps
//     that discipline -- it refuses the credential type before it reads the identity, so no
//     certificate chain is ever allocated on this package's behalf -- and two decoders of the
//     same commit disagreeing about it is how a caller comes to rely on the wrong one. Every
//     truncation and every unrecognised source is a refusal, and there are more of the first
//     than there are field boundaries.
//   - a receiver decoded into twice answers the second encoding both times. The variant arms
//     assign only the field their own source carries, so a commit leaf decoded into a receiver
//     that held a key_package leaf would otherwise come back carrying the previous leaf's
//     Lifetime. Nothing in the bytes says so, and the value compares unequal to the same bytes
//     decoded fresh.
func (self *LeafNode) UnmarshalMLS(r *syntax.Reader) error {
	staged := LeafNode{}
	if err := staged.unmarshalCore(r); err != nil {
		return err
	}
	signature, err := r.ReadOpaque()
	if err != nil {
		return err
	}
	staged.Signature = signature
	*self = staged
	return nil
}

// the C1 pin: drift between this type and the one codec convention fails at build.
var _ syntax.Codec = (*LeafNode)(nil)

// cloneSlice answers a shallow copy of in, and nil for nil.
//
// nil for nil for group_context.go's cloneBytes reason: an absent vector and a present empty
// one are different bytes on the wire, and a clone that turned one into the other would change
// the encoding of the thing it was asked to duplicate.
//
// Shallow is correct for a slice of scalars and is NOT correct for a slice of structures
// holding storage of their own -- Clone below copies the extension bodies itself for exactly
// that reason.
func cloneSlice[T any](in []T) []T {
	if in == nil {
		return nil
	}
	out := make([]T, len(in))
	copy(out, in)
	return out
}

// Clone answers a leaf that shares no storage with this one.
//
// A leaf is copied whenever it is re-signed or installed in a provisional tree, so nothing may
// alias between two epochs' trees: a commit that is later rejected must leave the epoch it was
// computed against exactly as it found it, and a shared backing array is how a rejected commit
// mutates a tree that never accepted it. Every slice the structure reaches is copied, at every
// depth, because a deep copy that stops one level short is indistinguishable from a complete
// one until the day something writes through the level it missed.
func (self *LeafNode) Clone() *LeafNode {
	out := *self
	out.EncryptionKey = HpkePublicKey(cloneBytes(self.EncryptionKey))
	out.SignatureKey = SignaturePublicKey(cloneBytes(self.SignatureKey))
	out.Credential.Identity = cloneBytes(self.Credential.Identity)
	out.Capabilities.Versions = cloneSlice(self.Capabilities.Versions)
	out.Capabilities.CipherSuites = cloneSlice(self.Capabilities.CipherSuites)
	out.Capabilities.Extensions = cloneSlice(self.Capabilities.Extensions)
	out.Capabilities.Proposals = cloneSlice(self.Capabilities.Proposals)
	out.Capabilities.Credentials = cloneSlice(self.Capabilities.Credentials)
	out.ParentHash = cloneBytes(self.ParentHash)
	out.Signature = cloneBytes(self.Signature)
	out.Extensions = cloneExtensions(self.Extensions)
	return &out
}

// cloneExtensions answers an extension vector that shares no storage with the one it was made
// from, and nil for nil for cloneSlice's reason.
//
// EACH ENTRY IS REBUILT FROM THE ENTRY IT IS COPYING, rather than a shallow copy fixed up
// through an index afterwards, and that shape is the whole of why this exists. What stood in
// Clone was
//
//	out.Extensions = cloneSlice(self.Extensions)
//	for i := range out.Extensions {
//		out.Extensions[i].ExtensionData = cloneBytes(self.Extensions[i].ExtensionData)
//	}
//
// which pairs two sequences by hand at every iteration, and a pairing written wrong -- entry
// zero's body read into every entry -- answers a leaf whose required_capabilities carries the
// urmessage_leaf_keys octets: correctly tagged, correctly length prefixed, refused by nothing
// this side of a peer parsing a body against the wrong type. There is no index here to get
// wrong, because the entry a body is taken from IS the entry it is written into.
// (*GroupContext).Clone is the same rebuild over the same element type one file over.
func cloneExtensions(in []Extension) []Extension {
	if in == nil {
		return nil
	}
	out := make([]Extension, 0, len(in))
	for _, extension := range in {
		out = append(out, Extension{
			ExtensionType: extension.ExtensionType,
			ExtensionData: cloneBytes(extension.ExtensionData),
		})
	}
	return out
}

// The RFC 9420 section 7.2 signature label, written once because a label spelled one way in
// the signing half and another in the verifying half agrees with itself perfectly: ed25519
// signs whatever preimage it is handed, and only a peer can tell "LeafNodeTBS" from
// "LeafNodeTbs". The label is what keeps a leaf signature from being a valid signature over
// some other structure the same key signed.
const leafNodeSignatureLabel = "LeafNodeTBS"

// errBadSignature is ErrBadSignature in the validation plan's catalogue, where it is ValSem010,
// and that plan owns the single declaration site for the exported name. It has not landed in
// this package yet, so the refusal is carried by this unexported value until it does -- the
// shape credential.go's errProfileCredentialType and extension.go's
// errMissingRequiredCapability already take, and the stand in gate fails on the commit that
// lands the exported twin beside it.
//
// It WRAPS ErrCryptoBadSignature, which is the pairing crypto_errors.go's own header already
// describes for the exported name: the crypto layer says the primitive refused, this layer says
// the leaf is not authentic, and a caller may ask either question rather than having to guess
// which of two sentinels for one condition it will be handed. Standing the wrap up now rather
// than when errors.go lands means no errors.Is answer this package gives changes under that
// commit.
var errBadSignature = fmt.Errorf("mls: leaf node signature does not verify: %w", ErrCryptoBadSignature)

// The four rules of Validate that all mean "the leaf does not list a capability it must",
// each with an identity of its own and each WRAPPING errMissingRequiredCapability so that a
// caller which only wants that broader question keeps being answered it.
//
// Four values rather than one, because a test can state only what an error can be asked. With
// one sentinel behind five return sites -- these four plus Capabilities.Supports' three loops
// -- every assertion in this area reads errors.Is(err, errMissingRequiredCapability), which
// any of the five satisfies, so no test can say that the rule it is named for is the rule that
// fired. That is not hypothetical here: the erratum 8745 group context rule had no identity of
// its own, TestErrata8745 asked only the broad question, and the case that would have caught
// that loop being applied to element zero was missing for exactly as long. An error a test
// cannot distinguish is a rule a test cannot observe.
//
// They are stand ins in the same sense errMissingRequiredCapability is -- the validation plan
// owns the exported catalogue -- and the stand in gate watches them for an exported twin.
var (
	errCredentialTypeNotListed        = fmt.Errorf("%w: credential type", errMissingRequiredCapability)
	errCipherSuiteNotListed           = fmt.Errorf("%w: ciphersuite", errMissingRequiredCapability)
	errLeafExtensionNotListed         = fmt.Errorf("%w: leaf extension", errMissingRequiredCapability)
	errGroupContextExtensionNotListed = fmt.Errorf("%w: group context extension", errMissingRequiredCapability)
)

// errLeafSourceRuleWaivedAndStated is a LeafValidationContext that both waives section 7.3's
// leaf_node_source rule and states an expectation for it.
//
// A refusal rather than a precedence rule, because either precedence is a trap. "The waiver wins"
// makes one added field switch off a real door in silence, which is the defect class this whole
// area exists to close; "the expectation wins" makes the waiver a field a caller can set with no
// effect, which is a second way to write a check that cannot fire. A caller that wrote both did
// not decide, so neither answer is its answer.
//
// It is a refusal about the CONTEXT and not about the leaf, so it names no leaf: every leaf
// reaching a call site written this way gets it, which is what makes it visible on the first run
// rather than on the first hostile input.
var errLeafSourceRuleWaivedAndStated = errors.New("mls: the leaf validation context both waives the leaf_node_source rule and expects a source")

// signatureContent is RFC 9420 section 7.2's LeafNodeTBS: the leaf's fields down to and
// including extensions, followed -- for the update and commit sources ONLY -- by the group id
// and the leaf index.
//
// That trailing select is the whole security value of this preimage, and it is the part an
// implementation can drop while agreeing with itself perfectly. A leaf signed without its group
// id verifies in whatever group it is pasted into. A leaf signed without its leaf index
// verifies at whatever position of the tree it is moved to. Both are member substitutions,
// both sign and verify against this package, and neither is visible to a round trip, to a
// golden taken off this encoder, or to any sign then verify test. What sees them is a corpus
// another implementation signed and the two binding tests beside it.
//
// The key_package arm carries NEITHER, and that is the section 7.2 structure rather than a
// shortcut: the select's key_package case is an empty struct, because a KeyPackage is minted
// before there is a group to bind it to or a position to put it in. Section 7.2's prose says
// only that "the group ID of the group is added as context"; the STRUCTURE adds the leaf index
// as well, and the prose is the half an implementation must not be written from. Section 7.3,
// which is where the leaf validation rules live, restates none of it -- it says only that the
// signature is verified using signature_key -- so the conditional binding is section 7.2's
// alone. groupId and leafIndex are therefore IGNORED under key_package rather than refused: a
// caller holding a group cannot tell from the leaf alone which arm applies, and refusing would
// push that switch into every call site.
//
// LeafNodeTBS is a preimage and never a message, so it does not go through syntax.Marshal:
// nothing decodes these bytes, and the trailing byte contract Marshal carries belongs to a
// wire type.
//
// It opens the Writer itself rather than handing an encoder to marshalBytes, and that is not a
// style choice. TestNoStubShapesRemainInSource reads a declaration's own body for the
// parameters it uses and deliberately does not descend into a function literal, so a preimage
// whose group id and leaf index were touched only inside a closure reads there exactly like one
// that ignored them -- and ignoring them IS the substitution this suffix exists to prevent.
// What is given up is a name: marshalBytes is a fresh Writer, the caller's encoder and Bytes,
// which is these three statements with the section 7.2 select written between them, under the
// same default vector limit for the same reason -- a LeafNodeTBS is one leaf and never a
// ratchet tree.
func (self *LeafNode) signatureContent(groupId []byte, leafIndex LeafIndex) ([]byte, error) {
	w := syntax.NewWriter()
	// the same call MarshalMLS makes, and that shared call is what stops the two from coming
	// apart: a field written in the wire form and not in the preimage is a signature over a
	// structure nobody sent, and it is invisible to both halves of a round trip
	if err := self.marshalCore(w); err != nil {
		return nil, err
	}
	switch self.LeafNodeSource {
	case LeafNodeSourceKeyPackage:
		// the empty struct arm of the section 7.2 select
	case LeafNodeSourceUpdate, LeafNodeSourceCommit:
		w.WriteOpaque(groupId)
		w.WriteUint32(uint32(leafIndex))
	default:
		// unreachable while marshalCore refuses an unknown source first, and written anyway
		// rather than left to fall through. A fall through would make an unrecognised source
		// the third spelling of "no context bound", which is exactly the key_package arm --
		// so a fourth source added later would inherit the unbound preimage in silence
		// instead of failing here.
		return nil, ErrTreeMalformed
	}
	// the Writer's sticky error, surfaced here the way marshalBytes surfaces it: a preimage
	// that came back short is a signature over bytes nobody agreed to
	return w.Bytes()
}

// Sign replaces this leaf's signature with one over its LeafNodeTBS.
//
// The signature field itself is NOT part of what is signed -- marshalCore stops above it --
// so signing twice over one leaf answers the same bytes, and a stale signature left on the
// receiver cannot feed into the new one.
//
// groupId and leafIndex are the context the update and commit sources bind and the key_package
// source ignores; see signatureContent.
func (self *LeafNode) Sign(crypto CryptoProvider, signer SignaturePrivateKey,
	groupId []byte, leafIndex LeafIndex) error {
	if crypto == nil {
		return fmt.Errorf("%w: the signature over the LeafNodeTBS is made through it", ErrNilCryptoProvider)
	}
	content, err := self.signatureContent(groupId, leafIndex)
	if err != nil {
		return err
	}
	signature, err := crypto.SignWithLabel(signer, leafNodeSignatureLabel, content)
	if err != nil {
		return err
	}
	self.Signature = signature
	return nil
}

// VerifySignature answers nil only if this leaf's signature is a signature by this leaf's own
// signature_key over this leaf's LeafNodeTBS in the context it was handed.
//
// Every way the primitive can say no becomes errBadSignature, and the detail is dropped on
// purpose: a wrong length key, a wrong length signature and a signature over other content all
// arrived from the network, and which of them it was is not something a peer gets to learn from
// the error. The refusal is never a nil error and never a partial comparison -- a length
// mismatch is refused inside VerifyWithLabel before anything is compared, which is why nil,
// empty, truncated and over long signatures all reach this line as a refusal rather than as a
// panic.
//
// A failure to BUILD the preimage is returned as itself rather than as a signature failure. An
// unknown leaf_node_source or a credential outside the profile is a structural refusal no
// signature could have repaired, and collapsing it into errBadSignature would send a caller
// looking for a forgery over a leaf that is simply not one this package reads.
func (self *LeafNode) VerifySignature(crypto CryptoProvider,
	groupId []byte, leafIndex LeafIndex) error {
	if crypto == nil {
		return fmt.Errorf("%w: the signature over the LeafNodeTBS is checked through it", ErrNilCryptoProvider)
	}
	content, err := self.signatureContent(groupId, leafIndex)
	if err != nil {
		return err
	}
	if err := crypto.VerifyWithLabel(self.SignatureKey, leafNodeSignatureLabel,
		content, self.Signature); err != nil {
		return errBadSignature
	}
	return nil
}

// Spec A section 3.1: a fresh KeyPackage leaf is valid from now, back dated by the clock skew
// allowance, for the default lifetime.
const (
	leafLifetimeSkewSeconds    uint64 = 3600
	leafLifetimeDefaultSeconds uint64 = 90 * 24 * 3600
)

// NewLeafNode builds and signs the key_package source leaf -- the only source a leaf can carry
// before it is in a tree -- which is signed with no group id and no leaf index, and is why this
// takes neither.
//
// The signature key is DERIVED from the private key it was handed and never generated here.
// Calling SignatureKeyPair inside a constructor that was already given a key pair is how a leaf
// ends up signed by a key nobody holds: the leaf verifies against itself, the caller stores the
// private half it passed in, and every later Update by that member is refused by the group with
// nothing to point at.
//
// Every vector the caller supplied is COPIED before the leaf keeps it. This is the property
// BasicCredential already states for the identity, and it applies to the encryption key, to the
// five capability vectors and to every extension body for the same reason: the leaf outlives
// the call and the caller usually holds a longer buffer it goes on writing into, so a retained
// slice is a leaf that changes after it was signed -- a signature that verified when it was
// made and does not afterwards, with nothing in between to point at. Clone does the copying
// rather than a field list written here, so a field added to LeafNode is copied on the commit
// that adds it.
func NewLeafNode(crypto CryptoProvider, signer SignaturePrivateKey, cred Credential,
	encKey HpkePublicKey, caps Capabilities, exts []Extension) (*LeafNode, error) {
	// the provider is refused before any argument is judged, which is the only order that
	// does not dereference it: a length check that reached the provider for a width first
	// would take the caller's process rather than its call
	if crypto == nil {
		return nil, fmt.Errorf("%w: the leaf is signed and verified through it", ErrNilCryptoProvider)
	}
	signatureKey, err := signaturePublicKeyOf(signer)
	if err != nil {
		return nil, err
	}
	// the clock is read as a SIGNED unix second and clamped before it is widened.
	// time.Now().Unix() is negative on a machine whose clock is not set, and uint64 of a
	// negative second is about 1.8e19 -- a not_before no peer is ever past, carried by a leaf
	// that is otherwise perfectly well formed and correctly signed. Clamping answers a
	// lifetime that is merely wrong, which the receiver's section 7.3 lifetime check refuses.
	now := max(time.Now().Unix(), 0)
	leaf := &LeafNode{
		EncryptionKey:  encKey,
		SignatureKey:   signatureKey,
		Credential:     cred,
		Capabilities:   caps,
		LeafNodeSource: LeafNodeSourceKeyPackage,
		Lifetime: Lifetime{
			NotBefore: uint64(max(now-int64(leafLifetimeSkewSeconds), 0)),
			NotAfter:  uint64(now) + leafLifetimeDefaultSeconds,
		},
		Extensions: exts,
	}
	// everything above is still the caller's storage; from here it is the leaf's own
	leaf = leaf.Clone()
	if err := leaf.Sign(crypto, signer, nil, 0); err != nil {
		return nil, err
	}
	// one ed25519 verify per key package, which buys a failure HERE rather than a leaf every
	// peer refuses later: a provider whose signing half and verifying half disagree, or a
	// preimage that cannot be rebuilt from what was just written, is a leaf nobody can use and
	// there is nothing in the returned value that would say so.
	if err := leaf.VerifySignature(crypto, nil, 0); err != nil {
		return nil, err
	}
	return leaf, nil
}

// ---------------------------------------------------------------------------
// RFC 9420 section 7.3: leaf node validation
// ---------------------------------------------------------------------------

// The RFC 9420 section 7.2 "default" extension types, as a RANGE rather than as a list.
//
// Section 7.2 says of the capabilities field: "The following proposal and extension types are
// considered 'default' and MUST NOT be listed", and names 0x0001 application_id, 0x0002
// ratchet_tree, 0x0003 required_capabilities, 0x0004 external_pub and 0x0005 external_senders.
// Those five are exactly the extension types RFC 9420 registers for itself -- section 17.3's
// initial registry is 0x0001 to 0x0005 and nothing else -- and that is what makes the class a
// contiguous range and not five identifiers somebody typed out. A code point registered by a
// later document is NOT default and has to be listed; this profile's own 0xF001 to 0xF003
// private use points are not default either, and are listed by every leaf this package builds.
//
// This matters in both directions, and the plan that specified this file got both wrong, in
// opposite ways, by naming two of the five:
//
//   - Section 7.2's own sentence is "The types of any NON-DEFAULT extensions that appear in the
//     extensions field of a LeafNode MUST be included in the extensions field of the
//     capabilities field." A check that demanded it of every extension would refuse a leaf
//     carrying application_id (0x0001), which is the most common LeafNode extension there is,
//     for doing exactly what section 7.2 tells it to do.
//   - Section 13.4 says a member's capabilities "MUST indicate support for each extension in
//     the GroupContext". external_senders and required_capabilities are both GroupContext
//     extensions, and section 7.2 forbids listing either. The only reading under which the two
//     sentences are not in direct contradiction is that section 13.4's requirement is
//     discharged for the default types by their being default: they are assumed of every
//     implementation, which is section 11.1's "The 'default' proposal and extension types
//     defined in this document are assumed to be implemented by all clients".
//
// So the exemption is not a convenience. Without it this validator refuses every conforming
// leaf of any group whose GroupContext carries external_senders, and every conforming leaf
// that carries an application_id.
// The two bounds are deliberately UNTYPED and not ExtensionType, which looks like a slip and is
// not. This package's registry gates derive their class off the type checker: every package
// level constant whose type is ExtensionType is taken to name a code point the extension
// registry assigns, and TestEveryRegistryDeclaredHereHoldsTheCodePointsTheRfcAssigns and the
// seed corpus generators are all written on that premise. A bound is not a registry member --
// nothing encodes it, no decoder has an arm for it -- so typing it ExtensionType would put two
// values that are not code points into every derivation that enumerates them, and the corpus
// would grow two seeds standing for extensions nobody can send. Untyped keeps the comparison
// below exact and the class honest.
const (
	defaultExtensionTypeLow  = 0x0001
	defaultExtensionTypeHigh = 0x0005
)

// isDefaultExtensionType reports whether section 7.2 forbids listing t in a capabilities
// vector, and therefore whether asking a leaf to list it is a refusal of a conforming leaf.
func isDefaultExtensionType(t ExtensionType) bool {
	return defaultExtensionTypeLow <= t && t <= defaultExtensionTypeHigh
}

// LeafValidationContext is everything RFC 9420 section 7.3 checks about ONE leaf that does not
// come from the leaf itself.
//
// A structure and not eight positional parameters, and the reason is NowMs and ClockSkewMs:
// two adjacent uint64 milliseconds in a positional signature are two arguments the compiler
// cannot tell apart, and swapping them is a validator that treats the clock as the tolerance.
//
// What is NOT here is as much of section 7.3 as is. Four of its rules are properties of a SET
// of leaves, or of an application policy, rather than of one leaf, and none of them can be
// answered from this structure:
//
//   - "Verify that the following fields are unique among the members of the group:
//     signature_key, encryption_key" needs every other leaf. It is the ratchet tree's, in
//     tree_sync.go.
//   - "Verify that the credential type is supported by all members of the group ... and that
//     the capabilities field of this LeafNode indicates support for all the credential types
//     currently in use by other members" needs every other leaf's capabilities and credential.
//     Also the ratchet tree's.
//   - The update arm of the leaf_node_source rule -- "encryption_key represents a different
//     public key than the encryption_key in the leaf node being replaced" -- needs the leaf
//     being replaced. It is proposal validation's, in the group lifecycle plan.
//   - Section 7.2's "Applications MUST define a maximum total lifetime that is acceptable for
//     a LeafNode, and reject any LeafNode where the total lifetime is longer than this
//     duration" is an application policy this profile has not fixed a number for. Until it
//     does, a key_package leaf may declare a not_after a thousand years out and be current.
//
// A fifth rule is absent for a different reason and is written down for the same one. Section
// 7.2's other half -- "The following proposal and extension types are considered "default" and
// MUST NOT be listed" -- is a rule about what a capabilities vector may CONTAIN. It is a
// property of one leaf, so unlike the four above this structure could answer it. It
// deliberately does not, on two grounds:
//
//   - Section 7.3's list of what a leaf validator verifies does not restate it. Refusing a leaf
//     for it would be a refusal this profile invented: a peer whose leaf harmlessly lists
//     ratchet_tree would be turned away by this implementation and by no other.
//   - It changes no decision made here. isDefaultExtensionType is used as an EXEMPTION and
//     never as a prohibition, so every rule that reads capabilities.extensions has already let
//     the default class through before it looks at what is listed. A leaf that lists a default
//     type is accepted or refused on exactly the same grounds as one that does not.
//
// TestLeafNodeValidateDoesNotRefuseALeafThatListsADefaultExtensionType states that decision
// over the derived default class, so enforcing the MUST NOT later is a commit that has to
// change this paragraph too rather than one that quietly contradicts it.
//
// Each is stated here rather than left out silently, because a validator's dangerous failure
// is the check that accepts by never having looked, and a reader who finds most of a section
// implemented has no way to tell a deliberate hand-off from an omission.
type LeafValidationContext struct {
	// Crypto verifies the leaf's signature. A nil one is refused before anything else.
	Crypto CryptoProvider

	// Suite is the group's ciphersuite. Section 11.1: "At a minimum, all members of the group
	// need to support the cipher suite and protocol version in use." The version half cannot
	// be decided from here -- this structure carries no ProtocolVersion and section 7.3 does
	// not restate the rule -- so the ciphersuite half is checked and the version half is owed.
	Suite CipherSuite

	// GroupId and LeafIndex are the context the update and commit sources bind their signature
	// to and the key_package source ignores; see signatureContent. Both are meaningless under
	// key_package and are passed anyway, because a caller holding a group cannot tell from the
	// leaf alone which arm applies.
	GroupId   []byte
	LeafIndex LeafIndex

	// ExpectedSource is section 7.3's leaf_node_source rule, and it is a REQUIRED input rather
	// than a defaulted one. The same Validate is reached from three places -- key_package.go
	// with key_package, proposal validation with update, the update path with commit -- and a
	// validator that did not compare against an expectation would accept a key_package leaf,
	// lifetime and all, exactly where an update leaf belongs.
	//
	// ALL THREE ARE WRITTEN, and this paragraph names them because it has twice described
	// doors that did not exist. key_package.go states key_package, validate_proposals.go's
	// validateUpdateLeafNodeIsValidForAnUpdate states update, and treekem.go's
	// ValidateUpdatePathLeafNode states commit over a RECEIVED UpdatePath's leaf. It described
	// all three for as long as only one of them existed -- which is how an Update's leaf came
	// to reach the tree with nothing having validated it at all -- and then described all
	// three again for as long as two of them did.
	//
	// So the sentence is no longer what holds it.
	// TestEveryLeafNodeSourceEitherHasAValidationDoorOrAnAdmittedGap derives the source class
	// off this package's LeafNodeSource constants and the door class off the CALL SITES of
	// this validator, and holds the two against each other: a fourth source, a door deleted,
	// or a call site whose expectation is not a constant this package declares is a failure
	// rather than a paragraph that has gone quietly out of date.
	ExpectedSource LeafNodeSource

	// SourceIsNotJudgedHere waives the rule above, and one caller legitimately needs it:
	// (*RatchetTree).validateLeaves sweeps a settled tree, which holds all three sources at
	// once -- key_package under a member added and not yet committed over, update under one
	// that refreshed itself, commit under whoever last committed a path -- so there is no
	// single source a whole tree sweep could demand. Its own header names the doors that owe
	// the per position rule instead.
	//
	// A FIELD rather than a value of ExpectedSource, and that is the whole of why it exists.
	// The waiver used to be spelled `ExpectedSource: leaf.LeafNodeSource`, the expectation
	// taken from the very leaf being judged, and under that spelling the comparison below is
	// `x != x` for every input: ErrLeafNodeSourceMismatch could not fire from the tree door
	// for anything, and the call site read exactly like a door. A waiver has to be visible to
	// be reviewable, and a gate can read a field name off a call site while it cannot decide
	// from an arbitrary expression whether the value came out of the leaf.
	//
	// It waives ONLY that rule; every other clause of Validate runs, which is what makes the
	// tree sweep worth running. Setting it beside an ExpectedSource is refused rather than
	// resolved -- see errLeafSourceRuleWaivedAndStated.
	SourceIsNotJudgedHere bool

	// RequiredCaps is the group's required_capabilities extension body, or nil for a group
	// that carries none. Nil is "no requirement" and is satisfied by anything.
	RequiredCaps *RequiredCapabilities

	// GroupExtensions is the GroupContext's extensions vector: section 13.4's rule, as
	// corrected by erratum 8745. See ERRATA.md and the comment on the loop below.
	GroupExtensions []Extension

	// NowMs is the validating client's clock in unix milliseconds, and ZERO IS AN OPT OUT: a
	// caller that passes 0 gets no lifetime check at all.
	//
	// That is the one place this validator accepts by not looking, and it is deliberate.
	// Section 7.3 makes the lifetime check a MUST only for a leaf "in a message being sent by
	// the client" and a RECOMMENDED for one "being received", because a leaf can expire in
	// flight; a receiver with no trustworthy clock has to be able to say so. Every SENDING
	// path -- a KeyPackage this client mints, a proposal it builds -- must pass a real clock,
	// and a real clock is never 0.
	NowMs uint64

	// ClockSkewMs widens the lifetime interval at BOTH ends by this many milliseconds. It is
	// applied to the lifetime rather than to the clock so that the two ends cannot come to
	// have different tolerances by accident.
	ClockSkewMs uint64
}

// Validate answers nil only if this leaf satisfies every RFC 9420 section 7.3 rule that can be
// decided from the leaf and this context alone. The ones that cannot are named on
// LeafValidationContext.
//
// The order is refusal-cheapest-and-most-specific first, and two positions in it are load
// bearing rather than tidy:
//
//   - the provider is refused before any field of the leaf is judged, which is the only order
//     that does not dereference it. It is the order Sign and VerifySignature already take.
//   - the source is compared before the signature is checked. The source SELECTS what the
//     signature covers -- a key_package leaf's preimage carries a lifetime and no group id, an
//     update leaf's carries a group id and no lifetime -- so verifying first would report a
//     signature failure for a leaf whose real fault is that it is the wrong kind of leaf.
//
// Section 7.3's "Verify that the credential in the LeafNode is valid, as described in
// Section 5.3.1" is discharged in two halves and neither is a check written here. The half this
// profile can decide -- is the credential type one this implementation reads at all -- is
// Credential.MarshalMLS's, which refuses everything but basic, and the signature preimage is
// built through it: a leaf carrying an x509 credential comes back as errProfileCredentialType
// from VerifySignature, which returns a preimage failure as itself rather than collapsing it
// into a signature failure. The half this profile cannot decide -- does the identity in the
// credential actually belong to the signature key, which section 5.3.1 hands to an
// Authentication Service -- has no AS in this profile and is NOT performed. One spelling of
// each rule and not two, and the second is written down because a reader who found seven of
// section 7.3's eight bullets here would otherwise take the eighth for an oversight.
func (self *LeafNode) Validate(ctx *LeafValidationContext) error {
	// a nil context is a context whose provider is nil, and it gets the refusal a nil provider
	// gets, so Validate(nil) and Validate(&LeafValidationContext{}) cannot answer two
	// different things about the same missing thing
	if ctx == nil || ctx.Crypto == nil {
		return fmt.Errorf("%w: the leaf's signature is verified through it", ErrNilCryptoProvider)
	}
	// the contradiction before the rule, so a context that cannot be read one way is refused
	// rather than being read the convenient way. See errLeafSourceRuleWaivedAndStated.
	if ctx.SourceIsNotJudgedHere && ctx.ExpectedSource != 0 {
		return fmt.Errorf("%w: it waives the rule and expects source %d",
			errLeafSourceRuleWaivedAndStated, uint8(ctx.ExpectedSource))
	}
	if !ctx.SourceIsNotJudgedHere && self.LeafNodeSource != ctx.ExpectedSource {
		return fmt.Errorf("%w: leaf_node_source is %d and this position takes %d",
			ErrLeafNodeSourceMismatch, uint8(self.LeafNodeSource), uint8(ctx.ExpectedSource))
	}
	// section 7.2: "the credential type used in the LeafNode MUST be included in the
	// credentials field of the capabilities field". There is no default credential type --
	// section 11.1 says so in as many words, "Note that this is not true for credential types"
	// -- so this one has no exemption to make.
	//
	// It is checked BEFORE the signature, which is section 7.3's own order -- the credential
	// rule is its first bullet and the signature rule its second -- and is also the only order
	// under which this check can ever fire. Credential.MarshalMLS refuses every credential type
	// outside this profile and the signature preimage is built through it, so a leaf carrying an
	// x509 credential never reaches a line below VerifySignature: put this check after the
	// signature and it becomes a branch no input can take, which is indistinguishable from a
	// check nobody wrote.
	if !self.Capabilities.SupportsCredential(self.Credential.CredentialType) {
		return fmt.Errorf("%w: the leaf's own credential type %#04x is not in its capabilities",
			errCredentialTypeNotListed, uint16(self.Credential.CredentialType))
	}
	if err := self.VerifySignature(ctx.Crypto, ctx.GroupId, ctx.LeafIndex); err != nil {
		return err
	}
	// section 11.1: "At a minimum, all members of the group need to support the cipher suite
	// and protocol version in use." This is the half that can be decided here, and it is what
	// makes Suite an input rather than a field nothing reads: a member whose capabilities do
	// not list the group's ciphersuite cannot do the group's crypto, whatever else verifies.
	if !self.Capabilities.SupportsCipherSuite(ctx.Suite) {
		return fmt.Errorf("%w: the group's ciphersuite %#04x is not in the leaf's capabilities",
			errCipherSuiteNotListed, uint16(ctx.Suite))
	}
	// section 7.3: "Verify that the extensions in the LeafNode are supported by checking that
	// the ID for each extension in the extensions field is listed in the capabilities.extensions
	// field of the LeafNode", narrowed by section 7.2 to the non-default types.
	//
	// EVERY entry, and the urmessage_leaf_keys body of every entry that carries one. This is a
	// WALK and not a lookup, and the difference is the whole reason it is written out rather
	// than delegated: FindExtensionEntry refuses a repeated type, so a body checked through the
	// lookup would make this clause refuse the leaf for carrying two instead of range checking
	// the second body -- and section 7.3 is a rule about every extension a leaf carries, which a
	// validator applies to all of them whatever some other door does about the repeat. Applying
	// it to the first alone is the p4 ValSem401 shape exactly: a rule applied to element zero
	// while the loop that reaches the rest never runs it.
	//
	// So a leaf carrying two well formed urmessage_leaf_keys entries passes HERE and is refused
	// at the lookup, by LeafKeysOf, which is this package's one position on the repeat stated
	// from the other side. TestEveryExtensionTypeSelectionOfBothPackagesIsClassifiedHere is what
	// holds this loop to visiting every entry, so an edit that quietly turns it into a lookup
	// fails rather than narrowing the clause to element zero.
	for i := range self.Extensions {
		extensionType := self.Extensions[i].ExtensionType
		if !isDefaultExtensionType(extensionType) && !self.Capabilities.SupportsExtension(extensionType) {
			return fmt.Errorf("%w: the leaf carries extension type %#04x and does not list it",
				errLeafExtensionNotListed, uint16(extensionType))
		}
		if extensionType != ExtensionTypeUrmessageLeafKeys {
			continue
		}
		// MASTER section 5.3: the body is range checked HERE because this is the last point
		// before the leaf is trusted by the tree and by connect/message's wrap path. The whole
		// entry goes to ParseLeafKeysFrom rather than its body to ParseLeafKeysExtension, so
		// the tag is checked by the only one of the two that is given it.
		if _, err := ParseLeafKeysFrom(self.Extensions[i]); err != nil {
			if errors.Is(err, ErrLeafKeysExtensionInvalid) {
				return err
			}
			// a truncated body, or bytes after it. ErrLeafKeysExtensionInvalid's own
			// declaration names trailing bytes as one of the things it stands for, so it is
			// the sentinel a caller asks with; the decoder's error is kept underneath it so a
			// caller can still ask the narrower question.
			return fmt.Errorf("%w: %w", ErrLeafKeysExtensionInvalid, err)
		}
	}
	// RFC 9420 section 13.4, as corrected by erratum 8745 (see ERRATA.md): "A client adding a
	// new member to a group MUST verify that the LeafNode for the new member is compatible
	// with the group's extensions. The capabilities field MUST indicate support for each
	// extension in the GroupContext" -- plus, per the erratum, the same of a leaf that arrives
	// by an Update proposal or in a commit's update_path.
	//
	// So this loop is NOT conditioned on the source. The pre-erratum reading applies it to
	// key_package leaves alone, which leaves an existing member free to update itself into a
	// leaf that no longer supports an extension the group is using -- and section 13.4's own
	// note is that "all MLS GroupContext extensions are mandatory, in the sense that an
	// extension in use by the group MUST be supported by all members of the group", which is a
	// statement about members and not about joiners.
	//
	// The default types are exempt for the reason isDefaultExtensionType gives: section 7.2
	// forbids listing them, so demanding them here would refuse every conforming leaf of any
	// group whose GroupContext carries external_senders or required_capabilities.
	//
	// EVERY entry, for the same reason the loop over the leaf's own extensions reads every one
	// of those. A GroupContext carries a vector and a real one carries several -- and the
	// entries a real group carries are led by exactly the ones the exemption above lets
	// through, required_capabilities and external_senders. So a loop that answered element
	// zero would step over an exempt entry, never reach the non-default extension behind it,
	// and admit -- or let a member update into -- a leaf that does not support an extension the
	// group is using, which is the consequence section 13.4 and ERRATA.md are about. That is
	// the p4 ValSem401 shape again, and it is why
	// TestLeafNodeValidateReadsEveryGroupContextExtensionAndNotOnlyTheFirst puts the offender
	// at every position of the vector rather than at the only position a one entry vector has.
	if err := self.checkGroupContextExtensions(ctx.GroupExtensions); err != nil {
		return err
	}
	// section 7.3: "If the GroupContext has a required_capabilities extension, then the
	// required extensions, proposals, and credential types MUST be listed in the LeafNode's
	// capabilities field."
	if err := self.Capabilities.Supports(ctx.RequiredCaps); err != nil {
		return err
	}
	return self.validateLifetime(ctx)
}

// checkGroupContextExtensions is the loop above, as a body of its own so that the second door onto
// this rule runs THIS one rather than a copy.
//
// The second door is validate_commit.go's CheckErrata8745, which states erratum 8745 over the leaf
// an UpdatePath publishes without a provider and without verifying a signature. Two bodies applying
// one rule is how the two come to disagree, and the disagreement is invisible: both accept every
// conforming leaf, and they differ only on the leaf the erratum is about.
//
// EVERY entry, for the reason the caller's comment gives: a loop that answered element zero steps
// over the exempt entries a real GroupContext carries first and never reaches the non-default
// extension behind them.
func (self *LeafNode) checkGroupContextExtensions(groupExtensions []Extension) error {
	for i := range groupExtensions {
		extensionType := groupExtensions[i].ExtensionType
		if !isDefaultExtensionType(extensionType) && !self.Capabilities.SupportsExtension(extensionType) {
			return fmt.Errorf("%w: the group context carries extension type %#04x and the leaf does not list it",
				errGroupContextExtensionNotListed, uint16(extensionType))
		}
	}
	return nil
}

// validateLifetime is section 7.3's lifetime rule, and it is a body of its own because it is
// the one rule of the eight whose two ends are symmetric enough to be swapped without anything
// noticing: a leaf comfortably inside its lifetime is accepted by a comparison in either
// direction and by a skew applied to either end.
//
// The interval is widened by the skew at BOTH ends -- accepted when
// not_before - skew <= now <= not_after + skew -- rather than the clock being moved, because
// moving the clock is how one end comes to be widened and the other narrowed. Both widenings
// are written as guarded SUBTRACTIONS from the side that cannot wrap: not_after + skew
// overflows uint64 for an attacker-chosen not_after near the top of the range, and the wrapped
// sum then refuses a leaf that a slightly SMALLER not_after would have accepted, which is a
// validator that is not monotone in the field it is reading.
//
// The lifetime is a variant field carried only under key_package, so it is checked only there.
// Under update and commit the Go field still holds whatever the value was built with and none
// of it was encoded or signed, so reading it would be judging a leaf by bytes nobody sent.
func (self *LeafNode) validateLifetime(ctx *LeafValidationContext) error {
	if self.LeafNodeSource != LeafNodeSourceKeyPackage {
		return nil
	}
	// the documented opt out; see NowMs.
	if ctx.NowMs == 0 {
		return nil
	}
	// an interval whose end precedes its start contains no instant at all, so no clock and no
	// tolerance can make it current. Refused as itself rather than left to the two comparisons
	// below, which a skew wider than the inversion makes both accept.
	if self.Lifetime.NotAfter < self.Lifetime.NotBefore {
		return fmt.Errorf("%w: not_after %d precedes not_before %d",
			ErrLeafNodeLifetime, self.Lifetime.NotAfter, self.Lifetime.NotBefore)
	}
	// section 7.2: the endpoints are absolute times in SECONDS since the unix epoch, and this
	// context carries milliseconds, so both are divided down. Truncation costs at most a
	// second at each end and the skew is orders of magnitude larger.
	nowSeconds := ctx.NowMs / 1000
	skewSeconds := ctx.ClockSkewMs / 1000
	if self.Lifetime.NotBefore > skewSeconds && nowSeconds < self.Lifetime.NotBefore-skewSeconds {
		return fmt.Errorf("%w: not_before is %d and now is %d, tolerating %d seconds of skew",
			ErrLeafNodeLifetime, self.Lifetime.NotBefore, nowSeconds, skewSeconds)
	}
	if nowSeconds > skewSeconds && self.Lifetime.NotAfter < nowSeconds-skewSeconds {
		return fmt.Errorf("%w: not_after is %d and now is %d, tolerating %d seconds of skew",
			ErrLeafNodeLifetime, self.Lifetime.NotAfter, nowSeconds, skewSeconds)
	}
	return nil
}
