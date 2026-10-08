//go:build linux || darwin

// Synthetic physical metadata models an inode-bearing unsigned census. The
// public core still owns actual exports, staged bytes, moves, fsyncs and guards.
package durablevolume

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

type preparationSyntheticCensus struct {
	Schema string `json:"schema"`
	Inode  uint64 `json:"inode"`
	Sha256 string `json:"sha256"`
}

// Only the fixture's known unsigned inode field may change. Payload hashes
// and bytes stay separate from the physical census and its owner checkpoint.
func preparationPhysicalTestAdapter() PreparationAdapter {
	return PreparationAdapter{
		Restore: func(ctx context.Context, name string, owner PreparationOwner, report Inventory) (PreparationOwnerPlan, error) {
			if owner.Kind != "synthetic-test-only" || owner.Purpose != "restore" || len(report.Entries) != 3 || report.Entries[1].Path != "census.json" || report.Entries[2].Path != "record.bin" || len(report.Entries[0].OwnerAttributes) != 1 {
				return PreparationOwnerPlan{}, errors.New("synthetic physical scope differs")
			}
			attribute := report.Entries[0].OwnerAttributes[0]
			var checkpoint preparationSyntheticCensus
			if json.Unmarshal(attribute.Value, &checkpoint) != nil || checkpoint.Schema != "synthetic-physical-checkpoint" || checkpoint.Inode != report.PhysicalRoot.Inode || checkpoint.Sha256 != report.Entries[1].Sha256 {
				return PreparationOwnerPlan{}, errors.New("synthetic original physical checkpoint differs")
			}
			files := []PreparationFile{}
			for _, entry := range report.Entries[1:] {
				files = append(files, PreparationFile{Path: entry.Path, Kind: entry.Kind, Mode: entry.Mode, Bytes: entry.Size, Sha256: entry.Sha256})
			}
			return PreparationOwnerPlan{Owner: owner, StagingName: name, ExclusiveRoot: true, Files: files,
				Attributes: []PreparationAttributeSpec{{Path: ".", Name: attribute.Name}}, Census: append(json.RawMessage(nil), attribute.Value...),
				PhysicalMetadata: &PreparationPhysicalMetadata{Path: "census.json", MaximumBytes: 4096}}, ctx.Err()
		},
		RebindRestore: func(ctx context.Context, owner PreparationOwnerPlan, report Inventory, raw []byte, targets []PreparationSource) ([]byte, error) {
			var census preparationSyntheticCensus
			if len(targets) != 1 || targets[0].File.Path != "record.bin" || json.Unmarshal(raw, &census) != nil || census.Schema != "synthetic-physical-census" || census.Inode != report.Entries[2].Physical.Inode || census.Sha256 != report.Entries[2].Sha256 || targets[0].File.Sha256 != census.Sha256 {
				return nil, errors.New("synthetic derivation changed original payload authority")
			}
			census.Inode = targets[0].Identity.Inode
			encoded, err := json.Marshal(census)
			return encoded, errors.Join(err, ctx.Err())
		},
		InspectRestore: func(ctx context.Context, root *os.File, owner PreparationOwnerPlan, report Inventory) ([]PreparedAttribute, error) {
			path := filepath.Join(root.Name(), "census.json")
			file, err := preparationOpenAbsolute(path, false)
			if err != nil {
				return nil, err
			}
			raw, readErr := boundedProtectedReadContext(ctx, file, 4096)
			if err := errors.Join(readErr, file.Close()); err != nil {
				return nil, err
			}
			var census preparationSyntheticCensus
			var stat unix.Stat_t
			if err := unix.Stat(filepath.Join(root.Name(), "record.bin"), &stat); err != nil {
				return nil, err
			}
			if json.Unmarshal(raw, &census) != nil || census.Inode != stat.Ino || census.Sha256 != report.Entries[2].Sha256 {
				return nil, errors.New("restored physical census does not name the actual member")
			}
			identity, err := preparationIdentity(root)
			if err != nil {
				return nil, err
			}
			checkpoint, err := json.Marshal(preparationSyntheticCensus{Schema: "synthetic-physical-checkpoint", Inode: identity.Inode, Sha256: testDigest(raw)})
			return []PreparedAttribute{{Spec: owner.Attributes[0], Raw: checkpoint}}, errors.Join(err, ctx.Err())
		},
	}
}

