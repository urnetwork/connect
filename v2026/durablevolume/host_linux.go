//go:build linux

// Linux kernel facts: the mountinfo census, block-device uuid resolution and
// filesystem magic numbers. Descriptor walks and admission are shared with
// Darwin in host_unix.go.
package durablevolume

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Fixed host paths are not configurable from a runtime declaration.
type linuxHost struct{}

// Each owner gets an immutable adapter with no process-global state.
func defaultHost() Host { return linuxHost{} }

// Linux's encoded dev_t is stable across stat and block-device identity reads.
func deviceNumber(number uint64) Device {
	return Device{Major: uint32((number>>8)&0xfff | (number>>32)&0xfffff000), Minor: uint32(number&0xff | (number>>12)&0xffffff00)}
}

// Filesystem type constants have exactly the same daemon/owner-local meaning.
func filesystemMagic(name string) int64 {
	switch name {
	case "ext4":
		return 0xef53
	case "xfs":
		return 0x58465342
	case "btrfs":
		return 0x9123683e
	}
	return -1
}

// Reads only a bounded kernel snapshot; malformed escapes cannot alter a path.
func (self linuxHost) Mounts() ([]Mount, error) {
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, 4*1024*1024+1))
	if err := errors.Join(readErr, file.Close()); err != nil {
		return nil, err
	}
	if len(raw) > 4*1024*1024 {
		return nil, errors.New("mount information exceeds its byte bound")
	}
	return parseMounts(raw)
}

// Parsing is independent of ambient mounts so escaped and competing entries
// can be qualified without mount privileges or changes to the host namespace.
func parseMounts(raw []byte) ([]Mount, error) {
	decodePath := func(value string) (string, error) {
		var result strings.Builder
		for index := 0; index < len(value); index++ {
			if value[index] != '\\' {
				result.WriteByte(value[index])
				continue
			}
			if index+4 > len(value) {
				return "", errors.New("mount path escape is truncated")
			}
			switch value[index : index+4] {
			case `\040`:
				result.WriteByte(' ')
			case `\011`:
				result.WriteByte('\t')
			case `\012`:
				result.WriteByte('\n')
			case `\134`:
				result.WriteByte('\\')
			default:
				return "", errors.New("mount path escape is unknown")
			}
			index += 3
		}
		return result.String(), nil
	}
	var result []Mount
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 4096), 64*1024)
	for scanner.Scan() {
		parts := strings.Fields(scanner.Text())
		separator := -1
		for index, part := range parts {
			if part == "-" {
				separator = index
				break
			}
		}
		if len(result) >= 8192 || separator < 6 || len(parts) != separator+4 {
			return nil, errors.New("mount information has an invalid or unbounded record")
		}
		id, idErr := strconv.ParseUint(parts[0], 10, 64)
		parent, parentErr := strconv.ParseUint(parts[1], 10, 64)
		deviceParts := strings.Split(parts[2], ":")
		if idErr != nil || parentErr != nil || id == 0 || len(deviceParts) != 2 {
			return nil, errors.New("mount identity is malformed")
		}
		major, majorErr := strconv.ParseUint(deviceParts[0], 10, 32)
		minor, minorErr := strconv.ParseUint(deviceParts[1], 10, 32)
		root, rootErr := decodePath(parts[3])
		path, pathErr := decodePath(parts[4])
		if err := errors.Join(majorErr, minorErr, rootErr, pathErr); err != nil {
			return nil, err
		}
		readOnly := false
		for _, option := range strings.Split(parts[5]+","+parts[separator+3], ",") {
			readOnly = readOnly || option == "ro"
		}
		result = append(result, Mount{Id: id, ParentId: parent, Device: Device{Major: uint32(major), Minor: uint32(minor)}, Root: root, Path: path, FilesystemType: parts[separator+1], ReadOnly: readOnly})
	}
	return result, scanner.Err()
}

// Re-enumerated device names are accepted only through the configured uuid.
func (self linuxHost) DeviceUuid(uuid string) (Device, error) {
	var stat syscall.Stat_t
	if err := syscall.Stat(filepath.Join("/dev/disk/by-uuid", uuid), &stat); err != nil {
		return Device{}, err
	}
	if stat.Mode&syscall.S_IFMT != syscall.S_IFBLK {
		return Device{}, errors.Join(ErrIdentity, errors.New("filesystem uuid does not resolve to a block device"))
	}
	return deviceNumber(uint64(stat.Rdev)), nil
}

// Available blocks/inodes use the process's available allocation, not totals.
func (self linuxHost) Filesystem(directory *os.File) (Filesystem, error) {
	var state syscall.Statfs_t
	if err := syscall.Fstatfs(int(directory.Fd()), &state); err != nil {
		return Filesystem{}, err
	}
	return filesystemFromStatfs(state)
}

// Kernel magic numbers retain their unsigned 32-bit identity in signed fields.
func filesystemFromStatfs(state syscall.Statfs_t) (Filesystem, error) {
	if state.Bsize <= 0 || state.Bavail > math.MaxUint64/uint64(state.Bsize) {
		return Filesystem{}, errors.New("filesystem available-byte arithmetic is invalid")
	}
	return Filesystem{Id: state.Fsid.X__val, Type: int64(uint32(state.Type)), ReadOnly: state.Flags&1 != 0,
		AvailableBytes: state.Bavail * uint64(state.Bsize), AvailableInodes: state.Ffree}, nil
}

// A compile-time assertion keeps the public host facts narrow and explicit.
var _ Host = linuxHost{}
