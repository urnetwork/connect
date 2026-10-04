// The gates over the METHODS of this package that are handed a CryptoProvider.
//
// Every derived provider gate this package already carries reads the package level half of
// the surface and stops there. packageLevelFunctionsIn skips a declaration carrying a
// receiver, and providerConstructions skips one the type checker reads as a method; the
// three gates built on them -- TestEveryConstructionInThisPackageLeavesItsInputAlone,
// TestEveryConstructionHandedAProviderRoutesThroughIt and
// TestEveryConstructionHandedAProviderReadsKdfNhFromIt -- therefore demand nothing of a
// method, and a method cannot be given a row in them even if somebody wanted to.
//
// That is a class boundary rather than an enumeration, and it is the right one for those
// gates: a construction is called by name and a method is called on a receiver, so the two
// need different tables. What was missing is the other half of the partition. A receiver was
// the whole of what it took to sit outside every one of them, and the two methods the
// transcript adds are exactly the shape those gates exist for -- they read KDF.Nh off the
// provider they are handed, they compute the one value a group cannot disagree about and
// recover from, and one of them RETAINS a caller's array for the lifetime of the group.
// Measured, not supposed: nh := crypto.HashSize() replaced by nh := 32 in
// (*TranscriptHashes).SetFromGroupInfo, and its defensive copy of the GroupInfo's confirmed
// transcript hash deleted, each left the whole of mls green.
//
// So this file states the other half. The class is read off the type checker -- every method
// of this package's non test source whose parameters include a CryptoProvider -- each member
// has to have a row, and TestEveryDeclarationTakingAProviderIsHeldByExactlyOneOfTheTwoClasses
// compares the two halves against the whole so a declaration cannot fall between them.
package mls

import (
	"bytes"
	"fmt"
	"go/types"
	"slices"
	"testing"
	"time"
)

// One value a method left behind, named.
//
// The values are read one at a time rather than concatenated, and that is the difference
// between this gate and a weaker one. A method leaving two values behind, one computed
// through the provider it was handed and one computed through a provider of its own, answers
// a DIFFERENT concatenation over a provider that flips every answer -- the half that routed
// correctly moves and carries the joined comparison with it. Measured, not supposed: with the
// values joined, an Update building its own provider out of a hardcoded suite for the
// confirmed hash and routing only the interim hash through its parameter passed this file.
type providerDrivenMethodValue struct {
	name    string
	content []byte
	// carried marks a value the method was HANDED rather than one it computed. The routing
	// differential is the wrong instrument for one: a copy of an argument is the same bytes
	// over every provider, so requiring it to move would report every possible
	// implementation as defective. The flag is not taken on trust -- the routing gate holds
	// a carried value to being identical across the two providers and every other value to
	// being different, so a flag set on the wrong value fails rather than exempting it.
	carried bool
	// storage is the array the RETENTION gate reads for this value, when content is a
	// rendering rather than the value itself.
	//
	// The two halves exist because the three gates want opposite things from one value. A
	// KEM key is 32 octets whatever the kdf does, so the KDF.Nh differential reads a raw one
	// as a length written down -- which is why this file renders keys as hex before
	// reporting them, and NodePrivateKey's row already does. But a rendering is a fresh
	// array, so the aliasing question the retention gate asks answers "shares nothing" for
	// every possible implementation once a value is rendered. Naming the storage alongside
	// the rendering is what lets one value answer both: the differentials read content, the
	// aliasing reads storage, and they are the same value read two ways rather than two
	// values that could drift apart.
	//
	// A row leaving this nil is read raw, which is the ordinary case.
	storage []byte
}

// One method of this package that is handed a provider, and how to drive it.
//
// The row answers what the RECEIVER holds after the call rather than a return value, because
// what these methods are for is state: two of the three answer nothing but an error, and
// every property below -- routing, KDF.Nh and retention -- is a property of what was left
// behind. Bytes the caller owns go through take, so the recorder the retention gate hands in
// sees exactly the arrays a caller would still own after the call and no others.
type providerDrivenMethodRow struct {
	name string
	call func(t *testing.T, crypto CryptoProvider, take func(content []byte) []byte) ([]providerDrivenMethodValue, error)
}

