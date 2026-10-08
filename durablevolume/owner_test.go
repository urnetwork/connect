//go:build linux || darwin

// Kernel facts are synthetic; protected paths, bytes, descriptors and leases
// are real. Explicit barriers expose ordering without sleeps or live mounts.
package durablevolume

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

// A private host instance never changes facts outside its own test owner.
type fixtureHost struct {
	stateLock       sync.Mutex
	mounts          []Mount
	uuidDevice      Device
	filesystem      Filesystem
	filesystemHook  func(*os.File)
	writeHealthHook func(string, *os.File) error
	mountsErr       error
	uuidErr         error
	filesystemErr   error
}

// Returns a copy so readers cannot mutate or race the next census.
func (self *fixtureHost) Mounts() ([]Mount, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]Mount(nil), self.mounts...), self.mountsErr
}

// The test controls only the kernel's uuid lookup result.
func (self *fixtureHost) DeviceUuid(string) (Device, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.uuidDevice, self.uuidErr
}

// Barriers run outside the fixture mutex, just as production host calls do.
func (self *fixtureHost) Filesystem(file *os.File) (Filesystem, error) {
	facts, hook, err := func() (Filesystem, func(*os.File), error) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.filesystem, self.filesystemHook, self.filesystemErr
	}()
	if hook != nil {
		hook(file)
	}
	return facts, err
}

// Refusal hooks cannot replace the successful write-health kernel operations.
func (self *fixtureHost) observeWriteHealth(operation string, file *os.File) error {
	hook := func() func(string, *os.File) error {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.writeHealthHook
	}()
	if hook != nil {
		return hook(operation, file)
	}
	return nil
}

// Changes facts at a chosen causal boundary without process-global hooks.
func (self *fixtureHost) change(update func()) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	update()
}

// Every fixture contains an already provisioned marker and owner root.
type volumeFixture struct {
	reference Reference
	config    Config
	root      string
	mount     string
	marker    string
	host      *fixtureHost
}

// Fixture digests use the same canonical wire representation as deployment.
func testDigest(raw []byte) string {
	digest := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(digest[:])
}

// No mount syscall is used; the host adapter must still match actual stat.dev.
func newVolumeFixture(t *testing.T) *volumeFixture {
	t.Helper()
	base, err := os.MkdirTemp("", "durable-volume-test-")
	if err != nil {
		t.Fatal(err)
	}
	// Custody walks never follow symlinks; Darwin's temp directory is below
	// the /var alias of /private/var.
	if base, err = filepath.EvalSymlinks(base); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(base); err != nil {
			t.Error(err)
		}
	})
	mount := filepath.Join(base, "volume")
	root := filepath.Join(mount, "owner")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(mount, "identity")
	markerBytes := []byte("synthetic-volume-generation-one\n")
	if err := os.WriteFile(marker, markerBytes, 0600); err != nil {
		t.Fatal(err)
	}
	lease := filepath.Join(mount, "owner-lease")
	leaseBytes := []byte("synthetic-owner-lease-generation-one\n")
	if err := os.WriteFile(lease, leaseBytes, 0600); err != nil {
		t.Fatal(err)
	}
	var stat unix.Stat_t
	if err := unix.Stat(root, &stat); err != nil {
		t.Fatal(err)
	}
	device := statDevice(&stat)
	rootDevice := Device{Major: device.Major ^ 1, Minor: device.Minor}
	host := &fixtureHost{
		mounts: []Mount{
			{Id: 1, ParentId: 1, Device: rootDevice, Root: "/", Path: "/", FilesystemType: testFilesystemType},
			{Id: 7, ParentId: 1, Device: device, Root: "/", Path: mount, FilesystemType: testFilesystemType},
		},
		uuidDevice: device,
		filesystem: Filesystem{Id: [2]int32{17, 19}, Type: filesystemMagic(testFilesystemType), AvailableBytes: 1024 * 1024, AvailableInodes: 1024},
	}
	config := Config{Schema: Schema, Volumes: []VolumeSpec{{MountPath: mount, FilesystemUuid: "1234-abcd", FilesystemType: testFilesystemType, MarkerPath: marker,
		MarkerSha256: testDigest(markerBytes), StateRoots: []StateRootSpec{provisionTestRoot(t, root, lease, testDigest(leaseBytes))}, MinAvailableBytes: 1024, MinAvailableInodes: 8}}}
	self := &volumeFixture{reference: Reference{Path: filepath.Join(base, "volumes.json")}, config: config, root: root, mount: mount, marker: marker, host: host}
	self.writeConfig(t)
	return self
}

