// Runners for the mlswg key schedule and psk_secret vector families.
//
// This file lands vector family 6, psk_secret.json, and the suite filter every runner in
// this file will share. Family 6 is retained even though PSK proposals are refused by the
// v1 profile: psk_secret is computed on every epoch as the empty case, and the non-empty
// cases are the only check in this system on the PSKLabel encoding and on the argument
// order of the section 8.4 recurrence.
//
// What this file is defending against, stated once because every assertion below is an
// instance of it: a vector runner that ran nothing reports exactly what a vector runner
// that passed everything reports. Five separate things stand in the way.
//
//   - The comparator returns the values it derived rather than a bool, and its callers
//     judge those values. This is the one that had to be rewritten. A bool whose last
//     statement is `return true` reports that control reached the bottom of the function,
//     not that a comparison happened, so an early return anywhere above it left the count
//     reading 22 with PskSecret never called; and moving that bool one stack frame out of
//     the loop, which the first fix did, keeps the shape and only changes where it lies.
//     A value that must be KDF.Nh octets wide, must have moved when the corpus's own psk
//     moved, and must equal the published answer cannot be produced by skipping the work.
//   - The suites the filter matched are compared against the ciphersuite registry rather
//     than against a list written here, so a filter that matched nothing, and a filter
//     that matched all seven published suites, both fail. The per suite split is checked
//     too, against what the corpus itself publishes at each suite, so 21 comparisons at
//     one registered suite and one at the other is a failure rather than a total of 22.
//   - The corpus is loaded through LoadVectorFile, which is fatal and never skipping, and
//     no runner file may name a skip at all; TestNoVectorRunnerCanSkip derives the skip
//     class from the testing package rather than listing it.
//   - Each comparison carries its own vacuity control: the published answer for a
//     non-empty list must differ from the empty answer, and one flipped octet of the
//     corpus's own psk must move the value this package computes. Both fail if the
//     corpus data never reached the derivation.
//   - The comparator is required to REFUSE. Every case in the vendored corpus agrees with
//     this implementation, so a comparator that checked everything and a comparator that
//     checked nothing produce identical runs over it; the only way to tell them apart is
//     to hand the comparator an answer that is wrong on purpose and require the matching
//     refusal, which is what TestComparePskSecretVectorRefusesAnAnswerItShouldNotAccept
//     does over four defect classes.
//
// And the generate direction. The published corpus never passes through our own encoder,
// so verification alone cannot see an encoder and a decoder that are wrong in the same
// direction. Generating a case and verifying it does -- but only if the generator is not
// the verifier under another name, which is the trap: PskSecret generating an answer that
// PskSecret then checks proves nothing about conformance at all. The generator here is
// independentPskSecret, written from RFC 5869 and RFC 9420 with crypto/hmac and nothing
// this package declares, held against the published corpus, and held structurally to
// touching no production function by TestTheGenerateDirectionSharesNoCodePathWithVerify.
// That gate walks the closure over EVERY test file of this package, not the vector runner
// files alone: a function it cannot see is a leaf whose body it never enters, so a narrower
// file class is an escape hatch one hop wide -- route the answer through a helper declared
// in psk_test.go and the whole production closure is laundered.
package mls

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect/mls/syntax"
)

// The family, and the accounting that makes its runner unable to pass having compared
// nothing.
//
// The three counts are transcriptions of what testdata/vectors/psk_secret.json holds at
// the pinned mlswg commit: 77 entries, eleven at each of the seven published ciphersuites,
// of which the two this package registers account for 22, and those 22 carry 21 distinct
// answers because the two empty lists both answer the all zero string. They are written
// down rather than derived because deriving the expected count with the same filter under
// test is how a filter that matched nothing ends up agreeing with itself.
//
// What IS derived, and checked alongside them, is that the two counts partition the file:
// compared plus skipped must equal the number of entries read, so an entry silently
// dropped by neither branch fails here rather than shrinking the run.
const (
	pskSecretKatFile            = "psk_secret.json"
	pskSecretKatComparisons     = 22
	pskSecretKatSkipped         = 55
	pskSecretKatDistinctAnswers = 21
)

// implementedSuite maps a vector's cipher_suite field to a provider this package
// implements. The published files cover suites 1 through 7 and v1 registers 0x0001 and
// 0x0003, so five of the seven are skipped.
//
// The answer is read out of the ciphersuite registry rather than written as a switch over
// the two code points. A switch would be a second, private opinion about which suites this
// package implements, and the failure it produces is the quiet one: register a third suite
// and every vector family in this package keeps skipping it, reporting a clean run over a
// corpus it silently stopped covering. TestImplementedSuiteIsTheRegistryAndNotAList sweeps
// the whole uint16 space against the registry, so the two cannot drift.
//
// The signature is the interface registry's, taking a uint16 because that is the type the
// vector files decode into, and returning a CipherSuite because that is what every caller
// needs next. Tasks 17, 18, 20 and 25 consume it.
func implementedSuite(suite uint16) (CipherSuite, bool) {
	candidate := CipherSuite(suite)
	if !IsRegisteredSuite(candidate) {
		return 0, false
	}
	return candidate, true
}

// pskSecretKatVector is one entry of psk_secret.json. Binary fields are hex strings in the
// file and stay strings here: MustHex is the single decoder, HexOf the single encoder, and
// a struct holding []byte would need a second one at the json boundary.
//
// The name carries the Kat prefix because psk_test.go already reads this file for the
// ValSem401 acceptance sweep and declares pskSecretVector for it. Two names for one shape
// in one package is worse than one, but the alternative here is worse still: the two
// structs are read by two different plans for two different reasons, and collapsing them
// would put this task's registry-facing decode inside a file the registry does not know
// about.
type pskSecretKatVector struct {
	CipherSuite uint16 `json:"cipher_suite"`
	Psks        []struct {
		PskId    string `json:"psk_id"`
		Psk      string `json:"psk"`
		PskNonce string `json:"psk_nonce"`
	} `json:"psks"`
	PskSecret string `json:"psk_secret"`
}

// Family 6 is installed here, and 6 is deleted from expectedPendingFamilies in the same
// commit. Without both halves TestVectorFamiliesVerify runs one fewer family and the
// manifest gate stays green while claiming this family is unimplemented.
func init() {
	RegisterVectorFamily(VectorFamily{
		Number:   6,
		Name:     "Pre-shared keys",
		File:     pskSecretKatFile,
		Slice:    "A3",
		Verify:   verifyPskSecretVector,
		Generate: generatePskSecretVectors,
	})
}

// The refusals comparePskSecretVector makes, as sentinels rather than as formatted
// strings, so a test can require a specific refusal rather than "some error".
//
// They are what makes the comparison observable. A comparator that reports its verdict by
// calling t.Fatalf can only be judged by a corpus that happens to disagree with it, and
// the corpus in this tree agrees with everything; a comparator that RETURNS its verdict
// can be handed a deliberately wrong answer and required to refuse it, which is what
// TestComparePskSecretVectorRefusesAnAnswerItShouldNotAccept does.
var (
	errPskSecretPublishedWidth = errors.New("the published psk_secret is not the suite's KDF.Nh")
	errPskSecretIsEmptyAnswer  = errors.New("the published answer for a non-empty list is the empty answer")
	errPskSecretMismatch       = errors.New("psk_secret does not match the published answer")
	errPskSecretDidNotMove     = errors.New("flipping one octet of the published psk left psk_secret unchanged")
	errPskSecretIncomplete     = errors.New("the comparison reports values it cannot have computed")
)

// pskSecretComparison is what one run of comparePskSecretVector PRODUCED, and it is the
// only thing its callers are allowed to judge it by.
//
// This shape is the fix for the defect the first version of this file had. That version
// returned a bool, and the bool was the last statement of the function, so what it
// reported was "control reached the bottom" and not "a comparison happened": an early
// return inserted anywhere above it left the runner counting 22 comparisons that never
// called PskSecret at all, and the run stayed green. A bool cannot be told apart from a
// literal. These fields can: every one of them is written at the point the work that
// produces it happens, so a return that skipped the work reports the zero value, and a
// caller that looks at the values rather than at the fact of returning sees that.
//
// published is decoded by the comparator and computed is produced by PskSecret, and the
// runner re-derives the published half from the corpus text it parsed itself before
// comparing the two. That is deliberate duplication: the comparator's own bytes.Equal and
// the runner's hex string comparison are two expressions of one predicate over two
// independent decodes, and neutralising either one leaves the other standing.
type pskSecretComparison struct {
	// inScope is true when the vector's ciphersuite is one this package registers. A
	// false here is not a failure and not a skip: it is a case with no provider.
	inScope bool
	// psks is how many pre-shared keys reached the derivation, which is what decides
	// whether the two non-empty controls below apply.
	psks int
	// hashSize is the suite's KDF.Nh, read off the provider rather than assumed.
	hashSize int
	// published is the corpus's own answer, decoded.
	published []byte
	// computed is what PskSecret answered for this case.
	computed []byte
	// empty is EmptyPskSecret for this suite: the answer an implementation that ignored
	// the list entirely would give.
	empty []byte
	// perturbed is what PskSecret answers with one octet of the last psk flipped. Nil
	// where there is no non-empty psk to flip, which the two callers both allow for.
	perturbed []byte
}

// incomplete reports whether the fields a compared case must carry are missing or
// inconsistent, without looking at whether the answer was right.
//
// This is the vacuity half, split out from the correctness half on purpose. A comparison
// that produced no computed value, or a computed value of the wrong width, or an empty
// control it never derived, has not compared anything whatever bytes.Equal would say
// about it -- and bytes.Equal over two empty slices says they agree.
func (self pskSecretComparison) incomplete() error {
	switch {
	case !self.inScope:
		return fmt.Errorf("%w: the case is out of scope and carries no comparison", errPskSecretIncomplete)
	case self.hashSize == 0:
		return fmt.Errorf("%w: no KDF.Nh was read from the provider", errPskSecretIncomplete)
	case len(self.computed) != self.hashSize:
		return fmt.Errorf("%w: PskSecret produced %d octets and the suite's KDF.Nh is %d",
			errPskSecretIncomplete, len(self.computed), self.hashSize)
	case len(self.published) != self.hashSize:
		return fmt.Errorf("%w: the published answer is %d octets and the suite's KDF.Nh is %d",
			errPskSecretIncomplete, len(self.published), self.hashSize)
	case len(self.empty) != self.hashSize:
		return fmt.Errorf("%w: the empty answer control was never derived", errPskSecretIncomplete)
	case self.psks > 0 && len(self.perturbed) != self.hashSize:
		return fmt.Errorf("%w: %d psks reached the derivation and the flipped octet control was never run",
			errPskSecretIncomplete, self.psks)
	}
	return nil
}

// verdict is the whole judgement over one compared case: it must be complete, it must
// agree with the corpus, and both vacuity controls must have moved.
//
// Both callers of the comparator run this, and the runner then runs its own comparison
// over the same evidence from the corpus text it decoded itself. One caller would be
// enough for a correct tree and is not enough for this one: the registry shim reaches
// this comparator over all 77 published cases and over every generated case, and the
// runner reaches it over the 22 in scope, so a defect that only the generated cases
// expose has to be refused on the shim's path too.
func (self pskSecretComparison) verdict() error {
	if err := self.incomplete(); err != nil {
		return err
	}
	if self.psks > 0 && bytes.Equal(self.published, self.empty) {
		return fmt.Errorf("%w: a list of %d psks answers %s, which is what an implementation that ignored the list would answer",
			errPskSecretIsEmptyAnswer, self.psks, HexOf(self.empty))
	}
	if !bytes.Equal(self.computed, self.published) {
		return fmt.Errorf("%w: %d psks give %s, the corpus publishes %s",
			errPskSecretMismatch, self.psks, HexOf(self.computed), HexOf(self.published))
	}
	if self.psks > 0 && bytes.Equal(self.perturbed, self.computed) {
		return fmt.Errorf("%w: %d psks, so the corpus data never reached the derivation",
			errPskSecretDidNotMove, self.psks)
	}
	return nil
}

// verifyPskSecretVector is the registry's shim: the signature RegisterVectorFamily needs,
// over the comparator that does the work and reports what it produced.
//
// The split is the whole point. Verify cannot return anything, so a runner that counted
// calls to it would count a case it declined to check exactly as it counts a case it
// compared. comparePskSecretVector returns the values it derived and TestVectorPskSecret
// judges them.
func verifyPskSecretVector(t *testing.T, raw json.RawMessage) {
	t.Helper()
	evidence, err := comparePskSecretVector(t, raw)
	if err != nil {
		t.Fatalf("psk_secret: %v", err)
	}
	if !evidence.inScope {
		return
	}
	if err := evidence.verdict(); err != nil {
		t.Fatalf("psk_secret: %v", err)
	}
}

// comparePskSecretVector runs one entry of psk_secret.json and returns what the run
// produced. A vector at a ciphersuite v1 does not implement is not a failure and not a
// skip: it is a case this package has no provider for, and it comes back with inScope
// false and nothing else set.
//
// A corpus that will not parse or will not hex decode is fatal here rather than returned,
// because it is not a verdict about this implementation -- it is the evidence itself being
// unreadable, and every family in this package treats that as the loudest failure there
// is. Everything that IS a verdict about this implementation is returned, so a caller can
// require a refusal instead of hoping the corpus disagrees with a defect.
//
// The two controls carried per comparison are here rather than in the runner because they
// need the vector's own bytes. A published answer for a non-empty list that equalled the
// empty answer would let an implementation returning KDF.Nh zeroes pass; and if the psk
// list never reached PskSecret -- a renamed json field, a struct tag typo, a decoder
// returning nothing -- then flipping an octet of the corpus's own psk would leave the
// answer where it was. Both are the shape that has caught this before: install the
// corpus's own data and refuse to proceed unless the value moves.
func comparePskSecretVector(t *testing.T, raw json.RawMessage) (pskSecretComparison, error) {
	t.Helper()
	vector := pskSecretKatVector{}
	if err := json.Unmarshal(raw, &vector); err != nil {
		t.Fatalf("parse psk_secret entry: %v", err)
	}
	suite, ok := implementedSuite(vector.CipherSuite)
	if !ok {
		return pskSecretComparison{}, nil
	}
	crypto, err := NewCryptoProvider(suite)
	if err != nil {
		t.Fatalf("NewCryptoProvider(%#04x): %v", uint16(suite), err)
	}
	psks := make([]PreSharedKeyInput, 0, len(vector.Psks))
	for _, entry := range vector.Psks {
		psks = append(psks, PreSharedKeyInput{
			Id: PreSharedKeyId{
				PskType:  PskTypeExternal,
				PskId:    MustHex(t, entry.PskId),
				PskNonce: MustHex(t, entry.PskNonce),
			},
			Secret: MustHex(t, entry.Psk),
		})
	}
	if len(psks) != len(vector.Psks) {
		t.Fatalf("the entry carries %d psks and %d reached the derivation", len(vector.Psks), len(psks))
	}
	evidence := pskSecretComparison{
		inScope:   true,
		psks:      len(psks),
		hashSize:  crypto.HashSize(),
		published: MustHex(t, vector.PskSecret),
		empty:     EmptyPskSecret(crypto),
	}
	if len(evidence.published) != evidence.hashSize {
		return evidence, fmt.Errorf("%w: %d octets against a KDF.Nh of %d, so this is not the comparison the corpus intends",
			errPskSecretPublishedWidth, len(evidence.published), evidence.hashSize)
	}
	computed, err := PskSecret(crypto, psks)
	if err != nil {
		return evidence, fmt.Errorf("%d psks: PskSecret: %w", len(psks), err)
	}
	evidence.computed = computed

	// the vacuity control: one octet of the corpus's own psk, flipped, must move the
	// answer. An agreement that survives this was not computed from the corpus.
	if len(psks) > 0 && len(psks[len(psks)-1].Secret) > 0 {
		moved := make([]PreSharedKeyInput, len(psks))
		copy(moved, psks)
		last := moved[len(moved)-1]
		flipped := bytes.Clone(last.Secret)
		flipped[0] ^= 0x01
		last.Secret = flipped
		moved[len(moved)-1] = last
		perturbed, err := PskSecret(crypto, moved)
		if err != nil {
			return evidence, fmt.Errorf("PskSecret over the perturbed list: %w", err)
		}
		evidence.perturbed = perturbed
	}
	return evidence, evidence.verdict()
}

// TestVectorPskSecret is vector family 6 over the published corpus.
//
// Every assertion vectorRunTally makes after the loop exists because the loop can be made to
// run zero times without anything else in this package noticing. A filter that matched
// nothing, a filter that matched all seven published suites, a corpus that parsed to an empty
// array, a comparator that declined every case: each of those is a green run of this test
// with the accounting removed, and a failure with it.
//
// What the loop counts is the part that had to be rewritten. It does not count calls that
// returned; it counts cases whose returned evidence the runner itself checked against the
// corpus text it decoded itself, so a comparator that answered without computing anything is
// a failure here rather than a number that looks right.
func TestVectorPskSecret(t *testing.T) {
	tally, entries := newVectorRunTally(t, pskSecretKatFile)
	for index, raw := range entries {
		header := struct {
			CipherSuite uint16 `json:"cipher_suite"`
			PskSecret   string `json:"psk_secret"`
		}{}
		if err := json.Unmarshal(raw, &header); err != nil {
			t.Fatalf("vector %d: %v", index, err)
		}
		suite, inScope := tally.filter(header.CipherSuite)
		if !inScope {
			continue
		}
		evidence, err := comparePskSecretVector(t, raw)
		if err != nil {
			t.Fatalf("vector %d (suite %#04x): %v", index, header.CipherSuite, err)
		}
		tally.requireCompared(t, index, suite, evidence.inScope)
		if err := evidence.verdict(); err != nil {
			t.Fatalf("vector %d (suite %#04x): %v", index, header.CipherSuite, err)
		}
		// and the runner's own half of the comparison, against the answer it decoded
		// out of the corpus itself rather than against the comparator's copy of it.
		if got := HexOf(evidence.computed); got != header.PskSecret {
			t.Fatalf("vector %d (suite %#04x, %d psks): this package computes %s, the corpus publishes %s",
				index, header.CipherSuite, evidence.psks, got, header.PskSecret)
		}
		tally.answer(header.PskSecret)
	}

	// the shape of this family's file, which the per suite split inside assertRun reads off
	// the corpus rather than transcribing: a grid of one list length series per published
	// suite, so every suite carries the same number of cases and what each registered suite
	// owes is what the corpus holds for it. A file that grew a case at one suite alone would
	// leave that split holding against a grid it is no longer reading.
	perSuite := map[int]bool{}
	for _, count := range tally.published {
		perSuite[count] = true
	}
	if len(perSuite) != 1 {
		t.Fatalf("the corpus publishes %v cases per suite; this family's file is a grid of one series per suite and the split assumes it",
			tally.published)
	}
	tally.assertRun(t, pskSecretKatComparisons, pskSecretKatSkipped,
		pskSecretKatComparisons, pskSecretKatDistinctAnswers)
}

