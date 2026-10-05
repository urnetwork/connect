// The gate on (*Group).PairwiseExport, and the one test in it that carries the whole reason the
// method exists.
//
// TestAThirdMemberWithTheWholeScheduleAndTreeDerivesSomethingDifferent is that test. Every other
// case here is a property a GROUP SCOPED exporter would also have had: two members agreeing, a
// closed group refusing, two epochs differing. The third member is the only one that separates a
// pairwise key from Group.Export, and it is written so that the separation is visible in the
// assertions rather than argued in a comment -- the same third member is first shown deriving the
// IDENTICAL group scoped Export the other two derive, from the same schedule and the same tree, and
// is then shown deriving something different from every pairwise call it can make.
//
// WHAT THE KAT IS FOR, and it is not a second opinion about HKDF. TestPairwiseExportDerivesOverThe
// RuledContext rebuilds the ruled context BY HAND -- fixed width big endian integers and a fixed 32
// bit length prefix written with encoding/binary, not with the syntax writer encodePairwiseContext
// uses -- and requires the answer to match.
//
// WHICH TEST KILLS WHICH TERM, MEASURED RATHER THAN ARGUED. Each term was removed from
// encodePairwiseContext or from the sort in (*Group).PairwiseExport and this file was re-run; the
// command is
//
//	go test ./mls/ -run 'TestPairwiseExport|TestTwoMembersDeriveTheSamePairwiseKey|TestAThirdMemberWithTheWholeScheduleAndTreeDerivesSomethingDifferent|TestTheSamePairDerivesADifferentKeyInADifferentEpoch|TestTwoPeersSharingAPublicPointDeriveDifferentPairwiseKeys' -timeout 600s
//
// and the answers were:
//
//	LP(group_id) removed              KAT only
//	u64(epoch) removed                KAT only
//	LP(epoch_authenticator) removed   KAT only
//	BOTH epoch terms removed          KAT + TestTheSamePairDerivesADifferentKeyInADifferentEpoch
//	u32(lo) and u32(hi) removed       KAT + TestTwoPeersSharingAPublicPointDeriveDifferentPairwiseKeys
//	either half of the sort removed   KAT + TestTwoMembersDeriveTheSamePairwiseKey + two more
//	LP(pk_lo) removed                 KAT only
//	LP(pk_hi) removed                 KAT only
//	both points removed               KAT only
//	pk_hi written before pk_lo        KAT only
//	LP spelled as MLS's varint        KAT only
//	the label dropped from Expand     KAT + TestPairwiseExportSeparatesTwoLabels
//
// THE ONE THAT CORRECTS A PLAUSIBLE READING, and it was written the wrong way here before it was
// measured: the EPOCH term has no behavioural killer of its own, and neither does the EPOCH
// AUTHENTICATOR, because each covers for the other. Remove either alone and the two-epoch case
// stays green, since the surviving term still moves when the epoch does. It is only with BOTH gone
// that TestTheSamePairDerivesADifferentKeyInADifferentEpoch goes red -- which is also what says
// that case is not vacuous.
//
// AND THE TERMS THE KAT ALONE HOLDS: group_id, both public points, and the LP spelling. No fixture
// this package can build distinguishes them. Both points are already inside the diffie-hellman, so
// naming them in the preimage changes nothing any member here can observe; two groups with
// different ids have different leaf keys as well, so their answers differ whether or not the id is
// in the preimage; and an LP spelled as a varint is self consistent across every member running it.
// They are there for what an ADVERSARY can do rather than for what this fixture can see --
// unknown-key-share for the points, cross-group separation for the id -- and a mirror of the ruled
// encoding is the only instrument that reaches them. That is a real limit of this file and it is
// written down rather than left to be inferred from a green run.
package mls

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/urnetwork/connect/mls/syntax"
)

// pairwiseTestLabel is the label the URmessage read receipt tag derives under. It is used here so
// that the KAT is taken over the string production will actually pass.
const pairwiseTestLabel = "URmessage/v1/receipt-pair"

