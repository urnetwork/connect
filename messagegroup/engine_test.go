// Spec A section 6's interface, held to its own block, and the connect/mls adapter that
// satisfies it.
//
// The value of a narrow interface is entirely in what it REFUSES, so most of this file is about
// what is not on it. Section 6:
//
//	Note what is not on this interface: no tree, no node, no secret tree, no HPKE, no
//	epoch_secret, no confirmation_key, no membership_key, no ciphersuite internals.
package messagegroup

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/printer"
	"go/token"
	"go/types"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect/mls"
	"github.com/urnetwork/connect/mls/syntax"
)

// ---------------------------------------------------------------------------
// Property 1: the interface is closed, and the closure is derived
// ---------------------------------------------------------------------------

// Section 6's block, transcribed from the spec and NOT normalised against connect/mls's own
// signatures.
//
// The signatures are here in full rather than the names alone because the names are the half a
// widening does not touch: a method whose RESULT quietly became mls.LeafIndex would keep its name
// and stop being a seam, and that is mutation 6 of this task's own list.
//
// The rendering is go/printer's over the parsed field, so a spelling difference in whitespace is
// not a failure and a difference in TYPE is.
var sectionSixGroupEngine = map[string]string{
	"Suite":           "func() uint16",
	"NewKeyPackage":   "func() (keyPackage []byte, err error)",
	"CreateGroup":     "func(groupId []byte, policy []byte, leafKeys []byte) (GroupHandle, error)",
	"JoinFromWelcome": "func(welcome []byte, ratchetTree []byte) (GroupHandle, error)",
	// THE FIFTH, AND IT IS AN AMENDMENT RATHER THAN A TRANSCRIPTION. Section 6's block has four
	// and none of them opens a persisted group, so a durable store was write-only: sdk carried its
	// own twenty-six-method GroupHandle over mls.LoadGroup, and two of those methods -- Process and
	// ApplyCommit -- could not be written at all, because EngineProcessed's staged half is
	// unexported. A restored group therefore could not ingest a commit. That was open item J1-8 and
	// LoadGroup is the change it named.
	"LoadGroup": "func(groupId []byte, epoch uint64) (GroupHandle, error)",
}

var sectionSixGroupHandle = map[string]string{
	"GroupId":      "func() []byte",
	"Epoch":        "func() uint64",
	"OwnLeafIndex": "func() uint32",
	"MemberCount":  "func() int",
	"MemberAt":     "func(i int) (leafIndex uint32, identityPub []byte, leafKeys []byte, err error)",
	// AMENDED 2026-09-22 for ledger item 242's R4, OBSERVER read-only: the second projection of
	// the membership snapshot MemberAt already builds, keyed by LEAF INDEX because every caller of
	// it arrives holding a leaf that an open authenticated, and answering connect/mls's own
	// Role.String() because a uint8 here would be a re-export of mls.Role under another name.
	"RoleAt": "func(leaf uint32) (identityPub []byte, role string, err error)",

	"Export":              "func(label string, context []byte, length int) ([]byte, error)",
	"PairwiseExport":      "func(label string, peer uint32, length int) ([]byte, error)",
	"SenderDataSecret":    "func() ([]byte, error)",
	"EncryptionSecret":    "func() ([]byte, error)",
	"EpochAuthenticator":  "func() []byte",
	"RatchetTreeSnapshot": "func() ([]byte, error)",
	"GroupContextBytes":   "func() ([]byte, error)",

	"ProposeAdd":         "func(keyPackage []byte) ([]byte, error)",
	"ProposeRemove":      "func(leafIndex uint32) ([]byte, error)",
	"ProposeUpdate":      "func() ([]byte, error)",
	"ProposeGroupPolicy": "func(policy []byte) ([]byte, error)",

	"Commit":    "func(byReference [][]byte) (commit []byte, welcome []byte, ratchetTree []byte, err error)",
	"CommitAdd": "func(keyPackages [][]byte) (commit []byte, welcome []byte, ratchetTree []byte, err error)",
	// AMENDED 2026-09-21 for ledger item 242's R1, the role model's seam plumbing: three more
	// by-value arms beside CommitAdd, each a commit carrying exactly the named proposals by value
	// and nothing by reference, and each taking go types -- ExtensionBytes is this package's
	// spelling of an extension, and a leaf is the uint32 every other method carries. CommitRemove
	// is on the seam so the receiving arm can be tested against a real Remove; the sdk exposes no
	// product method over it until items 243, 244 and 245 close.
	"CommitContextExtensions": "func(extensions []ExtensionBytes) (commit []byte, welcome []byte, ratchetTree []byte, err error)",
	"CommitPolicy":            "func(policy []byte) (commit []byte, welcome []byte, ratchetTree []byte, err error)",
	"CommitRemove":            "func(leaves []uint32) (commit []byte, welcome []byte, ratchetTree []byte, err error)",
	// AMENDED 2026-09-25 for ledger item 257's ruling 51, the removal track's seam arm: the one
	// commit that carries a Remove AND the policy that stops it naming an identity with no leaf.
	// CommitRemove above cannot ship a removal on its own -- a bare Remove of a NAMED identity's
	// last leaf is an R0c phantom at every honest receiver -- and Commit(nil), the only other door
	// onto a multi-proposal commit, is the by-reference fold item 242's ruling 13 forbids in
	// production. It is two go-typed parameters and not a proposal list because
	// TestNoMethodOfEitherEngineInterfaceNamesAConnectMlsType errors on any qualified type in
	// either interface's signature, which is what refuses a general CommitProposals outright.
	"CommitRemoveWithExtensions": "func(leaves []uint32, extensions []ExtensionBytes) (commit []byte, welcome []byte, ratchetTree []byte, err error)",
	// AMENDED 2026-09-22 for ledger item 242's R2, the role model's committing arm: the two
	// reads that let a committer submit before it merges. The record that announces an epoch
	// carries that epoch's facts -- write and read keys through its exporter, a hash of its
	// context, the wrap count, the number -- and until these the only door onto them was the
	// live handle after MergePendingCommit, so every committer merged first and a loser of
	// MASTER section 9.3's race forked itself. PendingEpoch is the three facts that are not key
	// material in one value; PendingExport is Export through the staged schedule, a call and
	// not a field so the secret is derived on demand and erased by the caller.
	"PendingEpoch":       "func() (*PendingEpoch, error)",
	"PendingExport":      "func(label string, context []byte, length int) ([]byte, error)",
	"MergePendingCommit": "func() error",
	"ClearPendingCommit": "func()",

	"Process":     "func(message []byte) (*EngineProcessed, error)",
	"ApplyCommit": "func(processed *EngineProcessed) error",
	// AMENDED 2026-09-21 for the same item: the discard door for a commit a receiving client
	// processed and then REFUSED. A refused commit is a fully derived epoch the caller holds and
	// nothing else erases, and the staged half is unexported, so only a method of this package
	// can reach it to erase it.
	"DiscardProcessed": "func(processed *EngineProcessed) error",

	// AMENDED 2026-09-17 for MASTER section 8.4.2 v2, transcribed from spec A section 8.2's own
	// amendment block. Three changes, all forced by the generation being inside aad_mls: the seal
	// takes a BUILDER because it cannot take a value, the open answers the generation because R2
	// is a function of it, and the peek answers the generation because R3 requires R2 to be
	// decided before any ratchet moves. Protect is KEPT for a caller whose aad is a constant, and
	// TestNoProductionSiteReachesTheUnboundProtect is what keeps the record layer off it.
	"Protect":      "func(aad []byte, plaintext []byte) ([]byte, error)",
	"ProtectBound": "func(aad func(generation uint32) ([]byte, error), plaintext []byte) ([]byte, error)",
	"Unprotect":    "func(message []byte) (aad []byte, plaintext []byte, senderLeaf uint32, generation uint32, err error)",
	"PeekSender":   "func(frame []byte) (senderLeaf uint32, aad []byte, generation uint32, err error)",

	"Close": "func() error",
}

// The names section 6 says are NOT on the interface, and the reason each is named.
//
// It is a MESSAGE and not the mechanism: the mechanism is the complete transcription above, and a
// method absent from that fails whether or not it is named here. This is what the failure says.
var sectionSixRefusesTheseNames = map[string]string{
	"SecretTree":      "no secret tree",
	"EpochSecret":     "no epoch_secret -- and reaching it by name would reach confirmation_key and membership_key through the same door",
	"ConfirmationKey": "no confirmation_key",
	"MembershipKey":   "no membership_key",
	"RatchetTree":     "no tree. RatchetTreeSnapshot answers opaque octets and is a different thing from a tree accessor",
	"Hpke":            "no HPKE",
	"Node":            "no node",
	"Ciphersuite":     "no ciphersuite internals",
}

// engineInterfaceMethods reads one interface's method set off the syntax tree of engine.go.
//
// It renders each signature through go/printer over the parsed field, so the comparison is
// against the TYPE and not against a spelling.
func engineInterfaceMethods(t *testing.T, name string) map[string]string {
	t.Helper()
	fileSet, sources := messagegroupProductionSources(t)
	methods := map[string]string{}
	found := false
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			general, isGeneral := declaration.(*ast.GenDecl)
			if !isGeneral || general.Tok != token.TYPE {
				continue
			}
			for _, spec := range general.Specs {
				typeSpec, isType := spec.(*ast.TypeSpec)
				if !isType || typeSpec.Name.Name != name {
					continue
				}
				declared, isInterface := typeSpec.Type.(*ast.InterfaceType)
				if !isInterface {
					t.Fatalf("%s is not an interface, so this gate is reading something other than section 6's block", name)
				}
				found = true
				for _, method := range declared.Methods.List {
					if len(method.Names) != 1 {
						t.Fatalf("%s carries an embedded interface, which section 6's block does not; every method of it is a design decision and an embed hides a set of them", name)
					}
					rendered := &bytes.Buffer{}
					if err := printer.Fprint(rendered, fileSet, method.Type); err != nil {
						t.Fatalf("render %s.%s: %v", name, method.Names[0].Name, err)
					}
					methods[method.Names[0].Name] = rendered.String()
				}
			}
		}
	}
	if !found {
		t.Fatalf("no interface named %s was read out of this package's production source, so this gate is holding nothing to section 6's block", name)
	}
	return methods
}

func TestTheEngineInterfacesAreExactlySectionSixsBlock(t *testing.T) {
	for _, subject := range []struct {
		name  string
		block map[string]string
	}{
		{name: "GroupEngine", block: sectionSixGroupEngine},
		{name: "GroupHandle", block: sectionSixGroupHandle},
	} {
		declared := engineInterfaceMethods(t, subject.name)
		if len(declared) == 0 {
			t.Fatalf("%s declares no method at all, so this gate read nothing", subject.name)
		}
		for name, want := range subject.block {
			got, isDeclared := declared[name]
			if !isDeclared {
				t.Errorf("%s.%s is in section 6's block and is not declared; the block is the interface and a method missing from it is a consumer that cannot be written",
					subject.name, name)
				continue
			}
			if got != want {
				t.Errorf("%s.%s is declared %s and section 6 writes %s; a signature that drifted is a seam that has stopped being one",
					subject.name, name, got, want)
			}
		}
		for name := range declared {
			if _, isInBlock := subject.block[name]; isInBlock {
				continue
			}
			reason := "section 6's block does not carry it, and adding a method here is a design decision rather than a convenience: everything on this interface is something a replacement implementation must provide"
			for refused, why := range sectionSixRefusesTheseNames {
				if strings.Contains(name, refused) {
					reason = "section 6 says of this interface: " + why
					break
				}
			}
			t.Errorf("%s.%s is declared and %s", subject.name, name, reason)
		}
	}
	// the two counts section 6 was measured at, so a block that gained a method AND a
	// transcription row in one edit is still a failure somebody has to look at.
	//
	// IT WAS 4 AND 23 UNTIL 2026-09-17 and 4 and 25 from MASTER section 8.4.2 v2: ProtectBound
	// and PeekSender are the two spec A section 8.2's amendment adds, and Unprotect's signature
	// moved beside them. Three changes to the seam, made deliberately with the ruling in hand,
	// which is exactly the event this pair of numbers exists to make somebody look at.
	//
	// AND IT IS 4 AND 26 FROM LEDGER ITEM 228's read-receipt tag ruling: PairwiseExport is the
	// one method that ruling adds, and it is one method rather than an accessor for a leaf
	// private key precisely so that no leaf scalar crosses this seam.
	//
	// AND IT IS 5 AND 26 FROM 2026-09-18's LoadGroup, which closes open item J1-8: the ENGINE half
	// gains its first method since section 6 was written, and the HANDLE half gains none -- which
	// is the shape the amendment was chosen for. A restored group is the same connectMlsHandle a
	// founded one is, so it reaches Process and ApplyCommit through the same bodies rather than
	// through a second implementation that cannot write EngineProcessed's unexported staged half.
	//
	// AND IT IS 5 AND 27 FROM 2026-09-18's CommitAdd, ledger item 239's group chats: the HANDLE
	// half gains the by-value commit arm. It is one method taking octets rather than a widening
	// of Commit, because Commit's one parameter is the by-reference vector and a second
	// parameter naming mls.Proposal would be the re-export Property 3 refuses; and it is on the
	// seam at all because the property it buys -- a member that never saw a proposal processes
	// the commit -- is one no caller of Commit can reach through Commit.
	//
	// AND IT IS 5 AND 31 FROM 2026-09-21's R1 of the role model, ledger item 242: the HANDLE
	// half gains four. CommitContextExtensions, CommitPolicy and CommitRemove are CommitAdd's
	// shape over the other three proposal types the receiving arm has to be tested against --
	// by value, so a receiver needs nothing cached -- and DiscardProcessed is the erase door for a
	// commit a receiving client processed and refused, which no caller could otherwise erase
	// because the staged half is unexported. Four changes to the seam, made with the ruling in
	// hand, which is again the event this pair of numbers exists to make somebody look at.
	//
	// AND IT IS 5 AND 33 FROM 2026-09-22's R2 of the same item: the HANDLE half gains
	// PendingEpoch and PendingExport, the reads off a handle's OWN staged commit that let the
	// committing arm submit before it merges, so that a committer which loses the epoch race
	// clears a staged epoch instead of forking itself off the group. Two methods and not four,
	// for the reason the interface gives; and two rather than none because the alternative --
	// keep merging first -- is the fork the seam's own Commit contract names.
	//
	// AND IT IS 5 AND 34 FROM 2026-09-22's R4 of the same item, OBSERVER read-only: the HANDLE
	// half gains RoleAt, ONE method and the only one R4 needs. It reads the Role field
	// (*mls.Group).Members() already resolves and MemberAt throws away, keyed by leaf index rather
	// than by ordinal because a record's sender is authenticated to a LEAF. It is on this seam
	// rather than in the sdk because the answer has to be read off the handle of the record's OWN
	// epoch -- item 242's ruling 17 -- and the only door onto a prior epoch's handle is the
	// session, behind this interface.
	// AND IT IS 5 AND 35 FROM 2026-09-25's X4 STEP 1, ledger item 257's ruling 51: the HANDLE half
	// gains CommitRemoveWithExtensions, ONE method, and it is the whole of what the removal track
	// needs from this seam. A removal of a NAMED identity's last leaf cannot ship as a bare
	// CommitRemove -- the policy would go on naming an identity with no leaf, which every honest
	// receiver refuses as an R0c phantom -- so the Remove and the GroupContextExtensions that
	// drops the entry have to leave in ONE commit, and the only other door onto a multi-proposal
	// commit is the by-reference fold item 242's ruling 13 forbids in production. One method and
	// not two: the ruling refuses a ...WithPolicy convenience beside it, because the sdk builds
	// the full extension list itself and a second arm here would be this package deciding what a
	// removal's post-commit policy is. And ZERO methods for the removed-leaf set the same ruling
	// derives, which is the other half of it: that read went on the PendingEpoch value this
	// interface already answers, so the seam grew a field rather than a thirty-sixth method.
	if len(sectionSixGroupEngine) != 5 || len(sectionSixGroupHandle) != 35 {
		t.Errorf("section 6's block is transcribed as %d and %d methods; it was measured at 5 and 35, and a change to either is a change to the seam",
			len(sectionSixGroupEngine), len(sectionSixGroupHandle))
	}
}

