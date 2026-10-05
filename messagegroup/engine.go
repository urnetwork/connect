// Spec A section 6's narrow swappable interface, and the connect/mls adapter that satisfies it.
//
// The interface and the adapter share a file because section 2.2's tree pairs them, and they
// share a PACKAGE because EngineProcessed.stagedRef is unexported: an adapter that lived
// anywhere else could not put a staged commit in the one field section 6 calls unforgeable, so
// the two travel together or the argument is lost to a file move.
//
// It is declared HERE and not in connect/message. Section 12.1 gives the message server "no MLS
// type" and GroupHandle is twenty three of them; an interface whose whole subject is an MLS group,
// declared in the package the server links, is the shape the split exists to prevent.
//
// WHAT IS NOT ON THE INTERFACE IS THE WHOLE VALUE OF IT, quoted from section 6 because a later
// widening will be argued as a convenience:
//
//	Note what is not on this interface: no tree, no node, no secret tree, no HPKE, no
//	epoch_secret, no confirmation_key, no membership_key, no ciphersuite internals.
//	EngineProcessed.Raw and stagedRef are deliberately opaque so a staged commit can be carried
//	across a policy decision without connect/messagegroup being able to read or forge it.
//
// *mls.Group DOES NOT SATISFY GroupHandle and is not meant to. Thirteen of the twenty three
// methods cannot match -- OwnLeafIndex answers mls.LeafIndex where this answers uint32 and go
// method sets are identical-type rather than convertible-type; MemberAt takes a leaf and answers
// a Member where this takes an ordinal and answers three byte slices; MemberCount,
// SenderDataSecret, EncryptionSecret and ProposeGroupPolicy are absent; RatchetTreeSnapshot,
// GroupContextBytes, Commit and Process are named differently; ApplyCommit and Process name
// EngineProcessed, which is declared in THIS package and carries an unexported field, so no
// method of connect/mls can ever name it. That last pair is structurally unclosable BY DESIGN.
// The adapter below is what closes all thirteen, and every one of them is a decision it takes
// rather than a delegation it forwards.
//
// The type conversions all run in ONE direction at the boundary: a uint32 off the wire becomes an
// mls.LeafIndex here and nowhere else, so a raw index never reaches connect/mls unconverted.
package messagegroup

import (
	"fmt"

	"github.com/urnetwork/connect/mls"
	"github.com/urnetwork/connect/mls/syntax"
)

// GroupEngine is the factory half of section 6: everything a caller needs in order to obtain a
// GroupHandle, and nothing about a group it already holds.
//
// Five methods. Four are transcribed from section 6's block and the fifth is LoadGroup, which is a
// section 6 AMENDMENT and is documented as one at its declaration below. JoinFromWelcome is HERE
// and not on GroupHandle, which is worth stating because the other factories are obviously
// factories and this one reads like a group operation: a member joining from a Welcome has no
// handle yet, and putting it on the handle would require one to exist before the join that
// creates it.
//
// THE OBLIGATION JoinFromWelcome HANDS ITS CALLER, AND IT IS STATED HERE BECAUSE THIS IS THE
// SURFACE AN APP CALLS. A WELCOME AUTHENTICATES NOBODY. mls.JoinFromWelcome's own header spends
// fifteen lines on this and the adapter below repeats it, but a sentence one layer down is not an
// obligation on the caller of this interface, and until now this interface stated none of its own.
//
// What that means concretely, and it was REPRODUCED rather than argued: an attacker holding a key
// package this device published -- which went to the delivery service and to every member of every
// group that ever added this device -- can found a group of its own, add this device, and hand
// over the Welcome. Every check in this package and every check in connect/mls passes. The handle
// this method answers is a real handle: its group id, its epoch, its member count and its exporter
// all agree with the founder's, because the founder is the attacker and the group is real. What is
// false is only the thing no octet on that path carries -- that the group is the one the user
// meant to be in.
//
// SO A CALLER MUST ANCHOR THE WELCOME TO SOMETHING IT ALREADY TRUSTED BEFORE IT ACTS ON THE
// HANDLE. Membership is the natural anchor, and this interface hands a caller everything it needs
// to read one: GroupHandle.MemberAt answers each member's identity, and a caller that expected an
// invitation from a particular person can require that person's identity to be in the group before
// it renders a message, publishes a key, or tells a user they have been added.
//
// THIS PACKAGE DOES NOT PERFORM THAT CHECK AND MUST NOT INVENT ONE. Which identity a joiner should
// expect, where the expectation comes from, and what a device does when the anchor is absent are a
// design ruling and not an adapter's decision -- an anchoring mechanism chosen here would be a
// security argument taken by the layer with the least context to take it. Open item MG-1, in this
// directory's OPENITEMS.md, states the obligation and files the mechanism.
type GroupEngine interface {
	Suite() uint16
	NewKeyPackage() (keyPackage []byte, err error)
	CreateGroup(groupId []byte, policy []byte, leafKeys []byte) (GroupHandle, error)

	// LoadGroup reopens a group this device already belongs to, out of the epoch state a previous
	// run persisted. IT IS THE FIFTH METHOD AND SECTION 6'S BLOCK HAS FOUR: it is an amendment,
	// and the paragraph a reader needs is why an amendment was the only honest answer.
	//
	// WITHOUT IT A DURABLE STORE IS WRITE-ONLY. CreateGroup founds, JoinFromWelcome joins, and
	// neither of them opens something already on the disk -- so a device that restarts has rows it
	// wrote and no door back into them. sdk carried its own GroupHandle over mls.LoadGroup for
	// exactly that reason and it was not free: connectMlsHandle is unexported, so the copy was a
	// SECOND implementation of twenty six methods by construction of the visibility rules, and
	// two of them -- Process and ApplyCommit -- could not be written at all. EngineProcessed's
	// staged half is unexported, so a foreign implementation can only build a value ApplyCommit
	// refuses; sdk's copy therefore REFUSED the pair by name, and a restored group could not
	// ingest a commit. That was open item J1-8 and this method is what closes it.
	//
	// THE EPOCH IS A PARAMETER AND THERE IS NO DEFAULT. mls.StateStore holds one blob per (group,
	// epoch) and nothing on it enumerates, so this layer cannot answer "the latest" without a
	// scan it has no method for. Whoever persisted the group is the one that knows which epoch it
	// was at, so the epoch travels with that caller's own record rather than being guessed here --
	// and an implementation that guessed would reopen a group at an epoch its peers have left,
	// which is a member that cannot read the next message and has nothing to say why.
	//
	// AND THE EPOCH IT ANSWERS AT IS CHECKED AGAINST THE EPOCH IT WAS ASKED FOR. See the adapter.
	LoadGroup(groupId []byte, epoch uint64) (GroupHandle, error)

	JoinFromWelcome(welcome []byte, ratchetTree []byte) (GroupHandle, error)
}

