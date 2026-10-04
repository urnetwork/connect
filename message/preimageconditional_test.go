// P3, the third property the eph_window ruling owes: NO PREIMAGE BUILDER HAS A CONDITIONAL
// FIELD.
//
// Master section 8's presence rule is the whole of why this is a gate and not a review
// habit. eph_window is "always present, zero on permanent, durable, media and eph bucket 0",
// and the ruling argues the point at length: LP(blob_id) is a ZERO LENGTH term on a record
// with no blob rather than an absent one, "so the preimage is defined for ordinary records
// without a special case", and the fixed width analogue of a zero length term is a ZERO
// VALUED one. Taking the surface reading instead -- present iff the class is eph -- would put
// a conditional into the one preimage builder this design has kept free of them, and it
// would do it in a way that round trips against itself perfectly: both sides of a single
// implementation would skip the same eight octets on the same records, and only a second
// implementation would ever see it.
//
// So the observation is over the SOURCE, because there is no record that exhibits it. A
// builder that writes a field only for some classes agrees with itself on every record.
//
// ---------------------------------------------------------------------------
// THE CLASS AND THE SCOPE, STATED SEPARATELY
// ---------------------------------------------------------------------------
//
// CLASS: every function in this package whose results are exactly ([]byte, error) and which
// calls at least one Write method on a writer. The result shape is shared with the input
// gate in aad_test.go -- preimageResultsAreBytesAndError is that gate's predicate and is
// called here rather than spelled a second time -- so the two gates cannot come to disagree
// about what a builder is. The second half is what narrows "hands back bytes" to "hands back
// bytes it WROTE", and it is what keeps a helper that merely returns a slice out of a rule
// about field order.
//
// SCOPE: every non test .go file in this package's own directory, enumerated at run time
// from the directory rather than named. It is written as its own paragraph because five
// times on this project a gate derived its class correctly and then wrote its scope beside
// it as a literal -- aad_test.go's own input gate is scoped to the string "aad.go", which is
// exactly right for what that gate is about and exactly the shape meant here. A conditional
// field is not aad.go's problem alone: it is equally a defect in codec.go, in writeauth.go
// and in attachment.go, and a gate that walked one file would report clean over the other
// three.
//
// PROPERTY: a Write call may not sit inside a branch or a loop. The branches a real builder
// has all REFUSE -- a nil header, an attachment that disagrees with its argument, a class
// and bucket pair the wire has no octet for -- and a branch that returns is not a branch that
// writes; the negative control is what holds this gate to that distinction.
//
// ---------------------------------------------------------------------------
// WHAT THIS GATE CANNOT SEE
// ---------------------------------------------------------------------------
//
// (1) A conditional moved one call deep. attachment.go's writeAttachmentBody takes a writer
// and switches on the attachment's kind, which is legitimate -- master section 8.3 says an
// attachment's body is the shape its kind names -- and it is OUT of the class because it
// hands back nothing. The complement below NAMES it, every run, which is the whole reason
// the complement is printed rather than counted: the day a second such function appears, it
// is in that list, and somebody reads why.
//
// (2) A field whose VALUE is computed conditionally. Writing w.WriteUint64(f(class)) where f
// answers zero off eph is the presence rule correctly implemented, and writing it where f
// answers a different window per class is a defect this gate cannot distinguish from it. The
// vectors and the round trip are what cover the values.
//
// (3) Any package but this one. connect/messagegroup builds no preimage -- it calls these --
// and connect/mls/syntax is the writer itself.
package message

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The control directory, relative to this package. It is the only path written down here,
// and it is a control rather than a subject.
const conditionalControlDir = "testdata/conditional"

// One write call and where it sits.
type conditionalWrite struct {
	function string
	file     string
	line     int
	call     string
	// the branching statement the call sits inside, or the empty string when it is written
	// straight in the function's own body
	insideOf string
}

// One judged function.
type conditionalBuilder struct {
	name   string
	file   string
	writes []conditionalWrite
}

// The verdict over one directory: the builders judged, and the functions the class removed.
type conditionalScan struct {
	builders []conditionalBuilder
	// every function that calls a writer and is NOT in the class, with the reason
	complement []string
	files      []string
}

