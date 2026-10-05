// RFC 9420 section 11 group creation, held to the four steps rather than to the answer.
//
// WHAT THIS FILE IS BUILT AROUND. NewGroup is the one construction in this package with nothing on
// the other side of it: no peer sent these octets, no signature was made over them by anybody else,
// and no published vector covers a group id and an epoch secret somebody chose at random. Every
// field it writes is therefore a field whose only judge is a test in this file, and the two fields
// that matter most -- the founding leaf's HPKE key pair and the initial epoch_secret -- are the two
// a wrong value is completely invisible in. A constant epoch secret still expands into nine
// well formed secrets of the right length; a constant leaf key still seals, still opens, and still
// hashes into a perfectly good tree. Two entropy substitutions of exactly that shape passed 963
// green tests on p5.
//
// So the entropy test here does not assert over the LINES that draw. It records the draws the
// provider was asked for, and then finds each of them again in what the group PUBLISHES: the leaf
// key by re-deriving it, the epoch secret by rebuilding the whole key schedule over the group's own
// context and comparing the epoch authenticator. A draw that never reaches what the group published
// is a draw that was thrown away, and a group whose two founding secrets came out of ONE draw is
// caught by the two being found at the same index.
package mls

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/mls/syntax"
)

// testStore is an in-memory StateStore for the lifecycle tests.
type testStore struct {
	states   map[string][]byte
	privs    map[string][]byte
	packages map[string][3][]byte
	deletes  []uint64
}

func newTestStore() *testStore {
	return &testStore{
		states:   map[string][]byte{},
		privs:    map[string][]byte{},
		packages: map[string][3][]byte{},
	}
}

// stateKey formats the epoch in decimal. string(rune(epoch)) would collapse every epoch in
// 0xD800-0xDFFF and everything above 0x10FFFF onto U+FFFD, and this fixture backs the past-epoch
// window assertions of task 19, where a silent collision reads as a correct deletion.
func stateKey(groupId []byte, epoch uint64) string {
	return string(groupId) + "/" + strconv.FormatUint(epoch, 10)
}

func (self *testStore) PutGroupState(groupId []byte, epoch uint64, state []byte) error {
	self.states[stateKey(groupId, epoch)] = bytes.Clone(state)
	return nil
}

func (self *testStore) GetGroupState(groupId []byte, epoch uint64) ([]byte, error) {
	state, held := self.states[stateKey(groupId, epoch)]
	if !held {
		return nil, errors.New("no state")
	}
	return state, nil
}

// DeleteGroupStateBefore walks the keys this store HOLDS rather than every epoch below the
// cutoff.
//
// The range loop it replaces was O(cutoff), and the defect this fixture exists to catch produces
// a cutoff of nearly 2^64: the past-epoch window's floor is what keeps epoch - PastEpochWindow
// from underflowing on a group younger than the window, and a build without it took the whole
// test binary out on a five minute timeout instead of on the assertion that names the defect. A
// fixture that answers a hang where it could answer a finding is a fixture that reports the same
// thing for a bug and for a broken machine.
func (self *testStore) DeleteGroupStateBefore(groupId []byte, epoch uint64) error {
	self.deletes = append(self.deletes, epoch)
	prefix := string(groupId) + "/"
	for key := range self.states {
		rest, isThisGroup := strings.CutPrefix(key, prefix)
		if !isThisGroup {
			continue
		}
		at, err := strconv.ParseUint(rest, 10, 64)
		if err != nil || at >= epoch {
			continue
		}
		delete(self.states, key)
	}
	return nil
}

func (self *testStore) PutPrivateKey(pub []byte, priv []byte) error {
	self.privs[string(pub)] = bytes.Clone(priv)
	return nil
}

func (self *testStore) GetPrivateKey(pub []byte) ([]byte, error) {
	priv, held := self.privs[string(pub)]
	if !held {
		return nil, errors.New("no key")
	}
	return priv, nil
}

func (self *testStore) DeletePrivateKey(pub []byte) error {
	delete(self.privs, string(pub))
	return nil
}

func (self *testStore) PutKeyPackage(ref []byte, kp []byte, initPriv []byte, encPriv []byte) error {
	self.packages[string(ref)] = [3][]byte{kp, initPriv, encPriv}
	return nil
}

func (self *testStore) TakeKeyPackage(ref []byte) ([]byte, []byte, []byte, error) {
	entry, held := self.packages[string(ref)]
	if !held {
		return nil, nil, nil, errors.New("no key package")
	}
	delete(self.packages, string(ref))
	return entry[0], entry[1], entry[2], nil
}

// testGroupConfig builds a v1-profile config for one member, who is the group's owner.
func testGroupConfig(t *testing.T, crypto CryptoProvider, owner *testMember, groupId string) *GroupConfig {
	t.Helper()
	policy := &GroupPolicyExtension{Roles: []RoleEntry{{MemberId: owner.IdentityPub, Role: RoleOwner}}}
	if err := policy.Canonicalize(); err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	policyExt, err := policy.Encode()
	if err != nil {
		t.Fatalf("Encode policy: %v", err)
	}
	return &GroupConfig{
		Suite:      CipherSuiteX25519ChaCha20Sha256Ed25519,
		GroupId:    []byte(groupId),
		Extensions: []Extension{policyExt},
		RequiredCaps: RequiredCapabilities{
			ExtensionTypes: []ExtensionType{
				ExtensionTypeUrmessageGroupPolicy,
				ExtensionTypeUrmessageLeafKeys,
			},
		},
		Crypto:   crypto,
		Store:    newTestStore(),
		Profile:  defaultProfile(),
		LeafKeys: LeafKeysExtension{AlgId: AlgIdXwing, DeviceXwingPub: owner.XwingPub},
	}
}

func testNewGroup(t *testing.T, crypto CryptoProvider, owner *testMember, groupId string) *Group {
	t.Helper()
	cfg := testGroupConfig(t, crypto, owner, groupId)
	group, err := NewGroup(cfg, owner.SigPriv, BasicCredential(owner.IdentityPub))
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	return group
}

// testGroupContextOf decodes the group's own published context, so that every assertion over it
// runs on the octets a peer would be handed rather than on the struct this package happens to hold.
func testGroupContextOf(t *testing.T, group *Group) *GroupContext {
	t.Helper()
	encoded, err := group.GroupContext()
	if err != nil {
		t.Fatalf("GroupContext: %v", err)
	}
	decoded := &GroupContext{}
	if err := syntax.Unmarshal(encoded, decoded); err != nil {
		t.Fatalf("unmarshal the published group context: %v", err)
	}
	return decoded
}

func TestNewGroupIsAOneMemberGroupAtEpochZero(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	group := testNewGroup(t, crypto, owner, "group-1")
	defer group.Close()

	if group.Epoch() != 0 {
		t.Fatalf("Epoch = %d, want 0", group.Epoch())
	}
	if group.OwnLeafIndex() != 0 {
		t.Fatalf("OwnLeafIndex = %d, want 0", group.OwnLeafIndex())
	}
	members := group.Members()
	if len(members) != 1 {
		t.Fatalf("Members = %d, want 1", len(members))
	}
	if !bytes.Equal(members[0].IdentityPub, owner.IdentityPub) {
		t.Fatal("the creator is not the single member")
	}
	if members[0].Role != RoleOwner {
		t.Fatalf("creator role = %v, want owner", members[0].Role)
	}
	if members[0].LeafKeys == nil || !bytes.Equal(members[0].LeafKeys.DeviceXwingPub, owner.XwingPub) {
		t.Fatal("the creator's leaf does not carry urmessage_leaf_keys")
	}
	at, found := group.MemberAt(0)
	if !found || !bytes.Equal(at.IdentityPub, owner.IdentityPub) {
		t.Fatalf("MemberAt(0) = %+v, %v", at, found)
	}
	if !bytes.Equal(group.GroupId(), []byte("group-1")) {
		t.Fatalf("GroupId = %q", group.GroupId())
	}
	if len(group.EpochAuthenticator()) != crypto.HashSize() {
		t.Fatalf("EpochAuthenticator length = %d, want %d",
			len(group.EpochAuthenticator()), crypto.HashSize())
	}
}

func TestNewGroupConfirmedTranscriptHashIsEmptyAtEpochZero(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	group := testNewGroup(t, crypto, owner, "group-1")
	defer group.Close()

	ctx := testGroupContextOf(t, group)
	// section 11: "Confirmed transcript hash: The zero-length octet string". KDF.Nh zero octets
	// and the hash of nothing are both well formed values of the wrong length, and both would
	// carry every assertion this file makes about the group except this one.
	if len(ctx.ConfirmedTranscriptHash) != 0 {
		t.Fatalf("ConfirmedTranscriptHash = %x, want the zero-length octet string",
			ctx.ConfirmedTranscriptHash)
	}
	if ctx.Epoch != 0 {
		t.Fatalf("group context epoch = %d, want 0", ctx.Epoch)
	}
	if ctx.Version != ProtocolVersionMls10 {
		t.Fatalf("group context version = %#04x", uint16(ctx.Version))
	}
	if ctx.CipherSuite != CipherSuiteX25519ChaCha20Sha256Ed25519 {
		t.Fatalf("group context ciphersuite = %#04x", uint16(ctx.CipherSuite))
	}
	if !bytes.Equal(ctx.GroupId, []byte("group-1")) {
		t.Fatalf("group context group id = %q", ctx.GroupId)
	}

	// the tree hash the context names is the hash of the tree the group publishes, taken over
	// the encoded tree rather than over the group's own field, so a context that named some
	// other tree's hash is visible from outside
	encodedTree, err := group.RatchetTree()
	if err != nil {
		t.Fatalf("RatchetTree: %v", err)
	}
	published, err := UnmarshalRatchetTree(encodedTree)
	if err != nil {
		t.Fatalf("UnmarshalRatchetTree: %v", err)
	}
	treeHash, err := published.TreeHash(crypto)
	if err != nil {
		t.Fatalf("TreeHash: %v", err)
	}
	if !bytes.Equal(ctx.TreeHash, treeHash) {
		t.Fatalf("group context tree hash = %x and the published tree hashes to %x",
			ctx.TreeHash, treeHash)
	}
}

// TestNewGroupFoldsTheEpochZeroConfirmationTagIntoTheInterimTranscriptHash is section 11's last
// step, and it is the one nothing outside this package can see.
//
// Section 11 ends with "Compute the updated interim_transcript_hash from the
// confirmed_transcript_hash and the confirmation_tag". A creator that skipped it holds the empty
// interim hash, derives a DIFFERENT confirmed hash for its first commit, and nothing notices --
// there is no second member at epoch 0 to disagree with, and a joiner at epoch 1 takes the confirmed
// hash out of the GroupInfo rather than recomputing it. The first thing that can see it is another
// implementation, which is to say the day this build is asked to interoperate.
//
// The expectation is rebuilt from the primitives rather than read off the group: the tag is a MAC
// over the empty octet string under this epoch's confirmation key, and the interim hash is
// Hash(confirmed || opaque(tag)). Both halves of the group's own transcript are asserted, because
// the confirmed half being empty is what makes the interim half the value it is.
func TestNewGroupFoldsTheEpochZeroConfirmationTagIntoTheInterimTranscriptHash(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	group := testNewGroup(t, crypto, owner, "group-1")
	defer group.Close()

	if len(group.transcript.Confirmed) != 0 {
		t.Fatalf("confirmed transcript hash = %x, want the zero-length octet string",
			group.transcript.Confirmed)
	}
	tag := crypto.Mac(group.schedule.Secrets().Confirmation, []byte{})
	want, err := InterimTranscriptHash(crypto, []byte{}, tag)
	if err != nil {
		t.Fatalf("InterimTranscriptHash: %v", err)
	}
	if len(want) == 0 {
		t.Fatal("the expectation this test compares against is empty")
	}
	if !bytes.Equal(group.transcript.Interim, want) {
		t.Fatalf("interim transcript hash = %x, want Hash(confirmed || opaque(confirmation_tag)) = %x",
			group.transcript.Interim, want)
	}
}

// TestNewGroupFoundingLeafIsKeyPackageSourcedAndValidates holds the founding leaf to section 7.3.
//
// The SOURCE is the field this is really about. Section 7.2 makes leaf_node_source select what the
// signature covers -- a key_package leaf carries a Lifetime and is bound to no group, an update or
// commit leaf carries none and is bound to a group id and a leaf index -- so a founding leaf built
// under one source and stamped with another is a leaf whose signature was made over a preimage it
// no longer describes. Nothing in the group's own arithmetic reads the field, so the tree hashes,
// the context encodes, and every epoch secret is the one it should be.
func TestNewGroupFoundingLeafIsKeyPackageSourcedAndValidates(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	group := testNewGroup(t, crypto, owner, "group-1")
	defer group.Close()

	leaf := group.OwnLeafNodeCopy()
	if leaf == nil {
		t.Fatal("the group holds no leaf at its own index")
	}
	// section 11's leaf is the creator's own key package leaf: it entered a tree nobody had
	// committed over, so there is no group id and no position for its signature to be bound to
	if leaf.LeafNodeSource != LeafNodeSourceKeyPackage {
		t.Fatalf("founding leaf source = %d, want key_package (%d)",
			leaf.LeafNodeSource, LeafNodeSourceKeyPackage)
	}
	ctx := testGroupContextOf(t, group)
	if err := leaf.Validate(&LeafValidationContext{
		Crypto:          crypto,
		Suite:           ctx.CipherSuite,
		GroupId:         ctx.GroupId,
		LeafIndex:       group.OwnLeafIndex(),
		ExpectedSource:  LeafNodeSourceKeyPackage,
		GroupExtensions: ctx.Extensions,
		NowMs:           uint64(time.Now().UnixMilli()),
	}); err != nil {
		t.Fatalf("the founding leaf does not pass section 7.3 validation: %v", err)
	}
	// and it is a COPY: a caller that scribbles on what it was handed must not reach the tree the
	// epoch's tree hash was taken over
	before, err := group.RatchetTree()
	if err != nil {
		t.Fatalf("RatchetTree: %v", err)
	}
	leaf.SignatureKey = bytes.Repeat([]byte{0x7f}, len(leaf.SignatureKey))
	after, err := group.RatchetTree()
	if err != nil {
		t.Fatalf("RatchetTree after the copy was written to: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("writing to the leaf OwnLeafNodeCopy handed back changed the group's own tree")
	}
}

// TestNewGroupBindsItsProposalCacheToEpochZeroOfThisGroup holds the binding the cache was given.
//
// The cache is bound at construction and only a VerifiedGroupContext can bind it. What this test
// can see is the PAIR the binding names; what says the pair came from a context this client
// verified rather than one NewGroup wrote down is
// TestEveryConstructionOfAVerifiedGroupContextIsClassifiedHere, which fails on the day this package
// grows a second way to build one. The two together are the rule: this one that the binding is
// right, that one that it was earned.
func TestNewGroupBindsItsProposalCacheToEpochZeroOfThisGroup(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	group := testNewGroup(t, crypto, owner, "group-1")
	defer group.Close()

	ctx := testGroupContextOf(t, group)
	if err := group.proposals.CheckEpoch(ctx); err != nil {
		t.Fatalf("the cache does not hold for the epoch its own group is in: %v", err)
	}
	later := ctx.Clone()
	later.Epoch = 1
	if err := group.proposals.CheckEpoch(later); err == nil {
		t.Fatal("the cache holds for epoch 1 of a group that is at epoch 0")
	}
	elsewhere := ctx.Clone()
	elsewhere.GroupId = []byte("group-2")
	if err := group.proposals.CheckEpoch(elsewhere); err == nil {
		t.Fatal("the cache holds for a group id it was not bound to")
	}
}

// entropyWitness records what a group creation asked its provider to draw, and what it derived a
// key pair from.
//
// It is a wrapper and not a stub: every answer is the real provider's, so the group it helps build
// is a real group whose secrets are real secrets. What it adds is the only thing an entropy gate
// can rest on -- knowing which values were drawn, so that each can be looked for in what the group
// went on to publish.
type entropyWitness struct {
	CryptoProvider
	draws [][]byte
	ikms  [][]byte
}

func (self *entropyWitness) Random(n int) []byte {
	drawn := self.CryptoProvider.Random(n)
	self.draws = append(self.draws, bytes.Clone(drawn))
	return drawn
}

func (self *entropyWitness) DeriveKeyPair(ikm []byte) (HpkePrivateKey, HpkePublicKey, error) {
	self.ikms = append(self.ikms, bytes.Clone(ikm))
	return self.CryptoProvider.DeriveKeyPair(ikm)
}

// drawIndexOf answers which recorded draw a value is, or -1 for a value that was never drawn.
func drawIndexOf(draws [][]byte, value []byte) int {
	for i, drawn := range draws {
		if bytes.Equal(drawn, value) {
			return i
		}
	}
	return -1
}

// TestNewGroupDrawsEachFoundingSecretFreshAndFromItsOwnDraw is the entropy gate, and it is written
// against what the group PUBLISHES rather than against the two lines that draw.
//
// Three defects it is built to see, each of which leaves every other test in this package green:
//
//  1. a draw replaced by a CONSTANT. The value the group published is then not one of the draws the
//     provider was asked for, so the search below finds nothing.
//  2. the two draws COLLAPSED into one, so that the founding leaf's key pair and the epoch secret
//     are the same 32 octets. Both are still fresh per group and both still change with the
//     entropy source, so a divergence test cannot see it; what sees it is the two being found at
//     the same INDEX.
//  3. a draw made and then THROWN AWAY. Every draw the provider was asked for has to be accounted
//     for by one of the two the group published, so a third draw is a failure here until somebody
//     says what it is for.
//
// The epoch secret is found by rebuilding the whole key schedule over the group's own published
// context and comparing the epoch authenticator, which is the only way to identify a value that no
// exported symbol of this package answers and that guardrail 6 says none ever may.
func TestNewGroupDrawsEachFoundingSecretFreshAndFromItsOwnDraw(t *testing.T) {
	base := testCrypto(t)
	owner := testIdentity(t, base, "owner")
	witness := &entropyWitness{CryptoProvider: base}
	cfg := testGroupConfig(t, witness, owner, "group-1")
	group, err := NewGroup(cfg, owner.SigPriv, BasicCredential(owner.IdentityPub))
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	defer group.Close()

	if len(witness.draws) < 2 {
		t.Fatalf("group creation drew %d values; section 11 draws a leaf key pair and an epoch secret, which is two",
			len(witness.draws))
	}
	if len(witness.ikms) != 1 {
		t.Fatalf("group creation derived %d key pairs, want the one the founding leaf is keyed by",
			len(witness.ikms))
	}

	// the founding leaf's key pair, identified by re-deriving it from the draw it names
	leafDraw := drawIndexOf(witness.draws, witness.ikms[0])
	if leafDraw < 0 {
		t.Fatal("the founding leaf's key pair was derived from something this group creation never drew")
	}
	_, encPub, err := base.DeriveKeyPair(witness.draws[leafDraw])
	if err != nil {
		t.Fatalf("re-derive the founding key pair: %v", err)
	}
	leaf := group.OwnLeafNodeCopy()
	if leaf == nil {
		t.Fatal("the group holds no leaf at its own index")
	}
	if !bytes.Equal(leaf.EncryptionKey, encPub) {
		t.Fatalf("the founding leaf's encryption key is %x and draw %d derives %x",
			leaf.EncryptionKey, leafDraw, encPub)
	}

	// the epoch secret, identified by rebuilding this epoch's key schedule over the group's own
	// published context. No exported symbol answers an epoch secret and none ever may, so the
	// value is recognised by what it derives rather than read off the group.
	ctx := testGroupContextOf(t, group)
	authenticator := group.EpochAuthenticator()
	if len(authenticator) == 0 {
		t.Fatal("the group answered no epoch authenticator, so nothing below is being compared")
	}
	epochDraw := -1
	for i, drawn := range witness.draws {
		if len(drawn) != base.HashSize() {
			continue
		}
		rebuilt, scheduleErr := NewKeyScheduleFromEpochSecret(base, drawn, ctx)
		if scheduleErr != nil {
			t.Fatalf("rebuild the key schedule over draw %d: %v", i, scheduleErr)
		}
		if bytes.Equal(rebuilt.Secrets().EpochAuthenticator, authenticator) {
			if epochDraw >= 0 {
				t.Fatalf("draws %d and %d both rebuild this epoch, so one of them is not the epoch secret",
					epochDraw, i)
			}
			epochDraw = i
		}
		rebuilt.Zeroize()
	}
	if epochDraw < 0 {
		t.Fatal("no value this group creation drew rebuilds its key schedule; the epoch secret is not one of the draws")
	}

	// the two came out of DIFFERENT draws. Collapsed into one they are both still fresh per
	// group, so nothing that compares two groups can see it.
	if epochDraw == leafDraw {
		t.Fatalf("the founding leaf's key pair and the epoch secret are both draw %d; section 11 draws them separately",
			leafDraw)
	}
	// and nothing was drawn that the group did not publish
	for i := range witness.draws {
		if i != epochDraw && i != leafDraw {
			t.Errorf("draw %d is neither the founding leaf's key material nor the epoch secret; a draw nothing published is entropy this gate cannot follow",
				i)
		}
	}
}

func TestNewGroupTwoCreatorsDiverge(t *testing.T) {
	// epoch_secret is fresh random per RFC 9420 section 11, so two creators of the same group id
	// must not derive the same epoch authenticator -- and must not key the same founding leaf
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	first := testNewGroup(t, crypto, owner, "group-1")
	defer first.Close()
	second := testNewGroup(t, crypto, owner, "group-1")
	defer second.Close()

	if bytes.Equal(first.EpochAuthenticator(), second.EpochAuthenticator()) {
		t.Fatal("two group creations produced the same epoch authenticator; epoch_secret is not random")
	}
	firstLeaf, secondLeaf := first.OwnLeafNodeCopy(), second.OwnLeafNodeCopy()
	if firstLeaf == nil || secondLeaf == nil {
		t.Fatal("a group holds no leaf at its own index")
	}
	if bytes.Equal(firstLeaf.EncryptionKey, secondLeaf.EncryptionKey) {
		t.Fatal("two group creations keyed the same founding leaf; the leaf key pair is not random")
	}
}

func TestNewGroupRefusesNonV1Ciphersuite(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	cfg := testGroupConfig(t, crypto, owner, "group-1")
	// registered and implemented, refused at group creation by policy
	cfg.Suite = CipherSuiteX25519AesGcm128Sha256Ed25519
	_, err := NewGroup(cfg, owner.SigPriv, BasicCredential(owner.IdentityPub))
	if !errors.Is(err, errProfileCiphersuite) {
		t.Fatalf("NewGroup error = %v, want errProfileCiphersuite", err)
	}
}

// TestNewGroupRefusesAProviderRunningAnotherSuite separates the profile's refusal from the
// wiring one. Both are about a ciphersuite and they are different faults: the profile refuses a
// suite this build will not create under, and this refuses a config and a provider that were
// wired together wrongly, over a suite the profile is perfectly happy with.
func TestNewGroupRefusesAProviderRunningAnotherSuite(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519AesGcm128Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider(0x0001): %v", err)
	}
	owner := testIdentity(t, crypto, "owner")
	cfg := testGroupConfig(t, crypto, owner, "group-1")
	_, err = NewGroup(cfg, owner.SigPriv, BasicCredential(owner.IdentityPub))
	if !errors.Is(err, errGroupConfigProviderSuite) {
		t.Fatalf("NewGroup error = %v, want errGroupConfigProviderSuite", err)
	}
	if errors.Is(err, errProfileCiphersuite) {
		t.Fatal("the wiring refusal also reads as the profile refusal, so a caller cannot tell the two apart")
	}
}

