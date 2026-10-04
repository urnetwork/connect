package mls

// The class held here is "a COMPOSITION reaching a labelled construction", and it exists
// because the premise it replaces was TRUE FIELD BY FIELD and false for a sum.
//
// crypto_labels.go used to say that every value reaching a labelled construction arrived
// through a decode or an encode already bounded by syntax.MaxVectorLength, so the panic in
// mlsLabelBytes was unreachable. Every FIELD does obey that limit. A whole serialized
// structure wrapped in ONE opaque<V> does not: RefHash takes an AuthenticatedContent whose
// group_id, authenticated_data, proposal arms and signature are each bounded by a mebibyte
// and whose sum is not, and one valid proposal carrying a mebibyte credential took the
// process down in seven places -- (*ProposalCache).Store, (*AuthenticatedContent).ProposalRef,
// (*KeyPackage).Ref, SignWithLabel, VerifyWithLabel, DeriveJoinerSecret and EncryptWithLabel --
// the signature ones running BEFORE any application check a caller could have made.
//
// Two instruments hold it now and neither is a list. The boundary test below drives every
// construction AT the limit, ONE UNDER and ONE OVER, because the one fixture in this package
// that probed size -- proposal_ceiling_test.go's testEnormousProposal, at MaxVectorLength-64
// of extension data -- lands about six octets under the threshold, which is to say the only
// size probe here exercised the largest proposal that does not panic. The gate below derives
// the class off the source: it anchors on the declaration that PANICS on an encoder's
// refusal, walks BACK to every parameter whose bytes reach it, and requires every value this
// package builds with a syntax encoder and sends to one of them to have been bounded first.

import (
	"errors"
	"fmt"
	"go/ast"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect/mls/syntax"
)

// ---------------------------------------------------------------------------
// the boundary itself, at every construction a composition reaches
// ---------------------------------------------------------------------------

// One path from a value this package builds to a labelled construction, with the size of
// that value as the dial.
//
// compose and drive are two functions over one payload rather than one function answering
// both, because the assertion is about a length the test must be able to state exactly. A
// path whose drive succeeded for the wrong reason -- a composition that came out shorter
// than intended -- would read as a clean run, so compose is measured before drive is called
// and the measurement is the test's own precondition.
type labelledCompositionPath struct {
	name string
	// the composition this path hands to a labelled construction, at a payload size
	compose func(t *testing.T, payload int) []byte
	// the path itself, driven with that payload
	drive func(t *testing.T, crypto CryptoProvider, payload int) error
	// what the path answers for a composition that FITS. nil for the constructions that
	// have nothing further to say.
	fits error
}

// labelledCompositionPaths is every route from a value this package encodes to a single
// length prefixed field of a labelled construction, driven end to end.
//
// It is a table, and what keeps it honest is that it is not the gate:
// TestEveryCompositionEnteringALabelledConstructionIsBoundedBeforeItGetsThere derives the
// same set off the source and fails when the two disagree. This one is what says the
// derived set is a set of REAL crashes rather than a set of call sites, which no amount of
// walking the syntax can say.
func labelledCompositionPaths() []labelledCompositionPath {
	return []labelledCompositionPath{
		{
			name: "(*AuthenticatedContent).ProposalRef",
			compose: func(t *testing.T, payload int) []byte {
				return mustMarshalForSize(t, enormousProposalContent(t, payload))
			},
			drive: func(t *testing.T, crypto CryptoProvider, payload int) error {
				_, err := enormousProposalContent(t, payload).ProposalRef(crypto)
				return err
			},
		},
		{
			// the site the reviewer reached. Store computes the reference BEFORE every
			// ceiling it has -- its own doc pins that order, because the reference is the
			// key and a ceiling asked first would refuse a re-delivery of a message the
			// cache already holds -- so every ceiling added last commit sits downstream of
			// the crash.
			name: "(*ProposalCache).Store",
			compose: func(t *testing.T, payload int) []byte {
				return mustMarshalForSize(t, enormousProposalContent(t, payload))
			},
			drive: func(t *testing.T, crypto CryptoProvider, payload int) error {
				_, err := testCache(t).Store(crypto, testResolveContext(), enormousProposalContent(t, payload))
				return err
			},
		},
		{
			name: "(*KeyPackage).Ref",
			compose: func(t *testing.T, payload int) []byte {
				return mustMarshalForSize(t, enormousKeyPackage(t, payload))
			},
			drive: func(t *testing.T, crypto CryptoProvider, payload int) error {
				_, err := enormousKeyPackage(t, payload).Ref(crypto)
				return err
			},
		},
		{
			name: "SignWithLabel over a FramedContentTBS",
			compose: func(t *testing.T, payload int) []byte {
				return enormousFramedContentTBS(t, payload)
			},
			drive: func(t *testing.T, crypto CryptoProvider, payload int) error {
				member := testIdentity(t, crypto, "alice")
				_, err := crypto.SignWithLabel(member.SigPriv, framedContentTBSLabel,
					enormousFramedContentTBS(t, payload))
				return err
			},
		},
		{
			// THE WORST OF THEM, and it is a row of its own rather than the second half of
			// the one above for a reason mutation testing found rather than review. Driven
			// as sign-then-verify, the over long case stops at the SIGN and the verify is
			// never reached with the value under test: removing the bound from
			// VerifyWithLabel left that row green. This is the receive path, so it is driven
			// the way a peer reaches it -- a signature made over something else, arriving
			// with a preimage this side has not looked at yet.
			//
			// At a length that fits, the signature is over this very content and the answer
			// is nil, so the row also says the refusal is not simply refusing everything.
			name: "VerifyWithLabel over a FramedContentTBS a peer sent",
			compose: func(t *testing.T, payload int) []byte {
				return enormousFramedContentTBS(t, payload)
			},
			drive: func(t *testing.T, crypto CryptoProvider, payload int) error {
				member := testIdentity(t, crypto, "alice")
				content := enormousFramedContentTBS(t, payload)
				// signed over what this side CAN sign, which for the over long case is not
				// the content being verified. A real 64 byte signature either way, so the
				// length gate above it passes and the preimage is really built.
				signed := content
				if len(signed) > syntax.MaxVectorLength {
					signed = enormousFramedContentTBS(t, 32)
				}
				signature, err := crypto.SignWithLabel(member.SigPriv, framedContentTBSLabel, signed)
				if err != nil {
					return err
				}
				return crypto.VerifyWithLabel(member.SigPub, framedContentTBSLabel, content, signature)
			},
		},
		{
			name: "DeriveJoinerSecret over a GroupContext",
			compose: func(t *testing.T, payload int) []byte {
				return mustMarshalForSize(t, enormousGroupContext(payload))
			},
			drive: func(t *testing.T, crypto CryptoProvider, payload int) error {
				nh := crypto.HashSize()
				_, err := DeriveJoinerSecret(crypto, make([]byte, nh), make([]byte, nh),
					enormousGroupContext(payload))
				return err
			},
		},
		{
			name: "EncryptWithLabel over a GroupContext",
			compose: func(t *testing.T, payload int) []byte {
				return mustMarshalForSize(t, enormousGroupContext(payload))
			},
			drive: func(t *testing.T, crypto CryptoProvider, payload int) error {
				_, pub, err := crypto.DeriveKeyPair(crypto.Random(32))
				if err != nil {
					return err
				}
				_, _, err = EncryptWithLabel(crypto, pub, "node",
					mustMarshalForSize(t, enormousGroupContext(payload)), []byte("x"))
				return err
			},
		},
		{
			// the open, driven the way the verify above it is and for the same reason: a
			// seal-then-open row stops at the seal and leaves the receiving half unread,
			// which is where a peer's context arrives.
			name: "DecryptWithLabel over a GroupContext a peer sent",
			compose: func(t *testing.T, payload int) []byte {
				return mustMarshalForSize(t, enormousGroupContext(payload))
			},
			drive: func(t *testing.T, crypto CryptoProvider, payload int) error {
				priv, pub, err := crypto.DeriveKeyPair(crypto.Random(32))
				if err != nil {
					return err
				}
				context := mustMarshalForSize(t, enormousGroupContext(payload))
				sealed := context
				if len(sealed) > syntax.MaxVectorLength {
					sealed = mustMarshalForSize(t, enormousGroupContext(32))
				}
				kemOutput, ciphertext, err := EncryptWithLabel(crypto, pub, "node", sealed, []byte("x"))
				if err != nil {
					return err
				}
				_, err = DecryptWithLabel(crypto, priv, "node", context, kemOutput, ciphertext)
				return err
			},
		},
		{
			// the member of this class the original report did not name. A PSKLabel inlines
			// a PreSharedKeyID whose psk_id and psk_nonce are each bounded on their own and
			// unbounded together, both of them arrive from the wire, and PskSecret hands the
			// result to ExpandWithLabel -- a provider method with no way to refuse.
			name: "PskSecret over a PSKLabel",
			compose: func(t *testing.T, payload int) []byte {
				id := enormousPskId(payload)
				encoded, err := encodePskLabel(&id, 0, 1)
				if err != nil {
					t.Fatalf("encodePskLabel at a psk id of %d octets: %v", payload, err)
				}
				return encoded
			},
			drive: func(t *testing.T, crypto CryptoProvider, payload int) error {
				id := enormousPskId(payload)
				_, err := PskSecret(crypto, []PreSharedKeyInput{
					{Id: id, Secret: make([]byte, crypto.HashSize())},
				})
				return err
			},
		},
	}
}

// An AuthenticatedContent carrying one group_context_extensions proposal of the requested
// size, which is proposal_ceiling_test.go's testEnormousProposal with the size made a
// parameter rather than frozen six octets under the threshold.
func enormousProposalContent(t *testing.T, payload int) *AuthenticatedContent {
	t.Helper()
	return testProposalContentAt(t, LeafIndex(0), []byte("group"), 1, &Proposal{
		ProposalType: ProposalTypeGroupContextExtensions,
		GroupContextExtensions: &GroupContextExtensions{Extensions: []Extension{{
			ExtensionType: ExtensionTypeRequiredCapabilities,
			ExtensionData: make([]byte, payload),
		}}},
	})
}

