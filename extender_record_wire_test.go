package connect

// These wire fixtures keep independent record extensions on their assigned
// tags; client ids intentionally include bytes that cannot be a proto string.

import (
	"bytes"
	"crypto/ed25519"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/urnetwork/connect/protocol"
)

// Tags 14 and 15 were published before CanaryChannel; adding a string on 14
// either rejects the binary client id as invalid utf8 or aliases its meaning.
func TestExtenderRecordAdditionalFieldsKeepWireNumbers(t *testing.T) {
	clientId := bytes.Repeat([]byte{0xff, 0x80, 0x00, 0xfe}, 4)
	realityKey := bytes.Repeat([]byte{0xa5}, 32)
	body := &protocol.ExtenderRecordBody{WebRtcClientId: clientId, RealityPublicKey: realityKey, CanaryChannel: ExtenderChannelDns}
	fields := []struct {
		name   protoreflect.Name
		number protowire.Number
		value  []byte
	}{
		{name: "WebRtcClientId", number: 14, value: clientId},
		{name: "RealityPublicKey", number: 15, value: realityKey},
		{name: "CanaryChannel", number: 16, value: []byte(ExtenderChannelDns)},
	}
	var expected []byte
	for _, field := range fields {
		descriptor := body.ProtoReflect().Descriptor().Fields().ByName(field.name)
		if descriptor == nil || descriptor.Number() != field.number {
			t.Fatalf("%s does not use field %d", field.name, field.number)
		}
		expected = protowire.AppendBytes(protowire.AppendTag(expected, field.number, protowire.BytesType), field.value)
	}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, expected) {
		t.Fatalf("record extension bytes = %x, want %x", encoded, expected)
	}
	var decoded protocol.ExtenderRecordBody
	if err := proto.Unmarshal(expected, &decoded); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(body, &decoded) {
		t.Fatal("binary client id, reality key and canary channel did not remain independent")
	}
}

// A signed record survives its opaque wire envelope with all three fields,
// while the canary restriction alone keeps it out of open samples and streams.
func TestExtenderRecordWireCanaryStaysOutOfOpenDiscovery(t *testing.T) {
	clock := newTestClock()
	directory, rootPrivateKey := newTestExtenderDirectory(t, clock, nil)
	messages, unsubscribe := directory.Subscribe()
	defer unsubscribe()
	extenderPrivateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x5a}, ed25519.SeedSize))
	body := &protocol.ExtenderRecordBody{
		PublicKey:        extenderPrivateKey.Public().(ed25519.PublicKey),
		Addresses:        []*protocol.ExtenderAddress{testExtenderAddress("192.0.2.10")},
		TcpPort:          443,
		IssueTimeMs:      uint64(clock.Now().UnixMilli()),
		ExpireTimeMs:     uint64(clock.Now().Add(time.Hour).UnixMilli()),
		NetworkHost:      testExtenderNetworkHost,
		WebRtcClientId:   bytes.Repeat([]byte{0xff, 0x80, 0x00, 0xfe}, 4),
		RealityPublicKey: bytes.Repeat([]byte{0xa5}, 32),
		CanaryChannel:    ExtenderChannelDns,
	}
	record, err := SignExtenderRecord(rootPrivateKey, body)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := proto.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var decoded protocol.ExtenderRecord
	if err := proto.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	verified, err := directory.RootKeys().VerifyRecord(&decoded)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(verified, body) {
		t.Fatal("signed wire record changed its extensions")
	}
	if ExtenderRecordOpen(verified) {
		t.Fatal("canary was classified open")
	}
	if _, err := directory.ApplyRecord(&decoded, ExtenderSourceDns); err != nil {
		t.Fatal(err)
	}
	if records := directory.SampleRecords(8, nil, []byte("synthetic-vantage")); len(records) != 0 {
		t.Fatal("signed canary reached the open sample")
	}
	select {
	case <-messages:
		t.Fatal("signed canary reached the open stream")
	default:
	}

	ordinary := proto.Clone(body).(*protocol.ExtenderRecordBody)
	ordinary.PublicKey = newTestExtenderKey(t)
	ordinary.CanaryChannel = ""
	ordinaryRecord, err := SignExtenderRecord(rootPrivateKey, ordinary)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := directory.ApplyRecord(ordinaryRecord, ExtenderSourceDns); err != nil {
		t.Fatal(err)
	}
	select {
	case message := <-messages:
		if !proto.Equal(message.GetRecord(), ordinaryRecord) {
			t.Fatal("open stream changed the ordinary record")
		}
	default:
		t.Fatal("ordinary record did not reach the open stream")
	}
	if records := directory.SampleRecords(8, nil, []byte("synthetic-vantage")); len(records) != 1 {
		t.Fatalf("open sample has %d ordinary records, want 1", len(records))
	}
}
