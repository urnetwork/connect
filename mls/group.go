// RFC 9420 section 11 group creation, and the state one group at one epoch carries.
//
// THE GROUP IS THE ONLY STATEFUL EXPORTED TYPE IN THIS PACKAGE, and every other type here is a
// value the group assembles or is handed. It is NOT safe for concurrent use: connect/message
// serializes every call through a single goroutine command loop, and stateLock is here so that
// misuse fails loudly under -race rather than tearing a tree in half.
//
// WHY CREATION IS THE RFC'S FOUR STEPS AND NOT A SHORTCUT. Section 11 could be read as "choose a
// tree, choose an epoch secret, done", and the RFC says in as many words why it is not written
// that way: the long form "removes unnecessary choices, by which, for example, bad randomness
// could be introduced". Every field NewGroup writes is a field nothing else in this package has
// ever validated -- there is no peer, no signature and no vector on the other side of a group
// creation -- so each of the four steps is spelled out here in the order the RFC gives them, and
// each value a step draws is held by a test that can see the draw through what the group
// PUBLISHES rather than by naming the line that made it.
//
// WHAT THE CACHE BINDS TO IS A VERIFIED CONTEXT AND NOT THE ONE THIS FUNCTION JUST BUILT.
// NewProposalCache takes a *VerifiedGroupContext, whose only constructor is
// (*GroupInfo).VerifiedContext, and the creator is not exempt from that: it assembles the epoch 0
// GroupInfo, signs it with its own signature key, and verifies it against its own tree. That is
// nearly a tautology for the creator -- it is the only member, so the tree it checks against holds
// only its own leaf -- and it is written that way ON PURPOSE. The alternative is a second door
// onto a verified context, and group_context_verified.go spent four rounds establishing that the
// second door IS the defect; a door used only by the one caller entitled to it is still a door.
// TestEveryConstructionOfAVerifiedGroupContextIsClassifiedHere fails on the day this file spells
// the construction directly.
package mls

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/urnetwork/connect/mls/syntax"
)

// The refusals group creation makes that are nobody else's.
//
// One value per rule, for errors_lifecycle.go's reason: errors.Is cannot tell two rules apart when
// they answer the same value, so a test asserting the broad question passes over a rule that fired
// for the wrong reason.
//
// errGroupConfigProviderSuite is NOT errProfileCiphersuite, and the difference is the whole reason
// it exists. The profile refusal is "this build does not create groups under that suite", which is
// a decision this profile made and could revisit; this one is "the config names one suite and the
// provider handed in runs another", which is a caller that wired two objects together wrongly and
// has nothing to do with the profile. Collapsing them would hand a caller reading "outside the v1
// profile" a suite that is squarely inside it.
var (
	errNilGroupConfig = errors.New("mls: there is no group config")
	errNilStateStore  = errors.New("mls: the group config carries no state store")

	errGroupConfigProviderSuite = errors.New("mls: the group config's ciphersuite is not the provider's")

	errGroupClosed = errors.New("mls: the group is closed and its epoch secrets have been zeroized")

	// the persisted state's own refusals, one value per rule for this block's stated reason.
	// A blob written at a layout this build does not read is a state to migrate or discard; a
	// restore kind this build has no constructor for is a blob that has been edited, truncated
	// into a neighbouring field, or written by a build that had a third kind. errors.Is cannot
	// tell those apart when they answer one value, and the two want different responses.
	errGroupStateBlobVersion = errors.New(
		"mls: the persisted group state was written at a blob version this build does not read")
	errGroupStateRestoreKind = errors.New(
		"mls: the persisted group state names no restore kind this build can rebuild a key schedule from")
	errGroupStateTranscript = errors.New(
		"mls: the persisted group state's transcript does not agree with the key schedule it names")

	// and the refusal a group makes about ITSELF rather than about a blob. An epoch whose
	// schedule has been erased answers KDF.Nh zero bytes for the secret it was built from, and
	// that is the exact length both constructors accept -- so persisting one would write a
	// state that restores a group every party can recompute, with a nil error the whole way.
	errGroupStateRestoreSecret = errors.New(
		"mls: this epoch's key schedule cannot answer the secret it was built from")

	// errGroupStatePathSecret is a persisted state whose TreeKEM ladder does not derive the
	// public keys the tree in the same blob carries.
	//
	// A SEPARATE VALUE from errGroupStateTranscript, which is the block's rule and here it is
	// load bearing: the interim transcript hash is a commitment to the restore secret and to the
	// kind that says how to use it, and it says NOTHING about the path secrets -- they are not an
	// input to the key schedule at all. So the two refusals name two different halves of one blob,
	// and a caller told "the transcript disagrees" about a corrupted path secret would go looking
	// at the schedule.
	errGroupStatePathSecret = errors.New(
		"mls: the persisted group state's path secrets do not derive the keys the tree it names carries")

	// errGroupStateLadderOrder is a persisted ladder that is not in strictly increasing node order.
	//
	// It is a SEPARATE VALUE from the one above because it is a different fault with a different
	// cause, which is this block's rule: a secret that does not derive its node's key is a rung that
	// was corrupted, and an out-of-order vector is a vector this build did not write -- marshalState
	// sorts, and the keys of a map are unique, so strictly increasing is exactly the shape that
	// leaves this process.
	//
	// AND IT IS THE ONLY THING THAT CATCHES A REPEATED NODE, which is the reason it is a door rather
	// than a comment on the encoder. Two entries naming one node collapse into ONE map entry when
	// the ladder is rebuilt, so the restored member silently holds one fewer rung than it persisted
	// -- and if the surviving entry is the correct one, the whole state passes the derivation check
	// below and produces a member that is right about everything it holds and missing the rung it
	// needed. Measured: with only the derivation check, a state whose first entry had been refiled
	// under the last entry's node restored with a nil error.
	errGroupStateLadderOrder = errors.New(
		"mls: the persisted group state's path secrets are not in strictly increasing node order")

	// errGroupStateSenderRatchetOrder is a persisted sender ratchet vector that is not in strictly
	// increasing RatchetType order.
	//
	// A SEPARATE VALUE from the one above for that value's own reason -- a different fault with a
	// different cause is a different door -- and the cause here is the one an ordering rule over
	// the ladder has no equivalent of. Two ladder entries naming one node COLLAPSE into one map
	// entry; two ratchet entries naming one kind OVERWRITE, so the restored member stands wherever
	// the entry that happened to come last says. When that is the earlier of the two, the member
	// has been handed back a generation it has already sent under -- which is the key reuse the
	// sender ratchet field exists to prevent, reached from a corrupted store rather than from a
	// missing field.
	errGroupStateSenderRatchetOrder = errors.New(
		"mls: the persisted group state's sender ratchets are not in strictly increasing ratchet type order")

	// errGroupStateSenderRatchet is a persisted sender ratchet vector the restored epoch's secret
	// tree refused.
	//
	// It wraps whatever the secret tree said -- a secret of the wrong length, a leaf outside the
	// tree, a ratchet type this build does not have -- for errGroupStatePathSecret's reason one
	// field over: a caller needs to be able to ask "is this state's own sender position the thing
	// that is wrong" and get the true answer, rather than reading a secret tree range failure and
	// going to look at the group's size.
	errGroupStateSenderRatchet = errors.New(
		"mls: the persisted group state's sender ratchets do not install into the epoch it names")

	// errStagedCommitErased is a staged commit whose key material has been erased, handed to a
	// door that installs one.
	//
	// ONE VALUE FOR TWO DOORS because it is one rule: ApplyCommit and MergePendingCommit are the
	// two ways a staged epoch becomes live, and what the rule says at both is that an erased
	// staged commit installs nothing. MEASURED through the exported API alone --
	// processed.Commit.Zeroize() and then receiver.ApplyCommit(processed) -- the group advanced to
	// the next epoch and answered a 32-zero epoch authenticator, because the group id and the
	// prior epoch SURVIVE Zeroize and were the only two fields this door read. Two members that
	// both took that path compare equal -- measured, byte for byte -- which is a fork detector
	// answering "no fork" out of erased storage about two groups with nothing in common.
	//
	// AND THE CALL DOES NOT ANSWER nil, which is worth writing down because it is WORSE than nil
	// rather than better. With both refusals removed the merge runs to completion, installs the
	// erased epoch, and then fails at the persist -- marshalState refuses an epoch whose schedule
	// cannot answer the secret it was built from -- so the caller is handed
	// errGroupStateRestoreSecret, which names the store. The group has already moved by then, and a
	// caller reading that value learns that its state could not be written rather than that the
	// value it passed was erased.
	errStagedCommitErased = errors.New("mls: the staged commit's key material has been erased")

	// errStagedCommitInstalled is a staged commit whose epoch a merge has ALREADY INSTALLED --
	// the shell MergePendingCommit leaves behind after it moves the schedule, the secret tree,
	// the leaf private state and the plan into the group and detaches all four -- handed to a
	// door that installs one.
	//
	// THE SAME ONE RULE AS errStagedCommitErased, AT THE SAME TWO DOORS, and it exists because
	// the shell is the one other value of this type that walks the provenance pair: the group
	// id and the prior epoch are public facts that survive the detach exactly as they survive
	// the erase, the erased flag is NOT set on a shell -- it was installed, not erased -- and so
	// a shell handed to a SECOND live instance of the same group at the same epoch clears every
	// check the door made and reaches the merge. MEASURED, 2026-09-21, through the exported API
	// alone: two views of one member at one epoch, the first applies a Processed and the second
	// is handed the same value; the merge erased the second view's live schedule, assigned the
	// shell's nil one in its place, and the persist dereferenced it -- a panic inside
	// ApplyCommit, and an instance that then panicked on every Protect after it. Reachable only
	// through raw mls: messagegroup's seam refuses a value staged by another handle by name
	// before mls is asked anything ((*connectMlsHandle).stagedBy), and releases the staged half
	// on its own install, so no EngineProcessed reaches this door as a shell.
	//
	// It is a value of its own and not errStagedCommitErased because the two are two facts a
	// caller acts on differently: an erased value is one the caller discarded and owes nothing
	// further, and an installed one is one the caller already applied -- at THIS instance or,
	// as measured, at another view of the same member -- and is holding past its life.
	errStagedCommitInstalled = errors.New("mls: the staged commit's epoch has already been installed and the value is a shell")

	errCreationConfirmationTag = errors.New("mls: the epoch 0 confirmation tag is not a tag of this suite's width")

	// and the same refusal one epoch on. A SECOND VALUE and not errCreationConfirmationTag,
	// for this block's stated reason: the epoch 0 condition is a group that could not be
	// founded and this one is a commit that could not be built, and a caller reading one
	// value cannot tell which of its calls is the one that failed.
	errCommitConfirmationTag = errors.New("mls: the commit's confirmation tag is not a tag of this suite's width")

	// the Welcome half of a commit, and two values because they are two faults a caller
	// repairs differently.
	//
	// errWelcomeAddPairing is the ONE ASSUMPTION (*StagedCommit).welcomeMessage rests on, written as a
	// refusal rather than left as an index: StagedCommit.added and (*ProposalList).Adds are two
	// readings of the same Add proposals -- ApplyProposals walks the commit order and appends a
	// leaf per Add, and Adds is that same order filtered -- so entry i of one is entry i of the
	// other. A build where they part company would seal each joiner's group secrets to some
	// OTHER joiner's init key, silently, with every length equal and every seal well formed.
	// The proposal list was rebuilt around one representation precisely because a COUNT is what
	// a dual representation gets held together by and a count is what let four forks through, so
	// this is stated where the pairing is used rather than assumed at a distance.
	errWelcomeAddPairing = errors.New(
		"mls: the leaves this commit added and the Add proposals it names are not the same list")

	// errWelcomePathSecret is a commit that carried an update path and cannot say where one of
	// its joiners picks that path up.
	//
	// A refusal and not a nil path secret, which is the whole reason it exists. GroupSecrets
	// carries an optional<PathSecret>, so "absent" is a MEANING a joiner acts on -- seed your
	// direct path from the tree as it stands -- and a commit that reset the nodes above the
	// joiner while telling it nothing was reset produces a member that derives the wrong key for
	// every node it shares with the committer, at the far end, with nothing here to point at.
	errWelcomePathSecret = errors.New(
		"mls: this commit carried an update path and holds no path secret for a joiner")
	// and the RECEIVING half, task 16. One value per rule for this block's stated reason; what
	// each of them means is written on JoinFromWelcome beside the step that answers it.
	//
	// errNilJoinKeyMaterial is a join with nothing to join AS. It is separate from
	// errNilGroupConfig because the two are different missing objects with different repairs, and
	// a caller told "there is no group config" while holding one would go looking at the wrong
	// argument.
	errNilJoinKeyMaterial = errors.New("mls: there is no key material for this joiner to join with")

	// errWelcomeWireFormat is a message handed to the joiner that is not a Welcome. It is not
	// ErrWelcomeSuiteMismatch or any other welcome refusal: nothing about a Welcome has been
	// judged, because the thing in hand is not one.
	errWelcomeWireFormat = errors.New("mls: the message handed to this joiner is not a welcome")

	// errWelcomeJoinerProviderSuite is a Welcome naming one ciphersuite in the clear while the
	// provider this joiner runs is another. It is NOT ErrWelcomeSuiteMismatch, which is the
	// welcome against the joiner's own key package, and it is not welcome.go's
	// errWelcomeSuiteProvider, which is the same disagreement at the BUILDING end -- three pairs
	// of values, and a caller sent to look at the wrong pair looks at two values that agree.
	errWelcomeJoinerProviderSuite = errors.New(
		"mls: the welcome names a ciphersuite the provider joining under it does not run")

	// errWelcomeGroupSecretsDecrypt is the entry addressed to this joiner's key package failing to
	// open under this joiner's init key.
	//
	// A SEPARATE VALUE FROM ErrWelcomeNoMatchingKeyPackage, and the split is the pairing. That one
	// is "no entry here is addressed to me", which is an ordinary condition -- a Welcome for a
	// commit that added somebody else. This one is "the entry that NAMES me was not sealed to me",
	// which is a Welcome whose reference half and whose seal half disagree: an entry lifted off
	// another Welcome, a seal made to another joiner's init key, or a builder that paired joiner
	// i's reference with joiner j's ciphertext. Collapsing the two would let a test asserting the
	// ordinary condition pass over the forgery.
	errWelcomeGroupSecretsDecrypt = errors.New(
		"mls: the group secrets addressed to this joiner did not open under its init key")

	// errWelcomeGroupIdNotTheOneAsked is a Welcome describing a group other than the one the
	// caller's config named. See JoinFromWelcome: it is an intent match and not authentication,
	// and it is its own value so that a caller can tell "this is not the group I asked for" from
	// every cryptographic refusal beside it.
	errWelcomeGroupIdNotTheOneAsked = errors.New(
		"mls: the welcome describes a group other than the one this client asked to join")

	// errWelcomeLeafNotTheJoiners is a tree carrying this joiner's signature key at a leaf that is
	// not the leaf its key package published.
	//
	// NOT ErrWelcomeLeafNotFound, which is "no leaf of this tree is mine". This one is "a leaf of
	// this tree claims to be mine and is not", which is the substitution the whole check exists
	// for, and a single value would let a test asserting the absence pass over it.
	errWelcomeLeafNotTheJoiners = errors.New(
		"mls: the tree's leaf for this joiner is not the leaf its key package published")

	// errJoinerSignatureKeyNotTheLeafs is a joiner whose signing key is not the signature key its
	// own key package published.
	//
	// The two halves arrive at this door independently -- the key package out of whatever the
	// caller stored, the private half out of the device's own keyring -- and this is the one door
	// of the three that produces a leaf for this client where they are not one value: NewLeafNode
	// and (*Group).ProposeUpdate both DERIVE the published key from the private one through
	// signaturePublicKeyOf, so a mismatch there cannot be written. A joiner that installed the
	// mismatched pair signs every message it sends with a key its own published leaf does not
	// name; every other member refuses those messages at the signature, and neither end holds
	// anything that says why.
	// errJoinerEncryptionKeyNotTheLeafs is a joiner whose leaf private key is not the private half
	// of the encryption key its own key package published.
	//
	// The twin of the value below and refused at the same door, for the same reason: the two halves
	// arrive there independently, the key package out of whatever the caller stored and the private
	// half out of the device's own keyring. Nothing further down JoinFromWelcome ever uses this
	// private half against a public one the Welcome carries -- (*TreeKEMPrivate).Consistent judges
	// the PATH SECRETS and says so -- so a joiner that installed a mismatched pair joins cleanly,
	// agrees on the epoch authenticator and carries application traffic for the rest of the epoch,
	// and then decrypts NOTHING from the first commit that seals an update path to its leaf: every
	// ciphertext addressed to the key its leaf publishes, opened with a key that is not its private
	// half, reported at the far end as a corrupt commit. It is errUpdatedLeafPrivateKey's condition
	// arriving one epoch early, at a door that had nothing to refuse it with.
	errJoinerEncryptionKeyNotTheLeafs = errors.New(
		"mls: this joiner's leaf private key is not the private half of the encryption key its key package published")

	errJoinerSignatureKeyNotTheLeafs = errors.New(
		"mls: this joiner's signing key is not the signature key its key package published")

	// errJoinerWelcomeSecret is this build disagreeing with itself about welcome_secret: the value
	// JoinFromWelcome derived to open the encrypted group info is not the one the key schedule
	// derives from the same joiner secret. Its whole account is at the comparison.
	errJoinerWelcomeSecret = errors.New(
		"mls: the welcome secret this joiner opened the group info with is not the one its key schedule derives")

	// errWelcomePathSecretNode is a Welcome handing this joiner a path secret for an epoch whose
	// update path never covered it: the lowest node the joiner and the sender share is not on the
	// sender's filtered direct path. It is the receiving half of errWelcomePathSecret above and a
	// separate value for that block's reason -- one is a commit that could not be built and this
	// is a message that cannot be believed.
	errWelcomePathSecretNode = errors.New(
		"mls: the welcome's path secret is for a node the sender's update path did not cover")
)

// StateStore persists group state and private key material across process restarts. It is
// deliberately dumb -- no queries, no cross-group transactions -- so that sdk can implement it over
// the sealed local store without leaking storage semantics into the crypto. Spec A section 3.5.
//
// TWO THINGS AN IMPLEMENTATION OWES ITSELF, both about arrays rather than about storage.
//
// It must not RETAIN a slice it was handed. Every octet that arrives here is the caller's, and a
// store that keeps one -- to key a map, to build a path with later, to write asynchronously --
// shares an array with the group for the group's lifetime; a write through it rewrites a group id
// every epoch secret of that group was derived over, with the context, the tree hash and the
// transcript all going on agreeing with each other over the wrong id. Nothing downstream would
// report that. TestNoOctetAGroupHandsOutwardIsStorageItKeeps is what holds this package's side of
// it, and the retention half is the implementation's.
//
// And the state PutGroupState is handed is ERASED as soon as the call returns. Those octets are a
// plaintext epoch secret and a leaf private key, written once per merged commit, and the group is
// the only holder left that could clear them -- so it does, rather than leaving one copy per epoch
// in the heap. An implementation that wants the bytes past the call owes itself a copy. Nothing is
// erased on the way back OUT: what GetGroupState answers is the STORE's array, and a group that
// wiped it would destroy the state it had just read.
type StateStore interface {
	PutGroupState(groupId []byte, epoch uint64, state []byte) error
	GetGroupState(groupId []byte, epoch uint64) ([]byte, error)
	DeleteGroupStateBefore(groupId []byte, epoch uint64) error

	PutPrivateKey(pub []byte, priv []byte) error
	GetPrivateKey(pub []byte) ([]byte, error)
	DeletePrivateKey(pub []byte) error

	PutKeyPackage(ref []byte, kp []byte, initPriv []byte, encPriv []byte) error
	TakeKeyPackage(ref []byte) (kp, initPriv, encPriv []byte, err error)
}

// GroupConfig is everything a group needs that is not epoch state.
//
// Profile is a *profile and not a *Profile because p8 owns that type; see proposal_list.go's
// header for why the stand-in is unexported and what the swap costs. A caller outside this package
// leaves the field nil and gets defaultProfile(), which is the only profile this build has.
type GroupConfig struct {
	Suite        CipherSuite
	GroupId      []byte
	Extensions   []Extension
	RequiredCaps RequiredCapabilities
	Crypto       CryptoProvider
	Store        StateStore
	Profile      *profile
	LeafKeys     LeafKeysExtension
}

// Member is one member of the group as the product sees it.
type Member struct {
	LeafIndex    LeafIndex
	IdentityPub  []byte
	SignatureKey SignaturePublicKey
	LeafKeys     *LeafKeysExtension
	Role         Role
}

// EpochSecretName is a CLOSED enum. MASTER section 8.2 needs exactly these two for archive_secret,
// and exporting epoch_secret instead would also expose confirmation_key and membership_key. An
// open string accessor would invite exactly that mistake, so there is no such accessor.
type EpochSecretName uint8

const (
	EpochSecretSenderData EpochSecretName = iota + 1
	EpochSecretEncryption
)

// restoreKind discriminates what the persisted state carries so that task 19's LoadGroup can
// rebuild the key schedule. Epoch 0 has no joiner secret and every later epoch does; there is no
// third case.
type restoreKind uint8

const (
	restoreFromEpochSecret restoreKind = 1
	restoreFromJoiner      restoreKind = 2
)

// createSuiteProfile is the v1 disposition of every REGISTERED ciphersuite AS A SUITE TO CREATE A
// GROUP UNDER: nil for the one spec A section 3.1 admits, and the sentinel naming why for the one
// it does not.
//
// A map keyed by the registry's own constants rather than a comparison against one name, for
// groupExtensionProfile's reason next door: the accepted set is the complement of a refused set,
// and only one of the two can be written down without understating the other. What holds it to the
// registry is TestTheV1ProfileClassifiesEveryRegisteredCiphersuiteForCreation, which reads Suites()
// -- the registry itself -- and compares the two sets in both directions.
//
// 0x0001 is REGISTERED AND IMPLEMENTED and still refused here, and that is the whole content of
// this table. A profile that refused only an unimplemented suite would be saying nothing.
var createSuiteProfile = map[CipherSuite]error{
	CipherSuiteX25519AesGcm128Sha256Ed25519: errProfileCiphersuite,
	CipherSuiteX25519ChaCha20Sha256Ed25519:  nil,
}

// checkCiphersuiteForCreate is the one refusal surface for the suite a group is created under,
// standing in for (*Profile).CheckCiphersuiteForCreate.
//
// It is a method on proposal_list.go's profile rather than a free function, for
// checkGroupExtension's reason: the narrowing is the PROFILE's and not this file's, and p8's
// Profile carries all seven Check* gates beside each other.
//
// The two refusals are separated for checkProposalType's reason. A code point this build has never
// heard of is ErrUnknownCipherSuite -- there is no narrowing to name -- and a REGISTERED one this
// profile will not create under is the profile sentinel, which is a decision that could be
// revisited.
func (self *profile) checkCiphersuiteForCreate(suite CipherSuite) error {
	refusal, registered := createSuiteProfile[suite]
	if !registered {
		return fmt.Errorf("%w: ciphersuite %#04x is not in this build's suite registry",
			ErrUnknownCipherSuite, uint16(suite))
	}
	if refusal != nil {
		return fmt.Errorf("%w: ciphersuite %#04x may not be used to create a group",
			refusal, uint16(suite))
	}
	return nil
}

// Group is one MLS group at one epoch.
//
// THE KEY SCHEDULE FIELD IS SPELLED schedule. p6 task 20's two construction-bypass seams were
// written against self.keySchedule and p7's plan declares self.schedule; p7 owns this struct, so
// schedule is what it is, and task 20 compiles against this name.
type Group struct {
	stateLock sync.Mutex

	// the STORE and not the *GroupConfig it came off. A config is a caller's own structure and
	// the caller goes on holding it -- to found a second group, or to write a fresh group id into
	// for the next one -- so a group that kept the pointer would be reading a value somebody else
	// is still editing, and every octet reachable from that config would be storage this group
	// and its caller share. Nothing here ever needed the rest of it: the suite, the extensions and
	// the leaf keys are read once during creation and the store is the only field that outlives
	// the call. TestNoConstructionOfSealedStorageRetainsItsCallersArrays is what holds this to
	// one field.
	store  StateStore
	crypto CryptoProvider
	signer SignaturePrivateKey
	cred   Credential

	ownLeaf LeafIndex
	ownPriv *TreeKEMPrivate
	tree    *RatchetTree
	context *GroupContext
	// the same context with its authority established, which is the only thing the proposal
	// cache binds an epoch to. It is held BESIDE the context rather than in place of it because
	// the two answer different questions -- every read of this epoch wants the fields, and only
	// the cache boundary wants the authority -- and because VerifiedGroupContext hands out a
	// Clone rather than its own pointer, so a group that kept only this one would rebuild the
	// context it publishes on every read.
	//
	// WHAT KEEPS THE TWO FROM PARTING COMPANY is that one statement list writes both: NewGroup
	// builds them from the same GroupInfo and MergePendingCommit installs both off the same
	// staged commit, which is what the epoch mover gate reads.
	verified   *VerifiedGroupContext
	schedule   *KeySchedule
	secretTree *SecretTree
	transcript *TranscriptHashes
	proposals  *ProposalCache

	// which constructor this epoch's schedule came from, which is what task 19's LoadGroup needs
	// in order to know what to rebuild it from. THE SECRET ITSELF IS NOT HERE, and that is the
	// decision rather than an omission: the schedule above already retains the epoch secret, its
	// erase is held by TestZeroizeErasesEveryByteSliceThisTypeDeclares over the type that
	// DECLARES that storage, and a second copy parked on this struct would be the same secret
	// held by nothing but a hand written Close. Task 19 rebuilds from self.schedule.
	restoreKind restoreKind

	// the commit this client has built and not yet merged, or the one it has staged out of a
	// peer's commit. AT MOST ONE, because the delivery service accepts at most one commit per
	// (group, epoch): a client holding two candidate epochs has nothing to say which of them
	// the service accepted, and (*Group).Commit refuses a second rather than replacing the
	// first. ClearPendingCommit is how a caller drops one.
	pending *StagedCommit

	closed bool
}

