// The inner MLS frame an application record's ct_body carries, its aad, and the two refusals an
// opener owes. MASTER section 8.4, RULED 2026-09-15.
//
// WHAT THIS FILE IS, IN ONE SENTENCE. MASTER section 8's record block has read "ct_body ... the
// MLS PrivateMessage payload" since revision 4 and its I5 paragraph "Sender authentication is
// MLS's, inside the ciphertext" for as long; this package padded the application plaintext and
// sealed it under a record key, using MLS as a key schedule and skipping the part of MLS that
// authenticates senders. Neither sentence changes. This file is the build catching up to them.
//
// WHY IT HAD TO. record_key[0] = HKDF-Expand(class_key, "sender/v1" | LP(leaf_index), 32) takes
// the class key EVERY MEMBER HOLDS and a LEAF NUMBER, and a leaf number is an input rather than a
// credential. sender_handle is the same shape and write_auth is a mac under a group wide key. So
// before this file, any member could derive any other member's record key at any position and
// seal a record the whole group opened as that member's -- which was not a defect in any one
// derivation but a property of the whole layer, because every input to it is group shared by
// construction. The signature inside a PrivateMessage is the one secret in the system that is
// not. TestOneMemberCannotForgeAMessageFromAnother is that sentence as a case.
//
// THE SCOPE IS THE BODY AND ONLY THE BODY. No wire field is added, removed, widened or
// reordered; format_version stays 0x02; octet_length(ct_body) is identical at every rung, because
// the rung is what is sealed and the frame sits INSIDE it; AAD_head, AAD_body and the write_auth
// preimage are byte for byte what they were. What changed is the plaintext inside one AEAD the
// server cannot read. The outer AEAD goes on answering the server -- which class, which window,
// which position, may this be erased -- and the inner frame answers members: who wrote this, and
// where.
//
// WHICH RECORDS CARRY ONE IS DERIVED AND NEVER PASSED IN. MASTER section 8.4.1's table is three
// rows and isApplicationRecord below is all three of them:
//
//	is_commit  attachment   inner
//	-----------------------------------------------------------------------------------
//	    1      any          the MLS COMMIT this record announces. It was ALREADY an
//	                        MLSMessage, which is why nobody caught the divergence by reading.
//	    0      NONE         an MLS APPLICATION message from Protect. THE WHOLE CHANGE.
//	    0      anything     no MLS frame at all -- a wrap, an epoch fan out, a completion
//	                        marker. Spec A section 5.11 (5) already said so.
//
// WHAT IT COSTS, MEASURED ON THIS TREE rather than taken from the ruling. A two member group, a
// thirty two octet group id, ciphersuite C5, aad_mls at thirty two octets: the frame's overhead
// over the application plaintext is a STEP FUNCTION with FOUR steps, because TWO nested varints
// widen -- varint(P) inside the ciphertext at 64 and at 16,384, and varint(C) around it, where
// C is about P + 82, at C = 16,384 and therefore at P = 16,300:
//
//	193   for       0 <= P <     64
//	194   for      64 <= P < 16,300
//	196   for  16,300 <= P < 16,384
//	198   for  16,384 <= P
//
// THIS COMMENT CARRIED THE THREE STEP FORM UNTIL 2026-09-17 and the 16,300..16,383 band was
// missing from it, which is ledger item 218 and MASTER section 8.4.4's own correction: the ladder
// was measured by WALKING and the step function beside it was DERIVED by hand, and only the
// derived one was wrong. It is load bearing now, because MASTER section 8.4.6's early size
// refusal is arithmetic over this function -- which is exactly why nothing in this package
// transcribes these four numbers: mls.FramedApplicationLength builds the frame's own structures
// and marshals them, and the four above are the expected ANSWER rather than an input.
//
// The usable application body per rung falls from 252/1,020/4,092/16,380/65,532 to
// 59/826/3,898/16,186/65,334. mlsframe_test.go publishes that ladder as a case and the query
// beside it, so the numbers in this comment are re-measured rather than re-asserted.
//
// THE 256 OCTET RUNG IS WHERE THE WHOLE BILL LANDS: 252 usable octets become 59, so a text
// longer than about 59 ASCII characters now pays the 1 KiB rung -- 1,040 stored octets where it
// paid 272, 3.8x, for a large fraction of real traffic. That is the price of the digest being 32
// octets rather than 104: carried VERBATIM, AAD_body's 104 octets leave the 256 rung carrying NO
// APPLICATION BODY AT ALL, not even a zero length one, which mlsframe_test.go measures rather
// than asserts. A rung that carries nothing turns every reaction into a 1 KiB record.
//
// AND THE ONE THING THAT STOPS WORKING, which is not a cost of the digest: A MEMBER CANNOT OPEN
// ITS OWN APPLICATION RECORD. The seal consumes a generation of this leaf's own sending ratchet,
// and mls derives no receiving ratchet for a leaf's own messages, so an open of one's own frame
// answers "mls: ratchet generation already consumed". That is inherent to MLS rather than to this
// file, and spec A section 5.2's "it does not make a working call stop working" is false of it --
// this package's fixtures had to move from one member to two to say so.
//
// RULED 2026-09-17, MASTER section 8.4.7 (1), ledger item 214: A DEVICE RENDERS ITS OWN SENT LINES
// FROM A COPY IT KEPT, never by decrypting the record it wrote, and an own record it holds no copy
// of is AUTHENTICATED by that very refusal, counted, and is not a failure. sdk had already built
// that answer before the ruling landed -- the ownSealed copy, PutSentRecord, the restore that reads
// it back and the counters beside it -- and the ruling RATIFIES it rather than commissioning it.
// The two refused options are recorded there so they are refused rather than rediscovered, and the
// second of them matters here: exempting a record at this member's own sender_handle from the inner
// open would re-open exactly the forgery MASTER section 8.4 closed, narrowed to self-attribution.
// Open item MG-4 is CLOSED.
package messagegroup

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"

	"github.com/urnetwork/connect/message"
	"github.com/urnetwork/connect/mls/syntax"
)

