// Tests for the path secret ladder of RFC 9420 section 7.4, the node key each rung derives, and
// the private half of the tree one member holds.
//
// Three properties here are not round trip properties, and they are why this file is longer than
// the code it holds.
//
// The ASYMMETRY. A member handed the path secret of a node can derive every node above it and
// none below it, and that is the whole security argument for sealing one secret to a whole
// subtree. A ladder that ran downward derives, matches the tree, and round trips exactly as the
// right one does -- so the negative direction is asserted here as loudly as the positive: a
// member holding rung k must reach every node above and must report that it holds nothing at any
// node below.
//
// The INTERMEDIATE SECRET. node_secret = DeriveSecret(path_secret, "node") sits between the rung
// and the KEM, and handing the path secret straight to DeriveKeyPair produces a key pair that is
// deterministic, that differs at every rung, and that every member agrees on -- every property a
// determinism or distinctness test can see, and the wrong key at every node in the group. Only a
// comparison against a derivation this package did not make separates the two, which is what the
// published TreeKEM corpus is for below, and what
// TestTheNodeKeyPairIsTheKemKeyOfTheNodeSecretAndNotOfThePathSecret states from the RFC's own
// words.
//
// And CONSISTENT's second clause. A state whose path secrets belong to the previous epoch
// derives private keys nobody's public half matches, and the symptom arrives one commit later as
// a decryption failure against a perfectly well formed commit. The clause that catches it is the
// KEY comparison and not the blank-node check in front of it, so there is a case here whose node
// is present and carries the key of a DIFFERENT secret -- without it, a Consistent that checked
// only for a blank node passes every test the shape of this file otherwise suggests.
package mls

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/urnetwork/connect/mls/syntax"
)

// ---------------------------------------------------------------------------
// the ladder
// ---------------------------------------------------------------------------

// TestDerivePathSecretsIsALadder is the plan's own golden: each rung is DeriveSecret of the one
// below it under the label "path", the first rung is the caller's own secret, and the answer is
// one longer than the count asked for because section 8.1 makes the rung past the root the
// commit secret.
func TestDerivePathSecretsIsALadder(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	initial := crypto.Random(crypto.HashSize())
	secrets := DerivePathSecrets(crypto, initial, 3)
	if len(secrets) != 4 {
		t.Fatalf("len = %d, want count+1", len(secrets))
	}
	if !bytes.Equal(secrets[0], initial) {
		t.Fatalf("path_secret[0] is not the initial secret")
	}
	for i := 1; i < len(secrets); i++ {
		want := crypto.DeriveSecret(secrets[i-1], "path")
		if !bytes.Equal(secrets[i], want) {
			t.Fatalf("path_secret[%d] is not DeriveSecret(path_secret[%d], \"path\")", i, i-1)
		}
		if bytes.Equal(secrets[i], secrets[i-1]) {
			t.Fatalf("path_secret[%d] equals its predecessor", i)
		}
	}
}

// TestDerivePathSecretsRunsUpwardAndNeverBackDown is the direction the golden above cannot see.
//
// The golden compares each rung against DeriveSecret of the one BELOW it, which a ladder built
// in either order satisfies as long as the sequence is consistent with itself: reverse the
// answer and rung 0 stops being the initial secret, which the golden does catch -- but a ladder
// that ran the other way while still reporting the initial secret first would not be a sequence
// this package can produce and is not what this test is for.
//
// What is asserted here is the ONE WAY property as a value property: from any rung, climbing
// reaches exactly the rungs above it and never reproduces a rung below it. That is what makes it
// safe to hand a subtree the secret of its own node -- the members under it learn the path from
// there up and nothing about the sender's leaf -- and it is checked over every starting rung
// rather than over one somebody picked.
func TestDerivePathSecretsRunsUpwardAndNeverBackDown(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	full := DerivePathSecrets(crypto, crypto.Random(crypto.HashSize()), 6)
	climbed := 0
	for from := range full {
		above := DerivePathSecrets(crypto, full[from], len(full)-1-from)
		if len(above) != len(full)-from {
			t.Fatalf("climbing from rung %d of %d gave %d rungs, want %d",
				from, len(full), len(above), len(full)-from)
		}
		for j, rung := range above {
			if !bytes.Equal(rung, full[from+j]) {
				t.Fatalf("climbing from rung %d reached a different rung at %d than the whole ladder holds at %d",
					from, j, from+j)
			}
			climbed += 1
			// and the negative: nothing reachable from rung `from` is a rung below it.
			for below := 0; below < from; below++ {
				if bytes.Equal(rung, full[below]) {
					t.Fatalf("climbing from rung %d reproduced rung %d, which is below it; the ladder runs both ways",
						from, below)
				}
			}
		}
	}
	if climbed == 0 {
		t.Fatal("the sweep climbed nothing")
	}
}

// TestTheNodeKeyPairIsTheKemKeyOfTheNodeSecretAndNotOfThePathSecret pins the one construction in
// this file that a self-consistency test cannot see.
//
// RFC 9420 section 7.4 writes node_secret = DeriveSecret(path_secret, "node") and derives the
// node's key pair from THAT. Every observable property a round trip can reach -- determinism,
// one key pair per rung, both members of the group agreeing -- holds just as well for a body
// that handed the path secret straight to the KEM, and the group it produces is internally
// consistent and interoperates with nobody. So the RFC's construction is transcribed here, and
// the three near misses are named and required to differ: the path secret itself, the rung above
// it, and the node secret of the rung above.
func TestTheNodeKeyPairIsTheKemKeyOfTheNodeSecretAndNotOfThePathSecret(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	pathSecret := bytes.Repeat([]byte{0x2b}, crypto.HashSize())
	gotPriv, gotPub, err := DeriveNodeKeyPair(crypto, pathSecret)
	if err != nil {
		t.Fatalf("DeriveNodeKeyPair: %v", err)
	}
	wantPriv, wantPub, err := crypto.DeriveKeyPair(crypto.DeriveSecret(pathSecret, "node"))
	if err != nil {
		t.Fatalf("the RFC's own construction: %v", err)
	}
	if !bytes.Equal(gotPriv, wantPriv) || !bytes.Equal(gotPub, wantPub) {
		t.Fatalf("DeriveNodeKeyPair = (%x, %x), and KEM.DeriveKeyPair(DeriveSecret(path_secret, \"node\")) = (%x, %x)",
			gotPriv, gotPub, wantPriv, wantPub)
	}
	for _, near := range []struct {
		name string
		ikm  []byte
	}{
		{name: "the path secret handed straight to the KEM", ikm: pathSecret},
		{name: "the rung above", ikm: crypto.DeriveSecret(pathSecret, "path")},
		{name: "the node secret of the rung above",
			ikm: crypto.DeriveSecret(crypto.DeriveSecret(pathSecret, "path"), "node")},
	} {
		_, pub, err := crypto.DeriveKeyPair(near.ikm)
		if err != nil {
			t.Fatalf("%s: %v", near.name, err)
		}
		if bytes.Equal(gotPub, pub) {
			t.Fatalf("the node key pair is indistinguishable from %s", near.name)
		}
	}
}

// TestDeriveNodeKeyPairIsDeterministicAndDistinctFromThePathSecret is the plan's golden. It is
// kept for the determinism half -- every member that learns a rung must derive the identical
// node key or nobody decrypts -- and it is NOT what holds the construction: see the test above,
// which is the one a key pair derived from the wrong secret fails.
func TestDeriveNodeKeyPairIsDeterministicAndDistinctFromThePathSecret(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	pathSecret := crypto.Random(crypto.HashSize())
	priv1, pub1, err := DeriveNodeKeyPair(crypto, pathSecret)
	if err != nil {
		t.Fatalf("DeriveNodeKeyPair: %v", err)
	}
	priv2, pub2, err := DeriveNodeKeyPair(crypto, pathSecret)
	if err != nil {
		t.Fatalf("DeriveNodeKeyPair: %v", err)
	}
	if !bytes.Equal(pub1, pub2) || !bytes.Equal(priv1, priv2) {
		t.Fatalf("DeriveNodeKeyPair is not deterministic")
	}
	_, otherPub, err := DeriveNodeKeyPair(crypto, crypto.DeriveSecret(pathSecret, "path"))
	if err != nil {
		t.Fatalf("DeriveNodeKeyPair: %v", err)
	}
	if bytes.Equal(pub1, otherPub) {
		t.Fatalf("two rungs of the ladder derive the same key pair")
	}
}

// ---------------------------------------------------------------------------
// the private state over a real tree
// ---------------------------------------------------------------------------

// installPathSecrets keys every node of a leaf's filtered direct path from one initial secret and
// answers the path together with the rungs it installed.
//
// The tree's public halves are written from the SAME ladder the private state below is built
// from, which is what a commit does: the sender derives the ladder, writes the public key of each
// rung into the tree, and seals each rung to the copath. Nothing here is task 18's UpdatePath --
// there is no encryption and no signature -- it is the minimum shape in which "the derived key
// pair matches the public key already in the tree" is a question that can be asked at all.
func installPathSecrets(t *testing.T, crypto CryptoProvider, tree *RatchetTree,
	sender LeafIndex, initial []byte) ([]NodeIndex, [][]byte) {
	t.Helper()
	path, err := tree.FilteredDirectPath(sender)
	if err != nil {
		t.Fatalf("FilteredDirectPath(%d): %v", sender, err)
	}
	if len(path) < 2 {
		t.Fatalf("leaf %d has a filtered direct path of %v, and a path with fewer than two nodes cannot separate above from below",
			sender, path)
	}
	secrets := DerivePathSecrets(crypto, initial, len(path))
	for at, node := range path {
		_, pub, err := DeriveNodeKeyPair(crypto, secrets[at])
		if err != nil {
			t.Fatalf("DeriveNodeKeyPair for node %d: %v", node, err)
		}
		if err := tree.SetParent(node, &ParentNode{EncryptionKey: pub}); err != nil {
			t.Fatalf("SetParent(%d): %v", node, err)
		}
	}
	return path, secrets
}

// TestAPathSecretReachesEveryNodeAboveItAndNoneBelow is the asymmetry, over a real tree.
//
// A member that learns the rung at path[k] climbs from there and must end up holding exactly the
// nodes at and above k. Three things are asserted at once, and each catches something the others
// do not:
//
//   - every node from k up is available, and the key it derives is the one the TREE carries at
//     that node. A private state that derives keys nobody's public half matches is a member who
//     cannot decrypt anything, and without this it is caught at commit time in task 22 rather
//     than here.
//   - every node below k reports that no key is available. This is the direction that matters:
//     a state that answered a key for a node below would be a member reading the part of the path
//     it was never sealed, and every one of those keys derives cleanly and looks exactly right.
//   - Consistent agrees, which is the same statement made through the entry point the commit path
//     actually calls.
//
// SWEPT rather than sampled, and the negative direction is why. One tree, one sender and one
// receiver made three observations of the direction that carries the whole security argument --
// a member holding rung k must be refused every node below k -- and three observations of a
// refusal is three chances for a body that refuses for the wrong reason to look right. The sweep
// below runs every width whose filtered path is long enough to have an above and a below at all,
// and every member of each as the sender, with the receiver derived from the tree rather than
// chosen.
//
// A sender whose filtered path holds fewer than two nodes is SKIPPED rather than excluded by a
// width, and the skips are counted. Which senders those are is a property of the filter -- in a
// three member tree leaf 2's step past the blank leaf 3 is filtered away and its path is one node
// -- so a width chosen to avoid them is a width that stops avoiding them the day the filter
// changes. There is no node below a single rung, and a run that was all one node paths would
// report a zero in the negative direction as a pass, which is what the counters below refuse.
func TestAPathSecretReachesEveryNodeAboveItAndNoneBelow(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	above, below, senders, ladders, tooShort := 0, 0, 0, 0, 0
	for width := uint32(2); width <= 8; width += 1 {
		for at := uint32(0); at < width; at += 1 {
			tree, members := newTestTree(t, crypto, width)
			sender := members[at].LeafIndex
			where := fmt.Sprintf("%d members, sender %d", width, sender)
			filtered, err := tree.FilteredDirectPath(sender)
			if err != nil {
				t.Fatalf("%s: FilteredDirectPath: %v", where, err)
			}
			if len(filtered) < 2 {
				tooShort += 1
				continue
			}
			path, secrets := installPathSecrets(t, crypto, tree, sender,
				crypto.Random(crypto.HashSize()))
			senders += 1
			// the member that receives the rungs is a different one, derived from the tree
			// rather than chosen; its own node index is none of the path nodes because every
			// path node is a parent.
			receiver := members[(at+1)%width]
			for from := range path {
				ladders += 1
				state := NewTreeKEMPrivate(receiver.LeafIndex, receiver.EncryptionPriv)
				for step, rung := range DerivePathSecrets(crypto, secrets[from], len(path)-1-from) {
					state.PathSecrets[path[from+step]] = rung
				}
				if err := state.Consistent(crypto, tree); err != nil {
					t.Fatalf("%s: a state built from the rung at path[%d] = node %d disagrees with the tree it was derived into: %v",
						where, from, path[from], err)
				}
				for step, node := range path {
					key, held, err := state.NodePrivateKey(crypto, node)
					if err != nil {
						t.Fatalf("%s: NodePrivateKey(%d): %v", where, node, err)
					}
					if step < from {
						if held {
							t.Fatalf("%s: a member holding only the rung at path[%d] answered a private key for node %d, which is BELOW it; the ladder runs downward",
								where, from, node)
						}
						below += 1
						continue
					}
					if !held {
						t.Fatalf("%s: a member holding the rung at path[%d] cannot derive node %d, which is at or above it",
							where, from, node)
					}
					_, pub, err := DeriveNodeKeyPair(crypto, state.PathSecrets[node])
					if err != nil {
						t.Fatalf("%s: DeriveNodeKeyPair at node %d: %v", where, node, err)
					}
					parent := tree.ParentAt(node)
					if parent == nil {
						t.Fatalf("%s: node %d of the path is blank in the tree the rungs were installed into", where, node)
					}
					if !bytes.Equal(pub, parent.EncryptionKey) {
						t.Fatalf("%s: the key derived at node %d is %x and the tree carries %x", where, node, pub, parent.EncryptionKey)
					}
					if len(key) == 0 {
						t.Fatalf("%s: NodePrivateKey(%d) answered an empty key", where, node)
					}
					above += 1
				}
			}
		}
	}
	if above == 0 || below == 0 || senders == 0 {
		t.Fatalf("the sweep ran %d senders over %d ladders, reached %d nodes at or above the rung it started from and %d below it; with either direction at zero this test holds one of the two it states",
			senders, ladders, above, below)
	}
	t.Logf("%d senders over widths 2 to 8 and %d skipped for a filtered path of fewer than two nodes, %d ladders, %d nodes derivable from a rung at or below them, %d refused from a rung above them",
		senders, tooShort, ladders, above, below)
}

// TestConsistentRefusesAPathSecretThatDerivesTheWrongKey is the clause the plan's own test cannot
// see.
//
// That test drives Consistent against a BLANK node and then against the right key, so a body
// whose only check was "is this node present" passes it -- and that body accepts every private
// state a previous epoch left behind, which is exactly the drift Consistent exists to catch. The
// table below separates the two: the node is present in every row, and only its contents move.
func TestConsistentRefusesAPathSecretThatDerivesTheWrongKey(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	build := func(t *testing.T) (*RatchetTree, *TreeKEMPrivate, []NodeIndex, [][]byte) {
		t.Helper()
		tree, members := newTestTree(t, crypto, 8)
		path, secrets := installPathSecrets(t, crypto, tree, members[0].LeafIndex,
			bytes.Repeat([]byte{0x3c}, crypto.HashSize()))
		state := NewTreeKEMPrivate(members[5].LeafIndex, members[5].EncryptionPriv)
		for at, node := range path {
			state.PathSecrets[node] = secrets[at]
		}
		return tree, state, path, secrets
	}
	// the control: the untouched pair agrees, or every refusal below is one that was already
	// there.
	tree, state, _, _ := build(t)
	if err := state.Consistent(crypto, tree); err != nil {
		t.Fatalf("the fixture does not agree with itself, so nothing below is about the case it names: %v", err)
	}
	for _, row := range []struct {
		name   string
		breaks func(t *testing.T, tree *RatchetTree, state *TreeKEMPrivate, path []NodeIndex, secrets [][]byte)
	}{
		{name: "a rung that derives some other key", breaks: func(t *testing.T, tree *RatchetTree, state *TreeKEMPrivate, path []NodeIndex, secrets [][]byte) {
			state.PathSecrets[path[0]] = crypto.DeriveSecret(secrets[0], "path")
		}},
		{name: "a rung flipped in one octet", breaks: func(t *testing.T, tree *RatchetTree, state *TreeKEMPrivate, path []NodeIndex, secrets [][]byte) {
			flipped := bytes.Clone(secrets[len(path)-1])
			flipped[0] ^= 0x01
			state.PathSecrets[path[len(path)-1]] = flipped
		}},
		{name: "the tree re-keyed under the state", breaks: func(t *testing.T, tree *RatchetTree, state *TreeKEMPrivate, path []NodeIndex, secrets [][]byte) {
			_, pub, err := DeriveNodeKeyPair(crypto, bytes.Repeat([]byte{0x4d}, crypto.HashSize()))
			if err != nil {
				t.Fatalf("DeriveNodeKeyPair: %v", err)
			}
			if err := tree.SetParent(path[1], &ParentNode{EncryptionKey: pub}); err != nil {
				t.Fatalf("SetParent(%d): %v", path[1], err)
			}
		}},
		{name: "a node the tree has blanked", breaks: func(t *testing.T, tree *RatchetTree, state *TreeKEMPrivate, path []NodeIndex, secrets [][]byte) {
			if err := tree.Blank(path[0]); err != nil {
				t.Fatalf("Blank(%d): %v", path[0], err)
			}
		}},
		{name: "a state whose leaf the tree does not have", breaks: func(t *testing.T, tree *RatchetTree, state *TreeKEMPrivate, path []NodeIndex, secrets [][]byte) {
			if err := tree.Blank(state.LeafIndex.NodeIndex()); err != nil {
				t.Fatalf("Blank(%d): %v", state.LeafIndex.NodeIndex(), err)
			}
		}},
		{name: "a rung held at a node that is not on the path at all", breaks: func(t *testing.T, tree *RatchetTree, state *TreeKEMPrivate, path []NodeIndex, secrets [][]byte) {
			// node 5 is a parent of this tree that no rung was installed at, so it is
			// blank; holding a secret for it is a state from a tree of another shape.
			state.PathSecrets[NodeIndex(5)] = secrets[0]
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			tree, state, path, secrets := build(t)
			row.breaks(t, tree, state, path, secrets)
			if err := state.Consistent(crypto, tree); !errors.Is(err, ErrPathSecretMismatch) {
				t.Fatalf("Consistent over %s gave err = %v, want ErrPathSecretMismatch", row.name, err)
			}
		})
	}
	// and a state holding NO rungs at all over a tree that has its leaf is consistent: the
	// refusals above must be about the secrets and not about having any.
	empty := NewTreeKEMPrivate(state.LeafIndex, state.EncryptionPriv)
	if err := empty.Consistent(crypto, tree); err != nil {
		t.Fatalf("a member holding no path secret at all was refused: %v", err)
	}
}

// TestTreeKEMPrivateNodePrivateKeyAndConsistency is the plan's golden for the container.
func TestTreeKEMPrivateNodePrivateKeyAndConsistency(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, members := newTestTree(t, crypto, 4)
	priv := NewTreeKEMPrivate(members[0].LeafIndex, members[0].EncryptionPriv)

	// the leaf's own key is always available.
	got, ok, err := priv.NodePrivateKey(crypto, members[0].LeafIndex.NodeIndex())
	if err != nil || !ok {
		t.Fatalf("NodePrivateKey(own leaf) = (_, %v, %v)", ok, err)
	}
	if !bytes.Equal(got, members[0].EncryptionPriv) {
		t.Fatalf("NodePrivateKey(own leaf) is not the leaf private key")
	}
	if _, ok, _ := priv.NodePrivateKey(crypto, NodeIndex(1)); ok {
		t.Fatalf("NodePrivateKey(1) is available with no path secret held")
	}

	// holding a path secret for node 1 makes node 1's key available, and Consistent
	// agrees with the tree only when the tree carries the derived public key.
	pathSecret := crypto.Random(crypto.HashSize())
	_, pub, err := DeriveNodeKeyPair(crypto, pathSecret)
	if err != nil {
		t.Fatalf("DeriveNodeKeyPair: %v", err)
	}
	priv.PathSecrets[NodeIndex(1)] = pathSecret
	if err := priv.Consistent(crypto, tree); !errors.Is(err, ErrPathSecretMismatch) {
		t.Fatalf("Consistent with a blank node 1 err = %v, want ErrPathSecretMismatch", err)
	}
	if err := tree.SetParent(NodeIndex(1), &ParentNode{EncryptionKey: pub}); err != nil {
		t.Fatalf("SetParent: %v", err)
	}
	if err := priv.Consistent(crypto, tree); err != nil {
		t.Fatalf("Consistent: %v", err)
	}
	derived, ok, err := priv.NodePrivateKey(crypto, NodeIndex(1))
	if err != nil || !ok {
		t.Fatalf("NodePrivateKey(1) with a path secret held = (_, %v, %v)", ok, err)
	}
	wantPriv, _, err := DeriveNodeKeyPair(crypto, pathSecret)
	if err != nil {
		t.Fatalf("DeriveNodeKeyPair: %v", err)
	}
	if !bytes.Equal(derived, wantPriv) {
		t.Fatalf("NodePrivateKey(1) is not the private half the rung derives")
	}
	clone := priv.Clone()
	clone.PathSecrets[NodeIndex(1)][0] ^= 0xFF
	if bytes.Equal(clone.PathSecrets[NodeIndex(1)], priv.PathSecrets[NodeIndex(1)]) {
		t.Fatalf("Clone shares path secret backing arrays")
	}
}

// TestTheStateSharesNoStorageWithItsCallerOrItsClone is the aliasing half, over every door into
// and OUT OF the state rather than the one the plan's golden happens to open.
//
// A rejected commit is the case for the entry doors. The caller works on a clone, the commit
// loses a race and is dropped, and a clone that shared one array with its parent has already
// written through to the state the group is still running on -- with every key still deriving and
// nothing to point at. The plan's golden holds the path secret map; the leaf key and the map's
// own membership are the other two, and neither is visible from it.
//
// The EXIT door is the last section and it was the door this test's name promised and did not
// cover. NodePrivateKey has two arms: the derived arm hands back what DeriveKeyPair just made
// and shares nothing, and the own-leaf arm used to hand back the state's live field. One
// function answering two ownership contracts is a caller that cannot have a policy, and the
// policy this package ships for this material is zeroizeSecret -- so "erase the private key when
// the decrypt is done", which is right for every other answer the function gives, erased the
// member's own leaf key and left it unable to decrypt again. Measured, before the fix: zeroizing
// the own-leaf answer wiped TreeKEMPrivate.EncryptionPriv, 0x11 x32 to zeros, and no test saw it.
//
// The exit door is swept rather than sampled. The doors are derived from the tree -- the member's
// own leaf node plus every node it holds a rung for -- so an arm added later is swept the day it
// is added, and both arms are covered by the same assertion rather than by one that happens to
// open the right one.
func TestTheStateSharesNoStorageWithItsCallerOrItsClone(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	callersLeafKey := bytes.Repeat([]byte{0x11}, 32)
	callersRung := bytes.Repeat([]byte{0x22}, crypto.HashSize())
	state := NewTreeKEMPrivate(LeafIndex(3), HpkePrivateKey(callersLeafKey))
	state.PathSecrets[NodeIndex(7)] = bytes.Clone(callersRung)

	// the constructor's own copy, which is what keeps a member decrypting after its caller
	// reuses the buffer the key arrived in.
	callersLeafKey[0] ^= 0xFF
	if bytes.Equal(state.EncryptionPriv, callersLeafKey) {
		t.Fatal("NewTreeKEMPrivate kept the caller's leaf key array")
	}
	callersLeafKey[0] ^= 0xFF

	clone := state.Clone()
	clone.EncryptionPriv[0] ^= 0xFF
	if bytes.Equal(clone.EncryptionPriv, state.EncryptionPriv) {
		t.Fatal("Clone shares the leaf private key array")
	}
	clone.EncryptionPriv[0] ^= 0xFF
	clone.PathSecrets[NodeIndex(7)][0] ^= 0xFF
	if bytes.Equal(clone.PathSecrets[NodeIndex(7)], state.PathSecrets[NodeIndex(7)]) {
		t.Fatal("Clone shares path secret backing arrays")
	}
	// and the MAP itself, which is the half neither byte comparison reaches: a clone sharing
	// the map holds every rung the abandoned commit added.
	clone.PathSecrets[NodeIndex(9)] = bytes.Repeat([]byte{0x33}, crypto.HashSize())
	if _, leaked := state.PathSecrets[NodeIndex(9)]; leaked {
		t.Fatal("a rung added to the clone appeared in the state it was cloned from")
	}
	if state.LeafIndex != clone.LeafIndex {
		t.Fatalf("Clone answered leaf %d for a state at leaf %d", clone.LeafIndex, state.LeafIndex)
	}

	// and the exit door, over every node this member can answer for.
	tree, members := newTestTree(t, crypto, 8)
	path, secrets := installPathSecrets(t, crypto, tree, members[0].LeafIndex,
		crypto.Random(crypto.HashSize()))
	holder := NewTreeKEMPrivate(members[5].LeafIndex, members[5].EncryptionPriv)
	for at, node := range path {
		holder.PathSecrets[node] = secrets[at]
	}
	ownLeafKey := bytes.Clone(holder.EncryptionPriv)
	doors := append([]NodeIndex{holder.LeafIndex.NodeIndex()}, path...)
	for _, x := range doors {
		answer, held, err := holder.NodePrivateKey(crypto, x)
		if err != nil || !held {
			t.Fatalf("NodePrivateKey(%d) = (_, %v, %v), and every door in this sweep is one the member holds", x, held, err)
		}
		kept := bytes.Clone(answer)
		if len(kept) == 0 {
			t.Fatalf("NodePrivateKey(%d) answered an empty key, so erasing it observes nothing", x)
		}
		// the policy task 22 is entitled to have: erase the private key once it is spent.
		zeroizeSecret(answer)
		again, held, err := holder.NodePrivateKey(crypto, x)
		if err != nil || !held {
			t.Fatalf("NodePrivateKey(%d) after erasing its own previous answer = (_, %v, %v)", x, held, err)
		}
		if !bytes.Equal(again, kept) {
			t.Fatalf("erasing the key NodePrivateKey answered for node %d changed what it answers next time, %x then %x: the answer aliases the state",
				x, kept, again)
		}
	}
	// the same statement made against the field directly, because the leaf key is the one the
	// re-ask above would also have been able to answer from a wiped array.
	if !bytes.Equal(holder.EncryptionPriv, ownLeafKey) {
		t.Fatalf("the state's own leaf key is %x after the sweep and was %x: an answer this member handed out aliased it",
			holder.EncryptionPriv, ownLeafKey)
	}
	if err := holder.Consistent(crypto, tree); err != nil {
		t.Fatalf("the state stopped agreeing with its tree after its own answers were erased: %v", err)
	}
	t.Logf("%d doors out of the state swept, 1 own leaf and %d derived rungs", len(doors), len(path))
}

// ---------------------------------------------------------------------------
// the published corpus
// ---------------------------------------------------------------------------

// treekemPrivateVector is the part of one entry of treekem.json this file reads: the ratchet
// tree of an epoch and the private state each member of it holds.
//
// The update_paths half is deliberately not read here. Verifying one is tasks 18 through 22's
// and registering family 11 is theirs to do; what this file can answer for is the part of that
// corpus its own two constructions decide, which is whether a published path secret derives the
// public key the published tree carries at its node.
type treekemPrivateVector struct {
	CipherSuite   uint16                     `json:"cipher_suite"`
	RatchetTree   string                     `json:"ratchet_tree"`
	LeavesPrivate []treekemLeafPrivateVector `json:"leaves_private"`
}

// signature_priv is read even though nothing in THIS file signs anything: family 11's
// generate direction in tree_kat_test.go re-commits from every published member and has to
// sign the commit leaf with the key that member's own leaf announces. It is here rather than
// in a second struct over there for this file's own stated reason -- two declarations of one
// corpus row is how two of them end up disagreeing about which json key a field lives at.
type treekemLeafPrivateVector struct {
	Index          uint32                    `json:"index"`
	EncryptionPriv string                    `json:"encryption_priv"`
	SignaturePriv  string                    `json:"signature_priv"`
	PathSecrets    []treekemPathSecretVector `json:"path_secrets"`
}

type treekemPathSecretVector struct {
	Node       uint32 `json:"node"`
	PathSecret string `json:"path_secret"`
}

// Transcriptions of what testdata/vectors/treekem.json holds at the pinned mlswg commit. A
// decoder that quietly stopped early, a filter that matched nothing, and a sweep that read a
// field that is not there all report exactly what a complete run reports without these.
const treekemEntryCount = 77
const treekemLeafPrivateCount = 124
const treekemPathSecretCount = 310

// TestEveryPublishedPathSecretDerivesTheNodeKeyItsRatchetTreeCarries is the only oracle in this
// file that is independent of this repository.
//
// Everything else here compares two derivations this package makes, or compares one against the
// RFC transcribed by the same reader who wrote the code. Neither separates a node key derived
// from the wrong secret: the whole group agrees with itself, every rung derives, every state is
// consistent, and the implementation interoperates with nobody. The working group's own
// implementations produced these path secrets and these trees, so a node_secret step that was
// skipped, labelled differently, or applied twice fails here and nowhere else in this package.
//
// The negative control is what makes the green run mean something. Every published case agrees
// with this implementation, so a Consistent that checked everything and one that returned nil
// produce identical runs over the corpus; the only way to separate them is to hand it an answer
// that is wrong on purpose and require the refusal.
func TestEveryPublishedPathSecretDerivesTheNodeKeyItsRatchetTreeCarries(t *testing.T) {
	entries := LoadVectorFile(t, "treekem.json")
	if len(entries) != treekemEntryCount {
		t.Fatalf("treekem.json holds %d entries, want %d", len(entries), treekemEntryCount)
	}
	matched, declined, leaves, secrets, deep := 0, 0, 0, 0, 0
	for at, raw := range entries {
		var vector treekemPrivateVector
		if err := json.Unmarshal(raw, &vector); err != nil {
			t.Fatalf("entry %d: %v", at, err)
		}
		suite, ok := implementedSuite(vector.CipherSuite)
		if !ok {
			declined += 1
			continue
		}
		matched += 1
		crypto := mustProvider(t, suite)
		tree, err := UnmarshalRatchetTree(MustHex(t, vector.RatchetTree))
		if err != nil {
			t.Fatalf("entry %d: UnmarshalRatchetTree: %v", at, err)
		}
		for _, published := range vector.LeavesPrivate {
			leaves += 1
			state := NewTreeKEMPrivate(LeafIndex(published.Index),
				HpkePrivateKey(MustHex(t, published.EncryptionPriv)))
			for _, rung := range published.PathSecrets {
				state.PathSecrets[NodeIndex(rung.Node)] = MustHex(t, rung.PathSecret)
				secrets += 1
			}
			if len(published.PathSecrets) > 1 {
				deep += 1
			}
			if err := state.Consistent(crypto, tree); err != nil {
				t.Errorf("entry %d, leaf %d: the published path secrets do not derive the published tree's node keys: %v",
					at, published.Index, err)
				continue
			}
			// the control, per case rather than once: one octet of one published rung
			// flipped, and the refusal is required. A comparator that answered nil
			// unconditionally satisfies every line above it.
			if len(published.PathSecrets) == 0 {
				continue
			}
			wrong := state.Clone()
			node := NodeIndex(published.PathSecrets[0].Node)
			wrong.PathSecrets[node][0] ^= 0x01
			if err := wrong.Consistent(crypto, tree); !errors.Is(err, ErrPathSecretMismatch) {
				t.Fatalf("entry %d, leaf %d: one octet of the rung at node %d flipped gave err = %v, want ErrPathSecretMismatch",
					at, published.Index, node, err)
			}
		}
	}
	if matched+declined != len(entries) {
		t.Fatalf("%d entries matched and %d were declined, and the file holds %d",
			matched, declined, len(entries))
	}
	if matched == 0 || leaves != treekemLeafPrivateCount || secrets != treekemPathSecretCount {
		t.Fatalf("the run read %d entries at a registered suite, %d private leaf states and %d path secrets; want %d states and %d secrets",
			matched, leaves, secrets, treekemLeafPrivateCount, treekemPathSecretCount)
	}
	if deep == 0 {
		t.Fatal("no published private state held more than one path secret, so the loop inside Consistent ran at most once in every case")
	}
	t.Logf("%d of %d entries at a registered suite: %d private leaf states, %d path secrets, %d of the states holding more than one",
		matched, len(entries), leaves, secrets, deep)
}

// ---------------------------------------------------------------------------
// task 18: the secrets, keys and parent hashes of an UpdatePath
// ---------------------------------------------------------------------------

// TestCreateUpdatePathSecretsInstallsAChainThatVerifies is the plan's own sweep: the shapes
// agree, every public key is the one its path secret derives, the commit secret is the rung past
// the root, the leaf is a re-signed commit leaf, and the tree the call leaves behind passes
// section 7.9.2.
func TestCreateUpdatePathSecretsInstallsAChainThatVerifies(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	for _, n := range []uint32{2, 3, 4, 7, 8} {
		tree, members := newTestTree(t, crypto, n)
		plan, err := tree.CreateUpdatePathSecrets(crypto, members[0].LeafIndex,
			members[0].SignaturePriv, testGroupId())
		if err != nil {
			t.Fatalf("n=%d CreateUpdatePathSecrets: %v", n, err)
		}
		if len(plan.Path) == 0 {
			t.Fatalf("n=%d the filtered direct path is empty in a group of more than one", n)
		}
		if len(plan.PathSecrets) != len(plan.Path) {
			t.Fatalf("n=%d %d path secrets for %d nodes", n, len(plan.PathSecrets), len(plan.Path))
		}
		if len(plan.PublicKeys) != len(plan.Path) {
			t.Fatalf("n=%d %d public keys for %d nodes", n, len(plan.PublicKeys), len(plan.Path))
		}
		if len(plan.CommitSecret) != crypto.HashSize() {
			t.Fatalf("n=%d commit secret length = %d", n, len(plan.CommitSecret))
		}
		want := crypto.DeriveSecret(plan.PathSecrets[len(plan.PathSecrets)-1], "path")
		if !bytes.Equal(plan.CommitSecret, want) {
			t.Fatalf("n=%d commit secret is not the rung past the root", n)
		}
		for i, x := range plan.Path {
			parent := tree.ParentAt(x)
			if parent == nil {
				t.Fatalf("n=%d node %d was not installed", n, x)
			}
			if !bytes.Equal(parent.EncryptionKey, plan.PublicKeys[i]) {
				t.Fatalf("n=%d node %d carries a different key from the plan", n, x)
			}
			if len(parent.UnmergedLeaves) != 0 {
				t.Fatalf("n=%d node %d kept unmerged leaves across a fresh path", n, x)
			}
			_, derived, err := DeriveNodeKeyPair(crypto, plan.PathSecrets[i])
			if err != nil {
				t.Fatalf("n=%d DeriveNodeKeyPair: %v", n, err)
			}
			if !bytes.Equal(derived, plan.PublicKeys[i]) {
				t.Fatalf("n=%d node %d public key is not derived from its path secret", n, x)
			}
		}
		leaf := tree.Leaf(members[0].LeafIndex)
		if leaf.LeafNodeSource != LeafNodeSourceCommit {
			t.Fatalf("n=%d leaf source = %d, want commit", n, leaf.LeafNodeSource)
		}
		if len(leaf.ParentHash) != crypto.HashSize() {
			t.Fatalf("n=%d leaf parent hash length = %d", n, len(leaf.ParentHash))
		}
		if err := leaf.VerifySignature(crypto, testGroupId(), members[0].LeafIndex); err != nil {
			t.Fatalf("n=%d the re-signed leaf does not verify: %v", n, err)
		}
		if err := tree.VerifyParentHashes(crypto); err != nil {
			t.Fatalf("n=%d VerifyParentHashes after a fresh path: %v", n, err)
		}
	}
}

// TestCreateUpdatePathSecretsGivesTheLeafAnIndependentKey is the plan's second: the leaf key is
// rotated, it is NOT derivable from path_secret[0], and the private state the call answers is
// consistent with the tree it left behind.
//
// Its name promises more than its body observes, and the two halves it does NOT observe are
// named here rather than left for the next reader to discover the way this one was. "Independent"
// has two more meanings past "not derived from path_secret[0]", and a constant satisfies both of
// the assertions below: a leaf key pair seeded from thirty two zero octets is not the old key and
// is not DeriveNodeKeyPair(path_secret[0]), so it passes here while being the same leaf private
// key in every group in the world. And the private key this checks is only checked for being
// non-empty, so a state whose EncryptionPriv came from a second, independent key pair -- one that
// cannot open anything sealed to the leaf key the tree publishes -- passes too. Both were applied
// and the whole package stayed green.
//
// TestTheUpdatePathDrawsEveryOctetStringItPublishesFromFreshEntropy holds the first and
// TestThePrivateStateOpensEverythingSealedToTheKeysThisCommitInstalls holds the second. What is
// left here is worth keeping: the rotation and the one derivation the RFC forbids are the two
// things a reader looks for under this name, and they are stated where they are easiest to read.
func TestCreateUpdatePathSecretsGivesTheLeafAnIndependentKey(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, members := newTestTree(t, crypto, 4)
	before := cloneBytes(tree.Leaf(members[0].LeafIndex).EncryptionKey)
	plan, err := tree.CreateUpdatePathSecrets(crypto, members[0].LeafIndex,
		members[0].SignaturePriv, testGroupId())
	if err != nil {
		t.Fatalf("CreateUpdatePathSecrets: %v", err)
	}
	after := tree.Leaf(members[0].LeafIndex).EncryptionKey
	if bytes.Equal(before, after) {
		t.Fatalf("the leaf encryption key was not rotated")
	}
	// the leaf key must NOT be derivable from path_secret[0]: everyone who decrypts
	// that secret would otherwise hold the sender's leaf private key.
	_, fromPathSecret, err := DeriveNodeKeyPair(crypto, plan.PathSecrets[0])
	if err != nil {
		t.Fatalf("DeriveNodeKeyPair: %v", err)
	}
	if bytes.Equal(after, fromPathSecret) {
		t.Fatalf("the leaf key is derived from path_secret[0]")
	}
	if len(plan.Private.EncryptionPriv) == 0 {
		t.Fatalf("the plan's private state carries no leaf private key")
	}
	if plan.Private.LeafIndex != members[0].LeafIndex {
		t.Fatalf("private state leaf index = %d", plan.Private.LeafIndex)
	}
	for i, x := range plan.Path {
		if !bytes.Equal(plan.Private.PathSecrets[x], plan.PathSecrets[i]) {
			t.Fatalf("private state is missing the path secret for node %d", x)
		}
	}
	if err := plan.Private.Consistent(crypto, tree); err != nil {
		t.Fatalf("Consistent: %v", err)
	}
}

