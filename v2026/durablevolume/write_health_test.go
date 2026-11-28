//go:build linux || darwin

// Failure-only host hooks retain real anonymous allocation, descriptor writes,
// syncs and closure while exposing admission and cleanup at exact boundaries.
package durablevolume

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// Every explicit admission owns a new bounded anonymous write. Ordinary
// identity/capacity and read-only checks do not duplicate these mutations.
func TestOwnerWriteHealthUsesFreshAnonymousProbe(t *testing.T) {
	for _, local := range []bool{false, true} {
		fixture := newVolumeFixture(t)
		open := OpenWithHost
		if local {
			fixture.config.Schema = OwnerLocalSchema
			fixture.writeConfig(t)
			open = OpenOwnerLocalWithHost
		}
		var operations []string
		var probe *os.File
		fixture.host.writeHealthHook = func(operation string, file *os.File) error {
			operations = append(operations, operation)
			switch operation {
			case "open", "root-sync":
				opened, err := file.Stat()
				if err != nil {
					return err
				}
				named, err := os.Stat(fixture.root)
				if err != nil {
					return err
				}
				if !os.SameFile(opened, named) {
					return errors.New("probe escaped the retained owner root")
				}
			case "file-write":
				probe = file
			case "file-sync":
				var stat unix.Stat_t
				if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
					return err
				}
				if file != probe || stat.Size <= 0 || stat.Size > 4096 || stat.Nlink != 0 || stat.Mode&07777 != 0600 || statDevice(&stat) != fixture.host.uuidDevice {
					return errors.New("probe lacks a bounded actual write to a private anonymous inode")
				}
			case "file-close":
				if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
					return errors.New("probe closure was replaced by a synthetic observation")
				}
			}
			return nil
		}
		owner, err := open(fixture.reference, fixture.root, ReadWrite, fixture.host)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := owner.Close(); err != nil {
				t.Error(err)
			}
		})
		want := "open,file-write,file-sync,file-close,root-sync"
		if strings.Join(operations, ",") != want {
			t.Fatal("initial writable admission omitted its probe", operations)
		}
		operations = nil
		directory, err := owner.OpenDirectory("", false)
		if err != nil {
			t.Fatal(err)
		}
		if err := errors.Join(owner.Check(), owner.CheckRead(), owner.CheckDirectory("", directory), owner.CheckReadDirectory("", directory), directory.Close()); err != nil {
			t.Fatal(err)
		}
		if len(operations) != 0 {
			t.Fatal("ordinary inspection performed a write probe", operations)
		}
		for range 2 {
			if err := owner.CheckWrite(); err != nil {
				t.Fatal(err)
			}
			if strings.Join(operations, ",") != want {
				t.Fatal("write admission reused a previous health result", operations)
			}
			operations = nil
			entries, err := os.ReadDir(fixture.root)
			if err != nil || len(entries) != 0 {
				t.Fatal("probe published or retained a name", entries, err)
			}
			requireNoProbeNames(t, fixture)
		}
	}
}

// Healthy statfs and readable custody cannot hide failed write/sync I/O. Each
// refusal retains its cause and permits a fresh check of the same generation.
func TestOwnerWriteHealthRefusesIoFailuresAndRetainsRetry(t *testing.T) {
	fixture := newVolumeFixture(t)
	owner := fixture.open(t, ReadWrite)
	for _, fault := range []struct {
		operation string
		cause     error
	}{
		{operation: "open", cause: syscall.EIO},
		{operation: "open", cause: syscall.ENOSPC},
		{operation: "open", cause: syscall.EOPNOTSUPP},
		{operation: "open", cause: syscall.EBADF},
		{operation: "file-write", cause: syscall.EIO},
		{operation: "file-write", cause: syscall.ENOSPC},
		{operation: "file-write", cause: syscall.EDQUOT},
		{operation: "file-write", cause: syscall.EROFS},
		{operation: "file-write", cause: io.ErrShortWrite},
		{operation: "file-write", cause: syscall.EBADF},
		{operation: "file-sync", cause: syscall.EIO},
		{operation: "file-sync", cause: syscall.ENOSPC},
		{operation: "file-sync", cause: syscall.EBADF},
		{operation: "file-close", cause: syscall.EIO},
		{operation: "file-close", cause: syscall.EBADF},
		{operation: "root-sync", cause: syscall.EIO},
		{operation: "root-sync", cause: syscall.ENOSPC},
		{operation: "root-sync", cause: syscall.EBADF},
	} {
		var probe *os.File
		refused := false
		fixture.host.change(func() {
			fixture.host.writeHealthHook = func(step string, file *os.File) error {
				if step == "file-close" {
					probe = file
				}
				if step == fault.operation {
					refused = true
					return fault.cause
				}
				return nil
			}
		})
		err := owner.CheckWrite()
		wantUnavailable := !errors.Is(fault.cause, syscall.EBADF)
		if !refused || !errors.Is(err, fault.cause) || errors.Is(err, ErrUnavailable) != wantUnavailable || errors.Is(err, ErrIdentity) {
			t.Fatalf("%s/%v admitted failed health or lost its cause: %v", fault.operation, fault.cause, err)
		}
		if fault.operation != "open" && probe == nil {
			t.Fatal("allocated probe omitted cleanup", fault.operation, fault.cause)
		}
		if probe != nil {
			if _, err := probe.Stat(); !errors.Is(err, os.ErrClosed) {
				t.Fatal("failed probe retained its descriptor", fault.operation, fault.cause, err)
			}
		}
		entries, err := os.ReadDir(fixture.root)
		if err != nil || len(entries) != 0 {
			t.Fatal("failed probe left namespace cleanup", entries, err)
		}
		requireNoProbeNames(t, fixture)
		if err := owner.CheckRead(); err != nil {
			t.Fatal("failed write health prevented read-only inspection", err)
		}
		fixture.host.change(func() { fixture.host.writeHealthHook = nil })
		if err := owner.CheckWrite(); err != nil {
			t.Fatal("failed write observation poisoned original custody", fault.operation, fault.cause, err)
		}
	}
}