// TestComparePskSecretVectorRefusesAnAnswerItShouldNotAccept is the control the runner
// cannot be: it hands the comparator answers that are wrong in each of the ways the corpus
// is not, and requires the matching refusal.
//
// Why this test rather than more assertions in the runner. Every comparison the runner
// makes is over a corpus that agrees with this implementation, so a comparator that
// accepted everything and a comparator that checked everything produce identical runs
// there. The only way to see the difference is to disagree with it on purpose. Each case
// below is a real defect class -- a wrong answer, an answer of the wrong width, a
// non-empty list published with the empty answer, a psk list that never reached the
// derivation -- and each names the sentinel it owes, so a refusal for the wrong reason is
// a failure too.
//
// The driver is assertComparatorRefuses, shared with every other family: it checks the
// unmodified case FIRST, which is the reason the four refusals mean anything, since a
// comparator that refused everything would satisfy them all. The evidence the unmodified
// case carries is checked here first as well, because "returned no error" and "compared
// something" are not the same claim.
func TestComparePskSecretVectorRefusesAnAnswerItShouldNotAccept(t *testing.T) {
	entries := LoadVectorFile(t, pskSecretKatFile)
	base := pskSecretKatVector{}
	found := false
	for _, raw := range entries {
		candidate := pskSecretKatVector{}
		if err := json.Unmarshal(raw, &candidate); err != nil {
			t.Fatalf("parse a psk_secret entry: %v", err)
		}
		// a case with at least two psks, so the flipped octet control has something to
		// flip and the index and count fields of PSKLabel are both exercised.
		if _, ok := implementedSuite(candidate.CipherSuite); ok && len(candidate.Psks) >= 2 {
			base, found = candidate, true
			break
		}
	}
	if !found {
		t.Fatal("no published case at a registered suite carries two or more psks, so this control has nothing to corrupt")
	}

	encode := func(vector pskSecretKatVector) json.RawMessage {
		body, err := json.Marshal(vector)
		if err != nil {
			t.Fatalf("marshal the case under test: %v", err)
		}
		return body
	}

	evidence, err := comparePskSecretVector(t, encode(base))
	if err != nil {
		t.Fatalf("the unmodified published case was refused: %v", err)
	}
	if !evidence.inScope || len(evidence.computed) == 0 {
		t.Fatalf("the unmodified published case produced %+v, which carries no comparison", evidence)
	}
	if HexOf(evidence.computed) != base.PskSecret {
		t.Fatalf("the unmodified published case computed %s against a published %s", HexOf(evidence.computed), base.PskSecret)
	}

	wrongAnswer := base
	flipped := MustHex(t, base.PskSecret)
	flipped[0] ^= 0x01
	wrongAnswer.PskSecret = HexOf(flipped)

	wrongWidth := base
	wrongWidth.PskSecret = base.PskSecret[:len(base.PskSecret)-2]

	emptyAnswer := base
	crypto, err := NewCryptoProvider(CipherSuite(base.CipherSuite))
	if err != nil {
		t.Fatalf("NewCryptoProvider(%#04x): %v", base.CipherSuite, err)
	}
	emptyAnswer.PskSecret = HexOf(EmptyPskSecret(crypto))

	// a psk list that decodes to nothing while the published answer stays the non-empty
	// one: the shape a renamed json field or a struct tag typo produces, and the one the
	// flipped octet control exists for.
	noPsks := base
	noPsks.Psks = nil

	assertComparatorRefuses(t, "psk_secret",
		func(t *testing.T, raw json.RawMessage) error {
			_, err := comparePskSecretVector(t, raw)
			return err
		},
		encode(base),
		[]comparatorRefusal{
			{"one flipped octet of the published answer", encode(wrongAnswer), errPskSecretMismatch},
			{"a published answer one octet short of KDF.Nh", encode(wrongWidth), errPskSecretPublishedWidth},
			{"the empty answer published for a non-empty list", encode(emptyAnswer), errPskSecretIsEmptyAnswer},
			{"the psk list dropped and the answer kept", encode(noPsks), errPskSecretMismatch},
		})
}

// TestPskSecretComparisonCannotReportAComparisonItDidNotMake is the control on the
// evidence struct itself.
//
// The defect this whole shape replaces was a comparator whose "I compared" signal was the
// last statement of the function. The signal is now the values it produced, and this is
// what says those values cannot be faked by omission: a zero comparison, a computed value
// of the wrong width, a missing empty control and a missing flipped octet control are each
// required to be refused as incomplete, so a return that skipped the work is a failure on
// every caller's path rather than a count that still reads 22.
func TestPskSecretComparisonCannotReportAComparisonItDidNotMake(t *testing.T) {
	full := pskSecretComparison{
		inScope:   true,
		psks:      2,
		hashSize:  sha256.Size,
		published: bytes.Repeat([]byte{0xaa}, sha256.Size),
		computed:  bytes.Repeat([]byte{0xaa}, sha256.Size),
		empty:     make([]byte, sha256.Size),
		perturbed: bytes.Repeat([]byte{0xbb}, sha256.Size),
	}
	if err := full.verdict(); err != nil {
		t.Fatalf("a complete and agreeing comparison was refused: %v; every case below would then pass for the wrong reason", err)
	}

	without := func(edit func(*pskSecretComparison)) pskSecretComparison {
		partial := full
		partial.published = bytes.Clone(full.published)
		partial.computed = bytes.Clone(full.computed)
		partial.empty = bytes.Clone(full.empty)
		partial.perturbed = bytes.Clone(full.perturbed)
		edit(&partial)
		return partial
	}
	for _, missing := range []struct {
		name string
		edit func(*pskSecretComparison)
	}{
		{"a comparison that returned before anything was set", func(c *pskSecretComparison) { *c = pskSecretComparison{} }},
		{"in scope and nothing else", func(c *pskSecretComparison) { *c = pskSecretComparison{inScope: true} }},
		{"no computed value", func(c *pskSecretComparison) { c.computed = nil }},
		{"a computed value of the wrong width", func(c *pskSecretComparison) { c.computed = c.computed[:len(c.computed)-1] }},
		{"no published value", func(c *pskSecretComparison) { c.published = nil }},
		{"no empty answer control", func(c *pskSecretComparison) { c.empty = nil }},
		{"no flipped octet control over a non-empty list", func(c *pskSecretComparison) { c.perturbed = nil }},
		{"no KDF.Nh read from the provider", func(c *pskSecretComparison) { c.hashSize = 0 }},
	} {
		partial := without(missing.edit)
		err := partial.verdict()
		if err == nil {
			t.Errorf("%s was accepted as a comparison", missing.name)
			continue
		}
		if !errors.Is(err, errPskSecretIncomplete) {
			t.Errorf("%s was refused as %v, want an incompleteness", missing.name, err)
		}
	}

	// and the correctness half, which incompleteness must not be standing in for.
	disagreeing := without(func(c *pskSecretComparison) { c.computed[0] ^= 0x01 })
	if err := disagreeing.verdict(); !errors.Is(err, errPskSecretMismatch) {
		t.Errorf("a complete comparison whose computed value disagrees was judged %v, want a mismatch", err)
	}
	stuck := without(func(c *pskSecretComparison) { c.perturbed = bytes.Clone(c.computed) })
	if err := stuck.verdict(); !errors.Is(err, errPskSecretDidNotMove) {
		t.Errorf("a comparison whose flipped octet control did not move was judged %v, want that refusal", err)
	}
	// the empty list arm: no flipped octet control is owed, and the empty answer control
	// does not apply, so an agreeing empty case must be accepted.
	empty := without(func(c *pskSecretComparison) {
		c.psks = 0
		c.perturbed = nil
		c.computed = bytes.Clone(c.empty)
		c.published = bytes.Clone(c.empty)
	})
	if err := empty.verdict(); err != nil {
		t.Fatalf("an agreeing empty list case was refused: %v; the 2 empty cases of the corpus would fail for the wrong reason", err)
	}
}

// TestImplementedSuiteIsTheRegistryAndNotAList sweeps every uint16 code point and requires
// implementedSuite to answer exactly what the ciphersuite registry answers.
//
// This is the check that stops the filter becoming a hand written list. Fourteen times on
// this project a hand written list understated the class it was meant to describe, and the
// version of that failure here is silent in the worst way: a third suite registered, every
// vector family still skipping it, and every runner reporting a clean pass over a corpus it
// no longer covers.
//
// The registry read is checked for the two suites it must hold first, so the sweep cannot
// pass by having derived nothing.
func TestImplementedSuiteIsTheRegistryAndNotAList(t *testing.T) {
	registered := Suites()
	if len(registered) != 2 {
		t.Fatalf("the registry holds %v; this test expects the two v1 suites, and a change here means the counts in this file need revisiting", registered)
	}
	for _, suite := range []CipherSuite{
		CipherSuiteX25519AesGcm128Sha256Ed25519,
		CipherSuiteX25519ChaCha20Sha256Ed25519,
	} {
		if !slices.Contains(registered, suite) {
			t.Fatalf("the registry does not hold %#04x, so the sweep below is reading nothing useful", uint16(suite))
		}
	}

	accepted := []CipherSuite{}
	for point := 0; point <= 0xffff; point++ {
		code := uint16(point)
		suite, ok := implementedSuite(code)
		if ok != IsRegisteredSuite(CipherSuite(code)) {
			t.Fatalf("implementedSuite(%#04x) = %v and the registry says %v", code, ok, IsRegisteredSuite(CipherSuite(code)))
		}
		if !ok {
			if suite != 0 {
				t.Fatalf("implementedSuite(%#04x) refused and still answered %#04x", code, uint16(suite))
			}
			continue
		}
		if uint16(suite) != code {
			t.Fatalf("implementedSuite(%#04x) answered %#04x, so the filter renumbers the suite it matched", code, uint16(suite))
		}
		accepted = append(accepted, suite)
	}
	if !slices.Equal(accepted, registered) {
		t.Fatalf("the sweep accepted %v and the registry holds %v", accepted, registered)
	}
}

// TestPskSecretFamilyIsInstalled is the registration half of this task.
//
// Registering the family and deleting its number from expectedPendingFamilies are two
// edits, and doing only the first leaves TestVectorManifestIsComplete failing while doing
// only the second leaves it passing with the family uninstalled. assertVectorFamilyIsInstalled
// asserts both, and asserts the runner and generator installed are this file's, so a row
// that kept the number and lost the function fails there.
func TestPskSecretFamilyIsInstalled(t *testing.T) {
	assertVectorFamilyIsInstalled(t, 6, pskSecretKatFile, verifyPskSecretVector, generatePskSecretVectors)
}

// ---------------------------------------------------------------------------
// the generate direction, written from the RFCs rather than from this package
// ---------------------------------------------------------------------------

// independentPsk is one pre-shared key as the hand written derivation below needs it: the
// three fields section 8.4 folds, and nothing this package declares.
type independentPsk struct {
	id       []byte
	pskNonce []byte
	secret   []byte
}

// independentOpaqueV writes the RFC 9420 section 2.1.2 variable length prefix by hand: one
// octet with prefix bits 0b00 below 64, two with 0b01 up to 16383.
//
// Written out because it is the hazard with no other witness on this side. A one octet
// length on the 71 octet PSKLabel, or a two octet length on a 32 octet psk_id, produces a
// perfectly well formed preimage and a psk_secret that is 32 octets of apparent random.
func independentOpaqueV(t *testing.T, body []byte) []byte {
	t.Helper()
	switch {
	case len(body) < 64:
		return append([]byte{byte(len(body))}, body...)
	case len(body) < 16384:
		return append([]byte{0x40 | byte(len(body)>>8), byte(len(body))}, body...)
	}
	t.Fatalf("a %d octet field needs a four octet varint, which this derivation does not write", len(body))
	return nil
}

// independentExpandWithLabel is RFC 9420 section 5.1's KDF.ExpandWithLabel, written from
// the two RFCs it is made of:
//
//	RFC 9420 section 5.1   ExpandWithLabel(Secret, Label, Context, Length) expands under
//	                       struct { uint16 length; opaque label<V>; opaque context<V> },
//	                       the label carrying the "MLS 1.0 " prefix
//	RFC 5869 section 2.3   HKDF-Expand's first block is HMAC(PRK, info || 0x01), and 32
//	                       octets is exactly that first block under sha256
//
// so for the 19 octet label "MLS 1.0 derived psk" and the 71 octet PSKLabel the info is
//
//	00 20      the requested output length, 32, big endian uint16
//	13         label<V> byte length 19; 19 < 64 so one octet, prefix bits 0b00
//	4d 4c ..   the 19 label octets
//	40 47      context<V> byte length 71; 71 > 63 so two octets: 0x40|(71>>8), 71&0xff
//	01 20 ..   the 71 PSKLabel octets
//
// A length field holding the label's size instead of the output's, a label without the
// prefix, or the two opaque fields transposed all give a well formed 32 octet answer.
func independentExpandWithLabel(t *testing.T, secret []byte, label string, context []byte, length int) []byte {
	t.Helper()
	// one HKDF-Expand block and a prefix of it, which covers every width this tree asks for:
	// KDF.Nh is 32 at both registered suites, and the secret tree's AEAD widths are narrower
	// still -- Nk 16 or 32, Nn 12. RFC 5869 section 2.3 takes the first L octets of
	// T(1) || T(2) || ..., so for any L at or below the hash size the answer is a prefix of
	// the first block and no second block is defined. A width past that is refused rather
	// than truncated the other way, because the block this writes would then be the whole
	// answer and would silently agree with an implementation that stopped early too.
	if length <= 0 || length > sha256.Size {
		t.Fatalf("this derivation writes one HKDF-Expand block and was asked for %d octets", length)
	}
	// the KDFLabel bytes are written by independentKdfLabel, which is the single hand
	// written encoder of this file. Inlining them here would make it two, and a second
	// hand encoder is a second opinion nothing checks -- task 18 holds the one below
	// against this package's own ExpandWithLabel byte for byte, and that check would say
	// nothing about a copy living here.
	expand := hmac.New(sha256.New, secret)
	expand.Write(independentKdfLabel(t, label, context, length))
	expand.Write([]byte{0x01})
	// the length field inside the KDFLabel above is the REQUESTED width and the block is cut
	// to the same width here. Writing the block whole while asking for a narrower one is the
	// mistake that looks right: the first 16 octets of a 32 octet request are not the 16
	// octet answer, because the width is inside the preimage.
	return expand.Sum(nil)[:length]
}

// independentPskSecret is the RFC 9420 section 8.4 recurrence, written out with crypto/hmac
// and reaching nothing this package declares:
//
//	RFC 5869 section 2.2   HKDF-Extract(salt, IKM) = HMAC-Hash(key = salt, data = IKM)
//	RFC 9420 section 8.4   psk_extracted_[i] = KDF.Extract(0, psk_[i])
//	                       psk_input_[i]     = ExpandWithLabel(psk_extracted_[i],
//	                                             "derived psk", PSKLabel_[i], KDF.Nh)
//	                       psk_secret_[0]    = 0
//	                       psk_secret_[i]    = KDF.Extract(psk_input_[i-1], psk_secret_[i-1])
//	RFC 9420 section 8.4   PSKLabel = { PreSharedKeyID id; uint16 index; uint16 count; }
//	RFC 9420 section 8.4   an external PreSharedKeyID is the octet 1, then psk_id<V>, then
//	                       psk_nonce<V>
//
// Both Extract calls are written as HMAC with the salt in the key position, which is the
// one place a second opinion is worth having: transposing either compiles, returns 32
// octets, and satisfies every property either side could assert about its own output.
// TestIndependentPskSecretSeesATransposedExtract requires this derivation to disagree with
// its own transposition, so it cannot agree with a transposed implementation by being
// transposed the same way.
//
// sha256 is written in rather than read off a provider on purpose: both registered suites
// are HKDF-SHA256 at KDF.Nh 32, and reading the width off the code under test is how a
// second opinion stops being one. TestBothRegisteredSuitesAreSha256AtThisWidth is what
// fails on the day that stops being true.
func independentPskSecret(t *testing.T, psks []independentPsk) []byte {
	t.Helper()
	if len(psks) > 0xffff {
		t.Fatalf("a list of %d psks cannot be described by the uint16 count field", len(psks))
	}
	// psk_secret_[0], the KDF.Nh all zero string.
	pskSecret := make([]byte, sha256.Size)
	count := len(psks)
	for i, psk := range psks {
		// psk_extracted_[i] = HKDF-Extract(salt = 0, IKM = psk_[i])
		extract := hmac.New(sha256.New, make([]byte, sha256.Size))
		extract.Write(psk.secret)
		extracted := extract.Sum(nil)

		// PSKLabel_[i], the external arm of PreSharedKeyID followed by index and count
		pskLabel := []byte{0x01}
		pskLabel = append(pskLabel, independentOpaqueV(t, psk.id)...)
		pskLabel = append(pskLabel, independentOpaqueV(t, psk.pskNonce)...)
		pskLabel = append(pskLabel, byte(i>>8), byte(i))
		pskLabel = append(pskLabel, byte(count>>8), byte(count))

		pskInput := independentExpandWithLabel(t, extracted, "derived psk", pskLabel, sha256.Size)

		// psk_secret_[i+1] = HKDF-Extract(salt = psk_input_[i], IKM = psk_secret_[i])
		fold := hmac.New(sha256.New, pskInput)
		fold.Write(pskSecret)
		pskSecret = fold.Sum(nil)
	}
	return pskSecret
}

// independentPskSecretTransposed is the same recurrence with both Extract calls the wrong
// way round. It exists only to be disagreed with: a hand written derivation that gave the
// same answer either way could not see the defect guardrail 1 names, and would agree with a
// transposed implementation while looking like a second opinion.
func independentPskSecretTransposed(t *testing.T, psks []independentPsk) []byte {
	t.Helper()
	pskSecret := make([]byte, sha256.Size)
	count := len(psks)
	for i, psk := range psks {
		extract := hmac.New(sha256.New, psk.secret)
		extract.Write(make([]byte, sha256.Size))
		extracted := extract.Sum(nil)

		pskLabel := []byte{0x01}
		pskLabel = append(pskLabel, independentOpaqueV(t, psk.id)...)
		pskLabel = append(pskLabel, independentOpaqueV(t, psk.pskNonce)...)
		pskLabel = append(pskLabel, byte(i>>8), byte(i))
		pskLabel = append(pskLabel, byte(count>>8), byte(count))

		pskInput := independentExpandWithLabel(t, extracted, "derived psk", pskLabel, sha256.Size)

		fold := hmac.New(sha256.New, pskSecret)
		fold.Write(pskInput)
		pskSecret = fold.Sum(nil)
	}
	return pskSecret
}