// Scan one directory: every non test go file in it, every function in the class, and the
// complement the class removed.
func scanConditionalWrites(t testing.TB, dir string, only []string) conditionalScan {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("the conditional-write gate cannot read %s: %v", dir, err)
	}
	scan := conditionalScan{}
	names := []string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if only != nil && !slices.Contains(only, name) {
			continue
		}
		names = append(names, name)
	}
	slices.Sort(names)
	if len(names) == 0 {
		t.Fatalf("the conditional-write gate found no go source in %s, so it would report clean having read nothing", dir)
	}
	scan.files = names
	fset := token.NewFileSet()
	for _, name := range names {
		path := filepath.Join(dir, name)
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("the conditional-write gate cannot parse %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			funcDecl, isFunc := decl.(*ast.FuncDecl)
			if !isFunc || funcDecl.Body == nil {
				continue
			}
			writes := conditionalWritesOf(fset, funcDecl, name)
			if len(writes) == 0 {
				continue
			}
			if !preimageResultsAreBytesAndError(funcDecl) {
				scan.complement = append(scan.complement,
					fmt.Sprintf("%s (%s): writes octets and hands back no preimage", funcDecl.Name.Name, name))
				continue
			}
			scan.builders = append(scan.builders, conditionalBuilder{name: funcDecl.Name.Name, file: name, writes: writes})
		}
	}
	slices.SortFunc(scan.builders, func(a conditionalBuilder, b conditionalBuilder) int {
		return strings.Compare(a.name, b.name)
	})
	slices.Sort(scan.complement)
	return scan
}

// Every Write call in one function, each tagged with the branching statement it sits inside.
//
// Containment is decided by POSITION rather than by walking parents, which is what makes it
// exact for the one shape that would otherwise be misjudged: an `if err := w.WriteX(...);
// err != nil` runs the call in the if's INIT, before the branch begins, so the call is
// unconditional and its position says so. codec.go and attachment.go both use that form.
func conditionalWritesOf(fset *token.FileSet, decl *ast.FuncDecl, file string) []conditionalWrite {
	regions := conditionalRegionsOf(decl.Body)
	writes := []conditionalWrite{}
	ast.Inspect(decl.Body, func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if !isCall {
			return true
		}
		selector, isSelector := call.Fun.(*ast.SelectorExpr)
		if !isSelector || !strings.HasPrefix(selector.Sel.Name, "Write") {
			return true
		}
		if _, isIdent := selector.X.(*ast.Ident); !isIdent {
			return true
		}
		at := call.Pos()
		insideOf := ""
		for _, region := range regions {
			if region.from <= at && at < region.to {
				insideOf = region.kind
				break
			}
		}
		writes = append(writes, conditionalWrite{
			function: decl.Name.Name,
			file:     file,
			line:     fset.Position(at).Line,
			call:     ephWindowRender(selector.X) + "." + selector.Sel.Name,
			insideOf: insideOf,
		})
		return true
	})
	return writes
}

// A span of source a statement reaches only sometimes, and what kind of statement it is.
type conditionalRegion struct {
	kind string
	from token.Pos
	to   token.Pos
}

// Every such span inside one body: the two arms of an if, the arms of both switches and of a
// select, and the bodies of both loop forms. A function literal's body is deliberately NOT
// one -- syntax's own nesting form takes a closure and invokes it exactly once, so a write
// inside one runs whenever the call around it runs, and the call's own position is what says
// whether THAT is conditional.
func conditionalRegionsOf(body *ast.BlockStmt) []conditionalRegion {
	regions := []conditionalRegion{}
	add := func(kind string, node ast.Node) {
		if node == nil {
			return
		}
		regions = append(regions, conditionalRegion{kind: kind, from: node.Pos(), to: node.End()})
	}
	ast.Inspect(body, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.IfStmt:
			add("an if", typed.Body)
			add("an else", typed.Else)
		case *ast.CaseClause:
			for _, stmt := range typed.Body {
				add("a switch case", stmt)
			}
		case *ast.CommClause:
			for _, stmt := range typed.Body {
				add("a select case", stmt)
			}
		case *ast.ForStmt:
			add("a for loop", typed.Body)
		case *ast.RangeStmt:
			add("a range loop", typed.Body)
		}
		return true
	})
	return regions
}

