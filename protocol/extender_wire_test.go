package protocol

// The record's original binary rendezvous tag must never be interpreted as
// the later canary channel string, including client ids containing non-utf8.

import (
	"bytes"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// This also runs against the valid pre-integration canary-only bindings:
// reusing field 14 for the string rejects an ordinary binary client id.
func TestExtenderRecordWireClientIdCannotBecomeCanary(t *testing.T) {
	clientId := bytes.Repeat([]byte{0xff, 0x80, 0x00, 0xfe}, 4)
	wire := protowire.AppendBytes(protowire.AppendTag(nil, 14, protowire.BytesType), clientId)
	var body ExtenderRecordBody
	if err := proto.Unmarshal(wire, &body); err != nil {
		t.Fatalf("binary field 14 failed to decode: %v", err)
	}
	if body.CanaryChannel != "" {
		t.Fatalf("binary client id became canary channel %q", body.CanaryChannel)
	}
}
