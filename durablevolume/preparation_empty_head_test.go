//go:build linux

// Empty fresh owners still require an exact bounded checkpoint; absence of
// payload files is not absence of custody or permission to infer a new owner.
package durablevolume

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// The synthetic fixed profile has no payload until its first runtime commit.
func preparationEmptyHeadAdapter() PreparationAdapter {
	return PreparationAdapter{Build: func(ctx context.Context, parent *os.File, name string, owner PreparationOwner) (PreparationOwnerPlan, error) {
		if err := ctx.Err(); err != nil {
			return PreparationOwnerPlan{}, err
		}
		if err := syscall.Mkdirat(int(parent.Fd()), name, 0700); err != nil {
			return PreparationOwnerPlan{}, err
		}
		return PreparationOwnerPlan{Owner: owner, StagingName: name,
			Census:     json.RawMessage(`{"schema":"synthetic-empty-head","present":false}`),
			Attributes: []PreparationAttributeSpec{{Path: ".", Name: "user.urnetwork.snapshot.synthetic-empty"}}}, parent.Sync()
	}, Inspect: func(ctx context.Context, root *os.File, owner PreparationOwnerPlan) ([]PreparedAttribute, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		identity, err := preparationIdentity(root)
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(struct {
			Inode   uint64
			Present bool
		}{Inode: identity.Inode})
		if err != nil {
			return nil, err
		}
		return []PreparedAttribute{{Spec: owner.Attributes[0], Raw: raw}}, nil
	}}
}

