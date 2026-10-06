// Original chain admission uses independently signed tuples, not counter labels.
package protocol

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"testing"
)

func inventoryTestOriginal(t *testing.T) (OriginalCloseReport, ed25519.PrivateKey) {
	t.Helper()
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{71}, 32))
	value, err := SignOriginalCloseReport(OriginalCloseReport{DomainHash: [32]byte{1}, ClientId: [16]byte{2}, ContractId: [16]byte{3}, ReportId: [16]byte{4}, AckedByteCount: 121, UnackedByteCount: 7}, key)
	if err != nil {
		t.Fatal(err)
	}
	return value, key
}

func TestOriginalInventoryRoundTripRetainsIndependentLegacyReport(t *testing.T) {
	original, key := inventoryTestOriginal(t)
	raw, _ := original.Bytes()
	value, err := SignOriginalCloseInventory(OriginalCloseInventory{DomainHash: original.DomainHash, ClientId: original.ClientId, ContractId: original.ContractId, ReportHash: sha256.Sum256(raw), Sequence: 1, CumulativeAckedBytes: 121, Terminal: true}, key)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := value.Bytes()
	decoded, decodeErr := DecodeOriginalCloseInventory(wire)
	if err != nil || decodeErr != nil || decoded != value || !decoded.Matches(original) || len(raw) != OriginalCloseReportBytes || !bytes.HasPrefix(raw, append([]byte(OriginalCloseReportSchema), 0)) {
		t.Fatal("independent inventory changed legacy original or its exact tuple", err, decodeErr)
	}
	for _, change := range []func(*OriginalCloseReport){func(v *OriginalCloseReport) { v.DomainHash[0]++ }, func(v *OriginalCloseReport) { v.ClientId[0]++ }, func(v *OriginalCloseReport) { v.ContractId[0]++ }, func(v *OriginalCloseReport) { v.ReportId[0]++ }, func(v *OriginalCloseReport) { v.AckedByteCount++ }, func(v *OriginalCloseReport) { v.Checkpoint = true }} {
		other := original
		change(&other)
		other, err = SignOriginalCloseReport(other, key)
		if err != nil || decoded.Matches(other) {
			t.Fatal("inventory admitted another original", err)
		}
	}
}

func TestOriginalInventoryRejectsMalformedAndForeignPredecessors(t *testing.T) {
	original, key := inventoryTestOriginal(t)
	raw, _ := original.Bytes()
	valid := OriginalCloseInventory{DomainHash: original.DomainHash, ClientId: original.ClientId, ContractId: original.ContractId, ReportHash: sha256.Sum256(raw), Sequence: 1, CumulativeAckedBytes: 121, Terminal: true}
	for _, change := range []func(*OriginalCloseInventory){func(v *OriginalCloseInventory) { v.Sequence = 0 }, func(v *OriginalCloseInventory) { v.Sequence = 2 }, func(v *OriginalCloseInventory) { v.Previous[0] = 1 }, func(v *OriginalCloseInventory) { v.Sequence = 1025; v.Previous[0] = 1 }} {
		v := valid
		change(&v)
		if _, err := SignOriginalCloseInventory(v, key); err == nil {
			t.Fatal("invalid original inventory predecessor was signed")
		}
	}
	v, err := SignOriginalCloseInventory(valid, key)
	if err != nil {
		t.Fatal(err)
	}
	wire, _ := v.Bytes()
	for _, bad := range [][]byte{wire[:len(wire)-1], append(bytes.Clone(wire), 0), func() []byte { v := bytes.Clone(wire); v[len(v)-1] ^= 1; return v }()} {
		if _, err := DecodeOriginalCloseInventory(bad); err == nil {
			t.Fatal("malformed original inventory admitted")
		}
	}
}
