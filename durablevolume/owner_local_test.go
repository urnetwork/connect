//go:build linux

// Owner-local custody uses real protected descriptors on the test data volume.
// Only kernel mount facts model a laptop whose state shares its system device.
package durablevolume

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The explicit scope changes, while the same physical roots and leases remain.
func newOwnerLocalFixture(t *testing.T) *volumeFixture {
	t.Helper()
	fixture := newVolumeFixture(t)
	fixture.config.Schema = OwnerLocalSchema
	fixture.host.change(func() {
		fixture.host.mounts[0].Device = fixture.host.uuidDevice
	})
	fixture.writeConfig(t)
	return fixture
}

// The test adapter never substitutes the actual files, ancestry or lease.
func openOwnerLocalFixture(t *testing.T, fixture *volumeFixture, access Access) *Owner {
	t.Helper()
	owner, err := OpenOwnerLocalWithHost(fixture.reference, fixture.root, access, fixture.host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := owner.Close(); err != nil {
			t.Error(err)
		}
	})
	return owner
}

// A separate filesystem is not an owner-device prerequisite. The explicit
// owner-local policy still pins a protected precreated state root and lease.
func TestOwnerLocalAdmitsDeclaredSystemDevice(t *testing.T) {
	fixture := newOwnerLocalFixture(t)
	owner := openOwnerLocalFixture(t, fixture, ReadWrite)
	directory, err := owner.OpenDirectory("original-journal", true)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	if err := owner.CheckDirectory("original-journal", directory); err != nil {
		t.Fatal(err)
	}
	if err := owner.CheckWrite(); err != nil {
		t.Fatal(err)
	}
}

// A policy cannot choose the API's scope. Daemon and owner-local declarations
// reject each other even when the selected mount is a secondary device.
func TestOwnerLocalPolicyCannotAuthorizeDaemon(t *testing.T) {
	fixture := newOwnerLocalFixture(t)
	if _, err := Load(fixture.reference); err == nil {
		t.Fatal("daemon loader accepted owner-local policy")
	}
	if owner, err := OpenWithHost(fixture.reference, fixture.root, ReadWrite, fixture.host); err == nil {
		owner.Close()
		t.Fatal("daemon admitted owner-local system device")
	}
	fixture.host.change(func() {
		fixture.host.mounts[0].Device.Major ^= 1
	})
	if owner, err := OpenWithHost(fixture.reference, fixture.root, ReadWrite, fixture.host); err == nil {
		owner.Close()
		t.Fatal("secondary device made owner-local policy a daemon declaration")
	}
	fixture.config.Schema = Schema
	fixture.writeConfig(t)
	if _, err := LoadOwnerLocal(fixture.reference); err == nil {
		t.Fatal("owner-local loader silently reinterpreted daemon policy")
	}
	if owner, err := OpenOwnerLocalWithHost(fixture.reference, fixture.root, ReadWrite, fixture.host); err == nil {
		owner.Close()
		t.Fatal("signing scope silently reinterpreted daemon policy")
	}
}

// A literal root mount is admitted only in the owner-local parser and kernel
// census. Physical-device checks are exercised above without writing root disk.
func TestOwnerLocalRootMountKeepsPathAndMountBounds(t *testing.T) {
	fixture := newOwnerLocalFixture(t)
	fixture.config.Volumes[0].MountPath = "/"
	fixture.host.change(func() { fixture.host.mounts = fixture.host.mounts[:1] })
	fixture.writeConfig(t)
	config, err := LoadOwnerLocal(fixture.reference)
	if err != nil {
		t.Fatal(err)
	}
	owner := &Owner{scope: ownerLocalScope, spec: config.Volumes[0], rootSpec: config.Volumes[0].StateRoots[0], rootPath: fixture.root, host: fixture.host}
	if mount, err := owner.mountFacts(); err != nil || mount.Path != "/" {
		t.Fatalf("declared system mount was refused: %+v %v", mount, err)
	}
	owner.scope = daemonScope
	if _, err := owner.mountFacts(); err == nil {
		t.Fatal("daemon kernel admission accepted the system filesystem")
	}
	owner.scope = ownerLocalScope
	fixture.host.change(func() { fixture.host.uuidDevice.Major ^= 1 })
	if _, err := owner.mountFacts(); err == nil {
		t.Fatal("root mount waived its independently declared filesystem uuid")
	}
	fixture.host.change(func() { fixture.host.uuidDevice.Major ^= 1 })
	fixture.host.change(func() {
		fixture.host.mounts = append(fixture.host.mounts, Mount{Id: 9, ParentId: 1, Device: fixture.host.uuidDevice, Root: "/", Path: fixture.root, FilesystemType: "ext4"})
	})
	if _, err := owner.mountFacts(); err == nil {
		t.Fatal("nested same-device mount concealed the selected state root")
	}
	fixture.config.Volumes[0].StateRoots[0].Path = "/"
	fixture.writeConfig(t)
	if _, err := LoadOwnerLocal(fixture.reference); err == nil {
		t.Fatal("filesystem root became an application journal root")
	}
	fixture.config.Volumes[0].StateRoots[0].Path = fixture.root
	fixture.config.Volumes[0].StateRoots = append(fixture.config.Volumes[0].StateRoots, StateRootSpec{
		Path: filepath.Join(fixture.root, "overlap"), LeasePath: filepath.Join(fixture.mount, "second-lease"), LeaseSha256: testDigest([]byte("synthetic-second-lease")),
	})
	fixture.writeConfig(t)
	if _, err := LoadOwnerLocal(fixture.reference); err == nil {
		t.Fatal("root mount waived overlapping journal owners")
	}
	for _, path := range []string{"relative/state", "../state", ""} {
		if beneath("/", path) {
			t.Fatal("root containment admitted a relative path", path)
		}
	}
}

