//go:build linux

// Restored targets use the same real write-ahead publisher as fresh targets;
// source authority, original bytes and new physical coordinates stay separate.
package durablevolume

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// Only this fixture's fixed synthetic grammar may produce its new checkpoint.
func preparationRestoreTestAdapter() PreparationAdapter {
	return PreparationAdapter{
		Restore: func(ctx context.Context, name string, owner PreparationOwner, report Inventory) (PreparationOwnerPlan, error) {
			if owner.Kind != "synthetic-test-only" || owner.Purpose != "restore" || len(report.Entries) != 2 {
				return PreparationOwnerPlan{}, errors.New("synthetic restore scope differs")
			}
			var old struct {
				Schema string
				Inode  uint64
				Sha256 string
			}
			root, record := report.Entries[0], report.Entries[1]
			if len(root.OwnerAttributes) != 1 || json.Unmarshal(root.OwnerAttributes[0].Value, &old) != nil || old.Schema != "synthetic-preparation-test" || old.Inode != root.Physical.Inode || old.Sha256 != record.Sha256 || record.Path != "record.bin" {
				return PreparationOwnerPlan{}, errors.New("synthetic original owner authority differs")
			}
			return PreparationOwnerPlan{Owner: owner, StagingName: name, Files: []PreparationFile{{Path: record.Path, Kind: record.Kind, Mode: record.Mode, Bytes: record.Size, Sha256: record.Sha256}},
				Attributes: []PreparationAttributeSpec{{Path: ".", Name: root.OwnerAttributes[0].Name}}, Census: append(json.RawMessage(nil), root.OwnerAttributes[0].Value...)}, ctx.Err()
		},
		InspectRestore: func(ctx context.Context, root *os.File, owner PreparationOwnerPlan, report Inventory) ([]PreparedAttribute, error) {
			return preparationTestAdapter().Inspect(ctx, root, owner)
		},
	}
}

// A copied archive preserves original attributes while every copied inode is
// different. The target fence explicitly denies prior target state only.
func newPreparationRestoreFixture(t *testing.T) *preparationFixture {
	t.Helper()
	return newPreparationRestoreModeFixture(t, 0600)
}

