// The runner for the mlswg message-protection vector family, number 4.
//
// This is the eighth family to register against the registry in vectors_test.go, and none of
// the machinery the seven before it needed is repeated here: the suite filter is
// implementedSuite, the accounting is vectorRunTally, the comparator control is
// assertComparatorRefuses, the registration assertion is assertVectorFamilyIsInstalled and
// the second decode of the corpus is publishedCorpusField, all of them in
// vectors_runner_test.go. An eighth independent copy of that would be an eighth place to
// rediscover that a comparator returning nothing reports a call that returned rather than a
// comparison that happened.
//
// What this family is. One case publishes a group's context -- group id, epoch, tree hash,
// confirmed transcript hash -- together with the four secrets a member protects and unprotects
// with, and then five protected messages beside the three raw values they carry: a proposal
// and a commit as PublicMessages and as PrivateMessages, and an application message as a
// PrivateMessage only. So one case is a complete member's-eye view of section 6, and the
// procedure is RFC 9420 appendix A's own: rebuild the group context, open each protected
// message, and get the published raw value back out of it.
//
// What is this family's own, and why each part is a part:
//
//   - the re-seal held BYTE EXACT against the published pub column. A PublicMessage carries no
//     randomness -- the signature is already inside the opened structure and the membership tag
//     is a MAC over it -- so sealing the opened content again must reproduce the corpus's own
//     octets, and it does, for both public columns of both registered suites. That is a
//     comparison of this encoder against foreign bytes rather than against itself, and it is
//     strictly more than appendix A asks for, which is only that the re-sealed message verify.
//     A message that verifies says the tag was computed over what the encoder wrote; it says
//     nothing about the encoder having written what a peer would.
//   - the private columns held to a ROUND TRIP and not to their bytes, because a PrivateMessage
//     cannot be reproduced: section 6.3.2's reuse guard is four fresh random octets per message,
//     so two seals of one content never match. What is held instead is that resealing through an
//     independently constructed secret tree and reopening through a third one recovers the same
//     authenticated content, and that the two seals produce different CONTENT CIPHERTEXTS. The
//     content ciphertext and not the whole message: the guard is also carried inside the encrypted
//     sender data, so two whole messages differ whether or not the guard ever reaches the nonce,
//     and comparing those would be a check that passes over a guard applied to nothing. Two seals
//     at one generation of one ratchet share a key and a base nonce, so the content ciphertexts
//     are equal exactly when the guard did not reach the nonce -- which is a nonce reused across
//     two messages, the failure the guard exists to prevent, and one no round trip can see.
//   - the group context compared against a second opinion. Every signature and every tag in this
//     family is over the serialized GroupContext, so a codec that serialized it differently from
//     the RFC would fail every open here with a signature error and name nothing. The comparison
//     is against independentGroupContext in key_schedule_kat_test.go, which is section 8.1
//     written out by hand with the version as a literal, so the refusal says which of the two
//     moved.
//   - the application arm's refusal. Section 6 forbids an application message on the
//     PublicMessage path, and the corpus publishes no application_pub column precisely because
//     there cannot be one; the obligation is therefore that sealing the corpus's own application
//     content as a PublicMessage is REFUSED, which is a thing no published answer can express.
//
// What this runner adds over the two tests that already read this corpus, stated because a
// runner that adds no protection of its own is better replaced by a comment.
// TestTheFramedContentSignatureIsTheOneMlswgPublished in framing_protect_test.go verifies the
// published signature over the two PUBLIC columns, and
// TestTheMembershipTagPreimageIsTheOneThePublishedTagsWereTakenOver holds the membership tag
// over the same two. Between them they read eleven of the corpus's eighteen columns and neither
// touches a private one: the three encryption_secret ratchets, the sender_data_secret header
// seal, the AEAD over the content, the application payload, and the raw values every column
// carries are unread by anything on this branch. This runner opens all five protected columns,
// recovers the three raw values out of them, holds the two public columns byte exact through a
// re-seal, and installs family 4 so the corpus is offered to it by TestVectorFamiliesVerify.
//
// What this family cannot see, stated because a runner that overstates itself is worse than one
// that does less, and measured rather than supposed.
//
// The re-seal comparison is not breakable by any edit to a published case: the resealed bytes are
// a function of the opened message, so a case that made them differ would have failed the open
// first. It is therefore absent from the refusal table below and reachable only by mutation.
//
// The two PUBLIC content comparisons are redundant with the two private ones, because both halves
// of a case carry the same raw value: replacing the recovered proposal and commit with the
// published ones in the public arm alone leaves this file green, and it is the private arm's
// comparison of the same two columns that then does the work. Replacing them in BOTH arms is
// refused, by three rows of the table below at once. So the family's content comparison is
// observed; either arm of it alone is not.
//
// And nothing here is the SOLE catcher of a code defect -- a claim first measured over a list of
// edits that had a hole in it, and the hole is written down here rather than quietly filled in.
// Every edit on the original list -- an epoch off by one inside the section 6.3.2 AAD, the two
// ratchets exchanged, the reuse guard xored onto the tail of the nonce, the reuse guard drawn as a
// constant, the SenderData codec transposed, the GroupContext codec transposed -- is refused by
// this runner AND by a hand derived preimage or golden test elsewhere in the package. The edit the
// list did not hold is the PrivateMessageContent codec transposed: section 6.3.1's auth data
// written before the content arm by the encoder and read before it by the decoder. That one is
// SYMMETRIC, so every round trip in the package still passes and every encode-then-decode still
// agrees, and MEASURED it failed four tests on the whole branch -- this runner, its installation
// gate, its comparator control, and the registry that drives it, which is family 4 four times over
// and nothing else. It is now also refused by
// TestEveryRegisteredContentTypeEncodesToThePrivateMessageContentLayoutSection631Writes in
// framing_protect_test.go, section 6.3.1 written out by hand over every registered content type in
// both directions, so the claim above holds again over a list one edit longer than the one it was
// first measured on.
//
// What this family holds alone is the three PRIVATE columns, which nothing else on this branch
// opens at all, the three raw values every column carries, the byte exact re-seal, and the
// accounting: fourteen comparisons against nine distinct published answers.
package mls

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect/mls/syntax"
)

// The accounting that makes this runner unable to pass having compared nothing.
//
// Transcriptions of what testdata/vectors/message-protection.json holds at the pinned mlswg
// commit: seven cases, one at each published ciphersuite, of which the two this package
// registers account for two. Seven answers are compared per case -- the raw value behind each
// of the five protected columns, and the byte exact re-seal of each of the two public ones --
// so fourteen comparisons. They are made against nine distinct published strings and not
// fourteen, because both covered cases publish the same remove proposal.
//
// Written down rather than derived, for the reason p4 task 16 gives: deriving the expected
// count with the same filter that is under test is how a filter matching nothing ends up
// agreeing with itself. What IS derived and checked alongside them is that covered plus skipped
// equals the number of cases read, that the per suite split is the corpus's own census of
// itself, and that the seven checks are exactly the columns the corpus publishes -- see
// TestMessageProtectionChecksAreEveryProtectedColumnTheCorpusPublishes.
const (
	messageProtectionCovered = 2
	messageProtectionSkipped = 5
	// the columns of a case that carry a protected message: proposal and commit as
	// PublicMessages, and those two plus application as PrivateMessages. Each owes the raw
	// value it carries, and each PUBLIC one owes a byte exact re-seal on top of that.
	messageProtectionPublicColumns  = 2
	messageProtectionPrivateColumns = 3
	messageProtectionProtected      = messageProtectionPublicColumns + messageProtectionPrivateColumns
	messageProtectionChecks         = messageProtectionProtected + messageProtectionPublicColumns
	messageProtectionComparisons    = messageProtectionCovered * messageProtectionChecks
	messageProtectionDistinct       = 9
)

// The group the corpus's messages were protected in: two members, and the sender is the second
// of them.
//
// Appendix A fixes both. They are named rather than spelled at the four call sites because a
// secret tree built for a different member count derives different ratchets and every private
// column then fails to open, which is a failure that says nothing about where the disagreement
// is. TestMessageProtectionSenderIsTheLeafEveryPublishedMessageNames holds the corpus to the
// leaf, so a corpus update that moved the sender is a failure here rather than five signature
// errors.
const (
	messageProtectionLeafCount  = LeafCount(2)
	messageProtectionSenderLeaf = LeafIndex(1)
)

// Family 4 is installed here, and 4 is deleted from expectedPendingFamilies in the same commit.
// Without both halves TestVectorFamiliesVerify runs one fewer family and the manifest gate stays
// green while claiming this family is unimplemented.
//
// Generate is not nil. Appendix A's generate direction for this family is not a byte comparison
// against the vendored file -- a PrivateMessage carries a fresh reuse guard, so no generator can
// reproduce one -- it is a freshly protected group fed back through the verify path. What makes
// it more than this package agreeing with itself is that the generator signs over
// independentGroupContext's bytes while the verifier verifies over the codec's, so the two halves
// are two implementations of the one structure every signature in this family covers.
func init() {
	RegisterVectorFamily(VectorFamily{
		Number:   4,
		Name:     "Message protection",
		File:     messageProtectionKatFile,
		Slice:    "A3",
		Verify:   verifyMessageProtectionVector,
		Generate: generateMessageProtectionVector,
	})
}

// The refusals compareMessageProtectionVector makes, as sentinels rather than as formatted
// strings, so a test can require a specific refusal rather than "some error".
//
// They are what makes the comparison observable at all. Every case of the vendored corpus agrees
// with this implementation, so a comparator that checked everything and a comparator that checked
// nothing produce identical runs over it; the only way to tell them apart is to hand it a case
// that is wrong on purpose and require the matching refusal, which is
// TestCompareMessageProtectionVectorRefusesACaseItShouldNotAccept.
var (
	errMessageProtectionIncomplete      = errors.New("the comparison reports values it cannot have computed")
	errMessageProtectionGroupContext    = errors.New("the serialized group context is not RFC 9420 section 8.1's")
	errMessageProtectionShape           = errors.New("a published column is not the message its name says it is")
	errMessageProtectionUnprotect       = errors.New("a published protected message did not unprotect")
	errMessageProtectionContext         = errors.New("an unprotected message does not name the group, epoch or sender the case publishes")
	errMessageProtectionApplicationPub  = errors.New("an application message was accepted onto the PublicMessage path")
	errMessageProtectionMismatch        = errors.New("an unprotected value does not match the published one")
	errMessageProtectionRoundTrip       = errors.New("a re-protected private message did not survive the round trip")
	errMessageProtectionReuseGuardStuck = errors.New("two seals of one content produced one ciphertext, so the reuse guard is not fresh per message")
)