// pairwiseContextByHand is the ruled context, rebuilt with encoding/binary rather than with the
// syntax writer the production path uses.
//
// A SECOND ENCODER AND NOT THE SAME ONE, which is the whole value of this helper. A KAT that
// called syntax.Writer.WriteOpaqueLP would agree with the production path about what LP means, so
// a path that had silently switched to the MLS varint opaque -- the one substitution the ruling
// names by name -- would be invisible to it. Four literal octets of big endian length, written out,
// cannot agree with a varint by accident.
func pairwiseContextByHand(groupId []byte, epoch uint64, lo uint32, hi uint32,
	pkLo []byte, pkHi []byte, epochAuthenticator []byte) []byte {

	out := []byte{}
	lp := func(bs []byte) {
		prefix := make([]byte, 4)
		binary.BigEndian.PutUint32(prefix, uint32(len(bs)))
		out = append(out, prefix...)
		out = append(out, bs...)
	}
	u32 := func(v uint32) {
		field := make([]byte, 4)
		binary.BigEndian.PutUint32(field, v)
		out = append(out, field...)
	}
	u64 := func(v uint64) {
		field := make([]byte, 8)
		binary.BigEndian.PutUint64(field, v)
		out = append(out, field...)
	}
	lp(groupId)
	u64(epoch)
	u32(lo)
	u32(hi)
	lp(pkLo)
	lp(pkHi)
	lp(epochAuthenticator)
	return out
}

// pairwiseExpected is what PairwiseExport must answer, computed from the group's own fields by a
// path that shares nothing with the method but the crypto provider and the DH wrapper.
func pairwiseExpected(t *testing.T, group *Group, peer LeafIndex, label string, length int) []byte {
	t.Helper()
	ownNode := group.tree.Leaf(group.ownLeaf)
	peerNode := group.tree.Leaf(peer)
	if ownNode == nil || peerNode == nil {
		t.Fatalf("the fixture holds no leaf for %d or %d, so this expectation is over nothing",
			group.ownLeaf, peer)
	}
	priv, err := X25519PrivateKey(group.ownPriv.EncryptionPriv)
	if err != nil {
		t.Fatalf("parse this member's own leaf scalar: %v", err)
	}
	pub, err := X25519PublicKey(peerNode.EncryptionKey)
	if err != nil {
		t.Fatalf("parse the peer's leaf point: %v", err)
	}
	dh, err := X25519DH(priv, pub)
	if err != nil {
		t.Fatalf("the static-static diffie-hellman this expectation is over: %v", err)
	}
	lo, hi := uint32(group.ownLeaf), uint32(peer)
	pkLo, pkHi := []byte(ownNode.EncryptionKey), []byte(peerNode.EncryptionKey)
	if hi < lo {
		lo, hi = hi, lo
		pkLo, pkHi = pkHi, pkLo
	}
	authenticator := group.schedule.Secrets().EpochAuthenticator
	context := pairwiseContextByHand(group.context.GroupId, group.context.Epoch, lo, hi,
		pkLo, pkHi, authenticator)
	// every field but the group id is fixed width at this suite: 4+8+4+4 of header, two 4+32
	// points and a 4+32 authenticator is 128, and the group id carries its own 4 octet prefix.
	// The ruling's 160 is this formula at URmessage's 32 octet group id, which the case below
	// asserts as the literal it is; here the formula is what catches a field that vanished under
	// a group id of some other width.
	if want := 128 + len(group.context.GroupId); len(context) != want {
		t.Fatalf("the ruled context is %d octets and its fields are %d: %d octets of group id, 8 of epoch, 8 of leaf indices, %d and %d of public points and %d of epoch authenticator",
			len(context), want, len(group.context.GroupId), len(pkLo), len(pkHi), len(authenticator))
	}
	return group.crypto.ExpandWithLabel(dh, label, context, length)
}

