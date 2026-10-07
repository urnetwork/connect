//go:build linux

// Linux supplies no-replace and exchange renames through renameat2 and reports
// an absent extended attribute as ENODATA.
package durablesys

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// Fails with EEXIST when the attribute already exists.
const AttributeCreate = unix.XATTR_CREATE

// Fails with ErrNoAttribute when the attribute is absent.
const AttributeReplace = unix.XATTR_REPLACE

// Linux reports an absent extended attribute as ENODATA.
const ErrNoAttribute = unix.ENODATA

// Atomic rename that fails with EEXIST instead of replacing the target name.
func RenameNoReplace(fromDirectory int, fromName string, toDirectory int, toName string) error {
	return unix.Renameat2(fromDirectory, fromName, toDirectory, toName, unix.RENAME_NOREPLACE)
}

// Atomically swaps two existing names; either missing name fails.
func RenameExchange(fromDirectory int, fromName string, toDirectory int, toName string) error {
	return unix.Renameat2(fromDirectory, fromName, toDirectory, toName, unix.RENAME_EXCHANGE)
}

// Descriptor-relative write. Flags are 0, AttributeCreate or AttributeReplace;
// any other value is refused rather than passed to the kernel.
func SetAttribute(fd int, name string, value []byte, flags int) error {
	if flags != 0 && flags != AttributeCreate && flags != AttributeReplace {
		return syscall.EINVAL
	}
	return unix.Fsetxattr(fd, name, value, flags)
}

// Descriptor-relative read. A short buffer fails with ERANGE.
func GetAttribute(fd int, name string, value []byte) (int, error) {
	return unix.Fgetxattr(fd, name, value)
}

// NUL-separated attribute names. A short buffer fails with ERANGE.
func ListAttributes(fd int, names []byte) (int, error) {
	return unix.Flistxattr(fd, names)
}

// The filesystem or kernel provides no extended attributes at all.
func AttributeUnsupported(err error) bool {
	return errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.ENOSYS)
}

// Raw dev_t of a descriptor or path stat, widened from an unsigned 32/64-bit field.
func StatDevice(stat *unix.Stat_t) uint64 { return uint64(stat.Dev) }

// Raw dev_t of a FileInfo returned by os.Stat, os.Lstat or File.Stat.
func FileInfoDevice(info os.FileInfo) (uint64, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(stat.Dev), true
}