// messageProtectionVector is one entry of message-protection.json, whole.
//
// messageProtectionKatEntry in key_schedule_test.go is the same corpus row and is NOT reused,
// which is a departure from transcript_kat_test.go's rule that one corpus row gets one
// declaration, and it is a departure with a reason: that struct carries eleven of the eighteen
// columns -- the public half -- because the two tests reading it need no private column and no
// secret beyond the membership key. Widening it would change what those tests decode for a
// reason that has nothing to do with them. What keeps the two from disagreeing about a key is
// asserted rather than assumed: TestMessageProtectionVectorDecodesEveryColumnTheCorpusPublishes
// holds every tag of that struct against this one, and both against the file.
//
// Binary fields are hex strings in the file and stay strings here: MustHex is the single decoder,
// HexOf the single encoder, and a struct holding []byte would need a second one at the json
// boundary. The json tags are also the CLASS this family's checks are derived from -- a column
// ending _pub or _priv is a protected message and owes a comparison, and the raw column it
// carries is that name with the suffix removed -- so a corpus that grew a column fails
// TestMessageProtectionChecksAreEveryProtectedColumnTheCorpusPublishes rather than being
// silently uncompared.
type messageProtectionVector struct {
	CipherSuite             uint16 `json:"cipher_suite"`
	GroupId                 string `json:"group_id"`
	Epoch                   uint64 `json:"epoch"`
	TreeHash                string `json:"tree_hash"`
	ConfirmedTranscriptHash string `json:"confirmed_transcript_hash"`
	SignaturePriv           string `json:"signature_priv"`
	SignaturePub            string `json:"signature_pub"`
	EncryptionSecret        string `json:"encryption_secret"`
	SenderDataSecret        string `json:"sender_data_secret"`
	MembershipKey           string `json:"membership_key"`
	Proposal                string `json:"proposal"`
	ProposalPub             string `json:"proposal_pub"`
	ProposalPriv            string `json:"proposal_priv"`
	Commit                  string `json:"commit"`
	CommitPub               string `json:"commit_pub"`
	CommitPriv              string `json:"commit_priv"`
	Application             string `json:"application"`
	ApplicationPriv         string `json:"application_priv"`
}

// messageProtectionCheckNames is every answer this runner compares for one case, named by the
// column it came out of and by what was done to it.
//
// The order is the order the comparator emits them in, and incomplete() holds a run to it
// position by position as well as requiring each name exactly once. The multiset is what stops a
// comparison dropped from the middle being made up for by another one made twice; the order is
// what keeps the names bound to the columns they are about, and a reorder that leaves every name
// present exactly once is a reorder a multiset cannot see.
var messageProtectionCheckNames = []string{
	"proposal_pub/content",
	"proposal_pub/reseal",
	"commit_pub/content",
	"commit_pub/reseal",
	"proposal_priv/content",
	"commit_priv/content",
	"application_priv/content",
}

// messageProtectionCheck is one value this package recovered held against one value the corpus
// published, named by the column and the operation that produced it and filed under the json key
// the published half lives at.
type messageProtectionCheck struct {
	// name is what produced the recovered half, and is one of messageProtectionCheckNames.
	name string
	// field is the json key the published half is published under, which the runner re-reads
	// out of a generic decode of the same case.
	field string
	got   []byte
	want  []byte
}

// messageProtectionComparison is what one run of compareMessageProtectionVector PRODUCED, and it
// is the only thing its callers are allowed to judge it by.
//
// A comparator returning a bool reports that control reached the bottom of the function and not
// that a comparison happened: an early return above it leaves the runner counting cases that
// never opened a message at all, and the run stays green. Every field below is written at the
// point the work that produces it happens, so a return that skipped the work reports the zero
// value and a caller that judges the values sees it.
type messageProtectionComparison struct {
	// inScope is true when the case's ciphersuite is one this package registers. A false here
	// is not a failure and not a skip: it is a case with no provider.
	inScope bool
	// hashSize is the suite's KDF.Nh, read off the provider rather than assumed.
	hashSize int
	// the group context every signature and every tag in this case is over, serialized twice:
	// by the production codec, and by hand from RFC 9420 section 8.1.
	groupContext       []byte
	independentContext []byte
	// applicationAsPublic is what SealPublicMessage answered when it was handed this case's own
	// application content. Section 6 forbids it, so the obligation is a refusal, and a nil here
	// is either an acceptance or a step that was never taken -- both of which are failures and
	// neither of which any published answer can express.
	applicationAsPublic error
	// contextsChecked counts the unprotected messages whose group id, epoch and sender leaf
	// were held against the ones the case publishes. Every protected column owes one.
	contextsChecked int
	// roundTrips counts the private columns that were resealed through an independently
	// constructed secret tree and reopened through a third one with the authenticated content
	// coming back byte identical, and resealsDiffer the private columns whose two independent
	// seals of one content came out as different CONTENT ciphertexts.
	roundTrips    int
	resealsDiffer int
	// checks is every comparison the run made, in the order it made them.
	checks []messageProtectionCheck
}

// incomplete reports whether the evidence a compared case must carry is missing or inconsistent,
// without looking at whether any answer was right.
//
// This is the vacuity half, split from the correctness half on purpose. bytes.Equal over two
// empty slices says they agree, so a check whose got or want is empty has compared nothing
// whatever the comparison would say about it -- and a runner that counted such checks would
// report the full seven having opened no message at all.
func (self messageProtectionComparison) incomplete() error {
	switch {
	case !self.inScope:
		return fmt.Errorf("%w: the case is out of scope and carries no comparison", errMessageProtectionIncomplete)
	case self.hashSize == 0:
		return fmt.Errorf("%w: no KDF.Nh was read from the provider", errMessageProtectionIncomplete)
	case len(self.groupContext) == 0 || len(self.independentContext) == 0:
		return fmt.Errorf("%w: the group context was serialized to %d octets by the codec and %d by hand, and every signature in this case is over it",
			errMessageProtectionIncomplete, len(self.groupContext), len(self.independentContext))
	case self.contextsChecked != messageProtectionProtected:
		return fmt.Errorf("%w: %d unprotected messages were held to the group, epoch and sender the case publishes, and it publishes %d protected columns",
			errMessageProtectionIncomplete, self.contextsChecked, messageProtectionProtected)
	case self.roundTrips != messageProtectionPrivateColumns:
		return fmt.Errorf("%w: %d private columns survived a reseal and reopen and the case publishes %d",
			errMessageProtectionIncomplete, self.roundTrips, messageProtectionPrivateColumns)
	case self.resealsDiffer != messageProtectionPrivateColumns:
		return fmt.Errorf("%w: %d private columns produced two different content ciphertexts from two seals and the case publishes %d",
			errMessageProtectionIncomplete, self.resealsDiffer, messageProtectionPrivateColumns)
	case len(self.checks) != len(messageProtectionCheckNames):
		return fmt.Errorf("%w: the run made %d comparisons and this family owes %d per case",
			errMessageProtectionIncomplete, len(self.checks), len(messageProtectionCheckNames))
	}
	// every name exactly once AND in messageProtectionCheckNames' own order. The count case
	// above makes the index safe.
	seen := map[string]int{}
	for index, check := range self.checks {
		if check.name != messageProtectionCheckNames[index] {
			return fmt.Errorf("%w: comparison %d is %s and this family emits %s there",
				errMessageProtectionIncomplete, index, check.name, messageProtectionCheckNames[index])
		}
		if len(check.got) == 0 || len(check.want) == 0 {
			return fmt.Errorf("%w: %s compared %d recovered octets against %d published ones, and an empty comparison agrees with anything",
				errMessageProtectionIncomplete, check.name, len(check.got), len(check.want))
		}
		if check.field == "" {
			return fmt.Errorf("%w: %s names no published field, so nothing independent of the comparator's own decode can re-read it",
				errMessageProtectionIncomplete, check.name)
		}
		seen[check.name]++
	}
	for _, name := range messageProtectionCheckNames {
		if seen[name] != 1 {
			return fmt.Errorf("%w: %s was compared %d times and this family compares it once per case",
				errMessageProtectionIncomplete, name, seen[name])
		}
	}
	return nil
}

// verdict is the whole judgement over one compared case: it must be complete, the two
// serializations of the group context must agree, the application content must have been refused
// on the public path, and every comparison must agree.
//
// The order is deliberate. A group context disagreement is a statement that nothing below could
// have been verified over the right bytes, and reporting it as a plain mismatch would let a test
// asking for one be satisfied by the other.
func (self messageProtectionComparison) verdict() error {
	if err := self.incomplete(); err != nil {
		return err
	}
	if !bytes.Equal(self.groupContext, self.independentContext) {
		return fmt.Errorf("%w: the codec wrote %s and section 8.1 written out by hand is %s",
			errMessageProtectionGroupContext, HexOf(self.groupContext), HexOf(self.independentContext))
	}
	if !errors.Is(self.applicationAsPublic, errApplicationMustBeCiphertext) {
		return fmt.Errorf("%w: sealing this case's application content as a PublicMessage answered %v",
			errMessageProtectionApplicationPub, self.applicationAsPublic)
	}
	for _, check := range self.checks {
		if !bytes.Equal(check.got, check.want) {
			return fmt.Errorf("%w: %s = %s, the corpus publishes %s for %s",
				errMessageProtectionMismatch, check.name, HexOf(check.got), HexOf(check.want), check.field)
		}
	}
	return nil
}