// TestPairwiseExportDerivesOverTheRuledContext is the KAT, and it is the killer for every term of
// the context. See this file's header for which terms have no other one.
func TestPairwiseExportDerivesOverTheRuledContext(t *testing.T) {
	crypto := testCrypto(t)
	// A THIRTY-TWO OCTET GROUP ID, which is not decoration: the ruling fixes the context at 160
	// octets and that total is only true at URmessage's own group id width. The assertion below
	// is the literal, so a term that vanished shortens it by a nameable amount.
	const groupId = "group-pairwise-kat-0123456789abc"
	if len(groupId) != 32 {
		t.Fatalf("this case's group id is %d octets and the 160 it asserts is a 32 octet group id's total", len(groupId))
	}
	fixture := testGroupOfSize(t, crypto, groupId, 3)
	defer fixture.closeAll()

	if got := len(pairwiseContextByHand(fixture.at(t, 0).group.context.GroupId, 1, 0, 1,
		make([]byte, 32), make([]byte, 32), make([]byte, 32))); got != 160 {
		t.Fatalf("the ruled context is %d octets at a 32 octet group id and the ruling fixes it at 160", got)
	}

	for _, row := range []struct {
		from LeafIndex
		to   LeafIndex
	}{
		// both directions of one pair, so the sort runs in both of its arms, and a third pair so
		// that a context that happened to be right for (0,1) alone is not what this reads
		{from: 0, to: 1},
		{from: 1, to: 0},
		{from: 2, to: 0},
		{from: 1, to: 2},
	} {
		group := fixture.at(t, row.from).group
		got, err := group.PairwiseExport(pairwiseTestLabel, row.to, 32)
		if err != nil {
			t.Fatalf("PairwiseExport(%d -> %d): %v", row.from, row.to, err)
		}
		if len(got) != 32 {
			t.Fatalf("PairwiseExport(%d -> %d) answered %d octets, want 32", row.from, row.to, len(got))
		}
		want := pairwiseExpected(t, group, row.to, pairwiseTestLabel, 32)
		if !bytes.Equal(got, want) {
			t.Errorf("PairwiseExport(%d -> %d) answered %s and the ruled context derives %s",
				row.from, row.to, hex.EncodeToString(got), hex.EncodeToString(want))
		}
	}
}

// TestPairwiseExportAnswersTheLengthItWasAsked holds the one thing the KAT above cannot see about
// the output size, since it asks for one length only. The two answers must differ rather than one
// being a prefix of the other, which is what KDFLabel's own length field buys.
func TestPairwiseExportAnswersTheLengthItWasAsked(t *testing.T) {
	crypto := testCrypto(t)
	alice, _, _, _ := testTwoMemberGroup(t, crypto)
	defer alice.Close()

	short, err := alice.PairwiseExport(pairwiseTestLabel, 1, 32)
	if err != nil {
		t.Fatalf("PairwiseExport at 32 octets: %v", err)
	}
	long, err := alice.PairwiseExport(pairwiseTestLabel, 1, 64)
	if err != nil {
		t.Fatalf("PairwiseExport at 64 octets: %v", err)
	}
	if len(short) != 32 || len(long) != 64 {
		t.Fatalf("answered %d and %d octets, want 32 and 64", len(short), len(long))
	}
	if bytes.Equal(long[:32], short) {
		t.Error("the 64 octet answer opens with the 32 octet answer, so the requested length is not inside the derivation and two callers asking for two lengths share key material")
	}
}

// TestPairwiseExportSeparatesTwoLabels is the domain separation ExpandWithLabel is there for: the
// receipt key and any other caller's key over the same pair and the same epoch are different keys.
func TestPairwiseExportSeparatesTwoLabels(t *testing.T) {
	crypto := testCrypto(t)
	alice, _, _, _ := testTwoMemberGroup(t, crypto)
	defer alice.Close()

	receipt, err := alice.PairwiseExport(pairwiseTestLabel, 1, 32)
	if err != nil {
		t.Fatalf("PairwiseExport under the receipt label: %v", err)
	}
	other, err := alice.PairwiseExport("URmessage/v1/some-other-pairwise-caller", 1, 32)
	if err != nil {
		t.Fatalf("PairwiseExport under a second label: %v", err)
	}
	if bytes.Equal(receipt, other) {
		t.Error("two labels over one pair and one epoch derive the same key, so the label is not in the derivation")
	}
}