// generatedPskOctets is deterministic filler for the generate direction. Deterministic
// rather than random so a generated case that fails is the same case on the next run;
// sha256 over a per-field string rather than a counter so no two fields of one psk are
// related in a way a broken PSKLabel encoding could hide behind.
func generatedPskOctets(field string, index int) []byte {
	digest := sha256.Sum256([]byte(fmt.Sprintf("mls slice1 p4 task16 psk_secret generate %s %d", field, index)))
	return digest[:]
}

// generatePskSecretVectors produces fresh psk_secret cases in the published format, with
// every answer computed by independentPskSecret.
//
// This function calls no production function of package mls at all, which is asserted
// rather than described: TestTheGenerateDirectionSharesNoCodePathWithVerify derives the
// production function names from the package's own source and requires this function's
// call closure to be disjoint from them. The ciphersuite code points below are constants,
// not a derivation, and TestGeneratedVectorsCoverEveryRegisteredSuite is what fails on the
// day a third suite is registered and this list stops covering the registry.
//
// List lengths 0, 1 and 2 are all three arms of the recurrence: the empty answer that every
// v1 epoch actually mixes in, the singleton that is the only check on the "derived psk"
// label, and the fold where the accumulator and the index live.
func generatePskSecretVectors(t *testing.T) json.RawMessage {
	t.Helper()
	generated := []pskSecretKatVector{}
	for _, suite := range []uint16{
		uint16(CipherSuiteX25519AesGcm128Sha256Ed25519),
		uint16(CipherSuiteX25519ChaCha20Sha256Ed25519),
	} {
		for _, count := range []int{0, 1, 2, 3} {
			vector := pskSecretKatVector{CipherSuite: suite}
			psks := make([]independentPsk, 0, count)
			for i := 0; i < count; i++ {
				tag := fmt.Sprintf("suite %d count %d", suite, count)
				psk := independentPsk{
					id:       generatedPskOctets(tag+" id", i),
					pskNonce: generatedPskOctets(tag+" nonce", i),
					secret:   generatedPskOctets(tag+" secret", i),
				}
				psks = append(psks, psk)
				vector.Psks = append(vector.Psks, struct {
					PskId    string `json:"psk_id"`
					Psk      string `json:"psk"`
					PskNonce string `json:"psk_nonce"`
				}{
					PskId:    HexOf(psk.id),
					Psk:      HexOf(psk.secret),
					PskNonce: HexOf(psk.pskNonce),
				})
			}
			vector.PskSecret = HexOf(independentPskSecret(t, psks))
			generated = append(generated, vector)
		}
	}
	body, err := json.Marshal(generated)
	if err != nil {
		t.Fatalf("marshal the generated psk_secret cases: %v", err)
	}
	return body
}

// TestIndependentPskSecretMatchesEveryUpstreamVector pins the generator to the published
// corpus.
//
// This is what makes the generate direction worth running. A generator agreeing with the
// verifier proves that two spellings of one algorithm agree; a generator that reproduces
// every answer mlswg published, computed with crypto/hmac from the RFC text, is a second
// implementation, and the round trip through it is then a statement about conformance.
//
// The comparison count is asserted for the same reason it is asserted in the runner.
func TestIndependentPskSecretMatchesEveryUpstreamVector(t *testing.T) {
	compared := 0
	for index, raw := range LoadVectorFile(t, pskSecretKatFile) {
		vector := pskSecretKatVector{}
		if err := json.Unmarshal(raw, &vector); err != nil {
			t.Fatalf("vector %d: %v", index, err)
		}
		if _, ok := implementedSuite(vector.CipherSuite); !ok {
			continue
		}
		psks := make([]independentPsk, 0, len(vector.Psks))
		for _, entry := range vector.Psks {
			psks = append(psks, independentPsk{
				id:       MustHex(t, entry.PskId),
				pskNonce: MustHex(t, entry.PskNonce),
				secret:   MustHex(t, entry.Psk),
			})
		}
		if got := HexOf(independentPskSecret(t, psks)); got != vector.PskSecret {
			t.Errorf("vector %d (suite %#04x, %d psks): the hand written derivation gives %s, the corpus publishes %s",
				index, vector.CipherSuite, len(psks), got, vector.PskSecret)
		}
		compared++
	}
	if compared != pskSecretKatComparisons {
		t.Fatalf("the hand written derivation was compared against %d published answers, want %d",
			compared, pskSecretKatComparisons)
	}
}

// TestIndependentPskSecretSeesATransposedExtract is the control the test above needs. A
// derivation that answers the same with its Extract arguments the wrong way round agrees
// with a transposed implementation and pins nothing about guardrail 1.
//
// The empty list is excluded because it folds nothing and both spellings answer the all
// zero string, which is correct rather than a failure of the control.
func TestIndependentPskSecretSeesATransposedExtract(t *testing.T) {
	checked := 0
	for index, raw := range LoadVectorFile(t, pskSecretKatFile) {
		vector := pskSecretKatVector{}
		if err := json.Unmarshal(raw, &vector); err != nil {
			t.Fatalf("vector %d: %v", index, err)
		}
		if _, ok := implementedSuite(vector.CipherSuite); !ok || len(vector.Psks) == 0 {
			continue
		}
		psks := make([]independentPsk, 0, len(vector.Psks))
		for _, entry := range vector.Psks {
			psks = append(psks, independentPsk{
				id:       MustHex(t, entry.PskId),
				pskNonce: MustHex(t, entry.PskNonce),
				secret:   MustHex(t, entry.Psk),
			})
		}
		if bytes.Equal(independentPskSecretTransposed(t, psks), independentPskSecret(t, psks)) {
			t.Fatalf("vector %d: the hand written derivation gives one answer for both Extract orders, so it cannot see a transposition",
				index)
		}
		checked++
	}
	if checked != pskSecretKatComparisons-2 {
		t.Fatalf("the transposition control ran over %d non-empty lists, want %d",
			checked, pskSecretKatComparisons-2)
	}
}

// TestBothRegisteredSuitesAreSha256AtThisWidth is the assumption independentPskSecret makes
// when it writes sha256 in rather than reading a width off the code under test. It stops
// being true the day a wider suite is registered, and this is where that is said.
func TestBothRegisteredSuitesAreSha256AtThisWidth(t *testing.T) {
	for _, suite := range Suites() {
		crypto, err := NewCryptoProvider(suite)
		if err != nil {
			t.Fatalf("NewCryptoProvider(%#04x): %v", uint16(suite), err)
		}
		if crypto.HashSize() != sha256.Size {
			t.Fatalf("suite %#04x has KDF.Nh %d and the hand written derivation writes %d; it needs widening before it can be a second opinion at that suite",
				uint16(suite), crypto.HashSize(), sha256.Size)
		}
	}
}

// TestGeneratedVectorsCoverEveryRegisteredSuite holds the generator's own list of code
// points to the registry, which is the check the generator cannot make about itself
// without calling into the package it is meant to stay clear of.
func TestGeneratedVectorsCoverEveryRegisteredSuite(t *testing.T) {
	entries := []pskSecretKatVector{}
	if err := json.Unmarshal(generatePskSecretVectors(t), &entries); err != nil {
		t.Fatalf("the generated cases do not parse: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the generator produced no cases")
	}
	covered := map[CipherSuite]int{}
	counts := map[int]int{}
	for _, entry := range entries {
		covered[CipherSuite(entry.CipherSuite)]++
		counts[len(entry.Psks)]++
	}
	if got := slices.Sorted(maps.Keys(covered)); !slices.Equal(got, Suites()) {
		t.Fatalf("the generator covers %v and the registry holds %v; widen generatePskSecretVectors", got, Suites())
	}
	for _, arm := range []int{0, 1, 2} {
		if counts[arm] == 0 {
			t.Errorf("the generator produced no case with %d psks, so that arm of the recurrence is unexercised in the generate direction", arm)
		}
	}
	t.Logf("%d generated cases over suites %v, list lengths %v", len(entries), slices.Sorted(maps.Keys(covered)), counts)
}

// ---------------------------------------------------------------------------
// the structural gates over the runner files themselves
// ---------------------------------------------------------------------------

// packageTestFiles parses every test file of this package, keyed by base name.
//
// Every test file, and that is the load bearing word. The call graph gate below walks the
// functions these files declare, and a function it cannot see is a leaf: its name is
// collected and its body is never entered, so anything it calls is invisible. Narrowing
// this set to the files that mention the vector harness is therefore not an optimisation,
// it is an escape hatch one hop wide -- a generator routed through a helper declared in
// psk_test.go, crypto_test.go or any other test file would launder the entire production
// closure and the gate would report clean. That is exactly the shape of the first version
// of this file, and it is why the class here is "is a test file of this package" and
// nothing narrower.
//
// The count is asserted against the directory listing so a filter added later cannot
// quietly shrink it.
func packageTestFiles(t *testing.T) map[string]*ast.File {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	fileSet := token.NewFileSet()
	files := map[string]*ast.File{}
	onDisk := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		onDisk++
		parsed, err := parser.ParseFile(fileSet, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files[name] = parsed
	}
	if onDisk == 0 {
		t.Fatal("no test file was read from the package directory, so every gate over this set would hold vacuously")
	}
	if len(files) != onDisk {
		t.Fatalf("%d test files are on disk and %d were kept; a filter here is an escape hatch for the call graph gate below",
			onDisk, len(files))
	}
	return files
}

// vectorRunnerFiles is every test file of this package that takes part in the vector
// harness, derived rather than listed: a file that installs a family or reads the corpus
// through the harness loader is one, and nothing else is.
//
// Derived because the class grows. Tasks 17, 18, 20 and 25 add runners to this file, and
// p2, p3, p5, p6 and p7 add runner files of their own; a written list would cover the files
// that existed when it was written and silently exempt every one that arrived after.
//
// This is the class for the skip gate, which is a question about a file: may THIS file
// decline to run. It is not the class for the call graph gate, which is a question about a
// closure and needs every test file -- see packageTestFiles.
func vectorRunnerFiles(t *testing.T) map[string]*ast.File {
	t.Helper()
	all := packageTestFiles(t)
	runners := map[string]*ast.File{}
	for name, parsed := range all {
		if namesMentionedIn(parsed)["RegisterVectorFamily"] || namesMentionedIn(parsed)["LoadVectorFile"] {
			runners[name] = parsed
		}
	}
	for _, required := range []string{"key_schedule_kat_test.go", "vectors_test.go"} {
		if _, found := runners[required]; !found {
			t.Fatalf("%s takes part in the vector harness and the derivation did not find it, so it is matching nothing useful over %d test files",
				required, len(all))
		}
	}
	return runners
}

// namesMentionedIn returns every identifier a node names, in any position. It answers the
// only question it is asked -- does this test file mention the harness at all -- where a
// mention is exactly the question and over-approximating costs nothing.
func namesMentionedIn(node ast.Node) map[string]bool {
	mentioned := map[string]bool{}
	ast.Inspect(node, func(current ast.Node) bool {
		if ident, isIdent := current.(*ast.Ident); isIdent {
			mentioned[ident.Name] = true
		}
		return true
	})
	return mentioned
}

// opensAScope answers whether a node introduces a scope a binding can be confined to. Every
// construct Go scopes on is here rather than a subset of them: the body of anything is a block,
// but an if, a for, a range, a switch, a type switch and a select each scope their own
// initialiser, and a case or a comm clause scopes its statements with no block around them.
func opensAScope(node ast.Node) bool {
	switch node.(type) {
	case *ast.FuncDecl, *ast.FuncLit, *ast.BlockStmt, *ast.IfStmt, *ast.ForStmt,
		*ast.RangeStmt, *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt,
		*ast.CaseClause, *ast.CommClause:
		return true
	}
	return false
}

// localUsesIn is every identifier NODE of one function that a binding the function itself
// introduces is in scope for: a parameter, a named result, a receiver, a short variable
// declaration, a var or const spec, a range variable, a type switch binding.
//
// It is the other half of the exclusion namesInvokedBy already makes for a field DECLARATION.
// Excluding the declaration and not the USES reads a parameter named treeHash as a call to a
// package level treeHash at every line that passes it on, which is not a hole in the abstract:
// independentGroupContext takes a treeHash []byte, (*RatchetTree).treeHash is a method of this
// package, and the disjointness gate below reported the hand written generator as reaching the
// implementation it exists to be a second opinion about. The same collision reached the
// extension body closure through the treeHash local of (*GroupContext).UnmarshalMLS.
//
// What this answers about is one identifier NODE rather than a name, and that is the whole
// difference from the per FUNCTION set it replaces. That set treated a name bound anywhere in a
// body as bound throughout it, so a name bound in an inner block and NAMED AS A VALUE in an
// outer scope -- where the binding does not reach -- was dropped, and every gate reading this
// matcher lost that edge. The doc this replaces admitted the case and nothing held it; the
// control below holds it now, and holds the other direction too, because a scope walk that
// popped too eagerly would read an ordinary local as an invocation and widen every gate rather
// than narrow it.
//
// One approximation is left and it is the conservative direction: the left hand side of a short
// variable declaration is bound before the right hand side is walked, so the second x of x := x
// reads as the local. That drops an edge rather than inventing one, and the call position
// escape in namesInvokedBy is what keeps a dropped edge from hiding a call.
func localUsesIn(node ast.Node) map[*ast.Ident]bool {
	uses := map[*ast.Ident]bool{}
	scopes := []map[string]bool{}
	path := []ast.Node{}
	bind := func(name string) {
		if len(scopes) == 0 || name == "" || name == "_" {
			return
		}
		scopes[len(scopes)-1][name] = true
	}
	take := func(expressions []ast.Expr) {
		for _, one := range expressions {
			if ident, isIdent := one.(*ast.Ident); isIdent {
				bind(ident.Name)
			}
		}
	}
	inScope := func(name string) bool {
		for at := len(scopes) - 1; at >= 0; at -= 1 {
			if scopes[at][name] {
				return true
			}
		}
		return false
	}
	ast.Inspect(node, func(current ast.Node) bool {
		// ast.Inspect calls back with nil after the children of every node it descended into,
		// which is what pops the scope stack in step with the walk rather than at a depth
		// counted here.
		if current == nil {
			last := path[len(path)-1]
			path = path[:len(path)-1]
			if opensAScope(last) {
				scopes = scopes[:len(scopes)-1]
			}
			return false
		}
		path = append(path, current)
		if opensAScope(current) {
			scopes = append(scopes, map[string]bool{})
		}
		switch typed := current.(type) {
		case *ast.Field:
			for _, name := range typed.Names {
				bind(name.Name)
			}
		case *ast.AssignStmt:
			if typed.Tok == token.DEFINE {
				take(typed.Lhs)
			}
		case *ast.ValueSpec:
			for _, name := range typed.Names {
				bind(name.Name)
			}
		case *ast.RangeStmt:
			if typed.Tok == token.DEFINE {
				take([]ast.Expr{typed.Key, typed.Value})
			}
		case *ast.Ident:
			if inScope(typed.Name) {
				uses[typed] = true
			}
		}
		return true
	})
	return uses
}

// namesInvokedBy returns the names a function reaches as code rather than as data: every
// call's callee by bare name, so crypto.Extract(...) contributes Extract; plus every
// identifier used as a value or as a type, so a function passed by name and a parameter
// typed CryptoProvider are both seen.
//
// Four positions are excluded, and each exclusion is the difference between a gate and a
// name collision. The selected half of a selector that is not a call is a field read:
// psk.nonce contributes psk, not nonce, and this package does declare a method called
// nonce. A composite literal key names a field of the type being built. A field
// declaration, which is also how parameter names are spelled, names a local. And a USE of a
// name the function itself bound, IN THE SCOPE THAT BINDING REACHES, is that local rather than
// the package level declaration of the same spelling -- localUsesIn is where the argument for
// the fourth is written.
func namesInvokedBy(node ast.Node) map[string]bool {
	invoked := map[string]bool{}
	positional := map[*ast.Ident]bool{}
	called := map[string]bool{}
	local := localUsesIn(node)
	ast.Inspect(node, func(current ast.Node) bool {
		switch typed := current.(type) {
		case *ast.SelectorExpr:
			positional[typed.Sel] = true
		case *ast.KeyValueExpr:
			if key, isIdent := typed.Key.(*ast.Ident); isIdent {
				positional[key] = true
			}
		case *ast.Field:
			for _, name := range typed.Names {
				positional[name] = true
			}
		case *ast.CallExpr:
			switch callee := typed.Fun.(type) {
			case *ast.Ident:
				invoked[callee.Name] = true
				called[callee.Name] = true
			case *ast.SelectorExpr:
				invoked[callee.Sel.Name] = true
				called[callee.Sel.Name] = true
			}
		}
		return true
	})
	ast.Inspect(node, func(current ast.Node) bool {
		ident, isIdent := current.(*ast.Ident)
		if !isIdent || positional[ident] {
			return true
		}
		if local[ident] && !called[ident.Name] {
			return true
		}
		invoked[ident.Name] = true
		return true
	})
	return invoked
}

// The control for namesInvokedBy, held by the test below: one function per row, and each row
// states what the matcher must say about one name in it.
const invocationMatcherControl = `
package control

type someContext struct{ TreeHash []byte }

func readsALocalOfTheSameSpelling(treeHash []byte) []byte {
	return append([]byte(nil), treeHash...)
}

func bindsALocalAndStillCallsTheFunctionOfThatName() []byte {
	marshalBytes := []byte{}
	return append(marshalBytes, marshalBytes()...)
}

func readsAFieldOfTheSameSpelling(context *someContext) []byte {
	return context.TreeHash
}

func namesAPackageFunctionAsAValue() func() []byte {
	return marshalBytes
}

func bindsALocalInABlockAndNamesTheFunctionOutside() func() []byte {
	if true {
		marshalBytes := []byte{}
		_ = marshalBytes
	}
	return marshalBytes
}

func readsAnOuterLocalInsideABlock(treeHash []byte) []byte {
	out := []byte{}
	if len(treeHash) > 0 {
		out = append(out, treeHash...)
	}
	return out
}

func callsAPackageFunction() []byte {
	return marshalBytes()
}
`