// providerDrivenMethodRows is the table, one row per member of the derived class.
//
// Every value a row is built out of is cut to the provider's own KDF.Nh rather than to 32,
// which is what lets the same rows run over the wide provider below. A row that wrote a
// length down would be refused there for a reason that is this file's rather than the
// method's, and the differential would read as a defect that is not there.
func providerDrivenMethodRows() []providerDrivenMethodRow {
	return []providerDrivenMethodRow{
		// psk.go's ValSem401. The nonce is the receiver's own field rather than a
		// parameter, so it is not taken through the recorder and this row is outside the
		// retention gate's derived class by the same reading that puts it inside the other
		// two: it is handed no caller array it could keep.
		{name: "(*PreSharedKeyId).Validate", call: func(t *testing.T, crypto CryptoProvider, take func([]byte) []byte) ([]providerDrivenMethodValue, error) {
			id := &PreSharedKeyId{
				PskType:  PskTypeExternal,
				PskId:    bytes.Repeat([]byte{0x11}, 16),
				PskNonce: bytes.Repeat([]byte{0x12}, crypto.HashSize()),
			}
			if err := id.Validate(crypto); err != nil {
				return nil, err
			}
			// a validator computes nothing: both values are the ones it was given, which
			// is why the routing gate cannot hold this row at all
			return []providerDrivenMethodValue{
				{name: "PskId", content: id.PskId, carried: true},
				{name: "PskNonce", content: id.PskNonce, carried: true},
			}, nil
		}},
		// p7 task 15's welcome builder, over a staged commit assembled HERE rather than
		// committed: what this row is about is the provider the seal runs through, and a real
		// commit would bring a second provider's worth of derivation along with it.
		//
		// Every run it is handed goes through the recorder -- the group id, the two transcript
		// hashes, the epoch's joiner and psk secrets, the confirmation tag and the init key the
		// entry is sealed to -- and the ONE value read back is the sealed group info. It is an
		// AEAD seal under a key and nonce this provider expanded, so it MOVES over a provider
		// that flips every answer; and its length is a whole encoded GroupInfo rather than any
		// registry width, so the KDF.Nh differential reads it without a coincidence this file
		// put there.
		//
		// The per-joiner ciphertexts are deliberately not read back. Each encapsulates to a
		// fresh ephemeral, so they differ between any two calls whatever provider made them, and
		// a row that compared them would report every provider as a different one.
		{name: "(*StagedCommit).welcomeMessage", call: func(t *testing.T, crypto CryptoProvider, take func([]byte) []byte) ([]providerDrivenMethodValue, error) {
			nh := crypto.HashSize()
			minted, err := welcomeMethodRowKeyPackage()
			if err != nil {
				return nil, err
			}
			addressed := *minted
			addressed.InitKey = HpkePublicKey(take(bytes.Clone(minted.InitKey)))
			context := &GroupContext{
				Version:                 ProtocolVersionMls10,
				CipherSuite:             crypto.Suite(),
				GroupId:                 take([]byte("the group this welcome row describes")),
				Epoch:                   4,
				TreeHash:                take(bytes.Repeat([]byte{0x91}, nh)),
				ConfirmedTranscriptHash: take(bytes.Repeat([]byte{0x92}, nh)),
			}
			schedule, err := NewKeyScheduleFromJoiner(crypto,
				take(bytes.Repeat([]byte{0x93}, nh)), take(bytes.Repeat([]byte{0x94}, nh)), context)
			if err != nil {
				return nil, err
			}
			staged := &StagedCommit{
				committer:  0,
				epoch:      context.Epoch,
				context:    context,
				schedule:   schedule,
				added:      []LeafIndex{1},
				confirmTag: take(bytes.Repeat([]byte{0x95}, nh)),
				list: NewProposalList([]CachedProposal{{
					Proposal: Proposal{ProposalType: ProposalTypeAdd, Add: &Add{KeyPackage: addressed}},
				}}),
			}
			message, err := staged.welcomeMessage(crypto, &GroupInfo{
				GroupContext:    *context,
				ConfirmationTag: take(bytes.Repeat([]byte{0x96}, nh)),
			})
			if err != nil {
				return nil, err
			}
			parsed, err := ParseMLSMessage(message)
			if err != nil {
				return nil, err
			}
			return []providerDrivenMethodValue{
				{name: "the sealed group info", content: parsed.Welcome.EncryptedGroupInfo},
			}, nil
		}},
		// framing's proposal reference of section 5.2: the identity a commit names a proposal
		// by, and the one value in this package whose whole job is to be the same on every peer
		// that sees the same proposal. The bytes it hashes are the RECEIVER's, so nothing goes
		// through the recorder and this row is outside the retention gate's derived class by the
		// same reading that puts it inside the other two: the method is handed no caller array
		// at all, only a provider.
		//
		// The ref is a derived value rather than a carried one, so the routing differential
		// requires it to MOVE over a provider that flips every answer: a reference computed
		// through crypto/sha256 directly, or through a provider built here out of a hardcoded
		// suite, agrees with every corpus in this package because they are all SHA-256 -- and it
		// is a commit naming a proposal nobody else can look up the moment a group runs at
		// another suite.
		{name: "(*AuthenticatedContent).ProposalRef", call: func(t *testing.T, crypto CryptoProvider, take func([]byte) []byte) ([]providerDrivenMethodValue, error) {
			authContent := &AuthenticatedContent{
				WireFormat: WireFormatPublicMessage,
				Content: FramedContent{
					GroupId:     bytes.Repeat([]byte{0x41}, 4),
					Epoch:       7,
					Sender:      Sender{SenderType: SenderTypeMember, LeafIndex: 1},
					ContentType: ContentTypeProposal,
					Proposal:    &Proposal{ProposalType: ProposalTypeRemove, Remove: &Remove{Removed: 2}},
				},
				Auth: FramedContentAuthData{Signature: bytes.Repeat([]byte{0x42}, 64)},
			}
			ref, err := authContent.ProposalRef(crypto)
			if err != nil {
				return nil, err
			}
			return []providerDrivenMethodValue{{name: "ProposalRef", content: ref}}, nil
		}},
		// the transcript's own advance. Both arguments are the caller's -- the serialized
		// ConfirmedTranscriptHashInput is the framed commit it goes on to verify a signature
		// over, and the confirmation tag is compared against a freshly computed one
		// afterwards -- and both hashes it leaves behind are read.
		//
		// The input is 64 octets: deliberately neither KDF.Nh of the two providers below, so
		// no comparison there reports a coincidence belonging to a length this file chose.
		{name: "(*TranscriptHashes).Update", call: func(t *testing.T, crypto CryptoProvider, take func([]byte) []byte) ([]providerDrivenMethodValue, error) {
			hashes := InitialTranscriptHashes()
			err := hashes.Update(crypto,
				take(bytes.Repeat([]byte{0x21}, 64)),
				take(bytes.Repeat([]byte{0x22}, crypto.HashSize())))
			if err != nil {
				return nil, err
			}
			return []providerDrivenMethodValue{
				{name: "Confirmed", content: hashes.Confirmed},
				{name: "Interim", content: hashes.Interim},
			}, nil
		}},
		// the joiner's seeding, which is the one long lived retention of somebody else's
		// bytes in this package: the confirmed transcript hash it is handed is a field of a
		// decoded GroupInfo whose buffer the caller still owns, and what it keeps is carried
		// into every later epoch of the group.
		//
		// Confirmed is marked carried because it is the GroupInfo's value and not a
		// derivation -- a joiner that recomputed it would hold a hash the group does not.
		// The routing gate therefore reads it as a value that must NOT move across the two
		// providers, which is a real assertion rather than an exemption.
		{name: "(*TranscriptHashes).SetFromGroupInfo", call: func(t *testing.T, crypto CryptoProvider, take func([]byte) []byte) ([]providerDrivenMethodValue, error) {
			nh := crypto.HashSize()
			joiner := InitialTranscriptHashes()
			err := joiner.SetFromGroupInfo(crypto,
				take(bytes.Repeat([]byte{0x31}, nh)),
				take(bytes.Repeat([]byte{0x32}, nh)))
			if err != nil {
				return nil, err
			}
			return []providerDrivenMethodValue{
				{name: "Confirmed", content: joiner.Confirmed, carried: true},
				{name: "Interim", content: joiner.Interim},
			}, nil
		}},
		// the leaf's signing half. What it leaves behind is the signature, which is a
		// signature over the LeafNodeTBS taken through the provider it was handed -- a body
		// that reached for ed25519 itself would answer a signature that verifies against
		// every leaf in this package and against every published tree, because the corpora
		// are all Ed25519, which is the scheme it would have hardcoded.
		//
		// The private key is a fixed 32 octets rather than one drawn through the provider,
		// so this row does not consume a stream some other row is positioned in, and the
		// group id is taken through the recorder because it is the caller's.
		{name: "(*LeafNode).Sign", call: func(t *testing.T, crypto CryptoProvider, take func([]byte) []byte) ([]providerDrivenMethodValue, error) {
			leaf := testLeafNodeOfSource(LeafNodeSourceCommit)
			err := leaf.Sign(crypto, SignaturePrivateKey(take(bytes.Repeat([]byte{0x51}, 32))),
				take([]byte("the group this leaf sits in")), 3)
			if err != nil {
				return nil, err
			}
			return []providerDrivenMethodValue{{name: "Signature", content: leaf.Signature}}, nil
		}},
		// the leaf's verifying half, whose whole answer is a yes or a no. The tagging
		// provider passes VerifyWithLabel through unchanged -- there is nothing in a refusal
		// for a flip to change -- so a row reading a value the verifier left behind could
		// not be separated from a constant, and this row reads the VERDICT instead.
		//
		// The leaf is signed through the same provider it is then verified against, which is
		// what makes the verdict move: over a provider whose signing half flips its answer, a
		// verifier that routed through that provider refuses, and one that reached for
		// ed25519 on its own accepts exactly as it did over the real provider. The receiver
		// carries the signer's own public key, so nothing here fails for a reason that is not
		// the routing.
		{name: "(*LeafNode).VerifySignature", call: func(t *testing.T, crypto CryptoProvider, take func([]byte) []byte) ([]providerDrivenMethodValue, error) {
			signer := SignaturePrivateKey(bytes.Repeat([]byte{0x52}, 32))
			signaturePub, err := signaturePublicKeyOf(signer)
			if err != nil {
				return nil, err
			}
			leaf := testLeafNodeOfSource(LeafNodeSourceCommit)
			leaf.SignatureKey = signaturePub
			groupId := take([]byte("the group this leaf sits in"))
			if err := leaf.Sign(crypto, signer, groupId, 3); err != nil {
				return nil, err
			}
			verdict := []byte("the leaf verifies")
			if refused := leaf.VerifySignature(crypto, groupId, 3); refused != nil {
				verdict = []byte("the leaf is refused: " + refused.Error())
			}
			return []providerDrivenMethodValue{{name: "the verdict", content: verdict}}, nil
		}},
		// key_package.go's reference, RFC 9420 section 5.2. It is a digest, which is what
		// both differentials can read: it moves under a provider that flips every answer, and
		// it is KDF.Nh wide under a provider whose hash is one width up. The receiver is
		// built by hand rather than by NewKeyPackage, because a constructor that verifies
		// what it signed refuses over the tagging provider and this row would then be
		// reporting the constructor's routing instead of the reference's.
		{name: "(*KeyPackage).Ref", call: func(t *testing.T, crypto CryptoProvider, take func([]byte) []byte) ([]providerDrivenMethodValue, error) {
			kp := &KeyPackage{
				Version:     ProtocolVersionMls10,
				CipherSuite: CipherSuiteX25519ChaCha20Sha256Ed25519,
				InitKey:     HpkePublicKey(bytes.Repeat([]byte{0x53}, 32)),
				LeafNode:    *testLeafNodeOfSource(LeafNodeSourceKeyPackage),
				Signature:   bytes.Repeat([]byte{0x54}, 64),
			}
			ref, err := kp.Ref(crypto)
			if err != nil {
				return nil, err
			}
			return []providerDrivenMethodValue{{name: "the key package reference", content: ref}}, nil
		}},
		// key_package.go's section 10.1 validation, whose whole answer is a yes or a no, so
		// this row reads the VERDICT for (*LeafNode).VerifySignature's reason. The key package
		// is minted through the same provider it is then validated against, which is what
		// makes the verdict move: over a provider whose signing half flips its answer the
		// constructor's own verify refuses, and a validator that reached for ed25519 on its
		// own would accept exactly as it did over the real provider.
		{name: "(*KeyPackage).Validate", call: func(t *testing.T, crypto CryptoProvider, take func([]byte) []byte) ([]providerDrivenMethodValue, error) {
			kp, _, _, err := NewKeyPackage(crypto, CipherSuiteX25519ChaCha20Sha256Ed25519,
				BasicCredential([]byte("alice")), leafNodeStubCapabilities(), nil)
			if err != nil {
				return []providerDrivenMethodValue{
					{name: "the verdict", content: []byte("refused at construction: " + err.Error())},
				}, nil
			}
			verdict := []byte("the key package validates")
			if refused := kp.Validate(crypto, CipherSuiteX25519ChaCha20Sha256Ed25519, time.Now()); refused != nil {
				verdict = []byte("the key package is refused: " + refused.Error())
			}
			return []providerDrivenMethodValue{{name: "the verdict", content: verdict}}, nil
		}},
		// tree_hash.go's four, RFC 9420 section 7.8. All four build their tree the same way and
		// none of them touches the provider to do it: newTestTree signs every leaf through the
		// provider it is handed, so a row built on it would be reporting the signer's routing
		// as the hash's. What each row leaves behind is a digest, and a digest is exactly what
		// both differentials can read -- it moves under a provider that flips every answer, and
		// it is KDF.Nh wide under a provider whose hash is one width up.
		{name: "(*RatchetTree).treeHash", call: func(t *testing.T, crypto CryptoProvider, take func([]byte) []byte) ([]providerDrivenMethodValue, error) {
			tree := providerRowRatchetTree(t)
			root, err := rootOf(tree.LeafWidth())
			if err != nil {
				return nil, err
			}
			// the exclusion arm rather than the nil one, so the row drives the parameter the
			// three exported methods never pass and that the section 7.9 parent hash will
			hash, err := tree.treeHash(crypto, root, map[LeafIndex]bool{LeafIndex(1): true})
			if err != nil {
				return nil, err
			}
			return []providerDrivenMethodValue{{name: "the original tree hash at the root", content: hash}}, nil
		}},
		{name: "(*RatchetTree).NodeTreeHash", call: func(t *testing.T, crypto CryptoProvider, take func([]byte) []byte) ([]providerDrivenMethodValue, error) {
			tree := providerRowRatchetTree(t)
			leaf, err := tree.NodeTreeHash(crypto, NodeIndex(0))
			if err != nil {
				return nil, err
			}
			parent, err := tree.NodeTreeHash(crypto, NodeIndex(1))
			if err != nil {
				return nil, err
			}
			// a leaf and a parent, because the two arms of the section 7.8 select are two
			// separate preimages and a method that routed one of them through its parameter
			// is the defect a single value cannot see
			return []providerDrivenMethodValue{
				{name: "the hash of leaf node 0", content: leaf},
				{name: "the hash of parent node 1", content: parent},
			}, nil
		}},
		{name: "(*RatchetTree).TreeHash", call: func(t *testing.T, crypto CryptoProvider, take func([]byte) []byte) ([]providerDrivenMethodValue, error) {
			tree := providerRowRatchetTree(t)
			hash, err := tree.TreeHash(crypto)
			if err != nil {
				return nil, err
			}
			return []providerDrivenMethodValue{{name: "the whole tree's hash", content: hash}}, nil
		}},
		{name: "(*RatchetTree).TreeHashes", call: func(t *testing.T, crypto CryptoProvider, take func([]byte) []byte) ([]providerDrivenMethodValue, error) {
			tree := providerRowRatchetTree(t)
			hashes, err := tree.TreeHashes(crypto)
			if err != nil {
				return nil, err
			}
			if uint32(len(hashes)) != tree.NodeWidth() {
				t.Fatalf("TreeHashes answered %d entries for a %d node tree", len(hashes), tree.NodeWidth())
			}
			// every entry, not the root alone: this method's whole content is that it answers
			// one hash per node, and a row reading one of them would pass over an
			// implementation that computed the rest through a provider of its own
			values := []providerDrivenMethodValue{}
			for x, hash := range hashes {
				values = append(values, providerDrivenMethodValue{
					name:    fmt.Sprintf("the hash of node %d", x),
					content: hash,
				})
			}
			return values, nil
		}},
		// section 7.9's three. ParentHash answers a digest, which the KDF.Nh differential reads
		// the way it reads the four above it -- and which the ROUTING differential does NOT
		// read the way it reads them. That is a limit of this row rather than a property of the
		// method, and it is written down because a row that looks like the four above it will
		// otherwise be trusted to hold what they hold.
		//
		// The preimage a parent hash is taken over already carries a provider-derived value:
		// the original tree hash of the sibling subtree. So this row's output moves across the
		// two providers whether the FINAL crypto.Hash call routed or not, and a body that took
		// that last digest with crypto/sha256 directly passes the routing gate. Measured, not
		// supposed -- `return crypto.Hash(input), nil` replaced by sha256.Sum256 left
		// TestEveryMethodHandedAProviderRoutesThroughIt green. The routing differential still
		// holds everything UNDER the final call, which is most of the method, and that is what
		// this row is worth.
		//
		// What holds the final call is the KDF.Nh row below -- sha256 answers 32 octets over a
		// provider whose Nh is 48 -- together with
		// TestTheParentHashIsTheProvidersHashOfTheHandDerivedParentHashInput, which compares
		// this method's answer against the tagging provider's own hash of the same hand written
		// octets. Neither is redundant with the other, and the difference is what each reads:
		// the Nh row reads a WIDTH, over a provider whose Nh is not 32 and which the golden does
		// not install; the golden reads the BYTES, which the Nh row never compares. A digest of
		// the right width taken outside the provider is visible only to the golden.
		{name: "(*RatchetTree).ParentHash", call: func(t *testing.T, crypto CryptoProvider, take func([]byte) []byte) ([]providerDrivenMethodValue, error) {
			tree := providerRowRatchetTree(t)
			// both copath children of node 1, because "with respect to the copath child" is
			// the whole mechanism of a parent hash and a row reading one of the two would
			// pass over a body that hashed the same subtree whichever child it was handed
			withRight, err := tree.ParentHash(crypto, NodeIndex(1), NodeIndex(2))
			if err != nil {
				return nil, err
			}
			withLeft, err := tree.ParentHash(crypto, NodeIndex(1), NodeIndex(0))
			if err != nil {
				return nil, err
			}
			return []providerDrivenMethodValue{
				{name: "the parent hash of node 1 with copath child 2", content: withRight},
				{name: "the parent hash of node 1 with copath child 0", content: withLeft},
			}, nil
		}},
		// the other two answer a verdict and a count rather than bytes, so what they leave
		// behind is rendered. Both are driven over a tree whose chain was built through a
		// provider FIXED in the fixture rather than through the row's, which is what makes the
		// answer move: a body that took its hashes from somewhere other than the provider it
		// was handed reproduces that fixed chain under every provider and answers "verifies"
		// to all of them.
		//
		// The renderings are deliberately neither 32 nor 64 octets long, so the KDF.Nh
		// differential reads them as the non-digests they are rather than as a width that
		// failed to follow the provider.
		{name: "(*RatchetTree).parentHashClaimsUnder", call: func(t *testing.T, crypto CryptoProvider, take func([]byte) []byte) ([]providerDrivenMethodValue, error) {
			tree := providerRowChainedRatchetTree(t)
			// the *ParentNode is the caller's, so the arrays it carries go through the
			// recorder -- installed into the node itself rather than handed to SetParent,
			// which copies what it is given and would leave the recorder watching an array
			// this method never sees. The claimant's parent_hash is taken for the same
			// reason: it is the field the walk compares against.
			parent := tree.ParentAt(NodeIndex(1))
			if parent == nil {
				return nil, fmt.Errorf("node 1 of the row's tree is blank, so this row has no parent node to hand over")
			}
			parent.EncryptionKey = HpkePublicKey(take(parent.EncryptionKey))
			claimant := tree.Leaf(LeafIndex(0))
			if claimant == nil {
				return nil, fmt.Errorf("leaf 0 of the row's tree is blank, so this row observes no claim")
			}
			claimant.ParentHash = take(claimant.ParentHash)
			claims, err := tree.parentHashClaimsUnder(crypto, parent,
				NodeIndex(1), NodeIndex(0), NodeIndex(2))
			if err != nil {
				return nil, err
			}
			return []providerDrivenMethodValue{
				{name: "the claim count", content: []byte(fmt.Sprintf("claimants: %d", claims))},
			}, nil
		}},
		{name: "(*RatchetTree).VerifyParentHashes", call: func(t *testing.T, crypto CryptoProvider, take func([]byte) []byte) ([]providerDrivenMethodValue, error) {
			tree := providerRowChainedRatchetTree(t)
			verdict := []byte("the parent hashes verify")
			if refused := tree.VerifyParentHashes(crypto); refused != nil {
				verdict = []byte("the parent hashes are refused")
			}
			return []providerDrivenMethodValue{{name: "the verdict", content: verdict}}, nil
		}},
		// task 18's path generation, which is the one member of this class that mutates its
		// receiver and the one that samples entropy.
		//
		// The three values are all KDF.Nh wide and none of them is a public key, which is this
		// row's whole shape. X25519 fixes an HPKE public key at 32 octets whatever the kdf does,
		// so a row reporting one of the path's public keys would be read by the KDF.Nh
		// differential as a length written down rather than as the key it is -- the same
		// coincidence constructionsWhoseAnswerOnlyCoincidesWithKdfNh names for DeriveKeyPair.
		//
		// The ROUTING differential is weak over this row, and that is written down rather than
		// left to be discovered by somebody who reads it as one of the rows above. Every value
		// here descends from crypto.Random, so all three move across a provider that flips every
		// answer whether or not the ladder, the key derivation or the hash routed through the
		// parameter. What holds those instead is the KDF.Nh row -- a commit secret or a parent
		// hash taken outside the provider stays 32 octets over a provider whose Nh is 48 -- and
		// TestTheParentHashChainOfRfc9420AppendixBsWorkedExample, which compares the BYTES of the
		// chain this method writes against preimages spelled out from the RFC's own notation.
		//
		// The signer comes from a provider FIXED in the row rather than from the one under test,
		// because it is a caller's array here: what this method does with it is hand it to
		// SignWithLabel, and generating it through the parameter would make the retention gate
		// read a key the method never received from its caller.
		{name: "(*RatchetTree).CreateUpdatePathSecrets", call: func(t *testing.T, crypto CryptoProvider, take func([]byte) []byte) ([]providerDrivenMethodValue, error) {
			fixed := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
			signer, _, err := fixed.SignatureKeyPair()
			if err != nil {
				return nil, err
			}
			tree := providerRowRatchetTree(t)
			plan, err := tree.CreateUpdatePathSecrets(crypto, LeafIndex(0),
				SignaturePrivateKey(take(signer)), take([]byte("provider-row-group-id")))
			if err != nil {
				return nil, err
			}
			if len(plan.PathSecrets) == 0 {
				return nil, fmt.Errorf("the row's tree gave leaf 0 an empty filtered direct path, so this row observes nothing")
			}
			return []providerDrivenMethodValue{
				{name: "the commit secret", content: plan.CommitSecret},
				{name: "the path secret of the topmost node", content: plan.PathSecrets[len(plan.PathSecrets)-1]},
				{name: "the leaf's parent hash", content: plan.LeafNode.ParentHash},
			}, nil
		}},
		// task 20's sealing, over the same tree and the plan task 18's row above answers.
		//
		// The plan is built through the ROW'S provider and not through a fixed one, which is
		// this table's own rule: every value a row is built out of is cut to the provider's
		// KDF.Nh. A plan cut to a fixed 32 octet ladder would seal 32 octet plaintexts under a
		// provider whose Nh is 48, and the differential below would read the ciphertext that
		// came back as a width written down rather than as the plaintext plus tag it is.
		//
		// The values are CIPHERTEXTS and never kem outputs. A kem output is Nenc, which X25519
		// fixes at 32 whatever the kdf does, so reporting one would hand the KDF.Nh gate exactly
		// the coincidence constructionsWhoseAnswerOnlyCoincidesWithKdfNh records for
		// SealWithLabel. A ciphertext is the path secret plus the aead tag, so it is Nh+Nt over
		// both providers and is Nh under neither.
		//
		// The ROUTING differential is weak over this row for CreateUpdatePathSecrets' reason
		// one row up -- every value here descends from a plan the same provider produced, so all
		// of them move whether or not the seal routed through the parameter. What holds the seal
		// itself is TestEncryptUpdatePathPairsEachCiphertextWithItsOwnResolutionEntry, which
		// opens every ciphertext with the private key of the resolution entry standing at its
		// own index, and TestEncryptUpdatePathSealsUnderTheContextOfTheEpochTheCommitOpens,
		// which says which bytes went into the hpke info.
		//
		// The group context is the one caller array this method is handed, so it is the one
		// thing taken through the recorder; the signer and the group id belong to the call above
		// it and are held by that row.
		{name: "(*RatchetTree).EncryptUpdatePath", call: func(t *testing.T, crypto CryptoProvider, take func([]byte) []byte) ([]providerDrivenMethodValue, error) {
			fixed := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
			signer, _, err := fixed.SignatureKeyPair()
			if err != nil {
				return nil, err
			}
			tree := providerRowRatchetTree(t)
			plan, err := tree.CreateUpdatePathSecrets(crypto, LeafIndex(0),
				SignaturePrivateKey(signer), []byte("provider-row-group-id"))
			if err != nil {
				return nil, err
			}
			path, err := tree.EncryptUpdatePath(crypto, plan, LeafIndex(0),
				take([]byte("provider-row-group-context")), nil)
			if err != nil {
				return nil, err
			}
			values := []providerDrivenMethodValue{}
			for i := range path.Nodes {
				if len(path.Nodes[i].EncryptedPathSecret) == 0 {
					return nil, fmt.Errorf("path node %d published no ciphertext, so this row observes nothing there", i)
				}
				values = append(values, providerDrivenMethodValue{
					name:    fmt.Sprintf("the ciphertext sealed at path node %d", i),
					content: path.Nodes[i].EncryptedPathSecret[0].Ciphertext,
				})
			}
			if len(values) == 0 {
				return nil, fmt.Errorf("the row's tree gave leaf 0 an empty filtered direct path, so this row observes nothing")
			}
			return values, nil
		}},
		// task 21's merge. The path it installs is built through the ROW'S provider, so every
		// value below descends from a plan that provider produced -- the same weakness
		// EncryptUpdatePath's row records above it, and the same remedy: what holds the merge's
		// own bytes is TestEveryPublishedUpdatePathMergesToItsPublishedTreeHash, over trees and
		// paths other implementations produced.
		//
		// The values are what the merge COMPUTES rather than what it copies. Only the encryption
		// keys travel in section 7.6, so the parent hash chain is the receiver's own work; the
		// topmost node of a filtered path carries the zero-length octet string section 7.9 gives
		// the root and is therefore not among them, which is why the loop stops one short and
		// the tree hash of the merged tree is read as the second value. Both are Hash outputs and
		// so are KDF.Nh wide under either provider.
		//
		// Every array of the decoded path goes through the recorder, and that is the correction
		// this row carries. The first reading of the retention class asked whether a parameter
		// WAS a byte slice, so this method -- whose only caller arrays arrive inside the
		// *UpdatePath -- sat outside the gate by its signature, and the row said so as though
		// it were principled. It is not: path.Nodes[i].EncryptionKey, path.LeafNode.ParentHash
		// and path.LeafNode.Signature are decoded straight off the wire, the caller still owns
		// the buffer they were cut from, and what the merge keeps is state cut from them that
		// outlives the call. reachesByteSliceType is the reading that holds it, and the
		// encryption keys the merge INSTALLED are read below alongside the hashes it computes,
		// because those are the values a merge that skipped the copy would alias.
		{name: "(*RatchetTree).MergeUpdatePath", call: func(t *testing.T, crypto CryptoProvider, take func([]byte) []byte) ([]providerDrivenMethodValue, error) {
			fixed := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
			signer, _, err := fixed.SignatureKeyPair()
			if err != nil {
				return nil, err
			}
			sender := providerRowRatchetTree(t)
			plan, err := sender.CreateUpdatePathSecrets(crypto, LeafIndex(0),
				SignaturePrivateKey(signer), []byte("provider-row-group-id"))
			if err != nil {
				return nil, err
			}
			path, err := sender.EncryptUpdatePath(crypto, plan, LeafIndex(0),
				[]byte("provider-row-group-context"), nil)
			if err != nil {
				return nil, err
			}
			if len(plan.Path) < 2 {
				return nil, fmt.Errorf("the row's tree gave leaf 0 a filtered direct path of %d nodes, and this row needs one below the root to read",
					len(plan.Path))
			}
			// the caller's storage, replaced in place with arrays the recorder watches. The
			// bytes are the same ones, so nothing the merge checks answers differently; what
			// changes is that the arrays now belong to a caller this test can ask about.
			for i := range path.Nodes {
				path.Nodes[i].EncryptionKey = HpkePublicKey(take(path.Nodes[i].EncryptionKey))
				for j := range path.Nodes[i].EncryptedPathSecret {
					sealed := &path.Nodes[i].EncryptedPathSecret[j]
					sealed.KemOutput = take(sealed.KemOutput)
					sealed.Ciphertext = take(sealed.Ciphertext)
				}
			}
			path.LeafNode.EncryptionKey = HpkePublicKey(take(path.LeafNode.EncryptionKey))
			path.LeafNode.SignatureKey = SignaturePublicKey(take(path.LeafNode.SignatureKey))
			path.LeafNode.Credential.Identity = take(path.LeafNode.Credential.Identity)
			path.LeafNode.ParentHash = take(path.LeafNode.ParentHash)
			path.LeafNode.Signature = take(path.LeafNode.Signature)
			receiver := providerRowRatchetTree(t)
			if err := receiver.MergeUpdatePath(crypto, LeafIndex(0), path); err != nil {
				return nil, err
			}
			values := []providerDrivenMethodValue{}
			for _, x := range plan.Path[:len(plan.Path)-1] {
				parent := receiver.ParentAt(x)
				if parent == nil {
					return nil, fmt.Errorf("node %d is blank after the merge, so this row observes nothing there", x)
				}
				values = append(values, providerDrivenMethodValue{
					name:    fmt.Sprintf("the parent hash the merge installed at node %d", x),
					content: parent.ParentHash,
				})
			}
			// the keys the merge COPIED, one per node of the path including the topmost. An
			// X25519 public key is 32 octets whatever the kdf does, so the content is a
			// rendering and the array itself is named as storage -- see
			// providerDrivenMethodValue.storage. They still move across the two providers,
			// because the path they were copied from was built through the row's provider,
			// so the routing differential reads them as the derived values they descend from.
			for _, x := range plan.Path {
				parent := receiver.ParentAt(x)
				if parent == nil {
					return nil, fmt.Errorf("node %d is blank after the merge, so this row observes nothing there", x)
				}
				values = append(values, providerDrivenMethodValue{
					name:    fmt.Sprintf("the encryption key the merge installed at node %d", x),
					content: []byte(fmt.Sprintf("node %d encryption key: %s", x, HexOf(parent.EncryptionKey))),
					storage: parent.EncryptionKey,
				})
			}
			// and the leaf the merge installed, whose signature is 64 octets -- neither
			// provider's KDF.Nh, so it is read raw rather than rendered
			merged := receiver.Leaf(LeafIndex(0))
			if merged == nil {
				return nil, fmt.Errorf("leaf 0 is blank after the merge, so this row observes nothing there")
			}
			values = append(values, providerDrivenMethodValue{
				name: "the signature of the leaf the merge installed", content: merged.Signature,
			})
			treeHash, err := receiver.TreeHash(crypto)
			if err != nil {
				return nil, err
			}
			return append(values, providerDrivenMethodValue{
				name: "the tree hash of the merged tree", content: treeHash,
			}), nil
		}},
		// task 22's decrypt, over a two leaf tree whose receiving leaf carries a REAL key pair --
		// derived through a provider fixed here rather than through the row's, because the
		// tagging provider flips the two halves of a key pair independently and a leaf keyed
		// with it would hold a private key that is not the public one's. That is not a limit of
		// this row, it is what the routing differential reads: over the tagging provider the
		// seal and the open no longer meet, so the answer this row leaves behind is a refusal
		// where the real provider leaves a commit secret. A verdict that moves is what
		// Consistent's row below records for the same shape.
		//
		// The two values are KDF.Nh wide under both real providers, which is what the KDF.Nh gate
		// reads: a path secret and the commit secret are rungs of section 7.4's ladder and are
		// cut to the provider's own digest width. Neither is a key, so neither carries the X25519
		// coincidence constructionsWhoseAnswerOnlyCoincidesWithKdfNh records for DeriveKeyPair.
		//
		// The caller still owns the group context, the decoded path and the private state it
		// hands in, and all three go through the recorder. The path's arrays are taken AFTER
		// the merge, so a defect this row reports is the decrypt's rather than the merge's --
		// that method has a row of its own one entry up. The state's leaf key is the sharpest
		// of the three: NodePrivateKey answers a COPY of it precisely so the decrypt can erase
		// the copy after opening with it, and an arm that answered the live array instead
		// would have this call zeroing a key the caller is still using, which the recorder
		// reads as an argument that came back changed.
		{name: "(*RatchetTree).DecryptUpdatePath", call: func(t *testing.T, crypto CryptoProvider, take func([]byte) []byte) ([]providerDrivenMethodValue, error) {
			fixed := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
			signer, _, err := fixed.SignatureKeyPair()
			if err != nil {
				return nil, err
			}
			receiverPriv, receiverPub, err := fixed.DeriveKeyPair(bytes.Repeat([]byte{0xa1}, fixed.HashSize()))
			if err != nil {
				return nil, err
			}
			base := NewRatchetTree()
			for _, i := range []LeafIndex{0, 1} {
				leaf := testLeafNodeOfSource(LeafNodeSourceUpdate)
				leaf.EncryptionKey = HpkePublicKey(bytes.Repeat([]byte{0x40 + byte(i)}, 32))
				leaf.SignatureKey = SignaturePublicKey(bytes.Repeat([]byte{0x50 + byte(i)}, 32))
				if i == 1 {
					leaf.EncryptionKey = receiverPub
				}
				if err := base.SetLeaf(i, leaf); err != nil {
					return nil, err
				}
			}
			groupContext := take([]byte("provider-row-group-context"))
			sender := base.Clone()
			plan, err := sender.CreateUpdatePathSecrets(crypto, LeafIndex(0),
				SignaturePrivateKey(signer), []byte("provider-row-group-id"))
			if err != nil {
				return nil, err
			}
			path, err := sender.EncryptUpdatePath(crypto, plan, LeafIndex(0), groupContext, nil)
			if err != nil {
				return nil, err
			}
			if len(plan.Path) != 1 {
				return nil, fmt.Errorf("the row's two leaf tree gave leaf 0 a filtered direct path of %d nodes, want 1",
					len(plan.Path))
			}
			receiver := base.Clone()
			if err := receiver.MergeUpdatePath(crypto, LeafIndex(0), path); err != nil {
				return nil, err
			}
			for i := range path.Nodes {
				path.Nodes[i].EncryptionKey = HpkePublicKey(take(path.Nodes[i].EncryptionKey))
				for j := range path.Nodes[i].EncryptedPathSecret {
					sealed := &path.Nodes[i].EncryptedPathSecret[j]
					sealed.KemOutput = take(sealed.KemOutput)
					sealed.Ciphertext = take(sealed.Ciphertext)
				}
			}
			// installed into the state rather than handed to the constructor, which copies what
			// it is given and would leave the recorder watching an array this call never sees
			state := NewTreeKEMPrivate(LeafIndex(1), receiverPriv)
			state.EncryptionPriv = HpkePrivateKey(take(receiverPriv))
			got, err := receiver.DecryptUpdatePath(crypto, LeafIndex(0), path, groupContext, state, nil)
			if err != nil {
				// the refusal IS the answer over a provider whose key pairs do not meet, and it
				// is reported as a value rather than as an error so the differential can read it
				refused := []byte("DecryptUpdatePath refused: " + err.Error())
				return []providerDrivenMethodValue{
					{name: "the commit secret", content: refused},
					{name: "the path secret recovered at the entry point", content: refused},
					{name: "the leaf private key the answered state carries", content: refused},
				}, nil
			}
			recovered, held := got.Private.PathSecrets[plan.Path[0]]
			if !held {
				return nil, fmt.Errorf("the decrypt left no path secret at node %d, so this row observes nothing there",
					plan.Path[0])
			}
			return []providerDrivenMethodValue{
				{name: "the commit secret", content: got.CommitSecret},
				{name: "the path secret recovered at the entry point", content: recovered},
				// the answered state's own leaf key, which must be a copy of the one handed in
				// and not a window onto it: a state cut from the caller's is a rejected commit
				// that has already replaced the epoch the group is still running on. Rendered
				// for the KDF.Nh gate's sake -- an X25519 private key is 32 octets whatever the
				// kdf does -- with the array named as storage so the aliasing question still
				// has something raw to ask about. NOT marked carried, because over the tagging
				// provider this row answers a refusal rather than the key, which is the same
				// reason the two values above it move.
				{name: "the leaf private key the answered state carries",
					content: []byte("leaf private key: " + HexOf(got.Private.EncryptionPriv)),
					storage: got.Private.EncryptionPriv},
			}, nil
		}},
		// treekem.go's two. NodePrivateKey has both arms driven, because they are two
		// different answers and only one of them is derived: the node key comes back through
		// the provider and moves, and the member's own leaf key is STORED and must not. The
		// carried flag on the second is a real assertion rather than an exemption -- the gate
		// requires a carried value to be identical across the two providers -- and it is what
		// says the leaf arm answers the key it holds rather than deriving one from a rung of
		// the ladder, which is the confusion the file header of treekem.go is about.
		//
		// Both answers are RENDERED rather than returned raw. An X25519 private key is 32
		// octets whatever the kdf is, so a row answering the key itself would report a
		// coincidence to the KDF.Nh gate that belongs to the KEM and not to this method.
		{name: "(*TreeKEMPrivate).NodePrivateKey", call: func(t *testing.T, crypto CryptoProvider, take func([]byte) []byte) ([]providerDrivenMethodValue, error) {
			state := NewTreeKEMPrivate(LeafIndex(0), HpkePrivateKey(bytes.Repeat([]byte{0x91}, 32)))
			state.PathSecrets[NodeIndex(1)] = bytes.Repeat([]byte{0x92}, crypto.HashSize())
			derived, held, err := state.NodePrivateKey(crypto, NodeIndex(1))
			if err != nil {
				return nil, err
			}
			if !held {
				t.Fatal("the row holds a path secret for node 1 and NodePrivateKey answered that it has none")
			}
			own, held, err := state.NodePrivateKey(crypto, LeafIndex(0).NodeIndex())
			if err != nil {
				return nil, err
			}
			if !held {
				t.Fatal("NodePrivateKey answered that the member does not hold its own leaf key")
			}
			return []providerDrivenMethodValue{
				{name: "the key derived from the path secret at node 1", content: []byte("derived: " + HexOf(derived))},
				{name: "the member's own leaf key", content: []byte("own leaf: " + HexOf(own)), carried: true},
			}, nil
		}},
		// the private state's agreement with the tree, driven over a tree whose node key was
		// derived through a provider fixed HERE rather than through the row's. That is
		// providerRowChainedRatchetTree's reason one construction along: a tree keyed with the
		// row's own provider agrees with itself under every provider, so the verdict would be
		// the same over the real one and the tagging one and the differential would read
		// nothing. Built against a fixed provider, "agrees" is the answer only while the
		// method derives through the provider it was handed.
		//
		// The two renderings are deliberately neither 32 nor 48 octets long, so the KDF.Nh
		// differential reads them as the non-digests they are.
		// p7 task 16's joiner ladder. Two values, and they answer the three gates differently.
		// The rung at the node the joiner and the sender share is the caller's own path secret
		// CLONED, so it must be identical over a provider that flips every answer and must share
		// no storage with what was handed in; every rung above it is a DeriveSecret through the
		// provider and must move. A row that read only the first would report a clean run over a
		// body that laddered through a provider of its own, and one that read only the last would
		// say nothing about the clone -- which is the array a joiner's whole epoch is seeded from.
		//
		// leaf 0 is the sender and leaf 1 the joiner, over the tree this file already builds: its
		// filtered direct path from leaf 0 is two nodes, so the ladder has a rung to climb. A
		// shared node at the TOP of that path would give the ladder one rung and the routing half
		// would observe nothing.
		{name: "(*RatchetTree).installJoinerPathSecrets", call: func(t *testing.T, crypto CryptoProvider, take func([]byte) []byte) ([]providerDrivenMethodValue, error) {
			tree := providerRowRatchetTree(t)
			steps, err := tree.filteredPathSteps(LeafIndex(0))
			if err != nil {
				return nil, err
			}
			if len(steps) != 2 {
				return nil, fmt.Errorf("the row's tree gave leaf 0 a filtered direct path of %d nodes, want 2",
					len(steps))
			}
			state := NewTreeKEMPrivate(LeafIndex(1), HpkePrivateKey(bytes.Repeat([]byte{0x95}, 32)))
			// the caller's own array, which the ladder starts from and must clone rather than keep
			pathSecret := take(bytes.Repeat([]byte{0x96}, crypto.HashSize()))
			if err := tree.installJoinerPathSecrets(crypto, state, LeafIndex(1), LeafIndex(0),
				pathSecret); err != nil {
				return nil, err
			}
			shared, held := state.PathSecrets[steps[0].Node]
			if !held {
				return nil, fmt.Errorf("the ladder installed no secret at node %d, so this row observes nothing at the shared node",
					steps[0].Node)
			}
			above, held := state.PathSecrets[steps[1].Node]
			if !held {
				return nil, fmt.Errorf("the ladder installed no secret at node %d, so this row observes nothing above the shared node",
					steps[1].Node)
			}
			return []providerDrivenMethodValue{
				{name: "the rung at the node the joiner and the sender share", content: shared, carried: true},
				{name: "the rung above it", content: above},
			}, nil
		}},
		{name: "(*TreeKEMPrivate).Consistent", call: func(t *testing.T, crypto CryptoProvider, take func([]byte) []byte) ([]providerDrivenMethodValue, error) {
			fixed := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
			pathSecret := bytes.Repeat([]byte{0x93}, fixed.HashSize())
			_, pub, err := DeriveNodeKeyPair(fixed, pathSecret)
			if err != nil {
				return nil, err
			}
			tree := providerRowRatchetTree(t)
			if err := tree.SetParent(NodeIndex(1), &ParentNode{EncryptionKey: pub}); err != nil {
				return nil, err
			}
			// the tree is the caller's, so the arrays this method actually reads off it go
			// through the recorder -- installed into the nodes rather than handed to SetParent,
			// which copies what it is given and would leave the recorder watching an array this
			// method never sees. The parent's encryption key is the one the comparison is
			// against; the leaf's is what the blank-leaf clause reads the node for.
			installed := tree.ParentAt(NodeIndex(1))
			if installed == nil {
				return nil, fmt.Errorf("node 1 of the row's tree is blank, so this row observes nothing there")
			}
			installed.EncryptionKey = HpkePublicKey(take(installed.EncryptionKey))
			leaf := tree.Leaf(LeafIndex(0))
			if leaf == nil {
				return nil, fmt.Errorf("leaf 0 of the row's tree is blank, so this row observes nothing there")
			}
			leaf.EncryptionKey = HpkePublicKey(take(leaf.EncryptionKey))
			state := NewTreeKEMPrivate(LeafIndex(0), HpkePrivateKey(bytes.Repeat([]byte{0x94}, 32)))
			state.PathSecrets[NodeIndex(1)] = pathSecret
			verdict := []byte("Consistent: the path secret derives the key the tree carries")
			if refused := state.Consistent(crypto, tree); refused != nil {
				verdict = []byte("Consistent: refused")
			}
			return []providerDrivenMethodValue{{name: "the verdict", content: verdict}}, nil
		}},
		// p7 task 6's proposal cache. Two values, answering the three gates differently: the
		// REFERENCE is derived through the provider and must move over one that flips every
		// answer, and the cached extension body is the caller's own and must neither move nor
		// share storage with what was handed in.
		//
		// The proposal is a group_context_extensions and not a remove, because a remove is a
		// leaf index and carries no octets at all -- a row built on one would hand nothing
		// through the recorder and the retention half of this file would observe nothing while
		// reporting a clean run.
		{name: "(*ProposalCache).Store", call: func(t *testing.T, crypto CryptoProvider, take func([]byte) []byte) ([]providerDrivenMethodValue, error) {
			// the cache is bound to the group and epoch this row's content names,
			// out of a context of the caller's own rather than out of the content --
			// which is also why the group id below is recorded once for the message
			// and built separately for the binding
			context := testResolveContextAt(bytes.Repeat([]byte{0x61}, 12), 9)
			cache := testCacheAt(t, context)
			content := &AuthenticatedContent{
				WireFormat: WireFormatPublicMessage,
				Content: FramedContent{
					GroupId:     take(bytes.Repeat([]byte{0x61}, 12)),
					Epoch:       9,
					Sender:      Sender{SenderType: SenderTypeMember, LeafIndex: 1},
					ContentType: ContentTypeProposal,
					Proposal: &Proposal{
						ProposalType: ProposalTypeGroupContextExtensions,
						GroupContextExtensions: &GroupContextExtensions{Extensions: []Extension{{
							ExtensionType: ExtensionTypeRequiredCapabilities,
							ExtensionData: take(bytes.Repeat([]byte{0x62}, 12)),
						}}},
					},
				},
				Auth: FramedContentAuthData{Signature: take(bytes.Repeat([]byte{0x63}, 64))},
			}
			ref, err := cache.Store(crypto, context, content)
			if err != nil {
				return nil, err
			}
			cached, held := cache.Cached(context, ref)
			if !held {
				return nil, fmt.Errorf("the cache missed the reference it had just answered")
			}
			return []providerDrivenMethodValue{
				{name: "ProposalRef", content: ref},
				{name: "the cached extension body", carried: true,
					content: cached.Proposal.GroupContextExtensions.Extensions[0].ExtensionData},
			}, nil
		}},
		// resolution, which reaches the provider for nothing at all -- see the excuse in
		// providerDrivenMethodsOverAnyProvider. Its one value is the caller's own extension body
		// carried through the copy, which is what the retention half reads: an applier walking
		// the resolved list must not be walking the array the commit was decoded out of.
		{name: "(*ProposalCache).Resolve", call: func(t *testing.T, crypto CryptoProvider, take func([]byte) []byte) ([]providerDrivenMethodValue, error) {
			list, err := testCache(t).Resolve(crypto, testResolveContext(), LeafIndex(3), []ProposalOrRef{{
				Type: ProposalOrRefTypeProposal,
				Proposal: &Proposal{
					ProposalType: ProposalTypeGroupContextExtensions,
					GroupContextExtensions: &GroupContextExtensions{Extensions: []Extension{{
						ExtensionType: ExtensionTypeRequiredCapabilities,
						ExtensionData: take(bytes.Repeat([]byte{0x64}, 12)),
					}}},
				},
			}})
			if err != nil {
				return nil, err
			}
			extensions, proposed := list.Extensions()
			if !proposed || len(extensions) != 1 {
				return nil, fmt.Errorf("the resolved list carries %d extensions, want 1", len(extensions))
			}
			return []providerDrivenMethodValue{
				{name: "the resolved extension body", content: extensions[0].ExtensionData, carried: true},
			}, nil
		}},
		// p7 task 14's group info signing half. What it leaves behind is a signature over the
		// GroupInfoTBS taken through the provider it was handed -- a body that reached for
		// ed25519 itself would answer a signature that verifies against every GroupInfo in this
		// package and against every published welcome, because the corpora are all Ed25519,
		// which is the scheme it would have hardcoded.
		//
		// The private key is a fixed 32 octets rather than one drawn through the provider, so
		// this row does not consume a stream another row is positioned in, and it is taken
		// through the recorder because it is the caller's.
		{name: "(*GroupInfo).Sign", call: func(t *testing.T, crypto CryptoProvider, take func([]byte) []byte) ([]providerDrivenMethodValue, error) {
			info, err := providerRowGroupInfo(t, crypto, providerRowRatchetTree(t))
			if err != nil {
				return nil, err
			}
			if err := info.Sign(crypto, SignaturePrivateKey(take(bytes.Repeat([]byte{0x73}, 32)))); err != nil {
				return nil, err
			}
			return []providerDrivenMethodValue{{name: "Signature", content: info.Signature}}, nil
		}},
		// its verifying half, whose whole answer is a yes or a no, so this row reads the VERDICT
		// for the reason (*LeafNode).VerifySignature's does: the tagging provider passes
		// VerifyWithLabel through unchanged, and there is nothing in a refusal for a flip to
		// change.
		//
		// The group info is signed through the same provider it is then verified against, which
		// is what makes the verdict move: over a provider whose signing half flips its answer, a
		// verifier that routed through that provider refuses and one that reached for ed25519 on
		// its own accepts exactly as it did over the real provider.
		//
		// The signer's public key is installed through (*RatchetTree).Leaf and not through
		// SetLeaf, and that is what puts a caller's array where the retention gate can see it:
		// SetLeaf stores a COPY, so an array handed to it is not the array Verify reads the
		// signer's key out of. This is also the one input a GroupInfo does not carry -- the
		// verification key is the tree's -- so it is the array that matters here.
		{name: "(*GroupInfo).Verify", call: func(t *testing.T, crypto CryptoProvider, take func([]byte) []byte) ([]providerDrivenMethodValue, error) {
			signer := SignaturePrivateKey(bytes.Repeat([]byte{0x74}, 32))
			signerPub, err := signaturePublicKeyOf(signer)
			if err != nil {
				return nil, err
			}
			tree := providerRowRatchetTree(t)
			tree.Leaf(LeafIndex(0)).SignatureKey = SignaturePublicKey(take(signerPub))
			info, err := providerRowGroupInfo(t, crypto, tree)
			if err != nil {
				return nil, err
			}
			if err := info.Sign(crypto, signer); err != nil {
				return nil, err
			}
			verdict := []byte("the group info verifies")
			if refused := info.Verify(crypto, tree); refused != nil {
				verdict = []byte("the group info is refused: " + refused.Error())
			}
			return []providerDrivenMethodValue{{name: "the verdict", content: verdict}}, nil
		}},
		// p7 task 15's door onto a verified group context, which is that verifying half plus a
		// decode. It is driven rather than excused because what it answers is the value a
		// proposal cache binds an epoch to: a body that reached for its own ed25519 would vouch
		// for a group info nobody in the tree signed, and the cache would bind to whatever that
		// group info named.
		//
		// TWO VALUES, because the two things that could route round the provider are separate.
		// The verdict moves when the signature check goes through the provider it was handed.
		// The confirmed transcript hash moves with the provider's KDF.Nh -- the fixture cuts it
		// to that width -- so it is what says the context that came back is the one that was
		// signed rather than something reassembled at a length written down here. The refusing
		// path answers a marker of its own so that both providers leave the same number of
		// values behind, which is what the differentials compare over.
		{name: "(*GroupInfo).VerifiedContext", call: func(t *testing.T, crypto CryptoProvider, take func([]byte) []byte) ([]providerDrivenMethodValue, error) {
			signer := SignaturePrivateKey(bytes.Repeat([]byte{0x75}, 32))
			signerPub, err := signaturePublicKeyOf(signer)
			if err != nil {
				return nil, err
			}
			tree := providerRowRatchetTree(t)
			tree.Leaf(LeafIndex(0)).SignatureKey = SignaturePublicKey(take(signerPub))
			info, err := providerRowGroupInfo(t, crypto, tree)
			if err != nil {
				return nil, err
			}
			if err := info.Sign(crypto, signer); err != nil {
				return nil, err
			}
			verdict := []byte("this group context was vouched for")
			transcript := []byte("nothing was vouched for at all")
			verified, refused := info.VerifiedContext(crypto, tree)
			if refused != nil {
				verdict = []byte("the group info is refused: " + refused.Error())
			} else {
				transcript = verified.Context().ConfirmedTranscriptHash
			}
			return []providerDrivenMethodValue{
				{name: "the verdict", content: verdict},
				{name: "the confirmed transcript hash of the context it vouched for", content: transcript},
			}, nil
		}},
	}
}