// messageProtectionProtectedColumns splits the corpus's own columns into the ones that carry a
// protected message, public and private.
//
// A protected column is one named <base>_pub or <base>_priv WHERE <base> IS ALSO A COLUMN, and
// the second half of that rule is the whole of why this is a function rather than a suffix test.
// The suffix alone reads signature_pub and signature_priv as protected messages -- they are a
// key pair -- and the class then holds seven columns, the checks derived from it hold nine, and
// every count in this file is one a run cannot reach. That was not hypothetical: the suffix
// version is what this file was written with and it is what the first run of it reported. The
// base column exists precisely because a protected message CARRIES a raw value, so requiring it
// is not a patch over the signature pair, it is the definition.
func messageProtectionProtectedColumns(t *testing.T) (public []string, private []string) {
	t.Helper()
	column := map[string]bool{}
	for _, tag := range messageProtectionColumnTags(t) {
		column[tag] = true
	}
	for _, tag := range messageProtectionColumnTags(t) {
		if base, cut := strings.CutSuffix(tag, "_pub"); cut && column[base] {
			public = append(public, tag)
		}
		if base, cut := strings.CutSuffix(tag, "_priv"); cut && column[base] {
			private = append(private, tag)
		}
	}
	return public, private
}

// messageProtectionColumnTags is every json key messageProtectionVector decodes, in declaration
// order.
//
// Derived by reflection over the struct rather than listed, for guardrail 5's reason: a listed
// class understates itself the moment a field lands beside the ones somebody remembered, and the
// gate then reports full coverage of a corpus it covers less of than it did yesterday.
//
// AND AN UNTAGGED FIELD IS FATAL RATHER THAN SKIPPED, which is the repair of a narrowing this
// package indexes in GATES.md. The skip read as harmless because the complement is EMPTY today --
// every field of the struct carries a tag -- and an empty complement is the one GATES.md's own
// table calls the dangerous case: it removes nothing now and begins removing real columns on the
// commit that adds the first untagged field. encoding/json decodes such a field under its GO
// NAME, so the column would decode perfectly well and this function, whose whole claim is "every
// json key this struct decodes", would answer without it -- and every count derived from it would
// be one short against a corpus that publishes the key.
func messageProtectionColumnTags(t *testing.T) []string {
	t.Helper()
	tags := []string{}
	shape := reflect.TypeOf(messageProtectionVector{})
	for index := 0; index < shape.NumField(); index++ {
		key, _, _ := strings.Cut(shape.Field(index).Tag.Get("json"), ",")
		if key == "" {
			t.Fatalf("messageProtectionVector.%s carries no json key, so it decodes under its go name and every class derived from this table would be one column short",
				shape.Field(index).Name)
		}
		tags = append(tags, key)
	}
	return tags
}

// theGroupContextOf is the serialized GroupContext one case describes, through the production
// codec.
//
// C4: callers build these with syntax.Marshal over the key schedule plan's struct, so the harness
// and the production path use one serializer and there is no MarshalGroupContext for a second one
// to drift from.
func (self *messageProtectionVector) theGroupContextOf(t *testing.T, suite CipherSuite) []byte {
	t.Helper()
	encoded, err := syntax.Marshal(&GroupContext{
		Version:                 ProtocolVersionMls10,
		CipherSuite:             suite,
		GroupId:                 MustHex(t, self.GroupId),
		Epoch:                   self.Epoch,
		TreeHash:                MustHex(t, self.TreeHash),
		ConfirmedTranscriptHash: MustHex(t, self.ConfirmedTranscriptHash),
	})
	if err != nil {
		t.Fatalf("serialize the group context of a %s case: %v", messageProtectionKatFile, err)
	}
	return encoded
}

// verifyMessageProtectionVector is the registry's shim: the signature RegisterVectorFamily needs,
// over the comparator that does the work and reports what it produced.
//
// Verify cannot return anything, so a runner counting calls to it would count a case it declined
// exactly as it counts one it compared. That is the split, and it is the defect p4 task 16
// shipped and then had to fix.
func verifyMessageProtectionVector(t *testing.T, raw json.RawMessage) {
	t.Helper()
	evidence, err := compareMessageProtectionVector(t, raw)
	if err != nil {
		t.Fatalf("%s: %v", messageProtectionKatFile, err)
	}
	if !evidence.inScope {
		return
	}
	if err := evidence.verdict(); err != nil {
		t.Fatalf("%s: %v", messageProtectionKatFile, err)
	}
}

// refuseMessageProtectionVector is the comparator in the shape assertComparatorRefuses drives: a
// verdict rather than a fatal, so a control can require a refusal instead of ending the test that
// asked for one.
//
// A case at an unimplemented suite is refused HERE and only here. It is not a defect -- five of
// the seven published cases are in that state and the family runner declines them -- but a
// control that handed the comparator a case it does not run would be a control satisfied by not
// running.
func refuseMessageProtectionVector(t *testing.T, raw json.RawMessage) error {
	t.Helper()
	evidence, err := compareMessageProtectionVector(t, raw)
	if err != nil {
		return err
	}
	if !evidence.inScope {
		return fmt.Errorf("%w: the case is at a ciphersuite this package does not register",
			errMessageProtectionIncomplete)
	}
	return evidence.verdict()
}