// TestCreateUpdatePathSecretsInASingleMemberGroup is the plan's third: a lone member's filtered
// direct path is empty, so there is no node to claim and the commit leaf's parent_hash field is
// the zero-length octet string rather than a digest of nothing.
func TestCreateUpdatePathSecretsInASingleMemberGroup(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, members := newTestTree(t, crypto, 1)
	plan, err := tree.CreateUpdatePathSecrets(crypto, members[0].LeafIndex,
		members[0].SignaturePriv, testGroupId())
	if err != nil {
		t.Fatalf("CreateUpdatePathSecrets: %v", err)
	}
	if len(plan.Path) != 0 {
		t.Fatalf("path = %v, want empty", plan.Path)
	}
	if len(plan.CommitSecret) != crypto.HashSize() {
		t.Fatalf("commit secret length = %d", len(plan.CommitSecret))
	}
	leaf := tree.Leaf(members[0].LeafIndex)
	if leaf.LeafNodeSource != LeafNodeSourceCommit || len(leaf.ParentHash) != 0 {
		t.Fatalf("a lone member's leaf must be a commit leaf with a zero-length parent hash, got source %d and %d bytes",
			leaf.LeafNodeSource, len(leaf.ParentHash))
	}
}

// treeHashOf is the section 7.8 hash of a whole tree, taken through the provider.
func treeHashOf(t *testing.T, crypto CryptoProvider, tree *RatchetTree) []byte {
	t.Helper()
	hash, err := tree.TreeHash(crypto)
	if err != nil {
		t.Fatalf("TreeHash: %v", err)
	}
	return hash
}

// testGroupContextOver is the serialized GroupContext of an epoch whose tree hashes to this,
// which is the shape of the HPKE info task 20 seals every path secret under.
//
// Everything but the tree hash is fixed, so two contexts built by this helper differ only where
// the trees they were taken over differ. That is the whole instrument: the ordering defect this
// file is about is invisible unless the two contexts are distinguishable, and a helper that
// varied an epoch number as well would make them differ for a reason that is not the tree's.
func testGroupContextOver(t *testing.T, treeHash []byte) []byte {
	t.Helper()
	context := &GroupContext{
		Version:                 ProtocolVersionMls10,
		CipherSuite:             CipherSuiteX25519ChaCha20Sha256Ed25519,
		GroupId:                 testGroupId(),
		Epoch:                   7,
		TreeHash:                treeHash,
		ConfirmedTranscriptHash: bytes.Repeat([]byte{0x77}, 32),
	}
	encoded, err := syntax.Marshal(context)
	if err != nil {
		t.Fatalf("Marshal(GroupContext): %v", err)
	}
	return encoded
}

// aFreshEncryptionKeyOtherThan is a well formed HPKE public key that is not the one handed in.
func aFreshEncryptionKeyOtherThan(t *testing.T, crypto CryptoProvider, key HpkePublicKey) HpkePublicKey {
	t.Helper()
	for attempt := 0; attempt < 8; attempt += 1 {
		_, other, err := crypto.DeriveKeyPair(crypto.Random(crypto.HashSize()))
		if err != nil {
			t.Fatalf("DeriveKeyPair: %v", err)
		}
		if !bytes.Equal(other, key) {
			return other
		}
	}
	t.Fatalf("eight draws from the provider all answered the key they were meant to differ from")
	return nil
}

// TestCreateUpdatePathSecretsLeavesTheTreeAtTheEpochItOpens is the ordering constraint the
// architecture note names, asserted at the boundary task 18 owns.
//
// UpdatePath generation is split in two because the HPKE encryption context task 20 seals each
// path secret under is the serialized GroupContext of the epoch the commit OPENS, and that
// context's tree_hash is only computable once the path's public keys are in the tree. So what
// this half owes its caller is a tree that has already moved: after this call, TreeHash is the
// new epoch's tree hash and the GroupContext built from it is the one every peer will decrypt
// against.
//
// An implementation that left the caller holding the previous epoch's tree -- one that worked on
// a clone, or that answered the plan before installing what it had computed -- produces
// ciphertexts every peer rejects, and it is invisible to any test where the two contexts are not
// distinguished. So both are built here and compared, and the THIRD tree is built too: the
// half-mutated one, direct path blanked and nothing put back, which is what a caller would hash
// if the install had been left to it.
//
// The last sweep is what says the keys are actually inside that hash rather than merely
// alongside it. Its positions come from the plan's own path rather than being picked, because a
// sampled node is a node the defect can be at.
func TestCreateUpdatePathSecretsLeavesTheTreeAtTheEpochItOpens(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	tree, members := newTestTree(t, crypto, 8)
	sender := members[3].LeafIndex

	previous := tree.Clone()
	// the tree mid-operation: the sender's own direct path blanked and nothing installed yet
	halfway := tree.Clone()
	if err := halfway.BlankDirectPath(sender); err != nil {
		t.Fatalf("BlankDirectPath: %v", err)
	}

	plan, err := tree.CreateUpdatePathSecrets(crypto, sender, members[3].SignaturePriv, testGroupId())
	if err != nil {
		t.Fatalf("CreateUpdatePathSecrets: %v", err)
	}
	opened := treeHashOf(t, crypto, tree)
	if stale := treeHashOf(t, crypto, previous); bytes.Equal(opened, stale) {
		t.Fatalf("the tree hash after the call is the one it had before it, so the caller would build the new epoch's GroupContext out of the previous epoch's tree")
	}
	if mid := treeHashOf(t, crypto, halfway); bytes.Equal(opened, mid) {
		t.Fatalf("the tree hash after the call is the hash of the blanked path with nothing installed, so the public keys this call computed are not in the tree it left behind")
	}
	// the two contexts task 20 has to tell apart, spelled out
	if bytes.Equal(testGroupContextOver(t, opened), testGroupContextOver(t, treeHashOf(t, crypto, previous))) {
		t.Fatalf("the GroupContext over the new tree and the one over the old tree serialize alike, so this test cannot see which of them an encryption used")
	}

	// every public key of the path is inside the hash the caller binds the epoch to
	swept, hashed := 0, 0
	for i, x := range plan.Path {
		held := tree.ParentAt(x)
		if held == nil {
			t.Fatalf("node %d was not installed", x)
		}
		swapped := tree.Clone()
		if err := swapped.SetParent(x, &ParentNode{
			EncryptionKey: aFreshEncryptionKeyOtherThan(t, crypto, plan.PublicKeys[i]),
			ParentHash:    cloneBytes(held.ParentHash),
		}); err != nil {
			t.Fatalf("SetParent(%d): %v", x, err)
		}
		if bytes.Equal(treeHashOf(t, crypto, swapped), opened) {
			t.Errorf("node %d's public key is not inside the tree hash the caller binds this epoch to", x)
		}
		swept += 1
		// and so is the parent hash it carries, wherever there is one to move. The node at the
		// top of the path carries the zero-length octet string, which is a value with no byte
		// to flip rather than an exemption, so it is skipped and counted.
		if len(held.ParentHash) == 0 {
			continue
		}
		rehung := tree.Clone()
		field := cloneBytes(held.ParentHash)
		field[0] ^= 0xff
		if err := rehung.SetParent(x, &ParentNode{
			EncryptionKey: cloneBytes(held.EncryptionKey),
			ParentHash:    field,
		}); err != nil {
			t.Fatalf("SetParent(%d): %v", x, err)
		}
		if bytes.Equal(treeHashOf(t, crypto, rehung), opened) {
			t.Errorf("node %d's parent hash is not inside the tree hash the caller binds this epoch to", x)
		}
		hashed += 1
	}
	// and the re-signed leaf, which is the other thing this call installed
	releafed := tree.Clone()
	leaf := tree.Leaf(sender).Clone()
	leaf.EncryptionKey = aFreshEncryptionKeyOtherThan(t, crypto, leaf.EncryptionKey)
	if err := releafed.SetLeaf(sender, leaf); err != nil {
		t.Fatalf("SetLeaf: %v", err)
	}
	if bytes.Equal(treeHashOf(t, crypto, releafed), opened) {
		t.Errorf("the sender's new leaf key is not inside the tree hash the caller binds this epoch to")
	}
	if swept == 0 || hashed == 0 {
		t.Fatalf("the sweep read %d path nodes and %d parent hash fields; a zero in either is a direction this test did not reach",
			swept, hashed)
	}
	t.Logf("%d path nodes and %d parent hash fields held inside the new epoch's tree hash", swept, hashed)
}

// updatePathChainOf reads the parent hash chain a tree CARRIES along one filtered direct path,
// rung by rung: rung 0 is the leaf's field and rung i+1 is the field at steps[i].
func updatePathChainOf(t *testing.T, tree *RatchetTree, sender LeafIndex, steps []PathStep) [][]byte {
	t.Helper()
	chain := make([][]byte, len(steps)+1)
	leaf := tree.Leaf(sender)
	if leaf == nil {
		t.Fatalf("leaf %d is blank", sender)
	}
	chain[0] = cloneBytes(leaf.ParentHash)
	for i, step := range steps {
		parent := tree.ParentAt(step.Node)
		if parent == nil {
			t.Fatalf("node %d of the filtered path is blank", step.Node)
		}
		chain[i+1] = cloneBytes(parent.ParentHash)
	}
	return chain
}

// recomputeUpdatePathChain writes section 7.9's chain over the tree it is given and answers it in
// updatePathChainOf's rungs.
//
// From the RFC's sentence rather than from treekem.go: "If P is the root, then the parent_hash
// field is set to a zero-length octet string. Otherwise, parent_hash is the parent hash of the
// next node after P on the filtered direct path", and section 7.9.1's "the parent_hash field of
// D is equal to the parent hash of P with copath child S. This is the case even when the node D
// is a leaf node." That is a recurrence from the top down, and it MUTATES the tree it walks --
// each node's field has to be final before the node below it can be hashed -- so every caller
// below hands it a clone.
func recomputeUpdatePathChain(t *testing.T, crypto CryptoProvider, tree *RatchetTree,
	steps []PathStep) [][]byte {
	t.Helper()
	chain := make([][]byte, len(steps)+1)
	carried := []byte{}
	for i := len(steps) - 1; i >= 0; i -= 1 {
		parent := tree.ParentAt(steps[i].Node)
		if parent == nil {
			t.Fatalf("node %d of the filtered path is blank", steps[i].Node)
		}
		parent.ParentHash = carried
		chain[i+1] = cloneBytes(carried)
		hash, err := tree.ParentHash(crypto, steps[i].Node, steps[i].CopathChild)
		if err != nil {
			t.Fatalf("ParentHash(%d, %d): %v", steps[i].Node, steps[i].CopathChild, err)
		}
		carried = hash
	}
	chain[0] = cloneBytes(carried)
	return chain
}

// TestTheUpdatePathParentHashChainMovesEverythingBelowANodeAndNothingAbove is the dependency
// structure of the chain, swept.
//
// Two properties, and the second is what makes the first worth stating. The chain a commit
// installs is the one section 7.9's recurrence produces from the root down -- that is asserted
// elementwise, so a chain computed in the other direction, or one that stopped short of the
// leaf, is a mismatch at a named rung rather than a verdict. And the recurrence really is a
// chain: a fresh key at one node of the path moves the parent hash of every rung BELOW it and no
// rung at or above it, because the hash at a rung above covers the OTHER child's subtree.
//
// The second half is the one that separates a real chain from a set of digests that merely
// verify. An implementation that hashed each node against a constant instead of against the node
// above it produces a full length chain over which VerifyParentHashes has nothing to say at the
// nodes it never links -- and every rung of it would be insensitive to a change higher up, which
// is exactly what this sweep counts.
//
// Both the positions and the senders are DERIVED. Every width from two to eight, every member of
// each as the sender, and every rung of each path as the node that changes; a sender whose
// filtered path is shorter than two nodes has no above and below to separate and is SKIPPED with
// a counter rather than excluded by a width, because which senders those are is a property of
// the filter and changes when the filter does.
func TestTheUpdatePathParentHashChainMovesEverythingBelowANodeAndNothingAbove(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	senders, moved, held, tooShort := 0, 0, 0, 0
	for width := uint32(2); width <= 8; width += 1 {
		for at := uint32(0); at < width; at += 1 {
			tree, members := newTestTree(t, crypto, width)
			sender := members[at].LeafIndex
			plan, err := tree.CreateUpdatePathSecrets(crypto, sender, members[at].SignaturePriv, testGroupId())
			if err != nil {
				t.Fatalf("width=%d sender=%d CreateUpdatePathSecrets: %v", width, sender, err)
			}
			steps, err := tree.filteredPathSteps(sender)
			if err != nil {
				t.Fatalf("width=%d sender=%d filteredPathSteps: %v", width, sender, err)
			}
			if len(steps) != len(plan.Path) {
				t.Fatalf("width=%d sender=%d the plan carries %d nodes and the tree's filtered path has %d",
					width, sender, len(plan.Path), len(steps))
			}
			if len(steps) < 2 {
				tooShort += 1
				continue
			}
			senders += 1
			// the chain as installed is the chain section 7.9's recurrence produces
			installed := updatePathChainOf(t, tree, sender, steps)
			original := recomputeUpdatePathChain(t, crypto, tree.Clone(), steps)
			for rung := range original {
				if !bytes.Equal(installed[rung], original[rung]) {
					t.Fatalf("width=%d sender=%d rung %d of the installed chain is %x and section 7.9's recurrence gives %x",
						width, sender, rung, installed[rung], original[rung])
				}
			}
			if len(original[len(steps)]) != 0 {
				t.Errorf("width=%d sender=%d the node at the top of the filtered path carries %d bytes, and section 7.9 gives it the zero-length octet string",
					width, sender, len(original[len(steps)]))
			}
			for k := range steps {
				changed := tree.Clone()
				node := changed.ParentAt(steps[k].Node)
				if node == nil {
					t.Fatalf("width=%d sender=%d node %d is blank", width, sender, steps[k].Node)
				}
				if err := changed.SetParent(steps[k].Node, &ParentNode{
					EncryptionKey: aFreshEncryptionKeyOtherThan(t, crypto, node.EncryptionKey),
					ParentHash:    cloneBytes(node.ParentHash),
				}); err != nil {
					t.Fatalf("SetParent(%d): %v", steps[k].Node, err)
				}
				after := recomputeUpdatePathChain(t, crypto, changed, steps)
				for rung := range after {
					// rung k is the parent hash OF node steps[k], so it and every rung under
					// it are downstream of that node's key and every rung above it is not
					shouldMove := rung <= k
					same := bytes.Equal(original[rung], after[rung])
					if shouldMove && same {
						t.Errorf("width=%d sender=%d a fresh key at node %d (rung %d of the path) left rung %d of the chain unchanged, and rung %d is below it",
							width, sender, steps[k].Node, k, rung, rung)
					}
					if !shouldMove && !same {
						t.Errorf("width=%d sender=%d a fresh key at node %d (rung %d of the path) moved rung %d of the chain, and rung %d is at or above it",
							width, sender, steps[k].Node, k, rung, rung)
					}
					if shouldMove {
						moved += 1
					} else {
						held += 1
					}
				}
			}
		}
	}
	if senders == 0 || moved == 0 || held == 0 {
		t.Fatalf("the sweep ran %d senders and made %d must-move and %d must-hold observations, with %d paths too short; a zero in any of the three is a direction this test did not reach",
			senders, moved, held, tooShort)
	}
	t.Logf("%d senders, %d must-move and %d must-hold observations, %d paths too short to have an above and a below",
		senders, moved, held, tooShort)
}

// ---------------------------------------------------------------------------
// task 18: the four mutants a review found this file could not see
// ---------------------------------------------------------------------------
//
// Everything above this line compares two derivations of the same commit against each other:
// the public key at a node against the key its own path secret derives, the commit secret
// against DeriveSecret of the last rung, the installed chain against a recomputation of it.
// Every one of those holds over a commit whose entropy is a constant. A sender that seeded its
// leaf key pair from thirty two zero octets installs the SAME leaf private key in every group
// in the world, agrees with itself at every node, satisfies Consistent, verifies its parent
// hashes and passed all of it -- measured, not supposed. So do a path secret ladder seeded from
// a constant, a private state whose leaf key has no relationship to the leaf key the tree
// publishes, and a filtered direct path published root-first.
//
// What separates those four from a correct commit is never a comparison inside one run. It is a
// second run: against a different entropy stream, against the tree's own filtered path, or
// against the HPKE round trip the private state exists to perform. That is what the four tests
// below are.

// octetStringsUnder is every non-empty octet string reachable from a value: any slice or array
// of octets, and any named type spelled over one, found through pointers, structs, slices,
// arrays and maps.
//
// Reflective rather than a list of fields, which is the whole reason it is worth writing. The
// property the freshness test asserts is about EVERY octet string a commit produces, and a
// field added to UpdatePathPlan or to a node by a later task is under that property the day it
// is declared rather than the day somebody remembers to add it here. The octets are read one at
// a time through Uint rather than through Bytes because the walk descends into RatchetTree's
// unexported node array, and a value obtained that way refuses to be handed out whole.
//
// Zero-length strings are skipped. The topmost node of a filtered path carries the zero-length
// octet string section 7.9 gives it, which is a value with no entropy in it either way, and a
// walk that reported it would make every run share one member with every other.
func octetStringsUnder(value reflect.Value) [][]byte {
	found := [][]byte{}
	var walk func(reflect.Value)
	walk = func(at reflect.Value) {
		if !at.IsValid() {
			return
		}
		switch at.Kind() {
		case reflect.Pointer, reflect.Interface:
			if !at.IsNil() {
				walk(at.Elem())
			}
		case reflect.Slice, reflect.Array:
			if at.Kind() == reflect.Slice && at.IsNil() {
				return
			}
			if at.Type().Elem().Kind() == reflect.Uint8 {
				if at.Len() == 0 {
					return
				}
				octets := make([]byte, at.Len())
				for i := range octets {
					octets[i] = byte(at.Index(i).Uint())
				}
				found = append(found, octets)
				return
			}
			for i := 0; i < at.Len(); i += 1 {
				walk(at.Index(i))
			}
		case reflect.Map:
			for _, key := range at.MapKeys() {
				walk(key)
				walk(at.MapIndex(key))
			}
		case reflect.Struct:
			for i := 0; i < at.NumField(); i += 1 {
				walk(at.Field(i))
			}
		}
	}
	walk(value)
	return found
}

// hexSetOf is a set of octet strings keyed by their hex, which is how the sweep below asks
// whether two runs produced one value without caring where in either structure it sat.
func hexSetOf(values [][]byte) map[string]bool {
	found := map[string]bool{}
	for _, value := range values {
		found[hex.EncodeToString(value)] = true
	}
	return found
}

// TestTheUpdatePathDrawsEveryOctetStringItPublishesFromFreshEntropy is the property no
// comparison inside one commit can make, over BOTH halves of what a commit publishes: the plan
// task 18 computes and the path task 20 seals out of it.
//
// A commit that seeds its leaf key pair, or its whole path secret ladder, from a constant is
// self-consistent at every point this file otherwise checks: each public key is the one its
// path secret derives, the commit secret is the rung past the root, the private state is
// Consistent with the tree, the parent hashes verify. It is also the same leaf private key, the
// same path_secret[0] and the same commit secret in every group that implementation ever runs
// -- so the copath of any one commit anywhere holds the sender's leaf key everywhere. Both
// mutants were applied and the whole package stayed green.
//
// The instrument is two entropy streams over ONE tree. Everything about the two runs is
// identical except the octets the provider hands out, so an octet string that comes back the
// same from both was not drawn: it was carried out of the tree, or it is a constant. The
// carried half is derived from the input tree itself rather than listed, so a credential, a
// signature key or an extension body the fixture happens to hold is excused by being IN it and
// nothing else is.
//
// The class of values judged is derived too, by walking the plan, the tree the calls left
// behind and the published path for every octet string in them. A field a later task adds to
// any of the three is under this property without an edit here, which is the half a named list
// of five secrets would lose -- and it is why the sealing was folded into this gate rather than
// given a second one of its own. Every seal draws an ephemeral KEM key, so a sealing that drew
// it from a constant would publish the same kem output to the same member in every group it
// ever ran in, and the ciphertexts would still open, still pair with their resolution entries
// and still round trip through the codec.
//
// Two controls, because a differential test's failure mode is having no difference to see.
// Running the SAME stream twice must reproduce the run exactly, which is what says a difference
// between the two streams is the entropy rather than incidental nondeterminism; and the values
// the RFC makes secret are looked up by name in the drawn set, which is what says the walk
// found them at all. A walker that returned nothing would satisfy the intersection property and
// nothing else.
func TestTheUpdatePathDrawsEveryOctetStringItPublishesFromFreshEntropy(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	fixture, members := newTestTree(t, crypto, 8)
	const at = 3
	sender := members[at].LeafIndex

	// what the call may answer without having drawn it: every octet string the tree it was
	// handed already held, plus the group id the caller passes in
	carried := hexSetOf(octetStringsUnder(reflect.ValueOf(fixture)))
	carried[hex.EncodeToString(testGroupId())] = true
	if len(carried) < 8 {
		t.Fatalf("the input tree walked to %d octet strings, and an eight member tree holds a key, a credential and an extension body per leaf; the walk is not reading it",
			len(carried))
	}

	// one commit over a copy of that tree, under a provider whose entropy the caller chose
	run := func(first byte) (map[string]bool, map[string][]byte) {
		tree := fixture.Clone()
		drawn := mustProviderOver(t, CipherSuiteX25519ChaCha20Sha256Ed25519, providerStubStream(first))
		plan, err := tree.CreateUpdatePathSecrets(drawn, sender, members[at].SignaturePriv, testGroupId())
		if err != nil {
			t.Fatalf("stream %#02x: CreateUpdatePathSecrets: %v", first, err)
		}
		// and the PUBLISHED half, sealed under the context of the epoch this commit opens.
		// Every kem output and every ciphertext there is an octet string this commit draws and
		// then publishes, so the class this sweep judges has to reach them -- and it reaches
		// them the way it reaches the plan's, by walking the value rather than by naming a field.
		treeHash, err := tree.TreeHash(drawn)
		if err != nil {
			t.Fatalf("stream %#02x: TreeHash: %v", first, err)
		}
		path, err := tree.EncryptUpdatePath(drawn, plan, sender, testUpdatePathContext(t, treeHash), nil)
		if err != nil {
			t.Fatalf("stream %#02x: EncryptUpdatePath: %v", first, err)
		}
		produced := slices.Concat(
			octetStringsUnder(reflect.ValueOf(plan)),
			octetStringsUnder(reflect.ValueOf(tree)),
			octetStringsUnder(reflect.ValueOf(path)))
		drew := map[string]bool{}
		for value := range hexSetOf(produced) {
			if !carried[value] {
				drew[value] = true
			}
		}
		named := map[string][]byte{
			"the sender's new leaf encryption key, as the tree publishes it": tree.Leaf(sender).EncryptionKey,
			"the leaf private key the plan's private state keeps":            plan.Private.EncryptionPriv,
			"path_secret[0]":    plan.PathSecrets[0],
			"the commit secret": plan.CommitSecret,
		}
		for i, x := range plan.Path {
			named[fmt.Sprintf("the public key published at node %d", x)] = plan.PublicKeys[i]
			named[fmt.Sprintf("the path secret the private state holds for node %d", x)] = plan.Private.PathSecrets[x]
		}
		for i, node := range path.Nodes {
			for j, ct := range node.EncryptedPathSecret {
				named[fmt.Sprintf("the kem output published at path node %d for resolution entry %d", i, j)] = ct.KemOutput
				named[fmt.Sprintf("the ciphertext published at path node %d for resolution entry %d", i, j)] = ct.Ciphertext
			}
		}
		return drew, named
	}

	first, named := run(0x11)
	second, _ := run(0xa4)
	again, _ := run(0x11)

	// the control: one stream is one run, so a value that survives below survives because the
	// entropy did not reach it and not because the fixture wandered
	if !maps.Equal(first, again) {
		t.Fatalf("two commits over one tree under one entropy stream drew %d and %d octet strings and the sets differ; this test cannot tell a constant from a value that varies for some other reason",
			len(first), len(again))
	}
	if len(first) == 0 {
		t.Fatal("the walk found no octet string in the plan or the tree that the input tree did not already hold, so the sweep below compares two empty sets")
	}

	// the coverage claim, checked rather than assumed: the values the RFC makes secret have to
	// be among the ones being judged
	for what, octets := range named {
		if len(octets) == 0 {
			t.Errorf("%s is empty", what)
			continue
		}
		if !first[hex.EncodeToString(octets)] {
			t.Errorf("%s is not among the octet strings this commit drew, so the sweep below is not judging it", what)
		}
	}

	// the property. Nothing a commit produces that its tree did not already hold may repeat
	// across two entropy streams.
	repeated := []string{}
	for value := range first {
		if second[value] {
			repeated = append(repeated, value)
		}
	}
	slices.Sort(repeated)
	for _, value := range repeated {
		what := "an octet string"
		for name, octets := range named {
			if hex.EncodeToString(octets) == value {
				what = name
			}
		}
		t.Errorf("two commits over one tree under two different entropy streams both produced %s = %s; it is a constant of this implementation rather than something this commit drew, which is the sender's own secret handed to every group it ever runs in",
			what, value)
	}
	t.Logf("%d octet strings drawn per commit, %d of them named, none shared across two entropy streams",
		len(first), len(named))
}

// encryptionKeyAt is the HPKE public key a tree carries at one node, whichever kind of node it
// is. A blank position is fatal rather than an empty key: every caller below reads a position
// the commit it just made installed.
func encryptionKeyAt(t *testing.T, tree *RatchetTree, x NodeIndex) HpkePublicKey {
	t.Helper()
	node := tree.Get(x)
	if node == nil {
		t.Fatalf("node %d is blank", x)
	}
	if node.Leaf != nil {
		return node.Leaf.EncryptionKey
	}
	if node.Parent != nil {
		return node.Parent.EncryptionKey
	}
	t.Fatalf("node %d holds neither a leaf nor a parent", x)
	return nil
}

// TestThePrivateStateOpensEverythingSealedToTheKeysThisCommitInstalls is the round trip the
// plan's private half exists to perform, made at the one place both halves are in scope.
//
// TreeKEMPrivate.Consistent deliberately does not check the leaf pair -- its scope is the path
// secrets, and its own comment defers the leaf pair to task 22's decrypt, where the UpdatePath
// carries the public key to compare against. (The provider INTERFACE has no private-to-public
// operation; hpkePublicKeyOf is the package level derivation outside it, and the join door uses it
// on the one leaf pair a caller brings in from outside.) That leaves task 18 as the only
// call in the package where the private key it stores and the public key it installs are both
// present, and before this test nothing related them: the plan's leaf private key could be
// drawn from a second, independent key pair and every test in this file passed, including the
// one named for the leaf's independence, which asserted only that the field was not empty. The
// sender would then hold a private state that cannot open anything sealed to its own published
// leaf key, and would find that out one epoch later as a decryption failure.
//
// So the assertion is the operation itself, through HpkeSeal and HpkeOpen, which the provider
// surface has had since task 15. The class is DERIVED from the commit: the sender's own leaf,
// plus every node of the path the plan publishes, read out of the tree the call left behind
// rather than out of the plan, because what a peer will seal to is what the TREE carries. Both
// arms of NodePrivateKey are exercised by that class without being named -- the leaf is the
// stored-key arm and every path node is the derived arm.
//
// The negative control is per node rather than once. A round trip that succeeded for a reason
// other than the key -- a provider that ignored the private key, say -- would pass every line
// above, so an unrelated private key is required to fail at each of the same positions.
func TestThePrivateStateOpensEverythingSealedToTheKeysThisCommitInstalls(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	info := []byte("urmessage: the info an update path seals under")
	aad := []byte("urmessage: the aad")
	plaintext := []byte("a path secret sized plaintext, thirty two octets.")
	opened, refused, senders := 0, 0, 0
	for _, n := range []uint32{2, 3, 4, 7, 8} {
		for at := uint32(0); at < n; at += 1 {
			tree, members := newTestTree(t, crypto, n)
			sender := members[at].LeafIndex
			plan, err := tree.CreateUpdatePathSecrets(crypto, sender, members[at].SignaturePriv, testGroupId())
			if err != nil {
				t.Fatalf("n=%d sender=%d CreateUpdatePathSecrets: %v", n, sender, err)
			}
			senders += 1
			// the class: the leaf this commit re-keyed, and every node it published a key at
			installed := append([]NodeIndex{sender.NodeIndex()}, plan.Path...)
			for _, x := range installed {
				public := encryptionKeyAt(t, tree, x)
				private, held, err := plan.Private.NodePrivateKey(crypto, x)
				if err != nil {
					t.Fatalf("n=%d sender=%d NodePrivateKey(%d): %v", n, sender, x, err)
				}
				if !held {
					t.Errorf("n=%d sender=%d the commit installed a key at node %d and its own private state holds nothing for it",
						n, sender, x)
					continue
				}
				kemOutput, ciphertext, err := crypto.HpkeSeal(public, info, aad, plaintext)
				if err != nil {
					t.Fatalf("n=%d sender=%d HpkeSeal to node %d: %v", n, sender, x, err)
				}
				got, err := crypto.HpkeOpen(private, kemOutput, info, aad, ciphertext)
				if err != nil {
					t.Errorf("n=%d sender=%d the private state cannot open what was sealed to the key this commit installed at node %d: %v",
						n, sender, x, err)
					continue
				}
				if !bytes.Equal(got, plaintext) {
					t.Errorf("n=%d sender=%d node %d opened to %x, want %x", n, sender, x, got, plaintext)
					continue
				}
				opened += 1
				// the control, at the same node: an unrelated private key must not open it
				other, _, err := crypto.DeriveKeyPair(crypto.Random(crypto.HashSize()))
				if err != nil {
					t.Fatalf("DeriveKeyPair: %v", err)
				}
				if _, err := crypto.HpkeOpen(other, kemOutput, info, aad, ciphertext); err == nil {
					t.Fatalf("n=%d sender=%d a private key unrelated to node %d opened its ciphertext, so the round trip above says nothing about which key was used",
						n, sender, x)
				}
				refused += 1
			}
		}
	}
	if senders == 0 || opened == 0 || refused == 0 {
		t.Fatalf("the sweep ran %d senders, %d round trips and %d controls; a zero in any of the three is a direction this test did not reach",
			senders, opened, refused)
	}
	t.Logf("%d senders, %d keys installed and opened through the plan's own private state, %d unrelated keys refused",
		senders, opened, refused)
}

// TestTheUpdatePathIsPublishedLeafFirstAlongTheFilteredDirectPath is the order contract, which
// nothing held.
//
// FilteredDirectPath's own doc states it: "The order is the contract and not a detail of it ...
// That is why the tests compare elementwise through equalNodeIndices and never as sets." The
// plan built the same slice and no test compared it against anything -- the two that touch it
// check that it is not empty and that it has the same LENGTH as the filtered path -- so
// publishing it root-first left the whole package green. That reversal is entirely
// self-consistent inside this task, because publicKeys[i] is installed at path[i] and the
// private state is keyed by path[i] too: the topmost node ends up carrying the key derived from
// the LOWEST rung, which inverts the one-way asymmetry this file's header says the security
// argument rests on, and task 19's wire type and task 20's ciphertexts pair positionally with
// exactly this slice.
//
// Two readings, deliberately not one. Elementwise against the tree's own FilteredDirectPath is
// the contract as this package states it; and against tree math's unfiltered direct path, which
// is bottom-up by construction, is the same statement made without the filter in it -- every
// published node is on the sender's direct path and their positions along it strictly ascend.
// A filter that reversed its own answer would satisfy the first and fail the second.
//
// The ladder pairing is here too, because it is the same positional claim about the other half
// of the plan: rung i+1 is DeriveSecret of rung i under "path", and it derives the key
// published one node higher.
//
// Every width from two to eight and every member of each as the sender, since which senders
// have a filtered path shorter than the direct path is a property of the filter.
func TestTheUpdatePathIsPublishedLeafFirstAlongTheFilteredDirectPath(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	senders, published, placed, rungs := 0, 0, 0, 0
	for width := uint32(2); width <= 8; width += 1 {
		for at := uint32(0); at < width; at += 1 {
			tree, members := newTestTree(t, crypto, width)
			sender := members[at].LeafIndex
			before := tree.Clone()
			want, err := before.FilteredDirectPath(sender)
			if err != nil {
				t.Fatalf("width=%d sender=%d FilteredDirectPath: %v", width, sender, err)
			}
			plan, err := tree.CreateUpdatePathSecrets(crypto, sender, members[at].SignaturePriv, testGroupId())
			if err != nil {
				t.Fatalf("width=%d sender=%d CreateUpdatePathSecrets: %v", width, sender, err)
			}
			senders += 1
			// the contract as this package states it, elementwise and never as a set
			if !equalNodeIndices(plan.Path, want) {
				t.Errorf("width=%d sender=%d the plan publishes %v and the filtered direct path of the tree it committed over is %v",
					width, sender, plan.Path, want)
			}
			// and over the tree the call left behind, which is the one task 20 hands on
			after, err := tree.FilteredDirectPath(sender)
			if err != nil {
				t.Fatalf("width=%d sender=%d FilteredDirectPath after the commit: %v", width, sender, err)
			}
			if !equalNodeIndices(plan.Path, after) {
				t.Errorf("width=%d sender=%d the plan publishes %v and the filtered direct path of the tree it left behind is %v",
					width, sender, plan.Path, after)
			}
			published += len(plan.Path)

			// the same claim without the filter in it: tree math's direct path is bottom-up by
			// construction, so a published path that is leaf-first visits its positions in
			// strictly ascending order
			direct, err := directPathOf(sender.NodeIndex(), before.LeafWidth())
			if err != nil {
				t.Fatalf("width=%d sender=%d directPathOf: %v", width, sender, err)
			}
			height := map[NodeIndex]int{}
			for rung, x := range direct {
				height[x] = rung
			}
			last := -1
			for i, x := range plan.Path {
				rung, on := height[x]
				if !on {
					t.Errorf("width=%d sender=%d the plan publishes node %d at position %d, and that node is not on the sender's direct path %v",
						width, sender, x, i, direct)
					continue
				}
				if rung <= last {
					t.Errorf("width=%d sender=%d the plan publishes node %d (rung %d of the direct path) at position %d, after a node at rung %d; the filtered direct path is published leaf-first and this one runs the other way",
						width, sender, x, rung, i, last)
				}
				last = rung
				placed += 1
			}

			// the ladder, paired with the path the same way: rung i+1 is one DeriveSecret above
			// rung i, and it is the secret the node one position higher was keyed from
			for i := 0; i+1 < len(plan.PathSecrets); i += 1 {
				next := crypto.DeriveSecret(plan.PathSecrets[i], "path")
				if !bytes.Equal(next, plan.PathSecrets[i+1]) {
					t.Errorf("width=%d sender=%d path secret %d is not DeriveSecret of path secret %d under the label \"path\"",
						width, sender, i+1, i)
					continue
				}
				_, derived, err := DeriveNodeKeyPair(crypto, next)
				if err != nil {
					t.Fatalf("DeriveNodeKeyPair: %v", err)
				}
				if !bytes.Equal(derived, plan.PublicKeys[i+1]) {
					t.Errorf("width=%d sender=%d the rung above path secret %d does not derive the key published at position %d (node %d)",
						width, sender, i, i+1, plan.Path[i+1])
				}
				rungs += 1
			}
		}
	}
	if senders == 0 || published == 0 || placed == 0 || rungs == 0 {
		t.Fatalf("the sweep ran %d senders, %d published nodes, %d of them placed against the unfiltered direct path and %d ladder steps; a zero in any of the four is a direction this test did not reach",
			senders, published, placed, rungs)
	}
	t.Logf("%d senders, %d published nodes, %d placed against the unfiltered direct path, %d ladder steps",
		senders, published, placed, rungs)
}

// ---------------------------------------------------------------------------
// task 18: what a failed commit must leave behind, and which fault it names
// ---------------------------------------------------------------------------

// errInjectedProviderFault is what the sweep below makes exactly one provider call answer, so
// a refusal produced by the injection is never mistaken for one the tree raised on its own.
var errInjectedProviderFault = errors.New("mls: injected provider fault")

// faultInjectingProvider fails the nth call to a CryptoProvider method that has an error to
// fail with, and passes every other call straight through to the provider it embeds.
//
// The embedded interface is what makes the passthrough total: a method this type does not
// override is answered by the real provider, so the sweep only ever perturbs the one call it
// means to. Which methods it MUST override is not a judgement made here --
// TestTheFaultInjectingProviderCoversEveryProviderCallThatCanFail derives that class off the
// interface itself and calls each member, so a fallible method added to CryptoProvider by a
// later task fails that gate rather than quietly becoming a failure this sweep cannot inject.
type faultInjectingProvider struct {
	CryptoProvider
	failAt int
	calls  int
}

// faults counts this call and answers whether it is the one to fail. Counting happens on every
// fallible call whether or not it fails, which is what makes a run with failAt of zero a census
// of how many injection points the operation has.
func (self *faultInjectingProvider) faults() bool {
	self.calls += 1
	return self.calls == self.failAt
}

func (self *faultInjectingProvider) AeadSeal(key []byte, nonce []byte, aad []byte, plaintext []byte) ([]byte, error) {
	if self.faults() {
		return nil, errInjectedProviderFault
	}
	return self.CryptoProvider.AeadSeal(key, nonce, aad, plaintext)
}

func (self *faultInjectingProvider) AeadOpen(key []byte, nonce []byte, aad []byte, ciphertext []byte) ([]byte, error) {
	if self.faults() {
		return nil, errInjectedProviderFault
	}
	return self.CryptoProvider.AeadOpen(key, nonce, aad, ciphertext)
}

func (self *faultInjectingProvider) SignWithLabel(priv SignaturePrivateKey, label string, content []byte) ([]byte, error) {
	if self.faults() {
		return nil, errInjectedProviderFault
	}
	return self.CryptoProvider.SignWithLabel(priv, label, content)
}

func (self *faultInjectingProvider) VerifyWithLabel(pub SignaturePublicKey, label string, content []byte, sig []byte) error {
	if self.faults() {
		return errInjectedProviderFault
	}
	return self.CryptoProvider.VerifyWithLabel(pub, label, content, sig)
}

func (self *faultInjectingProvider) HpkeSeal(pub HpkePublicKey, info []byte, aad []byte, plaintext []byte) ([]byte, []byte, error) {
	if self.faults() {
		return nil, nil, errInjectedProviderFault
	}
	return self.CryptoProvider.HpkeSeal(pub, info, aad, plaintext)
}

