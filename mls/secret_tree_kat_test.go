// The runner for the mlswg secret-tree vector family, number 3.
//
// This is the fourth family to register against the registry in vectors_test.go and it
// declares none of the machinery the first three needed: the suite filter is
// implementedSuite, the accounting is vectorRunTally, the comparator control is
// assertComparatorRefuses, the registration assertion is assertVectorFamilyIsInstalled and
// the second, struct-free decode of a published answer is publishedCorpusField, all of them
// in vectors_runner_test.go. One thing was added THERE rather than here, because it is not
// this family's own: publishedCorpusField could address a key one level down, and this
// family's answers live at leaves[leaf][generation], so its walk now takes a decimal segment
// as an array index. Every other family reads the same walk.
//
// The ratchet type class is not this file's either. secret_tree_test.go already DERIVES the
// live ratchet types by sweeping the code point space and asking which ones reach a
// keystream, and names the section 9 label of each; the check names below are built from
// that, so a third ratchet type changes what this family compares rather than leaving it
// comparing two of three and reporting a clean run.
//
// What is this family's own, and why:
//
//   - the descent. crypto_labels_test.go already reads this corpus, and it reads the SINGLE
//     LEAF cases only -- there the whole tree is one node, the encryption secret is the leaf
//     secret unchanged, and no descent happens at all. It also runs them through
//     ExpandWithLabel and DeriveTreeSecret directly rather than through SecretTree. So the
//     eight and thirty-two leaf cases, which are the only published pin on which child of
//     which node a leaf's secret comes from, were vendored and uncovered. That is what this
//     runner adds and it is why it is worth having: a left/right swap in pathToLeaf is
//     invisible to every other test in this package that reads this file.
//   - EVERY path to one generation, derived. A generation is reachable by four exported
//     methods -- NextSenderKey and receiverKey, keyed on the ratchet type, and NextMessageKey
//     and MessageKey, keyed on the ContentType the framing layer carries -- and the corpus
//     answers for all of them. They are compared separately rather than one assumed to follow
//     another, because step(), keyFor() and peekFor() are different code and an asymmetry
//     between them is exactly a message this group can send and cannot read. The class is not
//     written down here: it is stMethodsAnsweringBytes in secret_tree_test.go, the exported
//     methods of *SecretTree that answer a []byte read off the compiled type, and every member
//     of it must have a driver in this file. What this replaces named two of the four and
//     called them "the two exported ways", which was false when it was written -- MessageKey
//     answers exactly that question, through peekFor rather than keyFor. Measured: a third
//     exported key source added to secret_tree.go passed every test in this file while the
//     class was that list.
//   - the aliasing refusal, over every answer one case publishes. All of them are AEAD key
//     and nonce material of apparent random, and any two holding one value would be a corpus
//     that cannot tell a handshake key from an application key, or leaf 3's from leaf 4's --
//     which is precisely what a swapped ratchet label or a wrong descent produces. The
//     vendored corpus publishes 668 answers at the two registered suites and no two of them
//     coincide, so the rule costs nothing here and is the difference on a case whose answers
//     were recomputed to match a defective derivation.
//   - the two vacuity controls: the encryption secret with one octet changed must move the
//     leaf key, and the sender data ciphertext with one octet changed must move the header
//     key. Both are about an input reaching a derivation at all, which no whole-answer
//     comparison against an agreeing corpus can see.
//
// What this runner does NOT see, stated because a runner that oversells itself is worse than
// one that is missing: the deletions. Forward secrecy is the half of section 9 no known
// answer test can observe -- a tree that kept every parent secret forever answers every
// published question identically -- so the erasure gates stay secret_tree_test.go's. That was
// measured rather than assumed, once per deletion, each paired with the test that is actually
// the one to fail: zeroizeSecret(parentSecret) deleted from takeLeafSecret passes this whole
// file and fails TestSecretTreeParentSecretIsGoneOnceBothChildrenExist, and
// zeroizeSecret(leafSecret) deleted from ratchetFor passes this whole file and fails
// TestRatchetForErasesTheLeafSecretInPlaceOnceBothRootsExist. The sentence this replaces named
// the first function with the second test: both halves were true and the join was not.
package mls

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The accounting that makes this runner unable to pass having compared nothing.
//
// Transcriptions of what testdata/vectors/secret-tree.json holds at the pinned mlswg commit:
// 21 cases, three at each of the seven published ciphersuites -- one leaf, eight leaves and
// thirty-two -- of which the two this package registers account for six and 82 leaves. Every
// leaf publishes two generations, 0 and 15, and every generation publishes four answers.
//
// Written down rather than derived, for the reason task 16 gives: deriving the expected count
// with the same filter that is under test is how a filter matching nothing ends up agreeing
// with itself. What IS derived and checked alongside them is that covered plus skipped equals
// the number of cases read, and that the per suite split is the corpus's own.
const (
	secretTreeKatFile        = "secret-tree.json"
	secretTreeKatCovered     = 6
	secretTreeKatSkipped     = 15
	secretTreeKatLeaves      = 82
	secretTreeKatGenerations = 2
	// the sender data header does not ratchet: one key and one nonce for the whole case.
	secretTreeKatSenderDataChecks = 2
	// twelve per published generation: two ratchet types, a key and a nonce each, reached once
	// through each of the three exported paths to a leaf's key material. Both factors are
	// DERIVED and this transcription is held to them by
	// TestSecretTreeFamilyChecksAreEveryPathToAGeneration.
	//
	// IT WAS SIXTEEN OVER FOUR PATHS UNTIL 2026-09-17, and the fourth was ReceiverKey. MASTER
	// section 8.4.7 (3) ruled that door unexported, and this package's own stub gate then put the
	// declaration in test source -- so it is not an exported path to a leaf's key material any
	// more and this family does not compare it. What is NOT lost is the receive side's
	// derivation: MessageKey is the same ratchet walk's peek half and is still compared against
	// mlswg's published answers at every generation. See secret_tree_test.go's receiverKey block
	// for the move and what else it costs.
	secretTreeKatChecksPerGeneration = 12
	secretTreeKatComparisons         = secretTreeKatCovered*secretTreeKatSenderDataChecks +
		secretTreeKatLeaves*secretTreeKatGenerations*secretTreeKatChecksPerGeneration
	// the distinct published answers those comparisons are made against: each leaf answer is
	// compared once per path, and each sender data answer once.
	secretTreeKatDistinct = secretTreeKatCovered*secretTreeKatSenderDataChecks +
		secretTreeKatLeaves*secretTreeKatGenerations*(secretTreeKatChecksPerGeneration/secretTreeKatKeyPaths)
	// the paths a generation is reachable by, which is the factor above that is about this
	// package's own surface rather than about the corpus. Written down for the reason the
	// counts above are, and held to the class stMethodsAnsweringBytes derives.
	secretTreeKatKeyPaths = 3
	// a key and a nonce.
	secretTreeKatAnswersPerRatchet = 2
)

// secretTreeVector is one entry of secret-tree.json.
//
// A second struct over this file, and the first is labelKatSecretTree in crypto_labels_test.go.
// Two shapes for one row in one package is worse than one, and the alternative here is worse
// still: that one belongs to a task whose subject is the KDF labels and reads the single leaf
// cases only, and collapsing them would put family 3's registry-facing decode inside a file the
// registry knows nothing about.
type secretTreeVector struct {
	CipherSuite      uint16                   `json:"cipher_suite"`
	EncryptionSecret string                   `json:"encryption_secret"`
	SenderData       secretTreeSenderData     `json:"sender_data"`
	Leaves           [][]secretTreeGeneration `json:"leaves"`
}

// secretTreeSenderData is the RFC 9420 section 6.3.2 header material one case publishes.
//
// A named type rather than the anonymous struct the plan writes, because theJsonKeyOf reads a
// field's json key off its own tag by reflection and cannot be pointed at a field of an
// anonymous struct nested inside another one.
type secretTreeSenderData struct {
	SenderDataSecret string `json:"sender_data_secret"`
	Ciphertext       string `json:"ciphertext"`
	Key              string `json:"key"`
	Nonce            string `json:"nonce"`
}

// secretTreeGeneration is one published generation of one leaf: both ratchets' key and nonce.
type secretTreeGeneration struct {
	Generation       uint32 `json:"generation"`
	HandshakeKey     string `json:"handshake_key"`
	HandshakeNonce   string `json:"handshake_nonce"`
	ApplicationKey   string `json:"application_key"`
	ApplicationNonce string `json:"application_nonce"`
}

// Family 3 is installed here, and 3 is deleted from expectedPendingFamilies in the same commit.
// Without both halves TestVectorFamiliesVerify runs one fewer family and the manifest gate stays
// green while claiming this family is unimplemented.
func init() {
	RegisterVectorFamily(VectorFamily{
		Number:   3,
		Name:     "Secret tree",
		File:     secretTreeKatFile,
		Slice:    "A3",
		Verify:   verifySecretTreeVector,
		Generate: generateSecretTreeVector,
	})
}

// The refusals this family makes. Sentinels rather than formatted strings, so a control can
// require one refusal specifically: a refusal for the wrong reason is a comparator checking
// something other than what the case corrupts.
var (
	errSecretTreePublishedWidth  = errors.New("a published secret tree answer is not the suite's AEAD width")
	errSecretTreeAliased         = errors.New("two answers one secret-tree case publishes hold the same value")
	errSecretTreeGenerationOrder = errors.New("the generations one leaf publishes are not strictly ascending")
	errSecretTreeMismatch        = errors.New("a secret tree key or nonce does not match the published one")
	errSecretTreeDidNotMove      = errors.New("perturbing one of the corpus's own inputs left the answer unchanged")
	errSecretTreeIncomplete      = errors.New("the comparison reports values it cannot have computed")
	errSecretTreeRetained        = errors.New("a driven tree is not holding the skipped keys the corpus's own gaps come to")
)

// secretTreeCheck is one answer this package computed held against one answer the corpus
// published, named by the path that produced the computed half and filed under the dotted json
// path the published half lives at.
type secretTreeCheck struct {
	// name is what produced the computed half, and for a leaf answer it is one of the names
	// secretTreeCheckNamesFor derives.
	name string
	// field is the dotted json path the published half lives at, which the runner re-reads out
	// of a generic decode of the same case.
	field string
	// leaf is the leaf this answer belongs to, or secretTreeSenderDataLeaf for the two answers
	// of the sender data header, which belong to no leaf.
	leaf       int
	generation uint32
	// width is the AEAD width this answer must be, read off the provider: Nk for a key and Nn
	// for a nonce. An answer of some other width is not the comparison the corpus intends.
	width int
	got   []byte
	want  []byte
}

// secretTreeSenderDataLeaf is the leaf a sender data answer is filed under. The header is
// derived from the epoch's sender_data_secret and the ciphertext it protects and belongs to no
// leaf at all, so it takes a value no leaf index can hold rather than leaf 0, which would make
// the two indistinguishable to answerAt.
const secretTreeSenderDataLeaf = -1

// secretTreeSenderDataCheckNames is the two answers of the sender data header, in the order the
// comparator emits them.
var secretTreeSenderDataCheckNames = []string{
	"SenderDataKeyNonce/key",
	"SenderDataKeyNonce/nonce",
}

// secretTreeKeyPathDriver is how this family asks ONE exported key source for the answers a leaf
// publishes, over a tree of that path's own.
type secretTreeKeyPathDriver struct {
	// collect answers this leaf's key and nonce at every generation the case publishes, keyed
	// on the generation number. The published generations are handed in rather than a range
	// because the receiver side paths must be walked in the ascending order the case publishes
	// them in -- a corpus that published them descending would otherwise be reported as this
	// implementation refusing a replay.
	collect func(t *testing.T, tree *SecretTree, leaf LeafIndex, kind RatchetType,
		published []secretTreeGeneration) (map[uint32]secretTreeAnswer, error)
	// retainsSkipped is whether walking this path leaves the generations it stepped past in the
	// tree's retained windows. The sender paths reach step() directly and retain nothing; the
	// receiver side paths reach commitFor and retain every gap. It is what turns the retained key
	// headroom from a sentence in a comment into a number this runner measures.
	retainsSkipped bool
}

