// The import-direction gate for this module.
//
// Two rules, both from spec A decision A2 and both stated in every slice-1 plan's
// global constraints, neither of which the compiler enforces:
//
//   - connect must never import connect/mls or connect/message. Go permits a parent
//     to import its own subpackages, so this is a design rule the toolchain will not
//     catch. Violating it makes the data path depend on the messenger, which is the
//     opposite of the intended direction.
//   - connect/mls must not import connect, connect/message or connect/messagegroup, and
//     connect/message must not import connect. mls is the protocol core and has to stay
//     linkable without the data path.
//   - connect/message must not import connect/mls, and must not import
//     connect/messagegroup. This is the 2026-09-06 split stated as a rule rather than as a
//     habit: spec B section 2.2 forbids the message server from linking an MLS parser at
//     all, and connect/message is the half that server links. The edge is ALLOWED to run
//     the other way -- connect/messagegroup may import connect/message and connect/mls,
//     because it is the client half and the client holds the group -- and today only the
//     second of those is an import it actually has.
//
// The last of those is the one the compiler cannot hold yet, and that is why it is here
// rather than left to a build failure. At the commit that created connect/messagegroup it
// imports connect/mls and does NOT import connect/message, so there is no cycle for the
// compiler to refuse; it starts holding the direction at the first file over there that
// calls into connect/message, and an assertion written then would be one nobody could
// watch fail. connect/message importing connect/mls compiles cleanly forever and is held
// by nothing else in this tree at all -- msgrepo's dependency gate sees it, in another
// repository, on a run nobody makes before pushing.
//
// The rules were satisfied when each was written and none was checked. That
// is the state a rule is in just before it stops being true, so this is the check.
//
// Imports are read with go/parser rather than matched as text: a parser reports the
// import graph the compiler will see, where a text search would be fooled by a path
// in a comment or a string, and would miss an aliased or dot import entirely. Build
// tags are deliberately not applied — a forbidden import inside a _windows.go file is
// still a forbidden import.
//
// One thing this file does NOT measure, written here because this is where a reader comes
// to find out which way these packages depend on each other. The import graph is one way
// and stays that way; the TEST BINARIES are not one way. connect/message's suite reaches
// connect/messagegroup by FILESYSTEM PATH -- writeauth_test.go's authScanRoots for the
// constant time rules, record_test.go's messagegroupRoot for the join rule -- and mls's
// suite reaches both by path in forbiddenScanRoots. Those are os.ReadDir and go/parser
// calls over a sibling directory rather than imports, so they add no edge to the graph
// this file guards and cannot create a cycle for the compiler to refuse. What they do
// create is a coupling to the sibling DIRECTORY existing on disk: `go test ./message/` in
// a tree where connect/messagegroup has been deleted or renamed fails outright rather than
// passing over a quietly smaller scope, which is what those gates are written for. It is a
// real property of the design and not a defect, and it is the reason one of these packages
// can be moved only by moving the roots that name it in the same commit.
package connect

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const (
	modulePath       = "github.com/urnetwork/connect"
	mlsPath          = modulePath + "/mls"
	messagePath      = modulePath + "/message"
	messagegroupPath = modulePath + "/messagegroup"
)

// importsInDir returns every import path appearing in the go files directly inside
// dir, test files included, with the surrounding quotes removed. It does not recurse:
// each package is judged on its own files, so a violation is reported against the
// package that actually contains it rather than against its parent. A directory that
// cannot be read is an error rather than an empty result, because a gate that silently
// scans nothing passes forever.
func importsInDir(t *testing.T, dir string) map[string][]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	found := map[string][]string{}
	files := 0
	fset := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		files += 1
		for _, spec := range file.Imports {
			unquoted, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("%s: import path %s is not a quoted string: %v", path, spec.Path.Value, err)
			}
			found[unquoted] = append(found[unquoted], entry.Name())
		}
	}
	if files == 0 {
		t.Fatalf("scanned %s and found no go files, so this gate proved nothing", dir)
	}
	return found
}

