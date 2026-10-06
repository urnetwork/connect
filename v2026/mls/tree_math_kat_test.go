// The runner for the mlswg tree-math vector family, number 1.
//
// Why this file is on beta/message at all, stated because its absence had a reason and the
// reason was not good enough. 789 lines of production tree math landed on this branch with
// zero mlswg vector coverage, and no gate noticed: family 1 was already in
// expectedPendingFamilies, so the pending-families gate was satisfied by an entry written
// when the code was elsewhere. A family whose implementation has shipped and whose corpus is
// vendored and pinned is not pending, it is uncovered, and the two look identical to every
// gate in this tree. beta/message-p3 carries its own mls/tree_math_kat_test.go; that file
// declares LoadVectorFile at a different signature from this branch's, so the two cannot both
// compile, and landing this one makes `git merge beta/message-p3` a CONFLICT on this path
// rather than the same-content add it would otherwise be. That is the intent: the merge has
// to reconcile the two runners deliberately instead of silently keeping whichever arrived
// first.
//
// What this family is. Section 4.2.1's tree math corpus publishes, per full tree, the node
// array width and the root, plus four arrays indexed by node: left, right, parent and
// sibling, each entry a node index or null where the relation is undefined. So one case is a
// complete structural description of a tree, and there is no ciphersuite anywhere in it --
// tree math is index arithmetic and the same for every suite.
//
// That is why the shared runner machinery in vectors_runner_test.go is not used here and a
// smaller local version is. vectorRunTally partitions cases by implementedSuite, which would
// count every case of this family as skipped; aCaseAtARegisteredSuite would find no case to
// drive the verifier with; and flipEveryPublishedOctet corrupts hex strings, of which this
// corpus has none. assertComparatorRefuses fits unchanged and is used, because a refusal
// table is about a comparator and not about a suite.
//
// The NULLS are the half a comparator can most easily get wrong, so they are compared in both
// directions rather than skipped. A published null must meet a refusal from tree math and a
// published index must meet an answer; a comparator that only checked the published indices
// would accept an implementation that returned a parent for the root, which is the one answer
// that turns a bounded upward walk into an unbounded one.
package mls

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
)

// treeMathKatFile is the corpus this family reads. It is pinned by digest in VECTORS.sha256
// and gated by TestVectorFilesArePinned like the other fifteen.
const treeMathKatFile = "tree-math.json"

// The accounting that makes this runner unable to pass having compared nothing.
//
// Transcriptions of what testdata/vectors/tree-math.json holds at the pinned mlswg commit:
// ten cases, at one leaf through five hundred and twelve, whose node arrays are 1, 3, 7, 15,
// 31, 63, 127, 255, 511 and 1023 wide and therefore hold 2036 nodes between them. Two answers
// are compared per case -- the width and the root -- and four per node.
//
// Written down rather than derived, for the reason task 16 gives: deriving an expected count
// with the same reader that is under test is how a reader that matched nothing ends up
// agreeing with itself. What IS derived and checked alongside them is that the node total is
// the sum of the published widths, so a corpus that grew or lost a case fails here rather
// than quietly comparing a different number of things.
const (
	treeMathKatCases       = 10
	treeMathKatNodes       = 2036
	treeMathKatPerCase     = 2
	treeMathKatPerNode     = 4
	treeMathKatComparisons = treeMathKatCases*treeMathKatPerCase + treeMathKatNodes*treeMathKatPerNode
)