// The domain separation label of the inner frame's aad. Raw ascii, never length prefixed, which
// is every other label in this package's shape.
//
// THE LABEL IS THE VERSION, and that is a decision rather than a spelling. MASTER section 8.4.2 v2
// adds two terms to the preimage and no octet to the wire, so there is no format_version bump and
// no wire signal a reader could branch on -- deliberately, because format_version names the
// record's octet layout, which does not move, and its only reader is the SERVER, which must learn
// nothing about a change inside an AEAD it cannot open. What separates v1 from v2 is therefore this
// string: a v1 opener handed a v2 frame rebuilds v1's preimage, gets a different digest and refuses
// at R2. That is the fail-closed direction in BOTH directions, and it is also why ledger item 217
// exists -- the two versions do not interoperate and the flag day is the owner's.
const aadMlsLabel = "URmessage/v2/aad/mls"

// The width of aad_mls on the wire, which is the width of H rather than a number.
//
// It is DERIVED from sha256.Size and never written as 32, because MASTER section 8.4.6's early size
// refusal is arithmetic over it: a digest that changed width and a constant that did not would
// refuse a legal body or admit one the seal must then refuse late. It is the same at v1 and at v2,
// which is the whole reason v2 costs zero wire octets -- the 36 octets v2 adds are added to a
// PREIMAGE, and a preimage has no width on any wire.
const aadMlsBytes = sha256.Size