// compareMessageProtectionVector runs one case of message-protection.json and returns what the
// run produced. A case at a ciphersuite this package does not implement is not a failure and not
// a skip: it comes back with inScope false and nothing else set.
//
// A corpus that will not parse or will not hex decode is fatal here rather than returned, because
// it is not a verdict about this implementation -- it is the evidence itself being unreadable,
// and every family in this package treats that as the loudest failure there is. Everything that
// IS a verdict about this implementation is returned, so a caller can require a refusal instead
// of hoping the corpus disagrees with a defect.
func compareMessageProtectionVector(t *testing.T, raw json.RawMessage) (messageProtectionComparison, error) {
	t.Helper()
	vector := messageProtectionVector{}
	if err := json.Unmarshal(raw, &vector); err != nil {
		t.Fatalf("parse a %s case: %v", messageProtectionKatFile, err)
	}
	suite, ok := implementedSuite(vector.CipherSuite)
	if !ok {
		return messageProtectionComparison{}, nil
	}
	crypto, err := NewCryptoProvider(suite)
	if err != nil {
		t.Fatalf("NewCryptoProvider(%#04x): %v", uint16(suite), err)
	}

	groupId := MustHex(t, vector.GroupId)
	evidence := messageProtectionComparison{
		inScope:      true,
		hashSize:     crypto.HashSize(),
		groupContext: vector.theGroupContextOf(t, suite),
		independentContext: independentGroupContext(t, vector.CipherSuite, groupId, vector.Epoch,
			MustHex(t, vector.TreeHash), MustHex(t, vector.ConfirmedTranscriptHash)),
	}
	membershipKey := MustHex(t, vector.MembershipKey)
	senderDataSecret := MustHex(t, vector.SenderDataSecret)
	encryptionSecret := MustHex(t, vector.EncryptionSecret)
	resolve := StaticSignatureKey(SignaturePublicKey(MustHex(t, vector.SignaturePub)))

	// the two public columns. Each opens against the membership key and the signature key, its
	// raw value is recovered by re-encoding the arm its content type names, and the opened
	// content is sealed again and held BYTE EXACT against the column it came out of.
	for _, column := range []struct {
		name string
		// protected is the published MLSMessage, and rawField the json key of the value it
		// carries.
		protected []byte
		rawField  string
		raw       []byte
	}{
		{"proposal_pub", MustHex(t, vector.ProposalPub), "proposal", MustHex(t, vector.Proposal)},
		{"commit_pub", MustHex(t, vector.CommitPub), "commit", MustHex(t, vector.Commit)},
	} {
		message, err := ParseMLSMessage(column.protected)
		if err != nil {
			return evidence, fmt.Errorf("%w: %s: %w", errMessageProtectionShape, column.name, err)
		}
		if message.WireFormat != WireFormatPublicMessage || message.PublicMessage == nil {
			return evidence, fmt.Errorf("%w: %s carries wire format %#04x", errMessageProtectionShape,
				column.name, uint16(message.WireFormat))
		}
		opened, err := OpenPublicMessage(crypto, membershipKey, message.PublicMessage, resolve,
			evidence.groupContext)
		if err != nil {
			return evidence, fmt.Errorf("%w: %s: %w", errMessageProtectionUnprotect, column.name, err)
		}
		if err := messageProtectionContextOf(opened, groupId, vector.Epoch); err != nil {
			return evidence, fmt.Errorf("%w: %s: %w", errMessageProtectionContext, column.name, err)
		}
		evidence.contextsChecked++
		recovered, err := messageProtectionRawValueOf(opened)
		if err != nil {
			return evidence, fmt.Errorf("%w: %s: %w", errMessageProtectionShape, column.name, err)
		}
		evidence.checks = append(evidence.checks, messageProtectionCheck{
			name: column.name + "/content", field: column.rawField, got: recovered, want: column.raw,
		})

		// the re-seal, byte exact against the published column. A PublicMessage carries no
		// randomness, so this is the encoder held against foreign octets rather than against
		// its own.
		resealed, err := SealPublicMessage(crypto, membershipKey, opened, evidence.groupContext)
		if err != nil {
			return evidence, fmt.Errorf("%w: %s: reseal: %w", errMessageProtectionRoundTrip, column.name, err)
		}
		encoded, err := MarshalMLSMessage(&MLSMessage{
			Version:       ProtocolVersionMls10,
			WireFormat:    WireFormatPublicMessage,
			PublicMessage: resealed,
		})
		if err != nil {
			return evidence, fmt.Errorf("%w: %s: re-encode: %w", errMessageProtectionRoundTrip, column.name, err)
		}
		evidence.checks = append(evidence.checks, messageProtectionCheck{
			name: column.name + "/reseal", field: column.name, got: encoded, want: column.protected,
		})
	}

	// section 6's exception, which no published answer can express: this case's own application
	// content must be REFUSED on the PublicMessage path. The content is signed first, so what is
	// being refused is a message that is otherwise entirely well formed.
	applicationAuth, err := SignAuthenticatedContent(crypto, SignaturePrivateKey(MustHex(t, vector.SignaturePriv)),
		WireFormatPublicMessage, &FramedContent{
			GroupId:         groupId,
			Epoch:           vector.Epoch,
			Sender:          Sender{SenderType: SenderTypeMember, LeafIndex: messageProtectionSenderLeaf},
			ContentType:     ContentTypeApplication,
			ApplicationData: MustHex(t, vector.Application),
		}, evidence.groupContext)
	if err != nil {
		return evidence, fmt.Errorf("%w: signing this case's application content: %w",
			errMessageProtectionApplicationPub, err)
	}
	_, evidence.applicationAsPublic = SealPublicMessage(crypto, membershipKey, applicationAuth, evidence.groupContext)

	// the three private columns. Each opens against a secret tree built for this case's member
	// count, and is then resealed through a second tree and reopened through a third: a
	// PrivateMessage cannot be reproduced byte for byte, so what is held is the authenticated
	// content coming back unchanged and the two seals of it differing.
	for _, column := range []struct {
		name      string
		protected []byte
		rawField  string
		raw       []byte
	}{
		{"proposal_priv", MustHex(t, vector.ProposalPriv), "proposal", MustHex(t, vector.Proposal)},
		{"commit_priv", MustHex(t, vector.CommitPriv), "commit", MustHex(t, vector.Commit)},
		{"application_priv", MustHex(t, vector.ApplicationPriv), "application", MustHex(t, vector.Application)},
	} {
		message, err := ParseMLSMessage(column.protected)
		if err != nil {
			return evidence, fmt.Errorf("%w: %s: %w", errMessageProtectionShape, column.name, err)
		}
		if message.WireFormat != WireFormatPrivateMessage || message.PrivateMessage == nil {
			return evidence, fmt.Errorf("%w: %s carries wire format %#04x", errMessageProtectionShape,
				column.name, uint16(message.WireFormat))
		}
		// a fresh secret tree per column: each published message is at generation 0 of its own
		// ratchet, and a tree that had already answered for one of them would have consumed it.
		openTree, err := messageProtectionSecretTree(t, crypto, encryptionSecret)
		if err != nil {
			return evidence, err
		}
		opened, err := OpenPrivateMessage(crypto, openTree, senderDataSecret, message.PrivateMessage,
			resolve, evidence.groupContext)
		if err != nil {
			return evidence, fmt.Errorf("%w: %s: %w", errMessageProtectionUnprotect, column.name, err)
		}
		if err := messageProtectionContextOf(opened, groupId, vector.Epoch); err != nil {
			return evidence, fmt.Errorf("%w: %s: %w", errMessageProtectionContext, column.name, err)
		}
		evidence.contextsChecked++
		recovered, err := messageProtectionRawValueOf(opened)
		if err != nil {
			return evidence, fmt.Errorf("%w: %s: %w", errMessageProtectionShape, column.name, err)
		}
		evidence.checks = append(evidence.checks, messageProtectionCheck{
			name: column.name + "/content", field: column.rawField, got: recovered, want: column.raw,
		})

		first, firstCiphertext, err := messageProtectionReseal(t, crypto, encryptionSecret, senderDataSecret, opened)
		if err != nil {
			return evidence, fmt.Errorf("%w: %s: %w", errMessageProtectionRoundTrip, column.name, err)
		}
		_, secondCiphertext, err := messageProtectionReseal(t, crypto, encryptionSecret, senderDataSecret, opened)
		if err != nil {
			return evidence, fmt.Errorf("%w: %s: %w", errMessageProtectionRoundTrip, column.name, err)
		}
		// the reuse guard, read off the CONTENT ciphertext. Two seals of one content through two
		// identically seeded trees take the same key and the same base nonce at generation 0, so
		// the two ciphertexts are equal exactly when section 6.3.2's four fresh octets did not
		// reach the nonce -- which is a nonce reused across two messages of one generation. The
		// whole messages differ either way, because the guard is also carried inside the
		// encrypted sender data, so comparing those would pass over a guard applied to nothing.
		if len(firstCiphertext) == 0 || len(secondCiphertext) == 0 {
			return evidence, fmt.Errorf("%w: %s: a reseal produced %d and %d octets of content ciphertext",
				errMessageProtectionRoundTrip, column.name, len(firstCiphertext), len(secondCiphertext))
		}
		if bytes.Equal(firstCiphertext, secondCiphertext) {
			return evidence, fmt.Errorf("%w: %s", errMessageProtectionReuseGuardStuck, column.name)
		}
		evidence.resealsDiffer++

		reopenTree, err := messageProtectionSecretTree(t, crypto, encryptionSecret)
		if err != nil {
			return evidence, err
		}
		resealedMessage, err := ParseMLSMessage(first)
		if err != nil {
			return evidence, fmt.Errorf("%w: %s: re-parse: %w", errMessageProtectionRoundTrip, column.name, err)
		}
		if resealedMessage.PrivateMessage == nil {
			return evidence, fmt.Errorf("%w: %s: the reseal is not a PrivateMessage",
				errMessageProtectionRoundTrip, column.name)
		}
		reopened, err := OpenPrivateMessage(crypto, reopenTree, senderDataSecret,
			resealedMessage.PrivateMessage, resolve, evidence.groupContext)
		if err != nil {
			return evidence, fmt.Errorf("%w: %s: reopen: %w", errMessageProtectionRoundTrip, column.name, err)
		}
		// the whole authenticated content and not the payload alone, so a round trip that lost
		// the signature, the sender or the authenticated data is a failure here rather than a
		// payload that happens to match.
		before, err := syntax.Marshal(opened)
		if err != nil {
			return evidence, fmt.Errorf("%w: %s: %w", errMessageProtectionRoundTrip, column.name, err)
		}
		after, err := syntax.Marshal(reopened)
		if err != nil {
			return evidence, fmt.Errorf("%w: %s: %w", errMessageProtectionRoundTrip, column.name, err)
		}
		if !bytes.Equal(before, after) {
			return evidence, fmt.Errorf("%w: %s came back as %s, want %s",
				errMessageProtectionRoundTrip, column.name, HexOf(after), HexOf(before))
		}
		evidence.roundTrips++
	}

	return evidence, evidence.verdict()
}

// messageProtectionSecretTree is one generation-0 secret tree for the group appendix A describes.
//
// A helper rather than four call sites, because the member count is the parameter a wrong value
// of makes every private column fail to open with an error that names a key rather than a group
// size.
func messageProtectionSecretTree(t *testing.T, crypto CryptoProvider, encryptionSecret []byte) (*SecretTree, error) {
	t.Helper()
	tree, err := NewSecretTree(crypto, messageProtectionLeafCount, encryptionSecret)
	if err != nil {
		return nil, fmt.Errorf("%w: secret tree for %d members: %w", errMessageProtectionUnprotect,
			messageProtectionLeafCount, err)
	}
	return tree, nil
}

// messageProtectionReseal protects one already authenticated content as a PrivateMessage through
// a secret tree of its own, and returns the serialized MLSMessage together with the content
// ciphertext inside it.
//
// Both, because the two callers ask different questions of one seal: the round trip is over the
// serialized message, which is what a peer receives, and the reuse guard is over the content
// ciphertext alone, which is the only part of the message the guard changes.
func messageProtectionReseal(t *testing.T, crypto CryptoProvider, encryptionSecret []byte,
	senderDataSecret []byte, authContent *AuthenticatedContent) ([]byte, []byte, error) {

	t.Helper()
	tree, err := messageProtectionSecretTree(t, crypto, encryptionSecret)
	if err != nil {
		return nil, nil, err
	}
	sealed, err := SealPrivateMessage(crypto, tree, senderDataSecret, authContent, PaddingSizeV1)
	if err != nil {
		return nil, nil, err
	}
	encoded, err := MarshalMLSMessage(&MLSMessage{
		Version:        ProtocolVersionMls10,
		WireFormat:     WireFormatPrivateMessage,
		PrivateMessage: sealed,
	})
	if err != nil {
		return nil, nil, err
	}
	return encoded, bytes.Clone(sealed.Ciphertext), nil
}

// messageProtectionContextOf holds one unprotected message to the group, the epoch and the sender
// leaf the case publishes.
//
// CheckFramedContentContext is the production rule and is called rather than reimplemented: a
// harness that compared the group id itself would be a second copy of ValSem002 and ValSem003 in
// a file that does not own them, and the constant time comparison guardrail 8 asks for lives
// inside the production one.
func messageProtectionContextOf(opened *AuthenticatedContent, groupId []byte, epoch uint64) error {
	if err := CheckFramedContentContext(&opened.Content, groupId, epoch); err != nil {
		return err
	}
	if opened.Content.Sender.SenderType != SenderTypeMember {
		return fmt.Errorf("sender type %d, want a member", opened.Content.Sender.SenderType)
	}
	if opened.Content.Sender.LeafIndex != messageProtectionSenderLeaf {
		return fmt.Errorf("sender leaf %d, want %d", opened.Content.Sender.LeafIndex,
			messageProtectionSenderLeaf)
	}
	return nil
}

// messageProtectionRawValueOf is the published raw value one unprotected message carries: the
// re-encoded proposal, the re-encoded commit, or the application payload as it stands.
//
// The arm is selected by the content type the message itself declares, so a message whose arm
// disagrees with its discriminant is a refusal here rather than a nil dereference.
func messageProtectionRawValueOf(opened *AuthenticatedContent) ([]byte, error) {
	switch opened.Content.ContentType {
	case ContentTypeProposal:
		if opened.Content.Proposal == nil {
			return nil, errors.New("the content type names a proposal and the proposal arm is empty")
		}
		return syntax.Marshal(opened.Content.Proposal)
	case ContentTypeCommit:
		if opened.Content.Commit == nil {
			return nil, errors.New("the content type names a commit and the commit arm is empty")
		}
		return syntax.Marshal(opened.Content.Commit)
	case ContentTypeApplication:
		return bytes.Clone(opened.Content.ApplicationData), nil
	}
	return nil, fmt.Errorf("content type %d is not one of the three section 6 registers",
		opened.Content.ContentType)
}

