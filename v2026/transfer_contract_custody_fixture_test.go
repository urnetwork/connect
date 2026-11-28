package connect

import (
	"os"
	"path/filepath"
	"testing"
)

// Custody opens each ancestor without following symlinks. Only resolve roots
// just created by the test; production paths and deliberate aliases stay exact.
func physicalTempDir(t *testing.T) string {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return directory
}

// A subtest's first TempDir must traverse this alias even on hosts whose normal
// temporary directory is physical. The parent already owns the backing root.
func useSymlinkCustodyTempDir(t *testing.T) {
	t.Helper()
	parent := physicalTempDir(t)
	directory := filepath.Join(parent, "physical")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(parent, "alias")
	if err := os.Symlink(directory, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", alias)
	t.Setenv("GOTMPDIR", "")
}