// The refusals compareTreeMathVector makes, as sentinels rather than as formatted strings, so
// a test can require a specific refusal rather than "some error".
//
// They are what makes the comparison observable at all. Every case of the vendored corpus
// agrees with this implementation, so a comparator that checked everything and one that
// checked nothing produce identical runs over it; the only way to tell them apart is to hand
// it an answer that is wrong on purpose and require the matching refusal, which is
// TestCompareTreeMathVectorRefusesAnAnswerItShouldNotAccept.
var (
	errTreeMathPublishedShape = errors.New("a tree math case does not publish four arrays as wide as its node array")
	errTreeMathLeafCount      = errors.New("a tree math case publishes a leaf count this package refuses")
	errTreeMathNodeWidth      = errors.New("NodeWidth disagrees with the published node array width")
	errTreeMathRoot           = errors.New("Root disagrees with the published root")
	errTreeMathChild          = errors.New("Left or Right disagrees with the published child")
	errTreeMathParent         = errors.New("Parent disagrees with the published parent")
	errTreeMathSibling        = errors.New("Sibling disagrees with the published sibling")
)

// Family 1 is installed here, and 1 is deleted from expectedPendingFamilies in the same
// commit. Without both halves TestVectorFamiliesVerify runs one fewer family and the manifest
// gate stays green while claiming this family is unimplemented.
//
// Generate is NOT nil, and it is not the verifier wearing a hat: generateTreeMathVectors
// builds each case by laying a tree out recursively, in-order, and reading the relations off
// the layout. It calls none of Root, Left, Right, Parent or Sibling, so a case it produces is
// an independent statement of what the tree IS, and running the verifier over it compares the
// bit arithmetic against a structural construction rather than against itself.
func init() {
	RegisterVectorFamily(VectorFamily{
		Number:   1,
		Name:     "Tree math",
		File:     treeMathKatFile,
		Slice:    "A2",
		Verify:   verifyTreeMathVector,
		Generate: generateTreeMathVectors,
	})
}

// treeMathVector is one published case.
//
// The four relation arrays are pointers so that a published null and a published zero are
// different values. They are the same value to a []uint32, and node 0 is a real node in every
// tree, so a decode that flattened null to zero would read "leaf 0 has no parent" as "leaf
// 0's parent is node 0" and compare the root's absent relation against a real one.
type treeMathVector struct {
	NLeaves uint32    `json:"n_leaves"`
	NNodes  uint32    `json:"n_nodes"`
	Root    uint32    `json:"root"`
	Left    []*uint32 `json:"left"`
	Right   []*uint32 `json:"right"`
	Parent  []*uint32 `json:"parent"`
	Sibling []*uint32 `json:"sibling"`
}

// treeMathTally is what one comparison of one case actually compared.
//
// Returned rather than counted into a package variable, so the caller decides what it means
// and a comparator that returned nil having compared nothing is visible as a zero here.
type treeMathTally struct {
	nodes       int
	comparisons int
	// answers is every published value a comparison was made against, rendered with the
	// relation that published it. A corpus read as one repeated value -- every array
	// decoding empty, every entry decoding as null -- compares the right number of times
	// against the wrong number of answers.
	answers map[string]bool
}

// verifyTreeMathVector is the registered Verify: it turns a refusal into a verdict.
func verifyTreeMathVector(t *testing.T, raw json.RawMessage) {
	t.Helper()
	if _, err := compareTreeMathVector(raw); err != nil {
		t.Fatalf("tree math vector: %v", err)
	}
}

