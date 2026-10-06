package protocol_test

// The URmessage frame code points, the part of the URmessage wire that stays in connect.
//
// message.proto, its generated code and the checks over it moved to the protocol package of
// github.com/urnetwork/message (MESSAGEREVIEW.md). The four `MessageType` code points of Spec B
// §4.2 / Spec A §10.1 are values of frame.proto's own enum, so they stay, and the tests below read
// them from frame.proto's registered descriptor and nothing else. The values stay reserved until
// the messaging carrier moves to a subprotocol and retires them.
//
// Their enum value names are deliberately diverged from both specs (see frame.proto, where the
// domain prefix is repeated to avoid a proto3 scoping collision with the messages of the same name
// in message.proto), so the numbers are the only thing still tying the block to the normative
// text. A renumbered code point is not a MAC failure; it is worse. Two peers stop recognising each
// other's frames, and the frame is discarded as an unknown message type, which is what a
// forward-compatible enum is supposed to do with a code point that does not exist yet.
//
// The collision still exists in any binary that links connect and the message module, because
// message.proto keeps its proto package and its message names. The message repository checks
// that these names still diverge from those messages; here the names are pinned by the
// transcription below, and the last test checks that connect no longer registers the schema.

import (
	"errors"
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
// name both specs give it, and the message.proto message it carries (now in
// github.com/urnetwork/message/protocol).
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

// The messaging schema is registered by github.com/urnetwork/message/protocol and by nothing in
// connect. Two registrations of message.proto in one process are a conflict that protobuf-go
// panics on at init, so a copy left or restored here, for example by merging a branch from before
// the move, would stop every binary that links connect and the message module from starting.
func TestConnectRegistersNoMessagingSchema(t *testing.T) {
	// control: the same lookups find what connect does register
	if _, err := protoregistry.GlobalFiles.FindFileByPath(protocol.File_frame_proto.Path()); err != nil {
		t.Fatalf("%s is not registered, so the lookups below prove nothing: %v", protocol.File_frame_proto.Path(), err)
	}
	messageTypeEnum(t)

	if file, err := protoregistry.GlobalFiles.FindFileByPath("message.proto"); err == nil {
		t.Errorf("connect registers message.proto (proto package %s); the messaging schema belongs to github.com/urnetwork/message/protocol", file.Package())
	} else if !errors.Is(err, protoregistry.NotFound) {
		t.Errorf("looking up message.proto: %v", err)
	}
	// the messages the reserved code points carry, named as the specs name them
	for name, want := range specUrmessageCodePoints {
		full := protoreflect.FullName("bringyour." + want.specName)
		if descriptor, err := protoregistry.GlobalFiles.FindDescriptorByName(full); err == nil {
			t.Errorf("%s, which the code point %s carries, is registered here by %s; it belongs to github.com/urnetwork/message/protocol", full, name, descriptor.ParentFile().Path())
		} else if !errors.Is(err, protoregistry.NotFound) {
			t.Errorf("looking up %s: %v", full, err)
		}
	}
}
