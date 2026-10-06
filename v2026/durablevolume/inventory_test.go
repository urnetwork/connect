//go:build linux

// Backup controls use real retained files and a private child writer; no live
// mount, database, deployment key or service participates in these tests.
package durablevolume

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// The retained assertion is outside the journal and binds its precise lease.
func (self *volumeFixture) fence(t *testing.T) Reference {
	t.Helper()
	fence := FormerWriterFence{Schema: FormerWriterFenceSchema, RootPath: self.root, DeclarationSha256: self.reference.Sha256,
		LeaseSha256: self.config.Volumes[0].StateRoots[0].LeaseSha256, FormerWritersStopped: true, Evidence: "synthetic fixture writer was joined; no legacy writers are running"}
	raw, err := json.Marshal(fence)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(self.mount), "former-writer-fence.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return Reference{Path: path, Sha256: testDigest(raw)}
}

// Bounded fixtures keep both pending and completed bytes in the manifest.
func (self *volumeFixture) custody(t *testing.T) {
	t.Helper()
	if err := os.Mkdir(filepath.Join(self.root, "journal"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string][]byte{"pending": []byte("synthetic-original-signed-bytes"), "completed": []byte("synthetic-completed-custody"), "empty-lock": {}} {
		if err := os.WriteFile(filepath.Join(self.root, "journal", name), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

// An exact manifest can verify a byte restore without rewriting any journal.
func TestInventoryAndRestoreRetainExactLocalCustody(t *testing.T) {
	fixture := newVolumeFixture(t)
	fixture.custody(t)
	fence := fixture.fence(t)
	snapshot := fixture.open(t, Snapshot)
	limits := InventoryLimits{MaxEntries: 16, MaxBytes: 4096, MaxDepth: 4, MaxOwnerAttributes: 16, MaxOwnerAttributeBytes: 16384}
	inventory, err := snapshot.Inventory(t.Context(), fence, limits)
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.Entries) != 5 || inventory.RestartAuthorized {
		t.Fatalf("inventory scope: %+v", inventory)
	}
	raw, err := json.Marshal(inventory)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(fixture.mount), "inventory.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	reference := Reference{Path: path, Sha256: testDigest(raw)}
	verification, err := snapshot.VerifyInventory(t.Context(), reference, fence, limits)
	if err != nil || !verification.ExactLocalBytesAndMetadata || !verification.SamePhysicalRoot || verification.RestartAuthorized {
		t.Fatalf("verification: %+v %v", verification, err)
	}
	pending := filepath.Join(fixture.root, "journal", "pending")
	original, err := os.ReadFile(pending)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pending, []byte("damaged"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshot.VerifyInventory(t.Context(), reference, fence, limits); err == nil {
		t.Fatal("corrupt restore passed")
	}
	if err := os.WriteFile(pending, original, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshot.VerifyInventory(t.Context(), reference, fence, limits); err != nil {
		t.Fatalf("exact byte restoration: %v", err)
	}
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	fixture.open(t, ReadWrite)
	after, err := os.ReadFile(pending)
	if err != nil || !bytes.Equal(original, after) {
		t.Fatalf("restore altered original bytes: %v", err)
	}
}

// A lease by itself cannot claim quiescence of older unguarded writers.
func TestInventoryRequiresExactFormerWriterFence(t *testing.T) {
	fixture := newVolumeFixture(t)
	snapshot := fixture.open(t, Snapshot)
	limits := InventoryLimits{MaxEntries: 8, MaxBytes: 4096, MaxDepth: 4, MaxOwnerAttributes: 16, MaxOwnerAttributeBytes: 16384}
	if _, err := snapshot.Inventory(t.Context(), Reference{}, limits); err == nil {
		t.Fatal("missing legacy fence admitted")
	}
	fence := fixture.fence(t)
	raw, err := os.ReadFile(fence.Path)
	if err != nil {
		t.Fatal(err)
	}
	var assertion FormerWriterFence
	if err := json.Unmarshal(raw, &assertion); err != nil {
		t.Fatal(err)
	}
	assertion.FormerWritersStopped = false
	raw, err = json.Marshal(assertion)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fence.Path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	fence.Sha256 = testDigest(raw)
	if _, err := snapshot.Inventory(t.Context(), fence, limits); err == nil {
		t.Fatal("unjoined legacy writer fence admitted")
	}
}

// Bounds and cancellation return no partial success and leave custody intact.
func TestInventoryWorkAndByteBoundsAndCancellation(t *testing.T) {
	for _, kind := range []string{"entries", "bytes", "depth", "canceled"} {
		fixture := newVolumeFixture(t)
		fixture.custody(t)
		snapshot := fixture.open(t, Snapshot)
		limits := InventoryLimits{MaxEntries: 16, MaxBytes: 4096, MaxDepth: 4, MaxOwnerAttributes: 16, MaxOwnerAttributeBytes: 16384}
		ctx, cancel := context.WithCancel(t.Context())
		switch kind {
		case "entries":
			limits.MaxEntries = 2
		case "bytes":
			limits.MaxBytes = 1
		case "depth":
			limits.MaxDepth = 1
		case "canceled":
			cancel()
		}
		result, err := snapshot.Inventory(ctx, fixture.fence(t), limits)
		cancel()
		if err == nil || len(result.Entries) != 0 {
			t.Fatalf("%s returned partial success: %+v %v", kind, result, err)
		}
		if err := snapshot.Check(); err != nil {
			t.Fatalf("%s caller limit poisoned identity: %v", kind, err)
		}
	}
}

// A physical restore can match local bytes while still needing signed-root
// compatibility checks. The verification never grants restart authority.
func TestInventoryRestoredRootReportsChangedPhysicalIdentity(t *testing.T) {
	fixture := newVolumeFixture(t)
	fixture.custody(t)
	ownerName := "user.urnetwork.native-journal-custody"
	ownerBytes := []byte("opaque-original-inode-bound-acknowledgement")
	if err := syscall.Setxattr(fixture.root, ownerName, ownerBytes, 1); err != nil {
		t.Fatal(err)
	}
	fence := fixture.fence(t)
	snapshot := fixture.open(t, Snapshot)
	limits := InventoryLimits{MaxEntries: 16, MaxBytes: 4096, MaxDepth: 4, MaxOwnerAttributes: 16, MaxOwnerAttributeBytes: 16384}
	before, err := snapshot.Inventory(t.Context(), fence, limits)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(fixture.mount), "inventory.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(fixture.root, fixture.root+"-retained"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(fixture.root, 0700); err != nil {
		t.Fatal(err)
	}
	fixture.custody(t)
	if reopened, err := OpenWithHost(fixture.reference, fixture.root, Snapshot, fixture.host); !errors.Is(err, ErrIdentity) {
		if reopened != nil {
			reopened.Close()
		}
		t.Fatal("copied custody silently rebound old declaration", err)
	}
	priorRoot := fixture.config.Volumes[0].StateRoots[0]
	fixture.config.Volumes[0].StateRoots[0] = provisionTestRoot(t, fixture.root, priorRoot.LeasePath, priorRoot.LeaseSha256)
	fixture.writeConfig(t)
	fence = fixture.fence(t)
	restored := fixture.open(t, Snapshot)
	if _, err := restored.VerifyReboundInventory(t.Context(), Reference{Path: path, Sha256: testDigest(raw)}, fence, limits); err == nil {
		t.Fatal("explicit root rebind omitted original owner custody attributes")
	}
	if err := syscall.Setxattr(fixture.root, ownerName, ownerBytes, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := restored.VerifyInventory(t.Context(), Reference{Path: path, Sha256: testDigest(raw)}, fence, limits); err == nil {
		t.Fatal("ordinary verification inferred rebind authority")
	}
	result, err := restored.VerifyReboundInventory(t.Context(), Reference{Path: path, Sha256: testDigest(raw)}, fence, limits)
	if err != nil || !result.ExactLocalBytesAndMetadata || result.SamePhysicalRoot || result.SameDeclaration || result.SameRootGeneration || result.RestartAuthorized || result.ExpectedRootGeneration != before.RootGeneration {
		t.Fatalf("restored identity: %+v %v", result, err)
	}
}

// A same-device bind mount is refused just like a foreign device or symlink.
func TestInventoryRejectsAliasesAndNestedMounts(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "fifo", "nested-mount"} {
		fixture := newVolumeFixture(t)
		fixture.custody(t)
		snapshot := fixture.open(t, Snapshot)
		switch kind {
		case "symlink":
			if err := os.Symlink("journal/pending", filepath.Join(fixture.root, "alias")); err != nil {
				t.Fatal(err)
			}
		case "hardlink":
			if err := os.Link(filepath.Join(fixture.root, "journal", "pending"), filepath.Join(fixture.root, "alias")); err != nil {
				t.Fatal(err)
			}
		case "fifo":
			if err := syscall.Mkfifo(filepath.Join(fixture.root, "pipe"), 0600); err != nil {
				t.Fatal(err)
			}
		case "nested-mount":
			fixture.host.change(func() {
				fixture.host.mounts = append(fixture.host.mounts, Mount{Id: 9, ParentId: 7, Device: fixture.host.uuidDevice, Root: "/", Path: filepath.Join(fixture.root, "journal"), FilesystemType: "ext4"})
			})
		}
		if _, err := snapshot.Inventory(t.Context(), fixture.fence(t), InventoryLimits{MaxEntries: 16, MaxBytes: 4096, MaxDepth: 4, MaxOwnerAttributes: 16, MaxOwnerAttributeBytes: 16384}); !errors.Is(err, ErrIdentity) {
			t.Fatalf("%s integrity result: %v", kind, err)
		}
	}
}

// Process death releases only the dead writer's lease; retained synced bytes
// remain available to a new owner after the exact process has been joined.
func TestOwnerCrashReleasesLeaseAndRetainsCompletedBytes(t *testing.T) {
	if payload := os.Getenv("URNETWORK_SYNTHETIC_VOLUME_WRITER"); payload != "" {
		var input struct {
			Reference Reference
			Root      string
			Mount     string
		}
		if err := json.Unmarshal([]byte(payload), &input); err != nil {
			t.Fatal(err)
		}
		var stat syscall.Stat_t
		if err := syscall.Stat(input.Root, &stat); err != nil {
			t.Fatal(err)
		}
		device := deviceNumber(stat.Dev)
		host := &fixtureHost{mounts: []Mount{{Id: 1, ParentId: 1, Device: Device{Major: device.Major ^ 1, Minor: device.Minor}, Root: "/", Path: "/", FilesystemType: "ext4"}, {Id: 7, ParentId: 1, Device: device, Root: "/", Path: input.Mount, FilesystemType: "ext4"}}, uuidDevice: device, filesystem: Filesystem{Id: [2]int32{17, 19}, Type: 0xef53, AvailableBytes: 1024 * 1024, AvailableInodes: 1024}}
		owner, err := OpenWithHost(input.Reference, input.Root, ReadWrite, host)
		if err != nil {
			t.Fatal(err)
		}
		defer owner.Close()
		file, err := os.OpenFile(filepath.Join(input.Root, "completed"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte("synthetic-synced-completed-custody")); err != nil {
			t.Fatal(err)
		}
		if err := errors.Join(file.Sync(), file.Close(), owner.rootFile.Sync(), owner.CheckWrite()); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintln(os.Stdout, "writer-synced")
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}
	fixture := newVolumeFixture(t)
	payload, err := json.Marshal(struct {
		Reference Reference
		Root      string
		Mount     string
	}{Reference: fixture.reference, Root: fixture.root, Mount: fixture.mount})
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestOwnerCrashReleasesLeaseAndRetainsCompletedBytes$")
	command.Env = append(os.Environ(), "URNETWORK_SYNTHETIC_VOLUME_WRITER="+string(payload))
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	var diagnostics bytes.Buffer
	command.Stderr = &diagnostics
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	joined := false
	defer func() {
		if !joined {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "writer-synced\n" {
		t.Fatalf("child boundary: %q %v %s", line, err, diagnostics.String())
	}
	if owner, err := OpenWithHost(fixture.reference, fixture.root, Snapshot, fixture.host); !errors.Is(err, ErrBusy) {
		if owner != nil {
			owner.Close()
		}
		t.Fatalf("active child lease: %v", err)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("child did not die at the forced crash")
	}
	joined = true
	snapshot := fixture.open(t, Snapshot)
	result, err := snapshot.Inventory(t.Context(), fixture.fence(t), InventoryLimits{MaxEntries: 4, MaxBytes: 4096, MaxDepth: 2, MaxOwnerAttributes: 16, MaxOwnerAttributeBytes: 16384})
	if err != nil || len(result.Entries) != 2 {
		t.Fatalf("crash inventory: %+v %v", result, err)
	}
	raw, err := os.ReadFile(filepath.Join(fixture.root, "completed"))
	if err != nil || string(raw) != "synthetic-synced-completed-custody" {
		t.Fatalf("crash custody: %q %v", raw, err)
	}
}
