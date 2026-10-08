// The mlswg corpus, anchored to the commit interop/PINS.md pins rather than to itself.
//
// VECTORS.sha256 is a manifest computed over the bytes in this checkout, so every check
// made against it — TestVectorFilesArePinned in vectors_pin_test.go, the manifest half of
// TestVectorFilesPresent in key_schedule_deps_test.go — compares the local bytes to a
// digest of the local bytes. That detects a file edited without the manifest being
// updated, which is worth having, and it detects nothing at all about a file edited WITH
// the manifest updated. Re-recording a line of VECTORS.sha256 is one command, and after it
// sixteen of sixteen verify against bytes upstream never published.
//
// What closes that is an anchor outside the tree, which is what this file is: the sha256
// of each blob as mlswg published it at the pinned commit. Those digests were not invented
// here; each is `git cat-file -p <mlswg=commit>:test-vectors/<name> | sha256sum` against a
// clone of mlswg/mls-implementations, and the commit is cross-checked against the pin file
// below so the two cannot drift apart in silence. This is the shape hpke_vectors_test.go
// already uses for the RFC 9180 corpus; the mlswg corpus simply never got it.
//
// The known smudge, and why it is tolerated here rather than asserted away. All sixteen
// files were vendored through a checkout with core.autocrlf on, so each carries a carriage
// return on every line and none of them is byte-identical to what upstream published. The
// content is untouched — normalise the line endings and all sixteen match upstream exactly,
// which is asserted below — so no known answer in the corpus is wrong. Re-vendoring at LF
// belongs to the task that vendored them (p8 task 6) and is deliberately not done here,
// because a re-vendor rewrites sixteen files and this is a test task. What is done here is
// to make the provenance claim true again: the bytes are held to upstream modulo line
// endings, and the line ending difference itself is asserted to be exactly that and
// nothing more. A re-vendor at LF passes every test in this file unchanged.
package mls

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	// mlswgVectorUpstreamRepository is the repository the corpus is published from.
	mlswgVectorUpstreamRepository = "mlswg/mls-implementations"

	// mlswgVectorUpstreamCommit is the commit the digests below were read at. It is the
	// same forty characters as the mlswg= line of interop/PINS.md, and
	// TestMlswgVectorCommitMatchesThePinFile is what keeps it so.
	mlswgVectorUpstreamCommit = "cfd450286d1bfd9cd2519b95c80f9771f94a5b1a"

	// mlswgVectorUpstreamDirectory is the path within that repository.
	mlswgVectorUpstreamDirectory = "test-vectors"
)

// mlswgVectorDirectory is where the corpus is vendored, relative to this package.
var mlswgVectorDirectory = filepath.Join("testdata", "vectors")

// mlswgVectorUpstreamSha256 is the digest of each published blob at
// mlswgVectorUpstreamCommit, over the bytes upstream stores: LF, no carriage returns.
//
// This is a transcription, and the class it transcribes is not derivable in this tree —
// the bytes it names live in another repository at a commit this one only records the name
// of, and a test cannot fetch them. What IS derived is the membership: the loop below
// reads the vector directory and requires every file it finds to appear here and every
// name here to be a file, so a seventeenth family cannot land unanchored and an entry
// cannot be deleted to make a failure go away.
var mlswgVectorUpstreamSha256 = map[string]string{
	"crypto-basics.json":                  "17cfcf89af9f51d0f2aa7af77f6f9ec99376a039214b6d42a6f11646b83e8c29",
	"deserialization.json":                "1e394706e79f77df71454e5970f2aa736d4ab8b6c7e3219dd5609992388a953e",
	"key-schedule.json":                   "05aa9a68bd2538ace72d8c53375984cc728ef62220ebf314df675708546d97a7",
	"message-protection.json":             "f7c1ae62ce63c3003e526d539c99f2b1444f65ee0a487d3a5667d46867526c45",
	"messages.json":                       "b194abe1561995223482dbad51c180146920dc2f637e74d01e07a388308791fb",
	"passive-client-handling-commit.json": "b24949fdf857bf79d511f57504a8bdc7ce3247eb199684745e439c11b1a1da21",
	"passive-client-random.json":          "0095d863e9d316872e237fc708debcd20ba2687ee905a3e831f97dd3e578d3c1",
	"passive-client-welcome.json":         "92ebb04b67b1aca4290965ae363650921842ceebe275bd79a2733414078d849c",
	"psk_secret.json":                     "2b534969dba0b65a04b7d790496af5c0ccdb472b3fc4ca25c8c82df3e8523784",
	"secret-tree.json":                    "08f92e6272452e2c832e32d38e16cf0c4aa28967e47d3842c60bc354c6b67a94",
	"transcript-hashes.json":              "58046b58fbd98519b4e60953a105f550e4b427e2b140ea67dedcb751260eecb8",
	"tree-math.json":                      "27f04891f56106593b74b674445f01b845e173953770c084deb2f8cf5592e2fc",
	"tree-operations.json":                "9a25f8720714d1256ba2dad8d83a660ba0bae0d33ed15c682b9d74a5d67aad0a",
	"tree-validation.json":                "b66a432d93b520623f0640835ed43b9dfc95e116b8be081d81581d7c11e67a37",
	"treekem.json":                        "d8bbdf78394797f21db7476fd286af6830ebcb18d2e3b2c839fecd4eb11056f8",
	"welcome.json":                        "06be9d5c99817ef2545e4b15b8e73fd9b604685a8e55b59ca168eda98e236502",
}

