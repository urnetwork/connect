package protocol_test

// The op-byte gate.
//
// Spec A §5.7 and Spec B §4.3.8 define the URmessage request authenticator as
//
//	req_auth = MAC(read_key[e], "URmessage/v1/req" ‖ LP(server_nonce)
//	                            ‖ u8(op) ‖ LP(canonical_request_bytes))
//
// with "op = the field number of the selected `oneof body` arm in
// MessageServerRequest, as a u8".
//
// That makes the protobuf field numbers of MessageServerRequest.body protocol
// constants inside a MAC rather than an implementation detail. A transposed arm
// number compiles, parses, and produces a message that looks entirely correct; it
// fails only as a MAC mismatch, which Spec B §4.5 answers with the deliberately
// non-specific REASON_REJECTED, on one operation, for one implementation. These
// tests exist to turn that into a test failure instead.
//
// Every assertion below enumerates the arms FROM THE COMPILED DESCRIPTOR, never
// from a hand-written list of arms. The hand-written part is only the mapping from
// an arm's name to the two facts the spec states about it — its op byte and whether
// it carries req_auth — and TestRequestArmSetMatchesSpecA57 asserts that mapping's
// key set EQUALS the descriptor's arm set. So a new arm added to message.proto
// fails here until somebody decides its authentication status, which is exactly the
// decision that must not be made by default.

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/urnetwork/connect/protocol"
)

// armAuth is what Spec A §5.7 / Spec B §4.3.8 say about one oneof arm of
// MessageServerRequest.body.
type armAuth struct {
	// op is the arm's field number, which IS the op byte. Held as an int rather
	// than a uint8 so that an out-of-range value is expressible here and is caught
	// by TestRequestArmOpBytesFitInAU8 rather than by a compile error, which would
	// hide the defect instead of naming it.
	op int
	// requiresReqAuth is true for the five arms Spec A §5.7 lists under
	// "Required on" — the only list §5.7 gives that this file transcribes verbatim.
	// It is false for ten arms, but §5.7 does not name ten: its "NOT used on" list
	// holds exactly five (HelloRequest, CreateGroupRequest, UnsubscribeRequest,
	// SubmitRequest, RecoveryFetchRequest) and never mentions rendezvous. The other
	// five exemptions — the rendezvous arms 20-24 — come from Spec B §4.3.8, which
	// is also the only source for the op bytes of any exempt arm; §5.7 gives op
	// bytes only for the five required ones. Cite the source you actually read:
	// this comment is the reader's map back to the normative text, and the whole
	// argument of this file is that two independent transcriptions of one fact are
	// a gift.
	requiresReqAuth bool
}

// specA57 transcribes the classification in full: both the arms that require
// req_auth and the arms that are exempt, each with its op byte. It is named for
// Spec A §5.7 because that is where the required set comes from, but only the
// first block below is §5.7's; the second is Spec B §4.3.8's.
//
// Spec A §5.7, verbatim — "Required on, with their op bytes":
//
//	FetchRequest (13), SubscribeRequest (14), GroupStatusRequest (16),
//	BlobGrantRequest (17), WrapFetchRequest (19).
//
// Spec B §4.3.8, "The arms that carry no req_auth, each for its own reason", which
// is the source of every exempt arm AND of every exempt arm's op byte (Spec A
// §5.7 lists only the first five of these, gives no numbers for them, and does not
// mention rendezvous at all):
//
//	HelloRequest (10), CreateGroupRequest (11), SubmitRequest (12),
//	UnsubscribeRequest (15), RecoveryFetchRequest (18),
//	and the five rendezvous arms — RendezvousRegisterRequest (20),
//	RendezvousOpenRequest (21), RendezvousDepositRequest (22),
//	RendezvousCollectRequest (23), RendezvousRetireRequest (24) — which name no
//	group and carry their own Ed25519 authenticator instead (Spec B §4.3.11).
//
// This map is a decision table, not an inventory: its KEY SET is asserted equal to
// the descriptor's arm set, so it cannot silently fall behind message.proto.
var specA57 = map[string]armAuth{
	"hello":               {op: 10, requiresReqAuth: false},
	"create_group":        {op: 11, requiresReqAuth: false},
	"submit":              {op: 12, requiresReqAuth: false},
	"fetch":               {op: 13, requiresReqAuth: true},
	"subscribe":           {op: 14, requiresReqAuth: true},
	"unsubscribe":         {op: 15, requiresReqAuth: false},
	"group_status":        {op: 16, requiresReqAuth: true},
	"blob_grant":          {op: 17, requiresReqAuth: true},
	"recovery_fetch":      {op: 18, requiresReqAuth: false},
	"wrap_fetch":          {op: 19, requiresReqAuth: true},
	"rendezvous_register": {op: 20, requiresReqAuth: false},
	"rendezvous_open":     {op: 21, requiresReqAuth: false},
	"rendezvous_deposit":  {op: 22, requiresReqAuth: false},
	"rendezvous_collect":  {op: 23, requiresReqAuth: false},
	"rendezvous_retire":   {op: 24, requiresReqAuth: false},
}

// spec438OpBytes is a SECOND, INDEPENDENT transcription of the same numbers, taken
// from the prose of Spec B §4.3.8 rather than from the proto block of §4.3. §4.3.8
// names each op byte inline — "FetchRequest (13)", "SubscribeRequest (14)",
// "GroupStatusRequest (16)", "BlobGrantRequest (17)", "WrapFetchRequest (19)" and,
// in its exemption list, "HelloRequest (10)", "CreateGroupRequest (11)",
// "SubmitRequest (12)", "UnsubscribeRequest (15)", "RecoveryFetchRequest (18)" —
// and that redundancy is the cross-check the spec offers. Keeping it separate from
// specA57 means a coordinated edit has to get the same number wrong in three places
// (the proto, specA57, and here) before it can ship.
var spec438OpBytes = map[string]int{
	"fetch":          13,
	"subscribe":      14,
	"group_status":   16,
	"blob_grant":     17,
	"wrap_fetch":     19,
	"hello":          10,
	"create_group":   11,
	"submit":         12,
	"unsubscribe":    15,
	"recovery_fetch": 18,
}

