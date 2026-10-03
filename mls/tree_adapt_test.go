// The private adapters of tree_adapt.go, held to their own properties before the tasks that
// call them land.
//
// They are excused in packageDeclarationsAwaitingTheirFirstCaller until then, and that gate's
// own note is the reason this file exists: "a declaration excused here is still held by its own
// tests. That is the difference between no caller yet and the placeholder this gate is about,
// which has no tests either."
//
// What the shims claim is delegation -- the value the arithmetic returned, and its error
// collapsed to a bool -- so what is stated here is agreement with the arithmetic over a class
// covering both of its outcomes. A shim that answered true unconditionally is the failure this
// catches, and it is the one that matters: every call site the later tasks add turns false into
// ErrTreeMalformed, so a shim that never says false is a malformed tree that is never refused.
package mls

import (
	"errors"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect/mls/syntax"
)

// treeAdaptFile is the file whose declarations this file is answerable for.
const treeAdaptFile = "tree_adapt.go"

// boolShims is every (value, ok) shim, each paired with the arithmetic it delegates to,
// written so that the shim and the thing it wraps are called with the same argument and their
// answers compared rather than each being checked against a table.
//
// Comparing against the arithmetic rather than against expected values is the point: these
// shims exist to change an error into a bool and nothing else, so any disagreement at all --
// a different value, a false where the arithmetic succeeded, a true where it did not -- is the
// whole of what can be wrong with one.
var boolShims = map[string]struct {
	shim  func(NodeIndex) (uint32, bool)
	wraps func(NodeIndex) (uint32, error)
}{
	"leftOf": {
		shim:  func(x NodeIndex) (uint32, bool) { y, ok := leftOf(x); return uint32(y), ok },
		wraps: func(x NodeIndex) (uint32, error) { y, err := Left(x); return uint32(y), err },
	},
	"rightOf": {
		shim:  func(x NodeIndex) (uint32, bool) { y, ok := rightOf(x); return uint32(y), ok },
		wraps: func(x NodeIndex) (uint32, error) { y, err := Right(x); return uint32(y), err },
	},
	"leafIndexOf": {
		shim:  func(x NodeIndex) (uint32, bool) { i, ok := leafIndexOf(x); return uint32(i), ok },
		wraps: func(x NodeIndex) (uint32, error) { i, err := x.LeafIndex(); return uint32(i), err },
	},
}

// errorShims is the two that keep the error, paired with the arithmetic each delegates to
// once its own leaf count check has passed.
var errorShims = map[string]struct {
	shim  func(LeafCount) error
	wraps func(LeafCount) error
}{
	"rootOf": {
		shim:  func(n LeafCount) error { _, err := rootOf(n); return err },
		wraps: func(n LeafCount) error { _, err := Root(n); return err },
	},
	"directPathOf": {
		shim:  func(n LeafCount) error { _, err := directPathOf(0, n); return err },
		wraps: func(n LeafCount) error { _, err := DirectPath(0, n); return err },
	},
}

// TestEveryDeclarationOfTreeAdaptIsExercisedHere is the completeness half, derived from the
// file rather than from the tables.
//
// The tables above are a claim about a set, and a claim about a set is worth what its
// derivation is worth. A seventh helper added to tree_adapt.go by a later task would otherwise
// be excused as awaiting its first caller AND covered by nothing, which is exactly the
// placeholder the excuse table's own note says an excused declaration must not become.
func TestEveryDeclarationOfTreeAdaptIsExercisedHere(t *testing.T) {
	declared := packageLevelDeclarations(t, ".")
	fromFile := map[string]bool{}
	for name, file := range declared {
		if file == treeAdaptFile {
			fromFile[name] = true
		}
	}
	if !fromFile["marshalBytes"] {
		t.Fatalf("the scan did not find marshalBytes among the declarations of %s, which certainly declares it, so it is reading something other than that file",
			treeAdaptFile)
	}
	exercised := map[string]bool{"marshalBytes": true}
	for name := range boolShims {
		exercised[name] = true
	}
	for name := range errorShims {
		exercised[name] = true
	}
	if got, want := slices.Sorted(maps.Keys(fromFile)), slices.Sorted(maps.Keys(exercised)); !slices.Equal(got, want) {
		t.Fatalf("%s declares %v and this file exercises %v; a helper covered by nothing is the placeholder the excuse table forbids",
			treeAdaptFile, got, want)
	}
}