func TestNewGroupRefusesGroupWithoutPolicy(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	cfg := testGroupConfig(t, crypto, owner, "group-1")
	cfg.Extensions = nil
	_, err := NewGroup(cfg, owner.SigPriv, BasicCredential(owner.IdentityPub))
	if !errors.Is(err, ErrNoGroupPolicy) {
		t.Fatalf("NewGroup error = %v, want ErrNoGroupPolicy", err)
	}
}

func TestNewGroupRefusesAConfigWithNoStore(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	cfg := testGroupConfig(t, crypto, owner, "group-1")
	cfg.Store = nil
	_, err := NewGroup(cfg, owner.SigPriv, BasicCredential(owner.IdentityPub))
	if !errors.Is(err, errNilStateStore) {
		t.Fatalf("NewGroup error = %v, want errNilStateStore", err)
	}
}

func TestEpochSecretAccessorIsClosed(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	group := testNewGroup(t, crypto, owner, "group-1")
	defer group.Close()

	senderData, err := group.EpochSecret(EpochSecretSenderData)
	if err != nil {
		t.Fatalf("EpochSecret(sender_data): %v", err)
	}
	encryption, err := group.EpochSecret(EpochSecretEncryption)
	if err != nil {
		t.Fatalf("EpochSecret(encryption): %v", err)
	}
	if len(senderData) != crypto.HashSize() || len(encryption) != crypto.HashSize() {
		t.Fatalf("sender_data is %d octets and encryption is %d, want %d",
			len(senderData), len(encryption), crypto.HashSize())
	}
	if bytes.Equal(senderData, encryption) {
		t.Fatal("sender_data_secret and encryption_secret must be independent derivations")
	}
	if _, err := group.EpochSecret(EpochSecretName(9)); err == nil {
		t.Fatal("EpochSecret accepted a name outside the closed enum")
	}
	if _, err := group.EpochSecret(EpochSecretName(0)); err == nil {
		t.Fatal("EpochSecret accepted the zero value, which is a caller that named nothing")
	}
}

// TestAClosedGroupRefusesItsSecretsRatherThanAnsweringZeros is what makes Close safe to defer.
//
// Zeroize leaves KDF.Nh zero octets behind rather than a short slice, so an accessor that went on
// reading the schedule after a close would answer a secret of exactly the right length that every
// closed group in the process agrees on.
func TestAClosedGroupRefusesItsSecretsRatherThanAnsweringZeros(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	group := testNewGroup(t, crypto, owner, "group-1")

	if err := group.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// idempotent, because a deferred close beside an explicit one is not a mistake
	if err := group.Close(); err != nil {
		t.Fatalf("Close twice: %v", err)
	}
	if secret := group.EpochAuthenticator(); secret != nil {
		t.Fatalf("a closed group answered an epoch authenticator of %x", secret)
	}
	if _, err := group.EpochSecret(EpochSecretEncryption); !errors.Is(err, errGroupClosed) {
		t.Fatalf("EpochSecret on a closed group = %v, want errGroupClosed", err)
	}
	if _, err := group.Export("label", nil, 32); !errors.Is(err, errGroupClosed) {
		t.Fatalf("Export on a closed group = %v, want errGroupClosed", err)
	}
	// and the state that is not secret is still readable, which is what a closed group is for
	if group.Epoch() != 0 || !bytes.Equal(group.GroupId(), []byte("group-1")) {
		t.Fatal("a closed group forgot which group it was")
	}
}

func TestGroupExportAnswersTheKeyScheduleExporter(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	group := testNewGroup(t, crypto, owner, "group-1")
	defer group.Close()

	exported, err := group.Export("urmessage exporter", []byte("context"), 32)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	want, err := group.schedule.Export("urmessage exporter", []byte("context"), 32)
	if err != nil {
		t.Fatalf("KeySchedule.Export: %v", err)
	}
	if !bytes.Equal(exported, want) {
		t.Fatalf("Export = %x, want %x", exported, want)
	}
	// a different label is a different secret, so the exporter is reading its arguments
	other, err := group.Export("another label", []byte("context"), 32)
	if err != nil {
		t.Fatalf("Export under another label: %v", err)
	}
	if bytes.Equal(exported, other) {
		t.Fatal("two labels exported the same secret")
	}
}

func TestGroupStateIsPersistedAtCreation(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	cfg := testGroupConfig(t, crypto, owner, "group-1")
	store, isTestStore := cfg.Store.(*testStore)
	if !isTestStore {
		t.Fatalf("the fixture's store is a %T", cfg.Store)
	}
	group, err := NewGroup(cfg, owner.SigPriv, BasicCredential(owner.IdentityPub))
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	defer group.Close()

	blob, err := store.GetGroupState([]byte("group-1"), 0)
	if err != nil {
		t.Fatalf("epoch 0 state was not persisted: %v", err)
	}
	if len(blob) == 0 {
		t.Fatal("the persisted epoch 0 state is empty")
	}
	// nothing is written at any other epoch, which is what makes task 19's past-epoch window a
	// window over something
	if _, err := store.GetGroupState([]byte("group-1"), 1); err == nil {
		t.Fatal("group creation wrote state at epoch 1")
	}
}

// TestTheV1ProfileClassifiesEveryRegisteredCiphersuiteForCreation holds the creation table to the
// suite registry in both directions, and then holds the door to the table.
//
// The registry is Suites(), which is read off registeredSuiteParams rather than written down here:
// a suite added to this build with no row is a code point the door would answer "not registered"
// about, which is a sentence about a registration that is right there.
func TestTheV1ProfileClassifiesEveryRegisteredCiphersuiteForCreation(t *testing.T) {
	registered := Suites()
	if len(registered) == 0 {
		t.Fatal("this build registers no ciphersuite, so this gate read nothing")
	}
	admitted, refused := 0, 0
	for _, suite := range registered {
		refusal, classified := createSuiteProfile[suite]
		if !classified {
			t.Errorf("suite.go registers %#04x and createSuiteProfile has no row for it",
				uint16(suite))
			continue
		}
		if refusal == nil {
			admitted += 1
		} else {
			refused += 1
		}
	}
	for suite := range createSuiteProfile {
		if !slices.Contains(registered, suite) {
			t.Errorf("createSuiteProfile holds a row for %#04x and this build registers no such suite",
				uint16(suite))
		}
	}
	// both halves non-empty, or the table is satisfied by one answer. A profile that refused
	// only suites this build does not implement would be stating nothing at all.
	if admitted == 0 || refused == 0 {
		t.Fatalf("the creation profile admits %d suites and refuses %d; a table whose answer is the same for every member states nothing",
			admitted, refused)
	}
	// and the door answers what the table holds, so the two cannot drift
	active := defaultProfile()
	for _, suite := range registered {
		err := active.checkCiphersuiteForCreate(suite)
		switch want := createSuiteProfile[suite]; {
		case want == nil && err != nil:
			t.Errorf("the table admits %#04x for creation and the door answered %v", uint16(suite), err)
		case want != nil && !errors.Is(err, want):
			t.Errorf("the table refuses %#04x with %v and the door answered %v", uint16(suite), want, err)
		}
	}
	// an unregistered code point is a different sentence: there is no narrowing to name
	if err := active.checkCiphersuiteForCreate(CipherSuite(0xBEEF)); !errors.Is(err, ErrUnknownCipherSuite) {
		t.Errorf("checkCiphersuiteForCreate(0xBEEF) = %v, want ErrUnknownCipherSuite", err)
	}
	t.Logf("%d registered suites, %d admitted for creation and %d refused",
		admitted+refused, admitted, refused)
}

// TestNewGroupRefusesANilConfig holds the one argument that carries the whole group.
//
// It answers its own value rather than ErrNilCryptoProvider, because a caller that passed no config
// has not passed a config with no provider: the two are different mistakes made in different lines
// and repaired in different places.
func TestNewGroupRefusesANilConfig(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	_, err := NewGroup(nil, owner.SigPriv, BasicCredential(owner.IdentityPub))
	if !errors.Is(err, errNilGroupConfig) {
		t.Fatalf("NewGroup(nil) = %v, want errNilGroupConfig", err)
	}
	if errors.Is(err, ErrNilCryptoProvider) {
		t.Fatal("a nil config reads as a nil provider, so a caller cannot tell which it forgot")
	}
}

func TestNewGroupRefusesAConfigWithNoProvider(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	cfg := testGroupConfig(t, crypto, owner, "group-1")
	cfg.Crypto = nil
	_, err := NewGroup(cfg, owner.SigPriv, BasicCredential(owner.IdentityPub))
	if !errors.Is(err, ErrNilCryptoProvider) {
		t.Fatalf("NewGroup over a config with no provider = %v, want ErrNilCryptoProvider", err)
	}
}

// truncatingMacProvider is a provider whose MAC answers one octet short of a tag.
//
// It exists for one door and is not a hypothetical shape: (*KeySchedule).ConfirmationTag answers
// NIL for an epoch whose confirmation_key has been erased, and a creation path that folded a nil or
// short tag into the interim transcript hash would found a group whose transcript nobody else can
// reproduce -- with no error anywhere, because InterimTranscriptHash length-prefixes whatever it is
// handed and hashes it happily.
type truncatingMacProvider struct {
	CryptoProvider
}

func (self *truncatingMacProvider) Mac(key []byte, data []byte) []byte {
	full := self.CryptoProvider.Mac(key, data)
	return full[:len(full)-1]
}

func TestNewGroupRefusesAConfirmationTagOfTheWrongWidth(t *testing.T) {
	base := testCrypto(t)
	owner := testIdentity(t, base, "owner")
	cfg := testGroupConfig(t, &truncatingMacProvider{CryptoProvider: base}, owner, "group-1")
	_, err := NewGroup(cfg, owner.SigPriv, BasicCredential(owner.IdentityPub))
	if !errors.Is(err, errCreationConfirmationTag) {
		t.Fatalf("NewGroup over a provider whose MAC is a byte short = %v, want errCreationConfirmationTag", err)
	}
}

// TestNewGroupCarriesTheExtensionsItsCreatorChose holds the other half of what section 11 lets the
// creator decide, and it is the half nothing else in this file reads.
//
// The group context's extensions vector is covered by the confirmed transcript hash and by every
// epoch secret expanded over it, so a creator that dropped required_capabilities founds a group
// whose members are held to nothing and whose context still hashes, still encodes and still derives
// a perfectly good key schedule. Section 11.1 is what the vector is for: it is the group's statement
// of what every member must support, and a group that never made it cannot be told from one whose
// members all happen to support the right things.
func TestNewGroupCarriesTheExtensionsItsCreatorChose(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	cfg := testGroupConfig(t, crypto, owner, "group-1")
	group, err := NewGroup(cfg, owner.SigPriv, BasicCredential(owner.IdentityPub))
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	defer group.Close()

	ctx := testGroupContextOf(t, group)
	body, carried, err := FindExtension(ctx.Extensions, ExtensionTypeRequiredCapabilities)
	if err != nil {
		t.Fatalf("FindExtension(required_capabilities): %v", err)
	}
	if !carried {
		t.Fatal("the group context carries no required_capabilities, and the config asked for two extension types")
	}
	required := &RequiredCapabilities{}
	if err := syntax.Unmarshal(body, required); err != nil {
		t.Fatalf("unmarshal required_capabilities: %v", err)
	}
	if !slices.Equal(required.ExtensionTypes, cfg.RequiredCaps.ExtensionTypes) {
		t.Fatalf("required extension types = %v, want %v",
			required.ExtensionTypes, cfg.RequiredCaps.ExtensionTypes)
	}
	// the policy the creator chose is there too, and it is the same one GroupPolicy answers
	policy, err := group.GroupPolicy()
	if err != nil {
		t.Fatalf("GroupPolicy: %v", err)
	}
	role, named := policy.RoleOf(owner.IdentityPub)
	if !named || role != RoleOwner {
		t.Fatalf("the group policy names the creator as %v (named=%v), want owner", role, named)
	}
	// and every leaf in the group supports what the group requires, which is section 11.1's own
	// rule and the reason a creator may not require what it cannot itself do
	leaf := group.OwnLeafNodeCopy()
	if leaf == nil {
		t.Fatal("the group holds no leaf at its own index")
	}
	if err := leaf.Capabilities.Supports(required); err != nil {
		t.Fatalf("the founding leaf does not support the capabilities its own group requires: %v", err)
	}
}

// ---------------------------------------------------------------------------
// RFC 9420 section 12.1: proposal generation
// ---------------------------------------------------------------------------

// WHAT THE TESTS BELOW ARE BUILT AROUND. Proposal generation is the SENDING side of everything
// validate_proposals.go, validate_commit.go and the proposal cache judge on the receiving side, so
// the strongest thing that can be said about it is not that its output parses -- it is that this
// package's own doors ACCEPT what this package produced. A generator that emits a proposal its own
// validator refuses is a defect nothing downstream would report until a peer refused a commit, and
// it is a defect this package can detect in one call. That is
// TestEveryProposalThisGroupGeneratesIsAcceptedByItsOwnValidationDoors, and it is the reason the
// fixtures here go to the trouble of giving the group a second leaf.
//
// The second entropy gate is here for the reason the creation one is, one file section up: an
// Update exists to publish a FRESH encryption key and nothing else, and a constant in place of that
// draw still derives, still seals, still validates and still round trips. So the key is found again
// in the draws the provider was asked for rather than asserted over the line that made it.

// pendingProposalsForTest exposes this epoch's proposal cache to tests in this package.
//
// Declared here and not in group.go, which is framing_group_seams_test.go's precedent one file
// over: it is scaffolding no production caller has, and a helper spelled ForTest in the shipped
// source is a method every later reader has to work out is not part of the API.
//
// It reads through Cached rather than off the cache's map, because Pending answers REFERENCES and
// the entry behind one belongs to this epoch only if the cache is still bound to it -- which is the
// question Cached asks and a map lookup does not.
func (self *Group) pendingProposalsForTest() []CachedProposal {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	out := []CachedProposal{}
	for _, entry := range self.proposals.Pending(self.context) {
		if cached, held := self.proposals.Cached(self.context, entry.Reference); held {
			out = append(out, cached)
		}
	}
	return out
}

