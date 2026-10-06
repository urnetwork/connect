// the vector corpus is pinned by digest. Re-vendoring at a newer mlswg commit is a
// deliberate act with a visible diff, never something that happens during a rebase.
package mls

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// vectorFiles is Spec A §4.2.1, in table order.
var vectorFiles = []string{
	"tree-math.json",
	"crypto-basics.json",
	"secret-tree.json",
	"message-protection.json",
	"key-schedule.json",
	"psk_secret.json",
	"transcript-hashes.json",
	"welcome.json",
	"tree-operations.json",
	"tree-validation.json",
	"treekem.json",
	"messages.json",
	"passive-client-welcome.json",
	"passive-client-handling-commit.json",
	"passive-client-random.json",
	"deserialization.json",
}

// TestVectorFilesArePinned asserts every file named in vectorFiles is present under
// testdata/vectors and its sha256 digest matches the recorded VECTORS.sha256 manifest,
// so a corpus file cannot drift from what was actually fetched from upstream.
func TestVectorFilesArePinned(t *testing.T) {
	manifest := map[string]string{}
	handle, err := os.Open(filepath.Join("testdata", "vectors", "VECTORS.sha256"))
	if err != nil {
		t.Fatalf("open VECTORS.sha256: %v", err)
	}
	defer handle.Close()
	scanner := bufio.NewScanner(handle)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 {
			manifest[fields[1]] = fields[0]
		}
	}
	if len(manifest) != len(vectorFiles) {
		t.Fatalf("VECTORS.sha256 lists %d files, spec A §4.2.1 names %d", len(manifest), len(vectorFiles))
	}
	for _, name := range vectorFiles {
		body, err := os.ReadFile(filepath.Join("testdata", "vectors", name))
		if err != nil {
			t.Errorf("read %s: %v", name, err)
			continue
		}
		sum := sha256.Sum256(body)
		got := hex.EncodeToString(sum[:])
		if want := manifest[name]; got != want {
			t.Errorf("%s digest %s, manifest says %s", name, got, want)
		}
	}
}

// TestVectorFolderHoldsExactlySixteenFiles keeps the mlswg corpus separable from
// everything else we vendor. The crypto plan's RFC 9180 and X-Wing vectors are not
// mlswg files and live under testdata/vectors/rfc/, so a seventeenth top-level file
// is either an un-manifested mlswg family or a file in the wrong place.
func TestVectorFolderHoldsExactlySixteenFiles(t *testing.T) {
	found, err := filepath.Glob(filepath.Join("testdata", "vectors", "*.json"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(found) != 16 {
		t.Fatalf("testdata/vectors holds %d json files, spec A §4.2.1 names 16: %v", len(found), found)
	}
}

// TestPinsAreMachineReadable asserts the one pin file exists and carries the two
// lines the framing and lifecycle plans grep for. Three pin files in three formats
// is how two greps end up matching none of them.
func TestPinsAreMachineReadable(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("interop", "PINS.md"))
	if err != nil {
		t.Fatalf("read interop/PINS.md: %v", err)
	}
	text := string(body)
	for _, key := range []string{"mlswg=", "openmls="} {
		index := strings.Index(text, key)
		if index < 0 {
			t.Errorf("interop/PINS.md has no machine-readable %s<sha> line", key)
			continue
		}
		if len(strings.Fields(text[index:])[0]) != len(key)+40 {
			t.Errorf("%s must be followed by a full 40-character commit sha", key)
		}
	}
	for _, stale := range []string{
		filepath.Join("PINS.md"),
		filepath.Join("testdata", "vectors", "PINS.md"),
	} {
		if _, err := os.Stat(stale); err == nil {
			t.Errorf("%s must not exist; interop/PINS.md is the one pin file", stale)
		}
	}
}
