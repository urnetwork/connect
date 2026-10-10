// The working tree's go source carries the line ending .gitattributes pins for it.
//
// Ported from mls/vectors_runner_test.go when messaging moved to github.com/urnetwork/message
// (MESSAGEREVIEW.md). That gate walked this module's root, so it held every go file of connect
// and not only mls's; the message repository keeps its own copy for its own tree, and this file
// keeps the half that stays here. The functions are that file's, with their assertions unchanged;
// what changed is the scope's anchor (lineEndingScanRoots), the file the pin's live check reads,
// the comments, and two points of CODESTYLE.md: the control tables name their fields, and the two
// helpers only the gate calls are closures inside it.
//
// core.autocrlf=true is set at system scope on the windows boxes that build this repo. A text
// gate or an exact-string edit anchored on one line ending matches nothing in a file carrying the
// other, and then reports success. git cannot show that drift, because autocrlf cleans on the way
// in, so the working tree is checked directly.
package connect

import (
	"bytes"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Refuses a working tree in which a go file does not carry the line ending .gitattributes pins
// for it.
//
// Every text gate and exact-string edit over this source is anchored on one ending and matches
// nothing in a file carrying the other: the edit does nothing, the suite passes, and that reads as
// "the change was made and was harmless". Since `*.go text eol=lf`, no checkout writes a crlf .go
// file, so one in the working tree was written by a tool.
//
// Each file is held to its own pin, read out of .gitattributes rather than written here, so the
// pin and this gate are one mechanism: deleting the pin leaves every file pinned to nothing and
// turns this red. A package whose files are pinned to two endings is reported, as is a file mixed
// within itself, which is never a checkout. A file with no line ending is counted apart, so an
// empty file cannot make a mixed package look uniform. git cannot see this drift: the clean filter
// converts on the way in, so an on-disk flip shows an empty `git diff --numstat`. Check the bytes.
//
// The scope is checked rather than trusted: the files read are asserted equal to a second,
// independent enumeration of the module's go files. That one assertion refuses a directory skipped
// by name, a depth limit and a walk rooted somewhere else. What it cannot refuse is the same
// narrowing written into both enumerations, which is why they share no helper.
func TestThePackageSourceIsOneLineEndingThroughout(t *testing.T) {
	// Every directory of this module holding go source, derived by walking the module root, as paths
	// relative to this package. A package added to the module is in scope without anyone adding it
	// here, and nothing is excluded: not testdata, and not a nested module, whose files a checkout
	// writes like any other. A directory that ever had to be left out would be a finding about this
	// repository, not an entry here.
	lineEndingScanRoots := func() []string {
		t.Helper()
		moduleRoot := moduleRootDir(t)
		here, err := filepath.Abs(".")
		if err != nil {
			t.Fatalf("resolve this package's own directory: %v", err)
		}
		roots, err := goSourceDirsUnder(moduleRoot, here)
		if err != nil {
			t.Fatalf("walk %s for the go source of this module: %v", moduleRoot, err)
		}
		if len(roots) == 0 {
			t.Fatal("the walk found no directory with go source under the module root, so the gate below judged nothing")
		}
		// where the walk starts is the one thing a module walk can still get wrong. mls checked it
		// against directories beside that package. this package is the module root, so a walk started
		// below the root cannot return the root itself, and one started above it is refused by the
		// judged-file assertion in the gate.
		if here != moduleRoot {
			t.Fatalf("this scope is anchored at the module root %s, but the gate runs in %s", moduleRoot, here)
		}
		if !slices.Contains(roots, ".") {
			t.Fatalf("the walk returned %v, which does not hold the module root's own package, so it is not rooted at this module", roots)
		}
		return roots
	}
	// Every go file at the top level of one package directory, sorted.
	//
	// It does not recurse, because lineEndingScanRoots already returns every directory holding go
	// source at any depth, so a nested directory arrives as a root of its own. Testdata go is source,
	// and is judged like any other. A directory holding no go source is fatal and not an empty
	// result, because a scope that resolved to nothing looks like every file agreeing.
	packageSourcePathsIn := func(dir string) []string {
		t.Helper()
		paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatalf("list the source of %s: %v", dir, err)
		}
		if len(paths) == 0 {
			t.Fatalf("%s holds no go files, so whatever scans it scanned nothing", dir)
		}
		slices.Sort(paths)
		return paths
	}
	moduleRoot := moduleRootDir(t)
	roots := lineEndingScanRoots()
	t.Logf("the derived scope is %v", roots)
	judged := []string{}
	for _, root := range roots {
		paths := packageSourcePathsIn(root)
		heldTo := map[string]int{}
		decidedBy := map[string]bool{}
		unpinned := []string{}
		exempted := []string{}
		wrong := []string{}
		empty := 0
		for _, path := range paths {
			source, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			// recorded where the file is actually read, and not where the scope was derived, so a
			// narrowing anywhere between the two -- in the walk, in the glob, or in a filter added to
			// this loop later -- is a member the assertion at the end no longer finds.
			judged = append(judged, repositoryPathOf(t, moduleRoot, path))
			lines := bytes.Count(source, []byte("\n"))
			carried := bytes.Count(source, []byte("\r\n"))
			carries := ""
			switch {
			case lines == 0:
				empty += 1
				continue
			case carried == lines:
				carries = "crlf"
			case carried == 0:
				carries = "lf"
			default:
				t.Errorf("%s holds %d lines of which %d end crlf, so the file is mixed within itself and no anchored edit over it can be trusted either way",
					path, lines, carried)
				continue
			}
			pinned, decided := pinnedLineEndingOf(t, moduleRoot, path)
			switch {
			case decided == "":
				unpinned = append(unpinned, path)
				continue
			case pinned == "":
				// decided, and what it decided is "no ending at all": a -text or binary rule covering a
				// .go file. That is a different answer from "no rule set mentions this file", and is
				// reported as itself; pinnedLineEndingOf tells the two apart through its second return.
				exempted = append(exempted, fmt.Sprintf("%s, by %q", path, decided))
				continue
			}
			// counted against what the repository says rather than against what the tree does, so
			// heldTo below is the pin and never a majority vote of the files.
			heldTo[pinned] += 1
			decidedBy[decided] = true
			if carries != pinned {
				wrong = append(wrong, fmt.Sprintf("%s is %s", path, carries))
			}
		}
		// no report short circuits another. Each of these is a different defect about a different
		// set of the root's files, and a `continue` under the first would let one -text rule over
		// one .go file leave every other file of its root unjudged.
		if len(unpinned) > 0 {
			t.Errorf("%d of %s's %d source files are pinned to no line ending by any .gitattributes between them and the module root (%s is one), so this gate is holding them to nothing; the pin and this gate are one mechanism and neither half is worth anything alone",
				len(unpinned), root, len(paths), unpinned[0])
		}
		if len(exempted) > 0 {
			t.Errorf("%d of %s's %d source files are marked as carrying no line ending AT ALL (%s), so this gate can hold them to nothing; a .go file git has been told not to convert is a hole in the pin rather than an exemption from it, and it is reported as the decision it is instead of as an absent rule",
				len(exempted), root, len(paths), exempted[0])
		}
		if len(heldTo) > 1 {
			t.Errorf("%s's source is pinned to more than one line ending (%v by %v), so no ending the package could be in is uniform and an edit anchored on either matches nothing in the files pinned to the other",
				root, slices.Sorted(maps.Keys(heldTo)), slices.Sorted(maps.Keys(decidedBy)))
		}
		if len(wrong) > 0 {
			// the pin is named rather than the majority ending, because the pin is the answer and a
			// majority is only a vote.
			t.Errorf("%s: %v, and %v checks every one of them out %v; a file the working tree carries in an ending no checkout of this repository produces was written by a tool, and an exact-string edit anchored on the other ending matches nothing in it and reports the change as made",
				root, wrong, slices.Sorted(maps.Keys(decidedBy)), slices.Sorted(maps.Keys(heldTo)))
		}
		if len(heldTo) == 0 && len(unpinned) == 0 && len(exempted) == 0 {
			t.Errorf("none of %s's source files carries a line ending at all (%d were empty), so this gate read nothing of that package", root, empty)
		}
		if len(heldTo) == 1 && len(wrong) == 0 {
			for ending, count := range heldTo {
				t.Logf("all %d source files of %s end their lines %s, which is what %v checks them out as, and %d carry no line ending",
					count, root, ending, slices.Sorted(maps.Keys(decidedBy)), empty)
			}
		}
	}

	// the scope, checked against the class it claims to cover: every .go file of this module,
	// enumerated a second time and by other means, has to be a file this gate just read.
	slices.Sort(judged)
	inModule := everyGoSourceFileUnder(t, moduleRoot, moduleRoot)
	if !slices.Equal(judged, inModule) {
		if unjudged := missingFrom(inModule, judged); len(unjudged) > 0 {
			t.Errorf("this gate judged %d of the %d go files under %s, and %s went unread; whatever those files carry, nothing in this suite has looked at it, and a scope that has narrowed away from the module it claims is green over everything it stopped reaching",
				len(judged), len(inModule), moduleRoot, namesOf(unjudged, 12))
		}
		if outside := missingFrom(judged, inModule); len(outside) > 0 {
			t.Errorf("this gate judged %s, which the independent enumeration of %s does not hold; a file judged twice, or judged from outside the module, means the two halves disagree about what the module IS and neither number below can be read",
				namesOf(outside, 12), moduleRoot)
		}
	}
	t.Logf("the gate judged %d files, against %d go files under %s", len(judged), len(inModule), moduleRoot)
}