// secretTreeKeyPathDrivers is one driver per exported key source, keyed on the method name the
// derived class hands back.
//
// A map keyed on the derived name rather than a list of names, so the two cannot drift:
// secretTreeKeyPaths is fatal on a member of the class with no driver here. The list this
// replaces held NextSenderKey and receiverKey and called them the only two ways to ask; there
// were four, and adding a fifth changed nothing about what this family compared.
var secretTreeKeyPathDrivers = map[string]secretTreeKeyPathDriver{
	"NextSenderKey": {
		collect: func(t *testing.T, tree *SecretTree, leaf LeafIndex, kind RatchetType,
			published []secretTreeGeneration) (map[uint32]secretTreeAnswer, error) {
			return secretTreeWalkTheSenderRatchet(t, published, func() (uint32, []byte, []byte, error) {
				return tree.NextSenderKey(leaf, kind)
			})
		},
	},
	"NextMessageKey": {
		collect: func(t *testing.T, tree *SecretTree, leaf LeafIndex, kind RatchetType,
			published []secretTreeGeneration) (map[uint32]secretTreeAnswer, error) {
			contentType := secretTreeContentTypeFor(t, kind)
			return secretTreeWalkTheSenderRatchet(t, published, func() (uint32, []byte, []byte, error) {
				key, aeadNonce, generation, err := tree.NextMessageKey(contentType, leaf)
				return generation, key, aeadNonce, err
			})
		},
	},
	"MessageKey": {
		retainsSkipped: true,
		collect: func(t *testing.T, tree *SecretTree, leaf LeafIndex, kind RatchetType,
			published []secretTreeGeneration) (map[uint32]secretTreeAnswer, error) {
			contentType := secretTreeContentTypeFor(t, kind)
			collected := map[uint32]secretTreeAnswer{}
			for _, want := range published {
				key, aeadNonce, err := tree.MessageKey(contentType, leaf, want.Generation)
				if err != nil {
					return nil, fmt.Errorf("generation %d: %w", want.Generation, err)
				}
				collected[want.Generation] = secretTreeAnswer{
					key: bytes.Clone(key), aeadNonce: bytes.Clone(aeadNonce)}
				// the pair this path comes on is "look up, open, AUTHENTICATE, commit". The
				// lookup does not consume and does not move the ratchet at all, so a walk that
				// never committed would stand at a head of zero and a retention of zero -- and
				// the commit is what turns a repeatable lookup back into a single use key AND
				// what retains the gaps this runner counts.
				if err := tree.CommitMessageKey(contentType, leaf, want.Generation); err != nil {
					return nil, fmt.Errorf("committing generation %d: %w", want.Generation, err)
				}
			}
			return collected, nil
		},
	},
}

// secretTreeWalkTheSenderRatchet drives a sender path from the start of the epoch to the highest
// generation the case publishes, keeping the ones it publishes.
//
// From generation 0 rather than by lookup, because neither sender path can be asked for one
// generation twice -- and the walk holds the generation NUMBERS handed out to 0, 1, 2, ..., the
// half a lookup by number cannot see.
func secretTreeWalkTheSenderRatchet(t *testing.T, published []secretTreeGeneration,
	next func() (uint32, []byte, []byte, error)) (map[uint32]secretTreeAnswer, error) {
	t.Helper()
	wanted := map[uint32]bool{}
	highest := uint32(0)
	for _, generation := range published {
		wanted[generation.Generation] = true
		highest = generation.Generation
	}
	collected := map[uint32]secretTreeAnswer{}
	for want := uint32(0); ; want++ {
		generation, key, aeadNonce, err := next()
		if err != nil {
			return nil, fmt.Errorf("at generation %d: %w", want, err)
		}
		if generation != want {
			return nil, fmt.Errorf("%w: the sender path answered generation %d where the epoch stands at %d",
				errSecretTreeGenerationOrder, generation, want)
		}
		if wanted[generation] {
			collected[generation] = secretTreeAnswer{
				key: bytes.Clone(key), aeadNonce: bytes.Clone(aeadNonce)}
		}
		if want == highest {
			return collected, nil
		}
	}
}

// secretTreeContentTypeFor is a content type that reaches one ratchet type, taken from the RFC's
// own mapping rather than from the implementation's.
//
// stRatchetTypeOfContentType is the section 9.1 table written down in secret_tree_test.go
// independently of ratchetTypeOf, and reading it here is what keeps the two ContentType keyed
// paths from being a derivation that agrees with itself: a table taken from ratchetTypeOf would
// route the question and the answer through one swapped mapping and compare it against itself.
func secretTreeContentTypeFor(t *testing.T, kind RatchetType) ContentType {
	t.Helper()
	for _, contentType := range slices.Sorted(maps.Keys(stRatchetTypeOfContentType)) {
		if stRatchetTypeOfContentType[contentType] == kind {
			return contentType
		}
	}
	t.Fatalf("ratchet type %d reaches a keystream and RFC 9420 section 9.1 routes no content type to it, so this family cannot ask the ContentType keyed paths about it",
		kind)
	return 0
}

// secretTreeKeyPaths is every exported way this package answers "what are the key and nonce of
// generation g of leaf l", DERIVED off the compiled method set and named SecretTree.Method, in
// the order the comparator emits them.
//
// The class is stMethodsAnsweringBytes in secret_tree_test.go -- every exported method of
// *SecretTree whose results include a []byte, read off the type rather than typed out -- and
// this function's whole job is to hold this family's drivers to it. A member with no driver is
// fatal and not skipped: a path nothing drives is a path to every published generation that goes
// uncompared while the run reports a count that looks complete.
func secretTreeKeyPaths(t *testing.T) []string {
	t.Helper()
	name := reflect.TypeOf(&SecretTree{}).Elem().Name()
	paths := []string{}
	for _, method := range stMethodsAnsweringBytes(t) {
		if _, driven := secretTreeKeyPathDrivers[method]; !driven {
			t.Fatalf("%s.%s answers key material and this family has no driver for it, so one exported path to every published generation is compared by nothing while the run counts %d checks per generation",
				name, method, secretTreeKatChecksPerGeneration)
		}
		paths = append(paths, name+"."+method)
	}
	return paths
}

// secretTreeDriverFor is the driver of one derived path, fatal on a path with none.
func secretTreeDriverFor(t *testing.T, path string) secretTreeKeyPathDriver {
	t.Helper()
	method, isMethod := strings.CutPrefix(path, reflect.TypeOf(&SecretTree{}).Elem().Name()+".")
	if !isMethod {
		t.Fatalf("the key path %q is not named for a method of SecretTree", path)
	}
	driver, driven := secretTreeKeyPathDrivers[method]
	if !driven {
		t.Fatalf("this family has no driver for %s", path)
	}
	return driver
}

// secretTreeRetainedKeys is how many skipped generation keys one tree is holding, summed over
// every ratchet it has built.
//
// It reads the windows directly, because that is the only way an eviction can be seen at all:
// pruneRetained zeroizes what it drops and reports nothing, so a tree that has been pruned
// answers every question this family asks exactly as one that has not.
func secretTreeRetainedKeys(tree *SecretTree) int {
	retained := 0
	for _, r := range tree.ratchets {
		retained += len(r.window)
	}
	return retained
}

// secretTreeCheckNamesFor is every answer this runner compares for one published generation of
// one leaf, named by the path that produced it, in the order the comparator emits them.
//
// BOTH factors are derived and neither is written out, for guardrail 5's reason. The ratchet
// type class is stRatchetKinds, which sweeps the ratchet type code point space and keeps the
// ones that actually reach a keystream, and the section 9 label of each is stRatchetLabelOf; the
// path class is secretTreeKeyPaths above. A third ratchet type or a third key source added to
// secret_tree.go therefore changes this list, and the written down
// secretTreeKatChecksPerGeneration then disagrees with it, which is
// TestSecretTreeFamilyChecksAreEveryPathToAGeneration failing rather than this family quietly
// comparing two ratchets of three, or two paths of five.
func secretTreeCheckNamesFor(t *testing.T, kinds []RatchetType, paths []string) []string {
	t.Helper()
	names := []string{}
	for _, path := range paths {
		for _, kind := range kinds {
			label := stRatchetLabel(t, kind)
			names = append(names, path+"/"+label+"/key", path+"/"+label+"/nonce")
		}
	}
	return names
}

// secretTreeComparison is what one run of compareSecretTreeVector PRODUCED, and it is the only
// thing its callers are allowed to judge it by.
//
// The shape is task 16's and it is here for task 17's reason, which is the one this project has
// paid for twice: a comparator that returns nothing lets a runner count CALLS, and a call that
// returned is not a comparison that happened. Every field below is written at the point the
// work that produces it happens, so a return that skipped the work reports the zero value, and
// a caller that judges the values rather than the fact of returning sees that.
type secretTreeComparison struct {
	// inScope is true when the case's ciphersuite is one this package registers. A false here
	// is not a failure and not a skip: it is a case with no provider.
	inScope bool
	// the widths read off the provider rather than assumed. hashSize is KDF.Nh, which is also
	// the sender data sample boundary; keySize and nonceSize are AEAD.Nk and AEAD.Nn.
	hashSize  int
	keySize   int
	nonceSize int
	// leaves is how many leaves the case publishes, which is the tree's leaf count, and
	// generations how many published generations there are across all of them.
	leaves      int
	generations int
	// publishesEncryptionSecret is whether the case carries the key at all, read off a generic
	// decode with no struct tag in the way. An absent key and an empty value decode identically
	// through the struct and only one of the two is a tree seeded from a value nobody gave.
	publishesEncryptionSecret bool
	// sample is how many octets of the published ciphertext the sender data derivation takes. A
	// zero sample is a header key that does not depend on the message it protects, which is one
	// key and nonce pair for every message of the epoch.
	sample int
	// names is the per generation check name class this run was made against, so a run made
	// against a shorter class than this family asserts is visible.
	names []string
	// checks is every comparison the run made, in the order it made them.
	checks []secretTreeCheck
	// withoutEncryptionSecret is one published answer of leaf 0 recomputed from a tree seeded
	// with one octet of the encryption secret changed, and withoutSample the sender data key
	// recomputed with one octet of the ciphertext changed. Either one equal to the unperturbed
	// answer means that input never reached the derivation.
	withoutEncryptionSecret []byte
	withoutSample           []byte
	// controlName, controlLeaf and controlGeneration address the answer withoutEncryptionSecret
	// is about, so verdict reads it by name rather than by position.
	controlName       string
	controlLeaf       int
	controlGeneration uint32
}

// answerAt is the computed half of one of this case's comparisons, addressed by the name of what
// produced it together with the leaf and generation it belongs to, or nothing if this run made
// no such comparison.
//
// By name because the caller is ABOUT a particular answer. A positional read is only that answer
// while the emit order holds, and holding the order in one place and reading positions in
// another is how the two come apart without either one failing.
func (self secretTreeComparison) answerAt(name string, leaf int, generation uint32) []byte {
	for _, check := range self.checks {
		if check.name == name && check.leaf == leaf && check.generation == generation {
			return check.got
		}
	}
	return nil
}

// incomplete reports whether the evidence a compared case must carry is missing or inconsistent,
// without looking at whether any answer was right.
//
// This is the vacuity half, split from the correctness half on purpose. bytes.Equal over two
// empty slices says they agree, so a check whose got or want is empty has compared nothing
// whatever the comparison would say about it -- and a runner that counted such checks would
// report all 1324 having derived none of them.
func (self secretTreeComparison) incomplete() error {
	switch {
	case !self.inScope:
		return fmt.Errorf("%w: the case is out of scope and carries no comparison", errSecretTreeIncomplete)
	case self.hashSize == 0 || self.keySize == 0 || self.nonceSize == 0:
		return fmt.Errorf("%w: the provider answered %d, %d and %d for KDF.Nh, AEAD.Nk and AEAD.Nn",
			errSecretTreeIncomplete, self.hashSize, self.keySize, self.nonceSize)
	case self.leaves == 0:
		return fmt.Errorf("%w: the case publishes no leaf, so there is no ratchet to compare", errSecretTreeIncomplete)
	case self.generations == 0:
		return fmt.Errorf("%w: the case's %d leaves publish no generation between them",
			errSecretTreeIncomplete, self.leaves)
	case !self.publishesEncryptionSecret:
		return fmt.Errorf("%w: the case publishes no encryption_secret key at all, so whatever decodes it decodes to nothing and the tree was seeded from a value the corpus never gave",
			errSecretTreeIncomplete)
	case self.sample == 0:
		return fmt.Errorf("%w: the sender data key was derived over a zero octet ciphertext sample, which is one key and nonce pair for every message of the epoch",
			errSecretTreeIncomplete)
	case len(self.names) != secretTreeKatChecksPerGeneration:
		return fmt.Errorf("%w: the run compared %d answers per generation and this family owes %d",
			errSecretTreeIncomplete, len(self.names), secretTreeKatChecksPerGeneration)
	case len(self.withoutEncryptionSecret) != self.keySize:
		return fmt.Errorf("%w: the perturbed encryption_secret control was never run", errSecretTreeIncomplete)
	case len(self.withoutSample) != self.keySize:
		return fmt.Errorf("%w: the perturbed ciphertext control was never run", errSecretTreeIncomplete)
	case self.controlName == "":
		return fmt.Errorf("%w: the perturbed encryption_secret control names no answer, so nothing says which comparison it is about",
			errSecretTreeIncomplete)
	}
	if want := secretTreeKatSenderDataChecks + self.generations*len(self.names); len(self.checks) != want {
		return fmt.Errorf("%w: the run made %d comparisons over %d leaves and %d published generations, and this family owes %d",
			errSecretTreeIncomplete, len(self.checks), self.leaves, self.generations, want)
	}
	// the emit order, position by position. A multiset alone permits a reorder, and a reorder is
	// what points the vacuity control at the wrong answer permanently -- the shape
	// transcript-hashes measured, where two swapped rows left every name present exactly once
	// and the whole package green.
	for index, check := range self.checks {
		expected := ""
		if index < secretTreeKatSenderDataChecks {
			expected = secretTreeSenderDataCheckNames[index]
			if check.leaf != secretTreeSenderDataLeaf {
				return fmt.Errorf("%w: comparison %d is the sender data header and is filed under leaf %d",
					errSecretTreeIncomplete, index, check.leaf)
			}
		} else {
			expected = self.names[(index-secretTreeKatSenderDataChecks)%len(self.names)]
			if check.leaf < 0 || check.leaf >= self.leaves {
				return fmt.Errorf("%w: comparison %d is filed under leaf %d of a tree with %d leaves",
					errSecretTreeIncomplete, index, check.leaf, self.leaves)
			}
		}
		if check.name != expected {
			return fmt.Errorf("%w: comparison %d is %s and this family emits %s there; the vacuity control is read out of that order",
				errSecretTreeIncomplete, index, check.name, expected)
		}
		if len(check.got) == 0 || len(check.want) == 0 {
			return fmt.Errorf("%w: %s at leaf %d generation %d compared %d computed octets against %d published ones, and an empty comparison agrees with anything",
				errSecretTreeIncomplete, check.name, check.leaf, check.generation, len(check.got), len(check.want))
		}
		if check.field == "" {
			return fmt.Errorf("%w: %s at leaf %d generation %d names no published field, so nothing independent of the comparator's own decode can re-read it",
				errSecretTreeIncomplete, check.name, check.leaf, check.generation)
		}
		if check.width == 0 {
			return fmt.Errorf("%w: %s at leaf %d generation %d names no width, so nothing holds the published answer to the suite's AEAD parameters",
				errSecretTreeIncomplete, check.name, check.leaf, check.generation)
		}
	}
	return nil
}

