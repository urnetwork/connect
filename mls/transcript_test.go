// Tests for the RFC 9420 section 8.2 transcript hashes.
//
// A defect here does not produce a wrong answer, it produces two groups: members who agree
// with each other and disagree with everyone else, from the first divergent commit onward.
// So the three things a single-epoch comparison cannot see are what most of this file is
// about.
//
// The CHAINING. An implementation that computes each epoch's hash from the right inputs and
// forgets to fold in the previous epoch's value passes every one-epoch test and forks at
// epoch two. TestTranscriptHashChainCarriesEveryEarlierEpoch runs a sequence whose length is
// the published corpus's rather than one chosen here, and sweeps every (epoch, input) pair
// the type declares -- the count read off Update's own arity -- asserting exactly which
// later epochs a change reaches and which earlier ones it must not.
//
// The BYTE ENCODING. syntax.WriteOpaque is MLS's varint prefix and syntax.WriteOpaqueLP is
// the record layer's fixed 32 bit one. Both exist in this tree, both compile here, and both
// produce a 32 byte hash that a group would agree with itself on.
// TestInterimTranscriptHashLengthPrefixesTheTag separates them behaviourally, and
// TestNoMlsEncodingReachesTheRecordLayerLengthPrefix derives the whole class of LP entry
// points off the codec's own types rather than naming WriteOpaqueLP.
//
// The two ROLES. Confirmed and Interim are two values with two jobs and swapping them is a
// one line edit that leaves both looking like hashes.
// TestTheTwoTranscriptHashesAreNotSubstitutable holds them apart at each of the three places
// a substitution could be made.
//
// The known answers are mlswg's transcript-hashes corpus, read through the loader p4 task 10
// already built -- which authenticates the file against upstream's git object store before a
// byte of it is read, because a known answer test compared against a file an edit can change
// is a known answer test that can be made to agree with anything.
package mls

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect/mls/syntax"
)

// trTestCrypto returns the ciphersuite 0x0003 provider.
func trTestCrypto(t *testing.T) CryptoProvider {
	t.Helper()
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	return crypto
}

// TestInitialTranscriptHashesAreTheEmptyOctetStringRatherThanNil asserts the
// group-creation base case: both hashes are the zero-length octet string, not a hash of
// nothing, and the zero-length octet string rather than nil.
//
// The length half is the RFC's. The nil half is this file's own doc comment, which says the
// two start as "the zero-length octet string, and NOT the hash of nothing", and Clone's,
// which reaches for cloneBytes rather than append precisely so that distinction survives a
// copy. Nothing hashes or encodes the two differently today, so a constructor that answered
// nil passes every other test in this package; what it changes is which of the two values
// every caller holds, and a base case that is one thing in the constructor and another after
// a Clone is a base case nothing can be compared against.
func TestInitialTranscriptHashesAreTheEmptyOctetStringRatherThanNil(t *testing.T) {
	hashes := InitialTranscriptHashes()
	if len(hashes.Confirmed) != 0 {
		t.Fatalf("Confirmed = %x, want empty", hashes.Confirmed)
	}
	if len(hashes.Interim) != 0 {
		t.Fatalf("Interim = %x, want empty", hashes.Interim)
	}
	if hashes.Confirmed == nil {
		t.Error("the base case answers nil for Confirmed, and it is documented as the zero-length octet string")
	}
	if hashes.Interim == nil {
		t.Error("the base case answers nil for Interim, and it is documented as the zero-length octet string")
	}
}

// TestConfirmedTranscriptHashShape asserts the confirmed hash is
// Hash(interim_before || ConfirmedTranscriptHashInput) with no length prefix between
// the two, which is what makes the chain a transcript rather than a set.
func TestConfirmedTranscriptHashShape(t *testing.T) {
	crypto := trTestCrypto(t)
	interimBefore := crypto.Hash([]byte("previous epoch"))
	input := []byte("serialized ConfirmedTranscriptHashInput")
	got := ConfirmedTranscriptHash(crypto, interimBefore, input)
	want := crypto.Hash(append(append([]byte(nil), interimBefore...), input...))
	if !bytes.Equal(got, want) {
		t.Fatalf("ConfirmedTranscriptHash = %x, want %x", got, want)
	}
	// and the two operands are not commutative, which a self referential equality above
	// cannot see on its own: the reversed concatenation is a hash a group would also agree
	// with itself on
	reversed := crypto.Hash(append(append([]byte(nil), input...), interimBefore...))
	if bytes.Equal(got, reversed) {
		t.Fatal("the confirmed hash is taken over the two operands in either order")
	}
	if len(got) != crypto.HashSize() {
		t.Fatalf("ConfirmedTranscriptHash answered %d bytes and this suite's KDF.Nh is %d",
			len(got), crypto.HashSize())
	}
}

// TestInterimTranscriptHashLengthPrefixesTheTag asserts the confirmation tag enters
// the hash as InterimTranscriptHashInput { MAC confirmation_tag; }, which is an
// opaque<V>. Concatenating the raw tag instead would agree with itself on both sides
// and diverge from every other implementation.
//
// The record layer's fixed 32 bit prefix is refused explicitly as well as the absent one.
// WriteOpaqueLP is one identifier away from WriteOpaque, it is in scope in this package,
// and the hash it produces is as well formed as the right one.
func TestInterimTranscriptHashLengthPrefixesTheTag(t *testing.T) {
	crypto := trTestCrypto(t)
	confirmedAfter := crypto.Hash([]byte("confirmed"))
	tag := crypto.Mac(make([]byte, 32), confirmedAfter)

	got, err := InterimTranscriptHash(crypto, confirmedAfter, tag)
	if err != nil {
		t.Fatalf("InterimTranscriptHash: %v", err)
	}
	withPrefix := append([]byte(nil), confirmedAfter...)
	withPrefix = append(withPrefix, byte(len(tag)))
	withPrefix = append(withPrefix, tag...)
	if !bytes.Equal(got, crypto.Hash(withPrefix)) {
		t.Fatalf("InterimTranscriptHash = %x, want the length-prefixed form", got)
	}
	raw := append(append([]byte(nil), confirmedAfter...), tag...)
	if bytes.Equal(got, crypto.Hash(raw)) {
		t.Fatal("the confirmation tag is being concatenated without its length prefix")
	}
	// the record layer's prefix, built by the codec itself rather than by four bytes
	// written out here, so the control is the encoding this package must not use and not
	// somebody's idea of it
	recordLayer := syntax.NewWriter()
	recordLayer.WriteOpaqueLP(tag)
	lengthPrefixed, err := recordLayer.Bytes()
	if err != nil {
		t.Fatalf("build the record layer control: %v", err)
	}
	if bytes.Equal(got, crypto.Hash(append(append([]byte(nil), confirmedAfter...), lengthPrefixed...))) {
		t.Fatal("the confirmation tag is entering the interim hash under the record layer's 32 bit length prefix, not MLS's varint")
	}
	// and the operands are in the order RFC 9420 writes them
	if bytes.Equal(got, crypto.Hash(append(append([]byte(nil), lengthPrefixed...), confirmedAfter...))) ||
		bytes.Equal(got, crypto.Hash(append(append([]byte(nil), byte(len(tag))), append(append([]byte(nil), tag...), confirmedAfter...)...))) {
		t.Fatal("the interim hash is taken over the tag before the confirmed hash")
	}
}