// TestConnectDoesNotImportItsOwnSubpackages is the rule the compiler cannot enforce.
// A parent importing its own subpackage compiles cleanly, so nothing but this test
// stands between the data path and a dependency on the messenger.
// knownSubpackageImports records the one place connect already imports a child of its
// own, so the general rule can still be enforced against everything else.
//
// connect/protocol is the generated protobuf package and is imported by 94 files in
// the root package. That predates this gate by a long way and is not something a test
// added today gets to break the build over. It is recorded rather than dropped: an
// allow-list of one keeps the rule live for every future subpackage, where deleting
// the check would quietly license the next one. CODESTYLE.md section "Package
// layering" states the rule with no exception, so the file and the code disagree —
// worth an owner ruling, and left as-is here rather than settled by a test.
var knownSubpackageImports = map[string]string{
	modulePath + "/protocol": "generated protobuf, imported by 94 root files, predates this gate",
}

func TestConnectDoesNotImportItsOwnSubpackages(t *testing.T) {
	imports := importsInDir(t, ".")
	for _, forbidden := range []string{mlsPath, messagePath, messagegroupPath} {
		if files, ok := imports[forbidden]; ok {
			t.Errorf("connect imports %s from %v: the data path must not depend on the messenger", forbidden, files)
		}
	}
	for path, files := range imports {
		if !strings.HasPrefix(path, modulePath+"/") {
			continue
		}
		if _, known := knownSubpackageImports[path]; known {
			continue
		}
		t.Errorf("connect imports its own subpackage %s from %v, which CODESTYLE section Package layering forbids", path, files)
	}
	for path, reason := range knownSubpackageImports {
		if _, ok := imports[path]; !ok {
			t.Errorf("%s is allow-listed as %q but is no longer imported: drop it from the allow-list rather than leaving it to license a future import", path, reason)
		}
	}
}

// TestSubpackagesDoNotImportBack pins the other direction. mls is the protocol core
// and has to stay linkable on its own; a single import of connect would drag the whole
// data path in behind it.
func TestSubpackagesDoNotImportBack(t *testing.T) {
	cases := []struct {
		dir       string
		forbidden []string
	}{
		{"mls", []string{modulePath, messagePath, messagegroupPath}},
		{"mls/syntax", []string{modulePath, mlsPath, messagePath, messagegroupPath}},
		// the split: the server-safe half links no MLS parser and does not depend on the
		// client half either
		{"message", []string{modulePath, mlsPath, messagegroupPath}},
		{"messagegroup", []string{modulePath}},
	}
	for _, c := range cases {
		if _, err := os.Stat(c.dir); err != nil {
			t.Fatalf("%s is missing, so this gate would silently cover one package fewer: %v", c.dir, err)
		}
		imports := importsInDir(t, c.dir)
		for _, forbidden := range c.forbidden {
			if files, ok := imports[forbidden]; ok {
				t.Errorf("%s imports %s from %v", c.dir, forbidden, files)
			}
		}
	}
}

// TestImportScannerFindsAForbiddenImport is the positive control, and it is the only
// reason to believe the two gates above mean anything. Both of them pass by finding
// nothing, which is indistinguishable from a scanner that cannot find anything —
// exactly the failure this project has hit repeatedly. So the same function is pointed
// at a fixture that does contain a forbidden import, and must report it. The fixture
// covers a plain import, an aliased one, a blank one and a dot import, because a text search would
// catch the first and miss the other two.
func TestImportScannerFindsAForbiddenImport(t *testing.T) {
	dir := t.TempDir()
	source := "package fixture\n\n" +
		"import (\n" +
		"\t\"" + mlsPath + "\"\n" +
		"\talias \"" + messagePath + "\"\n" +
		"\t_ \"" + messagegroupPath + "\"\n" +
		"\t. \"" + mlsPath + "/syntax\"\n" +
		")\n"
	if err := os.WriteFile(filepath.Join(dir, "fixture.go"), []byte(source), 0o600); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}
	imports := importsInDir(t, dir)
	for _, want := range []string{mlsPath, messagePath, messagegroupPath, mlsPath + "/syntax"} {
		if _, ok := imports[want]; !ok {
			t.Errorf("the scanner missed %s, so the gates above prove nothing", want)
		}
	}
}
