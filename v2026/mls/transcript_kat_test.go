// The runner for the mlswg transcript-hashes vector family, number 7.
//
// This is the third family to register against the registry in vectors_test.go, and the
// parts it has in common with the first two are not repeated here: the suite filter is
// implementedSuite, the accounting is vectorRunTally, the comparator control is
// assertComparatorRefuses, the registration assertion is assertVectorFamilyIsInstalled and
// the opaque<V> tail split is splitTrailingOpaqueTag, all of them in vectors_runner_test.go.
// A third independent copy of that machinery would be a third place to rediscover the
// hardening the first one needed.
//
// What is this family's own, and why:
//
//   - the split. The corpus supplies a serialized AuthenticatedContent rather than the two
//     inputs section 8.2 takes. For a commit that is
//     wire_format || FramedContent || signature<V> || confirmation_tag, so
//     ConfirmedTranscriptHashInput is exactly the prefix and the confirmation tag is exactly
//     the trailing opaque<V>. The split is taken at the tail and then CHECKED, twice: the
//     octets in front of the tail must be the codec's own length prefix for a KDF.Nh vector,
//     and the recovered tag must verify as MAC(confirmation_key,
//     confirmed_transcript_hash_after) -- which is the corpus's own stated verification step.
//     A split taken one octet out fails that MAC, so the offset is self-validating rather
//     than assumed. When p6 lands (*AuthenticatedContent).UnmarshalMLS this is replaced by
//     syntax.Unmarshal plus authContent.ConfirmedTranscriptHashInput(), and the MAC check
//     stays.
//   - the aliasing refusal. The three hashes one case publishes -- the interim before, the
//     confirmed after, the interim after -- are all KDF.Nh octets of apparent random, and if
//     any two of them held one value then an implementation that answered the wrong one of
//     the two would agree with the corpus. That is not hypothetical for this family: the
//     confirmed hash and the interim hash are the two values transcript.go's own header says
//     neither can stand in for, and swapping the two assignments in Update is a one line
//     edit that leaves both looking like well formed hashes.
//   - the four answers compared per case: the two free functions, and the two fields Update
//     writes. Both free functions can be right while Update reads the wrong field of the
//     receiver or writes them back transposed, so the stateful path is compared separately
//     rather than assumed to follow.
//   - the epoch-0 base case. Section 8.2 starts the chain at an interim_transcript_hash_before
//     that is the ZERO LENGTH octet string -- exactly what transcript.go's
//     InitialTranscriptHashes hands out -- so the width rule here holds the two ANSWERS to
//     KDF.Nh and lets that one input be empty. What is refused instead is the key being ABSENT,
//     read off a generic decode: an empty value and a missing one decode identically through a
//     struct and only one of them is a vacuous comparison. The vendored corpus publishes no
//     such case at any suite, which is exactly why the rule has to be derived from the section
//     rather than transcribed from the file: transcribed, it refused the one case the whole
//     chain starts from.
//
// What this runner adds over TestTranscriptHashesMatchTheMlswgTranscriptHashes next door,
// stated because a runner that adds no protection of its own is better replaced by a comment.
// Three things. It installs family 7 in the registry, so the corpus is offered to it by
// TestVectorFamiliesVerify and family 7 leaves expectedPendingFamilies. It asserts a literal
// comparison count -- that test derives its own expected count from the suite registry on
// both sides, so a registry that answered nothing would leave it comparing zero cases
// against an expectation of zero. And it holds the comparator to REFUSING: that test can
// only be judged by a corpus that happens to disagree with this implementation, and the
// vendored one agrees with everything.
package mls

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
)

// The accounting that makes this runner unable to pass having compared nothing.
//
// Transcriptions of what testdata/vectors/transcript-hashes.json holds at the pinned mlswg
// commit: seven cases, one at each published ciphersuite, of which the two this package
// registers account for two. Four answers are compared per case, so eight comparisons, and
// they are made against four distinct published strings -- each case publishes one confirmed
// hash and one interim hash, and each of those is compared twice, once through the free
// function and once through Update.
//
// Written down rather than derived, for the reason task 16 gives: deriving the expected count
// with the same filter that is under test is how a filter matching nothing ends up agreeing
// with itself. What IS derived and checked alongside them is that covered plus skipped equals
// the number of cases read, and that the per suite split is the corpus's own.
const (
	transcriptHashKatCovered      = 2
	transcriptHashKatSkipped      = 5
	transcriptHashKatChecks       = 4
	transcriptHashKatComparisons  = transcriptHashKatCovered * transcriptHashKatChecks
	transcriptHashKatDistinct     = 4
	transcriptHashKatFreeFunction = 2
)

// transcriptHashCheckNames is every answer this runner compares for one case, named by what
// produced it. TestTranscriptHashFamilyChecksAreEveryPathToTheEpoch holds the count to the
// field count of TranscriptHashes plus the two free functions, so a third field added to the
// pair cannot arrive without a comparison.
//
// The order is the order the comparator emits them in, and incomplete() holds a run to it
// POSITION BY POSITION as well as requiring each name exactly once. Both halves are load
// bearing and for different reasons: the multiset is what stops a comparison dropped from the
// middle being made up for by another one made twice, and the order is what keeps verdict()'s
// two vacuity controls aimed at the pair they are about. A multiset alone permits a reorder,
// and a reorder is what nothing here refused -- the first two rows were swapped, every name
// still appeared exactly once, and the whole package stayed green with both controls
// permanently pointed at the wrong answers.
var transcriptHashCheckNames = []string{
	"ConfirmedTranscriptHash",
	"InterimTranscriptHash",
	"TranscriptHashes.Update/Confirmed",
	"TranscriptHashes.Update/Interim",
}

// Family 7 is installed here, and 7 is deleted from expectedPendingFamilies in the same
// commit. Without both halves TestVectorFamiliesVerify runs one fewer family and the manifest
// gate stays green while claiming this family is unimplemented.
//
// Generate is nil: the mlswg transcript-hashes format has no generate direction. A case is a
// serialized AuthenticatedContent, which this plan cannot build -- framing is p6's, and
// nothing in slice A2 can serialize a FramedContent. A generator that fabricated a blob would
// be generating its own corpus rather than the one the format describes.
func init() {
	RegisterVectorFamily(VectorFamily{
		Number:   7,
		Name:     "Transcript hashes",
		File:     transcriptHashKatFile,
		Slice:    "A3",
		Verify:   verifyTranscriptHashVector,
		Generate: nil,
	})
}

