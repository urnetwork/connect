// The machinery every mlswg vector runner in this package shares.
//
// Three families now register against the registry in vectors_test.go -- psk_secret and
// key-schedule from key_schedule_kat_test.go, transcript-hashes from transcript_kat_test.go
// -- and a fourth arrives with every later plan. The parts they have in common are here
// rather than in each of them, because the hardening those runners needed is not the sort
// that gets rediscovered: the first psk_secret runner reported a comparison count of 22
// with the derivation never called, and a second and third independent copy of that
// accounting would each have had to learn the same lesson separately, at whatever point
// somebody noticed.
//
// What is shared, and why each part is a part rather than a line in a runner:
//
//   - the run tally, which partitions a corpus into the cases the suite filter matched and
//     the cases it declined, and refuses a run whose two halves do not add up to the file.
//     A filter that matched nothing and a filter that matched all seven published suites
//     are both green runs without it, and both are runs of a family that proved nothing.
//   - the declined-at-a-registered-suite refusal. A comparator answering "out of scope" for
//     a case at a suite this package registers is the filter and the comparator disagreeing
//     about what runs, and the count then reads correct over a run that is smaller than it.
//   - the comparator control driver. Every case of every vendored corpus agrees with this
//     implementation, so a comparator that checked everything and a comparator that checked
//     nothing produce identical runs over it; the only way to separate them is to hand the
//     comparator an answer that is wrong on purpose and require the matching refusal. The
//     unmodified case is required to be ACCEPTED first, or a comparator that refuses
//     everything satisfies the whole table.
//   - the installed-family assertion, which is two edits in the source -- register the
//     runner, delete the number from expectedPendingFamilies -- and one of them alone
//     leaves a gate green over a family nothing runs.
//   - the opaque<V> tail split, which three files were reading the same published structures
//     with. The tag at the end of a FramedContentAuthData and the tag at the end of a
//     PublicMessage are the same shape, and the width of the length prefix in front of
//     either is the codec's answer and not the literal 1.
//
// The suite filter itself was already shared: implementedSuite in key_schedule_kat_test.go
// reads the ciphersuite registry rather than listing code points, and every family calls it.
//
// Nothing here decides what a family compares. That is each family's own, because the answer
// a family owes is the family's; what is shared is the accounting that makes an answer it
// did not compare impossible to report as one it did.
package mls

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/urnetwork/connect/mls/syntax"
)

// errVectorTagTail is the refusal splitTrailingOpaqueTag makes. A sentinel rather than a
// formatted string, so a family's control can require this refusal specifically and a
// comparator that refused for some other reason is reported.
var errVectorTagTail = errors.New("the tail of this structure is not an opaque<V> of the suite's KDF.Nh")

// vectorLengthPrefix is the length prefix an opaque<V> of n octets carries, spelled by the
// codec rather than written out here.
//
// RFC 9420 section 2.1.2 writes 0..63 as one octet and 64..16383 as two, so a helper that
// assumed a single octet is right for both registered suites -- KDF.Nh is 32 and 32 <= 63 --
// and stops being right at the first suite whose hash is SHA-512, where Nh is 64 and the
// prefix is 0x40 0x40. It would stop being right loudly, which is better than silently, but
// loudly in the helpers that read it rather than in the one line that is actually wrong. So
// the width is derived: the codec is asked to encode a vector of that length and the prefix
// is whatever it wrote in front of the content.
//
// The error is returned rather than reported, so the callers that need a verdict can have
// one and the callers that need a fatal can raise it themselves.
// TestThePublishedVectorPrefixWidensWithTheVector is the control, over both widths and over
// the boundary from both sides.
func vectorLengthPrefix(n int) ([]byte, error) {
	w := syntax.NewWriter()
	w.WriteOpaque(make([]byte, n))
	encoded, err := w.Bytes()
	if err != nil {
		return nil, fmt.Errorf("encode the length prefix a %d octet <V> vector carries: %w", n, err)
	}
	prefix := encoded[:len(encoded)-n]
	if len(prefix) == 0 {
		return nil, fmt.Errorf("%w: a %d octet opaque<V> encodes to %d octets and therefore carries no length prefix, so nothing separates a tail from the field in front of it",
			errVectorTagTail, n, len(encoded))
	}
	return prefix, nil
}

// splitTrailingOpaqueTag splits a serialized structure whose last field is an opaque<V> of
// exactly nh octets into everything in front of that field and the tag itself.
//
// Both a FramedContentAuthData's confirmation_tag and a PublicMessage's membership_tag are
// the LAST field of their structure and both are a MAC, which is an opaque<V> of exactly
// KDF.Nh octets, so the tail is the tag and what sits in front of it is that vector's own
// length prefix. For an AuthenticatedContent carrying a Commit the head is precisely the
// serialized ConfirmedTranscriptHashInput, which is what transcript-hashes.json is read
// with.
//
// The prefix is asserted rather than skipped over: if the octets at that offset are not the
// encoded length of the tail being read, then what is being read is not a tag, and a
// comparison against it would be a comparison against the wrong bytes rather than a failure.
// That check plus the caller's own MAC verification is what makes the split self-validating
// rather than assumed -- a split taken one octet out recovers bytes the MAC refuses. When p6
// lands (*AuthenticatedContent).UnmarshalMLS this is replaced by the parse and the MAC check
// stays.
//
// bytes.Equal and not a loop of this file's own. The comparison is over a PUBLIC opaque<V>
// length prefix and not over a tag, so guardrail 8 has nothing to say about it either way --
// but a byte loop spelled out here is a comparator living in this package outside the class
// every derived gate can see, and one edit away from being pointed at a tag. The library call
// is inside that class; the loop was not.
func splitTrailingOpaqueTag(blob []byte, nh int) (head []byte, tag []byte, err error) {
	prefix, err := vectorLengthPrefix(nh)
	if err != nil {
		return nil, nil, err
	}
	if len(blob) <= nh+len(prefix) {
		return nil, nil, fmt.Errorf("%w: the structure is %d octets and a %d octet tag with its %d octet length prefix leaves nothing in front of it",
			errVectorTagTail, len(blob), nh, len(prefix))
	}
	at := len(blob) - nh - len(prefix)
	if found := blob[at : at+len(prefix)]; !bytes.Equal(found, prefix) {
		return nil, nil, fmt.Errorf("%w: the %d octets before its last %d are %x, and a <V> vector of %d octets is written %x",
			errVectorTagTail, len(prefix), nh, found, nh, prefix)
	}
	return blob[:at], blob[len(blob)-nh:], nil
}

// vectorRunTally is the accounting one family's runner does over its corpus: how the suite
// filter partitioned the file, which registered suites it reached, and how many published
// answers were actually compared against.
//
// It exists because a runner that ran nothing reports exactly what a runner that passed
// everything reports. Every field here is written at the point the work that produces it
// happens, and assertRun holds all of them at once against numbers the family wrote down, so
// a filter that matched nothing, a comparator that declined every case and a corpus that
// parsed to an empty array are each a failure rather than a smaller PASS.
type vectorRunTally struct {
	// file is the corpus basename, for the failure messages.
	file string
	// entries is how many cases the corpus parsed to.
	entries int
	// suiteless is true where the corpus publishes no ciphersuite column at all, which is
	// what family 12's messages.json is: a pile of encodings, none of which a ciphersuite
	// selects anything in. Every case of such a corpus is covered and none is declined, and
	// the three per suite assertions in assertRun are about a column that does not exist.
	//
	// DERIVED from the corpus by newVectorRunTally and never declared by the family, for
	// guardrail 5's reason. A family that set this itself could set it over a corpus that
	// DOES publish the column, and the run would then take the suite split -- the assertion
	// that separates a filter which reached both registered suites from one that reached the
	// same suite twice -- out of its own accounting while every other count still added up.
	suiteless bool
	// covered and skipped are the two halves of the partition. Their sum must be entries.
	covered int
	skipped int
	// matched counts the covered cases by the registered suite they ran at, so a run that
	// reached one of the two registered suites for every case and the other once is a
	// failure rather than a total that looks right.
	matched map[CipherSuite]int
	// published counts EVERY case by its ciphersuite, in scope or not, so the per suite
	// split is derived from the corpus's own census of itself rather than transcribed.
	published map[uint16]int
	// answers counts each distinct published answer a comparison was made against, and
	// comparisons counts the comparisons. A corpus read as one repeated value -- every
	// field decoding to the same string, every epoch decoding as epoch 0 -- compares the
	// right number of times against the wrong number of answers.
	answers     map[string]int
	comparisons int
}

// newVectorRunTally loads a family's corpus through the harness loader and opens the
// accounting over it.
//
// LoadVectorFile is fatal and never skipping on a corpus it cannot read, which is the
// property TestNoVectorRunnerCanSkip holds structurally; an empty parse is fatal here for
// the same reason, since a corpus truncated to [] would otherwise take a family out of the
// suite and report that as a clean run.
func newVectorRunTally(t *testing.T, file string) (*vectorRunTally, []json.RawMessage) {
	t.Helper()
	entries := LoadVectorFile(t, file)
	if len(entries) == 0 {
		t.Fatalf("%s parsed to no entries, so every comparison below would be against nothing", file)
	}
	return &vectorRunTally{
		file:      file,
		entries:   len(entries),
		suiteless: !corpusPublishesACipherSuiteColumn(t, file, entries),
		matched:   map[CipherSuite]int{},
		published: map[uint16]int{},
		answers:   map[string]int{},
	}, entries
}

// vectorCaseHeader is the one field of a corpus case every piece of this file needs to read
// before it knows anything else about the family: the ciphersuite the case sits at.
//
// A named type and not a second anonymous struct literal, so the json key is spelled ONCE in
// this file and every reader of it -- the driver that picks a case to drive an installed
// verifier with, and the derivation of suiteless below -- asks theJsonKeyOf for the same
// spelling. Two spellings of one key is how the two come apart about which key the corpus
// actually uses, and the disagreement is silent in the worst direction: the second spelling
// looks up nothing, and a corpus that publishes the column perfectly well is then read as one
// that does not.
type vectorCaseHeader struct {
	CipherSuite uint16 `json:"cipher_suite"`
}

