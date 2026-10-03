// The RFC 9180 base mode corpus this package is held to, and the loader that is the one
// way into it.
//
// The values were transcribed by hand into this package before this file existed, because
// tasks 5 to 8 had to verify themselves before there was a corpus to verify against. Two
// corpora that are meant to agree, with nothing asserting that they do, is how they drift,
// so the transcriptions are gone and this file is what remains. The commit that added it
// carried a bridge test asserting the two agreed on all 94 published values they shared;
// that agreement is why the vendored file is trusted here, since a file fetched from a url
// proves nothing about itself beyond its own digest.
//
// The corpus is the whole published one rather than the six sequence numbers RFC 9180
// appendix A prints. That is the difference between the transcription and the file: the
// appendix is a readable excerpt and test-vectors.json is the corpus, so the seal known
// answer now walks 257 sequence numbers per suite where it used to walk six.
//
// Provenance is the part that has to be argued rather than asserted, and interop/PINS.md
// argues it. Both digests there were reproduced from the upstream commit before these
// bytes were written: the upstream file hashes to 61fc662f…, the filtered re-serialization
// hashes to the constant below, the two vendored entries deep-equal the two upstream
// entries the predicate selects, and re-running the transform reproduces these bytes. The
// filter exists because the upstream file is 5.9 MB and 128 entries, 126 of them for
// algorithms this implementation does not have.
//
// That argument is prose, and prose rots. Every value somebody re-vendoring this corpus
// would have to retype — the repository, the commit, the path, the upstream digest, the
// selection predicate, the serialization call — is therefore a constant here as well, and
// the pin file is parsed rather than grepped, so the two copies cannot drift apart in
// either direction. Before that, corrupting the provenance text left the package green:
// the upstream commit set to forty zeros, the machine-readable line deleted outright, the
// repository pointed at a fork nobody has heard of, 33 of 35 such mutations passed.
//
// Only two entries are vendored and that is a claim about the registry rather than about
// the RFC. RFC 9180 gives HKDF-SHA256 the kdf code point 0x0001 and AES-128-GCM the aead
// code point 0x0001 in two separate registries, so on suite 0x0001 those two positions
// hold the same byte and a transposition between them moves nothing anyone can observe. On
// suite 0x0003 the aead is 0x0003 and the same transposition moves every derived byte. A
// corpus carrying one suite is therefore a corpus that cannot see the mistake hpke.go's
// file comment is about, whichever suite it carries — which is why loadHpkeVectors refuses
// to return anything but one entry per registered suite.
package mls

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// One published encryption, as the upstream file carries it: the message a sender's
// context produces at one sequence number, with the nonce printed beside it.
//
// sequence is not a field of the file. Upstream indexes the encryptions array by sequence
// number and prints no counter, so the loader supplies it from the position and the nonce
// comparison is what holds that reading honest — an off by one between index and sequence
// moves the low byte of every nonce and fails at the first row.
type hpkeVectorEncryption struct {
	Aad   string `json:"aad"`
	Ct    string `json:"ct"`
	Nonce string `json:"nonce"`
	Pt    string `json:"pt"`

	sequence uint64
}

// One published exported value. The length travels with the value rather than being read
// off it, so a corpus that lost a byte fails as a disagreement about a published length
// instead of quietly asserting a shorter export.
type hpkeVectorExport struct {
	ExporterContext string `json:"exporter_context"`
	Length          int    `json:"L"`
	ExportedValue   string `json:"exported_value"`
}

// One base mode entry, with every field a labelled extract or expand, a key pair
// derivation, an encapsulation, a decapsulation, a seal or an export consumes or produces.
//
// name and suite are derived by the loader and are not in the file. The file identifies an
// entry by the kem, kdf and aead code points it was generated for, which is the only
// identification that can be checked against the registry rather than trusted; suite is
// the registry's answer to that triple and name is for the failure messages.
type hpkeVector struct {
	Mode               int                    `json:"mode"`
	KemId              HpkeKemId              `json:"kem_id"`
	KdfId              HpkeKdfId              `json:"kdf_id"`
	AeadId             HpkeAeadId             `json:"aead_id"`
	Info               string                 `json:"info"`
	IkmE               string                 `json:"ikmE"`
	IkmR               string                 `json:"ikmR"`
	SkEm               string                 `json:"skEm"`
	SkRm               string                 `json:"skRm"`
	PkEm               string                 `json:"pkEm"`
	PkRm               string                 `json:"pkRm"`
	Enc                string                 `json:"enc"`
	SharedSecret       string                 `json:"shared_secret"`
	KeyScheduleContext string                 `json:"key_schedule_context"`
	Secret             string                 `json:"secret"`
	Key                string                 `json:"key"`
	BaseNonce          string                 `json:"base_nonce"`
	ExporterSecret     string                 `json:"exporter_secret"`
	Encryptions        []hpkeVectorEncryption `json:"encryptions"`
	Exports            []hpkeVectorExport     `json:"exports"`

	name  string
	suite CipherSuite
}