// The refusals compareTranscriptHashVector makes, as sentinels rather than as formatted
// strings, so a test can require a specific refusal rather than "some error".
//
// They are what makes the comparison observable at all. Every case of the vendored corpus
// agrees with this implementation, so a comparator that checked everything and a comparator
// that checked nothing produce identical runs over it; the only way to tell them apart is to
// hand it an answer that is wrong on purpose and require the matching refusal, which is
// TestCompareTranscriptHashVectorRefusesAnAnswerItShouldNotAccept.
var (
	errTranscriptHashPublishedWidth = errors.New("a published transcript hash is not the suite's KDF.Nh")
	errTranscriptHashAliased        = errors.New("two of the three hashes one case publishes are the same value")
	errTranscriptHashTagRefused     = errors.New("the recovered confirmation tag does not verify as MAC(confirmation_key, confirmed_transcript_hash_after)")
	errTranscriptHashMismatch       = errors.New("a transcript hash does not match the published one")
	errTranscriptHashDidNotMove     = errors.New("perturbing one of the corpus's own inputs left the answer unchanged")
	errTranscriptHashIncomplete     = errors.New("the comparison reports values it cannot have computed")
)

// transcriptHashCheck is one answer this package computed held against one answer the corpus
// published, named by what produced it and filed under the json key the published half lives
// at.
type transcriptHashCheck struct {
	// name is what produced the computed half, and is one of transcriptHashCheckNames.
	name string
	// field is the json key the published half is published under, which the runner
	// re-reads out of a generic decode of the same case.
	field string
	got   []byte
	want  []byte
}

// transcriptHashComparison is what one run of compareTranscriptHashVector PRODUCED, and it is
// the only thing its callers are allowed to judge it by.
//
// The shape is task 16's and it is here for task 16's reason. A comparator returning a bool
// reports that control reached the bottom of the function and not that a comparison happened:
// an early return above it leaves the runner counting cases that never called
// ConfirmedTranscriptHash at all, and the run stays green. Every field below is written at
// the point the work that produces it happens, so a return that skipped the work reports the
// zero value, and a caller that judges the values rather than the fact of returning sees that.
type transcriptHashComparison struct {
	// inScope is true when the case's ciphersuite is one this package registers. A false
	// here is not a failure and not a skip: it is a case with no provider.
	inScope bool
	// hashSize is the suite's KDF.Nh, read off the provider rather than assumed.
	hashSize int
	// the corpus's own three hashes, decoded. All three are held rather than only the two
	// that are answers, because the aliasing refusal is about the three together.
	//
	// before may legitimately be EMPTY: section 8.2's epoch-0 interim hash is the zero-length
	// octet string. publishesBefore is the separate question of whether the corpus published
	// the key at all, read off a generic decode of the same case with no struct tag in the
	// way, because an absent key and an empty value decode to the same []byte through the
	// struct and only one of the two is a comparison that read nothing.
	before          []byte
	publishesBefore bool
	confirmedAfter  []byte
	interimAfter    []byte
	// split is the offset the ConfirmedTranscriptHashInput / confirmation_tag split was
	// taken at, and tag is what was recovered from the tail. A zero split is a comparison
	// that never divided the published blob.
	split int
	tag   []byte
	// verified is the corpus's own verification step: the recovered tag as
	// MAC(confirmation_key, confirmed_transcript_hash_after). It is what makes the split
	// self-validating rather than assumed.
	verified bool
	// checks is every comparison the run made, in the order it made them.
	checks []transcriptHashCheck
	// withoutChain is the confirmed hash recomputed with one octet of the corpus's own
	// interim_transcript_hash_before flipped, and withoutTag the interim hash recomputed
	// with one octet of the recovered tag flipped. Either one equal to the unperturbed
	// answer means that input never reached the derivation.
	withoutChain []byte
	withoutTag   []byte
}

// incomplete reports whether the evidence a compared case must carry is missing or
// inconsistent, without looking at whether any answer was right.
//
// This is the vacuity half, split from the correctness half on purpose. bytes.Equal over two
// empty slices says they agree, so a check whose got or want is empty has compared nothing
// whatever the comparison would say about it -- and a runner that counted such checks would
// report the full eight having derived none of them.
func (self transcriptHashComparison) incomplete() error {
	switch {
	case !self.inScope:
		return fmt.Errorf("%w: the case is out of scope and carries no comparison", errTranscriptHashIncomplete)
	case self.hashSize == 0:
		return fmt.Errorf("%w: no KDF.Nh was read from the provider", errTranscriptHashIncomplete)
	case self.split <= 0:
		return fmt.Errorf("%w: the published authenticated_content was split at offset %d, so no ConfirmedTranscriptHashInput was recovered from it",
			errTranscriptHashIncomplete, self.split)
	case len(self.tag) != self.hashSize:
		return fmt.Errorf("%w: the recovered confirmation tag is %d octets and the suite's KDF.Nh is %d",
			errTranscriptHashIncomplete, len(self.tag), self.hashSize)
	case !self.publishesBefore:
		return fmt.Errorf("%w: the case publishes no interim_transcript_hash_before key at all, so whatever decodes it decodes to nothing and the chain was seeded from a value the corpus never gave",
			errTranscriptHashIncomplete)
	case len(self.confirmedAfter) == 0 || len(self.interimAfter) == 0:
		return fmt.Errorf("%w: the case publishes %d and %d octets for the two hashes it answers, and an empty comparison agrees with anything",
			errTranscriptHashIncomplete, len(self.confirmedAfter), len(self.interimAfter))
	case len(self.checks) != len(transcriptHashCheckNames):
		return fmt.Errorf("%w: the run made %d comparisons and this family owes %d per case",
			errTranscriptHashIncomplete, len(self.checks), len(transcriptHashCheckNames))
	case len(self.withoutChain) != self.hashSize:
		return fmt.Errorf("%w: the flipped interim_transcript_hash_before control was never run", errTranscriptHashIncomplete)
	case len(self.withoutTag) != self.hashSize:
		return fmt.Errorf("%w: the flipped confirmation tag control was never run", errTranscriptHashIncomplete)
	}
	// every name exactly once AND in transcriptHashCheckNames' own order. The count case above
	// makes the index safe. Order because verdict()'s vacuity controls are about a particular
	// answer, and a swap of two rows that leaves every name present exactly once is a swap a
	// multiset cannot see -- it was applied, nothing refused it, and the controls then compared
	// the wrong answer against the wrong perturbation.
	seen := map[string]int{}
	for index, check := range self.checks {
		if check.name != transcriptHashCheckNames[index] {
			return fmt.Errorf("%w: comparison %d is %s and this family emits %s there; the vacuity controls are read out of that order",
				errTranscriptHashIncomplete, index, check.name, transcriptHashCheckNames[index])
		}
		if len(check.got) == 0 || len(check.want) == 0 {
			return fmt.Errorf("%w: %s compared %d computed octets against %d published ones, and an empty comparison agrees with anything",
				errTranscriptHashIncomplete, check.name, len(check.got), len(check.want))
		}
		if check.field == "" {
			return fmt.Errorf("%w: %s names no published field, so nothing independent of the comparator's own decode can re-read it",
				errTranscriptHashIncomplete, check.name)
		}
		seen[check.name]++
	}
	for _, name := range transcriptHashCheckNames {
		if seen[name] != 1 {
			return fmt.Errorf("%w: %s was compared %d times and this family compares it once per case",
				errTranscriptHashIncomplete, name, seen[name])
		}
	}
	if len(seen) != transcriptHashKatChecks {
		return fmt.Errorf("%w: the run compared %d distinct answers and this family compares %d",
			errTranscriptHashIncomplete, len(seen), transcriptHashKatChecks)
	}
	return nil
}