// ---------------------------------------------------------------------------
// Property 3: no method of either interface names a connect/mls type
// ---------------------------------------------------------------------------

// This is the property that holds the boundary and the one an implementer is pushed to break.
//
// The cheap way out of any friction between section 6's block and group.go's method set is to
// change the INTERFACE until *mls.Group fits -- widen OwnLeafIndex to mls.LeafIndex and delete
// the conversion, which is the first reshape anybody reaches for. An interface that names mls's
// types has stopped being a seam and become a re-export of connect/mls, and Gate 5's swap becomes
// a type change rather than a factory change.
//
// The scope question (R3a) is answered separately from the class question: the CLASS is the
// method set of both interfaces read off the syntax tree, which is 40 members at this commit --
// 5 and 35, and the figure written here was the HANDLE half alone until 2026-09-25, which is a
// count its own sentence does not describe -- and is never a list; the SCOPE is engine.go,
// because that is the file section 2.2's tree puts the
// interface in and an interface declared anywhere else would fail
// TestThisPackageIsBuiltFromExactlyTheseImports before it reached here.
func TestNoMethodOfEitherEngineInterfaceNamesAConnectMlsType(t *testing.T) {
	_, sources := messagegroupProductionSources(t)
	judged := 0
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			general, isGeneral := declaration.(*ast.GenDecl)
			if !isGeneral || general.Tok != token.TYPE {
				continue
			}
			for _, spec := range general.Specs {
				typeSpec, isType := spec.(*ast.TypeSpec)
				if !isType {
					continue
				}
				if typeSpec.Name.Name != "GroupEngine" && typeSpec.Name.Name != "GroupHandle" {
					continue
				}
				declared, isInterface := typeSpec.Type.(*ast.InterfaceType)
				if !isInterface {
					continue
				}
				for _, method := range declared.Methods.List {
					judged += 1
					ast.Inspect(method.Type, func(node ast.Node) bool {
						selector, isSelector := node.(*ast.SelectorExpr)
						if !isSelector {
							return true
						}
						qualifier, isIdentifier := selector.X.(*ast.Ident)
						if !isIdentifier {
							return true
						}
						t.Errorf("%s.%s names %s.%s in its signature; Gate 5 puts MLS behind this interface, and an interface that names a type from another package is a re-export of it rather than a seam over it",
							typeSpec.Name.Name, method.Names[0].Name, qualifier.Name, selector.Sel.Name)
						return true
					})
				}
			}
		}
	}
	if judged != len(sectionSixGroupEngine)+len(sectionSixGroupHandle) {
		t.Fatalf("this gate judged %d methods and section 6's block has %d; a class that is not the whole method set is a class the next method joins outside of",
			judged, len(sectionSixGroupEngine)+len(sectionSixGroupHandle))
	}
}

// ---------------------------------------------------------------------------
// Property 3b: the unbound seal has no production caller
// ---------------------------------------------------------------------------

// TestNoProductionSiteReachesTheUnboundProtect is what keeps GroupHandle.Protect from being the way
// back to a frame whose aad does not name its generation.
//
// WHY THE METHOD IS STILL ON THE INTERFACE. Spec A section 8.2's amendment keeps it, for a caller
// whose aad is a constant, and removing a published method is a larger call than adding one. What
// that leaves behind is a door that compiles: an application record sealed through Protect gets an
// aad_mls naming whatever generation the caller guessed, and every peer refuses it at R2 for a
// reason the sender cannot see. Nothing in the type system says so.
//
// SO THE RULE IS STATED HERE RATHER THAN HELD BY AN ABSENCE, which is MASTER section 8.4.7 (3)'s
// own argument about ReceiverKey applied one layer up: "there is no caller to have learned it from"
// is not a rule, because the next caller is the defect and nothing is looking for it. This is what
// looks for it.
//
// THE CLASS IS EVERY .Protect( CALL IN THIS PACKAGE'S PRODUCTION SOURCE and not a list of files, so
// a second seal site added next month is judged without anybody extending this. It is the SELECTOR
// and not the receiver's type, because the AST alone does not know types -- which makes the gate
// broader than the rule rather than narrower, and a production caller that genuinely needed a
// constant aad would have to come here and say so.
//
// EXACTLY ONE SITE IS EXEMPT AND IT IS THE INTERFACE METHOD ITSELF. (*connectMlsHandle).Protect
// delegates to (*mls.Group).Protect, because the method is on GroupHandle and something has to
// implement it; exempting the body of the method of that name is not a hole, because a SEAL site
// that hid inside it would be a seal site inside the one function whose whole content is one
// delegation. Anything else, in any file, is a failure.
//
// MUTATION: put self.handle.Protect(aad[:], bodyPlain) back into frameBodyOnLoop and this goes red.
func TestNoProductionSiteReachesTheUnboundProtect(t *testing.T) {
	fileSet, sources := messagegroupProductionSources(t)
	found := 0
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil {
				continue
			}
			// the delegation the interface forces: func (self *connectMlsHandle) Protect(...)
			if function.Name.Name == "Protect" && function.Recv != nil {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, isCall := node.(*ast.CallExpr)
				if !isCall {
					return true
				}
				selector, isSelector := call.Fun.(*ast.SelectorExpr)
				if !isSelector || selector.Sel.Name != "Protect" {
					return true
				}
				found += 1
				t.Errorf("%s calls .Protect, and an application record's aad_mls names the generation the frame is sealed at -- which Protect cannot know. MASTER section 8.4.2's S1: the seal must go through ProtectBound",
					fileSet.Position(call.Pos()))
				return true
			})
		}
	}
	// and the gate is looking at something: ProtectBound IS reached from production, so a build
	// where this scan read nothing at all reports rather than passing on an empty class.
	bound := 0
	for _, source := range sources {
		ast.Inspect(source.parsed, func(node ast.Node) bool {
			call, isCall := node.(*ast.CallExpr)
			if !isCall {
				return true
			}
			selector, isSelector := call.Fun.(*ast.SelectorExpr)
			if isSelector && selector.Sel.Name == "ProtectBound" {
				bound += 1
			}
			return true
		})
	}
	if bound == 0 {
		t.Fatal("no production site in this package calls ProtectBound, so this scan is reading a package with no seal in it and the refusal above is vacuous")
	}
	t.Logf("%d production calls to ProtectBound and %d to Protect", bound, found)
}

// ---------------------------------------------------------------------------
// Property 4: mls.Group is nameable in exactly one file
// ---------------------------------------------------------------------------

// Gate 5's actual content, and the exemption is by SCANNED PATH rather than by base name -- a base
// name exemption is the exemption shape this project keeps rediscovering.
//
// The COUNT of files taking the exemption is asserted, so a second cannot quietly join.
func TestOnlyOneProductionFileOfThisPackageNamesMlsGroup(t *testing.T) {
	_, sources := messagegroupProductionSources(t)
	naming := map[string]int{}
	for _, source := range sources {
		ast.Inspect(source.parsed, func(node ast.Node) bool {
			selector, isSelector := node.(*ast.SelectorExpr)
			if !isSelector {
				return true
			}
			qualifier, isIdentifier := selector.X.(*ast.Ident)
			if !isIdentifier || qualifier.Name != "mls" || selector.Sel.Name != "Group" {
				return true
			}
			naming[source.path] += 1
			return true
		})
	}
	if len(naming) == 0 {
		t.Fatal("no production file of this package names mls.Group, so the adapter that is supposed to wrap one is not there and this gate is reporting clean having read nothing")
	}
	allowed := ""
	for path := range naming {
		if strings.HasSuffix(strings.ReplaceAll(path, "\\", "/"), "/engine.go") ||
			strings.ReplaceAll(path, "\\", "/") == "engine.go" {
			allowed = path
			continue
		}
		t.Errorf("%s names mls.Group and is not the adapter's own file; section 2.2's tree pairs the interface and the adapter in engine.go, and a second file naming the group is a second place the seam is not a seam",
			path)
	}
	if allowed == "" {
		t.Error("no file whose path ends in engine.go names mls.Group; the exemption is by scanned path and the path it names has moved")
	}
	if len(naming) != 1 {
		t.Errorf("%d production files name mls.Group and the count this gate holds is 1: %v",
			len(naming), slices.Sorted(maps.Keys(naming)))
	}
}

// ---------------------------------------------------------------------------
// Task 9 Property 2 and Task 9a Property 3: stagedRef and Raw
// ---------------------------------------------------------------------------

// stagedRef is unexported, which is the half Task 9 could check off the syntax tree, and Raw is
// exported, which is what makes the never-inspected half a contract rather than a compile error.
func TestEngineProcessedKeepsItsStagedReferenceUnexported(t *testing.T) {
	_, sources := messagegroupProductionSources(t)
	fields := map[string]bool{}
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			general, isGeneral := declaration.(*ast.GenDecl)
			if !isGeneral || general.Tok != token.TYPE {
				continue
			}
			for _, spec := range general.Specs {
				typeSpec, isType := spec.(*ast.TypeSpec)
				if !isType || typeSpec.Name.Name != "EngineProcessed" {
					continue
				}
				structure, isStruct := typeSpec.Type.(*ast.StructType)
				if !isStruct {
					t.Fatal("EngineProcessed is not a struct, so this gate is reading something other than section 6's block")
				}
				for _, field := range structure.Fields.List {
					for _, name := range field.Names {
						fields[name.Name] = name.IsExported()
					}
				}
			}
		}
	}
	if len(fields) == 0 {
		t.Fatal("EngineProcessed declares no fields, so this gate read nothing")
	}
	if exported, isDeclared := fields["stagedRef"]; !isDeclared || exported {
		t.Errorf("EngineProcessed.stagedRef is declared=%v exported=%v; it is unexported so that only a member of this package can POPULATE one, which is the whole of section 6's unforgeability argument",
			isDeclared, exported)
	}
	for _, exportedField := range []string{"Kind", "SenderLeaf", "Aad", "Plaintext", "Raw"} {
		if exported, isDeclared := fields[exportedField]; !isDeclared || !exported {
			t.Errorf("EngineProcessed.%s is declared=%v exported=%v; section 6's block exports it",
				exportedField, isDeclared, exported)
		}
	}
}

