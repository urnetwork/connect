// The cost of this plan's tree operations at the sizing MASTER fixes, and the one open question
// task 21 left behind.
//
// Ledger open item 15: every MergeUpdatePath runs a whole-tree RFC 9420 section 7.9.2 sweep --
// one ParentHash and one ORIGINAL subtree tree hash per arm of every non-blank parent -- and
// nobody had measured it. The sweep is the superlinear term in this package: an original tree hash
// is a walk of the whole subtree under the copath child, so summed over the parents it is O(n
// log n) node hashes for a tree of n leaves. It runs on every join and on every out-of-band tree,
// and a regression in it is felt as a join that takes seconds, which is discovered by a user
// rather than by CI.
//
// Two things about the shape of these benchmarks are load bearing, and both were found by
// measurement rather than reasoned about.
//
// The first is the WIDTH. The design target is 500 MEMBERS x 2 DEVICES. Spec A section 3.1 caps a
// group at 500 members and 10 device leaves per identity, and the ratchet tree holds one leaf per
// DEVICE, so the tree this code runs on at the target is a THOUSAND leaves and not five hundred.
// The plan's benchmarks are named for 500 and are kept at 500 so the number they record is the one
// the plan asked for; every benchmark that answers open item 15 is run at both widths, because a
// superlinear term measured at half its input reads as affordable for the wrong reason.
//
// The second is the DENSITY, and it is the one that would have made this whole file report a
// number that means nothing. The plan's fixture is newTestTree plus ONE commit. A commit populates
// exactly the committer's own direct path, so that tree has NINE non-blank parents out of 511, the
// sweep skips 98% of the nodes it is supposed to walk, and it measures at 1 ms -- which a reader
// would take as the answer to open item 15 and it is not. The tree a real group runs on is dense:
// every member that has ever committed has left its direct path populated, and the working group's
// own tree-validation vectors carry 290 non-blank parents. So the sweep is measured on a tree
// where nearly every parent is non-blank, that density is DERIVED from the TREE rather than
// assumed, and TestTheDenseBenchmarkTreeIsDense is what holds it -- because a benchmark's premise
// is not checked by running the benchmark.
//
// The derivation is from the tree and not from the committing set, and that distinction is the one
// rule 5 names rather than a nicety. The first version of that gate compared the tree's non-blank
// parents against the union of the committers' filtered direct paths, and BOTH sides of that come
// off benchDenseCommitters: a fixture committing from every sixty-fourth leaf rather than every
// second agreed with itself at 55 of 511 non-blank parents, the gate reported that the premise
// held, and the sweep this file exists to price fell to a tenth of the real number with nothing
// turning red. parentsThatFullCommitmentRequires is the anchor that was missing -- a parent is
// non-blank exactly when both of its child subtrees hold an occupied leaf, which names no committer
// at all.
//
// Every benchmark calls b.ReportAllocs and resets the timer after its fixture, and neither is
// decoration: newTestTree at a thousand leaves is a thousand signature key pairs, a thousand HPKE
// derivations and a full section 7.3 validation sweep, and the dense fixture is 500 commits on top
// of that. Both are far more work than any operation under test, so a benchmark that measured its
// own fixture would report the fixture.
package mls

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// benchTreeFixture is one built tree and its members, kept so that the fixture is paid once per
// width per process rather than once per benchmark invocation.
//
// The go test harness calls a benchmark function at least twice -- once at b.N = 1 to find the
// scale, then at the chosen N -- so an uncached thousand-leaf fixture is built twice per benchmark
// and the wall clock of this file is dominated by key generation nobody measures.
type benchTreeFixture struct {
	tree    *RatchetTree
	members []*testTreeMember
	crypto  CryptoProvider
}

var (
	benchTreeOnce   sync.Map
	benchTreeCache  sync.Map
	benchDenseOnce  sync.Map
	benchDenseCache sync.Map
)

