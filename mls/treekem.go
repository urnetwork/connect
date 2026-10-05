// The path secret ladder of RFC 9420 section 7.4, the node key pair each rung derives, and the
// private half of the tree one member holds.
//
// The whole security argument of TreeKEM is an ASYMMETRY, and it is what the tests here are
// built against rather than the round trip. A member handed the path secret of a node can derive
// every node above it -- one DeriveSecret per level, all the way to the root -- and can derive
// nothing below it, because the derivation runs one way and there is no inverse. That is exactly
// what makes it safe to seal one secret to a whole subtree: everybody under the copath child
// learns the path from that node up and learns nothing about the sender's own leaf. A ladder
// that ran the other way would still be a ladder, would still round trip, would still produce
// keys that match at every node, and would hand every member of a subtree the secrets of the
// members below them.
//
// Two constructions and one container, and the thing that is deliberately NOT here:
//
// The leaf's own HPKE key pair is not derived from path_secret[0]. Everyone who can open the
// first ciphertext of an UpdatePath learns path_secret[0], so a leaf key derived from it would
// be the sender's leaf private key handed to the whole copath. It is sampled independently in
// task 19, and the difference is invisible to every test that only checks that keys match --
// both versions produce a private state whose public halves agree with the tree.
//
// path_secret[n] is one rung PAST the root, and it is not an off-by-one. RFC 9420 section 8.1
// makes the value beyond the last path node the commit secret of the epoch, so a caller that
// asked for exactly as many rungs as it has nodes would have to derive that one somewhere else
// -- which is one more place for the ladder to be written differently.
//
// Nothing here is serialized by this package. The state store seals a TreeKEMPrivate (spec A
// section 3.5), and it holds no codec of its own precisely so that the sealing is the only way
// it reaches a disk.
package mls

import (
	"crypto/subtle"
	"errors"
	"fmt"

	"github.com/urnetwork/connect/mls/syntax"
)

// DerivePathSecrets is the ladder of RFC 9420 section 7.4: path_secret[0] is the initial secret
// and path_secret[n] is DeriveSecret(path_secret[n-1], "path").
//
// count+1 values, one per node of the filtered direct path plus the rung beyond the root that
// section 8.1 makes the commit secret.
//
// A nil provider stops the process rather than answering. This construction has no error to
// return -- the interface the plan pins has none -- and with count of zero it never touches the
// provider at all, so the alternative to stopping is handing back a one entry ladder that was
// derived from nothing while looking exactly like one that was. That is the same choice
// crypto.go's Random and Expand make for the same reason: a value of the right shape that no
// provider produced is worse than a stop.
//
// The initial rung is CLONED. It is the caller's array -- in task 22 it is the plaintext an
// HpkeOpen just produced, and in task 18 it is fresh from Random -- and this answer outlives the
// call: it becomes the private state of an epoch. A ladder aliasing its caller's first rung is a
// group whose path secret changes when that buffer is reused.
func DerivePathSecrets(crypto CryptoProvider, initial []byte, count int) [][]byte {
	if crypto == nil {
		panic("mls: DerivePathSecrets was handed no crypto provider, and it has no way to say so")
	}
	out := make([][]byte, 0, count+1)
	out = append(out, cloneBytes(initial))
	for i := 0; i < count; i += 1 {
		out = append(out, crypto.DeriveSecret(out[len(out)-1], "path"))
	}
	return out
}

// DeriveNodeKeyPair is RFC 9420 section 7.4's node key: node_secret = DeriveSecret(path_secret,
// "node"), and the KEM key pair is derived from THAT rather than from the path secret itself.
//
// The intermediate secret is the whole of what this function is. Handing the path secret
// straight to DeriveKeyPair produces a key pair that is deterministic, that differs at every
// rung of the ladder, and that every member holding the path secret agrees on -- which is every
// property a round trip or a determinism test can see, and it is the wrong key at every node in
// the group. Only a comparison against an independent derivation separates the two, which is why
// the published TreeKEM corpus is what holds this and not a self-consistency check.
func DeriveNodeKeyPair(crypto CryptoProvider, pathSecret []byte) (HpkePrivateKey, HpkePublicKey, error) {
	if crypto == nil {
		return nil, nil, ErrNilCryptoProvider
	}
	return crypto.DeriveKeyPair(crypto.DeriveSecret(pathSecret, "node"))
}

// TreeKEMPrivate is what one member holds privately about the ratchet tree: its own leaf HPKE
// key, and the path secrets it has learned, keyed by the node each secret belongs to.
//
// Keyed by node and not by position in a path, which is the difference that matters when the
// tree changes shape. A member's filtered direct path gains and loses nodes as members are added
// and removed, so a slice indexed by depth silently renames every secret it holds the first time
// the path is filtered differently; a map keyed by the node index says what each secret is FOR.
type TreeKEMPrivate struct {
	LeafIndex      LeafIndex
	EncryptionPriv HpkePrivateKey
	PathSecrets    map[NodeIndex][]byte
	// erased once Zeroize has run, so a state whose key material is gone REFUSES rather than
	// answering out of it. (*SecretTree).Zeroize carries the same flag for the same reason and
	// its comment states it: without the flag the leaf arm below answers a zero length private
	// key with a nil error, and every caller downstream reports a decryption failure against a
	// message that is perfectly well formed.
	erased bool
}

// errTreeKEMPrivateErased is what an erased private state answers instead of key material.
//
// It is declared beside the type rather than in tree_errors.go with the exported ones because
// no caller outside this package can reach the erase: (*StagedCommit).Zeroize and
// (*Group).Close are the two paths that run it, and both drop the state in the same statement
// list.
var errTreeKEMPrivateErased = errors.New("mls: this member's private tree state has been erased")

// NewTreeKEMPrivate is the state of a member that holds its leaf key and no path secret yet,
// which is what a joiner has before it has processed a commit.
//
// The leaf private key is copied for DerivePathSecrets' reason: it is the caller's array and
// this state outlives the call.
func NewTreeKEMPrivate(i LeafIndex, encryptionPriv HpkePrivateKey) *TreeKEMPrivate {
	return &TreeKEMPrivate{
		LeafIndex:      i,
		EncryptionPriv: cloneBytes(encryptionPriv),
		PathSecrets:    map[NodeIndex][]byte{},
	}
}

// Clone is a deep copy, and the depth is the point.
//
// A commit is computed against a candidate epoch and may then be rejected -- by the delivery
// service, by a validation failure, or by losing a race with another member's commit -- so the
// caller works on a clone and keeps the original. A clone sharing one backing array with its
// parent is a rejected commit that has already written through to the state the group is still
// running on, and neither the map nor the leaf key may alias for that reason.
func (self *TreeKEMPrivate) Clone() *TreeKEMPrivate {
	out := NewTreeKEMPrivate(self.LeafIndex, self.EncryptionPriv)
	for x, secret := range self.PathSecrets {
		out.PathSecrets[x] = cloneBytes(secret)
	}
	// the erase travels with the copy, because a clone of an erased state holds a clone of
	// erased storage: without this line the copy answers zero length keys with a nil error,
	// which is the one outcome the flag exists to prevent.
	out.erased = self.erased
	return out
}

// Zeroize erases the private key material this state holds: the leaf's own HPKE private key and
// every path secret it has derived, in place, and then refuses.
//
// THIS TYPE IS KEY MATERIAL STORAGE AND ITS ERASE IS ITS OWN, which is the discipline
// (*KeySchedule).Zeroize and (*SecretTree).Zeroize are written under and the one a StagedCommit
// could not take until this existed: the committer draws a fresh leaf private key for the epoch
// its commit opens, and a commit the delivery service never accepted is dropped with that key
// still in it.
//
// The path secrets are DELETED as well as erased. The map is the only thing that says which
// nodes this member can derive a key for, so leaving zeroed entries in it would leave a state
// that reports it holds a key for every node of its direct path and answers zeros for each --
// which the refusal above catches, but only for a caller that reaches this type's own doors.
//
// The noinline directive is secret_zeroize.go's reason and this package's erase class rule: the
// stores this method makes are dead in the compiler's reading, and every erase of the class
// carries the directive rather than reasoning its way out of it.
//
//go:noinline
func (self *TreeKEMPrivate) Zeroize() {
	// nil accepted rather than guarded against at the call site, for zeroizeSecret's reason: a
	// group that never drew a private state and a staged commit that carries none are both
	// ordinary, and a guard needed at every drop site is one that will be missing at one of them.
	if self == nil {
		return
	}
	zeroizeSecret(self.EncryptionPriv)
	for x, secret := range self.PathSecrets {
		zeroizeSecret(secret)
		delete(self.PathSecrets, x)
	}
	self.erased = true
}