// corpusPublishesACipherSuiteColumn answers whether ANY case of a corpus publishes the
// ciphersuite key at all.
//
// Read off a generic decode rather than off vectorCaseHeader, because that is the whole
// question: an absent key and a published 0 decode identically through the struct, and only
// one of the two means "this family has no suite dimension". The key itself is read off
// vectorCaseHeader's own struct tag, so the spelling cannot drift from the filter's.
//
// ANY rather than ALL, so a corpus that publishes the column for some of its cases and not
// others is treated as suite bearing and its keyless cases fall to the filter, which declines
// them. A corpus in that state is a corpus problem and this is where it surfaces as one --
// as an unregistered suite 0 -- rather than as a family that quietly stopped asserting its
// suite split.
func corpusPublishesACipherSuiteColumn(t *testing.T, file string, entries []json.RawMessage) bool {
	t.Helper()
	key := theJsonKeyOf(t, vectorCaseHeader{}, "CipherSuite")
	for index, raw := range entries {
		published := map[string]json.RawMessage{}
		if err := json.Unmarshal(raw, &published); err != nil {
			t.Fatalf("parse %s case %d as a json object: %v", file, index, err)
		}
		if _, found := published[key]; found {
			return true
		}
	}
	return false
}

// filter records one case against the suite filter and answers whether this package has a
// provider for it.
//
// One call per case, taking both branches itself, so a case cannot be counted twice or take
// neither branch. The filter it consults is implementedSuite, which reads the ciphersuite
// registry rather than a list of code points -- see
// TestImplementedSuiteIsTheRegistryAndNotAList.
func (self *vectorRunTally) filter(code uint16) (CipherSuite, bool) {
	self.published[code]++
	suite, ok := implementedSuite(code)
	if !ok {
		self.skipped++
		return 0, false
	}
	self.covered++
	self.matched[suite]++
	return suite, true
}

// cover records one case of a corpus that publishes no ciphersuite column.
//
// The suiteless half of filter() above, and separate from it because the two answer different
// questions: filter asks the registry whether this package has a provider for a code point,
// and there is no code point here to ask about. A family that called this over a corpus that
// DOES publish the column would be counting a case as covered without consulting the filter
// at all, which is refused here rather than left to show up as a suite split that adds up.
func (self *vectorRunTally) cover(t *testing.T) {
	t.Helper()
	if !self.suiteless {
		t.Fatalf("%s publishes a cipher_suite column and a case of it was counted as covered without the suite filter being asked about it",
			self.file)
	}
	self.covered++
}

// requireCompared refuses a comparator that declined a case the suite filter matched.
//
// The two disagree about what runs when this fires, and the disagreement is silent in the
// worst direction: the tally counts the case as covered because the filter matched it, the
// comparator returned an empty comparison, and the count then reads correct over a run that
// compared one case fewer.
func (self *vectorRunTally) requireCompared(t *testing.T, index int, suite CipherSuite, inScope bool) {
	t.Helper()
	if !inScope {
		t.Fatalf("%s case %d is at suite %#04x, which this package registers, and the comparator declined it",
			self.file, index, uint16(suite))
	}
}

// answer records one comparison made against one published answer.
//
// Called by the runner and not by the comparator, and called with the answer the runner read
// out of the corpus text ITSELF rather than with the comparator's copy of it, so a
// comparator that answered without computing anything moves neither number.
func (self *vectorRunTally) answer(published string) {
	self.answers[published]++
	self.comparisons++
}

// assertRun is every accounting assertion a family runner in this package owes, held at once
// against the counts that family wrote down.
//
// The counts are written down by each family rather than derived here, for the reason task
// 16 gives: deriving an expected count with the same filter that is under test is how a
// filter matching nothing ends up agreeing with itself. What IS derived is the partition --
// covered plus skipped must equal the file -- and the per suite split, which is read off the
// corpus's own census of itself.
func (self *vectorRunTally) assertRun(t *testing.T, wantCovered int, wantSkipped int, wantComparisons int, wantDistinct int) {
	t.Helper()
	if self.covered+self.skipped != self.entries {
		t.Fatalf("%s: %d covered and %d skipped over %d cases; a case took neither branch",
			self.file, self.covered, self.skipped, self.entries)
	}
	if self.covered == 0 {
		t.Fatalf("%s: the suite filter matched no case at all, so this family ran nothing and reported it as a pass", self.file)
	}
	// the floor under the two counts the family writes down for itself, and the reason it is
	// here rather than assumed. Every assertion above is satisfied by a run that partitioned
	// the corpus perfectly and then compared NOTHING: covered and skipped add up, the key set
	// is the registry's, the per suite split matches the corpus census, and comparisons and
	// answers are held only against numbers the family supplied. A family that supplies zero
	// for both therefore passes having verified nothing at all, which is the single outcome
	// this whole accounting exists to be unable to reach -- and it is a two character edit,
	// smaller than deleting the comparison loop it describes. Measured on family 10: deleting
	// the answer() call and writing 0 down produced "0 comparisons against 0 distinct
	// published answers" under a green PASS.
	if wantComparisons < 1 || wantDistinct < 1 {
		t.Fatalf("%s: this family wrote down %d comparisons against %d distinct answers; a run that compares nothing reports what a run that compared everything reports, so neither count may be zero",
			self.file, wantComparisons, wantDistinct)
	}
	if self.covered != wantCovered {
		t.Fatalf("%s: covered %d cases, want %d; the filter matched %v", self.file, self.covered, wantCovered, self.matched)
	}
	if self.skipped != wantSkipped {
		t.Fatalf("%s: skipped %d cases at unimplemented suites, want %d", self.file, self.skipped, wantSkipped)
	}
	if self.suiteless {
		// a corpus with no ciphersuite column has nothing for the filter to decline, so a
		// run over one that reported a declined case, or that reached a suite, went through
		// filter() rather than cover() -- and the three assertions in the other arm would
		// then be held over a column the corpus does not publish.
		if self.skipped != 0 || len(self.matched) != 0 {
			t.Fatalf("%s: the corpus publishes no cipher_suite column and this run declined %d cases and reached %v; a suiteless corpus has no case for the suite filter to decline",
				self.file, self.skipped, slices.Sorted(maps.Keys(self.matched)))
		}
	} else {
		if got := slices.Sorted(maps.Keys(self.matched)); !slices.Equal(got, Suites()) {
			t.Fatalf("%s: the corpus answered for %v and this package registers %v", self.file, got, Suites())
		}
		// the per suite split, which the key set above says nothing about: a run that reached
		// one registered suite for every case and the other for one satisfies both the total and
		// the key set, and is a run that covered one suite.
		for _, suite := range Suites() {
			want := self.published[uint16(suite)]
			if want == 0 {
				t.Fatalf("%s: the corpus publishes nothing at suite %#04x, which this package registers",
					self.file, uint16(suite))
			}
			if self.matched[suite] != want {
				t.Fatalf("%s: suite %#04x was covered %d times and the corpus publishes %d cases at it",
					self.file, uint16(suite), self.matched[suite], want)
			}
		}
	}
	if self.comparisons != wantComparisons {
		t.Fatalf("%s: made %d comparisons over %d covered cases, want %d",
			self.file, self.comparisons, self.covered, wantComparisons)
	}
	if len(self.answers) != wantDistinct {
		t.Fatalf("%s: the %d comparisons were made against %d distinct published answers, want %d; a corpus read as one repeated value compares that many times and pins one answer",
			self.file, self.comparisons, len(self.answers), wantDistinct)
	}
	t.Logf("%s: covered %d cases at %v, skipped %d at unimplemented suites, %d comparisons against %d distinct published answers",
		self.file, self.covered, slices.Sorted(maps.Keys(self.matched)), self.skipped,
		self.comparisons, len(self.answers))
}

// publishedCorpusField reads one published answer out of a case decoded as a GENERIC json
// object, addressed by a dotted json path.
//
// Generic on purpose. A family's comparator reads the corpus through its own struct tags and
// this reads the same bytes with no struct involved, so the two are independent decodes of
// one file: a tag misspelled, renamed, or pointed at a key the corpus does not publish
// decodes to the empty string on the comparator's side and is a missing key here, which is a
// failure rather than a comparison of nothing against nothing.
//
// ONE reader and not one per answer shape. Every family before family 10 compares a json
// string, and family 10's resolutions column publishes an array of node indices per node, so
// there was for a while a second copy of this walk in tree_kat_test.go whose only difference
// was its tail. Two readers of one thing is what this file's shared machinery exists to
// prevent -- the two decode the same corpus and can come apart about it silently -- so the
// array shape lives in the tail below and there is one walk again. Nothing about the array
// arm is family 10's: any later family publishing a structured answer needs it.
func publishedCorpusField(t *testing.T, published map[string]json.RawMessage, name string) string {
	t.Helper()
	key, rest, isNested := strings.Cut(name, ".")
	raw, found := published[key]
	if !found {
		t.Fatalf("the corpus case does not publish %q, so whatever decodes it decodes to nothing and every comparison over it is vacuous", key)
	}
	// a loop rather than one nesting step, because a family's answers are not all one level
	// down: secret-tree publishes a leaf's generation at leaves[leaf][generation], and a
	// reader that could only take a key would have to decode that array through the
	// comparator's own struct, which is the one decode this function exists to be
	// independent of.
	walked := key
	for isNested {
		var segment string
		segment, rest, isNested = strings.Cut(rest, ".")
		raw = publishedCorpusSegment(t, raw, walked, segment)
		walked += "." + segment
	}
	text := ""
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	// the structured arm. The corpus's own bytes are rendered with their whitespace removed
	// rather than decoded into anything: a second read that went through a []uint32 would be
	// the comparator's own struct decode a second time, which is the one decode this whole
	// function exists to be independent of.
	//
	// Still a FAILURE and never an empty answer where the addressed value is not well formed
	// json, for the reason above: a path that addresses nothing has to be loud here rather
	// than an empty string compared against an empty string somewhere else.
	compacted := bytes.Buffer{}
	if err := json.Compact(&compacted, raw); err != nil {
		t.Fatalf("the published %s is neither a json string nor well formed json: %v", name, err)
	}
	return compacted.String()
}

