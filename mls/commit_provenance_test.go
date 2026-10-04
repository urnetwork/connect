// The provenance a staged commit carries, and the door that reads it.
//
// (*Group).ApplyCommit checked the Kind, the nil, the closed flag and RemovesSelf and NOTHING about
// where the commit it was handed came from. Measured, by deleting the two refusals this file exists
// for and running the first case below: two independent groups A and B, and receiverB.ApplyCommit
// given a Processed receiverA had staged answered nil, moved B out of epoch 1 into the epoch A's
// commit opened, and left the two groups answering one epoch authenticator. Processed and its Commit
// field are exported and connect/message holds Processed values across a policy decision, so two
// groups' results are two values of one type in one caller's hands: this is the expected caller
// shape rather than a contrived one.
package mls

import (
	"bytes"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"slices"
	"strings"
	"testing"
)

// TestApplyCommitRefusesACommitAnotherGroupStaged is the measurement above, run as a test.
//
// TWO GROUPS WITH DIFFERENT IDS, which the fixture has to be asked for: every group this client is
// a member of runs an epoch 1, so two groups sharing an id would let this pass on the epoch check
// and say nothing about the group one.
func TestApplyCommitRefusesACommitAnotherGroupStaged(t *testing.T) {
	crypto := testCrypto(t)
	committerA, receiverA, _, _, _ := testTwoMemberGroupNamed(t, crypto, "provenance-a")
	defer committerA.Close()
	defer receiverA.Close()
	committerB, receiverB, _, _, _ := testTwoMemberGroupNamed(t, crypto, "provenance-b")
	defer committerB.Close()
	defer receiverB.Close()

	result, err := committerA.CreateCommit(nil, nil, nil)
	if err != nil {
		t.Fatalf("group A CreateCommit: %v", err)
	}
	processed, err := receiverA.ProcessMessage(result.Commit)
	if err != nil {
		t.Fatalf("group A ProcessMessage: %v", err)
	}

	epochBefore := receiverB.Epoch()
	authenticatorBefore := bytes.Clone(receiverB.EpochAuthenticator())
	if err := receiverB.ApplyCommit(processed); !errors.Is(err, errApplyCommitNotThisGroups) {
		t.Fatalf("group B ApplyCommit over group A's staged commit = %v, want errApplyCommitNotThisGroups", err)
	}
	if receiverB.Epoch() != epochBefore {
		t.Fatalf("the refused ApplyCommit moved group B from epoch %d to epoch %d", epochBefore, receiverB.Epoch())
	}
	if !bytes.Equal(receiverB.EpochAuthenticator(), authenticatorBefore) {
		t.Fatal("the refused ApplyCommit changed group B's epoch authenticator")
	}
	// and the two groups still do not agree on an authenticator, which is the symptom itself rather
	// than the refusal: an adopted epoch is one B derived out of A's key schedule.
	if err := receiverA.ApplyCommit(processed); err != nil {
		t.Fatalf("group A's own receiver could not apply its own staged commit: %v", err)
	}
	if bytes.Equal(receiverA.EpochAuthenticator(), receiverB.EpochAuthenticator()) {
		t.Fatal("two independent groups answer one epoch authenticator")
	}
	// B is unharmed rather than merely unmoved: it still ingests its own peer's commits.
	resultB, err := committerB.CreateCommit(nil, nil, nil)
	if err != nil {
		t.Fatalf("group B CreateCommit after the refusal: %v", err)
	}
	processedB, err := receiverB.ProcessMessage(resultB.Commit)
	if err != nil {
		t.Fatalf("group B ProcessMessage after the refusal: %v", err)
	}
	if err := receiverB.ApplyCommit(processedB); err != nil {
		t.Fatalf("group B ApplyCommit of its own peer's commit after the refusal: %v", err)
	}
}