// benchmarkTree answers a FRESH tree of n leaves, every parent blank, and the members that occupy
// it. That is the shape a group has when everybody has been added and nobody has committed.
//
// The returned tree is a clone of the cached one, and that is what keeps these benchmarks
// independent of each other and of their own iteration count: CreateUpdatePathSecrets MUTATES the
// tree it is called on -- it blanks the sender's direct path and installs a fresh chain -- so a
// benchmark handed the cache directly would leave the next one measuring a tree the previous one
// committed over. Clone copies every node, so the members' private keys still match the leaves the
// clone publishes.
func benchmarkTree(b *testing.B, n uint32) (*RatchetTree, []*testTreeMember, CryptoProvider) {
	b.Helper()
	fixture := benchTreeFixtureFor(b, n)
	return fixture.tree.Clone(), fixture.members, fixture.crypto
}

func benchTreeFixtureFor(t testing.TB, n uint32) *benchTreeFixture {
	t.Helper()
	once, _ := benchTreeOnce.LoadOrStore(n, &sync.Once{})
	once.(*sync.Once).Do(func() {
		crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
		if err != nil {
			t.Fatalf("NewCryptoProvider: %v", err)
		}
		tree, members := newTestTree(t, crypto, n)
		benchTreeCache.Store(n, &benchTreeFixture{tree: tree, members: members, crypto: crypto})
	})
	cached, present := benchTreeCache.Load(n)
	if !present {
		t.Fatalf("the %d leaf fixture was not built", n)
	}
	return cached.(*benchTreeFixture)
}

// benchDenseCommitters is the set of leaves that commit to build the dense tree: every other one.
//
// Every other leaf rather than every leaf, and the halving is exact rather than a sample. A parent
// is left non-blank by a commit from any leaf in its subtree, and the smallest subtree a parent
// has is the two leaves directly under it, so one commit per adjacent PAIR reaches every parent
// that has a member under it at all. Committing from all n leaves would populate the same set of
// parents at twice the fixture cost, and committing from fewer would leave a band of height-one
// parents blank -- which is the density the sweep's cost is proportional to, so it would quietly
// halve the number this file exists to report.
func benchDenseCommitters(members []*testTreeMember) []LeafIndex {
	committers := []LeafIndex{}
	for index := 0; index < len(members); index += 2 {
		committers = append(committers, members[index].LeafIndex)
	}
	return committers
}

// benchDenseFixtureFor is the tree a group that has been running is actually shaped like: every
// parent with a member under it carries an encryption key and a parent hash.
func benchDenseFixtureFor(t testing.TB, n uint32) *benchTreeFixture {
	t.Helper()
	once, _ := benchDenseOnce.LoadOrStore(n, &sync.Once{})
	once.(*sync.Once).Do(func() {
		base := benchTreeFixtureFor(t, n)
		dense := base.tree.Clone()
		for _, leaf := range benchDenseCommitters(base.members) {
			member := base.members[leaf]
			if _, err := dense.CreateUpdatePathSecrets(base.crypto, member.LeafIndex,
				member.SignaturePriv, testGroupId()); err != nil {
				t.Fatalf("CreateUpdatePathSecrets(leaf %d): %v", member.LeafIndex, err)
			}
		}
		benchDenseCache.Store(n, &benchTreeFixture{tree: dense, members: base.members, crypto: base.crypto})
	})
	cached, present := benchDenseCache.Load(n)
	if !present {
		t.Fatalf("the dense %d leaf fixture was not built", n)
	}
	return cached.(*benchTreeFixture)
}

func benchmarkDenseTree(b *testing.B, n uint32) (*RatchetTree, []*testTreeMember, CryptoProvider) {
	b.Helper()
	fixture := benchDenseFixtureFor(b, n)
	return fixture.tree.Clone(), fixture.members, fixture.crypto
}

// benchmarkCommittedTree is a tree one member has committed a path over, plus the encrypted path
// and the group context those ciphertexts were sealed under, so the merge side and the verify side
// measure the same commit. base decides whether the commit lands on the blank-parent tree or on
// the dense one.
func benchmarkCommittedTree(b *testing.B, base *RatchetTree, members []*testTreeMember,
	crypto CryptoProvider) (*UpdatePath, []byte) {
	b.Helper()
	sender := base.Clone()
	plan, err := sender.CreateUpdatePathSecrets(crypto, members[0].LeafIndex,
		members[0].SignaturePriv, testGroupId())
	if err != nil {
		b.Fatalf("CreateUpdatePathSecrets: %v", err)
	}
	treeHash, err := sender.TreeHash(crypto)
	if err != nil {
		b.Fatalf("TreeHash: %v", err)
	}
	path, err := sender.EncryptUpdatePath(crypto, plan, members[0].LeafIndex, treeHash, nil)
	if err != nil {
		b.Fatalf("EncryptUpdatePath: %v", err)
	}
	return path, treeHash
}