// TestVectorMessageProtection is vector family 4 over the published corpus.
//
// Every assertion the tally makes after the loop exists because the loop can be made to run zero
// times without anything else in this package noticing. A filter that matched nothing, a filter
// that matched all seven published suites, a corpus that parsed to an empty array, a comparator
// that declined every case: each of those is a green run of this test with the accounting
// removed, and a failure with it.
//
// What the loop counts is not calls that returned. It counts comparisons whose recovered half
// this runner itself re-checked against a GENERIC decode of the corpus text -- no struct tag in
// the way.
//
// That second reading is REDUNDANT with the comparator's own verdict, and it is recorded here as
// redundant rather than stated as a second guarantee, the way family 12 records the same construct
// on messagesCheck in messages_kat_test.go. MEASURED: replacing it with the comparator's own copy
// of the published answer -- `want := HexOf(check.want)`, so the runner compares the corpus
// against itself and opens the file a second time for nothing -- leaves the whole of ./mls/... and
// ./message/... at 6511 passing and 0 failing, which is the baseline exactly. A comparator that
// answered without opening anything is a failure in verdict(), which compares every recovered half
// against the published one and requires every check to be non-empty; it is not this loop that
// catches that, and the wording here used to say it was.
//
// What the second reading is FOR is the one thing that comparison cannot reach: a check whose
// FIELD does not address the answer it carries, or names a column the corpus does not publish at
// all. Both are silent on the comparator's side, because the field a check names and the answer it
// carries come off one row of one table and cannot disagree there. The two readings are redundant
// only while that stays true, which is why this one is kept rather than deleted.
func TestVectorMessageProtection(t *testing.T) {
	tally, entries := newVectorRunTally(t, messageProtectionKatFile)
	for index, raw := range entries {
		published := map[string]json.RawMessage{}
		if err := json.Unmarshal(raw, &published); err != nil {
			t.Fatalf("%s case %d: %v", messageProtectionKatFile, index, err)
		}
		header := vectorCaseHeader{}
		if err := json.Unmarshal(raw, &header); err != nil {
			t.Fatalf("%s case %d: %v", messageProtectionKatFile, index, err)
		}
		suite, inScope := tally.filter(header.CipherSuite)
		if !inScope {
			continue
		}
		evidence, err := compareMessageProtectionVector(t, raw)
		if err != nil {
			t.Fatalf("%s case %d (suite %#04x): %v", messageProtectionKatFile, index, header.CipherSuite, err)
		}
		tally.requireCompared(t, index, suite, evidence.inScope)
		if err := evidence.verdict(); err != nil {
			t.Fatalf("%s case %d (suite %#04x): %v", messageProtectionKatFile, index, header.CipherSuite, err)
		}
		for _, check := range evidence.checks {
			want := publishedCorpusField(t, published, check.field)
			if got := HexOf(check.got); got != want {
				t.Fatalf("%s case %d (suite %#04x): %s answered %s, the corpus publishes %s for %s",
					messageProtectionKatFile, index, header.CipherSuite, check.name, got, want, check.field)
			}
			tally.answer(want)
		}
	}
	tally.assertRun(t, messageProtectionCovered, messageProtectionSkipped,
		messageProtectionComparisons, messageProtectionDistinct)
}

// TestMessageProtectionFamilyIsInstalled is the registration half of task 17.
//
// The generator is asserted as a presence, and the installed verifier is DRIVEN over a published
// case and over a corrupted one, by assertVectorFamilyIsInstalled. Pointer identity says the
// manifest holds this function; it says nothing about the function doing anything.
func TestMessageProtectionFamilyIsInstalled(t *testing.T) {
	assertVectorFamilyIsInstalled(t, 4, messageProtectionKatFile,
		verifyMessageProtectionVector, generateMessageProtectionVector)
}

// TestMessageProtectionChecksAreEveryProtectedColumnTheCorpusPublishes derives the class of
// answers this family owes from the corpus's own columns and holds the check names to it.
//
// This is guardrail 5 applied to a vector family. A hand written list of seven names understates
// itself the moment a corpus update adds an eighth column, and the run then reports fourteen
// comparisons over a corpus that publishes sixteen answers. The class is read off the vector
// struct's json tags: a column ending _pub or _priv is a protected message and owes a comparison
// against the raw column whose name is that name with the suffix removed, and a _pub column owes
// a byte exact re-seal on top of it. The struct is in turn held to the corpus by
// TestMessageProtectionVectorDecodesEveryColumnTheCorpusPublishes.
func TestMessageProtectionChecksAreEveryProtectedColumnTheCorpusPublishes(t *testing.T) {
	public, private := messageProtectionProtectedColumns(t)
	if len(public) == 0 || len(private) == 0 {
		t.Fatalf("%v holds %d public and %d private protected columns; this derivation read nothing",
			messageProtectionColumnTags(t), len(public), len(private))
	}
	if len(public) != messageProtectionPublicColumns {
		t.Fatalf("the corpus publishes the public columns %v and this family counts %d",
			public, messageProtectionPublicColumns)
	}
	if len(private) != messageProtectionPrivateColumns {
		t.Fatalf("the corpus publishes the private columns %v and this family owes %d round trips",
			private, messageProtectionPrivateColumns)
	}
	owed := []string{}
	for _, tag := range append(slices.Clone(public), private...) {
		owed = append(owed, tag+"/content")
	}
	for _, tag := range public {
		owed = append(owed, tag+"/reseal")
	}
	if !slices.Equal(slices.Sorted(slices.Values(owed)), slices.Sorted(slices.Values(messageProtectionCheckNames))) {
		t.Fatalf("the corpus's columns owe %v and this family compares %v", owed, messageProtectionCheckNames)
	}
	if len(messageProtectionCheckNames) != messageProtectionChecks {
		t.Fatalf("this family names %d checks per case and the count it asserts is %d",
			len(messageProtectionCheckNames), messageProtectionChecks)
	}
	if messageProtectionComparisons != messageProtectionCovered*messageProtectionChecks {
		t.Fatalf("%d comparisons over %d covered cases at %d checks each does not add up",
			messageProtectionComparisons, messageProtectionCovered, messageProtectionChecks)
	}
}

// TestMessageProtectionVectorDecodesEveryColumnTheCorpusPublishes holds the struct above to the
// corpus, in both directions.
//
// A field whose json tag names a key the corpus does not publish decodes to the empty string and
// every comparison over it would be vacuous; a key the corpus publishes and the struct does not
// name is a column this family never looks at. Both are read off a generic decode of the file
// rather than off the struct, so neither can be true and unnoticed.
func TestMessageProtectionVectorDecodesEveryColumnTheCorpusPublishes(t *testing.T) {
	declared := map[string]bool{}
	for _, tag := range messageProtectionColumnTags(t) {
		declared[tag] = true
	}
	if len(declared) == 0 {
		t.Fatal("messageProtectionVector declares no json key at all, so this gate read nothing")
	}
	// the other declaration of this corpus row, held against this one field by field. Two
	// structs decoding one file is how the two come apart about which key an answer lives at,
	// and the disagreement is silent in the worst direction: the second spelling decodes to the
	// empty string and whatever compares it compares nothing.
	narrower := reflect.TypeOf(messageProtectionKatEntry{})
	if narrower.NumField() == 0 {
		t.Fatal("messageProtectionKatEntry declares no field, so this cross check read nothing")
	}
	for index := 0; index < narrower.NumField(); index++ {
		field := narrower.Field(index)
		key, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if !declared[key] {
			t.Errorf("messageProtectionKatEntry decodes %q and messageProtectionVector names no field for it", key)
			continue
		}
		mine, found := reflect.TypeOf(messageProtectionVector{}).FieldByName(field.Name)
		if !found {
			t.Errorf("messageProtectionKatEntry.%s decodes %q and messageProtectionVector spells that key under another field name",
				field.Name, key)
			continue
		}
		if mineKey, _, _ := strings.Cut(mine.Tag.Get("json"), ","); mineKey != key {
			t.Errorf("messageProtectionKatEntry.%s decodes %q and messageProtectionVector.%s decodes %q",
				field.Name, key, mine.Name, mineKey)
		}
	}
	cases := 0
	for index, raw := range LoadVectorFile(t, messageProtectionKatFile) {
		published := map[string]json.RawMessage{}
		if err := json.Unmarshal(raw, &published); err != nil {
			t.Fatalf("%s case %d: %v", messageProtectionKatFile, index, err)
		}
		for key := range published {
			if !declared[key] {
				t.Errorf("%s case %d publishes %q and messageProtectionVector names no field for it",
					messageProtectionKatFile, index, key)
			}
		}
		for key := range declared {
			if _, found := published[key]; !found {
				t.Errorf("messageProtectionVector decodes %q and %s case %d does not publish it",
					key, messageProtectionKatFile, index)
			}
		}
		cases++
	}
	if cases == 0 {
		t.Fatalf("%s holds no case, so this gate read nothing", messageProtectionKatFile)
	}
}

// TestMessageProtectionSenderIsTheLeafEveryPublishedMessageNames holds the corpus to the sender
// this runner assumes, read out of the messages themselves.
//
// The leaf and the member count are inputs to the derivation of every private column's key, so a
// corpus update that moved the sender would turn this family into five decryption failures whose
// error text names an AEAD and not a group. This says which.
func TestMessageProtectionSenderIsTheLeafEveryPublishedMessageNames(t *testing.T) {
	senders := 0
	for index, raw := range LoadVectorFile(t, messageProtectionKatFile) {
		vector := messageProtectionVector{}
		if err := json.Unmarshal(raw, &vector); err != nil {
			t.Fatalf("%s case %d: %v", messageProtectionKatFile, index, err)
		}
		suite, ok := implementedSuite(vector.CipherSuite)
		if !ok {
			continue
		}
		crypto, err := NewCryptoProvider(suite)
		if err != nil {
			t.Fatalf("NewCryptoProvider(%#04x): %v", vector.CipherSuite, err)
		}
		// the public columns carry their sender in the clear, which is the half that can be
		// read without the group's secrets.
		for _, column := range []struct {
			name      string
			protected string
		}{
			{"proposal_pub", vector.ProposalPub},
			{"commit_pub", vector.CommitPub},
		} {
			message, err := ParseMLSMessage(MustHex(t, column.protected))
			if err != nil {
				t.Fatalf("%s case %d: %s: %v", messageProtectionKatFile, index, column.name, err)
			}
			if message.PublicMessage == nil {
				t.Fatalf("%s case %d: %s is not a PublicMessage", messageProtectionKatFile, index, column.name)
			}
			sender := message.PublicMessage.Content.Sender
			if sender.SenderType != SenderTypeMember || sender.LeafIndex != messageProtectionSenderLeaf {
				t.Fatalf("%s case %d: %s names sender %+v and this runner builds a %d member tree for leaf %d",
					messageProtectionKatFile, index, column.name, sender,
					messageProtectionLeafCount, messageProtectionSenderLeaf)
			}
			senders++
		}
		if crypto.HashSize() == 0 {
			t.Fatalf("%s case %d: the provider answers a zero KDF.Nh", messageProtectionKatFile, index)
		}
	}
	if senders != messageProtectionCovered*messageProtectionPublicColumns {
		t.Fatalf("read %d senders out of the covered cases, want %d",
			senders, messageProtectionCovered*messageProtectionPublicColumns)
	}
}

