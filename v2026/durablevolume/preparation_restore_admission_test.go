//go:build linux || darwin

package durablevolume

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// The accepted plan already contains the copied archive's physical identity.
// Staged payloads cannot silently replace that original source after planning.
func TestPreparationRestoreRechecksOriginalArchiveBeforeEffects(t *testing.T) {
	for _, fault := range []string{"missing-member", "same-size-bytes", "missing-head", "missing-generation", "replaced-archive", "busy-archive"} {
		f := newPreparationRestoreFixture(t)
		preparationRestoreAccept(t, f)
		archive := f.request.RestoreSource.Directory
		var held *os.File
		var err error
		switch fault {
		case "missing-member":
			err = os.Remove(filepath.Join(archive, "record.bin"))
		case "same-size-bytes":
			raw, readErr := os.ReadFile(filepath.Join(archive, "record.bin"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			raw[0] ^= 1
			err = os.WriteFile(filepath.Join(archive, "record.bin"), raw, 0600)
		case "missing-head":
			err = unix.Removexattr(archive, "user.urnetwork.attempt-ledger-custody")
		case "missing-generation":
			err = unix.Removexattr(archive, RootGenerationAttribute)
		case "replaced-archive":
			err = os.Rename(archive, archive+".retained")
			if err == nil {
				err = os.Mkdir(archive, 0700)
			}
		case "busy-archive":
			held, err = os.Open(archive)
			if err == nil {
				err = syscall.Flock(int(held.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			}
		}
		if err != nil {
			t.Fatal("fault did not reach accepted original", fault, err)
		}
		result, err := ApplyPreparationWithHost(t.Context(), f.accepted, preparationRestoreTestAdapter(), f.volume.host)
		if held != nil {
			if closeErr := held.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
		}
		want := ErrIdentity
		if fault == "busy-archive" {
			want = ErrBusy
		}
		if !errors.Is(err, want) || !reflect.DeepEqual(result, PreparationResult{}) {
			t.Fatal("lost original source was replaced by its staged copy", fault, result, err)
		}
		entries, err := os.ReadDir(f.request.RootPath)
		if err != nil || len(entries) != 0 {
			t.Fatal("source refusal changed target members", fault, err)
		}
		for _, path := range []string{f.request.ControlPath, f.request.MarkerPath, f.request.LeasePath, f.request.DeclarationPath} {
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("source refusal published target authority", fault, path, err)
			}
		}
	}
}

// Cold archive admission hashes actual bytes once. Target journals, metadata
// scans and repeated unchanged checks cannot reread those payloads per step.
func TestPreparationRestoreAuthenticatesCopiedPayloadOnlyOncePerOpen(t *testing.T) {
	f := newPreparationRestoreFixture(t)
	preparationRestoreAccept(t, f)
	path := filepath.Join(f.request.RestoreSource.Directory, "record.bin")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	total, calls := 0, 0
	hooks := &preparationHooks{sourceRead: func(current string, n int) {
		if current != path || n <= 0 {
			t.Fatal("unexpected archive admission read", current, n)
		}
		total += n
		calls++
	}}
	for attempt := 0; attempt < 2; attempt++ {
		total, calls = 0, 0
		result, err := applyPreparation(t.Context(), f.accepted, preparationRestoreTestAdapter(), f.volume.host, daemonScope, hooks)
		if err != nil || result.RestartAuthorized || total != len(raw) || calls != 1 {
			t.Fatal("archive admission repeated or omitted actual byte work", attempt, total, calls, err)
		}
	}
}

// The real post-read observation may fail without proving identity loss. A new
// invocation can use the exact same plan when the original facts recover.
func TestPreparationRestoreArchiveObservationRetainsRetryableCause(t *testing.T) {
	f := newPreparationRestoreFixture(t)
	preparationRestoreAccept(t, f)
	reads := 0
	_, err := applyPreparation(t.Context(), f.accepted, preparationRestoreTestAdapter(), f.volume.host, daemonScope, &preparationHooks{sourceRead: func(string, int) {
		reads++
		f.volume.host.change(func() { f.volume.host.mountsErr = syscall.EIO })
	}})
	f.volume.host.change(func() { f.volume.host.mountsErr = nil })
	if reads != 1 || !errors.Is(err, ErrUnavailable) || !errors.Is(err, syscall.EIO) || errors.Is(err, ErrIdentity) {
		t.Fatal("unobservable copied source became proven identity loss", reads, err)
	}
	if _, err := os.Stat(f.request.ControlPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unavailable original source reserved target", err)
	}
	if _, err := ApplyPreparationWithHost(t.Context(), f.accepted, preparationRestoreTestAdapter(), f.volume.host); err != nil {
		t.Fatal("unchanged source facts could not recover", err)
	}
}

// Partial and completed target journals remain immutable while source custody
// is absent. Restoring the exact retained attribute permits the same plan only.
func TestPreparationRestoreLostSourceKeepsOriginalTargetProgress(t *testing.T) {
	for _, stop := range []string{"partial", "complete"} {
		f := newPreparationRestoreFixture(t)
		preparationRestoreAccept(t, f)
		lost := errors.New("synthetic stop after original target reservation")
		var hooks *preparationHooks
		if stop == "partial" {
			hooks = &preparationHooks{after: func(stage, path string) error {
				if stage == "control-header" {
					return lost
				}
				return nil
			}}
		}
		_, err := applyPreparation(t.Context(), f.accepted, preparationRestoreTestAdapter(), f.volume.host, daemonScope, hooks)
		if stop == "partial" && !errors.Is(err, lost) || stop == "complete" && err != nil {
			t.Fatal("did not reach target progress", stop, err)
		}
		before, err := os.ReadFile(f.request.ControlPath)
		if err != nil {
			t.Fatal(err)
		}
		file, err := os.Open(f.request.RestoreSource.Directory)
		if err != nil {
			t.Fatal(err)
		}
		attribute := "user.urnetwork.attempt-ledger-custody"
		raw, readErr := readInventoryAttribute(file, attribute, 4096)
		if err := errors.Join(readErr, file.Close()); err != nil {
			t.Fatal(err)
		}
		if err := unix.Removexattr(f.request.RestoreSource.Directory, attribute); err != nil {
			t.Fatal(err)
		}
		if _, err := ApplyPreparationWithHost(t.Context(), f.accepted, preparationRestoreTestAdapter(), f.volume.host); !errors.Is(err, ErrIdentity) {
			t.Fatal("lost copied original resumed target", stop, err)
		}
		after, err := os.ReadFile(f.request.ControlPath)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("source refusal reset retained target progress", stop, err)
		}
		if err := unix.Setxattr(f.request.RestoreSource.Directory, attribute, raw, unix.XATTR_CREATE); err != nil {
			t.Fatal(err)
		}
		if _, err := ApplyPreparationWithHost(t.Context(), f.accepted, preparationRestoreTestAdapter(), f.volume.host); err != nil {
			t.Fatal("exact retained original could not resume its own plan", stop, err)
		}
		after, err = os.ReadFile(f.request.ControlPath)
		if err != nil || !bytes.HasPrefix(after, before) || stop == "complete" && !bytes.Equal(after, before) {
			t.Fatal("same-plan recovery rewrote completed target journal", stop, err)
		}
	}
}

// A caller canceled after actual source bytes arrive gets no reservation or
// usable declaration, and the original source remains byte-exact.
func TestPreparationRestoreArchiveAdmissionCancellationPrecedesEffects(t *testing.T) {
	f := newPreparationRestoreFixture(t)
	preparationRestoreAccept(t, f)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reads := 0
	_, err := applyPreparation(ctx, f.accepted, preparationRestoreTestAdapter(), f.volume.host, daemonScope, &preparationHooks{sourceRead: func(string, int) {
		reads++
		cancel()
	}})
	if reads != 1 || !errors.Is(err, context.Canceled) {
		t.Fatal("source admission lost caller cancellation", reads, err)
	}
	if _, err := os.Stat(f.request.ControlPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("canceled archive admission reserved target", err)
	}
	if _, err := ApplyPreparationWithHost(t.Context(), f.accepted, preparationRestoreTestAdapter(), f.volume.host); err != nil {
		t.Fatal("cancellation changed original source authority", err)
	}
}