// TestTheInvocationMatcherReadsCallsAndNotLocalsOfTheSameSpelling is namesInvokedBy's own
// control, and it runs the two directions that matter: a local or a field named like a
// package level declaration is NOT an invocation of it, and a name in call position IS one
// even when the same function also binds that spelling.
//
// Without the second half the exclusion would be a way to hide a call from every gate that
// reads this matcher -- declare a local of the name and the edge disappears -- which is the
// one outcome a narrowing of a gate must not be able to produce. The control is parsed and
// not compiled, so what is read off it is the shape and nothing has to type check.
func TestTheInvocationMatcherReadsCallsAndNotLocalsOfTheSameSpelling(t *testing.T) {
	parsed, err := parser.ParseFile(token.NewFileSet(), "control.go", invocationMatcherControl, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("the invocation matcher control does not parse: %v", err)
	}
	byName := map[string]*ast.FuncDecl{}
	for _, declaration := range parsed.Decls {
		if function, isFunction := declaration.(*ast.FuncDecl); isFunction {
			byName[function.Name.Name] = function
		}
	}
	for _, row := range []struct {
		function string
		name     string
		invoked  bool
	}{
		{"readsALocalOfTheSameSpelling", "treeHash", false},
		{"bindsALocalAndStillCallsTheFunctionOfThatName", "marshalBytes", true},
		{"readsAFieldOfTheSameSpelling", "TreeHash", false},
		{"namesAPackageFunctionAsAValue", "marshalBytes", true},
		{"callsAPackageFunction", "marshalBytes", true},
		// the two halves of the scope walk, and they fail in opposite directions: the first is
		// the hole the per function set left open -- a binding in an inner block swallowed the
		// package level function named in an outer scope, where that binding does not reach --
		// and the second is what a walk that popped a scope too eagerly would break, reading an
		// ordinary use of an outer local as an invocation.
		{"bindsALocalInABlockAndNamesTheFunctionOutside", "marshalBytes", true},
		{"readsAnOuterLocalInsideABlock", "treeHash", false},
	} {
		function, declared := byName[row.function]
		if !declared {
			t.Fatalf("the control declares %v and not %s", slices.Sorted(maps.Keys(byName)), row.function)
		}
		if got := namesInvokedBy(function)[row.name]; got != row.invoked {
			t.Errorf("namesInvokedBy(%s)[%q] = %v, want %v", row.function, row.name, got, row.invoked)
		}
	}
}

// reachableNames returns every identifier a function names, following calls to functions
// declared in the same set of files. A name declared nowhere in that set is a leaf and is
// still collected, so the standard library and this package's production surface both show
// up in the result.
func reachableNames(t *testing.T, declared map[string]*ast.FuncDecl, root string) map[string]bool {
	t.Helper()
	start, ok := declared[root]
	if !ok {
		t.Fatalf("%s is not declared in the files scanned, so its closure would be empty", root)
	}
	reached := map[string]bool{}
	pending := []*ast.FuncDecl{start}
	visited := map[string]bool{root: true}
	for len(pending) > 0 {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		for name := range namesInvokedBy(current) {
			reached[name] = true
			if next, isLocal := declared[name]; isLocal && !visited[name] {
				visited[name] = true
				pending = append(pending, next)
			}
		}
	}
	return reached
}

// testFileFunctions collects the package level functions the given files declare, keyed by
// name, so reachableNames can follow a call into one.
func testFileFunctions(files map[string]*ast.File) map[string]*ast.FuncDecl {
	declared := map[string]*ast.FuncDecl{}
	for _, file := range files {
		for _, decl := range file.Decls {
			function, isFunction := decl.(*ast.FuncDecl)
			if !isFunction || function.Recv != nil {
				continue
			}
			declared[function.Name.Name] = function
		}
	}
	return declared
}

// productionFunctionNames is every function and method this package declares in a non test
// file, by bare name, derived from the package's own syntax tree.
//
// Methods appear under their bare name rather than as Receiver.Name because that is how a
// call reaches them: crypto.Extract names Extract, and a gate keyed on the qualified form
// would not see it.
func productionFunctionNames(t *testing.T) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	fileSet := token.NewFileSet()
	names := map[string]bool{}
	read := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fileSet, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		read++
		for _, decl := range parsed.Decls {
			if function, isFunction := decl.(*ast.FuncDecl); isFunction {
				names[function.Name.Name] = true
			}
		}
	}
	if read == 0 {
		t.Fatal("no production file was read, so the class below would be empty and every gate over it vacuous")
	}
	for _, control := range []string{"PskSecret", "EmptyPskSecret", "NewCryptoProvider", "Extract", "ExpandWithLabel"} {
		if !names[control] {
			t.Fatalf("the derivation did not find %s, which this package certainly declares, so it read %d files and nothing useful",
				control, read)
		}
	}
	return names
}

// siblingPackageQualifiers is every package of THIS MODULE that this package imports, by
// the identifier a call site writes: syntax today, and whatever a later plan adds.
//
// This exists because a mutation survived the gate below without it. productionFunctionNames
// reads this package's own non test files, so it holds PskSecret and ExpandWithLabel and
// knows nothing about mls/syntax -- and the codec is exactly what a hand written wire
// encoder is supposed to be a second opinion about. An "independent" group context encoder
// whose body was replaced with a syntax.Marshal call reached the code under test and the
// gate reported clean, which is the whole failure this file exists to make impossible, one
// package boundary out.
//
// What is banned is the QUALIFIER and not the function name, because the walk matches text
// and cannot tell syntax.Marshal from json.Marshal: adding Marshal to the class would flag
// every generator that serializes its cases. Banning the qualifier is both tighter and
// truer -- a derivation that answers independently has no business naming a sibling package
// of the code under test at all, whichever function of it it calls.
//
// Derived from the import statements rather than written down, because the class grows: p5
// and p6 add packages under this module and a list would exempt every one of them.
func siblingPackageQualifiers(t *testing.T) map[string]bool {
	t.Helper()
	const modulePrefix = `"github.com/urnetwork/connect/`
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	fileSet := token.NewFileSet()
	qualifiers := map[string]bool{}
	read := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fileSet, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		read++
		for _, imported := range parsed.Imports {
			if !strings.HasPrefix(imported.Path.Value, modulePrefix) {
				continue
			}
			// the identifier a call site writes: the explicit alias where there is one,
			// and the last path element otherwise.
			qualifier := strings.Trim(imported.Path.Value, `"`)
			qualifier = qualifier[strings.LastIndex(qualifier, "/")+1:]
			if imported.Name != nil {
				qualifier = imported.Name.Name
			}
			qualifiers[qualifier] = true
		}
	}
	if read == 0 {
		t.Fatal("no production file was read, so the class below would be empty and every gate over it vacuous")
	}
	if !qualifiers["syntax"] {
		t.Fatalf("the import scan read %d files and did not find %s, which this package certainly imports, so it is deriving nothing",
			read, "mls/syntax")
	}
	return qualifiers
}

// oneHopLaunderingRoot and oneHopLaunderingHelper are the control on the call graph walk:
// a generator in one file, and the helper it calls declared in another, with the code under
// test reached only from the helper.
//
// This is the exact evasion the first version of this gate was open to. The walk followed
// calls only into functions declared in the vector runner files, so every other test file
// of the package was a leaf, and moving one line -- the answer -- into a helper declared in
// psk_test.go laundered the whole production closure while the gate reported clean. The
// control is written as two sources rather than as two real files because a real one would
// have to be a function nothing calls, and the point is the file boundary, not the code.
var (
	oneHopLaunderingRoot = strings.Join([]string{
		"package control",
		"",
		"func generateControl() []byte {",
		"\treturn launderControl()",
		"}",
		"",
	}, "\n")

	oneHopLaunderingHelper = strings.Join([]string{
		"package control",
		"",
		"func launderControl() []byte {",
		"\tout, _ := PskSecret(nil, nil)",
		"\treturn out",
		"}",
		"",
	}, "\n")
)

