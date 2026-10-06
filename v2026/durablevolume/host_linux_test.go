//go:build linux

// Kernel snapshots exercise both native statfs widths without live mounts.
package durablevolume

import (
	"math"
	"syscall"
	"testing"
)

// High-bit filesystem magic must match the same positive identity on 32/64-bit hosts.
func TestFilesystemFromStatfsMagic(t *testing.T) {
	// Build btrfs magic at runtime so its native signed field wraps on 32-bit hosts.
	btrfs := syscall.Statfs_t{Type: 0x9123683}
	btrfs.Type = btrfs.Type<<4 | 0xe
	// Also cover the signed 32-bit representation when the native field is wider.
	signedBtrfs := syscall.Statfs_t{Type: 0x6edc97c2}
	signedBtrfs.Type = -signedBtrfs.Type
	for _, test := range []struct {
		name  string
		state syscall.Statfs_t
		magic int64
	}{
		{name: "ext4", state: syscall.Statfs_t{Type: 0xef53}, magic: 0xef53},
		{name: "xfs", state: syscall.Statfs_t{Type: 0x58465342}, magic: 0x58465342},
		{name: "btrfs", state: btrfs, magic: 0x9123683e},
		{name: "btrfs signed", state: signedBtrfs, magic: 0x9123683e},
	} {
		test.state.Fsid.X__val = [2]int32{17, 19}
		test.state.Bsize = 4096
		test.state.Bavail = 3
		test.state.Ffree = 5
		test.state.Flags = 1
		got, err := filesystemFromStatfs(test.state)
		want := Filesystem{Id: [2]int32{17, 19}, Type: test.magic, ReadOnly: true, AvailableBytes: 3 * 4096, AvailableInodes: 5}
		if err != nil || got != want {
			t.Errorf("%s statfs type %d: got %+v, %v; want %+v", test.name, test.state.Type, got, err, want)
		}
	}
}

// Invalid native sizes and overflowing availability cannot publish usable facts.
func TestFilesystemFromStatfsRejectsInvalidAvailability(t *testing.T) {
	for _, state := range []syscall.Statfs_t{
		{Bsize: 0, Bavail: 1},
		{Bsize: 4096, Bavail: math.MaxUint64/4096 + 1},
	} {
		if got, err := filesystemFromStatfs(state); err == nil || got != (Filesystem{}) {
			t.Errorf("block size %d, available blocks %d: got %+v, %v", state.Bsize, state.Bavail, got, err)
		}
	}
}