// GroupHandle is the entire MLS surface the storage layer is allowed to see.
//
// Adding a method here is a design decision and not a convenience: everything on it is something
// a replacement implementation must provide, and engine_test.go holds the set to section 6's
// block by reading this file rather than by trusting this comment.
//
// Every signature names go types only. A method that named an mls type would make Gate 5's swap
// a type change rather than a factory change, and the interface would have stopped being a seam
// and become a re-export of connect/mls.
type GroupHandle interface {
	GroupId() []byte
	Epoch() uint64
	OwnLeafIndex() uint32
	MemberCount() int
	MemberAt(i int) (leafIndex uint32, identityPub []byte, leafKeys []byte, err error)

	// RoleAt is the thirty fourth method, added 2026-09-22 for ledger item 242's R4, and it is a
	// SECOND PROJECTION OF THE SNAPSHOT MemberAt ALREADY BUILDS rather than a second computation:
	// connect/mls resolves every member's role against urmessage_group_policy inside Members()
	// (group.go's membersLocked), and MemberAt throws that field away because the epoch fan out it
	// was written for wraps to leaf keys and has no use for a role.
	//
	// IT IS KEYED BY LEAF INDEX AND NOT BY ORDINAL, which is the one signature decision. MemberAt
	// is half of an ordinal PAIR -- MemberCount and MemberAt, so a caller walking 0..n-1 need not
	// know which leaves are blank -- and every caller of this one arrives holding a LEAF: a
	// record's sender is authenticated to a leaf, and a leaf's ordinal is a function of which
	// other leaves happen to be occupied at that epoch. A method keyed on the ordinal would make
	// its caller walk the membership to convert, which is the walk this method exists to replace,
	// and would answer a DIFFERENT member for the same leaf after any removal.
	//
	// THE ROLE IS A STRING AND IT IS connect/mls's OWN Role.String() -- "observer", "member",
	// "admin", "owner". A uint8 here would be either a re-export of mls.Role under another name or
	// a second numbering nobody can hold equal to the first; the string is the spelling MASTER
	// section 11 uses and the spelling the cgo surface already answers.
	//
	// AN UNNAMED IDENTITY READS AS "member", AND A CONTEXT CARRYING NO 0xF001 READS EVERY MEMBER
	// AS "member" -- item 242's rulings 8 and 20 -- and neither is decided here: both are already
	// Members()' reading, so this projection and the sdk's commit arm cannot come to disagree
	// about the same group at the same epoch. A policy-less context is therefore NOT an error.
	// What IS an error is a leaf no member of this group stands at, for MemberAt's stated reason:
	// every absence on this interface is an error and never a zero value, and a nil identity
	// beside an empty role would read downstream as an unnamed member rather than as nobody.
	RoleAt(leaf uint32) (identityPub []byte, role string, err error)

	// the two named secrets of MASTER section 8.2, and the exporter. Nothing else: EpochSecret
	// is deliberately absent, which is guardrail G6 seen from this side -- an accessor taking a
	// name would reach epoch_secret, confirmation_key and membership_key through the same door.
	Export(label string, context []byte, length int) ([]byte, error)

	// AMENDED for ledger item 228's read-receipt tag ruling. PairwiseExport is the exporter for
	// key material TWO NAMED MEMBERS HOLD AND NO THIRD MEMBER CAN DERIVE, and it is on this
	// interface for the same reason Export is and with the opposite scope: Export expands one of
	// the epoch schedule's own secrets, so every member of the group answers the same value from
	// it, while this one is a static-static diffie-hellman over two RFC 9420 leaf encryption
	// keypairs and no member outside the pair can reach it.
	//
	// THE PEER IS A uint32 AND NOT AN mls.LeafIndex, which is this block's standing rule and not
	// a convenience: a method naming an mls type would make Gate 5's swap a type change rather
	// than a factory change. It is the same leaf index MemberAt answers at its first result.
	//
	// THE DIFFIE-HELLMAN HAPPENS BEHIND THIS SEAM AND NO LEAF SCALAR CROSSES IT. That is what
	// this method buys over an accessor for the private key, and it is also forced: the label
	// expansion is a method on mls's own CryptoProvider, which nothing on this side holds.
	PairwiseExport(label string, peer uint32, length int) ([]byte, error)

	SenderDataSecret() ([]byte, error)
	EncryptionSecret() ([]byte, error)
	EpochAuthenticator() []byte
	RatchetTreeSnapshot() ([]byte, error)
	GroupContextBytes() ([]byte, error)

	ProposeAdd(keyPackage []byte) ([]byte, error)
	ProposeRemove(leafIndex uint32) ([]byte, error)
	ProposeUpdate() ([]byte, error)
	ProposeGroupPolicy(policy []byte) ([]byte, error)

	Commit(byReference [][]byte) (commit []byte, welcome []byte, ratchetTree []byte, err error)

	// CommitAdd builds a commit that carries one Add proposal PER KEY PACKAGE, BY VALUE, and
	// nothing by reference. IT IS THE TWENTY SEVENTH METHOD AND SECTION 6'S BLOCK HAS TWENTY SIX:
	// it is an amendment, made 2026-09-18 for ledger item 239's group chats, and the paragraph a
	// reader needs is what the by-reference arm cannot do.
	//
	// Commit names proposals by REFERENCE, and a reference resolves against the receiver's own
	// proposal cache for the epoch that is closing. A member that never received the proposal
	// refuses the commit -- (*mls.ProposalCache).Resolve answers errProposalNotCached, "proposal
	// reference is not cached for this epoch" -- so the ProposeAdd-then-Commit pair only works
	// while every member sees every proposal before the commit, which is one record per proposal
	// per member on top of the commit. RFC 9420 section 12.4 lets a commit carry a proposal
	// INLINE instead, attributed to the committer, and (*mls.Group).CreateCommit has carried that
	// arm since it was written; this method is that arm brought to the seam. A receiver needs
	// nothing cached, and N adds are one commit rather than N proposals and a commit.
	//
	// EXACTLY THESE ADDS AND NOTHING ELSE, which is the one contract decision. Passing a nil
	// by-reference vector to CreateCommit would fold in every proposal this member has cached,
	// and that would make "a member that never saw a proposal can process this" a fact about
	// the cache's state at the moment of the call rather than about the method. A caller that
	// wants cached proposals committed has Commit for them.
	//
	// THE KEY PACKAGES ARE OCTETS AND NOT mls.KeyPackage, for the same reason every other
	// signature on this interface names go types only; the decode happens behind the seam.
	CommitAdd(keyPackages [][]byte) (commit []byte, welcome []byte, ratchetTree []byte, err error)

	// CommitContextExtensions, CommitPolicy and CommitRemove are the by-value arms ledger item
	// 242's R1 adds beside CommitAdd, 2026-09-21, and they mirror it EXACTLY: each builds one
	// commit carrying the named proposals by value and nothing by reference, so a member that
	// never saw a proposal processes it cold, and each names go types only for CommitAdd's
	// reason -- a parameter naming mls.Proposal would be the re-export Property 3 refuses.
	//
	// CommitContextExtensions carries ONE GroupContextExtensions proposal with EXACTLY the list
	// it is handed, which is RFC 9420 section 12.1.6's WHOLESALE replacement: the caller passes
	// the full post-commit list, and an entry it leaves out is gone from the group. The seam
	// judges nothing about the list but that it is not empty. mls's own doors run inside
	// CreateCommit, and this profile's rule that the list still carries a policy is the
	// AUTHORIZER's on both arms rather than this adapter's -- which is what lets the receiving
	// arm be tested against a real commit that strips 0xF001, item 242's P3, through the seam.
	//
	// CommitPolicy is the convenience every role change is made through: the group's CURRENT
	// list with only 0xF001 replaced by the policy it is handed, so 0x0003 required_capabilities
	// and every other entry survive. It is ProposeGroupPolicy's repaired shape (item 242's P4)
	// committed by value.
	//
	// CommitRemove carries one Remove per leaf. THE SDK EXPOSES NO PRODUCT METHOD OVER IT UNTIL
	// LEDGER ITEMS 243, 244 AND 245 CLOSE -- pq_secret rotation, the served commit that hands a
	// removed member the next epoch's keys, and the sender_handle a newcomer inherits each gate
	// removal on its own -- and it is on the seam now so that the receiving arm can be tested
	// against a real Remove rather than a hand-built one.
	CommitContextExtensions(extensions []ExtensionBytes) (commit []byte, welcome []byte, ratchetTree []byte, err error)
	CommitPolicy(policy []byte) (commit []byte, welcome []byte, ratchetTree []byte, err error)
	CommitRemove(leaves []uint32) (commit []byte, welcome []byte, ratchetTree []byte, err error)

	// CommitRemoveWithExtensions IS THE ARM A REMOVAL ACTUALLY SHIPS ON, added 2026-09-25 for
	// ledger item 257's ruling 51, and it is here because CommitRemove above cannot carry one.
	//
	// A bare Remove of the LAST leaf of an identity the policy NAMES is an R0c phantom at every
	// honest receiver: MASTER section 6's urmessage_group_policy is keyed by credential identity,
	// the entry survives a commit that carries no GroupContextExtensions proposal, and the group
	// the commit opens then names an identity with no leaf. Any SetRole names an identity
	// permanently -- nothing calls RemoveRole -- so the OWNER cannot remove an ADMIN and a new
	// owner cannot remove the ex-owner's leaf through CommitRemove at all. What closes it is one
	// commit carrying the Remove AND the policy that drops the entry, and until this arm the only
	// door onto a multi-proposal commit was Commit(nil), the fold of cached proposals item 242's
	// ruling 13 forbids in production.
	//
	// TWO PARAMETERS AND NOT A PROPOSAL LIST. A general CommitProposals taking []mls.Proposal is
	// the obvious generalisation and it is refused by the same rule every other method here obeys:
	// TestNoMethodOfEitherEngineInterfaceNamesAConnectMlsType errors on any qualified type in
	// either interface's signature, so the arm is spelled in the two go types the seam already
	// carries -- a leaf is the uint32 CommitRemove takes, and a list is the []ExtensionBytes
	// CommitContextExtensions takes.
	//
	// AND THERE IS NO ...WithPolicy CONVENIENCE BESIDE IT. CommitPolicy exists because a role
	// change replaces one entry of a list the seam can read for itself; a removal's list is
	// assembled by the sdk, which holds the identity being removed and the role model that
	// decides what the post-commit list is. A second arm here would be this package deciding it.
	//
	// THE PROPOSAL ORDER IS FIXED INSIDE, Remove first and the GroupContextExtensions last, and
	// the adapter says why at the site. It is not a preference: the commit's own proposal vector
	// carries the caller's order into the confirmed transcript hash, so an order chosen per call
	// site is two signed commits for one removal.
	CommitRemoveWithExtensions(leaves []uint32, extensions []ExtensionBytes) (commit []byte, welcome []byte, ratchetTree []byte, err error)

	// PendingEpoch and PendingExport are the two reads that let a committer SUBMIT BEFORE IT
	// MERGES, added 2026-09-22 for ledger item 242's R2. Commit's own contract says the staged
	// epoch is staged and not merged because the delivery service accepts at most one commit per
	// (group, epoch) and an optimistic merge forks the loser (MASTER section 9.3) -- and yet every
	// committer merged first, because the record that announces an epoch carries facts of that
	// epoch: its write and read keys through its exporter, a hash of its group context, the count
	// the fan-out will wrap to, and the number itself; and the only door onto any of them was the
	// live handle after the merge. Measured, an honest owner whose transfer lost the race to an
	// admin's role change was left at a private epoch nobody else entered, unable to open the
	// winner's commit or to seal a record the server would take, until the app restarted.
	//
	// PendingEpoch answers the facts that are not key material in one value, read off the
	// staged tree and the staged context -- the epoch the staged commit opens, the members it
	// leaves in the group, the leaves it takes OUT of it, and the serialized post-commit
	// GroupContext -- and PendingExport is Export through the staged epoch's schedule, a fresh
	// derivation the caller owns and erases exactly as Export's answer is. Both answer an error
	// when nothing is staged, so an announcement can never be built out of the epoch the group is
	// already in. They are two methods and not five for the reason EngineProcessed is a value: the
	// facts arrive together and are read together, and the one that is a secret stays behind a
	// call. AMENDED 2026-09-25 for ledger item 257's ruling 51, which is what put the removed
	// leaves on the value: they were a parameter an arm passed to the fan-out, and a parameter is
	// a fact about whether an arm remembered. Nothing on this interface changed to carry them --
	// the field went on a struct the method already answers, which is the whole shape the ruling
	// chose.
	//
	// The values are the ones the live handle answers once MergePendingCommit has run --
	// engine_test.go holds each of them to its live sibling across a merge -- so a caller that
	// announces off these and merges after the server said yes announces the epoch it enters.
	PendingEpoch() (*PendingEpoch, error)
	PendingExport(label string, context []byte, length int) ([]byte, error)

	MergePendingCommit() error
	ClearPendingCommit()

	Process(message []byte) (*EngineProcessed, error)
	ApplyCommit(processed *EngineProcessed) error

	// DiscardProcessed erases the epoch a processed commit staged, for the caller that REFUSED it
	// between Process and ApplyCommit. AMENDED 2026-09-21 for ledger item 242's R1: MASTER section
	// 11 has a receiving client reject a bad commit on validation, and a rejected commit is a
	// fully derived second epoch -- key schedule, secret tree, leaf private state -- that nothing
	// else would erase. The staged value is the caller's, this handle's Close never sees it, and
	// a caller that simply dropped it would leave a whole epoch in the heap for the collector to
	// move around, which is the hazard connect/mls's erase gate exists to refuse. A later
	// ApplyCommit of the same value refuses rather than installing zeros. It is on the interface
	// and not a free function for ApplyCommit's reason: the staged half is unexported and only
	// this package can reach it. A processed application or proposal message holds no epoch and
	// discarding one is a no-op that answers nil.
	//
	// AND APPLY-THEN-DISCARD IS A NO-OP THAT ANSWERS NIL, stated because it is the order every
	// receiving arm will write -- `defer handle.DiscardProcessed(processed)` beside an
	// ApplyCommit that then succeeds -- and because the first build of this door got it wrong:
	// the value a caller holds after ApplyCommit named the epoch the handle had just entered, and
	// discarding it erased that epoch with no error at the line that did it. ApplyCommit now
	// releases the staged half on a successful install, so a later discard finds nothing to
	// erase; a caller may discard every value it processed, applied or refused, and the only
	// value a discard erases is one that was never installed.
	DiscardProcessed(processed *EngineProcessed) error

	// AMENDED 2026-09-17 FOR MASTER SECTION 8.4.2 v2, in three places, all forced by the same
	// sentence: the GENERATION is now inside aad_mls.
	//
	// ProtectBound takes a BUILDER rather than an aad, because the seal chooses the generation
	// INSIDE, from the sender ratchet, so no caller can compute the aad before the call. Protect
	// is KEPT beside it for a caller whose aad is a constant -- a commit's is -- and no
	// production site in this package may reach it for an application record:
	// TestNoProductionSiteReachesTheUnboundProtect is what says so, derived off this package's
	// own source rather than off a reviewer's memory.
	//
	// Unprotect answers the generation because MASTER section 8.4.3's R2 is a function of it and
	// the deciding reading is the one taken on what the open AUTHENTICATED.
	//
	// PeekSender answers the generation for the same reason one step earlier, and that one is
	// what section 8.4.3's R3 turns from an optimisation into a requirement: all three values
	// come out of ONE SenderData open under a secret every member already holds, so the refusal
	// is taken before any ratchet is reached.
	Protect(aad []byte, plaintext []byte) ([]byte, error)
	ProtectBound(aad func(generation uint32) ([]byte, error), plaintext []byte) ([]byte, error)
	Unprotect(message []byte) (aad []byte, plaintext []byte, senderLeaf uint32, generation uint32, err error)
	PeekSender(frame []byte) (senderLeaf uint32, aad []byte, generation uint32, err error)

	Close() error
}

// The three kinds Process discriminates, section 6's own numbering.
//
// They are declared as this package's constants rather than read off connect/mls's ProcessedKind
// because the field is a uint8 on an interface this package owns: a caller comparing against
// mls.ProcessedCommit would be naming an mls type through the seam, which is the one move the
// interface exists to prevent.
const (
	EngineProcessedApplication uint8 = 1
	EngineProcessedProposal    uint8 = 2
	EngineProcessedCommit      uint8 = 3
)

// ProcessedMember is one occupied leaf of the tree a commit ENTERS, with the credential identity
// that leaf carries and whether it carries a wrap target.
//
// It is a go type on the seam for GroupHandle's standing reason -- an mls.LeafIndex or an
// mls.Member here would be a re-export -- and it carries the identity because the identity is
// what a role is keyed by: MASTER section 6's urmessage_group_policy names members by credential
// identity, and item 242's ruling 6 ships the role model keyed on it as it stands.
//
// HasLeafKeys, ADDED 2026-09-21 AS THE ONE HARDENING R1 CARRIED INTO R2, is whether the leaf
// carries a urmessage_leaf_keys (0xF002) extension this profile can wrap to, read off the STAGED
// tree by the same call both send doors refuse a key package with -- mls.LeafKeysOf, through
// (*mls.StagedCommit).LeafHasKeysAfter -- so a leaf carrying the type twice or a body that does
// not parse reads false here exactly as it is refused there. It is here because the receiving
// side had no twin of that refusal: mls's list rules require an added leaf to LIST the type and
// never to carry one, this seam's CommitAdd and ProposeAdd are doors a hostile mls build never
// walks, and a keyless leaf admitted past them is a member every honest epoch wrap silently
// skips -- the first symptom is MemberAt refusing that ordinal one commit later, and the second
// is that member reading nothing. The authorizer refuses an Add whose leaf reads false; nothing
// below it does. A bool and not the body, because no caller of that decision wraps anything.
type ProcessedMember struct {
	Leaf        uint32
	Identity    []byte
	HasLeafKeys bool
}

// ExtensionBytes is one group-context extension as octets: the RFC 9420 extension type and its
// body, which is the seam's spelling of mls.Extension in both directions. EngineProcessed hands a
// caller the post-commit list in it and CommitContextExtensions takes one, so a caller can compare
// two lists entry by entry, and decode the one it cares about, without naming an mls type.
type ExtensionBytes struct {
	Type uint16
	Data []byte
}

