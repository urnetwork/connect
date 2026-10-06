//go:build linux

// A checkpoint exchange has two independently retained physical censuses.
// Both retain their original bytes and derive only the reviewed member inode.
package durablevolume

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"testing"
)

type preparationPairedTestCensus struct {
	Schema   string `json:"schema"`
	Inode    uint64 `json:"inode"`
	Sha256   string `json:"sha256"`
	Revision int    `json:"revision,omitempty"`
}

// JSON construction also compiles against the old single-metadata type. Its
// old behavior ignores the companion and fails the actual public derivation.
func preparationPairedTestAdapter() PreparationAdapter {
	base := preparationPhysicalTestAdapter()
	view := func(report Inventory) Inventory {
		filtered := report
		filtered.Entries = nil
		for _, entry := range report.Entries {
			if entry.Path != "census-next.json" {
				filtered.Entries = append(filtered.Entries, entry)
			}
		}
		return filtered
	}
	return PreparationAdapter{
		Restore: func(ctx context.Context, name string, owner PreparationOwner, report Inventory) (PreparationOwnerPlan, error) {
			result, err := base.Restore(ctx, name, owner, view(report))
			if err != nil {
				return result, err
			}
			found := false
			for _, entry := range report.Entries {
				if entry.Path == "census-next.json" {
					if found || entry.Kind != "file" || entry.Mode != 0600 || entry.Size == 0 || entry.Size > 4096 {
						return result, errors.New("synthetic companion is invalid")
					}
					found = true
					result.Files = append(result.Files, PreparationFile{Path: entry.Path, Kind: entry.Kind, Mode: entry.Mode, Bytes: entry.Size, Sha256: entry.Sha256})
				}
			}
			if !found {
				return result, errors.New("synthetic companion is absent")
			}
			sort.Slice(result.Files, func(i, j int) bool { return result.Files[i].Path < result.Files[j].Path })
			if err := json.Unmarshal([]byte(`{"path":"census.json","maximum_bytes":4096,"companion_path":"census-next.json"}`), result.PhysicalMetadata); err != nil {
				return result, err
			}
			return result, nil
		},
		RebindRestore: func(ctx context.Context, owner PreparationOwnerPlan, report Inventory, raw []byte, targets []PreparationSource) ([]byte, error) {
			var census preparationPairedTestCensus
			if json.Unmarshal(raw, &census) != nil || census.Revision < 0 || census.Revision > 1 {
				return nil, errors.New("synthetic census revision changed")
			}
			rebound, err := base.RebindRestore(ctx, owner, view(report), raw, targets)
			if err != nil {
				return nil, err
			}
			var changed preparationSyntheticCensus
			if err := json.Unmarshal(rebound, &changed); err != nil {
				return nil, err
			}
			census.Inode = changed.Inode
			return json.Marshal(census)
		},
		InspectRestore: func(ctx context.Context, root *os.File, owner PreparationOwnerPlan, report Inventory) ([]PreparedAttribute, error) {
			attributes, err := base.InspectRestore(ctx, root, owner, view(report))
			if err != nil {
				return nil, err
			}
			file, err := preparationOpenAbsolute(filepath.Join(root.Name(), "census-next.json"), false)
			if err != nil {
				return nil, err
			}
			raw, readErr := boundedProtectedReadContext(ctx, file, 4096)
			if err := errors.Join(readErr, file.Close()); err != nil {
				return nil, err
			}
			var census preparationPairedTestCensus
			var stat syscall.Stat_t
			if err := syscall.Stat(filepath.Join(root.Name(), "record.bin"), &stat); err != nil {
				return nil, err
			}
			if json.Unmarshal(raw, &census) != nil || census.Inode != stat.Ino || census.Revision != 1 || census.Sha256 != report.Entries[len(report.Entries)-1].Sha256 {
				return nil, errors.New("synthetic target companion changed")
			}
			return attributes, nil
		},
	}
}