// TestTranscriptHashesUpdateChains asserts two commits produce a chain in which the
// second confirmed hash depends on the first interim hash, which is the property that
// makes a fork detectable.
func TestTranscriptHashesUpdateChains(t *testing.T) {
	crypto := trTestCrypto(t)
	hashes := InitialTranscriptHashes()
	firstTag := crypto.Mac(make([]byte, 32), []byte("one"))
	if err := hashes.Update(crypto, []byte("commit one"), firstTag); err != nil {
		t.Fatalf("Update: %v", err)
	}
	afterFirst := hashes.Clone()
	secondTag := crypto.Mac(make([]byte, 32), []byte("two"))
	if err := hashes.Update(crypto, []byte("commit two"), secondTag); err != nil {
		t.Fatalf("Update: %v", err)
	}

	want := ConfirmedTranscriptHash(crypto, afterFirst.Interim, []byte("commit two"))
	if !bytes.Equal(hashes.Confirmed, want) {
		t.Fatalf("second confirmed hash does not chain from the first interim hash")
	}

	forked := afterFirst.Clone()
	if err := forked.Update(crypto, []byte("commit two prime"), secondTag); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if bytes.Equal(forked.Confirmed, hashes.Confirmed) {
		t.Fatal("two different commits produced the same confirmed transcript hash")
	}
}

// TestUpdateLeavesTheEpochWhereItWasWhenTheInterimWriterFails asserts the atomicity
// Update's own comment promises: "Neither field is written until both values exist, so a
// writer failure leaves the epoch where it was rather than half advanced."
//
// A half advanced epoch is the worst shape this type can be left in. It holds epoch n's
// confirmed hash beside epoch n-1's interim hash, which is a pair no member of the group has
// and which no length check, no encoding and no comparison against a published vector can
// see: both values are well formed hashes of the right width. The next commit is chained from
// it, and from there the member disagrees with everyone forever -- a permanent fork rather
// than an operation that failed and can be retried.
//
// The failure is reached through the codec rather than through a stub provider, because the
// codec is where Update's only error comes from: InterimTranscriptHashInput carries the
// confirmation tag as an opaque<V>, and syntax refuses a vector longer than MaxVectorLength.
// Nothing off the wire can be that long -- the Reader caps opaque<V> at the same limit -- so
// what this holds is the boundary a caller synthesising a tag would cross, and it holds it
// against the real writer rather than against an injected error that a different code path
// could answer.
func TestUpdateLeavesTheEpochWhereItWasWhenTheInterimWriterFails(t *testing.T) {
	crypto := trTestCrypto(t)
	nh := crypto.HashSize()
	hashes := InitialTranscriptHashes()
	if err := hashes.Update(crypto, []byte("commit one"), crypto.Mac(make([]byte, nh), []byte("one"))); err != nil {
		t.Fatalf("Update: %v", err)
	}
	before := hashes.Clone()

	// one octet past what an opaque<V> may carry. The control is read first, so a limit that
	// moved is reported here rather than turning the refusal below into a success and this
	// whole test into a comparison of an epoch against itself.
	oversized := make([]byte, syntax.MaxVectorLength+1)
	control := syntax.NewWriter()
	control.WriteOpaque(oversized)
	if _, err := control.Bytes(); err == nil {
		t.Fatalf("a %d octet opaque<V> encodes, so the tag below does not reach Update's writer failure and nothing here is about atomicity",
			len(oversized))
	}

	if err := hashes.Update(crypto, []byte("commit two"), oversized); err == nil {
		t.Fatal("Update accepted a confirmation tag no opaque<V> can carry")
	}
	if !bytes.Equal(hashes.Confirmed, before.Confirmed) {
		t.Errorf("a failed Update left Confirmed = %x and the epoch held %x; the epoch is half advanced and the next commit would chain from a pair nobody else has",
			hashes.Confirmed, before.Confirmed)
	}
	if !bytes.Equal(hashes.Interim, before.Interim) {
		t.Errorf("a failed Update left Interim = %x and the epoch held %x",
			hashes.Interim, before.Interim)
	}

	// and the control on the two comparisons above: the same commit under a tag the codec
	// accepts DOES move both fields, so an Update that had stopped writing anything at all
	// would be reported rather than reading as the atomicity this test is named for
	if err := hashes.Update(crypto, []byte("commit two"), crypto.Mac(make([]byte, nh), []byte("two"))); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if bytes.Equal(hashes.Confirmed, before.Confirmed) || bytes.Equal(hashes.Interim, before.Interim) {
		t.Fatal("a successful Update left the epoch where it was, so the comparisons above hold for an Update that writes neither field")
	}
}

// TestTranscriptHashesCloneIsDeep asserts a retained past epoch cannot be mutated.
func TestTranscriptHashesCloneIsDeep(t *testing.T) {
	crypto := trTestCrypto(t)
	hashes := InitialTranscriptHashes()
	if err := hashes.Update(crypto, []byte("commit"), crypto.Mac(make([]byte, 32), nil)); err != nil {
		t.Fatalf("Update: %v", err)
	}
	clone := hashes.Clone()
	clone.Confirmed[0] ^= 0xff
	clone.Interim[0] ^= 0xff
	if bytes.Equal(clone.Confirmed, hashes.Confirmed) {
		t.Fatal("Confirmed is shared")
	}
	if bytes.Equal(clone.Interim, hashes.Interim) {
		t.Fatal("Interim is shared")
	}
}

