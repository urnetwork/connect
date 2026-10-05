package protocol_test

// Wire code points and enum numbering for the URmessage control plane.
//
// message_op_test.go gates the numbers that live inside a MAC. This file gates the
// numbers that live on the wire outside one: the four `MessageType` frame code
// points of Spec B §4.2 / Spec A §10.1, and the value numbering of every enum in
// message.proto.
//
// Neither class had any test at all. That matters most for the frame code points,
// because their enum value NAMES are deliberately diverged from both specs — see
// frame.proto, where the domain prefix is repeated to dodge a proto3 scoping
// collision with the messages of the same name — so after that divergence the
// NUMBERS are the only thing still tying the block to the normative text. A
// renumbered code point is not a MAC failure; it is worse. Two peers simply stop
// recognising each other's frames, and the frame is discarded as an unknown
// message type, which is precisely the behaviour a forward-compatible enum is
// supposed to have for a code point that does not exist yet.
//
// As everywhere else in this package, the class is DERIVED from the descriptor and
// only the numbering is transcribed.

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/urnetwork/connect/protocol"
)

// ── The URmessage MessageType block (Spec B §4.2, Spec A §10.1) ──────────────

// urmessageBlockLo and urmessageBlockHi are the reserved range, transcribed from
// Spec B §4.2: "Block 1000-1099 reserved so parallel beta branches do not
// collide". Both the four exact values and their containment in the block are
// protocol constants — the block exists so that a second beta branch can add its
// own code points without a collision, which only works if this branch's stay
// inside it.
const (
	urmessageBlockLo = 1000
	urmessageBlockHi = 1099
)

// urmessageCodePoint is one frame code point: the number both specs give it, the
// name both specs give it, and the message.proto message it carries.
type urmessageCodePoint struct {
	// number is the wire code point. Verbatim from Spec A §10.1 and Spec B §4.2.
	number int
	// specName is the enum value name BOTH SPECS give, which frame.proto cannot
	// use: proto3 scopes enum value names to the enum's parent scope, so a value
	// named `MessageServerRequest` in package bringyour claims the same qualified
	// name as `message MessageServerRequest` in message.proto and protoc refuses
	// the pair. The collision is resolved on the enum side by repeating the domain
	// prefix, the same way this enum already resolves it for ip.proto
	// (`IpIpPing` for message `IpPing`).
	specName string
}

// specUrmessageCodePoints transcribes the block from Spec A §10.1 and Spec B
// §4.2, which state it identically. Keyed by the name frame.proto actually uses,
// with the spec's own name carried alongside so the divergence is auditable
// rather than merely tolerated.
var specUrmessageCodePoints = map[string]urmessageCodePoint{
	"MessageMessageServerRequest":  {number: 1000, specName: "MessageServerRequest"},
	"MessageMessageServerResponse": {number: 1001, specName: "MessageServerResponse"},
	"MessageMessageServerPush":     {number: 1002, specName: "MessageServerPush"},
	"MessageMessageServerFragment": {number: 1003, specName: "MessageServerFragment"},
}

func messageTypeEnum(t *testing.T) protoreflect.EnumDescriptor {
	t.Helper()
	ed, err := protoregistry.GlobalFiles.FindDescriptorByName("bringyour.MessageType")
	if err != nil {
		t.Fatalf("no MessageType enum registered: %v", err)
	}
	enum, ok := ed.(protoreflect.EnumDescriptor)
	if !ok {
		t.Fatalf("bringyour.MessageType is %T, not an enum", ed)
	}
	return enum
}

