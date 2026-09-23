// Runs the suite launcher against disposable Go packages and replay fixtures.
// Stubbed NGINX and gate commands own no network or shared test resources.
package connect

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// Owns the synthetic source, command trace and launcher environment.
type connectTestScriptFixture struct {
	directory string
	env       []string
	tracePath string
}

// Uses real Go discovery and compilation while isolating all source and tools
// from the checkout. Captured child failures never leak into passing output.
func newConnectTestScriptFixture(t *testing.T) *connectTestScriptFixture {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("the Connect suite launcher needs a POSIX host with zsh")
	}
	if _, err := exec.LookPath("zsh"); err != nil {
		t.Fatal(err)
	}
	goPath, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	launcher, err := os.ReadFile("test.sh")
	if err != nil {
		t.Fatal(err)
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(base, "workspace with spaces")
	fixture := &connectTestScriptFixture{
		directory: filepath.Join(workspace, "connect"),
		tracePath: filepath.Join(workspace, "trace"),
	}
	fixture.write(t, "test.sh", string(launcher), 0o700)
	fixture.write(t, "../tests/network-intensive-suite-lock.sh", `#!/bin/sh
if [ "$1" = --verify-held ]; then
  [ "$2" = run-all ] && [ "$URNETWORK_NETWORK_TEST_LOCK_HELD" = 1 ]
  exit $?
fi
[ "$1" = run-all ] && [ "$2" = run-all-connect ] && [ "$3" = -- ] || exit 70
shift 3
exec env URNETWORK_NETWORK_TEST_LOCK_HELD=1 "$@"
`, 0o700)
	fixture.write(t, "../bin/make", "#!/bin/sh\nexit 0\n", 0o700)
	fixture.write(t, "../bin/go", `#!/bin/sh
if [ "$1" = test ]; then
  if [ "$PWD" = "$CONNECT_TEST_FIXTURE_ROOT" ]; then package=.; else package="${PWD#"$CONNECT_TEST_FIXTURE_ROOT"/}"; fi
  printf 'go-test:%s\n' "$package" >>"$CONNECT_TEST_FIXTURE_TRACE"
fi
exec "$CONNECT_TEST_FIXTURE_GO" "$@"
`, 0o700)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "URNETWORK_NETWORK_TEST_") || strings.HasPrefix(name, "CONNECT_TEST_FIXTURE_") {
			continue
		}
		switch name {
		case "PATH", "URNETWORK_ROOT", "GOWORK", "GOENV", "GOTOOLCHAIN", "GOFLAGS":
			continue
		}
		fixture.env = append(fixture.env, entry)
	}
	fixture.env = append(fixture.env,
		"PATH="+filepath.Join(workspace, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"),
		"URNETWORK_ROOT="+workspace,
		"CONNECT_TEST_FIXTURE_ROOT="+fixture.directory,
		"CONNECT_TEST_FIXTURE_TRACE="+fixture.tracePath,
		"CONNECT_TEST_FIXTURE_GO="+goPath,
		"GOWORK=off", "GOENV=off", "GOTOOLCHAIN=local", "GOFLAGS=",
	)
	fixture.write(t, "go.mod", "module connect-fixture.example/root\n\ngo 1.26\n", 0o600)
	fixture.write(t, "fixture_test.go", "package fixture\nimport \"testing\"\nfunc TestFixture(t *testing.T) {}\n", 0o600)
	return fixture
}