// Property 3's relocated half: no reader of an EngineProcessed inspects Raw.
//
// The scope question (R3a) is answered separately: the CLASS is every production declaration of
// this package whose body reads a field of an EngineProcessed, derived off the syntax tree and
// never listed; the SCOPE is this package's production source, because EngineProcessed is
// declared here and a reader in another package can only reach the exported fields, which is a
// different property with a different owner.
//
// It FATALS on an empty class. Measured at Task 9's commit the class had no members at all, which
// is why this half could not land there: a derived class gate over an empty class reports the
// clean run of a complete gate.
func TestNoReaderOfAnEngineProcessedInspectsRaw(t *testing.T) {
	_, sources := messagegroupProductionSources(t)
	fields := engineProcessedFieldNames(t)
	readers := []string{}
	inspectors := []string{}
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil {
				continue
			}
			readsAField := false
			inspectsRaw := false
			ast.Inspect(function.Body, func(node ast.Node) bool {
				selector, isSelector := node.(*ast.SelectorExpr)
				if isSelector && fields[selector.Sel.Name] {
					readsAField = true
				}
				// what "inspects" means, read off the SHAPE rather than off a list of
				// function names: Raw indexed, sliced, compared, measured, ranged over or
				// handed to any call.
				if isSelector && selector.Sel.Name == "Raw" {
					inspectsRaw = inspectsRaw || engineRawIsInspectedAt(function.Body, selector)
				}
				return true
			})
			if readsAField {
				readers = append(readers, function.Name.Name)
			}
			if inspectsRaw {
				inspectors = append(inspectors, function.Name.Name)
			}
		}
	}
	slices.Sort(readers)
	if len(readers) == 0 {
		t.Fatal("no production declaration of this package reads a field of an EngineProcessed, so this gate is reporting clean having read nothing; the class had no members at task 9's commit and this half was relocated to the commit where it first has one")
	}
	if len(inspectors) != 0 {
		t.Errorf("%v index, parse, compare or measure EngineProcessed.Raw; section 6 makes it opaque so that a staged commit can be carried across a policy decision without connect/messagegroup being able to read or forge it",
			inspectors)
	}
	t.Logf("%d production declaration(s) read an EngineProcessed: %v", len(readers), readers)
}

// The producer half of Property 3: what this adapter WRITES.
//
// Batch C measured the gap this closes. Nothing behavioural in wave 1 can drive Process at all --
// a one member group has no inbound message, because the joining member is wave 2's and the
// adapter's JoinFromWelcome refuses -- so "the staged commit goes in stagedRef and never in Raw"
// had no case that could fail. Deleting the stagedRef the adapter puts there SURVIVED the whole
// suite.
//
// So it is read off the source instead. The scope question (R3a): the SCOPE is this package's
// production source, because stagedRef is unexported and only a member of this package can
// populate one -- which is the whole of what section 6's unforgeability argument confines.
//
// THE CLASS IS WHAT THE VALUE CARRIES WHEN IT LEAVES THE FUNCTION, and it had to be re-derived
// once already for exactly the reason this batch was sent to close: the first version read only
// COMPOSITE LITERALS, so an assignment one statement later escaped it. Measured -- inserting
// `answer.stagedRef = nil` immediately after the literal in (*connectMlsHandle).Process survived
// all three trees while making ApplyCommit refuse every commit this engine processed, and
// `stagedRef: any(nil)` escaped the same way because the nil check read an *ast.Ident. So:
//
//   - the class of PRODUCERS is every production declaration answering a *EngineProcessed, read
//     off the signature. It fatals on an empty class, so a refactor that stopped building them
//     here fails rather than reporting clean.
//   - every producer must WRITE stagedRef at least once, where a write is a keyed element of an
//     EngineProcessed literal or an assignment through a .stagedRef selector -- both shapes,
//     because both reach the field.
//   - and every such write ANYWHERE in production source, inside a producer or not, must not be
//     nil. "Is nil" is asked of go/types rather than of the syntax, after unwrapping parentheses
//     and conversions, so any(nil) and (nil) are the same answer the bare identifier gives.
//
// The limit, stated rather than hidden: a write of a VARIABLE that happens to hold nil is outside
// this reading, because deciding that needs dataflow rather than a type. Every shape the review
// walked past is inside it.
func TestEveryEngineProcessedThisPackageBuildsCarriesAStagedCommit(t *testing.T) {
	info, sources := repairTypeCheckProduction(t)
	producers, literals, writes := 0, 0, 0
	for _, file := range sources {
		for _, declaration := range file.parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil {
				continue
			}
			isProducer := engineAnswersAnEngineProcessed(function)
			if isProducer {
				producers += 1
			}
			written := 0
			ast.Inspect(function.Body, func(node ast.Node) bool {
				switch typed := node.(type) {
				case *ast.CompositeLit:
					named, isNamed := typed.Type.(*ast.Ident)
					if !isNamed || named.Name != "EngineProcessed" {
						return true
					}
					literals += 1
					staged := ast.Expr(nil)
					for _, element := range typed.Elts {
						pair, isPair := element.(*ast.KeyValueExpr)
						if !isPair {
							continue
						}
						if key, isKey := pair.Key.(*ast.Ident); isKey && key.Name == "stagedRef" {
							staged = pair.Value
						}
					}
					if staged == nil {
						t.Errorf("%s builds an EngineProcessed with no stagedRef; a value staged by this engine and carried in Raw instead is one this package can read and rebuild, which is exactly what section 6's unforgeability sentence is about",
							file.path)
						return true
					}
					written += 1
					writes += 1
					if engineExpressionIsNil(info, staged) {
						t.Errorf("%s builds an EngineProcessed whose stagedRef is nil; ApplyCommit then has nothing unforgeable to check and whatever the caller needs must have gone into Raw",
							file.path)
					}
				case *ast.AssignStmt:
					for at, target := range typed.Lhs {
						selector, isSelector := target.(*ast.SelectorExpr)
						if !isSelector || selector.Sel.Name != "stagedRef" || len(typed.Rhs) <= at {
							continue
						}
						written += 1
						writes += 1
						if engineExpressionIsNil(info, typed.Rhs[at]) {
							t.Errorf("%s assigns nil to a stagedRef after the value was built; a processed message that leaves this package with no staged commit is one ApplyCommit refuses, and no behavioural case in wave 1 can drive Process at all",
								file.path)
						}
					}
				}
				return true
			})
			if isProducer && written == 0 {
				t.Errorf("%s answers a *EngineProcessed and writes no stagedRef; section 6's unforgeability is that only a member of this package can populate that field, and a producer that populates none has handed the caller a value ApplyCommit refuses",
					function.Name.Name)
			}
		}
	}
	if producers == 0 {
		t.Fatal("no production declaration of this package answers a *EngineProcessed, so this gate is reporting clean having read nothing")
	}
	if literals == 0 {
		t.Fatal("no production declaration of this package builds an EngineProcessed, so the literal half of this gate read nothing")
	}
	t.Logf("%d producer(s), %d EngineProcessed literal(s), %d stagedRef write(s) in production source",
		producers, literals, writes)
}

// engineAnswersAnEngineProcessed reads the SIGNATURE: any result that is a *EngineProcessed.
func engineAnswersAnEngineProcessed(function *ast.FuncDecl) bool {
	if function.Type.Results == nil {
		return false
	}
	for _, field := range function.Type.Results.List {
		star, isStar := field.Type.(*ast.StarExpr)
		if !isStar {
			continue
		}
		if named, isNamed := star.X.(*ast.Ident); isNamed && named.Name == "EngineProcessed" {
			return true
		}
	}
	return false
}

// engineExpressionIsNil asks the type checker, after unwrapping parentheses and conversions, so
// nil, (nil) and any(nil) are one answer.
func engineExpressionIsNil(info *types.Info, expr ast.Expr) bool {
	for {
		switch typed := expr.(type) {
		case *ast.ParenExpr:
			expr = typed.X
			continue
		case *ast.CallExpr:
			// a CONVERSION and not a call: the callee names a type. A call answering a value is
			// left alone, which is what keeps a staging constructor out of this reading.
			if len(typed.Args) == 1 && info.Types[typed.Fun].IsType() {
				expr = typed.Args[0]
				continue
			}
		}
		break
	}
	if kind, isKnown := info.Types[expr]; isKnown {
		return kind.IsNil()
	}
	// nothing the checker saw: fall back to the identifier, so a gate over an expression the
	// info missed still reports the obvious shape rather than silently passing it.
	identifier, isIdentifier := expr.(*ast.Ident)
	return isIdentifier && identifier.Name == "nil"
}

// engineProcessedFieldNames reads the field names off the DECLARATION, so a field added to
// section 6's struct joins the reader class with no edit here.
//
// It was a written down list of six names under a comment claiming it was derived, which is the
// defect class this batch was sent to close pointed at this batch's own work: a class derived
// from the instance in front of it rather than from the property it names. The seventh field
// would have been invisible to it.
func engineProcessedFieldNames(t *testing.T) map[string]bool {
	t.Helper()
	_, sources := messagegroupProductionSources(t)
	names := map[string]bool{}
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			general, isGeneral := declaration.(*ast.GenDecl)
			if !isGeneral || general.Tok != token.TYPE {
				continue
			}
			for _, spec := range general.Specs {
				typeSpec, isType := spec.(*ast.TypeSpec)
				if !isType || typeSpec.Name.Name != "EngineProcessed" {
					continue
				}
				structure, isStruct := typeSpec.Type.(*ast.StructType)
				if !isStruct {
					continue
				}
				for _, field := range structure.Fields.List {
					for _, name := range field.Names {
						names[name.Name] = true
					}
				}
			}
		}
	}
	if len(names) == 0 {
		t.Fatal("EngineProcessed declares no field this reading can see, so every gate keyed on its field set is reporting clean having read nothing")
	}
	return names
}

// engineRawIsInspectedAt answers whether one mention of .Raw stands in a position that reads its
// content: indexed, sliced, compared, measured, ranged over, or passed to a call.
//
// A WRITE is not an inspection, which is what keeps (*connectMlsHandle).Process -- whose whole
// dealing with Raw is to assign a copy of the message into it -- out of the class.
func engineRawIsInspectedAt(body ast.Node, mention *ast.SelectorExpr) bool {
	inspected := false
	ast.Inspect(body, func(node ast.Node) bool {
		switch found := node.(type) {
		case *ast.IndexExpr:
			inspected = inspected || found.X == ast.Expr(mention)
		case *ast.SliceExpr:
			inspected = inspected || found.X == ast.Expr(mention)
		case *ast.BinaryExpr:
			inspected = inspected || found.X == ast.Expr(mention) || found.Y == ast.Expr(mention)
		case *ast.RangeStmt:
			inspected = inspected || found.X == ast.Expr(mention)
		case *ast.CallExpr:
			for _, argument := range found.Args {
				if argument == ast.Expr(mention) {
					inspected = true
				}
			}
		}
		return true
	})
	return inspected
}

// ---------------------------------------------------------------------------
// Property 5: the enum names are the closed ones
// ---------------------------------------------------------------------------

// G6 is landed on the mls side; this asserts the client half does not route around it.
//
// The class is every mls.EpochSecret* constant this package's production source names, derived off
// the syntax tree, and the assertion is that it is exactly the two MASTER section 8.2 needs.
func TestOnlyTheTwoClosedEpochSecretNamesAreNamedHere(t *testing.T) {
	_, sources := messagegroupProductionSources(t)
	named := map[string]bool{}
	for _, source := range sources {
		ast.Inspect(source.parsed, func(node ast.Node) bool {
			selector, isSelector := node.(*ast.SelectorExpr)
			if !isSelector {
				return true
			}
			qualifier, isIdentifier := selector.X.(*ast.Ident)
			if !isIdentifier || qualifier.Name != "mls" {
				return true
			}
			if strings.HasPrefix(selector.Sel.Name, "EpochSecret") {
				named[selector.Sel.Name] = true
			}
			return true
		})
	}
	if len(named) == 0 {
		t.Fatal("this package names no mls.EpochSecret constant at all, so SenderDataSecret and EncryptionSecret are reaching the epoch's secrets some other way and this gate read nothing")
	}
	want := []string{"EpochSecretEncryption", "EpochSecretSenderData"}
	if got := slices.Sorted(maps.Keys(named)); !slices.Equal(got, want) {
		t.Errorf("this package names %v; the enum is CLOSED at %v, and a third name is epoch_secret, confirmation_key or membership_key reached through the same door",
			got, want)
	}
}

// ---------------------------------------------------------------------------
// Task 9a Property 1: the compile time assertions, in production source
// ---------------------------------------------------------------------------

// The property Task 9 could not state about *mls.Group, stated about the type that is meant to
// have it. A violation is a BUILD failure, which is the point; this reads the assertions off the
// source so that deleting them is a failure too.
func TestTheAdapterCarriesItsCompileTimeAssertions(t *testing.T) {
	fileSet, sources := messagegroupProductionSources(t)
	asserted := map[string]string{}
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			general, isGeneral := declaration.(*ast.GenDecl)
			if !isGeneral || general.Tok != token.VAR {
				continue
			}
			for _, spec := range general.Specs {
				value, isValue := spec.(*ast.ValueSpec)
				if !isValue || value.Type == nil || len(value.Names) != 1 || value.Names[0].Name != "_" {
					continue
				}
				declaredType, isIdentifier := value.Type.(*ast.Ident)
				if !isIdentifier {
					continue
				}
				rendered := &bytes.Buffer{}
				if err := printer.Fprint(rendered, fileSet, value.Values[0]); err != nil {
					t.Fatalf("render the assertion of %s: %v", declaredType.Name, err)
				}
				asserted[declaredType.Name] = rendered.String()
			}
		}
	}
	for _, want := range []struct{ iface, implementation string }{
		{iface: "GroupEngine", implementation: "(*connectMlsEngine)(nil)"},
		{iface: "GroupHandle", implementation: "(*connectMlsHandle)(nil)"},
	} {
		got, isAsserted := asserted[want.iface]
		if !isAsserted {
			t.Errorf("no production declaration asserts that anything satisfies %s; a build failure is the only thing that can hold this property and deleting the assertion removes it",
				want.iface)
			continue
		}
		if got != want.implementation {
			t.Errorf("%s is asserted over %s and the adapter is %s", want.iface, got, want.implementation)
		}
	}
}