// TestEveryBoolShimAgreesWithTheArithmeticItWraps sweeps both outcomes of each shim over a
// node index range wide enough to hold leaves and parents at several levels.
//
// Both outcomes are counted and both are required. A shim answering true unconditionally
// agrees with the arithmetic on every index where the arithmetic succeeds, so a sweep that
// never reached a refusing index would pass over exactly the mutation this is here for.
func TestEveryBoolShimAgreesWithTheArithmeticItWraps(t *testing.T) {
	for _, name := range slices.Sorted(maps.Keys(boolShims)) {
		shim := boolShims[name]
		agreed, refused := 0, 0
		for x := range NodeIndex(64) {
			gotValue, gotOk := shim.shim(x)
			wantValue, wantErr := shim.wraps(x)
			if gotOk != (wantErr == nil) {
				t.Errorf("%s(%d) reported ok = %v and the arithmetic answered err = %v",
					name, x, gotOk, wantErr)
				continue
			}
			if gotOk && gotValue != wantValue {
				t.Errorf("%s(%d) = %d and the arithmetic answered %d", name, x, gotValue, wantValue)
				continue
			}
			agreed++
			if !gotOk {
				refused++
			}
		}
		if agreed == 0 {
			t.Errorf("%s agreed with the arithmetic on nothing, so this sweep read no index at all", name)
		}
		if refused == 0 {
			t.Errorf("%s never answered false over 64 node indices, so the outcome every call site turns into ErrTreeMalformed was never produced",
				name)
		}
		if refused == agreed {
			t.Errorf("%s answered false for every index, so the succeeding outcome was never produced", name)
		}
	}
}

// TestTheTwoErrorKeepingShimsRefuseAnEmptyTree states the one behaviour those two add over the
// arithmetic they wrap: a leaf count of zero is this package's malformed tree rather than
// whatever tree_math says about a width it was never meant to see.
//
// The check in front of each is not redundant with the arithmetic's own, and this is where
// that claim is made good: Root(0) is asked directly, and if it ever answers a node index
// rather than an error then the explicit zero check is the ONLY thing standing between an
// empty tree and node zero being treated as its root -- a whole tree hash computed over a leaf
// that is not there.
func TestTheTwoErrorKeepingShimsRefuseAnEmptyTree(t *testing.T) {
	for _, name := range slices.Sorted(maps.Keys(errorShims)) {
		shim := errorShims[name]
		if err := shim.shim(0); !errors.Is(err, ErrTreeMalformed) {
			t.Errorf("%s over a zero leaf count gave err = %v, want ErrTreeMalformed", name, err)
		}
		agreed := 0
		for n := LeafCount(1); n <= 64; n++ {
			got, want := shim.shim(n), shim.wraps(n)
			if (got == nil) != (want == nil) {
				t.Errorf("%s(%d) answered %v and the arithmetic answered %v", name, n, got, want)
				continue
			}
			if got != nil && !errors.Is(got, want) {
				t.Errorf("%s(%d) answered %v, which does not answer to the arithmetic's %v", name, n, got, want)
				continue
			}
			agreed++
		}
		if agreed == 0 {
			t.Errorf("%s agreed with the arithmetic on no leaf count at all", name)
		}
	}
	// the positive control on the paragraph above: if the arithmetic refused zero on its own,
	// the explicit check would be the redundancy it is claimed not to be, and this test would
	// be stating a property nothing depends on.
	if _, err := Root(0); err == nil {
		t.Error("Root(0) answers a node index rather than an error, which is what the explicit zero check in rootOf exists for; if that changed, say so in tree_adapt.go rather than here")
	}
}