// testGroupWithASecondLeaf splices one more member into a founded group's ratchet tree and rebuilds
// its secret tree at the new width.
//
// IT EDITS THE GROUP'S OWN STATE ON PURPOSE and it is not a stand-in for task 13's commit: the
// group context still names the one-leaf tree hash, and this is not a group two clients could
// agree on. What it makes real is exactly the two facts the tests below cannot do without.
//
// The first is that a leaf OTHER than our own is occupied, so ProposeRemove has a member to name,
// ValSem108 has one to find and ValSem111 has a committer that is not the updating sender. Every
// one of those rules is stated over a second member, and a one-member group cannot observe any of
// them.
//
// The second is that the SECRET TREE carries a ratchet at that leaf. Without it a proposal
// misattributed to leaf 1 fails inside the key source with "leaf out of range", so the sender
// attribution test would report a failure it never actually observed -- the message would not
// exist to be attributed. With it, a misattributed proposal is a well formed message, which is the
// input that test's name claims to judge.
func testGroupWithASecondLeaf(t *testing.T, crypto CryptoProvider, group *Group, m *testMember) LeafIndex {
	t.Helper()
	leaf, _ := testLeafNode(t, crypto, m)
	at, err := group.tree.AddLeaf(leaf)
	if err != nil {
		t.Fatalf("add %s's leaf to the group's tree: %v", m.Name, err)
	}
	if at == group.OwnLeafIndex() {
		t.Fatalf("%s landed at the group's own leaf %d, so this fixture added nothing", m.Name, at)
	}
	secretTree, err := NewSecretTree(crypto, group.tree.LeafWidth(), group.schedule.Secrets().Encryption)
	if err != nil {
		t.Fatalf("rebuild the secret tree at %d leaves: %v", group.tree.LeafWidth(), err)
	}
	group.secretTree.Zeroize()
	group.secretTree = secretTree
	return at
}

// testOpenOwnProposal opens a message this group sealed, the way a PEER would.
//
// A RECEIVER'S OWN RATCHET AND NOT THE SENDER'S, which is the whole reason this is not two lines.
// NextMessageKey CONSUMES, so the generation a proposal was sealed under is already spent in the
// group's own secret tree; a peer derives the same ratchet from the same encryption secret, and
// rebuilding one here is what lets these tests read what was actually sent rather than what the
// sender happens to still hold.
//
// The signature key is resolved out of the group's own tree at the leaf the SENDER DATA names, so
// a message attributed to a leaf its signer does not occupy fails the signature check exactly as it
// would at a peer.
func testOpenOwnProposal(t *testing.T, crypto CryptoProvider, group *Group, encoded []byte) *AuthenticatedContent {
	t.Helper()
	parsed, err := ParseMLSMessage(encoded)
	if err != nil {
		t.Fatalf("ParseMLSMessage: %v", err)
	}
	if parsed.WireFormat != WireFormatPrivateMessage {
		t.Fatalf("wire format = %#x, want PrivateMessage: A-ASSUME-4 puts handshake traffic in PrivateMessage",
			parsed.WireFormat)
	}
	if parsed.PrivateMessage == nil {
		t.Fatal("the message names PrivateMessage and carries no private message arm")
	}
	receiver, err := NewSecretTree(crypto, group.tree.LeafWidth(), group.schedule.Secrets().Encryption)
	if err != nil {
		t.Fatalf("build the receiver's secret tree: %v", err)
	}
	groupContext, err := group.GroupContext()
	if err != nil {
		t.Fatalf("GroupContext: %v", err)
	}
	resolve := func(sender Sender) (SignaturePublicKey, error) {
		leaf := group.tree.Leaf(sender.LeafIndex)
		if leaf == nil {
			return nil, fmt.Errorf("no leaf at %d", sender.LeafIndex)
		}
		return leaf.SignatureKey, nil
	}
	opened, err := OpenPrivateMessage(crypto, receiver, group.schedule.Secrets().SenderData,
		parsed.PrivateMessage, resolve, groupContext)
	if err != nil {
		t.Fatalf("OpenPrivateMessage: %v", err)
	}
	return opened
}

// proposalGeneratorRow is one kind of proposal this group can generate, plus the leaf a commit
// carrying it would be made by.
//
// THE COMMITTER IS PART OF THE ROW because it is part of the rule. RFC 9420 section 12.2 refuses a
// commit that covers its own sender's Update -- the committer's leaf is reset by the UpdatePath
// instead -- so an update is judged under ANOTHER member's commit, and a row that named the
// updating leaf would be asserting that this package accepts something it is required to refuse.
type proposalGeneratorRow struct {
	name      string
	kind      ProposalType
	committer LeafIndex
	propose   func() ([]byte, error)
}

// proposalGeneratorRows is every proposal kind (*Group) can generate, which is the accepted set of
// the v1 profile's own proposal table.
//
// The rows are checked against that table in both directions by the sweep that uses them, so a
// fifth accepted type with no generator and a generator for a type the profile no longer accepts
// both fail rather than going unnoticed.
func proposalGeneratorRows(t *testing.T, crypto CryptoProvider, group *Group,
	joinerKeyPackage []byte, other LeafIndex, extensions []Extension) []proposalGeneratorRow {

	t.Helper()
	own := group.OwnLeafIndex()
	return []proposalGeneratorRow{
		{name: "add", kind: ProposalTypeAdd, committer: own, propose: func() ([]byte, error) {
			return group.ProposeAdd(joinerKeyPackage)
		}},
		{name: "remove", kind: ProposalTypeRemove, committer: own, propose: func() ([]byte, error) {
			return group.ProposeRemove(other)
		}},
		{name: "group_context_extensions", kind: ProposalTypeGroupContextExtensions, committer: own,
			propose: func() ([]byte, error) {
				return group.ProposeGroupContextExtensions(extensions)
			}},
		{name: "update", kind: ProposalTypeUpdate, committer: other, propose: func() ([]byte, error) {
			return group.ProposeUpdate()
		}},
	}
}

// proposalGenerationParts is the fixture both sweeps below are driven from.
//
// It is handed back WHOLE rather than as the three values the row sweep happens to need, because
// the second sweep reaches parts a benign row never does -- the member already occupying a leaf,
// the group's own published extension set -- and a second fixture built beside this one is a
// second group that agrees with it until the day one of them is edited.
type proposalGenerationParts struct {
	crypto CryptoProvider
	group  *Group
	owner  *testMember
	// the member already occupying a leaf other than our own, and that leaf.
	second *testMember
	other  LeafIndex
	// a joiner's key package, encoded the way a caller of ProposeAdd holds one.
	joiner []byte
	// the group's OWN published extensions. A group context extensions proposal replaces the list
	// wholesale, so this is the smallest one a real caller could send: a list that still carries
	// the policy the group runs under.
	published []Extension
}

func testProposalGenerationParts(t *testing.T) *proposalGenerationParts {
	t.Helper()
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	group := testNewGroup(t, crypto, owner, "group-1")
	t.Cleanup(func() { group.Close() })
	second := testIdentity(t, crypto, "bob")
	other := testGroupWithASecondLeaf(t, crypto, group, second)

	joiner := testIdentity(t, crypto, "carol")
	kp, _, _ := testKeyPackage(t, crypto, joiner)
	encoded, err := syntax.Marshal(kp)
	if err != nil {
		t.Fatalf("marshal the joiner's key package: %v", err)
	}
	return &proposalGenerationParts{
		crypto:    crypto,
		group:     group,
		owner:     owner,
		second:    second,
		other:     other,
		joiner:    encoded,
		published: testGroupContextOf(t, group).Extensions,
	}
}

// testProposalGenerationFixture founds a group with a second member, mints a joiner's key package
// and answers everything the row sweeps below drive.
func testProposalGenerationFixture(t *testing.T) (CryptoProvider, *Group, []proposalGeneratorRow) {
	t.Helper()
	parts := testProposalGenerationParts(t)
	crypto, group := parts.crypto, parts.group
	rows := proposalGeneratorRows(t, crypto, group, parts.joiner, parts.other, parts.published)

	// the rows ARE the accepted set of the profile's proposal table, in both directions. A fifth
	// accepted type with no generator would be a proposal kind these sweeps never judged, and a
	// generator for a type the profile no longer accepts would be a row that outlived its rule.
	generated := map[ProposalType]bool{}
	for _, row := range rows {
		if generated[row.kind] {
			t.Fatalf("two rows generate %s", proposalTypeName(row.kind))
		}
		generated[row.kind] = true
	}
	for kind, refusal := range proposalTypeProfile {
		if refusal == nil && !generated[kind] {
			t.Errorf("the v1 profile accepts %s and no row generates one, so nothing here judges it",
				proposalTypeName(kind))
		}
		if refusal != nil && generated[kind] {
			t.Errorf("a row generates %s, which the v1 profile refuses", proposalTypeName(kind))
		}
	}
	return crypto, group, rows
}

// TestEveryProposalThisGroupGeneratesIsAcceptedByItsOwnValidationDoors is the gate this task's
// whole design rests on.
//
// The receive path of this package holds ValSem101-113, section 12.1.2's two rules on an Update's
// leaf, and section 12.2's three list rules. Every one of them is a rule the SENDING side must not
// violate, and a generator that emits a proposal its own validator refuses is a defect that shows
// up as a peer refusing a commit, several steps and one epoch away from the line that caused it.
//
// It is written over ValidateProposalList and not over an assertion about what the generator does,
// because the whole point is that the two are independent: the generator gets to be right for its
// own reasons and the validator gets to judge it for the RFC's.
//
// A ONE ENTRY LIST PER ROW, deliberately. This is a claim about each proposal on its own; the
// cross-proposal rules -- two adds publishing one key, an update and a remove on one leaf -- are
// about a LIST somebody assembled and are validate_proposals.go's own tests to make.
func TestEveryProposalThisGroupGeneratesIsAcceptedByItsOwnValidationDoors(t *testing.T) {
	crypto, group, rows := testProposalGenerationFixture(t)
	context := testGroupContextOf(t, group)
	for _, row := range rows {
		before := len(group.pendingProposalsForTest())
		if _, err := row.propose(); err != nil {
			t.Fatalf("%s: %v", row.name, err)
		}
		cached := group.pendingProposalsForTest()
		if len(cached) != before+1 {
			t.Fatalf("%s: the cache held %d entries before and %d after, so this row has no entry to judge",
				row.name, before, len(cached))
		}
		one := cached[len(cached)-1]
		if one.Proposal.ProposalType != row.kind {
			t.Fatalf("%s: the cache's newest entry is a %s", row.name,
				proposalTypeName(one.Proposal.ProposalType))
		}
		if refusal := testProposalDoorsRefusal(t, crypto, group, context, row.committer,
			one); refusal != nil {

			t.Errorf("%s: this group generated a proposal its own doors refuse: %v",
				row.name, refusal)
		}
	}
}

// testProposalDoorsRefusal is this package's own judgement of ONE proposal standing alone: the
// refusal its doors answer over a single entry list, or nil.
//
// TWO DOORS AND NOT ONE. ValidateProposalList is RFC 9420 section 12.2's whole procedure.
// ValSem209 is the commit door's read of an installed extension set and is not one of section
// 12.2's list rules, so a generator held to the list rules alone is held to strictly less than a
// receiver applies -- measured: without it the generator published a set carrying
// required_capabilities twice, which every list rule accepts because none of them looks that body
// up, and which ValSem209 refuses because it reads it through the lookup that refuses a vector
// carrying one type twice.
//
// BOTH SWEEPS BELOW JUDGE THROUGH THIS ONE FUNCTION, which is the point of it being one. A
// proposal accepted by one reading of "its own doors" and refused by the other would be a
// generator that passes the sweep it is written into and fails the one it is not.
func testProposalDoorsRefusal(t *testing.T, crypto CryptoProvider, group *Group,
	context *GroupContext, committer LeafIndex, one CachedProposal) error {

	t.Helper()
	list := NewProposalList([]CachedProposal{one})
	if err := ValidateProposalList(&ProposalValidationInput{
		Crypto:    crypto,
		Tree:      group.tree,
		Context:   context,
		Committer: committer,
		List:      list,
		Now:       time.Now(),
	}); err != nil {
		return err
	}
	// AND THE SECOND DOOR IS RUN OVER EVERY PROPOSAL AND NOT OVER THE ONE KIND IT HAS SOMETHING
	// TO SAY ABOUT. ValSem209 answers nil for a list carrying no GroupContextExtensions proposal
	// -- that is its own first clause -- so naming the kind here would be this file deriving a
	// class of doors and then hand-scoping where each one is asked, which is the defect this
	// round was sent to close one file over.
	commitIn := testCommitInput(t, crypto, group.tree, list, &Commit{})
	commitIn.Context = context
	// THE ENTRY'S OWN SENDER AND NOT THE COMMITTER THIS SWEEP NAMES.
	// (*CommitValidationInput).check gives a BY VALUE entry the committer as its sender and then
	// joins the two, so a control assembled with any other sender is refused by that join rather
	// than by the rule it is here for -- measured, and it is why this line is not `committer`.
	// ValSem209 reads no committer at all, so this decides nothing the rule below asks.
	commitIn.Committer = one.Sender
	commitIn.Own = group.OwnLeafIndex()
	// erratum 8815 runs at the commit door and asks whether every reference the commit names was
	// previously received; a cached entry is where that answer comes from, and an entry a control
	// assembled by hand carries ByValue so that the question is not asked of a proposal nobody
	// ever published.
	commitIn.Pending = group.proposals
	commitIn.Now = time.Now()
	return ValSem209GroupExtensionsSupported(commitIn)
}

// ---------------------------------------------------------------------------
// the obligation, and the inputs that put a generator under it
// ---------------------------------------------------------------------------

// testKeyPackagePublishing mints a key package for m whose leaf publishes a caller-chosen
// encryption key, re-signing both the leaf and the package over it.
//
// BOTH SIGNATURES, which is testKeyPackage's own correction restated: a leaf rebound without the
// package signature is refused by KeyPackage.Validate for the signature rather than for the key,
// and a temptation refused by the wrong door tempts nothing.
func testKeyPackagePublishing(t *testing.T, crypto CryptoProvider, m *testMember,
	encryptionKey HpkePublicKey) *KeyPackage {

	t.Helper()
	kp, _, _ := testKeyPackage(t, crypto, m)
	kp.LeafNode.EncryptionKey = bytes.Clone(encryptionKey)
	if err := kp.LeafNode.Sign(crypto, m.SigPriv, nil, 0); err != nil {
		t.Fatalf("re-sign %s's leaf over the planted encryption key: %v", m.Name, err)
	}
	content, err := kp.signedPreimage()
	if err != nil {
		t.Fatalf("KeyPackage.signedPreimage(%s): %v", m.Name, err)
	}
	signature, err := crypto.SignWithLabel(m.SigPriv, keyPackageSignatureLabel, content)
	if err != nil {
		t.Fatalf("SignWithLabel(%s): %v", m.Name, err)
	}
	kp.Signature = signature
	return kp
}

// testCachedAdd is the entry an Add of this key package would be judged as, carried BY VALUE
// because no cache ever held it: this is the proposal a generator WOULD have emitted, assembled
// with the generator out of the way.
func testCachedAdd(parts *proposalGenerationParts, kp *KeyPackage) CachedProposal {
	return CachedProposal{
		Proposal: Proposal{ProposalType: ProposalTypeAdd, Add: &Add{KeyPackage: *kp}},
		Sender:   parts.group.OwnLeafIndex(),
		ByValue:  true,
	}
}

// proposalGeneratorTemptation is one call a caller can make that would put a proposal this
// package's own doors refuse into this epoch's cache.
//
// THE REFUSAL IS NAMED, and naming it is the whole of what makes a temptation one. A shape refused
// for some unrelated reason -- a key package that does not validate, a leaf outside the tree while
// the rule under test is about keys -- satisfies every assertion a sweep could make while tempting
// nothing at all, which is exactly how the row for ProposeAdd came to be a joiner nobody in the
// group had ever met. The value is asserted at BOTH ends below: the doors must answer it over the
// proposal the call describes, and the generator must refuse the call with it.
type proposalGeneratorTemptation struct {
	name    string
	kind    ProposalType
	refusal error
	// the input, the proposal it describes, and the call that would make it. ONE function,
	// because the control and the call have to be about the same input: a control built from a
	// second draw is a control of something else.
	tempt func(t *testing.T, parts *proposalGenerationParts) (CachedProposal, func() ([]byte, error))
}