// verdict is the whole judgement over one compared case: it must be complete, every published
// answer must be the suite's AEAD width, no two answers of one case may hold the same value,
// every comparison must agree, and both vacuity controls must have moved.
//
// The order is deliberate. A width failure and an aliasing failure are both statements that this
// is not the comparison the corpus intends, and reporting either as a plain mismatch would let a
// test asking for one of them be satisfied by the other.
func (self secretTreeComparison) verdict() error {
	if err := self.incomplete(); err != nil {
		return err
	}
	for _, check := range self.checks {
		if len(check.want) != check.width {
			return fmt.Errorf("%w: %s is %d octets against a width of %d, so this is not the comparison the corpus intends",
				errSecretTreePublishedWidth, check.field, len(check.want), check.width)
		}
	}
	// every answer one case publishes is AEAD key or nonce material of apparent random, and two
	// of them holding one value would be a corpus this comparison cannot tell a handshake key
	// from an application key with -- or leaf 3's key from leaf 4's, which is the wrong descent
	// this family exists to see. Keyed on the published FIELD, because each leaf answer is
	// compared twice and one field carrying one value twice is this runner's own doing rather
	// than the corpus's.
	distinct := map[string]string{}
	for _, check := range self.checks {
		text := HexOf(check.want)
		if previous, duplicated := distinct[text]; duplicated && previous != check.field {
			return fmt.Errorf("%w: %s is published for both %s and %s",
				errSecretTreeAliased, text, previous, check.field)
		}
		distinct[text] = check.field
	}
	for _, check := range self.checks {
		if !bytes.Equal(check.got, check.want) {
			return fmt.Errorf("%w: %s at leaf %d generation %d = %s, the corpus publishes %s for %s",
				errSecretTreeMismatch, check.name, check.leaf, check.generation,
				HexOf(check.got), HexOf(check.want), check.field)
		}
	}
	// the two vacuity controls. Both are cheap redundancy over the comparisons above for a
	// corpus that agrees, and both are the difference on a case whose published answers were
	// recomputed to match a defective derivation: a tree that ignored its encryption secret, or
	// a sender data derivation that ignored the ciphertext, answers the same value whatever
	// those inputs hold.
	baseline := self.answerAt(self.controlName, self.controlLeaf, self.controlGeneration)
	if len(baseline) == 0 {
		return fmt.Errorf("%w: the perturbed encryption_secret control is about %s at leaf %d generation %d and the run made no such comparison",
			errSecretTreeIncomplete, self.controlName, self.controlLeaf, self.controlGeneration)
	}
	if bytes.Equal(self.withoutEncryptionSecret, baseline) {
		return fmt.Errorf("%w: one changed octet of encryption_secret left %s at leaf %d generation %d at %s, so the epoch's secret never reached the leaf key",
			errSecretTreeDidNotMove, self.controlName, self.controlLeaf, self.controlGeneration, HexOf(baseline))
	}
	headerKey := self.answerAt(secretTreeSenderDataCheckNames[0], secretTreeSenderDataLeaf, 0)
	if len(headerKey) == 0 {
		return fmt.Errorf("%w: the perturbed ciphertext control is about %s and the run made no such comparison",
			errSecretTreeIncomplete, secretTreeSenderDataCheckNames[0])
	}
	if bytes.Equal(self.withoutSample, headerKey) {
		return fmt.Errorf("%w: one changed octet of the sender data ciphertext left the header key at %s, so the sample never reached the derivation",
			errSecretTreeDidNotMove, HexOf(headerKey))
	}
	return nil
}

// verifySecretTreeVector is the registry's shim: the signature RegisterVectorFamily needs, over
// the comparator that does the work and reports what it produced.
//
// The split is the whole point, and it is the defect tasks 16 and 17 shipped one after the
// other. Verify cannot return anything, so a runner counting calls to it would count a case it
// declined to check exactly as it counts one it compared.
func verifySecretTreeVector(t *testing.T, raw json.RawMessage) {
	t.Helper()
	evidence, err := compareSecretTreeVector(t, raw)
	if err != nil {
		t.Fatalf("secret-tree: %v", err)
	}
	if !evidence.inScope {
		return
	}
	if err := evidence.verdict(); err != nil {
		t.Fatalf("secret-tree: %v", err)
	}
}

// secretTreePublishedAnswer is the published key and nonce of one ratchet type at one published
// generation, together with the json keys they live under.
//
// The json keys are read off the struct's own tags rather than spelled a second time: two
// spellings of one key in one package is how the two end up disagreeing about which key the
// corpus actually uses, and the disagreement is silent in the worst direction.
func secretTreePublishedAnswer(t *testing.T, published secretTreeGeneration, kind RatchetType) (
	key string, aeadNonce string, keyField string, nonceField string) {
	t.Helper()
	switch kind {
	case RatchetHandshake:
		return published.HandshakeKey, published.HandshakeNonce,
			theJsonKeyOf(t, secretTreeGeneration{}, "HandshakeKey"),
			theJsonKeyOf(t, secretTreeGeneration{}, "HandshakeNonce")
	case RatchetApplication:
		return published.ApplicationKey, published.ApplicationNonce,
			theJsonKeyOf(t, secretTreeGeneration{}, "ApplicationKey"),
			theJsonKeyOf(t, secretTreeGeneration{}, "ApplicationNonce")
	}
	// a ratchet type this corpus publishes no answer for. Fatal rather than skipped: the check
	// name class is derived from the LIVE ratchet types, so reaching here means this family is
	// being asked to compare an answer the published format does not carry, and comparing one
	// fewer answer per generation is exactly what the derived class exists to prevent.
	t.Fatalf("ratchet type %d reaches a keystream and secret-tree.json publishes no answer for it", kind)
	return "", "", "", ""
}

// secretTreeAnswer is one generation's key and nonce as this runner collected them, cloned out
// of whatever slice the implementation handed back.
type secretTreeAnswer struct {
	key       []byte
	aeadNonce []byte
}