// providerRowChainedRatchetTree is providerRowRatchetTree's two leaf cousin, carrying one valid
// RFC 9420 section 7.9.1 parent hash chain.
//
// The chain is computed through a provider fixed HERE and not through the one the row is being
// driven with, and that is the whole point of the fixture. A tree whose chain was built with the
// row's own provider chains under every provider, so the verdict it produces is the same over
// the real one and over the tagging one, and the routing differential would have nothing to
// read. Built once against a fixed provider, the verdict is "verifies" for a body that hashes
// through the provider it was handed and "verifies" for one that hardcoded SHA-256 only when
// those two providers agree -- which is exactly the separation the gate is after.
func providerRowChainedRatchetTree(t *testing.T) *RatchetTree {
	t.Helper()
	fixed := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	tree := NewRatchetTree()
	for _, i := range []LeafIndex{0, 1} {
		leaf := testLeafNodeOfSource(LeafNodeSourceUpdate)
		leaf.EncryptionKey = HpkePublicKey(bytes.Repeat([]byte{0x60 + byte(i)}, 32))
		leaf.SignatureKey = SignaturePublicKey(bytes.Repeat([]byte{0x70 + byte(i)}, 32))
		if err := tree.SetLeaf(i, leaf); err != nil {
			t.Fatalf("SetLeaf(%d): %v", i, err)
		}
	}
	// node 1 is the root of a two leaf tree, so section 7.9 gives it the zero-length
	// parent_hash field and the chain is one link long.
	if err := tree.SetParent(NodeIndex(1), &ParentNode{
		EncryptionKey: HpkePublicKey(bytes.Repeat([]byte{0x61}, 32)),
		ParentHash:    []byte{},
	}); err != nil {
		t.Fatalf("SetParent(1): %v", err)
	}
	hash, err := tree.ParentHash(fixed, NodeIndex(1), NodeIndex(2))
	if err != nil {
		t.Fatalf("ParentHash: %v", err)
	}
	claimant := testLeafNodeOfSource(LeafNodeSourceCommit)
	claimant.EncryptionKey = HpkePublicKey(bytes.Repeat([]byte{0x60}, 32))
	claimant.SignatureKey = SignaturePublicKey(bytes.Repeat([]byte{0x70}, 32))
	claimant.ParentHash = hash
	if err := tree.SetLeaf(LeafIndex(0), claimant); err != nil {
		t.Fatalf("SetLeaf(0): %v", err)
	}
	// the fixture's own claim, so a row driven over this tree cannot be reading a refusal that
	// was already there before the provider was varied
	if err := tree.VerifyParentHashes(fixed); err != nil {
		t.Fatalf("the row's tree does not verify under the provider its chain was built with: %v", err)
	}
	return tree
}