// proposalGeneratorTemptations is one input per generator that this package's own doors refuse.
//
// The set is held to the profile's proposal table by the sweep that uses it, in both directions,
// so a fifth accepted type is inside this obligation by existing rather than by somebody
// remembering to add a row.
func proposalGeneratorTemptations() []proposalGeneratorTemptation {
	return []proposalGeneratorTemptation{
		{
			// THE MEASURED ONE. ProposeAdd ran (*KeyPackage).Validate and LeafKeysOf and nothing
			// else, and section 10.1 judges a key package against itself and a suite -- so a
			// second device for a member who is already here is a well formed, correctly signed,
			// unexpired package that this group ACCEPTED, signed, sealed and cached, while
			// ValSem101 refuses it as a one entry list against the members the group already has.
			name:    "add/a key package publishing a signature key this group already carries",
			kind:    ProposalTypeAdd,
			refusal: ErrAddDuplicateSignatureKey,
			tempt: func(t *testing.T, parts *proposalGenerationParts) (CachedProposal, func() ([]byte, error)) {
				kp, _, _ := testKeyPackage(t, parts.crypto, parts.second)
				encoded, err := syntax.Marshal(kp)
				if err != nil {
					t.Fatalf("marshal the second device's key package: %v", err)
				}
				return testCachedAdd(parts, kp), func() ([]byte, error) {
					return parts.group.ProposeAdd(encoded)
				}
			},
		},
		{
			// the other half of the same finding: ValSem103's members' half reads the same
			// in.Tree.NonBlankLeaves ValSem101's does, so a fix that asked one question and not
			// the other would leave a joiner republishing a member's encryption key.
			name:    "add/a key package republishing a member's encryption key",
			kind:    ProposalTypeAdd,
			refusal: ErrAddDuplicateEncryptionKey,
			tempt: func(t *testing.T, parts *proposalGenerationParts) (CachedProposal, func() ([]byte, error)) {
				occupied := parts.group.tree.Leaf(parts.other)
				if occupied == nil {
					t.Fatalf("this fixture holds no leaf at %d, so there is no key to republish",
						parts.other)
				}
				kp := testKeyPackagePublishing(t, parts.crypto,
					testIdentity(t, parts.crypto, "dave"), occupied.EncryptionKey)
				encoded, err := syntax.Marshal(kp)
				if err != nil {
					t.Fatalf("marshal the republishing key package: %v", err)
				}
				return testCachedAdd(parts, kp), func() ([]byte, error) {
					return parts.group.ProposeAdd(encoded)
				}
			},
		},
		{
			// the generator that ALREADY asked its own doors' question, which is why the
			// asymmetry this sweep closes was visible in one file: ProposeRemove answers
			// ValSem108's value from ValSem108's own reading of the tree.
			name:    "remove/a leaf this group's tree does not hold",
			kind:    ProposalTypeRemove,
			refusal: ErrRemoveNonMember,
			tempt: func(t *testing.T, parts *proposalGenerationParts) (CachedProposal, func() ([]byte, error)) {
				beyond := LeafIndex(parts.group.tree.LeafWidth())
				return CachedProposal{
						Proposal: Proposal{ProposalType: ProposalTypeRemove,
							Remove: &Remove{Removed: beyond}},
						Sender:  parts.group.OwnLeafIndex(),
						ByValue: true,
					}, func() ([]byte, error) {
						return parts.group.ProposeRemove(beyond)
					}
			},
		},
		{
			// ProposeUpdate takes no argument, so the input is the entropy. The control is the
			// proposal this generator ITSELF produced from that draw, captured before the leaf it
			// replaces was made to hold the same key -- so the shape being judged is exactly the
			// shape the second call would emit, rather than one assembled by hand beside it.
			name:    "update/a draw that republishes the key the leaf already holds",
			kind:    ProposalTypeUpdate,
			refusal: ErrUpdateEncryptionKeyUnchanged,
			tempt: func(t *testing.T, parts *proposalGenerationParts) (CachedProposal, func() ([]byte, error)) {
				ikm := bytes.Repeat([]byte{0x5c}, parts.crypto.HashSize())
				_, drawn, err := parts.crypto.DeriveKeyPair(ikm)
				if err != nil {
					t.Fatalf("DeriveKeyPair over the fixed draw: %v", err)
				}
				// key_schedule_test.go's own provider, which answers one value from every draw of
				// that value's width and falls through for any other. A second one written here
				// would be a second answer to the same question, and this is the only input
				// ProposeUpdate has: it takes no argument, so the entropy its replacement key
				// comes out of is the whole of what a caller can vary about it.
				parts.group.crypto = &fixedRandomProvider{CryptoProvider: parts.crypto, value: ikm}
				before := len(parts.group.pendingProposalsForTest())
				if _, err := parts.group.ProposeUpdate(); err != nil {
					t.Fatalf("the first update under the fixed draw was refused: %v; this temptation needs one accepted proposal to be the shape of the second",
						err)
				}
				cached := parts.group.pendingProposalsForTest()
				if len(cached) != before+1 {
					t.Fatalf("the first update under the fixed draw cached nothing")
				}
				control := cached[len(cached)-1]
				if control.Proposal.Update == nil {
					t.Fatalf("the cache's newest entry is not an update")
				}
				if !bytes.Equal(control.Proposal.Update.LeafNode.EncryptionKey, drawn) {
					t.Fatalf("the update published %x and the fixed draw derives %x; this generator is not drawing through the provider it was handed",
						control.Proposal.Update.LeafNode.EncryptionKey, drawn)
				}
				// and now the leaf it replaces holds that key, so the SAME draw republishes it
				own := parts.group.tree.Leaf(parts.group.OwnLeafIndex())
				if own == nil {
					t.Fatalf("this group holds no leaf at its own index")
				}
				own.EncryptionKey = bytes.Clone(drawn)
				return control, func() ([]byte, error) { return parts.group.ProposeUpdate() }
			},
		},
		{
			// a set ValidateProposalList ACCEPTS and a receiver refuses, which is why the judge
			// above has two doors rather than one. No list rule of section 12.2 reads the extension
			// TYPES a group_context_extensions proposal installs -- the commit door does, through
			// the same profile gate this generator runs -- so with ValSem209 out of the judge this
			// temptation reports the doors as accepting a set no peer would install. Measured:
			// without it, dropping the commit door's arm from the judge left every gate green.
			name:    "group_context_extensions/a set installing an extension this profile refuses",
			kind:    ProposalTypeGroupContextExtensions,
			refusal: errProfileGroupExtension,
			tempt: func(t *testing.T, parts *proposalGenerationParts) (CachedProposal, func() ([]byte, error)) {
				outside := append(slices.Clone(parts.published),
					Extension{ExtensionType: ExtensionTypeApplicationId,
						ExtensionData: []byte{0x01}})
				return CachedProposal{
						Proposal: Proposal{ProposalType: ProposalTypeGroupContextExtensions,
							GroupContextExtensions: &GroupContextExtensions{Extensions: outside}},
						Sender:  parts.group.OwnLeafIndex(),
						ByValue: true,
					}, func() ([]byte, error) {
						return parts.group.ProposeGroupContextExtensions(outside)
					}
			},
		},
		{
			// this one every list rule accepts as well, because none of them looks that body up,
			// and ValSem106 reaches the same lookup on its own account -- so it is judged by both
			// doors rather than by the second alone.
			name:    "group_context_extensions/a set carrying required_capabilities twice",
			kind:    ProposalTypeGroupContextExtensions,
			refusal: ErrMalformedExtension,
			tempt: func(t *testing.T, parts *proposalGenerationParts) (CachedProposal, func() ([]byte, error)) {
				entry, found, err := FindExtensionEntry(parts.published,
					ExtensionTypeRequiredCapabilities)
				if err != nil || !found {
					t.Fatalf("the group publishes no single required_capabilities extension (found %v): %v",
						found, err)
				}
				doubled := append(slices.Clone(parts.published), entry)
				return CachedProposal{
						Proposal: Proposal{ProposalType: ProposalTypeGroupContextExtensions,
							GroupContextExtensions: &GroupContextExtensions{Extensions: doubled}},
						Sender:  parts.group.OwnLeafIndex(),
						ByValue: true,
					}, func() ([]byte, error) {
						return parts.group.ProposeGroupContextExtensions(doubled)
					}
			},
		},
	}
}

// TestNoGeneratorOnThisGroupEmitsAProposalItsOwnDoorsRefuse is the obligation, stated once over
// every generator rather than as a check inside each of them.
//
// THE SWEEP ABOVE MEASURED THE WRONG THING BY MEASURING ONLY GOOD INPUTS. Every row of it hands a
// generator an input nothing could object to -- a joiner nobody has met, the group's own extension
// set re-proposed -- so it establishes that the generators work and says nothing about what they
// do with an input their own doors refuse. Measured with this group's own doors: a key package for
// a member already occupying a leaf, carrying that member's own signature key and a fresh
// encryption key, was ACCEPTED by ProposeAdd -- signed, sealed and cached -- and
// ValidateProposalList refuses that same entry as a ONE ENTRY list. Judged alone, so the build's
// stated defence that "the cross-proposal rules are the committer's" does not reach it: ValSem101
// and ValSem103 each have a members' half read off in.Tree.NonBlankLeaves, which is this group's
// own pre-commit tree and is decidable the moment the proposal is built.
//
// SO THE OBLIGATION IS DERIVED AND NOT ENUMERATED, in three ways that matter:
//
//   - it is stated over EVERY generator. The temptations are held to the v1 profile's own proposal
//     table in both directions, exactly as the rows are, so a fifth accepted type is inside this
//     test by existing;
//   - the doors are the doors, not a transcription of two rules. testProposalDoorsRefusal is what
//     the sweep above judges through as well, so a rule added to section 12.2 tomorrow is asked of
//     every generator without a line changing here;
//   - and the answer is the same VALUE at both ends. A generator must refuse with the value its
//     own doors answer, which is what says it asked their question rather than a question of its
//     own that happens to refuse the same input today.
func TestNoGeneratorOnThisGroupEmitsAProposalItsOwnDoorsRefuse(t *testing.T) {
	temptations := proposalGeneratorTemptations()
	tempted := map[ProposalType]bool{}
	for _, one := range temptations {
		tempted[one.kind] = true
	}
	for kind, refusal := range proposalTypeProfile {
		if refusal == nil && !tempted[kind] {
			t.Errorf("the v1 profile accepts %s and this group generates one, and no temptation drives that generator with an input its own doors refuse; the obligation is not stated over it",
				proposalTypeName(kind))
		}
		if refusal != nil && tempted[kind] {
			t.Errorf("a temptation drives a %s generator, which the v1 profile refuses",
				proposalTypeName(kind))
		}
	}

	for _, one := range temptations {
		// a fixture per temptation, because a temptation is allowed to edit the group it tempts:
		// the update one plants a key in the tree and hands the group a provider that draws it
		// again, and a second temptation reading that state would be measuring the first one's
		// leftovers.
		parts := testProposalGenerationParts(t)
		context := testGroupContextOf(t, parts.group)
		// THE COMMITTER IS A LEAF OUTSIDE THIS TREE, which is (*Group).propose's own reading and
		// is here for its reason: exactly two rules of section 12.2 are stated over the committer,
		// and neither is decidable while a proposal is being generated.
		committer := LeafIndex(parts.group.tree.LeafWidth())
		control, call := one.tempt(t, parts)

		if refusal := testProposalDoorsRefusal(t, parts.crypto, parts.group, context, committer,
			control); !errors.Is(refusal, one.refusal) {

			t.Errorf("%s: this package's own doors answer %v over the proposal this call describes, and the temptation is written for %v; a temptation the doors accept -- or refuse for another reason -- measures nothing about the generator",
				one.name, refusal, one.refusal)
			continue
		}

		before := len(parts.group.pendingProposalsForTest())
		if _, err := call(); err != nil {
			if !errors.Is(err, one.refusal) {
				t.Errorf("%s: the generator refused with %v and its own doors refuse this proposal with %v; a generator that refuses for another reason is not asking the question its receivers will",
					one.name, err, one.refusal)
			}
			if after := len(parts.group.pendingProposalsForTest()); after != before {
				t.Errorf("%s: the refusal left this epoch's cache holding %d entries and it held %d; a proposal this package refuses must not be one this client's own next commit would name",
					one.name, after, before)
			}
			continue
		}
		cached := parts.group.pendingProposalsForTest()
		if len(cached) == before {
			t.Errorf("%s: the generator accepted an input whose proposal its own doors refuse with %v, and cached nothing",
				one.name, one.refusal)
			continue
		}
		t.Errorf("%s: this group generated and cached a proposal its own doors refuse: they answer %v over what it cached, and %v over the proposal the call describes",
			one.name,
			testProposalDoorsRefusal(t, parts.crypto, parts.group, context, committer,
				cached[len(cached)-1]),
			one.refusal)
	}
}

// The door every proposal this group emits has to pass, and the content type that says a message
// IS a proposal.
//
// These are the ANCHORS of the rule below and not its class. Both are checked against this
// package's own declarations before anything is derived from them, so a rename that moved one
// fails here rather than quietly leaving the rule reading nothing.
const (
	proposalGenerationDoor        = "ValidateProposalList"
	proposalGenerationContentType = "ContentTypeProposal"
	// the SEED of the position half and not the position itself. A declaration is in the class
	// when a group stands in a position its caller fills, over the closure seamCarriersOf
	// derives, so a free function taking a *Group and a method on a type holding one are inside
	// it. Reading this name as the receiver is what left both outside.
	proposalGenerationReceiver = "Group"
)

// framedContentCarrier is the structure a sender assembles to say what it is sending, the field of
// it that says which arm is populated, and the type of that field.
//
// THE FIELD IS FOUND BY ITS TYPE AND NOT BY ITS NAME, which is what makes this an anchor rather
// than a transcription: the structure declares exactly one field of the content type, so a rename
// of either moves with the compiled structure and a SECOND discriminant -- two fields saying which
// arm is populated -- answers the empty string and fails the check at the reading's own gate,
// rather than leaving that reading pointed at whichever of the two somebody had written down.
func framedContentCarrier() (carrier string, discriminant string, kind string) {
	structure := reflect.TypeOf(FramedContent{})
	declared := reflect.TypeOf(ContentType(0))
	found := []string{}
	for i := range structure.NumField() {
		if structure.Field(i).Type == declared {
			found = append(found, structure.Field(i).Name)
		}
	}
	if len(found) != 1 {
		return structure.Name(), "", declared.Name()
	}
	return structure.Name(), found[0], declared.Name()
}

// declarationsReaching answers which declarations of the scan reach target, directly by the
// reading `direct` gives and otherwise through another declaration of the scan that does, along
// the edges `edges` gives.
//
// TWO QUESTIONS ARE ASKED THROUGH IT AND THEY READ DIFFERENT EDGES, which is the whole reason
// the reading is an argument. "Did this declaration CHOOSE the proposal content type" is a
// question about identifiers in value positions, and its edges are every name a body carries,
// because a generator that delegates its framing to a helper has still chosen what it sends.
// "Did this declaration RUN its doors" is a question about a call whose answer is used, and its
// edges are the calls whose answers are used, because a body that merely names a validator has
// not validated and neither has one that calls it and drops the error.
//
// THE EDGE CONDITION IS THE OPPOSITE OF seamsReachingTheWireDoor'S, and the asymmetry is the whole
// design rather than an inconsistency between two walks. That one asks whether a forge REACHES the
// wire, so following a name into every declaration carrying it over-reports, and an over-report
// there is a failing test somebody has to answer for. This one asks whether a generator reached
// its doors, and the same over-report would CERTIFY a generator that never called them: a fifth
// generator whose helper happened to share a bare name with something that does validate would
// inherit that verdict, and MarshalMLS alone is carried by fifty-two declarations of this scan.
//
// So an edge is followed only when EVERY declaration carrying the name reaches the target. That
// under-reports, and under-reporting here is a generator wrongly said not to have called its doors
// -- a failing test rather than a silent pass, which is the direction an obligation has to fail in.
func declarationsReaching(candidates []seamCandidate, target string,
	direct func(seamCandidate) bool, edges func(seamCandidate) map[string]bool) map[string]bool {

	byBareName := map[string][]int{}
	for index, candidate := range candidates {
		byBareName[candidate.bare] = append(byBareName[candidate.bare], index)
	}
	reaches := map[string]bool{}
	for grew := true; grew; {
		grew = false
		for _, candidate := range candidates {
			if reaches[candidate.name] {
				continue
			}
			if direct(candidate) {
				reaches[candidate.name] = true
				grew = true
				continue
			}
			for named := range edges(candidate) {
				held := byBareName[named]
				if len(held) == 0 {
					continue
				}
				followed := true
				for _, index := range held {
					if !reaches[candidates[index].name] {
						followed = false
						break
					}
				}
				if followed {
					reaches[candidate.name] = true
					grew = true
					break
				}
			}
		}
	}
	return reaches
}

// proposalGeneratorsIn answers the class and the members of it that fail the obligation, so the
// control below and the real scan are judged by one piece of code rather than by two that agree
// until the day one of them is edited.
func proposalGeneratorsIn(parsed []parsedSource) (generators []seamCandidate, missing []seamCandidate) {
	candidates := seamCandidatesIn(parsed)
	reachesTheWire := seamsReachingTheWireDoor(candidates, seamWireDoorsIn(candidates))
	// what a declaration CHOOSES to send, over every name its body carries. A generator that
	// delegates the framing to a helper has still chosen the content type, so the edges here are
	// names; what is read at the end of them is the value position rather than the mention.
	emitsAProposal := declarationsReaching(candidates, proposalGenerationContentType,
		func(candidate seamCandidate) bool {
			// what it CHOSE, or -- for a body that assembles the carrier and sets the
			// discriminant from something this reading cannot pin to a named content type --
			// whatever it was handed, a cached proposal included. The two are a UNION because
			// each is the other's blind spot: the first cannot see a re-framer, and the second
			// cannot see a generator that delegates its framing to a helper.
			return candidate.chooses[proposalGenerationContentType] || candidate.framesUnknown
		},
		func(candidate seamCandidate) map[string]bool { return candidate.names })
	// and whether it RAN its door, over the calls whose answer it uses. Both halves of that are
	// measured: a body naming the door certifies nothing, and neither does one calling it and
	// dropping the answer.
	runsTheDoor := declarationsReaching(candidates, proposalGenerationDoor,
		func(candidate seamCandidate) bool { return candidate.consumes[proposalGenerationDoor] },
		func(candidate seamCandidate) map[string]bool { return candidate.consumes })
	for _, candidate := range candidates {
		if !candidate.holdsTheGroup || !candidate.exported {
			continue
		}
		if !reachesTheWire[candidate.name] || !emitsAProposal[candidate.name] {
			continue
		}
		generators = append(generators, candidate)
		if !runsTheDoor[candidate.name] {
			missing = append(missing, candidate)
		}
	}
	byName := func(a seamCandidate, b seamCandidate) int { return strings.Compare(a.name, b.name) }
	slices.SortFunc(generators, byName)
	slices.SortFunc(missing, byName)
	return generators, missing
}

