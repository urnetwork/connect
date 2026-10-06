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
// transcription below. The last two tests check that connect registers neither the schema nor any
// name the schema declared.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

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

// The package-scoped full names message.proto declared when it left connect: its 52 messages, its
// 2 enums, and the 21 values of those enums, which proto3 scopes to the proto package and not to
// the enum. They are transcribed from the descriptor connect b65ce856 registered for message.proto
// (sha256 fba4eba3...b302), and a text reading of that file gives the same 75. It declares no
// service, no extension and no nested type, so no other name of it can collide except through one
// of these.
//
// message.proto keeps proto package bringyour in github.com/urnetwork/message/protocol, and so do
// connect's own proto files. Before the move, protoc compiled message.proto together with them
// (protocol/Makefile builds *.proto), so a second declaration of one of these names failed the
// generation. Now nothing in connect sees message.proto, and this list is what holds the rule.
// The message repository's schema is append-only, so a name it adds later is held there, by its
// single-registration check and by the init of every binary that links both modules.
var messagingSchemaNames = []protoreflect.FullName{
	// messages
	"bringyour.Backpressure",
	"bringyour.BlobEndpoint",
	"bringyour.BlobGrantRequest",
	"bringyour.BlobGrantResponse",
	"bringyour.Capabilities",
	"bringyour.CapabilityChange",
	"bringyour.CreateGroupRequest",
	"bringyour.CreateGroupResponse",
	"bringyour.Drain",
	"bringyour.EpochKeyDelivery",
	"bringyour.FetchAttestation",
	"bringyour.FetchRequest",
	"bringyour.FetchResponse",
	"bringyour.GroupRecords",
	"bringyour.GroupStatusRequest",
	"bringyour.GroupStatusResponse",
	"bringyour.HelloRequest",
	"bringyour.HelloResponse",
	"bringyour.KtGossip",
	"bringyour.MessageServerFragment",
	"bringyour.MessageServerPush",
	"bringyour.MessageServerRequest",
	"bringyour.MessageServerResponse",
	"bringyour.Record",
	"bringyour.RecordPush",
	"bringyour.RecoveryFetchRequest",
	"bringyour.RecoveryFetchResponse",
	"bringyour.RendezvousCollectRequest",
	"bringyour.RendezvousCollectResponse",
	"bringyour.RendezvousDeposit",
	"bringyour.RendezvousDepositRequest",
	"bringyour.RendezvousDepositResponse",
	"bringyour.RendezvousOpenRequest",
	"bringyour.RendezvousOpenResponse",
	"bringyour.RendezvousPush",
	"bringyour.RendezvousRegisterRequest",
	"bringyour.RendezvousRegisterResponse",
	"bringyour.RendezvousRetireRequest",
	"bringyour.RendezvousRetireResponse",
	"bringyour.RetentionApplied",
	"bringyour.ServerKey",
	"bringyour.SubmitRequest",
	"bringyour.SubmitResponse",
	"bringyour.SubmitResult",
	"bringyour.SubscribeRequest",
	"bringyour.SubscribeResponse",
	"bringyour.Subscription",
	"bringyour.SubscriptionAck",
	"bringyour.TransientPush",
	"bringyour.UnsubscribeRequest",
	"bringyour.WrapFetchRequest",
	"bringyour.WrapFetchResponse",
	// enums
	"bringyour.Direction",
	"bringyour.Reason",
	// the values of those enums
	"bringyour.DIRECTION_DOWNLOAD",
	"bringyour.DIRECTION_UNSPECIFIED",
	"bringyour.DIRECTION_UPLOAD",
	"bringyour.REASON_BLOB_INCOMPLETE",
	"bringyour.REASON_BLOB_UNKNOWN",
	"bringyour.REASON_CARD_RATE_LIMITED",
	"bringyour.REASON_CARD_RETIRED",
	"bringyour.REASON_COMMIT_LOST",
	"bringyour.REASON_EPOCH_INCOMPLETE",
	"bringyour.REASON_EPOCH_STALE",
	"bringyour.REASON_INTERNAL",
	"bringyour.REASON_OK",
	"bringyour.REASON_OVERSIZE",
	"bringyour.REASON_QUOTA_EXCEEDED",
	"bringyour.REASON_RATE_LIMITED",
	"bringyour.REASON_REJECTED",
	"bringyour.REASON_RETENTION_CLAMPED",
	"bringyour.REASON_STREAM_INDEX_REGRESSED",
	"bringyour.REASON_STREAM_INDEX_REUSED",
	"bringyour.REASON_UNSUPPORTED_VERSION",
	"bringyour.REASON_WRAP_TARGET_UNKNOWN",
}

// The members of messagingSchemaNames that files declares, each with the file declaring it, sorted.
func messagingSchemaNamesDeclaredIn(t *testing.T, files *protoregistry.Files) []string {
	t.Helper()
	declared := []string{}
	for _, name := range messagingSchemaNames {
		descriptor, err := files.FindDescriptorByName(name)
		switch {
		case err == nil:
			declared = append(declared, fmt.Sprintf("%s is declared by %s", name, descriptor.ParentFile().Path()))
		case !errors.Is(err, protoregistry.NotFound):
			t.Errorf("looking up %s: %v", name, err)
		}
	}
	slices.Sort(declared)
	return declared
}