// TestTheGenerateDirectionSharesNoCodePathWithVerify is the gate that makes the generate
// direction worth having.
//
// The trap it closes: a generator that computes its answer with PskSecret and a verifier
// that checks it with PskSecret round trip perfectly and say nothing at all about
// conformance. The property asserted is the strongest form of the fix, and BOTH of its
// classes are derived rather than described -- the production function class is read out of
// this package's own non test source, and the closure of the generate direction is walked
// over every test file of the package, so neither a production function added later nor a
// helper hidden in another test file evades it.
//
// Two controls stand in front of the gate. The verify direction is required to reach
// PskSecret, so the disjointness cannot hold by the collector having read nothing; and the
// walk is required to cross a file boundary, so it cannot hold by the walk stopping at the
// first hop.
func TestTheGenerateDirectionSharesNoCodePathWithVerify(t *testing.T) {
	fileSet := token.NewFileSet()
	control := func(name, source string) *ast.File {
		parsed, err := parser.ParseFile(fileSet, name, source, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		return parsed
	}
	rootOnly := map[string]*ast.File{"root_test.go": control("root_test.go", oneHopLaunderingRoot)}
	if reachableNames(t, testFileFunctions(rootOnly), "generateControl")["PskSecret"] {
		t.Fatal("the walk reports PskSecret from a file that only names launderControl, so it is matching text rather than following calls")
	}
	bothFiles := map[string]*ast.File{
		"root_test.go":   control("root_test.go", oneHopLaunderingRoot),
		"helper_test.go": control("helper_test.go", oneHopLaunderingHelper),
	}
	if !reachableNames(t, testFileFunctions(bothFiles), "generateControl")["PskSecret"] {
		t.Fatal("the walk did not follow a call into a function declared in another file, so the gate below is evadable by one hop through any test file")
	}

	// the class the real walk runs over: every test file, strictly more than the vector
	// runner files, or the control above is describing a walk this gate does not make.
	all := packageTestFiles(t)
	runners := vectorRunnerFiles(t)
	if len(all) <= len(runners) {
		t.Fatalf("the package has %d test files and %d of them are vector runners; the closure below must be walked over every test file and there is nothing here to widen",
			len(all), len(runners))
	}
	declared := testFileFunctions(all)
	if len(declared) <= len(testFileFunctions(runners)) {
		t.Fatalf("the walk sees %d functions over every test file and %d over the runner files alone; it is not reading the wider class",
			len(declared), len(testFileFunctions(runners)))
	}
	// the forbidden class: this package's own production functions, and the qualifier of
	// every sibling package of this module it imports. The second half is not decoration
	// -- an independent encoder rewritten as a syntax.Marshal call reaches the codec it is
	// meant to be a second opinion about, and the function name class cannot see it
	// because the codec is declared one directory over.
	production := productionFunctionNames(t)
	siblings := siblingPackageQualifiers(t)
	for qualifier := range siblings {
		if production[qualifier] {
			t.Fatalf("%s is both a production function name and a sibling package qualifier, so a hit on it says nothing about which one was reached",
				qualifier)
		}
		production[qualifier] = true
	}
	// the control on the second half: the walk must actually report the qualifier of a
	// function that names it, or banning it bans nothing.
	if !reachableNames(t, declared, "compareKeyScheduleVector")["syntax"] {
		t.Fatal("the walk does not report syntax from a function that calls syntax.Marshal, so banning the qualifier below bans nothing")
	}

	verifyReaches := reachableNames(t, declared, "comparePskSecretVector")
	if !verifyReaches["PskSecret"] {
		t.Fatal("the verify direction does not reach PskSecret, so the collector is reading nothing and the disjointness below is vacuous")
	}

	// the roots, DERIVED and not listed: every function these test files declare whose
	// name claims independence, plus the generator of family 6, which answers with the
	// hand written derivation and owes the same disjointness.
	//
	// Derived because a list covers the functions that existed when it was written. Task
	// 18 adds six independent functions to this file and later plans add their own; the
	// version of this loop that named three of them would have gone on reporting a clean
	// run over the three while a fourth reached straight into the package it is meant to
	// be a second opinion about. The naming convention IS the claim, so it is what the
	// gate reads.
	roots := []string{}
	for name := range declared {
		if strings.HasPrefix(name, "independent") {
			roots = append(roots, name)
		}
	}
	slices.Sort(roots)
	// the derivation must have found the ones this tree certainly declares, or it is
	// matching nothing and the disjointness below holds over an empty set.
	for _, required := range []string{
		"independentPskSecret", "independentPskSecretTransposed", "independentExpandWithLabel",
		"independentKdfLabel", "independentKeyScheduleSecrets", "independentGroupContext",
		"independentSecretTreeLeafSecret", "independentSecretTreeRatchet",
		"independentDeriveTreeSecret", "independentSenderDataKeyNonce",
	} {
		if !slices.Contains(roots, required) {
			t.Fatalf("the root derivation found %v and %s is not among them, so it is reading nothing useful over %d declared functions",
				roots, required, len(declared))
		}
	}
	// generatePskSecretVectors is family 6's generator and generateSecretTreeCases is family
	// 3's; both compute every answer they emit with the hand written derivations, so both are
	// held to the same rule. Family 5's generator is NOT: it takes external_pub from the
	// implementation because DeriveKeyPair is HPKE and this tree has no second X25519, and
	// TestVectorKeyScheduleGenerate is what says the rest of that generator's answers came
	// from the hand written path.
	//
	// Named rather than derived, and that is a gap worth stating: the "independent" prefix is a
	// claim a function makes about itself and these two make the same claim in their names
	// without carrying the prefix. What stops a third generator arriving unheld is
	// assertVectorFamilyIsInstalled, which requires every registered Generate to be the one its
	// own runner declares, plus each family's own generate test -- not this list.
	roots = append(roots, "generatePskSecretVectors", "generateSecretTreeCases")
	for _, root := range roots {
		shared := []string{}
		for name := range reachableNames(t, declared, root) {
			if production[name] {
				shared = append(shared, name)
			}
		}
		slices.Sort(shared)
		if len(shared) != 0 {
			t.Errorf("%s reaches the production function(s) %v; the generate direction has to answer without the code under test or the round trip only proves that code agrees with itself",
				root, shared)
		}
	}
	t.Logf("the generate direction was walked over %d functions declared across %d test files, against %d production function names; %d independent roots: %v",
		len(declared), len(all), len(production), len(roots), roots)
}

// TestNoVectorRunnerCanSkip holds every vector harness file to failing rather than skipping.
//
// A skipped gate is an absent gate, and the shape that produced this rule is specific: a
// runner that skips when the corpus is missing turns a deleted, renamed or truncated corpus
// into a green run. There is no condition under which any of these files should decline to
// run: the corpus is vendored and pinned in this tree, so it is either there or the build
// is broken.
//
// The skip class is derived from the testing package rather than listed. A list would name
// Skip and Skipf and miss SkipNow, which is exactly the shape of failure this project has
// hit fourteen times.
func TestNoVectorRunnerCanSkip(t *testing.T) {
	skips := map[string]bool{}
	notSkips := []string{}
	reported := reflect.TypeOf((*testing.T)(nil))
	for index := 0; index < reported.NumMethod(); index++ {
		if name := reported.Method(index).Name; strings.HasPrefix(name, "Skip") {
			skips[name] = true
		} else {
			notSkips = append(notSkips, name)
		}
	}
	for _, required := range []string{"Skip", "Skipf", "SkipNow"} {
		if !skips[required] {
			t.Fatalf("the derived skip class %v does not hold %s, so it is not the testing package's", slices.Sorted(maps.Keys(skips)), required)
		}
	}
	if skips["Fatal"] || skips["Fatalf"] {
		t.Fatalf("the derived skip class %v holds a fatal, so it is matching more than skips", slices.Sorted(maps.Keys(skips)))
	}
	// and the COMPLEMENT is printed rather than left to be assumed harmless. The property is
	// "a member of *testing.T that abandons a test without leaving a verdict", and nothing in
	// the list below does that -- a sentence about the removed members that a reader can check,
	// rather than an exclusion nobody sees. Empty here would mean the name test removed nothing,
	// which is a reflection that read the wrong type.
	if len(notSkips) == 0 {
		t.Fatal("every method of *testing.T is named Skip<something>, so this name test removed nothing and the class below is the whole method set")
	}
	t.Logf("the derived skip class is %v; the %d methods of *testing.T it removes, none of which abandons a test without a verdict, are %v",
		slices.Sorted(maps.Keys(skips)), len(notSkips), notSkips)

	flags := func(source string) []string {
		fileSet := token.NewFileSet()
		parsed, err := parser.ParseFile(fileSet, "control.go", source, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse the control: %v", err)
		}
		return skipsNamedIn(parsed, skips)
	}
	if found := flags("package control\nimport \"testing\"\nfunc control(t *testing.T) { t.Skip(\"no corpus\") }\n"); len(found) == 0 {
		t.Fatal("the matcher did not flag a control that skips, so it would report every file below clean having matched nothing")
	}
	if found := flags("package control\nimport \"testing\"\nfunc control(t *testing.T) { t.Fatal(\"no corpus\") }\n"); len(found) != 0 {
		t.Fatalf("the matcher flagged a control that fails rather than skipping: %v", found)
	}

	for name, parsed := range vectorRunnerFiles(t) {
		if found := skipsNamedIn(parsed, skips); len(found) != 0 {
			t.Errorf("%s names %v; a vector runner that skips when the corpus is unreadable turns a deleted corpus into a green run",
				name, found)
		}
	}
}

// skipsNamedIn returns every selector in a file whose selected name is in the skip class.
// The qualifier is not checked: over the vector harness files there is no other kind of
// Skip, and over-approximating is the safe direction for this gate to be wrong in.
func skipsNamedIn(file *ast.File, skips map[string]bool) []string {
	found := map[string]bool{}
	ast.Inspect(file, func(node ast.Node) bool {
		selector, isSelector := node.(*ast.SelectorExpr)
		if isSelector && skips[selector.Sel.Name] {
			found[selector.Sel.Name] = true
		}
		return true
	})
	return slices.Sorted(maps.Keys(found))
}

// ---------------------------------------------------------------------------
// vector family 5, key-schedule.json
// ---------------------------------------------------------------------------

// The family, and the accounting that makes its runner unable to pass having compared
// nothing.
//
// These are transcriptions of what testdata/vectors/key-schedule.json holds at the pinned
// mlswg commit: seven entries, one per published ciphersuite, five epochs each, of which
// the two suites this package registers account for two entries and ten epochs. Fourteen
// answers are compared per epoch, so 140 comparisons, and the corpus publishes 140
// DISTINCT values for them -- no two of the answers this runner checks are the same
// string -- which is what makes the distinctness assertion in the runner a real one rather
// than a count of one repeated value.
//
// Written down rather than derived, for the reason task 16 gives: deriving the expected
// count with the same filter that is under test is how a filter matching nothing ends up
// agreeing with itself. What IS derived and checked alongside them is that compared plus
// skipped equals the number of entries read.
const (
	keyScheduleFamilyVectors        = 2
	keyScheduleFamilySkipped        = 5
	keyScheduleFamilyEpochs         = 10
	keyScheduleFamilyChecksPerEpoch = 14
	keyScheduleFamilyComparisons    = keyScheduleFamilyEpochs * keyScheduleFamilyChecksPerEpoch
)

// keyScheduleVector is one entry of key-schedule.json. Binary fields stay strings for the
// reason pskSecretKatVector gives: MustHex is the single decoder and a struct holding
// []byte would need a second one at the json boundary.
type keyScheduleVector struct {
	CipherSuite       uint16             `json:"cipher_suite"`
	GroupId           string             `json:"group_id"`
	InitialInitSecret string             `json:"initial_init_secret"`
	Epochs            []keyScheduleEpoch `json:"epochs"`
}

// keyScheduleEpoch is one epoch of a key-schedule vector: the three inputs the epoch is
// advanced on, the serialized group context, and every published answer.
//
// Exporter is labelKatExporter, which crypto_labels_test.go already declares for this same
// corpus. Its Label is a string in the mlswg format while every sibling field is hex, and
// it is NOT hex decoded -- see task 8's TestKeyScheduleExportLabelIsNotHexDecoded.
type keyScheduleEpoch struct {
	TreeHash                string           `json:"tree_hash"`
	CommitSecret            string           `json:"commit_secret"`
	PskSecret               string           `json:"psk_secret"`
	ConfirmedTranscriptHash string           `json:"confirmed_transcript_hash"`
	GroupContext            string           `json:"group_context"`
	JoinerSecret            string           `json:"joiner_secret"`
	WelcomeSecret           string           `json:"welcome_secret"`
	InitSecret              string           `json:"init_secret"`
	SenderDataSecret        string           `json:"sender_data_secret"`
	EncryptionSecret        string           `json:"encryption_secret"`
	ExporterSecret          string           `json:"exporter_secret"`
	EpochAuthenticator      string           `json:"epoch_authenticator"`
	ExternalSecret          string           `json:"external_secret"`
	ConfirmationKey         string           `json:"confirmation_key"`
	MembershipKey           string           `json:"membership_key"`
	ResumptionPsk           string           `json:"resumption_psk"`
	ExternalPub             string           `json:"external_pub"`
	Exporter                labelKatExporter `json:"exporter"`
}

// keyScheduleCheckNames is every comparison this runner makes for one epoch, named by the
// json path the published answer lives at rather than by the Go field that decodes it.
//
// The json path is the load bearing choice. The runner re-reads each published answer out
// of a GENERIC decode of the same entry -- map[string]json.RawMessage, no struct tags
// involved -- and holds the comparator's answer against that, so a struct tag renamed or
// misspelled shows up here as a key the corpus does not publish rather than as a field
// that silently decodes to the empty string and compares equal to nothing.
//
// The order is the order the comparator emits them in, and incomplete() requires every
// name to appear exactly once per epoch, so a comparison dropped from the middle of an
// epoch is a failure rather than a smaller count nobody wrote down.
var keyScheduleCheckNames = []string{
	"group_context",
	"joiner_secret",
	"welcome_secret",
	"sender_data_secret",
	"encryption_secret",
	"exporter_secret",
	"external_secret",
	"confirmation_key",
	"membership_key",
	"resumption_psk",
	"epoch_authenticator",
	"init_secret",
	"external_pub",
	"exporter.secret",
}

// keyScheduleNonSecretChecks are the three of those fourteen that are not a KDF.Nh secret
// of the epoch: the serialized group context, whose width is its own contents'; the
// external HPKE public key, whose width is the suite's Npk and not the kdf's; and the
// MLS-Exporter answer, whose width is the length the vector asked for.
//
// They are named here so the secret class below can be derived by subtraction. The width
// and aliasing controls apply to the secret class only, and a class written out directly
// would be an eleven name list that a twelfth secret would silently fall out of.
var keyScheduleNonSecretChecks = []string{"group_context", "external_pub", "exporter.secret"}

// keyScheduleSecretChecks is every check whose published answer must be exactly KDF.Nh
// octets and must differ from every other secret of its own epoch, derived by subtracting
// the three above from the fourteen.
//
// Derived because the class grows with the schedule. Nine of the eleven are the fields of
// EpochSecrets and the other two sit above them, so
// TestKeyScheduleFamilyChecksAreTheWholeEpoch holds this count to that type's own field
// count plus joiner and welcome. A tenth secret added to the schedule fails there rather
// than quietly leaving this list a name short.
func keyScheduleSecretChecks() []string {
	names := []string{}
	for _, name := range keyScheduleCheckNames {
		if !slices.Contains(keyScheduleNonSecretChecks, name) {
			names = append(names, name)
		}
	}
	return names
}

// Family 5 is installed here, and 5 is deleted from expectedPendingFamilies in the same
// commit. Without both halves TestVectorFamiliesVerify runs one fewer family and the
// manifest gate stays green while claiming this family is unimplemented.
func init() {
	RegisterVectorFamily(VectorFamily{
		Number:   5,
		Name:     "Key schedule",
		File:     keyScheduleKatFile,
		Slice:    "A3",
		Verify:   verifyKeyScheduleVector,
		Generate: generateKeyScheduleVector,
	})
}

// The refusals compareKeyScheduleVector makes, as sentinels rather than as formatted
// strings, so a test can require a specific refusal rather than "some error".
//
// They are what makes the comparison observable at all. Every entry of the vendored corpus
// agrees with this implementation, so a comparator that checked everything and a
// comparator that checked nothing produce identical runs over it; the only way to tell
// them apart is to hand it an answer that is wrong on purpose and require the matching
// refusal, which is TestCompareKeyScheduleVectorRefusesAnAnswerItShouldNotAccept.
var (
	errKeyScheduleWidth      = errors.New("a published key schedule secret is not the suite's KDF.Nh")
	errKeyScheduleAliased    = errors.New("two published secrets of one epoch are the same value")
	errKeyScheduleMismatch   = errors.New("a key schedule answer does not match the published one")
	errKeyScheduleDidNotMove = errors.New("flipping one octet of the published commit secret left joiner_secret unchanged")
	errKeyScheduleIncomplete = errors.New("the comparison reports values it cannot have computed")
)

// keyScheduleCheck is one answer this package computed held against one answer the corpus
// published, filed under the json path the published half lives at.
type keyScheduleCheck struct {
	epoch int
	name  string
	got   []byte
	want  []byte
}

// keyScheduleComparison is what one run of compareKeyScheduleVector PRODUCED, and it is
// the only thing its callers are allowed to judge it by.
//
// The shape is task 16's and it is here for task 16's reason. A comparator returning a
// bool reports that control reached the bottom of the function and not that a comparison
// happened: an early return above it leaves the runner counting vectors that never called
// NewKeySchedule at all, and the run stays green. Every field below is written at the
// point the work that produces it happens, so a return that skipped the work reports the
// zero value, and a caller that judges the values rather than the fact of returning sees
// that.
type keyScheduleComparison struct {
	// inScope is true when the vector's ciphersuite is one this package registers. A
	// false here is not a failure and not a skip: it is a vector with no provider.
	inScope bool
	// hashSize is the suite's KDF.Nh, read off the provider rather than assumed.
	hashSize int
	// epochs is how many epochs of the vector were advanced through.
	epochs int
	// checks is every comparison the run made, in the order it made them.
	checks []keyScheduleCheck
	// joiner is epoch 0's joiner secret, and perturbed is the same with one octet of the
	// vector's own commit_secret flipped. Equal means the corpus data never reached the
	// derivation.
	joiner    []byte
	perturbed []byte
}

// incomplete reports whether the evidence a compared vector must carry is missing or
// inconsistent, without looking at whether any answer was right.
//
// This is the vacuity half, split from the correctness half on purpose. bytes.Equal over
// two empty slices says they agree, so a check whose got or want is empty has compared
// nothing whatever the comparison would say about it -- and a runner that counted such
// checks would report the full 140 having derived none of them.
func (self keyScheduleComparison) incomplete() error {
	switch {
	case !self.inScope:
		return fmt.Errorf("%w: the vector is out of scope and carries no comparison", errKeyScheduleIncomplete)
	case self.hashSize == 0:
		return fmt.Errorf("%w: no KDF.Nh was read from the provider", errKeyScheduleIncomplete)
	case self.epochs == 0:
		return fmt.Errorf("%w: no epoch was advanced through", errKeyScheduleIncomplete)
	case len(self.checks) != self.epochs*keyScheduleFamilyChecksPerEpoch:
		return fmt.Errorf("%w: %d epochs produced %d comparisons and each epoch owes %d",
			errKeyScheduleIncomplete, self.epochs, len(self.checks), keyScheduleFamilyChecksPerEpoch)
	case len(self.joiner) != self.hashSize || len(self.perturbed) != self.hashSize:
		return fmt.Errorf("%w: the flipped octet control was never run", errKeyScheduleIncomplete)
	}
	// every name exactly once per epoch, so a comparison dropped from the middle of an
	// epoch cannot be made up for by another one made twice.
	seen := map[string]int{}
	for _, check := range self.checks {
		if len(check.got) == 0 || len(check.want) == 0 {
			return fmt.Errorf("%w: epoch %d %s compared %d computed octets against %d published ones, and an empty comparison agrees with anything",
				errKeyScheduleIncomplete, check.epoch, check.name, len(check.got), len(check.want))
		}
		if check.epoch < 0 || check.epoch >= self.epochs {
			return fmt.Errorf("%w: a comparison is filed under epoch %d of %d",
				errKeyScheduleIncomplete, check.epoch, self.epochs)
		}
		seen[check.name]++
	}
	for _, name := range keyScheduleCheckNames {
		if seen[name] != self.epochs {
			return fmt.Errorf("%w: %s was compared %d times over %d epochs",
				errKeyScheduleIncomplete, name, seen[name], self.epochs)
		}
	}
	if len(seen) != keyScheduleFamilyChecksPerEpoch {
		return fmt.Errorf("%w: the run compared %d distinct answers per epoch and this family checks %d",
			errKeyScheduleIncomplete, len(seen), keyScheduleFamilyChecksPerEpoch)
	}
	return nil
}

// verdict is the whole judgement over one compared vector: it must be complete, every
// published secret must be the suite's width, no two secrets of one epoch may be the same
// value, every comparison must agree, and the vacuity control must have moved.
//
// The order is deliberate. A width failure and an aliasing failure are both statements
// that this is not the comparison the corpus intends, and reporting either as a plain
// mismatch would let a test asking for one of them be satisfied by the other.
func (self keyScheduleComparison) verdict() error {
	if err := self.incomplete(); err != nil {
		return err
	}
	secrets := keyScheduleSecretChecks()
	for _, check := range self.checks {
		if !slices.Contains(secrets, check.name) {
			continue
		}
		if len(check.want) != self.hashSize {
			return fmt.Errorf("%w: epoch %d %s is %d octets against a KDF.Nh of %d",
				errKeyScheduleWidth, check.epoch, check.name, len(check.want), self.hashSize)
		}
	}
	// the nine secrets of an epoch are DeriveSecret over one epoch_secret under nine
	// different labels, and joiner and welcome sit above them. All eleven are KDF.Nh
	// octets of apparent random, so a label copied from the line above produces a
	// perfectly well formed secret; two published answers holding one value would be a
	// corpus this comparison cannot tell those two labels apart with.
	published := map[int]map[string]string{}
	for _, check := range self.checks {
		if !slices.Contains(secrets, check.name) {
			continue
		}
		epoch, started := published[check.epoch]
		if !started {
			epoch = map[string]string{}
			published[check.epoch] = epoch
		}
		text := HexOf(check.want)
		if previous, duplicated := epoch[text]; duplicated {
			return fmt.Errorf("%w: epoch %d publishes %s for both %s and %s",
				errKeyScheduleAliased, check.epoch, text, previous, check.name)
		}
		epoch[text] = check.name
	}
	for _, check := range self.checks {
		if !bytes.Equal(check.got, check.want) {
			return fmt.Errorf("%w: epoch %d %s = %s, the corpus publishes %s",
				errKeyScheduleMismatch, check.epoch, check.name, HexOf(check.got), HexOf(check.want))
		}
	}
	if bytes.Equal(self.perturbed, self.joiner) {
		return fmt.Errorf("%w: %s, so the corpus data never reached the derivation",
			errKeyScheduleDidNotMove, HexOf(self.joiner))
	}
	return nil
}

// verifyKeyScheduleVector is the registry's shim: the signature RegisterVectorFamily needs,
// over the comparator that does the work and reports what it produced.
//
// The split is the whole point, and it is the defect task 16 shipped and then had to fix.
// Verify cannot return anything, so a runner counting calls to it would count a vector it
// declined to check exactly as it counts one it compared.
func verifyKeyScheduleVector(t *testing.T, raw json.RawMessage) {
	t.Helper()
	evidence, err := compareKeyScheduleVector(t, raw)
	if err != nil {
		t.Fatalf("key-schedule: %v", err)
	}
	if !evidence.inScope {
		return
	}
	if err := evidence.verdict(); err != nil {
		t.Fatalf("key-schedule: %v", err)
	}
}

// compareKeyScheduleVector runs one entry of key-schedule.json and returns what the run
// produced. A vector at a ciphersuite v1 does not implement is not a failure and not a
// skip: it comes back with inScope false and nothing else set.
//
// The chain is carried forward with OUR init_secret rather than re-seeded from the vector
// at each epoch, so a divergence surfaces at the epoch that caused it instead of being
// masked by the next reseed. Only initial_init_secret is read from the vector.
//
// A corpus that will not parse or will not hex decode is fatal here rather than returned,
// because it is not a verdict about this implementation -- it is the evidence itself being
// unreadable. Everything that IS a verdict about this implementation is returned, so a
// caller can require a refusal instead of hoping the corpus disagrees with a defect.
func compareKeyScheduleVector(t *testing.T, raw json.RawMessage) (keyScheduleComparison, error) {
	t.Helper()
	vector := keyScheduleVector{}
	if err := json.Unmarshal(raw, &vector); err != nil {
		t.Fatalf("parse key-schedule entry: %v", err)
	}
	suite, ok := implementedSuite(vector.CipherSuite)
	if !ok {
		return keyScheduleComparison{}, nil
	}
	crypto, err := NewCryptoProvider(suite)
	if err != nil {
		t.Fatalf("NewCryptoProvider(%#04x): %v", uint16(suite), err)
	}
	evidence := keyScheduleComparison{
		inScope:  true,
		hashSize: crypto.HashSize(),
		epochs:   len(vector.Epochs),
	}
	initSecret := MustHex(t, vector.InitialInitSecret)
	if len(initSecret) != evidence.hashSize {
		return evidence, fmt.Errorf("%w: initial_init_secret is %d octets against a KDF.Nh of %d",
			errKeyScheduleWidth, len(initSecret), evidence.hashSize)
	}

	for n, epoch := range vector.Epochs {
		groupContext := &GroupContext{
			Version:                 ProtocolVersionMls10,
			CipherSuite:             suite,
			GroupId:                 MustHex(t, vector.GroupId),
			Epoch:                   uint64(n),
			TreeHash:                MustHex(t, epoch.TreeHash),
			ConfirmedTranscriptHash: MustHex(t, epoch.ConfirmedTranscriptHash),
			Extensions:              nil,
		}
		encoded, err := syntax.Marshal(groupContext)
		if err != nil {
			return evidence, fmt.Errorf("epoch %d: syntax.Marshal: %w", n, err)
		}
		commitSecret := MustHex(t, epoch.CommitSecret)
		pskSecret := MustHex(t, epoch.PskSecret)
		schedule, err := NewKeySchedule(crypto, initSecret, commitSecret, pskSecret, groupContext)
		if err != nil {
			return evidence, fmt.Errorf("epoch %d: NewKeySchedule: %w", n, err)
		}
		secrets := schedule.Secrets()
		_, externalPub, err := schedule.ExternalKeyPair()
		if err != nil {
			return evidence, fmt.Errorf("epoch %d: ExternalKeyPair: %w", n, err)
		}
		exported, err := schedule.Export(
			epoch.Exporter.Label, MustHex(t, epoch.Exporter.Context), epoch.Exporter.Length)
		if err != nil {
			return evidence, fmt.Errorf("epoch %d: Export: %w", n, err)
		}
		// the order here is keyScheduleCheckNames' order, which incomplete() holds it to.
		for _, check := range []struct {
			name string
			got  []byte
			want string
		}{
			{"group_context", encoded, epoch.GroupContext},
			{"joiner_secret", schedule.JoinerSecret(), epoch.JoinerSecret},
			{"welcome_secret", schedule.WelcomeSecret(), epoch.WelcomeSecret},
			{"sender_data_secret", secrets.SenderData, epoch.SenderDataSecret},
			{"encryption_secret", secrets.Encryption, epoch.EncryptionSecret},
			{"exporter_secret", secrets.Exporter, epoch.ExporterSecret},
			{"external_secret", secrets.External, epoch.ExternalSecret},
			{"confirmation_key", secrets.Confirmation, epoch.ConfirmationKey},
			{"membership_key", secrets.Membership, epoch.MembershipKey},
			{"resumption_psk", secrets.ResumptionPsk, epoch.ResumptionPsk},
			{"epoch_authenticator", secrets.EpochAuthenticator, epoch.EpochAuthenticator},
			{"init_secret", secrets.InitSecret, epoch.InitSecret},
			{"external_pub", externalPub, epoch.ExternalPub},
			{"exporter.secret", exported, epoch.Exporter.Secret},
		} {
			evidence.checks = append(evidence.checks, keyScheduleCheck{
				epoch: n,
				name:  check.name,
				got:   bytes.Clone(check.got),
				want:  MustHex(t, check.want),
			})
		}

		// the vacuity control, run on the first epoch: one octet of the corpus's own
		// commit_secret, flipped, must move joiner_secret. An agreement that survives this
		// was not computed from the corpus -- a renamed json field, a struct tag typo, a
		// decoder returning nothing all leave the answer where it was.
		if n == 0 {
			evidence.joiner = bytes.Clone(schedule.JoinerSecret())
			if len(commitSecret) == 0 {
				return evidence, fmt.Errorf("%w: epoch 0 publishes no commit_secret to flip", errKeyScheduleIncomplete)
			}
			flipped := bytes.Clone(commitSecret)
			flipped[0] ^= 0x01
			moved, err := NewKeySchedule(crypto, initSecret, flipped, pskSecret, groupContext)
			if err != nil {
				return evidence, fmt.Errorf("epoch 0: NewKeySchedule over the flipped commit secret: %w", err)
			}
			evidence.perturbed = bytes.Clone(moved.JoinerSecret())
		}

		// carry our own init_secret forward, not the vector's.
		initSecret = bytes.Clone(secrets.InitSecret)
	}
	return evidence, evidence.verdict()
}

// The generic decode this family reads its published answers back through is
// publishedCorpusField in vectors_runner_test.go, shared with every other family for the
// reason the comment there gives: one decoder over one corpus, so two of them cannot end up
// disagreeing about which json key an answer lives at. The dotted path this family addresses
// it with -- exporter.secret -- is why that helper takes one.

// TestVectorKeySchedule is vector family 5 over the published corpus.
//
// Every assertion vectorRunTally makes after the loop exists because the loop can be made to
// run zero times without anything else in this package noticing. A filter that matched
// nothing, a filter that matched all seven published suites, a corpus that parsed to an empty
// array, a comparator that declined every vector: each of those is a green run of this test
// with the accounting removed, and a failure with it.
//
// What the loop counts is not calls that returned. It counts comparisons whose evidence
// this runner itself re-checked against a generic decode of the corpus text, so a
// comparator that answered without computing anything is a failure here rather than a
// number that looks right.
func TestVectorKeySchedule(t *testing.T) {
	tally, entries := newVectorRunTally(t, keyScheduleKatFile)
	epochs := 0
	for index, raw := range entries {
		header := struct {
			CipherSuite uint16                       `json:"cipher_suite"`
			Epochs      []map[string]json.RawMessage `json:"epochs"`
		}{}
		if err := json.Unmarshal(raw, &header); err != nil {
			t.Fatalf("vector %d: %v", index, err)
		}
		suite, inScope := tally.filter(header.CipherSuite)
		if !inScope {
			continue
		}
		evidence, err := compareKeyScheduleVector(t, raw)
		if err != nil {
			t.Fatalf("vector %d (suite %#04x): %v", index, header.CipherSuite, err)
		}
		tally.requireCompared(t, index, suite, evidence.inScope)
		if err := evidence.verdict(); err != nil {
			t.Fatalf("vector %d (suite %#04x): %v", index, header.CipherSuite, err)
		}
		if evidence.epochs != len(header.Epochs) {
			t.Fatalf("vector %d publishes %d epochs and the comparator advanced through %d",
				index, len(header.Epochs), evidence.epochs)
		}
		// and this runner's own half of the comparison, against the answers it read out
		// of the corpus text itself with no struct tag in the way.
		for _, check := range evidence.checks {
			want := publishedCorpusField(t, header.Epochs[check.epoch], check.name)
			if got := HexOf(check.got); got != want {
				t.Fatalf("vector %d (suite %#04x) epoch %d: this package computes %s for %s, the corpus publishes %s",
					index, header.CipherSuite, check.epoch, got, check.name, want)
			}
			tally.answer(want)
		}
		epochs += evidence.epochs
	}

	// the epoch count is this family's own and not the tally's: the corpus is entries of
	// five epochs each, so a run that advanced through one epoch of each entry satisfies
	// every count the tally holds and covers a fifth of the schedule.
	if epochs != keyScheduleFamilyEpochs {
		t.Fatalf("advanced through %d epochs, want %d", epochs, keyScheduleFamilyEpochs)
	}
	// the corpus publishes 140 distinct answers for the 140 comparisons, so a file read as
	// one repeated value -- every field decoding to the same string, every epoch decoding
	// as epoch 0 -- compares the right number of times against the wrong number of answers.
	tally.assertRun(t, keyScheduleFamilyVectors, keyScheduleFamilySkipped,
		keyScheduleFamilyComparisons, keyScheduleFamilyComparisons)
}

// TestKeyScheduleFamilyChecksAreTheWholeEpoch holds the fourteen names this family compares
// to what an epoch actually holds, so a secret added to the schedule cannot arrive without
// a comparison.
//
// The secret class is derived from EpochSecrets by reflection rather than counted here: the
// nine fields of that type are exactly the DeriveSecret outputs of one epoch, and joiner and
// welcome are the two that sit above them. A tenth field added to EpochSecrets fails here
// rather than leaving this family comparing thirteen of fourteen answers and reporting a
// clean run.
func TestKeyScheduleFamilyChecksAreTheWholeEpoch(t *testing.T) {
	if len(keyScheduleCheckNames) != keyScheduleFamilyChecksPerEpoch {
		t.Fatalf("this family names %d checks per epoch and the count it asserts is %d",
			len(keyScheduleCheckNames), keyScheduleFamilyChecksPerEpoch)
	}
	distinct := slices.Compact(slices.Sorted(slices.Values(keyScheduleCheckNames)))
	if len(distinct) != len(keyScheduleCheckNames) {
		t.Fatalf("the check names hold %d distinct entries out of %d, so one is compared twice and another not at all",
			len(distinct), len(keyScheduleCheckNames))
	}
	for _, name := range keyScheduleNonSecretChecks {
		if !slices.Contains(keyScheduleCheckNames, name) {
			t.Fatalf("%s is excluded from the secret class and is not one of the checks, so the subtraction below removes nothing",
				name)
		}
	}
	secrets := keyScheduleSecretChecks()
	// the nine of EpochSecrets, plus joiner_secret and welcome_secret.
	epochSecrets := reflect.TypeOf(EpochSecrets{})
	want := epochSecrets.NumField() + 2
	if len(secrets) != want {
		t.Fatalf("this family compares %d KDF.Nh secrets per epoch and an epoch holds %d (%d fields of %s, plus joiner and welcome): %v",
			len(secrets), want, epochSecrets.NumField(), epochSecrets.Name(), secrets)
	}
	for _, required := range []string{"joiner_secret", "welcome_secret", "init_secret", "confirmation_key"} {
		if !slices.Contains(secrets, required) {
			t.Fatalf("the secret class %v does not hold %s", secrets, required)
		}
	}
	for _, excluded := range keyScheduleNonSecretChecks {
		if slices.Contains(secrets, excluded) {
			t.Fatalf("%s is in the secret class and is not a KDF.Nh secret, so the width control would refuse a correct corpus", excluded)
		}
	}
}

// TestKeyScheduleFamilyIsInstalled is the registration half of task 17.
//
// Registering the family and deleting its number from expectedPendingFamilies are two
// edits, and doing only the first leaves TestVectorManifestIsComplete failing while doing
// only the second leaves it passing with the family uninstalled. assertVectorFamilyIsInstalled
// asserts both, and asserts the runner and generator installed are this file's.
func TestKeyScheduleFamilyIsInstalled(t *testing.T) {
	assertVectorFamilyIsInstalled(t, 5, keyScheduleKatFile, verifyKeyScheduleVector, generateKeyScheduleVector)
}

// TestCompareKeyScheduleVectorRefusesAnAnswerItShouldNotAccept is the control the runner
// cannot be: it hands the comparator vectors that are wrong in each of the ways the corpus
// is not, and requires the matching refusal.
//
// Why this test rather than more assertions in the runner. Every comparison the runner
// makes is over a corpus that agrees with this implementation, so a comparator that
// accepted everything and a comparator that checked everything produce identical runs
// there. The only way to see the difference is to disagree with it on purpose.
//
// The driver is assertComparatorRefuses, shared with every other family: it checks the
// unmodified vector FIRST, which is the reason the refusals mean anything, since a
// comparator that refused everything would satisfy all of them. The evidence the unmodified
// vector carries is checked here first as well, because "returned no error" and "compared
// something" are not the same claim.
func TestCompareKeyScheduleVectorRefusesAnAnswerItShouldNotAccept(t *testing.T) {
	base := keyScheduleVector{}
	found := false
	for _, raw := range LoadVectorFile(t, keyScheduleKatFile) {
		candidate := keyScheduleVector{}
		if err := json.Unmarshal(raw, &candidate); err != nil {
			t.Fatalf("parse a key-schedule entry: %v", err)
		}
		if _, ok := implementedSuite(candidate.CipherSuite); ok && len(candidate.Epochs) >= 2 {
			base, found = candidate, true
			break
		}
	}
	if !found {
		t.Fatal("no published entry at a registered suite carries two or more epochs, so this control has nothing to corrupt")
	}

	encode := func(vector keyScheduleVector) json.RawMessage {
		body, err := json.Marshal(vector)
		if err != nil {
			t.Fatalf("marshal the vector under test: %v", err)
		}
		return body
	}
	// the epoch slice is copied before it is edited, so a case cannot see the previous
	// case's corruption through the shared backing array.
	corrupt := func(index int, edit func(*keyScheduleEpoch)) keyScheduleVector {
		copied := base
		copied.Epochs = slices.Clone(base.Epochs)
		epoch := copied.Epochs[index]
		edit(&epoch)
		copied.Epochs[index] = epoch
		return copied
	}
	flipHex := func(text string) string {
		octets := MustHex(t, text)
		if len(octets) == 0 {
			t.Fatalf("nothing to flip in %q", text)
		}
		octets[0] ^= 0x01
		return HexOf(octets)
	}

	evidence, err := compareKeyScheduleVector(t, encode(base))
	if err != nil {
		t.Fatalf("the unmodified published vector was refused: %v", err)
	}
	if !evidence.inScope || len(evidence.checks) == 0 {
		t.Fatalf("the unmodified published vector produced %+v, which carries no comparison", evidence)
	}
	if want := len(base.Epochs) * keyScheduleFamilyChecksPerEpoch; len(evidence.checks) != want {
		t.Fatalf("the unmodified published vector produced %d comparisons over %d epochs, want %d",
			len(evidence.checks), len(base.Epochs), want)
	}

	noEpochs := base
	noEpochs.Epochs = nil
	otherGroup := base
	otherGroup.GroupId = flipHex(base.GroupId)
	otherStart := base
	otherStart.InitialInitSecret = flipHex(base.InitialInitSecret)

	refusals := []comparatorRefusal{}
	for _, corrupted := range []struct {
		name   string
		vector keyScheduleVector
		want   error
	}{
		{"one flipped octet of a published init_secret", corrupt(1, func(e *keyScheduleEpoch) {
			e.InitSecret = flipHex(e.InitSecret)
		}), errKeyScheduleMismatch},
		{"one flipped octet of a published group_context", corrupt(0, func(e *keyScheduleEpoch) {
			e.GroupContext = flipHex(e.GroupContext)
		}), errKeyScheduleMismatch},
		{"one flipped octet of the published external_pub", corrupt(0, func(e *keyScheduleEpoch) {
			e.ExternalPub = flipHex(e.ExternalPub)
		}), errKeyScheduleMismatch},
		{"one flipped octet of the published exporter answer", corrupt(0, func(e *keyScheduleEpoch) {
			e.Exporter.Secret = flipHex(e.Exporter.Secret)
		}), errKeyScheduleMismatch},
		{"an exporter label that is not the one the answer was computed under", corrupt(0, func(e *keyScheduleEpoch) {
			e.Exporter.Label = e.Exporter.Label + "x"
		}), errKeyScheduleMismatch},
		{"an exporter context that is not the one the answer was computed under", corrupt(0, func(e *keyScheduleEpoch) {
			e.Exporter.Context = flipHex(e.Exporter.Context)
		}), errKeyScheduleMismatch},
		{"a published joiner_secret one octet short of KDF.Nh", corrupt(0, func(e *keyScheduleEpoch) {
			e.JoinerSecret = e.JoinerSecret[:len(e.JoinerSecret)-2]
		}), errKeyScheduleWidth},
		{"the confirmation key published as the membership key", corrupt(0, func(e *keyScheduleEpoch) {
			e.MembershipKey = e.ConfirmationKey
		}), errKeyScheduleAliased},
		{"a tree hash the group context was not built over", corrupt(0, func(e *keyScheduleEpoch) {
			e.TreeHash = flipHex(e.TreeHash)
		}), errKeyScheduleMismatch},
		{"a psk secret the epoch was not advanced on", corrupt(0, func(e *keyScheduleEpoch) {
			e.PskSecret = flipHex(e.PskSecret)
		}), errKeyScheduleMismatch},
		{"a vector with no epochs at all", noEpochs, errKeyScheduleIncomplete},
		{"a group id that is not the one the answers were computed for", otherGroup, errKeyScheduleMismatch},
		{"an initial init secret that is not the one the chain starts from", otherStart, errKeyScheduleMismatch},
	} {
		refusals = append(refusals, comparatorRefusal{corrupted.name, encode(corrupted.vector), corrupted.want})
	}
	assertComparatorRefuses(t, "key-schedule",
		func(t *testing.T, raw json.RawMessage) error {
			_, err := compareKeyScheduleVector(t, raw)
			return err
		},
		encode(base), refusals)
}

// TestKeyScheduleComparisonCannotReportAComparisonItDidNotMake is the control on the
// evidence struct itself: a return that skipped the work must be refused on every caller's
// path rather than counted as a comparison that agreed.
func TestKeyScheduleComparisonCannotReportAComparisonItDidNotMake(t *testing.T) {
	octets := func(seed byte) []byte { return bytes.Repeat([]byte{seed}, sha256.Size) }
	full := keyScheduleComparison{
		inScope:   true,
		hashSize:  sha256.Size,
		epochs:    1,
		joiner:    octets(0x01),
		perturbed: octets(0x02),
	}
	for index, name := range keyScheduleCheckNames {
		body := octets(byte(0x10 + index))
		full.checks = append(full.checks, keyScheduleCheck{epoch: 0, name: name, got: body, want: body})
	}
	if err := full.verdict(); err != nil {
		t.Fatalf("a complete and agreeing comparison was refused: %v; every case below would then pass for the wrong reason", err)
	}

	without := func(edit func(*keyScheduleComparison)) keyScheduleComparison {
		partial := full
		partial.checks = slices.Clone(full.checks)
		for i, check := range partial.checks {
			check.got = bytes.Clone(check.got)
			check.want = bytes.Clone(check.want)
			partial.checks[i] = check
		}
		partial.joiner = bytes.Clone(full.joiner)
		partial.perturbed = bytes.Clone(full.perturbed)
		edit(&partial)
		return partial
	}
	for _, missing := range []struct {
		name string
		edit func(*keyScheduleComparison)
	}{
		{"a comparison that returned before anything was set", func(c *keyScheduleComparison) { *c = keyScheduleComparison{} }},
		{"in scope and nothing else", func(c *keyScheduleComparison) { *c = keyScheduleComparison{inScope: true} }},
		{"no KDF.Nh read from the provider", func(c *keyScheduleComparison) { c.hashSize = 0 }},
		{"no epoch advanced through", func(c *keyScheduleComparison) { c.epochs = 0 }},
		{"one comparison short of the epoch", func(c *keyScheduleComparison) { c.checks = c.checks[:len(c.checks)-1] }},
		{"one comparison made twice in place of another", func(c *keyScheduleComparison) { c.checks[1] = c.checks[0] }},
		{"a computed value that was never derived", func(c *keyScheduleComparison) { c.checks[3].got = nil }},
		{"a published value that decoded to nothing", func(c *keyScheduleComparison) { c.checks[3].want = nil }},
		{"no flipped octet control", func(c *keyScheduleComparison) { c.perturbed = nil }},
		{"a comparison filed under an epoch the run never reached", func(c *keyScheduleComparison) { c.checks[2].epoch = 9 }},
	} {
		partial := without(missing.edit)
		err := partial.verdict()
		if err == nil {
			t.Errorf("%s was accepted as a comparison", missing.name)
			continue
		}
		if !errors.Is(err, errKeyScheduleIncomplete) {
			t.Errorf("%s was refused as %v, want an incompleteness", missing.name, err)
		}
	}

	// and the correctness half, which incompleteness must not be standing in for.
	disagreeing := without(func(c *keyScheduleComparison) { c.checks[5].got[0] ^= 0x01 })
	if err := disagreeing.verdict(); !errors.Is(err, errKeyScheduleMismatch) {
		t.Errorf("a complete comparison whose computed value disagrees was judged %v, want a mismatch", err)
	}
	narrow := without(func(c *keyScheduleComparison) { c.checks[5].want = c.checks[5].want[:sha256.Size-1] })
	if err := narrow.verdict(); !errors.Is(err, errKeyScheduleWidth) {
		t.Errorf("a published secret one octet short of KDF.Nh was judged %v, want a width refusal", err)
	}
	aliased := without(func(c *keyScheduleComparison) { c.checks[8].want = bytes.Clone(c.checks[7].want) })
	if err := aliased.verdict(); !errors.Is(err, errKeyScheduleAliased) {
		t.Errorf("two published secrets of one epoch holding one value was judged %v, want an aliasing refusal", err)
	}
	stuck := without(func(c *keyScheduleComparison) { c.perturbed = bytes.Clone(c.joiner) })
	if err := stuck.verdict(); !errors.Is(err, errKeyScheduleDidNotMove) {
		t.Errorf("a comparison whose flipped octet control did not move was judged %v, want that refusal", err)
	}
}

// ---------------------------------------------------------------------------
// family 5's generate direction, answered by the hand written derivation
// ---------------------------------------------------------------------------

// keyScheduleRfcLabels is the DeriveSecret label RFC 9420 section 8 names for each of the
// nine secrets an epoch_secret expands into, keyed by the json field the corpus publishes
// the answer under.
//
// Transcribed from the RFC's own key schedule diagram and NOT from newKeyScheduleFromParts,
// which is the point of the whole file below: all nine are KDF.Nh octets of apparent
// random, so a label copied from the line above produces a perfectly well formed secret
// that agrees with nobody, and the only thing that can see it is a second transcription
// of the text. TestTheIndependentKeyScheduleCoversEverySecretTheEpochHolds holds this map
// to the field count of EpochSecrets so a tenth secret cannot arrive unlabelled here.
var keyScheduleRfcLabels = map[string]string{
	"sender_data_secret":  "sender data",
	"encryption_secret":   "encryption",
	"exporter_secret":     "exporter",
	"external_secret":     "external",
	"confirmation_key":    "confirm",
	"membership_key":      "membership",
	"resumption_psk":      "resumption",
	"epoch_authenticator": "authentication",
	"init_secret":         "init",
}

// keyScheduleGeneratedSuites is the two ciphersuites the generator emits at, as constants
// rather than as a read of the registry.
//
// A generator that asked the registry which suites to cover would cover whatever the
// registry answered and could never report that it had stopped covering it.
// TestGeneratedKeyScheduleVectorsCoverEveryRegisteredSuite is what fails on the day a third
// suite is registered and this list stops matching.
var keyScheduleGeneratedSuites = []uint16{
	uint16(CipherSuiteX25519AesGcm128Sha256Ed25519),
	uint16(CipherSuiteX25519ChaCha20Sha256Ed25519),
}

// How many epochs each generated vector carries, and what the generate direction therefore
// owes in comparisons.
//
// Three rather than one, because the chain is where the interesting failure lives: an
// implementation that carried the wrong secret forward, or re-seeded from the vector
// instead of from its own init_secret, answers epoch 0 perfectly.
const (
	keyScheduleGeneratedEpochs = 3
	// every check of an epoch except group_context, whose second opinion is the hand
	// written group context encoder rather than a kdf derivation, and external_pub, which
	// is DeriveKeyPair over external_secret and is HPKE rather than kdf.
	keyScheduleIndependentChecksPerEpoch = keyScheduleFamilyChecksPerEpoch - 2
)

// independentKdfLabel is RFC 9420 section 5.1's KDFLabel, serialized by hand:
//
//	struct {
//	    uint16 length;
//	    opaque label<V>;
//	    opaque context<V>;
//	} KDFLabel;
//
// with the label carrying the "MLS 1.0 " prefix and length being the number of octets
// ExpandWithLabel was asked to produce. So for the 19 octet label "MLS 1.0 derived psk"
// and a 71 octet context the bytes are
//
//	00 20      the requested output length, 32, big endian uint16
//	13         label<V> byte length 19; 19 < 64 so one octet, prefix bits 0b00
//	4d 4c ..   the 19 label octets
//	40 47      context<V> byte length 71; 71 > 63 so two octets: 0x40|(71>>8), 71&0xff
//	01 20 ..   the 71 context octets
//
// This is the one encoder in this file that has no other witness on this side, and every
// way of getting it wrong produces a well formed answer: a length field holding the label's
// size instead of the output's, a label without the prefix, the two opaque fields
// transposed, or a one octet length written where two are owed all give 32 octets of
// apparent random. Nothing separates them except a value somebody else published, which is
// what the two corpora this encoder is held against supply.
//
// There is exactly ONE hand written KDFLabel encoder in this package's tests and this is
// it: independentExpandWithLabel calls it, so the psk_secret corpus pins these bytes over
// "derived psk" and the key schedule corpus pins them over the eleven labels of section 8.
// A second hand encoder would be a second opinion nothing checks.
func independentKdfLabel(t *testing.T, label string, context []byte, length int) []byte {
	t.Helper()
	encoded := binary.BigEndian.AppendUint16(nil, uint16(length))
	encoded = append(encoded, independentOpaqueV(t, []byte("MLS 1.0 "+label))...)
	return append(encoded, independentOpaqueV(t, context)...)
}

// independentExtract is RFC 5869 section 2.2's HKDF-Extract:
//
//	HKDF-Extract(salt, IKM) = HMAC-Hash(key = salt, data = IKM)
//
// The salt goes in the KEY position and the input keying material in the DATA position,
// and that is the whole reason this exists as a named function rather than as two lines at
// each call site. Every Extract in the key schedule takes two KDF.Nh pseudorandom secrets,
// so transposing them compiles, returns 32 octets, and satisfies every property either
// side could assert about its own output.
// TestTheIndependentKeyScheduleSeesATransposedExtract requires this derivation to disagree
// with its own transposition, so it cannot agree with a transposed implementation by being
// transposed the same way.
func independentExtract(salt []byte, ikm []byte) []byte {
	extract := hmac.New(sha256.New, salt)
	extract.Write(ikm)
	return extract.Sum(nil)
}

// independentDeriveSecret is RFC 9420 section 8's DeriveSecret:
//
//	DeriveSecret(Secret, Label) = ExpandWithLabel(Secret, Label, "", KDF.Nh)
//
// The context is the EMPTY string and not an absent field, so the KDFLabel still carries a
// context<V> and that vector's length octet is 0x00. A derivation that omitted the octet
// entirely would be one byte short of every peer's preimage.
func independentDeriveSecret(t *testing.T, secret []byte, label string) []byte {
	t.Helper()
	return independentExpandWithLabel(t, secret, label, nil, sha256.Size)
}

// independentGroupContext is RFC 9420 section 8.1's GroupContext, serialized by hand:
//
//	struct {
//	    ProtocolVersion version = mls10;
//	    CipherSuite cipher_suite;
//	    opaque group_id<V>;
//	    uint64 epoch;
//	    opaque tree_hash<V>;
//	    opaque confirmed_transcript_hash<V>;
//	    Extension extensions<V>;
//	} GroupContext;
//
// The version is written as the literal 1 rather than read from ProtocolVersionMls10, and
// the extension vector is written as its own empty length prefix rather than routed
// through WriteExtensions, because this is meant to be a second opinion about the encoding
// and a second opinion that reads its constants off the code under test is not one.
// TestProtocolVersionMls10IsTheCodePointRfc9420Registers is where the literal is held to
// the package's own constant.
//
// The two opaque fields and the group id take the MLS varint prefix. The record layer's
// fixed 32 bit prefix encodes the same 32 byte tree hash and is never interchangeable with
// it: only one of the two is what a peer will hash.
func independentGroupContext(t *testing.T, suite uint16, groupId []byte, epoch uint64, treeHash []byte, confirmedTranscriptHash []byte) []byte {
	t.Helper()
	// ProtocolVersion mls10 is 1, RFC 9420 section 17.1.
	encoded := binary.BigEndian.AppendUint16(nil, 1)
	encoded = binary.BigEndian.AppendUint16(encoded, suite)
	encoded = append(encoded, independentOpaqueV(t, groupId)...)
	encoded = binary.BigEndian.AppendUint64(encoded, epoch)
	encoded = append(encoded, independentOpaqueV(t, treeHash)...)
	encoded = append(encoded, independentOpaqueV(t, confirmedTranscriptHash)...)
	// extensions<V>, empty: the vector's own length prefix and no elements.
	return append(encoded, independentOpaqueV(t, nil)...)
}

// independentKeyScheduleSecrets is RFC 9420 section 8's epoch derivation, written from the
// RFC text with crypto/hmac and reaching nothing this package declares:
//
//	init_secret_[n-1] + commit_secret -> KDF.Extract -> ExpandWithLabel(., "joiner",
//	                                                       GroupContext_[n], KDF.Nh)
//	                                                 = joiner_secret
//	joiner_secret + psk_secret        -> KDF.Extract = member_secret
//	member_secret                     -> DeriveSecret(., "welcome") = welcome_secret
//	member_secret                     -> ExpandWithLabel(., "epoch", GroupContext_[n],
//	                                                       KDF.Nh) = epoch_secret
//	epoch_secret                      -> DeriveSecret(., <label>) for each of the nine
//
// The answers come back keyed by the json field key-schedule.json publishes each under, so
// a caller comparing them against a vector is comparing like with like and a name this
// derivation does not answer for is a missing key rather than a silent zero value.
//
// Both Extract calls take the previous secret as the SALT and the new material as the IKM,
// which is the one place a second opinion is worth having: guardrail 1 is that
// crypto/hkdf.Extract takes them the other way round, so a transposition compiles, returns
// 32 octets and satisfies everything either side could assert about its own output.
//
// sha256 is written in rather than read off a provider, for the reason
// independentPskSecret gives: both registered suites are HKDF-SHA256 at KDF.Nh 32, and
// reading the width off the code under test is how a second opinion stops being one.
// TestBothRegisteredSuitesAreSha256AtThisWidth is what fails on the day that stops
// being true.
func independentKeyScheduleSecrets(t *testing.T, initSecretPrev []byte, commitSecret []byte, pskSecret []byte, groupContext []byte) map[string][]byte {
	t.Helper()
	answers := map[string][]byte{}
	// joiner_secret = ExpandWithLabel(Extract(init_secret_[n-1], commit_secret), "joiner",
	// GroupContext_[n], KDF.Nh)
	joiner := independentExpandWithLabel(
		t, independentExtract(initSecretPrev, commitSecret), "joiner", groupContext, sha256.Size)
	answers["joiner_secret"] = joiner
	// member_secret = Extract(joiner_secret, psk_secret)
	member := independentExtract(joiner, pskSecret)
	answers["welcome_secret"] = independentDeriveSecret(t, member, "welcome")
	epochSecret := independentExpandWithLabel(t, member, "epoch", groupContext, sha256.Size)
	for field, label := range keyScheduleRfcLabels {
		answers[field] = independentDeriveSecret(t, epochSecret, label)
	}
	return answers
}

// independentKeyScheduleSecretsTransposed is the same derivation with both Extract calls
// the wrong way round. It exists only to be disagreed with: a hand written derivation
// giving the same answer either way could not see the defect guardrail 1 names, and would
// agree with a transposed implementation while looking like a second opinion.
func independentKeyScheduleSecretsTransposed(t *testing.T, initSecretPrev []byte, commitSecret []byte, pskSecret []byte, groupContext []byte) map[string][]byte {
	t.Helper()
	answers := map[string][]byte{}
	joiner := independentExpandWithLabel(
		t, independentExtract(commitSecret, initSecretPrev), "joiner", groupContext, sha256.Size)
	answers["joiner_secret"] = joiner
	member := independentExtract(pskSecret, joiner)
	answers["welcome_secret"] = independentDeriveSecret(t, member, "welcome")
	epochSecret := independentExpandWithLabel(t, member, "epoch", groupContext, sha256.Size)
	for field, label := range keyScheduleRfcLabels {
		answers[field] = independentDeriveSecret(t, epochSecret, label)
	}
	return answers
}

// independentExporter is RFC 9420 section 8.5's MLS-Exporter:
//
//	MLS-Exporter(Label, Context, Length) =
//	    ExpandWithLabel(DeriveSecret(exporter_secret, Label), "exported",
//	                    Hash(Context), Length)
//
// The caller's context is HASHED and not passed through, which is what lets it be any
// length; a derivation that passed it through would agree for a 32 octet context and
// disagree for every other one, and the corpus publishes only 32 octet contexts. And the
// per label secret is DeriveSecret over exporter_secret, not exporter_secret itself: an
// implementation that skipped that hop hands every label one secret.
func independentExporter(t *testing.T, exporterSecret []byte, label string, context []byte, length int) []byte {
	t.Helper()
	hashed := sha256.Sum256(context)
	return independentExpandWithLabel(
		t, independentDeriveSecret(t, exporterSecret, label), "exported", hashed[:], length)
}

// generatedKeyScheduleOctets is deterministic filler for the generate direction, over the
// same digest generatedPskOctets uses.
//
// Deterministic rather than crypto.Random, for the reason task 16 gives: a generated case
// that fails is then the same case on the next run. It also keeps the generator's own call
// closure clear of the provider, which is what lets the disjointness gate say something
// about it.
func generatedKeyScheduleOctets(field string, index int) []byte {
	return generatedPskOctets("key schedule "+field, index)
}

// generateKeyScheduleCases builds fresh key-schedule entries whose every kdf answer is
// computed by the hand written derivation above.
//
// This is the shape that makes the generate direction worth running, and it is not the
// shape the obvious version has. A generator that computed its answers with NewKeySchedule
// and a verifier that checked them with NewKeySchedule round trip perfectly and say
// nothing about conformance at all -- they prove this code agrees with itself, which it
// would whatever it computed. Every published answer below except external_pub comes from
// independentKeyScheduleSecrets, independentExporter or independentGroupContext, so
// feeding these entries back through verifyKeyScheduleVector compares two implementations.
//
// external_pub is the exception and it is named as one: it is DeriveKeyPair over
// external_secret, which is HPKE rather than kdf, and there is no second X25519 in this
// tree to derive it with. It is taken from the schedule this generator builds alongside,
// and the vendored corpus is what checks it -- see the family runner, which compares it at
// every one of its ten epochs.
//
// wrongLabel, when set, derives membership_key under it instead of under "membership".
// That is how the generate to verify loop is shown to be able to FAIL: every entry this
// generator emits agrees with the implementation, so a loop that checked nothing and a
// loop that checked everything produce identical runs over them.
func generateKeyScheduleCases(t *testing.T, wrongLabel string) []keyScheduleVector {
	t.Helper()
	generated := []keyScheduleVector{}
	for _, suite := range keyScheduleGeneratedSuites {
		provider, err := NewCryptoProvider(CipherSuite(suite))
		if err != nil {
			t.Fatalf("NewCryptoProvider(%#04x): %v", suite, err)
		}
		tag := fmt.Sprintf("suite %d", suite)
		groupId := generatedKeyScheduleOctets(tag+" group id", 0)[:16]
		initSecret := generatedKeyScheduleOctets(tag+" initial init secret", 0)
		vector := keyScheduleVector{
			CipherSuite:       suite,
			GroupId:           HexOf(groupId),
			InitialInitSecret: HexOf(initSecret),
		}
		for n := 0; n < keyScheduleGeneratedEpochs; n++ {
			treeHash := generatedKeyScheduleOctets(tag+" tree hash", n)
			commitSecret := generatedKeyScheduleOctets(tag+" commit secret", n)
			pskSecret := generatedKeyScheduleOctets(tag+" psk secret", n)
			confirmedTranscriptHash := generatedKeyScheduleOctets(tag+" confirmed transcript hash", n)
			exporterContext := generatedKeyScheduleOctets(tag+" exporter context", n)
			// a label that is not hex, unlike every label the vendored corpus publishes,
			// so an implementation that hex decoded it would refuse this vector outright.
			exporterLabel := fmt.Sprintf("urmessage generated exporter %d", n)

			groupContext := independentGroupContext(
				t, suite, groupId, uint64(n), treeHash, confirmedTranscriptHash)
			answers := independentKeyScheduleSecrets(t, initSecret, commitSecret, pskSecret, groupContext)
			if wrongLabel != "" {
				epochSecret := independentExpandWithLabel(
					t, independentExtract(answers["joiner_secret"], pskSecret), "epoch", groupContext, sha256.Size)
				answers["membership_key"] = independentDeriveSecret(t, epochSecret, wrongLabel)
			}
			exported := independentExporter(
				t, answers["exporter_secret"], exporterLabel, exporterContext, sha256.Size)

			// the one production call: external_pub is HPKE and has no second opinion
			// here. Building the schedule also means a generated entry whose group
			// context this file encoded differently from the codec fails loudly rather
			// than quietly, because the secrets below would then be derived over
			// different bytes.
			schedule, err := NewKeySchedule(provider, initSecret, commitSecret, pskSecret, &GroupContext{
				Version:                 ProtocolVersionMls10,
				CipherSuite:             CipherSuite(suite),
				GroupId:                 groupId,
				Epoch:                   uint64(n),
				TreeHash:                treeHash,
				ConfirmedTranscriptHash: confirmedTranscriptHash,
				Extensions:              nil,
			})
			if err != nil {
				t.Fatalf("suite %#04x epoch %d: NewKeySchedule: %v", suite, n, err)
			}
			_, externalPub, err := schedule.ExternalKeyPair()
			if err != nil {
				t.Fatalf("suite %#04x epoch %d: ExternalKeyPair: %v", suite, n, err)
			}

			vector.Epochs = append(vector.Epochs, keyScheduleEpoch{
				TreeHash:                HexOf(treeHash),
				CommitSecret:            HexOf(commitSecret),
				PskSecret:               HexOf(pskSecret),
				ConfirmedTranscriptHash: HexOf(confirmedTranscriptHash),
				GroupContext:            HexOf(groupContext),
				JoinerSecret:            HexOf(answers["joiner_secret"]),
				WelcomeSecret:           HexOf(answers["welcome_secret"]),
				InitSecret:              HexOf(answers["init_secret"]),
				SenderDataSecret:        HexOf(answers["sender_data_secret"]),
				EncryptionSecret:        HexOf(answers["encryption_secret"]),
				ExporterSecret:          HexOf(answers["exporter_secret"]),
				EpochAuthenticator:      HexOf(answers["epoch_authenticator"]),
				ExternalSecret:          HexOf(answers["external_secret"]),
				ConfirmationKey:         HexOf(answers["confirmation_key"]),
				MembershipKey:           HexOf(answers["membership_key"]),
				ResumptionPsk:           HexOf(answers["resumption_psk"]),
				ExternalPub:             HexOf(externalPub),
				Exporter: labelKatExporter{
					Label:   exporterLabel,
					Context: HexOf(exporterContext),
					Length:  sha256.Size,
					Secret:  HexOf(exported),
				},
			})
			// the chain advances on the derivation's own init_secret, which is what the
			// consume direction does too.
			initSecret = bytes.Clone(answers["init_secret"])
		}
		generated = append(generated, vector)
	}
	return generated
}

// generateKeyScheduleVector is the Generate half of family 5: fresh entries in the mlswg
// format, answered by the hand written derivation, for the registry to feed back through
// verifyKeyScheduleVector.
func generateKeyScheduleVector(t *testing.T) json.RawMessage {
	t.Helper()
	body, err := json.Marshal(generateKeyScheduleCases(t, ""))
	if err != nil {
		t.Fatalf("marshal the generated key schedule cases: %v", err)
	}
	return body
}

// TestVectorKeyScheduleGenerate is the generate direction of family 5.
//
// What it closes that verification alone cannot: a pinned vector never passes through our
// own encoder, so an encoder and a decoder that are wrong in the same direction verify
// perfectly. Generating a case and feeding it back sees that -- but only if the generator
// is not the verifier under another name, and that is the trap this task's name states.
// The answers below are computed by the hand written derivation and the KDFLabel they are
// expanded under is the hand written encoder, so consuming them with NewKeySchedule
// compares two implementations rather than one implementation with itself.
//
// Four things stand against the loop passing vacuously. The generated entries are
// re-derived here and the comparison count is asserted. They are then consumed by the real
// comparator, whose full verdict must accept them. A case with ONE secret derived under
// the wrong label must be REFUSED, so a consume direction that checked nothing fails here.
// And the entries are required to carry every field the family compares, because a
// generator that emitted a shorter entry would have the consume direction comparing an
// empty published answer against an empty computed one and agreeing.
func TestVectorKeyScheduleGenerate(t *testing.T) {
	serialized := generateKeyScheduleVector(t)
	readBack := []keyScheduleVector{}
	if err := json.Unmarshal(serialized, &readBack); err != nil {
		t.Fatalf("the generated cases do not parse: %v", err)
	}
	if len(readBack) != len(keyScheduleGeneratedSuites) {
		t.Fatalf("generated %d suites, want %d", len(readBack), len(keyScheduleGeneratedSuites))
	}

	compared, epochs := 0, 0
	answers := map[string]int{}
	for _, vector := range readBack {
		suite, ok := implementedSuite(vector.CipherSuite)
		if !ok {
			t.Fatalf("generated a vector at unimplemented suite %#04x", vector.CipherSuite)
		}
		if len(vector.Epochs) != keyScheduleGeneratedEpochs {
			t.Fatalf("suite %#04x: the round trip carries %d epochs, want %d",
				uint16(suite), len(vector.Epochs), keyScheduleGeneratedEpochs)
		}
		groupId := MustHex(t, vector.GroupId)
		// the chain is carried on the derivation's own init_secret, so a divergence
		// surfaces at the epoch that caused it rather than at the next reseed.
		initSecret := MustHex(t, vector.InitialInitSecret)
		for n, epoch := range vector.Epochs {
			epochs++
			groupContext := independentGroupContext(
				t, vector.CipherSuite, groupId, uint64(n),
				MustHex(t, epoch.TreeHash), MustHex(t, epoch.ConfirmedTranscriptHash))
			if got := HexOf(groupContext); got != epoch.GroupContext {
				t.Fatalf("suite %#04x epoch %d: the hand written group context is %s and the case carries %s",
					uint16(suite), n, got, epoch.GroupContext)
			}
			derived := independentKeyScheduleSecrets(
				t, initSecret, MustHex(t, epoch.CommitSecret), MustHex(t, epoch.PskSecret), groupContext)
			derived["exporter.secret"] = independentExporter(
				t, derived["exporter_secret"], epoch.Exporter.Label,
				MustHex(t, epoch.Exporter.Context), epoch.Exporter.Length)
			// addressed by the family's own check names, so a name the family compares
			// and this derivation does not answer for is a failure rather than a
			// comparison silently not made.
			published := map[string]json.RawMessage{}
			marshalled, err := json.Marshal(epoch)
			if err != nil {
				t.Fatalf("re-marshal a generated epoch: %v", err)
			}
			if err := json.Unmarshal(marshalled, &published); err != nil {
				t.Fatalf("re-read a generated epoch: %v", err)
			}
			for _, name := range keyScheduleCheckNames {
				if slices.Contains(keyScheduleNonSecretChecks, name) && name != "exporter.secret" {
					continue
				}
				want := publishedCorpusField(t, published, name)
				got, answered := derived[name]
				if !answered {
					t.Fatalf("the hand written derivation answers nothing for %s, so this family compares one answer nothing independent produced",
						name)
				}
				if HexOf(got) != want {
					t.Fatalf("suite %#04x epoch %d: the hand written derivation gives %s for %s, the generated case carries %s",
						uint16(suite), n, HexOf(got), name, want)
				}
				answers[want]++
				compared++
			}
			initSecret = bytes.Clone(derived["init_secret"])
		}
	}

	want := len(keyScheduleGeneratedSuites) * keyScheduleGeneratedEpochs
	if epochs != want {
		t.Fatalf("re-derived %d epochs, want %d", epochs, want)
	}
	if got := want * keyScheduleIndependentChecksPerEpoch; compared != got {
		t.Fatalf("re-derived %d answers over %d epochs, want %d", compared, epochs, got)
	}
	if len(answers) != compared {
		t.Fatalf("the %d re-derivations were made against %d distinct answers; a generator emitting one repeated value would compare that many times and pin one answer",
			compared, len(answers))
	}

	// and the consume direction, which is the half that makes any of this a statement
	// about conformance: NewKeySchedule must reproduce every one of these answers.
	consumed := 0
	for _, vector := range readBack {
		body, err := json.Marshal(vector)
		if err != nil {
			t.Fatalf("marshal a generated case: %v", err)
		}
		evidence, err := compareKeyScheduleVector(t, body)
		if err != nil {
			t.Fatalf("the consume direction refused a generated case: %v", err)
		}
		if err := evidence.verdict(); err != nil {
			t.Fatalf("the consume direction refused a generated case: %v", err)
		}
		if evidence.epochs != keyScheduleGeneratedEpochs {
			t.Fatalf("the consume direction advanced through %d epochs of a generated case", evidence.epochs)
		}
		consumed += len(evidence.checks)
	}
	if got := want * keyScheduleFamilyChecksPerEpoch; consumed != got {
		t.Fatalf("the consume direction made %d comparisons over the generated cases, want %d", consumed, got)
	}

	if out := os.Getenv("URMESSAGE_MLS_VECTOR_OUT"); out != "" {
		path := filepath.Join(out, "key-schedule-generated.json")
		if err := os.WriteFile(path, serialized, 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		t.Logf("wrote %s for the OpenMLS cross-check job", path)
	}
	t.Logf("key-schedule generate: %d epochs re-derived by hand over %d comparisons, %d consumed by the implementation",
		epochs, compared, consumed)
}

// TestTheConsumeDirectionRefusesAGeneratedVectorDerivedUnderTheWrongLabel is the control
// the test above needs.
//
// Every case the generator emits agrees with the implementation, so a consume direction
// that compared nothing and one that compared everything produce identical runs over them
// -- which is exactly the failure task 16 shipped. One secret derived under a label RFC
// 9420 does not name is a case the loop MUST refuse, and it must refuse it as a mismatch:
// the answer is a perfectly well formed KDF.Nh secret and nothing about the value itself
// says which label produced it.
//
// Two labels, for two different refusals. A label nothing else uses is a mismatch. The
// label of the secret NEXT to it in the schedule is the copy-paste defect the RFC's nine
// near-identical DeriveSecret lines invite, and it publishes one value under two names,
// which the aliasing control refuses first.
func TestTheConsumeDirectionRefusesAGeneratedVectorDerivedUnderTheWrongLabel(t *testing.T) {
	for _, wrong := range []struct {
		label string
		want  error
	}{
		{"membershipp", errKeyScheduleMismatch},
		{"membership ", errKeyScheduleMismatch},
		{"confirm", errKeyScheduleAliased},
	} {
		cases := generateKeyScheduleCases(t, wrong.label)
		if len(cases) == 0 {
			t.Fatalf("the generator produced no case under %q", wrong.label)
		}
		refused := 0
		for _, vector := range cases {
			body, err := json.Marshal(vector)
			if err != nil {
				t.Fatalf("marshal the case under test: %v", err)
			}
			_, err = compareKeyScheduleVector(t, body)
			if err == nil {
				t.Errorf("a case whose membership_key was derived under %q was accepted; the consume direction is not comparing",
					wrong.label)
				continue
			}
			if !errors.Is(err, wrong.want) {
				t.Errorf("a case whose membership_key was derived under %q was refused as %v, want %v",
					wrong.label, err, wrong.want)
				continue
			}
			refused++
		}
		if refused != len(cases) {
			t.Errorf("%d of %d generated cases under %q were refused", refused, len(cases), wrong.label)
		}
	}
}

// TestTheIndependentKeyScheduleMatchesEveryUpstreamVector pins the hand written derivation
// to the published corpus.
//
// This is what makes the generate direction worth running at all. A generator agreeing
// with the verifier proves two spellings of one algorithm agree; a generator that
// reproduces every answer mlswg published, computed with crypto/hmac from the RFC text, is
// a second implementation, and the round trip through it is then a statement about
// conformance.
//
// The comparison count is asserted for the reason the runner's is. So is the count of
// distinct answers: 120 of the corpus's 140 published values are reachable from the kdf
// alone, and a derivation compared against one repeated answer would satisfy the total.
func TestTheIndependentKeyScheduleMatchesEveryUpstreamVector(t *testing.T) {
	compared, epochs := 0, 0
	answers := map[string]int{}
	for index, raw := range LoadVectorFile(t, keyScheduleKatFile) {
		vector := keyScheduleVector{}
		if err := json.Unmarshal(raw, &vector); err != nil {
			t.Fatalf("vector %d: %v", index, err)
		}
		if _, ok := implementedSuite(vector.CipherSuite); !ok {
			continue
		}
		groupId := MustHex(t, vector.GroupId)
		initSecret := MustHex(t, vector.InitialInitSecret)
		for n, epoch := range vector.Epochs {
			epochs++
			groupContext := independentGroupContext(
				t, vector.CipherSuite, groupId, uint64(n),
				MustHex(t, epoch.TreeHash), MustHex(t, epoch.ConfirmedTranscriptHash))
			if got := HexOf(groupContext); got != epoch.GroupContext {
				t.Fatalf("vector %d epoch %d: the hand written group context is %s, the corpus publishes %s",
					index, n, got, epoch.GroupContext)
			}
			answers[epoch.GroupContext]++
			compared++

			derived := independentKeyScheduleSecrets(
				t, initSecret, MustHex(t, epoch.CommitSecret), MustHex(t, epoch.PskSecret), groupContext)
			derived["exporter.secret"] = independentExporter(
				t, derived["exporter_secret"], epoch.Exporter.Label,
				MustHex(t, epoch.Exporter.Context), epoch.Exporter.Length)
			published := map[string]json.RawMessage{}
			body, err := json.Marshal(epoch)
			if err != nil {
				t.Fatalf("re-marshal a published epoch: %v", err)
			}
			if err := json.Unmarshal(body, &published); err != nil {
				t.Fatalf("re-read a published epoch: %v", err)
			}
			for _, name := range keyScheduleCheckNames {
				got, answered := derived[name]
				if !answered {
					continue
				}
				want := publishedCorpusField(t, published, name)
				if HexOf(got) != want {
					t.Fatalf("vector %d epoch %d: the hand written derivation gives %s for %s, the corpus publishes %s",
						index, n, HexOf(got), name, want)
				}
				answers[want]++
				compared++
			}
			initSecret = bytes.Clone(derived["init_secret"])
		}
	}
	if epochs != keyScheduleFamilyEpochs {
		t.Fatalf("the hand written derivation ran over %d epochs, want %d", epochs, keyScheduleFamilyEpochs)
	}
	// group_context plus the twelve kdf answers of each epoch; external_pub is HPKE and
	// has no hand written twin.
	if want := epochs * (keyScheduleIndependentChecksPerEpoch + 1); compared != want {
		t.Fatalf("the hand written derivation was compared against %d published answers, want %d", compared, want)
	}
	if len(answers) != compared {
		t.Fatalf("the %d comparisons were made against %d distinct published answers", compared, len(answers))
	}
	t.Logf("the hand written key schedule reproduced %d published answers over %d epochs", compared, epochs)
}

// TestTheIndependentKeyScheduleSeesATransposedExtract is the control the test above needs.
// A derivation that answers the same with its Extract arguments the wrong way round agrees
// with a transposed implementation and pins nothing about guardrail 1.
//
// Every one of the twelve answers must move, not merely one of them: joiner_secret is the
// output of the first transposed Extract and everything below it is expanded from the
// second, so a control that only looked at joiner_secret would be satisfied by a
// derivation that transposed one of the two.
func TestTheIndependentKeyScheduleSeesATransposedExtract(t *testing.T) {
	checked := 0
	for index, raw := range LoadVectorFile(t, keyScheduleKatFile) {
		vector := keyScheduleVector{}
		if err := json.Unmarshal(raw, &vector); err != nil {
			t.Fatalf("vector %d: %v", index, err)
		}
		if _, ok := implementedSuite(vector.CipherSuite); !ok {
			continue
		}
		groupId := MustHex(t, vector.GroupId)
		initSecret := MustHex(t, vector.InitialInitSecret)
		for n, epoch := range vector.Epochs {
			groupContext := independentGroupContext(
				t, vector.CipherSuite, groupId, uint64(n),
				MustHex(t, epoch.TreeHash), MustHex(t, epoch.ConfirmedTranscriptHash))
			commitSecret := MustHex(t, epoch.CommitSecret)
			pskSecret := MustHex(t, epoch.PskSecret)
			straight := independentKeyScheduleSecrets(t, initSecret, commitSecret, pskSecret, groupContext)
			transposed := independentKeyScheduleSecretsTransposed(t, initSecret, commitSecret, pskSecret, groupContext)
			if len(straight) != len(transposed) || len(straight) == 0 {
				t.Fatalf("the two derivations answer %d and %d fields", len(straight), len(transposed))
			}
			for field, answer := range straight {
				if bytes.Equal(answer, transposed[field]) {
					t.Fatalf("vector %d epoch %d: %s is the same for both Extract orders, so this derivation cannot see a transposition",
						index, n, field)
				}
				checked++
			}
			initSecret = bytes.Clone(straight["init_secret"])
		}
	}
	if want := keyScheduleFamilyEpochs * (keyScheduleIndependentChecksPerEpoch - 1); checked != want {
		t.Fatalf("the transposition control ran over %d answers, want %d", checked, want)
	}
}

// TestTheIndependentKeyScheduleCoversEverySecretTheEpochHolds holds the hand written label
// table to what an epoch actually derives.
//
// Derived from EpochSecrets by reflection rather than counted here. A tenth secret added to
// the schedule with no label transcribed for it would leave this derivation answering nine
// of ten while every count above still balanced, which is the shape of failure a written
// list produces.
func TestTheIndependentKeyScheduleCoversEverySecretTheEpochHolds(t *testing.T) {
	epochSecrets := reflect.TypeOf(EpochSecrets{})
	if len(keyScheduleRfcLabels) != epochSecrets.NumField() {
		t.Fatalf("the hand written derivation transcribes %d labels and %s holds %d secrets",
			len(keyScheduleRfcLabels), epochSecrets.Name(), epochSecrets.NumField())
	}
	// the nine labels must be nine distinct strings, or two secrets share a preimage.
	labels := slices.Compact(slices.Sorted(maps.Values(keyScheduleRfcLabels)))
	if len(labels) != len(keyScheduleRfcLabels) {
		t.Fatalf("the %d transcribed labels hold %d distinct strings: %v",
			len(keyScheduleRfcLabels), len(labels), labels)
	}
	// and every field it answers for must be one this family compares, or it is an answer
	// nothing is held against.
	answered := slices.Sorted(maps.Keys(keyScheduleRfcLabels))
	for _, field := range answered {
		if !slices.Contains(keyScheduleCheckNames, field) {
			t.Errorf("the derivation answers %s and this family compares nothing under that name", field)
		}
	}
	// the twelve the derivation owes: the nine, plus joiner and welcome, plus the exporter
	// answer the caller asks for separately.
	if want := keyScheduleIndependentChecksPerEpoch; len(keyScheduleRfcLabels)+3 != want {
		t.Fatalf("the derivation answers %d of the %d checks this family makes independently",
			len(keyScheduleRfcLabels)+3, want)
	}
}

// TestGeneratedKeyScheduleVectorsCoverEveryRegisteredSuite holds the generator's own list
// of code points to the registry, which is the check the generator cannot make about
// itself without calling into the package it is meant to stay clear of.
func TestGeneratedKeyScheduleVectorsCoverEveryRegisteredSuite(t *testing.T) {
	entries := []keyScheduleVector{}
	if err := json.Unmarshal(generateKeyScheduleVector(t), &entries); err != nil {
		t.Fatalf("the generated cases do not parse: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the generator produced no cases")
	}
	covered := map[CipherSuite]int{}
	for _, entry := range entries {
		covered[CipherSuite(entry.CipherSuite)]++
		if len(entry.Epochs) != keyScheduleGeneratedEpochs {
			t.Errorf("a generated vector carries %d epochs, want %d", len(entry.Epochs), keyScheduleGeneratedEpochs)
		}
		// every field the family compares must be carried, or the consume direction is
		// comparing an empty published answer against an empty computed one.
		for n, epoch := range entry.Epochs {
			published := map[string]json.RawMessage{}
			body, err := json.Marshal(epoch)
			if err != nil {
				t.Fatalf("re-marshal a generated epoch: %v", err)
			}
			if err := json.Unmarshal(body, &published); err != nil {
				t.Fatalf("re-read a generated epoch: %v", err)
			}
			for _, name := range keyScheduleCheckNames {
				if publishedCorpusField(t, published, name) == "" {
					t.Errorf("suite %#04x epoch %d carries no %s, so the consume direction compares nothing there",
						entry.CipherSuite, n, name)
				}
			}
		}
	}
	if got := slices.Sorted(maps.Keys(covered)); !slices.Equal(got, Suites()) {
		t.Fatalf("the generator covers %v and the registry holds %v; widen keyScheduleGeneratedSuites", got, Suites())
	}
	t.Logf("%d generated key schedules over suites %v, %d epochs each",
		len(entries), slices.Sorted(maps.Keys(covered)), keyScheduleGeneratedEpochs)
}

// TestProtocolVersionMls10IsTheCodePointRfc9420Registers is the assumption
// independentGroupContext makes when it writes the version in as a literal rather than
// reading the package's own constant.
func TestProtocolVersionMls10IsTheCodePointRfc9420Registers(t *testing.T) {
	if uint16(ProtocolVersionMls10) != 1 {
		t.Fatalf("ProtocolVersionMls10 is %#04x and RFC 9420 section 17.1 registers mls10 as 1; the hand written group context encoder writes the literal",
			uint16(ProtocolVersionMls10))
	}
}

// TestTheKdfLabelThisPackageWritesIsTheOneRfc9420Describes holds the KDFLabel bytes this
// package expands under to the bytes the hand written encoder produces.
//
// The package never hands its KDFLabel out, so the comparison is made through the only
// door it leaves open: Expand over the hand written label must equal ExpandWithLabel over
// the same label, context and length. HKDF-Expand is deterministic in its info string, so
// two equal outputs over one PRK and one length is the statement that the two info strings
// are equal -- which is the KDFLabel byte comparison, at one remove.
//
// The controls come first and they are what stop this holding vacuously. Four deliberately
// wrong encodings -- the length field holding the label's size, the "MLS 1.0 " prefix
// dropped, the two opaque fields transposed, and a fixed 16 bit length written where the
// MLS varint is owed -- must each DISAGREE, or the comparison is not sensitive to the
// thing it claims to check.
func TestTheKdfLabelThisPackageWritesIsTheOneRfc9420Describes(t *testing.T) {
	secret := generatedKeyScheduleOctets("kdf label secret", 0)
	context := generatedKeyScheduleOctets("kdf label context", 0)
	// every label the key schedule expands under, plus the psk one, so this is the class
	// the two corpora between them pin rather than one example.
	labels := []string{"joiner", "epoch", "welcome", "exported", "derived psk"}
	labels = append(labels, slices.Sorted(maps.Values(keyScheduleRfcLabels))...)

	for _, suite := range Suites() {
		provider, err := NewCryptoProvider(suite)
		if err != nil {
			t.Fatalf("NewCryptoProvider(%#04x): %v", uint16(suite), err)
		}
		nh := provider.HashSize()
		for _, label := range labels {
			for _, ctx := range [][]byte{nil, context, bytes.Repeat(context, 4)} {
				kdfLabel := independentKdfLabel(t, label, ctx, nh)
				if len(kdfLabel) == 0 {
					t.Fatalf("the hand written KDFLabel for %q is empty, so every comparison below is over nothing", label)
				}
				want := provider.ExpandWithLabel(secret, label, ctx, nh)
				if got := provider.Expand(secret, kdfLabel, nh); !bytes.Equal(got, want) {
					t.Fatalf("suite %#04x label %q over a %d octet context: this package expands under a KDFLabel that is not %x",
						uint16(suite), label, len(ctx), kdfLabel)
				}
			}
		}

		// and the controls, over one label and one context, so the agreement above is a
		// statement about these bytes and not about Expand ignoring its info.
		full := []byte("MLS 1.0 " + "joiner")
		for _, wrong := range []struct {
			name  string
			label []byte
		}{
			{"the length field holding the label's size", append(
				binary.BigEndian.AppendUint16(nil, uint16(len(full))),
				append(independentOpaqueV(t, full), independentOpaqueV(t, context)...)...)},
			{"the MLS 1.0 prefix dropped", append(
				binary.BigEndian.AppendUint16(nil, uint16(nh)),
				append(independentOpaqueV(t, []byte("joiner")), independentOpaqueV(t, context)...)...)},
			{"the label and context transposed", append(
				binary.BigEndian.AppendUint16(nil, uint16(nh)),
				append(independentOpaqueV(t, context), independentOpaqueV(t, full)...)...)},
			{"a fixed 16 bit length where the MLS varint is owed", append(
				binary.BigEndian.AppendUint16(nil, uint16(nh)),
				append(binary.BigEndian.AppendUint16(nil, uint16(len(full))),
					append(full, independentOpaqueV(t, context)...)...)...)},
		} {
			want := provider.ExpandWithLabel(secret, "joiner", context, nh)
			if bytes.Equal(provider.Expand(secret, wrong.label, nh), want) {
				t.Fatalf("suite %#04x: %s expands to the same answer, so the comparison above sees nothing",
					uint16(suite), wrong.name)
			}
		}
	}
}