// mpKatBaseCase answers a published case at a registered suite, together with the encoder the
// controls below corrupt it through.
//
// The base is the corpus's own, not a fixture: the whole of what the refusals below mean is that
// this exact case is accepted and a one octet edit of it is not.
func mpKatBaseCase(t *testing.T) (messageProtectionVector, func(messageProtectionVector) json.RawMessage) {
	t.Helper()
	base := messageProtectionVector{}
	found := false
	for _, raw := range LoadVectorFile(t, messageProtectionKatFile) {
		candidate := messageProtectionVector{}
		if err := json.Unmarshal(raw, &candidate); err != nil {
			t.Fatalf("parse a %s case: %v", messageProtectionKatFile, err)
		}
		if _, ok := implementedSuite(candidate.CipherSuite); ok {
			base, found = candidate, true
			break
		}
	}
	if !found {
		t.Fatal("no published case is at a registered suite, so this control has nothing to corrupt")
	}
	encode := func(entry messageProtectionVector) json.RawMessage {
		body, err := json.Marshal(entry)
		if err != nil {
			t.Fatalf("marshal the case under test: %v", err)
		}
		return body
	}
	return base, encode
}

// mpFlipHex is one published hex string with its first octet changed, for the controls below.
func mpFlipHex(t *testing.T, text string) string {
	t.Helper()
	octets := MustHex(t, text)
	if len(octets) == 0 {
		t.Fatalf("nothing to flip in %q", text)
	}
	octets[0] ^= 0x01
	return HexOf(octets)
}

// TestCompareMessageProtectionVectorRefusesACaseItShouldNotAccept is the control the runner
// cannot be: it hands the comparator cases that are wrong in each of the ways the corpus is not,
// and requires the matching refusal.
//
// Each row is a real defect class of this family -- a key the messages were not protected under,
// a group context the signatures do not cover, a published raw value the messages do not carry --
// and each names the sentinel it owes, so a refusal for the wrong reason is a failure too.
//
// Two obligations this family holds are NOT rows here, and saying so is the point of this comment
// rather than an omission. The byte exact re-seal and the group context second opinion are both
// functions of this package alone: the resealed octets are derived from the opened message and
// the two serializations read the same published fields, so no edit to a case can make either
// disagree while leaving the open to succeed. They are covered by mutation instead -- transposing
// two writes in GroupContext.MarshalMLS, and dropping the membership tag from
// PublicMessage.MarshalMLS -- and both are caught by TestVectorMessageProtection.
func TestCompareMessageProtectionVectorRefusesACaseItShouldNotAccept(t *testing.T) {
	base, encode := mpKatBaseCase(t)

	// a case at a suite this package does not register is DECLINED and is not a refusal for a
	// defect: the comparator has no provider for it. It is refused by refuseMessageProtectionVector
	// so that the control below cannot be satisfied by a comparator that ran nothing, and the
	// distinction is asserted here rather than assumed.
	outOfScope := base
	outOfScope.CipherSuite = 2
	if _, ok := implementedSuite(outOfScope.CipherSuite); ok {
		t.Fatal("suite 0x0002 is registered, so the out of scope case below is not out of scope")
	}
	declined, err := compareMessageProtectionVector(t, encode(outOfScope))
	if err != nil {
		t.Fatalf("a case at an unimplemented suite was reported as a defect: %v", err)
	}
	if declined.inScope || len(declined.checks) != 0 {
		t.Fatalf("a case at an unimplemented suite came back inScope=%v with %d comparisons",
			declined.inScope, len(declined.checks))
	}

	rows := []struct {
		name string
		edit func(*messageProtectionVector)
		want error
	}{
		{"a membership key the public columns were not tagged under", func(v *messageProtectionVector) {
			v.MembershipKey = mpFlipHex(t, v.MembershipKey)
		}, errMessageProtectionUnprotect},
		{"a signature key the messages were not signed under", func(v *messageProtectionVector) {
			v.SignaturePub = mpFlipHex(t, v.SignaturePub)
		}, errMessageProtectionUnprotect},
		{"a sender data secret the private headers were not sealed under", func(v *messageProtectionVector) {
			v.SenderDataSecret = mpFlipHex(t, v.SenderDataSecret)
		}, errMessageProtectionUnprotect},
		{"an encryption secret the private ratchets do not come from", func(v *messageProtectionVector) {
			v.EncryptionSecret = mpFlipHex(t, v.EncryptionSecret)
		}, errMessageProtectionUnprotect},
		{"a tree hash the group context does not carry", func(v *messageProtectionVector) {
			v.TreeHash = mpFlipHex(t, v.TreeHash)
		}, errMessageProtectionUnprotect},
		{"an epoch the messages were not protected in", func(v *messageProtectionVector) {
			v.Epoch += 1
		}, errMessageProtectionUnprotect},
		{"a group id the messages do not name", func(v *messageProtectionVector) {
			v.GroupId = mpFlipHex(t, v.GroupId)
		}, errMessageProtectionUnprotect},
		{"a published proposal the protected columns do not carry", func(v *messageProtectionVector) {
			v.Proposal = mpFlipHex(t, v.Proposal)
		}, errMessageProtectionMismatch},
		{"a published commit the protected columns do not carry", func(v *messageProtectionVector) {
			v.Commit = mpFlipHex(t, v.Commit)
		}, errMessageProtectionMismatch},
		{"a published application payload the private column does not carry", func(v *messageProtectionVector) {
			v.Application = mpFlipHex(t, v.Application)
		}, errMessageProtectionMismatch},
		{"a published proposal_pub whose octets are not this encoder's", func(v *messageProtectionVector) {
			v.ProposalPub = mpFlipHex(t, v.ProposalPub)
		}, errMessageProtectionShape},
	}
	refusals := []comparatorRefusal{}
	for _, row := range rows {
		corrupted := base
		row.edit(&corrupted)
		refusals = append(refusals, comparatorRefusal{row.name, encode(corrupted), row.want})
	}
	assertComparatorRefuses(t, "message-protection", refuseMessageProtectionVector, encode(base), refusals)
}

// theGroupContextParameters is every production entry point of this package that takes a
// SERIALIZED GROUP CONTEXT, and the argument position it takes it in, read off the type checked
// package rather than listed.
//
// Derived because the class is the thing under test. "The two functions family 4's generator
// seals with" is a list, and a list covers the call sites somebody remembered: a third production
// entry point taking a group context -- and this derivation finds more than two -- called from the
// generator would be a message protected over whatever it was handed, with nothing asking where
// those octets came from. The join is the PARAMETER NAME, which production spells one way
// everywhere, and the gate below fails outright if the derivation finds none.
//
// Methods as well as package level functions. A group context reaches production through whatever
// names it, and a derivation reading the package scope alone would exempt every method the day one
// takes one.
func theGroupContextParameters(t *testing.T) map[string]int {
	t.Helper()
	positions := map[string]int{}
	take := func(name string, signature *types.Signature) {
		params := signature.Params()
		for index := 0; index < params.Len(); index++ {
			if params.At(index).Name() == "groupContext" {
				positions[name] = index
				return
			}
		}
	}
	scope := typeCheckedPackage(t).Scope()
	for _, name := range scope.Names() {
		switch found := scope.Lookup(name).(type) {
		case *types.Func:
			if signature, isSignature := found.Type().(*types.Signature); isSignature {
				take(found.Name(), signature)
			}
		case *types.TypeName:
			named, isNamed := found.Type().(*types.Named)
			if !isNamed {
				continue
			}
			for index := 0; index < named.NumMethods(); index++ {
				method := named.Method(index)
				if signature, isSignature := method.Type().(*types.Signature); isSignature {
					take(method.Name(), signature)
				}
			}
		}
	}
	return positions
}

// aGroupContextSite is one call a generator makes into production in the group context position:
// the callee, the function whose call produced the octets handed to it, and how that argument was
// written, so a failure names the shape it could not resolve rather than reporting a bare no.
type aGroupContextSite struct {
	callee   string
	origin   string
	spelling string
}

// theCalleeNameOf is the bare name a call expression invokes, which is how a call reaches a
// production function whether it is written plain or through a qualifier.
func theCalleeNameOf(callee ast.Expr) string {
	switch named := callee.(type) {
	case *ast.Ident:
		return named.Name
	case *ast.SelectorExpr:
		return named.Sel.Name
	}
	return ""
}

// theOriginOfArgument names the function whose call produced one argument, following a local name
// back through the single assignment that wrote it and no further.
//
// ONE hop and not a fixed point. One hop is the shape being judged -- a context built on one line
// and passed on the next -- and a walk that chased names indefinitely would have to answer what to
// do about a name assigned from itself. A second hop resolves to no origin, which the gate below
// reads as a failure and never as a pass.
func theOriginOfArgument(argument ast.Expr, assigned map[string][]ast.Expr) (origin string, spelling string) {
	switch expression := argument.(type) {
	case *ast.CallExpr:
		if name := theCalleeNameOf(expression.Fun); name != "" {
			return name, name + "(...)"
		}
		return "", "a call whose callee this walk cannot name"
	case *ast.Ident:
		wrote := assigned[expression.Name]
		if len(wrote) != 1 {
			return "", fmt.Sprintf("%s, which this body assigns %d times", expression.Name, len(wrote))
		}
		call, isCall := wrote[0].(*ast.CallExpr)
		if !isCall {
			return "", fmt.Sprintf("%s, which this body assigns from something that is not a call", expression.Name)
		}
		if name := theCalleeNameOf(call.Fun); name != "" {
			return name, fmt.Sprintf("%s, assigned from %s(...)", expression.Name, name)
		}
		return "", fmt.Sprintf("%s, assigned from a call this walk cannot name", expression.Name)
	}
	return "", "an expression this walk cannot resolve to a call"
}

