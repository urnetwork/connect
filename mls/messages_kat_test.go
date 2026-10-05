// The runner for the mlswg messages vector family, number 12.
//
// This is the ninth family to register against the registry in vectors_test.go, and none of the
// machinery the eight before it needed is repeated here: the accounting is vectorRunTally, the
// comparator control is assertComparatorRefuses, the registration assertion is
// assertVectorFamilyIsInstalled and the second decode of the corpus is publishedCorpusField, all
// of them in vectors_runner_test.go.
//
// What this family is. Three hundred cases, each a row of seventeen independent encodings of one
// MLS structure apiece, and one rule applied to all of them: decode with the structure the column
// names, re-encode, and the bytes must be identical. There is no ciphersuite column -- nothing
// here is decrypted, verified or derived, so no case selects a provider -- which is why the tally
// runs in its suiteless mode and why the per suite split assertions are not part of this family's
// accounting. Objects must be syntactically valid; a MAC or a signature inside one is arbitrary
// and is not checked, because nothing in this corpus publishes the key it was taken under.
//
// What is this family's own, and why each part is a part:
//
//   - the DECODE OBLIGATION in front of every round trip. syntax.CheckRoundTrip returns nil for
//     input that does not decode, which is exactly right for the fuzz targets it was written for
//     -- a decoder that refuses a random blob owes nothing about re-encoding it -- and exactly
//     wrong here, where every one of these 5100 blobs is a known good encoding published by
//     another implementation. Five of the seventeen columns go through that function, and with a
//     bare call they accept anything at all: measured, a case with all seventeen columns
//     truncated by one octet passes five of them. Every column here therefore decodes FIRST,
//     through a call whose failure is a refusal, and takes the round trip afterwards.
//   - the SHAPE obligation on the columns whose name states one. mls_welcome, mls_group_info and
//     mls_key_package name a wire format and public_message_application, _proposal and _commit
//     name a wire format and a content type. Without it, two columns of this corpus swapped for
//     each other is a case every round trip in this file accepts: both values decode, both
//     re-encode byte exactly, and the run reports seventeen comparisons over a row that holds
//     fifteen of its own answers. The shape is read off the column's NAME, which is the only
//     thing that says what the column is.
//   - the seven proposal columns read through the PRODUCTION Proposal codec rather than through
//     a decoder written here. The corpus publishes the BODY of a proposal arm with no
//     discriminant in front of it, and the obvious reading of that is a reader and a writer
//     spelled out in this file for each arm -- which is what a harness that then reports the
//     column as covered would be testing. It would test itself: RFC 9420 section 12.1's arm
//     bodies are proposal_wire.go's to encode, and a re_init arm this package wrote in another
//     field order would go on round tripping through a closure that made the same choice. The
//     discriminant is put back on instead and the whole read through Proposal, so what these
//     seven columns hold is production.
//
// What this runner adds over the four tests that already read this corpus, stated because a
// runner that adds no protection of its own is better replaced by a comment. Ten of the
// seventeen columns have another reader on this branch: framing_test.go's
// TestTheMessagesCorpusMlsMessagesRoundTripByteExactly takes the seven MLSMessage columns,
// commit_wire_test.go takes commit, welcome_wire_test.go takes group_secrets, and psk_test.go
// takes pre_shared_key_proposal. The other SEVEN -- add, update, remove, re_init, external_init
// and group_context_extensions, plus ratchet_tree -- are read by nothing else at all, and six of
// those seven are the arms whose bodies only exist in this corpus. This runner also installs
// family 12, so the corpus is offered to it by TestVectorFamiliesVerify and 12 leaves
// expectedPendingFamilies.
//
// Measured, and recorded because it is the shape of what this file does NOT hold: nothing here
// is the SOLE catcher of a code defect. Every single edit tried against the codecs these columns
// cover -- a joiner secret's length prefix widened symmetrically, a re_init arm's group id moved
// behind its two uint16 fields symmetrically -- is refused by this runner and by a hand derived
// layout or golden test next door. What this family holds alone is the seven columns above and
// the accounting: 5100 comparisons against 4801 distinct published answers, which is the number
// nothing else in the package states.
//   - the comparison made against a SECOND DECODE of the corpus text. The comparator returns the
//     octets it re-encoded rather than a verdict, and TestVectorMessages holds those against the
//     published string read out of a generic json decode with no struct tag in the way. A struct
//     tag pointed at a key the corpus does not publish decodes to the empty string, and an empty
//     re-encoding compared against an empty published value agrees.
//
// What this family cannot see, stated because a runner that overstates itself is worse than one
// that does less. Two columns swapped for each other are caught only where their names state
// different shapes; a corpus that swapped update_proposal for some other bare LeafNode column
// would be accepted, and there is no second LeafNode column today. And a codec whose encoder and
// decoder transpose the SAME two fields of one type in both directions re-encodes byte exactly by
// construction -- a round trip cannot see a symmetric transposition, only an asymmetric one. What
// does see it is the fact that these are foreign bytes: a transposition of two fields of
// different wire shapes stops the foreign encoding decoding at all, which is the refusal above.
package mls

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect/mls/syntax"
)

// messagesKatFile is the corpus this family reads. It is pinned by digest in VECTORS.sha256 and
// gated by TestVectorFilesArePinned like the other fifteen.
const messagesKatFile = "messages.json"

// The accounting that makes this runner unable to pass having compared nothing.
//
// Transcriptions of what testdata/vectors/messages.json holds at the pinned mlswg commit: three
// hundred cases of seventeen columns each, so 5100 comparisons, made against 4801 distinct
// published strings. The shortfall of 299 is the group_context_extensions_proposal column, which
// is an empty extensions vector -- one octet, 0x00 -- in every case.
//
// Written down rather than derived, for the reason p4 task 16 gives: deriving an expected count
// with the same reader that is under test is how a reader that matched nothing ends up agreeing
// with itself. What IS derived and checked alongside them is that the column count is the number
// of json keys the vector struct declares, that those keys are exactly the keys the corpus
// publishes, and that each codec row reads the column it is named after.
const (
	messagesEntries     = 300
	messagesFields      = 17
	messagesComparisons = messagesEntries * messagesFields
	messagesDistinct    = 4801
)

