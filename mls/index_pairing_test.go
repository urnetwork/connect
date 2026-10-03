// The loops that pair two sequences by one index, as a class read off the source.
//
// WHY THIS IS A CLASS AND NOT A LIST. A loop that walks two sequences together can pair entry i of
// one with entry j of the other, and what that produces is not a crash: it is a value of the right
// type, the right length and the right shape with its parts standing under the wrong labels. The
// round that reported sweeping this class named six sites, of which four were in it, and left
// fifteen unexamined; and the mutation it swept with was the weakest member there is -- pinning an
// index to zero -- which on (*GroupPolicyExtension).Clone was caught only INCIDENTALLY, by an
// index-out-of-range panic inside a test whose stated property is non-aliasing and which never
// compared the clone to its source. So the class is derived here, and the mutation used against it
// preserves order and length: out[len(out)-1-i] writes every entry, writes none of them twice and
// leaves no zero behind.
//
// WHAT IS IN THE CLASS. A loop with a position variable whose body indexes two or more ORDINAL
// sequences by an expression mentioning that variable, counting the sequence a range statement
// ranges over as one of them. Ordinal means a value positions are read out of by an integer: a
// slice, an array, a pointer to one, or a string.
//
// A MAP IS NOT ONE, and that exclusion is the rule's own rather than an exemption written beside
// it. A loop that inserts into a map under a key taken from the other sequence pairs nothing
// positional -- a set built by inserting the same elements in another order IS the same set, so
// there is no mispairing for any test to observe. Measured rather than argued: the three such loops
// in validate_proposals.go were reversed together and the whole of ./mls/... and ./message/...
// stayed green at 1974 passing, which is what a program identical to its unmutated self does.
//
// THE INDEX MAY BE AN EXPRESSION and not only the bare variable, which is what keeps this rule
// reading a site after somebody mispairs it: out.Roles[len(self.Roles)-1-i] names no bare i, and a
// reading that demanded one would drop the site out of the class on the very commit that broke it.
//
// AND A KEY NAMES A SITE RATHER THAN A NAME, which is a repair. The key was the function plus the
// sorted sequence names and nothing else, so a SECOND loop pairing the same two sequences inside an
// already-named function collided with the first and was certified by its row -- the coverage table
// is a map, one row cleared both loops, and the discrepancy between the loop count and the distinct
// key count was written into a t.Logf and asserted nowhere. Repeated keys now carry an occurrence
// ordinal, " #2" onward in source order, so the second loop is a key with no row and fails on the
// commit that adds it. TestEveryIndexPairedLoopIsHeldByATestThatSeesItsPairing asserts the two counts
// are equal as well, which is what says the numbering really made them unique; and the CONTROL holds
// the colliding pair -- PairsTwoSlicesTwiceInOneFunction is two loops over one pair of sequences
// inside one function, and both of its keys stand in the expected report -- so a build that stopped
// numbering fails at the control before it reaches this package's own source.
//
// NEITHER OF THOSE TWO SENTENCES WAS TRUE WHEN THIS PARAGRAPH FIRST CLAIMED THEM, which is recorded
// here rather than quietly repaired. The control contained no colliding pair, and len(derived)
// appeared exactly once in this file, inside the closing t.Logf -- so neutralising the ordinal and
// adding a second mispaired loop to an already-named function left the gate logging "20 index paired
// loop(s) at 19 distinct site(s)" and PASSING. A claim written into the record that the code does
// not support is the same defect this whole file is about, pointed at the gate instead of at the
// package.
//
// THE ORDINAL IS POSITIONAL WITHIN ITS FUNCTION, and the cost of that is worth stating: a new loop
// inserted ABOVE an existing one takes the unsuffixed key and pushes the existing loop to " #2", so
// the row that was measured against the old loop then names the new one and a " #2" row is missing.
// That fails rather than passes, which is the direction that matters, and the repair is to re-measure
// both.
package mls

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"go/types"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// indexPairedLoopCoverage is every member of the class with the test that FAILS when its pairing is
// broken, recorded from a mutation run rather than assumed. A site with no row is a site nothing was
// measured against, which is the state this whole class was in.
//
// The rows name ONE failing test each and not all of them. What the row is for is the reader who
// breaks a pairing and wants to know whether anything can see it; where a mutation took several
// tests down, the row names the one whose stated property IS the pairing.
var indexPairedLoopCoverage = map[string]string{
	"applyReuseGuard [guarded reuseGuard]": "TestApplyReuseGuardXorsEveryBitOfTheGuardAndNothingBeyondIt",
	// and only that one, out of 1974 passing tests: it is the only fixture in the package that
	// commits two Adds, and over one Add every pairing this loop makes is element zero with
	// element zero.
	"(*StagedCommit).welcomeMessage [adds self.added]":                                                "TestACommitAddingTwoMembersPairsEachJoinerWithItsOwnKeyPackageAndItsOwnLeaf",
	"pathSecretAt [plan.Path plan.PathSecrets]":                                                       "TestTheWelcomePathSecretIsTheOneForTheLowestNodeTheJoinerAndCommitterShare",
	"(*RatchetTree).installJoinerPathSecrets [chain steps]":                                           "TestJoinFromWelcomeLandsOnTheCommittersLadderOverAPathCommit",
	"(*GroupPolicyExtension).Clone [out.Roles self.Roles]":                                            "TestGroupPolicyCloneAnswersEveryRoleEntryInItsOwnPlace",
	"compareMemberIds [left right]":                                                                   "TestCompareMemberIdsAnswersWhatBytesCompareAnswers",
	"(*HpkeContext).nonce [nonce self.baseNonce]":                                                     "TestHpkeContextSealMatchesThePublishedEncryptions",
	"(*RatchetTree).Clone [out.nodes self.nodes]":                                                     "TestAGroupOfThreePublishesATreeItsOwnDecoderAcceptsBack",
	"reconcileWithGroupContext [ctx.GroupExtensions gc.Extensions]":                                   "TestValidateAgainstContextComparesEveryEntryOfTheExtensionsVectorAndBothHalvesOfEach",
	"(*RatchetTree).CreateUpdatePathSecrets [path pathSecrets publicKeys]":                            "TestCreateUpdatePathSecretsGivesTheLeafAnIndependentKey",
	"(*RatchetTree).CreateUpdatePathSecrets [path publicKeys]":                                        "TestCreateUpdatePathSecretsInstallsAChainThatVerifies",
	"(*RatchetTree).EncryptUpdatePath [plan.Path plan.PathSecrets plan.PublicKeys targets]":           "TestEncryptUpdatePathProducesOneCiphertextPerResolutionNode",
	"(*RatchetTree).MergeUpdatePath [path.Nodes steps]":                                               "TestMergeUpdatePathReproducesTheSendersTree",
	"updatePathCiphertextsMatchTheTargets [path.Nodes targets]":                                       "TestDecryptUpdatePathAgreesOnTheCommitSecret",
	"(*RatchetTree).DecryptUpdatePath [path.Nodes[start].EncryptedPathSecret targets[start]]":         "TestDecryptUpdatePathOpensTheCiphertextStandingAtItsOwnResolutionIndex",
	"(*RatchetTree).DecryptUpdatePath [path.Nodes steps]":                                             "TestDecryptUpdatePathAgreesOnTheCommitSecret",
	"(*CommitValidationInput).checkListResolvesTheCommitsVector [order vector]":                       "TestAFieldOfACachedProposalWithNoJoinRefusesTheCommit",
	"(*CommitValidationInput).checkExtensionsAreTheSetThisCommitInstalls [installed self.Extensions]": "TestProcessCommitStagesRatherThanMerges",
	"ValSem203PathDecrypt [in.Commit.Path.Nodes targets]":                                             "TestEveryCommitThisGroupGeneratesPassesItsOwnValidateCommit",
}