// A partially written probe is discarded even if closure and root durability
// also fail. Every cause remains visible; no uncertain application data exists.
func TestOwnerWriteHealthPartialWritePreservesCleanupFailures(t *testing.T) {
	fixture := newVolumeFixture(t)
	owner := fixture.open(t, ReadWrite)
	var probe *os.File
	fixture.host.writeHealthHook = func(operation string, file *os.File) error {
		switch operation {
		case "file-write":
			probe = file
			_, err := file.Write([]byte("partial"))
			return errors.Join(syscall.ENOSPC, err)
		case "file-close":
			return syscall.EIO
		case "root-sync":
			return syscall.EDQUOT
		}
		return nil
	}
	err := owner.CheckWrite()
	if !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrIdentity) || !errors.Is(err, syscall.ENOSPC) || !errors.Is(err, syscall.EIO) || !errors.Is(err, syscall.EDQUOT) {
		t.Fatal("partial probe lost a write or cleanup failure", err)
	}
	if probe == nil {
		t.Fatal("probe was never written")
	}
	if _, err := probe.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatal("partial probe retained its descriptor", err)
	}
	entries, err := os.ReadDir(fixture.root)
	if err != nil || len(entries) != 0 {
		t.Fatal("partial probe retained a name", entries, err)
	}
	requireNoProbeNames(t, fixture)
	fixture.host.writeHealthHook = nil
	if err := owner.CheckWrite(); err != nil {
		t.Fatal("same owner could not retry after failed probe cleanup", err)
	}
}