// TestTwoMembersDeriveTheSamePairwiseKey is the SYMMETRY case: the two members of a pair derive
// BYTE EQUAL material with nothing exchanged between them.
//
// It is the pairwise twin of the exporter assertion in commit_process_test.go, and it reads the two
// answers off two groups that have never seen each other's private state -- a committer and a
// joiner, each holding only its own leaf scalar and the public tree.
//
// IT IS ALSO THE BEHAVIOURAL KILLER FOR THE (lo, hi) SORT. The two sides call the method with the
// pair swapped, so a body that wrote its own leaf first and the peer's second would answer two
// different contexts here and this case would report it.
func TestTwoMembersDeriveTheSamePairwiseKey(t *testing.T) {
	crypto := testCrypto(t)
	alice, bob, _, _ := testTwoMemberGroup(t, crypto)
	defer alice.Close()
	defer bob.Close()

	if alice.OwnLeafIndex() == bob.OwnLeafIndex() {
		t.Fatalf("both views of this fixture sit at leaf %d, so there is no pair here", alice.OwnLeafIndex())
	}
	if !bytes.Equal(alice.EpochAuthenticator(), bob.EpochAuthenticator()) {
		t.Fatal("the two members are not in one epoch, so an agreement between them would say nothing")
	}
	fromAlice, err := alice.PairwiseExport(pairwiseTestLabel, bob.OwnLeafIndex(), 32)
	if err != nil {
		t.Fatalf("alice's PairwiseExport: %v", err)
	}
	fromBob, err := bob.PairwiseExport(pairwiseTestLabel, alice.OwnLeafIndex(), 32)
	if err != nil {
		t.Fatalf("bob's PairwiseExport: %v", err)
	}
	if len(fromAlice) != 32 {
		t.Fatalf("alice derived %d octets, so a comparison against it says nothing", len(fromAlice))
	}
	if !bytes.Equal(fromAlice, fromBob) {
		t.Errorf("alice derived %s and bob derived %s for the same pair at the same epoch, so the key is not symmetric and no tag either of them writes verifies at the other",
			hex.EncodeToString(fromAlice), hex.EncodeToString(fromBob))
	}
}