// One loop of the class: what it pairs, and where it stands.
type indexPairedLoop struct {
	key   string
	where string
}

// The position variables a for statement introduces. A range statement has at most one -- its key
// -- and a three clause for has whatever its init declares, which is what lets `for i, j := 0, 0`
// be read as two.
func indexPairingLoopVariables(node ast.Node) []string {
	switch loop := node.(type) {
	case *ast.RangeStmt:
		if name, isIdent := loop.Key.(*ast.Ident); isIdent && name.Name != "_" {
			return []string{name.Name}
		}
	case *ast.ForStmt:
		names := []string{}
		if assign, isAssign := loop.Init.(*ast.AssignStmt); isAssign {
			for _, lhs := range assign.Lhs {
				if name, isIdent := lhs.(*ast.Ident); isIdent && name.Name != "_" {
					names = append(names, name.Name)
				}
			}
		}
		return names
	}
	return nil
}

// Whether an expression mentions any of the position variables, which is what makes an index an
// index into THIS loop rather than a constant one.
func indexPairingMentions(node ast.Node, names []string) bool {
	found := false
	ast.Inspect(node, func(inner ast.Node) bool {
		if name, isIdent := inner.(*ast.Ident); isIdent && slices.Contains(names, name.Name) {
			found = true
		}
		return true
	})
	return found
}

