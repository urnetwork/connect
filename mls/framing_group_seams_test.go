// The two construction-bypass seams the validation forge is built on, and the gate that keeps
// them out of everything this package ships.
//
// p8's ValSem002-011 negative tests each need a message ON THE WIRE that no correct sender would
// ever produce -- a group id from some other conversation, an epoch that has already passed, a
// stripped membership tag, non-zero padding -- while the rest of the message is this group's real
// one, sealed under this epoch's real keys, so the receiver refuses it for the rule under test
// rather than because the ciphertext was nonsense. The production Seal* entry points cannot
// express any of that, and that is the whole of their value: every authenticator they put on the
// wire is one they computed themselves.
//
// So these two are a deliberate hole in the guarantees of a package whose entire worth is that
// its guarantees hold, and the only question that matters about them is whether a shipped binary
// can reach them. IT CANNOT, and the reason is the name of this file rather than a sentence in
// it: the go tool compiles a _test.go file into `go test`'s binary and into nothing else, so a
// production file naming either seam does not build at all. "Documented as test only" would have
// been an assertion; this is the compiler.
//
// p6's plan named a NON-test file, framing_group_seams.go, and would have excused the
// uncalled-declaration failure that follows with a packageDeclarationsAwaitingTheirFirstCaller
// entry. Both halves of that are refused here. Compiled into the package proper, the seams ship
// in every binary that imports mls; and the excuse table's entire safety argument is expiry by
// failure -- an entry dies on the commit that gives its declaration a production caller -- so an
// entry for a seam is the one entry that can never expire, which is what
// TestNoExcuseAwaitingAFirstCallerNamesAnExpiryThatCannotArrive already refuses. Task 11's count
// form of the section 6.3.1 serializer went into framing_protect_test.go for exactly this reason,
// and these go here for it.
//
// The gate at the foot of this file is the half the compiler does not hold. The compiler says
// production cannot call a declaration of the test binary; nothing said these declarations STAY
// in the test binary, and moving both into framing_group_seams.go together WITH a production
// caller passes every other gate this package has -- measured, not assumed. The class it reads is
// derived from what a construction bypass DOES rather than from these two names.
//
// What this cannot see, said out loud, AND THIS LINE OF WORK IS CLOSED AT THAT. Three rounds have
// each traded one axis of this gate for another -- the anchor for the door, the position for the
// door, the types for the generators -- and the property underneath all three is a code review
// property: no production declaration forges a message. What the COMPILER enforces is that a
// _test.go file is compiled into no shipped binary, and that already holds without a line of this.
// So what follows is recorded rather than built for, and a fourth axis is not opened.
//
//   - A forge whose CALLER fills a package level variable of a carrier type one frame earlier and
//     then calls a no-argument forge. Nothing arrives in a position its caller fills, so the
//     handed-over half reads it as being handed nothing, and it passes -- measured -- while
//     spelling the door's own name in its body. The whole handed-over half is a statement about
//     signatures, and a package level variable is not one.
//   - A call edge whose name is ambiguous. The walk used to drop such an edge outright, which was
//     a forge plus a two line helper renamed to collide -- MarshalMLS and UnmarshalMLS are carried
//     by fourteen declarations here, so colliding is free. That condition is gone, and what
//     replaced it follows an edge into EVERY declaration carrying the name: the walk now
//     over-reports rather than under-reports, so the residue is a verdict a declaration inherits
//     from a namesake rather than an exit. Over-reporting fails loudly, which is the direction
//     this file chooses everywhere it has to choose.
//   - A second declaration of the door itself: a body handed a whole *MLSMessage by its caller
//     that answers syntax.Marshal over it and names no message of its own. "No derivation
//     separates the two" was written here and is measurably wrong, so what is true is written
//     instead. Five production declarations of this package name the door type in a signature --
//     (*MLSMessage).populatedArms, (*MLSMessage).MarshalMLS, (*MLSMessage).UnmarshalMLS,
//     MarshalMLSMessage and ParseMLSMessage -- and not one of them is ALSO handed a carrier that
//     is not the door type: the four that take the message in a position their caller fills take
//     nothing beside it but a *syntax.Writer, a *syntax.Reader, or no parameter at all, and the
//     fifth ANSWERS a message rather than being handed one. So "handed the message and some other
//     carrier too" separates the codec from a forge exactly, over this source, and the shape this
//     bullet is really about is the narrower one left over: a declaration handed the message and
//     nothing else, which holds no state of its own to seal under and can do nothing to the
//     octets that its caller has not already done. The forging in that shape happens in the
//     CALLER, which is where this gate catches it or does not.
//   - The two shapes constructionBypassSeamsIn records below: a seam answering a *PrivateMessage
//     rather than an *MLSMessage or its octets, and one taking its forged tag as a bare []byte
//     rather than as the structure that carries it.
package mls