// NodePrivateKey answers the HPKE private key for a node if this member can derive one: its own
// leaf, or any node it holds a path secret for.
//
// A (value, ok) shape rather than ErrNoPathSecret, because "this member is not on that part of
// the path" is the ORDINARY condition for every member off the sender's path and not a fault.
// The sentinel belongs to the caller that was expecting to hold one -- task 22's decrypt, which
// has already found a ciphertext addressed to a node it cannot open -- and returning it from
// here would make every ordinary miss look like that failure.
//
// The own-leaf arm is checked first and answers the stored key rather than deriving one. The
// leaf key pair is NOT a rung of the ladder (see the file header), so there is no path secret it
// could be derived from, and a body that fell through to the map for it would answer "not
// available" for the one key every member always has.
//
// Both arms answer FRESH storage, and the own-leaf arm is the one that has to be made to. The
// derived arm hands back what DeriveKeyPair just produced and shares nothing by construction;
// the own-leaf arm is reading a field, and returning it directly hands the caller the state's
// live leaf key. That makes one function answer two different ownership contracts, which is a
// caller that cannot have a policy: this package ships zeroizeSecret for exactly this material
// and task 22 is the consumer, so "erase the private key when the decrypt is done" -- the right
// thing to do with every OTHER answer this function gives -- would erase the member's own leaf
// key and leave it unable to decrypt anything again, with every rung still deriving and nothing
// to point at. NewTreeKEMPrivate and Clone both copy this same array for the mirror of this
// reason; the exit door owes the entry doors their contract.
func (self *TreeKEMPrivate) NodePrivateKey(crypto CryptoProvider, x NodeIndex) (HpkePrivateKey, bool, error) {
	if crypto == nil {
		return nil, false, ErrNilCryptoProvider
	}
	// refused rather than answered out of erased storage, which is (*SecretTree).Zeroize's rule:
	// erasing and refusing are one operation, because the alternative here is a zero length leaf
	// private key handed back with a nil error and held by every caller as a key.
	if self.erased {
		return nil, false, errTreeKEMPrivateErased
	}
	if x == self.LeafIndex.NodeIndex() {
		return cloneBytes(self.EncryptionPriv), true, nil
	}
	secret, ok := self.PathSecrets[x]
	if !ok {
		return nil, false, nil
	}
	priv, _, err := DeriveNodeKeyPair(crypto, secret)
	if err != nil {
		return nil, false, err
	}
	return priv, true, nil
}

// Consistent checks that every path secret this member holds derives the public key the tree
// carries at that node, and that the tree has a leaf at this member's index at all.
//
// This is what catches a private state and a tree that have drifted an epoch apart, and it has
// to be checked rather than assumed because the symptom otherwise arrives one commit later and
// somewhere else: a member whose path secrets belong to the previous epoch derives private keys
// nobody's public half matches, decrypts nothing, and reports a decryption failure against a
// commit that is perfectly well formed.
//
// The comparison is crypto/subtle.ConstantTimeCompare and not bytes.Equal. What is compared here
// is public -- both halves are encryption keys the tree publishes -- so nothing leaks either
// way, and the reason is the class rather than this line: a variable time comparator written
// here is one edit away from being pointed at a secret.
//
// Two gates read this line, and which ones is written down because the sentence that used to
// stand here claimed a coverage nothing provided -- this function was under no comparator gate
// at all, and bytes.Equal here left the whole tree green.
// TestNothingThisPackageShipsComparesDataOutsideConstantTime bans the derived comparator class
// in every function of every production file, which is the half that reads EVERY call site;
// TestEveryKeyQuestionOverTheRatchetTreeIsAnsweredInConstantTime is the narrower half that also
// bans the shape naming no comparator at all, string(a) == string(b), and this function is in
// its class because it holds a key of its own and answers over a tree.
//
// It deliberately does NOT re-derive the leaf public key from EncryptionPriv, and the reason is
// this method's SCOPE rather than the absence of the operation. The sentence that used to stand
// here -- "the CryptoProvider surface has no private-to-public operation" -- is true of the
// INTERFACE and was read as "so it cannot be asked", which is what left the join door installing a
// joiner's encryption half against nothing. hpkePublicKeyOf is a package level derivation outside
// the interface, on signaturePublicKeyOf's precedent, and (*Group).JoinFromWelcome holds the
// joiner's own two halves together through it. What is judged HERE is the path secrets against the
// tree; the leaf key pair is judged where both halves exist -- in task 22's DecryptUpdatePath,
// which compares each derived public key against the one the UpdatePath carries, and at the join
// door for the leaf a joiner brings in with it.
func (self *TreeKEMPrivate) Consistent(crypto CryptoProvider, tree *RatchetTree) error {
	if crypto == nil {
		return ErrNilCryptoProvider
	}
	if tree == nil || tree.Leaf(self.LeafIndex) == nil {
		return ErrPathSecretMismatch
	}
	for x, secret := range self.PathSecrets {
		parent := tree.ParentAt(x)
		if parent == nil {
			return ErrPathSecretMismatch
		}
		_, pub, err := DeriveNodeKeyPair(crypto, secret)
		if err != nil {
			return err
		}
		if subtle.ConstantTimeCompare(pub, parent.EncryptionKey) != 1 {
			return ErrPathSecretMismatch
		}
	}
	return nil
}

// UpdatePathPlan is everything the sender computed before any encryption happened: the filtered
// direct path it is about to publish, one path secret and one public key per node of it, the
// re-signed leaf, the epoch's commit secret, and the private state the sender keeps.
//
// It exists because generation is SPLIT, and the split is an ordering constraint rather than a
// style. The HPKE encryption context task 20 seals each path secret under is the serialized
// GroupContext of the epoch the commit OPENS, whose tree_hash is the hash of the tree with these
// public keys and this leaf already installed. So the public half has to land in the tree before
// the context exists, and a single call that did both would either encrypt under the previous
// epoch's context -- ciphertexts every peer rejects, and invisible to any test that does not
// distinguish the two contexts -- or have to compute a tree hash over a tree it had not finished
// mutating.
//
// Nothing here is serialized. UpdatePath, the wire type, is task 19's and carries the public keys
// and the ciphertexts alone; the path secrets in this structure never leave the sender.
type UpdatePathPlan struct {
	Path         []NodeIndex
	PathSecrets  [][]byte
	PublicKeys   []HpkePublicKey
	LeafNode     *LeafNode
	CommitSecret []byte
	Private      *TreeKEMPrivate
}

// Zeroize erases the secret half of the plan: every path secret on the ladder and the commit
// secret past the root.
//
// PRIVATE IS DELIBERATELY NOT ERASED HERE, and that is the one thing worth reading this method
// for. The private state a plan carries is the leaf state a MERGE INSTALLS as the group's own --
// (*Group).MergePendingCommit assigns it to self.ownPriv -- so a plan that erased it would erase
// the key the epoch it opened runs on, on the ordinary path. The holder that DROPS the plan
// erases it instead: (*StagedCommit).Zeroize hands ownPriv, which is the same pointer, to
// (*TreeKEMPrivate).Zeroize, and a merge erases the leaf state it replaced rather than the one
// it installed.
//
// The noinline directive is the erase class's rule; see (*TreeKEMPrivate).Zeroize.
//
//go:noinline
func (self *UpdatePathPlan) Zeroize() {
	if self == nil {
		return
	}
	for _, secret := range self.PathSecrets {
		zeroizeSecret(secret)
	}
	zeroizeSecret(self.CommitSecret)
}