// NewGroup creates a one-member group by RFC 9420 section 11's four steps.
//
// The steps are numbered in the body against the RFC's own list. What is worth reading them for is
// the ORDER: the tree exists before the group context, because the context carries the tree hash;
// the context exists before the key schedule, because every secret is expanded over the context;
// and the confirmation tag exists before the interim transcript hash, because the interim hash is
// taken over it. Each of those is a dependency and not a preference, and an implementation that
// reordered any of them would produce a group that is self consistent and agrees with nobody.
func NewGroup(cfg *GroupConfig, signer SignaturePrivateKey, cred Credential) (*Group, error) {
	if cfg == nil {
		return nil, errNilGroupConfig
	}
	crypto := cfg.Crypto
	// refused rather than dereferenced, and refused first: every value below is drawn, derived or
	// signed through the provider, so there is nothing this function could judge without one
	if crypto == nil {
		return nil, fmt.Errorf("%w: every secret this group is founded on is drawn through it",
			ErrNilCryptoProvider)
	}
	if cfg.Store == nil {
		return nil, errNilStateStore
	}
	active := cfg.Profile
	if active == nil {
		active = defaultProfile()
	}
	if err := active.checkCiphersuiteForCreate(cfg.Suite); err != nil {
		return nil, err
	}
	// the suite the config names must be the suite the provider runs. Without this the
	// disagreement surfaces four steps later inside (*GroupInfo).Verify, as a tree hash taken with
	// one suite's hash function checked against another's -- a refusal naming the tree, over a
	// tree that is perfectly good.
	if cfg.Suite != crypto.Suite() {
		return nil, fmt.Errorf("%w: the config names %#04x and the provider runs %#04x",
			errGroupConfigProviderSuite, uint16(cfg.Suite), uint16(crypto.Suite()))
	}
	for _, ext := range cfg.Extensions {
		if err := active.checkGroupExtension(ext.ExtensionType); err != nil {
			return nil, err
		}
	}
	// a group this profile creates carries a policy. GroupPolicyOf answers ErrNoGroupPolicy for a
	// context that carries none, which is a different fault from a malformed one, and it is
	// answered here rather than at the first message that needed a role.
	if _, err := GroupPolicyOf(cfg.Extensions); err != nil {
		return nil, err
	}
	leafKeysExt, err := cfg.LeafKeys.Encode()
	if err != nil {
		return nil, err
	}

	// step 1: a one-member tree, epoch 0, and the zero-length confirmed transcript hash.
	//
	// The founding leaf's HPKE key pair is DRAWN HERE and is one of the two values this whole
	// function is trusted with. A constant in place of this draw still encrypts, still decrypts,
	// still round trips, and matches every vector that does not depend on the randomness -- which
	// is why what holds it is a test that reads the draw back out of what the group publishes
	// rather than one that asserts over this line.
	encPriv, encPub, err := crypto.DeriveKeyPair(crypto.Random(crypto.HashSize()))
	if err != nil {
		return nil, err
	}
	leaf, err := NewLeafNode(crypto, signer, cred, encPub, v1Capabilities(), []Extension{leafKeysExt})
	if err != nil {
		return nil, err
	}
	tree := NewRatchetTree()
	ownLeaf, err := tree.AddLeaf(leaf)
	if err != nil {
		return nil, err
	}
	treeHash, err := tree.TreeHash(crypto)
	if err != nil {
		return nil, err
	}
	// the extension BODIES are copied and not the entries alone. append copies the Extension
	// STRUCTS and leaves every ExtensionData pointing at the caller's octets, so a caller that
	// went on writing into the buffer its policy was encoded out of would be rewriting the
	// context this group PUBLISHES -- while the key schedule stays derived over the octets as
	// they were, because the epoch secrets were expanded over the context at creation. That is a
	// group whose published context and whose epoch secrets have parted company, with every
	// signature still verifying at the moment it was made and nothing in between to point at.
	// Nil is kept distinct from empty for cloneBytes's reason, and GroupContext.Clone copies the
	// bodies the same way for the same reason one epoch later.
	var extensions []Extension
	if cfg.Extensions != nil {
		extensions = make([]Extension, 0, len(cfg.Extensions))
		for _, extension := range cfg.Extensions {
			extensions = append(extensions, Extension{
				ExtensionType: extension.ExtensionType,
				ExtensionData: cloneBytes(extension.ExtensionData),
			})
		}
	}
	if len(cfg.RequiredCaps.ExtensionTypes) != 0 || len(cfg.RequiredCaps.ProposalTypes) != 0 ||
		len(cfg.RequiredCaps.CredentialTypes) != 0 {
		requiredExt, requiredErr := encodeRequiredCapabilities(&cfg.RequiredCaps)
		if requiredErr != nil {
			return nil, requiredErr
		}
		extensions = append([]Extension{requiredExt}, extensions...)
	}
	// AND THE ASSEMBLED LIST IS READ BACK THROUGH THE LOOKUP EVERY LATER DOOR READS IT THROUGH.
	//
	// This is not a second opinion about the caller's extensions -- checkGroupExtension has already
	// judged each of them, and GroupPolicyOf has already refused a repeated policy. It is about the
	// entry THIS FUNCTION ADDS. The prepend above does not ask whether the caller's own list already
	// carries a required_capabilities, so a config that both sets RequiredCaps and supplies the
	// encoded extension founds a group whose context carries the type twice -- and every door that
	// reads it afterwards reaches it through FindExtension, which refuses a repeat. Measured: such a
	// group is created, persisted and signed, and then ValidateProposalList over its OWN published
	// context answers "the extensions vector carries one type at entry 0 and again at entry 2" for
	// every proposal list anyone ever builds against it. A group nobody can validate a proposal in,
	// founded successfully, with nothing at the point of the mistake to point at.
	//
	// The same repair as (*Group).ProposeGroupContextExtensions makes at the other end of the
	// group's life, and for the same reason: a list this build EMITS must be one its own readers
	// accept. TestNewGroupFoundsAContextItsOwnProposalDoorsAccept is what says so, by running the
	// door rather than by counting entries.
	if _, err := requiredCapabilitiesOf(extensions); err != nil {
		return nil, err
	}
	context := &GroupContext{
		Version:     ProtocolVersionMls10,
		CipherSuite: cfg.Suite,
		GroupId:     cloneBytes(cfg.GroupId),
		Epoch:       0,
		TreeHash:    treeHash,
		// section 11: "Confirmed transcript hash: The zero-length octet string". NOT KDF.Nh zero
		// octets and not the hash of nothing -- the three are different values, all of them well
		// formed, and the first commit's confirmed hash is chained from this one, so a wrong
		// value here is invisible until a second member has to agree with it.
		ConfirmedTranscriptHash: []byte{},
		Extensions:              extensions,
	}

	// step 2: a fresh random epoch secret of size KDF.Nh, and the key schedule under it.
	//
	// The second of the two values this function is trusted with, and the one a substitution is
	// most invisible in: every derived secret goes on being a well formed secret of the right
	// length. It is not retained HERE -- NewKeyScheduleFromEpochSecret keeps its own copy, which
	// is what task 19 rebuilds from -- so this local is the only other reference and it dies with
	// the call. Guardrail 6: no exported symbol of this package answers it.
	epochSecret := crypto.Random(crypto.HashSize())
	schedule, err := NewKeyScheduleFromEpochSecret(crypto, epochSecret, context)
	if err != nil {
		return nil, err
	}

	// steps 3 and 4: the confirmation tag over the empty confirmed transcript hash, and then the
	// interim transcript hash from it.
	//
	// SetFromGroupInfo is the JOINER's half of this and cannot be used here: it refuses a
	// confirmed transcript hash that is not KDF.Nh octets, which is right for a value that came
	// off somebody else's wire and is wrong for the one value section 11 defines as empty.
	transcript := InitialTranscriptHashes()
	confirmationTag := schedule.ConfirmationTag(transcript.Confirmed)
	// ConfirmationTag answers nil for an epoch whose confirmation_key has been erased. Nothing can
	// have erased this one -- the schedule was built four statements ago -- so this is a refusal
	// over a build that has stopped agreeing with itself, and the alternative to making it is
	// folding a nil tag into the interim hash and founding a group nobody can join.
	if len(confirmationTag) != crypto.HashSize() {
		return nil, fmt.Errorf("%w: the schedule answered %d octets, want %d",
			errCreationConfirmationTag, len(confirmationTag), crypto.HashSize())
	}
	interim, err := InterimTranscriptHash(crypto, transcript.Confirmed, confirmationTag)
	if err != nil {
		return nil, err
	}
	transcript.Interim = interim

	secretTree, err := NewSecretTree(crypto, tree.LeafWidth(), schedule.Secrets().Encryption)
	if err != nil {
		return nil, err
	}

	// the proposal cache binds to a context this client has VERIFIED, and the creator earns that
	// the same way every other member does: it publishes the epoch 0 GroupInfo, signs it, and
	// checks the signature against the tree. See this file's header for why the creator gets no
	// shortcut past the one door.
	groupInfo := &GroupInfo{
		GroupContext:    *context,
		ConfirmationTag: confirmationTag,
		Signer:          ownLeaf,
	}
	if err := groupInfo.Sign(crypto, signer); err != nil {
		return nil, err
	}
	verified, err := groupInfo.VerifiedContext(crypto, tree)
	if err != nil {
		return nil, err
	}
	proposals, err := NewProposalCache(verified)
	if err != nil {
		return nil, err
	}

	group := &Group{
		store:  cfg.Store,
		crypto: crypto,
		// the signing key and the identity are COPIED, for NewLeafNode's reason one layer up:
		// both are arrays the caller owns and goes on using, and this group signs with the key
		// and names itself with the identity for the whole of its life. A group holding a view
		// over either is a group whose signatures start failing at the moment its caller reuses
		// its own buffer, one epoch after the mistake.
		signer:      SignaturePrivateKey(cloneBytes(signer)),
		cred:        Credential{CredentialType: cred.CredentialType, Identity: cloneBytes(cred.Identity)},
		ownLeaf:     ownLeaf,
		ownPriv:     NewTreeKEMPrivate(ownLeaf, encPriv),
		tree:        tree,
		context:     context,
		verified:    verified,
		schedule:    schedule,
		secretTree:  secretTree,
		transcript:  transcript,
		proposals:   proposals,
		restoreKind: restoreFromEpochSecret,
	}
	if err := group.persist(); err != nil {
		return nil, err
	}
	return group, nil
}

// v1Capabilities is what every v1 leaf advertises: one version, every registered suite, no optional
// proposal types, the three URmessage extensions, and basic credentials only.
//
// Suites() rather than the group's own code point, for the reason lifecycle_fixtures_test.go's
// testCapabilities gives: the vector says what this member CAN do and not what this group runs, and
// a member advertising only its own group's suite is a member no other suite could ever add.
func v1Capabilities() Capabilities {
	return Capabilities{
		Versions:     []ProtocolVersion{ProtocolVersionMls10},
		CipherSuites: Suites(),
		Extensions: []ExtensionType{
			ExtensionTypeUrmessageGroupPolicy,
			ExtensionTypeUrmessageLeafKeys,
			ExtensionTypeUrmessageOwnerSuccessor,
		},
		Proposals:   []ProposalType{},
		Credentials: []CredentialType{CredentialTypeBasic},
	}
}

// encodeRequiredCapabilities serializes the section 11.1 required_capabilities extension.
func encodeRequiredCapabilities(required *RequiredCapabilities) (Extension, error) {
	data, err := syntax.Marshal(required)
	if err != nil {
		return Extension{}, err
	}
	return Extension{ExtensionType: ExtensionTypeRequiredCapabilities, ExtensionData: data}, nil
}

// GroupId returns the group's identifier, as storage the caller owns.
func (self *Group) GroupId() []byte {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return cloneBytes(self.context.GroupId)
}

// Epoch returns the current epoch.
func (self *Group) Epoch() uint64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.context.Epoch
}

// OwnLeafIndex returns this device's leaf.
func (self *Group) OwnLeafIndex() LeafIndex {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.ownLeaf
}

// OwnLeafNodeCopy returns a deep copy of this device's leaf. It is a Clone and never the live leaf:
// the validation plan's forge mutates what it is handed, and handing it the tree's own leaf would
// corrupt the tree this epoch's tree hash was taken over, from a test.
func (self *Group) OwnLeafNodeCopy() *LeafNode {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	leaf := self.tree.Leaf(self.ownLeaf)
	if leaf == nil {
		return nil
	}
	return leaf.Clone()
}

// Members returns a snapshot of the membership with roles resolved from the group policy extension.
func (self *Group) Members() []Member {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.membersLocked()
}

// membersLocked is Members with the state lock already held.
//
// A member whose identity the policy does not name is RoleMember and not a refusal. The policy
// names the members that hold a role other than the default, so a tree position with no row is a
// position with the default role rather than a group that has stopped parsing.
func (self *Group) membersLocked() []Member {
	policy, policyErr := GroupPolicyOf(self.context.Extensions)
	out := []Member{}
	for _, leafIndex := range self.tree.NonBlankLeaves() {
		leaf := self.tree.Leaf(leafIndex)
		if leaf == nil {
			continue
		}
		// EVERY array of this snapshot is copied, and the two beside each other are why that is
		// said rather than left to be read off the lines: an identity that is cloned and a
		// signature key that is not are one struct three lines apart, and the second one is a
		// window onto the LIVE ratchet tree. A caller handed that window writes through it into
		// the tree this epoch's tree hash was taken over -- the same hazard OwnLeafNodeCopy
		// answers a Clone for -- and nothing in this package would report it, because the tree
		// goes on being self consistent with the octets it now holds.
		member := Member{
			LeafIndex:    leafIndex,
			IdentityPub:  cloneBytes(leaf.Credential.Identity),
			SignatureKey: SignaturePublicKey(cloneBytes(leaf.SignatureKey)),
			Role:         RoleMember,
		}
		if keys, keysErr := LeafKeysOf(leaf); keysErr == nil {
			member.LeafKeys = keys
		}
		if policyErr == nil {
			if role, named := policy.RoleOf(leaf.Credential.Identity); named {
				member.Role = role
			}
		}
		out = append(out, member)
	}
	return out
}

// MemberAt returns one member.
func (self *Group) MemberAt(leafIndex LeafIndex) (Member, bool) {
	for _, member := range self.Members() {
		if member.LeafIndex == leafIndex {
			return member, true
		}
	}
	return Member{}, false
}

// EpochAuthenticator is the value two members compare to detect a fork, or nil for a closed group.
//
// Nil rather than a panic, and nil rather than the zeros a zeroized schedule holds: a closed
// group's authenticator is not a shorter authenticator, it is no answer at all, and KDF.Nh zero
// octets is a value any two closed groups would agree on.
func (self *Group) EpochAuthenticator() []byte {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		return nil
	}
	return cloneBytes(self.schedule.Secrets().EpochAuthenticator)
}

// Export is the RFC 9420 section 8.5 exporter. MASTER section 7 derives mls_secret from it.
func (self *Group) Export(label string, context []byte, length int) ([]byte, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		return nil, errGroupClosed
	}
	return self.schedule.Export(label, context, length)
}

// PairwiseExport is the exporter for key material TWO NAMED MEMBERS HOLD AND NO THIRD MEMBER CAN
// DERIVE. Export above is the GROUP scoped one and this is its pairwise sibling; the two are not
// interchangeable and the difference is the whole reason this method exists.
//
// WHAT SEPARATES IT FROM Export, said first because it is the only thing a caller has to decide
// between them on. Export expands exporter_secret, which is one of the nine secrets of the epoch
// schedule -- so EVERY member of the group derives the same answer from it, and a tag keyed by it
// is forgeable by any of them. This method's key is an x25519 static-static Diffie-Hellman between
// TWO LEAF HPKE KEYPAIRS: the RFC 9420 section 7.2 leaf encryption key, whose private half this
// member holds on self.ownPriv and whose public half is in the ratchet tree every member holds. A
// third member c holds the whole key schedule, every Export of it, the whole tree and every public
// leaf point, and cannot derive this: the key lives OUTSIDE the key schedule, on two scalars c
// does not have. TestAThirdMemberWithTheWholeScheduleAndTreeDerivesSomethingDifferent is that
// sentence as a test, and it is the one property that could not be had from Export at any label.
//
// THE DH HAPPENS HERE AND NO LEAF SCALAR LEAVES THIS PACKAGE. That is not tidiness: self.ownPriv
// is the key MergePendingCommit zeroizes, and a seam that handed the scalar out would have put a
// copy of it in a package with no erase discipline. It is also forced -- ExpandWithLabel is a
// METHOD on CryptoProvider and nothing outside this package holds one.
//
// THE CONTEXT, 160 octets at URmessage's 32 octet group id because every field is fixed width or
// LP prefixed:
//
//	LP(group_id) || u64(epoch) || u32(lo) || u32(hi) || LP(pk_lo) || LP(pk_hi) || LP(epoch_authenticator)
//
// LP is the RECORD LAYER's fixed 32 bit big endian length prefix and NOT the MLS varint, and the
// whole account of that -- why it is right here, why it is reached through mls/syntax rather than
// through connect/message, and which gate sanctions it -- is on group_pairwise.go, which holds the
// encoder. It is a DECLARATION rather than a writer inline in this body, for a reason that gate's
// neighbour records: the labelled composition walk reads an inline self.schedule read here as a
// serialized structure arriving at a labelled field with nothing bounding it, exactly as it does
// for senderDataSecretLocked one screen down.
//
// WHY EACH TERM IS THERE, because a context term nothing can distinguish is a term that defends
// nothing and each of these is answerable by a test that goes red without it:
//
//   - (lo, hi) is the pair SORTED, and BOTH public points accompany it. That is what makes the
//     answer SYMMETRIC -- the two members derive byte-identical material without exchanging
//     anything -- and it is what kills unknown-key-share: a point cannot be moved between the two
//     positions without moving the index that names it.
//   - epoch_authenticator binds the key to THAT EPOCH'S SCHEDULE. This is what puts a pairwise key
//     under the SAME erase discipline the group scoped one is under: reconstructing it at a past
//     epoch needs that epoch's schedule, which MergePendingCommit zeroizes and which
//     DeleteGroupStateBefore(epoch - PastEpochWindow) prunes. Without it the answer would survive
//     as long as the two scalars do, which is longer than any epoch secret does.
//   - group_id and epoch make the same device pair in two groups, or in two epochs of one group,
//     two different keys.
//
// WHAT IT REFUSES. A closed group, as Export does. ErrPairwiseSelf for one's own leaf, because a
// DH with one's own point is not a two party key and answering one would hand a caller a value
// that looks exactly like a real one. ErrBlankLeaf for a position holding no member -- a blank
// node is not a member, and the own-leaf read is checked too so that the two public points are
// both a member's. X25519DH's own ErrInvalidPoint for a low order peer point.
//
// IT IS NOT A VERIFIER AND IT DOES NOT KNOW WHICH EPOCH A CALLER MEANT. Like Export, it answers
// the CURRENT epoch with a nil error, and MergePendingCommit REPLACES the schedule rather than
// leaving it erased -- so a caller recomputing a tag over a record from an older epoch gets a
// perfectly good key for the wrong epoch and no error at all. A caller that verifies has to read
// the record's own epoch first and refuse to compute when it is not this one.
func (self *Group) PairwiseExport(label string, peer LeafIndex, length int) ([]byte, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		return nil, errGroupClosed
	}
	if peer == self.ownLeaf {
		return nil, ErrPairwiseSelf
	}
	ownNode := self.tree.Leaf(self.ownLeaf)
	peerNode := self.tree.Leaf(peer)
	if ownNode == nil || peerNode == nil {
		return nil, ErrBlankLeaf
	}
	// (*KeySchedule).Export's two refusals, for its reasons and over the same two panics. The
	// length is Expand's ceiling and the label is one opaque<V> of a KDFLabel, and BOTH of them
	// reach a CryptoProvider method whose signature cannot report anything -- an over long label
	// takes the process down inside mlsLabelBytes and an impossible length inside Expand. This
	// declaration is exported, takes both from a caller, and already answers a caller's mistake
	// with a typed error, so it answers these two as well rather than letting them panic.
	//
	// AND THEY ARE ASKED IN FRONT OF THE DIFFIE-HELLMAN, which is this tree's standing rule about
	// a refusal and the work it precedes: a caller that cannot be served should not have had a
	// curve operation spent on it first.
	if length < 0 || length > 255*self.crypto.HashSize() {
		return nil, fmt.Errorf("%w: %d", ErrExportLength, length)
	}
	if err := checkLabelledFieldLength("pairwise exporter", " label",
		len(MlsLabelPrefix)+len(label)); err != nil {

		return nil, err
	}
	priv, err := X25519PrivateKey(self.ownPriv.EncryptionPriv)
	if err != nil {
		return nil, err
	}
	pub, err := X25519PublicKey(peerNode.EncryptionKey)
	if err != nil {
		return nil, err
	}
	dh, err := X25519DH(priv, pub)
	if err != nil {
		return nil, err
	}
	// the raw shared secret is erased on the way out. ExpandWithLabel has already read it by
	// then, and what the caller is handed is the expansion rather than the point.
	defer zeroizeSecret(dh)

	// the pair, sorted, with each public point carried in the position its index names. The two
	// members run this method with (ownLeaf, peer) swapped, so the sort is the whole of why they
	// agree -- and moving a point without moving its index is the case the ordering forbids.
	lo, hi := uint32(self.ownLeaf), uint32(peer)
	pkLo, pkHi := ownNode.EncryptionKey, peerNode.EncryptionKey
	if hi < lo {
		lo, hi = hi, lo
		pkLo, pkHi = pkHi, pkLo
	}
	pairContext, err := marshalPairwiseContext(self.context.GroupId, self.context.Epoch,
		lo, hi, pkLo, pkHi, self.schedule.Secrets().EpochAuthenticator)
	if err != nil {
		return nil, err
	}
	return self.crypto.ExpandWithLabel(dh, label, pairContext, length), nil
}

// EpochSecret exposes exactly the two secrets MASTER section 8.2 needs and refuses every other
// name. The default arm is a refusal rather than a zero value, so a caller that invented a third
// name is told so instead of being handed an empty slice it would go on to treat as a key.
func (self *Group) EpochSecret(name EpochSecretName) ([]byte, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		return nil, errGroupClosed
	}
	secrets := self.schedule.Secrets()
	switch name {
	case EpochSecretSenderData:
		return cloneBytes(secrets.SenderData), nil
	case EpochSecretEncryption:
		return cloneBytes(secrets.Encryption), nil
	}
	return nil, fmt.Errorf("mls: epoch secret name %d is not in the closed enum", name)
}

// senderDataSecretLocked is EpochSecret's sender_data arm with the state lock already held, which
// is membersLocked's relationship to Members one section up.
//
// It answers the schedule's OWN slice rather than a copy, because the one caller seals with it and
// keeps nothing: EpochSecret copies because it hands the secret to somebody outside this package,
// and a copy here would be a second live copy of an epoch secret for the length of a seal.
//
// IT IS A DECLARATION AND NOT AN INLINE READ, and that is worth writing down because it looks like
// one. The labelled composition walk in labelled_composition_test.go taints every value read off a
// field that carries a serialized structure -- self.schedule does, since
// NewKeyScheduleFromEpochSecret is a producer in that walk -- and it taints through a method call
// on the tainted receiver, so an inline self.schedule.Secrets().SenderData handed to
// SealPrivateMessage reads there as the serialized GROUP CONTEXT arriving at a labelled field with
// nothing bounding it. It is not that: it is KDF.Nh octets of ExpandWithLabel output, and it enters
// the AEAD as a key derivation input rather than as a labelled field. A call into a declaration of
// this package is a frame of its own to that walk, which is where the two stop being one value, and
// this frame is walked in its turn rather than being a place the question is skipped.
func (self *Group) senderDataSecretLocked() []byte {
	return self.schedule.Secrets().SenderData
}

// RatchetTree returns the encoded public tree, for out-of-band Welcome delivery and for MASTER
// section 8.2's per-epoch snapshot record.
//
// The tree is the one structure not bounded by the codec's 1 MiB default, so it is written under
// MaxRatchetTreeLength -- the same bound UnmarshalRatchetTree reads it back at.
//
// IT NO LONGER ASKS ValSem300 OF THE IN-MEMORY TREE, which is the SECOND instance of the fault
// validateCommitPostTreeIsExportable was repaired for and is written here rather than left for the
// next reader to find. Section 12.4.3.3's trailing blank rule is about the ARRAY this method
// EMITS, and MarshalMLS strips the trailing blanks as it writes; an in-memory tree is held at the
// full width 2^(d+1)-1, so a group whose size is not a power of two ends in blank nodes the moment
// an Add extends its tree. Measured: with the rule asked here, a three member group could not
// publish its own tree -- and there was no three member group in this build until commit generation
// landed, which is why the fixture corpus worked around it at 512 leaves rather than reporting it.
//
// AND NOTHING TAKES ITS PLACE, which is a decision rather than a hole. The one thing left to refuse
// about a tree at full width is an array with no non-blank node in it, and (*RatchetTree).MarshalMLS
// already refuses exactly that with ErrTreeMalformed on the line below -- so a guard here would be a
// second copy covering for the one really doing the work, which is the shape this file rejects in
// (*Group).ProposeGroupContextExtensions and which no input could tell apart from the encoder's own
// answer. validateCommitPostTreeIsExportable states that half itself because it encodes nothing.
func (self *Group) RatchetTree() ([]byte, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return syntax.MarshalLimit(self.tree, syntax.MaxRatchetTreeLength)
}

// GroupContext returns the serialized GroupContext for the current epoch. Every framing entry point
// takes these bytes rather than the struct, because a group context is inlined into
// FramedContentTBS with no framing of its own and a second encoding of it is a second answer.
func (self *Group) GroupContext() ([]byte, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return syntax.Marshal(self.context)
}

// GroupPolicy returns the parsed urmessage_group_policy extension.
func (self *Group) GroupPolicy() (*GroupPolicyExtension, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return GroupPolicyOf(self.context.Extensions)
}

// Close releases the group. Epoch state stays in the store; the secrets held in memory are zeroized
// and dropped.
//
// It is idempotent, because a caller that closes twice -- a deferred close beside an explicit one
// -- has made no mistake, and because Zeroize on a schedule that is already gone would be a nil
// dereference in a cleanup path.
//
// //go:noinline for secret_zeroize.go's reason, which is the class rather than this body: these
// stores are dead in the compiler's reading -- nothing reads the schedule afterwards -- and the
// directive is the only thing between them and a compiler entitled to delete them.
//
//go:noinline
func (self *Group) Close() error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.closed = true
	if self.schedule != nil {
		self.schedule.Zeroize()
	}
	if self.secretTree != nil {
		self.secretTree.Zeroize()
	}
	// THE STAGED EPOCH IS ERASED TOO, and it was not until this line existed. A close that
	// dropped self.pending left a complete second epoch in the heap -- its own key schedule, its
	// own secret tree, and the leaf private key this client drew for the epoch its commit would
	// have opened -- held by nothing that erases it, which is exactly the discipline
	// StagedCommit's own comment invokes and was not taking.
	self.pending.Zeroize()
	// and this epoch's own leaf private state and signing key, which are storage this group
	// DECLARES rather than storage it points at. Both are copies this group made -- NewGroup
	// clones the caller's signing key and draws the leaf key itself -- so the erase reaches
	// nothing the caller is still holding.
	self.ownPriv.Zeroize()
	zeroizeSecret(self.signer)
	self.schedule = nil
	self.secretTree = nil
	self.pending = nil
	self.ownPriv = nil
	self.signer = nil
	return nil
}

// persist writes the current epoch's state blob.
//
// Called with the state lock held, or before the group has been handed to anybody, which is the
// creation path's case. Task 19 defines the blob and the past-epoch window; this task writes only
// the current epoch.
//
// The group id is CLONED on the way out, for the reason GroupId() clones on the way out. The store
// is an object the caller supplies and goes on holding -- the sdk writes these implementations
// over its own sealed local store -- so a store that keeps the slice it was handed, to key a map
// or to build a path with later, shares an array with this group for the group's lifetime, and a
// write through it rewrites the group id every epoch secret of this group was derived over.
// Nothing downstream would report that: the context stays self consistent with the octets it now
// holds, and the transcript, the tree hash and the key schedule all go on agreeing with each
// other over the wrong id.
func (self *Group) persist() error {
	state, err := self.marshalState()
	if err != nil {
		return err
	}
	err = self.store.PutGroupState(cloneBytes(self.context.GroupId), self.context.Epoch, state)
	// AND THE ENCODING IS ERASED ONCE THE STORE HAS TAKEN IT. These octets are not a view over
	// anything: syntax.Writer copies into a buffer of its own, so what comes back out of the
	// marshal is a SECOND copy of this epoch's parent secret and of this member's leaf private
	// key, and after this statement nothing in the process can reach it to erase it. It is
	// erased on the refusal too, which is the path that matters most -- a store that answered
	// an error may have written nothing, and the copy would otherwise outlive the failure.
	//
	// What this asks of a StateStore is written on the interface: PutGroupState is handed a
	// buffer the group erases as soon as the call returns, so an implementation that wants
	// these octets past the call owes itself a copy. The alternative is a plaintext epoch
	// secret left in the heap for the collector to move around, once per merged commit.
	zeroizeSecret(state)
	return err
}

// sealAndRecordLocked seals one message under this epoch and RECORDS what the seal spent before the
// ciphertext leaves this group.
//
// THE PERSIST IS WHAT MAKES A SEAL SURVIVABLE. A seal consumes a generation of this leaf's ratchet,
// and the persisted state is what a later LoadGroup rebuilds that ratchet from -- so a send that
// did not write the new position back leaves the stored state standing where the ratchet was
// BEFORE it. The restored member then draws a generation it has already used: every peer answers
// ErrRatchetGenerationConsumed and drops the message, and, one layer worse, two different
// plaintexts of this epoch have gone out under one (key, base nonce) pair for this leaf and
// generation. The only thing between that and an AEAD nonce collision is the 32 bit reuse_guard.
//
// ONE DOOR AND NOT THREE PERSISTS, because there are three seal sites in this file -- a proposal, a
// commit and an application message -- and all three draw from the same two ratchets of the same
// leaf. A site somebody adds later records what it spent by calling this rather than by remembering
// to, which is the same argument the erase helpers of this package are written under.
//
// IT RUNS BEFORE THE CALLER IS HANDED ANYTHING, AND ITS FAILURE IS THE CALL'S. The alternative --
// answer the ciphertext and report the store's refusal some other way -- hands out a message whose
// generation nothing has recorded, which is the defect above reached one step later. A refusal here
// costs the generation and nothing else: the ciphertext is dropped inside this function, so nothing
// under that generation ever reaches a peer, and what is left behind is the ordinary out-of-order
// gap every receiver already handles.
//
// WHAT IT COSTS is one PutGroupState per message sent, and marshalState re-encodes the ratchet tree
// each time. That is the price of the store being the durability boundary; the alternative is a
// client whose crash recovery reuses a nonce.
//
// The caller holds stateLock.
func (self *Group) sealAndRecordLocked(authenticated *AuthenticatedContent) (*PrivateMessage, error) {
	private, err := SealPrivateMessage(self.crypto, self.secretTree,
		self.senderDataSecretLocked(), authenticated, PaddingSizeV1)
	if err != nil {
		return nil, err
	}
	if err := self.persist(); err != nil {
		return nil, err
	}
	return private, nil
}

// sealAndRecordBoundLocked is sealAndRecordLocked under MASTER section 8.4.2's S3 pin: the frame is
// emitted only if the generation it was sealed at is the one the caller's aad_mls names.
//
// THE PERSIST RUNS ON THE PIN'S REFUSAL TOO, and that is the one thing about this body that is not
// obvious. By the time the pin can fire, the ratchet has already moved -- the generation is spent
// whether or not anybody ever sees the ciphertext -- so a refusal that skipped the persist would
// leave the stored state standing where the ratchet was before it, and a later LoadGroup would draw
// a generation this member has already used. That is the exact defect sealAndRecordLocked's own
// header describes: two plaintexts of one epoch under one (key, base nonce) pair, with nothing but
// the 32 bit reuse_guard between them and an AEAD nonce collision. The pin exists to be LOUD and
// cheap, not to be free.
//
// A persist failure on that path is reported WITH the pin's refusal rather than instead of it,
// because the two say different things to an operator: one is a rule this build enforced, the other
// is a durability boundary that did not answer.
//
// The caller holds stateLock.
func (self *Group) sealAndRecordBoundLocked(authenticated *AuthenticatedContent,
	boundGeneration uint32) (*PrivateMessage, error) {

	private, sealErr := sealPrivateMessageBound(self.crypto, self.secretTree,
		self.senderDataSecretLocked(), authenticated, make([]byte, PaddingSizeV1), boundGeneration)
	if errors.Is(sealErr, errSealGenerationNotBound) {
		if persistErr := self.persist(); persistErr != nil {
			return nil, fmt.Errorf("%w (and the persist that would have recorded the spent generation failed: %w)",
				sealErr, persistErr)
		}
		return nil, sealErr
	}
	if sealErr != nil {
		return nil, sealErr
	}
	if err := self.persist(); err != nil {
		return nil, err
	}
	return private, nil
}

// groupStateBlobVersion is bumped when the blob layout changes, so a state written by an older
// build is REFUSED rather than misread. The two are not the same failure: a misread blob decodes
// into a group whose secrets nobody agrees with, and the first symptom of one is a member whose
// every message is refused by peers that are all behaving correctly.
// Version 2 carries this member's TreeKEM path secrets. Version 1 did not, and a state written at
// it restores a member holding its own leaf key and nothing above it -- which is a working group
// of TWO, where every ciphertext a commit addresses to this member stands at its own leaf, and a
// member WITH A COPATH NODE ABOVE ITS OWN LEAF that cannot open the next commit from the far side
// of the tree.
//
// THAT IS A PROPERTY OF THE MEMBER'S POSITION AND NOT OF THE GROUP'S SIZE, which is worth spelling
// out because this comment used to say "a group of four or more" and that is false. Measured over
// sizes two to eight by TestFourIsTheSmallestGroupWhoseMembersEnterTheLadderAboveTheirOwnLeaf: at
// FIVE members leaf 4 stands alone under the right subtree exactly as leaf 2 does at three, so it
// enters every sender's commit at its own leaf and restores correctly with an empty ladder, while
// four of its five neighbours do not. Four is the smallest size at which EVERY member is affected,
// which is a different sentence from the one this said.
//
// Refused rather than read with an empty ladder, because that restore is the defect.
//
// Version 3 carries this member's own SENDER RATCHET POSITION beside the ladder. Version 2 did
// not, and a state written at it restores a member whose next Protect draws generation 0 of a
// ratchet every peer is already past: each peer answers ErrRatchetGenerationConsumed and drops the
// message -- and the head differs per peer, so the recovery is not even uniform -- while two
// different plaintexts of that epoch have been sealed under one (key, base nonce) pair for this
// leaf and generation. The only thing between that and an AEAD nonce collision is the 32 bit
// reuse_guard. Refused rather than read at generation 0, for the reason version 1 is refused
// rather than read with an empty ladder: the restore IS the defect.
const groupStateBlobVersion uint16 = 3