// No builder in this package writes a field inside a branch or a loop.
//
// The header states the class, the scope and the property separately; this is where they are
// run. The complement is printed every time rather than only on a failure, because the
// complement is the part that goes wrong silently: a class predicate that quietly stopped
// matching would empty the judged set and this gate would pass, which is why the emptiness
// of the judged set is a Fatal and not a skip.
func TestNoPreimageBuilderWritesAFieldInsideABranch(t *testing.T) {
	scan := scanConditionalWrites(t, ".", nil)
	t.Logf("scope: %d non test go files in this package's directory, %v", len(scan.files), scan.files)
	if len(scan.builders) == 0 {
		t.Fatal("the class is EMPTY: no function in this package hands back a preimage it wrote, so this gate is reporting clean having judged nothing")
	}

	judged := []string{}
	for _, builder := range scan.builders {
		judged = append(judged, builder.name+" ("+builder.file+")")
	}
	t.Logf("class: %d builders judged, %v", len(scan.builders), judged)

	// THE COMPLEMENT, NAMED AND COUNTED, and the count is pinned rather than asserted merely
	// non empty -- twelve readings satisfy "non empty" when there are thirteen. It has two
	// members and the two are out of the class for different reasons, which is why they are
	// named individually rather than counted:
	//
	//	writeAttachmentBody  takes a writer and switches on the attachment's KIND, which is
	//	                     legitimate: master section 8.3 says an attachment's body is the
	//	                     shape its kind names, and none of those shapes is a record
	//	                     header field. It hands back nothing, so it is not a builder.
	//	authTag              calls Write on an hmac and not on a wire writer at all. It is
	//	                     matched by the write predicate -- Write is Write -- and removed
	//	                     by the result shape, and it is named here so that a reader is
	//	                     not left wondering which of the two the number counts.
	//
	// The number was wrong the first time this gate ran: it was written as 1, and the second
	// member is what the complement printed. That is the failure mode the printed complement
	// exists for and it is left recorded rather than tidied away.
	if len(scan.complement) == 0 {
		t.Fatal("the complement is EMPTY: every function that writes octets is in the class, which cannot be true while attachment.go declares writeAttachmentBody")
	}
	t.Logf("complement: %d functions write octets and are outside the class, %v", len(scan.complement), scan.complement)
	wantComplement := []string{"authTag ", "writeAttachmentBody "}
	if len(scan.complement) != len(wantComplement) {
		t.Errorf("the class removed %d writer-taking functions and %d is what this package declares: %v",
			len(scan.complement), len(wantComplement), scan.complement)
	}
	for i, prefix := range wantComplement {
		if len(scan.complement) <= i {
			t.Errorf("the complement has no member %d, and %s belongs there", i, strings.TrimSpace(prefix))
			continue
		}
		if !strings.HasPrefix(scan.complement[i], prefix) {
			t.Errorf("complement member %d is %q, want %s", i, scan.complement[i], strings.TrimSpace(prefix))
		}
	}

	// the coverage claim, checked rather than assumed: every file this package builds octets
	// in has to have contributed a builder
	files := map[string]bool{}
	for _, builder := range scan.builders {
		files[builder.file] = true
	}
	for _, want := range []string{"aad.go", "writeauth.go", "codec.go", "attachment.go"} {
		if !files[want] {
			t.Errorf("no builder was judged in %s, and that file writes record octets", want)
		}
	}

	total := 0
	for _, builder := range scan.builders {
		for _, write := range builder.writes {
			total++
			if write.insideOf == "" {
				continue
			}
			t.Errorf("%s (%s:%d) writes %s inside %s; master section 8's presence rule is that a field is always written and its VALUE carries the absence",
				write.function, write.file, write.line, write.call, write.insideOf)
		}
	}
	if total == 0 {
		t.Fatal("no write call was judged at all, so this gate asserted nothing")
	}
	t.Logf("%d write calls across %d builders, none inside a branch or a loop", total, len(scan.builders))
}

// The control, in all of its shapes. Without it the gate above proves nothing: it reports
// clean, and a gate that is broken reports clean too.
func TestTheConditionalWriteGateSeparatesTheControlShapes(t *testing.T) {
	scan := scanConditionalWrites(t, conditionalControlDir, []string{"control.go"})

	judged := []string{}
	flagged := map[string]string{}
	for _, builder := range scan.builders {
		judged = append(judged, builder.name)
		for _, write := range builder.writes {
			if write.insideOf != "" {
				flagged[builder.name] = write.insideOf
			}
		}
	}
	slices.Sort(judged)

	// the CLASS half: the control that writes conditionally and hands back nothing must not
	// be judged at all
	want := []string{
		"preimageWithAConditionalField",
		"preimageWithAConditionalFieldInASwitch",
		"preimageWithAWriteInALoop",
		"preimageWithRefusalsAndNoConditionalField",
	}
	if !slices.Equal(judged, want) {
		t.Fatalf("the gate judged %v in the control, want %v", judged, want)
	}
	if len(scan.complement) != 1 || !strings.HasPrefix(scan.complement[0], "notABuilderAtAll ") {
		t.Errorf("the control's complement is %v, want the one function that writes and hands back nothing", scan.complement)
	}

	// the POSITIVE half: each of the three branching shapes is named, and named with the
	// shape it is
	for _, positive := range []struct {
		name string
		kind string
	}{
		{name: "preimageWithAConditionalField", kind: "an if"},
		{name: "preimageWithAConditionalFieldInASwitch", kind: "a switch case"},
		{name: "preimageWithAWriteInALoop", kind: "a range loop"},
	} {
		kind, wasFlagged := flagged[positive.name]
		if !wasFlagged {
			t.Errorf("the gate did not flag %s, whose whole shape is a write inside %s", positive.name, positive.kind)
			continue
		}
		if kind != positive.kind {
			t.Errorf("the gate flagged %s as %q, want %q", positive.name, kind, positive.kind)
		}
	}

	// the NEGATIVE half: branches that refuse are not branches that write
	if kind, wasFlagged := flagged["preimageWithRefusalsAndNoConditionalField"]; wasFlagged {
		t.Errorf("the gate flagged the control whose branches only return, as %q; a gate that bans error handling is a gate that gets turned off", kind)
	}
}