// publishedCorpusSegment steps one segment of a dotted path into a published value: a json
// object takes a key and a json array takes a decimal index.
//
// The index arm is what lets a family address an answer inside a published array. It is a
// FAILURE and never an empty answer when the segment does not address anything -- an index
// past the end, or a key the case does not carry -- for publishedCorpusField's own reason:
// the whole point of this second decode is that a path which addresses nothing is loud here
// rather than an empty string compared against an empty string somewhere else.
//
// Which arm applies is read off the SHAPE OF THE PUBLISHED VALUE and never off the spelling of
// the segment. The version this replaces asked strconv.Atoi about the segment first, so a
// corpus that published an object key spelled as a decimal -- "0" -- would have had that object
// decoded as a json array and reported as a path that addresses nothing, at a path that is
// exactly right. No vendored corpus publishes such a key today, which is what makes the order a
// defect that arrives with a corpus update rather than with a change to any family: every
// family in this package now walks this one function, so the arm that fires has to be a
// question about the corpus and not about how a family spelled its path.
func publishedCorpusSegment(t *testing.T, raw json.RawMessage, walked string, segment string) json.RawMessage {
	t.Helper()
	object := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &object); err == nil {
		nested, found := object[segment]
		if !found {
			t.Fatalf("the published %s is a json object and does not carry %q", walked, segment)
		}
		return nested
	}
	elements := []json.RawMessage{}
	if err := json.Unmarshal(raw, &elements); err != nil {
		t.Fatalf("the published %s is neither a json object nor a json array, so the segment %q addresses nothing in it: %v",
			walked, segment, err)
	}
	index, err := strconv.Atoi(segment)
	if err != nil {
		t.Fatalf("the published %s is a json array and the segment %q is not a decimal index: %v", walked, segment, err)
	}
	// the canonical spelling, because "00" and "+1" parse to elements 0 and 1 while naming
	// something else. A path that addresses one element and reads as another is the same silent
	// mis-addressing this whole second decode exists to make loud.
	if strconv.Itoa(index) != segment {
		t.Fatalf("the published %s was addressed by the segment %q, which is index %d spelled another way",
			walked, segment, index)
	}
	if index < 0 || index >= len(elements) {
		t.Fatalf("the published %s holds %d elements and index %d was asked for", walked, len(elements), index)
	}
	return elements[index]
}

// theJsonKeyOf is the json key one field of a corpus row is published under, read off that
// field's own struct tag rather than typed out a second time.
//
// Two spellings of one key in one package is how the two end up disagreeing about which key the
// corpus actually uses, and the disagreement is silent in the worst direction: the second
// spelling looks up nothing, the lookup answers "absent", and whatever that absence means is
// then reported about a corpus that publishes the key perfectly well.
func theJsonKeyOf(t *testing.T, row any, field string) string {
	t.Helper()
	found, ok := reflect.TypeOf(row).FieldByName(field)
	if !ok {
		t.Fatalf("%T has no field %s, so nothing names the key it decodes", row, field)
	}
	key, _, _ := strings.Cut(found.Tag.Get("json"), ",")
	if key == "" {
		t.Fatalf("%T.%s carries no json key, so it decodes under its go name and this lookup would miss it", row, field)
	}
	return key
}

// comparatorRefusal is one deliberately wrong case a family's comparator must refuse, and
// the sentinel it must refuse it as.
type comparatorRefusal struct {
	// name is what was made wrong, for the failure message.
	name string
	// vector is the corrupted case, serialized the way the corpus serializes one.
	vector json.RawMessage
	// want is the sentinel the refusal must wrap. A refusal for the wrong reason is a
	// comparator checking something other than what this case corrupts.
	want error
}

// assertComparatorRefuses is the control every family runner in this package owes and cannot
// be.
//
// Every case of every vendored corpus agrees with this implementation, so a comparator that
// accepted everything and a comparator that checked everything produce identical runs over
// it. The only way to see the difference is to disagree with it on purpose, once per defect
// class, and require the matching refusal.
//
// The unmodified case is checked FIRST and is the reason the refusals mean anything: a
// comparator that refused everything would satisfy the whole table. The table is required to
// be non-empty and its names distinct, so a control that corrupted nothing, or that ran one
// case twice under two names, is a failure rather than a shorter table nobody wrote down.
func assertComparatorRefuses(
	t *testing.T,
	family string,
	compare func(t *testing.T, raw json.RawMessage) error,
	accepted json.RawMessage,
	refusals []comparatorRefusal,
) {
	t.Helper()
	if len(refusals) == 0 {
		t.Fatalf("%s: no deliberately wrong case was offered, so this control accepts a comparator that compares nothing", family)
	}
	named := map[string]bool{}
	for _, refusal := range refusals {
		if named[refusal.name] {
			t.Fatalf("%s: %q is offered twice, so this table covers one defect class fewer than it counts", family, refusal.name)
		}
		named[refusal.name] = true
		if refusal.want == nil {
			t.Fatalf("%s: %q names no sentinel, so any refusal at all would satisfy it", family, refusal.name)
		}
	}
	if err := compare(t, accepted); err != nil {
		t.Fatalf("%s: the unmodified published case was refused: %v; a comparator that refuses everything satisfies every case below", family, err)
	}
	for _, refusal := range refusals {
		err := compare(t, refusal.vector)
		if err == nil {
			t.Errorf("%s: %s was accepted; the comparator is not comparing", family, refusal.name)
			continue
		}
		if !errors.Is(err, refusal.want) {
			t.Errorf("%s: %s was refused as %v, want %v; a refusal for the wrong reason is a comparator checking something else",
				family, refusal.name, err, refusal.want)
		}
	}
}

// assertVectorFamilyIsInstalled is the registration half every family owes.
//
// Registering the family and deleting its number from expectedPendingFamilies are two edits,
// and doing only the first leaves TestVectorManifestIsComplete failing while doing only the
// second leaves it passing with the family uninstalled. Both are asserted here, and so is
// the identity of the functions installed, so a row that kept the number and lost the runner
// -- or that picked up some other family's runner -- fails rather than reading as installed.
//
// generate nil means the family has no generate direction, and it is asserted as an absence
// rather than ignored: a family that grew a generator without this call being updated is a
// generator nothing here says anything about.
//
// And the installed Verify is DRIVEN, not merely identified. Pointer identity says the manifest
// holds this function; it says nothing about the function doing anything. Measured rather than
// supposed: the body of each of the three registered verifiers was replaced by a discard of its
// argument and the package still reported 411 passes, with TestVectorFamiliesVerify still logging
// "3 families verified; 91 published cases offered to them" and TestVectorGenerateThenVerify --
// whose whole stated property rests on Verify -- still logging its generated cases as verified.
// assertInstalledVerifierRefusesAWrongCase closes that, once, here, for every family.
func assertVectorFamilyIsInstalled(
	t *testing.T,
	number int,
	file string,
	verify func(t *testing.T, raw json.RawMessage),
	generate func(t *testing.T) json.RawMessage,
) {
	t.Helper()
	family, ok := vectorManifest[number]
	if !ok {
		t.Fatalf("family %d is not in the manifest", number)
	}
	if family.File != file {
		t.Fatalf("family %d names %s, this runner reads %s", number, family.File, file)
	}
	if family.Verify == nil {
		t.Fatalf("family %d has no Verify, so TestVectorFamiliesVerify runs one family fewer and says nothing about it", number)
	}
	if slices.Contains(expectedPendingFamilies, number) {
		t.Fatalf("family %d is installed and expectedPendingFamilies still names it as pending", number)
	}
	if got := reflect.ValueOf(family.Verify).Pointer(); got != reflect.ValueOf(verify).Pointer() {
		t.Fatalf("family %d is installed with a verifier that is not this runner's", number)
	}
	assertInstalledVerifierRefusesAWrongCase(t, number, family)
	if generate == nil {
		if family.Generate != nil {
			t.Fatalf("family %d is installed with a generator and this runner declares none, so nothing holds that generator to anything", number)
		}
		return
	}
	if family.Generate == nil {
		t.Fatalf("family %d has no Generate, so the generate direction of spec A section 4.2.1 is unexercised for it", number)
	}
	if got := reflect.ValueOf(family.Generate).Pointer(); got != reflect.ValueOf(generate).Pointer() {
		t.Fatalf("family %d is installed with a generator that is not this runner's", number)
	}
}

// ---------------------------------------------------------------------------
// driving the shared machinery, rather than asserting about it
// ---------------------------------------------------------------------------

// probeAssertion runs one assertion against a probe *testing.T of its own and reports whether
// that assertion reported a failure.
//
// On its own goroutine because t.Fatalf leaves through runtime.Goexit: called on the caller's
// goroutine it would end the test doing the probing rather than the probe. A panic is recovered
// and returned rather than taken, for the reason recoveringRow in key_schedule_test.go records
// -- a panic here takes the whole test binary down, and the run then reports one failure
// somewhere else and nothing at all about every test declared after it.
//
// The probe has no parent, so what it records is recorded nowhere else and the caller decides
// what it means. mls/syntax/vectors_test.go drives its own family 16 runner the same way.
func probeAssertion(run func(probe *testing.T)) (failed bool, raised any) {
	probe := &testing.T{}
	done := make(chan struct{})
	go func() {
		// LIFO, so the recover runs first and close(done) last: a caller reading raised
		// after <-done reads it after it was written
		defer close(done)
		defer func() { raised = recover() }()
		run(probe)
	}()
	<-done
	return probe.Failed(), raised
}