// enormousAddContent is an AuthenticatedContent carrying one ADD whose key package holds a
// credential of the requested size, which is the composition three disclosures of this package
// state an octet count for.
//
// It is the Add and not the group_context_extensions proposal beside it because that is what those
// three sentences describe -- "an Add whose key package carries a BasicCredential of
// MaxVectorLength-64 octets" -- and a fixture that measured a different structure would answer a
// different number and certify the sentence anyway.
func enormousAddContent(t *testing.T, payload int) *AuthenticatedContent {
	t.Helper()
	return testProposalContentAt(t, LeafIndex(0), []byte("group"), 1, &Proposal{
		ProposalType: ProposalTypeAdd,
		Add:          &Add{KeyPackage: *enormousKeyPackage(t, payload)},
	})
}

// TestTheEnormousAddIsTheOctetCountsTheseDisclosuresState measures the numbers crypto_labels.go,
// framing_preimage.go and key_package.go each state about one composition.
//
// THE THREE SENTENCES DID NOT AGREE, which is what this case was written for. crypto_labels.go said
// the Add "marshals to 1050045 octets" and framing_preimage.go said 1050064 of the same value in
// the same words. Both numbers are real and they are lengths of DIFFERENT things: 1050045 is the
// serialized AuthenticatedContent, which is what ProposalRef and RefHash wrap, and 1050064 is the
// FramedContentTBS, which is what a signature covers. One of the two paragraphs was carrying the
// other quantity's number. Nothing measured either, so nothing said so.
//
// WHY THE COUNTS ARE PINNED EXACTLY rather than bounded below. A floor of "over
// syntax.MaxVectorLength" is satisfied by any composition at all and would leave the two disclosures
// free to go on disagreeing; what a reader takes from those paragraphs is the SIZE, so the size is
// what is held. A wire format change moves these numbers, this case fails, and the prose is
// corrected in the same commit rather than a round later.
//
// AND THE REFUSALS ARE HERE TOO, because an octet count on its own says nothing about the defect
// the paragraphs are about. The historical build signed and verified this value as an authentic
// member message and took the process down in the labelled constructions; today both doors that
// hash a whole composition refuse it, one frame above the panic.
func TestTheEnormousAddIsTheOctetCountsTheseDisclosuresState(t *testing.T) {
	crypto := testCrypto(t)
	const credential = syntax.MaxVectorLength - 64

	keyPackage := enormousKeyPackage(t, credential)
	encodedKeyPackage := mustMarshalForSize(t, keyPackage)
	if len(encodedKeyPackage) != 1050016 {
		t.Errorf("a key package whose credential carries MaxVectorLength-64 octets marshals to %d octets and key_package.go's disclosure describes one of 1050016",
			len(encodedKeyPackage))
	}

	content := enormousAddContent(t, credential)
	encodedContent := mustMarshalForSize(t, content)
	if len(encodedContent) != 1050045 {
		t.Errorf("the Add carrying it marshals to %d octets and crypto_labels.go states 1050045",
			len(encodedContent))
	}
	if len(encodedContent) <= syntax.MaxVectorLength {
		t.Fatalf("the composition is %d octets and one labelled field holds %d, so it fits and the refusals below would be about something else",
			len(encodedContent), syntax.MaxVectorLength)
	}

	// it DECODES, which is the half of the disclosure that says a peer can send this: a decoder
	// bounds each field and never their sum.
	var back AuthenticatedContent
	if err := syntax.Unmarshal(encodedContent, &back); err != nil {
		t.Fatalf("the composition did not decode back: %v; the disclosures say a decoder accepts it, which is what makes it a peer's value rather than a hand built one",
			err)
	}

	groupContext := mustMarshalForSize(t, testResolveContext())
	tbs, err := FramedContentTBSBytes(WireFormatPrivateMessage, &content.Content, groupContext)
	if err != nil {
		t.Fatalf("FramedContentTBSBytes over the composition: %v", err)
	}
	if len(tbs) != 1050064 {
		t.Errorf("the FramedContentTBS over it is %d octets and framing_preimage.go states 1050064",
			len(tbs))
	}

	if _, err := keyPackage.Ref(crypto); !errors.Is(err, syntax.ErrLengthExceedsMax) {
		t.Errorf("(*KeyPackage).Ref over the composition answered %v, want syntax.ErrLengthExceedsMax; key_package.go says this method used to take the process down over one",
			err)
	}
	if _, err := content.ProposalRef(crypto); !errors.Is(err, syntax.ErrLengthExceedsMax) {
		t.Errorf("(*AuthenticatedContent).ProposalRef over the composition answered %v, want syntax.ErrLengthExceedsMax",
			err)
	}
}

// A key package whose credential carries the requested size. The credential is where a
// peer's bytes sit in a structure this package hashes whole, and a decoder accepts it: it
// is one opaque<V> under the limit inside a composition over it.
func enormousKeyPackage(t *testing.T, payload int) *KeyPackage {
	t.Helper()
	crypto := testCrypto(t)
	keyPackage, _, _ := testKeyPackage(t, crypto, testIdentity(t, crypto, "alice"))
	keyPackage.LeafNode.Credential = BasicCredential(make([]byte, payload))
	return keyPackage
}

// The FramedContentTBS of that proposal, which is what every signature over a member
// message covers and what VerifyWithLabel is handed before anything else looks at it.
func enormousFramedContentTBS(t *testing.T, payload int) []byte {
	t.Helper()
	groupContext := mustMarshalForSize(t, testResolveContext())
	content := enormousProposalContent(t, payload)
	tbs, err := FramedContentTBSBytes(WireFormatPublicMessage, &content.Content, groupContext)
	if err != nil {
		t.Fatalf("FramedContentTBSBytes at a payload of %d octets: %v", payload, err)
	}
	return tbs
}

// A group context whose group_id carries the requested size. Every field of it is inside
// the limit and their sum need not be.
func enormousGroupContext(payload int) *GroupContext {
	return &GroupContext{
		Version:                 ProtocolVersionMls10,
		CipherSuite:             CipherSuiteX25519ChaCha20Sha256Ed25519,
		GroupId:                 make([]byte, payload),
		Epoch:                   1,
		TreeHash:                make([]byte, 32),
		ConfirmedTranscriptHash: make([]byte, 32),
	}
}

// An external psk id whose psk_id carries the requested size, with the nonce at the length
// Validate demands so that the path reaches marshalPskLabel rather than stopping short of it.
func enormousPskId(payload int) PreSharedKeyId {
	return PreSharedKeyId{
		PskType:  PskTypeExternal,
		PskId:    make([]byte, payload),
		PskNonce: make([]byte, 32),
	}
}

// syntax.Marshal with the error taken here, for the fixtures above whose whole job is to
// answer a length.
func mustMarshalForSize(t *testing.T, value syntax.Marshaler) []byte {
	t.Helper()
	encoded, err := syntax.Marshal(value)
	if err != nil {
		t.Fatalf("marshal a fixture for its size: %v", err)
	}
	return encoded
}

// The payload at which one path's composition is EXACTLY target octets, solved rather than
// computed from a formula.
//
// Solved because a formula here would be this test's own second opinion about an encoding,
// and the boundary is the whole assertion: a case that meant to sit one octet over and sat
// one octet under would pass while proving nothing. The first probe is deliberately below
// the target so that no intermediate value asks the encoder for a FIELD past its own limit,
// which is a different refusal from the one under test.
func payloadComposingTo(t *testing.T, path labelledCompositionPath, target int) int {
	t.Helper()
	payload := target - 4096
	for attempt := 0; attempt < 8; attempt++ {
		composed := len(path.compose(t, payload))
		if composed == target {
			return payload
		}
		payload += target - composed
		if payload < 0 {
			t.Fatalf("%s: no payload composes to %d octets", path.name, target)
		}
	}
	t.Fatalf("%s: solving for a composition of %d octets did not converge", path.name, target)
	return 0
}

// The outcome of one drive, with a panic reported as itself.
//
// The recover is the point rather than a courtesy. The defect being closed is a PANIC, and
// a test that only read the error would report a process kill as a failed assertion about a
// return value, which is how the ceilings added last commit were believed to cover a path
// that crashed before reaching them.
func labelledDriveOutcome(t *testing.T, crypto CryptoProvider, path labelledCompositionPath,
	payload int) (err error, recovered any) {

	t.Helper()
	defer func() { recovered = recover() }()
	return path.drive(t, crypto, payload), nil
}

// Every labelled construction refuses a composition past one field's limit, and accepts one
// at it.
//
// THREE SIZES AND THE MIDDLE ONE IS THE ASSERTION. One over says the refusal exists; at the
// limit and one under say the refusal is the RIGHT one, which is what an off by one bound
// gets wrong in the direction no crash reports. A gate written only against the crash would
// be satisfied by refusing everything.
func TestEveryLabelledConstructionRefusesACompositionPastOneFieldsLimit(t *testing.T) {
	crypto := testCrypto(t)
	for _, path := range labelledCompositionPaths() {
		t.Run(path.name, func(t *testing.T) {
			for _, size := range []struct {
				name    string
				target  int
				refused bool
			}{
				{name: "one octet under one field's limit", target: syntax.MaxVectorLength - 1},
				{name: "exactly one field's limit", target: syntax.MaxVectorLength},
				{name: "one octet over one field's limit", target: syntax.MaxVectorLength + 1, refused: true},
			} {
				payload := payloadComposingTo(t, path, size.target)
				if composed := len(path.compose(t, payload)); composed != size.target {
					t.Fatalf("%s: the composition is %d octets, want %d", size.name, composed, size.target)
				}
				err, recovered := labelledDriveOutcome(t, crypto, path, payload)
				if recovered != nil {
					t.Fatalf("%s: the path PANICKED rather than answering: %v", size.name, recovered)
				}
				if size.refused {
					if !errors.Is(err, syntax.ErrLengthExceedsMax) {
						t.Fatalf("%s: answered %v, want syntax.ErrLengthExceedsMax", size.name, err)
					}
					continue
				}
				if path.fits == nil && err != nil {
					t.Fatalf("%s: a composition that FITS was refused with %v", size.name, err)
				}
				if path.fits != nil && !errors.Is(err, path.fits) {
					t.Fatalf("%s: answered %v, want %v", size.name, err, path.fits)
				}
				if errors.Is(err, syntax.ErrLengthExceedsMax) {
					t.Fatalf("%s: a composition of exactly one field's worth was refused as too long", size.name)
				}
			}
		})
	}
}