// Whether a type is one positions are read out of by an integer. See the header for why a map is
// deliberately not, and why that is the rule rather than an exemption.
func indexPairingIsOrdinal(one types.Type) bool {
	if one == nil {
		return false
	}
	switch spelled := types.Unalias(one).Underlying().(type) {
	case *types.Slice:
		return true
	case *types.Array:
		return true
	case *types.Pointer:
		_, isArray := types.Unalias(spelled.Elem()).Underlying().(*types.Array)
		return isArray
	case *types.Basic:
		return spelled.Info()&types.IsString != 0
	}
	return false
}

func indexPairingRender(fileSet *token.FileSet, node ast.Node) string {
	out := &bytes.Buffer{}
	if err := printer.Fprint(out, fileSet, node); err != nil {
		return "<unprintable>"
	}
	return out.String()
}

// The name a failure names a site by: the function it stands in and the sequences it pairs.
//
// NOT THE LINE. A position moves whenever a line above it does, and a table keyed by position goes
// stale on every unrelated edit; what a reader needs in order to open the right loop is the function
// and the names.
func indexPairingKeyOf(function string, sequences []string) string {
	return function + " [" + strings.Join(sequences, " ") + "]"
}

// Every loop of the class in one set of type checked files.
//
// The occurrence counter is what keeps two loops that pair the same sequences inside one function
// from sharing a key. See the header for why that mattered and for what the ordinal costs.
func indexPairedLoopsIn(fileSet *token.FileSet, files []*ast.File, info *types.Info) []indexPairedLoop {
	found := []indexPairedLoop{}
	occurrences := map[string]int{}
	for _, file := range files {
		for _, declaration := range file.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil {
				continue
			}
			name := function.Name.Name
			if function.Recv != nil && len(function.Recv.List) != 0 {
				name = "(" + indexPairingRender(fileSet, function.Recv.List[0].Type) + ")." + name
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				variables := indexPairingLoopVariables(node)
				if len(variables) == 0 {
					return true
				}
				var body *ast.BlockStmt
				sequences := []string{}
				add := func(expression ast.Expr) {
					if !indexPairingIsOrdinal(info.Types[expression].Type) {
						return
					}
					if text := indexPairingRender(fileSet, expression); !slices.Contains(sequences, text) {
						sequences = append(sequences, text)
					}
				}
				switch loop := node.(type) {
				case *ast.RangeStmt:
					body = loop.Body
					add(loop.X)
				case *ast.ForStmt:
					body = loop.Body
				}
				if body == nil {
					return true
				}
				ast.Inspect(body, func(inner ast.Node) bool {
					index, isIndex := inner.(*ast.IndexExpr)
					if isIndex && indexPairingMentions(index.Index, variables) {
						add(index.X)
					}
					return true
				})
				if len(sequences) < 2 {
					return true
				}
				slices.Sort(sequences)
				base := indexPairingKeyOf(name, sequences)
				occurrences[base] += 1
				key := base
				if at := occurrences[base]; at != 1 {
					key = base + " #" + strconv.Itoa(at)
				}
				found = append(found, indexPairedLoop{
					key:   key,
					where: fileSet.Position(node.Pos()).String(),
				})
				return true
			})
		}
	}
	return found
}