// Writes inside the temporary workspace, including the sibling tool stubs.
func (self *connectTestScriptFixture) write(t *testing.T, relative, content string, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(self.directory, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// Joins every subprocess and returns its captured output and package trace.
func (self *connectTestScriptFixture) run(t *testing.T, args ...string) (string, []string, error) {
	t.Helper()
	command := exec.CommandContext(t.Context(), "zsh", append([]string{"./test.sh"}, args...)...)
	command.Dir = self.directory
	command.Env = self.env
	output, err := command.CombinedOutput()
	if err == nil && strings.Contains(string(output), "basename:") {
		t.Fatalf("source highlighting split the workspace path: %s", output)
	}
	trace, readErr := os.ReadFile(self.tracePath)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		t.Fatal(readErr)
	}
	return string(output), strings.Fields(string(trace)), err
}

// Reproduces the exact failure boundary: these replay inputs compile only
// when installed beside root-package definitions by their separate runner.
func TestTestScriptPackageDiscoveryExcludesReplayFixtures(t *testing.T) {
	fixture := newConnectTestScriptFixture(t)
	fixture.write(t, "fixture.go", "package fixture\ntype rootOnlyType int\n", 0o600)
	fixture.write(t, "testdata/throughput_root_cases/receiver/fixture_test.go", "package fixture\nvar replay rootOnlyType\n", 0o600)
	output, trace, err := fixture.run(t, "-count=1", "-run", "^TestFixture$")
	if err != nil {
		t.Fatalf("replay fixture was admitted as a package: %v\n%s", err, output)
	}
	expected := []string{"go-test:.", "go-test:.", "go-test:.", "go-test:."}
	if !slices.Equal(trace, expected) {
		t.Fatalf("package commands = %q, want %q", trace, expected)
	}
}

// Active tests include external and race-tagged packages; fixture, vendor,
// platform-only and nested-module sources obey Go's package boundaries.
func TestTestScriptPackageDiscoveryHonorsGoBoundaries(t *testing.T) {
	fixture := newConnectTestScriptFixture(t)
	fixture.write(t, "fixture.go", "package fixture\ntype rootOnlyType int\n", 0o600)
	for _, directory := range []string{
		"testdata/throughput_root_cases/receiver", "child/testdata/receiver",
		".archive/receiver", "_archive/receiver", "vendor/fixture.example/receiver",
	} {
		fixture.write(t, directory+"/fixture_test.go", "package fixture\nvar replay rootOnlyType\n", 0o600)
	}
	for _, source := range []struct {
		path        string
		packageName string
		constraint  string
	}{
		{path: "child/fixture_test.go", packageName: "fixture"},
		{path: "external/fixture_test.go", packageName: "fixture_test"},
		{path: "raceonly/fixture_test.go", packageName: "fixture", constraint: "//go:build race\n\n"},
		{path: "wasmonly/fixture_wasm_test.go", packageName: "fixture"},
	} {
		fixture.write(t, source.path, source.constraint+"package "+source.packageName+"\nimport \"testing\"\nfunc TestFixture(t *testing.T) {}\n", 0o600)
	}
	fixture.write(t, "tools/nested/go.mod", "module connect-fixture.example/nested\n\ngo 1.26\n", 0o600)
	fixture.write(t, "tools/nested/fixture_test.go", "package fixture\nvar replay rootOnlyType\n", 0o600)
	output, trace, err := fixture.run(t, "-count=1", "-run", "^TestFixture$")
	if err != nil {
		t.Fatalf("package discovery failed: %v\n%s", err, output)
	}
	expected := []string{"go-test:.", "go-test:.", "go-test:.", "go-test:.", "go-test:child", "go-test:external", "go-test:raceonly"}
	if !slices.Equal(trace, expected) {
		t.Fatalf("package commands = %q, want %q", trace, expected)
	}
}

// Build selection must agree between discovery and execution, including a
// test pattern that happens to look like a package-loading flag.
func TestTestScriptPackageDiscoveryHonorsBuildTags(t *testing.T) {
	for _, options := range []struct {
		args     []string
		flags    string
		selected bool
	}{
		{args: []string{"-tags", "fixture_tag"}, selected: true},
		{args: []string{"-tags=fixture_tag"}, selected: true},
		{flags: "-tags=fixture_tag", selected: true},
		{args: []string{"-run", "-tags", "-tags", "fixture_tag"}, selected: true},
		{args: []string{"-list", "-tags", "-tags", "fixture_tag"}, selected: true},
		{args: []string{"-tags=other_fixture_tag"}, flags: "-tags=fixture_tag", selected: false},
	} {
		fixture := newConnectTestScriptFixture(t)
		fixture.write(t, "tagged/fixture_test.go", "//go:build fixture_tag\n\npackage fixture\nimport \"testing\"\nfunc TestFixture(t *testing.T) {}\n", 0o600)
		fixture.env = append(fixture.env, "GOFLAGS="+options.flags)
		args := append([]string{"-count=1", "-run", "^TestFixture$"}, options.args...)
		output, trace, err := fixture.run(t, args...)
		if err != nil {
			t.Fatalf("tags args=%q flags=%q: %v\n%s", options.args, options.flags, err, output)
		}
		expected := []string{"go-test:.", "go-test:.", "go-test:.", "go-test:."}
		if options.selected {
			expected = append(expected, "go-test:tagged")
		}
		if !slices.Equal(trace, expected) {
			t.Fatalf("tags args=%q flags=%q: commands=%q, want %q", options.args, options.flags, trace, expected)
		}
	}
}

// A real source-package error must remain terminal even when Go lists other
// valid packages before returning its nonzero discovery status.
func TestTestScriptPackageDiscoveryErrorStopsSuite(t *testing.T) {
	fixture := newConnectTestScriptFixture(t)
	fixture.write(t, "broken/a.go", "package first\n", 0o600)
	fixture.write(t, "broken/b.go", "package second\n", 0o600)
	output, trace, err := fixture.run(t, "-count=1", "-run", "^TestFixture$")
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 1 || !strings.Contains(output, "found packages first") {
		t.Fatalf("source discovery error result: %v\n%s", err, output)
	}
	expected := []string{"go-test:.", "go-test:.", "go-test:."}
	if !slices.Equal(trace, expected) {
		t.Fatalf("discovery error continued the suite: %q", trace)
	}
}
