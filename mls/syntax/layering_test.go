// The structural gate on this package is the dependency gate: it must depend on
// nothing outside the standard library. Spec A section 2.3 makes
// connect/mls/syntax stdlib only so the codec can be audited and fuzzed with no
// transport, no crypto and no third party code in the graph, and so importing it
// from any wave creates no cycle.
package syntax

import (
	"os/exec"
	"strings"
	"testing"
)

const selfImportPath = "github.com/urnetwork/connect/mls/syntax"

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