const hpkeVectorPath = "testdata/vectors/rfc/hpke-rfc9180-x25519.json"

// The attributes file that keeps git's text conversion off the vendored bytes. It is named
// here because interop/PINS.md names it as the mechanism preventing the smudge this
// repository has already been bitten by, and a mechanism a pin file names is a mechanism
// something has to look at. Deleting it, or turning its rule around, was refused by nothing
// before TestHpkeVectorDirectoryDisablesGitsTextConversion existed: the smudge it prevents
// only lands on the next fresh checkout, long after the commit that removed the guard.
const hpkeVectorAttributesPath = "testdata/vectors/rfc/.gitattributes"

// The digest recorded in interop/PINS.md, where the provenance that makes it worth
// anything is written down. A vector file that changed under us must break the build
// rather than quietly weaken every known answer that reads it.
const hpkeVectorSha256 = "3cc5f951dea0b7dbe80419215e64c810498ee4dd76c376763bbe6860c346b11a"

// The upstream file this corpus was filtered out of, in exactly the four values somebody
// re-vendoring it would have to retype. The vendored digest above says the bytes have not
// moved since they were vendored; it says nothing about where they came from, and a corpus
// whose origin nobody can check is a corpus that verifies this implementation against
// itself.
//
// These duplicate interop/PINS.md on purpose. That file is the argument and this is the
// copy the compiler holds, and the test below compares them field by field, so re-vendoring
// at a newer commit has to move both or fail.
const (
	hpkeVectorUpstreamRepository = "cfrg/draft-irtf-cfrg-hpke"
	hpkeVectorUpstreamCommit     = "b1f7cb0cdeab6906c61b3d6574e8bdfdbe1cd3fb"
	hpkeVectorUpstreamPath       = "test-vectors.json"
	hpkeVectorUpstreamSha256     = "61fc662f01996cd06d713dacf5e133167bd309a1f329442d53f1e21a47b3ede6"
)

// The transform that turns the upstream file into this one, as the two strings that make it
// reproducible. Provenance for a re-serialization is not the fetch alone: a predicate that
// selected different entries, or a serializer called with different arguments, yields a
// different file from the same upstream commit, and then the recorded upstream digest
// verifies bytes nobody can regenerate.
const (
	hpkeVectorFilter        = "mode == 0 and kem_id == 32 and kdf_id == 1 and aead_id in (1, 3)"
	hpkeVectorSerialization = "json.dumps(out, indent=2, sort_keys=True)"
)

// The published sequence numbers the corpus has to reach. 255 and 256 are the load bearing
// pair: a counter written little endian, or written at the front of the nonce, agrees with
// a big endian one on every sequence number below 256 and disagrees at exactly the byte
// crossing. A corpus that stopped short of it would leave every nonce known answer passing
// against an implementation that matches nobody.
const hpkeVectorHighestSequence = 256

// The published exported values per entry. Named rather than written as a literal because
// interop/PINS.md states the same count in prose and the test below holds the prose to it.
const hpkeVectorExportCount = 3

// The corpus exactly as the file carries it: pinned by digest, parsed, and nothing else.
//
// This is split out of loadHpkeVectors so the shape of the corpus can be asserted by a test
// rather than only by the loader. A test that calls the loader sees only a corpus the loader
// has already accepted, so a count check written after a loader fatal on the same count is a
// check that cannot fail — three of the assertions in
// TestHpkeVectorCorpusIsTheWholePublishedCorpus were exactly that until this split.
func parseHpkeVectors(t testing.TB) []hpkeVector {
	t.Helper()
	raw, err := os.ReadFile(hpkeVectorPath)
	if err != nil {
		t.Fatalf("read %s: %v", hpkeVectorPath, err)
	}
	digest := sha256.Sum256(raw)
	if got := hex.EncodeToString(digest[:]); got != hpkeVectorSha256 {
		t.Fatalf("%s sha256 = %s, want %s (see interop/PINS.md)", hpkeVectorPath, got, hpkeVectorSha256)
	}
	var vectors []hpkeVector
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatalf("parse %s: %v", hpkeVectorPath, err)
	}
	return vectors
}