// A label the SENDING half of a construction can actually use.
//
// The two rows below driven from the receiving side need a message to receive, and an over
// long label is one this side cannot sign or seal under either -- so the message under test
// is made with a label that fits and judged against the one that does not, which is the
// shape a peer's message has.
func labelThisSideCanUse(label string) string {
	if len(MlsLabelPrefix)+len(label) > syntax.MaxVectorLength {
		return "short"
	}
	return label
}

// The label is the OTHER opaque<V> of a labelled construction, and it is refused on the same
// terms as the value.
//
// This is a test rather than a sentence in checkLabelledConstruction's comment because the
// half of a rule that nothing drives is the half somebody deletes. Measured: with the label
// clause removed from checkLabelledConstruction, all 1685 tests of ./mls/... and
// ./message/... passed except two PINS -- nothing behavioural saw it at all, and a pin says
// only that a body changed.
//
// The boundary is not MaxVectorLength. The "MLS 1.0 " prefix this layer adds is INSIDE the
// field whose length is being declared, so the largest label that can be written is
// MaxVectorLength minus the prefix -- and a gate that measured the caller's string alone
// would admit eight octets that panic. That is the case the middle row exists for.
func TestEveryLabelledConstructionRefusesALabelTooLongToBeOneField(t *testing.T) {
	crypto := testCrypto(t)
	member := testIdentity(t, crypto, "alice")
	priv, pub, err := crypto.DeriveKeyPair(crypto.Random(32))
	if err != nil {
		t.Fatalf("DeriveKeyPair: %v", err)
	}
	// the exporter's own epoch. Any epoch will do: what is under test is the label this
	// method takes from a caller, and the schedule is here so that the call reaches the
	// KDFLabel that label is written into rather than stopping at a live secret check.
	schedule, err := NewKeyScheduleFromEpochSecret(crypto, crypto.Random(crypto.HashSize()),
		&GroupContext{
			Version:                 ProtocolVersionMls10,
			CipherSuite:             CipherSuiteX25519ChaCha20Sha256Ed25519,
			GroupId:                 []byte("the labelled bound"),
			Epoch:                   1,
			TreeHash:                crypto.Random(crypto.HashSize()),
			ConfirmedTranscriptHash: crypto.Random(crypto.HashSize()),
		})
	if err != nil {
		t.Fatalf("the epoch the exporter row is driven over: %v", err)
	}
	content := []byte("content")
	for _, entry := range []struct {
		name string
		call func(label string) error
	}{
		{name: "SignWithLabel", call: func(label string) error {
			_, err := crypto.SignWithLabel(member.SigPriv, label, content)
			return err
		}},
		{name: "VerifyWithLabel", call: func(label string) error {
			signature, err := crypto.SignWithLabel(member.SigPriv, labelThisSideCanUse(label), content)
			if err != nil {
				return err
			}
			return crypto.VerifyWithLabel(member.SigPub, label, content, signature)
		}},
		{name: "EncryptWithLabel", call: func(label string) error {
			_, _, err := EncryptWithLabel(crypto, pub, label, nil, content)
			return err
		}},
		{name: "DecryptWithLabel", call: func(label string) error {
			kemOutput, ciphertext, err := EncryptWithLabel(crypto, pub, labelThisSideCanUse(label), nil, content)
			if err != nil {
				return err
			}
			_, err = DecryptWithLabel(crypto, priv, label, nil, kemOutput, ciphertext)
			return err
		}},
		// the exported method that used to PANIC on this row. Its boundary is the prefixed
		// one and not RefHash's, because the label reaches a KDFLabel through DeriveSecret and
		// the "MLS 1.0 " that construction writes is inside the field being declared.
		{name: "(*KeySchedule).Export", call: func(label string) error {
			_, err := schedule.Export(label, []byte("context"), 32)
			return err
		}},
	} {
		t.Run(entry.name, func(t *testing.T) {
			for _, size := range []struct {
				name    string
				length  int
				refused bool
			}{
				{name: "one octet under the prefixed limit", length: syntax.MaxVectorLength - len(MlsLabelPrefix) - 1},
				{name: "exactly the prefixed limit", length: syntax.MaxVectorLength - len(MlsLabelPrefix)},
				{name: "one octet over the prefixed limit", length: syntax.MaxVectorLength - len(MlsLabelPrefix) + 1, refused: true},
			} {
				label := strings.Repeat("l", size.length)
				var err error
				recovered := recoveredPanic(func() { err = entry.call(label) })
				if recovered != nil {
					t.Fatalf("%s: the construction PANICKED rather than answering: %v", size.name, recovered)
				}
				if size.refused {
					if !errors.Is(err, syntax.ErrLengthExceedsMax) {
						t.Fatalf("%s: answered %v, want syntax.ErrLengthExceedsMax", size.name, err)
					}
					continue
				}
				if err != nil {
					t.Fatalf("%s: a label that FITS was refused with %v", size.name, err)
				}
			}
		})
	}
}

// The refusal every message pays for builds no name on the path every message takes.
//
// This is a test rather than a sentence in checkLabelledConstruction's comment because the
// sentence WAS there and was half false. The label BYTES were measured rather than
// concatenated, exactly as it said; the label's NAME, what+" label", was then built eagerly to
// describe them, on every signature and every verify in the system, for the branch that same
// comment says no message takes.
//
// A SWEEP OVER THE NAME'S LENGTH rather than a drive of the two names this package passes, and
// the reason is the instrument rather than the subject. Go concatenates into a 32 octet buffer
// on the STACK when the result cannot escape, and "signature content label" is 23 of them, so
// over this package's own names an allocation count reports zero with the defect in place --
// measured, and it is how the first version of this test came to pass against the very
// spelling it was written to reject. Past that buffer the two spellings separate: measured, a
// field name of 26 octets allocates nothing and one of 33 allocates once, 26 plus " label"
// being exactly the buffer.
//
// So the sweep runs the length THROUGH the boundary rather than picking a side of it, which is
// what makes this a statement about the declaration instead of about Go's stack buffer: no
// length of a field's name makes this build one, and a spelling that concatenates eagerly
// fails somewhere in the range whatever the buffer happens to be.
//
// Zero rather than "fewer than before". One allocation is precisely what the defect was, so
// any allocation on the accepting path is the defect back.
//
// The refusal is asserted underneath it, because the cheap way to allocate nothing is to stop
// naming the field, and a length refusal that cannot say WHICH of a construction's two fields
// was too long sends the caller to shrink the wrong one.
func TestTheLabelledConstructionCheckBuildsNoNameOnThePathAMessageTakes(t *testing.T) {
	content := make([]byte, 4096)
	for length := 0; length <= 64; length++ {
		what := strings.Repeat("n", length)
		if err := checkLabelledConstruction(what, "UpdatePathNode", content); err != nil {
			t.Fatalf("the accepted construction this measures was refused: %v", err)
		}
		var answered error
		allocations := testing.AllocsPerRun(200, func() {
			answered = checkLabelledConstruction(what, "UpdatePathNode", content)
		})
		if answered != nil {
			t.Fatalf("the measured call answered %v", answered)
		}
		if allocations != 0 {
			t.Fatalf("checkLabelledConstruction allocates %v per ACCEPTED construction under a field name of %d octets, want 0",
				allocations, length)
		}
	}
	refused := checkLabelledConstruction("signature content",
		strings.Repeat("l", syntax.MaxVectorLength), content)
	if !errors.Is(refused, syntax.ErrLengthExceedsMax) {
		t.Fatalf("an over long label answered %v, want syntax.ErrLengthExceedsMax", refused)
	}
	if !strings.Contains(refused.Error(), "signature content label") {
		t.Errorf("the refusal reads %q and does not name the label as the field that did not fit",
			refused.Error())
	}
}

// A composition parked on a struct FIELD, in both spellings of a store.
//
// A control rather than a member of this package, for the reason the test below gives: the
// only struct field composition in the tree reaches no labelled construction, so the derived
// tables cannot tell a walk that reads this shape from one that does not.
const labelledStoredCompositionControl = `package mls

type carrier struct {
	stored []byte
}

func (self *carrier) fill(v syntax.Marshaler) error {
	encoded, err := syntax.Marshal(v)
	if err != nil {
		return err
	}
	self.stored = encoded
	return nil
}

func build(v syntax.Marshaler) (*carrier, error) {
	encoded, err := syntax.Marshal(v)
	if err != nil {
		return nil, err
	}
	return &carrier{stored: encoded}, nil
}
`

// Two compositions off two writers, one of which a caller can grow and one of which nobody
// can. They are the same statements in the same order and differ in one method name, which is
// the whole of what the width reading has to see.
const labelledWriterWidthControl = `package mls

func fixedWidth(generation uint32) []byte {
	writer := syntax.NewWriter()
	writer.WriteUint32(generation)
	return mlsLabelBytes(writer)
}

func callersOctets(value []byte) []byte {
	writer := syntax.NewWriter()
	writer.WriteOpaque(value)
	return mlsLabelBytes(writer)
}
`

// One control parsed the way the walk parses this package, with its declarations indexed.
func labelledControl(t *testing.T, name string, source string) (parsedSource, map[string][]labelledDeclaration) {
	t.Helper()
	parsed := mustParseText(t, name, source)
	return parsed, labelledDeclarationsIn([]parsedSource{parsed})
}