// countNonBlankParents is what the sweep actually walks, reported alongside the timings so the
// number a later reader compares against is attached to the tree it was measured on rather than to
// a member count.
func countNonBlankParents(tree *RatchetTree) int {
	parents := 0
	for x := uint32(1); x < tree.NodeWidth(); x += 2 {
		if tree.ParentAt(NodeIndex(x)) != nil {
			parents += 1
		}
	}
	return parents
}

// parentsOverCommitters is the number of parents a commit from each of these leaves must leave
// non-blank: the union of their FILTERED direct paths.
//
// DERIVED from the container's own arithmetic rather than written down as a count, for rule 5's
// reason, and the FILTERED path rather than the plain one because that is what
// CreateUpdatePathSecrets populates. The difference is not a rounding: a 500-member group sits in a
// 512-leaf tree, so leaves 500 to 511 are blank, and the two ancestors of leaf 496 whose copath
// child is entirely inside that blank tail resolve to nothing, are dropped from the path, and stay
// blank. The first version of this helper counted every parent with a committer under it, said 501,
// and would have had to be "corrected" to the 499 the tree actually has -- which is the shape rule
// 5 names: a number adjusted until it matches is a number that has stopped checking anything.
func parentsOverCommitters(t testing.TB, tree *RatchetTree, committers []LeafIndex) int {
	t.Helper()
	covered := map[NodeIndex]bool{}
	for _, leaf := range committers {
		path, err := tree.FilteredDirectPath(leaf)
		if err != nil {
			t.Fatalf("FilteredDirectPath(%d): %v", leaf, err)
		}
		for _, node := range path {
			covered[node] = true
		}
	}
	return len(covered)
}

// parentsThatFullCommitmentRequires is how many non-blank parents a tree HAS once every member has
// committed, derived from which of its leaves are occupied and from nothing else.
//
// This is the absolute the gate below is anchored on, and it exists because the obvious comparison
// is not one. Counting the parents the committing set covers puts benchDenseCommitters on both
// sides of an equality, so a fixture that commits from a sixteenth of the leaves agrees with itself
// and reports that the premise holds.
//
// The rule is a property of the ratchet tree. CreateUpdatePathSecrets blanks the committer's whole
// direct path and then repopulates its FILTERED one, so after the last commit a parent p is
// non-blank exactly when the last committer under p kept p on that filtered path -- which happens
// exactly when p's other child subtree resolves to something, and a subtree resolves to something
// exactly when it holds an occupied leaf. So p is non-blank if and only if BOTH of p's child
// subtrees hold an occupied leaf, and neither side of that names a committer.
//
// It is also the most a tree can carry, which is what makes an equality against it worth asserting:
// no commit can populate a parent whose copath child resolves to nothing. That is why a 500 member
// group answers 499 here rather than the 501 a "has a member somewhere under it" count gives -- the
// two ancestors of leaf 496 whose copath child lies entirely in the blank tail of the 512 leaf tree
// are dropped from every filtered path and stay blank however many commits run.
func parentsThatFullCommitmentRequires(t testing.TB, tree *RatchetTree) int {
	t.Helper()
	required := 0
	for x := uint32(1); x < tree.NodeWidth(); x += 2 {
		left, leftOk := leftOf(NodeIndex(x))
		right, rightOk := rightOf(NodeIndex(x))
		if !leftOk || !rightOk {
			t.Fatalf("node %d is an odd index with no two children, so this receiver is not a tree", x)
		}
		if subtreeHoldsAnOccupiedLeaf(tree, left) && subtreeHoldsAnOccupiedLeaf(tree, right) {
			required += 1
		}
	}
	return required
}

// subtreeHoldsAnOccupiedLeaf reports whether any leaf under x carries a member, which is the same
// question as whether that subtree's resolution is non-empty and is asked of the LEAVES so that the
// answer does not move when the parents above them do.
func subtreeHoldsAnOccupiedLeaf(tree *RatchetTree, x NodeIndex) bool {
	first, last := SubtreeLeaves(x)
	for leaf := first; leaf <= last; leaf += 1 {
		if tree.Leaf(leaf) != nil {
			return true
		}
	}
	return false
}

