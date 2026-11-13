//go:build linux || darwin || freebsd

// Each original publication holds an exclusive private descriptor lease.
package connect

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// A distinct namespace cannot be confused with the whole-work cut outbox.
func lockOriginalContractStore(root *os.Root) (*os.File, error) {
	file, err := root.OpenFile(OriginalContractStoreLeaseName, os.O_RDWR|syscall.O_NOFOLLOW, 0600)
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
	if listErr != nil {
		return nil, errors.Join(listErr, file.Close())
	}
	if !ok || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 || !originalWorkOutboxPrivate(info, false, 0600) || info.Size() != 0 || !originalWorkOutboxSameState(info, listed) {
		return nil, errors.Join(originalContractStoreLoss("original contract custody lease is not private", nil), file.Close())
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

// Read a bounded original attribute from the same protected directory inode.
func readOriginalContractStoreAttribute(file *os.File) ([]byte, error) {
	if file == nil {
		return nil, errors.New("original creation directory is closed")
	}
	raw := make([]byte, 4097)
	n, err := unix.Fgetxattr(int(file.Fd()), OriginalContractStoreAttribute, raw)
	if originalWorkOutboxAttributeAbsent(err) {
		return nil, originalContractStoreLoss("original contract requires its explicitly prepared birth", err)
	}
	if err != nil {
		return nil, err
	}
	if n == 0 || n > 4096 {
		return nil, originalContractStoreLoss("original contract birth exceeds its fixed bound", nil)
	}
	return raw[:n], nil
}

// Borrow the actual private empty layout. Only the external accepted fresh plan
// may publish these returned bytes with no-replace original-birth authority.
func BuildFreshOriginalContractStoreCheckpoint(ctx context.Context, root *os.File, scope OriginalContractStoreScope) (_ []byte, resultErr error) {
	if ctx == nil || root == nil {
		return nil, errors.New("original creation birth requires its borrowed root and owner")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	parent, err := root.Stat()
	if err != nil {
		return nil, err
	}
	if !originalWorkOutboxPrivate(parent, true, 0700) {
		return nil, originalContractStoreLoss("original creation birth root is not private", nil)
	}
	fd, err := unix.Openat(int(root.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	directory := os.NewFile(uintptr(fd), ".")
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	entries, err := directory.ReadDir(2)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(entries) != 1 || entries[0].Name() != OriginalContractStoreLeaseName {
		return nil, originalContractStoreLoss("original creation birth requires exactly its empty private lease", nil)
	}
	fd, err = unix.Openat(int(root.Fd()), OriginalContractStoreLeaseName, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	lease := os.NewFile(uintptr(fd), OriginalContractStoreLeaseName)
	defer func() { resultErr = errors.Join(resultErr, lease.Close()) }()
	info, err := lease.Stat()
	if err != nil {
		return nil, err
	}
	if !originalWorkOutboxPrivate(info, false, 0600) || info.Size() != 0 || originalWorkOutboxDevice(info) != originalWorkOutboxDevice(parent) {
		return nil, originalContractStoreLoss("original creation birth lease differs", nil)
	}
	var named unix.Stat_t
	if err := unix.Fstatat(int(root.Fd()), OriginalContractStoreLeaseName, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, err
	}
	if uint64(named.Dev) != originalWorkOutboxDevice(info) || uint64(named.Ino) != originalWorkOutboxInode(info) || named.Mode&unix.S_IFMT != unix.S_IFREG || named.Mode&07777 != 0600 || named.Nlink != 1 || named.Uid != uint32(os.Geteuid()) || named.Size != 0 {
		return nil, originalContractStoreLoss("original creation birth lease changed during inspection", nil)
	}
	after, err := root.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(parent, after) || !originalWorkOutboxPrivate(after, true, 0700) || parent.ModTime() != after.ModTime() || originalWorkOutboxChangeTime(parent) != originalWorkOutboxChangeTime(after) {
		return nil, originalContractStoreLoss("original creation birth root changed during inspection", nil)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return json.Marshal(OriginalContractStoreCheckpoint{Schema: OriginalContractStoreSchema, Scope: scope, DirectoryDevice: originalWorkOutboxDevice(parent), DirectoryInode: originalWorkOutboxInode(parent), LeaseInode: originalWorkOutboxInode(info)})
}
