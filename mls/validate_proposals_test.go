package mls

import (
	"bytes"
	"errors"
	"go/ast"
	"maps"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/mls/syntax"
)

// ---------------------------------------------------------------------------
// the fixtures
// ---------------------------------------------------------------------------

// testTreeWith builds a pre-commit tree of len(names) leaves and returns it with the members that
// own them.
func testTreeWith(t *testing.T, crypto CryptoProvider, names ...string) (*RatchetTree, []*testMember) {
	t.Helper()
	tree := NewRatchetTree()
	members := make([]*testMember, 0, len(names))
	for _, name := range names {
		member := testIdentity(t, crypto, name)
		leaf, _ := testLeafNode(t, crypto, member)
		if _, err := tree.AddLeaf(leaf); err != nil {
			t.Fatalf("AddLeaf %s: %v", name, err)
		}
		members = append(members, member)
	}
	return tree, members
}

// testProposalList buckets entries the way (*ProposalCache).Resolve does, so a fixture cannot
// accidentally test the bucket rules while meaning to test something else.
//
// The switch is Resolve's and is written out rather than called, because Resolve buckets while
// resolving a ProposalOrRef vector against a cache and these fixtures have neither. A type with no
// bucket is fatal here rather than dropped: a fixture whose proposal went nowhere would be judged
// by nothing and would report the clean bill a correct one reports.
func testProposalList(t *testing.T, entries ...CachedProposal) *ProposalList {
	t.Helper()
	// the type has no bucket to fill any more -- every per-type view is the commit order
	// filtered -- so this hands the order over and nothing else. What it still owes is the
	// refusal the old bucketing switch owed: a fixture built around a type no view answers would
	// be a list every rule stated over a view walks straight past, which is a green test over an
	// input nothing judged. The class is proposalListViewedTypes and not a switch of its own.
	viewed := proposalListViewedTypes()
	for _, entry := range entries {
		if !viewed[entry.Proposal.ProposalType] {
			t.Fatalf("testProposalList was handed a %s and a ProposalList answers no view for that type, so no rule stated over a view would read it",
				proposalTypeName(entry.Proposal.ProposalType))
		}
	}
	return NewProposalList(entries)
}

// testListPlus answers a NEW list carrying this one's commit order with the entries appended.
//
// A list has one setter, its constructor, so a fixture that wants a longer commit order builds a
// longer one rather than appending to a field. That is the whole property this package was
// rebuilt for, held at the fixtures too: a test that could append to the order without the views
// following would be a test of a type this package no longer has.
func testListPlus(t *testing.T, list *ProposalList, extra ...CachedProposal) *ProposalList {
	t.Helper()
	return NewProposalList(append(slices.Clone(list.All()), extra...))
}

// testListEntryAt answers a POINTER to the entry of the commit order that the named view answers
// at position at.
//
// THE VIEWS ARE ANSWERS AND NOT STORAGE, which is what this exists to say. (*ProposalList).Updates
// filters the commit order into a fresh slice at every call, so a fixture that edited
// `list.Updates()[at]` would edit a copy nothing reads and would then assert that the rule under
// test accepted an unbroken list. The commit order is the one place a list keeps a proposal, so an
// edit that has to be seen is made there.
func testListEntryAt(t *testing.T, list *ProposalList, view string, at int) *CachedProposal {
	t.Helper()
	carries := ProposalType(0)
	named := false
	for _, bucket := range proposalBucketsOf(list) {
		if bucket.accessor == view {
			carries, named = bucket.carries, true
		}
	}
	if !named {
		t.Fatalf("a ProposalList answers no view called %s", view)
	}
	order := list.All()
	seen := 0
	for i := range order {
		if order[i].Proposal.ProposalType != carries {
			continue
		}
		if seen == at {
			return &order[i]
		}
		seen += 1
	}
	t.Fatalf("the commit order carries %d %s proposals and this asked for the one at %d",
		seen, proposalTypeName(carries), at)
	return nil
}

// testAddOf, testRemoveOf and testGceOf are the three inline proposals this package's fixtures are
// built from, and each is attributed to testCommitterLeaf.
//
// THE SENDER IS NOT A DETAIL OF THESE BUILDERS, it is the fact the commit door's join reads. A
// by-value entry of a commit's ProposalOrRef vector resolves to the COMMITTER -- that is what
// (*ProposalCache).Resolve does and what entryTheCommitNames restates -- so a fixture whose inline
// entries carried the zero value while its committer sat at leaf 0 could not tell the field from
// the constant. Leaving the sender at the zero value here would put that back one builder down.
func testAddOf(kp *KeyPackage) CachedProposal {
	return CachedProposal{
		Proposal: Proposal{ProposalType: ProposalTypeAdd, Add: &Add{KeyPackage: *kp}},
		Sender:   testCommitterLeaf,
		ByValue:  true,
	}
}

func testRemoveOf(leaf LeafIndex) CachedProposal {
	return CachedProposal{
		Proposal: Proposal{ProposalType: ProposalTypeRemove, Remove: &Remove{Removed: leaf}},
		Sender:   testCommitterLeaf,
		ByValue:  true,
	}
}

func testUpdateOf(sender LeafIndex, leaf *LeafNode) CachedProposal {
	return CachedProposal{
		Proposal: Proposal{ProposalType: ProposalTypeUpdate, Update: &Update{LeafNode: *leaf}},
		Sender:   sender,
		ByValue:  true,
	}
}

// testUpdateProposalOf is one member's Update: a leaf built for the update source, signed at that
// member's own leaf index under the group id the input announces, carried by a proposal attributed
// to that leaf.
//
// The three have to agree and there is one place they are written, because every way they can
// disagree is a refusal at the update door rather than the rule a test is about: a leaf signed at
// another index, a leaf signed in another group, a proposal attributed to another leaf.
func testUpdateProposalOf(t *testing.T, crypto CryptoProvider, m *testMember,
	sender LeafIndex) (CachedProposal, *LeafNode) {
	t.Helper()
	leaf, _ := testUpdateLeafNode(t, crypto, m, testValidationGroupId(), sender)
	return testUpdateOf(sender, leaf), leaf
}

// testResignedUpdateOf re-signs a leaf a test has just altered and answers the proposal carrying
// it, so that the rule under test is what refuses it rather than the signature.
//
// Every fixture below that reaches past the signature clause of section 7.3 needs this: the
// signature covers the whole leaf, so a capability vector emptied or an extension appended after
// the fixture signed is a leaf whose signature no longer verifies, and the update door would
// answer errBadSignature for a leaf whose fault is something else entirely.
func testResignedUpdateOf(t *testing.T, crypto CryptoProvider, m *testMember,
	sender LeafIndex, leaf *LeafNode) CachedProposal {
	t.Helper()
	if err := leaf.Sign(crypto, m.SigPriv, testValidationGroupId(), sender); err != nil {
		t.Fatalf("re-sign %s's update leaf at leaf %d: %v", m.Name, sender, err)
	}
	return testUpdateOf(sender, leaf)
}

func testGceOf(exts ...Extension) CachedProposal {
	return CachedProposal{
		Proposal: Proposal{ProposalType: ProposalTypeGroupContextExtensions,
			GroupContextExtensions: &GroupContextExtensions{Extensions: exts}},
		Sender:  testCommitterLeaf,
		ByValue: true,
	}
}

// testRequiredCapabilitiesExtension is a required_capabilities naming one extension type no
// fixture leaf of this package lists, so a capability rule that fires is firing on the
// requirement rather than on something testCapabilities happens to leave out.
func testRequiredCapabilitiesExtension(t *testing.T) Extension {
	t.Helper()
	body, err := syntax.Marshal(&RequiredCapabilities{
		ExtensionTypes: []ExtensionType{ExtensionType(0xF00A)}})
	if err != nil {
		t.Fatalf("marshal required_capabilities: %v", err)
	}
	return Extension{ExtensionType: ExtensionTypeRequiredCapabilities, ExtensionData: body}
}

// testCommitterLeaf is the leaf every fixture in this package commits from, and it is NOT leaf
// zero.
//
// IT IS ONE OF THE FOUR NUMBERS THIS CORPUS SPENT THREE ROUNDS BEING UNABLE TO SEE. Every commit
// fixture used to be built at leaf 0 and every by-value entry left its Sender at the zero value,
// so `Sender: self.Committer` in entryTheCommitNames and the constant `LeafIndex(0)` produced the
// same octets for every input this package drives -- and the mutation was applied, measured and
// left the whole suite green. With the committer off leaf zero the two are told apart in both
// directions: the constant refuses every honest inline proposal and accepts one attributed to leaf
// 0, which apply_proposals.go line 131 writes into leaf 0.
//
// A BY-VALUE ENTRY CARRIES THIS SAME VALUE AS ITS SENDER, which is not a coincidence to be kept in
// step by hand but (*ProposalCache).Resolve's own rule: a proposal carried inline is attributed to
// the committer. testAddOf, testRemoveOf and testGceOf read this constant for that reason, so a
// fixture cannot attribute an inline proposal to a leaf no resolution could have produced without
// saying so.
const testCommitterLeaf = LeafIndex(1)

// testOwnLeaf is the leaf the member JUDGING these commits sits at, and it IS leaf zero, which is
// deliberate and is the opposite of what this comment used to claim.
//
// THE SENTENCE HERE SAID "neither zero nor the committer's" WHILE THE LINE BELOW IT SAID ZERO, and
// validate_commit_test.go repeated the claim -- so a reader auditing ValSem203PathDecrypt's
// leaf-zero exposure was told by two files that it could not exist. It could: `in.Own ==
// in.Committer` was replaced by `in.Own == LeafIndex(0)` with the whole suite green, because Own
// was 0 and Committer 1 in every fixture and the two mutants agree wherever that holds.
//
// LEAF ZERO IS WHERE THE FIELD AND THE CONSTANT DISAGREE, which is why it stays. What was missing
// was not a different number here but the OTHER witness: an input in which Own and Committer are
// the same leaf. testCommitTheCommitterJudgesItselfFromABlankSibling is that input, and the
// relation claim in fixture_corpus_test.go is what holds the corpus to carrying both.
//
// Own differing from Committer is still what makes ValSem203's tree reads run at all: the rule
// returns before it touches a tree when the two agree -- "the committer seals nothing to itself".
const testOwnLeaf = LeafIndex(0)

// testUpdatedLeaf is the member every update and single-leaf remove fixture of this file is ABOUT,
// and it is not the committer's leaf.
//
// DERIVED FROM testCommitterLeaf RATHER THAN WRITTEN AS A NUMBER, and held to it by
// TestTheLeafTheseFixturesUpdateIsNotTheCommitters. ValSem111 refuses a list covering the
// committer's own update and validateCommitterIsNotRemoved refuses one removing its leaf, so a
// fixture whose subject drifted onto the committer's leaf would be refused by one of those two
// rather than by the rule it is a fixture for -- and every negative assertion over it would go on
// passing, because they all assert THAT the aggregate refuses.
//
// It used to be able to be written as a number because the inputs of this file were all judged at
// leaf zero: the committer was LeafIndex(0) at forty-nine of fifty-two call sites, so "not the
// committer" and "not leaf zero" were the same sentence and neither was stated.
const testUpdatedLeaf = testCommitterLeaf ^ 1

// TestTheLeafTheseFixturesUpdateIsNotTheCommitters holds the two constants apart.
//
// It is one line because the constants are one line, and it is here because the alternative is a
// comment: every negative fixture of this file asserts THAT the aggregate refuses, so a subject
// that drifted onto the committer's leaf would be refused by ValSem111 or by
// validateCommitterIsNotRemoved and every one of those assertions would go on passing.
func TestTheLeafTheseFixturesUpdateIsNotTheCommitters(t *testing.T) {
	if testUpdatedLeaf == testCommitterLeaf {
		t.Fatalf("the fixtures of this file update leaf %d and commit from leaf %d; every rule they are built for is then unreachable behind the two rules that refuse a committer judging itself",
			testUpdatedLeaf, testCommitterLeaf)
	}
}

// testValidationGroupId is the group id every input below is judged under.

// ONE SPELLING, because an update leaf's LeafNodeTBS carries the group id and a fixture that
// signed under one id while the input announced another would be a leaf refused for the wrong
// reason -- and every test asserting a refusal would pass over it.
func testValidationGroupId() []byte {
	return []byte("group")
}

func testValidationInput(t *testing.T, crypto CryptoProvider, tree *RatchetTree,
	committer LeafIndex, list *ProposalList) *ProposalValidationInput {
	t.Helper()
	return &ProposalValidationInput{
		Crypto: crypto,
		Tree:   tree,
		Context: &GroupContext{Version: ProtocolVersionMls10,
			CipherSuite: CipherSuiteX25519ChaCha20Sha256Ed25519,
			GroupId:     testValidationGroupId(), Epoch: 1},
		Committer: committer,
		List:      list,
		Now:       time.Now(),
	}
}