// compareTreeMathVector holds one published case against this package's tree math.
//
// The published shape is checked before anything is compared, and that check is not a
// formality: a case whose arrays decoded to nothing would let every loop below run zero times
// and return a nil error, which is the shape a known answer test must never be able to reach.
// The array lengths are required to be the PUBLISHED node width rather than merely equal to
// each other, so a corpus read through a misspelled struct tag -- four empty arrays, all the
// same length -- is a refusal here rather than ten silent passes.
func compareTreeMathVector(raw json.RawMessage) (treeMathTally, error) {
	tally := treeMathTally{answers: map[string]bool{}}
	published := treeMathVector{}
	if err := json.Unmarshal(raw, &published); err != nil {
		return tally, fmt.Errorf("%w: %w", errTreeMathPublishedShape, err)
	}
	// the generic decode beside the struct one, so the seven keys are read twice by two
	// readers that share no struct tag. A tag pointed at a key the corpus does not publish
	// decodes to a zero value on the struct's side and is a missing key here.
	generic := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &generic); err != nil {
		return tally, fmt.Errorf("%w: %w", errTreeMathPublishedShape, err)
	}
	for _, key := range []string{"n_leaves", "n_nodes", "root", "left", "right", "parent", "sibling"} {
		if _, ok := generic[key]; !ok {
			return tally, fmt.Errorf("%w: the case does not publish %q", errTreeMathPublishedShape, key)
		}
	}
	if published.NNodes == 0 {
		return tally, fmt.Errorf("%w: the case publishes a node array of width zero", errTreeMathPublishedShape)
	}
	for _, relation := range []struct {
		name    string
		entries []*uint32
	}{
		{name: "left", entries: published.Left},
		{name: "right", entries: published.Right},
		{name: "parent", entries: published.Parent},
		{name: "sibling", entries: published.Sibling},
	} {
		if uint32(len(relation.entries)) != published.NNodes {
			return tally, fmt.Errorf("%w: %s holds %d entries and the case publishes %d nodes",
				errTreeMathPublishedShape, relation.name, len(relation.entries), published.NNodes)
		}
	}

	leaves := LeafCount(published.NLeaves)
	if !IsFullLeafCount(leaves) {
		return tally, fmt.Errorf("%w: %d", errTreeMathLeafCount, published.NLeaves)
	}
	if got := NodeWidth(leaves); got != published.NNodes {
		return tally, fmt.Errorf("%w: NodeWidth(%d) = %d, the corpus publishes %d",
			errTreeMathNodeWidth, leaves, got, published.NNodes)
	}
	tally.answers[fmt.Sprintf("n_nodes:%d", published.NNodes)] = true
	tally.comparisons++

	root, err := Root(leaves)
	if err != nil {
		return tally, fmt.Errorf("%w: Root(%d): %w", errTreeMathRoot, leaves, err)
	}
	if uint32(root) != published.Root {
		return tally, fmt.Errorf("%w: Root(%d) = %d, the corpus publishes %d",
			errTreeMathRoot, leaves, root, published.Root)
	}
	tally.answers[fmt.Sprintf("root:%d", published.Root)] = true
	tally.comparisons++

	for at := uint32(0); at < published.NNodes; at++ {
		node := NodeIndex(at)
		for _, relation := range []struct {
			name      string
			published *uint32
			answer    func() (NodeIndex, error)
			sentinel  error
		}{
			{name: "left", published: published.Left[at], sentinel: errTreeMathChild,
				answer: func() (NodeIndex, error) { return Left(node) }},
			{name: "right", published: published.Right[at], sentinel: errTreeMathChild,
				answer: func() (NodeIndex, error) { return Right(node) }},
			{name: "parent", published: published.Parent[at], sentinel: errTreeMathParent,
				answer: func() (NodeIndex, error) { return Parent(node, leaves) }},
			{name: "sibling", published: published.Sibling[at], sentinel: errTreeMathSibling,
				answer: func() (NodeIndex, error) { return Sibling(node, leaves) }},
		} {
			got, err := relation.answer()
			// both directions. A published null must meet a refusal and a published index
			// must meet an answer, because an implementation that answered where the corpus
			// publishes nothing -- a parent for the root -- is exactly as wrong as one that
			// answered the wrong index, and only one of the two is caught by comparing
			// values alone.
			if relation.published == nil {
				if err == nil {
					return tally, fmt.Errorf("%w: %s of node %d in a %d leaf tree answered %d and the corpus publishes null",
						relation.sentinel, relation.name, node, leaves, got)
				}
				tally.answers[relation.name+":null"] = true
				tally.comparisons++
				continue
			}
			if err != nil {
				return tally, fmt.Errorf("%w: %s of node %d in a %d leaf tree: %w, and the corpus publishes %d",
					relation.sentinel, relation.name, node, leaves, err, *relation.published)
			}
			if uint32(got) != *relation.published {
				return tally, fmt.Errorf("%w: %s of node %d in a %d leaf tree = %d, the corpus publishes %d",
					relation.sentinel, relation.name, node, leaves, got, *relation.published)
			}
			tally.answers[fmt.Sprintf("%s:%d", relation.name, *relation.published)] = true
			tally.comparisons++
		}
		tally.nodes++
	}
	return tally, nil
}