// The directory this module's go.mod sits in, walked up to rather than written down.
func moduleRootDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolve this package's own directory: %v", err)
	}
	for range 8 {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("no go.mod above %s, so this module's root cannot be derived", dir)
	return ""
}

// Every directory at or below root that directly holds a .go file, as paths relative to from,
// sorted.
//
// A .go file is the evidence that its directory holds source, so the roots come out of one walk
// over files. Nothing is skipped: no directory name exempts the go source in it from an
// exact-string edit, which is all this scope is about. It is split out so the controls can run it
// against a tree built for them; a walk exercised only against the tree it ships in cannot be
// told apart from a list that happens to be right about that tree.
func goSourceDirsUnder(root string, from string) ([]string, error) {
	seen := map[string]bool{}
	dirs := []string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".go" {
			return nil
		}
		dir := filepath.Dir(path)
		if seen[dir] {
			return nil
		}
		seen[dir] = true
		relative, err := filepath.Rel(from, dir)
		if err != nil {
			return err
		}
		dirs = append(dirs, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.Sort(dirs)
	return dirs, nil
}

// Every .go file at or below root, as paths relative to from, sorted.
//
// The second half of the gate's judged-file assertion, sharing nothing with the walk it is
// compared against: goSourceDirsUnder asks about directories through filepath.WalkDir and the
// gate globs each answer, while this asks about files through an os.ReadDir recursion. A
// narrowing written into either shows up as a set the other no longer matches, which a helper
// shared by both would hide. There is no name test and no depth test, and nothing is pruned,
// .git included, so the two enumerations read the same tree.
func everyGoSourceFileUnder(t *testing.T, root string, from string) []string {
	t.Helper()
	files := []string{}
	pending := []string{root}
	for len(pending) > 0 {
		dir := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s while enumerating this module's go source: %v", dir, err)
		}
		for _, entry := range entries {
			path := filepath.Join(dir, entry.Name())
			if entry.IsDir() {
				pending = append(pending, path)
				continue
			}
			if filepath.Ext(entry.Name()) != ".go" {
				continue
			}
			relative, err := filepath.Rel(from, path)
			if err != nil {
				t.Fatalf("place %s under %s: %v", path, from, err)
			}
			files = append(files, filepath.ToSlash(relative))
		}
	}
	slices.Sort(files)
	return files
}