// The corpus, with every shape assertion the loops that consume it depend on already made.
//
// The assertions live here rather than in the tests because this is the only place that
// can make them once for all of them. A table that lost a row is not an empty table, and
// only the empty case was ever refused — so until the count was checked, dropping either
// entry left every known answer in the package green. The same is true of the encryptions
// and the exports: a loop over an empty slice reports success, so a corpus stripped to its
// key schedule would satisfy the seal and export known answers by never running them.
func loadHpkeVectors(t testing.TB) []hpkeVector {
	t.Helper()
	vectors := parseHpkeVectors(t)
	suites := Suites()
	if len(vectors) != len(suites) {
		t.Fatalf("%s holds %d vectors for %d registered suites, so a suite goes unpinned",
			hpkeVectorPath, len(vectors), len(suites))
	}
	for i := range vectors {
		params := suiteForHpkeVector(t, vectors[i])
		vectors[i].suite = params.Suite
		vectors[i].name = "rfc 9180 test-vectors.json, mode 0, " + params.Name
		for j := range vectors[i].Encryptions {
			vectors[i].Encryptions[j].sequence = uint64(j)
		}
	}
	slices.SortFunc(vectors, func(a hpkeVector, b hpkeVector) int {
		return int(a.suite) - int(b.suite)
	})
	for i, suite := range suites {
		if vectors[i].suite != suite {
			t.Fatalf("vector %d is for suite %#04x, want %#04x", i, uint16(vectors[i].suite), uint16(suite))
		}
		if got := len(vectors[i].Encryptions); got <= hpkeVectorHighestSequence {
			t.Fatalf("%s carries %d encryptions, so the sequence counter never reaches %d and never crosses a byte",
				vectors[i].name, got, hpkeVectorHighestSequence)
		}
		if len(vectors[i].Exports) == 0 {
			t.Fatalf("%s carries no exported values, so every export known answer would loop over nothing", vectors[i].name)
		}
	}
	return vectors
}

// The registered suite a vector was generated for, refusing anything else. A vector for a
// mode, kem, kdf or aead this package does not implement asserts nothing about it, and
// silently skipping such an entry is how a corpus ends up with two rows and one of them
// tested.
func suiteForHpkeVector(t testing.TB, vector hpkeVector) *SuiteParams {
	t.Helper()
	if vector.Mode != 0 {
		t.Fatalf("vector mode is %d, want 0 (base)", vector.Mode)
	}
	for _, suite := range Suites() {
		params, err := LookupSuite(suite)
		if err != nil {
			t.Fatalf("LookupSuite: %v", err)
		}
		if params.KemId == vector.KemId && params.KdfId == vector.KdfId && params.AeadId == vector.AeadId {
			return params
		}
	}
	t.Fatalf("no registered suite for kem %#04x kdf %#04x aead %#04x", vector.KemId, vector.KdfId, vector.AeadId)
	return nil
}

// How an entry names itself before the loader has named it, out of the three code points
// that are actually in the file. The loader's name is the registry's answer to those code
// points and is not available to anything reading the raw parse.
func hpkeVectorCodePoints(vector hpkeVector) string {
	return fmt.Sprintf("the entry for kem %#04x kdf %#04x aead %#04x", vector.KemId, vector.KdfId, vector.AeadId)
}

// TestHpkeVectorFileWasNotSmudgedOnTheWayIn is the provenance check the digest pin cannot
// make on its own. A digest says the file has not moved since the digest was taken; it says
// nothing about what the file was when that happened. This repository has already vendored a
// corpus whose sixteen files were all rewritten by git's autocrlf on the way in and whose
// manifest was then computed over the rewritten bytes, so it verified sixteen of sixteen
// against bytes upstream never published, and only a reviewer who fetched upstream and
// compared noticed.
//
// A carriage return is what that rewrite leaves behind. Upstream publishes this file with
// none anywhere in it, so one appearing here means the bytes were smudged between upstream
// and the index — and the count is reported rather than merely the presence, because a
// single stray one and a wholesale rewrite are different accidents.
//
// The name says what this does rather than what one would like it to do. Nothing in this
// package compares these bytes against upstream, because nothing in this package has a
// network; that comparison was a human act, recorded in interop/PINS.md and held to
// constants by TestHpkeVectorProvenanceIsRecordedInThePinFile. This is the smudge detector,
// and a name claiming it is the upstream comparison is how a reader ends up believing the
// fetch is automated.
func TestHpkeVectorFileWasNotSmudgedOnTheWayIn(t *testing.T) {
	raw, err := os.ReadFile(hpkeVectorPath)
	if err != nil {
		t.Fatalf("read %s: %v", hpkeVectorPath, err)
	}
	if len(raw) == 0 {
		t.Fatalf("%s is empty, so the count below is about nothing", hpkeVectorPath)
	}
	if n := bytes.Count(raw, []byte{'\r'}); n != 0 {
		t.Fatalf("%s carries %d carriage returns; upstream publishes it with none, so these bytes are not the bytes that were fetched",
			hpkeVectorPath, n)
	}
}