// aCaseAtARegisteredSuite is the first case of a corpus at a ciphersuite this package has a
// provider for.
//
// The only kind of case a verifier can be driven with. A case at an unimplemented suite is
// DECLINED -- the normal condition for five of the seven suites the mlswg files publish -- and a
// verifier that declined everything is indistinguishable from one that checked everything, which
// is the whole failure this file exists to make impossible.
func aCaseAtARegisteredSuite(t *testing.T, file string) (json.RawMessage, bool) {
	t.Helper()
	entries := LoadVectorFile(t, file)
	// the suiteless corpus. A family whose cases carry no ciphersuite column runs every one
	// of them -- messages.json is 300 encodings and nothing in it is selected by a suite --
	// so its first case is a case the verifier can be driven with. Read off whether the
	// column is PUBLISHED and not off a decoded value, because an absent key decodes to 0,
	// suite 0 is not registered, and this loop would then report that a corpus of 300
	// drivable cases has none.
	if !corpusPublishesACipherSuiteColumn(t, file, entries) {
		if len(entries) == 0 {
			return nil, false
		}
		return entries[0], true
	}
	for _, raw := range entries {
		header := vectorCaseHeader{}
		if err := json.Unmarshal(raw, &header); err != nil {
			t.Fatalf("parse a %s case: %v", file, err)
		}
		if _, ok := implementedSuite(header.CipherSuite); ok {
			return raw, true
		}
	}
	return nil, false
}

// flipEveryPublishedOctet rewrites one corpus case so every hex string it publishes, at any
// depth, differs from the published one in a single bit, and reports how many it changed.
//
// DERIVED and not supplied by the family, which is the point. A per family hand written wrong
// case is a list, and a list is the shape this project has watched understate its own class
// fourteen times: the family that landed without one would be driven by nothing and would still
// read as installed. One flipped bit of every hex field is wrong wherever the family reads,
// without this function knowing what any family reads.
//
// Numbers are decoded as json.Number and written back as they were read, so the ciphersuite the
// case sits at survives untouched: a case rewritten to an unimplemented suite would be declined
// rather than refused, and every verifier alive or dead would satisfy the control by not running.
//
// Only non-empty valid hex is touched. A label or a name is left alone, since corrupting one
// tests a decoder rather than a comparison -- and a label that happens to read as hex being
// flipped anyway is harmless, because the case is meant to be wrong.
func flipEveryPublishedOctet(t *testing.T, raw json.RawMessage) (json.RawMessage, int) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var tree any
	if err := decoder.Decode(&tree); err != nil {
		t.Fatalf("parse the case to corrupt: %v", err)
	}
	flipped := 0
	var walk func(node any) any
	walk = func(node any) any {
		switch value := node.(type) {
		case map[string]any:
			for key, held := range value {
				value[key] = walk(held)
			}
			return value
		case []any:
			for index, held := range value {
				value[index] = walk(held)
			}
			return value
		case string:
			octets, err := hex.DecodeString(value)
			if err != nil || len(octets) == 0 {
				return value
			}
			octets[0] ^= 0x01
			flipped++
			return hex.EncodeToString(octets)
		}
		return node
	}
	body, err := json.Marshal(walk(tree))
	if err != nil {
		t.Fatalf("re-encode the corrupted case: %v", err)
	}
	return body, flipped
}

// assertInstalledVerifierRefusesAWrongCase drives the Verify a family registered instead of
// asserting about it.
//
// The unmodified case is required to be ACCEPTED first, and that is not a formality: a Verify
// that fataled on everything would satisfy the refusal below while comparing nothing, which is
// the same lesson assertComparatorRefuses records one screen up.
func assertInstalledVerifierRefusesAWrongCase(t *testing.T, number int, family VectorFamily) {
	t.Helper()
	accepted, found := aCaseAtARegisteredSuite(t, family.File)
	if !found {
		t.Fatalf("family %d publishes no case at a suite this package registers, so nothing drives the verifier it installed", number)
	}
	refused, flipped := flipEveryPublishedOctet(t, accepted)
	if flipped == 0 {
		t.Fatalf("family %d's case publishes no hex field to corrupt, so the refusal below would be over an unmodified case", number)
	}
	if failed, raised := probeAssertion(func(probe *testing.T) { family.Verify(probe, accepted) }); raised != nil {
		t.Fatalf("family %d's installed verifier panicked over a published case at a registered suite: %v", number, raised)
	} else if failed {
		t.Fatalf("family %d's installed verifier refused a published case at a registered suite; a verifier that refuses everything satisfies the refusal below",
			number)
	}
	if failed, raised := probeAssertion(func(probe *testing.T) { family.Verify(probe, refused) }); raised != nil {
		t.Fatalf("family %d's installed verifier panicked over a case with all %d of its published hex fields changed: %v",
			number, flipped, raised)
	} else if !failed {
		t.Fatalf("family %d's installed verifier accepted a case with all %d of its published hex fields changed, so it is installed and compares nothing",
			number, flipped)
	}
}

// TestProbeAssertionSeesBothOutcomes is the control on the probe every control below is read
// through.
//
// A probe stuck at "did not report" turns every row below into a green run over an assertion
// that reports nothing, and a probe stuck at "reported" turns every baseline into one. Both
// directions are asserted, and so is the panic path, because a probe that took a panic rather
// than returning it would take the binary down instead of reporting one row.
func TestProbeAssertionSeesBothOutcomes(t *testing.T) {
	if failed, raised := probeAssertion(func(probe *testing.T) {}); failed || raised != nil {
		t.Errorf("an assertion that reported nothing came back failed=%v raised=%v", failed, raised)
	}
	if failed, raised := probeAssertion(func(probe *testing.T) { probe.Errorf("reported") }); !failed || raised != nil {
		t.Errorf("an assertion that reported came back failed=%v raised=%v", failed, raised)
	}
	reached := false
	if failed, raised := probeAssertion(func(probe *testing.T) {
		probe.Fatalf("reported and stopped")
		reached = true
	}); !failed || raised != nil {
		t.Errorf("an assertion that raised a fatal came back failed=%v raised=%v", failed, raised)
	}
	if reached {
		t.Error("a probe's t.Fatalf returned to its caller rather than ending it")
	}
	if failed, raised := probeAssertion(func(probe *testing.T) { panic("the assertion panicked") }); raised == nil {
		t.Errorf("an assertion that panicked came back failed=%v with no panic reported", failed)
	} else if raised != "the assertion panicked" {
		t.Errorf("the probe reported %v rather than what was raised", raised)
	}
}

// theSourceDeclaring finds the file of this package that declares one function, or one method on
// a named receiver, rather than being told which file to read.
//
// Found rather than named for the reason sourceDeclaringPackageFunction records: a gate told
// which file to read goes on issuing a clean bill after the thing it guards moves next door. A
// subject in no file, or in two, is fatal rather than clean.
func theSourceDeclaring(t *testing.T, receiver string, name string) parsedSource {
	t.Helper()
	found := []parsedSource{}
	declaring := []string{}
	for _, path := range packageSourcePaths(t) {
		parsed := mustParseSource(t, path)
		for _, declaration := range parsed.file.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Name.Name != name || parsed.receiverOf(function) != receiver {
				continue
			}
			found = append(found, parsed)
			declaring = append(declaring, path)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%q %s is declared in %v, want exactly one file of this package", receiver, name, declaring)
	}
	return found[0]
}

// theReportsOf is every failure one declaration can raise, read as the format strings it hands
// t.Errorf, t.Error, t.Fatalf and t.Fatal.
//
// Derived from the source rather than listed, for guardrail 5's reason. A control table that
// enumerates the failures somebody remembered understates its class the moment a tenth assertion
// lands beside the nine it was written against, and the table then reports full coverage of a
// function it covers less of than it did yesterday. t.Logf is not a failure and is not collected.
//
// A report whose first argument is not a string literal is fatal rather than skipped: it is a
// report no row can be bound to, and skipping it would silently shrink the class again.
func theReportsOf(t *testing.T, parsed parsedSource, receiver string, name string) []string {
	t.Helper()
	reports := []string{}
	ast.Inspect(parsed.declarationOf(t, receiver, name), func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if !isCall {
			return true
		}
		selector, isSelector := call.Fun.(*ast.SelectorExpr)
		if !isSelector || !slices.Contains([]string{"Error", "Errorf", "Fatal", "Fatalf"}, selector.Sel.Name) {
			return true
		}
		if len(call.Args) == 0 {
			t.Fatalf("%s reports through %s with no argument at all, so nothing names it", name, selector.Sel.Name)
		}
		literal, isLiteral := call.Args[0].(*ast.BasicLit)
		if !isLiteral || literal.Kind != token.STRING {
			t.Fatalf("%s reports through %s with a first argument that is not a string literal, so no control row can be bound to it",
				name, selector.Sel.Name)
		}
		text, err := strconv.Unquote(literal.Value)
		if err != nil {
			t.Fatalf("unquote a report of %s: %v", name, err)
		}
		reports = append(reports, text)
		return true
	})
	if len(reports) == 0 {
		t.Fatalf("%s raises no report at all, so the control table over it controls nothing", name)
	}
	return reports
}

// assertEveryReportIsControlled binds a control table to the reports of the function it controls:
// one row per report, one report per row, and neither list allowed to be the longer.
//
// This is what makes the tables below a derived class rather than a list. A row keyed on a
// message that has been reworded matches nothing and fails here; an assertion added without a row
// leaves a report unclaimed and fails here; two rows aimed at one report leave another unclaimed
// and fail here.
func assertEveryReportIsControlled(t *testing.T, name string, reports []string, keys []string) {
	t.Helper()
	if len(keys) != len(reports) {
		t.Fatalf("%s can raise %d reports and this control offers %d rows; the rows name %v",
			name, len(reports), len(keys), keys)
	}
	claimed := map[int]string{}
	for _, key := range keys {
		matched := []int{}
		for index, report := range reports {
			if strings.Contains(report, key) {
				matched = append(matched, index)
			}
		}
		if len(matched) != 1 {
			t.Fatalf("%s: the control row %q names %d of its reports, want exactly one", name, key, len(matched))
		}
		if already, taken := claimed[matched[0]]; taken {
			t.Fatalf("%s: %q and %q both name the report %q, so some other report is named by nothing",
				name, already, key, reports[matched[0]])
		}
		claimed[matched[0]] = key
	}
}

