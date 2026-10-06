// Connect has no dependency on github.com/urnetwork/message.
//
// Messaging lives in that module, which depends on connect. MESSAGEREVIEW.md: "Neither core SDK
// nor connect may depend on the message module, directly or transitively." Three readings hold
// the rule, because each sees what the others cannot:
//   - the module files of this repository (go.mod and go.sum, nested modules included);
//   - every go file of this repository, its imports parsed with no build constraint applied, so
//     test, platform and ignored files are read as well;
//   - the go command's answer, `go list -deps -test ./...`, under each GOOS/GOARCH of
//     platformBuildVariants, which also sees what is linked transitively. It reads this module
//     only, so the go source it never reports (nested modules, testdata) is printed by the rule
//     that leaves it out, and the first two readings hold it. It lists with cgo on and no build
//     tags, so a file that only a cgo-off build compiles (the js/wasm build ships without cgo) or
//     that only a tagged build compiles (flightgate_next, acklineagetrace, race) is not in its
//     answer. The import reading applies no build constraint, so it holds those files.
//
// The go list reading needs every variant's dependencies in the module cache, or the network to
// fetch them. Like TestPlatformBuildVariantsCompile, it is skipped under -short.
//
// A path is in the module when it is the module path or below it. A bare prefix would also match
// github.com/urnetwork/message-server, a separate module this rule does not cover, so the control
// holds that row as well.
package connect

