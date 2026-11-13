//go:build linux || darwin

// Unavailable metadata refuses admission without inventing a changed identity.
package durablevolume

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Both root and retained-child paths must recover on the same owner generation.
func TestOwnerNamedObservationUnavailableRetainsGeneration(t *testing.T) {
	for _, relative := range []string{"", "retained"} {
		for _, operation := range []string{"opened-stat", "named-open", "named-stat", "named-close"} {
			for _, cause := range []error{syscall.EIO, syscall.EMFILE} {
				fixture := newVolumeFixture(t)
				owner := fixture.open(t, ReadWrite)
				directory, err := owner.OpenDirectory(relative, true)
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(fixture.root, relative)
				owner.observeFile = func(step string, _ *os.File, observed string) error {
					if step == operation && observed == path {
						return cause
					}
					return nil
				}
				err = owner.CheckDirectory(relative, directory)
				if !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrIdentity) || !errors.Is(err, cause) {
					t.Errorf("%q/%s/%v inferred identity from unavailable observation: %v", relative, operation, cause, err)
				}
				owner.observeFile = nil
				if err := owner.CheckDirectory(relative, directory); err != nil {
					t.Errorf("%q/%s/%v did not recover original owner: %v", relative, operation, cause, err)
				}
				if err := directory.Close(); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}

// A caller-owned closed descriptor is invalid input, not observed volume loss.
func TestOwnerClosedBorrowedDescriptorDoesNotPoison(t *testing.T) {
	fixture := newVolumeFixture(t)
	owner := fixture.open(t, ReadWrite)
	directory, err := owner.OpenDirectory("retained", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := directory.Close(); err != nil {
		t.Fatal(err)
	}
	err = owner.CheckDirectory("retained", directory)
	if err == nil || errors.Is(err, ErrIdentity) || errors.Is(err, ErrUnavailable) {
		t.Fatal("caller descriptor error was misclassified", err)
	}
	if err := owner.Check(); err != nil {
		t.Fatal("caller error poisoned volume", err)
	}
}

// A failed final pathname observation must not publish a plausible partial report.
func TestInventoryNamedObservationUnavailableCanRetry(t *testing.T) {
	fixture := newVolumeFixture(t)
	fixture.custody(t)
	owner := fixture.open(t, Snapshot)
	fence := fixture.fence(t)
	limits := InventoryLimits{MaxEntries: 16, MaxBytes: 4096, MaxDepth: 4, MaxOwnerAttributes: 16, MaxOwnerAttributeBytes: 16384}
	owner.observeFile = func(step string, _ *os.File, path string) error {
		if step == "named-stat" && path == filepath.Join(fixture.root, "journal", "pending") {
			return syscall.EIO
		}
		return nil
	}
	report, err := owner.Inventory(t.Context(), fence, limits)
	if !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrIdentity) || !errors.Is(err, syscall.EIO) || len(report.Entries) != 0 {
		t.Fatal("inventory inferred identity or published incomplete observation", report, err)
	}
	owner.observeFile = nil
	if _, err := owner.Inventory(t.Context(), fence, limits); err != nil {
		t.Fatal("same snapshot generation did not recover", err)
	}
}