// One scanned file's pin across every rule set between it and the module root, nearest first,
// which is how git resolves an attribute: a nearer .gitattributes overrides the root's, and a
// further one answers only for what the nearer one says nothing about.
func pinnedLineEndingOf(t *testing.T, moduleRoot string, scanned string) (string, string) {
	t.Helper()
	segments := strings.Split(repositoryPathOf(t, moduleRoot, scanned), "/")
	for depth := len(segments) - 1; depth >= 0; depth-- {
		dir := filepath.Join(append([]string{moduleRoot}, segments[:depth]...)...)
		body, err := os.ReadFile(filepath.Join(dir, ".gitattributes"))
		if err != nil {
			continue
		}
		// a rule set's patterns are written against paths inside it, so protocol/.gitattributes would
		// say "*.go" about "frame.go" and never about "protocol/frame.go".
		if ending, decidedBy := pinnedLineEndingFor(string(body), strings.Join(segments[depth:], "/")); decidedBy != "" {
			return ending, decidedBy
		}
	}
	return "", ""
}

// The ending one .gitattributes checks one path out with ("lf", "crlf" or ""), and the line that
// decided it.
//
// The second value says whether the rule set had an opinion at all: a rule marking the path
// binary answers "" and has decided, while a rule set that never mentions the path answers "" and
// has not. Resolution is git's: rules in file order, last match wins; -text and the binary macro
// turn conversion off, so they clear an eol an earlier line asked for and decide; text with no eol
// leaves the ending to the checkout and decides nothing. Patterns are matched by
// gitAttributesPatternMatches rather than compared as strings, because *.go, /*.go and **/*.go are
// one rule to git.
func pinnedLineEndingFor(body string, filePath string) (string, string) {
	ending, decidedBy := "", ""
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		if !gitAttributesPatternMatches(fields[0], filePath) {
			continue
		}
		for _, attribute := range fields[1:] {
			switch {
			case attribute == "-text" || attribute == "binary":
				// an opinion, and the opinion is "no ending at all". The line is reported for that
				// reason: pinnedLineEndingOf walks outward until a rule set has decided, and a -text
				// reporting no decision would let a further away eol= answer for a file git has been
				// told to leave alone.
				ending, decidedBy = "", strings.TrimSpace(line)
			case strings.HasPrefix(attribute, "eol="):
				ending, decidedBy = strings.TrimPrefix(attribute, "eol="), strings.TrimSpace(line)
			}
		}
	}
	return ending, decidedBy
}

