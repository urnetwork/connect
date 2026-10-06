// The mechanical half of master section 7.2 and spec A section 5.9, guardrails 1 and 3.
// These walk the source of mls, message and messagegroup rather than grepping in continuous
// integration, so a developer sees the failure before pushing and so the rule travels
// with the code. The banned primitives share one property: for a low order point they
// hand back an all zero shared secret instead of an error, and a caller that logs it and
// continues then encrypts under a key its peer chose.
//
// A scanner that finds nothing because it is broken reports exactly what one that finds
// nothing because the code is clean reports, so nothing below rests on a scan having
// run. Three things hold that. The scan refuses a root it could not read or that held no
// go source, so an empty walk fails rather than issuing a clean bill. Every matcher is a
// function the gates and a positive control both call, and the control feeds it
// testdata/forbidden, a fixture committing every banned act, so a matcher that stopped
// matching fails there rather than passing everything quietly. And that fixture carries
// the other half of each case — the same calls in the file names allowed to make them,
// and every banned token in prose — so "not reported" is pinned to mean "allowed" rather
// than "not present".
//
// The matchers run on code with comments removed. These gates are about call sites and
// an import path, while the comment explaining why a primitive is banned is the comment
// a reader most wants: crypto_errors.go and the x25519 helper both name the banned sdk
// helper in their file comments for exactly that reason, and a gate that fires on the
// sentence teaching the rule is a gate the next contributor deletes. A commented out
// call is not a call, so nothing hides there that could also run. The stripping is line
// based, so a trailing comment on a line of code counts as code — deliberately, since
// telling a real trailing comment from a // inside a string literal needs a lexer, and
// over reporting is the safe direction for a ban list.
package mls

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The three package trees the guardrails cover, relative to this package's directory.
// connect itself is not among them: it is the parent, it may not import any of these
// packages, and its own legacy call sites are a separate migration.
//
// ../messagegroup joined on the commit that split connect/message in two. It is the client
// half -- the half that holds the group and may import mls -- and it is where the record
// layer's key schedule now lands, so a root that stopped at ../message would have left every
// file of that schedule outside the hkdf and .ECDH( confinements. That loss is NOT silent,
// which was measured rather than assumed: reverting this line to the two roots turns three
// tests of this package red, because TestNoEntropyTakingFunctionLivesWhereThisGateCannotCallIt
// resolves its rows against the declaring package and cryptoSourcePaths fatals on a root it
// read no production file of. The hkdf and .ECDH( confinements are the half that would go
// quiet. Five other gates alias this value rather than restating it, so they gained the root
// with it.
//
// This list is the SCOPE half of the rule GATES.md states: a gate that reads a set of
// directories has to derive that set and print the ones it did not read. It is the DIRECTORY
// instance recorded there -- named by the SHAPE it had and not by its position in a list whose
// length moves, because a citation that names a total goes stale the round after it is written,
// and this one has now gone stale twice: it named a total, was rewritten to name a different
// total in the same sentence that explains why totals go stale, and had to be rewritten again on
// the commit that recorded the ninth. The derivation asserting this list is directly below.
var forbiddenScanRoots = []string{".", "../message", "../messagegroup"}