// Source metadata is created before a new real physical export. Copied
// original attributes are not authority for the new target's physical inodes.
func newPreparationPhysicalFixture(t *testing.T) *preparationFixture {
	return newPreparationPhysicalModeFixture(t, 0600)
}

// The census remains private mutable metadata while the independently copied
// signed/public record can retain its actual immutable mode.
func newPreparationPhysicalModeFixture(t *testing.T, mode os.FileMode) *preparationFixture {
	t.Helper()
	f := newPreparationRestoreFixture(t)
	for _, root := range []string{f.volume.root, f.request.RestoreSource.Directory} {
		if err := os.Chmod(filepath.Join(root, "record.bin"), mode); err != nil {
			t.Fatal(err)
		}
	}
	var stat unix.Stat_t
	if err := unix.Stat(filepath.Join(f.volume.root, "record.bin"), &stat); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(preparationSyntheticCensus{Schema: "synthetic-physical-census", Inode: stat.Ino, Sha256: testDigest([]byte("exact reviewed public bytes\n"))})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.volume.root, "census.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := unix.Stat(f.volume.root, &stat); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := json.Marshal(preparationSyntheticCensus{Schema: "synthetic-physical-checkpoint", Inode: stat.Ino, Sha256: testDigest(raw)})
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{f.volume.root, f.request.RestoreSource.Directory} {
		if err := unix.Setxattr(root, "user.urnetwork.attempt-ledger-custody", checkpoint, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(f.request.RestoreSource.Directory, "census.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	snapshot := f.volume.open(t, Snapshot)
	report, err := snapshot.InventoryPhysical(t.Context(), f.request.RestoreSource.FormerWriterFence, InventoryLimits{MaxEntries: 16, MaxBytes: 1024 * 1024, MaxDepth: 4, MaxOwnerAttributes: 4, MaxOwnerAttributeBytes: 16384})
	if closeErr := snapshot.Close(); err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	reportRaw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.request.RestoreSource.Inventory.Path, reportRaw, 0600); err != nil {
		t.Fatal(err)
	}
	f.request.RestoreSource.Inventory.Sha256 = testDigest(reportRaw)
	f.writeRequest(t)
	plan, err := PlanPreparationWithHost(t.Context(), f.reference, preparationPhysicalTestAdapter(), f.volume.host)
	if err != nil {
		t.Fatal("public physical metadata planning failed", err)
	}
	f.plan = plan
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	f.accepted = Reference{Path: filepath.Join(filepath.Dir(f.request.ControlPath), "physical-plan.json"), Sha256: testDigest(encoded)}
	if err := os.WriteFile(f.accepted.Path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestPreparationPhysicalMetadataPreservesImmutableOriginalMode(t *testing.T) {
	f := newPreparationPhysicalModeFixture(t, 0400)
	result, err := ApplyPreparationWithHost(t.Context(), f.accepted, preparationPhysicalTestAdapter(), f.volume.host)
	if err != nil {
		t.Fatal("physical derivation rejected an already-supported immutable original", err)
	}
	assertPreparationPhysicalResult(t, f, result)
	for _, name := range []string{"record.bin", "census.json"} {
		info, err := os.Stat(filepath.Join(f.request.RootPath, name))
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0600)
		if name == "record.bin" {
			want = 0400
		}
		if info.Mode().Perm() != want {
			t.Fatal("physical derivation broadened original mode", name, info.Mode())
		}
	}
	if _, err := ApplyPreparationWithHost(t.Context(), f.accepted, preparationPhysicalTestAdapter(), f.volume.host); err != nil {
		t.Fatal("exact completed restore did not continue", err)
	}
}

// The actual declared owner accepts the new generation; signed/public member
// payload bytes and the complete original metadata remain separately exact.
func assertPreparationPhysicalResult(t *testing.T, f *preparationFixture, result PreparationResult) {
	t.Helper()
	if result.RestartAuthorized || len(f.plan.Derivations) != 1 {
		t.Fatal("physical preparation grants activation or lost derivation lineage")
	}
	derivation := f.plan.Derivations[0]
	if derivation.Original.File.Sha256 == derivation.Derived.Sha256 {
		t.Fatal("new physical census did not bind different member inodes")
	}
	for _, source := range f.plan.Sources {
		var stat unix.Stat_t
		path := filepath.Join(f.request.RootPath, source.File.Path)
		if err := unix.Stat(path, &stat); err != nil || stat.Ino != source.Identity.Inode || uint64(stat.Dev) != source.Identity.Device {
			t.Fatal("published member did not preserve its reviewed staging inode", path, err)
		}
		if _, err := os.Lstat(source.Path); !os.IsNotExist(err) {
			t.Fatal("published member still aliases its stage", source.Path, err)
		}
		raw, err := os.ReadFile(path)
		if err != nil || testDigest(raw) != source.File.Sha256 {
			t.Fatal("published member bytes differ", err)
		}
	}
	original, err := os.ReadFile(derivation.Original.Path)
	if err != nil || testDigest(original) != derivation.Original.File.Sha256 {
		t.Fatal("original census was discarded", err)
	}
	for _, root := range []string{f.volume.root, f.request.RestoreSource.Directory} {
		raw, err := os.ReadFile(filepath.Join(root, "census.json"))
		if err != nil || !bytes.Equal(raw, original) {
			t.Fatal("original source or copied authority was rewritten", err)
		}
	}
	owner, err := OpenWithHost(result.Declaration, f.request.RootPath, ReadOnly, f.volume.host)
	if err != nil {
		t.Fatal("prepared physical declaration cannot be reopened", err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
}

// Completed reapplication must use the moved target identities rather than
// falsely require the now-absent former staging names or recreate them.
func TestPreparationPhysicalMetadataRetainsBothCensuses(t *testing.T) {
	f := newPreparationPhysicalFixture(t)
	result, err := ApplyPreparationWithHost(t.Context(), f.accepted, preparationPhysicalTestAdapter(), f.volume.host)
	if err != nil {
		t.Fatal(err)
	}
	assertPreparationPhysicalResult(t, f, result)
	before, err := os.ReadFile(f.request.ControlPath)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := ApplyPreparationWithHost(t.Context(), f.accepted, preparationPhysicalTestAdapter(), f.volume.host)
	after, readErr := os.ReadFile(f.request.ControlPath)
	if err != nil || readErr != nil || repeated != result || !bytes.Equal(before, after) {
		t.Fatal("repeated physical restore changed original progress", err, readErr)
	}
	assertPreparationPhysicalResult(t, f, repeated)
}

// Each real publication boundary can lose its acknowledgement. A joined
// invocation must finish the same inode move and retain the original prefix.
func TestPreparationPhysicalMovesReconcileEachSyncedBoundary(t *testing.T) {
	for _, stage := range []string{"restore-member-rename", "restore-source-parent-sync", "restore-target-parent-sync"} {
		f := newPreparationPhysicalFixture(t)
		called := false
		cause := errors.New("synthetic lost physical move acknowledgement")
		_, err := applyPreparation(t.Context(), f.accepted, preparationPhysicalTestAdapter(), f.volume.host, daemonScope, &preparationHooks{after: func(observed, path string) error {
			if observed == stage && filepath.Base(path) == "record.bin" {
				called = true
				return cause
			}
			return nil
		}})
		if !called || !errors.Is(err, cause) || !errors.Is(err, ErrPreparationUncertain) {
			t.Fatal("actual move barrier did not retain uncertainty", stage, called, err)
		}
		before, err := os.ReadFile(f.request.ControlPath)
		if err != nil {
			t.Fatal(err)
		}
		result, err := ApplyPreparationWithHost(t.Context(), f.accepted, preparationPhysicalTestAdapter(), f.volume.host)
		after, readErr := os.ReadFile(f.request.ControlPath)
		if err != nil || readErr != nil || !bytes.HasPrefix(after, before) {
			t.Fatal("joined move lost its exact original progress", stage, err, readErr)
		}
		assertPreparationPhysicalResult(t, f, result)
	}
}

// Competing targets and lost source/target pairs never receive a copying or
// fresh-state fallback, even when the original archived bytes still exist.
func TestPreparationPhysicalMovesRefuseAmbiguousAndLostMembers(t *testing.T) {
	for _, mode := range []string{"both-present", "both-missing", "completed-missing", "replaced-stage"} {
		f := newPreparationPhysicalFixture(t)
		source := f.plan.Sources[0]
		target := filepath.Join(f.request.RootPath, source.File.Path)
		if mode == "completed-missing" {
			if _, err := ApplyPreparationWithHost(t.Context(), f.accepted, preparationPhysicalTestAdapter(), f.volume.host); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(target); err != nil {
				t.Fatal(err)
			}
		} else if mode == "both-missing" {
			if err := os.Remove(source.Path); err != nil {
				t.Fatal(err)
			}
		} else {
			raw, err := os.ReadFile(source.Path)
			if err != nil {
				t.Fatal(err)
			}
			path := target
			if mode == "replaced-stage" {
				if err := os.Rename(source.Path, source.Path+".held"); err != nil {
					t.Fatal(err)
				}
				path = source.Path
			}
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
		}
		_, err := ApplyPreparationWithHost(t.Context(), f.accepted, preparationPhysicalTestAdapter(), f.volume.host)
		if !errors.Is(err, ErrIdentity) {
			t.Fatal("physical member ambiguity/loss was not retained", mode, err)
		}
		if mode == "both-missing" || mode == "completed-missing" {
			if _, err := os.Lstat(target); !os.IsNotExist(err) {
				t.Fatal("lost physical member was recreated", mode, err)
			}
		}
	}
}

// A different accepted result hash cannot authorize a different derivation or
// use altered original metadata. Rejection precedes any target publication.
func TestPreparationPhysicalMetadataRefusesChangedLineage(t *testing.T) {
	for _, mode := range []string{"original-bytes", "derived-hash", "missing-lineage"} {
		f := newPreparationPhysicalFixture(t)
		if mode == "original-bytes" {
			if err := os.WriteFile(f.plan.Derivations[0].Original.Path, []byte("different original"), 0600); err != nil {
				t.Fatal(err)
			}
		} else {
			if mode == "derived-hash" {
				f.plan.Derivations[0].Derived.Sha256 = testDigest([]byte("unreviewed derivation"))
			} else {
				f.plan.Derivations = nil
			}
			raw, err := json.Marshal(f.plan)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f.accepted.Path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			f.accepted.Sha256 = testDigest(raw)
		}
		if _, err := ApplyPreparationWithHost(t.Context(), f.accepted, preparationPhysicalTestAdapter(), f.volume.host); err == nil {
			t.Fatal("changed physical lineage was admitted", mode)
		}
		names, err := os.ReadDir(f.request.RootPath)
		if err != nil || len(names) != 0 {
			t.Fatal("lineage refusal published target members", mode, err)
		}
		if _, err := os.Lstat(f.request.ControlPath); !os.IsNotExist(err) {
			t.Fatal("lineage refusal created target progress", mode, err)
		}
	}
}
