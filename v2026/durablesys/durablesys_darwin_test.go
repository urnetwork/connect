//go:build darwin

package durablesys

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// Root cause: x/sys's Darwin Fsetxattr passes zero options, so XATTR_CREATE
// silently overwrites. SetAttribute must refuse the same write.
func TestSetAttributeHonorsCreateWhereXSysFsetxattrDropsIt(t *testing.T) {
	file := openTestFile(t)
	if err := unix.Fsetxattr(int(file.Fd()), testAttribute, []byte("original"), unix.XATTR_CREATE); err != nil {
		t.Fatal(err)
	}
	if err := unix.Fsetxattr(int(file.Fd()), testAttribute, []byte("overwritten"), unix.XATTR_CREATE); err != nil {
		t.Fatalf("x/sys Fsetxattr now honors XATTR_CREATE (%v); SetAttribute may delegate to it", err)
	}
	if value, err := readTestAttribute(t, file); err != nil || string(value) != "overwritten" {
		t.Fatalf("x/sys Fsetxattr baseline left %q %v", value, err)
	}
	if err := SetAttribute(int(file.Fd()), testAttribute, []byte("refused"), AttributeCreate); !errors.Is(err, syscall.EEXIST) {
		t.Fatalf("SetAttribute create over an existing attribute returned %v, want EEXIST", err)
	}
	if value, err := readTestAttribute(t, file); err != nil || string(value) != "overwritten" {
		t.Fatalf("refused SetAttribute changed the attribute to %q %v", value, err)
	}
}

// Darwin's ENODATA is a stream errno; absence must never be compared with it.
func TestAbsentAttributeIsNotDarwinEnodata(t *testing.T) {
	file := openTestFile(t)
	_, err := readTestAttribute(t, file)
	if !errors.Is(err, unix.ENOATTR) || errors.Is(err, unix.ENODATA) {
		t.Fatalf("absent attribute returned %v, want ENOATTR and not ENODATA", err)
	}
}

func testMountPoint(t *testing.T) string {
	t.Helper()
	var state unix.Statfs_t
	if err := unix.Statfs(t.TempDir(), &state); err != nil {
		t.Fatal(err)
	}
	return unix.ByteSliceToString(state.Mntonname[:])
}

func TestVolumeUuidIsCanonicalAndStable(t *testing.T) {
	mountPath := testMountPoint(t)
	first, err := VolumeUuid(mountPath)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`).MatchString(first) {
		t.Fatalf("volume uuid %q is not canonical lowercase", first)
	}
	second, err := VolumeUuid(mountPath)
	if err != nil || second != first {
		t.Fatalf("volume uuid changed between reads: %q %q %v", first, second, err)
	}
}

func TestVolumeUuidRefusesNonRootDirectory(t *testing.T) {
	if uuid, err := VolumeUuid(t.TempDir()); err == nil {
		t.Fatalf("non-root directory returned volume uuid %q", uuid)
	}
}

// The link itself lives on the same volume, so only the exact mount point
// comparison distinguishes it from the volume root it names.
func TestVolumeUuidDoesNotFollowSymlink(t *testing.T) {
	link := filepath.Join(t.TempDir(), "volume")
	if err := os.Symlink(testMountPoint(t), link); err != nil {
		t.Fatal(err)
	}
	if uuid, err := VolumeUuid(link); err == nil {
		t.Fatalf("symlink to a volume root returned volume uuid %q", uuid)
	}
}