// pathSecretEntry is one rung of this member's TreeKEM ladder as the persisted state carries it:
// the node the secret belongs to, and the secret.
//
// A VECTOR OF PAIRS AND NOT A POSITIONAL LIST, which is (*TreeKEMPrivate).PathSecrets' own reason
// read one layer out. That map is keyed by node rather than by depth because a member's filtered
// direct path gains and loses nodes as members are added and removed, so a secret written down by
// its position in a path is a secret silently RENAMED the first time it is read back against a
// differently filtered path -- and a restore is exactly a read-back at a distance, where nothing
// on either side of the write can compare the two shapes.
type pathSecretEntry struct {
	Node   NodeIndex
	Secret []byte
}

// The element halves, named rather than written as closures at the call site, for
// writeOneHpkeCiphertext's stated reason: TestEverySyntaxEncoderInThisPackageUsesTheDefaultLimit
// pins every syntax entry point of this package by the SOURCE TEXT of the call.
func writeOnePathSecret(w *syntax.Writer, entry pathSecretEntry) error {
	w.WriteUint32(uint32(entry.Node))
	w.WriteOpaque(entry.Secret)
	return w.Err()
}

func readOnePathSecret(r *syntax.Reader) (pathSecretEntry, error) {
	node, err := r.ReadUint32()
	if err != nil {
		return pathSecretEntry{}, err
	}
	secret, err := r.ReadOpaque()
	if err != nil {
		return pathSecretEntry{}, err
	}
	return pathSecretEntry{Node: NodeIndex(node), Secret: secret}, nil
}

// zeroizePathSecretEntries erases every secret in a DECODED path secret vector.
//
// Only ever called on the read side, and the asymmetry is the point. On the write side each entry
// is a view of the live private state's own storage, so an erase there would destroy the ladder
// the group is running on -- the drop defect pointing backwards. On the read side every entry is
// storage syntax.Reader allocated for this one call, and LoadGroup owes it the erase it already
// owes the other two secrets the blob carries.
func zeroizePathSecretEntries(entries []pathSecretEntry) {
	for _, entry := range entries {
		zeroizeSecret(entry.Secret)
	}
}

// senderRatchetEntry is one of this member's OWN sender ratchets as the persisted state carries
// it: which ratchet, how many generations it has handed out, and the secret the next one comes
// out of.
//
// A COUNT AND NOT THE (head, exhausted) PAIR the ratchet itself holds, and (*ratchet).consumed
// says why: the pair has a state it must never be in, and a field a corrupted store can write is
// a field that will one day hold it.
//
// THE SECRET IS HERE RATHER THAN THE COUNT ALONE, which is the trade this entry makes and the one
// worth reading. A count on its own is enough -- a fresh ratchet stepped forward that many times
// reaches the same secret -- and it would make a restore cost one KDF call per message this member
// has ever sent in the epoch, a number the store's contents choose, up to 2^32. Carrying the
// secret makes the restore O(1) and adds NO secret to the blob that was not already derivable from
// it: the same state carries the restore secret, and the encryption secret expanded out of that
// derives every leaf's every ratchet from generation 0. What it does add is a second copy on the
// DISK, which is the store's obligation to seal -- spec A section 3.5 -- exactly as for the two
// secrets this blob already carried.
type senderRatchetEntry struct {
	Kind     RatchetType
	Consumed uint64
	Secret   []byte
}

// The element halves, named rather than written as closures at the call site, for
// writeOnePathSecret's reason.
func writeOneSenderRatchet(w *syntax.Writer, entry senderRatchetEntry) error {
	w.WriteUint8(uint8(entry.Kind))
	w.WriteUint64(entry.Consumed)
	w.WriteOpaque(entry.Secret)
	return w.Err()
}

func readOneSenderRatchet(r *syntax.Reader) (senderRatchetEntry, error) {
	kind, err := r.ReadUint8()
	if err != nil {
		return senderRatchetEntry{}, err
	}
	consumed, err := r.ReadUint64()
	if err != nil {
		return senderRatchetEntry{}, err
	}
	secret, err := r.ReadOpaque()
	if err != nil {
		return senderRatchetEntry{}, err
	}
	return senderRatchetEntry{Kind: RatchetType(kind), Consumed: consumed, Secret: secret}, nil
}

// zeroizeSenderRatchetEntries erases every secret in a DECODED sender ratchet vector.
//
// Read side only, for zeroizePathSecretEntries' reason and it is the same asymmetry: on the write
// side each Secret is a view of the live ratchet's own storage, so an erase there would destroy
// the ratchet this group is sending under.
func zeroizeSenderRatchetEntries(entries []senderRatchetEntry) {
	for _, entry := range entries {
		zeroizeSecret(entry.Secret)
	}
}