// benchGateWidths are the leaf counts the two gates below run at.
//
// Under -short they run at a width whose fixture is cheap rather than being SKIPPED, and that is a
// correction: both gates carried a t.Skip, so a -short lane checked neither the density premise nor
// the join bound, and the zero skipped count this package reports was a property of the long lane
// only. A tree of 32 leaves states every assertion below -- the derived density, the equality with
// what the tree actually carries, the refusal of a forged parent at every non-blank position -- and
// the only thing it does not state is the design target's NUMBER, which is what the long lane is
// for and what the log line names.
func benchGateWidths() []uint32 {
	if testing.Short() {
		return []uint32{32}
	}
	return []uint32{500, 1000}
}

// TestTheDenseBenchmarkTreeIsDense is the premise of every Dense benchmark below, stated where a
// plain go test run reaches it.
//
// A benchmark's fixture is never checked by running the benchmark: a sweep over nine parents and a
// sweep over 499 both report a duration, and the one that means nothing reports the smaller number
// with no note attached. This is the note, and it is an assertion rather than a log because the
// number it defends is the answer to open item 15. Both directions are held: the dense tree is
// dense, and the tree the plan's own fixture would have handed the sweep is NOT -- so a later
// change that reverted the fixture turns this red instead of quietly reporting a tenth of the cost.
func TestTheDenseBenchmarkTreeIsDense(t *testing.T) {
	for _, width := range benchGateWidths() {
		t.Run(fmt.Sprintf("leaves=%d", width), func(t *testing.T) {
			base := benchTreeFixtureFor(t, width)
			dense := benchDenseFixtureFor(t, width)

			got := countNonBlankParents(dense.tree)

			// the anchor, and the only comparison here that does not mention the committing set:
			// the tree carries every parent a fully committed group leaves non-blank.
			required := parentsThatFullCommitmentRequires(t, dense.tree)
			// a control on the derivation itself, so that a helper which had stopped counting
			// could not be satisfied by a fixture which had stopped committing. Every adjacent
			// pair of occupied leaves is a parent with an occupied leaf under each child, so a
			// tree of n occupied leaves requires at least n/2 of them.
			if minimum := int(width / 2); required < minimum {
				t.Fatalf("the derivation says a %d leaf tree needs only %d non-blank parents, and its %d adjacent leaf pairs alone account for that many; the equality below would be met by almost any fixture",
					width, required, minimum)
			}
			if got != required {
				t.Errorf("the dense tree carries %d non-blank parents and a group in which every member has committed carries %d; the sweep benchmarks would report the cost of a tree that is not the one a running group has",
					got, required)
			}

			// and against the committing set, separately, because the two say different things:
			// the equality above is the fixture against the TREE, and this is the fixture against
			// the paths its own commits are supposed to populate.
			expected := parentsOverCommitters(t, dense.tree, benchDenseCommitters(dense.members))
			if expected == 0 {
				t.Fatal("the committing set covers no parent, so this gate asserts nothing")
			}
			if got != expected {
				t.Errorf("the dense tree carries %d non-blank parents and every parent over a committing leaf is %d; a commit is not leaving its own filtered direct path populated",
					got, expected)
			}

			// the plan's own fixture, for contrast, and the reason this file does not use it: one
			// commit reaches one direct path, which is log2(width) parents out of half the node
			// width.
			single := base.tree.Clone()
			if _, err := single.CreateUpdatePathSecrets(base.crypto, base.members[0].LeafIndex,
				base.members[0].SignaturePriv, testGroupId()); err != nil {
				t.Fatalf("CreateUpdatePathSecrets: %v", err)
			}
			sparse := countNonBlankParents(single)
			if sparse >= got {
				t.Errorf("a single commit left %d non-blank parents and the dense tree has %d; if one commit already populates the tree then the dense fixture is buying nothing and this file should say so rather than paying for it",
					sparse, got)
			}
			t.Logf("%d leaves: %d of %d parents non-blank after %d commits (a fully committed group requires %d), against %d after one",
				width, got, dense.tree.NodeWidth()/2, len(benchDenseCommitters(dense.members)), required, sparse)
		})
	}
}