// The directories this list must name, derived rather than read off it.
//
// R5, and it is the half this list was failing: the class these guardrails cover was a written
// list of three directories, so a FOURTH package of this module doing cryptography was invisible
// to every gate that aliases it -- an entropy taking function there, or a direct crypto/hkdf call,
// would have been scanned by nothing and reported clean by everything.
//
// The property, said as a property: a package is in scope when it does CRYPTOGRAPHY and is
// CONNECTED to the packages URmessage is built from. Both halves are read off the source. "Does
// cryptography" is an import of crypto, crypto/... or golang.org/x/crypto/... in production
// source, which is derived off the import path rather than off a list of primitive names -- the
// same reading the ban lists below use one layer down. "Connected" is the undirected component of
// this package in the module's own import graph, so a sibling that imports one of these, or that
// one of these imports, joins on the commit that adds the edge and not on the commit somebody
// remembers to widen a list.
//
// It is measured over the whole module and the answer today is exactly the three roots above.
// mls/syntax is in the component and is NOT in the class, because it imports no crypto at all --
// it is a codec -- and on the day it imports one it becomes a scan root here rather than a hole.
// The module's root package, blocker and extender all do cryptography and are NOT in the
// component: they are the legacy side this file's header already excludes, and the edge that would
// bring them in is the one connect/layering_test.go refuses.
//
// The list is kept as a LIST because twenty odd gates alias it and a value computed at init that
// came back short would narrow every one of them in silence, which is this tree's most expensive
// failure mode. So the class is derived and only the answer is written down, and the two are
// required to be equal in both directions: a root in the class and not in the list fails here, and
// so does a root in the list that the class does not contain.
func cryptographicUrmessageDirectories(t *testing.T) []string {
	t.Helper()
	const moduleRoot = ".."
	modulePath := ""
	goMod, err := os.ReadFile(filepath.Join(moduleRoot, "go.mod"))
	if err != nil {
		t.Fatalf("read the module's go.mod: %v -- this rule walks the module and cannot say where it starts", err)
	}
	for _, line := range strings.Split(string(goMod), "\n") {
		if after, found := strings.CutPrefix(strings.TrimSpace(line), "module "); found {
			modulePath = strings.TrimSpace(after)
			break
		}
	}
	if modulePath == "" {
		t.Fatal("go.mod declares no module path, so every in module import below would resolve to nothing and the component would be this package alone")
	}
	rootPath, err := filepath.Abs(moduleRoot)
	if err != nil {
		t.Fatalf("resolve the module root: %v", err)
	}
	ownPath, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolve this package: %v", err)
	}
	ownKey, err := filepath.Rel(rootPath, ownPath)
	if err != nil {
		t.Fatalf("place this package inside the module: %v", err)
	}
	ownKey = filepath.ToSlash(ownKey)

	holdsSource := map[string]bool{}
	doesCrypto := map[string]bool{}
	edges := map[string][]string{}
	fileSet := token.NewFileSet()
	frontier := []string{moduleRoot}
	for 0 < len(frontier) {
		dir := frontier[len(frontier)-1]
		frontier = frontier[:len(frontier)-1]
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		relative, err := filepath.Rel(moduleRoot, dir)
		if err != nil {
			t.Fatalf("place %s inside the module: %v", dir, err)
		}
		key := filepath.ToSlash(relative)
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() {
				// testdata is skipped for the reason forbiddenControlRoot exists: the
				// fixtures under it commit every banned act on purpose, and they are
				// unbuildable by the go tool, so they are not packages of this module.
				if strings.HasPrefix(name, ".") || name == "testdata" || name == "vendor" {
					continue
				}
				frontier = append(frontier, filepath.Join(dir, name))
				continue
			}
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			parsed, err := parser.ParseFile(fileSet, filepath.Join(dir, name), nil,
				parser.ImportsOnly|parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse %s: %v", filepath.Join(dir, name), err)
			}
			holdsSource[key] = true
			for _, spec := range parsed.Imports {
				imported := strings.Trim(spec.Path.Value, `"`)
				if imported == "crypto" || strings.HasPrefix(imported, "crypto/") ||
					strings.HasPrefix(imported, "golang.org/x/crypto/") {
					doesCrypto[key] = true
				}
				if sibling, found := strings.CutPrefix(imported, modulePath); found {
					sibling = strings.TrimPrefix(sibling, "/")
					if sibling == "" {
						sibling = "."
					}
					edges[key] = append(edges[key], sibling)
					edges[sibling] = append(edges[sibling], key)
				}
			}
		}
	}
	// three floors, because a walk that read nothing answers what a clean module answers
	if !holdsSource["."] {
		t.Fatal("the walk found no production source in the module's own root package, so it did not reach the top of the module and the component below is whatever it happened to see")
	}
	if !holdsSource[ownKey] {
		t.Fatalf("the walk found no production source in %s, which is the package it is running in", ownKey)
	}
	if !doesCrypto[ownKey] {
		t.Fatalf("%s reads as importing no crypto package at all, so the class this rule derives cannot contain the package whose guardrails it is checking", ownKey)
	}

	component := map[string]bool{ownKey: true}
	reach := []string{ownKey}
	for 0 < len(reach) {
		at := reach[len(reach)-1]
		reach = reach[:len(reach)-1]
		for _, next := range edges[at] {
			if component[next] || !holdsSource[next] {
				continue
			}
			component[next] = true
			reach = append(reach, next)
		}
	}
	class := []string{}
	outside := []string{}
	for key := range component {
		if doesCrypto[key] {
			class = append(class, key)
			continue
		}
		outside = append(outside, key)
	}
	slices.Sort(class)
	slices.Sort(outside)
	if len(class) < 2 {
		t.Fatalf("this reading finds %v and nothing else, so it has stopped seeing the module's import graph: connect/message and connect/messagegroup are two packages this one is connected to and both do cryptography", class)
	}
	t.Logf("%d packages walked, component %d, cryptographic %v, connected and not cryptographic %v",
		len(holdsSource), len(component), class, outside)
	return class
}