// A package holding one of each shape the class must separate, so a conjunct that stopped doing
// work fails here rather than being carried by the other one.
//
// SIX POSITIVES, and every one of them has to be REPORTED. One reaches its door only through a
// helper whose bare name a SECOND declaration also carries, because nothing in the scan says
// which of the two it calls: one runs the door and one runs nothing, so certifying it off the
// first is certifying it off a coin toss. Two more are the door reading itself -- one MENTIONS
// ValidateProposalList and never calls it, one CALLS it and throws the refusal away -- and both
// were measured to certify a generator that validates nothing. The last two are the position:
// a free function handed the group, and a method on a type that holds one. Neither has a
// receiver spelled Group and both send what a method on the group sends. The sixth sets its
// content type from a VARIABLE -- a cached proposal re-framed -- which names the proposal
// content type in no value position and is what the narrowed reading stopped seeing.
//
// FOUR NEGATIVES, each a conjunct of the class asked on its own. One reaches the same wire door
// carrying a COMMIT, which is what says the class is read off what a message CARRIES -- the
// commit path runs validate_commit.go's own rules and calls ValidateProposalList nowhere, so a
// class read off the wire door alone reports this package's own committer as a generator that
// skipped its doors. One names the proposal content type while sending nothing, which is every
// accessor that reads one, and is what says the class is read off what reaches the WIRE. One
// SENDS AN APPLICATION MESSAGE through the demultiplexer every sender in this package goes
// through, which is the shape the rule was measured to fire on while the reading was a mention.
// And one frames a proposal, reaches the wire and is handed no group at all, which is what keeps
// the derived position from being no position.
const proposalGeneratorControl = `package control

func (self *Group) ProposeThroughAnAmbiguouslyNamedHelper() ([]byte, error) {
	content := &FramedContent{ContentType: ContentTypeProposal}
	if err := judges(content); err != nil {
		return nil, err
	}
	return MarshalMLSMessage(&MLSMessage{})
}

// the declaration that DOES run the door.
func judges(content *FramedContent) error {
	return ValidateProposalList(nil)
}

// and the second declaration of that bare name, which runs nothing. It is the whole of what makes
// the edge above ambiguous.
func (self *Group) judges(content *FramedContent) error {
	return nil
}

// the same wire door, carrying a commit rather than a proposal.
func (self *Group) CommitSomething() ([]byte, error) {
	content := &FramedContent{ContentType: ContentTypeCommit}
	_ = content
	return MarshalMLSMessage(&MLSMessage{})
}

// naming the proposal content type and sending nothing.
func (self *Group) PendingProposalKinds() []ContentType {
	return []ContentType{ContentTypeProposal}
}

// an APPLICATION sender, which is the shape this rule was measured to FIRE ON. It reaches the
// wire through the same demultiplexer every real generator reaches it through, and what it puts
// there is not a proposal.
func (self *Group) SendReviewApplication(data []byte) ([]byte, error) {
	content := &FramedContent{ContentType: ContentTypeApplication, ApplicationData: data}
	return sealsWhicheverArmIsPopulated(content)
}

// the demultiplexer both of them go through: every content type NAMED, none of them CHOSEN.
// marshalPrivateMessageContentWithPadding is this shape in production, and reading its mentions
// is what put every application sender into a rule about proposals.
func sealsWhicheverArmIsPopulated(content *FramedContent) ([]byte, error) {
	switch content.ContentType {
	case ContentTypeApplication:
		_ = content.ApplicationData
	case ContentTypeProposal:
		_ = content.Proposal
	case ContentTypeCommit:
		_ = content.Commit
	}
	return MarshalMLSMessage(&MLSMessage{})
}

// a generator that MENTIONS the door and never runs it.
func (self *Group) ProposeAndMentionsTheDoor() ([]byte, error) {
	_ = ValidateProposalList
	content := &FramedContent{ContentType: ContentTypeProposal}
	_ = content
	return MarshalMLSMessage(&MLSMessage{})
}

// and one that RUNS it and throws the answer away, which is the same certificate with a call
// expression in it: the door answers a refusal and this body is not looking at it.
func (self *Group) ProposeAndDiscardsTheDoorsAnswer() ([]byte, error) {
	content := &FramedContent{ContentType: ContentTypeProposal}
	_ = content
	ValidateProposalList(nil)
	return MarshalMLSMessage(&MLSMessage{})
}

// a generator that RE-FRAMES a cached proposal. It asks what it was handed -- a comparison, and
// a comparison is not a choice -- and then sends whatever that was, so ContentTypeProposal
// stands in no value position of this body at all. The reading that narrowed "emits a proposal"
// to the chosen identifier does not see it, and the mention-based reading it replaced did.
func (self *Group) ReframeACachedProposal(cached *FramedContent) ([]byte, error) {
	if cached.ContentType != ContentTypeProposal {
		return nil, errNotAProposal
	}
	content := &FramedContent{ContentType: cached.ContentType, Proposal: cached.Proposal}
	_ = content
	return MarshalMLSMessage(&MLSMessage{})
}

// a free function handed the group, and a method on a type that HOLDS one. Both send exactly
// what a method on *Group sends, and a rule that named the position had neither.
func ProposeOverAGroupItWasHanded(group *Group) ([]byte, error) {
	_ = group
	content := &FramedContent{ContentType: ContentTypeProposal}
	_ = content
	return MarshalMLSMessage(&MLSMessage{})
}

type reviewProposalSender struct {
	group *Group
}

func (self *reviewProposalSender) ProposeThroughAHolderOfTheGroup() ([]byte, error) {
	content := &FramedContent{ContentType: ContentTypeProposal}
	_ = content
	return MarshalMLSMessage(&MLSMessage{})
}

// and the other side of that position: nothing hands this one a group, so no epoch's keys are
// reachable from it and it seals nothing any peer would accept. It frames a proposal and reaches
// the wire door, which is every other conjunct of the class.
func ProposeWithNoGroupAnywhere() ([]byte, error) {
	content := &FramedContent{ContentType: ContentTypeProposal}
	_ = content
	return MarshalMLSMessage(&MLSMessage{})
}
`

// TestTheProposalGeneratorObligationReadsItsControl runs the rule on a package known to hold
// every shape it must separate, so a rule narrowed or widened by an edit fails here rather than
// reporting the real package clean -- which is the one outcome an obligation must never produce by
// accident.
//
// The AMBIGUOUS EDGE is the half real source cannot show, because the walk under-reports on
// purpose. declarationsNamingThrough follows an edge only when EVERY declaration carrying the name
// reaches the target; read the other way -- the way seamsReachingTheWireDoor reads it, correctly,
// for a question whose over-report is a failing test -- a generator inherits the verdict of a
// namesake it never calls. That is not a rare shape here: MarshalMLS alone is carried by
// fifty-two declarations of this scan, so a helper renamed to collide with any of them is free.
//
// The NEGATIVES are each conjunct of the class asked on its own, and each was measured to be
// necessary the only way that means anything: with its conjunct removed, one of them enters the
// class and is reported as a generator that never called its doors. The application sender is the
// one that was not hypothetical -- the rule as written reported it, in the real scan, as a method
// that puts a proposal on the wire.
func TestTheProposalGeneratorObligationReadsItsControl(t *testing.T) {
	control := []parsedSource{
		mustParseText(t, "proposal_generator_control.go", proposalGeneratorControl)}
	helpers := 0
	for _, candidate := range seamCandidatesIn(control) {
		if candidate.bare == "judges" {
			helpers += 1
		}
	}
	if helpers < 2 {
		t.Fatalf("the control declares %d declaration(s) named judges; this edge is ambiguous only while two carry the name, so the control has stopped holding the shape it is here for",
			helpers)
	}
	generators, missing := proposalGeneratorsIn(control)
	// every shape the class must REPORT, with what each of them is here to hold. Each is in the
	// class -- it is handed a group, it frames a proposal and it answers the wire door -- and
	// none of them has run its doors, so every one of them has to appear in both answers.
	inside := map[string]string{
		"Group.ProposeThroughAnAmbiguouslyNamedHelper":         "its only route to the door is a helper two declarations spell the same way, one of which runs nothing; a walk that followed that edge would certify off a coin toss",
		"Group.ProposeAndMentionsTheDoor":                      "it names the door and never calls it, which is what a name-reachability reading certifies",
		"Group.ProposeAndDiscardsTheDoorsAnswer":               "it calls the door and drops the refusal, which is the same certificate with a call expression in it",
		"ProposeOverAGroupItWasHanded":                         "the group stands in a parameter rather than the receiver, which is a position a rule reading the receiver has no name for",
		"reviewProposalSender.ProposeThroughAHolderOfTheGroup": "the group is held by the receiver's own type, which is the other half of that same position",
		"Group.ReframeACachedProposal":                         "it sets the content type it sends from a VARIABLE, so it names the proposal content type in no value position -- it sends whatever it was handed, and a cached proposal is one of the things it can be handed",
	}
	for _, generator := range slices.Sorted(maps.Keys(inside)) {
		if !slices.Contains(seamNamesOf(generators), generator) {
			t.Errorf("the class read %v and %s is not in it -- %s. It frames a proposal and answers %s, so a class that leaves it out is not stated over what a generator DOES",
				seamNamesOf(generators), generator, inside[generator], seamWireDoor)
			continue
		}
		if !slices.Contains(seamNamesOf(missing), generator) {
			t.Errorf("%s is certified as reaching %s, and %s; an obligation that over-reports certifies the generator it was written to catch",
				generator, proposalGenerationDoor, inside[generator])
		}
	}
	outside := map[string]string{
		"Group.CommitSomething":       "it reaches the same wire door carrying a commit, and section 12.2's proposal rules are not stated over a committer",
		"Group.PendingProposalKinds":  "it names the proposal content type and sends nothing, which is every accessor that reads one",
		"Group.SendReviewApplication": "it sends an APPLICATION message through the demultiplexer every sender goes through, and a rule that read that demultiplexer's mentions reported it as putting a proposal on the wire -- a gate firing on correct code, which is how gates get switched off",
		"ProposeWithNoGroupAnywhere":  "nothing hands it a group, so no epoch's keys are reachable from it and there is no group whose doors it could have skipped",
	}
	for _, name := range slices.Sorted(maps.Keys(outside)) {
		if slices.Contains(seamNamesOf(generators), name) {
			t.Errorf("%s is read as a proposal generator and %s; the class read %v, so a conjunct of it has stopped doing work",
				name, outside[name], seamNamesOf(generators))
		}
	}
}

// TestEveryProposalGeneratorOnThisGroupSendsWhatItEmitsThroughItsOwnDoors states the obligation
// above over the GENERATORS, which is the half it was resting on and not asserting.
//
// TestNoGeneratorOnThisGroupEmitsAProposalItsOwnDoorsRefuse is stated over every proposal TYPE the
// v1 profile accepts, reached through (*Group).propose -- and it is a statement about the four
// generators only while all four go through that one method. That is true today, and nothing said
// so. Measured, not assumed: a FIFTH generator doing its own framing, signing, sealing and
// proposals.Store passes that obligation untouched while putting 1670 octets on the wire over a
// proposal this package's own doors refuse. Its four failures were roster lines; not one of them
// mentions a validation door.
//
// SO THE CLASS IS DERIVED FROM WHAT A GENERATOR DOES, in three conjuncts and no one of them
// alone:
//
//   - a GROUP stands in a position its caller fills, over seamCarriersOf's closure and not over
//     the receiver. This half was written as candidate.receiver == "Group", which is the very
//     defect the same commit had just fixed for the seam gate one file over: a free function
//     taking a *Group, and a method on a type holding one, seal under this epoch's real keys and
//     put exactly what a method on the group puts on the wire, and both were outside a class
//     stated over what a generator DOES;
//   - it puts a message on the WIRE, read through the seam gate's own derived doors, so a
//     generator that answers octets under any spelling is inside this by existing. Reading the
//     position alone would put every accessor of a group in the class;
//   - and what it puts there is a PROPOSAL, read as the content type the declaration CHOOSES --
//     an identifier in a value position -- and not as one it names. The mention was measured to
//     do no work and worse than none: every sender in this package reaches
//     marshalPrivateMessageContentWithPadding, whose switch names all three content types, so the
//     conjunct was satisfied by reaching the send path at all. It put this group's own committer
//     in a class about proposals, and it made the rule FIRE on a correct exported method that
//     sends an application message -- reported as putting a proposal on the wire and reaching its
//     doors through nothing. A gate that fires on correct code is worse than one that misses,
//     because it teaches the next person to switch gates off. And the narrowing traded one
//     under-report for another, so the conjunct is the UNION of two readings of one question:
//     the content type a declaration CHOOSES, and the content type of the carrier it ASSEMBLES
//     -- which is every content type when the discriminant is set from an expression this
//     reading cannot pin to a named one. A generator re-framing a cached proposal chooses
//     nothing and frames whatever it was handed; a generator delegating its framing to a helper
//     chooses and assembles nothing. Each reading is the other's blind spot, and "cannot tell"
//     resolving to "every content type" is the direction this obligation has to fail in.
//
// And the obligation itself is ONE name RUN: whatever else a generator does, its output has to
// have gone through this package's single entry ValidateProposalList, reached along calls whose
// ANSWER IS USED. The name alone certifies nothing -- `_ = ValidateProposalList` in a body was
// measured to satisfy it, and so was calling the door and dropping the refusal it answered.
// (*Group).propose runs it over a one entry list inside an `if err :=`; a generator that reaches
// it through any other route satisfies this, and one that reaches it through none does not,
// whatever hand written checks it runs instead.
func TestEveryProposalGeneratorOnThisGroupSendsWhatItEmitsThroughItsOwnDoors(t *testing.T) {
	declared := packageLevelDeclarations(t, ".")
	for _, anchor := range []string{proposalGenerationDoor, proposalGenerationContentType,
		proposalGenerationReceiver} {
		file, isDeclared := declared[anchor]
		if !isDeclared {
			t.Fatalf("this package declares no %s, so the rule below is anchored on a name that has moved and reads nothing",
				anchor)
		}
		if strings.HasSuffix(file, "_test.go") {
			t.Fatalf("%s is declared in %s; all three anchors are production's own, and one that had become the test binary's would make every generator its own excuse",
				anchor, file)
		}
	}

	// and the two the "sends whatever it was handed" reading is rooted in, both derived rather
	// than written down: the discriminant off the COMPILED structure by its type, the constants
	// off this package's own source. An empty constant set would read every write as "cannot
	// tell" and put every sender in this package into a class about proposals, and a structure
	// with no single discriminant would leave that reading pointed at nothing.
	carrier, discriminant, kind := framedContentCarrier()
	if discriminant == "" {
		t.Fatalf("%s declares no single field of type %s, and the reading that catches a generator setting its content type from a variable is rooted in that field",
			carrier, kind)
	}
	constants := theContentTypeConstants()
	if !constants[proposalGenerationContentType] {
		t.Fatalf("this package's own source declares %d constant(s) of %s and %s is not among them, so nothing here can tell a body that NAMED a content type from one that wrote an expression",
			len(constants), kind, proposalGenerationContentType)
	}
	if len(constants) < 2 {
		t.Fatalf("this package declares %d constant(s) of %s; with one, every write of any other content type reads as \"cannot tell\" and every sender in the package is a proposal generator",
			len(constants), kind)
	}

	// both roots the guardrails walk, for the seam gate's reason: the door class is derived from
	// exported names, and scoping a derivation to the directory its first members happen to sit in
	// is the defect this project has paid for more than once.
	scan := mustScanSources(t, forbiddenScanRoots)
	parsed := []parsedSource{}
	for _, path := range slices.Sorted(maps.Keys(scan.sourceTexts)) {
		parsed = append(parsed, mustParseText(t, path, scan.sourceTexts[path]))
	}
	generators, missing := proposalGeneratorsIn(parsed)

	// pinned against what this package has, in both directions. A class that stopped reading the
	// generators reports exactly what a compliant package reports, which is the one outcome an
	// obligation must never produce by accident; and a class that swept in an accessor would be
	// asking section 12.2's proposal rules of a method that sends no proposal.
	accepted := 0
	for _, refusal := range proposalTypeProfile {
		if refusal == nil {
			accepted += 1
		}
	}
	if len(generators) < accepted {
		t.Fatalf("the class read %d generator(s) -- %v -- and the v1 profile accepts %d proposal type(s) this group generates one of each of; the derivation is not reading the generators it is stated over",
			len(generators), seamNamesOf(generators), accepted)
	}

	for _, generator := range missing {
		t.Errorf("%s (%s) puts a proposal on the wire and reaches %s through nothing; every rule of section 12.2 that is decidable off this group's own pre-commit tree is then a rule its receivers apply and it does not, and the refusal arrives at a PEER one commit later",
			generator.name, generator.file, proposalGenerationDoor)
	}
	t.Logf("%d proposal generator(s) on *Group, every one of them through %s: %v",
		len(generators), proposalGenerationDoor, seamNamesOf(generators))
}

// TestEveryProposalThisGroupGeneratesNamesItsOwnLeafAsSender reads the sender out of what was
// actually SENT.
//
// The sender is the one field of a proposal nothing else can supply. It travels in the encrypted
// sender data, so a peer learns it from the message and from nowhere else; ValSem111 compares it
// against the committer, section 12.1.2 rebuilds the update leaf's own preimage from it, and the
// cache keys its per sender ceilings on it. A generator that attributed its proposals to another
// leaf would produce messages that decrypt, parse and carry a perfectly good proposal -- and every
// rule stated over a sender would then be a rule about somebody else.
//
// Both readings are taken and they are different claims: the CACHE's attribution is what this
// client's own commit path will use, and the MESSAGE's is what every peer will use. A generator
// that got one right and the other wrong would be a group whose commit says one thing and whose
// wire says another.
func TestEveryProposalThisGroupGeneratesNamesItsOwnLeafAsSender(t *testing.T) {
	crypto, group, rows := testProposalGenerationFixture(t)
	own := group.OwnLeafIndex()
	for _, row := range rows {
		message, err := row.propose()
		if err != nil {
			t.Fatalf("%s: %v", row.name, err)
		}
		cached := group.pendingProposalsForTest()
		one := cached[len(cached)-1]
		if one.Sender != own {
			t.Errorf("%s: the cache attributed this proposal to leaf %d and this client is leaf %d",
				row.name, one.Sender, own)
		}
		opened := testOpenOwnProposal(t, crypto, group, message)
		if opened.Content.Sender.SenderType != SenderTypeMember {
			t.Errorf("%s: sender type = %d, want member", row.name, opened.Content.Sender.SenderType)
		}
		if opened.Content.Sender.LeafIndex != own {
			t.Errorf("%s: the message names leaf %d as its sender and this client is leaf %d",
				row.name, opened.Content.Sender.LeafIndex, own)
		}
		if !bytes.Equal(opened.Content.GroupId, group.GroupId()) {
			t.Errorf("%s: the message names group %x and this group is %x",
				row.name, opened.Content.GroupId, group.GroupId())
		}
		if opened.Content.Epoch != group.Epoch() {
			t.Errorf("%s: the message names epoch %d and this group is at %d",
				row.name, opened.Content.Epoch, group.Epoch())
		}
		if opened.Content.ContentType != ContentTypeProposal {
			t.Errorf("%s: content type = %d, want proposal", row.name, opened.Content.ContentType)
		}
		if opened.Content.Proposal == nil {
			t.Fatalf("%s: the message carries no proposal arm", row.name)
		}
		if opened.Content.Proposal.ProposalType != row.kind {
			t.Errorf("%s: the message carries a %s", row.name,
				proposalTypeName(opened.Content.Proposal.ProposalType))
		}
	}
}

// TestProposingLeavesTheEpochAndTheOwnLeafAlone is the "a proposal is a request and not a change"
// half.
//
// The own leaf is the reading that can fail on its own. An implementation that applied its own
// Update as it proposed it would hold a leaf no peer has seen, publish a tree hash nothing agrees
// with from the next commit onward, and answer every structural question correctly in between --
// and the epoch, which is the obvious thing to check, would still be right.
func TestProposingLeavesTheEpochAndTheOwnLeafAlone(t *testing.T) {
	_, group, rows := testProposalGenerationFixture(t)
	epoch := group.Epoch()
	before := group.OwnLeafNodeCopy()
	if before == nil {
		t.Fatal("OwnLeafNodeCopy returned nil, so this test compares nothing")
	}
	members := len(group.Members())
	for _, row := range rows {
		if _, err := row.propose(); err != nil {
			t.Fatalf("%s: %v", row.name, err)
		}
		if group.Epoch() != epoch {
			t.Fatalf("%s: the epoch went from %d to %d; proposing is not an epoch change",
				row.name, epoch, group.Epoch())
		}
		after := group.OwnLeafNodeCopy()
		if after == nil {
			t.Fatalf("%s: the group holds no leaf at its own index afterwards", row.name)
		}
		if !bytes.Equal(after.EncryptionKey, before.EncryptionKey) {
			t.Fatalf("%s: this client's own leaf now publishes %x and published %x before; a proposal is not applied by the client that made it",
				row.name, after.EncryptionKey, before.EncryptionKey)
		}
		if got := len(group.Members()); got != members {
			t.Fatalf("%s: the group went from %d members to %d", row.name, members, got)
		}
	}
}