// ---------------------------------------------------------------------------
// tree hash
// ---------------------------------------------------------------------------

func BenchmarkTreeHash500(b *testing.B) {
	tree, _, crypto := benchmarkDenseTree(b, 500)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := tree.TreeHash(crypto); err != nil {
			b.Fatalf("TreeHash: %v", err)
		}
	}
}

// BenchmarkTreeHashes500 is the whole COLUMN rather than the root, which is the shape the parent
// hash sweep and the tree-validation vector both consume. tree_hash.go re-walks each subtree rather
// than memoising, by its own recorded decision, and this is where that decision is priced.
func BenchmarkTreeHashes500(b *testing.B) {
	tree, _, crypto := benchmarkDenseTree(b, 500)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := tree.TreeHashes(crypto); err != nil {
			b.Fatalf("TreeHashes: %v", err)
		}
	}
}

// ---------------------------------------------------------------------------
// open item 15: the whole-tree section 7.9.2 sweep
// ---------------------------------------------------------------------------

// BenchmarkVerifyParentHashes500 is the plan's own benchmark, on the plan's own fixture: one
// commit, nine non-blank parents. It is kept because the plan named it and because the contrast
// with the Dense pair below is the finding, not because this number answers anything.
func BenchmarkVerifyParentHashes500(b *testing.B) {
	tree, members, crypto := benchmarkTree(b, 500)
	if _, err := tree.CreateUpdatePathSecrets(crypto, members[0].LeafIndex,
		members[0].SignaturePriv, testGroupId()); err != nil {
		b.Fatalf("CreateUpdatePathSecrets: %v", err)
	}
	benchmarkSweep(b, tree, crypto)
}

// BenchmarkVerifyParentHashesDense500 and BenchmarkVerifyParentHashesDense1000 are open item 15's
// question: the whole-tree sweep on a tree whose parents are populated, at the design target and at
// half of it.
func BenchmarkVerifyParentHashesDense500(b *testing.B) {
	tree, _, crypto := benchmarkDenseTree(b, 500)
	benchmarkSweep(b, tree, crypto)
}

func BenchmarkVerifyParentHashesDense1000(b *testing.B) {
	tree, _, crypto := benchmarkDenseTree(b, 1000)
	benchmarkSweep(b, tree, crypto)
}

func benchmarkSweep(b *testing.B, tree *RatchetTree, crypto CryptoProvider) {
	b.Helper()
	parents := countNonBlankParents(tree)
	if parents == 0 {
		b.Fatal("this tree has no non-blank parent, so the benchmark times a loop that skips every node")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := tree.VerifyParentHashes(crypto); err != nil {
			b.Fatalf("VerifyParentHashes: %v", err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(parents), "parents")
}

// ---------------------------------------------------------------------------
// path generation and path processing
// ---------------------------------------------------------------------------

func BenchmarkCreateUpdatePathSecrets500(b *testing.B) {
	tree, members, crypto := benchmarkDenseTree(b, 500)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		working := tree.Clone()
		b.StartTimer()
		if _, err := working.CreateUpdatePathSecrets(crypto, members[0].LeafIndex,
			members[0].SignaturePriv, testGroupId()); err != nil {
			b.Fatalf("CreateUpdatePathSecrets: %v", err)
		}
	}
}

func BenchmarkCreateAndEncryptUpdatePath500(b *testing.B) {
	tree, members, crypto := benchmarkDenseTree(b, 500)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// the clone is outside the timer for the reason benchmarkMergeUpdatePath states: these
		// calls MUTATE the tree, so a per iteration copy is unavoidable, and a two thousand node
		// deep copy inside the timer is reported as part of the operation. It was inside it here
		// and in the merge-and-decrypt pair below while the two neighbouring benchmarks excluded
		// it, which left the pair answering open item 15 not comparable with the pair beside it.
		b.StopTimer()
		working := tree.Clone()
		b.StartTimer()
		plan, err := working.CreateUpdatePathSecrets(crypto, members[0].LeafIndex,
			members[0].SignaturePriv, testGroupId())
		if err != nil {
			b.Fatalf("CreateUpdatePathSecrets: %v", err)
		}
		treeHash, err := working.TreeHash(crypto)
		if err != nil {
			b.Fatalf("TreeHash: %v", err)
		}
		if _, err := working.EncryptUpdatePath(crypto, plan, members[0].LeafIndex, treeHash, nil); err != nil {
			b.Fatalf("EncryptUpdatePath: %v", err)
		}
	}
}

