//go:build linux || darwin || freebsd

// Each original publication holds an exclusive private descriptor lease.
package connect

import (
	"errors"
	"os"
	"syscall"
)

// A distinct namespace cannot be confused with the whole-work cut outbox.
func lockOriginalContractStore(root *os.Root) (*os.File, error) {
	file, err := root.OpenFile(".creation.lock", os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	listed, listErr := root.Lstat(".creation.lock")
	if !ok || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || listErr != nil || !listed.Mode().IsRegular() || !os.SameFile(info, listed) {
		file.Close()
		return nil, errors.New("original contract custody lease is not private")
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}