// PendingEpoch is the epoch a handle's OWN staged commit would open, as the committer is allowed
// to see it before the delivery service has accepted the commit: the epoch number, the number of
// members the staged tree holds, the leaves the commit takes out of the group, and the serialized
// post-commit GroupContext. It is what GroupHandle.PendingEpoch answers and it carries no key
// material; the staged epoch's exporter is GroupHandle.PendingExport, a call rather than a field,
// so the one secret an announcement needs is derived on demand and erased by the caller rather
// than parked in a struct.
//
// REMOVEDLEAVES IS A FIELD HERE AND NOT AN ARGUMENT SOMEWHERE ELSE, added 2026-09-25 for ledger
// item 257's ruling 51, and the reason is what the field replaced. The epoch fan-out that seals
// the next epoch's post-quantum secret to each member is built PRE-MERGE off the LIVE tree, where
// a removed member is still standing -- so the fan-out has to EXCLUDE the leaves this commit
// removes, or the one member the commit exists to shut out is handed the secret of the epoch it
// opens. That exclusion was a vector a caller passed down beside the commit, on the reasoning
// that there was nothing to read it off; the staged commit had it all along, this handle already
// answers a value read off that staged commit, and the field costs no method on the seam. An arm
// can no longer forget an argument it does not pass.
//
// IT IS THE LEAVES AND NOT THE MEMBER COUNT'S COMPLEMENT. len(RemovedLeaves) has no arithmetic
// relationship to MemberCount that holds across proposal types: an Add leaves this empty while
// MemberCount rises, because an added leaf is not in the live tree and gets no wrap at all, so
// any equality between a wrap set's size and MemberCount is false on every Add.
//
// Every field is a value or a copy. GroupContext is a fresh marshal of the staged context and
// RemovedLeaves is a fresh vector, so a caller that keeps this value keeps nothing that aliases
// the epoch a merge is about to install.
type PendingEpoch struct {
	Epoch         uint64
	MemberCount   int
	RemovedLeaves []uint32
	GroupContext  []byte
}

// EngineProcessed is one ingested MLS message as the storage layer is allowed to see it.
//
// Raw and stagedRef are both opaque and they are opaque for different reasons. Raw is opaque by
// CONTRACT -- nothing in this package may index, parse, compare or length check it, and
// engine_test.go derives the class of functions that read an EngineProcessed and holds every one
// of them to that. stagedRef is opaque by CONSTRUCTION: it is unexported, so only a member of
// this package can populate one, which is what makes a staged commit carried across a policy
// decision unforgeable BY THIS PACKAGE.
//
// The scope of that guarantee, stated because the loose reading of it has been wrong once. A
// keyed composite literal naming only the exported fields is legal across package boundaries, so
// a foreign engine may satisfy GroupHandle and return one of these; what it cannot do is put
// anything in stagedRef. The unforgeability therefore holds for engines declared in this
// package, and a foreign engine trades it for its independence. Open item M1-43.
type EngineProcessed struct {
	Kind       uint8
	SenderLeaf uint32
	Aad        []byte
	Plaintext  []byte

	// ── what a COMMIT does, read off the staged commit at Process time ─────────────────────
	//
	// These are populated ONLY on the commit arm (Kind == EngineProcessedCommit) and are the zero
	// value on the application and proposal arms. They are here because MASTER section 11 makes a
	// bad commit one that "is refused by the committing client, and is rejected by every receiving
	// client ON VALIDATION" -- and a receiving client's validation is an authorization decision it
	// must take BEFORE ApplyCommit, on what the commit DOES and who authored it, without reaching
	// the opaque staged half. So the storage layer needs the commit's shape out of Process's own
	// result rather than out of a membership diff it could only take AFTER applying.
	//
	// Committer is AUTHENTICATED and the three vectors are NOT A CLAIM: the commit's signature has
	// been verified against Committer's own leaf by the time Process answers, and the leaves are
	// where its proposals were RESOLVED and APPLIED against this member's own tree, not where a
	// header said they would be. They are LEAF INDICES and not sender_handles because that is what
	// the staged commit names and what the interface's uint32 rule already carries; a caller that
	// wants a role reads it off the membership at that leaf.
	//
	// They are on the struct rather than behind a new interface method because a method would be a
	// twenty-eighth entry on section 6's block, and these are facts Process already holds -- the
	// same reasoning SenderLeaf, Aad and Plaintext are fields for. A foreign engine leaves them
	// zero, which is the zero value a keyed literal already produces and which ApplyCommit's
	// foreign-refusal makes moot.
	CommitterLeaf uint32
	AddedLeaves   []uint32
	RemovedLeaves []uint32
	UpdatedLeaves []uint32

	// AMENDED 2026-09-21 FOR LEDGER ITEM 242's R1, the receiving arm of the role model. The three
	// vectors above say WHERE a commit's proposals landed and nothing about WHO, and a role is
	// keyed by identity: an authorizer holding them alone cannot tell an Add that admits a
	// stranger from one whose credential merely CLAIMS the owner's identity, cannot see that an
	// Update or the committer's own path changed a leaf's identity, and cannot read the policy
	// the commit installs. So the commit arm also carries the three below, populated on that arm
	// only and zero on the other two, fields and not methods for the reason the paragraph above
	// gives. Every slice is storage the caller owns, cloned out of the staged value, so nothing
	// here aliases the epoch ApplyCommit is about to enter.
	//
	// CommitterIdentity is the committer's credential identity AS OF THE PRE-COMMIT TREE. It is
	// authenticated for the reason CommitterLeaf is: Process verified the commit's signature
	// against that leaf, and this is the identity the leaf carried when it did. It is read off
	// the live tree at Process time, which is still the pre-commit tree because Process moves no
	// live state.
	//
	// AND THE PRE-COMMIT READING IS THE WHOLE POINT, so it is stated as the obligation it puts
	// on the caller. connect/mls holds NO identity-continuity rule: the committer's own path
	// leaf is its current leaf cloned and re-signed, nothing compares the credential identity
	// on that leaf against the one it replaces, and a committer that rewrote its own leaf's
	// identity to another member's before committing produces a commit every honest receiver
	// ACCEPTS -- mls pins that acceptance, and the two reads that tell it apart, in
	// TestTheCommittersOwnPathCanSwapItsLeafIdentityAndOnlyThePreCommitTreeStillNamesIt.
	// Over such a commit this field still names the committer as the pre-commit tree knew it,
	// and MembersAfter at CommitterLeaf names the identity the path put there. THE AUTHORIZER
	// MUST COMPARE THE TWO -- CommitterIdentity against MembersAfter[CommitterLeaf].Identity --
	// and refuse the commit when they differ: that comparison is MASTER section 11's rule that
	// "a leaf's identity does not change across ... the committer's own path", it is made by no
	// layer below the authorizer, and a receiving arm that skips it hands the committer whatever
	// role the claimed identity holds. A CommitterIdentity read off the staged tree would name
	// the victim here and make the comparison vacuous, which is why the reading is pinned off
	// the source in TestCommitterIdentityIsReadOffTheLiveGroupAndMembersAfterOffTheStagedValue.
	// The same rule's other arm, an Update, is the caller's to check the same way: every
	// UpdatedLeaves entry's identity in MembersAfter against the pre-commit membership.
	//
	// MembersAfter is EVERY occupied leaf of the STAGED, post-commit tree with its identity and
	// its wrap-target fact, in leaf order: what the added identities, identity continuity on
	// Update and on the committer's own path, the post-commit identity set the policy is judged
	// against, and whether every added leaf can be wrapped to are all computed from. It is read
	// off the staged commit's own tree -- the one ApplyCommit installs -- and never off a
	// membership diff, which a caller could only take after applying, AND NEVER OFF THE LIVE
	// GROUP for a leaf that happens to exist there: over the swap above, the entry at
	// CommitterLeaf names the identity the path put there, which is the whole of what the
	// comparison reads, and TestMembersAfterNamesTheIdentityTheCommitLeavesAtTheCommittersLeaf
	// builds that commit and holds it. A commit that removes THIS member is answered off the
	// report mls hands a removed member: the post-proposal tree, minus the committer's path,
	// which a removed member cannot open.
	//
	// ContextExtensionsAfter is the FULL post-commit group-context extension list, so a caller
	// can check the policy the commit installs AND that every other entry -- 0x0003
	// required_capabilities above all -- is byte-identical to the pre-commit list it decodes out
	// of GroupContextBytes. A commit carrying no GroupContextExtensions proposal installs the
	// list the group already had, so this is then the PRE-COMMIT list entry for entry, and the
	// adapter makes that so for the removed member's report too, whose staged value carries no
	// context of its own.
	CommitterIdentity      []byte
	MembersAfter           []ProcessedMember
	ContextExtensionsAfter []ExtensionBytes

	// opaque to this package and handed back to ApplyCommit. It is the message these values
	// were read out of and nothing else; the staged commit is never here.
	Raw []byte
	// engine private. This package never inspects it, and no package but this one can write it.
	stagedRef any
}

// ---------------------------------------------------------------------------
// the connect/mls adapter
// ---------------------------------------------------------------------------

// The compile time assertions Task 9 could not make about *mls.Group, made about the type that is
// meant to have the property. A failure here is a build failure, which is the point: there is no
// state of this tree in which the adapter has stopped satisfying the interface and a test reports
// it later.
var (
	_ GroupEngine = (*connectMlsEngine)(nil)
	_ GroupHandle = (*connectMlsHandle)(nil)
)

// connectMlsEngine is the v1 engine: one identity, one crypto provider, one state store.
//
// It is unexported because the factory is the door. Gate 5's swap point is
// NewConnectMlsEngine's return type -- an interface -- and a caller holding the concrete type
// would be a caller the swap breaks.
type connectMlsEngine struct {
	crypto mls.CryptoProvider
	store  mls.StateStore
	signer mls.SignaturePrivateKey
	cred   mls.Credential
	// the encoded urmessage_leaf_keys body this device publishes: u16 alg_id followed by the
	// opaque X-Wing encapsulation key. It is held on the engine because section 6's
	// NewKeyPackage takes no arguments and a key package with no leaf keys extension is a leaf
	// no epoch fan out can ever wrap to.
	leafKeys []byte
}

// NewConnectMlsEngine builds the v1 engine over connect/mls.
//
// Everything it holds is injected, which is what makes the engine swappable at the factory: this
// function is the only place in the tree that names *mls.Group's neighbourhood on the way in.
//
// leafKeys is the ENCODED urmessage_leaf_keys body and is refused here rather than at the first
// group, because a device that cannot say what its wrap target key is has nothing to fix later:
// every group it creates would carry a leaf no device wrap can address.
func NewConnectMlsEngine(crypto mls.CryptoProvider, store mls.StateStore,
	signer mls.SignaturePrivateKey, cred mls.Credential, leafKeys []byte) (GroupEngine, error) {

	if crypto == nil {
		return nil, fmt.Errorf("%w: every secret this engine derives is drawn through it", ErrEngineCryptoProvider)
	}
	if store == nil {
		return nil, fmt.Errorf("%w: a group with nowhere to persist an epoch is a group that cannot be reopened", ErrEngineStateStore)
	}
	if len(signer) == 0 {
		return nil, fmt.Errorf("%w: the leaf of every group this engine founds is signed with it", ErrEngineSigner)
	}
	if _, err := mls.ParseLeafKeysExtension(leafKeys); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrEngineLeafKeys, err)
	}
	return &connectMlsEngine{
		crypto: crypto,
		store:  store,
		// copies, because a caller's arrays are the caller's: a signer or a leaf keys body that
		// moved under this engine would sign one group's leaf with another group's key.
		signer:   append(mls.SignaturePrivateKey(nil), signer...),
		cred:     mls.BasicCredential(cred.Identity),
		leafKeys: append([]byte(nil), leafKeys...),
	}, nil
}

// Zeroize erases the identity signing key this engine holds a copy of.
//
// NOTHING IN THIS PACKAGE CALLS IT, and that is a gap in section 6 rather than an oversight here:
// GroupEngine has five methods and none of them is a lifecycle method, so an engine has no
// documented end. mls.Group clones the same signer into every group this device founds and erases
// its own copy at Close; this copy has no Close to be erased at. The erase is declared so the
// obligation sits on the type that holds the octets -- which is where connect/mls's erase reading
// puts it -- and so that whatever owns the engine has something to call the day section 6 grows
// one. It is reported as a finding of this batch rather than left as a comment.
//
//go:noinline
func (self *connectMlsEngine) Zeroize() {
	zeroize(self.signer)
}

// Suite is the ciphersuite the provider runs, as the code point rather than as mls.CipherSuite.
//
// The conversion is the seam. mls.CipherSuite is a defined type and a method answering it would
// put an mls type on the interface, which is the one thing section 6's block never does.
func (self *connectMlsEngine) Suite() uint16 {
	return uint16(self.crypto.Suite())
}