// BenchmarkMergeUpdatePathDense500 and BenchmarkMergeUpdatePathDense1000 are open item 15's other
// half, isolated from the decrypt beside it: MergeUpdatePath is the call that runs the sweep, and a
// number that also carried an HPKE open and a path-secret ladder would not say what fraction of a
// received commit the sweep is.
//
// The clone is outside the timer rather than inside it, because merge MUTATES the tree and the
// benchmark would otherwise be measuring a two-thousand-node deep copy alongside the operation.
func BenchmarkMergeUpdatePathDense500(b *testing.B) {
	benchmarkMergeUpdatePath(b, 500)
}

func BenchmarkMergeUpdatePathDense1000(b *testing.B) {
	benchmarkMergeUpdatePath(b, 1000)
}

func benchmarkMergeUpdatePath(b *testing.B, n uint32) {
	tree, members, crypto := benchmarkDenseTree(b, n)
	path, _ := benchmarkCommittedTree(b, tree, members, crypto)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		receiver := tree.Clone()
		b.StartTimer()
		if err := receiver.MergeUpdatePath(crypto, members[0].LeafIndex, path); err != nil {
			b.Fatalf("MergeUpdatePath: %v", err)
		}
	}
}

func BenchmarkMergeAndDecryptUpdatePath500(b *testing.B) {
	tree, members, crypto := benchmarkDenseTree(b, 500)
	path, groupContext := benchmarkCommittedTree(b, tree, members, crypto)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		receiver := tree.Clone()
		b.StartTimer()
		if err := receiver.MergeUpdatePath(crypto, members[0].LeafIndex, path); err != nil {
			b.Fatalf("MergeUpdatePath: %v", err)
		}
		priv := NewTreeKEMPrivate(members[1].LeafIndex, members[1].EncryptionPriv)
		if _, err := receiver.DecryptUpdatePath(crypto, members[0].LeafIndex, path,
			groupContext, priv, nil); err != nil {
			b.Fatalf("DecryptUpdatePath: %v", err)
		}
	}
}

// ---------------------------------------------------------------------------
// tree operations
// ---------------------------------------------------------------------------

func BenchmarkResolutionOfRoot500(b *testing.B) {
	tree, _, _ := benchmarkTree(b, 500)
	root, err := rootOf(tree.LeafWidth())
	if err != nil {
		b.Fatalf("rootOf: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if len(tree.Resolution(root)) == 0 {
			b.Fatal("the resolution of the root of a 500 leaf tree is empty")
		}
	}
}

func BenchmarkFilteredDirectPath500(b *testing.B) {
	tree, members, _ := benchmarkDenseTree(b, 500)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := tree.FilteredDirectPath(members[0].LeafIndex); err != nil {
			b.Fatalf("FilteredDirectPath: %v", err)
		}
	}
}

func BenchmarkAddLeaf500(b *testing.B) {
	tree, _, _ := benchmarkDenseTree(b, 500)
	leaf := tree.Leaf(LeafIndex(0)).Clone()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		working := tree.Clone()
		b.StartTimer()
		if _, err := working.AddLeaf(leaf.Clone()); err != nil {
			b.Fatalf("AddLeaf: %v", err)
		}
	}
}

func BenchmarkRatchetTreeEncode500(b *testing.B) {
	tree, _, _ := benchmarkDenseTree(b, 500)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := marshalRatchetTree(tree); err != nil {
			b.Fatalf("marshalRatchetTree: %v", err)
		}
	}
}

