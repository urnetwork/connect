package mls

import (
	"bytes"
	"errors"
	"slices"
	"testing"

	"github.com/urnetwork/connect/mls/syntax"
)

func TestLeafKeysOfReadsTheMembersWrapKey(t *testing.T) {
	crypto := testCrypto(t)
	alice := testIdentity(t, crypto, "alice")
	leaf, _ := testLeafNode(t, crypto, alice)

	keys, err := LeafKeysOf(leaf)
	if err != nil {
		t.Fatalf("LeafKeysOf: %v", err)
	}
	if keys.AlgId != AlgIdXwing {
		t.Fatalf("AlgId = %#04x, want %#04x", keys.AlgId, AlgIdXwing)
	}
	if !bytes.Equal(keys.DeviceXwingPub, alice.XwingPub) {
		t.Fatal("LeafKeysOf returned a different X-Wing key than the leaf carries")
	}
	if len(keys.DeviceXwingPub) != XwingPublicKeyLen {
		t.Fatalf("DeviceXwingPub length = %d, want %d", len(keys.DeviceXwingPub), XwingPublicKeyLen)
	}
}

func TestLeafKeysOfMissingExtension(t *testing.T) {
	leaf := &LeafNode{}
	if _, err := LeafKeysOf(leaf); !errors.Is(err, ErrMalformedExtension) {
		t.Fatalf("LeafKeysOf error = %v, want ErrMalformedExtension for a leaf with no 0xF002", err)
	}
}

func TestLeafKeysOfNilLeaf(t *testing.T) {
	if _, err := LeafKeysOf(nil); !errors.Is(err, ErrMalformedExtension) {
		t.Fatalf("LeafKeysOf(nil) error = %v, want ErrMalformedExtension", err)
	}
}

func TestLeafKeysOfPropagatesAMalformedBody(t *testing.T) {
	crypto := testCrypto(t)
	alice := testIdentity(t, crypto, "alice")
	leaf, _ := testLeafNode(t, crypto, alice)
	for i := range leaf.Extensions {
		if leaf.Extensions[i].ExtensionType == ExtensionTypeUrmessageLeafKeys {
			leaf.Extensions[i].ExtensionData = []byte{0x00}
		}
	}
	if _, err := LeafKeysOf(leaf); err == nil {
		t.Fatal("LeafKeysOf accepted a truncated urmessage_leaf_keys body")
	}
}

// TestLeafKeysOfNamesBothTheAccessorsRefusalAndTheCodecsForAMalformedBody is the half the plan's
// propagation test cannot see.
//
// "not nil" is satisfied by an accessor that answered ErrMalformedExtension having never called
// the codec at all, and by one that answered the codec's refusal having lost the accessor's. The
// two sentinels name different repairs -- this leaf has no wrap key I can read, versus this leaf
// has one this profile cannot wrap to -- and a caller that has to tell them apart can only do it
// if BOTH survive the wrap. That is what the two %w verbs in LeafKeysOf are for, and a single
// one of them passes every assertion above.
func TestLeafKeysOfNamesBothTheAccessorsRefusalAndTheCodecsForAMalformedBody(t *testing.T) {
	crypto := testCrypto(t)
	alice := testIdentity(t, crypto, "alice")
	leaf, _ := testLeafNode(t, crypto, alice)
	// a well formed body under an alg_id this profile does not implement, which is the codec's
	// OWN refusal rather than a truncation: 0x0012 is registered in MASTER section 7.1 and is
	// unimplemented in v1
	body, err := marshalBytes(func(w *syntax.Writer) error {
		w.WriteUint16(0x0012)
		w.WriteOpaque(make([]byte, XwingPublicKeyLen))
		return nil
	})
	if err != nil {
		t.Fatalf("the body this test hands the accessor: %v", err)
	}
	for i := range leaf.Extensions {
		if leaf.Extensions[i].ExtensionType == ExtensionTypeUrmessageLeafKeys {
			leaf.Extensions[i].ExtensionData = body
		}
	}
	_, err = LeafKeysOf(leaf)
	if !errors.Is(err, ErrMalformedExtension) {
		t.Errorf("LeafKeysOf error = %v, want it to carry ErrMalformedExtension, which is what the group lifecycle asks this question with", err)
	}
	if !errors.Is(err, ErrLeafKeysExtensionInvalid) {
		t.Errorf("LeafKeysOf error = %v, want it to carry ErrLeafKeysExtensionInvalid as well; a caller told only that the extension is malformed cannot tell a leaf with no wrap key from a leaf whose wrap key this profile cannot use", err)
	}
}

