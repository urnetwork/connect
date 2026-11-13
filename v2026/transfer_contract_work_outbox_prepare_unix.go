//go:build linux || darwin || freebsd

package connect

// Explicit offline preparation derives an empty birth from borrowed physical
// custody. It creates no files, writes no attributes and grants no runtime key.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// The accepted external fresh plan owns the stopped-writer and zero-history
// authority. This helper verifies only the exact private empty physical layout;
// its caller must publish with no-replace or exact prior-acknowledgement checks.
func BuildFreshOriginalWorkOutboxCheckpoint(ctx context.Context, root *os.File) (_ []byte, resultErr error) {
	if ctx == nil || root == nil {
		return nil, errors.New("outbox birth requires its borrowed root and preparation context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	parent, err := root.Stat()
	if err != nil {
		return nil, err
	}
	if !originalWorkOutboxPrivate(parent, true, 0700) {
		return nil, originalWorkOutboxLoss("outbox birth root is not private", nil)
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
	if len(entries) != 1 || entries[0].Name() != originalWorkOutboxIndexName {
		return nil, originalWorkOutboxLoss("outbox birth requires exactly its empty original index", nil)
	}
	fd, err = unix.Openat(int(root.Fd()), originalWorkOutboxIndexName, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	index := os.NewFile(uintptr(fd), originalWorkOutboxIndexName)
	defer func() { resultErr = errors.Join(resultErr, index.Close()) }()
	info, err := index.Stat()
	if err != nil {
		return nil, err
	}
	if !originalWorkOutboxPrivate(info, false, 0600) || originalWorkOutboxDevice(info) != originalWorkOutboxDevice(parent) || info.Size() != 0 {
		return nil, originalWorkOutboxLoss("outbox birth index is not an empty private original", nil)
	}
	var named unix.Stat_t
	if err := unix.Fstatat(int(root.Fd()), originalWorkOutboxIndexName, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, err
	}
	if uint64(named.Dev) != originalWorkOutboxDevice(info) || uint64(named.Ino) != originalWorkOutboxInode(info) || named.Mode&unix.S_IFMT != unix.S_IFREG || named.Mode&07777 != 0600 || named.Nlink != 1 || named.Uid != uint32(os.Geteuid()) || named.Size != 0 {
		return nil, originalWorkOutboxLoss("outbox birth index changed during observation", nil)
	}
	after, err := root.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(parent, after) || !originalWorkOutboxPrivate(after, true, 0700) || parent.ModTime() != after.ModTime() || originalWorkOutboxChangeTime(parent) != originalWorkOutboxChangeTime(after) {
		return nil, originalWorkOutboxLoss("outbox birth root changed during inspection", nil)
	}
	raw, err := json.Marshal(OriginalWorkOutboxCheckpoint{Schema: OriginalWorkOutboxSchema, DirectoryDevice: originalWorkOutboxDevice(parent), DirectoryInode: originalWorkOutboxInode(parent), IndexInode: originalWorkOutboxInode(info), IndexSha256: originalWorkOutboxDigest(nil)})
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return raw, nil
}