func BenchmarkRatchetTreeDecode500(b *testing.B) {
	tree, _, _ := benchmarkDenseTree(b, 500)
	encoded, err := marshalRatchetTree(tree)
	if err != nil {
		b.Fatalf("marshalRatchetTree: %v", err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(encoded)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := UnmarshalRatchetTree(encoded); err != nil {
			b.Fatalf("UnmarshalRatchetTree: %v", err)
		}
	}
}

// ---------------------------------------------------------------------------
// the join, as a bound rather than as a rate
// ---------------------------------------------------------------------------

// TestJoinCostAtTheDesignTargetIsBounded is the join path -- validate a tree that arrived out of
// band -- at both widths of the 500-member x 2-device target, because it runs before the first
// message renders and a joiner has nothing to show until it returns.
//
// It asserts on the WORK as well as on the clock. A wall-clock bound alone is satisfied by a
// Validate that returned early, by a tree whose parents are all blank, and by a sweep somebody
// narrowed to nothing -- all of which are fast for the wrong reason and none of which a bound
// distinguishes from a sound tree validated quickly. So the tree it validates is the dense one, its
// density is asserted rather than assumed, and the sweep is required to REFUSE a tree with one
// parent key moved as well as to accept the sound one.
func TestJoinCostAtTheDesignTargetIsBounded(t *testing.T) {
	for _, width := range benchGateWidths() {
		t.Run(fmt.Sprintf("leaves=%d", width), func(t *testing.T) {
			fixture := benchDenseFixtureFor(t, width)
			crypto := fixture.crypto
			tree := fixture.tree.Clone()

			parents := countNonBlankParents(tree)
			required := parentsThatFullCommitmentRequires(t, tree)
			if parents != required || parents == 0 {
				t.Fatalf("the tree carries %d non-blank parents and a group in which every member has committed carries %d; the elapsed times below would be the cost of a sweep over a tree that is not the one a running group has",
					parents, required)
			}

			start := time.Now()
			if err := tree.Validate(testTreeValidationContext(t, crypto)); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			elapsed := time.Since(start)

			sweepStart := time.Now()
			if err := tree.VerifyParentHashes(crypto); err != nil {
				t.Fatalf("VerifyParentHashes: %v", err)
			}
			sweep := time.Since(sweepStart)
			// a sweep over hundreds of parents that costs nothing a clock can see is a sweep that
			// did not happen. Narrowing tree_hash.go's loop to two of 999 parents made this line
			// print "of which VerifyParentHashes 0s" and nothing here asked about it.
			if sweep <= 0 {
				t.Errorf("VerifyParentHashes over %d non-blank parents took no measurable time; the figure this test exists to report is the cost of a walk that is not happening",
					parents)
			}

			// the sweep answered for a sound tree. that it also REFUSES is what says the number
			// above is the cost of a check rather than of a walk that returns nil either way -- a
			// narrowed sweep is faster, and it is the failure open item 15 warns against.
			forged := tree.Clone()
			moved := 0
			for x := uint32(1); x < forged.NodeWidth(); x += 2 {
				parent := forged.ParentAt(NodeIndex(x))
				if parent == nil {
					continue
				}
				if len(parent.EncryptionKey) == 0 {
					t.Fatalf("parent %d carries no encryption key, so moving it moves nothing", x)
				}
				replacement := parent.Clone()
				replacement.EncryptionKey = append(HpkePublicKey(nil), parent.EncryptionKey...)
				replacement.EncryptionKey[0] ^= 0x01
				if err := forged.SetParent(NodeIndex(x), replacement); err != nil {
					t.Fatalf("SetParent(%d): %v", x, err)
				}
				if err := forged.VerifyParentHashes(crypto); err == nil {
					t.Errorf("a tree with parent %d's encryption key moved was accepted; the sweep timed above does not decide anything about that node",
						x)
				}
				// put it back rather than re-cloning the tree, so that the loop costs sweeps and
				// not one two-thousand-node deep copy per parent. SetParent installs a copy, so
				// what is handed back here is the untouched original and not an alias of the
				// forgery.
				if err := forged.SetParent(NodeIndex(x), parent); err != nil {
					t.Fatalf("SetParent(%d) restoring: %v", x, err)
				}
				moved += 1
			}
			if moved != parents {
				t.Fatalf("%d of the %d non-blank parents were forged; the refusals above are a statement about a subset somebody chose",
					moved, parents)
			}

			if elapsed > 2*time.Second {
				t.Fatalf("validating a %d leaf tree took %s, want under 2s", width, elapsed)
			}
			t.Logf("%d leaves, %d of %d parents non-blank: Validate %s, of which VerifyParentHashes %s",
				width, parents, tree.NodeWidth()/2, elapsed, sweep)
		})
	}
}