// ---------------------------------------------------------------------------
// Property 4: every projection is total
// ---------------------------------------------------------------------------

// A projection that drops a bool is how a missing member becomes leaf 0.
func TestEveryProjectionOfTheAdapterIsTotal(t *testing.T) {
	fixture := newTestEngine(t)
	handle := fixture.createGroup(t, "totality")
	defer handle.Close()

	if count := handle.MemberCount(); count != 1 {
		t.Fatalf("a freshly founded group has %d members, want 1", count)
	}
	leaf, identityPub, leafKeys, err := handle.MemberAt(0)
	if err != nil {
		t.Fatalf("MemberAt(0): %v", err)
	}
	if leaf != handle.OwnLeafIndex() {
		t.Errorf("MemberAt(0) answered leaf %d and the founder holds %d", leaf, handle.OwnLeafIndex())
	}
	if !bytes.Equal(identityPub, fixture.identityPub) {
		t.Errorf("MemberAt(0) answered identity %x, want %x", identityPub, fixture.identityPub)
	}
	if !bytes.Equal(leafKeys, fixture.leafKeys) {
		t.Errorf("MemberAt(0) answered leaf keys %x, want %x", leafKeys, fixture.leafKeys)
	}
	// THE ABSENCES. Each is a place a projection that answered a zero value would hand the
	// caller something it would go on to use as a member, a key or a leaf.
	for _, ordinal := range []int{-1, 1, 2, 1 << 20} {
		leaf, identityPub, leafKeys, err := handle.MemberAt(ordinal)
		if err == nil {
			t.Errorf("MemberAt(%d) answered leaf %d with no error; an ordinal the membership does not have must be a refusal and never a zero member",
				ordinal, leaf)
		}
		if leaf != 0 || identityPub != nil || leafKeys != nil {
			t.Errorf("MemberAt(%d) answered leaf %d, %d octets of identity and %d octets of leaf keys beside its error",
				ordinal, leaf, len(identityPub), len(leafKeys))
		}
	}
	// a closed group reports through every accessor that can, rather than answering a zero.
	if err := handle.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for _, closed := range []struct {
		name string
		call func() ([]byte, error)
	}{
		{name: "Export", call: func() ([]byte, error) { return handle.Export("URmessage/v1/storage", nil, 32) }},
		{name: "SenderDataSecret", call: handle.SenderDataSecret},
		{name: "EncryptionSecret", call: handle.EncryptionSecret},
	} {
		answer, err := closed.call()
		if err == nil {
			t.Errorf("%s over a closed group answered %d octets and no error; a storage root computed over an empty exporter output is thirty two well formed octets no other member reproduces",
				closed.name, len(answer))
		}
		if answer != nil {
			t.Errorf("%s over a closed group answered %d octets beside its error", closed.name, len(answer))
		}
	}
	if authenticator := handle.EpochAuthenticator(); authenticator != nil {
		t.Errorf("EpochAuthenticator over a closed group answered %d octets; mls answers nil and section 6 gives this no error to report through",
			len(authenticator))
	}
}

// ---------------------------------------------------------------------------
// the adapter's behaviour
// ---------------------------------------------------------------------------