// One scanned file as .gitattributes addresses it: relative to the module root, with forward
// slashes, and never the relative path a scan happened to open it by.
func repositoryPathOf(t *testing.T, moduleRoot string, scanned string) string {
	t.Helper()
	absolute, err := filepath.Abs(scanned)
	if err != nil {
		t.Fatalf("resolve %s: %v", scanned, err)
	}
	relative, err := filepath.Rel(moduleRoot, absolute)
	if err != nil {
		t.Fatalf("place %s under %s: %v", scanned, moduleRoot, err)
	}
	return filepath.ToSlash(relative)
}

// Whether a gitattributes pattern applies to a repository path, by gitignore's semantics, which
// gitattributes borrows: a pattern holding no slash matches the base name at any depth; any other
// pattern is anchored at the directory holding the .gitattributes file, and a leading slash only
// anchors it; *, ? and a character class match within one path component; ** stands for any
// number of components, including none. A prefix comparison would instead ask whether the
// pattern was spelled the way a test's author expected.
func gitAttributesPatternMatches(pattern string, filePath string) bool {
	pattern = strings.TrimSuffix(pattern, "/")
	if pattern == "" {
		return false
	}
	anchored := strings.HasPrefix(pattern, "/") || strings.Contains(strings.TrimPrefix(pattern, "/"), "/")
	pattern = strings.TrimPrefix(pattern, "/")
	segments := strings.Split(filePath, "/")
	if !anchored {
		for _, segment := range segments {
			if matched, err := path.Match(pattern, segment); err == nil && matched {
				return true
			}
		}
		return false
	}
	return gitAttributesSegmentsMatch(strings.Split(pattern, "/"), segments)
}