// TestCloneAnswersTheEmptyOctetStringRatherThanNil asserts a Clone does not change which of
// the two empty values a caller holds.
//
// Clone's own comment gives the reason it spells the copy cloneBytes rather than append:
// append to a nil slice collapses the empty non nil slices InitialTranscriptHashes hands out
// into nil, "and a clone that changed which of the two a caller holds changed the value".
// The pair run over here is therefore the base case, which is the only pair where the two
// spellings differ at all -- a clone of a post-Update epoch is non empty and every copier
// agrees on it, which is why TestTranscriptHashesCloneIsDeep cannot see this.
//
// Both directions are held. A copier that answered an empty slice where it was handed nil is
// the same substitution written the other way round and changes what a caller holds just as
// much, and a gate that read only the first direction is satisfied by a Clone that answers
// an empty slice for everything.
func TestCloneAnswersTheEmptyOctetStringRatherThanNil(t *testing.T) {
	clone := InitialTranscriptHashes().Clone()
	if len(clone.Confirmed) != 0 || len(clone.Interim) != 0 {
		t.Fatalf("a clone of the base case holds (%x, %x), want two zero-length values",
			clone.Confirmed, clone.Interim)
	}
	if clone.Confirmed == nil {
		t.Error("a clone of the base case answers nil for Confirmed, and the base case is the zero-length octet string")
	}
	if clone.Interim == nil {
		t.Error("a clone of the base case answers nil for Interim, and the base case is the zero-length octet string")
	}
	// the other direction, over the pair a zero value carries: a TranscriptHashes nothing has
	// seeded holds nil, and a copier that answered an empty slice for it changed that value
	absent := (&TranscriptHashes{}).Clone()
	if absent.Confirmed != nil {
		t.Errorf("a clone of a nil Confirmed answers %v, want nil", absent.Confirmed)
	}
	if absent.Interim != nil {
		t.Errorf("a clone of a nil Interim answers %v, want nil", absent.Interim)
	}
}

// TestSetFromGroupInfoSeedsAJoiner asserts a member added by Welcome reaches the same
// interim hash the existing members hold, which it must to process the next commit.
func TestSetFromGroupInfoSeedsAJoiner(t *testing.T) {
	crypto := trTestCrypto(t)
	member := InitialTranscriptHashes()
	tag := crypto.Mac(make([]byte, 32), []byte("epoch one"))
	if err := member.Update(crypto, []byte("commit one"), tag); err != nil {
		t.Fatalf("Update: %v", err)
	}

	joiner := InitialTranscriptHashes()
	if err := joiner.SetFromGroupInfo(crypto, member.Confirmed, tag); err != nil {
		t.Fatalf("SetFromGroupInfo: %v", err)
	}
	if !bytes.Equal(joiner.Confirmed, member.Confirmed) {
		t.Fatal("joiner confirmed hash differs")
	}
	if !bytes.Equal(joiner.Interim, member.Interim) {
		t.Fatal("joiner interim hash differs")
	}
	// and the joiner goes on agreeing: the next commit both apply has to reach the same
	// confirmed hash on both sides, which is the whole reason the interim is seeded
	next := []byte("the commit that arrives after the welcome")
	nextTag := crypto.Mac(make([]byte, 32), next)
	if err := member.Update(crypto, next, nextTag); err != nil {
		t.Fatalf("member Update: %v", err)
	}
	if err := joiner.Update(crypto, next, nextTag); err != nil {
		t.Fatalf("joiner Update: %v", err)
	}
	if !bytes.Equal(joiner.Confirmed, member.Confirmed) || !bytes.Equal(joiner.Interim, member.Interim) {
		t.Fatal("the joiner and the member diverged on the first commit after the welcome")
	}
}

// TestSetFromGroupInfoRejectsWrongLengths asserts a malformed GroupInfo is refused
// rather than seeding a member with a hash no peer agrees with.
//
// The refusals are swept over every length the suite does not permit rather than at the
// one byte short case, because a check written as "at least Nh" or "at most Nh" answers
// the sampled case correctly and takes half the class.
func TestSetFromGroupInfoRejectsWrongLengths(t *testing.T) {
	crypto := trTestCrypto(t)
	joiner := InitialTranscriptHashes()
	if err := joiner.SetFromGroupInfo(crypto, make([]byte, 31), make([]byte, 32)); !errors.Is(err, ErrTranscriptHashLength) {
		t.Fatalf("short confirmed hash err = %v, want ErrTranscriptHashLength", err)
	}
	if err := joiner.SetFromGroupInfo(crypto, make([]byte, 32), nil); !errors.Is(err, ErrTranscriptHashLength) {
		t.Fatalf("empty tag err = %v, want ErrTranscriptHashLength", err)
	}

	nh := crypto.HashSize()
	refused := 0
	for length := 0; length <= 2*nh; length++ {
		if length == nh {
			continue
		}
		if err := joiner.SetFromGroupInfo(crypto, make([]byte, length), make([]byte, nh)); !errors.Is(err, ErrTranscriptHashLength) {
			t.Errorf("a %d byte confirmed transcript hash was answered %v, want ErrTranscriptHashLength", length, err)
			continue
		}
		if err := joiner.SetFromGroupInfo(crypto, make([]byte, nh), make([]byte, length)); !errors.Is(err, ErrTranscriptHashLength) {
			t.Errorf("a %d byte confirmation tag was answered %v, want ErrTranscriptHashLength", length, err)
			continue
		}
		refused += 2
	}
	if want := 2 * 2 * nh; refused != want {
		t.Fatalf("%d wrong lengths were refused, want %d; the sweep is not covering the class", refused, want)
	}
	// the control on the sweep: the one permitted length is ACCEPTED, so a body that
	// refused everything would be reported rather than reading as the strictest possible
	// implementation
	if err := joiner.SetFromGroupInfo(crypto, make([]byte, nh), make([]byte, nh)); err != nil {
		t.Fatalf("SetFromGroupInfo refused the one pair of lengths this suite permits: %v", err)
	}
	// and a refusal leaves the joiner where it was rather than half seeded
	before := joiner.Clone()
	if err := joiner.SetFromGroupInfo(crypto, make([]byte, nh-1), make([]byte, nh)); err == nil {
		t.Fatal("a short confirmed hash was accepted")
	}
	if !bytes.Equal(joiner.Confirmed, before.Confirmed) || !bytes.Equal(joiner.Interim, before.Interim) {
		t.Fatal("a refused GroupInfo still moved the joiner's transcript")
	}
}

