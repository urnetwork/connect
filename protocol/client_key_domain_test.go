// An optional history domain extends the existing public-key wire additively.
package protocol

import (
	"bytes"
	"encoding/hex"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Independent old bytes stay exact; the new domain has one stable field number.
func TestClientKeyDomainPreservesIndependentLegacyWire(t *testing.T) {
	message := &ClientKey{PublicKey: []byte{1, 2}}
	wire, err := proto.Marshal(message)
	if err != nil || hex.EncodeToString(wire) != "0a020102" {
		t.Fatal("absent key domain changed legacy enrollment wire", err)
	}
	field := message.ProtoReflect().Descriptor().Fields().ByNumber(2)
	if field == nil || field.Name() != "history_domain_hash" || field.Kind() != protoreflect.BytesKind {
		t.Fatal("client-key history domain field identity changed")
	}
	message.HistoryDomainHash = bytes.Repeat([]byte{41}, 32)
	withDomain, err := proto.Marshal(message)
	want := append(append(bytes.Clone(wire), 0x12, 0x20), message.HistoryDomainHash...)
	var decoded ClientKey
	if err != nil || !bytes.Equal(withDomain, want) || proto.Unmarshal(withDomain, &decoded) != nil || !proto.Equal(message, &decoded) {
		t.Fatal("exact key domain did not survive actual protobuf encoding", err)
	}
}
