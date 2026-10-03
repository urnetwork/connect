//go:build linux

// Retained file bytes are insufficient when owner acknowledgement lives in
// descriptor attributes. These controls keep the real directory and file data.
package durablevolume

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// JSON keeps this exact control compilable against the prior limits struct.
// The old implementation ignores the new finite metadata limits, never the test.
func inventoryCustodyLimits(t testing.TB) InventoryLimits {
	t.Helper()
	var limits InventoryLimits
	if err := json.Unmarshal([]byte(`{"max_entries":16,"max_bytes":4096,"max_depth":4,"max_owner_attributes":16,"max_owner_attribute_bytes":16384}`), &limits); err != nil {
		t.Fatal(err)
	}
	return limits
}

func TestInventoryRetainsOwnerCustodyMetadata(t *testing.T) {
	fixture := newVolumeFixture(t)
	fixture.custody(t)
	values := []struct {
		path, name string
		raw        []byte
	}{
		{fixture.root, "user.urnetwork.native-journal-custody", []byte(`{"pending":"original-signed-byte-census"}`)},
		{filepath.Join(fixture.root, "journal"), "user.urnetwork.attempt-ledger-custody", []byte(`{"committed":"acknowledged-logical-head"}`)},
		{filepath.Join(fixture.root, "journal", "empty-lock"), "user.urnetwork.snapshot." + strings.Repeat("a", 64), []byte(`{"pending":"actual-retained-temporary-member"}`)},
	}
	for _, value := range values {
		if err := syscall.Setxattr(value.path, value.name, value.raw, 1); err != nil {
			t.Fatal(err)
		}
	}
	owner := fixture.open(t, Snapshot)
	report, err := owner.Inventory(t.Context(), fixture.fence(t), inventoryCustodyLimits(t))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Entries []struct {
			Path            string
			OwnerAttributes []struct {
				Name   string
				Value  []byte
				Sha256 string
			} `json:"owner_attributes"`
		}
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	for _, expected := range values {
		found := false
		for _, entry := range wire.Entries {
			if filepath.Join(fixture.root, entry.Path) != expected.path {
				continue
			}
			for _, attribute := range entry.OwnerAttributes {
				if attribute.Name == expected.name && bytes.Equal(attribute.Value, expected.raw) && attribute.Sha256 == testDigest(expected.raw) {
					found = true
				}
			}
		}
		if !found {
			t.Fatal("inventory omitted retained owner custody", expected.path, expected.name)
		}
	}
	if report.RestartAuthorized {
		t.Fatal("inventory authorized a writer restart")
	}
}

func TestVerifyInventoryRefusesLostOwnerCustodyMetadata(t *testing.T) {
	fixture := newVolumeFixture(t)
	fixture.custody(t)
	path, name := filepath.Join(fixture.root, "journal", "empty-lock"), "user.urnetwork.snapshot."+strings.Repeat("b", 64)
	original := []byte(`{"committed":"retained-completed-history"}`)
	if err := syscall.Setxattr(path, name, original, 1); err != nil {
		t.Fatal(err)
	}
	owner := fixture.open(t, Snapshot)
	fence := fixture.fence(t)
	limits := inventoryCustodyLimits(t)
	report, err := owner.Inventory(t.Context(), fence, limits)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	retained := filepath.Join(filepath.Dir(fixture.mount), "owner-custody-inventory.json")
	if err := os.WriteFile(retained, raw, 0600); err != nil {
		t.Fatal(err)
	}
	expected := Reference{Path: retained, Sha256: testDigest(raw)}
	if err := syscall.Removexattr(path, name); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.VerifyInventory(t.Context(), expected, fence, limits); err == nil {
		t.Fatal("missing acknowledged owner anchor passed exact restore verification")
	}
	if err := syscall.Setxattr(path, name, []byte(`{"committed":"different-history"}`), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.VerifyInventory(t.Context(), expected, fence, limits); err == nil {
		t.Fatal("different acknowledged owner anchor passed exact restore verification")
	}
	if err := syscall.Setxattr(path, name, original, 2); err != nil {
		t.Fatal(err)
	}
	verified, err := owner.VerifyInventory(t.Context(), expected, fence, limits)
	if err != nil || !verified.ExactLocalBytesAndMetadata || verified.RestartAuthorized {
		t.Fatal("exact metadata readback differs or authorizes restart", verified, err)
	}
}

func TestInventoryRefusesUnknownOwnerCustodyMetadata(t *testing.T) {
	fixture := newVolumeFixture(t)
	fixture.custody(t)
	if err := syscall.Setxattr(fixture.root, "user.urnetwork.future-custody-owner", []byte("unrecognized-retained-authority"), 1); err != nil {
		t.Fatal(err)
	}
	owner := fixture.open(t, Snapshot)
	if _, err := owner.Inventory(t.Context(), fixture.fence(t), inventoryCustodyLimits(t)); err == nil {
		t.Fatal("unknown custody namespace was silently omitted from complete inventory")
	}
}
