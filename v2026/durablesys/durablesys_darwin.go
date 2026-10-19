//go:build darwin

// Darwin supplies no-replace and exchange renames through renameatx_np and
// reports an absent extended attribute as ENOATTR. Darwin defines ENODATA too,
// with an unrelated meaning, so callers must compare with ErrNoAttribute.
//
// Descriptor attribute writes call libc fsetxattr directly: x/sys's Darwin
// Fsetxattr passes zero options, silently dropping XATTR_CREATE/XATTR_REPLACE.
// Volume identity calls libc getattrlist. Both use the same libSystem
// trampolines as x/sys rather than raw kernel traps, whose ABI is private.
package durablesys

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Fails with EEXIST when the attribute already exists.
const AttributeCreate = unix.XATTR_CREATE

// Fails with ErrNoAttribute when the attribute is absent.
const AttributeReplace = unix.XATTR_REPLACE

// Darwin reports an absent extended attribute as ENOATTR.
const ErrNoAttribute = unix.ENOATTR

// Atomic rename that fails with EEXIST instead of replacing the target name.
func RenameNoReplace(fromDirectory int, fromName string, toDirectory int, toName string) error {
	return unix.RenameatxNp(fromDirectory, fromName, toDirectory, toName, unix.RENAME_EXCL)
}

// Atomically swaps two existing names; either missing name fails.
func RenameExchange(fromDirectory int, fromName string, toDirectory int, toName string) error {
	return unix.RenameatxNp(fromDirectory, fromName, toDirectory, toName, unix.RENAME_SWAP)
}

// Descriptor-relative write. Flags are 0, AttributeCreate or AttributeReplace;
// any other value is refused rather than passed to the kernel. The flags travel
// in the options argument, with position zero as required for ordinary names.
func SetAttribute(fd int, name string, value []byte, flags int) error {
	if flags != 0 && flags != AttributeCreate && flags != AttributeReplace {
		return syscall.EINVAL
	}
	key, err := unix.BytePtrFromString(name)
	if err != nil {
		return err
	}
	var data unsafe.Pointer
	if len(value) > 0 {
		data = unsafe.Pointer(&value[0])
	}
	_, _, errno := syscallSyscall6(libcFsetxattrTrampolineAddr, uintptr(fd), uintptr(unsafe.Pointer(key)), uintptr(data), uintptr(len(value)), 0, uintptr(flags))
	runtime.KeepAlive(key)
	runtime.KeepAlive(value)
	if errno != 0 {
		return errno
	}
	return nil
}

// Descriptor-relative read. A short buffer fails with ERANGE.
func GetAttribute(fd int, name string, value []byte) (int, error) {
	return unix.Fgetxattr(fd, name, value)
}

// NUL-separated attribute names. A short buffer fails with ERANGE.
func ListAttributes(fd int, names []byte) (int, error) {
	return unix.Flistxattr(fd, names)
}

// The filesystem provides no extended attributes. Darwin distinguishes ENOTSUP
// from EOPNOTSUPP; either refuses the attribute contract.
func AttributeUnsupported(err error) bool {
	return errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.ENOSYS)
}

// Canonical lowercase uuid of the volume mounted exactly at mountPath. The
// kernel answers for the volume containing any path, so the returned mount
// point must equal mountPath byte for byte; a symlink is not followed, and the
// reply must confirm that both attributes were actually returned.
func VolumeUuid(mountPath string) (string, error) {
	path, err := unix.BytePtrFromString(mountPath)
	if err != nil {
		return "", err
	}
	request := unix.Attrlist{
		Bitmapcount: unix.ATTR_BIT_MAP_COUNT,
		Commonattr:  unix.ATTR_CMN_RETURNED_ATTRS,
		Volattr:     unix.ATTR_VOL_INFO | unix.ATTR_VOL_MOUNTPOINT | unix.ATTR_VOL_UUID,
	}
	// Reply: u_int32 length, attribute_set_t (five u_int32), the mount point's
	// attrreference_t (int32 offset from itself, u_int32 length including NUL),
	// the uuid_t, then the variable-length mount point bytes.
	const returnedOffset = 4
	const mountOffset = returnedOffset + 5*4
	const uuidOffset = mountOffset + 8
	const fixedBytes = uuidOffset + 16
	reply := make([]byte, fixedBytes+2*1024)
	_, _, errno := syscallSyscall6(libcGetattrlistTrampolineAddr, uintptr(unsafe.Pointer(path)), uintptr(unsafe.Pointer(&request)), uintptr(unsafe.Pointer(&reply[0])), uintptr(len(reply)), uintptr(unix.FSOPT_NOFOLLOW), 0)
	runtime.KeepAlive(path)
	runtime.KeepAlive(&request)
	runtime.KeepAlive(reply)
	if errno != 0 {
		return "", errno
	}
	length := int(binary.NativeEndian.Uint32(reply[0:4]))
	returnedVolume := binary.NativeEndian.Uint32(reply[returnedOffset+4 : returnedOffset+8])
	required := uint32(unix.ATTR_VOL_MOUNTPOINT | unix.ATTR_VOL_UUID)
	if length < fixedBytes || length > len(reply) || returnedVolume&required != required {
		return "", errors.New("volume mount point and uuid were not returned")
	}
	mountStart := mountOffset + int(int32(binary.NativeEndian.Uint32(reply[mountOffset:mountOffset+4])))
	mountLength := int(binary.NativeEndian.Uint32(reply[mountOffset+4 : mountOffset+8]))
	if mountStart < fixedBytes || mountLength < 2 || mountStart+mountLength > length || reply[mountStart+mountLength-1] != 0 {
		return "", errors.New("volume mount point reference is malformed")
	}
	if mounted := string(reply[mountStart : mountStart+mountLength-1]); mounted != mountPath {
		return "", fmt.Errorf("path %q is not a volume mount point (volume is mounted at %q)", mountPath, mounted)
	}
	uuid := reply[uuidOffset : uuidOffset+16]
	zero := true
	for _, value := range uuid {
		zero = zero && value == 0
	}
	if zero {
		return "", errors.New("volume uuid is empty")
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", uuid[0:4], uuid[4:6], uuid[6:8], uuid[8:10], uuid[10:16]), nil
}

// Addresses of the libSystem trampolines defined in the package assembly.
var libcFsetxattrTrampolineAddr uintptr
var libcGetattrlistTrampolineAddr uintptr

//go:cgo_import_dynamic libc_fsetxattr fsetxattr "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_getattrlist getattrlist "/usr/lib/libSystem.B.dylib"

// The runtime's libc call path, exported for x/sys by the syscall package.
//
//go:linkname syscallSyscall6 syscall.syscall6
func syscallSyscall6(fn, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, err syscall.Errno)

// Raw dev_t of a descriptor or path stat. Darwin reports a signed 32-bit
// value; widening without sign extension keeps it equal to unix.Mkdev.
func StatDevice(stat *unix.Stat_t) uint64 { return uint64(uint32(stat.Dev)) }

// Raw dev_t of a FileInfo returned by os.Stat, os.Lstat or File.Stat.
func FileInfoDevice(info os.FileInfo) (uint64, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(uint32(stat.Dev)), true
}
