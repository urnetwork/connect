// The structural gates on this package. The first is the dependency gate: it must
// depend on nothing outside the standard library. Spec A section 2.3 makes
// connect/mls/syntax stdlib only so the codec can be audited and fuzzed with no
// transport, no crypto and no third party code in the graph, and so importing it
// from any wave creates no cycle.
//
// The second is the continuous integration gate, asserted from here for the same
// reason: slice A1 is done when family 16 passes and the fuzz properties are clean
// for 60 seconds on each target, and a done-when that only a reviewer checks is a
// note rather than a gate. These checks read the workflow as text, because parsing
// yaml would need a dependency the first gate forbids, so they can see that a step
// is spelled correctly and cannot see that the file is valid yaml or that the job
// schedules. Running the workflow's own commands is what establishes the rest.
package syntax

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

const selfImportPath = "github.com/urnetwork/connect/mls/syntax"

// The per commit gate lives here, relative to the package directory the tests run in.
const workflowPath = "../../.github/workflows/mls-syntax.yml"

// Fails if go list -deps reports a dependency whose first path element is not stdlib.
func TestSyntaxImportsStdlibOnly(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps failed: %v\n%s", err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		dep := strings.TrimSpace(line)
		if dep == "" || dep == selfImportPath {
			continue
		}
		// every standard library import path has a dot free first element
		first, _, _ := strings.Cut(dep, "/")
		if strings.Contains(first, ".") {
			t.Errorf("non stdlib dependency %s; this package is stdlib only per spec A section 2.3", dep)
		}
	}
}

// Reads the workflow, failing the calling test rather than returning an error,
// because every check below is meaningless if the file is not there.
func readSyntaxWorkflow(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatalf("reading the syntax workflow: %v", err)
	}
	return string(raw)
}

// Scans the package's own test sources for fuzz target declarations, so the list
// the workflow is checked against is derived rather than restated. A hand written
// list would still pass on the day a fourth target is added and never run in the
// gate, which is the failure this file exists to catch.
func declaredFuzzTargets(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}
	targets := []string{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		raw, err := os.ReadFile(entry.Name())
		if err != nil {
			t.Fatalf("reading %s: %v", entry.Name(), err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if !strings.HasPrefix(line, "func Fuzz") {
				continue
			}
			name, _, found := strings.Cut(strings.TrimPrefix(line, "func "), "(")
			if found {
				targets = append(targets, name)
			}
		}
	}
	// a scan that silently matched nothing would make the caller's loop vacuous
	if len(targets) == 0 {
		t.Fatal("found no fuzz targets in the package sources; the scan is broken, not the workflow")
	}
	return targets
}

// The 60 seconds on each of the three targets that slice A1's done-when asks for is
// only a gate if the workflow exists, pins the toolchain this plan is built against
// and names every target the package actually declares.
func TestSyntaxWorkflowRunsEveryFuzzTarget(t *testing.T) {
	workflow := readSyntaxWorkflow(t)
	needles := []string{
		"go-version: '1.26.5'",
		"go vet ./mls/syntax",
		"go test ./mls/syntax/... -count=1 -race",
	}
	for _, target := range declaredFuzzTargets(t) {
		needles = append(needles, "-fuzz="+target+" -fuzztime=60s")
	}
	for _, needle := range needles {
		if !strings.Contains(workflow, needle) {
			t.Errorf("the syntax workflow does not run %q", needle)
		}
	}
}

// A workflow can name every command and still gate nothing, in two ways this file
// can see. It can trigger on a branch the work is not on, so it never runs at all.
// Or a step can carry continue-on-error, which reports a failure and passes the job
// anyway — this repo's own history has that on the flaky extender step in
// .github/workflows/test.yml, so it is a convention a later edit may well copy here,
// where the whole point is to block the commit.
func TestSyntaxWorkflowGatesRatherThanReports(t *testing.T) {
	workflow := readSyntaxWorkflow(t)
	for _, needle := range []string{"push:", "pull_request:", "- main"} {
		if !strings.Contains(workflow, needle) {
			t.Errorf("the syntax workflow is missing %q, so it does not run on the branch this work is on", needle)
		}
	}
	if strings.Contains(workflow, "continue-on-error") {
		t.Error("the syntax workflow has a continue-on-error step; a step that cannot fail the job gates nothing")
	}
}