// vendoredMlswgVectorNames reads the corpus directory and returns the json files in it,
// sorted. It is the class every gate in this file runs over, derived rather than listed,
// so a family added or removed is judged instead of ignored.
func vendoredMlswgVectorNames(t *testing.T) []string {
	t.Helper()
	found, err := filepath.Glob(filepath.Join(mlswgVectorDirectory, "*.json"))
	if err != nil {
		t.Fatalf("glob %s: %v", mlswgVectorDirectory, err)
	}
	names := []string{}
	for _, path := range found {
		names = append(names, filepath.Base(path))
	}
	slices.Sort(names)
	if len(names) == 0 {
		t.Fatalf("no json file under %s, so every check in this file would pass over nothing",
			mlswgVectorDirectory)
	}
	return names
}

// TestMlswgVectorCommitMatchesThePinFile ties the commit these digests were read at to the
// one pin file, field by field rather than by a substring search over the whole document.
//
// Without it the two copies drift: someone bumps mlswg= to a newer corpus, re-vendors, and
// the digests here still name the old commit while claiming to describe the new bytes. The
// failure then reads as "the vectors are corrupt" rather than as "the pin moved".
func TestMlswgVectorCommitMatchesThePinFile(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("interop", "PINS.md"))
	if err != nil {
		t.Fatalf("read interop/PINS.md: %v", err)
	}
	const key = "mlswg="
	pinned := ""
	for _, line := range strings.Split(string(body), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, key) {
			continue
		}
		if pinned != "" {
			t.Fatalf("interop/PINS.md carries more than one %s line, so which one is normative is a guess", key)
		}
		pinned = strings.TrimPrefix(trimmed, key)
	}
	if pinned == "" {
		t.Fatalf("interop/PINS.md has no machine-readable %s<sha> line to anchor these digests to", key)
	}
	if pinned != mlswgVectorUpstreamCommit {
		t.Fatalf("interop/PINS.md pins %s%s, the digests in this file were read at %s; whichever moved, the other has to move with it",
			key, pinned, mlswgVectorUpstreamCommit)
	}
}