// providerRowRatchetTree is the tree the four tree hash rows run over, built without touching a
// provider at all.
//
// Three occupied leaves in a four wide tree with one parent node set, which is the smallest
// shape that reaches both arms of the section 7.8 select and both kinds of position on each:
// an occupied leaf, a blank leaf, an occupied parent and a blank parent. The leaves carry
// filler key material because nothing here verifies them -- what these rows read is a digest --
// and signing them through the provider under test is exactly what would make the routing
// differential report the signer rather than the hash.
func providerRowRatchetTree(t *testing.T) *RatchetTree {
	tree := NewRatchetTree()
	for _, i := range []LeafIndex{0, 1, 3} {
		// this file's own encodable leaf, with its two keys moved apart per index so no two
		// leaves of the tree hash alike for a reason that is their position rather than their
		// content. The source is update because that arm carries neither a lifetime nor a
		// parent hash, so the leaf encodes with nothing that has to be made up.
		leaf := testLeafNodeOfSource(LeafNodeSourceUpdate)
		leaf.EncryptionKey = HpkePublicKey(bytes.Repeat([]byte{0x20 + byte(i)}, 32))
		leaf.SignatureKey = SignaturePublicKey(bytes.Repeat([]byte{0x30 + byte(i)}, 32))
		if err := tree.SetLeaf(i, leaf); err != nil {
			t.Fatalf("SetLeaf(%d): %v", i, err)
		}
	}
	if err := tree.SetParent(NodeIndex(1), &ParentNode{
		EncryptionKey:  HpkePublicKey(bytes.Repeat([]byte{0x51}, 32)),
		UnmergedLeaves: []LeafIndex{1},
	}); err != nil {
		t.Fatalf("SetParent(1): %v", err)
	}
	if tree.NodeWidth() != 7 {
		t.Fatalf("the row's tree is %d nodes wide, want 7", tree.NodeWidth())
	}
	return tree
}