// verdict is the whole judgement over one compared case: it must be complete, every published
// hash must be the suite's width, no two of the three may be the same value, the recovered tag
// must verify, every comparison must agree, and both vacuity controls must have moved.
//
// The order is deliberate. A width failure and an aliasing failure are both statements that
// this is not the comparison the corpus intends, and reporting either as a plain mismatch
// would let a test asking for one of them be satisfied by the other. The tag refusal is ahead
// of the mismatches for the same reason: a split taken at the wrong offset produces four
// disagreements, and reporting them as mismatches would hide the one thing that is actually
// wrong.
func (self transcriptHashComparison) verdict() error {
	if err := self.incomplete(); err != nil {
		return err
	}
	// the two ANSWERS are Hash outputs and are KDF.Nh octets or they are not what the corpus
	// means. The chained INPUT has a second legal width and only one: section 8.2 starts the
	// chain at the zero-length octet string, which is what InitialTranscriptHashes hands out,
	// so a group-creation case is the base case and not a defect. Anything between the two is
	// neither, and is still refused.
	for _, published := range []struct {
		name       string
		body       []byte
		mayBeEmpty bool
	}{
		{"interim_transcript_hash_before", self.before, true},
		{"confirmed_transcript_hash_after", self.confirmedAfter, false},
		{"interim_transcript_hash_after", self.interimAfter, false},
	} {
		if published.mayBeEmpty && len(published.body) == 0 {
			continue
		}
		if len(published.body) != self.hashSize {
			return fmt.Errorf("%w: %s is %d octets against a KDF.Nh of %d, so this is not the comparison the corpus intends",
				errTranscriptHashPublishedWidth, published.name, len(published.body), self.hashSize)
		}
	}
	// the three published hashes are one epoch's chain and all three are KDF.Nh octets of
	// apparent random. Two of them holding one value would be a corpus this comparison
	// cannot tell the confirmed hash from the interim one with, which is precisely the pair
	// transcript.go says neither can stand in for.
	distinct := map[string]string{}
	for _, published := range []struct {
		name string
		body []byte
	}{
		{"interim_transcript_hash_before", self.before},
		{"confirmed_transcript_hash_after", self.confirmedAfter},
		{"interim_transcript_hash_after", self.interimAfter},
	} {
		text := HexOf(published.body)
		if previous, duplicated := distinct[text]; duplicated {
			return fmt.Errorf("%w: %s is published for both %s and %s",
				errTranscriptHashAliased, text, previous, published.name)
		}
		distinct[text] = published.name
	}
	if !self.verified {
		return fmt.Errorf("%w: the tail of the published authenticated_content was split at offset %d",
			errTranscriptHashTagRefused, self.split)
	}
	for _, check := range self.checks {
		if !bytes.Equal(check.got, check.want) {
			return fmt.Errorf("%w: %s = %s, the corpus publishes %s for %s",
				errTranscriptHashMismatch, check.name, HexOf(check.got), HexOf(check.want), check.field)
		}
	}
	// the two vacuity controls. Both are cheap redundancy over the comparisons above for a
	// corpus that agrees, and both are the difference on a case whose published answers were
	// recomputed to match a defective derivation: an implementation that dropped the chained
	// interim hash, or that hashed the confirmed hash without the tag, answers the same value
	// whatever those two inputs hold.
	//
	// Addressed BY NAME and not by position. self.checks[0] is the confirmed hash only while the
	// emit order holds, and an order nothing refuses points both controls at the wrong pair
	// permanently. What refuses that reorder is incomplete(), and that is the half a mutation can
	// see: swap the two rows and it fails, delete the order check and
	// TestTranscriptHashComparisonCannotReportAComparisonItDidNotMake fails. Reading by name here
	// is redundant with it and is stated as redundant rather than as a second guarantee -- with
	// the order enforced the two are equivalent, and replacing these two lines with self.checks[0]
	// and self.checks[1] is a mutation nothing catches. It is here so that a later edit weakening
	// the order check cannot silently re-aim the controls as well.
	confirmedAnswer := self.answerNamed("ConfirmedTranscriptHash")
	interimAnswer := self.answerNamed("InterimTranscriptHash")
	if len(confirmedAnswer) == 0 || len(interimAnswer) == 0 {
		return fmt.Errorf("%w: the vacuity controls are read out of ConfirmedTranscriptHash and InterimTranscriptHash and the run compared %d and %d octets for them",
			errTranscriptHashIncomplete, len(confirmedAnswer), len(interimAnswer))
	}
	if bytes.Equal(self.withoutChain, confirmedAnswer) {
		return fmt.Errorf("%w: one flipped octet of interim_transcript_hash_before left the confirmed hash at %s, so the chained value never reached ConfirmedTranscriptHash",
			errTranscriptHashDidNotMove, HexOf(self.withoutChain))
	}
	if bytes.Equal(self.withoutTag, interimAnswer) {
		return fmt.Errorf("%w: one flipped octet of the confirmation tag left the interim hash at %s, so the tag never reached InterimTranscriptHash",
			errTranscriptHashDidNotMove, HexOf(self.withoutTag))
	}
	return nil
}