// The expression one declaration answers, for a control whose whole body is a return.
func labelledControlAnswer(t *testing.T, function *ast.FuncDecl) ast.Expr {
	t.Helper()
	last := function.Body.List[len(function.Body.List)-1]
	returned, isReturn := last.(*ast.ReturnStmt)
	if !isReturn || len(returned.Results) != 1 {
		t.Fatalf("the control %s does not end in a single valued return", function.Name.Name)
	}
	return returned.Results[0]
}

// Every reading this walk grew is driven over a body known to hold the shape it looks for.
//
// THE DERIVED TABLES CANNOT DO THIS, and that is the whole reason this test exists rather than
// being folded into them. They compare the walk against the tree, and the tree does not have
// to carry a member of every shape the walk can read. Measured, against the tree as it stands:
// with labelledCompositionFieldsIn returning nothing, and separately with
// labelledFixedWidthComposition answering true unconditionally, the gate passed and so did all
// 1687 tests of ./mls/... and ./message/.... The only struct field composition in this package
// -- KeySchedule.groupContextBytes -- reaches no labelled construction, and the only fixed
// width one is the single row whose answer does not move when the rule stops discriminating.
//
// A reading nothing drives is a reading somebody deletes, which is the sentence
// TestEveryLabelledConstructionRefusesALabelTooLongToBeOneField is written under. So each is
// driven over a CONTROL: source this file writes, holding the shape, parsed by the same parser
// the gate reads the package with, which is the idiom parsedSource was declared for.
func TestEveryReadingOfTheCompositionWalkSeesTheShapeItLooksFor(t *testing.T) {
	// the control's own producer, named rather than derived: what is under test is the
	// reading, and a control that had to bootstrap the producer relation as well would fail
	// for two reasons at once
	producers := map[string]bool{"mlsLabelBytes": true}

	t.Run("a composition stored on a struct field", func(t *testing.T) {
		parsed, _ := labelledControl(t, "stored_composition_control.go", labelledStoredCompositionControl)
		for _, entry := range []struct {
			receiver string
			name     string
		}{
			// assigned onto a selector after the struct exists
			{receiver: "*carrier", name: "fill"},
			// and written at the composite literal that builds it, which is the spelling
			// newKeyScheduleFromParts uses
			{receiver: "", name: "build"},
		} {
			body := parsed.declarationOf(t, entry.receiver, entry.name).Body
			stored := labelledCompositionFieldsIn(body, producers,
				labelledCompositionLocals(body, producers, nil))
			if stored["stored"] != "syntax.Marshal" {
				t.Errorf("%s stores a composition on the field and the walk read %q from it",
					entry.name, stored["stored"])
			}
		}
	})

	t.Run("a composition whose size the encoding fixes", func(t *testing.T) {
		parsed, index := labelledControl(t, "writer_width_control.go", labelledWriterWidthControl)
		types := labelledParameterTypes([]parsedSource{parsed})
		carriers := labelledFieldCarrierTypes([]parsedSource{parsed})
		for _, entry := range []struct {
			name  string
			fixed bool
		}{
			{name: "fixedWidth", fixed: true},
			{name: "callersOctets", fixed: false},
		} {
			function := parsed.declarationOf(t, "", entry.name)
			answered := labelledControlAnswer(t, function)
			// the inline reading first, since a width answer about an expression the walk
			// never reaches as a composition would be an answer about nothing
			if labelledInlineComposition(answered, producers) == "" {
				t.Fatalf("%s answers a composition inline and the inline reading did not see one",
					entry.name)
			}
			got := labelledFixedWidthComposition(function.Body, answered, index[entry.name][0],
				labelledCompositionLocals(function.Body, producers, nil), types, carriers)
			if got != entry.fixed {
				t.Errorf("%s: the width reading answered %v, want %v", entry.name, got, entry.fixed)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// the class, derived off the source rather than listed
// ---------------------------------------------------------------------------

// The rendered type of every parameter of every declaration, keyed as a frame names one.
//
// A composition is BYTES, and this is what stops the walk at a parameter that cannot carry
// one. Without it a CryptoProvider handed down four frames drags every declaration in the
// package that takes a provider into the class, because ExpandWithLabel writes its LENGTH
// argument into the same preimage as its context.
func labelledParameterTypes(sources []parsedSource) map[string]string {
	types := map[string]string{}
	for _, source := range sources {
		for _, declaration := range source.file.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil {
				continue
			}
			receiver := source.receiverOf(function)
			for _, field := range function.Type.Params.List {
				rendered := source.render(field.Type)
				for _, written := range field.Names {
					frame := labelledFrame{receiver: receiver, name: function.Name.Name, parameter: written.Name}
					types[frame.String()] = rendered
				}
			}
		}
	}
	return types
}

// Every name this package declares that can BECOME one length prefixed field of a labelled
// construction, so that a parameter typed SignaturePublicKey or ProposalRef is read as the
// byte carrier it is -- and a parameter typed string is read as the LABEL half of the same
// preimage.
//
// THE BYTE HALF ALONE WAS THE CLASS UNTIL NOW, and that is written out rather than quietly
// widened, because it is the same failure the premise above this file replaced: a rule that
// was true of everything it could see and silent about half the shape. A labelled
// construction is TWO opaque<V> in one preimage -- checkLabelledConstruction says so and
// refuses both of them -- and the position walk dropped every parameter whose type was not a
// slice of bytes BEFORE it started, so the label of every labelled construction was
// structurally outside the set this gate could report. Measured: a declaration forwarding a
// caller's string straight into mlsKdfLabel left the gate green, while the control, a []byte
// composition handed to RefHash through a local, was caught. (*KeySchedule).Export was a live
// member of that blind spot: exported, holding an error return, and panicking on a caller's
// label.
//
// The two base spellings are not a list this package can understate. A value arrives at
// syntax.Writer.WriteOpaque as []byte, or through the one conversion every labelled
// construction in crypto_labels.go writes, []byte(aString); Go admits no third pre-declared
// type convertible to a slice of bytes, so what a later declaration can add is a NAMED type
// over one of those two -- and those are derived off the type declarations below, to a
// fixpoint so that a name declared over another name is one as well.
func labelledFieldCarrierTypes(sources []parsedSource) map[string]bool {
	named := map[string]bool{"[]byte": true, "string": true}
	for spreading := true; spreading; {
		spreading = false
		for _, source := range sources {
			for _, declaration := range source.file.Decls {
				generic, isGeneric := declaration.(*ast.GenDecl)
				if !isGeneric {
					continue
				}
				for _, specification := range generic.Specs {
					typed, isType := specification.(*ast.TypeSpec)
					if !isType || named[typed.Name.Name] {
						continue
					}
					if named[source.render(typed.Type)] {
						named[typed.Name.Name] = true
						spreading = true
					}
				}
			}
		}
	}
	return named
}

// Whether one node mentions any of a set of identifiers.
func labelledMentions(node ast.Node, names map[string]bool) bool {
	found := false
	ast.Inspect(node, func(inner ast.Node) bool {
		if identifier, isIdentifier := inner.(*ast.Ident); isIdentifier && names[identifier.Name] {
			found = true
		}
		return true
	})
	return found
}

// The name of the syntax encoder a call expression is, or the empty string.
//
// A COMPOSITION in the sense this file uses the word is what one of these answers: the whole
// serialized form of a structure, whose length is the SUM of its fields and is bounded by
// nothing. syntax.Marshal bounds each field it writes and says nothing about the total,
// which is precisely the gap -- it answers 1050064 octets happily and the labelled field
// that wraps them refuses.
func labelledCompositionRoot(call *ast.CallExpr) string {
	selector, isSelector := call.Fun.(*ast.SelectorExpr)
	if !isSelector {
		return ""
	}
	if base, isIdentifier := selector.X.(*ast.Ident); isIdentifier &&
		base.Name == "syntax" && strings.HasPrefix(selector.Sel.Name, "Marshal") {
		return "syntax." + selector.Sel.Name
	}
	// a Writer's own finaliser, which is what every hand assembled preimage in this package
	// ends with
	if selector.Sel.Name == "Bytes" && len(call.Args) == 0 {
		return "Writer.Bytes"
	}
	return ""
}

// The encoder a call expression answers a composition from, following calls to declarations
// of this package that answer one themselves.
func labelledCompositionOf(call *ast.CallExpr, producers map[string]bool) string {
	if root := labelledCompositionRoot(call); root != "" {
		return root
	}
	name := ""
	switch called := call.Fun.(type) {
	case *ast.Ident:
		name = called.Name
	case *ast.SelectorExpr:
		name = called.Sel.Name
	default:
		return ""
	}
	if producers[name] {
		return name
	}
	return ""
}

// The composition ONE expression answers, or the empty string.
//
// Three readings and not one, because a composition arrives at a name three ways: as the
// encoder call itself, as an alias of something already holding one -- which is the shape the
// labelled path walk in crypto_labels_test.go records nine surviving mutants for -- and as a
// FIELD read back off a receiver, which is how a composition stored in one declaration is
// used in another.
func labelledCompositionIn(value ast.Expr, producers map[string]bool, held map[string]string) string {
	switch read := value.(type) {
	case *ast.CallExpr:
		return labelledCompositionOf(read, producers)
	case *ast.Ident:
		return held[read.Name]
	case *ast.SelectorExpr:
		return held[read.Sel.Name]
	}
	return ""
}

// Every name inside one body that holds a composition, and the encoder it came from.
//
// The seed is every struct field of this package a composition is stored on, which is what
// makes a body that only READS one see it: a field is not assigned here, so no statement of
// this body mentions where its bytes came from. It is keyed by the field's own name, since a
// selector expression carries that name as an identifier and the flow walk below matches on
// identifiers.
func labelledCompositionLocals(body *ast.BlockStmt, producers map[string]bool,
	seed map[string]string) map[string]string {

	locals := map[string]string{}
	for name, from := range seed {
		locals[name] = from
	}
	for spreading := true; spreading; {
		spreading = false
		ast.Inspect(body, func(node ast.Node) bool {
			assignment, isAssignment := node.(*ast.AssignStmt)
			if !isAssignment {
				return true
			}
			from := ""
			for _, value := range assignment.Rhs {
				if of := labelledCompositionIn(value, producers, locals); of != "" {
					from = of
				}
			}
			if from == "" {
				return true
			}
			for _, target := range assignment.Lhs {
				written, isIdentifier := target.(*ast.Ident)
				if !isIdentifier || written.Name == "_" || written.Name == "err" {
					continue
				}
				if locals[written.Name] == "" {
					locals[written.Name] = from
					spreading = true
				}
			}
			return true
		})
	}
	return locals
}

// Every struct FIELD one body stores a composition on, by the name the field is written with.
//
// THE THIRD SHAPE, and the one the walk could not see at all. A composition assigned to a
// local is walked from that local and a composition passed inline is read at the call; a
// composition parked on a field leaves the body entirely and comes back somewhere else, so
// nothing in the reading body says what it is. Measured green against the version that read
// AssignStmt targets as identifiers only -- and the shape is in the tree today, as
// KeySchedule.groupContextBytes, which GroupContextBytes() hands straight back out. It is
// bounded at every constructor that fills it, so there is no defect to report; what there was
// is a gate that could not have reported one, which is why the author's own note says one
// constructor was bounded "although the derived gate does not require it".
//
// Both spellings of a store: the composite literal that BUILDS the struct, which is how
// newKeyScheduleFromParts fills its field, and an assignment onto a selector afterwards.
//
// The field NAME and not the (type, field) pair. Resolving a selector to the type it is
// reached through needs a type checker, and the coarser reading over-approximates, which is
// the safe direction here for the reason labelledDeclarationsIn gives about arity: a field
// that is not really the composition costs a row a reviewer reads, and one that is and is
// missed costs the property.
func labelledCompositionFieldsIn(body *ast.BlockStmt, producers map[string]bool,
	held map[string]string) map[string]string {

	stored := map[string]string{}
	ast.Inspect(body, func(node ast.Node) bool {
		switch written := node.(type) {
		case *ast.KeyValueExpr:
			named, isNamed := written.Key.(*ast.Ident)
			if !isNamed {
				return true
			}
			if from := labelledCompositionIn(written.Value, producers, held); from != "" {
				stored[named.Name] = from
			}
		case *ast.AssignStmt:
			from := ""
			for _, value := range written.Rhs {
				if of := labelledCompositionIn(value, producers, held); of != "" {
					from = of
				}
			}
			if from == "" {
				return true
			}
			for _, target := range written.Lhs {
				if selector, isSelector := target.(*ast.SelectorExpr); isSelector {
					stored[selector.Sel.Name] = from
				}
			}
		}
		return true
	})
	return stored
}

// Every parameter of this package one body hands a whole composition to, positionally.
//
// This exists for ONE reader and is deliberately not used anywhere else. A field is often
// filled from a parameter rather than from a local -- newKeyScheduleFromParts is handed the
// serialized group context and stores it -- so without this reading the field walk above stops
// one frame short of the only struct field composition in the tree.
//
// It is NOT fed to the producer rule and NOT fed to the edge table, and the line is the
// difference between building a composition and forwarding one. A declaration that forwards
// what its caller built has no bound to place: the caller already had one, and the frontier
// walk is what says whether the position it forwards INTO refuses. Feeding it in measured as
// exactly that mistake -- SignWithLabel would have been reported as a body that builds a
// composition and hands it to mlsSignContent with nothing bounding it, which is a sentence
// about a refusal it performs itself two lines earlier.
func labelledCompositionArgumentsIn(index map[string][]labelledDeclaration, body *ast.BlockStmt,
	producers map[string]bool, held map[string]string) map[string]string {

	handed := map[string]string{}
	ast.Inspect(body, func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if !isCall {
			return true
		}
		for _, callee := range labelledCalleesOf(index, call) {
			for i, argument := range call.Args {
				if from := labelledCompositionIn(argument, producers, held); from != "" {
					handed[labelledFrame{receiver: callee.receiver, name: callee.name,
						parameter: callee.parameters[i]}.String()] = from
				}
			}
		}
		return true
	})
	return handed
}

// Whether one declaration ANSWERS a composition, so that a caller assigning from it holds
// one.
func labelledAnswersAComposition(function *ast.FuncDecl, producers map[string]bool,
	fields map[string]string) bool {

	locals := labelledCompositionLocals(function.Body, producers, fields)
	answered := map[string]bool{}
	for name := range locals {
		answered[name] = true
	}
	found := false
	ast.Inspect(function.Body, func(node ast.Node) bool {
		returned, isReturn := node.(*ast.ReturnStmt)
		if !isReturn {
			return true
		}
		for _, result := range returned.Results {
			if call, isCall := result.(*ast.CallExpr); isCall && labelledCompositionOf(call, producers) != "" {
				found = true
			}
			if labelledMentions(result, answered) {
				found = true
			}
		}
		return true
	})
	return found
}

// Every declaration of this package that ANSWERS a composition, and every struct field one is
// STORED on, to a joint fixpoint.
//
// The two are one fixpoint rather than two passes because each is the other's input. A field
// carries a composition only once something that answers one has been written into it, and a
// declaration that hands that field back out answers one in its turn --
// KeySchedule.groupContextBytes and GroupContextBytes() are exactly that pair, and neither is
// visible from a walk that computed the other first.
func labelledCompositionCarriers(index map[string][]labelledDeclaration, t *testing.T) (
	map[string]bool, map[string]string) {

	producers := map[string]bool{}
	fields := map[string]string{}
	// see labelledCompositionArgumentsIn: this reading is the field walk's alone
	parameters := map[string]string{}
	for spreading := true; spreading; {
		spreading = false
		note := func(into map[string]string, name string, from string) {
			if from == "" || into[name] != "" {
				return
			}
			into[name] = from
			spreading = true
		}
		for name, declarations := range index {
			for _, declaration := range declarations {
				function := declaration.source.declarationOf(t, declaration.receiver, declaration.name)
				seed := map[string]string{}
				for field, from := range fields {
					seed[field] = from
				}
				for _, parameter := range declaration.parameters {
					here := labelledFrame{receiver: declaration.receiver, name: declaration.name,
						parameter: parameter}
					if from := parameters[here.String()]; from != "" && seed[parameter] == "" {
						seed[parameter] = from
					}
				}
				held := labelledCompositionLocals(function.Body, producers, seed)
				for field, from := range labelledCompositionFieldsIn(function.Body, producers, held) {
					note(fields, field, from)
				}
				for landing, from := range labelledCompositionArgumentsIn(index, function.Body, producers, held) {
					note(parameters, landing, from)
				}
				// the producer relation is deliberately NOT told about fields, and the
				// line is the same one labelledCompositionArgumentsIn draws: a
				// declaration that hands a stored composition back out FORWARDS one, and
				// this relation is about which declarations BUILD one. Admitting it
				// measured as a package wide cascade -- 47 further declarations became
				// producers, among them every Clone in the package, because this relation
				// is coarse on purpose and a field name matches everywhere it is written
				// -- which would have turned the table a reviewer reads into noise.
				if !producers[name] && labelledAnswersAComposition(function, producers, nil) {
					producers[name] = true
					spreading = true
				}
			}
		}
	}
	return producers, fields
}

// The one comparison in this package that refuses a length, found rather than named: a body
// that reads BOTH syntax.MaxVectorLength and syntax.ErrLengthExceedsMax is a body deciding
// whether something fits in one length prefixed field.
func labelledRefusesALength(source parsedSource, function *ast.FuncDecl) bool {
	rendered := source.render(function)
	return strings.Contains(rendered, "syntax.MaxVectorLength") &&
		strings.Contains(rendered, "syntax.ErrLengthExceedsMax")
}

// Every (declaration, parameter) this package BOUNDS before it forwards it, to a fixpoint
// and positionally.
//
// Positionally rather than by name, because what makes SignWithLabel safe is not that it
// mentions a refusal somewhere in its body but that the argument it hands over is the one
// the refusal judges. A by-name rule would read a body that checked its label and forwarded
// its content unbounded as a body that checks both.
//
// The base case is a parameter, so the transitive step is a parameter too: a declaration
// bounds its own parameter when it passes it to a declaration that bounds the parameter it
// LANDS ON. That is what carries the refusal in checkLabelledFieldLength out through
// checkLabelledConstruction to SignWithLabel, VerifyWithLabel, EncryptWithLabel and
// DecryptWithLabel without any of the four being named here.
func labelledBoundedParameters(t *testing.T, sources []parsedSource,
	index map[string][]labelledDeclaration,
	bounds func(parsedSource, *ast.FuncDecl, string) bool) map[string]bool {

	refused := map[string]bool{}
	for _, source := range sources {
		for _, declaration := range source.file.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil {
				continue
			}
			receiver := source.receiverOf(function)
			// the parameter the bound is over, and only that one
			for _, field := range function.Type.Params.List {
				for _, written := range field.Names {
					if bounds(source, function, written.Name) {
						refused[labelledFrame{receiver: receiver, name: function.Name.Name,
							parameter: written.Name}.String()] = true
					}
				}
			}
		}
	}
	for spreading := true; spreading; {
		spreading = false
		for _, declarations := range index {
			for _, declaration := range declarations {
				function := declaration.source.declarationOf(t, declaration.receiver, declaration.name)
				ast.Inspect(function.Body, func(node ast.Node) bool {
					call, isCall := node.(*ast.CallExpr)
					if !isCall {
						return true
					}
					for _, callee := range labelledCalleesOf(index, call) {
						for i, argument := range call.Args {
							landing := labelledFrame{receiver: callee.receiver, name: callee.name,
								parameter: callee.parameters[i]}
							if !refused[landing.String()] {
								continue
							}
							for _, field := range function.Type.Params.List {
								for _, written := range field.Names {
									if !labelledMentions(argument, map[string]bool{written.Name: true}) {
										continue
									}
									here := labelledFrame{receiver: declaration.receiver,
										name: declaration.name, parameter: written.Name}
									if !refused[here.String()] {
										refused[here.String()] = true
										spreading = true
									}
								}
							}
						}
					}
					return true
				})
			}
		}
	}
	return refused
}

// Whether one declaration REFUSES a parameter for being too long: it is a body that decides
// whether something fits in one length prefixed field, and the thing it decides about is
// this parameter.
func labelledRefusesParameter(source parsedSource, function *ast.FuncDecl, parameter string) bool {
	if !labelledRefusesALength(source, function) {
		return false
	}
	found := false
	ast.Inspect(function.Body, func(node ast.Node) bool {
		comparison, isBinary := node.(*ast.BinaryExpr)
		if !isBinary {
			return true
		}
		if !strings.Contains(source.render(comparison.Y), "syntax.MaxVectorLength") {
			return true
		}
		if labelledMentions(comparison.X, labelledAliasesOf(function, parameter)) {
			found = true
		}
		return true
	})
	return found
}

// Whether one declaration TRUNCATES a parameter to a length of its own before forwarding it.
//
// The OTHER way a value reaches a labelled field bounded, and it is the way RFC 9420 itself
// prescribes rather than a weaker substitute for a refusal. Section 6.3.2 derives the sender
// data key over a ciphertext_sample of the first KDF.Nh octets, so SenderDataKeyNonce hands
// ExpandWithLabel at most a hash's worth however long the message was. A refusal there would
// be wrong rather than stricter: a peer's whole PrivateMessage is meant to arrive.
//
// The high bound must not be read off the value's own length, and that is the line between
// the two: sample = sample[:nh] is a ceiling this declaration chose, and
// sample = sample[:len(sample)-1] is a length the caller still controls wearing a slice
// expression. Only the first bounds anything.
func labelledTruncatesParameter(source parsedSource, function *ast.FuncDecl, parameter string) bool {
	aliases := labelledAliasesOf(function, parameter)
	found := false
	ast.Inspect(function.Body, func(node ast.Node) bool {
		slice, isSlice := node.(*ast.SliceExpr)
		if !isSlice || slice.High == nil || slice.Low != nil {
			return true
		}
		if !labelledMentions(slice.X, aliases) {
			return true
		}
		if strings.Contains(source.render(slice.High), "len(") {
			return true
		}
		found = true
		return true
	})
	return found
}

// One parameter and every local of the same body that carries it.
//
// An alias is the shape this exists for, and it is the same shape crypto_labels_test.go's
// own flow walk records nine surviving mutants for: sample := ciphertext followed by
// sample = sample[:nh] bounds the ciphertext, and a matcher reading only the parameter's own
// name sees a body that never touches it.
func labelledAliasesOf(function *ast.FuncDecl, parameter string) map[string]bool {
	aliases := map[string]bool{parameter: true}
	for spreading := true; spreading; {
		spreading = false
		ast.Inspect(function.Body, func(node ast.Node) bool {
			assignment, isAssignment := node.(*ast.AssignStmt)
			if !isAssignment {
				return true
			}
			carries := false
			for _, value := range assignment.Rhs {
				if labelledMentions(value, aliases) {
					carries = true
				}
			}
			if !carries {
				return true
			}
			for _, target := range assignment.Lhs {
				written, isIdentifier := target.(*ast.Ident)
				if !isIdentifier || written.Name == "_" || aliases[written.Name] {
					continue
				}
				aliases[written.Name] = true
				spreading = true
			}
			return true
		})
	}
	return aliases
}

// Whether one declaration bounds the composition it ANSWERS, which is the other half of the
// rule: a producer either refuses its own result or hands an unbounded one to its caller.
func labelledBoundsWhatItAnswers(source parsedSource, function *ast.FuncDecl,
	producers map[string]bool, fields map[string]string) bool {

	if labelledRefusesALength(source, function) {
		return true
	}
	answered := map[string]bool{}
	for name := range labelledCompositionLocals(function.Body, producers, fields) {
		answered[name] = true
	}
	found := false
	ast.Inspect(function.Body, func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if !isCall {
			return true
		}
		identifier, isIdentifier := call.Fun.(*ast.Ident)
		if !isIdentifier || !strings.HasPrefix(identifier.Name, "checkLabelled") {
			return true
		}
		for _, argument := range call.Args {
			if labelledMentions(argument, answered) {
				found = true
			}
		}
		return true
	})
	return found
}

// The (declaration, parameter) pairs where an encoder's refusal becomes a PANIC, which is
// the anchor everything below hangs off.
//
// Derived and not named. A declaration qualifies when it panics on a value carrying an error
// it took from a composition producer -- which is exactly "this body turned an encoder's no
// into a process exit" and is what separates mlsLabelBytes from the five other panics in
// this package, none of which is reached from a syntax encoder.
func labelledPanicAnchors(t *testing.T, sources []parsedSource, producers map[string]bool) []labelledFrame {
	anchors := []labelledFrame{}
	for _, source := range sources {
		for _, declaration := range source.file.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil {
				continue
			}
			panicked := map[string]bool{}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, isCall := node.(*ast.CallExpr)
				if !isCall {
					return true
				}
				if identifier, isIdentifier := call.Fun.(*ast.Ident); !isIdentifier || identifier.Name != "panic" {
					return true
				}
				for _, argument := range call.Args {
					ast.Inspect(argument, func(inner ast.Node) bool {
						if named, isNamed := inner.(*ast.Ident); isNamed {
							panicked[named.Name] = true
						}
						return true
					})
				}
				return true
			})
			if len(panicked) == 0 {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				assignment, isAssignment := node.(*ast.AssignStmt)
				if !isAssignment {
					return true
				}
				carries := false
				for _, target := range assignment.Lhs {
					if identifier, isIdentifier := target.(*ast.Ident); isIdentifier && panicked[identifier.Name] {
						carries = true
					}
				}
				if !carries {
					return true
				}
				for _, value := range assignment.Rhs {
					call, isCall := value.(*ast.CallExpr)
					if !isCall || labelledCompositionOf(call, producers) == "" {
						continue
					}
					for _, field := range function.Type.Params.List {
						for _, written := range field.Names {
							if !labelledMentions(call, map[string]bool{written.Name: true}) {
								continue
							}
							anchors = append(anchors, labelledFrame{
								receiver:  source.receiverOf(function),
								name:      function.Name.Name,
								parameter: written.Name,
							})
						}
					}
				}
				return true
			})
		}
	}
	return anchors
}