// The engine's own refusals, each of which is a value the caller cannot fix later.
func TestTheEngineRefusesEveryThingItCannotBeBuiltWithout(t *testing.T) {
	fixture := newTestEngine(t)
	crypto, err := mls.NewCryptoProvider(mls.CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	signer, identityPub, err := crypto.SignatureKeyPair()
	if err != nil {
		t.Fatalf("SignatureKeyPair: %v", err)
	}
	cred := mls.BasicCredential(identityPub)
	for _, refusal := range []struct {
		name string
		call func() (GroupEngine, error)
		want error
	}{
		{name: "no crypto provider", want: ErrEngineCryptoProvider, call: func() (GroupEngine, error) {
			return NewConnectMlsEngine(nil, newMemoryStateStore(), signer, cred, fixture.leafKeys)
		}},
		{name: "no state store", want: ErrEngineStateStore, call: func() (GroupEngine, error) {
			return NewConnectMlsEngine(crypto, nil, signer, cred, fixture.leafKeys)
		}},
		{name: "no signer", want: ErrEngineSigner, call: func() (GroupEngine, error) {
			return NewConnectMlsEngine(crypto, newMemoryStateStore(), nil, cred, fixture.leafKeys)
		}},
		{name: "no leaf keys", want: ErrEngineLeafKeys, call: func() (GroupEngine, error) {
			return NewConnectMlsEngine(crypto, newMemoryStateStore(), signer, cred, nil)
		}},
		{name: "a leaf keys body that is not one", want: ErrEngineLeafKeys, call: func() (GroupEngine, error) {
			return NewConnectMlsEngine(crypto, newMemoryStateStore(), signer, cred, []byte{0x00, 0x01})
		}},
	} {
		engine, err := refusal.call()
		if !errorIs(err, refusal.want) {
			t.Errorf("building an engine with %s answered %v, want %v", refusal.name, err, refusal.want)
		}
		if engine != nil {
			t.Errorf("building an engine with %s answered an engine beside its error", refusal.name)
		}
	}
}

// TestJoinFromWelcomeRefusesAndSaysWhatIsMissing is INVERTED rather than deleted, and it keeps its
// name so that the record of what it used to assert is not lost with it.
//
// It used to hold a refusal that named a gap in connect/mls's exported surface, and the two shapes
// its own comment rejected were: a join that answered a handle built on a signature key this device
// does not hold, and a second assembly of KeyPackageTBS beside a second spelling of its signature
// label. NEITHER IS WHAT LANDED. The label and the preimage have exactly one assembly each in
// package mls, and the leaf this device publishes names the key it signs with -- so the refusal's
// own reason is gone and the case becomes the positive statement it was standing in for.
//
// BOTH HALVES ARE HELD HERE. The join answers a HANDLE over a real Welcome, and it still refuses
// octets that are not one -- by name, and with no handle beside the error.
func TestJoinFromWelcomeRefusesAndSaysWhatIsMissing(t *testing.T) {
	fixture := newTestEngine(t)
	handle, err := fixture.engine.JoinFromWelcome([]byte("a welcome"), []byte("a tree"))
	if !errorIs(err, ErrEngineWelcomeShape) {
		t.Errorf("JoinFromWelcome answered %v, want ErrEngineWelcomeShape", err)
	}
	if handle != nil {
		t.Error("JoinFromWelcome answered a handle beside its error, which is a member built on a key this device does not hold")
	}

	// THE INVERSION: a real welcome answers a real handle, built on the key this device DOES
	// hold. The signature key is read through the joined group's own ratchet tree, because that
	// is the only route from this package to a leaf's signature_key -- MemberAt drops it.
	founder := newTestEngine(t)
	keyPackage, err := fixture.engine.NewKeyPackage()
	if err != nil {
		t.Fatalf("NewKeyPackage: %v", err)
	}
	group := founder.createGroup(t, "the-inversion")
	defer group.Close()
	if _, err := group.ProposeAdd(keyPackage); err != nil {
		t.Fatalf("ProposeAdd: %v", err)
	}
	_, welcome, ratchetTree, err := group.Commit(nil)
	if err != nil {
		t.Fatalf("Commit(nil): %v", err)
	}
	if err := group.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	joined, err := fixture.engine.JoinFromWelcome(welcome, ratchetTree)
	if err != nil {
		t.Fatalf("JoinFromWelcome over a real welcome: %v", err)
	}
	defer joined.Close()
	if joined == nil {
		t.Fatal("JoinFromWelcome answered no handle and no error")
	}
	if named, _ := engineLeafKeyOf(t, joined, joined.OwnLeafIndex()); !bytes.Equal(named, fixture.signerPub) {
		t.Errorf("the joined handle sits at a leaf naming %x and this device signs with %x; a member built on a key it does not hold is one every peer refuses at its first commit",
			named, fixture.signerPub)
	}
}

// ApplyCommit refuses anything this handle did not stage, in both of the two ways it can be
// handed one.
//
// The first shape is the one section 6's own note says is legal go and which this package
// deliberately does not try to prevent: a keyed composite literal naming only the exported fields.
// The second is the one that would otherwise be caught a step later, at mls's transcript, naming
// a group nobody tampered with.
func TestApplyCommitRefusesAnyProcessedMessageThisHandleDidNotStage(t *testing.T) {
	fixture := newTestEngine(t)
	first := fixture.createGroup(t, "apply-first")
	defer first.Close()
	second := fixture.createGroup(t, "apply-second")
	defer second.Close()

	if err := first.ApplyCommit(nil); !errorIs(err, ErrEngineProcessedForeign) {
		t.Errorf("ApplyCommit(nil) answered %v, want ErrEngineProcessedForeign", err)
	}
	// the shape section 6 says is legal, written here to prove it is: exported fields only.
	foreign := &EngineProcessed{Kind: EngineProcessedCommit, SenderLeaf: 0, Raw: []byte("staged elsewhere")}
	if err := first.ApplyCommit(foreign); !errorIs(err, ErrEngineProcessedForeign) {
		t.Errorf("ApplyCommit over a keyed literal built outside this engine answered %v, want ErrEngineProcessedForeign", err)
	}
	// and one staged by ANOTHER handle of this package, which is the half a nil stagedRef check
	// alone would let through.
	handedOver := &EngineProcessed{Kind: EngineProcessedCommit}
	handedOver.stagedRef = &stagedProcessed{handle: second.(*connectMlsHandle), processed: nil}
	if err := first.ApplyCommit(handedOver); !errorIs(err, ErrEngineProcessedForeign) {
		t.Errorf("ApplyCommit over a value staged by another handle answered %v, want ErrEngineProcessedForeign", err)
	}
}

// The engine's own lifecycle, as far as one member can drive it: found, propose, commit, merge.
//
// It is here because every property above is about shape, and a shape that does not run is a
// shape nobody has to keep. The epoch moving is what makes the session's own epoch case real.
func TestTheAdapterDrivesAGroupThroughAnEpoch(t *testing.T) {
	fixture := newTestEngine(t)
	handle := fixture.createGroup(t, "one-epoch")
	defer handle.Close()

	if suite := fixture.engine.Suite(); suite != uint16(mls.CipherSuiteX25519ChaCha20Sha256Ed25519) {
		t.Errorf("Suite() = %#04x, want %#04x", suite, uint16(mls.CipherSuiteX25519ChaCha20Sha256Ed25519))
	}
	if epoch := handle.Epoch(); epoch != 0 {
		t.Errorf("a freshly founded group is at epoch %d, want 0", epoch)
	}
	if !bytes.Equal(handle.GroupId(), testGroupId("one-epoch")) {
		t.Errorf("GroupId() = %x, want %x", handle.GroupId(), testGroupId("one-epoch"))
	}
	for _, opaque := range []struct {
		name string
		call func() ([]byte, error)
	}{
		{name: "RatchetTreeSnapshot", call: handle.RatchetTreeSnapshot},
		{name: "GroupContextBytes", call: handle.GroupContextBytes},
		{name: "SenderDataSecret", call: handle.SenderDataSecret},
		{name: "EncryptionSecret", call: handle.EncryptionSecret},
	} {
		answer, err := opaque.call()
		if err != nil || len(answer) == 0 {
			t.Errorf("%s answered %d octets and %v", opaque.name, len(answer), err)
		}
	}
	// the two that answer opaque octets and are easy to swap: they must not agree.
	snapshot, _ := handle.RatchetTreeSnapshot()
	context, _ := handle.GroupContextBytes()
	if bytes.Equal(snapshot, context) {
		t.Error("RatchetTreeSnapshot and GroupContextBytes answered the same octets; nothing downstream in wave 1 parses either, so a swapped pair of bodies is invisible everywhere else")
	}
	sender, _ := handle.SenderDataSecret()
	encryption, _ := handle.EncryptionSecret()
	if bytes.Equal(sender, encryption) {
		t.Error("SenderDataSecret and EncryptionSecret answered the same octets, so the closed enum's two names reach one secret")
	}

	// A COMMIT WITH NO PROPOSALS, which is the only kind a one member group can make: RFC 9420
	// refuses a committer that covers its own Update, and there is no second member to propose
	// anything else. ProposeUpdate and ProposeAdd are exercised elsewhere in this file; what this
	// case is for is the epoch moving.
	commit, _, _, err := handle.Commit(nil)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if len(commit) == 0 {
		t.Fatal("Commit answered no commit message")
	}
	if epoch := handle.Epoch(); epoch != 0 {
		t.Errorf("the group is at epoch %d after a commit was STAGED; a committer that merged optimistically would fork itself off the group", epoch)
	}
	if err := handle.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	if epoch := handle.Epoch(); epoch != 1 {
		t.Errorf("the group is at epoch %d after a merge, want 1", epoch)
	}
	// and the exporter moves with the epoch, which is what the session's storage root rests on.
	beforeSecret, err := handle.Export("URmessage/v1/storage", nil, 32)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if _, _, _, err := handle.Commit(nil); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := handle.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	afterSecret, err := handle.Export("URmessage/v1/storage", nil, 32)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if bytes.Equal(beforeSecret, afterSecret) {
		t.Error("mls_secret is the same at two epochs, so every key of the record layer would be too")
	}
	// ClearPendingCommit drops a staged commit rather than merging it.
	if _, _, _, err := handle.Commit(nil); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	handle.ClearPendingCommit()
	if err := handle.MergePendingCommit(); err == nil {
		t.Error("MergePendingCommit succeeded after ClearPendingCommit, so the staged commit was not dropped")
	}
}

// NewKeyPackage answers a key package connect/mls itself will parse and admit, and persists the
// two private halves the store carries.
//
// THE ADMIT IS INTO ANOTHER DEVICE'S GROUP AFTER j1 TASK 4, and the move is the finding rather
// than a fixture convenience. This case used to add the engine's own key package to a group the
// SAME engine founded, and that assertion goes RED on the commit that points the mint at
// self.signer -- it was the only one of this package's 186 cases that did. It is re-pointed here,
// where it keeps meaning exactly what it was written to mean: the encoding is one ProposeAdd
// takes rather than merely some octets. The self-add it used to perform is now its own case below,
// with the refusal asserted by name.
func TestNewKeyPackageAnswersOneConnectMlsWillAdmit(t *testing.T) {
	fixture := newTestEngine(t)
	encoded, err := fixture.engine.NewKeyPackage()
	if err != nil {
		t.Fatalf("NewKeyPackage: %v", err)
	}
	if len(encoded) == 0 {
		t.Fatal("NewKeyPackage answered no octets")
	}
	if len(fixture.store.keyPackages) != 1 {
		t.Errorf("the store holds %d key packages after one was minted, want 1: a key package published without its private halves stored is a Welcome nobody can open",
			len(fixture.store.keyPackages))
	}
	// and ANOTHER device's group admits it, which is the reading that says the encoding is the one
	// ProposeAdd takes rather than merely some octets.
	admitting := newTestEngine(t)
	handle := admitting.createGroup(t, "admits")
	defer handle.Close()
	if _, err := handle.ProposeAdd(encoded); err != nil {
		t.Errorf("ProposeAdd over another device's key package: %v", err)
	}
}

// TestAnEngineAddingItsOwnKeyPackageToItsOwnGroupIsRefusedAtProposeAdd is the other half of
// j1 task 4's fourth property, and the two are not one assertion in two moods: they are two
// assertions over two different groups, and an earlier reading that stated them as one required
// a self-add to succeed and to be refused two sentences apart.
//
// THE REFUSAL EXISTS ONLY UNDER THIS TASK. Before the mint was pointed at self.signer the engine
// published leaves under a key it did not hold, so a self-add published a signature key the
// group's own leaf did not carry and RFC 9420's ValSem101 saw no duplicate. It fires at
// ProposeAdd and not at Commit, because mls validates the proposal list against its own
// pre-commit tree.
//
// THE REFUSAL IS ASSERTED BY NAME. A case that accepted "some error" here would also pass over a
// device with no leaf keys, which is a different defect with a different repair.
func TestAnEngineAddingItsOwnKeyPackageToItsOwnGroupIsRefusedAtProposeAdd(t *testing.T) {
	fixture := newTestEngine(t)
	encoded, err := fixture.engine.NewKeyPackage()
	if err != nil {
		t.Fatalf("NewKeyPackage: %v", err)
	}
	handle := fixture.createGroup(t, "the-self-add")
	defer handle.Close()

	proposal, err := handle.ProposeAdd(encoded)
	if !errorIs(err, mls.ErrAddDuplicateSignatureKey) {
		t.Errorf("ProposeAdd over this engine's OWN key package answered %v, want mls.ErrAddDuplicateSignatureKey: after task 4 the leaf this device publishes and the leaf it founded under name one key, which is the whole point",
			err)
	}
	if errorIs(err, ErrEngineLeafKeys) {
		t.Errorf("the refusal reads as a leaf keys failure (%v); a gate that accepted any error here could not tell the duplicate signature key from a device with no leaf keys at all", err)
	}
	if proposal != nil {
		t.Error("ProposeAdd answered a proposal beside its refusal")
	}
}

// errorIs is errors.Is spelled once, so a case reads as the property it is asserting rather than
// as a chain of unwraps.
func errorIs(err error, want error) bool {
	return errors.Is(err, want)
}

// ---------------------------------------------------------------------------
// j1 task 4: the engine mints under its own signer
// ---------------------------------------------------------------------------

// engineLeafKeyOf decodes one leaf out of a handle's published ratchet tree and answers the
// signature key it names and the signature over it.
//
// THIS IS THE ONLY ROUTE from package messagegroup to a leaf's signature_key, and naming the
// wrong one is what the control below exists for. MemberAt answers
// (leafIndex, identityPub, leafKeys, err) and DROPS mls.Member.SignatureKey at the seam, so a
// gate that compared MemberAt's identityPub against this device's signer is comparing the
// CREDENTIAL against the SIGNER -- two independent draws after this package's fixture repair --
// and reports a key mismatch where there is none.
func engineLeafKeyOf(t *testing.T, handle GroupHandle, at uint32) (signatureKey []byte, signature []byte) {
	t.Helper()
	snapshot, err := handle.RatchetTreeSnapshot()
	if err != nil {
		t.Fatalf("RatchetTreeSnapshot: %v", err)
	}
	tree, err := mls.UnmarshalRatchetTree(snapshot)
	if err != nil {
		t.Fatalf("UnmarshalRatchetTree: %v", err)
	}
	leaf := tree.Leaf(mls.LeafIndex(at))
	if leaf == nil {
		t.Fatalf("the published tree carries no leaf at %d", at)
	}
	return bytes.Clone(leaf.SignatureKey), bytes.Clone(leaf.Signature)
}

// engineKeyPackageLeafKeyOf decodes a published key package encoding and answers the signature key
// its leaf names. It is door 1's route, and every name on it is exported.
func engineKeyPackageLeafKeyOf(t *testing.T, encoded []byte) []byte {
	t.Helper()
	var kp mls.KeyPackage
	if err := syntax.Unmarshal(encoded, &kp); err != nil {
		t.Fatalf("decode the key package this engine published: %v", err)
	}
	return bytes.Clone(kp.LeafNode.SignatureKey)
}

// TestEveryLeafThisEngineMintsNamesTheDeviceSigner is j1 task 4's first property, over the doors
// this seam has to an MLS leaf rather than over "the key package".
//
// CLAUSE A rests on nothing anybody has to rule: it is mls.JoinFromWelcome's own caller-material
// gate read backwards. That gate compares the public half of a joiner's signing key against the
// signature_key its published leaf names, BEFORE one octet of the Welcome is judged -- so a key
// package whose leaf names a key this device cannot sign with is a key package no Welcome
// addressed to it can ever be opened with. Clause A is the whole of what tasks 5 and 6 need.
//
// CLAUSE B rests on J1-1, which this plan files as UNRULED: MASTER section 5.2 calls device_sig
// "the MLS leaf signature key" and does not say in as many words that EVERY leaf this device
// publishes -- including a KeyPackage's, which RFC 9420 permits to carry a fresh key per
// advertisement -- names it. The wide reading is taken here, and what falls under the narrow one
// is clause B itself, this package's fixture repair, the duplicate-signature-key case below, and
// task 5's assembly.
//
// THE CLASS IS THE DOORS AND IT IS FOUR, derived and printed rather than asserted at a number:
// GroupEngine.NewKeyPackage, GroupEngine.CreateGroup, GroupHandle.ProposeUpdate and
// GroupHandle.Commit are the four sites at which this device's code mints or re-signs an MLS
// leaf. Three of the four are OBSERVABLE from this package and all three are driven; the fourth,
// ProposeUpdate, is printed as UNOBSERVED with its reason. The fifth door of section 6's own
// method set -- JoinFromWelcome -- is the COMPLEMENT: its leaf comes off a peer's ratchet tree
// and it mints none, which is why it is named and excluded here and covered by the two-engine
// join instead.
func TestEveryLeafThisEngineMintsNamesTheDeviceSigner(t *testing.T) {
	fixture := newTestEngine(t)

	// THE CONTROL ON THE FIXTURE, not on the engine. If the credential identity and the signer's
	// public half are one draw, then "the leaf names the device signer" and "the leaf names the
	// credential" are the same program and this gate cannot fail for the reason it exists.
	if bytes.Equal(fixture.signerPub, fixture.identityPub) {
		t.Fatalf("this device's credential identity and its signer's public half are the same %d octets, so the comparison below is equal by construction and observes nothing",
			len(fixture.signerPub))
	}

	// DOOR 1 -- GroupEngine.NewKeyPackage, through mls.NewKeyPackageWithSigner -> NewLeafNode.
	// The method answers the encoding; the route is syntax.Unmarshal and kp.LeafNode.SignatureKey.
	encoded, err := fixture.engine.NewKeyPackage()
	if err != nil {
		t.Fatalf("NewKeyPackage: %v", err)
	}
	if published := engineKeyPackageLeafKeyOf(t, encoded); !bytes.Equal(published, fixture.signerPub) {
		t.Errorf("door 1, GroupEngine.NewKeyPackage: the published leaf names %x as its signature_key and this device signs with %x",
			published, fixture.signerPub)
	}

	// DOOR 2 -- GroupEngine.CreateGroup, through mls.NewGroup -> NewLeafNode.
	handle := fixture.createGroup(t, "the-doors-of-this-engine")
	defer handle.Close()
	founding, foundingSignature := engineLeafKeyOf(t, handle, handle.OwnLeafIndex())
	if !bytes.Equal(founding, fixture.signerPub) {
		t.Errorf("door 2, GroupEngine.CreateGroup: the founding leaf names %x as its signature_key and this device signs with %x",
			founding, fixture.signerPub)
	}

	// DOOR 4 -- GroupHandle.Commit, through mls's own CreateUpdatePathSecrets -> leaf.Sign, which
	// is a SECOND, INDEPENDENT mls site. An empty proposal list is PathRequired, so Commit(nil) on
	// this one member group populates the update path and re-signs leaf 0.
	if _, _, _, err := handle.Commit(nil); err != nil {
		t.Fatalf("Commit(nil), which is door 4's own driver: %v", err)
	}
	if err := handle.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	committed, committedSignature := engineLeafKeyOf(t, handle, handle.OwnLeafIndex())
	// the control on door 4, and without it a gate that read the tree BEFORE the merge passes a
	// committer that re-signed under a key of its own: the leaf it read was never re-signed.
	if bytes.Equal(committedSignature, foundingSignature) {
		t.Errorf("door 4, GroupHandle.Commit: leaf %d carries the FOUNDING signature %x after a commit that merged, so this door was never driven and the assertion below reads door 2's leaf a second time",
			handle.OwnLeafIndex(), foundingSignature)
	}
	if !bytes.Equal(committed, fixture.signerPub) {
		t.Errorf("door 4, GroupHandle.Commit: the re-signed leaf names %x as its signature_key and this device signs with %x",
			committed, fixture.signerPub)
	}

	// DOOR 3 -- GroupHandle.ProposeUpdate -- PRINTED AS UNOBSERVED rather than asserted.
	t.Logf("door 3, GroupHandle.ProposeUpdate: UNOBSERVED. The method answers a serialized MLSMessage carrying an RFC 9420 section 6.3 PrivateMessage, so the leaf it re-signs is inside the AEAD, and the proposer cannot commit its own update to get that leaf into a tree -- mls refuses a committer that covers its own update. What is KNOWN without observing it is an argument and not an observation: mls clones the caller's signer into every group, so this door keeps working after the engine's own key is destroyed, which is exactly why task 5's erase is invisible from a handle. Owed: an UNOBSERVED entry, or a section 6 amendment that lets a caller read its own leaf")
	t.Logf("the complement, printed: GroupEngine.JoinFromWelcome is the one door of the five whose leaf does not come out of this device -- it comes off a peer's ratchet tree and this device mints none -- and it is covered by the two-engine join rather than here")
}

// TestNewKeyPackageErasesEveryPrivateHalfItMintedBeforeItReturns holds j1 task 4's second and
// fifth properties, which are the MINT half of one aliasing rule: a method that erases a value
// aliasing self.signer destroys the device, and a method that drops the values un-erased leaves
// three private keys in the heap for the collector to move around. Either alone is a defect and
// the pair is the only safe state.
//
// THE TWO ROUTES ARE DIFFERENT AND NEITHER ALONE IS THE PROPERTY.
//
//   - THE ERASE clause -- the two HPKE halves are zero when the method returns -- is observed
//     through the ALIAS, recordingAliasStore, which retains the caller's slice headers. It cannot
//     be observed through memoryStateStore: that store COPIES at call time, so its entry is
//     byte-identical under a correct body, under a body that erases neither half and under one
//     that erases only the init half. Measured, and it is the reason the instrument exists.
//   - THE ORDERING clause -- the erase happens AFTER PutKeyPackage returns -- is observed through
//     the COPY, memoryStateStore's own map, which holds the real octets under a correct body and
//     32 zeros under a body that erased first. The alias cannot see that one.
//
// The third secret, the key package's own clone of device_sig, has NO RUNTIME ROUTE from this
// package: the value is a local of the method and never leaves it. That clause is held over the
// method's own source, and the route is named rather than left implied.
func TestNewKeyPackageErasesEveryPrivateHalfItMintedBeforeItReturns(t *testing.T) {
	// (1) THE ERASE CLAUSE, through the alias.
	fixture, aliasing := newRecordingEngine(t)
	if _, err := fixture.engine.NewKeyPackage(); err != nil {
		t.Fatalf("NewKeyPackage over the aliasing instrument: %v", err)
	}
	if len(aliasing.initAlias) == 0 || len(aliasing.encAlias) == 0 {
		t.Fatalf("the instrument retained %d init octets and %d encryption octets, so this clause observes nothing",
			len(aliasing.initAlias), len(aliasing.encAlias))
	}
	for _, held := range []struct {
		what  string
		array []byte
	}{
		{what: "the init private half", array: aliasing.initAlias},
		{what: "the encryption private half", array: aliasing.encAlias},
	} {
		for _, octet := range held.array {
			if octet != 0 {
				t.Errorf("%s is still in the heap after NewKeyPackage returned: %x", held.what, held.array)
				break
			}
		}
	}

	// (2) THE ORDERING CLAUSE, through the copy. The store's own entry must hold the REAL octets:
	// a body that erased before the put leaves the store holding 32 zeros where the init private
	// belongs, and every Welcome addressed to this device is then unopenable -- with the whole
	// suite otherwise green, because nothing joins in this task.
	copied := newTestEngine(t)
	if _, err := copied.engine.NewKeyPackage(); err != nil {
		t.Fatalf("NewKeyPackage over the copying store: %v", err)
	}
	if len(copied.store.keyPackages) != 1 {
		t.Fatalf("the store holds %d key packages, want 1", len(copied.store.keyPackages))
	}
	for _, entry := range copied.store.keyPackages {
		for at, what := range []string{"the encoding", "the init private half", "the encryption private half"} {
			zero := true
			for _, octet := range entry[at] {
				if octet != 0 {
					zero = false
					break
				}
			}
			if len(entry[at]) == 0 || zero {
				t.Errorf("the store holds %d octets of %s and every one of them is zero; the erase ran BEFORE the put and this device published a key package nobody can address",
					len(entry[at]), what)
			}
		}
	}

	// (3) ERASING DOES NOT DISTURB self.signer. A second mint on the same engine must still name
	// the same key, which is false the moment the engine's own signer has been zeroed -- and
	// nothing refuses an all-zero seed: it derives a perfectly valid public key.
	second, err := copied.engine.NewKeyPackage()
	if err != nil {
		t.Fatalf("a second NewKeyPackage on the same engine: %v", err)
	}
	if published := engineKeyPackageLeafKeyOf(t, second); !bytes.Equal(published, copied.signerPub) {
		t.Errorf("the engine's SECOND key package names %x and this device signs with %x; the first mint's erase reached self.signer",
			published, copied.signerPub)
	}

	// (4) THE THIRD SECRET, over the method's own source, because no runtime route from this
	// package reaches it. The minted key package holds a COPY of device_sig on an unexported
	// field, the value is a local, and a method that returned the encoding and dropped it leaves
	// the device's long term signing key in the heap.
	erases := engineNewKeyPackageErasesInSource(t)
	t.Logf("(*connectMlsEngine).NewKeyPackage reaches these erases in its own source: %v", erases)
	for _, owed := range []string{"keyPackage.Zeroize", "zeroize(initPrivate)", "zeroize(encryptPrivate)"} {
		if !slices.Contains(erases, owed) {
			t.Errorf("(*connectMlsEngine).NewKeyPackage does not reach %s; it holds three private halves and drops every one this method does not erase",
				owed)
		}
	}
}

// engineNewKeyPackageErasesInSource reads this package's own production source and answers, for
// (*connectMlsEngine).NewKeyPackage, the erase calls its body makes -- rendered as callee plus
// argument so the gate can name WHICH array is missing rather than reporting "an erase is
// missing".
func engineNewKeyPackageErasesInSource(t *testing.T) []string {
	t.Helper()
	fileSet, sources := messagegroupProductionSources(t)
	found := []string{}
	seen := false
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Name.Name != "NewKeyPackage" || function.Recv == nil {
				continue
			}
			seen = true
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, isCall := node.(*ast.CallExpr)
				if !isCall {
					return true
				}
				rendered := &strings.Builder{}
				if err := printer.Fprint(rendered, fileSet, call); err != nil {
					t.Fatalf("render a call of %s: %v", function.Name.Name, err)
				}
				text := rendered.String()
				if strings.HasPrefix(text, "zeroize(") || strings.HasSuffix(text, ".Zeroize()") {
					found = append(found, strings.TrimSuffix(text, "()"))
				}
				return true
			})
		}
	}
	if !seen {
		t.Fatal("no production method of this package is named NewKeyPackage, so this clause read nothing")
	}
	slices.Sort(found)
	return found
}