// answerNamed is the computed half of one of this case's comparisons, addressed by the name of
// what produced it, or nothing if this run made no such comparison.
//
// By name because the callers are ABOUT a particular answer. A positional read is only that
// answer while the emit order holds, and holding the order in one place and reading positions in
// another is how the two come apart without either one failing.
func (self transcriptHashComparison) answerNamed(name string) []byte {
	for _, check := range self.checks {
		if check.name == name {
			return check.got
		}
	}
	return nil
}

// perturbedChainValue is the corpus's own interim_transcript_hash_before with one bit changed --
// or, for the group-creation base case whose published value is the zero-length octet string
// section 8.2 starts the chain from, a one octet value where the corpus publishes none.
//
// The control is "an input this derivation must depend on, changed", and at epoch 0 there is no
// octet to flip. Refusing that case instead, which is what this runner did, reports the one case
// the whole chain starts from as an incomplete comparison rather than verifying it.
func perturbedChainValue(before []byte) []byte {
	if len(before) == 0 {
		return []byte{0x00}
	}
	flipped := bytes.Clone(before)
	flipped[0] ^= 0x01
	return flipped
}

// verifyTranscriptHashVector is the registry's shim: the signature RegisterVectorFamily needs,
// over the comparator that does the work and reports what it produced.
//
// The split is the whole point, and it is the defect task 16 shipped and then had to fix.
// Verify cannot return anything, so a runner counting calls to it would count a case it
// declined to check exactly as it counts one it compared.
func verifyTranscriptHashVector(t *testing.T, raw json.RawMessage) {
	t.Helper()
	evidence, err := compareTranscriptHashVector(t, raw)
	if err != nil {
		t.Fatalf("transcript-hashes: %v", err)
	}
	if !evidence.inScope {
		return
	}
	if err := evidence.verdict(); err != nil {
		t.Fatalf("transcript-hashes: %v", err)
	}
}

// compareTranscriptHashVector runs one case of transcript-hashes.json and returns what the run
// produced. A case at a ciphersuite v1 does not implement is not a failure and not a skip: it
// comes back with inScope false and nothing else set.
//
// A corpus that will not parse or will not hex decode is fatal here rather than returned,
// because it is not a verdict about this implementation -- it is the evidence itself being
// unreadable, and every family in this package treats that as the loudest failure there is.
// Everything that IS a verdict about this implementation is returned, so a caller can require
// a refusal instead of hoping the corpus disagrees with a defect.
//
// The interim hash is computed from OUR confirmed hash rather than from the published one, and
// so is Update's, because that is the chain a group actually runs: an implementation whose
// confirmed hash is wrong has an interim hash that is wrong for the same reason, and reseeding
// from the corpus between the two would hide it.
func compareTranscriptHashVector(t *testing.T, raw json.RawMessage) (transcriptHashComparison, error) {
	t.Helper()
	// trPublishedEntry is transcript_test.go's row for this same file, reused rather than
	// declared again: three declarations of one corpus row in one package is how two of them
	// end up disagreeing about which json key an answer lives at.
	entry := trPublishedEntry{}
	if err := json.Unmarshal(raw, &entry); err != nil {
		t.Fatalf("parse transcript-hashes case: %v", err)
	}
	suite, ok := implementedSuite(entry.CipherSuite)
	if !ok {
		return transcriptHashComparison{}, nil
	}
	crypto, err := NewCryptoProvider(suite)
	if err != nil {
		t.Fatalf("NewCryptoProvider(%#04x): %v", uint16(suite), err)
	}

	// whether the chained interim hash was PUBLISHED, read off a generic decode of the same
	// bytes with no struct tag in the way, and under the key the struct tag itself names rather
	// than a second spelling of it. An absent key and an empty value decode identically through
	// the struct: the empty octet string is section 8.2's epoch-0 value and the absent key is
	// what a renamed field or a struct tag typo produces, and only the second is vacuous.
	published := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &published); err != nil {
		t.Fatalf("parse transcript-hashes case as a json object: %v", err)
	}
	_, publishesBefore := published[theJsonKeyOf(t, trPublishedEntry{}, "InterimTranscriptHashBefore")]

	evidence := transcriptHashComparison{
		inScope:         true,
		hashSize:        crypto.HashSize(),
		before:          MustHex(t, entry.InterimTranscriptHashBefore),
		publishesBefore: publishesBefore,
		confirmedAfter:  MustHex(t, entry.ConfirmedTranscriptHashAfter),
		interimAfter:    MustHex(t, entry.InterimTranscriptHashAfter),
	}
	confirmationKey := MustHex(t, entry.ConfirmationKey)
	blob := MustHex(t, entry.AuthenticatedContent)

	confirmedInput, confirmationTag, err := splitTrailingOpaqueTag(blob, evidence.hashSize)
	if err != nil {
		return evidence, fmt.Errorf("the published authenticated_content: %w", err)
	}
	evidence.split = len(confirmedInput)
	evidence.tag = bytes.Clone(confirmationTag)
	// the corpus's own verification step, and what makes the split honest rather than
	// assumed: a split taken at the wrong offset recovers bytes this MAC refuses. Guardrail 8
	// -- the comparison is CryptoProvider.MacVerify and nothing spelled out here.
	evidence.verified = crypto.MacVerify(confirmationKey, evidence.confirmedAfter, confirmationTag)

	confirmed := ConfirmedTranscriptHash(crypto, evidence.before, confirmedInput)
	interim, err := InterimTranscriptHash(crypto, confirmed, confirmationTag)
	if err != nil {
		return evidence, fmt.Errorf("InterimTranscriptHash: %w", err)
	}
	// the same epoch through the stateful API a group drives, seeded at the published interim
	// hash. Both free functions can be right while Update reads the wrong field of the
	// receiver or writes the two back transposed.
	hashes := &TranscriptHashes{Confirmed: nil, Interim: bytes.Clone(evidence.before)}
	if err := hashes.Update(crypto, confirmedInput, confirmationTag); err != nil {
		return evidence, fmt.Errorf("TranscriptHashes.Update: %w", err)
	}

	// the order here is transcriptHashCheckNames' order, position by position, which
	// incomplete() holds it to.
	for _, check := range []transcriptHashCheck{
		{"ConfirmedTranscriptHash", "confirmed_transcript_hash_after", confirmed, evidence.confirmedAfter},
		{"InterimTranscriptHash", "interim_transcript_hash_after", interim, evidence.interimAfter},
		{"TranscriptHashes.Update/Confirmed", "confirmed_transcript_hash_after", hashes.Confirmed, evidence.confirmedAfter},
		{"TranscriptHashes.Update/Interim", "interim_transcript_hash_after", hashes.Interim, evidence.interimAfter},
	} {
		check.got = bytes.Clone(check.got)
		evidence.checks = append(evidence.checks, check)
	}

	// the two vacuity controls, over the corpus's own bytes.
	evidence.withoutChain = ConfirmedTranscriptHash(crypto, perturbedChainValue(evidence.before), confirmedInput)

	flippedTag := bytes.Clone(confirmationTag)
	flippedTag[0] ^= 0x01
	perturbedInterim, err := InterimTranscriptHash(crypto, confirmed, flippedTag)
	if err != nil {
		return evidence, fmt.Errorf("InterimTranscriptHash over the flipped tag: %w", err)
	}
	evidence.withoutTag = perturbedInterim

	return evidence, evidence.verdict()
}

