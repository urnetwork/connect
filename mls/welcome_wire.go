// The RFC 9420 section 12.4.3 GroupInfo and section 12.4.3.1 Welcome wire types and their
// codecs.
//
// Codecs ONLY. Signing a GroupInfo, building a Welcome and joining from one are the group
// lifecycle's; nothing here decides whether a GroupInfo's signature is good, whether a Welcome
// is addressed to this member, or whether the psks it names are ones this profile admits. They
// live in package mls rather than in the lifecycle plan because MLSMessage names *Welcome and
// *GroupInfo by direct type and package mls is one package -- the same split already applied to
// Proposal and Commit.
//
// Two of these structures are unlike anything else in this file's neighbourhood, and each is
// the reason for a decision below.
//
// GroupInfo is SIGNED, and what is signed is GroupInfo minus its own signature. That relation
// is held here by CONSTRUCTION rather than by two encoders that agree: GroupInfo.MarshalMLS
// encodes a GroupInfoTBS and then appends the signature, so there is exactly one description of
// the first four fields in the package. Two independent encoders is the shape of the classic
// defect -- a field the object carries and the preimage omits is a field the signature does not
// cover, and an attacker rewrites it in flight with the signature still verifying -- and a
// defect that lives in the DIFFERENCE between two field lists is one no round trip test can
// see, because each half round trips perfectly on its own.
//
// Welcome is parsed by a party who is not yet a member. There is no group state to check it
// against, no signature over it, and every length prefix in it was chosen by whoever sent it.
// So the decoder has to be safe on its own terms, and the way it gets there is by never reading
// past a region boundary itself: the secrets vector is syntax's ReadVector, which takes a
// BOUNDED sub reader over the declared region and decodes elements inside that, so an
// EncryptedGroupSecrets whose kem_output claims more bytes than the vector holds is refused
// rather than reaching forward into encrypted_group_info. A hand written loop over the parent
// reader is the same code with that one property removed.
package mls

import (
	"github.com/urnetwork/connect/mls/syntax"
)

// GroupInfo is the group's state at one epoch, signed by a member, and the structure a joiner
// is handed inside a Welcome's encrypted_group_info.
//
// Signer is the leaf index of the member whose signature key signed it; the signature covers
// GroupInfoTBS, which is every field but Signature itself.
type GroupInfo struct {
	GroupContext    GroupContext
	Extensions      []Extension
	ConfirmationTag []byte
	Signer          LeafIndex
	Signature       []byte
}

// toBeSigned is the preimage view of this object: the same four fields, in the same order, out
// of the same struct.
//
// This exists so that the preimage cannot be a SECOND opinion about what a GroupInfo carries.
// The signature covers whatever GroupInfoTBS encodes, so a field this object gains and the
// preimage does not is a field nobody's signature protects -- and since both halves would still
// round trip byte exactly on their own, nothing in a round trip suite would report it.
func (self *GroupInfo) toBeSigned() GroupInfoTBS {
	return GroupInfoTBS{
		GroupContext:    self.GroupContext,
		Extensions:      self.Extensions,
		ConfirmationTag: self.ConfirmationTag,
		Signer:          self.Signer,
	}
}

// MarshalMLS writes the to-be-signed prefix and then the signature.
//
// The delegation is the point rather than a saving of five lines: it makes "a GroupInfo is its
// own GroupInfoTBS followed by its signature" true at build time instead of a claim some test
// has to keep checking against two independently maintained field lists.
func (self *GroupInfo) MarshalMLS(w *syntax.Writer) error {
	tbs := self.toBeSigned()
	if err := tbs.MarshalMLS(w); err != nil {
		return err
	}
	w.WriteOpaque(self.Signature)
	return nil
}

// UnmarshalMLS decodes the five fields and publishes the receiver whole.
//
// Nothing is assigned until every field has been read, which is GroupContext's and
// PreSharedKeyId's convention and is here for their reason: a decoder that assigned as it read
// would leave a refused decode holding some fields from the new input and the rest from
// whatever the caller's value held before. For this type that is worse than untidy -- the
// result is a well formed GroupInfo describing an epoch nobody published, carrying a signature
// taken over a different one.
func (self *GroupInfo) UnmarshalMLS(r *syntax.Reader) error {
	decoded := GroupInfo{}
	if err := decoded.GroupContext.UnmarshalMLS(r); err != nil {
		return err
	}
	extensions, err := ReadExtensions(r)
	if err != nil {
		return err
	}
	decoded.Extensions = extensions
	confirmationTag, err := r.ReadOpaque()
	if err != nil {
		return err
	}
	decoded.ConfirmationTag = confirmationTag
	signer, err := r.ReadUint32()
	if err != nil {
		return err
	}
	decoded.Signer = LeafIndex(signer)
	signature, err := r.ReadOpaque()
	if err != nil {
		return err
	}
	decoded.Signature = signature
	*self = decoded
	return nil
}