// Pressure and failed kernel observations retain the same original owner;
// neither may turn a retry into an implicit fresh journal generation.
func TestOwnerLocalPressureAndObservationCanRetrySameOwner(t *testing.T) {
	fixture := newOwnerLocalFixture(t)
	owner := openOwnerLocalFixture(t, fixture, ReadWrite)
	original := fixture.host.filesystem
	for _, kind := range []string{"bytes", "inodes", "read-only", "observation"} {
		fixture.host.change(func() {
			switch kind {
			case "bytes":
				fixture.host.filesystem.AvailableBytes = 0
			case "inodes":
				fixture.host.filesystem.AvailableInodes = 0
			case "read-only":
				fixture.host.filesystem.ReadOnly = true
			case "observation":
				fixture.host.filesystemErr = errors.New("synthetic kernel observation failure")
			}
		})
		if err := owner.CheckWrite(); !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrIdentity) {
			t.Fatalf("%s pressure was not a recoverable refusal: %v", kind, err)
		}
		if directory, err := owner.OpenDirectory("must-not-appear", true); !errors.Is(err, ErrUnavailable) || directory != nil {
			t.Fatalf("%s admitted new state: %v", kind, err)
		}
		if _, err := os.Stat(filepath.Join(fixture.root, "must-not-appear")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s mutated original custody: %v", kind, err)
		}
		fixture.host.change(func() {
			fixture.host.filesystem = original
			fixture.host.filesystemErr = nil
		})
		if err := owner.CheckWrite(); err != nil {
			t.Fatalf("%s could not retry the original owner: %v", kind, err)
		}
	}
}

// Returning the original inode does not revive an owner that observed lost
// custody. A fresh explicit owner can reopen those exact retained bytes.
func TestOwnerLocalReplacementRequiresOriginalCustodyReopen(t *testing.T) {
	fixture := newOwnerLocalFixture(t)
	fixture.custody(t)
	owner := openOwnerLocalFixture(t, fixture, ReadWrite)
	originalPath := fixture.root + "-retained"
	if err := os.Rename(fixture.root, originalPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(fixture.root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := owner.CheckWrite(); !errors.Is(err, ErrIdentity) {
		t.Fatalf("replacement did not invalidate original owner: %v", err)
	}
	if err := os.Remove(fixture.root); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(originalPath, fixture.root); err != nil {
		t.Fatal(err)
	}
	if err := owner.CheckWrite(); !errors.Is(err, ErrIdentity) {
		t.Fatalf("old inode revived a poisoned owner: %v", err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	openOwnerLocalFixture(t, fixture, ReadWrite)
	raw, err := os.ReadFile(filepath.Join(fixture.root, "journal", "pending"))
	if err != nil || !bytes.Equal(raw, []byte("synthetic-original-signed-bytes")) {
		t.Fatalf("reopen altered retained intent: %q %v", raw, err)
	}
}

// Reusing a filesystem uuid cannot conceal a remount within an open owner.
func TestOwnerLocalRemountKeepsStickyGeneration(t *testing.T) {
	fixture := newOwnerLocalFixture(t)
	owner := openOwnerLocalFixture(t, fixture, ReadWrite)
	original := fixture.host.mounts[1]
	fixture.host.change(func() { fixture.host.mounts[1].Id++ })
	if err := owner.CheckWrite(); !errors.Is(err, ErrIdentity) {
		t.Fatalf("remount kept owner admission: %v", err)
	}
	fixture.host.change(func() { fixture.host.mounts[1] = original })
	if err := owner.CheckWrite(); !errors.Is(err, ErrIdentity) {
		t.Fatalf("remount restoration revived owner: %v", err)
	}
}

// Independent stopped-writer fencing and exact inventory remain necessary for
// owner-local backups; a verification never authorizes another device signing.
func TestOwnerLocalSnapshotPreservesPendingAndCompletedIntent(t *testing.T) {
	fixture := newOwnerLocalFixture(t)
	fixture.custody(t)
	writer := openOwnerLocalFixture(t, fixture, ReadWrite)
	if owner, err := OpenOwnerLocalWithHost(fixture.reference, fixture.root, Snapshot, fixture.host); !errors.Is(err, ErrBusy) || owner != nil {
		t.Fatalf("snapshot admitted an active signer: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	snapshot := openOwnerLocalFixture(t, fixture, Snapshot)
	limits := InventoryLimits{MaxEntries: 16, MaxBytes: 4096, MaxDepth: 4, MaxOwnerAttributes: 16, MaxOwnerAttributeBytes: 16384}
	if _, err := snapshot.Inventory(t.Context(), Reference{}, limits); err == nil {
		t.Fatal("owner-local snapshot waived the external former-writer fence")
	}
	fence := fixture.fence(t)
	inventory, err := snapshot.Inventory(t.Context(), fence, limits)
	if err != nil || len(inventory.Entries) != 5 || inventory.RestartAuthorized {
		t.Fatalf("owner-local retained inventory: %+v %v", inventory, err)
	}
	raw, err := json.Marshal(inventory)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(fixture.mount), "owner-inventory.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	verification, err := snapshot.VerifyInventory(t.Context(), Reference{Path: path, Sha256: testDigest(raw)}, fence, limits)
	if err != nil || !verification.ExactLocalBytesAndMetadata || !verification.SamePhysicalRoot || verification.RestartAuthorized {
		t.Fatalf("owner-local inventory invented signing authority: %+v %v", verification, err)
	}
}