// TestUrmessageCodePointsMatchTheSpecs asserts the four numbers, in both
// directions: every transcribed name has the transcribed number, and every value
// that landed in the reserved block is one of the four.
//
// The second direction is what makes this a gate rather than four assertions. A
// code point moved OUT of the block (say to 999) fails the first direction; a
// fifth code point added INTO the block without being transcribed fails the
// second.
func TestUrmessageCodePointsMatchTheSpecs(t *testing.T) {
	enum := messageTypeEnum(t)
	values := enum.Values()

	for name, want := range specUrmessageCodePoints {
		v := values.ByName(protoreflect.Name(name))
		if v == nil {
			t.Errorf("frame.proto has no MessageType value %q; Spec A §10.1 and Spec B §4.2 "+
				"define its code point as %d", name, want.number)
			continue
		}
		if int(v.Number()) != want.number {
			t.Errorf("MessageType.%s = %d; Spec A §10.1 and Spec B §4.2 both give it %d. This is "+
				"a wire code point: a frame sent under the wrong number is not rejected, it is "+
				"silently discarded as an unknown message type by the peer.",
				name, v.Number(), want.number)
		}
	}

	inBlock := map[string]int{}
	for i := 0; i < values.Len(); i++ {
		v := values.Get(i)
		n := int(v.Number())
		if n >= urmessageBlockLo && n <= urmessageBlockHi {
			inBlock[string(v.Name())] = n
		}
	}
	for name, n := range inBlock {
		if _, ok := specUrmessageCodePoints[name]; !ok {
			t.Errorf("MessageType.%s = %d sits in the %d-%d block Spec B §4.2 reserves for "+
				"URmessage, but is not one of the four code points the specs define there. "+
				"The block is reserved so parallel beta branches do not collide; adding to it "+
				"is a spec decision.", name, n, urmessageBlockLo, urmessageBlockHi)
		}
	}
	if len(inBlock) != len(specUrmessageCodePoints) {
		t.Errorf("the %d-%d block holds %d MessageType values, the specs define %d",
			urmessageBlockLo, urmessageBlockHi, len(inBlock), len(specUrmessageCodePoints))
	}
}

// TestUrmessageCodePointsStayInsideTheReservedBlock states the containment rule on
// its own, derived from the transcription rather than from the descriptor, so that
// a transcription error is caught too. Spec B §4.2 reserves 1000-1099 "so parallel
// beta branches do not collide": a URmessage code point outside it is a collision
// waiting for whichever branch claims that number next.
func TestUrmessageCodePointsStayInsideTheReservedBlock(t *testing.T) {
	enum := messageTypeEnum(t)
	values := enum.Values()
	for name, want := range specUrmessageCodePoints {
		if want.number < urmessageBlockLo || want.number > urmessageBlockHi {
			t.Errorf("the transcription gives %s the code point %d, outside the %d-%d block "+
				"Spec B §4.2 reserves", name, want.number, urmessageBlockLo, urmessageBlockHi)
		}
		if v := values.ByName(protoreflect.Name(name)); v != nil {
			n := int(v.Number())
			if n < urmessageBlockLo || n > urmessageBlockHi {
				t.Errorf("MessageType.%s = %d, outside the %d-%d block Spec B §4.2 reserves for "+
					"URmessage. Another beta branch is entitled to that number.",
					name, n, urmessageBlockLo, urmessageBlockHi)
			}
		}
	}
}

// TestUrmessageCodePointNamesDivergeDeliberately pins the ONE thing about this
// block that does not match the specs, so that it reads as a decision rather than
// as a typo, and so that the reason for it stays true.
//
// The reason is a proto3 scoping rule: enum value names live in the enum's PARENT
// scope, so `MessageServerRequest = 1000` inside `enum MessageType` in package
// bringyour claims the qualified name `bringyour.MessageServerRequest`, which
// `message MessageServerRequest` in message.proto already holds. protoc refuses
// the pair. The message names are the normative ones — they are the oneof arm
// types the op-byte MAC of Spec B §4.3.8 is defined over — so the collision is
// resolved on the enum side.
//
// This test asserts the collision is real (a message of the spec's name exists),
// that the enum value therefore does not use that name, and that what it uses
// instead is the spec name with a domain prefix rather than an unrelated
// invention. If message.proto ever renamed its envelopes, the collision would
// dissolve and the divergence would need revisiting; this fails then.
func TestUrmessageCodePointNamesDivergeDeliberately(t *testing.T) {
	enum := messageTypeEnum(t)
	values := enum.Values()
	for name, want := range specUrmessageCodePoints {
		full := protoreflect.FullName("bringyour." + want.specName)
		if _, err := protoregistry.GlobalFiles.FindDescriptorByName(full); err != nil {
			t.Errorf("the enum value %s is spelled that way only because %s is taken by a message "+
				"in the same proto package, but no such message is registered (%v). Either the "+
				"message was renamed — in which case the spec's own spelling is available again "+
				"and this divergence should be revisited — or the code point now carries nothing.",
				name, full, err)
			continue
		}
		if name == want.specName {
			t.Errorf("MessageType.%s uses the spec's own name, which collides with message %s; "+
				"protoc could not have accepted this", name, full)
		}
		if !strings.HasSuffix(name, want.specName) {
			t.Errorf("MessageType value %q does not end in the spec's name %q. The divergence is a "+
				"repeated domain prefix (the frame.proto precedent is IpIpPing for message IpPing), "+
				"not a free rename: a reader must be able to get from the spec's name to this one.",
				name, want.specName)
		}
		if v := values.ByName(protoreflect.Name(name)); v == nil {
			t.Errorf("frame.proto has no MessageType value %q", name)
		}
	}
}