// specA57RequiredOps is the set of op BYTES §5.7 lists under "Required on",
// transcribed as numbers rather than as names, so that renaming an arm cannot
// quietly move a number in or out of the authenticated set.
var specA57RequiredOps = []int{13, 14, 16, 17, 19}

func bodyOneof(t *testing.T, m protoreflect.ProtoMessage) protoreflect.FieldDescriptors {
	t.Helper()
	d := m.ProtoReflect().Descriptor()
	od := d.Oneofs().ByName("body")
	if od == nil {
		t.Fatalf("%s has no `oneof body`; every URmessage envelope is defined around one (Spec B §4.3)", d.FullName())
	}
	return od.Fields()
}

func armNames(fields protoreflect.FieldDescriptors) []string {
	names := make([]string, 0, fields.Len())
	for i := 0; i < fields.Len(); i++ {
		names = append(names, string(fields.Get(i).Name()))
	}
	sort.Strings(names)
	return names
}

func armNumbers(fields protoreflect.FieldDescriptors) map[string]int {
	byName := make(map[string]int, fields.Len())
	for i := 0; i < fields.Len(); i++ {
		f := fields.Get(i)
		byName[string(f.Name())] = int(f.Number())
	}
	return byName
}

// TestRequestArmSetMatchesSpecA57 is the equality that makes the rest of this file
// mean something. A test that only checked the arms it already knew about would
// pass forever, however many arms were added to message.proto afterwards.
func TestRequestArmSetMatchesSpecA57(t *testing.T) {
	fields := bodyOneof(t, (*protocol.MessageServerRequest)(nil))
	got := armNames(fields)

	want := make([]string, 0, len(specA57))
	for name := range specA57 {
		want = append(want, name)
	}
	sort.Strings(want)

	inDescriptor := make(map[string]bool, len(got))
	for _, n := range got {
		inDescriptor[n] = true
	}
	for _, n := range want {
		if !inDescriptor[n] {
			t.Errorf("specA57 names arm %q, but MessageServerRequest.body has no such arm: "+
				"the decision table has fallen out of step with message.proto", n)
		}
	}
	inSpec := make(map[string]bool, len(want))
	for _, n := range want {
		inSpec[n] = true
	}
	for _, n := range got {
		if !inSpec[n] {
			t.Errorf("MessageServerRequest.body has arm %q, which Spec A §5.7 does not classify. "+
				"Adding an arm is a decision about whether it carries req_auth (Spec B §4.3.8); "+
				"record that decision in specA57 rather than defaulting it.", n)
		}
	}
	if len(got) != len(want) {
		t.Errorf("arm count: descriptor has %d, specA57 has %d", len(got), len(want))
	}
}

// TestRequestArmNumberIsItsOpByte is the assertion the whole file exists for.
func TestRequestArmNumberIsItsOpByte(t *testing.T) {
	fields := bodyOneof(t, (*protocol.MessageServerRequest)(nil))
	for i := 0; i < fields.Len(); i++ {
		f := fields.Get(i)
		name := string(f.Name())
		spec, ok := specA57[name]
		if !ok {
			// Reported by TestRequestArmSetMatchesSpecA57; nothing to compare against.
			continue
		}
		if int(f.Number()) != spec.op {
			t.Errorf("arm %q has field number %d but Spec A §5.7 / Spec B §4.3.8 give it op byte %d. "+
				"The field number IS the op byte: it is hashed into req_auth as u8(op), so this "+
				"mismatch is a silent cross-implementation MAC failure answered with REASON_REJECTED.",
				name, f.Number(), spec.op)
		}
	}
}

// TestRequestOpBytesAgreeWithSpecSection438 checks the transcription of §4.3 against
// the independent transcription of §4.3.8, and both against the descriptor.
func TestRequestOpBytesAgreeWithSpecSection438(t *testing.T) {
	fields := bodyOneof(t, (*protocol.MessageServerRequest)(nil))
	numbers := armNumbers(fields)

	for name, op := range spec438OpBytes {
		got, ok := numbers[name]
		if !ok {
			t.Errorf("Spec B §4.3.8 names op byte %d for arm %q, which MessageServerRequest.body does not have", op, name)
			continue
		}
		if got != op {
			t.Errorf("arm %q: descriptor field number %d, Spec B §4.3.8 prose op byte %d", name, got, op)
		}
		if spec, ok := specA57[name]; ok && spec.op != op {
			t.Errorf("arm %q: specA57 op %d disagrees with the §4.3.8 prose op %d "+
				"(two transcriptions of the same protocol constant must not differ)", name, spec.op, op)
		}
	}
}

// TestRequestArmOpBytesFitInAU8 derives the u8 ceiling from the descriptor. An arm
// numbered above 255 cannot be expressed as u8(op) at all, so it is not a "large
// number" but a request that cannot be authenticated. Asserting it here means such
// an arm fails at build time rather than at the first MAC verification.
func TestRequestArmOpBytesFitInAU8(t *testing.T) {
	fields := bodyOneof(t, (*protocol.MessageServerRequest)(nil))
	if fields.Len() == 0 {
		t.Fatal("MessageServerRequest.body has no arms")
	}
	for i := 0; i < fields.Len(); i++ {
		f := fields.Get(i)
		n := int(f.Number())
		if n < 1 || n > 255 {
			t.Errorf("arm %q has field number %d, which does not fit the u8(op) of the req_auth "+
				"preimage (Spec A §5.7). An arm outside 1..255 has no representable op byte and "+
				"could never be authenticated; renumber it inside the range.", f.Name(), n)
		}
	}
	// The table must not be able to promise an op byte that the wire cannot carry
	// either, which is why armAuth.op is an int.
	for name, spec := range specA57 {
		if spec.op < 1 || spec.op > 255 {
			t.Errorf("specA57 gives arm %q op byte %d, outside the u8 range of Spec A §5.7", name, spec.op)
		}
	}
}