// CreateUpdatePathSecrets is the secret and public half of RFC 9420 section 7.5's UpdatePath
// generation. It MUTATES the tree: it blanks the sender's direct path, installs the fresh public
// keys and the parent hash chain on the filtered path, and installs the re-signed leaf. After it
// returns, TreeHash is the NEW epoch's tree hash and the caller can build the GroupContext task
// 20 encrypts under.
//
// The parent hashes are assigned from the ROOT DOWN, which is the order section 7.9 requires and
// not a preference. The parent hash of a node is taken over that node's own parent_hash field, so
// the value at the top of the filtered path -- the zero-length octet string section 7.9 gives the
// root -- has to be final before the node below it can be hashed, and so on down to the leaf. A
// loop that ran the other way would hash a placeholder field at every level and produce a chain
// that is full length, stable, self-consistent and rejected by every joiner. Each node's copath
// child comes from the PathStep rather than being re-derived here, so this walk and task 21's
// receiving walk are provably over the same children.
//
// The leaf's own key pair is sampled independently and is NOT the first rung of the ladder.
// Everyone who can open the first ciphertext of the UpdatePath learns path_secret[0], so a leaf
// key derived from it is the sender's leaf private key handed to its whole copath -- and the two
// versions are indistinguishable to any test that only checks that the private state's public
// halves agree with the tree.
//
// CommitSecret is the rung PAST the root, section 8.1, which is why DerivePathSecrets answers one
// more value than there are nodes.
//
// It is ATOMIC. Every mutation is made on a working copy and the receiver adopts it in a single
// assignment at the end, so a call that answers an error leaves the caller's tree exactly as it
// found it. Ordering the refusals ahead of the mutation is not enough on its own and this is not
// a hypothetical: leaf.Sign is reachable only after the direct path has been blanked, the new
// public keys installed and the parent hash chain written, so a signature private key that does
// not match the ciphersuite used to answer an error over a tree whose tree hash had already moved
// and which VerifyParentHashes then refused. A caller cannot repair that -- the previous epoch's
// keys are gone -- so the only safe contract is that a failure is a no-op, and task 20 is a
// caller that would otherwise have to know to throw the tree away.
func (self *RatchetTree) CreateUpdatePathSecrets(crypto CryptoProvider, sender LeafIndex,
	signer SignaturePrivateKey, groupId []byte) (*UpdatePathPlan, error) {
	// before anything is read off the receiver, for the reason the nil provider gate states:
	// every secret, key and hash below comes through this provider, and a caller that passed
	// none is told that rather than being sent to look at a tree that is not the problem.
	// bare rather than wrapped with a reason, which is DeriveNodeKeyPair's spelling two
	// declarations up and is not a style choice here: the shape of this signature puts it inside
	// TestEveryKeyQuestionOverTheRatchetTreeIsAnsweredInConstantTime's derived class, and that
	// gate bans every call out of this package from a member of it.
	if crypto == nil {
		return nil, ErrNilCryptoProvider
	}
	// the two ways a sender has no leaf here are two different faults and are answered as two,
	// which is the range check's whole reason for being written out rather than left to Leaf's
	// nil. An index past the width is a caller that computed an index wrong and repairs it by
	// recomputing one; a blank slot inside the tree is a member committing from a position the
	// group REMOVED, whose index was right the whole time and whose repair is to rejoin. One
	// sentinel for both told the second to fix the thing that was not broken.
	if LeafCount(sender) >= self.LeafWidth() {
		return nil, ErrLeafIndexOutOfRange
	}
	// the working copy the atomicity note above is about. Everything below mutates this and the
	// receiver adopts it in one assignment once the last thing that can fail has succeeded.
	working := self.Clone()
	current := working.Leaf(sender)
	if current == nil {
		return nil, ErrLeafBlank
	}
	// computed before the direct path is blanked: blanking the sender's own direct path cannot
	// change any copath child's resolution, so the filtered path is the same either way, and
	// computing it first keeps the failure ahead of the mutation.
	steps, err := working.filteredPathSteps(sender)
	if err != nil {
		return nil, err
	}
	path := make([]NodeIndex, 0, len(steps))
	for _, step := range steps {
		path = append(path, step.Node)
	}
	// the ladder: one secret per filtered node, plus the rung past the root.
	ladder := DerivePathSecrets(crypto, crypto.Random(crypto.HashSize()), len(path))
	pathSecrets := ladder[:len(path)]
	commitSecret := ladder[len(ladder)-1]

	publicKeys := make([]HpkePublicKey, len(path))
	private := NewTreeKEMPrivate(sender, nil)
	for i := range path {
		_, pub, err := DeriveNodeKeyPair(crypto, pathSecrets[i])
		if err != nil {
			return nil, err
		}
		publicKeys[i] = pub
		private.PathSecrets[path[i]] = cloneBytes(pathSecrets[i])
	}

	// the leaf key pair is sampled independently: deriving it from path_secret[0] would hand the
	// sender's leaf private key to everyone who decrypts that secret.
	leafPriv, leafPub, err := crypto.DeriveKeyPair(crypto.Random(crypto.HashSize()))
	if err != nil {
		return nil, err
	}
	private.EncryptionPriv = cloneBytes(leafPriv)

	if err := working.BlankDirectPath(sender); err != nil {
		return nil, err
	}
	// installed BEFORE the parent hashes are taken, and the order is load bearing twice over.
	// The parent hash of a node is a hash of that node's encryption_key, so a chain computed
	// against the blanked tree would refuse outright; and the tree hash the caller reads after
	// this call is the new epoch's only because these keys are in it.
	for i, x := range path {
		if err := working.SetParent(x, &ParentNode{EncryptionKey: publicKeys[i]}); err != nil {
			return nil, err
		}
	}

	// the chain, root down. carried is the parent hash of the node ABOVE the one being written,
	// and it starts as the zero-length octet string section 7.9 gives the top of the path.
	carried := []byte{}
	for i := len(steps) - 1; i >= 0; i -= 1 {
		parent := working.ParentAt(steps[i].Node)
		if parent == nil {
			return nil, ErrTreeMalformed
		}
		parent.ParentHash = carried
		hash, err := working.ParentHash(crypto, steps[i].Node, steps[i].CopathChild)
		if err != nil {
			return nil, err
		}
		carried = hash
	}

	// the leaf carries the parent hash of the LOWEST node of the filtered path, section 7.9.1,
	// and it is a commit leaf so that the field is inside what the signature covers. In a group
	// of one the path is empty and carried is still the zero-length octet string, which is the
	// conforming answer rather than an omission: there is no parent node for the leaf to claim.
	leaf := current.Clone()
	leaf.EncryptionKey = leafPub
	leaf.LeafNodeSource = LeafNodeSourceCommit
	leaf.ParentHash = carried
	if err := leaf.Sign(crypto, signer, groupId, sender); err != nil {
		return nil, err
	}
	if err := working.SetLeaf(sender, leaf); err != nil {
		return nil, err
	}

	// the adoption, and the last statement that can be reached by a failure is above it. A
	// caller holding this tree sees the previous epoch or the new one and never a half of both.
	self.nodes = working.nodes

	return &UpdatePathPlan{
		Path:         path,
		PathSecrets:  pathSecrets,
		PublicKeys:   publicKeys,
		LeafNode:     leaf,
		CommitSecret: commitSecret,
		Private:      private,
	}, nil
}

// ---------------------------------------------------------------------------
// task 19: the UpdatePath wire types
// ---------------------------------------------------------------------------

// errNilHpkeCiphertext is OpenWithLabel handed no ciphertext at all.
//
// Unexported, and that is a decision rather than an omission. Nothing outside this package can
// produce the condition: an HpkeCiphertext only ever reaches an open as an element of an
// UpdatePathNode's vector, which decoding builds by value and never as a nil pointer, so the
// only way here is an in-package caller that lost track of a pointer. It is also what
// TestEveryExportedErrorOfThisPackageIsInAMaintainedClass demands -- an exported Err in a file
// with no maintained error class is one no exclusivity sweep of this package judges, and this
// file has no class.
//
// An error rather than the nil dereference the plan's body would take, for guardrail 7's reason
// one layer up: an open answers a failure as an error and never as a plaintext, and a panic in a
// routine fed by the network is a failure no caller can answer at all.
var errNilHpkeCiphertext = errors.New("mls: no hpke ciphertext was supplied")

// HpkeCiphertext is RFC 9420 section 6.1: one HPKE encryption, the KEM output and the AEAD
// ciphertext, in the order the RFC writes them.
//
//	struct { opaque kem_output<V>; opaque ciphertext<V>; } HPKECiphertext;
//
// ONE declaration and not two. The framing plan's Welcome encrypts its group secrets into
// exactly this structure, and a second copy declared there would be two Go types that agree on
// the wire today and disagree the moment either gains a field -- which is the only reason a
// second declaration ever gets written. So Welcome consumes this one.
//
// The two fields are both []byte and adjacent, so a codec that wrote them in the other order
// compiles, round trips, and re-encodes byte exact against the published corpus. Only a golden
// derived from the RFC separates the two orders, which is what
// TestHpkeCiphertextMarshalMatchesTheHandDerivedGolden is.
type HpkeCiphertext struct {
	KemOutput  []byte
	Ciphertext []byte
}

func (self *HpkeCiphertext) MarshalMLS(w *syntax.Writer) error {
	w.WriteOpaque(self.KemOutput)
	w.WriteOpaque(self.Ciphertext)
	return nil
}

// UnmarshalMLS decodes both fields before either is assigned, which is leaf_node.go's staged
// decode at this scale and holds for the same two reasons: a refused decode leaves the receiver
// exactly as it found it, and a receiver decoded into twice answers the second encoding rather
// than a mixture of both.
func (self *HpkeCiphertext) UnmarshalMLS(r *syntax.Reader) error {
	kemOutput, err := r.ReadOpaque()
	if err != nil {
		return err
	}
	ciphertext, err := r.ReadOpaque()
	if err != nil {
		return err
	}
	self.KemOutput = kemOutput
	self.Ciphertext = ciphertext
	return nil
}

// the C1 pin: drift between this type and the one codec convention fails at build.
var _ syntax.Codec = (*HpkeCiphertext)(nil)

// The element halves of encrypted_path_secret<V>, named rather than written as closures at
// the call site.
//
// That is extension.go's and tree.go's spelling for the same shape, and here it is not only
// consistency: TestEverySyntaxEncoderInThisPackageUsesTheDefaultLimit pins every syntax entry
// point of this package by the SOURCE TEXT of the call, so a closure written inline puts its
// whole body into that table and every later edit to the body is a diff in a limit gate.
func writeOneHpkeCiphertext(w *syntax.Writer, ct HpkeCiphertext) error {
	return ct.MarshalMLS(w)
}

func readOneHpkeCiphertext(r *syntax.Reader) (HpkeCiphertext, error) {
	var ct HpkeCiphertext
	err := ct.UnmarshalMLS(r)
	return ct, err
}

// SealWithLabel is the HpkeCiphertext-shaped form of crypto_labels.go's flat pair.
//
// It lives here rather than there because that file must not name a TreeKEM type -- the crypto
// layer is written so it can be read without the tree -- and because this is the file that owns
// the struct. It is an adaptation of two return values into one structure and deliberately not a
// second HPKE implementation: everything that decides interoperability, the label framing and
// the empty aad, stays in the one place crypto_labels.go's own corpus test holds.
//
// The ORDER of the two results is the whole of what can go wrong here. EncryptWithLabel answers
// (kemOutput, ciphertext) and both are []byte, so a transposed adaptation compiles and is
// invisible to a seal-then-open round trip through this same pair of functions. What separates
// them is the LENGTHS -- the KEM output is the suite's Nenc and the ciphertext is the plaintext
// plus the AEAD tag -- which is what
// TestSealWithLabelPutsTheKemOutputAndTheCiphertextInTheirOwnFields asserts.
func SealWithLabel(crypto CryptoProvider, pub HpkePublicKey, label string,
	context []byte, plaintext []byte) (*HpkeCiphertext, error) {
	kemOutput, ciphertext, err := EncryptWithLabel(crypto, pub, label, context, plaintext)
	if err != nil {
		return nil, err
	}
	return &HpkeCiphertext{KemOutput: kemOutput, Ciphertext: ciphertext}, nil
}