// NewKeyPackage mints and publishes one key package for this device, persisting the two private
// halves against its reference so that whoever admits this device can be answered.
//
// THE LEAF NAMES device_sig, WHICH IS THE SAME KEY THIS ENGINE'S FOUNDING LEAVES NAME. That is
// what mls.NewKeyPackageWithSigner buys and it is the whole of why a join is possible at all:
// mls.JoinFromWelcome's caller-material gate compares the public half of the signing key a joiner
// holds against the signature_key its published leaf names, before one octet of the Welcome is
// judged, so a key package whose leaf named a key this device cannot sign with is a key package
// no Welcome addressed to it could ever be opened with. Before this, mls.NewKeyPackage drew its
// own signature key pair and the device published leaves under a key it did not hold, while
// founding groups under one it did -- one device, two identities, and nothing in the protocol that
// would ever report it.
//
// WHAT REACHES THE STORE IS UNCHANGED: the ref, the encoding, the init private and the encryption
// private. The signature key is NOT persisted here and the set of things this method persists that
// it did not persist before is empty: device_sig lives in the keyfile, per Spec A section 8.1, and
// PutKeyPackage still takes four arguments.
//
// ALL THREE PRIVATE HALVES ARE ERASED BEFORE THIS RETURNS, and each for its own reason. The key
// package holds a COPY of device_sig on its unexported seed -- the constructor clones, so this
// erase reaches the copy and never self.signer -- and a method that returned the encoding and
// dropped the value would leave the device's long term signing key in the heap for the collector
// to move around. The two HPKE halves are the ones mls.JoinKeyMaterial's own header names FIRST:
// the init key opens every Welcome addressed to this key package and the encryption key is this
// member's leaf key for as long as it holds that leaf. PutKeyPackage COPIES them, so erasing
// afterwards costs the store nothing -- and the erases are deferred rather than written before the
// put for exactly that reason: erased first, the store would hold zeros where the init private
// belongs and every Welcome addressed here would be unopenable.
func (self *connectMlsEngine) NewKeyPackage() ([]byte, error) {
	leafKeys, err := mls.ParseLeafKeysExtension(self.leafKeys)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrEngineLeafKeys, err)
	}
	leafKeysExtension, err := leafKeys.Encode()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrEngineLeafKeys, err)
	}
	keyPackage, initPrivate, encryptPrivate, err := mls.NewKeyPackageWithSigner(self.crypto,
		self.crypto.Suite(), self.signer, self.cred, engineCapabilities(),
		[]mls.Extension{leafKeysExtension})
	if err != nil {
		return nil, err
	}
	// AFTER the put, never before: see the header. Zeroize reaches the key package's own copy of
	// the seed and nothing else, and the two locals are this method's own.
	defer keyPackage.Zeroize()
	defer zeroize(initPrivate)
	defer zeroize(encryptPrivate)
	encoded, err := syntax.Marshal(keyPackage)
	if err != nil {
		return nil, err
	}
	ref, err := keyPackage.Ref(self.crypto)
	if err != nil {
		return nil, err
	}
	if err := self.store.PutKeyPackage(ref, encoded, initPrivate, encryptPrivate); err != nil {
		return nil, err
	}
	return encoded, nil
}

// CreateGroup founds a one member group with this device at leaf 0.
//
// policy is the encoded urmessage_group_policy body and leafKeys the encoded urmessage_leaf_keys
// body, both as opaque octets: section 6's signature takes bytes so that the storage layer never
// names an mls extension type, and the tagging is done here where the tag and the body are put
// together in one statement.
func (self *connectMlsEngine) CreateGroup(groupId []byte, policy []byte, leafKeys []byte) (GroupHandle, error) {
	parsedLeafKeys, err := mls.ParseLeafKeysExtension(leafKeys)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrEngineLeafKeys, err)
	}
	group, err := mls.NewGroup(&mls.GroupConfig{
		Suite:   self.crypto.Suite(),
		GroupId: groupId,
		Extensions: []mls.Extension{{
			ExtensionType: mls.ExtensionTypeUrmessageGroupPolicy,
			ExtensionData: policy,
		}},
		RequiredCaps: engineRequiredCapabilities(),
		Crypto:       self.crypto,
		Store:        self.store,
		LeafKeys:     *parsedLeafKeys,
	}, self.signer, self.cred)
	if err != nil {
		return nil, err
	}
	return &connectMlsHandle{group: group}, nil
}

// LoadGroup reopens the group this device persisted at that epoch, and answers a handle no caller
// can tell from a founded or a joined one.
//
// THAT INDISTINGUISHABILITY IS THE WHOLE POINT AND IT IS WHY THE RETURN IS connectMlsHandle. The
// type this answers is the SAME type CreateGroup and JoinFromWelcome answer, so a restored group
// reaches every one of GroupHandle's methods through the same bodies a live group does -- Process
// and ApplyCommit included, which is the pair a foreign implementation of this interface cannot
// write: EngineProcessed.stagedRef is unexported, so only a member of this package can put a staged
// commit in one. A restored group can therefore INGEST A COMMIT and follow its group into the next
// epoch, which is the thing open item J1-8 named and which no adapter outside this package could
// have provided.
//
// THE CONFIG CARRIES ONLY WHAT THE LOAD READS, which is the discipline joinWithTakenKeyPackage
// states one method down and is checked the same way. mls.LoadGroup reads Crypto, Store and
// GroupId; Suite, Extensions, RequiredCaps and LeafKeys are CreateGroup's four and are unread here,
// because a restore rebuilds the group context out of the persisted blob rather than out of a
// caller's intent. A field set here that nothing reads is a field a later reader believes is load
// bearing.
//
// THE GROUP ID IS CLONED ON THE WAY IN for CreateGroup's reason inverted: this hands a caller's
// array to an object mls goes on holding, and a group id that moved underneath a restored group is
// every secret of that group derived over something else.
//
// AND THE EPOCH IT CAME BACK AT IS REFUSED BY NAME WHEN IT IS NOT THE EPOCH THAT WAS ASKED FOR.
// mls.LoadGroup does not make that comparison and says so -- it reads the blob the store answers at
// the key it was given and rebuilds whatever is in it -- so a store that answers the wrong epoch's
// state produces a group that is internally consistent, exports real secrets, and is at an epoch
// the caller never asked for. Nothing downstream can see the difference: the session built over it
// derives a perfectly good storage root at the WRONG epoch, seals records under it, and every peer
// refuses them for a reason this device cannot name. sdk's own restore made this comparison before
// this method existed and it is kept HERE so that every caller of the interface gets it rather than
// the one caller that remembered.
func (self *connectMlsEngine) LoadGroup(groupId []byte, epoch uint64) (GroupHandle, error) {
	group, err := mls.LoadGroup(&mls.GroupConfig{
		Crypto:  self.crypto,
		Store:   self.store,
		GroupId: append([]byte(nil), groupId...),
	}, epoch, self.signer)
	if err != nil {
		return nil, err
	}
	handle := &connectMlsHandle{group: group}
	if loaded := handle.Epoch(); loaded != epoch {
		// closed rather than leaked: the restored group holds a live epoch schedule and a leaf
		// private state, and a handle nobody is answered is a handle nobody can Close.
		handle.Close()
		return nil, fmt.Errorf("%w: group %x was asked for at epoch %d and the state that came back stands at epoch %d",
			ErrEngineLoadedEpoch, groupId, epoch, loaded)
	}
	return handle, nil
}

// JoinFromWelcome builds this device's group state out of a Welcome addressed to a key package it
// published and the ratchet tree the sender anchored.
//
// WHERE THE REF COMES FROM, because section 6's signature does not carry one. JoinFromWelcome
// names no key package, NewKeyPackage returns no ref, and StateStore.TakeKeyPackage demands one.
// THE WELCOME ITSELF CARRIES THEM: Welcome.Secrets[i].NewMember IS the KeyPackageRef that entry is
// addressed to, and every type on that path is exported. So no new connect/mls surface is needed
// and none is added -- no refs helper, no store enumeration, no third parameter on section 6's
// method. Rejected: the engine remembering the refs it minted, because that state does not survive
// a restart and would be a second, divergent copy of what the store already holds.
//
// EXACTLY THE ONE REF THIS STORE HOLDS, and neither the first nor all of them. The refs a Welcome
// names are addressed to DIFFERENT joiners: a device that took on the first is asking a question
// of somebody else's entry, and a device that took on every one is destroying other entries'
// addressing for no reason.
//
// THE TAKE IS DESTRUCTIVE AND THE POSITION IS TAKE-AND-PUT-BACK. TakeKeyPackage reads and deletes
// in one call and there is no non-destructive read on the eight-method interface. A Welcome
// authenticates nobody -- mls.JoinFromWelcome's own header spends fifteen lines on it, and
// GroupEngine's header states what that leaves the CALLER of this method owing, which is open item
// MG-1 -- so anybody holding this device's published key package can seal a well formed one to it,
// and a joiner that took and then failed would have consumed the device's only copy: the
// legitimate Welcome could never be opened. This body puts the entry back on EVERY failure path after the
// take, so a bogus Welcome costs a store round trip rather than the device's only copy.
// Rejected: taking only after a successful join, which this interface cannot express, because
// mls.JoinFromWelcome needs the material in order to decide. THE WINDOW THIS LEAVES AND DOES NOT
// CLOSE: a crash between the take and the put-back loses the key package permanently, and the
// device must publish a fresh one and be re-added. That is the store's to close, with a
// Get/Delete split or a normative sentence, and not this method's.
//
// THE REFUSAL CARRIES WHAT THE INTERFACE CANNOT SAY. TakeKeyPackage answers a bare error with no
// declared not-found value, so a loop that treated every error as "not mine" reports a broken disk
// as an unaddressed Welcome. ErrEngineNoKeyPackageForWelcome therefore carries the ref count, the
// refusal count and the LAST STORE ERROR VERBATIM, so an operator reading the message can tell the
// two apart even though a caller matching on the type cannot. The taxonomy that would let the type
// tell them apart is owed by whoever owns the store interface.
func (self *connectMlsEngine) JoinFromWelcome(welcome []byte, ratchetTree []byte) (GroupHandle, error) {
	parsed, err := mls.ParseMLSMessage(welcome)
	if err != nil {
		return nil, fmt.Errorf("%w: %d octets do not decode as an MLSMessage: %w",
			ErrEngineWelcomeShape, len(welcome), err)
	}
	if parsed.Welcome == nil {
		return nil, fmt.Errorf("%w: the %d octet message decodes and carries no welcome arm",
			ErrEngineWelcomeShape, len(welcome))
	}
	refused := 0
	var lastStoreRefusal error
	for _, addressed := range parsed.Welcome.Secrets {
		encoded, initPrivate, encryptPrivate, takeErr := self.store.TakeKeyPackage(addressed.NewMember)
		if takeErr != nil {
			refused += 1
			lastStoreRefusal = takeErr
			continue
		}
		handle, joinErr := self.joinWithTakenKeyPackage(welcome, ratchetTree, encoded,
			initPrivate, encryptPrivate)
		if joinErr != nil {
			// the put-back, on EVERY failure after the take rather than on a named list of
			// them: an allow-list of failure reasons is the shape that fails open the day
			// connect/mls adds a check.
			if putErr := self.store.PutKeyPackage(addressed.NewMember, encoded, initPrivate,
				encryptPrivate); putErr != nil {
				return nil, fmt.Errorf("%w: and the key package it was taken from could not be put back: %w",
					joinErr, putErr)
			}
			return nil, joinErr
		}
		return handle, nil
	}
	return nil, fmt.Errorf("%w: the welcome names %d key package refs, this store refused %d of them, and the last refusal it answered was %v",
		ErrEngineNoKeyPackageForWelcome, len(parsed.Welcome.Secrets), refused, lastStoreRefusal)
}

