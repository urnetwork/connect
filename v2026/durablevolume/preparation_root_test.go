//go:build linux

// Root creation tests force exact source/parent/control transitions. Empty
// replacement paths never substitute for a reviewed original root generation.
package durablevolume

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// Only this fixture's known fresh root is removed. The new parent remains
// private, precreated, and explicitly named in the no-former-history fence.
func newPrivateRootPreparationFixture(t *testing.T) *preparationFixture {
	t.Helper()
	f := newPreparationFixture(t)
	if err := os.Remove(f.request.RootPath); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(f.volume.mount, "root-parent")
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	f.request.RootPath = filepath.Join(parent, "new-root")
	f.request.RootCreation = "create-private"
	var stat syscall.Stat_t
	if err := syscall.Stat(parent, &stat); err != nil {
		t.Fatal(err)
	}
	fence, err := json.Marshal(PreparationFence{Schema: PreparationFenceSchema, RootPath: f.request.RootPath, ParentInode: stat.Ino, Purpose: "fresh", FormerWritersStopped: true, NoPreviousOwnerState: true, Evidence: "synthetic private root, no former owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.request.FormerWriterFence.Path, fence, 0600); err != nil {
		t.Fatal(err)
	}
	f.request.FormerWriterFence.Sha256 = testDigest(fence)
	f.writeRequest(t)
	return f
}

// The bounded plan names exactly one private staged root and no target effects.
func privateRootPreparationPlan(t *testing.T, f *preparationFixture) {
	t.Helper()
	plan, err := PlanPreparationWithHost(t.Context(), f.reference, preparationTestAdapter(), f.volume.host)
	if err != nil {
		t.Fatal("private root plan refused", err)
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	f.plan = plan
	f.accepted = Reference{Path: filepath.Join(filepath.Dir(f.request.ControlPath), "private-plan.json"), Sha256: testDigest(raw)}
	if err := os.WriteFile(f.accepted.Path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(f.request.RootPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("planning created target", err)
	}
	info, err := os.Stat(plan.RootSource)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 || info.Sys().(*syscall.Stat_t).Ino != plan.Root.Inode {
		t.Fatal("plan lost exact private stage", err)
	}
}

// Publication preserves the reviewed inode and permanently moves its source.
func TestPreparationPrivateRootStagesBeforeExactPublication(t *testing.T) {
	f := newPrivateRootPreparationFixture(t)
	privateRootPreparationPlan(t, f)
	original, err := os.Stat(f.plan.RootSource)
	if err != nil {
		t.Fatal(err)
	}
	result, err := f.apply(t.Context(), nil)
	if err != nil || result.RestartAuthorized {
		t.Fatal("private root cannot apply exact plan", err)
	}
	target, err := os.Stat(f.request.RootPath)
	if err != nil || !os.SameFile(original, target) || target.Mode().Perm() != 0700 {
		t.Fatal("private root changed generation or mode", err)
	}
	if _, err := os.Lstat(f.plan.RootSource); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("published root kept a second source", err)
	}
	raw, err := os.ReadFile(f.request.ControlPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"kind":"root-directory"`)) {
		t.Fatal("root publication omitted retained move intent")
	}
	again, err := f.apply(t.Context(), nil)
	retained, readErr := os.ReadFile(f.request.ControlPath)
	if err != nil || readErr != nil || again != result || !bytes.Equal(raw, retained) {
		t.Fatal("completed root replay rewrote custody", err, readErr)
	}
}

// These failures happen before any apply control is created. The planner may
// leave known staging bytes, but cannot adopt aliases or unexpected payloads.
func TestPreparationPrivateRootRefusesChangedCustody(t *testing.T) {
	for _, mode := range []string{"target-exists", "source-replaced", "source-missing", "source-symlink", "source-content", "source-attribute", "parent-replaced", "unreserved-move"} {
		f := newPrivateRootPreparationFixture(t)
		privateRootPreparationPlan(t, f)
		switch mode {
		case "target-exists":
			if err := os.Mkdir(f.request.RootPath, 0700); err != nil {
				t.Fatal(err)
			}
		case "source-replaced":
			if err := os.Rename(f.plan.RootSource, f.plan.RootSource+"-retained"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(f.plan.RootSource, 0700); err != nil {
				t.Fatal(err)
			}
		case "source-missing":
			if err := os.Remove(f.plan.RootSource); err != nil {
				t.Fatal(err)
			}
		case "source-symlink":
			if err := os.Rename(f.plan.RootSource, f.plan.RootSource+"-retained"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(f.plan.RootSource+"-retained", f.plan.RootSource); err != nil {
				t.Fatal(err)
			}
		case "source-content":
			if err := os.WriteFile(filepath.Join(f.plan.RootSource, "unknown"), []byte("retained bytes"), 0600); err != nil {
				t.Fatal(err)
			}
		case "source-attribute":
			if err := syscall.Setxattr(f.plan.RootSource, "user.urnetwork.unknown-custody", []byte("retained"), 1); err != nil {
				t.Fatal(err)
			}
		case "parent-replaced":
			parent := filepath.Dir(f.request.RootPath)
			if err := os.Rename(parent, parent+"-retained"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(parent, 0700); err != nil {
				t.Fatal(err)
			}
		case "unreserved-move":
			if err := os.Rename(f.plan.RootSource, f.request.RootPath); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := f.apply(t.Context(), nil); !errors.Is(err, ErrIdentity) {
			t.Fatal("changed root custody was not refused as identity loss", mode, err)
		}
		if _, err := os.Lstat(f.request.ControlPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("changed custody started a new control", mode, err)
		}
	}
}

// Deleting a completed root cannot make either the source or target fresh.
// Reinstalling the exact retained original inode is an explicit separate act.
func TestPreparationPrivateRootCompletedLossNeverRecreates(t *testing.T) {
	f := newPrivateRootPreparationFixture(t)
	privateRootPreparationPlan(t, f)
	if _, err := f.apply(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	old, err := os.Stat(f.request.RootPath)
	if err != nil {
		t.Fatal(err)
	}
	originalControl, err := os.ReadFile(f.request.ControlPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(f.request.RootPath, f.request.RootPath+"-retained"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.apply(t.Context(), nil); !errors.Is(err, ErrIdentity) {
		t.Fatal("both missing root paths were recreated", err)
	}
	if err := os.Mkdir(f.request.RootPath, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := f.apply(t.Context(), nil); !errors.Is(err, ErrIdentity) {
		t.Fatal("empty replacement root gained original intent", err)
	}
	if err := os.Remove(f.request.RootPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(f.request.RootPath+"-retained", f.request.RootPath); err != nil {
		t.Fatal(err)
	}
	if _, err := f.apply(t.Context(), nil); err != nil {
		t.Fatal("explicit original inode restoration cannot inspect completed plan", err)
	}
	current, err := os.Stat(f.request.RootPath)
	control, readErr := os.ReadFile(f.request.ControlPath)
	if err != nil || readErr != nil || !os.SameFile(old, current) || !bytes.Equal(originalControl, control) {
		t.Fatal("refused recovery rewrote original custody", err, readErr)
	}
}

// Every acknowledgement boundary follows the real syscall. A joined retry
// retains that root and prefix, including a rename before either parent sync.
func TestPreparationPrivateRootParentSyncLossResumesOriginal(t *testing.T) {
	for _, stage := range []string{"root-reservation", "control-pending", "root-rename", "root-source-parent-sync", "root-target-parent-sync", "control-complete"} {
		f := newPrivateRootPreparationFixture(t)
		privateRootPreparationPlan(t, f)
		original, err := os.Stat(f.plan.RootSource)
		if err != nil {
			t.Fatal(err)
		}
		fired := false
		_, err = f.apply(t.Context(), &preparationHooks{after: func(operation, path string) error {
			if !fired && operation == stage && path == f.request.RootPath {
				fired = true
				return syscall.EIO
			}
			return nil
		}})
		if !fired || !errors.Is(err, ErrPreparationUncertain) || !errors.Is(err, syscall.EIO) {
			t.Fatal("root failure lost uncertainty", stage, fired, err)
		}
		before, err := os.ReadFile(f.request.ControlPath)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.apply(t.Context(), nil); err != nil {
			t.Fatal("joined root could not retain original step", stage, err)
		}
		current, err := os.Stat(f.request.RootPath)
		after, readErr := os.ReadFile(f.request.ControlPath)
		if err != nil || readErr != nil || !os.SameFile(original, current) || !bytes.HasPrefix(after, before) {
			t.Fatal("root recovery replaced inode or discarded prefix", stage, err, readErr)
		}
	}
}

// A real child exits at each root publication barrier under private umask.
// The parent's joined readback cannot depend on a deferred close or callback.
func TestPreparationPrivateRootChildCrashJoinsBeforeResume(t *testing.T) {
	if path := os.Getenv("URNETWORK_PRIVATE_ROOT_CRASH_PLAN"); path != "" {
		syscall.Umask(0077)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var plan PreparationPlan
		if err := json.Unmarshal(raw, &plan); err != nil {
			t.Fatal(err)
		}
		host := &fixtureHost{mounts: []Mount{{Id: 1, ParentId: 1, Device: Device{Major: plan.Mount.Device.Major ^ 1, Minor: plan.Mount.Device.Minor}, Root: "/", Path: "/", FilesystemType: "ext4"}, plan.Mount}, uuidDevice: plan.Mount.Device, filesystem: plan.Filesystem}
		_, err = applyPreparation(t.Context(), Reference{Path: path, Sha256: testDigest(raw)}, preparationTestAdapter(), host, daemonScope, &preparationHooks{after: func(stage, path string) error {
			if stage == os.Getenv("URNETWORK_PRIVATE_ROOT_CRASH_STAGE") {
				os.Exit(74)
			}
			return nil
		}})
		t.Fatal("private-root crash barrier not reached", err)
	}
	for _, stage := range []string{"root-reservation", "control-pending", "root-rename", "root-source-parent-sync", "root-target-parent-sync", "control-complete"} {
		f := newPrivateRootPreparationFixture(t)
		privateRootPreparationPlan(t, f)
		binary, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(binary, "-test.run=^TestPreparationPrivateRootChildCrashJoinsBeforeResume$")
		cmd.Env = append(os.Environ(), "URNETWORK_PRIVATE_ROOT_CRASH_PLAN="+f.accepted.Path, "URNETWORK_PRIVATE_ROOT_CRASH_STAGE="+stage)
		raw, err := cmd.CombinedOutput()
		var exited *exec.ExitError
		if !errors.As(err, &exited) || exited.ExitCode() != 74 {
			t.Fatal("child did not exit after original real syscall", stage, err, string(raw))
		}
		if _, err := f.apply(t.Context(), nil); err != nil {
			t.Fatal("joined crashed root cannot resume", stage, err)
		}
		info, err := os.Stat(f.request.RootPath)
		if err != nil || info.Mode().Perm() != 0700 || info.Sys().(*syscall.Stat_t).Ino != f.plan.Root.Inode {
			t.Fatal("crash resumed another root generation", stage, err)
		}
	}
}

// Pressure is pre-admission and leaves the reviewed stage reusable. A real
// no-replace syscall independently refuses an already existing target inode.
func TestPreparationPrivateRootUnavailableAndCompetingTarget(t *testing.T) {
	f := newPrivateRootPreparationFixture(t)
	privateRootPreparationPlan(t, f)
	original := f.volume.host.filesystem.AvailableBytes
	f.volume.host.filesystem.AvailableBytes = 0
	if _, err := f.apply(t.Context(), nil); !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrIdentity) || errors.Is(err, ErrPreparationUncertain) {
		t.Fatal("reserve pressure became identity loss or publication", err)
	}
	if _, err := os.Lstat(f.request.ControlPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("pre-admission pressure consumed root intent", err)
	}
	f.volume.host.filesystem.AvailableBytes = original
	if err := os.Mkdir(f.request.RootPath, 0700); err != nil {
		t.Fatal(err)
	}
	sourceBefore, err := os.Stat(f.plan.RootSource)
	if err != nil {
		t.Fatal(err)
	}
	targetBefore, err := os.Stat(f.request.RootPath)
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(f.request.StagingDirectory)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	target, err := os.Open(filepath.Dir(f.request.RootPath))
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if err := preparationRenameRoot(source, filepath.Base(f.plan.RootSource), target, filepath.Base(f.request.RootPath)); !errors.Is(err, syscall.EEXIST) {
		t.Fatal("root move replaced competing target", err)
	}
	sourceAfter, err := os.Stat(f.plan.RootSource)
	if err != nil {
		t.Fatal(err)
	}
	targetAfter, err := os.Stat(f.request.RootPath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(sourceBefore, sourceAfter) || !os.SameFile(targetBefore, targetAfter) {
		t.Fatal("no-replace refusal changed either generation")
	}
	if err := os.Remove(f.request.RootPath); err != nil {
		t.Fatal(err)
	}
	if _, err := f.apply(context.Background(), nil); err != nil {
		t.Fatal("same reviewed root cannot proceed after pressure and refused rival", err)
	}
}