// providerRowGroupInfo is the p7 task 14 GroupInfo the two rows below drive, over the tree it is
// handed and with every derived field cut to that provider's own KDF.Nh.
//
// The tree hash is taken through the ROW's provider rather than through one fixed here, which is
// the opposite of what providerRowChainedRatchetTree does and is right for the opposite reason.
// What the rows below read is a signature and a verdict; a group context whose tree hash was
// computed under some other provider would make (*GroupInfo).Verify refuse over every provider,
// and the verdict would then be constant for a reason that is this fixture's rather than the
// method's.
//
// It is called AFTER any leaf of the tree has been settled, because the hash it writes into the
// group context is the hash of the tree as it stands.
func providerRowGroupInfo(t *testing.T, crypto CryptoProvider, tree *RatchetTree) (*GroupInfo, error) {
	t.Helper()
	treeHash, err := tree.TreeHash(crypto)
	if err != nil {
		return nil, err
	}
	return &GroupInfo{
		GroupContext: GroupContext{
			Version:                 ProtocolVersionMls10,
			CipherSuite:             CipherSuiteX25519ChaCha20Sha256Ed25519,
			GroupId:                 []byte("the group this info describes"),
			Epoch:                   9,
			TreeHash:                treeHash,
			ConfirmedTranscriptHash: bytes.Repeat([]byte{0x71}, crypto.HashSize()),
		},
		ConfirmationTag: bytes.Repeat([]byte{0x72}, crypto.HashSize()),
		Signer:          LeafIndex(0),
	}, nil
}

