package connect

// The randomness rule of connect's CODESTYLE.md ("Randomness"), checked over
// every non-test go file of this repository with go/parser (connect's
// RANDUPDATE.md 4.3). A file fails on:
//
//   - an import of math/rand (v1);
//   - a generator seeded on purpose: NewSource, NewPCG or a PCG value, or a
//     call to Seed;
//   - a ChaCha8 generator (NewChaCha8 or a ChaCha8 value) outside
//     connect.NewSecureRand, in connect's random.go.
//
// A PCG or ChaCha8 value counts with its constructor, because its zero value is
// a fixed seed. Test files may keep deterministic generators, so they are not
// read. Neither are the allowlisted paths: vendored code, and simulations and
// tools whose seeded generators must reproduce their recorded output exactly.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Where the rule bends. Paths are slash separated and relative to the
// repository root; an entry that ends with a slash is a directory.
type randomRuleAllowlist struct {
	// paths the rule does not read
	paths []string
	// the one function allowed to build a ChaCha8 generator. Empty in a
	// repository that takes it from connect.
	helperPath     string
	helperFuncName string
}

// The rule's findings for one parsed non-test file, each as "path:line: what".
// path is slash separated and relative to the repository root.
func randomRuleFindings(fset *token.FileSet, file *ast.File, path string, allowlist randomRuleAllowlist) []string {
	for _, entry := range allowlist.paths {
		if path == entry || strings.HasSuffix(entry, "/") && strings.HasPrefix(path, entry) {
			return nil
		}
	}

	findings := []string{}
	report := func(pos token.Pos, what string) {
		findings = append(findings, fmt.Sprintf("%s:%d: %s", path, fset.Position(pos).Line, what))
	}

	randNames := map[string]bool{}
	for _, spec := range file.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil || importPath != "math/rand" && importPath != "math/rand/v2" {
			continue
		}
		if importPath == "math/rand" {
			report(spec.Pos(), "imports math/rand; use math/rand/v2")
		}
		name := "rand"
		if spec.Name != nil {
			name = spec.Name.Name
		}
		switch name {
		case "_":
		case ".":
			report(spec.Pos(), fmt.Sprintf("dot-imports %s, which hides its calls from this rule", importPath))
		default:
			randNames[name] = true
		}
	}

	for _, decl := range file.Decls {
		inHelper := false
		if funcDecl, ok := decl.(*ast.FuncDecl); ok && funcDecl.Recv == nil {
			inHelper = allowlist.helperPath != "" && path == allowlist.helperPath && funcDecl.Name.Name == allowlist.helperFuncName
		}
		ast.Inspect(decl, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.SelectorExpr:
				ident, ok := node.X.(*ast.Ident)
				if !ok || !randNames[ident.Name] {
					return true
				}
				switch node.Sel.Name {
				case "NewSource", "NewPCG", "PCG", "Seed":
					report(node.Pos(), fmt.Sprintf("%s.%s seeds a predictable generator; use the top-level math/rand/v2 functions or connect.NewSecureRand()", ident.Name, node.Sel.Name))
				case "NewChaCha8", "ChaCha8":
					if !inHelper {
						report(node.Pos(), fmt.Sprintf("%s.%s outside NewSecureRand; use connect.NewSecureRand()", ident.Name, node.Sel.Name))
					}
				}
			case *ast.CallExpr:
				// a generator's Seed method; a key's seed is read with no arguments
				selector, ok := node.Fun.(*ast.SelectorExpr)
				if !ok || selector.Sel.Name != "Seed" || len(node.Args) == 0 {
					return true
				}
				if ident, ok := selector.X.(*ast.Ident); ok && randNames[ident.Name] {
					// reported as a selector
					return true
				}
				report(node.Pos(), "Seed re-seeds a generator; use the top-level math/rand/v2 functions or connect.NewSecureRand()")
			}
			return true
		})
	}
	return findings
}

