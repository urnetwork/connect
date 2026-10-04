//go:build linux || darwin || freebsd

// A process-scoped descriptor lease serializes the complete private outbox.
package connect

import (
	"errors"
	"os"
	"syscall"
)

// Symlinks, public permissions and multiply linked files cannot own custody.
func lockOriginalWorkOutbox(root *os.Root) (*os.File, error) {
	file, err := root.OpenFile(".owner.lock", os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	listed, listErr := root.Lstat(".owner.lock")
	if !ok || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || listErr != nil || !listed.Mode().IsRegular() || !os.SameFile(info, listed) {
		file.Close()
		return nil, errors.New("whole-work outbox lock is not privately owned")
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}
