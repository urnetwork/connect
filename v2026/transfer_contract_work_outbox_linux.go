//go:build linux

package connect

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func originalWorkOutboxChangeTime(info os.FileInfo) syscall.Timespec {
	return info.Sys().(*syscall.Stat_t).Ctim
}

func originalWorkOutboxAttributeAbsent(err error) bool { return errors.Is(err, unix.ENODATA) }

func replaceOriginalWorkOutboxAttribute(file *os.File, raw []byte, fresh bool) error {
	flags := unix.XATTR_REPLACE
	if fresh {
		flags = unix.XATTR_CREATE
	}
	return unix.Fsetxattr(int(file.Fd()), OriginalWorkOutboxAttribute, raw, flags)
}