// TestReqAuthArmsCarryAReqAuthField derives, from each arm's own message
// descriptor, whether it actually has somewhere to put a req_auth, and checks that
// against the decision table. An arm classified as authenticated but with no
// req_auth field could never be authenticated; an arm classified as exempt but
// carrying one invites a server to verify something no client computes.
func TestReqAuthArmsCarryAReqAuthField(t *testing.T) {
	fields := bodyOneof(t, (*protocol.MessageServerRequest)(nil))
	for i := 0; i < fields.Len(); i++ {
		f := fields.Get(i)
		name := string(f.Name())
		spec, ok := specA57[name]
		if !ok {
			continue
		}
		md := f.Message()
		if md == nil {
			t.Errorf("arm %q is not a message type; every oneof arm of MessageServerRequest.body is", name)
			continue
		}
		hasReqAuth := md.Fields().ByName("req_auth") != nil
		if hasReqAuth != spec.requiresReqAuth {
			t.Errorf("arm %q: Spec A §5.7 says requiresReqAuth=%v, but %s %s req_auth field",
				name, spec.requiresReqAuth, md.FullName(),
				map[bool]string{true: "has a", false: "has no"}[hasReqAuth])
		}
		// §4.3.8: the read key is selected by read_epoch, which travels inside
		// canonical_request_bytes. An authenticated request without it would force
		// the server to trial keys, which §4.3.8 explicitly forbids.
		hasReadEpoch := md.Fields().ByName("read_epoch") != nil
		if hasReadEpoch != spec.requiresReqAuth {
			t.Errorf("arm %q: requiresReqAuth=%v, but %s %s read_epoch field. Spec B §4.3.8 "+
				"requires the epoch to be named inside the MAC so the server never trials keys.",
				name, spec.requiresReqAuth, md.FullName(),
				map[bool]string{true: "has a", false: "has no"}[hasReadEpoch])
		}
	}
}

// TestRequiredReqAuthOpsAreExactlySpecA57 checks the required set as a set of
// NUMBERS, so that renaming an arm cannot move a number in or out of the
// authenticated set unnoticed.
func TestRequiredReqAuthOpsAreExactlySpecA57(t *testing.T) {
	fields := bodyOneof(t, (*protocol.MessageServerRequest)(nil))
	var got []int
	for i := 0; i < fields.Len(); i++ {
		f := fields.Get(i)
		if spec, ok := specA57[string(f.Name())]; ok && spec.requiresReqAuth {
			got = append(got, int(f.Number()))
		}
	}
	sort.Ints(got)
	want := append([]int(nil), specA57RequiredOps...)
	sort.Ints(want)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("op bytes requiring req_auth: got %v, Spec A §5.7 requires exactly %v "+
			"(Fetch 13, Subscribe 14, GroupStatus 16, BlobGrant 17, WrapFetch 19)", got, want)
	}
}

// TestResponseArmNumbersMatchRequestArms pins the request/response symmetry, and
// pins the one deliberate asymmetry.
func TestResponseArmNumbersMatchRequestArms(t *testing.T) {
	reqNumbers := armNumbers(bodyOneof(t, (*protocol.MessageServerRequest)(nil)))
	respNumbers := armNumbers(bodyOneof(t, (*protocol.MessageServerResponse)(nil)))

	for name, respNum := range respNumbers {
		reqNum, ok := reqNumbers[name]
		if !ok {
			t.Errorf("MessageServerResponse.body has arm %q with no matching request arm; "+
				"response arms exist to answer request arms and share their numbers", name)
			continue
		}
		if reqNum != respNum {
			t.Errorf("arm %q: request number %d, response number %d. They must agree — the request "+
				"number is the op byte inside req_auth (Spec A §5.7), and a response arm that "+
				"disagrees makes the §4.3.8 cross-check unreadable.", name, reqNum, respNum)
		}
	}

	// Derive the arms that exist only on the request side, rather than listing them.
	var onlyRequest []string
	for name := range reqNumbers {
		if _, ok := respNumbers[name]; !ok {
			onlyRequest = append(onlyRequest, name)
		}
	}
	sort.Strings(onlyRequest)
	want := []string{"unsubscribe"}
	if fmt.Sprint(onlyRequest) != fmt.Sprint(want) {
		t.Errorf("request arms with no response arm: got %v, want %v. Exactly one arm is answered "+
			"by reason alone: UnsubscribeRequest, which Spec B §4.3.8 records as reading no group "+
			"state and cancelling only the caller's own subscription.", onlyRequest, want)
	}
}

// TestResponseHasNoArm15 asserts the gap at 15 is deliberate rather than tolerated.
//
// MessageServerResponse has no arm 15 because there is no UnsubscribeResponse. That
// is not an omission to tidy up. Number 15 belongs to `unsubscribe` on the request
// side, where it is also an op byte; handing it to some other response arm would
// break the invariant that a number means the same operation in both envelopes,
// and would make the §4.3.8 op-byte cross-check read differently depending on which
// envelope you looked at. The number stays reserved by staying empty.
func TestResponseHasNoArm15(t *testing.T) {
	fields := bodyOneof(t, (*protocol.MessageServerResponse)(nil))
	for i := 0; i < fields.Len(); i++ {
		f := fields.Get(i)
		if f.Number() == 15 {
			t.Errorf("MessageServerResponse.body gained arm %q at number 15. 15 is unsubscribe's "+
				"op byte on the request side and is deliberately unused here (Spec B §4.3): there "+
				"is no UnsubscribeResponse, and reusing the number for anything else breaks the "+
				"one-number-one-operation invariant across the two envelopes.", f.Name())
		}
	}
	// And the reason it is absent is that nothing answers unsubscribe with a body.
	if fields.ByName("unsubscribe") != nil {
		t.Error("MessageServerResponse.body has an `unsubscribe` arm; Spec B §4.3 defines no UnsubscribeResponse")
	}
}