// TestTheScanRootsAreEveryCryptographicPackageConnectedToThisOne is R5 over this file's own scope.
//
// A gate that derives its class and then walks an enumerated scope is not a derived gate, and that
// is what the three roots above were until this case existed. What is asserted is equality in both
// directions with a class read off the module: a fourth package of this module that does
// cryptography and shares an import edge with these fails here on the commit that adds it, and a
// root left in the list after its package stopped qualifying fails here too.
func TestTheScanRootsAreEveryCryptographicPackageConnectedToThisOne(t *testing.T) {
	derived := cryptographicUrmessageDirectories(t)

	ownPath, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolve this package: %v", err)
	}
	rootPath, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolve the module root: %v", err)
	}
	ownKey, err := filepath.Rel(rootPath, ownPath)
	if err != nil {
		t.Fatalf("place this package inside the module: %v", err)
	}
	declared := []string{}
	for _, root := range forbiddenScanRoots {
		declared = append(declared, filepath.ToSlash(filepath.Clean(filepath.Join(filepath.ToSlash(ownKey), root))))
	}
	slices.Sort(declared)
	if !slices.Equal(derived, declared) {
		t.Errorf("forbiddenScanRoots names %v and the cryptographic packages this one is connected to are %v. Every gate in this package that aliases that list reads exactly the directories it names, so a package in the class and not in the list is a package no guardrail in this tree scans: a direct crypto/hkdf call there is guardrail 1 gone, and an entropy taking function there is the nil source substitution p5 shipped twice",
			declared, derived)
	}
}

// The fixture tree the positive controls scan. It sits under testdata on purpose, which
// is what makes it unreachable from the roots above and unbuildable by the go tool.
const forbiddenControlRoot = "testdata/forbidden"

// Primitives that must not appear anywhere in either package. The first three return an
// all zero secret for a low order point; the fourth is the import that supplies the
// second, banned outright so the package cannot enter the graph at all.
var forbiddenPrimitiveTokens = []string{
	"GenerateSharedSecret",
	"box.Precompute",
	"curve25519.ScalarMult",
	"golang.org/x/crypto/nacl/box",
}

// Guardrail 1. crypto/hkdf.Extract takes the input keying material first and the salt
// second, the reverse of the HKDF-Extract(salt, ikm) every spec text in this project
// writes, so every wrapper here swaps. Confining the call keeps the swap in two
// reviewable files instead of scattering a silent argument transposition.
//
// The needle this sentence used to name was a const holding that one spelling.
// hkdfEntryPointNeedles below derives the whole class off crypto/hkdf instead, because the
// same transposition is available through hkdf.Expand and through hkdf.Key, and neither of
// those was confined by anything.

// The two files that may make the call, as PATHS relative to this package's directory --
// which is the key scanSources collects a file under -- and not as base names.
//
// A base name is the exemption shape this project keeps rediscovering, and here it is load
// bearing: this gate is the only thing in the tree that catches a direct hkdf.Extract, so
// its exemption is the whole of guardrail 1's confinement. Read off the base name, every
// crypto.go and every hpke.go anywhere under forbiddenScanRoots inherited the excuse -- a
// subpackage's, a subdirectory's -- and moving a confined call into one of them was
// invisible. TestHkdfConfinementFlagsTheControlFixture builds one nested twin per entry
// here and requires each to be reported, so a path added without its twin fails rather
// than arriving uncontrolled.
var hkdfExtractAllowedPaths = []string{"crypto.go", "hpke.go"}

// Guardrail 3. One helper turns an x25519 failure into ErrInvalidPoint, so there is
// exactly one place that could ignore it and that place is reviewed.
const ecdhNeedle = ".ECDH("

// The one file that may make the call, as a path for the reason above.
var ecdhAllowedPaths = []string{"crypto_x25519.go"}

// This file has to quote every token and every assignment shape it bans, so it is the
// one file no matcher may run against. The exemption is by exact scanned path -- not by
// base name, which would excuse a crypto_forbidden_test.go in any subdirectory of either
// root -- and the count of files taking it is asserted, so a second file cannot quietly
// join it and become a place to hide a real call.
const forbiddenSelfPath = "crypto_forbidden_test.go"

// The control fixture's subdirectory, holding a twin of each allowed path: the same base
// name, one directory deeper. Under a base name reading every twin is exempt and the
// controls below report only violations.go; under a path reading every twin is a
// violation, which is what those controls demand.
const forbiddenNestedControlDirectory = "nested"