// compareSecretTreeVector runs one case of secret-tree.json and returns what the run produced. A
// case at a ciphersuite v1 does not implement is not a failure and not a skip: it comes back
// with inScope false and nothing else set.
//
// A corpus that will not parse or will not hex decode is fatal here rather than returned,
// because it is not a verdict about this implementation -- it is the evidence itself being
// unreadable, and every family in this package treats that as the loudest failure there is.
// Everything that IS a verdict about this implementation is returned, so a caller can require a
// refusal instead of hoping the corpus disagrees with a defect.
//
// Two trees, and the reason is the surface rather than the corpus. NextSenderKey advances the
// leaf's ratchet past the generation it hands out and there is no way to ask for one twice, so a
// tree driven from the sender side cannot then be asked the same question from the receiver
// side. receiverKey consumes as well, which is why its generations are walked in the ascending
// order the case publishes them in -- an order this comparator REQUIRES rather than sorts into,
// because a corpus that published them descending would otherwise be reported as this
// implementation refusing a replay.
//
// One tree per PATH, because each of the four consumes: the sender paths cannot be asked for one
// generation twice and the receiver side paths consume or erase what they answer, so a shared
// tree would make the second path to arrive a replay rather than a second opinion. Within a path
// one tree serves both ratchet types rather than one per type, because that is the shape a real
// epoch has: ratchetFor takes the leaf secret ONCE and expands both roots out of it, and a
// per-type tree would take it twice and never exercise that.
//
// The retained key bound is not reached by either receiver side tree, and that is MEASURED here
// rather than asserted in this comment. Both of them step past every generation the corpus skips
// and keep it, so what a case retains is the case's own gaps: the largest published case is 32
// leaves reaching generation 15, which is 32 x 2 ratchets x 14 skipped generations = 896 against
// MaxRetainedWindowKeys of 1024, or 12.5% of headroom. pruneRetained evicts SILENTLY, so a
// corpus update that raised the published generation numbers, or a fall in RatchetWindowSize,
// would start dropping retained key material with nothing to say about it. The arithmetic is
// therefore checked against the bound before any tree is driven, and what each tree actually
// retained is checked against those same gaps once they have been.
func compareSecretTreeVector(t *testing.T, raw json.RawMessage) (secretTreeComparison, error) {
	t.Helper()
	vector := secretTreeVector{}
	if err := json.Unmarshal(raw, &vector); err != nil {
		t.Fatalf("parse secret-tree case: %v", err)
	}
	suite, ok := implementedSuite(vector.CipherSuite)
	if !ok {
		return secretTreeComparison{}, nil
	}
	crypto, err := NewCryptoProvider(suite)
	if err != nil {
		t.Fatalf("NewCryptoProvider(%#04x): %v", uint16(suite), err)
	}

	// whether the encryption secret was PUBLISHED, read off a generic decode of the same bytes
	// with no struct tag in the way, and under the key the struct tag itself names rather than a
	// second spelling of it.
	published := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &published); err != nil {
		t.Fatalf("parse secret-tree case as a json object: %v", err)
	}
	_, publishesEncryptionSecret := published[theJsonKeyOf(t, secretTreeVector{}, "EncryptionSecret")]

	kinds := stRatchetKinds(t)
	paths := secretTreeKeyPaths(t)
	evidence := secretTreeComparison{
		inScope:                   true,
		hashSize:                  crypto.HashSize(),
		keySize:                   crypto.KeySize(),
		nonceSize:                 crypto.NonceSize(),
		leaves:                    len(vector.Leaves),
		publishesEncryptionSecret: publishesEncryptionSecret,
		names:                     secretTreeCheckNamesFor(t, kinds, paths),
	}
	if evidence.leaves == 0 {
		return evidence, fmt.Errorf("%w: the case publishes no leaf, so there is no ratchet to compare",
			errSecretTreeIncomplete)
	}
	// ahead of NewSecretTree deliberately, and the precedence is the point rather than a
	// convenience. An absent encryption_secret KEY decodes through the struct to the empty
	// string, so the constructor refuses it as a secret of the wrong length -- which reports a
	// renamed struct tag as this implementation rejecting a zero length secret, a different
	// statement about a different thing. incomplete() carries the same rule for a caller that
	// judges the evidence rather than the error.
	if !evidence.publishesEncryptionSecret {
		return evidence, fmt.Errorf("%w: the case publishes no encryption_secret key at all, so whatever decodes it decodes to nothing and the tree would be seeded from a value the corpus never gave",
			errSecretTreeIncomplete)
	}
	for leaf, generations := range vector.Leaves {
		if len(generations) == 0 {
			return evidence, fmt.Errorf("%w: leaf %d publishes no generation", errSecretTreeIncomplete, leaf)
		}
		for index := 1; index < len(generations); index++ {
			if generations[index].Generation <= generations[index-1].Generation {
				return evidence, fmt.Errorf("%w: leaf %d publishes generation %d after generation %d",
					errSecretTreeGenerationOrder, leaf,
					generations[index].Generation, generations[index-1].Generation)
			}
		}
		evidence.generations += len(generations)
	}

	// the sender data header first, because it is the one derivation of section 9 with no tree in
	// it and its two answers are the ones the runner emits first.
	senderDataSecret := MustHex(t, vector.SenderData.SenderDataSecret)
	ciphertext := MustHex(t, vector.SenderData.Ciphertext)
	evidence.sample = len(ciphertext)
	if evidence.sample > evidence.hashSize {
		evidence.sample = evidence.hashSize
	}
	senderDataField := theJsonKeyOf(t, secretTreeVector{}, "SenderData")
	headerKey, headerNonce, err := SenderDataKeyNonce(crypto, senderDataSecret, ciphertext)
	if err != nil {
		return evidence, fmt.Errorf("SenderDataKeyNonce: %w", err)
	}
	evidence.checks = append(evidence.checks,
		secretTreeCheck{
			name:       secretTreeSenderDataCheckNames[0],
			field:      senderDataField + "." + theJsonKeyOf(t, secretTreeSenderData{}, "Key"),
			leaf:       secretTreeSenderDataLeaf,
			generation: 0,
			width:      evidence.keySize,
			got:        bytes.Clone(headerKey),
			want:       MustHex(t, vector.SenderData.Key),
		},
		secretTreeCheck{
			name:       secretTreeSenderDataCheckNames[1],
			field:      senderDataField + "." + theJsonKeyOf(t, secretTreeSenderData{}, "Nonce"),
			leaf:       secretTreeSenderDataLeaf,
			generation: 0,
			width:      evidence.nonceSize,
			got:        bytes.Clone(headerNonce),
			want:       MustHex(t, vector.SenderData.Nonce),
		})

	encryptionSecret := MustHex(t, vector.EncryptionSecret)
	leafCount := LeafCount(evidence.leaves)
	// the retained key headroom this case needs, from the corpus and BEFORE a tree is driven.
	// Both receiver side paths step past every generation the case skips and retain it, and
	// pruneRetained evicts without reporting, so a corpus whose gaps outgrew the tree wide bound
	// would quietly be compared against key material that had been zeroized.
	skipped := 0
	for _, generations := range vector.Leaves {
		skipped += int(generations[len(generations)-1].Generation) + 1 - len(generations)
	}
	skipped *= len(kinds)
	if skipped > MaxRetainedWindowKeys {
		return evidence, fmt.Errorf("%w: this case's own gaps come to %d retained keys against a tree wide bound of %d, so a receiver side path here is answered out of a window that has been evicted",
			errSecretTreeRetained, skipped, MaxRetainedWindowKeys)
	}
	trees := map[string]*SecretTree{}
	for _, path := range paths {
		tree, err := NewSecretTree(crypto, leafCount, encryptionSecret)
		if err != nil {
			return evidence, fmt.Errorf("NewSecretTree for %s: %w", path, err)
		}
		trees[path] = tree
	}
	leavesField := theJsonKeyOf(t, secretTreeVector{}, "Leaves")

	for leaf, generations := range vector.Leaves {
		index := LeafIndex(leaf)
		// every derived path to this leaf's key material, each over the tree of its own.
		answers := map[string]map[RatchetType]map[uint32]secretTreeAnswer{}
		for _, path := range paths {
			collect := secretTreeDriverFor(t, path).collect
			perKind := map[RatchetType]map[uint32]secretTreeAnswer{}
			for _, kind := range kinds {
				collected, err := collect(t, trees[path], index, kind, generations)
				if err != nil {
					return evidence, fmt.Errorf("%s(leaf %d, ratchet %d): %w", path, leaf, kind, err)
				}
				perKind[kind] = collected
			}
			answers[path] = perKind
		}

		// the emit order is secretTreeCheckNamesFor's order, position by position, which
		// incomplete() holds it to.
		for position, want := range generations {
			for _, path := range paths {
				for _, kind := range kinds {
					label := stRatchetLabel(t, kind)
					publishedKey, publishedNonce, keyField, nonceField := secretTreePublishedAnswer(t, want, kind)
					at := fmt.Sprintf("%s.%d.%d.", leavesField, leaf, position)
					got := answers[path][kind][want.Generation]
					evidence.checks = append(evidence.checks,
						secretTreeCheck{
							name: path + "/" + label + "/key", field: at + keyField,
							leaf: leaf, generation: want.Generation, width: evidence.keySize,
							got: got.key, want: MustHex(t, publishedKey),
						},
						secretTreeCheck{
							name: path + "/" + label + "/nonce", field: at + nonceField,
							leaf: leaf, generation: want.Generation, width: evidence.nonceSize,
							got: got.aeadNonce, want: MustHex(t, publishedNonce),
						})
				}
			}
		}
	}

	// what each tree actually retained, against what the corpus's gaps say it should hold. Fewer
	// than the gaps is pruneRetained having fired, which it does silently; more is a receiver
	// side path that stopped consuming or erasing what it answers, which is a key a replay can
	// be opened with. The sender paths reach step() and must hold nothing at all.
	for _, path := range paths {
		want := 0
		if secretTreeDriverFor(t, path).retainsSkipped {
			want = skipped
		}
		if got := secretTreeRetainedKeys(trees[path]); got != want {
			return evidence, fmt.Errorf("%w: %s left %d retained keys over this case and the gaps it stepped past come to %d",
				errSecretTreeRetained, path, got, want)
		}
	}

	// the two vacuity controls, over the corpus's own bytes. The first is about the whole tree: a
	// leaf key that does not move when the epoch's encryption secret moves was derived from
	// something other than the epoch.
	perturbedSecret := bytes.Clone(encryptionSecret)
	perturbedSecret[0] ^= 0x01
	perturbedTree, err := NewSecretTree(crypto, leafCount, perturbedSecret)
	if err != nil {
		return evidence, fmt.Errorf("NewSecretTree over a perturbed encryption secret: %w", err)
	}
	// driven through the first derived path and that path's own driver rather than through a
	// call written out here, so the control cannot come to name a path this family no longer
	// compares -- which is a control whose baseline is nil and whose comparison verdict() skips.
	controlPath, controlKind := paths[0], kinds[0]
	evidence.controlLeaf = 0
	evidence.controlGeneration = vector.Leaves[0][0].Generation
	evidence.controlName = controlPath + "/" + stRatchetLabel(t, controlKind) + "/key"
	perturbed, err := secretTreeDriverFor(t, controlPath).collect(t, perturbedTree, 0, controlKind, vector.Leaves[0])
	if err != nil {
		return evidence, fmt.Errorf("%s over a perturbed encryption secret: %w", controlPath, err)
	}
	evidence.withoutEncryptionSecret = bytes.Clone(perturbed[evidence.controlGeneration].key)

	perturbedCiphertext := bytes.Clone(ciphertext)
	if len(perturbedCiphertext) == 0 {
		// the sample is already zero here and incomplete() refuses that; a flip needs an octet.
		perturbedCiphertext = []byte{0x00}
	}
	perturbedCiphertext[0] ^= 0x01
	perturbedHeaderKey, _, err := SenderDataKeyNonce(crypto, senderDataSecret, perturbedCiphertext)
	if err != nil {
		return evidence, fmt.Errorf("SenderDataKeyNonce over a perturbed ciphertext: %w", err)
	}
	evidence.withoutSample = bytes.Clone(perturbedHeaderKey)

	return evidence, evidence.verdict()
}

// TestVectorSecretTree is vector family 3 over the published corpus.
//
// Every assertion the tally makes after the loop exists because the loop can be made to run zero
// times without anything else in this package noticing. A filter that matched nothing, a filter
// that matched all seven published suites, a corpus that parsed to an empty array, a comparator
// that declined every case: each of those is a green run of this test with the accounting
// removed, and a failure with it.
//
// What the loop counts is not calls that returned. It counts comparisons whose computed half
// this runner itself re-checked against a GENERIC decode of the corpus text -- no struct tag in
// the way -- so a comparator that answered without computing anything is a failure here rather
// than a number that looks right.
func TestVectorSecretTree(t *testing.T) {
	tally, entries := newVectorRunTally(t, secretTreeKatFile)
	leaves := 0
	for index, raw := range entries {
		published := map[string]json.RawMessage{}
		if err := json.Unmarshal(raw, &published); err != nil {
			t.Fatalf("%s case %d: %v", secretTreeKatFile, index, err)
		}
		header := struct {
			CipherSuite uint16 `json:"cipher_suite"`
		}{}
		if err := json.Unmarshal(raw, &header); err != nil {
			t.Fatalf("%s case %d: %v", secretTreeKatFile, index, err)
		}
		suite, inScope := tally.filter(header.CipherSuite)
		if !inScope {
			continue
		}
		evidence, err := compareSecretTreeVector(t, raw)
		if err != nil {
			t.Fatalf("%s case %d (suite %#04x): %v", secretTreeKatFile, index, header.CipherSuite, err)
		}
		tally.requireCompared(t, index, suite, evidence.inScope)
		if err := evidence.verdict(); err != nil {
			t.Fatalf("%s case %d (suite %#04x): %v", secretTreeKatFile, index, header.CipherSuite, err)
		}
		leaves += evidence.leaves
		for _, check := range evidence.checks {
			// what this loop compares, and what it deliberately does NOT. The computed half
			// against the published half is verdict()'s and is not repeated here: with the
			// line below holding, the two comparisons are the same statement, and a runner
			// that spelled both would have one of them permanently unable to fail. What is
			// only reachable here is whether the answer the COMPARATOR read is the answer the
			// corpus text carries at the path this check names -- two independent decodes of
			// one file, one through this family's struct tags and one through none. A check
			// filed under another answer's path passes every comparison verdict() makes and
			// is caught by this one.
			want := publishedCorpusField(t, published, check.field)
			if published := HexOf(check.want); published != want {
				t.Fatalf("%s case %d (suite %#04x): %s at leaf %d generation %d was compared against %s and the corpus text carries %s at %s; the struct tag and the field path name different answers",
					secretTreeKatFile, index, header.CipherSuite, check.name, check.leaf, check.generation,
					published, want, check.field)
			}
			tally.answer(want)
		}
	}
	if leaves != secretTreeKatLeaves {
		t.Fatalf("%s: the covered cases carry %d leaves, want %d; the corpus's three leaf counts are the only published pin on the descent",
			secretTreeKatFile, leaves, secretTreeKatLeaves)
	}
	tally.assertRun(t, secretTreeKatCovered, secretTreeKatSkipped, secretTreeKatComparisons, secretTreeKatDistinct)
}

// TestSecretTreeFamilyIsInstalled is the registration half of task 25.
//
// Family 3 declares a generator and it is named here, so a generator dropped from that row -- or
// replaced by another family's -- fails rather than leaving the generate direction unexercised
// while the manifest still reads as installed.
func TestSecretTreeFamilyIsInstalled(t *testing.T) {
	assertVectorFamilyIsInstalled(t, 3, secretTreeKatFile, verifySecretTreeVector, generateSecretTreeVector)
}

// TestSecretTreeFamilyChecksAreEveryPathToAGeneration holds the sixteen answers this family
// compares per published generation to the number of ways this package can produce one.
//
// Both factors are DERIVED, and the path half of that is what this test exists for now. The
// ratchet type class is stRatchetKinds, which sweeps the code point space and keeps the types
// that reach a keystream, so a third ratchet type fails here rather than leaving this family
// comparing two of three and reporting a clean run. The path class is stMethodsAnsweringBytes --
// the exported methods of *SecretTree that answer a []byte, read off the compiled type in
// secret_tree_test.go -- and this family's drivers are held to it in BOTH directions: a key
// source with no driver is a path to every published generation that nothing compares, and a
// driver for a method that no longer exists is a check name that describes nothing.
//
// Measured, and the reason the class is no longer a list: with the two names that list held, a
// third exported key source added to secret_tree.go -- ProbeKey, delegating to receiverKey --
// left every test in this file green. It was caught only by the two gates in secret_tree_test.go
// that derive the same class, one file away and unused here.
func TestSecretTreeFamilyChecksAreEveryPathToAGeneration(t *testing.T) {
	kinds := stRatchetKinds(t)
	paths := secretTreeKeyPaths(t)
	names := secretTreeCheckNamesFor(t, kinds, paths)
	if want := len(kinds) * secretTreeKatAnswersPerRatchet * secretTreeKatKeyPaths; len(names) != want {
		t.Fatalf("this family compares %d answers per generation and there are %d ratchet types, %d answers each and %d paths: %v",
			len(names), len(kinds), secretTreeKatAnswersPerRatchet, secretTreeKatKeyPaths, names)
	}
	if len(names) != secretTreeKatChecksPerGeneration {
		t.Fatalf("the derived check names hold %d entries and the count this family asserts is %d; the two move together or the run count is a transcription of a class that changed",
			len(names), secretTreeKatChecksPerGeneration)
	}
	distinct := slices.Compact(slices.Sorted(slices.Values(names)))
	if len(distinct) != len(names) {
		t.Fatalf("the check names hold %d distinct entries out of %d, so one is compared twice and another not at all",
			len(distinct), len(names))
	}
	if len(paths) != secretTreeKatKeyPaths {
		t.Fatalf("this package answers a leaf's key material by %d exported paths and this family asserts %d: %v",
			len(paths), secretTreeKatKeyPaths, paths)
	}
	// the class, both directions. secretTreeKeyPaths is fatal on a member with no driver, and
	// this is the other half: a driver naming a method the type no longer has.
	driven := slices.Sorted(maps.Keys(secretTreeKeyPathDrivers))
	if class := stMethodsAnsweringBytes(t); !slices.Equal(driven, class) {
		t.Fatalf("this family drives %v and the exported methods of *SecretTree answering key material are %v; a key source with no driver here is an exported path to every published generation that this runner compares nothing for",
			driven, class)
	}
	tree := reflect.TypeOf(&SecretTree{})
	for _, path := range paths {
		method, isMethod := strings.CutPrefix(path, tree.Elem().Name()+".")
		if !isMethod {
			t.Fatalf("the key path %q is not named for a method of %s", path, tree.Elem().Name())
		}
		if _, found := tree.MethodByName(method); !found {
			t.Fatalf("%s names no method %s, so this family compares a path that does not exist", tree, method)
		}
	}
	if len(secretTreeSenderDataCheckNames) != secretTreeKatSenderDataChecks {
		t.Fatalf("this family names %d sender data answers and asserts %d",
			len(secretTreeSenderDataCheckNames), secretTreeKatSenderDataChecks)
	}
}

