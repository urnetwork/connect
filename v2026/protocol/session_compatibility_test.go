package protocol

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

func TestSessionProtocolAllocationsAndRoundTrip(t *testing.T) {
	if MessageType_TransferNetworkSessionsChanged != 33 || MessageType_TransferStreamAuthorization != 34 {
		t.Fatal("session protocol enum allocation changed")
	}
	source := &Auth{ByJwt: "jwt", ClientInfo: `{"v":1,"device_type":"ios","app_version":"1"}`, StreamLeaseVersion: 1}
	fields := source.ProtoReflect().Descriptor().Fields()
	if fields.ByName("client_info").Number() != 8 || fields.ByName("stream_lease_version").Number() != 9 {
		t.Fatal("auth wire fields changed")
	}
	bytes, err := proto.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	var target Auth
	if err = proto.Unmarshal(bytes, &target); err != nil || !proto.Equal(source, &target) {
		t.Fatal("auth metadata did not round trip", err)
	}
}

func TestSessionLeaseWireFieldsAndLegacyAuth(t *testing.T) {
	for _, test := range []struct {
		message proto.Message
		fields  map[protoreflect.Name]protoreflect.FieldNumber
	}{
		{&StreamOpen{}, map[protoreflect.Name]protoreflect.FieldNumber{"authorization_generation": 4, "authorization_lease_millis": 5, "authorization_deadline_unix_millis": 6}},
		{&StreamClose{}, map[protoreflect.Name]protoreflect.FieldNumber{"authorization_generation": 4}},
		{&NetworkSessionsChanged{}, map[protoreflect.Name]protoreflect.FieldNumber{"generation": 1, "event_id": 2}},
		{&StreamAuthorization{}, map[protoreflect.Name]protoreflect.FieldNumber{"stream_id": 1, "authorization_generation": 2, "lease_millis": 3, "retired": 4, "clock_id": 5, "clock_unix_millis": 6, "deadline_unix_millis": 7}},
	} {
		fields := test.message.ProtoReflect().Descriptor().Fields()
		for name, number := range test.fields {
			field := fields.ByName(name)
			if field == nil || field.Number() != number {
				t.Fatalf("%T.%s changed its allocated field %d", test.message, name, number)
			}
		}
	}

	// Reconstruct the pre-session Auth descriptor. Old peers can still read
	// known authentication fields while treating appended capabilities as unknown.
	oldAuth := protodesc.ToDescriptorProto((&Auth{}).ProtoReflect().Descriptor())
	oldAuth.Field = oldAuth.Field[:7]
	file, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name: proto.String("legacy_auth.proto"), Package: proto.String("legacy"), Syntax: proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{oldAuth},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := proto.Marshal(&Auth{ByJwt: "credential", AppVersion: "legacy", ClientInfo: `{"v":1}`, StreamLeaseVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	legacy := dynamicpb.NewMessage(file.Messages().Get(0))
	if err := proto.Unmarshal(encoded, legacy); err != nil {
		t.Fatal(err)
	}
	if got := legacy.Get(legacy.Descriptor().Fields().ByName("by_jwt")).String(); got != "credential" || len(legacy.GetUnknown()) == 0 {
		t.Fatal("appended fields broke legacy Auth decoding")
	}
	oldBytes, err := proto.Marshal(dynamicpb.NewMessage(file.Messages().Get(0)))
	if err != nil {
		t.Fatal(err)
	}
	var current Auth
	if err := proto.Unmarshal(oldBytes, &current); err != nil || current.StreamLeaseVersion != 0 || current.ClientInfo != "" {
		t.Fatal("legacy Auth falsely advertises session capabilities", err)
	}
}
