//go:build linux || darwin || freebsd

// A process-scoped descriptor lease serializes the complete private outbox.
package connect

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// Symlinks, public permissions and multiply linked files cannot own custody.
func lockOriginalWorkOutbox(root *os.Root) (*os.File, error) {
	flags := os.O_RDWR | syscall.O_NOFOLLOW
	file, err := root.OpenFile(originalWorkOutboxIndexName, flags, 0600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	listed, listErr := root.Lstat(".owner.lock")
	if listErr != nil {
		return nil, errors.Join(originalWorkOutboxObservation("cannot observe named outbox lease", listErr), file.Close())
	}
	if !ok || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || !listed.Mode().IsRegular() || !os.SameFile(info, listed) {
		return nil, errors.Join(originalWorkOutboxLoss("whole-work outbox lock is not privately owned", nil), file.Close())
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}

func originalWorkOutboxNoFollow() int { return syscall.O_NOFOLLOW }

func originalWorkOutboxInode(info os.FileInfo) uint64 {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return uint64(stat.Ino)
}

func originalWorkOutboxDevice(info os.FileInfo) uint64 {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return uint64(stat.Dev)
}

func originalWorkOutboxPrivate(info os.FileInfo, directory bool, mode os.FileMode) bool {
	if info == nil || info.Mode().Perm() != mode || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || info.Mode()&os.ModeType != 0 && !directory || directory && !info.IsDir() {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid()) && (directory || stat.Nlink == 1)
}

func originalWorkOutboxSameOwner(first, second os.FileInfo) bool {
	a, aok := first.Sys().(*syscall.Stat_t)
	b, bok := second.Sys().(*syscall.Stat_t)
	return aok && bok && a.Uid == b.Uid && a.Gid == b.Gid
}

func originalWorkOutboxSameState(first, second os.FileInfo) bool {
	return first != nil && second != nil && os.SameFile(first, second) && first.Mode() == second.Mode() && first.Size() == second.Size() && first.ModTime() == second.ModTime() && originalWorkOutboxSameOwner(first, second) && originalWorkOutboxChangeTime(first) == originalWorkOutboxChangeTime(second) && originalWorkOutboxPrivate(first, false, first.Mode().Perm()) && originalWorkOutboxPrivate(second, false, second.Mode().Perm())
}

var errOriginalWorkOutboxAttributeAbsent = errors.New("whole-work outbox birth attribute is absent")

func readOriginalWorkOutboxAttribute(file *os.File) ([]byte, error) {
	raw := make([]byte, 4097)
	n, err := unix.Fgetxattr(int(file.Fd()), OriginalWorkOutboxAttribute, raw)
	if originalWorkOutboxAttributeAbsent(err) {
		return nil, errors.Join(errOriginalWorkOutboxAttributeAbsent, err)
	}
	if err != nil {
		return nil, err
	}
	if n == 0 || n > 4096 {
		return nil, originalWorkOutboxLoss("outbox birth checkpoint exceeds its bound", nil)
	}
	return raw[:n], nil
}