// A fresh synthetic export preserves the original fixture's record and first
// census, and adds the second exact image before any target publication.
func newPreparationPairedFixture(t *testing.T) *preparationFixture {
	t.Helper()
	f := newPreparationPhysicalFixture(t)
	raw, err := os.ReadFile(filepath.Join(f.volume.root, "census.json"))
	if err != nil {
		t.Fatal(err)
	}
	var census preparationPairedTestCensus
	if err := json.Unmarshal(raw, &census); err != nil {
		t.Fatal(err)
	}
	census.Revision = 1
	raw, err = json.Marshal(census)
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{f.volume.root, f.request.RestoreSource.Directory} {
		if err := os.WriteFile(filepath.Join(root, "census-next.json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := f.volume.open(t, Snapshot)
	report, err := snapshot.InventoryPhysical(t.Context(), f.request.RestoreSource.FormerWriterFence, InventoryLimits{MaxEntries: 16, MaxBytes: 1024 * 1024, MaxDepth: 4, MaxOwnerAttributes: 4, MaxOwnerAttributeBytes: 16384})
	if closeErr := snapshot.Close(); err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	raw, err = json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.request.RestoreSource.Inventory.Path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	f.request.RestoreSource.Inventory.Sha256 = testDigest(raw)
	f.writeRequest(t)
	f.plan, err = PlanPreparationWithHost(t.Context(), f.reference, preparationPairedTestAdapter(), f.volume.host)
	if err != nil {
		t.Fatal("public paired census planning cannot retain both original images", err)
	}
	raw, err = json.Marshal(f.plan)
	if err != nil {
		t.Fatal(err)
	}
	f.accepted = Reference{Path: filepath.Join(filepath.Dir(f.request.ControlPath), "paired-plan.json"), Sha256: testDigest(raw)}
	if err := os.WriteFile(f.accepted.Path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestPreparationPairedMetadataRetainsExactOriginals(t *testing.T) {
	f := newPreparationPairedFixture(t)
	if len(f.plan.Derivations) != 2 || f.plan.Derivations[0].Original.Path == f.plan.Derivations[1].Original.Path {
		t.Fatal("both original census generations were not independently retained")
	}
	result, err := ApplyPreparationWithHost(t.Context(), f.accepted, preparationPairedTestAdapter(), f.volume.host)
	if err != nil || result.RestartAuthorized {
		t.Fatal("paired restore failed or grants runtime authority", err)
	}
	for _, derivation := range f.plan.Derivations {
		original, err := os.ReadFile(derivation.Original.Path)
		if err != nil || testDigest(original) != derivation.Original.File.Sha256 {
			t.Fatal("original census changed", err)
		}
		var before, after preparationPairedTestCensus
		raw, err := os.ReadFile(filepath.Join(f.request.RootPath, derivation.Derived.Path))
		if err != nil || json.Unmarshal(original, &before) != nil || json.Unmarshal(raw, &after) != nil {
			t.Fatal("derived census is unavailable", err)
		}
		if before.Inode == after.Inode {
			t.Fatal("restored census retained old physical inode")
		}
		after.Inode = before.Inode
		if before != after {
			t.Fatal("derivation changed authority beyond physical coordinates")
		}
	}
	before, err := os.ReadFile(f.request.ControlPath)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := ApplyPreparationWithHost(t.Context(), f.accepted, preparationPairedTestAdapter(), f.volume.host)
	after, readErr := os.ReadFile(f.request.ControlPath)
	if err != nil || readErr != nil || repeated != result || !bytes.Equal(before, after) {
		t.Fatal("paired reapply changed completed progress", err, readErr)
	}
}

func TestPreparationPairedMetadataRejectsChangedLineage(t *testing.T) {
	for _, fault := range []string{"missing", "duplicate", "swapped", "bytes", "companion-alias"} {
		f := newPreparationPairedFixture(t)
		switch fault {
		case "missing":
			f.plan.Derivations = f.plan.Derivations[:1]
		case "duplicate":
			f.plan.Derivations[1] = f.plan.Derivations[0]
		case "swapped":
			f.plan.Derivations[0].Original.Path, f.plan.Derivations[1].Original.Path = f.plan.Derivations[1].Original.Path, f.plan.Derivations[0].Original.Path
		case "bytes":
			if err := os.WriteFile(f.plan.Derivations[1].Original.Path, []byte("unknown original census"), 0600); err != nil {
				t.Fatal(err)
			}
		case "companion-alias":
			if err := json.Unmarshal([]byte(`{"path":"census.json","maximum_bytes":4096,"companion_path":"census.json"}`), f.plan.Owners[0].PhysicalMetadata); err != nil {
				t.Fatal(err)
			}
		}
		raw, err := json.Marshal(f.plan)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.accepted.Path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		f.accepted.Sha256 = testDigest(raw)
		if _, err := ApplyPreparationWithHost(t.Context(), f.accepted, preparationPairedTestAdapter(), f.volume.host); err == nil {
			t.Fatal("changed paired metadata lineage was admitted", fault)
		}
		names, err := os.ReadDir(f.request.RootPath)
		if err != nil || len(names) != 0 {
			t.Fatal("invalid paired lineage published target bytes", fault, err)
		}
		if _, err := os.Lstat(f.request.ControlPath); !os.IsNotExist(err) {
			t.Fatal("invalid paired lineage created progress", fault, err)
		}
	}
}

func TestPreparationPairedMetadataReconcilesInterruptedMove(t *testing.T) {
	for _, name := range []string{"census.json", "census-next.json"} {
		f := newPreparationPairedFixture(t)
		cause := errors.New("synthetic paired metadata move acknowledgement loss")
		reached := false
		_, err := applyPreparation(t.Context(), f.accepted, preparationPairedTestAdapter(), f.volume.host, daemonScope, &preparationHooks{after: func(stage, path string) error {
			if stage == "restore-target-parent-sync" && filepath.Base(path) == name {
				reached = true
				return cause
			}
			return nil
		}})
		if !reached || !errors.Is(err, cause) || !errors.Is(err, ErrPreparationUncertain) {
			t.Fatal("paired move did not reach original durable boundary", name, reached, err)
		}
		prefix, err := os.ReadFile(f.request.ControlPath)
		if err != nil {
			t.Fatal(err)
		}
		result, err := ApplyPreparationWithHost(t.Context(), f.accepted, preparationPairedTestAdapter(), f.volume.host)
		retained, readErr := os.ReadFile(f.request.ControlPath)
		if err != nil || readErr != nil || result.RestartAuthorized || !bytes.HasPrefix(retained, prefix) {
			t.Fatal("paired move cannot resume exact original progress", name, err, readErr)
		}
	}
}