// Family 12 is installed here, and 12 is deleted from expectedPendingFamilies in the same commit.
// Without both halves TestVectorFamiliesVerify runs one fewer family and the manifest gate stays
// green while claiming this family is unimplemented.
//
// Generate is nil, and it is nil for a reason rather than for want of an implementation. This
// family's corpus is a pile of FOREIGN encodings, and its whole content is that our decoder
// accepts them and our encoder reproduces them. Cases emitted by this package's own encoder
// would be re-encoded by this package's own encoder and would agree by construction, at every
// column, whatever either half did. A generator here would close no loop; it would report one.
func init() {
	RegisterVectorFamily(VectorFamily{
		Number:   12,
		Name:     "Messages",
		File:     messagesKatFile,
		Slice:    "A4",
		Verify:   verifyMessagesVector,
		Generate: nil,
	})
}

// The refusals compareMessagesVector makes, as sentinels rather than as formatted strings, so a
// test can require a specific refusal rather than "some error".
//
// They are what makes the comparison observable at all. Every case of the vendored corpus agrees
// with this implementation, so a comparator that checked everything and a comparator that checked
// nothing produce identical runs over it; the only way to tell them apart is to hand it a case
// that is wrong on purpose and require the matching refusal, which is
// TestCompareMessagesVectorRefusesACaseItShouldNotAccept.
var (
	errMessagesIncomplete    = errors.New("the comparison reports values it cannot have computed")
	errMessagesFieldMissing  = errors.New("the case publishes no value for a column this family reads")
	errMessagesFieldEmpty    = errors.New("the case publishes an empty value for a column this family reads")
	errMessagesDoesNotDecode = errors.New("a published encoding does not decode as the structure its column names")
	errMessagesWrongShape    = errors.New("a published encoding is not the message its column name states")
	errMessagesNotByteExact  = errors.New("a published encoding did not survive a decode and re-encode unchanged")
)

// messagesVector is one entry of messages.json.
//
// Every column is a hex string in the file and stays a string here: MustHex is the single
// decoder, HexOf the single encoder, and a struct holding []byte would need a second one at the
// json boundary. The json tags are also the CLASS this family's rows are derived from --
// TestMessagesCodecsAreEveryColumnTheCorpusPublishes holds one codec row per tag and one tag per
// row -- so a corpus that grew an eighteenth column fails there rather than being silently
// uncompared.
type messagesVector struct {
	MlsWelcome                     string `json:"mls_welcome"`
	MlsGroupInfo                   string `json:"mls_group_info"`
	MlsKeyPackage                  string `json:"mls_key_package"`
	RatchetTree                    string `json:"ratchet_tree"`
	GroupSecrets                   string `json:"group_secrets"`
	AddProposal                    string `json:"add_proposal"`
	UpdateProposal                 string `json:"update_proposal"`
	RemoveProposal                 string `json:"remove_proposal"`
	PreSharedKeyProposal           string `json:"pre_shared_key_proposal"`
	ReInitProposal                 string `json:"re_init_proposal"`
	ExternalInitProposal           string `json:"external_init_proposal"`
	GroupContextExtensionsProposal string `json:"group_context_extensions_proposal"`
	Commit                         string `json:"commit"`
	PublicMessageApplication       string `json:"public_message_application"`
	PublicMessageProposal          string `json:"public_message_proposal"`
	PublicMessageCommit            string `json:"public_message_commit"`
	PrivateMessage                 string `json:"private_message"`
}

// messagesCodec is one row of the table: the column, the accessor that reads it out of a decoded
// case, and the check that decodes and re-encodes it.
//
// Written as data so that a column with no decoder is a compile error naming the column rather
// than a silent skip. The name is the json key the column is published under, and
// TestMessagesCodecsReadTheColumnTheyAreNamedAfter holds the accessor to it over a case whose
// every field carries its own key as its value -- so a row that names one column and reads
// another is a failure rather than a comparison of the wrong answer against the right one.
type messagesCodec struct {
	name  string
	field func(v *messagesVector) string
	// check decodes data, requires the round trip to be byte exact, and RETURNS the octets it
	// re-encoded. A checker returning only an error reports that it ran; the octets are what
	// let the runner hold the answer against a decode of the corpus this file did not make.
	check func(data []byte) ([]byte, error)
}

// checkProposalArmColumn is the form for the seven columns that carry the BODY of one arm of an
// RFC 9420 section 12.1 Proposal, with no discriminant in front of it.
//
// The discriminant is put BACK ON and the whole read through the production Proposal codec, then
// taken off the re-encoding again. The alternative -- a reader and a writer for each arm spelled
// out in this file -- is what the shape of the corpus invites and it is a harness testing itself:
// the arm bodies are proposal_wire.go's to encode, and an arm this package wrote in another field
// order round trips perfectly through a closure in a test file that made the same choice. Going
// through Proposal means these 2100 bodies are foreign encodings held against the encoder a peer
// will actually meet.
//
// The prefix is derived by asking the codec to write the discriminant rather than by assuming two
// octets, and it is CHECKED off the front of the re-encoding rather than skipped: a Proposal that
// emitted its discriminant at another width would otherwise have that width silently absorbed
// into the body on the way back out, and the body would compare equal.
func checkProposalArmColumn(proposalType ProposalType) func(data []byte) ([]byte, error) {
	return func(data []byte) ([]byte, error) {
		w := syntax.NewWriter()
		w.WriteUint16(uint16(proposalType))
		prefix, err := w.Bytes()
		if err != nil {
			return nil, fmt.Errorf("%w: encode the %#04x discriminant: %w", errMessagesNotByteExact,
				uint16(proposalType), err)
		}
		framed := append(bytes.Clone(prefix), data...)
		proposal := &Proposal{}
		if err := syntax.Unmarshal(framed, proposal); err != nil {
			return nil, fmt.Errorf("%w: %w", errMessagesDoesNotDecode, err)
		}
		if proposal.ProposalType != proposalType {
			return nil, fmt.Errorf("%w: the body decoded as proposal type %#04x under the %#04x discriminant",
				errMessagesWrongShape, uint16(proposal.ProposalType), uint16(proposalType))
		}
		encoded, err := syntax.Marshal(proposal)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", errMessagesNotByteExact, err)
		}
		if len(encoded) < len(prefix) || !bytes.Equal(encoded[:len(prefix)], prefix) {
			return nil, fmt.Errorf("%w: the re-encoded proposal is %s and does not begin with the %s this codec wrote for the discriminant",
				errMessagesNotByteExact, HexOf(encoded), HexOf(prefix))
		}
		body := encoded[len(prefix):]
		if !bytes.Equal(body, data) {
			return body, fmt.Errorf("%w: re-encoded %s, want %s", errMessagesNotByteExact,
				HexOf(body), HexOf(data))
		}
		return body, nil
	}
}