// TestWhatReachesTheStoreWhenAKeyPackageIsPublishedIsFourValuesAndNothingElse is j1 task 4's
// third property, and it has two halves that do not share one route.
//
// THE CONTENT half -- the ref is KeyPackage.Ref over the encoding, and the encoding is the octets
// the method returned -- is readable off the landed fixture's own map. THE ARITY half -- "and
// nothing else" -- is not: a map records no call, so a body that persisted a fifth value through
// a SECOND store method leaves a keyPackages entry that still looks exactly right. That half
// reads the call record.
func TestWhatReachesTheStoreWhenAKeyPackageIsPublishedIsFourValuesAndNothingElse(t *testing.T) {
	fixture, recording := newRecordingEngine(t)
	encoded, err := fixture.engine.NewKeyPackage()
	if err != nil {
		t.Fatalf("NewKeyPackage: %v", err)
	}

	puts := recording.callsTo("PutKeyPackage")
	if len(puts) != 1 {
		t.Fatalf("publishing one key package drove PutKeyPackage %d times", len(puts))
	}
	if got := len(puts[0].args); got != 4 {
		t.Fatalf("PutKeyPackage was handed %d byte arguments, want 4", got)
	}
	// the CONTENT: the encoding the store took is the encoding the caller got, and the ref is
	// KeyPackage.Ref over it rather than over anything smaller.
	if !bytes.Equal(puts[0].args[1], encoded) {
		t.Errorf("the store took %d octets of encoding and the method answered %d",
			len(puts[0].args[1]), len(encoded))
	}
	var kp mls.KeyPackage
	if err := syntax.Unmarshal(encoded, &kp); err != nil {
		t.Fatalf("decode the published key package: %v", err)
	}
	ref, err := kp.Ref(fixture.crypto)
	if err != nil {
		t.Fatalf("KeyPackage.Ref: %v", err)
	}
	if !bytes.Equal(puts[0].args[0], ref) {
		t.Errorf("the store keyed this key package under %x and KeyPackage.Ref over the WHOLE encoding is %x; a ref taken over the leaf alone is not the value a Welcome names",
			puts[0].args[0], ref)
	}
	if len(puts[0].args[2]) == 0 || len(puts[0].args[3]) == 0 {
		t.Errorf("the store took %d init octets and %d encryption octets",
			len(puts[0].args[2]), len(puts[0].args[3]))
	}

	// THE ARITY HALF. Publishing a key package drives PutKeyPackage and nothing else: the set of
	// things this method persists that it did not persist before task 4 is EMPTY, deliberately,
	// and device_sig is in the keyfile rather than in the store.
	called := recording.methodsCalled()
	if !slices.Equal(called, []string{"PutKeyPackage"}) {
		t.Errorf("publishing one key package drove %v; a fifth value persisted through a second store method is Option A arriving by the back door, and the map this package's other store holds would still look right",
			called)
	}
}

// TestTheEnginesOwnDocumentationNoLongerNamesTheJoinBlocker is a documentation assertion made
// over the package's own source, in the shape this package's existing AST gates use.
//
// engine.go's NewKeyPackage paragraph used to state the CAUSE of the join refusal -- that
// mls.NewKeyPackage keeps the signature private half on an unexported field and
// StateStore.TakeKeyPackage does not carry it, so nothing outside package mls can assemble the
// join material. After task 4 that sentence is false in the file that publishes it, and a
// sentence a later reader would trust and re-derive the wrong fix from is the same defect class
// as a sentinel naming an impossibility that is no longer impossible.
func TestTheEnginesOwnDocumentationNoLongerNamesTheJoinBlocker(t *testing.T) {
	_, sources := messagegroupProductionSources(t)
	found := []string{}
	seen := false
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Name.Name != "NewKeyPackage" || function.Recv == nil {
				continue
			}
			seen = true
			if function.Doc == nil {
				t.Errorf("%s documents its NewKeyPackage with nothing", source.path)
				continue
			}
			for _, line := range function.Doc.List {
				if strings.Contains(line.Text, "TakeKeyPackage") {
					found = append(found, strings.TrimSpace(line.Text))
				}
			}
		}
	}
	if !seen {
		t.Fatal("no production method of this package is named NewKeyPackage, so this gate read nothing")
	}
	if len(found) != 0 {
		t.Errorf("this engine's NewKeyPackage still documents StateStore.TakeKeyPackage as the reason a join is impossible: %v. After task 4 the leaf names device_sig and the material a join needs is assemblable",
			found)
	}
}

// ---------------------------------------------------------------------------
// j1 task 5: the welcome's refs, the take, and the put-back
// ---------------------------------------------------------------------------

// engineJoinFixture is one founder and TWO joiners, so that the Welcome the founder answers names
// TWO refs addressed to two different devices.
//
// The class this task's first property derives is EVERY ENTRY of Welcome.Secrets, read off the
// parsed message rather than assumed to be one, and it is two members here on purpose: a one-entry
// Welcome cannot distinguish "found mine" from "took the first", which is the whole of what that
// property says.
type engineJoinFixture struct {
	founder *testEngine
	joiner  *testEngine
	other   *testEngine
	// the joiner's engine writes into this, so the refs it took are readable in order
	joinerStore *recordingAliasStore
	handle      GroupHandle
	welcome     []byte
	ratchetTree []byte
	joinerRef   []byte
	welcomeRefs [][]byte
}

// newEngineJoinFixture founds a group and commits an Add for each joiner, in the order given.
//
// joinerFirst chooses whether the joiner under test is the FIRST entry of the Welcome or the
// second, and both orderings are driven: with the joiner second, a body that took on the first ref
// never reaches its own; with the joiner first, a body that took on every ref keeps taking after it
// has what it needs.
func newEngineJoinFixture(t *testing.T, name string, joinerFirst bool) *engineJoinFixture {
	t.Helper()
	joiner, joinerStore := newRecordingEngine(t)
	return buildEngineJoinFixture(t, name, joinerFirst, joiner, joinerStore)
}

// newEraseObservingJoinFixture is newEngineJoinFixture whose JOINER also carries the provider
// instrument, so the array the join assembles and is obliged to erase is aliased.
func newEraseObservingJoinFixture(t *testing.T, name string, joinerFirst bool) (*engineJoinFixture,
	*aliasingCryptoProvider) {

	t.Helper()
	joiner, joinerStore, observed := newEraseObservingEngine(t)
	return buildEngineJoinFixture(t, name, joinerFirst, joiner, joinerStore), observed
}

