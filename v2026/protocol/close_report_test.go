package protocol

import (
	"bytes"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestCloseReportOptionalIdentityWireCompatibility(t *testing.T) {
	legacy := &CloseContract{ContractId: []byte{1, 2}, AckedByteCount: 100, Checkpoint: true}
	before, err := proto.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	legacy.ReportId = nil
	after, err := proto.Marshal(legacy)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("empty identity changed legacy wire")
	}
	f := legacy.ProtoReflect().Descriptor().Fields().ByNumber(5)
	if f == nil || f.Name() != "report_id" || f.Kind() != protoreflect.BytesKind {
		t.Fatal("optional field identity changed")
	}
	legacy.ReportId = bytes.Repeat([]byte{7}, 16)
	wire, err := proto.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	decoded := new(CloseContract)
	if err := proto.Unmarshal(wire, decoded); err != nil || !proto.Equal(legacy, decoded) {
		t.Fatal("report identity did not survive protobuf")
	}
}