// One walk's result: the text of every go file found, keyed by slash separated path,
// and how many files each root contributed. The per root count is what separates "the
// roots are clean" from "the roots were never read".
type forbiddenScan struct {
	sourceTexts    map[string]string
	rootFileCounts map[string]int
}

// Walks each root and collects go source. Vendored corpora and the interop harness are
// skipped by directory name, except where a root names one outright, which is how the
// controls reach their fixture and nothing else reaches it.
//
// A root that cannot be walked and a root that yielded no go file are both errors,
// because either one produces a scan that reports every gate clean without having read
// the code. Returning the error rather than failing a test is what lets that refusal be
// tested directly instead of asserted about.
func scanSources(roots []string) (forbiddenScan, error) {
	scan := forbiddenScan{
		sourceTexts:    map[string]string{},
		rootFileCounts: map[string]int{},
	}
	if len(roots) == 0 {
		return scan, fmt.Errorf("no roots to scan")
	}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if path != root && (entry.Name() == "testdata" || entry.Name() == "interop") {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			scan.sourceTexts[filepath.ToSlash(path)] = string(body)
			scan.rootFileCounts[root]++
			return nil
		})
		if err != nil {
			return scan, fmt.Errorf("walk %s: %w", root, err)
		}
		if scan.rootFileCounts[root] == 0 {
			return scan, fmt.Errorf("walk %s read no go files; the scan is broken, not the source", root)
		}
	}
	return scan, nil
}

// The scan every gate starts from, with a failed walk fatal rather than reported: each
// assertion downstream is meaningless if the source was never read.
func mustScanSources(t *testing.T, roots []string) forbiddenScan {
	t.Helper()
	scan, err := scanSources(roots)
	if err != nil {
		t.Fatalf("scanning %v: %v", roots, err)
	}
	return scan
}

// Every scanned file except this one, with the exemption counted so it stays at one.
func sourcesUnderGate(t *testing.T, scan forbiddenScan) map[string]string {
	t.Helper()
	gated := map[string]string{}
	exempt := 0
	for path, text := range scan.sourceTexts {
		if path == forbiddenSelfPath {
			exempt++
			continue
		}
		gated[path] = text
	}
	if exempt != 1 {
		t.Errorf("%d scanned files carry the self exemption, want exactly 1 at %s", exempt, forbiddenSelfPath)
	}
	return gated
}

// The non test half of a scan. The confinement rules are about what ships: a test that
// calls a primitive to assert something about it is not a second call site in the code
// an auditor reads.
func productionSources(sourceTexts map[string]string) map[string]string {
	production := map[string]string{}
	for path, text := range sourceTexts {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		production[path] = text
	}
	return production
}

// One file's text with comments blanked out, line positions preserved so the line based
// matcher below still reports what a reader will find. Blanking rather than deleting
// keeps a stripped line from joining the two lines around it into a shape neither of
// them had.
//
// The line endings are normalised first, because every matcher downstream of this
// anchors on what a line holds, and a carriage return sits on the end of every one of
// them in a file something smudged. This repository has already paid for that once, with
// eighty four source anchors passing on windows because they matched nothing at all --
// and a matcher that stops matching is a gate that stops demanding.
//
// A CHECKOUT is no longer that something: `*.go text eol=lf` means git writes lf into
// every Go file here whatever core.autocrlf says. A TOOL writing crlf into the working
// tree still is, which is why this normalisation stays --
// TestThePackageSourceIsOneLineEndingThroughout catches that one, but only after a
// matcher has already read the file.
func codeOf(text string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	code := make([]string, 0, len(lines))
	inBlock := false
	for _, line := range lines {
		if inBlock {
			_, afterClose, closed := strings.Cut(line, "*/")
			if !closed {
				code = append(code, "")
				continue
			}
			inBlock = false
			line = afterClose
		}
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			code = append(code, "")
			continue
		}
		if beforeOpen, afterOpen, opened := strings.Cut(line, "/*"); opened {
			if _, tail, closed := strings.Cut(afterOpen, "*/"); closed {
				line = beforeOpen + tail
			} else {
				inBlock = true
				line = beforeOpen
			}
		}
		code = append(code, line)
	}
	return strings.Join(code, "\n")
}

// Every banned token present in one file's code. The gate and its control both call
// this, so a change that makes it stop matching fails the control instead of passing
// every file in the tree.
func forbiddenTokensIn(text string, tokens []string) []string {
	found := []string{}
	for _, token := range tokens {
		if strings.Contains(codeOf(text), token) {
			found = append(found, token)
		}
	}
	return found
}