// checkCodecReEncode is the whole-wire-type form: decode through the type's own Codec, hold the
// round trip to syntax.CheckRoundTrip -- the same property gate 4 asserts, on the same code path,
// rather than a hand rolled comparison -- and answer the re-encoded octets.
//
// The syntax.Unmarshal in front of CheckRoundTrip is not redundant and this file exists partly to
// carry that sentence. CheckRoundTrip returns nil for input that does not decode: that is its
// contract and the right one for a fuzz target, where a decoder refusing a random blob owes
// nothing about re-encoding it. Here every input is a known good encoding published by another
// implementation, so "it did not decode" is the loudest finding this family can make, and a bare
// CheckRoundTrip converts it into a pass. Measured on this corpus: with the bare call, a case
// whose commit and group_secrets columns each stop one octet short is ACCEPTED at both, and the
// two are the only columns this function is asked about. It was asked about five before the seven
// proposal columns moved to the production codec, and the other three were then refused for the
// WRONG reason -- an encode failure out of a half decoded value rather than a decode failure --
// which a control naming its sentinel catches and a control asking only for "some error" does
// not.
func checkCodecReEncode[T any, PT interface {
	*T
	syntax.Codec
}](data []byte) ([]byte, error) {

	value := PT(new(T))
	if err := syntax.Unmarshal(data, value); err != nil {
		return nil, fmt.Errorf("%w: %w", errMessagesDoesNotDecode, err)
	}
	if err := syntax.CheckRoundTrip[T, PT](data); err != nil {
		return nil, fmt.Errorf("%w: %w", errMessagesNotByteExact, err)
	}
	encoded, err := syntax.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errMessagesNotByteExact, err)
	}
	return encoded, nil
}

// checkMLSMessageColumn is the form for the six columns that are a whole MLSMessage, closed over
// the shape the column's name states.
//
// Every byte goes through ParseMLSMessage and MarshalMLSMessage, which are the one entry point
// and the one exit the whole system names, rather than through a second decode path invented for
// the harness.
//
// The shape is the half a round trip cannot supply. Six columns of this corpus are MLSMessages
// and all six round trip; what separates public_message_commit from public_message_proposal is
// the content type inside, and without checking it a corpus with those two columns exchanged is a
// case this family accepts at every one of its seventeen comparisons. contentType zero means the
// column's name states no content type -- a welcome, a group info, a key package and a private
// message name none -- and is asserted as an absence rather than skipped.
func checkMLSMessageColumn(wireFormat WireFormat, contentType ContentType) func(data []byte) ([]byte, error) {
	return func(data []byte) ([]byte, error) {
		message, err := ParseMLSMessage(data)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", errMessagesDoesNotDecode, err)
		}
		if message.WireFormat != wireFormat {
			return nil, fmt.Errorf("%w: the column carries wire format %#04x and its name states %#04x",
				errMessagesWrongShape, uint16(message.WireFormat), uint16(wireFormat))
		}
		if contentType != 0 {
			carried, err := mlsMessageContentType(message)
			if err != nil {
				return nil, fmt.Errorf("%w: %w", errMessagesWrongShape, err)
			}
			if carried != contentType {
				return nil, fmt.Errorf("%w: the column carries content type %d and its name states %d",
					errMessagesWrongShape, carried, contentType)
			}
		}
		encoded, err := MarshalMLSMessage(message)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", errMessagesNotByteExact, err)
		}
		if !bytes.Equal(encoded, data) {
			return encoded, fmt.Errorf("%w: re-encoded %s, want %s", errMessagesNotByteExact,
				HexOf(encoded), HexOf(data))
		}
		return encoded, nil
	}
}

// mlsMessageContentType is the framing content type an MLSMessage carries, or a refusal where the
// arm it carries has none.
//
// The two framed arms keep it in different places -- a PublicMessage inside its FramedContent, a
// PrivateMessage in its own header, which is the whole point of the header -- so this is a switch
// and not a field read.
func mlsMessageContentType(message *MLSMessage) (ContentType, error) {
	switch {
	case message.PublicMessage != nil:
		return message.PublicMessage.Content.ContentType, nil
	case message.PrivateMessage != nil:
		return message.PrivateMessage.ContentType, nil
	}
	return 0, fmt.Errorf("the message carries no framed arm, so it states no content type")
}

// checkRatchetTreeColumn is the ratchet_tree column, which is the one column that must NOT run at
// the default vector length limit.
//
// UnmarshalRatchetTree carries MaxRatchetTreeLength, and the re-encode is given the same bound
// rather than the default: a tree that decoded under the raised limit and was then written back
// through a Writer capped at MaxVectorLength would fail byte exactness with a length error,
// reporting a round trip violation for a reason that has nothing to do with canonicality --
// which is a false positive arriving in the one situation where somebody is chasing a real one.
// The corpus's trees are a couple of hundred octets, so neither bound bites today; the pair is
// written as a pair so that the day one does, both halves move together.
func checkRatchetTreeColumn(data []byte) ([]byte, error) {
	tree, err := UnmarshalRatchetTree(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errMessagesDoesNotDecode, err)
	}
	encoded, err := syntax.MarshalLimit(tree, syntax.MaxRatchetTreeLength)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errMessagesNotByteExact, err)
	}
	if !bytes.Equal(encoded, data) {
		return encoded, fmt.Errorf("%w: re-encoded %s, want %s", errMessagesNotByteExact,
			HexOf(encoded), HexOf(data))
	}
	return encoded, nil
}