// the C1 pin: drift between this type and the one codec convention fails at build.
var _ syntax.Codec = (*GroupInfo)(nil)

// GroupInfoTBS is the bytes SignWithLabel signs under the "GroupInfoTBS" label.
//
// Encode only. Nothing decodes a to-be-signed structure, and offering an UnmarshalMLS would
// invite a caller to RECONSTRUCT one from parsed fields instead of re-serializing the object it
// verified -- the same class of defect as two encoders, arriving one layer later: the bytes a
// verifier checks would be the bytes its own parser chose to write rather than the ones that
// came off the wire.
type GroupInfoTBS struct {
	GroupContext    GroupContext
	Extensions      []Extension
	ConfirmationTag []byte
	Signer          LeafIndex
}

// MarshalMLS encodes the four fields in RFC 9420 section 12.4.3 order.
//
// The leaf writes return nothing and are no ops after the first failure; the buffer error is
// collected by syntax.Marshal at Bytes. The error return carries WriteExtensions' semantic
// refusal, which is the only one this encoder has.
func (self *GroupInfoTBS) MarshalMLS(w *syntax.Writer) error {
	if err := self.GroupContext.MarshalMLS(w); err != nil {
		return err
	}
	if err := WriteExtensions(w, self.Extensions); err != nil {
		return err
	}
	w.WriteOpaque(self.ConfirmationTag)
	w.WriteUint32(uint32(self.Signer))
	return nil
}

// the pin this type's shape allows: it is a Marshaler and deliberately not a Codec, so the
// narrower interface is what says so at build time.
var _ syntax.Marshaler = (*GroupInfoTBS)(nil)

// PathSecret is the one field structure the RFC wraps a joiner's path secret in, which is what
// makes optional<PathSecret> a presence octet followed by a length prefixed value rather than a
// bare optional opaque.
type PathSecret struct {
	PathSecret []byte
}

func (self *PathSecret) MarshalMLS(w *syntax.Writer) error {
	w.WriteOpaque(self.PathSecret)
	return nil
}

func (self *PathSecret) UnmarshalMLS(r *syntax.Reader) error {
	pathSecret, err := r.ReadOpaque()
	if err != nil {
		return err
	}
	*self = PathSecret{PathSecret: pathSecret}
	return nil
}

var _ syntax.Codec = (*PathSecret)(nil)

// GroupSecrets is the per-joiner half of a Welcome: the epoch's joiner secret, the path secret
// the joiner needs when the commit that added it carried an UpdatePath, and the psks the epoch
// mixed in.
//
// Psks is always empty under the v1 profile, and the codec still round trips a populated vector
// on purpose: refusing a psk is the PROFILE's decision and not the codec's, and the mlswg
// messages family carries a group_secrets with the field present -- a codec that refused it
// would fail the family for a reason that is a policy rather than an encoding.
type GroupSecrets struct {
	JoinerSecret []byte
	PathSecret   *PathSecret
	Psks         []PreSharedKeyId
}

// The element codecs of the psks vector, as named functions rather than as closures at the call
// site -- extension.go's, tree.go's and commit_wire.go's spelling. A closure renders as its
// whole body wherever this package's source is read, and the gate that pins every syntax entry
// point to the default vector limit reads exactly that text: a named pair keeps one decision on
// one line where a reviewer can see which limit it runs under.
func writeOnePreSharedKeyId(w *syntax.Writer, item PreSharedKeyId) error {
	return item.MarshalMLS(w)
}

func readOnePreSharedKeyId(r *syntax.Reader) (PreSharedKeyId, error) {
	psk := PreSharedKeyId{}
	if err := psk.UnmarshalMLS(r); err != nil {
		return PreSharedKeyId{}, err
	}
	return psk, nil
}

func (self *GroupSecrets) MarshalMLS(w *syntax.Writer) error {
	w.WriteOpaque(self.JoinerSecret)
	err := w.WriteOptional(self.PathSecret != nil, func(w *syntax.Writer) error {
		return self.PathSecret.MarshalMLS(w)
	})
	if err != nil {
		return err
	}
	return syntax.WriteVector(w, self.Psks, writeOnePreSharedKeyId)
}

// UnmarshalMLS reads the joiner secret, the optional path secret, then the psks vector.
//
// The path secret is decoded into a local and attached only when the presence octet said
// present, so a GroupSecrets carrying none leaves PathSecret nil rather than pointing at a zero
// valued PathSecret. Commit.UnmarshalMLS makes the same distinction for the same reason, and
// here it decides whether the joiner seeds its direct path from a secret the committer sent or
// takes the tree's nodes as they stand.
func (self *GroupSecrets) UnmarshalMLS(r *syntax.Reader) error {
	joinerSecret, err := r.ReadOpaque()
	if err != nil {
		return err
	}
	pathSecret := &PathSecret{}
	present, err := r.ReadOptional(func(r *syntax.Reader) error {
		return pathSecret.UnmarshalMLS(r)
	})
	if err != nil {
		return err
	}
	psks, err := syntax.ReadVector(r, readOnePreSharedKeyId)
	if err != nil {
		return err
	}
	decoded := GroupSecrets{JoinerSecret: joinerSecret, Psks: psks}
	if present {
		decoded.PathSecret = pathSecret
	}
	*self = decoded
	return nil
}