// TestNewGroupFoundsAContextItsOwnProposalDoorsAccept is task 12's own claim turned on task 11,
// which is where the same class turned out to be standing.
//
// The claim these two tests share is that a list this build EMITS is one its own readers accept.
// For a proposal that is ValidateProposalList; for a group context it is the same door, because
// every rule of section 12.2 that reads an extension reads the GROUP'S context through it. The
// creation path assembles that context out of two sources -- the caller's extensions and a
// required_capabilities entry it encodes and prepends itself -- and nothing joined them: a config
// that set RequiredCaps AND supplied its own encoded entry founded a group carrying the type twice,
// which FindExtension refuses for every reader afterwards. The group was created, signed and
// persisted, and then every proposal list anyone built against it was refused with a message about
// an extensions vector.
//
// It is written over the DOOR and not over a count of entries, for the reason the proposal rows
// are: a count is this test's own opinion about what a repeat looks like, and the door is what the
// rest of the package will actually do with the context.
func TestNewGroupFoundsAContextItsOwnProposalDoorsAccept(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	group := testNewGroup(t, crypto, owner, "group-1")
	defer group.Close()
	in := &ProposalValidationInput{
		Crypto:  crypto,
		Tree:    group.tree,
		Context: testGroupContextOf(t, group),
		List:    NewProposalList([]CachedProposal{}),
		Now:     time.Now(),
	}
	if err := ValidateProposalList(in); err != nil {
		t.Fatalf("this group's own published context is one its own proposal door refuses: %v", err)
	}
}

// TestNewGroupRefusesAConfigThatWouldCarryRequiredCapabilitiesTwice is the refusal half, over the
// one input that reaches it: the entry the creation path adds is the only one no earlier check
// joined against the caller's own list.
func TestNewGroupRefusesAConfigThatWouldCarryRequiredCapabilitiesTwice(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	cfg := testGroupConfig(t, crypto, owner, "group-1")
	if len(cfg.RequiredCaps.ExtensionTypes) == 0 {
		t.Fatal("this fixture no longer sets RequiredCaps, so nothing is prepended and this test drives nothing")
	}
	supplied, err := encodeRequiredCapabilities(&RequiredCapabilities{
		ExtensionTypes: []ExtensionType{ExtensionTypeUrmessageGroupPolicy},
	})
	if err != nil {
		t.Fatalf("encode the caller's own required_capabilities: %v", err)
	}
	cfg.Extensions = append(cfg.Extensions, supplied)
	group, err := NewGroup(cfg, owner.SigPriv, BasicCredential(owner.IdentityPub))
	if err == nil {
		group.Close()
		t.Fatal("a config that both sets RequiredCaps and supplies its own required_capabilities founded a group; its context carries the type twice and every later reader refuses it")
	}
	if !errors.Is(err, ErrMalformedExtension) {
		t.Fatalf("NewGroup error = %v, want ErrMalformedExtension; the refusal is the lookup's", err)
	}
}

// TestProposeAddProducesACacheableProposal is the plan's own row, kept because it is the one that
// reads the wire format.
func TestProposeAddProducesACacheableProposal(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	group := testNewGroup(t, crypto, owner, "group-1")
	defer group.Close()

	bob := testIdentity(t, crypto, "bob")
	kp, _, _ := testKeyPackage(t, crypto, bob)
	encoded, err := syntax.Marshal(kp)
	if err != nil {
		t.Fatalf("marshal key package: %v", err)
	}
	message, err := group.ProposeAdd(encoded)
	if err != nil {
		t.Fatalf("ProposeAdd: %v", err)
	}
	if len(message) == 0 {
		t.Fatal("ProposeAdd returned no message")
	}
	parsed, err := ParseMLSMessage(message)
	if err != nil {
		t.Fatalf("ParseMLSMessage: %v", err)
	}
	if parsed.WireFormat != WireFormatPrivateMessage {
		t.Fatalf("wire format = %#x, want PrivateMessage: A-ASSUME-4 puts handshake traffic in PrivateMessage",
			parsed.WireFormat)
	}
	if group.Epoch() != 0 {
		t.Fatal("proposing must not advance the epoch")
	}
	cached := group.pendingProposalsForTest()
	if len(cached) != 1 {
		t.Fatalf("the proposal was not cached: %d entries", len(cached))
	}
	if cached[0].Proposal.Add == nil {
		t.Fatal("the cached entry carries no add arm")
	}
	// the KEY PACKAGE the caller handed over and not some other one, read off the cache's own
	// copy: the entry is what a commit will carry, and an add that cached a different package
	// admits a different client
	if !bytes.Equal(cached[0].Proposal.Add.KeyPackage.InitKey, kp.InitKey) {
		t.Fatalf("the cached add publishes init key %x and the key package handed over publishes %x",
			cached[0].Proposal.Add.KeyPackage.InitKey, kp.InitKey)
	}
}

// TestProposeAddRefusesAKeyPackageItsOwnValidatorWouldRefuse is the send side of section 10.1.
//
// The suite is flipped after the package was signed, which is the cheapest reachable refusal of
// (*KeyPackage).Validate that is not the signature itself: version, then ciphersuite, then the
// signature, so this row observes the SUITE clause specifically.
func TestProposeAddRefusesAKeyPackageItsOwnValidatorWouldRefuse(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	group := testNewGroup(t, crypto, owner, "group-1")
	defer group.Close()

	bob := testIdentity(t, crypto, "bob")
	kp, _, _ := testKeyPackage(t, crypto, bob)
	kp.CipherSuite = CipherSuiteX25519AesGcm128Sha256Ed25519
	encoded, err := syntax.Marshal(kp)
	if err != nil {
		t.Fatalf("marshal key package: %v", err)
	}
	if _, err := group.ProposeAdd(encoded); !errors.Is(err, errProfileCiphersuite) {
		t.Fatalf("ProposeAdd error = %v, want the ciphersuite refusal (*KeyPackage).Validate makes", err)
	}
	if cached := group.pendingProposalsForTest(); len(cached) != 0 {
		t.Fatalf("a refused add cached %d entries", len(cached))
	}
}

// TestProposeAddRefusesALeafWithNoWrapTarget is the v1 half of the same door, and it is not in
// section 10.1 at all: every commit this profile makes wraps the epoch secret to
// urmessage_leaf_keys, so a joiner whose leaf carries none is a member no epoch can reach.
func TestProposeAddRefusesALeafWithNoWrapTarget(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	group := testNewGroup(t, crypto, owner, "group-1")
	defer group.Close()

	bob := testIdentity(t, crypto, "bob")
	kp, _, _, err := NewKeyPackage(crypto, crypto.Suite(), BasicCredential(bob.IdentityPub),
		testCapabilities(), nil)
	if err != nil {
		t.Fatalf("NewKeyPackage with no extensions: %v", err)
	}
	encoded, err := syntax.Marshal(kp)
	if err != nil {
		t.Fatalf("marshal key package: %v", err)
	}
	if _, err := group.ProposeAdd(encoded); !errors.Is(err, ErrMalformedExtension) {
		t.Fatalf("ProposeAdd error = %v, want the leaf keys refusal", err)
	}
}

// TestProposeUpdateUsesAFreshEncryptionKey is the plan's own row: the property an Update exists
// for, read off the cache's copy of what was published.
func TestProposeUpdateUsesAFreshEncryptionKey(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	group := testNewGroup(t, crypto, owner, "group-1")
	defer group.Close()

	before := group.OwnLeafNodeCopy()
	if before == nil {
		t.Fatal("OwnLeafNodeCopy returned nil")
	}
	if _, err := group.ProposeUpdate(); err != nil {
		t.Fatalf("ProposeUpdate: %v", err)
	}
	cached := group.pendingProposalsForTest()
	if len(cached) != 1 {
		t.Fatalf("cached = %d, want 1", len(cached))
	}
	if cached[0].Proposal.Update == nil {
		t.Fatal("the cached entry carries no update arm")
	}
	updated := cached[0].Proposal.Update.LeafNode
	if bytes.Equal(updated.EncryptionKey, before.EncryptionKey) {
		t.Fatal("an update that reuses the leaf's encryption key provides no post compromise security")
	}
	if updated.LeafNodeSource != LeafNodeSourceUpdate {
		t.Fatalf("leaf_node_source = %d, want update", updated.LeafNodeSource)
	}
	// the SIGNATURE is over the update variant's own preimage, which carries the group id and the
	// leaf index a key_package leaf's does not. A leaf signed with nil and 0 verifies against
	// itself and against no receiver, and nothing about the value says which it was.
	if err := updated.VerifySignature(crypto, group.GroupId(), group.OwnLeafIndex()); err != nil {
		t.Fatalf("the update leaf does not verify in this group at this leaf: %v", err)
	}
	// the private half is where a committer will look for it
	priv, err := group.store.GetPrivateKey(updated.EncryptionKey)
	if err != nil {
		t.Fatalf("the update's private key was not filed under the key it publishes: %v", err)
	}
	if len(priv) == 0 {
		t.Fatal("the store holds an empty private key for the update's encryption key")
	}
}

// TestProposeUpdateDrawsItsEncryptionKeyFreshAndFromItsOwnDraw is the entropy gate, written against
// what the proposal PUBLISHES rather than against the line that draws.
//
// Three defects it is built to see, each of which leaves the rest of this package green:
//
//  1. the draw replaced by a CONSTANT, or by any value this call did not draw -- the group id, the
//     founding leaf's own ikm, a zero buffer. The key the proposal published is then not derivable
//     from anything the provider was asked for, so the search below finds nothing.
//  2. the draw REUSED from the group's founding, which is the substitution that makes an Update
//     publish a key an attacker who compromised the founding already has. The founding draws are
//     recorded before the call and are excluded explicitly.
//  3. a draw made and THROWN AWAY. Every KDF.Nh draw this call made has to be the one the proposal
//     published, so a second one is a failure here until somebody says what it is for.
//
// The four octet reuse guard SealPrivateMessage draws is not a KDF.Nh draw and is not part of that
// accounting; it is the message's, not the leaf key's.
func TestProposeUpdateDrawsItsEncryptionKeyFreshAndFromItsOwnDraw(t *testing.T) {
	base := testCrypto(t)
	owner := testIdentity(t, base, "owner")
	witness := &entropyWitness{CryptoProvider: base}
	cfg := testGroupConfig(t, witness, owner, "group-1")
	group, err := NewGroup(cfg, owner.SigPriv, BasicCredential(owner.IdentityPub))
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	defer group.Close()

	founding := len(witness.draws)
	foundingIkms := len(witness.ikms)
	if founding == 0 || foundingIkms == 0 {
		t.Fatal("group creation recorded no draws, so the exclusion below excludes nothing")
	}
	if _, err := group.ProposeUpdate(); err != nil {
		t.Fatalf("ProposeUpdate: %v", err)
	}
	drawn := witness.draws[founding:]
	derived := witness.ikms[foundingIkms:]
	if len(derived) != 1 {
		t.Fatalf("ProposeUpdate derived %d key pairs, want the one the replacement leaf is keyed by",
			len(derived))
	}
	at := drawIndexOf(drawn, derived[0])
	if at < 0 {
		t.Fatal("the update leaf's key pair was derived from something this call never drew")
	}
	if drawIndexOf(witness.draws[:founding], derived[0]) >= 0 {
		t.Fatal("the update leaf's key pair was derived from a value this group was FOUNDED on; an update that republishes founding key material provides no post compromise security")
	}
	_, encPub, err := base.DeriveKeyPair(drawn[at])
	if err != nil {
		t.Fatalf("re-derive the update key pair: %v", err)
	}
	cached := group.pendingProposalsForTest()
	if len(cached) != 1 || cached[0].Proposal.Update == nil {
		t.Fatalf("the update was not cached: %d entries", len(cached))
	}
	if published := cached[0].Proposal.Update.LeafNode.EncryptionKey; !bytes.Equal(published, encPub) {
		t.Fatalf("the update publishes %x and draw %d derives %x", published, at, encPub)
	}
	// and nothing of the leaf key's width was drawn that the proposal did not publish
	for i, one := range drawn {
		if i != at && len(one) == base.HashSize() {
			t.Errorf("ProposeUpdate drew a second value of KDF.Nh at %d and published neither it nor anything derived from it; a draw nothing publishes is entropy this gate cannot follow",
				i)
		}
	}
}

// TestTwoGroupsProposeUpdatesUnderDifferentKeys is the divergence half of the entropy claim, and it
// sees one thing the gate above cannot: a draw that IS recorded and IS used and is nevertheless the
// same value every time -- a provider stub, a seeded source, a package level buffer.
func TestTwoGroupsProposeUpdatesUnderDifferentKeys(t *testing.T) {
	crypto := testCrypto(t)
	keys := [][]byte{}
	for _, name := range []string{"owner-a", "owner-b"} {
		owner := testIdentity(t, crypto, name)
		group := testNewGroup(t, crypto, owner, "group-1")
		if _, err := group.ProposeUpdate(); err != nil {
			t.Fatalf("%s: ProposeUpdate: %v", name, err)
		}
		cached := group.pendingProposalsForTest()
		if len(cached) != 1 || cached[0].Proposal.Update == nil {
			t.Fatalf("%s: the update was not cached", name)
		}
		keys = append(keys, bytes.Clone(cached[0].Proposal.Update.LeafNode.EncryptionKey))
		group.Close()
	}
	if bytes.Equal(keys[0], keys[1]) {
		t.Fatalf("two groups proposed updates publishing the same encryption key %x", keys[0])
	}
}

// TestProposeUpdateIsRefusedTwiceInOneEpoch is the section 12.2 ceiling read from the sending side.
//
// One sender contributes at most ONE committable update however many it publishes, because an
// update applies to its own sender's leaf and section 12.2 refuses a list carrying two proposals
// that apply to one leaf. A generator that cached a second would be filling this epoch's cache with
// entries its own commit path cannot carry.
func TestProposeUpdateIsRefusedTwiceInOneEpoch(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	group := testNewGroup(t, crypto, owner, "group-1")
	defer group.Close()

	if _, err := group.ProposeUpdate(); err != nil {
		t.Fatalf("the first ProposeUpdate: %v", err)
	}
	if _, err := group.ProposeUpdate(); err == nil {
		t.Fatal("a second update in one epoch was accepted; section 12.2 admits one update per sender per commit")
	}
	if cached := group.pendingProposalsForTest(); len(cached) != 1 {
		t.Fatalf("the cache holds %d updates, want 1", len(cached))
	}
}

// TestProposeRemoveRefusesOwnLeaf is the plan's own row.
//
// The refusal is about this client's CACHE and not about RFC 9420: section 12.1.3's leave flow is a
// member sending a Remove for its own leaf. What this build has no room for is CACHING one, because
// task 13 commits what the cache holds and validateCommitterIsNotRemoved refuses that commit at
// every receiver.
func TestProposeRemoveRefusesOwnLeaf(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	group := testNewGroup(t, crypto, owner, "group-1")
	defer group.Close()
	if _, err := group.ProposeRemove(group.OwnLeafIndex()); !errors.Is(err, ErrRemoveCommitter) {
		t.Fatalf("ProposeRemove error = %v, want the committer-removal refusal", err)
	}
	if cached := group.pendingProposalsForTest(); len(cached) != 0 {
		t.Fatalf("a refused self remove cached %d entries", len(cached))
	}
}

// TestProposeRemoveRefusesALeafTheTreeDoesNotHold is ValSem108 asked at generation time: a caller
// that named a member who has already been removed, or a leaf beyond the tree, learns it from the
// call it made rather than from a commit nobody accepts.
func TestProposeRemoveRefusesALeafTheTreeDoesNotHold(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	group := testNewGroup(t, crypto, owner, "group-1")
	defer group.Close()
	if _, err := group.ProposeRemove(LeafIndex(7)); !errors.Is(err, ErrRemoveNonMember) {
		t.Fatalf("ProposeRemove error = %v, want ErrRemoveNonMember", err)
	}
}

// TestProposeGroupContextExtensionsRefusesProfileViolation is the plan's own row, against the
// package's real sentinel: errProfileExternalSender is the stand-in p8's Profile will export, and
// validate_commit.go declares it once for both the sending and the receiving side of ValSem209.
func TestProposeGroupContextExtensionsRefusesProfileViolation(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	group := testNewGroup(t, crypto, owner, "group-1")
	defer group.Close()
	bad := []Extension{{ExtensionType: ExtensionTypeExternalSenders, ExtensionData: []byte{}}}
	if _, err := group.ProposeGroupContextExtensions(bad); !errors.Is(err, errProfileExternalSender) {
		t.Fatalf("ProposeGroupContextExtensions error = %v, want errProfileExternalSender", err)
	}
	if cached := group.pendingProposalsForTest(); len(cached) != 0 {
		t.Fatalf("a refused extension set cached %d entries", len(cached))
	}
}

// TestProposeGroupContextExtensionsRefusesAnExtensionSetWithNoPolicy is the second door, and it is
// this profile's rather than the RFC's. A group here always carries a urmessage_group_policy, so a
// wholesale replacement that drops it is a group with no owner and no roles -- which nothing
// downstream reports until the first message that needed one.
func TestProposeGroupContextExtensionsRefusesAnExtensionSetWithNoPolicy(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	group := testNewGroup(t, crypto, owner, "group-1")
	defer group.Close()
	if _, err := group.ProposeGroupContextExtensions(nil); !errors.Is(err, ErrNoGroupPolicy) {
		t.Fatalf("ProposeGroupContextExtensions error = %v, want ErrNoGroupPolicy", err)
	}
}

// TestTheProposalThisGroupCachesSharesNoStorageWithItsCaller is the retention half, and its NAME is
// the correction rather than a detail.
//
// It was written as "ProposeGroupContextExtensions copies the extension bodies it was handed", and
// under that name it could not fail: the generator copied, and so did the cache, one frame further
// down and through the codec -- so deleting the generator's copy left this green, because what the
// assertions read is the CACHE's entry. Two copies, and the test observed the one it was not about.
//
// The property that is real, and the one that matters, is stated over the value that OUTLIVES the
// call. The cached entry is what a later commit carries and what this client will still be holding
// an epoch from now; a caller's array reaching into it is a proposal that changes after the group
// signed it, with the signature going on verifying over the octets as they were and nothing at the
// point of the write to say so. Where the copy is made is the cache's business -- it is the thing
// doing the retaining -- and this reads the property rather than the line.
func TestTheProposalThisGroupCachesSharesNoStorageWithItsCaller(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	group := testNewGroup(t, crypto, owner, "group-1")
	defer group.Close()

	handed := testGroupContextOf(t, group).Extensions
	if len(handed) == 0 {
		t.Fatal("the group publishes no extensions, so this test scribbles nothing")
	}
	if _, err := group.ProposeGroupContextExtensions(handed); err != nil {
		t.Fatalf("ProposeGroupContextExtensions: %v", err)
	}
	cached := group.pendingProposalsForTest()
	if len(cached) != 1 || cached[0].Proposal.GroupContextExtensions == nil {
		t.Fatalf("the proposal was not cached: %d entries", len(cached))
	}
	kept := [][]byte{}
	for _, ext := range cached[0].Proposal.GroupContextExtensions.Extensions {
		kept = append(kept, bytes.Clone(ext.ExtensionData))
	}
	scribbled := 0
	for _, ext := range handed {
		for i := range ext.ExtensionData {
			ext.ExtensionData[i] ^= 0xff
			scribbled += 1
		}
	}
	if scribbled == 0 {
		t.Fatal("every extension handed over had an empty body, so nothing was scribbled")
	}
	for at, ext := range cached[0].Proposal.GroupContextExtensions.Extensions {
		if !bytes.Equal(ext.ExtensionData, kept[at]) {
			t.Errorf("extension %d of the cached proposal held %x before its caller wrote into its own array and %x afterwards",
				at, kept[at], ext.ExtensionData)
		}
	}
}