// TestSetFromGroupInfoCopiesTheGroupInfoRatherThanRetainingIt asserts the joiner's
// confirmed transcript hash is storage of its own.
//
// This is the one long lived retention of somebody else's bytes in this file, and the
// argument has the worst possible provenance for one: a GroupInfo is a decoded Welcome, the
// confirmed transcript hash is a field of the GroupContext inside it, and the caller that
// decoded it still owns the whole message. It goes on reading later fields out of that array,
// it may hand it back to a pool, and it may erase it once the Welcome is processed. A joiner
// that retained the slice holds a transcript that changes underneath it with no error path
// anywhere: the next commit is chained from bytes no peer has, and the group forks at the
// first commit after the welcome.
//
// The array is REUSED after the call rather than the two pointers being compared, because
// identity is only one of the two ways to share storage -- a copy taken by appending into a
// slice that still has the caller's capacity behind it aliases exactly as a retained slice
// does. Both readings are here, and the reuse is checked to have changed the field first, so
// a test whose overwrite had stopped overwriting is reported rather than reading as proof of
// a copy.
func TestSetFromGroupInfoCopiesTheGroupInfoRatherThanRetainingIt(t *testing.T) {
	crypto := trTestCrypto(t)
	nh := crypto.HashSize()
	// the decode buffer, longer than the field, because what the caller owns is the whole
	// GroupInfo and the argument is a window into it
	groupInfo := bytes.Repeat([]byte{0x5a}, 4*nh)
	confirmedTranscriptHash := groupInfo[nh : 2*nh]
	tag := crypto.Mac(make([]byte, nh), []byte("the welcome's confirmation tag"))

	joiner := InitialTranscriptHashes()
	if err := joiner.SetFromGroupInfo(crypto, confirmedTranscriptHash, tag); err != nil {
		t.Fatalf("SetFromGroupInfo: %v", err)
	}
	seededConfirmed := bytes.Clone(joiner.Confirmed)
	seededInterim := bytes.Clone(joiner.Interim)
	if !bytes.Equal(seededConfirmed, confirmedTranscriptHash) {
		t.Fatalf("the joiner seeded to %x and the GroupInfo carried %x", seededConfirmed, confirmedTranscriptHash)
	}

	// what a caller does next: reuse the array. A pooled decode buffer, a zeroize of a
	// processed Welcome and a second message read into the same storage all look like this.
	for i := range groupInfo {
		groupInfo[i] = 0x00
	}
	if bytes.Equal(seededConfirmed, confirmedTranscriptHash) {
		t.Fatal("reusing the caller's array did not change the field the joiner was seeded from, so nothing below separates a copy from a retained slice")
	}

	if !bytes.Equal(joiner.Confirmed, seededConfirmed) {
		t.Errorf("the joiner's confirmed hash is %x after its caller reused the GroupInfo buffer and was %x when it was seeded; the transcript aliases somebody else's array",
			joiner.Confirmed, seededConfirmed)
	}
	if !bytes.Equal(joiner.Interim, seededInterim) {
		t.Errorf("the joiner's interim hash is %x after its caller reused the GroupInfo buffer and was %x when it was seeded",
			joiner.Interim, seededInterim)
	}

	// and the same property read as storage rather than as a value: no byte the joiner holds
	// is a byte of the caller's array. Every offset is compared and not just the first, so a
	// slice cut from the middle of the buffer is as visible as one cut from its front.
	for _, held := range []struct {
		name    string
		content []byte
	}{
		{name: "Confirmed", content: joiner.Confirmed},
		{name: "Interim", content: joiner.Interim},
	} {
		if len(held.content) == 0 {
			t.Errorf("the joiner holds no %s, so that half of this gate observed nothing", held.name)
			continue
		}
		for i := range groupInfo {
			if &groupInfo[i] == &held.content[0] {
				t.Errorf("the joiner's %s is cut from the caller's GroupInfo buffer at offset %d", held.name, i)
				break
			}
		}
	}
}

// trPublishedEntry is one entry of mlswg's transcript-hashes.json, whole.
//
// p4 task 10 reads four of these six fields for the confirmation tag it holds
// (*KeySchedule).ConfirmationTag to; the transcript arithmetic needs the other two, so the
// row is declared again here rather than widened there -- task 10's struct documents itself
// as "the part of one entry this file reads", and the corpus is loaded through the same
// authenticating loader either way.
type trPublishedEntry struct {
	CipherSuite                  uint16 `json:"cipher_suite"`
	ConfirmationKey              string `json:"confirmation_key"`
	AuthenticatedContent         string `json:"authenticated_content"`
	InterimTranscriptHashBefore  string `json:"interim_transcript_hash_before"`
	ConfirmedTranscriptHashAfter string `json:"confirmed_transcript_hash_after"`
	InterimTranscriptHashAfter   string `json:"interim_transcript_hash_after"`
}

// trPublishedEntries is the vendored corpus, authenticated against upstream's git object
// store before it is read. An empty parse is fatal rather than clean: a filter that stopped
// matching, or a field renamed so every string decodes empty, turns every known answer below
// into a loop that runs zero times and reports PASS.
func trPublishedEntries(t *testing.T) []trPublishedEntry {
	t.Helper()
	entries := []trPublishedEntry{}
	mustLoadAuthenticatedCorpus(t, transcriptHashKatFile, &entries)
	if len(entries) == 0 {
		t.Fatalf("%s parsed to no entries, so every comparison below would run over nothing",
			transcriptHashKatFile)
	}
	for i, entry := range entries {
		if entry.AuthenticatedContent == "" || entry.ConfirmedTranscriptHashAfter == "" ||
			entry.InterimTranscriptHashBefore == "" || entry.InterimTranscriptHashAfter == "" {
			t.Fatalf("%s entry %d decoded with an empty field, so the json names below no longer match the corpus",
				transcriptHashKatFile, i)
		}
	}
	return entries
}

// trSplitPublishedCommit splits a serialized AuthenticatedContent carrying a Commit into
// its ConfirmedTranscriptHashInput prefix and its confirmation tag.
//
// AuthenticatedContent is wire_format || FramedContent || FramedContentAuthData, and for a
// commit the auth data is signature<V> followed by confirmation_tag, a MAC, which is an
// opaque<V> of exactly KDF.Nh octets. ConfirmedTranscriptHashInput is
// wire_format || FramedContent || signature<V>, which is precisely the prefix. So the split
// sits at len(ac) - (Nh + the width of that vector's own length prefix), and that width is
// read off the codec rather than written as 1: section 2.1.2 spells 32 as one octet and 64 as
// two, so a literal here is right for both registered suites and wrong for the first SHA-512
// one.
//
// It is checked twice rather than assumed. splitTrailingOpaqueTag refuses a tail whose
// preceding octets are not the varint length of what is being read, and the caller then
// verifies the recovered tag as MAC(confirmation_key, confirmed_transcript_hash_after) --
// the corpus's own stated verification step. A wrong split fails that MAC. When p6 lands
// (*AuthenticatedContent).UnmarshalMLS this is replaced by the parse and the MAC check
// stays.
//
// The split itself is in vectors_runner_test.go, shared with the family 7 runner that reads
// this same file through the vector registry: two implementations of one offset is how the
// two stop agreeing, and only one of them is checked by a MAC.
func trSplitPublishedCommit(t *testing.T, at string, crypto CryptoProvider, blob []byte) (confirmedInput []byte, confirmationTag []byte) {
	t.Helper()
	confirmedInput, confirmationTag, err := splitTrailingOpaqueTag(blob, crypto.HashSize())
	if err != nil {
		t.Fatalf("%s authenticated_content: %v", at, err)
	}
	return confirmedInput, confirmationTag
}