// TestVectorTranscriptHashes is vector family 7 over the published corpus.
//
// Every assertion the tally makes after the loop exists because the loop can be made to run
// zero times without anything else in this package noticing. A filter that matched nothing, a
// filter that matched all seven published suites, a corpus that parsed to an empty array, a
// comparator that declined every case: each of those is a green run of this test with the
// accounting removed, and a failure with it.
//
// What the loop counts is not calls that returned. It counts comparisons whose computed half
// this runner itself re-checked against a GENERIC decode of the corpus text -- no struct tag
// in the way -- so a comparator that answered without computing anything is a failure here
// rather than a number that looks right.
func TestVectorTranscriptHashes(t *testing.T) {
	tally, entries := newVectorRunTally(t, transcriptHashKatFile)
	for index, raw := range entries {
		published := map[string]json.RawMessage{}
		if err := json.Unmarshal(raw, &published); err != nil {
			t.Fatalf("%s case %d: %v", transcriptHashKatFile, index, err)
		}
		header := struct {
			CipherSuite uint16 `json:"cipher_suite"`
		}{}
		if err := json.Unmarshal(raw, &header); err != nil {
			t.Fatalf("%s case %d: %v", transcriptHashKatFile, index, err)
		}
		suite, inScope := tally.filter(header.CipherSuite)
		if !inScope {
			continue
		}
		evidence, err := compareTranscriptHashVector(t, raw)
		if err != nil {
			t.Fatalf("%s case %d (suite %#04x): %v", transcriptHashKatFile, index, header.CipherSuite, err)
		}
		tally.requireCompared(t, index, suite, evidence.inScope)
		if err := evidence.verdict(); err != nil {
			t.Fatalf("%s case %d (suite %#04x): %v", transcriptHashKatFile, index, header.CipherSuite, err)
		}
		for _, check := range evidence.checks {
			want := publishedCorpusField(t, published, check.field)
			if got := HexOf(check.got); got != want {
				t.Fatalf("%s case %d (suite %#04x): %s answered %s, the corpus publishes %s for %s",
					transcriptHashKatFile, index, header.CipherSuite, check.name, got, want, check.field)
			}
			tally.answer(want)
		}
	}
	tally.assertRun(t, transcriptHashKatCovered, transcriptHashKatSkipped,
		transcriptHashKatComparisons, transcriptHashKatDistinct)
}

// TestTranscriptHashFamilyIsInstalled is the registration half of task 20.
//
// Family 7 declares no generator and that is asserted as an absence rather than left
// unmentioned, so a generator added to this row without a test that holds it to anything is a
// failure here.
func TestTranscriptHashFamilyIsInstalled(t *testing.T) {
	assertVectorFamilyIsInstalled(t, 7, transcriptHashKatFile, verifyTranscriptHashVector, nil)
}

// TestTranscriptHashFamilyChecksAreEveryPathToTheEpoch holds the four answers this family
// compares to the number of ways this package can produce them, so a path added to the
// transcript surface cannot arrive without a comparison.
//
// The stateful half is DERIVED from TranscriptHashes by reflection rather than counted here:
// its fields are exactly the values Update writes, and Update writing one of them correctly
// while leaving the other stale is the failure this family exists to see. A third field added
// to that pair fails here rather than leaving this family comparing three of four answers and
// reporting a clean run.
func TestTranscriptHashFamilyChecksAreEveryPathToTheEpoch(t *testing.T) {
	if len(transcriptHashCheckNames) != transcriptHashKatChecks {
		t.Fatalf("this family names %d checks per case and the count it asserts is %d",
			len(transcriptHashCheckNames), transcriptHashKatChecks)
	}
	distinct := slices.Compact(slices.Sorted(slices.Values(transcriptHashCheckNames)))
	if len(distinct) != len(transcriptHashCheckNames) {
		t.Fatalf("the check names hold %d distinct entries out of %d, so one is compared twice and another not at all",
			len(distinct), len(transcriptHashCheckNames))
	}
	pair := reflect.TypeOf(TranscriptHashes{})
	if want := pair.NumField() + transcriptHashKatFreeFunction; len(transcriptHashCheckNames) != want {
		t.Fatalf("this family compares %d answers per case and an epoch has %d paths (%d fields of %s that Update writes, plus %d free functions): %v",
			len(transcriptHashCheckNames), want, pair.NumField(), pair.Name(),
			transcriptHashKatFreeFunction, transcriptHashCheckNames)
	}
	// and the names really are the two free functions and the two fields, so the count above
	// cannot be satisfied by four comparisons of one thing.
	for _, required := range []string{"ConfirmedTranscriptHash", "InterimTranscriptHash"} {
		if !slices.Contains(transcriptHashCheckNames, required) {
			t.Fatalf("the check names %v do not hold %s", transcriptHashCheckNames, required)
		}
	}
	for index := 0; index < pair.NumField(); index++ {
		name := "TranscriptHashes.Update/" + pair.Field(index).Name
		if !slices.Contains(transcriptHashCheckNames, name) {
			t.Fatalf("%s is a field of %s and %v does not compare it, so Update could leave it stale",
				pair.Field(index).Name, pair.Name(), transcriptHashCheckNames)
		}
	}
}