// providerDrivenMethods is every method this package's non test source declares whose
// parameters include a CryptoProvider, read off the type checker.
//
// The same reading providerConstructions makes, with the receiver filter turned the other
// way round. It is the compiler's view of the signature rather than the spelling the line
// gives the parameter, so a method taking an interface that embeds the provider is a member
// of the class and a method taking something merely named CryptoProvider is not.
//
// Absence is fatal, for the reason every other derivation here is: a filter that stopped
// matching leaves the gates reading it demanding nothing, and a gate that demands nothing
// reports exactly what a complete one reports.
func providerDrivenMethods(t *testing.T) []declaredFunction {
	t.Helper()
	provider := providerInterfaceType(t)
	methods := []declaredFunction{}
	for _, function := range declaredFunctionsOf(t, cryptoOwnRoot) {
		if !function.method || len(function.takes(provider)) == 0 {
			continue
		}
		methods = append(methods, function)
	}
	if len(methods) == 0 {
		t.Fatalf("no method of this package takes a %s, so every gate in this file demands nothing",
			providerInterfaceName)
	}
	return methods
}

// The names of those methods, sorted.
func providerDrivenMethodNames(t *testing.T) []string {
	t.Helper()
	names := []string{}
	for _, method := range providerDrivenMethods(t) {
		names = append(names, method.name)
	}
	slices.Sort(names)
	return names
}

// isByteSliceType reports whether the compiler reads a type as a slice of octets, under
// whatever name it is spelled.
//
// Underlying on both halves is what makes a named storage type a member: HpkePublicKey is a
// caller's array exactly as a []byte is, and a filter matching the spelling alone would drop
// it. The element is compared as a kind rather than as a name, because byte and uint8 are one
// type written two ways.
func isByteSliceType(of types.Type) bool {
	slice, isSlice := of.Underlying().(*types.Slice)
	if !isSlice {
		return false
	}
	element, isBasic := slice.Elem().Underlying().(*types.Basic)
	return isBasic && element.Kind() == types.Uint8
}