// TestAThirdMemberWithTheWholeScheduleAndTreeDerivesSomethingDifferent IS THE TEST THIS WHOLE
// PRIMITIVE EXISTS FOR, AND IT IS THE ONE A GROUP SCOPED KEY COULD NEVER PASS.
//
// The third member c is not a weakened observer. It is a full member of the same group at the same
// epoch: it holds the entire key schedule, every epoch secret, the whole ratchet tree and every
// public leaf point in it. The case ASSERTS that, rather than assuming it, in the only way that
// matters -- c is first shown deriving the IDENTICAL Group.Export that a and b derive, from the
// same label and the same epoch. That is the counterfactual made concrete: had the receipt tag been
// keyed by the group exporter, the value c holds at that line IS the key, and c forges every
// member's receipt at will.
//
// And then every pairwise value c can reach is required to differ from k_pair(a, b). c has no call
// it can make that reaches that key, because the key is a Diffie-Hellman over two leaf scalars c
// does not hold; what this case pins is that nothing c CAN call lands on it by another route.
func TestAThirdMemberWithTheWholeScheduleAndTreeDerivesSomethingDifferent(t *testing.T) {
	crypto := testCrypto(t)
	fixture := testGroupOfSize(t, crypto, "group-pairwise-third-member", 3)
	defer fixture.closeAll()

	a := fixture.at(t, 0).group
	b := fixture.at(t, 1).group
	c := fixture.at(t, 2).group

	// FIRST: c holds everything a group scoped key is made of, asserted rather than assumed.
	if !bytes.Equal(c.EpochAuthenticator(), a.EpochAuthenticator()) {
		t.Fatal("the third member is not in the same epoch as the pair, so nothing below is the case this test is named for")
	}
	groupScoped := map[string][]byte{}
	for name, member := range map[string]*Group{"a": a, "b": b, "c": c} {
		exported, err := member.Export(pairwiseTestLabel, nil, 32)
		if err != nil {
			t.Fatalf("%s's group scoped Export: %v", name, err)
		}
		groupScoped[name] = exported
	}
	if !bytes.Equal(groupScoped["c"], groupScoped["a"]) || !bytes.Equal(groupScoped["c"], groupScoped["b"]) {
		t.Fatalf("the three members do not agree on Group.Export, so this fixture is not one epoch and the contrast below is not the one this test is about: a %s, b %s, c %s",
			hex.EncodeToString(groupScoped["a"]), hex.EncodeToString(groupScoped["b"]),
			hex.EncodeToString(groupScoped["c"]))
	}
	tree, err := c.RatchetTree()
	if err != nil {
		t.Fatalf("the third member's ratchet tree: %v", err)
	}
	fromA, err := a.RatchetTree()
	if err != nil {
		t.Fatalf("the first member's ratchet tree: %v", err)
	}
	if !bytes.Equal(tree, fromA) {
		t.Fatal("the third member holds a different tree from the pair, so it is not the fully informed member this test is about")
	}
	for _, leaf := range []LeafIndex{0, 1, 2} {
		if c.tree.Leaf(leaf) == nil || len(c.tree.Leaf(leaf).EncryptionKey) == 0 {
			t.Fatalf("the third member holds no public encryption point for leaf %d, so it is not as well informed as this test claims",
				leaf)
		}
	}

	// SECOND: the pair's key, which the two of them agree on.
	pairFromA, err := a.PairwiseExport(pairwiseTestLabel, 1, 32)
	if err != nil {
		t.Fatalf("a's PairwiseExport with b: %v", err)
	}
	pairFromB, err := b.PairwiseExport(pairwiseTestLabel, 0, 32)
	if err != nil {
		t.Fatalf("b's PairwiseExport with a: %v", err)
	}
	if !bytes.Equal(pairFromA, pairFromB) {
		t.Fatal("the pair does not agree on its own key, so the comparison below is against nothing")
	}

	// THIRD: and c, holding all of the above, derives something else at every call it can make.
	for _, peer := range []LeafIndex{0, 1} {
		derived, err := c.PairwiseExport(pairwiseTestLabel, peer, 32)
		if err != nil {
			t.Fatalf("c's PairwiseExport with leaf %d: %v", peer, err)
		}
		if bytes.Equal(derived, pairFromA) {
			t.Errorf("the third member derived the pair's own key %s by naming leaf %d, so a member outside the pair reaches it and the whole reason this primitive is not Group.Export is gone",
				hex.EncodeToString(derived), peer)
		}
	}
	// and the two values c CAN have are not each other either, which is the same property one
	// level down: c's key with a is not c's key with b
	withA, err := c.PairwiseExport(pairwiseTestLabel, 0, 32)
	if err != nil {
		t.Fatalf("c's PairwiseExport with a: %v", err)
	}
	withB, err := c.PairwiseExport(pairwiseTestLabel, 1, 32)
	if err != nil {
		t.Fatalf("c's PairwiseExport with b: %v", err)
	}
	if bytes.Equal(withA, withB) {
		t.Error("the third member derives one key for both of its peers, so the peer is not in the derivation")
	}
	// the contrast, printed so that a reader of a passing run sees what the case measured
	t.Logf("all three members derive the group scoped Export %s; the pair (0,1) derives %s and the third member reaches %s with leaf 0 and %s with leaf 1",
		hex.EncodeToString(groupScoped["c"]), hex.EncodeToString(pairFromA),
		hex.EncodeToString(withA), hex.EncodeToString(withB))
}

// TestTheSamePairDerivesADifferentKeyInADifferentEpoch is the behavioural killer for the epoch
// term, and it is what puts a pairwise key under the erase discipline the ruling requires: a key
// that survived a commit would outlive the schedule that is pruned at 32 epochs.
//
// The pair is the SAME two leaves and the same two devices across the commit, which is what makes
// the case say something: nothing about the members changed, only the epoch did.
func TestTheSamePairDerivesADifferentKeyInADifferentEpoch(t *testing.T) {
	crypto := testCrypto(t)
	fixture := testGroupOfSize(t, crypto, "group-pairwise-epochs", 3)
	defer fixture.closeAll()

	a := fixture.at(t, 0).group
	b := fixture.at(t, 1).group
	before, err := a.PairwiseExport(pairwiseTestLabel, 1, 32)
	if err != nil {
		t.Fatalf("PairwiseExport before the commit: %v", err)
	}
	epochBefore := a.Epoch()

	// the commit comes from the THIRD member, so neither of the pair replaced its own leaf key and
	// the only thing separating the two answers is the epoch the commit opened
	fixture.commitFrom(t, 2)
	if a.Epoch() == epochBefore {
		t.Fatal("the commit did not advance the epoch, so this case compares one epoch with itself")
	}
	after, err := a.PairwiseExport(pairwiseTestLabel, 1, 32)
	if err != nil {
		t.Fatalf("PairwiseExport after the commit: %v", err)
	}
	if bytes.Equal(before, after) {
		t.Errorf("the pair derives %s at epoch %d and at epoch %d, so the key does not move with the epoch and outlives the schedule that is erased at 32",
			hex.EncodeToString(before), epochBefore, a.Epoch())
	}
	// and the pair still agrees at the new epoch, which says the value moved rather than broke
	fromB, err := b.PairwiseExport(pairwiseTestLabel, 0, 32)
	if err != nil {
		t.Fatalf("b's PairwiseExport after the commit: %v", err)
	}
	if !bytes.Equal(after, fromB) {
		t.Error("the pair no longer agrees after a commit, so the key moved by breaking rather than by advancing")
	}
}