// TestApplyCommitRefusesACommitStagedAgainstAnotherEpoch is the other half of the binding, in BOTH
// directions -- an epoch this group has already left and an epoch it has not reached -- because a
// rule written as one comparison is one edit away from being written as an inequality, and a case
// for one direction alone cannot tell the two apart.
func TestApplyCommitRefusesACommitStagedAgainstAnotherEpoch(t *testing.T) {
	crypto := testCrypto(t)
	committer, receiver, _, _, material := testTwoMemberGroupNamed(t, crypto, "provenance-epoch")
	defer committer.Close()
	defer receiver.Close()

	// a SECOND view of this same group at the epoch the welcome describes, which is what makes the
	// forward direction observable: it holds the group id the binding compares and sits at an epoch
	// the staged commits below are not staged against.
	laggard, err := material.join(t, nil)
	if err != nil {
		t.Fatalf("the second join this case lags with: %v", err)
	}
	defer laggard.Close()

	first, err := committer.CreateCommit(nil, nil, nil)
	if err != nil {
		t.Fatalf("the first CreateCommit: %v", err)
	}
	staged, err := receiver.ProcessMessage(first.Commit)
	if err != nil {
		t.Fatalf("the first ProcessMessage: %v", err)
	}
	if err := receiver.ApplyCommit(staged); err != nil {
		t.Fatalf("the first ApplyCommit: %v", err)
	}
	if err := committer.MergePendingCommit(); err != nil {
		t.Fatalf("the first MergePendingCommit: %v", err)
	}

	// BACKWARD: a value of this group staged against the epoch it has just left. The laggard --
	// a second view at the epoch the commit was staged against -- stages the same commit and
	// never applies it, so what the receiver is handed is a LIVE staged epoch of its own group
	// and of no epoch it is in, and the epoch comparison is the only thing that refuses it.
	//
	// It used to be the receiver's OWN value handed back after it was applied, and that value is
	// now refused one door earlier, by name: an installed value is a shell, and
	// staged_commit_install_test.go holds that the shell is read before this pair. So the shape
	// this arm is about -- a Processed held across a policy decision and handed back late -- is
	// built here from a value that still holds its epoch, which is the only value the pair can
	// be the refusal of.
	stale, err := laggard.ProcessMessage(first.Commit)
	if err != nil {
		t.Fatalf("the laggard's ProcessMessage of the first commit: %v", err)
	}
	// the laggard's staged epoch is erased when this case is done with it, which is the
	// discipline every drop site of a staged epoch is held to
	defer stale.Commit.Zeroize()
	if stale.Commit.installed() || stale.Commit.Epoch() != staged.Commit.Epoch() {
		t.Fatal("the laggard's staged value is not a live staged epoch of the commit the receiver applied, so this arm observes something else")
	}
	if err := receiver.ApplyCommit(stale); !errors.Is(err, errApplyCommitNotThisEpochs) {
		t.Fatalf("ApplyCommit of a live staged commit of the epoch this group has left = %v, want errApplyCommitNotThisEpochs", err)
	}
	if receiver.Epoch() != committer.Epoch() {
		t.Fatalf("the replayed ApplyCommit moved the receiver to epoch %d, committer at %d",
			receiver.Epoch(), committer.Epoch())
	}
	// and the value the receiver installed, handed back, is named for what it is
	if err := receiver.ApplyCommit(staged); !errors.Is(err, errStagedCommitInstalled) {
		t.Fatalf("ApplyCommit of the value this receiver installed = %v, want errStagedCommitInstalled", err)
	}

	// FORWARD: a commit staged against an epoch the laggard has not reached.
	second, err := committer.CreateCommit(nil, nil, nil)
	if err != nil {
		t.Fatalf("the second CreateCommit: %v", err)
	}
	ahead, err := receiver.ProcessMessage(second.Commit)
	if err != nil {
		t.Fatalf("the second ProcessMessage: %v", err)
	}
	if laggard.Epoch() >= ahead.Commit.Epoch() {
		t.Fatalf("the laggard is at epoch %d and the staged commit opens epoch %d, so this arm is not the forward one",
			laggard.Epoch(), ahead.Commit.Epoch())
	}
	authenticatorBefore := bytes.Clone(laggard.EpochAuthenticator())
	if err := laggard.ApplyCommit(ahead); !errors.Is(err, errApplyCommitNotThisEpochs) {
		t.Fatalf("ApplyCommit of a commit staged two epochs on = %v, want errApplyCommitNotThisEpochs", err)
	}
	if !bytes.Equal(laggard.EpochAuthenticator(), authenticatorBefore) {
		t.Fatal("the refused ApplyCommit moved the laggard's epoch")
	}
}

// ---------------------------------------------------------------------------
// the construction class
// ---------------------------------------------------------------------------

// stagedCommitProvenanceFields is what ApplyCommit compares a staged commit against the group it is
// handed to. A construction that names neither is a value that door clears by comparing two zero
// values.
var stagedCommitProvenanceFields = []string{"groupId", "priorEpoch"}

// One site that brings a StagedCommit VALUE into existence: how it is spelled, the function or type
// it stands in, and -- for the one spelling this rule can read -- the fields it names.
type stagedCommitConstruction struct {
	key   string
	how   string
	named []string
	where string
}