// OpenWithLabel is the inverse, and it forwards to DecryptWithLabel rather than reimplementing
// it so that the "no fallback info" rule that file argues for holds here by construction.
func OpenWithLabel(crypto CryptoProvider, priv HpkePrivateKey, label string,
	context []byte, ct *HpkeCiphertext) ([]byte, error) {
	// the provider first and the ciphertext second, and the order is required rather than
	// arbitrary. It is what TestEveryDeclarationHandedANilProviderRefusesRatherThanDereferencingIt
	// demands of every construction taking a provider, and it is the right order anyway: a caller
	// that passed no provider passed nothing this function could have used, and answering it
	// about the ciphertext sends it to look at the argument that is not the problem. Bare rather
	// than wrapped with a reason, which is DeriveNodeKeyPair's spelling in this file.
	if crypto == nil {
		return nil, ErrNilCryptoProvider
	}
	if ct == nil {
		return nil, errNilHpkeCiphertext
	}
	return DecryptWithLabel(crypto, priv, label, context, ct.KemOutput, ct.Ciphertext)
}

// UpdatePathNode is RFC 9420 section 7.6: one node of the sender's filtered direct path, its
// fresh public key, and that node's path secret encrypted once per node in the resolution of the
// copath child.
//
//	struct { HPKEPublicKey encryption_key; HPKECiphertext encrypted_path_secret<V>; } UpdatePathNode;
//
// Nothing in this file checks that the number of ciphertexts is the number of nodes that
// resolution holds, and that is a BOUNDARY rather than a gap worth stating where the type is
// declared. The resolution is a property of a ratchet tree at an epoch, and a codec has neither:
// it is handed bytes. The check belongs to whoever holds the tree the path was built against --
// task 22's decrypt walks the resolution to pick its own ciphertext out of this vector, and is
// the first place both facts exist at once -- and the hazard worth naming is that a check both
// sides assume the other makes is a check nobody makes.
//
// The relation is over EVERY node of the path and not only the one the receiver decrypts from.
// A decrypt that checked the count at its own entry point and then walked the rest of the path
// on trust has enforced it at one index out of the path's length, which is the same hazard one
// step smaller.
//
// TestEveryPublishedUpdatePathHasOneCiphertextPerResolutionNode states the relation against the
// published corpus, so the two halves at least agree on what the relation IS.
type UpdatePathNode struct {
	EncryptionKey       HpkePublicKey
	EncryptedPathSecret []HpkeCiphertext
}

func (self *UpdatePathNode) MarshalMLS(w *syntax.Writer) error {
	w.WriteOpaque(self.EncryptionKey)
	return syntax.WriteVector(w, self.EncryptedPathSecret, writeOneHpkeCiphertext)
}

// UnmarshalMLS reads the key and the vector before either is assigned, for the reason
// HpkeCiphertext's decode states.
//
// The vector prefix counts BYTES and not elements, which is syntax.ReadVector's contract rather
// than this file's choice, and it is the one thing about this structure a round trip cannot see:
// an encoder writing an element count and a decoder reading an element count agree with each
// other and with no other implementation. The hand derived goldens are what pin it.
func (self *UpdatePathNode) UnmarshalMLS(r *syntax.Reader) error {
	encryptionKey, err := r.ReadOpaque()
	if err != nil {
		return err
	}
	ciphertexts, err := syntax.ReadVector(r, readOneHpkeCiphertext)
	if err != nil {
		return err
	}
	self.EncryptionKey = HpkePublicKey(encryptionKey)
	self.EncryptedPathSecret = ciphertexts
	return nil
}

var _ syntax.Codec = (*UpdatePathNode)(nil)

// the element halves of nodes<V>, named for the reason the ciphertext pair above is.
func writeOneUpdatePathNode(w *syntax.Writer, node UpdatePathNode) error {
	return node.MarshalMLS(w)
}

func readOneUpdatePathNode(r *syntax.Reader) (UpdatePathNode, error) {
	var node UpdatePathNode
	err := node.UnmarshalMLS(r)
	return node, err
}

// UpdatePath is RFC 9420 section 7.6, the public half of what task 18's UpdatePathPlan computed:
// the sender's re-signed commit leaf, and one node per step of its filtered direct path.
//
//	struct { LeafNode leaf_node; UpdatePathNode nodes<V>; } UpdatePath;
//
// The path secrets themselves are NOT here and never are -- UpdatePathPlan holds those and has
// no codec at all -- so this is the whole of what a commit publishes about the sender's path.
//
// Section 7.6 also requires the leaf_node here to carry leaf_node_source = commit, and this
// codec does not enforce that either: it decodes whatever leaf the leaf codec accepts, so a
// key_package sourced leaf inside an UpdatePath decodes cleanly. The door that DOES enforce it is
// ValidateUpdatePathLeafNode below, which is section 7.3's commit door onto (*LeafNode).Validate.
//
// THAT SENTENCE USED TO SAY THE DOOR WAS ALREADY BUILT AND IT WAS NOT. What existed was the
// validator's ability to hold an expectation -- LeafValidationContext.ExpectedSource -- with no
// production caller anywhere in this package setting it to commit, so the relation named here was
// enforced by nobody, and a test could exercise it only by writing the context itself. The
// difference between "the validator could answer this" and "some caller asks it" is the whole of
// the defect that comment was an instance of. TestEveryPublishedUpdatePathCarriesACommitSourceLeaf
// states the relation against the published corpus and drives the real door.
type UpdatePath struct {
	LeafNode LeafNode
	Nodes    []UpdatePathNode
}

func (self *UpdatePath) MarshalMLS(w *syntax.Writer) error {
	if err := self.LeafNode.MarshalMLS(w); err != nil {
		return err
	}
	return syntax.WriteVector(w, self.Nodes, writeOneUpdatePathNode)
}

// UnmarshalMLS decodes into a STAGED value and assigns it whole, which is leaf_node.go's
// discipline carried up one level, and up here it is load bearing in a way it is not inside a
// leaf.
//
// The plan's body decodes the leaf straight into the receiver's own field and then reads the
// nodes vector. A truncated or refused vector then leaves the caller holding an UpdatePath whose
// leaf came from these bytes and whose nodes are whatever the receiver arrived with -- and the
// leaf is the half carrying a signature, an encryption key and a parent hash, so that value is a
// path nobody sent, assembled out of two messages, sitting behind an error the caller may well
// have logged rather than returned. Staging makes the decoded path a function of the bytes it
// was read from and of nothing else.
func (self *UpdatePath) UnmarshalMLS(r *syntax.Reader) error {
	staged := UpdatePath{}
	if err := staged.LeafNode.UnmarshalMLS(r); err != nil {
		return err
	}
	nodes, err := syntax.ReadVector(r, readOneUpdatePathNode)
	if err != nil {
		return err
	}
	staged.Nodes = nodes
	*self = staged
	return nil
}

var _ syntax.Codec = (*UpdatePath)(nil)

// ---------------------------------------------------------------------------
// task 20: sealing the path secrets to the copath resolutions
// ---------------------------------------------------------------------------

// RFC 9420 section 7.6's label for every path secret encryption. One constant and not a
// literal at the call site, because the label is the whole of what stops a ciphertext sealed
// for one purpose from opening under another -- crypto_labels.go's own header argues that --
// and task 22's open has to spell the same string. Two literals agreeing today is two literals.
const updatePathNodeLabel = "UpdatePathNode"

// errPathLength is ValSem202, carried unexported on psk.go's terms and for psk.go's reason: the
// validation plan owns the exported ErrPathLength and declares it in errors.go, and two plans
// declaring one name in one package is a compile error at merge. The swap is mechanical -- this
// name for that one, wrapped in ValSem(ValSem202, ...) -- and it is owed by tasks 21 and 22 as
// well as by this one.
//
// It names a disagreement about HOW MANY nodes the path has, and this is the sending side of
// the rule ValSem202 makes a receiver enforce. There are two ways to reach it here and they are
// one condition: the tree this call was made over gives the sender a filtered direct path of a
// different length than the plan was built against -- the tree moved under the plan -- or the
// plan disagrees with itself, carrying a different number of secrets or public keys than nodes.
// Either way the positional pairing this whole construction rests on has no meaning, so it is
// refused before a single secret is sealed rather than being sealed to whatever the shorter of
// the two runs out at.
//
// It does NOT name a plan published under the wrong leaf, and the sentence that used to say it
// did was wrong for a reason that was measured rather than argued: every leaf of a full tree has
// a filtered direct path of the same length as every other, so a length comparison accepted leaf
// 0's plan published as leaf 4 and sealed leaf 0's path secrets to leaf 4's copath resolution.
// errPlanNotThisSenders is that refusal, and its own comment is why it has to be a second one.
var errPathLength = errors.New("mls: the update path is not the length of the sender's filtered direct path")