// aadMls is MASTER section 8.4.2 v2's authenticated_data:
//
//	aad_mls = H("URmessage/v2/aad/mls" | AAD_body | u32(generation) | head_commit)     32 octets
//
// The preimage is 160 octets and has exactly four terms, concatenated in this order with no
// separator, no padding and no framing: the 20 octet label, AAD_body's own 104 octets verbatim,
// the frame's generation as four BIG ENDIAN octets, and the full 32 octet HMAC output head_commit
// carries. u32 and not u64 because u32 is the width RFC 9420 section 6.3.2 gives the field, and a
// second width here is a preimage no MLS implementation reproduces.
//
// WHY THE GENERATION IS IN IT, stated as the attack it stops. The generation is NOT in the
// signature preimage: RFC 9420 section 6.1's FramedContentTBS is ProtocolVersion | WireFormat |
// FramedContent | GroupContext, and FramedContent carries the group id, the epoch, the sender, the
// authenticated_data, the content type and the content -- and no generation. The generation lives
// in SenderData, sealed under the epoch's GROUP SHARED sender_data_secret, so every member can
// write one; and section 9 derives every leaf's ratchet from the group shared encryption_secret, so
// every member can seal AT one. So a member who cannot forge Alice's signature can open Alice's
// frame, keep her FramedContent and her signature octets unchanged, and re-seal them at a
// generation of its own choosing. Under v1 that record passed R1 and R2 and the receiver obeyed the
// attacker's generation -- and an accepted frame COMMITS its generation, so Alice's own later frame
// at that generation was then refused for ever. Measured on this tree by
// TestAReEnvelopedGenerationIsRefusedAndTheVictimsRecordsSurvive: before the bind, three
// substitutes were accepted and seven of the victim's seven genuine records were dead.
//
// WHY THE HEAD IS IN IT is headCommit's own header, and the reason it is a KEYED commitment rather
// than a bare hash is there too.
//
// IT IS AAD_body AND NOT A NEW PREIMAGE, and that is the decision this function embodies.
// AAD_body already carries exactly the six fields that fix a record's identity and its position
// -- group_id, sender_handle, epoch, stream_index, the retention wire byte and eph_window -- so
// there is nothing a new preimage would add, one builder cannot drift from itself, and a field
// added to AAD_body later is bound here with no second edit for somebody to forget. It goes
// through message.AADBody and a BodyBinding rather than assembling the six fields again, which is
// guardrail G4 arriving here for free: a value with no hash within its reach cannot put
// body_hash inside the frame that body_hash is a hash of.
//
// IT IS HASHED AND NOT CARRIED VERBATIM, and the reason is a measurement rather than a taste.
// AAD_body is 104 octets, the frame sits inside the size rung, and those octets come out of the
// application body: verbatim, the 256 octet rung carries nothing at all. The digest costs 73
// octets a record against the verbatim column and buys the rung back.
//
// WHAT IT DEFENDS is re-enveloping. A member who cannot forge Alice's signature can still take a
// frame Alice signed and seal it into a DIFFERENT record: a different stream_index, which is a
// replay into a later conversational position and is indistinguishable from Alice saying it
// again, or a different retention_class, which is a DURABLE message dropped into EPH(1) so it
// self-destructs within the hour or an EPH one promoted to PERMANENT so it never does. Both
// attack what the product promises rather than what the ciphertext says, and both are a record
// whose inner aad names a position it is not in.
//
// WHAT IT CANNOT DEFEND, stated here because the complement is the part a reader has to be told,
// and the complement is FIVE things. IT WAS SIX UNTIL 2026-09-17 and the sixth was the head
// plaintext, which v2's fourth term binds; open item MG-6 and ledger item 204 are CLOSED by that
// term and the entry is struck from this list rather than left standing with a note.
//
// AAD_head is NOT bound and CANNOT be in either form: AAD_head contains body_hash = H(ct_body), and
// ct_body is sealed over the frame this aad is inside. That is MASTER section 8's construction
// order seen from the inside, and it is a cycle rather than an ordering -- unlike the head
// PLAINTEXT, which both sides hold at the right moment, which is exactly why one of the two could
// be bound and the other cannot. The five fields AAD_head carries and AAD_body does not --
// is_commit, size_bucket, expire_at, blob_id, H(server_attachment) -- are therefore still
// authenticated by the group alone, and is_commit is the one the SERVER acts on. Ledger open item
// 199, still open and unchanged in substance.
//
// IT TAKES NO alg_id, and that is this package's own gate rather than a simplification.
// TestEveryAadCallInEitherHalfPassesTheRecordAeadAlgId requires every AADBody call in either half
// of the record layer to pass RecordAeadAlgId itself, on the argument that a literal, an X-Wing
// identifier or an attachment identifier is an aad no second implementation reconstructs. A
// parameter here would have put the one call this file makes outside that rule.
func aadMls(binding message.BodyBinding, generation uint32, head [32]byte) ([32]byte, error) {
	aadBody, err := message.AADBody(RecordAeadAlgId, binding)
	if err != nil {
		return [32]byte{}, err
	}
	writer := syntax.NewWriter()
	writer.WriteRaw([]byte(aadMlsLabel))
	writer.WriteRaw(aadBody)
	// BIG ENDIAN, four octets, most significant first, written out rather than taken from a
	// helper for the reason every label in a preimage is transcribed: the width and the order
	// are the wire, and MASTER section 8.4.3's mutation (d) is a sealer that writes them the
	// other way round -- which produces a well formed digest that no peer computes.
	writer.WriteRaw([]byte{
		byte(generation >> 24), byte(generation >> 16), byte(generation >> 8), byte(generation),
	})
	// RAW, no length prefix, the full 32 octet HMAC output.
	writer.WriteRaw(head[:])
	preimage, err := writer.Bytes()
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(preimage), nil
}

