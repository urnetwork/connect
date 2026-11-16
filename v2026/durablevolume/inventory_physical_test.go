//go:build linux || darwin

// Physical export retains each original leaf generation needed by owner census
// rebinding. Ordinary byte inventory remains a distinct historical report.
package durablevolume

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// An interface keeps the exact old inventory source usable as a causal control;
// it does not let a missing implementation fabricate original member identity.
func physicalInventoryUnderTest(ctx context.Context, owner *Owner, fence Reference, limits InventoryLimits) (Inventory, error) {
	if exporter, ok := any(owner).(interface {
		InventoryPhysical(context.Context, Reference, InventoryLimits) (Inventory, error)
	}); ok {
		return exporter.InventoryPhysical(ctx, fence, limits)
	}
	return owner.Inventory(ctx, fence, limits)
}

// Every exported device/inode must be read from its original protected member.
func TestPhysicalInventoryRetainsEveryOriginalMemberGeneration(t *testing.T) {
	fixture := newVolumeFixture(t)
	fixture.custody(t)
	owner := fixture.open(t, Snapshot)
	limits := InventoryLimits{MaxEntries: 16, MaxBytes: 4096, MaxDepth: 4, MaxOwnerAttributes: 16, MaxOwnerAttributeBytes: 16384}
	report, err := physicalInventoryUnderTest(t.Context(), owner, fixture.fence(t), limits)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var observed struct {
		Schema  string `json:"schema"`
		Entries []struct {
			Path     string        `json:"path"`
			Physical *PhysicalRoot `json:"physical,omitempty"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(raw, &observed); err != nil {
		t.Fatal(err)
	}
	if len(observed.Entries) != 5 {
		t.Fatal("physical export lost original members", len(observed.Entries))
	}
	for _, member := range observed.Entries {
		var stat unix.Stat_t
		if err := unix.Stat(filepath.Join(fixture.root, member.Path), &stat); err != nil {
			t.Fatal(err)
		}
		if member.Physical == nil || member.Physical.Inode != stat.Ino || member.Physical.Device != statDevice(&stat) {
			t.Fatal("inventory lacks the original member generation required to authenticate retained owner census", member.Path, member.Physical)
		}
	}
	if observed.Schema != "urnetwork-durable-volume-physical-inventory-v1" || report.RestartAuthorized {
		t.Fatal("physical export lost explicit scope", observed.Schema)
	}
}

// A new physical export must not silently change old v3 evidence bytes.
func TestPhysicalInventoryLeavesOrdinaryReportEncodingUnchanged(t *testing.T) {
	fixture := newVolumeFixture(t)
	fixture.custody(t)
	owner := fixture.open(t, Snapshot)
	fence := fixture.fence(t)
	limits := InventoryLimits{MaxEntries: 16, MaxBytes: 4096, MaxDepth: 4, MaxOwnerAttributes: 16, MaxOwnerAttributeBytes: 16384}
	before, err := owner.Inventory(t.Context(), fence, limits)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := physicalInventoryUnderTest(t.Context(), owner, fence, limits); err != nil {
		t.Fatal(err)
	}
	after, err := owner.Inventory(t.Context(), fence, limits)
	if err != nil {
		t.Fatal(err)
	}
	a, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) || before.Schema != InventorySchema {
		t.Fatal("physical export reinterpreted original byte inventory")
	}
	var fields map[string]json.RawMessage
	for _, entry := range after.Entries {
		raw, _ := json.Marshal(entry)
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		if _, ok := fields["physical"]; ok {
			t.Fatal("ordinary inventory gained physical authority")
		}
	}
}

// Cancellation and capacity failures return no partial physical authority.
func TestPhysicalInventoryBoundsAndCancellationRetainOriginalOwner(t *testing.T) {
	for _, mode := range []string{"count", "bytes", "canceled", "fence"} {
		func() {
			fixture := newVolumeFixture(t)
			fixture.custody(t)
			owner := fixture.open(t, Snapshot)
			fence := fixture.fence(t)
			limits := InventoryLimits{MaxEntries: 16, MaxBytes: 4096, MaxDepth: 4, MaxOwnerAttributes: 16, MaxOwnerAttributeBytes: 16384}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch mode {
			case "count":
				limits.MaxEntries = 1
			case "bytes":
				limits.MaxBytes = 1
			case "canceled":
				cancel()
			case "fence":
				fence = Reference{}
			}
			report, err := physicalInventoryUnderTest(ctx, owner, fence, limits)
			if err == nil || len(report.Entries) != 0 || report.Schema != "" {
				t.Fatal("failed physical export admitted partial authority", mode, err)
			}
			if err := owner.CheckRead(); err != nil {
				t.Fatal("caller refusal poisoned original identity", mode, err)
			}
		}()
	}
}

// A real renamed member after descriptor open cannot be exported as complete.
func TestPhysicalInventoryReplacementRefusesEntireReport(t *testing.T) {
	fixture := newVolumeFixture(t)
	fixture.custody(t)
	owner := fixture.open(t, Snapshot)
	fence := fixture.fence(t)
	limits := InventoryLimits{MaxEntries: 16, MaxBytes: 4096, MaxDepth: 4, MaxOwnerAttributes: 16, MaxOwnerAttributeBytes: 16384}
	changed := false
	owner.observeFile = func(stage string, file *os.File, path string) error {
		if stage == "inventory-attributes-list" && filepath.Base(file.Name()) == "pending" && !changed {
			changed = true
			original := filepath.Join(fixture.root, "journal", "pending")
			if err := os.Rename(original, filepath.Join(filepath.Dir(fixture.root), "retained-original")); err != nil {
				return err
			}
			return os.WriteFile(original, []byte("synthetic-original-signed-bytes"), 0600)
		}
		return nil
	}
	report, err := physicalInventoryUnderTest(t.Context(), owner, fence, limits)
	if !changed || !errors.Is(err, ErrIdentity) || len(report.Entries) != 0 {
		t.Fatal("changed named generation became physical backup authority", changed, err)
	}
	if err := owner.CheckRead(); !errors.Is(err, ErrIdentity) {
		t.Fatal("confirmed member loss did not remain sticky", err)
	}
}