// The two sentinels the comparator control refuses with. Two, because a refusal for the wrong
// reason is one of the things assertComparatorRefuses reports, and a control with one sentinel
// cannot produce it.
var (
	errComparatorControl      = errors.New("the control comparator's own refusal")
	errComparatorControlOther = errors.New("some other refusal entirely")
)

// theComparatorControl has the shape every family's comparator has: it accepts the case that
// says it is the right one and refuses every other, naming its own sentinel.
func theComparatorControl(t *testing.T, raw json.RawMessage) error {
	t.Helper()
	entry := struct {
		Right bool `json:"right"`
	}{}
	if err := json.Unmarshal(raw, &entry); err != nil {
		return fmt.Errorf("%w: the case will not parse: %v", errComparatorControl, err)
	}
	if entry.Right {
		return nil
	}
	return fmt.Errorf("%w: the case is not the right one", errComparatorControl)
}

// comparatorControlFixture is one whole call of assertComparatorRefuses: the comparator, the case
// it must accept, and the table of cases it must refuse.
type comparatorControlFixture struct {
	compare  func(t *testing.T, raw json.RawMessage) error
	accepted json.RawMessage
	refusals []comparatorRefusal
}

func (self comparatorControlFixture) run(probe *testing.T) {
	assertComparatorRefuses(probe, "control", self.compare, self.accepted, self.refusals)
}

// aPassingComparatorControl is the fixture assertComparatorRefuses must accept, and it is what
// makes every row below mean anything: a driver that reported everything would satisfy a table of
// failures and say nothing whatever about the driver.
func aPassingComparatorControl() comparatorControlFixture {
	return comparatorControlFixture{
		compare:  theComparatorControl,
		accepted: json.RawMessage("{\"right\":true}"),
		refusals: []comparatorRefusal{
			{"a case that is not the right one", json.RawMessage("{\"right\":false}"), errComparatorControl},
		},
	}
}

// TestAssertComparatorRefusesFlagsTheControlFixture is the control the shared refusal driver owes
// and has never had.
//
// Three families read their refusal tables through this one function now, and the extraction that
// put them there raised the blast radius of a single deletion inside it from one family to three:
// the one t.Errorf that reports a comparator accepting a deliberately wrong case was deleted, all
// three tables stopped meaning anything, and the package reported 411 passes. Measured, not
// supposed. The rows are bound to the function's own reports, so a seventh assertion cannot land
// here uncontrolled.
func TestAssertComparatorRefusesFlagsTheControlFixture(t *testing.T) {
	if failed, raised := probeAssertion(aPassingComparatorControl().run); failed || raised != nil {
		t.Fatalf("a correct comparator was reported: failed=%v raised=%v; every row below would then pass for the wrong reason",
			failed, raised)
	}
	rows := []struct {
		// names is the substring of the report this row must provoke. The bijection
		// against the function's own reports is asserted below.
		names string
		edit  func(*comparatorControlFixture)
	}{
		{"no deliberately wrong case was offered", func(f *comparatorControlFixture) {
			f.refusals = nil
		}},
		{"is offered twice", func(f *comparatorControlFixture) {
			f.refusals = append(f.refusals, f.refusals[0])
		}},
		{"names no sentinel", func(f *comparatorControlFixture) {
			f.refusals[0].want = nil
		}},
		{"the unmodified published case was refused", func(f *comparatorControlFixture) {
			f.compare = func(t *testing.T, raw json.RawMessage) error { return errComparatorControl }
		}},
		{"the comparator is not comparing", func(f *comparatorControlFixture) {
			f.compare = func(t *testing.T, raw json.RawMessage) error { return nil }
		}},
		{"a refusal for the wrong reason", func(f *comparatorControlFixture) {
			f.compare = func(t *testing.T, raw json.RawMessage) error {
				if err := theComparatorControl(t, raw); err != nil {
					return errComparatorControlOther
				}
				return nil
			}
		}},
	}
	keys := []string{}
	for _, row := range rows {
		keys = append(keys, row.names)
	}
	assertEveryReportIsControlled(t, "assertComparatorRefuses",
		theReportsOf(t, theSourceDeclaring(t, "", "assertComparatorRefuses"), "", "assertComparatorRefuses"), keys)
	for _, row := range rows {
		fixture := aPassingComparatorControl()
		fixture.refusals = slices.Clone(fixture.refusals)
		row.edit(&fixture)
		failed, raised := probeAssertion(fixture.run)
		if raised != nil {
			t.Errorf("%s: the driver panicked: %v", row.names, raised)
			continue
		}
		if !failed {
			t.Errorf("%s: the driver reported nothing, so that report can be deleted with all three families' refusal tables still reading green",
				row.names)
		}
	}
}

// vectorRunControlFixture is one whole call of assertRun: the tally, and the four counts the
// family wrote down.
type vectorRunControlFixture struct {
	tally       *vectorRunTally
	covered     int
	skipped     int
	comparisons int
	distinct    int
}

func (self vectorRunControlFixture) run(probe *testing.T) {
	self.tally.assertRun(probe, self.covered, self.skipped, self.comparisons, self.distinct)
}

// aPassingVectorRun is a run assertRun must accept, built over the suites this package actually
// registers rather than over invented ones, so a suite added to the registry cannot leave this
// control asserting over a registry that no longer exists.
func aPassingVectorRun(t *testing.T) vectorRunControlFixture {
	t.Helper()
	suites := Suites()
	if len(suites) == 0 {
		t.Fatal("this package registers no ciphersuite, so assertRun has nothing to be controlled over")
	}
	// a code point outside the registry, so this run has a skipped half as every real run
	// does. Checked rather than assumed: a registry that grew to hold it would turn the
	// skipped case into a covered one and every row below would drift.
	if _, ok := implementedSuite(unregisteredControlSuite); ok {
		t.Fatalf("suite %#04x is registered, so the skipped case of this control is not skipped", unregisteredControlSuite)
	}
	tally := &vectorRunTally{
		file:        "control.json",
		matched:     map[CipherSuite]int{},
		published:   map[uint16]int{unregisteredControlSuite: 1},
		answers:     map[string]int{"00": 1, "01": 1},
		comparisons: 2,
		skipped:     1,
	}
	for _, suite := range suites {
		tally.published[uint16(suite)]++
		tally.matched[suite]++
		tally.covered++
	}
	tally.entries = tally.covered + tally.skipped
	return vectorRunControlFixture{
		tally:       tally,
		covered:     tally.covered,
		skipped:     tally.skipped,
		comparisons: tally.comparisons,
		distinct:    len(tally.answers),
	}
}

// Two code points this package does not register: one for the skipped half of a control run, one
// for the row that widens the covered key set. Both are asserted unregistered where they are used
// rather than assumed, because a registry that grew to hold either would make that row silently
// stop being the edit it is named after.
const (
	unregisteredControlSuite = uint16(0xffff)
	alienControlSuite        = uint16(0xfffe)
)

// TestAssertRunFlagsTheControlFixture is the control the shared accounting owes and has never had.
//
// Every count assertion three families make about their own runs lives in assertRun, and an
// `if true { return }` inserted after its t.Helper() disarmed all three at once with the package
// still reporting 411 passes. Measured, not supposed. Each row below is one minimal edit of a run
// assertRun accepts, and the rows are bound to the function's own reports, so an assertion added
// without a row fails here rather than arriving uncontrolled.
func TestAssertRunFlagsTheControlFixture(t *testing.T) {
	if failed, raised := probeAssertion(aPassingVectorRun(t).run); failed || raised != nil {
		t.Fatalf("a complete and consistent run was reported: failed=%v raised=%v; every row below would then pass for the wrong reason",
			failed, raised)
	}
	first := Suites()[0]
	if _, ok := implementedSuite(alienControlSuite); ok {
		t.Fatalf("suite %#04x is registered, so the row below does not widen the covered key set", alienControlSuite)
	}
	rows := []struct {
		// names is the substring of the report this row must provoke; the bijection
		// against assertRun's own reports is asserted below.
		names string
		edit  func(*vectorRunControlFixture)
	}{
		{"a case took neither branch", func(f *vectorRunControlFixture) {
			f.tally.entries++
		}},
		{"matched no case at all", func(f *vectorRunControlFixture) {
			f.tally.matched = map[CipherSuite]int{}
			f.tally.covered = 0
			f.tally.entries = f.tally.skipped
		}},
		{"covered %d cases, want", func(f *vectorRunControlFixture) {
			f.covered++
		}},
		{"skipped %d cases at unimplemented suites", func(f *vectorRunControlFixture) {
			f.skipped++
		}},
		// the suiteless arm taken over a corpus that does publish the column. Every count in
		// this fixture still adds up -- the partition, the totals, the comparisons -- and the
		// three assertions in the other arm are the ones that separate a run which reached
		// both registered suites from one that reached the same suite twice, so a family that
		// reached this arm by mistake would go on passing with its suite split unasserted.
		{"publishes no cipher_suite column", func(f *vectorRunControlFixture) {
			f.tally.suiteless = true
		}},
		{"the corpus answered for", func(f *vectorRunControlFixture) {
			f.tally.matched[CipherSuite(alienControlSuite)] = 1
		}},
		{"publishes nothing at suite", func(f *vectorRunControlFixture) {
			delete(f.tally.published, uint16(first))
		}},
		{"was covered %d times", func(f *vectorRunControlFixture) {
			f.tally.published[uint16(first)] = f.tally.matched[first] + 1
		}},
		{"made %d comparisons over", func(f *vectorRunControlFixture) {
			f.comparisons++
		}},
		// the family writing zero down for both, which every other row leaves reachable:
		// nothing above this one looks at the magnitude of the counts a family supplies,
		// only at whether the run matched them, and a run that compared nothing matches
		// zero exactly.
		{"so neither count may be zero", func(f *vectorRunControlFixture) {
			f.comparisons = 0
			f.distinct = 0
		}},
		{"distinct published answers, want", func(f *vectorRunControlFixture) {
			f.distinct++
		}},
	}
	keys := []string{}
	for _, row := range rows {
		keys = append(keys, row.names)
	}
	assertEveryReportIsControlled(t, "assertRun",
		theReportsOf(t, theSourceDeclaring(t, "*vectorRunTally", "assertRun"), "*vectorRunTally", "assertRun"), keys)
	for _, row := range rows {
		fixture := aPassingVectorRun(t)
		row.edit(&fixture)
		failed, raised := probeAssertion(fixture.run)
		if raised != nil {
			t.Errorf("%s: assertRun panicked: %v", row.names, raised)
			continue
		}
		if !failed {
			t.Errorf("%s: assertRun reported nothing, so that assertion can be deleted with all three families' counts still reading green",
				row.names)
		}
	}
}