// TestArmNumbersAreUniqueWithinEachEnvelope derives uniqueness from the descriptor.
// protoc already rejects a duplicate field number inside one message, so this
// cannot fail while the file compiles; it is kept as a tripwire for the day the
// descriptors stop coming from protoc (a hand-built descriptor, a dynamicpb
// schema, a merge that resolves a conflict by hand).
func TestArmNumbersAreUniqueWithinEachEnvelope(t *testing.T) {
	for _, env := range []struct {
		name string
		msg  protoreflect.ProtoMessage
	}{
		{"MessageServerRequest", (*protocol.MessageServerRequest)(nil)},
		{"MessageServerResponse", (*protocol.MessageServerResponse)(nil)},
		{"MessageServerPush", (*protocol.MessageServerPush)(nil)},
	} {
		fields := bodyOneof(t, env.msg)
		seen := make(map[int]string, fields.Len())
		for i := 0; i < fields.Len(); i++ {
			f := fields.Get(i)
			n := int(f.Number())
			if prev, dup := seen[n]; dup {
				t.Errorf("%s.body: arms %q and %q share number %d", env.name, prev, f.Name(), n)
			}
			seen[n] = string(f.Name())
		}
	}
}

// TestArmNumberMeansOneOperationAcrossEnvelopes is the falsifiable form of
// uniqueness: across MessageServerRequest and MessageServerResponse together, a
// number must name exactly one operation. A swap of two response arms' numbers, or
// a response arm reusing a request-only number, shows up here as one number
// claimed by two different arm names.
func TestArmNumberMeansOneOperationAcrossEnvelopes(t *testing.T) {
	owner := make(map[int]string)
	for _, env := range []struct {
		name string
		msg  protoreflect.ProtoMessage
	}{
		{"MessageServerRequest", (*protocol.MessageServerRequest)(nil)},
		{"MessageServerResponse", (*protocol.MessageServerResponse)(nil)},
	} {
		fields := bodyOneof(t, env.msg)
		for i := 0; i < fields.Len(); i++ {
			f := fields.Get(i)
			n := int(f.Number())
			name := string(f.Name())
			if prev, ok := owner[n]; ok && prev != name {
				t.Errorf("number %d names arm %q in one envelope and %q in the other; "+
					"a number is an operation, and Spec A §5.7 hashes it as u8(op)", n, prev, name)
			}
			owner[n] = name
		}
	}
}

// isAuthenticatorField DERIVES whether a field is a request authenticator, rather
// than matching it against a list of the authenticators that exist today.
//
// Spec B §4.3 puts the request authenticator at field 15 in every message that has
// one. There are six such fields right now: `req_auth` on the five authenticated
// read arms, and the five rendezvous authenticators of §4.3.11 — `register_auth`,
// `open_auth`, `deposit_auth`, `collect_auth`, `retire_auth` — which §4.3.11
// describes as sitting "at field 15, the slot req_auth occupies on the group
// arms", each under its own name because each is verified against a different
// pinned key.
//
// The naming convention is what makes the class derivable: every authenticator in
// message.proto is spelled `<something>_auth`, and nothing else in the file is.
// `write_auth` is the one authenticator that is NOT a proto field at all — it
// lives inside the opaque Record.record_bytes and is parsed by
// message.ParseRecord (§4.3.3, §12.1 A-2) — so it cannot be caught here and does
// not need to be.
//
// A hand-typed list of the six names would let a SEVENTH authenticator, added
// under any other name, escape the field-15 assertion in silence. Given the shape
// of §4.3.11 a sixth rendezvous operation is a plausible near-term addition, and
// the whole point of this test is that it should not be possible to add one
// without deciding, explicitly, that its authenticator sits at 15.
func isAuthenticatorField(name protoreflect.Name) bool {
	return strings.HasSuffix(string(name), "_auth")
}

// knownAuthenticatorFields is NOT the class this test gates — isAuthenticatorField
// is. It is a floor: the six authenticator fields that exist in message.proto
// today, asserted to be found by the derivation, so that a derivation which
// quietly stops matching anything (a renamed suffix, a walk over the wrong file)
// fails loudly instead of passing over an empty set.
var knownAuthenticatorFields = []string{
	"req_auth", "register_auth", "open_auth", "deposit_auth", "collect_auth", "retire_auth",
}

// readEpochIsAMacInput says whether a message's `read_epoch` is one of the ones
// Spec B §4.3 reserves field 14 for.
//
// THE RULE WAS NARROWED BY RULING 32, AND IT IS A PREDICATE SO THAT WHAT THE
// NARROWING REMOVES CAN BE PRINTED. Until 2026-09-22 the assertion below was "every
// field named read_epoch in this file is field 14", over every message in it. That
// held because every read_epoch there had ever been was a REQUEST's, and a request's
// every field number is inside canonical_request_bytes and therefore inside the
// req_auth MAC (Spec A §5.7) — which is the entire reason 14 is reserved rather than
// merely conventional. Ruling 32 put a read_epoch on FetchAttestation, which is a
// RESPONSE sub-message: it is in no MAC, its numbering is pinned by §4.3.4's
// explicit named preimage list and by nothing else, and Spec B §4.3.4 and MASTER
// §9.4 both number it 11 because `sig` landed at 10 and a landed field number is
// never renumbered.
//
// The predicate is "this message carries a req_auth" — the same seed set
// reqAuthSeeds derives the MAC-input closure from — and not a list of message names,
// so a new authenticated request with a read_epoch is inside the rule on the day it
// is added. The test PRINTS THE COMPLEMENT, every read_epoch the predicate excludes,
// and fails unless that complement is exactly what this narrowing was taken for. A
// gate that was widened by being narrowed is the failure mode here, and an exclusion
// list checked in both directions is what makes it noisy instead of silent.
func readEpochIsAMacInput(md protoreflect.MessageDescriptor) bool {
	return md.Fields().ByName("req_auth") != nil
}