// TestTreeMathMatchesTheMlswgTreeMath is the known answer run over the whole corpus.
//
// The counts are what stop it passing having read a corpus it could not parse. Cases, nodes
// and comparisons are each held to a written number, and the node total is ALSO checked
// against the sum of the widths the corpus itself publishes, so the two halves have to agree
// about what was read rather than both being transcriptions.
func TestTreeMathMatchesTheMlswgTreeMath(t *testing.T) {
	entries := LoadVectorFile(t, treeMathKatFile)
	if len(entries) != treeMathKatCases {
		t.Fatalf("%s holds %d cases, this runner is written against %d", treeMathKatFile, len(entries), treeMathKatCases)
	}
	nodes, comparisons := 0, 0
	widths := 0
	answers := map[string]bool{}
	leafCounts := map[uint32]bool{}
	for index, raw := range entries {
		census := struct {
			NLeaves uint32 `json:"n_leaves"`
			NNodes  uint32 `json:"n_nodes"`
		}{}
		if err := json.Unmarshal(raw, &census); err != nil {
			t.Fatalf("case %d: %v", index, err)
		}
		widths += int(census.NNodes)
		leafCounts[census.NLeaves] = true
		tally, err := compareTreeMathVector(raw)
		if err != nil {
			t.Fatalf("case %d: %v", index, err)
		}
		if tally.nodes != int(census.NNodes) {
			t.Fatalf("case %d compared %d nodes and publishes %d", index, tally.nodes, census.NNodes)
		}
		nodes += tally.nodes
		comparisons += tally.comparisons
		for answer := range tally.answers {
			answers[answer] = true
		}
	}
	// the corpus's own census of itself, against the number this runner is written to.
	if widths != treeMathKatNodes {
		t.Fatalf("%s publishes %d nodes across its cases, this runner is written against %d",
			treeMathKatFile, widths, treeMathKatNodes)
	}
	if nodes != treeMathKatNodes {
		t.Fatalf("compared %d nodes, want %d", nodes, treeMathKatNodes)
	}
	if comparisons != treeMathKatComparisons {
		t.Fatalf("made %d comparisons, want %d", comparisons, treeMathKatComparisons)
	}
	// and the cases are ten DIFFERENT trees. A corpus read as one case ten times compares
	// the right number of times against one tree's answers.
	if len(leafCounts) != treeMathKatCases {
		t.Fatalf("the %d cases name %d distinct leaf counts", treeMathKatCases, len(leafCounts))
	}
	t.Logf("%s: %d cases, %d nodes, %d comparisons against %d distinct published answers",
		treeMathKatFile, len(entries), nodes, comparisons, len(answers))
}

// treeMathLayout lays a full tree out in the array representation by CONSTRUCTION, and
// answers the relations read off that layout.
//
// This is the independent half of the generate direction, and it is independent in the only
// way that matters: it calls none of Root, Left, Right, Parent or Sibling. A subtree of level
// k occupies 2^(k+1)-1 consecutive node indices; its root sits in the middle, its left
// subtree occupies the indices below and its right subtree the indices above. Recursing on
// that sentence produces every parent-child pair of the tree, and everything else -- the
// root, the siblings, the leaves -- falls out of the pairs.
type treeMathLayout struct {
	width   uint32
	root    uint32
	left    map[uint32]uint32
	right   map[uint32]uint32
	parent  map[uint32]uint32
	sibling map[uint32]uint32
}