// A fixture holding one of every shape the rule has to tell apart, so a reading that stopped
// matching fails here rather than issuing the real source the clean bill a working one issues.
//
// Every member is here because some half of the rule has to be the only thing reporting it, or the
// only thing NOT reporting it:
//
//   - PairsTwoSlices is the member any reading finds.
//   - PairsARangedSliceWithAnIndexedOne is (*GroupPolicyExtension).Clone's shape, and the one the
//     previous sweep missed: the SOURCE is ranged and only the destination is indexed, so a reading
//     that looked at index expressions alone counts one sequence and clears it.
//   - PairsThroughAnOffsetIndex is the mispairing itself. A reading that demanded the bare loop
//     variable as the index would drop a site out of the class on the commit that broke it.
//   - PairsAStringWithASlice is the ordinal type that is not a slice.
//   - WritesAMapKeyedByTheOtherSequence and CopiesAMapUnderItsOwnKey are the negative half: there is
//     no position to mispair, and reversing either writes the identical map.
//   - IndexesOneSequence is the ordinary loop, which is what keeps the class from being every loop
//     in the package.
//   - RangesOverAnInteger indexes ONE sequence and ranges over a count; a reading that took every
//     range expression for a sequence would report it as a pair.
//   - PairsTwoSlicesTwiceInOneFunction is the COLLIDING PAIR, and it is the only member here whose
//     job is the key rather than the class: two loops over one pair of sequences inside one
//     function, which under the pre-ordinal key were one key and are now two. It is what makes a
//     build that stopped numbering fail at the control, and the expected report below carries both
//     of its keys rather than one.
const indexPairedLoopControl = `package control

type entry struct {
	id   []byte
	role uint8
}

func PairsTwoSlices(out []byte, in []byte) {
	for i := range in {
		out[i] = in[i]
	}
}

func PairsARangedSliceWithAnIndexedOne(out []entry, in []entry) {
	for i, one := range in {
		out[i] = one
	}
}

func PairsThroughAnOffsetIndex(out []byte, in []byte) {
	for i := range in {
		out[len(out)-1-i] = in[i]
	}
}

func PairsAStringWithASlice(out []byte, in string) {
	for i := range in {
		out[i] = in[i]
	}
}

func WritesAMapKeyedByTheOtherSequence(out map[uint8]bool, in []entry) {
	for i := range in {
		out[in[i].role] = true
	}
}

func CopiesAMapUnderItsOwnKey(out map[uint8][]byte, in map[uint8][]byte) {
	for k, v := range in {
		out[k] = v
	}
}

func IndexesOneSequence(out []byte) {
	for i := range out {
		out[i] = 0
	}
}

func RangesOverAnInteger(out []byte, n int) {
	for i := range n {
		out[i] = 0
	}
}

func PairsTwoSlicesTwiceInOneFunction(out []byte, in []byte) {
	for i := range in {
		out[i] = in[i]
	}
	for i := range in {
		out[i] = out[i] ^ in[i]
	}
}
`

// What the rule must report over the fixture, exactly rather than as a floor: a reading that
// widened to flag every loop fails here as surely as one that stopped matching.
var indexPairedLoopControlReports = []string{
	"PairsARangedSliceWithAnIndexedOne [in out]",
	"PairsAStringWithASlice [in out]",
	"PairsThroughAnOffsetIndex [in out]",
	"PairsTwoSlices [in out]",
	"PairsTwoSlicesTwiceInOneFunction [in out]",
	"PairsTwoSlicesTwiceInOneFunction [in out] #2",
}