// Every (declaration, parameter) whose bytes reach a panicking encoder, walked BACKWARDS
// from the anchor.
//
// The walk stops at a position the declaration REFUSES, and that is the whole shape of the
// answer this landed: where a signature can carry a refusal the construction carries it, and
// where it cannot the caller bounds what it builds. A refused position stays in the set --
// a reviewer needs to see it, and the composition edges that end there are closed BY it --
// but nothing behind it is dragged in, because nothing behind it can reach the panic.
func labelledFieldPositions(t *testing.T, sources []parsedSource, index map[string][]labelledDeclaration,
	anchors []labelledFrame, refused map[string]bool) (positions map[string]bool) {

	types := labelledParameterTypes(sources)
	carriers := labelledFieldCarrierTypes(sources)
	forward := map[string][]string{}
	for _, declarations := range index {
		for _, declaration := range declarations {
			for _, parameter := range declaration.parameters {
				here := labelledFrame{receiver: declaration.receiver, name: declaration.name,
					parameter: parameter}
				if !carriers[types[here.String()]] {
					continue
				}
				for _, next := range labelledFlowFrom(declaration, t, index, parameter) {
					forward[here.String()] = append(forward[here.String()], next.String())
				}
			}
		}
	}
	// reaching is "the panic is still downstream of here". An anchor reaches it by
	// definition; a position reaches it unless its own declaration refuses the value, which
	// is what makes the walk stop at SignWithLabel rather than climbing into every caller
	// that ever signed anything.
	reaching := map[string]bool{}
	for _, anchor := range anchors {
		reaching[anchor.String()] = true
	}
	positions = map[string]bool{}
	for spreading := true; spreading; {
		spreading = false
		for from, targets := range forward {
			if positions[from] {
				continue
			}
			for _, to := range targets {
				if !reaching[to] {
					continue
				}
				positions[from] = true
				if !refused[from] {
					reaching[from] = true
				}
				spreading = true
				break
			}
		}
	}
	return positions
}