// stKatBaseCase answers a published case at a registered suite, together with the encoder the
// controls below corrupt it through.
//
// The base is the corpus's own and not a fixture: the whole of what the refusals below mean is
// that this exact case is accepted and a one octet edit of it is not.
func stKatBaseCase(t *testing.T) (secretTreeVector, func(secretTreeVector) json.RawMessage) {
	t.Helper()
	base := secretTreeVector{}
	found := false
	for _, raw := range LoadVectorFile(t, secretTreeKatFile) {
		candidate := secretTreeVector{}
		if err := json.Unmarshal(raw, &candidate); err != nil {
			t.Fatalf("parse a secret-tree case: %v", err)
		}
		// a case with more than one leaf, so the descent is inside every refusal below rather
		// than only inside the ones that name it. The first case of the corpus is a single leaf
		// tree and would make the aliasing and mismatch rows hold over a tree with no descent
		// in it at all.
		if _, ok := implementedSuite(candidate.CipherSuite); ok && len(candidate.Leaves) > 1 {
			base, found = candidate, true
			break
		}
	}
	if !found {
		t.Fatal("no published case at a registered suite carries more than one leaf, so this control has nothing to corrupt")
	}
	encode := func(vector secretTreeVector) json.RawMessage {
		body, err := json.Marshal(vector)
		if err != nil {
			t.Fatalf("marshal the case under test: %v", err)
		}
		return body
	}
	return base, encode
}

// stKatCloneLeaves is a deep copy of a case's published leaves, so a control can change one
// answer without changing the base every other control is built from.
func stKatCloneLeaves(leaves [][]secretTreeGeneration) [][]secretTreeGeneration {
	copied := make([][]secretTreeGeneration, 0, len(leaves))
	for _, generations := range leaves {
		copied = append(copied, slices.Clone(generations))
	}
	return copied
}

// TestCompareSecretTreeVectorRefusesAnAnswerItShouldNotAccept is the control the runner cannot
// be: it hands the comparator cases that are wrong in each of the ways the corpus is not, and
// requires the matching refusal.
//
// Each case below is a real defect class of this family -- an answer that is not the published
// one, an answer of the wrong AEAD width, two answers of one case holding one value, a leaf
// count section 7.7 does not admit, an encryption secret of the wrong length, a leaf whose
// generations run backwards, a case with no leaf and a case with no encryption_secret key at all
// -- and each names the sentinel it owes, so a refusal for the wrong reason is a failure too.
func TestCompareSecretTreeVectorRefusesAnAnswerItShouldNotAccept(t *testing.T) {
	base, encode := stKatBaseCase(t)
	flipHex := func(text string) string {
		octets := MustHex(t, text)
		if len(octets) == 0 {
			t.Fatalf("nothing to flip in %q", text)
		}
		octets[0] ^= 0x01
		return HexOf(octets)
	}

	// the unmodified case must carry a real comparison, not merely return without error: a
	// comparator that answered an empty struct would satisfy assertComparatorRefuses' acceptance
	// step and every refusal below.
	evidence, err := compareSecretTreeVector(t, encode(base))
	if err != nil {
		t.Fatalf("the unmodified published case was refused: %v", err)
	}
	want := secretTreeKatSenderDataChecks + evidence.generations*secretTreeKatChecksPerGeneration
	if !evidence.inScope || len(evidence.checks) != want {
		t.Fatalf("the unmodified published case produced %d comparisons and inScope=%v, want %d and true",
			len(evidence.checks), evidence.inScope, want)
	}
	if evidence.leaves < 2 {
		t.Fatalf("the base case carries %d leaves, so every refusal below holds over a tree with no descent in it", evidence.leaves)
	}

	// a case at a suite this package does not register is declined and is NOT a refusal: the
	// comparator has no provider for it, and turning that into an error would make fifteen of
	// the twenty-one published cases failures.
	outOfScope := base
	outOfScope.CipherSuite = 2
	if _, ok := implementedSuite(outOfScope.CipherSuite); ok {
		t.Fatal("suite 0x0002 is registered, so the out of scope case below is not out of scope")
	}
	declined, err := compareSecretTreeVector(t, encode(outOfScope))
	if err != nil {
		t.Fatalf("a case at an unimplemented suite was refused rather than declined: %v", err)
	}
	if declined.inScope {
		t.Fatal("a case at an unimplemented suite came back in scope")
	}

	wrongHandshakeKey := base
	wrongHandshakeKey.Leaves = stKatCloneLeaves(base.Leaves)
	wrongHandshakeKey.Leaves[1][0].HandshakeKey = flipHex(base.Leaves[1][0].HandshakeKey)

	wrongApplicationNonce := base
	wrongApplicationNonce.Leaves = stKatCloneLeaves(base.Leaves)
	wrongApplicationNonce.Leaves[0][1].ApplicationNonce = flipHex(base.Leaves[0][1].ApplicationNonce)

	wrongEncryptionSecret := base
	wrongEncryptionSecret.EncryptionSecret = flipHex(base.EncryptionSecret)

	wrongHeaderKey := base
	wrongHeaderKey.SenderData.Key = flipHex(base.SenderData.Key)

	wrongHeaderSecret := base
	wrongHeaderSecret.SenderData.SenderDataSecret = flipHex(base.SenderData.SenderDataSecret)

	wrongCiphertext := base
	wrongCiphertext.SenderData.Ciphertext = flipHex(base.SenderData.Ciphertext)

	// one leaf's key published as another leaf's is the wrong descent, written into the corpus
	// rather than into the code: the aliasing rule is what refuses it, ahead of the mismatch it
	// would otherwise read as.
	aliasedAcrossLeaves := base
	aliasedAcrossLeaves.Leaves = stKatCloneLeaves(base.Leaves)
	aliasedAcrossLeaves.Leaves[1][0].HandshakeKey = base.Leaves[0][0].HandshakeKey

	// and one ratchet's key published as the other's, which is the swapped section 9 label.
	aliasedAcrossRatchets := base
	aliasedAcrossRatchets.Leaves = stKatCloneLeaves(base.Leaves)
	aliasedAcrossRatchets.Leaves[0][0].ApplicationKey = base.Leaves[0][0].HandshakeKey

	narrowKey := base
	narrowKey.Leaves = stKatCloneLeaves(base.Leaves)
	narrowKey.Leaves[0][0].HandshakeKey = base.Leaves[0][0].HandshakeKey[:len(base.Leaves[0][0].HandshakeKey)-2]

	narrowNonce := base
	narrowNonce.Leaves = stKatCloneLeaves(base.Leaves)
	narrowNonce.Leaves[0][0].HandshakeNonce = base.Leaves[0][0].HandshakeNonce[:len(base.Leaves[0][0].HandshakeNonce)-2]

	// section 7.7 makes a valid leaf count a power of two, and a case one leaf short of the base
	// is not one. The refusal is the constructor's own sentinel, wrapped: a family that turned
	// it into a mismatch would be reporting a defect in this implementation for a corpus that
	// describes a tree it cannot build.
	notAPowerOfTwo := base
	notAPowerOfTwo.Leaves = stKatCloneLeaves(base.Leaves)[:len(base.Leaves)-1]

	noLeaf := base
	noLeaf.Leaves = [][]secretTreeGeneration{}

	shortEncryptionSecret := base
	shortEncryptionSecret.EncryptionSecret = base.EncryptionSecret[:len(base.EncryptionSecret)-2]

	descending := base
	descending.Leaves = stKatCloneLeaves(base.Leaves)
	slices.Reverse(descending.Leaves[0])

	// a case that publishes no encryption_secret KEY, which decodes through the struct exactly
	// as an empty value does. Built by deleting the key from a generic decode, because the
	// struct always emits it.
	withoutTheKey := map[string]json.RawMessage{}
	if err := json.Unmarshal(encode(base), &withoutTheKey); err != nil {
		t.Fatalf("re-read the base case: %v", err)
	}
	delete(withoutTheKey, theJsonKeyOf(t, secretTreeVector{}, "EncryptionSecret"))
	absentKey, err := json.Marshal(withoutTheKey)
	if err != nil {
		t.Fatalf("re-encode the base case without its encryption secret: %v", err)
	}

	assertComparatorRefuses(t, "secret-tree",
		func(t *testing.T, raw json.RawMessage) error {
			_, err := compareSecretTreeVector(t, raw)
			return err
		},
		encode(base),
		[]comparatorRefusal{
			{"one flipped octet of a published handshake_key at leaf 1", encode(wrongHandshakeKey), errSecretTreeMismatch},
			{"one flipped octet of a published application_nonce at generation 15", encode(wrongApplicationNonce), errSecretTreeMismatch},
			{"one flipped octet of the published encryption_secret", encode(wrongEncryptionSecret), errSecretTreeMismatch},
			{"one flipped octet of the published sender_data key", encode(wrongHeaderKey), errSecretTreeMismatch},
			{"one flipped octet of the published sender_data_secret", encode(wrongHeaderSecret), errSecretTreeMismatch},
			{"one flipped octet of the published sender data ciphertext", encode(wrongCiphertext), errSecretTreeMismatch},
			{"leaf 0's handshake_key published as leaf 1's", encode(aliasedAcrossLeaves), errSecretTreeAliased},
			{"the handshake_key published as the application_key of the same generation", encode(aliasedAcrossRatchets), errSecretTreeAliased},
			{"a published handshake_key one octet short of AEAD.Nk", encode(narrowKey), errSecretTreePublishedWidth},
			{"a published handshake_nonce one octet short of AEAD.Nn", encode(narrowNonce), errSecretTreePublishedWidth},
			{"a leaf count that is not a power of two", encode(notAPowerOfTwo), ErrSecretTreeLeafOutOfRange},
			{"a case that publishes no leaf at all", encode(noLeaf), errSecretTreeIncomplete},
			{"a published encryption_secret one octet short of KDF.Nh", encode(shortEncryptionSecret), ErrSecretLength},
			{"a leaf whose generations run backwards", encode(descending), errSecretTreeGenerationOrder},
			{"a case with no encryption_secret key at all", absentKey, errSecretTreeIncomplete},
		})
}