// The messaging schema is registered by github.com/urnetwork/message/protocol and by nothing in
// connect. Two registrations of message.proto in one process are a conflict that protobuf-go
// panics on at init, so a copy left or restored here, for example by merging a branch from before
// the move, would stop every binary that links connect and the message module from starting.
//
// So would a connect proto file that declared any name of messagingSchemaNames, and no connect
// test would notice, because no connect binary links the message module. The names are looked up
// in every proto file this repository holds: the files are found by walking the repository, and
// each has to be one this binary registers, so a proto file this package does not generate cannot
// declare a name unread.
func TestConnectRegistersNoMessagingSchema(t *testing.T) {
	// control: the same lookups find what connect does register
	if _, err := protoregistry.GlobalFiles.FindFileByPath(protocol.File_frame_proto.Path()); err != nil {
		t.Fatalf("%s is not registered, so the lookups below prove nothing: %v", protocol.File_frame_proto.Path(), err)
	}
	messageTypeEnum(t)

	// the scope: every proto file of the repository, against the files this binary registers in
	// proto package bringyour
	root, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolve this package's own directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			t.Fatal("no go.mod above this package, so the repository holding its proto files cannot be found")
		}
		root = parent
	}
	protoFiles := []string{}
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".proto" {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		protoFiles = append(protoFiles, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s for proto files: %v", root, err)
	}
	registered := []string{}
	protoregistry.GlobalFiles.RangeFilesByPackage("bringyour", func(file protoreflect.FileDescriptor) bool {
		registered = append(registered, "protocol/"+file.Path())
		return true
	})
	slices.Sort(protoFiles)
	slices.Sort(registered)
	if !slices.Contains(protoFiles, "protocol/"+protocol.File_frame_proto.Path()) {
		t.Fatalf("the walk of %s found the proto files %v, which do not hold frame.proto, so it did not read this repository", root, protoFiles)
	}
	for _, file := range protoFiles {
		if !slices.Contains(registered, file) {
			t.Errorf("%s is a proto file this binary does not register, so the names it declares are not looked up below; generate it into this package", file)
		}
	}
	for _, file := range registered {
		if !slices.Contains(protoFiles, file) {
			t.Errorf("this binary registers %s in proto package bringyour, and the repository holds no such proto file", file)
		}
	}
	t.Logf("the names are looked up in the %d proto files of this repository: %v", len(protoFiles), protoFiles)

	if file, err := protoregistry.GlobalFiles.FindFileByPath("message.proto"); err == nil {
		t.Errorf("connect registers message.proto (proto package %s); the messaging schema belongs to github.com/urnetwork/message/protocol", file.Package())
	} else if !errors.Is(err, protoregistry.NotFound) {
		t.Errorf("looking up message.proto: %v", err)
	}
	for _, declared := range messagingSchemaNamesDeclaredIn(t, protoregistry.GlobalFiles) {
		t.Errorf("%s; message.proto declared that name, and the two schemas share proto package bringyour, so a binary that links connect and github.com/urnetwork/message/protocol panics at init on the second declaration", declared)
	}
	t.Logf("looked up the %d names message.proto declared", len(messagingSchemaNames))
}

// Controls the name check against a registry built here, in both directions: a file in proto
// package bringyour that declares a message and an enum value of messagingSchemaNames is reported
// for exactly those two, and its own names are not. Then holds the transcription to what its
// comment says it is.
func TestTheMessagingNameCheckFindsAPlantedDeclaration(t *testing.T) {
	planted, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:    proto.String("planted.proto"),
		Package: proto.String("bringyour"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("Capabilities")},
			{Name: proto.String("PlantedMessage")},
		},
		EnumType: []*descriptorpb.EnumDescriptorProto{
			{
				Name: proto.String("PlantedReason"),
				Value: []*descriptorpb.EnumValueDescriptorProto{
					{Name: proto.String("REASON_OK"), Number: proto.Int32(0)},
					{Name: proto.String("PLANTED_REASON_OTHER"), Number: proto.Int32(1)},
				},
			},
		},
	}, nil)
	if err != nil {
		t.Fatalf("build the planted file: %v", err)
	}
	files := &protoregistry.Files{}
	if err := files.RegisterFile(planted); err != nil {
		t.Fatalf("register the planted file: %v", err)
	}
	want := []string{
		"bringyour.Capabilities is declared by planted.proto",
		"bringyour.REASON_OK is declared by planted.proto",
	}
	if got := messagingSchemaNamesDeclaredIn(t, files); !slices.Equal(got, want) {
		t.Errorf("the name check over the planted file reported %q, want %q", got, want)
	}

	// the transcription: 75 distinct names, each directly in proto package bringyour, holding every
	// message a reserved code point carries
	if len(messagingSchemaNames) != 75 {
		t.Errorf("the transcription holds %d names; message.proto declared 75 (52 messages, 2 enums, 21 enum values)", len(messagingSchemaNames))
	}
	seen := map[protoreflect.FullName]bool{}
	for _, name := range messagingSchemaNames {
		if seen[name] {
			t.Errorf("%s is transcribed twice", name)
		}
		seen[name] = true
		if !name.IsValid() || name.Parent() != "bringyour" {
			t.Errorf("%s is not a name directly in proto package bringyour", name)
		}
	}
	for name, want := range specUrmessageCodePoints {
		if !seen[protoreflect.FullName("bringyour."+want.specName)] {
			t.Errorf("the code point %s carries bringyour.%s, which the transcription does not hold", name, want.specName)
		}
	}
}