// Test-only provisioning explicitly enrolls physical roots before admission.
func provisionTestRoot(t *testing.T, path, lease, leaseSha256 string) StateRootSpec {
	t.Helper()
	raw := make([]byte, RootGenerationBytes)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(path, RootGenerationAttribute, raw, 0); err != nil {
		t.Fatal(err)
	}
	var stat unix.Stat_t
	if err := unix.Stat(path, &stat); err != nil {
		t.Fatal(err)
	}
	return StateRootSpec{Path: path, LeasePath: lease, LeaseSha256: leaseSha256, RootInode: stat.Ino, GenerationSha256: testDigest(raw)}
}

// Deliberate invalid declarations are also hashed, so validation is exercised.
func (self *volumeFixture) writeConfig(t *testing.T) {
	t.Helper()
	raw, err := json.Marshal(self.config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(self.reference.Path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	self.reference.Sha256 = testDigest(raw)
}

// Each returned guard has exactly one cleanup owner.
func (self *volumeFixture) open(t *testing.T, access Access) *Owner {
	t.Helper()
	owner, err := OpenWithHost(self.reference, self.root, access, self.host)
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

// Admission and nested creation use real anchored descriptors and syncs.
func TestOwnerAdmitsDeclaredRootAndDescendant(t *testing.T) {
	fixture := newVolumeFixture(t)
	owner := fixture.open(t, ReadWrite)
	directory, err := owner.OpenDirectory("journal/pending", true)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	if err := owner.CheckDirectory("journal/pending", directory); err != nil {
		t.Fatal(err)
	}
	if owner.RootPath() != fixture.root {
		t.Fatal("owner root changed")
	}
	if err := owner.CheckWrite(); err != nil {
		t.Fatal(err)
	}
}

// Missing mount facts cannot turn ordinary directories into new custody.
func TestOwnerRefusesAbsentMountBeforeCreatingState(t *testing.T) {
	fixture := newVolumeFixture(t)
	fixture.host.mounts = fixture.host.mounts[:1]
	if owner, err := OpenWithHost(fixture.reference, fixture.root, ReadWrite, fixture.host); err == nil {
		owner.Close()
		t.Fatal("absent mount admitted")
	}
	entries, err := os.ReadDir(fixture.root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("state changed: %v %v", entries, err)
	}
}

// An absent approved root is never silently re-enrolled on the same volume.
func TestOwnerRefusesMissingRootWithoutRecreation(t *testing.T) {
	fixture := newVolumeFixture(t)
	if err := os.Remove(fixture.root); err != nil {
		t.Fatal(err)
	}
	if owner, err := OpenWithHost(fixture.reference, fixture.root, ReadWrite, fixture.host); err == nil {
		owner.Close()
		t.Fatal("missing root admitted")
	}
	if _, err := os.Lstat(fixture.root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("root recreated: %v", err)
	}
}

// A uuid result must identify the exact currently mounted device.
func TestOwnerRefusesWrongUuidAndAmbiguousMounts(t *testing.T) {
	for _, kind := range []string{"uuid", "nested", "duplicate"} {
		fixture := newVolumeFixture(t)
		switch kind {
		case "uuid":
			fixture.host.uuidDevice.Minor++
		case "nested":
			fixture.host.mounts = append(fixture.host.mounts, Mount{Id: 8, ParentId: 7, Device: fixture.host.uuidDevice, Root: "/", Path: fixture.root, FilesystemType: testFilesystemType})
		case "duplicate":
			fixture.host.mounts = append(fixture.host.mounts, fixture.host.mounts[1])
		}
		if owner, err := OpenWithHost(fixture.reference, fixture.root, ReadWrite, fixture.host); err == nil {
			owner.Close()
			t.Fatalf("%s admitted", kind)
		}
	}
}

// A declared volume may share the root filesystem's device; its uuid, marker
// and lease still identify it.
func TestOwnerAdmitsDeclaredVolumeOnRootDevice(t *testing.T) {
	fixture := newVolumeFixture(t)
	fixture.host.mounts[0].Device = fixture.host.uuidDevice
	owner, err := OpenWithHost(fixture.reference, fixture.root, ReadWrite, fixture.host)
	if err != nil {
		t.Fatal("declared volume on the root device was refused:", err)
	}
	owner.Close()
}

// Each admission checks both allocation dimensions and each read-only source.
func TestOwnerRefusesReadOnlyAndExhaustedReserve(t *testing.T) {
	for _, kind := range []string{"mount-readonly", "statfs-readonly", "bytes", "inodes"} {
		fixture := newVolumeFixture(t)
		owner := fixture.open(t, ReadWrite)
		fixture.host.change(func() {
			switch kind {
			case "mount-readonly":
				fixture.host.mounts[1].ReadOnly = true
			case "statfs-readonly":
				fixture.host.filesystem.ReadOnly = true
			case "bytes":
				fixture.host.filesystem.AvailableBytes = fixture.config.Volumes[0].MinAvailableBytes - 1
			case "inodes":
				fixture.host.filesystem.AvailableInodes = fixture.config.Volumes[0].MinAvailableInodes - 1
			}
		})
		if directory, err := owner.OpenDirectory("must-not-exist", true); !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrIdentity) {
			if directory != nil {
				directory.Close()
			}
			t.Fatalf("%s availability classification: %v", kind, err)
		}
		if _, err := os.Lstat(filepath.Join(fixture.root, "must-not-exist")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s changed state: %v", kind, err)
		}
		fixture.host.change(func() {
			fixture.host.filesystem.AvailableBytes = 1024 * 1024
			fixture.host.filesystem.AvailableInodes = 1024
			fixture.host.filesystem.ReadOnly = false
			fixture.host.mounts[1].ReadOnly = false
		})
		if err := owner.CheckWrite(); err != nil {
			t.Fatalf("%s availability recovery: %v", kind, err)
		}
	}
}

// An unavailable kernel observation refuses this attempt and retains the cause,
// but identical later facts can admit work without discarding the owner.
func TestOwnerTransientKernelObservationRetainsGeneration(t *testing.T) {
	for _, kind := range []string{"mounts", "uuid", "filesystem"} {
		fixture := newVolumeFixture(t)
		owner := fixture.open(t, ReadWrite)
		cause := errors.New("synthetic kernel observation unavailable")
		fixture.host.change(func() {
			switch kind {
			case "mounts":
				fixture.host.mountsErr = cause
			case "uuid":
				fixture.host.uuidErr = cause
			case "filesystem":
				fixture.host.filesystemErr = cause
			}
		})
		if err := owner.Check(); !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrIdentity) || !errors.Is(err, cause) {
			t.Fatalf("%s transient observation classification: %v", kind, err)
		}
		fixture.host.change(func() { fixture.host.mountsErr = nil; fixture.host.uuidErr = nil; fixture.host.filesystemErr = nil })
		if err := owner.CheckWrite(); err != nil {
			t.Fatalf("%s observation recovery: %v", kind, err)
		}
	}
}

// A proven descendant loss invalidates that borrowed generation even if the
// original inode is restored under its original name before another check.
func TestOwnerDescendantLossRemainsPoisonedAfterRestoration(t *testing.T) {
	for _, kind := range []string{"missing", "replacement", "symlink", "permissions", "nested-mount"} {
		fixture := newVolumeFixture(t)
		owner := fixture.open(t, ReadWrite)
		directory, err := owner.OpenDirectory("journal", true)
		if err != nil {
			t.Fatal(err)
		}
		defer directory.Close()
		path := filepath.Join(fixture.root, "journal")
		switch kind {
		case "missing", "replacement", "symlink":
			if err := os.Rename(path, path+"-retained"); err != nil {
				t.Fatal(err)
			}
			if kind == "replacement" {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "symlink" {
				if err := os.Symlink(path+"-retained", path); err != nil {
					t.Fatal(err)
				}
			}
		case "permissions":
			if err := os.Chmod(path, 0777); err != nil {
				t.Fatal(err)
			}
		case "nested-mount":
			fixture.host.change(func() {
				fixture.host.mounts = append(fixture.host.mounts, Mount{Id: 9, ParentId: 7, Device: fixture.host.uuidDevice, Root: "/", Path: path, FilesystemType: testFilesystemType})
			})
		}
		if err := owner.CheckDirectory("journal", directory); !errors.Is(err, ErrIdentity) {
			t.Fatalf("%s custody loss classification: %v", kind, err)
		}
		switch kind {
		case "missing", "replacement", "symlink":
			if kind != "missing" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Rename(path+"-retained", path); err != nil {
				t.Fatal(err)
			}
		case "permissions":
			if err := os.Chmod(path, 0700); err != nil {
				t.Fatal(err)
			}
		case "nested-mount":
			fixture.host.change(func() { fixture.host.mounts = fixture.host.mounts[:2] })
		}
		if err := owner.CheckDirectory("journal", directory); !errors.Is(err, ErrIdentity) {
			t.Fatalf("%s restored generation was readmitted: %v", kind, err)
		}
	}
}

// Invalid caller arguments and an as-yet unclaimed absent descendant do not
// falsely report observed destruction of already admitted custody.
func TestOwnerCallerErrorsDoNotPoisonIdentity(t *testing.T) {
	fixture := newVolumeFixture(t)
	owner := fixture.open(t, ReadWrite)
	directory, err := owner.OpenDirectory("", false)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	if err := owner.CheckDirectory("../escape", directory); err == nil || errors.Is(err, ErrIdentity) {
		t.Fatalf("caller input classification: %v", err)
	}
	if file, err := owner.OpenDirectory("not-yet-created", false); !errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrIdentity) {
		if file != nil {
			file.Close()
		}
		t.Fatalf("unclaimed absence: %v", err)
	}
	if err := owner.CheckWrite(); err != nil {
		t.Fatalf("caller error poisoned owner: %v", err)
	}
}

// Read-only inspection remains possible at low reserve but cannot create.
func TestOwnerReadOnlyInspectionCannotMutate(t *testing.T) {
	fixture := newVolumeFixture(t)
	fixture.host.filesystem.AvailableBytes = 0
	fixture.host.filesystem.AvailableInodes = 0
	fixture.host.filesystem.ReadOnly = true
	fixture.host.mounts[1].ReadOnly = true
	owner := fixture.open(t, ReadOnly)
	directory, err := owner.OpenDirectory("", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := directory.Close(); err != nil {
		t.Fatal(err)
	}
	if err := owner.CheckWrite(); err == nil {
		t.Fatal("read-only owner admitted a write")
	}
}

// Namespace replacement cannot acknowledge new state even with identical data.
func TestOwnerRejectsReplacementRootAndMarker(t *testing.T) {
	for _, kind := range []string{"root", "marker", "marker-bytes", "mount-id", "filesystem-id"} {
		fixture := newVolumeFixture(t)
		owner := fixture.open(t, ReadWrite)
		switch kind {
		case "root":
			if err := os.Rename(fixture.root, fixture.root+"-retained"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(fixture.root, 0700); err != nil {
				t.Fatal(err)
			}
		case "marker":
			raw, err := os.ReadFile(fixture.marker)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(fixture.marker, fixture.marker+"-retained"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(fixture.marker, raw, 0600); err != nil {
				t.Fatal(err)
			}
		case "marker-bytes":
			if err := os.WriteFile(fixture.marker, []byte("replacement-generation\n"), 0600); err != nil {
				t.Fatal(err)
			}
		case "mount-id":
			fixture.host.change(func() { fixture.host.mounts[1].Id++ })
		case "filesystem-id":
			fixture.host.change(func() { fixture.host.filesystem.Id[0]++ })
		}
		if err := owner.Check(); err == nil {
			t.Fatalf("%s replacement admitted", kind)
		}
	}
}

// Only the affected owner closes; reopening the unchanged volume retains bytes.
func TestOwnerSameVolumeRecoveryPreservesCustody(t *testing.T) {
	fixture := newVolumeFixture(t)
	original := []byte("retained-synthetic-signed-pending-and-completed-bytes\n")
	path := filepath.Join(fixture.root, "journal")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	owner := fixture.open(t, ReadWrite)
	fixture.host.change(func() { fixture.host.filesystem.AvailableBytes = 0 })
	if err := owner.Check(); err == nil {
		t.Fatal("exhausted owner admitted")
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	fixture.host.change(func() { fixture.host.filesystem.AvailableBytes = 1024 * 1024 })
	reopened := fixture.open(t, ReadWrite)
	if err := reopened.Check(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, original) {
		t.Fatalf("retained custody changed: %q %v", raw, err)
	}
}

// Snapshot and writer admission return typed pending without blocking.
func TestOwnerSnapshotLeaseConflictsWithoutWaiting(t *testing.T) {
	fixture := newVolumeFixture(t)
	writer := fixture.open(t, ReadWrite)
	_, err := OpenWithHost(fixture.reference, fixture.root, Snapshot, fixture.host)
	var busy *BusyError
	if !errors.Is(err, ErrBusy) || !errors.As(err, &busy) || busy.Access != Snapshot {
		t.Fatalf("snapshot result: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	snapshot := fixture.open(t, Snapshot)
	_, err = OpenWithHost(fixture.reference, fixture.root, ReadWrite, fixture.host)
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("writer result: %v", err)
	}
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	fixture.open(t, ReadWrite)
}

// Root-local leases leave unrelated owners running on the same durable volume.
func TestOwnerSnapshotOfStoppedRootCoexistsWithOtherRootWriter(t *testing.T) {
	fixture := newVolumeFixture(t)
	otherRoot := filepath.Join(fixture.mount, "other-owner")
	otherLease := filepath.Join(fixture.mount, "other-lease")
	otherLeaseBytes := []byte("synthetic-independent-owner-lease\n")
	if err := os.Mkdir(otherRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(otherLease, otherLeaseBytes, 0600); err != nil {
		t.Fatal(err)
	}
	fixture.config.Volumes[0].StateRoots = append(fixture.config.Volumes[0].StateRoots, provisionTestRoot(t, otherRoot, otherLease, testDigest(otherLeaseBytes)))
	fixture.writeConfig(t)
	other, err := OpenWithHost(fixture.reference, otherRoot, ReadWrite, fixture.host)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	snapshot := fixture.open(t, Snapshot)
	if err := snapshot.Check(); err != nil {
		t.Fatal(err)
	}
	directory, err := other.OpenDirectory("continues", true)
	if err != nil {
		t.Fatalf("unrelated writer blocked: %v", err)
	}
	if err := directory.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenWithHost(fixture.reference, fixture.root, ReadWrite, fixture.host); !errors.Is(err, ErrBusy) {
		t.Fatalf("same-root writer: %v", err)
	}
}

// Changing an intermediate name to a symlink cannot preserve approval merely
// because the leaf inode and bytes are still those originally opened.
func TestOwnerRewalkRejectsAncestorSymlinkToRetainedInode(t *testing.T) {
	fixture := newVolumeFixture(t)
	owner := fixture.open(t, ReadWrite)
	retained := fixture.mount + "-retained"
	if err := os.Rename(fixture.mount, retained); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(retained, fixture.mount); err != nil {
		t.Fatal(err)
	}
	if err := owner.Check(); !errors.Is(err, ErrIdentity) {
		t.Fatalf("symlink ancestry result: %v", err)
	}
	if err := os.Remove(fixture.mount); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(retained, fixture.mount); err != nil {
		t.Fatal(err)
	}
	if err := owner.Check(); !errors.Is(err, ErrIdentity) {
		t.Fatalf("identity poison disappeared: %v", err)
	}
}

// Permissions are part of every admission, including unchanged inode ancestry.
func TestOwnerRewalkRejectsWritableAncestor(t *testing.T) {
	fixture := newVolumeFixture(t)
	owner := fixture.open(t, ReadWrite)
	if err := os.Chmod(filepath.Dir(fixture.mount), 0777); err != nil {
		t.Fatal(err)
	}
	if err := owner.Check(); !errors.Is(err, ErrIdentity) {
		t.Fatalf("writable ancestor result: %v", err)
	}
	if err := os.Chmod(filepath.Dir(fixture.mount), 0700); err != nil {
		t.Fatal(err)
	}
}

// Identity and journal payload remain distinct, with leases outside inventories.
func TestOwnerRejectsLeaseInsideStateRootAndLeaseReplacement(t *testing.T) {
	fixture := newVolumeFixture(t)
	owner := fixture.open(t, ReadWrite)
	lease := fixture.config.Volumes[0].StateRoots[0].LeasePath
	raw, err := os.ReadFile(lease)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(lease, lease+"-retained"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lease, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := owner.Check(); !errors.Is(err, ErrIdentity) {
		t.Fatalf("replaced lease: %v", err)
	}
	fixture.config.Volumes[0].StateRoots[0].LeasePath = filepath.Join(fixture.root, "lease")
	fixture.writeConfig(t)
	if _, err := Load(fixture.reference); err == nil {
		t.Fatal("lease inside inventory admitted")
	}
}

// Close observes an in-flight real descriptor read, rejects new work, joins it
// and then releases exactly once. The check cannot report a successful admission.
func TestOwnerCloseJoinsCheckAndRejectsLateAdmission(t *testing.T) {
	fixture := newVolumeFixture(t)
	owner := fixture.open(t, ReadWrite)
	entered := make(chan *os.File, 1)
	resume := make(chan struct{})
	fixture.host.change(func() { fixture.host.filesystemHook = func(file *os.File) { entered <- file; <-resume } })
	checked := make(chan error, 1)
	go func() { checked <- owner.Check() }()
	file := <-entered
	closed := make(chan error, 1)
	go func() { closed <- owner.Close() }()
	<-owner.stopping
	if _, err := file.Stat(); err != nil {
		t.Fatalf("close outran in-flight descriptor: %v", err)
	}
	if err := owner.Check(); !errors.Is(err, ErrClosed) {
		t.Fatalf("late check: %v", err)
	}
	close(resume)
	if err := <-checked; !errors.Is(err, ErrClosed) {
		t.Fatalf("in-flight admission: %v", err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("descriptor retained after join: %v", err)
	}
}

// Concurrent callers share only immutable identity and a joined fd lifetime.
func TestOwnerConcurrentCheckAndClose(t *testing.T) {
	fixture := newVolumeFixture(t)
	owner := fixture.open(t, ReadWrite)
	start := make(chan struct{})
	var joined sync.WaitGroup
	results := make(chan error, 32)
	for index := 0; index < 32; index++ {
		joined.Add(1)
		go func() { defer joined.Done(); <-start; results <- owner.Check() }()
	}
	close(start)
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	joined.Wait()
	close(results)
	for err := range results {
		if err != nil && !errors.Is(err, ErrClosed) {
			t.Fatal(err)
		}
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
}

// Uninitialized and nil guards cannot become admitted owners or panic on close.
func TestOwnerZeroAndNilStayClosed(t *testing.T) {
	for _, owner := range []*Owner{nil, {}} {
		if err := owner.Check(); !errors.Is(err, ErrClosed) {
			t.Fatalf("zero admission: %v", err)
		}
		if err := owner.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// Protected no-follow traversal rejects aliases in both root and descendant.
func TestOwnerRejectsSymlinkAndReplacedDescendant(t *testing.T) {
	fixture := newVolumeFixture(t)
	owner := fixture.open(t, ReadWrite)
	directory, err := owner.OpenDirectory("journal", true)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	path := filepath.Join(fixture.root, "journal")
	if err := os.Rename(path, path+"-retained"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := owner.CheckDirectory("journal", directory); err == nil {
		t.Fatal("replaced descendant admitted")
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, filepath.Join(fixture.root, "alias")); err != nil {
		t.Fatal(err)
	}
	reopened := fixture.open(t, ReadWrite)
	if file, err := reopened.OpenDirectory("alias", false); err == nil {
		file.Close()
		t.Fatal("symlink admitted")
	}
}

// Duplicate/unknown fields and changed authenticated bytes never select policy.
func TestLoadRejectsAmbiguousDeclaration(t *testing.T) {
	for _, kind := range []string{"duplicate", "unknown", "hash", "trailing", "overlap"} {
		fixture := newVolumeFixture(t)
		raw, err := os.ReadFile(fixture.reference.Path)
		if err != nil {
			t.Fatal(err)
		}
		switch kind {
		case "duplicate":
			raw = append([]byte(`{"schema":"ignored",`), raw[1:]...)
		case "unknown":
			raw = append([]byte(`{"unknown":true,`), raw[1:]...)
		case "hash":
			raw = append(raw, '\n')
		case "trailing":
			raw = append(raw, []byte(`{}`)...)
		case "overlap":
			fixture.config.Volumes[0].StateRoots = append(fixture.config.Volumes[0].StateRoots, StateRootSpec{Path: filepath.Join(fixture.root, "nested"), LeasePath: filepath.Join(fixture.mount, "nested-lease"), LeaseSha256: fixture.config.Volumes[0].StateRoots[0].LeaseSha256})
			raw, err = json.Marshal(fixture.config)
			if err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(fixture.reference.Path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if kind != "hash" {
			fixture.reference.Sha256 = testDigest(raw)
		}
		if _, err := Load(fixture.reference); err == nil {
			t.Fatalf("%s declaration admitted", kind)
		}
	}
}
