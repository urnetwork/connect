//go:build linux || darwin

// Read-only evidence admission keeps identity without requiring write capacity.
// Cancellation is checked before opening or hashing externally supplied files.
package durablevolume

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// Cancel at a precise protected-read chunk boundary without sleep or polling.
type readChunkCancelContext struct {
	context.Context
	cancel context.CancelFunc
	checks atomic.Uint64
}

// The first chunk completes before the next admission closes the actual context.
func (self *readChunkCancelContext) Err() error {
	if self.checks.Add(1) == 3 {
		self.cancel()
	}
	return self.Context.Err()
}

// No partial protected evidence survives cancellation during a bounded read.
func TestProtectedEvidenceReadStopsBetweenFiniteChunks(t *testing.T) {
	fixture := newVolumeFixture(t)
	path := filepath.Join(fixture.mount, "large-evidence")
	if err := os.WriteFile(path, make([]byte, 512*1024), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := openProtectedFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	parent, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx := &readChunkCancelContext{Context: parent, cancel: cancel}
	raw, err := boundedProtectedReadContext(ctx, file, 1024*1024)
	if !errors.Is(err, context.Canceled) || raw != nil || ctx.checks.Load() != 3 {
		t.Fatalf("chunk cancellation retained partial bytes=%d checks=%d err=%v", len(raw), ctx.checks.Load(), err)
	}
}

// Existing writer ownership must still allow inspection during resource pressure.
func TestOwnerReadAdmissionPreservesIdentityWithoutWriteReserve(t *testing.T) {
	fixture := newVolumeFixture(t)
	owner := fixture.open(t, ReadWrite)
	directory, err := owner.OpenDirectory("retained", true)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	fixture.host.change(func() {
		fixture.host.filesystem.AvailableBytes = 0
		fixture.host.filesystem.AvailableInodes = 0
		fixture.host.filesystem.ReadOnly = true
		fixture.host.mounts[1].ReadOnly = true
	})
	if err := errors.Join(owner.CheckRead(), owner.CheckReadDirectory("retained", directory)); err != nil {
		t.Fatal("capacity blocked identity-only inspection", err)
	}
	if err := owner.CheckWrite(); !errors.Is(err, ErrUnavailable) {
		t.Fatal("read admission authorized writes", err)
	}
	fixture.host.change(func() { fixture.host.filesystem.Id[0]++ })
	if err := owner.CheckReadDirectory("retained", directory); !errors.Is(err, ErrIdentity) {
		t.Fatal("read admission ignored proven identity loss", err)
	}
	fixture.host.change(func() { fixture.host.filesystem.Id[0]-- })
	if err := owner.CheckRead(); !errors.Is(err, ErrIdentity) {
		t.Fatal("read admission resurrected invalidated generation", err)
	}
}

// A missing expected path is deliberately observable only after context admission.
func TestVerifyInventoryCancellationPrecedesExpectedEvidenceRead(t *testing.T) {
	fixture := newVolumeFixture(t)
	owner := fixture.open(t, Snapshot)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	expected := Reference{Path: filepath.Join(fixture.mount, "absent-inventory.json"), Sha256: "sha256:" + strings.Repeat("0", 64)}
	_, err := owner.VerifyInventory(ctx, expected, Reference{}, InventoryLimits{})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("canceled verification opened expected evidence first", err)
	}
}

// Nil admission has the same pre-I/O refusal contract as Inventory itself.
func TestVerifyInventoryNilContextPrecedesExpectedEvidenceRead(t *testing.T) {
	fixture := newVolumeFixture(t)
	owner := fixture.open(t, Snapshot)
	_, err := owner.VerifyInventory(nil, Reference{}, Reference{}, InventoryLimits{})
	if err == nil || err.Error() != "durable inventory context is required" {
		t.Fatal("nil verification reached evidence work", err)
	}
}