// hkdfEntryPointNeedles is guardrail 1's class: a call site needle for every exported
// function crypto/hkdf declares, read out of that package's own source under the toolchain
// this repository pins.
//
// It is derived because the enumeration it replaced held ONE name. The gate banned
// "hkdf.Extract(" outside two reviewed files while crypto/hkdf declares three entry points,
// and the other two carry the same trap: Expand and Key take the secret before the salt, the
// reverse of the KDF.Extract(salt, ikm) every spec text in this project writes. hkdf.Key is
// the worse of the two -- it is Extract and Expand in one call, so a transposition there
// produces a whole key schedule that is internally consistent, 32 bytes long, and wrong, and
// nothing but a vector from another implementation can see it. A file calling hkdf.Key
// compiled clean and passed every gate in this tree.
//
// That is standing rule 5 exactly: fourteen times on this project a hand written list has
// understated the class it named. Reading the class off crypto/hkdf means a fourth entry
// point added by a later go release is confined by the release that adds it.
//
// The needle keeps the trailing parenthesis the single name gate used, so a mention of
// hkdf.Extract in prose is not a call site. Comments are stripped ahead of the match anyway,
// which is belt and braces of the same claim.
func hkdfEntryPointNeedles(t *testing.T) []string {
	t.Helper()
	dir := filepath.Join(build.Default.GOROOT, "src", "crypto", "hkdf")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s, where the confined class is derived from: %v", dir, err)
	}
	fileSet := token.NewFileSet()
	names := []string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fileSet, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s of crypto/hkdf: %v", name, err)
		}
		for _, declaration := range file.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Recv != nil || !function.Name.IsExported() {
				continue
			}
			names = append(names, "hkdf."+function.Name.Name+"(")
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)
	// the derivation's own positive control, in both directions. Extract is the call this
	// package actually makes and the one the guardrail is named for, so a derivation that
	// stopped deriving reports an empty class and clears every file; and a class of one is
	// the enumeration this replaced, which is the state the gate was in when hkdf.Expand and
	// hkdf.Key were outside it.
	if !slices.Contains(names, "hkdf.Extract(") {
		t.Fatalf("the derivation read %v out of %s and hkdf.Extract is not among them, so this gate confines nothing", names, dir)
	}
	if len(names) < 2 {
		t.Fatalf("the derivation read %v out of %s; crypto/hkdf declares Extract, Expand and Key, and a class of one is the enumeration this gate replaced", names, dir)
	}
	return names
}

// The scanned paths whose code contains needle and whose PATH is not allowed, sorted so a
// failure reads the same twice and a control can compare an exact set.
//
// The comparison is against the whole key the scan collected the file under. Comparing
// base names is what this used to do, and it excused a file by what it was called rather
// than by where it is: one crypto.go is reviewed and confined, and every other crypto.go
// under either root inherited that review without anybody making a decision.
func confinementViolations(sourceTexts map[string]string, needle string, allowedPaths []string) []string {
	violations := []string{}
	for path, text := range sourceTexts {
		if !strings.Contains(codeOf(text), needle) {
			continue
		}
		if !slices.Contains(allowedPaths, path) {
			violations = append(violations, path)
		}
	}
	slices.Sort(violations)
	return violations
}

// One list of allowed paths, rewritten to the control fixture's copies of them, so the
// controls run the gate's own allowed list rather than a second transcription of it.
func underControlRoot(paths []string) []string {
	rooted := make([]string, 0, len(paths))
	for _, path := range paths {
		rooted = append(rooted, forbiddenControlRoot+"/"+path)
	}
	slices.Sort(rooted)
	return rooted
}

// The nested twin of each allowed path: the same base name, in a directory no allowed path
// names. Derived from the allowed list rather than written out, so an entry added there
// without a fixture here fails the control instead of going uncontrolled.
func nestedControlTwins(allowedPaths []string) []string {
	twins := make([]string, 0, len(allowedPaths))
	for _, path := range allowedPaths {
		twins = append(twins,
			forbiddenControlRoot+"/"+forbiddenNestedControlDirectory+"/"+filepath.Base(path))
	}
	slices.Sort(twins)
	return twins
}

// The lines of code that take an x25519 result and throw it, or its error, away. Both
// spellings of assignment count: secret, _ := priv.ECDH(pub) discards the error exactly
// as secret, _ = priv.ECDH(pub) does, and a line opening with an underscore discards the
// secret. The short declaration form is the one the plan text missed, and it is the one
// a contributor reaches for first.
func discardedEcdhLines(text string) []string {
	discarded := []string{}
	for _, line := range strings.Split(codeOf(text), "\n") {
		if !strings.Contains(line, ecdhNeedle) {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "_") ||
			strings.Contains(trimmed, ", _ = ") ||
			strings.Contains(trimmed, ", _ := ") {
			discarded = append(discarded, trimmed)
		}
	}
	return discarded
}

