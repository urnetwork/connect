//go:build darwin

// Darwin kernel facts observed on the host running the tests: the getfsstat
// census, APFS uuid resolution and custody qualification.
package durablevolume

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/urnetwork/connect/durablesys"
	"golang.org/x/sys/unix"
)

// Fixture custody claims the platform's qualified filesystem type.
const testFilesystemType = "apfs"

// The real temporary directory, its device and the mount point holding it.
func darwinTestVolume(t *testing.T) (string, Device, string) {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var stat unix.Stat_t
	if err := unix.Stat(directory, &stat); err != nil {
		t.Fatal(err)
	}
	var state unix.Statfs_t
	if err := unix.Statfs(directory, &state); err != nil {
		t.Fatal(err)
	}
	return directory, statDevice(&stat), unix.ByteSliceToString(state.Mntonname[:])
}

func TestDarwinMountsQualifyTheTemporaryVolume(t *testing.T) {
	_, device, mountPath := darwinTestVolume(t)
	mounts, err := darwinHost{}.Mounts()
	if err != nil {
		t.Fatal(err)
	}
	var selected []Mount
	ids := map[uint64]bool{}
	for _, mount := range mounts {
		if mount.Id == 0 || ids[mount.Id] {
			t.Fatalf("census mount id is zero or repeated: %+v", mount)
		}
		ids[mount.Id] = true
		if mount.Path == mountPath {
			selected = append(selected, mount)
		}
	}
	if len(selected) != 1 || selected[0].FilesystemType != "apfs" || selected[0].Device != device || selected[0].ReadOnly {
		t.Fatalf("temporary volume census entry is not qualified writable apfs on %+v: %+v", device, selected)
	}
}

func TestDarwinDeviceUuidResolvesQualifiedVolume(t *testing.T) {
	_, device, mountPath := darwinTestVolume(t)
	uuid, err := durablesys.VolumeUuid(mountPath)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := darwinHost{}.DeviceUuid(uuid)
	if err != nil || resolved != device {
		t.Fatalf("volume uuid %s resolved to %+v %v, want %+v", uuid, resolved, err, device)
	}
}

// An unmounted or unknown volume is unavailable, never proven identity loss.
func TestDarwinDeviceUuidReportsUnknownVolumeUnavailable(t *testing.T) {
	_, err := darwinHost{}.DeviceUuid("00000000-0000-4000-8000-000000000000")
	if !errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrIdentity) {
		t.Fatalf("unknown uuid returned %v, want not-exist without identity loss", err)
	}
}

// The sealed system snapshot has a uuid but can never resolve as custody.
func TestDarwinDeviceUuidIgnoresSystemSnapshot(t *testing.T) {
	var state unix.Statfs_t
	if err := unix.Statfs("/", &state); err != nil {
		t.Fatal(err)
	}
	if state.Flags&unix.MNT_SNAPSHOT == 0 {
		t.Skip("root filesystem is not a sealed snapshot on this host")
	}
	uuid, err := durablesys.VolumeUuid("/")
	if err != nil {
		t.Fatal(err)
	}
	if device, err := (darwinHost{}).DeviceUuid(uuid); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("system snapshot uuid resolved to %+v %v", device, err)
	}
}

func TestDarwinFilesystemReportsQualifiedApfsOnly(t *testing.T) {
	directory, _, _ := darwinTestVolume(t)
	for _, entry := range []struct {
		path      string
		qualified bool
	}{
		{path: directory, qualified: true},
		{path: "/", qualified: false},
	} {
		file, err := os.Open(entry.path)
		if err != nil {
			t.Fatal(err)
		}
		facts, err := darwinHost{}.Filesystem(file)
		if closeErr := file.Close(); err != nil || closeErr != nil {
			t.Fatal(err, closeErr)
		}
		var state unix.Statfs_t
		if err := unix.Statfs(entry.path, &state); err != nil {
			t.Fatal(err)
		}
		if entry.qualified != (facts.Type == filesystemMagic("apfs")) || entry.qualified && (facts.ReadOnly || facts.AvailableBytes == 0) {
			t.Fatalf("%s filesystem facts %+v, qualified want %v", entry.path, facts, entry.qualified)
		}
		if entry.path == "/" && state.Flags&unix.MNT_SNAPSHOT != 0 && !facts.ReadOnly {
			t.Fatalf("sealed system snapshot reported writable: %+v", facts)
		}
	}
}

// Ownership-ignoring volumes, snapshots, network mounts and other filesystems
// cannot qualify even when their type name begins as APFS.
func TestDarwinQualificationRequiresLocalOwnedApfs(t *testing.T) {
	state := func(kind string, flags uint32) *unix.Statfs_t {
		result := &unix.Statfs_t{Flags: flags}
		copy(result.Fstypename[:], kind)
		return result
	}
	for _, entry := range []struct {
		state     *unix.Statfs_t
		qualified bool
	}{
		{state: state("apfs", unix.MNT_LOCAL), qualified: true},
		{state: state("apfs", unix.MNT_LOCAL|unix.MNT_RDONLY), qualified: true},
		{state: state("apfs", unix.MNT_LOCAL|unix.MNT_IGNORE_OWNERSHIP), qualified: false},
		{state: state("apfs", unix.MNT_LOCAL|unix.MNT_SNAPSHOT), qualified: false},
		{state: state("apfs", 0), qualified: false},
		{state: state("hfs", unix.MNT_LOCAL), qualified: false},
		{state: state("smbfs", 0), qualified: false},
	} {
		if qualifiedVolume(entry.state) != entry.qualified {
			t.Errorf("%s flags %#x qualified %v, want %v", unix.ByteSliceToString(entry.state.Fstypename[:]), entry.state.Flags, !entry.qualified, entry.qualified)
		}
	}
}
