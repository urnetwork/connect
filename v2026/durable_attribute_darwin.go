//go:build darwin

// Descriptor attribute writes call libc fsetxattr directly: x/sys's Darwin
// Fsetxattr passes zero options, silently dropping XATTR_CREATE/XATTR_REPLACE.
// The durablesys subpackage owns this primitive for durable custody; the root
// package cannot import its own subpackage, so it keeps this private binding.
package connect

import (
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Flags are 0, unix.XATTR_CREATE or unix.XATTR_REPLACE and travel in the
// options argument, with position zero as required for ordinary names.
func setDescriptorAttribute(fd int, name string, value []byte, flags int) error {
	if flags != 0 && flags != unix.XATTR_CREATE && flags != unix.XATTR_REPLACE {
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

// Address of the libSystem trampoline defined in the package assembly.
var libcFsetxattrTrampolineAddr uintptr

//go:cgo_import_dynamic libc_fsetxattr fsetxattr "/usr/lib/libSystem.B.dylib"

// The runtime's libc call path, exported for x/sys by the syscall package.
//
//go:linkname syscallSyscall6 syscall.syscall6
func syscallSyscall6(fn, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, err syscall.Errno)