// TestTranscriptHashesMatchTheMlswgTranscriptHashes is the known answer test for both
// halves of section 8.2, against values this package did not compute.
//
// Both directions of one entry are held, because they fail differently: the confirmed hash
// is the concatenation with no separator, and the interim hash is the concatenation with the
// tag's varint prefix. An implementation that got either one right and the other wrong
// produces a group that agrees with itself and with nobody else.
//
// The comparison count is derived from the suite registry rather than written down, and
// every registered suite has to appear: a corpus renumbered, or a registry that grew a suite
// nothing here runs, is loud instead of green.
func TestTranscriptHashesMatchTheMlswgTranscriptHashes(t *testing.T) {
	entries := trPublishedEntries(t)
	expected := map[CipherSuite]int{}
	for _, entry := range entries {
		suite := CipherSuite(entry.CipherSuite)
		if IsRegisteredSuite(suite) {
			expected[suite]++
		}
	}
	for _, suite := range Suites() {
		if expected[suite] == 0 {
			t.Fatalf("%s carries no entry for registered suite %#04x, so nothing below runs at it",
				transcriptHashKatFile, uint16(suite))
		}
	}

	compared := 0
	seen := map[CipherSuite]int{}
	for index, entry := range entries {
		suite := CipherSuite(entry.CipherSuite)
		if !IsRegisteredSuite(suite) {
			continue
		}
		at := fmt.Sprintf("%s entry %d suite %#04x", transcriptHashKatFile, index, uint16(suite))
		crypto := mustProvider(t, suite)
		confirmationKey := mustDecodeHex(t, at+" confirmation_key", entry.ConfirmationKey)
		interimBefore := mustDecodeHex(t, at+" interim_transcript_hash_before", entry.InterimTranscriptHashBefore)
		confirmedAfter := mustDecodeHex(t, at+" confirmed_transcript_hash_after", entry.ConfirmedTranscriptHashAfter)
		interimAfter := mustDecodeHex(t, at+" interim_transcript_hash_after", entry.InterimTranscriptHashAfter)
		blob := mustDecodeHex(t, at+" authenticated_content", entry.AuthenticatedContent)
		confirmedInput, confirmationTag := trSplitPublishedCommit(t, at, crypto, blob)

		// the corpus's own verification step, and what makes the split honest rather than
		// assumed: a split taken at the wrong offset recovers bytes this MAC refuses
		if !crypto.MacVerify(confirmationKey, confirmedAfter, confirmationTag) {
			t.Fatalf("%s: the recovered confirmation tag does not verify as MAC(confirmation_key, confirmed_transcript_hash_after), so the split is wrong and nothing below is a known answer",
				at)
		}
		// and the published before and after are not the same value, so an implementation
		// that answered its own second argument would be reported rather than agreeing
		if bytes.Equal(interimBefore, confirmedAfter) || bytes.Equal(confirmedAfter, interimAfter) {
			t.Fatalf("%s: two of the published hashes are equal, so a comparison below holds for an implementation that answers the wrong one",
				at)
		}

		if got := ConfirmedTranscriptHash(crypto, interimBefore, confirmedInput); !bytes.Equal(got, confirmedAfter) {
			t.Errorf("%s: ConfirmedTranscriptHash = %x, and mlswg published %x", at, got, confirmedAfter)
		}
		got, err := InterimTranscriptHash(crypto, confirmedAfter, confirmationTag)
		if err != nil {
			t.Fatalf("%s: InterimTranscriptHash: %v", at, err)
		}
		if !bytes.Equal(got, interimAfter) {
			t.Errorf("%s: InterimTranscriptHash = %x, and mlswg published %x", at, got, interimAfter)
		}

		// the same epoch through the stateful API a group actually drives, seeded at the
		// published interim hash. Both free functions can be right while Update reads the
		// wrong field of the receiver or writes them back transposed.
		hashes := &TranscriptHashes{Confirmed: nil, Interim: interimBefore}
		if err := hashes.Update(crypto, confirmedInput, confirmationTag); err != nil {
			t.Fatalf("%s: Update: %v", at, err)
		}
		if !bytes.Equal(hashes.Confirmed, confirmedAfter) {
			t.Errorf("%s: Update left Confirmed = %x, and mlswg published %x", at, hashes.Confirmed, confirmedAfter)
		}
		if !bytes.Equal(hashes.Interim, interimAfter) {
			t.Errorf("%s: Update left Interim = %x, and mlswg published %x", at, hashes.Interim, interimAfter)
		}

		// and the joiner's path onto the same epoch, which is the third way to reach these
		// two values and the one a Welcome takes
		joiner := InitialTranscriptHashes()
		if err := joiner.SetFromGroupInfo(crypto, confirmedAfter, confirmationTag); err != nil {
			t.Fatalf("%s: SetFromGroupInfo: %v", at, err)
		}
		if !bytes.Equal(joiner.Confirmed, confirmedAfter) || !bytes.Equal(joiner.Interim, interimAfter) {
			t.Errorf("%s: a joiner seeded from this GroupInfo holds (%x, %x), and mlswg published (%x, %x)",
				at, joiner.Confirmed, joiner.Interim, confirmedAfter, interimAfter)
		}

		seen[suite]++
		compared++
	}
	total := 0
	for _, count := range expected {
		total += count
	}
	if compared != total {
		t.Fatalf("%d published epochs were compared and the corpus carries %d at registered suites; the loop matched %v",
			compared, total, seen)
	}
	if !maps2Equal(seen, expected) {
		t.Fatalf("the loop ran %v and the corpus carries %v at registered suites", seen, expected)
	}
	t.Logf("%d published transcript epochs compared across %d registered suites", compared, len(seen))
}

// maps2Equal compares two suite histograms. Written out because the two maps are built by
// different loops and a gate that compared only their totals would read a corpus whose
// entries had been reassigned between suites as unchanged.
func maps2Equal(left map[CipherSuite]int, right map[CipherSuite]int) bool {
	if len(left) != len(right) {
		return false
	}
	for suite, count := range left {
		if right[suite] != count {
			return false
		}
	}
	return true
}

// trEpochInputs names the per-epoch inputs the chain sweep mutates, in the order Update
// takes them. The list is held to Update's own arity below rather than trusted, so an input
// added to the signature fails the sweep instead of silently going unswept.
var trEpochInputs = []string{"confirmedTranscriptHashInput", "confirmationTag"}

const (
	trInputField = 0
	trTagField   = 1
)