// theGroupContextSitesOf resolves, for every call one function body makes to a production entry
// point that takes a group context, which function PRODUCED the octets passed there.
//
// A CALL and not a mention, for theCallsReachableFrom's reason: a call replaced by a discard of the
// same identifier still names it, and a gate reading mentions is satisfied by the edit it exists to
// refuse.
//
// Two spellings resolve -- the argument is itself a call, or it is a local name this body assigns
// exactly once from one -- and everything else resolves to NO origin and is reported rather than
// accepted. The walk fails CLOSED on purpose: a refactor that moved the seal into a helper, or that
// assigned the context in two places, leaves the gate with an origin it cannot name, and that is a
// derivation to be redone rather than a pass to be collected.
func theGroupContextSitesOf(function *ast.FuncDecl, positions map[string]int) []aGroupContextSite {
	assigned := map[string][]ast.Expr{}
	ast.Inspect(function, func(node ast.Node) bool {
		assignment, isAssignment := node.(*ast.AssignStmt)
		if !isAssignment {
			return true
		}
		for index, target := range assignment.Lhs {
			name, isName := target.(*ast.Ident)
			if !isName || index >= len(assignment.Rhs) {
				continue
			}
			assigned[name.Name] = append(assigned[name.Name], assignment.Rhs[index])
		}
		return true
	})
	sites := []aGroupContextSite{}
	ast.Inspect(function, func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if !isCall {
			return true
		}
		callee := theCalleeNameOf(call.Fun)
		at, takes := positions[callee]
		if !takes || at >= len(call.Args) {
			return true
		}
		origin, spelling := theOriginOfArgument(call.Args[at], assigned)
		sites = append(sites, aGroupContextSite{callee: callee, origin: origin, spelling: spelling})
		return true
	})
	return sites
}

// TestTheMessageProtectionGeneratorProtectsOverAnIndependentGroupContext is the other half of the
// claim this family's generate direction makes about itself.
//
// The claim is that the generator protects its messages over a group context THIS FILE wrote out
// by hand while the consume direction verifies them over the codec's, so the two halves are two
// implementations of section 8.1 rather than one agreeing with itself. Nothing else in this file
// can see that: swap independentGroupContext for syntax.Marshal over the p4 struct and every
// generated case still verifies, every count still adds up, and the generate direction becomes a
// round trip through one serializer while continuing to report itself as a generate direction.
// Measured -- that edit leaves this file's other five tests green.
//
// What this gate asks, and why it is not the question the first version of it asked. That version
// asked whether the generator REACHED any function whose name claims independence, and the answer
// to that question is yes for a generator that reaches one and protects over another. MEASURED:
// the swap above, plus the single line `_ = independentOpaqueV(t, groupId)` -- and
// independentGroupContext calls independentOpaqueV, so that line is what a refactor which
// hand-wrote part of section 8.1 and took the codec for the rest looks like, not a contrivance --
// left the whole of ./mls/... and ./message/... at 6510 passing with the exact defect the
// paragraph above describes present in the source. Reaching a derivation and protecting over one
// are two claims, and only the second is the one this family makes.
//
// So the question is asked of the ARGUMENT. Every call the generator makes to a production entry
// point that takes a group context is found -- the class derived off the type checked package by
// the parameter's own name, so a third such entry point called from here is covered on the commit
// that declares it -- and the octets each one is handed must come from a function whose name claims
// independence AND which these test files declare. The class of derivations stays DERIVED for the
// reason it was before: a list names the ones that existed when it was written.
func TestTheMessageProtectionGeneratorProtectsOverAnIndependentGroupContext(t *testing.T) {
	// the controls, over sources written here, because a resolver that answered "independent" for
	// everything -- or that could not tell a mention from a call -- would pass this file while
	// reporting on nothing.
	control := func(source string) []string {
		fileSet := token.NewFileSet()
		parsed, err := parser.ParseFile(fileSet, "control_test.go", source, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse the control: %v", err)
		}
		declared := testFileFunctions(map[string]*ast.File{"control_test.go": parsed})
		generator, held := declared["generateControl"]
		if !held {
			t.Fatal("the control declares no generateControl, so it exercises nothing")
		}
		origins := []string{}
		for _, site := range theGroupContextSitesOf(generator, map[string]int{"sealControl": 1}) {
			origins = append(origins, site.origin)
		}
		return origins
	}
	for _, row := range []struct {
		name   string
		source string
		want   []string
	}{
		{"a context built by a hand derivation on the line above",
			"package control\n\nfunc generateControl() {\n\tcontext := independentControl()\n\tsealControl(nil, context)\n}\n",
			[]string{"independentControl"}},
		{"a context built by the codec beside a MENTION of a hand derivation",
			"package control\n\nfunc generateControl() {\n\tcontext := marshalControl()\n\t_ = independentControl\n\tsealControl(nil, context)\n}\n",
			[]string{"marshalControl"}},
		{"a hand derivation called inline in the argument",
			"package control\n\nfunc generateControl() {\n\tsealControl(nil, independentControl())\n}\n",
			[]string{"independentControl"}},
		{"a context this body never assigns, which must resolve to no origin at all",
			"package control\n\nfunc generateControl() {\n\tsealControl(nil, whateverThisIs)\n}\n",
			[]string{""}},
		{"a context assigned twice, which is a body this one hop walk must refuse to name",
			"package control\n\nfunc generateControl() {\n\tcontext := independentControl()\n\tcontext = marshalControl()\n\tsealControl(nil, context)\n}\n",
			[]string{""}},
		{"a call with no argument in the group context position",
			"package control\n\nfunc generateControl() {\n\tsealControl(nil)\n}\n",
			[]string{}},
	} {
		if got := control(row.source); !slices.Equal(got, row.want) {
			t.Fatalf("the resolver reads %s as %v, want %v", row.name, got, row.want)
		}
	}

	positions := theGroupContextParameters(t)
	if len(positions) == 0 {
		t.Fatal("no production declaration of this package takes a groupContext parameter, so every call site in the generator is invisible to this derivation and the gate below holds over nothing")
	}
	declared := testFileFunctions(packageTestFiles(t))
	// the generator the registry installs has to reach the case builder, or what is held below is
	// a function nothing runs.
	if !theCallsReachableFrom(t, declared, "generateMessageProtectionVector")["generateMessageProtectionCases"] {
		t.Fatal("generateMessageProtectionVector does not call generateMessageProtectionCases, so the derivation held below is not on the registered generator's path")
	}
	builder, held := declared["generateMessageProtectionCases"]
	if !held {
		t.Fatal("generateMessageProtectionCases is not declared in the test files scanned, so its call sites would be an empty set")
	}
	sites := theGroupContextSitesOf(builder, positions)
	if len(sites) == 0 {
		t.Fatalf("generateMessageProtectionCases calls none of the %d production declarations that take a group context, so it protects nothing over one and this gate would hold vacuously",
			len(positions))
	}
	for _, site := range sites {
		if !strings.HasPrefix(site.origin, "independent") {
			t.Errorf("generateMessageProtectionCases hands %s a group context from %s; family 4's generate direction then protects over the serializer its consume direction verifies over, and an encoder and a decoder wrong in the same direction agree perfectly",
				site.callee, site.spelling)
			continue
		}
		if _, ours := declared[site.origin]; !ours {
			t.Errorf("generateMessageProtectionCases hands %s a group context from %s, which these test files do not declare; a name that claims independence and lives in production is production",
				site.callee, site.spelling)
		}
	}
	t.Logf("generateMessageProtectionCases passes %d group contexts into production, every one of them from a derivation these test files declare, against the %d production declarations that take one",
		len(sites), len(positions))
}

// mpGeneratedApplication is the application payload a generated case at one suite carries.
//
// Distinct per suite, so a generator that emitted one payload for every suite would leave the
// distinct-answer count below unable to tell a run over two suites from a run over one twice.
func mpGeneratedApplication(suite CipherSuite) []byte {
	return []byte(fmt.Sprintf("urmessage generated application payload at suite %#04x", uint16(suite)))
}