// Every non-test go file of this repository keeps the randomness rule. Each
// finding names its file and line.
func TestRandomnessRule(t *testing.T) {
	allowlist := randomRuleAllowlist{
		// connect has no vendored code and no simulation that seeds a generator
		paths:          []string{},
		helperPath:     "random.go",
		helperFuncName: "NewSecureRand",
	}

	// this package is the repository root
	if _, err := os.Stat("go.mod"); err != nil {
		t.Fatalf("the rule walks from the repository root, but this package has no go.mod: %v", err)
	}
	// an entry that names nothing would exempt whatever later takes its name
	for _, entry := range allowlist.paths {
		info, err := os.Stat(filepath.FromSlash(strings.TrimSuffix(entry, "/")))
		if err != nil || info.IsDir() != strings.HasSuffix(entry, "/") {
			t.Errorf("allowlist entry %s names nothing in this repository", entry)
		}
	}

	findings := []string{}
	judgedPaths := []string{}
	err := filepath.WalkDir(".", func(osPath string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := entry.Name()
		if entry.IsDir() {
			// what the go tool leaves out of a module's packages
			if osPath != "." && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata" || name == "vendor" || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, osPath, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		path := filepath.ToSlash(osPath)
		judgedPaths = append(judgedPaths, path)
		findings = append(findings, randomRuleFindings(fset, file, path, allowlist)...)
		return nil
	})
	if err != nil {
		t.Fatalf("walk the repository: %v", err)
	}
	for _, finding := range findings {
		t.Error(finding)
	}
	if len(judgedPaths) == 0 {
		t.Error("the walk judged no file, so it read the wrong tree")
	}
	if allowlist.helperPath != "" && !slices.Contains(judgedPaths, allowlist.helperPath) {
		t.Errorf("the walk judged %d files but not %s, so it read the wrong tree", len(judgedPaths), allowlist.helperPath)
	}
}

// The rule finds each way to build a predictable generator and lets through
// what it allows, so a pass over the repository means something.
func TestRandomnessRuleFindsEachViolation(t *testing.T) {
	allowlist := randomRuleAllowlist{
		paths:          []string{"third_party/", "sim/tool.go"},
		helperPath:     "random.go",
		helperFuncName: "NewSecureRand",
	}
	cases := []struct {
		path        string
		source      string
		expectCount int
	}{
		{path: "a.go", source: `package a; import "math/rand/v2"; func f() int { return rand.IntN(2) }`, expectCount: 0},
		{path: "a.go", source: `package a; import "math/rand"; func f() int { return rand.Intn(2) }`, expectCount: 1},
		{path: "a.go", source: `package a; import r "math/rand"; func f() { r.Seed(1) }`, expectCount: 2},
		{path: "a.go", source: `package a; import r "math/rand"; func f() *r.Rand { return r.New(r.NewSource(1)) }`, expectCount: 2},
		{path: "a.go", source: `package a; import mathrand "math/rand/v2"; func f() *mathrand.Rand { return mathrand.New(mathrand.NewPCG(1, 2)) }`, expectCount: 1},
		{path: "a.go", source: `package a; import "math/rand/v2"; func f() { p := &rand.PCG{}; p.Seed(1, 2) }`, expectCount: 2},
		{path: "a.go", source: `package a; import "math/rand/v2"; func f() *rand.Rand { return rand.New(rand.NewChaCha8([32]byte{})) }`, expectCount: 1},
		{path: "a.go", source: `package a; import "math/rand/v2"; func f() *rand.Rand { return rand.New(new(rand.ChaCha8)) }`, expectCount: 1},
		{path: "a.go", source: `package a; import . "math/rand/v2"; func f() int { return IntN(2) }`, expectCount: 1},
		{path: "a.go", source: `package a; type key interface{ Seed() []byte }; func f(k key) []byte { return k.Seed() }`, expectCount: 0},
		{path: "random.go", source: `package a; import "math/rand/v2"; func NewSecureRand() *rand.Rand { return rand.New(rand.NewChaCha8([32]byte{})) }`, expectCount: 0},
		{path: "random.go", source: `package a; import "math/rand/v2"; func other() *rand.Rand { return rand.New(rand.NewChaCha8([32]byte{})) }`, expectCount: 1},
		{path: "b/random.go", source: `package a; import "math/rand/v2"; func NewSecureRand() *rand.Rand { return rand.New(rand.NewChaCha8([32]byte{})) }`, expectCount: 1},
		{path: "third_party/a/a.go", source: `package a; import "math/rand"; func f() *rand.Rand { rand.Seed(1); return rand.New(rand.NewSource(1)) }`, expectCount: 0},
		{path: "sim/tool.go", source: `package a; import "math/rand"; func f() *rand.Rand { return rand.New(rand.NewSource(1)) }`, expectCount: 0},
		{path: "sim/other.go", source: `package a; import "math/rand/v2"; func f() *rand.Rand { return rand.New(rand.NewPCG(1, 2)) }`, expectCount: 1},
		{path: "third_party_fork/a.go", source: `package a; import "math/rand"; func f() int { return rand.Intn(2) }`, expectCount: 1},
	}
	for _, c := range cases {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, c.path, c.source, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("%s: %v", c.source, err)
		}
		findings := randomRuleFindings(fset, file, c.path, allowlist)
		if len(findings) != c.expectCount {
			t.Errorf("%s: %s: %d findings %v, expected %d", c.path, c.source, len(findings), findings, c.expectCount)
		}
	}
}
