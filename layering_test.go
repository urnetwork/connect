// The import-direction gate for this module.
//
// connect must never import its own subpackages. Go permits a parent to import a child,
// so this is a design rule the toolchain will not catch (CODESTYLE.md, "Package
// layering").
//
// This file also held the layering of the messaging packages, connect/mls,
// connect/message and connect/messagegroup. They moved to github.com/urnetwork/message
// (MESSAGEREVIEW.md), and their rules moved with them. Their old paths stay forbidden
// here, so a branch from before the move cannot bring an import of them back unnoticed;
// message_module_boundary_test.go holds the rule that connect does not depend on the
// message module at all.
//
// The rule was satisfied when it was written and was not checked. That is the state a
// rule is in just before it stops being true, so this is the check.
//
// Imports are read with go/parser rather than matched as text: a parser reports the
// import graph the compiler will see, where a text search would be fooled by a path
// in a comment or a string, and would miss an aliased or dot import entirely. Build
// tags are deliberately not applied — a forbidden import inside a _windows.go file is
// still a forbidden import.
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

// TestImportScannerFindsAForbiddenImport is the positive control, and it is the only
// reason to believe the gate above means anything. It passes by finding
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