var _ syntax.Codec = (*GroupSecrets)(nil)

// EncryptedGroupSecrets is one joiner's entry in a Welcome: the KeyPackageRef naming which
// published key package it is addressed to, and the GroupSecrets sealed to that key package's
// init key.
type EncryptedGroupSecrets struct {
	NewMember             []byte
	EncryptedGroupSecrets HpkeCiphertext
}

func (self *EncryptedGroupSecrets) MarshalMLS(w *syntax.Writer) error {
	w.WriteOpaque(self.NewMember)
	return self.EncryptedGroupSecrets.MarshalMLS(w)
}

// UnmarshalMLS reads the reference and then the ciphertext, publishing the receiver whole.
//
// The staging is not decoration on this one. A joiner scans the secrets vector for the entry
// its own key package reference matches, so a half filled entry left behind by a refused decode
// is an entry that can still be MATCHED -- a reference out of these bytes paired with a
// ciphertext out of whatever the reused value held before.
func (self *EncryptedGroupSecrets) UnmarshalMLS(r *syntax.Reader) error {
	newMember, err := r.ReadOpaque()
	if err != nil {
		return err
	}
	decoded := EncryptedGroupSecrets{NewMember: newMember}
	if err := decoded.EncryptedGroupSecrets.UnmarshalMLS(r); err != nil {
		return err
	}
	*self = decoded
	return nil
}

var _ syntax.Codec = (*EncryptedGroupSecrets)(nil)

// Welcome is what a joiner is handed by whoever committed its Add: the suite the group runs,
// one sealed GroupSecrets per joiner, and the group's GroupInfo sealed under the welcome key
// and nonce that WelcomeKeyNonce derives from the epoch's welcome_secret.
//
// CipherSuite appears here as well as inside the sealed GroupInfo's GroupContext, and that is
// not redundancy the codec may drop: the joiner needs it to pick a KDF and an AEAD BEFORE it
// can open either ciphertext, so it is the one field of a Welcome that is necessarily in the
// clear. A receiver still has to hold the two readings against each other once it has opened
// the group info, which is the lifecycle's check and not this file's.
type Welcome struct {
	CipherSuite        CipherSuite
	Secrets            []EncryptedGroupSecrets
	EncryptedGroupInfo []byte
}

// The element codecs of the secrets vector, named for the reason the psks pair above is.
func writeOneEncryptedGroupSecrets(w *syntax.Writer, item EncryptedGroupSecrets) error {
	return item.MarshalMLS(w)
}

func readOneEncryptedGroupSecrets(r *syntax.Reader) (EncryptedGroupSecrets, error) {
	encrypted := EncryptedGroupSecrets{}
	if err := encrypted.UnmarshalMLS(r); err != nil {
		return EncryptedGroupSecrets{}, err
	}
	return encrypted, nil
}

func (self *Welcome) MarshalMLS(w *syntax.Writer) error {
	w.WriteUint16(uint16(self.CipherSuite))
	err := syntax.WriteVector(w, self.Secrets, writeOneEncryptedGroupSecrets)
	if err != nil {
		return err
	}
	w.WriteOpaque(self.EncryptedGroupInfo)
	return nil
}

// UnmarshalMLS decodes a Welcome out of bytes nobody has authenticated.
//
// This is the least trusted decode in the package. A joiner runs it before it holds any group
// state, so there is nothing to check the result against, and every length prefix in the input
// was chosen by whoever sent it. Three things carry that, and none of them is written here:
// ReadVector takes a bounded sub reader over the secrets region so a nested length cannot
// overrun into encrypted_group_info; ReadOpaque validates a declared length against the
// configured maximum and then against the bytes actually remaining BEFORE it sizes any
// allocation, so a prefix claiming a gibibyte costs nothing; and syntax.Unmarshal joins this
// method's answer with Done, so a Welcome with a tail is refused rather than silently accepted
// as a second encoding of the same object.
//
// The receiver is published whole for the reason the entry decoder above states, one level up.
func (self *Welcome) UnmarshalMLS(r *syntax.Reader) error {
	cipherSuite, err := r.ReadUint16()
	if err != nil {
		return err
	}
	secrets, err := syntax.ReadVector(r, readOneEncryptedGroupSecrets)
	if err != nil {
		return err
	}
	encryptedGroupInfo, err := r.ReadOpaque()
	if err != nil {
		return err
	}
	*self = Welcome{
		CipherSuite:        CipherSuite(cipherSuite),
		Secrets:            secrets,
		EncryptedGroupInfo: encryptedGroupInfo,
	}
	return nil
}

var _ syntax.Codec = (*Welcome)(nil)