// ---------------------------------------------------------------------------
// the error class, derived from the file that declares it
// ---------------------------------------------------------------------------

const proposalValidationErrorsFile = "errors_proposal_validation.go"

// proposalValidationOwnedErrors is every value errors_proposal_validation.go declares, keyed by
// its name.
//
// Written out rather than derived at run time, for the reason every other class of this package
// gives: a class computed from the file it is judging agrees with that file whatever the file
// says. TestProposalValidationOwnedErrorsIsEveryErrorItsFileDeclares is what joins the two, in
// both directions, and mlsErrorClasses is what puts all of them inside the package wide
// exclusivity sweep.
var proposalValidationOwnedErrors = map[string]error{
	"ErrAddDuplicateSignatureKey":        ErrAddDuplicateSignatureKey,
	"ErrDuplicateInitKey":                ErrDuplicateInitKey,
	"ErrAddDuplicateEncryptionKey":       ErrAddDuplicateEncryptionKey,
	"ErrInitEqualsEncryptionKey":         ErrInitEqualsEncryptionKey,
	"ErrSuiteMismatch":                   ErrSuiteMismatch,
	"ErrAddMissingRequiredCapability":    ErrAddMissingRequiredCapability,
	"ErrDuplicateRemove":                 ErrDuplicateRemove,
	"ErrRemoveNonMember":                 ErrRemoveNonMember,
	"ErrUpdateMissingRequiredCapability": ErrUpdateMissingRequiredCapability,
	"ErrUpdateDuplicateEncryptionKey":    ErrUpdateDuplicateEncryptionKey,
	"ErrSelfUpdateInCommit":              ErrSelfUpdateInCommit,
	"ErrUpdateSenderNotMember":           ErrUpdateSenderNotMember,
	"ErrUpdateLeafNodeInvalid":           ErrUpdateLeafNodeInvalid,
	"ErrUpdateEncryptionKeyUnchanged":    ErrUpdateEncryptionKeyUnchanged,
	"ErrRemoveCommitter":                 ErrRemoveCommitter,
	"ErrUpdateOrRemoveSameLeaf":          ErrUpdateOrRemoveSameLeaf,
	"errNilProposalValidationInput":      errNilProposalValidationInput,
	"errNilProposalList":                 errNilProposalList,
	"errNilRatchetTree":                  errNilRatchetTree,
}

// TestProposalValidationOwnedErrorsIsEveryErrorItsFileDeclares holds the class to the file in both
// directions, so a seventeenth refusal added by a later task is swept the moment it is declared
// and a name deleted from the file fails here rather than shrinking every sweep in silence.
func TestProposalValidationOwnedErrorsIsEveryErrorItsFileDeclares(t *testing.T) {
	declared := packageLevelDeclarations(t, ".")
	fromFile := []string{}
	for name, file := range declared {
		if file == proposalValidationErrorsFile {
			fromFile = append(fromFile, name)
		}
	}
	slices.Sort(fromFile)
	// the positive control: a scan reading the wrong file reports exactly the clean bill a
	// complete one reports
	if !slices.Contains(fromFile, "ErrRemoveNonMember") {
		t.Fatalf("the scan read %v out of %s, which certainly declares ErrRemoveNonMember, so it is reading something other than that file",
			fromFile, proposalValidationErrorsFile)
	}
	if want := slices.Sorted(maps.Keys(proposalValidationOwnedErrors)); !slices.Equal(fromFile, want) {
		t.Fatalf("%s declares %v and proposalValidationOwnedErrors holds %v; every sweep of this package runs over the second",
			proposalValidationErrorsFile, fromFile, want)
	}
	if _, held := mlsErrorClasses[proposalValidationErrorsFile]; !held {
		t.Fatalf("mlsErrorClasses holds no class for %s, so the package wide exclusivity sweep runs past every one of these",
			proposalValidationErrorsFile)
	}
}