// pathSecretsOf is a member's TreeKEM ladder as the vector the blob carries, IN NODE ORDER.
//
// SORTED, because a map has no order and these octets go to a disk. Two persists of one unchanged
// epoch that answered different octets would make every comparison anybody makes over a stored
// state -- did this epoch change, is this the state I wrote, do two devices hold the same one --
// report a difference that is nothing but Go's map iteration seed.
//
// Insertion sorted rather than through slices.SortFunc, and the IMPORT is the reason rather than
// the cost: this package's constant-time gate derives its banned comparator class out of the
// imports each production file makes, so an import added here for a sort widens that class over
// the file holding every door onto this group's secrets. The vector is one entry per level of the
// tree -- nine at five hundred members -- so writing the sort out costs nothing.
//
// NO SECRET IS COPIED, which is marshalState's rule for every field of this blob: the slice is new
// and each Secret in it is a view of the live private state's storage, so this adds no second copy
// of any path secret to the heap. The one copy is the encoder's output, and persist erases that.
func pathSecretsOf(priv *TreeKEMPrivate) []pathSecretEntry {
	if priv == nil {
		return nil
	}
	out := make([]pathSecretEntry, 0, len(priv.PathSecrets))
	for node, secret := range priv.PathSecrets {
		out = append(out, pathSecretEntry{Node: node, Secret: secret})
	}
	for i := 1; i < len(out); i += 1 {
		for j := i; 0 < j && out[j].Node < out[j-1].Node; j -= 1 {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// groupStateBlob is one epoch of one group as it is handed to the StateStore.
//
// IT IS A WIRE TYPE WHOSE WIRE IS A DISK, and that is worth writing down here because no gate in
// this package draws the distinction. Everything else carrying a codec here encodes octets a peer
// already has; these octets are this client's own epoch_secret or joiner_secret and its own leaf
// HPKE private key, and the only reason handing them to a caller-supplied object is acceptable at
// all is spec A section 3.5, which requires the store implementation to SEAL them before they
// touch disk. The erase class in staged_erase_test.go excuses a type that declares a codec on the
// argument that everything it holds is about to be public; that argument is false of this one, and
// what is done about it instead is below.
//
// IT DECLARES NO ERASE ON PURPOSE. On the WRITE path it holds no storage of its own: every field
// is a view of the live epoch -- the schedule's parent secret, the group's own leaf key, the
// transcript this group is standing on -- so a Zeroize here would erase the epoch the group is
// running on, which is the drop defect pointing backwards. The one copy the write path makes is
// the encoder's output, and persist erases that. On the READ path every field is a copy
// syntax.Reader made, and LoadGroup erases the two that are key material as soon as the schedule
// and the leaf state have been rebuilt from them.
//
// One codec method set and no struct tags, which is C1 and is this package's rule for every type
// it serializes at all.
type groupStateBlob struct {
	Version       uint16
	Context       []byte
	Tree          []byte
	Confirmed     []byte
	Interim       []byte
	OwnLeaf       uint32
	OwnEncPriv    []byte
	RestoreKind   uint8
	RestoreSecret []byte
	// this member's own TreeKEM ladder. LAST, so that the version 1 layout is a strict prefix of
	// this one and a build reading a version 1 state stops at the version check rather than at a
	// truncated vector -- which is the difference between "migrate or discard this state" and a
	// decode failure that says nothing about why.
	PathSecrets []pathSecretEntry
	// and where this member's own two ratchets stand, which is what makes this state a restore of
	// a SENDER rather than only of a receiver. LAST for the reason the ladder is last: each layout
	// is a strict prefix of the next, so a state written by an older build stops at the version
	// check rather than at a truncated vector.
	SenderRatchets []senderRatchetEntry
}

var _ syntax.Codec = (*groupStateBlob)(nil)

// MarshalMLS writes the blob. The version is FIRST so that the decoder can refuse a layout it does
// not read before it has interpreted one octet of the rest as anything.
func (self *groupStateBlob) MarshalMLS(w *syntax.Writer) error {
	w.WriteUint16(self.Version)
	w.WriteOpaque(self.Context)
	w.WriteOpaque(self.Tree)
	w.WriteOpaque(self.Confirmed)
	w.WriteOpaque(self.Interim)
	w.WriteUint32(self.OwnLeaf)
	w.WriteOpaque(self.OwnEncPriv)
	w.WriteUint8(self.RestoreKind)
	w.WriteOpaque(self.RestoreSecret)
	if err := syntax.WriteVector(w, self.PathSecrets, writeOnePathSecret); err != nil {
		return err
	}
	return syntax.WriteVector(w, self.SenderRatchets, writeOneSenderRatchet)
}

// refuseDecodedGroupState erases the key material a REFUSED decode has already read, and answers
// the refusal.
//
// IT EXISTS BECAUSE THE RECEIVER IS NOT ASSIGNED ON THESE PATHS, which is what makes a refusal
// here the one door in this file whose caller cannot clean up after it. LoadGroup installs three
// deferred erases over the blob it passes in, and every one of them runs over the value the caller
// still holds -- so a decode that read this member's leaf key, its restore secret and its whole
// ladder and then refused leaves those three defers erasing nil, while the octets themselves are
// storage syntax.Reader allocated for this one call and nothing in the process can reach again.
//
// ONE FUNCTION AND NOT AN ERASE WRITTEN AT EACH RETURN, which is this package's rule for drop
// sites and is the half that was wrong here: the ladder order refusal was added as a fourth exit
// past the first secret and erased nothing, beside three read failures that erased nothing either.
// A site somebody adds later erases the whole of what the decode holds rather than the fields
// whoever wrote it remembered.
//
// The noinline directive is the erase-helper class's: every store is inside zeroizeSecret and the
// two vector helpers, reached with storage of the staged value handed over as an argument.
//
//go:noinline
func refuseDecodedGroupState(staged *groupStateBlob, err error) error {
	zeroizeSecret(staged.OwnEncPriv)
	zeroizeSecret(staged.RestoreSecret)
	zeroizePathSecretEntries(staged.PathSecrets)
	zeroizeSenderRatchetEntries(staged.SenderRatchets)
	return err
}

// UnmarshalMLS reads the blob. It STAGES -- every field into one local value, the receiver
// assigned whole at the end -- for the reason every decoder in this package stages: a truncated
// state has to leave the caller holding what it had rather than a composite of two epochs.
//
// ONE STAGED VALUE AND NOT NINE LOCALS, which is the shape the erase below needs. Every refusal
// past the first secret drops this member's leaf key, its restore secret and its whole ladder, and
// a refusal is the one exit whose caller cannot erase them -- see refuseDecodedGroupState. With
// nine locals each such exit would carry its own list of what to erase, which is the enumeration
// that was already wrong here in four places.
func (self *groupStateBlob) UnmarshalMLS(r *syntax.Reader) error {
	version, err := r.ReadUint16()
	if err != nil {
		return err
	}
	// REFUSED HERE AND NOT BY THE CALLER, which is what the version being written first is for and
	// what it was not doing. LoadGroup used to make this comparison after the whole decode had
	// succeeded, which works for exactly as long as every layout is the same LENGTH: the moment
	// version 2 appended the path secret vector, a version 1 state stopped decoding at all and the
	// caller's named refusal -- "this is a state to migrate or discard" -- was replaced by a
	// truncation error that says nothing about why. The check belongs at the first field that can
	// answer it.
	if version != groupStateBlobVersion {
		return fmt.Errorf("%w: the state is version %d and this build writes %d",
			errGroupStateBlobVersion, version, groupStateBlobVersion)
	}
	staged := groupStateBlob{Version: version}
	staged.Context, err = r.ReadOpaque()
	if err != nil {
		return err
	}
	staged.Tree, err = r.ReadOpaque()
	if err != nil {
		return err
	}
	staged.Confirmed, err = r.ReadOpaque()
	if err != nil {
		return err
	}
	staged.Interim, err = r.ReadOpaque()
	if err != nil {
		return err
	}
	staged.OwnLeaf, err = r.ReadUint32()
	if err != nil {
		return err
	}
	// EVERY REFUSAL FROM HERE ON DROPS KEY MATERIAL, AND A DROPPED DECODE IS AN ERASED ONE. The
	// four fields below are this member's leaf HPKE private key, the secret its whole epoch is
	// rebuilt from, every rung of its TreeKEM ladder and the secret both of its sender ratchets
	// stand on. This read is the last one that can fail with nothing yet to erase.
	staged.OwnEncPriv, err = r.ReadOpaque()
	if err != nil {
		return err
	}
	staged.RestoreKind, err = r.ReadUint8()
	if err != nil {
		return refuseDecodedGroupState(&staged, err)
	}
	staged.RestoreSecret, err = r.ReadOpaque()
	if err != nil {
		return refuseDecodedGroupState(&staged, err)
	}
	staged.PathSecrets, err = syntax.ReadVector(r, readOnePathSecret)
	if err != nil {
		return refuseDecodedGroupState(&staged, err)
	}
	staged.SenderRatchets, err = syntax.ReadVector(r, readOneSenderRatchet)
	if err != nil {
		return refuseDecodedGroupState(&staged, err)
	}
	// STRICTLY INCREASING, which is the shape marshalState writes and therefore the only shape this
	// build can have produced. It refuses a reordered vector and, more importantly, a REPEATED node:
	// two entries naming one node collapse into one map entry when the ladder is rebuilt, so the
	// restored member holds one fewer rung than it persisted and nothing downstream can tell.
	// See errGroupStateLadderOrder.
	//
	// THE SENDER RATCHET VECTOR HAS THE SAME RULE AND IT IS NOT MADE HERE, which is a difference
	// worth writing down rather than leaving to be noticed. A repeated node in the ladder is
	// invisible after this function, because the vector becomes a MAP one statement into LoadGroup
	// and the collapse has already happened; a repeated ratchet type is invisible at the point it
	// is INSTALLED, because installing is what overwrites. So that check stands in
	// (*SecretTree).RestoreSenderRatchets, beside the store it protects, and this one stands here.
	for i := 1; i < len(staged.PathSecrets); i += 1 {
		if staged.PathSecrets[i].Node <= staged.PathSecrets[i-1].Node {
			return refuseDecodedGroupState(&staged, fmt.Errorf("%w: entry %d names node %d after node %d",
				errGroupStateLadderOrder, i, staged.PathSecrets[i].Node, staged.PathSecrets[i-1].Node))
		}
	}
	*self = staged
	return nil
}

// marshalState serializes the epoch this group is standing in.
//
// WHAT IT STORES IS AN INPUT AND NOT AN OUTPUT. EpochSecrets is nine expansions of one parent, and
// a blob carrying the nine would hand a restored group a SECOND path to the same epoch -- so a
// restore and the group it restored would agree for exactly as long as the two paths agreed, and
// nothing on either side would report the day they stopped. What is stored is the ONE secret this
// epoch's schedule was built from, beside the kind that says which constructor takes it: the
// sampled epoch_secret for a group this client created, the joiner_secret for every epoch a commit
// opened. LoadGroup rebuilds through the constructor that built it in the first place.
//
// NO FIELD OF THE BLOB IS A COPY. Each is a view of storage this group already holds, so this
// method adds no second copy of any secret to the heap; the one copy is the encoder's output, and
// persist erases that as soon as the store has taken it.
//
// The RAISED bound, and it is a capacity rather than an acceptance rule. These octets are this
// client's own local state and never travel, and the tree inside them is written by tree.go's own
// encoder at MaxRatchetTreeLength -- so a default-limit writer here would refuse to persist a
// group this build is entitled to hold, at 500 members, and would refuse it only on the largest
// group anybody ever made.
func (self *Group) marshalState() ([]byte, error) {
	// asked FIRST, because it is the one field of this blob that can be missing: an epoch whose
	// schedule has been erased answers a refusal here rather than KDF.Nh zero bytes, and the
	// alternative is persisting a state that restores a group anybody can recompute.
	restoreSecret, err := self.schedule.restoreSecret(self.restoreKind)
	if err != nil {
		return nil, err
	}
	// asked SECOND and for the first field's reason: an erased secret tree refuses here rather
	// than answering one zero secret per ratchet, and the alternative is persisting a sender
	// position every party in the world can derive keys from.
	senderRatchets, err := self.secretTree.SenderRatchets(self.ownLeaf)
	if err != nil {
		return nil, err
	}
	tree, err := syntax.MarshalLimit(self.tree, syntax.MaxRatchetTreeLength)
	if err != nil {
		return nil, err
	}
	context, err := syntax.Marshal(self.context)
	if err != nil {
		return nil, err
	}
	blob := &groupStateBlob{
		Version:       groupStateBlobVersion,
		Context:       context,
		Tree:          tree,
		Confirmed:     self.transcript.Confirmed,
		Interim:       self.transcript.Interim,
		OwnLeaf:       uint32(self.ownLeaf),
		OwnEncPriv:    self.ownPriv.EncryptionPriv,
		RestoreKind:   uint8(self.restoreKind),
		RestoreSecret: restoreSecret,
		// AND THE LADDER, which is what makes this state a restore of a member rather than of a
		// group. Without it a restored member holds its own leaf key alone: every ciphertext a
		// commit addresses to it stands at its own leaf in a group of two -- which is why nothing
		// reported this for as long as the corpus had no larger group -- and stands at a copath
		// node it can no longer derive a key for as soon as this member HAS a copath node above
		// its own leaf and the next commit comes from the other side of the tree.
		//
		// That is a property of the member's POSITION and not of the group's size, and this
		// comment used to say "four members" as though it were the second. Leaf 4 of a group of
		// FIVE stands alone under the right subtree, enters every sender's commit at its own leaf,
		// and restores correctly with an empty ladder; leaf 0 of a group of THREE does not. Four
		// is the smallest size at which every member is affected, which is a different sentence.
		PathSecrets: pathSecretsOf(self.ownPriv),
		// AND WHERE THIS MEMBER'S OWN RATCHETS STAND, which is what makes the state a restore of a
		// SENDER. Without it a restored member draws generation 0 of a ratchet every peer is past:
		// each peer refuses the message as a replay, and two different plaintexts of this epoch go
		// out under one (key, base nonce) pair for this leaf and generation. See the version
		// constant, and (*SecretTree).SenderRatchets for why nothing is created to answer it.
		SenderRatchets: senderRatchets,
	}
	return syntax.MarshalLimit(blob, syntax.MaxRatchetTreeLength)
}

// ---------------------------------------------------------------------------
// RFC 9420 section 12.1: proposal generation
// ---------------------------------------------------------------------------

// THE FOUR METHODS BELOW ARE THE SENDING SIDE OF EVERY RULE THIS PACKAGE'S RECEIVE PATH ALREADY
// ENFORCES, and that symmetry is the design rather than a property somebody hoped for.
// validate_proposals.go holds ValSem101-113 and section 12.1.2's two rules on an Update's own leaf;
// validate_commit.go holds ValSem200-209; (*ProposalCache).Store holds the v1 profile, the epoch
// binding and section 12.2's ceilings. A generator that emitted a proposal any of those would
// refuse is a client publishing a message its own package will not process, and the refusal arrives
// at a PEER, one commit later, with nothing at the point of the mistake to point at.
//
// So a proposal built here goes through this package's own doors before it is returned:
//
//   - checkProposalProfile, before a single octet is signed;
//   - the structure's own validator for the two kinds that carry one a peer will judge --
//     (*KeyPackage).Validate for an add, GroupPolicyOf for a group context extension set;
//   - ValidateProposalList itself, over a list holding this one proposal, which is section 12.2's
//     whole procedure asked at generation time and is the door the three above cannot stand in
//     for -- see (*Group).propose;
//   - and the CACHE, which re-runs the profile gate, checks the epoch binding and applies the
//     per sender and per leaf ceilings.
//
// TestEveryProposalThisGroupGeneratesIsAcceptedByItsOwnValidationDoors is what holds the claim, by
// putting each generated proposal through ValidateProposalList rather than by asserting over the
// calls made here. A proposal this package refuses to validate is a finding about the generator.
//
// AND ONE GATE DOES ASSERT OVER THE CALLS, deliberately, because the sentence above is stated over
// the four methods that exist rather than over the class of them.
// TestEveryProposalGeneratorOnThisGroupSendsWhatItEmitsThroughItsOwnDoors derives that class --
// every exported method of *Group that puts a message of content type proposal on the wire -- and
// requires each member to reach ValidateProposalList. Measured, which is why it is here: a FIFTH
// generator doing its own framing, signing, sealing and Store passes every dynamic obligation in
// group_test.go while emitting a proposal these doors refuse, because those obligations are
// reached through this method and it is not.
//
// EPOCH STATE IS UNTOUCHED. A proposal is a request and not a change: nothing below writes the
// tree, the group context, the transcript or the key schedule, and the epoch a caller reads after
// proposing is the epoch it read before. Two things DO move, and both are meant to. The sender's
// own message ratchet advances a generation, exactly as an application message would -- a proposal
// is an ordinary PrivateMessage on the wire and is meant to be indistinguishable from one -- and
// the proposal cache grows the entry a later commit will name.
//
// THE PROFILE IS defaultProfile() AND NOT ONE THE GROUP KEPT, which is this package's convention
// and not an omission here. `profile` is an empty struct with one constructor, so every profile in
// this build is the same value; validate_proposals.go, validate_commit.go and the cache all reach
// for it the same way, and NewGroup is the single place a caller's *profile is read at all. When
// p8 lands a Profile that actually carries state, that swap is a package-wide one and these are
// two more of its call sites.

// ProposeAdd proposes that the client holding keyPackage join the group.
//
// The key package is judged HERE and not only at commit time. That is the difference between a
// caller learning that it fetched an expired, wrong-suite or wrongly signed package now, while it
// still has the directory it fetched from in hand, and learning it from a peer's refusal of a
// commit several steps later. Section 10.1's whole door is run rather than a version and suite
// comparison written out again in this file, so a package this build would refuse to ADMIT is one
// it also refuses to ADVERTISE.
//
// The clock is this client's own, for (*KeyPackage).Validate's stated reason: section 7.3 makes the
// lifetime a MUST for a leaf a client is about to admit, and this is the moment it is admitted.
//
// The v1 wrap target is required as well, and it is not part of section 10.1. Every commit this
// profile makes wraps the epoch's secret to urmessage_leaf_keys, so a joiner whose leaf carries
// none is a member no epoch can ever reach -- and the first symptom of admitting one is a member
// who cannot read anything, one commit after the commit that added them.
//
// NEITHER OF THOSE ASKS WHETHER THE GROUP ALREADY CARRIES THIS CLIENT'S KEYS, and that is not
// asked here either. Section 10.1 judges a key package against itself and a suite; ValSem101 and
// ValSem103 judge it against the members this group already has, and both of them are decidable
// off the pre-commit tree. They are asked once, for every generator, in (*Group).propose -- so
// what is left in this method is the part of an Add's judgement no list rule makes, and the part
// every list rule makes is not written out again here where a fourth generator would miss it.
func (self *Group) ProposeAdd(keyPackage []byte) ([]byte, error) {
	self.stateLock.Lock()
	crypto, suite, closed := self.crypto, self.context.CipherSuite, self.closed
	self.stateLock.Unlock()
	if closed {
		return nil, errGroupClosed
	}
	// decoded rather than taken as a structure, because what a peer will hash into the
	// KeyPackageRef and re-validate is these octets. syntax.Reader copies every opaque field it
	// reads, so the value below shares no array with the caller's buffer.
	var kp KeyPackage
	if err := syntax.Unmarshal(keyPackage, &kp); err != nil {
		return nil, err
	}
	if err := kp.Validate(crypto, suite, time.Now()); err != nil {
		return nil, err
	}
	if _, err := LeafKeysOf(&kp.LeafNode); err != nil {
		return nil, err
	}
	return self.propose(&Proposal{ProposalType: ProposalTypeAdd, Add: &Add{KeyPackage: kp}})
}

// ProposeRemove proposes removing a leaf.
//
// REMOVING OUR OWN LEAF IS REFUSED, and the reason is what this method does with the proposal
// rather than anything RFC 9420 forbids. Section 12.1.3's leave flow IS a member sending a Remove
// for its own leaf and another member committing it, so the proposal itself is legitimate; what is
// not legitimate is this client CACHING one. Every proposal generated here is stored in this
// epoch's cache, and task 13's commit generation commits what the cache holds -- so a self remove
// sitting in it is a proposal this client's own next commit would carry, and
// validateCommitterIsNotRemoved refuses exactly that at every receiver. This plan has no leave
// flow to hand such a proposal to instead, so the refusal is here and the gap is named rather than
// left as a proposal that poisons the next commit.
//
// A leaf that is blank or outside the tree is ValSem108's refusal, asked at generation time under
// ValSem108's own value: a caller that named a member who has already been removed learns it from
// the call it made rather than from a commit nobody accepts.
func (self *Group) ProposeRemove(leaf LeafIndex) ([]byte, error) {
	self.stateLock.Lock()
	own, closed := self.ownLeaf, self.closed
	occupied := self.tree.Leaf(leaf) != nil
	self.stateLock.Unlock()
	if closed {
		return nil, errGroupClosed
	}
	if leaf == own {
		return nil, fmt.Errorf("%w: leaf %d is this client's own and a cached self remove is one this client's next commit would carry",
			ErrRemoveCommitter, leaf)
	}
	// REDUNDANT WITH THE DOOR (*Group).propose NOW RUNS, AND KEPT, on ValidateProposalList's own
	// stated terms and measured the same way: ValSem108 answers this condition the same value off
	// the same tree, so deleting this line leaves the whole of ./mls/... and ./message/... green.
	// It stays because it is asked before the lock and before anything is signed, and because the
	// asymmetry it used to be the only half of -- this generator asking its receivers' question
	// while ProposeAdd did not -- is what that door was written for. Nothing here claims a test
	// can tell which of the two guards fired.
	if !occupied {
		return nil, fmt.Errorf("%w: leaf %d", ErrRemoveNonMember, leaf)
	}
	return self.propose(&Proposal{ProposalType: ProposalTypeRemove, Remove: &Remove{Removed: leaf}})
}

// ProposeUpdate proposes replacing our own leaf with one holding a FRESH encryption key.
//
// THE FRESH KEY IS THE ENTIRE POINT OF THE PROPOSAL. An update that republished the key it
// replaces would be accepted by everything structural about it -- the leaf validates, the signature
// verifies, the tree still hashes -- and would provide no post compromise security at all, which is
// the only thing an Update buys. Section 12.1.2 states it as a rule and this package answers it
// with ErrUpdateEncryptionKeyUnchanged on the receive side; the draw below is the send side of the
// same rule, and TestProposeUpdateDrawsItsEncryptionKeyFreshAndFromItsOwnDraw is what holds it, by
// finding the published key again in the draws the provider was asked for rather than by asserting
// over this line.
//
// THE LEAF IS BUILT HERE AND NOT THROUGH NewLeafNode, which mints the key_package variant. That
// constructor fills in a Lifetime section 7.2's update arm does not carry and signs over a
// LeafNodeTBS that excludes the group id and the leaf index, so its answer would have to be mutated
// and re-signed anyway -- and a Lifetime left standing under a source whose encoding drops it is a
// field the Go value carries and no peer ever sees. This is testUpdateLeafNodeNaming's shape, which
// is the fixture every update this package validates is built from, and the Clone before the
// signature is NewLeafNode's own discipline: a leaf that goes on sharing an array with anything is
// a leaf that can change after it was signed.
//
// The device wrap key is read off the leaf being REPLACED and re-encoded, so an update publishes
// the same urmessage_leaf_keys the group already wraps to. A member whose update quietly changed it
// is a member the next commit wraps to a key it does not hold.
func (self *Group) ProposeUpdate() ([]byte, error) {
	var crypto CryptoProvider
	var signer SignaturePrivateKey
	var cred Credential
	var groupId []byte
	var at LeafIndex
	var leafKeysExt Extension
	var store StateStore
	if err := func() error {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return errGroupClosed
		}
		current := self.tree.Leaf(self.ownLeaf)
		if current == nil {
			return fmt.Errorf("%w: this group holds no leaf at its own index %d",
				ErrTreeMalformed, self.ownLeaf)
		}
		keys, err := LeafKeysOf(current)
		if err != nil {
			return err
		}
		// re-encoded through the extension's own Encode, which answers the tag and the body
		// together and answers them as FRESH storage. The parse above hands back a body cut from
		// the tree's own extension, and a replacement leaf carrying that array would be a leaf
		// with a window onto the ratchet tree this epoch's tree hash was taken over.
		encoded, err := keys.Encode()
		if err != nil {
			return err
		}
		crypto, signer, at, store = self.crypto, self.signer, self.ownLeaf, self.store
		// the identity is copied for the leaf's sake and not the group's: everything below runs
		// outside this lock, and the credential is about to be signed over.
		cred = Credential{
			CredentialType: self.cred.CredentialType,
			Identity:       cloneBytes(self.cred.Identity),
		}
		groupId = cloneBytes(self.context.GroupId)
		leafKeysExt = encoded
		return nil
	}(); err != nil {
		return nil, err
	}

	signatureKey, err := signaturePublicKeyOf(signer)
	if err != nil {
		return nil, err
	}
	// the key pair this proposal exists to publish. Its own draw and its own DeriveKeyPair, for
	// NewKeyPackage's reason: one draw feeding two derivations answers two identical key pairs and
	// nothing about the value that comes back says so.
	encPriv, encPub, err := crypto.DeriveKeyPair(crypto.Random(crypto.HashSize()))
	if err != nil {
		return nil, err
	}
	leaf := (&LeafNode{
		EncryptionKey:  encPub,
		SignatureKey:   signatureKey,
		Credential:     cred,
		Capabilities:   v1Capabilities(),
		LeafNodeSource: LeafNodeSourceUpdate,
		Extensions:     []Extension{leafKeysExt},
	}).Clone()
	// the group id and the leaf index are what section 7.2's update arm binds and the key_package
	// arm does not, so an update leaf signed with nil and 0 verifies against itself and against no
	// receiver -- see (*LeafNode).signatureContent.
	if err := leaf.Sign(crypto, signer, groupId, at); err != nil {
		return nil, err
	}
	// one verify, for NewLeafNode's reason: a provider whose signing half and verifying half
	// disagree, or a preimage that cannot be rebuilt from what was just written, is a leaf every
	// peer refuses and nothing in the value that comes back says so.
	if err := leaf.VerifySignature(crypto, groupId, at); err != nil {
		return nil, err
	}
	// THE PRIVATE HALF IS FILED BEFORE THE PROPOSAL IS PUBLISHED, and that order is the only one
	// that cannot lose it. A client that published an update and then failed to store the key
	// would be committed into an epoch whose own leaf key it does not hold, by a committer acting
	// entirely correctly on what it was sent. The pair is copied on the way out because the store
	// is an object the caller supplies and keeps what it is handed.
	if err := store.PutPrivateKey(cloneBytes(encPub), cloneBytes(encPriv)); err != nil {
		return nil, err
	}
	return self.propose(&Proposal{ProposalType: ProposalTypeUpdate, Update: &Update{LeafNode: *leaf}})
}

// ProposeGroupContextExtensions proposes replacing the group context's extension list wholesale.
//
// WHOLESALE IS THE RFC'S SEMANTICS AND NOT THIS METHOD'S CHOICE: section 12.1.6's proposal carries
// an extensions vector that REPLACES the group's, so a caller that means to add one extension
// passes the group's current list with the new entry in it. A caller that passes only the new entry
// drops the policy, which is what GroupPolicyOf refuses below.
//
// Every entry is judged before the proposal is built, so a policy violation never reaches the wire:
// checkGroupExtension is the same derived gate ValSem209 asks of a commit that installs one, so
// this build cannot propose an extension set it would itself refuse to install. GroupPolicyOf is
// the second door and it is this profile's rather than the RFC's -- a group here always carries a
// policy, and an extension set with none is a group with no owner and no roles, which nothing
// downstream would report until the first message that needed one.
func (self *Group) ProposeGroupContextExtensions(exts []Extension) ([]byte, error) {
	self.stateLock.Lock()
	closed := self.closed
	self.stateLock.Unlock()
	if closed {
		return nil, errGroupClosed
	}
	// THE CALLER'S ENTRIES ARE JUDGED AND FORWARDED AS THEY ARE, AND NOT COPIED HERE, which is the
	// opposite of NewGroup's rule at the other end of the same list and is opposite for the reason
	// that rule is stated over: RETENTION. NewGroup copies because the group's context KEEPS what
	// it was handed for the whole life of the group. Nothing below keeps anything -- the entries
	// are read into a preimage that is signed, into a message that is sealed and encoded, and into
	// the proposal cache, which makes its OWN copy through the codec because it is the thing that
	// outlives the call.
	//
	// A copy here as well would be two copies each covering for the other, which is the shape
	// proposal_list.go's own header rejects over its two map initialisations: neither line is one
	// any test can observe, and the one really doing the work stops being obvious. Measured, not
	// argued -- with the copy in place, deleting the CACHE's clone left this side green, and with
	// the copy gone TestTheProposalThisGroupCachesSharesNoStorageWithItsCaller fails on it.
	proposed := exts
	active := defaultProfile()
	for i := range proposed {
		if err := active.checkGroupExtension(proposed[i].ExtensionType); err != nil {
			return nil, fmt.Errorf("%w: at group_context_extensions entry %d", err, i)
		}
	}
	if _, err := GroupPolicyOf(proposed); err != nil {
		return nil, err
	}
	// AND THE REQUIRED CAPABILITIES BODY, READ THROUGH THE SAME LOOKUP ValSem209 READS IT THROUGH.
	// This is not a second policy check: it is the one extension of the installed set that the
	// receiving side reads BY TYPE, and reading it here is what makes a set carrying two of them a
	// refusal at generation rather than a choice the committer gets to make. Without it this
	// generator publishes a proposal ValSem209 refuses -- measured, by the probe in
	// extension_lookup_test.go's row for this method, which is the shape this whole section is
	// built to prevent.
	//
	// What is NOT asked here is ValSem209's other half, whether every member the commit LEAVES in
	// the group supports what the new set requires. That question is the commit's rather than the
	// proposal's: which members remain depends on the removes the same commit carries, and none of
	// them exist yet.
	if _, err := requiredCapabilitiesOf(proposed); err != nil {
		return nil, err
	}
	return self.propose(&Proposal{
		ProposalType:           ProposalTypeGroupContextExtensions,
		GroupContextExtensions: &GroupContextExtensions{Extensions: proposed},
	})
}

// propose frames, signs, protects, caches and encodes one proposal.
//
// The group context crosses into the framing layer as BYTES: FramedContentTBS inlines it with no
// length prefix of its own, and handing over the serialized form is what makes that impossible to
// get wrong here.
//
// The group id in the FramedContent is a COPY of the group's and not the group's, which is
// (*Group).persist's rule one call further out. sealPrivateMessage carries content.GroupId straight
// into the PrivateMessage header it builds, so a content assembled over the live slice would put
// the octets every epoch secret of this group was derived over into a structure this method hands
// to an encoder.
//
// IT IS A MEASURED SURVIVOR AND IT STAYS, which is worth writing down rather than leaving for the
// next reader to rediscover. Deleting the clone leaves the whole of ./mls/... and ./message/...
// green: the PrivateMessage that receives it is marshalled and dropped inside this call, and a
// CachedProposal holds a proposal and a sender and no group id, so nothing on this path retains it
// today. It is NOT the shape the extension copy in ProposeGroupContextExtensions was -- that one
// was a second copy standing beside the cache's, which was doing the work, and deleting the cache's
// is what a test could see. This is the only copy on this path, so the property is held by this
// line or by nobody, and what it guards is the one value every epoch secret of this group was
// derived over.
//
// THE CACHE IS WRITTEN LAST, AND THE ORDER IS INVERTED FROM THE PLAN'S ON PURPOSE. Stored first,
// every failure after it -- the seal, the encode -- leaves the cache holding a proposal no peer
// ever received, which task 13's commit generation then names in a commit every receiver refuses
// with "proposal reference is not cached for this epoch". The caller is told the propose failed and
// the group has quietly disagreed with it. Stored last, the only cost of a refusal is a generation
// of this leaf's own handshake ratchet spent on a message that was never returned, which every
// receiver already tolerates: generations are consumed by the sender and gaps are ordinary.
// Cheap and invisible on one side, silent divergence on the other, so the cheap failure is the one
// this takes.
//
// *SecretTree is passed where the framing layer declares a MessageKeySource; framing_protect.go
// carries the var _ MessageKeySource = (*SecretTree)(nil) assertion, so a drift between the two
// fails at build rather than at the first message.
//
// The compiler directive is the package's convention for a member of the class
// TestEveryEraseHelperCarriesTheNoInlineDirective derives, and it is not a claim that this body
// erases a secret. That class is "writes through storage that outlives the call", read off the
// source; this writes into the receiver's own proposal cache, which is the shape, and
// (*ProposalCache).Store carries the directive under the same reading and says so.
//
//go:noinline
func (self *Group) propose(proposal *Proposal) ([]byte, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		return nil, errGroupClosed
	}
	// before a single octet is signed, for (*Proposal).checkArm's reason one layer down: what is
	// signed here is a serialized form, and a caller that ignored an error out of the encoder must
	// not be able to walk away with a signature over a proposal that does not exist.
	//
	// REDUNDANT WITH THE CACHE'S OWN CALL AND KEPT, on ValidateProposalList's stated terms and
	// measured the same way: (*ProposalCache).Store runs checkProposalProfile too, so deleting this
	// line leaves the whole of ./mls/... and ./message/... green. It stays because the cache runs
	// LAST here -- see the ordering note above -- so without it a proposal outside the v1 profile is
	// signed, sealed and encoded, and a generation of this leaf's ratchet is spent, before anything
	// refuses it. Nothing here claims a test can tell which of the two guards fired.
	if err := checkProposalProfile(defaultProfile(), proposal); err != nil {
		return nil, err
	}
	// AND THROUGH THE DOOR EVERY RECEIVER WILL JUDGE IT AT, over a list holding this one proposal.
	//
	// THE OBLIGATION IS DERIVED HERE RATHER THAN RESTATED IN EACH GENERATOR, and that is the
	// difference between this and the two checks it makes unnecessary. ProposeAdd ran kp.Validate
	// and LeafKeysOf and nothing else, so an Add republishing a signature key this group already
	// carries was accepted, signed, sealed and CACHED -- while ValidateProposalList refuses that
	// same entry as a ONE ENTRY list, which is what says it was never a cross-proposal question
	// for the committer to answer. ValSem101 and ValSem103 each read their members' half off
	// in.Tree.NonBlankLeaves, which is this group's own pre-commit tree and is sitting under this
	// lock; ValSem105, ValSem106 and section 12.1.2's two rules on an Update's leaf are the same
	// shape. A fifth generator written later is inside this by existing, which a third hand
	// written check beside the other two would not have been.
	//
	// THE COMMITTER IS A LEAF INDEX OUTSIDE THIS TREE, and it is the one thing this reading cannot
	// decide. Exactly two rules of section 12.2 are stated over the committer -- ValSem111
	// compares it against an Update's sender, validateCommitterIsNotRemoved against a Remove's
	// target -- and nobody knows yet who will commit this. A leaf beyond the tree's width is named
	// by no proposal a member can send over this tree, so those two rules are left to the
	// committer and every other rule is asked now. That is what "the cross-proposal rules are the
	// committer's" was always meant to cover, and a one entry list is not it.
	//
	// It runs BEFORE the signature for the reason the profile gate above does: a refusal here
	// costs nothing, and the same refusal after the seal has already spent a generation of this
	// leaf's ratchet on a message no peer will ever be sent.
	if err := ValidateProposalList(&ProposalValidationInput{
		Crypto:    self.crypto,
		Tree:      self.tree,
		Context:   self.context,
		Committer: LeafIndex(self.tree.LeafWidth()),
		List: NewProposalList([]CachedProposal{
			{Proposal: *proposal, Sender: self.ownLeaf}}),
		Now: time.Now(),
	}); err != nil {
		return nil, err
	}
	groupContext, err := syntax.Marshal(self.context)
	if err != nil {
		return nil, err
	}
	content := &FramedContent{
		GroupId:     cloneBytes(self.context.GroupId),
		Epoch:       self.context.Epoch,
		Sender:      Sender{SenderType: SenderTypeMember, LeafIndex: self.ownLeaf},
		ContentType: ContentTypeProposal,
		Proposal:    proposal,
	}
	authenticated, err := SignAuthenticatedContent(self.crypto, self.signer,
		WireFormatPrivateMessage, content, groupContext)
	if err != nil {
		return nil, err
	}
	// A-ASSUME-4: handshake traffic travels as a PrivateMessage, so the transport learns neither
	// which member proposed nor what was proposed.
	private, err := self.sealAndRecordLocked(authenticated)
	if err != nil {
		return nil, err
	}
	encoded, err := MarshalMLSMessage(&MLSMessage{
		Version:        ProtocolVersionMls10,
		WireFormat:     WireFormatPrivateMessage,
		PrivateMessage: private,
	})
	if err != nil {
		return nil, err
	}
	// the cache is handed the AuthenticatedContent and not the Proposal, because the reference an
	// entry is keyed by is a hash over the framed, signed content -- which is what a receiver of
	// the octets above will compute for the same message. A cache keyed by anything else holds
	// entries no commit of this group could name.
	if _, err := self.proposals.Store(self.crypto, self.context, authenticated); err != nil {
		return nil, err
	}
	return encoded, nil
}

// ---------------------------------------------------------------------------
// RFC 9420 section 12.4.1: commit generation
// ---------------------------------------------------------------------------

// THE ORDER OF THE STEPS BELOW IS THE RFC'S AND EVERY ONE OF THEM IS A DEPENDENCY.
//
// Section 12.4.1 reads as a list and could be mistaken for a checklist. It is not one: apply the
// proposals, then compute the update path, then the new epoch's secrets, then the confirmation tag
// over the new confirmed transcript hash. Each arrow is a value that does not exist until the step
// before it has run, and an implementation that took them in another order still produces 32 well
// formed octets at every stage:
//
//   - the update path's HPKE context is the NEW epoch's group context, whose tree_hash covers the
//     path's own public keys -- so the secrets are created (which installs those keys), THEN the
//     tree hash is taken, THEN the path is encrypted. That is why CreateUpdatePathSecrets and
//     EncryptUpdatePath are two calls and why no single CreateUpdatePath exists.
//   - the confirmed transcript hash is a function of the SIGNED commit, so the framing and the
//     signature come before it, and the new group context comes after.
//   - the confirmation tag is a MAC over the confirmed transcript hash OF THE EPOCH THIS COMMIT
//     OPENS. A tag taken over the epoch this commit closes is the same length, verifies against
//     nothing any receiver computes, and is what opts.confirmationTagOverPreCommitTranscript makes
//     on purpose.
//
// AND THE COMMIT IS STAGED RATHER THAN MERGED. The delivery service accepts at most one commit per
// (group, epoch), so a committer that advanced its own epoch here would fork itself off the group
// the moment somebody else's commit won that race (MASTER section 9.3). MergePendingCommit is what
// the acceptance calls.

// CreateCommit builds a commit over the named proposals and stages the epoch it opens.
//
// byReference is a slice of SERIALIZED ProposalRef values, so connect/message can name the
// proposals it saw on the wire without holding an mls type. Passing nil means "every valid proposal
// this client has cached for this epoch", which is RFC 9420 section 12.4's SHOULD; passing an empty
// non-nil slice means "none of them", and the two are deliberately different.
//
// byValue and opts.ExtraProposals are appended after the by-reference entries, in that order, and
// both are attributed to the committer -- which is what (*ProposalCache).Resolve does with a
// by-value entry and what makes ValSem111 decidable about them.
//
// THE COMMIT'S OWN VECTOR IS TAKEN FROM THE RESOLVED LIST AND NOT FROM THE ARGUMENTS, which is the
// one structural decision in this body. ValidateCommit holds a commit's ProposalOrRef vector and the
// list resolved from it to each other field by field -- checkListResolvesTheCommitsVector -- and a
// generator that built the two from two sources would be a generator whose commit can disagree with
// its own list. (*ProposalList).Refs rebuilds the vector from the list, so the two are one value
// here by construction rather than by a comparison this file would have to keep passing. It also
// settles the aliasing question for the by-value arm at the same time: the list's proposals were
// copied through the codec by Resolve, so nothing the commit carries is storage its caller still
// holds.
func (self *Group) CreateCommit(byReference [][]byte, byValue []Proposal, opts *CommitOptions) (*CommitResult, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		return nil, errGroupClosed
	}
	// before anything is resolved. A second staged commit would be a client holding two candidate
	// epochs with nothing to say which of them the delivery service accepted, and the repair --
	// ClearPendingCommit -- is a decision the caller makes rather than one this method can.
	if self.pending != nil {
		return nil, ErrPendingCommitExists
	}
	if opts == nil {
		opts = &CommitOptions{}
	}

	// step 1: name the proposals.
	//
	// THE CALLER'S OCTETS ARE NOT COPIED HERE, and that is measured rather than assumed. Nothing
	// below retains this vector: it is read into (*ProposalCache).Resolve, which keys every
	// by-reference entry through `string(entry.Reference)` -- a conversion that copies -- and
	// copies every by-value proposal through the codec, and the commit's own vector is then built
	// from the RESOLVED list rather than from these entries. A clone here would be a second copy
	// covering for the one really doing the work, which is the shape
	// (*Group).ProposeGroupContextExtensions rejects at the other end of the same file: with the
	// clone in place, aliasing the caller's reference left the whole of ./mls/... and
	// ./message/... green, because there was no input that could tell the two programs apart.
	refs := []ProposalOrRef{}
	if byReference == nil {
		refs = append(refs, self.proposals.Pending(self.context)...)
	} else {
		for _, ref := range byReference {
			refs = append(refs, ProposalOrRef{
				Type:      ProposalOrRefTypeReference,
				Reference: ProposalRef(ref),
			})
		}
	}
	for i := range byValue {
		proposal := byValue[i]
		refs = append(refs, ProposalOrRef{Type: ProposalOrRefTypeProposal, Proposal: &proposal})
	}
	for i := range opts.ExtraProposals {
		proposal := opts.ExtraProposals[i]
		refs = append(refs, ProposalOrRef{Type: ProposalOrRefTypeProposal, Proposal: &proposal})
	}
	list, err := self.proposals.Resolve(self.crypto, self.context, self.ownLeaf, refs)
	if err != nil {
		return nil, err
	}
	commit := &Commit{Proposals: list.Refs()}

	// step 2: apply the list to a tree of this call's own, and judge it against the pre-commit
	// state. ApplyProposals clones, so self.tree is untouched however this call ends.
	applied, err := ApplyProposals(self.tree, self.context, self.ownLeaf, list)
	if err != nil {
		return nil, err
	}
	if !opts.skipValidation {
		if err := ValidateProposalList(&ProposalValidationInput{
			Crypto:     self.crypto,
			Tree:       self.tree,
			Context:    self.context,
			Extensions: applied.Extensions,
			Committer:  self.ownLeaf,
			List:       list,
			Now:        time.Now(),
		}); err != nil {
			return nil, err
		}
		// AND THIS PROFILE'S OWN THREE DOORS GO HERE, in this order, between section 12.2's
		// rules and the path: MASTER section 6's membership and device caps and section 8's
		// removal authority are task 20's, and section 11's owner succession is task 21's. All
		// three are decidable off `applied` and `list`, both of which are in hand at this point
		// and neither of which any later step can restate, and all three must run BEFORE the
		// path is built, because a refusal after CreateUpdatePathSecrets has drawn a leaf key
		// costs a key pair and a tree clone for a commit that was never going to be sent.
		//
		// THE PLAN LANDS THEM HERE AS STUBS AND THIS TASK DOES NOT, and that is a deliberate
		// deviation. A body that names its arguments, reads none of them and answers nil is the
		// shape TestNoStubShapesRemainInSource derives and refuses -- "a parameter the body never
		// reads, which is the plausible zero value" -- and the two spellings that get past it are
		// an underscore parameter, which the sealed storage scan refuses in its turn, and a
		// `_ = argument` line, which is that gate's own stated blind spot. A door that decides
		// nothing is not made safer by being written down; what the ordering needs is this
		// paragraph, and what the doors need is their tasks.
	}

	// step 3: the path, in the three ordered steps this file's header states.
	//
	// The provisional context carries the PREVIOUS epoch's confirmed transcript hash, because the
	// new one is a function of this very commit and this commit is not framed yet. That is not an
	// approximation: section 12.4.2 has every receiver decrypt the path under exactly this
	// context, so what matters is that the two sides build the same one.
	commitSecret := ZeroSecret(self.crypto)
	var plan *UpdatePathPlan
	// EVERY REFUSAL BELOW DROPS THE PLAN, and the plan is key material: this epoch's commit
	// secret, every path secret on the ladder, and the leaf private key this commit drew. There
	// are a dozen returns between the plan's construction and the staging that takes ownership of
	// it -- a seal that fails, a door that refuses, a GroupInfo this client cannot sign -- so the
	// erase is written once here rather than a dozen times, and the flag is what keeps it off the
	// one path where a StagedCommit took the plan and owes it an erase of its own.
	//
	// Private is erased HERE and not by (*UpdatePathPlan).Zeroize, for the reason that method's
	// own comment gives: on the path where the plan is handed on, its private half is the leaf
	// state the merge installs.
	handedOn := false
	defer func() {
		if handedOn || plan == nil {
			return
		}
		plan.Zeroize()
		plan.Private.Zeroize()
	}()
	hasPath := CommitPathRequired(list) || opts.Force
	// the tree a RECEIVER judges this commit against, kept apart from the one the epoch ends up
	// with. Section 12.4.2 applies the proposals, validates the path against THAT tree, and
	// merges the path afterwards, and the two trees are not interchangeable here: ValSem207
	// refuses a path publishing a key that already stands in the tree it is handed, so a
	// validator given the tree with the path already installed refuses every commit whose path
	// has a node in it -- which is every commit of a group with more than one member.
	// CreateUpdatePathSecrets mutates the tree it is called on, so the clone is taken first.
	postProposal := applied.Tree
	if hasPath {
		postProposal = applied.Tree.Clone()
		plan, err = applied.Tree.CreateUpdatePathSecrets(self.crypto, self.ownLeaf,
			self.signer, cloneBytes(self.context.GroupId))
		if err != nil {
			return nil, err
		}
		pathTreeHash, err := applied.Tree.TreeHash(self.crypto)
		if err != nil {
			return nil, err
		}
		provisional := &GroupContext{
			Version:                 self.context.Version,
			CipherSuite:             self.context.CipherSuite,
			GroupId:                 cloneBytes(self.context.GroupId),
			Epoch:                   self.context.Epoch + 1,
			TreeHash:                pathTreeHash,
			ConfirmedTranscriptHash: cloneBytes(self.context.ConfirmedTranscriptHash),
			Extensions:              applied.Extensions,
		}
		provisionalBytes, err := syntax.Marshal(provisional)
		if err != nil {
			return nil, err
		}
		// the members this commit ADDS are excluded: they receive the path secret in their
		// Welcome and must not also be sealed to here, where they hold no key yet.
		path, err := applied.Tree.EncryptUpdatePath(self.crypto, plan, self.ownLeaf,
			provisionalBytes, applied.AddedLeaves)
		if err != nil {
			return nil, err
		}
		commit.Path = path
		commitSecret = plan.CommitSecret
	}

	// step 4: frame and sign the commit against the OLD group context, because that is the epoch
	// every receiver is still in and the epoch its signature has to verify under.
	oldContextBytes, err := syntax.Marshal(self.context)
	if err != nil {
		return nil, err
	}
	content := &FramedContent{
		GroupId:     cloneBytes(self.context.GroupId),
		Epoch:       self.context.Epoch,
		Sender:      Sender{SenderType: SenderTypeMember, LeafIndex: self.ownLeaf},
		ContentType: ContentTypeCommit,
		Commit:      commit,
	}
	authenticated, err := SignAuthenticatedContent(self.crypto, self.signer,
		WireFormatPrivateMessage, content, oldContextBytes)
	if err != nil {
		return nil, err
	}

	// step 5: the new transcript hashes, the new group context and the new key schedule.
	confirmedInput, err := authenticated.ConfirmedTranscriptHashInput()
	if err != nil {
		return nil, err
	}
	confirmedHash := ConfirmedTranscriptHash(self.crypto, self.transcript.Interim, confirmedInput)
	treeHash, err := applied.Tree.TreeHash(self.crypto)
	if err != nil {
		return nil, err
	}
	newContext := &GroupContext{
		Version:                 self.context.Version,
		CipherSuite:             self.context.CipherSuite,
		GroupId:                 cloneBytes(self.context.GroupId),
		Epoch:                   self.context.Epoch + 1,
		TreeHash:                treeHash,
		ConfirmedTranscriptHash: confirmedHash,
		Extensions:              applied.Extensions,
	}
	schedule, err := NewKeySchedule(self.crypto, self.schedule.Secrets().InitSecret, commitSecret,
		EmptyPskSecret(self.crypto), newContext)
	if err != nil {
		return nil, err
	}

	// step 6: the confirmation tag over the confirmed transcript hash OF THE EPOCH THIS COMMIT
	// OPENS, and then the interim hash from it.
	tagOver := confirmedHash
	if opts.confirmationTagOverPreCommitTranscript {
		tagOver = self.transcript.Confirmed
	}
	confirmationTag := schedule.ConfirmationTag(tagOver)
	// ConfirmationTag answers nil for an epoch whose confirmation_key has been erased. Nothing can
	// have erased this one -- the schedule was built four statements ago -- so this is a refusal
	// over a build that has stopped agreeing with itself, and it is made here for NewGroup's
	// reason: the alternative is folding a nil tag into the interim hash and staging an epoch
	// whose successor nobody can compute.
	if len(confirmationTag) != self.crypto.HashSize() {
		return nil, fmt.Errorf("%w: the schedule answered %d octets, want %d",
			errCommitConfirmationTag, len(confirmationTag), self.crypto.HashSize())
	}
	authenticated.Auth.ConfirmationTag = confirmationTag
	if opts.dropConfirmationTag {
		authenticated.Auth.ConfirmationTag = nil
	}
	// a CLONE of the transcript, so a refusal below leaves this group's own transcript where the
	// epoch it is still in put it.
	transcript := self.transcript.Clone()
	if err := transcript.Update(self.crypto, confirmedInput, confirmationTag); err != nil {
		return nil, err
	}

	// step 7: the commit this client is about to send, through the door every receiver of it will
	// judge it at.
	//
	// THE CONTEXT IS THE PRE-COMMIT ONE AND THE EXTENSIONS ARE THE POST-COMMIT SET, which is not a
	// muddle. Every rule of section 12.2 is stated over the group the proposals ARRIVED in, the
	// by-reference arm of the vector join resolves each reference against this member's cache --
	// which is bound to the epoch that is closing -- and effectiveExtensions is the field that
	// carries the post-proposal set to the rules that need it. A newContext here answers
	// errProposalNotCached for every commit that names a proposal by reference, which is every
	// commit a real group produces.
	//
	// CheckErrata8745 and CheckErrata8815 are NOT called beside this. ValidateCommit reaches both
	// through validateCommitErrata, so a second call here would be a second transcription of two
	// rules whose whole point is that there is one of each.
	if !opts.skipValidation {
		commitInput := &CommitValidationInput{
			Crypto:          self.crypto,
			PreTree:         self.tree,
			PostTree:        postProposal,
			Context:         self.context,
			Extensions:      applied.Extensions,
			Committer:       self.ownLeaf,
			Own:             self.ownLeaf,
			List:            list,
			Commit:          commit,
			Pending:         self.proposals,
			ConfirmationKey: schedule.Secrets().Confirmation,
			ConfirmedHash:   confirmedHash,
			ConfirmationTag: authenticated.Auth.ConfirmationTag,
			Now:             time.Now(),
		}
		// MEASURED, because a door that never fires reads like one that is not needed. Deleting
		// this call refuses no commit any test of this package builds -- section 12.2's rules are
		// already asked one block up, and this generator builds its own path correctly -- so the
		// only thing that reports its absence today is
		// TestEveryExportedRuleIsAppliedByThisPackageOrPinnedAsUnwired, which reads it as this
		// package shipping a commit validator its own construction does not run. What the call is
		// FOR is the class it was put here for, a generator handing its own door a tree the
		// receiver will not have, and that class has a measured instance: with `postProposal`
		// replaced by the tree the path has already been merged into, ValSem207 refuses the commit
		// HERE and nothing else in this package notices.
		if err := ValidateCommit(commitInput); err != nil {
			return nil, err
		}
		// ValSem205 is not in ValidateCommit's own list and its own comment says why: the
		// confirmation key belongs to the epoch this commit OPENS, so it does not exist until
		// the schedule above has been derived. The caller that has one runs it, and this is
		// that caller.
		if err := ValSem205ConfirmationTag(commitInput); err != nil {
			return nil, err
		}
	}

	// step 8: the new epoch's verified context, which is what the boundary owes the proposal
	// cache. The creator earns one the same way at epoch 0 -- see NewGroup -- and it is earned
	// HERE rather than at the merge so that every refusal this commit can make is made before a
	// generation of this leaf's message ratchet has been spent on it.
	groupInfo := &GroupInfo{
		GroupContext:    *newContext,
		ConfirmationTag: confirmationTag,
		Signer:          self.ownLeaf,
	}
	if err := groupInfo.Sign(self.crypto, self.signer); err != nil {
		return nil, err
	}
	verified, err := groupInfo.VerifiedContext(self.crypto, applied.Tree)
	if err != nil {
		return nil, err
	}
	secretTree, err := NewSecretTree(self.crypto, applied.Tree.LeafWidth(),
		schedule.Secrets().Encryption)
	if err != nil {
		return nil, err
	}

	// step 9: protect the commit under the OLD epoch's keys and put it on the wire. A-ASSUME-4:
	// handshake traffic travels as a PrivateMessage, so the transport learns neither who committed
	// nor what the commit did.
	//
	// LAST, because it is the first thing here that cannot be undone: the seal consumes a
	// generation of this leaf's ratchet whether or not the commit is ever sent.
	private, err := self.sealAndRecordLocked(authenticated)
	if err != nil {
		return nil, err
	}
	commitMessage, err := MarshalMLSMessage(&MLSMessage{
		Version:        ProtocolVersionMls10,
		WireFormat:     WireFormatPrivateMessage,
		PrivateMessage: private,
	})
	if err != nil {
		return nil, err
	}
	encodedTree, err := syntax.MarshalLimit(applied.Tree, syntax.MaxRatchetTreeLength)
	if err != nil {
		return nil, err
	}

	// a commit with no path leaves this client's own leaf key where it was, so the private state
	// carries forward; a commit with one replaces it, and the plan's private half is that
	// replacement.
	//
	// THE CARRIED-FORWARD STATE IS CLONED AND NOT SHARED, which it was not before the staged
	// epoch was given an erase. A staged commit is erased when it is dropped -- ClearPendingCommit
	// on MASTER section 9.3's lost-commit race, and Close -- so a staged commit holding this
	// group's own live leaf private state would erase, on that ordinary path, the key the epoch
	// this group is still in opens its update paths with. It is the reason (*TreeKEMPrivate).Clone
	// exists at all, stated one caller further out.
	ownPriv := self.ownPriv
	if plan != nil && plan.Private != nil {
		ownPriv = plan.Private
	} else if ownPriv != nil {
		ownPriv = ownPriv.Clone()
	}
	staged := &StagedCommit{
		committer:   self.ownLeaf,
		epoch:       newContext.Epoch,
		groupId:     cloneBytes(self.context.GroupId),
		priorEpoch:  self.context.Epoch,
		context:     newContext,
		verified:    verified,
		tree:        applied.Tree,
		schedule:    schedule,
		secretTree:  secretTree,
		ownPriv:     ownPriv,
		transcript:  transcript,
		list:        list,
		commit:      commit,
		added:       applied.AddedLeaves,
		removed:     applied.RemovedLeaves,
		updated:     applied.UpdatedLeaves,
		selfRemoved: applied.SelfRemoved,
		hasPath:     hasPath,
		confirmTag:  confirmationTag,
		plan:        plan,
		restoreKind: restoreFromJoiner,
	}
	// step 10: the Welcome for the members this commit adds, built BEFORE the staged epoch is
	// installed as this group's pending commit.
	//
	// THAT ORDER IS THE REFUSAL'S. welcomeMessage can fail -- a joiner whose key package will
	// not encode, a commit that cannot say where a joiner picks up its path -- and a failure
	// after `self.pending = staged` would leave this group holding a staged epoch its caller was
	// never handed and does not know to clear, while answering that caller an error. So the
	// staged commit stays a local until the last thing that can fail has succeeded, and the one
	// path that drops it erases it: it is a complete second epoch -- a key schedule, a secret
	// tree and the leaf key this commit drew -- and after the return nothing in this process can
	// reach it.
	//
	// IT IS HANDED THE GROUP INFO STEP 8 ALREADY BUILT, SIGNED AND VERIFIED, rather than
	// assembling a second one. Two assemblies of one structure agree with each other for exactly
	// as long as their field lists happen to match, which is the defect welcome_wire.go's header
	// states and which this project has already paid for once on KeyPackage; and this way the
	// group info a joiner opens out of the Welcome is the very object VerifiedContext accepted
	// against the tree this result publishes, rather than a second one built to the same recipe.
	welcome, err := staged.welcomeMessage(self.crypto, groupInfo)
	if err != nil {
		staged.Zeroize()
		return nil, err
	}
	self.pending = staged
	handedOn = true

	return &CommitResult{
		Commit:      commitMessage,
		Welcome:     welcome,
		RatchetTree: encodedTree,
	}, nil
}