// Plan and apply use their public production entrypoints with facts-only Host.
func preparationEmptyHeadPlan(t *testing.T, f *preparationFixture) {
	t.Helper()
	plan, err := PlanPreparationWithHost(t.Context(), f.reference, preparationEmptyHeadAdapter(), f.volume.host)
	if err != nil {
		t.Fatal("exact empty-head owner cannot be planned", err)
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	f.plan = plan
	f.accepted = Reference{Path: filepath.Join(filepath.Dir(f.request.ControlPath), "empty-plan.json"), Sha256: testDigest(raw)}
	if err := os.WriteFile(f.accepted.Path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

// A file-free owner must produce its exact root checkpoint and no fake payload.
func TestPreparationAttributeOnlyOwnerPublishesExactAbsentHead(t *testing.T) {
	f := newPreparationFixture(t)
	preparationEmptyHeadPlan(t, f)
	if len(f.plan.Sources) != 0 || len(f.plan.Owners) != 1 {
		t.Fatal("empty head gained payload sources")
	}
	result, err := ApplyPreparationWithHost(t.Context(), f.accepted, preparationEmptyHeadAdapter(), f.volume.host)
	if err != nil || result.RestartAuthorized {
		t.Fatal("empty-head apply refused or authorized restart", err)
	}
	names, err := os.ReadDir(f.request.RootPath)
	if err != nil || len(names) != 0 {
		t.Fatal("empty-head apply fabricated payload", err)
	}
	raw := make([]byte, 4096)
	n, err := syscall.Getxattr(f.request.RootPath, "user.urnetwork.snapshot.synthetic-empty", raw)
	if err != nil || n == 0 {
		t.Fatal("empty-head checkpoint was not retained", err)
	}
	before, err := os.ReadFile(f.request.ControlPath)
	if err != nil {
		t.Fatal(err)
	}
	again, err := ApplyPreparationWithHost(t.Context(), f.accepted, preparationEmptyHeadAdapter(), f.volume.host)
	after, readErr := os.ReadFile(f.request.ControlPath)
	if err != nil || readErr != nil || result != again || string(before) != string(after) {
		t.Fatal("completed empty-head replay changed custody", err, readErr)
	}
}

// Empty, unbounded or unbound authority is still refused before target effects.
func TestPreparationAttributeOnlyOwnerRequiresExactAuthority(t *testing.T) {
	for _, mode := range []string{"no-census", "no-attribute", "absent-destination", "capacity"} {
		f := newPreparationFixture(t)
		adapter := preparationEmptyHeadAdapter()
		build := adapter.Build
		adapter.Build = func(ctx context.Context, parent *os.File, name string, owner PreparationOwner) (PreparationOwnerPlan, error) {
			plan, err := build(ctx, parent, name, owner)
			if err != nil {
				return plan, err
			}
			switch mode {
			case "no-census":
				plan.Census = nil
			case "no-attribute":
				plan.Attributes = nil
			case "absent-destination":
				plan.Attributes[0].Path = "missing"
			case "capacity":
				for i := 0; i < 5; i++ {
					plan.Attributes = append(plan.Attributes, PreparationAttributeSpec{Path: ".", Name: "user.urnetwork.snapshot.extra-" + string(rune('a'+i))})
				}
			}
			return plan, nil
		}
		if _, err := PlanPreparationWithHost(t.Context(), f.reference, adapter, f.volume.host); err == nil {
			t.Fatal("empty owner gained unbound authority", mode)
		}
		names, err := os.ReadDir(f.request.RootPath)
		if err != nil || len(names) != 0 {
			t.Fatal("refused empty owner mutated root", mode, err)
		}
		if _, err := os.Lstat(f.request.ControlPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("refused empty owner started apply", mode, err)
		}
	}
}

// An acknowledged attribute is not rebuilt after loss. A lost completion ack
// instead resumes only its exact original fully written bytes after close.
func TestPreparationAttributeOnlyOwnerResumesExactPendingHead(t *testing.T) {
	f := newPreparationFixture(t)
	preparationEmptyHeadPlan(t, f)
	fired := false
	hook := &preparationHooks{after: func(stage, path string) error {
		if stage == "attribute-sync" && strings.HasSuffix(path, ":user.urnetwork.snapshot.synthetic-empty") {
			fired = true
			return syscall.EIO
		}
		return nil
	}}
	_, err := applyPreparation(t.Context(), f.accepted, preparationEmptyHeadAdapter(), f.volume.host, daemonScope, hook)
	if !fired || !errors.Is(err, ErrPreparationUncertain) || !errors.Is(err, syscall.EIO) {
		t.Fatal("did not retain exact empty-head uncertainty", fired, err)
	}
	raw := make([]byte, 4096)
	n, err := syscall.Getxattr(f.request.RootPath, "user.urnetwork.snapshot.synthetic-empty", raw)
	if err != nil {
		t.Fatal(err)
	}
	retained := string(raw[:n])
	if _, err := ApplyPreparationWithHost(t.Context(), f.accepted, preparationEmptyHeadAdapter(), f.volume.host); err != nil {
		t.Fatal("exact empty-head readback cannot resume", err)
	}
	n, err = syscall.Getxattr(f.request.RootPath, "user.urnetwork.snapshot.synthetic-empty", raw)
	if err != nil || string(raw[:n]) != retained {
		t.Fatal("resume rewrote original empty head", err)
	}
	if err := syscall.Removexattr(f.request.RootPath, "user.urnetwork.snapshot.synthetic-empty"); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyPreparationWithHost(t.Context(), f.accepted, preparationEmptyHeadAdapter(), f.volume.host); !errors.Is(err, ErrIdentity) {
		t.Fatal("missing completed empty head was recreated", err)
	}
}

// Exclusive namespace intent is part of the exact plan, not a later advisory.
func TestPreparationExclusiveHeadRefusesSharedNamespace(t *testing.T) {
	f := newPreparationFixture(t)
	f.request.Owners = append(f.request.Owners, PreparationOwner{Kind: "second-synthetic", RelativePath: ".", Purpose: "fresh", Inputs: json.RawMessage(`{"public":true}`)})
	f.writeRequest(t)
	adapter := preparationEmptyHeadAdapter()
	build := adapter.Build
	adapter.Build = func(ctx context.Context, parent *os.File, name string, owner PreparationOwner) (PreparationOwnerPlan, error) {
		plan, err := build(ctx, parent, name, owner)
		plan.ExclusiveRoot = true
		return plan, err
	}
	if _, err := PlanPreparationWithHost(t.Context(), f.reference, adapter, f.volume.host); err == nil || !strings.Contains(err.Error(), "exclusive root namespace") {
		t.Fatal("exclusive owner reached conflicting peer or wrong failure", err)
	}
	if _, err := os.Lstat(f.request.ControlPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("shared exclusive roots started publication", err)
	}
}
