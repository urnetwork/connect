//go:build linux

// Complete shared coverage is checked before staging. Synthetic fixed owners
// exercise the real public plan/apply, retained heads and joined recovery.
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

const preparationUnionSecondAttribute = "user.urnetwork.snapshot.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// The same public-byte grammar has two disjoint fixed names and checkpoints.
func preparationUnionTestAdapter() PreparationAdapter {
	return PreparationAdapter{
		Restore: func(ctx context.Context, name string, owner PreparationOwner, report Inventory) (PreparationOwnerPlan, error) {
			fileName, attributeName := "record.bin", "user.urnetwork.attempt-ledger-custody"
			if owner.Kind == "synthetic-second" {
				fileName, attributeName = "second.bin", preparationUnionSecondAttribute
			} else if owner.Kind != "synthetic-test-only" {
				return PreparationOwnerPlan{}, errors.New("synthetic union owner is unknown")
			}
			var record InventoryEntry
			var original []byte
			for _, entry := range report.Entries {
				if entry.Path == fileName {
					record = entry
				}
				if entry.Path == "" {
					for _, attribute := range entry.OwnerAttributes {
						if attribute.Name == attributeName {
							original = attribute.Value
						}
					}
				}
			}
			var checkpoint struct {
				Schema string
				Inode  uint64
				Sha256 string
			}
			if record.Path == "" || json.Unmarshal(original, &checkpoint) != nil || checkpoint.Schema != "synthetic-preparation-test" || checkpoint.Inode != report.PhysicalRoot.Inode || checkpoint.Sha256 != record.Sha256 {
				return PreparationOwnerPlan{}, errors.New("synthetic union lost original checkpoint authority")
			}
			return PreparationOwnerPlan{Owner: owner, StagingName: name,
				Files:      []PreparationFile{{Path: record.Path, Kind: record.Kind, Mode: record.Mode, Bytes: record.Size, Sha256: record.Sha256}},
				Attributes: []PreparationAttributeSpec{{Path: ".", Name: attributeName}}, Census: append(json.RawMessage(nil), original...)}, ctx.Err()
		},
		InspectRestore: func(ctx context.Context, root *os.File, owner PreparationOwnerPlan, report Inventory) ([]PreparedAttribute, error) {
			member := owner.Files[0]
			fd, err := syscall.Openat(int(root.Fd()), member.Path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
			if err != nil {
				return nil, err
			}
			file := os.NewFile(uintptr(fd), member.Path)
			if err := errors.Join(preparationVerifyFile(ctx, file, member.Bytes, member.Sha256), file.Close()); err != nil {
				return nil, err
			}
			identity, err := preparationIdentity(root)
			if err != nil {
				return nil, err
			}
			raw, err := json.Marshal(struct {
				Schema string
				Inode  uint64
				Sha256 string
			}{Schema: "synthetic-preparation-test", Inode: identity.Inode, Sha256: member.Sha256})
			return []PreparedAttribute{{Spec: owner.Attributes[0], Raw: raw}}, errors.Join(err, ctx.Err())
		},
	}
}

// The second owner exists before original inventory/export; it is never added
// to an already accepted historical report to disguise omitted custody.
func newPreparationUnionFixture(t *testing.T) *preparationFixture {
	t.Helper()
	f := newPreparationRestoreFixture(t)
	raw := []byte("second exact synthetic original\n")
	for _, root := range []string{f.volume.root, f.request.RestoreSource.Directory} {
		if err := os.WriteFile(filepath.Join(root, "second.bin"), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	var stat syscall.Stat_t
	if err := syscall.Stat(f.volume.root, &stat); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := json.Marshal(struct {
		Schema string
		Inode  uint64
		Sha256 string
	}{Schema: "synthetic-preparation-test", Inode: stat.Ino, Sha256: testDigest(raw)})
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{f.volume.root, f.request.RestoreSource.Directory} {
		if err := syscall.Setxattr(root, preparationUnionSecondAttribute, checkpoint, 1); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := f.volume.open(t, Snapshot)
	report, err := snapshot.InventoryPhysical(t.Context(), f.request.RestoreSource.FormerWriterFence, InventoryLimits{MaxEntries: 16, MaxBytes: 1024 * 1024, MaxDepth: 4, MaxOwnerAttributes: 4, MaxOwnerAttributeBytes: 16384})
	if closeErr := snapshot.Close(); err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.request.RestoreSource.Inventory.Path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	f.request.RestoreSource.Inventory.Sha256 = testDigest(encoded)
	f.request.Owners = append(f.request.Owners, PreparationOwner{Kind: "synthetic-second", RelativePath: ".", Purpose: "restore", Inputs: json.RawMessage(`{"public":true}`)})
	writePreparationUnionRequest(t, f)
	return f
}

// Literal wire construction also compiles against the strict old decoder.
func writePreparationUnionRequest(t *testing.T, f *preparationFixture) {
	t.Helper()
	f.writeRequest(t)
	raw, err := os.ReadFile(f.reference.Path)
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]json.RawMessage
	var owners []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &request); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(request["owners"], &owners); err != nil {
		t.Fatal(err)
	}
	for _, owner := range owners {
		owner["restore_coverage"] = json.RawMessage(`"complete-union-v1"`)
	}
	request["owners"], err = json.Marshal(owners)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.reference.Path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	f.reference.Sha256 = testDigest(raw)
}

// The public core must retain both owners, complete both heads, and return the
// identical completed result after all old users have joined.
func TestPreparationRestoreCompleteUnionKeepsEveryHead(t *testing.T) {
	f := newPreparationUnionFixture(t)
	adapter := preparationUnionTestAdapter()
	plan, err := PlanPreparationWithHost(t.Context(), f.reference, adapter, f.volume.host)
	if err != nil || len(plan.Owners) != 2 || len(plan.Sources) != 2 {
		t.Fatal("complete shared restore cannot plan both owners", err)
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	reference := Reference{Path: filepath.Join(filepath.Dir(f.request.ControlPath), "union-plan.json"), Sha256: testDigest(raw)}
	if err := os.WriteFile(reference.Path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	first, err := ApplyPreparationWithHost(t.Context(), reference, adapter, f.volume.host)
	if err != nil || first.RestartAuthorized {
		t.Fatal("complete shared restore cannot publish both heads", err)
	}
	before, err := os.ReadFile(f.request.ControlPath)
	if err != nil {
		t.Fatal(err)
	}
	again, err := ApplyPreparationWithHost(t.Context(), reference, adapter, f.volume.host)
	after, readErr := os.ReadFile(f.request.ControlPath)
	if err != nil || readErr != nil || first != again || !bytes.Equal(before, after) {
		t.Fatal("completed union restore changed retained progress", err, readErr)
	}
	root, err := os.Open(f.request.RootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, owner := range plan.Owners {
		for _, member := range owner.Files {
			actual, err := os.ReadFile(filepath.Join(f.request.RootPath, member.Path))
			if err != nil || testDigest(actual) != member.Sha256 {
				t.Fatal("union restore lost original payload", member.Path, err)
			}
		}
		if _, err := readInventoryAttribute(root, owner.Attributes[0].Name, 4096); err != nil {
			t.Fatal("union restore omitted an original head", err)
		}
	}
}

// Refusal happens during pure coverage, before any owner staging directory,
// target file or target checkpoint is created.
func TestPreparationRestoreUnionRefusesMissingOverlapAndChangedClaims(t *testing.T) {
	for _, mode := range []string{"omitted-owner", "overlap", "omitted-file", "omitted-head", "changed-file", "invented-head"} {
		f := newPreparationUnionFixture(t)
		if mode == "omitted-owner" {
			f.request.Owners = f.request.Owners[:1]
			writePreparationUnionRequest(t, f)
		}
		adapter := preparationUnionTestAdapter()
		original := adapter.Restore
		adapter.Restore = func(ctx context.Context, name string, owner PreparationOwner, report Inventory) (PreparationOwnerPlan, error) {
			if mode == "overlap" && owner.Kind == "synthetic-second" {
				first := owner
				first.Kind = "synthetic-test-only"
				plan, err := original(ctx, name, first, report)
				plan.Owner = owner
				return plan, err
			}
			plan, err := original(ctx, name, owner, report)
			if owner.Kind == "synthetic-second" {
				switch mode {
				case "omitted-file":
					plan.Files = nil
				case "omitted-head":
					plan.Attributes = nil
				case "changed-file":
					plan.Files[0].Sha256 = testDigest([]byte("invented data"))
				case "invented-head":
					plan.Attributes[0].Name = "user.urnetwork.snapshot." + strings.Repeat("b", 64)
				}
			}
			return plan, err
		}
		if _, err := PlanPreparationWithHost(t.Context(), f.reference, adapter, f.volume.host); err == nil || !strings.Contains(err.Error(), "coverage") {
			t.Fatal("bad union did not fail at complete coverage", mode, err)
		}
		for _, path := range []string{f.request.RootPath, f.request.StagingDirectory} {
			entries, err := os.ReadDir(path)
			if err != nil || len(entries) != 0 {
				t.Fatal("bad union performed staging or target effects", mode, path, err)
			}
		}
	}
}