// trChainInputs builds the epoch sequence the chain sweep runs over.
//
// The inputs are the published AuthenticatedContent blobs, one per corpus entry, so the
// number of epochs is the corpus's rather than a number chosen here and the bytes are real
// serialized commits rather than a string somebody typed. The tags are computed under this
// test's own provider so every one is KDF.Nh octets whatever suite the entry came from --
// what the sweep is about is the chaining, and these are the inputs it chains.
func trChainInputs(t *testing.T, crypto CryptoProvider) (inputs [][]byte, tags [][]byte) {
	t.Helper()
	for index, entry := range trPublishedEntries(t) {
		at := fmt.Sprintf("%s entry %d", transcriptHashKatFile, index)
		blob := mustDecodeHex(t, at+" authenticated_content", entry.AuthenticatedContent)
		key := mustDecodeHex(t, at+" confirmation_key", entry.ConfirmationKey)
		inputs = append(inputs, blob)
		tags = append(tags, crypto.Mac(key, blob))
	}
	return inputs, tags
}

// trRunChain applies a sequence of epochs and answers the pair held after each.
//
// The snapshot is taken with bytes.Clone rather than through Clone, so a defect in Clone is
// reported by the test whose name is about Clone rather than scrambling this one.
func trRunChain(t *testing.T, crypto CryptoProvider, inputs [][]byte, tags [][]byte) []*TranscriptHashes {
	t.Helper()
	hashes := InitialTranscriptHashes()
	held := make([]*TranscriptHashes, 0, len(inputs))
	for epoch := range inputs {
		if err := hashes.Update(crypto, inputs[epoch], tags[epoch]); err != nil {
			t.Fatalf("epoch %d: Update: %v", epoch, err)
		}
		held = append(held, &TranscriptHashes{
			Confirmed: bytes.Clone(hashes.Confirmed),
			Interim:   bytes.Clone(hashes.Interim),
		})
	}
	return held
}

// TestTranscriptHashChainCarriesEveryEarlierEpoch is the property a single-epoch known
// answer cannot reach.
//
// Every vector in mlswg's corpus is ONE epoch: an interim hash in, a confirmed and an
// interim hash out. An implementation that passes all seven of them and drops self.Interim
// from Update produces a group that forks at epoch two, and no entry of that corpus, and no
// test that applies one commit, can tell. So this runs a sequence and asserts exactly which
// later epochs a change at epoch i reaches -- and, just as load bearing, which earlier ones
// it must not.
//
// Nothing here is sampled. The sequence length is the published corpus's; the mutation
// positions are every epoch of it crossed with every input Update takes, the count of those
// read off the method's own signature; and the assertion runs over every epoch of the chain
// rather than the last one.
//
// The two inputs diverge at different points, and that asymmetry is the sharpest thing this
// test holds. ConfirmedTranscriptHashInput at epoch i enters confirmed_[i] directly, so the
// confirmed hash differs from epoch i onward. The confirmation tag at epoch i enters
// interim_[i], which is not consumed until epoch i+1, so confirmed_[i] is UNCHANGED and the
// confirmed hashes differ only from i+1. An implementation that folded the tag into its own
// epoch's confirmed hash, or that never folded it in at all, breaks one half of that and not
// the other.
func TestTranscriptHashChainCarriesEveryEarlierEpoch(t *testing.T) {
	crypto := trTestCrypto(t)
	// the receiver and the provider are the two parameters that are not per-epoch inputs
	if arity := reflect.TypeOf((*TranscriptHashes).Update).NumIn() - 2; arity != len(trEpochInputs) {
		t.Fatalf("Update takes %d inputs per epoch and this sweep mutates %d of them (%v); widen the sweep",
			arity, len(trEpochInputs), trEpochInputs)
	}
	inputs, tags := trChainInputs(t, crypto)
	epochs := len(inputs)
	if epochs < 3 {
		t.Fatalf("the chain is %d epochs long; at least three are needed for a mutation position to have both an earlier epoch and a strictly later one", epochs)
	}
	base := trRunChain(t, crypto, inputs, tags)

	// the control on the chain itself: every epoch of an unaltered run has to be distinct,
	// or the equalities below would hold for an implementation that answered one constant
	for i := range base {
		for j := range base {
			if i == j {
				continue
			}
			if bytes.Equal(base[i].Confirmed, base[j].Confirmed) {
				t.Fatalf("epochs %d and %d of an unaltered chain hold the same confirmed hash, so nothing below separates a chain from a constant", i, j)
			}
		}
	}

	swept := 0
	for position := 0; position < epochs; position++ {
		for field := range trEpochInputs {
			alteredInputs := slices.Clone(inputs)
			alteredTags := slices.Clone(tags)
			switch field {
			case trInputField:
				alteredInputs[position] = bytes.Clone(inputs[position])
				alteredInputs[position][0] ^= 0x01
			case trTagField:
				alteredTags[position] = bytes.Clone(tags[position])
				alteredTags[position][0] ^= 0x01
			}
			altered := trRunChain(t, crypto, alteredInputs, alteredTags)

			// the confirmed hash carries an altered input from that epoch and an altered
			// tag only from the epoch after it, because the tag reaches the chain through
			// the interim hash
			firstConfirmed := position
			if field == trTagField {
				firstConfirmed = position + 1
			}
			for epoch := 0; epoch < epochs; epoch++ {
				sameConfirmed := bytes.Equal(base[epoch].Confirmed, altered[epoch].Confirmed)
				if epoch >= firstConfirmed && sameConfirmed {
					t.Errorf("altering %s at epoch %d left the confirmed hash at epoch %d unchanged; the chain does not carry it forward",
						trEpochInputs[field], position, epoch)
				}
				if epoch < firstConfirmed && !sameConfirmed {
					t.Errorf("altering %s at epoch %d changed the confirmed hash at the earlier epoch %d",
						trEpochInputs[field], position, epoch)
				}
				sameInterim := bytes.Equal(base[epoch].Interim, altered[epoch].Interim)
				if epoch >= position && sameInterim {
					t.Errorf("altering %s at epoch %d left the interim hash at epoch %d unchanged",
						trEpochInputs[field], position, epoch)
				}
				if epoch < position && !sameInterim {
					t.Errorf("altering %s at epoch %d changed the interim hash at the earlier epoch %d",
						trEpochInputs[field], position, epoch)
				}
			}
			swept++
		}
	}
	if want := epochs * len(trEpochInputs); swept != want {
		t.Fatalf("the sweep ran %d mutations over a %d epoch chain, want %d", swept, epochs, want)
	}
	t.Logf("%d mutation positions swept over a %d epoch chain, %d assertions each",
		swept, epochs, 2*epochs)
}