// One value this package builds with a syntax encoder, and the labelled construction it
// reaches.
type labelledCompositionEdge struct {
	file     string
	from     string
	producer string
	to       string
	closed   string
}

func (self labelledCompositionEdge) String() string {
	return fmt.Sprintf("%s %s: %s -> %s (%s)", self.file, self.from, self.producer, self.to, self.closed)
}

// The expression one name was assigned from in this body, or the name itself where nothing
// here assigns it.
//
// What reads it is the fixed width question below, which is about the WRITER a composition
// came out of. A composition given a name and one passed inline have to answer that question
// the same way, or hoisting a call into a local would move an edge from one column of the
// table to another while changing no byte.
func labelledAssignedFrom(body *ast.BlockStmt, name string) ast.Expr {
	var from ast.Expr = ast.NewIdent(name)
	ast.Inspect(body, func(node ast.Node) bool {
		assignment, isAssignment := node.(*ast.AssignStmt)
		if !isAssignment || len(assignment.Rhs) != 1 {
			return true
		}
		for _, target := range assignment.Lhs {
			if written, isIdentifier := target.(*ast.Ident); isIdentifier && written.Name == name {
				from = assignment.Rhs[0]
			}
		}
		return true
	})
	return from
}

// Whether the composition an expression answers has a size the ENCODING fixes rather than one
// a caller can move.
//
// THE FOURTH ANSWER the column below can carry, and a real member rather than an excuse for
// one. (*suiteCryptoProvider).DeriveTreeSecret hands ExpandWithLabel a context built out of a
// uint32 generation and nothing else -- five octets including the vector's own length prefix,
// at every call any caller anywhere can make -- and a rule that reported that as unbounded
// would be saying one labelled field might not hold four bytes. The defect this whole file
// exists for needs a VARIABLE length field: a sum is unbounded because the fields it adds are
// each bounded and their COUNT is not.
//
// Derived off what enters the writer rather than off the writer's name or its method's. Every
// value written into it is read for whether it can carry octets -- an identifier of a field
// carrying type, a conversion to one, or a local already holding a composition -- and the
// answer is no only when NONE of them can. A statement that starts writing a caller's bytes
// into this same writer stops matching here, and its edge changes column rather than
// disappearing, which is what the table below is compared for.
func labelledFixedWidthComposition(body *ast.BlockStmt, built ast.Expr,
	declaration labelledDeclaration, locals map[string]string,
	types map[string]string, carriers map[string]bool) bool {

	writers := map[string]bool{}
	ast.Inspect(built, func(node ast.Node) bool {
		if named, isNamed := node.(*ast.Ident); isNamed {
			writers[named.Name] = true
		}
		return true
	})
	carrying := func(argument ast.Expr) bool {
		found := false
		ast.Inspect(argument, func(node ast.Node) bool {
			switch read := node.(type) {
			// a conversion to a slice, which is how a string reaches an opaque field
			case *ast.ArrayType:
				found = true
			case *ast.Ident:
				here := labelledFrame{receiver: declaration.receiver, name: declaration.name,
					parameter: read.Name}
				if carriers[read.Name] || carriers[types[here.String()]] || locals[read.Name] != "" {
					found = true
				}
			}
			return true
		})
		return found
	}
	wrote := false
	fixed := true
	ast.Inspect(body, func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if !isCall {
			return true
		}
		selector, isSelector := call.Fun.(*ast.SelectorExpr)
		if !isSelector || !strings.HasPrefix(selector.Sel.Name, "Write") {
			return true
		}
		if base, isBase := selector.X.(*ast.Ident); !isBase || !writers[base.Name] {
			return true
		}
		wrote = true
		for _, argument := range call.Args {
			if carrying(argument) {
				fixed = false
			}
		}
		return true
	})
	return wrote && fixed
}