// TestEveryProposalListRefusalIsDistinctFromEveryOther is the one-value-per-rule property, swept
// over the derived class rather than over a slice literal of its own.
//
// Thirteen ValSem codes is thirteen rules, and the plan this task comes from puts five of them on
// three values. errors.Is cannot tell two rules apart when they answer one value, so a sweep over
// every ordered pair is the only statement of the property that survives somebody later deciding
// two of them are "the same thing really".
func TestEveryProposalListRefusalIsDistinctFromEveryOther(t *testing.T) {
	names := slices.Sorted(maps.Keys(proposalValidationOwnedErrors))
	// 19 since ProposalList came to derive its per-type views from its commit order: it was 21 --
	// the nineteen this file's own task declared plus ErrUpdateLeafNodeInvalid and
	// ErrUpdateEncryptionKeyUnchanged from the section 12.1.2 update door -- and
	// ErrProposalListMisbucketed and ErrProposalListBucketsDisagree went with the two structural
	// rules they were the values of. Both were about a list whose buckets disagreed with its
	// commit order, and that list can no longer be constructed. The count is asserted rather than
	// derived for lifecycleOwnedErrors' reason -- a later task adding or removing a sentinel moves
	// this number and says which task moved it.
	if len(names) != 19 {
		t.Fatalf("the proposal list refusal set holds %d values, this file declares 19", len(names))
	}
	for _, name := range names {
		one := proposalValidationOwnedErrors[name]
		if one == nil {
			t.Fatalf("%s is nil", name)
		}
		if !strings.HasPrefix(one.Error(), "mls: ") {
			t.Errorf("%s reads %q; every refusal this package hands a caller names the package it came from",
				name, one.Error())
		}
		for _, other := range names {
			if name != other && errors.Is(one, proposalValidationOwnedErrors[other]) {
				t.Fatalf("%s and %s are the same value: %v", name, other, one)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// the rule table: one row per rule, derived against the rules the aggregate runs
// ---------------------------------------------------------------------------

// proposalListRuleRow is one rule of the list, the input that breaks it, and the value it must
// answer.
type proposalListRuleRow struct {
	sentinel error
	build    func(t *testing.T, crypto CryptoProvider) *ProposalValidationInput
}

// proposalListRuleName is the declared name of one of the rule functions, read off the value
// rather than written beside it, so a row cannot claim to cover a rule it does not hold.
func proposalListRuleName(rule func(*ProposalValidationInput) error) string {
	full := runtime.FuncForPC(reflect.ValueOf(rule).Pointer()).Name()
	return full[strings.LastIndex(full, ".")+1:]
}

// proposalListRules is every rule ValidateProposalList runs, in the order it runs them, read off
// the three groups the production source declares rather than listed here.
func proposalListRules() []string {
	names := []string{}
	for _, group := range [][]func(*ProposalValidationInput) error{
		proposalListStructuralChecks, proposalListChecks, proposalListCrossChecks} {
		for _, rule := range group {
			names = append(names, proposalListRuleName(rule))
		}
	}
	return names
}

// proposalListRuleRows is the input that breaks each rule and the value that rule must answer.
//
// Every row is built so that every rule the aggregate runs BEFORE it passes, which is what lets
// the sweep assert the same value out of the named function and out of ValidateProposalList. A row
// whose fixture tripped an earlier rule would still pass a test that only called the named
// function, and the aggregate half is what catches it.
func proposalListRuleRows() map[string]proposalListRuleRow {
	return map[string]proposalListRuleRow{
		// a psk proposal, which is registered and outside the v1 profile. It is carried in the
		// commit order and in no bucket, which is what a list assembled around a type this
		// build does not implement looks like.
		"ValSem113ProposalTypeSupported": {sentinel: errProfilePsk,
			build: func(t *testing.T, crypto CryptoProvider) *ProposalValidationInput {
				tree, _ := testTreeWith(t, crypto, "alice")
				psk := CachedProposal{Proposal: Proposal{
					ProposalType: ProposalTypePreSharedKey, PreSharedKey: &PreSharedKey{}}}
				return testValidationInput(t, crypto, tree, testCommitterLeaf,
					NewProposalList([]CachedProposal{psk}))
			}},
		"validateOneGroupContextExtensions": {sentinel: errMultipleGroupContextExtensions,
			build: func(t *testing.T, crypto CryptoProvider) *ProposalValidationInput {
				tree, _ := testTreeWith(t, crypto, "alice")
				one := testGceOf(Extension{ExtensionType: ExtensionType(0x00FF), ExtensionData: []byte{1}})
				two := testGceOf(Extension{ExtensionType: ExtensionType(0x00FF), ExtensionData: []byte{2}})
				return testValidationInput(t, crypto, tree, testCommitterLeaf, testProposalList(t, one, two))
			}},
		// two entries of one commit naming one cached proposal. They remove DIFFERENT leaves,
		// so no content rule of this file could answer instead.
		"validateNoRepeatedProposalReference": {sentinel: errDuplicateProposalReference,
			build: func(t *testing.T, crypto CryptoProvider) *ProposalValidationInput {
				tree, _ := testTreeWith(t, crypto, "alice", "bob", "carol")
				first := testRemoveOf(1)
				first.ByValue, first.Ref = false, ProposalRef([]byte("one-name"))
				second := testRemoveOf(2)
				second.ByValue, second.Ref = false, ProposalRef([]byte("one-name"))
				return testValidationInput(t, crypto, tree, testCommitterLeaf, testProposalList(t, first, second))
			}},
		"ValSem101UniqueSignatureKey": {sentinel: ErrAddDuplicateSignatureKey,
			build: func(t *testing.T, crypto CryptoProvider) *ProposalValidationInput {
				tree, _ := testTreeWith(t, crypto, "alice")
				carol := testIdentity(t, crypto, "carol")
				first, _, _ := testKeyPackage(t, crypto, carol)
				second, _, _ := testKeyPackage(t, crypto, carol)
				return testValidationInput(t, crypto, tree, testCommitterLeaf,
					testProposalList(t, testAddOf(first), testAddOf(second)))
			}},
		"ValSem102UniqueInitKey": {sentinel: ErrDuplicateInitKey,
			build: func(t *testing.T, crypto CryptoProvider) *ProposalValidationInput {
				tree, _ := testTreeWith(t, crypto, "alice")
				dave, _, _ := testKeyPackage(t, crypto, testIdentity(t, crypto, "dave"))
				erin, _, _ := testKeyPackage(t, crypto, testIdentity(t, crypto, "erin"))
				erin.InitKey = dave.InitKey
				return testValidationInput(t, crypto, tree, testCommitterLeaf,
					testProposalList(t, testAddOf(dave), testAddOf(erin)))
			}},
		"ValSem103UniqueEncryptionKey": {sentinel: ErrAddDuplicateEncryptionKey,
			build: func(t *testing.T, crypto CryptoProvider) *ProposalValidationInput {
				tree, _ := testTreeWith(t, crypto, "alice")
				dave, _, _ := testKeyPackage(t, crypto, testIdentity(t, crypto, "dave"))
				erin, _, _ := testKeyPackage(t, crypto, testIdentity(t, crypto, "erin"))
				erin.LeafNode.EncryptionKey = dave.LeafNode.EncryptionKey
				return testValidationInput(t, crypto, tree, testCommitterLeaf,
					testProposalList(t, testAddOf(dave), testAddOf(erin)))
			}},
		"ValSem104InitNotEqualEncryptionKey": {sentinel: ErrInitEqualsEncryptionKey,
			build: func(t *testing.T, crypto CryptoProvider) *ProposalValidationInput {
				tree, _ := testTreeWith(t, crypto, "alice")
				dave, _, _ := testKeyPackage(t, crypto, testIdentity(t, crypto, "dave"))
				dave.InitKey = HpkePublicKey(dave.LeafNode.EncryptionKey)
				return testValidationInput(t, crypto, tree, testCommitterLeaf,
					testProposalList(t, testAddOf(dave)))
			}},
		"ValSem105SuiteAndVersionMatch": {sentinel: ErrSuiteMismatch,
			build: func(t *testing.T, crypto CryptoProvider) *ProposalValidationInput {
				tree, _ := testTreeWith(t, crypto, "alice")
				dave, _, _ := testKeyPackage(t, crypto, testIdentity(t, crypto, "dave"))
				dave.Version = ProtocolVersion(0x0002)
				return testValidationInput(t, crypto, tree, testCommitterLeaf,
					testProposalList(t, testAddOf(dave)))
			}},
		"ValSem106RequiredCapabilitiesSatisfied": {sentinel: ErrAddMissingRequiredCapability,
			build: func(t *testing.T, crypto CryptoProvider) *ProposalValidationInput {
				tree, _ := testTreeWith(t, crypto, "alice")
				dave, _, _ := testKeyPackage(t, crypto, testIdentity(t, crypto, "dave"))
				in := testValidationInput(t, crypto, tree, testCommitterLeaf,
					testProposalList(t, testAddOf(dave)))
				in.Context.Extensions = []Extension{testRequiredCapabilitiesExtension(t)}
				return in
			}},
		"ValSem107UniqueRemove": {sentinel: ErrDuplicateRemove,
			build: func(t *testing.T, crypto CryptoProvider) *ProposalValidationInput {
				tree, _ := testTreeWith(t, crypto, "alice", "bob")
				return testValidationInput(t, crypto, tree, testCommitterLeaf,
					testProposalList(t, testRemoveOf(testUpdatedLeaf),
						testRemoveOf(testUpdatedLeaf)))
			}},
		"ValSem108RemoveExists": {sentinel: ErrRemoveNonMember,
			build: func(t *testing.T, crypto CryptoProvider) *ProposalValidationInput {
				tree, _ := testTreeWith(t, crypto, "alice", "bob")
				return testValidationInput(t, crypto, tree, testCommitterLeaf,
					testProposalList(t, testRemoveOf(9)))
			}},
		"ValSem109UpdateRequiredCapabilities": {sentinel: ErrUpdateMissingRequiredCapability,
			build: func(t *testing.T, crypto CryptoProvider) *ProposalValidationInput {
				tree, members := testTreeWith(t, crypto, "alice", "bob")
				leaf, _ := testLeafNode(t, crypto, members[testUpdatedLeaf])
				in := testValidationInput(t, crypto, tree, testCommitterLeaf,
					testProposalList(t, testUpdateOf(testUpdatedLeaf, leaf)))
				in.Context.Extensions = []Extension{testRequiredCapabilitiesExtension(t)}
				return in
			}},
		// the updated member republishes carol's encryption key, which no member may hold twice
		"ValSem110UpdateUniqueEncryptionKey": {sentinel: ErrUpdateDuplicateEncryptionKey,
			build: func(t *testing.T, crypto CryptoProvider) *ProposalValidationInput {
				tree, members := testTreeWith(t, crypto, "alice", "bob", "carol")
				leaf, _ := testLeafNode(t, crypto, members[testUpdatedLeaf])
				leaf.EncryptionKey = tree.Leaf(2).EncryptionKey
				return testValidationInput(t, crypto, tree, testCommitterLeaf,
					testProposalList(t, testUpdateOf(testUpdatedLeaf, leaf)))
			}},
		"ValSem111NoCommitterUpdate": {sentinel: ErrSelfUpdateInCommit,
			build: func(t *testing.T, crypto CryptoProvider) *ProposalValidationInput {
				tree, members := testTreeWith(t, crypto, "alice", "bob")
				leaf, _ := testLeafNode(t, crypto, members[testCommitterLeaf])
				return testValidationInput(t, crypto, tree, testCommitterLeaf,
					testProposalList(t, testUpdateOf(testCommitterLeaf, leaf)))
			}},
		"ValSem112UpdateSenderIsMember": {sentinel: ErrUpdateSenderNotMember,
			build: func(t *testing.T, crypto CryptoProvider) *ProposalValidationInput {
				tree, members := testTreeWith(t, crypto, "alice", "bob")
				leaf, _ := testLeafNode(t, crypto, members[1])
				return testValidationInput(t, crypto, tree, testCommitterLeaf,
					testProposalList(t, testUpdateOf(9, leaf)))
			}},
		// the updated member's own leaf, signed at its own index -- and then signed AGAIN by the
		// committer, so what the leaf publishes and what signed it are two different members.
		// Every other field is well formed, which is the point: before this door existed the leaf
		// went into the tree with nothing having verified the signature at all.
		"validateUpdateLeafNodeIsValidForAnUpdate": {sentinel: ErrUpdateLeafNodeInvalid,
			build: func(t *testing.T, crypto CryptoProvider) *ProposalValidationInput {
				tree, members := testTreeWith(t, crypto, "alice", "bob")
				_, leaf := testUpdateProposalOf(t, crypto, members[testUpdatedLeaf],
					testUpdatedLeaf)
				return testValidationInput(t, crypto, tree, testCommitterLeaf, testProposalList(t,
					testResignedUpdateOf(t, crypto, members[testCommitterLeaf],
						testUpdatedLeaf, leaf)))
			}},
		// that member's update, well formed and correctly signed, republishing the encryption key
		// its leaf already holds. ValSem110 excludes exactly that key from what it compares
		// against, so no other rule of this file can answer for this one.
		"validateUpdateChangesTheEncryptionKey": {sentinel: ErrUpdateEncryptionKeyUnchanged,
			build: func(t *testing.T, crypto CryptoProvider) *ProposalValidationInput {
				tree, members := testTreeWith(t, crypto, "alice", "bob")
				_, leaf := testUpdateProposalOf(t, crypto, members[testUpdatedLeaf],
					testUpdatedLeaf)
				leaf.EncryptionKey = tree.Leaf(testUpdatedLeaf).EncryptionKey
				return testValidationInput(t, crypto, tree, testCommitterLeaf, testProposalList(t,
					testResignedUpdateOf(t, crypto, members[testUpdatedLeaf],
						testUpdatedLeaf, leaf)))
			}},
		// an update and a remove landing on one leaf. Two removes would be ValSem107's, which
		// is why this pair is mixed.
		"validateSingleUpdateOrRemovePerLeaf": {sentinel: ErrUpdateOrRemoveSameLeaf,
			build: func(t *testing.T, crypto CryptoProvider) *ProposalValidationInput {
				tree, members := testTreeWith(t, crypto, "alice", "bob")
				update, _ := testUpdateProposalOf(t, crypto, members[testUpdatedLeaf],
					testUpdatedLeaf)
				return testValidationInput(t, crypto, tree, testCommitterLeaf,
					testProposalList(t, update, testRemoveOf(testUpdatedLeaf)))
			}},
		"validateCommitterIsNotRemoved": {sentinel: ErrRemoveCommitter,
			build: func(t *testing.T, crypto CryptoProvider) *ProposalValidationInput {
				tree, _ := testTreeWith(t, crypto, "alice", "bob")
				return testValidationInput(t, crypto, tree, testCommitterLeaf,
					testProposalList(t, testRemoveOf(testCommitterLeaf)))
			}},
	}
}

// TestEachProposalListRuleAnswersOnlyItsOwnSentinel is the property the whole of this task turns
// on, and it is the reason the sentinels are not shared.
//
// Three claims per row, and they are different claims. The named function must answer its own
// value, which is ledger 17's rule that a refusal owes a test naming its sentinel. NO OTHER ROW'S
// value may answer, which is what "thirteen rules" means and is invisible to a test that only
// asserts the positive half: five of these rules answered three values in the plan as written, and
// every positive assertion over them passed. And ValidateProposalList must answer the SAME value,
// which says the aggregate reaches this rule with this input rather than tripping over an earlier
// one -- a fixture that failed two rules would satisfy the first two claims and hide the second
// rule entirely.
func TestEachProposalListRuleAnswersOnlyItsOwnSentinel(t *testing.T) {
	crypto := testCrypto(t)
	rows := proposalListRuleRows()
	for _, name := range slices.Sorted(maps.Keys(rows)) {
		row := rows[name]
		t.Run(name, func(t *testing.T) {
			in := row.build(t, crypto)
			ruled := proposalListRuleFor(t, name)(in)
			if !errors.Is(ruled, row.sentinel) {
				t.Fatalf("%s answered %v, want %v", name, ruled, row.sentinel)
			}
			for _, other := range slices.Sorted(maps.Keys(rows)) {
				if other == name {
					continue
				}
				if errors.Is(ruled, rows[other].sentinel) {
					t.Errorf("%s answers %s's value as well, so errors.Is cannot tell the two rules apart",
						name, other)
				}
			}
			aggregated := ValidateProposalList(in)
			if !errors.Is(aggregated, row.sentinel) {
				t.Fatalf("ValidateProposalList over the %s fixture answered %v, want %v; the aggregate is reaching a different rule with this input",
					name, aggregated, row.sentinel)
			}
		})
	}
}

// proposalListRuleFor answers the rule function of that name out of the groups the production
// source declares, so a row is joined to a rule the aggregate actually runs.
func proposalListRuleFor(t *testing.T, name string) func(*ProposalValidationInput) error {
	t.Helper()
	for _, group := range [][]func(*ProposalValidationInput) error{
		proposalListStructuralChecks, proposalListChecks, proposalListCrossChecks} {
		for _, rule := range group {
			if proposalListRuleName(rule) == name {
				return rule
			}
		}
	}
	t.Fatalf("no rule of ValidateProposalList is named %s", name)
	return nil
}

// TestEveryRuleTheAggregateRunsHasARowAndEveryRowIsARuleItRuns holds the table above to the
// production groups in both directions.
//
// Without it the table is a list, and a list is what rule 5 exists about: a fourteenth ValSem code
// added to proposalListChecks with no row would be run by the aggregate and asserted by nothing,
// and a row naming a rule that had been deleted would go on reporting a clean bill over a rule
// that no longer exists.
func TestEveryRuleTheAggregateRunsHasARowAndEveryRowIsARuleItRuns(t *testing.T) {
	rules := proposalListRules()
	rows := proposalListRuleRows()
	if !slices.Contains(rules, "ValSem108RemoveExists") {
		t.Fatalf("the rule groups read as %v, and ValidateProposalList certainly runs ValSem108RemoveExists, so the names are being read off something else",
			rules)
	}
	for _, name := range rules {
		if _, written := rows[name]; !written {
			t.Errorf("ValidateProposalList runs %s and no row builds an input that breaks it, so nothing here says what that rule refuses",
				name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(rows)) {
		if !slices.Contains(rules, name) {
			t.Errorf("a row names %s and ValidateProposalList runs no rule under that name", name)
		}
	}
	// and every rule is named once: a function repeated across two groups would be run twice
	// and would make the group ordering above unreadable
	seen := map[string]bool{}
	for _, name := range rules {
		if seen[name] {
			t.Errorf("%s appears in more than one of the rule groups", name)
		}
		seen[name] = true
	}
}

// proposalListRulesDeclared is every rule validate_proposals.go declares, read off the SIGNATURE
// rather than off a name.
//
// The shape is the class: a package level function taking one *ProposalValidationInput and
// answering one error is a rule of this file and there is nothing else it could be. Deriving on
// the ValSem prefix instead would cover the thirteen codes and leave the six rules that carry no
// code outside the coverage gate entirely -- the two structural ones, the bucket count, the
// single GCE, the same-leaf rule and the remove-the-committer rule. That is rule 5s shortfall
// exactly: a class stated over the members somebody remembered rather than over the ones that
// exist.
func proposalListRulesDeclared(t *testing.T) []string {
	t.Helper()
	parsed := mustParseSource(t, "validate_proposals.go")
	found := []string{}
	for _, declaration := range parsed.file.Decls {
		function, isFunction := declaration.(*ast.FuncDecl)
		if !isFunction || function.Recv != nil || function.Type.Params == nil {
			continue
		}
		if len(function.Type.Params.List) != 1 ||
			parsed.render(function.Type.Params.List[0].Type) != "*ProposalValidationInput" {
			continue
		}
		if function.Type.Results == nil || len(function.Type.Results.List) != 1 ||
			parsed.render(function.Type.Results.List[0].Type) != "error" {
			continue
		}
		// the aggregate is not one of its own rules
		if function.Name.Name == "ValidateProposalList" {
			continue
		}
		found = append(found, function.Name.Name)
	}
	slices.Sort(found)
	return found
}

// TestValidateProposalListRunsEveryRuleThisFileDeclares derives the rule set from the source
// rather than counting it here.
//
// The aggregate holds its rules in three slice literals. A slice literal is a transcription, and
// the failure it has on this project is the quiet one: a rule declared, documented and never run.
// So the class is every rule-shaped function validate_proposals.go declares, held in both
// directions, and the thirteen ValSem codes are counted inside it -- because "thirteen" is a claim
// about RFC 9420 section 12.2 rather than about this file.
func TestValidateProposalListRunsEveryRuleThisFileDeclares(t *testing.T) {
	declared := proposalListRulesDeclared(t)
	if !slices.Contains(declared, "ValSem108RemoveExists") {
		t.Fatalf("the signature scan read %v out of validate_proposals.go, which certainly declares ValSem108RemoveExists, so it is reading something else",
			declared)
	}
	codes := []string{}
	for _, name := range declared {
		if strings.HasPrefix(name, "ValSem") {
			codes = append(codes, name)
		}
	}
	if len(codes) != 13 {
		t.Fatalf("validate_proposals.go declares %d ValSem functions (%v), RFC 9420 section 12.2 as this plan states it is thirteen",
			len(codes), codes)
	}
	run := proposalListRules()
	for _, name := range declared {
		if !slices.Contains(run, name) {
			t.Errorf("%s is declared and ValidateProposalList runs %v, so that rule is documented and reached by nothing",
				name, run)
		}
	}
	for _, name := range run {
		if !slices.Contains(declared, name) {
			t.Errorf("ValidateProposalList runs %s and validate_proposals.go declares no rule of that name", name)
		}
	}
}

// TestEveryPerTypeViewOfAProposalListIsNamedByTheViewRule derives the view set from the type.
//
// proposalBucketsOf is four rows written by hand, and four rows written by hand is the shape that
// understates its class the moment a fifth view is added -- a psk view, when the profile widens.
// So the class is read off *ProposalList's own METHOD SET: every method answering
// []CachedProposal must be named, except the one that answers the commit order rather than a view
// of it, which is excluded by name and with its reason inside proposalListViewMethods.
//
// OFF THE METHODS AND NO LONGER OFF THE FIELDS. The views used to be exported fields and this gate
// used to read them off the struct; a list now stores the commit order alone, so a field walk
// would find one field, exclude it as the commit order, and pass over an empty class reporting a
// clean bill.
func TestEveryPerTypeViewOfAProposalListIsNamedByTheViewRule(t *testing.T) {
	named := []string{}
	for _, bucket := range proposalBucketsOf(&ProposalList{}) {
		named = append(named, bucket.accessor)
	}
	slices.Sort(named)
	answered := proposalListBucketNames(t)
	if len(answered) == 0 {
		t.Fatal("*ProposalList answers no per-type view at all, so this gate read something other than the type")
	}
	if !slices.Equal(named, answered) {
		t.Fatalf("*ProposalList answers the views %v and proposalBucketsOf names %v; a view nothing names is judged by no rule of validate_proposals.go and applied by nothing",
			answered, named)
	}
}

// TestEveryProposalValidationEntryPointRefusesANilInput drives every door of this file with
// nothing.
//
// A panic out of a library is not a refusal: it takes the caller's process rather than its call
// and says nothing about which argument was wrong. The class is the rules the production source
// declares rather than a list, and the four shapes are the four ways an input can be missing the
// thing a rule reads -- the input itself, the list, the tree, the context -- because a guard that
// only covered the outermost of them would leave the other three as dereferences.
func TestEveryProposalValidationEntryPointRefusesANilInput(t *testing.T) {
	crypto := testCrypto(t)
	tree, _ := testTreeWith(t, crypto, "alice")
	shapes := map[string]struct {
		in       *ProposalValidationInput
		sentinel error
	}{
		"a nil input":    {nil, errNilProposalValidationInput},
		"no list":        {&ProposalValidationInput{Tree: tree, Context: &GroupContext{}}, errNilProposalList},
		"no tree":        {&ProposalValidationInput{List: &ProposalList{}, Context: &GroupContext{}}, errNilRatchetTree},
		"no context":     {&ProposalValidationInput{List: &ProposalList{}, Tree: tree}, ErrNilGroupContext},
		"nothing at all": {&ProposalValidationInput{}, errNilProposalList},
	}
	doors := map[string]func(*ProposalValidationInput) error{"ValidateProposalList": ValidateProposalList}
	for _, name := range proposalListRules() {
		doors[name] = proposalListRuleFor(t, name)
	}
	if len(doors) < 14 {
		t.Fatalf("the sweep found %d doors, and this file declares thirteen ValSem codes plus the aggregate at least", len(doors))
	}
	for _, name := range slices.Sorted(maps.Keys(doors)) {
		for _, shape := range slices.Sorted(maps.Keys(shapes)) {
			answered := proposalValidationRefusalOf(t, name+" with "+shape, doors[name], shapes[shape].in)
			if !errors.Is(answered, shapes[shape].sentinel) {
				t.Errorf("%s with %s answered %v, want %v", name, shape, answered, shapes[shape].sentinel)
			}
		}
	}
}

// proposalValidationRefusalOf runs one door and turns a panic into the error it should have been,
// so one door dereferencing its input is a failure of its own case rather than the end of the
// sweep.
func proposalValidationRefusalOf(t *testing.T, what string,
	door func(*ProposalValidationInput) error, in *ProposalValidationInput) (answered error) {
	t.Helper()
	defer func() {
		if panicked := recover(); panicked != nil {
			t.Errorf("%s panicked with %v; a panic out of a library takes the caller's process rather than its call",
				what, panicked)
			answered = nil
		}
	}()
	return door(in)
}

// ---------------------------------------------------------------------------
// the positive half, and the rules whose fixtures are worth reading on their own
// ---------------------------------------------------------------------------

// TestValidateProposalListAcceptsAValidList is the control every negative row above is measured
// against: with none of the twenty one rules broken, the aggregate accepts.
func TestValidateProposalListAcceptsAValidList(t *testing.T) {
	crypto := testCrypto(t)
	tree, members := testTreeWith(t, crypto, "alice", "bob", "carol")
	kp, _, _ := testKeyPackage(t, crypto, testIdentity(t, crypto, "dave"))
	update, _ := testUpdateProposalOf(t, crypto, members[testUpdatedLeaf], testUpdatedLeaf)
	list := testProposalList(t, update, testRemoveOf(2), testAddOf(kp))
	if err := ValidateProposalList(testValidationInput(t, crypto, tree, testCommitterLeaf, list)); err != nil {
		t.Fatalf("ValidateProposalList: %v", err)
	}
}

// TestValSem101AcceptsAnAddOfAMemberTheSameListRemoves is RFC 9420 section 12.2's "unless there is
// a Remove proposal in the list removing the matching client from the group".
//
// It is the clause that makes a device replacing itself in one commit legal, and it is invisible
// to the duplicate test next to it: a ValSem101 written without the exemption refuses this list,
// and every OTHER test of ValSem101 passes over that version.
func TestValSem101AcceptsAnAddOfAMemberTheSameListRemoves(t *testing.T) {
	crypto := testCrypto(t)
	tree, members := testTreeWith(t, crypto, "alice", "bob")
	// bob's own signature key, arriving again in a key package, beside a remove of bob's leaf
	kp, _, _ := testKeyPackage(t, crypto, members[1])
	list := testProposalList(t, testRemoveOf(1), testAddOf(kp))
	in := testValidationInput(t, crypto, tree, testCommitterLeaf, list)
	if err := ValSem101UniqueSignatureKey(in); err != nil {
		t.Fatalf("ValSem101 over a rejoin refused it: %v", err)
	}
	// and the same list without the remove is the refusal, which is what says the exemption is
	// doing the work rather than the fixture being toothless
	without := testProposalList(t, testAddOf(kp))
	if err := ValSem101UniqueSignatureKey(testValidationInput(t, crypto, tree, testCommitterLeaf, without)); !errors.Is(err, ErrAddDuplicateSignatureKey) {
		t.Fatalf("ValSem101 over the same add with no remove answered %v, want %v", err, ErrAddDuplicateSignatureKey)
	}
}

// TestValSem110ExcludesAnUpdatingLeafsOwnOutgoingKey is the other half of ValSem110's members'
// loop.
//
// A leaf's outgoing encryption key is exactly the key its own update replaces, so a members' loop
// that did not exclude the leaves this list updates would refuse every update that republished
// anything -- and the duplicate row above passes over that version, because a key collision is a
// key collision whichever leaf it came from.
func TestValSem110ExcludesAnUpdatingLeafsOwnOutgoingKey(t *testing.T) {
	crypto := testCrypto(t)
	tree, members := testTreeWith(t, crypto, "alice", "bob")
	leaf, _ := testLeafNode(t, crypto, members[1])
	// bob republishing the key bob's leaf already holds: legal, because that leaf is being
	// replaced by this very proposal
	leaf.EncryptionKey = tree.Leaf(1).EncryptionKey
	in := testValidationInput(t, crypto, tree, testCommitterLeaf, testProposalList(t, testUpdateOf(1, leaf)))
	if err := ValSem110UpdateUniqueEncryptionKey(in); err != nil {
		t.Fatalf("ValSem110 refused a leaf republishing its own outgoing key: %v", err)
	}
}

// TestValSem106ReadsTheExtensionsAGroupContextExtensionsProposalInTheSameListAdds is RFC 9420
// section 12.3's "The new extensions MUST be used when evaluating other proposals in this list".
//
// The group context here requires nothing, so a ValSem106 reading only the context accepts; the
// list's own GCE proposal is what makes the add illegal, and that is the whole rule.
func TestValSem106ReadsTheExtensionsAGroupContextExtensionsProposalInTheSameListAdds(t *testing.T) {
	crypto := testCrypto(t)
	tree, _ := testTreeWith(t, crypto, "alice")
	kp, _, _ := testKeyPackage(t, crypto, testIdentity(t, crypto, "dave"))
	list := testProposalList(t, testGceOf(testRequiredCapabilitiesExtension(t)), testAddOf(kp))
	in := testValidationInput(t, crypto, tree, testCommitterLeaf, list)
	if err := ValSem106RequiredCapabilitiesSatisfied(in); !errors.Is(err, ErrAddMissingRequiredCapability) {
		t.Fatalf("ValSem106 answered %v, want %v; the requirement this commit adds is not being read",
			err, ErrAddMissingRequiredCapability)
	}
	// the control: the same add with no GCE beside it is accepted, so the refusal above is the
	// proposal's doing rather than the fixture's
	if err := ValSem106RequiredCapabilitiesSatisfied(testValidationInput(t, crypto, tree,
		testCommitterLeaf, testProposalList(t, testAddOf(kp)))); err != nil {
		t.Fatalf("ValSem106 refused an add under a group requiring nothing: %v", err)
	}
}

// TestRequiredCapabilitiesThatCannotBeReadIsARefusalRatherThanNoRequirement is the one place this
// file departs from the plan's sketch, and it is a security departure rather than a taste one.
//
// The sketch answers "(nil, false)" for a required_capabilities whose body does not decode and for
// a vector carrying the extension twice, which reads as "the group requires nothing" -- so a
// malformed extension would admit a member who supports none of it, and would be strictly better
// for an attacker than a well formed one.
func TestRequiredCapabilitiesThatCannotBeReadIsARefusalRatherThanNoRequirement(t *testing.T) {
	crypto := testCrypto(t)
	tree, _ := testTreeWith(t, crypto, "alice")
	kp, _, _ := testKeyPackage(t, crypto, testIdentity(t, crypto, "dave"))
	list := testProposalList(t, testAddOf(kp))
	for name, exts := range map[string][]Extension{
		"a body that does not decode": {{ExtensionType: ExtensionTypeRequiredCapabilities,
			ExtensionData: []byte{0xff, 0xff, 0xff}}},
		"the extension twice": {testRequiredCapabilitiesExtension(t), testRequiredCapabilitiesExtension(t)},
	} {
		in := testValidationInput(t, crypto, tree, testCommitterLeaf, list)
		in.Context.Extensions = exts
		for rule, run := range map[string]func(*ProposalValidationInput) error{
			"ValSem106": ValSem106RequiredCapabilitiesSatisfied,
			"ValSem109": ValSem109UpdateRequiredCapabilities,
			// the update door reads the same extension for the same reason, and answers the
			// same refusal ahead of any leaf: a group whose required_capabilities cannot be
			// read is one no leaf can be held to
			"validateUpdateLeafNodeIsValidForAnUpdate": validateUpdateLeafNodeIsValidForAnUpdate,
		} {
			if err := run(in); !errors.Is(err, ErrMalformedExtension) {
				t.Errorf("%s over %s answered %v, want %v", rule, name, err, ErrMalformedExtension)
			}
		}
	}
}

// TestValSem113AnswersTheProfileGatesOwnValueForEveryTypeItRefuses says the delegation is complete
// rather than covering the one type a single fixture happens to use.
//
// The class is READ OFF proposalTypeProfile rather than listed, which matters because the point of
// ValSem113 is that there is exactly one table of what the v1 profile accepts: a fixture list of
// three or four types would be a second, shorter statement of that table, and the first code point
// whose disposition changed would leave the two disagreeing with nothing to say so. The profile
// table is itself held to the registry in both directions by
// TestTheV1ProfileClassifiesEveryRegisteredProposalType, so this is derived from a derived class.
//
// No arm is built for any row, and that is not laziness: checkProposalProfile judges the TYPE
// before it looks at the arm, so a refused type is refused whatever it carries -- and a row that
// supplied the matching arm would be asserting the same thing through more fixture. The
// unregistered code point is the one row the table cannot supply, because a table of the registry
// has no member outside it.
func TestValSem113AnswersTheProfileGatesOwnValueForEveryTypeItRefuses(t *testing.T) {
	crypto := testCrypto(t)
	tree, _ := testTreeWith(t, crypto, "alice")
	answered := func(proposal Proposal) error {
		return ValSem113ProposalTypeSupported(testValidationInput(t, crypto, tree, testCommitterLeaf,
			NewProposalList([]CachedProposal{{Proposal: proposal}})))
	}
	refused := 0
	for _, code := range slices.Sorted(maps.Keys(proposalTypeProfile)) {
		want := proposalTypeProfile[code]
		if want == nil {
			// a type this profile accepts. It is refused here anyway, by the arm check,
			// which is a different rule and is why nothing is asserted about its value.
			continue
		}
		refused += 1
		if err := answered(Proposal{ProposalType: code}); !errors.Is(err, want) {
			t.Errorf("ValSem113 over a %s proposal answered %v, and the v1 profile refuses that code point with %v",
				proposalTypeName(code), err, want)
		}
	}
	if refused == 0 {
		t.Fatal("the v1 profile table refuses no registered proposal type at all, so this sweep asserted nothing")
	}
	// and a code point outside the registry, which is the one member of the class the table
	// cannot hold
	if err := answered(Proposal{ProposalType: ProposalType(0x1A1A), UnknownBody: []byte{1}}); !errors.Is(err, errUnregisteredProposalType) {
		t.Errorf("ValSem113 over an unregistered code point answered %v, want %v", err, errUnregisteredProposalType)
	}
	t.Logf("%d registered proposal types the v1 profile refuses, each answered through ValSem113", refused)
}

// TestAnArmlessAddIsRefusedEvenWhenAnInnocentAddPrecedesIt is what makes the twelve rules below it
// safe to write.
//
// IT USED TO SAY "ValSem113 READS THE BUCKETS AND NOT ONLY THE COMMIT ORDER", and both halves of
// that name have had to go. A ProposalList once carried its per-type views as writable fields, so
// a proposal reachable through a view and absent from the commit order was a real input, judged by
// no arm check at all and dereferenced by the rules reading that view; a view is now the order
// filtered, so there is no such input and no bucket half to read. And the rule is not named,
// because no test can say which guard fired: every door of this file opens with in.check, which
// reaches checkProposalListStructure over the same order, and NARROWING ValSem113's OWN LOOP TO
// THE FIRST PROPOSAL LEAVES THE WHOLE SUITE GREEN. That is measured, it is the position
// ValidateProposalList's header already takes about its own redundant door check, and a test name
// claiming otherwise would be the claim rather than the observation.
//
// What IS driven is the input: an Add with a nil Add arm, BEHIND an innocent add, is refused
// before any rule reaches `add.KeyPackage`. Behind an innocent add of its own type, which is what
// this adds to TestNoDoorHandedAProposalListDereferencesAMissingArm one file over -- that sweep
// puts its armless entry behind an innocent REMOVE, so the adds view there answers one entry and
// a guard narrowed to the first entry of a view passes it.
func TestAnArmlessAddIsRefusedEvenWhenAnInnocentAddPrecedesIt(t *testing.T) {
	crypto := testCrypto(t)
	tree, _ := testTreeWith(t, crypto, "alice")
	kp, _, _ := testKeyPackage(t, crypto, testIdentity(t, crypto, "carol"))
	armless := CachedProposal{Proposal: Proposal{ProposalType: ProposalTypeAdd}}
	list := NewProposalList([]CachedProposal{testAddOf(kp), armless})
	// the fixture is only worth driving if the armless entry is one the adds view answers
	if adds := list.Adds(); len(adds) != 2 {
		t.Fatalf("the adds view answers %d of the two adds in the commit order", len(adds))
	}
	in := testValidationInput(t, crypto, tree, testCommitterLeaf, list)
	if err := ValSem113ProposalTypeSupported(in); err == nil {
		t.Fatal("the type door accepted an add with no add arm behind an innocent one")
	}
	// and the aggregate refuses it rather than dereferencing it
	if err := proposalValidationRefusalOf(t, "ValidateProposalList over an armless add",
		ValidateProposalList, in); err == nil {
		t.Fatal("ValidateProposalList accepted an add with no add arm")
	}
}

// ---------------------------------------------------------------------------
// the update door: RFC 9420 section 12.1.2 and what it reaches of section 7.3
// ---------------------------------------------------------------------------

// updateDoorFault is one way an Update's LeafNode can be invalid for an update, the section 7.3
// clause it breaks, and the value that clause answers underneath ErrUpdateLeafNodeInvalid.
type updateDoorFault struct {
	// the section 7.3 clause, for the failure text.
	clause string
	// the sentinel the clause answers. It is asked THROUGH the wrap, which is what
	// ErrUpdateLeafNodeInvalid's own declaration promises a caller: which proposal is bad, and
	// what section 7.3 said about it.
	inner error
	// alsoRefusedBy names every OTHER production rule of this file that refuses this row's list,
	// and it is empty for almost every row because that emptiness IS the table's claim: a leaf
	// no other rule of section 12.2 objects to is a leaf that reached the tree unjudged before
	// this door existed. The set is compared exactly rather than merely permitted, so a rule
	// that starts or stops sharing a row fails here instead of widening the claim in silence.
	alsoRefusedBy []string
	build         func(t *testing.T, crypto CryptoProvider) *ProposalValidationInput
}

// updateDoorFaults is one input per clause of section 7.3 that (*LeafNode).Validate can decide
// about an update leaf, each built so that EVERY other rule of this file accepts it.
//
// That last part is the whole content of the table and is asserted rather than described. Before
// the update door existed, ValSem109 read the update leaf's Capabilities and ValSem110 read its
// EncryptionKey and nothing read anything else about it -- so each row here is a leaf that reached
// (*RatchetTree).UpdateLeaf with no signature check, no leaf_node_source check, no credential
// check and no section 13.4 group extension check. The rows are not a list of everything section
// 7.3 says: they are one input per clause of it a caller of this door can reach, and the clauses
// that need the leaf being replaced or every other leaf of the group are named on
// LeafValidationContext as belonging elsewhere.
func updateDoorFaults() map[string]updateDoorFault {
	return map[string]updateDoorFault{
		// the headline. A key_package leaf carries a lifetime, is signed with NO group id and NO
		// leaf index, and therefore verifies in whatever group it is pasted into at whatever
		// position it is moved to -- which is exactly what an update leaf must not be.
		"a key_package leaf offered as an update": {
			clause: "the leaf_node_source rule", inner: ErrLeafNodeSourceMismatch,
			build: func(t *testing.T, crypto CryptoProvider) *ProposalValidationInput {
				tree, members := testTreeWith(t, crypto, "alice", "bob")
				leaf, _ := testLeafNode(t, crypto, members[testUpdatedLeaf])
				return testValidationInput(t, crypto, tree, testCommitterLeaf,
					testProposalList(t, testUpdateOf(testUpdatedLeaf, leaf)))
			}},
		"a leaf another member signed": {
			clause: "the signature rule", inner: errBadSignature,
			build: func(t *testing.T, crypto CryptoProvider) *ProposalValidationInput {
				tree, members := testTreeWith(t, crypto, "alice", "bob")
				_, leaf := testUpdateProposalOf(t, crypto, members[testUpdatedLeaf], testUpdatedLeaf)
				return testValidationInput(t, crypto, tree, testCommitterLeaf, testProposalList(t,
					testResignedUpdateOf(t, crypto, members[testCommitterLeaf], testUpdatedLeaf, leaf)))
			}},
		// the leaf index half of the section 7.2 context select: the updated member's own leaf,
		// its own signature, signed at the committer's index and offered at its own
		"a leaf signed at another index": {
			clause: "the signature rule, over the leaf index it is bound to", inner: errBadSignature,
			build: func(t *testing.T, crypto CryptoProvider) *ProposalValidationInput {
				tree, members := testTreeWith(t, crypto, "alice", "bob")
				leaf, _ := testUpdateLeafNode(t, crypto, members[testUpdatedLeaf], testValidationGroupId(),
					testCommitterLeaf)
				return testValidationInput(t, crypto, tree, testCommitterLeaf,
					testProposalList(t, testUpdateOf(testUpdatedLeaf, leaf)))
			}},
		// and the group id half of it
		"a leaf signed in another group": {
			clause: "the signature rule, over the group id it is bound to", inner: errBadSignature,
			build: func(t *testing.T, crypto CryptoProvider) *ProposalValidationInput {
				tree, members := testTreeWith(t, crypto, "alice", "bob")
				leaf, _ := testUpdateLeafNode(t, crypto, members[testUpdatedLeaf], []byte("another group"), testUpdatedLeaf)
				return testValidationInput(t, crypto, tree, testCommitterLeaf,
					testProposalList(t, testUpdateOf(testUpdatedLeaf, leaf)))
			}},
		// x509's registered code point, which testCapabilities does not list. The leaf is NOT
		// re-signed and does not need to be: section 7.3 puts the credential rule ahead of the
		// signature rule and (*LeafNode).Validate takes that order, which is the only order under
		// which this clause can ever fire -- Credential.MarshalMLS refuses every type outside this
		// profile, so a leaf carrying one cannot have a preimage built for it at all.
		//
		// Emptying capabilities.credentials instead would assert nothing: SupportsCredential
		// answers true for basic unconditionally, so a basic leaf reaches this clause whatever its
		// vector says. Measured -- that fixture left this row green over a door that refused
		// nothing.
		"a leaf whose credential type is not in its own capabilities": {
			clause: "section 7.2's credential type rule", inner: errCredentialTypeNotListed,
			build: func(t *testing.T, crypto CryptoProvider) *ProposalValidationInput {
				tree, members := testTreeWith(t, crypto, "alice", "bob")
				_, leaf := testUpdateProposalOf(t, crypto, members[testUpdatedLeaf], testUpdatedLeaf)
				leaf.Credential.CredentialType = CredentialType(0x0002)
				return testValidationInput(t, crypto, tree, testCommitterLeaf,
					testProposalList(t, testUpdateOf(testUpdatedLeaf, leaf)))
			}},
		"a leaf that does not list the group's ciphersuite": {
			clause: "section 11.1's ciphersuite rule", inner: errCipherSuiteNotListed,
			build: func(t *testing.T, crypto CryptoProvider) *ProposalValidationInput {
				tree, members := testTreeWith(t, crypto, "alice", "bob")
				_, leaf := testUpdateProposalOf(t, crypto, members[testUpdatedLeaf], testUpdatedLeaf)
				leaf.Capabilities.CipherSuites = []CipherSuite{}
				return testValidationInput(t, crypto, tree, testCommitterLeaf, testProposalList(t,
					testResignedUpdateOf(t, crypto, members[testUpdatedLeaf], testUpdatedLeaf, leaf)))
			}},
		"a leaf carrying an extension it does not list": {
			clause: "the leaf extension rule", inner: errLeafExtensionNotListed,
			build: func(t *testing.T, crypto CryptoProvider) *ProposalValidationInput {
				tree, members := testTreeWith(t, crypto, "alice", "bob")
				_, leaf := testUpdateProposalOf(t, crypto, members[testUpdatedLeaf], testUpdatedLeaf)
				leaf.Extensions = append(leaf.Extensions,
					Extension{ExtensionType: ExtensionType(0xF00A), ExtensionData: []byte{1}})
				return testValidationInput(t, crypto, tree, testCommitterLeaf, testProposalList(t,
					testResignedUpdateOf(t, crypto, members[testUpdatedLeaf], testUpdatedLeaf, leaf)))
			}},
		// MASTER section 5.3's range check, which (*LeafNode).Validate makes on the whole entry
		// rather than on its body. An update is how a member changes its device keys, so a
		// malformed urmessage_leaf_keys body arriving by Update is the ordinary way this clause is
		// reached at all -- and it reached the tree unread.
		"a leaf whose urmessage_leaf_keys body does not parse": {
			clause: "the leaf keys range check", inner: ErrLeafKeysExtensionInvalid,
			build: func(t *testing.T, crypto CryptoProvider) *ProposalValidationInput {
				tree, members := testTreeWith(t, crypto, "alice", "bob")
				_, leaf := testUpdateProposalOf(t, crypto, members[testUpdatedLeaf], testUpdatedLeaf)
				leaf.Extensions[0].ExtensionData = []byte{0xff, 0xff}
				return testValidationInput(t, crypto, tree, testCommitterLeaf, testProposalList(t,
					testResignedUpdateOf(t, crypto, members[testUpdatedLeaf], testUpdatedLeaf, leaf)))
			}},
		// the argument rule, which is a refusal of this door like any other: every rule of this
		// file is reachable with a provider nobody supplied, and a validator that dereferenced one
		// would take the caller's process rather than its call
		"a validation input carrying no crypto provider": {
			clause: "the provider rule", inner: ErrNilCryptoProvider,
			build: func(t *testing.T, crypto CryptoProvider) *ProposalValidationInput {
				tree, members := testTreeWith(t, crypto, "alice", "bob")
				update, _ := testUpdateProposalOf(t, crypto, members[testUpdatedLeaf], testUpdatedLeaf)
				in := testValidationInput(t, crypto, tree, testCommitterLeaf, testProposalList(t, update))
				in.Crypto = nil
				return in
			}},
		// erratum 8745 at the list level, and the row this door exists for as much as the source
		// row does. ERRATA.md says this package applies section 13.4's group extension rule to
		// all three sources; that was true of (*LeafNode).Validate and, for an update leaf,
		// reached by nobody. ValSem109 cannot answer it: it asks only about
		// required_capabilities, and this group context carries none.
		"a leaf that does not support an extension the group context carries": {
			clause: "section 13.4 as corrected by erratum 8745", inner: errGroupContextExtensionNotListed,
			build: func(t *testing.T, crypto CryptoProvider) *ProposalValidationInput {
				tree, members := testTreeWith(t, crypto, "alice", "bob")
				update, _ := testUpdateProposalOf(t, crypto, members[testUpdatedLeaf], testUpdatedLeaf)
				in := testValidationInput(t, crypto, tree, testCommitterLeaf, testProposalList(t, update))
				in.Context.Extensions = []Extension{
					{ExtensionType: ExtensionType(0xF00A), ExtensionData: []byte{1}}}
				return in
			}},
		// errProfileCredentialType, which is raised inside the SIGNATURE PREIMAGE rather than by
		// a clause of Validate at all: Credential.MarshalMLS refuses every credential type
		// outside this profile, marshalCore builds the preimage through it, and VerifySignature
		// hands that failure back as itself. It escaped this table entirely for as long as the
		// refusal class was bounded by three method names all declared in leaf_node.go.
		//
		// The leaf is NOT re-signed, and cannot be -- Sign goes through the same
		// Credential.MarshalMLS and would fail in the fixture instead of at the door. It does
		// not need to be: the preimage cannot be built, so no signature comparison is reached.
		// The type IS listed in the leaf's own capabilities, and that is what separates this row
		// from the one above: without it SupportsCredential refuses first and this becomes a
		// second spelling of errCredentialTypeNotListed.
		"a leaf whose credential type is outside this profile": {
			clause: "section 5.3.1's credential rule, the half this profile can decide",
			inner:  errProfileCredentialType,
			build: func(t *testing.T, crypto CryptoProvider) *ProposalValidationInput {
				tree, members := testTreeWith(t, crypto, "alice", "bob")
				_, leaf := testUpdateProposalOf(t, crypto, members[testUpdatedLeaf], testUpdatedLeaf)
				leaf.Credential.CredentialType = CredentialType(0x0002)
				leaf.Capabilities.Credentials = append(leaf.Capabilities.Credentials,
					CredentialType(0x0002))
				return testValidationInput(t, crypto, tree, testCommitterLeaf,
					testProposalList(t, testUpdateOf(testUpdatedLeaf, leaf)))
			}},
		// errMissingRequiredCapability, which is Capabilities.Supports' own value: the fifth
		// return site of the sentinel the four clauses above WRAP, and the only one outside
		// leaf_node.go. It is the second refusal the transitive derivation found, and it is the
		// one row of this table another rule of the file also refuses -- ValSem109 states the
		// required_capabilities clause at the list level with a value of its own, which is why
		// the aggregate answers ValSem109's value here and not this door's.
		//
		// It is a row rather than an admitted reason because it IS reachable through this door
		// with a real list, which is the thing worth writing down: a reader who found it only in
		// ValSem109 would conclude the leaf validator's own capability check is unreachable.
		"a leaf that does not support a capability the group requires": {
			clause:        "section 7.3's required_capabilities rule",
			inner:         errMissingRequiredCapability,
			alsoRefusedBy: []string{"ValSem109UpdateRequiredCapabilities"},
			build: func(t *testing.T, crypto CryptoProvider) *ProposalValidationInput {
				tree, members := testTreeWith(t, crypto, "alice", "bob")
				update, _ := testUpdateProposalOf(t, crypto, members[testUpdatedLeaf], testUpdatedLeaf)
				in := testValidationInput(t, crypto, tree, testCommitterLeaf, testProposalList(t, update))
				in.Context.Extensions = []Extension{testRequiredCapabilitiesExtension(t)}
				return in
			}},
	}
}

// TestTheUpdateDoorRefusesWhatEveryOtherUpdateRuleOfThisFileReadsPast is the defect this door was
// written for, stated one section 7.3 clause at a time.
//
// FOUR CLAIMS PER ROW, and the middle two are the ones that make this a test about a missing door
// rather than about a validator that already existed:
//
//   - the door answers ErrUpdateLeafNodeInvalid, and answers the clause's own sentinel THROUGH it,
//     which is what the wrap promises a caller.
//   - EVERY OTHER RULE OF THIS FILE THAT READS AN UPDATE ACCEPTS THE SAME LIST. ValSem109 reads
//     the leaf's Capabilities, ValSem110 its EncryptionKey, ValSem111 and ValSem112 its sender,
//     and the encryption key rule the leaf it replaces -- and not one of them is a section 7.3
//     validator. The rules are read off the production groups rather than named here, so a
//     fourteenth update rule added later is swept without anybody editing this.
//   - ApplyProposals INSTALLS the leaf. That is the consequence: RFC 9420 section 12.3 application
//     is not a validator, and until this door existed nothing between the wire and
//     (*RatchetTree).UpdateLeaf judged an update's leaf at all.
//   - the aggregate answers the same value, so the door is reached with this input rather than
//     tripping over an earlier rule.
func TestTheUpdateDoorRefusesWhatEveryOtherUpdateRuleOfThisFileReadsPast(t *testing.T) {
	crypto := testCrypto(t)
	faults := updateDoorFaults()
	if len(faults) == 0 {
		t.Fatal("no fault is written down, so this sweep asserted nothing")
	}
	for _, name := range slices.Sorted(maps.Keys(faults)) {
		fault := faults[name]
		t.Run(name, func(t *testing.T) {
			in := fault.build(t, crypto)
			answered := validateUpdateLeafNodeIsValidForAnUpdate(in)
			if !errors.Is(answered, ErrUpdateLeafNodeInvalid) {
				t.Fatalf("the update door answered %v over %s, want %v", answered, name, ErrUpdateLeafNodeInvalid)
			}
			if !errors.Is(answered, fault.inner) {
				t.Errorf("the update door answered %v and %s is refused by %s, whose value is %v; a caller cannot ask which section 7.3 clause fired",
					answered, name, fault.clause, fault.inner)
			}
			// every other rule the aggregate runs, so this is one door's refusal and not a
			// second spelling of somebody else's. The set is compared EXACTLY against what the
			// row says rather than merely required to be empty, so a rule that starts sharing a
			// row, or stops, fails here instead of quietly changing what the row proves.
			refusedElsewhere := []string{}
			for _, other := range proposalListRules() {
				if other == "validateUpdateLeafNodeIsValidForAnUpdate" {
					continue
				}
				if err := proposalListRuleFor(t, other)(in); err != nil {
					refusedElsewhere = append(refusedElsewhere, other)
					t.Logf("%s also refuses %s, with %v", other, name, err)
				}
			}
			slices.Sort(refusedElsewhere)
			shared := slices.Clone(fault.alsoRefusedBy)
			slices.Sort(shared)
			if !slices.Equal(refusedElsewhere, shared) {
				t.Errorf("%s is also refused by %v and the row says %v; a row with nothing written there is a leaf every other rule of this file accepts, which is what makes it evidence that this door was missing",
					name, refusedElsewhere, shared)
			}
			// the consequence: with no door, the leaf goes into the tree
			applied, err := ApplyProposals(in.Tree, in.Context, in.Committer, in.List)
			if err != nil {
				t.Fatalf("ApplyProposals refused %s: %v; section 12.3 application is not a validator and this row is written to reach the tree",
					name, err)
			}
			installed := applied.Tree.Leaf(in.List.Updates()[0].Sender)
			if installed == nil {
				t.Fatalf("applying %s left leaf %d blank", name, in.List.Updates()[0].Sender)
			}
			if !bytes.Equal(installed.EncryptionKey, in.List.Updates()[0].Proposal.Update.LeafNode.EncryptionKey) {
				t.Fatalf("applying %s did not install the update's own leaf, so this row does not show what an unjudged update reaches",
					name)
			}
			aggregated := ValidateProposalList(in)
			if len(fault.alsoRefusedBy) == 0 {
				if !errors.Is(aggregated, ErrUpdateLeafNodeInvalid) {
					t.Fatalf("ValidateProposalList over %s answered %v, want %v; the aggregate is reaching a different rule with this input",
						name, aggregated, ErrUpdateLeafNodeInvalid)
				}
				return
			}
			// a row another rule also refuses reaches the aggregate through whichever of the two
			// runs first, so what the aggregate owes here is a refusal rather than this door's
			// value. Saying that plainly is better than asserting a running order the groups
			// above are free to change.
			if aggregated == nil {
				t.Fatalf("ValidateProposalList accepted %s, which this door and %v each refuse",
					name, fault.alsoRefusedBy)
			}
		})
	}
}

// The class of refusals (*LeafNode).Validate can answer is derived in
// leaf_validation_doors_test.go, by leafValidationRefusalNames, off the TRANSITIVE CLOSURE of the
// calls that validator makes.
//
// It used to be derived here, off three method names typed out beside a comment calling them
// "every refusal (*LeafNode).Validate can answer" -- Validate, VerifySignature, validateLifetime.
// Validate delegates to five, and the two that escaped that list were both then measured reachable
// through this very door with real inputs: errProfileCredentialType from Credential.MarshalMLS,
// reached from inside the signature preimage, and errMissingRequiredCapability from
// Capabilities.Supports. A gate written to enforce rule 5 had enumerated its own scope, which is
// how the table below came to assert nine clauses of section 7.3 and call that all of them.

// leafValidationRefusals joins those names to the values, so a row can be said to cover one.
//
// Written here and held to the derived names in BOTH directions by the gate below, which is the
// shape proposalValidationOwnedErrors already takes: a class computed from the same source it is
// judging agrees with that source whatever the source says, and a class typed out beside it goes
// stale in silence. Neither alone is worth anything; the two held against each other are.
var leafValidationRefusals = map[string]error{
	"ErrNilCryptoProvider":              ErrNilCryptoProvider,
	"ErrLeafNodeSourceMismatch":         ErrLeafNodeSourceMismatch,
	"errCredentialTypeNotListed":        errCredentialTypeNotListed,
	"errBadSignature":                   errBadSignature,
	"errCipherSuiteNotListed":           errCipherSuiteNotListed,
	"errLeafExtensionNotListed":         errLeafExtensionNotListed,
	"ErrLeafKeysExtensionInvalid":       ErrLeafKeysExtensionInvalid,
	"errGroupContextExtensionNotListed": errGroupContextExtensionNotListed,
	"ErrLeafNodeLifetime":               ErrLeafNodeLifetime,

	// the four the derivation gained when it stopped being bounded by three method names. The
	// first two are refusals raised in bodies leaf_node.go does not declare -- one inside the
	// signature preimage, one inside the capability check -- and both are reachable through
	// this door with a real proposal list, which is what the two rows below build. The last two
	// are reachable by no input here and say so next door.
	"errProfileCredentialType":         errProfileCredentialType,
	"errMissingRequiredCapability":     errMissingRequiredCapability,
	"ErrTreeMalformed":                 ErrTreeMalformed,
	"errLeafSourceRuleWaivedAndStated": errLeafSourceRuleWaivedAndStated,
}

// updateDoorClausesNoInputCanReach is the refusal of that class no input at THIS door can produce,
// with the reason -- and it is the conclusion this task was asked to reach, written where it can be
// checked rather than in a comment.
//
// Section 7.3's lifetime rule is the one clause an update leaf owes nothing to. The lifetime is a
// variant field carried only under key_package, so validateLifetime returns before reading it for
// every other source; a leaf whose source is key_package is refused by the leaf_node_source rule
// first, so no input that reaches the lifetime clause reaches it AS AN UPDATE. What section 7.3
// puts in its place is the update arm of the leaf_node_source rule, which is
// validateUpdateChangesTheEncryptionKey's, and which is a rule about two leaves rather than a field
// of the context.
//
// TestTheUpdateDoorDoesNotJudgeALifetimeAnUpdateLeafDoesNotCarry asserts that rather than trusting
// it, in both directions: an update leaf carrying an expired lifetime is accepted here, and the
// same leaf under the key_package source with the same clock is refused.
var updateDoorClausesNoInputCanReach = map[string]string{
	"ErrLeafNodeLifetime": "the lifetime is a variant field only a key_package leaf carries, and a key_package leaf is refused by the leaf_node_source rule before validateLifetime is reached",

	// the two the transitive derivation added that no proposal can reach. Both are about the
	// SHAPE of the call rather than about the leaf, which is why no list of proposals produces
	// them: this door writes its own context and hands Validate a source the RFC defines.
	"ErrTreeMalformed":                 "marshalCore and signatureContent have no arm for a source this package does not read, and this door expects update -- so a leaf carrying a fourth source is refused by the leaf_node_source rule before either arm is reached. What reaches them is a caller that WAIVES that rule, which is (*RatchetTree).validateLeaves and not any proposal list",
	"errLeafSourceRuleWaivedAndStated": "it is a refusal about the LeafValidationContext and not about the leaf: this door states an expectation and waives nothing, so the contradiction it names cannot be built out of a proposal list at all. leaf_validation_doors_test.go is what holds every call site to setting exactly one of the two fields",
}

// TestEveryRefusalTheLeafValidatorCanAnswerHasAnUpdateDoorRowOrAnAdmittedReason is the gate that
// stops updateDoorFaults from being the clauses somebody remembered.
//
// A table of section 7.3 clauses written by hand is exactly the shape this commit was sent to close
// one level up: the package named three callers of (*LeafNode).Validate in a comment, wrote two,
// and nothing reported the third. So the class is derived off the validator's own source, the table
// is held to it, and a refusal added to (*LeafNode).Validate by a later task fails here until
// somebody either builds an input that reaches it or writes down why none can.
//
// A ROW COVERS A REFUSAL BY IDENTITY AND NOT BY errors.Is, and that is a correction rather than a
// nicety. Four of this class WRAP errMissingRequiredCapability -- leaf_node.go's own header says
// so, and says why: with one sentinel behind five return sites, every assertion in this area reads
// errors.Is and any of the five satisfies it, "so no test can say that the rule it is named for is
// the rule that fired". Under errors.Is the broad value was covered by whichever wrapping row
// sorted first, and Capabilities.Supports' own three loops -- the fifth site, and the only one
// outside leaf_node.go -- were reached by no row at all while the gate reported a clean bill.
func TestEveryRefusalTheLeafValidatorCanAnswerHasAnUpdateDoorRowOrAnAdmittedReason(t *testing.T) {
	derived := leafValidationRefusalNames(t)
	// the positive control: a scan that resolved nothing reports the clean bill a complete one
	// reports, and Validate certainly names the source rule's own value
	if !slices.Contains(derived, "ErrLeafNodeSourceMismatch") {
		t.Fatalf("the scan read %v out of leaf_node.go, which certainly names ErrLeafNodeSourceMismatch, so it is reading something other than the validator",
			derived)
	}
	if got := slices.Sorted(maps.Keys(leafValidationRefusals)); !slices.Equal(got, derived) {
		t.Fatalf("the closure of (*LeafNode).Validate names the refusals %v and leafValidationRefusals holds %v; the class this gate sweeps is the second, so a name in the first and not the second is a section 7.3 clause nothing here covers",
			derived, got)
	}
	rows := updateDoorFaults()
	for _, name := range derived {
		value := leafValidationRefusals[name]
		covered := ""
		for _, row := range slices.Sorted(maps.Keys(rows)) {
			if rows[row].inner == value {
				covered = row
				break
			}
		}
		reason, admitted := updateDoorClausesNoInputCanReach[name]
		switch {
		case covered != "" && admitted:
			t.Errorf("%s is reached by the %q row and is also written down as reachable by no input, with the reason %q; one of the two is stale",
				name, covered, reason)
		case covered == "" && !admitted:
			t.Errorf("(*LeafNode).Validate can answer %s and no row of updateDoorFaults builds an input that reaches it, so the update door's coverage of section 7.3 is asserted over %d clauses and not over that one",
				name, len(rows))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(updateDoorClausesNoInputCanReach)) {
		if _, isRefusal := leafValidationRefusals[name]; !isRefusal {
			t.Errorf("the admitted reasons name %s and the leaf validator answers no refusal of that name", name)
		}
	}
	// and every row is about a refusal the validator can actually answer, which is the other
	// direction: a row asserting a sentinel this door cannot produce would pass its own case by
	// never having been reached
	for _, row := range slices.Sorted(maps.Keys(rows)) {
		reached := false
		for _, name := range derived {
			if rows[row].inner == leafValidationRefusals[name] {
				reached = true
				break
			}
		}
		if !reached {
			t.Errorf("the %q row is held to %v and (*LeafNode).Validate names no refusal that value answers to",
				row, rows[row].inner)
		}
	}
}

// TestTheUpdateDoorDoesNotJudgeALifetimeAnUpdateLeafDoesNotCarry is the one clause of section 7.3
// the update door owes nothing to, asserted in both directions.
//
// A key_package leaf's freshness is its lifetime. An update leaf carries none -- the field is a
// variant of the section 7.2 select and is neither encoded nor signed under this source -- so the
// Go struct's Lifetime holds whatever it was built with and reading it would be judging a leaf by
// bytes nobody sent. Section 7.3 puts the update arm of the leaf_node_source rule there instead,
// which is validateUpdateChangesTheEncryptionKey's.
//
// The second half is what makes the first half a statement about the SOURCE rather than about a
// validator that has no lifetime clause at all: the same expired interval, under key_package with a
// real clock, is refused.
func TestTheUpdateDoorDoesNotJudgeALifetimeAnUpdateLeafDoesNotCarry(t *testing.T) {
	crypto := testCrypto(t)
	tree, members := testTreeWith(t, crypto, "alice", "bob")
	_, leaf := testUpdateProposalOf(t, crypto, members[testUpdatedLeaf], testUpdatedLeaf)
	// an interval that ended in 1970, which no clock and no skew can make current
	leaf.Lifetime = Lifetime{NotBefore: 1, NotAfter: 2}
	in := testValidationInput(t, crypto, tree, testCommitterLeaf, testProposalList(t,
		testResignedUpdateOf(t, crypto, members[testUpdatedLeaf], testUpdatedLeaf, leaf)))
	if err := validateUpdateLeafNodeIsValidForAnUpdate(in); err != nil {
		t.Fatalf("the update door refused an update leaf carrying an expired lifetime with %v; the lifetime is a variant field this source does not carry, so nothing here may read it",
			err)
	}
	if err := ValidateProposalList(in); err != nil {
		t.Fatalf("the aggregate refused it with %v", err)
	}
	// and the clause exists: the same interval under the source that DOES carry it, with the
	// clock every sending path passes
	expired := &LeafValidationContext{
		Crypto: crypto, Suite: crypto.Suite(), GroupId: testValidationGroupId(),
		LeafIndex:      testUpdatedLeaf,
		ExpectedSource: LeafNodeSourceUpdate,
		NowMs:          uint64(max(time.Now().UnixMilli(), 1)),
		ClockSkewMs:    leafLifetimeSkewSeconds * 1000,
	}
	if err := leaf.Validate(expired); err != nil {
		t.Fatalf("the same leaf is refused under the update source once a clock is supplied, with %v; then the lifetime IS being read under a source that does not carry it",
			err)
	}
	expired.ExpectedSource = LeafNodeSourceKeyPackage
	leaf.LeafNodeSource = LeafNodeSourceKeyPackage
	if err := leaf.Sign(crypto, members[testUpdatedLeaf].SigPriv, nil, 0); err != nil {
		t.Fatalf("re-sign the leaf under the key_package source: %v", err)
	}
	if err := leaf.Validate(expired); !errors.Is(err, ErrLeafNodeLifetime) {
		t.Fatalf("the same expired interval under key_package answered %v, want %v; if that clause cannot fire then the update half above asserts nothing",
			err, ErrLeafNodeLifetime)
	}
}

// TestValSem110AndTheEncryptionKeyRuleAreOppositeHalvesOfOneQuestion states each of the two
// encryption key rules over the input the OTHER one is written to accept.
//
// They are near opposites and that is why they carry two values. ValSem110 refuses a key somebody
// else holds and must therefore exclude the updating leaf's own outgoing key -- "without that
// exclusion a member republishing anything at all would be refused for colliding with itself" --
// and the excluded key is exactly what section 7.3's update arm refuses. Asserting either half
// alone passes over an implementation that answered one value for both, which would leave a
// caller unable to tell "your key is somebody else's" from "your key has not changed".
func TestValSem110AndTheEncryptionKeyRuleAreOppositeHalvesOfOneQuestion(t *testing.T) {
	crypto := testCrypto(t)
	tree, members := testTreeWith(t, crypto, "alice", "bob", "carol")

	// bob republishing bob's own outgoing key
	_, own := testUpdateProposalOf(t, crypto, members[1], LeafIndex(1))
	own.EncryptionKey = tree.Leaf(1).EncryptionKey
	unchanged := testValidationInput(t, crypto, tree, testCommitterLeaf, testProposalList(t,
		testResignedUpdateOf(t, crypto, members[1], LeafIndex(1), own)))
	if err := ValSem110UpdateUniqueEncryptionKey(unchanged); err != nil {
		t.Fatalf("ValSem110 refused a leaf republishing its own outgoing key: %v", err)
	}
	if err := validateUpdateChangesTheEncryptionKey(unchanged); !errors.Is(err, ErrUpdateEncryptionKeyUnchanged) {
		t.Fatalf("the update arm of section 7.3's leaf_node_source rule answered %v over an update that changed no key, want %v",
			err, ErrUpdateEncryptionKeyUnchanged)
	}

	// and bob republishing carol's, which is the other half
	_, borrowed := testUpdateProposalOf(t, crypto, members[1], LeafIndex(1))
	borrowed.EncryptionKey = tree.Leaf(2).EncryptionKey
	duplicate := testValidationInput(t, crypto, tree, testCommitterLeaf, testProposalList(t,
		testResignedUpdateOf(t, crypto, members[1], LeafIndex(1), borrowed)))
	if err := ValSem110UpdateUniqueEncryptionKey(duplicate); !errors.Is(err, ErrUpdateDuplicateEncryptionKey) {
		t.Fatalf("ValSem110 answered %v over an update publishing another member's key, want %v",
			err, ErrUpdateDuplicateEncryptionKey)
	}
	if err := validateUpdateChangesTheEncryptionKey(duplicate); err != nil {
		t.Fatalf("the update arm refused an update whose key DID change, with %v; the two rules are answering one question",
			err)
	}
}

// TestTheUpdateDoorReadsTheEffectiveExtensionsAndNotTheGroupContextsOwn drives all three arms of
// (*ProposalValidationInput).effectiveExtensions through this door.
//
// The door calls effectiveExtensions and not in.Context.Extensions, and until this test the two
// were indistinguishable: every fixture that reached the section 13.4 clause set the extension on
// the GROUP CONTEXT, which is the third arm, so swapping the call for in.Context.Extensions left
// the whole of ./mls/... and ./message/... green. Measured, not supposed.
//
// What the swap drops is the arm the door's own header claims: RFC 9420 section 12.3 applies a
// GroupContextExtensions proposal FIRST and requires the rest of the list to be judged against the
// result, so a commit that adds an extension in the same breath as an Update must judge that
// update against the extension it is adding. Under in.Context.Extensions that list is judged
// against the extensions the group is leaving behind, and the member updates itself into a leaf
// that cannot support what the same commit installs -- which is erratum 8745's condition arriving
// by the one route the erratum is about.
func TestTheUpdateDoorReadsTheEffectiveExtensionsAndNotTheGroupContextsOwn(t *testing.T) {
	crypto := testCrypto(t)
	unsupported := Extension{ExtensionType: ExtensionType(0xF00A), ExtensionData: []byte{1}}
	base := func() *ProposalValidationInput {
		tree, members := testTreeWith(t, crypto, "alice", "bob")
		update, _ := testUpdateProposalOf(t, crypto, members[1], LeafIndex(1))
		return testValidationInput(t, crypto, tree, testCommitterLeaf, testProposalList(t, update))
	}
	// the control every arm below is one change away from: the same update, judged against a
	// group that requires nothing, is accepted
	if err := validateUpdateLeafNodeIsValidForAnUpdate(base()); err != nil {
		t.Fatalf("the update door refused a well formed update against a group carrying no extensions: %v", err)
	}

	// arm one, the explicit list: a caller that has already applied section 12.3's first step and
	// is re-checking the applied state
	applied := base()
	applied.Extensions = []Extension{unsupported}
	// arm two, the sender side: the list carries its own GroupContextExtensions proposal and
	// nothing has been applied yet. This is the fixture that separates the two calls, and the GCE
	// is FIRST in the list so that a door reading the list in order meets it before the update.
	tree, members := testTreeWith(t, crypto, "alice", "bob")
	update, _ := testUpdateProposalOf(t, crypto, members[1], LeafIndex(1))
	sending := testValidationInput(t, crypto, tree, testCommitterLeaf,
		testProposalList(t, testGceOf(unsupported), update))
	// arm three, the ordinary case: the group's own extensions, unchanged
	ordinary := base()
	ordinary.Context.Extensions = []Extension{unsupported}

	for name, in := range map[string]*ProposalValidationInput{
		"the applied extension list a caller handed in":        applied,
		"the GroupContextExtensions proposal in the same list": sending,
		"the group context's own extensions":                   ordinary,
	} {
		if err := validateUpdateLeafNodeIsValidForAnUpdate(in); !errors.Is(err, errGroupContextExtensionNotListed) {
			t.Errorf("the update door judged against %s answered %v, want %v; that arm of effectiveExtensions is reaching no clause",
				name, err, errGroupContextExtensionNotListed)
		}
	}
	// and the first two arms carry NOTHING on the group context, which is what makes them
	// observations of effectiveExtensions rather than second spellings of the third
	for name, in := range map[string]*ProposalValidationInput{
		"the applied extension list a caller handed in":        applied,
		"the GroupContextExtensions proposal in the same list": sending,
	} {
		if len(in.Context.Extensions) != 0 {
			t.Errorf("%s carries %d group context extensions of its own, so a door reading in.Context.Extensions would refuse it too and this row separates nothing",
				name, len(in.Context.Extensions))
		}
	}
}

// ---------------------------------------------------------------------------
// every rule that sweeps the Updates bucket reaches every position of it
// ---------------------------------------------------------------------------

// updateSweepRule is one rule of this file that ranges over in.List.Updates(), and the edit that
// makes the update at a chosen position the one it must refuse.
//
// The EDIT takes a position rather than the fixture carrying the fault, because that is the whole
// property: the fault is moved along the bucket and the rule has to keep finding it. A rule
// narrowed to updates[0] passes every fixture this package had -- each carries exactly one Update
// -- which is the p4 ValSem401 shape this file's comments cite three times as what they guard
// against, and it was measured on this tree: both section 12.1.2 rules could be narrowed to
// element zero with the whole of ./mls/... and ./message/... green.
type updateSweepRule struct {
	// breaks edits the fixture so that the update at index `at` is the one this rule refuses,
	// and nothing else about the list is wrong.
	breaks func(t *testing.T, in *ProposalValidationInput, at int)
}

// updateSweepFixture is a pre-commit tree of four members and a list of three VALID updates, one
// per member other than the committer.
//
// THREE and not two, because a rule that read the first and the last would pass a two entry list
// while stepping over everything in between, and a commit's proposal list legitimately holds one
// Update per member. Fresh every call, because every row below edits what it is handed.
func updateSweepFixture(t *testing.T, crypto CryptoProvider) *ProposalValidationInput {
	t.Helper()
	tree, members := testTreeWith(t, crypto, "alice", "bob", "carol", "dave")
	entries := []CachedProposal{}
	for at := 0; at < len(members); at += 1 {
		// SKIPPED BY COMPARISON WITH THE COMMITTER and not by starting the loop at one.
		// The header says "one per member other than the committer" and the loop used to
		// say "every member but leaf zero", which were the same sentence only while every
		// input of this package sat at leaf zero. ValSem111 refuses a list covering the
		// committer's own update, so the day the two stopped agreeing this fixture became
		// one the aggregate refuses before any sweep rule runs.
		if LeafIndex(at) == testCommitterLeaf {
			continue
		}
		update, _ := testUpdateProposalOf(t, crypto, members[at], LeafIndex(at))
		entries = append(entries, update)
	}
	return testValidationInput(t, crypto, tree, testCommitterLeaf, testProposalList(t, entries...))
}

// testRequiredCapabilitiesNaming is a required_capabilities extension over the types a caller
// chooses, so a row can require something every fixture leaf DOES list and then take it away from
// exactly one of them. testRequiredCapabilitiesExtension names a type no fixture lists, which is
// the opposite fixture and cannot separate positions.
func testRequiredCapabilitiesNaming(t *testing.T, types ...ExtensionType) Extension {
	t.Helper()
	body, err := syntax.Marshal(&RequiredCapabilities{ExtensionTypes: types})
	if err != nil {
		t.Fatalf("marshal required_capabilities: %v", err)
	}
	return Extension{ExtensionType: ExtensionTypeRequiredCapabilities, ExtensionData: body}
}

// updateSweepRules is one edit per rule of the derived class. It is not what decides the class:
// the gate below reads that off validate_proposals.go and holds this table to it in both
// directions, so a rule that grows an Updates loop later has no row and fails here.
func updateSweepRules() map[string]updateSweepRule {
	return map[string]updateSweepRule{
		// the group requires a type every fixture leaf lists, and this one stops listing it. The
		// requirement has to be one the others satisfy or every position refuses and the sweep
		// separates nothing.
		"ValSem109UpdateRequiredCapabilities": {breaks: func(t *testing.T, in *ProposalValidationInput, at int) {
			in.Context.Extensions = []Extension{
				testRequiredCapabilitiesNaming(t, ExtensionTypeUrmessageLeafKeys)}
			leaf := &testListEntryAt(t, in.List, "Updates", at).Proposal.Update.LeafNode
			leaf.Capabilities.Extensions = withoutExtensionType(leaf.Capabilities.Extensions,
				ExtensionTypeUrmessageLeafKeys)
		}},
		// the committer's own key, which is the one member key the members' half of ValSem110
		// does not exclude: alice commits and is not updated, so her outgoing key is still taken.
		"ValSem110UpdateUniqueEncryptionKey": {breaks: func(t *testing.T, in *ProposalValidationInput, at int) {
			testListEntryAt(t, in.List, "Updates", at).Proposal.Update.LeafNode.EncryptionKey =
				in.Tree.Leaf(in.Committer).EncryptionKey
		}},
		"ValSem111NoCommitterUpdate": {breaks: func(t *testing.T, in *ProposalValidationInput, at int) {
			in.Committer = testListEntryAt(t, in.List, "Updates", at).Sender
		}},
		// a leaf index past the width of the fixture tree, which is a sender no leaf occupies.
		"ValSem112UpdateSenderIsMember": {breaks: func(t *testing.T, in *ProposalValidationInput, at int) {
			testListEntryAt(t, in.List, "Updates", at).Sender = LeafIndex(in.Tree.LeafWidth()) + 1
		}},
		// the signature, flipped AFTER signing, which is the one edit that reaches the update
		// door without reaching any other rule of this file: no other rule verifies anything.
		"validateUpdateLeafNodeIsValidForAnUpdate": {breaks: func(t *testing.T, in *ProposalValidationInput, at int) {
			leaf := &testListEntryAt(t, in.List, "Updates", at).Proposal.Update.LeafNode
			if len(leaf.Signature) == 0 {
				t.Fatal("the fixture update leaf carries no signature, so flipping a byte of it asserts nothing")
			}
			leaf.Signature[0] ^= 0xFF
		}},
		// the leaf republishing the key it already holds, which is section 7.3's update arm and
		// is exactly the shape ValSem110 must let through.
		"validateUpdateChangesTheEncryptionKey": {breaks: func(t *testing.T, in *ProposalValidationInput, at int) {
			entry := testListEntryAt(t, in.List, "Updates", at)
			entry.Proposal.Update.LeafNode.EncryptionKey =
				in.Tree.Leaf(entry.Sender).EncryptionKey
		}},
		// a Remove landing on the leaf this Update refreshes. The refusal is raised in the
		// Removes loop, so what it observes is that the UPDATES loop reached position `at` to
		// record that leaf at all.
		"validateSingleUpdateOrRemovePerLeaf": {breaks: func(t *testing.T, in *ProposalValidationInput, at int) {
			remove := testRemoveOf(testListEntryAt(t, in.List, "Updates", at).Sender)
			in.List = testListPlus(t, in.List, remove)
		}},
	}
}

// proposalListRulesThatSweepTheUpdates is every rule of this file whose body ranges over the
// updates view, read off the source rather than named.
//
// Off the RANGE STATEMENT and not off a token search, because what is being measured is that a
// loop exists over that view: a body merely mentioning the updates once would match a token scan
// and would be a rule reading element zero, which is the defect this gate is about.
//
// IN TWO STEPS NOW, and the second step is what the derivation of the views cost this gate. The
// view is a method rather than a field, so a rule cannot range over `in.List.Updates()` -- ranging
// over a call would refilter the commit order at every index. Each rule binds the answer once and
// ranges over the binding, so the scan first collects the names bound from in.List.Updates() and
// then finds the loops over them. A gate left reading the old selector would find no rule at all
// and report a clean bill over an empty class, which is why the caller below keeps a positive
// control.
func proposalListRulesThatSweepTheUpdates(t *testing.T) []string {
	t.Helper()
	parsed := mustParseSource(t, "validate_proposals.go")
	found := []string{}
	for _, name := range proposalListRulesDeclared(t) {
		body := parsed.declarationOf(t, "", name).Body
		bound := map[string]bool{}
		ast.Inspect(body, func(node ast.Node) bool {
			assign, isAssign := node.(*ast.AssignStmt)
			if !isAssign {
				return true
			}
			for at, value := range assign.Rhs {
				if parsed.render(value) != "in.List.Updates()" || at >= len(assign.Lhs) {
					continue
				}
				bound[parsed.render(assign.Lhs[at])] = true
			}
			return true
		})
		sweeps := false
		ast.Inspect(body, func(node ast.Node) bool {
			loop, isLoop := node.(*ast.RangeStmt)
			if !isLoop || !bound[parsed.render(loop.X)] {
				return true
			}
			sweeps = true
			return false
		})
		if sweeps {
			found = append(found, name)
		}
	}
	slices.Sort(found)
	return found
}

// TestEveryRuleThatSweepsTheUpdatesBucketReachesEveryPositionOfIt moves one fault along the bucket
// and requires each rule to keep finding it.
//
// The class is derived off the range statements of validate_proposals.go, so a rule that grows an
// Updates loop later is swept on the commit that grows it. What each row asserts is the pair that
// makes a position claim mean anything: the unbroken list is ACCEPTED, and the list with the fault
// at position n is refused, for every n. A rule narrowed to updates[0] passes the first half and
// fails the second at every n above zero -- which no fixture in this package could say before,
// because each carried exactly one Update.
func TestEveryRuleThatSweepsTheUpdatesBucketReachesEveryPositionOfIt(t *testing.T) {
	crypto := testCrypto(t)
	derived := proposalListRulesThatSweepTheUpdates(t)
	rows := updateSweepRules()
	// the positive control: a scan that resolved nothing reports the clean bill a complete one
	// reports, and the update door certainly sweeps the bucket
	if !slices.Contains(derived, "validateUpdateLeafNodeIsValidForAnUpdate") {
		t.Fatalf("the scan read %v out of validate_proposals.go, which certainly has validateUpdateLeafNodeIsValidForAnUpdate ranging over the updates view, so it is reading something else",
			derived)
	}
	for _, name := range slices.Sorted(maps.Keys(rows)) {
		if !slices.Contains(derived, name) {
			t.Errorf("updateSweepRules names %s and no rule of this file by that name ranges over the updates view; the row has outlived what it covered",
				name)
		}
	}
	positions := len(updateSweepFixture(t, crypto).List.Updates())
	if positions < 3 {
		t.Fatalf("the fixture carries %d updates; with fewer than three, a rule reading the first and the last would step over nothing and this sweep would assert almost nothing",
			positions)
	}
	for _, name := range derived {
		row, written := rows[name]
		if !written {
			t.Errorf("%s ranges over the updates view and has no row here, so nothing says it reaches any position but the first",
				name)
			continue
		}
		rule := proposalListRuleFor(t, name)
		if err := rule(updateSweepFixture(t, crypto)); err != nil {
			t.Errorf("%s refuses the unbroken fixture with %v; every refusal below would then be this rule objecting to the fixture rather than to the fault",
				name, err)
			continue
		}
		for at := 0; at < positions; at += 1 {
			in := updateSweepFixture(t, crypto)
			row.breaks(t, in, at)
			if err := rule(in); err == nil {
				t.Errorf("%s accepted a list whose fault is at updates[%d] of %d; it is reading some positions of the bucket and not others",
					name, at, positions)
			}
		}
	}
	t.Logf("%d rules sweep the updates bucket, each held at every one of %d positions", len(derived), positions)
}
