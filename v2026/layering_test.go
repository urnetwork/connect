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
// Package clauses are retained: connect's internal tests belong to connect,
// while connect_test files are a separate consumer package in the test binary.
// An external package clause in a non-test file is not that Go test boundary.
package connect

import (
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const (
	modulePath       = "github.com/urnetwork/connect/v2026"
	mlsPath          = modulePath + "/mls"
	messagePath      = modulePath + "/message"
	messagegroupPath = modulePath + "/messagegroup"
)

// Package clauses and source filenames retain the real internal/external test
// boundary. Import paths are still parsed, including aliases and build tags.
type sourcePackageImports struct {
	files   []string
	imports map[string][]string
}

// Read every direct Go source, without build-tag filtering. Missing, empty or
// malformed source cannot silently turn a dependency gate into an empty pass.
func importsByPackageInDir(t *testing.T, dir string) map[string]*sourcePackageImports {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	found := map[string]*sourcePackageImports{}
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
		packageName := file.Name.Name
		packageImports := found[packageName]
		if packageImports == nil {
			packageImports = &sourcePackageImports{imports: map[string][]string{}}
			found[packageName] = packageImports
		}
		packageImports.files = append(packageImports.files, entry.Name())
		for _, spec := range file.Imports {
			unquoted, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("%s: import path %s is not a quoted string: %v", path, spec.Path.Value, err)
			}
			packageImports.imports[unquoted] = append(packageImports.imports[unquoted], entry.Name())
		}
	}
	if files == 0 {
		t.Fatalf("scanned %s and found no go files, so this gate proved nothing", dir)
	}
	return found
}