// trKatBaseCase answers a published case at a registered suite, together with the encoder the
// controls below corrupt it through.
//
// The base is the corpus's own, not a fixture: the whole of what the refusals below mean is
// that this exact case is accepted and a one octet edit of it is not.
func trKatBaseCase(t *testing.T) (trPublishedEntry, func(trPublishedEntry) json.RawMessage) {
	t.Helper()
	base := trPublishedEntry{}
	found := false
	for _, raw := range LoadVectorFile(t, transcriptHashKatFile) {
		candidate := trPublishedEntry{}
		if err := json.Unmarshal(raw, &candidate); err != nil {
			t.Fatalf("parse a transcript-hashes case: %v", err)
		}
		if _, ok := implementedSuite(candidate.CipherSuite); ok {
			base, found = candidate, true
			break
		}
	}
	if !found {
		t.Fatal("no published case is at a registered suite, so this control has nothing to corrupt")
	}
	encode := func(entry trPublishedEntry) json.RawMessage {
		body, err := json.Marshal(entry)
		if err != nil {
			t.Fatalf("marshal the case under test: %v", err)
		}
		return body
	}
	return base, encode
}

// TestCompareTranscriptHashVectorRefusesAnAnswerItShouldNotAccept is the control the runner
// cannot be: it hands the comparator cases that are wrong in each of the ways the corpus is
// not, and requires the matching refusal.
//
// Each case below is a real defect class of this family -- a hash that is not the published
// one, a hash of the wrong width, two of the three published hashes holding one value, a
// confirmation key the tag was not computed under, a tag corrupted at the tail, a split taken
// somewhere that is not a tag -- and each names the sentinel it owes, so a refusal for the
// wrong reason is a failure too.
func TestCompareTranscriptHashVectorRefusesAnAnswerItShouldNotAccept(t *testing.T) {
	base, encode := trKatBaseCase(t)
	flipHex := func(text string) string {
		octets := MustHex(t, text)
		if len(octets) == 0 {
			t.Fatalf("nothing to flip in %q", text)
		}
		octets[0] ^= 0x01
		return HexOf(octets)
	}
	// the unmodified case must carry a real comparison, not merely return without error: a
	// comparator that answered an empty struct would satisfy assertComparatorRefuses'
	// acceptance step and every refusal below.
	evidence, err := compareTranscriptHashVector(t, encode(base))
	if err != nil {
		t.Fatalf("the unmodified published case was refused: %v", err)
	}
	if !evidence.inScope || len(evidence.checks) != transcriptHashKatChecks {
		t.Fatalf("the unmodified published case produced %d comparisons and inScope=%v, want %d and true",
			len(evidence.checks), evidence.inScope, transcriptHashKatChecks)
	}
	if !evidence.verified || evidence.split <= 0 {
		t.Fatalf("the unmodified published case split at %d and verified=%v, so the split below is not the one under test",
			evidence.split, evidence.verified)
	}

	// a case at a suite this package does not register is declined and is NOT a refusal: the
	// comparator has no provider for it, and turning that into an error would make five of
	// the seven published cases failures.
	outOfScope := base
	outOfScope.CipherSuite = 2
	if _, ok := implementedSuite(outOfScope.CipherSuite); ok {
		t.Fatal("suite 0x0002 is registered, so the out of scope case below is not out of scope")
	}
	declined, err := compareTranscriptHashVector(t, encode(outOfScope))
	if err != nil {
		t.Fatalf("a case at an unimplemented suite was refused rather than declined: %v", err)
	}
	if declined.inScope {
		t.Fatal("a case at an unimplemented suite came back in scope")
	}

	blob := MustHex(t, base.AuthenticatedContent)
	prefix, err := vectorLengthPrefix(evidence.hashSize)
	if err != nil {
		t.Fatalf("the length prefix a %d octet tag carries: %v", evidence.hashSize, err)
	}
	corruptBlob := func(at int, edit func(byte) byte) string {
		copied := bytes.Clone(blob)
		copied[at] = edit(copied[at])
		return HexOf(copied)
	}
	tagAt := len(blob) - evidence.hashSize
	prefixAt := tagAt - len(prefix)

	insideTheTag := base
	insideTheTag.AuthenticatedContent = corruptBlob(tagAt, func(b byte) byte { return b ^ 0x01 })

	insideTheInput := base
	insideTheInput.AuthenticatedContent = corruptBlob(0, func(b byte) byte { return b ^ 0x01 })

	wrongPrefix := base
	wrongPrefix.AuthenticatedContent = corruptBlob(prefixAt, func(b byte) byte { return b + 1 })

	tooShort := base
	tooShort.AuthenticatedContent = HexOf(blob[:evidence.hashSize])

	aliased := base
	aliased.InterimTranscriptHashAfter = base.ConfirmedTranscriptHashAfter

	narrow := base
	narrow.ConfirmedTranscriptHashAfter = base.ConfirmedTranscriptHashAfter[:len(base.ConfirmedTranscriptHashAfter)-2]

	// the chained input has two legal widths -- KDF.Nh, or empty at epoch 0 -- and this is
	// neither, so it is still a width refusal and not a mismatch.
	narrowBefore := base
	narrowBefore.InterimTranscriptHashBefore = base.InterimTranscriptHashBefore[:len(base.InterimTranscriptHashBefore)-2]

	wrongConfirmed := base
	wrongConfirmed.ConfirmedTranscriptHashAfter = flipHex(base.ConfirmedTranscriptHashAfter)

	wrongInterim := base
	wrongInterim.InterimTranscriptHashAfter = flipHex(base.InterimTranscriptHashAfter)

	wrongBefore := base
	wrongBefore.InterimTranscriptHashBefore = flipHex(base.InterimTranscriptHashBefore)

	wrongKey := base
	wrongKey.ConfirmationKey = flipHex(base.ConfirmationKey)

	assertComparatorRefuses(t, "transcript-hashes",
		func(t *testing.T, raw json.RawMessage) error {
			_, err := compareTranscriptHashVector(t, raw)
			return err
		},
		encode(base),
		[]comparatorRefusal{
			// the confirmed hash is what the confirmation tag is a MAC over, so the corpus's
			// own verification step is what sees a flipped octet of it first
			{"one flipped octet of the published confirmed_transcript_hash_after", encode(wrongConfirmed), errTranscriptHashTagRefused},
			{"one flipped octet of the published confirmation_key", encode(wrongKey), errTranscriptHashTagRefused},
			{"one flipped octet inside the confirmation tag at the tail of authenticated_content", encode(insideTheTag), errTranscriptHashTagRefused},
			{"one flipped octet of the published interim_transcript_hash_after", encode(wrongInterim), errTranscriptHashMismatch},
			{"one flipped octet of the published interim_transcript_hash_before", encode(wrongBefore), errTranscriptHashMismatch},
			{"one flipped octet of the ConfirmedTranscriptHashInput prefix of authenticated_content", encode(insideTheInput), errTranscriptHashMismatch},
			{"an authenticated_content whose trailing length prefix is not the tag's width", encode(wrongPrefix), errVectorTagTail},
			{"an authenticated_content with nothing in front of its tag", encode(tooShort), errVectorTagTail},
			{"the confirmed hash published as the interim hash", encode(aliased), errTranscriptHashAliased},
			{"a published confirmed_transcript_hash_after one octet short of KDF.Nh", encode(narrow), errTranscriptHashPublishedWidth},
			{"a published interim_transcript_hash_before that is neither KDF.Nh nor the empty epoch-0 value", encode(narrowBefore), errTranscriptHashPublishedWidth},
		})
}