// buildEngineJoinFixture is the body both constructors share, with the joiner handed in.
//
// The joiner is a PARAMETER rather than built here because the two instruments a gate may need on
// that device -- the recording store and the aliasing provider -- are chosen by the property being
// observed, and a fixture that built one of them would make the other a second fixture with a
// second copy of this commit chain.
func buildEngineJoinFixture(t *testing.T, name string, joinerFirst bool, joiner *testEngine,
	joinerStore *recordingAliasStore) *engineJoinFixture {

	t.Helper()
	founder := newTestEngine(t)
	other := newTestEngine(t)

	joinerKeyPackage, err := joiner.engine.NewKeyPackage()
	if err != nil {
		t.Fatalf("the joiner's NewKeyPackage: %v", err)
	}
	otherKeyPackage, err := other.engine.NewKeyPackage()
	if err != nil {
		t.Fatalf("the other joiner's NewKeyPackage: %v", err)
	}
	handle := founder.createGroup(t, name)
	t.Cleanup(func() { handle.Close() })
	added := [][]byte{otherKeyPackage, joinerKeyPackage}
	if joinerFirst {
		added = [][]byte{joinerKeyPackage, otherKeyPackage}
	}
	for at, keyPackage := range added {
		if _, err := handle.ProposeAdd(keyPackage); err != nil {
			t.Fatalf("ProposeAdd %d: %v", at, err)
		}
	}
	commit, welcome, ratchetTree, err := handle.Commit(nil)
	if err != nil {
		t.Fatalf("Commit(nil) over two adds: %v", err)
	}
	if len(commit) == 0 || len(welcome) == 0 || len(ratchetTree) == 0 {
		t.Fatalf("the commit answered commit=%d welcome=%d ratchetTree=%d",
			len(commit), len(welcome), len(ratchetTree))
	}
	if err := handle.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}

	// the refs the Welcome names, read off the parsed message rather than assumed
	parsed, err := mls.ParseMLSMessage(welcome)
	if err != nil {
		t.Fatalf("ParseMLSMessage over the welcome this fixture built: %v", err)
	}
	if parsed.Welcome == nil {
		t.Fatal("the message this fixture built carries no welcome arm")
	}
	refs := [][]byte{}
	for _, addressed := range parsed.Welcome.Secrets {
		refs = append(refs, bytes.Clone(addressed.NewMember))
	}
	if len(refs) != 2 {
		t.Fatalf("this fixture built a welcome naming %d refs, want 2: a one entry welcome cannot distinguish found-mine from took-the-first",
			len(refs))
	}
	var decoded mls.KeyPackage
	if err := syntax.Unmarshal(joinerKeyPackage, &decoded); err != nil {
		t.Fatalf("decode the joiner's key package: %v", err)
	}
	joinerRef, err := decoded.Ref(joiner.crypto)
	if err != nil {
		t.Fatalf("KeyPackage.Ref for the joiner: %v", err)
	}
	at := slices.IndexFunc(refs, func(ref []byte) bool { return bytes.Equal(ref, joinerRef) })
	if at < 0 {
		t.Fatalf("the welcome names %d refs and none of them is the joiner's %x", len(refs), joinerRef)
	}
	if joinerFirst != (at == 0) {
		t.Fatalf("this fixture asked for joinerFirst=%v and the joiner's entry is at index %d; the ordering is the whole of what the two mutants below are separated by",
			joinerFirst, at)
	}
	return &engineJoinFixture{
		founder: founder, joiner: joiner, other: other, joinerStore: joinerStore,
		handle: handle, welcome: welcome, ratchetTree: ratchetTree,
		joinerRef: joinerRef, welcomeRefs: refs,
	}
}

// TestTheJoinTakesExactlyTheOneRefTheWelcomeNamesThatThisStoreHolds is j1 task 5's first property.
//
// Not the first ref and not every ref. The refs a Welcome names are addressed to DIFFERENT
// joiners: a device that took on the first is asking a question of somebody else's entry, and a
// device that took on every one is destroying other entries' addressing for no reason.
//
// THE OBSERVATION IS THE CALL RECORD and not the map. A map records no call, so "which refs were
// taken, in which order, and where the body stopped" is a question memoryStateStore cannot answer
// at all -- and it is the only question that separates the two mutants this property exists for.
func TestTheJoinTakesExactlyTheOneRefTheWelcomeNamesThatThisStoreHolds(t *testing.T) {
	for _, ordering := range []struct {
		what        string
		joinerFirst bool
	}{
		{what: "the joiner's entry FIRST", joinerFirst: true},
		{what: "the joiner's entry SECOND", joinerFirst: false},
	} {
		fixture := newEngineJoinFixture(t, "take-exactly-one-"+ordering.what, ordering.joinerFirst)
		joined, err := fixture.joiner.engine.JoinFromWelcome(fixture.welcome, fixture.ratchetTree)
		if err != nil {
			t.Errorf("%s: JoinFromWelcome: %v", ordering.what, err)
			continue
		}
		defer joined.Close()

		// the refs this store was asked for, in order. A correct body walks the Welcome's own
		// order and STOPS at the one it holds.
		taken := [][]byte{}
		for _, call := range fixture.joinerStore.callsTo("TakeKeyPackage") {
			taken = append(taken, call.args[0])
		}
		at := slices.IndexFunc(fixture.welcomeRefs, func(ref []byte) bool {
			return bytes.Equal(ref, fixture.joinerRef)
		})
		want := fixture.welcomeRefs[:at+1]
		if len(taken) != len(want) {
			t.Errorf("%s: the join took on %d refs and the welcome names this device's own at index %d, so a body that stops when it finds its own takes %d",
				ordering.what, len(taken), at, len(want))
		}
		for i := range want {
			if i < len(taken) && !bytes.Equal(taken[i], want[i]) {
				t.Errorf("%s: take %d was over %x and the welcome's entry %d is addressed to %x",
					ordering.what, i, taken[i], i, want[i])
			}
		}
		if len(taken) > 0 && !bytes.Equal(taken[len(taken)-1], fixture.joinerRef) {
			t.Errorf("%s: the last ref this join took was %x and this device's own is %x",
				ordering.what, taken[len(taken)-1], fixture.joinerRef)
		}
	}
}

// TestAJoinThatFailsPutsTheKeyPackageBackByteForByte is j1 task 5's second and third properties.
//
// SECOND: every failure path after the take restores the entry, and the store is byte-identical to
// what it was before the attempt -- observed by driving a SECOND attempt with the good Welcome
// over the same ref and requiring it to succeed.
//
// THIRD: the arrays the store handed back are NOT the arrays JoinKeyMaterial.Zeroize erases.
// memoryStateStore's TakeKeyPackage answers the store's OWN arrays, and
// (*JoinKeyMaterial).Zeroize erases InitPrivate, EncryptPrivate, SignPrivate and the key package's
// retained seed -- so a body that assembled the material directly over what the store handed back
// and then erased it puts ZEROED OCTETS back. The octets are compared and not merely the presence
// of an entry: a gate that compared presence alone passes that mutant, which is why the comparison
// below is byte for byte.
func TestAJoinThatFailsPutsTheKeyPackageBackByteForByte(t *testing.T) {
	fixture := newEngineJoinFixture(t, "the-put-back", true)
	before, held := fixture.joiner.store.keyPackages[fmt.Sprintf("%x", fixture.joinerRef)]
	if !held {
		t.Fatalf("the joiner's store holds no entry under its own ref %x before the attempt", fixture.joinerRef)
	}
	beforeCopy := [3][]byte{
		bytes.Clone(before[0]), bytes.Clone(before[1]), bytes.Clone(before[2]),
	}

	// a ratchet tree from a DIFFERENT commit, which fails after the take rather than before it
	second := newEngineJoinFixture(t, "the-put-back-other-commit", true)
	refused, err := fixture.joiner.engine.JoinFromWelcome(fixture.welcome, second.ratchetTree)
	if err == nil {
		t.Fatal("a join over a ratchet tree from another commit succeeded, so this case reaches no failure path")
	}
	if refused != nil {
		t.Error("the refused join answered a handle beside its error")
	}
	// the underlying refusal is WRAPPED and not swallowed: "this join failed" and "your keyring is
	// wrong" are different problems for whoever has to fix one of them
	if errorIs(err, ErrEngineNoKeyPackageForWelcome) {
		t.Errorf("a join that took and then failed answered %v, which reads as a welcome addressed to somebody else", err)
	}

	after, held := fixture.joiner.store.keyPackages[fmt.Sprintf("%x", fixture.joinerRef)]
	if !held {
		t.Fatalf("the joiner's store holds no entry under %x after a failed join; the device's only copy is gone and the legitimate welcome can never be opened",
			fixture.joinerRef)
	}
	for at, what := range []string{"the encoding", "the init private half", "the encryption private half"} {
		if !bytes.Equal(after[at], beforeCopy[at]) {
			t.Errorf("%s was put back as %x and was stored as %x; the material was assembled over the store's own arrays and the erase reached them",
				what, after[at], beforeCopy[at])
		}
	}

	// and the entry still WORKS, which is the half a byte comparison alone does not say
	joined, err := fixture.joiner.engine.JoinFromWelcome(fixture.welcome, fixture.ratchetTree)
	if err != nil {
		t.Fatalf("a second join with the good welcome over the put-back entry: %v", err)
	}
	defer joined.Close()
}

// TestTheJoinRefusalTellsAStoreFailureFromAWelcomeAddressedElsewhere is j1 task 5's fourth
// property, and it is DEFERRED rather than defended: the taxonomy that would let a caller matching
// on the TYPE tell the two apart is owed by whoever owns mls.StateStore, and until then the only
// place the difference can survive is the message.
//
// StateStore.TakeKeyPackage returns a bare error with no declared not-found value, so a loop that
// treated every error as "not mine" reports a broken disk as an unaddressed Welcome. The refusal
// therefore carries the ref count, the refusal count and the last store error VERBATIM.
func TestTheJoinRefusalTellsAStoreFailureFromAWelcomeAddressedElsewhere(t *testing.T) {
	// (a) a welcome addressed to nobody this store holds
	fixture := newEngineJoinFixture(t, "the-refusal-taxonomy", true)
	stranger, _ := newRecordingEngine(t)
	err := func() error {
		_, err := stranger.engine.JoinFromWelcome(fixture.welcome, fixture.ratchetTree)
		return err
	}()
	if !errorIs(err, ErrEngineNoKeyPackageForWelcome) {
		t.Fatalf("a welcome addressed to two other devices answered %v, want ErrEngineNoKeyPackageForWelcome", err)
	}
	unaddressed := err.Error()
	for _, owed := range []string{"2 key package refs", "refused 2"} {
		if !strings.Contains(unaddressed, owed) {
			t.Errorf("the refusal does not carry %q: %s", owed, unaddressed)
		}
	}

	// (b) the same shape, with the store answering an I/O failure for the ref this device DOES
	// hold. A caller matching on the type cannot tell these apart; an operator reading the
	// message must be able to.
	broken := newEngineJoinFixture(t, "the-broken-disk", true)
	broken.joinerStore.failTake = errors.New("the disk this store sits on answered EIO")
	_, ioErr := broken.joiner.engine.JoinFromWelcome(broken.welcome, broken.ratchetTree)
	if !errorIs(ioErr, ErrEngineNoKeyPackageForWelcome) {
		t.Fatalf("a store that cannot read answered %v, want ErrEngineNoKeyPackageForWelcome", ioErr)
	}
	if !strings.Contains(ioErr.Error(), "EIO") {
		t.Errorf("the refusal over a broken store does not carry the store's own error: %s", ioErr.Error())
	}
	if ioErr.Error() == unaddressed {
		t.Errorf("a broken disk and a welcome addressed elsewhere answer the same message, so nothing anywhere can tell them apart: %s", unaddressed)
	}
}

// TestTheJoinConfigCarriesExactlyTheFieldsTheJoinReads is j1 task 5's fifth property, held over
// the CONFIG this method builds rather than over mls.JoinFromWelcome's read-set.
//
// That is the choice the plan leaves the implementer, and it is taken because the alternative
// needs a scan root this package does not have: every AST gate here reads this package's own files
// and none reaches ../mls. The route is named rather than left implied.
//
// THE CLASS is the GroupConfig fields mls.JoinFromWelcome reads, and it is FOUR: Crypto, Store,
// Profile -- defaulted if nil -- and GroupId, which it reads ONLY as an intent match the caller
// opts into. GroupId is left unset because section 6's signature gives this engine no group id to
// intend. THE COMPLEMENT IS PRINTED and is the four fields CreateGroup sets and this must not:
// Suite, Extensions, RequiredCaps and LeafKeys, all unread on the join path because required
// capabilities come off the Welcome's own GroupInfo. A gate that did not print them would be one
// nobody can tell from a gate that checked nothing.
func TestTheJoinConfigCarriesExactlyTheFieldsTheJoinReads(t *testing.T) {
	read := []string{"Crypto", "Store", "Profile", "GroupId"}
	complement := []string{"Suite", "Extensions", "RequiredCaps", "LeafKeys"}
	t.Logf("mls.JoinFromWelcome reads %d GroupConfig fields (%v); the complement this method must not set is %d (%v)",
		len(read), read, len(complement), complement)

	set := engineGroupConfigFieldsSetOnTheJoinPath(t)
	t.Logf("the join path builds an mls.GroupConfig naming %v", set)
	if len(set) == 0 {
		t.Fatal("no mls.GroupConfig literal was found on this engine's join path, so this gate read nothing")
	}
	if slices.Contains(set, "GroupId") {
		t.Error("the join path sets GroupId. Section 6 gives this engine no group id to intend, so a value here is either a guess that refuses every legitimate welcome or a group id recovered from the message about to be judged -- which turns an intent match into a tautology, and reads like a check")
	}
	for _, unread := range complement {
		if slices.Contains(set, unread) {
			t.Errorf("the join path sets %s, which mls.JoinFromWelcome does not read: required capabilities come off the welcome's own GroupInfo, and a field nothing reads is a field a later reader will believe is load bearing",
				unread)
		}
	}
	for _, named := range set {
		if !slices.Contains(read, named) {
			t.Errorf("the join path sets %s, which is not one of the %v mls.JoinFromWelcome reads", named, read)
		}
	}
	for _, owed := range []string{"Crypto", "Store"} {
		if !slices.Contains(set, owed) {
			t.Errorf("the join path does not set %s, which mls.JoinFromWelcome refuses a nil of", owed)
		}
	}
}