// generateMessageProtectionCases builds one freshly protected case per registered ciphersuite, in
// the corpus's own json shape.
//
// The group context every message here is signed and tagged over is independentGroupContext's --
// RFC 9420 section 8.1 written out by hand -- and NOT the codec's, which is the whole of what
// makes this a generate direction rather than this package agreeing with itself. The consume
// direction rebuilds the context with syntax.Marshal over the p4 struct, so a codec that wrote
// the context differently from the RFC produces a corpus that this package cannot verify, which
// is the one defect a round trip through a single serializer can never see.
//
// wrongContext is the control's own defect: the messages are protected over a context whose tree
// hash differs from the one the case publishes, so a consume direction that verified nothing
// accepts it. Nothing else about the case changes.
func generateMessageProtectionCases(t *testing.T, wrongContext bool) []messageProtectionVector {
	t.Helper()
	generated := []messageProtectionVector{}
	for _, suite := range Suites() {
		crypto, err := NewCryptoProvider(suite)
		if err != nil {
			t.Fatalf("NewCryptoProvider(%#04x): %v", uint16(suite), err)
		}
		signPriv, signPub, err := crypto.SignatureKeyPair()
		if err != nil {
			t.Fatalf("suite %#04x: SignatureKeyPair: %v", uint16(suite), err)
		}
		groupId := crypto.Random(16)
		treeHash := crypto.Random(crypto.HashSize())
		confirmedTranscriptHash := crypto.Random(crypto.HashSize())
		encryptionSecret := crypto.Random(crypto.HashSize())
		senderDataSecret := crypto.Random(crypto.HashSize())
		membershipKey := crypto.Random(crypto.HashSize())
		epoch := uint64(3)

		vector := messageProtectionVector{
			CipherSuite:             uint16(suite),
			GroupId:                 HexOf(groupId),
			Epoch:                   epoch,
			TreeHash:                HexOf(treeHash),
			ConfirmedTranscriptHash: HexOf(confirmedTranscriptHash),
			SignaturePriv:           HexOf(signPriv),
			SignaturePub:            HexOf(signPub),
			EncryptionSecret:        HexOf(encryptionSecret),
			SenderDataSecret:        HexOf(senderDataSecret),
			MembershipKey:           HexOf(membershipKey),
			Application:             HexOf(mpGeneratedApplication(suite)),
		}

		protectedOver := treeHash
		if wrongContext {
			protectedOver = bytes.Clone(treeHash)
			protectedOver[0] ^= 0x01
		}
		groupContext := independentGroupContext(t, uint16(suite), groupId, epoch, protectedOver,
			confirmedTranscriptHash)

		// the raw values, encoded from structures this file built rather than recovered from
		// anything that was protected. The consume direction gets them back by unprotecting,
		// so the loop those two close is encode-then-decode and not decode-then-encode.
		proposal := &Proposal{ProposalType: ProposalTypeRemove, Remove: &Remove{Removed: 0}}
		proposalBytes, err := syntax.Marshal(proposal)
		if err != nil {
			t.Fatalf("suite %#04x: marshal the generated proposal: %v", uint16(suite), err)
		}
		vector.Proposal = HexOf(proposalBytes)
		commit := &Commit{}
		commitBytes, err := syntax.Marshal(commit)
		if err != nil {
			t.Fatalf("suite %#04x: marshal the generated commit: %v", uint16(suite), err)
		}
		vector.Commit = HexOf(commitBytes)

		sender := Sender{SenderType: SenderTypeMember, LeafIndex: messageProtectionSenderLeaf}
		confirmationTag := crypto.Random(crypto.HashSize())
		contents := []*FramedContent{
			{GroupId: groupId, Epoch: epoch, Sender: sender,
				ContentType: ContentTypeProposal, Proposal: proposal},
			{GroupId: groupId, Epoch: epoch, Sender: sender,
				ContentType: ContentTypeCommit, Commit: commit},
			{GroupId: groupId, Epoch: epoch, Sender: sender,
				ContentType: ContentTypeApplication, ApplicationData: mpGeneratedApplication(suite)},
		}

		sign := func(wireFormat WireFormat, content *FramedContent) *AuthenticatedContent {
			authContent, err := SignAuthenticatedContent(crypto, signPriv, wireFormat, content, groupContext)
			if err != nil {
				t.Fatalf("suite %#04x: sign a generated content: %v", uint16(suite), err)
			}
			// a commit that encoded without its confirmation tag is a message every peer
			// rejects at ValSem009 having verified its signature first, so the tag is set
			// before the content is ever written.
			if content.ContentType == ContentTypeCommit {
				authContent.Auth.ConfirmationTag = bytes.Clone(confirmationTag)
			}
			return authContent
		}

		for index, content := range contents[:2] {
			sealed, err := SealPublicMessage(crypto, membershipKey,
				sign(WireFormatPublicMessage, content), groupContext)
			if err != nil {
				t.Fatalf("suite %#04x: seal a generated PublicMessage: %v", uint16(suite), err)
			}
			encoded, err := MarshalMLSMessage(&MLSMessage{
				Version:       ProtocolVersionMls10,
				WireFormat:    WireFormatPublicMessage,
				PublicMessage: sealed,
			})
			if err != nil {
				t.Fatalf("suite %#04x: encode a generated PublicMessage: %v", uint16(suite), err)
			}
			if index == 0 {
				vector.ProposalPub = HexOf(encoded)
			} else {
				vector.CommitPub = HexOf(encoded)
			}
		}

		for index, content := range contents {
			tree, err := NewSecretTree(crypto, messageProtectionLeafCount, encryptionSecret)
			if err != nil {
				t.Fatalf("suite %#04x: secret tree: %v", uint16(suite), err)
			}
			sealed, err := SealPrivateMessage(crypto, tree, senderDataSecret,
				sign(WireFormatPrivateMessage, content), PaddingSizeV1)
			if err != nil {
				t.Fatalf("suite %#04x: seal a generated PrivateMessage: %v", uint16(suite), err)
			}
			encoded, err := MarshalMLSMessage(&MLSMessage{
				Version:        ProtocolVersionMls10,
				WireFormat:     WireFormatPrivateMessage,
				PrivateMessage: sealed,
			})
			if err != nil {
				t.Fatalf("suite %#04x: encode a generated PrivateMessage: %v", uint16(suite), err)
			}
			switch index {
			case 0:
				vector.ProposalPriv = HexOf(encoded)
			case 1:
				vector.CommitPriv = HexOf(encoded)
			case 2:
				vector.ApplicationPriv = HexOf(encoded)
			}
		}
		generated = append(generated, vector)
	}
	return generated
}

// generateMessageProtectionVector is the Generate half of family 4: freshly protected cases in
// the mlswg format, one per registered ciphersuite, for the registry to feed back through
// verifyMessageProtectionVector.
//
// An ARRAY and not one object. TestVectorGenerateThenVerify decodes what a generator answers as
// a json array of cases and fails on anything else, so a generator that answered a single case
// would take the whole generate direction out of the suite.
//
// One case PER REGISTERED SUITE and not one at a suite this file names. A generator pinned to a
// single code point leaves the other registered suite's providers unexercised in this direction,
// and the run still reports a generated case verified.
func generateMessageProtectionVector(t *testing.T) json.RawMessage {
	t.Helper()
	body, err := json.Marshal(generateMessageProtectionCases(t, false))
	if err != nil {
		t.Fatalf("marshal the generated %s cases: %v", messageProtectionKatFile, err)
	}
	return body
}

// TestVectorMessageProtectionGenerate is the generate direction of family 4.
//
// What it closes that verification alone cannot: a pinned vector never passes through our own
// encoder, so an encoder and a decoder that are wrong in the same direction verify perfectly.
// Generating a case and feeding it back sees that -- but only if the generator is not the
// verifier under another name, which is the trap this task's name states.
//
// Four things stand against this loop passing vacuously. The generated cases must cover every
// registered suite, so a generator pinned to one code point fails here. Every column the family
// compares must be present and non-empty, because a generator that emitted a shorter case would
// have the consume direction comparing an empty published answer against an empty recovered one
// and agreeing. The consume direction's full verdict must ACCEPT them, comparison count included.
// And a case protected over a group context that differs from the one it publishes must be
// REFUSED, so a consume direction that verified nothing fails here.
func TestVectorMessageProtectionGenerate(t *testing.T) {
	serialized := generateMessageProtectionVector(t)
	readBack := []messageProtectionVector{}
	if err := json.Unmarshal(serialized, &readBack); err != nil {
		t.Fatalf("the generated cases do not parse as an array: %v", err)
	}
	if len(readBack) != len(Suites()) {
		t.Fatalf("generated %d cases and this package registers %d suites", len(readBack), len(Suites()))
	}
	covered := []CipherSuite{}
	answers := map[string]bool{}
	comparisons := 0
	for _, vector := range readBack {
		suite, ok := implementedSuite(vector.CipherSuite)
		if !ok {
			t.Fatalf("generated a case at unimplemented suite %#04x", vector.CipherSuite)
		}
		covered = append(covered, suite)
		// every column the family reads, present and non-empty, read through the same
		// reflection that derives the check class.
		body, err := json.Marshal(vector)
		if err != nil {
			t.Fatalf("re-encode a generated case: %v", err)
		}
		published := map[string]json.RawMessage{}
		if err := json.Unmarshal(body, &published); err != nil {
			t.Fatalf("re-decode a generated case: %v", err)
		}
		for _, tag := range messageProtectionColumnTags(t) {
			raw, found := published[tag]
			if !found {
				t.Fatalf("a generated case at suite %#04x publishes no %s", vector.CipherSuite, tag)
			}
			text := ""
			if err := json.Unmarshal(raw, &text); err == nil && text == "" {
				t.Fatalf("a generated case at suite %#04x publishes an empty %s, and an empty comparison agrees with anything",
					vector.CipherSuite, tag)
			}
		}
		evidence, err := compareMessageProtectionVector(t, body)
		if err != nil {
			t.Fatalf("a generated case at suite %#04x was refused: %v", vector.CipherSuite, err)
		}
		if err := evidence.verdict(); err != nil {
			t.Fatalf("a generated case at suite %#04x did not verify: %v", vector.CipherSuite, err)
		}
		for _, check := range evidence.checks {
			answers[HexOf(check.want)] = true
			comparisons++
		}
	}
	if got := slices.Sorted(slices.Values(covered)); !slices.Equal(got, Suites()) {
		t.Fatalf("the generated cases cover %v and this package registers %v", got, Suites())
	}
	if want := len(readBack) * messageProtectionChecks; comparisons != want {
		t.Fatalf("the generated cases were compared %d times, want %d", comparisons, want)
	}
	// the raw proposal and the raw commit are the same structure at both suites, so the
	// distinct answers are those two plus a per suite application payload and a per suite pub
	// column. Asserted so that a generator emitting one case twice cannot report two.
	if want := 2 + 3*len(readBack); len(answers) != want {
		t.Fatalf("the generated cases were compared against %d distinct answers, want %d", len(answers), want)
	}

	// the control: a case protected over a group context it does not publish must be refused.
	for _, wrong := range generateMessageProtectionCases(t, true) {
		body, err := json.Marshal(wrong)
		if err != nil {
			t.Fatalf("re-encode the control case: %v", err)
		}
		err = refuseMessageProtectionVector(t, body)
		if err == nil {
			t.Fatalf("a case at suite %#04x protected over a group context it does not publish was accepted; the consume direction is not verifying over the context",
				wrong.CipherSuite)
		}
		if !errors.Is(err, errMessageProtectionUnprotect) {
			t.Fatalf("a case at suite %#04x protected over the wrong group context was refused as %v, want %v",
				wrong.CipherSuite, err, errMessageProtectionUnprotect)
		}
	}
}
