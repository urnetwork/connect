//go:build linux || darwin

// A completed restore has moved every staged member into its target. Its
// recorded progress decides whether a derivation replay borrows the published
// members or, for an incomplete apply, the staged sources exactly as before.
package durablevolume

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// Like the SDK owner, this rebinding also reads each borrowed member by its
// source name and refuses any changed generation or bytes before deriving.
func preparationBorrowingPhysicalTestAdapter(borrowed *[]string) PreparationAdapter {
	adapter := preparationPhysicalTestAdapter()
	rebind := adapter.RebindRestore
	adapter.RebindRestore = func(ctx context.Context, owner PreparationOwnerPlan, report Inventory, raw []byte, targets []PreparationSource) ([]byte, error) {
		for _, target := range targets {
			file, err := preparationOpenAbsolute(target.Path, false)
			if err != nil {
				return nil, err
			}
			identity, observeErr := preparationIdentity(file)
			if observeErr == nil && identity != target.Identity {
				observeErr = errors.Join(ErrIdentity, errors.New("synthetic borrowed member generation differs"))
			}
			if observeErr == nil {
				observeErr = preparationVerifyFile(ctx, file, target.File.Bytes, target.File.Sha256)
			}
			if err := errors.Join(observeErr, file.Close()); err != nil {
				return nil, err
			}
			*borrowed = append(*borrowed, target.Path)
		}
		return rebind(ctx, owner, report, raw, targets)
	}
	return adapter
}

// The derivation's only borrowed member is the record; the census is derived.
func preparationPhysicalRecordPaths(t *testing.T, f *preparationFixture) (staged, published string) {
	t.Helper()
	for _, source := range f.plan.Sources {
		if source.File.Path == "record.bin" {
			return source.Path, filepath.Join(f.request.RootPath, source.File.Path)
		}
	}
	t.Fatal("physical plan lost its borrowed record")
	return "", ""
}

// One failed invocation loses the acknowledgement at the named publication.
func preparationPhysicalInterrupt(t *testing.T, f *preparationFixture, adapter PreparationAdapter, stage, path string) {
	t.Helper()
	cause := errors.New("synthetic lost acknowledgement")
	reached := false
	_, err := applyPreparation(t.Context(), f.accepted, adapter, f.volume.host, daemonScope, &preparationHooks{after: func(observed, observedPath string) error {
		if observed == stage && observedPath == path {
			reached = true
			return cause
		}
		return nil
	}})
	if !reached || !errors.Is(err, cause) || !errors.Is(err, ErrPreparationUncertain) {
		t.Fatal("apply did not lose the intended acknowledgement", stage, path, reached, err)
	}
}