// TestSecretTreeComparisonCannotReportAComparisonItDidNotMake is the control on the evidence
// struct itself: a return that skipped the work must be refused on every caller's path rather
// than counted as a comparison that agreed.
//
// The full value below is the one this table means anything relative to. It is required to pass
// FIRST, or a rule that refused everything would satisfy every weakening under it.
func TestSecretTreeComparisonCannotReportAComparisonItDidNotMake(t *testing.T) {
	const nk, nn, nh = 16, 12, 32
	octets := func(seed byte, width int) []byte { return bytes.Repeat([]byte{seed}, width) }
	names := []string{}
	paths := secretTreeKeyPaths(t)
	for _, path := range paths {
		for _, label := range []string{"handshake", "application"} {
			names = append(names, path+"/"+label+"/key", path+"/"+label+"/nonce")
		}
	}
	checks := []secretTreeCheck{
		{name: secretTreeSenderDataCheckNames[0], field: "sender_data.key", leaf: secretTreeSenderDataLeaf,
			width: nk, got: octets(0x01, nk), want: octets(0x01, nk)},
		{name: secretTreeSenderDataCheckNames[1], field: "sender_data.nonce", leaf: secretTreeSenderDataLeaf,
			width: nn, got: octets(0x02, nn), want: octets(0x02, nn)},
	}
	for index, name := range names {
		width, seed := nk, byte(0x10+index*2)
		if strings.HasSuffix(name, "/nonce") {
			width = nn
		}
		checks = append(checks, secretTreeCheck{
			name: name, field: fmt.Sprintf("leaves.0.0.answer%d", index), leaf: 0, generation: 0,
			width: width, got: octets(seed, width), want: octets(seed, width),
		})
	}
	full := secretTreeComparison{
		inScope: true, hashSize: nh, keySize: nk, nonceSize: nn,
		leaves: 1, generations: 1, publishesEncryptionSecret: true, sample: nh,
		names: names, checks: checks,
		withoutEncryptionSecret: octets(0xa1, nk),
		withoutSample:           octets(0xa2, nk),
		controlName:             paths[0] + "/handshake/key",
		controlLeaf:             0,
		controlGeneration:       0,
	}
	if err := full.verdict(); err != nil {
		t.Fatalf("the complete comparison was refused: %v; every weakening below is measured against it", err)
	}

	for _, row := range []struct {
		name   string
		weaken func(evidence *secretTreeComparison)
		want   error
	}{
		{"a run that never entered scope", func(e *secretTreeComparison) { e.inScope = false }, errSecretTreeIncomplete},
		{"a provider whose widths were never read", func(e *secretTreeComparison) { e.keySize = 0 }, errSecretTreeIncomplete},
		{"a case with no leaf", func(e *secretTreeComparison) { e.leaves = 0 }, errSecretTreeIncomplete},
		{"a case with no published generation", func(e *secretTreeComparison) { e.generations = 0 }, errSecretTreeIncomplete},
		{"an encryption_secret key that was never published", func(e *secretTreeComparison) { e.publishesEncryptionSecret = false }, errSecretTreeIncomplete},
		{"a sender data key derived over an empty sample", func(e *secretTreeComparison) { e.sample = 0 }, errSecretTreeIncomplete},
		{"a shorter check name class than this family owes", func(e *secretTreeComparison) { e.names = names[:len(names)-1] }, errSecretTreeIncomplete},
		{"the perturbed encryption_secret control never run", func(e *secretTreeComparison) { e.withoutEncryptionSecret = nil }, errSecretTreeIncomplete},
		{"the perturbed ciphertext control never run", func(e *secretTreeComparison) { e.withoutSample = nil }, errSecretTreeIncomplete},
		{"a control that names no answer", func(e *secretTreeComparison) { e.controlName = "" }, errSecretTreeIncomplete},
		{"a control aimed at an answer this run never made", func(e *secretTreeComparison) { e.controlLeaf = 7 }, errSecretTreeIncomplete},
		{"one comparison dropped", func(e *secretTreeComparison) { e.checks = slices.Clone(checks)[:len(checks)-1] }, errSecretTreeIncomplete},
		{"two comparisons transposed", func(e *secretTreeComparison) {
			swapped := slices.Clone(checks)
			swapped[2], swapped[3] = swapped[3], swapped[2]
			e.checks = swapped
		}, errSecretTreeIncomplete},
		{"a comparison whose computed half is empty", func(e *secretTreeComparison) {
			emptied := slices.Clone(checks)
			emptied[4].got = nil
			e.checks = emptied
		}, errSecretTreeIncomplete},
		{"a comparison that names no published field", func(e *secretTreeComparison) {
			unfiled := slices.Clone(checks)
			unfiled[4].field = ""
			e.checks = unfiled
		}, errSecretTreeIncomplete},
		{"a comparison that names no width", func(e *secretTreeComparison) {
			unwidened := slices.Clone(checks)
			unwidened[4].width = 0
			e.checks = unwidened
		}, errSecretTreeIncomplete},
		{"a published answer of the wrong width", func(e *secretTreeComparison) {
			narrowed := slices.Clone(checks)
			narrowed[4].want = octets(0x33, nk-1)
			narrowed[4].got = octets(0x33, nk-1)
			e.checks = narrowed
		}, errSecretTreePublishedWidth},
		{"two published answers holding one value", func(e *secretTreeComparison) {
			aliased := slices.Clone(checks)
			aliased[4].want = checks[2].want
			aliased[4].got = checks[2].want
			e.checks = aliased
		}, errSecretTreeAliased},
		{"one comparison that disagrees", func(e *secretTreeComparison) {
			disagreeing := slices.Clone(checks)
			disagreeing[4].got = octets(0x7f, nk)
			e.checks = disagreeing
		}, errSecretTreeMismatch},
		{"an encryption secret that never reached the leaf key", func(e *secretTreeComparison) {
			e.withoutEncryptionSecret = bytes.Clone(full.answerAt(full.controlName, 0, 0))
		}, errSecretTreeDidNotMove},
		{"a ciphertext sample that never reached the header key", func(e *secretTreeComparison) {
			e.withoutSample = bytes.Clone(checks[0].got)
		}, errSecretTreeDidNotMove},
	} {
		weakened := full
		weakened.checks = slices.Clone(full.checks)
		row.weaken(&weakened)
		err := weakened.verdict()
		if err == nil {
			t.Errorf("%s was accepted; the evidence struct reports a comparison it did not make", row.name)
			continue
		}
		if !errors.Is(err, row.want) {
			t.Errorf("%s was refused as %v, want %v; a refusal for the wrong reason is a rule about something else",
				row.name, err, row.want)
		}
	}
}

// ---------------------------------------------------------------------------
// the generate direction, written from RFC 9420 section 9 rather than from this package
// ---------------------------------------------------------------------------

// secretTreeGenerateLabels is every string RFC 9420 section 9 puts into a KDFLabel, so a control
// can derive a case under a label the section does not name and require the consume direction to
// refuse it.
//
// A struct of labels rather than literals at the call sites, and that is the whole reason it
// exists: every one of these produces a well formed answer of exactly the right width whichever
// string is used, so the ONLY way to show that the round trip below can fail is to run it under
// a label that is wrong on purpose. See
// TestTheConsumeDirectionRefusesAGeneratedSecretTreeUnderTheWrongLabel.
type secretTreeGenerateLabels struct {
	tree        string
	left        string
	right       string
	handshake   string
	application string
	key         string
	aeadNonce   string
	secret      string
}

// secretTreeRfcLabels is RFC 9420 section 9's own strings:
//
//	tree_node_[2i+1]   = ExpandWithLabel(tree_node_[i], "tree", "left",  KDF.Nh)
//	tree_node_[2i+2]   = ExpandWithLabel(tree_node_[i], "tree", "right", KDF.Nh)
//	handshake_ratchet  = ExpandWithLabel(leaf_secret, "handshake",   "", KDF.Nh)
//	application_ratchet= ExpandWithLabel(leaf_secret, "application", "", KDF.Nh)
//	key_[j]            = DeriveTreeSecret(ratchet_[j], "key",    j, AEAD.Nk)
//	nonce_[j]          = DeriveTreeSecret(ratchet_[j], "nonce",  j, AEAD.Nn)
//	ratchet_[j+1]      = DeriveTreeSecret(ratchet_[j], "secret", j, KDF.Nh)
//
// Transcribed from the section and NOT read off secret_tree.go, which is the point of the whole
// derivation below: a label copied from the line above produces a perfectly well formed key that
// agrees with nobody, and the only thing that can see it is a second transcription of the text
// held against a value somebody else published. That holding is
// TestTheIndependentSecretTreeMatchesEveryUpstreamVector, which reproduces all 668 answers the
// vendored corpus publishes at the two registered suites.
var secretTreeRfcLabels = secretTreeGenerateLabels{
	tree:        "tree",
	left:        "left",
	right:       "right",
	handshake:   "handshake",
	application: "application",
	key:         "key",
	aeadNonce:   "nonce",
	secret:      "secret",
}

// secretTreeGeneratedSuites is what the generator emits at, as constants rather than as a read of
// the registry.
//
// Both halves are constants and both matter. A generator that asked the registry which suites to
// cover would cover whatever the registry answered and could never report that it had stopped
// covering it; a generator that read AEAD.Nk and AEAD.Nn off the provider would derive at
// whatever widths the code under test chose, which is the one thing a second opinion must not do
// -- a provider answering Nk 32 where RFC 9420 section 17.1 registers 16 would then be agreed
// with rather than caught. The widths below are the registry table of section 17.1.
// TestSecretTreeGeneratedSuitesAreTheRegistryAtTheseWidths is what fails on the day a third suite
// is registered or a width moves.
var secretTreeGeneratedSuites = []struct {
	code        uint16
	keyLength   int
	nonceLength int
	leaves      []uint32
}{
	{
		code:        uint16(CipherSuiteX25519AesGcm128Sha256Ed25519),
		keyLength:   16,
		nonceLength: 12,
		leaves:      []uint32{1, 4},
	},
	{
		code:        uint16(CipherSuiteX25519ChaCha20Sha256Ed25519),
		keyLength:   32,
		nonceLength: 12,
		leaves:      []uint32{1, 4},
	},
}

// secretTreeGeneratedGenerations is the generations every generated leaf publishes.
//
// Three rather than one, and not consecutive. Generation 0 is the base case, generation 1 is the
// single ratchet step, and the jump to 5 is the skip -- a receiver that ratcheted forward the
// wrong number of times answers 0 and 1 perfectly. The single leaf arm of the generated cases is
// the one with no descent in it and the four leaf arm is the one that has it, so the pair covers
// both.
var secretTreeGeneratedGenerations = []uint32{0, 1, 5}

// secretTreeGeneratedOctets is deterministic filler for the generate direction. Deterministic
// rather than crypto.Random, for the reason task 16 gives: a generated case that fails is then
// the same case on the next run. It also keeps the generator's own call closure clear of the
// provider, which is what lets the disjointness gate say something about it.
func secretTreeGeneratedOctets(field string, index int) []byte {
	digest := sha256.Sum256([]byte(fmt.Sprintf("mls slice1 p4 task25 secret-tree generate %s %d", field, index)))
	return digest[:]
}

// independentSecretTreeLeafSecret is RFC 9420 section 9's descent, written from the section.
//
// The tree of a valid epoch is full -- section 7.7 makes the leaf count a power of two -- so the
// leaves under a node at height h are exactly the ones whose index agrees with that node above
// bit h, and the bit at each height is therefore what chooses the child. That is why this needs
// no node arithmetic at all and reaches nothing this package declares: the array representation,
// Left, Right, Root and Level are the implementation's way of answering the same question, and a
// second opinion that borrowed them would agree with a transposed descent by being transposed
// the same way.
func independentSecretTreeLeafSecret(t *testing.T, encryptionSecret []byte, leaf uint32, leafCount uint32, labels secretTreeGenerateLabels) []byte {
	t.Helper()
	height := uint32(0)
	for span := uint32(1); span < leafCount; span <<= 1 {
		height++
	}
	if leafCount == 0 || leafCount != uint32(1)<<height {
		t.Fatalf("a leaf count of %d is not a power of two and RFC 9420 section 7.7 admits no other", leafCount)
	}
	if leaf >= leafCount {
		t.Fatalf("leaf %d of a tree with %d leaves", leaf, leafCount)
	}
	derived := append([]byte(nil), encryptionSecret...)
	for level := height; level > 0; level-- {
		side := labels.left
		if leaf&(uint32(1)<<(level-1)) != 0 {
			side = labels.right
		}
		derived = independentExpandWithLabel(t, derived, labels.tree, []byte(side), sha256.Size)
	}
	return derived
}

// independentDeriveTreeSecret is RFC 9420 section 9's DeriveTreeSecret:
//
//	DeriveTreeSecret(Secret, Label, Generation, Length) =
//	    ExpandWithLabel(Secret, Label, Generation, Length)
//
// with the generation written as a big endian uint32 and no length prefix of its own, because a
// uint32 is a fixed width field of the presentation language rather than a vector. A little
// endian generation, or one written as an opaque<V>, produces a well formed key at every
// generation and a different one at every generation past zero -- which is exactly the shape the
// published corpus's generation 15 separates and generation 0 cannot.
func independentDeriveTreeSecret(t *testing.T, secret []byte, label string, generation uint32, length int) []byte {
	t.Helper()
	return independentExpandWithLabel(t, secret, label, binary.BigEndian.AppendUint32(nil, generation), length)
}

// independentSecretTreeRatchet walks one leaf's hash ratchet from the start of the epoch and
// answers the key and the nonce at every generation up to and including highest.
//
// The three derivations of a step all read the CURRENT ratchet secret, before it is replaced, and
// each binds the generation number into its own KDFLabel. Deriving the successor first and the
// key from it, or binding the generation into only one of the three, both produce well formed
// answers at every generation.
func independentSecretTreeRatchet(t *testing.T, root []byte, highest uint32, keyLength int, nonceLength int, labels secretTreeGenerateLabels) (keys [][]byte, aeadNonces [][]byte) {
	t.Helper()
	current := append([]byte(nil), root...)
	for generation := uint32(0); ; generation++ {
		keys = append(keys, independentDeriveTreeSecret(t, current, labels.key, generation, keyLength))
		aeadNonces = append(aeadNonces, independentDeriveTreeSecret(t, current, labels.aeadNonce, generation, nonceLength))
		if generation == highest {
			return keys, aeadNonces
		}
		current = independentDeriveTreeSecret(t, current, labels.secret, generation, sha256.Size)
	}
}