// TestTranscriptHashComparisonCannotReportAComparisonItDidNotMake is the control on the
// evidence struct itself: a return that skipped the work must be refused on every caller's
// path rather than counted as a comparison that agreed.
func TestTranscriptHashComparisonCannotReportAComparisonItDidNotMake(t *testing.T) {
	const nh = 32
	octets := func(seed byte) []byte { return bytes.Repeat([]byte{seed}, nh) }
	full := transcriptHashComparison{
		inScope:         true,
		hashSize:        nh,
		before:          octets(0x01),
		publishesBefore: true,
		confirmedAfter:  octets(0x02),
		interimAfter:    octets(0x03),
		split:           7,
		tag:             octets(0x04),
		verified:        true,
		checks: []transcriptHashCheck{
			{"ConfirmedTranscriptHash", "confirmed_transcript_hash_after", octets(0x02), octets(0x02)},
			{"InterimTranscriptHash", "interim_transcript_hash_after", octets(0x03), octets(0x03)},
			{"TranscriptHashes.Update/Confirmed", "confirmed_transcript_hash_after", octets(0x02), octets(0x02)},
			{"TranscriptHashes.Update/Interim", "interim_transcript_hash_after", octets(0x03), octets(0x03)},
		},
		withoutChain: octets(0x05),
		withoutTag:   octets(0x06),
	}
	if err := full.verdict(); err != nil {
		t.Fatalf("a complete and agreeing comparison was refused: %v; every case below would then pass for the wrong reason", err)
	}

	without := func(edit func(*transcriptHashComparison)) transcriptHashComparison {
		partial := full
		partial.before = bytes.Clone(full.before)
		partial.confirmedAfter = bytes.Clone(full.confirmedAfter)
		partial.interimAfter = bytes.Clone(full.interimAfter)
		partial.tag = bytes.Clone(full.tag)
		partial.withoutChain = bytes.Clone(full.withoutChain)
		partial.withoutTag = bytes.Clone(full.withoutTag)
		partial.checks = slices.Clone(full.checks)
		for index, check := range partial.checks {
			check.got = bytes.Clone(check.got)
			check.want = bytes.Clone(check.want)
			partial.checks[index] = check
		}
		edit(&partial)
		return partial
	}
	// the group-creation base case, which must be ACCEPTED and not counted among the vacuous
	// ones below: section 8.2's epoch-0 interim_transcript_hash_before IS the zero-length octet
	// string, and a comparison whose only difference from an accepted one is that it sits at
	// epoch 0 is a comparison, not an absence.
	base := without(func(c *transcriptHashComparison) { c.before = []byte{} })
	if err := base.verdict(); err != nil {
		t.Fatalf("the group-creation base case was refused: %v; the one case the chain starts from is not a defect", err)
	}

	for _, missing := range []struct {
		name string
		edit func(*transcriptHashComparison)
	}{
		{"a comparison that returned before anything was set", func(c *transcriptHashComparison) { *c = transcriptHashComparison{} }},
		{"in scope and nothing else", func(c *transcriptHashComparison) { *c = transcriptHashComparison{inScope: true} }},
		{"no KDF.Nh read from the provider", func(c *transcriptHashComparison) { c.hashSize = 0 }},
		{"a published blob that was never split", func(c *transcriptHashComparison) { c.split = 0 }},
		{"no confirmation tag recovered", func(c *transcriptHashComparison) { c.tag = nil }},
		{"a confirmation tag of the wrong width", func(c *transcriptHashComparison) { c.tag = c.tag[:nh-1] }},
		{"a published hash that decoded to nothing", func(c *transcriptHashComparison) { c.interimAfter = nil }},
		{"one comparison short of the case", func(c *transcriptHashComparison) { c.checks = c.checks[:len(c.checks)-1] }},
		{"one comparison made twice in place of another", func(c *transcriptHashComparison) { c.checks[1] = c.checks[0] }},
		{"a computed value that was never derived", func(c *transcriptHashComparison) { c.checks[2].got = nil }},
		{"a published value that decoded to nothing", func(c *transcriptHashComparison) { c.checks[2].want = nil }},
		{"a comparison naming no published field", func(c *transcriptHashComparison) { c.checks[3].field = "" }},
		{"no flipped interim_transcript_hash_before control", func(c *transcriptHashComparison) { c.withoutChain = nil }},
		{"no flipped confirmation tag control", func(c *transcriptHashComparison) { c.withoutTag = nil }},
		{"a case that does not publish interim_transcript_hash_before at all", func(c *transcriptHashComparison) {
			c.publishesBefore = false
		}},
		{"the four comparisons emitted in some other order", func(c *transcriptHashComparison) {
			c.checks[0], c.checks[1] = c.checks[1], c.checks[0]
		}},
	} {
		partial := without(missing.edit)
		err := partial.verdict()
		if err == nil {
			t.Errorf("%s was accepted as a comparison", missing.name)
			continue
		}
		if !errors.Is(err, errTranscriptHashIncomplete) {
			t.Errorf("%s was refused as %v, want an incompleteness", missing.name, err)
		}
	}

	// and the correctness half, which incompleteness must not be standing in for.
	for _, wrong := range []struct {
		name string
		edit func(*transcriptHashComparison)
		want error
	}{
		{"a published hash one octet short of KDF.Nh", func(c *transcriptHashComparison) {
			c.confirmedAfter = c.confirmedAfter[:nh-1]
		}, errTranscriptHashPublishedWidth},
		{"a published interim_transcript_hash_before that is neither KDF.Nh nor empty", func(c *transcriptHashComparison) {
			c.before = c.before[:nh-1]
		}, errTranscriptHashPublishedWidth},
		{"the interim hash published as the confirmed hash", func(c *transcriptHashComparison) {
			c.interimAfter = bytes.Clone(c.confirmedAfter)
		}, errTranscriptHashAliased},
		{"a confirmation tag that did not verify", func(c *transcriptHashComparison) {
			c.verified = false
		}, errTranscriptHashTagRefused},
		{"a computed value that disagrees", func(c *transcriptHashComparison) {
			c.checks[2].got[0] ^= 0x01
		}, errTranscriptHashMismatch},
		{"a flipped interim_transcript_hash_before that left the confirmed hash where it was", func(c *transcriptHashComparison) {
			c.withoutChain = bytes.Clone(c.checks[0].got)
		}, errTranscriptHashDidNotMove},
		{"a flipped confirmation tag that left the interim hash where it was", func(c *transcriptHashComparison) {
			c.withoutTag = bytes.Clone(c.checks[1].got)
		}, errTranscriptHashDidNotMove},
	} {
		err := without(wrong.edit).verdict()
		if err == nil {
			t.Errorf("%s was accepted as a comparison", wrong.name)
			continue
		}
		if !errors.Is(err, wrong.want) {
			t.Errorf("%s was judged %v, want %v", wrong.name, err, wrong.want)
		}
	}
}