// isApplicationRecord is MASTER section 8.4.1's predicate, computed in ONE place and read
// identically by the sealer and by the opener.
//
// It takes the ENCODED attachment rather than the *message.ServerAttachment the sealer holds,
// which is what makes the two sides the same sentence rather than two sentences that agree
// today. connect/message's encoder already collapses a nil attachment and an explicit
// AttachmentNone to NO BYTES AT ALL -- it has to, because MASTER section 8 requires the two to
// contribute the same LP(H(server_attachment)) -- and it already refuses an attachment whose tag
// and whose body disagree. So "the attachment is NONE" is "the encoding is empty", the opener
// reads that off a header field AAD_head has already authenticated, and neither side needs a
// second reading of the presence rule.
func isApplicationRecord(isCommit bool, serverAttachment []byte) bool {
	return !isCommit && len(serverAttachment) == 0
}

// frameBodyOnLoop answers the octets ct_body is sealed over: the inner MLS frame for an
// application record, and the caller's own body for every other kind.
//
// THE ORDER IS FORCED AND IS NOT A PREFERENCE. aad_mls is a digest of AAD_body and AAD_body
// carries stream_index, so there is no legal ordering in which the frame is built before the
// index is reserved. That is why this runs inside newRecordBuilderOnLoop, after Next, rather than
// in front of it where a reader's instinct puts it.
//
// A SECOND WRITE-ONCE RESOURCE IS CONSUMED HERE. The seal takes a generation of this leaf's MLS
// ratchet and persists group state whether or not the record is ever submitted, exactly as the
// reservation takes an index whether or not it is. A refused submit therefore leaves a legal gap
// in TWO sequences. Both are monotonic and both tolerate gaps; the bound is ledger open item 201
// and mls's MaxGenerationSkip is 1,024.
//
// IT GOES THROUGH ProtectBound AND NOT Protect, AND THE BUILDER IS WHY. MASTER section 8.4.2 v2
// puts u32(generation) inside aad_mls, and the seal chooses the generation INSIDE, from the sender
// ratchet -- so there is no value this function could compute and hand over. What it hands over is
// the BUILDER below, which mls calls with the generation it is about to spend, under one hold of
// the group's own lock, and which mls then PINS: if the generation consumed is not the one the
// builder was handed, the seal emits nothing. See (*mls.Group).ProtectBound for the four part race
// argument and for why the pin is a rule rather than an assertion.
//
// THE HEAD PLAINTEXT IS AN ARGUMENT NOW, and that is the whole of what the head bind cost this
// signature. sealRecordOnLoop has held headPlain since before the stream index was reserved, and
// the rung exists by the time this runs, so both inputs to head_commit are in hand -- which is the
// non-circularity headCommit's header states, read from the caller's side.
//
// The caller is the loop goroutine, which is what lets it touch self.handle at all.
func (self *GroupSession) frameBodyOnLoop(isCommit bool, serverAttachment []byte,
	binding message.BodyBinding, recordKey []byte, headPlain []byte, bodyPlain []byte) ([]byte, error) {

	if !isApplicationRecord(isCommit, serverAttachment) {
		return bodyPlain, nil
	}
	head := headCommit(recordKey, headPlain)
	inner, err := self.handle.ProtectBound(func(generation uint32) ([]byte, error) {
		aad, err := aadMls(binding, generation, head)
		if err != nil {
			return nil, err
		}
		return aad[:], nil
	}, bodyPlain)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRecordInnerFrame, err)
	}
	return inner, nil
}