// independentSenderDataKeyNonce is RFC 9420 section 6.3.2's header material:
//
//	ciphertext_sample = ciphertext[0..KDF.Nh-1]   (all of it when it is shorter)
//	sender_data_key   = ExpandWithLabel(sender_data_secret, "key",   ciphertext_sample, AEAD.Nk)
//	sender_data_nonce = ExpandWithLabel(sender_data_secret, "nonce", ciphertext_sample, AEAD.Nn)
//
// The sample rule is the part with no other witness on this side: a sample of the wrong length,
// or taken from the wrong offset, is still real ciphertext and derives a key of exactly the right
// width that simply opens nothing -- and against a peer with the same mistake it interoperates.
func independentSenderDataKeyNonce(t *testing.T, senderDataSecret []byte, ciphertext []byte, keyLength int, nonceLength int, labels secretTreeGenerateLabels) (key []byte, aeadNonce []byte) {
	t.Helper()
	sample := ciphertext
	if len(sample) > sha256.Size {
		sample = sample[:sha256.Size]
	}
	return independentExpandWithLabel(t, senderDataSecret, labels.key, sample, keyLength),
		independentExpandWithLabel(t, senderDataSecret, labels.aeadNonce, sample, nonceLength)
}

// generateSecretTreeCases builds fresh secret-tree entries whose every answer is computed by the
// hand written derivation above.
//
// This is the shape that makes the generate direction worth running, and it is not the shape the
// plan's version has. A generator that computed its answers with NewSecretTree and NextSenderKey
// and a verifier that checked them with NewSecretTree and receiverKey round trip perfectly and
// say nothing about conformance at all -- they prove this code agrees with itself, which it would
// whatever it computed. Nothing below reaches package mls, which is asserted rather than
// described: TestTheGenerateDirectionSharesNoCodePathWithVerify derives the production function
// names from the package's own source and requires this function's call closure to be disjoint
// from them.
//
// labels is how the round trip is shown to be able to FAIL. Every case emitted under
// secretTreeRfcLabels agrees with the implementation, so a consume direction that compared
// nothing and one that compared everything produce identical runs over them.
func generateSecretTreeCases(t *testing.T, labels secretTreeGenerateLabels) []secretTreeVector {
	t.Helper()
	generated := []secretTreeVector{}
	highest := secretTreeGeneratedGenerations[len(secretTreeGeneratedGenerations)-1]
	for _, suite := range secretTreeGeneratedSuites {
		for _, leafCount := range suite.leaves {
			tag := fmt.Sprintf("suite %d leaves %d", suite.code, leafCount)
			encryptionSecret := secretTreeGeneratedOctets(tag+" encryption secret", 0)
			senderDataSecret := secretTreeGeneratedOctets(tag+" sender data secret", 0)
			// three digests of filler, so the ciphertext is 96 octets and the sample rule has a
			// boundary to be wrong about. A ciphertext at or below KDF.Nh is used whole and
			// would leave the cut unexercised in the generate direction.
			ciphertext := []byte{}
			for index := 0; index < 3; index++ {
				ciphertext = append(ciphertext, secretTreeGeneratedOctets(tag+" ciphertext", index)...)
			}
			headerKey, headerNonce := independentSenderDataKeyNonce(
				t, senderDataSecret, ciphertext, suite.keyLength, suite.nonceLength, labels)

			vector := secretTreeVector{
				CipherSuite:      suite.code,
				EncryptionSecret: HexOf(encryptionSecret),
				SenderData: secretTreeSenderData{
					SenderDataSecret: HexOf(senderDataSecret),
					Ciphertext:       HexOf(ciphertext),
					Key:              HexOf(headerKey),
					Nonce:            HexOf(headerNonce),
				},
			}
			for leaf := uint32(0); leaf < leafCount; leaf++ {
				leafSecret := independentSecretTreeLeafSecret(t, encryptionSecret, leaf, leafCount, labels)
				handshakeKeys, handshakeNonces := independentSecretTreeRatchet(
					t, independentExpandWithLabel(t, leafSecret, labels.handshake, nil, sha256.Size),
					highest, suite.keyLength, suite.nonceLength, labels)
				applicationKeys, applicationNonces := independentSecretTreeRatchet(
					t, independentExpandWithLabel(t, leafSecret, labels.application, nil, sha256.Size),
					highest, suite.keyLength, suite.nonceLength, labels)
				published := []secretTreeGeneration{}
				for _, generation := range secretTreeGeneratedGenerations {
					published = append(published, secretTreeGeneration{
						Generation:       generation,
						HandshakeKey:     HexOf(handshakeKeys[generation]),
						HandshakeNonce:   HexOf(handshakeNonces[generation]),
						ApplicationKey:   HexOf(applicationKeys[generation]),
						ApplicationNonce: HexOf(applicationNonces[generation]),
					})
				}
				vector.Leaves = append(vector.Leaves, published)
			}
			generated = append(generated, vector)
		}
	}
	return generated
}

// generateSecretTreeVector is the Generate half of family 3: fresh entries in the mlswg format,
// answered by the hand written derivation, for the registry to feed back through
// verifySecretTreeVector.
func generateSecretTreeVector(t *testing.T) json.RawMessage {
	t.Helper()
	body, err := json.Marshal(generateSecretTreeCases(t, secretTreeRfcLabels))
	if err != nil {
		t.Fatalf("marshal the generated secret tree cases: %v", err)
	}
	return body
}

// secretTreeGeneratedCases is how many cases the generator emits, derived from its own table so
// the counts below cannot drift from it.
func secretTreeGeneratedCases() int {
	cases := 0
	for _, suite := range secretTreeGeneratedSuites {
		cases += len(suite.leaves)
	}
	return cases
}

// TestVectorSecretTreeGenerate is the generate direction of family 3.
//
// What it closes that verification alone cannot: a pinned vector never passes through our own
// derivation from the other side, so a sender path and a receiver path that are wrong in the same
// way verify perfectly against each other. Generating a case and feeding it back sees that -- but
// only if the generator is not the verifier under another name, which is the trap this task's
// name states and the one the plan's own version walks into by computing its answers with
// NextSenderKey.
//
// Four things stand against the loop passing vacuously. The generated entries are re-derived here
// by hand and the comparison count is asserted, against DISTINCT answers, so a generator emitting
// one repeated value is a failure. They are then consumed by the real comparator, whose full
// verdict must accept them, and the number of comparisons IT made is asserted too. And a case
// derived under a label section 9 does not name must be refused --
// TestTheConsumeDirectionRefusesAGeneratedSecretTreeUnderTheWrongLabel.
func TestVectorSecretTreeGenerate(t *testing.T) {
	serialized := generateSecretTreeVector(t)
	readBack := []secretTreeVector{}
	if err := json.Unmarshal(serialized, &readBack); err != nil {
		t.Fatalf("the generated cases do not parse: %v", err)
	}
	if len(readBack) != secretTreeGeneratedCases() {
		t.Fatalf("generated %d cases, want %d", len(readBack), secretTreeGeneratedCases())
	}

	compared, leaves := 0, 0
	answers := map[string]int{}
	sawDescent := false
	for _, vector := range readBack {
		suite, ok := implementedSuite(vector.CipherSuite)
		if !ok {
			t.Fatalf("generated a case at unimplemented suite %#04x", vector.CipherSuite)
		}
		crypto, err := NewCryptoProvider(suite)
		if err != nil {
			t.Fatalf("NewCryptoProvider(%#04x): %v", uint16(suite), err)
		}
		keyLength, nonceLength := crypto.KeySize(), crypto.NonceSize()
		leafCount := uint32(len(vector.Leaves))
		if leafCount > 1 {
			sawDescent = true
		}
		leaves += len(vector.Leaves)

		published := map[string]json.RawMessage{}
		if err := json.Unmarshal(mustRemarshal(t, vector), &published); err != nil {
			t.Fatalf("re-read a generated case: %v", err)
		}
		encryptionSecret := MustHex(t, vector.EncryptionSecret)
		headerKey, headerNonce := independentSenderDataKeyNonce(
			t, MustHex(t, vector.SenderData.SenderDataSecret), MustHex(t, vector.SenderData.Ciphertext),
			keyLength, nonceLength, secretTreeRfcLabels)
		for _, header := range []struct {
			field string
			got   []byte
		}{
			{"sender_data.key", headerKey},
			{"sender_data.nonce", headerNonce},
		} {
			want := publishedCorpusField(t, published, header.field)
			if HexOf(header.got) != want {
				t.Fatalf("suite %#04x: the hand written derivation gives %s for %s, the generated case carries %s",
					uint16(suite), HexOf(header.got), header.field, want)
			}
			answers[want]++
			compared++
		}

		highest := secretTreeGeneratedGenerations[len(secretTreeGeneratedGenerations)-1]
		for leaf := uint32(0); leaf < leafCount; leaf++ {
			leafSecret := independentSecretTreeLeafSecret(t, encryptionSecret, leaf, leafCount, secretTreeRfcLabels)
			for _, ratchet := range []struct {
				label      string
				keyField   string
				nonceField string
			}{
				{secretTreeRfcLabels.handshake, "handshake_key", "handshake_nonce"},
				{secretTreeRfcLabels.application, "application_key", "application_nonce"},
			} {
				keys, aeadNonces := independentSecretTreeRatchet(
					t, independentExpandWithLabel(t, leafSecret, ratchet.label, nil, sha256.Size),
					highest, keyLength, nonceLength, secretTreeRfcLabels)
				for position, generation := range secretTreeGeneratedGenerations {
					at := fmt.Sprintf("leaves.%d.%d.", leaf, position)
					for _, answer := range []struct {
						field string
						got   []byte
					}{
						{at + ratchet.keyField, keys[generation]},
						{at + ratchet.nonceField, aeadNonces[generation]},
					} {
						want := publishedCorpusField(t, published, answer.field)
						if HexOf(answer.got) != want {
							t.Fatalf("suite %#04x leaf %d generation %d: the hand written derivation gives %s for %s, the generated case carries %s",
								uint16(suite), leaf, generation, HexOf(answer.got), answer.field, want)
						}
						answers[want]++
						compared++
					}
				}
			}
		}
	}
	if !sawDescent {
		t.Fatal("no generated case carries more than one leaf, so the generate direction never descends and the descent is unexercised by it")
	}
	want := secretTreeGeneratedCases()*secretTreeKatSenderDataChecks +
		leaves*len(secretTreeGeneratedGenerations)*(secretTreeKatChecksPerGeneration/secretTreeKatKeyPaths)
	if compared != want {
		t.Fatalf("re-derived %d answers over %d leaves, want %d", compared, leaves, want)
	}
	if len(answers) != compared {
		t.Fatalf("the %d re-derivations were made against %d distinct answers; a generator emitting one repeated value would compare that many times and pin one answer",
			compared, len(answers))
	}

	// and the consume direction, which is the half that makes any of this a statement about
	// conformance: SecretTree must reproduce every one of these answers, through both paths.
	consumed := 0
	for _, vector := range readBack {
		evidence, err := compareSecretTreeVector(t, mustRemarshal(t, vector))
		if err != nil {
			t.Fatalf("the consume direction refused a generated case: %v", err)
		}
		if err := evidence.verdict(); err != nil {
			t.Fatalf("the consume direction refused a generated case: %v", err)
		}
		consumed += len(evidence.checks)
	}
	if got := secretTreeGeneratedCases()*secretTreeKatSenderDataChecks +
		leaves*len(secretTreeGeneratedGenerations)*secretTreeKatChecksPerGeneration; consumed != got {
		t.Fatalf("the consume direction made %d comparisons over the generated cases, want %d", consumed, got)
	}
	t.Logf("secret-tree generate: %d cases over %d leaves, %d answers re-derived by hand, %d consumed by the implementation",
		len(readBack), leaves, compared, consumed)
}

// mustRemarshal re-encodes one generated case as the corpus serializes one.
func mustRemarshal(t *testing.T, vector secretTreeVector) json.RawMessage {
	t.Helper()
	body, err := json.Marshal(vector)
	if err != nil {
		t.Fatalf("marshal a generated case: %v", err)
	}
	return body
}