// ── Enum value numbering in message.proto ────────────────────────────────────

// specBEnums transcribes every enum Spec B declares for the control plane:
// `Reason` from §4.5 and `Direction` from §4.3.10.
//
// Reason's numbers are not MAC inputs and both ends of the URnetwork Go world link
// this same generated code, so a renumbering here stays self-consistent inside Go.
// They are still protocol constants: Spec B §4.5 numbers them normatively, Spec C
// surfaces them in the Windows client, and Spec A §11.1 plans shared test vectors
// across implementations. Direction's ARE MAC inputs — BlobGrantRequest.direction
// is inside canonical_request_bytes — and message_op_test.go's
// TestMacInputEnumsAreTranscribed asserts that any enum reachable from an
// authenticated request body appears in this map.
var specBEnums = map[string]map[string]int{
	// Spec B §4.5.
	//
	// RECORDED HAZARD, NOT A DEVIATION — REASON_OK = 0.
	//
	// Under proto3 implicit presence a `reason` field that was never set, or that
	// was lost to a truncated or hand-rolled encoder, decodes as 0, and 0 here
	// means success. MessageServerResponse.reason, SubmitResult.reason and
	// SubscriptionAck.reason are all plain fields, so a partially-built
	// SubmitResult in a batch reads as REASON_OK rather than as "unspecified".
	// Failing toward success is a poor default anywhere, and it is a worse one on a
	// surface whose whole design — §4.5's deliberately non-specific
	// REASON_REJECTED, §5.1's padded-latency reject path — is built around refusals
	// being uninformative but unambiguous. This same file gets the sentinel right
	// one enum down: Direction reserves 0 for DIRECTION_UNSPECIFIED.
	//
	// It is transcribed exactly as §4.5 declares it, because it IS §4.5: this is a
	// spec-level default, not an implementation slip, and changing the numbering
	// unilaterally would renumber all eighteen values against a normative table
	// that Spec C and the §11.1 vectors also read. It needs a Spec B ruling.
	// Recorded here, pinned by this test, and raised as a spec divergence so the
	// ruling is asked for rather than assumed.
	"Reason": {
		"REASON_OK":                     0,
		"REASON_REJECTED":               1,
		"REASON_EPOCH_STALE":            2,
		"REASON_COMMIT_LOST":            3,
		"REASON_STREAM_INDEX_REUSED":    4,
		"REASON_STREAM_INDEX_REGRESSED": 5,
		"REASON_OVERSIZE":               6,
		"REASON_QUOTA_EXCEEDED":         7,
		"REASON_RATE_LIMITED":           8,
		"REASON_RETENTION_CLAMPED":      9,
		"REASON_BLOB_UNKNOWN":           10,
		"REASON_BLOB_INCOMPLETE":        11,
		"REASON_UNSUPPORTED_VERSION":    12,
		"REASON_INTERNAL":               13,
		"REASON_EPOCH_INCOMPLETE":       14,
		"REASON_WRAP_TARGET_UNKNOWN":    15,
		"REASON_CARD_RETIRED":           16,
		"REASON_CARD_RATE_LIMITED":      17,
	},
	// Spec B §4.3.10, declared on one line.
	"Direction": {
		"DIRECTION_UNSPECIFIED": 0,
		"DIRECTION_UPLOAD":      1,
		"DIRECTION_DOWNLOAD":    2,
	},
}

// messageProtoEnums returns every enum declared in message.proto, top level and
// nested, keyed by simple name. Derived, so a new enum is gated the moment it is
// added rather than when somebody remembers it.
func messageProtoEnums(t *testing.T) map[string]protoreflect.EnumDescriptor {
	t.Helper()
	fd := (*protocol.MessageServerRequest)(nil).ProtoReflect().Descriptor().ParentFile()
	out := map[string]protoreflect.EnumDescriptor{}
	var collectEnums func(protoreflect.EnumDescriptors)
	collectEnums = func(eds protoreflect.EnumDescriptors) {
		for i := 0; i < eds.Len(); i++ {
			ed := eds.Get(i)
			out[string(ed.Name())] = ed
		}
	}
	var walk func(protoreflect.MessageDescriptors)
	walk = func(mds protoreflect.MessageDescriptors) {
		for i := 0; i < mds.Len(); i++ {
			md := mds.Get(i)
			collectEnums(md.Enums())
			walk(md.Messages())
		}
	}
	collectEnums(fd.Enums())
	walk(fd.Messages())
	return out
}