// The anchored half, component by component so that * cannot cross a separator and ** can.
func gitAttributesSegmentsMatch(patternSegments []string, pathSegments []string) bool {
	if len(patternSegments) == 0 {
		return len(pathSegments) == 0
	}
	if patternSegments[0] == "**" {
		// any number of components, including none, so every suffix of the remaining path is a
		// candidate.
		for skip := 0; skip <= len(pathSegments); skip++ {
			if gitAttributesSegmentsMatch(patternSegments[1:], pathSegments[skip:]) {
				return true
			}
		}
		return false
	}
	if len(pathSegments) == 0 {
		return false
	}
	matched, err := path.Match(patternSegments[0], pathSegments[0])
	if err != nil || !matched {
		return false
	}
	return gitAttributesSegmentsMatch(patternSegments[1:], pathSegments[1:])
}

// A difference printed at a readable length, saying how much it left out. A one-file
// difference is named in full, which is why the assertions compare sets and not counts.
func namesOf(paths []string, most int) string {
	if len(paths) <= most {
		return fmt.Sprintf("%v", paths)
	}
	return fmt.Sprintf("%v and %d more", paths[:most], len(paths)-most)
}

// The members of want that got does not hold, so a failure names what is absent rather
// than printing two lists for a reader to difference by eye.
func missingFrom(want []string, got []string) []string {
	absent := []string{}
	for _, name := range want {
		if !slices.Contains(got, name) {
			absent = append(absent, name)
		}
	}
	return absent
}

// Controls the scope walk against a tree built here, because this module's own answer cannot
// separate a walk that descends from a list that is right about this checkout.
//
// The walk descends to any depth; a directory holding no go source is not a root; no name is an
// exception, shown with the four the go tool skips (a leading dot, a leading underscore, testdata
// and vendor); and the roots come back relative to where the caller stands. The fixture
// demonstrates that no name is skipped; the judged-file assertion in the gate is what proves it
// for names this fixture does not hold.
func TestTheLineEndingScopeIsEveryDirectoryHoldingGoSource(t *testing.T) {
	root := t.TempDir()
	for _, built := range []string{
		"top.go",
		"nested/mid.go",
		"nested/deeper/leaf.go",
		"nested/deeper/notes.md",
		"prose/readme.md",
		"prose/deeper/readme.md",
		".dotted/hidden.go",
		"_underscored/skipped.go",
		"testdata/fixture.go",
		"vendor/vendored.go",
	} {
		path := filepath.Join(root, filepath.FromSlash(built))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("build the scope fixture: %v", err)
		}
		if err := os.WriteFile(path, []byte("package p\n"), 0o644); err != nil {
			t.Fatalf("build the scope fixture: %v", err)
		}
	}
	dirs, err := goSourceDirsUnder(root, root)
	if err != nil {
		t.Fatalf("walk the scope fixture: %v", err)
	}
	if want := []string{".", ".dotted", "_underscored", "nested", "nested/deeper", "testdata", "vendor"}; !slices.Equal(dirs, want) {
		t.Errorf("the walk answered %v, want %v: every directory holding go source at any depth under any name, and no directory holding none", dirs, want)
	}

	from := filepath.Join(root, "nested")
	dirs, err = goSourceDirsUnder(root, from)
	if err != nil {
		t.Fatalf("walk the scope fixture from inside it: %v", err)
	}
	if want := []string{".", "..", "../.dotted", "../_underscored", "../testdata", "../vendor", "deeper"}; !slices.Equal(dirs, want) {
		t.Errorf("standing in %s the walk answered %v, want %v: a directory beside the caller is named by the step up to it and not by its absolute path", from, dirs, want)
	}
}