import (
	"bytes"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// the seams
// ---------------------------------------------------------------------------

// sealFramedContentForTest seals a caller-assembled FramedContent under this group's real epoch
// keys, with the caller's own FramedContentAuthData in place of the authenticators a correct
// sender would have computed.
//
// It is the zero-padding entry point and it delegates, so there is one copy of the forge rather
// than two that agree until the day one of them changes.
func (self *Group) sealFramedContentForTest(c *FramedContent, auth *FramedContentAuthData,
	wf WireFormat, signer SignaturePrivateKey) ([]byte, error) {

	return self.sealFramedContentWithPaddingForTest(c, auth, wf, signer, nil)
}

// sealFramedContentWithPaddingForTest is the same seal with caller-supplied padding octets, which
// is the shape ValSem011 needs: PaddingSizeV1 is 0 and the exported SealPrivateMessage takes a
// paddingSize int, so a NON-zero padding tail is expressible through the unexported
// sealPrivateMessage and through nothing else this package offers.
//
// The answer is a whole encoded MLSMessage, through the same MarshalMLSMessage every sender
// leaves by, so the forge hands the receiver exactly what the network would rather than a struct
// the receive path never had to parse.
//
// Only the SENDER-CHOSEN fields are the caller's. The group context, the membership key, the
// sender data secret and the secret tree are this group's own, which is why these are methods on
// *Group at all: a forge holding its own keys would produce a message the receiver refuses at the
// first AEAD open, and a negative test that fails for two reasons at once establishes neither.
//
// No stateLock is taken, and that is a decision rather than an omission. GroupContext takes it
// and sync.Mutex is not reentrant, so a seam that locked around this call would deadlock on its
// first use; a Group is documented as unsafe for concurrent use and the forge drives one from a
// single test goroutine. It does CONSUME a generation of the sender's ratchet, exactly as a real
// send does, because the message is meant to be indistinguishable from one.
func (self *Group) sealFramedContentWithPaddingForTest(c *FramedContent, auth *FramedContentAuthData,
	wf WireFormat, signer SignaturePrivateKey, padding []byte) ([]byte, error) {

	groupContext, err := self.GroupContext()
	if err != nil {
		return nil, err
	}
	// signed NORMALLY first, so the signature is over the caller's own content and the forge is
	// confined to the auth data. The alternative -- assembling an AuthenticatedContent by hand --
	// would put a message on the wire failing the signature check as well as the rule under test.
	authContent, err := SignAuthenticatedContent(self.crypto, signer, wf, c, groupContext)
	if err != nil {
		return nil, err
	}
	// nil leaves the honest signature standing, which is what the ValSem002-004 tests want: those
	// forge the CONTENT and need the authenticators to be real, so the refusal is the group id or
	// the epoch and not a signature nobody wrote.
	if auth != nil {
		authContent.Auth = *auth
	}

	// the field is spelled schedule. p6's plan wrote self.keySchedule against a p7 draft that
	// never landed under that name, and p7 owns this struct.
	secrets := self.schedule.Secrets()
	message := &MLSMessage{Version: ProtocolVersionMls10, WireFormat: wf}
	switch wf {
	case WireFormatPublicMessage:
		publicMessage, err := SealPublicMessage(self.crypto, secrets.Membership, authContent,
			groupContext)
		if err != nil {
			return nil, err
		}
		message.PublicMessage = publicMessage
	case WireFormatPrivateMessage:
		privateMessage, err := sealPrivateMessage(self.crypto, self.secretTree, secrets.SenderData,
			authContent, padding)
		if err != nil {
			return nil, err
		}
		message.PrivateMessage = privateMessage
	default:
		return nil, ErrWireFormatMismatch
	}
	return MarshalMLSMessage(message)
}

// ---------------------------------------------------------------------------
// what the seams actually put on the wire
// ---------------------------------------------------------------------------

// seamSealPrivate seals one private message through the padding seam and hands back the arm a
// receiver would be given, so the three padding cases below differ in the padding and in nothing
// else.
func seamSealPrivate(t *testing.T, group *Group, content *FramedContent, owner *testMember,
	padding []byte) *PrivateMessage {

	t.Helper()
	encoded, err := group.sealFramedContentWithPaddingForTest(content, nil, WireFormatPrivateMessage,
		owner.SigPriv, padding)
	if err != nil {
		t.Fatalf("seal a private message with %d octets of padding: %v", len(padding), err)
	}
	message, err := ParseMLSMessage(encoded)
	if err != nil {
		t.Fatalf("the seam's octets do not parse as an MLSMessage: %v", err)
	}
	if message.WireFormat != WireFormatPrivateMessage || message.PrivateMessage == nil {
		t.Fatalf("the seam answered wire format %d with private arm %v, want a PrivateMessage",
			message.WireFormat, message.PrivateMessage)
	}
	return message.PrivateMessage
}

// TestTheConstructionBypassSeamsPutTheGroupsOwnMessageOnTheWire is the seams' own contract, and
// it is deliberately NOT a second copy of p8's ValSem catalogue.
//
// p6's plan says to write no test here, on the grounds that two tests over one seam drift into
// disagreeing about which fields the caller controls. For the TEN RULES that is right: each of
// them is a statement about the receive path and each of them is p8's. What the reasoning does
// not cover is the seam itself, and the gap is the shape this project keeps paying for. Every one
// of those ten tests reads a refusal, so a seam that sealed under the wrong epoch secret, dropped
// the padding it was handed, or ignored the auth data entirely still produces a refusal at every
// one of them: ten green tests over a forge that forges nothing.
//
// So what is asserted here is the property that has to hold before any of those ten mean
// anything, and nothing beyond it. With NOTHING forged the seam produces a message this group's
// own receive path ACCEPTS; and each of the three things a caller controls -- the content, the
// auth data, the padding -- is observed on the wire rather than assumed to have reached it.
func TestTheConstructionBypassSeamsPutTheGroupsOwnMessageOnTheWire(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	group := testNewGroup(t, crypto, owner, "seam-group")
	defer group.Close()

	groupContext, err := group.GroupContext()
	if err != nil {
		t.Fatalf("GroupContext: %v", err)
	}
	context := testGroupContextOf(t, group)
	// a PROPOSAL and not application data, because ValSem005 refuses application content in a
	// PublicMessage on the send path as well as on the receive one; one content type is what lets
	// both arms below carry the same message.
	content := &FramedContent{
		GroupId:           context.GroupId,
		Epoch:             context.Epoch,
		Sender:            Sender{SenderType: SenderTypeMember, LeafIndex: group.OwnLeafIndex()},
		AuthenticatedData: []byte{0x09},
		ContentType:       ContentTypeProposal,
		Proposal:          &Proposal{ProposalType: ProposalTypeRemove, Remove: &Remove{Removed: 3}},
	}
	resolve := StaticSignatureKey(owner.SigPub)
	secrets := group.schedule.Secrets()

	// the public arm with nothing forged: this epoch's membership tag, the owner's signature, and
	// the receive path a peer would run.
	encoded, err := group.sealFramedContentForTest(content, nil, WireFormatPublicMessage,
		owner.SigPriv)
	if err != nil {
		t.Fatalf("seal a public message through the seam: %v", err)
	}
	message, err := ParseMLSMessage(encoded)
	if err != nil {
		t.Fatalf("the seam's octets do not parse as an MLSMessage: %v", err)
	}
	if message.WireFormat != WireFormatPublicMessage || message.PublicMessage == nil {
		t.Fatalf("the seam answered wire format %d with public arm %v, want a PublicMessage",
			message.WireFormat, message.PublicMessage)
	}
	opened, err := OpenPublicMessage(crypto, secrets.Membership, message.PublicMessage, resolve,
		groupContext)
	if err != nil {
		t.Fatalf("this group's own receive path refuses the seam's UNFORGED message: %v; a forge whose honest output is already refused says nothing about the rule a negative test is aiming at",
			err)
	}
	if opened.Content.ContentType != ContentTypeProposal ||
		opened.Content.Sender.LeafIndex != group.OwnLeafIndex() ||
		!bytes.Equal(opened.Content.AuthenticatedData, content.AuthenticatedData) ||
		!bytes.Equal(opened.Content.GroupId, content.GroupId) {

		t.Errorf("the message that came back carries %+v and the caller assembled %+v; the seam is not sending the content it was handed",
			opened.Content, *content)
	}

	// and the auth data really is the caller's. A signature over nothing, carried through the
	// seal and refused by the SIGNATURE check -- the membership tag verifies, because the seam
	// MACs what it is about to send rather than what it signed, which is what makes a forged
	// signature reach the receiver's signature check at all.
	forged := &FramedContentAuthData{
		Signature: bytes.Repeat([]byte{0xaa}, len(opened.Auth.Signature))}
	encoded, err = group.sealFramedContentForTest(content, forged, WireFormatPublicMessage,
		owner.SigPriv)
	if err != nil {
		t.Fatalf("seal a forged public message through the seam: %v", err)
	}
	message, err = ParseMLSMessage(encoded)
	if err != nil {
		t.Fatalf("the forged message does not parse: %v", err)
	}
	if !bytes.Equal(message.PublicMessage.Auth.Signature, forged.Signature) {
		t.Fatalf("the seam put %x on the wire and the caller's auth data carried %x; nothing p8 forges through the auth argument would reach a receiver",
			message.PublicMessage.Auth.Signature, forged.Signature)
	}
	if _, err := OpenPublicMessage(crypto, secrets.Membership, message.PublicMessage, resolve,
		groupContext); err == nil {
		t.Errorf("the receive path accepted a message carrying a signature over nothing")
	}

	// the private arm, opened on a RECEIVER's own ratchet at this epoch rather than on the
	// sender's: the seal consumes the generation it sealed under, exactly as a real send does, so
	// the sending group cannot also be the receiving one.
	receiver, err := NewSecretTree(crypto, group.tree.LeafWidth(), secrets.Encryption)
	if err != nil {
		t.Fatalf("NewSecretTree: %v", err)
	}
	unpadded := seamSealPrivate(t, group, content, owner, nil)
	padded := seamSealPrivate(t, group, content, owner, make([]byte, seamPaddingOctets))
	nonZero := seamSealPrivate(t, group, content, owner,
		bytes.Repeat([]byte{0x01}, seamPaddingOctets))

	if grew := len(padded.Ciphertext) - len(unpadded.Ciphertext); grew != seamPaddingOctets {
		t.Errorf("%d octets of padding grew the ciphertext by %d; the padding the caller hands the seam is not reaching the wire, and ValSem011 has nothing to be a rule about",
			seamPaddingOctets, grew)
	}
	// in generation order, because that is the order they were sealed in and one receiver ratchet
	// walks forward through them the way a real one does.
	if _, err := OpenPrivateMessage(crypto, receiver, secrets.SenderData, unpadded, resolve,
		groupContext); err != nil {

		t.Fatalf("this group's own receive path refuses the seam's UNFORGED private message: %v", err)
	}
	if _, err := OpenPrivateMessage(crypto, receiver, secrets.SenderData, padded, resolve,
		groupContext); err != nil {

		t.Fatalf("the receive path refuses an all zero padded message the seam produced: %v", err)
	}
	if _, err := OpenPrivateMessage(crypto, receiver, secrets.SenderData, nonZero, resolve,
		groupContext); !errors.Is(err, errNonZeroPadding) {

		t.Errorf("a non zero padding tail was answered %v, want errNonZeroPadding; ValSem011 is the one rule in the catalogue this package can produce a case for through nothing but this seam",
			err)
	}

	// the wire format the seams refuse. Welcome is a REGISTERED format that carries no framed
	// content, so it reaches the select rather than being turned away by the signature preimage,
	// which is what makes this the default arm's own refusal and not some earlier one.
	if _, err := group.sealFramedContentForTest(content, nil, WireFormatWelcome,
		owner.SigPriv); !errors.Is(err, ErrWireFormatMismatch) {

		t.Errorf("sealing under a wire format that frames no content answered %v, want ErrWireFormatMismatch",
			err)
	}
}

// seamPaddingOctets is the padding tail the two cases above differ by. Wide enough that a
// ciphertext growing by it is not a length this codec could have produced by accident.
const seamPaddingOctets = 32

// ---------------------------------------------------------------------------
// and they stay in the test binary
// ---------------------------------------------------------------------------

// The door every octet this package sends leaves through, the type it leaves as, and the
// authenticators a correct assembly computes for itself.
//
// These are the gate's ANCHORS and not its class. What rule 5 refuses is a class written out as a
// list of its members, which is how a hand written roster comes to understate the thing it
// guards; a derivation still has to start from something the package declares, and each of these
// three is checked below against the package's own declarations, so a rename that moved one fails
// here rather than quietly emptying the rule.
//
// seamForgedAuthenticators is the SEED of the handed-over half and not the whole of it, and the
// distinction is the one this file was reopened for. A caller hands a seam the authenticators
// when the type in the position it fills CARRIES them, not when its name is this one, and
// seamAuthenticatorCarriersIn below is where that class is derived.
//
// seamWireDoorType is read at BOTH ends and the two readings are different claims. Where a whole
// message LEAVES a declaration -- its results -- it has reached the wire with the marshal one
// frame up. Where it is NAMED IN A BODY it says which of the derived doors below the body is
// standing at: a serializer generic over every wire type of this package is a whole message
// reaching the wire only when the body names a whole message too.
//
// Reading the body mention ALONE is what the previous round removed, and removing it was the
// regression this one is here to undo. It was removed for a real cost -- (*MLSMessage).UnmarshalMLS
// names its own type while CONSUMING octets, so once the receiver joined the handed-over half every
// codec of this package became a candidate -- and the trade was paid for out of the door: three
// production forges that call MarshalMLSMessage's WRAPPEE instead of MarshalMLSMessage walked
// straight under the narrowed reading. Both halves are held here. The mention is read, and it is
// read together with a door, so a decoder that names the message while producing no octets names
// no door and stays out.
//
// seamWireDoor is the SEED of the door half and not the whole of it, exactly as
// seamForgedAuthenticators is the seed of the handed-over half. MarshalMLSMessage is five lines --
// a nil check and syntax.Marshal -- so the name is a wrapper and the octets are produced under it;
// seamWireDoorsIn below is where the rest of the chain is derived.
const (
	seamWireDoor             = "MarshalMLSMessage"
	seamWireDoorType         = "MLSMessage"
	seamForgedAuthenticators = "FramedContentAuthData"
)

// One declaration of this package, as this gate reads it.
type seamCandidate struct {
	// Type.Method for a method and the bare name otherwise, which is how a report names it.
	name string
	// the method or function name alone, which is how ANOTHER body names it. A method is reached
	// through a selector whose tail carries no receiver, so the reachability walk below is keyed
	// on this and the report on the name above.
	bare string
	file string
	// its CALLER handed it the authenticators, so the caller chose what the receiver will check.
	forgeable bool
	// a whole message LEAVES it, which is the door type read where a declaration answers with
	// one rather than wherever the name is mentioned.
	answersTheDoorType bool
	// the door TYPE stands in a position its caller fills. It is what separates a serializer of
	// the message from a serializer of everything: the first can be named alone.
	takesTheDoorType bool
	// OCTETS leave it, read off its results. A door hands its answer down to the next frame, and
	// a frame that answers no octets is not the one the answer came from. The octet type is the
	// language's own and is spelled rather than anchored, because nothing can rename it.
	answersOctets bool
	// the callees its own RETURN statements name: what it hands its answer BACK from. This is how
	// a wrapper is walked down to the declaration that does the work, and it is deliberately
	// narrower than the body -- MarshalMLS returns fmt.Errorf on three paths, so a walk over every
	// callee puts Errorf in the door class and reads the whole package as reaching the wire.
	answersFrom map[string]bool
	// the receiver type name as written, or the empty string for a free function.
	receiver string
	// a GROUP stands in a position its caller fills, read over the same closure and the same
	// positions the authenticators are read over. It is what the proposal generator rule asks
	// instead of naming the receiver: a free function handed a *Group and a method on a type
	// holding one send exactly what a method on the group sends.
	holdsTheGroup bool
	// whether the go tool exports it, which is the surface a caller outside this package has.
	exported bool
	// every identifier its body names.
	names map[string]bool
	// the callee names its body writes in CALL POSITION, whether or not the answer is used.
	//
	// It is what the reachability walk follows, and it is narrower than names for a reason that
	// only appeared once the scan covered three packages: a selector tail carries no receiver AND
	// no distinction between a call and a FIELD READ, so processed.Commit -- a field of this
	// package's own Processed -- reads in names exactly like a call of a method named Commit. The
	// day connect/messagegroup declared (*connectMlsHandle).Commit, which reaches the wire door
	// through CreateCommit, every declaration of this package that so much as reads .Commit off a
	// struct became reachable: measured, 24 production declarations of this package -- the whole
	// framing layer, its codecs and its preimages -- were reported as construction bypasses, and
	// every one of them was a false positive of the walk rather than a widening of the class.
	//
	// It is narrower than consumes, deliberately: consumes drops a call whose answer is thrown
	// away, and a forge that assembles a message and sends it as a STATEMENT has still put it on
	// the wire. What this reads is "this body calls that", which is what an edge in a call graph
	// means.
	calls map[string]bool
	// the callees its body CALLS and whose answer it does not throw away: a call in a return,
	// in an if's init or condition, in an assignment to something other than the blank
	// identifier, or nested inside another call's arguments.
	//
	// It is the difference between running a door and mentioning one. `_ = ValidateProposalList`
	// puts the name in names and nothing in here; so does calling it as a statement and
	// dropping the error, which is the shape that certified a generator while validating
	// nothing. A gate asking "did this reach its doors" has to read the answer being used,
	// because the answer is the whole of what a door produces.
	consumes map[string]bool
	// the identifiers its body names in a VALUE position -- assigned, passed, or written into a
	// composite literal -- as opposed to the ones it only compares against.
	//
	// This is what separates a sender from a demultiplexer, and it is the reading the proposal
	// generator rule was missing. A body that writes ContentType: ContentTypeProposal has CHOSEN
	// what the message is; a body with `case ContentTypeProposal:` or
	// `if x.ContentType != ContentTypeProposal` has asked what somebody else chose, and every
	// codec, every validator and every ratchet selector in this package asks that about all
	// three. Reading the mention alone put the whole of the send path -- SealPrivateMessage down
	// to marshalPrivateMessageContentWithPadding, whose switch names every content type -- into
	// the set of declarations that emit a proposal.
	chooses map[string]bool
	// the content types this body WRITES into the framed content's discriminant, and whether any
	// of those writes is an expression this reading cannot pin to a named one.
	//
	// The other half of "what does this declaration put on the wire", and it is here because the
	// half above rests on the content type being SPELLED OUT. A generator that re-frames a cached
	// proposal writes `ContentType: cached.ContentType`: it frames, signs, seals and sends a
	// proposal while naming ContentTypeProposal in no value position at all -- the only mention in
	// such a body is the guard that ASKED whether what it was handed was a proposal, which is a
	// comparison. So it chooses nothing the reading above can see, and it sends whatever it was
	// handed; and what it can be handed includes a proposal. Narrowing the mention to the value
	// position was right and it traded one under-report for another, which is what this reads.
	//
	// A body that writes a NAMED content type has told this reading which message it is building.
	// A body that writes anything else has not, and "cannot tell" resolves to EVERY content type,
	// which is the direction this obligation has to fail in: an over-report is a generator asked
	// to run doors it already runs, and an under-report is the generator the rule exists to catch.
	//
	// A DEMULTIPLEXER WRITES NEITHER, which is what keeps the send path out of a class about
	// proposals. marshalPrivateMessageContentWithPadding switches on the discriminant of a framed
	// content it was HANDED and assembles none of its own -- the same separation `chooses` makes,
	// made over the assembly instead of over the mention.
	frames        map[string]bool
	framesUnknown bool
}

// seamAuthenticatorCarriersIn answers the type names a caller can put its own authenticators
// inside: the seed itself, every type declared in the scan holding one in a field, every type
// holding one of THOSE, and so on to a fixed point.
//
// THIS IS THE HALF THAT WAS NAMED RATHER THAN DERIVED, and what naming it cost was measured
// rather than argued. The rule read one identifier -- a parameter handed over the authenticators
// when its type spelled FramedContentAuthData -- while AuthenticatedContent CARRIES that type in
// a field, and BOTH of this package's production seal entry points take one:
// SealPublicMessage(crypto, membershipKey, authContent, groupContext) and
// sealPrivateMessage(crypto, tree, senderData, authContent, padding). So a construction bypass of
// identical power written over *AuthenticatedContent -- same file, same unexported caller, only
// the parameter type differing -- passed this gate while the two real seams failed it, and would
// have shipped in every binary that imports mls with this gate reporting the package clean.
//
// The walk enters EVERY field and not the exported ones alone, which is where it parts company
// with typeReachesNamed next door, deliberately. That walk asks what a holder in ANOTHER package
// can spell; this one asks what a caller can hand a seam, and the caller of a seam is inside the
// package that declares it -- both of this file's are methods on *Group -- so an unexported field
// is storage a forger can fill in.
//
// Non struct declarations are followed whole. type X = AuthenticatedContent, type X
// AuthenticatedContent and type X []AuthenticatedContent are each a spelling that carries the
// authenticators, and a walk reading struct fields alone would read a seam over any of the three
// as taking nothing.
func seamAuthenticatorCarriersIn(parsed []parsedSource) map[string]bool {
	return seamCarriersOf(parsed, seamForgedAuthenticators)
}

// seamCarriersOf is the closure above with the seed as an argument, so the two questions that
// need one -- "what can a caller put its own authenticators inside" and "what can a caller hand
// a group inside" -- are asked by one walk rather than by two that agree until the day one of
// them is edited.
//
// The generator rule needs the same closure for the same reason this one does. Its position half
// named the receiver, so a free function taking a *Group, and a method on a type holding one,
// were outside a class stated over what a declaration DOES; that is the defect this file's own
// header records having fixed for the seam rule and reintroduced in the rule next door.
func seamCarriersOf(parsed []parsedSource, seed string) map[string]bool {
	named := seamTypeFieldNamesIn(parsed)
	carriers := map[string]bool{seed: true}
	for grew := true; grew; {
		grew = false
		for name, names := range named {
			if carriers[name] {
				continue
			}
			for other := range names {
				if carriers[other] {
					carriers[name] = true
					grew = true
					break
				}
			}
		}
	}
	return carriers
}

// seamIdentifiersOnlyCompared answers the identifier nodes of one body that stand in a
// comparison rather than in a value: the two sides of an == or a !=, and the expressions of a
// case clause, which is a switch's own way of spelling the same comparison.
//
// The node set is the answer and not the names, because the same name can stand in both
// positions in one body -- a validator that refuses a content type and then writes it into the
// message it builds -- and a reading keyed on the name would lose one of the two.
func seamIdentifiersOnlyCompared(body ast.Node) map[*ast.Ident]bool {
	compared := map[*ast.Ident]bool{}
	mark := func(node ast.Node) {
		ast.Inspect(node, func(inner ast.Node) bool {
			if identifier, isIdentifier := inner.(*ast.Ident); isIdentifier {
				compared[identifier] = true
			}
			return true
		})
	}
	ast.Inspect(body, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.BinaryExpr:
			if typed.Op == token.EQL || typed.Op == token.NEQ {
				mark(typed.X)
				mark(typed.Y)
			}
		case *ast.CaseClause:
			for _, expr := range typed.List {
				mark(expr)
			}
		}
		return true
	})
	return compared
}

