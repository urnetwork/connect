//go:build linux

package durablevolume

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A reviewed inventory's finite depth is usable through real plan/apply. The
// monitor consumer separately exercises an actual 32-component snapshot tree.
func TestPreparationRestoreAdmitsPhysicalInventoryDepthProfile(t *testing.T) {
	f := newPreparationRestoreFixture(t)
	f.request.Limits.MaxDepth = 32
	f.writeRequest(t)
	preparationRestoreAccept(t, f)
	result, err := ApplyPreparationWithHost(t.Context(), f.accepted, preparationRestoreTestAdapter(), f.volume.host)
	if err != nil || result.RestartAuthorized {
		t.Fatal("admitted physical inventory depth cannot restore", result, err)
	}
	before, err := os.ReadFile(filepath.Join(f.request.RestoreSource.Directory, "record.bin"))
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(f.request.RootPath, "record.bin"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("depth profile changed exact original payload", err)
	}
}

// Restoring a known inventory does not expand fresh provisioning, and a
// request beyond the inventory grammar refuses before any target authority.
func TestPreparationDepthOverflowAndFreshProfileRefuseBeforeEffects(t *testing.T) {
	for _, purpose := range []string{"fresh", "restore"} {
		var f *preparationFixture
		adapter := preparationTestAdapter()
		if purpose == "restore" {
			f = newPreparationRestoreFixture(t)
			adapter = preparationRestoreTestAdapter()
			f.request.Limits.MaxDepth = 33
		} else {
			f = newPreparationFixture(t)
			f.request.Limits.MaxDepth = 32
		}
		f.writeRequest(t)
		if _, err := PlanPreparationWithHost(t.Context(), f.reference, adapter, f.volume.host); err == nil || !strings.Contains(err.Error(), "capacities") {
			t.Fatal("out-of-profile depth was admitted or failed at unrelated boundary", purpose, err)
		}
		entries, err := os.ReadDir(f.request.RootPath)
		if err != nil || len(entries) != 0 {
			t.Fatal("refused depth changed target members", purpose, err)
		}
		for _, path := range []string{f.request.ControlPath, f.request.MarkerPath, f.request.LeasePath, f.request.DeclarationPath} {
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("refused depth published target authority", purpose, path, err)
			}
		}
	}
}
