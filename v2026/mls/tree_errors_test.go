// The gates over tree_errors.go: that the class judged below is the file's own, that no two
// of its errors answer for each other, and that the two which deliberately wrap a tree math
// sentinel are the only two that do.
//
// The plan's own test for this task is not here in the shape it was written, and the reason
// is a finding rather than a preference. It listed the thirteen names by hand, checked that
// no two carried the same message, and checked that each survived errors.Join. All three
// halves are weaker than they read:
//
//   - a hand written list is not the class. A fourteenth error declared in tree_errors.go and
//     left off the list is judged by nothing at all, and nothing fails. The count has the
//     wrong polarity for that: it fails on adding an error and remembering the list, and
//     stays quiet on adding one and forgetting it. So the class is derived from the file's
//     own declarations here and the list is held to it in both directions, which is what
//     TestKeyScheduleOwnedErrorsIsEveryDeclarationOfItsFile does for the key schedule's file.
//   - comparing MESSAGES does not see aliasing. The mutation this task was asked to survive
//     is a sentinel redeclared so two names wrap one underlying error, and
//     `ErrTreeHashMismatch = fmt.Errorf("...: %w", ErrParentHashMismatch)` carries a message
//     no other name carries while making errors.Is(ErrTreeHashMismatch, ErrParentHashMismatch)
//     true. Every caller branching on the pair then reads the wrong one as the right one, and
//     the plan's test passes. Measured, not supposed. The sweep below is over errors.Is in
//     both directions as well as over the messages.
//   - errors.Join(err, errors.New("context")) followed by errors.Is(joined, err) cannot fail
//     for any non-nil err. It is a property of errors.Join, asserted thirteen times, and it
//     says nothing about this file.
package mls