// stagedCommitConstructionsIn derives the class off the TYPE CHECKER rather than off the source text
// of a literal's type expression, and the difference is two whole spellings.
//
// The reading this replaces matched a composite literal whose written type rendered as the string
// "StagedCommit" and a call to new() whose written argument did the same. That reads two of the four
// ways this package can bring one into existence, and it was the SPELLING it could not read that was
// dangerous: `var staged StagedCommit` followed by field assignment is precisely the shape that
// yields a nil groupId and a zero priorEpoch, and the old rule cleared it in silence. It also missed
// an elided literal -- []StagedCommit{{...}} writes no type at the element at all -- so a literal
// inside a slice or a map was invisible to the half of the rule that checks the fields.
//
// The four members of the class, and why each is in it:
//
//   - a composite literal, which is the one spelling this rule can READ the fields of;
//   - new(StagedCommit), which names nothing;
//   - a var declaration of the value type with no initialiser, which is the zero value plus whatever
//     the next lines happen to assign;
//   - a struct field of the value type, which is a zero StagedCommit inside somebody else's value and
//     travels wherever that value does.
//
// A PARAMETER OF THE VALUE TYPE IS NOT IN THE CLASS and a `var staged *StagedCommit` is not either,
// which is the rule's own boundary rather than an exemption written beside it: a parameter receives a
// value that was constructed somewhere this rule already reads, and a nil pointer is not a
// StagedCommit. The control fixture holds one of each so that a reading which widened to flag them
// fails there rather than issuing findings against correct code.
func stagedCommitConstructionsIn(fileSet *token.FileSet, files []*ast.File,
	info *types.Info) []stagedCommitConstruction {

	isStagedCommit := func(one types.Type) bool {
		if one == nil {
			return false
		}
		named, isNamed := types.Unalias(one).(*types.Named)
		return isNamed && named.Obj() != nil && named.Obj().Name() == "StagedCommit"
	}
	found := []stagedCommitConstruction{}
	for _, file := range files {
		for _, declaration := range file.Decls {
			switch spelled := declaration.(type) {
			case *ast.FuncDecl:
				if spelled.Body == nil {
					continue
				}
				name := spelled.Name.Name
				if spelled.Recv != nil && len(spelled.Recv.List) != 0 {
					name = "(" + indexPairingRender(fileSet, spelled.Recv.List[0].Type) + ")." + name
				}
				ast.Inspect(spelled.Body, func(node ast.Node) bool {
					switch inner := node.(type) {
					case *ast.CallExpr:
						builtin, isIdent := inner.Fun.(*ast.Ident)
						if isIdent && builtin.Name == "new" && len(inner.Args) == 1 &&
							isStagedCommit(info.Types[inner.Args[0]].Type) {
							found = append(found, stagedCommitConstruction{
								key: name + " new []", how: "new",
								where: fileSet.Position(inner.Pos()).String(),
							})
						}
					case *ast.ValueSpec:
						if len(inner.Values) != 0 || inner.Type == nil ||
							!isStagedCommit(info.Types[inner.Type].Type) {
							return true
						}
						found = append(found, stagedCommitConstruction{
							key: name + " var []", how: "var",
							where: fileSet.Position(inner.Pos()).String(),
						})
					case *ast.CompositeLit:
						if !isStagedCommit(info.Types[inner].Type) {
							return true
						}
						named := []string{}
						positional := false
						for _, element := range inner.Elts {
							pair, isPair := element.(*ast.KeyValueExpr)
							if !isPair {
								positional = true
								continue
							}
							named = append(named, indexPairingRender(fileSet, pair.Key))
						}
						how := "literal"
						if positional {
							how = "positional literal"
						}
						slices.Sort(named)
						found = append(found, stagedCommitConstruction{
							key: name + " " + how + " [" + strings.Join(named, " ") + "]",
							how: how, named: named,
							where: fileSet.Position(inner.Pos()).String(),
						})
					}
					return true
				})
			case *ast.GenDecl:
				for _, specification := range spelled.Specs {
					typed, isType := specification.(*ast.TypeSpec)
					if !isType {
						continue
					}
					structure, isStruct := typed.Type.(*ast.StructType)
					if !isStruct || structure.Fields == nil {
						continue
					}
					for _, field := range structure.Fields.List {
						if !isStagedCommit(info.Types[field.Type].Type) {
							continue
						}
						for _, fieldName := range field.Names {
							found = append(found, stagedCommitConstruction{
								key: typed.Name.Name + " field " + fieldName.Name, how: "field",
								where: fileSet.Position(field.Pos()).String(),
							})
						}
					}
				}
			}
		}
	}
	return found
}