// seamDiscardedCalls answers the call expressions of one body whose answer goes nowhere: a call
// standing alone as a statement, a call whose results are all assigned to the blank identifier,
// and the call a go or a defer statement makes.
//
// A call in any other position hands its answer to something -- a return, a condition, a
// variable, another call's argument list -- and that is what "ran the door" means. Both shapes
// here were measured to certify a generator that validates nothing: ValidateProposalList(nil) as
// a statement, and _ = ValidateProposalList(nil).
func seamDiscardedCalls(body ast.Node) map[ast.Expr]bool {
	discarded := map[ast.Expr]bool{}
	ast.Inspect(body, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.ExprStmt:
			discarded[typed.X] = true
		case *ast.GoStmt:
			discarded[typed.Call] = true
		case *ast.DeferStmt:
			discarded[typed.Call] = true
		case *ast.AssignStmt:
			blank := true
			for _, left := range typed.Lhs {
				name, isBare := left.(*ast.Ident)
				if !isBare || name.Name != "_" {
					blank = false
					break
				}
			}
			if blank {
				for _, right := range typed.Rhs {
					discarded[right] = true
				}
			}
		}
		return true
	})
	return discarded
}

// seamTypeFieldNamesIn answers, per type the scan declares, the identifiers its FIELD TYPES name.
//
// Field types and not the whole struct, because a struct inspected whole yields its field NAMES
// too -- and a field called Auth beside a type called Auth would put a type into the closure for
// the spelling of a label rather than for what it holds.
func seamTypeFieldNamesIn(parsed []parsedSource) map[string]map[string]bool {
	named := map[string]map[string]bool{}
	for _, source := range parsed {
		for _, declaration := range source.file.Decls {
			general, isGeneral := declaration.(*ast.GenDecl)
			if !isGeneral || general.Tok != token.TYPE {
				continue
			}
			for _, specification := range general.Specs {
				typeSpec, isType := specification.(*ast.TypeSpec)
				if !isType {
					continue
				}
				held := named[typeSpec.Name.Name]
				if held == nil {
					held = map[string]bool{}
					named[typeSpec.Name.Name] = held
				}
				bodies := []ast.Expr{typeSpec.Type}
				if structType, isStruct := typeSpec.Type.(*ast.StructType); isStruct &&
					structType.Fields != nil {

					bodies = []ast.Expr{}
					for _, field := range structType.Fields.List {
						bodies = append(bodies, field.Type)
					}
				}
				for _, body := range bodies {
					ast.Inspect(body, func(node ast.Node) bool {
						if identifier, isIdentifier := node.(*ast.Ident); isIdentifier {
							held[identifier.Name] = true
						}
						return true
					})
				}
			}
		}
	}
	return named
}