// TestTheIndexPairingRuleFlagsItsControlFixture runs before the rule over the real source, so a
// reading that stopped matching fails here rather than certifying the package.
func TestTheIndexPairingRuleFlagsItsControlFixture(t *testing.T) {
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, "index_paired_control.go", indexPairedLoopControl,
		parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse the index pairing control: %v", err)
	}
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}}
	if _, err := (&types.Config{}).Check("control", fileSet, []*ast.File{parsed}, info); err != nil {
		t.Fatalf("type check the index pairing control: %v", err)
	}
	reported := []string{}
	for _, one := range indexPairedLoopsIn(fileSet, []*ast.File{parsed}, info) {
		reported = append(reported, one.key)
	}
	slices.Sort(reported)
	if !slices.Equal(reported, indexPairedLoopControlReports) {
		t.Errorf("the rule reported %v over the control, want %v", reported, indexPairedLoopControlReports)
	}
}

// TestEveryIndexPairedLoopIsHeldByATestThatSeesItsPairing is the gate: the class, derived off this
// package's own type checked source, held against what was measured over each of its members.
//
// BOTH DIRECTIONS. A site with no row fails, which is what makes the next one somebody writes fail
// on the commit that adds it; and a row naming no site fails, because a table that only had to be a
// superset would go on certifying a class it no longer covers.
func TestEveryIndexPairedLoopIsHeldByATestThatSeesItsPairing(t *testing.T) {
	source := typeCheckedPackageWithBodies(t)
	loops := indexPairedLoopsIn(source.fileSet, source.files, source.info)
	if len(loops) == 0 {
		t.Fatal("no index paired loop was found in this package's production source, so this gate demanded nothing")
	}
	derived := []string{}
	for _, one := range loops {
		if _, covered := indexPairedLoopCoverage[one.key]; !covered {
			t.Errorf("%s at %s pairs two sequences by one index and no test is recorded against it; apply an order-preserving mispairing -- out[len(out)-1-i] -- and record the test that fails",
				one.key, one.where)
		}
		if !slices.Contains(derived, one.key) {
			derived = append(derived, one.key)
		}
	}
	for key := range indexPairedLoopCoverage {
		if !slices.Contains(derived, key) {
			t.Errorf("the coverage table names %s, which is not a loop of this class; a row that outlived its loop covers nothing and hides that it does",
				key)
		}
	}
	// and every test a row names is a test that EXISTS, which is the half a coverage table gets
	// wrong quietly: a row naming a test somebody renamed or deleted reads exactly like a row
	// naming one that runs, and the site it covers is then covered by nothing at all.
	declared := testFunctionsDeclaredInThisPackage(t)
	if len(declared) == 0 {
		t.Fatal("no test function was read out of this package, so the row check below demanded nothing")
	}
	for key, named := range indexPairedLoopCoverage {
		if !slices.Contains(declared, named) {
			t.Errorf("the row for %s names %s, which this package declares no test by; a row naming no test covers nothing",
				key, named)
		}
	}
	// AND THE TWO COUNTS ARE COMPARED, which is the half this file claimed and did not have. The
	// coverage table is a MAP, so a second loop sharing a key with the first is certified by the
	// first loop's row and the only trace of it is that there are more loops than keys. Measured on
	// the shape this closes: with the occurrence ordinal neutralised and a second mispaired loop
	// added to an already-named function, this gate logged "20 index paired loop(s) at 19 distinct
	// site(s)" and passed.
	if len(loops) != len(derived) {
		t.Errorf("%d loops of this class stand at %d distinct keys, so at least two of them share one row and one measurement is certifying a loop it was never made against; the occurrence ordinal is what keeps them apart",
			len(loops), len(derived))
	}
	slices.Sort(derived)
	t.Logf("%d index paired loop(s) at %d distinct site(s), %d of them named by a declared test: %v",
		len(loops), len(derived), len(indexPairedLoopCoverage), derived)
}

// testFunctionsDeclaredInThisPackage is every Test function of this package's own test source, read
// off the files rather than from a list, so the check above is against what really runs.
func testFunctionsDeclaredInThisPackage(t *testing.T) []string {
	t.Helper()
	found := []string{}
	for _, path := range packageSourcePaths(t) {
		if !strings.HasSuffix(path, "_test.go") {
			continue
		}
		for _, declaration := range mustParseSource(t, path).file.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if isFunction && function.Recv == nil && strings.HasPrefix(function.Name.Name, "Test") {
				found = append(found, function.Name.Name)
			}
		}
	}
	return found
}