// refuseFrameBindingsOnLoop is MASTER section 8.4.3's two refusals over one reading of a frame's
// sender leaf and its aad, written once and taken twice.
//
// NEITHER REFUSAL IMPLIES THE OTHER, and the whole reason both are written is that each is
// invisible from the other's side.
//
// R1, the SENDER binding, is what converts "someone in this group" into "Alice". R2 does not
// imply it: a member at leaf B can perfectly well call Protect with an aad_mls naming leaf A's
// handle -- R2 passes, the signature is B's, and the record then claims A while the frame says B.
// An opener without R1 has two answers to "who wrote this" and no rule for choosing, which is a
// forgery with extra steps.
//
// R2, the POSITION binding, is what makes the aad load-bearing rather than decorative, and R1
// does not imply it: R1 pins the writer and says nothing at all about the position, the class or
// the window the writer's frame was put into.
//
// BOTH REFUSE THE WHOLE RECORD. A record failing either is not rendered as a message from
// anybody -- not as a gap attributed to a sender, and not as spec A section 7.4's "malformed",
// which is a different condition about a body that opened.
//
// The caller is the loop goroutine.
// R2 IS NOW A FUNCTION OF THE GENERATION AND THE HEAD AS WELL AS THE POSITION, which is v2, and the
// caller is what supplies them: this function compares one digest against another and does not care
// which terms went into either. The position digest is built ONCE per reading by the caller, out of
// this record's own header, its own rung and the generation the reading answered, so the two
// readings cannot come to disagree about what "this record's aad" is.
func (self *GroupSession) refuseFrameBindingsOnLoop(header *message.RecordHeader,
	position [32]byte, senderLeaf uint32, aad []byte) error {

	// R1. The comparison goes through subtle for guardrail G8's reason and not because a
	// handle is secret: G8 bans the other spelling in a FILE rather than in a kind of
	// function, so every comparison of octets in this package goes one way.
	signed := SenderHandle(self.groupHandleKey, senderLeaf)
	if subtle.ConstantTimeCompare(signed[:], header.SenderHandle[:]) != 1 {
		return fmt.Errorf("%w: the frame was signed at leaf %d, whose handle is %x, and the record carries %x",
			ErrRecordSenderBinding, senderLeaf, signed, header.SenderHandle)
	}
	// R2, against the aad this record's OWN position produces.
	if subtle.ConstantTimeCompare(position[:], aad) != 1 {
		return fmt.Errorf("%w: the frame carries %x and this record's position is %x",
			ErrRecordPositionBinding, aad, position)
	}
	return nil
}