// joinWithTakenKeyPackage assembles the join material out of ONE taken entry and hands it to
// connect/mls.
//
// EVERY FIELD OF THE MATERIAL IS A COPY THIS METHOD MADE, and that sentence is the whole reason
// this helper exists as its own paragraph. mls.JoinKeyMaterial OWNS every array it carries:
// (*JoinKeyMaterial).Zeroize erases InitPrivate, EncryptPrivate, SignPrivate and the key package's
// own retained seed, and this body must call it -- the two HPKE halves open every Welcome
// addressed to this key package and the signing key is the device's identity, which is that type's
// own header. So the material is assembled over four copies and the erase destroys COPIES.
//
// THE TWO HPKE HALVES because TakeKeyPackage answered the STORE'S OWN arrays: a body that
// assembled over them and then erased would put zeroed octets back on the put-back path.
//
// AND SignPrivate BECAUSE IT IS device_sig, WHICH IS THE ONE THAT COSTS SOMETHING. A material
// assembled directly over self.signer loses the device's long term signing key on the first
// successful join, and NOTHING ANYWHERE REFUSES AFTERWARDS: zeroizeSecret writes zeros through the
// slice, signaturePublicKeyOf accepts an all-zero seed, NewKeyPackage and CreateGroup both go on
// succeeding, and every leaf this device publishes afterwards names the ed25519 public key of the
// all-zero seed -- derivable by anyone -- while its credential still names the real device. The
// joined handle goes on working too, because connect/mls clones SignPrivate into the group before
// the erase. The defensive copy is a fourth instance of a discipline this path already spells
// three times: the key package constructor clones the caller's seed, and both the join and the
// founder clone it into the group.
//
// Rejected: narrowing (*JoinKeyMaterial).Zeroize so it leaves SignPrivate alone, which removes a
// real erase from the type that declares this material for EVERY caller and contradicts its own
// header. Rejected: not calling Zeroize at all, which leaves the two HPKE halves in the heap and
// buys nothing.
func (self *connectMlsEngine) joinWithTakenKeyPackage(welcome []byte, ratchetTree []byte,
	encoded []byte, initPrivate []byte, encryptPrivate []byte) (GroupHandle, error) {

	var keyPackage mls.KeyPackage
	if err := syntax.Unmarshal(encoded, &keyPackage); err != nil {
		return nil, fmt.Errorf("%w: the store answered %d octets under a ref this welcome names and they do not decode as a key package: %w",
			ErrEngineWelcomeShape, len(encoded), err)
	}
	keys := &mls.JoinKeyMaterial{
		KeyPackage:     keyPackage,
		InitPrivate:    append(mls.HpkePrivateKey(nil), initPrivate...),
		EncryptPrivate: append(mls.HpkePrivateKey(nil), encryptPrivate...),
		SignPrivate:    append(mls.SignaturePrivateKey(nil), self.signer...),
	}
	// it destroys COPIES: see the header. Deferred rather than written after the call because
	// mls.JoinFromWelcome refuses at some fifteen places and every one of them is an exit.
	defer keys.Zeroize()
	// THE CONFIG CARRIES ONLY WHAT THE JOIN READS. mls.JoinFromWelcome's body reads Crypto,
	// Store, Profile -- defaulted if nil -- and GroupId, and GroupId ONLY as an intent match the
	// caller opts into. Section 6's signature gives this engine no group id to intend, so it is
	// left unset: a config that guessed one would refuse every legitimate Welcome, and a config
	// that recovered one from the message it is about to judge would turn an intent match into a
	// tautology, which is worse than leaving it unset because it reads like a check. Suite,
	// Extensions, RequiredCaps and LeafKeys are the four CreateGroup sets and this must not --
	// they are unread on this path, because required capabilities come off the Welcome's own
	// GroupInfo.
	group, err := mls.JoinFromWelcome(&mls.GroupConfig{
		Crypto: self.crypto,
		Store:  self.store,
	}, welcome, ratchetTree, keys)
	if err != nil {
		return nil, err
	}
	return &connectMlsHandle{group: group}, nil
}

// engineCapabilities is what every leaf this engine publishes advertises.
//
// Suites() rather than the one code point this engine runs, because the vector says what this
// device CAN do rather than what one group does: a leaf advertising only its own group's suite is
// a leaf no other suite could ever add. The two private use extension types are listed because
// every leaf carries urmessage_leaf_keys and every group context carries urmessage_group_policy,
// and RFC 9420 section 7.3 refuses a leaf that carries or meets a type it does not list.
//
// The proposal vector is empty and that is the CONFORMING answer rather than a gap: add, update
// and remove are section 7.2 default types, which section 7.2 forbids a leaf to list.
func engineCapabilities() mls.Capabilities {
	return mls.Capabilities{
		Versions:     []mls.ProtocolVersion{mls.ProtocolVersionMls10},
		CipherSuites: mls.Suites(),
		Extensions: []mls.ExtensionType{
			mls.ExtensionTypeUrmessageGroupPolicy,
			mls.ExtensionTypeUrmessageLeafKeys,
		},
		Proposals:   []mls.ProposalType{},
		Credentials: []mls.CredentialType{mls.CredentialTypeBasic},
	}
}

// engineRequiredCapabilities is what every group this engine founds requires of a joiner: the two
// private use extension types the profile puts on every leaf and every group context.
func engineRequiredCapabilities() mls.RequiredCapabilities {
	return mls.RequiredCapabilities{
		ExtensionTypes: []mls.ExtensionType{
			mls.ExtensionTypeUrmessageGroupPolicy,
			mls.ExtensionTypeUrmessageLeafKeys,
		},
	}
}

// connectMlsHandle wraps exactly one *mls.Group.
//
// It is the one production declaration in this package whose type mentions mls.Group, and
// engine_test.go derives that class off the syntax tree and holds it to this file BY SCANNED PATH
// rather than by base name -- a base name exemption is the exemption shape this project keeps
// rediscovering.
type connectMlsHandle struct {
	group *mls.Group
}

// GroupId is the group's identifier, as storage the caller owns: mls.Group already answers a copy.
func (self *connectMlsHandle) GroupId() []byte {
	return self.group.GroupId()
}

// Epoch is the epoch this handle is at.
func (self *connectMlsHandle) Epoch() uint64 {
	return self.group.Epoch()
}

// OwnLeafIndex is this device's leaf, converted out of mls.LeafIndex at the boundary.
//
// The conversion runs one way and only here. mls.LeafIndex is a defined type, so a widening of
// this result to it would make *mls.Group fit the interface -- which is the reshape section 6
// exists to refuse, because an interface that names mls's types is a re-export rather than a seam.
func (self *connectMlsHandle) OwnLeafIndex() uint32 {
	return uint32(self.group.OwnLeafIndex())
}

// MemberCount is how many members the group has.
func (self *connectMlsHandle) MemberCount() int {
	return len(self.group.Members())
}

// MemberAt projects one member down to section 6's three byte slices.
//
// i is an ORDINAL into the membership snapshot and not a leaf index, which is section 6's own
// signature: MemberCount and MemberAt are a pair, and a caller walking 0..MemberCount-1 must not
// have to know which leaves are blank.
//
// EVERY ABSENCE IS AN ERROR AND NEVER A ZERO VALUE. An ordinal off the end is a refusal, and a
// member whose leaf carries no urmessage_leaf_keys extension is a refusal too: a projection that
// answered a nil leafKeys would hand the epoch fan out a member it silently cannot wrap to, and a
// projection that dropped mls.MemberAt's bool would turn a missing member into leaf 0.
func (self *connectMlsHandle) MemberAt(i int) (uint32, []byte, []byte, error) {
	members := self.group.Members()
	if i < 0 || len(members) <= i {
		return 0, nil, nil, fmt.Errorf("%w: ordinal %d of %d members", ErrEngineMemberOrdinal, i, len(members))
	}
	member := members[i]
	if member.LeafKeys == nil {
		return 0, nil, nil, fmt.Errorf("%w: the member at ordinal %d carries no urmessage_leaf_keys extension",
			ErrEngineMemberLeafKeys, i)
	}
	leafKeys, err := member.LeafKeys.Encode()
	if err != nil {
		return 0, nil, nil, fmt.Errorf("%w: %w", ErrEngineMemberLeafKeys, err)
	}
	return uint32(member.LeafIndex), member.IdentityPub, leafKeys.ExtensionData, nil
}

// RoleAt projects one member's credential identity and its role, KEYED BY LEAF INDEX.
//
// IT READS THE SAME self.group.Members() SNAPSHOT MemberAt READS AND TAKES A SECOND FIELD OFF IT.
// membersLocked already runs GroupPolicyOf over the group context's extension list and sets
// Member.Role from it, so the policy is parsed exactly once per snapshot and this method computes
// nothing: it selects. The two readings of an unnamed identity connect/mls used to hold -- RoleOf
// without its bool answering OBSERVER, Members() answering MEMBER -- were closed to MEMBER at R1,
// and taking the role off Members() is what keeps this door on the same side of that.
//
// THE LEAF KEYS EXTENSION IS NOT REQUIRED HERE, which is the one way this differs from MemberAt
// beyond the key. MemberAt refuses a member with no urmessage_leaf_keys because its caller is
// about to wrap an epoch secret to that key and a nil there is a member silently left out of the
// epoch. A ROLE is not a wrap target: a leaf admitted without leaf keys is a member with a role,
// it is exactly the leaf the receiving arm's R6d refuses a commit for, and refusing to say what
// role it holds would hide the member rather than the defect.
func (self *connectMlsHandle) RoleAt(leaf uint32) ([]byte, string, error) {
	for _, member := range self.group.Members() {
		if uint32(member.LeafIndex) != leaf {
			continue
		}
		return member.IdentityPub, member.Role.String(), nil
	}
	return nil, "", fmt.Errorf("%w: leaf %d at epoch %d", ErrEngineMemberLeaf, leaf, self.group.Epoch())
}

// Export is RFC 9420 section 8.5's exporter, which MASTER section 7 derives mls_secret from.
//
// It is the one thing on this interface that reaches the epoch's secrets at all, and it reaches
// them through a label the caller names -- which is what keeps epoch_secret itself off the surface.
func (self *connectMlsHandle) Export(label string, context []byte, length int) ([]byte, error) {
	return self.group.Export(label, context, length)
}

// PairwiseExport is Export's pairwise sibling, ledger item 228. The whole of the conversion this
// adapter makes is the leaf index: uint32 on the seam, mls.LeafIndex behind it, and the refusal for
// a position that holds no member comes back from mls as ErrBlankLeaf rather than being invented
// here out of a MemberAt walk that would be a second reading of the same tree.
func (self *connectMlsHandle) PairwiseExport(label string, peer uint32, length int) ([]byte, error) {
	return self.group.PairwiseExport(label, mls.LeafIndex(peer), length)
}

// SenderDataSecret is MASTER section 8.2's sender_data secret.
//
// It names mls's closed EpochSecretName enum, and this file is the only place in the tree that
// names either constant. That is guardrail G6 seen from the client half: EpochSecret is not on
// GroupHandle precisely so epoch_secret, confirmation_key and membership_key cannot be reached
// from a package that holds the record keys.
func (self *connectMlsHandle) SenderDataSecret() ([]byte, error) {
	return self.group.EpochSecret(mls.EpochSecretSenderData)
}

// EncryptionSecret is MASTER section 8.2's encryption secret, through the same closed enum.
func (self *connectMlsHandle) EncryptionSecret() ([]byte, error) {
	return self.group.EpochSecret(mls.EpochSecretEncryption)
}

// EpochAuthenticator is the value two members compare to detect a fork, or nil for a closed group.
//
// Section 6 gives it no error and this adapter does not invent one: nil is mls's own answer for a
// closed group, and a nil authenticator compared against a nil authenticator is a comparison the
// caller has to refuse rather than a value this layer can improve.
func (self *connectMlsHandle) EpochAuthenticator() []byte {
	return self.group.EpochAuthenticator()
}

// RatchetTreeSnapshot is the encoded public tree, which MASTER section 8.2's per epoch snapshot
// record carries. The name is this plan's and mls's is RatchetTree; the divergence is naming only.
func (self *connectMlsHandle) RatchetTreeSnapshot() ([]byte, error) {
	return self.group.RatchetTree()
}

// GroupContextBytes is the serialized GroupContext for the current epoch. mls's name is
// GroupContext; the divergence is naming only, and the two bodies are NOT interchangeable even
// though both answer opaque octets.
func (self *connectMlsHandle) GroupContextBytes() ([]byte, error) {
	return self.group.GroupContext()
}

// ProposeAdd publishes an Add proposal for one encoded key package.
func (self *connectMlsHandle) ProposeAdd(keyPackage []byte) ([]byte, error) {
	return self.group.ProposeAdd(keyPackage)
}

// ProposeRemove publishes a Remove proposal for one leaf, converting at the boundary.
func (self *connectMlsHandle) ProposeRemove(leafIndex uint32) ([]byte, error) {
	return self.group.ProposeRemove(mls.LeafIndex(leafIndex))
}

// ProposeUpdate publishes an Update proposal for this device's own leaf.
func (self *connectMlsHandle) ProposeUpdate() ([]byte, error) {
	return self.group.ProposeUpdate()
}