// reachesByteSliceType reports whether a value of this type can hold a caller's array: itself,
// or through any pointer, slice, array, map or struct field it is built out of.
//
// The DEPTH is what this adds over isByteSliceType, and it is a correction rather than a
// refinement. "Handed a caller's array" is a property of what a parameter REACHES and not of
// how its outermost type is spelled. An *UpdatePath is a struct pointer whose fields are the
// encryption keys, the parent hash and the signature a caller decoded off the wire, so a
// method taking one is handed exactly as much of somebody else's storage as a method taking
// the []byte directly -- and the first version of this filter, reading only the outermost
// type, left MergeUpdatePath outside the retention gate for that reason alone. Measured, not
// supposed: with the merge installing the caller's own EncryptionKey array into the tree
// instead of a copy of it, the whole of mls and message stayed green.
//
// Interfaces are not descended into, and that is a decision rather than an omission. An
// interface value's storage belongs to whatever implements it, the type checker cannot say
// what that is, and descending into method signatures would put every method here in the
// class by way of the CryptoProvider itself -- the one argument in all of these signatures
// that is NOT a caller's array.
//
// The walk is bounded by the types already on it, because a type may refer to itself: a Node
// holds a *LeafNode and this package's tree types are mutually recursive. A type already
// being visited answers no, which is the ordinary reading of reachability -- whatever it can
// reach is reachable through the visit that is still open.
func reachesByteSliceType(of types.Type, visiting map[types.Type]bool) bool {
	if of == nil || visiting[of] {
		return false
	}
	visiting[of] = true
	if isByteSliceType(of) {
		return true
	}
	switch under := of.Underlying().(type) {
	case *types.Pointer:
		return reachesByteSliceType(under.Elem(), visiting)
	case *types.Slice:
		return reachesByteSliceType(under.Elem(), visiting)
	case *types.Array:
		return reachesByteSliceType(under.Elem(), visiting)
	case *types.Chan:
		return reachesByteSliceType(under.Elem(), visiting)
	case *types.Map:
		return reachesByteSliceType(under.Key(), visiting) ||
			reachesByteSliceType(under.Elem(), visiting)
	case *types.Struct:
		for i := 0; i < under.NumFields(); i++ {
			if reachesByteSliceType(under.Field(i).Type(), visiting) {
				return true
			}
		}
	}
	return false
}

// providerDrivenMethodNamesTakingCallerBytes is the subset of the class above that is handed
// an array the caller still owns.
//
// Derived rather than listed, and derived as the property itself: a method that can reach no
// byte slice through any of its parameters cannot write into a caller's array and cannot keep
// one, so it is outside the retention gate by what its signature is rather than by an excuse
// somebody wrote. The reading is reachesByteSliceType's rather than isByteSliceType's, which
// is the whole of the difference between this gate holding the methods that take wire-decoded
// structures and this gate skipping them.
func providerDrivenMethodNamesTakingCallerBytes(t *testing.T) []string {
	t.Helper()
	names := []string{}
	for _, method := range providerDrivenMethods(t) {
		for i := 0; i < method.signature.Params().Len(); i++ {
			if reachesByteSliceType(method.signature.Params().At(i).Type(), map[types.Type]bool{}) {
				names = append(names, method.name)
				break
			}
		}
	}
	if len(names) == 0 {
		t.Fatalf("no method of this package is handed both a %s and a caller's array, so the retention gate demands nothing",
			providerInterfaceName)
	}
	slices.Sort(names)
	return names
}

// The rows this file declares, checked against the class and answered in the class's order.
//
// Both directions are checked. A member with no row is a method nothing below runs, and a row
// naming a method this package does not declare is a row that outlived what it covered.
func providerDrivenMethodRowsFor(t *testing.T, gate string, class []string) []providerDrivenMethodRow {
	t.Helper()
	byName := map[string]providerDrivenMethodRow{}
	for _, row := range providerDrivenMethodRows() {
		if _, repeated := byName[row.name]; repeated {
			t.Fatalf("providerDrivenMethodRows declares %s twice, so one of the two is never run", row.name)
		}
		byName[row.name] = row
	}
	declared := providerDrivenMethodNames(t)
	for name := range byName {
		if !slices.Contains(declared, name) {
			t.Errorf("providerDrivenMethodRows names %s, and no method of this package takes a %s under that name",
				name, providerInterfaceName)
		}
	}
	rows := []providerDrivenMethodRow{}
	for _, name := range class {
		row, written := byName[name]
		if !written {
			t.Errorf("%s: %s is handed a %s and has no row, so nothing holds it",
				gate, name, providerInterfaceName)
			continue
		}
		rows = append(rows, row)
	}
	return rows
}

// A method handed a provider that the routing gate cannot hold, named with the reason. Every
// name here is checked against the derived class below, AND against the row's own shape, so
// an entry cannot outlive the method it excuses and cannot excuse one that has provider
// derived state to compare.
var providerDrivenMethodsOverAnyProvider = map[string]string{
	// Validate reaches the provider for a length and for nothing else, and leaves nothing
	// behind but what it was given. The tagging provider passes HashSize through -- it has
	// no bytes to flip in an int -- so what this holds after a call over the tagging
	// provider is what it holds after a call over the real one, and a row here would report
	// "did not route through its provider" for every possible implementation. It is the
	// same limit labelConstructionsOverAnyProvider records for ZeroSecret, and it is not
	// unheld: the KDF.Nh gate below holds the length it refuses against a provider whose Nh
	// is not 32, which is the whole of what it uses the provider for.
	"(*PreSharedKeyId).Validate": "reads a length off the provider and nothing else, so a provider that flips every answer cannot separate it from a literal",
	// resolution turns a commit's ProposalOrRef vector into a bucketed list out of entries the
	// cache already holds, and reaches the provider for nothing: the reference is the map key
	// and the copy is the codec's. A provider that flips every answer therefore cannot separate
	// it from a body handed none, and a row here would report "did not route through its
	// provider" for every possible implementation. The parameter is in the signature because
	// the group lifecycle plan pins it there and every caller already holds one; what holds it
	// is the nil provider gate next door, which demands the refusal rather than a dereference.
	"(*ProposalCache).Resolve": "reaches the provider for nothing at all, so a provider that flips every answer cannot separate it from a body handed none",
}

// TestEveryMethodHandedAProviderRoutesThroughIt is
// TestEveryConstructionHandedAProviderRoutesThroughIt over the other half of the partition.
//
// The property is the one the parameter exists for. A method that reached for crypto/sha256
// directly, or built a provider out of a hardcoded suite, agrees with every corpus in this
// package, because the corpora are all X25519/SHA-256 -- the suite it would have hardcoded.
// It matters most on the transcript: the transcript is the one value a group cannot disagree
// about and recover from, so a hash taken under a suite of the method's own choosing is a
// permanent fork rather than a retryable failure.
//
// What separates the two is a provider that answers differently. Over the tagging provider a
// value the method derived through its parameter moves, and one it derived through a provider
// of its own does not. Each value is compared on its own, because a method that routed one of
// two through its parameter is exactly the defect a joined comparison cannot see.
func TestEveryMethodHandedAProviderRoutesThroughIt(t *testing.T) {
	class := providerDrivenMethodNames(t)
	want := []string{}
	for _, name := range class {
		if _, isExcused := providerDrivenMethodsOverAnyProvider[name]; !isExcused {
			want = append(want, name)
		}
	}
	// the provider underneath draws from a constant reader so nothing a row reaches can
	// answer differently on a second call for a reason that is the entropy's
	plain := mustProviderOver(t, CipherSuiteX25519ChaCha20Sha256Ed25519, constantReader{value: 0x35})
	held := func(row providerDrivenMethodRow, crypto CryptoProvider) []providerDrivenMethodValue {
		values, err := row.call(t, crypto, func(content []byte) []byte { return content })
		if err != nil {
			t.Fatalf("%s: %v", row.name, err)
		}
		return values
	}
	compared := 0
	for _, row := range providerDrivenMethodRowsFor(t, "the routing gate", want) {
		tagging := &taggingCryptoProvider{inner: plain}
		// with a panic caught rather than taken, for the reason recoveringRow gives: a row
		// that panics on inputs this test chose would otherwise take the test binary down
		// and every gate declared after this one with it
		overTheRealProvider, raised := recoveringRow(func() []providerDrivenMethodValue { return held(row, plain) })
		if raised != nil {
			t.Errorf("%s panicked with %v rather than answering", row.name, raised)
			continue
		}
		overTheTaggingProvider, raised := recoveringRow(func() []providerDrivenMethodValue { return held(row, tagging) })
		if raised != nil {
			t.Errorf("%s panicked with %v over the tagging provider; it called %v", row.name, raised, tagging.calls)
			continue
		}
		if len(overTheRealProvider) == 0 || len(overTheTaggingProvider) != len(overTheRealProvider) {
			t.Errorf("%s left %d values behind over the real provider and %d over the tagging one, so nothing below is compared",
				row.name, len(overTheRealProvider), len(overTheTaggingProvider))
			continue
		}
		derived := 0
		for i, value := range overTheRealProvider {
			flipped := overTheTaggingProvider[i]
			if len(value.content) == 0 {
				t.Errorf("%s left nothing behind in %s, so that value observed nothing", row.name, value.name)
				continue
			}
			if value.carried {
				if !bytes.Equal(value.content, flipped.content) {
					t.Errorf("%s is named as carrying %s through rather than deriving it, and it came back %x over the real provider and %x over one that flips every answer",
						row.name, value.name, value.content, flipped.content)
				}
				continue
			}
			derived++
			if bytes.Equal(value.content, flipped.content) {
				t.Errorf("%s left the same %s behind over a provider that flips every answer, so that value did not route through the provider it was handed; it called %v",
					row.name, value.name, tagging.calls)
			}
		}
		if derived == 0 {
			t.Errorf("%s leaves nothing behind that it derived, so this row holds it to nothing; excuse it in providerDrivenMethodsOverAnyProvider or give it a value to compare",
				row.name)
			continue
		}
		compared++
	}
	// and the excuse is held to the row's own shape as well as to the class: a method left
	// out of the differential must have nothing for the differential to read
	for name, reason := range providerDrivenMethodsOverAnyProvider {
		if !slices.Contains(class, name) {
			t.Errorf("the gate excuses %s, and no method of this package takes a %s under that name",
				name, providerInterfaceName)
			continue
		}
		for _, row := range providerDrivenMethodRows() {
			if row.name != name {
				continue
			}
			for _, value := range held(row, plain) {
				if !value.carried {
					t.Errorf("%s is excused from the routing differential as %q, and its row leaves %s behind as a derived value, which the differential could have held",
						name, reason, value.name)
				}
			}
		}
	}
	if compared != len(want) {
		t.Fatalf("%d of the %d methods the routing differential covers were compared", compared, len(want))
	}
	t.Logf("%d of the %d methods handed a %s held to routing through it", len(want), len(class), providerInterfaceName)
}