// welcomeMessage builds the Welcome for every member this commit added, or nil for a commit that
// added nobody.
//
// A METHOD ON THE STAGED COMMIT AND NOT ON THE GROUP, where the plan wrote it. Every
// field it reads is this commit's own -- the leaves the adds landed on, the proposal list they
// came from, the update path plan, the new epoch's key schedule and the committer's leaf -- so it
// touches no group state and needs nothing the epoch boundary has not already settled.
//
// The shape is also what keeps CreateCommit inside
// TestEveryCompositionEnteringALabelledConstructionIsBoundedBeforeItGetsThere, and that is
// measured rather than incidental. That gate taints a receiver the moment a composition is handed
// to a METHOD ON IT -- "a value handed to a method on a local is part of that local from here on"
// -- so the same body written as a GROUP method, called self.buildWelcome(staged, groupInfo),
// makes every later self.something() in CreateCommit carry the group info, and five of that
// function's own compositions were then reported as reaching a labelled construction through
// self.senderDataSecretLocked(). Written as a method on the value the bytes already belong to,
// the taint stays where it was.
//
// NIL AND NOT AN EMPTY WELCOME for the no-add case, because CommitResult.Welcome is documented as
// nil when the commit adds nobody and a caller branches on it: a Welcome sealed to an empty
// joiner set is a message a delivery service would carry to nobody, at the cost of one AEAD seal
// over the group info every time anybody commits anything.
//
// THE PATH SECRET EACH JOINER IS HANDED is the one for the LOWEST NODE ITS LEAF AND THE
// COMMITTER'S LEAF SHARE, RFC 9420 section 12.4.3.1. That node is on the committer's filtered
// direct path whenever the commit carries a path at all: its child on the joiner's side contains
// the joiner's leaf, which the proposals have already installed in the tree the plan was built
// over, so that child's resolution is non-empty and the node is not one the filter drops. It is
// also exactly the node the joiner is NOT sealed to in the update path itself -- CreateCommit
// excludes the added leaves from EncryptUpdatePath, because a member that holds no key yet cannot
// open anything -- so this secret and that exclusion are two halves of one decision.
//
// The secret is CLONED into the joiner entry rather than aliased off the plan. The entries are
// erased below the moment the seals are made, and an entry that aliased the plan's ladder would
// erase the committer's own path secrets as a side effect of building a message.
func (self *StagedCommit) welcomeMessage(crypto CryptoProvider, info *GroupInfo) ([]byte, error) {
	// the provider first, before anything is read off the receiver: every seal, expansion and
	// reference below is taken through it, and a caller that passed none is told that rather
	// than being answered "this commit added nobody", which is true of the zero value and is
	// not the fault it made.
	if crypto == nil {
		return nil, fmt.Errorf("%w: the welcome is sealed through it", ErrNilCryptoProvider)
	}
	if len(self.added) == 0 {
		return nil, nil
	}
	adds := self.list.Adds()
	if len(adds) != len(self.added) {
		return nil, fmt.Errorf("%w: %d leaf/leaves were added and %d Add proposal(s) are named",
			errWelcomeAddPairing, len(self.added), len(adds))
	}
	joiners := make([]WelcomeJoiner, 0, len(self.added))
	// the entries hold this epoch's path secrets, and they are this function's own copies, so
	// this function is the drop site that owes them an erase. The defer covers the refusals below
	// as well as the ordinary return: a Welcome that could not be built is still a Welcome whose
	// ladder was assembled.
	defer func() {
		for i := range joiners {
			joiners[i].Zeroize()
		}
	}()
	for i, leafIndex := range self.added {
		joiner := WelcomeJoiner{
			KeyPackage: adds[i].Proposal.Add.KeyPackage,
			LeafIndex:  leafIndex,
		}
		if self.plan != nil {
			ancestor := CommonAncestor(leafIndex.NodeIndex(), self.committer.NodeIndex())
			secret := pathSecretAt(self.plan, ancestor)
			if secret == nil {
				return nil, fmt.Errorf("%w: node %d is the lowest node leaf %d and leaf %d share",
					errWelcomePathSecret, ancestor, leafIndex, self.committer)
			}
			joiner.PathSecret = cloneBytes(secret)
		}
		joiners = append(joiners, joiner)
	}

	// the CLEARTEXT suite of the Welcome is the one the epoch this commit opens names, which is
	// the same suite the provider runs -- NewGroup refused any other pairing at creation and no
	// commit changes a group's ciphersuite. BuildWelcome refuses the disagreement rather than
	// trusting that sentence.
	welcome, err := BuildWelcome(crypto, self.context.CipherSuite, info,
		self.schedule.JoinerSecret(), self.schedule.WelcomeSecret(), joiners)
	if err != nil {
		return nil, err
	}
	return MarshalMLSMessage(&MLSMessage{
		Version:    ProtocolVersionMls10,
		WireFormat: WireFormatWelcome,
		Welcome:    welcome,
	})
}

// pathSecretAt is the plan's secret for one node, or nil for a node the plan does not cover.
//
// UpdatePathPlan.Path and UpdatePathPlan.PathSecrets are PARALLEL -- CreateUpdatePathSecrets
// builds one ladder rung per filtered node and slices the commit secret off the end -- so this is
// an index lookup into the structure that already exists rather than a second map of the same
// data kept beside it. A second map is the dual representation the proposal list was rebuilt to
// remove, at a smaller scale.
//
// Nil for a nil plan, for the same reason it answers nil for an unlisted node: "the secret for
// this node, if the commit has one" is the question every call site asks, and the caller that
// cannot proceed without one refuses on the nil rather than on a second guard here.
func pathSecretAt(plan *UpdatePathPlan, node NodeIndex) []byte {
	if plan == nil {
		return nil
	}
	for i, x := range plan.Path {
		if x == node && i < len(plan.PathSecrets) {
			return plan.PathSecrets[i]
		}
	}
	return nil
}

// ClearPendingCommit discards a staged commit. It is what a caller calls when the delivery service
// accepted somebody ELSE's commit for this epoch (MASTER section 9.3, spec A section 5.12): the
// staged epoch is then an epoch nobody entered, and the group has to be able to build another
// commit against the epoch it is still in.
//
// It answers nothing, because there is no failure here to report: clearing a commit that was never
// staged is the same state as clearing one that was.
func (self *Group) ClearPendingCommit() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	// erased BEFORE it is dropped, and this is the ordinary path rather than a cleanup one: the
	// epoch being discarded is fully derived -- key schedule, secret tree and the leaf key drawn
	// for it -- and after the assignment below nothing in this process can reach it to erase it.
	self.pending.Zeroize()
	self.pending = nil
}

// MergePendingCommit promotes the staged commit to live state, persists the epoch it opened, and
// drops every epoch older than the past-epoch window.
//
// THE DELETE IS A SECURITY REQUIREMENT AND NOT HOUSEKEEPING, and it is the reason PastEpochWindow
// is a hard bound here where RFC 9420's ValSem400 makes it a SHOULD. A persisted epoch state
// carries that epoch's parent secret, so it carries everything derived from it -- the resumption
// PSK, the exporter, and through the exporter MASTER section 8.1's eph_root[n], the value that
// section promises becomes undecryptable. A retained old state is a retained eph_root, and no
// amount of erasing in this process reaches one that is on a disk.
//
// WHAT THE WINDOW COSTS, in the terms it is a decision about. Thirty-two superseded epochs are
// kept so that a member can still open a message that was sent under an epoch it has since left --
// a phone that was asleep, a laptop that was shut, a peer whose commit crossed with somebody
// else's. Every one of those epochs is an epoch of FORWARD SECRECY given up: a device compromised
// today hands the attacker the last thirty-three epochs of this group rather than one, and every
// message sent under them. Thirty-two rather than eight because the window is a product promise
// about how long a device may stay offline and an active group can burn eight epochs in a day;
// thirty-two rather than unbounded because unbounded is what OpenMLS ships (openmls#1122) and it
// means the guarantee is never delivered at all. The cutoff is computed off THIS group's epoch,
// and the delete is keyed by THIS group's id: every group this client is in runs an epoch 7, so an
// epoch number alone is not an identity and a window keyed on one would evict a stranger's state.
//
// THE ORDER OF THE THREE THINGS AFTER THE SWAP IS THE DECISION. The cache is rebound BEFORE the
// persist, and epoch_advance_test.go names the other order as a measured defect: the write, then a
// persist that can fail, then the rebind, leaves every failing persist with the group moved and
// the cache bound to the epoch that closed -- and a cache bound to a closed epoch is a member that
// can resolve no proposal of the new epoch, never reaches the next boundary, and is never healed
// by anything in this package. A failed persist is recoverable; the next merged commit writes the
// state again, and the group in memory is correct in the meantime.
//
// THE CACHE IS REBOUND TO THE EPOCH THE GROUP MOVED TO, on every path out of this method, and that
// is the whole of what an epoch boundary owes it. A cache left behind belongs to the epoch that just
// closed and every reference in it names a proposal this commit has already applied; a cache
// rebound to that same closing epoch is worse, because it then refuses every proposal of the new
// epoch as well and nothing in this package releases it.
func (self *Group) MergePendingCommit() error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.pending == nil {
		return ErrNoPendingCommit
	}
	// the SECOND door of errStagedCommitErased's one rule, and it is here rather than only in
	// ApplyCommit because the rule is about installing a staged epoch and this is the method that
	// installs one -- ApplyCommit reaches live state through this call. A guard written only at the
	// caller is a guard that holds for the callers somebody enumerated.
	//
	// The pending commit is LEFT WHERE IT IS rather than dropped, which is this method's discipline
	// at every other refusal: what to do with a staged epoch this group could not enter is
	// ClearPendingCommit's decision and the caller's, and a merge that erased it on the way out
	// would be the same defect one door along.
	if self.pending.erased {
		return errStagedCommitErased
	}
	// and the second door of errStagedCommitInstalled's, for the same reason and in the same
	// place: this is the method that assigns the staged schedule over the live one, so a shell
	// that reached it would erase this group's epoch three statements down and install nothing
	// in its place. Nothing in this package files a shell as pending -- ApplyCommit refuses one
	// before it does -- and the guard stands here so that holds for the callers nobody
	// enumerated. Read BEFORE the erase of the epoch this merge would close, which is the
	// whole point: a refusal after that line is a group with no epoch at all.
	if self.pending.installed() {
		return errStagedCommitInstalled
	}
	staged := self.pending
	// THE EPOCH THIS MERGE CLOSES IS ERASED AS IT IS DROPPED. There is no past-epoch window in
	// this build -- task 19 adds one -- so the schedule, the secret tree and the leaf private
	// state this group holds at this instant become unreachable the moment the assignments below
	// land, and unreachable is not erased. What makes erasing them safe is that none of the three
	// is the value being installed: CreateCommit derives a fresh schedule and a fresh secret tree
	// for the staged epoch, and its leaf private state is either the update path plan's or a
	// CLONE of this one.
	if self.schedule != nil {
		self.schedule.Zeroize()
	}
	if self.secretTree != nil {
		self.secretTree.Zeroize()
	}
	self.ownPriv.Zeroize()
	// the rebind runs BEFORE nothing and AFTER the write, which is the order the boundary is
	// stated in: the cache takes its epoch from the context the group has moved to, and a rebind
	// written ahead of the move would hand it the epoch that is closing.
	self.tree = staged.tree
	self.context = staged.context
	self.verified = staged.verified
	self.schedule = staged.schedule
	self.secretTree = staged.secretTree
	self.ownPriv = staged.ownPriv
	self.transcript = staged.transcript
	self.restoreKind = staged.restoreKind
	// the update path plan is the one piece of key material in the staged commit that no field of
	// this group receives, and it holds the commit secret and every path secret the update path
	// was built from. Its private half is NOT erased here and must not be: it is the leaf state
	// installed six statements above, and (*UpdatePathPlan).Zeroize leaves it alone for that
	// reason.
	staged.plan.Zeroize()
	// AND THE STAGED VALUE IS LEFT A SHELL, which the paragraph below already promised and which,
	// until 2026-09-21, was not true. The three assignments above MOVE the schedule, the secret
	// tree and the leaf private state by pointer, so after them the staged value and this group
	// name one epoch through two handles -- and the staged value is the one a CALLER holds:
	// ProcessMessage handed it out inside a Processed, and ApplyCommit reaches this method with
	// it. A caller that then ran (*StagedCommit).Zeroize on its value -- the ordinary cleanup
	// shape, `defer discard(processed)` beside an ApplyCommit that succeeded, and the exact
	// shape messagegroup's DiscardProcessed exists for -- erased the epoch this group had just
	// entered: measured, Export and Protect refused with the erased-epoch error and the next
	// commit did not decrypt, with no error at the line that did it. So the four fields are
	// DETACHED here, at the one site that knows they were moved rather than dropped: what the
	// staged value goes on holding is the public facts about the commit -- the tree, the
	// context, the leaf vectors, the provenance pair -- and no key material, and an erase run on
	// it afterwards erases nothing this group is running on. The plan is detached with them
	// because its Private half is the same pointer as ownPriv. The flag is NOT set: this value
	// was installed, not erased, and a second ApplyCommit of it is refused BY NAME, as
	// errStagedCommitInstalled, read off the nil schedule this detach leaves. Until 2026-09-21
	// that second ApplyCommit was left to the provenance pair, and the pair does not see a
	// shell handed to another live view of the same member at the same epoch: measured, that
	// view walked the whole door and panicked in the persist on this nil.
	staged.schedule = nil
	staged.secretTree = nil
	staged.ownPriv = nil
	staged.plan = nil
	// AND THE STAGED COMMIT IS RELEASED BEFORE THE REBIND RATHER THAN AFTER IT. Every field of it
	// that holds key material now stands in this group's own storage, so a rebind that refused
	// while self.pending still pointed at it would leave a group whose staged commit and whose
	// live epoch are one epoch -- and the next ClearPendingCommit, which is a caller's ordinary
	// response to a refusal, would erase the epoch this group is running on.
	self.pending = nil
	if err := self.proposals.Rebind(staged.verified); err != nil {
		return err
	}
	if err := self.persist(); err != nil {
		return err
	}
	// the window, computed off the epoch this group HAS MOVED TO. The floor is nought and not a
	// wrapped uint64: a group younger than the window has nothing to drop, and the subtraction
	// written without this guard underflows to a cutoff near 2^64 and deletes every state this
	// group has ever written, on the first commit of every new group.
	cutoff := uint64(0)
	if self.context.Epoch > PastEpochWindow {
		cutoff = self.context.Epoch - PastEpochWindow
	}
	// the group id is CLONED on the way out for persist's stated reason, and it is the same
	// reason twice: this is the second call this method makes into an object the caller supplied
	// and goes on holding, and the live group id is what every secret of this group was derived
	// over.
	return self.store.DeleteGroupStateBefore(cloneBytes(self.context.GroupId), cutoff)
}

// ---------------------------------------------------------------------------
// the epoch this group's OWN staged commit would open, read before it is entered
// ---------------------------------------------------------------------------

// THE FIVE ACCESSORS BELOW ARE WHAT LETS A COMMITTER SUBMIT BEFORE IT MERGES, added 2026-09-22
// for ledger item 242's R2 (the role model's committing arm). MASTER section 9.3 gives the delivery
// service at most one commit per (group, epoch) and has a losing submitter "re-derive against the
// winner and retry", and CreateCommit's own header says the staged epoch is staged and not merged
// for exactly that reason. But the RECORD a committer submits announces the epoch it opens -- the
// new write and read keys, derived through the new epoch's exporter; a hash of the new group
// context; the number of members the fan-out will wrap to -- and until now the only door onto any
// of those was the live group AFTER MergePendingCommit. So every committer merged first and
// submitted second, and a committer that lost the race was left at a private epoch nobody else
// entered: measured, an owner that transferred ownership within one fetch interval of an admin's
// role change could neither open the winner's commit nor seal a record the server would take.
// These read the same facts off the STAGED value, so the merge can wait for the server's answer
// and a refusal costs ClearPendingCommit and nothing else.
//
// EVERY ONE OF THEM READS self.pending AND NOTHING OFF THE LIVE GROUP, and answers
// ErrNoPendingCommit when there is none: a caller that reached these with no commit staged is
// building an announcement for an epoch that does not exist, and the live epoch's facts would
// announce the epoch the group is already in. PendingExport is the one that reaches key material,
// and it refuses an erased value and a shell by name as MergePendingCommit does, for the same
// reason in each case -- an erased schedule exports zeros, and a shell has no schedule at all.
//
// NOTHING HERE IS A SETTER AND NOTHING HERE MOVES STATE. What a caller does with the answers --
// seal an attachment, submit it, then merge or clear -- is the caller's; these five are the reads
// the live group already offers, one epoch early.
//
// THE FIFTH IS PendingRemovedLeaves, ADDED 2026-09-25 FOR LEDGER ITEM 257's RULING 51, and it is
// the one of the five whose answer the live group does NOT offer one epoch later either: once the
// commit is merged the removed leaves are blank, and a blank leaf is indistinguishable from one
// that was never filled. The fact has to be read while the staged commit still holds it, which is
// what makes it an accessor rather than something a caller recomputes. What it is for is the epoch
// fan-out: the wrap set is built PRE-MERGE off the LIVE tree (ruling 37), and a removed member is
// still in that tree, so a fan-out that did not exclude it would seal the next epoch's
// post-quantum secret to the member the commit removes -- ledger item 243 arriving inverted. Until
// this accessor the exclusion was an argument the calling arm passed, which is a fact about
// whether an arm remembered rather than about what the commit does.

// PendingEpoch is the epoch this group's staged commit opens.
func (self *Group) PendingEpoch() (uint64, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.pending == nil {
		return 0, ErrNoPendingCommit
	}
	return self.pending.epoch, nil
}

// PendingMemberCount is how many members the group has in the epoch the staged commit opens:
// every occupied leaf of the staged tree, which is the count Members answers once the commit is
// merged.
func (self *Group) PendingMemberCount() (int, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.pending == nil {
		return 0, ErrNoPendingCommit
	}
	return len(self.pending.OccupiedLeavesAfter()), nil
}

// PendingRemovedLeaves is the leaves this group's staged commit takes OUT of the group, in the
// order the commit's proposals resolved -- (*StagedCommit).RemovedLeaves, which is where the
// commit's Remove proposals were APPLIED against this client's own tree rather than where a
// header said they would be.
//
// IT IS READ OFF THE STAGED COMMIT AND NOT OFF A DIFF OF THE TWO TREES, and that is the decision
// worth stating, because the diff is what a reader reaches for. A commit may remove a leaf and
// ADD a member that lands on the very leaf the removal blanked: RFC 9420 section 12.3 applies
// Removes before Adds and an Add fills the leftmost blank, so the occupied-leaf sets of the live
// and the staged tree can agree exactly while a member was removed. A derivation off that diff
// answers the empty set for such a commit, and the fan-out then seals the next epoch to the
// removed member's own X-Wing key, which is the whole defect the exclusion exists to prevent.
// The staged commit carries the answer directly and no tree comparison can reproduce it. That is
// MEASURED and not reasoned from the RFC, one package over: messagegroup's
// engineremovewithextensions_test.go builds that commit, asserts the two occupied-leaf sets and the
// two member counts EQUAL, and holds this accessor to naming the removal anyway. The order is the
// proposals' and not an ascending one, which the same file drives with two leaves named high to
// low. (Cited by file, not by test name: this package's citation gate checks a named test against
// the tests this package declares, and those two are messagegroup's.)
//
// The slice is storage the caller owns: RemovedLeaves copies, so nothing here aliases the staged
// value the merge is about to install.
func (self *Group) PendingRemovedLeaves() ([]LeafIndex, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.pending == nil {
		return nil, ErrNoPendingCommit
	}
	return self.pending.RemovedLeaves(), nil
}

// PendingGroupContext is the serialized GroupContext of the epoch the staged commit opens, which
// is the value GroupContext answers once the commit is merged. Marshal copies, so what this
// answers shares nothing with the staged value the merge is about to install.
func (self *Group) PendingGroupContext() ([]byte, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.pending == nil {
		return nil, ErrNoPendingCommit
	}
	if self.pending.context == nil {
		return nil, fmt.Errorf("%w: the staged commit carries no group context", ErrNoPendingCommit)
	}
	return syntax.Marshal(self.pending.context)
}

// PendingExport is Export through the STAGED epoch's schedule: RFC 9420 section 8.5's exporter
// over the epoch the staged commit opens, which is the value Export answers once the commit is
// merged. It is the one accessor of the four that reaches key material, and what it hands back
// is a fresh derivation the caller owns and erases, exactly as Export's answer is.
func (self *Group) PendingExport(label string, context []byte, length int) ([]byte, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		return nil, errGroupClosed
	}
	if self.pending == nil {
		return nil, ErrNoPendingCommit
	}
	if self.pending.erased {
		return nil, errStagedCommitErased
	}
	if self.pending.schedule == nil {
		return nil, errStagedCommitInstalled
	}
	return self.pending.schedule.Export(label, context, length)
}