// The encoder a composition passed INLINE came from, or the empty string.
//
// The whole argument expression rather than only its outermost call, because a composition
// wrapped in a conversion on the way to a field is the same composition arriving at the same
// field, and ProposalRef(...) is a conversion this package writes.
func labelledInlineComposition(argument ast.Expr, producers map[string]bool) string {
	from := ""
	ast.Inspect(argument, func(node ast.Node) bool {
		if call, isCall := node.(*ast.CallExpr); isCall {
			if of := labelledCompositionOf(call, producers); of != "" {
				from = of
			}
		}
		return true
	})
	return from
}

// Every composition this package builds that reaches a labelled construction, with how each
// one is closed.
//
// TWO SHAPES AND NOT ONE. A composition given a NAME is walked from that name, which is what
// the locals do and what this read until now; a composition passed INLINE has no name to walk
// from, so the argument expression is read where it lands. Measured: an inline composition
// handed to a labelled construction left the version of this gate that read only the named
// shape green, and it is the same value arriving at the same field by one fewer statement.
func labelledCompositionEdges(t *testing.T, sources []parsedSource,
	index map[string][]labelledDeclaration, producers map[string]bool, fields map[string]string,
	positions map[string]bool, refused map[string]bool, truncated map[string]bool) []string {

	types := labelledParameterTypes(sources)
	carriers := labelledFieldCarrierTypes(sources)
	edges := []string{}
	for _, declarations := range index {
		for _, declaration := range declarations {
			function := declaration.source.declarationOf(t, declaration.receiver, declaration.name)
			locals := labelledCompositionLocals(function.Body, producers, fields)
			record := func(producer string, built ast.Expr, frame labelledFrame) {
				if !positions[frame.String()] {
					return
				}
				bounded := false
				for _, candidate := range index[producer] {
					if labelledBoundsWhatItAnswers(candidate.source,
						candidate.source.declarationOf(t, candidate.receiver, candidate.name),
						producers, fields) {
						bounded = true
					}
				}
				closed := ""
				switch {
				case refused[frame.String()]:
					closed = "refused at the construction"
				case truncated[frame.String()]:
					closed = "truncated at the construction"
				case bounded:
					closed = "bounded where it is built"
				case labelledFixedWidthComposition(function.Body, built, declaration, locals,
					types, carriers):
					closed = "fixed width where it is built"
				default:
					closed = "NOTHING BOUNDS IT"
				}
				here := labelledFrame{receiver: declaration.receiver, name: declaration.name}
				edges = append(edges, labelledCompositionEdge{
					file: declaration.source.fileSet.Position(function.Pos()).Filename,
					from: strings.TrimSpace(here.String()), producer: producer,
					to: frame.String(), closed: closed,
				}.String())
			}
			for local, producer := range locals {
				built := labelledAssignedFrom(function.Body, local)
				for _, frame := range labelledFlowFrom(declaration, t, index, local) {
					record(producer, built, frame)
				}
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, isCall := node.(*ast.CallExpr)
				if !isCall {
					return true
				}
				for _, callee := range labelledCalleesOf(index, call) {
					for i, argument := range call.Args {
						if producer := labelledInlineComposition(argument, producers); producer != "" {
							record(producer, argument, labelledFrame{receiver: callee.receiver,
								name: callee.name, parameter: callee.parameters[i]})
						}
					}
				}
				return true
			})
		}
	}
	slices.Sort(edges)
	return slices.Compact(edges)
}

// The one place in this package where a syntax encoder's refusal becomes a panic.
//
// One entry and the gate compares both ways, so a SECOND declaration that turns an encoder's
// no into a process exit fails here rather than being walked from and never noticed. The
// five other panics in this package -- three in crypto.go, one in hpke.go, one in treekem.go
// -- are not reached from an encoder and are correctly outside this.
var labelledPanicAnchorFrames = []string{
	"mlsLabelBytes w",
}

// Every parameter of this package whose bytes reach the panicking encoder, and what stops
// them there.
//
// This is the FRONTIER of the class rather than the class itself: it is where a composition
// would have to arrive to reach a panic, and each entry says whether that position refuses
// an over long value, truncates it, or is open and leaves the bound to whoever built the
// value. The open ones are now exactly ONE construction: the KDF label under a CryptoProvider
// method, whose signature the pinned interface fixes. That is why the answer this landed is
// split rather than uniform.
//
// BOTH HALVES OF EVERY PREIMAGE ARE HERE NOW, and the arrival of the label rows is the whole
// second half of this table. A labelled construction is two opaque<V> and either of them alone
// latches the writer, and until labelledFieldCarrierTypes learned to read a string the walk
// dropped every label position before it started -- so the label of every construction in this
// package was outside the class this gate could report, while the sentence above it claimed
// "every parameter of this package whose bytes reach the panicking encoder". (*KeySchedule).Export
// was a live member of that blind spot and is the reason its row now says refused.
//
// The two RFC 9420 section 5.2 reference MAKERS have left this table, and their absence is the
// statement rather than an omission: RefHash refuses both of its own fields now, and the walk
// STOPS at a refused position, so MakeKeyPackageRef and MakeProposalRef are no longer upstream
// of anything that panics. Take RefHash's bound away and both come back.
//
// It is derived and compared both ways for the same reason the table below it is. A
// declaration that starts forwarding bytes into a labelled field appears here; one that
// stops refusing appears here with its parenthesis changed.
//
// mlsSignContent and mlsEncryptContext are open and that is not a gap: nothing calls either
// of them except the four entry points that refuse first, which is a fact this walk
// establishes rather than assumes -- a refused position is where the walk STOPS, so those
// two having no callers in this table is the statement that they have no unrefused ones.
// mlsKdfLabel is open on both fields for the same reason with one caller more, and that one
// is the residual: ExpandWithLabel is a provider method and cannot refuse.
//
// The six senderDataSecret rows are the walk over-approximating and are left in rather than
// filtered out. crypto_labels_test.go's flow relation spreads taint through a method call on
// a tainted local, so senderDataSecret taints the provider, the provider taints KDF.Nh, and
// KDF.Nh taints the ciphertext sample sliced with it -- a chain of three that ends at a
// context field the secret never enters. Over-approximating is the safe direction and the
// cost of it is a row; the alternative is a walk that decides for itself which flows are
// real, which is the class of judgement that let the premise this file replaces survive.
var labelledFieldFrontier = []string{
	// p7 task 11's group exporter, which forwards its caller's context straight into the
	// schedule's. The LABEL does not reach this frontier because it is a string and this walk
	// is over the bytes of this package; the context does, and it arrives here open for the
	// same reason the schedule's own row is open one line down -- a group cannot bound what a
	// caller exports over without deciding for the caller what an exporter context may be.
	"*Group.Export context (open, so a caller must bound what it sends)",
	// ledger item 228's pairwise exporter forwards the caller's LABEL to ExpandWithLabel directly
	// rather than through (*KeySchedule).Export, so the refusal that declaration makes on its
	// behalf is made here instead. The CONTEXT is not a position on this frontier at all, because
	// nothing a caller passes reaches it: PairwiseExport builds the context itself.
	"*Group.PairwiseExport label (refused here)",
	// ledger item 242's R2 exporter over the STAGED epoch, which is Export's row one schedule
	// over: it forwards its caller's context straight into the staged schedule's Export, and it
	// arrives here open for exactly the reason Export's does two lines up.
	"*Group.PendingExport context (open, so a caller must bound what it sends)",
	// the exported method that took a caller's label straight into a KDFLabel and panicked
	// on it, while its own signature already carried ErrExportLength for a caller's number
	"*KeySchedule.Export context (open, so a caller must bound what it sends)",
	"*KeySchedule.Export label (refused here)",
	// the three provider methods the pinned interface fixes, which is the whole residual
	"*suiteCryptoProvider.DeriveSecret label (open, so a caller must bound what it sends)",
	"*suiteCryptoProvider.DeriveTreeSecret label (open, so a caller must bound what it sends)",
	"*suiteCryptoProvider.ExpandWithLabel context (open, so a caller must bound what it sends)",
	"*suiteCryptoProvider.ExpandWithLabel label (open, so a caller must bound what it sends)",
	"*suiteCryptoProvider.SignWithLabel content (refused here)",
	"*suiteCryptoProvider.SignWithLabel label (refused here)",
	"*suiteCryptoProvider.VerifyWithLabel content (refused here)",
	"*suiteCryptoProvider.VerifyWithLabel label (refused here)",
	"DecryptWithLabel context (refused here)",
	"DecryptWithLabel label (refused here)",
	"EncryptWithLabel context (refused here)",
	"EncryptWithLabel label (refused here)",
	"OpenPrivateMessage senderDataSecret (open, so a caller must bound what it sends)",
	// the exported reference maker, on both of its fields. It adds no "MLS 1.0 " of its own,
	// so both boundaries are the whole MaxVectorLength rather than the prefix less one
	"PeekPrivateMessageSender senderDataSecret (open, so a caller must bound what it sends)",
	"RefHash label (refused here)",
	"RefHash value (refused here)",
	"SealPrivateMessage senderDataSecret (open, so a caller must bound what it sends)",
	"SenderDataKeyNonce ciphertext (truncated here)",
	"SenderDataKeyNonce senderDataSecret (open, so a caller must bound what it sends)",
	"mlsEncryptContext context (open, so a caller must bound what it sends)",
	"mlsEncryptContext label (open, so a caller must bound what it sends)",
	"mlsKdfLabel context (open, so a caller must bound what it sends)",
	"mlsKdfLabel label (open, so a caller must bound what it sends)",
	"mlsSignContent content (open, so a caller must bound what it sends)",
	"mlsSignContent label (open, so a caller must bound what it sends)",
	"openPrivateMessageAt senderDataSecret (open, so a caller must bound what it sends)",
	"openSenderData senderDataSecret (open, so a caller must bound what it sends)",
	"sealPrivateMessage senderDataSecret (open, so a caller must bound what it sends)",
	// MASTER section 8.4.2 v2's two seal bodies and the open one, each carrying the neighbouring
	// name's reading unchanged: the secret reaches SenderDataKeyNonce's ExpandWithLabel exactly as
	// it does through the name that delegates to them, and the bound is the caller's for the same
	// reason -- this is an epoch secret, so a caller that handed over an unbounded one has already
	// lost more than a panic.
	"sealPrivateMessageAt senderDataSecret (open, so a caller must bound what it sends)",
	"sealPrivateMessageBound senderDataSecret (open, so a caller must bound what it sends)",
	"sealSenderData senderDataSecret (open, so a caller must bound what it sends)",
}