// TestTheTwoTranscriptHashesAreNotSubstitutable holds the confirmed and interim values
// apart at each of the three places one could stand in for the other.
//
// They are the same length, they are both indistinguishable from random, and every one of
// these substitutions is a one line edit that leaves the group agreeing with itself. What
// separates them is only that the interim value has the confirmation tag folded into it and
// the confirmed one does not.
func TestTheTwoTranscriptHashesAreNotSubstitutable(t *testing.T) {
	crypto := trTestCrypto(t)
	entries := trPublishedEntries(t)
	checked := 0
	for index, entry := range entries {
		suite := CipherSuite(entry.CipherSuite)
		if !IsRegisteredSuite(suite) {
			continue
		}
		at := fmt.Sprintf("%s entry %d suite %#04x", transcriptHashKatFile, index, uint16(suite))
		crypto = mustProvider(t, suite)
		interimBefore := mustDecodeHex(t, at, entry.InterimTranscriptHashBefore)
		confirmedAfter := mustDecodeHex(t, at, entry.ConfirmedTranscriptHashAfter)
		interimAfter := mustDecodeHex(t, at, entry.InterimTranscriptHashAfter)
		blob := mustDecodeHex(t, at, entry.AuthenticatedContent)
		confirmedInput, confirmationTag := trSplitPublishedCommit(t, at, crypto, blob)

		// one. the two arithmetics are different functions of the same two arguments, so
		// neither free function can be called where the other belongs
		confirmed := ConfirmedTranscriptHash(crypto, confirmedAfter, confirmationTag)
		interim, err := InterimTranscriptHash(crypto, confirmedAfter, confirmationTag)
		if err != nil {
			t.Fatalf("%s: InterimTranscriptHash: %v", at, err)
		}
		if bytes.Equal(confirmed, interim) {
			t.Errorf("%s: the confirmed and interim arithmetics answer the same bytes for one pair of arguments, so one is the other under another name", at)
		}

		// two. the pair a live epoch holds is two values, not one written twice
		if bytes.Equal(confirmedAfter, interimAfter) {
			t.Fatalf("%s: mlswg published the same value for both hashes of this epoch, so nothing below separates them", at)
		}
		hashes := &TranscriptHashes{Confirmed: nil, Interim: interimBefore}
		if err := hashes.Update(crypto, confirmedInput, confirmationTag); err != nil {
			t.Fatalf("%s: Update: %v", at, err)
		}
		if bytes.Equal(hashes.Confirmed, hashes.Interim) {
			t.Errorf("%s: an epoch's two hashes are equal after Update", at)
		}

		// three. chaining the NEXT epoch from the confirmed value instead of the interim
		// one is the substitution a transposed pair of assignments makes, and it has to
		// reach a different answer
		next := []byte("the next commit")
		fromInterim := ConfirmedTranscriptHash(crypto, hashes.Interim, next)
		fromConfirmed := ConfirmedTranscriptHash(crypto, hashes.Confirmed, next)
		if bytes.Equal(fromInterim, fromConfirmed) {
			t.Errorf("%s: chaining the next epoch from the confirmed hash reaches the same answer as chaining it from the interim hash", at)
		}

		// four. a joiner handed the interim hash where the confirmed one belongs must not
		// land on the group's interim value; if it did, a GroupInfo whose two fields were
		// transposed would join successfully and fork on the next commit
		transposed := InitialTranscriptHashes()
		if err := transposed.SetFromGroupInfo(crypto, interimAfter, confirmationTag); err != nil {
			t.Fatalf("%s: SetFromGroupInfo over the transposed pair: %v", at, err)
		}
		if bytes.Equal(transposed.Interim, interimAfter) {
			t.Errorf("%s: a joiner seeded with the interim hash in the confirmed hash's place reached the group's interim value anyway", at)
		}
		checked++
	}
	if checked == 0 {
		t.Fatalf("no entry of %s ran at a registered suite, so this gate compared nothing", transcriptHashKatFile)
	}
	t.Logf("%d published epochs held to the two hashes being distinct in four ways each", checked)
}

// trRecordLayerCodecMethods is every entry point of the codec that spells a length the
// record layer's way, derived from the codec's own types rather than named.
//
// The class is "the LP suffixed methods of syntax.Writer and syntax.Reader", which is the
// naming rule encode.go states: LP(x) is the master protocol design's notation for a fixed
// 32 bit big endian length, and every method carrying it is a record layer entry point. A
// hand written list here would say WriteOpaqueLP and miss WriteNestedLP, ReadOpaqueLP,
// ReadSubLP and ReadNestedLP -- which is the shape of enumeration this project has already
// paid for more than once.
func trRecordLayerCodecMethods(t *testing.T) []string {
	t.Helper()
	found := []string{}
	notRecordLayer := []string{}
	for _, of := range []reflect.Type{
		reflect.TypeOf((*syntax.Writer)(nil)),
		reflect.TypeOf((*syntax.Reader)(nil)),
	} {
		for i := 0; i < of.NumMethod(); i++ {
			if name := of.Method(i).Name; strings.HasSuffix(name, "LP") {
				found = append(found, name)
			} else {
				notRecordLayer = append(notRecordLayer, of.Elem().Name()+"."+name)
			}
		}
	}
	slices.Sort(found)
	found = slices.Compact(found)
	if len(found) == 0 {
		t.Fatalf("no LP suffixed method was read off syntax.Writer or syntax.Reader, so the gate below forbids nothing")
	}
	// and the COMPLEMENT is printed. This is a name test over a reflected method set, the shape
	// GATES.md indexes, and until now it removed twenty-odd methods without naming one of them.
	// The sentence that puts them out is the naming rule encode.go states: LP(x) is the master
	// protocol design notation for a fixed 32 bit big endian length, so a method carrying the
	// suffix is a record layer entry point and one without it is not.
	if len(notRecordLayer) == 0 {
		t.Fatalf("every method of syntax.Writer and syntax.Reader carries the LP suffix, so this name test removes nothing")
	}
	t.Logf("the record layer entry points are %v; the %d methods of syntax.Writer and syntax.Reader this suffix removes, none of which is a fixed 32 bit length prefixed entry point, are %v",
		found, len(notRecordLayer), notRecordLayer)
	return found
}

// trCallsToMethods answers every call in a parsed file whose selector names one of the given
// methods, as "file DECLARATION: name" so a failure points at the line's file AND at the
// declaration that made the call.
//
// THE DECLARATION IS IN THE ANSWER BECAUSE THE ONE SANCTIONED SITE BELOW IS A DECLARATION. A file
// was the whole of this reading until ledger item 228's pairwise exporter, and a sanction keyed on
// a file would have licensed every future call in that file -- which is the shape GATES.md indexes
// and which this project has shipped as a base-name exemption before. A call outside any function
// reads as "(file scope)", which no sanction names.
func trCallsToMethods(parsed parsedSource, path string, methods []string) []string {
	found := []string{}
	declaration := "(file scope)"
	ast.Inspect(parsed.file, func(node ast.Node) bool {
		if function, isFunction := node.(*ast.FuncDecl); isFunction {
			declaration = trDeclarationName(function)
			return true
		}
		call, isCall := node.(*ast.CallExpr)
		if !isCall {
			return true
		}
		selector, isSelector := call.Fun.(*ast.SelectorExpr)
		if !isSelector {
			return true
		}
		if slices.Contains(methods, selector.Sel.Name) {
			found = append(found, path+" "+declaration+": "+selector.Sel.Name)
		}
		return true
	})
	return found
}