// TestLeafKeysOfReadsTheEntryOfItsOwnTypeAndNoOther is the property the plan's four tests leave
// entirely uncovered, and it is the one that matters most.
//
// Every fixture leaf in this package carries exactly ONE extension, so an accessor rewritten to
// answer leaf.Extensions[0] -- ignoring the type it selects on altogether -- passes all four of
// them. What it would actually do is read a wrap target out of whatever extension happened to sit
// in that slot, and a commit secret wrapped to it goes to nobody: not a parse failure anywhere,
// just a device that silently stops receiving. So the leaves here carry the leaf keys entry LAST,
// behind two entries whose bodies are not X-Wing keys.
//
// Both directions. A leaf whose only extensions are somebody else's must be refused rather than
// misread, and a leaf that carries the real one behind them must still answer it.
func TestLeafKeysOfReadsTheEntryOfItsOwnTypeAndNoOther(t *testing.T) {
	crypto := testCrypto(t)
	alice := testIdentity(t, crypto, "alice")
	leaf, _ := testLeafNode(t, crypto, alice)

	// a real group policy body, because 0xF001 is the neighbour a caller actually reaches by
	// accident: it is declared one line from 0xF002 and its body is a well formed extension body
	policy, err := (&GroupPolicyExtension{
		Roles: []RoleEntry{{MemberId: alice.IdentityPub, Role: RoleOwner}},
	}).Encode()
	if err != nil {
		t.Fatalf("the group policy entry this test puts in front: %v", err)
	}
	decoys := []Extension{
		{ExtensionType: ExtensionTypeApplicationId, ExtensionData: []byte("urmessage")},
		policy,
	}

	refused := &LeafNode{Extensions: decoys}
	if _, err := LeafKeysOf(refused); !errors.Is(err, ErrMalformedExtension) {
		t.Fatalf("LeafKeysOf over a leaf carrying only %#04x and %#04x error = %v, want ErrMalformedExtension; an accessor that answers whatever entry is first hands back a wrap target read out of somebody else's body",
			uint16(ExtensionTypeApplicationId), uint16(ExtensionTypeUrmessageGroupPolicy), err)
	}

	behind := &LeafNode{Extensions: append(append([]Extension{}, decoys...), leaf.Extensions...)}
	keys, err := LeafKeysOf(behind)
	if err != nil {
		t.Fatalf("LeafKeysOf over a leaf whose 0xF002 entry is not first: %v", err)
	}
	if !bytes.Equal(keys.DeviceXwingPub, alice.XwingPub) {
		t.Fatal("LeafKeysOf answered something other than the leaf's own X-Wing key when the entry was not first")
	}
}