// Every composition this package builds that reaches a labelled construction, and how each
// one is closed.
//
// A hand written list is what this project has understated fourteen times, so nothing here
// is trusted: the walk derives the same set from the syntax of the package on every run and
// the two are compared BOTH WAYS. A composition added anywhere in this package that reaches
// a labelled field appears here and fails; one deleted from this table fails as well; and a
// bound removed from either end changes the parenthesis rather than the line, so it fails
// too. The table is what a reviewer reads. The walk is what says the table is the class.
//
// Read down the right hand column and the shape of the answer is the whole of it. Where the
// construction's signature can carry a refusal it carries one, and the composition arrives
// unbounded on purpose -- a signature preimage IS the whole message and refusing to build
// one this side would be refusing to check a peer's. Where it cannot, the value is bounded
// at the declaration that BUILDS it, which is the only place upstream of a provider method
// that still has a caller to answer.
//
// THREE SHAPES REACH THIS TABLE and only one of them is an assignment. A composition given a
// name is walked from that name; one passed INLINE is read at the call it lands in; one parked
// on a STRUCT FIELD is picked up wherever the field is read. The second and third were
// measured green against the version of this walk that read AssignStmt alone, which is what
// "every composition this package builds" was claiming while two thirds of the ways to write
// one were invisible to it.
//
// The two RFC 9420 section 5.2 reference rows have left this table, and their absence is a
// statement about RefHash rather than about (*KeyPackage).Ref or (*AuthenticatedContent).ProposalRef.
// Those two still bound what they build -- the error a caller reads has to name a KEY PACKAGE
// rather than a reference input -- but RefHash refuses the same octets one frame further down,
// the frontier walk stops at a refused position, and a composition that cannot reach a panic is
// not an edge to a panic. Take RefHash's bound away and both rows come back.
//
// The fixed width row is the fourth answer and not a hole. DeriveTreeSecret's context is a
// uint32 generation and nothing else, so its size is the encoding's rather than a caller's;
// see labelledFixedWidthComposition, which derives that off what enters the writer.
var labelledCompositionClass = []string{
	"crypto_labels.go *suiteCryptoProvider.DeriveTreeSecret: mlsLabelBytes -> *suiteCryptoProvider.ExpandWithLabel context (fixed width where it is built)",
	"framing_protect.go SignAuthenticatedContent: FramedContentTBSBytes -> *suiteCryptoProvider.SignWithLabel content (refused at the construction)",
	"framing_protect.go VerifyAuthenticatedContent: FramedContentTBSBytes -> *suiteCryptoProvider.VerifyWithLabel content (refused at the construction)",
	// ledger item 228's pairwise exporter, and it is the psk label's row one more time: the
	// context is a COMPOSITION handed to ExpandWithLabel, whose signature cannot refuse, so the
	// bound sits on the outermost declaration of the path that still has a caller to answer.
	// marshalPairwiseContext is that declaration.
	//
	// AND IT IS A DECLARATION RATHER THAN A WRITER INLINE IN PairwiseExport BECAUSE OF THIS GATE.
	// Written inline, this walk read THREE unbounded edges out of the one method: Writer.Bytes,
	// and two more through the group's own fields -- self.schedule taints from
	// NewKeyScheduleFromJoiner and self.tree from syntax.MarshalLimit, so an inline
	// self.schedule.Secrets() read arrived here as a serialized structure reaching a labelled
	// field with nothing bounding it. That is senderDataSecretLocked's account of the same
	// over-reporting, and it has the same answer: a call into a declaration of this package is a
	// frame of its own, walked in its turn.
	"group.go *Group.PairwiseExport: marshalPairwiseContext -> *suiteCryptoProvider.ExpandWithLabel context (bounded where it is built)",
	"key_package.go *KeyPackage.Validate: signedPreimage -> *suiteCryptoProvider.VerifyWithLabel content (refused at the construction)",
	"key_package.go NewKeyPackageWithSigner: signedPreimage -> *suiteCryptoProvider.SignWithLabel content (refused at the construction)",
	"key_schedule.go DeriveJoinerSecret: marshalBoundedComposition -> *suiteCryptoProvider.ExpandWithLabel context (bounded where it is built)",
	"key_schedule.go NewKeyScheduleFromJoiner: marshalBoundedComposition -> *suiteCryptoProvider.ExpandWithLabel context (bounded where it is built)",
	"leaf_node.go *LeafNode.Sign: signatureContent -> *suiteCryptoProvider.SignWithLabel content (refused at the construction)",
	"leaf_node.go *LeafNode.VerifySignature: signatureContent -> *suiteCryptoProvider.VerifyWithLabel content (refused at the construction)",
	"psk.go PskSecret: marshalPskLabel -> *suiteCryptoProvider.ExpandWithLabel context (bounded where it is built)",
	"welcome.go *GroupInfo.Sign: signaturePreimage -> *suiteCryptoProvider.SignWithLabel content (refused at the construction)",
	"welcome.go *GroupInfo.Verify: signaturePreimage -> *suiteCryptoProvider.VerifyWithLabel content (refused at the construction)",
}

func TestEveryCompositionEnteringALabelledConstructionIsBoundedBeforeItGetsThere(t *testing.T) {
	sources := packageSources(t)
	index := labelledDeclarationsIn(sources)
	producers, fields := labelledCompositionCarriers(index, t)
	anchors := labelledPanicAnchors(t, sources, producers)

	anchored := []string{}
	for _, anchor := range anchors {
		anchored = append(anchored, anchor.String())
	}
	slices.Sort(anchored)
	anchored = slices.Compact(anchored)
	if !slices.Equal(anchored, labelledPanicAnchorFrames) {
		t.Fatalf("an encoder's refusal becomes a panic at\n%s\nand this gate knows of\n%s",
			strings.Join(anchored, "\n"), strings.Join(labelledPanicAnchorFrames, "\n"))
	}

	// the two shapes a declaration bounds what it forwards in, kept apart because they are
	// not the same statement: a refusal says the value cannot pass, a truncation says only
	// a fixed prefix of it does. Both stop the walk; only one of them is a refusal, and a
	// reviewer reading the table below needs to see which is which.
	refused := labelledBoundedParameters(t, sources, index, labelledRefusesParameter)
	truncated := labelledBoundedParameters(t, sources, index, labelledTruncatesParameter)
	bounded := map[string]bool{}
	for position := range refused {
		bounded[position] = true
	}
	for position := range truncated {
		bounded[position] = true
	}
	positions := labelledFieldPositions(t, sources, index, anchors, bounded)
	frontier := []string{}
	for position := range positions {
		switch {
		case refused[position]:
			frontier = append(frontier, position+" (refused here)")
		case truncated[position]:
			frontier = append(frontier, position+" (truncated here)")
		default:
			frontier = append(frontier, position+" (open, so a caller must bound what it sends)")
		}
	}
	slices.Sort(frontier)
	if !slices.Equal(frontier, labelledFieldFrontier) {
		t.Errorf("the bytes of this package reach the panicking encoder through\n%s\nand this gate knows of\n%s",
			strings.Join(frontier, "\n"), strings.Join(labelledFieldFrontier, "\n"))
	}

	edges := labelledCompositionEdges(t, sources, index, producers, fields, positions, refused, truncated)
	if !slices.Equal(edges, labelledCompositionClass) {
		t.Errorf("the compositions of this package reach a labelled construction as\n%s\nand this gate knows of\n%s",
			strings.Join(edges, "\n"), strings.Join(labelledCompositionClass, "\n"))
	}
	for _, edge := range edges {
		if strings.Contains(edge, "NOTHING BOUNDS IT") {
			t.Errorf("a composition reaches a labelled construction unbounded: %s", edge)
		}
	}
}