// LoadGroup restores a group from the epoch state a previous run of this client persisted.
//
// THE SIGNING KEY IS THE CALLER'S ARGUMENT AND IS NOT IN THE BLOB, which is the one structural
// decision this function makes. A signature private key is the device's identity across every
// group it is in; an epoch state is one group at one epoch. Putting them in one blob would mean
// that any state a store hands back is enough to speak as this device -- in this group and in
// every other -- and would put the device key inside a value this package writes thirty-three
// copies of per group.
//
// THE SCHEDULE IS REBUILT THROUGH THE CONSTRUCTOR THAT BUILT IT: from the epoch secret for a group
// this client created, from the joiner secret for every epoch a commit opened. See marshalState
// for why a stored EpochSecrets would be the wrong answer -- it is a second derivation path, and a
// second derivation path is how a restored group and a live one come to disagree about the epoch
// authenticator with nothing to say which of them is right.
//
// AND THE PROPOSAL CACHE BINDS TO A VERIFIED CONTEXT, exactly as NewGroup's does and for this
// file's header reason: (*GroupInfo).VerifiedContext is the only door onto one, and a restore is
// not exempt from it. It costs this function nothing it was not going to do anyway and it buys a
// real check -- the group info is signed with the key the CALLER handed in and verified against
// the leaf the restored tree holds at this member's own index, so a caller that restored a state
// with the wrong device key is refused here rather than at the first message a peer drops.
func LoadGroup(cfg *GroupConfig, epoch uint64, signer SignaturePrivateKey) (*Group, error) {
	if cfg == nil {
		return nil, errNilGroupConfig
	}
	crypto := cfg.Crypto
	// refused rather than dereferenced and refused first, for NewGroup's reason: every secret
	// this restore rebuilds and every signature it makes is taken through the provider.
	if crypto == nil {
		return nil, fmt.Errorf("%w: every secret of the restored epoch is rebuilt through it",
			ErrNilCryptoProvider)
	}
	if cfg.Store == nil {
		return nil, errNilStateStore
	}
	// the group id is CLONED on the way out for persist's reason: this hands an array to an
	// object the caller supplies and goes on holding.
	raw, err := cfg.Store.GetGroupState(cloneBytes(cfg.GroupId), epoch)
	if err != nil {
		return nil, err
	}
	// the RAISED bound, matching the one marshalState wrote under. These octets are this
	// client's own and carry a ratchet tree written at MaxRatchetTreeLength, so a default-limit
	// reader here would refuse to restore a group this build persisted correctly.
	var blob groupStateBlob
	if err := syntax.UnmarshalLimit(raw, &blob, syntax.MaxRatchetTreeLength); err != nil {
		return nil, err
	}
	// the two decoded secrets are copies THIS call made -- ReadOpaque allocates rather than
	// viewing -- so they are erased on every path out, once the schedule and the leaf state have
	// been rebuilt from them. What is not erased is raw: that array is the STORE's, and a wipe
	// of it would destroy the state that was just read. The blob's own type declares no Zeroize
	// and says why.
	defer zeroizeSecret(blob.RestoreSecret)
	defer zeroizeSecret(blob.OwnEncPriv)
	// and the ladder, which is the same obligation over the field this version added. It is one
	// call rather than a loop written here for the erase class's reason: the drop sites of this
	// package name a function that erases, so a site somebody adds later erases the whole of what
	// it holds rather than the fields whoever wrote it remembered.
	defer zeroizePathSecretEntries(blob.PathSecrets)
	// and the same obligation over the sender ratchet vector, which carries the secret both of
	// this member's own ratchets are standing on.
	defer zeroizeSenderRatchetEntries(blob.SenderRatchets)
	// the version is NOT compared here. The decoder refuses a layout this build does not read at the
	// first field of the blob, which is where the version is written for that purpose; a second
	// comparison here would be a line no state can reach and therefore a line no test can hold.

	var context GroupContext
	if err := syntax.Unmarshal(blob.Context, &context); err != nil {
		return nil, err
	}
	tree, err := UnmarshalRatchetTree(blob.Tree)
	if err != nil {
		return nil, err
	}
	ownLeaf := LeafIndex(blob.OwnLeaf)
	leaf := tree.Leaf(ownLeaf)
	if leaf == nil {
		return nil, ErrWelcomeLeafNotFound
	}
	// THE PRIVATE HALF OF THE TREE THIS MEMBER HOLDS: its leaf HPKE key, and every path secret it
	// had derived when the state was written.
	//
	// THE LADDER IS THE HALF A TWO MEMBER CORPUS CANNOT SEE MISSING, and it was missing here. In a
	// group of two the only node of a sender's filtered direct path that covers the receiver is the
	// root, and the receiver's own leaf is the whole of that node's copath resolution -- so every
	// commit is sealed to the leaf key, an empty ladder answers every question anybody asks, and a
	// restore that dropped it passes every test there is. At FOUR members a commit from the far side
	// of the tree is sealed to the receiver's sibling subtree root, which is a node no leaf key
	// derives, and a member restored without its ladder answers ErrNoPathSecret to the very next
	// commit it is sent. Persistence is the whole point of this function, so a restore that produces
	// a member who cannot stay in the group is the defect rather than a limitation.
	//
	// EVERY SECRET IS CLONED OUT OF THE BLOB, for the reason the two defers above give: the decoded
	// vector is erased on every path out of this function, and a private state built over those
	// arrays would hold key material its own constructor is about to wipe -- a ladder of zeros,
	// deriving one wrong node key per rung, with nothing anywhere to point at.
	//
	// This is done BEFORE the schedule so the refusal below has nothing to unwind.
	ownPriv := NewTreeKEMPrivate(ownLeaf, HpkePrivateKey(blob.OwnEncPriv))
	for _, entry := range blob.PathSecrets {
		ownPriv.PathSecrets[entry.Node] = cloneBytes(entry.Secret)
	}
	// EVERY REFUSAL FROM HERE ON DROPS KEY MATERIAL THIS CALL BUILT, and a dropped epoch is an
	// erased one. ownPriv holds this device's leaf key and a clone of every rung of its ladder;
	// the schedule below holds the init secret, the confirmation key, the encryption secret, the
	// epoch authenticator, the exporter and the resumption PSK; the secret tree holds every
	// message key derived from that encryption secret. After the return nothing in this process
	// can reach any of it, and unreachable is not erased.
	//
	// IT IS ONE DEFERRED ERASE AND NOT A LIST AT EACH REFUSAL, which is JoinFromWelcome's `handedOn`
	// one function over, and it is here because the list shape had already failed. COUNTED off the
	// body this replaces: TEN refusals stood after this statement, ONE of them erased ownPriv --
	// the Consistent arm, which is the only one anybody wrote it into -- and the other nine
	// dropped this device's leaf key and its whole ladder unerased, two of them dropping the
	// schedule as well. A refusal somebody adds later erases the whole of what this call is
	// holding rather than the values whoever wrote it remembered.
	//
	// The nil guards are the price of a defer installed before the values exist, and they are the
	// honest shape: this runs on the refusals ABOVE the constructors as well.
	handedOn := false
	var schedule *KeySchedule
	var secretTree *SecretTree
	defer func() {
		if handedOn {
			return
		}
		ownPriv.Zeroize()
		if schedule != nil {
			schedule.Zeroize()
		}
		if secretTree != nil {
			secretTree.Zeroize()
		}
	}()
	// AND THE LADDER IS CHECKED AGAINST THE TREE IT IS ABOUT TO BE USED WITH. The interim transcript
	// comparison further down is a commitment to the restore secret and to the kind that says how to
	// use it, and it commits to NOTHING about these secrets: a path secret is not an input to the key
	// schedule at all. So this is the only thing between a ladder that was corrupted, truncated or
	// written for another epoch and a member that derives private keys nobody's public half matches,
	// decrypts nothing, and reports a decryption failure against commits that are perfectly well
	// formed. (*TreeKEMPrivate).Consistent is that check and its own comment names the same symptom.
	if err := ownPriv.Consistent(crypto, tree); err != nil {
		return nil, fmt.Errorf("%w: %w", errGroupStatePathSecret, err)
	}

	switch restoreKind(blob.RestoreKind) {
	case restoreFromEpochSecret:
		schedule, err = NewKeyScheduleFromEpochSecret(crypto, blob.RestoreSecret, &context)
	case restoreFromJoiner:
		// the empty psk secret, which is what every epoch of the v1 profile is built under:
		// nothing in this build injects a PSK, so the joiner path's second input is KDF.Nh zero
		// bytes on both the live side and this one. A profile that grows PSKs owes this blob the
		// psk_secret beside the joiner secret, and until it does, a restore that guessed one
		// would rebuild an epoch nobody else has.
		schedule, err = NewKeyScheduleFromJoiner(crypto, blob.RestoreSecret,
			EmptyPskSecret(crypto), &context)
	default:
		return nil, fmt.Errorf("%w: the state names kind %d", errGroupStateRestoreKind, blob.RestoreKind)
	}
	if err != nil {
		return nil, err
	}

	secretTree, err = NewSecretTree(crypto, tree.LeafWidth(), schedule.Secrets().Encryption)
	if err != nil {
		return nil, err
	}
	// AND THE SENDER POSITION, which is the difference between restoring a member and restoring a
	// member that can still speak. A fresh secret tree starts every ratchet at generation 0, so
	// without this the very next Protect draws a generation every peer is already past: each peer
	// answers ErrRatchetGenerationConsumed and drops the message, and two different plaintexts of
	// this epoch have been sealed under one (key, base nonce) pair for this leaf and generation.
	//
	// THE SENDER POSITION AND NOT THE RECEIVING ONE, and what that costs is stated here rather
	// than left to be met. This blob carries where THIS member's own two ratchets stand and
	// nothing about where its receiving ratchets for its PEERS stood, so every peer's head comes
	// back at 0. Two consequences, and the second is the one the earlier disclosure did not say.
	//
	//   - the replay guard for that epoch is gone: a message from a peer at a generation this
	//     member had already opened is opened again, because the head that would have called it
	//     consumed is back at zero. That is a lost guard and it is not key reuse -- no key of this
	//     member's own is drawn twice by it.
	//   - and a peer that is more than MaxGenerationSkip generations ahead is REFUSED, for the
	//     rest of the epoch. Measured on a settled four: alice Protects 1026 times, live bob opens
	//     all of them, bob is restored, alice Protects once more, and bob answers "generation too
	//     far ahead: generation 1026, head 0, bound 1024" -- and answers it again for every later
	//     message alice sends in that epoch, because nothing in this package moves a receiving
	//     head except a message it ACCEPTS.
	//     TestARestoredMemberIsDeafToAPeerThatMovedPastTheSkipBound drives it to that end.
	//
	//     THIS PARAGRAPH USED TO SAY IT WAS BOUNDED, and the bound it described is gone on
	//     purpose. (*ratchet).peekFor answered a distance past the bound by walking the head
	//     forward by MaxGenerationSkip and then refusing, so a restored member closed the gap by
	//     MaxGenerationSkip-1 per message it lost. That walk was driven by a generation number
	//     arriving inside sender data sealed under a GROUP SHARED secret, which made it a walk any
	//     member could aim at any other member's receiving ratchet -- and a head pushed forward is
	//     a run of that member's messages classified consumed and lost permanently. The
	//     resynchronisation and the deletion channel were one mechanism, and closing the second
	//     closed the first. See (*ratchet).classify.
	//
	//     WHAT THE DURABLE REPAIR IS, so this reads as a gap with an owner rather than as a
	//     regression. The distance exists because this blob does not carry the receiving
	//     positions; a blob that carried them would restore a member that is not behind at all,
	//     and no header from a peer would have to move anything. That is the fix, it is a change
	//     to the persisted state's shape, and it is not this one.
	//   - and "loses" IS the epoch for that peer, which is the sentence a retransmission used to
	//     soften. While a refusal walked the head on by MaxGenerationSkip, retransmitting the
	//     refused generation opened it on delivery ceil(g/MaxGenerationSkip). Nothing walks on a
	//     refusal now, so a retransmission is refused exactly as the first delivery was, however
	//     many times it arrives. The gap closes when the epoch does.
	//
	// WHY NOT PERSIST THE PEERS TOO, since the blob could hold them and they are no more secret
	// than what it already carries -- the encryption secret it rebuilds derives every leaf's every
	// ratchet from generation 0. Because the write points are wrong for it: persist runs at an
	// epoch boundary and inside sealAndRecordLocked, and RECEIVING writes nothing. Bob in the
	// measured case never sent, so a blob with peer heads in it would have recorded the zeros it
	// already assumes. Making it true would mean one PutGroupState per message RECEIVED, which is
	// a durability boundary this package has not chosen.
	//
	// THAT COST USED TO BE BOUNDED BY THE CATCH-UP AND IS NOW NOT BOUNDED AT ALL, and this
	// paragraph says so rather than leaving the old trade standing. The catch-up bounded it by
	// letting any member walk any other member's receiving head, which is the deletion channel
	// (*ratchet).classify describes; a durability boundary is the honest price of closing that,
	// and choosing it is a change to this blob's shape rather than to the ratchet's.
	if err := secretTree.RestoreSenderRatchets(ownLeaf, blob.SenderRatchets); err != nil {
		return nil, fmt.Errorf("%w: %w", errGroupStateSenderRatchet, err)
	}
	transcript := InitialTranscriptHashes()
	transcript.Confirmed = blob.Confirmed
	transcript.Interim = blob.Interim

	// the confirmation tag of the epoch being restored, recomputed rather than stored: it is a
	// MAC under this epoch's confirmation key over this epoch's confirmed transcript hash, and
	// both of those are already here.
	confirmationTag := schedule.ConfirmationTag(transcript.Confirmed)
	if len(confirmationTag) != crypto.HashSize() {
		return nil, fmt.Errorf("%w: the restored schedule answered %d octets, want %d",
			errCreationConfirmationTag, len(confirmationTag), crypto.HashSize())
	}

	// AND THE RECOMPUTED TAG IS CHECKED AGAINST THE INTERIM HASH THIS STATE CARRIES, which is the
	// one thing that says the schedule rebuilt above is the schedule this epoch actually had.
	//
	// It is here because the blob's own fields cannot say it. Every input the restore is handed
	// is KDF.Nh pseudorandom octets and one uint8, so a RestoreKind flipped from one arm to the
	// other -- by a corrupted write, by a truncated field, by anything that got past the store's
	// seal -- hands the other constructor a secret of exactly the length it requires and builds a
	// complete, well formed epoch that agrees with nobody. Measured while writing this: with no
	// check here, LoadGroup answered a nil error over a state whose kind had been flipped, and the
	// group it answered derived a different epoch authenticator from every peer's.
	//
	// The interim transcript hash is what closes it, and it costs one hash. RFC 9420 section 8.2
	// makes interim_transcript_hash_[n] a hash over confirmed_transcript_hash_[n] and the
	// confirmation tag of the commit that opened epoch n -- and that tag is a MAC under epoch n's
	// OWN confirmation key. So the stored interim hash is a commitment to the epoch's confirmation
	// key, which is a commitment to its epoch secret, which is a commitment to the restore secret
	// and the kind that says how to use it. Nothing about a wrong restore can survive it.
	//
	// crypto/subtle.ConstantTimeCompare and not bytes.Equal, which is guardrail 8's rule over
	// every comparison this package ships and not only over the ones whose operands are secret.
	interim, err := InterimTranscriptHash(crypto, transcript.Confirmed, confirmationTag)
	if err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare(interim, transcript.Interim) != 1 {
		return nil, errGroupStateTranscript
	}
	groupInfo := &GroupInfo{
		GroupContext:    context,
		ConfirmationTag: confirmationTag,
		Signer:          ownLeaf,
	}
	if err := groupInfo.Sign(crypto, signer); err != nil {
		return nil, err
	}
	verified, err := groupInfo.VerifiedContext(crypto, tree)
	if err != nil {
		return nil, err
	}
	proposals, err := NewProposalCache(verified)
	if err != nil {
		return nil, err
	}

	group := &Group{
		store:  cfg.Store,
		crypto: crypto,
		// the signing key and the credential are COPIED, for NewGroup's reason: the key is an
		// array the caller owns and goes on using, and the credential here is a view into the
		// restored tree, which a later commit replaces.
		signer:      SignaturePrivateKey(cloneBytes(signer)),
		cred:        Credential{CredentialType: leaf.Credential.CredentialType, Identity: cloneBytes(leaf.Credential.Identity)},
		ownLeaf:     ownLeaf,
		ownPriv:     ownPriv,
		tree:        tree,
		context:     &context,
		verified:    verified,
		schedule:    schedule,
		secretTree:  secretTree,
		transcript:  transcript,
		proposals:   proposals,
		restoreKind: restoreKind(blob.RestoreKind),
	}
	// the one path that does not drop what this call derived: the group owns all three now and
	// owes them its own Close.
	handedOn = true
	return group, nil
}

// ---------------------------------------------------------------------------
// p7 task 16: joining a group from a Welcome and a ratchet tree the CALLER anchored
// ---------------------------------------------------------------------------

// JoinKeyMaterial is what a joiner holds for the KeyPackage a Welcome names: the key package it
// PUBLISHED, and the three private halves it published nowhere.
//
// The key package is carried by value and the three keys by reference, which is the shape a
// caller already has -- (StateStore).TakeKeyPackage hands back the encoding and the two private
// keys it stored beside it, and the signing key is the device's own. Nothing here is retained by
// the group this material joins: the signing key and the credential are CLONED into it and the
// encryption key is cloned by NewTreeKEMPrivate, so a caller goes on owning every array it passed
// and owes each of them the erase below.
type JoinKeyMaterial struct {
	KeyPackage     KeyPackage
	InitPrivate    HpkePrivateKey
	EncryptPrivate HpkePrivateKey
	SignPrivate    SignaturePrivateKey
}

// Zeroize erases the three private keys this material carries.
//
// It is here because this type DECLARES key material, which is the obligation the erase discipline
// states over the types that hold octets rather than over the call sites somebody remembered: the
// init key opens every Welcome addressed to this key package, the encryption key is this member's
// leaf key for as long as it holds that leaf, and the signing key is its identity. A caller that
// dropped one of these after a join left all three in the heap for the collector to move around.
//
// THE KEY PACKAGE'S PUBLISHED HALF IS DELIBERATELY NOT TOUCHED, for (*WelcomeJoiner).Zeroize's
// reason one file over: every field of its encoding went to the delivery service and to every
// member of every group that added this device, so erasing it would take away nothing an attacker
// lacks while destroying a value the caller still owns.
//
// ITS ONE UNPUBLISHED FIELD IS. signPriv is the seed the leaf's signature key was minted from,
// key_package.go's two constructors set it, and marshalCore stops above it -- so a joiner
// assembled out of a key package this process minted is holding a signature private key inside a
// structure that is otherwise entirely public.
//
// UNDER NewKeyPackageWithSigner THAT SEED IS A COPY OF THE CALLER'S OWN SIGNING KEY, and the copy
// is what makes this erase safe rather than what makes it dangerous: the constructor clones, so
// the array this reaches is the key package's and never the device's. A CALLER ASSEMBLING THIS
// STRUCTURE OWES THE SAME. Every field here is erased by name below, so SignPrivate must be an
// array the caller is willing to have destroyed -- a material assembled directly over a device's
// long term signing key loses that key on the first join, and nothing anywhere refuses
// afterwards: an all-zero seed derives a perfectly valid public key and every leaf the device
// publishes after it names that one. (*KeyPackage).Zeroize erases that field and leaves the rest, which is
// why the call is to it rather than to a zeroizeSecret written here: this type cannot see whether
// it was handed a minted key package or one decoded off the wire, and the type that can does not
// have to.
//
// A nil receiver is accepted for zeroizeSecret's reason, and the noinline directive is the erase
// class's rule; see (*TreeKEMPrivate).Zeroize.
//
//go:noinline
func (self *JoinKeyMaterial) Zeroize() {
	if self == nil {
		return
	}
	self.KeyPackage.Zeroize()
	zeroizeSecret(self.InitPrivate)
	zeroizeSecret(self.EncryptPrivate)
	zeroizeSecret(self.SignPrivate)
}

// JoinFromWelcome builds this client's group state from a Welcome and a ratchet tree the CALLER
// obtained, following RFC 9420 section 12.4.3.1's receive steps in the order the section states
// them.
//
// WHAT A CALLER MUST ESTABLISH BEFORE IT CALLS THIS, first because nothing below can do it.
//
// A WELCOME AUTHENTICATES NOBODY. It is HPKE-sealed to an init key its recipient PUBLISHED, in a
// key package that went to the delivery service and to every member of every group that added this
// device, so anybody at all holding that key package can build one addressed to it: mint a ratchet
// tree, sign a GroupInfo at a leaf of that tree with a signing key drawn for the purpose, choose a
// joiner secret, seal it to the published init key. Every check this function makes passes on that
// message, and the client that ran them is then a member of a group whose entire membership is one
// attacker. That is not a gap in this function; it is what a Welcome IS. welcome.go's header says
// it from the tree's end, (*GroupInfo).Verify says it under "WHERE THE TREE COMES FROM", and
// BuildWelcome says it over the object it produces.
//
// THE TREE ARRIVING AS A PARAMETER OF ITS OWN DOES NOT CHANGE THAT BY ONE BIT, and the shape is
// named here because it reads as though it does. The attacker supplies both octet strings:
// ratchetTree is exactly as much the sender's as welcome is, and a caller that lifted this
// argument out of the same delivery that carried the Welcome has authenticated nothing that a
// caller reading the tree out of a ratchet_tree extension would not also have "authenticated".
// (*GroupInfo).Verify establishes that a member OF THE TREE IT WAS HANDED signed this GroupInfo
// about THAT tree, and a forged pair agrees with itself perfectly.
//
// So the caller owes an ANCHOR FOR THE TREE THAT DOES NOT COME FROM THE WELCOME. Concretely, before
// it treats the answered group as the group a human meant to join, it must have established --
// over a channel it already authenticates, which in this product means an existing URmessage
// session or something the two people compared out of band -- the tree hash of the epoch it is
// joining, and it must reconcile that value against the tree it passes here. The group id and the
// epoch of the answered group are reachable through (*Group).GroupId and (*Group).Epoch and the
// tree through (*Group).RatchetTree, so the reconciliation has values to run on; this function is
// handed the result of it and cannot audit it.
//
// cfg.GroupId, when the caller sets it, is checked against the group id the Welcome describes.
// THAT IS AN INTENT MATCH AND NOT AUTHENTICATION, and the distinction is the whole reason it is
// spelled out: a group id is public, and an attacker naming the id of the group its target expects
// passes this check on the first try. What it buys is that a caller which said which group it
// meant to join is not silently placed in another one. A caller that does not yet know the id
// leaves the field empty and the check does not run.
//
// WHAT THIS FUNCTION DOES CHECK, once the tree is granted:
//
//  1. the message is a Welcome, and its cleartext ciphersuite is the one this joiner's key package
//     names, the one this profile creates groups under, and the one the provider runs;
//  2. an entry of it is addressed to THIS joiner's key package reference, and that entry opens
//     under THIS joiner's init private key with this Welcome's own encrypted_group_info as the
//     HPKE context -- which is what makes an entry lifted off another Welcome, or a seal made to
//     another joiner's init key, a refusal rather than a group;
//  3. the group secrets name no pre-shared key, which is outside this profile;
//  4. the encrypted group info opens under welcome_key/welcome_nonce derived from the joiner
//     secret that entry carried;
//  5. a member of the tree the caller passed signed that group info about that tree, and the tree
//     hashes to what its group context names -- both through (*GroupInfo).VerifiedContext, which
//     is (*GroupInfo).Verify and nothing else;
//  6. the tree is a valid ratchet tree for that group context: every leaf signature, every parent
//     hash, the required capabilities, the extensions -- (*RatchetTree).ValidateAgainstContext;
//  7. every group context extension is one this profile admits, and the context carries a group
//     policy;
//  8. a leaf of the tree carries this joiner's signature key, and THE LEAF NODE STANDING THERE IS
//     THE ONE THIS JOINER PUBLISHED, byte for byte;
//  9. the confirmation tag the group info carries is the one this joiner's own key schedule
//     produces over the confirmed transcript hash it carries -- which is what says the epoch this
//     joiner derived is the epoch the sender described;
//  10. every path secret it derives from the one the Welcome carried produces the public key the
//     tree already holds at that node -- (*TreeKEMPrivate).Consistent.
//
// WHAT IT DOES NOT CHECK: that the tree is the group's tree, per every paragraph above; that the
// sender is who it claims to be, because a Welcome carries no claim to check; and that the members
// the tree names are people this user knows, which is the product's question and not this
// function's.
//
// THE TREE IS ALWAYS OUT OF BAND HERE. v1 does not put a ratchet_tree extension in the GroupInfo,
// because MASTER section 8.2 already publishes one snapshot record per epoch and a second copy
// inside every Welcome is the same 300 KB again. A GroupInfo that carries one anyway is not
// ignored -- (*GroupInfo).Verify's rule 9 refuses one describing a tree other than the one in hand
// -- and it is still never read as the tree.
func JoinFromWelcome(cfg *GroupConfig, welcome []byte, ratchetTree []byte,
	keys *JoinKeyMaterial) (*Group, error) {

	if cfg == nil {
		return nil, errNilGroupConfig
	}
	crypto := cfg.Crypto
	// refused rather than dereferenced, and refused before any other argument is judged: every
	// seal this function opens, every secret it derives and every signature it checks is taken
	// through the provider, so a caller that passed none passed nothing this function could have
	// used.
	if crypto == nil {
		return nil, fmt.Errorf("%w: every seal this joiner opens and every secret it derives is taken through it",
			ErrNilCryptoProvider)
	}
	if cfg.Store == nil {
		return nil, errNilStateStore
	}
	if keys == nil {
		return nil, errNilJoinKeyMaterial
	}
	// THE JOINER'S OWN TWO HALVES, held together before one octet the SENDER chose is judged.
	//
	// Here rather than beside step 6's leaf comparison, because this is the caller's material and
	// not the message's: every refusal below names something about a Welcome, and a caller whose
	// keyring and whose stored key package have parted company would be sent to look at a message
	// nobody tampered with. Step 6 asks whether the TREE's leaf is the one this joiner published;
	// this asks whether the key this group will sign with is the one that leaf names, which no
	// amount of reading the tree can answer.
	//
	// Through subtle for guardrail 8's class reason, which is the class and not this line: a
	// signature key is public, and every comparison in this package that decides whether a
	// structure is this client's is spelled the one way.
	joinerSignatureKey, err := signaturePublicKeyOf(keys.SignPrivate)
	if err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare(joinerSignatureKey, keys.KeyPackage.LeafNode.SignatureKey) != 1 {
		return nil, errJoinerSignatureKeyNotTheLeafs
	}
	// AND THE ENCRYPTION HALF, at the same door, in the same sentence and for the same reason.
	//
	// This half went unchecked, and the reason written down for that -- in the comment at step 6 and
	// in the round that reviewed it -- was that the provider has no private-to-public operation for
	// HPKE. That is a true sentence about CryptoProvider and a false reason: hpkePublicKeyOf is a
	// package level derivation deliberately outside the interface, exactly as signaturePublicKeyOf
	// three lines up is, and both registered suites are DHKEM(X25519) so the public half of the
	// scalar a caller holds is one multiplication away. So the door held the SIGNING half against
	// the published leaf and installed the ENCRYPTION half against nothing.
	joinerEncryptionKey, err := hpkePublicKeyOf(keys.EncryptPrivate)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errJoinerEncryptionKeyNotTheLeafs, err)
	}
	if subtle.ConstantTimeCompare(joinerEncryptionKey, keys.KeyPackage.LeafNode.EncryptionKey) != 1 {
		return nil, errJoinerEncryptionKeyNotTheLeafs
	}
	active := cfg.Profile
	if active == nil {
		active = defaultProfile()
	}

	// step 0: the frame, and the three things the cleartext ciphersuite has to agree with.
	//
	// ParseMLSMessage runs at the DEFAULT vector bound and is not raised here, which is its own
	// comment's decision restated at the one call site that is a stranger's message: this is
	// decoded by a party who is not yet a member, with no group state to check it against and
	// every length in it chosen by whoever sent it.
	message, err := ParseMLSMessage(welcome)
	if err != nil {
		return nil, err
	}
	// THE WIRE FORMAT COMPARISON IS REDUNDANT THROUGH THIS DOOR AND IS KEPT ANYWAY, which is
	// written down because the alternative is a later reader deleting it as noise.
	// (*MLSMessage).UnmarshalMLS refuses a frame carrying any arm its discriminant does not name,
	// so a message ParseMLSMessage accepted cannot have a welcome arm under another wire format --
	// measured, with the comparison deleted the whole of mls and message is green. What it is for
	// is the reading rather than the reachability: this function's contract is "a Welcome", and
	// the arm alone says only that some field of a struct is non nil. A caller inside this package
	// that assembled an MLSMessage rather than decoding one -- which no gate here forbids -- would
	// otherwise reach the joiner's whole derivation with a frame nothing had judged.
	if message.WireFormat != WireFormatWelcome || message.Welcome == nil {
		return nil, fmt.Errorf("%w: it is framed as wire format %#04x",
			errWelcomeWireFormat, uint16(message.WireFormat))
	}
	parsed := message.Welcome
	if parsed.CipherSuite != keys.KeyPackage.CipherSuite {
		return nil, fmt.Errorf("%w: the welcome names %#04x and this joiner's key package names %#04x",
			ErrWelcomeSuiteMismatch, uint16(parsed.CipherSuite), uint16(keys.KeyPackage.CipherSuite))
	}
	if err := active.checkCiphersuiteForCreate(parsed.CipherSuite); err != nil {
		return nil, err
	}
	// and the provider, for NewGroup's reason at the same position: without this the disagreement
	// surfaces two steps later as an AEAD that will not open, which is a refusal naming a group
	// info nobody tampered with.
	if parsed.CipherSuite != crypto.Suite() {
		return nil, fmt.Errorf("%w: the welcome names %#04x and the provider runs %#04x",
			errWelcomeJoinerProviderSuite, uint16(parsed.CipherSuite), uint16(crypto.Suite()))
	}

	// step 1: the entry addressed to the key package this joiner published.
	//
	// Through subtle for guardrail 8's reason, which is the class rather than this line: a key
	// package reference is public, and every comparison in this package that decides whether a
	// structure is ADOPTED is spelled the one way.
	ref, err := keys.KeyPackage.Ref(crypto)
	if err != nil {
		return nil, err
	}
	var entry *EncryptedGroupSecrets
	for i := range parsed.Secrets {
		if subtle.ConstantTimeCompare(parsed.Secrets[i].NewMember, ref) == 1 {
			entry = &parsed.Secrets[i]
			break
		}
	}
	if entry == nil {
		return nil, fmt.Errorf("%w: this welcome carries %d entries and none names %x",
			ErrWelcomeNoMatchingKeyPackage, len(parsed.Secrets), ref)
	}

	// step 2: the group secrets, opened under THIS joiner's init key with THIS Welcome's encrypted
	// group info as the HPKE context.
	//
	// THE HALF OF THE JOINER PAIRING THAT IS NOT A LOOKUP. The step above found the entry that
	// NAMES this key package, which is a field whoever built the Welcome chose; this is the one
	// that cannot be chosen, because the seal was made to the init key that key package published
	// and no other private key opens it. A build where the two disagreed -- BuildWelcome pairing
	// joiner i's reference with joiner j's seal, which is the fork errWelcomeAddPairing exists to
	// refuse on the sending side -- is a refusal here rather than a member that derives an epoch
	// nobody else is in. The context binding is the other half: an entry lifted off a different
	// Welcome no longer opens, because encrypted_group_info is what it was sealed against.
	plaintext, err := OpenWithLabel(crypto, keys.InitPrivate, welcomeHpkeLabel,
		parsed.EncryptedGroupInfo, &entry.EncryptedGroupSecrets)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errWelcomeGroupSecretsDecrypt, err)
	}
	secrets := &GroupSecrets{}
	decodeErr := syntax.Unmarshal(plaintext, secrets)
	// the cleartext GroupSecrets encoding, erased the moment it has been decoded and on the
	// failure path as well -- which is why the erase is written before the error is read. It holds
	// this epoch's joiner secret and this joiner's path secret side by side, and it is a buffer
	// this function produced rather than one a caller owns.
	zeroizeSecret(plaintext)
	if decodeErr != nil {
		return nil, decodeErr
	}
	// and the two secrets the decode lifted out of it, on every path out of this function. Both
	// are COPIED into the values that outlive the call -- NewKeyScheduleFromJoiner clones the
	// joiner secret and DerivePathSecrets clones the rung it starts from -- so what stands here
	// afterwards is a second live copy of the epoch, held by nothing that erases it.
	defer func() {
		zeroizeSecret(secrets.JoinerSecret)
		if secrets.PathSecret != nil {
			zeroizeSecret(secrets.PathSecret.PathSecret)
		}
	}()
	if len(secrets.Psks) != 0 {
		return nil, fmt.Errorf("%w: this welcome names %d pre-shared key(s)",
			errProfilePsk, len(secrets.Psks))
	}

	// step 3: the group info, opened under welcome_key/welcome_nonce with EMPTY AAD.
	//
	// welcome_secret is DeriveSecret(Extract(joiner_secret, psk_secret), "welcome"), and this is
	// the one derivation a joiner has to make outside the key schedule: NewKeyScheduleFromJoiner
	// needs the group context, and the group context is inside the thing this key opens. Extract
	// takes (salt, ikm) through the provider and the joiner secret is the SALT -- guardrail 1, and
	// the transposition compiles and answers a secret exactly as well formed as the right one. The
	// two derivations are held together twelve statements below rather than trusted to agree.
	//
	// The EMPTY AAD is transcribed from RFC 9420 section 12.4.3.1 and is not an omission: an
	// implementation that fed the group context instead seals and opens against itself perfectly
	// and interoperates with nothing.
	memberSecret := crypto.Extract(secrets.JoinerSecret, EmptyPskSecret(crypto))
	welcomeSecret := crypto.DeriveSecret(memberSecret, "welcome")
	zeroizeSecret(memberSecret)
	defer zeroizeSecret(welcomeSecret)
	key, nonce, err := WelcomeKeyNonce(crypto, welcomeSecret)
	if err != nil {
		return nil, err
	}
	infoBytes, err := crypto.AeadOpen(key, nonce, nil, parsed.EncryptedGroupInfo)
	// erased whether or not the open succeeded, for (*RatchetTree).DecryptUpdatePath's reason: the
	// return path that would skip the erase is the one an error takes.
	zeroizeSecret(key)
	zeroizeSecret(nonce)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrWelcomeGroupInfoDecrypt, err)
	}
	info := &GroupInfo{}
	if err := syntax.Unmarshal(infoBytes, info); err != nil {
		return nil, err
	}

	// step 4: the tree, and whatever authority the group info has over it.
	//
	// VerifiedContext AND NOT Verify FOLLOWED BY A TREE HASH COMPARISON OF THIS FUNCTION'S OWN.
	// Verify's rule 4 already compares the tree's hash against the group context's tree_hash, and
	// a second comparison here would be a second reading of one question -- the dual
	// representation this package has been rebuilt twice to remove -- which agrees with the first
	// for exactly as long as the two happen to be spelled the same way. The door that answers it
	// is also the door that answers the signature, the version, the suite, the signer bound, the
	// blank signer leaf and the carried ratchet_tree, so a rule added there is added here by
	// existing. What it costs is that this function inherits every gap Verify has, and the biggest
	// of them is the one the header above is entirely about.
	//
	// It answers the VERIFIED context as well, which is what the proposal cache is bound to below:
	// there is exactly one door onto a VerifiedGroupContext and this is a caller of it, rather
	// than a second construction of the same authority.
	tree, err := UnmarshalRatchetTree(ratchetTree)
	if err != nil {
		return nil, err
	}
	verified, err := info.VerifiedContext(crypto, tree)
	if err != nil {
		return nil, err
	}
	// the caller's statement of which group it meant to join, matched and never trusted. See the
	// header: this refuses a joiner being placed in a group it did not ask for, and refuses
	// nothing at all to an attacker who names the id its target expects.
	if len(cfg.GroupId) != 0 &&
		subtle.ConstantTimeCompare(cfg.GroupId, info.GroupContext.GroupId) != 1 {
		return nil, fmt.Errorf("%w: this client asked to join %x and this welcome describes %x",
			errWelcomeGroupIdNotTheOneAsked, cfg.GroupId, info.GroupContext.GroupId)
	}

	// step 5: the tree is a tree of this group at this epoch, and the group is one this profile
	// runs. ValidateAgainstContext is where every leaf signature and every parent hash is checked;
	// without it the group info above has been verified against a leaf of a tree nothing judged.
	requiredCaps, err := requiredCapabilitiesOf(info.GroupContext.Extensions)
	if err != nil {
		return nil, err
	}
	treeCtx := &TreeValidationContext{
		Crypto:          crypto,
		Suite:           info.GroupContext.CipherSuite,
		GroupId:         info.GroupContext.GroupId,
		RequiredCaps:    requiredCaps,
		GroupExtensions: info.GroupContext.Extensions,
		// clamped to one millisecond for (*KeyPackage).Validate's reason: a NowMs of zero is
		// LeafValidationContext's documented opt out of the lifetime check, so a machine whose
		// clock is not set must not turn the one untrustworthy input into the one that disables
		// the check it exists for.
		NowMs:       uint64(max(time.Now().UnixMilli(), 1)),
		ClockSkewMs: leafLifetimeSkewSeconds * 1000,
	}
	if err := tree.ValidateAgainstContext(treeCtx, &info.GroupContext); err != nil {
		return nil, err
	}
	for i := range info.GroupContext.Extensions {
		if err := active.checkGroupExtension(info.GroupContext.Extensions[i].ExtensionType); err != nil {
			return nil, err
		}
	}
	// a group this profile runs carries a policy, and it is asked for here rather than at the
	// first message that needed a role -- which is (*Group).Members, whose answer degrades to
	// RoleMember for every leaf when the policy will not parse.
	if _, err := GroupPolicyOf(info.GroupContext.Extensions); err != nil {
		return nil, err
	}

	// step 6: this joiner's own leaf.
	//
	// FOUND BY SIGNATURE KEY AND THEN HELD TO THE WHOLE LEAF NODE. The lookup alone says only that
	// some leaf carries this device's signature key, and a tree is not known to be free of
	// duplicate signature keys at this point -- FindLeafBySignatureKey's own comment says so, and
	// picks the lowest for that reason. What the joiner actually needs is the leaf the committer
	// installed FROM ITS KEY PACKAGE, because that is the leaf whose encryption key it holds the
	// private half of and the leaf every later signature of its own will be attributed to. A tree
	// carrying this joiner's signature key at a leaf with somebody else's encryption key passes
	// the lookup, passes (*TreeKEMPrivate).Consistent -- which judges the PATH SECRETS and
	// deliberately does not re-derive the leaf public key; see its own comment -- and produces a
	// member that decrypts nothing, with no refusal anywhere to point at. The joiner's OWN two
	// halves are held together at the top of this function, through signaturePublicKeyOf and
	// hpkePublicKeyOf; what this step adds is that the leaf the TREE carries is the one those two
	// halves belong to.
	ownLeaf, found := tree.FindLeafBySignatureKey(keys.KeyPackage.LeafNode.SignatureKey)
	if !found {
		return nil, fmt.Errorf("%w: no leaf of this tree carries this joiner's signature key",
			ErrWelcomeLeafNotFound)
	}
	if err := leafIsTheOneThisJoinerPublished(tree.Leaf(ownLeaf), &keys.KeyPackage.LeafNode); err != nil {
		return nil, fmt.Errorf("%w: at leaf %d", err, ownLeaf)
	}

	// step 7: the key schedule of the epoch this joiner is entering.
	schedule, err := NewKeyScheduleFromJoiner(crypto, secrets.JoinerSecret,
		EmptyPskSecret(crypto), &info.GroupContext)
	if err != nil {
		return nil, err
	}
	ownPriv := NewTreeKEMPrivate(ownLeaf, keys.EncryptPrivate)
	var secretTree *SecretTree
	// EVERY REFUSAL FROM HERE ON DROPS A FULLY DERIVED EPOCH, and an epoch that is dropped is
	// erased. The schedule holds the init secret, the confirmation key, the encryption secret, the
	// epoch authenticator, the exporter and the resumption PSK; the private state holds this
	// device's leaf key and every path secret laddered above it. After the return nothing in this
	// process can reach any of it, and unreachable is not erased. handedOn is what tells a refusal
	// from the one path that gives all three to a Group that owes them its own Close.
	handedOn := false
	defer func() {
		if handedOn {
			return
		}
		schedule.Zeroize()
		if secretTree != nil {
			secretTree.Zeroize()
		}
		ownPriv.Zeroize()
	}()

	// the welcome secret this function derived in step 3 and the one the schedule derives from the
	// same joiner secret are the SAME value by definition, and they are compared rather than
	// assumed to be. They are two transcriptions of DeriveSecret(Extract(joiner_secret,
	// psk_secret), "welcome") in two files, which is the shape guardrail 1 is about: a transposed
	// Extract in either of them answers KDF.Nh well formed octets, and every test that does not
	// hold the two against each other passes. This is a build disagreeing with itself rather than
	// anything a peer did -- the same kind of refusal errCreationConfirmationTag makes in NewGroup
	// -- and the alternative to making it is a joiner that opened the group info with one epoch's
	// key and runs on another.
	if subtle.ConstantTimeCompare(welcomeSecret, schedule.WelcomeSecret()) != 1 {
		return nil, errJoinerWelcomeSecret
	}
	// ValSem205 from the joining side: the tag the group info carries is the one THIS joiner's
	// confirmation key produces over the confirmed transcript hash THIS group info names. It is
	// what turns "the sender described an epoch" into "the epoch I derived is that epoch", and it
	// is the only check here that reaches the joiner secret at all. Through
	// (*KeySchedule).VerifyConfirmationTag and therefore through CryptoProvider.MacVerify and
	// crypto/subtle -- guardrail 8 -- which is also what refuses a truncated tag rather than
	// comparing as much of it as fits.
	if !schedule.VerifyConfirmationTag(info.GroupContext.ConfirmedTranscriptHash, info.ConfirmationTag) {
		return nil, fmt.Errorf("%w: this welcome describes an epoch this joiner does not derive",
			errBadConfirmationTag)
	}
	transcript := InitialTranscriptHashes()
	if err := transcript.SetFromGroupInfo(crypto,
		info.GroupContext.ConfirmedTranscriptHash, info.ConfirmationTag); err != nil {
		return nil, err
	}

	// step 8: the private tree state -- this device's leaf key, and the ladder above the node it
	// shares with the sender.
	//
	// Consistent re-derives each held secret's node key pair and requires the derived public key to
	// be the one the tree already carries at that node, RFC 9420 section 12.4.3.1: "The private key
	// MUST be the private key that corresponds to the public key in the node." It runs whether or
	// not a path secret arrived, because a state holding nothing but a leaf index still asserts
	// that the tree has a leaf there.
	if secrets.PathSecret != nil {
		if err := tree.installJoinerPathSecrets(crypto, ownPriv, ownLeaf, info.Signer,
			secrets.PathSecret.PathSecret); err != nil {
			return nil, err
		}
	}
	if err := ownPriv.Consistent(crypto, tree); err != nil {
		return nil, err
	}

	secretTree, err = NewSecretTree(crypto, tree.LeafWidth(), schedule.Secrets().Encryption)
	if err != nil {
		return nil, err
	}
	proposals, err := NewProposalCache(verified)
	if err != nil {
		return nil, err
	}

	group := &Group{
		// THE STORE AND NOT THE CONFIG, which is Group's own field comment: a config is a caller's
		// structure and the caller goes on holding it, and every other field this join needs comes
		// out of the group info rather than out of the config. The group id, the extensions and
		// the suite this group runs are the EPOCH's, and a join that installed the caller's copies
		// of them would be a member whose published context and whose epoch secrets can part
		// company.
		store:  cfg.Store,
		crypto: crypto,
		// both COPIED, for NewGroup's reason: they are arrays the caller owns and goes on using,
		// and this group signs with the one and names itself with the other for the whole of its
		// life.
		signer: SignaturePrivateKey(cloneBytes(keys.SignPrivate)),
		cred: Credential{
			CredentialType: keys.KeyPackage.LeafNode.Credential.CredentialType,
			Identity:       cloneBytes(keys.KeyPackage.LeafNode.Credential.Identity),
		},
		ownLeaf:     ownLeaf,
		ownPriv:     ownPriv,
		tree:        tree,
		context:     &info.GroupContext,
		verified:    verified,
		schedule:    schedule,
		secretTree:  secretTree,
		transcript:  transcript,
		proposals:   proposals,
		restoreKind: restoreFromJoiner,
	}
	if err := group.persist(); err != nil {
		return nil, err
	}
	handedOn = true
	return group, nil
}