// messagesCodecs is one row per column. A column with no checker does not compile, and
// TestMessagesCodecsAreEveryColumnTheCorpusPublishes names the shortfall if a row goes missing.
func messagesCodecs() []messagesCodec {
	return []messagesCodec{
		// the whole MLSMessages. Each names its wire format, and the three framed ones name
		// their content type too.
		{"mls_welcome", func(v *messagesVector) string { return v.MlsWelcome },
			checkMLSMessageColumn(WireFormatWelcome, 0)},
		{"mls_group_info", func(v *messagesVector) string { return v.MlsGroupInfo },
			checkMLSMessageColumn(WireFormatGroupInfo, 0)},
		{"mls_key_package", func(v *messagesVector) string { return v.MlsKeyPackage },
			checkMLSMessageColumn(WireFormatKeyPackage, 0)},
		{"public_message_application", func(v *messagesVector) string { return v.PublicMessageApplication },
			checkMLSMessageColumn(WireFormatPublicMessage, ContentTypeApplication)},
		{"public_message_proposal", func(v *messagesVector) string { return v.PublicMessageProposal },
			checkMLSMessageColumn(WireFormatPublicMessage, ContentTypeProposal)},
		{"public_message_commit", func(v *messagesVector) string { return v.PublicMessageCommit },
			checkMLSMessageColumn(WireFormatPublicMessage, ContentTypeCommit)},
		{"private_message", func(v *messagesVector) string { return v.PrivateMessage },
			checkMLSMessageColumn(WireFormatPrivateMessage, 0)},

		// the standalone wire types, through their own codecs.
		{"commit", func(v *messagesVector) string { return v.Commit },
			checkCodecReEncode[Commit, *Commit]},
		{"group_secrets", func(v *messagesVector) string { return v.GroupSecrets },
			checkCodecReEncode[GroupSecrets, *GroupSecrets]},

		// the seven proposal arm bodies, each read through the production Proposal codec with
		// its own discriminant put back in front of it.
		{"add_proposal", func(v *messagesVector) string { return v.AddProposal },
			checkProposalArmColumn(ProposalTypeAdd)},
		{"update_proposal", func(v *messagesVector) string { return v.UpdateProposal },
			checkProposalArmColumn(ProposalTypeUpdate)},
		{"remove_proposal", func(v *messagesVector) string { return v.RemoveProposal },
			checkProposalArmColumn(ProposalTypeRemove)},
		{"pre_shared_key_proposal", func(v *messagesVector) string { return v.PreSharedKeyProposal },
			checkProposalArmColumn(ProposalTypePreSharedKey)},
		{"re_init_proposal", func(v *messagesVector) string { return v.ReInitProposal },
			checkProposalArmColumn(ProposalTypeReInit)},
		{"external_init_proposal", func(v *messagesVector) string { return v.ExternalInitProposal },
			checkProposalArmColumn(ProposalTypeExternalInit)},
		{"group_context_extensions_proposal", func(v *messagesVector) string {
			return v.GroupContextExtensionsProposal
		},
			checkProposalArmColumn(ProposalTypeGroupContextExtensions)},

		// the ratchet tree, at the raised vector length bound.
		{"ratchet_tree", func(v *messagesVector) string { return v.RatchetTree },
			checkRatchetTreeColumn},
	}
}

// messagesCheck is one column of one case: the column it came from, and the octets this package
// re-encoded it to.
//
// The published half is NOT carried here and that is deliberate. It is re-read by the runner out
// of a generic decode of the corpus text, so the comparison is between what this package produced
// and what the file says, with no struct tag standing between the two.
//
// That second reading is REDUNDANT with the byte comparison each checker already makes, and it is
// recorded as redundant rather than as a second guarantee: replacing this field with the
// published octets themselves, so that the runner compares the corpus against itself, leaves the
// whole of ./mls/... and ./message/... green. It is here because the two are redundant only while
// every checker keeps its own byte comparison -- a checker that lost one would leave the runner's
// reading as the only thing between a re-encoding and the file.
type messagesCheck struct {
	name string
	got  []byte
}

// messagesComparison is what one run of compareMessagesVector PRODUCED, and it is the only thing
// its callers are allowed to judge it by.
//
// A comparator returning a bool reports that control reached the bottom of the function and not
// that a comparison happened: an early return above it leaves the runner counting cases that
// never decoded a column at all, and the run stays green.
type messagesComparison struct {
	// columns is the number of codec rows this run was driven with, written before the loop so
	// that a table that shrank is visible even from a run that made no comparison at all.
	columns int
	// checks is every column that decoded and re-encoded, in row order.
	checks []messagesCheck
	// failures is every column that did not, kept rather than returned one at a time so that a
	// case reports all seventeen of its verdicts instead of the first.
	failures []error
}

// incomplete reports whether the evidence a compared case must carry is missing or inconsistent,
// without looking at whether any re-encoding was right.
//
// bytes.Equal over two empty slices says they agree, so a column whose re-encoding is empty has
// compared nothing whatever a comparison over it would say -- and a runner that counted such
// columns would report the full seventeen having decoded none of them.
func (self messagesComparison) incomplete() error {
	if self.columns != messagesFields {
		return fmt.Errorf("%w: the table offered %d columns and this family reads %d",
			errMessagesIncomplete, self.columns, messagesFields)
	}
	if len(self.checks)+len(self.failures) != self.columns {
		return fmt.Errorf("%w: %d columns answered and %d were refused over a table of %d; a column took neither branch",
			errMessagesIncomplete, len(self.checks), len(self.failures), self.columns)
	}
	seen := map[string]int{}
	for _, check := range self.checks {
		if len(check.got) == 0 {
			return fmt.Errorf("%w: %s re-encoded to nothing, and an empty comparison agrees with anything",
				errMessagesIncomplete, check.name)
		}
		seen[check.name]++
	}
	for name, count := range seen {
		if count != 1 {
			return fmt.Errorf("%w: %s was compared %d times and this family compares each column once per case",
				errMessagesIncomplete, name, count)
		}
	}
	return nil
}