// TestTwoPeersSharingAPublicPointDeriveDifferentPairwiseKeys is the unknown-key-share case as far
// as this package can build it, and it is the second behavioural killer for the (lo, hi) terms.
//
// A leaf carrying a COPY of another member's encryption point is spliced into the ratchet tree, so
// the diffie-hellman against it is BYTE IDENTICAL to the diffie-hellman against the member it
// copied. The only thing separating the two derivations is the pair of leaf indices in the context.
// Remove them and the two answers collide, which is a member deriving another member's key by
// claiming its point.
func TestTwoPeersSharingAPublicPointDeriveDifferentPairwiseKeys(t *testing.T) {
	crypto := testCrypto(t)
	alice, bob, _, _ := testTwoMemberGroup(t, crypto)
	defer alice.Close()
	defer bob.Close()

	genuine := alice.tree.Leaf(bob.OwnLeafIndex())
	if genuine == nil {
		t.Fatalf("the fixture holds no leaf at %d", bob.OwnLeafIndex())
	}
	impostor, _ := testLeafNode(t, crypto, testIdentity(t, crypto, "the leaf that copied bob's point"))
	impostor.EncryptionKey = genuine.EncryptionKey
	at, err := alice.tree.AddLeaf(impostor)
	if err != nil {
		t.Fatalf("splice the copying leaf into the tree: %v", err)
	}
	if at == bob.OwnLeafIndex() {
		t.Fatalf("the copying leaf landed on bob's own position %d, so there is no second position here", at)
	}
	withBob, err := alice.PairwiseExport(pairwiseTestLabel, bob.OwnLeafIndex(), 32)
	if err != nil {
		t.Fatalf("PairwiseExport with bob: %v", err)
	}
	withImpostor, err := alice.PairwiseExport(pairwiseTestLabel, at, 32)
	if err != nil {
		t.Fatalf("PairwiseExport with the copying leaf: %v", err)
	}
	if bytes.Equal(withBob, withImpostor) {
		t.Errorf("leaf %d and leaf %d publish one point and derive one key %s, so a member that republishes another member's encryption key derives that member's pairwise keys",
			bob.OwnLeafIndex(), at, hex.EncodeToString(withBob))
	}
}

// TestPairwiseExportRefusesOwnLeaf holds ErrPairwiseSelf. A diffie-hellman with one's own point is
// a perfectly good 32 octets and is not a two party key, so the refusal is what keeps a caller from
// being handed a value it cannot tell from a real one.
func TestPairwiseExportRefusesOwnLeaf(t *testing.T) {
	crypto := testCrypto(t)
	alice, _, _, _ := testTwoMemberGroup(t, crypto)
	defer alice.Close()

	answer, err := alice.PairwiseExport(pairwiseTestLabel, alice.OwnLeafIndex(), 32)
	if !errors.Is(err, ErrPairwiseSelf) {
		t.Errorf("PairwiseExport with its own leaf answered %v, want ErrPairwiseSelf", err)
	}
	if answer != nil {
		t.Errorf("the refusal came with %d octets beside it, which a caller that ignored the error would use as a key",
			len(answer))
	}
}