// buildTreeMathLayout lays out a tree of 2^levels leaves.
func buildTreeMathLayout(levels uint32) *treeMathLayout {
	layout := &treeMathLayout{
		width:   (uint32(1) << (levels + 1)) - 1,
		left:    map[uint32]uint32{},
		right:   map[uint32]uint32{},
		parent:  map[uint32]uint32{},
		sibling: map[uint32]uint32{},
	}
	var place func(first uint32, level uint32) uint32
	place = func(first uint32, level uint32) uint32 {
		span := (uint32(1) << level) - 1
		here := first + span
		if level == 0 {
			return here
		}
		left := place(first, level-1)
		right := place(here+1, level-1)
		layout.left[here] = left
		layout.right[here] = right
		layout.parent[left] = here
		layout.parent[right] = here
		layout.sibling[left] = right
		layout.sibling[right] = left
		return here
	}
	layout.root = place(0, levels)
	return layout
}

// entry renders one relation of one node as the corpus renders it: a node index, or null
// where the relation is undefined.
func (self *treeMathLayout) entry(relation map[uint32]uint32, node uint32) *uint32 {
	value, ok := relation[node]
	if !ok {
		return nil
	}
	held := value
	return &held
}

// generateTreeMathVectors is the generate direction of section 4.2.1 for this family.
//
// It produces one case per full tree from one leaf to 2048, which is two widths beyond
// anything the vendored corpus publishes, so the loop TestVectorGenerateThenVerify closes
// covers trees the known answer run never reaches.
func generateTreeMathVectors(t *testing.T) json.RawMessage {
	t.Helper()
	cases := []treeMathVector{}
	for levels := uint32(0); levels <= 11; levels++ {
		layout := buildTreeMathLayout(levels)
		one := treeMathVector{
			NLeaves: uint32(1) << levels,
			NNodes:  layout.width,
			Root:    layout.root,
		}
		for node := uint32(0); node < layout.width; node++ {
			one.Left = append(one.Left, layout.entry(layout.left, node))
			one.Right = append(one.Right, layout.entry(layout.right, node))
			one.Parent = append(one.Parent, layout.entry(layout.parent, node))
			one.Sibling = append(one.Sibling, layout.entry(layout.sibling, node))
		}
		cases = append(cases, one)
	}
	body, err := json.Marshal(cases)
	if err != nil {
		t.Fatalf("encode the generated tree math cases: %v", err)
	}
	return body
}