// seamCandidatesIn reads one set of parsed files into the shape the two rules below ask about.
//
// THE POSITION IS DERIVED AND NOT NAMED, and naming it is what this half got wrong. The rule read
// function.Type.Params alone while the receiver was touched only to build a report name, and the
// reasoning written here for that -- a method whose RECEIVER is the authenticators is a codec of
// them, so banning that shape would ban the encoder every honest sender runs -- mistook a
// consequence for a rule. What separates (*FramedContentAuthData).MarshalMLS from a forge is that
// it serializes a FRAGMENT and never reaches the wire door, and that half of the class holds
// whichever position the authenticators arrived in.
//
// What naming the position cost was measured, and the rework that derived the carrier closure
// made it strictly WORSE: the unprotected receiver set had been {FramedContentAuthData} and
// became the whole derived closure, so every type newly protected as a parameter became newly
// unprotected as a receiver. A production file forge of identical power --
// (*FramedContentAuthData).reviewSealUnder, handed the group, signing through its crypto, sealing
// under this epoch's real keys and answering MarshalMLSMessage -- passed this gate with a byte
// identical suite, and so did the same shape over *AuthenticatedContent and over *MLSMessage.
//
// So a declaration is handed the authenticators when a carrier stands in any position its CALLER
// fills in: the receiver first, then the parameters. A result is not one of those -- nobody hands
// a declaration what it answers -- and that same asymmetry is what the door type is read by
// below.
//
// The carriers are read off the SAME parsed set the candidates are, so a scan widened to a root
// widens the closure with it: a type declared in connect/message that holds an
// mls.FramedContentAuthData is a type a forge over there is handed.
func seamCandidatesIn(parsed []parsedSource) []seamCandidate {
	carriers := seamAuthenticatorCarriersIn(parsed)
	// the carrier a sender assembles, its discriminant field and the names of the content
	// types this scan declares. All three are derived -- the field off the compiled structure
	// by its TYPE, the constants off the const declarations -- so the reading below is anchored
	// on what the package has rather than on what somebody wrote down about it.
	contentCarrier, contentDiscriminant, _ := framedContentCarrier()
	contentTypes := theContentTypeConstants()
	// the same closure over the type the proposal generator rule is stated about. The anchor is
	// group_test.go's, checked against this package's own declarations there before anything is
	// derived from it, so a rename that moved the type fails rather than quietly emptying a class.
	groupCarriers := seamCarriersOf(parsed, proposalGenerationReceiver)
	candidates := []seamCandidate{}
	for _, source := range parsed {
		for _, declaration := range source.file.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil {
				continue
			}
			candidate := seamCandidate{
				name:        function.Name.Name,
				bare:        function.Name.Name,
				file:        source.fileSet.Position(function.Pos()).Filename,
				exported:    function.Name.IsExported(),
				names:       map[string]bool{},
				calls:       map[string]bool{},
				answersFrom: map[string]bool{},
				consumes:    map[string]bool{},
				chooses:     map[string]bool{},
				frames:      map[string]bool{},
			}
			// every position the caller fills in, gathered before any of them is read, so the
			// receiver and the parameters are answered by ONE walk rather than by two rules that
			// agree until the day one of them is edited.
			supplied := []ast.Expr{}
			if function.Recv != nil && len(function.Recv.List) == 1 {
				candidate.receiver = receiverTypeName(function.Recv.List[0].Type)
				candidate.name = candidate.receiver + "." + candidate.bare
				supplied = append(supplied, function.Recv.List[0].Type)
			}
			for _, field := range function.Type.Params.List {
				supplied = append(supplied, field.Type)
			}
			for _, position := range supplied {
				ast.Inspect(position, func(node ast.Node) bool {
					identifier, isIdentifier := node.(*ast.Ident)
					if !isIdentifier {
						return true
					}
					if carriers[identifier.Name] {
						candidate.forgeable = true
					}
					if identifier.Name == seamWireDoorType {
						candidate.takesTheDoorType = true
					}
					if groupCarriers[identifier.Name] {
						candidate.holdsTheGroup = true
					}
					return true
				})
			}
			// and the answering side, which is where a whole message leaves a declaration. A
			// forge that hands its caller an assembled *MLSMessage rather than the octets has put
			// the authenticators on the wire just as surely, with the marshal one frame up.
			if function.Type.Results != nil {
				for _, field := range function.Type.Results.List {
					ast.Inspect(field.Type, func(node ast.Node) bool {
						identifier, isIdentifier := node.(*ast.Ident)
						if !isIdentifier {
							return true
						}
						if identifier.Name == seamWireDoorType {
							candidate.answersTheDoorType = true
						}
						if identifier.Name == "byte" {
							candidate.answersOctets = true
						}
						return true
					})
				}
			}
			compared := seamIdentifiersOnlyCompared(function.Body)
			discarded := seamDiscardedCalls(function.Body)
			// what this body writes into the framed content's discriminant, recorded as the named
			// content type it wrote or as "cannot tell" for anything else. Written as a closure so
			// the composite literal and the assignment forms answer through ONE reading: a body
			// that builds the carrier and one that overwrites the field of a carrier it was handed
			// have both decided what goes on the wire.
			framed := func(value ast.Expr) {
				if named, isNamed := value.(*ast.Ident); isNamed && contentTypes[named.Name] {
					candidate.frames[named.Name] = true
					return
				}
				candidate.framesUnknown = true
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				if identifier, isIdentifier := node.(*ast.Ident); isIdentifier {
					candidate.names[identifier.Name] = true
					if !compared[identifier] {
						candidate.chooses[identifier.Name] = true
					}
				}
				// the carrier assembled here, keyed by the field this package's own structure
				// declares as its discriminant.
				if literal, isLiteral := node.(*ast.CompositeLit); isLiteral && literal.Type != nil &&
					slices.Contains(identifiersNamedIn(literal.Type), contentCarrier) {

					for _, element := range literal.Elts {
						pair, isPair := element.(*ast.KeyValueExpr)
						if !isPair {
							continue
						}
						if key, isKey := pair.Key.(*ast.Ident); isKey && key.Name == contentDiscriminant {
							framed(pair.Value)
						}
					}
				}
				// and the discriminant written over after the fact. The base of the selector is
				// not resolved to a type, so a write to the same field of the WIRE header is read
				// here too -- an over-report, and over-reporting is the direction this half fails
				// in: a header whose content type is whatever it was handed puts whatever it was
				// handed on the wire just as surely.
				if assignment, isAssignment := node.(*ast.AssignStmt); isAssignment {
					for i, left := range assignment.Lhs {
						selector, isSelector := left.(*ast.SelectorExpr)
						if !isSelector || selector.Sel.Name != contentDiscriminant {
							continue
						}
						if i < len(assignment.Rhs) {
							framed(assignment.Rhs[i])
						}
					}
				}
				// and the callees whose answer this body USES, which is the reading that tells a
				// door that was run from one that was named. The callee's own identifier is
				// excluded from the chosen set above by nothing -- a call is a value position --
				// so the two readings are kept apart here rather than conflated.
				if call, isCall := node.(*ast.CallExpr); isCall {
					switch callee := call.Fun.(type) {
					case *ast.Ident:
						candidate.calls[callee.Name] = true
						if !discarded[call] {
							candidate.consumes[callee.Name] = true
						}
					case *ast.SelectorExpr:
						candidate.calls[callee.Sel.Name] = true
						if !discarded[call] {
							candidate.consumes[callee.Sel.Name] = true
						}
					}
				}
				// and the answered callees on their own, which is the door chain's edge. A
				// declaration hands its answer to the frame it RETURNS, so that is where the
				// wrappee is read from rather than from every call the body makes.
				if returned, isReturn := node.(*ast.ReturnStmt); isReturn {
					for _, result := range returned.Results {
						ast.Inspect(result, func(inner ast.Node) bool {
							call, isCall := inner.(*ast.CallExpr)
							if !isCall {
								return true
							}
							switch callee := call.Fun.(type) {
							case *ast.Ident:
								candidate.answersFrom[callee.Name] = true
							case *ast.SelectorExpr:
								candidate.answersFrom[callee.Sel.Name] = true
							}
							return true
						})
					}
				}
				return true
			})
			candidates = append(candidates, candidate)
		}
	}
	return candidates
}