// TestMarshalBytesYieldsTheWritersBytesAndSurfacesBothOfItsFailures states the three outcomes
// the preimage encoder has, because two of them are silent if they are wrong.
//
// The interesting one is the sticky failure. syntax.Writer's leaf writes return nothing and
// latch their error, so an encode function that writes an over long field returns nil and
// leaves a Writer that cannot produce bytes. A marshalBytes that returned w's buffer without
// consulting Bytes would hand back a TRUNCATED preimage with no error at all -- and a
// signature over a truncated preimage verifies, against the wrong content, at both ends.
func TestMarshalBytesYieldsTheWritersBytesAndSurfacesBothOfItsFailures(t *testing.T) {
	encoded, err := marshalBytes(func(w *syntax.Writer) error {
		w.WriteUint16(0xf00d)
		w.WriteOpaque([]byte{0xa1, 0xa2})
		return nil
	})
	if err != nil {
		t.Fatalf("marshalBytes: %v", err)
	}
	if want := []byte{0xf0, 0x0d, 0x02, 0xa1, 0xa2}; string(encoded) != string(want) {
		t.Errorf("marshalBytes = %x, want %x", encoded, want)
	}

	// a fresh Writer each time, which is the other half of what this helper is for: two
	// preimages assembled through it must not share a buffer.
	second, err := marshalBytes(func(w *syntax.Writer) error {
		w.WriteUint16(0x0001)
		return nil
	})
	if err != nil {
		t.Fatalf("marshalBytes: %v", err)
	}
	if string(second) != string([]byte{0x00, 0x01}) {
		t.Errorf("the second call returned %x, so the writer is not fresh per call", second)
	}

	// the encoder's own semantic refusal
	sentinel := errors.New("the encoder refused")
	if bs, err := marshalBytes(func(w *syntax.Writer) error {
		w.WriteUint16(0x0001)
		return sentinel
	}); !errors.Is(err, sentinel) || bs != nil {
		t.Errorf("an encoder that refused gave (%x, %v), want (nil, the refusal)", bs, err)
	}

	// the writer's sticky failure, which the encoder itself never sees
	bs, err := marshalBytes(func(w *syntax.Writer) error {
		w.WriteOpaque(make([]byte, syntax.MaxVectorLength+1))
		return nil
	})
	if err == nil {
		t.Errorf("an over long field produced %d bytes and no error; a truncated preimage is a signature over content nobody agrees with", len(bs))
	}
	if bs != nil {
		t.Errorf("an over long field produced %d bytes alongside its error", len(bs))
	}
}

// TestTreeAdaptCitesNoTestThatDoesNotExist holds this file's own prose and tree_adapt.go's to
// naming tests that are there.
//
// TestThisPlansFilesCiteNoTestThatDoesNotExist derives its file set from where this plan's
// error names are declared, which reaches tree_errors.go and not tree_adapt.go, so the two
// files this task adds would otherwise be the ones whose prose nothing reads.
func TestTreeAdaptCitesNoTestThatDoesNotExist(t *testing.T) {
	declared := packageLevelDeclarations(t, ".")
	cited := 0
	for _, file := range []string{treeAdaptFile, "extension.go", "tree_adapt_test.go", "extension_test.go"} {
		body, err := readSourceFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for _, name := range testNameCitation.FindAllString(commentLineWrap.ReplaceAllString(body, ""), -1) {
			cited++
			if _, found := declared[name]; !found {
				t.Errorf("%s names %s and package mls declares no test under that name, so the enforcement that sentence points at cannot be followed to anything",
					file, name)
			}
		}
	}
	if cited == 0 {
		t.Fatal("the four files of this task name no test at all, which cannot be true of them, so the scan read something other than them")
	}
	t.Logf("%d test citations resolved", cited)
}

// readSourceFile reads one file of this package as text, with its line endings normalised so a
// prose scan reads the same thing whatever last wrote the file. This used to say "on a checkout
// that stores CRLF as on one that does not"; `*.go text eol=lf` means no checkout of this
// repository stores CRLF in a Go file, and what is left to normalise against is a tool that
// rewrote the working tree.
func readSourceFile(name string) (string, error) {
	body, err := os.ReadFile(name)
	if err != nil {
		return "", err
	}
	return strings.ReplaceAll(string(body), "\r\n", "\n"), nil
}