import (
	"errors"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// treeErrorsFile is the single file this plan declares its non-ValSem errors in. Every class
// below is derived from that file rather than from the list, which is the difference between
// sweeping the class and sweeping a copy of it.
const treeErrorsFile = "tree_errors.go"

// treeMathErrorsFile is the wave 1 file whose two index sentinels this file's two index
// errors wrap. Its error class is derived from it for the same reason.
const treeMathErrorsFile = "tree_math.go"

// treeOwnedErrors is the thirteen the interface registry names as this plan's plus the one
// task 11 added, keyed by the name each is declared under so the derivation below can
// compare the two sets by name.
//
// Nothing here is trusted: TestTreeOwnedErrorsIsEveryDeclarationOfItsFile holds this map to
// what tree_errors.go actually declares, in both directions.
var treeOwnedErrors = map[string]error{
	"ErrLeafIndexOutOfRange":      ErrLeafIndexOutOfRange,
	"ErrNodeIndexOutOfRange":      ErrNodeIndexOutOfRange,
	"ErrLeafBlank":                ErrLeafBlank,
	"ErrTreeMalformed":            ErrTreeMalformed,
	"ErrNodeTypeMismatch":         ErrNodeTypeMismatch,
	"ErrUnmergedLeavesNotSorted":  ErrUnmergedLeavesNotSorted,
	"ErrUnmergedLeafInconsistent": ErrUnmergedLeafInconsistent,
	"ErrParentHashMismatch":       ErrParentHashMismatch,
	"ErrTreeHashMismatch":         ErrTreeHashMismatch,
	"ErrLeafNodeSourceMismatch":   ErrLeafNodeSourceMismatch,
	"ErrLeafNodeLifetime":         ErrLeafNodeLifetime,
	"ErrLeafKeysExtensionInvalid": ErrLeafKeysExtensionInvalid,
	"ErrNoPathSecret":             ErrNoPathSecret,
	"ErrPathSecretMismatch":       ErrPathSecretMismatch,
	"ErrRatchetTreeExtensionTag":  ErrRatchetTreeExtensionTag,
}

// treeMathOwnedErrors is every error tree_math.go declares, which is the class the
// exclusivity sweep runs over.
//
// Held to that file's own Err-prefixed declarations below, so a tenth tree math sentinel
// cannot land outside this sweep: the whole point of the sweep is that a tree error must
// answer to exactly one of these or to none, and a class that stopped growing with the file
// would report a clean bill over the sentinel it had never heard of.
var treeMathOwnedErrors = map[string]error{
	"ErrLeafCountRange":    ErrLeafCountRange,
	"ErrLeafCountNotFull":  ErrLeafCountNotFull,
	"ErrNodeOutOfRange":    ErrNodeOutOfRange,
	"ErrLeafOutOfRange":    ErrLeafOutOfRange,
	"ErrNodeIsParent":      ErrNodeIsParent,
	"ErrLeafHasNoChildren": ErrLeafHasNoChildren,
	"ErrRootHasNoParent":   ErrRootHasNoParent,
	"ErrRootHasNoSibling":  ErrRootHasNoSibling,
	"ErrNodeWidthNotOdd":   ErrNodeWidthNotOdd,
}

// treeIndexWraps is the whole sanctioned wrapping relation between the two files: which of
// this file's errors may answer to which of tree math's, and no other pair may.
//
// Two entries and not a rule, because the sanction is a judgement about a pair rather than a
// property either name has. The file header carries the argument for each.
var treeIndexWraps = map[string]string{
	"ErrLeafIndexOutOfRange": "ErrLeafOutOfRange",
	"ErrNodeIndexOutOfRange": "ErrNodeOutOfRange",
}

// TestTreeOwnedErrorsIsEveryDeclarationOfItsFile derives the class every sweep below runs
// over instead of trusting the transcription of it.
//
// The derivation is every package level name the file declares, not every name that looks
// like an error, because that file holds nothing else by design and a name this gate cannot
// classify should be loud. A helper or a type landing there fails here too, which says either
// move it or widen the rule on purpose.
func TestTreeOwnedErrorsIsEveryDeclarationOfItsFile(t *testing.T) {
	declared := packageLevelDeclarations(t, ".")
	fromFile := map[string]bool{}
	for name, file := range declared {
		if file == treeErrorsFile {
			fromFile[name] = true
		}
	}
	if len(fromFile) == 0 {
		t.Fatalf("the scan found nothing declared in %s, so this gate compared the list against an empty set",
			treeErrorsFile)
	}
	if !fromFile["ErrTreeMalformed"] {
		t.Fatalf("the scan did not find ErrTreeMalformed among the declarations of %s, which certainly declares it, so it is reading something other than that file",
			treeErrorsFile)
	}
	for _, name := range slices.Sorted(maps.Keys(fromFile)) {
		if _, listed := treeOwnedErrors[name]; !listed {
			t.Errorf("%s declares %s and treeOwnedErrors does not list it, so neither the distinctness sweep nor the tree math exclusivity sweep judges it; add it there",
				treeErrorsFile, name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(treeOwnedErrors)) {
		if !fromFile[name] {
			t.Errorf("treeOwnedErrors lists %s and %s does not declare it, so the sweeps run over a name this file does not own",
				name, treeErrorsFile)
		}
	}
}

// TestTreeMathOwnedErrorsIsEveryErrorItsFileDeclares is the same derivation over the other
// half of the exclusivity sweep.
//
// Err-prefixed rather than every declaration, because tree_math.go is 789 lines of arithmetic
// and its errors are one block of it. The prefix is the file's own naming convention, and a
// sentinel that broke it would be invisible here -- which is why the positive control below
// names one of the nine explicitly: a scan reading the wrong file, or a prefix filter matching
// nothing, reports the same clean bill as a complete one.
func TestTreeMathOwnedErrorsIsEveryErrorItsFileDeclares(t *testing.T) {
	declared := packageLevelDeclarations(t, ".")
	fromFile := map[string]bool{}
	for name, file := range declared {
		if file == treeMathErrorsFile && strings.HasPrefix(name, "Err") {
			fromFile[name] = true
		}
	}
	if !fromFile["ErrLeafOutOfRange"] {
		t.Fatalf("the scan did not find ErrLeafOutOfRange among the Err declarations of %s, which certainly declares it, so it is reading something other than that file",
			treeMathErrorsFile)
	}
	if got, want := slices.Sorted(maps.Keys(fromFile)), slices.Sorted(maps.Keys(treeMathOwnedErrors)); !slices.Equal(got, want) {
		t.Fatalf("%s declares %v and treeMathOwnedErrors holds %v; the exclusivity sweep below runs over the second, so a sentinel missing from it is one no tree error is held against",
			treeMathErrorsFile, got, want)
	}
}

// TestTreeErrorsAreDistinctAndNamed is the plan's test for this task, rewritten to be able to
// fail at the thing it is named after.
//
// Three properties per pair, and the first is the one the plan's version could not see:
// neither error may answer for the other under errors.Is in either direction, which is what a
// redeclaration or an accidental wrap actually looks like. The messages are compared as well,
// because two distinct values carrying one string are indistinguishable in a log; and each is
// required to be non-nil and non-empty, because a var that was declared and never assigned
// satisfies every comparison below by being nothing.
//
// The count pins the file: a name added to it is a change somebody makes deliberately, in the
// same commit, with a reason. What stops the list shrinking, and what stops it lagging behind
// the file, is TestTreeOwnedErrorsIsEveryDeclarationOfItsFile rather than this number.
func TestTreeErrorsAreDistinctAndNamed(t *testing.T) {
	if len(treeOwnedErrors) != 15 {
		t.Fatalf("this plan owns %d errors and the interface registry's tree_errors.go block names 13, plus ErrRatchetTreeExtensionTag which task 11 added and ErrLeafBlank which split task 18's blank-leaf refusal off ErrLeafIndexOutOfRange; if one landed or left, say so here in the same commit",
			len(treeOwnedErrors))
	}
	names := slices.Sorted(maps.Keys(treeOwnedErrors))
	for i, name := range names {
		first := treeOwnedErrors[name]
		if first == nil {
			t.Fatalf("%s is nil", name)
		}
		if first.Error() == "" {
			t.Fatalf("%s has an empty message", name)
		}
		if !strings.HasPrefix(first.Error(), "mls: ") {
			t.Errorf("%s reads %q; every typed error of this package names the package it came from, so a caller logging one at a distance can tell",
				name, first.Error())
		}
		for j, other := range names {
			if i == j {
				continue
			}
			second := treeOwnedErrors[other]
			if errors.Is(first, second) {
				t.Errorf("%s answers to %s (%v), so a caller branching on the two reads one as the other and nothing reports it",
					name, other, first)
			}
			if first.Error() == second.Error() {
				t.Errorf("%s and %s carry the same message %q, so a log cannot tell them apart",
					name, other, first.Error())
			}
		}
	}
}

// TestTheTwoTreeIndexErrorsWrapTheTreeMathSentinels is the positive half of the wrapping
// argument in the file header: a caller that only knows the arithmetic's sentinel still
// matches the ratchet tree's refusal of the same condition.
//
// Held over treeIndexWraps rather than written twice, so the pair the exclusivity sweep
// exempts and the pair asserted here are one list. A wrap sanctioned there and absent here
// would be an exemption granted to a relationship that does not exist.
func TestTheTwoTreeIndexErrorsWrapTheTreeMathSentinels(t *testing.T) {
	if len(treeIndexWraps) == 0 {
		t.Fatal("no wrap is sanctioned, so the exclusivity sweep below exempts nothing and this test asserts nothing")
	}
	for _, name := range slices.Sorted(maps.Keys(treeIndexWraps)) {
		wrapped := treeIndexWraps[name]
		outer, ok := treeOwnedErrors[name]
		if !ok {
			t.Fatalf("treeIndexWraps names %s and %s does not declare it", name, treeErrorsFile)
		}
		inner, ok := treeMathOwnedErrors[wrapped]
		if !ok {
			t.Fatalf("treeIndexWraps names %s and %s does not declare it", wrapped, treeMathErrorsFile)
		}
		if !errors.Is(outer, inner) {
			t.Errorf("%s does not answer to %s, so a caller that knows only the arithmetic's sentinel stops matching the ratchet tree's refusal of the same condition",
				name, wrapped)
		}
		if errors.Is(inner, outer) {
			t.Errorf("%s answers to %s, so the wrap runs both ways and the two names have stopped being distinguishable",
				wrapped, name)
		}
	}
}

// TestOnlyTheTreeIndexErrorsAnswerToATreeMathSentinel is the half with teeth.
//
// Wrapping is useful only while it stays exclusive. A third error of this file answering to
// ErrLeafOutOfRange would make a caller asking "was this a leaf index problem" get yes for a
// malformed tree, and the answer would be wrong in the direction that reads as working. The
// sweep is over both derived classes, so it judges every pair of the two files rather than the
// pairs somebody remembered, and the number of sanctioned matches is asserted -- a sweep that
// found none at all reports exactly what an exclusive one reports.
func TestOnlyTheTreeIndexErrorsAnswerToATreeMathSentinel(t *testing.T) {
	matched := 0
	for _, name := range slices.Sorted(maps.Keys(treeOwnedErrors)) {
		for _, wrapped := range slices.Sorted(maps.Keys(treeMathOwnedErrors)) {
			if !errors.Is(treeOwnedErrors[name], treeMathOwnedErrors[wrapped]) {
				continue
			}
			matched++
			if treeIndexWraps[name] != wrapped {
				t.Errorf("%s answers to %s and the only sanctioned wraps are %v; two sentinels for one condition is how a caller comes to read the wrong one as the right one",
					name, wrapped, treeIndexWraps)
			}
		}
	}
	if matched != len(treeIndexWraps) {
		t.Fatalf("%d pairs of the two files answer for each other and %d are sanctioned; a sweep that matched nothing reports what an exclusive one reports",
			matched, len(treeIndexWraps))
	}
}

// TestNoTreeErrorAnswersToAKeyScheduleSentinel is the same exclusivity against the other
// error file of this package that carries a maintained, file-derived class.
//
// Nothing is sanctioned here at all, which makes it the cheap half: the key schedule's
// sixteen and this plan's thirteen name conditions from different layers, and an errors.Is
// holding across them would mean one of the two files had grown a wrap nobody argued for.
//
// crypto_errors.go used to be excluded here, on the grounds that it had no map held to its own
// declarations and that a written one would be a hole of the kind the header of this file
// objects to, and that a wrap onto one of its names would have to be written in tree_errors.go
// where the file gate above accounts for it. The first two are answered by deriving
// cryptoOwnedErrors from that file the way this class is derived from its own. The third was
// measured and is false where it matters: the wrap IS written in tree_errors.go, on a
// declaration the file gate already knows about, and nothing looked at what it answers to.
// TestOnlyTheSanctionedWrapsHoldAcrossThisPackagesErrorClasses now judges that pair and every
// other, so this test is the narrow, named half of a sweep that no longer depends on somebody
// remembering to write it.
func TestNoTreeErrorAnswersToAKeyScheduleSentinel(t *testing.T) {
	if len(keyScheduleOwnedErrors) == 0 {
		t.Fatal("the key schedule's error class is empty, so this sweep compares against nothing")
	}
	for _, name := range slices.Sorted(maps.Keys(treeOwnedErrors)) {
		for _, other := range slices.Sorted(maps.Keys(keyScheduleOwnedErrors)) {
			if errors.Is(treeOwnedErrors[name], keyScheduleOwnedErrors[other]) {
				t.Errorf("%s answers to %s, and nothing in either plan argues for a wrap between the two layers",
					name, other)
			}
		}
	}
}

// cryptoErrorsFile is the crypto layer's error file. Its class is derived from it below for
// the reason tree math's is: the two sweeps at the bottom of this file judge every pair of
// this package's maintained error classes, and a class that stopped growing with its file
// reports a clean bill over the sentinel it had never heard of.
const cryptoErrorsFile = "crypto_errors.go"

// cryptoOwnedErrors is every error crypto_errors.go declares.
//
// It is here rather than absent, and that is a reversal of what the header of tree_errors.go
// argued when this file landed. The argument was that crypto_errors.go has no maintained map,
// that adding a written one would be a hole of the same shape this file objects to, and that a
// wrap onto one of its names would have to be written in tree_errors.go where the file gate
// catches it. The first two are answered by holding the map to the file, exactly as the other
// three classes are held. The third was measured and is false in the direction that matters:
// `ErrTreeMalformed = fmt.Errorf("mls: ratchet tree is malformed: %w", ErrCryptoBadSignature)`
// IS written in tree_errors.go, the file gate sees a declaration it already knows about, and
// every test in the package passed -- after which errors.Is(ErrTreeMalformed,
// ErrCryptoBadSignature) is true and a caller telling "the signature did not verify" from
// "this is not a tree" reads one as the other. The adjacency is not hypothetical: the tasks
// after this one return ErrBadSignature and ErrLeafNodeSourceMismatch out of one validator.
var cryptoOwnedErrors = map[string]error{
	"ErrUnknownCipherSuite": ErrUnknownCipherSuite,
	"ErrInvalidPoint":       ErrInvalidPoint,
	"ErrBadKeyLength":       ErrBadKeyLength,
	"ErrNilRandomSource":    ErrNilRandomSource,
	"ErrBadNonceLength":     ErrBadNonceLength,
	"ErrBadKemOutput":       ErrBadKemOutput,
	"ErrBadSignatureKey":    ErrBadSignatureKey,
	"ErrCryptoBadSignature": ErrCryptoBadSignature,
	"ErrAeadOpen":           ErrAeadOpen,
	"ErrSequenceOverflow":   ErrSequenceOverflow,
}

// mlsErrorClasses is every maintained error class of this package, keyed by the file each is
// derived from.
//
// This is the class the two sweeps at the bottom of this file run over, and keying it by FILE
// is what lets it be checked against the package rather than against itself:
// TestEveryExportedErrorOfThisPackageIsInAMaintainedClass walks every Err-prefixed package
// level declaration of every non-test file and requires the class held for that declaration's
// file to list it. A fourteenth error of this plan declared in a new file is then a failure
// naming the file, rather than a name every sweep in this package runs past.
//
// Five entries and not three. crypto_errors.go joined when a review declared a fourteenth p5
// error in a file of its own and watched the entire package stay green, and framing_errors.go
// joined the same way -- p6 task 1 landed ten structural framing errors in a file of its own,
// and this gate is what said so in the same run rather than a plan later.
// errors_lifecycle.go is the sixth and it joined on p7 task 1, in the same way and for the same
// reason: twenty five group lifecycle policy refusals landed in a file of their own, no class
// held them, and this gate named all twenty five on the first full run of that task rather than
// leaving a whole exported surface unjudged until a later plan wrapped one of them.
var mlsErrorClasses = map[string]map[string]error{
	treeErrorsFile:        treeOwnedErrors,
	treeMathErrorsFile:    treeMathOwnedErrors,
	keyScheduleErrorsFile: keyScheduleOwnedErrors,
	cryptoErrorsFile:      cryptoOwnedErrors,
	framingErrorsFile:     framingOwnedErrors,
	lifecycleErrorsFile:   lifecycleOwnedErrors,

	// the seventh, and it joined on p7 task 7 for the reason the sixth did: nineteen proposal
	// list refusals landed in a file of their own, no class held them, and without an entry
	// here the whole exported surface of RFC 9420 section 12.2 would sit outside both sweeps
	// below.
	proposalValidationErrorsFile: proposalValidationOwnedErrors,
}

// TestCryptoOwnedErrorsIsEveryErrorItsFileDeclares is the same derivation the other three
// classes get, over the fourth.
//
// Err-prefixed rather than every declaration, because crypto_errors.go is one var block and
// the prefix is its own naming convention; the positive control names one of the ten
// explicitly, since a scan reading the wrong file and a prefix filter matching nothing report
// the same clean bill a complete one reports.
func TestCryptoOwnedErrorsIsEveryErrorItsFileDeclares(t *testing.T) {
	declared := packageLevelDeclarations(t, ".")
	fromFile := map[string]bool{}
	for name, file := range declared {
		if file == cryptoErrorsFile && strings.HasPrefix(name, "Err") {
			fromFile[name] = true
		}
	}
	if !fromFile["ErrCryptoBadSignature"] {
		t.Fatalf("the scan did not find ErrCryptoBadSignature among the Err declarations of %s, which certainly declares it, so it is reading something other than that file",
			cryptoErrorsFile)
	}
	if got, want := slices.Sorted(maps.Keys(fromFile)), slices.Sorted(maps.Keys(cryptoOwnedErrors)); !slices.Equal(got, want) {
		t.Fatalf("%s declares %v and cryptoOwnedErrors holds %v; the sweeps below run over the second, so a sentinel missing from it is one no error of this package is held against",
			cryptoErrorsFile, got, want)
	}
}

// TestEveryExportedErrorOfThisPackageIsInAMaintainedClass is the answer to the class this file
// derived from a FILE when the property is about the PACKAGE.
//
// treeOwnedErrors is derived from tree_errors.go, which makes it complete for that file and
// silent about everywhere else. A review measured what that costs: a new file declaring
// `ErrTreeShapeOutOfRange = fmt.Errorf("...: %w", ErrLeafOutOfRange)` is a third error
// answering to the leaf index sentinel -- the exact condition the exclusivity sweep exists to
// forbid, and the condition tree_errors.go's header says would make "was this a leaf index
// problem" mean nothing -- and the whole package stayed green, because every sweep here judges
// only what lives in the one file. This plan has some twenty-five tasks left, adding tree.go,
// leaf_node.go and update_path.go among others, and nothing forces their errors into
// tree_errors.go.
//
// So the class is the package's: every Err-prefixed package level declaration of every
// non-test file must be listed in the class held for the file it is declared in, and every
// exported name of every class must be a declaration this scan actually found. A file with no
// class at all is the loud case, because that is the shape the mutation took.
//
// Test files are excluded on purpose and this is the note that says why rather than leaving
// the filter looking arbitrary: the runner files of this package declare unexported errKat*
// sentinels by the dozen for their comparator controls, they are Err-prefixed only in the
// lowercase, and they are the private business of the test that raises them. What is swept
// here is the surface a CALLER can branch on.
func TestEveryExportedErrorOfThisPackageIsInAMaintainedClass(t *testing.T) {
	declared := packageLevelDeclarations(t, ".")
	swept := 0
	for _, name := range slices.Sorted(maps.Keys(declared)) {
		file := declared[name]
		if strings.HasSuffix(file, "_test.go") || !strings.HasPrefix(name, "Err") {
			continue
		}
		swept++
		class, held := mlsErrorClasses[file]
		if !held {
			t.Errorf("%s declares %s and mlsErrorClasses holds no class for that file, so every exclusivity sweep of this package runs past it; give the file a class or move the error into one that has one",
				file, name)
			continue
		}
		if _, listed := class[name]; !listed {
			t.Errorf("%s declares %s and the class held for that file does not list it, so no sweep judges what it answers to",
				file, name)
		}
	}
	if swept == 0 {
		t.Fatal("the scan found no exported error in any non-test file of this package, which cannot be true, so it read something other than the package")
	}
	for _, file := range slices.Sorted(maps.Keys(mlsErrorClasses)) {
		for _, name := range slices.Sorted(maps.Keys(mlsErrorClasses[file])) {
			if !strings.HasPrefix(name, "Err") {
				// the key schedule's two unexported invariant names, which are that
				// plan's own and are held to their file by its own gate
				continue
			}
			if declared[name] != file {
				t.Errorf("the class for %s lists %s and the package declares that name in %q, so the sweeps run over a name that file does not own",
					file, name, declared[name])
			}
		}
	}
}

// TestOnlyTheSanctionedWrapsHoldAcrossThisPackagesErrorClasses is the exclusivity sweep run
// over the package rather than over one pair of files.
//
// TestOnlyTheTreeIndexErrorsAnswerToATreeMathSentinel judges tree_errors.go against
// tree_math.go, which is the pair the file header argues about and leaves every other pair
// unjudged. Two of them were measured and both are silent: a tree error wrapping
// ErrCryptoBadSignature, and an error of this plan declared in a file of its own wrapping
// ErrLeafOutOfRange. This sweep is every ordered pair of every maintained class, so a wrap
// between any two of the four files is a failure naming both names.
//
// Sanctioned by treeIndexWraps and nothing else, deliberately. That table is two entries with
// an argument each in the header of tree_errors.go, and it is the same table
// TestTheTwoTreeIndexErrorsWrapTheTreeMathSentinels asserts the positive half of, so a wrap
// cannot be excused here without also being required there. If a later plan argues for a
// third, it goes in that table with its reason and both halves move together.
func TestOnlyTheSanctionedWrapsHoldAcrossThisPackagesErrorClasses(t *testing.T) {
	swept := map[string]error{}
	for _, file := range slices.Sorted(maps.Keys(mlsErrorClasses)) {
		for name, value := range mlsErrorClasses[file] {
			if already, seen := swept[name]; seen {
				t.Fatalf("%s is held by two classes (%v and %v), so one of them is judging a name it does not own",
					name, already, value)
			}
			swept[name] = value
		}
	}
	if len(swept) < len(treeOwnedErrors)+len(treeMathOwnedErrors) {
		t.Fatalf("the sweep runs over %d errors, fewer than the two classes the file header argues about hold between them; it is reading something other than mlsErrorClasses",
			len(swept))
	}
	matched := 0
	for _, name := range slices.Sorted(maps.Keys(swept)) {
		for _, other := range slices.Sorted(maps.Keys(swept)) {
			if name == other || !errors.Is(swept[name], swept[other]) {
				continue
			}
			if treeIndexWraps[name] != other {
				t.Errorf("%s answers to %s and the only sanctioned wraps are %v; two sentinels for one condition is how a caller comes to read the wrong one as the right one",
					name, other, treeIndexWraps)
				continue
			}
			matched++
		}
	}
	if matched != len(treeIndexWraps) {
		t.Fatalf("%d sanctioned wraps hold and %d are written down; a sweep over values that stopped answering to each other reports what an exclusive one reports",
			matched, len(treeIndexWraps))
	}
}

// The two patterns the citation gate below reads a file's prose with. A citation is a Test
// name written anywhere in the file; the wrap pattern rejoins a name these files split across
// two comment lines with a trailing hyphen, which is a real spelling here and would otherwise
// be reported as two names that do not exist.
var (
	testNameCitation = regexp.MustCompile(`\bTest[A-Z][A-Za-z0-9_]*`)
	commentLineWrap  = regexp.MustCompile(`-\r?\n[ \t]*//[ \t]*`)
)

// treePlanSourceFiles is the files of this package this plan writes, derived from where the
// names it owns are declared rather than listed.
//
// Derived because a list of three file names is the same defect one level up: a fourth file of
// this plan lands, the gate below never reads it, and the gate goes on reporting a clean bill
// over prose it has not seen. The anchors are names this plan certainly owns -- its two class
// tables, its vector run table, and every error of its own file -- and a missing one is fatal
// rather than a smaller file set.
func treePlanSourceFiles(t *testing.T) []string {
	t.Helper()
	declared := packageLevelDeclarations(t, ".")
	anchors := []string{"treeOwnedErrors", "treeIndexWraps", "treeVectorRuns"}
	anchors = append(anchors, slices.Sorted(maps.Keys(treeOwnedErrors))...)
	files := map[string]bool{}
	for _, name := range anchors {
		file, held := declared[name]
		if !held {
			t.Fatalf("package mls declares no %s, so this file set is derived from a name this plan no longer owns", name)
		}
		files[file] = true
	}
	if !files[treeErrorsFile] {
		t.Fatalf("the derivation did not reach %s, which declares this plan's fourteen errors, so it is reading something other than the package",
			treeErrorsFile)
	}
	return slices.Sorted(maps.Keys(files))
}

// TestThisPlansFilesCiteNoTestThatDoesNotExist holds this plan's prose to naming tests that
// are there.
//
// A file header that says which test has the teeth is doing real work -- it is the only place
// the argument for a design decision and the thing enforcing it are joined -- and it does that
// work only while the name resolves. tree_errors.go named its own exclusivity sweep in the
// plural, one letter off from the singular the function is declared under, and the sentence
// read exactly as it would have if the enforcement existed and had been deleted.
//
// This plan's files and NOT the package, which is a narrowing with a reason rather than a
// convenience. Six comments elsewhere in package mls cite a test another plan has not landed
// yet and say so in the same sentence -- the validation plan's ValSem401 refusal, p7's
// counterpart to the external key pair, a task 12 test whose three properties this package
// already covers harder -- and a forward reference to scheduled work is prose doing its job.
// This plan makes no such reference and owes the stronger rule.
func TestThisPlansFilesCiteNoTestThatDoesNotExist(t *testing.T) {
	declared := packageLevelDeclarations(t, ".")
	cited := 0
	for _, file := range treePlanSourceFiles(t) {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for _, name := range testNameCitation.FindAllString(commentLineWrap.ReplaceAllString(string(body), ""), -1) {
			cited++
			if _, found := declared[name]; !found {
				t.Errorf("%s names %s and package mls declares no test under that name, so the enforcement that sentence points at cannot be followed to anything",
					file, name)
			}
		}
	}
	if cited == 0 {
		t.Fatal("this plan's files name no test at all, which cannot be true of files that are all gates, so the scan read something other than them")
	}
}