// TestEveryMethodHandedAProviderReadsKdfNhFromIt is the differential the registered suites
// cannot supply, over the method half of the partition.
//
// Both registered suites fix KDF.Nh at 32, so nothing already in this tree separates a method
// that reads the length off the provider it was handed from one that writes 32 down. The
// second provider is the registered suite with its whole hash and kdf surface one width up,
// and the rows are cut to whichever provider they are running under.
//
// Two things are compared, because KDF.Nh governs two separate things here. A method that
// wrote the length down REFUSES an input cut to the wide provider's own Nh, which is the
// refusal the first half reads; and a method that read the provider for its refusal and wrote
// 32 down for what it produced leaves a value of the wrong width behind, which is what
// kdfNhCoincidences reads. Either half alone is satisfiable by the other mistake.
func TestEveryMethodHandedAProviderReadsKdfNhFromIt(t *testing.T) {
	class := providerDrivenMethodNames(t)
	narrow := mustProviderOver(t, CipherSuiteX25519ChaCha20Sha256Ed25519, constantReader{value: 0x35})
	wide := &wideKdfProvider{CryptoProvider: narrow}
	if narrow.HashSize() == wide.HashSize() {
		t.Fatalf("both providers answer KDF.Nh %d, so every row below compares a length against itself",
			narrow.HashSize())
	}
	contents := func(values []providerDrivenMethodValue) [][]byte {
		out := [][]byte{}
		for _, value := range values {
			out = append(out, value.content)
		}
		return out
	}
	compared := 0
	for _, row := range providerDrivenMethodRowsFor(t, "the KDF.Nh gate", class) {
		overTheNarrowProvider, err := row.call(t, narrow, func(content []byte) []byte { return content })
		if err != nil {
			t.Errorf("%s refused inputs cut to the narrow provider's KDF.Nh of %d: %v",
				row.name, narrow.HashSize(), err)
			continue
		}
		overTheWideProvider, err := row.call(t, wide, func(content []byte) []byte { return content })
		if err != nil {
			t.Errorf("%s refused inputs cut to the KDF.Nh of %d the provider it was handed answers, so it is holding a length of its own rather than reading that provider: %v",
				row.name, wide.HashSize(), err)
			continue
		}
		if len(overTheNarrowProvider) == 0 || len(overTheWideProvider) != len(overTheNarrowProvider) {
			t.Errorf("%s left %d values behind over the narrow provider and %d over the wide one, so nothing below is compared",
				row.name, len(overTheNarrowProvider), len(overTheWideProvider))
			continue
		}
		for _, at := range kdfNhCoincidences(contents(overTheNarrowProvider), contents(overTheWideProvider),
			narrow.HashSize(), wide.HashSize()) {
			t.Errorf("%s left %s behind at %d bytes over a provider whose KDF.Nh is %d and at %d bytes over one whose KDF.Nh is %d; one of the two is a length written down rather than read",
				row.name, overTheNarrowProvider[at].name, len(overTheNarrowProvider[at].content),
				narrow.HashSize(), len(overTheWideProvider[at].content), wide.HashSize())
		}
		compared++
	}
	if compared != len(class) {
		t.Fatalf("%d of the %d methods handed a %s were compared across the two providers",
			compared, len(class), providerInterfaceName)
	}
}

// TestNoMethodHandedAProviderRetainsOrRewritesTheCallerBytes is
// TestEveryConstructionInThisPackageLeavesItsInputAlone over the other half of the partition,
// and it is the half where retention actually happens.
//
// A construction answers and is done with what it was handed. A method leaves state behind,
// and state outlives the array it was cut from. The joiner's confirmed transcript hash is the
// case: it is a field of a decoded GroupInfo, the caller still owns the message it decoded it
// out of and goes on reading later fields from that buffer, and a joiner that kept the slice
// holds a transcript that changes underneath it with no error path anywhere. The next commit
// is then chained from bytes no peer has, which is a permanent fork rather than an operation
// that failed.
//
// Both directions of sharing are read. The recorder's arrays carry a pattern in their spare
// capacity rather than zeros, so a method that appended a byte to save an allocation is
// visible; and every byte the receiver holds is compared against every byte of every argument,
// so state cut from the middle of a caller's buffer is as visible as state cut from its front.
func TestNoMethodHandedAProviderRetainsOrRewritesTheCallerBytes(t *testing.T) {
	class := providerDrivenMethodNamesTakingCallerBytes(t)
	crypto := mustProviderOver(t, CipherSuiteX25519ChaCha20Sha256Ed25519, constantReader{value: 0x35})
	compared := 0
	for _, row := range providerDrivenMethodRowsFor(t, "the retention gate", class) {
		recorder := &argumentRecorder{}
		held, err := row.call(t, crypto, recorder.take)
		if err != nil {
			t.Errorf("%s: %v", row.name, err)
			continue
		}
		if len(recorder.arrays) == 0 {
			t.Errorf("%s was handed nothing through the recorder, so this row observed nothing", row.name)
			continue
		}
		if changed := recorder.changed(); len(changed) != 0 {
			t.Errorf("%s wrote into the storage behind arguments %v of the %d it was handed",
				row.name, changed, len(recorder.arrays))
		}
		if len(held) == 0 {
			t.Errorf("%s left nothing behind, so this row observed nothing", row.name)
			continue
		}
		for _, value := range held {
			if len(value.content) == 0 {
				t.Errorf("%s left nothing behind in %s, so that value observed nothing", row.name, value.name)
				continue
			}
			// the ARRAY the value stands for, which is the rendering's own storage unless the
			// row named a separate one. See providerDrivenMethodValue.storage: a rendering is
			// a fresh array, so a gate reading renderings answers "shares nothing" for every
			// possible implementation and holds nothing at all.
			observed := value.content
			if value.storage != nil {
				if len(value.storage) == 0 {
					t.Errorf("%s names storage for %s and it is empty, so nothing reads that value raw",
						row.name, value.name)
					continue
				}
				observed = value.storage
			}
			if recorder.aliases(observed) {
				t.Errorf("%s kept %s over the storage of one of the arrays it was handed; that array is its caller's and the state outlives the call",
					row.name, value.name)
			}
		}
		compared++
	}
	if compared != len(class) {
		t.Fatalf("%d of the %d methods handed a %s and a caller's array were run through the recorder",
			compared, len(class), providerInterfaceName)
	}
	// the size of the class, logged rather than asserted: what holds the class to the package is
	// the derivation, and what a number here is worth is a reader seeing the gate shrink.
	t.Logf("%d of the %d methods handed a %s can reach a caller's array: %v",
		len(class), len(providerDrivenMethodNames(t)), providerInterfaceName, class)
}

// TestEveryDeclarationTakingAProviderIsHeldByExactlyOneOfTheTwoClasses is what records the
// boundary the package level gates draw, so it cannot be widened or narrowed in silence.
//
// packageLevelFunctionsIn and providerConstructions each skip a declaration carrying a
// receiver. That skip is invisible from the gates reading them: they compare their tables
// against a class that never contained a method, find it matches, and report the clean run a
// complete gate reports. What says otherwise is this -- the whole of what the type checker
// reads as taking a provider, split by the same receiver test, with each half compared against
// the class the gates for that half actually run over.
//
// A declaration in neither half is one nothing holds. A declaration in both is a gate reading
// a class it was not built for. Either is a failure here rather than a coverage report that
// happens to be short.
func TestEveryDeclarationTakingAProviderIsHeldByExactlyOneOfTheTwoClasses(t *testing.T) {
	provider := providerInterfaceType(t)
	constructions := []string{}
	methods := []string{}
	for _, function := range declaredFunctionsOf(t, cryptoOwnRoot) {
		if len(function.takes(provider)) == 0 {
			continue
		}
		if function.method {
			methods = append(methods, function.name)
			continue
		}
		constructions = append(constructions, function.name)
	}
	if len(constructions) == 0 || len(methods) == 0 {
		t.Fatalf("the whole of what takes a %s reads as %d constructions and %d methods, so one of the two halves below is compared against nothing",
			providerInterfaceName, len(constructions), len(methods))
	}
	slices.Sort(constructions)
	slices.Sort(methods)
	if declared := packageLevelFunctionsTaking(t, providerInterfaceName); !slices.Equal(constructions, declared) {
		t.Errorf("the type checker reads %v as the constructions taking a %s and the package level scan the construction gates run over reads %v",
			constructions, providerInterfaceName, declared)
	}
	if held := providerDrivenMethodNames(t); !slices.Equal(methods, held) {
		t.Errorf("the type checker reads %v as the methods taking a %s and the gates in this file run over %v",
			methods, providerInterfaceName, held)
	}
	for _, name := range methods {
		if slices.Contains(constructions, name) {
			t.Errorf("%s reads as both a construction and a method, so one of the two gate families is running over a class it was not built for", name)
		}
	}
	// and the retention gate's narrower class is a subset of this one rather than a second
	// reading that could drift away from it
	for _, name := range providerDrivenMethodNamesTakingCallerBytes(t) {
		if !slices.Contains(methods, name) {
			t.Errorf("the retention gate runs over %s, which is not one of the methods taking a %s", name, providerInterfaceName)
		}
	}
	t.Logf("%d constructions and %d methods take a %s", len(constructions), len(methods), providerInterfaceName)
}

// TestTheByteSliceReadingAgreesWithTheSpellingBasedOne cross checks the two readings of
// "handed a caller's array" over the surface where both exist.
//
// The retention gate's class is read off the type checker; the construction gate's is read off
// the parse tree, matching []byte and the byte slice type names this package declares. Two
// readings of one property drift, and a reading that stopped matching shrinks the class it
// feeds while every gate over it goes on reporting a clean run. They share nothing but the
// package's source, so a filter that broke in either one is a disagreement here.
func TestTheByteSliceReadingAgreesWithTheSpellingBasedOne(t *testing.T) {
	typeChecked := []string{}
	for _, function := range declaredFunctionsOf(t, cryptoOwnRoot) {
		if function.method {
			continue
		}
		for i := 0; i < function.signature.Params().Len(); i++ {
			if isByteSliceType(function.signature.Params().At(i).Type()) {
				typeChecked = append(typeChecked, function.name)
				break
			}
		}
	}
	slices.Sort(typeChecked)
	if len(typeChecked) == 0 {
		t.Fatal("the type checked reading found no construction handed a caller's array, so this comparison holds for a filter that matches nothing")
	}
	if spelled := packageLevelFunctionsTakingCallerBytes(t); !slices.Equal(typeChecked, spelled) {
		t.Errorf("the type checked reading of the constructions handed a caller's array is %v and the spelling based one is %v",
			typeChecked, spelled)
	}
}