// TestCompareTranscriptHashVectorRunsTheGroupCreationBaseCase is the epoch-0 half of this
// family, end to end, over a case the vendored corpus does not publish.
//
// Section 8.2 starts the chain at an interim_transcript_hash_before that is the ZERO LENGTH
// octet string -- transcript.go's InitialTranscriptHashes hands out exactly that -- and this
// runner used to refuse that case twice over. incomplete() read an empty published hash as a
// vacuous decode, and verdict() held all three published hashes to KDF.Nh. Both rules were
// transcribed from the corpus that happens to be vendored -- all seven of its cases publish a
// full width value -- rather than derived from the section, so the one case the whole chain
// starts from would have been reported as an incomplete comparison rather than verified.
//
// The case is CONSTRUCTED here, and constructing it is honest in a way generating a corpus case
// would not be: nothing about this implementation's answers is asserted from this implementation.
// What is asserted is that a case whose only difference from a published one is the epoch it sits
// at is COMPARED -- four comparisons, both vacuity controls run -- rather than refused. The head
// of the blob is opaque octets on purpose: what section 8.2 hashes is the serialized
// ConfirmedTranscriptHashInput and this runner never parses it, so the framing p6 owns is not
// what is under test here.
//
// The absent-key half is asserted beside it, because permitting the empty value is only safe if
// "published as empty" and "not published at all" stay two different things.
func TestCompareTranscriptHashVectorRunsTheGroupCreationBaseCase(t *testing.T) {
	suite := Suites()[0]
	crypto, err := NewCryptoProvider(suite)
	if err != nil {
		t.Fatalf("NewCryptoProvider(%#04x): %v", uint16(suite), err)
	}
	nh := crypto.HashSize()
	prefix, err := vectorLengthPrefix(nh)
	if err != nil {
		t.Fatalf("the length prefix a %d octet tag carries: %v", nh, err)
	}
	confirmationKey := bytes.Repeat([]byte{0x5a}, nh)
	confirmedInput := []byte{0x01, 0x02, 0x03, 0x04}

	confirmed := ConfirmedTranscriptHash(crypto, []byte{}, confirmedInput)
	tag := crypto.Mac(confirmationKey, confirmed)
	interim, err := InterimTranscriptHash(crypto, confirmed, tag)
	if err != nil {
		t.Fatalf("InterimTranscriptHash: %v", err)
	}
	entry := trPublishedEntry{
		CipherSuite:                  uint16(suite),
		ConfirmationKey:              HexOf(confirmationKey),
		AuthenticatedContent:         HexOf(slices.Concat(confirmedInput, prefix, tag)),
		InterimTranscriptHashBefore:  "",
		ConfirmedTranscriptHashAfter: HexOf(confirmed),
		InterimTranscriptHashAfter:   HexOf(interim),
	}
	body, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal the base case: %v", err)
	}

	evidence, err := compareTranscriptHashVector(t, json.RawMessage(body))
	if err != nil {
		t.Fatalf("the group-creation base case was refused: %v", err)
	}
	if !evidence.inScope {
		t.Fatal("the group-creation base case came back out of scope")
	}
	if len(evidence.before) != 0 || !evidence.publishesBefore {
		t.Fatalf("the base case was read with %d octets of chained interim hash and publishesBefore=%v, want 0 and true",
			len(evidence.before), evidence.publishesBefore)
	}
	if len(evidence.checks) != transcriptHashKatChecks || !evidence.verified {
		t.Fatalf("the base case produced %d comparisons and verified=%v, want %d and true",
			len(evidence.checks), evidence.verified, transcriptHashKatChecks)
	}
	// the chained control ran and MOVED, which is the assertion the width refusal used to
	// stand in for: an empty published value is still an input the derivation must depend on.
	if len(evidence.withoutChain) != nh || bytes.Equal(evidence.withoutChain, evidence.answerNamed("ConfirmedTranscriptHash")) {
		t.Fatalf("the chained control over an empty interim_transcript_hash_before answered %s against a confirmed hash of %s",
			HexOf(evidence.withoutChain), HexOf(evidence.answerNamed("ConfirmedTranscriptHash")))
	}
	if err := evidence.verdict(); err != nil {
		t.Fatalf("the group-creation base case was judged %v", err)
	}

	// and the same case with the key REMOVED rather than published empty. It decodes to the
	// same []byte through the struct and it is not the same thing: this is what a renamed field
	// or a struct tag typo produces, and it is still refused as a comparison that read nothing.
	generic := map[string]json.RawMessage{}
	if err := json.Unmarshal(body, &generic); err != nil {
		t.Fatalf("decode the base case generically: %v", err)
	}
	key := theJsonKeyOf(t, trPublishedEntry{}, "InterimTranscriptHashBefore")
	if _, published := generic[key]; !published {
		t.Fatalf("the base case does not publish %q, so removing it below removes nothing", key)
	}
	delete(generic, key)
	absent, err := json.Marshal(generic)
	if err != nil {
		t.Fatalf("re-encode the base case without %q: %v", key, err)
	}
	if _, err := compareTranscriptHashVector(t, json.RawMessage(absent)); !errors.Is(err, errTranscriptHashIncomplete) {
		t.Fatalf("a case publishing no %q was judged %v, want an incompleteness; an absent key and an empty value are not the same claim",
			key, err)
	}
}