// The listing and journal bytes a refused invocation must leave unchanged.
func preparationPhysicalSnapshot(t *testing.T, f *preparationFixture) (names []string, control []byte) {
	t.Helper()
	entries, err := os.ReadDir(f.request.RootPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	control, err = os.ReadFile(f.request.ControlPath)
	if err != nil {
		t.Fatal(err)
	}
	return names, control
}

// A lost final acknowledgement re-runs a completed restore. The derivation is
// replayed over the published member, not the staged name its move consumed,
// and every later invocation returns the same result without new progress.
func TestPreparationPhysicalCompletedRestoreReappliesIdempotently(t *testing.T) {
	f := newPreparationPhysicalFixture(t)
	staged, published := preparationPhysicalRecordPaths(t, f)
	var borrowed []string
	adapter := preparationBorrowingPhysicalTestAdapter(&borrowed)
	preparationPhysicalInterrupt(t, f, adapter, "control-complete", f.request.DeclarationPath)
	if len(borrowed) != 1 || borrowed[0] != staged {
		t.Fatal("first admission did not borrow the staged member", borrowed)
	}
	before, err := os.ReadFile(f.request.ControlPath)
	if err != nil {
		t.Fatal(err)
	}
	var first PreparationResult
	for attempt := range 2 {
		borrowed = nil
		result, err := ApplyPreparationWithHost(t.Context(), f.accepted, adapter, f.volume.host)
		after, readErr := os.ReadFile(f.request.ControlPath)
		if err != nil || readErr != nil || !bytes.Equal(before, after) {
			t.Fatal("completed derivation did not re-apply idempotently", attempt, err, readErr)
		}
		if len(borrowed) != 1 || borrowed[0] != published {
			t.Fatal("completed derivation did not borrow its published member", attempt, borrowed)
		}
		if attempt == 0 {
			first = result
		} else if result != first {
			t.Fatal("repeated completion changed its result", result, first)
		}
		assertPreparationPhysicalResult(t, f, result)
	}
}

// Progress short of the declaration is incomplete: a header alone, an
// acknowledged prefix or a pending move. Re-admission still borrows the staged
// member and the joined invocation finishes the original journal.
func TestPreparationPhysicalIncompleteRestoreResumes(t *testing.T) {
	for _, stage := range []string{"control-header", "control-complete", "restore-target-parent-sync"} {
		f := newPreparationPhysicalFixture(t)
		staged, _ := preparationPhysicalRecordPaths(t, f)
		var borrowed []string
		adapter := preparationBorrowingPhysicalTestAdapter(&borrowed)
		path := filepath.Join(f.request.RootPath, "census.json")
		if stage == "control-header" {
			path = f.request.ControlPath
		}
		preparationPhysicalInterrupt(t, f, adapter, stage, path)
		prefix, err := os.ReadFile(f.request.ControlPath)
		if err != nil {
			t.Fatal(err)
		}
		borrowed = nil
		result, err := ApplyPreparationWithHost(t.Context(), f.accepted, adapter, f.volume.host)
		after, readErr := os.ReadFile(f.request.ControlPath)
		if err != nil || readErr != nil || !bytes.HasPrefix(after, prefix) || len(after) == len(prefix) {
			t.Fatal("incomplete derivation did not resume its original progress", stage, err, readErr)
		}
		if len(borrowed) != 1 || borrowed[0] != staged {
			t.Fatal("incomplete derivation stopped borrowing its staged member", stage, borrowed)
		}
		assertPreparationPhysicalResult(t, f, result)
	}
}

// Completed progress never excuses a different result. Changed or missing
// members, custody, original lineage or progress refuse before any new step,
// and no tampered member is repaired, moved or acknowledged again.
func TestPreparationPhysicalCompletedRestoreRefusesTamperedDestination(t *testing.T) {
	for _, tamper := range []struct {
		name     string
		want     error
		borrowed int
	}{
		{"derived-census", ErrIdentity, 0},
		{"missing-member", ErrIdentity, 0},
		{"custody-checkpoint", ErrIdentity, 1},
		{"original-census", ErrIdentity, 0},
		{"moved-back", syscall.ENOENT, 0},
		{"torn-progress", ErrPreparationUncertain, 0},
		{"linked-progress", syscall.ELOOP, 0},
	} {
		f := newPreparationPhysicalFixture(t)
		staged, published := preparationPhysicalRecordPaths(t, f)
		var borrowed []string
		adapter := preparationBorrowingPhysicalTestAdapter(&borrowed)
		if _, err := ApplyPreparationWithHost(t.Context(), f.accepted, adapter, f.volume.host); err != nil {
			t.Fatal(err)
		}
		switch tamper.name {
		case "derived-census":
			if err := os.WriteFile(filepath.Join(f.request.RootPath, "census.json"), []byte(`{"schema":"unreviewed"}`), 0600); err != nil {
				t.Fatal(err)
			}
		case "missing-member":
			if err := os.Remove(published); err != nil {
				t.Fatal(err)
			}
		case "custody-checkpoint":
			if err := unix.Setxattr(f.request.RootPath, "user.urnetwork.attempt-ledger-custody", []byte(`{"schema":"unreviewed"}`), 0); err != nil {
				t.Fatal(err)
			}
		case "original-census":
			if err := os.WriteFile(f.plan.Derivations[0].Original.Path, []byte(`{"schema":"unreviewed"}`), 0600); err != nil {
				t.Fatal(err)
			}
		case "moved-back":
			if err := os.Rename(published, staged); err != nil {
				t.Fatal(err)
			}
		case "torn-progress":
			control, err := os.OpenFile(f.request.ControlPath, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, writeErr := control.WriteString(`{"sequence":`)
			if err := errors.Join(writeErr, control.Close()); err != nil {
				t.Fatal(err)
			}
		case "linked-progress":
			if err := os.Rename(f.request.ControlPath, f.request.ControlPath+".held"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(f.request.ControlPath+".held", f.request.ControlPath); err != nil {
				t.Fatal(err)
			}
		}
		names, control := preparationPhysicalSnapshot(t, f)
		borrowed = nil
		_, err := ApplyPreparationWithHost(t.Context(), f.accepted, adapter, f.volume.host)
		if !errors.Is(err, tamper.want) || len(borrowed) != tamper.borrowed {
			t.Fatal("tampered completion was not refused at its boundary", tamper.name, err, borrowed)
		}
		if after, afterControl := preparationPhysicalSnapshot(t, f); !bytes.Equal(control, afterControl) || !slices.Equal(after, names) {
			t.Fatal("refused completion changed its target or progress", tamper.name, names, after)
		}
		if tamper.name == "moved-back" {
			if _, err := os.Lstat(staged); err != nil {
				t.Fatal("refused completion moved its member again", err)
			}
		}
	}
}

// An incomplete apply keeps the staged-source replay. A deleted stage and a
// stage its interrupted move already consumed both refuse like lost custody.
func TestPreparationPhysicalIncompleteRestoreRefusesMissingStagedSource(t *testing.T) {
	for _, mode := range []string{"deleted", "consumed"} {
		f := newPreparationPhysicalFixture(t)
		staged, published := preparationPhysicalRecordPaths(t, f)
		var borrowed []string
		adapter := preparationBorrowingPhysicalTestAdapter(&borrowed)
		if mode == "deleted" {
			preparationPhysicalInterrupt(t, f, adapter, "restore-target-parent-sync", filepath.Join(f.request.RootPath, "census.json"))
			if err := os.Remove(staged); err != nil {
				t.Fatal(err)
			}
		} else {
			preparationPhysicalInterrupt(t, f, adapter, "restore-member-rename", published)
			if _, err := os.Lstat(staged); !os.IsNotExist(err) {
				t.Fatal("interrupted move left its staged source", err)
			}
		}
		names, control := preparationPhysicalSnapshot(t, f)
		borrowed = nil
		_, err := ApplyPreparationWithHost(t.Context(), f.accepted, adapter, f.volume.host)
		if !errors.Is(err, ErrIdentity) || mode == "consumed" && !errors.Is(err, syscall.ENOENT) || len(borrowed) != 0 {
			t.Fatal("missing staged source of an incomplete apply was admitted", mode, err, borrowed)
		}
		if after, afterControl := preparationPhysicalSnapshot(t, f); !bytes.Equal(control, afterControl) || !slices.Equal(after, names) {
			t.Fatal("refused incomplete apply changed its target or progress", mode, names, after)
		}
		if _, err := os.Lstat(f.request.DeclarationPath); !os.IsNotExist(err) {
			t.Fatal("refused incomplete apply published its declaration", mode, err)
		}
	}
}