func (self *faultInjectingProvider) HpkeOpen(priv HpkePrivateKey, kemOutput []byte, info []byte, aad []byte, ciphertext []byte) ([]byte, error) {
	if self.faults() {
		return nil, errInjectedProviderFault
	}
	return self.CryptoProvider.HpkeOpen(priv, kemOutput, info, aad, ciphertext)
}

func (self *faultInjectingProvider) DeriveKeyPair(ikm []byte) (HpkePrivateKey, HpkePublicKey, error) {
	if self.faults() {
		return nil, nil, errInjectedProviderFault
	}
	return self.CryptoProvider.DeriveKeyPair(ikm)
}

func (self *faultInjectingProvider) SignatureKeyPair() (SignaturePrivateKey, SignaturePublicKey, error) {
	if self.faults() {
		return nil, nil, errInjectedProviderFault
	}
	return self.CryptoProvider.SignatureKeyPair()
}

// fallibleProviderMethods is every method of CryptoProvider whose results include an error,
// read off the interface type rather than written out.
//
// This is the class the injecting provider has to cover, and it is derived for the reason
// standing rule 5 gives: a hand written list of eight names understates the class the moment a
// ninth fallible method lands, and the sweep that uses it would then report a clean bill over a
// failure it never injected.
func fallibleProviderMethods() []string {
	surface := reflect.TypeOf((*CryptoProvider)(nil)).Elem()
	failure := reflect.TypeOf((*error)(nil)).Elem()
	found := []string{}
	for i := 0; i < surface.NumMethod(); i += 1 {
		method := surface.Method(i)
		for at := 0; at < method.Type.NumOut(); at += 1 {
			if method.Type.Out(at) == failure {
				found = append(found, method.Name)
				break
			}
		}
	}
	slices.Sort(found)
	return found
}

// TestTheFaultInjectingProviderCoversEveryProviderCallThatCanFail holds the fixture to the
// class above, in the only way that cannot go quiet: by CALLING each member.
//
// A structural check -- does this type declare a method of that name -- would be satisfied by
// an override that forgot to consult the counter. So each fallible method is invoked with the
// fault armed at the first call and is required to answer the injected fault: a method the
// fixture does not override reaches the embedded provider and answers something else, or
// panics on the zero arguments, and either way is reported here rather than silently becoming
// a failure the atomicity sweep cannot reach.
func TestTheFaultInjectingProviderCoversEveryProviderCallThatCanFail(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	names := fallibleProviderMethods()
	if len(names) == 0 {
		t.Fatal("no method of CryptoProvider was read as having an error result, so this gate is judging an empty class")
	}
	for _, name := range names {
		armed := CryptoProvider(&faultInjectingProvider{CryptoProvider: crypto, failAt: 1})
		method := reflect.ValueOf(armed).MethodByName(name)
		if !method.IsValid() {
			t.Errorf("CryptoProvider declares %s and the fault injecting provider has no such method", name)
			continue
		}
		arguments := make([]reflect.Value, method.Type().NumIn())
		for i := range arguments {
			arguments[i] = reflect.New(method.Type().In(i)).Elem()
		}
		results := []reflect.Value{}
		if panicked := recoveredPanic(func() { results = method.Call(arguments) }); panicked != nil {
			t.Errorf("%s panicked with %v instead of answering the armed fault, so the fault injecting provider does not override it and the atomicity sweep can never fail that call",
				name, panicked)
			continue
		}
		var answered error
		for _, result := range results {
			if failure, isError := result.Interface().(error); isError && failure != nil {
				answered = failure
			}
		}
		if !errors.Is(answered, errInjectedProviderFault) {
			t.Errorf("%s answered %v with the fault armed at its first call; the fault injecting provider does not override it, so a failure there is one the atomicity sweep cannot inject",
				name, answered)
		}
	}
	t.Logf("%d fallible provider methods, every one of them injectable: %v", len(names), names)
}

// TestCreateUpdatePathSecretsLeavesTheTreeExactlyAsItFoundItWhenItFails is the atomicity
// contract, and it is here because the shipped body did not have one.
//
// Every refusal this call makes was placed ahead of the mutation on purpose -- the nil
// provider, the leaf index, the filtered path -- except one. leaf.Sign is reachable only after
// the direct path has been blanked, the new public keys installed and the parent hash chain
// written, so a signature private key that does not match the ciphersuite used to answer an
// error over a caller's tree whose tree hash had already moved and which VerifyParentHashes
// then refused. Measured on the shipped source, not supposed: the tree hash moved and
// VerifyParentHashes answered "node 1 is claimed by 0 of its descendants". The previous epoch's
// keys are gone at that point, so there is nothing the caller can do with what it is left
// holding, and task 20 would have had to know to throw the tree away.
//
// The failure positions are DERIVED rather than picked. A census run counts how many fallible
// provider calls one commit makes, and the sweep then fails each of them in turn, so a call
// this operation gains or loses moves the sweep with it. On top of that sits the one failure no
// provider fault can produce -- a signature key of the wrong length, which is refused inside
// the provider rather than by failing it -- because that is the exact shape the review found.
//
// The instrument is checked before it is used. Clone is what the snapshot is taken with and
// what the operation works on, so a Clone that normalised anything would make DeepEqual either
// blind or permanently red; requiring a fresh clone to compare equal to its original says which.
func TestCreateUpdatePathSecretsLeavesTheTreeExactlyAsItFoundItWhenItFails(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	fixture, members := newTestTree(t, crypto, 8)
	const at = 3
	sender := members[at].LeafIndex
	if !reflect.DeepEqual(fixture, fixture.Clone()) {
		t.Fatal("a fresh clone of the fixture does not compare equal to it, so DeepEqual cannot say whether a failed call changed a tree")
	}

	// the census: how many fallible provider calls a SUCCESSFUL commit makes, which is the
	// range the sweep covers. failAt of zero never matches, so nothing is injected here.
	census := &faultInjectingProvider{CryptoProvider: crypto}
	probe := fixture.Clone()
	if _, err := probe.CreateUpdatePathSecrets(census, sender, members[at].SignaturePriv, testGroupId()); err != nil {
		t.Fatalf("the census run failed: %v", err)
	}
	if census.calls == 0 {
		t.Fatal("a successful commit made no fallible provider call, so the sweep below has no failure to inject")
	}

	refused := 0
	for point := 1; point <= census.calls; point += 1 {
		tree := fixture.Clone()
		before := tree.Clone()
		faulty := &faultInjectingProvider{CryptoProvider: crypto, failAt: point}
		plan, err := tree.CreateUpdatePathSecrets(faulty, sender, members[at].SignaturePriv, testGroupId())
		if err == nil {
			t.Errorf("failing provider call %d of %d was accepted and answered a plan over %d nodes; a fault the operation swallows is one it built a commit on top of",
				point, census.calls, len(plan.Path))
			continue
		}
		if !errors.Is(err, errInjectedProviderFault) {
			t.Errorf("failing provider call %d of %d answered %v, which is not the injected fault", point, census.calls, err)
		}
		refused += 1
		if !reflect.DeepEqual(tree, before) {
			t.Errorf("failing provider call %d of %d left the caller's tree changed; a commit that answers an error must be a no-op, because the epoch it half-installed cannot be undone",
				point, census.calls)
		}
	}
	if refused != census.calls {
		t.Errorf("%d of the %d fallible provider calls were refused; the sweep is meant to reach every one", refused, census.calls)
	}

	// the failure the injection cannot make, which is the one the review actually found: a
	// signature private key that does not match the ciphersuite, refused INSIDE SignWithLabel
	// and therefore after the whole mutation has happened
	tree := fixture.Clone()
	before := tree.Clone()
	wanted := treeHashOf(t, crypto, before)
	if _, err := tree.CreateUpdatePathSecrets(crypto, sender, SignaturePrivateKey{1, 2, 3}, testGroupId()); err == nil {
		t.Fatal("a signature private key of three octets was accepted, so this half of the test asserts nothing")
	}
	if !reflect.DeepEqual(tree, before) {
		t.Error("a commit refused for a signature key that does not match the ciphersuite left the caller's tree changed")
	}
	// spelled again at the two boundaries a caller actually reads, because those are what the
	// next epoch is built out of
	if got := treeHashOf(t, crypto, tree); !bytes.Equal(got, wanted) {
		t.Errorf("the tree hash moved from %x to %x across a commit that answered an error, so the caller would bind the next epoch to a tree no commit produced",
			wanted, got)
	}
	if err := tree.VerifyParentHashes(crypto); err != nil {
		t.Errorf("a commit that answered an error left the caller's tree failing section 7.9.2: %v", err)
	}
	t.Logf("%d fallible provider calls in one commit, every one of them refused and every one of them a no-op", census.calls)
}

// TestCreateUpdatePathSecretsTellsABlankLeafApartFromAnIndexOutsideTheTree is the split the
// review asked for while the sentinel was still cheap to move.
//
// RatchetTree.Leaf answers nil for a leaf index past the width and for an occupied position
// that has been blanked alike, and a body that let that nil decide answered
// ErrLeafIndexOutOfRange for both. The two callers are opposite: one computed an index wrong
// and repairs it by recomputing one, the other is a member committing from a slot the group
// REMOVED, whose index was right the whole time and whose only repair is to rejoin. Telling the
// second to check its index sends it to look at the one thing that is not the problem.
//
// The blank positions are derived from the tree rather than picked -- every member of an eight
// member tree is blanked in turn -- because which leaf indices are blankable is a property of
// the tree and not of a fixture. The out of range half sweeps from the first index past the
// width, which is the boundary, rather than a number chosen to be obviously too large.
//
// The exclusivity is the half with teeth. The two sentinels are useless if either answers for
// the other, so each refusal is required to match its own and to NOT match the other's, which
// is what a wrap between them would break.
func TestCreateUpdatePathSecretsTellsABlankLeafApartFromAnIndexOutsideTheTree(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	fixture, members := newTestTree(t, crypto, 8)
	width := uint32(fixture.LeafWidth())
	blanked, outside := 0, 0
	for at := uint32(0); at < width; at += 1 {
		tree := fixture.Clone()
		sender := LeafIndex(at)
		if err := tree.Blank(sender.NodeIndex()); err != nil {
			t.Fatalf("Blank(leaf %d): %v", sender, err)
		}
		_, err := tree.CreateUpdatePathSecrets(crypto, sender, members[at].SignaturePriv, testGroupId())
		if !errors.Is(err, ErrLeafBlank) {
			t.Errorf("a commit from blanked leaf %d, which is inside an %d leaf tree, answered %v; want %v",
				sender, width, err, ErrLeafBlank)
		}
		if errors.Is(err, ErrLeafIndexOutOfRange) {
			t.Errorf("a commit from blanked leaf %d answered %v, which also matches ErrLeafIndexOutOfRange; the member is being told to fix an index that was correct",
				sender, err)
		}
		blanked += 1
	}
	for _, sender := range []LeafIndex{LeafIndex(width), LeafIndex(width + 1), LeafIndex(width + 97)} {
		tree := fixture.Clone()
		_, err := tree.CreateUpdatePathSecrets(crypto, sender, members[0].SignaturePriv, testGroupId())
		if !errors.Is(err, ErrLeafIndexOutOfRange) {
			t.Errorf("a commit from leaf %d, which is outside an %d leaf tree, answered %v; want %v",
				sender, width, err, ErrLeafIndexOutOfRange)
		}
		if errors.Is(err, ErrLeafBlank) {
			t.Errorf("a commit from leaf %d, which is outside the tree, answered %v, which also matches ErrLeafBlank; the caller is being told a position exists and is empty when it does not exist",
				sender, err)
		}
		outside += 1
	}
	if blanked == 0 || outside == 0 {
		t.Fatalf("the sweep made %d blank-leaf calls and %d out of range calls; a zero in either is an arm this test did not reach", blanked, outside)
	}
	t.Logf("%d blanked leaves and %d indices past the width, each answered by its own sentinel and by neither the other", blanked, outside)
}

// ---------------------------------------------------------------------------
// task 19: the UpdatePath wire types
// ---------------------------------------------------------------------------

// The three levels of length prefix are what makes this codec different from every other one in
// this package, and two of the shapes that got through elsewhere are exactly the shapes nesting
// hides:
//
// A SYMMETRIC FIELD ORDER SWAP -- two fields exchanged in both halves -- round trips perfectly,
// re-encodes byte exact, and is byte exact against the published corpus too, because the corpus
// check only ever compares what this implementation decoded against what it re-encoded. The only
// thing that separates the two orders is a statement of the encoding written from the RFC without
// reference to the code, which is what the hand derived goldens below are.
//
// A FIELD DROPPED FROM BOTH HALVES round trips byte exact while being lost, because the decoder
// never reads what the encoder never wrote. Only a comparison of the decoded VALUE against the
// original catches it, never a comparison of bytes against re-encoded bytes.
//
// The third shape is this file's own and is NOT a codec property at all: a ciphertext count that
// does not match the resolution the node was built for. The codec is handed bytes and has no
// tree, so it cannot check it; the relation is stated here against the published corpus, so that
// this layer and task 22's decrypt at least agree about what the relation is.

// testUpdatePathFixture is the value every golden below is compared against.
//
// Every octet string in it is DIFFERENT from every other one, and the two fields of a
// ciphertext have different LENGTHS as well -- 32 against 48 -- which is what makes a swap of
// kem_output and ciphertext visible in the bytes at all. Two nodes, the first carrying two
// ciphertexts and the second carrying none, so that the vector prefix is exercised at both a one
// octet and a two octet width and an empty nested vector is covered by the same golden.
func testUpdatePathFixture() *UpdatePath {
	leaf := testLeafNodeTemplate()
	leaf.LeafNodeSource = LeafNodeSourceCommit
	leaf.ParentHash = repeatByte(0x44, 32)
	return &UpdatePath{
		LeafNode: *leaf,
		Nodes: []UpdatePathNode{
			{
				EncryptionKey: HpkePublicKey(repeatByte(0x01, 32)),
				EncryptedPathSecret: []HpkeCiphertext{
					{KemOutput: repeatByte(0x02, 32), Ciphertext: repeatByte(0x03, 48)},
					{KemOutput: repeatByte(0x04, 32), Ciphertext: repeatByte(0x05, 48)},
				},
			},
			{
				EncryptionKey:       HpkePublicKey(repeatByte(0x06, 32)),
				EncryptedPathSecret: []HpkeCiphertext{},
			},
		},
	}
}

// testUpdatePathDecodedForm is what a decode of that fixture must produce as a VALUE.
//
// It differs from the fixture in exactly one way, and it is the leaf's: the template carries a
// lifetime under a commit source, which section 7.2's select does not encode, so a correct
// decode answers the zero lifetime. leaf_node_test.go's decodedFormOf derives that from the
// variant table rather than from a case written here, so a fourth source is handled by this
// comparison on the commit that declares it.
func testUpdatePathDecodedForm(t *testing.T) *UpdatePath {
	t.Helper()
	out := testUpdatePathFixture()
	out.LeafNode = *decodedFormOf(t, &out.LeafNode)
	return out
}

// ---------------------------------------------------------------------------
// the hand derived goldens
// ---------------------------------------------------------------------------

// handDerivedHpkeCiphertext is one HPKECiphertext of the fixture, written out from RFC 9420
// section 6.1 rather than read back through the encoder:
//
//	kem_output<V>    32 octets -> 20 <kem>*32                              33
//	ciphertext<V>    48 octets -> 30 <ct>*48                               49
//	                                                                     ----
//	                                                                       82
//
// Both prefixes are the ONE octet form, because section 2.1.2 gives lengths 0..63 a single octet
// whose top two bits are zero: 32 is 0x20 and 48 is 0x30 with nothing else added. The two
// lengths are deliberately different, so that a codec writing the two fields in the other order
// produces 30 <ct>*48 20 <kem>*32, which is not these bytes.
func handDerivedHpkeCiphertext(kem byte, ct byte) []byte {
	return joinBytes([]byte{0x20}, repeatByte(kem, 32), []byte{0x30}, repeatByte(ct, 48))
}

// handDerivedUpdatePathNodeWithCiphertexts is the fixture's first node:
//
//	encryption_key<V>          32 octets      -> 20 01*32                  33
//	encrypted_path_secret<V>   2 x 82 = 164   -> 40 a4 <164 octets>       166
//	                                                                     ----
//	                                                                      199
//
// The vector prefix is the TWO octet form and the value is the BODY LENGTH IN OCTETS, not the
// element count. 164 is above 63 so section 2.1.2 gives it two octets with the prefix bits 0b01:
// 0x40 | (164 >> 8) = 0x40, then 164 & 0xff = 0xa4. An encoder writing the element count would
// write the single octet 0x02 here, an encoder using the four octet framing would write
// 0x800000a4, and neither is visible to any round trip because this implementation would read
// back whatever it wrote.
func handDerivedUpdatePathNodeWithCiphertexts() []byte {
	return joinBytes(
		[]byte{0x20}, repeatByte(0x01, 32),
		[]byte{0x40, 0xa4},
		handDerivedHpkeCiphertext(0x02, 0x03),
		handDerivedHpkeCiphertext(0x04, 0x05),
	)
}

// handDerivedUpdatePathNodeWithNone is the fixture's second node:
//
//	encryption_key<V>          32 octets      -> 20 06*32                  33
//	encrypted_path_secret<V>   empty          -> 00                         1
//	                                                                     ----
//	                                                                       34
//
// The empty vector is the single zero octet and nothing else, which is also what a nil vector
// encodes to: the wire format has no representation for "absent".
func handDerivedUpdatePathNodeWithNone() []byte {
	return joinBytes([]byte{0x20}, repeatByte(0x06, 32), []byte{0x00})
}

// handDerivedUpdatePathGolden is the whole structure of RFC 9420 section 7.6:
//
//	leaf_node       a commit source leaf, leaf_node_test.go's own derivation       192
//	nodes<V>        199 + 34 = 233 octets of body -> 40 e9 <233 octets>            235
//	                                                                             ----
//	                                                                              427
//
// The leaf comes from handDerivedLeafNodeGolden, which is derived from section 7.2 in the file
// that owns that structure and is checked against its own arithmetic there. Reusing it rather
// than re-deriving it here is deliberate: two hand derivations of one structure drift, and the
// one that drifted would be the one nothing else reads.
//
// 233 is above 63, so its prefix is the two octet form again: 0x40 | (233 >> 8) = 0x40, then
// 233 & 0xff = 0xe9.
func handDerivedUpdatePathGolden() []byte {
	return joinBytes(
		handDerivedLeafNodeGolden(LeafNodeSourceCommit),
		[]byte{0x40, 0xe9},
		handDerivedUpdatePathNodeWithCiphertexts(),
		handDerivedUpdatePathNodeWithNone(),
	)
}

// The arithmetic of the comments above, stated separately so that a derivation edited without
// its comment fails rather than quietly redefining what it is compared to. This is
// leaf_node_test.go's handDerivedLeafNodeSizes at three levels instead of one.
const (
	handDerivedHpkeCiphertextSize          = 82
	handDerivedUpdatePathNodeWithCtSize    = 199
	handDerivedUpdatePathNodeWithNoneSize  = 34
	handDerivedUpdatePathSize              = 427
	handDerivedUpdatePathLeafSize          = 192
	handDerivedUpdatePathNodesRegionLength = 233
)

// TestTheUpdatePathHandDerivationsAgreeWithTheirOwnArithmetic runs before the goldens are used
// for anything, so that a derivation which no longer holds the octets its comment counts fails
// here rather than pinning the encoder to whatever it happens to produce.
func TestTheUpdatePathHandDerivationsAgreeWithTheirOwnArithmetic(t *testing.T) {
	for _, one := range []struct {
		name string
		got  int
		want int
	}{
		{"HPKECiphertext", len(handDerivedHpkeCiphertext(0x02, 0x03)), handDerivedHpkeCiphertextSize},
		{"UpdatePathNode with two ciphertexts", len(handDerivedUpdatePathNodeWithCiphertexts()), handDerivedUpdatePathNodeWithCtSize},
		{"UpdatePathNode with none", len(handDerivedUpdatePathNodeWithNone()), handDerivedUpdatePathNodeWithNoneSize},
		{"the commit leaf", len(handDerivedLeafNodeGolden(LeafNodeSourceCommit)), handDerivedUpdatePathLeafSize},
		{"UpdatePath", len(handDerivedUpdatePathGolden()), handDerivedUpdatePathSize},
	} {
		if one.got != one.want {
			t.Errorf("the hand derivation of %s is %d octets and the arithmetic in its comment says %d",
				one.name, one.got, one.want)
		}
	}
	// the two nested totals, stated as the sums the prefixes above declare rather than as two
	// more constants: a prefix that says one number while the body holds another is the exact
	// defect a golden built out of the same expression on both sides cannot see.
	if got := handDerivedUpdatePathNodeWithCtSize + handDerivedUpdatePathNodeWithNoneSize; got != handDerivedUpdatePathNodesRegionLength {
		t.Errorf("the two nodes are %d octets and the nodes<V> prefix in the golden declares %d",
			got, handDerivedUpdatePathNodesRegionLength)
	}
	if got := handDerivedUpdatePathLeafSize + 2 + handDerivedUpdatePathNodesRegionLength; got != handDerivedUpdatePathSize {
		t.Errorf("the leaf, the two octet prefix and the nodes region are %d octets and the whole derivation says %d",
			got, handDerivedUpdatePathSize)
	}
}

// TestHpkeCiphertextMarshalMatchesTheHandDerivedGolden is the field order and prefix width pin
// for the innermost of the three structures, and it is the only test in this file that a
// kem_output and ciphertext exchanged in BOTH halves does not survive.
func TestHpkeCiphertextMarshalMatchesTheHandDerivedGolden(t *testing.T) {
	in := &HpkeCiphertext{KemOutput: repeatByte(0x02, 32), Ciphertext: repeatByte(0x03, 48)}
	want := handDerivedHpkeCiphertext(0x02, 0x03)
	encoded, err := syntax.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !bytes.Equal(encoded, want) {
		t.Errorf("Marshal =\n %x\nwant\n %x", encoded, want)
	}
	out := &HpkeCiphertext{}
	if err := syntax.Unmarshal(want, out); err != nil {
		t.Fatalf("Unmarshal the golden: %v", err)
	}
	// the whole value and not the two lengths: a decoder that put the kem output in the
	// ciphertext field agrees about both lengths in every fixture where they are equal, and
	// this package's real ciphertexts are 32 and 48 only by accident of the plaintext.
	if !reflect.DeepEqual(out, in) {
		t.Errorf("the golden decoded to kem_output %x ciphertext %x, want %x and %x",
			out.KemOutput, out.Ciphertext, in.KemOutput, in.Ciphertext)
	}
}

// TestUpdatePathNodeMarshalMatchesTheHandDerivedGoldens is the same pin one level out, over both
// shapes the fixture holds: a node whose ciphertext vector needs the two octet prefix, and one
// whose empty vector needs the single zero octet.
func TestUpdatePathNodeMarshalMatchesTheHandDerivedGoldens(t *testing.T) {
	fixture := testUpdatePathFixture()
	for at, want := range [][]byte{
		handDerivedUpdatePathNodeWithCiphertexts(),
		handDerivedUpdatePathNodeWithNone(),
	} {
		in := fixture.Nodes[at]
		encoded, err := syntax.Marshal(&in)
		if err != nil {
			t.Fatalf("node %d: Marshal: %v", at, err)
		}
		if !bytes.Equal(encoded, want) {
			t.Errorf("node %d: Marshal =\n %x\nwant\n %x", at, encoded, want)
		}
		out := &UpdatePathNode{}
		if err := syntax.Unmarshal(want, out); err != nil {
			t.Fatalf("node %d: Unmarshal the golden: %v", at, err)
		}
		if !reflect.DeepEqual(out, &in) {
			t.Errorf("node %d: the golden decoded to %+v, want %+v", at, out, in)
		}
	}
}

// TestUpdatePathMarshalMatchesTheHandDerivedGolden is the pin over the whole three level
// structure: the leaf, the nodes vector's byte length prefix, and every nested prefix under it.
func TestUpdatePathMarshalMatchesTheHandDerivedGolden(t *testing.T) {
	want := handDerivedUpdatePathGolden()
	encoded, err := syntax.Marshal(testUpdatePathFixture())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !bytes.Equal(encoded, want) {
		t.Errorf("Marshal =\n %x\nwant\n %x", encoded, want)
	}
	out := &UpdatePath{}
	if err := syntax.Unmarshal(want, out); err != nil {
		t.Fatalf("Unmarshal the golden: %v", err)
	}
	if !reflect.DeepEqual(out, testUpdatePathDecodedForm(t)) {
		t.Errorf("the golden decoded to a value the fixture does not describe:\ngot  %s\nwant %s",
			describeUpdatePath(out), describeUpdatePath(testUpdatePathDecodedForm(t)))
	}
}