// Controls the other half of the judged-file assertion against a tree built here, four levels deep
// so that a depth limit shows.
//
// The enumeration names files, not directories, so the gate can compare it member by member; it
// descends to any depth; it excepts no directory name; and a file that is not go source is not a
// member.
func TestTheIndependentGoSourceEnumerationNamesEveryGoFileAtAnyDepth(t *testing.T) {
	root := t.TempDir()
	for _, built := range []string{
		"top.go",
		"notes.md",
		"nested/mid.go",
		"nested/deeper/leaf.go",
		"nested/deeper/further/deepest.go",
		"nested/deeper/further/readme.md",
		"prose/readme.md",
		".dotted/hidden.go",
		"_underscored/skipped.go",
		"testdata/fixture.go",
		"testdata/corpus/seed",
		"vendor/vendored.go",
		"oddly.named/inside.go",
	} {
		path := filepath.Join(root, filepath.FromSlash(built))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("build the enumeration fixture: %v", err)
		}
		if err := os.WriteFile(path, []byte("package p\n"), 0o644); err != nil {
			t.Fatalf("build the enumeration fixture: %v", err)
		}
	}

	files := everyGoSourceFileUnder(t, root, root)
	want := []string{
		".dotted/hidden.go",
		"_underscored/skipped.go",
		"nested/deeper/further/deepest.go",
		"nested/deeper/leaf.go",
		"nested/mid.go",
		"oddly.named/inside.go",
		"testdata/fixture.go",
		"top.go",
		"vendor/vendored.go",
	}
	if !slices.Equal(files, want) {
		t.Errorf("the enumeration answered %v, want %v: every go file at any depth under any directory name, and nothing that is not go source",
			files, want)
	}

	// and it answers relative to where the caller stands, because the gate compares it against
	// paths it resolved against the module root and a set of absolute paths matches none of them.
	fromNested := everyGoSourceFileUnder(t, root, filepath.Join(root, "nested"))
	if !slices.Contains(fromNested, "../top.go") || !slices.Contains(fromNested, "deeper/leaf.go") {
		t.Errorf("standing in nested the enumeration answered %v, want a file above the caller named by the step up to it and one below it named without one", fromNested)
	}
}

// Controls the pin derivation the gate rests on, in both directions: a reader answering lf for
// everything would pass whatever .gitattributes said, and one answering "" would fail a correctly
// pinned repository.
//
// The table is written against rule sets spelled out here, so it keeps its meaning when the live
// file changes; its paths are the ones it was written against in mls and are only strings here.
// The nesting is exercised against a tree built for it, because this repository has no nested
// rule set covering go source. The live file is then asked one question of its own, because
// everything above would pass against a repository that had stopped pinning anything.
func TestTheLineEndingPinIsReadTheWayGitResolvesIt(t *testing.T) {
	const pin = "*.go text eol=lf"
	for _, probe := range []struct {
		body    string
		path    string
		want    string
		decides bool
		why     string
	}{
		{body: pin, path: "mls/group.go", want: "lf", decides: true, why: "the rule this repository carries, on a path it covers"},
		{body: pin, path: "protocol/message.proto", want: "", decides: false, why: "and one it does not"},
		{body: "", path: "mls/group.go", want: "", decides: false, why: "no rule at all is no pin, which is what deleting the line looks like"},
		{body: "# " + pin, path: "mls/group.go", want: "", decides: false, why: "a commented out rule is not a rule"},
		{body: "/*.go text eol=lf", path: "group.go", want: "lf", decides: true, why: "the same rule anchored at the root"},
		{body: "**/*.go text eol=lf", path: "mls/group.go", want: "lf", decides: true, why: "and spelled with a leading globstar"},
		{body: pin + "\n*.go text eol=crlf", path: "mls/group.go", want: "crlf", decides: true, why: "the last matching line wins, which is git's resolution and not a preference"},
		{body: pin + "\nmls/** -text", path: "mls/group.go", want: "", decides: true, why: "-text turns conversion off and clears the eol an earlier line asked for -- and DECIDES, so no outer rule set answers for it"},
		{body: pin + "\nmls/** binary", path: "mls/group.go", want: "", decides: true, why: "and binary is git's macro for the same thing"},
		{body: "*.go text", path: "mls/group.go", want: "", decides: false, why: "text with no eol says the file is text, not which ending a checkout writes, so an outer rule set still answers"},
	} {
		ending, decidedBy := pinnedLineEndingFor(probe.body, probe.path)
		if ending != probe.want || (decidedBy != "") != probe.decides {
			t.Errorf("%q against %q answered %q decided-by %q, want %q decided %v: %s",
				probe.body, probe.path, ending, decidedBy, probe.want, probe.decides, probe.why)
		}
	}

	// the nesting, against a tree built for it. Nearest rule set with an opinion wins; a nearer one
	// with no opinion about this path defers outward.
	root := t.TempDir()
	nested := filepath.Join(root, "pkg")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("build the nesting fixture: %v", err)
	}
	source := filepath.Join(nested, "x.go")
	if err := os.WriteFile(source, []byte("package pkg\n"), 0o644); err != nil {
		t.Fatalf("build the nesting fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".gitattributes"), []byte(pin+"\n"), 0o644); err != nil {
		t.Fatalf("build the nesting fixture: %v", err)
	}
	if ending, _ := pinnedLineEndingOf(t, root, source); ending != "lf" {
		t.Errorf("with only a module root rule set the walk answered %q, want lf", ending)
	}
	for _, nearer := range []struct {
		body string
		want string
		why  string
	}{
		{body: "*.go text eol=crlf\n", want: "crlf", why: "a nearer rule set with an opinion overrides the module root's"},
		{body: "*.json -text\n", want: "lf", why: "a nearer rule set saying nothing about this path defers outward"},
		{body: "*.go -text\n", want: "", why: "a nearer rule set marking it binary decides, and the root's eol does not answer for it"},
	} {
		if err := os.WriteFile(filepath.Join(nested, ".gitattributes"), []byte(nearer.body), 0o644); err != nil {
			t.Fatalf("build the nesting fixture: %v", err)
		}
		if ending, _ := pinnedLineEndingOf(t, root, source); ending != nearer.want {
			t.Errorf("with %q nearer the file the walk answered %q, want %q: %s", nearer.body, ending, nearer.want, nearer.why)
		}
	}

	ending, decidedBy := pinnedLineEndingOf(t, moduleRootDir(t), "line_ending_test.go")
	if ending == "" {
		t.Fatal("no .gitattributes from this package up to the module root pins a line ending for this file, so the gate below has nothing to hold the working tree to")
	}
	t.Logf("the live rule set checks this file out %s, by %q", ending, decidedBy)
}