// publishedCorpusSegmentFixture is one whole call of publishedCorpusSegment: the published value,
// the dotted path already walked to reach it, and the segment to step by.
type publishedCorpusSegmentFixture struct {
	raw     json.RawMessage
	walked  string
	segment string
	// want is the value the step must answer when it answers at all, so a row that reached the
	// wrong element is a failure rather than a step that returned something.
	want string
}

func (self publishedCorpusSegmentFixture) run(probe *testing.T) {
	if got := publishedCorpusSegment(probe, self.raw, self.walked, self.segment); string(got) != self.want {
		probe.Errorf("the step answered %s, want %s", got, self.want)
	}
}

// aPassingCorpusSegment is the step publishedCorpusSegment must take, and the shape every row
// below is one edit of: an array of objects addressed by index, which is what family 3's
// leaves.N.M.field paths walk.
func aPassingCorpusSegment() publishedCorpusSegmentFixture {
	return publishedCorpusSegmentFixture{
		raw:     json.RawMessage(`[{"handshake_key":"00"},{"handshake_key":"01"}]`),
		walked:  "leaves.0",
		segment: "1",
		want:    `{"handshake_key":"01"}`,
	}
}

// TestPublishedCorpusSegmentStepsIntoTheShapeThePublishedValueHas is the control the walk every
// family reads its second decode through has never had.
//
// It was exercised only through family 3's leaves.N.M.field paths, and only in the direction
// where those paths are right: nothing asked it what it does with a segment that addresses
// nothing, and nothing at all pinned WHICH ARM it takes. The arm is the reason this test
// exists. It used to be chosen by asking strconv.Atoi about the segment, so an object key
// spelled as a decimal -- which no vendored corpus publishes today and any corpus update may --
// was decoded as an array and reported as a broken path at a path that is correct. The first
// assertion below is that case, and it is the one that fails against the order this replaces.
//
// The rows are bound to the function's own reports, so a sixth refusal cannot land here
// uncontrolled -- and a refusal deleted from it leaves a row naming nothing and fails here
// rather than turning a path that addresses nothing into an empty string compared against an
// empty string somewhere else.
func TestPublishedCorpusSegmentStepsIntoTheShapeThePublishedValueHas(t *testing.T) {
	if failed, raised := probeAssertion(aPassingCorpusSegment().run); failed || raised != nil {
		t.Fatalf("the step every family's second decode is made of was reported: failed=%v raised=%v; every row below would then pass for the wrong reason",
			failed, raised)
	}
	// the arm itself: a json object whose keys are spelled as decimals takes a KEY. Read off the
	// value's shape, this is an ordinary object lookup; read off the segment's spelling, it is an
	// array index into something that is not an array.
	decimalKeys := publishedCorpusSegmentFixture{
		raw:     json.RawMessage(`{"0":"beef","1":"cafe"}`),
		walked:  "sender_data",
		segment: "1",
		want:    `"cafe"`,
	}
	if failed, raised := probeAssertion(decimalKeys.run); failed || raised != nil {
		t.Errorf("a json object whose keys are spelled as decimals was not walked as an object: failed=%v raised=%v",
			failed, raised)
	}
	rows := []struct {
		// names is the substring of the report this row must provoke; the bijection against
		// publishedCorpusSegment's own reports is asserted below.
		names string
		edit  func(*publishedCorpusSegmentFixture)
	}{
		{"is a json object and does not carry", func(f *publishedCorpusSegmentFixture) {
			f.raw, f.segment = json.RawMessage(`{"handshake_key":"00"}`), "application_key"
		}},
		{"neither a json object nor a json array", func(f *publishedCorpusSegmentFixture) {
			f.raw = json.RawMessage(`"00"`)
		}},
		{"is not a decimal index", func(f *publishedCorpusSegmentFixture) {
			f.segment = "handshake_key"
		}},
		{"spelled another way", func(f *publishedCorpusSegmentFixture) {
			f.segment = "01"
		}},
		{"elements and index", func(f *publishedCorpusSegmentFixture) {
			f.segment = "2"
		}},
	}
	keys := []string{}
	for _, row := range rows {
		keys = append(keys, row.names)
	}
	assertEveryReportIsControlled(t, "publishedCorpusSegment",
		theReportsOf(t, theSourceDeclaring(t, "", "publishedCorpusSegment"), "", "publishedCorpusSegment"), keys)
	for _, row := range rows {
		fixture := aPassingCorpusSegment()
		row.edit(&fixture)
		failed, raised := probeAssertion(fixture.run)
		if raised != nil {
			t.Errorf("%s: the step panicked: %v", row.names, raised)
			continue
		}
		if !failed {
			t.Errorf("%s: the step reported nothing, so a path that addresses nothing comes back as a value every family compares against",
				row.names)
		}
	}
}

// ── the line ending gate, and the scope it derives ──────────────────────────────────