// TestPairwiseExportRefusesABlankLeaf holds ErrBlankLeaf, over both shapes a blank position takes:
// a position inside the tree that holds no node, and a position past the end of it.
func TestPairwiseExportRefusesABlankLeaf(t *testing.T) {
	crypto := testCrypto(t)
	fixture := testGroupOfSize(t, crypto, "group-pairwise-blank", 3)
	defer fixture.closeAll()
	alice := fixture.at(t, 0).group

	// the tree of a three member group is held at the full width of four leaves, so leaf 3 is
	// inside it and holds nothing
	if alice.tree.Leaf(3) != nil {
		t.Fatalf("leaf 3 of this three member fixture holds a node, so this case is not reading a blank position")
	}
	for _, peer := range []LeafIndex{3, 500, 1 << 20} {
		answer, err := alice.PairwiseExport(pairwiseTestLabel, peer, 32)
		if !errors.Is(err, ErrBlankLeaf) {
			t.Errorf("PairwiseExport naming leaf %d answered %v, want ErrBlankLeaf", peer, err)
		}
		if answer != nil {
			t.Errorf("the refusal for leaf %d came with %d octets beside it", peer, len(answer))
		}
	}
}

// TestPairwiseExportRefusesAnImpossibleLength and the case below it are (*KeySchedule).Export's
// two refusals asked through this door, and neither is hygiene: both of these arguments reach a
// CryptoProvider method whose signature cannot report anything, so without the refusal a caller's
// mistake is a PANIC rather than an error. The length lands in Expand and the label in
// mlsLabelBytes.
func TestPairwiseExportRefusesAnImpossibleLength(t *testing.T) {
	crypto := testCrypto(t)
	alice, _, _, _ := testTwoMemberGroup(t, crypto)
	defer alice.Close()

	for _, length := range []int{-1, 255*crypto.HashSize() + 1} {
		answer, err := alice.PairwiseExport(pairwiseTestLabel, 1, length)
		if !errors.Is(err, ErrExportLength) {
			t.Errorf("PairwiseExport at length %d answered %v, want ErrExportLength", length, err)
		}
		if answer != nil {
			t.Errorf("the refusal at length %d came with %d octets beside it", length, len(answer))
		}
	}
	// and the ceiling itself is answered rather than refused, so the bound above is the ceiling
	// and not a band below it
	if _, err := alice.PairwiseExport(pairwiseTestLabel, 1, 255*crypto.HashSize()); err != nil {
		t.Errorf("PairwiseExport at the ceiling answered %v, so the refusal above starts too low", err)
	}
}

func TestPairwiseExportRefusesAnOverLongLabel(t *testing.T) {
	crypto := testCrypto(t)
	alice, _, _, _ := testTwoMemberGroup(t, crypto)
	defer alice.Close()

	answer, err := alice.PairwiseExport(strings.Repeat("l", syntax.MaxVectorLength), 1, 32)
	if !errors.Is(err, syntax.ErrLengthExceedsMax) {
		t.Errorf("PairwiseExport under a label one labelled field cannot hold answered %v, want ErrLengthExceedsMax",
			err)
	}
	if answer != nil {
		t.Errorf("the refusal came with %d octets beside it", len(answer))
	}
}

// TestPairwiseExportRefusesAClosedGroup is Export's own refusal over this method, and it is not
// hygiene: Close zeroizes the leaf scalar this derivation is over, so a body that read it after the
// close would derive over zeros and answer a key every closed group in the process agrees on.
func TestPairwiseExportRefusesAClosedGroup(t *testing.T) {
	crypto := testCrypto(t)
	alice, _, _, _ := testTwoMemberGroup(t, crypto)
	peer := LeafIndex(1)
	if err := alice.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	answer, err := alice.PairwiseExport(pairwiseTestLabel, peer, 32)
	if !errors.Is(err, errGroupClosed) {
		t.Errorf("PairwiseExport on a closed group answered %v, want errGroupClosed", err)
	}
	if answer != nil {
		t.Errorf("the refusal came with %d octets beside it", len(answer))
	}
}