// A fixture holding one of every spelling the rule has to tell apart, so a reading that stopped
// matching fails here rather than issuing the real source the clean bill a working one issues.
//
// Every member is here because some half of the rule has to be the only thing reporting it, or the
// only thing NOT reporting it. ThroughVar and InsideASlice are the two the previous reading missed;
// APointerVarIsNotAConstruction and AParameterIsNotAConstruction are the negative half, and a rule
// that widened to flag either would issue findings against correct code.
const stagedCommitConstructionControl = `package control

type StagedCommit struct {
	groupId    []byte
	priorEpoch uint64
	epoch      uint64
}

type Holder struct {
	staged StagedCommit
}

func KeyedLiteralCarryingBoth() *StagedCommit {
	return &StagedCommit{groupId: nil, priorEpoch: 0}
}

func KeyedLiteralMissingProvenance() *StagedCommit {
	return &StagedCommit{epoch: 1}
}

func PositionalLiteral() *StagedCommit {
	return &StagedCommit{nil, 0, 1}
}

func ThroughNew() *StagedCommit {
	return new(StagedCommit)
}

func ThroughVar() *StagedCommit {
	var staged StagedCommit
	staged.epoch = 1
	return &staged
}

func InsideASlice() []StagedCommit {
	return []StagedCommit{{groupId: nil, priorEpoch: 0}}
}

func APointerVarIsNotAConstruction() *StagedCommit {
	var staged *StagedCommit
	return staged
}

func AParameterIsNotAConstruction(staged StagedCommit) uint64 {
	return staged.epoch
}
`

// What the rule must report over the fixture, EXACTLY rather than as a floor: a reading that widened
// to flag every declaration fails here as surely as one that stopped matching.
var stagedCommitConstructionControlReports = []string{
	"Holder field staged",
	"InsideASlice literal [groupId priorEpoch]",
	"KeyedLiteralCarryingBoth literal [groupId priorEpoch]",
	"KeyedLiteralMissingProvenance literal [epoch]",
	"PositionalLiteral positional literal []",
	"ThroughNew new []",
	"ThroughVar var []",
}

// TestTheStagedCommitConstructionRuleFlagsItsControlFixture runs before the rule over the real
// source, so a reading that stopped matching fails here rather than certifying the package.
func TestTheStagedCommitConstructionRuleFlagsItsControlFixture(t *testing.T) {
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, "staged_commit_construction_control.go",
		stagedCommitConstructionControl, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse the staged commit construction control: %v", err)
	}
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}}
	if _, err := (&types.Config{}).Check("control", fileSet, []*ast.File{parsed}, info); err != nil {
		t.Fatalf("type check the staged commit construction control: %v", err)
	}
	reported := []string{}
	for _, one := range stagedCommitConstructionsIn(fileSet, []*ast.File{parsed}, info) {
		reported = append(reported, one.key)
	}
	slices.Sort(reported)
	if !slices.Equal(reported, stagedCommitConstructionControlReports) {
		t.Errorf("the rule reported %v over the control, want %v", reported, stagedCommitConstructionControlReports)
	}
}

// TestEveryStagedCommitCarriesTheGroupAndEpochThatStagedIt is the gate over the CLASS rather than
// over the two doors this task closed: a staged commit built anywhere in this package without its
// provenance is a value ApplyCommit clears by comparing two zero values, and the site that builds one
// need not be one of the three that exist today.
//
// A construction spelled any way but a keyed composite literal is refused OUTRIGHT rather than
// skipped, because it is a construction whose fields this rule cannot read, and a rule that cannot
// read a construction clears it.
func TestEveryStagedCommitCarriesTheGroupAndEpochThatStagedIt(t *testing.T) {
	source := typeCheckedPackageWithBodies(t)
	constructions := stagedCommitConstructionsIn(source.fileSet, source.files, source.info)
	literals := 0
	for _, one := range constructions {
		switch one.how {
		case "literal":
			literals += 1
			for _, want := range stagedCommitProvenanceFields {
				if !slices.Contains(one.named, want) {
					t.Errorf("%s at %s stages a commit without %s; ApplyCommit compares that field against the group it is handed to, and a zero one is a commit any group at epoch 0 adopts",
						one.key, one.where, want)
				}
			}
		case "positional literal":
			t.Errorf("%s at %s builds a StagedCommit positionally, so this rule cannot say which field is which; write it keyed",
				one.key, one.where)
		default:
			t.Errorf("%s at %s brings a StagedCommit into existence as a %s, which names no field at all and yields a nil groupId and a zero priorEpoch; build it as a keyed composite literal so its provenance is visible here",
				one.key, one.where, one.how)
		}
	}
	if literals == 0 {
		t.Fatal("no StagedCommit literal was found in this package's production source, so this rule demanded nothing")
	}
	t.Logf("%d StagedCommit construction site(s), %d of them keyed literals carrying %v",
		len(constructions), literals, stagedCommitProvenanceFields)
}