// TestHpkeVectorDirectoryDisablesGitsTextConversion checks the guard rather than the
// symptom. The test above notices a smudge that has already happened, in a working tree
// already checked out; this notices the rule going missing, on the commit that removes it,
// which is the only moment anybody can still act on it. core.autocrlf is true at system
// scope on at least one machine that writes here, so without the rule the next fresh clone
// rewrites the corpus and the digest gate fires as an unexplained mismatch a long way from
// its cause.
func TestHpkeVectorDirectoryDisablesGitsTextConversion(t *testing.T) {
	body, err := os.ReadFile(hpkeVectorAttributesPath)
	if err != nil {
		t.Fatalf("read %s: %v (it is what keeps git's text conversion off the corpus)", hpkeVectorAttributesPath, err)
	}
	covering := 0
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		// only a rule whose pattern reaches the corpus file says anything about it
		switch fields[0] {
		case "*", "*.json", filepath.Base(hpkeVectorPath):
		default:
			continue
		}
		covering++
		// binary is git's own macro for -diff -merge -text, so it says the same thing
		if !slices.Contains(fields[1:], "-text") && !slices.Contains(fields[1:], "binary") {
			t.Errorf("%s rule %q covers %s with neither -text nor binary, so git is free to convert its line endings",
				hpkeVectorAttributesPath, strings.TrimSpace(line), filepath.Base(hpkeVectorPath))
		}
	}
	if covering == 0 {
		t.Fatalf("%s carries no rule covering %s, so nothing keeps git out of the vendored bytes",
			hpkeVectorAttributesPath, filepath.Base(hpkeVectorPath))
	}
}

// The pin file, read once for the assertions that follow.
func hpkeVectorPinFile(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("interop", "PINS.md"))
	if err != nil {
		t.Fatalf("read interop/PINS.md: %v", err)
	}
	return string(body)
}

// The shape interop/PINS.md says its machine-readable lines keep, exactly.
var hpkeVectorPinLine = regexp.MustCompile(`^([a-z][a-z0-9]*)=([0-9a-f]{40})$`)

// The key=sha lines out of interop/PINS.md's one fenced block, parsed rather than grepped,
// with every line in the block held to the shape the file's own prose requires.
//
// The shape is asserted over the block rather than over a list of key names, and that is
// the point of parsing it. The list that existed before was []string{"mlswg=", "openmls="},
// written when there were two keys; a third was added by this task and nobody extended it,
// so the new key was held to nothing at all. A block that enumerates itself cannot go stale
// that way.
func hpkeVectorMachineReadablePins(t *testing.T, text string) map[string]string {
	t.Helper()
	pins := map[string]string{}
	inside := false
	closed := false
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "```") {
			if inside {
				closed = true
				break
			}
			inside = true
			continue
		}
		if !inside || line == "" {
			continue
		}
		match := hpkeVectorPinLine.FindStringSubmatch(line)
		if match == nil {
			t.Errorf("interop/PINS.md pin line %q is not the key=<40-char sha> shape the file requires", line)
			continue
		}
		pins[match[1]] = match[2]
	}
	if !inside || !closed {
		t.Fatalf("interop/PINS.md has no closed fenced block, so its machine-readable pins cannot be read")
	}
	return pins
}