// describeUpdatePath spells a path out far enough that a failure above says which of the three
// levels moved, since %+v over a structure of slices of slices prints addresses for the parts
// that matter.
func describeUpdatePath(path *UpdatePath) string {
	out := fmt.Sprintf("leaf{%s} nodes=%d", describeLeafNode(&path.LeafNode), len(path.Nodes))
	for at, node := range path.Nodes {
		out += fmt.Sprintf(" | node %d key=%x ciphertexts=%d", at, node.EncryptionKey, len(node.EncryptedPathSecret))
		for i, ct := range node.EncryptedPathSecret {
			out += fmt.Sprintf(" [%d kem=%x ct=%x]", i, ct.KemOutput, ct.Ciphertext)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// the round trip, as a value rather than as bytes
// ---------------------------------------------------------------------------

// TestUpdatePathRoundTripsAsAWholeValue is the plan's round trip with the comparison it was
// missing.
//
// The plan compared the re-encoded bytes, the node count, the two ciphertext counts and the
// leaf's parent hash. A codec that dropped the kem output from BOTH halves satisfies every one
// of those -- the bytes it does not write it does not read, so the re-encode is exact and every
// count is right -- while losing a field the whole protocol depends on. reflect.DeepEqual
// against the value that went in is what closes that, and it is the only assertion here that
// could not be satisfied by an encoder and a decoder agreeing with each other about the wrong
// thing.
func TestUpdatePathRoundTripsAsAWholeValue(t *testing.T) {
	in := testUpdatePathFixture()
	encoded, err := syntax.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	out := &UpdatePath{}
	if err := syntax.Unmarshal(encoded, out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(out, testUpdatePathDecodedForm(t)) {
		t.Fatalf("the round trip did not preserve the value:\ngot  %s\nwant %s",
			describeUpdatePath(out), describeUpdatePath(testUpdatePathDecodedForm(t)))
	}
	reencoded, err := syntax.Marshal(out)
	if err != nil {
		t.Fatalf("re-Marshal: %v", err)
	}
	if !bytes.Equal(reencoded, encoded) {
		t.Fatalf("re-encode differs:\ngot  %x\nwant %x", reencoded, encoded)
	}
}

// TestANilAndAnEmptyCiphertextVectorAreOneUpdatePathOnTheWire states the one place the Go value
// and the wire disagree, so that a caller comparing two paths as values knows which of the two
// forms a decode answers.
//
// The wire has no representation for "absent", so both encode to the single zero octet; decoding
// answers the empty non nil slice, because syntax.ReadVector never answers nil. A path built by
// hand with a nil vector therefore does not survive a round trip under DeepEqual even though its
// encoding does, which is the same statement leaf_node.go makes about its own vectors.
func TestANilAndAnEmptyCiphertextVectorAreOneUpdatePathOnTheWire(t *testing.T) {
	withNil := testUpdatePathFixture()
	withNil.Nodes[1].EncryptedPathSecret = nil
	withEmpty := testUpdatePathFixture()
	fromNil, err := syntax.Marshal(withNil)
	if err != nil {
		t.Fatalf("Marshal the nil form: %v", err)
	}
	fromEmpty, err := syntax.Marshal(withEmpty)
	if err != nil {
		t.Fatalf("Marshal the empty form: %v", err)
	}
	if !bytes.Equal(fromNil, fromEmpty) {
		t.Fatalf("a nil ciphertext vector encoded to\n %x\nand an empty one to\n %x", fromNil, fromEmpty)
	}
	out := &UpdatePath{}
	if err := syntax.Unmarshal(fromNil, out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.Nodes[1].EncryptedPathSecret == nil {
		t.Fatal("the decode answered a nil ciphertext vector, so an empty vector and an absent one are two values in Go for one encoding")
	}
	// and the whole path, which is where a nil nodes vector would show: a path with no nodes
	// at all is what a commit in a group of one publishes.
	alone := &UpdatePath{LeafNode: testUpdatePathFixture().LeafNode}
	encoded, err := syntax.Marshal(alone)
	if err != nil {
		t.Fatalf("Marshal the single member path: %v", err)
	}
	decoded := &UpdatePath{}
	if err := syntax.Unmarshal(encoded, decoded); err != nil {
		t.Fatalf("Unmarshal the single member path: %v", err)
	}
	if decoded.Nodes == nil || len(decoded.Nodes) != 0 {
		t.Fatalf("a path with no nodes decoded to %v, want an empty non nil vector", decoded.Nodes)
	}
}

// ---------------------------------------------------------------------------
// what the decoder must refuse
// ---------------------------------------------------------------------------

// TestUpdatePathRejectsTrailingBytes is the plan's refusal, plus the truncation sweep it stated
// for one length only.
//
// The trailing byte half is the one that matters most in this package and is worth the sentence:
// MLS signs over serialized forms, so a decoder that accepts a tail accepts two encodings of one
// object, which is a signature bypass primitive rather than a leniency. It is enforced by
// syntax.Unmarshal's join against Done rather than by anything in this file, and this is what
// says so.
//
// The truncation half runs over EVERY prefix of a valid encoding rather than the one the plan
// cut. There are three levels of length prefix here and a truncation inside any one of them is a
// different code path -- a varint that runs out, a declared length past the end of the input, a
// nested region that ends mid element -- and cutting only the last octet exercises exactly one
// of them.
func TestUpdatePathRejectsTrailingBytes(t *testing.T) {
	encoded, err := syntax.Marshal(testUpdatePathFixture())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := syntax.Unmarshal(append(bytes.Clone(encoded), 0x00), &UpdatePath{}); !errors.Is(err, syntax.ErrTrailingBytes) {
		t.Fatalf("trailing byte err = %v, want ErrTrailingBytes", err)
	}
	refused := 0
	for cut := 0; cut < len(encoded); cut += 1 {
		if err := syntax.Unmarshal(encoded[:cut], &UpdatePath{}); err == nil {
			t.Errorf("Unmarshal of the first %d octets of a %d octet path returned no error", cut, len(encoded))
			continue
		}
		refused += 1
	}
	if refused != len(encoded) {
		t.Fatalf("%d of %d truncations were refused", refused, len(encoded))
	}
	t.Logf("%d truncations of a %d octet path all refused", refused, len(encoded))
}

// TestUpdatePathRefusesANestedVectorThatOverrunsItsParentRegion is the failure three levels of
// length prefix make possible and one level does not: an inner vector that declares more octets
// than the region enclosing it holds, while still declaring fewer than the whole input has left.
//
// Those two numbers are the whole test. A decoder that validated a declared length against the
// bytes remaining in the INPUT rather than in the enclosing region accepts this and reads the
// next node's octets as part of this node's ciphertext vector -- a structure the sender never
// sent, assembled out of its own neighbours, and byte exact on re-encode because the decoder
// would write back what it read. The fixture is built so that the two bounds disagree, and the
// disagreement is asserted before the refusal is required: without that, this test passes
// against exactly the decoder it exists to forbid.
func TestUpdatePathRefusesANestedVectorThatOverrunsItsParentRegion(t *testing.T) {
	leaf := handDerivedLeafNodeGolden(LeafNodeSourceCommit)
	first := handDerivedUpdatePathNodeWithCiphertexts()
	second := handDerivedUpdatePathNodeWithNone()

	// the nodes<V> region is cut down to the first node alone -- 199 octets, prefix 40 c7 --
	// and the second node's octets are left in the buffer after it. So the input still has
	// bytes the region does not.
	nodesRegion := handDerivedUpdatePathNodeWithCtSize
	if nodesRegion != 199 {
		t.Fatalf("the first node is %d octets and this fixture's prefix declares 199", nodesRegion)
	}
	// inside that region: 33 octets of encryption_key, then the ciphertext vector's prefix.
	// The vector's true body is 164 octets and the region has 164 left once the key and the
	// prefix are read, so a declared 190 overruns the region by 26 and is still 8 short of
	// what the buffer holds.
	const declared = 190
	const trueBody = 164
	overrun := joinBytes(
		leaf,
		[]byte{0x40, 0xc7},
		first[:33],
		[]byte{0x40, byte(declared)},
		first[35:],
		second,
	)
	if len(overrun) != len(handDerivedUpdatePathGolden()) {
		t.Fatalf("the fixture is %d octets and the golden it was built from is %d, so the surgery moved something",
			len(overrun), len(handDerivedUpdatePathGolden()))
	}
	afterInnerPrefix := len(leaf) + 2 + 33 + 2
	if remainingInInput := len(overrun) - afterInnerPrefix; declared > remainingInInput {
		t.Fatalf("the fixture declares %d octets and the input has %d left, so a decoder bounded by the input alone would refuse this too and the test would prove nothing",
			declared, remainingInInput)
	}
	if declared <= trueBody {
		t.Fatalf("the fixture declares %d octets and the enclosing region has %d left, so nothing is overrun",
			declared, trueBody)
	}
	if err := syntax.Unmarshal(overrun, &UpdatePath{}); !errors.Is(err, syntax.ErrLengthExceedsInput) {
		t.Fatalf("a ciphertext vector declaring %d octets inside a %d octet region gave err = %v, want ErrLengthExceedsInput",
			declared, trueBody, err)
	}
	// the control: the same surgery with the true length restored is a well formed node
	// followed by a tail, so it must be refused for the OTHER reason. Without it a decoder
	// that refused every input this test builds would pass the assertion above.
	honest := joinBytes(leaf, []byte{0x40, 0xc7}, first, second)
	if err := syntax.Unmarshal(honest, &UpdatePath{}); !errors.Is(err, syntax.ErrTrailingBytes) {
		t.Fatalf("the control, whose only fault is the second node sitting outside the nodes region, gave err = %v, want ErrTrailingBytes", err)
	}
}

// TestUpdatePathDecodeIsStagedAndDoesNotWriteThroughAFailure states the two properties
// leaf_node.go argues for, at the level where they are load bearing.
//
// A refused decode must leave the receiver as it found it. UpdatePath decodes a leaf and then a
// vector, and the leaf is the half that carries a signature, an encryption key and a parent
// hash, so a body that wrote the leaf into the receiver before reading the vector hands a caller
// which logged the error rather than returning it a path assembled out of two different
// messages -- one whose leaf came from the attacker's bytes and whose nodes are the previous
// epoch's.
//
// And a receiver decoded into twice must answer the second encoding, not a mixture.
func TestUpdatePathDecodeIsStagedAndDoesNotWriteThroughAFailure(t *testing.T) {
	full, err := syntax.Marshal(testUpdatePathFixture())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	receiver := &UpdatePath{}
	if err := syntax.Unmarshal(full, receiver); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	before := describeUpdatePath(receiver)

	// the truncations are of a DIFFERENT path from the one the receiver already holds, and
	// that is the whole of what makes a write through observable. This test was first
	// written sweeping truncations of the receiver's OWN encoding and it could not fail:
	// LeafNode's own decode is staged, so a short leaf leaves the receiver untouched either
	// way, and once the leaf is complete an unstaged body writes a leaf identical to the one
	// already there. That version was measured against the unstaged body -- decode the leaf
	// into self.LeafNode, then read the vector -- and it passed. So the second path differs
	// in the leaf AND in the first node.
	other := testUpdatePathFixture()
	other.LeafNode.ParentHash = repeatByte(0x55, 32)
	other.Nodes[0].EncryptionKey = HpkePublicKey(repeatByte(0x66, 32))
	otherEncoded, err := syntax.Marshal(other)
	if err != nil {
		t.Fatalf("Marshal the second path: %v", err)
	}
	if describeUpdatePath(other) == before {
		t.Fatal("the two paths describe identically, so one decoded over the other would be invisible here")
	}
	if len(otherEncoded) <= handDerivedUpdatePathLeafSize {
		t.Fatalf("the second path is %d octets and its leaf alone is %d, so no truncation of it reaches the nodes vector -- which is the only place an unstaged body writes a complete leaf and then fails",
			len(otherEncoded), handDerivedUpdatePathLeafSize)
	}

	// every truncation, so that a failure at any of the three levels is covered rather than
	// the one the last octet happens to reach.
	for cut := 0; cut < len(otherEncoded); cut += 1 {
		if err := syntax.Unmarshal(otherEncoded[:cut], receiver); err == nil {
			t.Fatalf("the first %d octets decoded without error", cut)
		}
		if got := describeUpdatePath(receiver); got != before {
			t.Fatalf("a refused decode of the first %d octets of another path wrote through to the receiver:\ngot  %s\nwant %s",
				cut, got, before)
		}
	}

	// the second decode into the same receiver: a path with no nodes at all, which is what a
	// group of one publishes. A body that appended to what it found, or that left the previous
	// path's nodes standing, answers something no encoding describes.
	alone := &UpdatePath{LeafNode: testUpdatePathFixture().LeafNode}
	encoded, err := syntax.Marshal(alone)
	if err != nil {
		t.Fatalf("Marshal the single member path: %v", err)
	}
	if err := syntax.Unmarshal(encoded, receiver); err != nil {
		t.Fatalf("Unmarshal the single member path: %v", err)
	}
	fresh := &UpdatePath{}
	if err := syntax.Unmarshal(encoded, fresh); err != nil {
		t.Fatalf("Unmarshal into a fresh value: %v", err)
	}
	if !reflect.DeepEqual(receiver, fresh) {
		t.Fatalf("a receiver decoded into twice answered\n %s\nand a fresh one answered\n %s",
			describeUpdatePath(receiver), describeUpdatePath(fresh))
	}

	// the same property one level down, which nothing above can observe: syntax.ReadVector
	// builds a fresh HpkeCiphertext for every element, so a ciphertext decode that assigned
	// as it read is invisible through an UpdatePath. It is visible to a caller decoding into
	// a value it already holds, and the framing plan's Welcome is that caller.
	held := &HpkeCiphertext{KemOutput: repeatByte(0x88, 8), Ciphertext: repeatByte(0x99, 8)}
	kept := *held
	one := handDerivedHpkeCiphertext(0x02, 0x03)
	for cut := 0; cut < len(one); cut += 1 {
		if err := syntax.Unmarshal(one[:cut], held); err == nil {
			t.Fatalf("the first %d octets of a ciphertext decoded without error", cut)
		}
		if !reflect.DeepEqual(held, &kept) {
			t.Fatalf("a refused ciphertext decode of the first %d octets wrote through: kem_output %x ciphertext %x, want %x and %x",
				cut, held.KemOutput, held.Ciphertext, kept.KemOutput, kept.Ciphertext)
		}
	}
}

// ---------------------------------------------------------------------------
// the seal and open adaptation
// ---------------------------------------------------------------------------

// TestSealAndOpenWithLabelRoundTrip is the plan's round trip: the pair agrees with itself, and
// neither a different context nor a different label opens what was sealed.
//
// The two refusals are the point rather than the round trip. A label and a context that were
// dropped on the way into the HPKE info still produce a ciphertext this same pair opens, so the
// only thing that can see the omission is a message sealed under one and opened under the other.
func TestSealAndOpenWithLabelRoundTrip(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	priv, pub, err := crypto.DeriveKeyPair(crypto.Random(crypto.HashSize()))
	if err != nil {
		t.Fatalf("DeriveKeyPair: %v", err)
	}
	ct, err := SealWithLabel(crypto, pub, "Welcome", []byte("context"), []byte("secret"))
	if err != nil {
		t.Fatalf("SealWithLabel: %v", err)
	}
	if len(ct.KemOutput) == 0 || len(ct.Ciphertext) == 0 {
		t.Fatalf("SealWithLabel produced an empty ciphertext")
	}
	got, err := OpenWithLabel(crypto, priv, "Welcome", []byte("context"), ct)
	if err != nil {
		t.Fatalf("OpenWithLabel: %v", err)
	}
	if !bytes.Equal(got, []byte("secret")) {
		t.Fatalf("OpenWithLabel = %q, want %q", got, "secret")
	}
	if _, err := OpenWithLabel(crypto, priv, "Welcome", []byte("other"), ct); err == nil {
		t.Fatalf("the ciphertext opened under a different context")
	}
	if _, err := OpenWithLabel(crypto, priv, "GroupSecrets", []byte("context"), ct); err == nil {
		t.Fatalf("the ciphertext opened under a different label")
	}
	// the two halves of the structure are not interchangeable, which is the failure a
	// transposed adaptation produces and which the round trip above cannot see: it would seal
	// and open transposed values consistently.
	swapped := &HpkeCiphertext{KemOutput: ct.Ciphertext, Ciphertext: ct.KemOutput}
	if _, err := OpenWithLabel(crypto, priv, "Welcome", []byte("context"), swapped); err == nil {
		t.Fatal("a ciphertext whose two fields were exchanged still opened")
	}
	if _, err := OpenWithLabel(crypto, priv, "Welcome", []byte("context"), nil); !errors.Is(err, errNilHpkeCiphertext) {
		t.Fatalf("OpenWithLabel(nil ciphertext) = %v, want errNilHpkeCiphertext", err)
	}
}

// TestSealWithLabelPutsTheKemOutputAndTheCiphertextInTheirOwnFields is what separates the
// adaptation from a transposed one.
//
// EncryptWithLabel answers (kemOutput, ciphertext) and both are []byte, so a transposed
// assignment compiles and round trips through this same pair of functions perfectly. The
// LENGTHS are what tell them apart: RFC 9180 fixes the encapsulated key at the suite's Nenc, and
// the ciphertext is the plaintext plus the suite's Nt. Both numbers are read from the suite
// registry rather than written here, so a suite whose KEM or AEAD differs is covered by this on
// the commit that registers it.
func TestSealWithLabelPutsTheKemOutputAndTheCiphertextInTheirOwnFields(t *testing.T) {
	for _, suite := range registeredSuitesForUpdatePath(t) {
		params, err := LookupSuite(suite)
		if err != nil {
			t.Fatalf("LookupSuite(%d): %v", suite, err)
		}
		crypto := mustProvider(t, suite)
		_, pub, err := crypto.DeriveKeyPair(crypto.Random(crypto.HashSize()))
		if err != nil {
			t.Fatalf("DeriveKeyPair: %v", err)
		}
		// a plaintext whose length is neither Nenc nor Nenc plus Nt, so that the two fields
		// cannot be told apart by accident of the fixture.
		plaintext := repeatByte(0x77, 7)
		ct, err := SealWithLabel(crypto, pub, "Welcome", []byte("context"), plaintext)
		if err != nil {
			t.Fatalf("SealWithLabel: %v", err)
		}
		if len(ct.KemOutput) != params.Nenc {
			t.Errorf("%s: kem_output is %d octets, and the suite's Nenc is %d; the two results are transposed",
				params.Name, len(ct.KemOutput), params.Nenc)
		}
		if want := len(plaintext) + params.Nt; len(ct.Ciphertext) != want {
			t.Errorf("%s: ciphertext is %d octets, and the plaintext plus the suite's Nt is %d",
				params.Name, len(ct.Ciphertext), want)
		}
	}
}

// registeredSuitesForUpdatePath is every ciphersuite this package registers, derived off the
// registry rather than listed, so a third suite is swept by the sizes test above on the commit
// that registers it.
func registeredSuitesForUpdatePath(t *testing.T) []CipherSuite {
	t.Helper()
	suites := []CipherSuite{}
	for _, value := range sortedValues(registryConstantsOfType(t, "CipherSuite")) {
		suite := CipherSuite(value)
		if _, err := LookupSuite(suite); err != nil {
			continue
		}
		suites = append(suites, suite)
	}
	if len(suites) == 0 {
		t.Fatal("no registered ciphersuite was derived, so the sweep over them runs over nothing")
	}
	return suites
}

// ---------------------------------------------------------------------------
// the argument rule crypto_test.go's provider gates need for this file's structure
// ---------------------------------------------------------------------------

// providerHpkeCiphertextPerturbations moves an *HpkeCiphertext argument, one byte of one
// field at a time, for the provider gates in crypto_test.go.
//
// The rule lives beside the structure it moves rather than in that file's switch, which is
// where psk_test.go and leaf_node_test.go keep theirs: those gates walk every declaration of
// the package, and a structure declared here whose perturbation is written there is one whose
// rule stops matching when a field is added and says nothing about it.
//
// BOTH fields are moved, and that is the point rather than thoroughness. OpenWithLabel hands
// its two fields to two different parameters of DecryptWithLabel, so an adaptation that passed
// the kem output twice, or passed a constant for one of them, answers a refusal for every
// input and moves under one of these perturbations and not the other -- and a rule that moved
// only the first field would report the second as observed having never changed it.
//
// Each perturbation is a fresh structure over fresh arrays, so a moved value cannot write
// through into the base argument every other row of those gates is built from.
func providerHpkeCiphertextPerturbations(t *testing.T, operation string,
	parameter providerParameter, argument reflect.Value) []providerPerturbation {
	t.Helper()
	base := argument.Interface().(*HpkeCiphertext)
	moved := []providerPerturbation{}
	for _, field := range []struct {
		name string
		at   func(ct *HpkeCiphertext) *[]byte
	}{
		{name: "kem_output", at: func(ct *HpkeCiphertext) *[]byte { return &ct.KemOutput }},
		{name: "ciphertext", at: func(ct *HpkeCiphertext) *[]byte { return &ct.Ciphertext }},
	} {
		length := len(*field.at(base))
		if length == 0 {
			t.Fatalf("the base argument for %s.%s carries no %s, so perturbing that field changes nothing",
				operation, parameter.name, field.name)
		}
		for _, position := range perturbedPositions(length) {
			perturbed := &HpkeCiphertext{
				KemOutput:  bytes.Clone(base.KemOutput),
				Ciphertext: bytes.Clone(base.Ciphertext),
			}
			(*field.at(perturbed))[position] ^= 0xff
			moved = append(moved, providerPerturbation{
				where: fmt.Sprintf("byte %d of %d of the %s", position, length, field.name),
				value: reflect.ValueOf(perturbed),
			})
		}
	}
	if len(moved) == 0 {
		t.Fatalf("no perturbation was built for %s.%s, so that argument is called twice with one value and reported as observed",
			operation, parameter.name)
	}
	return moved
}

// ---------------------------------------------------------------------------
// the published corpus
// ---------------------------------------------------------------------------

// treekemUpdatePathVector is the other half of one entry of treekem.json: the tree an epoch
// started from, and the UpdatePath each member would publish committing over it.
//
// This is the only oracle in this file that is independent of this repository. Everything above
// compares this codec against a derivation the same reader wrote, which separates a swapped
// field order from a correct one but cannot separate this whole family of structures from a
// consistent misreading of section 7.6. The working group's implementations produced these
// octets.
type treekemUpdatePathVector struct {
	CipherSuite uint16                       `json:"cipher_suite"`
	RatchetTree string                       `json:"ratchet_tree"`
	UpdatePaths []treekemPublishedUpdatePath `json:"update_paths"`
}

type treekemPublishedUpdatePath struct {
	Sender     uint32 `json:"sender"`
	UpdatePath string `json:"update_path"`
}

// Transcriptions of what testdata/vectors/treekem.json holds at the pinned mlswg commit, for the
// suites this package implements. A decoder that stopped early, a filter that matched nothing and
// a loop that read a field that is not there all report exactly what a complete run reports
// without these.
const (
	treekemUpdatePathCount         = 124
	treekemUpdatePathNodeCount     = 330
	treekemUpdatePathCiphertexts   = 356
	treekemUpdatePathDeepNodeCount = 26
)

// TestEveryPublishedUpdatePathDecodesAndReEncodesExactly is the corpus half of the codec: 124
// UpdatePaths produced by other implementations, each decoded and re-encoded to the same octets.
//
// It catches the family the goldens above cannot: a prefix width, a field order or a nesting
// this reader transcribed consistently wrongly in both the code and the derivation. It does NOT
// catch a symmetric swap or a dropped field -- the comparison is against what this
// implementation re-encoded, so both are byte exact here -- which is why it is the third test of
// this codec and not the first.
//
// The two negative controls are what make the green run mean something: every published case
// agrees, so a decoder that accepted everything and one that checked everything produce
// identical runs, and only an input that must be refused separates them.
func TestEveryPublishedUpdatePathDecodesAndReEncodesExactly(t *testing.T) {
	paths, nodes, ciphertexts, deep := 0, 0, 0, 0
	forEachPublishedUpdatePath(t, func(at int, tree *RatchetTree, published treekemPublishedUpdatePath, raw []byte) {
		paths += 1
		decoded := &UpdatePath{}
		if err := syntax.Unmarshal(raw, decoded); err != nil {
			t.Fatalf("entry %d, sender %d: Unmarshal a published update path: %v", at, published.Sender, err)
		}
		reencoded, err := syntax.Marshal(decoded)
		if err != nil {
			t.Fatalf("entry %d, sender %d: Marshal: %v", at, published.Sender, err)
		}
		if !bytes.Equal(reencoded, raw) {
			t.Fatalf("entry %d, sender %d: re-encoding a published update path produced\n %x\nwant\n %x",
				at, published.Sender, reencoded, raw)
		}
		nodes += len(decoded.Nodes)
		for _, node := range decoded.Nodes {
			ciphertexts += len(node.EncryptedPathSecret)
			if len(node.EncryptedPathSecret) > 1 {
				deep += 1
			}
		}
		// the controls, per case rather than once. A tail must be refused, and so must the
		// same encoding one octet short; a decoder that answered nil for both satisfies every
		// line above.
		if err := syntax.Unmarshal(append(bytes.Clone(raw), 0x00), &UpdatePath{}); !errors.Is(err, syntax.ErrTrailingBytes) {
			t.Fatalf("entry %d, sender %d: a published path with one octet appended gave err = %v, want ErrTrailingBytes",
				at, published.Sender, err)
		}
		if err := syntax.Unmarshal(raw[:len(raw)-1], &UpdatePath{}); err == nil {
			t.Fatalf("entry %d, sender %d: a published path one octet short decoded without error",
				at, published.Sender)
		}
	})
	if paths != treekemUpdatePathCount || nodes != treekemUpdatePathNodeCount ||
		ciphertexts != treekemUpdatePathCiphertexts || deep != treekemUpdatePathDeepNodeCount {
		t.Fatalf("the run read %d published paths, %d nodes, %d ciphertexts and %d nodes carrying more than one ciphertext; want %d, %d, %d and %d",
			paths, nodes, ciphertexts, deep,
			treekemUpdatePathCount, treekemUpdatePathNodeCount, treekemUpdatePathCiphertexts, treekemUpdatePathDeepNodeCount)
	}
	t.Logf("%d published update paths, %d nodes, %d ciphertexts, %d nodes sealing to more than one resolution node",
		paths, nodes, ciphertexts, deep)
}

// TestEveryPublishedUpdatePathHasOneCiphertextPerResolutionNode states the relation this codec
// deliberately does not enforce, against the corpus, so that the layer which will enforce it and
// the layer which produces it agree about what it IS.
//
// Section 7.6 gives an UpdatePath one node per step of the sender's filtered direct path and one
// ciphertext per node of the resolution of that step's copath child. Neither number is available
// to a codec -- both are properties of a tree at an epoch -- so a path whose counts are wrong is
// perfectly well formed on the wire and is decoded here without complaint. The check belongs to
// the layer holding the tree, which is task 22's decrypt, and it is written down in this file
// because a cross-layer check both sides assume the other makes is a check nobody makes.
//
// Both numbers are recomputed from the published ratchet tree by this package's own tree walk,
// so this is also the first thing that compares that walk against another implementation's
// choice of resolution rather than against itself.
func TestEveryPublishedUpdatePathHasOneCiphertextPerResolutionNode(t *testing.T) {
	paths, checked, separated := 0, 0, 0
	forEachPublishedUpdatePath(t, func(at int, tree *RatchetTree, published treekemPublishedUpdatePath, raw []byte) {
		paths += 1
		decoded := &UpdatePath{}
		if err := syntax.Unmarshal(raw, decoded); err != nil {
			t.Fatalf("entry %d, sender %d: Unmarshal: %v", at, published.Sender, err)
		}
		targets, err := tree.EncryptionTargets(LeafIndex(published.Sender), nil)
		if err != nil {
			t.Fatalf("entry %d, sender %d: EncryptionTargets: %v", at, published.Sender, err)
		}
		if len(targets) != len(decoded.Nodes) {
			t.Fatalf("entry %d, sender %d: the published path carries %d nodes and the filtered direct path of that sender has %d steps",
				at, published.Sender, len(decoded.Nodes), len(targets))
		}
		for i := range targets {
			if len(targets[i]) != len(decoded.Nodes[i].EncryptedPathSecret) {
				t.Fatalf("entry %d, sender %d, node %d: the published node carries %d ciphertexts and the resolution it was built for holds %d nodes",
					at, published.Sender, i, len(decoded.Nodes[i].EncryptedPathSecret), len(targets[i]))
			}
			checked += 1
		}
		// the negative control, per case rather than once. Every published path agrees with
		// the resolution, so a comparison that read a count off the decoded path on BOTH
		// sides -- or that compared two numbers which are equal for every tree in this file
		// -- produces exactly the run a correct one produces. A resolution computed with one
		// leaf excluded is a DIFFERENT set of targets, and somewhere in the corpus it has to
		// disagree with what the sender published, or the comparison above is reading
		// something that cannot vary.
		wrong, err := tree.EncryptionTargets(LeafIndex(published.Sender), []LeafIndex{0})
		if err != nil {
			t.Fatalf("entry %d, sender %d: EncryptionTargets with an exclusion: %v", at, published.Sender, err)
		}
		for i := range wrong {
			if len(wrong[i]) != len(decoded.Nodes[i].EncryptedPathSecret) {
				separated += 1
			}
		}
	})
	if paths != treekemUpdatePathCount || checked != treekemUpdatePathNodeCount {
		t.Fatalf("the run compared %d published paths and %d nodes against the tree; want %d and %d",
			paths, checked, treekemUpdatePathCount, treekemUpdatePathNodeCount)
	}
	if separated == 0 {
		t.Fatal("no published node's ciphertext count differs from the resolution taken with one leaf excluded, so the comparison above is between two numbers that agree whatever the tree says")
	}
	t.Logf("%d published paths, %d nodes whose ciphertext count is the resolution size the tree gives, %d of them separated by the excluded-leaf control",
		paths, checked, separated)
}

// TestEveryPublishedUpdatePathCarriesACommitSourceLeaf is the SECOND cross layer relation at
// this boundary, stated the way the ciphertext count above is and for the same reason.
//
// RFC 9420 section 7.6 requires the leaf_node of an UpdatePath to carry leaf_node_source =
// commit, which is what makes it carry a parent_hash at all -- the parent hash the receiver
// recomputes the path against. The codec cannot enforce it: it decodes whatever leaf the leaf
// codec accepts, and a key_package source leaf inside an UpdatePath decodes here without a
// complaint, which is the first control below. Until this test the relation was NAMED nowhere --
// the type comment on UpdatePathNode names the ciphertext count as the deliberately unenforced
// one and this was the other, unwritten, which is the shape the ciphertext paragraph itself
// warns about.
//
// It differs from the ciphertext count in one way worth writing down: the door is
// ValidateUpdatePathLeafNode, and the second control drives THAT rather than a context this test
// wrote for itself, over every source the package declares.
//
// THAT PARAGRAPH USED TO SAY THE DOOR WAS ALREADY BUILT, and it was not. What was built was the
// validator's ability to hold an expectation -- LeafValidationContext.ExpectedSource -- with no
// production caller setting it to commit anywhere in this package, so this control demonstrated
// only that a test could ask the question. A test that constructs the caller it is checking for
// is a test of the callee, and the gap it reported closed was still open: the leaf of a received
// UpdatePath reached MergeUpdatePath and the tree with nothing having judged it.
func TestEveryPublishedUpdatePathCarriesACommitSourceLeaf(t *testing.T) {
	leaves := 0
	forEachPublishedUpdatePath(t, func(at int, tree *RatchetTree, published treekemPublishedUpdatePath, raw []byte) {
		decoded := &UpdatePath{}
		if err := syntax.Unmarshal(raw, decoded); err != nil {
			t.Fatalf("entry %d, sender %d: Unmarshal: %v", at, published.Sender, err)
		}
		if decoded.LeafNode.LeafNodeSource != LeafNodeSourceCommit {
			t.Fatalf("entry %d, sender %d: the published path's leaf carries source %d and section 7.6 requires commit, which is %d",
				at, published.Sender, decoded.LeafNode.LeafNodeSource, LeafNodeSourceCommit)
		}
		// and the field that source exists to carry, which is the half a receiver reads: a
		// leaf claiming the commit source with no parent hash is a path whose chain cannot be
		// recomputed against anything.
		if len(decoded.LeafNode.ParentHash) == 0 {
			t.Fatalf("entry %d, sender %d: the published path's leaf is commit sourced and carries no parent hash",
				at, published.Sender)
		}
		leaves += 1
	})
	if leaves != treekemUpdatePathCount {
		t.Fatalf("the run read the leaf of %d published paths, want %d", leaves, treekemUpdatePathCount)
	}

	// the first control: this layer states the relation and does NOT enforce it. Every
	// published leaf agrees with section 7.6, so a decoder that checked the source and one
	// that never looked at it produce identical runs above, and only a leaf that must be
	// refused somewhere separates them -- here it is accepted, which is what says the check
	// belongs to a layer holding an expectation rather than to the codec.
	lenient := testUpdatePathFixture()
	lenient.LeafNode = *testLeafNodeOfSource(LeafNodeSourceKeyPackage)
	encoded, err := syntax.Marshal(lenient)
	if err != nil {
		t.Fatalf("Marshal a path whose leaf carries the key_package source: %v", err)
	}
	roundTripped := &UpdatePath{}
	if err := syntax.Unmarshal(encoded, roundTripped); err != nil {
		t.Fatalf("a path whose leaf carries the key_package source was refused by the codec: %v", err)
	}
	if roundTripped.LeafNode.LeafNodeSource != LeafNodeSourceKeyPackage {
		t.Fatalf("the decode answered source %d, so the control did not carry the source it was built to carry",
			roundTripped.LeafNode.LeafNodeSource)
	}

	// the second control: the production door that does enforce it, called the way a client
	// processing a commit calls it. Over every source the package declares rather than over the
	// one wrong source somebody would have written, so a fourth source is refused by this on the
	// commit that declares it.
	crypto := leafValidationCrypto(t)
	accepted, refused := 0, 0
	for _, source := range leafNodeSources(t) {
		leaf := leafValidationSignedLeaf(t, crypto, source, nil)
		err := ValidateUpdatePathLeafNode(crypto, commitDoorGroupContext(),
			leafValidationLeafIndex, commitDoorPathCarrying(leaf))
		if source == LeafNodeSourceCommit {
			if err != nil {
				t.Errorf("the commit sourced leaf was refused at the update path door: %v", err)
			}
			accepted += 1
			continue
		}
		if !errors.Is(err, ErrLeafNodeSourceMismatch) {
			t.Errorf("a %d sourced leaf judged at the update path door gave err = %v, want ErrLeafNodeSourceMismatch",
				source, err)
		}
		if !errors.Is(err, errUpdatePathLeafNodeInvalid) {
			t.Errorf("a %d sourced leaf judged at the update path door gave err = %v, which does not answer to errUpdatePathLeafNodeInvalid; a caller cannot tell which half of a commit it must reject",
				source, err)
		}
		refused += 1
	}
	if accepted != 1 || refused != len(leafNodeSources(t))-1 {
		t.Fatalf("the expectation this position takes accepted %d leaves and refused %d, over the %d sources this package declares",
			accepted, refused, len(leafNodeSources(t)))
	}
	t.Logf("%d published paths carry a commit sourced leaf with a parent hash; the codec accepts a key_package sourced one and ValidateUpdatePathLeafNode refuses it",
		leaves)
}

// forEachPublishedUpdatePath is the shared walk of treekem.json's update_paths half, so that the
// two properties above run over provably the same set rather than over two filters that could
// drift.
//
// Entries at a suite this package does not implement are skipped rather than failed, matching
// TestEveryPublishedPathSecretDerivesTheNodeKeyItsRatchetTreeCarries above; the counts each
// caller asserts are what stop the skip from swallowing the file.
func forEachPublishedUpdatePath(t *testing.T, visit func(at int, tree *RatchetTree,
	published treekemPublishedUpdatePath, raw []byte)) {
	t.Helper()
	entries := LoadVectorFile(t, "treekem.json")
	if len(entries) != treekemEntryCount {
		t.Fatalf("treekem.json holds %d entries, want %d", len(entries), treekemEntryCount)
	}
	matched, declined := 0, 0
	for at, raw := range entries {
		var vector treekemUpdatePathVector
		if err := json.Unmarshal(raw, &vector); err != nil {
			t.Fatalf("entry %d: %v", at, err)
		}
		if _, ok := implementedSuite(vector.CipherSuite); !ok {
			declined += 1
			continue
		}
		matched += 1
		tree, err := UnmarshalRatchetTree(MustHex(t, vector.RatchetTree))
		if err != nil {
			t.Fatalf("entry %d: UnmarshalRatchetTree: %v", at, err)
		}
		for _, published := range vector.UpdatePaths {
			visit(at, tree, published, MustHex(t, published.UpdatePath))
		}
	}
	if matched+declined != len(entries) {
		t.Fatalf("%d entries matched and %d were declined, and the file holds %d",
			matched, declined, len(entries))
	}
	if matched == 0 {
		t.Fatal("no entry of treekem.json is at a suite this package implements, so the sweep ran over nothing")
	}
}

// ---------------------------------------------------------------------------
// the three properties this file's own level by level tests did not reach
// ---------------------------------------------------------------------------

// Each of the three below was measured against the code it forbids before it was kept: the
// mutation was applied, the test was watched failing, and the mutation was reverted.

// TestAnUpdatePathEncodeAnswersExactlyWhatItsLeafEncoderAnswers is the propagation the whole
// three level encoder rests on, and nothing here observed it.
//
// UpdatePath.MarshalMLS is the only encoder in this file that calls a FALLIBLE one. The leaf's
// refuses a source it has no variant for as a RETURNED error rather than by latching the Writer,
// so a body that dropped it -- `_ = self.LeafNode.MarshalMLS(w)` -- still produces bytes:
// syntax.MarshalLimit joins the encoder's refusal with the Writer's, the Writer never latched
// anything, the join is nil, and the caller is handed a path whose leaf carries a source octet
// and no variant at all. That is a structure every other implementation reads differently, and
// it is the octets a commit's confirmation tag is then taken over.
//
// The class is the WHOLE octet space of the source field and the verdict on each member is the
// leaf encoder's own answer, never a list of sources written here. A fourth source moves from
// the refused side of this sweep to the accepted side on the commit that declares it, and the
// accepted side is compared against the package's declared constants rather than against a
// count, so a source declared and not implemented fails here too.
func TestAnUpdatePathEncodeAnswersExactlyWhatItsLeafEncoderAnswers(t *testing.T) {
	nodes := testUpdatePathFixture().Nodes
	accepted := []LeafNodeSource{}
	refused := 0
	for octet := 0; octet <= 0xff; octet += 1 {
		source := LeafNodeSource(octet)
		leaf := testLeafNodeOfSource(source)
		leafBytes, leafErr := syntax.Marshal(leaf)
		path := &UpdatePath{LeafNode: *testLeafNodeOfSource(source), Nodes: nodes}
		pathBytes, pathErr := syntax.Marshal(path)
		if leafErr != nil {
			refused += 1
			if pathErr == nil {
				t.Errorf("source %d: the leaf encoder refused it with %v and the path encoded %d octets and no error, so a path publishes a leaf its own encoder would not write",
					octet, leafErr, len(pathBytes))
				continue
			}
			// the SAME failure and not merely a failure. Compared against the leaf's own
			// answer rather than against a sentinel named here, so a leaf that learns a
			// second refusal is covered by this on the commit that adds it, and a path
			// encoder that answered an error of its own -- sending the caller to look at
			// the half of the structure that is not the problem -- is reported.
			if pathErr.Error() != leafErr.Error() {
				t.Errorf("source %d: the leaf encoder answered %v and the path answered %v",
					octet, leafErr, pathErr)
			}
			if pathBytes != nil {
				t.Errorf("source %d: the path refused and still answered %d octets", octet, len(pathBytes))
			}
			continue
		}
		accepted = append(accepted, source)
		if pathErr != nil {
			t.Errorf("source %d: the leaf encoded and the path refused it with %v", octet, pathErr)
			continue
		}
		// and the accepted half is not vacuous either: the octets the path published for the
		// leaf are the leaf's own encoding, so a path that swallowed the refusal by encoding
		// something else in the leaf's place is reported here rather than passing as a leaf
		// that happened to encode.
		if !bytes.HasPrefix(pathBytes, leafBytes) {
			t.Errorf("source %d: the path's first %d octets are not the leaf's own encoding",
				octet, len(leafBytes))
		}
	}
	// both outcomes must occur, or every branch above holds over an empty set: a leaf encoder
	// that refused everything satisfies this test exactly as one that refused nothing does.
	if want := leafNodeSources(t); !slices.Equal(accepted, want) {
		t.Fatalf("the leaf encoder accepted the source octets %v and this package declares %v",
			accepted, want)
	}
	if refused != 0x100-len(accepted) {
		t.Fatalf("%d source octets were refused and %d accepted, and the space is %d",
			refused, len(accepted), 0x100)
	}
	t.Logf("%d of 256 source octets refused by both encoders, %d accepted by both", refused, len(accepted))
}

// TestOpenWithLabelMakesOneAttemptUnderTheCallersOwnParametersAndNoOther is the "no fallback
// info" rule, at the layer whose comment claims it holds by construction.
//
// crypto_labels.go argues the rule and crypto_labels_test.go walks it for DecryptWithLabel. This
// function forwards to that one, and the comment on it says the rule therefore holds here by
// construction -- a construction claim with nothing that fails when the construction changes. An
// OpenWithLabel that retried a failed open under any other parameters opens a ciphertext sealed
// for a purpose its caller never named, and the round trip beside this cannot see it: that test
// seals under the REAL parameters and opens under wrong ones, so a fallback attempt fails too
// and it still passes.
//
// Two halves, because either alone leaves a fallback standing. The BEHAVIOURAL half is the whole
// cross product of crypto_labels_test.go's own probe pairs -- a ciphertext sealed under one pair
// must open under exactly the pairs framing to the same info and under no other -- which is a
// derived class rather than the one retry somebody thought of, and it holds against a fallback
// to the empty label, to the bare label, to any pair in the class. The STRUCTURAL half is the
// count of calls the provider saw: an open reaches HPKE exactly once whether it succeeded or
// failed, which is what holds against a fallback to parameters outside the class entirely.
func TestOpenWithLabelMakesOneAttemptUnderTheCallersOwnParametersAndNoOther(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	priv, pub, err := crypto.DeriveKeyPair(crypto.Random(crypto.HashSize()))
	if err != nil {
		t.Fatalf("DeriveKeyPair: %v", err)
	}
	plaintext := []byte("the sealed secret")
	pairs := signatureProbePairs()
	if len(pairs) < 2 {
		t.Fatalf("the probe class holds %d pairs, so no ciphertext is ever opened under another pair's parameters", len(pairs))
	}
	opened := 0
	refused := 0
	for _, sealed := range pairs {
		ct, err := SealWithLabel(crypto, pub, sealed.label, sealed.content, plaintext)
		if err != nil {
			t.Fatalf("SealWithLabel under %s: %v", sealed.name, err)
		}
		for _, opening := range pairs {
			// which pairs are the same OPEN is decided by the info they frame to and not by
			// the pair's name: two pairs that framed to one info are one parameter set, and
			// a test that called them different would demand a refusal no correct open can
			// give.
			same := bytes.Equal(mlsEncryptContext(sealed.label, sealed.content),
				mlsEncryptContext(opening.label, opening.content))
			counter := &faultInjectingProvider{CryptoProvider: crypto}
			got, err := OpenWithLabel(counter, priv, opening.label, opening.content, ct)
			if same {
				opened += 1
				if err != nil {
					t.Errorf("sealed under %s, opened under %s, which frames to the same info: %v",
						sealed.name, opening.name, err)
				} else if !bytes.Equal(got, plaintext) {
					t.Errorf("sealed under %s, opened under %s: got %x, want %x",
						sealed.name, opening.name, got, plaintext)
				}
			} else {
				refused += 1
				if err == nil {
					t.Errorf("a ciphertext sealed under %s opened under %s, so the open falls back to parameters its caller never asked for",
						sealed.name, opening.name)
				}
				// and never a plaintext beside the error, which is guardrail 7's half of
				// this: a caller reading the slice rather than the error would take it.
				if got != nil {
					t.Errorf("sealed under %s, opened under %s: refused with %v and still answered %x",
						sealed.name, opening.name, err, got)
				}
			}
			if counter.calls != 1 {
				t.Errorf("sealed under %s, opened under %s: the provider saw %d fallible calls, want exactly the one HpkeOpen -- a second attempt is a fallback whatever it retries under",
					sealed.name, opening.name, counter.calls)
			}
		}
	}
	if opened == 0 || refused == 0 {
		t.Fatalf("%d opens under the sealing parameters and %d under another pair's; both have to occur or this test forbids nothing",
			opened, refused)
	}
	t.Logf("%d pairs: %d opens under the sealing info, %d refusals under another", len(pairs), opened, refused)
}

// ---------------------------------------------------------------------------
// the staged decode, over every codec this file declares rather than two of them
// ---------------------------------------------------------------------------

// stagedDecodeProbe is one codec's pair of values for the sweep below.
//
// held is what a receiver already carries; other is a DIFFERENT value whose truncations are
// decoded into it. The two must differ inside the FIRST field, because that is the only place a
// body which assigned as it read can be caught: a difference in the last field is written only
// by a decode that reached the end and succeeded.
type stagedDecodeProbe struct {
	held  syntax.Codec
	other syntax.Codec
	// firstFieldOctets is the cut at which other's first field is complete and its second has
	// not started. It is a claim about the ENCODING, checked below against both encodings
	// rather than trusted, and it is what makes the sweep contain the one cut where an
	// unstaged body has written the first field and is about to fail.
	firstFieldOctets int
}

// The probes, one per codec treekem.go pins. The KEYS are held to those pins by the sweep, so a
// fourth codec declared in this file fails there until it has one here.
func updatePathStagedDecodeProbes() map[string]stagedDecodeProbe {
	otherPath := testUpdatePathFixture()
	otherPath.LeafNode.ParentHash = repeatByte(0x55, 32)
	otherNode := testUpdatePathFixture().Nodes[0]
	return map[string]stagedDecodeProbe{
		// 0x20 and thirty two octets of kem_output, and then the ciphertext's own prefix.
		"HpkeCiphertext": {
			held:             &HpkeCiphertext{KemOutput: repeatByte(0x88, 32), Ciphertext: repeatByte(0x99, 48)},
			other:            &HpkeCiphertext{KemOutput: repeatByte(0x02, 32), Ciphertext: repeatByte(0x03, 48)},
			firstFieldOctets: 33,
		},
		// 0x20 and thirty two octets of encryption_key, and then encrypted_path_secret's.
		// This is the level nothing reached: syntax.ReadVector builds a fresh UpdatePathNode
		// per element exactly as it builds a fresh HpkeCiphertext, so an unstaged node decode
		// is invisible through an UpdatePath and visible only to a caller decoding into a
		// node it already holds. Task 22's decrypt is that caller.
		"UpdatePathNode": {
			held: &UpdatePathNode{
				EncryptionKey:       HpkePublicKey(repeatByte(0x88, 32)),
				EncryptedPathSecret: []HpkeCiphertext{{KemOutput: repeatByte(0x99, 32), Ciphertext: repeatByte(0xaa, 48)}},
			},
			other:            &otherNode,
			firstFieldOctets: 33,
		},
		// the leaf, whose own derivation counts its octets in this file.
		"UpdatePath": {
			held:             testUpdatePathFixture(),
			other:            otherPath,
			firstFieldOctets: handDerivedUpdatePathLeafSize,
		},
	}
}

// TestEveryCodecThisFileDeclaresStagesItsDecode is the staged decode property over the class
// this file DECLARES rather than over the levels a test happened to reach.
//
// The property is leaf_node.go's: a refused decode leaves the receiver exactly as it found it.
// It was already stated here for UpdatePath and for HpkeCiphertext -- the outer level and the
// inner one -- and UpdatePathNode, the level between them, was covered by neither. Moving its
// assignment above the ReadVector it precedes changed nothing any test could see, and the
// argument for why the inner level needed its own sweep applies to it word for word: ReadVector
// builds a fresh element per iteration, so nothing decoded through a container observes it.
//
// So the class is read off treekem.go's own `var _ syntax.Codec` pins instead of being written
// out, and the probe table is required to match it in BOTH directions. A codec declared here
// without a probe fails; a probe for a codec no longer declared fails too.
func TestEveryCodecThisFileDeclaresStagesItsDecode(t *testing.T) {
	pinned := codecTypesPinnedIn(t, "treekem.go", nil)
	if len(pinned) == 0 {
		t.Fatal("no syntax.Codec pin was read out of treekem.go, so this sweep would run over nothing")
	}
	probes := updatePathStagedDecodeProbes()
	if covered := slices.Sorted(maps.Keys(probes)); !slices.Equal(covered, pinned) {
		t.Fatalf("treekem.go pins %v as codecs and the staged decode probes cover %v", pinned, covered)
	}
	for _, name := range pinned {
		probe := probes[name]
		heldEncoded, err := syntax.Marshal(probe.held)
		if err != nil {
			t.Fatalf("%s: Marshal the held value: %v", name, err)
		}
		otherEncoded, err := syntax.Marshal(probe.other)
		if err != nil {
			t.Fatalf("%s: Marshal the other value: %v", name, err)
		}
		// the four controls, before anything is required of the decoder. Without them this
		// sweep passes against exactly the body it exists to forbid, which is how its
		// predecessor was first written.
		if reflect.DeepEqual(probe.held, probe.other) {
			t.Fatalf("%s: the two probe values are equal, so a decode writing one over the other is invisible here", name)
		}
		if probe.firstFieldOctets <= 0 || probe.firstFieldOctets >= len(otherEncoded) {
			t.Fatalf("%s: the first field is said to end at octet %d of a %d octet encoding, so the cut where an unstaged body has written it is outside the sweep",
				name, probe.firstFieldOctets, len(otherEncoded))
		}
		if len(heldEncoded) < probe.firstFieldOctets {
			t.Fatalf("%s: the held value encodes to %d octets and the first field is said to be %d",
				name, len(heldEncoded), probe.firstFieldOctets)
		}
		if bytes.Equal(heldEncoded[:probe.firstFieldOctets], otherEncoded[:probe.firstFieldOctets]) {
			t.Fatalf("%s: the two probes agree over the whole first field, so a body that assigned it before reading on would write a value equal to the one already there",
				name)
		}

		kept := reflect.ValueOf(probe.held).Elem().Interface()
		for cut := 0; cut < len(otherEncoded); cut += 1 {
			if err := syntax.Unmarshal(otherEncoded[:cut], probe.held); err == nil {
				t.Fatalf("%s: the first %d octets of a %d octet encoding decoded without error",
					name, cut, len(otherEncoded))
			}
			if now := reflect.ValueOf(probe.held).Elem().Interface(); !reflect.DeepEqual(now, kept) {
				t.Fatalf("%s: a refused decode of the first %d octets wrote through to the receiver:\ngot  %+v\nwant %+v",
					name, cut, now, kept)
			}
		}

		// the positive control on the whole sweep: these bytes ARE acceptable to this
		// decoder, so the refusals above are the truncation and not a value it would never
		// have taken. And the receiver decoded into answers what a fresh one answers, which
		// is the second half of the discipline -- a receiver decoded into twice must answer
		// the second encoding and not a mixture of both.
		if err := syntax.Unmarshal(otherEncoded, probe.held); err != nil {
			t.Fatalf("%s: the whole encoding was refused by the same receiver: %v", name, err)
		}
		fresh, isCodec := reflect.New(reflect.TypeOf(probe.other).Elem()).Interface().(syntax.Codec)
		if !isCodec {
			t.Fatalf("%s: a fresh value of the probe's type is not a syntax.Codec", name)
		}
		if err := syntax.Unmarshal(otherEncoded, fresh); err != nil {
			t.Fatalf("%s: the whole encoding was refused by a fresh value: %v", name, err)
		}
		if !reflect.DeepEqual(probe.held, fresh) {
			t.Fatalf("%s: a receiver decoded into twice answered\n %+v\nand a fresh one answered\n %+v",
				name, probe.held, fresh)
		}
		t.Logf("%s: %d truncations refused with no write through", name, len(otherEncoded))
	}
}

// codecTypesPinnedIn is every type one file pins as a syntax.Codec, read off the pins rather
// than listed, sorted.
//
// The pin is this package's convention for "this type is a codec" -- treekem.go writes one under
// each of its three structures -- so it is the declaration to derive the class from. A list
// written beside the sweep is the thing standing rule 5 exists about, and the omission it
// produced here was the middle of three levels.
func codecTypesPinnedIn(t *testing.T, path string, source any) []string {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), path, source, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	names := []string{}
	for _, declaration := range parsed.Decls {
		generic, isGeneric := declaration.(*ast.GenDecl)
		if !isGeneric || generic.Tok != token.VAR {
			continue
		}
		for _, spec := range generic.Specs {
			value, isValue := spec.(*ast.ValueSpec)
			if !isValue || len(value.Names) != 1 || value.Names[0].Name != "_" || len(value.Values) != 1 {
				continue
			}
			selector, isSelector := value.Type.(*ast.SelectorExpr)
			if !isSelector || selector.Sel.Name != "Codec" {
				continue
			}
			qualifier, isIdent := selector.X.(*ast.Ident)
			if !isIdent || qualifier.Name != "syntax" {
				continue
			}
			if name := pointerConversionTypeName(value.Values[0]); name != "" {
				names = append(names, name)
			}
		}
	}
	slices.Sort(names)
	return names
}

// pointerConversionTypeName reads T out of the expression (*T)(nil) and answers the empty string
// for anything else.
func pointerConversionTypeName(expression ast.Expr) string {
	call, isCall := expression.(*ast.CallExpr)
	if !isCall {
		return ""
	}
	parenthesised, isParenthesised := call.Fun.(*ast.ParenExpr)
	if !isParenthesised {
		return ""
	}
	pointer, isPointer := parenthesised.X.(*ast.StarExpr)
	if !isPointer {
		return ""
	}
	named, isNamed := pointer.X.(*ast.Ident)
	if !isNamed {
		return ""
	}
	return named.Name
}

// The control for that reader. A collector that had started matching text reports the pin named
// in the comment; one that had lost the interface filter reports the Marshaler and the untyped
// declaration; one that had lost the blank name filter reports the named variable. All four are
// shapes this package's source actually holds somewhere.
const codecPinControl = `package control

var _ syntax.Codec = (*Pinned)(nil)

var (
	_ syntax.Codec     = (*AlsoPinned)(nil)
	_ syntax.Marshaler = (*NotACodec)(nil)
	_                  = (*Untyped)(nil)
)

var namedPin syntax.Codec = (*NotBlank)(nil)

// var _ syntax.Codec = (*InAComment)(nil) is prose and not a pin.
func control() {}
`

// TestTheCodecPinReaderFindsItsControlAndNothingElse holds the derivation above to a fixture
// whose answer is known, so a reader that had gone quiet -- and a quiet reader answers the empty
// class, which the sweep would then compare against an empty probe table and pass -- fails here
// instead.
func TestTheCodecPinReaderFindsItsControlAndNothingElse(t *testing.T) {
	got := codecTypesPinnedIn(t, "codec_pin_control.go", codecPinControl)
	if want := []string{"AlsoPinned", "Pinned"}; !slices.Equal(got, want) {
		t.Fatalf("the pin reader found %v in the control, want %v", got, want)
	}
	// and against the real file, so the sweep's class is not one only the control supports
	if found := codecTypesPinnedIn(t, "treekem.go", nil); len(found) == 0 {
		t.Fatal("the pin reader found no codec in treekem.go, which declares three")
	}
}

// ---------------------------------------------------------------------------
// task 20: sealing the path secrets to the copath resolutions
// ---------------------------------------------------------------------------
//
// Two failure directions and they are not symmetric, which is what decides the shape of
// everything below.
//
// TOO FEW ciphertexts, or one sealed to a key nobody in the resolution holds, locks a member
// out. It is loud: the next commit that member tries to process fails, it says so, and somebody
// looks. TOO MANY, or one sealed to the wrong member of the resolution, is silent -- a member
// who should not be able to read the epoch reads it, decrypts everything, and no test that
// counts ciphertexts or round trips one of them can tell. So the sweep here does not ask "does
// this open" at any single position. It builds the whole matrix of WHO opens WHAT and compares
// it against the target lists, in both directions at once.
//
// The pairing is POSITIONAL and nothing about a wrong ordering is visible from inside this
// task. Ciphertext j of node i belongs to entry j of that node's resolution; permute either
// vector and every member still receives a ciphertext of exactly the right shape, the counts
// still agree, the wire encoding still round trips, and the first thing that goes wrong is a
// decrypt in task 22, where it reads as a decryption bug. The matrix is what states the pairing
// as an assertion rather than as a comment.

// TestEncryptUpdatePathProducesOneCiphertextPerResolutionNode is the plan's own shape golden:
// one published node per node of the plan's path, carrying that node's public key and one
// non-empty ciphertext per entry of that node's target list.
//
// It is a COUNT and it is worth saying what a count cannot see, because everything sharper below
// exists for exactly that: a path that sealed every one of a node's secrets to the first member
// of its resolution, or that permuted the ciphertexts against the resolution, publishes exactly
// these numbers and exactly these lengths.
func TestEncryptUpdatePathProducesOneCiphertextPerResolutionNode(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, members := newTestTree(t, crypto, 4)
	targets, err := tree.EncryptionTargets(members[0].LeafIndex, nil)
	if err != nil {
		t.Fatalf("EncryptionTargets: %v", err)
	}
	plan, err := tree.CreateUpdatePathSecrets(crypto, members[0].LeafIndex,
		members[0].SignaturePriv, testGroupId())
	if err != nil {
		t.Fatalf("CreateUpdatePathSecrets: %v", err)
	}
	treeHash, err := tree.TreeHash(crypto)
	if err != nil {
		t.Fatalf("TreeHash: %v", err)
	}
	groupContext := append([]byte("test-group-context"), treeHash...)
	path, err := tree.EncryptUpdatePath(crypto, plan, members[0].LeafIndex, groupContext, nil)
	if err != nil {
		t.Fatalf("EncryptUpdatePath: %v", err)
	}
	if len(path.Nodes) != len(plan.Path) {
		t.Fatalf("nodes = %d, want %d", len(path.Nodes), len(plan.Path))
	}
	for i := range path.Nodes {
		if !bytes.Equal(path.Nodes[i].EncryptionKey, plan.PublicKeys[i]) {
			t.Fatalf("node %d key differs from the plan", i)
		}
		if len(path.Nodes[i].EncryptedPathSecret) != len(targets[i]) {
			t.Fatalf("node %d has %d ciphertexts for %d resolution entries",
				i, len(path.Nodes[i].EncryptedPathSecret), len(targets[i]))
		}
		for j, ct := range path.Nodes[i].EncryptedPathSecret {
			if len(ct.KemOutput) == 0 || len(ct.Ciphertext) == 0 {
				t.Fatalf("node %d ciphertext %d is empty", i, j)
			}
		}
	}
	if !bytes.Equal(path.LeafNode.ParentHash, plan.LeafNode.ParentHash) {
		t.Fatalf("the update path carries a different leaf from the plan")
	}
}

// TestEncryptUpdatePathIsDecryptableByAResolutionMember is the plan's round trip at the one
// position where the resolution holds a single member, so the ciphertext at index 0 has only one
// key it could belong to.
//
// The negative half is over the CONTEXT alone. It says nothing about which member a ciphertext
// belongs to at any node whose resolution holds more than one, which is every node above the
// first in a group of more than two.
func TestEncryptUpdatePathIsDecryptableByAResolutionMember(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, members := newTestTree(t, crypto, 4)
	plan, err := tree.CreateUpdatePathSecrets(crypto, members[0].LeafIndex,
		members[0].SignaturePriv, testGroupId())
	if err != nil {
		t.Fatalf("CreateUpdatePathSecrets: %v", err)
	}
	groupContext := []byte("context")
	path, err := tree.EncryptUpdatePath(crypto, plan, members[0].LeafIndex, groupContext, nil)
	if err != nil {
		t.Fatalf("EncryptUpdatePath: %v", err)
	}
	// leaf 1 is the whole resolution of node 1's copath child, so its ciphertext is
	// the only one at index 0 and it must open with leaf 1's private key.
	ct := path.Nodes[0].EncryptedPathSecret[0]
	got, err := DecryptWithLabel(crypto, members[1].EncryptionPriv, "UpdatePathNode",
		groupContext, ct.KemOutput, ct.Ciphertext)
	if err != nil {
		t.Fatalf("DecryptWithLabel: %v", err)
	}
	if !bytes.Equal(got, plan.PathSecrets[0]) {
		t.Fatalf("decrypted secret is not path_secret[0]")
	}
	// a different context must not open it.
	if _, err := DecryptWithLabel(crypto, members[1].EncryptionPriv, "UpdatePathNode",
		[]byte("other"), ct.KemOutput, ct.Ciphertext); err == nil {
		t.Fatalf("the ciphertext opened under a different group context")
	}
}

// TestEncryptUpdatePathSkipsExcludedLeaves is the plan's golden for the exclusion reaching
// EncryptionTargets at all: node 1 of a four member path is the root, whose copath child
// resolves to leaves 2 and 3, and excluding leaf 3 leaves one ciphertext there.
//
// It is a count at one node. That the SURVIVING ciphertext is leaf 2's, rather than leaf 3's
// with the count reduced somewhere else, is the sweep below's.
func TestEncryptUpdatePathSkipsExcludedLeaves(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, members := newTestTree(t, crypto, 4)
	plan, err := tree.CreateUpdatePathSecrets(crypto, members[0].LeafIndex,
		members[0].SignaturePriv, testGroupId())
	if err != nil {
		t.Fatalf("CreateUpdatePathSecrets: %v", err)
	}
	path, err := tree.EncryptUpdatePath(crypto, plan, members[0].LeafIndex,
		[]byte("context"), []LeafIndex{3})
	if err != nil {
		t.Fatalf("EncryptUpdatePath: %v", err)
	}
	if len(path.Nodes[1].EncryptedPathSecret) != 1 {
		t.Fatalf("node 1 has %d ciphertexts, want 1 with leaf 3 excluded",
			len(path.Nodes[1].EncryptedPathSecret))
	}
}

// testUpdatePathContext is a serialized GroupContext over one tree hash, which is the form the
// plan pins for this call's HPKE info: bytes from syntax.Marshal, not a *GroupContext.
//
// The tree hash is the only field a caller varies below, so two contexts built here differ
// exactly where the epoch boundary puts them and nowhere else. That is what lets the context
// test distinguish the epoch the commit OPENS from the one it closed without also changing the
// group id, the epoch number or the transcript.
func testUpdatePathContext(t *testing.T, treeHash []byte) []byte {
	t.Helper()
	encoded, err := syntax.Marshal(&GroupContext{
		Version:                 ProtocolVersionMls10,
		CipherSuite:             CipherSuiteX25519ChaCha20Sha256Ed25519,
		GroupId:                 testGroupId(),
		Epoch:                   7,
		TreeHash:                treeHash,
		ConfirmedTranscriptHash: bytes.Repeat([]byte{0x63}, 32),
	})
	if err != nil {
		t.Fatalf("syntax.Marshal(GroupContext): %v", err)
	}
	return encoded
}

// updatePathOpenings is the whole matrix at one node of a published path: every pair of a member
// and a ciphertext index where that member's own leaf private key opens that ciphertext to the
// path secret this node published.
//
// Every member is tried against every ciphertext, which is the half that reads the silent
// direction. A sweep that only checked that the members of the resolution CAN open would pass
// over a path that also sealed the secret to a leaf this commit excluded, or that sealed every
// ciphertext of a node to one member of its resolution -- both of which hand the secret to
// somebody who should not have it while every count and every round trip stays correct.
//
// An opening is recorded only when the plaintext is the expected secret, so a ciphertext that
// opened to something else is a failure rather than an opening counted as one.
func updatePathOpenings(t *testing.T, crypto CryptoProvider, members []*testTreeMember,
	groupContext []byte, node UpdatePathNode, secret []byte) []string {
	t.Helper()
	found := []string{}
	for _, member := range members {
		for j := range node.EncryptedPathSecret {
			ct := node.EncryptedPathSecret[j]
			opened, err := OpenWithLabel(crypto, member.EncryptionPriv, updatePathNodeLabel,
				groupContext, &ct)
			if err != nil {
				continue
			}
			if !bytes.Equal(opened, secret) {
				t.Errorf("leaf %d opened ciphertext %d to %x, and the path secret published at that node is %x; an open that succeeds on the wrong plaintext is not a refusal",
					member.LeafIndex, j, opened, secret)
				continue
			}
			found = append(found, fmt.Sprintf("leaf %d opens ciphertext %d", member.LeafIndex, j))
		}
	}
	slices.Sort(found)
	return found
}

// updatePathOpeningsTheResolutionRequires is the same matrix derived from the target lists
// rather than from the ciphertexts: entry j of the resolution, and only that entry, opens
// ciphertext j.
//
// Read off EncryptionTargets rather than written down, so a change to what a resolution holds
// moves the expectation with it instead of leaving a hand written table agreeing with a
// membership the tree no longer has.
//
// Every entry is required to be a LEAF, which is a claim about the fixture and is checked rather
// than assumed: newTestTree leaves every parent blank, so a resolution of it is a list of leaves
// and each entry names a member whose private key this file holds. The day that stops being true
// the sweep would silently cover fewer entries than it names.
func updatePathOpeningsTheResolutionRequires(t *testing.T, targets []NodeIndex) []string {
	t.Helper()
	want := []string{}
	for j, y := range targets {
		leaf, err := y.LeafIndex()
		if err != nil {
			t.Fatalf("resolution entry %d is node %d, which is not a leaf, and this sweep can only hold a resolution of leaves: %v",
				j, y, err)
		}
		want = append(want, fmt.Sprintf("leaf %d opens ciphertext %d", leaf, j))
	}
	slices.Sort(want)
	return want
}

// TestEncryptUpdatePathPairsEachCiphertextWithItsOwnResolutionEntry is the property the whole
// task exists to get right, stated over every node of every path of several trees.
//
// The assertion is set EQUALITY between who can open and who the resolution says may, so both
// failure directions are held by one comparison. A missing pair is a member locked out of the
// epoch; an extra pair is a member reading an epoch it was not given, which is the direction
// nothing else here can see. And because the pairs carry the ciphertext INDEX, a permuted
// ciphertext vector and a path that sealed every secret of a node to the first member of its
// resolution both fail as loudly as a dropped ciphertext does -- those are the three mutants
// that leave the counts, the round trip and the wire encoding all correct.
//
// Exclusions are swept alongside, because "this member is not in the target list" and "this
// member cannot open" are the same sentence read from the two sides, and a commit that adds a
// member is the case where the second is what actually matters: the added leaf gets the secret
// in its Welcome and must not get it here as well.
func TestEncryptUpdatePathPairsEachCiphertextWithItsOwnResolutionEntry(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	nodes, ciphertexts, openings, vacant := 0, 0, 0, 0
	for _, n := range []uint32{2, 3, 4, 7, 8} {
		fixture, members := newTestTree(t, crypto, n)
		for _, member := range members {
			// every other leaf is excluded once, plus the run that excludes nobody. The
			// excluded leaf stands in for one this commit adds: it is in the tree, it is in
			// the resolutions the unexcluded run seals to, and after the exclusion it must
			// open nothing at all.
			exclusions := [][]LeafIndex{nil}
			for _, other := range members {
				if other.LeafIndex != member.LeafIndex {
					exclusions = append(exclusions, []LeafIndex{other.LeafIndex})
				}
			}
			for _, exclude := range exclusions {
				at := fmt.Sprintf("a %d member tree, sender %d, excluding %v", n, member.LeafIndex, exclude)
				tree := fixture.Clone()
				plan, err := tree.CreateUpdatePathSecrets(crypto, member.LeafIndex,
					member.SignaturePriv, testGroupId())
				if err != nil {
					t.Fatalf("%s: CreateUpdatePathSecrets: %v", at, err)
				}
				treeHash, err := tree.TreeHash(crypto)
				if err != nil {
					t.Fatalf("%s: TreeHash: %v", at, err)
				}
				groupContext := testUpdatePathContext(t, treeHash)
				path, err := tree.EncryptUpdatePath(crypto, plan, member.LeafIndex, groupContext, exclude)
				if err != nil {
					t.Fatalf("%s: EncryptUpdatePath: %v", at, err)
				}
				// the targets are read back out of the tree the path was built against, with
				// the same exclusion, and that is the measure this sweep is against
				targets, err := tree.EncryptionTargets(member.LeafIndex, exclude)
				if err != nil {
					t.Fatalf("%s: EncryptionTargets: %v", at, err)
				}
				if len(path.Nodes) != len(targets) {
					t.Fatalf("%s: the path published %d nodes and the tree gives the sender %d",
						at, len(path.Nodes), len(targets))
				}
				for i := range path.Nodes {
					nodes++
					ciphertexts += len(path.Nodes[i].EncryptedPathSecret)
					want := updatePathOpeningsTheResolutionRequires(t, targets[i])
					// the per case floor. The totals at the end are satisfied by one node of
					// one tree, and at a position an exclusion has emptied the comparison
					// below is [] against [], which is correct and is an assertion about
					// nothing. With nobody excluded there is no such position, and that is a
					// RULE rather than an observation about this fixture: a node reaches the
					// filtered direct path only because its copath child resolves to
					// something. So the empty positions are counted, and with no exclusion in
					// force there may be none.
					if len(want) == 0 {
						vacant++
						if exclude == nil {
							t.Errorf("%s: path node %d published no ciphertext with nobody excluded, and a node is on the filtered direct path only when its copath child resolves to something",
								at, i)
						}
					}
					got := updatePathOpenings(t, crypto, members, groupContext,
						path.Nodes[i], plan.PathSecrets[i])
					openings += len(got)
					if !slices.Equal(got, want) {
						t.Errorf("%s: at path node %d the ciphertexts open as %v and the resolution of that node's copath child is %v, so they must open as %v; a pair present here and absent there is a member reading an epoch it was not given, and one absent here and present there is a member locked out",
							at, i, got, targets[i], want)
					}
				}
			}
		}
	}
	if nodes == 0 || ciphertexts == 0 || openings == 0 {
		t.Fatalf("the sweep read %d path nodes, %d ciphertexts and %d openings; with any of the three at zero it compared two empty sets",
			nodes, ciphertexts, openings)
	}
	t.Logf("%d path nodes, %d ciphertexts, %d openings, each paired with its own resolution entry; %d of the nodes had their whole resolution excluded and compared two empty sets",
		nodes, ciphertexts, openings, vacant)
}

// TestEncryptUpdatePathSealsUnderTheContextOfTheEpochTheCommitOpens is the other thing about
// this call that no round trip through it can see.
//
// The HPKE info is the serialized GroupContext of the epoch the commit OPENS, whose tree_hash
// covers the public keys and the commit leaf task 18 has just installed. A path sealed under the
// context of the epoch the commit CLOSED encrypts, decrypts against itself, publishes the right
// number of ciphertexts to the right members, and is rejected by every peer in the group at
// once -- and a seal-and-open written over one context cannot tell the two apart, because it
// uses the same wrong bytes on both sides.
//
// So the two contexts are made distinguishable -- the same GroupContext over the tree hash
// before the commit and after it -- and the test asserts which one was used, in BOTH directions.
// A path sealed under the new context opens only under the new one, and a path sealed under the
// old one opens only under the old one. Together those say the info is exactly the bytes this
// call was handed: an implementation that ignored its groupContext argument and built a context
// of its own out of the tree it can see fails the first, and one that put the argument in the
// aad or the label instead fails both.
//
// The control in front of them is that the tree hash MOVED across the commit at all. If it had
// not, the two contexts would be one and every assertion below would be comparing bytes with
// themselves.
func TestEncryptUpdatePathSealsUnderTheContextOfTheEpochTheCommitOpens(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	tree, members := newTestTree(t, crypto, 4)
	before, err := tree.TreeHash(crypto)
	if err != nil {
		t.Fatalf("TreeHash before the commit: %v", err)
	}
	plan, err := tree.CreateUpdatePathSecrets(crypto, members[0].LeafIndex,
		members[0].SignaturePriv, testGroupId())
	if err != nil {
		t.Fatalf("CreateUpdatePathSecrets: %v", err)
	}
	after, err := tree.TreeHash(crypto)
	if err != nil {
		t.Fatalf("TreeHash after the commit: %v", err)
	}
	if bytes.Equal(before, after) {
		t.Fatal("the tree hash did not move across the commit, so the two group contexts below are one and nothing here distinguishes them")
	}
	closed := testUpdatePathContext(t, before)
	opened := testUpdatePathContext(t, after)
	if bytes.Equal(closed, opened) {
		t.Fatal("the two serialized group contexts are identical, so this test cannot say which of them a ciphertext was sealed under")
	}
	// leaf 1 is the whole resolution of node 1's copath child in a four member tree whose
	// parents are blank, so the ciphertext at index 0 of the first path node is its own
	for _, sealed := range []struct {
		name  string
		under []byte
		other []byte
	}{
		{name: "the context of the epoch the commit opens", under: opened, other: closed},
		{name: "the context of the epoch the commit closed", under: closed, other: opened},
	} {
		path, err := tree.EncryptUpdatePath(crypto, plan, members[0].LeafIndex, sealed.under, nil)
		if err != nil {
			t.Fatalf("%s: EncryptUpdatePath: %v", sealed.name, err)
		}
		ct := path.Nodes[0].EncryptedPathSecret[0]
		got, err := OpenWithLabel(crypto, members[1].EncryptionPriv, updatePathNodeLabel,
			sealed.under, &ct)
		if err != nil {
			t.Fatalf("%s: the ciphertext did not open under the very bytes it was sealed with, so this call is not using its groupContext argument as the hpke info: %v",
				sealed.name, err)
		}
		if !bytes.Equal(got, plan.PathSecrets[0]) {
			t.Fatalf("%s: the ciphertext opened to %x rather than to path_secret[0] %x",
				sealed.name, got, plan.PathSecrets[0])
		}
		if _, err := OpenWithLabel(crypto, members[1].EncryptionPriv, updatePathNodeLabel,
			sealed.other, &ct); err == nil {
			t.Fatalf("%s: the ciphertext also opened under the other epoch's group context, so the context is not bound into the encryption at all",
				sealed.name)
		}
	}
}

// TestEncryptUpdatePathLeavesTheTreeExactlyAsItFoundIt states the boundary between task 18 and
// task 20 as an assertion.
//
// Task 18 mutates the tree and this call must not, because the tree hash the caller put into the
// group context was read BETWEEN the two calls. A second mutation here would move the tree hash
// out from under the context every ciphertext was just sealed with, and the symptom is the one
// the context test above describes: a path every peer rejects, from a sender whose own round
// trip succeeded.
func TestEncryptUpdatePathLeavesTheTreeExactlyAsItFoundIt(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	tree, members := newTestTree(t, crypto, 8)
	plan, err := tree.CreateUpdatePathSecrets(crypto, members[3].LeafIndex,
		members[3].SignaturePriv, testGroupId())
	if err != nil {
		t.Fatalf("CreateUpdatePathSecrets: %v", err)
	}
	before, err := tree.TreeHash(crypto)
	if err != nil {
		t.Fatalf("TreeHash: %v", err)
	}
	if _, err := tree.EncryptUpdatePath(crypto, plan, members[3].LeafIndex,
		testUpdatePathContext(t, before), nil); err != nil {
		t.Fatalf("EncryptUpdatePath: %v", err)
	}
	after, err := tree.TreeHash(crypto)
	if err != nil {
		t.Fatalf("TreeHash after: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("the tree hash moved from %x to %x across a call that only reads the tree; every ciphertext this call just sealed was bound to the first one",
			before, after)
	}
}

// TestEncryptUpdatePathRefusesAPlanWhoseLengthsDoNotAgree is the refusal in front of the
// positional pairing.
//
// Every one of the four vectors this call walks -- the plan's path, its secrets, its public
// keys, and the target lists it reads off the tree -- is indexed by the same i, and the pairing
// means nothing unless they are the same length. A body that trusted them and ranged over the
// shortest would publish a path with fewer nodes than the sender's filtered direct path, which
// every receiver refuses as ValSem202 one hop later; one that ranged over the plan's path alone
// would index past a shorter target list and take the caller's process down.
//
// Each vector is shortened on its own, because a check written over one pair of them passes
// every case the other pair breaks, and the control is the unshortened plan: it has to be
// accepted, or the four refusals would be reporting whatever else is wrong with the fixture.
func TestEncryptUpdatePathRefusesAPlanWhoseLengthsDoNotAgree(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	fixture, members := newTestTree(t, crypto, 8)
	build := func(t *testing.T) (*RatchetTree, *UpdatePathPlan, []byte) {
		t.Helper()
		tree := fixture.Clone()
		plan, err := tree.CreateUpdatePathSecrets(crypto, members[0].LeafIndex,
			members[0].SignaturePriv, testGroupId())
		if err != nil {
			t.Fatalf("CreateUpdatePathSecrets: %v", err)
		}
		treeHash, err := tree.TreeHash(crypto)
		if err != nil {
			t.Fatalf("TreeHash: %v", err)
		}
		return tree, plan, testUpdatePathContext(t, treeHash)
	}
	// the control: the plan the fixture actually produced has to be accepted
	tree, plan, groupContext := build(t)
	if len(plan.Path) < 2 {
		t.Fatalf("the fixture gives the sender a filtered direct path of %d nodes, and every case below shortens one vector by one",
			len(plan.Path))
	}
	if _, err := tree.EncryptUpdatePath(crypto, plan, members[0].LeafIndex, groupContext, nil); err != nil {
		t.Fatalf("the unshortened plan was refused with %v, so the refusals below say nothing about the lengths", err)
	}
	for _, shortened := range []struct {
		name  string
		apply func(plan *UpdatePathPlan)
	}{
		{name: "one node fewer than the tree gives the sender", apply: func(plan *UpdatePathPlan) {
			plan.Path = plan.Path[:len(plan.Path)-1]
			plan.PathSecrets = plan.PathSecrets[:len(plan.PathSecrets)-1]
			plan.PublicKeys = plan.PublicKeys[:len(plan.PublicKeys)-1]
		}},
		{name: "a path longer than its own secrets", apply: func(plan *UpdatePathPlan) {
			plan.PathSecrets = plan.PathSecrets[:len(plan.PathSecrets)-1]
		}},
		{name: "a path longer than its own public keys", apply: func(plan *UpdatePathPlan) {
			plan.PublicKeys = plan.PublicKeys[:len(plan.PublicKeys)-1]
		}},
		{name: "a path shorter than its own secrets and keys", apply: func(plan *UpdatePathPlan) {
			plan.Path = plan.Path[:len(plan.Path)-1]
		}},
	} {
		broken, brokenPlan, brokenContext := build(t)
		shortened.apply(brokenPlan)
		_, err := broken.EncryptUpdatePath(crypto, brokenPlan, members[0].LeafIndex, brokenContext, nil)
		if !errors.Is(err, errPathLength) {
			t.Errorf("%s: EncryptUpdatePath err = %v, want errPathLength", shortened.name, err)
		}
	}
}

// TestEncryptUpdatePathRefusesAPlanItWasNotGiven is the nil arm, an error rather than the panic
// the shorter body would take, for errNilHpkeCiphertext's reason in the same file.
//
// The private half is one of the three, and it is the one that is not merely defensive. It is
// where the plan records the leaf it was generated for, which is what errPlanNotThisSenders is
// decided against, so a plan reaching the seal without it is a plan whose sender nothing can
// check -- and a body that used the private state only when it happened to be there would let
// any caller switch that refusal off by handing over less.
func TestEncryptUpdatePathRefusesAPlanItWasNotGiven(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	tree, members := newTestTree(t, crypto, 4)
	standing := tree.Leaf(members[0].LeafIndex)
	if standing == nil {
		t.Fatalf("the fixture carries no leaf at %d, so the case below is not the one it names",
			members[0].LeafIndex)
	}
	for _, missing := range []struct {
		name string
		plan *UpdatePathPlan
	}{
		{name: "no plan at all", plan: nil},
		{name: "a plan carrying no leaf", plan: &UpdatePathPlan{}},
		{name: "a plan carrying a leaf and no private state",
			plan: &UpdatePathPlan{LeafNode: standing.Clone()}},
	} {
		_, err := tree.EncryptUpdatePath(crypto, missing.plan, members[0].LeafIndex,
			[]byte("context"), nil)
		if !errors.Is(err, errNilUpdatePathPlan) {
			t.Errorf("%s: EncryptUpdatePath err = %v, want errNilUpdatePathPlan", missing.name, err)
		}
	}
}

// TestEncryptUpdatePathAnswersStorageOfItsOwnRatherThanThePlansArrays is the ownership half.
//
// What this call answers is a wire value: it is about to be serialized, and the framing layer
// may hold it after the sender has thrown the plan away -- a commit that loses a race is
// discarded whole, which is what TreeKEMPrivate.Clone's own comment is about one task back. Two
// structures over one array make either one's disposal the other's corruption, and the leaf is
// the half that matters most, because its signature covers the very arrays that would be shared.
func TestEncryptUpdatePathAnswersStorageOfItsOwnRatherThanThePlansArrays(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	tree, members := newTestTree(t, crypto, 4)
	plan, err := tree.CreateUpdatePathSecrets(crypto, members[0].LeafIndex,
		members[0].SignaturePriv, testGroupId())
	if err != nil {
		t.Fatalf("CreateUpdatePathSecrets: %v", err)
	}
	treeHash, err := tree.TreeHash(crypto)
	if err != nil {
		t.Fatalf("TreeHash: %v", err)
	}
	path, err := tree.EncryptUpdatePath(crypto, plan, members[0].LeafIndex,
		testUpdatePathContext(t, treeHash), nil)
	if err != nil {
		t.Fatalf("EncryptUpdatePath: %v", err)
	}
	shared := map[string][2][]byte{
		"the leaf's encryption key": {path.LeafNode.EncryptionKey, plan.LeafNode.EncryptionKey},
		"the leaf's parent hash":    {path.LeafNode.ParentHash, plan.LeafNode.ParentHash},
		"the leaf's signature":      {path.LeafNode.Signature, plan.LeafNode.Signature},
	}
	for i := range path.Nodes {
		shared[fmt.Sprintf("the public key published at path node %d", i)] =
			[2][]byte{path.Nodes[i].EncryptionKey, plan.PublicKeys[i]}
	}
	for what, pair := range shared {
		if len(pair[0]) == 0 || !bytes.Equal(pair[0], pair[1]) {
			t.Fatalf("%s came back as %x and the plan holds %x, so this test is not comparing the two halves it names",
				what, pair[0], pair[1])
		}
		if &pair[0][0] == &pair[1][0] {
			t.Errorf("the published path and the plan share the storage of %s; the plan is the sender's live state and the path is what the framing layer keeps",
				what)
		}
	}
}

// ---------------------------------------------------------------------------
// task 20 review: the sender argument, the parent arm, and the two refusals
// ---------------------------------------------------------------------------

// TestEncryptUpdatePathRefusesAPlanPublishedUnderAnotherLeaf holds the sender argument against
// the identity the plan already carries, over EVERY ordered pair of leaves rather than a chosen
// one.
//
// The pair SWEEP is the property and not thoroughness for its own sake. sender was a second,
// unchecked source of truth, and all that stood between it and the plan was a comparison of
// lengths -- which over a full tree is no comparison at all, because every leaf's filtered direct
// path is the same three nodes long. So the class refused here is "every leaf that is not the
// plan's own", and it is enumerated out of the tree rather than sampled: a check written against
// the tree's PATH instead of against the plan's own index refuses most of these pairs and accepts
// the sibling ones, where the two filtered direct paths are equal node for node and only the
// copath differs. Those pairs are counted, so a fixture that stopped holding any would say so
// instead of quietly covering less.
//
// What the acceptance is for: a plan published under its own leaf has to succeed, or the
// refusals below would be reporting a fixture that refuses everything.
func TestEncryptUpdatePathRefusesAPlanPublishedUnderAnotherLeaf(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	fixture, members := newTestTree(t, crypto, 8)
	refused, accepted, sharingAPath := 0, 0, 0
	for _, owner := range members {
		for _, publisher := range members {
			at := fmt.Sprintf("a plan built for leaf %d and published as leaf %d",
				owner.LeafIndex, publisher.LeafIndex)
			tree := fixture.Clone()
			plan, err := tree.CreateUpdatePathSecrets(crypto, owner.LeafIndex,
				owner.SignaturePriv, testGroupId())
			if err != nil {
				t.Fatalf("%s: CreateUpdatePathSecrets: %v", at, err)
			}
			treeHash, err := tree.TreeHash(crypto)
			if err != nil {
				t.Fatalf("%s: TreeHash: %v", at, err)
			}
			groupContext := testUpdatePathContext(t, treeHash)
			// held in names of its own, because the two path lookups below overwrite an err
			// this assertion is about and a sweep reading the wrong one reports nothing
			path, refusal := tree.EncryptUpdatePath(crypto, plan, publisher.LeafIndex,
				groupContext, nil)
			if publisher.LeafIndex == owner.LeafIndex {
				if refusal != nil {
					t.Fatalf("%s: the plan's own leaf was refused with %v, so every refusal below says nothing about the sender",
						at, refusal)
				}
				accepted += 1
				continue
			}
			refused += 1
			if !errors.Is(refusal, errPlanNotThisSenders) {
				t.Errorf("%s: EncryptUpdatePath err = %v, want errPlanNotThisSenders; accepted, the plan's path secrets go out sealed to the publishing leaf's copath resolution, which is a subtree that never generated them",
					at, refusal)
			}
			if path != nil {
				t.Errorf("%s: EncryptUpdatePath refused and answered a path of %d nodes anyway",
					at, len(path.Nodes))
			}
			// the pairs a check derived from the tree cannot tell apart, counted so the sweep
			// says whether it covered them rather than leaving it to this comment
			ownersPath, err := tree.FilteredDirectPath(owner.LeafIndex)
			if err != nil {
				t.Fatalf("%s: FilteredDirectPath(%d): %v", at, owner.LeafIndex, err)
			}
			publishersPath, err := tree.FilteredDirectPath(publisher.LeafIndex)
			if err != nil {
				t.Fatalf("%s: FilteredDirectPath(%d): %v", at, publisher.LeafIndex, err)
			}
			if equalNodeIndices(ownersPath, publishersPath) {
				sharingAPath += 1
			}
		}
	}
	if want := len(members) * (len(members) - 1); refused != want {
		t.Fatalf("the sweep refused %d pairs and the fixture has %d ordered pairs of distinct leaves",
			refused, want)
	}
	if accepted != len(members) {
		t.Fatalf("the sweep accepted %d plans published under their own leaf and the fixture has %d leaves",
			accepted, len(members))
	}
	if sharingAPath == 0 {
		t.Fatalf("no pair in this sweep had two leaves sharing a filtered direct path, so it never covered the sibling case a check against the tree's own path cannot see")
	}
	t.Logf("%d ordered pairs refused, %d of them sibling pairs whose filtered direct paths are equal node for node, %d plans accepted under their own leaf",
		refused, sharingAPath, accepted)
}

// TestEncryptUpdatePathSealsToTheKeyOfANonLeafResolutionEntry is the parent arm of
// nodeEncryptionKey, which nothing in this file reached.
//
// Every resolution this task ever saw was a list of LEAVES, and that is a property of the fixture
// rather than of the code: newTestTree fills leaves and leaves every parent blank, and
// CreateUpdatePathSecrets fills only the sender's own direct path, which is never a copath child
// of itself. So a resolution entry that is a parent needs a tree where somebody has ALREADY
// committed -- and until then the arm carrying the majority of resolution entries in any real
// group after the first commit was dead code from the suite's point of view.
//
// Two members commit in turn. Leaf 2's commit fills node 5, and node 5 is leaf 0's copath child
// at the root, so leaf 0's second path node seals to that PARENT rather than to the two leaves
// beneath it. The assertion is an OPEN and not a shape: the ciphertext is opened with the private
// key leaf 2 derives for node 5 and has to be the path secret leaf 0 published at that position,
// which says the key the tree carries at that parent is the key the seal actually used.
//
// The over-wide direction is asserted beside it. No member's own leaf key may open that
// ciphertext -- a resolution names a parent instead of its subtree precisely because the parent's
// key is the one that subtree shares -- and a seal that reached for a leaf's key instead would
// still open, still round trip and still publish exactly these counts.
func TestEncryptUpdatePathSealsToTheKeyOfANonLeafResolutionEntry(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	tree, members := newTestTree(t, crypto, 4)
	first, err := tree.CreateUpdatePathSecrets(crypto, members[2].LeafIndex,
		members[2].SignaturePriv, testGroupId())
	if err != nil {
		t.Fatalf("CreateUpdatePathSecrets(leaf 2): %v", err)
	}
	plan, err := tree.CreateUpdatePathSecrets(crypto, members[0].LeafIndex,
		members[0].SignaturePriv, testGroupId())
	if err != nil {
		t.Fatalf("CreateUpdatePathSecrets(leaf 0): %v", err)
	}
	treeHash, err := tree.TreeHash(crypto)
	if err != nil {
		t.Fatalf("TreeHash: %v", err)
	}
	groupContext := testUpdatePathContext(t, treeHash)
	path, err := tree.EncryptUpdatePath(crypto, plan, members[0].LeafIndex, groupContext, nil)
	if err != nil {
		t.Fatalf("EncryptUpdatePath: %v", err)
	}
	targets, err := tree.EncryptionTargets(members[0].LeafIndex, nil)
	if err != nil {
		t.Fatalf("EncryptionTargets: %v", err)
	}
	parents := 0
	for i, list := range targets {
		for j, y := range list {
			if _, isALeaf := y.LeafIndex(); isALeaf == nil {
				// the leaf entries are the pairing sweep's, next door
				continue
			}
			parents += 1
			priv, held, err := first.Private.NodePrivateKey(crypto, y)
			if err != nil {
				t.Fatalf("NodePrivateKey(%d): %v", y, err)
			}
			if !held {
				t.Fatalf("the first committer's private state holds no secret for node %d, so this test has nothing to open what was sealed there with",
					y)
			}
			ct := path.Nodes[i].EncryptedPathSecret[j]
			opened, err := OpenWithLabel(crypto, priv, updatePathNodeLabel, groupContext, &ct)
			if err != nil {
				t.Fatalf("path node %d ciphertext %d did not open with the private key of node %d, the parent standing at that position of the resolution: %v",
					i, j, y, err)
			}
			if !bytes.Equal(opened, plan.PathSecrets[i]) {
				t.Fatalf("path node %d ciphertext %d opened to %x and the secret published at that node is %x",
					i, j, opened, plan.PathSecrets[i])
			}
			for _, member := range members {
				if _, err := OpenWithLabel(crypto, member.EncryptionPriv, updatePathNodeLabel,
					groupContext, &ct); err == nil {
					t.Errorf("leaf %d's own key opens the ciphertext sealed to parent node %d; that position of the resolution is the parent precisely because the parent's key is the one its subtree shares",
						member.LeafIndex, y)
				}
			}
		}
	}
	if parents == 0 {
		t.Fatalf("every entry of %v is a leaf, so this test never reached the arm of nodeEncryptionKey it is named for",
			targets)
	}
	t.Logf("%d non-leaf resolution entries, each sealed to and opened with the key the tree carries at that parent",
		parents)
}

// TestNodeEncryptionKeyTellsAnIndexOutsideTheTreeApartFromABlankInsideIt is the claim
// nodeEncryptionKey's own comment makes, turned into assertions.
//
// The comment argues that the two ways there is no key here are different faults with different
// repairs -- an index past the width is a caller that computed one wrong and repairs it by
// recomputing, a blank node reached through a resolution is a tree whose walk and whose storage
// disagree and no re-derived index repairs that -- and nothing observed either arm or the
// distinction between them. Both refusals and the third, a stored node holding neither half, were
// dead code from the suite's point of view, and one sentinel for both would have read the same.
//
// The two ANSWERS are asserted alongside, because a refusal test over a function nothing ever
// asks a successful question of is an error path with no path beside it.
func TestNodeEncryptionKeyTellsAnIndexOutsideTheTreeApartFromABlankInsideIt(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	tree, members := newTestTree(t, crypto, 4)
	// a non-blank parent to ask about, which newTestTree has none of on its own: leaf 2's
	// commit fills nodes 5 and 3, and node 1 stays blank for the refusal below.
	if _, err := tree.CreateUpdatePathSecrets(crypto, members[2].LeafIndex,
		members[2].SignaturePriv, testGroupId()); err != nil {
		t.Fatalf("CreateUpdatePathSecrets: %v", err)
	}
	leaf := tree.Leaf(members[1].LeafIndex)
	if leaf == nil {
		t.Fatalf("the fixture carries no leaf at %d", members[1].LeafIndex)
	}
	atTheLeaf, err := tree.nodeEncryptionKey(members[1].LeafIndex.NodeIndex())
	if err != nil {
		t.Fatalf("nodeEncryptionKey at a leaf: %v", err)
	}
	if len(atTheLeaf) == 0 || !bytes.Equal(atTheLeaf, leaf.EncryptionKey) {
		t.Errorf("nodeEncryptionKey at leaf %d answered %x and the tree carries %x there",
			members[1].LeafIndex, atTheLeaf, leaf.EncryptionKey)
	}
	parent := tree.ParentAt(NodeIndex(5))
	if parent == nil {
		t.Fatalf("node 5 is blank after leaf 2's commit, so this test has no parent to ask about")
	}
	atTheParent, err := tree.nodeEncryptionKey(NodeIndex(5))
	if err != nil {
		t.Fatalf("nodeEncryptionKey at a parent: %v", err)
	}
	if len(atTheParent) == 0 || !bytes.Equal(atTheParent, parent.EncryptionKey) {
		t.Errorf("nodeEncryptionKey at parent node 5 answered %x and the tree carries %x there",
			atTheParent, parent.EncryptionKey)
	}
	if tree.Get(NodeIndex(1)) != nil {
		t.Fatalf("node 1 is occupied in this fixture and the blank refusal below needs it blank")
	}
	// the third arm needs a position that is occupied and holds neither half, which no walk of
	// this package produces and only a hand written node reaches
	occupied := tree.Clone()
	occupied.nodes[1] = &Node{NodeType: NodeTypeParent}
	for _, refusal := range []struct {
		name string
		tree *RatchetTree
		at   NodeIndex
		want error
	}{
		{name: "the first index past the width of the tree", tree: tree,
			at: NodeIndex(tree.NodeWidth()), want: ErrNodeIndexOutOfRange},
		{name: "an index well past the width of the tree", tree: tree,
			at: NodeIndex(tree.NodeWidth() + 8), want: ErrNodeIndexOutOfRange},
		{name: "a blank position inside the tree", tree: tree,
			at: NodeIndex(1), want: ErrTreeMalformed},
		{name: "an occupied position holding neither a leaf nor a parent", tree: occupied,
			at: NodeIndex(1), want: ErrTreeMalformed},
	} {
		key, err := refusal.tree.nodeEncryptionKey(refusal.at)
		if !errors.Is(err, refusal.want) {
			t.Errorf("%s: nodeEncryptionKey(%d) err = %v, want %v; one sentinel for two faults sends the second to repair the thing that was not broken",
				refusal.name, refusal.at, err, refusal.want)
		}
		if key != nil {
			t.Errorf("%s: nodeEncryptionKey(%d) refused and answered %x anyway",
				refusal.name, refusal.at, key)
		}
	}
}

// TestEncryptUpdatePathPairsAgainstAHandDerivedResolution states the pairing INDEPENDENTLY of
// EncryptionTargets.
//
// The sweep above reads its expectation out of the same EncryptionTargets call the implementation
// uses, so what it proves is that EncryptUpdatePath agrees with task 16 -- not what the
// resolution of a four member tree is. An error inside EncryptionTargets moves both sides of that
// comparison at once and the sweep goes on passing. This table is written from the tree's
// GEOMETRY instead: four leaves at nodes 0, 2, 4 and 6 with every parent blank, so the resolution
// of a blank node is its subtree's leaves left to right and the resolution of a leaf is itself.
//
// It is small on purpose. The sweep above is the one that covers five tree sizes and every
// exclusion; this exists so that the two of them are not one function's answer read back to
// itself.
func TestEncryptUpdatePathPairsAgainstAHandDerivedResolution(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	fixture, members := newTestTree(t, crypto, 4)
	// leaves 0 and 1 are siblings under node 1, so their first copath child is the other one
	// and their second is node 5, which is blank and resolves to leaves 2 and 3 in that order;
	// leaves 2 and 3 are siblings under node 5 whose second copath child is node 1, resolving
	// to leaves 0 and 1.
	handDerived := map[LeafIndex][][]LeafIndex{
		0: {{1}, {2, 3}},
		1: {{0}, {2, 3}},
		2: {{3}, {0, 1}},
		3: {{2}, {0, 1}},
	}
	if len(handDerived) != len(members) {
		t.Fatalf("the table names %d leaves and the fixture has %d", len(handDerived), len(members))
	}
	for _, member := range members {
		want, named := handDerived[member.LeafIndex]
		if !named {
			t.Fatalf("the table names no resolution for leaf %d and the fixture has one",
				member.LeafIndex)
		}
		tree := fixture.Clone()
		plan, err := tree.CreateUpdatePathSecrets(crypto, member.LeafIndex,
			member.SignaturePriv, testGroupId())
		if err != nil {
			t.Fatalf("leaf %d: CreateUpdatePathSecrets: %v", member.LeafIndex, err)
		}
		treeHash, err := tree.TreeHash(crypto)
		if err != nil {
			t.Fatalf("leaf %d: TreeHash: %v", member.LeafIndex, err)
		}
		groupContext := testUpdatePathContext(t, treeHash)
		path, err := tree.EncryptUpdatePath(crypto, plan, member.LeafIndex, groupContext, nil)
		if err != nil {
			t.Fatalf("leaf %d: EncryptUpdatePath: %v", member.LeafIndex, err)
		}
		if len(path.Nodes) != len(want) {
			t.Fatalf("leaf %d published %d path nodes and the geometry of a four member tree gives it %d",
				member.LeafIndex, len(path.Nodes), len(want))
		}
		for i := range want {
			expected := []string{}
			for j, opener := range want[i] {
				expected = append(expected, fmt.Sprintf("leaf %d opens ciphertext %d", opener, j))
			}
			slices.Sort(expected)
			got := updatePathOpenings(t, crypto, members, groupContext, path.Nodes[i],
				plan.PathSecrets[i])
			if !slices.Equal(got, expected) {
				t.Errorf("leaf %d at path node %d: the ciphertexts open as %v and the hand derived resolution of that node's copath child is %v",
					member.LeafIndex, i, got, expected)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// task 21: merging a received UpdatePath into the public tree
// ---------------------------------------------------------------------------
//
// A merge is a REFUSAL SURFACE before it is a mutation, and that is what decides the shape of
// everything below. The argument arrives off the wire from somebody who may be hostile, and the
// worst outcome is not a rejected commit -- it is a HALF merged tree, where the previous epoch's
// keys on the sender's path are already gone, the new ones are only partly in, and the group is
// left in a state no member agreed to and no caller can undo. So every refusal below is asserted
// twice: once for the sentinel, and once for the whole node array being what it was.
//
// The other half is that a round trip through this package's own generator proves the code agrees
// with itself and nothing else. A merge that recomputed the chain with the copath child derived
// backwards, or that skipped section 7.9.2's third condition, reproduces the sender's tree
// exactly -- the sender made the same mistake. What separates those is the published corpus at
// the bottom of this block: trees and paths produced by other implementations, with the tree hash
// of the merged tree published beside them.

// createAndEncryptPath is the sender's half of one commit, ready for a receiver: the tree the
// sender ends up with, the path it publishes, the plan it kept, and the group context it sealed
// under.
//
// The sender works on a CLONE, so the tree handed in is left at the epoch it was at and every
// caller below can use it as the receiver's starting point. That is the whole shape a receiver
// test needs and it is why this helper exists: a test that committed over the tree it then
// merged into would be comparing a tree against itself.
//
// The context is testUpdatePathContext's serialized GroupContext over the tree hash of the epoch
// the commit OPENS, which is the one task 20 requires and the one the published corpus uses. A
// context assembled some other way here would still round trip, because the same bytes would go
// into the seal and the open.
func createAndEncryptPath(t *testing.T, crypto CryptoProvider, tree *RatchetTree,
	member *testTreeMember, exclude []LeafIndex) (*RatchetTree, *UpdatePath, *UpdatePathPlan, []byte) {
	t.Helper()
	senderTree := tree.Clone()
	plan, err := senderTree.CreateUpdatePathSecrets(crypto, member.LeafIndex,
		member.SignaturePriv, testGroupId())
	if err != nil {
		t.Fatalf("CreateUpdatePathSecrets: %v", err)
	}
	treeHash, err := senderTree.TreeHash(crypto)
	if err != nil {
		t.Fatalf("TreeHash: %v", err)
	}
	groupContext := testUpdatePathContext(t, treeHash)
	path, err := senderTree.EncryptUpdatePath(crypto, plan, member.LeafIndex, groupContext, exclude)
	if err != nil {
		t.Fatalf("EncryptUpdatePath: %v", err)
	}
	return senderTree, path, plan, groupContext
}

// treeSnapshot is every position of a node array rendered as text, blanks included.
//
// A tree HASH is not what an atomicity assertion wants and the difference is the point. The tree
// hash is a digest of the fields section 7.8 hashes, so two trees that agree on it agree on those
// fields; what a refused merge must leave alone is the STORAGE, which includes the parent_hash
// field of every node the chain walk writes through on its way to discovering the refusal. That
// field is not in a leaf's tree hash input under any source but commit, and a partial merge that
// rewrote it on an update sourced leaf would leave the tree hash exactly where it was.
//
// The rendering goes through fmt rather than through the codec because the codec REFUSES a tree
// that ends in a blank node, which is a shape several of the fixtures below deliberately have.
func treeSnapshot(tree *RatchetTree) []string {
	out := []string{}
	for x := uint32(0); x < tree.NodeWidth(); x += 1 {
		node := tree.Get(NodeIndex(x))
		switch {
		case node == nil:
			out = append(out, fmt.Sprintf("%d blank", x))
		case node.Leaf != nil:
			out = append(out, fmt.Sprintf("%d leaf type=%d key=%x signature_key=%x source=%d parent_hash=%x signature=%x",
				x, node.NodeType, node.Leaf.EncryptionKey, node.Leaf.SignatureKey,
				node.Leaf.LeafNodeSource, node.Leaf.ParentHash, node.Leaf.Signature))
		case node.Parent != nil:
			out = append(out, fmt.Sprintf("%d parent type=%d key=%x parent_hash=%x unmerged=%v",
				x, node.NodeType, node.Parent.EncryptionKey, node.Parent.ParentHash,
				node.Parent.UnmergedLeaves))
		default:
			out = append(out, fmt.Sprintf("%d occupied and empty", x))
		}
	}
	return out
}

// mergeMustNotTouchTheTree drives one refusal and asserts both halves of it: the sentinel, and
// the node array being byte for byte what it was.
//
// One helper rather than the same eight lines at every refusal, because the atomicity half is the
// one a test is likeliest to be written without -- the sentinel is what the plan's own tests
// assert and it is satisfied by a body that mutated everything and then returned an error on the
// way out.
func mergeMustNotTouchTheTree(t *testing.T, crypto CryptoProvider, tree *RatchetTree,
	sender LeafIndex, path *UpdatePath, want error, what string) {
	t.Helper()
	before := treeSnapshot(tree)
	err := tree.MergeUpdatePath(crypto, sender, path)
	if !errors.Is(err, want) {
		t.Errorf("%s: err = %v, want %v", what, err, want)
	}
	after := treeSnapshot(tree)
	if len(before) != len(after) {
		t.Fatalf("%s: a refused merge changed the width of the node array, from %d to %d",
			what, len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("%s: a refused merge mutated the tree at node %d:\n before %s\n after  %s",
				what, i, before[i], after[i])
		}
	}
}

// TestMergeUpdatePathReproducesTheSendersTree is the plan's own round trip: a receiver that
// merges a published path ends up at the sender's tree hash, and the tree it ends up with passes
// section 7.9.2.
//
// It is the necessary half and not the sufficient one. Both sides of this comparison are this
// package's: the sender built the chain in task 18 and the receiver rebuilds it here, so a copath
// child derived backwards, a chain walked leaf-up instead of root-down, or a third condition
// nobody checks agrees with itself at both ends and produces exactly this run. The corpus test at
// the end of this block is the half that cannot.
func TestMergeUpdatePathReproducesTheSendersTree(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	for _, n := range []uint32{2, 3, 4, 7, 8} {
		tree, members := newTestTree(t, crypto, n)
		senderTree, path, _, _ := createAndEncryptPath(t, crypto, tree, members[0], nil)
		receiverTree := tree.Clone()
		if err := receiverTree.MergeUpdatePath(crypto, members[0].LeafIndex, path); err != nil {
			t.Fatalf("n=%d MergeUpdatePath: %v", n, err)
		}
		// the whole node array and not the tree hash, for treeSnapshot's reason: the receiver has
		// to end up at the sender's STORAGE, parent hash fields included, because those fields are
		// what the next epoch's chain is taken over.
		got, want := treeSnapshot(receiverTree), treeSnapshot(senderTree)
		if len(got) != len(want) {
			t.Fatalf("n=%d the merged tree holds %d nodes and the sender's holds %d",
				n, len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("n=%d node %d after the merge is\n %s\nand the sender has\n %s", n, i, got[i], want[i])
			}
		}
		wantHash, err := senderTree.TreeHash(crypto)
		if err != nil {
			t.Fatalf("n=%d sender TreeHash: %v", n, err)
		}
		gotHash, err := receiverTree.TreeHash(crypto)
		if err != nil {
			t.Fatalf("n=%d receiver TreeHash: %v", n, err)
		}
		if !bytes.Equal(gotHash, wantHash) {
			t.Fatalf("n=%d the merged tree hash differs from the sender's", n)
		}
		if err := receiverTree.VerifyParentHashes(crypto); err != nil {
			t.Fatalf("n=%d VerifyParentHashes: %v", n, err)
		}
	}
}

// TestMergeUpdatePathRejectsAWrongLengthPath is ValSem202 in both directions, and the tree is
// untouched by either.
func TestMergeUpdatePathRejectsAWrongLengthPath(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, members := newTestTree(t, crypto, 4)
	_, path, _, _ := createAndEncryptPath(t, crypto, tree, members[0], nil)
	short := &UpdatePath{LeafNode: path.LeafNode, Nodes: path.Nodes[:len(path.Nodes)-1]}
	receiverTree := tree.Clone()
	mergeMustNotTouchTheTree(t, crypto, receiverTree, members[0].LeafIndex, short,
		errPathLength, "a path one node short")
	long := &UpdatePath{LeafNode: path.LeafNode,
		Nodes: append(append([]UpdatePathNode{}, path.Nodes...), path.Nodes[0])}
	mergeMustNotTouchTheTree(t, crypto, receiverTree, members[0].LeafIndex, long,
		errPathLength, "a path one node long")
}

// TestMergeUpdatePathRejectsATamperedNodeKey is the chain comparison doing its job: a node key
// nobody signed cannot be made to fit under the leaf's own signed parent_hash.
func TestMergeUpdatePathRejectsATamperedNodeKey(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, members := newTestTree(t, crypto, 4)
	_, path, _, _ := createAndEncryptPath(t, crypto, tree, members[0], nil)
	// every node of the path in turn, rather than only the topmost: a chain that hashed one node
	// twice, or that stopped one short of the root, is invisible to a fixture that tampers with
	// the one node such a chain still covers.
	for at := range path.Nodes {
		tampered := &UpdatePath{LeafNode: path.LeafNode,
			Nodes: append([]UpdatePathNode{}, path.Nodes...)}
		tampered.Nodes[at].EncryptionKey =
			HpkePublicKey(bytes.Repeat([]byte{0xEE}, len(path.Nodes[at].EncryptionKey)))
		mergeMustNotTouchTheTree(t, crypto, tree.Clone(), members[0].LeafIndex, tampered,
			ErrParentHashMismatch, fmt.Sprintf("node %d of the path carrying a key nobody signed", at))
	}
	// and the same refusal from the other end: the leaf's own field moved instead of the keys.
	moved := &UpdatePath{LeafNode: *path.LeafNode.Clone(),
		Nodes: append([]UpdatePathNode{}, path.Nodes...)}
	moved.LeafNode.ParentHash = bytes.Repeat([]byte{0x5A}, len(path.LeafNode.ParentHash))
	mergeMustNotTouchTheTree(t, crypto, tree.Clone(), members[0].LeafIndex, moved,
		ErrParentHashMismatch, "a leaf claiming a parent hash the path does not produce")
	// and a leaf carrying no parent hash at all, which the length arm of the sanctioned
	// comparison refuses rather than a length clause written in front of it.
	empty := &UpdatePath{LeafNode: *path.LeafNode.Clone(),
		Nodes: append([]UpdatePathNode{}, path.Nodes...)}
	empty.LeafNode.ParentHash = nil
	mergeMustNotTouchTheTree(t, crypto, tree.Clone(), members[0].LeafIndex, empty,
		ErrParentHashMismatch, "a leaf carrying no parent hash")
}

// parentHashChainOfAMergedPath is section 7.9.2's SECOND condition on its own: the merged chain
// recomputed root down, with nothing said about the resolutions those nodes sit in.
//
// It exists so the test below can state what the weaker rule accepts. This is the whole of the
// check a merge would make if it re-derived section 7.9.2 from a one-condition reading, and the
// splice fixture makes it agree while the real merge refuses -- which is the only way to say that
// the third condition is load bearing rather than decorative.
func parentHashChainOfAMergedPath(t *testing.T, crypto CryptoProvider, tree *RatchetTree,
	sender LeafIndex, path *UpdatePath) []byte {
	t.Helper()
	steps, err := tree.filteredPathSteps(sender)
	if err != nil {
		t.Fatalf("filteredPathSteps: %v", err)
	}
	if len(steps) != len(path.Nodes) {
		t.Fatalf("the fixture path carries %d nodes for a filtered path of %d steps",
			len(path.Nodes), len(steps))
	}
	working := tree.Clone()
	if err := working.BlankDirectPath(sender); err != nil {
		t.Fatalf("BlankDirectPath: %v", err)
	}
	for i, step := range steps {
		if err := working.SetParent(step.Node, &ParentNode{
			EncryptionKey: HpkePublicKey(cloneBytes(path.Nodes[i].EncryptionKey)),
		}); err != nil {
			t.Fatalf("SetParent: %v", err)
		}
	}
	carried := []byte{}
	for i := len(steps) - 1; i >= 0; i -= 1 {
		parent := working.ParentAt(steps[i].Node)
		if parent == nil {
			t.Fatalf("node %d of the filtered path is blank after being installed", steps[i].Node)
		}
		parent.ParentHash = carried
		hash, err := working.ParentHash(crypto, steps[i].Node, steps[i].CopathChild)
		if err != nil {
			t.Fatalf("ParentHash: %v", err)
		}
		carried = hash
	}
	return carried
}

// TestMergeUpdatePathRefusesASplicedSubtreeThatTheChainAloneAccepts is the reason the merge calls
// section 7.9.2's whole rule instead of restating its second condition.
//
// The fixture is the attack the third condition exists for. Somebody puts a parent node of their
// own into a subtree they have never committed over -- a public key whose private half they hold,
// at a position no member's leaf attests to. Nothing about the next honest commit notices: the
// splice is not on the committer's direct path, so the chain the committer builds and the chain
// the receiver rebuilds agree perfectly, and this test SAYS SO by recomputing that chain and
// requiring it to match the leaf's signed field. What the splice does do is sit in the resolution
// of a copath child, so the honest commit seals a path secret straight to a key the splicer can
// open -- and the splicer is not in the subtree it read.
//
// Section 7.9.2's third condition is what refuses it: the spliced node has no descendant that is
// parent-hash valid with respect to it, so it is claimed zero times where the rule requires
// exactly one. A merge that re-derived the rule from a version holding only the parent_hash
// equality accepts this tree, and the run it produces is identical to a correct one everywhere
// else in this file.
func TestMergeUpdatePathRefusesASplicedSubtreeThatTheChainAloneAccepts(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, members := newTestTree(t, crypto, 4)
	// the control first: with no splice, this commit merges.
	_, path, _, _ := createAndEncryptPath(t, crypto, tree, members[1], nil)
	if err := tree.Clone().MergeUpdatePath(crypto, members[1].LeafIndex, path); err != nil {
		t.Fatalf("the unspliced control did not merge: %v", err)
	}

	// the splice: an attacker's key at node 5, the parent of leaves 2 and 3, which no leaf of
	// this tree claims. Installed through SetParent rather than by hand so that everything the
	// container enforces about a parent node still holds -- what is wrong with this tree is the
	// section 7.9.2 relation and nothing structural.
	splicedPriv, splicedPub, err := crypto.DeriveKeyPair(crypto.Random(crypto.HashSize()))
	if err != nil {
		t.Fatalf("DeriveKeyPair: %v", err)
	}
	spliced := tree.Clone()
	if err := spliced.SetParent(5, &ParentNode{EncryptionKey: splicedPub}); err != nil {
		t.Fatalf("SetParent(5): %v", err)
	}
	// what this fixture cannot separate, recorded where the fixture is built rather than left for
	// a reader to discover. The splice makes the RECEIVER's tree section 7.9.2 invalid too, so the
	// refusal at the end of this test arrives whether the merge sweeps the tree it is about to
	// adopt or the one it is about to replace -- measured, not supposed: with
	// provisional.VerifyParentHashes written as self.VerifyParentHashes this test passed. The
	// placement is held by TestMergeUpdatePathJudgesTheMergedTreeAndNotTheOneItReplaces, from the
	// only direction that can reach it, and by the two refusals whose base tree is valid.
	if err := spliced.VerifyParentHashes(crypto); err == nil {
		t.Fatal("the spliced tree satisfies section 7.9.2 on its own, so the paragraph above is no longer true and this test now claims more than it holds")
	}
	// the commit an honest member 1 publishes over the spliced tree. Node 5 is now non-blank, so
	// it IS the resolution of the copath child of node 3, and this commit seals node 3's path
	// secret to the splicer's key.
	_, splicedPath, plan, groupContext := createAndEncryptPath(t, crypto, spliced, members[1], nil)
	steps, err := spliced.filteredPathSteps(members[1].LeafIndex)
	if err != nil {
		t.Fatalf("filteredPathSteps: %v", err)
	}
	targets, err := spliced.EncryptionTargets(members[1].LeafIndex, nil)
	if err != nil {
		t.Fatalf("EncryptionTargets: %v", err)
	}
	sealedToTheSplice := false
	for i, step := range steps {
		if step.Node != 3 {
			continue
		}
		if !equalNodeIndices(targets[i], []NodeIndex{5}) {
			t.Fatalf("the copath resolution at node 3 is %v and the fixture needs it to be the spliced node alone",
				targets[i])
		}
		// the fixture's own premise, asserted rather than assumed: the honest commit really does
		// hand this secret to the splicer. Without this the test could pass over a tree where the
		// splice was harmless and the refusal was about something else.
		ct := splicedPath.Nodes[i].EncryptedPathSecret[0]
		opened, err := OpenWithLabel(crypto, splicedPriv, updatePathNodeLabel, groupContext, &ct)
		if err == nil && bytes.Equal(opened, plan.PathSecrets[i]) {
			sealedToTheSplice = true
		}
	}
	if !sealedToTheSplice {
		t.Fatal("the fixture's spliced node did not receive the commit's path secret, so it is not the attack the third condition is about")
	}

	// the weaker rule accepts it: the chain the receiver rebuilds is exactly the leaf's signed
	// parent hash.
	carried := parentHashChainOfAMergedPath(t, crypto, spliced, members[1].LeafIndex, splicedPath)
	if !bytes.Equal(carried, splicedPath.LeafNode.ParentHash) {
		t.Fatalf("the recomputed chain is %x and the leaf claims %x; this fixture is meant to be one the parent_hash equality accepts",
			carried, splicedPath.LeafNode.ParentHash)
	}
	// and the whole rule refuses it, without touching the tree.
	mergeMustNotTouchTheTree(t, crypto, spliced, members[1].LeafIndex, splicedPath,
		ErrParentHashMismatch, "an honest commit over a tree carrying a spliced parent node")
}

// TestMergeUpdatePathRefusesALeafWhoseSourceCarriesNoParentHash is the derived half of the
// section 7.6 rule that an UpdatePath's leaf is commit sourced.
//
// The merge states no rule about the source of its own. What it relies on is
// nodeParentHashField, which answers "carries no parent_hash field" for every source but commit
// -- so a path whose leaf claims another source leaves the node above it with no claimant, and
// section 7.9.2's exactly-one requirement is what refuses it. The sweep is over every source the
// package DECLARES rather than over the one wrong source somebody would have written down, so a
// fourth source is judged by this on the commit that declares it.
func TestMergeUpdatePathRefusesALeafWhoseSourceCarriesNoParentHash(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, members := newTestTree(t, crypto, 4)
	_, path, _, _ := createAndEncryptPath(t, crypto, tree, members[0], nil)
	accepted, refused := 0, 0
	for _, source := range leafNodeSources(t) {
		candidate := &UpdatePath{LeafNode: *path.LeafNode.Clone(),
			Nodes: append([]UpdatePathNode{}, path.Nodes...)}
		candidate.LeafNode.LeafNodeSource = source
		if source == LeafNodeSourceCommit {
			if err := tree.Clone().MergeUpdatePath(crypto, members[0].LeafIndex, candidate); err != nil {
				t.Errorf("the commit sourced leaf was refused: %v", err)
			}
			accepted += 1
			continue
		}
		mergeMustNotTouchTheTree(t, crypto, tree.Clone(), members[0].LeafIndex, candidate,
			ErrParentHashMismatch, fmt.Sprintf("a path whose leaf claims source %d", source))
		refused += 1
	}
	if accepted != 1 || refused != len(leafNodeSources(t))-1 {
		t.Fatalf("the merge accepted %d leaves and refused %d over the %d sources this package declares",
			accepted, refused, len(leafNodeSources(t)))
	}
}

// TestMergeUpdatePathTellsABlankLeafApartFromAnIndexOutsideTheTree holds the same split
// CreateUpdatePathSecrets makes, at the receiving door.
//
// The two faults have opposite repairs -- recompute an index, or rejoin the group -- so one
// sentinel for both sends a member whose position the group removed to go and check the index
// that was right the whole time.
func TestMergeUpdatePathTellsABlankLeafApartFromAnIndexOutsideTheTree(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, members := newTestTree(t, crypto, 4)
	_, path, _, _ := createAndEncryptPath(t, crypto, tree, members[0], nil)
	outside := tree.Clone()
	mergeMustNotTouchTheTree(t, crypto, outside, LeafIndex(tree.LeafWidth()), path,
		ErrLeafIndexOutOfRange, "a sender index past the width of the tree")
	blanked := tree.Clone()
	if err := blanked.RemoveLeaf(members[2].LeafIndex); err != nil {
		t.Fatalf("RemoveLeaf: %v", err)
	}
	mergeMustNotTouchTheTree(t, crypto, blanked, members[2].LeafIndex, path,
		ErrLeafBlank, "a sender whose leaf the group removed")
	if errors.Is(ErrLeafBlank, ErrLeafIndexOutOfRange) {
		t.Fatal("ErrLeafBlank answers to ErrLeafIndexOutOfRange, so the split above is not one")
	}
}

// TestMergeUpdatePathRefusesANilPathRatherThanDereferencingIt is errNilUpdatePath's own control.
func TestMergeUpdatePathRefusesANilPathRatherThanDereferencingIt(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, members := newTestTree(t, crypto, 4)
	mergeMustNotTouchTheTree(t, crypto, tree, members[0].LeafIndex, nil,
		errNilUpdatePath, "no path at all")
	if _, err := tree.DecryptUpdatePath(crypto, members[0].LeafIndex, nil, nil,
		NewTreeKEMPrivate(members[1].LeafIndex, members[1].EncryptionPriv), nil); !errors.Is(err, errNilUpdatePath) {
		t.Fatalf("DecryptUpdatePath with no path gave err = %v, want errNilUpdatePath", err)
	}
	if _, err := tree.DecryptUpdatePath(crypto, members[0].LeafIndex, &UpdatePath{}, nil,
		nil, nil); !errors.Is(err, errNilTreeKEMPrivate) {
		t.Fatalf("DecryptUpdatePath with no private state gave err = %v, want errNilTreeKEMPrivate", err)
	}
}

// ---------------------------------------------------------------------------
// task 21 and 22 against the published corpus
// ---------------------------------------------------------------------------

// treekemReceivedUpdatePath is the update_paths half of one entry of treekem.json, which is the
// half the private-state sweep near the top of this file deliberately left alone.
//
// path_secrets is one entry per LEAF INDEX and not one per member, null where that leaf cannot
// decrypt this path -- the sender's own index, and every leaf outside the sender's copath. The
// pointer is what carries the null: a []string would decode a JSON null into the empty string,
// which is a value this sweep would then compare against and pass.
type treekemReceivedUpdatePath struct {
	Sender        uint32    `json:"sender"`
	UpdatePath    string    `json:"update_path"`
	PathSecrets   []*string `json:"path_secrets"`
	CommitSecret  string    `json:"commit_secret"`
	TreeHashAfter string    `json:"tree_hash_after"`
}

// treekemReceiverVector is everything one entry of treekem.json says about receiving a path: the
// epoch's tree, the private state of the members that can decrypt, the group context fields, and
// the paths themselves.
type treekemReceiverVector struct {
	CipherSuite             uint16                      `json:"cipher_suite"`
	GroupId                 string                      `json:"group_id"`
	Epoch                   uint64                      `json:"epoch"`
	ConfirmedTranscriptHash string                      `json:"confirmed_transcript_hash"`
	RatchetTree             string                      `json:"ratchet_tree"`
	LeavesPrivate           []treekemLeafPrivateVector  `json:"leaves_private"`
	UpdatePaths             []treekemReceivedUpdatePath `json:"update_paths"`
}

// groupContext is the HPKE info every ciphertext of one published path was sealed under: the
// entry's own group id, epoch and confirmed transcript hash, with the tree hash of the tree AFTER
// that path is merged.
//
// The tree hash is the field that makes this the context of the epoch the commit OPENS, and it is
// the reason generation and encryption are two calls in this package. A context built over the
// tree hash the epoch started from is a context every other implementation computes differently,
// and nothing in a self consistent seal and open can tell the two apart -- which is exactly why
// the corpus is where it has to be said.
func (self *treekemReceiverVector) groupContext(t *testing.T, treeHash []byte) []byte {
	t.Helper()
	encoded, err := syntax.Marshal(&GroupContext{
		Version:                 ProtocolVersionMls10,
		CipherSuite:             CipherSuite(self.CipherSuite),
		GroupId:                 MustHex(t, self.GroupId),
		Epoch:                   self.Epoch,
		TreeHash:                treeHash,
		ConfirmedTranscriptHash: MustHex(t, self.ConfirmedTranscriptHash),
	})
	if err != nil {
		t.Fatalf("Marshal(GroupContext): %v", err)
	}
	return encoded
}

// private is the published private state of one leaf, or false where the file publishes none.
func (self *treekemReceiverVector) private(t *testing.T, index uint32) (*TreeKEMPrivate, bool) {
	t.Helper()
	for _, entry := range self.LeavesPrivate {
		if entry.Index != index {
			continue
		}
		priv := NewTreeKEMPrivate(LeafIndex(entry.Index),
			HpkePrivateKey(MustHex(t, entry.EncryptionPriv)))
		for _, rung := range entry.PathSecrets {
			priv.PathSecrets[NodeIndex(rung.Node)] = MustHex(t, rung.PathSecret)
		}
		return priv, true
	}
	return nil, false
}

// Transcriptions of what treekem.json holds at the pinned mlswg commit for the suites this
// package implements. A filter that matched nothing, a loop that read a field that is not there
// and a decoder that stopped early all report exactly what a complete run reports without these.
const (
	treekemReceiverEntryCount   = 22
	treekemReceiverPathCount    = 124
	treekemReceiverDecryptCount = 656
)

// forEachPublishedReceivedPath is the shared walk of the receiving half of treekem.json, so the
// merge sweep and the decrypt sweep run over provably the same set rather than over two filters
// that could drift.
func forEachPublishedReceivedPath(t *testing.T, visit func(at int, vector *treekemReceiverVector,
	crypto CryptoProvider, base *RatchetTree, published treekemReceivedUpdatePath, path *UpdatePath)) {
	t.Helper()
	entries := LoadVectorFile(t, "treekem.json")
	if len(entries) != treekemEntryCount {
		t.Fatalf("treekem.json holds %d entries, want %d", len(entries), treekemEntryCount)
	}
	matched, declined := 0, 0
	for at, raw := range entries {
		var vector treekemReceiverVector
		if err := json.Unmarshal(raw, &vector); err != nil {
			t.Fatalf("entry %d: %v", at, err)
		}
		suite, ok := implementedSuite(vector.CipherSuite)
		if !ok {
			declined += 1
			continue
		}
		matched += 1
		crypto := mustProvider(t, suite)
		base, err := UnmarshalRatchetTree(MustHex(t, vector.RatchetTree))
		if err != nil {
			t.Fatalf("entry %d: UnmarshalRatchetTree: %v", at, err)
		}
		for _, published := range vector.UpdatePaths {
			path := &UpdatePath{}
			if err := syntax.Unmarshal(MustHex(t, published.UpdatePath), path); err != nil {
				t.Fatalf("entry %d, sender %d: Unmarshal(UpdatePath): %v", at, published.Sender, err)
			}
			visit(at, &vector, crypto, base, published, path)
		}
	}
	if matched+declined != len(entries) {
		t.Fatalf("%d entries matched and %d were declined, and the file holds %d",
			matched, declined, len(entries))
	}
	if matched != treekemReceiverEntryCount {
		t.Fatalf("the walk read %d entries at a registered suite, want %d",
			matched, treekemReceiverEntryCount)
	}
}

// TestEveryPublishedUpdatePathMergesToItsPublishedTreeHash is the only oracle for task 21 that is
// independent of this repository.
//
// Everything else in this block compares a merge against a generation the same reader wrote. Both
// halves are this package's, so a chain walked in the wrong direction, a copath child derived
// backwards, or a parent hash preimage with two fields transposed agrees with itself at both ends
// and reproduces the sender's tree exactly. The working group's implementations produced these
// trees, these paths and the tree hash of the merged result, so a merge that lands anywhere else
// fails here and nowhere else in this package.
//
// The negative control is what makes the green run mean something: every published path merges,
// so a merge that checked everything and one that installed the keys and returned nil produce
// identical runs, and only an input that must be refused separates them.
func TestEveryPublishedUpdatePathMergesToItsPublishedTreeHash(t *testing.T) {
	paths, refusals := 0, 0
	forEachPublishedReceivedPath(t, func(at int, vector *treekemReceiverVector, crypto CryptoProvider,
		base *RatchetTree, published treekemReceivedUpdatePath, path *UpdatePath) {
		paths += 1
		merged := base.Clone()
		if err := merged.MergeUpdatePath(crypto, LeafIndex(published.Sender), path); err != nil {
			t.Fatalf("entry %d, sender %d: MergeUpdatePath: %v", at, published.Sender, err)
		}
		treeHash, err := merged.TreeHash(crypto)
		if err != nil {
			t.Fatalf("entry %d, sender %d: TreeHash: %v", at, published.Sender, err)
		}
		if want := MustHex(t, published.TreeHashAfter); !bytes.Equal(treeHash, want) {
			t.Fatalf("entry %d, sender %d: the merged tree hashes to %s and the corpus publishes %s",
				at, published.Sender, HexOf(treeHash), published.TreeHashAfter)
		}
		// section 7.9.2 over another implementation's tree, which is the half a fixture built by
		// this package cannot state: every non-blank parent of the merged tree has to be claimed
		// exactly once, including the parents this commit never touched.
		if err := merged.VerifyParentHashes(crypto); err != nil {
			t.Fatalf("entry %d, sender %d: the merged tree is not parent-hash valid: %v",
				at, published.Sender, err)
		}
		// the control, per case rather than once: one octet of one announced key flipped, the
		// refusal required, and the tree left as it was.
		tampered := &UpdatePath{LeafNode: *path.LeafNode.Clone(),
			Nodes: append([]UpdatePathNode{}, path.Nodes...)}
		flipped := cloneBytes(tampered.Nodes[0].EncryptionKey)
		flipped[0] ^= 0x01
		tampered.Nodes[0].EncryptionKey = HpkePublicKey(flipped)
		control := base.Clone()
		before := treeSnapshot(control)
		if err := control.MergeUpdatePath(crypto, LeafIndex(published.Sender), tampered); !errors.Is(err, ErrParentHashMismatch) {
			t.Fatalf("entry %d, sender %d: one octet of a published node key flipped gave err = %v, want ErrParentHashMismatch",
				at, published.Sender, err)
		}
		if !slices.Equal(before, treeSnapshot(control)) {
			t.Fatalf("entry %d, sender %d: a refused merge mutated the tree", at, published.Sender)
		}
		refusals += 1
	})
	if paths != treekemReceiverPathCount || refusals != treekemReceiverPathCount {
		t.Fatalf("the run merged %d published paths and refused %d tampered ones, want %d of each",
			paths, refusals, treekemReceiverPathCount)
	}
	t.Logf("%d published update paths merged to their published tree hash, %d tampered controls refused",
		paths, refusals)
}

// ---------------------------------------------------------------------------
// task 22: decrypting a received UpdatePath
// ---------------------------------------------------------------------------
//
// This is where a wrong resolution finally shows, and the point of the assertions below is that
// they say WHICH thing was wrong. Task 16's resolution, task 20's positional pairing and task
// 22's own entry point all fail here as "the ciphertext did not open", and a test that asserted
// only that a decrypt succeeded would leave the next reader with three suspects and no way to
// separate them. So the entry point is derived from the tree math in the test, the ciphertext
// index it names is opened directly, and the ciphertexts it does NOT name are required not to
// open.

// updatePathEntryFor is where a receiver's own ciphertext stands, derived from the tree math and
// the private state rather than from the call under test: the index of the lowest node of the
// sender's filtered path that covers this member, and the position within that node's copath
// resolution of the first entry this member can derive a key for.
//
// Derived here a second time on purpose. The decrypt computes the same two numbers, and a test
// that read them back off the decrypt would agree with whatever the decrypt did.
func updatePathEntryFor(t *testing.T, crypto CryptoProvider, tree *RatchetTree, sender LeafIndex,
	priv *TreeKEMPrivate, exclude []LeafIndex) (step int, entry NodeIndex, position int, found bool) {
	t.Helper()
	steps, err := tree.filteredPathSteps(sender)
	if err != nil {
		t.Fatalf("filteredPathSteps: %v", err)
	}
	targets, err := tree.EncryptionTargets(sender, exclude)
	if err != nil {
		t.Fatalf("EncryptionTargets: %v", err)
	}
	lowest := CommonAncestor(sender.NodeIndex(), priv.LeafIndex.NodeIndex())
	at := -1
	for i := range steps {
		if steps[i].Node == lowest {
			at = i
		}
	}
	if at < 0 {
		return 0, 0, 0, false
	}
	for j, y := range targets[at] {
		_, held, err := priv.NodePrivateKey(crypto, y)
		if err != nil {
			t.Fatalf("NodePrivateKey: %v", err)
		}
		if held {
			return at, y, j, true
		}
	}
	return at, 0, 0, false
}

// TestDecryptUpdatePathAgreesOnTheCommitSecret is the plan's sweep: every member other than the
// sender opens the path, reaches the sender's commit secret, ends up with a private state that
// agrees with the merged tree, and holds a secret for its own entry point.
func TestDecryptUpdatePathAgreesOnTheCommitSecret(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	receivers := 0
	for _, n := range []uint32{2, 3, 4, 7, 8} {
		tree, members := newTestTree(t, crypto, n)
		_, path, plan, groupContext := createAndEncryptPath(t, crypto, tree, members[0], nil)
		for _, receiver := range members[1:] {
			receiverTree := tree.Clone()
			if err := receiverTree.MergeUpdatePath(crypto, members[0].LeafIndex, path); err != nil {
				t.Fatalf("n=%d receiver %d MergeUpdatePath: %v", n, receiver.LeafIndex, err)
			}
			priv := NewTreeKEMPrivate(receiver.LeafIndex, receiver.EncryptionPriv)
			got, err := receiverTree.DecryptUpdatePath(crypto, members[0].LeafIndex, path,
				groupContext, priv, nil)
			if err != nil {
				t.Fatalf("n=%d receiver %d DecryptUpdatePath: %v", n, receiver.LeafIndex, err)
			}
			if !bytes.Equal(got.CommitSecret, plan.CommitSecret) {
				t.Fatalf("n=%d receiver %d commit secret differs from the sender's", n, receiver.LeafIndex)
			}
			if err := got.Private.Consistent(crypto, receiverTree); err != nil {
				t.Fatalf("n=%d receiver %d private state: %v", n, receiver.LeafIndex, err)
			}
			// the receiver learns the secrets from its own entry point up to the root and nothing
			// below it, which is the ASYMMETRY the whole construction rests on rather than a
			// property of this call.
			lowest := CommonAncestor(members[0].LeafIndex.NodeIndex(), receiver.LeafIndex.NodeIndex())
			if _, ok := got.Private.PathSecrets[lowest]; !ok {
				t.Fatalf("n=%d receiver %d did not learn the secret for node %d",
					n, receiver.LeafIndex, lowest)
			}
			steps, err := receiverTree.filteredPathSteps(members[0].LeafIndex)
			if err != nil {
				t.Fatalf("filteredPathSteps: %v", err)
			}
			for i := range steps {
				if steps[i].Node == lowest {
					break
				}
				if _, ok := got.Private.PathSecrets[steps[i].Node]; ok {
					t.Fatalf("n=%d receiver %d learned the secret of node %d, which is BELOW its entry point at node %d",
						n, receiver.LeafIndex, steps[i].Node, lowest)
				}
			}
			// the receiver's own leaf key survives the decrypt: NodePrivateKey's own-leaf arm
			// answers a copy precisely so this call can erase what it was handed, and erasing the
			// original would leave the member unable to decrypt anything ever again.
			if !bytes.Equal(got.Private.EncryptionPriv, receiver.EncryptionPriv) {
				t.Fatalf("n=%d receiver %d: the decrypt changed the member's own leaf private key",
					n, receiver.LeafIndex)
			}
			receivers += 1
		}
	}
	if receivers == 0 {
		t.Fatal("no receiver was swept")
	}
	t.Logf("%d receivers reached the sender's commit secret", receivers)
}

// TestDecryptUpdatePathOpensTheCiphertextStandingAtItsOwnResolutionIndex is the pairing stated as
// an assertion, and it is what tells a wrong resolution from a wrong pairing from a wrong ladder.
//
// Three separate claims, each about a different one of the three:
//
//   - the secret recovered at the entry point is the SENDER'S OWN RUNG at that step. A decrypt
//     that opened the wrong node's ciphertext, or that started its ladder one rung off, reaches a
//     different value here even when everything above still derives.
//   - the ciphertext the entry point NAMES opens under the key this member holds for that
//     resolution entry, which is task 20's positional pairing read from the receiving end.
//   - no OTHER ciphertext at that node opens under that key. This is the half that separates a
//     decrypt which walked to the right index from one that tried them all: trial decryption
//     agrees with a correct sender and agrees just as well with a permuted one.
func TestDecryptUpdatePathOpensTheCiphertextStandingAtItsOwnResolutionIndex(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	checked, separating := 0, 0
	for _, n := range []uint32{2, 3, 4, 7, 8} {
		tree, members := newTestTree(t, crypto, n)
		_, path, plan, groupContext := createAndEncryptPath(t, crypto, tree, members[0], nil)
		for _, receiver := range members[1:] {
			receiverTree := tree.Clone()
			if err := receiverTree.MergeUpdatePath(crypto, members[0].LeafIndex, path); err != nil {
				t.Fatalf("n=%d receiver %d MergeUpdatePath: %v", n, receiver.LeafIndex, err)
			}
			priv := NewTreeKEMPrivate(receiver.LeafIndex, receiver.EncryptionPriv)
			step, entry, position, found := updatePathEntryFor(t, crypto, receiverTree,
				members[0].LeafIndex, priv, nil)
			if !found {
				t.Fatalf("n=%d receiver %d: the tree math finds no entry point for a member on the sender's copath",
					n, receiver.LeafIndex)
			}
			got, err := receiverTree.DecryptUpdatePath(crypto, members[0].LeafIndex, path,
				groupContext, priv, nil)
			if err != nil {
				t.Fatalf("n=%d receiver %d DecryptUpdatePath: %v", n, receiver.LeafIndex, err)
			}
			steps, err := receiverTree.filteredPathSteps(members[0].LeafIndex)
			if err != nil {
				t.Fatalf("filteredPathSteps: %v", err)
			}
			if !bytes.Equal(got.Private.PathSecrets[steps[step].Node], plan.PathSecrets[step]) {
				t.Fatalf("n=%d receiver %d: the secret recovered at node %d is not the rung the sender published there",
					n, receiver.LeafIndex, steps[step].Node)
			}
			nodePriv, held, err := priv.NodePrivateKey(crypto, entry)
			if err != nil || !held {
				t.Fatalf("n=%d receiver %d: no private key for resolution entry %d: held=%v err=%v",
					n, receiver.LeafIndex, entry, held, err)
			}
			ciphertexts := path.Nodes[step].EncryptedPathSecret
			opened, err := OpenWithLabel(crypto, nodePriv, updatePathNodeLabel, groupContext,
				&ciphertexts[position])
			if err != nil {
				t.Fatalf("n=%d receiver %d: ciphertext %d of node %d, which is where entry %d stands, did not open: %v",
					n, receiver.LeafIndex, position, steps[step].Node, entry, err)
			}
			if !bytes.Equal(opened, plan.PathSecrets[step]) {
				t.Fatalf("n=%d receiver %d: ciphertext %d of node %d opened to something other than that node's path secret",
					n, receiver.LeafIndex, position, steps[step].Node)
			}
			for k := range ciphertexts {
				if k == position {
					continue
				}
				if _, err := OpenWithLabel(crypto, nodePriv, updatePathNodeLabel, groupContext,
					&ciphertexts[k]); err == nil {
					t.Fatalf("n=%d receiver %d: ciphertext %d of node %d also opened under the key of entry %d, so the pairing this decrypt reads is not unique",
						n, receiver.LeafIndex, k, steps[step].Node, entry)
				}
				separating += 1
			}
			checked += 1
		}
	}
	if separating == 0 {
		t.Fatal("no node of any fixture carried more than one ciphertext, so the uniqueness half of this test never ran and a decrypt that tried every index would pass it")
	}
	t.Logf("%d receivers opened the ciphertext standing at their own resolution index, %d neighbouring ciphertexts required not to open",
		checked, separating)
}

// TestDecryptUpdatePathRejectsATamperedCiphertext is ValSem203 from both directions a ciphertext
// can be wrong: the bytes changed, and the context they were sealed under changed.
func TestDecryptUpdatePathRejectsATamperedCiphertext(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, members := newTestTree(t, crypto, 4)
	_, path, _, groupContext := createAndEncryptPath(t, crypto, tree, members[0], nil)
	receiverTree := tree.Clone()
	if err := receiverTree.MergeUpdatePath(crypto, members[0].LeafIndex, path); err != nil {
		t.Fatalf("MergeUpdatePath: %v", err)
	}
	tampered := &UpdatePath{LeafNode: path.LeafNode, Nodes: append([]UpdatePathNode{}, path.Nodes...)}
	tampered.Nodes[0].EncryptedPathSecret = append([]HpkeCiphertext{},
		tampered.Nodes[0].EncryptedPathSecret...)
	corrupt := tampered.Nodes[0].EncryptedPathSecret[0]
	corrupt.Ciphertext = cloneBytes(corrupt.Ciphertext)
	corrupt.Ciphertext[0] ^= 0xFF
	tampered.Nodes[0].EncryptedPathSecret[0] = corrupt

	priv := NewTreeKEMPrivate(members[1].LeafIndex, members[1].EncryptionPriv)
	_, err = receiverTree.DecryptUpdatePath(crypto, members[0].LeafIndex, tampered,
		groupContext, priv, nil)
	if !errors.Is(err, errPathDecrypt) {
		t.Fatalf("err = %v, want errPathDecrypt", err)
	}
	// the same ciphertext under a different group context also fails to open, which is what says
	// the HPKE info is bound to the epoch rather than being decoration.
	priv = NewTreeKEMPrivate(members[1].LeafIndex, members[1].EncryptionPriv)
	_, err = receiverTree.DecryptUpdatePath(crypto, members[0].LeafIndex, path,
		[]byte("wrong context"), priv, nil)
	if !errors.Is(err, errPathDecrypt) {
		t.Fatalf("wrong context err = %v, want errPathDecrypt", err)
	}
	// and errPathDecrypt is not the answer for a member this commit simply did not seal to, which
	// is the distinction the two sentinels exist for.
	if errors.Is(errPathDecrypt, ErrNoPathSecret) || errors.Is(ErrNoPathSecret, errPathDecrypt) {
		t.Fatal("errPathDecrypt and ErrNoPathSecret answer for each other, so a member off the path reports a decryption failure")
	}
}

// TestDecryptUpdatePathRejectsAnAnnouncedKeyMismatch is ValSem204, and it is checked at every node
// of the path rather than at the entry point alone.
//
// The tamper is over each node from the entry point up in turn. A version that compared only the
// key at its own entry point, or only the key at the root, passes a fixture that tampers with the
// other one -- and what it accepts is a private state whose public halves the group does not
// agree with, which has no symptom until the NEXT commit fails to decrypt.
func TestDecryptUpdatePathRejectsAnAnnouncedKeyMismatch(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, members := newTestTree(t, crypto, 4)
	_, path, _, groupContext := createAndEncryptPath(t, crypto, tree, members[0], nil)
	receiverTree := tree.Clone()
	if err := receiverTree.MergeUpdatePath(crypto, members[0].LeafIndex, path); err != nil {
		t.Fatalf("MergeUpdatePath: %v", err)
	}
	priv := NewTreeKEMPrivate(members[1].LeafIndex, members[1].EncryptionPriv)
	start, _, _, found := updatePathEntryFor(t, crypto, receiverTree, members[0].LeafIndex, priv, nil)
	if !found {
		t.Fatal("no entry point for member 1")
	}
	swapped := 0
	for at := start; at < len(path.Nodes); at += 1 {
		// an unrelated key that the ciphertexts still open under, so only the derived-key
		// comparison can catch it.
		_, unrelated, err := crypto.DeriveKeyPair(crypto.Random(crypto.HashSize()))
		if err != nil {
			t.Fatalf("DeriveKeyPair: %v", err)
		}
		tampered := &UpdatePath{LeafNode: path.LeafNode,
			Nodes: append([]UpdatePathNode{}, path.Nodes...)}
		tampered.Nodes[at].EncryptionKey = unrelated
		fresh := NewTreeKEMPrivate(members[1].LeafIndex, members[1].EncryptionPriv)
		if _, err := receiverTree.DecryptUpdatePath(crypto, members[0].LeafIndex, tampered,
			groupContext, fresh, nil); !errors.Is(err, errPathKeyMismatch) {
			t.Fatalf("node %d of the path announcing an unrelated key gave err = %v, want errPathKeyMismatch",
				at, err)
		}
		swapped += 1
	}
	if swapped < 2 {
		t.Fatalf("only %d node of the path was swept, so a check at one index would pass this", swapped)
	}
}

// TestDecryptUpdatePathRefusesACiphertextCountThatDisagreesWithTheResolution is section 7.6's
// count, at every node of the path rather than at the receiver's own.
//
// The tamper is deliberately at a node the receiver does NOT decrypt from. A decrypt that counted
// its own entry point and then walked the rest of the path on trust accepts this exactly, and
// what it accepts is a path whose positional pairing means nothing for every other member of the
// group -- none of whom is running this code.
func TestDecryptUpdatePathRefusesACiphertextCountThatDisagreesWithTheResolution(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, members := newTestTree(t, crypto, 4)
	_, path, _, groupContext := createAndEncryptPath(t, crypto, tree, members[0], nil)
	receiverTree := tree.Clone()
	if err := receiverTree.MergeUpdatePath(crypto, members[0].LeafIndex, path); err != nil {
		t.Fatalf("MergeUpdatePath: %v", err)
	}
	priv := NewTreeKEMPrivate(members[1].LeafIndex, members[1].EncryptionPriv)
	start, _, _, found := updatePathEntryFor(t, crypto, receiverTree, members[0].LeafIndex, priv, nil)
	if !found {
		t.Fatal("no entry point for member 1")
	}
	// the fixture's premise: there is a node of this path the receiver does not decrypt from, and
	// it is the one being tampered with.
	away := -1
	for at := range path.Nodes {
		if at != start {
			away = at
		}
	}
	if away < 0 {
		t.Fatal("every node of this path is the receiver's own entry point, so this fixture cannot say what it is for")
	}
	// the control first: untouched, this path decrypts.
	if _, err := receiverTree.DecryptUpdatePath(crypto, members[0].LeafIndex, path,
		groupContext, NewTreeKEMPrivate(members[1].LeafIndex, members[1].EncryptionPriv), nil); err != nil {
		t.Fatalf("the untampered control did not decrypt: %v", err)
	}
	for _, one := range []struct {
		what  string
		nodes func() []UpdatePathNode
	}{
		{what: "one ciphertext too many", nodes: func() []UpdatePathNode {
			nodes := append([]UpdatePathNode{}, path.Nodes...)
			nodes[away].EncryptedPathSecret = append(
				append([]HpkeCiphertext{}, nodes[away].EncryptedPathSecret...),
				nodes[away].EncryptedPathSecret[0])
			return nodes
		}},
		{what: "one ciphertext too few", nodes: func() []UpdatePathNode {
			nodes := append([]UpdatePathNode{}, path.Nodes...)
			trimmed := append([]HpkeCiphertext{}, nodes[away].EncryptedPathSecret...)
			nodes[away].EncryptedPathSecret = trimmed[:len(trimmed)-1]
			return nodes
		}},
	} {
		tampered := &UpdatePath{LeafNode: path.LeafNode, Nodes: one.nodes()}
		fresh := NewTreeKEMPrivate(members[1].LeafIndex, members[1].EncryptionPriv)
		if _, err := receiverTree.DecryptUpdatePath(crypto, members[0].LeafIndex, tampered,
			groupContext, fresh, nil); !errors.Is(err, errPathLength) {
			t.Fatalf("node %d of the path carrying %s gave err = %v, want errPathLength",
				away, one.what, err)
		}
	}
}

// TestDecryptUpdatePathRefusesWhenNoCiphertextIsAddressedToUs is the ordinary condition rather
// than a fault: a member this commit ADDS receives the path secret in its Welcome and nothing
// here, so the sentinel has to be the one that says so.
func TestDecryptUpdatePathRefusesWhenNoCiphertextIsAddressedToUs(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, members := newTestTree(t, crypto, 4)
	// leaf 3 is treated as added by this commit, so nothing is encrypted to it.
	_, path, _, groupContext := createAndEncryptPath(t, crypto, tree, members[0], []LeafIndex{3})
	receiverTree := tree.Clone()
	if err := receiverTree.MergeUpdatePath(crypto, members[0].LeafIndex, path); err != nil {
		t.Fatalf("MergeUpdatePath: %v", err)
	}
	priv := NewTreeKEMPrivate(members[3].LeafIndex, members[3].EncryptionPriv)
	if _, err := receiverTree.DecryptUpdatePath(crypto, members[0].LeafIndex, path,
		groupContext, priv, []LeafIndex{3}); !errors.Is(err, ErrNoPathSecret) {
		t.Fatalf("err = %v, want ErrNoPathSecret", err)
	}
	// the sender itself is the other member of this class: its own common ancestor with itself is
	// its leaf, which is on no filtered direct path.
	own := NewTreeKEMPrivate(members[0].LeafIndex, members[0].EncryptionPriv)
	if _, err := receiverTree.DecryptUpdatePath(crypto, members[0].LeafIndex, path,
		groupContext, own, []LeafIndex{3}); !errors.Is(err, ErrNoPathSecret) {
		t.Fatalf("the sender decrypting its own path gave err = %v, want ErrNoPathSecret", err)
	}
	// the control: with no exclusion the same member decrypts, so the refusal above is about the
	// exclusion and not about a member that could never have decrypted.
	_, path2, _, context2 := createAndEncryptPath(t, crypto, tree, members[0], nil)
	control := tree.Clone()
	if err := control.MergeUpdatePath(crypto, members[0].LeafIndex, path2); err != nil {
		t.Fatalf("control MergeUpdatePath: %v", err)
	}
	if _, err := control.DecryptUpdatePath(crypto, members[0].LeafIndex, path2, context2,
		NewTreeKEMPrivate(members[3].LeafIndex, members[3].EncryptionPriv), nil); err != nil {
		t.Fatalf("the same member with no exclusion did not decrypt: %v", err)
	}
}

// TestDecryptUpdatePathUsesAHeldPathSecretWhenItHasOne is the case where the entry point is not
// the receiver's own leaf, and it is the one shape a single-commit fixture cannot reach.
//
// After member 4 commits, members 5, 6 and 7 hold path secrets for nodes ABOVE them, and those
// nodes are now non-blank -- so a resolution stops at them instead of walking down to the leaves.
// The next commit therefore seals to the node rather than to the leaf, and a decrypt that looked
// only for its own leaf in the resolution finds nothing and reports a member that cannot decrypt
// a commit it is perfectly able to decrypt.
func TestDecryptUpdatePathUsesAHeldPathSecretWhenItHasOne(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, members := newTestTree(t, crypto, 8)
	// member 4 commits first, so members 5, 6 and 7 hold path secrets above them.
	_, firstPath, _, firstContext := createAndEncryptPath(t, crypto, tree, members[4], nil)
	receiverTree := tree.Clone()
	if err := receiverTree.MergeUpdatePath(crypto, members[4].LeafIndex, firstPath); err != nil {
		t.Fatalf("MergeUpdatePath: %v", err)
	}
	priv := NewTreeKEMPrivate(members[5].LeafIndex, members[5].EncryptionPriv)
	first, err := receiverTree.DecryptUpdatePath(crypto, members[4].LeafIndex, firstPath,
		firstContext, priv, nil)
	if err != nil {
		t.Fatalf("first DecryptUpdatePath: %v", err)
	}
	if len(first.Private.PathSecrets) == 0 {
		t.Fatalf("the receiver learned no path secrets")
	}
	// now member 0 commits over the tree member 4's commit left behind -- a tree neither of the
	// two ends of this decrypt built alone.
	_, secondPath, secondPlan, secondContext := createAndEncryptPath(t, crypto, receiverTree, members[0], nil)
	if err := receiverTree.MergeUpdatePath(crypto, members[0].LeafIndex, secondPath); err != nil {
		t.Fatalf("second MergeUpdatePath: %v", err)
	}
	// the fixture's premise, asserted rather than assumed: the entry this member decrypts at is a
	// PARENT node it holds a secret for and not its own leaf. Without this the test would still
	// pass over a tree where the resolution walked down to the leaf, and the shape it exists for
	// would never have been exercised.
	_, entry, _, found := updatePathEntryFor(t, crypto, receiverTree, members[0].LeafIndex,
		first.Private, nil)
	if !found {
		t.Fatal("no entry point for member 5 in member 0's commit")
	}
	if entry == members[5].LeafIndex.NodeIndex() {
		t.Fatalf("member 5's entry into the second commit is its own leaf at node %d, so this fixture does not exercise a held path secret",
			entry)
	}
	if _, holds := first.Private.PathSecrets[entry]; !holds {
		t.Fatalf("member 5 does not hold a path secret for node %d, which is where its ciphertext stands", entry)
	}
	// the caller's state recorded WHOLE before the call, because what says a decrypt did not
	// write through is the values and the key set together. See treeKEMPrivateSnapshot.
	beforeTheDecrypt := treeKEMPrivateSnapshot(first.Private)
	second, err := receiverTree.DecryptUpdatePath(crypto, members[0].LeafIndex, secondPath,
		secondContext, first.Private, nil)
	if err != nil {
		t.Fatalf("second DecryptUpdatePath: %v", err)
	}
	if !bytes.Equal(second.CommitSecret, secondPlan.CommitSecret) {
		t.Fatalf("commit secret differs from the sender's")
	}
	if err := second.Private.Consistent(crypto, receiverTree); err != nil {
		t.Fatalf("the state after the second commit does not agree with the tree: %v", err)
	}
	// the state handed in is not written through: the caller may still throw this commit away,
	// and the epoch it is still running on is the one that state describes.
	//
	// Every field is compared, in both directions, and that is the correction rather than a
	// flourish. What stood here read only that the key at the entry point was still IN the map --
	// which the decrypt satisfies whether it clones the caller's state or edits it in place,
	// because it assigns into PathSecrets and never deletes from it. The difference between the
	// two implementations is the VALUE under that key, which the aliasing version has already
	// replaced with the next epoch's rung, and the keys the map has GAINED for every node above
	// the entry point. Measured, not supposed: with `out := priv.Clone()` written as `out := priv`
	// the whole of mls and message stayed green against the assertion this replaces.
	if changed := treeKEMPrivateDifference(beforeTheDecrypt, treeKEMPrivateSnapshot(first.Private)); changed != "" {
		t.Fatalf("the decrypt mutated the private state it was handed: %s", changed)
	}
	// and the answer is a different state rather than the same one handed back, which is the same
	// defect read from the other end: a decrypt returning its argument reports no difference above
	// only because there is nothing left to compare against.
	if second.Private == first.Private {
		t.Fatalf("the decrypt answered the very state it was handed rather than a new one")
	}
	// the rungs really did land somewhere, so the comparison above ran over a decrypt that had
	// something to write rather than over one that wrote nothing at all
	if _, wrote := second.Private.PathSecrets[entry]; !wrote {
		t.Fatalf("the answered state holds no path secret at node %d, so nothing above observed a decrypt that wrote",
			entry)
	}
}

// treeKEMPrivateSnapshot is the WHOLE of a private state, rendered so two of them compare as
// values rather than as pointers.
//
// It exists because the assertion it replaced could not fail. "The decrypt did not write through
// the state it was handed" was written as "the key at the entry point is still in the map", and
// DecryptUpdatePath assigns into PathSecrets and never deletes from it -- so that key survives
// both the implementation that clones the caller's state and the implementation that edits it in
// place, and the whole of the difference between them is in the VALUE under the key and in the
// keys the map GAINED. Both are here, along with the leaf index and the leaf private key, because
// a state is all three and a comparison over one field is the same shape of claim as the one it
// replaces.
func treeKEMPrivateSnapshot(priv *TreeKEMPrivate) []string {
	out := []string{
		fmt.Sprintf("leaf %d", priv.LeafIndex),
		fmt.Sprintf("encryption_priv %x", priv.EncryptionPriv),
	}
	for _, x := range slices.Sorted(maps.Keys(priv.PathSecrets)) {
		out = append(out, fmt.Sprintf("path_secret %d %x", x, priv.PathSecrets[x]))
	}
	return out
}

// The first way two snapshots differ, named, or the empty string when they do not differ at all.
//
// The length is compared before the entries so that a map which GAINED a rung is reported as the
// gain it is rather than as whatever line happens to have shifted underneath it.
func treeKEMPrivateDifference(before []string, after []string) string {
	if len(before) != len(after) {
		return fmt.Sprintf("it held %d fields before the call and holds %d after:\n before %v\n after  %v",
			len(before), len(after), before, after)
	}
	for i := range before {
		if before[i] != after[i] {
			return fmt.Sprintf("%q became %q", before[i], after[i])
		}
	}
	return ""
}

// everyByteIsZero reads a slice as erased, and answers no for an empty one on purpose: a loop
// over a zero length slice finds no non-zero byte and so reports "erased" for a key that was
// never there, which is the reading that would let an assertion pass over a recorder that
// captured nothing.
func everyByteIsZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return len(b) != 0
}

// keyRecordingCryptoProvider keeps the SLICE HEADER of every private key that crossed the
// boundary between it and the code under test: the private half of every key pair it derived, and
// every private key it was handed to open with.
//
// It is how an erasure of a LOCAL becomes observable from outside the body that made it.
// zeroizeSecret writes into the backing array, and secret_zeroize.go's header says that is
// precisely the part go lets a test see -- every other live slice over the same array observes
// the zeros. So a recorder holding a second slice over the array a decrypt derived reads zeros
// when the decrypt erased it and reads the key when the decrypt merely dropped the reference.
//
// The class it records is DERIVED and never listed: it is every private key that passes through
// the provider interface during one call, whatever the body did to reach it. A rung added to the
// ladder, a second resolution entry tried, or an entry point that turns out to be the member's own
// leaf rather than a node above it are all recorded without this type being touched, which is the
// difference between this and a test naming the two calls that happen to be there today.
type keyRecordingCryptoProvider struct {
	CryptoProvider
	derived [][]byte
	opened  [][]byte
}

var _ CryptoProvider = (*keyRecordingCryptoProvider)(nil)

func (self *keyRecordingCryptoProvider) DeriveKeyPair(ikm []byte) (HpkePrivateKey, HpkePublicKey, error) {
	priv, pub, err := self.CryptoProvider.DeriveKeyPair(ikm)
	if err == nil {
		self.derived = append(self.derived, priv)
	}
	return priv, pub, err
}

// Recorded before the call rather than after it, and recorded whatever the open answers: the
// contract DecryptUpdatePath's own comment states is that the key is "erased whether or not the
// open succeeded", so the arm that must not escape this recorder is the failing one.
func (self *keyRecordingCryptoProvider) HpkeOpen(priv HpkePrivateKey, kemOutput []byte, info []byte,
	aad []byte, ciphertext []byte) ([]byte, error) {
	self.opened = append(self.opened, priv)
	return self.CryptoProvider.HpkeOpen(priv, kemOutput, info, aad, ciphertext)
}

// TestDecryptUpdatePathErasesEveryPrivateKeyThatCrossedTheProviderBoundary is the erasure
// DecryptUpdatePath's header states twice, observed instead of read.
//
// Both statements were prose and nothing held either of them: zeroizeSecret(nodePriv) and
// zeroizeSecret(derivedPriv) were each replaced by a discard and the whole of mls and message
// stayed green. What the erasure is worth is the paragraph the method header gives -- the
// comparison at every rung needs the PUBLIC half, DeriveNodeKeyPair answers both, and the private
// halves of every node between the entry point and the root are otherwise left on the heap of a
// member that has no further use for them -- and a contract nothing observes is a comment.
//
// Both arms of the entry point are driven, because they reach the erasure by different routes and
// a fixture with one of them holds only half of it. When the entry point is the member's own leaf
// the key is a COPY of the leaf key it holds, made by NodePrivateKey precisely so this call can
// erase it, and it reaches the provider only as an argument to the open; when the entry point is a
// node above the member the key is DERIVED through the provider and never handed back to it. So
// the recorder reads both directions of the boundary, and a run whose fixture reached only one of
// them is a failure here rather than a pass over half the property.
func TestDecryptUpdatePathErasesEveryPrivateKeyThatCrossedTheProviderBoundary(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	erased := func(what string, tree *RatchetTree, sender LeafIndex, path *UpdatePath,
		groupContext []byte, priv *TreeKEMPrivate, ownLeaf bool) {
		entryStep, entry, _, found := updatePathEntryFor(t, crypto, tree, sender, priv, nil)
		if !found {
			t.Fatalf("%s: the receiver has no entry point in this path, so the fixture decrypts nothing", what)
		}
		// the fixture's premise, asserted rather than assumed: which arm of NodePrivateKey this
		// run reaches is the whole reason there are two of them here.
		if isOwn := entry == priv.LeafIndex.NodeIndex(); isOwn != ownLeaf {
			t.Fatalf("%s: the entry point is node %d at step %d and this fixture is meant to enter at %s",
				what, entry, entryStep, map[bool]string{true: "the member's own leaf", false: "a node above it"}[ownLeaf])
		}
		recorder := &keyRecordingCryptoProvider{CryptoProvider: crypto}
		if _, err := tree.DecryptUpdatePath(recorder, sender, path, groupContext, priv, nil); err != nil {
			t.Fatalf("%s: DecryptUpdatePath: %v", what, err)
		}
		if len(recorder.derived) == 0 {
			t.Fatalf("%s: the decrypt derived no key pair through its provider, so the ladder half of this test observed nothing", what)
		}
		if len(recorder.opened) == 0 {
			t.Fatalf("%s: the decrypt opened nothing through its provider, so the entry point half of this test observed nothing", what)
		}
		for i, key := range recorder.derived {
			if !everyByteIsZero(key) {
				t.Errorf("%s: the private half of key pair %d of the %d this decrypt derived reads %x when it returned, so it was dropped rather than erased",
					what, i, len(recorder.derived), key)
			}
		}
		for i, key := range recorder.opened {
			if !everyByteIsZero(key) {
				t.Errorf("%s: private key %d of the %d this decrypt opened with reads %x when it returned, so it was dropped rather than erased",
					what, i, len(recorder.opened), key)
			}
		}
	}

	// the entry point that is the member's own leaf: member 1 stands in the resolution of the
	// copath child of member 0's lowest path node, as itself.
	tree, members := newTestTree(t, crypto, 4)
	_, path, _, groupContext := createAndEncryptPath(t, crypto, tree, members[0], nil)
	receiverTree := tree.Clone()
	if err := receiverTree.MergeUpdatePath(crypto, members[0].LeafIndex, path); err != nil {
		t.Fatalf("MergeUpdatePath: %v", err)
	}
	erased("the entry point is the member's own leaf", receiverTree, members[0].LeafIndex, path,
		groupContext, NewTreeKEMPrivate(members[1].LeafIndex, members[1].EncryptionPriv), true)

	// and the entry point that is a node above the member, which takes two commits to reach for
	// TestDecryptUpdatePathUsesAHeldPathSecretWhenItHasOne's reason: a member holds a secret for a
	// node above it only after some earlier commit sealed one to it.
	wide, wideMembers := newTestTree(t, crypto, 8)
	_, firstPath, _, firstContext := createAndEncryptPath(t, crypto, wide, wideMembers[4], nil)
	wideReceiver := wide.Clone()
	if err := wideReceiver.MergeUpdatePath(crypto, wideMembers[4].LeafIndex, firstPath); err != nil {
		t.Fatalf("first MergeUpdatePath: %v", err)
	}
	first, err := wideReceiver.DecryptUpdatePath(crypto, wideMembers[4].LeafIndex, firstPath,
		firstContext, NewTreeKEMPrivate(wideMembers[5].LeafIndex, wideMembers[5].EncryptionPriv), nil)
	if err != nil {
		t.Fatalf("first DecryptUpdatePath: %v", err)
	}
	_, secondPath, _, secondContext := createAndEncryptPath(t, crypto, wideReceiver, wideMembers[0], nil)
	if err := wideReceiver.MergeUpdatePath(crypto, wideMembers[0].LeafIndex, secondPath); err != nil {
		t.Fatalf("second MergeUpdatePath: %v", err)
	}
	erased("the entry point is a node the member holds a secret for", wideReceiver,
		wideMembers[0].LeafIndex, secondPath, secondContext, first.Private, false)
}

// TestMergeUpdatePathJudgesTheMergedTreeAndNotTheOneItReplaces is the half of section 7.9.2's
// PLACEMENT that the spliced-subtree fixture cannot state.
//
// That fixture refuses, and it refuses under a merge that swept the tree it was about to REPLACE
// just as readily as under one that swept the tree it is about to adopt, because the splice makes
// both of them section 7.9.2 invalid. So it says the sweep happens and says nothing about which
// tree it happens over -- and the placement is the whole of what makes a commit judgeable at all,
// since the tree a commit has to satisfy is the one it produces.
//
// This is the other direction, and it is the direction only a positive case can reach: a receiver
// whose CURRENT tree fails the rule at a node the incoming commit repairs. A merge sweeping the
// old tree refuses a perfectly good commit here and leaves the member unable to follow the group;
// a merge sweeping the merged tree accepts it. The stale node is put on the sender's own direct
// path deliberately, because that is the path the merge blanks and refills, and it is what makes
// the repair a consequence of the commit rather than of anything this test does afterwards.
func TestMergeUpdatePathJudgesTheMergedTreeAndNotTheOneItReplaces(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, members := newTestTree(t, crypto, 4)
	_, unclaimed, err := crypto.DeriveKeyPair(crypto.Random(crypto.HashSize()))
	if err != nil {
		t.Fatalf("DeriveKeyPair: %v", err)
	}
	// node 1 is the parent of leaves 0 and 1 and is on the sender's direct path. Nothing in this
	// tree is parent-hash valid with respect to it, so section 7.9.2's exactly-one requirement
	// refuses the tree as it stands.
	stale := tree.Clone()
	if err := stale.SetParent(NodeIndex(1), &ParentNode{EncryptionKey: unclaimed}); err != nil {
		t.Fatalf("SetParent(1): %v", err)
	}
	// the fixture's premise, asserted rather than assumed. Without it a later change that made
	// this tree valid would leave the test passing while comparing two trees that both verify,
	// which is exactly the shape it is written to avoid.
	if err := stale.VerifyParentHashes(crypto); err == nil {
		t.Fatal("the receiver's tree satisfies section 7.9.2, so the merge below is not being asked to tell two trees apart")
	}
	_, path, _, _ := createAndEncryptPath(t, crypto, stale, members[0], nil)
	merged := stale.Clone()
	if err := merged.MergeUpdatePath(crypto, members[0].LeafIndex, path); err != nil {
		t.Fatalf("the merge refused a commit that repairs the very node the receiver's tree fails at, so it is judging the tree it was about to replace: %v", err)
	}
	// and the tree it adopted really is the repaired one, so the acceptance above is not a sweep
	// that was skipped
	if err := merged.VerifyParentHashes(crypto); err != nil {
		t.Fatalf("the tree the merge adopted does not satisfy section 7.9.2: %v", err)
	}
}

// TestDecryptUpdatePathCarriesForwardASecretForANodeTheMergeBlanked is the boundary
// DecryptUpdatePath's header states, held here so a later pass that decides to prune has to come
// and say so.
//
// The decrypt copies the caller's whole state and writes the new rungs into the copy. It prunes
// nothing, and it cannot: it knows the merged tree and the caller's secrets and has no way to tell
// a secret that is stale from one for a node it simply did not walk. So a secret for a node this
// commit BLANKED -- a node on the sender's direct path that the filtered path drops, because its
// copath child resolves to nothing -- travels into the answered state, where
// TreeKEMPrivate.Consistent refuses the whole state for it.
//
// The refusal is the right one and the input was already stale, which is why this is a boundary
// and not a defect: the state handed in agreed with the tree it was built against, and what made
// it stale is the commit, an epoch after the removal that emptied the subtree. What this pins is
// that the condition arrives as ErrPathSecretMismatch out of Consistent rather than as a silently
// wrong state or as a refusal from the decrypt, so the layer above chooses knowing which it is.
func TestDecryptUpdatePathCarriesForwardASecretForANodeTheMergeBlanked(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, members := newTestTree(t, crypto, 8)
	// leaves 2 and 3 leave the group, which empties the resolution of node 5 -- the copath child
	// of node 3 for a sender at leaf 0. Node 3 is on that sender's DIRECT path and is dropped from
	// its FILTERED path, which is the one shape that gets a node blanked and not refilled.
	stale := tree.Clone()
	for _, gone := range []LeafIndex{2, 3} {
		if err := stale.Blank(gone.NodeIndex()); err != nil {
			t.Fatalf("Blank(leaf %d): %v", gone, err)
		}
	}
	// the secret the receiver holds for node 3, and the key node 3 carries because of it. Built
	// this way round so the state below agrees with the tree it is about to be handed: what makes
	// it stale is the commit and not the fixture.
	staleSecret := crypto.Random(crypto.HashSize())
	_, stalePub, err := DeriveNodeKeyPair(crypto, staleSecret)
	if err != nil {
		t.Fatalf("DeriveNodeKeyPair: %v", err)
	}
	if err := stale.SetParent(NodeIndex(3), &ParentNode{EncryptionKey: stalePub}); err != nil {
		t.Fatalf("SetParent(3): %v", err)
	}
	priv := NewTreeKEMPrivate(members[1].LeafIndex, members[1].EncryptionPriv)
	priv.PathSecrets[NodeIndex(3)] = cloneBytes(staleSecret)
	if err := priv.Consistent(crypto, stale); err != nil {
		t.Fatalf("the state does not agree with the tree it was built against, so this fixture is stale before the commit rather than because of it: %v", err)
	}

	_, path, _, groupContext := createAndEncryptPath(t, crypto, stale, members[0], nil)
	receiverTree := stale.Clone()
	if err := receiverTree.MergeUpdatePath(crypto, members[0].LeafIndex, path); err != nil {
		t.Fatalf("MergeUpdatePath: %v", err)
	}
	// the premise: the commit really did blank node 3 and really did not refill it
	if receiverTree.ParentAt(NodeIndex(3)) != nil {
		t.Fatal("node 3 is occupied after the commit, so this fixture no longer reaches the shape it is named for")
	}
	got, err := receiverTree.DecryptUpdatePath(crypto, members[0].LeafIndex, path, groupContext,
		priv, nil)
	if err != nil {
		t.Fatalf("DecryptUpdatePath refused a commit that is well formed: %v", err)
	}
	// carried forward, VALUE and all, rather than dropped
	carried, held := got.Private.PathSecrets[NodeIndex(3)]
	if !held {
		t.Fatal("the decrypt pruned the secret for the blanked node; that is a defensible choice and it is not the one this test was written against, so move the boundary paragraph on DecryptUpdatePath in the same commit")
	}
	if !bytes.Equal(carried, staleSecret) {
		t.Fatalf("the secret carried forward for node 3 is %x and the state held %x", carried, staleSecret)
	}
	// and the state the caller is handed is refused against the tree it belongs to, by the
	// sentinel that names the condition
	if err := got.Private.Consistent(crypto, receiverTree); !errors.Is(err, ErrPathSecretMismatch) {
		t.Fatalf("the answered state gave Consistent err = %v, want ErrPathSecretMismatch", err)
	}
}

// TestDecryptUpdatePathRefusesARecoveredSecretThatIsNotAKdfWidth is errPathSecretLength, over
// every width around the one the provider answers rather than over one wrong number.
//
// A sender is free to seal a plaintext of any length, and nothing downstream of the open objects:
// DeriveNodeKeyPair derives a key pair from a secret of any width, and the ValSem204 comparison at
// every rung is against a key that same sender announced, so a sender-controlled pair agrees with
// itself all the way to the root. The two ends then hold different commit secrets, and what
// separates them is the key schedule's confirmation tag, an epoch later and two layers up.
//
// The control at the end is what says the refusal is about the WIDTH: a secret of the right width
// that is simply the wrong secret is refused too, and refused as errPathKeyMismatch. Without it a
// check that had degenerated into "any resealed ciphertext is refused" would pass this.
func TestDecryptUpdatePathRefusesARecoveredSecretThatIsNotAKdfWidth(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, members := newTestTree(t, crypto, 4)
	_, path, _, groupContext := createAndEncryptPath(t, crypto, tree, members[0], nil)
	receiverTree := tree.Clone()
	if err := receiverTree.MergeUpdatePath(crypto, members[0].LeafIndex, path); err != nil {
		t.Fatalf("MergeUpdatePath: %v", err)
	}
	probe := NewTreeKEMPrivate(members[1].LeafIndex, members[1].EncryptionPriv)
	step, entry, position, found := updatePathEntryFor(t, crypto, receiverTree, members[0].LeafIndex,
		probe, nil)
	if !found {
		t.Fatal("no entry point for member 1")
	}
	// the key this member's own ciphertext is sealed to, read off the tree rather than assumed, so
	// every reseal below is one this member really does open
	entryNode := receiverTree.Get(entry)
	if entryNode == nil || entryNode.Leaf == nil {
		t.Fatalf("the entry point at node %d is not an occupied leaf, so the reseal below cannot address it", entry)
	}
	entryPub := entryNode.Leaf.EncryptionKey

	reseal := func(plaintext []byte) *UpdatePath {
		sealed, err := SealWithLabel(crypto, entryPub, updatePathNodeLabel, groupContext, plaintext)
		if err != nil {
			t.Fatalf("SealWithLabel: %v", err)
		}
		tampered := &UpdatePath{LeafNode: path.LeafNode,
			Nodes: append([]UpdatePathNode{}, path.Nodes...)}
		tampered.Nodes[step].EncryptedPathSecret = append([]HpkeCiphertext{},
			tampered.Nodes[step].EncryptedPathSecret...)
		tampered.Nodes[step].EncryptedPathSecret[position] = *sealed
		return tampered
	}

	nh := crypto.HashSize()
	widths := 0
	for _, width := range []int{0, 1, nh - 1, nh + 1, 2 * nh} {
		fresh := NewTreeKEMPrivate(members[1].LeafIndex, members[1].EncryptionPriv)
		if _, err := receiverTree.DecryptUpdatePath(crypto, members[0].LeafIndex,
			reseal(bytes.Repeat([]byte{0x7e}, width)), groupContext, fresh,
			nil); !errors.Is(err, errPathSecretLength) {
			t.Errorf("a path secret of %d octets under a provider whose KDF.Nh is %d gave err = %v, want errPathSecretLength",
				width, nh, err)
		}
		widths += 1
	}
	if widths < 2 {
		t.Fatalf("only %d width was swept, so a check written against one number would pass this", widths)
	}
	// the control: the same reseal at the width the provider answers is not this refusal. It is
	// still refused -- the rung does not derive the announced key -- and by the other sentinel,
	// which is what says this check reads a length rather than reading "resealed".
	fresh := NewTreeKEMPrivate(members[1].LeafIndex, members[1].EncryptionPriv)
	if _, err := receiverTree.DecryptUpdatePath(crypto, members[0].LeafIndex,
		reseal(bytes.Repeat([]byte{0x7e}, nh)), groupContext, fresh,
		nil); !errors.Is(err, errPathKeyMismatch) {
		t.Fatalf("a wrong path secret of the right width gave err = %v, want errPathKeyMismatch", err)
	}
}

// TestEveryPublishedUpdatePathDecryptsToItsPublishedSecrets is the only oracle for task 22 that is
// independent of this repository.
//
// The corpus publishes, for each path, the path secret every leaf that can decrypt should recover
// and the commit secret the epoch should reach. Both are values another implementation's ladder
// produced, so this separates the whole family of self-consistent errors from a correct
// implementation: a node_secret step skipped or applied twice, a DeriveSecret label spelled
// differently, an Extract with its arguments transposed, a resolution walked in the wrong order,
// or a ciphertext paired with the wrong entry all agree with a sender in this package and
// disagree with the working group's octets.
//
// It is also a receiver that is NOT the generator, over a tree neither end of it constructed:
// the tree, the private state, the path and the answer all come out of the file.
func TestEveryPublishedUpdatePathDecryptsToItsPublishedSecrets(t *testing.T) {
	paths, decrypts, deep, refusals := 0, 0, 0, 0
	forEachPublishedReceivedPath(t, func(at int, vector *treekemReceiverVector, crypto CryptoProvider,
		base *RatchetTree, published treekemReceivedUpdatePath, path *UpdatePath) {
		paths += 1
		merged := base.Clone()
		if err := merged.MergeUpdatePath(crypto, LeafIndex(published.Sender), path); err != nil {
			t.Fatalf("entry %d, sender %d: MergeUpdatePath: %v", at, published.Sender, err)
		}
		treeHash, err := merged.TreeHash(crypto)
		if err != nil {
			t.Fatalf("entry %d, sender %d: TreeHash: %v", at, published.Sender, err)
		}
		groupContext := vector.groupContext(t, treeHash)
		wantCommitSecret := MustHex(t, published.CommitSecret)
		for leafIndex, wantSecret := range published.PathSecrets {
			if wantSecret == nil {
				continue
			}
			priv, ok := vector.private(t, uint32(leafIndex))
			if !ok {
				t.Fatalf("entry %d, sender %d: the file publishes a path secret for leaf %d and no private state for it",
					at, published.Sender, leafIndex)
			}
			_, entry, _, found := updatePathEntryFor(t, crypto, merged,
				LeafIndex(published.Sender), priv, nil)
			if !found {
				t.Fatalf("entry %d, sender %d, leaf %d: the file says this leaf decrypts and the tree math finds it no entry point",
					at, published.Sender, leafIndex)
			}
			if entry != LeafIndex(leafIndex).NodeIndex() {
				deep += 1
			}
			got, err := merged.DecryptUpdatePath(crypto, LeafIndex(published.Sender), path,
				groupContext, priv, nil)
			if err != nil {
				t.Fatalf("entry %d, sender %d, leaf %d: DecryptUpdatePath: %v",
					at, published.Sender, leafIndex, err)
			}
			lowest := CommonAncestor(LeafIndex(published.Sender).NodeIndex(),
				LeafIndex(leafIndex).NodeIndex())
			if recovered := got.Private.PathSecrets[lowest]; !bytes.Equal(recovered, MustHex(t, *wantSecret)) {
				t.Fatalf("entry %d, sender %d, leaf %d: the path secret recovered at node %d is %s and the corpus publishes %s",
					at, published.Sender, leafIndex, lowest, HexOf(recovered), *wantSecret)
			}
			if !bytes.Equal(got.CommitSecret, wantCommitSecret) {
				t.Fatalf("entry %d, sender %d, leaf %d: the commit secret is %s and the corpus publishes %s",
					at, published.Sender, leafIndex, HexOf(got.CommitSecret), published.CommitSecret)
			}
			// the private state this leaves behind has to agree with the merged tree, which is
			// the ValSem204 comparison stated over another implementation's announced keys.
			if err := got.Private.Consistent(crypto, merged); err != nil {
				t.Fatalf("entry %d, sender %d, leaf %d: the state after the decrypt does not agree with the merged tree: %v",
					at, published.Sender, leafIndex, err)
			}
			// the control, per case rather than once: the same path under a context that is one
			// octet different must not open. Every published case succeeds, so a decrypt that
			// checked everything and one that returned the first plaintext it found produce
			// identical runs, and only an input that must be refused separates them.
			wrongContext := cloneBytes(groupContext)
			wrongContext[0] ^= 0x01
			fresh, _ := vector.private(t, uint32(leafIndex))
			if _, err := merged.DecryptUpdatePath(crypto, LeafIndex(published.Sender), path,
				wrongContext, fresh, nil); !errors.Is(err, errPathDecrypt) {
				t.Fatalf("entry %d, sender %d, leaf %d: one octet of the group context flipped gave err = %v, want errPathDecrypt",
					at, published.Sender, leafIndex, err)
			}
			refusals += 1
			decrypts += 1
		}
	})
	if paths != treekemReceiverPathCount || decrypts != treekemReceiverDecryptCount {
		t.Fatalf("the run decrypted %d published paths at %d leaves; want %d and %d",
			paths, decrypts, treekemReceiverPathCount, treekemReceiverDecryptCount)
	}
	if refusals != treekemReceiverDecryptCount {
		t.Fatalf("%d of %d cases were separated by the wrong-context control", refusals, decrypts)
	}
	if deep == 0 {
		t.Fatal("no published case entered the path above the receiver's own leaf, so the held-path-secret arm never ran over the corpus")
	}
	t.Logf("%d published paths, %d published (leaf, path secret, commit secret) triples reproduced, %d of them entering above the receiver's own leaf",
		paths, decrypts, deep)
}

// parentHashEqualityAloneAccepts is section 7.9.2 stated the way a one-condition reading of it
// states it: every non-blank parent has SOME descendant whose parent_hash field is the parent hash
// of that node taken over the other child.
//
// It is the whole of the second condition and none of the rest -- no "D is in the resolution of C
// with only P's unmerged leaves beside it", no "exactly one". It exists so the test below can say
// what a merge that re-derived the rule from that reading would accept, rather than leaving the
// difference as a sentence in a comment. Nothing in this package calls it and nothing should: it
// is a MODEL of the weaker rule, kept next to the fixture that separates it from the real one.
func parentHashEqualityAloneAccepts(t *testing.T, crypto CryptoProvider, tree *RatchetTree) bool {
	t.Helper()
	for x := uint32(1); x < tree.NodeWidth(); x += 2 {
		p := NodeIndex(x)
		if tree.ParentAt(p) == nil {
			continue
		}
		left, leftOk := leftOf(p)
		right, rightOk := rightOf(p)
		if !leftOk || !rightOk {
			t.Fatalf("node %d is an odd index with no children", p)
		}
		claimed := false
		for _, arm := range [2][2]NodeIndex{{left, right}, {right, left}} {
			hash, err := tree.ParentHash(crypto, p, arm[1])
			if err != nil {
				t.Fatalf("ParentHash(%d, %d): %v", p, arm[1], err)
			}
			for _, node := range tree.Resolution(arm[0]) {
				field, carries := nodeParentHashField(tree.Get(node))
				if carries && bytes.Equal(field, hash) {
					claimed = true
				}
			}
		}
		if !claimed {
			return false
		}
	}
	return true
}

// TestMergeUpdatePathHoldsSection792sThirdConditionAndNotOnlyItsParentHashEquality is the fixture
// that separates the rule the merge CALLS from the rule a one-condition reading of section 7.9.2
// would have it restate.
//
// The tree is one add that was never recorded. Leaf 0 committed node 3 at an epoch when leaf 1's
// slot was empty, and leaf 1 was then put into the tree WITHOUT being added to node 3's
// unmerged_leaves. Everything a parent_hash equality can see is still right: leaf 0's signed field
// is exactly the parent hash of node 3 over node 5, so the chain from leaf 0 to node 3 verifies
// link for link, and parentHashEqualityAloneAccepts above says so out loud for both halves of this
// fixture. What has changed is the RESOLUTION: leaf 1 now stands in the resolution of node 1
// beside leaf 0, so every secret any later commit seals to that resolution reaches a key node 3's
// chain never accounted for.
//
// Section 7.9.2's third condition is the rule that refuses it, and it is the condition the plan's
// text omits. The two halves of the loop are the same tree with and without the add recorded, so
// what separates them is the unmerged_leaves vector and nothing else: with leaf 1 recorded the
// resolution of node 1 with the claimant removed is exactly node 3's unmerged leaves under node 1
// and the merge proceeds, and without it the same commit over the same shape is refused. A merge
// that restated the second condition alone accepts both, which is what the assertion above the
// refusal says.
func TestMergeUpdatePathHoldsSection792sThirdConditionAndNotOnlyItsParentHashEquality(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	for _, recorded := range []bool{true, false} {
		tree, members := newTestTree(t, crypto, 8)
		// the node whose key the forger holds. It sits above leaves 0 to 3 and, being non-blank,
		// IS the resolution of the copath child of the root for every commit from the other half
		// of the tree -- which is what makes this worth refusing rather than merely irregular.
		forgedPriv, forgedPub, err := crypto.DeriveKeyPair(crypto.Random(crypto.HashSize()))
		if err != nil {
			t.Fatalf("DeriveKeyPair: %v", err)
		}
		unmerged := []LeafIndex{}
		if recorded {
			unmerged = []LeafIndex{1}
		}
		if err := tree.SetParent(3, &ParentNode{
			EncryptionKey:  forgedPub,
			ParentHash:     []byte{},
			UnmergedLeaves: unmerged,
		}); err != nil {
			t.Fatalf("SetParent(3): %v", err)
		}
		// leaf 0 is the claimant, and its claim is HONEST: this is the parent hash of node 3 over
		// node 5, which is what a member that had committed node 3 would have signed.
		claim, err := tree.ParentHash(crypto, 3, 5)
		if err != nil {
			t.Fatalf("ParentHash(3, 5): %v", err)
		}
		claimant := tree.Leaf(members[0].LeafIndex).Clone()
		claimant.LeafNodeSource = LeafNodeSourceCommit
		claimant.ParentHash = claim
		if err := claimant.Sign(crypto, members[0].SignaturePriv, testGroupId(), members[0].LeafIndex); err != nil {
			t.Fatalf("Sign: %v", err)
		}
		if err := tree.SetLeaf(members[0].LeafIndex, claimant); err != nil {
			t.Fatalf("SetLeaf: %v", err)
		}
		// the one-condition reading accepts BOTH halves, which is the whole point of the fixture:
		// the difference between them is invisible to it.
		if !parentHashEqualityAloneAccepts(t, crypto, tree) {
			t.Fatalf("recorded=%v: the parent hash equality alone already refuses this tree, so it cannot separate the two rules",
				recorded)
		}

		// an honest commit from the other half of the tree, which is what carries the payoff.
		_, path, plan, groupContext := createAndEncryptPath(t, crypto, tree, members[4], nil)
		steps, err := tree.filteredPathSteps(members[4].LeafIndex)
		if err != nil {
			t.Fatalf("filteredPathSteps: %v", err)
		}
		targets, err := tree.EncryptionTargets(members[4].LeafIndex, nil)
		if err != nil {
			t.Fatalf("EncryptionTargets: %v", err)
		}
		sealed := false
		for i, step := range steps {
			if step.CopathChild != 3 {
				continue
			}
			for j, y := range targets[i] {
				if y != 3 {
					continue
				}
				ct := path.Nodes[i].EncryptedPathSecret[j]
				opened, err := OpenWithLabel(crypto, forgedPriv, updatePathNodeLabel, groupContext, &ct)
				if err == nil && bytes.Equal(opened, plan.PathSecrets[i]) {
					sealed = true
				}
			}
		}
		if !sealed {
			t.Fatalf("recorded=%v: the forged node did not receive the commit's path secret, so this fixture is not the attack the third condition is about",
				recorded)
		}

		receiver := tree.Clone()
		if recorded {
			// the add is recorded, condition 3 holds, and the commit merges.
			if err := receiver.MergeUpdatePath(crypto, members[4].LeafIndex, path); err != nil {
				t.Fatalf("the tree whose add IS recorded was refused: %v", err)
			}
			continue
		}
		mergeMustNotTouchTheTree(t, crypto, receiver, members[4].LeafIndex, path,
			ErrParentHashMismatch,
			"a commit over a tree where a leaf entered the resolution of node 1 without node 3 recording it")
	}
}

// TestMergeUpdatePathBlanksTheWholeDirectPathAndNotOnlyTheFilteredOne is the difference between
// the two paths, which is invisible over a full tree.
//
// A filtered direct path drops every node whose copath child resolves to nothing, and over the
// complete trees the rest of this file uses nothing is ever dropped -- so a merge that refilled the
// filtered path without blanking the direct one reproduces the sender's tree exactly, and every
// other test here passes. The shape that separates them needs blank leaves under a copath child:
// in a five member group leaf 4's copath children at nodes 9 and 11 cover leaves 5, 6 and 7, all
// empty, so both nodes are dropped from the path the commit publishes.
//
// They are non-blank BEFORE the commit here, which is what makes the omission observable at all: a
// node dropped from a filtered path carries a key from an earlier epoch that this commit does not
// replace and must not leave standing, because the sender blanked it and the two trees have to be
// the same tree. What a merge that skipped the blanking leaves behind is a receiver whose tree hash
// is not the group's -- and, since nothing claims those stale nodes any more, a tree section 7.9.2
// also refuses.
func TestMergeUpdatePathBlanksTheWholeDirectPathAndNotOnlyTheFilteredOne(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, members := newTestTree(t, crypto, 5)
	// the previous epoch's keys at the two nodes this commit will drop.
	stale := []NodeIndex{9, 11}
	for _, x := range stale {
		_, pub, err := crypto.DeriveKeyPair(crypto.Random(crypto.HashSize()))
		if err != nil {
			t.Fatalf("DeriveKeyPair: %v", err)
		}
		if err := tree.SetParent(x, &ParentNode{EncryptionKey: pub, ParentHash: []byte{}}); err != nil {
			t.Fatalf("SetParent(%d): %v", x, err)
		}
	}
	// the fixture's premise, derived from the tree math rather than written down: the sender's
	// filtered path is strictly shorter than its direct path, and the nodes it is short by are
	// exactly the ones carrying a stale key.
	filtered, err := tree.FilteredDirectPath(members[4].LeafIndex)
	if err != nil {
		t.Fatalf("FilteredDirectPath: %v", err)
	}
	direct, err := directPathOf(members[4].LeafIndex.NodeIndex(), tree.LeafWidth())
	if err != nil {
		t.Fatalf("directPathOf: %v", err)
	}
	dropped := []NodeIndex{}
	for _, x := range direct {
		if !slices.Contains(filtered, x) {
			dropped = append(dropped, x)
		}
	}
	if !equalNodeIndices(dropped, stale) {
		t.Fatalf("this commit drops %v of the direct path %v, and the fixture put stale keys at %v",
			dropped, direct, stale)
	}

	senderTree, path, _, _ := createAndEncryptPath(t, crypto, tree, members[4], nil)
	// what the sender did with those nodes, read off task 18 rather than asserted here, so this
	// test states "the receiver ends up where the sender did" and not a second opinion about what
	// blanking means.
	for _, x := range stale {
		if senderTree.ParentAt(x) != nil {
			t.Fatalf("the sender left node %d occupied, so the expectation below is not task 18's", x)
		}
	}
	receiver := tree.Clone()
	if err := receiver.MergeUpdatePath(crypto, members[4].LeafIndex, path); err != nil {
		t.Fatalf("MergeUpdatePath: %v", err)
	}
	for _, x := range stale {
		if receiver.ParentAt(x) != nil {
			t.Fatalf("node %d was dropped from the filtered path and still carries the previous epoch's key after the merge", x)
		}
	}
	got, want := treeSnapshot(receiver), treeSnapshot(senderTree)
	if len(got) != len(want) {
		t.Fatalf("the merged tree holds %d nodes and the sender's holds %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("node %d after the merge is\n %s\nand the sender has\n %s", i, got[i], want[i])
		}
	}
}

// TestMergeUpdatePathRefusesALeafClaimingAParentInAGroupThatHasNone is the one place the
// commit-time comparison is not subsumed by the sweep that follows it.
//
// Everywhere else in this file the two refusals coincide, and they coincide for a reason worth
// writing down rather than leaving as a coincidence: the lowest node of a filtered direct path has
// the sender's own leaf as the ONLY entry in the resolution of the child on that path -- every
// node between them was dropped precisely because its copath child resolved to nothing -- so
// section 7.9.2's second condition at that node IS the comparison of the recomputed chain against
// the leaf's field, and a tampered path fails both. Which is why deleting the comparison passes
// every other test here.
//
// A group of ONE has no parent node at all. filteredPathSteps is empty, the recomputed chain is
// still the zero-length octet string section 7.9 gives the top of a path, and VerifyParentHashes
// sweeps no node and answers nil -- so the whole of what stands between a commit leaf carrying a
// signed claim about a parent that does not exist and the tree is this comparison. It is the
// state every group starts in and the one CreateUpdatePathSecrets' own comment names.
func TestMergeUpdatePathRefusesALeafClaimingAParentInAGroupThatHasNone(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, members := newTestTree(t, crypto, 1)
	_, path, _, _ := createAndEncryptPath(t, crypto, tree, members[0], nil)
	// the fixture's premise: this really is a commit over a tree with no parent to claim.
	if len(path.Nodes) != 0 {
		t.Fatalf("the one member commit published %d path nodes, and a group of one has no filtered direct path",
			len(path.Nodes))
	}
	if len(path.LeafNode.ParentHash) != 0 {
		t.Fatalf("the one member commit leaf carries a %d octet parent hash, and section 7.9 gives the top of a path the zero-length octet string",
			len(path.LeafNode.ParentHash))
	}
	// the control: the conforming commit merges.
	control := tree.Clone()
	if err := control.MergeUpdatePath(crypto, members[0].LeafIndex, path); err != nil {
		t.Fatalf("the one member commit was refused: %v", err)
	}
	// and the sweep alone cannot tell the two apart, which is what makes this fixture the
	// separator: there is no non-blank parent for it to judge either way.
	if err := control.VerifyParentHashes(crypto); err != nil {
		t.Fatalf("the merged one member tree does not pass section 7.9.2: %v", err)
	}
	claiming := &UpdatePath{LeafNode: *path.LeafNode.Clone(), Nodes: append([]UpdatePathNode{}, path.Nodes...)}
	claiming.LeafNode.ParentHash = bytes.Repeat([]byte{0x5A}, crypto.HashSize())
	forged := tree.Clone()
	if err := forged.SetLeaf(members[0].LeafIndex, claiming.LeafNode.Clone()); err != nil {
		t.Fatalf("SetLeaf: %v", err)
	}
	if err := forged.VerifyParentHashes(crypto); err != nil {
		t.Fatalf("section 7.9.2 refuses the forged tree on its own, so this fixture does not separate the two refusals: %v", err)
	}
	mergeMustNotTouchTheTree(t, crypto, tree.Clone(), members[0].LeafIndex, claiming,
		ErrParentHashMismatch, "a one member commit whose leaf claims a parent hash")
}

// ---------------------------------------------------------------------------
// the commit door: RFC 9420 section 7.3 over a received UpdatePath's leaf
// ---------------------------------------------------------------------------

// commitDoorGroupContext is the group a received UpdatePath is judged in, matching the group id
// and the leaf index leafValidationSignedLeaf signs under so that a leaf built there is accepted
// here. The urmessage_leaf_keys entry is what makes section 13.4's clause live at this door: a
// group using an extension its members must support.
func commitDoorGroupContext() *GroupContext {
	return &GroupContext{
		Version:     ProtocolVersionMls10,
		CipherSuite: CipherSuiteX25519ChaCha20Sha256Ed25519,
		GroupId:     []byte(leafValidationGroupId),
		Epoch:       1,
		Extensions: []Extension{
			{ExtensionType: ExtensionTypeUrmessageLeafKeys, ExtensionData: []byte("x")},
		},
	}
}

// commitDoorPathCarrying is a published-shaped UpdatePath whose leaf is the one handed in, so a
// case below changes the LEAF and nothing else about the path.
func commitDoorPathCarrying(leaf *LeafNode) *UpdatePath {
	path := testUpdatePathFixture()
	path.LeafNode = *leaf
	return path
}

// TestTheCommitDoorRefusesEveryWayAnUpdatePathLeafIsWrongForThisPosition is the commit half of
// section 7.3, stated one clause at a time over the door that did not exist.
//
// Before ValidateUpdatePathLeafNode the leaf of a received UpdatePath was held by nothing that
// judged it AS A LEAF. MergeUpdatePath recomputes the parent hash chain against it and says in as
// many words that it does not verify the signature; VerifyParentHashes counts claimants. Neither
// asks whether the leaf is signed by the key it names, whether that signature is bound to this
// group and this position, whether the credential is one this profile reads, or whether the member
// supports the extensions the group is using. Every row here is a leaf all of those accept.
//
// The wrap is asserted on every row alongside the clause's own value, because that pairing is what
// errUpdatePathLeafNodeInvalid promises a caller: which half of the commit to reject, and what
// section 7.3 said about it.
func TestTheCommitDoorRefusesEveryWayAnUpdatePathLeafIsWrongForThisPosition(t *testing.T) {
	crypto := leafValidationCrypto(t)
	// the control every row below is one change away from
	good := leafValidationSignedLeaf(t, crypto, LeafNodeSourceCommit, nil)
	if err := ValidateUpdatePathLeafNode(crypto, commitDoorGroupContext(),
		leafValidationLeafIndex, commitDoorPathCarrying(good)); err != nil {
		t.Fatalf("the commit door refused a well formed commit leaf at its own position: %v", err)
	}
	elsewhere := commitDoorGroupContext()
	elsewhere.GroupId = []byte("another group")
	unsupportedExtension := commitDoorGroupContext()
	unsupportedExtension.Extensions = append(unsupportedExtension.Extensions,
		Extension{ExtensionType: ExtensionType(0xF00A), ExtensionData: []byte{1}})
	unsupportedCapability := commitDoorGroupContext()
	unsupportedCapability.Extensions = append(unsupportedCapability.Extensions,
		testRequiredCapabilitiesNaming(t, ExtensionType(0xF00A)))
	forged := leafValidationSignedLeaf(t, crypto, LeafNodeSourceCommit, nil)
	forged.Signature = leafValidationSignedLeaf(t, crypto, LeafNodeSourceCommit, nil).Signature
	for name, row := range map[string]struct {
		context *GroupContext
		sender  LeafIndex
		leaf    *LeafNode
		inner   error
	}{
		// the leaf index half of section 7.2's context select: the committer's own leaf, its own
		// signature, offered at a position it was not signed for. A path accepted at another
		// index reseals another member's copath.
		"a commit leaf offered at another position": {
			context: commitDoorGroupContext(), sender: leafValidationLeafIndex + 1,
			leaf: good, inner: errBadSignature},
		// and the group id half of it, which is what stops a commit leaf being pasted into a
		// group its signer never committed in
		"a commit leaf pasted into another group": {
			context: elsewhere, sender: leafValidationLeafIndex,
			leaf: good, inner: errBadSignature},
		"a commit leaf carrying another leaf's signature": {
			context: commitDoorGroupContext(), sender: leafValidationLeafIndex,
			leaf: forged, inner: errBadSignature},
		// section 13.4 as corrected by erratum 8745, which ERRATA.md says this package applies to
		// all three sources -- and which, for a commit leaf, was reached by nobody
		"a commit leaf that does not support an extension the group carries": {
			context: unsupportedExtension, sender: leafValidationLeafIndex,
			leaf: good, inner: errGroupContextExtensionNotListed},
		// and the required_capabilities clause, which is this door reading the group's own
		// extensions rather than taking a second argument for them
		"a commit leaf that does not support a capability the group requires": {
			context: unsupportedCapability, sender: leafValidationLeafIndex,
			leaf: good, inner: errMissingRequiredCapability},
	} {
		err := ValidateUpdatePathLeafNode(crypto, row.context, row.sender,
			commitDoorPathCarrying(row.leaf))
		if !errors.Is(err, errUpdatePathLeafNodeInvalid) {
			t.Errorf("the commit door over %s answered %v, which does not answer to errUpdatePathLeafNodeInvalid",
				name, err)
		}
		if !errors.Is(err, row.inner) {
			t.Errorf("the commit door over %s answered %v, want %v underneath; a caller cannot ask which section 7.3 clause fired",
				name, err, row.inner)
		}
	}
}

// TestTheCommitDoorRefusesEveryArgumentItCannotJudgeWithout is the argument rule, and it is a
// refusal of this door like any other: a client processing a commit reaches it with whatever it
// holds, and a door that dereferenced a missing argument would take the caller's process rather
// than its call.
//
// The provider is FIRST, ahead of the group context and the path, which is the order every
// declaration of this package taking a provider is held to -- and is the only order that does not
// read a field off an argument the caller never supplied.
func TestTheCommitDoorRefusesEveryArgumentItCannotJudgeWithout(t *testing.T) {
	crypto := leafValidationCrypto(t)
	good := commitDoorPathCarrying(leafValidationSignedLeaf(t, crypto, LeafNodeSourceCommit, nil))
	for name, row := range map[string]struct {
		call func() error
		want error
	}{
		"no provider at all": {want: ErrNilCryptoProvider, call: func() error {
			return ValidateUpdatePathLeafNode(nil, commitDoorGroupContext(),
				leafValidationLeafIndex, good)
		}},
		"no provider and no other argument either": {want: ErrNilCryptoProvider, call: func() error {
			return ValidateUpdatePathLeafNode(nil, nil, 0, nil)
		}},
		"no group context": {want: ErrNilGroupContext, call: func() error {
			return ValidateUpdatePathLeafNode(crypto, nil, leafValidationLeafIndex, good)
		}},
		"no update path": {want: errNilUpdatePath, call: func() error {
			return ValidateUpdatePathLeafNode(crypto, commitDoorGroupContext(),
				leafValidationLeafIndex, nil)
		}},
	} {
		answered := func() (out error) {
			defer func() {
				if panicked := recover(); panicked != nil {
					t.Errorf("the commit door with %s panicked with %v", name, panicked)
					out = nil
				}
			}()
			return row.call()
		}()
		if !errors.Is(answered, row.want) {
			t.Errorf("the commit door with %s answered %v, want %v", name, answered, row.want)
		}
	}
}

// commitPathValidUnder is an UpdatePath whose leaf passes ValidateUpdatePathLeafNode in one group,
// built from that group's own context rather than from a fixture's assumptions.
//
// Written for the sweeps in other files that need a path this door ACCEPTS -- the nil argument
// gate needs the other arguments live so its refusal is attributable to the nil one, and the
// KDF.Nh gate needs the call to work over a provider whose hash is 48. Both would otherwise carry
// a copy of this construction, and two copies of "what this door accepts" is two chances to write
// one of them wrong.
//
// The leaf's capabilities are taken FROM the context: the ciphersuite it announces, and every
// extension type it carries. A fixture that listed a fixed vector would refuse in any group whose
// extensions this file did not anticipate, and the refusal would be attributed to whatever the
// caller was measuring.
func commitPathValidUnder(t *testing.T, crypto CryptoProvider, context *GroupContext,
	sender LeafIndex) *UpdatePath {
	t.Helper()
	signerPriv, signerPub, err := crypto.SignatureKeyPair()
	if err != nil {
		t.Fatalf("SignatureKeyPair for the commit leaf: %v", err)
	}
	listed := []ExtensionType{}
	for i := range context.Extensions {
		listed = append(listed, context.Extensions[i].ExtensionType)
	}
	leaf := &LeafNode{
		EncryptionKey: HpkePublicKey(repeatByte(0x11, 32)),
		SignatureKey:  signerPub,
		Credential:    BasicCredential([]byte("alice")),
		Capabilities: Capabilities{
			Versions:     []ProtocolVersion{ProtocolVersionMls10},
			CipherSuites: []CipherSuite{context.CipherSuite},
			Extensions:   listed,
			Proposals:    []ProposalType{},
			Credentials:  []CredentialType{CredentialTypeBasic},
		},
		LeafNodeSource: LeafNodeSourceCommit,
		ParentHash:     repeatByte(0x44, 32),
		Extensions:     []Extension{},
	}
	if err := leaf.Sign(crypto, signerPriv, context.GroupId, sender); err != nil {
		t.Fatalf("sign the commit leaf at leaf %d: %v", sender, err)
	}
	return &UpdatePath{LeafNode: *leaf, Nodes: []UpdatePathNode{}}
}

// providerUpdatePathPerturbations moves a received UpdatePath in the half a validator judges,
// which is the LEAF inside it.
//
// Its rule lives here rather than in crypto_test.go's switch for the reason
// providerHpkeCiphertextPerturbations' does: the generic byte rule cannot reach through a pointer
// to a struct, and a rule that moved the path's node vector would be moving the half
// ValidateUpdatePathLeafNode is written NOT to judge -- the ciphertexts and the node keys belong to
// MergeUpdatePath and DecryptUpdatePath.
//
// Every row is a change the commit door must notice, and each moves a DIFFERENT clause of section
// 7.3: the signature itself, the key the signature is checked against, the source that selects what
// the signature covers, the identity inside the credential, and the encryption key the tree will
// install. A single row would be satisfied by a door that read one field.
//
// The path is CLONED per row -- the leaf through LeafNode.Clone, which makes every vector the
// clone's own -- so a perturbation cannot write through into the base argument every other row of
// that gate is built from.
func providerUpdatePathPerturbations(t *testing.T, operation string, parameter providerParameter,
	argument reflect.Value) []providerPerturbation {
	t.Helper()
	base := argument.Interface().(*UpdatePath)
	moved := []providerPerturbation{}
	for _, row := range []struct {
		where string
		edit  func(leaf *LeafNode)
	}{
		{where: "the leaf's signature", edit: func(leaf *LeafNode) { leaf.Signature[0] ^= 0xff }},
		{where: "the leaf's signature key", edit: func(leaf *LeafNode) { leaf.SignatureKey[0] ^= 0xff }},
		{where: "the leaf's encryption key", edit: func(leaf *LeafNode) { leaf.EncryptionKey[0] ^= 0xff }},
		{where: "the leaf's source", edit: func(leaf *LeafNode) {
			leaf.LeafNodeSource = LeafNodeSourceUpdate
		}},
		{where: "the identity in the leaf's credential", edit: func(leaf *LeafNode) {
			leaf.Credential.Identity[0] ^= 0xff
		}},
	} {
		leaf := base.LeafNode.Clone()
		if len(leaf.Signature) == 0 || len(leaf.SignatureKey) == 0 ||
			len(leaf.EncryptionKey) == 0 || len(leaf.Credential.Identity) == 0 {
			t.Fatalf("the base argument for %s.%s carries a leaf with an empty vector, so a perturbation of it changes nothing",
				operation, parameter.name)
		}
		row.edit(leaf)
		moved = append(moved, providerPerturbation{
			where: row.where,
			value: reflect.ValueOf(&UpdatePath{LeafNode: *leaf, Nodes: base.Nodes}),
		})
	}
	return moved
}

// providerCommitDoorContextPerturbations moves the three fields of a GroupContext that
// ValidateUpdatePathLeafNode reads, and it exists because the generic rule moves the one field it
// must not.
//
// The generic *GroupContext perturbation moves the EPOCH, on the grounds that it is "the field two
// contexts of one group differ in and nothing else" -- which is right for every construction whose
// preimage carries a group context. A LeafNodeTBS does not: section 7.2 binds an update or commit
// leaf to the group id and the leaf index and to nothing else about the epoch, so a leaf validator
// whose verdict moved with the epoch would be refusing leaves that are still perfectly valid one
// commit later. The epoch is the field this door is RIGHT not to read, and a rule that demanded
// otherwise would be asking for a defect.
//
// So the three it does read are moved instead, one clause each: the group id, which the signature
// covers; the ciphersuite, which section 11.1 requires the leaf to list; and the extensions, which
// section 13.4 as corrected by erratum 8745 requires it to support. Each must change the verdict.
func providerCommitDoorContextPerturbations(t *testing.T, operation string,
	parameter providerParameter, argument reflect.Value) []providerPerturbation {
	t.Helper()
	base := argument.Interface().(*GroupContext)
	if len(base.GroupId) == 0 {
		t.Fatalf("the base argument for %s.%s carries no group id, so moving it changes nothing",
			operation, parameter.name)
	}
	elsewhere := base.Clone()
	elsewhere.GroupId[0] ^= 0xff
	otherSuite := base.Clone()
	for _, suite := range Suites() {
		if suite != base.CipherSuite {
			otherSuite.CipherSuite = suite
			break
		}
	}
	if otherSuite.CipherSuite == base.CipherSuite {
		t.Fatalf("this profile registers one ciphersuite, so %s.%s has no second suite to move to",
			operation, parameter.name)
	}
	demanding := base.Clone()
	demanding.Extensions = append(demanding.Extensions,
		Extension{ExtensionType: ExtensionType(0xF00A), ExtensionData: []byte{1}})
	return []providerPerturbation{
		{where: "byte 0 of the group id", value: reflect.ValueOf(elsewhere)},
		{where: "the ciphersuite moved to another registered one", value: reflect.ValueOf(otherSuite)},
		{where: "an extension the leaf does not list added to the group", value: reflect.ValueOf(demanding)},
	}
}