// TestThePairwiseKeyDependsOnTheLeafPrivateScalarAndNotOnGroupSecretsAlone is the case that
// actually separates this primitive from [Group.Export], and it exists because the case NAMED for
// that property does not hold it.
//
// WHY TestAThirdMemberWithTheWholeScheduleAndTreeDerivesSomethingDifferent IS NOT THIS CASE,
// MEASURED RATHER THAN ARGUED. PairwiseExport always puts self.ownLeaf on one side, so a third
// member CANNOT NAME THE PAIR'S KEY THROUGH THIS API AT ALL -- c can ask only for (c,a) and (c,b).
// Those differ from (a,b) because the CONTEXT differs, and the context differs just as much when
// the key material is group scoped as when it is a DH. Substituting
//
//	dh, err := self.schedule.Export("...", nil, 32)      // i.e. shape C, which item 228 rejects
//
// for the X25519DH call leaves that case GREEN. Run at connect 4a70be8:
//
//	go test ./mls/ -run 'TestAThirdMemberWithTheWholeScheduleAndTreeDerivesSomethingDifferent' \
//	    -timeout 600s -count=1     ->  ok, WITH THE MUTATION APPLIED
//
// So the third-member case measures that the PEER is in the derivation. It does not measure that
// the SECRET is pairwise, which is the whole of ledger item 228's third-member property and the
// only reason this method is not Group.Export.
//
// THIS CASE HOLDS IT, at the one place it is decidable. Everything group scoped is held fixed --
// same group, same epoch, same schedule, same ratchet tree, same peer, and therefore the same
// context octets, since the context reads its points out of the TREE and not out of the caller --
// and ONLY the caller's own leaf scalar changes. A group scoped key cannot notice that. A DH
// cannot ignore it.
func TestThePairwiseKeyDependsOnTheLeafPrivateScalarAndNotOnGroupSecretsAlone(t *testing.T) {
	crypto := testCrypto(t)
	fixture := testGroupOfSize(t, crypto, "group-pairwise-scalar", 3)
	defer fixture.closeAll()

	a := fixture.at(t, 0).group

	before, err := a.PairwiseExport(pairwiseTestLabel, 1, 32)
	if err != nil {
		t.Fatalf("a's PairwiseExport with b: %v", err)
	}
	groupBefore, err := a.Export(pairwiseTestLabel, nil, 32)
	if err != nil {
		t.Fatalf("a's group scoped Export: %v", err)
	}

	// THE ONLY CHANGE IN THIS CASE. A fresh leaf scalar of the same suite, swapped in while the
	// tree, the schedule and the epoch stay exactly where they are. The tree still carries a's OLD
	// public point, which is deliberate: it keeps the context octets identical, so the only thing
	// that can move the answer is the secret.
	fresh, _, err := crypto.DeriveKeyPair([]byte("a leaf scalar this group never had"))
	if err != nil {
		t.Fatalf("deriving a replacement leaf scalar: %v", err)
	}
	if bytes.Equal(fresh, a.ownPriv.EncryptionPriv) {
		t.Fatal("the replacement scalar is the one a already held, so the swap below changes nothing and this case would pass vacuously")
	}
	a.ownPriv.EncryptionPriv = fresh

	after, err := a.PairwiseExport(pairwiseTestLabel, 1, 32)
	if err != nil {
		t.Fatalf("a's PairwiseExport with b after the scalar swap: %v", err)
	}

	// the contrast is only about the scalar if everything group scoped really did stay put
	groupAfter, err := a.Export(pairwiseTestLabel, nil, 32)
	if err != nil {
		t.Fatalf("a's group scoped Export after the swap: %v", err)
	}
	if !bytes.Equal(groupBefore, groupAfter) {
		t.Fatalf("the swap moved the GROUP scoped export too (%s -> %s), so what this case measures is not the leaf scalar",
			hex.EncodeToString(groupBefore), hex.EncodeToString(groupAfter))
	}

	if bytes.Equal(before, after) {
		t.Errorf("the pairwise key did not move when the caller's own leaf scalar was replaced, so it is derived from material every member of this group holds: that is Group.Export wearing a pairwise signature, and ledger item 228's third-member property -- 'a third group member cannot forge, at any group size' -- is NOT delivered. before %s, after %s",
			hex.EncodeToString(before), hex.EncodeToString(after))
	}
	t.Logf("the group scoped export stayed %s across the swap; the pairwise key moved %s -> %s",
		hex.EncodeToString(groupBefore), hex.EncodeToString(before), hex.EncodeToString(after))
}