// engineGroupConfigFieldsSetOnTheJoinPath reads this package's own production source and answers
// the field names of every mls.GroupConfig composite literal built inside a method whose body
// calls mls.JoinFromWelcome.
func engineGroupConfigFieldsSetOnTheJoinPath(t *testing.T) []string {
	t.Helper()
	_, sources := messagegroupProductionSources(t)
	named := []string{}
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil {
				continue
			}
			joins := false
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, isCall := node.(*ast.CallExpr)
				if !isCall {
					return true
				}
				selector, isSelector := call.Fun.(*ast.SelectorExpr)
				if !isSelector || selector.Sel.Name != "JoinFromWelcome" {
					return true
				}
				if qualifier, isIdentifier := selector.X.(*ast.Ident); isIdentifier && qualifier.Name == "mls" {
					joins = true
				}
				return true
			})
			if !joins {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				literal, isLiteral := node.(*ast.CompositeLit)
				if !isLiteral {
					return true
				}
				selector, isSelector := literal.Type.(*ast.SelectorExpr)
				if !isSelector || selector.Sel.Name != "GroupConfig" {
					return true
				}
				for _, element := range literal.Elts {
					pair, isPair := element.(*ast.KeyValueExpr)
					if !isPair {
						continue
					}
					if key, isKey := pair.Key.(*ast.Ident); isKey {
						named = append(named, key.Name)
					}
				}
				return true
			})
		}
	}
	slices.Sort(named)
	return slices.Compact(named)
}

// TestTheDeviceSurvivesItsOwnJoin is j1 task 5's sixth property, and it is the one whose absence
// destroys a key.
//
// mls.JoinKeyMaterial OWNS every array it carries and its header prescribes the erase. If this
// method assembled SignPrivate directly over self.signer, (*JoinKeyMaterial).Zeroize would destroy
// the device's long term signing key on the first successful join -- and NOTHING ANYWHERE REFUSES
// AFTERWARDS. zeroizeSecret writes zeros through the slice, an all-zero seed derives a perfectly
// valid ed25519 public key, and every leaf this device publishes afterwards names
// 3b6a27bcceb6a42d62a3a8d02a6f0d73653215771de243a63ac048a18b59da29 while its credential still
// names the real device.
//
// THE OBSERVATION IS ON THE ENGINE AND NOT ON THE HANDLE, and three readings are INADMISSIBLE --
// each is green over a destroyed device and each is the obvious one:
//
//   - MemberAt(0)'s identityPub, which reads Credential.Identity and the erase does not touch;
//   - anything read off the handle this call answered, because connect/mls CLONES SignPrivate
//     into the group before the erase, so Commit, ProposeUpdate and Protect are all green on it;
//   - the two-engine exporter equality itself, which never drives the joiner's engine again.
//
// The admissible observation is a DOOR OF THIS ENGINE driven a SECOND time: a second
// NewKeyPackage whose leaf names the pre-join signature key, and a CreateGroup whose leaf 0 names
// it -- both read through RatchetTreeSnapshot rather than through MemberAt.
//
// IT IS TAKEN ON BOTH EXITS. An implementation that erases on exactly one of them is caught by
// neither half alone, and erasing on the success path only is what a defer refactor produces by
// accident.
func TestTheDeviceSurvivesItsOwnJoin(t *testing.T) {
	for _, exit := range []struct {
		what string
		join func(fixture *engineJoinFixture) (GroupHandle, error)
	}{
		{what: "a join that succeeded", join: func(fixture *engineJoinFixture) (GroupHandle, error) {
			return fixture.joiner.engine.JoinFromWelcome(fixture.welcome, fixture.ratchetTree)
		}},
		// a failure AFTER the take, which is the exit a body erasing only on the success return
		// walks out of with the device intact and a body erasing only on the put-back path
		// destroys it on
		{what: "a join that failed after the take", join: func(fixture *engineJoinFixture) (GroupHandle, error) {
			other := newEngineJoinFixture(t, "the-survival-other-commit", true)
			return fixture.joiner.engine.JoinFromWelcome(fixture.welcome, other.ratchetTree)
		}},
	} {
		fixture := newEngineJoinFixture(t, "the-device-survives-"+exit.what, true)
		before := bytes.Clone(fixture.joiner.signerPub)

		handle, err := exit.join(fixture)
		if handle != nil {
			defer handle.Close()
		}
		_ = err

		// ADMISSIBLE OBSERVATION 1: door 1, driven a second time.
		published, mintErr := fixture.joiner.engine.NewKeyPackage()
		if mintErr != nil {
			t.Errorf("%s: the engine's next NewKeyPackage: %v", exit.what, mintErr)
			continue
		}
		if named := engineKeyPackageLeafKeyOf(t, published); !bytes.Equal(named, before) {
			t.Errorf("%s: the engine's NEXT key package names %x as its leaf signature_key and this device signed with %x before the join; its long term signing key was destroyed by a material assembled over it",
				exit.what, named, before)
		}

		// ADMISSIBLE OBSERVATION 2: door 2, driven a second time, read through the ratchet tree.
		founded := fixture.joiner.createGroup(t, "after-the-join-"+exit.what)
		defer founded.Close()
		if named, _ := engineLeafKeyOf(t, founded, founded.OwnLeafIndex()); !bytes.Equal(named, before) {
			t.Errorf("%s: a group this engine founds AFTER the join names %x at leaf 0 and this device signed with %x before it",
				exit.what, named, before)
		}
	}

	// AND THE OTHER HALF OF THE OWNERSHIP RULE, over the method's own source, because the
	// material is a LOCAL and no runtime route from this package reaches it. The copies exist so
	// that the erase is safe; the erase has to actually happen, on EVERY exit. An implementation
	// that erases on exactly one of them leaves a copy of device_sig and both HPKE halves in the
	// heap on the other, and that is what a defer refactor produces by accident -- so the
	// assertion is that the call is a DEFER and not a statement on one path.
	deferred, plain := engineJoinMaterialEraseSites(t)
	t.Logf("the join path erases its material at %d deferred site(s) %v and %d plain one(s) %v",
		len(deferred), deferred, len(plain), plain)
	if len(deferred) != 1 {
		t.Errorf("the join path defers %d erases of the material it assembled, want exactly 1: mls.JoinFromWelcome refuses at some fifteen places and every one of them is an exit this method returns through",
			len(deferred))
	}
	if len(plain) != 0 {
		t.Errorf("the join path erases its material with %d plain statement(s) %v; a statement erases the exit it stands on and no other",
			len(plain), plain)
	}
}

// engineJoinMaterialEraseSites reads this package's own production source and answers where the
// method that calls mls.JoinFromWelcome erases the material it assembled: the deferred calls and
// the plain ones, separately.
func engineJoinMaterialEraseSites(t *testing.T) (deferred []string, plain []string) {
	t.Helper()
	fileSet, sources := messagegroupProductionSources(t)
	seen := false
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil {
				continue
			}
			joins := false
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, isCall := node.(*ast.CallExpr)
				if !isCall {
					return true
				}
				selector, isSelector := call.Fun.(*ast.SelectorExpr)
				if !isSelector || selector.Sel.Name != "JoinFromWelcome" {
					return true
				}
				if qualifier, isIdentifier := selector.X.(*ast.Ident); isIdentifier && qualifier.Name == "mls" {
					joins = true
				}
				return true
			})
			if !joins {
				continue
			}
			seen = true
			deferredAt := map[token.Pos]bool{}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				if statement, isDefer := node.(*ast.DeferStmt); isDefer {
					deferredAt[statement.Call.Pos()] = true
				}
				return true
			})
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, isCall := node.(*ast.CallExpr)
				if !isCall {
					return true
				}
				selector, isSelector := call.Fun.(*ast.SelectorExpr)
				if !isSelector || selector.Sel.Name != "Zeroize" {
					return true
				}
				at := fmt.Sprintf("%s:%d", source.path, fileSet.Position(call.Pos()).Line)
				if deferredAt[call.Pos()] {
					deferred = append(deferred, at)
				} else {
					plain = append(plain, at)
				}
				return true
			})
		}
	}
	if !seen {
		t.Fatal("no production method of this package calls mls.JoinFromWelcome, so this clause read nothing")
	}
	slices.Sort(deferred)
	slices.Sort(plain)
	return deferred, plain
}

// TestTheJoinErasesTheArrayAndNotOnlyAMethodNamed Zeroize is the R7 half of task 5's erase
// obligation, and it is here because the source read at the bottom of TestTheDeviceSurvivesItsOwnJoin
// was the whole of that obligation and BINDS ONLY A METHOD NAME.
//
// MEASURED, and this is the reason the file gained an instrument rather than a sentence:
// `defer keys.Zeroize()` rewritten as `defer mutantEraser{}.Zeroize()`, with
// `type mutantEraser struct{}` and a no-op `Zeroize()` beside it in engine.go, leaves the source
// read seeing exactly one deferred erase and no plain one -- and left the whole of ./mls/...
// ./message/... ./messagegroup/... GREEN at 7,692 passing, 0 failing, 0 skipped. A join that
// erased nothing passed the entire shipped suite. Every private half the material was assembled
// over -- both HPKE halves and the copy of device_sig -- stayed in the heap, and nothing anywhere
// said so.
//
// R7, quoted from recordingAliasStore because this gate is that rule one seam over: "an erase is
// observable only through an ALIAS of the array erased, and wherever the far side copies, the
// property must build the alias or it is measuring a photograph." The source read is the
// photograph. aliasingCryptoProvider's header carries the one seam through which the material's
// own array crosses back into a type this package controls.
//
// THE CONTROL IS THE AT-CALL COPY AND IT IS NOT OPTIONAL. "It reads all zero afterwards" is
// satisfied by an array that was all zero when it was handed over and by an empty one, so the
// alias is admitted only when the copy taken at call time is non-zero AND is the octets the store
// held under this device's own ref. That last clause is what says the aliased array is the
// material's InitPrivate rather than some other private half the join happened to open with.
//
// IT IS TAKEN ON BOTH EXITS, for TestTheDeviceSurvivesItsOwnJoin's reason: an implementation that
// erases on the success return only is what a defer refactor produces by accident, and the
// failure-after-take exit is the one it walks out of holding the copies.
func TestTheJoinErasesTheArrayAndNotOnlyAMethodNamedZeroize(t *testing.T) {
	for _, exit := range []struct {
		what string
		join func(fixture *engineJoinFixture) (GroupHandle, error)
	}{
		{what: "a join that succeeded", join: func(fixture *engineJoinFixture) (GroupHandle, error) {
			return fixture.joiner.engine.JoinFromWelcome(fixture.welcome, fixture.ratchetTree)
		}},
		{what: "a join that failed after the take", join: func(fixture *engineJoinFixture) (GroupHandle, error) {
			other := newEngineJoinFixture(t, "the-erase-other-commit", true)
			return fixture.joiner.engine.JoinFromWelcome(fixture.welcome, other.ratchetTree)
		}},
	} {
		fixture, observed := newEraseObservingJoinFixture(t, "the-erase-"+exit.what, true)
		stored, held := fixture.joiner.store.keyPackages[fmt.Sprintf("%x", fixture.joinerRef)]
		if !held {
			t.Fatalf("%s: the joiner's store holds no entry under its own ref %x before the attempt",
				exit.what, fixture.joinerRef)
		}
		initPrivate := bytes.Clone(stored[1])
		if isAllZero(initPrivate) {
			t.Fatalf("%s: the init private half this device published is all zero before the join, so every reading below would pass against an erase that did nothing",
				exit.what)
		}
		before := len(observed.hpkeOpenPriv)

		handle, err := exit.join(fixture)
		if handle != nil {
			defer handle.Close()
		}
		_ = err

		// the alias, found by WHAT IT HELD and not by its position: mls.JoinFromWelcome opens more
		// than one ciphertext on this path and which of them is the welcome secret is that
		// function's business rather than this gate's.
		found := -1
		for at := before; at < len(observed.hpkeOpenPriv); at += 1 {
			if bytes.Equal(observed.hpkeOpenPrivAtCall[at], initPrivate) {
				found = at
				break
			}
		}
		if found < 0 {
			t.Fatalf("%s: the join made %d HpkeOpen call(s) and none of them was handed the init private half the store held under this device's ref; this gate is aliasing nothing and would report clean having read nothing",
				exit.what, len(observed.hpkeOpenPriv)-before)
		}
		alias := observed.hpkeOpenPriv[found]
		t.Logf("%s: the join handed HPKE an init private half of %d octets and the array is %d octets after it returned",
			exit.what, len(observed.hpkeOpenPrivAtCall[found]), len(alias))
		if len(alias) == 0 {
			t.Errorf("%s: the aliased array is empty, so every octet of it reads zero vacuously", exit.what)
		}
		if !isAllZero(alias) {
			t.Errorf("%s: the array the join assembled its material over reads %x after JoinFromWelcome returned and it was %x when the join handed it to HPKE; the material was NOT erased and a method named Zeroize having been deferred over it says nothing",
				exit.what, alias, initPrivate)
		}

		// AND THE ERASE REACHED A COPY. The store's own array is the one the put-back writes, so
		// an erase that reached it would put zeroed octets back -- which is the property
		// TestAJoinThatFailsPutsTheKeyPackageBackByteForByte holds from the other end, and it is
		// asserted here too because this gate is the one holding an alias at all.
		if bytes.Equal(alias, stored[1]) && isAllZero(stored[1]) {
			t.Errorf("%s: the store's own init private half is all zero after the join; the material was assembled over the store's arrays rather than over copies",
				exit.what)
		}
	}
}

// isAllZero answers whether every octet of a slice is zero. An EMPTY slice answers true and every
// caller above refuses that case separately, because "all of no octets are zero" is the vacuous
// reading an erase gate must never accept as its observation.
func isAllZero(value []byte) bool {
	for _, octet := range value {
		if octet != 0 {
			return false
		}
	}
	return true
}