// The scanned paths, sorted, for a failure message that has to show what was read.
func scannedPaths(sourceTexts map[string]string) []string {
	paths := make([]string, 0, len(sourceTexts))
	for path := range sourceTexts {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	return paths
}

// One fixture file, missing being fatal rather than empty: an absent fixture would make
// every control assertion below trivially true, which is the failure this file exists to
// rule out.
func controlFile(t *testing.T, control forbiddenScan, name string) string {
	t.Helper()
	return controlFileAt(t, control, forbiddenControlRoot+"/"+name)
}

// The same, addressed by the scanned path, which is what the confinement controls hold
// because their expectations are built out of the gate's own allowed list.
func controlFileAt(t *testing.T, control forbiddenScan, path string) string {
	t.Helper()
	text, ok := control.sourceTexts[path]
	if !ok {
		t.Fatalf("control fixture %s is missing; the scan read %v", path, scannedPaths(control.sourceTexts))
	}
	return text
}

// The gate: no file in either package may name a banned primitive in code.
func TestForbiddenPrimitivesAreAbsent(t *testing.T) {
	scan := mustScanSources(t, forbiddenScanRoots)
	for path, text := range sourcesUnderGate(t, scan) {
		for _, token := range forbiddenTokensIn(text, forbiddenPrimitiveTokens) {
			t.Errorf("%s references the forbidden primitive %q", path, token)
		}
	}
}

// hkdfExtraCallSites is the reviewed call site of one entry point OUTSIDE the crypto's two
// KDF files, by the path the scan collects a file under.
//
// One entry today. connect/message derives the group's write and read keys as
// HKDF-Expand(storage_root, "write/v1"|"read/v1", 32) of master section 9.2, which is that
// package's own derivation and not one of mls's labelled expansions -- there is no KDFLabel
// to have a second implementation of. It is exempt from the confinement and it is NOT exempt
// from being read: the gate below refuses an entry here that does not make the call, so a
// path that stops calling it stops being excused.
//
// Expand is the entry point with no salt argument, so it does not carry the transposition
// this guardrail is named for. Extract and Key do, and neither has an entry here.
//
// A needle the derived class holds and this map does not name is allowed in the crypto's two
// KDF files and NOWHERE else, which is the safe default: a fourth entry point added by a
// later go release is confined the moment it exists rather than exempted by nobody having
// thought of it. hkdf.Key -- Extract and Expand in one call, and the worst of the three to
// transpose, because the whole schedule it produces is internally consistent and wrong -- is
// in exactly that position.
var hkdfExtraCallSites = map[string][]string{
	"hkdf.Expand(": {"../message/writeauth.go"},
}

// The paths one entry point may be called from: the crypto's two reviewed KDF files, plus
// whatever the map above reviews for that entry point specifically.
func hkdfAllowedPathsFor(needle string) []string {
	return slices.Concat(hkdfExtractAllowedPaths, hkdfExtraCallSites[needle])
}

// The gate on guardrail 1, over every entry point crypto/hkdf declares rather than over the
// one the const above used to name. The allowed names are read out of the list the check
// itself uses, so a message cannot outlive the rule it describes.
//
// Each exemption outside the two KDF files is required to COVER something -- the path has to
// be scanned, and it has to make the call it is excused for. An allow list entry that names a
// file which no longer calls the function is a hole standing open for the next person to
// write one into, and it reads as coverage.
func TestHkdfExtractHasOnlyTwoCallSites(t *testing.T) {
	scan := mustScanSources(t, forbiddenScanRoots)
	sources := productionSources(sourcesUnderGate(t, scan))
	for _, needle := range hkdfEntryPointNeedles(t) {
		allowed := hkdfAllowedPathsFor(needle)
		for _, path := range hkdfExtraCallSites[needle] {
			text, scanned := sources[path]
			if !scanned {
				t.Errorf("%s is excused for %s and the scan of %v did not read it, so the exemption covers nothing",
					path, needle, forbiddenScanRoots)
				continue
			}
			if !strings.Contains(codeOf(text), needle) {
				t.Errorf("%s is excused for %s and does not call it; an exemption that covers nothing is a hole with a name on it",
					path, needle)
			}
		}
		for _, path := range confinementViolations(sources, needle, allowed) {
			t.Errorf("%s calls %s; only %s may", path, needle, strings.Join(allowed, " and "))
		}
	}
}

// The gate on guardrail 3, the call site half.
func TestEcdhHasOneCallSite(t *testing.T) {
	scan := mustScanSources(t, forbiddenScanRoots)
	sources := productionSources(sourcesUnderGate(t, scan))
	for _, path := range confinementViolations(sources, ecdhNeedle, ecdhAllowedPaths) {
		t.Errorf("%s calls %s; only %s may", path, ecdhNeedle, strings.Join(ecdhAllowedPaths, " and "))
	}
}

// The gate on guardrail 3, the ignored error half. Tests are in scope here, unlike the
// confinement gates: a test that shrugs off an x25519 error is a test that would pass on
// a broken refusal.
func TestEcdhResultIsNeverDiscarded(t *testing.T) {
	scan := mustScanSources(t, forbiddenScanRoots)
	for path, text := range sourcesUnderGate(t, scan) {
		for _, line := range discardedEcdhLines(text) {
			t.Errorf("%s discards an x25519 result: %s", path, line)
		}
	}
}

// The positive control for the token matcher, and the negative one beside it: the
// fixture that commits every banned act must yield every banned token, and the fixture
// that only writes about them must yield none. Without the second half, a matcher that
// answered yes to everything would pass the first.
func TestForbiddenTokenMatcherFlagsTheControlFixture(t *testing.T) {
	if len(forbiddenPrimitiveTokens) == 0 {
		t.Fatal("the banned token list is empty, so the gate has nothing to match")
	}
	control := mustScanSources(t, []string{forbiddenControlRoot})
	found := forbiddenTokensIn(controlFile(t, control, "violations.go"), forbiddenPrimitiveTokens)
	if !slices.Equal(found, forbiddenPrimitiveTokens) {
		t.Errorf("the matcher found %v in the control fixture, want all of %v", found, forbiddenPrimitiveTokens)
	}
	for _, name := range []string{"crypto.go", "hpke.go", "crypto_x25519.go", "documented.go"} {
		if found := forbiddenTokensIn(controlFile(t, control, name), forbiddenPrimitiveTokens); len(found) != 0 {
			t.Errorf("the matcher flagged %v in %s, which names them only in comments or not at all", found, name)
		}
	}
}

// The positive control for the guardrail 1 confinement, run through the allowed list the
// gate itself uses and over every entry point the derived class holds. Every fixture file is
// checked to contain each call before the report is compared, so an unreported file means the
// path was allowed rather than that the fixture forgot to make the call.
//
// Per NEEDLE and not once over the class, which is what makes the widening real: the fixture
// commits a call to each of crypto/hkdf's entry points in each of the five files, so a class
// that dropped back to Extract alone fails here rather than reporting a clean tree. A go
// release adding a fourth entry point fails this control -- "the fixture does not call it" --
// which is the intended detection rather than a false alarm: it is the sentence that says the
// new call is unconfined until somebody looks at it.
//
// The nested twins are the half that says the exemption is by path. Each is the base name of
// an allowed path in a directory no allowed path names, so a base name reading excuses every
// one of them and reports only violations.go -- which is exactly what this gate did before,
// and exactly what the expectation below refuses. The twins are derived from the allowed list
// rather than listed, so a third allowed path cannot land without one.
func TestHkdfConfinementFlagsTheControlFixture(t *testing.T) {
	control := mustScanSources(t, []string{forbiddenControlRoot})
	allowed := underControlRoot(hkdfExtractAllowedPaths)
	twins := nestedControlTwins(hkdfExtractAllowedPaths)
	if len(twins) != len(hkdfExtractAllowedPaths) || len(twins) == 0 {
		t.Fatalf("the allowed list holds %d paths and the control built %d twins",
			len(hkdfExtractAllowedPaths), len(twins))
	}
	violating := []string{forbiddenControlRoot + "/violations.go"}
	want := slices.Concat(violating, twins)
	slices.Sort(want)
	for _, needle := range hkdfEntryPointNeedles(t) {
		for _, path := range slices.Concat(violating, allowed, twins) {
			if !strings.Contains(codeOf(controlFileAt(t, control, path)), needle) {
				t.Fatalf("control fixture %s does not call %s, so it controls nothing for that entry point", path, needle)
			}
		}
		violations := confinementViolations(control.sourceTexts, needle, allowed)
		if !slices.Equal(violations, want) {
			t.Errorf("the confinement check reported %v for %s, want %v", violations, needle, want)
		}
	}
}

// The positive control for the guardrail 3 confinement, built the same way and with the
// same nested twin, because guardrail 3's exemption had the same shape as guardrail 1's.
func TestEcdhConfinementFlagsTheControlFixture(t *testing.T) {
	control := mustScanSources(t, []string{forbiddenControlRoot})
	allowed := underControlRoot(ecdhAllowedPaths)
	twins := nestedControlTwins(ecdhAllowedPaths)
	if len(twins) != len(ecdhAllowedPaths) || len(twins) == 0 {
		t.Fatalf("the allowed list holds %d paths and the control built %d twins",
			len(ecdhAllowedPaths), len(twins))
	}
	violating := []string{forbiddenControlRoot + "/violations.go"}
	for _, path := range slices.Concat(violating, allowed, twins) {
		if !strings.Contains(codeOf(controlFileAt(t, control, path)), ecdhNeedle) {
			t.Fatalf("control fixture %s does not call %s, so it controls nothing", path, ecdhNeedle)
		}
	}
	violations := confinementViolations(control.sourceTexts, ecdhNeedle, allowed)
	want := slices.Concat(violating, twins)
	slices.Sort(want)
	if !slices.Equal(violations, want) {
		t.Errorf("the confinement check reported %v, want %v", violations, want)
	}
}

// The positive control for the discard matcher. The expected set is exact, so a matcher
// that widened to flag every call site fails here as surely as one that stopped matching
// — the fixture's fourth x25519 call takes its error, and the comment beside it spells
// out a discarding line that must stay unreported.
func TestEcdhDiscardMatcherFlagsTheControlFixture(t *testing.T) {
	control := mustScanSources(t, []string{forbiddenControlRoot})
	discarded := discardedEcdhLines(controlFile(t, control, "violations.go"))
	want := []string{
		"_, _ = priv.ECDH(pub)",
		"_, err := priv.ECDH(pub)",
		"secret, _ := priv.ECDH(pub)",
		"shared, _ = priv.ECDH(pub)",
	}
	if !slices.Equal(discarded, want) {
		t.Errorf("the discard matcher reported %v, want %v", discarded, want)
	}
	for _, name := range []string{"crypto_x25519.go", "documented.go"} {
		if lines := discardedEcdhLines(controlFile(t, control, name)); len(lines) != 0 {
			t.Errorf("the discard matcher reported %v in %s, which discards nothing", lines, name)
		}
	}
}

// The coverage guarantee, exercised rather than assumed. A root that is not there and a
// root holding no go source both have to be refused: either one hands every gate above a
// clean result it did not earn. The fourth case is the one that actually bit — a second
// root that reads nothing while the first reads plenty, which a scan wide total would
// never notice.
func TestScanRefusesARootItCannotCover(t *testing.T) {
	uncoveredRootSets := [][]string{
		{},
		{"../this-package-does-not-exist"},
		{"testdata/vectors"},
		{".", "../this-package-does-not-exist"},
		{".", "testdata/vectors"},
	}
	for _, roots := range uncoveredRootSets {
		if _, err := scanSources(roots); err == nil {
			t.Errorf("scanning %v succeeded; a root that contributes no source must be refused", roots)
		}
	}
	// and the real roots must pass it, or the refusal above is just "everything fails"
	if _, err := scanSources(forbiddenScanRoots); err != nil {
		t.Errorf("scanning the real roots failed: %v", err)
	}
}

// What the gates actually read, reported rather than trusted. The bookkeeping check is
// the part the scan itself does not do: a per root count that no longer adds up to the
// collected set means files are being counted for a root that did not supply them.
func TestForbiddenScanCoversEveryRoot(t *testing.T) {
	scan := mustScanSources(t, forbiddenScanRoots)
	total := 0
	for _, root := range forbiddenScanRoots {
		t.Logf("root %s contributed %d go files", root, scan.rootFileCounts[root])
		total += scan.rootFileCounts[root]
	}
	if len(scan.sourceTexts) != total {
		t.Errorf("the scan holds %d files while the roots counted %d", len(scan.sourceTexts), total)
	}
	if len(scan.rootFileCounts) != len(forbiddenScanRoots) {
		t.Errorf("%d roots contributed files, want %d", len(scan.rootFileCounts), len(forbiddenScanRoots))
	}
}

// The fixture is a file full of real violations, so the gates must be unable to see it.
// If a directory named testdata ever stopped being skipped, the gates would fail on the
// control instead of on the code, which is loud but misleading; this names the reason.
func TestForbiddenScanSkipsTheControlFixture(t *testing.T) {
	scan := mustScanSources(t, forbiddenScanRoots)
	for _, path := range scannedPaths(scan.sourceTexts) {
		if strings.HasPrefix(path, "testdata/") || strings.Contains(path, "/testdata/") {
			t.Errorf("the gates read %s; vendored corpora and the control fixture must stay out of scope", path)
		}
	}
}