// Initial health failure must close both the anonymous probe and the owner's
// shared lease. Exclusive read-only inspection then succeeds without a probe.
func TestOwnerWriteHealthConstructorRefusalReleasesLease(t *testing.T) {
	for _, local := range []bool{false, true} {
		for _, operation := range []string{"open", "file-write", "file-sync", "file-close", "root-sync"} {
			fixture := newVolumeFixture(t)
			open := OpenWithHost
			if local {
				fixture.config.Schema = OwnerLocalSchema
				fixture.writeConfig(t)
				open = OpenOwnerLocalWithHost
			}
			var probe *os.File
			fixture.host.writeHealthHook = func(step string, file *os.File) error {
				if step == "file-close" {
					probe = file
				}
				if step == operation {
					return syscall.EIO
				}
				return nil
			}
			owner, err := open(fixture.reference, fixture.root, ReadWrite, fixture.host)
			if owner != nil || !errors.Is(err, syscall.EIO) || !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrIdentity) {
				if owner != nil {
					owner.Close()
				}
				t.Fatal("initial failed write health was admitted", operation, local, err)
			}
			if operation != "open" && probe == nil {
				t.Fatal("failed constructor omitted probe cleanup", operation, local)
			}
			if probe != nil {
				if _, err := probe.Stat(); !errors.Is(err, os.ErrClosed) {
					t.Fatal("failed constructor retained a probe descriptor", err)
				}
			}
			owner, err = open(fixture.reference, fixture.root, Snapshot, fixture.host)
			if err != nil {
				t.Fatal("failed constructor retained its lease or snapshot attempted a write", err)
			}
			if err := errors.Join(owner.CheckRead(), owner.Close()); err != nil {
				t.Fatal(err)
			}
			fixture.host.writeHealthHook = nil
			owner, err = open(fixture.reference, fixture.root, ReadWrite, fixture.host)
			if err != nil {
				t.Fatal("initial write-health refusal prevented exact recovery", err)
			}
			if err := owner.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// All read access modes and writer-owned read-only inspection remain usable
// without probing writes, even with failed write health and no write reserve.
func TestOwnerWriteHealthReadAdmissionDoesNotProbe(t *testing.T) {
	for _, access := range []Access{ReadOnly, Snapshot, ReadWrite} {
		fixture := newVolumeFixture(t)
		var owner *Owner
		if access == ReadWrite {
			owner = fixture.open(t, access)
		}
		probes := 0
		fixture.host.writeHealthHook = func(string, *os.File) error {
			probes++
			return syscall.EIO
		}
		if owner == nil {
			owner = fixture.open(t, access)
		}
		directory, err := owner.OpenDirectory("", false)
		if err != nil {
			t.Fatal(err)
		}
		if err := errors.Join(owner.Check(), owner.CheckDirectory("", directory)); err != nil {
			t.Fatal("identity inspection depended on write health", access, err)
		}
		fixture.host.change(func() {
			fixture.host.mounts[1].ReadOnly = true
			fixture.host.filesystem.ReadOnly = true
			fixture.host.filesystem.AvailableBytes = 0
			fixture.host.filesystem.AvailableInodes = 0
		})
		if err := errors.Join(owner.CheckRead(), owner.CheckReadDirectory("", directory), directory.Close()); err != nil {
			t.Fatal("read-only inspection required write health or reserve", access, err)
		}
		if err := owner.CheckWrite(); err == nil {
			t.Fatal("read admission granted mutation", access)
		}
		if probes != 0 {
			t.Fatal("read-only admission attempted a write probe", access, probes)
		}
	}
}

// Established namespace, device, protection and capacity refusals occur before
// allocating anything, including when the visible mount has disappeared.
func TestOwnerWriteHealthPreconditionsPrecedeProbe(t *testing.T) {
	for _, kind := range []string{"mount-read-only", "filesystem-read-only", "bytes", "inodes", "absent-mount", "filesystem-id", "marker"} {
		fixture := newVolumeFixture(t)
		owner := fixture.open(t, ReadWrite)
		probes := 0
		fixture.host.writeHealthHook = func(string, *os.File) error {
			probes++
			return syscall.EIO
		}
		want := ErrUnavailable
		switch kind {
		case "mount-read-only":
			fixture.host.mounts[1].ReadOnly = true
		case "filesystem-read-only":
			fixture.host.filesystem.ReadOnly = true
		case "bytes":
			fixture.host.filesystem.AvailableBytes = 0
		case "inodes":
			fixture.host.filesystem.AvailableInodes = 0
		case "absent-mount":
			fixture.host.mounts = fixture.host.mounts[:1]
			want = ErrIdentity
		case "filesystem-id":
			fixture.host.filesystem.Id[0]++
			want = ErrIdentity
		case "marker":
			if err := os.WriteFile(fixture.marker, []byte("synthetic-replaced-marker"), 0600); err != nil {
				t.Fatal(err)
			}
			want = ErrIdentity
		}
		if err := owner.CheckWrite(); !errors.Is(err, want) || probes != 0 {
			t.Fatal("probe preceded existing admission", kind, probes, err)
		}
	}
}

// Probe completion is not permission to acknowledge custody or reserve that
// changed during I/O. Proven replacement remains sticky after exact restoration.
func TestOwnerWriteHealthRechecksAdmissionAfterProbe(t *testing.T) {
	for _, kind := range []string{"mount", "filesystem", "root", "read-only", "reserve"} {
		fixture := newVolumeFixture(t)
		owner := fixture.open(t, ReadWrite)
		observed := false
		fixture.host.writeHealthHook = func(operation string, _ *os.File) error {
			if operation != "root-sync" {
				return nil
			}
			observed = true
			switch kind {
			case "mount":
				fixture.host.change(func() { fixture.host.mounts[1].Id++ })
			case "filesystem":
				fixture.host.change(func() { fixture.host.filesystem.Id[0]++ })
			case "root":
				if err := os.Rename(fixture.root, fixture.root+"-retained"); err != nil {
					return err
				}
				return os.Mkdir(fixture.root, 0700)
			case "read-only":
				fixture.host.change(func() { fixture.host.filesystem.ReadOnly = true })
			case "reserve":
				fixture.host.change(func() { fixture.host.filesystem.AvailableBytes = 0 })
			}
			return nil
		}
		want := ErrIdentity
		if kind == "read-only" || kind == "reserve" {
			want = ErrUnavailable
		}
		if err := owner.CheckWrite(); !observed || !errors.Is(err, want) {
			t.Fatal("successful probe hid changed admission", kind, err)
		}
		fixture.host.writeHealthHook = nil
		switch kind {
		case "mount":
			fixture.host.mounts[1].Id--
		case "filesystem":
			fixture.host.filesystem.Id[0]--
		case "root":
			if err := os.Remove(fixture.root); err != nil {
				t.Fatal("probe mutated the replacement root", err)
			}
			if err := os.Rename(fixture.root+"-retained", fixture.root); err != nil {
				t.Fatal(err)
			}
		case "read-only":
			fixture.host.filesystem.ReadOnly = false
		case "reserve":
			fixture.host.filesystem.AvailableBytes = 1024 * 1024
		}
		err := owner.CheckWrite()
		if want == ErrIdentity && !errors.Is(err, ErrIdentity) || want == ErrUnavailable && err != nil {
			t.Fatal("probe changed existing identity/retry semantics", kind, err)
		}
	}
}

// Close rejects new admissions but cannot close retained descriptors or finish
// before a blocked synchronous probe releases its own anonymous descriptor.
func TestOwnerWriteHealthCloseJoinsProbe(t *testing.T) {
	fixture := newVolumeFixture(t)
	owner := fixture.open(t, ReadWrite)
	entered := make(chan *os.File)
	resume := make(chan struct{})
	var resumeOnce sync.Once
	t.Cleanup(func() { resumeOnce.Do(func() { close(resume) }) })
	fixture.host.writeHealthHook = func(operation string, file *os.File) error {
		if operation == "file-sync" {
			entered <- file
			<-resume
		}
		return nil
	}
	checked := make(chan error, 1)
	go func() { checked <- owner.CheckWrite() }()
	var probe *os.File
	select {
	case probe = <-entered:
	case err := <-checked:
		t.Fatal("write admission completed without joining its probe", err)
	}
	closed := make(chan error, 1)
	go func() { closed <- owner.Close() }()
	<-owner.stopping
	if err := owner.CheckWrite(); !errors.Is(err, ErrClosed) {
		t.Fatal("closing owner admitted a new probe", err)
	}
	if _, err := owner.rootFile.Stat(); err != nil {
		t.Fatal("close released the root during an active probe", err)
	}
	if _, err := probe.Stat(); err != nil {
		t.Fatal("close released the active probe", err)
	}
	resumeOnce.Do(func() { close(resume) })
	if err := <-checked; !errors.Is(err, ErrClosed) {
		t.Fatal("probe success outran owner close", err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if _, err := probe.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatal("joined probe retained a descriptor", err)
	}
}

// A failed root's write-health observation does not close, poison or consume
// mutable admission state belonging to another independently owned root.
func TestOwnerWriteHealthFailureLeavesIndependentOwnerUsable(t *testing.T) {
	failedFixture := newVolumeFixture(t)
	failedOwner := failedFixture.open(t, ReadWrite)
	healthyFixture := newVolumeFixture(t)
	healthyOwner := healthyFixture.open(t, ReadWrite)
	failedFixture.host.writeHealthHook = func(string, *os.File) error { return syscall.EIO }
	if err := failedOwner.CheckWrite(); !errors.Is(err, ErrUnavailable) {
		t.Fatal("failed owner admitted write health", err)
	}
	if err := healthyOwner.CheckWrite(); err != nil {
		t.Fatal("independent owner inherited another root's failure", err)
	}
	if err := failedOwner.Close(); err != nil {
		t.Fatal(err)
	}
	if err := healthyOwner.CheckWrite(); err != nil {
		t.Fatal("closing a failed owner changed independent admission", err)
	}
}

// Darwin names its probe beside the external lease before unlinking it; no
// platform may leave that name, or any probe, behind after a check returns.
func requireNoProbeNames(t *testing.T, fixture *volumeFixture) {
	t.Helper()
	for _, directory := range []string{fixture.root, filepath.Dir(fixture.config.Volumes[0].StateRoots[0].LeasePath)} {
		entries, err := os.ReadDir(directory)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".urnetwork-write-health-") {
				t.Fatal("write probe retained its name", directory, entry.Name())
			}
		}
	}
}