// TestGeneratedTreeMathCasesAgreeWithTheMlswgCorpus is the control on the generator, and it
// is the one that says the generator is not this package's arithmetic wearing a hat.
//
// A generator that shared the code path under test closes no loop; it agrees with itself. So
// the structural layout is held against the PUBLISHED file, field by field, for every width
// the two have in common. If the recursion and mlswg's corpus agree on ten trees and 2036
// nodes, then feeding the generator's wider trees to the verifier is a real comparison of the
// bit arithmetic against something else.
func TestGeneratedTreeMathCasesAgreeWithTheMlswgCorpus(t *testing.T) {
	published := map[uint32]treeMathVector{}
	for index, raw := range LoadVectorFile(t, treeMathKatFile) {
		one := treeMathVector{}
		if err := json.Unmarshal(raw, &one); err != nil {
			t.Fatalf("case %d: %v", index, err)
		}
		published[one.NLeaves] = one
	}
	if len(published) != treeMathKatCases {
		t.Fatalf("read %d distinct published trees, want %d", len(published), treeMathKatCases)
	}
	generated := []treeMathVector{}
	if err := json.Unmarshal(generateTreeMathVectors(t), &generated); err != nil {
		t.Fatalf("parse the generated cases: %v", err)
	}
	compared, nodes := 0, 0
	for _, one := range generated {
		want, ok := published[one.NLeaves]
		if !ok {
			continue
		}
		if one.NNodes != want.NNodes || one.Root != want.Root {
			t.Fatalf("the layout of a %d leaf tree is %d nodes wide with root %d; the corpus publishes %d and %d",
				one.NLeaves, one.NNodes, one.Root, want.NNodes, want.Root)
		}
		for _, relation := range []struct {
			name      string
			built     []*uint32
			published []*uint32
		}{
			{name: "left", built: one.Left, published: want.Left},
			{name: "right", built: one.Right, published: want.Right},
			{name: "parent", built: one.Parent, published: want.Parent},
			{name: "sibling", built: one.Sibling, published: want.Sibling},
		} {
			if len(relation.built) != len(relation.published) {
				t.Fatalf("the layout of a %d leaf tree publishes %d %s entries, the corpus publishes %d",
					one.NLeaves, len(relation.built), relation.name, len(relation.published))
			}
			for at := range relation.built {
				builtEntry, publishedEntry := relation.built[at], relation.published[at]
				if (builtEntry == nil) != (publishedEntry == nil) {
					t.Fatalf("the layout and the corpus disagree about whether node %d of a %d leaf tree has a %s",
						at, one.NLeaves, relation.name)
				}
				if builtEntry != nil && *builtEntry != *publishedEntry {
					t.Fatalf("the layout says the %s of node %d of a %d leaf tree is %d, the corpus publishes %d",
						relation.name, at, one.NLeaves, *builtEntry, *publishedEntry)
				}
			}
		}
		nodes += int(one.NNodes)
		compared++
	}
	if compared != treeMathKatCases {
		t.Fatalf("the generator produced cases for %d of the %d published widths", compared, treeMathKatCases)
	}
	if nodes != treeMathKatNodes {
		t.Fatalf("compared %d nodes against the corpus, want %d", nodes, treeMathKatNodes)
	}
}