// verdict is the whole judgement over one compared case: it must be complete, and every column
// must have decoded and re-encoded unchanged.
//
// Completeness first, so a run that compared fewer columns than the table holds is reported as
// that rather than as whichever of its columns happened to also disagree.
func (self messagesComparison) verdict() error {
	if err := self.incomplete(); err != nil {
		return err
	}
	return errors.Join(self.failures...)
}

// verifyMessagesVector is the registry's shim: the signature RegisterVectorFamily needs, over the
// comparator that does the work and reports what it produced.
//
// Every column's refusal is reported rather than only the first, because a case that broke in
// four places is more useful read as four than as one.
func verifyMessagesVector(t *testing.T, raw json.RawMessage) {
	t.Helper()
	evidence := compareMessagesVector(t, raw)
	if err := evidence.incomplete(); err != nil {
		t.Fatalf("%s: %v", messagesKatFile, err)
	}
	for _, failure := range evidence.failures {
		t.Errorf("%s: %v", messagesKatFile, failure)
	}
}

// refuseMessagesVector is the comparator in the shape assertComparatorRefuses drives: a verdict
// rather than a fatal, so a control can require a refusal instead of ending the test that asked
// for one.
func refuseMessagesVector(t *testing.T, raw json.RawMessage) error {
	t.Helper()
	return compareMessagesVector(t, raw).verdict()
}

// compareMessagesVector runs one case of messages.json and returns what the run produced.
//
// A corpus that will not parse or will not hex decode is fatal here rather than returned, because
// it is not a verdict about this implementation -- it is the evidence itself being unreadable,
// and every family in this package treats that as the loudest failure there is. Everything that
// IS a verdict about this implementation is returned, so a caller can require a refusal instead
// of hoping the corpus disagrees with a defect.
//
// A column the case does not publish at all, and a column published as the empty string, are two
// different failures and are reported as two: the first is a struct tag pointed at a key the
// corpus does not use, the second is a corpus that published nothing for a column it names.
// Through the struct alone the two are one empty string.
func compareMessagesVector(t *testing.T, raw json.RawMessage) messagesComparison {
	t.Helper()
	vector := messagesVector{}
	if err := json.Unmarshal(raw, &vector); err != nil {
		t.Fatalf("parse a %s case: %v", messagesKatFile, err)
	}
	published := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &published); err != nil {
		t.Fatalf("parse a %s case as a json object: %v", messagesKatFile, err)
	}

	codecs := messagesCodecs()
	evidence := messagesComparison{columns: len(codecs)}
	for _, codec := range codecs {
		if _, found := published[codec.name]; !found {
			evidence.failures = append(evidence.failures,
				fmt.Errorf("%w: %s", errMessagesFieldMissing, codec.name))
			continue
		}
		encoded := codec.field(&vector)
		if encoded == "" {
			evidence.failures = append(evidence.failures,
				fmt.Errorf("%w: %s", errMessagesFieldEmpty, codec.name))
			continue
		}
		reencoded, err := codec.check(MustHex(t, encoded))
		if err != nil {
			evidence.failures = append(evidence.failures, fmt.Errorf("%s: %w", codec.name, err))
			continue
		}
		evidence.checks = append(evidence.checks, messagesCheck{name: codec.name, got: reencoded})
	}
	return evidence
}

// TestVectorMessages is vector family 12 over the published corpus.
//
// Every assertion the tally makes after the loop exists because the loop can be made to run zero
// times without anything else in this package noticing. A corpus that parsed to an empty array, a
// codec table that lost half its rows, a comparator that answered without decoding: each of those
// is a green run of this test with the accounting removed, and a failure with it.
//
// The tally runs in its suiteless mode, which it derives from the corpus itself: messages.json
// publishes no cipher_suite column, so every case is covered and none is declined. What the loop
// counts is not calls that returned. It counts re-encodings this runner itself held against a
// GENERIC decode of the corpus text -- no struct tag in the way -- so a comparator that answered
// without decoding anything is a failure here rather than a number that looks right.
func TestVectorMessages(t *testing.T) {
	tally, entries := newVectorRunTally(t, messagesKatFile)
	for index, raw := range entries {
		published := map[string]json.RawMessage{}
		if err := json.Unmarshal(raw, &published); err != nil {
			t.Fatalf("%s case %d: %v", messagesKatFile, index, err)
		}
		tally.cover(t)
		evidence := compareMessagesVector(t, raw)
		if err := evidence.verdict(); err != nil {
			t.Fatalf("%s case %d: %v", messagesKatFile, index, err)
		}
		for _, check := range evidence.checks {
			want := publishedCorpusField(t, published, check.name)
			if got := HexOf(check.got); got != want {
				t.Fatalf("%s case %d: %s re-encoded to %s, the corpus publishes %s",
					messagesKatFile, index, check.name, got, want)
			}
			tally.answer(want)
		}
	}
	tally.assertRun(t, messagesEntries, 0, messagesComparisons, messagesDistinct)
}

// TestMessagesFamilyIsInstalled is the registration half of task 18.
//
// Family 12 declares no generator and that is asserted as an absence rather than left unmentioned,
// so a generator added to this row without a test that holds it to anything is a failure here. The
// installed verifier is DRIVEN over a published case and over one with every hex field corrupted,
// by assertVectorFamilyIsInstalled: pointer identity says the manifest holds this function, and
// says nothing about the function doing anything.
func TestMessagesFamilyIsInstalled(t *testing.T) {
	assertVectorFamilyIsInstalled(t, 12, messagesKatFile, verifyMessagesVector, nil)
}