// ProposeGroupPolicy publishes a GroupContextExtensions proposal carrying the group's CURRENT
// extension list with only the policy replaced.
//
// This is the one method of the thirteen whose body is not a projection: mls takes a vector of
// tagged extensions and section 6 takes the body alone, so the 0xF001 tag is applied here. Pairing
// the body with the tag in one statement is what keeps a caller from pairing it with another.
//
// REPAIRED 2026-09-21, ledger item 242's P4. This body used to hand mls a ONE-ENTRY list, and
// RFC 9420 section 12.1.6's proposal replaces the group's list WHOLESALE -- mls's own header on
// ProposeGroupContextExtensions says so -- so every policy proposal this seam ever published
// stripped 0x0003 required_capabilities from the group, measured: after the proposal was committed
// the context carried the policy and nothing else. The current list is read out of the group's
// own context, through the same decode every other reader of it uses, and only the 0xF001 entry is
// replaced; mls.ExtensionsWithGroupPolicy is the one helper, shared with CommitPolicy, so the two
// doors cannot disagree about which entries survive a policy change -- and it lives in mls
// rather than here because that package owns what a policy is and owns the one door that refuses
// a list carrying two of them.
func (self *connectMlsHandle) ProposeGroupPolicy(policy []byte) ([]byte, error) {
	current, err := self.currentExtensions()
	if err != nil {
		return nil, err
	}
	replaced, err := mls.ExtensionsWithGroupPolicy(current, policy)
	if err != nil {
		return nil, err
	}
	return self.group.ProposeGroupContextExtensions(replaced)
}