// readEpochOutsideTheMac is the complement the narrowing above is allowed to have,
// transcribed from Spec B §4.3.4 with the number that section gives it. The walk
// asserts the complement it actually finds EQUALS this, in both directions.
var readEpochOutsideTheMac = map[string]int{
	"bringyour.FetchAttestation.read_epoch": 11,
}

// TestReqAuthAndReadEpochUseTheirReservedFieldNumbers walks every message in
// message.proto, not a list of the ones that happen to have these fields today,
// and derives the authenticator class by name rather than enumerating it.
// Because req_auth is zeroed inside canonical_request_bytes, its field number
// decides where the hole in the canonical bytes falls, so it is as much a protocol
// constant as the op byte is.
func TestReqAuthAndReadEpochUseTheirReservedFieldNumbers(t *testing.T) {
	fd := (*protocol.MessageServerRequest)(nil).ProtoReflect().Descriptor().ParentFile()
	msgs := fd.Messages()
	foundAuth := map[string]bool{}
	checkedEpoch := 0
	excluded := map[string]int{}
	for i := 0; i < msgs.Len(); i++ {
		md := msgs.Get(i)
		fields := md.Fields()
		for j := 0; j < fields.Len(); j++ {
			f := fields.Get(j)
			name := string(f.Name())
			if isAuthenticatorField(f.Name()) {
				foundAuth[name] = true
				if f.Number() != 15 {
					t.Errorf("%s.%s is field %d; Spec B §4.3 reserves 15 for the request "+
						"authenticator, and req_auth's number decides where the zeroed hole "+
						"falls inside canonical_request_bytes (Spec A §5.7)", md.FullName(), name, f.Number())
				}
				if f.Kind() != protoreflect.BytesKind || f.Cardinality() == protoreflect.Repeated {
					t.Errorf("%s.%s is %v %v; every authenticator in Spec B §4.3 is a singular "+
						"`bytes`, and canonical_request_bytes is defined as that field \"set to "+
						"zero length\" (Spec A §5.7), which only a length-delimited field has",
						md.FullName(), name, f.Cardinality(), f.Kind())
				}
			}
			if name == "read_epoch" {
				if !readEpochIsAMacInput(md) {
					excluded[string(f.FullName())] = int(f.Number())
					continue
				}
				checkedEpoch++
				if f.Number() != 14 {
					t.Errorf("%s.read_epoch is field %d; Spec B §4.3 reserves 14 for it on every "+
						"message that carries a req_auth, because its number is inside "+
						"canonical_request_bytes (Spec A §5.7)", md.FullName(), f.Number())
				}
			}
		}
	}
	if len(foundAuth) == 0 || checkedEpoch == 0 {
		t.Fatal("walked message.proto and found no authenticator or no read_epoch field at all; " +
			"the walk is looking at the wrong file descriptor")
	}
	for _, name := range knownAuthenticatorFields {
		if !foundAuth[name] {
			t.Errorf("the authenticator derivation did not match %q, which message.proto still "+
				"defines. isAuthenticatorField has stopped recognising the class it is supposed "+
				"to derive, so the field-15 assertion is now passing over a smaller set than it "+
				"claims to cover.", name)
		}
	}

	// THE COMPLEMENT, PRINTED. Everything readEpochIsAMacInput removed from the
	// field-14 rule, with the number it is at instead, checked against the one
	// exclusion ruling 32 bought. An empty complement would mean the narrowing is
	// doing nothing and the predicate is wrong; a larger one would mean a read_epoch
	// escaped the rule after this test was narrowed, which is the failure this block
	// exists to catch and the reason the narrowing is not simply an `if` on a name.
	t.Logf("field-14 rule applied to %d read_epoch fields; complement (excluded) = %v", checkedEpoch, excluded)
	for name, number := range excluded {
		want, allowed := readEpochOutsideTheMac[name]
		if !allowed {
			t.Errorf("%s is outside the field-14 rule because its message carries no req_auth, and "+
				"nothing in this file says it may be. Either it belongs on an authenticated request "+
				"— in which case it is field 14 — or Spec B gives it a number of its own and that "+
				"number belongs in readEpochOutsideTheMac with the section that says so.", name)
			continue
		}
		if number != want {
			t.Errorf("%s is field %d; Spec B numbers it %d", name, number, want)
		}
	}
	for name := range readEpochOutsideTheMac {
		if _, found := excluded[name]; !found {
			t.Errorf("readEpochOutsideTheMac excuses %s from the field-14 rule, and the walk did not "+
				"exclude it. Either it is gone from message.proto or it has moved onto a req_auth "+
				"carrier, and in both cases the excuse is now covering nothing.", name)
		}
	}
	if len(excluded) == 0 {
		t.Error("the complement is empty, so readEpochIsAMacInput narrowed nothing and this whole " +
			"block is passing vacuously over a rule that is still the old one")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// THE BODY OF AN AUTHENTICATED REQUEST IS A MAC INPUT IN FULL, NOT JUST ITS
// ARM NUMBER.
//
// Everything above gates the op byte, req_auth's field number and read_epoch's.
// That stops one level above the class the spec actually names. Spec A §5.7 and
// Spec B §4.3.8 define
//
//	canonical_request_bytes = the deterministically-marshaled request body message
//	                          (protobuf deterministic marshal, fields ascending)
//	                          with its own `req_auth` field set to zero length.
//
// "the request body message" — the whole message. Every field number in it, every
// field's wire type, and every field's presence rule is therefore a protocol
// constant hashed into the MAC, exactly as the arm number is. Transposing
// FetchRequest's `heads_only = 4` and `class_mask = 5`, deleting a field,
// narrowing `read_epoch` from uint64 to uint32, or adding a proto3 `optional`
// keyword to a scalar all change canonical_request_bytes for the same logical
// request — and all of them compile, parse, and round-trip. They surface only as
// a MAC mismatch on one operation, for one implementation, answered with the
// deliberately non-specific REASON_REJECTED of §4.5. That is the same archetypal
// defect the op-byte gate exists to prevent, one level down.
//
// The round-trip tests in message_test.go cannot see any of this: `populate`
// derives the field set from the same descriptor it then compares against, so
// source and destination agree on whatever schema the file happens to declare.
// Nothing that reads only the descriptor can catch a descriptor that is wrong.
// The transcription below is the second, independent source that makes the
// comparison possible.
//
// The CLASS is derived, not listed. The seed set is every message in
// message.proto that carries a `req_auth` field — the exact class §5.7's recipe
// applies to — and the gated set is the transitive closure of the message types
// reachable from those seeds, because a nested message inside a MAC input is
// itself inside the MAC. `Subscription` is in the closure for exactly that
// reason: it is not a request body, but SubscribeRequest carries a repeated
// Subscription, so its field numbers are hashed too.
//
// The rendezvous arms are deliberately NOT seeded here. Their authenticators are
// Ed25519 signatures over the explicit field-by-field preimages of Spec A §5.14,
// not over a marshaled body, so their field numbering is pinned by that preimage
// rather than by canonical_request_bytes. Only `req_auth` carries the
// whole-message rule.
// ─────────────────────────────────────────────────────────────────────────────

// macFieldSpec is one field of a MAC-input message as Spec B declares it, on
// every axis that changes canonical_request_bytes.
type macFieldSpec struct {
	// number is the field number, which is the wire tag.
	number int
	// kind is the declared type, which fixes the wire type and the encoding.
	kind protoreflect.Kind
	// cardinality distinguishes singular from repeated; a repeated field encodes
	// as several occurrences, or one packed run, rather than as one value.
	cardinality protoreflect.Cardinality
	// typeName is the fully-qualified name of the message or enum type, for
	// MessageKind and EnumKind fields, and "" for scalars. A field that keeps its
	// number and kind but changes its message type reshapes the bytes entirely.
	typeName string
	// explicitPresence is true only where the spec writes the proto3 `optional`
	// keyword. It is false everywhere in Spec B §4.3, and it matters: under
	// implicit presence a zero-valued field is omitted from the encoding, while
	// under explicit presence a zero that was set is emitted. The two spellings
	// of the same logical request then MAC differently.
	explicitPresence bool
}

func macScalar(number int, kind protoreflect.Kind) macFieldSpec {
	return macFieldSpec{number: number, kind: kind, cardinality: protoreflect.Optional}
}

func macRepeatedMessage(number int, typeName string) macFieldSpec {
	return macFieldSpec{
		number:      number,
		kind:        protoreflect.MessageKind,
		cardinality: protoreflect.Repeated,
		typeName:    typeName,
	}
}

func macEnum(number int, typeName string) macFieldSpec {
	return macFieldSpec{
		number:      number,
		kind:        protoreflect.EnumKind,
		cardinality: protoreflect.Optional,
		typeName:    typeName,
	}
}

// specBMacInputs transcribes, from the proto blocks of Spec B §4.3.4 (Fetch),
// §4.3.5 (Subscribe), §4.3.6 (Blob grant), §4.3.9 (Wrap fetch) and §4.3.10
// (GroupStatusRequest), every field of every message that is hashed into a
// req_auth. Keyed by the message's simple name, then by the field's name, so a
// RENAME shows up as a key-set difference and a RENUMBER as a value difference.
//
// This map is a transcription, not an inventory: TestMacInputMessagesAreExactlyTheReqAuthClosure
// asserts its key set equals the closure derived from the descriptor, so a field
// added to an authenticated body — or a new message type nested inside one —
// fails here until somebody copies the spec's numbering in.
var specBMacInputs = map[string]map[string]macFieldSpec{
	// Spec B §4.3.4.
	"FetchRequest": {
		"group_id":        macScalar(1, protoreflect.BytesKind),
		"since_record_id": macScalar(2, protoreflect.Uint64Kind),
		"limit":           macScalar(3, protoreflect.Uint32Kind),
		"heads_only":      macScalar(4, protoreflect.BoolKind),
		"class_mask":      macScalar(5, protoreflect.Uint32Kind),
		"read_epoch":      macScalar(14, protoreflect.Uint64Kind),
		"req_auth":        macScalar(15, protoreflect.BytesKind),
	},
	// Spec B §4.3.5.
	"SubscribeRequest": {
		"subscriptions": macRepeatedMessage(1, "bringyour.Subscription"),
		"replace":       macScalar(2, protoreflect.BoolKind),
		"read_epoch":    macScalar(14, protoreflect.Uint64Kind),
		"req_auth":      macScalar(15, protoreflect.BytesKind),
	},
	// Spec B §4.3.5. Not a request body, but every SubscribeRequest carries a
	// repeated Subscription, so these two numbers are inside the MAC as surely as
	// SubscribeRequest's own are.
	"Subscription": {
		"group_id":        macScalar(1, protoreflect.BytesKind),
		"since_record_id": macScalar(2, protoreflect.Uint64Kind),
	},
	// Spec B §4.3.6.
	"BlobGrantRequest": {
		"group_id":        macScalar(1, protoreflect.BytesKind),
		"blob_id":         macScalar(2, protoreflect.BytesKind),
		"direction":       macEnum(3, "bringyour.Direction"),
		"declared_bytes":  macScalar(4, protoreflect.Uint64Kind),
		"retention_class": macScalar(5, protoreflect.Uint32Kind),
		"read_epoch":      macScalar(14, protoreflect.Uint64Kind),
		"req_auth":        macScalar(15, protoreflect.BytesKind),
	},
	// Spec B §4.3.10, which declares it on one line.
	"GroupStatusRequest": {
		"group_id":   macScalar(1, protoreflect.BytesKind),
		"read_epoch": macScalar(14, protoreflect.Uint64Kind),
		"req_auth":   macScalar(15, protoreflect.BytesKind),
	},
	// Spec B §4.3.9.
	"WrapFetchRequest": {
		"group_id":           macScalar(1, protoreflect.BytesKind),
		"epoch":              macScalar(2, protoreflect.Uint64Kind),
		"wrap_target_handle": macScalar(3, protoreflect.BytesKind),
		"want_snapshot":      macScalar(4, protoreflect.BoolKind),
		"read_epoch":         macScalar(14, protoreflect.Uint64Kind),
		"req_auth":           macScalar(15, protoreflect.BytesKind),
	},
}

// reqAuthSeeds returns the messages Spec A §5.7's canonical_request_bytes recipe
// applies to: those carrying a `req_auth`. Derived from the file descriptor.
func reqAuthSeeds(t *testing.T) []protoreflect.MessageDescriptor {
	t.Helper()
	fd := (*protocol.MessageServerRequest)(nil).ProtoReflect().Descriptor().ParentFile()
	msgs := fd.Messages()
	var seeds []protoreflect.MessageDescriptor
	for i := 0; i < msgs.Len(); i++ {
		md := msgs.Get(i)
		if md.Fields().ByName("req_auth") != nil {
			seeds = append(seeds, md)
		}
	}
	return seeds
}

// macInputClosure returns every message descriptor reachable from a req_auth
// carrier, seeds included, keyed by simple name. Reachability is what makes this
// a derived class rather than a list: nesting a new message type inside an
// authenticated body pulls that type into the MAC, and therefore into the gate.
func macInputClosure(t *testing.T) map[string]protoreflect.MessageDescriptor {
	t.Helper()
	out := map[string]protoreflect.MessageDescriptor{}
	var visit func(md protoreflect.MessageDescriptor)
	visit = func(md protoreflect.MessageDescriptor) {
		name := string(md.Name())
		if _, seen := out[name]; seen {
			return
		}
		out[name] = md
		fields := md.Fields()
		for i := 0; i < fields.Len(); i++ {
			f := fields.Get(i)
			if sub := f.Message(); sub != nil {
				visit(sub)
			}
			if f.IsMap() {
				if sub := f.MapValue().Message(); sub != nil {
					visit(sub)
				}
			}
		}
	}
	for _, seed := range reqAuthSeeds(t) {
		visit(seed)
	}
	return out
}

// TestMacInputMessagesAreExactlyTheReqAuthClosure is the equality that makes the
// field-level assertions below mean something, in the same way
// TestRequestArmSetMatchesSpecA57 does for the arms. Without it, a field added to
// FetchRequest — or a whole new message type nested inside SubscribeRequest —
// would simply go ungated.
func TestMacInputMessagesAreExactlyTheReqAuthClosure(t *testing.T) {
	closure := macInputClosure(t)

	seeds := reqAuthSeeds(t)
	if len(seeds) != 5 {
		t.Errorf("found %d messages carrying req_auth; Spec A §5.7 requires it on exactly five "+
			"(FetchRequest, SubscribeRequest, GroupStatusRequest, BlobGrantRequest, WrapFetchRequest). "+
			"canonical_request_bytes is defined for that class and no other.", len(seeds))
	}

	for name := range closure {
		if _, ok := specBMacInputs[name]; !ok {
			t.Errorf("%s is reachable from a req_auth-carrying request body, so every one of its "+
				"field numbers is hashed into canonical_request_bytes (Spec A §5.7), but Spec B's "+
				"declaration of it has not been transcribed into specBMacInputs. Transcribe it: "+
				"the numbers are protocol constants, not implementation detail.", name)
		}
	}
	for name := range specBMacInputs {
		if _, ok := closure[name]; !ok {
			t.Errorf("specBMacInputs transcribes %s, but nothing reachable from a req_auth-carrying "+
				"body has that name any more. Either the message was renamed — which changes nothing "+
				"on the wire but breaks this cross-check — or it dropped out of the MAC input set, "+
				"which is a protocol change.", name)
		}
	}
	if len(closure) != len(specBMacInputs) {
		t.Errorf("MAC-input closure has %d messages, specBMacInputs transcribes %d", len(closure), len(specBMacInputs))
	}
}

// TestMacInputFieldsMatchSpecB compares every field of every MAC-input message
// against Spec B on the axes that change canonical_request_bytes: name, number,
// kind, cardinality, type and presence. The message set is derived; only the
// numbering is transcribed.
func TestMacInputFieldsMatchSpecB(t *testing.T) {
	closure := macInputClosure(t)
	names := make([]string, 0, len(closure))
	for name := range closure {
		names = append(names, name)
	}
	sort.Strings(names)

	checked := 0
	for _, name := range names {
		md := closure[name]
		want, ok := specBMacInputs[name]
		if !ok {
			// Reported by TestMacInputMessagesAreExactlyTheReqAuthClosure.
			continue
		}
		t.Run(name, func(t *testing.T) {
			fields := md.Fields()

			// Key-set equality in both directions: a deleted field and an added
			// field are each a change to the MAC preimage.
			seen := map[string]bool{}
			for i := 0; i < fields.Len(); i++ {
				seen[string(fields.Get(i).Name())] = true
			}
			for fname := range want {
				if !seen[fname] {
					t.Errorf("Spec B declares %s.%s, which message.proto does not have. A field "+
						"missing from a MAC input changes canonical_request_bytes for every "+
						"request of this kind, so this client can never authenticate to a "+
						"server built from the spec.", name, fname)
				}
			}
			for i := 0; i < fields.Len(); i++ {
				fname := string(fields.Get(i).Name())
				if _, ok := want[fname]; !ok {
					t.Errorf("message.proto declares %s.%s, which Spec B does not. Every field of "+
						"this message is hashed into canonical_request_bytes (Spec A §5.7), so an "+
						"extra field is an extra MAC input that no peer built from the spec will "+
						"reproduce.", name, fname)
				}
			}

			for i := 0; i < fields.Len(); i++ {
				f := fields.Get(i)
				fname := string(f.Name())
				spec, ok := want[fname]
				if !ok {
					continue
				}
				checked++
				if int(f.Number()) != spec.number {
					t.Errorf("%s.%s is field %d; Spec B numbers it %d. The field number is the wire "+
						"tag, and the tag bytes are inside canonical_request_bytes, so this is a "+
						"silent cross-implementation MAC failure answered with REASON_REJECTED.",
						name, fname, f.Number(), spec.number)
				}
				if f.Kind() != spec.kind {
					t.Errorf("%s.%s is %v; Spec B declares it %v. The kind fixes the wire type and "+
						"the encoding of the value, both of which are inside the MAC.",
						name, fname, f.Kind(), spec.kind)
				}
				if f.Cardinality() != spec.cardinality {
					t.Errorf("%s.%s is %v; Spec B declares it %v. Repeated and singular encode "+
						"differently, so this changes canonical_request_bytes.",
						name, fname, f.Cardinality(), spec.cardinality)
				}
				gotType := ""
				if f.Message() != nil {
					gotType = string(f.Message().FullName())
				} else if f.Enum() != nil {
					gotType = string(f.Enum().FullName())
				}
				if gotType != spec.typeName {
					t.Errorf("%s.%s has type %q; Spec B declares %q. A field that keeps its number "+
						"but changes its type reshapes the MAC preimage completely.",
						name, fname, gotType, spec.typeName)
				}
				if f.HasOptionalKeyword() != spec.explicitPresence {
					t.Errorf("%s.%s: proto3 `optional` keyword present = %v, Spec B declares %v. "+
						"Explicit presence emits a set zero value that implicit presence omits, so "+
						"two implementations that disagree about the keyword compute different "+
						"canonical_request_bytes for the same logical request — the same defect "+
						"the req_auth field-number rule guards against, one field over.",
						name, fname, f.HasOptionalKeyword(), spec.explicitPresence)
				}
				wantPresence := spec.explicitPresence ||
					(spec.cardinality == protoreflect.Optional &&
						(spec.kind == protoreflect.MessageKind || spec.kind == protoreflect.GroupKind))
				if f.HasPresence() != wantPresence {
					t.Errorf("%s.%s: HasPresence() = %v, Spec B's declaration implies %v",
						name, fname, f.HasPresence(), wantPresence)
				}
				if od := f.ContainingOneof(); od != nil && !od.IsSynthetic() {
					t.Errorf("%s.%s sits inside `oneof %s`. Spec B declares no oneof in any MAC "+
						"input; membership changes which fields may be present at once and so "+
						"changes the set of reachable canonical_request_bytes.", name, fname, od.Name())
				}
			}

			// A oneof added to a MAC input would also let two peers disagree about
			// which fields can coexist. Synthetic oneofs (the proto3 `optional`
			// keyword) are reported per-field above, with a better message.
			oneofs := md.Oneofs()
			for i := 0; i < oneofs.Len(); i++ {
				if od := oneofs.Get(i); !od.IsSynthetic() {
					t.Errorf("%s declares `oneof %s`; Spec B declares none on any MAC input", name, od.Name())
				}
			}
		})
	}
	if checked == 0 {
		t.Fatal("compared no fields at all; the MAC-input closure is empty and this gate is vacuous")
	}
	t.Logf("compared %d fields across %d MAC-input messages against Spec B", checked, len(names))
}

// TestMacInputEnumsAreTranscribed closes the last hole in the closure: an enum
// reachable from a MAC input contributes its VALUE NUMBERS to the preimage, not
// just its field's tag. BlobGrantRequest.direction is the live case —
// DIRECTION_UPLOAD encodes as 1 and nothing else — so Direction's numbering is a
// MAC input in exactly the way FetchRequest's field numbers are.
//
// The transcription itself lives in specBEnums (message_wire_test.go), which
// covers every enum in message.proto; this asserts the reachable ones are in it,
// so a new enum introduced into an authenticated body cannot arrive ungated.
func TestMacInputEnumsAreTranscribed(t *testing.T) {
	closure := macInputClosure(t)
	reachable := map[string]bool{}
	for _, md := range closure {
		fields := md.Fields()
		for i := 0; i < fields.Len(); i++ {
			f := fields.Get(i)
			if ed := f.Enum(); ed != nil {
				reachable[string(ed.Name())] = true
			}
			if f.IsMap() {
				if ed := f.MapValue().Enum(); ed != nil {
					reachable[string(ed.Name())] = true
				}
			}
		}
	}
	if len(reachable) == 0 {
		t.Fatal("no enum is reachable from any MAC input; BlobGrantRequest.direction is a " +
			"Direction, so the closure walk is wrong")
	}
	for name := range reachable {
		if _, ok := specBEnums[name]; !ok {
			t.Errorf("enum %s is reachable from a req_auth-carrying request body, so its value "+
				"numbers are hashed into canonical_request_bytes, but it is not transcribed in "+
				"specBEnums", name)
		}
	}
}