import (
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const messageModulePath = "github.com/urnetwork/message"

// The module itself or a path below it; a longer sibling name is not in it.
func inMessageModule(path string) bool {
	return path == messageModulePath || strings.HasPrefix(path, messageModulePath+"/")
}

// Lines of the module files under root that name a path in the message module, and the module
// files read, relative to root. A module file is any *.mod or *.sum, which covers go.mod and
// go.sum and an alternate modfile beside them. Comments are not read.
func messageModuleRequirementsUnder(t *testing.T, root string) (found []string, read []string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if extension := filepath.Ext(path); extension != ".mod" && extension != ".sum" {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		read = append(read, relative)
		for i, line := range strings.Split(string(body), "\n") {
			if comment := strings.Index(line, "//"); 0 <= comment {
				line = line[:comment]
			}
			for _, field := range strings.Fields(line) {
				if unquoted, err := strconv.Unquote(field); err == nil {
					field = unquoted
				}
				if inMessageModule(field) {
					found = append(found, fmt.Sprintf("%s:%d: %s", relative, i+1, strings.TrimSpace(line)))
					break
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s for module files: %v", root, err)
	}
	slices.Sort(read)
	return found, read
}

// Imports of a path in the message module from every go file under root, and the go files read,
// relative to root. Build constraints are not applied: an import in a _windows.go file, a test
// or a file built only under a tag is still an import of this repository.
func messageModuleImportsUnder(t *testing.T, root string) (found []string, read []string) {
	t.Helper()
	fileSet := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".go" {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		file, err := parser.ParseFile(fileSet, path, nil, parser.ImportsOnly)
		if err != nil {
			// a file this cannot read is a file whose imports nothing has checked
			return fmt.Errorf("parse %s: %w", relative, err)
		}
		read = append(read, relative)
		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return fmt.Errorf("%s: import path %s is not a quoted string: %w", relative, spec.Path.Value, err)
			}
			if inMessageModule(importPath) {
				found = append(found, fmt.Sprintf("%s imports %s", relative, importPath))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s for go files: %v", root, err)
	}
	slices.Sort(read)
	return found, read
}

// Every package `go list -deps -test ./...` reports for the module in dir, one line each: the
// import path, then the path of the module providing it, if any. The go command is the one that
// built this test, or the one on the path.
func goListDepsTest(t *testing.T, dir string, env ...string) []string {
	t.Helper()
	goPath := filepath.Join(runtime.GOROOT(), "bin", "go")
	if _, err := os.Stat(goPath); err != nil {
		var lookErr error
		goPath, lookErr = exec.LookPath("go")
		if lookErr != nil {
			t.Skip("go toolchain not available")
		}
	}
	command := exec.CommandContext(
		t.Context(),
		goPath,
		"list", "-deps", "-test", "-f", "{{.ImportPath}}{{with .Module}} {{.Path}}{{end}}", "./...",
	)
	command.Dir = dir
	command.Env = append(os.Environ(), env...)
	output, err := command.Output()
	if err != nil {
		stderr := []byte{}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			stderr = exitErr.Stderr
		}
		t.Fatalf("%s: go list -deps -test ./... with %v: %v\n%s", dir, env, err, stderr)
	}
	return strings.Split(strings.TrimSpace(string(output)), "\n")
}

// The lines of a go list answer that name a package or module in the message module. A test
// variant is printed as "path [path.test]", so every field is read.
func messageModulePackagesIn(lines []string) []string {
	found := []string{}
	for _, line := range lines {
		for _, field := range strings.Fields(line) {
			if inMessageModule(strings.Trim(field, "[]")) {
				found = append(found, line)
				break
			}
		}
	}
	return found
}

func TestConnectHasNoMessageModuleDependency(t *testing.T) {
	root := moduleRootDir(t)

	requirements, moduleFiles := messageModuleRequirementsUnder(t, root)
	for _, requirement := range requirements {
		t.Errorf("%s names the message module; connect must not depend on it", requirement)
	}
	for _, want := range []string{"go.mod", "go.sum"} {
		if !slices.Contains(moduleFiles, want) {
			t.Fatalf("the module files read were %v, which does not hold this module's own %s", moduleFiles, want)
		}
	}

	imports, goFiles := messageModuleImportsUnder(t, root)
	for _, found := range imports {
		t.Errorf("%s; connect must not depend on the message module", found)
	}
	// the files read are held to the line-ending gate's enumeration, which shares no walk with this
	inModule := everyGoSourceFileUnder(t, root, root)
	if unread := missingFrom(inModule, goFiles); len(unread) > 0 {
		t.Errorf("the import scan read %d of the %d go files under %s, and %s went unread", len(goFiles), len(inModule), root, namesOf(unread, 12))
	}
	if outside := missingFrom(goFiles, inModule); len(outside) > 0 {
		t.Errorf("the import scan read %s, which the enumeration of %s does not hold", namesOf(outside, 12), root)
	}
	t.Logf("read %d module files %v and the imports of %d go files", len(moduleFiles), moduleFiles, len(goFiles))
}

func TestNoConnectBuildLinksAMessageModulePackage(t *testing.T) {
	if testing.Short() {
		t.Skip("lists every platform variant, which needs each variant's dependencies in the module cache or the network")
	}
	if runtime.GOOS == "js" || runtime.GOOS == "wasip1" {
		t.Skip("no toolchain subprocess under wasm")
	}
	root := moduleRootDir(t)
	// directories of this module's own packages, as go list reports them under any platform
	listedDirs := map[string]bool{}
	for _, variant := range platformBuildVariants {
		// cgo on, as in the native builds that link connect (gomobile, the c-shared sdk). go list
		// reads files without compiling them, so no C toolchain is needed; and ios/arm64 refuses
		// to list test binaries without cgo, which it links externally.
		lines := goListDepsTest(
			t,
			root,
			"GOOS="+variant.goos,
			"GOARCH="+variant.goarch,
			"CGO_ENABLED=1",
			"GOWORK=off",
		)
		for _, line := range messageModulePackagesIn(lines) {
			t.Errorf("GOOS=%s GOARCH=%s links %s; no connect build may link the message module", variant.goos, variant.goarch, line)
		}
		// the answer is this module's: its generated protocol package is in every build
		if !slices.Contains(lines, modulePath+"/protocol "+modulePath) {
			t.Errorf("GOOS=%s GOARCH=%s: go list answered %d lines without %s/protocol, so it did not list this module", variant.goos, variant.goarch, len(lines), modulePath)
		}
		for _, line := range lines {
			// a package of this module as itself; a test variant carries a third field
			fields := strings.Fields(line)
			if len(fields) != 2 || fields[1] != modulePath {
				continue
			}
			if fields[0] == modulePath {
				listedDirs["."] = true
			} else if dir, ok := strings.CutPrefix(fields[0], modulePath+"/"); ok {
				listedDirs[dir] = true
			}
		}
		t.Logf("GOOS=%s GOARCH=%s: %d packages, tests included", variant.goos, variant.goarch, len(lines))
	}

	// the complement: go source in this repository that go list never reported. Each directory
	// must be left out by a rule, and each rule must leave something out; the source readings in
	// TestConnectHasNoMessageModuleDependency hold all of it.
	nestedModule := func(dir string) bool {
		for ; dir != "."; dir = path.Dir(dir) {
			if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(dir), "go.mod")); err == nil {
				return true
			}
		}
		return false
	}
	leftOut := map[string][]string{"in a nested module": {}, "under testdata": {}}
	seen := map[string]bool{}
	for _, file := range everyGoSourceFileUnder(t, root, root) {
		dir := path.Dir(file)
		if listedDirs[dir] || seen[dir] {
			continue
		}
		seen[dir] = true
		switch {
		case nestedModule(dir):
			leftOut["in a nested module"] = append(leftOut["in a nested module"], dir)
		case slices.Contains(strings.Split(dir, "/"), "testdata"):
			leftOut["under testdata"] = append(leftOut["under testdata"], dir)
		default:
			t.Errorf("%s holds go source that go list reported under no platform, and no rule says why", dir)
		}
	}
	for _, rule := range slices.Sorted(maps.Keys(leftOut)) {
		dirs := leftOut[rule]
		if len(dirs) == 0 {
			t.Errorf("go list leaves out no directory %s any more; delete the rule", rule)
		}
		slices.Sort(dirs)
		t.Logf("go list leaves out %d directories %s, which the source readings hold: %v", len(dirs), rule, dirs)
	}
}

// Plants a dependency on the message module every way the readings above can see one, and
// requires each to report exactly the planted ones and none of the neighbours.
func TestTheMessageModuleBoundaryFindsAPlantedDependency(t *testing.T) {
	for _, c := range []struct {
		path string
		in   bool
	}{
		{path: "github.com/urnetwork/message", in: true},
		{path: "github.com/urnetwork/message/protocol", in: true},
		{path: "github.com/urnetwork/message/sdk/urmessage", in: true},
		{path: "github.com/urnetwork/message/v2", in: true},
		{path: "github.com/urnetwork/message-server", in: false},
		{path: "github.com/urnetwork/message-server/api", in: false},
		{path: "github.com/urnetwork/messages", in: false},
		{path: "github.com/urnetwork/connect/protocol", in: false},
		{path: "github.com/urnetwork", in: false},
	} {
		if got := inMessageModule(c.path); got != c.in {
			t.Errorf("%s in the message module: %v, want %v", c.path, got, c.in)
		}
	}

	write := func(root string, name string, body string) {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("build the fixture: %v", err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("build the fixture: %v", err)
		}
	}
	root := t.TempDir()
	write(root, "go.mod", "module example.com/fixture\n\ngo 1.21\n\n"+
		"require (\n\tgithub.com/urnetwork/message v0.0.0\n\tgithub.com/urnetwork/message-server v0.0.0 // not github.com/urnetwork/message\n)\n\n"+
		"replace github.com/urnetwork/message => ../message\n")
	write(root, "go.sum", "github.com/urnetwork/message v0.1.0 h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n"+
		"github.com/urnetwork/message-server v0.1.0 h1:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB=\n")
	write(root, "nested/alternate.mod", "module example.com/nested\n\nrequire \"github.com/urnetwork/message/sdk\" v0.0.0\n")
	write(root, "plain.go", "package fixture\n\nimport \"github.com/urnetwork/message/protocol\"\n")
	write(root, "aliased_windows.go", "package fixture\n\nimport messageprotocol \"github.com/urnetwork/message/protocol\"\n")
	write(root, "blank_test.go", "package fixture\n\nimport _ \"github.com/urnetwork/message/mls\"\n")
	write(root, "ignored.go", "//go:build ignore\n\npackage fixture\n\nimport . \"github.com/urnetwork/message\"\n")
	write(root, "testdata/deeper/still/fixture.go", "package fixture\n\nimport \"github.com/urnetwork/message/sdk/urmessage\"\n")
	write(root, "neighbours.go", "package fixture\n\n// github.com/urnetwork/message is named only in this comment\n\nimport (\n"+
		"\t\"github.com/urnetwork/connect/protocol\"\n\t\"github.com/urnetwork/message-server/api\"\n\t\"github.com/urnetwork/messages\"\n)\n")

	requirements, moduleFiles := messageModuleRequirementsUnder(t, root)
	wantRequirements := []string{
		"go.mod:6: github.com/urnetwork/message v0.0.0",
		"go.mod:10: replace github.com/urnetwork/message => ../message",
		"go.sum:1: github.com/urnetwork/message v0.1.0 h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		"nested/alternate.mod:3: require \"github.com/urnetwork/message/sdk\" v0.0.0",
	}
	if !slices.Equal(requirements, wantRequirements) {
		t.Errorf("the module file scan reported %q, want %q", requirements, wantRequirements)
	}
	if want := []string{"go.mod", "go.sum", "nested/alternate.mod"}; !slices.Equal(moduleFiles, want) {
		t.Errorf("the module file scan read %v, want %v", moduleFiles, want)
	}

	imports, goFiles := messageModuleImportsUnder(t, root)
	wantImports := []string{
		"aliased_windows.go imports github.com/urnetwork/message/protocol",
		"blank_test.go imports github.com/urnetwork/message/mls",
		"ignored.go imports github.com/urnetwork/message",
		"plain.go imports github.com/urnetwork/message/protocol",
		"testdata/deeper/still/fixture.go imports github.com/urnetwork/message/sdk/urmessage",
	}
	if !slices.Equal(imports, wantImports) {
		t.Errorf("the import scan reported %q, want %q", imports, wantImports)
	}
	if want := everyGoSourceFileUnder(t, root, root); !slices.Equal(goFiles, want) {
		t.Errorf("the import scan read %v, want every go file of the fixture %v", goFiles, want)
	}

	if runtime.GOOS == "js" || runtime.GOOS == "wasip1" {
		t.Skip("no toolchain subprocess under wasm")
	}
	// and the go command's answer, for an application module that links the message module
	// through a replace, the way a consumer composes the two
	modules := t.TempDir()
	write(modules, "application/go.mod", "module example.com/application\n\ngo 1.21\n\n"+
		"require github.com/urnetwork/message v0.0.0\n\nreplace github.com/urnetwork/message => ../message\n")
	write(modules, "application/application.go", "package application\n\nimport _ \"github.com/urnetwork/message/protocol\"\n")
	write(modules, "message/go.mod", "module github.com/urnetwork/message\n\ngo 1.21\n")
	write(modules, "message/protocol/protocol.go", "package protocol\n")
	lines := goListDepsTest(t, filepath.Join(modules, "application"), "GOWORK=off", "GOPROXY=off", "GOFLAGS=-mod=mod")
	if found := messageModulePackagesIn(lines); !slices.Equal(found, []string{"github.com/urnetwork/message/protocol github.com/urnetwork/message"}) {
		t.Errorf("go list over a module that links the message module answered %q, from which the build reading took %q", lines, found)
	}
}