// TestCompareTreeMathVectorRefusesAnAnswerItShouldNotAccept is the control every family
// runner in this package owes and cannot be.
//
// Every case of the vendored corpus agrees with this implementation, so a comparator that
// accepted everything runs identically over it. The table below disagrees with it on purpose,
// once per defect class, and requires the matching refusal -- including the two null cases,
// which are the classes a comparator that only compared published indices would miss.
func TestCompareTreeMathVectorRefusesAnAnswerItShouldNotAccept(t *testing.T) {
	entries := LoadVectorFile(t, treeMathKatFile)
	// a case with parents in it, so the child and sibling rows below have something to
	// corrupt. The one leaf tree has neither.
	accepted := json.RawMessage(nil)
	for _, raw := range entries {
		one := treeMathVector{}
		if err := json.Unmarshal(raw, &one); err != nil {
			t.Fatalf("parse a case: %v", err)
		}
		if one.NLeaves >= 4 {
			accepted = raw
			break
		}
	}
	if accepted == nil {
		t.Fatalf("%s publishes no tree of four leaves or more, so every corruption below would be over a tree with no parent in it", treeMathKatFile)
	}

	moved := func(t *testing.T, edit func(one *treeMathVector)) json.RawMessage {
		t.Helper()
		one := treeMathVector{}
		if err := json.Unmarshal(accepted, &one); err != nil {
			t.Fatalf("parse the case to corrupt: %v", err)
		}
		edit(&one)
		body, err := json.Marshal(one)
		if err != nil {
			t.Fatalf("re-encode the corrupted case: %v", err)
		}
		return body
	}
	bump := func(entry *uint32) *uint32 {
		held := *entry + 1
		return &held
	}
	// the index of the root, and of some node that is not the root, read off the case rather
	// than written down.
	base := treeMathVector{}
	if err := json.Unmarshal(accepted, &base); err != nil {
		t.Fatalf("parse the case: %v", err)
	}
	aParent, aLeaf := -1, -1
	for at := range base.Left {
		if base.Left[at] != nil && aParent < 0 {
			aParent = at
		}
		if base.Left[at] == nil && aLeaf < 0 {
			aLeaf = at
		}
	}
	if aParent < 0 || aLeaf < 0 {
		t.Fatal("the case has no parent or no leaf in it, so half the table below would corrupt nothing")
	}

	assertComparatorRefuses(t, "tree math",
		func(t *testing.T, raw json.RawMessage) error {
			_, err := compareTreeMathVector(raw)
			return err
		},
		accepted,
		[]comparatorRefusal{
			{
				name: "the node array width moved",
				want: errTreeMathNodeWidth,
				vector: moved(t, func(one *treeMathVector) {
					one.NNodes += 2
					one.Left = append(one.Left, nil, nil)
					one.Right = append(one.Right, nil, nil)
					one.Parent = append(one.Parent, nil, nil)
					one.Sibling = append(one.Sibling, nil, nil)
				}),
			},
			{
				name:   "the root moved",
				want:   errTreeMathRoot,
				vector: moved(t, func(one *treeMathVector) { one.Root++ }),
			},
			{
				name:   "a left child moved",
				want:   errTreeMathChild,
				vector: moved(t, func(one *treeMathVector) { one.Left[aParent] = bump(one.Left[aParent]) }),
			},
			{
				name:   "a right child moved",
				want:   errTreeMathChild,
				vector: moved(t, func(one *treeMathVector) { one.Right[aParent] = bump(one.Right[aParent]) }),
			},
			{
				name:   "a leaf published as having children",
				want:   errTreeMathChild,
				vector: moved(t, func(one *treeMathVector) { one.Left[aLeaf] = bump(one.Right[aParent]) }),
			},
			{
				name:   "a parent moved",
				want:   errTreeMathParent,
				vector: moved(t, func(one *treeMathVector) { one.Parent[aLeaf] = bump(one.Parent[aLeaf]) }),
			},
			{
				name: "the root published as having a parent",
				want: errTreeMathParent,
				vector: moved(t, func(one *treeMathVector) {
					held := uint32(0)
					one.Parent[base.Root] = &held
				}),
			},
			{
				name:   "a sibling moved",
				want:   errTreeMathSibling,
				vector: moved(t, func(one *treeMathVector) { one.Sibling[aLeaf] = bump(one.Sibling[aLeaf]) }),
			},
			{
				name: "the root published as having a sibling",
				want: errTreeMathSibling,
				vector: moved(t, func(one *treeMathVector) {
					held := uint32(0)
					one.Sibling[base.Root] = &held
				}),
			},
			{
				name: "the relation arrays truncated",
				want: errTreeMathPublishedShape,
				vector: moved(t, func(one *treeMathVector) {
					one.Parent = one.Parent[:len(one.Parent)-1]
				}),
			},
			{
				name: "a leaf count no tree can have",
				want: errTreeMathLeafCount,
				vector: moved(t, func(one *treeMathVector) {
					one.NLeaves += 1
				}),
			},
		})
}

// bumpEveryPublishedNumber rewrites one corpus case so every number it publishes, at any
// depth, differs from the published one by one, and reports how many it changed.
//
// It is this family's answer to flipEveryPublishedOctet, which corrupts hex strings and
// therefore corrupts nothing in a corpus made entirely of integers. DERIVED for the same
// reason that one is: a per family hand written wrong case is a list, and the family that
// landed without one would be driven by nothing and would still read as installed. Nulls are
// left as nulls, because a null rewritten to a number is a change of SHAPE and would be
// refused by the published-shape check before any comparison happened.
func bumpEveryPublishedNumber(t *testing.T, raw json.RawMessage) (json.RawMessage, int) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var tree any
	if err := decoder.Decode(&tree); err != nil {
		t.Fatalf("parse the case to corrupt: %v", err)
	}
	bumped := 0
	var walk func(node any) any
	walk = func(node any) any {
		switch value := node.(type) {
		case map[string]any:
			for key, held := range value {
				value[key] = walk(held)
			}
			return value
		case []any:
			for index, held := range value {
				value[index] = walk(held)
			}
			return value
		case json.Number:
			held, err := value.Int64()
			if err != nil {
				return value
			}
			bumped++
			return json.Number(fmt.Sprintf("%d", held+1))
		}
		return node
	}
	body, err := json.Marshal(walk(tree))
	if err != nil {
		t.Fatalf("re-encode the corrupted case: %v", err)
	}
	return body, bumped
}

