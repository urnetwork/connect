//go:build freebsd

package connect

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func originalWorkOutboxChangeTime(info os.FileInfo) syscall.Timespec {
	return info.Sys().(*syscall.Stat_t).Ctimespec
}

func originalWorkOutboxAttributeAbsent(err error) bool { return errors.Is(err, unix.ENOATTR) }

// FreeBSD lacks create/replace xattr flags; the retained exclusive inode lease
// owns this observation and update, including the explicit absent-birth check.
func replaceOriginalWorkOutboxAttribute(file *os.File, raw []byte, fresh bool) error {
	_, err := readOriginalWorkOutboxAttribute(file)
	if fresh && !errors.Is(err, errOriginalWorkOutboxAttributeAbsent) {
		return errors.Join(originalWorkOutboxLoss("outbox birth already exists", nil), err)
	}
	if !fresh && err != nil {
		return err
	}
	return unix.Fsetxattr(int(file.Fd()), OriginalWorkOutboxAttribute, raw, 0)
}