// TestTheConsumeDirectionRefusesAGeneratedSecretTreeUnderTheWrongLabel is the control the test
// above needs.
//
// Every case the generator emits agrees with the implementation, so a consume direction that
// compared nothing and one that compared everything produce identical runs over them -- which is
// exactly the failure task 16 shipped. A case derived under a string RFC 9420 section 9 does not
// name is one the loop MUST refuse, and the answers are perfectly well formed AEAD key material
// whichever label produced them, so nothing about the values themselves says which.
//
// The descent row is the one that says the most, and it is also the one whose scope has to be
// stated: swapping "left" and "right" permutes the leaves of a tree that HAS more than one, and
// a single leaf tree has no descent at all, so the single leaf cases are legitimately accepted.
// Reporting that as a refusal that did not happen would be reporting a defect in the generator.
func TestTheConsumeDirectionRefusesAGeneratedSecretTreeUnderTheWrongLabel(t *testing.T) {
	for _, wrong := range []struct {
		name           string
		labels         secretTreeGenerateLabels
		want           error
		onlyWithLeaves bool
	}{
		{
			name: "the descent labels transposed",
			labels: func() secretTreeGenerateLabels {
				swapped := secretTreeRfcLabels
				swapped.left, swapped.right = secretTreeRfcLabels.right, secretTreeRfcLabels.left
				return swapped
			}(),
			want:           errSecretTreeMismatch,
			onlyWithLeaves: true,
		},
		{
			name: "the two ratchet roots transposed",
			labels: func() secretTreeGenerateLabels {
				swapped := secretTreeRfcLabels
				swapped.handshake, swapped.application = secretTreeRfcLabels.application, secretTreeRfcLabels.handshake
				return swapped
			}(),
			want: errSecretTreeMismatch,
		},
		{
			name: "the ratchet successor derived under the key label",
			labels: func() secretTreeGenerateLabels {
				reused := secretTreeRfcLabels
				reused.secret = secretTreeRfcLabels.key
				return reused
			}(),
			want: errSecretTreeMismatch,
		},
		{
			name: "the tree label misspelt",
			labels: func() secretTreeGenerateLabels {
				misspelt := secretTreeRfcLabels
				misspelt.tree = secretTreeRfcLabels.tree + " "
				return misspelt
			}(),
			want:           errSecretTreeMismatch,
			onlyWithLeaves: true,
		},
	} {
		cases := generateSecretTreeCases(t, wrong.labels)
		if len(cases) == 0 {
			t.Fatalf("the generator produced no case under %s", wrong.name)
		}
		refused, accepted := 0, 0
		for _, vector := range cases {
			_, err := compareSecretTreeVector(t, mustRemarshal(t, vector))
			if err == nil {
				accepted++
				if wrong.onlyWithLeaves && len(vector.Leaves) == 1 {
					// a one leaf tree has no descent, so a case derived with the descent
					// labels changed is the same case: accepting it is correct.
					continue
				}
				t.Errorf("a case with %s and %d leaves was accepted; the consume direction is not comparing",
					wrong.name, len(vector.Leaves))
				continue
			}
			if !errors.Is(err, wrong.want) {
				t.Errorf("a case with %s was refused as %v, want %v", wrong.name, err, wrong.want)
				continue
			}
			refused++
		}
		if refused == 0 {
			t.Errorf("no case with %s was refused over %d generated cases", wrong.name, len(cases))
		}
		if !wrong.onlyWithLeaves && accepted != 0 {
			t.Errorf("%d of %d generated cases with %s were accepted", accepted, len(cases), wrong.name)
		}
	}
}

// TestTheIndependentSecretTreeMatchesEveryUpstreamVector pins the hand written derivation to the
// published corpus.
//
// This is what makes the generate direction worth running at all. A generator agreeing with the
// verifier proves two spellings of one algorithm agree; a generator that reproduces every answer
// mlswg published, computed with crypto/hmac from the RFC text and reaching nothing this package
// declares, is a SECOND IMPLEMENTATION, and the round trip through it is then a statement about
// conformance.
//
// It is also what holds secretTreeRfcLabels to the section rather than to secret_tree.go. The
// labels are transcribed here and nothing in this package can check a transcription against
// itself; 668 published answers can.
//
// The comparison count is asserted for the reason the runner's is, and so is the count of
// distinct answers: a derivation compared against one repeated answer satisfies the total.
func TestTheIndependentSecretTreeMatchesEveryUpstreamVector(t *testing.T) {
	compared, leaves := 0, 0
	answers := map[string]int{}
	for index, raw := range LoadVectorFile(t, secretTreeKatFile) {
		vector := secretTreeVector{}
		if err := json.Unmarshal(raw, &vector); err != nil {
			t.Fatalf("case %d: %v", index, err)
		}
		suite, ok := implementedSuite(vector.CipherSuite)
		if !ok {
			continue
		}
		crypto, err := NewCryptoProvider(suite)
		if err != nil {
			t.Fatalf("NewCryptoProvider(%#04x): %v", uint16(suite), err)
		}
		// the widths this derivation is asked for come from the provider HERE and not inside the
		// derivation, which is the same trade generatedSuites makes the other way: this test is
		// about the derivation agreeing with the corpus, and the corpus's own answer lengths are
		// what the widths have to be. The generator is where they are constants.
		keyLength, nonceLength := crypto.KeySize(), crypto.NonceSize()
		encryptionSecret := MustHex(t, vector.EncryptionSecret)
		leafCount := uint32(len(vector.Leaves))
		leaves += len(vector.Leaves)

		headerKey, headerNonce := independentSenderDataKeyNonce(
			t, MustHex(t, vector.SenderData.SenderDataSecret), MustHex(t, vector.SenderData.Ciphertext),
			keyLength, nonceLength, secretTreeRfcLabels)
		for _, header := range []struct {
			name string
			got  []byte
			want string
		}{
			{"sender_data key", headerKey, vector.SenderData.Key},
			{"sender_data nonce", headerNonce, vector.SenderData.Nonce},
		} {
			if HexOf(header.got) != header.want {
				t.Fatalf("case %d (suite %#04x): the hand written %s is %s, the corpus publishes %s",
					index, uint16(suite), header.name, HexOf(header.got), header.want)
			}
			answers[header.want]++
			compared++
		}

		for leaf := uint32(0); leaf < leafCount; leaf++ {
			generations := vector.Leaves[leaf]
			highest := generations[len(generations)-1].Generation
			leafSecret := independentSecretTreeLeafSecret(t, encryptionSecret, leaf, leafCount, secretTreeRfcLabels)
			for _, ratchet := range []struct {
				label string
				key   func(secretTreeGeneration) string
				nonce func(secretTreeGeneration) string
			}{
				{
					label: secretTreeRfcLabels.handshake,
					key:   func(published secretTreeGeneration) string { return published.HandshakeKey },
					nonce: func(published secretTreeGeneration) string { return published.HandshakeNonce },
				},
				{
					label: secretTreeRfcLabels.application,
					key:   func(published secretTreeGeneration) string { return published.ApplicationKey },
					nonce: func(published secretTreeGeneration) string { return published.ApplicationNonce },
				},
			} {
				keys, aeadNonces := independentSecretTreeRatchet(
					t, independentExpandWithLabel(t, leafSecret, ratchet.label, nil, sha256.Size),
					highest, keyLength, nonceLength, secretTreeRfcLabels)
				for _, published := range generations {
					for _, answer := range []struct {
						name string
						got  []byte
						want string
					}{
						{ratchet.label + " key", keys[published.Generation], ratchet.key(published)},
						{ratchet.label + " nonce", aeadNonces[published.Generation], ratchet.nonce(published)},
					} {
						if HexOf(answer.got) != answer.want {
							t.Fatalf("case %d (suite %#04x) leaf %d generation %d: the hand written %s is %s, the corpus publishes %s",
								index, uint16(suite), leaf, published.Generation, answer.name,
								HexOf(answer.got), answer.want)
						}
						answers[answer.want]++
						compared++
					}
				}
			}
		}
	}
	if leaves != secretTreeKatLeaves {
		t.Fatalf("the hand written derivation walked %d leaves, want %d", leaves, secretTreeKatLeaves)
	}
	if compared != secretTreeKatDistinct {
		t.Fatalf("the hand written derivation reproduced %d published answers, want %d", compared, secretTreeKatDistinct)
	}
	if len(answers) != compared {
		t.Fatalf("the %d answers were compared against %d distinct published values; a derivation agreeing with one repeated value satisfies the total",
			compared, len(answers))
	}
	t.Logf("the hand written secret tree reproduced %d published answers over %d leaves", compared, leaves)
}

// TestTheIndependentSecretTreeSeesATransposedDescent requires the hand written derivation to
// disagree with its own transposition.
//
// Without it the derivation could agree with a transposed implementation by being transposed the
// same way, and every comparison above would still pass -- the same argument
// TestTheIndependentKeyScheduleSeesATransposedExtract makes for HKDF-Extract's two arguments, and
// the same class of defect: "left" and "right" are two strings of the same shape in adjacent
// lines, and a tree that swapped them derives a full set of well formed leaf secrets.
func TestTheIndependentSecretTreeSeesATransposedDescent(t *testing.T) {
	transposed := secretTreeRfcLabels
	transposed.left, transposed.right = secretTreeRfcLabels.right, secretTreeRfcLabels.left
	encryptionSecret := secretTreeGeneratedOctets("transposed descent", 0)
	const leafCount = uint32(4)

	agreed, checked := 0, 0
	for leaf := uint32(0); leaf < leafCount; leaf++ {
		straight := independentSecretTreeLeafSecret(t, encryptionSecret, leaf, leafCount, secretTreeRfcLabels)
		swapped := independentSecretTreeLeafSecret(t, encryptionSecret, leaf, leafCount, transposed)
		if len(straight) == 0 || len(swapped) == 0 {
			t.Fatalf("leaf %d derived %d and %d octets, so this comparison is over nothing", leaf, len(straight), len(swapped))
		}
		if bytes.Equal(straight, swapped) {
			agreed++
		}
		checked++
	}
	if checked != int(leafCount) {
		t.Fatalf("the control ran over %d leaves, want %d", checked, leafCount)
	}
	if agreed != 0 {
		t.Fatalf("%d of %d leaves derive the same secret under transposed descent labels, so this derivation cannot see a transposed descent",
			agreed, checked)
	}
	// and the one leaf tree is the case where they legitimately agree, so the control above is a
	// statement about the descent rather than about the labels being different strings.
	single := independentSecretTreeLeafSecret(t, encryptionSecret, 0, 1, secretTreeRfcLabels)
	if !bytes.Equal(single, independentSecretTreeLeafSecret(t, encryptionSecret, 0, 1, transposed)) {
		t.Fatal("a one leaf tree derives different secrets under transposed descent labels, and it takes no descent step at all")
	}
}

// TestSecretTreeGeneratedSuitesAreTheRegistryAtTheseWidths holds the generator's own table of
// code points and AEAD widths to the registry, which is the check the generator cannot make about
// itself without calling into the package it is meant to stay clear of.
//
// The widths are the half that matters most. A generator reading Nk and Nn off the provider would
// agree with a provider that answered the wrong ones; these are RFC 9420 section 17.1's table,
// and this is where they stop being true if a suite's parameters move or a third suite is
// registered.
func TestSecretTreeGeneratedSuitesAreTheRegistryAtTheseWidths(t *testing.T) {
	covered := []CipherSuite{}
	for _, suite := range secretTreeGeneratedSuites {
		covered = append(covered, CipherSuite(suite.code))
		params, err := LookupSuite(CipherSuite(suite.code))
		if err != nil {
			t.Fatalf("LookupSuite(%#04x): %v", suite.code, err)
		}
		if params.Nk != suite.keyLength || params.Nn != suite.nonceLength {
			t.Errorf("suite %#04x is registered at Nk %d and Nn %d, and the generator derives at %d and %d",
				suite.code, params.Nk, params.Nn, suite.keyLength, suite.nonceLength)
		}
		if len(suite.leaves) == 0 {
			t.Errorf("suite %#04x generates no case at all", suite.code)
		}
		descends := false
		for _, leafCount := range suite.leaves {
			if leafCount > 1 {
				descends = true
			}
		}
		if !descends {
			t.Errorf("suite %#04x generates only single leaf cases, so the generate direction never descends at it", suite.code)
		}
	}
	slices.Sort(covered)
	if !slices.Equal(covered, Suites()) {
		t.Fatalf("the generator covers %v and the registry holds %v; widen secretTreeGeneratedSuites", covered, Suites())
	}
	// the hand written derivation writes sha256 in, so a suite at another KDF.Nh would need it
	// widened before it could be a second opinion at that suite.
	for _, suite := range Suites() {
		crypto, err := NewCryptoProvider(suite)
		if err != nil {
			t.Fatalf("NewCryptoProvider(%#04x): %v", uint16(suite), err)
		}
		if crypto.HashSize() != sha256.Size {
			t.Fatalf("suite %#04x has KDF.Nh %d and the hand written secret tree writes %d",
				uint16(suite), crypto.HashSize(), sha256.Size)
		}
	}
}