// messagesColumnTags is every json key messagesVector decodes, in declaration order.
//
// Derived by reflection over the struct rather than listed, for guardrail 5's reason: a listed
// class understates itself the moment a field lands beside the ones somebody remembered.
//
// AND AN UNTAGGED FIELD IS FATAL RATHER THAN SKIPPED, which is the repair of a narrowing this
// package indexes in GATES.md. The skip read as harmless because the complement is EMPTY today --
// every field of the struct carries a tag -- and an empty complement is the one GATES.md's own
// table calls the dangerous case: it removes nothing now and begins removing real columns on the
// commit that adds the first untagged field. encoding/json decodes such a field under its GO
// NAME, so the column would decode perfectly well and this function, whose whole claim is "every
// json key this struct decodes", would answer without it -- and every count derived from it would
// be one short against a corpus that publishes the key.
func messagesColumnTags(t *testing.T) []string {
	t.Helper()
	tags := []string{}
	shape := reflect.TypeOf(messagesVector{})
	for index := 0; index < shape.NumField(); index++ {
		key, _, _ := strings.Cut(shape.Field(index).Tag.Get("json"), ",")
		if key == "" {
			t.Fatalf("messagesVector.%s carries no json key, so it decodes under its go name and this table would name every column but that one",
				shape.Field(index).Name)
		}
		tags = append(tags, key)
	}
	return tags
}

// TestMessagesCodecsAreEveryColumnTheCorpusPublishes binds the codec table to the corpus in both
// directions, through the struct that stands between them.
//
// Three sets have to agree and any two of them agreeing is not enough: the keys the corpus
// publishes, the json tags the vector struct decodes, and the names the codec table holds. A key
// the struct does not name is a column nothing reads; a tag the table does not hold is a column
// that decodes and is never checked; a row naming a key the corpus does not publish is a
// comparison against nothing. The plan's own row-count assertion catches only the third of those
// and only when the count also changes.
func TestMessagesCodecsAreEveryColumnTheCorpusPublishes(t *testing.T) {
	tags := messagesColumnTags(t)
	if len(tags) != messagesFields {
		t.Fatalf("messagesVector declares %d json keys and this family reads %d columns",
			len(tags), messagesFields)
	}
	rows := []string{}
	for _, codec := range messagesCodecs() {
		if codec.field == nil || codec.check == nil {
			t.Fatalf("the codec row %q carries no accessor or no checker", codec.name)
		}
		rows = append(rows, codec.name)
	}
	if !slices.Equal(slices.Sorted(slices.Values(rows)), slices.Sorted(slices.Values(tags))) {
		t.Fatalf("the codec table holds %v and messagesVector decodes %v", rows, tags)
	}
	if len(slices.Compact(slices.Sorted(slices.Values(rows)))) != len(rows) {
		t.Fatalf("the codec table names %d columns of which some is named twice: %v", len(rows), rows)
	}

	declared := map[string]bool{}
	for _, tag := range tags {
		declared[tag] = true
	}
	cases := 0
	for index, raw := range LoadVectorFile(t, messagesKatFile) {
		published := map[string]json.RawMessage{}
		if err := json.Unmarshal(raw, &published); err != nil {
			t.Fatalf("%s case %d: %v", messagesKatFile, index, err)
		}
		for key := range published {
			if !declared[key] {
				t.Errorf("%s case %d publishes %q and messagesVector names no field for it",
					messagesKatFile, index, key)
			}
		}
		for key := range declared {
			if _, found := published[key]; !found {
				t.Errorf("messagesVector decodes %q and %s case %d does not publish it",
					key, messagesKatFile, index)
			}
		}
		cases++
	}
	if cases != messagesEntries {
		t.Fatalf("%s holds %d cases and this family's accounting is written for %d",
			messagesKatFile, cases, messagesEntries)
	}
}

// TestMessagesCodecsReadTheColumnTheyAreNamedAfter holds every row's accessor to its own name.
//
// A row that names one column and reads another is the copy-paste this table's shape invites, and
// it is invisible to every other assertion in this file: the comparison would decode a valid
// encoding, re-encode it byte exactly, and be held against the published value of the column it
// read rather than the one it named -- so the corpus would agree, twice, about one column, and
// the other would never be checked. The case below carries its own key as the value of every
// field, so a row reading the wrong field answers the wrong key by name.
func TestMessagesCodecsReadTheColumnTheyAreNamedAfter(t *testing.T) {
	marker := messagesVector{}
	value := reflect.ValueOf(&marker).Elem()
	shape := value.Type()
	filled := 0
	for index := 0; index < shape.NumField(); index++ {
		key, _, _ := strings.Cut(shape.Field(index).Tag.Get("json"), ",")
		// fatal rather than skipped, for messagesColumnTags' reason: a field with no json key
		// still decodes, under its go name, and a marker that leaves it empty would hand every
		// codec reading it the empty string -- which reads here as a codec naming the wrong
		// column rather than as a column this loop declined to fill.
		if key == "" {
			t.Fatalf("messagesVector.%s carries no json key, so this marker would leave it empty and every codec reading it would answer the empty string",
				shape.Field(index).Name)
		}
		value.Field(index).SetString(key)
		filled++
	}
	if filled != messagesFields {
		t.Fatalf("filled %d fields of messagesVector and this family reads %d columns", filled, messagesFields)
	}
	for _, codec := range messagesCodecs() {
		if got := codec.field(&marker); got != codec.name {
			t.Errorf("the codec row %q reads the field that decodes %q", codec.name, got)
		}
	}
}

// messagesKatBaseCase answers a published case, together with the encoder the controls below
// corrupt it through.
//
// The base is the corpus's own, not a fixture: the whole of what the refusals below mean is that
// this exact case is accepted and a one octet edit of it is not.
func messagesKatBaseCase(t *testing.T) (messagesVector, func(messagesVector) json.RawMessage) {
	t.Helper()
	entries := LoadVectorFile(t, messagesKatFile)
	if len(entries) == 0 {
		t.Fatalf("%s holds no case, so this control has nothing to corrupt", messagesKatFile)
	}
	base := messagesVector{}
	if err := json.Unmarshal(entries[0], &base); err != nil {
		t.Fatalf("parse a %s case: %v", messagesKatFile, err)
	}
	encode := func(entry messagesVector) json.RawMessage {
		body, err := json.Marshal(entry)
		if err != nil {
			t.Fatalf("marshal the case under test: %v", err)
		}
		return body
	}
	return base, encode
}