// Commit builds a commit over the proposals named by reference, projecting *mls.CommitResult to
// section 6's three byte slices.
//
// The commit is STAGED and not merged. mls stages on both sides for the reason MASTER section 9.3
// gives: the delivery service accepts at most one commit per (group, epoch), so a committer that
// merged optimistically would fork itself off the group.
func (self *connectMlsHandle) Commit(byReference [][]byte) ([]byte, []byte, []byte, error) {
	result, err := self.group.CreateCommit(byReference, nil, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	return result.Commit, result.Welcome, result.RatchetTree, nil
}

// CommitAdd builds a commit carrying one by-value Add per key package. See the interface for why
// the arm exists; what is decided here is what the adapter checks before mls is reached.
//
// THE KEY PACKAGE IS DECODED HERE, from the caller's octets, because mls.Add carries a structure
// and section 6's signature carries octets. syntax.Unmarshal copies every opaque field it reads,
// so the proposal shares no array with the caller's buffer -- the same property ProposeAdd states
// for its own decode, and the reason a caller's later write through keyPackages[i] reaches nothing
// this commit signed.
//
// urmessage_leaf_keys IS REQUIRED HERE, AND IT IS THE ONE CHECK mls's LIST RULES DO NOT MAKE.
// ValidateProposalList runs inside CreateCommit over every by-value Add -- ValSem105 is
// (*KeyPackage).Validate, so an expired, wrong-suite or wrongly signed package is refused by mls
// without this adapter restating section 10.1 -- but the v1 wrap target is this profile's and not
// RFC 9420's. ProposeAdd asks LeafKeysOf at generation for the reason its header gives: a joiner
// whose leaf carries none is a member no epoch wrap can reach, and the first symptom is that
// member reading nothing one commit later. The by-value arm admits a member without ever passing
// through ProposeAdd, so the question is asked again here, once per package, before anything is
// staged. Rejected: asking it in mls's list rules, where it would judge every Add of every
// profile by a v1 extension type.
//
// THE BY-REFERENCE VECTOR IS EMPTY AND NOT NIL. CreateCommit reads nil as "every proposal cached
// for this epoch" and an empty slice as "none of them", and the interface's contract is the
// second. A caller that never saw a proposal can process what this builds precisely because
// nothing it builds names one.
//
// NOTHING IS STAGED ON A REFUSAL. Every return before CreateCommit leaves the group where it
// stood, and CreateCommit's own refusals do the same -- self.pending is written only at its end.
func (self *connectMlsHandle) CommitAdd(keyPackages [][]byte) ([]byte, []byte, []byte, error) {
	if len(keyPackages) == 0 {
		return nil, nil, nil, fmt.Errorf("%w: no key packages", ErrEngineCommitAddEmpty)
	}
	byValue := make([]mls.Proposal, 0, len(keyPackages))
	for i, encoded := range keyPackages {
		var keyPackage mls.KeyPackage
		if err := syntax.Unmarshal(encoded, &keyPackage); err != nil {
			return nil, nil, nil, fmt.Errorf("%w: key package %d of %d does not decode: %w",
				ErrEngineCommitAddKeyPackage, i, len(keyPackages), err)
		}
		if _, err := mls.LeafKeysOf(&keyPackage.LeafNode); err != nil {
			return nil, nil, nil, fmt.Errorf("%w: key package %d of %d: %w",
				ErrEngineCommitAddKeyPackage, i, len(keyPackages), err)
		}
		byValue = append(byValue, mls.Proposal{
			ProposalType: mls.ProposalTypeAdd,
			Add:          &mls.Add{KeyPackage: keyPackage},
		})
	}
	result, err := self.group.CreateCommit([][]byte{}, byValue, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	return result.Commit, result.Welcome, result.RatchetTree, nil
}

// CommitContextExtensions builds a commit carrying one by-value GroupContextExtensions proposal
// whose list is EXACTLY the one it was handed. See the interface for what wholesale means and for
// what this adapter deliberately does not judge; what is decided here is the shape.
//
// THE BODIES ARE CLONED ON THE WAY IN, which is CommitAdd's property stated for a list rather than
// a decode: mls copies every by-value proposal through the codec, so nothing it stages aliases
// the caller's arrays either way, and the clone here is what makes that a fact about this method
// rather than about mls's current body.
//
// AN EMPTY LIST IS REFUSED BY NAME, as CommitAdd refuses an empty vector: RFC 9420 lets a group
// carry no extensions, and this profile does not -- a group with no policy has no owner -- so the
// one list that can never be the caller's intent is refused before anything is staged, and every
// other list is mls's to judge. THE BY-REFERENCE VECTOR IS EMPTY AND NOT NIL, for CommitAdd's
// reason, and nothing is staged on a refusal, for its reason.
func (self *connectMlsHandle) CommitContextExtensions(extensions []ExtensionBytes) ([]byte, []byte, []byte, error) {
	if len(extensions) == 0 {
		return nil, nil, nil, fmt.Errorf("%w: no extensions", ErrEngineCommitContextExtensionsEmpty)
	}
	result, err := self.group.CreateCommit([][]byte{}, []mls.Proposal{{
		ProposalType:           mls.ProposalTypeGroupContextExtensions,
		GroupContextExtensions: &mls.GroupContextExtensions{Extensions: mlsExtensionsOf(extensions)},
	}}, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	return result.Commit, result.Welcome, result.RatchetTree, nil
}

// CommitPolicy is CommitContextExtensions over the group's current list with only 0xF001
// replaced: the door every role change goes through. The list is read out of this handle's own
// context and the replacement is mls.ExtensionsWithGroupPolicy, the same helper
// ProposeGroupPolicy uses, so a policy committed by value and one proposed by reference leave the
// same entries standing.
func (self *connectMlsHandle) CommitPolicy(policy []byte) ([]byte, []byte, []byte, error) {
	current, err := self.currentExtensions()
	if err != nil {
		return nil, nil, nil, err
	}
	replaced, err := mls.ExtensionsWithGroupPolicy(current, policy)
	if err != nil {
		return nil, nil, nil, err
	}
	return self.CommitContextExtensions(extensionBytesOf(replaced))
}

// CommitRemove builds a commit carrying one by-value Remove per leaf. See the interface for why it
// exists and for what the sdk does not build over it.
//
// THE CONVERSION IS THE SEAM: a uint32 becomes an mls.LeafIndex here and nowhere else, which is
// this file's header rule. Nothing else is judged here, because mls already judges everything a
// Remove can be wrong about -- ValSem108 refuses a blank leaf and one outside the tree, and
// validateCommitterIsNotRemoved refuses this member's own leaf -- and the one thing it does not
// refuse, an empty vector, is refused by name for CommitAdd's reason: a commit with no proposal
// and a path is a legitimate MLS commit that removes nobody, and that is never what a caller of
// this method meant. THE BY-REFERENCE VECTOR IS EMPTY AND NOT NIL, and nothing is staged on a
// refusal, both for CommitAdd's reasons.
func (self *connectMlsHandle) CommitRemove(leaves []uint32) ([]byte, []byte, []byte, error) {
	if len(leaves) == 0 {
		return nil, nil, nil, fmt.Errorf("%w: no leaves", ErrEngineCommitRemoveEmpty)
	}
	result, err := self.group.CreateCommit([][]byte{}, removeProposals(leaves), nil)
	if err != nil {
		return nil, nil, nil, err
	}
	return result.Commit, result.Welcome, result.RatchetTree, nil
}

// removeProposals is one by-value Remove per leaf, in the caller's order. It is a declaration of
// its own because two arms build it -- CommitRemove and CommitRemoveWithExtensions -- and a
// second transcription of the conversion would be a second place the seam's uint32 becomes an
// mls.LeafIndex, which this file's header rule allows in exactly one.
func removeProposals(leaves []uint32) []mls.Proposal {
	byValue := make([]mls.Proposal, 0, len(leaves))
	for _, leaf := range leaves {
		byValue = append(byValue, mls.Proposal{
			ProposalType: mls.ProposalTypeRemove,
			Remove:       &mls.Remove{Removed: mls.LeafIndex(leaf)},
		})
	}
	return byValue
}

// CommitRemoveWithExtensions builds ONE commit carrying every Remove and then the wholesale
// GroupContextExtensions, which is the shape a removal ships on. See the interface for why a bare
// CommitRemove cannot carry a named identity's last leaf and why this is two parameters rather
// than a proposal list.
//
// THE ORDER IS REMOVE FIRST AND THE EXTENSIONS LAST, AND IT IS FIXED HERE RATHER THAN LEFT TO THE
// CALLER. Both orders build, both are followed by an honest receiver, and the state they leave is
// the same one: RFC 9420 section 12.3 applies proposals by TYPE -- Update, then Remove, then Add,
// then GroupContextExtensions -- so the vector's order reaches neither the tree nor the extension
// list, and measured through this seam both orders answer the same RemovedLeaves, the same
// MembersAfter and a byte-identical post-commit extension list at every receiver.
//
// WHAT THE ORDER DOES REACH IS THE CONFIRMED TRANSCRIPT HASH. The commit's own proposal vector is
// inside the FramedContent that RFC 9420 section 8.2's ConfirmedTranscriptHashInput is taken
// over, so two clients building "the same" removal in two orders sign two different commits and
// the epoch has two names. Measured rather than reasoned, because a removal's own commit cannot
// show it: section 12.4 forces an update path on any commit carrying a Remove, and the path draws
// fresh secrets every time, so the SAME removal built twice from one state already answers two
// confirmed transcript hashes. The shape that isolates the order is the one commit section 12.4
// leaves pathless -- Adds only -- where the same vector built twice answers one hash and the
// swapped vector answers another;
// TestTheProposalOrderOfACommitIsWhatTheConfirmedTranscriptHashIsTakenOver drives both halves,
// and the equal pair is the control that says the difference is the order and not the draw.
//
// SO A KAT OVER THIS ARM IS A KAT OVER THE VECTOR, not over the octets: what is pinned is that
// the removal and the policy leave in one commit in one order, and the arm is the one place that
// order is decided.
//
// BOTH EMPTIES ARE REFUSED BY NAME, each with the sentinel of the arm that owns the refusal, and
// nothing is staged on either: an empty leaf vector is CommitRemove's refusal (a commit with a
// path and no proposal removes nobody) and an empty list is CommitContextExtensions' (RFC 9420
// section 12.1.6 replaces the list wholesale, so an empty one is a group with no policy). THE
// BY-REFERENCE VECTOR IS EMPTY AND NOT NIL, for CommitAdd's reason.
func (self *connectMlsHandle) CommitRemoveWithExtensions(leaves []uint32,
	extensions []ExtensionBytes) ([]byte, []byte, []byte, error) {

	if len(leaves) == 0 {
		return nil, nil, nil, fmt.Errorf("%w: and the combining arm refuses it for that reason too",
			ErrEngineCommitRemoveEmpty)
	}
	if len(extensions) == 0 {
		return nil, nil, nil, fmt.Errorf("%w: and the combining arm refuses it for that reason too",
			ErrEngineCommitContextExtensionsEmpty)
	}
	result, err := self.group.CreateCommit([][]byte{},
		removeWithExtensionsProposals(leaves, extensions), nil)
	if err != nil {
		return nil, nil, nil, err
	}
	return result.Commit, result.Welcome, result.RatchetTree, nil
}

// removeWithExtensionsProposals is the combining arm's by-value vector: every Remove, then the
// one GroupContextExtensions.
//
// IT IS A DECLARATION OF ITS OWN SO THAT THE ORDER IS A VALUE SOMETHING CAN READ. The order is a
// property of the commit this seam signs -- see the arm's header -- and an order built inline at
// the CreateCommit call would be a property only a reader could check.
// TestTheCombiningArmBuildsTheRemovesFirstAndTheExtensionsLast asks this function for its answer
// and reads the types off it, so a flip goes red on the vector rather than on a hash nobody can
// hold two builds of.
func removeWithExtensionsProposals(leaves []uint32, extensions []ExtensionBytes) []mls.Proposal {
	byValue := removeProposals(leaves)
	return append(byValue, mls.Proposal{
		ProposalType:           mls.ProposalTypeGroupContextExtensions,
		GroupContextExtensions: &mls.GroupContextExtensions{Extensions: mlsExtensionsOf(extensions)},
	})
}

// currentExtensions is this handle's group-context extension list at the current epoch, decoded
// out of the same octets GroupContextBytes answers.
//
// THROUGH THE ENCODING AND NOT THROUGH AN ACCESSOR, for two reasons that agree. mls exports no
// extension-list accessor and this adapter adds none, because framedApplicationLength and
// peekWithGroupSecrets already read the context this way and a third reader through a new door
// would be a second answer to the same question; and syntax.Unmarshal copies every opaque field
// it reads, so what this answers shares no array with the epoch the group is running.
func (self *connectMlsHandle) currentExtensions() ([]mls.Extension, error) {
	contextBytes, err := self.group.GroupContext()
	if err != nil {
		return nil, err
	}
	context := &mls.GroupContext{}
	if err := syntax.Unmarshal(contextBytes, context); err != nil {
		return nil, err
	}
	return context.Extensions, nil
}

// extensionBytesOf projects an mls extension list onto the seam's type, every entry in its
// position and every body cloned, in the one direction the boundary allows. A nil list answers
// nil, so "the staged value carries no list" and "an empty list" keep their difference.
//
// IT SELECTS NOTHING, and the per-entry projection is a declaration of its own so that this stays
// visibly so: extensionBytes is handed one whole entry and reads its tag off that, which is the
// shape mls's extension-selection gate exempts because a declaration handed a whole entry cannot
// choose between two of them. A walk here that read a tag out of the vector to skip or pick an
// entry would be a selection, and that gate would report it for a row.
func extensionBytesOf(extensions []mls.Extension) []ExtensionBytes {
	if extensions == nil {
		return nil
	}
	out := make([]ExtensionBytes, len(extensions))
	for at, extension := range extensions {
		out[at] = extensionBytes(extension)
	}
	return out
}

// extensionBytes is one whole entry, projected: the tag becomes a uint16 here and nowhere else,
// and the body is a copy.
func extensionBytes(extension mls.Extension) ExtensionBytes {
	return ExtensionBytes{
		Type: uint16(extension.ExtensionType),
		Data: append([]byte(nil), extension.ExtensionData...),
	}
}

// mlsExtensionsOf is extensionBytesOf's inverse, for the one door that takes a list in: the type
// becomes an mls.ExtensionType here and nowhere else, and every body is cloned.
func mlsExtensionsOf(extensions []ExtensionBytes) []mls.Extension {
	out := make([]mls.Extension, len(extensions))
	for at, extension := range extensions {
		out[at] = mls.Extension{
			ExtensionType: mls.ExtensionType(extension.Type),
			ExtensionData: append([]byte(nil), extension.Data...),
		}
	}
	return out
}

// PendingEpoch is the epoch this handle's own staged commit would open, projected off mls's
// three pending accessors onto the seam's value. See the interface for why it exists.
//
// EVERY FIELD IS READ OFF THE STAGED VALUE AND NOTHING OFF THE LIVE GROUP, which is the property
// that makes the method worth having: a projection that read MemberCount or GroupContext off
// the live handle would announce the epoch the group is already in, and the server would key
// the new epoch under the old context's hash and the old membership's count. mls's own accessors
// answer ErrNoPendingCommit when nothing is staged and this adapter passes that through, as it
// passes MergePendingCommit's through.
//
// REMOVEDLEAVES IS THE SHARPEST CASE OF THAT SAME RULE, and it is the field ledger item 257's
// ruling 51 added: the LIVE tree of a staged removal still holds the member being removed, so a
// reading off the live handle answers the empty set for every removal there is -- and the caller
// that reads this is the fan-out deciding which leaves NOT to seal the next epoch to. The live
// reading is not a stale answer there; it is the removed member handed the secret. mls's
// PendingRemovedLeaves reads the staged commit's own resolved vector, and the conversion back to
// this seam's uint32 happens here, which is this file's header rule.
func (self *connectMlsHandle) PendingEpoch() (*PendingEpoch, error) {
	epoch, err := self.group.PendingEpoch()
	if err != nil {
		return nil, err
	}
	memberCount, err := self.group.PendingMemberCount()
	if err != nil {
		return nil, err
	}
	removed, err := self.group.PendingRemovedLeaves()
	if err != nil {
		return nil, err
	}
	contextBytes, err := self.group.PendingGroupContext()
	if err != nil {
		return nil, err
	}
	removedLeaves := make([]uint32, 0, len(removed))
	for _, leaf := range removed {
		removedLeaves = append(removedLeaves, uint32(leaf))
	}
	return &PendingEpoch{
		Epoch:         epoch,
		MemberCount:   memberCount,
		RemovedLeaves: removedLeaves,
		GroupContext:  contextBytes,
	}, nil
}

// PendingExport is Export through the staged epoch's schedule: the same three go types in and
// the same fresh slice out, and the caller erases it as it erases Export's.
func (self *connectMlsHandle) PendingExport(label string, context []byte, length int) ([]byte, error) {
	return self.group.PendingExport(label, context, length)
}

// MergePendingCommit enters the epoch this handle's own staged commit opens.
func (self *connectMlsHandle) MergePendingCommit() error {
	return self.group.MergePendingCommit()
}

// ClearPendingCommit drops this handle's own staged commit.
func (self *connectMlsHandle) ClearPendingCommit() {
	self.group.ClearPendingCommit()
}

// Process ingests one MLS message, staging a commit rather than applying it.
//
// THE STAGED COMMIT GOES IN stagedRef AND NEVER IN Raw. Raw carries the message these values were
// read out of -- opaque octets, never inspected by this package -- and stagedRef carries the
// *mls.Processed together with the handle that staged it. Putting the staged value in Raw would
// hand this package a commit it could read and rebuild, which is exactly what section 6's
// unforgeability sentence is about.
func (self *connectMlsHandle) Process(message []byte) (*EngineProcessed, error) {
	processed, err := self.group.ProcessMessage(message)
	if err != nil {
		return nil, err
	}
	answer := &EngineProcessed{
		Kind: uint8(processed.Kind),
		// a copy, because the caller owns what it is handed and this package promises never
		// to look at it again.
		Raw:       append([]byte(nil), message...),
		stagedRef: &stagedProcessed{handle: self, processed: processed},
	}
	if processed.Kind == mls.ProcessedApplication {
		if processed.Application == nil {
			return nil, fmt.Errorf("%w: an application message with no application arm", ErrEngineProcessedArm)
		}
		answer.SenderLeaf = uint32(processed.Application.SenderLeaf)
		answer.Aad = processed.Application.AuthenticatedData
		answer.Plaintext = processed.Application.Plaintext
	}
	if processed.Kind == mls.ProcessedCommit {
		if processed.Commit == nil {
			return nil, fmt.Errorf("%w: a commit message with no commit arm", ErrEngineProcessedArm)
		}
		// what the commit DOES, off the staged commit its signature has already been verified
		// against. The accessors each hand back a fresh slice, so nothing here aliases the staged
		// value ApplyCommit is about to enter -- and these leave no key material to erase.
		answer.CommitterLeaf = uint32(processed.Commit.Committer())
		answer.AddedLeaves = leafIndexValues(processed.Commit.AddedLeaves())
		answer.RemovedLeaves = leafIndexValues(processed.Commit.RemovedLeaves())
		answer.UpdatedLeaves = leafIndexValues(processed.Commit.UpdatedLeaves())
		// WHO, beside where: item 242's R1. The committer's identity is read off the LIVE tree,
		// which is still the pre-commit tree because ProcessMessage moved no live state, and
		// the membership after is read off the staged tree. mls's Members clones the identity
		// it answers, so nothing here is a window onto either tree.
		committer, isMember := self.group.MemberAt(processed.Commit.Committer())
		if !isMember {
			// the staged epoch is erased before it is dropped, which is the erase discipline
			// and not a courtesy: this value holds a fully derived key schedule.
			processed.Commit.Zeroize()
			return nil, fmt.Errorf("%w: the commit's signature verified against leaf %d and the pre-commit tree holds no member there",
				ErrEngineCommitterUnknown, answer.CommitterLeaf)
		}
		answer.CommitterIdentity = committer.IdentityPub
		answer.MembersAfter = processedMembersOf(processed.Commit)
		extensions := processed.Commit.GroupContextExtensions()
		if extensions == nil {
			// the report a removed member is handed carries no context, and mls documents nil
			// there as "the list the group already had": this is where that is made so.
			current, err := self.currentExtensions()
			if err != nil {
				processed.Commit.Zeroize()
				return nil, err
			}
			extensions = current
		}
		answer.ContextExtensionsAfter = extensionBytesOf(extensions)
	}
	return answer, nil
}

// processedMembersOf is the staged tree's occupied leaves with their identities and their
// wrap-target facts, projected onto the seam's type in the one direction the boundary allows: an
// mls.LeafIndex becomes a uint32 here and nowhere else. The identities are the clones mls's
// accessor answers.
//
// EVERY FIELD IS READ OFF THE STAGED VALUE AND NOTHING OFF THE LIVE GROUP, which is why this
// takes the staged commit and nothing else: a leaf that exists before and after a commit carries
// the identity the commit LEFT there, and over the committer's own path that can differ from the
// one the live tree still holds -- TestMembersAfterNamesTheIdentityTheCommitLeavesAtTheCommitters
// Leaf builds that commit and holds this reading over it. A projection that took the identity
// off the live group wherever the leaf already existed would pass every honest commit and hand
// the authorizer the pre-commit identity at exactly the leaf whose change it must see.
func processedMembersOf(staged *mls.StagedCommit) []ProcessedMember {
	leaves := staged.OccupiedLeavesAfter()
	out := make([]ProcessedMember, 0, len(leaves))
	for _, leaf := range leaves {
		identity, held := staged.LeafIdentityAfter(leaf)
		if !held {
			continue
		}
		out = append(out, ProcessedMember{
			Leaf:        uint32(leaf),
			Identity:    identity,
			HasLeafKeys: staged.LeafHasKeysAfter(leaf),
		})
	}
	return out
}

// leafIndexValues projects the staged commit's leaf vectors onto the interface's own type, in
// the one direction the boundary allows -- an mls.LeafIndex becomes a uint32 here and nowhere else.
// A nil input answers nil rather than an empty non-nil slice, so a commit that adds nobody and one
// this package could not read are not told apart by the shape of the answer.
func leafIndexValues(leaves []mls.LeafIndex) []uint32 {
	if leaves == nil {
		return nil
	}
	out := make([]uint32, len(leaves))
	for at, leaf := range leaves {
		out[at] = uint32(leaf)
	}
	return out
}

// ApplyCommit enters the epoch a staged commit opens, and refuses anything this handle did not
// stage.
//
// The refusal is typed and is neither a panic nor a silent no-op. An EngineProcessed built by a
// keyed composite literal outside this package has a nil stagedRef -- that shape is legal go and
// section 6 says so -- and one staged by a DIFFERENT handle of this package carries another
// group's commit; both are refused here, so the guarantee is "the commit this handle staged" and
// not merely "some commit some engine staged".
//
// THE STAGED HALF IS DETACHED ON A SUCCESSFUL INSTALL, and that line is what makes
// DiscardProcessed after ApplyCommit a no-op rather than the erase of a live epoch. Once mls has
// answered nil the epoch the value staged is this handle's own, and the value the caller goes on
// holding must reach nothing of it: a later DiscardProcessed finds no staged half and answers
// nil, which is the shape every receiving arm writes -- `defer handle.DiscardProcessed(processed)`
// beside an ApplyCommit that then succeeds. mls detaches its own key material at the merge as
// well (see (*mls.Group).MergePendingCommit, 2026-09-21), so the two readings agree; this one is
// kept because it is the one this package's own door reads, and because it makes the discard's
// "nothing staged" arm reachable rather than dead. On mls.ErrRemovedFromGroup the value is left
// attached: the report a removed member is handed holds no key material -- stageInboundCommitLocked
// builds it without a schedule, a secret tree or a leaf private state -- so a discard of it erases
// nothing, and on every other refusal the value is still a staged epoch the caller owes an erase.
// A value handed back AFTER its install is refused by name, ErrEngineProcessedApplied, before mls
// is asked anything about it.
func (self *connectMlsHandle) ApplyCommit(processed *EngineProcessed) error {
	staged, err := self.stagedBy(processed)
	if err != nil {
		return err
	}
	if staged.processed == nil {
		return fmt.Errorf("%w: its staged half was released on the install", ErrEngineProcessedApplied)
	}
	if err := self.group.ApplyCommit(staged.processed); err != nil {
		return err
	}
	staged.processed = nil
	return nil
}

// DiscardProcessed erases the epoch a processed commit staged. See the interface for why a refused
// commit owes an erase; what is decided here is that the SAME three refusals ApplyCommit makes are
// made first, so a value this handle did not stage is neither installed nor erased through it.
//
// THE ERASE IS mls's OWN, (*StagedCommit).Zeroize, which is the erase every other drop site of a
// staged epoch already runs -- ClearPendingCommit's and Close's -- and which sets the flag
// (*Group).ApplyCommit reads FIRST, so a later ApplyCommit of the same value answers mls's
// errStagedCommitErased rather than installing zeros. No second flag is kept here: the one mls
// holds is the one its own door reads, and a copy on this side could only disagree with it.
//
// A VALUE APPLYCOMMIT INSTALLED HAS NO STAGED HALF, because ApplyCommit detached it, and the
// guard below is that arm: it answers nil and erases nothing. TestDiscardProcessedAfterA
// SuccessfulApplyCommitErasesNothing holds it, and before 2026-09-21 this guard was dead --
// nothing set the field to nil -- and this method erased the epoch the handle had just entered.
func (self *connectMlsHandle) DiscardProcessed(processed *EngineProcessed) error {
	staged, err := self.stagedBy(processed)
	if err != nil {
		return err
	}
	if staged.processed == nil {
		return nil
	}
	// the application and proposal arms stage no epoch: Zeroize accepts a nil receiver for
	// exactly this shape and this call spells the guard anyway, so the arm that holds nothing is
	// visibly the arm that erases nothing.
	if staged.processed.Commit != nil {
		staged.processed.Commit.Zeroize()
	}
	return nil
}

// stagedBy answers the staged half of a processed message THIS handle staged, and refuses the
// three shapes ApplyCommit's header names: no value, a value with no staged half, and one staged
// by another handle of this package. One body for the two doors that reach the staged half, so
// they cannot drift apart on what "this handle staged it" means.
func (self *connectMlsHandle) stagedBy(processed *EngineProcessed) (*stagedProcessed, error) {
	if processed == nil {
		return nil, fmt.Errorf("%w: no processed message", ErrEngineProcessedForeign)
	}
	staged, isStaged := processed.stagedRef.(*stagedProcessed)
	if !isStaged {
		return nil, fmt.Errorf("%w: it carries no staged commit this engine put there", ErrEngineProcessedForeign)
	}
	if staged.handle != self {
		return nil, fmt.Errorf("%w: it was staged by another handle", ErrEngineProcessedForeign)
	}
	return staged, nil
}

// Protect seals one application message under the current epoch, for a caller whose aad is a
// constant. An application record's is not; see ProtectBound.
func (self *connectMlsHandle) Protect(aad []byte, plaintext []byte) ([]byte, error) {
	return self.group.Protect(aad, plaintext)
}

// ProtectBound seals one application message whose aad NAMES the generation it is sealed at.
//
// The builder travels through verbatim: mls calls it with the generation it is about to spend, and
// PINS the answer -- a frame whose aad names a generation the frame is not at is never emitted. See
// (*mls.Group).ProtectBound for the four part argument about why the two cannot diverge.
func (self *connectMlsHandle) ProtectBound(aad func(generation uint32) ([]byte, error),
	plaintext []byte) ([]byte, error) {

	return self.group.ProtectBound(aad, plaintext)
}

// Unprotect opens one application message, projecting *mls.ApplicationMessage to four values.
//
// The projection is total: mls answers an error for everything it refuses, and a nil message with
// a nil error is not a state mls can produce -- but it is refused here anyway, because the
// alternative to refusing it is four zero values that read as an empty message from leaf 0 at
// generation 0.
func (self *connectMlsHandle) Unprotect(message []byte) ([]byte, []byte, uint32, uint32, error) {
	application, err := self.group.Unprotect(message)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	if application == nil {
		return nil, nil, 0, 0, fmt.Errorf("%w: an opened application message with no content", ErrEngineProcessedArm)
	}
	return application.AuthenticatedData, application.Plaintext,
		uint32(application.SenderLeaf), application.Generation, nil
}

// PeekSender is MASTER section 8.4.3's PRE-RATCHET reading: the leaf, the aad and the generation an
// inner frame names, read without touching a ratchet.
//
// IT MOVED ONTO THE INTERFACE ON 2026-09-17 and it used to be a free function over SenderDataSecret
// and GroupContextBytes. What that cost is worth recording rather than leaving to be noticed: the
// free form reached EVERY implementation of this interface for nothing, including ones in other
// repositories, and this form requires each of them to provide it. What it buys is spec A section
// 8.2's own amendment -- the peek is one of the three doors v2 names -- and an engine that can
// answer the three values out of state it already holds instead of rebuilding a crypto provider out
// of its own group context on every application record (the cost the free form's own comment
// flagged, ledger item MG-5).
//
// IT AUTHENTICATES NOTHING AND IS NEVER THE ANSWER. All three values come out of sender data sealed
// under a secret every member holds plus a cleartext header field, so all three are an attacker's
// claim. A refusal on a claim is honest; an acceptance on one is not, which is why
// unframeBodyOnLoop takes R1 and R2 a SECOND time on what mls has authenticated.
func (self *connectMlsHandle) PeekSender(frame []byte) (uint32, []byte, uint32, error) {
	return peekWithGroupSecrets(self, frame)
}

// Close releases the group, erasing the epoch secrets it holds.
func (self *connectMlsHandle) Close() error {
	return self.group.Close()
}

// framedApplicationLength answers how many octets an application record's ct_body plaintext will be
// for a caller body of plaintextLen, WITHOUT reserving a stream index or spending a generation.
//
// MASTER SECTION 8.4.6 IS WHY IT EXISTS and newRecordBuilderOnLoop's own comment says what the old
// rule cost. What this one adds is where the three inputs come from: the ciphersuite and the group
// id's width are read out of the handle's own group context, and the aad's width is aadMlsBytes --
// the digest's width, which is 32 at v1 and at v2 alike and is what makes the whole function a pure
// function of the plaintext's length.
//
// IT IS IN THIS FILE for peekWithGroupSecrets' reason: this file is the one place in the package
// that names connect/mls, and the seam is unchanged by it -- it reaches only GroupContextBytes,
// which GroupHandle already declares.
//
// THE COST is one group context parse per application seal, which is the same cost the open path
// already pays per application record and is written this way rather than cached for the same
// reason: a cache on the session would be a fourth piece of epoch state to invalidate.
func framedApplicationLength(handle GroupHandle, plaintextLen int) (int, error) {
	if handle == nil {
		return 0, ErrNilGroupHandle
	}
	contextBytes, err := handle.GroupContextBytes()
	if err != nil {
		return 0, err
	}
	context := &mls.GroupContext{}
	if err := syntax.Unmarshal(contextBytes, context); err != nil {
		return 0, err
	}
	return mls.FramedApplicationLength(context.CipherSuite, len(context.GroupId),
		aadMlsBytes, plaintextLen)
}

// peekInnerFrameSender is the record layer's door onto GroupHandle.PeekSender, and it is a door
// rather than a direct call for one reason: the nil handle.
//
// A nil GroupHandle is a caller's wiring mistake and a nil interface method call is a panic that
// takes the caller's process rather than its call, so the refusal is stated once here rather than
// at each of the places a reading is taken.
func peekInnerFrameSender(handle GroupHandle, frame []byte) (senderLeaf uint32, aad []byte,
	generation uint32, err error) {

	if handle == nil {
		return 0, nil, 0, ErrNilGroupHandle
	}
	return handle.PeekSender(frame)
}

// peekWithGroupSecrets reads the three fields of an inner MLS frame that MASTER section 8.4.3's
// refusals are functions of, WITHOUT letting the frame touch a receiving ratchet.
//
// THE THIRD FIELD IS THE GENERATION AND IT ARRIVED WITH v2. MASTER section 8.4.2 puts
// u32(generation) inside aad_mls, so R2 is a function of it; MASTER section 8.4.3's R3 requires R1
// and R2 to be DECIDED before any ratchet moves; and opening a frame is what commits its
// generation. A reading that answered the leaf and the aad but not the generation would therefore
// leave the refusal to be taken after the open, which is after the commit -- the check implemented
// and the vulnerability kept. All three come out of the SAME single sender data open, so the third
// value costs no AEAD, no derivation and no ratchet.
//
// WHY A SECOND READING OF THE SAME TWO FIELDS EXISTS AT ALL, because a reader's first instinct is
// that it is redundant with what Unprotect already answers. It is not redundant, it is EARLIER, and
// the whole value is in the "earlier". mls opens a frame and, on success, erases the message key
// of the generation the frame came at -- that is RFC 9420's forward secrecy and it is correct. So a
// refusal taken AFTER Unprotect is taken after the erase, and a member who lifts another member's
// genuine frame out of one record and seals it into another gets exactly that: the frame opens, it
// authenticates, R1 or R2 refuses it, and the generation the true sender's own record needed is
// gone at that receiver for good. One ordinary record per message an attacker wants deleted. With
// the two fields read first, the record is refused before mls is asked for a key at all.
//
// IT AUTHENTICATES NOTHING AND IS NEVER THE ANSWER. The leaf comes out of sender data sealed under
// a secret every member holds and the aad is a cleartext field, so both are an attacker's claim. A
// refusal on a claim is honest -- refusing needs no authentication -- but an acceptance on one is
// not, which is why unframeBodyOnLoop takes both refusals a SECOND time, on the values mls has
// authenticated, and the second reading is the one that decides. mls's own
// TestThePeekAgreesWithTheOpenOnEveryMessageThatOpens holds the two readings together, so the
// pre-filter can never be laxer than the rule it runs in front of.
//
// IT LIVES IN THIS FILE and not beside the refusals it serves, because this file is the one place
// in the package that names connect/mls. The seam is unchanged: GroupHandle grows no method, and
// this reaches only SenderDataSecret and GroupContextBytes, both already on it -- which is what
// makes the repair reach every implementation of the interface, including the ones in other
// repositories, rather than only the adapter below.
//
// THE COST, stated because the complement is the part a reader has to be told: the crypto provider
// is rebuilt per call, out of the ciphersuite in the group context. That is a suite table lookup
// and a struct, and it is on the open path of every application record. It is written this way
// rather than cached because a cache on the session would be a fourth piece of epoch state to
// invalidate, and nothing has measured the call as hot. Ledger item MG-5.
func peekWithGroupSecrets(handle GroupHandle, frame []byte) (senderLeaf uint32, aad []byte,
	generation uint32, err error) {

	if handle == nil {
		return 0, nil, 0, ErrNilGroupHandle
	}
	contextBytes, err := handle.GroupContextBytes()
	if err != nil {
		return 0, nil, 0, err
	}
	context := &mls.GroupContext{}
	if err := syntax.Unmarshal(contextBytes, context); err != nil {
		return 0, nil, 0, err
	}
	crypto, err := mls.NewCryptoProvider(context.CipherSuite)
	if err != nil {
		return 0, nil, 0, err
	}
	senderDataSecret, err := handle.SenderDataSecret()
	if err != nil {
		return 0, nil, 0, err
	}
	defer zeroize(senderDataSecret)
	leaf, authenticatedData, frameGeneration, err := mls.PeekPrivateMessageSender(crypto, senderDataSecret, frame)
	if err != nil {
		return 0, nil, 0, err
	}
	return uint32(leaf), authenticatedData, frameGeneration, nil
}

// stagedProcessed is what this adapter puts in EngineProcessed.stagedRef: the mls value ApplyCommit
// needs, and the handle that staged it.
//
// The handle is in it because the mls value alone would let one group's staged commit be applied
// to another group's handle. mls would refuse it a step later, at the transcript; refusing it here
// names the mistake instead of naming a group nobody tampered with.
type stagedProcessed struct {
	handle    *connectMlsHandle
	processed *mls.Processed
}