// leafIsTheOneThisJoinerPublished compares the leaf standing in the tree against the leaf this
// joiner's key package published, as ENCODINGS.
//
// The encoding and not a field by field comparison, for the reason (*GroupInfo).Verify compares
// tree hashes rather than tree encodings, read the other way round: what is being asked is whether
// these are the same leaf node, and a comparison written over the fields somebody listed answers
// that question about the fields somebody listed. A leaf that grew a tenth field would be compared
// over nine of them, silently, and the encoding is the one reading that cannot fall behind the
// struct. Through subtle for the class reason guardrail 8 states.
func leafIsTheOneThisJoinerPublished(inTree *LeafNode, published *LeafNode) error {
	// a blank leaf is refused as an absent leaf and not as a mismatched one: FindLeafBySignatureKey
	// skips blanks, so this arm is reachable only from a tree that changed under us, and a caller
	// told "your leaf is not the one you published" would go looking at its key package.
	if inTree == nil {
		return fmt.Errorf("%w: the tree holds no leaf where this joiner's signature key was found",
			ErrWelcomeLeafNotFound)
	}
	here, err := syntax.Marshal(inTree)
	if err != nil {
		return err
	}
	mine, err := syntax.Marshal(published)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(here, mine) != 1 {
		return errWelcomeLeafNotTheJoiners
	}
	return nil
}

// installJoinerPathSecrets seeds the joiner's ladder from the one rung its Welcome carried.
//
// The Welcome carries the secret for the LOWEST NODE THE JOINER'S LEAF AND THE SENDER'S LEAF SHARE
// and for no other node, RFC 9420 section 12.4.3.1 -- it is also exactly the node the joiner is not
// sealed to in the UpdatePath itself, because a member added by the same commit holds no key yet.
// Every node above it is one DeriveSecret(., "path") away, which is what DerivePathSecrets returns
// as a chain.
//
// THE WALK IS OVER THE SENDER'S FILTERED DIRECT PATH AND NOT OVER THE JOINER'S UNFILTERED ONE, and
// the two are different node lists. The ladder the sender built runs over its own FILTERED path --
// CreateUpdatePathSecrets derives one rung per node of it, and (*RatchetTree).DecryptUpdatePath
// walks that same list from the shared node upward on the receiving side -- so a joiner laddering
// over its unfiltered direct path would place the sender's rung k at the tree's node k+j for
// whatever j the filter dropped, and every node from the first dropped one upward would hold a
// secret that derives a key pair nothing in the tree matches. (*TreeKEMPrivate).Consistent refuses
// that, so the symptom is a joiner that cannot join rather than one that joins wrongly; the reason
// it is written here is that the two paths COINCIDE in the shapes a small fixture produces, and a
// round trip through a two or three member group cannot tell them apart.
//
// Above the shared node the sender's filtered path and the joiner's are the same list, which is why
// walking the sender's is walking the joiner's: for a node above the common ancestor both leaves
// sit in the same child subtree, so the copath child the filter tests is the same node for both.
//
// The refusal for a shared node that is not on the sender's path is not the ordinary case -- it is
// (*RatchetTree).DecryptUpdatePath's ErrNoPathSecret condition read from the other end -- but here
// it means a Welcome that handed this joiner a path secret for an epoch whose update path never
// covered it, which is a message to refuse rather than a state to build.
// A METHOD ON THE TREE, where (*RatchetTree).EncryptUpdatePath, MergeUpdatePath and
// DecryptUpdatePath are: every fact it reads is the tree's -- the sender's filtered direct path and
// the node the two leaves share -- and the private state it writes into is a caller's, exactly as
// it is for the decrypt half.
func (self *RatchetTree) installJoinerPathSecrets(crypto CryptoProvider, priv *TreeKEMPrivate,
	own LeafIndex, signer LeafIndex, pathSecret []byte) error {

	// the provider first, before the tree is read: every rung of the ladder below is a
	// DeriveSecret through it and the width check reads KDF.Nh off it, so a caller that passed
	// none passed nothing this method could have used. Bare rather than wrapped, which is
	// DeriveNodeKeyPair's spelling for the same condition.
	if crypto == nil {
		return ErrNilCryptoProvider
	}
	steps, err := self.filteredPathSteps(signer)
	if err != nil {
		return err
	}
	lowest := CommonAncestor(signer.NodeIndex(), own.NodeIndex())
	start, onThePath := indexOfStep(steps, lowest)
	if !onThePath {
		return fmt.Errorf("%w: node %d is the lowest node leaf %d and leaf %d share, and it is not on leaf %d's filtered direct path",
			errWelcomePathSecretNode, lowest, own, signer, signer)
	}
	// the width, checked before the ladder for (*RatchetTree).DecryptUpdatePath's reason at the
	// same position: everything below this line agrees with a sender that agrees with itself, so
	// this is the last place a wrong width is a statement about a message rather than about an
	// epoch. It is a length and carries no secret, so the comparison is a plain one.
	if len(pathSecret) != crypto.HashSize() {
		return fmt.Errorf("%w: this welcome carries a path secret of %d octets, want %d",
			errPathSecretLength, len(pathSecret), crypto.HashSize())
	}
	// one secret per node from the shared node to the root, which is len(steps)-start of them.
	// DerivePathSecrets answers count+1 rungs -- the extra one is the rung past the root that
	// section 8.1 makes the commit secret, which a joiner has no use for -- so the count is one
	// less than the number of nodes. Asking for len(steps)-start here indexes one past the end of
	// the path.
	chain := DerivePathSecrets(crypto, pathSecret, len(steps)-start-1)
	for i, secret := range chain {
		priv.PathSecrets[steps[start+i].Node] = secret
	}
	return nil
}

// ---------------------------------------------------------------------------
// p7 task 18: RFC 9420 section 12.4.2, the receive half of a commit
// ---------------------------------------------------------------------------

// The refusals ingesting a message makes that are nobody else's, one value per rule for the reason
// this file's first error block states.
var (
	// errProcessWireFormat is an MLSMessage this profile does not process on a group's inbound
	// path. It is a PROFILE refusal and not a codec one: the codec accepts all five wire formats
	// -- a Welcome and a KeyPackage are perfectly well formed messages -- and what refuses them
	// here is that neither is something a group at an epoch ingests. Spec A section 3.4 sends
	// every handshake message as a PrivateMessage, so PublicMessage is refused too and the
	// receive path has one shape rather than two.
	errProcessWireFormat = errors.New("mls: this profile does not process that wire format on a group's inbound path")

	// errProcessSenderType is a framed content from a sender type this build has no key for. It
	// is NOT errProfileExternalSender, which is the external_senders GROUP CONTEXT EXTENSION
	// refused at a proposal door: that one is a group whose extensions this profile will not
	// carry, and this one is a message whose signer this member cannot name. A caller sent to
	// look at its group's extensions over a message from an external sender would find nothing
	// wrong with them.
	errProcessSenderType = errors.New("mls: the v1 profile processes member senders only")

	// errProcessContentType is a framed content whose content_type is outside RFC 9420 section
	// 6's three. The codec refuses an undefined code point, so what could reach this is a type
	// this select has not been taught, and answering it is what keeps a fourth arm from being a
	// silent fall through to nil.
	errProcessContentType = errors.New("mls: framed content of a type this receive path does not process")

	// errCommitContentCarriesNoCommit is a content_type of commit with no commit inside it. The
	// codec pairs the two, so this is this build disagreeing with itself rather than anything a
	// peer did -- and it is stated rather than dereferenced because the alternative is a nil
	// dereference inside the receive path of a library.
	errCommitContentCarriesNoCommit = errors.New("mls: a commit message carries no commit")

	// errApplyCommitNotACommit is ApplyCommit handed something ProcessMessage did not answer as a
	// commit. It is a caller's mistake and not a message fault, which is why it is not one of the
	// three above.
	errApplyCommitNotACommit = errors.New("mls: ApplyCommit was handed a result that is not a staged commit")

	// errApplyCommitNotThisGroups is a staged commit ANOTHER GROUP derived, handed to this one.
	//
	// It is a caller mistake in the same sense the value above is -- Processed and its Commit
	// field are exported and connect/message holds them across a policy decision, so two groups'
	// results are two values of one type in one caller's hands -- and it is the refusal that keeps
	// the mistake from being silent. MEASURED, with the two refusals below deleted: group B, given
	// a Processed group A had staged, answered nil, left epoch 1 for the epoch A's commit opened,
	// and then answered an epoch authenticator byte-identical to A's -- two groups with different
	// ids and no member in common, running one epoch, because nothing in this body read the
	// commit's provenance at all.
	errApplyCommitNotThisGroups = errors.New("mls: ApplyCommit was handed a commit another group staged")

	// errApplyCommitNotThisEpochs is a staged commit of THIS group derived against an epoch this
	// group is no longer in -- one already merged and handed back, or one staged against an epoch
	// the group has not reached.
	//
	// Separate from the value above rather than folded into it, because the two send a reader to
	// different places: a group id mismatch is a caller that crossed two groups' results, and an
	// epoch mismatch is a caller that kept one group's result across a boundary it already applied.
	errApplyCommitNotThisEpochs = errors.New("mls: ApplyCommit was handed a commit staged against another epoch")

	// errUpdatedLeafPrivateKey is a commit that installs an Update at THIS client's own leaf whose
	// encryption key this client cannot produce the private half of.
	//
	// It is the receiving end of the order (*Group).ProposeUpdate files its key pair in: the
	// private half goes into the store BEFORE the proposal is published, precisely so that a
	// committer acting entirely correctly on what it was sent cannot commit this client into an
	// epoch whose own leaf key it does not hold. This is the value for the case where the store
	// nonetheless has no answer -- a store that lost the entry, or a leaf updated by somebody
	// else, which ValSem111 forbids and this refuses rather than assumes.
	//
	// A REFUSAL AND NOT A CARRY-FORWARD, which is the whole reason it exists. The alternative is
	// keeping the leaf key of the epoch that just closed, and that produces a member which merges
	// the commit, enters the new epoch, and then decrypts NOTHING for the rest of the group's life
	// -- every update path sealed to its published key, opened with a key that is not its private
	// half, reported at the far end as a corrupt commit.
	errUpdatedLeafPrivateKey = errors.New(
		"mls: this commit updates this client's own leaf and its private half is not in the store")
)

// checkWireFormat is the v1 disposition of the wire formats a GROUP INGESTS, standing in for
// (*Profile).CheckWireFormat exactly as checkCiphersuiteForCreate stands in for
// (*Profile).CheckCiphersuiteForCreate; see proposal_list.go's profile for why the stand-in is
// unexported and what the swap costs.
//
// PrivateMessage AND NOTHING ELSE, and the refusals are two different sentences. A Welcome, a
// GroupInfo and a KeyPackage are messages a group at an epoch does not ingest at all --
// JoinFromWelcome takes the first two and the directory takes the third -- and a PublicMessage is
// a handshake message this profile does not send, because spec A section 3.4 has handshake traffic
// travel as a PrivateMessage so that the delivery service learns neither who committed nor what
// the commit did. A build that accepted one would let a peer choose which of the two
// authenticators its message is judged under.
//
// THERE IS NO checkVersion BESIDE IT, and that is a decision rather than the omission it looks
// like. (*MLSMessage).UnmarshalMLS refuses every ProtocolVersion but mls10 INLINE, before it
// selects an arm, so a second version gate here could not be reached by any octets this package
// can parse -- and a guard no input can fire is the shape (*Group).RatchetTree and
// (*Group).ProposeGroupContextExtensions both reject: a second copy covering for the one really
// doing the work, which no test can tell apart from the real answer. When p8's Profile makes the
// version a profile decision, the door it needs is the decoder's rather than one written here.
func (self *profile) checkWireFormat(format WireFormat) error {
	if format == WireFormatPrivateMessage {
		return nil
	}
	return fmt.Errorf("%w: wire format %d", errProcessWireFormat, format)
}

// ProcessedKind discriminates what ProcessMessage returned.
type ProcessedKind uint8

const (
	ProcessedApplication ProcessedKind = 1
	ProcessedProposal    ProcessedKind = 2
	ProcessedCommit      ProcessedKind = 3
)

// ApplicationMessage is one decrypted application message, as storage the caller owns: every field
// of it was cut out of a plaintext this call decrypted, and nothing in this package retains it.
type ApplicationMessage struct {
	SenderLeaf        LeafIndex
	AuthenticatedData []byte
	Plaintext         []byte
	// The RFC 9420 section 6.3.2 SenderData.generation this frame was sealed at.
	//
	// It is here because MASTER section 8.4.2 v2 puts it inside aad_mls and section 8.4.3's R2
	// is therefore a function of it, so the record layer above has to be able to take that
	// refusal a second time on what the open AUTHENTICATED rather than only on what the peek
	// read. The value reported is the one the content AEAD's key was derived from, so a frame
	// that opened at all opened at exactly this generation -- which is why section 8.4.3 names
	// the generation half of that second reading as the clause predicted to defend nothing, and
	// requires the pass that builds it to delete it, measure, and say so.
	Generation uint32
}

// Processed is the result of ingesting one MLSMessage.
//
// Exactly one of the three arms is populated and Kind says which, which is MLSMessage's own select
// discipline one layer up: a caller reading the wrong arm of a value with two populated would be
// acting on a message nobody sent.
type Processed struct {
	Kind        ProcessedKind
	Sender      Sender
	Application *ApplicationMessage
	Proposal    *Proposal
	Commit      *StagedCommit
}

// ProcessMessage ingests one MLSMessage.
//
// IT NEVER MUTATES LIVE EPOCH STATE. A commit comes back STAGED, so the caller can run its own
// policy and record the epoch's wraps before the epoch advances -- connect/message needs that gap,
// and a receive path that advanced the epoch as it validated would leave a caller that refused the
// commit already inside it. (*Group).ApplyCommit is the second half.
//
// TWO THINGS DO MOVE, and both are meant to, exactly as they are on the proposal generation path
// one section up. An inbound PROPOSAL is cached, because that is what makes a later commit able to
// name it by reference; and the message ratchet of the SENDER's leaf advances a generation, which
// is what opening a PrivateMessage is.
//
// The order is RFC 9420 section 6's and it is not interchangeable: the message is parsed, the
// profile judges the envelope, the framing layer opens and authenticates it, and only then is the
// content read. A body that branched on the content type before the open would be branching on an
// attacker's octets.
func (self *Group) ProcessMessage(message []byte) (*Processed, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		return nil, errGroupClosed
	}

	parsed, err := ParseMLSMessage(message)
	if err != nil {
		return nil, err
	}
	if err := defaultProfile().checkWireFormat(parsed.WireFormat); err != nil {
		return nil, err
	}
	// THERE IS NO ARM CHECK BESIDE IT, and its absence is a decision rather than the omission it
	// looks like -- the same one stageInboundCommitLocked makes about the confirmation tag.
	// (*MLSMessage).UnmarshalMLS populates the arm its wire format names and refuses a message
	// carrying any other, so behind the gate above parsed.PrivateMessage is non-nil for every
	// message this package can parse. A guard here would answer the SAME sentinel that gate does,
	// which is worse than redundant: no input could tell the two apart, so a profile that had
	// stopped refusing a Welcome would go on reading as one that refuses it. MEASURED, on the
	// mutation that says so -- with checkWireFormat accepting every format, the arm check kept the
	// whole suite green and TestProcessMessageRefusesEveryWireFormatButPrivateMessage passed on the
	// wrong refusal.
	//
	// The direction is unchanged: OpenPrivateMessage refuses a nil message with errNilPrivateMessage
	// before it reads anything off it, so a build that somehow reached this line without one fails
	// closed rather than dereferencing.

	groupContext, err := syntax.Marshal(self.context)
	if err != nil {
		return nil, err
	}

	// the GENERATION comes back beside the authenticated content, and only the application arm
	// carries it onward. A proposal and a commit are judged by the transcript and the epoch rather
	// than by a position in a record stream, so there is nothing above this layer that would read
	// it for either.
	authenticated, generation, err := openPrivateMessageAt(self.crypto, self.secretTree,
		self.senderDataSecretLocked(), parsed.PrivateMessage,
		self.signatureKeyResolverLocked(), groupContext)
	if err != nil {
		return nil, err
	}
	return self.processAuthenticatedLocked(authenticated, generation)
}

// signatureKeyResolverLocked is what turns a Sender into the key the framing layer verifies
// against, and it is where ValSem004 and this profile's sender rule are enforced: an external
// sender or a blank leaf never yields a key, so a message from either is refused at the signature
// rather than further in, where its leaf index would already have been used for something.
//
// A DECLARATION AND NOT A CLOSURE INSIDE ProcessMessage, and the difference is what a test can
// reach. OpenPrivateMessage constructs the Sender it resolves -- section 6.3's sender data carries
// a leaf index and nothing else, so every sender that reaches this through the one wire format this
// profile ingests is a member sender. The non-member arm is therefore a refusal no octets can fire,
// and written as a lambda it would be a refusal nothing in this package has ever read, which is
// exactly what validation_framing_test.go's two refusal gates exist to report. It is kept rather
// than deleted because it is the fail-CLOSED half: CheckSenderLeaf answers nil for every non-member
// sender by design, so a body without this arm would fall through to leaf 0's key for a sender type
// that carries no leaf index at all.
func (self *Group) signatureKeyResolverLocked() SignatureKeyResolver {
	return func(sender Sender) (SignaturePublicKey, error) {
		if sender.SenderType != SenderTypeMember {
			return nil, fmt.Errorf("%w: sender type %d", errProcessSenderType, sender.SenderType)
		}
		// ValSem004, through the framing plan's own door rather than through a second occupancy
		// test written here, so the sender side and the receiver side cannot come to disagree
		// about what an occupied leaf is.
		if err := CheckSenderLeaf(sender, func(leaf LeafIndex) bool {
			return self.tree.Leaf(leaf) != nil
		}); err != nil {
			return nil, err
		}
		leaf := self.tree.Leaf(sender.LeafIndex)
		if leaf == nil {
			return nil, fmt.Errorf("%w: leaf %d", errBlankSenderLeaf, sender.LeafIndex)
		}
		// a COPY of the key and not the tree's own array. SignaturePublicKey is a []byte, so what
		// this hands the framing layer would otherwise be a window onto the live ratchet tree --
		// the same hazard (*Group).Members answers a clone for, and it costs 32 octets a message.
		return SignaturePublicKey(cloneBytes(leaf.SignatureKey)), nil
	}
}

// processAuthenticatedLocked is the content half of ProcessMessage: RFC 9420 section 6's context
// rules over a framed content that has been opened and authenticated, and the select over its three
// content types.
//
// A DECLARATION AND NOT A BLOCK, for signatureKeyResolverLocked's reason one door up. The codec
// refuses every content_type outside the registry, so the arm that answers an undefined one cannot
// be reached through ProcessMessage by any octets at all -- and a refusal nothing has ever read is
// the shape framing_test.go's code point gate reports. Written as a declaration it is reachable
// with a content this package's own encoder cannot produce, which is what makes the message a
// caller would see something a test has actually looked at.
//
// The compiler directive is this package's convention for a member of the class
// TestEveryEraseHelperCarriesTheNoInlineDirective derives, and not a claim that this body erases a
// secret itself. That class is closed under the hand-off -- a declaration that gives storage it was
// handed to an eraser is a member -- and this one hands its argument to (*ProposalCache).Store and
// to stageInboundCommitLocked, both of which are in it. (*ProposalCache).Store carries the
// directive under exactly this reading and says so, which is the point of deriving a class rather
// than listing one.
//
//go:noinline
func (self *Group) processAuthenticatedLocked(authenticated *AuthenticatedContent,
	generation uint32) (*Processed, error) {
	// ValSem002 and ValSem003, through the framing plan's one check so that the sender side and
	// the receiver side cannot disagree about what "this group, this epoch" means. It runs AFTER
	// the open because a PrivateMessage has no framed content until it has been decrypted.
	if err := CheckFramedContentContext(&authenticated.Content,
		self.context.GroupId, self.context.Epoch); err != nil {
		return nil, err
	}
	sender := authenticated.Content.Sender

	switch authenticated.Content.ContentType {
	case ContentTypeApplication:
		return &Processed{
			Kind:   ProcessedApplication,
			Sender: sender,
			Application: &ApplicationMessage{
				SenderLeaf:        sender.LeafIndex,
				AuthenticatedData: authenticated.Content.AuthenticatedData,
				Plaintext:         authenticated.Content.ApplicationData,
				Generation:        generation,
			},
		}, nil
	case ContentTypeProposal:
		// stored through the cache rather than kept here, because the cache is what re-runs the
		// profile gate, checks the epoch binding and applies section 12.2's per sender and per
		// leaf ceilings. The entry it keeps is a CLONE of the proposal, so what is answered below
		// shares no storage with what a later commit resolves.
		if _, err := self.proposals.Store(self.crypto, self.context, authenticated); err != nil {
			return nil, err
		}
		return &Processed{Kind: ProcessedProposal, Sender: sender,
			Proposal: authenticated.Content.Proposal}, nil
	case ContentTypeCommit:
		staged, err := self.stageInboundCommitLocked(authenticated)
		if err != nil {
			return nil, err
		}
		return &Processed{Kind: ProcessedCommit, Sender: sender, Commit: staged}, nil
	}
	return nil, fmt.Errorf("%w: content type %d", errProcessContentType, authenticated.Content.ContentType)
}