// Controls the matcher in both directions: one returning true for everything would report every
// path pinned by whichever line came last, and one returning false would fail a correctly
// configured repository. The probe paths are the ones this control was written against in mls;
// they are strings to the matcher.
func TestTheGitAttributesPatternMatcherAnswersGitsQuestion(t *testing.T) {
	const seed = "mls/testdata/corpus/FuzzGroupContextRoundTrip/seed001"
	for _, probe := range []struct {
		pattern string
		path    string
		matches bool
		why     string
	}{
		{pattern: "mls/testdata/corpus/**", path: seed, matches: true, why: "the spelling this repository uses"},
		{pattern: "/mls/testdata/corpus/**", path: seed, matches: true, why: "the same rule anchored at the root, which is what the old prefix comparison rejected"},
		{pattern: "**/corpus/**", path: seed, matches: true, why: "a leading ** skips any number of components"},
		{pattern: "mls/testdata/corpus/*", path: seed, matches: false, why: "a single star does not cross a separator, so this rule reaches the folders and not the seeds"},
		{pattern: "mls/testdata/corpus/", path: seed, matches: false, why: "a directory pattern does not recursively cover the paths inside it"},
		{pattern: "message/testdata/corpus/**", path: seed, matches: false, why: "another package's corpus"},
		{pattern: "seed001", path: seed, matches: true, why: "a pattern with no slash matches the base name at any depth"},
		{pattern: "seed001", path: "mls/testdata/corpus/FuzzGroupContextRoundTrip/seed002", matches: false, why: "and only that base name"},
		{pattern: "*.proto", path: "protocol/message.proto", matches: true, why: "the repository's other rule, on a path it covers"},
		{pattern: "*.proto", path: "protocol/message.pb.go", matches: false, why: "and one it does not"},
	} {
		if matched := gitAttributesPatternMatches(probe.pattern, probe.path); matched != probe.matches {
			t.Errorf("%q against %q answered %v, want %v: %s", probe.pattern, probe.path, matched, probe.matches, probe.why)
		}
	}
}