// TestMessageProtoEnumSetMatchesSpecB is the key-set equality that keeps the
// numbering assertions below from going stale, the same construction
// TestRequestArmSetMatchesSpecA57 uses for the arms.
func TestMessageProtoEnumSetMatchesSpecB(t *testing.T) {
	enums := messageProtoEnums(t)
	if len(enums) == 0 {
		t.Fatal("message.proto declares no enum at all; the descriptor walk is wrong")
	}
	for name := range enums {
		if _, ok := specBEnums[name]; !ok {
			t.Errorf("message.proto declares enum %s, which is not transcribed in specBEnums. "+
				"An enum's value numbers go on the wire; record them against the spec rather "+
				"than letting them default.", name)
		}
	}
	for name := range specBEnums {
		if _, ok := enums[name]; !ok {
			t.Errorf("specBEnums transcribes enum %s, which message.proto no longer declares", name)
		}
	}
}

// TestEnumValueNumbersMatchSpecB compares every value of every enum in
// message.proto against the transcription, in both directions.
func TestEnumValueNumbersMatchSpecB(t *testing.T) {
	enums := messageProtoEnums(t)
	names := make([]string, 0, len(enums))
	for name := range enums {
		names = append(names, name)
	}
	sort.Strings(names)

	checked := 0
	for _, name := range names {
		ed := enums[name]
		want, ok := specBEnums[name]
		if !ok {
			// Reported by TestMessageProtoEnumSetMatchesSpecB.
			continue
		}
		t.Run(name, func(t *testing.T) {
			values := ed.Values()
			seen := map[string]bool{}
			for i := 0; i < values.Len(); i++ {
				v := values.Get(i)
				vname := string(v.Name())
				seen[vname] = true
				checked++
				spec, ok := want[vname]
				if !ok {
					t.Errorf("%s.%s = %d is not in Spec B's declaration of the enum. A value the "+
						"spec does not define is a code point no other implementation will "+
						"recognise.", name, vname, v.Number())
					continue
				}
				if int(v.Number()) != spec {
					t.Errorf("%s.%s = %d; Spec B numbers it %d. Enum values are encoded as their "+
						"numbers: a renumbering means one implementation's %s decodes as a "+
						"different value entirely on the other side.", name, vname, v.Number(), spec, vname)
				}
			}
			for vname, n := range want {
				if !seen[vname] {
					t.Errorf("Spec B declares %s.%s = %d, which message.proto does not", name, vname, n)
				}
			}
			if values.Len() != len(want) {
				t.Errorf("%s has %d values, Spec B declares %d", name, values.Len(), len(want))
			}
		})
	}
	if checked == 0 {
		t.Fatal("compared no enum values at all")
	}
	t.Logf("compared %d enum values across %d enums against Spec B", checked, len(names))
}

// TestEnumZeroValuesAreDistinct is a small structural check that costs nothing and
// makes the REASON_OK hazard above legible instead of invisible: it reports, for
// every enum in message.proto, which value owns 0. proto3 gives an unset or
// truncated field that value, so it is the value every decoder falls back to.
func TestEnumZeroValuesAreDistinct(t *testing.T) {
	enums := messageProtoEnums(t)
	names := make([]string, 0, len(enums))
	for name := range enums {
		names = append(names, name)
	}
	sort.Strings(names)

	var report []string
	for _, name := range names {
		ed := enums[name]
		zero := ed.Values().ByNumber(0)
		if zero == nil {
			t.Errorf("enum %s has no value 0; proto3 requires the first value to be zero", name)
			continue
		}
		if want, ok := specBEnums[name]; ok {
			if got, ok := want[string(zero.Name())]; !ok || got != 0 {
				t.Errorf("enum %s: value 0 is %s, which Spec B does not number 0", name, zero.Name())
			}
		}
		report = append(report, fmt.Sprintf("%s=%s", name, zero.Name()))
	}
	t.Logf("the value an unset or truncated field decodes as, per enum: %s", strings.Join(report, " "))
}