// stageInboundCommitLocked is the receive half of RFC 9420 section 12.4.2, in the order the RFC
// lists the steps, with the state lock already held.
//
// THE ORDER IS NORMATIVE AND EVERY STEP DEPENDS ON THE ONE BEFORE IT, which is why it is numbered
// in the body against the RFC's own list rather than left to be read off the lines:
//
//   - the proposals are resolved and applied BEFORE the commit is judged, because every rule of
//     section 12.4 is stated over the tree the proposals build;
//   - the commit is judged BEFORE the update path is merged, because ValSem206 and ValSem207 ask
//     that no key the path publishes already stands in that tree -- and after the merge every one
//     of them does. CheckUpdatePathKeyUniqueness says so in as many words, and a validator handed
//     the merged tree refuses every honest commit of a group with more than one member;
//   - the path is merged BEFORE it is decrypted, because the ciphertexts were sealed under a group
//     context whose tree_hash covers the path's own public keys;
//   - the key schedule is advanced BEFORE the confirmation tag is checked, because the
//     confirmation key belongs to the epoch this commit OPENS;
//   - and the tag is checked against the NEW confirmed transcript hash. A tag checked against the
//     transcript of the epoch that is closing still produces KDF.Nh octets and still compares equal
//     on both sides of an honest exchange, so nothing that round trips can land on that fault.
//
// NOTHING HERE WRITES A FIELD OF THE GROUP. ApplyProposals clones the tree, DecryptUpdatePath is
// handed a Clone of this member's private tree state, and the transcript is a Clone -- so a commit
// refused at any of the returns below leaves this group exactly where the epoch it is still in put
// it.
func (self *Group) stageInboundCommitLocked(authenticated *AuthenticatedContent) (*StagedCommit, error) {
	commit := authenticated.Content.Commit
	if commit == nil {
		return nil, errCommitContentCarriesNoCommit
	}
	committer := authenticated.Content.Sender.LeafIndex
	// THERE IS NO PRESENCE CHECK ON THE CONFIRMATION TAG HERE, and its absence is a decision rather
	// than the omission it looks like. ValSem009 is already stated twice on the way in and once more
	// on the way through: (*FramedContentAuthData).UnmarshalMLS refuses an empty tag as it decodes,
	// VerifyAuthenticatedContent refuses a commit carrying none immediately after the signature, and
	// ValSem205ConfirmationTag refuses one at step 7 under the same sentinel. A fourth copy could not
	// be reached by any octets this package can parse, which is the shape (*Group).RatchetTree and
	// (*Group).ProposeGroupContextExtensions both reject -- a guard covering for the one really doing
	// the work, that no input can tell apart from the real answer. The direction is unchanged: a
	// commit that somehow reached this body with no tag is refused at step 7 rather than accepted.

	// step 1: resolve the proposals this commit names, against THIS member's cache and THIS
	// member's epoch. Erratum 8815 -- "a reference to a proposal that was not previously received"
	// -- is the same rule and the same value, asked here by Resolve rather than restated;
	// ValidateCommit reaches it again through validateCommitErrata, over the cache this step read.
	list, err := self.proposals.Resolve(self.crypto, self.context, committer, commit.Proposals)
	if err != nil {
		return nil, err
	}

	// step 2: apply the list to a tree of this call's own, and judge it against the PRE-commit
	// state, which is the state section 12.2's rules are stated over.
	applied, err := ApplyProposals(self.tree, self.context, self.ownLeaf, list)
	if err != nil {
		return nil, err
	}
	if err := ValidateProposalList(&ProposalValidationInput{
		Crypto:     self.crypto,
		Tree:       self.tree,
		Context:    self.context,
		Extensions: applied.Extensions,
		Committer:  committer,
		List:       list,
		Now:        time.Now(),
	}); err != nil {
		return nil, err
	}

	// step 3: the commit that ejects this client, answered before anything is derived.
	//
	// A REMOVED MEMBER CANNOT DERIVE THE EPOCH AND MUST NOT PRETEND TO, and that is a fact about
	// the protocol rather than a shortcut taken here. Its leaf is blank in the tree the proposals
	// built, so the committer's filtered direct path no longer covers it, no ciphertext of the
	// update path is addressed to it, and there is therefore no commit secret -- which means no
	// key schedule, no confirmation key, and no way to check the confirmation tag. Every remaining
	// rule of section 12.4.2 is a question about an epoch this client does not enter, and
	// ValSem203 answers exactly that: this leaf shares no node with the committer's filtered path.
	//
	// So what is handed back is a REPORT and not an epoch: the committer, the proposals, the
	// leaves that moved, and RemovesSelf. (*Group).ApplyCommit answers ErrRemovedFromGroup for it
	// and closes the group rather than merging anything, and the staged value holds no key
	// material for that close to erase.
	//
	// The commit is not unauthenticated at this point: it opened under this epoch's message keys,
	// its signature verified against the committer's own leaf, and it names this group and this
	// epoch. What is missing is the confirmation tag, and no build can supply that to a member the
	// commit removed.
	if applied.SelfRemoved {
		return &StagedCommit{
			committer:   committer,
			epoch:       self.context.Epoch + 1,
			groupId:     cloneBytes(self.context.GroupId),
			priorEpoch:  self.context.Epoch,
			tree:        applied.Tree,
			list:        list,
			commit:      commit,
			added:       applied.AddedLeaves,
			removed:     applied.RemovedLeaves,
			updated:     applied.UpdatedLeaves,
			selfRemoved: true,
			hasPath:     commit.Path != nil,
			confirmTag:  authenticated.Auth.ConfirmationTag,
		}, nil
	}

	// step 4: section 12.4's own rules, over the post-proposal tree and BEFORE the merge.
	//
	// PostTree IS THE TREE THE PROPOSALS BUILT AND NOT THE ONE THE PATH IS MERGED INTO, which is
	// (*Group).CreateCommit's split at the other end of the same file and is here for the reason
	// CheckUpdatePathKeyUniqueness states: the keys an update path publishes are the keys the merge
	// installs, so a validator handed the merged tree finds every one of them already standing
	// there and refuses ValSem207 on every commit that carries a path.
	//
	// The context is the PRE-commit one and the extensions are the post-proposal set, which is not
	// a muddle and is CreateCommit's pairing for its stated reason: the by-reference arm of the
	// vector join resolves against this member's cache, which is bound to the epoch that is
	// closing, and effectiveExtensions is the field that carries the post-proposal set to the
	// rules that need it.
	commitInput := &CommitValidationInput{
		Crypto:     self.crypto,
		PreTree:    self.tree,
		PostTree:   applied.Tree,
		Context:    self.context,
		Extensions: applied.Extensions,
		Committer:  committer,
		Own:        self.ownLeaf,
		List:       list,
		Commit:     commit,
		Pending:    self.proposals,
		Now:        time.Now(),
	}
	if err := ValidateCommit(commitInput); err != nil {
		return nil, err
	}

	// step 5: this client's own leaf key, if the commit's proposals replaced it.
	//
	// AN UPDATE AT THIS CLIENT'S OWN LEAF IS AN UPDATE THIS CLIENT PUBLISHED -- ValSem111 makes an
	// Update's sender the leaf it updates -- so the private half is one this client drew and filed
	// in its own store before it published the proposal. It has to be installed BEFORE the path is
	// decrypted, because the committer sealed this member's rung of the update path to the key the
	// UPDATE published and not to the one the epoch that is closing carries.
	//
	// MEASURED, which is why it is a step of its own rather than a line inside the path block:
	// without it, a member whose Update another member commits is refused at DecryptUpdatePath on
	// the very commit that carried its own proposal -- and on every commit after that one, because
	// the leaf key it goes on holding is one the group replaced.
	commitSecret := ZeroSecret(self.crypto)
	ownPriv := self.ownPriv
	replaced, err := self.updatedOwnLeafPrivateLocked(applied)
	if err != nil {
		return nil, err
	}
	if replaced != nil {
		// erased when this call returns, whatever it returns: every path below takes a CLONE of
		// this state or replaces it with the one the decrypt answered, so it is never the value
		// handed on and after the return nothing in this process can reach it.
		defer replaced.Zeroize()
		ownPriv = replaced
	}

	// step 6: the path, in the ordered steps section 12.4.2 gives it.
	if commit.Path != nil {
		// RFC 9420 section 7.3's COMMIT door -- the third of the leaf validator's three
		// expectations, and the one that decides the leaf of a RECEIVED update path is signed, is
		// sourced commit, and carries a credential and capabilities this group admits.
		//
		// IT RUNS BEFORE THE MERGE, which is the whole of why it stands here rather than after it.
		// MergeUpdatePath compares a recomputed parent hash chain against path.LeafNode.ParentHash
		// and says in as many words that it does not verify the leaf's signature -- and parent_hash
		// is a field only the commit arm of section 7.6's select signs over. A merge run first
		// would be comparing its chain against an unsigned field, on a tree it has already begun to
		// adopt.
		//
		// The context it is judged in carries the POST-PROPOSAL extension set, so that a commit
		// which changes the group's extensions and publishes a path in one step is judged against
		// the extensions it installs. That is erratum 8745's own pairing, and effectiveContext is
		// where this package states it once.
		if err := ValidateUpdatePathLeafNode(self.crypto, commitInput.effectiveContext(),
			committer, commit.Path); err != nil {
			return nil, err
		}
		if err := applied.Tree.MergeUpdatePath(self.crypto, committer, commit.Path); err != nil {
			return nil, err
		}
		pathTreeHash, err := applied.Tree.TreeHash(self.crypto)
		if err != nil {
			return nil, err
		}
		// the provisional context carries the PREVIOUS epoch's confirmed transcript hash, because
		// the new one is a function of this very commit. Section 12.4.1 has the sender seal the
		// path under exactly this context, so what matters is that the two sides build the same
		// one; (*Group).CreateCommit builds it from the same six fields.
		provisional := &GroupContext{
			Version:                 self.context.Version,
			CipherSuite:             self.context.CipherSuite,
			GroupId:                 cloneBytes(self.context.GroupId),
			Epoch:                   self.context.Epoch + 1,
			TreeHash:                pathTreeHash,
			ConfirmedTranscriptHash: cloneBytes(self.context.ConfirmedTranscriptHash),
			Extensions:              applied.Extensions,
		}
		provisionalBytes, err := syntax.Marshal(provisional)
		if err != nil {
			return nil, err
		}
		// a CLONE of this member's private tree state, because a decrypt that wrote the new rungs
		// through the live one would have replaced the epoch this group is still running on before
		// anything below got the chance to refuse the commit. PathDecryptResult's own header
		// states the contract from the other side.
		//
		// The leaves this commit ADDS are excluded, which is the receiving half of the exclusion
		// (*Group).CreateCommit makes: a member added by this commit receives the path secret in
		// its Welcome and is sealed to nowhere in the update path.
		decrypted, err := applied.Tree.DecryptUpdatePath(self.crypto, committer, commit.Path,
			provisionalBytes, ownPriv.Clone(), applied.AddedLeaves)
		if err != nil {
			// the cryptographic half of ValSem203; the structural half ran in ValidateCommit, and
			// both carry the same sentinel so a caller can ask the question with one errors.Is
			return nil, fmt.Errorf("%w: %w", errPathDecrypt, err)
		}
		commitSecret = decrypted.CommitSecret
		ownPriv = decrypted.Private
	} else if ownPriv != nil {
		// a commit with no path leaves this client's own leaf key where it was, so the private
		// state carries forward -- and it is CLONED for (*Group).CreateCommit's stated reason: a
		// staged commit is erased when it is dropped, so a staged commit holding this group's own
		// live leaf state would erase, on that ordinary path, the key the epoch this group is
		// still in opens its update paths with.
		ownPriv = ownPriv.Clone()
	}

	// step 7: the new transcript hashes, the new group context and the new key schedule.
	confirmedInput, err := authenticated.ConfirmedTranscriptHashInput()
	if err != nil {
		return nil, err
	}
	confirmedHash := ConfirmedTranscriptHash(self.crypto, self.transcript.Interim, confirmedInput)
	treeHash, err := applied.Tree.TreeHash(self.crypto)
	if err != nil {
		return nil, err
	}
	newContext := &GroupContext{
		Version:                 self.context.Version,
		CipherSuite:             self.context.CipherSuite,
		GroupId:                 cloneBytes(self.context.GroupId),
		Epoch:                   self.context.Epoch + 1,
		TreeHash:                treeHash,
		ConfirmedTranscriptHash: confirmedHash,
		Extensions:              applied.Extensions,
	}
	schedule, err := NewKeySchedule(self.crypto, self.schedule.Secrets().InitSecret, commitSecret,
		EmptyPskSecret(self.crypto), newContext)
	if err != nil {
		return nil, err
	}

	// EVERY REFUSAL FROM HERE DOWN DROPS A WHOLE DERIVED EPOCH, and a derived epoch that becomes
	// unreachable is not a derived epoch that was erased. The schedule holds the init secret, the
	// confirmation key, the encryption secret, the epoch authenticator, the exporter and the
	// resumption PSK, and ownPriv is either the leaf state this path decrypted or a clone of this
	// group's own -- so the erase is written once, here, rather than at each of the five returns
	// below, and the flag is what keeps it off the one path where the StagedCommit takes ownership.
	// It is (*Group).CreateCommit's `handedOn` at the other end of the same file.
	handedOn := false
	defer func() {
		if handedOn {
			return
		}
		schedule.Zeroize()
		ownPriv.Zeroize()
	}()

	// step 8: ValSem205, the group's fork detector, against the confirmed transcript hash of the
	// epoch this commit OPENS and under the confirmation key of that same epoch.
	//
	// Both halves are load bearing. The tag is a MAC over the NEW confirmed transcript hash, so a
	// build that checked it before the transcript advanced would be comparing a value both sides
	// agree on whatever the commit did; and the key is the NEW epoch's, which is why this rule is
	// not one of ValidateCommit's twelve and is run by the caller that has a schedule. The
	// comparison goes through CryptoProvider.MacVerify, the only route to a tag comparison this
	// package allows.
	commitInput.ConfirmationKey = schedule.Secrets().Confirmation
	commitInput.ConfirmedHash = confirmedHash
	commitInput.ConfirmationTag = authenticated.Auth.ConfirmationTag
	if err := ValSem205ConfirmationTag(commitInput); err != nil {
		return nil, err
	}
	// a CLONE of the transcript, so a refusal below leaves this group's own transcript where the
	// epoch it is still in put it.
	transcript := self.transcript.Clone()
	if err := transcript.Update(self.crypto, confirmedInput, authenticated.Auth.ConfirmationTag); err != nil {
		return nil, err
	}

	// step 9: the new epoch's verified context, which is what the boundary owes the proposal cache,
	// and the secret tree the new epoch's messages ratchet out of.
	//
	// THIS CLIENT SIGNS THE GROUP INFO ITSELF, exactly as the creator does at epoch 0 and as
	// (*Group).CreateCommit does at every epoch after it. A commit carries no GroupInfo -- only a
	// Welcome does -- so there is no signature of the committer's over this context to verify, and
	// the authority the cache is owed is this client's own: it has just checked the confirmation
	// tag of the epoch it is about to enter, which is the strongest statement any member ever makes
	// about an epoch. A second door onto a verified context is what group_context_verified.go spent
	// four rounds establishing IS the defect, so the one door is used here too.
	groupInfo := &GroupInfo{
		GroupContext:    *newContext,
		ConfirmationTag: authenticated.Auth.ConfirmationTag,
		Signer:          self.ownLeaf,
	}
	if err := groupInfo.Sign(self.crypto, self.signer); err != nil {
		return nil, err
	}
	verified, err := groupInfo.VerifiedContext(self.crypto, applied.Tree)
	if err != nil {
		return nil, err
	}
	secretTree, err := NewSecretTree(self.crypto, applied.Tree.LeafWidth(),
		schedule.Secrets().Encryption)
	if err != nil {
		return nil, err
	}

	staged := &StagedCommit{
		committer:   committer,
		epoch:       newContext.Epoch,
		groupId:     cloneBytes(self.context.GroupId),
		priorEpoch:  self.context.Epoch,
		context:     newContext,
		verified:    verified,
		tree:        applied.Tree,
		schedule:    schedule,
		secretTree:  secretTree,
		ownPriv:     ownPriv,
		transcript:  transcript,
		list:        list,
		commit:      commit,
		added:       applied.AddedLeaves,
		removed:     applied.RemovedLeaves,
		updated:     applied.UpdatedLeaves,
		selfRemoved: false,
		hasPath:     commit.Path != nil,
		confirmTag:  authenticated.Auth.ConfirmationTag,
		restoreKind: restoreFromJoiner,
	}
	handedOn = true
	return staged, nil
}

// updatedOwnLeafPrivateLocked answers the private tree state this client holds AFTER a commit's
// proposals have been applied, or nil when they left this client's leaf alone.
//
// THE STATE IS FRESH AND CARRIES NO PATH SECRET, which is a consequence of section 12.1.2 rather
// than a simplification: an Update blanks the direct path of the leaf it replaces -- ApplyProposals
// says so and RatchetTree.UpdateLeaf does it -- so every rung this client held above that leaf is a
// secret for a node the commit blanked. Carrying them forward would leave a state whose held
// secrets name blank nodes, which is exactly what TreeKEMPrivate.Consistent refuses, and the rungs
// this client is entitled to are the ones the update path is about to hand it.
//
// The store's array is NOT erased here and is not the caller's to erase. GetPrivateKey answers
// whatever storage the implementation keeps -- the sdk writes these over its sealed local store --
// and NewTreeKEMPrivate copies what it is given, so an erase here would blank the entry this client
// may still need if the commit carrying it is refused further down and a later one carries it
// again. Deleting it is (StateStore).DeletePrivateKey's, on the epoch boundary task 19 owns.
func (self *Group) updatedOwnLeafPrivateLocked(applied *ApplyResult) (*TreeKEMPrivate, error) {
	// a spelled comparison of two leaf INDICES and not slices.Contains, which is what this
	// package's constant-time gate derives out of its own imports and refuses. That gate is about
	// comparisons of DATA and these are positions in a tree every member holds -- but the class it
	// derives is the comparator and not the operand, deliberately, and routing around a derived
	// class is the one thing standing rule 5 exists to prevent.
	updated := false
	for _, at := range applied.UpdatedLeaves {
		if at == self.ownLeaf {
			updated = true
		}
	}
	if !updated {
		return nil, nil
	}
	leaf := applied.Tree.Leaf(self.ownLeaf)
	if leaf == nil {
		return nil, fmt.Errorf("%w: this commit updates leaf %d and leaves it blank",
			ErrTreeMalformed, self.ownLeaf)
	}
	// the public key is CLONED on the way out, for persist's reason and it is the same reason
	// read one door over. applied.Tree is the tree this group is about to install -- ApplyCommit
	// assigns it three statements later -- so leaf.EncryptionKey is about to be the group's own
	// storage, and a store that keeps the slice it was handed to key a map with shares an array
	// with the live ratchet tree. A write through it changes an encryption public key the tree
	// hash and every parent hash were taken over, and nothing downstream would name it.
	//
	// Found by task 19's package-wide pass over the class persist was repaired for: this was the
	// one of the four calls into a caller-supplied store that did not clone.
	// TestNoOctetAGroupHandsOutwardIsStorageItKeeps does not reach it -- its own comment records
	// that the only outward call it observes is the one inside NewGroup.
	stored, err := self.store.GetPrivateKey(cloneBytes(leaf.EncryptionKey))
	if err != nil {
		return nil, fmt.Errorf("%w: leaf %d: %w", errUpdatedLeafPrivateKey, self.ownLeaf, err)
	}
	if len(stored) == 0 {
		return nil, fmt.Errorf("%w: leaf %d: the store answered no octets",
			errUpdatedLeafPrivateKey, self.ownLeaf)
	}
	return NewTreeKEMPrivate(self.ownLeaf, HpkePrivateKey(stored)), nil
}

// ApplyCommit promotes a staged inbound commit to live state.
//
// A STAGED COMMIT IS BOUND TO THE GROUP AND THE EPOCH THAT STAGED IT, and this is the door that
// asks. Processed and its Commit field are exported and connect/message is documented as holding
// Processed values across a policy decision, so what the two refusals below catch is the EXPECTED
// caller shape rather than a contrived one: two groups results are two values of one type in one
// caller hands. With the two refusals below deleted, group B handed a Processed group A had staged
// answered nil, left epoch 1 for the epoch A commit opened, and then answered an epoch
// authenticator byte-identical to A own -- measured, and the reason the binding is the first thing
// this body reads.
//
// BOTH HALVES, AND NEVER THE EPOCH ALONE. Every group this client is a member of runs an epoch 7,
// so an epoch number is not an identity; and the group id alone would admit a staged epoch of this
// group that some other epoch of it derived. It is the pair (*ProposalCache).bindingHolds asks, for
// that method own reason.
//
// A COMMIT THAT REMOVES THIS CLIENT MERGES NOTHING. There is no epoch to enter: the staged value is
// the report stageInboundCommitLocked answers for that case, this group own secrets are erased
// through Close, and the caller is told with ErrRemovedFromGroup rather than being handed a merge
// that silently produced a group this client is not a member of. It is answered BEFORE the pending
// refusal below and after the two above: a removal installs no epoch, so it is not one of the two
// candidate epochs that refusal exists to keep apart, and it is not a commit a caller can decline.
//
// A LIVE PENDING COMMIT IS REFUSED AND NOT OVERWRITTEN, which is (*Group).CreateCommit discipline
// at the other end of the same file rather than a second answer to the same question. A client
// holding two staged epochs has nothing to say which of them the delivery service accepted, and the
// repair -- ClearPendingCommit, which erases the epoch it drops -- is a decision the caller makes.
// What this body did instead was erase the caller staged epoch itself and install the peer commit
// over it, so a caller that had merely handed the wrong value lost a fully derived epoch to a call
// that answered nil.
func (self *Group) ApplyCommit(processed *Processed) error {
	if processed == nil || processed.Kind != ProcessedCommit || processed.Commit == nil {
		return errApplyCommitNotACommit
	}
	self.stateLock.Lock()
	if self.closed {
		self.stateLock.Unlock()
		return errGroupClosed
	}
	staged := processed.Commit
	// AN ERASED STAGED COMMIT INSTALLS NOTHING, and it is read FIRST because it is the one
	// condition none of the checks below can see. Everything they read -- the group id, the prior
	// epoch, the removal flag -- survives Zeroize, so an erased commit walks the whole door and
	// installs an erased key schedule, an erased secret tree and an erased leaf private state as
	// this group's live epoch. See errStagedCommitErased for the measurement.
	if staged.erased {
		self.stateLock.Unlock()
		return errStagedCommitErased
	}
	// AN INSTALLED STAGED COMMIT INSTALLS NOTHING EITHER, read SECOND and for the same reason:
	// a shell's group id and prior epoch are the ones it was staged with, its erased flag is
	// not set, and so it walks the pair below and reaches the merge, which assigns its nil
	// schedule over the live one and dereferences it at the persist. See errStagedCommitInstalled
	// for the measurement and for why the seam cannot reach this. It is read before the pair so
	// that a value the caller has finished with is named as such wherever it is handed, rather
	// than as a value of the wrong epoch at one instance and a panic at another.
	if staged.installed() {
		self.stateLock.Unlock()
		return errStagedCommitInstalled
	}
	// through crypto/subtle for guardrail 8 class reason, which is the class and not this line: a
	// group id is public, and every comparison of octets in this package is spelled the one way.
	if subtle.ConstantTimeCompare(staged.groupId, self.context.GroupId) != 1 {
		self.stateLock.Unlock()
		return fmt.Errorf("%w: it was staged by group %x and this group is %x",
			errApplyCommitNotThisGroups, staged.groupId, self.context.GroupId)
	}
	if staged.priorEpoch != self.context.Epoch {
		self.stateLock.Unlock()
		return fmt.Errorf("%w: it was staged against epoch %d and this group is in epoch %d",
			errApplyCommitNotThisEpochs, staged.priorEpoch, self.context.Epoch)
	}
	if staged.RemovesSelf() {
		self.stateLock.Unlock()
		// Close erases this epoch schedule, its secret tree, this leaf private state, the
		// signing key and any commit still staged, and it is idempotent -- so a caller own
		// deferred Close beside this one erases nothing twice.
		if err := self.Close(); err != nil {
			return err
		}
		return ErrRemovedFromGroup
	}
	if self.pending != nil {
		self.stateLock.Unlock()
		return ErrPendingCommitExists
	}
	self.pending = staged
	self.stateLock.Unlock()
	return self.MergePendingCommit()
}

// Protect seals an application message under the current epoch.
//
// The AAD and the plaintext are the CALLER'S ARRAYS and nothing here retains either: both are read
// once, into the signature preimage and into the AEAD, and what leaves is the ciphertext. A caller
// that edits either after the call has changed nothing about the message it was handed.
func (self *Group) Protect(aad, plaintext []byte) ([]byte, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		return nil, errGroupClosed
	}
	groupContext, err := syntax.Marshal(self.context)
	if err != nil {
		return nil, err
	}
	content := &FramedContent{
		GroupId:           cloneBytes(self.context.GroupId),
		Epoch:             self.context.Epoch,
		Sender:            Sender{SenderType: SenderTypeMember, LeafIndex: self.ownLeaf},
		AuthenticatedData: aad,
		ContentType:       ContentTypeApplication,
		ApplicationData:   plaintext,
	}
	authenticated, err := SignAuthenticatedContent(self.crypto, self.signer,
		WireFormatPrivateMessage, content, groupContext)
	if err != nil {
		return nil, err
	}
	// LAST, because it is the first thing here that cannot be undone: the seal consumes a
	// generation of this leaf's ratchet whether or not the message is ever sent.
	private, err := self.sealAndRecordLocked(authenticated)
	if err != nil {
		return nil, err
	}
	return MarshalMLSMessage(&MLSMessage{
		Version:        ProtocolVersionMls10,
		WireFormat:     WireFormatPrivateMessage,
		PrivateMessage: private,
	})
}

// ProtectBound is Protect for an AAD that has to NAME the generation the frame is sealed at.
//
// WHY IT IS A BUILDER AND NOT A VALUE, which is the whole of the design and not a style choice.
// MASTER section 8.4.2 v2 makes u32(generation) the third term of aad_mls, and Protect chooses the
// generation INSIDE, from the sender ratchet -- so the AAD cannot exist before the call, and the
// generation cannot be looked up and passed in without two calls, two lock acquisitions and a
// window between them. The AAD is also an INPUT to the signature, so there is no shape in which the
// caller is handed the generation afterwards and rebuilds it. What is left is this: the seal reads
// the next generation, calls the caller's builder with it, signs and seals -- all under ONE hold of
// the lock that already serialises this group's seals.
//
// WHY IT CANNOT RACE, in four parts, the fourth of which is the one that makes S1 a rule.
//
//	(i)   ONE CONSUMER. The only thing that advances a leaf's APPLICATION ratchet is the
//	      application seal's own keys.NextMessageKey(ContentTypeApplication, leaf). A proposal and
//	      a commit draw ContentTypeProposal and ContentTypeCommit, which select the HANDSHAKE
//	      ratchet -- a different ratchet of the same leaf.
//	(ii)  NO EXTERNAL DOOR. self.secretTree is unexported and is never returned by anything on this
//	      type, and the SecretTree's own generation doors -- NextSenderKey, NextMessageKey,
//	      SenderGeneration, receiverKey -- have no caller outside this package's own source and
//	      tests. receiverKey is unexported for exactly that reason, MASTER section 8.4.7 (3).
//	(iii) ONE LOCK HOLD spans the read, the build, the signature and the seal, so no other seal on
//	      this group can interleave between the read and the consume.
//	(iv)  THE PIN. sealAndRecordBoundLocked refuses and emits nothing if the generation consumed is
//	      not the one the builder was handed. If (i) to (iii) are ever wrong, the cost is one local
//	      refusal and one spent generation -- an ordinary gap, exactly like a refused submit --
//	      instead of a sender every one of whose messages its own peers refuse at R2 for a reason
//	      it cannot see. That asymmetry is why the pin is a MUST and not an assertion.
//
// THE GENERATION IS READ THROUGH SenderGeneration, which is the tree's own answer to "what will the
// next Next hand out" and is not a second derivation of it. It materialises the ratchet if this leaf
// has not sent yet, which is the same materialisation the seal below would have done a moment later.
//
// Protect is kept beside this for callers whose AAD is a constant. An application record's is not,
// and what keeps the record layer off it is a gate in connect/messagegroup, derived over that
// package's own production source, which refuses any call of .Protect outside the one method whose
// whole body is the interface delegation.
func (self *Group) ProtectBound(aad func(generation uint32) ([]byte, error),
	plaintext []byte) ([]byte, error) {

	if aad == nil {
		return nil, errNilAadBuilder
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		return nil, errGroupClosed
	}
	generation, err := self.secretTree.SenderGeneration(self.ownLeaf, RatchetApplication)
	if err != nil {
		return nil, err
	}
	authenticatedData, err := aad(generation)
	if err != nil {
		return nil, err
	}
	groupContext, err := syntax.Marshal(self.context)
	if err != nil {
		return nil, err
	}
	content := &FramedContent{
		GroupId:           cloneBytes(self.context.GroupId),
		Epoch:             self.context.Epoch,
		Sender:            Sender{SenderType: SenderTypeMember, LeafIndex: self.ownLeaf},
		AuthenticatedData: authenticatedData,
		ContentType:       ContentTypeApplication,
		ApplicationData:   plaintext,
	}
	authenticated, err := SignAuthenticatedContent(self.crypto, self.signer,
		WireFormatPrivateMessage, content, groupContext)
	if err != nil {
		return nil, err
	}
	// LAST, for Protect's reason, and PINNED for this one's: the seal consumes a generation of
	// this leaf's ratchet whether or not the message is ever sent, and the frame is emitted only
	// if the generation it consumed is the one the AAD above names.
	private, err := self.sealAndRecordBoundLocked(authenticated, generation)
	if err != nil {
		return nil, err
	}
	return MarshalMLSMessage(&MLSMessage{
		Version:        ProtocolVersionMls10,
		WireFormat:     WireFormatPrivateMessage,
		PrivateMessage: private,
	})
}

// errNilAadBuilder is what ProtectBound answers a caller that passed no builder.
//
// It is refused rather than defaulted to an empty AAD, which is the difference between a caller
// whose wiring is wrong and a caller that meant to send an unbound frame: an empty AAD is a legal
// value, so a default here would seal a frame naming no position and no generation and answer it as
// though it had been bound.
var errNilAadBuilder = errors.New("mls: a bound protect requires an aad builder")

// Unprotect opens an application message.
//
// It goes through ProcessMessage rather than calling OpenPrivateMessage itself, which is the whole
// reason it is six lines: the epoch check, the sender leaf check, the signature and the profile
// gate are stated once there, and a second receive path here would be a second answer to what this
// group accepts. What it adds is the refusal a caller asking for an application message needs -- a
// handshake message opened here is not a shorter answer, it is a different message, and by the time
// this returns a proposal it carried has already been cached.
func (self *Group) Unprotect(privateMessage []byte) (*ApplicationMessage, error) {
	processed, err := self.ProcessMessage(privateMessage)
	if err != nil {
		return nil, err
	}
	if processed.Kind != ProcessedApplication {
		return nil, fmt.Errorf("%w: this message was processed as kind %d",
			errApplicationMustBeCiphertext, processed.Kind)
	}
	return processed.Application, nil
}