// The two column provenance table under the vendored corpus's own heading, as a field to
// value map.
//
// Scoped to that section rather than to the file, because the same strings appear in the
// prose and in the fetch url, and a strings.Contains over the whole file cannot tell a
// corrupted table row from an intact copy somewhere else. The vendored digest was already
// recorded twice when this was written, and replacing one copy of it left every test green
// precisely because the other copy still answered the Contains.
func hpkeVectorPinFileRows(t *testing.T, text string) map[string]string {
	t.Helper()
	heading := "## " + hpkeVectorPath
	start := strings.Index(text, heading)
	if start < 0 {
		t.Fatalf("interop/PINS.md has no %q section, so its provenance table cannot be located", heading)
	}
	section := text[start+len(heading):]
	if next := strings.Index(section, "\n## "); next >= 0 {
		section = section[:next]
	}
	rows := map[string]string{}
	for _, raw := range strings.Split(section, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) != 2 {
			continue
		}
		field := strings.TrimSpace(cells[0])
		if field == "Field" || strings.Trim(field, "-") == "" {
			continue
		}
		rows[field] = strings.Trim(strings.TrimSpace(cells[1]), "`")
	}
	return rows
}

// TestHpkeVectorProvenanceIsRecordedInThePinFile ties this file's constants to the one pin
// file, in both directions and field by field. The provenance is the deliverable: a
// vendored corpus is worth exactly what can be said about where it came from, and until
// this existed nothing held interop/PINS.md to what it says. Corrupting that text left the
// package green 33 times out of 35 — the upstream commit set to forty zeros, the
// machine-readable hpke= line deleted outright, the repository pointed at a fork, the
// selection predicate replaced, every row of the provenance table rewritten or removed.
//
// Contains is not enough for the parts that repeat. The commit appears in the fence, in the
// table and in the fetch url, and the vendored digest appears twice, so a check that asks
// only whether a string is somewhere in the file is answered by the copy that was not
// corrupted. The fence and the table are therefore parsed, and only claims that occur once
// are matched as text.
func TestHpkeVectorProvenanceIsRecordedInThePinFile(t *testing.T) {
	text := hpkeVectorPinFile(t)

	pins := hpkeVectorMachineReadablePins(t, text)
	if got, ok := pins["hpke"]; !ok {
		t.Errorf("interop/PINS.md has no machine-readable hpke=<sha> line, so the upstream commit is recorded only in prose")
	} else if got != hpkeVectorUpstreamCommit {
		t.Errorf("interop/PINS.md pins hpke=%s, this corpus was vendored from %s", got, hpkeVectorUpstreamCommit)
	}

	rows := hpkeVectorPinFileRows(t, text)
	want := map[string]string{
		"Upstream repository": hpkeVectorUpstreamRepository,
		"Upstream commit":     hpkeVectorUpstreamCommit,
		"Upstream path":       hpkeVectorUpstreamPath,
		"Upstream sha256":     hpkeVectorUpstreamSha256,
		"Vendored sha256":     hpkeVectorSha256,
	}
	for _, field := range slices.Sorted(maps.Keys(want)) {
		got, ok := rows[field]
		if !ok {
			t.Errorf("interop/PINS.md provenance table has no %q row", field)
			continue
		}
		if got != want[field] {
			t.Errorf("interop/PINS.md records %s = %s, this corpus was vendored from %s", field, got, want[field])
		}
	}
	for _, field := range slices.Sorted(maps.Keys(rows)) {
		if _, ok := want[field]; !ok {
			t.Errorf("interop/PINS.md provenance table carries an unpinned %q row", field)
		}
	}

	// the claims that occur once, including the url that has to agree with the three fields
	// it is built out of
	for _, claim := range []struct {
		what string
		want string
	}{
		{
			what: "the fetch url",
			want: "https://raw.githubusercontent.com/" + hpkeVectorUpstreamRepository +
				"/" + hpkeVectorUpstreamCommit + "/" + hpkeVectorUpstreamPath,
		},
		{what: "the selection predicate", want: hpkeVectorFilter},
		{what: "the serialization call", want: hpkeVectorSerialization},
		{
			what: "the published depth",
			want: fmt.Sprintf("all %d encryptions and all %d exports",
				hpkeVectorHighestSequence+1, hpkeVectorExportCount),
		},
		{what: "the attributes file", want: hpkeVectorAttributesPath},
		{what: "the attributes rule", want: "`* -text`"},
		{what: "the file holding the second copy of the digest", want: "mls/hpke_vectors_test.go"},
		{what: "the constant holding it", want: "hpkeVectorSha256"},
		{what: "the smudge detector", want: "TestHpkeVectorFileWasNotSmudgedOnTheWayIn"},
		{what: "the attributes check", want: "TestHpkeVectorDirectoryDisablesGitsTextConversion"},
		{what: "this test", want: "TestHpkeVectorProvenanceIsRecordedInThePinFile"},
	} {
		if !strings.Contains(text, claim.want) {
			t.Errorf("interop/PINS.md does not record %s: %q", claim.what, claim.want)
		}
	}

	// the row in the summary table at the top, which records the vendored digest a second
	// time and points at the fence for the commit
	row := ""
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "|") && strings.Contains(line, "`"+hpkeVectorPath+"`") {
			row = line
			break
		}
	}
	if row == "" {
		t.Fatalf("interop/PINS.md summary table has no row for %s", hpkeVectorPath)
	}
	if !strings.Contains(row, hpkeVectorSha256) {
		t.Errorf("interop/PINS.md summary row for %s does not carry sha256 %s", hpkeVectorPath, hpkeVectorSha256)
	}
	if !strings.Contains(row, "`hpke=` line above") {
		t.Errorf("interop/PINS.md summary row for %s does not point at the machine-readable hpke= line", hpkeVectorPath)
	}
}