// Exact original modes are exported from real files, never patched in a report.
func newPreparationRestoreModeFixture(t *testing.T, mode os.FileMode) *preparationFixture {
	t.Helper()
	f := newPreparationFixture(t)
	raw := []byte("exact reviewed public bytes\n")
	if err := os.WriteFile(filepath.Join(f.volume.root, "record.bin"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(f.volume.root, "record.bin"), mode); err != nil {
		t.Fatal(err)
	}
	var stat syscall.Stat_t
	if err := syscall.Stat(f.volume.root, &stat); err != nil {
		t.Fatal(err)
	}
	anchor, err := json.Marshal(struct {
		Schema string
		Inode  uint64
		Sha256 string
	}{Schema: "synthetic-preparation-test", Inode: stat.Ino, Sha256: testDigest(raw)})
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Setxattr(f.volume.root, "user.urnetwork.attempt-ledger-custody", anchor, 1); err != nil {
		t.Fatal(err)
	}
	fence := f.volume.fence(t)
	snapshot := f.volume.open(t, Snapshot)
	report, err := snapshot.InventoryPhysical(t.Context(), fence, InventoryLimits{MaxEntries: 16, MaxBytes: 1024 * 1024, MaxDepth: 4, MaxOwnerAttributes: 4, MaxOwnerAttributeBytes: 16384})
	if closeErr := snapshot.Close(); err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	reportRaw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(filepath.Dir(f.request.ControlPath), "original-inventory.json")
	if err := os.WriteFile(reportPath, reportRaw, 0600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(f.volume.mount, "original-archive")
	if err := os.Mkdir(archive, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(archive, "record.bin"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(archive, "record.bin"), mode); err != nil {
		t.Fatal(err)
	}
	for _, attribute := range report.Entries[0].OwnerAttributes {
		if err := syscall.Setxattr(archive, attribute.Name, attribute.Value, 1); err != nil {
			t.Fatal(err)
		}
	}
	generation, err := os.Open(f.volume.root)
	if err != nil {
		t.Fatal(err)
	}
	nonce, readErr := readInventoryAttribute(generation, RootGenerationAttribute, RootGenerationBytes)
	if err := errors.Join(readErr, generation.Close()); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Setxattr(archive, RootGenerationAttribute, nonce, 1); err != nil {
		t.Fatal(err)
	}
	f.request.Purpose = "restore"
	f.request.Owners[0].Purpose = "restore"
	f.request.RestoreSource = &PreparationRestoreSource{Directory: archive, Inventory: Reference{Path: reportPath, Sha256: testDigest(reportRaw)}, FormerWriterFence: fence}
	if err := syscall.Stat(f.request.RootPath, &stat); err != nil {
		t.Fatal(err)
	}
	targetFence, err := json.Marshal(PreparationFence{Schema: PreparationFenceSchema, RootPath: f.request.RootPath, RootInode: stat.Ino, Purpose: "restore", FormerWritersStopped: true, NoPreviousTargetState: true, Evidence: "synthetic target has no earlier state; original source is explicitly retained"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.request.FormerWriterFence.Path, targetFence, 0600); err != nil {
		t.Fatal(err)
	}
	f.request.FormerWriterFence.Sha256 = testDigest(targetFence)
	f.writeRequest(t)
	return f
}

// Only successful complete planning becomes accepted apply authority.
func preparationRestoreAccept(t *testing.T, f *preparationFixture) {
	t.Helper()
	plan, err := PlanPreparationWithHost(t.Context(), f.reference, preparationRestoreTestAdapter(), f.volume.host)
	if err != nil {
		t.Fatal("exact original restore planning refused", err)
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	f.plan = plan
	f.accepted = Reference{Path: filepath.Join(filepath.Dir(f.request.ControlPath), "restore-plan.json"), Sha256: testDigest(raw)}
	if err := os.WriteFile(f.accepted.Path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

// The actual public core entrypoints produce a readable runtime declaration,
// preserve original archive bytes and retain exact repeated completion.
func TestPreparationRestoreCopiedArchiveKeepsOriginalIntent(t *testing.T) {
	f := newPreparationRestoreFixture(t)
	preparationRestoreAccept(t, f)
	result, err := ApplyPreparationWithHost(t.Context(), f.accepted, preparationRestoreTestAdapter(), f.volume.host)
	if err != nil || result.RestartAuthorized {
		t.Fatal("restore apply failed or granted restart", result, err)
	}
	owner, err := OpenWithHost(result.Declaration, f.request.RootPath, ReadOnly, f.volume.host)
	if err != nil {
		t.Fatal("restored declaration cannot admit physical custody", err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(f.request.ControlPath)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := ApplyPreparationWithHost(t.Context(), f.accepted, preparationRestoreTestAdapter(), f.volume.host)
	after, readErr := os.ReadFile(f.request.ControlPath)
	if err != nil || readErr != nil || repeated != result || !bytes.Equal(before, after) {
		t.Fatal("restored completion was reset", err, readErr)
	}
	for _, root := range []string{f.volume.root, f.request.RestoreSource.Directory, f.request.RootPath} {
		raw, err := os.ReadFile(filepath.Join(root, "record.bin"))
		if err != nil || string(raw) != "exact reviewed public bytes\n" {
			t.Fatal("original payload changed", root, err)
		}
	}
}

// Real synced target bytes survive lost acknowledgements. Each failed owner
// closes before exact-plan readback, which must preserve original inodes/prefix.
func TestPreparationRestoreLostAcknowledgementsKeepOriginalTarget(t *testing.T) {
	for _, boundary := range []string{"control-header", "member-sync", "parent-sync", "attribute-sync", "control-complete"} {
		func() {
			f := newPreparationRestoreFixture(t)
			preparationRestoreAccept(t, f)
			called := false
			_, err := applyPreparation(t.Context(), f.accepted, preparationRestoreTestAdapter(), f.volume.host, daemonScope, &preparationHooks{after: func(stage, path string) error {
				if called || stage != boundary {
					return nil
				}
				if boundary == "member-sync" || boundary == "parent-sync" || boundary == "control-complete" {
					if path != filepath.Join(f.request.RootPath, "record.bin") {
						return nil
					}
				}
				if boundary == "attribute-sync" && !strings.HasSuffix(path, ":user.urnetwork.attempt-ledger-custody") {
					return nil
				}
				called = true
				return syscall.EIO
			}})
			if !called || !errors.Is(err, ErrPreparationUncertain) || !errors.Is(err, syscall.EIO) {
				t.Fatal("restore did not reach the intended uncertain boundary", boundary, called, err)
			}
			before, err := os.ReadFile(f.request.ControlPath)
			if err != nil {
				t.Fatal(err)
			}
			info, statErr := os.Stat(filepath.Join(f.request.RootPath, "record.bin"))
			result, err := ApplyPreparationWithHost(t.Context(), f.accepted, preparationRestoreTestAdapter(), f.volume.host)
			if err != nil || result.RestartAuthorized {
				t.Fatal("joined restore could not resume original bytes", boundary, err)
			}
			after, err := os.ReadFile(f.request.ControlPath)
			if err != nil || !bytes.HasPrefix(after, before) {
				t.Fatal("restore replaced original progress prefix", boundary, err)
			}
			if statErr == nil {
				current, err := os.Stat(filepath.Join(f.request.RootPath, "record.bin"))
				if err != nil || !os.SameFile(info, current) {
					t.Fatal("restore replaced an acknowledged target inode", boundary, err)
				}
			}
		}()
	}
}

// Source loss, unreviewed bytes and false zero-history assertions fail before
// any target control or member is created. Existing unknown state is not fresh.
func TestPreparationRestoreRefusesAlteredSourceAndTargetAuthority(t *testing.T) {
	for _, mode := range []string{"missing", "extra", "bytes", "attribute", "source-unjoined", "target-fresh", "capacity", "ambiguous-mount", "canceled"} {
		func() {
			f := newPreparationRestoreFixture(t)
			ctx := t.Context()
			want := ""
			switch mode {
			case "missing":
				if err := os.Remove(filepath.Join(f.request.RestoreSource.Directory, "record.bin")); err != nil {
					t.Fatal(err)
				}
				want = "missing retained members"
			case "extra":
				if err := os.WriteFile(filepath.Join(f.request.RestoreSource.Directory, "unknown"), nil, 0600); err != nil {
					t.Fatal(err)
				}
				want = "unreviewed member"
			case "bytes":
				altered := []byte("exact reviewed public bytes\n")
				altered[0] = 'X'
				if err := os.WriteFile(filepath.Join(f.request.RestoreSource.Directory, "record.bin"), altered, 0600); err != nil {
					t.Fatal(err)
				}
				want = "differs from original exported bytes"
			case "attribute":
				if err := syscall.Removexattr(f.request.RestoreSource.Directory, "user.urnetwork.attempt-ledger-custody"); err != nil {
					t.Fatal(err)
				}
				want = "missing original owner authority"
			case "source-unjoined":
				ref := f.request.RestoreSource.FormerWriterFence
				var fence FormerWriterFence
				raw, err := os.ReadFile(ref.Path)
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(raw, &fence); err != nil {
					t.Fatal(err)
				}
				fence.FormerWritersStopped = false
				raw, err = json.Marshal(fence)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(ref.Path, raw, 0600); err != nil {
					t.Fatal(err)
				}
				f.request.RestoreSource.FormerWriterFence.Sha256 = testDigest(raw)
				want = "source stop assertion"
			case "target-fresh":
				ref := f.request.FormerWriterFence
				var fence PreparationFence
				raw, err := os.ReadFile(ref.Path)
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(raw, &fence); err != nil {
					t.Fatal(err)
				}
				fence.NoPreviousOwnerState = true
				raw, err = json.Marshal(fence)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(ref.Path, raw, 0600); err != nil {
					t.Fatal(err)
				}
				f.request.FormerWriterFence.Sha256 = testDigest(raw)
				want = "target history"
			case "capacity":
				f.request.Limits.MaxBytes = 1
				want = "target capacities"
			case "ambiguous-mount":
				f.volume.host.change(func() {
					mount := f.volume.host.mounts[len(f.volume.host.mounts)-1]
					mount.Path = f.request.RestoreSource.Directory
					mount.Id += 100
					f.volume.host.mounts = append(f.volume.host.mounts, mount, mount)
				})
				want = "ambiguous physical mount views"
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				want = "canceled"
			}
			f.writeRequest(t)
			_, err := PlanPreparationWithHost(ctx, f.reference, preparationRestoreTestAdapter(), f.volume.host)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatal("restore refusal did not prove its intended boundary", mode, err)
			}
			names, err := os.ReadDir(f.request.RootPath)
			if err != nil || len(names) != 0 {
				t.Fatal("failed restore created target members", mode, err)
			}
			if _, err := os.Stat(f.request.ControlPath); !os.IsNotExist(err) {
				t.Fatal("failed planning created target control", mode, err)
			}
		}()
	}
}

// Logical raw slots are not the total namespace size. The explicit physical
// profile admits structural entries while ordinary v3 retains its old ceiling.
func TestPhysicalRestoreCapacityCountsNamespaceSeparately(t *testing.T) {
	limits := InventoryLimits{MaxEntries: 10003, MaxBytes: 1024 * 1024, MaxDepth: 4, MaxOwnerAttributes: 4, MaxOwnerAttributeBytes: 16384}
	if err := limits.validate(); err == nil {
		t.Fatal("legacy ordinary inventory silently changed its approved entry ceiling")
	}
	if err := limits.validateEntries(MaximumPhysicalInventoryEntries); err != nil {
		t.Fatal("physical restore cannot represent a full native raw census plus its structure", err)
	}
	f := newPreparationRestoreFixture(t)
	f.request.Limits.MaxEntries = 10003
	if err := f.request.validate(daemonScope); err != nil {
		t.Fatal("restore target collapsed namespace count into raw-member slots", err)
	}
	f.request.Limits.MaxEntries = MaximumPhysicalInventoryEntries + 1
	if err := f.request.validate(daemonScope); err == nil {
		t.Fatal("restore silently expanded its finite physical-entry ceiling")
	}
}