// trDeclarationName is a function declaration's name with its receiver type in front of it, which
// is how every other gate in this package spells a frame.
func trDeclarationName(function *ast.FuncDecl) string {
	if function.Recv == nil || len(function.Recv.List) != 1 {
		return function.Name.Name
	}
	rendered := ""
	ast.Inspect(function.Recv.List[0].Type, func(node ast.Node) bool {
		switch read := node.(type) {
		case *ast.StarExpr:
			rendered += "*"
		case *ast.Ident:
			rendered += read.Name
		}
		return true
	})
	return rendered + "." + function.Name.Name
}

// trRecordLayerSanctioned is the DECLARATION -- one, today -- that is allowed to spell a length the
// record layer's way inside package mls, with the reason beside it.
//
// THE RULE THIS GATE ENFORCES IS ABOUT MLS STRUCTURES, and that is what makes one exemption
// possible without the rule losing its meaning. The cost the gate's own header describes is a FORK
// AT THE FIRST CROSS-IMPLEMENTATION JOIN: an MLS structure spelled with a fixed 32 bit prefix is 32
// well formed octets every member of a group running this code agrees on and no other
// implementation does. encodePairwiseContext builds nothing any other implementation ever reads --
// it is a KDF context consumed by HKDF-Expand inside this profile and it appears on no wire and in
// no MLS structure -- so the failure this gate exists to catch cannot happen there, and ledger item
// 228 rules the fixed width prefix for it by name.
//
// It is held in BOTH DIRECTIONS below: a sanctioned declaration that no longer makes the call fails
// here rather than sitting on as a licence for the next one.
var trRecordLayerSanctioned = map[string]string{
	"group_pairwise.go encodePairwiseContext: WriteOpaqueLP": "ledger item 228's pairwise exporter context, which is a KDF preimage inside this profile and is not an MLS structure on any wire",
}

// TestNoMlsEncodingReachesTheRecordLayerLengthPrefix is guardrail-shaped rather than
// behavioural, and it is here because of what the substitution costs.
//
// syntax.WriteOpaque is MLS's varint prefix; syntax.WriteOpaqueLP is the record layer's
// fixed 32 bit one. connect/message builds every AAD and write_auth preimage with the second
// and package mls builds every MLS structure with the first, and encode.go's own comment
// says they are never interchangeable. The substitution is one identifier, it compiles, and
// the interim transcript hash it produces is 32 well formed bytes that every member of a
// group running it agrees on -- so it is invisible to every round trip and every self
// consistent comparison, and visible only against another implementation. That is a fork
// discovered at the first cross-implementation join.
//
// The class is derived twice over: the forbidden methods are read off the codec's types by
// their naming rule, and the files scanned are this package's whole non test source rather
// than transcript.go, because a gate that names one file goes on reporting a clean run while
// the substitution is written next door.
func TestNoMlsEncodingReachesTheRecordLayerLengthPrefix(t *testing.T) {
	methods := trRecordLayerCodecMethods(t)
	if !slices.Contains(methods, "WriteOpaqueLP") {
		t.Fatalf("the derived class is %v and WriteOpaqueLP is not among them, so this gate is not reading the codec it claims to", methods)
	}

	// the positive control, so a scanner that stopped matching fails here rather than
	// issuing the real source a clean bill
	control := mustParseText(t, "the record layer control", trRecordLayerControl)
	if found := trCallsToMethods(control, "control", methods); len(found) != 2 {
		t.Fatalf("the scan read %v out of a control making one forbidden write and one forbidden read, want two", found)
	}
	allowed := mustParseText(t, "the mls encoding control", trMlsEncodingControl)
	if found := trCallsToMethods(allowed, "control", methods); len(found) != 0 {
		t.Fatalf("the scan reported %v for a control that only ever spells an MLS vector, so every finding below is a defect that is not there", found)
	}

	scanned := 0
	offending := []string{}
	for _, path := range packageSourcePaths(t) {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		scanned++
		offending = append(offending, trCallsToMethods(mustParseSource(t, path), path, methods)...)
	}
	if scanned == 0 {
		t.Fatalf("no non test source file was scanned, so this gate read nothing")
	}
	// the sanction is applied HERE rather than inside the scanner, so what it removes is printed
	// and the complement of the narrowing is visible on every run
	sanctioned := []string{}
	remaining := []string{}
	for _, frame := range offending {
		if reason, isSanctioned := trRecordLayerSanctioned[frame]; isSanctioned {
			sanctioned = append(sanctioned, frame+" -- "+reason)
			continue
		}
		remaining = append(remaining, frame)
	}
	if len(remaining) != 0 {
		t.Errorf("%v spell a length the record layer's way inside package mls; MLS vectors are opaque<V> and the two encodings are never interchangeable",
			remaining)
	}
	// and the other direction: a sanction whose declaration no longer makes the call is a licence
	// for whatever is written there next, so it has to be deleted rather than left standing
	for frame, reason := range trRecordLayerSanctioned {
		if !slices.Contains(offending, frame) {
			t.Errorf("%s is sanctioned as %q and no longer spells a length that way: drop the row rather than leaving it to license the next call in that declaration",
				frame, reason)
		}
	}
	slices.Sort(sanctioned)
	t.Logf("%d non test files scanned for %v; %d sanctioned call(s): %v", scanned, methods,
		len(sanctioned), sanctioned)
}

// One forbidden write and one forbidden read. Every matcher above runs on this, so a scan
// that started reading only the receiver name, or only the first call of a file, fails here.
const trRecordLayerControl = `package mls

func encodeTheWrongWay(w *syntax.Writer, r *syntax.Reader, tag []byte) {
	w.WriteOpaqueLP(tag)
	_, _ = r.ReadOpaqueLP()
}
`

// The same two operations spelled MLS's way, so a matcher that had widened to every method
// whose name contains Opaque fails here rather than reporting the real source as offending.
const trMlsEncodingControl = `package mls

func encodeTheRightWay(w *syntax.Writer, r *syntax.Reader, tag []byte) {
	w.WriteOpaque(tag)
	_, _ = r.ReadOpaque()
}
`
