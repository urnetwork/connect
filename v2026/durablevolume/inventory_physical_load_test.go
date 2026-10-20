//go:build linux || darwin

// Exact physical export is a new authority format. A byte-only historical
// report, changed totals or invented original generation cannot substitute.
package durablevolume

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The caller separately reviews and supplies the full immutable report digest.
func physicalInventoryReference(t *testing.T, fixture *volumeFixture, report Inventory) Reference {
	t.Helper()
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(fixture.mount), "physical-source.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return Reference{Path: path, Sha256: testDigest(raw)}
}

// Structural authority checks run before an adapter can select retained work.
func TestPhysicalInventoryLoaderRequiresCompleteOriginalAuthority(t *testing.T) {
	for _, mode := range []string{"original", "legacy-v3", "missing-leaf", "aliased-leaf", "total", "generation", "parent", "unknown-attribute", "attribute-hash"} {
		func() {
			fixture := newVolumeFixture(t)
			fixture.custody(t)
			owner := fixture.open(t, Snapshot)
			limits := InventoryLimits{MaxEntries: 16, MaxBytes: 4096, MaxDepth: 4, MaxOwnerAttributes: 16, MaxOwnerAttributeBytes: 16384}
			report, err := owner.InventoryPhysical(t.Context(), fixture.fence(t), limits)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "legacy-v3":
				report.Schema = InventorySchema
			case "missing-leaf":
				report.Entries[2].Physical = nil
			case "aliased-leaf":
				report.Entries[2].Physical = report.Entries[1].Physical
			case "total":
				report.TotalBytes++
			case "generation":
				report.RootGeneration = "00"
			case "parent":
				report.Entries[2].Path = "unknown/member"
			case "unknown-attribute":
				report.Entries[0].OwnerAttributes = []InventoryAttribute{{Name: "user.urnetwork.unreviewed", Value: []byte("unknown"), Sha256: testDigest([]byte("unknown"))}}
			case "attribute-hash":
				report.Entries[0].OwnerAttributes = []InventoryAttribute{{Name: PreparationAttribute, Value: []byte("unknown"), Sha256: testDigest(nil)}}
			}
			loaded, err := LoadPhysicalInventory(t.Context(), physicalInventoryReference(t, fixture, report))
			if mode == "original" {
				if err != nil || len(loaded.Entries) != 5 {
					t.Fatal("original exact source rejected", err)
				}
			} else if err == nil || len(loaded.Entries) != 0 {
				t.Fatal("incomplete physical source admitted", mode, err)
			}
			if err := owner.CheckRead(); err != nil {
				t.Fatal("report refusal poisoned actual source", mode, err)
			}
		}()
	}
}
