// The credential a leaf node carries, and the one credential type this profile admits.
//
// This file belongs to p5 task 4A, which task 5 could not compile without: LeafNode embeds a
// Credential by value, so the structure has to exist before the leaf that carries it does. It
// landed in the same commit as leaf_node.go for that reason and for no other, and it is the
// plan's task 4A code rather than an improvisation, so that task 4A finds it already here.
package mls

import (
	"errors"

	"github.com/urnetwork/connect/mls/syntax"
)

// errProfileCredentialType is ErrProfileCredentialType in the validation plan's catalogue, and
// that plan owns the single declaration site for the exported name. It has not landed in this
// package yet, so the refusal is carried by this unexported value until it does.
//
// Unexported is the whole point of the shape, and the argument is extension.go's, made for
// errMissingRequiredCapability and psk.go's for its ValSem sentinels. An exported
// ErrProfileCredentialType declared here would be a second public declaration site for a name
// the validation plan also declares, the two would not be the same value, and a caller matching
// one would silently stop matching the other. A name that cannot be reached from outside this
// package cannot be depended on from outside it either, so the swap costs nobody else anything,
// and every consumer of this refusal in the tasks after this one is inside package mls.
//
// TestNoValidationOwnedNameHasLandedBesideItsStandIn derives the owed pair from the package's
// own declarations and fails on the commit that lands the real name.
var errProfileCredentialType = errors.New("mls: credential type is outside the v1 profile")

// Credential is RFC 9420 section 5.3. BasicCredential only in v1.
//
// x509 is refused at PARSE rather than by a later check, so no x509 bytes are ever carried
// inside a LeafNode this package accepted (Spec A section 3.2). A refusal made one layer up
// would leave a window in which the certificate chain sits inside an accepted structure, and
// every later reader of that structure would be entitled to assume somebody had looked at it.
type Credential struct {
	CredentialType CredentialType
	Identity       []byte
}

// BasicCredential builds the one credential this profile constructs.
//
// The identity is COPIED rather than retained. The caller's array is usually a slice of a
// larger buffer it goes on writing into, and a credential that aliased it would change under
// the leaf that carries it -- which is a signature that verified when it was made and does not
// afterwards, with nothing in between to point at. It is also the property
// TestEveryConstructionInThisPackageLeavesItsInputAlone holds every construction of this
// package handed a caller's bytes to.
func BasicCredential(identity []byte) Credential {
	return Credential{CredentialType: CredentialTypeBasic, Identity: cloneBytes(identity)}
}

// MarshalMLS encodes the section 5.3 structure: a uint16 credential_type then, for a basic
// credential, opaque identity<V>.
//
// The refusal is a semantic one rather than a buffer failure, which is the reason this returns
// an error at all (C2): under a return free encoder it would have to panic or be dropped, and a
// dropped encoder refusal produces wrong signed bytes rather than a failure.
func (self *Credential) MarshalMLS(w *syntax.Writer) error {
	if self.CredentialType != CredentialTypeBasic {
		return errProfileCredentialType
	}
	w.WriteUint16(uint16(self.CredentialType))
	w.WriteOpaque(self.Identity)
	return nil
}

// UnmarshalMLS decodes one credential, consuming exactly its own two fields, because it runs
// inside a LeafNode whose remaining bytes belong to the fields after it.
//
// The type is refused before the identity is read, so a chain of certificates never reaches an
// allocation this package made on its behalf.
func (self *Credential) UnmarshalMLS(r *syntax.Reader) error {
	credentialType, err := r.ReadUint16()
	if err != nil {
		return err
	}
	if CredentialType(credentialType) != CredentialTypeBasic {
		return errProfileCredentialType
	}
	identity, err := r.ReadOpaque()
	if err != nil {
		return err
	}
	self.CredentialType = CredentialTypeBasic
	self.Identity = identity
	return nil
}

// the C1 pin: drift between this type and the one codec convention fails at build.
var _ syntax.Codec = (*Credential)(nil)