// packageSourcePathsIn is every go file at the top level of one package directory, sorted.
//
// It does not recurse because its CALLER has already descended: lineEndingScanRoots hands back
// every directory of this module holding Go source at any depth, so a nested directory arrives
// as a root of its own and recursing here would judge its files twice.
//
// This used to give a different reason, and both halves of that reason are false. It said what
// sits under testdata is either fixture source that only ever reaches a go/parser -- "which has
// no opinion at all about how a line ends" -- or bytes .gitattributes marks -text, and that
// "neither is read by an anchored string edit". Both were measured, not argued with:
//
//   - go/parser is not the only reader. crypto_forbidden_test.go opens every .go file under
//     testdata/forbidden with os.ReadFile and keeps string(body) in sourceTexts, and its
//     matchers run over that text. It survives a crlf flip only because codeOf normalises
//     the carriage returns away itself -- a defence that reader wrote for itself, and the
//     comment above it says so, naming this gate as what catches the flip "only after a
//     matcher has already read the file".
//   - nothing under testdata/forbidden is marked -text. It is covered by `*.go text eol=lf`
//     like every other Go file in this module, which is what this gate logs for that root.
//     Flipping mls/testdata/forbidden/nested/*.go to crlf on disk turns this gate RED.
//
// Testdata Go is source, it is judged, and the judged-file assertion below now states that as
// an assertion rather than as a sentence.
//
// A directory holding no go source is a fatal and not an empty result, because a scope that
// resolved to nothing is what "every file agreed" looks like when nothing was read.
func packageSourcePathsIn(t *testing.T, dir string) []string {
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

// moduleRootDir is the directory this module's go.mod sits in, walked up to rather than
// written down, so the closure below can tell a package of this checkout from a sibling
// repository beside it.
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

// goSourceDirsUnder is every directory at or below one root that DIRECTLY holds a .go file,
// answered as paths relative to `from`.
//
// One walk rather than a walk plus a glob per directory: a file with a .go extension IS the
// evidence that its directory holds source, so a root falls out of a file rather than out of a
// second question asked about the directory. A directory holding only subdirectories, or only
// files of other kinds, is not a root, because there is no source in it for an anchored edit to
// read and packageSourcePathsIn is fatal on a directory that turns out to hold none.
//
// Nothing is skipped. No name a directory carries exempts the Go source inside it from being
// read by an exact-string edit, which is the only thing this scope is about, and an exception
// here would be the thing the derivation below exists to have removed.
//
// It only ever descends, so the sibling checkout the closure had to decline in prose -- the
// sdk repository beside connect, which joinScanRoots reaches and which is a different working
// tree with its own core.autocrlf -- is not declined here. It is unreachable.
//
// Split out from its caller so it can be run against a tree built for a test. Its whole
// substance is that it DESCENDS, and a walk exercised only against the one tree it ships in
// cannot be told apart from a list that happens to be right about that tree.
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

// everyGoSourceFileUnder is every .go FILE at or below one directory, answered as paths relative
// to `from`, sorted.
//
// It is the second half of the judged-file assertion the gate below makes, and its whole value
// is that it shares NOTHING with the walk it is compared against. goSourceDirsUnder asks a
// question about DIRECTORIES through filepath.WalkDir and the gate then globs each answer; this
// asks a question about FILES through an explicit os.ReadDir recursion. A narrowing written into
// either one shows up as a set the other no longer matches. A helper called by both would be
// narrowed once for both, which is why this is not the walk above with a different predicate.
//
// There is no name test in it and no depth test in it. An entry is a directory, in which case it
// is descended into, or it ends .go, in which case it is named. That is deliberate, and it is
// the shape the scope control below gets wrong: that control says "NO NAME is an exception" and
// then observes it through a fixture holding four names, so a fifth name -- `security` is the
// one measured -- walks past a control written to forbid exactly that. A predicate with no names
// in it has no fixture of names to fall outside of.
//
// Nothing is pruned, .git included, for the same reason: the two enumerations have to agree
// about the same tree, and a prune here that the walk above does not make would report a
// difference that is this function's and not the tree's. .git holds no .go file, so the two
// agree either way; the symmetry is what is being kept, not the cost.
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

// namesOf prints a difference at a length a failure can be read at, and says how much it left
// out. A one file difference is named in full, which is the case that matters: the whole reason
// the assertion is over sets and not over counts is that a set can say WHICH file went missing.
func namesOf(paths []string, most int) string {
	if len(paths) <= most {
		return fmt.Sprintf("%v", paths)
	}
	return fmt.Sprintf("%v and %d more", paths[:most], len(paths)-most)
}

// lineEndingScanRoots is every directory of this module holding Go source, DERIVED by walking
// the module root rather than listed.
//
// The scope question, answered separately from the class question as R3a requires. The CLASS is
// each file's own pinned ending, read out of .gitattributes per file, and says nothing about
// where to look. This is where.
//
// It used to be a closure: start at this package, follow every "../" path literal its source
// hands to something, repeat in whatever that reached. The closure landed on the packages that
// read each other's TEXT, and that grouping was the whole mechanism while the gate was ending
// AGNOSTIC -- back then the gate asked whether a package agreed with ITSELF, so it had to know
// which files to compare against which, and "the packages that scan each other" was the answer.
//
// Since b4e84f4 the gate resolves each file's required ending from the pin independently and
// compares no file against any other. Grouping carries no information any more, so the closure
// had nothing left to compute -- and, more to the point, nothing left to justify the
// directories it left out. It excluded connect/mls/syntax and connect's own package, on a
// sentence that contradicted itself: the pin holds the child directory, and the pin cannot
// reach the working tree. Both halves cannot be true, and it is the second that is. Measured
// against the closure before it was replaced, rather than reasoned about: with all 23 files of
// mls/syntax flipped uniformly to crlf on disk, ./mls/... ./message/... ./messagegroup/... ran
// 7496 PASS, 0 FAIL, 0 SKIP.
//
// So the scope is the module, and the closure is deleted rather than given another root. A
// directory is in scope because it HOLDS Go source, so a package added to this module arrives
// in scope with nobody remembering to add it -- which is all the closure was ever for, kept,
// with the list of coupled packages it needed dropped. Nothing is excluded: not the module
// root's own package, not mls/syntax, and not testdata, whose Go fixtures are read by an
// anchored string edit exactly like any other file here. If some directory ever genuinely had
// to be left out, that is a finding about this repository and not an entry to add here.
//
// The count is the measure of what the closure was missing: 156 files in three directories
// before, 474 in fifteen after -- and it costs LESS, not more. Measured over four runs each:
// 0.15-0.18s before, 0.07-0.10s after. The closure had to parse every file of every root
// through go/parser to find the path literals in it; this reads nothing but the bytes it is
// judging, and there are three times as many of them for half the time.
func lineEndingScanRoots(t *testing.T) []string {
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
	// the coverage claim is checked rather than asserted, against a scope this package already
	// declares: every root of forbiddenScanRoots -- which five further gates alias rather than
	// restate -- has to fall out of the walk. It no longer asks whether a closure kept following
	// the coupling, because there is no closure; it asks the one thing a module walk can still
	// get wrong, which is whether it started at the MODULE root. A walk rooted at this package
	// reaches everything under mls and nothing beside it, and that is a green gate over a third
	// of the files it claims to judge.
	for _, declared := range forbiddenScanRoots {
		want := filepath.ToSlash(filepath.Clean(declared))
		if !slices.Contains(roots, want) {
			t.Fatalf("the walk returned %v, which does not reach %s; forbiddenScanRoots scans that directory's text, so a scope that cannot reach it is not rooted at this module",
				roots, want)
		}
	}
	return roots
}

// TestTheLineEndingScopeIsEveryDirectoryHoldingGoSource is the control on that walk, run
// against a tree built here rather than against this module, because the module's own answer
// cannot separate a walk that descends from a list that is right about this checkout. The
// derivation this replaced could not reach a child directory at all and said so in prose;
// prose is what let that go unnoticed.
//
// Four answers are stated, and each is a way this scope has actually been wrong here or is a
// way it would next go wrong:
//
//   - it DESCENDS, at any depth. A walk that stops at its root loses connect/mls/syntax, which
//     is the directory the closure could not reach and the one measured able to go uniformly
//     crlf against 7496 passing tests.
//   - a directory holding no Go source is not a root. packageSourcePathsIn is fatal on one, so
//     a walk keyed off directories rather than off files turns a folder of prose into a failure
//     nobody can act on.
//   - NO NAME is an exception. The four spellings below are the four a reader would reach for
//     first, because they are the four the Go tool itself skips: a leading dot, a leading
//     underscore, testdata, vendor. Go skips them when it is deciding what to COMPILE; this
//     gate is deciding what an exact-string edit can read, and every one of them can be read.
//     Four names are four names, though, and this bullet used to be the whole guarantee: a
//     FIFTH name walked past it, and `security` was the one measured doing it. What holds the
//     claim now is the judged-file assertion at the end of the gate, which compares the files
//     read against everyGoSourceFileUnder and needs no name in it at all. This fixture stays as
//     the demonstration that the walk descends and skips nothing; it is not the proof.
//   - the ROOTS come back relative to where the caller stands, so one reads as "../message",
//     the way every scan root in these gates is written, rather than as an absolute path that
//     matches nothing else anybody writes down here.
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

// TestTheIndependentGoSourceEnumerationNamesEveryGoFileAtAnyDepth is the control on the OTHER
// half of the judged-file assertion, run against a tree built here for the same reason the scope
// control is: an enumeration exercised only against the tree it ships in cannot be told apart
// from a list that happens to be right about that tree.
//
// The fixture is deliberately DEEPER than the scope control's, which is exactly two levels and
// therefore cannot see a depth limit of two. Thirteen files of this module sit three and four
// levels below the module root -- mls/testdata/forbidden/nested holds three of them -- so a
// limit that reads as harmless against a two deep fixture drops them, and that was one of the
// four narrowings this assertion was built to close.
//
// It states four answers, and the third is the point of the function existing at all:
//
//   - it names FILES, not directories, so what the gate judged can be compared against it member
//     by member rather than as a count that matches by accident.
//   - it DESCENDS to any depth, four levels here.
//   - no directory name is an exception -- and, unlike the scope control, the code it is
//     observing contains no directory name at all, so this fixture is a demonstration and not
//     the enumeration the property rests on.
//   - a file that is not .go is not a member, so a folder of prose does not inflate the set the
//     gate is held to.
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

// pinnedLineEndingFor answers the ending ONE .gitattributes checks one path out with -- "lf",
// "crlf", or "" -- and names the line that decided. The second value is what says whether this
// rule set had an OPINION at all, which is not the same question: a rule set that marks the path
// binary answers "" and has decided, and one that never mentions the path answers "" and has not.
//
// The rule set is read rather than restated for the reason the gate below gives: this repository
// states which ending is right in exactly one place, and a gate that hard-coded the answer would
// leave that place unguarded again.
//
// Resolution is git's own. Rules in FILE ORDER, LAST match wins, so a narrower line further down
// answers for the paths it covers and a scan stopping at the first match would report a pin that
// had already been undone. `-text` and the `binary` macro turn conversion off entirely, so they
// CLEAR an eol an earlier line asked for rather than sitting beside it. `text` with no eol says
// the file is text and leaves the ending to core.eol, which is a checkout's answer and not this
// repository's, so it pins nothing here and decides nothing either.
//
// The pattern half is gitAttributesPatternMatches, which has its own control, rather than a
// comparison over the pattern string. `*.go`, `/*.go` and `**/*.go` are one rule to git, and a
// gate that recognised only the spelling its author happened to use is a gate that gets silenced
// by respelling the line instead of by fixing the tree. That false positive has already happened
// here once, to the corpus rule.
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

// pinnedLineEndingOf resolves one SCANNED file's pin across every rule set that has a say in it,
// nearest first, which is how git resolves an attribute: a .gitattributes in the file's own
// directory overrides one in the module root, and the further one answers only for what the
// nearer one says nothing about.
//
// The walk is done rather than assumed, and that is not hypothetical tidiness. The first version
// of this read connect/.gitattributes and treated it as the whole answer. That is true today and
// stops being true the moment a package grows a rule set of its own -- and the failure it buys is
// this file's oldest one: a gate demanding lf while git checks the file out crlf, reading
// something other than what it claims to read, and reporting a correct tree as broken or a broken
// one as correct depending on which way the nearer rule went.
func pinnedLineEndingOf(t *testing.T, moduleRoot string, scanned string) (string, string) {
	t.Helper()
	segments := strings.Split(repositoryPathOf(t, moduleRoot, scanned), "/")
	for depth := len(segments) - 1; depth >= 0; depth-- {
		dir := filepath.Join(append([]string{moduleRoot}, segments[:depth]...)...)
		body, err := os.ReadFile(filepath.Join(dir, ".gitattributes"))
		if err != nil {
			continue
		}
		// a rule set's patterns are written against paths INSIDE it, so mls/.gitattributes says
		// "*.go" about "group.go" and never about "mls/group.go".
		if ending, decidedBy := pinnedLineEndingFor(string(body), strings.Join(segments[depth:], "/")); decidedBy != "" {
			return ending, decidedBy
		}
	}
	return "", ""
}

// repositoryPathOf is one scanned file as .gitattributes addresses it: relative to the module
// root, forward slashes. A rule's pattern is matched against a repository path and never against
// the "../message/framing.go" a scan happens to open the file by.
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

// TestTheLineEndingPinIsReadTheWayGitResolvesIt is the control on the derivation the gate below
// now rests on, and it is not optional in either direction: a reader answering "lf" for
// everything would report the tree correct whatever .gitattributes said, and one answering ""
// for everything would fail the gate on a correctly pinned repository. Both are stated, and so
// is the third answer the walk depends on -- whether a rule set decided anything at all.
//
// The table is written against rule sets spelled out here rather than against the live file, so
// it goes on meaning what it says when the live file changes; the nesting is exercised against a
// tree built for it, because this repository has no nested rule set covering Go source and a
// control that can only be run where the property already holds proves nothing. The live file is
// then asked one question of its own, because everything above would pass unchanged against a
// repository that had stopped pinning anything at all.
func TestTheLineEndingPinIsReadTheWayGitResolvesIt(t *testing.T) {
	const pin = "*.go text eol=lf"
	for _, probe := range []struct {
		body    string
		path    string
		want    string
		decides bool
		why     string
	}{
		{pin, "mls/group.go", "lf", true, "the rule this repository carries, on a path it covers"},
		{pin, "protocol/message.proto", "", false, "and one it does not"},
		{"", "mls/group.go", "", false, "no rule at all is no pin, which is what deleting the line looks like"},
		{"# " + pin, "mls/group.go", "", false, "a commented out rule is not a rule"},
		{"/*.go text eol=lf", "group.go", "lf", true, "the same rule anchored at the root"},
		{"**/*.go text eol=lf", "mls/group.go", "lf", true, "and spelled with a leading globstar"},
		{pin + "\n*.go text eol=crlf", "mls/group.go", "crlf", true, "the last matching line wins, which is git's resolution and not a preference"},
		{pin + "\nmls/** -text", "mls/group.go", "", true, "-text turns conversion off and clears the eol an earlier line asked for -- and DECIDES, so no outer rule set answers for it"},
		{pin + "\nmls/** binary", "mls/group.go", "", true, "and binary is git's macro for the same thing"},
		{"*.go text", "mls/group.go", "", false, "text with no eol says the file is text, not which ending a checkout writes, so an outer rule set still answers"},
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
		{"*.go text eol=crlf\n", "crlf", "a nearer rule set with an opinion overrides the module root's"},
		{"*.json -text\n", "lf", "a nearer rule set saying nothing about this path defers outward"},
		{"*.go -text\n", "", "a nearer rule set marking it binary decides, and the root's eol does not answer for it"},
	} {
		if err := os.WriteFile(filepath.Join(nested, ".gitattributes"), []byte(nearer.body), 0o644); err != nil {
			t.Fatalf("build the nesting fixture: %v", err)
		}
		if ending, _ := pinnedLineEndingOf(t, root, source); ending != nearer.want {
			t.Errorf("with %q nearer the file the walk answered %q, want %q: %s", nearer.body, ending, nearer.want, nearer.why)
		}
	}

	ending, decidedBy := pinnedLineEndingOf(t, moduleRootDir(t), "vectors_runner_test.go")
	if ending == "" {
		t.Fatal("no .gitattributes from this package up to the module root pins a line ending for this file, so the gate below has nothing to hold the working tree to")
	}
	t.Logf("the live rule set checks this file out %s, by %q", ending, decidedBy)
}

// TestThePackageSourceIsOneLineEndingThroughout refuses a working tree in which some file of a
// package disagrees with the rest of that package about how a line ends.
//
// Not a style rule. Every gate in this file and in the family runners reads source, and every
// repair to these packages is made as an exact-string edit over it -- and an edit anchored on
// one line ending matches nothing at all in a file that uses the other. It edits nothing, the
// suite then passes, and that reads as "the change was made and was harmless". Three wrong
// conclusions on this tree came from exactly that, and one of them also rewrote a whole file's
// endings while its own substitution matched nothing, so the evidence pointed two ways at once.
// The sharper form of it is a gate rather than an edit: a scanner anchored on a brace at the
// start of a line finds no match at all in a crlf file, reads the whole file as one body, and
// reports clean having found nothing -- which is exactly what a clean file looks like too.
//
// Judged FILE BY FILE, against the ending .gitattributes PINS for that file rather than against
// whichever ending a package happens to be uniform in. The directory survives as how the report
// is grouped, and as the one question still asked of a package as a whole -- whether all of its
// files are pinned to the SAME ending -- but no longer as what any file's class is measured
// against, which is why the scope below no longer has to know which packages read each other.
//
// This sentence used to say the opposite, and it was right when it was written: which ending was
// correct belonged to the checkout and not to this repository, because a clone with
// core.autocrlf off was lf throughout and one with it on was crlf throughout and both were fine
// to work in. `*.go text eol=lf` ended that. Measured on a fresh clone of the commit that added
// it, with core.autocrlf confirmed live at system scope: all 474 tracked .go files arrived lf,
// while 116 tracked non-.go text files arrived crlf -- which is the control saying autocrlf was
// on for that clone rather than quietly off. So no checkout produces a crlf .go file any more,
// and a package that is uniformly crlf is not a checkout at all: it is a tool having rewritten
// the working tree, which is the one thing this gate exists to catch.
//
// Accepting uniform-crlf cost the gate that whole class. Flipping all 13 files of ../message to
// crlf left this gate PASSING, on the sentence "all 13 source files of ../message end their
// lines crlf", and no other gate in the suite noticed.
//
// The requirement is READ OUT OF .gitattributes rather than written down here, which is
// guardrail 5 pointed at a constant instead of at a list. It also closes the other half of the
// property: deleting `*.go text eol=lf` was invisible to the entire suite, because the pin said
// which ending was right and nothing observed the tree, while this gate observed the tree and
// asked for no particular ending. Neither half was worth anything alone. Now the pin is this
// gate's input, so deleting it leaves every file here pinned to nothing and the gate goes red --
// one mechanism instead of two unguarded halves.
//
// Uniformity is no longer asserted separately because it FALLS OUT: every file of a package is
// held to the ending its own path is pinned to, so a package that passes is uniform by
// construction. A package whose files are pinned to two different endings is reported as that,
// since no ending it could be in would then be uniform.
//
// DERIVED at both ends -- the class off the pin, per file; the scope by lineEndingScanRoots,
// which walks this module and returns every directory holding Go source. Until 2026-09-05 the
// scope was this one directory, and the consequence was measurable: mls was 137 files crlf and
// 0 lf while connect/message had drifted to 6 crlf
// and 7 lf and connect/messagegroup to 5 and 1. mls stayed uniform BECAUSE of this gate, and
// the two packages with no gate are what a package with no gate looks like after a fortnight.
// The review that first raised this named a single file; reading the class off the directory
// found sixteen, which is guardrail 5's point made by the same defect a second time.
//
// The scope then spent part of one day as a closure over "../" path literals, which reached those
// three packages and excluded connect/mls/syntax and connect's own package on a justification
// that contradicted itself. Measured before it was removed: all 23 files of mls/syntax went
// uniformly crlf against 7496 passing tests. Walking the module needs no such argument, and
// needed no exception either -- an exception is what would have said this was a widening
// wearing a simplification's clothes.
//
// None of that was visible in git. core.autocrlf=true cleans on the way in, so every blob of
// all three packages was already lf and `git diff` was empty across the whole drift. The
// working tree is the only place it shows, and the working tree is what an anchored edit reads.
// It is also why `git diff --numstat` is not a landed-check for a line ending edit any more: the
// clean filter converts a crlf back to lf before the diff is computed, so a mutation that really
// is on disk reports an empty numstat. Check the bytes.
//
// The scope is then CHECKED rather than trusted: every file this gate reads is collected as it is
// read, and the set of them is asserted equal to the .go files of this module, enumerated a
// second time by everyGoSourceFileUnder. Until 2026-09-05 the derivation was never checked at
// all -- the gate logged a per root count summing to exactly 474 and asserted on neither half of
// it, so any narrowing that left forbiddenScanRoots reachable passed in silence. Measured:
// inserting `if entry.IsDir() && entry.Name() == "security" { return fs.SkipDir }` into the walk
// AND flipping security/main.go uniformly to crlf on disk ran 7,497 PASS, 0 FAIL, 0 SKIP,
// identical to clean, while the same file flipped without the skip is correctly red. Sets and
// not counts, because a count matches by accident and a set names the file that went missing.
//
// That closes four narrowings at once, which is why it is one assertion and not four rules: a
// directory skipped under a name outside the scope control's four name fixture; a depth limit,
// which that fixture is exactly two deep and cannot see; the two line survivor that roots the
// walk at this package AND drops the forbiddenScanRoots coverage loop, taking the gate from 474
// files to 168 with each half alone caught and the pair not; and the next one, which is the
// actual point.
//
// What it does NOT hold, said here rather than left to be found: a narrowing written into BOTH
// enumerations at once. Every A-equals-B assertion has that residual. It is pushed as far out as
// it goes by giving the two halves nothing in common -- different traversal primitive, different
// question, no shared helper, no shared predicate, and a second half whose comment says it
// exists to have no name test and no depth test in it -- so the second edit has to be written
// deliberately into a function that says why it is empty. It is smaller than it was. It is not
// zero, and this gate is best effort to exactly that extent.
//
// A file carrying no line ending at all belongs to neither class and is counted apart, so an
// empty file cannot make a mixed package look uniform, and a file mixed WITHIN itself is its
// own report: that one is never a checkout and is always a tool that wrote part of a file.
func TestThePackageSourceIsOneLineEndingThroughout(t *testing.T) {
	moduleRoot := moduleRootDir(t)
	roots := lineEndingScanRoots(t)
	t.Logf("the derived scope is %v", roots)
	judged := []string{}
	for _, root := range roots {
		paths := packageSourcePathsIn(t, root)
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
			// recorded where the file is actually READ, and not where the scope was derived, so a
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
				// DECIDED, and what it decided is "no ending at all": a -text or binary rule covering a
				// .go file. That is a different answer from "no rule set mentions this file", and
				// reporting it as that one did two wrong things at once -- it named a rule set that HAD
				// spoken as silent, and then suppressed every remaining file of the root behind the
				// diagnosis. pinnedLineEndingOf has always distinguished the two through its second
				// return; only this caller collapsed them.
				exempted = append(exempted, fmt.Sprintf("%s, by %q", path, decided))
				continue
			}
			// counted against what the REPOSITORY says rather than against what the tree does, so
			// heldTo below is the pin and never a majority vote of the files.
			heldTo[pinned] += 1
			decidedBy[decided] = true
			if carries != pinned {
				wrong = append(wrong, fmt.Sprintf("%s is %s", path, carries))
			}
		}
		// no report short circuits another. Each of these is a different defect about a different
		// set of the root's files, and the `continue` that used to stand under the first is how one
		// -text rule over one .go file would leave every OTHER file of its root unjudged.
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
