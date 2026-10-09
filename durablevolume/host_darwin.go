//go:build darwin

// Darwin kernel facts: the getfsstat mount census, APFS volume uuid resolution
// and a stand-in filesystem magic. Only a local APFS volume with ownership
// enforced that is not a snapshot mount is reported as type "apfs"; any other
// APFS mount is "apfs-unqualified" and fails the declared type check, so a
// network share, an ownership-ignoring external volume or the sealed system
// snapshot can never hold custody. Descriptor walks and admission are shared
// with Linux in host_unix.go.
package durablevolume

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"os"
	"path/filepath"

	"github.com/urnetwork/connect/durablesys"
	"golang.org/x/sys/unix"
)

// Fixed host facts are not configurable from a runtime declaration.
type darwinHost struct{}

// Each owner gets an immutable adapter with no process-global state.
func defaultHost() Host { return darwinHost{} }

// Darwin encodes dev_t as major<<24 | minor; fsid and stat share it.
func deviceNumber(number uint64) Device {
	return Device{Major: unix.Major(number), Minor: unix.Minor(number)}
}

// APFS has no statfs magic number; "apfs" in ASCII stands in for qualified
// volumes so the shared type checks keep one meaning on both platforms.
func filesystemMagic(name string) int64 {
	if name == "apfs" {
		return 0x61706673
	}
	return -1
}

// Local APFS with ownership enforced, not a snapshot. Read-only state is a
// separate fact that only refuses writes.
func qualifiedVolume(state *unix.Statfs_t) bool {
	return unix.ByteSliceToString(state.Fstypename[:]) == "apfs" && state.Flags&unix.MNT_LOCAL != 0 &&
		state.Flags&(unix.MNT_IGNORE_OWNERSHIP|unix.MNT_SNAPSHOT) == 0
}

// One bounded snapshot. A census that fills its buffer may be partial and is
// refused rather than treated as complete.
func mountStates() ([]unix.Statfs_t, error) {
	count, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil {
		return nil, err
	}
	if count <= 0 || count > 8192 {
		return nil, errors.New("mount information is empty or exceeds its bound")
	}
	states := make([]unix.Statfs_t, count+16)
	count, err = unix.Getfsstat(states, unix.MNT_NOWAIT)
	if err != nil {
		return nil, err
	}
	if count <= 0 || count >= len(states) {
		return nil, errors.New("mount information changed during its census")
	}
	return states[:count], nil
}

// Darwin has no mount ids; the fsid and mount path identify one census entry.
// A remount at the same path keeps its id, but forced unmount revokes every
// retained descriptor, which the shared same-file checks then refuse.
func (self darwinHost) Mounts() ([]Mount, error) {
	states, err := mountStates()
	if err != nil {
		return nil, err
	}
	result := make([]Mount, 0, len(states))
	for index := range states {
		state := &states[index]
		path := unix.ByteSliceToString(state.Mntonname[:])
		kind := unix.ByteSliceToString(state.Fstypename[:])
		if path == "" || !filepath.IsAbs(path) || kind == "" {
			return nil, errors.New("mount identity is malformed")
		}
		if kind == "apfs" && !qualifiedVolume(state) {
			kind = "apfs-unqualified"
		}
		id := fnv.New64a()
		_ = binary.Write(id, binary.LittleEndian, state.Fsid.Val)
		_, _ = id.Write([]byte(path))
		result = append(result, Mount{Id: max(id.Sum64(), 1), Device: deviceNumber(uint64(uint32(state.Fsid.Val[0]))), Root: "/", Path: path,
			FilesystemType: kind, ReadOnly: state.Flags&unix.MNT_RDONLY != 0})
	}
	return result, nil
}

// Exactly one qualified mounted volume may carry the configured uuid. An
// unmounted volume, or one whose identity could not be read, is unavailable
// rather than proven lost; two matches are ambiguous identity.
func (self darwinHost) DeviceUuid(uuid string) (Device, error) {
	states, err := mountStates()
	if err != nil {
		return Device{}, err
	}
	var matches []Device
	var observeErr error
	for index := range states {
		state := &states[index]
		if !qualifiedVolume(state) {
			continue
		}
		volume, err := durablesys.VolumeUuid(unix.ByteSliceToString(state.Mntonname[:]))
		if err != nil {
			observeErr = errors.Join(observeErr, err)
			continue
		}
		if volume == uuid {
			matches = append(matches, deviceNumber(uint64(uint32(state.Fsid.Val[0]))))
		}
	}
	switch {
	case len(matches) > 1:
		return Device{}, errors.Join(ErrIdentity, errors.New("filesystem uuid resolves to more than one mounted volume"))
	case len(matches) == 1:
		return matches[0], nil
	case observeErr != nil:
		return Device{}, observeErr
	}
	return Device{}, fmt.Errorf("no qualified mounted volume has filesystem uuid %q: %w", uuid, os.ErrNotExist)
}

// Available blocks/inodes use the process's available allocation, not totals.
func (self darwinHost) Filesystem(directory *os.File) (Filesystem, error) {
	var state unix.Statfs_t
	if err := unix.Fstatfs(int(directory.Fd()), &state); err != nil {
		return Filesystem{}, err
	}
	if state.Bsize == 0 || state.Bavail > math.MaxUint64/uint64(state.Bsize) {
		return Filesystem{}, errors.New("filesystem available-byte arithmetic is invalid")
	}
	kind := int64(0)
	if qualifiedVolume(&state) {
		kind = filesystemMagic("apfs")
	}
	return Filesystem{Id: state.Fsid.Val, Type: kind, ReadOnly: state.Flags&unix.MNT_RDONLY != 0,
		AvailableBytes: state.Bavail * uint64(state.Bsize), AvailableInodes: state.Ffree}, nil
}

// A compile-time assertion keeps the public host facts narrow and explicit.
var _ Host = darwinHost{}