// unframeBodyOnLoop opens the inner frame and takes MASTER section 8.4.3's two refusals.
//
// THE TWO REFUSALS ARE TAKEN TWICE AND THAT IS THE POINT OF THIS FUNCTION'S SHAPE. Once on the
// frame's PRE-RATCHET reading -- peekInnerFrameSender, which opens only the sender data and reads
// the cleartext aad, and which moves nothing -- and once on the values Unprotect has authenticated.
// Only the second decides anything. The first exists because of what sits between them:
//
//	mls opens the frame, verifies the signature, and ERASES the message key of the generation
//	the frame came at. A refusal taken after that has already cost the frame's true sender its
//	own message.
//
// Measured on this tree rather than argued: with the pre-reading removed, a member lifts another
// member's genuine frame out of a record -- the record key is RecordKeyZero(class_key, leaf) and
// the class key is group shared, so every member can -- seals it into a record at a different
// stream_index, and the opener refuses it at R2 AFTER mls has erased the generation. The true
// sender's own record at that generation then answers "mls: ratchet generation already consumed"
// at that receiver forever. One ordinary record, at the attacker's own handle and its own index,
// per message the attacker wants deleted, chosen precisely.
// TestARecordRefusedAtTheInnerFrameMovesNoReceiverRatchet drives both refusals and is red without
// the pre-reading.
//
// WHY A PRE-READING IS NOT A WEAKER SECOND RULE. The two values it reads are the two values the
// signature covers: the leaf is the one mls builds its Sender from, and the aad is the cleartext
// authenticated_data the content AEAD is taken over, so a message that OPENS cannot disagree with
// its own peek -- mls's TestThePeekAgreesWithTheOpenOnEveryMessageThatOpens is that, swept over
// every boundary generation. The peek can therefore only ever refuse what the second reading would
// have refused, and the second reading is still written, still reached and still the answer.
//
// AND BOTH RUN BEFORE THE RECEIVER RATCHET COMMITS, which is openRecordOnLoop's own discipline
// one level out. A forged envelope at the true sender's next index would otherwise burn that index
// at every opener, so a refusal that moved that ladder would turn a forgery this file defeats into
// a denial it causes. That is the same sentence as the paragraph above, about the other of the two
// receiver ratchets a record passes through.
//
// R3 IS WHAT v2 TURNS FROM A DEFENCE INTO THE REFUSAL ITSELF, and it is the reason the pre-reading
// now has to answer THREE values. Under v1 the pre-reading was a defence against a denial channel
// and a correct opener could have been written without it. Under v2 the GENERATION is one of the
// values being bound, and OPENING a frame is what commits its generation at this receiver -- so an
// opener that checked R2 after the open would have implemented the check and kept the vulnerability
// whole. peekInnerFrameSender answers the leaf, the aad AND the generation out of one SenderData
// open, so all three are in hand before any ratchet is reached.
//
// AND ONE HALF OF THE SECOND READING IS PREDICTED TO DEFEND NOTHING. For the leaf and the aad the
// second reading is load bearing in the ordinary way. For the GENERATION it is not, and the reason
// is mechanical: the content AEAD's key is derived from the generation the sender data named, so a
// frame that OPENS AT ALL opened at exactly the generation the pre-reading read, and a disagreement
// between the two readings is unreachable through any octets. MASTER section 8.4.3 requires the
// implementing pass to DELETE the generation half of the second reading, run this package's suite
// and mls's, and say by name whether anything went red. It was done and NOTHING went red -- the
// measurement is in this package's OPENITEMS.md under MG-6. The clause is written anyway, because
// the alternative is an argument a reader has to reconstruct rather than a rule, and because its
// premise -- one SenderData open feeding both the pre-reading and the key derivation -- is a
// property of mls's implementation rather than of any document.
//
// THE HANDLE IS AN ARGUMENT AND NOT self.handle SINCE LEDGER ITEM 241, because the frame inside a
// record sealed at a prior epoch opens under THAT epoch's secret tree and no other: mls refuses a
// frame naming any epoch but the group's own. openRecordOnLoop chooses the handle by the record's
// epoch and hands it down; the sealing half one function up still reads self.handle, because a
// record is only ever sealed at the epoch the session is at.
//
// The caller is the loop goroutine.
func (self *GroupSession) unframeBodyOnLoop(handle GroupHandle, header *message.RecordHeader,
	recordKey []byte, headPlain []byte, bodyPlain []byte) ([]byte, error) {

	if !isApplicationRecord(header.IsCommit, header.ServerAttachment) {
		return bodyPlain, nil
	}
	// head_commit over THIS record's own rung and THIS record's own head plaintext, computed
	// once. The opener holds headPlain because openRecordOnLoop opened ct_head above this call,
	// which is the non-circularity headCommit's header states from the opening side.
	head := headCommit(recordKey, headPlain)
	binding := header.BodyBinding()
	peekLeaf, peekAad, peekGeneration, err := peekInnerFrameSender(handle, bodyPlain)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRecordInnerFrame, err)
	}
	// the aad this record's position, generation and head produce. It is built per reading
	// because the GENERATION is an input to it and each reading answers its own -- which is the
	// whole of what makes the second reading a second reading rather than a repetition.
	peekPosition, err := aadMls(binding, peekGeneration, head)
	if err != nil {
		return nil, err
	}
	if err := self.refuseFrameBindingsOnLoop(header, peekPosition, peekLeaf, peekAad); err != nil {
		return nil, err
	}
	aad, plaintext, senderLeaf, generation, err := handle.Unprotect(bodyPlain)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRecordInnerFrame, err)
	}
	position, err := aadMls(binding, generation, head)
	if err != nil {
		return nil, err
	}
	// and again, on what the signature covers. This is the reading that decides.
	if err := self.refuseFrameBindingsOnLoop(header, position, senderLeaf, aad); err != nil {
		return nil, err
	}
	return plaintext, nil
}