// TestTreeMathFamilyIsInstalled is the registration half this family owes.
//
// It is a local version of assertVectorFamilyIsInstalled rather than a call to it, and the
// difference is one line: the shared one drives the installed verifier with
// aCaseAtARegisteredSuite, which reads a cipher_suite this family does not publish and would
// fatal here for a reason that has nothing to do with this runner. Everything else is held to
// the same things -- the number, the file, the pending list, the identity of both installed
// functions, and the verifier DRIVEN rather than merely identified.
func TestTreeMathFamilyIsInstalled(t *testing.T) {
	family, ok := vectorManifest[1]
	if !ok {
		t.Fatal("family 1 is not in the manifest")
	}
	if family.File != treeMathKatFile {
		t.Fatalf("family 1 names %s, this runner reads %s", family.File, treeMathKatFile)
	}
	if family.Verify == nil {
		t.Fatal("family 1 has no Verify, so TestVectorFamiliesVerify runs one family fewer and says nothing about it")
	}
	if slices.Contains(expectedPendingFamilies, 1) {
		t.Fatal("family 1 is installed and expectedPendingFamilies still names it as pending")
	}
	if got := reflect.ValueOf(family.Verify).Pointer(); got != reflect.ValueOf(verifyTreeMathVector).Pointer() {
		t.Fatal("family 1 is installed with a verifier that is not this runner's")
	}
	if family.Generate == nil {
		t.Fatal("family 1 has no Generate, so the generate direction of spec A section 4.2.1 is unexercised for it")
	}
	if got := reflect.ValueOf(family.Generate).Pointer(); got != reflect.ValueOf(generateTreeMathVectors).Pointer() {
		t.Fatal("family 1 is installed with a generator that is not this runner's")
	}

	// and the installed verifier is DRIVEN. Pointer identity says the manifest holds this
	// function; it says nothing about the function doing anything. Measured elsewhere in this
	// package: three registered verifiers had their bodies replaced by a discard of the
	// argument and 411 tests still passed.
	entries := LoadVectorFile(t, treeMathKatFile)
	if len(entries) == 0 {
		t.Fatalf("%s has no cases, so nothing drives the verifier family 1 installed", treeMathKatFile)
	}
	accepted := entries[0]
	refused, bumped := bumpEveryPublishedNumber(t, accepted)
	if bumped == 0 {
		t.Fatal("family 1's case publishes no number to corrupt, so the refusal below would be over an unmodified case")
	}
	if failed, raised := probeAssertion(func(probe *testing.T) { family.Verify(probe, accepted) }); raised != nil {
		t.Fatalf("family 1's installed verifier panicked over a published case: %v", raised)
	} else if failed {
		t.Fatal("family 1's installed verifier refused a published case; a verifier that refuses everything satisfies the refusal below")
	}
	if failed, raised := probeAssertion(func(probe *testing.T) { family.Verify(probe, refused) }); raised != nil {
		t.Fatalf("family 1's installed verifier panicked over a case with all %d of its published numbers changed: %v", bumped, raised)
	} else if !failed {
		t.Fatalf("family 1's installed verifier accepted a case with all %d of its published numbers changed, so it is installed and compares nothing", bumped)
	}
}