// Keep the existing whole-directory import contract for the scanner positive
// control. No source or test file is excluded there.
func importsInDir(t *testing.T, dir string) map[string][]string {
	t.Helper()
	found := map[string][]string{}
	for _, packageImports := range importsByPackageInDir(t, dir) {
		for path, files := range packageImports.imports {
			found[path] = append(found[path], files...)
		}
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
	for _, violation := range connectImportViolations(importsByPackageInDir(t, ".")) {
		t.Error(violation)
	}
}

// Apply the same parent-to-child rule to connect's actual compiled package,
// internal tests included. Only Go's separate connect_test consumer is distinct;
// neither a changed namespace nor an external clause in ordinary source bypasses it.
func connectImportViolations(packages map[string]*sourcePackageImports) []string {
	var violations []string
	for name, packageImports := range packages {
		switch name {
		case "connect":
		case "connect_test":
			for _, file := range packageImports.files {
				if !strings.HasSuffix(file, "_test.go") {
					violations = append(violations, fmt.Sprintf("%s declares external test package connect_test outside a _test.go file", file))
				}
			}
		default:
			violations = append(violations, fmt.Sprintf("unexpected package %s in connect source files %v", name, packageImports.files))
		}
	}
	root := packages["connect"]
	if root == nil {
		violations = append(violations, "no connect package was scanned, so the parent import gate proved nothing")
		slices.Sort(violations)
		return violations
	}
	imports := root.imports
	for _, forbidden := range []string{mlsPath, messagePath, messagegroupPath} {
		if files, ok := imports[forbidden]; ok {
			violations = append(violations, fmt.Sprintf("connect imports %s from %v: the data path must not depend on the messenger", forbidden, files))
		}
	}
	for path, files := range imports {
		if !strings.HasPrefix(path, modulePath+"/") {
			continue
		}
		if _, known := knownSubpackageImports[path]; known {
			continue
		}
		violations = append(violations, fmt.Sprintf("connect imports its own subpackage %s from %v, which CODESTYLE section Package layering forbids", path, files))
	}
	for path, reason := range knownSubpackageImports {
		if _, ok := imports[path]; !ok {
			violations = append(violations, fmt.Sprintf("%s is allow-listed as %q but is no longer imported: drop it from the allow-list rather than leaving it to license a future import", path, reason))
		}
	}
	slices.Sort(violations)
	return violations
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

// Supply the pre-existing generated-protocol edge so each fixture isolates the
// package-identity rule instead of tripping the unchanged stale-allow-list check.
func connectImportFixture(t *testing.T, filename, source string) map[string]*sourcePackageImports {
	t.Helper()
	dir := t.TempDir()
	base := "package connect\nimport _ \"" + modulePath + "/protocol\"\n"
	if err := os.WriteFile(filepath.Join(dir, "base.go"), []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	return importsByPackageInDir(t, dir)
}

// The original production package cannot borrow an external consumer's import
// direction, including when the child is reached through an alias.
func TestConnectImportGateRejectsProductionChild(t *testing.T) {
	packages := connectImportFixture(t, "conformance.go", "package connect\nimport alias \""+modulePath+"/fingerprint\"\n")
	violations := connectImportViolations(packages)
	if len(violations) != 1 || !strings.Contains(violations[0], "conformance.go") || !strings.Contains(violations[0], modulePath+"/fingerprint") {
		t.Fatalf("production child import was not rejected exactly: %v", violations)
	}
}

// An internal test is compiled into connect's test variant, not a separate
// consumer. A _test.go suffix and a dot import cannot exempt its child edge.
func TestConnectImportGateRejectsInternalTestChild(t *testing.T) {
	packages := connectImportFixture(t, "conformance_test.go", "package connect\nimport . \""+modulePath+"/fingerprint\"\n")
	violations := connectImportViolations(packages)
	if len(violations) != 1 || !strings.Contains(violations[0], "conformance_test.go") || !strings.Contains(violations[0], modulePath+"/fingerprint") {
		t.Fatalf("internal-test child import was not rejected exactly: %v", violations)
	}
}

// The external test is a real consumer of both packages. Parse and retain its
// imports rather than hiding test files or allow-listing a conformance path.
func TestConnectImportGateAcceptsExternalTestConsumer(t *testing.T) {
	source := "package connect_test\nimport (\n parent \"" + modulePath + "\"\n child \"" + modulePath + "/fingerprint\"\n)\n"
	packages := connectImportFixture(t, "conformance_test.go", source)
	consumer := packages["connect_test"]
	if consumer == nil || len(consumer.imports[modulePath]) != 1 || len(consumer.imports[modulePath+"/fingerprint"]) != 1 {
		t.Fatal("external consumer imports disappeared from the parsed package graph")
	}
	if violations := connectImportViolations(packages); len(violations) != 0 {
		t.Fatalf("separate external consumer changed the parent dependency graph: %v", violations)
	}
}

// All platform/build-tagged files are scanned, regardless of the current host;
// both production and internal-test imports keep the strict parent rule.
func TestConnectImportGateRejectsTaggedChild(t *testing.T) {
	for _, filename := range []string{"conformance_windows.go", "conformance_windows_test.go"} {
		source := "//go:build windows\n\npackage connect\nimport _ \"" + modulePath + "/fingerprint\"\n"
		violations := connectImportViolations(connectImportFixture(t, filename, source))
		if len(violations) != 1 || !strings.Contains(violations[0], filename) || !strings.Contains(violations[0], modulePath+"/fingerprint") {
			t.Fatalf("%s escaped the import gate: %v", filename, violations)
		}
	}
}

// Only connect_test in a _test.go file is Go's separate root test package.
// A mismatched namespace or a package clause hidden behind a tag still fails.
func TestConnectImportGateRejectsPackageMismatch(t *testing.T) {
	cases := []struct {
		filename string
		name     string
	}{
		{filename: "consumer.go", name: "connect_test"},
		{filename: "consumer_windows.go", name: "connect_test"},
		{filename: "consumer_test.go", name: "other_test"},
		{filename: "consumer_test.go", name: "connect_test_test"},
		{filename: "consumer.go", name: "other"},
	}
	for _, c := range cases {
		source := "//go:build windows\n\npackage " + c.name + "\nimport _ \"" + modulePath + "/fingerprint\"\n"
		violations := connectImportViolations(connectImportFixture(t, c.filename, source))
		if len(violations) != 1 || !strings.Contains(violations[0], c.filename) || !strings.Contains(violations[0], c.name) {
			t.Fatalf("%s package %s bypassed package identity: %v", c.filename, c.name, violations)
		}
	}
}

// Comments and ordinary string literals describe imports without adding an
// edge. The parser, not a filename or text-pattern exception, decides this.
func TestConnectImportGateIgnoresNonImportText(t *testing.T) {
	source := "package connect\n// import \"" + modulePath + "/fingerprint\"\nvar example = \"" + modulePath + "/fingerprint\"\n"
	if violations := connectImportViolations(connectImportFixture(t, "comment_test.go", source)); len(violations) != 0 {
		t.Fatalf("non-import text created a dependency: %v", violations)
	}
}