// messagesDropOctet is one published encoding with its last octet removed.
//
// One octet and not a rewrite, because the claim being controlled is that a column which stops
// short of the structure it names is REFUSED. A blob of random would be refused by anything; a
// valid encoding missing its tail is refused only by a decode that reads to the end of it, and
// that is the obligation five of these columns did not have before this file.
func messagesDropOctet(t *testing.T, text string) string {
	t.Helper()
	octets := MustHex(t, text)
	if len(octets) < 2 {
		t.Fatalf("nothing to drop from %q", text)
	}
	return HexOf(octets[:len(octets)-1])
}

// TestCompareMessagesVectorRefusesACaseItShouldNotAccept is the control the runner cannot be: it
// hands the comparator cases that are wrong in each of the ways the corpus is not, and requires
// the matching refusal.
//
// The truncation rows are the ones that matter most and they are one per DECODE PATH as well as
// spread across the columns: the seven proposal arms, the two whole wire types, the seven
// MLSMessages and the ratchet tree are four different paths, and a row per path is what says each
// of them reads to the end of its input. The two rows named commit and group_secrets are also the
// measurement in checkCodecReEncode's own comment: against a bare syntax.CheckRoundTrip they are
// ACCEPTED.
func TestCompareMessagesVectorRefusesACaseItShouldNotAccept(t *testing.T) {
	base, encode := messagesKatBaseCase(t)
	rows := []struct {
		name string
		edit func(*messagesVector)
		want error
	}{
		{"a commit that stops one octet short", func(v *messagesVector) {
			v.Commit = messagesDropOctet(t, v.Commit)
		}, errMessagesDoesNotDecode},
		{"a group_secrets that stops one octet short", func(v *messagesVector) {
			v.GroupSecrets = messagesDropOctet(t, v.GroupSecrets)
		}, errMessagesDoesNotDecode},
		{"an add_proposal that stops one octet short", func(v *messagesVector) {
			v.AddProposal = messagesDropOctet(t, v.AddProposal)
		}, errMessagesDoesNotDecode},
		{"an update_proposal that stops one octet short", func(v *messagesVector) {
			v.UpdateProposal = messagesDropOctet(t, v.UpdateProposal)
		}, errMessagesDoesNotDecode},
		{"a pre_shared_key_proposal that stops one octet short", func(v *messagesVector) {
			v.PreSharedKeyProposal = messagesDropOctet(t, v.PreSharedKeyProposal)
		}, errMessagesDoesNotDecode},
		{"a remove_proposal that stops one octet short", func(v *messagesVector) {
			v.RemoveProposal = messagesDropOctet(t, v.RemoveProposal)
		}, errMessagesDoesNotDecode},
		{"a re_init_proposal that stops one octet short", func(v *messagesVector) {
			v.ReInitProposal = messagesDropOctet(t, v.ReInitProposal)
		}, errMessagesDoesNotDecode},
		{"an external_init_proposal that stops one octet short", func(v *messagesVector) {
			v.ExternalInitProposal = messagesDropOctet(t, v.ExternalInitProposal)
		}, errMessagesDoesNotDecode},
		{"a private_message that stops one octet short", func(v *messagesVector) {
			v.PrivateMessage = messagesDropOctet(t, v.PrivateMessage)
		}, errMessagesDoesNotDecode},
		{"a ratchet_tree that stops one octet short", func(v *messagesVector) {
			v.RatchetTree = messagesDropOctet(t, v.RatchetTree)
		}, errMessagesDoesNotDecode},
		// the two column swaps. Both values decode and both re-encode byte exactly, so nothing
		// but the shape the column's NAME states separates them.
		{"the proposal message published where the commit message belongs", func(v *messagesVector) {
			v.PublicMessageCommit = v.PublicMessageProposal
		}, errMessagesWrongShape},
		{"the group info published where the welcome belongs", func(v *messagesVector) {
			v.MlsWelcome = v.MlsGroupInfo
		}, errMessagesWrongShape},
		{"a column published as the empty string", func(v *messagesVector) {
			v.ExternalInitProposal = ""
		}, errMessagesFieldEmpty},
	}
	refusals := []comparatorRefusal{}
	for _, row := range rows {
		corrupted := base
		row.edit(&corrupted)
		refusals = append(refusals, comparatorRefusal{row.name, encode(corrupted), row.want})
	}
	// and the column the case does not publish at all, which no edit to the struct can express:
	// an absent key and an empty value decode identically through it.
	withoutColumn := map[string]json.RawMessage{}
	if err := json.Unmarshal(encode(base), &withoutColumn); err != nil {
		t.Fatalf("re-decode the base case: %v", err)
	}
	if _, found := withoutColumn["remove_proposal"]; !found {
		t.Fatal("the base case publishes no remove_proposal, so the row below deletes nothing")
	}
	delete(withoutColumn, "remove_proposal")
	body, err := json.Marshal(withoutColumn)
	if err != nil {
		t.Fatalf("re-encode the case with a column deleted: %v", err)
	}
	refusals = append(refusals, comparatorRefusal{
		"a case that does not publish one of the columns at all", body, errMessagesFieldMissing,
	})

	assertComparatorRefuses(t, "messages", refuseMessagesVector, encode(base), refusals)
}

// messagesShapelessProbe is one opaque<V> over octets no column of this corpus publishes.
//
// It is what separates a checker that CHECKS A STRUCTURE from one whose whole body is a variable
// length vector and therefore cannot tell one column of that shape from another. RFC 9420 section
// 12.1 gives ExternalInit exactly such a body -- opaque kem_output<V> and nothing else -- so this
// is a real property of the registry rather than a hole in the table.
func messagesShapelessProbe(t *testing.T) []byte {
	t.Helper()
	w := syntax.NewWriter()
	w.WriteOpaque([]byte("an opaque vector no column of the messages corpus publishes"))
	probe, err := w.Bytes()
	if err != nil {
		t.Fatalf("build the shapeless probe: %v", err)
	}
	return probe
}