// TestAClosedGroupProposesNothing is the ender's half. A closed group has zeroized its schedule and
// its secret tree, so a proposal path that reached either would be sealing under erased keys or
// dereferencing nil -- and the answer a library gives there must be a refusal, not a panic.
//
// EVERY ARGUMENT BELOW IS ONE AN OPEN GROUP WOULD ALSO REFUSE, and that is what makes each row
// observe the generator's OWN closed check rather than the one in propose. With a good argument a
// closed group answers errGroupClosed either way -- propose refuses last -- so the rows would pass
// against a generator that had stopped asking, and the two checks would be guards covering for each
// other, which is the shape proposal_list.go's own header rejects. With a bad one they separate: a
// generator that judged its argument first answers the argument's refusal, and a caller repairing
// what it was told about goes on getting the same answer from a group that was never going to act.
//
// The store is read afterwards because ProposeUpdate is the one generator that WRITES before it
// publishes: the private half is filed before the proposal is framed, so a closed group that got as
// far as the draw leaves a key in its caller's store for an epoch it can never reach.
func TestAClosedGroupProposesNothing(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	group := testNewGroup(t, crypto, owner, "group-1")
	store, isTestStore := group.store.(*testStore)
	if !isTestStore {
		t.Fatalf("this group's store is a %T and this test reads the fixture's own", group.store)
	}
	filedBefore := len(store.privs)
	if err := group.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	rows := map[string]func() ([]byte, error){
		// octets no decoder accepts
		"ProposeAdd": func() ([]byte, error) { return group.ProposeAdd(nil) },
		// the one generator with no argument to get wrong; what its own check buys is below
		"ProposeUpdate": group.ProposeUpdate,
		// a leaf this group never held
		"ProposeRemove": func() ([]byte, error) { return group.ProposeRemove(LeafIndex(7)) },
		// an extension the v1 profile refuses in a group context
		"ProposeGroupContextExtensions": func() ([]byte, error) {
			return group.ProposeGroupContextExtensions(
				[]Extension{{ExtensionType: ExtensionTypeExternalSenders, ExtensionData: []byte{}}})
		},
	}
	for name, call := range rows {
		if _, err := call(); !errors.Is(err, errGroupClosed) {
			t.Errorf("%s on a closed group answered %v, want the closed refusal", name, err)
		}
	}
	if filed := len(store.privs); filed != filedBefore {
		t.Errorf("a closed group's refused update left %d private keys in its caller's store and there were %d before",
			filed, filedBefore)
	}
}

// ---------------------------------------------------------------------------
// p7 task 19: the epoch advance -- persistence and the past-epoch window
// ---------------------------------------------------------------------------

// windowStore is a StateStore that keeps the ACTUAL slices it was handed rather than copies of
// them, and records which group each window delete was run for.
//
// The slices and not clones of them, for stagedEpochStorage's reason one file over: zeroizeSecret
// writes through the backing array, so a double holding a copy would read its copy afterwards and
// report a clean erase over a persist that never ran one. Whether the state was LIVE inside the
// call is recorded at the same time, because "all zero afterwards" is satisfied by storage that
// was already zero and a fixture that had stopped serializing anything would pass every assertion
// below while observing nothing.
type windowStore struct {
	*testStore
	putIds [][]byte
	// the epoch each write named. It used to be left to the INDEX of the write, which was right
	// only while a group happened to write once per epoch -- and a group now records the
	// generation each seal spent, so it writes at the commit as well as at the merge.
	putEpochs []uint64
	putStates [][]byte
	putLive   []bool
	getIds    [][]byte
	deleteIds [][]byte
	cutoffs   []uint64
}

func newWindowStore() *windowStore {
	return &windowStore{testStore: newTestStore()}
}

func (self *windowStore) PutGroupState(groupId []byte, epoch uint64, state []byte) error {
	self.putIds = append(self.putIds, groupId)
	self.putEpochs = append(self.putEpochs, epoch)
	self.putStates = append(self.putStates, state)
	self.putLive = append(self.putLive,
		slices.ContainsFunc(state, func(b byte) bool { return b != 0 }))
	return self.testStore.PutGroupState(groupId, epoch, state)
}

func (self *windowStore) GetGroupState(groupId []byte, epoch uint64) ([]byte, error) {
	self.getIds = append(self.getIds, groupId)
	return self.testStore.GetGroupState(groupId, epoch)
}

func (self *windowStore) DeleteGroupStateBefore(groupId []byte, epoch uint64) error {
	self.deleteIds = append(self.deleteIds, groupId)
	self.cutoffs = append(self.cutoffs, epoch)
	return self.testStore.DeleteGroupStateBefore(groupId, epoch)
}

// advanceEpochs commits and merges n times, which is the only way this build moves a group's
// epoch. It answers nothing: what every caller below reads is the store.
func advanceEpochs(t *testing.T, group *Group, n int) {
	t.Helper()
	for i := 0; i < n; i += 1 {
		if _, err := group.CreateCommit(nil, nil, nil); err != nil {
			t.Fatalf("CreateCommit %d: %v", i, err)
		}
		if err := group.MergePendingCommit(); err != nil {
			t.Fatalf("MergePendingCommit %d: %v", i, err)
		}
	}
}

func TestMergePendingCommitPersistsAndPrunes(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	cfg := testGroupConfig(t, crypto, owner, "group-1")
	store := cfg.Store.(*testStore)
	group, err := NewGroup(cfg, owner.SigPriv, BasicCredential(owner.IdentityPub))
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	defer group.Close()

	advanceEpochs(t, group, 3)
	if group.Epoch() != 3 {
		t.Fatalf("Epoch = %d, want 3", group.Epoch())
	}
	if _, err := store.GetGroupState([]byte("group-1"), 3); err != nil {
		t.Fatalf("epoch 3 state was not persisted: %v", err)
	}
	// and every epoch on the way, because a merge that persisted only the newest state would
	// leave the window a window over one epoch
	for epoch := uint64(0); epoch <= 3; epoch += 1 {
		if _, err := store.GetGroupState([]byte("group-1"), epoch); err != nil {
			t.Fatalf("epoch %d state was not persisted: %v", epoch, err)
		}
	}
	if len(store.deletes) != 3 {
		t.Fatalf("DeleteGroupStateBefore was called %d times, want once per merged commit", len(store.deletes))
	}
	// epoch 3 - PastEpochWindow would underflow, so the cutoff floors at nought and nothing is
	// deleted while the group is younger than the window
	for at, cutoff := range store.deletes {
		if cutoff != 0 {
			t.Fatalf("delete %d ran at cutoff %d, want 0 while the group is younger than the window",
				at, cutoff)
		}
	}
}

// TestPastEpochWindowDropsOlderState reads the bound in BOTH directions, which is what a cutoff
// assertion alone cannot do: a window one epoch too wide and one epoch too narrow both produce a
// group whose oldest epoch is gone, and only the state that must SURVIVE separates them.
func TestPastEpochWindowDropsOlderState(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	cfg := testGroupConfig(t, crypto, owner, "group-1")
	store := cfg.Store.(*testStore)
	group, err := NewGroup(cfg, owner.SigPriv, BasicCredential(owner.IdentityPub))
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	defer group.Close()

	advanceEpochs(t, group, int(PastEpochWindow)+2)
	if group.Epoch() != PastEpochWindow+2 {
		t.Fatalf("Epoch = %d, want %d", group.Epoch(), PastEpochWindow+2)
	}
	cutoff := store.deletes[len(store.deletes)-1]
	if cutoff != group.Epoch()-PastEpochWindow {
		t.Fatalf("delete cutoff = %d, want %d", cutoff, group.Epoch()-PastEpochWindow)
	}
	// the epoch one BELOW the window is gone, and with it the eph_root it carried
	dropped := group.Epoch() - PastEpochWindow - 1
	if _, err := store.GetGroupState([]byte("group-1"), dropped); err == nil {
		t.Fatalf("epoch %d state survived the past-epoch window; eph_root would survive with it", dropped)
	}
	// and the OLDEST epoch inside the window is still there, which is the half a window one
	// epoch too narrow fails. Without it, dropping thirty-three epochs instead of thirty-two
	// reads exactly like this test passing.
	oldest := group.Epoch() - PastEpochWindow
	if _, err := store.GetGroupState([]byte("group-1"), oldest); err != nil {
		t.Fatalf("epoch %d is the oldest epoch inside a %d epoch window and its state is gone: %v",
			oldest, PastEpochWindow, err)
	}
	// and so is the epoch the group is standing in
	if _, err := store.GetGroupState([]byte("group-1"), group.Epoch()); err != nil {
		t.Fatalf("the live epoch's state is gone: %v", err)
	}
}

// TestThePastEpochWindowEvictsOnlyTheGroupItRanFor is the half a single-group fixture cannot see.
// Every group this client is in runs an epoch 7, so an epoch number is not an identity: a window
// keyed on the number alone evicts a stranger's epochs, and a client that lost those is a client
// that cannot open a late message in a group it never committed to.
func TestThePastEpochWindowEvictsOnlyTheGroupItRanFor(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	neighbour := testIdentity(t, crypto, "neighbour")
	store := newWindowStore()

	first := testGroupConfig(t, crypto, owner, "group-1")
	first.Store = store
	second := testGroupConfig(t, crypto, neighbour, "group-2")
	second.Store = store

	elder, err := NewGroup(first, owner.SigPriv, BasicCredential(owner.IdentityPub))
	if err != nil {
		t.Fatalf("NewGroup group-1: %v", err)
	}
	defer elder.Close()
	younger, err := NewGroup(second, neighbour.SigPriv, BasicCredential(neighbour.IdentityPub))
	if err != nil {
		t.Fatalf("NewGroup group-2: %v", err)
	}
	defer younger.Close()

	advanceEpochs(t, elder, int(PastEpochWindow)+2)
	if len(store.deleteIds) == 0 {
		t.Fatal("no window delete ran at all, so this test compared nothing")
	}
	for at, id := range store.deleteIds {
		if !bytes.Equal(id, []byte("group-1")) {
			t.Fatalf("window delete %d was run for group %q, and the group that advanced is %q",
				at, id, "group-1")
		}
	}
	// the neighbour is at epoch 0 and the elder has evicted everything below epoch 2. A window
	// keyed by the epoch alone takes the neighbour's only state with it.
	if _, err := store.GetGroupState([]byte("group-2"), 0); err != nil {
		t.Fatalf("group-2's epoch 0 state was evicted by a window run for group-1: %v", err)
	}
	// and the control, so the assertion above is not passing over a delete that dropped nothing
	if _, err := store.GetGroupState([]byte("group-1"), 0); err == nil {
		t.Fatal("group-1's epoch 0 state survived its own window, so the delete above dropped nothing")
	}
}

// TestPersistErasesTheEncodedStateItHandedTheStore observes the one copy of key material this
// task adds to the heap.
//
// A serialized epoch carries the secret its key schedule was built from and this member's leaf
// private key, in the clear, in a buffer syntax.Writer allocated -- so it is a second copy of the
// epoch, written once per merged commit, and after the store call nothing in the process can
// reach it. Held by nothing, it would sit in the heap for the collector to move around for as
// long as the process runs.
func TestPersistErasesTheEncodedStateItHandedTheStore(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	cfg := testGroupConfig(t, crypto, owner, "group-1")
	store := newWindowStore()
	cfg.Store = store
	group, err := NewGroup(cfg, owner.SigPriv, BasicCredential(owner.IdentityPub))
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	defer group.Close()
	advanceEpochs(t, group, 2)

	// FIVE and not three, and the number moved because the group now writes MORE often rather than
	// because this reading was loosened: a seal spends a generation of this leaf's ratchet, and
	// (*Group).sealAndRecordLocked records what it spent before the ciphertext leaves. So each of
	// the two epochs costs a persist at the commit it sealed as well as one at the merge, beside
	// the founding write. The assertion below is unchanged and now runs over five states instead
	// of three.
	if len(store.putStates) != 5 {
		t.Fatalf("the store was handed %d states and this group founded, then sealed and merged two commits",
			len(store.putStates))
	}
	for at, state := range store.putStates {
		// the live control: every one of them was non-zero INSIDE the call, so an all-zero
		// reading afterwards is the erase and not a fixture that stopped serializing
		if !store.putLive[at] {
			t.Fatalf("the state handed to the store at %d was already all zero inside the call, so the reading below says nothing",
				at)
		}
		if len(state) == 0 {
			t.Fatalf("the state handed to the store at %d is empty", at)
		}
		for i, b := range state {
			if b != 0 {
				t.Fatalf("byte %d of the state handed to the store at %d is %#02x after the call, want 0; it is this epoch's parent secret and this member's leaf private key",
					i, at, b)
			}
		}
		// and the store's own copy is intact, which is what says the erase reached the group's
		// buffer rather than the persisted state
		if _, err := store.GetGroupState([]byte("group-1"), store.putEpochs[at]); err != nil {
			t.Fatalf("the epoch %d state written at %d is not in the store after the erase: %v",
				store.putEpochs[at], at, err)
		}
	}
}

// TestNoOctetTheWindowHandsTheStoreIsTheGroupsOwn is persist's caller-array discipline asked of
// the second call this epoch boundary makes.
//
// The sdk writes these StateStore implementations. One that keeps the slice it was handed -- to
// key a map, to build a path with later -- shares an array with the group for the group's
// lifetime, and a write through it rewrites the group id every epoch secret of this group was
// derived over, with the context, the tree hash and the transcript all going on agreeing with
// each other over the wrong id.
func TestNoOctetTheWindowHandsTheStoreIsTheGroupsOwn(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	cfg := testGroupConfig(t, crypto, owner, "group-1")
	store := newWindowStore()
	cfg.Store = store
	group, err := NewGroup(cfg, owner.SigPriv, BasicCredential(owner.IdentityPub))
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	defer group.Close()
	advanceEpochs(t, group, 1)

	handed := slices.Concat(store.putIds, store.deleteIds)
	if len(handed) < 3 {
		t.Fatalf("the store was handed %d group ids and this group founded, persisted and pruned; a reading this thin compares nothing",
			len(handed))
	}
	before := group.EpochAuthenticator()
	for at, id := range handed {
		if len(id) == 0 {
			t.Fatalf("the group id handed to the store at %d is empty", at)
		}
		// a store that kept the slice and wrote through it, which is the whole hazard
		for i := range id {
			id[i] = 0xff
		}
	}
	if got := group.GroupId(); !bytes.Equal(got, []byte("group-1")) {
		t.Fatalf("the group id is %q after a store wrote through what it was handed, want %q",
			got, "group-1")
	}
	if after := group.EpochAuthenticator(); !bytes.Equal(after, before) {
		t.Fatal("the epoch authenticator moved when a store wrote through the group id it was handed")
	}
}

// TestLoadGroupRestoresAnEpoch is the round trip, over TWO groups rather than one: a state read
// back has to produce a group that derives the same secrets, and a comparison a single group made
// against itself would be satisfied by a restore that answered the group it was called on.
func TestLoadGroupRestoresAnEpoch(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	cfg := testGroupConfig(t, crypto, owner, "group-1")
	group, err := NewGroup(cfg, owner.SigPriv, BasicCredential(owner.IdentityPub))
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	advanceEpochs(t, group, 1)

	epoch := group.Epoch()
	wantAuth := group.EpochAuthenticator()
	wantExport, err := group.Export("urmessage exporter", []byte("context"), 32)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	wantSenderData, err := group.EpochSecret(EpochSecretSenderData)
	if err != nil {
		t.Fatalf("EpochSecret(sender data): %v", err)
	}
	wantEncryption, err := group.EpochSecret(EpochSecretEncryption)
	if err != nil {
		t.Fatalf("EpochSecret(encryption): %v", err)
	}
	wantTree, err := group.RatchetTree()
	if err != nil {
		t.Fatalf("RatchetTree: %v", err)
	}
	group.Close()

	restored, err := LoadGroup(cfg, epoch, owner.SigPriv)
	if err != nil {
		t.Fatalf("LoadGroup: %v", err)
	}
	defer restored.Close()
	if restored.Epoch() != epoch {
		t.Fatalf("restored epoch = %d, want %d", restored.Epoch(), epoch)
	}
	if !bytes.Equal(restored.GroupId(), []byte("group-1")) {
		t.Fatalf("restored group id = %q, want %q", restored.GroupId(), "group-1")
	}
	if !bytes.Equal(restored.EpochAuthenticator(), wantAuth) {
		t.Fatal("restored group derives a different epoch authenticator")
	}
	gotExport, err := restored.Export("urmessage exporter", []byte("context"), 32)
	if err != nil {
		t.Fatalf("restored Export: %v", err)
	}
	if !bytes.Equal(gotExport, wantExport) {
		t.Fatal("restored group derives a different exporter secret")
	}
	gotSenderData, err := restored.EpochSecret(EpochSecretSenderData)
	if err != nil {
		t.Fatalf("restored EpochSecret(sender data): %v", err)
	}
	if !bytes.Equal(gotSenderData, wantSenderData) {
		t.Fatal("restored group derives a different sender data secret")
	}
	gotEncryption, err := restored.EpochSecret(EpochSecretEncryption)
	if err != nil {
		t.Fatalf("restored EpochSecret(encryption): %v", err)
	}
	if !bytes.Equal(gotEncryption, wantEncryption) {
		t.Fatal("restored group derives a different encryption secret")
	}
	gotTree, err := restored.RatchetTree()
	if err != nil {
		t.Fatalf("restored RatchetTree: %v", err)
	}
	if !bytes.Equal(gotTree, wantTree) {
		t.Fatal("restored group holds a different ratchet tree")
	}
	// and the control on all six comparisons: a DIFFERENT epoch of the same group derives
	// different secrets, so an assertion that passed by reading the same value twice would fail
	// here
	older, err := LoadGroup(cfg, epoch-1, owner.SigPriv)
	if err != nil {
		t.Fatalf("LoadGroup at epoch %d: %v", epoch-1, err)
	}
	defer older.Close()
	if bytes.Equal(older.EpochAuthenticator(), wantAuth) {
		t.Fatal("two epochs of one group derive the same epoch authenticator, so the comparisons above compare nothing")
	}
}