// TestEveryVendoredVectorHashesToItsUpstreamBlob is the anchor the manifest cannot be.
//
// It reads each vendored file, normalises the line endings, and compares the digest to
// what mlswg published at the pinned commit. A byte changed anywhere in the corpus fails
// here, and re-recording VECTORS.sha256 does not help, because this comparison never
// consults VECTORS.sha256.
//
// Membership is derived from the directory in both directions: a family present without an
// upstream digest is unanchored, and a digest without a file is a line kept alive after the
// file it describes was removed.
func TestEveryVendoredVectorHashesToItsUpstreamBlob(t *testing.T) {
	names := vendoredMlswgVectorNames(t)
	for _, name := range names {
		if _, anchored := mlswgVectorUpstreamSha256[name]; !anchored {
			t.Errorf("%s is vendored and mlswgVectorUpstreamSha256 does not name it, so nothing holds it to %s/%s at %s; read its digest there and add it",
				name, mlswgVectorUpstreamRepository, mlswgVectorUpstreamDirectory, mlswgVectorUpstreamCommit)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(mlswgVectorUpstreamSha256)) {
		if !slices.Contains(names, name) {
			t.Errorf("mlswgVectorUpstreamSha256 names %s and %s holds no such file",
				name, mlswgVectorDirectory)
		}
	}

	checked := 0
	for _, name := range names {
		want, anchored := mlswgVectorUpstreamSha256[name]
		if !anchored {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(mlswgVectorDirectory, name))
		if err != nil {
			t.Errorf("read %s: %v", name, err)
			continue
		}
		digest := sha256.Sum256(normalisedLineEndings(raw))
		got := hex.EncodeToString(digest[:])
		if got != want {
			t.Errorf("%s hashes to %s with its line endings normalised, %s/%s at %s published %s; this file is not the vector mlswg published, whatever VECTORS.sha256 says about it",
				name, got, mlswgVectorUpstreamRepository, mlswgVectorUpstreamDirectory,
				mlswgVectorUpstreamCommit, want)
			continue
		}
		checked++

		// the comparison has to be able to fail, and a digest of the wrong thing --
		// of a constant, of an empty slice -- would match nothing and be reported as
		// a corrupt corpus rather than as a broken test. one extra byte must not match.
		mutated := sha256.Sum256(append(normalisedLineEndings(raw), 0x00))
		if hex.EncodeToString(mutated[:]) == want {
			t.Errorf("%s: a byte appended to the file hashes to the same digest, so this comparison is not reading the file", name)
		}
	}
	if checked != len(mlswgVectorUpstreamSha256) {
		t.Errorf("%d of %d anchored vectors were checked against upstream", checked, len(mlswgVectorUpstreamSha256))
	}
}

// TestVendoredVectorsDifferFromUpstreamOnlyInLineEndings says what the normalisation above
// is allowed to hide.
//
// Normalising before comparing is what lets the check pass over a corpus this checkout
// smudged, and it is also a hole if the normalisation is doing more than it says: a rule
// that deleted every carriage return would also hide a carriage return introduced INSIDE a
// hex string, which json would not treat as whitespace. So every carriage return in every
// file is asserted to be the first half of a CRLF pair and nothing else, which is the
// difference between "these are the upstream bytes with the line endings a windows
// checkout writes" and "these are bytes that happen to normalise the same way".
//
// A file re-vendored at LF has no carriage returns and passes trivially, which is the
// intent: this asserts a bound on the difference, not the presence of one.
func TestVendoredVectorsDifferFromUpstreamOnlyInLineEndings(t *testing.T) {
	for _, name := range vendoredMlswgVectorNames(t) {
		raw, err := os.ReadFile(filepath.Join(mlswgVectorDirectory, name))
		if err != nil {
			t.Errorf("read %s: %v", name, err)
			continue
		}
		carriageReturns := bytes.Count(raw, []byte{'\r'})
		pairs := bytes.Count(raw, []byte{'\r', '\n'})
		if carriageReturns != pairs {
			t.Errorf("%s holds %d carriage returns of which %d begin a CRLF pair; a bare CR is a content difference from upstream, not a line ending, and normalising it away would hide it",
				name, carriageReturns, pairs)
			continue
		}
		if len(normalisedLineEndings(raw)) != len(raw)-carriageReturns {
			t.Errorf("%s: normalising removed something other than the %d carriage returns it holds",
				name, carriageReturns)
		}
	}
}

// TestTheManifestStillDescribesTheBytesOnDisk keeps the local manifest meaningful next to
// the upstream anchor rather than replacing it.
//
// The two answer different questions. VECTORS.sha256 says "this file has not changed since
// it was vendored", which is the question a rebase or a stray editor save answers wrongly;
// the upstream digests say "what was vendored is what upstream published". Only the second
// survives an edit made together with a manifest update, and only the first notices a file
// that upstream would still recognise but this checkout no longer stores verbatim.
func TestTheManifestStillDescribesTheBytesOnDisk(t *testing.T) {
	manifest := readVectorManifest(t)
	for _, name := range vendoredMlswgVectorNames(t) {
		want, listed := manifest[name]
		if !listed {
			t.Errorf("%s is not listed in VECTORS.sha256", name)
			continue
		}
		raw, err := os.ReadFile(filepath.Join(mlswgVectorDirectory, name))
		if err != nil {
			t.Errorf("read %s: %v", name, err)
			continue
		}
		digest := sha256.Sum256(raw)
		if got := hex.EncodeToString(digest[:]); got != want {
			t.Errorf("%s digest %s, VECTORS.sha256 says %s", name, got, want)
		}
	}
}

// normalisedLineEndings returns the bytes with every CRLF pair reduced to LF and every
// remaining bare carriage return removed. The caller asserts separately that there are no
// bare ones, so on this corpus the two rules coincide; both are written out because a
// helper that silently deletes bytes the caller has not reasoned about is how a
// normalisation becomes a hiding place.
func normalisedLineEndings(raw []byte) []byte {
	return bytes.ReplaceAll(bytes.ReplaceAll(raw, []byte{'\r', '\n'}, []byte{'\n'}), []byte{'\r'}, nil)
}