// TestLeafKeysOfRefusesALeafCarryingTwoWrapKeysRatherThanPickingOne is finding 8's other half,
// and it is a refusal rather than a test pinning which of two illegal entries wins.
//
// The prior question -- is a repeated extension type refused anywhere -- had the answer
// "nowhere". ValSem209 was named in three comments of this package and implemented in none;
// LeafNode.Validate, the door those comments pointed at, walks every entry and range checks every
// urmessage_leaf_keys body it finds, and accepts a leaf carrying two well formed ones on purpose.
// So this accessor was the only reader of such a leaf, and it was choosing the group's wrap
// target by iteration order: not a parse failure anywhere, just a commit secret wrapped to one of
// two devices while the other silently stops receiving. Reversing the walk changed which device
// that was and passed the whole suite.
//
// The answer is now "at the lookup", once, for the whole package: FindExtensionEntry refuses the
// repeat and this accessor names the type. What this test states is unchanged by that -- the
// refusal has to be observable THROUGH LeafKeysOf, which is what a caller holds -- and a
// delegation that dropped the error on the floor would fail here.
//
// Both orders, and a decoy between them, because a refusal that only fires for one arrangement is
// an accessor still answering by position.
func TestLeafKeysOfRefusesALeafCarryingTwoWrapKeysRatherThanPickingOne(t *testing.T) {
	crypto := testCrypto(t)
	alice := testIdentity(t, crypto, "alice")
	bob := testIdentity(t, crypto, "bob")
	aliceKeys, bobKeys := testLeafKeys(t, alice), testLeafKeys(t, bob)
	if bytes.Equal(aliceKeys.ExtensionData, bobKeys.ExtensionData) {
		t.Fatal("the two leaf keys entries encode identically, so this test cannot tell a refusal from a lucky pick")
	}

	// the control: either entry alone is answered, so a refusal below is about the repeat rather
	// than about either body
	for _, one := range []struct {
		name  string
		entry Extension
		owner *testMember
	}{{"alice", aliceKeys, alice}, {"bob", bobKeys, bob}} {
		keys, err := LeafKeysOf(&LeafNode{Extensions: []Extension{one.entry}})
		if err != nil {
			t.Fatalf("LeafKeysOf over %s's entry alone: %v", one.name, err)
		}
		if !bytes.Equal(keys.DeviceXwingPub, one.owner.XwingPub) {
			t.Fatalf("LeafKeysOf over %s's entry alone answered somebody else's wrap key", one.name)
		}
	}

	for _, one := range []struct {
		name       string
		extensions []Extension
		at         []int
	}{
		{"alice then bob", []Extension{aliceKeys, bobKeys}, []int{0, 1}},
		{"bob then alice", []Extension{bobKeys, aliceKeys}, []int{0, 1}},
		{"with an unrelated entry between them", []Extension{
			aliceKeys,
			{ExtensionType: ExtensionTypeApplicationId, ExtensionData: []byte("urmessage")},
			bobKeys,
		}, []int{0, 2}},
		{"behind two entries of other types", []Extension{
			{ExtensionType: ExtensionTypeApplicationId, ExtensionData: []byte("urmessage")},
			{ExtensionType: ExtensionTypeRequiredCapabilities, ExtensionData: []byte{0x00, 0x00, 0x00}},
			aliceKeys,
			bobKeys,
		}, []int{2, 3}},
	} {
		keys, err := LeafKeysOf(&LeafNode{Extensions: one.extensions})
		if !errors.Is(err, ErrMalformedExtension) {
			t.Errorf("LeafKeysOf over a leaf carrying urmessage_leaf_keys twice (%s) answered %v, want ErrMalformedExtension",
				one.name, err)
			continue
		}
		if keys != nil {
			t.Errorf("LeafKeysOf over a leaf carrying urmessage_leaf_keys twice (%s) answered the wrap key %x beside its refusal; a commit secret wrapped to a target picked by iteration order goes to one of two devices and the other stops receiving without a parse failure anywhere",
				one.name, keys.DeviceXwingPub)
		}
		// the same reader group_policy_test.go uses, for the same reason: with the repeat
		// refused, the ORDER the two entries are named in is the only observable a reversed walk
		// still changes, and "the walk was reversed and nothing noticed" is what this is for.
		if at := groupPolicyPositionsNamedBy(t, err); !slices.Equal(at, one.at) {
			t.Errorf("LeafKeysOf over a leaf %s named entries %v, want %v; the refusal has to point at the two entries in the order the vector holds them",
				one.name, at, one.at)
		}
	}
}
