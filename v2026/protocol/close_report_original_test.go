// Fixed original bytes and independent tuple mutations guard signature authority.
package protocol

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Synthetic fixed identities make format assertions independent of timestamps.
func originalCloseReportFixture(t testing.TB) (OriginalCloseReport, ed25519.PrivateKey) {
	t.Helper()
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{29}, ed25519.SeedSize))
	report := OriginalCloseReport{
		DomainHash: [32]byte(bytes.Repeat([]byte{1}, 32)), ClientId: [16]byte(bytes.Repeat([]byte{2}, 16)),
		ContractId: [16]byte(bytes.Repeat([]byte{3}, 16)), ReportId: [16]byte(bytes.Repeat([]byte{4}, 16)),
		AckedByteCount: 0x0102030405060708, UnackedByteCount: ^uint64(0), Checkpoint: true,
	}
	signed, err := SignOriginalCloseReport(report, key)
	if err != nil {
		t.Fatal(err)
	}
	return signed, key
}

// Explicit byte positions, integer encoding and legacy wire bytes are a format
// oracle independent of encoding and decoding through the same implementation.
func TestOriginalCloseReportCanonicalWireAndLegacyGolden(t *testing.T) {
	report, key := originalCloseReportFixture(t)
	wire, err := report.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("urnetwork-original-close-report-v1\x00")
	want = append(want, bytes.Repeat([]byte{1}, 32)...)
	want = append(want, bytes.Repeat([]byte{2}, 16)...)
	want = append(want, bytes.Repeat([]byte{3}, 16)...)
	want = append(want, bytes.Repeat([]byte{4}, 16)...)
	want = append(want, key[32:]...)
	want = append(want, 1, 2, 3, 4, 5, 6, 7, 8, 255, 255, 255, 255, 255, 255, 255, 255, 1)
	if len(wire) != OriginalCloseReportBytes || !bytes.Equal(wire[:len(wire)-64], want) || !ed25519.Verify(ed25519.PublicKey(key[32:]), want, wire[len(wire)-64:]) {
		t.Fatal("original close report canonical signed bytes changed")
	}
	decoded, err := DecodeOriginalCloseReport(wire)
	if err != nil || decoded != report {
		t.Fatal("original signed report did not round trip", err)
	}
	legacy := &CloseContract{ContractId: []byte{1, 2}, AckedByteCount: 100, Checkpoint: true}
	legacyWire, err := proto.Marshal(legacy)
	if err != nil || hex.EncodeToString(legacyWire) != "0a02010210642001" {
		t.Fatal("empty original envelope changed independent legacy wire", err)
	}
	field := legacy.ProtoReflect().Descriptor().Fields().ByNumber(6)
	if field == nil || field.Name() != "original_report" || field.Kind() != protoreflect.BytesKind {
		t.Fatal("original report protobuf field differs")
	}
	legacy.OriginalReport = wire
	encoded, err := proto.Marshal(legacy)
	var restored CloseContract
	if err != nil || proto.Unmarshal(encoded, &restored) != nil || !bytes.Equal(restored.OriginalReport, wire) {
		t.Fatal("protobuf did not retain exact original envelope")
	}
}

// Every signed identity and count is causal; even equal-byte reports are distinct.
func TestOriginalCloseReportRefusesEveryForeignSignedDimension(t *testing.T) {
	report, _ := originalCloseReportFixture(t)
	for index, mutate := range []func(*OriginalCloseReport){
		func(r *OriginalCloseReport) { r.DomainHash[0]++ },
		func(r *OriginalCloseReport) { r.ClientId[0]++ },
		func(r *OriginalCloseReport) { r.ContractId[0]++ },
		func(r *OriginalCloseReport) { r.ReportId[0]++ },
		func(r *OriginalCloseReport) { r.PublicKey[0]++ },
		func(r *OriginalCloseReport) { r.AckedByteCount++ },
		func(r *OriginalCloseReport) { r.UnackedByteCount-- },
		func(r *OriginalCloseReport) { r.Checkpoint = false },
		func(r *OriginalCloseReport) { r.Signature[0]++ },
	} {
		changed := report
		mutate(&changed)
		if changed.Verify() == nil {
			t.Fatalf("foreign signed report dimension %d was admitted", index)
		}
	}
}

// No prefix, trailing bytes, unknown version or alternate boolean is accepted.
func TestOriginalCloseReportRefusesMalformedEnvelopeAndOwner(t *testing.T) {
	report, key := originalCloseReportFixture(t)
	wire, _ := report.Bytes()
	badVersion, badBool := bytes.Clone(wire), bytes.Clone(wire)
	badVersion[len(OriginalCloseReportSchema)-1] = '2'
	badBool[len(wire)-65] = 2
	for _, data := range [][]byte{nil, wire[:len(wire)-1], append(bytes.Clone(wire), 0), badVersion, badBool} {
		if _, err := DecodeOriginalCloseReport(data); err == nil {
			t.Fatal("malformed original envelope was admitted")
		}
	}
	key[32] ^= 1
	if _, err := SignOriginalCloseReport(report, key); err == nil {
		t.Fatal("malformed key owner was silently reconstructed")
	}
	report.DomainHash = [32]byte{}
	if _, err := SignOriginalCloseReport(report, ed25519.NewKeyFromSeed(bytes.Repeat([]byte{30}, 32))); err == nil {
		t.Fatal("missing policy domain was signed")
	}
}

// An authenticated envelope cannot replace the transport's owner or outer tuple.
func TestOriginalCloseReportMatchesOnlyActualTransportTuple(t *testing.T) {
	report, _ := originalCloseReportFixture(t)
	outer := &CloseContract{ContractId: report.ContractId[:], ReportId: report.ReportId[:], AckedByteCount: report.AckedByteCount, UnackedByteCount: report.UnackedByteCount, Checkpoint: report.Checkpoint}
	if !report.Matches(report.ClientId, outer) {
		t.Fatal("exact original transport tuple did not join")
	}
	for _, mutate := range []func(*CloseContract){
		func(r *CloseContract) { r.ContractId = bytes.Repeat([]byte{7}, 16) },
		func(r *CloseContract) { r.ReportId = bytes.Repeat([]byte{8}, 16) },
		func(r *CloseContract) { r.AckedByteCount++ },
		func(r *CloseContract) { r.UnackedByteCount-- },
		func(r *CloseContract) { r.Checkpoint = false },
	} {
		changed := proto.Clone(outer).(*CloseContract)
		mutate(changed)
		if report.Matches(report.ClientId, changed) {
			t.Fatal("original signature authorized a different transport tuple")
		}
	}
	foreign := report.ClientId
	foreign[0]++
	if report.Matches(foreign, outer) || report.Matches(report.ClientId, nil) {
		t.Fatal("original report selected another authenticated client")
	}
}