// TestLoadGroupRestoresTheGroupItsConfigNames is the identity half. A store holds every group this
// client is in and every group runs an epoch 1, so a restore keyed on the epoch alone answers
// whichever group was written last -- with a nil error and a perfectly well formed group.
func TestLoadGroupRestoresTheGroupItsConfigNames(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	neighbour := testIdentity(t, crypto, "neighbour")
	store := newTestStore()

	first := testGroupConfig(t, crypto, owner, "group-1")
	first.Store = store
	second := testGroupConfig(t, crypto, neighbour, "group-2")
	second.Store = store

	elder, err := NewGroup(first, owner.SigPriv, BasicCredential(owner.IdentityPub))
	if err != nil {
		t.Fatalf("NewGroup group-1: %v", err)
	}
	advanceEpochs(t, elder, 1)
	elderAuth := elder.EpochAuthenticator()
	elder.Close()

	younger, err := NewGroup(second, neighbour.SigPriv, BasicCredential(neighbour.IdentityPub))
	if err != nil {
		t.Fatalf("NewGroup group-2: %v", err)
	}
	advanceEpochs(t, younger, 1)
	youngerAuth := younger.EpochAuthenticator()
	younger.Close()

	if bytes.Equal(elderAuth, youngerAuth) {
		t.Fatal("two groups derived the same epoch authenticator, so the comparisons below say nothing")
	}
	restored, err := LoadGroup(first, 1, owner.SigPriv)
	if err != nil {
		t.Fatalf("LoadGroup group-1: %v", err)
	}
	defer restored.Close()
	if !bytes.Equal(restored.GroupId(), []byte("group-1")) {
		t.Fatalf("LoadGroup over group-1's config restored %q", restored.GroupId())
	}
	if !bytes.Equal(restored.EpochAuthenticator(), elderAuth) {
		t.Fatal("LoadGroup over group-1's config restored an epoch of another group")
	}
}

// TestARestoredGroupGoesOnPersistingItsOwnEpochs is the property a one-shot restore cannot see:
// the blob stores an INPUT to the key schedule, so a restored group has to be able to answer that
// input again for the epoch it moves to next. A restore that rebuilt a schedule it could not
// re-persist would produce a group that works until its first commit.
func TestARestoredGroupGoesOnPersistingItsOwnEpochs(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	cfg := testGroupConfig(t, crypto, owner, "group-1")
	group, err := NewGroup(cfg, owner.SigPriv, BasicCredential(owner.IdentityPub))
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	// epoch 0 is restored from the sampled epoch secret and every later epoch from the joiner
	// secret its commit produced, so both arms of the restore kind are driven here
	for _, epoch := range []uint64{0, 1} {
		if epoch != 0 {
			advanceEpochs(t, group, 1)
		}
		if group.Epoch() != epoch {
			t.Fatalf("Epoch = %d, want %d", group.Epoch(), epoch)
		}
		restored, err := LoadGroup(cfg, epoch, owner.SigPriv)
		if err != nil {
			t.Fatalf("LoadGroup at epoch %d: %v", epoch, err)
		}
		if !bytes.Equal(restored.EpochAuthenticator(), group.EpochAuthenticator()) {
			t.Fatalf("the group restored from epoch %d derives a different epoch authenticator", epoch)
		}
		// and it can open the next epoch and write that one down too
		advanceEpochs(t, restored, 1)
		reloaded, err := LoadGroup(cfg, epoch+1, owner.SigPriv)
		if err != nil {
			t.Fatalf("LoadGroup at the epoch a restored group opened from %d: %v", epoch, err)
		}
		if !bytes.Equal(reloaded.EpochAuthenticator(), restored.EpochAuthenticator()) {
			t.Fatalf("the epoch a group restored from %d committed to did not round trip", epoch)
		}
		reloaded.Close()
		restored.Close()
	}
	group.Close()
}

// TestLoadGroupRefusesAStateThisBuildDoesNotRead holds the two refusals the blob itself carries.
// A version this build does not write is a layout it would MISREAD -- every field after the first
// disagreement lands in the wrong place -- and a restore kind it has no constructor for is a state
// that was edited or written by a build with a third kind.
func TestLoadGroupRefusesAStateThisBuildDoesNotRead(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	cfg := testGroupConfig(t, crypto, owner, "group-1")
	store := cfg.Store.(*testStore)
	group, err := NewGroup(cfg, owner.SigPriv, BasicCredential(owner.IdentityPub))
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	group.Close()

	raw, err := store.GetGroupState([]byte("group-1"), 0)
	if err != nil {
		t.Fatalf("epoch 0 state was not persisted: %v", err)
	}
	// the control: the state as written restores
	restored, err := LoadGroup(cfg, 0, owner.SigPriv)
	if err != nil {
		t.Fatalf("LoadGroup over the state as written: %v", err)
	}
	restored.Close()

	for _, row := range []struct {
		name string
		edit func(blob *groupStateBlob)
		want error
	}{
		{
			name: "a blob version this build does not write",
			edit: func(blob *groupStateBlob) { blob.Version += 1 },
			want: errGroupStateBlobVersion,
		},
		{
			name: "a restore kind this build has no constructor for",
			edit: func(blob *groupStateBlob) { blob.RestoreKind = uint8(restoreFromJoiner) + 1 },
			want: errGroupStateRestoreKind,
		},
		// the row this gate exists for. Both arms take KDF.Nh pseudorandom octets, so the
		// other constructor accepts this epoch's secret and builds a complete, well formed
		// epoch out of it. Before the interim transcript check in LoadGroup this row read
		// LoadGroup = <nil> and handed back a group that agreed with nobody.
		{
			name: "the restore kind of the other arm",
			edit: func(blob *groupStateBlob) { blob.RestoreKind = uint8(restoreFromJoiner) },
			want: errGroupStateTranscript,
		},
		// and the same refusal reached from the secret rather than from the kind
		{
			name: "one flipped bit in the secret the schedule is rebuilt from",
			edit: func(blob *groupStateBlob) { blob.RestoreSecret[0] ^= 0x01 },
			want: errGroupStateTranscript,
		},
		// and from the confirmed transcript hash, which the tag is taken over
		{
			name: "a confirmed transcript hash this epoch did not close over",
			edit: func(blob *groupStateBlob) { blob.Confirmed = append(bytes.Clone(blob.Confirmed), 0x00) },
			want: errGroupStateTranscript,
		},
		// an own-leaf index the restored tree has no leaf at. It is refused BEFORE the schedule
		// is rebuilt, because everything after it reads that leaf: without the check the restore
		// answers a group whose credential is empty and whose leaf private state belongs to a
		// position in the tree somebody else holds.
		{
			name: "an own leaf index the tree has no leaf at",
			edit: func(blob *groupStateBlob) { blob.OwnLeaf = 4096 },
			want: ErrWelcomeLeafNotFound,
		},
	} {
		var blob groupStateBlob
		if err := syntax.UnmarshalLimit(bytes.Clone(raw), &blob, syntax.MaxRatchetTreeLength); err != nil {
			t.Fatalf("%s: the persisted state did not decode: %v", row.name, err)
		}
		row.edit(&blob)
		edited, err := syntax.MarshalLimit(&blob, syntax.MaxRatchetTreeLength)
		if err != nil {
			t.Fatalf("%s: re-encoding the edited state: %v", row.name, err)
		}
		if err := store.PutGroupState([]byte("group-1"), 0, edited); err != nil {
			t.Fatalf("%s: writing the edited state: %v", row.name, err)
		}
		loaded, err := LoadGroup(cfg, 0, owner.SigPriv)
		if !errors.Is(err, row.want) {
			if loaded != nil {
				loaded.Close()
			}
			t.Fatalf("%s: LoadGroup = %v, want %v", row.name, err, row.want)
		}
		if loaded != nil {
			loaded.Close()
			t.Fatalf("%s: LoadGroup refused and answered a group as well", row.name)
		}
	}
}

// TestLoadGroupRefusesASignerThatIsNotThisLeafs is what binding the restore to a VERIFIED context
// buys. The signing key is the caller's argument rather than a field of the blob, so nothing about
// the state says which key belongs with it; without the group info round trip a caller that
// restored a state with the wrong device key would get a group that signs messages every peer
// drops, one epoch later, with nothing at the point of the mistake to point at.
func TestLoadGroupRefusesASignerThatIsNotThisLeafs(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	stranger := testIdentity(t, crypto, "stranger")
	cfg := testGroupConfig(t, crypto, owner, "group-1")
	group, err := NewGroup(cfg, owner.SigPriv, BasicCredential(owner.IdentityPub))
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	group.Close()

	// the control first: the right key restores
	restored, err := LoadGroup(cfg, 0, owner.SigPriv)
	if err != nil {
		t.Fatalf("LoadGroup with this leaf's own signing key: %v", err)
	}
	restored.Close()

	loaded, err := LoadGroup(cfg, 0, stranger.SigPriv)
	if err == nil {
		loaded.Close()
		t.Fatal("LoadGroup restored a group under a signing key the tree's leaf does not name")
	}
}

// TestPersistRefusesAnEpochWhoseScheduleHasBeenErased holds the liveness check on the one secret
// the blob carries.
//
// An erase leaves KDF.Nh ZERO bytes rather than a short slice, and that is exactly the length both
// key schedule constructors require -- so without the check a state written out of an erased epoch
// restores, with a nil error, into a group whose every secret is an expansion of a value any party
// on earth can compute.
func TestPersistRefusesAnEpochWhoseScheduleHasBeenErased(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	cfg := testGroupConfig(t, crypto, owner, "group-1")
	group, err := NewGroup(cfg, owner.SigPriv, BasicCredential(owner.IdentityPub))
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	// the control: a live epoch marshals
	state, err := group.marshalState()
	if err != nil {
		t.Fatalf("marshalState over a live epoch: %v", err)
	}
	if len(state) == 0 {
		t.Fatal("a live epoch marshalled to nothing, so the refusal below says nothing")
	}
	group.schedule.Zeroize()
	if _, err := group.marshalState(); !errors.Is(err, errGroupStateRestoreSecret) {
		t.Fatalf("marshalState over an erased epoch = %v, want errGroupStateRestoreSecret", err)
	}
	// and it refuses through persist as well, which is the path a merge takes
	if err := group.persist(); !errors.Is(err, errGroupStateRestoreSecret) {
		t.Fatalf("persist over an erased epoch = %v, want errGroupStateRestoreSecret", err)
	}
	group.Close()
}

// refusingPutStore refuses PutGroupState once it is armed, which is the only way to reach the
// path the ordering below is about. Everything else delegates.
type refusingPutStore struct {
	*testStore
	refusing bool
}

var errTheStoreRefusedThisWrite = errors.New("the store refused this write")

func (self *refusingPutStore) PutGroupState(groupId []byte, epoch uint64, state []byte) error {
	if self.refusing {
		return errTheStoreRefusedThisWrite
	}
	return self.testStore.PutGroupState(groupId, epoch, state)
}

// TestAFailedPersistLeavesTheGroupAbleToProposeInTheEpochItMovedTo is the ORDER of the three
// things a merge does after the state swap, held to the epoch boundary's own terms.
//
// epoch_advance_test.go names the other order as a measured defect: the write, then a persist that
// can fail, then the rebind. Every failing persist then returns with the group moved and the
// proposal cache bound to the epoch that just closed -- and a cache bound to a closed epoch is a
// member that can resolve no proposal of the new epoch, never reaches the next boundary, and is
// never healed by anything in this package. A failed persist is recoverable: the next merged
// commit writes the state again.
//
// The failure is reached through the store because that is where it comes from in the field -- a
// disk that is full, a keychain that is locked, a sealed store whose key is not yet available at
// start-up -- and none of those is a reason to wedge the group.
//
// WHAT THIS CASE STOPPED OBSERVING WHEN THE SEAL LEARNED TO PERSIST, and where that went. It used
// to arm the store BEFORE CreateCommit and never disarm it, so a refusing store ran across the
// commit's seal and across the proposal's; the commit that made (*Group).sealAndRecordLocked
// persist moved the arming to after CreateCommit and disarmed it before ProposeUpdate, which is the
// right shape for a case about an epoch boundary and left NO test in the package running a refusing
// store across any seal site. seal_persist_test.go is the case that was missing:
// TestEverySealSiteRefusesWhenTheStoreRefusesToRecordWhatItSpent derives every declaration that
// reaches sealAndRecordLocked out of the source and drives a refusing store through each.
func TestAFailedPersistLeavesTheGroupAbleToProposeInTheEpochItMovedTo(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	cfg := testGroupConfig(t, crypto, owner, "group-1")
	store := &refusingPutStore{testStore: newTestStore()}
	cfg.Store = store
	group, err := NewGroup(cfg, owner.SigPriv, BasicCredential(owner.IdentityPub))
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	defer group.Close()

	// the control, on a group of its own so that the proposal it leaves in its cache is not a
	// proposal the subject's next commit has to cover: after an ORDINARY merge, a group can
	// propose in the epoch it moved to. Without it, an assertion that the subject can propose
	// would be satisfied by a package where nothing can fail rather than by the ordering.
	control := testNewGroup(t, crypto, testIdentity(t, crypto, "the control's owner"), "group-2")
	defer control.Close()
	advanceEpochs(t, control, 1)
	if _, err := control.ProposeUpdate(); err != nil {
		t.Fatalf("a group whose persist succeeded cannot propose in the epoch it moved to: %v", err)
	}

	advanceEpochs(t, group, 1)
	// the commit is BUILT before the store starts refusing, which it did not have to be until a
	// seal began recording the generation it spent: (*Group).sealAndRecordLocked persists inside
	// CreateCommit, so a store refusing from here would refuse the commit itself and this case
	// would never reach the merge it is about.
	if _, err := group.CreateCommit(nil, nil, nil); err != nil {
		t.Fatalf("CreateCommit: %v", err)
	}
	store.refusing = true
	before := group.Epoch()
	if err := group.MergePendingCommit(); !errors.Is(err, errTheStoreRefusedThisWrite) {
		t.Fatalf("MergePendingCommit over a refusing store = %v, want the store's own refusal", err)
	}
	// the group HAS moved -- the swap ran before the persist, and the epoch it opened is derived
	if group.Epoch() != before+1 {
		t.Fatalf("Epoch = %d after a refused persist, want %d; this test is about a group that moved",
			group.Epoch(), before+1)
	}
	// and the cache moved with it, which is the whole claim.
	//
	// The store is let go first, for the reason the commit was built before it started refusing: a
	// proposal is sealed and the seal is recorded, so a refusing store refuses ProposeUpdate for a
	// reason that has nothing to do with the epoch its cache is bound to -- and this case would
	// then be satisfied by a refusal that says nothing. What it observes is unchanged: after a
	// merge whose persist was refused, the group proposes in the epoch it moved to.
	store.refusing = false
	if _, err := group.ProposeUpdate(); err != nil {
		t.Fatalf("a group whose persist was refused cannot propose in the epoch it moved to: %v; the cache is bound to the epoch that closed and nothing in this package releases it",
			err)
	}
}

// TestARestoredMemberCanStillOpenAnUpdatePathAddressedToItsLeaf is the half of the round trip that
// a one-member fixture cannot see.
//
// Every secret comparison a single restored group makes is a comparison about the KEY SCHEDULE,
// and the leaf private key is not in it: a blob that stored no leaf key at all restores a group
// whose epoch authenticator, exporter and epoch secrets are all correct. What it cannot do is
// decrypt the next commit -- an UpdatePath is sealed to the encryption keys on the receiver's
// filtered direct path, and its leaf is the first of them -- so the member comes back, agrees with
// everybody, and then fails to process the first commit anybody sends.
//
// Measured: with OwnEncPriv left out of the blob, every other test in this task stayed green.
func TestARestoredMemberCanStillOpenAnUpdatePathAddressedToItsLeaf(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	group := testNewGroup(t, crypto, owner, "group-1")
	defer group.Close()

	bob := testIdentity(t, crypto, "bob")
	kp, initPriv, encPriv := testKeyPackage(t, crypto, bob)
	encoded, err := syntax.Marshal(kp)
	if err != nil {
		t.Fatalf("marshal bob's key package: %v", err)
	}
	if _, err := group.ProposeAdd(encoded); err != nil {
		t.Fatalf("ProposeAdd: %v", err)
	}
	welcoming, err := group.CreateCommit(nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateCommit adding bob: %v", err)
	}
	if err := group.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	bobCfg := testGroupConfig(t, crypto, bob, "group-1")
	joined, err := JoinFromWelcome(bobCfg, welcoming.Welcome, welcoming.RatchetTree, &JoinKeyMaterial{
		KeyPackage:     *kp,
		InitPrivate:    initPriv,
		EncryptPrivate: encPriv,
		SignPrivate:    bob.SigPriv,
	})
	if err != nil {
		t.Fatalf("JoinFromWelcome: %v", err)
	}
	joinedEpoch := joined.Epoch()
	joined.Close()

	restored, err := LoadGroup(bobCfg, joinedEpoch, bob.SigPriv)
	if err != nil {
		t.Fatalf("LoadGroup over the joiner's own state: %v", err)
	}
	defer restored.Close()

	// a commit with no proposals MUST carry an update path (RFC 9420 section 12.4), so this is
	// the path arm and not the pathless one
	next, err := group.CreateCommit(nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateCommit over the restored member: %v", err)
	}
	if staged := group.stagedForTest(); staged == nil || !staged.hasPath {
		t.Fatal("this commit carries no update path, so nothing here is addressed to the restored member's leaf")
	}
	processed, err := restored.ProcessMessage(next.Commit)
	if err != nil {
		t.Fatalf("the restored member could not open the update path addressed to its leaf: %v", err)
	}
	if err := restored.ApplyCommit(processed); err != nil {
		t.Fatalf("ApplyCommit: %v", err)
	}
	if err := group.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	if restored.Epoch() != group.Epoch() {
		t.Fatalf("the restored member is at epoch %d and the committer at %d",
			restored.Epoch(), group.Epoch())
	}
	if !bytes.Equal(restored.EpochAuthenticator(), group.EpochAuthenticator()) {
		t.Fatal("the restored member and the committer derive different epoch authenticators")
	}
}

// TestNoOctetTheRestoreHandsTheStoreIsItsCallersOwn is persist's discipline on the way back IN,
// and it is the one direction no gate of this package reads.
//
// TestNoOctetAGroupHandsOutwardIsStorageItKeeps follows what a GROUP hands to an object its caller
// supplied, and a restore is not a method on a group -- there is no group yet when the lookup is
// made. What goes outward there is the caller's own group id, handed to the caller's own store,
// and a store that keeps it to key a cache with shares an array with a config the caller is still
// holding and is entitled to reuse for the next group it founds.
func TestNoOctetTheRestoreHandsTheStoreIsItsCallersOwn(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	cfg := testGroupConfig(t, crypto, owner, "group-1")
	store := newWindowStore()
	cfg.Store = store
	founded, err := NewGroup(cfg, owner.SigPriv, BasicCredential(owner.IdentityPub))
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	founded.Close()

	restored, err := LoadGroup(cfg, 0, owner.SigPriv)
	if err != nil {
		t.Fatalf("LoadGroup: %v", err)
	}
	defer restored.Close()
	if len(store.getIds) == 0 {
		t.Fatal("the restore looked no state up, so this test compared nothing")
	}
	for at, id := range store.getIds {
		if len(id) == 0 {
			t.Fatalf("the group id handed to the store at %d is empty", at)
		}
		for i := range id {
			id[i] = 0xff
		}
	}
	if !bytes.Equal(cfg.GroupId, []byte("group-1")) {
		t.Fatalf("the caller's own config now names group %q after a store wrote through what the restore handed it",
			cfg.GroupId)
	}
	if !bytes.Equal(restored.GroupId(), []byte("group-1")) {
		t.Fatalf("the restored group's id is %q", restored.GroupId())
	}
}