// theContentTypeConstants is the names THIS PACKAGE declares as constants of the framing content
// type, read off its own source once.
//
// AN ANCHOR AND NOT A READING OF THE SCAN, for the reason the door name and the receiver name are
// anchors, and measured rather than argued: the control the generator rule is judged against is a
// synthetic package that USES this package's content type constants while declaring none, so a set
// read off the sources under test calls every write in that control "cannot tell". On the first run
// of this rule that reported the control's commit sender and its application sender -- two of the
// four negatives -- as proposal generators.
//
// A read that fails answers the empty set, which makes every write unknown: an over-report, which
// is a failing test rather than a silent pass. The gate asserts the set is non-empty rather than
// resting on that fallback.
var theContentTypeConstants = sync.OnceValue(func() map[string]bool {
	_, _, kind := framedContentCarrier()
	scan, err := scanSources([]string{"."})
	if err != nil {
		return map[string]bool{}
	}
	parsed := []parsedSource{}
	texts := productionSources(scan.sourceTexts)
	for _, path := range slices.Sorted(maps.Keys(texts)) {
		fileSet := token.NewFileSet()
		file, err := parser.ParseFile(fileSet, path, texts[path], parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		parsed = append(parsed, parsedSource{fileSet: fileSet, file: file})
	}
	return contentTypeConstantsIn(parsed, kind)
})

// contentTypeConstantsIn answers the names this scan declares as constants of the framing content
// type, so the reading above can tell a body that NAMED a content type from one that wrote an
// expression.
//
// Derived off the const declarations rather than written down, for standing rule 5's reason and
// for a concrete one: the list is the registry, and a fourth content type registered tomorrow is
// a value a generator can frame from. A transcription of three names would read that fourth as
// "cannot tell" -- which errs safe here, and would still be a list somebody had to remember.
//
// The type of a spec with no type of its own is the previous spec's, which is the language's own
// repetition rule and is how an iota block spells the second constant onward.
func contentTypeConstantsIn(parsed []parsedSource, kind string) map[string]bool {
	named := map[string]bool{}
	for _, source := range parsed {
		for _, declaration := range source.file.Decls {
			constants, isConstant := declaration.(*ast.GenDecl)
			if !isConstant || constants.Tok != token.CONST {
				continue
			}
			carried := ""
			for _, specification := range constants.Specs {
				value, isValue := specification.(*ast.ValueSpec)
				if !isValue {
					continue
				}
				if value.Type != nil {
					carried = ""
					for _, mentioned := range identifiersNamedIn(value.Type) {
						carried = mentioned
					}
				}
				if carried != kind {
					continue
				}
				for _, name := range value.Names {
					named[name.Name] = true
				}
			}
		}
	}
	return named
}

// The doors of one scan: every name a whole message becomes octets under, and which of them
// serialize anything else as well.
type seamWireDoors struct {
	names   map[string]bool
	generic map[string]bool
}

// seamWireDoorsIn derives the doors from the one name this package sanctions.
//
// THE DOOR WAS A NAME, AND THAT IS THE REGRESSION THIS FUNCTION EXISTS TO UNDO. MarshalMLSMessage
// is five lines of framing.go -- a nil check and syntax.Marshal -- so a production forge that
// calls the wrappee instead emits BYTE IDENTICAL wire octets, and a gate anchored on the wrapper's
// spelling reports the package clean. Measured, not argued: thirteen production forges compiled
// into this package, nine caught, the no-carrier negative correctly not caught, and three survived
// -- syntax.Marshal called directly, the same over a carrier, and (*MLSMessage).MarshalMLS. All
// three spell the door type in their bodies, which is the reading the previous round dropped.
//
// So the chain is WALKED, and the edge it is walked along is the answer rather than the call. A
// wrapper hands its own result to the frame beneath it, so the edge is read off RETURN statements
// and followed only into a frame that answers octets itself: MarshalMLSMessage returns
// syntax.Marshal(message), Marshal returns MarshalLimit(v, MaxVectorLength), and MarshalLimit
// returns a local, which is where the chain stops. Walking every CALLEE instead was measured and
// is what a reader would reach for first: it takes in fmt.Errorf off MarshalMLS's three error
// paths and every leaf writer under it -- eleven names -- and a door class holding Errorf reads
// this whole package as reaching the wire.
//
// THE LAST FRAME IS THE ENCODE METHOD THE VALUE CARRIES, and the return walk cannot reach it.
// MarshalLimit does not answer v.MarshalMLS(w); it calls it for effect and hands back the writer's
// octets. So it is derived from the other end: a name a frame OF THE CHAIN calls that the door
// TYPE declares as a method of its own. MLSMessage declares MarshalMLS, UnmarshalMLS and
// populatedArms, and exactly one of the three is named by a frame of the chain.
//
// GENERIC AND SPECIFIC, because a call site means different things by the two. MarshalMLSMessage
// is handed the door type and nothing else, so a body that names it has put a whole message on the
// wire and that is the end of it. Marshal, MarshalLimit and MarshalMLS serialize every wire type
// this package has -- one takes a Marshaler, and fourteen declarations here carry the name
// MarshalMLS -- so a body naming one of those has sent a WHOLE MESSAGE only if it names a whole
// message too. That split is derived and not chosen: a door is generic when some declaration
// carrying its name is handed something other than the door type. Reading it the other way is what
// would ban marshalBoundedComposition and every fragment serializer beside it.
func seamWireDoorsIn(candidates []seamCandidate) seamWireDoors {
	answersOctets := map[string]bool{}
	methodsOfTheDoorType := map[string]bool{}
	for _, candidate := range candidates {
		if candidate.answersOctets {
			answersOctets[candidate.bare] = true
		}
		if candidate.receiver == seamWireDoorType {
			methodsOfTheDoorType[candidate.bare] = true
		}
	}
	// the wrappee chain, from the sanctioned name down
	chain := map[string]bool{seamWireDoor: true}
	for grew := true; grew; {
		grew = false
		for _, candidate := range candidates {
			if !chain[candidate.bare] {
				continue
			}
			for answered := range candidate.answersFrom {
				if answersOctets[answered] && !chain[answered] {
					chain[answered] = true
					grew = true
				}
			}
		}
	}
	doors := seamWireDoors{names: map[string]bool{}, generic: map[string]bool{}}
	maps.Copy(doors.names, chain)
	// and the encode method the innermost frame drives, which answers no octets and so is reached
	// from the door type's own method set rather than along the chain.
	for _, candidate := range candidates {
		if !chain[candidate.bare] {
			continue
		}
		for named := range candidate.names {
			if methodsOfTheDoorType[named] {
				doors.names[named] = true
			}
		}
	}
	for _, candidate := range candidates {
		if doors.names[candidate.bare] && !candidate.takesTheDoorType {
			doors.generic[candidate.bare] = true
		}
	}
	return doors
}

// seamNamesAWireDoor answers whether one body sends a WHOLE MESSAGE through a door.
//
// The specific door is enough on its own; a generic one needs the message named beside it. Both
// halves are load bearing and each was measured: without the generic half, a forge answering
// syntax.Marshal over an &MLSMessage{} it just assembled reads as clean, and without the specific
// half, a forge that hands MarshalMLSMessage a message its caller assembled does.
func seamNamesAWireDoor(candidate seamCandidate, doors seamWireDoors) bool {
	for named := range candidate.names {
		if !doors.names[named] {
			continue
		}
		if !doors.generic[named] || candidate.names[seamWireDoorType] {
			return true
		}
	}
	return false
}

// seamsReachingTheWireDoor answers which declarations put a whole message on the wire, DIRECTLY
// or through another declaration of the scan that does.
//
// Transitive rather than one hop, because the pair this file declares is a wrapper and a body:
// the wrapper names no door at all, and a rule reading one hop would report the seam that does
// the work and call the seam beside it clean.
//
// TWO THINGS THIS WALK USED TO DO ARE GONE, and each of them was a hole rather than a nicety.
//
// It was keyed on the BARE name, so one verdict was shared by every declaration spelled that way.
// (*MLSMessage).UnmarshalMLS mentions its own type, so the whole UnmarshalMLS family of this
// package was marked as reaching the wire on the strength of one member's body. A verdict is a
// property of a declaration, so it is keyed on the declaration.
//
// And an edge was followed only when the name it was written as belonged to ONE declaration of
// the scan. MarshalMLS and UnmarshalMLS are carried by fourteen declarations here, so that
// condition was doing real work -- but the work it was doing was hiding the bare-name merge
// above: with the merge gone, no MarshalMLS or UnmarshalMLS of this package reaches the door at
// all, and the near miss the condition was protecting (marshalPrivateMessageContentWithPadding,
// production source, genuinely handed the authenticators, serializing a FRAGMENT) stays out on
// its own merits. What the condition COST was measured: a forge plus a two line helper whose bare
// name any second declaration also carried shipped with this gate reporting clean. So an edge is
// now followed into every declaration carrying the name, and a helper renamed to collide is no
// longer an exit.
//
// The remaining approximation, stated because a gate nobody knows the edge of is worse than none:
// an edge is followed by NAME and not by resolved callee, so a declaration naming a method of
// some other type is read as reaching whatever any declaration spelled that way reaches. That
// direction over-reports, which fails loudly; the direction that under-reports is the one this
// package has paid for.
func seamsReachingTheWireDoor(candidates []seamCandidate, doors seamWireDoors) map[string]bool {
	byBareName := map[string][]int{}
	for index, candidate := range candidates {
		byBareName[candidate.bare] = append(byBareName[candidate.bare], index)
	}
	reaches := map[string]bool{}
	for grew := true; grew; {
		grew = false
		for _, candidate := range candidates {
			if reaches[candidate.name] {
				continue
			}
			if candidate.answersTheDoorType || seamNamesAWireDoor(candidate, doors) {
				reaches[candidate.name] = true
				grew = true
				continue
			}
			for named := range candidate.calls {
				followed := false
				for _, index := range byBareName[named] {
					if reaches[candidates[index].name] {
						followed = true
						break
					}
				}
				if followed {
					reaches[candidate.name] = true
					grew = true
					break
				}
			}
		}
	}
	return reaches
}

// constructionBypassSeamsIn is the class: a declaration handed the authenticators by its caller
// that reaches the wire door with them.
//
// The honest limit, recorded because a gate nobody knows the edge of is worse than none. A seam
// that answered a *PrivateMessage instead of an *MLSMessage or its octets is outside this
// reading, as is one that took its forged tag as a bare []byte rather than as the structure that
// carries it. Both are further from the shape the validation forge asked for than the plan's own
// draft was, and neither is what this gate was measured against: what it is measured against is
// the plan's file, moved into the package proper with a caller, and the three receiver position
// forges that passed the version of this gate that named the position.
func constructionBypassSeamsIn(parsed []parsedSource) []seamCandidate {
	candidates := seamCandidatesIn(parsed)
	reaches := seamsReachingTheWireDoor(candidates, seamWireDoorsIn(candidates))
	seams := []seamCandidate{}
	for _, candidate := range candidates {
		if candidate.forgeable && reaches[candidate.name] {
			seams = append(seams, candidate)
		}
	}
	slices.SortFunc(seams, func(a seamCandidate, b seamCandidate) int {
		return strings.Compare(a.name, b.name)
	})
	return seams
}

// seamNamesOf is what the assertions compare, so a report and a comparison cannot spell the same
// declaration two ways.
func seamNamesOf(seams []seamCandidate) []string {
	names := []string{}
	for _, seam := range seams {
		names = append(names, seam.name)
	}
	return names
}

// A package holding one of each shape, so a matcher that stopped matching fails here rather than
// reporting this package clean.
//
// Every negative is a shape this package's PRODUCTION source actually has: a fragment serializer
// handed the authenticators as a parameter (marshalPrivateMessageContentWithPadding), the same
// handed them as a RECEIVER ((*FramedContentAuthData).MarshalMLS), a sender that computes its own
// and names the type in its body (SignAuthenticatedContent), and a decoder that answers a whole
// message while being handed no authenticators at all (ParseMLSMessage). A rule that swept any of
// them in would ban the send path rather than the forge.
//
// It declares THE DOOR CHAIN as well, which the version of this control that read the wrapper's
// name did not need. MarshalMLSMessage, the syntax.Marshal it answers, the MarshalLimit that
// answers, and the encode method that one drives are transcribed from framing.go and
// syntax/marshal.go, so the derivation has real frames to walk instead of a name to look up. Each
// of the four is also a NEGATIVE in its own right: a door is not a bypass of itself, and a
// derivation that swept the chain in would ban this package's own encoder.
//
// It declares TYPES as well as functions, which the version of this control that read one
// identifier did not need. Three of them are production's own shape transcribed --
// AuthenticatedContent holds the auth data, PublicMessage holds it, MLSMessage holds a
// PublicMessage -- so the closure the handed-over half derives is exercised at one hop and at
// two, and the fourth carries none at any depth so the closure has something to leave out.
const constructionBypassSeamControl = `package control

// the authenticators themselves, and the three types production really puts them inside. A seam
// over any of these is handed exactly what a seam over the first is.
type FramedContentAuthData struct {
	Signature []byte
}

type AuthenticatedContent struct {
	Content FramedContent
	Auth    FramedContentAuthData
}

type PublicMessage struct {
	Content FramedContent
	Auth    FramedContentAuthData
}

type MLSMessage struct {
	PublicMessage *PublicMessage
}

// carrying none of them at any depth, which is what says the closure closes.
type FramedContent struct {
	GroupId []byte
}

// the seam shape: the caller chooses the authenticators and the answer is what the wire carries.
func forgesTheAuthenticatorsOntoTheWire(auth *FramedContentAuthData) ([]byte, error) {
	message := &MLSMessage{}
	return MarshalMLSMessage(message)
}

// the same, one call away. Without this half the rule reads the body of a two declaration seam
// and reports the wrapper beside it clean.
func forgesThroughAWrapper(auth *FramedContentAuthData) ([]byte, error) {
	return forgesTheAuthenticatorsOntoTheWire(auth)
}

// a METHOD with the seam shape. Both real seams are methods on *Group, so a matcher reading only
// free functions would report this package clean while shipping them.
func (self *Group) forgesAsAMethod(auth *FramedContentAuthData) ([]byte, error) {
	return MarshalMLSMessage(&MLSMessage{})
}

// handed the authenticators and serializing a FRAGMENT, which is
// marshalPrivateMessageContentWithPadding's shape and production's own.
func serializesAFragment(auth *FramedContentAuthData) ([]byte, error) {
	return auth.Signature, nil
}

// reaching the wire door while computing its own authenticators, which is every honest sender.
// The type is named in the BODY, so this is what says the rule reads parameters and not mentions.
func sealsWhatItSignedItself(signer SignaturePrivateKey) ([]byte, error) {
	auth := FramedContentAuthData{Signature: signer}
	_ = auth
	return MarshalMLSMessage(&MLSMessage{})
}

// A POSITIVE, and it was written here as a negative. Not one octet of it has changed: what
// changed is the rule that reads it, and this member is where that shows.
//
// It was named "the codec OF the authenticators, reaching the wire door with them as its
// RECEIVER" and held up as what says the rule reads parameters and not receivers -- and the
// second half of that sentence is the defect, not a property. It is handed the authenticators
// through the position its caller fills, and it reaches the door with them; a codec of the
// authenticators is (*FramedContentAuthData).MarshalMLS below, which serializes a FRAGMENT and
// never reaches the door at all. Its parameter was ALSO retuned once, from *MLSMessage to
// *FramedContent, so that it would go on reading as a negative once *MLSMessage became a carrier.
// The parameter is left exactly as that commit left it, because with the receiver read this
// member is a positive either way and restoring it would only make it a positive twice over --
// what it is for now is the receiver alone.
func (self *FramedContentAuthData) marshalsAMessage(content *FramedContent) error {
	_, err := MarshalMLSMessage(&MLSMessage{})
	return err
}

// the seam written over the type that CARRIES the authenticators rather than over the
// authenticators themselves. Identical power -- the caller fills in Auth and this puts it on the
// wire -- and the rule that anchored on one identifier read it as clean while failing the two real
// seams beside it.
func forgesOverACarrier(authContent *AuthenticatedContent) ([]byte, error) {
	return MarshalMLSMessage(&MLSMessage{})
}

// the same TWO hops out, since an MLSMessage carries a PublicMessage which carries the auth data.
// Without this member a closure that took one hop and stopped would pass.
func forgesOverACarrierTwoHopsOut(message *MLSMessage) ([]byte, error) {
	return MarshalMLSMessage(message)
}

// reaching the wire door over a type that carries no authenticators at any depth. It is what says
// the closure is a closure rather than "every type this package declares", and a rule that swept
// it in would ban every encoder that takes a content.
func sealsOverSomethingThatCarriesNone(content *FramedContent) ([]byte, error) {
	return MarshalMLSMessage(&MLSMessage{})
}

// naming neither the door nor the authenticators, so the fixed point has something to leave out.
func touchesNeither(count int) int {
	return count + 1
}

// the seam whose authenticators arrive through the RECEIVER. It is forgesOverACarrier with the
// carrier moved one position left, and the gate that read the position out of a list called this
// one clean while failing the one beside it.
func (self *AuthenticatedContent) forgesFromItsReceiver() ([]byte, error) {
	message := &MLSMessage{PublicMessage: &PublicMessage{Auth: self.Auth}}
	return MarshalMLSMessage(message)
}

// the same over the SEED of the closure rather than over a carrier one hop out, so the receiver
// half is exercised at the root as well as inside the derivation.
func (self *FramedContentAuthData) forgesItselfOntoTheWire() ([]byte, error) {
	message := &MLSMessage{PublicMessage: &PublicMessage{Auth: *self}}
	return MarshalMLSMessage(message)
}

// handed the authenticators through its RECEIVER and serializing a fragment, which is
// (*FramedContentAuthData).MarshalMLS's shape and production's own. It is what says the receiver
// did not become "every method of a carrier": what keeps a codec out is the door it never
// reaches, and that was true of the parameter half before the receiver joined it.
func (self *FramedContentAuthData) serializesItselfAsAFragment() []byte {
	return self.Signature
}

// a seam answering a whole MESSAGE rather than its octets, so the door type is read where a
// message leaves a declaration. The caller marshalling on the forge's behalf is one frame up, and
// a rule that stopped at the octets would put the forge one step outside its own class.
func answersAMessageRatherThanOctets(auth *FramedContentAuthData) (*MLSMessage, error) {
	return &MLSMessage{PublicMessage: &PublicMessage{Auth: *auth}}, nil
}

// the same answer handed NO authenticators, which is ParseMLSMessage's shape and the one every
// peer runs on bytes off the network. Answering a message is what reaches the wire; being handed
// the authenticators is what makes it a forge, and the decoder is only ever the first.
func parsesOctetsIntoAMessage(encoded []byte) (*MLSMessage, error) {
	return &MLSMessage{}, nil
}

// a forge reaching the door only through a helper whose bare name a SECOND declaration of this
// package also carries. A walk that dropped an ambiguous edge reported this clean, which is a
// forge plus two lines.
func forgesThroughAnAmbiguouslyNamedHelper(auth *FramedContentAuthData) ([]byte, error) {
	_ = auth
	return putsItOnTheWire()
}

func putsItOnTheWire() ([]byte, error) {
	return MarshalMLSMessage(&MLSMessage{})
}

// the second declaration of that bare name, which is the whole of what made the edge ambiguous.
// It reaches nothing.
//
// Its RECEIVER carries the authenticators, and that is the second thing this member is for. A walk
// that kept one verdict per BARE name -- which this one did, and which is what the ambiguous edge
// condition was really covering for -- hands this declaration the verdict earned by the free
// function above, and a declaration handed the authenticators that reaches the door is a seam. So
// under that reading this is a positive, and it is a negative here: a verdict belongs to a
// declaration and not to a spelling.
func (self *AuthenticatedContent) putsItOnTheWire() {
}

// ---------------------------------------------------------------------------
// the door chain, transcribed, and the three forges that walked under it
// ---------------------------------------------------------------------------

// the octet sink and the encode half of the method set every wire type implements, which are the
// two things the chain's innermost frame is written in terms of.
type Writer struct {
	buffer []byte
}

type Reader struct {
	buffer []byte
}

type Marshaler interface {
	MarshalMLS(w *Writer) error
}

func (self *Writer) Bytes() ([]byte, error) {
	return self.buffer, nil
}

// THE DOOR ITSELF, five lines, exactly as framing.go writes it: a refusal and a delegation. It is
// a NEGATIVE, and it is the one negative no derivation could make a positive -- a declaration
// handed a whole message that answers the wrappee over it and names no message of its own is this
// declaration, line for line.
func MarshalMLSMessage(message *MLSMessage) ([]byte, error) {
	if message == nil {
		return nil, nil
	}
	return Marshal(message)
}

// the wrappee, and the frame under IT. Neither is handed the door type -- one takes a Marshaler --
// which is what makes both of them generic doors rather than doors of the message.
func Marshal(v Marshaler) ([]byte, error) {
	return MarshalLimit(v, 0)
}

func MarshalLimit(v Marshaler, maxVectorLength int) ([]byte, error) {
	w := &Writer{}
	marshalErr := v.MarshalMLS(w)
	bs, writerErr := w.Bytes()
	if marshalErr != nil || writerErr != nil {
		return nil, marshalErr
	}
	return bs, nil
}

// the encode method the frame above drives, which is production's own and a NEGATIVE. Its
// receiver is the door type, so it is handed the authenticators; it names a door; and it names no
// message of its own, which is the whole of what keeps this package's encoder out of its own gate.
func (self *MLSMessage) MarshalMLS(w *Writer) error {
	var arm Marshaler = self.PublicMessage
	return arm.MarshalMLS(w)
}

// THE SECOND DECLARATION OF THE ENCODE METHOD, and the whole of what makes that name generic.
// Every wire type of this package carries one -- fourteen declarations here -- so a body that
// names MarshalMLS has serialized SOMETHING, and only a body that names a message beside it has
// serialized a message. It is production's own near miss twice over: handed the authenticators
// through its receiver, serializing a FRAGMENT, reaching no door, and a NEGATIVE here.
func (self *FramedContentAuthData) MarshalMLS(w *Writer) error {
	w.buffer = append(w.buffer, self.Signature...)
	return nil
}

// naming the door TYPE while CONSUMING octets rather than producing any, which is
// (*MLSMessage).UnmarshalMLS's shape and production's own. It is what says the restored reading is
// a door read and not a mention read: reading the mention alone swept every codec of this package
// in the moment the receiver joined the handed-over half, and that cost is what the door was
// traded away to pay.
func (self *MLSMessage) decodesItselfFromTheWire(r *Reader) error {
	decoded := MLSMessage{}
	_ = decoded
	return nil
}

// handed the authenticators and naming the wrappee over a FRAGMENT, which is
// marshalBoundedComposition's shape and production's own. Naming a generic serializer is not
// reaching the wire; naming one over a whole message is, and this is what says the two are told
// apart by the message rather than by the serializer.
func serializesAFragmentThroughTheWrappee(auth *FramedContentAuthData) ([]byte, error) {
	return Marshal(auth)
}

// THE FIRST OF THE THREE SURVIVORS. It never names MarshalMLSMessage, so the gate anchored on that
// spelling read it as clean -- and syntax.Marshal over an assembled MLSMessage is byte for byte
// what MarshalMLSMessage emits.
func forgesUnderTheWrapper(auth *FramedContentAuthData) ([]byte, error) {
	message := &MLSMessage{PublicMessage: &PublicMessage{Auth: *auth}}
	return Marshal(message)
}

// THE SECOND, the same one position left. The receiver derivation put the carrier inside the
// handed-over half and the narrowed door read let it straight back out, which is what "one axis
// traded for another" means in practice.
func (self *AuthenticatedContent) forgesUnderTheWrapperFromItsReceiver() ([]byte, error) {
	message := &MLSMessage{PublicMessage: &PublicMessage{Auth: self.Auth}}
	return Marshal(message)
}

// THE THIRD, one frame further down again: the encode method the wrappee drives. It answers no
// octets at all, so the caller takes them out of the writer, and a door class derived from what a
// declaration ANSWERS would stop one frame short of it.
func forgesThroughTheEncodeMethod(auth *FramedContentAuthData) ([]byte, error) {
	message := &MLSMessage{PublicMessage: &PublicMessage{Auth: *auth}}
	w := &Writer{}
	if err := message.MarshalMLS(w); err != nil {
		return nil, err
	}
	return w.Bytes()
}
`

// The other root's shape, written the way a package outside mls has to write it: all three
// names are exported, so a forge assembled in connect/message reaches them through a qualified
// selector and the identifier this matcher reads is that selector's tail.
const constructionBypassSeamInTheOtherRootControl = `package message

import "github.com/urnetwork/connect/mls"

func forgesFromAnotherPackage(auth *mls.FramedContentAuthData) ([]byte, error) {
	return mls.MarshalMLSMessage(&mls.MLSMessage{})
}
`

// The other root again, this time over a type of THIS root that CARRIES the authenticators
// rather than over the authenticators themselves. It is the shape the scope widening and the
// anchor derivation only catch together: the closure is read off the same scan the candidates
// are, so mls's own type declarations are what make a qualified *mls.AuthenticatedContent read as
// a parameter handed the authenticators.
const constructionBypassSeamCarrierInTheOtherRootControl = `package message

import "github.com/urnetwork/connect/mls"

func forgesOverACarrierFromAnotherPackage(authContent *mls.AuthenticatedContent) ([]byte, error) {
	return mls.MarshalMLSMessage(&mls.MLSMessage{})
}
`

// TestTheConstructionBypassSeamGateDerivesTheTypesThatCarryTheAuthenticators is the anchor half's
// own control, and it is here because the anchor was the half this gate got wrong.
//
// A parameter hands a seam the authenticators when its type CARRIES them. Reading the identifier
// instead was measured against a controlled A/B -- one file, one unexported caller, only the
// parameter type differing -- where the *AuthenticatedContent version passed five gates the two
// real seams failed. So the closure is asserted at one hop and at two, and it is asserted to
// CLOSE: a type carrying none of them at any depth stays out, or the rule would ban every encoder
// that takes a content.
func TestTheConstructionBypassSeamGateDerivesTheTypesThatCarryTheAuthenticators(t *testing.T) {
	carriers := seamAuthenticatorCarriersIn([]parsedSource{
		mustParseText(t, "seam_control.go", constructionBypassSeamControl)})
	want := []string{"AuthenticatedContent", "FramedContentAuthData", "MLSMessage", "PublicMessage"}
	if got := slices.Sorted(maps.Keys(carriers)); !slices.Equal(got, want) {
		t.Fatalf("the closure read %v out of the control, want %v; a carrier it misses is a seam of identical power it would ship, and one it invents bans the send path",
			got, want)
	}

	// and over the real scan, where the two things that matter are that the closure grows past
	// its seed and that what it grows into is production's own
	scan := mustScanSources(t, forbiddenScanRoots)
	parsed := []parsedSource{}
	for _, path := range slices.Sorted(maps.Keys(scan.sourceTexts)) {
		parsed = append(parsed, mustParseText(t, path, scan.sourceTexts[path]))
	}
	declared := packageLevelDeclarations(t, ".")
	real := seamAuthenticatorCarriersIn(parsed)
	for _, carrier := range []string{"AuthenticatedContent", "PublicMessage", "MLSMessage"} {
		if !real[carrier] {
			t.Errorf("%s is not read as carrying the authenticators, and framing.go declares it with a FramedContentAuthData inside; the anchor has gone back to being a spelling",
				carrier)
		}
		file, isDeclared := declared[carrier]
		if !isDeclared {
			t.Errorf("this package declares no %s, so the carrier this closure is pinned against has moved", carrier)
			continue
		}
		if strings.HasSuffix(file, "_test.go") {
			t.Errorf("%s is declared in %s, so the carriers this closure is pinned against are no longer production's own",
				carrier, file)
		}
	}

	// the two halves together: a forge assembled in the OTHER root over a carrier of this one.
	// Neither widening catches it alone -- the scope puts the file in front of the rule and the
	// closure is what says its parameter is the authenticators.
	seams := constructionBypassSeamsIn(append(slices.Clone(parsed),
		mustParseText(t, "../message/a_carrier_forge.go",
			constructionBypassSeamCarrierInTheOtherRootControl)))
	if !slices.Contains(seamNamesOf(seams), "forgesOverACarrierFromAnotherPackage") {
		t.Errorf("the matcher read %v and not the forge written in the other root over an *mls.AuthenticatedContent; the closure is not reaching across the scan it is derived from",
			seamNamesOf(seams))
	}
}

// TestTheConstructionBypassSeamGateDerivesTheDoorBeneathTheWrapper is the door half's own
// control, and it is here because the door is the half this gate got wrong THIS round.
//
// seamWireDoor was read as a single name and MarshalMLSMessage is a five line wrapper, so three
// production forges that call what it delegates to emitted byte identical octets and were reported
// clean -- measured over thirteen forges, nine caught and three through. The chain is derived here
// instead, and it is asserted to reach all three frames: the sanctioned name, the wrappee it
// answers, and the encode method that wrappee drives.
//
// The GENERIC half is asserted with it, because a door class alone would be the old defect
// inverted. Marshal, MarshalLimit and MarshalMLS serialize every wire type this package has, so a
// door read that stopped at "names one of these" would ban marshalBoundedComposition,
// (*FramedContentAuthData).MarshalMLS and every fragment serializer beside them -- which is a gate
// that fails on correct source, and that is as useless as a gate that passes on a forge.
func TestTheConstructionBypassSeamGateDerivesTheDoorBeneathTheWrapper(t *testing.T) {
	control := seamWireDoorsIn(seamCandidatesIn([]parsedSource{
		mustParseText(t, "seam_control.go", constructionBypassSeamControl)}))
	want := []string{"Marshal", "MarshalLimit", "MarshalMLS", seamWireDoor}
	slices.Sort(want)
	if got := slices.Sorted(maps.Keys(control.names)); !slices.Equal(got, want) {
		t.Fatalf("the chain read %v out of the control, want %v; a frame it misses is a forge of identical power it would ship, and one it invents bans this package's own encoder",
			got, want)
	}
	wantGeneric := []string{"Marshal", "MarshalLimit", "MarshalMLS"}
	if got := slices.Sorted(maps.Keys(control.generic)); !slices.Equal(got, wantGeneric) {
		t.Fatalf("the generic doors read %v out of the control, want %v; %s takes the door type and nothing else, and the other three serialize every wire type there is",
			got, wantGeneric, seamWireDoor)
	}

	// and over the real scan, where what matters is that the chain grew past its seed, that it
	// reached the encode method at the far end of it, and that every frame it found is one the go
	// tool compiles into a shipped binary rather than into the test one.
	scan := mustScanSources(t, forbiddenScanRoots)
	parsed := []parsedSource{}
	for _, path := range slices.Sorted(maps.Keys(scan.sourceTexts)) {
		parsed = append(parsed, mustParseText(t, path, scan.sourceTexts[path]))
	}
	candidates := seamCandidatesIn(parsed)
	doors := seamWireDoorsIn(candidates)
	if len(doors.names) < 2 {
		t.Fatalf("the chain read %v over the real scan; %s is a nil check and a delegation, so a door class holding its name alone is exactly the reading three production forges walked under",
			slices.Sorted(maps.Keys(doors.names)), seamWireDoor)
	}
	if doors.generic[seamWireDoor] {
		t.Errorf("%s is read as a generic door, and it takes the door type and nothing else; a body that names it has put a whole message on the wire and needs nothing said beside it",
			seamWireDoor)
	}
	if len(doors.generic) == 0 {
		t.Errorf("no derived door is read as generic, so the door type named in a body is doing no work; every frame under the wrapper serializes any wire type it is handed, and reading them as specific bans this package's own fragment serializers")
	}
	encoders := 0
	for _, candidate := range candidates {
		if candidate.receiver == seamWireDoorType && doors.names[candidate.bare] {
			encoders += 1
		}
	}
	if encoders == 0 {
		t.Errorf("no method of %s is read as a door. The innermost frame answers no octets -- it writes them into a writer it was handed -- so a chain walked along answers alone stops one frame short of it, and a forge calling it directly is the third of the three that survived",
			seamWireDoorType)
	}
	for _, door := range slices.Sorted(maps.Keys(doors.names)) {
		shipped := false
		for _, candidate := range candidates {
			if candidate.bare == door && !strings.HasSuffix(candidate.file, "_test.go") {
				shipped = true
			}
		}
		if !shipped {
			t.Errorf("%s is read as a door and the scan declares it in no production file; the chain is walking the test binary, and a door only the test binary has is one no shipped forge can reach",
				door)
		}
	}
	t.Logf("%d door(s) beneath %s, %d of them generic: %v", len(doors.names), seamWireDoor,
		len(doors.generic), slices.Sorted(maps.Keys(doors.names)))
}

// TestTheConstructionBypassSeamGateReadsAForgeThatGoesUnderTheWrapper is the regression itself,
// named rather than left to the roster above.
//
// Three production forges survived the gate at HEAD, and all three go UNDER MarshalMLSMessage:
// syntax.Marshal called directly over an assembled message, the same written over a carrier in
// receiver position, and (*MLSMessage).MarshalMLS driven by hand with the caller taking the octets
// out of the writer. Each emits what the sanctioned door emits, to the byte. The roster test above
// fails if any of them stops being read, and this one says WHICH shapes and why, so a reader who
// breaks it is sent to the finding rather than to a list.
func TestTheConstructionBypassSeamGateReadsAForgeThatGoesUnderTheWrapper(t *testing.T) {
	names := seamNamesOf(constructionBypassSeamsIn([]parsedSource{
		mustParseText(t, "seam_control.go", constructionBypassSeamControl)}))
	for _, forge := range []string{
		"forgesUnderTheWrapper",
		"AuthenticatedContent.forgesUnderTheWrapperFromItsReceiver",
		"forgesThroughTheEncodeMethod",
	} {
		if !slices.Contains(names, forge) {
			t.Errorf("the matcher read %v and not %s, which puts the same octets on the wire as %s does one frame up; the door is a spelling again",
				names, forge, seamWireDoor)
		}
	}
}

// TestTheConstructionBypassSeamGateReadsAForgeInTheOtherRoot measures the reach the widened
// scope was for.
//
// The gate walks both roots the guardrails walk, and the reason it has to is that every name
// the class is derived from is exported. That reasoning is worth nothing unqualified: the
// matcher reads identifiers, and a qualified name is a selector rather than an identifier at
// the position a reader might expect one. So the shape is run rather than argued about, and it
// is run against a file named under the other root so the report says what a reviewer would be
// sent to look at.
func TestTheConstructionBypassSeamGateReadsAForgeInTheOtherRoot(t *testing.T) {
	seams := constructionBypassSeamsIn([]parsedSource{
		mustParseText(t, "../message/a_forge.go", constructionBypassSeamInTheOtherRootControl)})
	if !slices.Equal(seamNamesOf(seams), []string{"forgesFromAnotherPackage"}) {
		t.Fatalf("the matcher read %v out of a forge written in the other root, want [forgesFromAnotherPackage]; the class is derived from three EXPORTED names and this is the only thing that says a qualified one is read at all",
			seamNamesOf(seams))
	}
	if strings.HasSuffix(seams[0].file, "_test.go") {
		t.Errorf("the forge was read as declared in %s, so it would be waved through as test source; the file half of the rule is not reading the path the scan handed it",
			seams[0].file)
	}
}

// TestTheConstructionBypassSeamGateReadsItsControl runs the matcher on a package known to hold
// every shape it must separate, so a rule narrowed by an edit fails here rather than reporting
// the real package clean -- which is the one outcome a gate must never produce by accident.
//
// (*FramedContentAuthData).marshalsAMessage moved from the negatives to this list, and its source
// was not touched to make it move. A control that starts reading differently as a class widens is
// reporting the widening, and retuning it back is how the receiver half came to be certified as
// an exclusion in the first place -- see the note on the member itself.
func TestTheConstructionBypassSeamGateReadsItsControl(t *testing.T) {
	seams := constructionBypassSeamsIn([]parsedSource{
		mustParseText(t, "seam_control.go", constructionBypassSeamControl)})
	want := []string{
		"AuthenticatedContent.forgesFromItsReceiver",
		"AuthenticatedContent.forgesUnderTheWrapperFromItsReceiver",
		"FramedContentAuthData.forgesItselfOntoTheWire",
		"FramedContentAuthData.marshalsAMessage",
		"Group.forgesAsAMethod",
		"answersAMessageRatherThanOctets",
		"forgesOverACarrier",
		"forgesOverACarrierTwoHopsOut",
		"forgesTheAuthenticatorsOntoTheWire",
		"forgesThroughAWrapper",
		"forgesThroughAnAmbiguouslyNamedHelper",
		"forgesThroughTheEncodeMethod",
		"forgesUnderTheWrapper",
	}
	if !slices.Equal(seamNamesOf(seams), want) {
		t.Fatalf("the matcher read %v out of the control, want %v; a positive it misses is a seam it would ship and a negative it sweeps in is the send path",
			seamNamesOf(seams), want)
	}
}

// seamReceiverForgeMethod is the method the test below writes over every carrier the closure
// derives. ONE name for all of them, because a method name is unique per receiver type and
// reusing it is what makes each forge differ from its neighbours in the receiver alone.
const seamReceiverForgeMethod = "reviewSealsFromItsReceiver"

// TestTheConstructionBypassSeamGateReadsAForgeOverEveryCarrierInReceiverPosition is the position
// half's own control, and it is here because the position is the half this gate got wrong.
//
// The receiver was read to build a report name and the class was set from function.Type.Params
// alone, so a construction bypass whose authenticators arrive through its RECEIVER was never in
// it. Deriving the carrier closure made that strictly worse rather than better: the unprotected
// receiver set had been the one type the old anchor spelled and became the whole closure, so
// every type newly protected as a parameter became newly unprotected as a receiver. Measured, not
// argued: (*FramedContentAuthData).reviewSealUnder in a production file -- handed the group,
// signing through its crypto, sealing under this epoch's real keys and answering
// MarshalMLSMessage -- returned a suite byte identical to a clean baseline, and so did the same
// shape written over *AuthenticatedContent and over *MLSMessage.
//
// So the carriers are DERIVED and a forge is written over EVERY one of them in receiver position,
// in a file named under production. A carrier the closure grows into tomorrow is inside this test
// by existing rather than by being added to a list, which is the whole difference between this
// and the table that certified the exclusion.
func TestTheConstructionBypassSeamGateReadsAForgeOverEveryCarrierInReceiverPosition(t *testing.T) {
	scan := mustScanSources(t, forbiddenScanRoots)
	parsed := []parsedSource{}
	for _, path := range slices.Sorted(maps.Keys(scan.sourceTexts)) {
		parsed = append(parsed, mustParseText(t, path, scan.sourceTexts[path]))
	}
	carriers := slices.Sorted(maps.Keys(seamAuthenticatorCarriersIn(parsed)))
	// the four production carriers at least, or the closure has stopped reading this package and
	// a forge per member of an empty class is no measurement at all.
	if len(carriers) < 4 {
		t.Fatalf("the closure read %v over the real scan; there is no carrier class here to write a receiver forge over",
			carriers)
	}
	forge := &strings.Builder{}
	forge.WriteString("package mls\n")
	want := []string{}
	for _, carrier := range carriers {
		forge.WriteString("\nfunc (self *" + carrier + ") " + seamReceiverForgeMethod +
			"() ([]byte, error) {\n\t_ = self\n\treturn MarshalMLSMessage(&MLSMessage{})\n}\n")
		want = append(want, carrier+"."+seamReceiverForgeMethod)
	}
	seams := constructionBypassSeamsIn(append(slices.Clone(parsed),
		mustParseText(t, "a_receiver_forge.go", forge.String())))
	shipped := map[string]bool{}
	for _, seam := range seams {
		if !strings.HasSuffix(seam.file, "_test.go") {
			shipped[seam.name] = true
		}
	}
	missed := []string{}
	for _, name := range want {
		if !shipped[name] {
			missed = append(missed, name)
		}
	}
	if len(missed) != 0 {
		t.Errorf("%d of %d carriers are outside the class in receiver position -- %v are handed the authenticators by their caller and answer %s from a production file, and the gate reports them clean; the position is being named again",
			len(missed), len(want), missed, seamWireDoor)
	}
}

// TestTheConstructionBypassSeamGateFollowsACallEdgeWrittenUnderAnAmbiguousName is the second half
// of the same finding, and the cheapest bypass this gate had: a forge plus two lines.
//
// The walk followed a call edge only when the name it was written as belonged to ONE declaration
// of the scan. That is not a rare shape here -- a selector tail carries no receiver, so every
// X.MarshalMLS and every X.UnmarshalMLS of this package is one bare name -- so the condition was
// dropping edges by the dozen, and a forge reaching the door through a helper renamed to collide
// with any of them shipped with the gate reporting clean. Confirmed, 7294 green.
//
// Both halves are asserted: the control holds the shape, and the ambiguity the condition turned
// on is counted off REAL source, because a condition that cost nothing would be an argument
// rather than a measurement.
func TestTheConstructionBypassSeamGateFollowsACallEdgeWrittenUnderAnAmbiguousName(t *testing.T) {
	control := []parsedSource{mustParseText(t, "seam_control.go", constructionBypassSeamControl)}
	helpers := 0
	for _, candidate := range seamCandidatesIn(control) {
		if candidate.bare == "putsItOnTheWire" {
			helpers += 1
		}
	}
	if helpers < 2 {
		t.Fatalf("the control declares %d declaration(s) named putsItOnTheWire; this edge is ambiguous only while two carry the name, so the control has stopped holding the shape it is here for",
			helpers)
	}
	forge := "forgesThroughAnAmbiguouslyNamedHelper"
	if names := seamNamesOf(constructionBypassSeamsIn(control)); !slices.Contains(names, forge) {
		t.Errorf("the matcher read %v and not %s, which is handed the authenticators and reaches %s through a helper two declarations spell the same way; an ambiguous edge is an exit again",
			names, forge, seamWireDoor)
	}

	scan := mustScanSources(t, forbiddenScanRoots)
	parsed := []parsedSource{}
	for _, path := range slices.Sorted(maps.Keys(scan.sourceTexts)) {
		parsed = append(parsed, mustParseText(t, path, scan.sourceTexts[path]))
	}
	declarations := map[string]int{}
	for _, candidate := range seamCandidatesIn(parsed) {
		declarations[candidate.bare] += 1
	}
	ambiguous, widest, carried := 0, "", 0
	for name, count := range declarations {
		if count < 2 {
			continue
		}
		ambiguous += 1
		if count > carried {
			widest, carried = name, count
		}
	}
	if ambiguous == 0 {
		t.Fatalf("no bare name in the scan is carried by two declarations, so the condition this test is about could never have dropped an edge and this scan is not reading the package")
	}
	t.Logf("%d bare name(s) carried by more than one declaration of the scan, the widest being %s at %d; every one of them named a helper the walk used to refuse to follow",
		ambiguous, widest, carried)
}

// TestEveryConstructionBypassSeamIsDeclaredInTestSource is the statement the compiler cannot
// make on its own.
//
// The compiler holds one direction already and holds it absolutely: production source naming an
// unexported declaration of a _test.go file does not build. What it does not hold is the seams
// STAYING there. Moving both into framing_group_seams.go the way p6's plan wrote it, with a
// production caller so nothing reads as uncalled, satisfies every other gate in this package --
// measured against TestNoStubShapesRemainInSource, which reports an uncalled declaration and has
// nothing to say about a called one.
//
// So the class is derived from what a construction bypass DOES: it is handed the authenticators a
// correct assembly would have computed, and it reaches the door this package's octets leave by. A
// seam written tomorrow under any spelling, in any file, in either of those two shapes, is in it.
func TestEveryConstructionBypassSeamIsDeclaredInTestSource(t *testing.T) {
	// both roots the guardrails walk, and not this directory alone. The seams themselves are
	// methods on *Group and could not be declared in another package, but the CLASS is not
	// about them: every name it is derived from -- FramedContentAuthData, MLSMessage,
	// MarshalMLSMessage -- is EXPORTED, so the same forge assembled in connect/message is the
	// same shape reached through a qualified name, and a selector tail is the identifier this
	// matcher already reads. Deriving a class and then scoping it to the directory its first
	// two members happen to sit in is the defect this project has paid for more than once, so
	// the scope is the walk every other package wide gate here uses.
	scan := mustScanSources(t, forbiddenScanRoots)
	// and the scope is OBSERVED rather than stated, because it was measured to be invisible
	// otherwise: narrowing this scan back to this directory alone passes every assertion below,
	// since connect/message declares no member of the class today. That is the same silence the
	// excuse reader's own widening was landed into and reported nothing about. A root that
	// contributed no source is a root this rule is not holding.
	for _, root := range forbiddenScanRoots {
		if scan.rootFileCounts[root] == 0 {
			t.Fatalf("the scan read no source under %q, and it is one of the roots the guardrails walk (%v); every name this class is derived from is exported, so a forge written under a root this gate steps over is one it reports clean",
				root, forbiddenScanRoots)
		}
	}
	parsed := []parsedSource{}
	for _, path := range slices.Sorted(maps.Keys(scan.sourceTexts)) {
		parsed = append(parsed, mustParseText(t, path, scan.sourceTexts[path]))
	}

	// the anchors, against the package's own declarations. A door that had been renamed leaves
	// the reachability half matching nothing, and a class of nothing reports exactly what a clean
	// package reports.
	declared := packageLevelDeclarations(t, ".")
	for _, anchor := range []string{seamWireDoor, seamWireDoorType, seamForgedAuthenticators} {
		file, isDeclared := declared[anchor]
		if !isDeclared {
			t.Fatalf("this package declares no %s, so the rule below is anchored on a name that has moved and reads nothing",
				anchor)
		}
		if strings.HasSuffix(file, "_test.go") {
			t.Fatalf("%s is declared in %s; the anchors are production's own, and one that had become the test binary's would make every seam its own excuse",
				anchor, file)
		}
	}

	candidates := seamCandidatesIn(parsed)
	// the near miss, pinned in both directions the way errNilLeafOccupancyTest is. It is
	// production source, it IS handed the authenticators, and it serializes a fragment rather
	// than reaching the wire -- so it is what says the parameter half of the rule reads real
	// source, and if it ever reached the door this package would hold a production forge.
	nearMiss := "marshalPrivateMessageContentWithPadding"
	held := false
	for _, candidate := range candidates {
		if candidate.name != nearMiss {
			continue
		}
		held = true
		if !candidate.forgeable {
			t.Errorf("%s is not read as handed the authenticators, and it takes an *%s; the parameter half of this rule is matching nothing in real source",
				nearMiss, seamForgedAuthenticators)
		}
		if strings.HasSuffix(candidate.file, "_test.go") {
			t.Errorf("%s has moved into the test binary (%s), so the near miss this rule is measured against is no longer production's own",
				nearMiss, candidate.file)
		}
	}
	if !held {
		t.Fatalf("the scan read no declaration of %s, so it is not reading this package's production source at all",
			nearMiss)
	}

	// the derived anchor, pinned against production's own source the way the near miss is. Both
	// of these are real seal entry points of this package and both take an *AuthenticatedContent
	// -- a type that CARRIES the authenticators without being spelled like them -- so a
	// parameter half anchored on the identifier reads both as taking nothing. That is exactly
	// what let a bypass of identical power, written over that type in a production file with a
	// production caller, ship while this gate reported the package clean.
	entryPoints := map[string]bool{"SealPublicMessage": false, "sealPrivateMessage": false}
	for _, candidate := range candidates {
		if _, isEntryPoint := entryPoints[candidate.name]; !isEntryPoint {
			continue
		}
		entryPoints[candidate.name] = true
		if !candidate.forgeable {
			t.Errorf("%s is not read as handed the authenticators, and it takes an *AuthenticatedContent; the parameter half is anchored on a spelling again, and a seam written over that type would ship",
				candidate.name)
		}
		if strings.HasSuffix(candidate.file, "_test.go") {
			t.Errorf("%s has moved into the test binary (%s), so the entry point this anchor is measured against is no longer production's own",
				candidate.name, candidate.file)
		}
	}
	for name, read := range entryPoints {
		if !read {
			t.Fatalf("the scan read no declaration of %s, so the seal entry points the anchor is pinned against are not the ones this package has",
				name)
		}
	}

	// the RECEIVER half's near miss, pinned in both directions the way the parameter half's is.
	// (*FramedContentAuthData).MarshalMLS is production's own codec of the authenticators: its
	// caller hands them over through the receiver, and it serializes a FRAGMENT rather than
	// reaching the wire. So it says the receiver is read in real source AND that reading it did
	// not ban the encoder every honest sender runs -- the two claims the version of this gate
	// that named the position could only ever make one of, by making the second one true and the
	// first one false.
	receiverNearMiss := "FramedContentAuthData.MarshalMLS"
	held = false
	for _, candidate := range candidates {
		if candidate.name != receiverNearMiss {
			continue
		}
		held = true
		if !candidate.forgeable {
			t.Errorf("%s is not read as handed the authenticators, and the authenticators are its RECEIVER; the position half of this rule is naming parameters again",
				receiverNearMiss)
		}
		if strings.HasSuffix(candidate.file, "_test.go") {
			t.Errorf("%s has moved into the test binary (%s), so the receiver near miss this rule is measured against is no longer production's own",
				receiverNearMiss, candidate.file)
		}
	}
	if !held {
		t.Fatalf("the scan read no declaration of %s, so the receiver half of this rule is matching nothing in real source",
			receiverNearMiss)
	}

	seams := constructionBypassSeamsIn(parsed)
	if len(seams) == 0 {
		t.Fatalf("the class is empty, and this file declares two of it; the rule is reading nothing rather than finding nothing")
	}
	shipped := []string{}
	for _, seam := range seams {
		if !strings.HasSuffix(seam.file, "_test.go") {
			shipped = append(shipped, seam.name+" ("+seam.file+")")
		}
	}
	if len(shipped) != 0 {
		t.Errorf("%v are handed their authenticators and reach %s from source the go tool compiles into every binary that imports this package; a construction bypass belongs in a _test.go file, where the compiler and not a comment is what keeps production out of it",
			shipped, seamWireDoor)
	}
	t.Logf("%d construction bypass seam(s), every one of them declared in test source: %v",
		len(seams), seamNamesOf(seams))
}