// errPlanNotThisSenders is EncryptUpdatePath asked to publish one leaf's plan under another
// leaf's index.
//
// The sender argument is a SECOND source of truth for an identity the plan already carries.
// CreateUpdatePathSecrets generated this plan for one leaf and recorded it in
// plan.Private.LeafIndex, and until this refusal nothing made the two agree -- the length
// agreement next door cannot, because in a full tree every leaf's filtered direct path is the
// same length as every other's. What that accepted is not a shape error: leaf 0's plan published
// as leaf 4 seals path_secret[0] to the resolution of leaf 4's copath, so leaves 5 and 6 receive
// secrets for nodes on leaf 0's direct path and leaves 1, 2 and 3 receive nothing at all.
//
// A receiver does reject that commit -- plan.LeafNode was signed over the index the plan was
// made for, so the leaf signature fails -- and that is exactly why the refusal belongs HERE. The
// signature is checked by a peer that has already received the ciphertexts; the secret is on the
// wire, sealed to a subtree that never generated it, before anyone is in a position to reject
// anything. A refusal that arrives after the send is not a refusal of this fault.
//
// Unexported for errNilUpdatePathPlan's reason in this same file: nothing outside this package
// can reach the condition, so it is not a refusal any exclusivity sweep should have to judge.
var errPlanNotThisSenders = errors.New("mls: the update path plan was built for a leaf other than the sender publishing it")

// errNilUpdatePathPlan is EncryptUpdatePath handed no plan, or one carrying no leaf, or one
// carrying no private state.
//
// The private half is REQUIRED and not merely used when present, because it is where the plan
// records the leaf it was generated for and that is what errPlanNotThisSenders is decided
// against. A plan without it is a plan whose sender cannot be checked at all, and the
// alternative -- checking only when it happens to be there -- is a refusal any caller switches
// off by handing over less.
//
// Unexported and an error rather than the nil dereference the shorter body would take, which is
// errNilHpkeCiphertext's argument in this same file: nothing outside this package can produce
// the condition, so it is not a refusal any exclusivity sweep should have to judge, and a panic
// out of a library takes the caller's process rather than its call.
var errNilUpdatePathPlan = errors.New("mls: no update path plan was supplied")

// EncryptUpdatePath is the public half of RFC 9420 section 7.6: each path secret of the plan,
// sealed once per node of the resolution of that node's copath child, in resolution order.
//
// The PAIRING is positional and it is the contract, not an implementation detail. A receiver
// finds its own ciphertext by index into the resolution it computes for itself, so ciphertext j
// of node i belongs to entry j of EncryptionTargets(sender, exclude)[i] and to nothing else. A
// permutation of either vector still hands every member a ciphertext of exactly the right
// shape, still publishes the right number of them, and is invisible until a decrypt fails one
// task later -- where it reads as a decryption bug rather than as an ordering one. That is why
// the loop walks targets[i] in the order EncryptionTargets answered and never a set, a map or a
// sorted copy, and why TestEncryptUpdatePathPairsEachCiphertextWithItsOwnResolutionEntry opens
// every ciphertext with the key of the resolution entry standing at its own index.
//
// groupContext is []byte rather than *GroupContext because the serialized form is what goes
// into the HPKE info, and taking bytes keeps this call from having to know how the key schedule
// plan encodes a GroupContext (C4). Callers pass syntax.Marshal(gc).
//
// It is the NEW epoch's context, and that is the whole reason task 18 and this call are two
// calls. The context's tree_hash covers the public keys and the commit leaf this path installs,
// so it does not exist until CreateUpdatePathSecrets has mutated the tree; a context built from
// the tree hash the sender started with is a context every peer computes differently, which
// makes every ciphertext here undecryptable for all of them at once. Nothing in a self
// consistent seal-and-open can see the difference, so
// TestEncryptUpdatePathSealsUnderTheContextOfTheEpochTheCommitOpens is what distinguishes the
// two contexts and says which one was used.
//
// exclude is forwarded to EncryptionTargets unchanged. A member this commit ADDS receives the
// path secret in its Welcome and must not receive it here as well, and the exclusion is applied
// to the resolution rather than to the tree for the reason that function's own comment gives.
//
// The tree is NOT mutated. Task 18 already installed everything this publishes; here the tree is
// read for the encryption keys of the resolution nodes and for nothing else.
func (self *RatchetTree) EncryptUpdatePath(crypto CryptoProvider, plan *UpdatePathPlan,
	sender LeafIndex, groupContext []byte, exclude []LeafIndex) (*UpdatePath, error) {
	// the provider before anything else is read, which is what
	// TestEveryDeclarationHandedANilProviderRefusesRatherThanDereferencingIt demands of every
	// declaration taking one and is the right order anyway: a caller that passed no provider
	// passed nothing this function could have sealed with, and answering it about its plan or
	// its sender index sends it to look at the argument that is not the problem.
	if crypto == nil {
		return nil, ErrNilCryptoProvider
	}
	if plan == nil || plan.LeafNode == nil || plan.Private == nil {
		return nil, errNilUpdatePathPlan
	}
	targets, err := self.EncryptionTargets(sender, exclude)
	if err != nil {
		return nil, err
	}
	// WHOSE plan this is, decided before its lengths, because a plan belonging to another leaf
	// makes every length below meaningless: two leaves of a full tree agree on the count of
	// their filtered direct paths and on nothing else. errPlanNotThisSenders' own comment is
	// what this costs to get wrong.
	if !updatePathPlanWasBuiltFor(plan, sender) {
		return nil, errPlanNotThisSenders
	}
	if !updatePathPlanMatchesTargets(plan, targets) {
		return nil, errPathLength
	}
	nodes := make([]UpdatePathNode, 0, len(plan.Path))
	for i := range plan.Path {
		ciphertexts := make([]HpkeCiphertext, 0, len(targets[i]))
		for _, y := range targets[i] {
			pub, err := self.nodeEncryptionKey(y)
			if err != nil {
				return nil, err
			}
			ct, err := SealWithLabel(crypto, pub, updatePathNodeLabel, groupContext, plan.PathSecrets[i])
			if err != nil {
				return nil, err
			}
			ciphertexts = append(ciphertexts, *ct)
		}
		nodes = append(nodes, UpdatePathNode{
			// CLONED, for DerivePathSecrets' reason one direction over. The plan is the
			// sender's live state for as long as the commit is in flight and may be thrown
			// away wholesale if the commit is rejected; what this answers is a wire value that
			// is about to be serialized and may be held by the framing layer for longer. Two
			// structures over one array make either one's disposal the other's corruption.
			EncryptionKey:       HpkePublicKey(cloneBytes(plan.PublicKeys[i])),
			EncryptedPathSecret: ciphertexts,
		})
	}
	// the leaf is deep copied for the same reason, and it is the half that matters more: it
	// carries a signature over its own encryption key and parent hash, so a value sharing those
	// arrays with the plan is a published leaf whose signature stops covering it if anything
	// touches either side.
	return &UpdatePath{LeafNode: *plan.LeafNode.Clone(), Nodes: nodes}, nil
}

// updatePathPlanMatchesTargets is the one length agreement EncryptUpdatePath rests on: the
// plan's path, its secrets, its public keys and the target lists all count the same nodes.
//
// A function of its own rather than three clauses in the body above, because
// TestEveryKeyQuestionOverTheRatchetTreeIsAnsweredInConstantTime's equality rule reads a body
// for == and != with a value on both sides and has no type information to tell a length from a
// key. Any later edit that gives EncryptUpdatePath a key shaped parameter would put it in that
// class, and a length comparison written inline would then be reported as a variable time
// comparison of data. Behind a helper that takes no key, answers a bool and is therefore not
// itself a member of the class, the decision is where that gate's own file comment says a path
// builder's index and length comparisons belong.
func updatePathPlanMatchesTargets(plan *UpdatePathPlan, targets [][]NodeIndex) bool {
	return len(targets) == len(plan.Path) &&
		len(plan.PathSecrets) == len(plan.Path) &&
		len(plan.PublicKeys) == len(plan.Path)
}

// updatePathPlanWasBuiltFor is the identity agreement EncryptUpdatePath rests on beside the
// length one: the leaf this plan was generated for is the leaf publishing it.
//
// Derived from the PLAN and not from the tree, and that is the whole of why it holds. A leaf's
// direct path is a function of its index and the tree's width alone, so two SIBLING leaves have
// the same filtered direct path node for node -- an elementwise comparison of plan.Path against
// FilteredDirectPath(sender) accepts leaf 0's plan published as leaf 1, because the only thing
// that differs between the two is the COPATH, which no node of the path names. The plan's own
// recorded index is the one value that separates every leaf from every other.
//
// It is also why nothing here compares plan.Path elementwise against the tree as well. Once the
// leaf index agrees, the length agreement above decides the nodes too: the filtered direct path
// of a fixed leaf is an ordered subsequence of that leaf's direct path, so two of them of the
// same length over the same tree are the same list. A comparison no edit can make fail is not a
// second check, it is a line that reads like one.
//
// A function of its own for updatePathPlanMatchesTargets' reason one declaration up: the
// decision is an equality over indices, and a body that takes no key is not a member of the
// class TestEveryKeyQuestionOverTheRatchetTreeIsAnsweredInConstantTime reads bodies for.
func updatePathPlanWasBuiltFor(plan *UpdatePathPlan, sender LeafIndex) bool {
	return plan.Private.LeafIndex == sender
}