// TestHpkeVectorCorpusIsTheWholePublishedCorpus states the shape every known answer in
// this package relies on, as a test of its own rather than only as a precondition inside
// the loader. The loader's assertions run whenever a test loads the corpus, but they read
// as plumbing; a reader asking what the corpus is supposed to contain should not have to
// infer it from a helper's fatal calls.
//
// It reads the raw parse rather than the loader's output, and that is not a style choice.
// A test that calls the loader sees only a corpus the loader has already accepted, so the
// entry count, the mode, the registry triple and the aead distinctness restated here were
// all unreachable: the loader fatals on each of those bands first, and a check placed
// where it cannot observe its subject is a check that cannot fail.
//
// The counts are exact rather than lower bounds. A corpus that grew an entry is a corpus
// somebody re-vendored under a different filter, and the right response is a failure that
// says so rather than a loop that quietly covers more or less than the pin file claims.
func TestHpkeVectorCorpusIsTheWholePublishedCorpus(t *testing.T) {
	vectors := parseHpkeVectors(t)
	if len(vectors) != 2 {
		t.Fatalf("the corpus holds %d entries, want 2", len(vectors))
	}
	for _, vector := range vectors {
		if vector.Mode != 0 {
			t.Errorf("%s is for mode %d, want 0 (base)", hpkeVectorCodePoints(vector), vector.Mode)
		}
		if got := len(vector.Encryptions); got != hpkeVectorHighestSequence+1 {
			t.Errorf("%s carries %d encryptions, want %d",
				hpkeVectorCodePoints(vector), got, hpkeVectorHighestSequence+1)
		}
		if got := len(vector.Exports); got != hpkeVectorExportCount {
			t.Errorf("%s carries %d exported values, want %d",
				hpkeVectorCodePoints(vector), got, hpkeVectorExportCount)
		}
		if vector.KemId != HpkeKemX25519HkdfSha256 || vector.KdfId != HpkeKdfHkdfSha256 {
			t.Errorf("%s is for kem %#04x kdf %#04x, want %#04x and %#04x",
				hpkeVectorCodePoints(vector), vector.KemId, vector.KdfId,
				HpkeKemX25519HkdfSha256, HpkeKdfHkdfSha256)
		}
	}
	// the two entries have to differ in their aead, since that is the only position where
	// the 0x0001 kdf and aead collision is visible at all
	if vectors[0].AeadId == vectors[1].AeadId {
		t.Fatalf("both entries are for aead %#04x, so the kdf and aead positions can be transposed unobserved", vectors[0].AeadId)
	}
}

// TestHpkeVectorEncapsulatedKeyIsTheEphemeralPublicKey states what RFC 9180 section 4.1
// means by enc for a DHKEM: SerializePublicKey(pkE) and nothing else. The corpus publishes
// pkEm and enc as separate fields and the key derivation known answer compares the derived
// ephemeral public key against one of them, so without this the other is never read and a
// corpus whose two fields disagreed would go unnoticed.
//
// It is also the only thing in this package that says the encapsulated key is a public key
// rather than an opaque kem output. Both are 32 bytes here, so no length can say it.
func TestHpkeVectorEncapsulatedKeyIsTheEphemeralPublicKey(t *testing.T) {
	for _, vector := range loadHpkeVectors(t) {
		pkEm := decodeVectorField(t, vector.name, "pkEm", vector.PkEm)
		enc := decodeVectorField(t, vector.name, "enc", vector.Enc)
		if !bytes.Equal(pkEm, enc) {
			t.Errorf("%s: pkEm = %x but enc = %x, and RFC 9180 section 4.1 makes them the same bytes",
				vector.name, pkEm, enc)
		}
	}
}