// messagesIllegalProbes are octets no structure this corpus publishes encodes to.
//
// The middle one is the load bearing one: 0xff is varint prefix bits 11, which p1's reader refuses
// before it has read a length at all, so a checker that accepts it is not reading through this
// package's syntax layer whatever else it claims. They exist so that the exclusion below cannot be
// bought: a checker that accepted EVERYTHING would accept the shapeless probe too and would
// otherwise be exempted for looking like an opaque vector.
func messagesIllegalProbes() map[string][]byte {
	return map[string][]byte{
		"nothing at all":                      {},
		"a reserved varint length prefix":     {0xff},
		"five octets with no length in front": {0x01, 0x02, 0x03, 0x04, 0x05},
	}
}

// aWholeInputOpaqueVector reports whether these octets are one opaque<V> and nothing after it.
//
// The second half of the exclusion below. A shapeless checker is only entitled to accept a column
// whose encoding IS the shape it cannot distinguish, so the column is required to be one too --
// which makes the exemption a statement about the pair rather than a licence granted to one
// checker over everything it happens to swallow.
func aWholeInputOpaqueVector(data []byte) bool {
	r := syntax.NewReader(data)
	body, err := r.ReadOpaque()
	if err != nil || r.Remaining() != 0 {
		return false
	}
	w := syntax.NewWriter()
	w.WriteOpaque(body)
	encoded, err := w.Bytes()
	return err == nil && bytes.Equal(encoded, data)
}

// TestMessagesEveryColumnDecodesThroughItsOwnCheckerAndNotAnother is the control on the checker
// shapes themselves, and it is the PAIRWISE exclusion its name states rather than a count.
//
// Every column of this corpus round trips, so a table in which two rows had been given each
// other's checker would fail loudly -- but only for the pairs whose encodings are incompatible,
// and there is no gate saying which pairs those are. The version this replaces asked only that at
// least ONE of the other sixteen checkers refuse each column, which is weaker than the name: a
// checker that accepted fifteen of the seventeen column encodings satisfies every row of that
// loop, because one incompatible checker somewhere in the table is enough to keep the count above
// zero.
//
// What is asserted instead is that NO other checker accepts a column, with one exclusion that is
// derived rather than named. MEASURED over this corpus: fifteen of the seventeen columns are
// refused by all sixteen of the other checkers, and the two that are not --
// group_context_extensions_proposal and ratchet_tree -- are accepted by external_init_proposal's
// checker alone, because section 12.1 gives ExternalInit a body that is one opaque<V> and nothing
// else, and a vector is a vector. That containment is not a defect in any row, so it is exempted
// -- but only by proving BOTH halves of it: the accepting checker must accept a bare opaque<V>
// over octets this corpus does not publish, and the accepted column's own octets must be one
// opaque<V> spanning the whole input. Neither half is written down as a name.
//
// Every checker is separately required to refuse octets nothing in this corpus encodes to, so a
// checker that accepted everything cannot buy the exclusion by also accepting the probe. The count
// is derived from the table rather than written down, so a row added without a checker of its own
// cannot slip past by keeping the total right.
//
// What this still cannot see: a checker that dropped ONE half of the shape its name states -- the
// wire format or the content type -- while keeping the other, because the surviving half is enough
// to refuse every other column in this table. Each half is observed instead by a column swap row
// of TestCompareMessagesVectorRefusesACaseItShouldNotAccept: the proposal-for-commit swap is
// refused only by the content type check and the group-info-for-welcome swap only by the wire
// format check, so neither half of checkMLSMessageColumn is unmeasured.
func TestMessagesEveryColumnDecodesThroughItsOwnCheckerAndNotAnother(t *testing.T) {
	base, _ := messagesKatBaseCase(t)
	codecs := messagesCodecs()
	if len(codecs) != messagesFields {
		t.Fatalf("the codec table holds %d rows and this family reads %d columns", len(codecs), messagesFields)
	}

	probe := messagesShapelessProbe(t)
	shapeless := map[string]bool{}
	for _, codec := range codecs {
		if encoded, err := codec.check(probe); err == nil && bytes.Equal(encoded, probe) {
			shapeless[codec.name] = true
		}
		for name, illegal := range messagesIllegalProbes() {
			if _, err := codec.check(illegal); err == nil {
				t.Errorf("%s accepted %s, so it accepts what nothing in this corpus encodes to and the exclusion below would exempt it for a reason that is not true of it",
					codec.name, name)
			}
		}
	}
	if len(shapeless) == len(codecs) {
		t.Fatalf("all %d checkers in this table accept a bare opaque vector, so the exclusion below exempts every pair and this test holds nothing",
			len(codecs))
	}

	excluded := 0
	for _, own := range codecs {
		data := MustHex(t, own.field(&base))
		if _, err := own.check(data); err != nil {
			t.Errorf("%s was refused by its own checker: %v", own.name, err)
			continue
		}
		for _, other := range codecs {
			if other.name == own.name {
				continue
			}
			if _, err := other.check(data); err != nil {
				continue
			}
			if !shapeless[other.name] {
				t.Errorf("%s accepts %s's octets and is not a checker whose whole body is one opaque vector; a checker that accepts a column it does not name is not checking the structure its name states",
					other.name, own.name)
				continue
			}
			if !aWholeInputOpaqueVector(data) {
				t.Errorf("%s accepts %s's octets and those octets are not one opaque vector spanning the whole input, so the containment that exempts this pair is not the one that is actually happening",
					other.name, own.name)
				continue
			}
			excluded++
		}
	}
	t.Logf("%d of the %d checkers accept a bare opaque vector and %d ordered pairs are exempted by that containment; every other pair is refused",
		len(shapeless), len(codecs), excluded)
}