// nodeEncryptionKey is the HPKE public key a tree carries at one node, whichever kind of node
// it is.
//
// The two ways there is no key here are told apart, which is ErrLeafBlank's argument at the
// node level: an index outside the node array is a caller that computed an index wrong and
// repairs it by recomputing one, and a BLANK node inside the array reached through a resolution
// is a tree whose resolution walk and whose node storage disagree -- Resolution answers only
// non-blank nodes by definition, so a blank one arriving here is structural and no re-derived
// index repairs it. Get answers nil for both alike, so the range check is written out rather
// than left to it.
func (self *RatchetTree) nodeEncryptionKey(x NodeIndex) (HpkePublicKey, error) {
	if uint32(x) >= self.NodeWidth() {
		return nil, ErrNodeIndexOutOfRange
	}
	node := self.Get(x)
	if node == nil {
		return nil, ErrTreeMalformed
	}
	if node.Leaf != nil {
		return node.Leaf.EncryptionKey, nil
	}
	if node.Parent != nil {
		return node.Parent.EncryptionKey, nil
	}
	// a Node is one occupied position and exactly one of its two halves is set, so a stored
	// node holding neither is the same structural fault as a blank one reached through a
	// resolution.
	return nil, ErrTreeMalformed
}

// ---------------------------------------------------------------------------
// task 21: merging a received UpdatePath into the public tree
// ---------------------------------------------------------------------------

// errNilUpdatePath is a merge or a decrypt handed no path at all.
//
// Unexported and an error rather than the nil dereference the shorter body would take, which is
// errNilHpkeCiphertext's and errNilUpdatePathPlan's argument in this same file: nothing outside
// this package can reach the condition -- a path arriving off the wire is decoded into a value
// and never into a nil pointer -- so it is not a refusal any exclusivity sweep of this package
// should have to judge, and a panic out of a library fed by the network takes the caller's
// process rather than its call.
var errNilUpdatePath = errors.New("mls: no update path was supplied")

// errNilTreeKEMPrivate is DecryptUpdatePath handed no private state.
//
// Required rather than merely used when present, for errNilUpdatePathPlan's reason one task
// over: the private state is where the RECEIVER's own leaf index is recorded, and that index is
// what decides which ciphertext of the path is addressed to us. A decrypt without it has no
// entry point to compute, and the alternative -- treating a nil state as leaf 0 holding nothing
// -- answers ErrNoPathSecret to a caller whose actual mistake was passing no state at all.
var errNilTreeKEMPrivate = errors.New("mls: no treekem private state was supplied")

// updatePathCoversTheFilteredPath is ValSem202 as a predicate: the path carries exactly one node
// per step of the sender's filtered direct path.
//
// A function of its own rather than a clause in either body, for updatePathPlanMatchesTargets'
// reason one task over. The decision is a comparison of two lengths with a value on either side,
// which is the shape TestEveryKeyQuestionOverTheRatchetTreeIsAnsweredInConstantTime's equality
// rule reports with no type information to tell a length from a key; behind a helper that takes
// no key and answers a bool, the decision stays where that gate's own file comment says a path
// builder's length comparisons belong, and it stays there if a later edit gives either caller a
// key shaped parameter and moves it into the class.
func updatePathCoversTheFilteredPath(path *UpdatePath, steps []PathStep) bool {
	return len(path.Nodes) == len(steps)
}

// MergeUpdatePath installs the public half of a received UpdatePath: RFC 9420 section 7.6's node
// keys on the sender's filtered direct path, and the sender's re-signed commit leaf.
//
// Only the ENCRYPTION KEYS travel. A parent node's parent_hash field is on no wire in section
// 7.6, so the receiver recomputes the chain here exactly as task 18 built it, walking the same
// PathStep list root down so that the two walks cannot differ by a copath child derived two
// different ways. The value that walk carries out at the bottom is compared against the leaf's
// own parent_hash field, which is inside what the leaf signature covers -- and that comparison is
// RFC 9420 section 7.9.2's obligation for a client PROCESSING A COMMIT, spelled there as
// "recompute the expected value of parent_hash for the committer's new leaf and verify that it
// matches the parent_hash value in the supplied leaf_node".
//
// And then section 7.9.2's other obligation, over the MERGED tree, through task 14's
// VerifyParentHashes. The chain comparison above is the RFC's second condition and only its
// second condition: it says the node keys and the leaf agree with each other, and it says nothing
// about the resolution those nodes sit in. Section 7.9.2's third condition -- D is in the
// resolution of C, and P's unmerged leaves under C are the resolution of C with D removed -- is
// what refuses a node somebody SPLICED into a subtree it never committed over, whose private key
// that somebody holds and whose position no member's leaf attests to. Such a node is in the
// resolution of a copath child, so the next honest commit seals a path secret straight to it, and
// every check the chain comparison makes passes: the splice is nowhere on the sender's path and
// the sender's own chain is perfectly self consistent. VerifyParentHashes counts the claimants of
// every non-blank parent under all three conditions and is what sees it. It is CALLED rather than
// restated here, for the reason the copath child is taken from the PathStep rather than
// re-derived: a second statement of a rule is a second chance to state it weaker, and this rule
// has already been written down once with the third condition missing.
//
// It is ATOMIC, which is task 18's contract at the other door and matters more here because the
// argument is attacker controlled. Everything below mutates a clone and the receiver adopts it in
// one assignment once the last thing that can fail has succeeded, so a refused merge leaves a
// tree byte identical to the one it was called on. A partially merged tree is worse than a
// rejected one: the caller is still processing the rest of a commit against it, the previous
// epoch's keys on the sender's path are already gone, and the group is left in a state no member
// agreed to and no caller can repair.
//
// What it deliberately does NOT do is verify the leaf's SIGNATURE, and saying so is the point of
// writing it down -- a check both sides assume the other makes is a check nobody makes. The
// signature is what makes the parent_hash comparison above mean anything, and it is verified by
// whoever holds the group id and the sender's index, which is the commit processing layer rather
// than the tree. That layer's door is ValidateUpdatePathLeafNode, immediately below, and the caller
// that runs it is p7 task 18's (*Group).stageInboundCommitLocked, which asks it BEFORE it calls
// this. Both halves are NAMED here rather than gestured at, and each was missing in its turn: this
// paragraph said "the commit processing layer" for as long as there was no such door anywhere in
// this package, and then named the door for as long as nothing called it -- and either sentence
// alone reads to a caller as an assurance that somebody else is doing it.
// What this does enforce, and enforces by DERIVATION rather than by a rule of its
// own, is that the leaf carries a parent_hash field at all: nodeParentHashField answers "no
// field" for every leaf whose source is not commit, so a path whose leaf claims some other source
// leaves the node above it with no claimant and VerifyParentHashes refuses it.
func (self *RatchetTree) MergeUpdatePath(crypto CryptoProvider, sender LeafIndex,
	path *UpdatePath) error {
	// the provider before anything is read off the receiver or the argument, which is what
	// TestEveryDeclarationHandedANilProviderRefusesRatherThanDereferencingIt demands of every
	// declaration taking one and is the right order anyway: every hash below comes through it,
	// and answering a caller that passed none about its tree or its path sends it to look at the
	// argument that is not the problem.
	if crypto == nil {
		return ErrNilCryptoProvider
	}
	if path == nil {
		return errNilUpdatePath
	}
	// the two ways a sender has no leaf here are two different faults and are answered as two,
	// which is CreateUpdatePathSecrets' split at the other door and is here for its reason: an
	// index past the width is a caller that computed an index wrong and repairs it by recomputing
	// one, and a blank slot inside the tree is a commit published from a position the group
	// REMOVED, whose index was right the whole time and whose repair is to rejoin.
	if LeafCount(sender) >= self.LeafWidth() {
		return ErrLeafIndexOutOfRange
	}
	if self.Leaf(sender) == nil {
		return ErrLeafBlank
	}
	// computed against the tree as it stands, and it is the same list the merged tree gives:
	// blanking and refilling the sender's own direct path cannot change the resolution of any
	// copath child, because no copath child is on that path. Computing it first keeps the length
	// refusal ahead of every mutation.
	steps, err := self.filteredPathSteps(sender)
	if err != nil {
		return err
	}
	// ValSem202.
	if !updatePathCoversTheFilteredPath(path, steps) {
		return errPathLength
	}
	provisional := self.Clone()
	if err := provisional.BlankDirectPath(sender); err != nil {
		return err
	}
	// the whole direct path is blanked and only the FILTERED path refilled, which is task 18's
	// shape and not an omission: a node dropped from the filtered path is one whose copath child
	// resolves to nothing, so it publishes no key and stays blank on the sender's tree too. A
	// fresh ParentNode also clears unmerged_leaves at every node of the path, which is what a
	// commit does to the members it merges.
	for i, step := range steps {
		if err := provisional.SetParent(step.Node, &ParentNode{
			EncryptionKey: HpkePublicKey(cloneBytes(path.Nodes[i].EncryptionKey)),
		}); err != nil {
			return err
		}
	}
	// the chain, root down, over the same PathStep list task 18 walked. carried is the parent
	// hash of the node ABOVE the one being written and starts as the zero-length octet string
	// section 7.9 gives the top of the path. The direction is required rather than preferred: the
	// parent hash of a node is taken over that node's own parent_hash field, so a loop running
	// the other way would hash a placeholder at every level and produce a chain that is full
	// length, stable, self consistent and rejected by every other member.
	carried := []byte{}
	for i := len(steps) - 1; i >= 0; i -= 1 {
		parent := provisional.ParentAt(steps[i].Node)
		if parent == nil {
			return ErrTreeMalformed
		}
		parent.ParentHash = carried
		hash, err := provisional.ParentHash(crypto, steps[i].Node, steps[i].CopathChild)
		if err != nil {
			return err
		}
		carried = hash
	}
	// the leaf is installed before the sweep below so that the tree that sweep judges is the whole
	// merged tree, claimant included. SetLeaf copies, so the path keeps its own value.
	if err := provisional.SetLeaf(sender, path.LeafNode.Clone()); err != nil {
		return err
	}
	// section 7.9.2's commit-time obligation, through crypto/subtle for guardrail 8's reason: a
	// parent hash is public, and every comparison in this package that decides whether a tree is
	// adopted is written the one way so no later reader has to work out which of them were the
	// safe ones. A length mismatch answers 0 here, so a leaf carrying no parent hash at all is
	// refused by this line rather than by a length clause in front of it.
	if subtle.ConstantTimeCompare(carried, path.LeafNode.ParentHash) != 1 {
		return ErrParentHashMismatch
	}
	// and section 7.9.2 in full, over the merged tree. See the header: the comparison above is
	// the second of three conditions, and the third is the one that refuses a spliced subtree.
	if err := provisional.VerifyParentHashes(crypto); err != nil {
		return err
	}
	// the adoption, and the last statement that can be reached by a failure is above it. A caller
	// holding this tree sees the previous epoch or the new one and never a half of both.
	self.nodes = provisional.nodes
	return nil
}

