// The RFC 9420 section 6.1 signature: what is in the preimage, what a verifier does with it,
// and the two ValSem codes that come out.
//
// Every refusal here is derived over the LENGTH or the SHAPE of the thing it alters rather
// than sampled at a position somebody chose, and that is not style. The three authentication
// bypasses this project has shipped were all found by something other than the test that was
// supposed to find them: a tag verifier reading the first byte of a 32 byte tag passed a test
// that flipped bit zero of byte zero, and a verifier accepting every truncation passed a suite
// with no length case in it at all. A sampled refusal states that ONE input is refused; the
// property is that every one is.
package mls

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"maps"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/urnetwork/connect/mls/syntax"
)

// ---------------------------------------------------------------------------
// the values this file and the package's provider gates sign
// ---------------------------------------------------------------------------

// framingStubSignaturePriv is the seed every row of every gate in this package signs a framed
// content with. One value rather than one per row, so a row that reads the wrong key is not
// answered by a neighbour's.
func framingStubSignaturePriv() SignaturePrivateKey {
	return SignaturePrivateKey(bytes.Repeat([]byte{0x5d}, 32))
}

// framingStubFramedContent is a member's application message with every byte carrying field
// populated.
//
// Every field carries something on purpose. A preimage that dropped one is invisible when that
// field is empty to begin with, and the stub gate's zero-answer report reads the byte fields
// of the AuthenticatedContent this is signed into -- so an empty one there would be reported
// as the stub it is not.
func framingStubFramedContent() *FramedContent {
	return framingStubFramedContentOver(func(content []byte) []byte { return content })
}

// The same content with every array it carries taken through a caller's hook, which is what
// lets the aliasing gate see the arrays a signature was built over.
func framingStubFramedContentOver(take func(content []byte) []byte) *FramedContent {
	return &FramedContent{
		GroupId:           take([]byte{0x11, 0x12, 0x13, 0x14}),
		Epoch:             7,
		Sender:            Sender{SenderType: SenderTypeMember, LeafIndex: 2},
		AuthenticatedData: take([]byte{0x21, 0x22}),
		ContentType:       ContentTypeApplication,
		ApplicationData:   take([]byte("the payload a framed content carries")),
	}
}

// framingStubGroupContext is a serialized GroupContext for the gates that sign one in.
//
// It is built through this package's own codec and at the PROVIDER's own hash width, rather
// than being a run of bytes this file chose. The width is what makes it usable by the KDF.Nh
// differential: a group context whose hashes were a written down 32 octets would be the same
// bytes over a provider whose KDF.Nh is 48, and the row would state nothing about the width
// the caller is running at.
func framingStubGroupContext(t *testing.T, crypto CryptoProvider) []byte {
	t.Helper()
	encoded, err := syntax.Marshal(&GroupContext{
		Version:                 ProtocolVersionMls10,
		CipherSuite:             crypto.Suite(),
		GroupId:                 []byte{0x11, 0x12, 0x13, 0x14},
		Epoch:                   7,
		TreeHash:                bytes.Repeat([]byte{0x92}, crypto.HashSize()),
		ConfirmedTranscriptHash: bytes.Repeat([]byte{0x93}, crypto.HashSize()),
	})
	if err != nil {
		t.Fatalf("encode the group context these signatures are bound to: %v", err)
	}
	return encoded
}

// providerStubFramingArguments fills in the arguments the package's provider gates call
// SignAuthenticatedContent and VerifyAuthenticatedContent with.
//
// It lives here rather than in crypto_test.go for the reason the psk list's and the leaf's
// values do: nothing there knows how to build a FramedContent that encodes, and the verify row
// needs a message that has ACTUALLY been signed rather than one that resembles a signed one --
// a base call that refused would leave every perturbation below it comparing one refusal
// against another and reporting a complete implementation as observing all of its inputs.
//
// The group context is the one the rest of that gate's rows are built over, serialized, so a
// construction that read the epoch out of it is moved by the same perturbation that moves it
// for the key schedule.
func providerStubFramingArguments(t *testing.T, fixture CryptoProvider, priv SignaturePrivateKey,
	groupContext *GroupContext, arguments map[string]any) {

	t.Helper()
	encodedGroupContext, err := syntax.Marshal(groupContext)
	if err != nil {
		t.Fatalf("encode the group context the framing rows are built over: %v", err)
	}
	arguments["WireFormat"] = WireFormatPrivateMessage
	arguments["*FramedContent"] = framingStubFramedContent()
	arguments["SignAuthenticatedContent.groupContext"] = encodedGroupContext
	arguments["VerifyAuthenticatedContent.groupContext"] = encodedGroupContext
	signed, err := SignAuthenticatedContent(fixture, priv, WireFormatPrivateMessage,
		framingStubFramedContent(), encodedGroupContext)
	if err != nil {
		t.Fatalf("sign the message the VerifyAuthenticatedContent row reads: %v", err)
	}
	arguments["VerifyAuthenticatedContent.authContent"] = signed

	// the membership tag pair. The key is at the provider's own hash width rather than a
	// written down 32 octets, for framingStubGroupContext's reason: a key whose length was a
	// constant would be the same bytes over a provider whose KDF.Nh is 48, and the rows built
	// on it would state nothing about the width the caller is running at.
	membershipKey := bytes.Repeat([]byte{0x6b}, fixture.HashSize())
	arguments["ComputeMembershipTag.membershipKey"] = membershipKey
	arguments["ComputeMembershipTag.authContent"] = signed
	arguments["ComputeMembershipTag.groupContext"] = encodedGroupContext
	arguments["verifyMembershipTag.membershipKey"] = membershipKey
	arguments["verifyMembershipTag.authContent"] = signed
	arguments["verifyMembershipTag.groupContext"] = encodedGroupContext
	// the tag the verify row is handed has to be a REAL one over these arguments, for the
	// reason the signature above is: a base call that refused would leave every perturbation
	// below it comparing one refusal against another and reporting a verifier that reads none
	// of its inputs as observing all of them.
	tag, err := ComputeMembershipTag(fixture, membershipKey, signed, encodedGroupContext)
	if err != nil {
		t.Fatalf("compute the membership tag the verify row reads: %v", err)
	}
	arguments["verifyMembershipTag.tag"] = tag

	// section 6.2's seal and open. Both rows need a base call that SUCCEEDS, for the reason the
	// tag above is a real one: a base call that refused would leave every perturbation below it
	// comparing one refusal against another, and would report a construction that reads none of
	// its inputs as one that observes all of them.
	//
	// The content is a PROPOSAL and not the application message the rows above are built over,
	// because ValSem005 refuses an application message in a public frame -- that is the rule these
	// two exist to hold, not a shape they can be measured through. It is signed under
	// WireFormatPublicMessage, because the wire format is inside the signature preimage and the
	// seal refuses any other; and its sender is a member, because that is the arm section 6.2
	// gives a membership tag and therefore the only arm in which the membership key is read at all.
	sealContent := framingStubFramedContent()
	sealContent.ContentType = ContentTypeProposal
	sealContent.ApplicationData = nil
	sealContent.Proposal = &Proposal{ProposalType: ProposalTypeRemove, Remove: &Remove{Removed: 5}}
	sealed, err := SignAuthenticatedContent(fixture, priv, WireFormatPublicMessage,
		sealContent, encodedGroupContext)
	if err != nil {
		t.Fatalf("sign the message the SealPublicMessage row reads: %v", err)
	}
	arguments["SealPublicMessage.membershipKey"] = membershipKey
	arguments["SealPublicMessage.authContent"] = sealed
	arguments["SealPublicMessage.groupContext"] = encodedGroupContext
	message, err := SealPublicMessage(fixture, membershipKey, sealed, encodedGroupContext)
	if err != nil {
		t.Fatalf("seal the message the OpenPublicMessage row reads: %v", err)
	}
	pub, isKey := arguments["SignaturePublicKey"].(SignaturePublicKey)
	if !isKey {
		t.Fatal("the stub arguments hold no signature public key, so the open row has no resolver to build")
	}
	arguments["OpenPublicMessage.membershipKey"] = membershipKey
	arguments["OpenPublicMessage.message"] = message
	arguments["OpenPublicMessage.resolve"] = StaticSignatureKey(pub)
	arguments["OpenPublicMessage.groupContext"] = encodedGroupContext

	// section 6.3.2's seal and open. The open's row needs a base call that SUCCEEDS, for the
	// reason section 6.2's does: a base call that refused would leave every perturbation below
	// it comparing one refusal against another, and would report a construction that reads none
	// of its inputs as one that observes all of them.
	//
	// The secret is at the provider's own hash width rather than a written down 32 octets, for
	// the membership key's reason above, and because SenderDataKeyNonce refuses every other
	// length -- a refused call is a row that observed nothing.
	//
	// The CIPHERTEXT is exactly KDF.Nh, which is SenderDataKeyNonce.ciphertext's choice and is
	// made here for that argument's reason. RFC 9420 section 6.3.2 samples the first KDF.Nh
	// octets and no more, so a longer ciphertext would put the middle and last perturbations
	// outside the sample -- where an answer that does not move is the RFC working rather than a
	// stub, and would be reported as "does not read the ciphertext it was handed". Where the
	// sample boundary is held instead is
	// TestTheSenderDataSampleLocatesBothItsOffsetAndItsLength.
	senderDataSecret := bytes.Repeat([]byte{0x6d}, fixture.HashSize())
	senderDataCiphertext := bytes.Repeat([]byte{0x6e}, fixture.HashSize())
	senderDataHeader := &PrivateMessage{
		GroupId:           []byte{0x11, 0x12},
		Epoch:             4,
		ContentType:       ContentTypeApplication,
		AuthenticatedData: []byte{0x13},
	}
	// every field carries something, so a perturbation has a field to move and a seal that
	// dropped one is not hidden by that field being zero to begin with.
	senderData := &SenderData{LeafIndex: 2, Generation: 5, ReuseGuard: [4]byte{0x21, 0x22, 0x23, 0x24}}
	arguments["sealSenderData.senderDataSecret"] = senderDataSecret
	arguments["sealSenderData.senderData"] = senderData
	arguments["sealSenderData.header"] = senderDataHeader
	arguments["sealSenderData.ciphertext"] = senderDataCiphertext
	encryptedSenderData, err := sealSenderData(fixture, senderDataSecret, senderData,
		senderDataHeader, senderDataCiphertext)
	if err != nil {
		t.Fatalf("seal the sender data the openSenderData row reads: %v", err)
	}
	arguments["openSenderData.senderDataSecret"] = senderDataSecret
	arguments["openSenderData.encryptedSenderData"] = encryptedSenderData
	arguments["openSenderData.header"] = senderDataHeader
	arguments["openSenderData.ciphertext"] = senderDataCiphertext

	// section 6.3.1's three. The open's row needs a base call that SUCCEEDS, for the reason the
	// two sections above do: a base call that refused would leave every perturbation below it
	// comparing one refusal against another.
	//
	// The key source is PINNED at one generation, which is what makes these rows mean anything.
	// This gate calls each row twice over one script and requires the two answers to agree, and
	// a source that advanced would hand the second call a different key whatever its arguments
	// were -- a row satisfied by the ratchet rather than by the argument that moved. The real
	// secret tree is exercised where its advance is the subject, in
	// TestPrivateMessageRoundTripsThroughTheRealSecretTreeAtEveryBoundaryGeneration.
	//
	// The seal that builds the open's message draws its reuse guard from a provider of its OWN
	// rather than from the fixture. Four octets off the fixture's stream would move every
	// argument built after this one, which is a change to rows this task has nothing to do with.
	privateSealer := mustProviderOver(t, fixture.Suite(), providerStubStream(0x71))
	arguments["SealPrivateMessage.keys"] = framingPinnedKeySource(fixture, 0x4b, 0)
	arguments["SealPrivateMessage.senderDataSecret"] = senderDataSecret
	arguments["SealPrivateMessage.authContent"] = signed
	arguments["SealPrivateMessage.paddingSize"] = 16
	arguments["sealPrivateMessage.keys"] = framingPinnedKeySource(fixture, 0x4b, 0)
	arguments["sealPrivateMessage.senderDataSecret"] = senderDataSecret
	arguments["sealPrivateMessage.authContent"] = signed
	// non-zero padding, which is the whole reason this variant exists: zeros here would leave
	// the padding argument indistinguishable from an argument the seal ignored.
	arguments["sealPrivateMessage.padding"] = bytes.Repeat([]byte{0x71}, 16)
	// MASTER section 8.4.2 v2's three bodies, on the rows above's terms. sealPrivateMessageAt is
	// the seal that reports the generation it consumed and openPrivateMessageAt the open that
	// reports the generation it opened at; sealPrivateMessageBound is the first under the S3 pin,
	// and its boundGeneration is the generation this pinned key source actually hands out -- 0 --
	// because a base call that refused would leave every perturbation below it comparing one
	// refusal against another, which is this gate's own rule.
	arguments["sealPrivateMessageAt.keys"] = framingPinnedKeySource(fixture, 0x4b, 0)
	arguments["sealPrivateMessageAt.senderDataSecret"] = senderDataSecret
	arguments["sealPrivateMessageAt.authContent"] = signed
	arguments["sealPrivateMessageAt.padding"] = bytes.Repeat([]byte{0x71}, 16)
	arguments["sealPrivateMessageBound.keys"] = framingPinnedKeySource(fixture, 0x4b, 0)
	arguments["sealPrivateMessageBound.senderDataSecret"] = senderDataSecret
	arguments["sealPrivateMessageBound.authContent"] = signed
	arguments["sealPrivateMessageBound.padding"] = bytes.Repeat([]byte{0x71}, 16)
	arguments["sealPrivateMessageBound.boundGeneration"] = uint32(0)
	privateMessage, err := SealPrivateMessage(privateSealer, framingPinnedKeySource(fixture, 0x4b, 0),
		senderDataSecret, signed, 16)
	if err != nil {
		t.Fatalf("seal the message the OpenPrivateMessage row reads: %v", err)
	}
	arguments["OpenPrivateMessage.keys"] = framingPinnedKeySource(fixture, 0x4b, 0)
	arguments["OpenPrivateMessage.senderDataSecret"] = senderDataSecret
	arguments["OpenPrivateMessage.message"] = privateMessage
	arguments["OpenPrivateMessage.resolve"] = StaticSignatureKey(pub)
	arguments["OpenPrivateMessage.groupContext"] = encodedGroupContext
	arguments["openPrivateMessageAt.keys"] = framingPinnedKeySource(fixture, 0x4b, 0)
	arguments["openPrivateMessageAt.senderDataSecret"] = senderDataSecret
	arguments["openPrivateMessageAt.message"] = privateMessage
	arguments["openPrivateMessageAt.resolve"] = StaticSignatureKey(pub)
	arguments["openPrivateMessageAt.groupContext"] = encodedGroupContext

	// the pre-ratchet peek, over the SAME message the open row reads, marshalled. It takes the
	// marshalled form rather than the structure because that is what its one caller holds -- an
	// application record's ct_body plaintext is a marshalled MLSMessage -- and a base call that
	// failed to parse would leave every perturbation below it comparing one refusal against
	// another, which is this gate's own rule two sections up.
	marshalledPrivateMessage, err := MarshalMLSMessage(&MLSMessage{
		Version:        ProtocolVersionMls10,
		WireFormat:     WireFormatPrivateMessage,
		PrivateMessage: privateMessage,
	})
	if err != nil {
		t.Fatalf("marshal the message the PeekPrivateMessageSender row reads: %v", err)
	}
	arguments["PeekPrivateMessageSender.senderDataSecret"] = senderDataSecret
	arguments["PeekPrivateMessageSender.marshalled"] = marshalledPrivateMessage
}

// providerPeekedMessagePerturbations moves bytes of PeekPrivateMessageSender's marshalled message,
// and moves them only where that call READS.
//
// THE RULE EXISTS BECAUSE THE READ REGION IS A PREFIX AND SAYING SO IS THE POINT. The peek parses
// the MLSMessage, opens the sender data and returns the cleartext authenticated_data; the content
// ciphertext reaches it only through RFC 9420 section 6.3.2's sample, which is its first KDF.Nh
// octets. So the default byte rule -- first, middle and last of the whole message -- moves two
// positions that sit inside the content ciphertext's tail, and the peek answers the same thing for
// them because it is a PEEK and not an open. Reported under the default rule that reads "does not
// read the marshalled it was handed", which is false of the region it is about.
//
// WHAT IS MOVED INSTEAD is every position up to and including the ciphertext's own length prefix:
// the version, the wire format, the group id, the epoch, the content type, the authenticated_data
// and the encrypted sender data. Every one of those changes the answer -- the first four and the
// content type are in the sender data's AAD, the authenticated_data IS the answer, and the sender
// data is the ciphertext the open takes -- so the property this states is the whole of what the
// peek claims to read. The boundary is DERIVED from the argument, by parsing it and subtracting
// the ciphertext's own length, rather than written down: a message that grew a field moves the
// boundary with it.
//
// It is SenderDataKeyNonce.ciphertext's rule one layer out and for the same reason: an argument a
// construction reads a bounded prefix of is one whose perturbations belong inside that prefix,
// and where the boundary itself is held is TestTheSenderDataSampleLocatesBothItsOffsetAndItsLength.
func providerPeekedMessagePerturbations(t *testing.T, operation string, parameter providerParameter,
	argument reflect.Value) ([]providerPerturbation, bool) {

	t.Helper()
	if operation != "PeekPrivateMessageSender" || parameter.name != "marshalled" {
		return nil, false
	}
	octets, isBytes := argument.Interface().([]byte)
	if !isBytes {
		t.Fatalf("the base argument for %s.%s is a %s rather than octets", operation, parameter.name, argument.Type())
	}
	parsed, err := ParseMLSMessage(octets)
	if err != nil || parsed.PrivateMessage == nil {
		t.Fatalf("the base argument for %s.%s does not parse as a PrivateMessage, so this rule cannot find its read region: %v",
			operation, parameter.name, err)
	}
	read := len(octets) - len(parsed.PrivateMessage.Ciphertext)
	if read <= 0 || len(octets) <= read {
		t.Fatalf("the base argument for %s.%s is %d octets and its ciphertext is %d, so the read region is not a proper prefix",
			operation, parameter.name, len(octets), len(parsed.PrivateMessage.Ciphertext))
	}
	moved := []providerPerturbation{}
	for _, at := range perturbedPositions(read) {
		value := append([]byte(nil), octets...)
		value[at] ^= 0xff
		moved = append(moved, providerPerturbation{
			where: "byte " + strconv.Itoa(at) + " of the " + strconv.Itoa(read) + " this call reads",
			value: reflect.ValueOf(value),
		})
	}
	return moved, true
}

// providerPublicMessagePerturbations moves the epoch of the message being opened.
//
// It is providerAuthenticatedContentPerturbations' rule and it is here for that rule's reason,
// with one more consequence: BOTH authenticators travel with the value unchanged, so what this
// asks is whether the open rebuilt both preimages out of the message it was handed. An open that
// checked either authenticator against anything but a preimage over these bytes answers the same
// thing twice.
func providerPublicMessagePerturbations(t *testing.T, operation string, parameter providerParameter,
	argument reflect.Value) []providerPerturbation {

	t.Helper()
	base := argument.Interface().(*PublicMessage)
	if base == nil {
		t.Fatalf("the base argument for %s.%s is a nil public message, so perturbing it changes nothing",
			operation, parameter.name)
	}
	moved := *base
	moved.Content.Epoch++
	return []providerPerturbation{{where: "epoch one higher", value: reflect.ValueOf(&moved)}}
}

// providerSignatureKeyResolverPerturbations answers a resolver that hands back a DIFFERENT key.
//
// The resolver is the one argument of the open that is not bytes, and what it decides is whose
// signature the message is checked against -- the whole of ValSem010 at this layer. An open whose
// answer did not reach the verification would accept any member's message under any other
// member's leaf, so what is moved is the ANSWER and not the shape: the base resolver's own key
// with a byte flipped, which is a key nothing ever signed with.
//
// The positions come off the key's length rather than being written down, which is
// perturbedPositions' rule: moving only the last byte states that the last byte is read.
func providerSignatureKeyResolverPerturbations(t *testing.T, operation string, parameter providerParameter,
	argument reflect.Value) []providerPerturbation {

	t.Helper()
	base, isResolver := argument.Interface().(SignatureKeyResolver)
	if !isResolver || base == nil {
		t.Fatalf("the base argument for %s.%s is not a resolver, so perturbing it changes nothing",
			operation, parameter.name)
	}
	answered, err := base(Sender{SenderType: SenderTypeMember})
	if err != nil {
		t.Fatalf("the base resolver for %s.%s refused: %v", operation, parameter.name, err)
	}
	if len(answered) == 0 {
		t.Fatalf("the base resolver for %s.%s answers no key, so a flipped one is not a different one",
			operation, parameter.name)
	}
	moved := []providerPerturbation{}
	for _, at := range perturbedPositions(len(answered)) {
		flipped := append([]byte(nil), answered...)
		flipped[at] ^= 0xff
		moved = append(moved, providerPerturbation{
			where: fmt.Sprintf("the key it answers, byte %d of %d", at, len(answered)),
			value: reflect.ValueOf(StaticSignatureKey(SignaturePublicKey(flipped))),
		})
	}
	return moved
}

// providerFramedContentPerturbations moves the epoch of a framed content and nothing else.
//
// The epoch for the reason the group context's own rule moves the epoch: it is the field two
// messages of one group differ in, it is inside the preimage on every path, and a construction
// that dropped the content out of what it signed answers identically here while one that kept
// it cannot. The copy is made field by field through a fresh value rather than by taking the
// address of a struct copy, so this perturbation cannot write through into the base argument
// every other row is built from.
func providerFramedContentPerturbations(t *testing.T, operation string, parameter providerParameter,
	argument reflect.Value) []providerPerturbation {

	t.Helper()
	base := argument.Interface().(*FramedContent)
	if base == nil {
		t.Fatalf("the base argument for %s.%s is a nil framed content, so perturbing it changes nothing",
			operation, parameter.name)
	}
	moved := *base
	moved.Epoch++
	return []providerPerturbation{{where: "epoch one higher", value: reflect.ValueOf(&moved)}}
}

// providerAuthenticatedContentPerturbations moves the epoch of the message being verified, for
// the reason above and with one extra consequence worth stating: the signature travels with the
// value unchanged, so what this asks is whether the verifier rebuilt its preimage out of the
// message it was handed. A verifier that had cached, or that compared the signature against
// anything but a preimage over these bytes, answers the same thing twice.
func providerAuthenticatedContentPerturbations(t *testing.T, operation string, parameter providerParameter,
	argument reflect.Value) []providerPerturbation {

	t.Helper()
	base := argument.Interface().(*AuthenticatedContent)
	if base == nil {
		t.Fatalf("the base argument for %s.%s is a nil authenticated content, so perturbing it changes nothing",
			operation, parameter.name)
	}
	moved := *base
	moved.Content.Epoch++
	return []providerPerturbation{{where: "epoch one higher", value: reflect.ValueOf(&moved)}}
}

// providerSenderDataPerturbations moves the GENERATION of the sender data being sealed.
//
// The generation and not the leaf index, for one reason that is worth writing down: the two are
// adjacent uint32s in section 6.3.2's structure, so a codec that swapped them agrees with itself
// and a perturbation of either moves the answer just the same. What this row asks is only whether
// the seal put the sender data into the plaintext AT ALL -- a seal that sealed a constant, or that
// sealed its header twice, answers identically here and one that carried the caller's value
// cannot. Which field goes where is TestSenderDataRoundTrip's golden.
func providerSenderDataPerturbations(t *testing.T, operation string, parameter providerParameter,
	argument reflect.Value) []providerPerturbation {

	t.Helper()
	base := argument.Interface().(*SenderData)
	if base == nil {
		t.Fatalf("the base argument for %s.%s is a nil sender data, so perturbing it changes nothing",
			operation, parameter.name)
	}
	moved := *base
	moved.Generation++
	return []providerPerturbation{{where: "generation one higher", value: reflect.ValueOf(&moved)}}
}

// providerPrivateMessagePerturbations moves the epoch of the cleartext header, which is the field
// two messages of one group differ in and is inside section 6.3.2's associated data.
//
// The header is not encrypted and is not the plaintext: what a seal or an open does with it is
// build the AAD, so an operation that dropped it out of that AAD answers identically here and one
// that kept it cannot. The copy is a struct copy whose byte fields are read and never written, so
// this perturbation cannot reach into the base argument every other row is built from.
func providerPrivateMessagePerturbations(t *testing.T, operation string, parameter providerParameter,
	argument reflect.Value) []providerPerturbation {

	t.Helper()
	base := argument.Interface().(*PrivateMessage)
	if base == nil {
		t.Fatalf("the base argument for %s.%s is a nil private message header, so perturbing it changes nothing",
			operation, parameter.name)
	}
	moved := *base
	moved.Epoch++
	return []providerPerturbation{{where: "epoch one higher", value: reflect.ValueOf(&moved)}}
}

// providerMessageKeySourcePerturbations answers a key source that derives from a DIFFERENT seed.
//
// The seed and not the generation, because what this row asks is whether the operation used the
// key material it was handed AT ALL: a seal that derived a key of its own, or that sealed under a
// constant, answers identically here and one that read its source cannot. Everything else about
// the source is unchanged -- same widths, same generation, same erase behaviour -- so a difference
// in the answer is attributable to the key material and to nothing else.
func providerMessageKeySourcePerturbations(t *testing.T, operation string, parameter providerParameter,
	argument reflect.Value) []providerPerturbation {

	t.Helper()
	base, isSource := argument.Interface().(*framingKeySource)
	if !isSource || base == nil {
		t.Fatalf("the base argument for %s.%s is not this file's key source, so perturbing it changes nothing",
			operation, parameter.name)
	}
	moved := framingPinnedKeySource(base.crypto, base.seed^0xff, base.start)
	return []providerPerturbation{{
		where: "a source seeded differently",
		value: reflect.ValueOf(moved),
	}}
}

// ---------------------------------------------------------------------------
// the preimage
// ---------------------------------------------------------------------------

// emptyByteSpelling is one way a caller can hand this package a byte slice of length zero.
type emptyByteSpelling struct {
	what  string
	value []byte
}

// emptyByteSpellings is EVERY such way, which is three and not the one a guard's author
// pictures.
//
// A rule about an absent value has to be written on the LENGTH, and these three separate it
// from the two things it can be written on by mistake. nil is the zero value a fresh struct
// field carries; the empty literal is non nil with no capacity, which is what syntax.Marshal
// of nothing and a caller's []byte{} both answer; and a slice re-sliced to nothing out of a
// longer buffer is non nil WITH capacity, which is what a decoder hands back after reading an
// empty opaque<V>. A guard spelled == nil accepts the last two and one spelled on cap accepts
// the first two; only one spelled on len refuses all three.
//
// Measured rather than supposed. With the preimage's binding guard rewritten from
// len(self.GroupContext) == 0 to self.GroupContext == nil -- which signs an epoch UNBOUND
// preimage for a member handed an empty non nil context, the signature valid in every epoch
// of the group that senderBindsGroupContext names as the most expensive omission available
// here -- and with ValSem009's tag guard rewritten the same way, this package's whole suite
// passed both times.
func emptyByteSpellings() []emptyByteSpelling {
	return []emptyByteSpelling{
		{what: "nil", value: nil},
		{what: "the empty literal", value: []byte{}},
		{what: "re-sliced to nothing out of a longer buffer", value: make([]byte, 0, 8)},
	}
}

// framingTestGroupContext is a real serialized GroupContext, built the way every caller must
// build one: syntax.Marshal over the key schedule's structure. The preimage inlines these bytes
// verbatim, with no length prefix.
func framingTestGroupContext(t *testing.T) []byte {
	t.Helper()
	encoded, err := syntax.Marshal(&GroupContext{
		Version:                 ProtocolVersionMls10,
		CipherSuite:             CipherSuiteX25519ChaCha20Sha256Ed25519,
		GroupId:                 []byte{0x01, 0x02},
		Epoch:                   4,
		TreeHash:                bytes.Repeat([]byte{0xc0}, 32),
		ConfirmedTranscriptHash: bytes.Repeat([]byte{0xee}, 32),
	})
	if err != nil {
		t.Fatalf("group context: %v", err)
	}
	return encoded
}

func framingTestMemberContent() *FramedContent {
	return &FramedContent{
		GroupId:           []byte{0x01, 0x02},
		Epoch:             4,
		Sender:            Sender{SenderType: SenderTypeMember, LeafIndex: 1},
		AuthenticatedData: []byte{0x09},
		ContentType:       ContentTypeApplication,
		ApplicationData:   []byte("payload"),
	}
}

func framingTestProposalContent() *FramedContent {
	content := framingTestMemberContent()
	content.ContentType = ContentTypeProposal
	content.ApplicationData = nil
	content.Proposal = &Proposal{ProposalType: ProposalTypeRemove, Remove: &Remove{Removed: 3}}
	return content
}

func framingTestCommitContent() *FramedContent {
	content := framingTestMemberContent()
	content.ContentType = ContentTypeCommit
	content.ApplicationData = nil
	content.Commit = &Commit{}
	return content
}

// TestFramedContentTBSInlinesGroupContextWithoutLengthPrefix holds the layout against an
// encoder written out here rather than against the one under test.
//
// The group context is the field this exists for. It is a STRUCT in RFC 9420's presentation
// language and not an opaque<V>, so it carries no length prefix -- and a preimage that added
// one is a signature this package verifies against itself perfectly, since both halves would
// add it, and every other implementation rejects. Nothing round trips through a
// FramedContentTBS, so no symmetry property in this package can see the substitution.
//
// The table runs every content type and both group context arms rather than one row, because
// the layout is where the arms differ: the version and wire format are two uint16s, the content
// is whatever its own codec writes, and the context is either the whole of the tail or absent.
//
// The byte equality is the WHOLE of each row and nothing stands after it, which is a deletion
// from the version this task was handed rather than an omission. That version closed with
// bytes.HasSuffix(tbs, groupContext) after the equality had already passed, and an assertion
// reached only once the bytes are known to equal an encoding that ends in those same bytes is
// an assertion no implementation can fail -- decoration that reads as a second check. The same
// went for every restatement tried here: once a preimage is compared against a full independent
// encoding, every property of it is settled.
//
// What that equality cannot do is notice a hand written encoder in this file that is wrong the
// same way the implementation is wrong. Nothing in this package can; what can is
// TestTheFramedContentSignatureIsTheOneMlswgPublished, which compares against signatures
// somebody else made. The two are meant to be read together -- this one localises a failure to
// a field, and that one is the reason to believe the layout at all.
func TestFramedContentTBSInlinesGroupContextWithoutLengthPrefix(t *testing.T) {
	groupContext := framingTestGroupContext(t)
	external := framingTestProposalContent()
	external.Sender = Sender{SenderType: SenderTypeExternal, SenderIndex: 0}
	for _, testCase := range []struct {
		name         string
		wireFormat   WireFormat
		content      *FramedContent
		groupContext []byte
	}{
		{name: "a member's application message", wireFormat: WireFormatPrivateMessage,
			content: framingTestMemberContent(), groupContext: groupContext},
		{name: "a member's proposal", wireFormat: WireFormatPublicMessage,
			content: framingTestProposalContent(), groupContext: groupContext},
		{name: "a member's commit", wireFormat: WireFormatPublicMessage,
			content: framingTestCommitContent(), groupContext: groupContext},
		{name: "an external sender's proposal", wireFormat: WireFormatPublicMessage,
			content: external, groupContext: nil},
	} {
		tbs, err := FramedContentTBSBytes(testCase.wireFormat, testCase.content, testCase.groupContext)
		if err != nil {
			t.Errorf("%s: tbs: %v", testCase.name, err)
			continue
		}
		w := syntax.NewWriter()
		w.WriteUint16(uint16(ProtocolVersionMls10))
		w.WriteUint16(uint16(testCase.wireFormat))
		if err := testCase.content.MarshalMLS(w); err != nil {
			t.Errorf("%s: content: %v", testCase.name, err)
			continue
		}
		w.WriteRaw(testCase.groupContext)
		want, err := w.Bytes()
		if err != nil {
			t.Errorf("%s: bytes: %v", testCase.name, err)
			continue
		}
		if !bytes.Equal(tbs, want) {
			t.Errorf("%s: tbs %x, want %x", testCase.name, tbs, want)
		}
	}
}

func TestFramedContentTBSOmitsGroupContextForExternalSender(t *testing.T) {
	content := framingTestProposalContent()
	content.Sender = Sender{SenderType: SenderTypeExternal, SenderIndex: 0}

	tbs, err := FramedContentTBSBytes(WireFormatPublicMessage, content, nil)
	if err != nil {
		t.Fatalf("tbs: %v", err)
	}
	if bytes.Contains(tbs, framingTestGroupContext(t)) {
		t.Fatal("group context present for an external sender")
	}
	_, err = FramedContentTBSBytes(WireFormatPublicMessage, content, framingTestGroupContext(t))
	if !errors.Is(err, ErrUnexpectedGroupContext) {
		t.Fatalf("got %v, want ErrUnexpectedGroupContext", err)
	}
}

// Over every spelling of an absent group context rather than the nil one alone, which is what
// this test's name claims and what a guard written on the pointer does not hold: a member
// handed an empty non nil context would sign a preimage carrying no epoch at all.
func TestFramedContentTBSRequiresGroupContextForMember(t *testing.T) {
	for _, empty := range emptyByteSpellings() {
		_, err := FramedContentTBSBytes(WireFormatPrivateMessage, framingTestMemberContent(), empty.value)
		if !errors.Is(err, ErrMissingGroupContext) {
			t.Fatalf("a group context that is %s: got %v, want ErrMissingGroupContext", empty.what, err)
		}
	}
}

// rfc9420SendersThatBindTheGroupContext is RFC 9420 section 6.1's select on sender_type, keyed
// by the RFC's own spelling of each arm.
//
// Read off the RFC rather than off senderBindsGroupContext, which is the only thing that can
// make the test below an assertion: a table derived from the function under test agrees with
// whatever that function does.
var rfc9420SendersThatBindTheGroupContext = map[string]bool{
	"member":              true,
	"external":            false,
	"new_member_proposal": false,
	"new_member_commit":   true,
}

// TestEverySenderTypeBindsTheGroupContextSection61GivesIt joins that table against the registry
// this package declares, in both directions, and then against the BEHAVIOUR rather than against
// the switch.
//
// Derived rather than listed for the reason every sweep in framing_test.go is: a fifth sender
// type declared and left out of a hand written list is a sender type nothing here judges, and
// this is the rule that decides whether a signature is bound to an epoch at all. An omission
// here is a message replayable into every later epoch of the group.
func TestEverySenderTypeBindsTheGroupContextSection61GivesIt(t *testing.T) {
	derived := registryConstantsOfType(t, "SenderType")
	if len(derived) == 0 {
		t.Fatal("no SenderType constant was derived, so this gate runs over the empty set")
	}
	measured := map[string]bool{}
	for _, name := range slices.Sorted(maps.Keys(derived)) {
		senderType := SenderType(derived[name])
		binds, err := senderBindsGroupContext(senderType)
		if err != nil {
			t.Errorf("%s is a registered sender type and senderBindsGroupContext refused it: %v", name, err)
			continue
		}
		measured[rfcNameOfFramingConstant("SenderType", name)] = binds
		// and the same answer read off the preimage rather than off the helper, so a
		// MarshalMLS that stopped consulting it is reported here as well
		content := framingTestProposalContent()
		content.Sender = Sender{SenderType: senderType}
		_, withContext := FramedContentTBSBytes(WireFormatPublicMessage, content, framingTestGroupContext(t))
		if binds && withContext != nil {
			t.Errorf("%s binds the group context and the preimage refused one: %v", name, withContext)
		}
		if !binds && !errors.Is(withContext, ErrUnexpectedGroupContext) {
			t.Errorf("%s binds no group context and the preimage answered %v to one", name, withContext)
		}
		// the absent direction over EVERY spelling of absent rather than over nil alone.
		// The two arms do not cost the same thing when a guard is spelled on the pointer:
		// a sender that binds the context and was handed an empty non nil one would sign a
		// preimage with no epoch in it, which is a signature valid in every epoch this
		// group ever has.
		for _, empty := range emptyByteSpellings() {
			_, withoutContext := FramedContentTBSBytes(WireFormatPublicMessage, content, empty.value)
			if binds && !errors.Is(withoutContext, ErrMissingGroupContext) {
				t.Errorf("%s binds the group context and the preimage answered %v to one that is %s",
					name, withoutContext, empty.what)
			}
			if !binds && withoutContext != nil {
				t.Errorf("%s binds no group context and the preimage refused one that is %s: %v",
					name, empty.what, withoutContext)
			}
		}
	}
	if !maps.Equal(measured, rfc9420SendersThatBindTheGroupContext) {
		t.Errorf("this package binds the group context for\n %v\nand RFC 9420 section 6.1's select gives\n %v",
			measured, rfc9420SendersThatBindTheGroupContext)
	}
}

// ---------------------------------------------------------------------------
// sign and verify
// ---------------------------------------------------------------------------

// framingSigned is one signed message together with everything needed to check it, built once
// per test so that each test varies one thing rather than declaring a slightly different value.
type framingSigned struct {
	crypto       CryptoProvider
	priv         SignaturePrivateKey
	pub          SignaturePublicKey
	groupContext []byte
	authContent  *AuthenticatedContent
}

func framingSignedMemberMessage(t *testing.T) framingSigned {
	t.Helper()
	crypto := newTestCrypto(t)
	priv, pub, err := crypto.SignatureKeyPair()
	if err != nil {
		t.Fatalf("key pair: %v", err)
	}
	groupContext := framingTestGroupContext(t)
	authContent, err := SignAuthenticatedContent(crypto, priv, WireFormatPrivateMessage,
		framingTestMemberContent(), groupContext)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return framingSigned{crypto: crypto, priv: priv, pub: pub,
		groupContext: groupContext, authContent: authContent}
}

func TestSignAndVerifyAuthenticatedContent(t *testing.T) {
	signed := framingSignedMemberMessage(t)
	if err := VerifyAuthenticatedContent(signed.crypto, signed.pub, signed.authContent, signed.groupContext); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

// TestSignAuthenticatedContentLeavesTheConfirmationTagToItsCaller states the half of this
// constructor's contract that has no signature in it.
//
// The empty confirmation tag is the SHAPE and not an omission: section 8.2 takes the confirmed
// transcript hash over this signature and the tag is a MAC over that hash, so the tag cannot
// exist yet. A constructor that filled one in would be filling in a value derived from a
// transcript that has not been advanced, which every peer would compute differently.
func TestSignAuthenticatedContentLeavesTheConfirmationTagToItsCaller(t *testing.T) {
	signed := framingSignedMemberMessage(t)
	if len(signed.authContent.Auth.ConfirmationTag) != 0 {
		t.Errorf("the signer produced a confirmation tag %x", signed.authContent.Auth.ConfirmationTag)
	}
	if signed.authContent.WireFormat != WireFormatPrivateMessage {
		t.Errorf("the signer carried the wire format %d and it was handed %d",
			signed.authContent.WireFormat, WireFormatPrivateMessage)
	}
	if !reflect.DeepEqual(&signed.authContent.Content, framingTestMemberContent()) {
		t.Errorf("the signer carried a content that is not the one it was handed")
	}
	if len(signed.authContent.Auth.Signature) == 0 {
		t.Error("the signer produced no signature at all")
	}
}

// TestVerifyRefusesEveryFlippedBitOfTheSignature sweeps the signature bit by bit, derived over
// its own length rather than sampled.
//
// The sampling is the property. A verifier that compared the first byte of a 64 byte signature
// and nothing else passes a test that flips bit zero, and this project has already shipped a
// tag verifier of exactly that shape past a test of exactly that shape. Every bit of every byte
// is a forgery that must be refused, and the count is asserted so a sweep that stopped sweeping
// is loud rather than green.
func TestVerifyRefusesEveryFlippedBitOfTheSignature(t *testing.T) {
	signed := framingSignedMemberMessage(t)
	refused := 0
	for at := range signed.authContent.Auth.Signature {
		for bit := 0; bit < 8; bit++ {
			forged := *signed.authContent
			forged.Auth.Signature = bytes.Clone(signed.authContent.Auth.Signature)
			forged.Auth.Signature[at] ^= 1 << bit
			err := VerifyAuthenticatedContent(signed.crypto, signed.pub, &forged, signed.groupContext)
			if !errors.Is(err, errBadSignature) {
				t.Errorf("bit %d of byte %d flipped: got %v, want the ValSem010 sentinel", bit, at, err)
				continue
			}
			refused++
		}
	}
	if want := 8 * len(signed.authContent.Auth.Signature); refused != want {
		t.Fatalf("%d of %d single bit forgeries were refused", refused, want)
	}
	if refused == 0 {
		t.Fatal("the signature is empty, so this sweep flipped nothing")
	}
}

// TestVerifyRefusesEverySignatureLengthButItsOwn sweeps the length, derived the same way.
//
// A length mismatch is a REFUSAL and never a panic and never a short comparison. The empty and
// nil cases are the zero value of a FramedContentAuthData -- the state a freshly allocated one
// is in -- and this project shipped a bypass whose whole shape was an all zero authenticator
// reaching a comparison that accepted it.
func TestVerifyRefusesEverySignatureLengthButItsOwn(t *testing.T) {
	signed := framingSignedMemberMessage(t)
	signature := signed.authContent.Auth.Signature
	lengths := []struct {
		what string
		sig  []byte
	}{
		{what: "nil", sig: nil},
		{what: "empty", sig: []byte{}},
		{what: "one byte longer", sig: append(bytes.Clone(signature), 0x00)},
		{what: "twice as long", sig: append(bytes.Clone(signature), signature...)},
	}
	for cut := 0; cut < len(signature); cut++ {
		lengths = append(lengths, struct {
			what string
			sig  []byte
		}{what: fmt.Sprintf("truncated to %d of %d", cut, len(signature)), sig: bytes.Clone(signature[:cut])})
	}
	for _, one := range lengths {
		forged := *signed.authContent
		forged.Auth.Signature = one.sig
		if err := VerifyAuthenticatedContent(signed.crypto, signed.pub, &forged, signed.groupContext); !errors.Is(err, errBadSignature) {
			t.Errorf("%s: got %v, want the ValSem010 sentinel", one.what, err)
		}
	}
	if len(lengths) != 4+len(signature) {
		t.Fatalf("the sweep built %d lengths for a %d byte signature", len(lengths), len(signature))
	}
}

// framingAcceptingProvider is a provider whose VerifyWithLabel accepts everything.
//
// It exists to ask one question no real provider can be asked: does the framing layer's own
// refusal of the zero authenticator depend on the crypto agreeing? The three bypasses this
// project has shipped were all a caller trusting a layer underneath it, and the answer here has
// to be no.
type framingAcceptingProvider struct {
	CryptoProvider
}

func (self *framingAcceptingProvider) VerifyWithLabel(pub SignaturePublicKey, label string,
	content []byte, sig []byte) error {
	return nil
}

// TestVerifyRefusesTheZeroAuthenticatorWhateverTheProviderSays holds the empty signature check
// to being the framing layer's own.
//
// The control matters as much as the assertion and runs first: over the same accepting provider
// a signature of the right LENGTH and entirely wrong content is accepted, which is what says the
// provider really is lenient. Without it a refusal here would be indistinguishable from a real
// provider doing the work.
func TestVerifyRefusesTheZeroAuthenticatorWhateverTheProviderSays(t *testing.T) {
	signed := framingSignedMemberMessage(t)
	lenient := &framingAcceptingProvider{CryptoProvider: signed.crypto}

	forged := *signed.authContent
	forged.Auth.Signature = bytes.Repeat([]byte{0xaa}, len(signed.authContent.Auth.Signature))
	if err := VerifyAuthenticatedContent(lenient, signed.pub, &forged, signed.groupContext); err != nil {
		t.Fatalf("the accepting provider refused a forged signature (%v), so it is not lenient and the assertion below states nothing", err)
	}

	for _, one := range []struct {
		what string
		sig  []byte
	}{{what: "nil", sig: nil}, {what: "empty", sig: []byte{}}} {
		zero := *signed.authContent
		zero.Auth.Signature = one.sig
		if err := VerifyAuthenticatedContent(lenient, signed.pub, &zero, signed.groupContext); !errors.Is(err, errBadSignature) {
			t.Errorf("%s signature over a provider that accepts everything: got %v, want the ValSem010 sentinel",
				one.what, err)
		}
	}
}

// ---------------------------------------------------------------------------
// every field of the preimage
// ---------------------------------------------------------------------------

// framingPreimageInput is the three arguments a FramedContentTBS is built out of.
type framingPreimageInput struct {
	wireFormat   WireFormat
	content      *FramedContent
	groupContext []byte
}

// framingFieldMoves is one move per FIELD of the preimage, keyed by the struct and field the
// move is about.
//
// Every entry answers a base and a moved input that differ in that field alone, except where
// the wire format makes that impossible and the entry says so. This is the table the sweep
// below joins against the two structures by reflection, in both directions, so a field added to
// either one has no entry and fails rather than being left out of the sweep.
//
// This is the shape p5 task 6 found for LeafNodeTBS: omit group_id or leaf_index and everything
// still round trips, while a leaf lifted out of another group verifies. The equivalent here is
// worse, because a field dropped from this preimage is a message another member can replay.
var framingFieldMoves = map[string]func(t *testing.T) (framingPreimageInput, framingPreimageInput){
	"framedContentTBS.WireFormat": func(t *testing.T) (framingPreimageInput, framingPreimageInput) {
		return framingPreimageInput{WireFormatPrivateMessage, framingTestMemberContent(), framingTestGroupContext(t)},
			framingPreimageInput{WireFormatPublicMessage, framingTestMemberContent(), framingTestGroupContext(t)}
	},
	// the whole content, moved as one. Its own fields are moved one at a time below; this
	// entry is what says the field EXISTS in the preimage at all, which is the reading that
	// survives a codec that stopped writing any of it.
	"framedContentTBS.Content": func(t *testing.T) (framingPreimageInput, framingPreimageInput) {
		moved := framingTestMemberContent()
		moved.ApplicationData = []byte("a different payload entirely")
		return framingPreimageInput{WireFormatPrivateMessage, framingTestMemberContent(), framingTestGroupContext(t)},
			framingPreimageInput{WireFormatPrivateMessage, moved, framingTestGroupContext(t)}
	},
	// the epoch binding. A preimage that dropped it is a signature valid in every epoch of the
	// group, which is the single most expensive omission available here.
	"framedContentTBS.GroupContext": func(t *testing.T) (framingPreimageInput, framingPreimageInput) {
		moved := bytes.Clone(framingTestGroupContext(t))
		moved[len(moved)-1] ^= 0xff
		return framingPreimageInput{WireFormatPrivateMessage, framingTestMemberContent(), framingTestGroupContext(t)},
			framingPreimageInput{WireFormatPrivateMessage, framingTestMemberContent(), moved}
	},
	"FramedContent.GroupId": func(t *testing.T) (framingPreimageInput, framingPreimageInput) {
		moved := framingTestMemberContent()
		moved.GroupId = []byte{0x01, 0x03}
		return framingPreimageInput{WireFormatPrivateMessage, framingTestMemberContent(), framingTestGroupContext(t)},
			framingPreimageInput{WireFormatPrivateMessage, moved, framingTestGroupContext(t)}
	},
	"FramedContent.Epoch": func(t *testing.T) (framingPreimageInput, framingPreimageInput) {
		moved := framingTestMemberContent()
		moved.Epoch++
		return framingPreimageInput{WireFormatPrivateMessage, framingTestMemberContent(), framingTestGroupContext(t)},
			framingPreimageInput{WireFormatPrivateMessage, moved, framingTestGroupContext(t)}
	},
	// the sender, which for a member is the leaf index -- the field that says WHO signed. A
	// preimage that dropped it is one member's signature accepted as another's.
	"FramedContent.Sender": func(t *testing.T) (framingPreimageInput, framingPreimageInput) {
		moved := framingTestMemberContent()
		moved.Sender = Sender{SenderType: SenderTypeMember, LeafIndex: 9}
		return framingPreimageInput{WireFormatPrivateMessage, framingTestMemberContent(), framingTestGroupContext(t)},
			framingPreimageInput{WireFormatPrivateMessage, moved, framingTestGroupContext(t)}
	},
	"FramedContent.AuthenticatedData": func(t *testing.T) (framingPreimageInput, framingPreimageInput) {
		moved := framingTestMemberContent()
		moved.AuthenticatedData = []byte{0x0a}
		return framingPreimageInput{WireFormatPrivateMessage, framingTestMemberContent(), framingTestGroupContext(t)},
			framingPreimageInput{WireFormatPrivateMessage, moved, framingTestGroupContext(t)}
	},
	// the content type, moved together with the arm it selects, because on the wire the two are
	// one thing: an application message with a proposal beside it does not encode at all. What
	// this row states is therefore that the discriminant and its arm are both in the preimage,
	// and the two rows below separate the arms from each other.
	"FramedContent.ContentType": func(t *testing.T) (framingPreimageInput, framingPreimageInput) {
		return framingPreimageInput{WireFormatPublicMessage, framingTestMemberContent(), framingTestGroupContext(t)},
			framingPreimageInput{WireFormatPublicMessage, framingTestProposalContent(), framingTestGroupContext(t)}
	},
	"FramedContent.ApplicationData": func(t *testing.T) (framingPreimageInput, framingPreimageInput) {
		moved := framingTestMemberContent()
		moved.ApplicationData = []byte("payloae")
		return framingPreimageInput{WireFormatPrivateMessage, framingTestMemberContent(), framingTestGroupContext(t)},
			framingPreimageInput{WireFormatPrivateMessage, moved, framingTestGroupContext(t)}
	},
	// the proposal arm, moved inside itself: two removals of two different leaves. A preimage
	// that carried the discriminant and dropped the body is a signature over "some proposal",
	// which every member of the group could replay as a removal of anybody.
	"FramedContent.Proposal": func(t *testing.T) (framingPreimageInput, framingPreimageInput) {
		moved := framingTestProposalContent()
		moved.Proposal = &Proposal{ProposalType: ProposalTypeRemove, Remove: &Remove{Removed: 4}}
		return framingPreimageInput{WireFormatPublicMessage, framingTestProposalContent(), framingTestGroupContext(t)},
			framingPreimageInput{WireFormatPublicMessage, moved, framingTestGroupContext(t)}
	},
	// the commit arm, moved the same way: an empty commit against one naming a proposal.
	"FramedContent.Commit": func(t *testing.T) (framingPreimageInput, framingPreimageInput) {
		moved := framingTestCommitContent()
		moved.Commit = &Commit{Proposals: []ProposalOrRef{{
			Type:      ProposalOrRefTypeReference,
			Reference: ProposalRef(bytes.Repeat([]byte{0x77}, 32)),
		}}}
		return framingPreimageInput{WireFormatPublicMessage, framingTestCommitContent(), framingTestGroupContext(t)},
			framingPreimageInput{WireFormatPublicMessage, moved, framingTestGroupContext(t)}
	},
	// the sender's own three fields. FramedContent.Sender above moves the whole value and
	// moves the leaf index inside it, which is one field of three: these three are what the
	// walk reaches now that it descends into a structure held by value, and each of them
	// decides WHO a message is attributed to.
	//
	// The sender type is moved between the two arms that bind the group context, so the row
	// separates the discriminant rather than the binding rule -- which is
	// TestEverySenderTypeBindsTheGroupContextSection61GivesIt's subject and not this sweep's.
	// A preimage that dropped it is a member's commit accepted as an external joiner's.
	"Sender.SenderType": func(t *testing.T) (framingPreimageInput, framingPreimageInput) {
		joining := framingTestCommitContent()
		joining.Sender = Sender{SenderType: SenderTypeNewMemberCommit}
		return framingPreimageInput{WireFormatPublicMessage, framingTestCommitContent(), framingTestGroupContext(t)},
			framingPreimageInput{WireFormatPublicMessage, joining, framingTestGroupContext(t)}
	},
	"Sender.LeafIndex": func(t *testing.T) (framingPreimageInput, framingPreimageInput) {
		moved := framingTestMemberContent()
		moved.Sender = Sender{SenderType: SenderTypeMember, LeafIndex: 5}
		return framingPreimageInput{WireFormatPrivateMessage, framingTestMemberContent(), framingTestGroupContext(t)},
			framingPreimageInput{WireFormatPrivateMessage, moved, framingTestGroupContext(t)}
	},
	// the external sender's index, which is the arm of Sender the leaf index is not. It binds
	// no group context, so both inputs of this row carry none -- and a preimage that dropped
	// it is one external sender's proposal accepted as any other's.
	"Sender.SenderIndex": func(t *testing.T) (framingPreimageInput, framingPreimageInput) {
		base := framingTestProposalContent()
		base.Sender = Sender{SenderType: SenderTypeExternal, SenderIndex: 0}
		moved := framingTestProposalContent()
		moved.Sender = Sender{SenderType: SenderTypeExternal, SenderIndex: 5}
		return framingPreimageInput{WireFormatPublicMessage, base, nil},
			framingPreimageInput{WireFormatPublicMessage, moved, nil}
	},
}

// framingPreimageStructTypes is every structure the preimage is assembled from, WALKED out of
// framedContentTBS rather than listed beside it.
//
// The walk descends through a field held by VALUE and stops at a pointer, and that rule is the
// whole of the class. A structure held by value is present in every preimage this type can
// produce, so its fields are as much of what gets signed as its parent's are; a pointer to one
// is an ARM the content type selects, is absent from most preimages, and is moved as a whole
// by the row that names it -- FramedContent.Proposal and FramedContent.Commit are exactly
// those two.
//
// Listed, this was framedContentTBS and FramedContent and stopped there, which is one level
// shallower than this sweep's own prose ("every field of the preimage"). Sender's three fields
// sat under a single FramedContent.Sender row that moves the leaf index alone, so a field
// added to Sender entered no sweep and failed nothing.
func framingPreimageStructTypes(t *testing.T) []reflect.Type {
	t.Helper()
	found := []reflect.Type{}
	queue := []reflect.Type{reflect.TypeOf(framedContentTBS{})}
	for len(queue) != 0 {
		declared := queue[0]
		queue = queue[1:]
		if slices.Contains(found, declared) {
			continue
		}
		if declared.Name() == "" {
			t.Fatal("the walk reached an unnamed struct type, which no row of this sweep can be keyed by")
		}
		if declared.NumField() == 0 {
			t.Fatalf("%s declares no fields, so the sweep below runs over less than the preimage",
				declared.Name())
		}
		found = append(found, declared)
		for i := range declared.NumField() {
			if field := declared.Field(i).Type; field.Kind() == reflect.Struct {
				queue = append(queue, field)
			}
		}
	}
	if len(found) < 2 {
		t.Fatalf("the walk reached %d structure, so it descended into nothing and this sweep is one type wide", len(found))
	}
	return found
}

// framingPreimageFieldNames is every field of every one of those structures, read off the types
// rather than written down.
func framingPreimageFieldNames(t *testing.T) []string {
	t.Helper()
	names := []string{}
	for _, declared := range framingPreimageStructTypes(t) {
		for i := range declared.NumField() {
			names = append(names, declared.Name()+"."+declared.Field(i).Name)
		}
	}
	slices.Sort(names)
	return names
}

// TestASignatureOverOneTbsNeverVerifiesAgainstAnother is the omitted-field gate, run over every
// field of the preimage rather than over the ones somebody thought of.
//
// Two things are asserted per field and they are different claims. The BYTES must differ, which
// says the field reaches the preimage at all; and the signature over one must be refused
// against the other, which says the verifier rebuilds the preimage from the same fields the
// signer used. A codec that wrote a field the verifier then ignored would satisfy the first and
// not the second.
func TestASignatureOverOneTbsNeverVerifiesAgainstAnother(t *testing.T) {
	declared := framingPreimageFieldNames(t)
	written := slices.Sorted(maps.Keys(framingFieldMoves))
	if !slices.Equal(declared, written) {
		t.Fatalf("the preimage is assembled out of the fields %v and this sweep moves %v; a field with no move is a field nothing here judges",
			declared, written)
	}
	crypto := newTestCrypto(t)
	priv, pub, err := crypto.SignatureKeyPair()
	if err != nil {
		t.Fatalf("key pair: %v", err)
	}
	for _, name := range written {
		base, moved := framingFieldMoves[name](t)
		baseTbs, err := FramedContentTBSBytes(base.wireFormat, base.content, base.groupContext)
		if err != nil {
			t.Errorf("%s: base preimage: %v", name, err)
			continue
		}
		movedTbs, err := FramedContentTBSBytes(moved.wireFormat, moved.content, moved.groupContext)
		if err != nil {
			t.Errorf("%s: moved preimage: %v", name, err)
			continue
		}
		if bytes.Equal(baseTbs, movedTbs) {
			t.Errorf("%s: moving it leaves the preimage byte identical, so the field is not in what gets signed",
				name)
			continue
		}
		signed, err := SignAuthenticatedContent(crypto, priv, base.wireFormat, base.content, base.groupContext)
		if err != nil {
			t.Errorf("%s: sign: %v", name, err)
			continue
		}
		// a commit is refused at ValSem009 before its signature is in question, so the rows
		// whose content is a commit carry a tag. What this sweep is about is the preimage, and
		// a refusal for a missing tag would hide whatever answer the signature gave.
		if base.content.ContentType == ContentTypeCommit {
			signed.Auth.ConfirmationTag = bytes.Repeat([]byte{0x5a}, crypto.HashSize())
		}
		if err := VerifyAuthenticatedContent(crypto, pub, signed, base.groupContext); err != nil {
			t.Errorf("%s: the signature does not verify against its own preimage: %v", name, err)
			continue
		}
		lifted := &AuthenticatedContent{
			WireFormat: moved.wireFormat,
			Content:    *moved.content,
			Auth:       FramedContentAuthData{Signature: signed.Auth.Signature},
		}
		if moved.content.ContentType == ContentTypeCommit {
			lifted.Auth.ConfirmationTag = bytes.Repeat([]byte{0x5a}, crypto.HashSize())
		}
		if err := VerifyAuthenticatedContent(crypto, pub, lifted, moved.groupContext); !errors.Is(err, errBadSignature) {
			t.Errorf("%s: a signature over one preimage verified against another: got %v, want the ValSem010 sentinel",
				name, err)
		}
	}
}

// TestASignatureUnderOneWireFormatDoesNotVerifyUnderAnother is the wire format binding, over
// every ORDERED PAIR of the registry rather than over the one pair this plan names.
//
// The wire format is in the preimage precisely so that a PublicMessage cannot be replayed as a
// PrivateMessage or the reverse, which is the pair the plan names -- and the registry has five
// members, so a preimage that separated those two and confused any other pair would pass a test
// written for the pair alone. The class is derived off the registry for that reason.
func TestASignatureUnderOneWireFormatDoesNotVerifyUnderAnother(t *testing.T) {
	crypto := newTestCrypto(t)
	priv, pub, err := crypto.SignatureKeyPair()
	if err != nil {
		t.Fatalf("key pair: %v", err)
	}
	groupContext := framingTestGroupContext(t)
	derived := registryConstantsOfType(t, "WireFormat")
	if len(derived) < 2 {
		t.Fatalf("the WireFormat registry derived %d constants, so there is no pair to confuse", len(derived))
	}
	names := slices.Sorted(maps.Keys(derived))
	compared := 0
	for _, signedUnder := range names {
		signed, err := SignAuthenticatedContent(crypto, priv, WireFormat(derived[signedUnder]),
			framingTestMemberContent(), groupContext)
		if err != nil {
			t.Errorf("sign under %s: %v", signedUnder, err)
			continue
		}
		for _, verifiedUnder := range names {
			replayed := *signed
			replayed.WireFormat = WireFormat(derived[verifiedUnder])
			err := VerifyAuthenticatedContent(crypto, pub, &replayed, groupContext)
			if signedUnder == verifiedUnder {
				if err != nil {
					t.Errorf("%s: a message verified under the wire format it was signed under was refused: %v",
						signedUnder, err)
				}
				continue
			}
			compared++
			if !errors.Is(err, errBadSignature) {
				t.Errorf("a message signed under %s verified under %s: got %v, want the ValSem010 sentinel",
					signedUnder, verifiedUnder, err)
			}
		}
	}
	if want := len(names) * (len(names) - 1); compared != want {
		t.Fatalf("%d of the %d ordered wire format pairs were compared", compared, want)
	}
}

// TestVerifyRefusesACommitWithNoConfirmationTag is ValSem009, with the positive case beside it
// so the rule is not satisfied by a verifier that refuses every commit.
//
// The order matters and is asserted: the tag is checked AFTER the signature, so a commit whose
// signature is wrong AND whose tag is missing is refused as ValSem010. An unauthenticated
// message must not learn which of the two rules it failed.
func TestVerifyRefusesACommitWithNoConfirmationTag(t *testing.T) {
	crypto := newTestCrypto(t)
	priv, pub, err := crypto.SignatureKeyPair()
	if err != nil {
		t.Fatalf("key pair: %v", err)
	}
	groupContext := framingTestGroupContext(t)
	signed, err := SignAuthenticatedContent(crypto, priv, WireFormatPublicMessage,
		framingTestCommitContent(), groupContext)
	if err != nil {
		t.Fatalf("sign a commit: %v", err)
	}
	// every spelling of an absent tag and not the nil one a fresh signature happens to
	// carry. ValSem009 written on the pointer accepts a commit whose tag is an empty non
	// nil slice, which is a commit binding itself to no transcript at all.
	for _, empty := range emptyByteSpellings() {
		untagged := *signed
		untagged.Auth.ConfirmationTag = empty.value
		if err := VerifyAuthenticatedContent(crypto, pub, &untagged, groupContext); !errors.Is(err, errMissingConfirmationTag) {
			t.Fatalf("a commit whose confirmation tag is %s: got %v, want the ValSem009 sentinel", empty.what, err)
		}
	}
	tagged := *signed
	tagged.Auth.ConfirmationTag = bytes.Repeat([]byte{0x5a}, crypto.HashSize())
	if err := VerifyAuthenticatedContent(crypto, pub, &tagged, groupContext); err != nil {
		t.Fatalf("a commit carrying a confirmation tag was refused: %v", err)
	}
	// the ordering: a bad signature under a missing tag answers ValSem010 and not ValSem009
	forged := *signed
	forged.Auth.Signature = bytes.Clone(signed.Auth.Signature)
	forged.Auth.Signature[0] ^= 0x01
	if err := VerifyAuthenticatedContent(crypto, pub, &forged, groupContext); !errors.Is(err, errBadSignature) {
		t.Fatalf("a forged commit with no tag: got %v, want the ValSem010 sentinel", err)
	}
	// and a proposal is not held to carrying one, which is what stops the rule being "refuse
	// everything that has no tag"
	proposal, err := SignAuthenticatedContent(crypto, priv, WireFormatPublicMessage,
		framingTestProposalContent(), groupContext)
	if err != nil {
		t.Fatalf("sign a proposal: %v", err)
	}
	if err := VerifyAuthenticatedContent(crypto, pub, proposal, groupContext); err != nil {
		t.Fatalf("a proposal carrying no confirmation tag was refused: %v", err)
	}
}

// ---------------------------------------------------------------------------
// the published corpus
// ---------------------------------------------------------------------------

// One signature per registered suite per published public message: mlswg's message-protection
// corpus carries a proposal and a commit for each.
//
// Counted rather than assumed, for the reason every known answer count in this package is: a
// filter that stopped matching turns a corpus comparison into a loop that runs zero times and
// reports PASS, which is the one outcome a known answer test must not be able to reach.
const framedContentSignatureComparisons = 4

// framingPublishedPublicMessage decodes one MLSMessage carrying a PublicMessage out of the
// corpus, through THIS package's own codecs.
//
// Decoded rather than spliced, which is the difference between this and the membership tag's
// own reader next door: p4 owns no framing types and had to locate the boundary by searching
// for the published body, and this plan owns them. What is read is version, wire format, the
// FramedContent, the FramedContentAuthData under that content's own type, and the membership
// tag a member's public message ends with -- and the reader is required to be empty at the end,
// so a decode that stopped early cannot pass for one that read the whole message.
func framingPublishedPublicMessage(t *testing.T, at string, mlsMessage []byte) *AuthenticatedContent {
	t.Helper()
	r := syntax.NewReader(mlsMessage)
	version, err := r.ReadUint16()
	if err != nil {
		t.Fatalf("%s: read the protocol version: %v", at, err)
	}
	if ProtocolVersion(version) != ProtocolVersionMls10 {
		t.Fatalf("%s: the message names protocol version %#04x, want mls10", at, version)
	}
	wireFormat, err := r.ReadUint16()
	if err != nil {
		t.Fatalf("%s: read the wire format: %v", at, err)
	}
	if WireFormat(wireFormat) != WireFormatPublicMessage {
		t.Fatalf("%s: the message names wire format %#04x, want a public message", at, wireFormat)
	}
	authContent := &AuthenticatedContent{WireFormat: WireFormat(wireFormat)}
	if err := authContent.Content.UnmarshalMLS(r); err != nil {
		t.Fatalf("%s: decode the framed content: %v", at, err)
	}
	if err := authContent.Auth.UnmarshalMLS(r, authContent.Content.ContentType); err != nil {
		t.Fatalf("%s: decode the auth data: %v", at, err)
	}
	if authContent.Content.Sender.SenderType != SenderTypeMember {
		t.Fatalf("%s: the published message is from sender type %d, and this reader expects the member arm that carries a membership tag",
			at, authContent.Content.Sender.SenderType)
	}
	if _, err := r.ReadOpaque(); err != nil {
		t.Fatalf("%s: read the membership tag: %v", at, err)
	}
	if err := r.Done(); err != nil {
		t.Fatalf("%s: %v, so this reader did not consume the whole published message and the content it answers is not all of it",
			at, err)
	}
	return authContent
}

// TestTheFramedContentSignatureIsTheOneMlswgPublished is the known answer test for this whole
// file, against signatures this package did not compute.
//
// Everything else here is self consistent by construction: sign, verify, and every refusal is
// this package agreeing with itself. A preimage that inlined the group context with a length
// prefix, or that omitted the wire format, or that ordered the fields differently, signs and
// verifies against itself perfectly and fails only against another implementation. This is that
// other implementation.
//
// The corpus is authenticated against upstream's git object store before a byte of it is read,
// through the same loader p4's tag known answer tests use: a known answer test that compares
// against a file an edit can change is a known answer test that can be made to agree with
// anything.
//
// The group context is rebuilt out of the entry's own four fields and encoded by this package's
// codec, which the group context task already holds to this same corpus family. Nothing here is
// circular -- a wrong reconstruction makes the comparison FAIL rather than pass, since only the
// right preimage under the right key verifies a signature somebody else made.
func TestTheFramedContentSignatureIsTheOneMlswgPublished(t *testing.T) {
	entries := []messageProtectionKatEntry{}
	mustLoadAuthenticatedCorpus(t, messageProtectionKatFile, &entries)
	if len(entries) == 0 {
		t.Fatalf("%s parsed to no entries, so every comparison below would run over nothing", messageProtectionKatFile)
	}
	compared := 0
	matched := []CipherSuite{}
	for _, entry := range entries {
		suite := CipherSuite(entry.CipherSuite)
		if !IsRegisteredSuite(suite) {
			continue
		}
		matched = append(matched, suite)
		crypto := mustProvider(t, suite)
		suiteAt := fmt.Sprintf("%s suite %#04x", messageProtectionKatFile, uint16(suite))
		signaturePub := SignaturePublicKey(mustDecodeHex(t, suiteAt+" signature_pub", entry.SignaturePub))
		groupContext, err := syntax.Marshal(&GroupContext{
			Version:                 ProtocolVersionMls10,
			CipherSuite:             suite,
			GroupId:                 mustDecodeHex(t, suiteAt+" group_id", entry.GroupId),
			Epoch:                   entry.Epoch,
			TreeHash:                mustDecodeHex(t, suiteAt+" tree_hash", entry.TreeHash),
			ConfirmedTranscriptHash: mustDecodeHex(t, suiteAt+" confirmed_transcript_hash", entry.ConfirmedTranscriptHash),
		})
		if err != nil {
			t.Fatalf("%s: encode the group context these messages were framed under: %v", suiteAt, err)
		}
		for _, message := range []struct {
			what string
			pub  string
		}{
			{what: "proposal_pub", pub: entry.ProposalPub},
			{what: "commit_pub", pub: entry.CommitPub},
		} {
			at := suiteAt + " " + message.what
			authContent := framingPublishedPublicMessage(t, at, mustDecodeHex(t, at, message.pub))
			if err := VerifyAuthenticatedContent(crypto, signaturePub, authContent, groupContext); err != nil {
				t.Errorf("%s: %v. The signature is over version || wire_format || FramedContent || GroupContext with the context inlined and no length prefix, and nothing else; a preimage that agrees with itself agrees with no other implementation",
					at, err)
				continue
			}
			// and the same message under the epoch next door is refused, so what passed above
			// is the binding rather than a verifier that accepts whatever it is handed
			otherEpoch, err := syntax.Marshal(&GroupContext{
				Version:                 ProtocolVersionMls10,
				CipherSuite:             suite,
				GroupId:                 mustDecodeHex(t, suiteAt+" group_id", entry.GroupId),
				Epoch:                   entry.Epoch + 1,
				TreeHash:                mustDecodeHex(t, suiteAt+" tree_hash", entry.TreeHash),
				ConfirmedTranscriptHash: mustDecodeHex(t, suiteAt+" confirmed_transcript_hash", entry.ConfirmedTranscriptHash),
			})
			if err != nil {
				t.Fatalf("%s: encode the neighbouring epoch's group context: %v", at, err)
			}
			if err := VerifyAuthenticatedContent(crypto, signaturePub, authContent, otherEpoch); !errors.Is(err, errBadSignature) {
				t.Errorf("%s: the published signature verified under the next epoch's group context: got %v, want the ValSem010 sentinel",
					at, err)
			}
			compared++
		}
	}
	if compared != framedContentSignatureComparisons {
		t.Fatalf("%d published framed content signatures were verified, want %d; the loop matched %v",
			compared, framedContentSignatureComparisons, matched)
	}
	if got := slices.Sorted(slices.Values(matched)); !slices.Equal(got, Suites()) {
		t.Fatalf("%s answered for %v and this package registers %v", messageProtectionKatFile, got, Suites())
	}
}

// TestTheFramedContentTbsLabelIsTheOneSection61Names holds the domain separation this signature
// rests on.
//
// A label spelled one way in both halves of this package agrees with itself, so nothing
// behavioural in here can see it. What separates them is a signature made under a NEIGHBOURING
// label over the same preimage, which must not verify: that is the whole of what stops a leaf
// node signature, an update path node signature and a framed content signature being
// interchangeable under one key.
func TestTheFramedContentTbsLabelIsTheOneSection61Names(t *testing.T) {
	if framedContentTBSLabel != "FramedContentTBS" {
		t.Fatalf("the framing signature label is %q, and RFC 9420 section 6.1 writes FramedContentTBS",
			framedContentTBSLabel)
	}
	crypto := newTestCrypto(t)
	priv, pub, err := crypto.SignatureKeyPair()
	if err != nil {
		t.Fatalf("key pair: %v", err)
	}
	groupContext := framingTestGroupContext(t)
	content := framingTestMemberContent()
	tbs, err := FramedContentTBSBytes(WireFormatPrivateMessage, content, groupContext)
	if err != nil {
		t.Fatalf("tbs: %v", err)
	}
	for _, label := range []string{leafNodeSignatureLabel, updatePathNodeLabel, "FramedContentTbs", ""} {
		if label == framedContentTBSLabel {
			t.Fatalf("%q is the framing label itself, so this row compares it against itself", label)
		}
		signature, err := crypto.SignWithLabel(priv, label, tbs)
		if err != nil {
			t.Fatalf("sign under %q: %v", label, err)
		}
		lifted := &AuthenticatedContent{
			WireFormat: WireFormatPrivateMessage,
			Content:    *content,
			Auth:       FramedContentAuthData{Signature: signature},
		}
		if err := VerifyAuthenticatedContent(crypto, pub, lifted, groupContext); !errors.Is(err, errBadSignature) {
			t.Errorf("a signature over this preimage under the label %q verified as a framed content signature: got %v, want the ValSem010 sentinel",
				label, err)
		}
	}
	// and the label really is what the signer used, read back through the provider rather than
	// through this package's own verify
	signed, err := SignAuthenticatedContent(crypto, priv, WireFormatPrivateMessage, content, groupContext)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := crypto.VerifyWithLabel(pub, framedContentTBSLabel, tbs, signed.Auth.Signature); err != nil {
		t.Errorf("the signature this package made is not a signature over the FramedContentTBS preimage under the section 6.1 label: %v", err)
	}
}

// TestTheFramingRefusalsAnswerTheirOwnSentinels keeps the two ValSem codes distinguishable from
// each other and from the structural refusals of the same layer.
//
// A caller branching on ValSem010 must not be answered yes by ValSem009 or by a codec's arm
// mismatch, and this is where that is stated rather than in each test's own assertion: every
// assertion in this file reads errors.Is against one of the two, so two values that answered for
// each other would satisfy all of them at once.
func TestTheFramingRefusalsAnswerTheirOwnSentinels(t *testing.T) {
	for _, one := range []struct {
		name  string
		value error
		other []error
	}{
		{name: "errFramedContentBadSignature", value: errFramedContentBadSignature,
			other: []error{errMissingConfirmationTag, ErrContentArmMismatch, ErrMissingGroupContext,
				ErrUnexpectedGroupContext, ErrUnknownSenderType, ErrUnknownWireFormat, errNilFramedContent,
				errNilAuthenticatedContent}},
		{name: "errMissingConfirmationTag", value: errMissingConfirmationTag,
			other: []error{errFramedContentBadSignature, errBadSignature, ErrCryptoBadSignature}},
		// ValSem007 and ValSem008. Each names the other, because "the sender sent no tag" and
		// "the tag does not verify" are the pair a validator has to keep apart, and each names
		// the signature refusal next door, because a caller branching on ValSem010 must not be
		// answered yes by a membership tag that failed.
		{name: "errMissingMembershipTag", value: errMissingMembershipTag,
			other: []error{errBadMembershipTag, errFramedContentBadSignature, errBadSignature,
				errMissingConfirmationTag, ErrCryptoBadSignature, ErrSenderNotMember}},
		{name: "errBadMembershipTag", value: errBadMembershipTag,
			other: []error{errMissingMembershipTag, errFramedContentBadSignature, errBadSignature,
				errMissingConfirmationTag, ErrCryptoBadSignature, ErrSenderNotMember}},
		// ValSem005. It names both tag refusals and the signature's, because a validator that
		// could not tell "this was framed in the clear" from "this was not authenticated" would
		// report the wrong rule for the wrong message -- and it names the structural pair next
		// door, because an application message in a public frame is neither an arm mismatch nor
		// an unregistered content type.
		{name: "errApplicationMustBeCiphertext", value: errApplicationMustBeCiphertext,
			other: []error{errMissingMembershipTag, errBadMembershipTag, errFramedContentBadSignature,
				errBadSignature, errMissingConfirmationTag, ErrCryptoBadSignature,
				ErrContentArmMismatch, ErrUnknownContentType, ErrWireFormatMismatch}},
		// the two argument refusals section 6.2's open adds. Each names the other and both name
		// the framing layer's existing pair: "the caller passed nothing" and "no key exists for
		// this sender" are different things to do about, and neither is a message that failed.
		{name: "errNilPublicMessage", value: errNilPublicMessage,
			other: []error{errNilAuthenticatedContent, errNilFramedContent, errNilSignatureKeyResolver,
				errApplicationMustBeCiphertext, errFramedContentBadSignature}},
		{name: "errNilSignatureKeyResolver", value: errNilSignatureKeyResolver,
			other: []error{errNilAuthenticatedContent, errNilFramedContent, errNilPublicMessage,
				errApplicationMustBeCiphertext, errFramedContentBadSignature}},
		// section 6.2's select on the tag, in the direction that has no field on the wire. It
		// names both tag refusals, because "this message carries a tag nothing can check" and
		// "this message is missing the one thing that says it came from inside the group" are
		// opposite mistakes and a caller told the wrong one is sent to fix the wrong field, and
		// it names the structural pair next door for ErrUnexpectedGroupContext's reason: the two
		// are the same rule about two different fields and a caller branching on one must not be
		// answered yes by the other.
		{name: "errUnexpectedMembershipTag", value: errUnexpectedMembershipTag,
			other: []error{errMissingMembershipTag, errBadMembershipTag, errFramedContentBadSignature,
				errApplicationMustBeCiphertext, ErrUnexpectedGroupContext, ErrMissingGroupContext,
				ErrSenderNotMember, ErrUnknownSenderType}},
	} {
		if one.value == nil || one.value.Error() == "" {
			t.Fatalf("%s is nil or has an empty message", one.name)
		}
		if !strings.HasPrefix(one.value.Error(), "mls: ") {
			t.Errorf("%s reads %q; every typed error of this package names the package it came from",
				one.name, one.value.Error())
		}
		for _, other := range one.other {
			if errors.Is(one.value, other) {
				t.Errorf("%s answers to %v, so a caller branching on the two reads one as the other",
					one.name, other)
			}
		}
	}
	// the one wrap this layer does argue for, in the direction it argues for it: the framing
	// refusal answers the broad "the signature did not verify" question, so a caller that only
	// wants that keeps being answered by it.
	if !errors.Is(errFramedContentBadSignature, errBadSignature) {
		t.Error("the framing signature refusal does not answer the package's ValSem010 stand in, so a caller matching that name stops matching this layer")
	}
}

// framingUnregisteredCodePoint is the smallest code point of a registry's width that the
// registry does not hold.
//
// Derived rather than written down, so a later task that registers 0x0006 as a wire format does
// not leave a row below building "the unknown one" over a code point that has since become
// known. That row would go on passing while asserting the opposite of what it says.
func framingUnregisteredCodePoint(t *testing.T, typeName string, width uint64) uint64 {
	t.Helper()
	registered := registryConstantsOfType(t, typeName)
	for candidate := uint64(1); candidate <= width; candidate++ {
		taken := false
		for _, value := range registered {
			if value == candidate {
				taken = true
				break
			}
		}
		if !taken {
			return candidate
		}
	}
	t.Fatalf("every code point of %s up to %d is registered, so this gate has no unknown one to build",
		typeName, width)
	return 0
}

// framingStructuralPreimageRefusals is every input this package can assemble that NO preimage can
// be built out of, keyed by what makes it one.
//
// Derived over the sender type registry plus a code point neither registry holds, rather than
// sampled: which group context arm a sender type forbids comes off senderBindsGroupContext rather
// than off a list here, so a sender type or a wire format a later task registers joins every sweep
// reading this by existing.
//
// Two gates read it and they ask opposite questions of the same rows. One asserts that a message
// carrying a signature is answered by the preimage's own refusal verbatim; the other asserts that
// the same message carrying NO membership tag is answered by ValSem007 instead, which is the only
// input that separates the two orders verifyMembershipTag's first two guards can be written in.
func framingStructuralPreimageRefusals(t *testing.T) map[string]framingPreimageInput {
	t.Helper()
	structural := map[string]framingPreimageInput{}
	for name, code := range registryConstantsOfType(t, "SenderType") {
		senderType := SenderType(code)
		binds, err := senderBindsGroupContext(senderType)
		if err != nil {
			t.Errorf("%s is a registered sender type and senderBindsGroupContext refused it: %v", name, err)
			continue
		}
		content := framingTestProposalContent()
		content.Sender = Sender{SenderType: senderType}
		// the arm this sender type forbids: one that binds the epoch handed no context, one
		// that binds none handed a context
		forbidden := framingTestGroupContext(t)
		if binds {
			forbidden = nil
		}
		structural[name+" handed the group context arm it forbids"] =
			framingPreimageInput{WireFormatPublicMessage, content, forbidden}
	}
	if len(structural) == 0 {
		t.Fatal("no registered sender type produced a row, so every gate reading this runs over the empty set")
	}
	unknownSender := framingTestProposalContent()
	unknownSender.Sender = Sender{SenderType: SenderType(framingUnregisteredCodePoint(t, "SenderType", 0xff))}
	structural["a sender type no registry holds"] =
		framingPreimageInput{WireFormatPublicMessage, unknownSender, framingTestGroupContext(t)}
	structural["a wire format no registry holds"] = framingPreimageInput{
		WireFormat(framingUnregisteredCodePoint(t, "WireFormat", 0xffff)),
		framingTestMemberContent(), framingTestGroupContext(t)}
	return structural
}

// TestVerifyAnswersThePreimagesRefusalVerbatimAndCollapsesEverySignatureFailure states which of
// the two rules each refusal of VerifyAuthenticatedContent falls under.
//
// The function's documentation used to say every failure collapses into
// errFramedContentBadSignature, and four sentinels travel out of it unchanged -- three of them
// reachable from a message a PEER sent, because the arm they select on is the sender type
// inside that message. Nothing observed the claim in either direction, so the prose and the
// code disagreed silently, and a later ValSem code mapper keyed on the sentinel would have had
// no code for those inputs.
//
// Both halves are derived. The structural half runs the group context arm each REGISTERED
// sender type forbids, plus a code point neither registry holds, and asserts the verifier hands
// back the preimage builder's own error -- compared by message, so a row cannot pass by
// answering some other value that happens to wrap the same sentinel. The signature half runs
// every way this package can produce a signature that does not verify and asserts each answers
// the ONE value by identity rather than by errors.Is, which is what "and nothing narrower"
// means.
func TestVerifyAnswersThePreimagesRefusalVerbatimAndCollapsesEverySignatureFailure(t *testing.T) {
	signed := framingSignedMemberMessage(t)
	signature := signed.authContent.Auth.Signature
	if len(signature) == 0 {
		t.Fatal("the fixture carries no signature, so every row below is refused before it is reached")
	}

	// the structural half: inputs no preimage can be assembled from at all
	structural := framingStructuralPreimageRefusals(t)

	for _, name := range slices.Sorted(maps.Keys(structural)) {
		one := structural[name]
		_, preimage := FramedContentTBSBytes(one.wireFormat, one.content, one.groupContext)
		if preimage == nil {
			t.Errorf("%s: the preimage was assembled, so this row states nothing about a refusal", name)
			continue
		}
		answered := VerifyAuthenticatedContent(signed.crypto, signed.pub, &AuthenticatedContent{
			WireFormat: one.wireFormat,
			Content:    *one.content,
			Auth:       FramedContentAuthData{Signature: signature},
		}, one.groupContext)
		if answered == nil || answered.Error() != preimage.Error() {
			t.Errorf("%s: the preimage refused with %v and the verifier answered %v; a structural refusal travels out of the verifier unchanged",
				name, preimage, answered)
		}
		if errors.Is(answered, errBadSignature) {
			t.Errorf("%s: a message no preimage could be built for was answered as a signature that does not verify (%v), which sends the caller to check a signature nothing checked",
				name, answered)
		}
	}

	// the signature half: every way this package can produce one that does not verify
	type framingForgery struct {
		what        string
		authContent *AuthenticatedContent
		against     []byte
	}
	forged := []framingForgery{}
	flipped := *signed.authContent
	flipped.Auth.Signature = bytes.Clone(signature)
	flipped.Auth.Signature[0] ^= 0x01
	forged = append(forged, framingForgery{"a signature with one bit flipped", &flipped, signed.groupContext})
	truncated := *signed.authContent
	truncated.Auth.Signature = bytes.Clone(signature)[:len(signature)-1]
	forged = append(forged, framingForgery{"a signature one octet short", &truncated, signed.groupContext})
	for _, empty := range emptyByteSpellings() {
		zero := *signed.authContent
		zero.Auth.Signature = empty.value
		forged = append(forged, framingForgery{"a signature that is " + empty.what, &zero, signed.groupContext})
	}
	// a signature over another epoch's preimage, which is the replay the group context is in
	// the preimage to stop, and a signature by a key that is not this one. Both are real
	// signatures: what fails is the preimage they cover and the key that made them.
	otherEpoch := bytes.Clone(signed.groupContext)
	otherEpoch[len(otherEpoch)-1] ^= 0xff
	elsewhere, err := SignAuthenticatedContent(signed.crypto, signed.priv, WireFormatPrivateMessage,
		framingTestMemberContent(), otherEpoch)
	if err != nil {
		t.Fatalf("sign under another epoch: %v", err)
	}
	forged = append(forged, framingForgery{"a signature over another epoch's preimage", elsewhere, signed.groupContext})
	strangerPriv, _, err := signed.crypto.SignatureKeyPair()
	if err != nil {
		t.Fatalf("a second key pair: %v", err)
	}
	stranger, err := SignAuthenticatedContent(signed.crypto, strangerPriv, WireFormatPrivateMessage,
		framingTestMemberContent(), signed.groupContext)
	if err != nil {
		t.Fatalf("sign under another key: %v", err)
	}
	forged = append(forged, framingForgery{"a signature by a key that is not this one", stranger, signed.groupContext})

	for _, one := range forged {
		answered := VerifyAuthenticatedContent(signed.crypto, signed.pub, one.authContent, one.against)
		if answered != errFramedContentBadSignature {
			t.Errorf("%s: the verifier answered %v, want errFramedContentBadSignature itself; a caller that can tell these apart learns which of its guesses was closest",
				one.what, answered)
		}
	}
	if len(forged) < 5 {
		t.Fatalf("%d ways of failing the signature were run, and the empty spellings alone are three", len(forged))
	}
}

// ---------------------------------------------------------------------------
// AuthenticatedContentTBM and the membership tag
// ---------------------------------------------------------------------------

// framingSignedOfEveryContentType signs one message per REGISTERED content type and answers
// them keyed by it, with a commit's confirmation tag filled in the way its committer would.
//
// Derived against the registry rather than written as three rows, which is this package's rule
// after fourteen hand written class lists understated the class they named. The arms are not
// interchangeable here: a commit's FramedContentAuthData is a signature FOLLOWED BY a
// confirmation tag and every other arm's is a signature alone, so a preimage that stopped at
// the signature is invisible from an application message and visible from a commit. A fourth
// content type registered by a later task joins every sweep below by existing, and until
// somebody builds a message for it these fail rather than covering part of the registry
// quietly.
func framingSignedOfEveryContentType(t *testing.T, signed framingSigned) map[ContentType]*AuthenticatedContent {
	t.Helper()
	contents := map[ContentType]*FramedContent{
		ContentTypeApplication: framingTestMemberContent(),
		ContentTypeProposal:    framingTestProposalContent(),
		ContentTypeCommit:      framingTestCommitContent(),
	}
	registered := map[ContentType]string{}
	for name, code := range registryConstantsOfType(t, "ContentType") {
		registered[ContentType(code)] = name
	}
	built := map[ContentType]*AuthenticatedContent{}
	for contentType, name := range registered {
		content, held := contents[contentType]
		if !held {
			t.Fatalf("%s is a registered content type and no message is built for it, so every sweep reading this runs over a subset of the registry",
				name)
		}
		if content.ContentType != contentType {
			t.Fatalf("the message built for %s carries content type %d", name, content.ContentType)
		}
		authContent, err := SignAuthenticatedContent(signed.crypto, signed.priv,
			WireFormatPublicMessage, content, signed.groupContext)
		if err != nil {
			t.Fatalf("sign a %s: %v", name, err)
		}
		// a commit's auth data carries a confirmation tag as well as a signature, and the
		// encoder refuses to write one without it. The committer fills it in once it has
		// advanced the transcript; here it is a value of the provider's own tag width and
		// nothing more, because what these sweeps are about is that it is COVERED.
		if contentType == ContentTypeCommit {
			authContent.Auth.ConfirmationTag = bytes.Repeat([]byte{0x77}, signed.crypto.HashSize())
		}
		built[contentType] = authContent
	}
	for contentType := range contents {
		if _, isRegistered := registered[contentType]; !isRegistered {
			t.Fatalf("a message is built for content type %d, which no registry of this package holds", contentType)
		}
	}
	return built
}

// TestTheMembershipTagPreimageIsTheSignaturePreimageFollowedByTheAuthData holds the layout of
// AuthenticatedContentTBM at every registered content type.
//
// The two halves are asserted separately rather than as one byte comparison, because they fail
// for different reasons and a reader has to be told which. A preimage that does not BEGIN with
// the FramedContentTBS is one whose membership tag covers a different message than its
// signature does; a preimage whose tail is not the auth data is one whose membership tag does
// not cover the authenticators, which for a commit means the confirmation tag is outside the
// MAC and a tag can be lifted from one commit onto another.
//
// The auth data is required to be non empty, and that line is not decoration. Without it a
// preimage that stopped at the FramedContentTBS satisfies the prefix half, satisfies the tail
// half against an empty expectation, and reports PASS.
func TestTheMembershipTagPreimageIsTheSignaturePreimageFollowedByTheAuthData(t *testing.T) {
	signed := framingSignedMemberMessage(t)
	for contentType, authContent := range framingSignedOfEveryContentType(t, signed) {
		at := fmt.Sprintf("content type %d", contentType)
		tbm, err := AuthenticatedContentTBMBytes(authContent, signed.groupContext)
		if err != nil {
			t.Fatalf("%s: the tag preimage: %v", at, err)
		}
		tbs, err := FramedContentTBSBytes(authContent.WireFormat, &authContent.Content, signed.groupContext)
		if err != nil {
			t.Fatalf("%s: the signature preimage: %v", at, err)
		}
		w := syntax.NewWriter()
		if err := authContent.Auth.MarshalMLS(w, contentType); err != nil {
			t.Fatalf("%s: encode the auth data: %v", at, err)
		}
		auth, err := w.Bytes()
		if err != nil {
			t.Fatalf("%s: the auth data bytes: %v", at, err)
		}
		if len(auth) == 0 {
			t.Fatalf("%s: the auth data encoded to nothing, so the tail comparison below holds for a preimage that stops at the signature preimage",
				at)
		}
		if !bytes.HasPrefix(tbm, tbs) {
			t.Errorf("%s: the tag preimage is %x and does not begin with the signature preimage %x; a membership tag over a preimage that is not the signature's own covers a different message than the signature does",
				at, tbm, tbs)
			continue
		}
		if tail := tbm[len(tbs):]; !bytes.Equal(tail, auth) {
			t.Errorf("%s: the tag preimage carries %x after the signature preimage and the auth data is %x; RFC 9420 section 6.1 puts the FramedContentAuthData there, so a commit's confirmation tag is inside what the membership tag authenticates",
				at, tail, auth)
		}
	}
}

// TestTheMembershipTagPreimageBindsEveryByteOfTheGroupContext is the epoch binding, derived
// over the LENGTH of the context rather than sampled at a position somebody chose.
//
// What it refuses is the omission senderBindsGroupContext calls the most expensive one
// available here, one layer up: a TBM assembled without the group context is a membership tag
// that verifies in EVERY epoch of the group, so a proposal a member sent in epoch 4 is a
// proposal any peer can replay into epoch 9 and every receiver accepts. That preimage is well
// formed, it agrees with itself in both directions, and no round trip property in this package
// can see it.
//
// Every byte and not the epoch field alone, because the group id, the tree hash and the
// confirmed transcript hash are in there for reasons of their own -- two groups, two trees and
// two histories -- and a preimage that inlined a truncated context binds only some of them.
func TestTheMembershipTagPreimageBindsEveryByteOfTheGroupContext(t *testing.T) {
	signed := framingSignedMemberMessage(t)
	if len(signed.groupContext) == 0 {
		t.Fatal("the fixture's group context is empty, so the sweep below runs over nothing")
	}
	base, err := AuthenticatedContentTBMBytes(signed.authContent, signed.groupContext)
	if err != nil {
		t.Fatalf("the tag preimage: %v", err)
	}
	for at := range signed.groupContext {
		moved := bytes.Clone(signed.groupContext)
		moved[at] ^= 0xff
		tbm, err := AuthenticatedContentTBMBytes(signed.authContent, moved)
		if err != nil {
			t.Fatalf("byte %d of %d moved: %v", at, len(signed.groupContext), err)
		}
		if bytes.Equal(tbm, base) {
			t.Errorf("byte %d of the %d byte group context does not reach the tag preimage, so a membership tag taken under this epoch is a valid tag under a group context that differs there",
				at, len(signed.groupContext))
		}
	}
	// and the same statement in the direction a caller reaches by passing nothing: a member's
	// preimage cannot be built without one at all, rather than being built one field shorter.
	for _, empty := range emptyByteSpellings() {
		if _, err := AuthenticatedContentTBMBytes(signed.authContent, empty.value); !errors.Is(err, ErrMissingGroupContext) {
			t.Errorf("a member's tag preimage was built over a group context that is %s: got %v, want ErrMissingGroupContext",
				empty.what, err)
		}
	}
}

// TestTheMembershipTagPreimageBindsEveryByteOfTheAuthData is the other half, derived over the
// lengths of the authenticators the message actually carries.
//
// The fields are read off the VALUE rather than listed per content type: whatever a
// FramedContentAuthData is carrying at this arm is swept, so a task that gives that structure a
// third authenticator joins this sweep by filling it in. What the sweep refuses is a TBM built
// from the FramedContent rather than from the AuthenticatedContent -- the shape that reads like
// the obvious one, since the tag travels beside the content on the wire -- which authenticates
// neither the signature nor the confirmation tag.
//
// The commit arm is required to have been swept, because it is the only one that carries a
// confirmation tag and it is the arm the whole property is about.
func TestTheMembershipTagPreimageBindsEveryByteOfTheAuthData(t *testing.T) {
	signed := framingSignedMemberMessage(t)
	for contentType, authContent := range framingSignedOfEveryContentType(t, signed) {
		at := fmt.Sprintf("content type %d", contentType)
		base, err := AuthenticatedContentTBMBytes(authContent, signed.groupContext)
		if err != nil {
			t.Fatalf("%s: the tag preimage: %v", at, err)
		}
		swept := map[string]int{}
		for name, field := range map[string]*[]byte{
			"the signature":        &authContent.Auth.Signature,
			"the confirmation tag": &authContent.Auth.ConfirmationTag,
		} {
			original := bytes.Clone(*field)
			for position := range original {
				moved := bytes.Clone(original)
				moved[position] ^= 0xff
				*field = moved
				tbm, err := AuthenticatedContentTBMBytes(authContent, signed.groupContext)
				*field = original
				if err != nil {
					t.Fatalf("%s: byte %d of %s moved: %v", at, position, name, err)
				}
				if bytes.Equal(tbm, base) {
					t.Errorf("%s: byte %d of %s does not reach the tag preimage, so the membership tag does not cover it",
						at, position, name)
				}
				swept[name]++
			}
		}
		if swept["the signature"] == 0 {
			t.Fatalf("%s: the fixture carries no signature, so this sweep ran over nothing", at)
		}
		if contentType == ContentTypeCommit && swept["the confirmation tag"] == 0 {
			t.Fatalf("%s: the commit fixture carries no confirmation tag, so the one arm this sweep exists for was never run", at)
		}
	}
}

// TestTheMembershipTagPreimageBindsTheWireFormat runs the whole registry rather than the two
// code points a reader pictures, and asks for the preimages to be pairwise distinct.
//
// Distinctness rather than a layout assertion, because what the wire format is in the preimage
// FOR is that no two of them produce the same authenticated bytes: a PublicMessage replayed as
// a PrivateMessage is the substitution section 6.1 puts the field there to refuse. A TBM built
// under a wire format the caller named, or under a constant, is the same bytes for every entry
// of the registry and fails here.
func TestTheMembershipTagPreimageBindsTheWireFormat(t *testing.T) {
	signed := framingSignedMemberMessage(t)
	registered := registryConstantsOfType(t, "WireFormat")
	seen := map[string]string{}
	for name, code := range registered {
		lifted := *signed.authContent
		lifted.WireFormat = WireFormat(code)
		tbm, err := AuthenticatedContentTBMBytes(&lifted, signed.groupContext)
		if err != nil {
			t.Fatalf("%s: the tag preimage: %v", name, err)
		}
		if other, collided := seen[string(tbm)]; collided {
			t.Errorf("the tag preimage under %s is byte for byte the one under %s, so a membership tag over a %s is a valid membership tag over the same content sent as a %s",
				name, other, other, name)
			continue
		}
		seen[string(tbm)] = name
	}
	if len(seen) != len(registered) {
		t.Errorf("%d of the %d registered wire formats produced a distinct tag preimage", len(seen), len(registered))
	}
}

// TestVerifyMembershipTagRefusesEveryTagButItsOwn is ValSem007 and ValSem008, with every
// refusal derived over the length or the shape of the thing it alters.
//
// The sampled version of this test is the one this project has already been burned by twice: a
// suite that flips bit zero of byte zero is satisfied by a verifier that reads byte zero and
// stops, and a suite with no length case in it at all is satisfied by a verifier that accepts
// every truncation of a valid tag -- a forgery an attacker finds by trying tags one octet long.
// So the bit sweep is every bit of every byte, and the length sweep is every length shorter
// than its own as well as several longer.
//
// The absent tag is swept over all three spellings of absent, for emptyByteSpellings' reason: a
// guard written on == nil accepts the empty non nil slice a decoder hands back after reading an
// empty opaque<V>, which is a PublicMessage whose membership_tag field is present and empty.
func TestVerifyMembershipTagRefusesEveryTagButItsOwn(t *testing.T) {
	signed := framingSignedMemberMessage(t)
	membershipKey := bytes.Repeat([]byte{0x5a}, signed.crypto.HashSize())
	tag, err := ComputeMembershipTag(signed.crypto, membershipKey, signed.authContent, signed.groupContext)
	if err != nil {
		t.Fatalf("compute the tag: %v", err)
	}
	if len(tag) != signed.crypto.HashSize() {
		t.Fatalf("the tag is %d bytes and this provider's mac is %d", len(tag), signed.crypto.HashSize())
	}
	// the positive first: without it every refusal below is satisfied by a verifier that
	// refuses everything, which is the shape a suite of nothing but negatives cannot see.
	if err := verifyMembershipTag(signed.crypto, membershipKey, signed.authContent,
		signed.groupContext, tag); err != nil {
		t.Fatalf("the verifier refused the tag ComputeMembershipTag produced over the same message under the same key: %v", err)
	}
	for _, empty := range emptyByteSpellings() {
		if err := verifyMembershipTag(signed.crypto, membershipKey, signed.authContent,
			signed.groupContext, empty.value); !errors.Is(err, errMissingMembershipTag) {
			t.Errorf("a tag that is %s answered %v, want the ValSem007 sentinel", empty.what, err)
		}
	}
	for at := range tag {
		for bit := 0; bit < 8; bit++ {
			flipped := bytes.Clone(tag)
			flipped[at] ^= 1 << bit
			if err := verifyMembershipTag(signed.crypto, membershipKey, signed.authContent,
				signed.groupContext, flipped); !errors.Is(err, errBadMembershipTag) {
				t.Errorf("bit %d of byte %d of the tag flipped answered %v, want the ValSem008 sentinel", bit, at, err)
			}
		}
	}
	for n := 1; n < len(tag); n++ {
		if err := verifyMembershipTag(signed.crypto, membershipKey, signed.authContent,
			signed.groupContext, bytes.Clone(tag)[:n]); !errors.Is(err, errBadMembershipTag) {
			t.Errorf("a tag truncated to %d of %d bytes answered %v; a prefix comparison accepts every truncation of a valid tag",
				n, len(tag), err)
		}
	}
	// longer as well as shorter, and over a CLONE rather than an append onto the tag itself:
	// append on a slice with spare capacity writes through into the caller's array, which turns
	// a refusal row into a row that also moved the value every other row is built from.
	for n := 1; n <= 4; n++ {
		extended := append(bytes.Clone(tag), bytes.Repeat([]byte{0x00}, n)...)
		if err := verifyMembershipTag(signed.crypto, membershipKey, signed.authContent,
			signed.groupContext, extended); !errors.Is(err, errBadMembershipTag) {
			t.Errorf("a tag %d bytes longer than its own answered %v", n, err)
		}
	}
	// the neighbouring key, over every byte of it. confirmation_key and membership_key are
	// adjacent DeriveSecret calls over one parent, so they are the same width and a swap
	// produces a tag just as well formed; nothing about the SHAPE of an answer separates them.
	for at := range membershipKey {
		other := bytes.Clone(membershipKey)
		other[at] ^= 0xff
		if err := verifyMembershipTag(signed.crypto, other, signed.authContent,
			signed.groupContext, tag); !errors.Is(err, errBadMembershipTag) {
			t.Errorf("the tag verified under a key differing at byte %d of %d: %v", at, len(membershipKey), err)
		}
	}
	// the neighbouring epoch, which is what the group context is in the preimage for
	otherEpoch := bytes.Clone(signed.groupContext)
	otherEpoch[len(otherEpoch)-1] ^= 0xff
	if err := verifyMembershipTag(signed.crypto, membershipKey, signed.authContent,
		otherEpoch, tag); !errors.Is(err, errBadMembershipTag) {
		t.Errorf("the tag verified under another epoch's group context: %v", err)
	}
	// and a message no preimage can be assembled for is answered by the PREIMAGE's refusal
	// rather than by ValSem008, for VerifyAuthenticatedContent's reason: there is no comparison
	// to have failed, so telling the caller its tag did not verify sends it to check a tag
	// nothing checked.
	external := framingTestProposalContent()
	external.Sender = Sender{SenderType: SenderTypeExternal}
	lifted := &AuthenticatedContent{
		WireFormat: WireFormatPublicMessage,
		Content:    *external,
		Auth:       signed.authContent.Auth,
	}
	if err := verifyMembershipTag(signed.crypto, membershipKey, lifted,
		signed.groupContext, tag); !errors.Is(err, ErrUnexpectedGroupContext) {
		t.Errorf("a sender type that binds no group context answered %v, want the preimage's own refusal", err)
	}
	if err := verifyMembershipTag(signed.crypto, membershipKey, nil,
		signed.groupContext, tag); !errors.Is(err, errNilAuthenticatedContent) {
		t.Errorf("a nil message answered %v, want the nil message refusal", err)
	}
}

// TestTheMembershipTagPreimageIsTheOneThePublishedTagsWereTakenOver is the known answer test
// for this task, and it is the join nothing on this project had made.
//
// Everything else in this file is this package agreeing with itself. A preimage that inlined
// the group context with a length prefix, that omitted the wire format, that ordered its two
// halves the other way round, or that stopped at the FramedContent, computes a tag and verifies
// it back perfectly and fails only against another implementation. mlswg's message-protection
// corpus is that other implementation: it publishes an epoch's membership_key, the four
// GroupContext fields the epoch was framed under, and two PublicMessages whose membership_tag
// is MAC(membership_key, AuthenticatedContentTBM).
//
// Three separate things are compared here and each catches what the others do not.
//
// The bytes this task builds are held against the SPLICE p4's own known answer test rebuilds
// out of the published message -- version, wire format and FramedContent taken verbatim from
// the corpus, the group context inserted at the boundary the published body locates, the auth
// data taken from what is left once the trailing membership tag is removed. That reconstruction
// is a function of published bytes alone and shares no code with this one, so a disagreement
// names the preimage rather than the key.
//
// The tag is compared against the published one through THIS plan's ComputeMembershipTag and
// through p4's (*KeySchedule).MembershipTag, over the same preimage. That pair is the cross
// plan property nobody had checked: p5's note says p6 builds the TBM and passes the bytes, the
// two halves were written by different plans against a prose description of one structure, and
// until now nothing had put one into the other. A key schedule that verified its own tags and a
// framing layer that verified its own would both have been green.
//
// And the published tag is required to be REFUSED under the neighbouring epoch's context, so
// what passed above is the binding rather than a verifier that accepts what it is handed.
func TestTheMembershipTagPreimageIsTheOneThePublishedTagsWereTakenOver(t *testing.T) {
	entries := []messageProtectionKatEntry{}
	mustLoadAuthenticatedCorpus(t, messageProtectionKatFile, &entries)
	if len(entries) == 0 {
		t.Fatalf("%s parsed to no entries, so every comparison below would run over nothing", messageProtectionKatFile)
	}
	epochs := ksVectorEpochs(t)
	compared := 0
	matched := []CipherSuite{}
	for _, entry := range entries {
		suite := CipherSuite(entry.CipherSuite)
		if !IsRegisteredSuite(suite) {
			continue
		}
		matched = append(matched, suite)
		crypto := mustProvider(t, suite)
		nh := crypto.HashSize()
		suiteAt := fmt.Sprintf("%s suite %#04x", messageProtectionKatFile, uint16(suite))
		membershipKey := mustDecodeHex(t, suiteAt+" membership_key", entry.MembershipKey)
		if len(membershipKey) != nh {
			t.Fatalf("%s: the published membership_key is %d bytes and this suite's KDF.Nh is %d",
				suiteAt, len(membershipKey), nh)
		}
		context := func(epoch uint64) []byte {
			t.Helper()
			encoded, err := syntax.Marshal(&GroupContext{
				Version:                 ProtocolVersionMls10,
				CipherSuite:             suite,
				GroupId:                 mustDecodeHex(t, suiteAt+" group_id", entry.GroupId),
				Epoch:                   epoch,
				TreeHash:                mustDecodeHex(t, suiteAt+" tree_hash", entry.TreeHash),
				ConfirmedTranscriptHash: mustDecodeHex(t, suiteAt+" confirmed_transcript_hash", entry.ConfirmedTranscriptHash),
			})
			if err != nil {
				t.Fatalf("%s: encode the group context these messages were framed under: %v", suiteAt, err)
			}
			return encoded
		}
		groupContext := context(entry.Epoch)
		for _, message := range []struct {
			what string
			body string
			pub  string
		}{
			{what: "proposal_pub", body: entry.Proposal, pub: entry.ProposalPub},
			{what: "commit_pub", body: entry.Commit, pub: entry.CommitPub},
		} {
			at := suiteAt + " " + message.what
			publicMessage := mustDecodeHex(t, at, message.pub)
			authContent := framingPublishedPublicMessage(t, at, publicMessage)
			tbm, err := AuthenticatedContentTBMBytes(authContent, groupContext)
			if err != nil {
				t.Fatalf("%s: the tag preimage: %v", at, err)
			}
			spliced := authenticatedContentTbm(t, at, publicMessage,
				mustDecodeHex(t, at+" body", message.body), groupContext, nh)
			if !bytes.Equal(tbm, spliced) {
				t.Errorf("%s: AuthenticatedContentTBMBytes built %x, and splicing the published message at the boundary its own body locates gives %x. These are the bytes p4's tag functions consume, and the two plans wrote them from one prose description of section 6.1 without ever comparing them",
					at, tbm, spliced)
			}
			want := publishedTagAtTheTail(t, at, publicMessage, nh)
			got, err := ComputeMembershipTag(crypto, membershipKey, authContent, groupContext)
			if err != nil {
				t.Fatalf("%s: compute the tag: %v", at, err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s: ComputeMembershipTag = %x, and mlswg published %x. The tag is MAC(membership_key, AuthenticatedContentTBM) and nothing else; a preimage that agrees with itself agrees with no other implementation",
					at, got, want)
			}
			if err := verifyMembershipTag(crypto, membershipKey, authContent, groupContext, want); err != nil {
				t.Errorf("%s: the verifier refused the tag mlswg published for this key and this message: %v", at, err)
			}
			// p4's half of the join, over the preimage this task built
			schedule := ksScheduleForSuite(t, epochs, suite)
			installTheCorpusKey(t, at, &schedule.Secrets().Membership, membershipKey,
				schedule.MembershipTag(tbm), want)
			if fromSchedule := schedule.MembershipTag(tbm); !bytes.Equal(fromSchedule, want) {
				t.Errorf("%s: (*KeySchedule).MembershipTag over the preimage this task builds = %x, and mlswg published %x",
					at, fromSchedule, want)
			}
			if !schedule.VerifyMembershipTag(tbm, want) {
				t.Errorf("%s: (*KeySchedule).VerifyMembershipTag refused the published tag over the preimage this task builds", at)
			}
			if err := verifyMembershipTag(crypto, membershipKey, authContent,
				context(entry.Epoch+1), want); !errors.Is(err, errBadMembershipTag) {
				t.Errorf("%s: the published tag verified under the next epoch's group context: got %v, want the ValSem008 sentinel",
					at, err)
			}
			compared++
		}
	}
	if compared != membershipTagComparisons {
		t.Fatalf("%d published membership tags were reproduced, want %d; the loop matched %v",
			compared, membershipTagComparisons, matched)
	}
	if got := slices.Sorted(slices.Values(matched)); !slices.Equal(got, Suites()) {
		t.Fatalf("%s answered for %v and this package registers %v", messageProtectionKatFile, got, Suites())
	}
}

// ---------------------------------------------------------------------------
// guardrail 8 over the membership tag refusal
// ---------------------------------------------------------------------------

// membershipTagRefusal is one declaration that can answer a membership tag refusal, together
// with the parsed file it was read out of, because every rule below renders nodes of it back to
// source and a node rendered against the wrong file set gives the wrong positions.
//
// decides separates the half of the class that MAKES the decision from the half that carries
// somebody else's out. Both are in the class -- a refusal is a refusal to the caller wherever it
// was decided -- and they are held to different rules, because a rule about HOW a tag was
// compared is a rule about a body that compared one.
type membershipTagRefusal struct {
	name     string
	host     parsedSource
	function *ast.FuncDecl
	decides  bool
}

// membershipTagSource is one parsed file together with the path the class reports its
// declarations under.
type membershipTagSource struct {
	path   string
	parsed parsedSource
}

// membershipTagNames answers whether one expression mentions an identifier anywhere inside it.
//
// ANYWHERE, rather than as the whole of the expression, and that is the difference between a
// class and a spelling. `return errBadMembershipTag` and
// `return fmt.Errorf("%w: ...", errBadMembershipTag)` are one refusal to every caller -- errors.Is
// answers yes to both -- and the file this rule reads already writes its OWN refusals in the
// second shape. Measured: with the rule rendering the result and string comparing it to the
// sentinel's name, a declaration that decided the tag with a hand written byte loop and refused in
// the wrapping shape entered no class here and was reported by nothing in ./mls/... or
// ./message/....
func membershipTagNames(expression ast.Expr, sentinel string) bool {
	named := false
	ast.Inspect(expression, func(node ast.Node) bool {
		if identifier, isIdentifier := node.(*ast.Ident); isIdentifier && identifier.Name == sentinel {
			named = true
		}
		return !named
	})
	return named
}

// membershipTagRefusalReturns is every return that can carry one sentinel out of a node, rendered.
func membershipTagRefusalReturns(parsed parsedSource, node ast.Node, sentinel string) []string {
	found := []string{}
	ast.Inspect(node, func(inner ast.Node) bool {
		returns, isReturn := inner.(*ast.ReturnStmt)
		if !isReturn {
			return true
		}
		for _, result := range returns.Results {
			if membershipTagNames(result, sentinel) {
				found = append(found, parsed.render(returns))
				break
			}
		}
		return true
	})
	return found
}

// membershipTagCalleeNames is every name one declaration calls, as the call site spells it: the
// bare identifier for a function of this package and the selected name for a method.
//
// The selected half over reports and is meant to. A method sharing a name with a member of the
// class pulls its caller in, which costs that caller the propagation rule and nothing else; the
// direction that loses a mutant is the other one.
func membershipTagCalleeNames(function *ast.FuncDecl) []string {
	names := []string{}
	ast.Inspect(function.Body, func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if !isCall {
			return true
		}
		switch callee := call.Fun.(type) {
		case *ast.Ident:
			names = append(names, callee.Name)
		case *ast.SelectorExpr:
			names = append(names, callee.Sel.Name)
		}
		return true
	})
	return names
}

// membershipTagRefusalsIn is the class over a set of parsed files: every declaration that can
// answer the ValSem008 refusal, whether it decides one or carries one out.
//
// Derived off the SENTINEL rather than off a name, and closed under CALLS rather than stopping at
// the declarations that name it, which is the whole of why this is a class. The rules below are
// about how that refusal is REACHED and about what happens to it once it exists, so the members
// are every declaration either question can be asked of, and p7's receive path joins by refusing
// or by calling something that refuses rather than by somebody remembering to add it here. The one
// shape that escapes the derivation -- a verifier that stopped refusing at all -- empties the class
// rather than passing it, and an empty class is fatal below.
//
// The fixed point is not decoration. p7 will reach this refusal through its own helpers, and a
// class that read only the direct callers would drop the declaration two hops out exactly as the
// name comparison dropped the wrapping shape.
func membershipTagRefusalsIn(sources []membershipTagSource, sentinel string) []membershipTagRefusal {
	candidates := []membershipTagRefusal{}
	for _, source := range sources {
		for _, declaration := range source.parsed.file.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil {
				continue
			}
			candidates = append(candidates, membershipTagRefusal{
				name:     source.path + ": " + function.Name.Name,
				host:     source.parsed,
				function: function,
				decides:  len(membershipTagRefusalReturns(source.parsed, function.Body, sentinel)) != 0,
			})
		}
	}
	inClass := map[string]bool{}
	for _, candidate := range candidates {
		if candidate.decides {
			inClass[candidate.function.Name.Name] = true
		}
	}
	for grew := true; grew; {
		grew = false
		for _, candidate := range candidates {
			if inClass[candidate.function.Name.Name] {
				continue
			}
			for _, callee := range membershipTagCalleeNames(candidate.function) {
				if !inClass[callee] {
					continue
				}
				inClass[candidate.function.Name.Name] = true
				grew = true
				break
			}
		}
	}
	found := []membershipTagRefusal{}
	for _, candidate := range candidates {
		if inClass[candidate.function.Name.Name] {
			found = append(found, candidate)
		}
	}
	return found
}

// membershipTagRoutingFaults reads one declaration for every way it can decide a membership tag
// other than the one guardrail 8 permits, and for every way it can lose one it was handed, each
// answered as "kind: detail".
//
// The KIND is what the control compares, so each half of the rule has to be the only thing
// reporting some member of that fixture. A rule whose halves cannot be told apart is a rule that
// can have a half deleted with its control still matching exactly what it wants.
func membershipTagRoutingFaults(parsed parsedSource, function *ast.FuncDecl, sentinel string,
	class []membershipTagRefusal) []string {

	faults := []string{}
	if len(membershipTagRefusalReturns(parsed, function.Body, sentinel)) != 0 {
		faults = append(faults, membershipTagDecisionFaults(parsed, function, sentinel)...)
	}
	return append(faults, membershipTagPropagationFaults(parsed, function, class)...)
}

// membershipTagDecisionFaults judges a body that DECIDES the refusal: what it decided with, and
// whether it walked the bytes itself.
//
// The loop clause is the one that is not about a comparator name, and it is here because
// constant_time_test.go's own header says what that gate cannot see: "a comparison written as a
// byte loop in this package's own source names no comparator and is in no class derived from
// imports". That blind spot is closed for this refusal by refusing the loop itself, which a
// decision written as one cannot do without.
//
// It is asked only of a decider, and that is the boundary rather than an omission. A declaration
// that carries somebody else's refusal out compares nothing, and a receive path is a loop:
// reporting the loop there would be a fault about a comparison that is not in the body.
func membershipTagDecisionFaults(parsed parsedSource, function *ast.FuncDecl, sentinel string) []string {
	parameters := []string{}
	if function.Type.Params != nil {
		for _, field := range function.Type.Params.List {
			for _, name := range field.Names {
				parameters = append(parameters, name.Name)
			}
		}
	}
	faults := []string{}
	refusals := len(membershipTagRefusalReturns(parsed, function.Body, sentinel))
	guarded := 0
	ast.Inspect(function.Body, func(node ast.Node) bool {
		branch, isIf := node.(*ast.IfStmt)
		if !isIf {
			return true
		}
		inside := len(membershipTagRefusalReturns(parsed, branch.Body, sentinel))
		if inside == 0 {
			return true
		}
		guarded += inside
		faults = append(faults, membershipTagGuardFaults(parsed, branch.Cond, parameters)...)
		return true
	})
	if guarded < refusals {
		faults = append(faults, fmt.Sprintf("unguarded: %d of its %d refusals are reached from no condition at all",
			refusals-guarded, refusals))
	}
	ast.Inspect(function.Body, func(node ast.Node) bool {
		switch node.(type) {
		case *ast.ForStmt, *ast.RangeStmt:
			faults = append(faults, "loop: it walks the bytes itself, and a comparison written as a loop names no comparator for any import derived gate to find")
		}
		return true
	})
	return faults
}

// membershipTagErrorResultAt is the position an error comes back at in one declaration's results,
// or -1 for a declaration that cannot report one.
func membershipTagErrorResultAt(member membershipTagRefusal) int {
	if member.function.Type.Results == nil {
		return -1
	}
	at := 0
	for _, result := range member.function.Type.Results.List {
		if member.host.render(result.Type) == "error" {
			return at
		}
		width := len(result.Names)
		if width == 0 {
			width = 1
		}
		at += width
	}
	return -1
}

// membershipTagPropagationFaults is guardrail 7 over the same refusal: a declaration that reaches
// a member of the class must not lose the answer.
//
// This is the half the decision rules cannot state, and p7 is the caller it is written for. A
// receive path that reaches verifyMembershipTag and throws the error away applies a proposal or a
// commit that no member of the group sent, and it does that with a body in which the sanctioned
// comparison is the only comparison there is -- so every rule above reports it clean. The doc
// comment on verifyMembershipTag writes the obligation out in prose, "p7 MUST RETURN on this
// refusal rather than logging it and continuing"; this is where the prose is measured.
//
// Two ways to lose it and both are syntax. A call written as a statement of its own binds no error
// at all. A call whose error result is assigned to the blank identifier binds it to nothing, which
// is the spelling that compiles and reads like a decision. And a declaration that reaches one of
// these while answering no error of its own cannot carry the refusal out however the call is
// written, so that is its own kind rather than a second report of the first.
func membershipTagPropagationFaults(parsed parsedSource, function *ast.FuncDecl,
	class []membershipTagRefusal) []string {

	reached := func(call *ast.CallExpr) (membershipTagRefusal, bool) {
		name := ""
		switch callee := call.Fun.(type) {
		case *ast.Ident:
			name = callee.Name
		case *ast.SelectorExpr:
			name = callee.Sel.Name
		}
		for _, member := range class {
			if member.function.Name.Name == name && member.function != function {
				return member, true
			}
		}
		return membershipTagRefusal{}, false
	}
	answersAnError := false
	if function.Type.Results != nil {
		for _, result := range function.Type.Results.List {
			if parsed.render(result.Type) == "error" {
				answersAnError = true
			}
		}
	}
	faults := []string{}
	carried := []string{}
	ast.Inspect(function.Body, func(node ast.Node) bool {
		switch statement := node.(type) {
		case *ast.ExprStmt:
			call, isCall := statement.X.(*ast.CallExpr)
			if !isCall {
				return true
			}
			member, isMember := reached(call)
			if !isMember || membershipTagErrorResultAt(member) < 0 {
				return true
			}
			carried = append(carried, member.function.Name.Name)
			faults = append(faults, "discarded: it calls "+member.function.Name.Name+
				" as a statement of its own, so the refusal is bound to nothing")
		case *ast.AssignStmt:
			if len(statement.Rhs) != 1 {
				return true
			}
			call, isCall := statement.Rhs[0].(*ast.CallExpr)
			if !isCall {
				return true
			}
			member, isMember := reached(call)
			if !isMember {
				return true
			}
			at := membershipTagErrorResultAt(member)
			if at < 0 || at >= len(statement.Lhs) {
				return true
			}
			carried = append(carried, member.function.Name.Name)
			if target, isIdentifier := statement.Lhs[at].(*ast.Ident); isIdentifier && target.Name == "_" {
				faults = append(faults, "discarded: it assigns "+member.function.Name.Name+
					"'s refusal to the blank identifier")
			}
		}
		return true
	})
	if len(carried) != 0 && !answersAnError {
		faults = append(faults, "unanswerable: it reaches "+strings.Join(slices.Compact(carried), ", ")+
			" and answers no error of its own, so no spelling of the call could carry the refusal out")
	}
	return faults
}

// membershipTagGuardFaults reads the condition one refusal is reached from.
func membershipTagGuardFaults(parsed parsedSource, condition ast.Expr, parameters []string) []string {
	negated, isUnary := condition.(*ast.UnaryExpr)
	if !isUnary || negated.Op != token.NOT {
		return []string{"guard: it refuses on " + parsed.render(condition) +
			" rather than on a MacVerify that answered false"}
	}
	call, isCall := negated.X.(*ast.CallExpr)
	if !isCall {
		return []string{"guard: it refuses on " + parsed.render(condition) +
			" rather than on a MacVerify that answered false"}
	}
	selector, isSelector := call.Fun.(*ast.SelectorExpr)
	if !isSelector || selector.Sel.Name != "MacVerify" {
		return []string{"guard: it refuses on " + parsed.render(condition) +
			", and guardrail 8 says a tag comparison is CryptoProvider.MacVerify and nothing else"}
	}
	faults := []string{}
	base, isIdentifier := selector.X.(*ast.Ident)
	if !isIdentifier || !slices.Contains(parameters, base.Name) {
		faults = append(faults, "provider: it verifies through "+parsed.render(selector.X)+
			", which is not a provider it was handed")
	}
	if len(call.Args) != 3 {
		return append(faults, fmt.Sprintf("arity: it calls MacVerify with %d arguments", len(call.Args)))
	}
	for at, argument := range call.Args {
		if _, isWhole := argument.(*ast.Ident); !isWhole {
			faults = append(faults, fmt.Sprintf("argument: it passes %s at MacVerify position %d, which is not the whole of a value it holds",
				parsed.render(argument), at))
		}
	}
	if parsed.render(call.Args[1]) == parsed.render(call.Args[2]) {
		faults = append(faults, "self: it compares the tag against itself rather than against a mac over the preimage")
	}
	return faults
}

// membershipTagRoutingControl declares one of each shape the rules above have to tell apart: the
// sanctioned body, the two comparators a ban list would have had to think of, the byte loop that
// carries no comparator at all, a prefix of the tag pushed through the sanctioned call, a tag
// compared against itself, a provider the function was never handed, a refusal reached from no
// condition, and then the five shapes that are about the refusal AFTER it exists -- the byte loop
// whose refusal is WRAPPED rather than bare, a caller that carries the refusal out, and the three
// ways to lose one.
//
// Every one is here because a control that does not DISCRIMINATE its own rule issues a broken
// matcher exactly the clean bill a working one issues. hmac.Equal is in the fixture deliberately:
// it is constant time and it is still wrong, because guardrail 8 names
// crypto/subtle.ConstantTimeCompare reached through CryptoProvider.MacVerify specifically, and a
// second comparison site is a second place the length refusal can be dropped.
//
// The wrapping and propagating shapes are here because they were measured to be outside the class
// the earlier rule derived: the rule rendered each return and string compared it to the sentinel's
// name, so `fmt.Errorf("%w: p7", errBadMembershipTag)` and `return err` were both invisible, and a
// variable time comparison written in the first of them was caught by nothing in ./mls/... or
// ./message/....
const membershipTagRoutingControl = `package control

func VerifiesThroughTheProvider(crypto CryptoProvider, key []byte, data []byte, tag []byte) error {
	if !crypto.MacVerify(key, data, tag) {
		return errBadMembershipTag
	}
	return nil
}

func VerifiesWithBytesEqual(crypto CryptoProvider, key []byte, data []byte, tag []byte) error {
	if !bytes.Equal(crypto.Mac(key, data), tag) {
		return errBadMembershipTag
	}
	return nil
}

func VerifiesWithHmacEqual(crypto CryptoProvider, key []byte, data []byte, tag []byte) error {
	if !hmac.Equal(crypto.Mac(key, data), tag) {
		return errBadMembershipTag
	}
	return nil
}

func VerifiesWithAByteLoop(crypto CryptoProvider, key []byte, data []byte, tag []byte) error {
	mine := crypto.Mac(key, data)
	same := len(mine) == len(tag)
	for at := range tag {
		if at < len(mine) && mine[at] != tag[at] {
			same = false
		}
	}
	if !same {
		return errBadMembershipTag
	}
	return nil
}

func VerifiesAPrefixOfTheTag(crypto CryptoProvider, key []byte, data []byte, tag []byte) error {
	if !crypto.MacVerify(key, data, tag[:1]) {
		return errBadMembershipTag
	}
	return nil
}

func VerifiesTheTagAgainstItself(crypto CryptoProvider, key []byte, data []byte, tag []byte) error {
	if !crypto.MacVerify(key, tag, tag) {
		return errBadMembershipTag
	}
	return nil
}

func VerifiesThroughAProviderItWasNotGiven(crypto CryptoProvider, key []byte, data []byte, tag []byte) error {
	if !elsewhere.MacVerify(key, data, tag) {
		return errBadMembershipTag
	}
	return nil
}

func RefusesWithNoConditionAtAll(crypto CryptoProvider, key []byte, data []byte, tag []byte) error {
	crypto.MacVerify(key, data, tag)
	return errBadMembershipTag
}

func WrapsTheSentinelAfterAByteLoop(crypto CryptoProvider, key []byte, data []byte, tag []byte) error {
	mine := crypto.Mac(key, data)
	same := len(mine) == len(tag)
	for at := range tag {
		if at < len(mine) && mine[at] != tag[at] {
			same = false
		}
	}
	if !same {
		return fmt.Errorf("%w: the wrapping shape", errBadMembershipTag)
	}
	return nil
}

func PropagatesTheRefusal(crypto CryptoProvider, key []byte, data []byte, tag []byte) error {
	if err := VerifiesThroughTheProvider(crypto, key, data, tag); err != nil {
		return err
	}
	return nil
}

func DiscardsTheRefusal(crypto CryptoProvider, key []byte, data []byte, tag []byte) error {
	VerifiesThroughTheProvider(crypto, key, data, tag)
	return nil
}

func BlanksTheRefusal(crypto CryptoProvider, key []byte, data []byte, tag []byte) error {
	_ = VerifiesThroughTheProvider(crypto, key, data, tag)
	return nil
}

func CannotAnswerTheRefusal(crypto CryptoProvider, key []byte, data []byte, tag []byte) {
	VerifiesThroughTheProvider(crypto, key, data, tag)
}
`

// membershipTagRoutingControlFaults is the kind of fault each control declaration must draw,
// written out here rather than derived, because a control is the one thing in a derived gate
// that cannot be derived from the rule it controls.
var membershipTagRoutingControlFaults = map[string][]string{
	"VerifiesThroughTheProvider":            {},
	"VerifiesWithBytesEqual":                {"guard"},
	"VerifiesWithHmacEqual":                 {"guard"},
	"VerifiesWithAByteLoop":                 {"guard", "loop"},
	"VerifiesAPrefixOfTheTag":               {"argument"},
	"VerifiesTheTagAgainstItself":           {"self"},
	"VerifiesThroughAProviderItWasNotGiven": {"provider"},
	"RefusesWithNoConditionAtAll":           {"unguarded"},
	// the wrapping shape, which is a member of the class only because the rule reads the whole
	// return expression rather than its rendering
	"WrapsTheSentinelAfterAByteLoop": {"guard", "loop"},
	// and the four propagating shapes, which are members only because the class is closed under
	// calls. The first must draw NOTHING, or the rule reports every caller and says nothing.
	"PropagatesTheRefusal":   {},
	"DiscardsTheRefusal":     {"discarded"},
	"BlanksTheRefusal":       {"discarded"},
	"CannotAnswerTheRefusal": {"discarded", "unanswerable"},
}

// TestEveryMembershipTagRefusalIsDecidedByMacVerifyAndNothingElse is guardrails 8 and 7 over this
// task's refusal, read off the source rather than off an input.
//
// No behavioural test in this file can see this. A verifier that compared with bytes.Equal, or
// with a byte loop of its own, answers exactly what this one answers for every input above: the
// timing leak and the dropped length refusal are properties of HOW the answer was reached, and
// the answer is identical. constant_time_test.go reads every comparison in this package's source
// against a class derived from its imports and catches the named comparators; what its own header
// says it cannot catch is the loop, and this is where that is closed for the one refusal that
// decides whether a message no member sent is applied to the group.
func TestEveryMembershipTagRefusalIsDecidedByMacVerifyAndNothingElse(t *testing.T) {
	const sentinel = "errBadMembershipTag"
	// the control first: a rule that has stopped matching issues the real source exactly the
	// clean bill a working one issues
	control := mustParseText(t, "the membership tag routing control", membershipTagRoutingControl)
	controlClass := membershipTagRefusalsIn([]membershipTagSource{{path: "control", parsed: control}}, sentinel)
	reported := map[string][]string{}
	for _, member := range controlClass {
		kinds := []string{}
		for _, fault := range membershipTagRoutingFaults(control, member.function, sentinel, controlClass) {
			kind, _, named := strings.Cut(fault, ": ")
			if !named {
				t.Fatalf("the rule answered %q, which carries no kind for the control to compare", fault)
			}
			if !slices.Contains(kinds, kind) {
				kinds = append(kinds, kind)
			}
		}
		slices.Sort(kinds)
		reported[member.function.Name.Name] = kinds
	}
	if len(reported) != len(membershipTagRoutingControlFaults) {
		t.Fatalf("the class read %v out of the control and the control declares %d bodies that refuse; a body it does not read is a shape the real source can be written in",
			slices.Sorted(maps.Keys(reported)), len(membershipTagRoutingControlFaults))
	}
	for _, name := range slices.Sorted(maps.Keys(membershipTagRoutingControlFaults)) {
		got, read := reported[name]
		if !read {
			t.Errorf("the class did not read %s out of the control", name)
			continue
		}
		if want := membershipTagRoutingControlFaults[name]; !slices.Equal(got, want) {
			t.Errorf("the rule reports %s with %v, want %v; a half of it that reports nothing of its own can be deleted with this control still matching",
				name, got, want)
		}
	}

	// and then this package's own source
	sources := []membershipTagSource{}
	for _, path := range packageLevelFunctions(t).files {
		sources = append(sources, membershipTagSource{path: path, parsed: mustParseSource(t, path)})
	}
	class := membershipTagRefusalsIn(sources, sentinel)
	deciders := 0
	for _, member := range class {
		if member.decides {
			deciders++
		}
	}
	if deciders == 0 {
		t.Fatalf("no declaration of this package's non test source answers %s, and this task lands one, so this gate is demanding nothing",
			sentinel)
	}
	for _, member := range class {
		for _, fault := range membershipTagRoutingFaults(member.host, member.function, sentinel, class) {
			t.Errorf("%s: %s", member.name, fault)
		}
	}
	t.Logf("%d declaration(s) can answer %s: %d decide one through CryptoProvider.MacVerify alone, and %d carry one out",
		len(class), sentinel, deciders, len(class)-deciders)
}

// ---------------------------------------------------------------------------
// the membership_key itself, and the order the two doors judge their arguments in
// ---------------------------------------------------------------------------

// membershipTagDoorNames is every declaration of this package's non test source that takes a
// membership_key, read off the parameter rather than listed.
//
// The class is the parameter NAME because that is what the doors share and what a third one would
// share: RFC 9420 section 6.2 has one key, and a declaration that takes it is a declaration that
// can mac under it. A gate whose table named two functions is a gate a third door gets written
// beside -- which is the shape of the finding this exists for, since ComputeMembershipTag and
// verifyMembershipTag both accepted a key p4's own door refuses.
func membershipTagDoorNames(t *testing.T) []string {
	t.Helper()
	names := []string{}
	for _, path := range packageLevelFunctions(t).files {
		parsed := mustParseSource(t, path)
		for _, declaration := range parsed.file.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Type.Params == nil {
				continue
			}
			for _, field := range function.Type.Params.List {
				for _, parameter := range field.Names {
					if parameter.Name == "membershipKey" {
						names = append(names, function.Name.Name)
					}
				}
			}
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)
	if len(names) == 0 {
		t.Fatal("no declaration of this package's non test source takes a membershipKey, so this gate demands nothing")
	}
	return names
}

// membershipTagKeyRefusal is one shape of membership_key no tag may be taken under, with the
// sentinel both doors must answer for it.
type membershipTagKeyRefusal struct {
	what     string
	key      []byte
	sentinel error
}

// membershipTagUnusableKeys is every such shape, derived over the provider's own KDF.Nh rather
// than written at 32 octets.
//
// The width sweep is every length from nothing up to twice its own rather than one short key and
// one long one, for TestVerifyMembershipTagRefusesEveryTagButItsOwn's reason: a guard that read
// the first byte of the length, or that refused only what is shorter, passes a sampled sweep and
// this project has shipped exactly that twice.
//
// The last row is not a length case at all and it is the row this gate exists for. An epoch that
// has left PastEpochWindow is zeroized IN PLACE, so its membership_key is still KDF.Nh bytes and
// every length check in the world clears it, while a mac under KDF.Nh zero bytes is publicly
// computable: any party can forge a membership tag the receiver would accept, and that tag is the
// only authentication a member's PublicMessage carries besides the signature.
func membershipTagUnusableKeys(nh int) []membershipTagKeyRefusal {
	rows := []membershipTagKeyRefusal{}
	for _, empty := range emptyByteSpellings() {
		rows = append(rows, membershipTagKeyRefusal{
			what:     "a key that is " + empty.what,
			key:      empty.value,
			sentinel: ErrSecretLength,
		})
	}
	for n := 1; n <= 2*nh; n++ {
		if n == nh {
			continue
		}
		rows = append(rows, membershipTagKeyRefusal{
			what:     fmt.Sprintf("a key %d bytes wide and not %d", n, nh),
			key:      bytes.Repeat([]byte{0x6b}, n),
			sentinel: ErrSecretLength,
		})
	}
	rows = append(rows, membershipTagKeyRefusal{
		what:     fmt.Sprintf("the %d zero bytes an erased epoch leaves behind", nh),
		key:      make([]byte, nh),
		sentinel: ErrEpochErased,
	})
	return rows
}

// TestBothDoorsIntoSection62RefuseEveryKeyNoTagMayBeTakenUnder is the membership_key half of RFC
// 9420 section 6.2, and it is the half nothing observed.
//
// The finding it lands: ComputeMembershipTag and verifyMembershipTag took a key of ANY length and
// ANY content. Over the erased epoch's key -- KDF.Nh zero bytes, which is what
// PastEpochWindow's zeroize leaves behind and what the length can never see -- p6 produced a tag
// and accepted it back, while p4's (*KeySchedule).MembershipTag and VerifyMembershipTag refused
// the identical key through secretIsLive. Two doors into one rule, one of them guarded, and the
// doc comment on the unguarded one steering p7 toward it.
//
// Both halves are derived. The DOORS come off the parameter name, so a third one is swept by
// existing rather than by being added to a table; and which keys are unusable comes off p4's OWN
// predicate rather than off an opinion here -- every row is asserted to be a key secretIsLive
// calls dead before it is asked of p6, so the two plans are held to one class rather than to two
// lists that agree today.
//
// The positive row is not decoration. Without it every refusal below is satisfied by a door that
// refuses everything, which is the shape a sweep of nothing but negatives cannot see.
func TestBothDoorsIntoSection62RefuseEveryKeyNoTagMayBeTakenUnder(t *testing.T) {
	signed := framingSignedMemberMessage(t)
	sealed := framingSealedMemberProposal(t)
	nh := signed.crypto.HashSize()
	if sealed.crypto.HashSize() != nh {
		t.Fatalf("the two fixtures run at KDF.Nh %d and %d, so one set of key widths cannot be swept through both",
			nh, sealed.crypto.HashSize())
	}
	live := bytes.Repeat([]byte{0x5a}, nh)
	tag, err := ComputeMembershipTag(signed.crypto, live, signed.authContent, signed.groupContext)
	if err != nil {
		t.Fatalf("compute the tag every row below is run against: %v", err)
	}
	doors := map[string]func(t *testing.T, key []byte) error{
		"ComputeMembershipTag": func(t *testing.T, key []byte) error {
			answered, err := ComputeMembershipTag(signed.crypto, key, signed.authContent, signed.groupContext)
			// a refusal that also hands back bytes is the shape this project shipped once: the
			// caller reads the answer, the error goes to a log, and a tag derived from nothing
			// travels.
			if err != nil && answered != nil {
				t.Errorf("ComputeMembershipTag refused with %v and handed back %x anyway", err, answered)
			}
			return err
		},
		// section 6.2's two wire format doors, which reach the key through the two above. They are
		// swept here because the class is the PARAMETER, not the pair somebody wrote a table for:
		// each is a declaration a caller can hand an erased epoch's key to, and each has to refuse
		// it rather than mac under a run of zeros any party can compute.
		//
		// Both rows run over a message sealed for the PUBLIC wire format and carrying a proposal,
		// because ValSem005 refuses an application message here and the seal refuses any other wire
		// format -- the fixture the two rows above use is a private-format application message and
		// would be refused by the rule under test rather than by the key.
		"SealPublicMessage": func(t *testing.T, key []byte) error {
			message, err := SealPublicMessage(sealed.crypto, key, sealed.authContent, sealed.groupContext)
			// a refusal that also hands back a message is ComputeMembershipTag's shape one layer
			// out: the caller reads the answer, the error goes to a log, and a public message
			// travels carrying a tag derived from nothing.
			if err != nil && message != nil {
				t.Errorf("SealPublicMessage refused with %v and handed back a message carrying %x anyway",
					err, message.MembershipTag)
			}
			return err
		},
		"OpenPublicMessage": func(t *testing.T, key []byte) error {
			opened, err := OpenPublicMessage(sealed.crypto, key, sealed.message,
				StaticSignatureKey(sealed.pub), sealed.groupContext)
			if err != nil && opened != nil {
				t.Errorf("OpenPublicMessage refused with %v and handed back an authenticated content anyway", err)
			}
			return err
		},
		"verifyMembershipTag": func(t *testing.T, key []byte) error {
			return verifyMembershipTag(signed.crypto, key, signed.authContent, signed.groupContext, tag)
		},
	}
	if got, want := slices.Sorted(maps.Keys(doors)), membershipTagDoorNames(t); !slices.Equal(got, want) {
		t.Fatalf("this gate runs %v and this package's non test source takes a membership key at %v; a door with no row is a door with no guard",
			got, want)
	}
	// p4's own predicate is what says which keys are unusable rather than this test's opinion of
	// them. secretIsLive is the guard (*KeySchedule).MembershipTag and VerifyMembershipTag refuse
	// through, and the whole of the finding was that section 6.2's other door never asked.
	schedule := &KeySchedule{crypto: signed.crypto}
	if !schedule.secretIsLive(live) {
		t.Fatalf("p4's predicate calls the key every positive row is taken under erased, so the rows below compare two different classes")
	}
	for _, name := range slices.Sorted(maps.Keys(doors)) {
		door := doors[name]
		if err := door(t, live); err != nil {
			t.Errorf("%s refused a live key of the provider's own width: %v", name, err)
		}
		for _, refusal := range membershipTagUnusableKeys(nh) {
			if schedule.secretIsLive(refusal.key) {
				t.Errorf("%s: p4's secretIsLive calls %s live, so this row asks the two plans for different things",
					name, refusal.what)
				continue
			}
			answered := door(t, refusal.key)
			if !errors.Is(answered, refusal.sentinel) {
				t.Errorf("%s over %s answered %v, want %v", name, refusal.what, answered, refusal.sentinel)
			}
			// and it is refused as what it is. A key the RECEIVER got wrong answered as ValSem007
			// or ValSem008 sends the caller to look at a message that was never the problem, and
			// a validator mapping sentinels to codes would report a rule the sender did not fail.
			if errors.Is(answered, errBadMembershipTag) || errors.Is(answered, errMissingMembershipTag) {
				t.Errorf("%s over %s answered a ValSem code about the MESSAGE (%v), and what was wrong was the receiver's own key",
					name, refusal.what, answered)
			}
		}
	}
}

// TestTheAbsentMembershipTagIsRefusedAheadOfEveryPreimageThatCannotBeBuilt is the ORDER of
// verifyMembershipTag's first two message guards, which its own comment states and which nothing
// observed.
//
// The order is invisible from every other test in this file. A tagless message whose preimage
// assembles answers ValSem007 whichever side of the AuthenticatedContentTBMBytes call the guard is
// written on, so what separates the two orders is exactly the input that is BOTH tagless and
// unbuildable -- and there was none. Measured: the guard moved below the preimage build left the
// whole of ./mls/... green.
//
// What the order is worth. A receiver that built the preimage first answers a message carrying no
// membership tag at all with ErrUnknownSenderType or ErrMissingGroupContext, which is the
// preimage's complaint about a structure nobody was going to authenticate; the rule that actually
// refused the message is ValSem007, and a validator mapping sentinels to ValSem codes would have
// none for it. It also does the assembly for a message that could not have been accepted however
// it assembled.
//
// The class is framingStructuralPreimageRefusals', so a sender type or a wire format a later task
// registers joins by existing, and each row is run over all three spellings of an absent tag for
// emptyByteSpellings' reason.
func TestTheAbsentMembershipTagIsRefusedAheadOfEveryPreimageThatCannotBeBuilt(t *testing.T) {
	signed := framingSignedMemberMessage(t)
	membershipKey := bytes.Repeat([]byte{0x5a}, signed.crypto.HashSize())
	tag, err := ComputeMembershipTag(signed.crypto, membershipKey, signed.authContent, signed.groupContext)
	if err != nil {
		t.Fatalf("compute the tag the discriminating half is run with: %v", err)
	}
	structural := framingStructuralPreimageRefusals(t)
	for _, name := range slices.Sorted(maps.Keys(structural)) {
		one := structural[name]
		lifted := &AuthenticatedContent{
			WireFormat: one.wireFormat,
			Content:    *one.content,
			Auth:       signed.authContent.Auth,
		}
		// the discriminator first: carrying a tag, this row has to REACH the preimage and be
		// answered by its refusal verbatim. Without that half every assertion below is satisfied
		// by a verifier that answers ValSem007 to everything.
		_, preimage := AuthenticatedContentTBMBytes(lifted, one.groupContext)
		if preimage == nil {
			t.Errorf("%s: the preimage was assembled, so this row states nothing about an ordering", name)
			continue
		}
		if answered := verifyMembershipTag(signed.crypto, membershipKey, lifted, one.groupContext,
			tag); answered == nil || answered.Error() != preimage.Error() {
			t.Errorf("%s carrying a tag answered %v and the preimage refused with %v", name, answered, preimage)
		}
		for _, empty := range emptyByteSpellings() {
			answered := verifyMembershipTag(signed.crypto, membershipKey, lifted, one.groupContext, empty.value)
			if !errors.Is(answered, errMissingMembershipTag) {
				t.Errorf("%s carrying a tag that is %s answered %v, want the ValSem007 sentinel: the absent tag is refused before the preimage is built",
					name, empty.what, answered)
			}
		}
	}
	// the nil message is the preimage's other refusal and no row above can carry it, since a nil
	// authenticated content has no wire format to key one on.
	for _, empty := range emptyByteSpellings() {
		answered := verifyMembershipTag(signed.crypto, membershipKey, nil, signed.groupContext, empty.value)
		if !errors.Is(answered, errMissingMembershipTag) {
			t.Errorf("a nil message carrying a tag that is %s answered %v, want the ValSem007 sentinel",
				empty.what, answered)
		}
	}
}

// ---------------------------------------------------------------------------
// what the commentary claims about the gates
// ---------------------------------------------------------------------------

// The gate this package's commentary cites as the one that reads every comparison it ships. Held
// as the string the prose writes rather than as a reference to the function, because what is being
// checked is whether the prose names something that exists.
const membershipTagComparatorGate = "TestNothingThisPackageShipsComparesDataOutsideConstantTime"

// membershipTagCommentBlocks is every run of consecutive line comments in one file, joined.
//
// The BLOCK and not the line, because a claim runs across several lines and a name cited on one of
// them is a claim of the whole block. Read out of the file's text rather than out of go/parser's
// doc comments because this package's parse helper reads source with SkipObjectResolution and
// without ParseComments, so a declaration's Doc is nil under it.
func membershipTagCommentBlocks(t *testing.T, path string) []string {
	t.Helper()
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	blocks := []string{}
	current := []string{}
	for _, line := range strings.Split(string(source), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			current = append(current, trimmed)
			continue
		}
		if len(current) != 0 {
			blocks = append(blocks, strings.Join(current, "\n"))
			current = nil
		}
	}
	if len(current) != 0 {
		blocks = append(blocks, strings.Join(current, "\n"))
	}
	return blocks
}

// TestTheMembershipTagCommentaryNamesGatesThatExistAndAClassThatHoldsItsSpellings measures the
// claims this package's prose makes about its own gates.
//
// It is here because one of them was wrong in a way no test could see. verifyMembershipTag's
// comment said "the package's derived comparator gate reads every comparison in this file's source
// and finds eighteen such names". Eighteen is the count the ./message gate reports over its own
// directory -- message/writeauth_test.go scans "." and is clean over a bytes.Equal planted in
// mls/framing_protect.go -- and the gate that does read this file derives twenty six. A number
// stated in a comment nothing recomputes is the half of a claim that goes stale in silence, so the
// number is gone and what is left is checked.
//
// Three rules, each derived. Every Test name any production comment of this package cites must be
// a test this package declares -- that rule found a second stale citation the moment it was
// written, tree.go naming a gate that had been renamed. Every comparator spelling cited by a
// comment that NAMES the comparator gate must be in the class that gate derives over this
// package's imports, and the one exempt package must be seen to be outside it. And the file that
// declares the verifier really must be one the cited gate reads, which is the half the wrong count
// was a symptom of.
func TestTheMembershipTagCommentaryNamesGatesThatExistAndAClassThatHoldsItsSpellings(t *testing.T) {
	testNames := regexp.MustCompile(`\bTest[A-Z][A-Za-z0-9_]*\b`)
	qualified := regexp.MustCompile(`\b([a-z][a-z0-9_]*)\.([A-Z][A-Za-z0-9_]*)\b`)

	declared := map[string]bool{}
	for _, path := range packageSourcePaths(t) {
		if !strings.HasSuffix(path, "_test.go") {
			continue
		}
		for _, declaration := range mustParseSource(t, path).file.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if isFunction && function.Recv == nil {
				declared[function.Name.Name] = true
			}
		}
	}
	if !declared[membershipTagComparatorGate] {
		t.Fatalf("this package declares no %s, so the rules below check prose against nothing",
			membershipTagComparatorGate)
	}

	production := parsedProductionSourcesOfThisPackage(t)
	class := dataComparatorsOf(t, "this package's production source", production)
	paths := map[string]string{}
	for _, one := range importsOfSources(production) {
		paths[one.name] = one.path
	}

	cited, claiming, spellings := 0, 0, 0
	for _, path := range packageLevelFunctions(t).files {
		for _, block := range membershipTagCommentBlocks(t, path) {
			for _, name := range testNames.FindAllString(block, -1) {
				cited++
				if !declared[name] {
					t.Errorf("%s cites %s and this package declares no such test; a gate named in prose that does not exist is a claim nobody can check",
						path, name)
				}
			}
			if !strings.Contains(block, membershipTagComparatorGate) {
				continue
			}
			claiming++
			for _, spelling := range qualified.FindAllStringSubmatch(block, -1) {
				imported, isImported := paths[spelling[1]]
				if !isImported {
					// a package this source does not import is a package nothing here can call,
					// so the comment is naming it as prose rather than as a spelling of the ban
					continue
				}
				if imported == theConstantTimePackagePath {
					if slices.Contains(class, spelling[0]) {
						t.Errorf("%s names %s as the sanctioned comparison and the derived class holds it, so the gate it cites would ban the tool guardrail 8 names",
							path, spelling[0])
					}
					continue
				}
				spellings++
				if !slices.Contains(class, spelling[0]) {
					t.Errorf("%s names %s as a spelling %s catches, and the class that gate derives over this package's imports does not hold it: %v",
						path, spelling[0], membershipTagComparatorGate, class)
				}
			}
		}
	}
	if cited == 0 || claiming == 0 || spellings == 0 {
		t.Fatalf("%d test names, %d comment blocks naming %s and %d comparator spellings were read out of this package's commentary, and each of the three rules above runs over one of them",
			cited, claiming, membershipTagComparatorGate, spellings)
	}

	// and the file the claim is about really is one the cited gate reads. This is the half the
	// wrong count was a symptom of: the eighteen name gate scans ./message and never opens this
	// directory, so citing "the package's derived comparator gate" without saying which one made
	// the number wrong and the coverage unstated.
	declaring := ""
	for _, function := range packageLevelFunctions(t).functions {
		if function.name == "verifyMembershipTag" {
			declaring = function.file
		}
	}
	if declaring == "" {
		t.Fatal("this package declares no verifyMembershipTag, so the commentary this gate reads has no subject")
	}
	read := []string{}
	for _, parsed := range production {
		read = append(read, parsed.fileSet.Position(parsed.file.Pos()).Filename)
	}
	if !slices.Contains(read, declaring) {
		t.Errorf("%s declares the membership tag verifier and %s reads %v, which does not include it",
			declaring, membershipTagComparatorGate, read)
	}
	t.Logf("%d test names and %d comparator spellings were checked against %d comparators derived over this package's imports",
		cited, spellings, len(class))
}

// ---------------------------------------------------------------------------
// SealPublicMessage and OpenPublicMessage, RFC 9420 section 6.2
// ---------------------------------------------------------------------------

// framingSealed is one sealed member proposal together with everything needed to open it, built
// once per test so that each test below varies one thing rather than declaring a slightly
// different value. It is framingSigned's arrangement one layer out.
type framingSealed struct {
	crypto        CryptoProvider
	priv          SignaturePrivateKey
	pub           SignaturePublicKey
	groupContext  []byte
	membershipKey []byte
	authContent   *AuthenticatedContent
	message       *PublicMessage
}

func framingSealedMemberProposal(t *testing.T) framingSealed {
	t.Helper()
	crypto := newTestCrypto(t)
	priv, pub, err := crypto.SignatureKeyPair()
	if err != nil {
		t.Fatalf("key pair: %v", err)
	}
	groupContext := framingTestGroupContext(t)
	membershipKey := bytes.Repeat([]byte{0x5a}, crypto.HashSize())
	authContent, err := SignAuthenticatedContent(crypto, priv, WireFormatPublicMessage,
		framingTestProposalContent(), groupContext)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	message, err := SealPublicMessage(crypto, membershipKey, authContent, groupContext)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	return framingSealed{crypto: crypto, priv: priv, pub: pub, groupContext: groupContext,
		membershipKey: membershipKey, authContent: authContent, message: message}
}

// TestPublicMessageSealOpenRoundTrip runs the whole path a peer runs: sign, seal, serialize, parse
// somebody else's octets, open.
//
// The serialization in the middle is what makes this more than a seal-then-open. Every field the
// two authenticators cover has to survive the codec for this to pass, so a codec that dropped one
// or moved one shows up here as an authentication failure rather than as a difference nothing
// compares.
//
// What it CANNOT see is stated so nobody reads it as the guard: it is a symmetry property, so an
// open that skipped either authenticator passes it, and so does a seal that took the tag under the
// wrong key -- both halves would be wrong the same way. The refusal sweeps below are what hold
// those.
func TestPublicMessageSealOpenRoundTrip(t *testing.T) {
	sealed := framingSealedMemberProposal(t)
	encoded, err := syntax.Marshal(sealed.message)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	decoded := PublicMessage{}
	if err := syntax.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	opened, err := OpenPublicMessage(sealed.crypto, sealed.membershipKey, &decoded,
		StaticSignatureKey(sealed.pub), sealed.groupContext)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if opened.WireFormat != WireFormatPublicMessage {
		t.Errorf("opened under wire format %d, want %d", opened.WireFormat, WireFormatPublicMessage)
	}
	if opened.Content.Proposal == nil || opened.Content.Proposal.Remove == nil ||
		opened.Content.Proposal.Remove.Removed != 3 {
		t.Fatalf("opened %+v", opened.Content.Proposal)
	}
}

// TestOpenPublicMessageRefusesEveryFlippedBitOfTheSignature is ValSem010 on this path, and it is
// what says the open verifies the SIGNATURE and not only the tag.
//
// Nothing else in this file states that. The round trip is symmetric, the tag sweep above passes
// unchanged over an open that never reaches VerifyAuthenticatedContent, and the membership tag is
// taken under a key every member of the group holds -- so an open that stopped at the tag would
// accept any member's forgery of any other member's message, which is the whole distinction the
// two authenticators exist to draw.
//
// Every bit rather than one, and every length, for TestPublicMessageRefusesForgedMembershipTag's
// reason. Each row re-computes the membership tag over the tampered content, so what is being
// refused is the signature and not the tag standing in front of it.
func TestOpenPublicMessageRefusesEveryFlippedBitOfTheSignature(t *testing.T) {
	sealed := framingSealedMemberProposal(t)
	signature := sealed.message.Auth.Signature
	if len(signature) == 0 {
		t.Fatal("the sealed message carries no signature, so there is nothing here to flip")
	}
	forged := func(t *testing.T, what string, replacement []byte) {
		t.Helper()
		tampered := *sealed.message
		tampered.Auth = FramedContentAuthData{Signature: replacement}
		tag, err := ComputeMembershipTag(sealed.crypto, sealed.membershipKey,
			tampered.AuthenticatedContent(), sealed.groupContext)
		if err != nil {
			t.Fatalf("%s: the tag over the tampered message: %v", what, err)
		}
		tampered.MembershipTag = tag
		_, err = OpenPublicMessage(sealed.crypto, sealed.membershipKey, &tampered,
			StaticSignatureKey(sealed.pub), sealed.groupContext)
		if !errors.Is(err, errFramedContentBadSignature) {
			t.Fatalf("%s: got %v, want errFramedContentBadSignature", what, err)
		}
	}
	for at := 0; at < len(signature); at += 1 {
		for bit := 0; bit < 8; bit += 1 {
			flipped := append([]byte(nil), signature...)
			flipped[at] ^= 1 << uint(bit)
			forged(t, fmt.Sprintf("bit %d of signature octet %d", bit, at), flipped)
		}
	}
	for n := 0; n <= 2*len(signature); n += 1 {
		if n == len(signature) {
			continue
		}
		resized := make([]byte, n)
		copy(resized, signature)
		forged(t, fmt.Sprintf("a %d byte signature", n), resized)
	}
}

// TestOpenPublicMessageRefusesEveryKeyButTheSendersOwn sweeps the resolver's answer.
//
// A signature verification that was reached but handed the wrong key answers exactly what a
// verification that never happened answers, for a message whose signature is valid: nil. What
// separates them is a key that is not the signer's, and the class of those is every OTHER key --
// so this sweeps freshly generated pairs rather than one, and a truncated and an extended key too,
// which is the length class a verifier that compares prefixes accepts.
func TestOpenPublicMessageRefusesEveryKeyButTheSendersOwn(t *testing.T) {
	sealed := framingSealedMemberProposal(t)
	refused := 0
	for row := 0; row < 8; row += 1 {
		_, other, err := sealed.crypto.SignatureKeyPair()
		if err != nil {
			t.Fatalf("another key pair: %v", err)
		}
		if bytes.Equal(other, sealed.pub) {
			t.Fatalf("the provider answered the sender's own key at row %d", row)
		}
		_, err = OpenPublicMessage(sealed.crypto, sealed.membershipKey, sealed.message,
			StaticSignatureKey(other), sealed.groupContext)
		if !errors.Is(err, errFramedContentBadSignature) {
			t.Fatalf("row %d under another member's key: got %v, want errFramedContentBadSignature", row, err)
		}
		refused += 1
	}
	for n := 0; n <= 2*len(sealed.pub); n += 1 {
		if n == len(sealed.pub) {
			continue
		}
		resized := make([]byte, n)
		copy(resized, sealed.pub)
		_, err := OpenPublicMessage(sealed.crypto, sealed.membershipKey, sealed.message,
			StaticSignatureKey(resized), sealed.groupContext)
		if !errors.Is(err, errFramedContentBadSignature) {
			t.Fatalf("a %d byte key: got %v, want errFramedContentBadSignature", n, err)
		}
		refused += 1
	}
	if refused == 0 {
		t.Fatal("no wrong key was refused, so this observed nothing")
	}
}

// TestOpenPublicMessageAnswersItsResolversRefusalVerbatim states the other half of the resolver's
// contract.
//
// "No key could be found for this sender" is not a signature failure. It is what a receive path
// answers for a message from a leaf that has been removed or was never in the tree, there is
// nothing to verify against, and a caller has a different thing to do about it -- so it is
// answered verbatim rather than collapsed into ValSem010.
func TestOpenPublicMessageAnswersItsResolversRefusalVerbatim(t *testing.T) {
	sealed := framingSealedMemberProposal(t)
	own := errors.New("no key for that leaf")
	asked := 0
	_, err := OpenPublicMessage(sealed.crypto, sealed.membershipKey, sealed.message,
		func(sender Sender) (SignaturePublicKey, error) {
			asked += 1
			if sender.SenderType != SenderTypeMember || sender.LeafIndex != sealed.message.Content.Sender.LeafIndex {
				t.Errorf("the resolver was asked about %+v, want the message's own sender %+v",
					sender, sealed.message.Content.Sender)
			}
			return nil, own
		}, sealed.groupContext)
	if !errors.Is(err, own) {
		t.Fatalf("got %v, want the resolver's own refusal", err)
	}
	if errors.Is(err, errFramedContentBadSignature) {
		t.Error("the resolver's refusal answers to the signature refusal, so a caller cannot tell a missing key from a forgery")
	}
	if asked != 1 {
		t.Errorf("the resolver was asked %d times, want once", asked)
	}
}

// TestOpenPublicMessageRefusesAContentSignedUnderAnotherWireFormat is what the wire format is doing
// inside the section 6.1 preimage, observed at this layer.
//
// The message is a real signature over a real FramedContent, re-framed as a PublicMessage by a
// peer that has both. Its membership tag is recomputed over the PUBLIC view, so the tag verifies
// and what refuses the message is the signature -- which is the only thing left to refuse it, and
// the reason the wire format is bound into the preimage rather than merely carried beside it.
func TestOpenPublicMessageRefusesAContentSignedUnderAnotherWireFormat(t *testing.T) {
	sealed := framingSealedMemberProposal(t)
	registry := registryConstantsOfType(t, "WireFormat")
	refused := 0
	for _, name := range slices.Sorted(maps.Keys(registry)) {
		wireFormat := WireFormat(registry[name])
		if wireFormat == WireFormatPublicMessage {
			continue
		}
		elsewhere, err := SignAuthenticatedContent(sealed.crypto, sealed.priv, wireFormat,
			framingTestProposalContent(), sealed.groupContext)
		if err != nil {
			t.Fatalf("%s: sign: %v", name, err)
		}
		replayed := &PublicMessage{Content: elsewhere.Content, Auth: elsewhere.Auth}
		tag, err := ComputeMembershipTag(sealed.crypto, sealed.membershipKey,
			replayed.AuthenticatedContent(), sealed.groupContext)
		if err != nil {
			t.Fatalf("%s: the tag over the replayed message: %v", name, err)
		}
		replayed.MembershipTag = tag
		_, err = OpenPublicMessage(sealed.crypto, sealed.membershipKey, replayed,
			StaticSignatureKey(sealed.pub), sealed.groupContext)
		if !errors.Is(err, errFramedContentBadSignature) {
			t.Fatalf("%s: got %v, want errFramedContentBadSignature", name, err)
		}
		refused += 1
	}
	if refused == 0 {
		t.Fatal("no other wire format was replayed, so this observed nothing")
	}
}

// TestSealPublicMessageRefusesEveryWireFormatButItsOwn is the send side of the same binding.
//
// A caller that signed under one format and sealed under another would ship a signature that
// verifies against neither, and the failure would surface at every peer as ValSem010 rather than as
// the caller's own mistake. Refused rather than re-stamped, for framedContentTBS's reason: a
// re-stamp would sign bytes describing a message the caller did not build.
func TestSealPublicMessageRefusesEveryWireFormatButItsOwn(t *testing.T) {
	sealed := framingSealedMemberProposal(t)
	registry := registryConstantsOfType(t, "WireFormat")
	refused, accepted := 0, 0
	for _, name := range slices.Sorted(maps.Keys(registry)) {
		wireFormat := WireFormat(registry[name])
		authContent, err := SignAuthenticatedContent(sealed.crypto, sealed.priv, wireFormat,
			framingTestProposalContent(), sealed.groupContext)
		if err != nil {
			t.Fatalf("%s: sign: %v", name, err)
		}
		_, err = SealPublicMessage(sealed.crypto, sealed.membershipKey, authContent, sealed.groupContext)
		if wireFormat == WireFormatPublicMessage {
			if err != nil {
				t.Fatalf("%s: seal: %v", name, err)
			}
			accepted += 1
			continue
		}
		if !errors.Is(err, ErrWireFormatMismatch) {
			t.Fatalf("%s: got %v, want ErrWireFormatMismatch", name, err)
		}
		refused += 1
	}
	if refused == 0 || accepted != 1 {
		t.Fatalf("the sweep refused %d wire formats and accepted %d; with either half empty this states one rule rather than two",
			refused, accepted)
	}
}

// TestSealAndOpenCarryEverySenderTypeSectionSixTwoAdmits sweeps the sender type registry through
// both halves.
//
// The membership tag arm is the thing being swept. Section 6.2 gives the field to a member and to
// nobody else, because nobody else has a membership_key: an external sender has no leaf, and a new
// member has not joined. A seal that attached one anyway produces a message every other
// implementation refuses at the field after it; a seal that attached none produces a member's
// message with one of its two authenticators missing. A single-arm test is passed by both.
//
// Which sender types bind the group context comes off senderBindsGroupContext rather than off a
// list here, so the two halves cannot drift: the preimage's own rule decides what this test
// supplies.
func TestSealAndOpenCarryEverySenderTypeSectionSixTwoAdmits(t *testing.T) {
	sealed := framingSealedMemberProposal(t)
	registry := registryConstantsOfType(t, "SenderType")
	swept := 0
	for _, name := range slices.Sorted(maps.Keys(registry)) {
		senderType := SenderType(registry[name])
		binds, err := senderBindsGroupContext(senderType)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		groupContext := []byte(nil)
		if binds {
			groupContext = sealed.groupContext
		}
		content := framingTestProposalContent()
		content.Sender = *testSenderOfType(senderType)
		authContent, err := SignAuthenticatedContent(sealed.crypto, sealed.priv,
			WireFormatPublicMessage, content, groupContext)
		if err != nil {
			t.Fatalf("%s: sign: %v", name, err)
		}
		message, err := SealPublicMessage(sealed.crypto, sealed.membershipKey, authContent, groupContext)
		if err != nil {
			t.Fatalf("%s: seal: %v", name, err)
		}
		carries := senderType == SenderTypeMember
		if got := len(message.MembershipTag) != 0; got != carries {
			t.Errorf("%s: the sealed message carries a membership tag = %v, want %v", name, got, carries)
			continue
		}
		opened, err := OpenPublicMessage(sealed.crypto, sealed.membershipKey, message,
			StaticSignatureKey(sealed.pub), groupContext)
		if err != nil {
			t.Fatalf("%s: open: %v", name, err)
		}
		if opened.Content.Sender.SenderType != senderType {
			t.Errorf("%s: opened a message from sender type %d", name, opened.Content.Sender.SenderType)
			continue
		}
		swept += 1
	}
	if swept != len(registry) {
		t.Fatalf("%d of the %d sender types were carried through seal and open", swept, len(registry))
	}
}

// TestStaticSignatureKeyAnswersOneKeyForEverySender states what that resolver is and, by stating
// it, states what it must not be used for.
//
// It answers the same key whatever the sender says, which is right for the published vectors and
// for a two party test and is wrong for a group: a receive path wired to this would accept any
// member's message under any other member's leaf index. It is swept over the sender type registry
// so that "for every sender" is the class rather than the one sender somebody passed.
func TestStaticSignatureKeyAnswersOneKeyForEverySender(t *testing.T) {
	pub := SignaturePublicKey(bytes.Repeat([]byte{0x7c}, 32))
	resolve := StaticSignatureKey(pub)
	registry := registryConstantsOfType(t, "SenderType")
	for _, name := range slices.Sorted(maps.Keys(registry)) {
		answered, err := resolve(*testSenderOfType(SenderType(registry[name])))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !bytes.Equal(answered, pub) {
			t.Errorf("%s: answered %x, want %x", name, answered, pub)
		}
	}
}

// ---------------------------------------------------------------------------
// the orderings section 6.2 requires of the two doors
// ---------------------------------------------------------------------------

// framingCountingProvider counts the work each of the two authenticators asks the provider for.
//
// It is what turns an ordering claim into an observation. Every refusal below is reachable in
// more than one order and several of them answer the SAME sentinel whichever order they are
// checked in -- a corrupt tag beside a valid signature is refused as a bad tag whether the
// signature was verified first or not -- so the error alone cannot say what ran. What says it is
// the count at the moment the refusal came back: OpenPublicMessage's own comment claims a
// receiver does no public key work on behalf of any party that can reach the transport, and that
// claim is about a verification that did not happen rather than about an error.
//
// The methods are promoted from an embedded interface rather than written out, which is the
// opposite of taggingCryptoProvider's decision and is right here for the reason that one is
// right there: this wrapper counts two NAMED operations and is not a stand in for the provider,
// so a method added to the interface arriving here already implemented is exactly what should
// happen.
type framingCountingProvider struct {
	CryptoProvider
	macVerifies int
	verifies    int
}

func (self *framingCountingProvider) MacVerify(key []byte, data []byte, tag []byte) bool {
	self.macVerifies += 1
	return self.CryptoProvider.MacVerify(key, data, tag)
}

func (self *framingCountingProvider) VerifyWithLabel(pub SignaturePublicKey, label string,
	content []byte, sig []byte) error {

	self.verifies += 1
	return self.CryptoProvider.VerifyWithLabel(pub, label, content, sig)
}

// TestOpenPublicMessageRefusesInTheOrderSectionSixTwoRequires holds the two orderings
// OpenPublicMessage's documentation states as security properties.
//
// Both were prose. Measured, not supposed: with the membership tag block and the resolve-then-
// verify block exchanged -- so that a receiver does an Ed25519 verification for anybody who can
// reach the transport before asking whether the message came from inside the group at all -- the
// whole of ./mls/... and ./message/... stayed green. So did moving the ValSem005 refusal from
// ahead of both authenticators to immediately before the successful return, which is the timing
// oracle the same comment says the order exists to close.
//
// Neither reversal is visible from the error alone, which is why every row carries counts. A
// message whose tag is wrong and whose signature is wrong is refused as a bad tag in one order
// and as a bad signature in the other, so the sentinel separates those two; but a message whose
// tag is wrong and whose signature is GOOD answers the tag refusal in both orders, and what
// separates them is that one of them ran a signature verification first. The last row is the
// control and it runs the other way: with the earlier rules passing, the later checks must
// actually happen, so the zeroes above are about the order rather than about a receiver that
// verifies nothing.
func TestOpenPublicMessageRefusesInTheOrderSectionSixTwoRequires(t *testing.T) {
	sealed := framingSealedMemberProposal(t)
	forged := bytes.Clone(sealed.message.Auth.Signature)
	if len(forged) == 0 {
		t.Fatal("the sealed message carries no signature, so no row below is a message with two bad authenticators")
	}
	forged[0] ^= 0x01
	wrongTag := bytes.Repeat([]byte{0x77}, sealed.crypto.HashSize())

	// an application message a hostile peer framed in the clear, carrying authenticators that
	// are both wrong. ValSem005 needs no key to see, so nothing is spent on it.
	hostile := &PublicMessage{
		Content:       *framingTestMemberContent(),
		Auth:          FramedContentAuthData{Signature: forged},
		MembershipTag: wrongTag,
	}
	// a member's proposal whose tag is wrong and whose signature is wrong
	bothWrong := *sealed.message
	bothWrong.Auth = FramedContentAuthData{Signature: forged}
	bothWrong.MembershipTag = wrongTag
	// the same message carrying no tag at all
	tagless := bothWrong
	tagless.MembershipTag = nil
	// and one whose tag verifies over its own view and whose signature does not
	tagged := *sealed.message
	tagged.Auth = FramedContentAuthData{Signature: forged}
	honestTag, err := ComputeMembershipTag(sealed.crypto, sealed.membershipKey,
		tagged.AuthenticatedContent(), sealed.groupContext)
	if err != nil {
		t.Fatalf("the tag over the forged message: %v", err)
	}
	tagged.MembershipTag = honestTag

	for _, row := range []struct {
		what        string
		message     *PublicMessage
		want        error
		macVerifies int
		verifies    int
		resolved    int
	}{
		{what: "an application message whose tag and signature are both wrong",
			message: hostile, want: errApplicationMustBeCiphertext},
		{what: "a member's proposal whose tag and signature are both wrong",
			message: &bothWrong, want: errBadMembershipTag, macVerifies: 1},
		{what: "a member's proposal carrying no tag, whose signature is wrong",
			message: &tagless, want: errMissingMembershipTag},
		{what: "a member's proposal whose tag verifies and whose signature does not",
			message: &tagged, want: errFramedContentBadSignature, macVerifies: 1, verifies: 1, resolved: 1},
	} {
		counting := &framingCountingProvider{CryptoProvider: sealed.crypto}
		resolved := 0
		_, err := OpenPublicMessage(counting, sealed.membershipKey, row.message,
			func(sender Sender) (SignaturePublicKey, error) {
				resolved += 1
				return sealed.pub, nil
			}, sealed.groupContext)
		if !errors.Is(err, row.want) {
			t.Errorf("%s: got %v, want %v; section 6.2 refuses these in one order and this is not it",
				row.what, err, row.want)
			continue
		}
		if counting.macVerifies != row.macVerifies || counting.verifies != row.verifies || resolved != row.resolved {
			t.Errorf("%s: refused after %d mac verification(s), %d signature verification(s) and %d key resolution(s), want %d, %d and %d; a receiver that does public key work before the cheap keyless rules does it on behalf of anybody who can reach the transport",
				row.what, counting.macVerifies, counting.verifies, resolved,
				row.macVerifies, row.verifies, row.resolved)
			continue
		}
	}
}

// TestSealPublicMessageRefusesApplicationContentAheadOfEveryWireFormatMismatch is the send side's
// order, over the wire format registry rather than over the one row somebody would have written.
//
// Unlike the open's two orderings this one was claimed nowhere, which is why it could be
// exchanged with the suite green: an application message signed under a wire format that is not
// public breaks both of SealPublicMessage's message rules at once, and nothing said which answers.
// It answers ValSem005, which is the receive path's order -- a caller framing an application
// message in the clear is told the same rule by its own send path that every peer would tell it,
// rather than being sent to fix a wire format and then told about the content type on the next
// call.
//
// The second half of each row is the control that makes the first half a statement about
// PRECEDENCE. With the content type fixed at proposal the same wire formats are refused by the
// format check, so a seal that answered ValSem005 to everything would fail here.
func TestSealPublicMessageRefusesApplicationContentAheadOfEveryWireFormatMismatch(t *testing.T) {
	sealed := framingSealedMemberProposal(t)
	registry := registryConstantsOfType(t, "WireFormat")
	refused := 0
	for _, name := range slices.Sorted(maps.Keys(registry)) {
		wireFormat := WireFormat(registry[name])
		if wireFormat == WireFormatPublicMessage {
			continue
		}
		application, err := SignAuthenticatedContent(sealed.crypto, sealed.priv, wireFormat,
			framingTestMemberContent(), sealed.groupContext)
		if err != nil {
			t.Fatalf("%s: sign the application message: %v", name, err)
		}
		_, err = SealPublicMessage(sealed.crypto, sealed.membershipKey, application, sealed.groupContext)
		if !errors.Is(err, errApplicationMustBeCiphertext) {
			t.Errorf("%s: an application message signed under this wire format was refused with %v, want ValSem005; both rules refuse it and the protocol's rule is the one a peer would raise",
				name, err)
			continue
		}
		proposal, err := SignAuthenticatedContent(sealed.crypto, sealed.priv, wireFormat,
			framingTestProposalContent(), sealed.groupContext)
		if err != nil {
			t.Fatalf("%s: sign the proposal: %v", name, err)
		}
		_, err = SealPublicMessage(sealed.crypto, sealed.membershipKey, proposal, sealed.groupContext)
		if !errors.Is(err, ErrWireFormatMismatch) {
			t.Errorf("%s: a proposal signed under this wire format was refused with %v, want ErrWireFormatMismatch; without this half the row above says only that the seal refuses everything with one error",
				name, err)
			continue
		}
		refused += 1
	}
	if refused == 0 {
		t.Fatal("no wire format but the public one was swept, so this observed nothing")
	}
}

// TestSealAndOpenCarryEveryContentTypeSectionSixTwoAdmits sweeps the content type registry through
// the whole path a peer runs: sign, seal, serialize, parse somebody else's octets, open.
//
// The COMMIT row is what this exists for. Every seal and open test in this file builds its message
// out of framingTestProposalContent, and the sender type sweep beside it holds the content type
// fixed while it varies the sender -- so the confirmation_tag arm of section 6.2 was exercised on
// this path by nothing, in either half of the codec and at either door. Two independent ways of
// dropping a public commit's confirmation tag survived the whole of ./mls/... and ./message/...:
// framing the auth data under a hardcoded proposal content type in both halves of the codec, and
// a view that rebuilt the auth data with the signature alone. A public commit whose confirmation
// tag is missing from the wire and from both preimages is a commit carrying no binding to the
// transcript it confirms, and under A-ASSUME-4 the only place this code runs is interop, where
// nothing in this package is there to notice.
//
// The serialization in the middle is not decoration. It is what makes the codec part of the
// claim: a field the encoder drops is a field the membership tag no longer covers at the far end,
// so it arrives as an authentication failure rather than as a difference nothing compares.
//
// ValSem005's row is the application one, refused at the seal, which is where the registry sweep
// and the rule meet: every registered content type is carried by this path except the one the RFC
// forbids in the clear.
func TestSealAndOpenCarryEveryContentTypeSectionSixTwoAdmits(t *testing.T) {
	sealed := framingSealedMemberProposal(t)
	registry := registryConstantsOfType(t, "ContentType")
	carried, refused := 0, 0
	for _, name := range slices.Sorted(maps.Keys(registry)) {
		contentType := ContentType(registry[name])
		authContent, err := SignAuthenticatedContent(sealed.crypto, sealed.priv,
			WireFormatPublicMessage, framingTestContentOfType(t, contentType), sealed.groupContext)
		if err != nil {
			t.Fatalf("%s: sign: %v", name, err)
		}
		if contentType == ContentTypeCommit {
			// the tag is the committer's and is set after the signature, which is
			// SignAuthenticatedContent's contract: a commit's confirmation tag is a mac over a
			// transcript hash taken over this very signature, so it cannot exist until the
			// signature does.
			authContent.Auth.ConfirmationTag = bytes.Repeat([]byte{0xc7}, sealed.crypto.HashSize())
		}
		message, err := SealPublicMessage(sealed.crypto, sealed.membershipKey, authContent, sealed.groupContext)
		if contentType == ContentTypeApplication {
			if !errors.Is(err, errApplicationMustBeCiphertext) {
				t.Errorf("%s: seal: got %v, want ValSem005", name, err)
				continue
			}
			refused += 1
			continue
		}
		if err != nil {
			t.Fatalf("%s: seal: %v", name, err)
		}
		encoded, err := syntax.Marshal(message)
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		decoded := PublicMessage{}
		if err := syntax.Unmarshal(encoded, &decoded); err != nil {
			t.Fatalf("%s: unmarshal: %v", name, err)
		}
		opened, err := OpenPublicMessage(sealed.crypto, sealed.membershipKey, &decoded,
			StaticSignatureKey(sealed.pub), sealed.groupContext)
		if err != nil {
			t.Errorf("%s: open: %v", name, err)
			continue
		}
		if opened.Content.ContentType != contentType {
			t.Errorf("%s: opened a message of content type %d", name, opened.Content.ContentType)
			continue
		}
		if !bytes.Equal(opened.Auth.ConfirmationTag, authContent.Auth.ConfirmationTag) {
			t.Errorf("%s: opened carrying the confirmation tag %x, want the one that was sealed, %x; that tag is inside both preimages of section 6.2, so a message that arrives without it was authenticated without it at both ends",
				name, opened.Auth.ConfirmationTag, authContent.Auth.ConfirmationTag)
			continue
		}
		carried += 1
	}
	if carried == 0 || refused == 0 || carried+refused != len(registry) {
		t.Fatalf("%d of the %d registered content types were carried through seal and open and %d were refused; with either half short this states one arm rather than the rule",
			carried, len(registry), refused)
	}
}

// TestOpenPublicMessageRefusesAMembershipTagOnASenderTypeSectionSixTwoGivesNone is the arm of
// section 6.2's select that had no answer.
//
// The open read the sender type, took the no-tag branch, and left message.MembershipTag read by
// nothing and refused by nothing -- so a caller holding an external sender's message with a tag
// on it was handed back a verified object, believing two authenticators had been checked when one
// had. It is not reachable from the wire, because the codec's own select reads the field off the
// member arm alone; it is reachable from this package's callers, which is the half a codec guard
// does not cover.
//
// The refusal is stated over a tag that WOULD verify as well as over an arbitrary run of octets,
// and that row is the point: what is being refused is the presence of the field and not the value
// in it. There is no key any of these three sender types holds that a tag could have been taken
// under -- an external sender has no leaf and a new member has not joined -- so "verify it
// instead" is not an available third answer.
//
// Both controls run beside it. The same message with no tag opens, so what refused it above is
// the tag; and every spelling of an empty byte run opens too, which is emptyByteSpellings' rule
// in the direction that matters here -- a decoder hands back a non nil empty slice and a caller
// can re-slice one to nothing, and neither of those is a tag anybody attached.
func TestOpenPublicMessageRefusesAMembershipTagOnASenderTypeSectionSixTwoGivesNone(t *testing.T) {
	sealed := framingSealedMemberProposal(t)
	registry := registryConstantsOfType(t, "SenderType")
	refused, members := 0, 0
	for _, name := range slices.Sorted(maps.Keys(registry)) {
		senderType := SenderType(registry[name])
		binds, err := senderBindsGroupContext(senderType)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		groupContext := []byte(nil)
		if binds {
			groupContext = sealed.groupContext
		}
		content := framingTestProposalContent()
		content.Sender = *testSenderOfType(senderType)
		authContent, err := SignAuthenticatedContent(sealed.crypto, sealed.priv,
			WireFormatPublicMessage, content, groupContext)
		if err != nil {
			t.Fatalf("%s: sign: %v", name, err)
		}
		message, err := SealPublicMessage(sealed.crypto, sealed.membershipKey, authContent, groupContext)
		if err != nil {
			t.Fatalf("%s: seal: %v", name, err)
		}
		if senderType == SenderTypeMember {
			// the member arm is the one that REQUIRES the field, and it is checked here so that
			// this sweep cannot pass by refusing the tag everywhere.
			if len(message.MembershipTag) == 0 {
				t.Errorf("%s: the seal attached no membership tag to a member's message", name)
				continue
			}
			if _, err := OpenPublicMessage(sealed.crypto, sealed.membershipKey, message,
				StaticSignatureKey(sealed.pub), groupContext); err != nil {
				t.Errorf("%s: open: %v", name, err)
				continue
			}
			members += 1
			continue
		}
		if len(message.MembershipTag) != 0 {
			t.Errorf("%s: the seal attached a membership tag to a sender type section 6.2 gives none", name)
			continue
		}
		// the control first: with no tag this message opens, so the refusals below are about
		// the field and not about the message.
		if _, err := OpenPublicMessage(sealed.crypto, sealed.membershipKey, message,
			StaticSignatureKey(sealed.pub), groupContext); err != nil {
			t.Errorf("%s: the tagless message did not open: %v", name, err)
			continue
		}
		honest, err := ComputeMembershipTag(sealed.crypto, sealed.membershipKey,
			message.AuthenticatedContent(), groupContext)
		if err != nil {
			t.Fatalf("%s: the tag over this message's own view: %v", name, err)
		}
		for _, spelling := range []struct {
			what  string
			value []byte
		}{
			{what: "a tag this epoch's key would verify", value: honest},
			{what: "an arbitrary run of octets", value: bytes.Repeat([]byte{0x77}, sealed.crypto.HashSize())},
			{what: "a single octet", value: []byte{0x01}},
		} {
			carrying := *message
			carrying.MembershipTag = spelling.value
			_, err := OpenPublicMessage(sealed.crypto, sealed.membershipKey, &carrying,
				StaticSignatureKey(sealed.pub), groupContext)
			if !errors.Is(err, errUnexpectedMembershipTag) {
				t.Errorf("%s carrying %s: got %v, want errUnexpectedMembershipTag; a tag read by nothing is a tag the caller believes was checked",
					name, spelling.what, err)
				continue
			}
			refused += 1
		}
		for _, empty := range emptyByteSpellings() {
			carrying := *message
			carrying.MembershipTag = empty.value
			if _, err := OpenPublicMessage(sealed.crypto, sealed.membershipKey, &carrying,
				StaticSignatureKey(sealed.pub), groupContext); err != nil {
				t.Errorf("%s whose tag is %s: got %v, want the message to open; the guard is on the length, and an empty opaque<V> is not a tag anybody attached",
					name, empty.what, err)
			}
		}
	}
	if refused == 0 || members != 1 {
		t.Fatalf("%d tags were refused on sender types that carry none and %d member arms were carried; with either half empty this states one rule rather than the select",
			refused, members)
	}
}

// ---------------------------------------------------------------------------
// the sender data, RFC 9420 section 6.3.2
// ---------------------------------------------------------------------------

func TestSenderDataRoundTrip(t *testing.T) {
	senderData := SenderData{
		LeafIndex:  1,
		Generation: 7,
		ReuseGuard: [4]byte{0xde, 0xad, 0xbe, 0xef},
	}
	encoded, err := syntax.Marshal(&senderData)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// the golden is hand derived and it is what separates the field ORDER and the raw
	// reuse guard from a codec that agrees with itself: leaf_index 1 and generation 7 are
	// two different numbers, so a codec that swapped them in both halves round trips
	// perfectly and produces 00000007 00000001 here, and a reuse guard written as an
	// opaque<V> produces thirteen octets rather than twelve.
	want := []byte{0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x07, 0xde, 0xad, 0xbe, 0xef}
	if !bytes.Equal(encoded, want) {
		t.Fatalf("encoded %x, want %x", encoded, want)
	}
	var decoded SenderData
	if err := syntax.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded != senderData {
		t.Fatalf("decoded %+v, want %+v", decoded, senderData)
	}
}

// TestCiphertextSampleIsBoundedByHashSize is the plan's regression test against the key-schedule
// plan's SenderDataKeyNonce, kept because this is the caller that ships broken if that derivation
// drifts.
//
// What it is worth on its own is less than its name claims, and that is recorded here rather than
// left for the next reader to rediscover. Its ciphertext is a run of one repeated octet, so a
// sample taken from the WRONG OFFSET reads the same bytes as one taken from the front and this
// test cannot see it; and its truncated arm is a ciphertext of exactly KDF.Nh bytes, which a rule
// that cut at Nh-1 would also cut, so a sample one octet SHORT is invisible to it too. Both were
// measured against the mutants. What it does catch is a sample longer than Nh.
// TestTheSenderDataSampleLocatesBothItsOffsetAndItsLength is the version that puts the boundary
// where the name says it is.
func TestCiphertextSampleIsBoundedByHashSize(t *testing.T) {
	crypto := newTestCrypto(t)
	secret := bytes.Repeat([]byte{0x11}, crypto.HashSize())

	long := bytes.Repeat([]byte{0xab}, crypto.HashSize()+40)
	keyLong, nonceLong, err := SenderDataKeyNonce(crypto, secret, long)
	if err != nil {
		t.Fatalf("long ciphertext: %v", err)
	}
	keyTrunc, nonceTrunc, err := SenderDataKeyNonce(crypto, secret, long[:crypto.HashSize()])
	if err != nil {
		t.Fatalf("truncated ciphertext: %v", err)
	}
	if !bytes.Equal(keyLong, keyTrunc) || !bytes.Equal(nonceLong, nonceTrunc) {
		t.Fatal("sample is not truncated to KDF.Nh")
	}

	// a ciphertext shorter than KDF.Nh must not panic and must use the whole thing
	short := []byte{0x01, 0x02, 0x03}
	keyShort, nonceShort, err := SenderDataKeyNonce(crypto, secret, short)
	if err != nil {
		t.Fatalf("short ciphertext: %v", err)
	}
	if len(keyShort) != crypto.KeySize() || len(nonceShort) != crypto.NonceSize() {
		t.Fatalf("short sample produced key %d nonce %d", len(keyShort), len(nonceShort))
	}
	keyWhole := crypto.ExpandWithLabel(secret, "key", short, crypto.KeySize())
	if !bytes.Equal(keyShort, keyWhole) {
		t.Fatal("short ciphertext sample was padded or truncated")
	}
}

// TestTheSenderDataSampleLocatesBothItsOffsetAndItsLength puts section 6.3.2's sample boundary
// where the sample rule says it is, in both directions, one octet at a time.
//
// The sweep is over a ciphertext of DISTINCT octets, which is the whole reason this test exists
// beside the one above it. A ciphertext that is a run of one value cannot separate
// ciphertext[0..Nh-1] from ciphertext[1..Nh] -- both samples read the same bytes -- so a sample
// taken from the wrong offset derives the right key from the wrong place and every whole-answer
// comparison over a repeated octet agrees with it.
//
// Every octet below KDF.Nh has to change the answer and every octet at or above it has to leave it
// alone, which locates the offset AND the length rather than inferring them from one comparison.
// Three lengths, because a rule whose bound is wrong only past 2*Nh behaves correctly at a long
// ciphertext: Nh+1 is the shortest input the cut applies to at all and is where that family is
// visible.
//
// Why the caller cares, rather than leaving this to the derivation's own plan: a sample of the
// wrong length or from the wrong offset is not a failure. It is real ciphertext, so it derives a
// well formed key of exactly the right width that opens nothing -- and against a peer that made
// the same mistake it interoperates perfectly, which is how it survives a round trip test.
func TestTheSenderDataSampleLocatesBothItsOffsetAndItsLength(t *testing.T) {
	crypto := newTestCrypto(t)
	nh := crypto.HashSize()
	secret := bytes.Repeat([]byte{0x5c}, nh)
	swept := 0
	for _, length := range []int{nh + 1, nh + nh/2, 3 * nh} {
		if length <= nh {
			t.Fatalf("a ciphertext of %d octets is not past KDF.Nh (%d), so this row observes no boundary",
				length, nh)
		}
		ciphertext := make([]byte, length)
		for i := range ciphertext {
			ciphertext[i] = byte(i%251) + 1
		}
		baseKey, baseNonce, err := SenderDataKeyNonce(crypto, secret, ciphertext)
		if err != nil {
			t.Fatalf("SenderDataKeyNonce over %d octets: %v", length, err)
		}
		inside, outside := 0, 0
		for i := range ciphertext {
			altered := bytes.Clone(ciphertext)
			altered[i] ^= 0xff
			key, nonce, err := SenderDataKeyNonce(crypto, secret, altered)
			if err != nil {
				t.Fatalf("SenderDataKeyNonce with octet %d of %d flipped: %v", i, length, err)
			}
			changed := !bytes.Equal(key, baseKey) || !bytes.Equal(nonce, baseNonce)
			if i < nh {
				if !changed {
					t.Errorf("ciphertext of %d octets: flipping octet %d changed nothing, and the sample is ciphertext[0..%d]; the sample is shorter than KDF.Nh or starts past the front",
						length, i, nh-1)
				}
				inside++
				continue
			}
			if changed {
				t.Errorf("ciphertext of %d octets: flipping octet %d changed the answer, and the sample ends at %d; the sample is longer than KDF.Nh or the cut does not fire at this length",
					length, i, nh-1)
			}
			outside++
		}
		if inside != nh || outside != length-nh {
			t.Fatalf("ciphertext of %d octets: the sweep read %d octets inside the sample and %d outside, want %d and %d",
				length, inside, outside, nh, length-nh)
		}
		swept += length
	}

	// the other end of the rule: a ciphertext shorter than KDF.Nh is used WHOLE and never
	// padded. Padding is the plausible mistake and it is a real one -- two short ciphertexts
	// differing only in length would sample identically, which is one keystream over two
	// messages, the reuse the sample exists to prevent reintroduced at the short end.
	short := bytes.Repeat([]byte{0x2a}, nh/2)
	shortKey, _, err := SenderDataKeyNonce(crypto, secret, short)
	if err != nil {
		t.Fatalf("SenderDataKeyNonce over a short ciphertext: %v", err)
	}
	padded := make([]byte, nh)
	copy(padded, short)
	paddedKey, _, err := SenderDataKeyNonce(crypto, secret, padded)
	if err != nil {
		t.Fatalf("SenderDataKeyNonce over the padded ciphertext: %v", err)
	}
	if bytes.Equal(shortKey, paddedKey) {
		t.Error("a short ciphertext derives the same key as itself zero padded to KDF.Nh, so it is being padded rather than used whole")
	}
	shorterKey, _, err := SenderDataKeyNonce(crypto, secret, short[:len(short)-1])
	if err != nil {
		t.Fatalf("SenderDataKeyNonce over a shorter ciphertext: %v", err)
	}
	if bytes.Equal(shortKey, shorterKey) {
		t.Error("two short ciphertexts of different lengths derive one key, which is one keystream over two messages")
	}
	if swept == 0 {
		t.Fatal("no ciphertext length was swept, so this gate located no boundary")
	}
}

// TestTheSenderDataKeyAndNonceAreTheWidthsTheProviderAnswers is the differential this registry
// cannot supply on its own.
//
// Both registered suites fix AEAD.Nn at 12, and the suite every other test in this file runs at
// fixes AEAD.Nk at 32 -- which is also KDF.Nh, and also the literal a body would have written
// down. So inside this registry a hardcoded 32 and a read of KeySize() are the same number and
// nothing above can separate them: measured, KeySize() replaced by 32 and NonceSize() by 12 in
// SenderDataKeyNonce leaves every other test of the section 6.3.2 path passing.
//
// The synthetic suite is the input that separates them, and the row list below is what stops the
// separation going quiet: a width here that coincided with Nk or Nn would be satisfied by the very
// literal this test exists to catch.
func TestTheSenderDataKeyAndNonceAreTheWidthsTheProviderAnswers(t *testing.T) {
	crypto := &suiteCryptoProvider{params: &ksWelcomeSyntheticParams, random: constantReader{value: 0x40}}
	for _, other := range []struct {
		name  string
		value int
	}{
		{name: "this suite's KDF.Nh", value: ksWelcomeSyntheticParams.Nh},
		{name: "the aes suite's Nk", value: 16},
		{name: "the chacha suite's Nk", value: 32},
		{name: "the registry's Nn", value: 12},
		{name: "the registry's KDF.Nh", value: newTestCrypto(t).HashSize()},
	} {
		if other.value == ksWelcomeSyntheticParams.Nk || other.value == ksWelcomeSyntheticParams.Nn {
			t.Fatalf("this suite's Nk is %d and its Nn is %d, and %s is %d; a width that coincides with either leaves the substitution it exists to catch satisfying this test",
				ksWelcomeSyntheticParams.Nk, ksWelcomeSyntheticParams.Nn, other.name, other.value)
		}
	}
	secret := bytes.Repeat([]byte{0x61}, ksWelcomeSyntheticParams.Nh)
	ciphertext := bytes.Repeat([]byte{0x62}, 4*ksWelcomeSyntheticParams.Nh)
	key, nonce, err := SenderDataKeyNonce(crypto, secret, ciphertext)
	if err != nil {
		t.Fatalf("SenderDataKeyNonce over a suite whose KDF.Nh is %d: %v", ksWelcomeSyntheticParams.Nh, err)
	}
	if len(key) != crypto.KeySize() {
		t.Errorf("the sender data key is %d octets and this suite's AEAD.Nk is %d, so the width is written down rather than read off the provider",
			len(key), crypto.KeySize())
	}
	if len(nonce) != crypto.NonceSize() {
		t.Errorf("the sender data nonce is %d octets and this suite's AEAD.Nn is %d, so the width is written down rather than read off the provider",
			len(nonce), crypto.NonceSize())
	}
	// and the values, not merely the lengths: a body that answered the right widths out of the
	// wrong expansion would satisfy everything above.
	sample := ciphertext[:crypto.HashSize()]
	if want := crypto.ExpandWithLabel(secret, "key", sample, crypto.KeySize()); !bytes.Equal(key, want) {
		t.Errorf("the sender data key is %x, want %x", key, want)
	}
	if want := crypto.ExpandWithLabel(secret, "nonce", sample, crypto.NonceSize()); !bytes.Equal(nonce, want) {
		t.Errorf("the sender data nonce is %x, want %x", nonce, want)
	}
}

// senderDataTestHeader is the cleartext PrivateMessage header the seal and open rows below run
// against. It carries authenticated_data, which the sender data AAD must NOT cover -- a header
// with that field empty cannot tell section 6.3.2's AAD from section 6.3.1's.
func senderDataTestHeader() *PrivateMessage {
	return &PrivateMessage{
		GroupId:             []byte{0x01, 0x02},
		Epoch:               9,
		ContentType:         ContentTypeApplication,
		AuthenticatedData:   []byte{0x71, 0x72, 0x73},
		EncryptedSenderData: []byte{0x81},
		Ciphertext:          []byte{0x91, 0x92},
	}
}

func TestSenderDataSealOpen(t *testing.T) {
	crypto := newTestCrypto(t)
	secret := bytes.Repeat([]byte{0x11}, crypto.HashSize())
	ciphertext := bytes.Repeat([]byte{0xab}, 64)
	header := &PrivateMessage{
		GroupId:     []byte{0x01, 0x02},
		Epoch:       9,
		ContentType: ContentTypeApplication,
	}
	senderData := SenderData{LeafIndex: 1, Generation: 7, ReuseGuard: [4]byte{1, 2, 3, 4}}

	sealed, err := sealSenderData(crypto, secret, &senderData, header, ciphertext)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	opened, err := openSenderData(crypto, secret, sealed, header, ciphertext)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if *opened != senderData {
		t.Fatalf("opened %+v, want %+v", *opened, senderData)
	}

	// the epoch is in the AAD, so a rewritten header fails to open
	rewritten := *header
	rewritten.Epoch = 10
	if _, err := openSenderData(crypto, secret, sealed, &rewritten, ciphertext); !errors.Is(err, errDecryptFailed) {
		t.Fatalf("rewritten epoch: got %v, want errDecryptFailed", err)
	}

	// the ciphertext keys the sender data, so a rewritten ciphertext fails too
	other := bytes.Repeat([]byte{0xcd}, 64)
	if _, err := openSenderData(crypto, secret, sealed, header, other); !errors.Is(err, errDecryptFailed) {
		t.Fatalf("rewritten ciphertext: got %v, want errDecryptFailed", err)
	}
}

// senderDataAADParameterNames is the names of senderDataAAD's parameters, read off the source.
//
// This is what makes the sweep below a DERIVATION rather than a list. Which fields of the header
// the sender data is bound to is decided by that function's parameter list and by nothing else --
// it cannot reach a field it was not passed -- so the class of covered fields is read from there,
// and a later task that widens or narrows it moves this test with it rather than leaving a list
// behind that says what somebody once believed.
func senderDataAADParameterNames(t *testing.T) []string {
	t.Helper()
	names := []string{}
	found := false
	for file, parsed := range framingParsedProductionFiles(t) {
		for _, declaration := range parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Recv != nil || function.Name.Name != "senderDataAAD" {
				continue
			}
			if found {
				t.Fatalf("two production files declare senderDataAAD, the second being %s", file)
			}
			found = true
			for _, field := range function.Type.Params.List {
				for _, name := range field.Names {
					names = append(names, name.Name)
				}
			}
		}
	}
	if !found {
		t.Fatal("no production file of this package declares senderDataAAD, so the covered field class cannot be derived")
	}
	slices.Sort(names)
	return names
}

// TestTheSenderDataAadCoversExactlyTheHeaderFieldsItsParameterListNames sweeps EVERY field of the
// cleartext header and holds what the seal is bound to against what senderDataAAD can see.
//
// The plan's own seal test moves one field -- the epoch -- and states that one is covered. That is
// not the property. A seal built over section 6.3.1's AAD instead of section 6.3.2's is bound to
// authenticated_data as well, agrees with its own open at every input, and passes every round trip
// and every rewritten-epoch check in this file; a seal that dropped group_id from the preimage is
// invisible the same way. What separates them is the whole header, swept, against a class read off
// the source.
//
// The alteration is per TYPE rather than per field, and an unhandled type is a FAILURE rather than
// a skip, so a field added to PrivateMessage by a later task arrives here as a red test instead of
// silently leaving the sweep.
func TestTheSenderDataAadCoversExactlyTheHeaderFieldsItsParameterListNames(t *testing.T) {
	crypto := newTestCrypto(t)
	secret := bytes.Repeat([]byte{0x11}, crypto.HashSize())
	ciphertext := bytes.Repeat([]byte{0xab}, 64)
	senderData := SenderData{LeafIndex: 3, Generation: 11, ReuseGuard: [4]byte{9, 8, 7, 6}}

	header := senderDataTestHeader()
	sealed, err := sealSenderData(crypto, secret, &senderData, header, ciphertext)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	covered := senderDataAADParameterNames(t)
	if len(covered) == 0 {
		t.Fatal("senderDataAAD takes no parameters, so this sweep has no class to hold the header against")
	}
	observed := []string{}
	fields := reflect.TypeOf(PrivateMessage{})
	for i := 0; i < fields.NumField(); i++ {
		name := fields.Field(i).Name
		altered := *header
		target := reflect.ValueOf(&altered).Elem().Field(i)
		switch value := target.Interface().(type) {
		case []byte:
			target.Set(reflect.ValueOf(append(bytes.Clone(value), 0x5a)))
		case uint64:
			target.Set(reflect.ValueOf(value + 1))
		case ContentType:
			// a REGISTERED neighbour, because senderDataAAD refuses an unregistered content
			// type before it writes an octet -- an unregistered one would be answered by that
			// guard rather than by the AEAD, and this row would be observing the wrong refusal.
			if value == ContentTypeApplication {
				target.Set(reflect.ValueOf(ContentTypeProposal))
			} else {
				target.Set(reflect.ValueOf(ContentTypeApplication))
			}
		default:
			t.Fatalf("PrivateMessage.%s is a %s and this sweep alters no value of that type; a header field nothing alters is a field this gate says nothing about",
				name, target.Type())
		}
		if reflect.DeepEqual(altered, *header) {
			t.Fatalf("PrivateMessage.%s was not moved by the alteration, so its row states nothing", name)
		}
		_, err := openSenderData(crypto, secret, sealed, &altered, ciphertext)
		switch {
		case err == nil:
			continue
		case errors.Is(err, errDecryptFailed):
			observed = append(observed, name)
		default:
			t.Fatalf("PrivateMessage.%s rewritten answered %v, which is neither an open nor ValSem006", name, err)
		}
	}
	slices.Sort(observed)

	want := []string{}
	for i := 0; i < fields.NumField(); i++ {
		name := fields.Field(i).Name
		if slices.Contains(covered, strings.ToLower(name[:1])+name[1:]) {
			want = append(want, name)
		}
	}
	slices.Sort(want)
	if len(want) == 0 {
		t.Fatalf("no field of PrivateMessage matched a parameter of senderDataAAD (%v), so the class reader is reading the wrong thing", covered)
	}
	if !slices.Equal(observed, want) {
		t.Errorf("rewriting %v broke the sender data open and senderDataAAD's parameters name %v; a field covered but not named is an AAD wider than section 6.3.2's, and one named but not covered is a header field the seal does not bind",
			observed, want)
	}
}

// TestTheSenderDataSealIsTheSectionSixThreeTwoConstructionAndNotOnlyItsOwnInverse recomputes the
// whole of section 6.3.2 beside the seal and compares.
//
// A seal and an open that are each other's inverse agree at every input whatever they do in
// between: the wrong label, the wrong secret, the sample taken from the wrong place, the AAD
// assembled in the wrong order -- every one of those round trips perfectly and interoperates with
// nobody. So this reads the sealed octets with the pieces the RFC names, assembled here rather
// than borrowed from the code under test, and the answer has to be the sender data's own encoding.
func TestTheSenderDataSealIsTheSectionSixThreeTwoConstructionAndNotOnlyItsOwnInverse(t *testing.T) {
	crypto := newTestCrypto(t)
	secret := bytes.Repeat([]byte{0x11}, crypto.HashSize())
	ciphertext := bytes.Repeat([]byte{0xab}, 64)
	header := senderDataTestHeader()
	senderData := SenderData{LeafIndex: 5, Generation: 2, ReuseGuard: [4]byte{0xa1, 0xa2, 0xa3, 0xa4}}

	sealed, err := sealSenderData(crypto, secret, &senderData, header, ciphertext)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	sample := ciphertext[:crypto.HashSize()]
	key := crypto.ExpandWithLabel(secret, "key", sample, crypto.KeySize())
	nonce := crypto.ExpandWithLabel(secret, "nonce", sample, crypto.NonceSize())
	aad, err := senderDataAAD(header.GroupId, header.Epoch, header.ContentType)
	if err != nil {
		t.Fatalf("the section 6.3.2 aad: %v", err)
	}
	plaintext, err := crypto.AeadOpen(key, nonce, aad, sealed)
	if err != nil {
		t.Fatalf("the sealed sender data does not open under section 6.3.2's own key, nonce and aad: %v", err)
	}
	encoded, err := syntax.Marshal(&senderData)
	if err != nil {
		t.Fatalf("marshal the sender data: %v", err)
	}
	if !bytes.Equal(plaintext, encoded) {
		t.Errorf("the sealed plaintext is %x and the sender data encodes to %x", plaintext, encoded)
	}

	// and the AAD is section 6.3.2's and NOT section 6.3.1's, which is the confusion the two
	// AADs sitting next to each other invites. The content AAD is this one plus
	// authenticated_data, so a seal built over it is its own inverse and differs from this
	// only in bytes no round trip reads.
	contentAAD, err := privateContentAAD(header.GroupId, header.Epoch, header.ContentType,
		header.AuthenticatedData)
	if err != nil {
		t.Fatalf("the section 6.3.1 aad: %v", err)
	}
	if bytes.Equal(aad, contentAAD) {
		t.Fatal("the two section 6.3 aads are equal at this header, so this row cannot separate them")
	}
	if _, err := crypto.AeadOpen(key, nonce, contentAAD, sealed); err == nil {
		t.Error("the sender data opens under section 6.3.1's aad, so it was sealed under the content's associated data rather than the header's")
	}
}

// TestOpenSenderDataRefusesAPlaintextThatIsNotExactlyASenderData is the full consumption half.
//
// syntax.Unmarshal joins the decoder's answer with Done, so twelve good octets followed by a tail
// is refused. An open that reached for a bare Reader and never asked Done would accept an
// unbounded family of encodings of one header -- every one of them decrypting, decoding and
// attributing identically -- and nothing that round trips could see it, because this package would
// only ever produce the twelve octet form itself.
//
// The plaintexts are sealed with the provider directly rather than through sealSenderData, because
// sealSenderData cannot produce them: it marshals a SenderData, which is exactly twelve octets.
// That is the point -- the input this rule exists for is one a conforming sender never sends.
func TestOpenSenderDataRefusesAPlaintextThatIsNotExactlyASenderData(t *testing.T) {
	crypto := newTestCrypto(t)
	secret := bytes.Repeat([]byte{0x11}, crypto.HashSize())
	ciphertext := bytes.Repeat([]byte{0xab}, 64)
	header := senderDataTestHeader()
	senderData := SenderData{LeafIndex: 1, Generation: 7, ReuseGuard: [4]byte{1, 2, 3, 4}}
	encoded, err := syntax.Marshal(&senderData)
	if err != nil {
		t.Fatalf("marshal the sender data: %v", err)
	}

	sample := ciphertext[:crypto.HashSize()]
	key := crypto.ExpandWithLabel(secret, "key", sample, crypto.KeySize())
	nonce := crypto.ExpandWithLabel(secret, "nonce", sample, crypto.NonceSize())
	aad, err := senderDataAAD(header.GroupId, header.Epoch, header.ContentType)
	if err != nil {
		t.Fatalf("the section 6.3.2 aad: %v", err)
	}
	sealAs := func(plaintext []byte) []byte {
		t.Helper()
		blob, sealErr := crypto.AeadSeal(key, nonce, aad, plaintext)
		if sealErr != nil {
			t.Fatalf("seal %x: %v", plaintext, sealErr)
		}
		return blob
	}

	// the control: the exact encoding opens, so every refusal below is about the plaintext's
	// length and not about a key, a nonce or an aad this row got wrong.
	opened, err := openSenderData(crypto, secret, sealAs(encoded), header, ciphertext)
	if err != nil {
		t.Fatalf("the exact twelve octet encoding was refused: %v", err)
	}
	if *opened != senderData {
		t.Fatalf("opened %+v, want %+v", *opened, senderData)
	}

	for _, row := range []struct {
		what      string
		plaintext []byte
		sentinel  error
	}{
		{what: "one trailing octet", plaintext: append(bytes.Clone(encoded), 0x00), sentinel: syntax.ErrTrailingBytes},
		{what: "a whole second sender data appended", plaintext: append(bytes.Clone(encoded), encoded...), sentinel: syntax.ErrTrailingBytes},
		{what: "one octet short", plaintext: encoded[:len(encoded)-1], sentinel: syntax.ErrTruncated},
		{what: "empty", plaintext: nil, sentinel: syntax.ErrTruncated},
	} {
		got, err := openSenderData(crypto, secret, sealAs(row.plaintext), header, ciphertext)
		if !errors.Is(err, row.sentinel) {
			t.Errorf("a sender data plaintext with %s answered %v, want %v; a header this package accepts in two encodings is one a peer can rewrite without breaking",
				row.what, err, row.sentinel)
		}
		if got != nil {
			t.Errorf("a sender data plaintext with %s was refused and still answered %+v", row.what, *got)
		}
	}
}

// ---------------------------------------------------------------------------
// PrivateMessageContent and its padding, RFC 9420 section 6.3.1
// ---------------------------------------------------------------------------

// marshalPrivateMessageContent serializes a PrivateMessageContent with paddingSize zero octets
// after the auth data, which is the count form of the section 6.3.1 serializer.
//
// It lives HERE and not in framing_protect.go, and that is a deviation from the plan's file layout
// with a reason rather than a preference. Nothing in production takes a padding count at this
// level: SealPrivateMessage is where a size enters the package, and it turns one into octets and
// hands those down, so a count form in framing_protect.go is a declaration nothing in production
// names -- which TestNoStubShapesRemainInSource refuses, and refuses correctly, because a body
// nothing reaches is a body nothing checks. The signature and the refusal are the plan's
// unchanged, so a later plan's test that calls it compiles against what it was promised.
//
// The negative refusal is duplicated here rather than dropped so this helper cannot hand a test a
// panic where the production entry point hands a caller an error.
func marshalPrivateMessageContent(content *FramedContent, auth *FramedContentAuthData,
	paddingSize int) ([]byte, error) {

	if paddingSize < 0 {
		return nil, ErrInvalidPaddingSize
	}
	return marshalPrivateMessageContentWithPadding(content, auth, make([]byte, paddingSize))
}

// framingPrivateHeaderFor is the cleartext PrivateMessage header that goes with one framed
// content: the five fields section 6.3.1 leaves outside the ciphertext.
//
// The decoder is handed these rather than reading them out of the plaintext, so a header built
// some other way would be reassembling a different message from the same octets. Every test
// below builds it through this function for that reason.
func framingPrivateHeaderFor(content *FramedContent) *PrivateMessage {
	return &PrivateMessage{
		GroupId:           content.GroupId,
		Epoch:             content.Epoch,
		ContentType:       content.ContentType,
		AuthenticatedData: content.AuthenticatedData,
	}
}

// framingPrivateAuthFor is the auth data one content type carries.
//
// A commit carries a confirmation tag as well as a signature and the encoder refuses to write one
// without it, so the tail this file pads is TWO fields long for a commit and one for the other
// two. That difference is the reason the padding sweeps below run over every content type instead
// of over the application arm alone: a padding check written against a one field tail is a check
// whose offset arithmetic is wrong for a commit and right for everything the author looked at.
func framingPrivateAuthFor(t *testing.T, crypto CryptoProvider, contentType ContentType) *FramedContentAuthData {
	t.Helper()
	auth := &FramedContentAuthData{Signature: bytes.Repeat([]byte{0x51}, 64)}
	if contentType == ContentTypeCommit {
		auth.ConfirmationTag = bytes.Repeat([]byte{0x52}, crypto.HashSize())
	}
	return auth
}

// framingPrivateContentsOfEveryType is one framed content per REGISTERED content type, read off
// the registry rather than listed.
//
// Listed, this sweep would state a property about the content types somebody remembered. Read off
// the registry it states it about the ones that exist, and a content type added by a later task
// arrives here as a fatal rather than as a silently narrower sweep.
func framingPrivateContentsOfEveryType(t *testing.T) map[ContentType]*FramedContent {
	t.Helper()
	built := map[ContentType]*FramedContent{
		ContentTypeApplication: framingTestMemberContent(),
		ContentTypeProposal:    framingTestProposalContent(),
		ContentTypeCommit:      framingTestCommitContent(),
	}
	registered := registryConstantsOfType(t, "ContentType")
	for name, code := range registered {
		content, held := built[ContentType(code)]
		if !held {
			t.Fatalf("%s is a registered content type and no content is built for it, so every padding sweep in this file runs over a subset of the registry",
				name)
		}
		if content.ContentType != ContentType(code) {
			t.Fatalf("the content built for %s carries content type %d", name, content.ContentType)
		}
	}
	for contentType := range built {
		found := false
		for _, code := range registered {
			if ContentType(code) == contentType {
				found = true
			}
		}
		if !found {
			t.Fatalf("a content is built for content type %d, which no registry of this package holds", contentType)
		}
	}
	return built
}

// framingPrivatePaddingLengths is the padding lengths the sweeps that are QUADRATIC in the
// length run over -- the ones that tamper once per octet and read the whole tail each time.
//
// The ladder is derived off the widths the wire gives a length rather than sampled in the middle,
// which is the rule p4's nonce reuse at 2^32 was missed by. 255, 256 and 257 straddle the octet
// boundary a length-prefixed decoder would carry, and 1024 is past anything a hand written
// constant in a padding check is likely to have been sized for.
//
// It stops at 1025 for a reason that is about cost and not about the class: at 2^17 octets a
// per octet sweep is seventeen billion byte reads. The lengths past the 16 bit boundary are
// framingPrivateWidePaddingLengths, and every sweep that is linear in the length runs over
// that one instead.
func framingPrivatePaddingLengths() []int {
	lengths := []int{0, 1, 2, 3}
	for bits := 4; bits <= 10; bits++ {
		lengths = append(lengths, (1<<bits)-1, 1<<bits, (1<<bits)+1)
	}
	slices.Sort(lengths)
	return slices.Compact(lengths)
}

// framingPrivateWidePaddingLengths carries the same ladder past the 16 bit boundary.
//
// It is a second function rather than more rungs on the first because the sweeps below split by
// COST and not by taste. The per octet refusal is QUADRATIC in the padding length -- one tamper
// per octet, each of them read over the whole tail -- so 2^17 octets of it is seventeen billion
// byte reads and a package that takes a minute becomes one that takes an hour. Every sweep that
// is linear in the length runs over this ladder instead, so no test in this file stops at 1025
// for a reason no rule states.
//
// What the extra rungs are for: the padding tail carries no length prefix of its own, so the only
// widths that could matter to it are the ones a length would be ENCODED in if somebody added one,
// and 2^16 is the one this file could not previously reach.
func framingPrivateWidePaddingLengths() []int {
	lengths := framingPrivatePaddingLengths()
	for bits := 11; bits <= 17; bits++ {
		lengths = append(lengths, (1<<bits)-1, 1<<bits, (1<<bits)+1)
	}
	slices.Sort(lengths)
	return slices.Compact(lengths)
}

// framedContentArmFields is every field of FramedContent that RFC 9420 section 6.3.1 carries
// INSIDE the ciphertext, derived by joining the two structures rather than listed.
//
// A field of FramedContent the cleartext PrivateMessage header also carries is REASSEMBLED by the
// decoder and never decoded: section 6.3.1 leaves the group id, the epoch, the content type and
// the authenticated data outside the ciphertext, and unmarshalPrivateMessageContent copies them
// off the header. What remains is the content arm, and Sender -- which is the one exclusion, and
// it is the RFC's rather than a convenience: section 6.3.2 carries the sender in the ENCRYPTED
// SENDER DATA and not in the content, which is why the decoder takes it as a parameter instead of
// reading it out of these octets.
//
// Derived so that an arm added to FramedContent by a later task arrives in the layout table below
// as a fatal rather than as a plaintext nothing ever wrote down.
func framedContentArmFields(t *testing.T) []string {
	t.Helper()
	elsewhere := map[string]bool{"Sender": true}
	header := reflect.TypeOf(PrivateMessage{})
	for index := 0; index < header.NumField(); index++ {
		elsewhere[header.Field(index).Name] = true
	}
	arms := []string{}
	removed := []string{}
	shape := reflect.TypeOf(FramedContent{})
	for index := 0; index < shape.NumField(); index++ {
		if name := shape.Field(index).Name; !elsewhere[name] {
			arms = append(arms, name)
		} else {
			removed = append(removed, name)
		}
	}
	slices.Sort(arms)
	slices.Sort(removed)
	if len(arms) == 0 {
		t.Fatal("the join of FramedContent against the cleartext header found no content arm at all, so the layout table below would be held to an empty class")
	}
	// the complement, named rather than counted. Everything here but Sender is derived from
	// PrivateMessage's own fields; Sender is put out by hand because section 6.3.2 carries it in
	// the ENCRYPTED SENDER DATA, and no type in this package holds that placement as a field
	// this join could read off it.
	t.Logf("%d content arms %v; %d field(s) of FramedContent removed because the private message carries them outside the content: %v",
		len(arms), arms, len(removed), removed)
	return arms
}

// privateMessageContentGolden is one row of the section 6.3.1 layout table: the arm the row's
// content type selects, a content that carries it, and the octets that arm alone encodes to.
type privateMessageContentGolden struct {
	field   string
	content *FramedContent
	arm     []byte
}

// handDerivedPrivateMessageContentGoldens is RFC 9420 section 6.3.1's PrivateMessageContent
// written from the wire format, not read back out of framing_protect.go.
//
//	struct {
//	    select (PrivateMessageContent.content_type) {
//	        case application:  opaque application_data<V>;
//	        case proposal:     Proposal proposal;
//	        case commit:       Commit commit;
//	    };
//	    FramedContentAuthData auth;
//	    opaque padding[length_of_padding];
//	} PrivateMessageContent;
//
// The octet arithmetic, from the varint length prefix p1 implements: a length below 64 has prefix
// bits 00 and occupies one octet.
//
//	application  application_data<V> over aa bb    -> 02 aa bb
//	proposal     ProposalType remove = 0x0003, then Remove.removed as a uint32 3
//	                                               -> 00 03 00 00 00 03
//	commit       proposals<V> empty -> 00, then optional<UpdatePath> absent -> 00
//	                                               -> 00 00
//
// Every row also carries distinctive values in the fields the body must NOT hold -- a group id, an
// epoch, a sender and an authenticated data -- so the sweep below can move each of them and
// require these octets not to move with them.
func handDerivedPrivateMessageContentGoldens() map[ContentType]privateMessageContentGolden {
	reassembled := func() *FramedContent {
		return &FramedContent{
			GroupId:           []byte{0x67, 0x69},
			Epoch:             4,
			Sender:            Sender{SenderType: SenderTypeMember, LeafIndex: 2},
			AuthenticatedData: []byte{0xad, 0xae},
		}
	}
	application := reassembled()
	application.ContentType = ContentTypeApplication
	application.ApplicationData = []byte{0xaa, 0xbb}

	proposal := reassembled()
	proposal.ContentType = ContentTypeProposal
	proposal.Proposal = &Proposal{ProposalType: ProposalTypeRemove, Remove: &Remove{Removed: 3}}

	commit := reassembled()
	commit.ContentType = ContentTypeCommit
	commit.Commit = &Commit{}

	return map[ContentType]privateMessageContentGolden{
		ContentTypeApplication: {"ApplicationData", application, []byte{0x02, 0xaa, 0xbb}},
		ContentTypeProposal:    {"Proposal", proposal, []byte{0x00, 0x03, 0x00, 0x00, 0x00, 0x03}},
		ContentTypeCommit:      {"Commit", commit, []byte{0x00, 0x00}},
	}
}

// handDerivedPrivateMessageContentAuth is the value handDerivedAuthDataGolden's octets are the
// encoding of, built off authDataVariantPaths rather than off a select written out a second time.
//
// That arm table is the one framing_test.go already holds to the type, so an arm that gained a
// field there gains one here rather than leaving this layout stating the shape it used to have.
func handDerivedPrivateMessageContentAuth(t *testing.T, contentType ContentType) *FramedContentAuthData {
	t.Helper()
	fields, held := authDataVariantPaths[contentType]
	if !held || len(fields) == 0 {
		t.Fatalf("no auth data arm is written down for content type %d, so the layout below would be over an empty tail", contentType)
	}
	auth := &FramedContentAuthData{}
	for _, name := range fields {
		switch name {
		case "Signature":
			auth.Signature = []byte{0x11, 0x22, 0x33}
		case "ConfirmationTag":
			auth.ConfirmationTag = []byte{0x44, 0x55}
		default:
			t.Fatalf("the auth data arm of content type %d carries the field %s and this layout has no octets for it",
				contentType, name)
		}
	}
	return auth
}

// perturbFramedContentField moves one field of a FramedContent to a different value, by SHAPE
// rather than by name.
//
// It fails on a shape it has no move for, deliberately: a field added to FramedContent that this
// could not move is a field the sweep below would otherwise report as absent from the encrypted
// body having never once changed it.
func perturbFramedContentField(t *testing.T, content reflect.Value, name string) {
	t.Helper()
	field := content.FieldByName(name)
	if !field.IsValid() || !field.CanSet() {
		t.Fatalf("%s is not a settable field of FramedContent, so this sweep would move nothing", name)
	}
	switch {
	case field.Type() == reflect.TypeOf([]byte(nil)):
		field.Set(reflect.ValueOf([]byte{0x5a, 0x5b, 0x5c}))
	case field.Kind() == reflect.Uint64:
		field.SetUint(field.Uint() + 0x0102030405060708)
	case field.Type() == reflect.TypeOf(Sender{}):
		field.Set(reflect.ValueOf(Sender{SenderType: SenderTypeExternal, SenderIndex: 0x0b0c0d0e}))
	default:
		t.Fatalf("%s is a %s and this sweep has no move for that shape", name, field.Type())
	}
}

// TestEveryRegisteredContentTypeEncodesToThePrivateMessageContentLayoutSection631Writes holds the
// section 6.3.1 plaintext to the RFC, over a class derived twice and in both directions.
//
// Why a golden and not another round trip. The defect this refuses is the auth data written
// BEFORE the content arm by the encoder and read before it by the decoder -- a SYMMETRIC
// transposition, so every round trip in this package still passes, every encode-then-decode still
// agrees, and what comes out is a plaintext every peer decrypts and then reads as a different
// message. MEASURED before this test existed: that edit failed four tests on the whole branch and
// all four were family 4's -- message_protection_kat_test.go's vector runner, its installation
// gate, its comparator control, and the registry that drives it -- because family 4's three
// private columns are the only foreign PrivateMessages anything on this branch opens. A vector
// runner is a fine catcher and a poor SOLE catcher: it reports a corpus that disagreed rather
// than a layout that moved, and it goes quiet the day the corpus is not vendored.
//
// The class is joined twice, the way TestEveryRegisteredProposalArmEncodesToTheLayoutSection121-
// Writes joins its own: to the ContentType registry, so a fourth content type cannot land without
// a layout, and to the arms of FramedContent that section 6.3.1 carries inside the ciphertext, so
// an arm cannot land without one either. The COMPLEMENT of that join is swept as well -- every
// field of FramedContent that is neither an arm nor the selector must leave these octets unchanged
// when it moves -- because "the sender comes from the sender data" is a claim about what is ABSENT
// from this plaintext, and nothing that round trips can see an absence.
func TestEveryRegisteredContentTypeEncodesToThePrivateMessageContentLayoutSection631Writes(t *testing.T) {
	goldens := handDerivedPrivateMessageContentGoldens()
	covered := slices.Sorted(maps.Keys(goldens))

	declared := []ContentType{}
	for _, value := range registryConstantsOfType(t, "ContentType") {
		declared = append(declared, ContentType(value))
	}
	slices.Sort(declared)
	if !slices.Equal(declared, covered) {
		t.Fatalf("package mls registers the content types %v and this table lays out %v; a content type with no golden is one whose plaintext nothing has written down, and its layout is whatever the encoder happens to do",
			declared, covered)
	}

	arms := framedContentArmFields(t)
	laid := []string{}
	for _, contentType := range covered {
		laid = append(laid, goldens[contentType].field)
	}
	slices.Sort(laid)
	if !slices.Equal(arms, laid) {
		t.Fatalf("section 6.3.1 carries the FramedContent arms %v inside the ciphertext and this table lays out %v; the two derivations disagree, so one of them has stopped describing the structure",
			arms, laid)
	}

	// the complement of the arm join. A field of FramedContent is an arm with a golden, or the
	// selector, or a field this plaintext must not carry -- and there is no fourth kind, so a
	// field added later cannot sit outside all three.
	reassembled := []string{}
	removed := []string{}
	shape := reflect.TypeOf(FramedContent{})
	for index := 0; index < shape.NumField(); index++ {
		name := shape.Field(index).Name
		if name == "ContentType" || slices.Contains(arms, name) {
			removed = append(removed, name)
			continue
		}
		reassembled = append(reassembled, name)
	}
	if len(reassembled) == 0 {
		t.Fatal("every field of FramedContent is an arm or the selector, so the absence sweep below moves nothing")
	}
	t.Logf("%d field(s) this plaintext must not carry %v; %d removed as the arm class or the selector: %v",
		len(reassembled), reassembled, len(removed), removed)

	padding := []byte{0x00, 0x00, 0x00, 0x00}
	for _, contentType := range covered {
		row := goldens[contentType]
		if row.content.ContentType != contentType {
			t.Fatalf("the %s row carries content type %d and is keyed at %d", row.field, row.content.ContentType, contentType)
		}
		if arm := reflect.ValueOf(*row.content).FieldByName(row.field); !arm.IsValid() || arm.IsZero() {
			t.Fatalf("the %s row does not populate the arm it names, so whatever it encodes to is not that arm", row.field)
		}
		auth := handDerivedPrivateMessageContentAuth(t, contentType)
		golden := joinBytes(row.arm, handDerivedAuthDataGolden(contentType), padding)

		encoded, err := marshalPrivateMessageContentWithPadding(row.content, auth, padding)
		if err != nil {
			t.Errorf("%s: marshal: %v", row.field, err)
			continue
		}
		if !bytes.Equal(encoded, golden) {
			t.Errorf("a %s PrivateMessageContent encodes to %x and RFC 9420 section 6.3.1 writes %x; an encoder and a decoder that agree on an order the RFC does not still seal and open every message this package makes, and read every message a peer makes as a different one",
				row.field, encoded, golden)
			continue
		}

		// the absence half, and it is the SOLE catcher of its own class rather than a restatement
		// of the comparison above. Each field the header and the encrypted sender data carry is
		// moved in turn, and these octets must not move with it.
		//
		// MEASURED, because a sweep whose whole class is already covered by the line above it is
		// better deleted: an encoder that writes the group id into the body only when it is longer
		// than the octets this row publishes leaves the comparison above green and every round trip
		// sweep in this file green, and fails here. That is the shape a covert channel written into
		// the plaintext under a CONDITION has, and no golden over the octets one content happens to
		// produce can see it.
		for _, name := range reassembled {
			moved := *row.content
			perturbFramedContentField(t, reflect.ValueOf(&moved).Elem(), name)
			again, err := marshalPrivateMessageContentWithPadding(&moved, auth, padding)
			if err != nil {
				t.Errorf("%s: marshal with %s moved: %v", row.field, name, err)
				continue
			}
			if !bytes.Equal(again, golden) {
				t.Errorf("%s: moving %s changed the section 6.3.1 plaintext to %x; the cleartext header and the encrypted sender data carry that field, and a body holding a second copy of it is a copy no peer reads",
					row.field, name, again)
			}
		}

		// the decode direction, over the HAND WRITTEN octets and not over this encoder's.
		//
		// Recorded as what it is rather than stated as a second guarantee. MEASURED: the symmetric
		// transposition this test exists for fails the comparison ABOVE and leaves every round trip
		// sweep in this file green, and the same transposition on the decode side alone fails those
		// sweeps as well as this -- so this half is the sole catcher of neither, and it is redundant
		// for as long as the comparison above stays byte exact and something else round trips. It is
		// kept because that transitivity is only as good as those two, and because a decoder that
		// refused octets no encoder in this package produces has no other reader on this branch.
		//
		// The sender handed in is deliberately not the one the row carries: section 6.3.1 has no
		// sender in it, so a decoder that produced this row's sender read it somewhere it does
		// not belong.
		sender := Sender{SenderType: SenderTypeMember, LeafIndex: 9}
		decoded, decodedAuth, err := unmarshalPrivateMessageContent(golden, framingPrivateHeaderFor(row.content), sender)
		if err != nil {
			t.Errorf("%s: the hand written layout did not decode: %v", row.field, err)
			continue
		}
		if decoded.Sender != sender {
			t.Errorf("%s: the layout decoded with sender %+v, want the one handed in, %+v", row.field, decoded.Sender, sender)
			continue
		}
		if err := decoded.checkArms(); err != nil {
			t.Errorf("%s: the layout decoded to a content production's own arm rule refuses: %v", row.field, err)
			continue
		}
		if arm := reflect.ValueOf(*decoded).FieldByName(row.field); !arm.IsValid() || arm.IsZero() {
			t.Errorf("%s: the layout decoded with that arm empty, so the decoder read past the body and dropped it", row.field)
			continue
		}
		if !bytes.Equal(decodedAuth.Signature, auth.Signature) ||
			!bytes.Equal(decodedAuth.ConfirmationTag, auth.ConfirmationTag) {
			t.Errorf("%s: the layout's tail decoded as signature %x and tag %x, want %x and %x",
				row.field, decodedAuth.Signature, decodedAuth.ConfirmationTag, auth.Signature, auth.ConfirmationTag)
			continue
		}
		reencoded, err := marshalPrivateMessageContentWithPadding(decoded, decodedAuth, padding)
		if err != nil {
			t.Errorf("%s: re-marshal what the layout decoded to: %v", row.field, err)
			continue
		}
		if !bytes.Equal(reencoded, golden) {
			t.Errorf("%s: what the layout decoded to re-encodes as %x, want %x", row.field, reencoded, golden)
		}
	}
	t.Logf("RFC 9420 section 6.3.1 held over the content types %v and the arms %v, in both directions", covered, arms)
}

// TestPrivateMessageContentRoundTripsEveryPaddingLengthAtEveryContentType is the symmetry
// property, and it is stated for what it CANNOT see as much as for what it can.
//
// It holds three things a round trip can hold: that the padding lengthens the plaintext by exactly
// the number of octets asked for and not by a length prefix as well, that the content survives,
// and that the sender comes back as the one the caller handed in rather than one decoded out of
// the body -- section 6.3.1 does not carry a sender inside the ciphertext, so a decoder that
// produced one got it from somewhere it does not belong.
//
// What it cannot see is a padding check that never runs, because a round trip only ever presents
// zero padding. TestPrivateMessageContentRefusesEveryNonZeroPaddingOctet is that half.
func TestPrivateMessageContentRoundTripsEveryPaddingLengthAtEveryContentType(t *testing.T) {
	crypto := newTestCrypto(t)
	for contentType, content := range framingPrivateContentsOfEveryType(t) {
		auth := framingPrivateAuthFor(t, crypto, contentType)
		header := framingPrivateHeaderFor(content)
		unpadded, err := marshalPrivateMessageContent(content, auth, 0)
		if err != nil {
			t.Fatalf("content type %d: marshal unpadded: %v", contentType, err)
		}
		for _, paddingSize := range framingPrivateWidePaddingLengths() {
			plaintext, err := marshalPrivateMessageContent(content, auth, paddingSize)
			if err != nil {
				t.Fatalf("content type %d padding %d: marshal: %v", contentType, paddingSize, err)
			}
			if len(plaintext) != len(unpadded)+paddingSize {
				t.Fatalf("content type %d padding %d: plaintext is %d octets, want %d",
					contentType, paddingSize, len(plaintext), len(unpadded)+paddingSize)
			}
			if !bytes.Equal(plaintext[:len(unpadded)], unpadded) {
				t.Fatalf("content type %d padding %d: the padded plaintext does not begin with the unpadded one",
					contentType, paddingSize)
			}
			for at, b := range plaintext[len(unpadded):] {
				if b != 0 {
					t.Fatalf("content type %d padding %d: the encoder wrote %#02x at padding octet %d",
						contentType, paddingSize, b, at)
				}
			}

			// the sender the decoder is handed is deliberately NOT the one the content
			// carries, so a decoder that read it out of the plaintext or off the header
			// answers a value this comparison refuses
			sender := Sender{SenderType: SenderTypeMember, LeafIndex: 6}
			decodedContent, decodedAuth, err := unmarshalPrivateMessageContent(plaintext, header, sender)
			if err != nil {
				t.Fatalf("content type %d padding %d: unmarshal: %v", contentType, paddingSize, err)
			}
			if decodedContent.Sender != sender {
				t.Fatalf("content type %d padding %d: sender %+v, want %+v",
					contentType, paddingSize, decodedContent.Sender, sender)
			}
			if decodedContent.ContentType != contentType || decodedContent.Epoch != content.Epoch ||
				!bytes.Equal(decodedContent.GroupId, content.GroupId) ||
				!bytes.Equal(decodedContent.AuthenticatedData, content.AuthenticatedData) {
				t.Fatalf("content type %d padding %d: header fields came back as %+v",
					contentType, paddingSize, decodedContent)
			}
			if !bytes.Equal(decodedContent.ApplicationData, content.ApplicationData) {
				t.Fatalf("content type %d padding %d: application data %q, want %q",
					contentType, paddingSize, decodedContent.ApplicationData, content.ApplicationData)
			}
			if !bytes.Equal(decodedAuth.Signature, auth.Signature) ||
				!bytes.Equal(decodedAuth.ConfirmationTag, auth.ConfirmationTag) {
				t.Fatalf("content type %d padding %d: auth data %+v, want %+v",
					contentType, paddingSize, decodedAuth, auth)
			}
			reEncoded, err := marshalPrivateMessageContent(decodedContent, decodedAuth, paddingSize)
			if err != nil {
				t.Fatalf("content type %d padding %d: re-marshal: %v", contentType, paddingSize, err)
			}
			if !bytes.Equal(reEncoded, plaintext) {
				t.Fatalf("content type %d padding %d: re-encoding the decode gives %x, want %x",
					contentType, paddingSize, reEncoded, plaintext)
			}
		}
	}
}

// TestPrivateMessageContentRefusesEveryNonZeroPaddingOctet is ValSem011, swept over EVERY position
// of EVERY padding length at EVERY content type.
//
// Derived rather than sampled, and the difference is the whole test. A decoder that checks only
// the first padding octet, or only the last, or only the first eight, passes a test that tampers
// at offsets somebody chose; the class here is the padding length, so every such decoder fails at
// some position of some length. The value flipped in is 0x01 rather than 0xff because it is the
// smallest non-zero octet and therefore the one a check written on a truthiness test or on a high
// bit misses.
//
// Non-zero padding is not a cosmetic violation. The padding sits INSIDE the AEAD and OUTSIDE the
// FramedContent the signature is taken over, so a member that writes into it has a covert channel
// of unbounded width that no authenticator in this protocol can see. This refusal is the only
// thing that closes it.
func TestPrivateMessageContentRefusesEveryNonZeroPaddingOctet(t *testing.T) {
	crypto := newTestCrypto(t)
	refused := 0
	for contentType, content := range framingPrivateContentsOfEveryType(t) {
		auth := framingPrivateAuthFor(t, crypto, contentType)
		header := framingPrivateHeaderFor(content)
		for _, paddingSize := range framingPrivatePaddingLengths() {
			if paddingSize == 0 {
				continue
			}
			plaintext, err := marshalPrivateMessageContent(content, auth, paddingSize)
			if err != nil {
				t.Fatalf("content type %d padding %d: marshal: %v", contentType, paddingSize, err)
			}
			for offset := range paddingSize {
				tampered := append([]byte(nil), plaintext...)
				tampered[len(tampered)-paddingSize+offset] = 0x01
				decoded, decodedAuth, err := unmarshalPrivateMessageContent(tampered, header, content.Sender)
				if !errors.Is(err, errNonZeroPadding) {
					t.Fatalf("content type %d padding %d octet %d: got %v, want errNonZeroPadding",
						contentType, paddingSize, offset, err)
				}
				// and NOTHING alongside it. A decoder that answered the content it had just
				// decided not to trust hands a caller that checked the error loosely -- or
				// that logged it and carried on -- the very FramedContent whose padding
				// carries the covert channel this refusal exists to close. Measured: with
				// the refusal returning content and auth beside the error, the whole of
				// ./mls/... and ./message/... stayed green.
				if decoded != nil || decodedAuth != nil {
					t.Fatalf("content type %d padding %d octet %d: refused and answered a content or an auth data alongside",
						contentType, paddingSize, offset)
				}
				refused++
			}
		}
	}
	if refused == 0 {
		t.Fatal("no padding octet was tampered with, so this sweep asserts nothing")
	}
	t.Logf("%d non-zero padding octets refused across every position of every padding length at every content type", refused)
}

// TestPrivateMessageContentRefusesEveryNonZeroOctetValue is the other axis of the same rule: every
// VALUE a padding octet can hold other than zero.
//
// Derived off the width of a byte rather than off a handful of values. A check written as
// `b > 0x7f`, or as a comparison against a particular byte, or one that treated 0x00 and some
// other value as equivalent, refuses most of the range and admits part of it -- and part of the
// range is all an attacker needs, because a covert channel over a restricted alphabet is still a
// covert channel.
func TestPrivateMessageContentRefusesEveryNonZeroOctetValue(t *testing.T) {
	crypto := newTestCrypto(t)
	content := framingTestMemberContent()
	auth := framingPrivateAuthFor(t, crypto, content.ContentType)
	header := framingPrivateHeaderFor(content)
	const paddingSize = 8
	plaintext, err := marshalPrivateMessageContent(content, auth, paddingSize)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	values := 0
	for value := 1; value <= int(^uint8(0)); value++ {
		for offset := range paddingSize {
			tampered := append([]byte(nil), plaintext...)
			tampered[len(tampered)-paddingSize+offset] = byte(value)
			_, _, err := unmarshalPrivateMessageContent(tampered, header, content.Sender)
			if !errors.Is(err, errNonZeroPadding) {
				t.Fatalf("padding octet %d set to %#02x: got %v, want errNonZeroPadding", offset, value, err)
			}
			values++
		}
	}
	if values != paddingSize*int(^uint8(0)) {
		t.Fatalf("the sweep ran %d combinations and the class holds %d", values, paddingSize*int(^uint8(0)))
	}
}

// framingByteFold is one accumulator a padding check could be written as, together with the
// operator that spells it.
type framingByteFold struct {
	name string
	fold func(accumulated byte, next byte) byte
}

// framingCancellingByteFolds derives the accumulators the tail sweep below is about: the binary
// operators the language defines over a byte, filtered to the ones that already refuse a LONE
// non-zero octet.
//
// Derived off the operator set rather than off the operators that occurred to the author, which
// is this project's rule and is here because the defect this exists for is ONE CHARACTER:
// accumulated |= b becoming accumulated ^= b. Every operator listed is one such edit away from
// the one the package ships, and the list is the language's rather than a guess at which edits
// are likely.
//
// The filter is what makes the family the right one. A fold whose zero is ABSORBING -- and, and
// not, multiply, and both shifts -- accepts a tail of a single non-zero octet and is therefore
// already failed by TestPrivateMessageContentRefusesEveryNonZeroOctetValue; carrying it here
// would restate that test rather than add to it. What survives is exactly the family that agrees
// with or on every one octet tail and disagrees with it somewhere above one octet, which is the
// blind spot the single octet sweeps have.
func framingCancellingByteFolds(t *testing.T) []framingByteFold {
	t.Helper()
	candidates := []framingByteFold{
		{name: "or", fold: func(accumulated byte, next byte) byte { return accumulated | next }},
		{name: "xor", fold: func(accumulated byte, next byte) byte { return accumulated ^ next }},
		{name: "and", fold: func(accumulated byte, next byte) byte { return accumulated & next }},
		{name: "andnot", fold: func(accumulated byte, next byte) byte { return accumulated &^ next }},
		{name: "add", fold: func(accumulated byte, next byte) byte { return accumulated + next }},
		{name: "subtract", fold: func(accumulated byte, next byte) byte { return accumulated - next }},
		{name: "multiply", fold: func(accumulated byte, next byte) byte { return accumulated * next }},
		{name: "shift left", fold: func(accumulated byte, next byte) byte { return accumulated << (next & 7) }},
		{name: "shift right", fold: func(accumulated byte, next byte) byte { return accumulated >> (next & 7) }},
	}
	kept := []framingByteFold{}
	for _, candidate := range candidates {
		refusesEverySingle := true
		for value := 1; value <= int(^uint8(0)); value += 1 {
			if candidate.fold(0, byte(value)) == 0 {
				refusesEverySingle = false
			}
		}
		if refusesEverySingle {
			kept = append(kept, candidate)
		}
	}
	if len(kept) < 2 {
		t.Fatalf("the derived fold family holds %d accumulators, so a sweep over it says nothing about any fold but the one this package ships",
			len(kept))
	}
	return kept
}

// framingCovertPayload is one shape the data an attacker hides in the padding comes in.
type framingCovertPayload struct {
	what string
	at   func(at int) byte
}

// framingCovertPayloads is those shapes: a repeated octet, a counter, and text.
//
// Every one of them is non-zero at every position, so a tail built from one is non-zero wherever
// the closing octet lands. Three shapes rather than one because a payload of a single repeated
// value cancels under folds a varying one does not, and the reverse.
func framingCovertPayloads() []framingCovertPayload {
	covert := []byte("this octet is not padding")
	return []framingCovertPayload{
		{what: "one repeated octet", at: func(int) byte { return 0x01 }},
		{what: "a counter", at: func(at int) byte { return byte(at%int(^uint8(0))) + 1 }},
		{what: "text", at: func(at int) byte { return covert[at%len(covert)] }},
	}
}

// framingFoldingPaddingTail builds the tail a member writes when the receiver's padding check
// FOLDS: payload in the leading octets and one closing octet chosen so the fold comes back to
// zero.
//
// This is the attack itself rather than a model of it. A member that wants a covert channel
// writes whatever it likes into the first n-1 octets and then picks the last one to satisfy
// whatever the receiver accumulates -- so the closing octet is found by SEARCHING the byte range
// here rather than by inverting the fold, which keeps it correct for whatever the derived family
// holds instead of for the two folds an author can invert in their head.
//
// It answers false when the fold admits no closing octet, and that is the honest answer for the
// bitwise or: there is no non-zero tail an or accumulator folds to zero, which is the whole
// reason it is the accumulator this package ships. The caller counts what it built rather than
// treating false as a skip, so a family that had quietly gone uncancellable fails instead of
// passing vacuously.
func framingFoldingPaddingTail(length int, fold func(byte, byte) byte, payload func(int) byte) ([]byte, bool) {
	if length < 1 {
		return nil, false
	}
	tail := make([]byte, length)
	var accumulated byte
	for at := range length - 1 {
		tail[at] = payload(at)
		accumulated = fold(accumulated, tail[at])
	}
	for closing := 0; closing <= int(^uint8(0)); closing += 1 {
		if fold(accumulated, byte(closing)) != 0 {
			continue
		}
		tail[length-1] = byte(closing)
		if slices.Max(tail) == 0 {
			// an all zero tail is legal padding and not the attack
			continue
		}
		return tail, true
	}
	return nil, false
}

// framingPaddingPositions derives the octets of a tail a sparse tamper is placed at: every one of
// them while the whole set is small, and otherwise the two ends, the middle, and the octets
// either side of every boundary a length would be encoded across.
//
// Bounded on purpose, and the bound is why it is derived rather than exhaustive: every pair of
// every position of a 2^17 octet tail is eight billion tampers. What the pairs are FOR is the
// position axis -- the value axis is covered exhaustively at length two by the sweep below -- and
// the boundaries are where offset arithmetic written against a narrower length goes wrong.
func framingPaddingPositions(length int) []int {
	if length <= 16 {
		positions := make([]int, length)
		for at := range length {
			positions[at] = at
		}
		return positions
	}
	positions := []int{0, 1, length/2 - 1, length / 2, length - 2, length - 1}
	for bits := 8; bits < 32; bits += 8 {
		positions = append(positions, (1<<bits)-1, 1<<bits)
	}
	slices.Sort(positions)
	positions = slices.Compact(positions)
	inside := []int{}
	for _, at := range positions {
		if 0 <= at && at < length {
			inside = append(inside, at)
		}
	}
	return inside
}

// framingPaddingPositionPairs is every pair of those positions.
func framingPaddingPositionPairs(length int) [][2]int {
	positions := framingPaddingPositions(length)
	pairs := [][2]int{}
	for i := range positions {
		for j := i + 1; j < len(positions); j += 1 {
			pairs = append(pairs, [2]int{positions[i], positions[j]})
		}
	}
	return pairs
}

// framingCancellingOctetPairs derives one two octet tail per fold that that fold accumulates to
// zero, by searching the byte square rather than by writing the pairs down.
//
// One representative per fold rather than all of them, because the axes are separated on purpose:
// every value a two octet tail can hold is visited exhaustively by the whole space sweep below,
// and what this set is spent on is the POSITION axis, which the whole space sweep cannot reach.
func framingCancellingOctetPairs(folds []framingByteFold) [][2]byte {
	seen := map[[2]byte]bool{}
	found := [][2]byte{}
	for _, fold := range folds {
		for first := 1; first <= int(^uint8(0)); first += 1 {
			closed := false
			for second := 1; second <= int(^uint8(0)); second += 1 {
				if fold.fold(fold.fold(0, byte(first)), byte(second)) != 0 {
					continue
				}
				pair := [2]byte{byte(first), byte(second)}
				if !seen[pair] {
					seen[pair] = true
					found = append(found, pair)
				}
				closed = true
				break
			}
			if closed {
				break
			}
		}
	}
	return found
}

// TestPrivateMessageContentRefusesEveryNonZeroPaddingTailNotOnlyTheSingleOctetOnes is the half of
// ValSem011 the per octet and per value sweeps above cannot reach.
//
// Both of those set exactly ONE padding octet non-zero, so the accumulator that decides the
// refusal is only ever handed a single non-zero byte -- and every fold agrees with every other
// fold on a tail like that. Measured: with the accumulator folding by xor instead of by or, the
// whole of ./mls/... and ./message/... stayed green while a tail of eight octets with two of them
// set to 0x41 went from refused to ACCEPTED. That is the covert channel ValSem011 exists to
// close, reopened by one character and invisible to every test in the package.
//
// The class is the TAIL and not the octet, and it is attacked from three sides.
//
// The whole space, at the lengths whose whole space fits: every non-zero tail of one octet and
// every non-zero tail of two. That is exhaustive rather than derived -- the only sweep in this
// file that can be -- and a fold has nowhere in those two lengths to hide, which is what pins the
// VALUE axis for the two sweeps under it.
//
// The attack itself, at every length the wide ladder reaches: n-1 octets of payload and one
// closing octet chosen so the fold returns to zero. That is what a member with a covert channel
// actually writes, and every octet of the tail but one carries data.
//
// And the sparse version of the same thing: two non-zero octets with the rest of the tail zero,
// at position pairs derived off the length, which is the axis the exhaustive sweep cannot reach.
func TestPrivateMessageContentRefusesEveryNonZeroPaddingTailNotOnlyTheSingleOctetOnes(t *testing.T) {
	crypto := newTestCrypto(t)
	folds := framingCancellingByteFolds(t)
	cancelling := framingCancellingOctetPairs(folds)
	// the refusal is checked without formatting anything, because the exhaustive half runs it
	// two hundred thousand times per content type and a Sprintf per iteration costs more than
	// the decoder under it does
	check := func(plaintext []byte, header *PrivateMessage, sender Sender) error {
		content, auth, err := unmarshalPrivateMessageContent(plaintext, header, sender)
		if !errors.Is(err, errNonZeroPadding) {
			return fmt.Errorf("got %v, want errNonZeroPadding", err)
		}
		if content != nil || auth != nil {
			return errors.New("refused and answered a content or an auth data alongside")
		}
		return nil
	}

	exhaustive, closed, sparse := 0, 0, 0
	for contentType, content := range framingPrivateContentsOfEveryType(t) {
		auth := framingPrivateAuthFor(t, crypto, contentType)
		header := framingPrivateHeaderFor(content)
		body, err := marshalPrivateMessageContent(content, auth, 0)
		if err != nil {
			t.Fatalf("content type %d: marshal: %v", contentType, err)
		}

		one := append(append([]byte(nil), body...), 0)
		for value := 1; value <= int(^uint8(0)); value += 1 {
			one[len(body)] = byte(value)
			if err := check(one, header, content.Sender); err != nil {
				t.Fatalf("content type %d, the one octet tail %#02x: %v", contentType, value, err)
			}
			exhaustive += 1
		}
		two := append(append([]byte(nil), body...), 0, 0)
		for first := 0; first <= int(^uint8(0)); first += 1 {
			for second := 0; second <= int(^uint8(0)); second += 1 {
				if first == 0 && second == 0 {
					continue
				}
				two[len(body)], two[len(body)+1] = byte(first), byte(second)
				if err := check(two, header, content.Sender); err != nil {
					t.Fatalf("content type %d, the two octet tail %#02x %#02x: %v",
						contentType, first, second, err)
				}
				exhaustive += 1
			}
		}

		for _, paddingSize := range framingPrivateWidePaddingLengths() {
			if paddingSize < 2 {
				continue
			}
			for _, fold := range folds {
				for _, payload := range framingCovertPayloads() {
					tail, built := framingFoldingPaddingTail(paddingSize, fold.fold, payload.at)
					if !built {
						continue
					}
					plaintext := append(append([]byte(nil), body...), tail...)
					if err := check(plaintext, header, content.Sender); err != nil {
						t.Fatalf("content type %d, %d octets of padding carrying %s and closed under %s: %v",
							contentType, paddingSize, payload.what, fold.name, err)
					}
					closed += 1
				}
			}

			plaintext := append(append([]byte(nil), body...), make([]byte, paddingSize)...)
			tail := plaintext[len(body):]
			for _, at := range framingPaddingPositionPairs(paddingSize) {
				for _, values := range cancelling {
					tail[at[0]], tail[at[1]] = values[0], values[1]
					if err := check(plaintext, header, content.Sender); err != nil {
						t.Fatalf("content type %d, %d octets of padding with %#02x at %d and %#02x at %d: %v",
							contentType, paddingSize, values[0], at[0], values[1], at[1], err)
					}
					tail[at[0]], tail[at[1]] = 0, 0
					sparse += 1
				}
			}
		}
	}
	if closed == 0 {
		t.Fatal("no fold in the derived family admitted a tail that closes, so the sweep over the attack ran nothing")
	}
	if sparse == 0 {
		t.Fatal("no cancelling octet pair was derived, so the sweep over the position axis ran nothing")
	}
	t.Logf("%d tails over the whole space at one and two octets, %d tails closed under a folding accumulator, %d sparse cancelling pairs",
		exhaustive, closed, sparse)
}

// TestPrivateMessageContentPaddingRefusalNamesNoPositionOrValue is the oracle half of ValSem011.
//
// A padding decoder that says WHICH octet offended answers, for every position, the question "were
// all the octets before this one zero" -- which is the padding oracle this whole class of bug is
// named after. The refusal is therefore required to render IDENTICALLY however the padding is
// wrong: same sentinel, same words, same absence of a number.
//
// The comparison is against the refusal for a DIFFERENT position rather than against a written
// down string, so a later task that rewords the sentinel moves this test with it instead of
// leaving a literal behind that says what somebody once believed.
func TestPrivateMessageContentPaddingRefusalNamesNoPositionOrValue(t *testing.T) {
	crypto := newTestCrypto(t)
	content := framingTestMemberContent()
	auth := framingPrivateAuthFor(t, crypto, content.ContentType)
	header := framingPrivateHeaderFor(content)
	const paddingSize = 32
	plaintext, err := marshalPrivateMessageContent(content, auth, paddingSize)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rendered := map[string][]string{}
	for offset := range paddingSize {
		for _, value := range []byte{0x01, 0x02, 0x40, 0x80, 0xff} {
			tampered := append([]byte(nil), plaintext...)
			tampered[len(tampered)-paddingSize+offset] = value
			_, _, err := unmarshalPrivateMessageContent(tampered, header, content.Sender)
			if err == nil {
				t.Fatalf("padding octet %d set to %#02x was accepted", offset, value)
			}
			where := fmt.Sprintf("octet %d set to %#02x", offset, value)
			rendered[err.Error()] = append(rendered[err.Error()], where)
		}
	}
	if len(rendered) != 1 {
		t.Fatalf("the padding refusal renders %d different ways across the tampered positions, which tells a caller where the padding went wrong: %v",
			len(rendered), rendered)
	}
	only := ""
	for text := range rendered {
		only = text
	}
	// and it does not name a position or a length by number either, which a single spelling
	// would still allow if every case happened to render the same digits
	if regexp.MustCompile(`[0-9]`).MatchString(only) {
		t.Errorf("the padding refusal renders as %q, which carries a number; the position and the count are exactly what must not be in it", only)
	}
}

// TestPrivateMessageContentAcceptsEveryAllZeroPaddingLength is the interop half of the rule.
//
// The RFC fixes what padding must CONTAIN and says nothing about how much of it there may be, and
// peers in the harness emit their own. A receiver that refused a length it would not itself have
// produced would fail the harness against implementations doing nothing wrong, so the acceptance
// is a property in its own right rather than the absence of a check.
func TestPrivateMessageContentAcceptsEveryAllZeroPaddingLength(t *testing.T) {
	crypto := newTestCrypto(t)
	for contentType, content := range framingPrivateContentsOfEveryType(t) {
		auth := framingPrivateAuthFor(t, crypto, contentType)
		header := framingPrivateHeaderFor(content)
		body, err := marshalPrivateMessageContent(content, auth, 0)
		if err != nil {
			t.Fatalf("content type %d: marshal: %v", contentType, err)
		}
		for _, paddingSize := range framingPrivateWidePaddingLengths() {
			// assembled here rather than through the encoder, so this states that the
			// DECODER accepts a peer's padding and not merely that it accepts its own
			peer := append(append([]byte(nil), body...), make([]byte, paddingSize)...)
			if _, _, err := unmarshalPrivateMessageContent(peer, header, content.Sender); err != nil {
				t.Fatalf("content type %d: a peer that padded to %d octets was refused: %v",
					contentType, paddingSize, err)
			}
		}
	}
}

// TestSealPrivateMessageRefusesEveryNegativePaddingSize sweeps the negative half of the int
// rather than -1 alone, with the boundary derived off the width of the type.
//
// make([]byte, n) panics for a negative n, so the alternative to this refusal is not a wrong
// answer but a crash inside the runtime with this frame nowhere in the message. The minimum is
// included because a clamp written as `if paddingSize < 0 { paddingSize = 0 }` and an arithmetic
// overflow in a caller meet exactly there.
//
// SealPrivateMessage is the subject: it is the only place in this package a padding SIZE enters
// from a caller. The count form of the serializer is held to the same refusal in the same loop,
// because it is this file's own helper and a helper that panicked where the production path
// refuses would fail a test for a reason that is not the one it is about.
func TestSealPrivateMessageRefusesEveryNegativePaddingSize(t *testing.T) {
	const maxInt = int(^uint(0) >> 1)
	const minInt = -maxInt - 1
	crypto := newTestCrypto(t)
	content := framingTestMemberContent()
	auth := framingPrivateAuthFor(t, crypto, content.ContentType)
	for _, paddingSize := range []int{-1, -2, -3, -256, minInt + 1, minInt} {
		encoded, err := marshalPrivateMessageContent(content, auth, paddingSize)
		if !errors.Is(err, ErrInvalidPaddingSize) {
			t.Fatalf("padding size %d: got %v, want ErrInvalidPaddingSize", paddingSize, err)
		}
		if encoded != nil {
			t.Errorf("padding size %d: refused and answered %x alongside", paddingSize, encoded)
		}
		message, err := SealPrivateMessage(crypto, framingNewKeySource(crypto, 0x01, 0),
			bytes.Repeat([]byte{0x33}, crypto.HashSize()),
			&AuthenticatedContent{WireFormat: WireFormatPrivateMessage, Content: *content}, paddingSize)
		if !errors.Is(err, ErrInvalidPaddingSize) {
			t.Fatalf("seal at padding size %d: got %v, want ErrInvalidPaddingSize", paddingSize, err)
		}
		if message != nil {
			t.Errorf("seal at padding size %d refused and answered a message alongside", paddingSize)
		}
	}
}

// TestPaddingSizeV1IsZeroBecauseTheRecordLayerPads pins the product decision, not the RFC.
//
// MASTER section 8 requires connect/message to pad ct_body to a size bucket, so MLS level padding
// is padding inside padding: it cannot narrow a bucket already rounded up, and every octet of it
// pushes a message that was under a boundary onto the next rung. The constant is what SealPrivate
// Message emits by default, so a later task that "adds some padding for safety" changes an
// observable property of every message this product sends and has to change this line to do it.
func TestPaddingSizeV1IsZeroBecauseTheRecordLayerPads(t *testing.T) {
	if PaddingSizeV1 != 0 {
		t.Fatalf("PaddingSizeV1 = %d; connect/message pads ct_body to a size bucket, so MLS padding must be 0", PaddingSizeV1)
	}
}

// TestUnmarshalPrivateMessageContentRefusesAnUnregisteredContentType is the header arm nothing
// else reaches, and it is run against an EMPTY body as well as a full one.
//
// The content type comes off the CLEARTEXT header, which is to say off the wire, so this switch
// runs on a value an unauthenticated peer chose. A default arm that fell through to the
// application case would decode a proposal's octets as an opaque blob and hand it up as
// application data.
//
// The empty body is what makes this a statement about THIS arm. Against a full plaintext the
// refusal is not attributable: an unregistered content type is refused a second time by
// FramedContentAuthData.UnmarshalMLS a few lines further down, so a decoder whose own default
// fell through to the application case is still refused -- measured, on the mutation that made it
// fall through, which this test passed before the empty row was added. With no octets to read,
// a fall-through answers a TRUNCATION and only an arm that refuses before it consumes anything
// answers the content type.
func TestUnmarshalPrivateMessageContentRefusesAnUnregisteredContentType(t *testing.T) {
	crypto := newTestCrypto(t)
	content := framingTestMemberContent()
	auth := framingPrivateAuthFor(t, crypto, content.ContentType)
	plaintext, err := marshalPrivateMessageContent(content, auth, 0)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	registered := registryConstantsOfType(t, "ContentType")
	for code := uint64(0); code <= uint64(^uint8(0)); code++ {
		isRegistered := false
		for _, held := range registered {
			if held == code {
				isRegistered = true
			}
		}
		if isRegistered {
			continue
		}
		header := framingPrivateHeaderFor(content)
		header.ContentType = ContentType(code)
		for _, body := range []struct {
			what      string
			plaintext []byte
		}{
			{what: "a full body", plaintext: plaintext},
			{what: "no body at all", plaintext: nil},
			{what: "a body of one octet", plaintext: []byte{0x00}},
		} {
			_, _, err := unmarshalPrivateMessageContent(body.plaintext, header, content.Sender)
			if !errors.Is(err, ErrUnknownContentType) {
				t.Fatalf("content type %d with %s: got %v, want ErrUnknownContentType",
					code, body.what, err)
			}
		}
	}
}

// TestUnmarshalPrivateMessageContentAnswersTheAuthDataCodecsRefusalVerbatim is the guard on the
// one refusal of this decoder that nothing downstream repeats.
//
// It exists because the obvious test for it states nothing. A truncated auth data LATCHES the
// Reader, so the padding read a few lines below refuses the plaintext whatever this line does --
// which is why the truncation sweep in the second half is here for completeness rather than as
// the property.
//
// The refusal that is only this line's is a commit carrying an EMPTY confirmation tag. That is
// wire legal, it consumes its octets cleanly and leaves the Reader unlatched, and
// FramedContentAuthData.UnmarshalMLS refuses it because an empty opaque is the encoding of "no
// tag" and a commit with no tag is one every peer rejects at ValSem009 having verified its
// signature first. A decoder that dropped the codec's error hands that plaintext up carrying
// neither a signature nor a confirmation tag, with a nil error, and the padding check under it
// passes because the tail really is zeros.
//
// Measured rather than supposed: with `if err := auth.UnmarshalMLS(...); err != nil` reduced to
// `_ = auth.UnmarshalMLS(...)`, the whole of ./mls/... and ./message/... stayed green. This test
// is what that mutation now fails.
func TestUnmarshalPrivateMessageContentAnswersTheAuthDataCodecsRefusalVerbatim(t *testing.T) {
	content := framingTestCommitContent()
	header := framingPrivateHeaderFor(content)
	for _, paddingSize := range framingPrivatePaddingLengths() {
		w := syntax.NewWriter()
		if err := content.Commit.MarshalMLS(w); err != nil {
			t.Fatalf("the commit arm this row hands the decoder: %v", err)
		}
		w.WriteOpaque(bytes.Repeat([]byte{0x51}, 64))
		// an empty opaque<V> is a legal encoding and is the encoding of "no tag"
		w.WriteOpaque(nil)
		w.WriteRaw(make([]byte, paddingSize))
		plaintext, err := w.Bytes()
		if err != nil {
			t.Fatalf("the plaintext this row hands the decoder: %v", err)
		}
		decoded, auth, err := unmarshalPrivateMessageContent(plaintext, header, content.Sender)
		if !errors.Is(err, errMissingConfirmationTag) {
			t.Fatalf("padding %d: a commit whose confirmation tag is an empty opaque: got %v, want errMissingConfirmationTag",
				paddingSize, err)
		}
		if decoded != nil || auth != nil {
			t.Errorf("padding %d: refused and answered a content or an auth data alongside", paddingSize)
		}
	}

	// and every truncation of a well formed body, derived off its length rather than sampled.
	// These are refused through the latched Reader rather than through the line above, and the
	// reason they are here anyway is that a decoder which stopped latching -- a Reader rewritten
	// to answer per call rather than stickily -- would turn every one of them into a plaintext
	// accepted with a half read auth data, and nothing else in this file would notice.
	crypto := newTestCrypto(t)
	for contentType, whole := range framingPrivateContentsOfEveryType(t) {
		auth := framingPrivateAuthFor(t, crypto, contentType)
		full, err := marshalPrivateMessageContent(whole, auth, 0)
		if err != nil {
			t.Fatalf("content type %d: marshal: %v", contentType, err)
		}
		wholeHeader := framingPrivateHeaderFor(whole)
		for at := range len(full) {
			if _, _, err := unmarshalPrivateMessageContent(full[:at], wholeHeader, whole.Sender); err == nil {
				t.Fatalf("content type %d: a body truncated to %d of %d octets was accepted",
					contentType, at, len(full))
			}
		}
	}
}

// ---------------------------------------------------------------------------
// MessageKeySource, the reuse guard, and section 6.3's seal and open
// ---------------------------------------------------------------------------

// framingKeySource is a MessageKeySource that answers one deterministic key and nonce per
// (contentType, leaf, generation) triple. The real one is the secret tree.
//
// It exists because the properties this file is about -- which nonce a seal used, whether the
// guarded nonce was written back over the ratchet's, what a refusal from the ratchet does to the
// caller -- are not reachable through a real secret tree at the generations they matter at. The
// tree is exercised too, in the tests below that plant a ratchet head, and the two are
// complementary rather than alternatives: this one can be driven to any generation and cannot say
// what the tree does, and the tree can say what the tree does and cannot be stepped four billion
// times.
//
// handed keeps the nonce slices this source gave out, unre-derived, which is what makes a guard
// that wrote through into the ratchet's storage visible: a copy is invisible from the return value
// and shows up only in what the source still holds afterwards.
type framingKeySource struct {
	crypto       CryptoProvider
	seed         byte
	start        uint32
	pinned       bool
	head         map[ContentType]uint32
	erased       []string
	committed    []string
	handed       [][]byte
	refuseNext   error
	refuseLookup error
	refuseCommit error
}

// framingNewKeySource builds a source whose first generation is start and which advances.
func framingNewKeySource(crypto CryptoProvider, seed byte, start uint32) *framingKeySource {
	return &framingKeySource{crypto: crypto, seed: seed, start: start, head: map[ContentType]uint32{}}
}

// framingPinnedKeySource builds one whose generation never advances, so two calls over it answer
// the same key. The provider gates need that: a row whose base call and control call disagree
// states nothing about any argument.
func framingPinnedKeySource(crypto CryptoProvider, seed byte, at uint32) *framingKeySource {
	source := framingNewKeySource(crypto, seed, at)
	source.pinned = true
	return source
}

// derive is a function of the whole triple and of this source's seed, at the provider's own key
// and nonce widths.
//
// The generation enters as all FOUR of its octets. Truncated to one, every generation congruent
// mod 256 would share a key and a nonce -- which is the nonce reuse these tests are written to
// catch, reintroduced inside the instrument that is supposed to see it.
func (self *framingKeySource) derive(contentType ContentType, leaf LeafIndex, generation uint32) (key []byte, nonce []byte) {
	context := []byte{
		self.seed, byte(contentType),
		byte(leaf), byte(leaf >> 8), byte(leaf >> 16), byte(leaf >> 24),
		byte(generation), byte(generation >> 8), byte(generation >> 16), byte(generation >> 24),
	}
	secret := bytes.Repeat([]byte{0x77}, self.crypto.HashSize())
	key = self.crypto.ExpandWithLabel(secret, "framing test message key", context, self.crypto.KeySize())
	nonce = self.crypto.ExpandWithLabel(secret, "framing test message nonce", context, self.crypto.NonceSize())
	return key, nonce
}

func (self *framingKeySource) headOf(contentType ContentType) uint32 {
	if _, held := self.head[contentType]; !held {
		self.head[contentType] = self.start
	}
	return self.head[contentType]
}

func (self *framingKeySource) NextMessageKey(contentType ContentType, leaf LeafIndex) ([]byte, []byte, uint32, error) {
	if self.refuseNext != nil {
		return nil, nil, 0, self.refuseNext
	}
	generation := self.headOf(contentType)
	if !self.pinned {
		self.head[contentType] = generation + 1
	}
	key, nonce := self.derive(contentType, leaf, generation)
	self.handed = append(self.handed, nonce)
	return key, nonce, generation, nil
}

func (self *framingKeySource) MessageKey(contentType ContentType, leaf LeafIndex, generation uint32) ([]byte, []byte, error) {
	if self.refuseLookup != nil {
		return nil, nil, self.refuseLookup
	}
	key, nonce := self.derive(contentType, leaf, generation)
	self.handed = append(self.handed, nonce)
	return key, nonce, nil
}

// CommitMessageKey is the double's second half of the two phase read, and it is recorded
// SEPARATELY from the erase rather than folded into it.
//
// The two are different statements and this file's cases turn on the difference. An erase says one
// generation's key stopped existing; a commit says the receiving head MOVED, which is the write a
// forged header used to reach without a signature and which every refusal on the open path is now
// required to leave alone. A double that appended both to one list would report "the refused open
// moved nothing" for a build that moved the head and erased nothing, which is exactly the shape the
// repair this surface exists for had to close.
func (self *framingKeySource) CommitMessageKey(contentType ContentType, leaf LeafIndex, generation uint32) error {
	if self.refuseCommit != nil {
		return self.refuseCommit
	}
	self.committed = append(self.committed, fmt.Sprintf("%d/%d/%d", contentType, leaf, generation))
	return nil
}

func (self *framingKeySource) EraseMessageKey(contentType ContentType, leaf LeafIndex, generation uint32) {
	self.erased = append(self.erased, fmt.Sprintf("%d/%d/%d", contentType, leaf, generation))
}

// the test double answers the same surface the secret tree does, asserted here so a change to the
// interface moves both rather than leaving this file compiling against a shape nothing implements
var _ MessageKeySource = (*framingKeySource)(nil)

// framingRecordedAead is one AEAD call this file watched go past.
type framingRecordedAead struct {
	key   []byte
	nonce []byte
	aad   []byte
}

// framingRecordingCrypto is a provider that records the key and nonce of every AEAD call made
// through it and otherwise behaves exactly like the one it wraps.
//
// The nonce a seal actually used is not in its answer and is not recoverable from the ciphertext,
// so without this the reuse guard's whole contract -- that the nonce on the wire is the ratchet
// nonce XOR the four guard octets, and that two generations never share one -- is unobservable.
// The alternative is to re-implement section 6.3.1 in the test and compare ciphertexts, which
// tests the re-implementation.
type framingRecordingCrypto struct {
	CryptoProvider
	seals []framingRecordedAead
	opens []framingRecordedAead
}

func framingRecordingProvider(inner CryptoProvider) *framingRecordingCrypto {
	return &framingRecordingCrypto{CryptoProvider: inner}
}

func (self *framingRecordingCrypto) AeadSeal(key []byte, nonce []byte, aad []byte, plaintext []byte) ([]byte, error) {
	self.seals = append(self.seals, framingRecordedAead{
		key:   append([]byte(nil), key...),
		nonce: append([]byte(nil), nonce...),
		aad:   append([]byte(nil), aad...),
	})
	return self.CryptoProvider.AeadSeal(key, nonce, aad, plaintext)
}

func (self *framingRecordingCrypto) AeadOpen(key []byte, nonce []byte, aad []byte, ciphertext []byte) ([]byte, error) {
	self.opens = append(self.opens, framingRecordedAead{
		key:   append([]byte(nil), key...),
		nonce: append([]byte(nil), nonce...),
		aad:   append([]byte(nil), aad...),
	})
	return self.CryptoProvider.AeadOpen(key, nonce, aad, ciphertext)
}

// framingPrivateSigned is a message signed under WireFormatPrivateMessage together with what it
// takes to seal, open and check it.
type framingPrivateSigned struct {
	crypto           CryptoProvider
	priv             SignaturePrivateKey
	pub              SignaturePublicKey
	groupContext     []byte
	senderDataSecret []byte
	authContent      *AuthenticatedContent
}

func framingPrivateSignedContent(t *testing.T, crypto CryptoProvider, content *FramedContent) framingPrivateSigned {
	t.Helper()
	priv, pub, err := crypto.SignatureKeyPair()
	if err != nil {
		t.Fatalf("key pair: %v", err)
	}
	groupContext := framingTestGroupContext(t)
	authContent, err := SignAuthenticatedContent(crypto, priv, WireFormatPrivateMessage, content, groupContext)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	// a commit's auth data carries a confirmation tag as well, and both the encoder and the
	// verifier refuse one without it. The committer fills it in once it has advanced the
	// transcript; here it is a value of the provider's own tag width and nothing more.
	if content.ContentType == ContentTypeCommit {
		authContent.Auth.ConfirmationTag = bytes.Repeat([]byte{0x77}, crypto.HashSize())
	}
	return framingPrivateSigned{
		crypto: crypto, priv: priv, pub: pub, groupContext: groupContext,
		senderDataSecret: bytes.Repeat([]byte{0x33}, crypto.HashSize()),
		authContent:      authContent,
	}
}

func framingPrivateSignedMember(t *testing.T) framingPrivateSigned {
	t.Helper()
	return framingPrivateSignedContent(t, newTestCrypto(t), framingTestMemberContent())
}

// framingSenderDataOf opens the cleartext header's sender data, which is where the generation and
// the reuse guard a seal chose are readable from.
func framingSenderDataOf(t *testing.T, crypto CryptoProvider, senderDataSecret []byte, message *PrivateMessage) *SenderData {
	t.Helper()
	senderData, err := openSenderData(crypto, senderDataSecret, message.EncryptedSenderData,
		message, message.Ciphertext)
	if err != nil {
		t.Fatalf("open the sender data: %v", err)
	}
	return senderData
}

// TestApplyReuseGuardXorsEveryBitOfTheGuardAndNothingBeyondIt sweeps every bit of the guard and
// every octet of the nonce.
//
// One guard value states that the function does something. The class here is the 32 bits of the
// guard crossed with the whole width of the nonce, which is what separates a guard that XORs the
// first four octets from one that XORs the first, from one that XORs all twelve with the guard
// repeated, from one that XORs the LAST four -- all four of which round trip perfectly against
// themselves and interoperate with nobody.
func TestApplyReuseGuardXorsEveryBitOfTheGuardAndNothingBeyondIt(t *testing.T) {
	crypto := newTestCrypto(t)
	base := crypto.ExpandWithLabel(bytes.Repeat([]byte{0x41}, crypto.HashSize()),
		"reuse guard sweep", nil, crypto.NonceSize())
	if len(base) <= senderDataReuseGuardSize {
		t.Fatalf("the nonce is %d octets and the guard is %d, so there is nothing beyond the guard to hold",
			len(base), senderDataReuseGuardSize)
	}
	for at := range senderDataReuseGuardSize {
		for bit := range 8 {
			var guard [senderDataReuseGuardSize]byte
			guard[at] = 1 << bit
			original := append([]byte(nil), base...)
			guarded := applyReuseGuard(base, guard)

			if !bytes.Equal(base, original) {
				t.Fatalf("guard octet %d bit %d: applyReuseGuard mutated the ratchet nonce in place", at, bit)
			}
			if len(guarded) != len(base) {
				t.Fatalf("guard octet %d bit %d: the guarded nonce is %d octets and the ratchet nonce is %d",
					at, bit, len(guarded), len(base))
			}
			want := append([]byte(nil), base...)
			want[at] ^= 1 << bit
			if !bytes.Equal(guarded, want) {
				t.Fatalf("guard octet %d bit %d: guarded %x, want %x", at, bit, guarded, want)
			}
		}
	}

	// the zero guard is the identity, which is what says the XOR is over the guard and not over
	// some constant of its own
	if guarded := applyReuseGuard(base, [senderDataReuseGuardSize]byte{}); !bytes.Equal(guarded, base) {
		t.Errorf("the zero guard moved the nonce to %x from %x", guarded, base)
	}
	// and the all ones guard flips exactly the guard's own width
	all := [senderDataReuseGuardSize]byte{}
	for i := range all {
		all[i] = 0xff
	}
	guarded := applyReuseGuard(base, all)
	for at, b := range guarded {
		want := base[at]
		if at < senderDataReuseGuardSize {
			want ^= 0xff
		}
		if b != want {
			t.Fatalf("under an all ones guard, nonce octet %d of %d is %#02x and want %#02x",
				at, len(guarded), b, want)
		}
	}
}

// TestApplyReuseGuardAnswersStorageTheCallerDoesNotAlreadyHold is the aliasing half.
//
// A guard that returned its argument, or that wrote through it, leaves this sender holding a nonce
// no other member of the group computes for that generation -- the guard is per message and the
// ratchet nonce is not -- so every later message it tried to open at that generation would fail
// with a bad tag and be diagnosed as tampering.
func TestApplyReuseGuardAnswersStorageTheCallerDoesNotAlreadyHold(t *testing.T) {
	base := bytes.Repeat([]byte{0x5c}, 12)
	guarded := applyReuseGuard(base, [senderDataReuseGuardSize]byte{0x11, 0x22, 0x33, 0x44})
	guarded[len(guarded)-1] ^= 0xff
	if base[len(base)-1] != 0x5c {
		t.Fatal("writing into the guarded nonce reached the ratchet nonce, so the two share storage")
	}
}

// TestPrivateMessageSealOpenRoundTripsEveryContentType runs the whole path a peer runs: sign,
// seal, serialize, parse somebody else's octets, open.
//
// The serialization in the middle is what makes this more than a seal-then-open: every field the
// two AEADs are bound to has to survive the codec, so a codec that dropped one shows up here as a
// decryption failure rather than as a difference nothing compares.
//
// What it CANNOT see is stated so nobody reads it as the guard: it is a symmetry property, so a
// seal and an open that are wrong the same way pass it. The sweeps below are what hold those.
func TestPrivateMessageSealOpenRoundTripsEveryContentType(t *testing.T) {
	crypto := newTestCrypto(t)
	for contentType, content := range framingPrivateContentsOfEveryType(t) {
		signed := framingPrivateSignedContent(t, crypto, content)
		sealKeys := framingNewKeySource(crypto, 0x01, 0)
		message, err := SealPrivateMessage(crypto, sealKeys, signed.senderDataSecret,
			signed.authContent, PaddingSizeV1)
		if err != nil {
			t.Fatalf("content type %d: seal: %v", contentType, err)
		}
		// the source starts at generation 0, so the seal spends exactly that one and the
		// open spends exactly the one the sender data named it. Both are held by VALUE and
		// not by count, for the reason the boundary sweep writes down.
		spent := fmt.Sprintf("%d/%d/%d", contentType, content.Sender.LeafIndex, 0)
		if !slices.Equal(sealKeys.erased, []string{spent}) {
			t.Fatalf("content type %d: the seal erased %v, want exactly [%s]",
				contentType, sealKeys.erased, spent)
		}
		if content.ApplicationData != nil && bytes.Contains(message.Ciphertext, content.ApplicationData) {
			t.Fatalf("content type %d: the plaintext is visible in the ciphertext", contentType)
		}
		if len(message.EncryptedSenderData) == 0 {
			t.Fatalf("content type %d: the message carries no encrypted sender data", contentType)
		}

		encoded, err := syntax.Marshal(message)
		if err != nil {
			t.Fatalf("content type %d: marshal: %v", contentType, err)
		}
		decoded := PrivateMessage{}
		if err := syntax.Unmarshal(encoded, &decoded); err != nil {
			t.Fatalf("content type %d: unmarshal: %v", contentType, err)
		}

		openKeys := framingNewKeySource(crypto, 0x01, 0)
		opened, err := OpenPrivateMessage(crypto, openKeys, signed.senderDataSecret, &decoded,
			StaticSignatureKey(signed.pub), signed.groupContext)
		if err != nil {
			t.Fatalf("content type %d: open: %v", contentType, err)
		}
		// the OPEN commits rather than erases, and the seal above erases rather than commits.
		// Holding both halves by value is what says the two paths did not swap rules.
		if !slices.Equal(openKeys.committed, []string{spent}) {
			t.Fatalf("content type %d: the open committed %v, want exactly [%s]",
				contentType, openKeys.committed, spent)
		}
		if len(openKeys.erased) != 0 {
			t.Fatalf("content type %d: the open erased %v; the open path spends its generation through the COMMIT, which states where the head stands as well as that the key is gone",
				contentType, openKeys.erased)
		}
		if opened.WireFormat != WireFormatPrivateMessage {
			t.Errorf("content type %d: opened under wire format %d", contentType, opened.WireFormat)
		}
		if opened.Content.Sender != content.Sender {
			t.Fatalf("content type %d: sender %+v, want %+v", contentType, opened.Content.Sender, content.Sender)
		}
		if !bytes.Equal(opened.Content.ApplicationData, content.ApplicationData) {
			t.Fatalf("content type %d: application data %q, want %q",
				contentType, opened.Content.ApplicationData, content.ApplicationData)
		}
		if opened.Content.ContentType != contentType {
			t.Fatalf("content type %d came back as %d", contentType, opened.Content.ContentType)
		}
		if !bytes.Equal(opened.Auth.Signature, signed.authContent.Auth.Signature) ||
			!bytes.Equal(opened.Auth.ConfirmationTag, signed.authContent.Auth.ConfirmationTag) {
			t.Fatalf("content type %d: auth data %+v, want %+v", contentType, opened.Auth, signed.authContent.Auth)
		}
	}
}

// framingBoundaryGenerations is the generations every sweep in this file drives a seal at.
//
// Derived off the WIDTH of the counter rather than sampled around a value somebody picked. p4's
// secret tree shipped a nonce reuse at 2^32 that passed sixty nine tests, every one of which
// sampled the middle of the range; the two values at the top of this ladder are the ones that
// would have caught it, and the octet boundaries below them are where a counter carried through a
// narrower type stops agreeing with itself.
func framingBoundaryGenerations() []uint32 {
	last := ^uint32(0)
	found := []uint32{0, 1, 2}
	for bits := 8; bits < 32; bits += 8 {
		found = append(found, uint32(1)<<bits-1, uint32(1)<<bits)
	}
	found = append(found, last-1, last)
	slices.Sort(found)
	return slices.Compact(found)
}

// framingOrderingsOf derives every ordering of n things, by insertion rather than from a table.
//
// It exists so the AAD test below can state which ordering of section 6.3.1's fields the code
// actually used, out of all of them, rather than comparing against the one wrong ordering the
// author thought of.
func framingOrderingsOf(n int) [][]int {
	if n <= 0 {
		return [][]int{{}}
	}
	orderings := [][]int{}
	for _, shorter := range framingOrderingsOf(n - 1) {
		for at := 0; at <= len(shorter); at += 1 {
			ordering := append([]int{}, shorter[:at]...)
			ordering = append(ordering, n-1)
			ordering = append(ordering, shorter[at:]...)
			orderings = append(orderings, ordering)
		}
	}
	return orderings
}

// TestThePrivateMessageContentSealIsTheSectionSixThreeOneConstructionAndNotOnlyItsOwnInverse is
// the content half of what the sender data seal already has.
//
// Nothing round trips through an AAD. It is never compared against a peer's, so the seal and the
// open agree with each other by construction whatever they compute, and a whole family of wrong
// constructions is invisible from every symmetry property in this package: a length prefix in
// front of the header, a field written twice, the two opaque fields exchanged. Measured: with
// group_id and authenticated_data swapped at BOTH call sites -- the seal's and the open's -- the
// whole of ./mls/... and ./message/... stayed green. The asymmetric version of that edit is
// caught by the round trip; the symmetric one is exactly what a round trip is blind to, and it is
// permanent interop divergence rather than a bug that shows up locally.
//
// What is compared is not "the aad the code computes" against one alternative, but WHICH ORDERING
// of section 6.3.1's four fields the recorded aad is, out of all twenty four of them. That is the
// derived class: any argument order mistake among the four lands on one of the twenty three wrong
// orderings, and the failure names the ordering it found instead of printing two hex strings at a
// reader.
//
// The header is given a group_id and an authenticated_data of different values AND different
// lengths on purpose. At a header whose two opaque fields are equal the swap is invisible, so a
// fixture that happened to make them equal would be a test that passed under the mutation it is
// named for -- which is why the twenty four encodings are asserted distinct before anything is
// compared to them.
func TestThePrivateMessageContentSealIsTheSectionSixThreeOneConstructionAndNotOnlyItsOwnInverse(t *testing.T) {
	inner := newTestCrypto(t)
	for contentType, content := range framingPrivateContentsOfEveryType(t) {
		content.GroupId = []byte("section 6.3.1 group id")
		content.AuthenticatedData = []byte("aad")
		signed := framingPrivateSignedContent(t, inner, content)

		// the four fields of PrivateContentAAD, in the order RFC 9420 section 6.3.1 writes
		// them, assembled here rather than borrowed from the code under test
		fields := []struct {
			name  string
			write func(w *syntax.Writer)
		}{
			{name: "group_id", write: func(w *syntax.Writer) { w.WriteOpaque(content.GroupId) }},
			{name: "epoch", write: func(w *syntax.Writer) { w.WriteUint64(content.Epoch) }},
			{name: "content_type", write: func(w *syntax.Writer) { w.WriteUint8(uint8(content.ContentType)) }},
			{name: "authenticated_data", write: func(w *syntax.Writer) { w.WriteOpaque(content.AuthenticatedData) }},
		}
		orderings := framingOrderingsOf(len(fields))
		orderingOf := map[string]string{}
		sectionOrder := ""
		for _, ordering := range orderings {
			w := syntax.NewWriter()
			names := []string{}
			for _, at := range ordering {
				fields[at].write(w)
				names = append(names, fields[at].name)
			}
			encoded, err := w.Bytes()
			if err != nil {
				t.Fatalf("content type %d: the %s ordering: %v", contentType, strings.Join(names, " "), err)
			}
			if previous, clash := orderingOf[string(encoded)]; clash {
				t.Fatalf("content type %d: the orderings %q and %q encode identically, so this fixture cannot separate them",
					contentType, previous, strings.Join(names, " "))
			}
			orderingOf[string(encoded)] = strings.Join(names, " ")
			if slices.IsSorted(ordering) {
				sectionOrder = strings.Join(names, " ")
			}
		}
		if len(orderingOf) != len(orderings) || sectionOrder == "" {
			t.Fatalf("content type %d: %d distinct encodings over %d orderings, section 6.3.1's is %q",
				contentType, len(orderingOf), len(orderings), sectionOrder)
		}

		recording := framingRecordingProvider(inner)
		message, err := SealPrivateMessage(recording, framingPinnedKeySource(inner, 0x01, 9),
			signed.senderDataSecret, signed.authContent, PaddingSizeV1)
		if err != nil {
			t.Fatalf("content type %d: seal: %v", contentType, err)
		}
		// the content is sealed first and the sender data second, which is the order section
		// 6.3.2 forces: the sender data key is derived from the content ciphertext
		if len(recording.seals) != 2 {
			t.Fatalf("content type %d: the seal made %d aead calls, want the content's and the sender data's",
				contentType, len(recording.seals))
		}
		sealedUnder, known := orderingOf[string(recording.seals[0].aad)]
		if !known {
			t.Fatalf("content type %d: the content was sealed under %x, which is no ordering of section 6.3.1's four fields at all",
				contentType, recording.seals[0].aad)
		}
		if sealedUnder != sectionOrder {
			t.Fatalf("content type %d: the content was sealed under the fields in the order %q; section 6.3.1 is %q",
				contentType, sealedUnder, sectionOrder)
		}

		// and the sender data is sealed under section 6.3.2's three fields rather than this
		// one's four, which is the confusion two AADs side by side invites
		senderAAD, err := senderDataAAD(content.GroupId, content.Epoch, content.ContentType)
		if err != nil {
			t.Fatalf("content type %d: the section 6.3.2 aad: %v", contentType, err)
		}
		if !bytes.Equal(recording.seals[1].aad, senderAAD) {
			t.Fatalf("content type %d: the sender data was sealed under %x and section 6.3.2's aad is %x",
				contentType, recording.seals[1].aad, senderAAD)
		}
		if bytes.Equal(recording.seals[0].aad, recording.seals[1].aad) {
			t.Fatalf("content type %d: both seals used one aad, so this row cannot separate the two constructions",
				contentType)
		}

		// the OPEN computes the same construction. An order mistake present on one side alone
		// is caught by the round trip; this is what says the two sides agree on section 6.3.1
		// rather than merely on each other.
		opening := framingRecordingProvider(inner)
		if _, err := OpenPrivateMessage(opening, framingPinnedKeySource(inner, 0x01, 9),
			signed.senderDataSecret, message, StaticSignatureKey(signed.pub), signed.groupContext); err != nil {
			t.Fatalf("content type %d: open: %v", contentType, err)
		}
		if len(opening.opens) != 2 {
			t.Fatalf("content type %d: the open made %d aead calls, want the sender data's and the content's",
				contentType, len(opening.opens))
		}
		if !bytes.Equal(opening.opens[0].aad, senderAAD) {
			t.Fatalf("content type %d: the sender data was opened under %x and section 6.3.2's aad is %x",
				contentType, opening.opens[0].aad, senderAAD)
		}
		openedUnder, known := orderingOf[string(opening.opens[1].aad)]
		if !known {
			t.Fatalf("content type %d: the content was opened under %x, which is no ordering of section 6.3.1's four fields at all",
				contentType, opening.opens[1].aad)
		}
		if openedUnder != sectionOrder {
			t.Fatalf("content type %d: the content was opened under the fields in the order %q; section 6.3.1 is %q",
				contentType, openedUnder, sectionOrder)
		}
	}
}

// TestSealPrivateMessageSealsUnderTheGuardedNonceAtEveryBoundaryGeneration is the reuse guard's
// contract read off the AEAD call itself.
//
// Three things are held, and none of them is visible from the message. The key is the one the
// source handed over, unaltered. The nonce is the source's nonce XOR the four guard octets the
// sender data carries, exactly -- so a seal that used the unguarded nonce, or that guarded a
// different four octets, or that drew a second guard for the AEAD than the one it published, fails
// here while round tripping perfectly against itself. And the source's own nonce storage is
// UNCHANGED afterwards, which is what says the guard copied.
//
// The sweep is over framingBoundaryGenerations because a generation that only appears in the
// middle of the range is a generation a counter defect cannot be seen at.
func TestSealPrivateMessageSealsUnderTheGuardedNonceAtEveryBoundaryGeneration(t *testing.T) {
	inner := newTestCrypto(t)
	signed := framingPrivateSignedMember(t)
	content := &signed.authContent.Content
	for _, generation := range framingBoundaryGenerations() {
		recording := framingRecordingProvider(inner)
		keys := framingPinnedKeySource(inner, 0x01, generation)
		message, err := SealPrivateMessage(recording, keys, signed.senderDataSecret,
			signed.authContent, PaddingSizeV1)
		if err != nil {
			t.Fatalf("generation %d: seal: %v", generation, err)
		}
		if len(recording.seals) != 2 {
			t.Fatalf("generation %d: the seal made %d AEAD calls, want the content then the sender data",
				generation, len(recording.seals))
		}
		senderData := framingSenderDataOf(t, inner, signed.senderDataSecret, message)
		if senderData.Generation != generation {
			t.Fatalf("the sender data carries generation %d and the source handed out %d",
				senderData.Generation, generation)
		}
		if senderData.LeafIndex != content.Sender.LeafIndex {
			t.Fatalf("generation %d: the sender data names leaf %d and the content names %d",
				generation, senderData.LeafIndex, content.Sender.LeafIndex)
		}
		// the erase names the generation the message was actually sealed under, and not a
		// neighbour of it. Counting erasures cannot see this: a seal that erased
		// generation+1 erases exactly once, leaves the generation it DID use alive in the
		// sender's window, and answers a message that opens perfectly -- so what it costs
		// is the forward secrecy the erase is the whole of. Measured: with the argument
		// moved by one, the whole of ./mls/... and ./message/... stayed green.
		erased := fmt.Sprintf("%d/%d/%d", content.ContentType, content.Sender.LeafIndex, generation)
		if !slices.Equal(keys.erased, []string{erased}) {
			t.Fatalf("generation %d: the seal erased %v, want exactly [%s]",
				generation, keys.erased, erased)
		}

		wantKey, wantNonce := keys.derive(content.ContentType, content.Sender.LeafIndex, generation)
		if !bytes.Equal(recording.seals[0].key, wantKey) {
			t.Fatalf("generation %d: the content was sealed under key %x and the source handed out %x",
				generation, recording.seals[0].key, wantKey)
		}
		guarded := applyReuseGuard(wantNonce, senderData.ReuseGuard)
		if !bytes.Equal(recording.seals[0].nonce, guarded) {
			t.Fatalf("generation %d: the content was sealed under nonce %x and the ratchet nonce guarded by the published reuse guard is %x",
				generation, recording.seals[0].nonce, guarded)
		}
		if bytes.Equal(recording.seals[0].nonce, wantNonce) && senderData.ReuseGuard != [senderDataReuseGuardSize]byte{} {
			t.Fatalf("generation %d: the content was sealed under the UNGUARDED ratchet nonce while publishing a non-zero reuse guard",
				generation)
		}
		if len(keys.handed) != 1 {
			t.Fatalf("generation %d: the source handed out %d nonces, want one", generation, len(keys.handed))
		}
		if !bytes.Equal(keys.handed[0], wantNonce) {
			t.Fatalf("generation %d: the source's own nonce is %x after the seal and was %x before it, so the guard wrote through",
				generation, keys.handed[0], wantNonce)
		}
	}
}

// TestSealPrivateMessageNeverRepeatsAContentNonceAcrossGenerations is the nonce reuse property
// itself, over the boundary ladder, and it is in two halves because one instrument cannot hold
// both of them.
//
// A repeated (key, nonce) pair over two different plaintexts is a total loss of confidentiality for
// both under a stream cipher AEAD, and it is the failure p4 shipped one rung down. What is compared
// is the PAIR: a source that varied the key and reused the nonce, and one that varied the nonce and
// reused the key, are both caught, and so is a seal that ignored one of the two.
//
// The first half runs over framingPinnedKeySource, and what it holds is the PASS THROUGH. That
// source feeds all four octets of the generation into ExpandWithLabel, so its nonces are distinct
// across generations by construction, and the distinctness the loop observes is the instrument's
// rather than the subject's. It is worth running -- a seal that derived a nonce of its own, or that
// reused the last message's, fails it -- but on its own the name would overclaim.
//
// The second half is where the name is earned. It reads the nonce off a REAL secret tree at each
// boundary generation, holds those pairwise distinct, and then holds the seal's own nonce to be
// exactly that one guarded by the reuse guard the message published. Distinct ratchet nonces plus a
// seal that uses them is what "no nonce repeats" means on the wire. The ratchet's half of it is
// DeriveTreeSecret(secret, "nonce", generation, ...) one layer down, held there and by its vectors.
func TestSealPrivateMessageNeverRepeatsAContentNonceAcrossGenerations(t *testing.T) {
	inner := newTestCrypto(t)
	signed := framingPrivateSignedMember(t)
	seenNonce := map[string]uint32{}
	seenPair := map[string]uint32{}
	for _, generation := range framingBoundaryGenerations() {
		recording := framingRecordingProvider(inner)
		keys := framingPinnedKeySource(inner, 0x01, generation)
		if _, err := SealPrivateMessage(recording, keys, signed.senderDataSecret,
			signed.authContent, PaddingSizeV1); err != nil {
			t.Fatalf("generation %d: seal: %v", generation, err)
		}
		nonce := string(recording.seals[0].nonce)
		pair := string(recording.seals[0].key) + "|" + nonce
		if previous, repeated := seenNonce[nonce]; repeated {
			t.Fatalf("generation %d seals under the same nonce as generation %d", generation, previous)
		}
		if previous, repeated := seenPair[pair]; repeated {
			t.Fatalf("generation %d seals under the same key and nonce as generation %d", generation, previous)
		}
		seenNonce[nonce] = generation
		seenPair[pair] = generation
	}
	if len(seenPair) != len(framingBoundaryGenerations()) {
		t.Fatalf("the sweep recorded %d pairs over %d generations", len(seenPair), len(framingBoundaryGenerations()))
	}

	// and the same property against the real ratchet, which is the half the loop above cannot
	// state. The maps are separate because the two halves draw from different key sources, so a
	// collision between them would mean nothing.
	leaf := signed.authContent.Content.Sender.LeafIndex
	contentType := signed.authContent.Content.ContentType
	seenRatchet := map[string]uint32{}
	seenBare := map[string]uint32{}
	seenSealed := map[string]uint32{}
	for _, generation := range framingBoundaryGenerations() {
		recording := framingRecordingProvider(inner)
		sender := framingSecretTreeAt(t, inner, leaf, contentType, generation)
		lookup := framingSecretTreeAt(t, inner, leaf, contentType, generation)
		message, err := SealPrivateMessage(recording, sender, signed.senderDataSecret,
			signed.authContent, PaddingSizeV1)
		if err != nil {
			t.Fatalf("generation %d: seal through the real tree: %v", generation, err)
		}
		key, nonce, err := lookup.MessageKey(contentType, leaf, generation)
		if err != nil {
			t.Fatalf("generation %d: the ratchet's own key: %v", generation, err)
		}
		pair := string(key) + "|" + string(nonce)
		if previous, repeated := seenRatchet[pair]; repeated {
			t.Fatalf("the ratchet answers the same key and nonce at generation %d as at generation %d",
				generation, previous)
		}
		seenRatchet[pair] = generation
		// and the NONCE on its own, not only the pair. A ratchet that varied the key and held
		// the nonce still where it was is safe against this AEAD -- a nonce is only reused
		// under one key -- and is a different nonce from the one every peer derives for that
		// generation, which is silent interop divergence rather than a broken open. Measured:
		// with the generation dropped from DeriveTreeSecret(secret, "nonce", ...) the pair
		// above still separates every generation.
		if previous, repeated := seenBare[string(nonce)]; repeated {
			t.Fatalf("the ratchet answers the same nonce at generation %d as at generation %d",
				generation, previous)
		}
		seenBare[string(nonce)] = generation
		senderData := framingSenderDataOf(t, inner, signed.senderDataSecret, message)
		guarded := applyReuseGuard(nonce, senderData.ReuseGuard)
		if !bytes.Equal(recording.seals[0].nonce, guarded) {
			t.Fatalf("generation %d: the content was sealed under %x and the ratchet nonce guarded by the published guard is %x",
				generation, recording.seals[0].nonce, guarded)
		}
		if previous, repeated := seenSealed[string(recording.seals[0].nonce)]; repeated {
			t.Fatalf("generation %d seals under the same nonce as generation %d through the real tree",
				generation, previous)
		}
		seenSealed[string(recording.seals[0].nonce)] = generation
	}
	if len(seenRatchet) != len(framingBoundaryGenerations()) || len(seenBare) != len(seenRatchet) {
		t.Fatalf("the real tree answered %d distinct key and nonce pairs and %d distinct nonces over %d generations",
			len(seenRatchet), len(seenBare), len(framingBoundaryGenerations()))
	}
}

// TestSealPrivateMessageDrawsAFreshReuseGuardForEveryMessage is the other half of what the guard is
// for: one generation handed out twice must not produce one nonce twice.
//
// That is exactly the rolled back sender the guard exists for -- a restored backup, a forked
// device, a resumed process -- and it is the case a source that advances cannot present. The
// generation is held FIXED here on purpose, so the only thing that can differ between the two
// messages is the guard.
func TestSealPrivateMessageDrawsAFreshReuseGuardForEveryMessage(t *testing.T) {
	inner := newTestCrypto(t)
	signed := framingPrivateSignedMember(t)
	seen := map[string]int{}
	guards := map[[senderDataReuseGuardSize]byte]int{}
	const messages = 32
	for i := range messages {
		recording := framingRecordingProvider(inner)
		keys := framingPinnedKeySource(inner, 0x01, 7)
		message, err := SealPrivateMessage(recording, keys, signed.senderDataSecret,
			signed.authContent, PaddingSizeV1)
		if err != nil {
			t.Fatalf("message %d: seal: %v", i, err)
		}
		senderData := framingSenderDataOf(t, inner, signed.senderDataSecret, message)
		if previous, repeated := guards[senderData.ReuseGuard]; repeated {
			t.Fatalf("message %d published the same reuse guard as message %d, so the guard is not drawn per message",
				i, previous)
		}
		guards[senderData.ReuseGuard] = i
		nonce := string(recording.seals[0].nonce)
		if previous, repeated := seen[nonce]; repeated {
			t.Fatalf("message %d reuses the nonce of message %d at one generation, which is the reuse the guard exists to prevent",
				i, previous)
		}
		seen[nonce] = i
	}
	if len(seen) != messages {
		t.Fatalf("the sweep produced %d distinct nonces over %d messages at one generation", len(seen), messages)
	}
}

// TestSealPrivateMessageAnswersItsKeySourceRefusalVerbatim is the guard against a seal that
// re-derives when the ratchet says no.
//
// Every one of these sentinels means "there is no key here": the ratchet is exhausted, the epoch is
// gone, the content type has no ratchet behind it. A seal that answered one of them by falling back
// to some other generation would be encrypting under a key the receiver has already consumed --
// which is a message an attacker who kept the earlier ciphertext can replay -- and it would do it
// with a nil error, so nothing above would know.
func TestSealPrivateMessageAnswersItsKeySourceRefusalVerbatim(t *testing.T) {
	crypto := newTestCrypto(t)
	signed := framingPrivateSignedMember(t)
	for _, refusal := range []error{ErrRatchetExhausted, ErrEpochErased, ErrUnknownContentType,
		ErrRatchetGenerationTooFarAhead, ErrSecretTreeLeafOutOfRange} {

		keys := framingNewKeySource(crypto, 0x01, 0)
		keys.refuseNext = fmt.Errorf("the ratchet says no: %w", refusal)
		message, err := SealPrivateMessage(crypto, keys, signed.senderDataSecret,
			signed.authContent, PaddingSizeV1)
		if !errors.Is(err, refusal) {
			t.Fatalf("a key source refusing with %v: the seal answered %v", refusal, err)
		}
		if message != nil {
			t.Errorf("a key source refusing with %v: the seal answered a message alongside its refusal", refusal)
		}
		if len(keys.erased) != 0 {
			t.Errorf("a key source refusing with %v: the seal erased %v", refusal, keys.erased)
		}
	}
}

// TestOpenPrivateMessageAnswersItsKeySourceRefusalVerbatim is the same rule on the receive side,
// and it is the sharper of the two.
//
// A refusal here is a fact about the ratchet -- consumed, too far ahead, erased epoch -- and it is
// NOT ValSem006, which says a key was found and the message did not open under it. An open that
// collapsed the two would tell an operator to go looking for a key mismatch when what happened is a
// replay; an open that answered a ratchet refusal by re-deriving the generation would decrypt that
// replay.
func TestOpenPrivateMessageAnswersItsKeySourceRefusalVerbatim(t *testing.T) {
	crypto := newTestCrypto(t)
	signed := framingPrivateSignedMember(t)
	message, err := SealPrivateMessage(crypto, framingNewKeySource(crypto, 0x01, 0),
		signed.senderDataSecret, signed.authContent, PaddingSizeV1)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	for _, refusal := range []error{ErrRatchetGenerationConsumed, ErrRatchetGenerationTooFarAhead,
		ErrRatchetExhausted, ErrEpochErased, ErrUnknownContentType} {

		keys := framingNewKeySource(crypto, 0x01, 0)
		keys.refuseLookup = fmt.Errorf("the ratchet says no: %w", refusal)
		opened, err := OpenPrivateMessage(crypto, keys, signed.senderDataSecret, message,
			StaticSignatureKey(signed.pub), signed.groupContext)
		if !errors.Is(err, refusal) {
			t.Fatalf("a key source refusing with %v: the open answered %v", refusal, err)
		}
		if errors.Is(err, errDecryptFailed) {
			t.Errorf("a key source refusing with %v: the open folded a ratchet refusal into ValSem006", refusal)
		}
		if opened != nil {
			t.Errorf("a key source refusing with %v: the open answered a message alongside its refusal", refusal)
		}
		if len(keys.erased) != 0 {
			t.Errorf("a key source refusing with %v: the open erased %v", refusal, keys.erased)
		}
		if len(keys.committed) != 0 {
			t.Errorf("a key source refusing with %v: the open COMMITTED %v, so the receiving head moved on a message that never opened",
				refusal, keys.committed)
		}
	}
}

// TestOpenPrivateMessageAnswersACommitRefusalRatherThanAcceptingTheMessageTwice is the far end of
// the two phase read, and it is the one arm of it that a sequential case cannot reach by itself.
//
// WHY A COMMIT CAN REFUSE AT ALL after a lookup that succeeded. The two admit exactly the same
// generations, so nothing about THIS message can have changed between them -- but this type is
// built for concurrent callers, and a second goroutine opening the same message commits that
// generation first. The commit that arrives second is then answered ErrRatchetGenerationConsumed,
// which is the honest sentence: this delivery is a replay of one that has already been accepted.
//
// WHAT SWALLOWING IT WOULD COST. The open would return an AuthenticatedContent for a message whose
// generation is spent, so one message would be accepted twice by two callers that both believe
// they consumed it. Measured: with the refusal replaced by a discard, the whole of ./mls/,
// ./message/ and ./messagegroup/ stayed green -- which is why this case is written rather than
// left to the argument above.
//
// The double is driven directly rather than through a race, because a case whose failure depends
// on a scheduler reports a probability and this one reports a rule.
func TestOpenPrivateMessageAnswersACommitRefusalRatherThanAcceptingTheMessageTwice(t *testing.T) {
	crypto := newTestCrypto(t)
	signed := framingPrivateSignedMember(t)
	message, err := SealPrivateMessage(crypto, framingNewKeySource(crypto, 0x01, 0),
		signed.senderDataSecret, signed.authContent, PaddingSizeV1)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	// the control: the same message, the same source, and no refusal on the commit. Without it a
	// build that refused every open would pass the half below.
	control := framingNewKeySource(crypto, 0x01, 0)
	if _, err := OpenPrivateMessage(crypto, control, signed.senderDataSecret, message,
		StaticSignatureKey(signed.pub), signed.groupContext); err != nil {
		t.Fatalf("the control open: %v", err)
	}
	if len(control.committed) != 1 {
		t.Fatalf("the control committed %v, want exactly one generation", control.committed)
	}

	for _, refusal := range []error{ErrRatchetGenerationConsumed, ErrEpochErased} {
		keys := framingNewKeySource(crypto, 0x01, 0)
		keys.refuseCommit = fmt.Errorf("another caller got there first: %w", refusal)
		opened, err := OpenPrivateMessage(crypto, keys, signed.senderDataSecret, message,
			StaticSignatureKey(signed.pub), signed.groupContext)
		if !errors.Is(err, refusal) {
			t.Fatalf("a commit refusing with %v: the open answered %v; a swallowed commit refusal accepts one message twice",
				refusal, err)
		}
		if opened != nil {
			t.Errorf("a commit refusing with %v: the open answered a message alongside its refusal", refusal)
		}
		if len(keys.committed) != 0 {
			t.Errorf("a commit refusing with %v: the source recorded %v as committed", refusal, keys.committed)
		}
	}
}

// TestOpenPrivateMessageRefusesEveryKeyButTheOneTheSenderUsed is what says the open READS its key
// source rather than deriving a key of its own.
//
// The source is the same construction with a different seed, so the shapes are all right and only
// the bytes are wrong -- which is the difference between an open that looked a generation up and
// one that recomputed something.
func TestOpenPrivateMessageRefusesEveryKeyButTheOneTheSenderUsed(t *testing.T) {
	crypto := newTestCrypto(t)
	signed := framingPrivateSignedMember(t)
	message, err := SealPrivateMessage(crypto, framingNewKeySource(crypto, 0x01, 0),
		signed.senderDataSecret, signed.authContent, PaddingSizeV1)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	for seed := 0; seed <= int(^uint8(0)); seed += 17 {
		if byte(seed) == 0x01 {
			continue
		}
		opened, err := OpenPrivateMessage(crypto, framingNewKeySource(crypto, byte(seed), 0),
			signed.senderDataSecret, message, StaticSignatureKey(signed.pub), signed.groupContext)
		if !errors.Is(err, errDecryptFailed) {
			t.Fatalf("a key source seeded %#02x: got %v, want errDecryptFailed", seed, err)
		}
		if opened != nil {
			t.Errorf("a key source seeded %#02x: refused and answered a message alongside", seed)
		}
	}
}

// TestSealPrivateMessageRefusesEverySenderTypeButMember derives the class off the registry.
//
// Section 6.3.2 gives a PrivateMessage's sender data a leaf_index and NOTHING else, so there is no
// field on the wire for an external sender's index and no ratchet for a new member's absent leaf. A
// seal that let one through would write the sender's index into the leaf_index field, which names a
// real member's ratchet -- a message sealed under somebody else's keys and attributed to them.
func TestSealPrivateMessageRefusesEverySenderTypeButMember(t *testing.T) {
	crypto := newTestCrypto(t)
	refused := 0
	for name, code := range registryConstantsOfType(t, "SenderType") {
		if SenderType(code) == SenderTypeMember {
			continue
		}
		content := framingTestProposalContent()
		content.Sender = Sender{SenderType: SenderType(code), SenderIndex: 2}
		// assembled rather than signed, so what is being measured is the guard and not
		// whatever the signer thinks of this sender type
		authContent := &AuthenticatedContent{
			WireFormat: WireFormatPrivateMessage,
			Content:    *content,
			Auth:       FramedContentAuthData{Signature: bytes.Repeat([]byte{0x01}, 64)},
		}
		message, err := SealPrivateMessage(crypto, framingNewKeySource(crypto, 0x01, 0),
			bytes.Repeat([]byte{0x33}, crypto.HashSize()), authContent, PaddingSizeV1)
		if !errors.Is(err, ErrSenderNotMember) {
			t.Fatalf("%s: got %v, want ErrSenderNotMember", name, err)
		}
		if message != nil {
			t.Errorf("%s: refused and answered a message alongside", name)
		}
		refused++
	}
	if refused == 0 {
		t.Fatal("every registered sender type is member, so this sweep asserts nothing")
	}
}

// TestSealPrivateMessageRefusesEveryWireFormatButItsOwn derives the class off the registry too.
//
// The wire format is inside the signature preimage, so a content signed for a PublicMessage and
// sealed as a PrivateMessage is a message whose signature verifies against neither -- and the
// caller finds out at the receiver rather than here. It is checked BEFORE the sender type, and this
// sweep pins that order by handing it a member sender: an implementation that checked the sender
// first would answer the same thing here, so the ordering is held by the neighbouring test.
func TestSealPrivateMessageRefusesEveryWireFormatButItsOwn(t *testing.T) {
	crypto := newTestCrypto(t)
	refused := 0
	for name, code := range registryConstantsOfType(t, "WireFormat") {
		if WireFormat(code) == WireFormatPrivateMessage {
			continue
		}
		authContent := &AuthenticatedContent{
			WireFormat: WireFormat(code),
			Content:    *framingTestMemberContent(),
			Auth:       FramedContentAuthData{Signature: bytes.Repeat([]byte{0x01}, 64)},
		}
		message, err := SealPrivateMessage(crypto, framingNewKeySource(crypto, 0x01, 0),
			bytes.Repeat([]byte{0x33}, crypto.HashSize()), authContent, PaddingSizeV1)
		if !errors.Is(err, ErrWireFormatMismatch) {
			t.Fatalf("%s: got %v, want ErrWireFormatMismatch", name, err)
		}
		if message != nil {
			t.Errorf("%s: refused and answered a message alongside", name)
		}
		refused++
	}
	if refused == 0 {
		t.Fatal("every registered wire format is the private message one, so this sweep asserts nothing")
	}
}

// TestSealPrivateMessageRefusesTheWireFormatAheadOfTheSenderType pins the order the two guards run
// in, which neither sweep above can see on its own.
//
// The order matters because a caller that got both wrong has to be told the one it can act on. The
// wire format is a fact about how this message was SIGNED, and no amount of fixing the sender makes
// a content signed under another wire format sealable.
func TestSealPrivateMessageRefusesTheWireFormatAheadOfTheSenderType(t *testing.T) {
	crypto := newTestCrypto(t)
	content := framingTestProposalContent()
	content.Sender = Sender{SenderType: SenderTypeNewMemberProposal}
	authContent := &AuthenticatedContent{
		WireFormat: WireFormatPublicMessage,
		Content:    *content,
		Auth:       FramedContentAuthData{Signature: bytes.Repeat([]byte{0x01}, 64)},
	}
	_, err := SealPrivateMessage(crypto, framingNewKeySource(crypto, 0x01, 0),
		bytes.Repeat([]byte{0x33}, crypto.HashSize()), authContent, PaddingSizeV1)
	if !errors.Is(err, ErrWireFormatMismatch) {
		t.Fatalf("a message wrong in both: got %v, want ErrWireFormatMismatch", err)
	}
}

// TestOpenPrivateMessageRefusesEveryTamperedOctet sweeps the whole of the message a peer can write
// to and holds each refusal to ValSem006.
//
// Derived over the LENGTH of each field rather than at a position somebody chose, which is this
// file's rule and is what separates an open that authenticates its whole ciphertext from one that
// authenticates its first block. The sender data is keyed off the content ciphertext, so a flipped
// ciphertext octet fails at the sender data step and a rewritten authenticated_data -- which is in
// the content AAD and not in the sender data's -- fails at the content step; both are ValSem006,
// and that they are the same value is the point rather than an accident.
func TestOpenPrivateMessageRefusesEveryTamperedOctet(t *testing.T) {
	crypto := newTestCrypto(t)
	signed := framingPrivateSignedMember(t)
	message, err := SealPrivateMessage(crypto, framingNewKeySource(crypto, 0x01, 0),
		signed.senderDataSecret, signed.authContent, PaddingSizeV1)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	open := func(m *PrivateMessage) error {
		_, err := OpenPrivateMessage(crypto, framingNewKeySource(crypto, 0x01, 0),
			signed.senderDataSecret, m, StaticSignatureKey(signed.pub), signed.groupContext)
		return err
	}
	if err := open(message); err != nil {
		t.Fatalf("the untampered message did not open, so every refusal below is unattributable: %v", err)
	}

	for at := range message.Ciphertext {
		tampered := *message
		tampered.Ciphertext = append([]byte(nil), message.Ciphertext...)
		tampered.Ciphertext[at] ^= 0xff
		if err := open(&tampered); !errors.Is(err, errDecryptFailed) {
			t.Fatalf("ciphertext octet %d of %d: got %v, want errDecryptFailed", at, len(message.Ciphertext), err)
		}
	}
	for at := range message.EncryptedSenderData {
		tampered := *message
		tampered.EncryptedSenderData = append([]byte(nil), message.EncryptedSenderData...)
		tampered.EncryptedSenderData[at] ^= 0xff
		if err := open(&tampered); !errors.Is(err, errDecryptFailed) {
			t.Fatalf("encrypted sender data octet %d of %d: got %v, want errDecryptFailed",
				at, len(message.EncryptedSenderData), err)
		}
	}
	for at := range message.AuthenticatedData {
		tampered := *message
		tampered.AuthenticatedData = append([]byte(nil), message.AuthenticatedData...)
		tampered.AuthenticatedData[at] ^= 0xff
		if err := open(&tampered); !errors.Is(err, errDecryptFailed) {
			t.Fatalf("authenticated data octet %d of %d: got %v, want errDecryptFailed",
				at, len(message.AuthenticatedData), err)
		}
	}
	for at := range message.GroupId {
		tampered := *message
		tampered.GroupId = append([]byte(nil), message.GroupId...)
		tampered.GroupId[at] ^= 0xff
		if err := open(&tampered); !errors.Is(err, errDecryptFailed) {
			t.Fatalf("group id octet %d of %d: got %v, want errDecryptFailed", at, len(message.GroupId), err)
		}
	}
	// the epoch and the content type are in both AADs and are not byte runs
	movedEpoch := *message
	movedEpoch.Epoch++
	if err := open(&movedEpoch); !errors.Is(err, errDecryptFailed) {
		t.Fatalf("an epoch one higher: got %v, want errDecryptFailed", err)
	}
	movedType := *message
	movedType.ContentType = ContentTypeProposal
	if err := open(&movedType); !errors.Is(err, errDecryptFailed) {
		t.Fatalf("a rewritten content type: got %v, want errDecryptFailed", err)
	}
	// and the sender data secret itself, at every octet of its width
	for at := range signed.senderDataSecret {
		wrong := append([]byte(nil), signed.senderDataSecret...)
		wrong[at] ^= 0xff
		_, err := OpenPrivateMessage(crypto, framingNewKeySource(crypto, 0x01, 0), wrong, message,
			StaticSignatureKey(signed.pub), signed.groupContext)
		if !errors.Is(err, errDecryptFailed) {
			t.Fatalf("sender data secret octet %d of %d: got %v, want errDecryptFailed",
				at, len(signed.senderDataSecret), err)
		}
	}
}

// TestOpenPrivateMessageVerifiesTheSignatureItDecrypted is ValSem010 on this path.
//
// The open is the only place this check can happen: who signed a PrivateMessage is not knowable
// until the sender data has been opened, so a caller handed a decrypted AuthenticatedContent and
// told to verify it would be verifying a sender this function had already decided to trust. An open
// that skipped it accepts any member's message under any other member's leaf.
//
// The sweep is over every octet of the signature and every octet of the key, derived off their
// lengths, because a verifier narrowed to the first block passes a single flipped bit.
func TestOpenPrivateMessageVerifiesTheSignatureItDecrypted(t *testing.T) {
	crypto := newTestCrypto(t)
	signed := framingPrivateSignedMember(t)
	for at := range signed.authContent.Auth.Signature {
		forged := *signed.authContent
		forged.Auth = FramedContentAuthData{Signature: append([]byte(nil), signed.authContent.Auth.Signature...)}
		forged.Auth.Signature[at] ^= 0xff
		message, err := SealPrivateMessage(crypto, framingNewKeySource(crypto, 0x01, 0),
			signed.senderDataSecret, &forged, PaddingSizeV1)
		if err != nil {
			t.Fatalf("signature octet %d: seal: %v", at, err)
		}
		_, err = OpenPrivateMessage(crypto, framingNewKeySource(crypto, 0x01, 0),
			signed.senderDataSecret, message, StaticSignatureKey(signed.pub), signed.groupContext)
		if !errors.Is(err, errBadSignature) {
			t.Fatalf("signature octet %d of %d: got %v, want a bad signature",
				at, len(signed.authContent.Auth.Signature), err)
		}
	}

	message, err := SealPrivateMessage(crypto, framingNewKeySource(crypto, 0x01, 0),
		signed.senderDataSecret, signed.authContent, PaddingSizeV1)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	for at := range signed.pub {
		wrong := append(SignaturePublicKey(nil), signed.pub...)
		wrong[at] ^= 0xff
		_, err := OpenPrivateMessage(crypto, framingNewKeySource(crypto, 0x01, 0),
			signed.senderDataSecret, message, StaticSignatureKey(wrong), signed.groupContext)
		if !errors.Is(err, errBadSignature) {
			t.Fatalf("public key octet %d of %d: got %v, want a bad signature", at, len(signed.pub), err)
		}
	}
	// and the group context, which is inside the signature preimage for a member sender
	for at := range signed.groupContext {
		wrong := append([]byte(nil), signed.groupContext...)
		wrong[at] ^= 0xff
		_, err := OpenPrivateMessage(crypto, framingNewKeySource(crypto, 0x01, 0),
			signed.senderDataSecret, message, StaticSignatureKey(signed.pub), wrong)
		if !errors.Is(err, errBadSignature) {
			t.Fatalf("group context octet %d of %d: got %v, want a bad signature", at, len(signed.groupContext), err)
		}
	}
}

// TestOpenPrivateMessageAnswersItsResolversRefusalVerbatim.
//
// The resolver is how the caller says whose key a leaf holds, and "I do not know that leaf" is a
// fact the caller has to be handed back unchanged: it is a gap in the tree, not a bad signature,
// and folding it into ValSem010 would send an operator looking for a forgery.
func TestOpenPrivateMessageAnswersItsResolversRefusalVerbatim(t *testing.T) {
	crypto := newTestCrypto(t)
	signed := framingPrivateSignedMember(t)
	message, err := SealPrivateMessage(crypto, framingNewKeySource(crypto, 0x01, 0),
		signed.senderDataSecret, signed.authContent, PaddingSizeV1)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	refusal := errors.New("the resolver has no key for that leaf")
	asked := []Sender{}
	resolve := func(sender Sender) (SignaturePublicKey, error) {
		asked = append(asked, sender)
		return nil, refusal
	}
	if _, err := OpenPrivateMessage(crypto, framingNewKeySource(crypto, 0x01, 0),
		signed.senderDataSecret, message, resolve, signed.groupContext); !errors.Is(err, refusal) {
		t.Fatalf("got %v, want the resolver's own refusal", err)
	}
	if len(asked) != 1 {
		t.Fatalf("the resolver was asked %d times, want once", len(asked))
	}
	// and it is asked about the sender the SENDER DATA named, not about one read off the
	// plaintext -- there is no sender in the plaintext to read
	want := Sender{SenderType: SenderTypeMember, LeafIndex: signed.authContent.Content.Sender.LeafIndex}
	if asked[0] != want {
		t.Fatalf("the resolver was asked about %+v, want %+v", asked[0], want)
	}
}

// TestOpenPrivateMessageRefusesNonZeroPaddingItSelfDecrypted is ValSem011 reached the way a peer
// reaches it: through a real seal, with a real AEAD tag over the offending plaintext.
//
// This is the only route to that check that is not a hand assembled plaintext. Reaching it requires
// a valid tag, so what it says is that a MEMBER cannot open a covert channel inside the padding --
// which is the threat, since the padding is inside the AEAD and outside every signature.
func TestOpenPrivateMessageRefusesNonZeroPaddingItSelfDecrypted(t *testing.T) {
	crypto := newTestCrypto(t)
	signed := framingPrivateSignedMember(t)
	for _, paddingSize := range []int{1, 2, 16, 64} {
		for offset := range paddingSize {
			padding := make([]byte, paddingSize)
			padding[offset] = 0x01
			message, err := sealPrivateMessage(crypto, framingNewKeySource(crypto, 0x01, 0),
				signed.senderDataSecret, signed.authContent, padding)
			if err != nil {
				t.Fatalf("padding %d octet %d: seal: %v", paddingSize, offset, err)
			}
			opened, err := OpenPrivateMessage(crypto, framingNewKeySource(crypto, 0x01, 0),
				signed.senderDataSecret, message, StaticSignatureKey(signed.pub), signed.groupContext)
			if !errors.Is(err, errNonZeroPadding) {
				t.Fatalf("padding %d octet %d: got %v, want errNonZeroPadding", paddingSize, offset, err)
			}
			if opened != nil {
				t.Errorf("padding %d octet %d: refused and answered a message alongside", paddingSize, offset)
			}
		}
		// and the all zero tail of the same length opens, so what refuses above is the
		// CONTENT of the padding and not its presence
		message, err := sealPrivateMessage(crypto, framingNewKeySource(crypto, 0x01, 0),
			signed.senderDataSecret, signed.authContent, make([]byte, paddingSize))
		if err != nil {
			t.Fatalf("padding %d: seal: %v", paddingSize, err)
		}
		if _, err := OpenPrivateMessage(crypto, framingNewKeySource(crypto, 0x01, 0),
			signed.senderDataSecret, message, StaticSignatureKey(signed.pub), signed.groupContext); err != nil {
			t.Fatalf("padding %d, all zero: %v", paddingSize, err)
		}
	}
}

// TestOpenPrivateMessageRefusesAPaddingTailThatFoldsToZeroItSelfDecrypted is the same class the
// decoder sweep holds, reached the way a member reaches it: through a real seal, with a real AEAD
// tag over the offending tail.
//
// The sweep above it in this file sets ONE padding octet, and every accumulator agrees on a tail
// like that. This is the shape that matters, because the covert channel is only worth anything to
// somebody who can produce a valid tag -- which is to say a member of the group -- and the tail it
// writes is as wide as the padding, not one octet of it.
func TestOpenPrivateMessageRefusesAPaddingTailThatFoldsToZeroItSelfDecrypted(t *testing.T) {
	crypto := newTestCrypto(t)
	signed := framingPrivateSignedMember(t)
	folds := framingCancellingByteFolds(t)
	sealed := 0
	for _, paddingSize := range framingPrivatePaddingLengths() {
		if paddingSize < 2 {
			continue
		}
		for _, fold := range folds {
			for _, payload := range framingCovertPayloads() {
				tail, built := framingFoldingPaddingTail(paddingSize, fold.fold, payload.at)
				if !built {
					continue
				}
				message, err := sealPrivateMessage(crypto, framingNewKeySource(crypto, 0x01, 0),
					signed.senderDataSecret, signed.authContent, tail)
				if err != nil {
					t.Fatalf("%d octets carrying %s closed under %s: seal: %v",
						paddingSize, payload.what, fold.name, err)
				}
				opened, err := OpenPrivateMessage(crypto, framingNewKeySource(crypto, 0x01, 0),
					signed.senderDataSecret, message, StaticSignatureKey(signed.pub), signed.groupContext)
				if !errors.Is(err, errNonZeroPadding) {
					t.Fatalf("%d octets carrying %s closed under %s: got %v, want errNonZeroPadding",
						paddingSize, payload.what, fold.name, err)
				}
				if opened != nil {
					t.Errorf("%d octets carrying %s closed under %s: refused and answered a message alongside",
						paddingSize, payload.what, fold.name)
				}
				sealed += 1
			}
		}
	}
	if sealed == 0 {
		t.Fatal("no fold in the derived family admitted a tail that closes, so this sweep sealed nothing")
	}
	t.Logf("%d sealed messages whose padding folds to zero were refused on open", sealed)
}

// TestSealPrivateMessageEmitsExactlyThePaddingItWasAskedFor.
//
// The ciphertext grows by exactly the padding size, which is what says the padding reached the
// plaintext rather than being counted somewhere and dropped, and PaddingSizeV1 emits none.
func TestSealPrivateMessageEmitsExactlyThePaddingItWasAskedFor(t *testing.T) {
	crypto := newTestCrypto(t)
	signed := framingPrivateSignedMember(t)
	unpadded, err := SealPrivateMessage(crypto, framingNewKeySource(crypto, 0x01, 0),
		signed.senderDataSecret, signed.authContent, 0)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	for _, paddingSize := range framingPrivateWidePaddingLengths() {
		message, err := SealPrivateMessage(crypto, framingNewKeySource(crypto, 0x01, 0),
			signed.senderDataSecret, signed.authContent, paddingSize)
		if err != nil {
			t.Fatalf("padding %d: seal: %v", paddingSize, err)
		}
		if len(message.Ciphertext) != len(unpadded.Ciphertext)+paddingSize {
			t.Fatalf("padding %d: the ciphertext is %d octets and the unpadded one is %d",
				paddingSize, len(message.Ciphertext), len(unpadded.Ciphertext))
		}
	}
	atV1, err := SealPrivateMessage(crypto, framingNewKeySource(crypto, 0x01, 0),
		signed.senderDataSecret, signed.authContent, PaddingSizeV1)
	if err != nil {
		t.Fatalf("seal at PaddingSizeV1: %v", err)
	}
	if len(atV1.Ciphertext) != len(unpadded.Ciphertext) {
		t.Errorf("a seal at PaddingSizeV1 is %d octets and one at zero padding is %d",
			len(atV1.Ciphertext), len(unpadded.Ciphertext))
	}
}

// ---------------------------------------------------------------------------
// the real secret tree as the message key source
// ---------------------------------------------------------------------------

// framingSecretTreeAt builds a secret tree and plants one ratchet's head, so a seal through it
// draws the generation this file is interested in.
//
// Planting is the only way to reach the top of the counter: a ratchet cannot be stepped four
// billion times in a test, and every generation this project has shipped a defect at was one no
// sampled test visited. It is the same seam TestRatchetRefusesToWrapTheGenerationCounter uses one
// layer down.
func framingSecretTreeAt(t *testing.T, crypto CryptoProvider, leaf LeafIndex,
	contentType ContentType, head uint32) *SecretTree {

	t.Helper()
	tree, err := NewSecretTree(crypto, 8, bytes.Repeat([]byte{0x2a}, crypto.HashSize()))
	if err != nil {
		t.Fatalf("NewSecretTree: %v", err)
	}
	kind, err := ratchetTypeOf(contentType)
	if err != nil {
		t.Fatalf("ratchetTypeOf(%d): %v", contentType, err)
	}
	r, err := tree.ratchetFor(leaf, kind)
	if err != nil {
		t.Fatalf("ratchetFor(%d, %d): %v", leaf, kind, err)
	}
	r.head = head
	return tree
}

// TestPrivateMessageRoundTripsThroughTheRealSecretTreeAtEveryBoundaryGeneration is the assertion
// that the interface this plan declares and the implementation p4 ships are the same thing in
// practice as well as at compile time.
//
// Two trees over one encryption secret, which is what a sender and a receiver actually hold: the
// sender's NextMessageKey consumes and the receiver's MessageKey looks the same generation up, and
// a single tree could not do both. The generations are the boundary ladder, so the wrap and the
// octet carries are visited rather than sampled around.
func TestPrivateMessageRoundTripsThroughTheRealSecretTreeAtEveryBoundaryGeneration(t *testing.T) {
	crypto := newTestCrypto(t)
	signed := framingPrivateSignedMember(t)
	leaf := signed.authContent.Content.Sender.LeafIndex
	contentType := signed.authContent.Content.ContentType
	for _, generation := range framingBoundaryGenerations() {
		sender := framingSecretTreeAt(t, crypto, leaf, contentType, generation)
		receiver := framingSecretTreeAt(t, crypto, leaf, contentType, generation)

		message, err := SealPrivateMessage(crypto, sender, signed.senderDataSecret,
			signed.authContent, PaddingSizeV1)
		if err != nil {
			t.Fatalf("generation %d: seal: %v", generation, err)
		}
		senderData := framingSenderDataOf(t, crypto, signed.senderDataSecret, message)
		if senderData.Generation != generation {
			t.Fatalf("a tree planted at generation %d sealed at generation %d", generation, senderData.Generation)
		}
		opened, err := OpenPrivateMessage(crypto, receiver, signed.senderDataSecret, message,
			StaticSignatureKey(signed.pub), signed.groupContext)
		if err != nil {
			t.Fatalf("generation %d: open: %v", generation, err)
		}
		if !bytes.Equal(opened.Content.ApplicationData, signed.authContent.Content.ApplicationData) {
			t.Fatalf("generation %d: application data %q", generation, opened.Content.ApplicationData)
		}
		// the open erased the generation it used, so a replay of the same octets finds no key
		_, err = OpenPrivateMessage(crypto, receiver, signed.senderDataSecret, message,
			StaticSignatureKey(signed.pub), signed.groupContext)
		if err == nil {
			t.Fatalf("generation %d: a replay of the same message opened a second time", generation)
		}
		if errors.Is(err, errNonZeroPadding) {
			t.Fatalf("generation %d: a replay was refused for its padding, which is not the reason", generation)
		}
	}
}

// TestARefusedOpenLeavesTheMessageKeyWhereItWasAndAnAcceptedOneErasesIt is the erase ordering,
// held from the side that decides whether one member can delete another member's message.
//
// WHAT THIS CASE USED TO SAY, because the inversion is the finding. It was
// TestOpenPrivateMessageErasesTheMessageKeyAheadOfEveryCheckThatCanStillRefuse, and it required a
// replay of a REFUSED ciphertext to answer ErrRatchetGenerationConsumed -- that is, it required the
// refused open to have consumed the generation. The argument was that holding the key open across
// the signature check "would leave a replay of the same ciphertext decryptable a second time". A
// replay of a ciphertext that FAILS is refused again by the same check, so that costs nothing; what
// the ordering actually bought was a way for any member to destroy any other member's key. RFC 9420
// section 9 derives the whole secret tree from encryption_secret, which every member holds, so
// every member can build a ciphertext that opens under any leaf's key at any generation. The erase
// then ran and the signature refused, and the genuine message at that generation was gone for good.
//
// SO BOTH HALVES ARE HELD HERE, and either alone is a gate that reads as coverage it does not have.
// The refusal half: a replay of a refused ciphertext reaches THE SAME CHECK again, which is only
// possible if the key survived. The acceptance half: an open that succeeds erases, erases exactly
// once, and erases the generation the message arrived at -- without which "leaves the key where it
// was" would be satisfied by a build that never erases at all and has no forward secrecy.
//
// Both refusals below the content open are swept, because the erase sits behind both now and an
// ordering held over one of them is an ordering held half way.
func TestARefusedOpenLeavesTheMessageKeyWhereItWasAndAnAcceptedOneErasesIt(t *testing.T) {
	crypto := newTestCrypto(t)
	signed := framingPrivateSignedMember(t)
	leaf := signed.authContent.Content.Sender.LeafIndex
	contentType := signed.authContent.Content.ContentType

	forged := *signed.authContent
	forged.Auth = FramedContentAuthData{Signature: append([]byte(nil), signed.authContent.Auth.Signature...)}
	forged.Auth.Signature[0] ^= 0xff

	rows := []struct {
		what     string
		content  *AuthenticatedContent
		padding  []byte
		sentinel error
	}{
		{what: "a padding tail the sender wrote into", content: signed.authContent,
			padding: []byte{0x01}, sentinel: errNonZeroPadding},
		{what: "a signature that does not verify", content: &forged,
			padding: nil, sentinel: errBadSignature},
	}

	for _, row := range rows {
		for _, generation := range framingBoundaryGenerations() {
			sender := framingSecretTreeAt(t, crypto, leaf, contentType, generation)
			receiver := framingSecretTreeAt(t, crypto, leaf, contentType, generation)
			message, err := sealPrivateMessage(crypto, sender, signed.senderDataSecret,
				row.content, row.padding)
			if err != nil {
				t.Fatalf("%s at generation %d: seal: %v", row.what, generation, err)
			}
			open := func() error {
				_, err := OpenPrivateMessage(crypto, receiver, signed.senderDataSecret, message,
					StaticSignatureKey(signed.pub), signed.groupContext)
				return err
			}
			if err := open(); !errors.Is(err, row.sentinel) {
				t.Fatalf("%s at generation %d: got %v, want %v", row.what, generation, err, row.sentinel)
			}
			replay := open()
			if errors.Is(replay, ErrRatchetGenerationConsumed) {
				t.Fatalf("%s at generation %d: the refused open CONSUMED the generation, so any member can delete any other member's message at that generation by sending one ciphertext it built itself",
					row.what, generation)
			}
			if !errors.Is(replay, row.sentinel) {
				t.Fatalf("%s at generation %d: a replay answered %v, want the same refusal %v",
					row.what, generation, replay, row.sentinel)
			}
			// and the genuine message at that generation still opens, which is the whole
			// stake: the refusal above must have cost its sender nothing.
			genuine, err := sealPrivateMessage(crypto,
				framingSecretTreeAt(t, crypto, leaf, contentType, generation),
				signed.senderDataSecret, signed.authContent, nil)
			if err != nil {
				t.Fatalf("%s at generation %d: seal the genuine message: %v", row.what, generation, err)
			}
			if _, err := OpenPrivateMessage(crypto, receiver, signed.senderDataSecret, genuine,
				StaticSignatureKey(signed.pub), signed.groupContext); err != nil {
				t.Fatalf("%s at generation %d: the GENUINE message at that generation no longer opens: %v. A refused ciphertext must cost the true sender nothing",
					row.what, generation, err)
			}
		}
	}

	// THE REFUSAL HALF, counted rather than inferred: a refused open erases nothing at all.
	for _, row := range rows {
		message, err := sealPrivateMessage(crypto, framingNewKeySource(crypto, 0x01, 0),
			signed.senderDataSecret, row.content, row.padding)
		if err != nil {
			t.Fatalf("%s: seal: %v", row.what, err)
		}
		keys := framingNewKeySource(crypto, 0x01, 0)
		if _, err := OpenPrivateMessage(crypto, keys, signed.senderDataSecret, message,
			StaticSignatureKey(signed.pub), signed.groupContext); !errors.Is(err, row.sentinel) {
			t.Fatalf("%s: got %v, want %v", row.what, err, row.sentinel)
		}
		if 0 < len(keys.erased) {
			t.Fatalf("%s: the refused open erased %v, want nothing", row.what, keys.erased)
		}
		// AND MOVED NOTHING, which is the half "erases nothing" cannot see. A build whose
		// lookup advanced the receiving head destroys the true sender's next MaxGenerationSkip
		// messages without erasing a single key, and passes the line above.
		if 0 < len(keys.committed) {
			t.Fatalf("%s: the refused open committed %v, want nothing", row.what, keys.committed)
		}
	}

	// THE ACCEPTANCE HALF, and it names the generation. Without it "erases nothing on a refusal"
	// is satisfied by a build that erases nothing ever, which has no forward secrecy at all.
	for _, generation := range framingBoundaryGenerations() {
		message, err := sealPrivateMessage(crypto, framingNewKeySource(crypto, 0x01, generation),
			signed.senderDataSecret, signed.authContent, nil)
		if err != nil {
			t.Fatalf("seal at generation %d: %v", generation, err)
		}
		keys := framingNewKeySource(crypto, 0x01, generation)
		if _, err := OpenPrivateMessage(crypto, keys, signed.senderDataSecret, message,
			StaticSignatureKey(signed.pub), signed.groupContext); err != nil {
			t.Fatalf("the message at generation %d did not open: %v", generation, err)
		}
		spent := fmt.Sprintf("%d/%d/%d", contentType, leaf, generation)
		if !slices.Equal(keys.committed, []string{spent}) {
			t.Fatalf("an accepted open at generation %d committed %v, want exactly [%s]",
				generation, keys.committed, spent)
		}
	}
}

// TestThePeekAgreesWithTheOpenOnEveryMessageThatOpens is what makes PeekPrivateMessageSender safe
// to refuse on: the THREE values it reads before the ratchet are values the open then confirms.
//
// A caller uses the peek to take its own refusals EARLY, and then takes them again on what
// OpenPrivateMessage answers. That is only a pre-filter rather than a second, weaker rule if the
// early reading and the authenticated reading are the same reading. They are, by construction --
// the leaf is the sender data's and the open builds its Sender from the same field, and the
// authenticated_data is a cleartext header field the content AEAD covers -- and construction is
// what this case turns into a measurement, over every boundary generation.
//
// THE GENERATION IS THE THIRD, since MASTER section 8.4.2 v2 made it one of the values a caller's
// R2 is a function of. It is confirmed against the generation the message was SEALED at, which is
// the strongest form available here and is why this case is where the claim lives: a frame's
// content key is derived from the generation the sender data names, so a frame that opens at all
// opened at the generation the peek read.
//
// THE OTHER DIRECTION IS THE ONE THAT MATTERS AND IT IS HERE TOO: a message whose cleartext
// authenticated_data has been moved does not open at all, so there is no message that opens and
// disagrees with its own peek.
func TestThePeekAgreesWithTheOpenOnEveryMessageThatOpens(t *testing.T) {
	crypto := newTestCrypto(t)
	signed := framingPrivateSignedMember(t)
	leaf := signed.authContent.Content.Sender.LeafIndex
	contentType := signed.authContent.Content.ContentType

	for _, generation := range framingBoundaryGenerations() {
		message, err := sealPrivateMessage(crypto,
			framingSecretTreeAt(t, crypto, leaf, contentType, generation),
			signed.senderDataSecret, signed.authContent, nil)
		if err != nil {
			t.Fatalf("seal at generation %d: %v", generation, err)
		}
		marshalled, err := MarshalMLSMessage(&MLSMessage{
			Version:        ProtocolVersionMls10,
			WireFormat:     WireFormatPrivateMessage,
			PrivateMessage: message,
		})
		if err != nil {
			t.Fatalf("marshal at generation %d: %v", generation, err)
		}
		peekLeaf, peekAad, peekGeneration, err := PeekPrivateMessageSender(crypto, signed.senderDataSecret, marshalled)
		if err != nil {
			t.Fatalf("peek at generation %d: %v", generation, err)
		}
		if peekGeneration != generation {
			t.Fatalf("the peek read generation %d for a frame sealed at generation %d",
				peekGeneration, generation)
		}
		opened, err := OpenPrivateMessage(crypto,
			framingSecretTreeAt(t, crypto, leaf, contentType, generation),
			signed.senderDataSecret, message, StaticSignatureKey(signed.pub), signed.groupContext)
		if err != nil {
			t.Fatalf("open at generation %d: %v", generation, err)
		}
		if peekLeaf != opened.Content.Sender.LeafIndex {
			t.Fatalf("at generation %d the peek read leaf %d and the open authenticated leaf %d",
				generation, peekLeaf, opened.Content.Sender.LeafIndex)
		}
		if !bytes.Equal(peekAad, opened.Content.AuthenticatedData) {
			t.Fatalf("at generation %d the peek read aad %x and the open authenticated %x",
				generation, peekAad, opened.Content.AuthenticatedData)
		}
	}

	// THE PEEK IS NOT A SECOND COPY OF THE FIELD: moving the cleartext authenticated_data moves
	// the peek's answer AND stops the message opening, so no message both opens and disagrees.
	message, err := sealPrivateMessage(crypto, framingSecretTreeAt(t, crypto, leaf, contentType, 0),
		signed.senderDataSecret, signed.authContent, nil)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	moved := *message
	moved.AuthenticatedData = append(append([]byte(nil), message.AuthenticatedData...), 0x5a)
	marshalled, err := MarshalMLSMessage(&MLSMessage{
		Version:        ProtocolVersionMls10,
		WireFormat:     WireFormatPrivateMessage,
		PrivateMessage: &moved,
	})
	if err != nil {
		t.Fatalf("marshal the moved message: %v", err)
	}
	_, peekAad, _, err := PeekPrivateMessageSender(crypto, signed.senderDataSecret, marshalled)
	if err != nil {
		t.Fatalf("peek the moved message: %v", err)
	}
	if bytes.Equal(peekAad, message.AuthenticatedData) {
		t.Fatal("the peek answered the original authenticated_data for a message whose field was moved")
	}
	if _, err := OpenPrivateMessage(crypto, framingSecretTreeAt(t, crypto, leaf, contentType, 0),
		signed.senderDataSecret, &moved, StaticSignatureKey(signed.pub), signed.groupContext); err == nil {
		t.Fatal("a message whose cleartext authenticated_data was moved OPENED")
	}
}

// PeekPrivateMessageSender refuses what it cannot read, rather than answering a zero leaf.
//
// A zero leaf is leaf 0, an ordinary member, so an answer-on-failure here would hand a caller a
// claim about the founder for octets that were never a message.
func TestThePeekRefusesEveryShapeItCannotRead(t *testing.T) {
	crypto := newTestCrypto(t)
	signed := framingPrivateSignedMember(t)
	leaf := signed.authContent.Content.Sender.LeafIndex
	contentType := signed.authContent.Content.ContentType
	message, err := sealPrivateMessage(crypto, framingSecretTreeAt(t, crypto, leaf, contentType, 0),
		signed.senderDataSecret, signed.authContent, nil)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	marshalled, err := MarshalMLSMessage(&MLSMessage{
		Version:        ProtocolVersionMls10,
		WireFormat:     WireFormatPrivateMessage,
		PrivateMessage: message,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	rows := map[string][]byte{
		"octets that are not an MLSMessage at all": []byte("not a frame"),
		"an empty body":          {},
		"a truncated MLSMessage": marshalled[:len(marshalled)/2],
	}
	for what, octets := range rows {
		gotLeaf, gotAad, gotGeneration, err := PeekPrivateMessageSender(crypto, signed.senderDataSecret, octets)
		if err == nil {
			t.Errorf("%s peeked to leaf %d / aad %x with no error", what, gotLeaf, gotAad)
		}
		if gotLeaf != 0 || gotAad != nil || gotGeneration != 0 {
			t.Errorf("%s answered leaf %d and %d octets of aad beside its error", what, gotLeaf, len(gotAad))
		}
	}

	// A PublicMessage is a well formed MLSMessage this call still refuses, and by a value of its
	// own: it carries no sender data, so answering anything about its sender would be an
	// invention rather than a reading. It is the shape that separates "these octets are not a
	// message" from "these octets are a message of the wrong kind".
	public, err := MarshalMLSMessage(&MLSMessage{
		Version:    ProtocolVersionMls10,
		WireFormat: WireFormatPublicMessage,
		PublicMessage: &PublicMessage{
			Content:       signed.authContent.Content,
			Auth:          signed.authContent.Auth,
			MembershipTag: bytes.Repeat([]byte{0x11}, crypto.HashSize()),
		},
	})
	if err != nil {
		t.Fatalf("marshal a PublicMessage: %v", err)
	}
	if _, _, _, err := PeekPrivateMessageSender(crypto, signed.senderDataSecret, public); !errors.Is(err, errPeekWireFormat) {
		t.Errorf("a PublicMessage peeked with %v, want errPeekWireFormat", err)
	}

	// and a nil provider is refused rather than dereferenced.
	if _, _, _, err := PeekPrivateMessageSender(nil, signed.senderDataSecret, marshalled); !errors.Is(err, ErrNilCryptoProvider) {
		t.Errorf("a nil crypto provider peeked with %v, want ErrNilCryptoProvider", err)
	}
}

// TestSealPrivateMessageRefusesRatherThanWrappingTheGenerationCounter is the boundary this project
// has shipped a defect at one layer down, held from the layer that consumes it.
//
// A wrap is not a lost message. It is the generation numbers on the wire starting again at zero
// under keys that have moved on, so every one of them collides with a number the receiver has
// already marked consumed and the four billionth message silently becomes a replay of the first.
// What is asserted is the REFUSAL and the absence of generation zero, not merely that something
// went wrong: a seal that wrapped would answer a perfectly well formed message with a nil error.
func TestSealPrivateMessageRefusesRatherThanWrappingTheGenerationCounter(t *testing.T) {
	last := ^uint32(0)
	crypto := newTestCrypto(t)
	signed := framingPrivateSignedMember(t)
	leaf := signed.authContent.Content.Sender.LeafIndex
	contentType := signed.authContent.Content.ContentType
	tree := framingSecretTreeAt(t, crypto, leaf, contentType, last-1)

	produced := []uint32{}
	for i := range 2 {
		message, err := SealPrivateMessage(crypto, tree, signed.senderDataSecret,
			signed.authContent, PaddingSizeV1)
		if err != nil {
			t.Fatalf("seal %d at the top of the counter: %v", i, err)
		}
		produced = append(produced,
			framingSenderDataOf(t, crypto, signed.senderDataSecret, message).Generation)
	}
	if !slices.Equal(produced, []uint32{last - 1, last}) {
		t.Fatalf("the last two seals produced generations %v, want %v", produced, []uint32{last - 1, last})
	}
	for i := range 3 {
		message, err := SealPrivateMessage(crypto, tree, signed.senderDataSecret,
			signed.authContent, PaddingSizeV1)
		if !errors.Is(err, ErrRatchetExhausted) {
			t.Fatalf("seal %d past the end of the counter: got %v, want ErrRatchetExhausted", i, err)
		}
		if message != nil {
			generation := framingSenderDataOf(t, crypto, signed.senderDataSecret, message).Generation
			t.Fatalf("seal %d past the end of the counter answered a message at generation %d", i, generation)
		}
	}
}

// ---------------------------------------------------------------------------
// MASTER section 8.4.2 v2: the S3 pin and the derived framed length
// ---------------------------------------------------------------------------

// framingSkippingKeySource is a key source whose NextMessageKey answers a generation OTHER than the
// one it hands keys for -- the exact shape MASTER section 8.4.2 names as the mutation for S1.
//
// It is not a hypothetical about a hostile tree. It is what any drift between the generation a
// sealer READ and the generation the seal CONSUMED looks like from inside the seal, and the whole
// point of S3 is that the seal must be able to see it without knowing how it happened.
type framingSkippingKeySource struct {
	inner *framingKeySource
	skip  uint32
}

func (self *framingSkippingKeySource) NextMessageKey(contentType ContentType,
	leaf LeafIndex) ([]byte, []byte, uint32, error) {

	key, nonce, generation, err := self.inner.NextMessageKey(contentType, leaf)
	return key, nonce, generation + self.skip, err
}

func (self *framingSkippingKeySource) MessageKey(contentType ContentType, leaf LeafIndex,
	generation uint32) ([]byte, []byte, error) {

	return self.inner.MessageKey(contentType, leaf, generation)
}

func (self *framingSkippingKeySource) CommitMessageKey(contentType ContentType, leaf LeafIndex,
	generation uint32) error {

	return self.inner.CommitMessageKey(contentType, leaf, generation)
}

func (self *framingSkippingKeySource) EraseMessageKey(contentType ContentType, leaf LeafIndex,
	generation uint32) {

	self.inner.EraseMessageKey(contentType, leaf, generation)
}

// TestTheSealRefusesWhenTheGenerationConsumedIsNotTheOneTheAadNames is MASTER section 8.4.2's S3,
// held as a case rather than as an argument.
//
// THE PROPERTY, and it is not the same as S1: every frame a sealer EMITS names, in its AAD, the
// generation it is sealed under. S1 is the obligation and this is what makes it checkable -- a
// build in which the two came apart would emit a frame every peer refuses at R2, for a reason the
// sender cannot see, on every message it ever writes. The pin turns that into one local refusal.
//
// THE MUTATION MASTER SECTION 8.4.2 NAMES IS THE FIXTURE: hand the seal a key source that skips one
// generation and require the call to refuse rather than return octets. Deleting the comparison in
// sealPrivateMessageBound turns this red.
//
// THE CONTROL IS THE SAME CALL WITH NO SKIP, so the refusal is about the disagreement rather than
// about a fixture that never sealed anything.
func TestTheSealRefusesWhenTheGenerationConsumedIsNotTheOneTheAadNames(t *testing.T) {
	crypto := newTestCrypto(t)
	signed := framingPrivateSignedMember(t)

	// the control: the generation the source hands out IS the one the aad names.
	agreeing := framingNewKeySource(crypto, 0x31, 7)
	message, err := sealPrivateMessageBound(crypto, agreeing, signed.senderDataSecret,
		signed.authContent, nil, 7)
	if err != nil {
		t.Fatalf("a seal whose consumed generation is the one the aad names was refused: %v", err)
	}
	if message == nil {
		t.Fatal("the control seal answered no message and no error")
	}
	if got := framingSenderDataOf(t, crypto, signed.senderDataSecret, message).Generation; got != 7 {
		t.Fatalf("the control sealed at generation %d, want 7", got)
	}

	// and the skips, in both directions, because a pin written as "greater than" would admit half
	// of them.
	for _, skip := range []uint32{1, 1024} {
		skipping := &framingSkippingKeySource{inner: framingNewKeySource(crypto, 0x31, 7), skip: skip}
		message, err := sealPrivateMessageBound(crypto, skipping, signed.senderDataSecret,
			signed.authContent, nil, 7)
		if !errors.Is(err, errSealGenerationNotBound) {
			t.Errorf("a seal that consumed generation %d while its aad named 7 answered %v, want errSealGenerationNotBound",
				7+skip, err)
		}
		if message != nil {
			t.Errorf("the refused seal returned a message beside its error; MASTER section 8.4.2's S3 is that it MUST emit NOTHING")
		}
	}
	// the other direction: the aad names a generation the seal never reaches.
	behind := framingNewKeySource(crypto, 0x31, 7)
	if message, err := sealPrivateMessageBound(crypto, behind, signed.senderDataSecret,
		signed.authContent, nil, 9); !errors.Is(err, errSealGenerationNotBound) || message != nil {
		t.Errorf("a seal whose aad named generation 9 while the ratchet stood at 7 answered (%v, %v), want no message and errSealGenerationNotBound",
			message != nil, err)
	}
}

// TestTheRegisteredSignatureWidthIsTheWidthTheProviderProduces holds SuiteParams.Nsig to the
// scheme rather than to a number somebody typed.
//
// WHY IT MATTERS RATHER THAN BEING TIDY: Nsig is a term of FramedApplicationLength, which MASTER
// section 8.4.6 makes a sealer's early size refusal arithmetic. A registry that disagreed with the
// scheme by even one octet would refuse a legal body at one boundary, or admit one the seal must
// then refuse late -- after a stream index and an MLS generation are spent, which is the whole
// defect section 8.4.6 exists to end.
func TestTheRegisteredSignatureWidthIsTheWidthTheProviderProduces(t *testing.T) {
	for _, suite := range Suites() {
		crypto, err := NewCryptoProvider(suite)
		if err != nil {
			t.Fatalf("NewCryptoProvider(%#04x): %v", uint16(suite), err)
		}
		params, err := LookupSuite(suite)
		if err != nil {
			t.Fatalf("LookupSuite(%#04x): %v", uint16(suite), err)
		}
		priv, _, err := crypto.SignatureKeyPair()
		if err != nil {
			t.Fatalf("SignatureKeyPair over %#04x: %v", uint16(suite), err)
		}
		signature, err := crypto.SignWithLabel(priv, framedContentTBSLabel, []byte("a preimage of some length"))
		if err != nil {
			t.Fatalf("SignWithLabel over %#04x: %v", uint16(suite), err)
		}
		if len(signature) != params.Nsig {
			t.Errorf("suite %#04x registers Nsig %d and its provider produced a %d octet signature; MASTER section 8.4.6's framed length is arithmetic over this number",
				uint16(suite), params.Nsig, len(signature))
		}
		// and it is not NsigPub under another name, which is the transposition a reader makes:
		// ed25519 has 32 octet keys and 64 octet signatures.
		if params.Nsig == params.NsigPub {
			t.Errorf("suite %#04x registers Nsig and NsigPub at the same %d octets, so a body that read the key width where it wanted the signature width would compute the same answer",
				uint16(suite), params.Nsig)
		}
	}
}

// TestFramedApplicationLengthRefusesASuiteWithNoRegisteredSignatureWidth is the fail-closed half.
//
// A zero Nsig is a SuiteParams that was assembled rather than looked up, and this package's own
// tests build those. Answering a length for one would answer a number 66 octets short of the truth
// -- which is not a wrong number, it is an early refusal that admits a body the seal must then
// refuse after spending a write once index and a write once generation.
func TestFramedApplicationLengthRefusesASuiteWithNoRegisteredSignatureWidth(t *testing.T) {
	if _, err := FramedApplicationLength(CipherSuite(0xfffe), 32, 32, 0); !errors.Is(err, ErrUnknownCipherSuite) {
		t.Errorf("an unregistered suite answered %v, want ErrUnknownCipherSuite", err)
	}
	for _, suite := range Suites() {
		if _, err := FramedApplicationLength(suite, 32, 32, -1); err == nil {
			t.Errorf("suite %#04x answered a length for a negative plaintext", uint16(suite))
		}
	}
}

// TestFramedApplicationLengthRefusesASuiteWithNoSignatureWidth reaches the guard the registry can
// never fire, which is why framedApplicationLengthFor is split out at all.
//
// A zero Nsig is a SuiteParams that was assembled rather than looked up. Answering a length for one
// would answer a number 66 octets short of the truth -- not a wrong number in the abstract, but an
// early refusal that ADMITS a body the seal must then refuse after spending a write once stream
// index and a write once MLS generation, which is the whole defect MASTER section 8.4.6 ends.
func TestFramedApplicationLengthRefusesASuiteWithNoSignatureWidth(t *testing.T) {
	assembled := SuiteParams{Suite: CipherSuite(0xfffe), Nt: 16}
	if _, err := framedApplicationLengthFor(&assembled, 32, 32, 0); !errors.Is(err, errFramedLengthSuiteWidth) {
		t.Errorf("a suite with no registered signature width answered %v, want errFramedLengthSuiteWidth", err)
	}
	if _, err := framedApplicationLengthFor(nil, 32, 32, 0); !errors.Is(err, errFramedLengthSuiteWidth) {
		t.Errorf("nil parameters answered %v, want errFramedLengthSuiteWidth", err)
	}
	// and the control: the same call over a registered suite's own parameters answers a length.
	params, err := LookupSuite(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("LookupSuite: %v", err)
	}
	if length, err := framedApplicationLengthFor(params, 32, 32, 0); err != nil || length == 0 {
		t.Errorf("a registered suite answered (%d, %v), want a length and no error", length, err)
	}
}

// TestProtectBoundBindsTheGenerationTheFrameIsSealedAt is MASTER section 8.4.2's S1 and S2 driven
// over a real group, and it is where errNilAadBuilder is reached.
//
// WHAT IT OBSERVES, and it is the sentence the builder shape exists for: the generation the builder
// is HANDED is the generation the frame is actually sealed at, so an AAD built out of it names the
// frame's own position in the sender's ratchet. The peek reads that generation back before any
// ratchet moves, and the open reports the same one -- which is what makes MASTER section 8.4.3's R2
// checkable at all.
func TestProtectBoundBindsTheGenerationTheFrameIsSealedAt(t *testing.T) {
	crypto := testCrypto(t)
	sender, receiver, _, _ := testTwoMemberGroup(t, crypto)

	// the refusal first: a bound seal with no builder is a caller whose wiring is wrong, and an
	// empty AAD is a legal value, so defaulting would seal a frame naming no position at all and
	// answer it as though it had been bound.
	if _, err := sender.ProtectBound(nil, []byte("no builder")); !errors.Is(err, errNilAadBuilder) {
		t.Errorf("a bound seal with no aad builder answered %v, want errNilAadBuilder", err)
	}

	senderDataSecret, err := sender.EpochSecret(EpochSecretSenderData)
	if err != nil {
		t.Fatalf("EpochSecret: %v", err)
	}
	seen := []uint32{}
	for i := 0; i < 4; i += 1 {
		handed := uint32(0)
		built := 0
		frame, err := sender.ProtectBound(func(generation uint32) ([]byte, error) {
			handed = generation
			built += 1
			// the AAD NAMES the generation, which is the whole shape: four big endian
			// octets of it, exactly as MASTER section 8.4.2's term (3) writes them.
			return []byte{byte(generation >> 24), byte(generation >> 16),
				byte(generation >> 8), byte(generation)}, nil
		}, fmt.Appendf(nil, "message %d", i))
		if err != nil {
			t.Fatalf("ProtectBound %d: %v", i, err)
		}
		if built != 1 {
			t.Errorf("the builder was called %d times for one seal, want 1", built)
		}
		// the PEEK, which is the reading MASTER section 8.4.3's R3 requires to answer the
		// generation before any ratchet is reached.
		_, peekAad, peekGeneration, err := PeekPrivateMessageSender(crypto, senderDataSecret, frame)
		if err != nil {
			t.Fatalf("peek %d: %v", i, err)
		}
		if peekGeneration != handed {
			t.Errorf("the builder was handed generation %d and the frame's sender data names %d; MASTER section 8.4.2's S1 is that these are the same number",
				handed, peekGeneration)
		}
		want := []byte{byte(handed >> 24), byte(handed >> 16), byte(handed >> 8), byte(handed)}
		if !bytes.Equal(peekAad, want) {
			t.Errorf("the frame carries aad %x and the generation it is sealed at is %d", peekAad, handed)
		}
		// and the OPEN reports the same generation, which is the second reading R2 is taken on.
		opened, err := receiver.Unprotect(frame)
		if err != nil {
			t.Fatalf("Unprotect %d: %v", i, err)
		}
		if opened.Generation != handed {
			t.Errorf("the open reports generation %d and the seal consumed %d", opened.Generation, handed)
		}
		if !bytes.Equal(opened.AuthenticatedData, want) {
			t.Errorf("the open authenticated aad %x, want %x", opened.AuthenticatedData, want)
		}
		seen = append(seen, handed)
	}
	// the generations advance by one per seal and none repeats, which is what says the builder is
	// reading the ratchet rather than a constant.
	for i, generation := range seen {
		if i > 0 && generation != seen[i-1]+1 {
			t.Errorf("the seals ran at generations %v; an application ratchet advances by one per seal", seen)
			break
		}
	}
}

// TestTheDerivedFramedLengthIsTheLengthProtectActuallyEmits is what makes MASTER section 8.4.6's
// early size refusal a rule rather than an approximation.
//
// THE RULE it stands under: an application record's size refusal is taken over
// framed_length(len(bodyPlain)), BEFORE a stream index is reserved and BEFORE a generation is
// spent. That is only a rule if framed_length is the length the seal actually emits. One octet
// short and the check admits a body the seal must then refuse late, after spending both; one octet
// long and it refuses a legal body at the boundary.
//
// THE LENGTHS ARE THE STEP FUNCTION'S OWN BOUNDARIES AND NOT A SAMPLE. MASTER section 8.4.4 records
// that the frame's overhead has FOUR steps and that the corpus published three of them for two
// days, because the ladder was measured by walking and the step function beside it was derived by
// hand. Two nested varints widen: varint(P) inside the ciphertext at 64 and at 16,384, and
// varint(C) around it at C = 16,384, which is P = 16,300. Both sides of all three boundaries are
// driven here.
//
// IT IS SEALED WITH A REAL SIGNATURE, which is what makes the signature width a measurement rather
// than a transcription: framingPrivateSignedMember signs with the suite's own scheme, so a registry
// whose Nsig disagreed with the provider moves this comparison rather than agreeing with itself.
func TestTheDerivedFramedLengthIsTheLengthProtectActuallyEmits(t *testing.T) {
	crypto := newTestCrypto(t)
	signed := framingPrivateSignedMember(t)
	content := signed.authContent.Content
	leaf := content.Sender.LeafIndex
	contentType := content.ContentType
	aad := bytes.Repeat([]byte{0x5a}, 32)

	overheads := map[int]int{}
	for _, plaintext := range []int{0, 1, 63, 64, 65, 16299, 16300, 16350, 16383, 16384, 65334} {
		authContent := &AuthenticatedContent{
			WireFormat: WireFormatPrivateMessage,
			Content: FramedContent{
				GroupId:           content.GroupId,
				Epoch:             content.Epoch,
				Sender:            content.Sender,
				AuthenticatedData: aad,
				ContentType:       contentType,
				ApplicationData:   make([]byte, plaintext),
			},
			Auth: signed.authContent.Auth,
		}
		message, err := SealPrivateMessage(crypto, framingSecretTreeAt(t, crypto, leaf, contentType, 0),
			signed.senderDataSecret, authContent, PaddingSizeV1)
		if err != nil {
			t.Fatalf("seal a %d octet plaintext: %v", plaintext, err)
		}
		marshalled, err := MarshalMLSMessage(&MLSMessage{
			Version:        ProtocolVersionMls10,
			WireFormat:     WireFormatPrivateMessage,
			PrivateMessage: message,
		})
		if err != nil {
			t.Fatalf("marshal a %d octet plaintext's frame: %v", plaintext, err)
		}
		derived, err := FramedApplicationLength(crypto.Suite(), len(content.GroupId), len(aad), plaintext)
		if err != nil {
			t.Fatalf("FramedApplicationLength(%d): %v", plaintext, err)
		}
		if derived != len(marshalled) {
			t.Errorf("the derived framed length of a %d octet plaintext is %d and the seal emitted %d",
				plaintext, derived, len(marshalled))
		}
		overheads[plaintext] = len(marshalled) - plaintext
	}
	// AND THE STEP FUNCTION HAS FOUR STEPS, held as the SHAPE rather than as the four numbers.
	// MASTER section 8.4.4's 193 / 194 / 196 / 198 are its expected answer at a 32 OCTET group id,
	// and this fixture's group id is shorter -- so the absolute column belongs to
	// connect/messagegroup's sweep, which runs over a real 32 octet one, and what belongs here is
	// the part that is a property of the encoding: three boundaries, each widening by exactly the
	// varint it widens, and nothing moving between them. A build carrying the old THREE step form
	// answers the 16,300 row equal to the 16,299 row and fails here.
	for _, boundary := range []struct {
		below, at, widensBy int
	}{
		{below: 63, at: 64, widensBy: 1},
		{below: 16299, at: 16300, widensBy: 2},
		{below: 16383, at: 16384, widensBy: 2},
	} {
		if got := overheads[boundary.at] - overheads[boundary.below]; got != boundary.widensBy {
			t.Errorf("the overhead widens by %d across the boundary at %d octets and RFC 9420's varint widens by %d there",
				got, boundary.at, boundary.widensBy)
		}
	}
	for _, band := range [][]int{{0, 1, 63}, {64, 65, 16299}, {16300, 16350, 16383}, {16384, 65334}} {
		for _, plaintext := range band[1:] {
			if overheads[plaintext] != overheads[band[0]] {
				t.Errorf("the overhead at %d octets is %d and at %d octets is %d, and both are inside one band of the step function",
					band[0], overheads[band[0]], plaintext, overheads[plaintext])
			}
		}
	}
	// and the AAD's VALUE moves nothing, which is the measurement that makes framed_length a
	// function of the plaintext's length ALONE and therefore computable before anything is spent.
	for _, value := range [][]byte{make([]byte, 32), bytes.Repeat([]byte{0xff}, 32), crypto.Random(32)} {
		authContent := &AuthenticatedContent{
			WireFormat: WireFormatPrivateMessage,
			Content: FramedContent{
				GroupId:           content.GroupId,
				Epoch:             content.Epoch,
				Sender:            content.Sender,
				AuthenticatedData: value,
				ContentType:       contentType,
				ApplicationData:   make([]byte, 100),
			},
			Auth: signed.authContent.Auth,
		}
		message, err := SealPrivateMessage(crypto, framingSecretTreeAt(t, crypto, leaf, contentType, 0),
			signed.senderDataSecret, authContent, PaddingSizeV1)
		if err != nil {
			t.Fatalf("seal under a %x aad: %v", value[:4], err)
		}
		marshalled, err := MarshalMLSMessage(&MLSMessage{
			Version:        ProtocolVersionMls10,
			WireFormat:     WireFormatPrivateMessage,
			PrivateMessage: message,
		})
		if err != nil {
			t.Fatalf("marshal under a %x aad: %v", value[:4], err)
		}
		if got := len(marshalled) - 100; got != overheads[65] {
			t.Errorf("a 100 octet plaintext under a 32 octet aad of %x framed to overhead %d and the same band answers %d; the aad's VALUE must not move any length",
				value[:4], got, overheads[65])
		}
	}
	t.Logf("overheads by plaintext length: %v", overheads)
}
