//go:build linux

// Original read-only evidence keeps its exact mode through physical export,
// private staging, publication and resumed completion. No permission is inferred.
package durablevolume

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestPhysicalInventoryLoadsOriginalReadOnlyEvidence(t *testing.T) {
	f := newPreparationRestoreModeFixture(t, 0400)
	report, err := LoadPhysicalInventory(t.Context(), f.request.RestoreSource.Inventory)
	if err != nil || len(report.Entries) != 2 || report.Entries[1].Mode != 0400 {
		t.Fatal("original immutable evidence mode was not admitted", report, err)
	}
	for _, mode := range []uint32{0, 0200, 0440, 0640, 0644, 0700, 01400} {
		report.Entries[1].Mode = mode
		if _, err := LoadPhysicalInventory(t.Context(), physicalInventoryReference(t, f.volume, report)); err == nil {
			t.Fatal("unreviewed file protection was admitted", mode)
		}
	}
}

func TestPreparationRestoreReadOnlyEvidenceKeepsExactMode(t *testing.T) {
	f := newPreparationRestoreModeFixture(t, 0400)
	preparationRestoreAccept(t, f)
	result, err := ApplyPreparationWithHost(t.Context(), f.accepted, preparationRestoreTestAdapter(), f.volume.host)
	if err != nil || result.RestartAuthorized {
		t.Fatal("immutable original restore failed", result, err)
	}
	for _, root := range []string{f.volume.root, f.request.RestoreSource.Directory, f.request.RootPath} {
		path := filepath.Join(root, "record.bin")
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0400 {
			t.Fatal("immutable original evidence became writable", path, info, err)
		}
		raw, err := os.ReadFile(path)
		if err != nil || string(raw) != "exact reviewed public bytes\n" {
			t.Fatal("original bytes changed", path, err)
		}
	}
	before, err := os.Stat(filepath.Join(f.request.RootPath, "record.bin"))
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := ApplyPreparationWithHost(t.Context(), f.accepted, preparationRestoreTestAdapter(), f.volume.host)
	after, statErr := os.Stat(filepath.Join(f.request.RootPath, "record.bin"))
	if err != nil || statErr != nil || result != repeated || !os.SameFile(before, after) || after.Mode().Perm() != 0400 {
		t.Fatal("repeat changed immutable completion", err, statErr)
	}
}

func TestPreparationRestoreReadOnlyLostAckRetainsModeAndInode(t *testing.T) {
	for _, boundary := range []string{"member-sync", "parent-sync", "control-complete"} {
		f := newPreparationRestoreModeFixture(t, 0400)
		preparationRestoreAccept(t, f)
		path := filepath.Join(f.request.RootPath, "record.bin")
		called := false
		_, err := applyPreparation(t.Context(), f.accepted, preparationRestoreTestAdapter(), f.volume.host, daemonScope, &preparationHooks{after: func(stage, selected string) error {
			if stage != boundary || selected != path || called {
				return nil
			}
			called = true
			return syscall.EIO
		}})
		if !called || !errors.Is(err, ErrPreparationUncertain) || !errors.Is(err, syscall.EIO) {
			t.Fatal("immutable publication did not reach lost acknowledgement", boundary, err)
		}
		before, err := os.Stat(path)
		if err != nil || before.Mode().Perm() != 0400 {
			t.Fatal("uncertain immutable publication lost its mode", boundary, err)
		}
		prefix, err := os.ReadFile(f.request.ControlPath)
		if err != nil {
			t.Fatal(err)
		}
		result, err := ApplyPreparationWithHost(t.Context(), f.accepted, preparationRestoreTestAdapter(), f.volume.host)
		if err != nil || result.RestartAuthorized {
			t.Fatal("immutable publication could not resume", boundary, err)
		}
		after, err := os.Stat(path)
		if err != nil || !os.SameFile(before, after) || after.Mode().Perm() != 0400 {
			t.Fatal("resume changed immutable inode or mode", boundary, err)
		}
		raw, err := os.ReadFile(f.request.ControlPath)
		if err != nil || !bytes.HasPrefix(raw, prefix) {
			t.Fatal("resume discarded original progress", boundary, err)
		}
	}
}

func TestPreparationRestoreReadOnlyChangedModeRemainsIdentityConflict(t *testing.T) {
	for _, target := range []bool{false, true} {
		f := newPreparationRestoreModeFixture(t, 0400)
		preparationRestoreAccept(t, f)
		path := filepath.Join(f.request.RestoreSource.Directory, "record.bin")
		if target {
			if _, err := ApplyPreparationWithHost(t.Context(), f.accepted, preparationRestoreTestAdapter(), f.volume.host); err != nil {
				t.Fatal(err)
			}
			path = filepath.Join(f.request.RootPath, "record.bin")
		}
		if err := os.Chmod(path, 0600); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("mode contradiction was not created", err)
		}
		_, err = ApplyPreparationWithHost(t.Context(), f.accepted, preparationRestoreTestAdapter(), f.volume.host)
		if !errors.Is(err, ErrIdentity) {
			t.Fatal("changed original mode lost identity refusal", target, err)
		}
		if err := os.Chmod(path, 0400); err != nil {
			t.Fatal(err)
		}
		if _, err := ApplyPreparationWithHost(t.Context(), f.accepted, preparationRestoreTestAdapter(), f.volume.host); err != nil {
			t.Fatal("exact immutable source could not recover", target, err)
		}
	}
}