// errUpdatePathLeafNodeInvalid is a received UpdatePath whose leaf_node does not pass RFC 9420
// section 7.3 at the sender's position, carried unexported on errPathLength's terms and for its
// reason: the validation plan owns the exported catalogue, and two plans declaring one name in one
// package is a compile error at merge.
//
// It is a WRAP and never the whole answer -- the clause that refused is underneath it, reachable
// with errors.Is. A caller that could learn only "the path's leaf is bad" cannot tell a forged
// commit from a member whose capabilities have fallen behind the group's extensions, and those two
// are repaired at opposite ends: one by refusing the commit, one by asking that member to update.
var errUpdatePathLeafNodeInvalid = errors.New("mls: the update path's leaf node is not valid for a commit")

// ValidateUpdatePathLeafNode is RFC 9420 section 7.3's COMMIT door: the leaf of a RECEIVED
// UpdatePath, judged at the sender's position in the group the receiver holds.
//
// IT IS THE THIRD OF SECTION 7.3'S THREE DOORS AND UNTIL IT EXISTED THERE WERE TWO.
// LeafValidationContext.ExpectedSource takes one of three values and each needs a caller stating
// it: key_package.go states key_package, validate_proposals.go states update, and this states
// commit. Nothing stated commit; three separate comments described the commit door as already
// built; and the leaf of a received UpdatePath therefore reached MergeUpdatePath -- and the tree --
// with no signature check, no leaf_node_source check, no credential check and no section 13.4
// check. MergeUpdatePath says in as many words that it does not make them. This is the body that
// does.
//
// AND ITS CALLER IS p7 TASK 18's (*Group).stageInboundCommitLocked, which is the second half of that
// same account. The body landed one task ahead of the layer that had to run it, and for those tasks
// this package shipped a section 7.3 door its own construction did not run --
// rulesThisPackageExportsAndNothingApplies held that fact rather than leaving it in the gap between
// two tasks, and the entry came off by FAILING on the commit that gave it this caller. It is asked
// BEFORE the merge, because the merge's whole comparison is against a parent_hash field that only a
// commit-sourced leaf's own signature covers.
//
// WHAT A COMMIT LEAF OWES THAT AN UPDATE LEAF DOES NOT, worked out from section 7.6 rather than
// copied off the update door:
//
//   - the SOURCE is commit, which is the field that makes the leaf carry a parent_hash at all.
//     marshalCore's select gives the commit arm parent_hash and gives the update arm nothing, so a
//     leaf claiming update inside an UpdatePath is one whose parent_hash is not covered by its own
//     signature -- and that field is the entire subject of the chain comparison MergeUpdatePath
//     makes. A receiver skipping this rule compares a recomputed chain against an unsigned field.
//   - the CONTEXT the signature is bound to is the group's id and the SENDER's index, which is the
//     binding the update source takes too and is why signatureContent's select covers update and
//     commit together. Without it a commit leaf verifies in whatever group it is pasted into at
//     whatever position it is moved to.
//
// WHAT IT DOES NOT JUDGE is the parent_hash VALUE and the path around it. That is section 7.9.2's
// obligation, it needs the whole merged tree rather than one leaf, and MergeUpdatePath already
// makes it twice over -- the chain comparison and VerifyParentHashes. Two doors, one rule each,
// and this one is written so neither has to assume the other ran: it is a pure function of a leaf
// and a group context, safe to call before the merge as well as after.
//
// The clock is left out, and for a commit leaf that is not the documented NowMs opt out being
// taken -- it is the one input this rule has no use for. The lifetime is a variant field only a
// key_package leaf carries, so validateLifetime returns before reading it under any other source,
// and a NowMs passed here would be an input no branch could read.
func ValidateUpdatePathLeafNode(crypto CryptoProvider, context *GroupContext, sender LeafIndex,
	path *UpdatePath) error {
	// the provider ahead of every other argument, which is what
	// TestEveryDeclarationHandedANilProviderRefusesRatherThanDereferencingIt demands of every
	// declaration taking one, and is MergeUpdatePath's order next door.
	if crypto == nil {
		return ErrNilCryptoProvider
	}
	if context == nil {
		return fmt.Errorf("%w: the group id, the suite and the extensions the leaf is judged against are all its",
			ErrNilGroupContext)
	}
	if path == nil {
		return errNilUpdatePath
	}
	// read off the group's own extensions rather than taken as a second argument, so this door and
	// section 13.4's loop inside Validate read one list. A required_capabilities passed separately
	// is a second source of truth for something the GroupContext already carries, and the two
	// disagreeing admits a leaf against requirements the group does not have.
	required, err := requiredCapabilitiesOf(context.Extensions)
	if err != nil {
		return err
	}
	err = path.LeafNode.Validate(&LeafValidationContext{
		Crypto:          crypto,
		Suite:           context.CipherSuite,
		GroupId:         context.GroupId,
		LeafIndex:       sender,
		ExpectedSource:  LeafNodeSourceCommit,
		RequiredCaps:    required,
		GroupExtensions: context.Extensions,
	})
	if err != nil {
		return fmt.Errorf("%w: the path published at leaf %d: %w",
			errUpdatePathLeafNodeInvalid, sender, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// task 22: decrypting a received UpdatePath
// ---------------------------------------------------------------------------

// errPathDecrypt is ValSem203, carried unexported on errPathLength's terms and for its reason: the
// validation plan owns the exported ErrPathDecrypt and declares it in errors.go, and two plans
// declaring one name in one package is a compile error at merge.
//
// It names the ciphertext addressed to THIS member failing to open, and it is deliberately not
// the same answer as "no ciphertext here is addressed to us". A member off the sender's path, or
// one this commit adds, is in the ordinary condition and answers ErrNoPathSecret; a member that
// found its own entry in the resolution, holds the key that entry names, and still could not open
// the ciphertext standing at that entry's index has received a commit that is either corrupt or
// sealed under a group context it does not share. The first is repaired by re-fetching it and the
// second is not repaired at all, and one sentinel for both would tell the second to do the first.
var errPathDecrypt = errors.New("mls: the update path ciphertext addressed to this member did not open")

// errPathKeyMismatch is ValSem204, carried unexported for errPathDecrypt's reason.
//
// It names a node of the path whose announced encryption_key is not the key the recovered path
// secret derives. That is the refusal with the least visible failure mode of the three: every
// ciphertext opened, every rung of the ladder derived, and the receiver installed a private state
// whose public halves the group does not agree with -- after which it decrypts nothing for the
// rest of the group's life and reports a decryption failure against commits that are perfectly
// well formed. It is checked at EVERY node from the entry point to the root rather than at the
// entry point alone, because a path that announced one honest key and then any keys at all above
// it would otherwise be accepted.
var errPathKeyMismatch = errors.New("mls: an update path node's announced key is not the one its path secret derives")

// errPathSecretLength is the recovered path secret arriving at a width the suite's kdf does not
// answer, carried unexported for errPathDecrypt's reason.
//
// A sender seals a plaintext of whatever length it likes, and every check this decrypt makes
// afterwards agrees with a sender that agrees with itself: DeriveNodeKeyPair derives a key pair
// from a secret of any width, and the ValSem204 comparison is against a key that same sender
// announced. So a short or a long rung reaches the root with nothing here objecting, and the two
// ends of the group walk away holding different commit secrets -- which the key schedule's
// confirmation tag does separate, two layers up and one epoch later, with nothing left to say
// which octets were wrong. RFC 9420 section 7.6 does not spell this check at this layer; it is
// here because the width IS knowable here, from the provider the caller handed in, and a refusal
// that names the path is worth more than a confirmation tag that names the epoch.
var errPathSecretLength = errors.New("mls: the recovered update path secret is not the width of the suite's kdf")

// updatePathCiphertextsMatchTheTargets is the section 7.6 relation UpdatePathNode's own comment
// names as the boundary this layer owns: one ciphertext per node of the resolution of that node's
// copath child, at every node of the path.
//
// At EVERY node and not only at the receiver's own entry point, which is that comment's second
// paragraph made into a check. A decrypt that counted its own node and walked the rest of the path
// on trust has enforced the relation at one index out of the path's length; what it accepts is a
// path whose ciphertext vectors are the wrong size everywhere else, which is a positional pairing
// that means nothing for every OTHER member of the group -- and the member that would notice is
// not the one running this code.
//
// A function of its own for updatePathCoversTheFilteredPath's reason: it decides two lengths with
// a value on either side, and it holds no key.
func updatePathCiphertextsMatchTheTargets(path *UpdatePath, targets [][]NodeIndex) bool {
	if len(path.Nodes) != len(targets) {
		return false
	}
	for i := range targets {
		if len(path.Nodes[i].EncryptedPathSecret) != len(targets[i]) {
			return false
		}
	}
	return true
}

// indexOfStep is the position of one node in a filtered direct path, or false.
//
// A linear scan and not a map, because the list is one entry per LEVEL of the tree -- ten of them
// in the 500 member group task 28 benchmarks -- and a map allocated per decrypt to answer one
// question is more work than the scan it replaces.
//
// A function of its own for the reason its two neighbours give: the comparison inside it has a
// value on either side, and a helper over node indices alone holds no key and answers about no
// tree, so it is not itself a member of the class that gate reads bodies for.
func indexOfStep(steps []PathStep, x NodeIndex) (int, bool) {
	for i, step := range steps {
		if step.Node == x {
			return i, true
		}
	}
	return 0, false
}

// PathDecryptResult is what a receiver ends up with after opening one UpdatePath: the epoch's
// commit secret, and the private state it should hold against the merged tree.
//
// The private state is a NEW value rather than an edit of the one handed in, which is
// TreeKEMPrivate.Clone's contract carried up one layer. A commit is processed against a candidate
// epoch and may still be rejected -- by a validation failure further along the same commit, or by
// losing a race with another member's -- so a decrypt that wrote the new rungs through the
// caller's state would have replaced the epoch the group is still running on before anything got
// the chance to refuse it.
type PathDecryptResult struct {
	CommitSecret []byte
	Private      *TreeKEMPrivate
}

// DecryptUpdatePath opens the one ciphertext of a received UpdatePath that is addressed to this
// member and ratchets the rest of the way to the root, RFC 9420 section 7.6.
//
// Called AFTER MergeUpdatePath and on the merged tree. The copath resolutions the pairing is read
// out of are the same either way -- the merge touches the sender's direct path and its leaf, and
// no copath child of that path is on it -- but the announced keys this checks against are in the
// tree only after the merge, and the group context the ciphertexts were sealed under carries the
// tree hash of the merged tree. A caller that decrypted first would be checking a path against
// the epoch it closed.
//
// The receiver's own ciphertext is found STRUCTURALLY and never by trial decryption. The lowest
// node of the sender's filtered direct path that covers this member is
// CommonAncestor(senderLeaf, receiverLeaf), and it is always in the filtered path because this
// member's own non-blank leaf keeps the relevant copath resolution non-empty. Within that node's
// resolution the receiver takes the first entry it can derive a private key for, and the
// ciphertext it opens is the one standing at THAT entry's index -- which is the positional
// pairing EncryptUpdatePath's comment calls the contract, read from the other end. Trial
// decryption over the whole vector would agree with a correct sender and would agree just as well
// with one that permuted its ciphertexts, which is the failure with no symptom until some other
// member cannot open the same commit.
//
// The entry is the first entry we hold a key for and not necessarily our own leaf, and that is
// the case a second commit covers: a member that already holds a path secret for a node above it
// derives that node's key, and the sender sealed to that node rather than to the leaf, because a
// non-blank node resolves to itself and its subtree is never walked.
//
// Every rung is checked against the announced key (ValSem204) before it is kept, and the count of
// ciphertexts is checked against the resolution at every node of the path (section 7.6) before
// anything is opened. What each of the four refusals means is written on the sentinels:
// errPathLength is a path of the wrong shape for this tree, errPathDecrypt is our own ciphertext
// failing to open, errPathSecretLength is our own ciphertext opening to something that is not a
// rung of this suite's ladder, ErrNoPathSecret is the ordinary condition of a member this commit
// did not seal to.
//
// What the answered state carries forward is the WHOLE of what the caller's state held, plus the
// rungs from the entry point up. Nothing is pruned, and that is a BOUNDARY worth stating rather
// than an oversight. This method knows the merged tree and the caller's secrets and has no way to
// tell a secret that is stale from one for a node it simply did not walk, so it keeps both. A
// caller whose state held a secret for a node this commit BLANKED therefore carries that secret
// into the answer, and TreeKEMPrivate.Consistent is what refuses the result: a held secret whose
// node is blank answers ErrPathSecretMismatch there. Reachable only from a state that was already
// stale before this commit arrived -- the blanking happens at the proposal that removed the
// subtree, one epoch earlier -- so it is garbage in and a named refusal out rather than a fault
// introduced here. TestDecryptUpdatePathCarriesForwardASecretForANodeTheMergeBlanked is what
// holds it, so a later pass that decides to prune has to say so there.
//
// The private half of every node key from the entry point up is derived, compared and ERASED. The
// comparison needs the public half alone and DeriveNodeKeyPair answers both, so leaving the
// private halves of every node between here and the root on the heap is exactly the material
// secret_zeroize.go exists for. What is kept is the path secret, which is what the state is for
// and what re-derives them on demand.
func (self *RatchetTree) DecryptUpdatePath(crypto CryptoProvider, sender LeafIndex,
	path *UpdatePath, groupContext []byte, priv *TreeKEMPrivate,
	exclude []LeafIndex) (*PathDecryptResult, error) {
	if crypto == nil {
		return nil, ErrNilCryptoProvider
	}
	if path == nil {
		return nil, errNilUpdatePath
	}
	if priv == nil {
		return nil, errNilTreeKEMPrivate
	}
	steps, err := self.filteredPathSteps(sender)
	if err != nil {
		return nil, err
	}
	// ValSem202, before the targets are walked: a path of the wrong length pairs nothing with
	// anything, and the index this is about to compute would be an index into it.
	if !updatePathCoversTheFilteredPath(path, steps) {
		return nil, errPathLength
	}
	targets, err := self.EncryptionTargets(sender, exclude)
	if err != nil {
		return nil, err
	}
	if !updatePathCiphertextsMatchTheTargets(path, targets) {
		return nil, errPathLength
	}
	// the lowest node of the sender's path that covers us.
	lowest := CommonAncestor(sender.NodeIndex(), priv.LeafIndex.NodeIndex())
	start, onThePath := indexOfStep(steps, lowest)
	if !onThePath {
		// the sender itself, or a member whose common ancestor with the sender was filtered out
		// of the path, which is the ordinary condition rather than a fault.
		return nil, ErrNoPathSecret
	}
	var secret []byte
	for j, y := range targets[start] {
		nodePriv, held, err := priv.NodePrivateKey(crypto, y)
		if err != nil {
			return nil, err
		}
		if !held {
			continue
		}
		ct := path.Nodes[start].EncryptedPathSecret[j]
		opened, err := OpenWithLabel(crypto, nodePriv, updatePathNodeLabel, groupContext, &ct)
		// erased whether or not the open succeeded: NodePrivateKey answers fresh storage for both
		// of its arms precisely so that this call can, and the return path that would skip the
		// erasure is the one an error takes.
		zeroizeSecret(nodePriv)
		if err != nil {
			return nil, errPathDecrypt
		}
		// the width, checked here rather than left for the ladder. See errPathSecretLength:
		// everything below this line agrees with a sender that agrees with itself, so this is
		// the last place a wrong width is a statement about a message rather than about an
		// epoch. The comparison is a length and carries no secret, so it is a plain one.
		if len(opened) != crypto.HashSize() {
			return nil, errPathSecretLength
		}
		secret = opened
		break
	}
	if secret == nil {
		return nil, ErrNoPathSecret
	}
	out := priv.Clone()
	for i := start; i < len(steps); i += 1 {
		derivedPriv, derivedPub, err := DeriveNodeKeyPair(crypto, secret)
		if err != nil {
			return nil, err
		}
		zeroizeSecret(derivedPriv)
		// ValSem204, through crypto/subtle for the reason MergeUpdatePath's comparison gives.
		if subtle.ConstantTimeCompare(derivedPub, path.Nodes[i].EncryptionKey) != 1 {
			return nil, errPathKeyMismatch
		}
		out.PathSecrets[steps[i].Node] = cloneBytes(secret)
		// the rung PAST the last node is the epoch's commit secret, section 8.1, which is why
		// this derives once more than there are nodes left and why the value that falls out of
		// the loop is the answer rather than a leftover.
		secret = crypto.DeriveSecret(secret, "path")
	}
	return &PathDecryptResult{CommitSecret: secret, Private: out}, nil
}
