package connect

// Required provider launch validates the actual prepared original outbox before
// constructing SDK workers. Runtime subsequently owns and fences its live lease.

import (
	"bytes"
	"context"
	"errors"
	"time"
)

// This owns and closes a temporary exclusive reader. It neither creates birth
// nor reconciles pending publication; every retained signature uses the supplied
// independent launch scope. A failure grants no construction or restart approval.
func ValidateOriginalWorkOutbox(ctx context.Context, directory string, scope OriginalWorkOutboxScope) (resultErr error) {
	if ctx == nil {
		return errors.New("outbox admission requires its caller context")
	}
	owner, cancel := context.WithTimeout(ctx, 300*time.Second)
	defer cancel()
	outbox, err := openOriginalWorkOutboxOwned(owner, directory, false)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, outbox.close()) }()
	root, err := outbox.directory.Stat()
	if err != nil {
		return err
	}
	index, err := outbox.lock.Stat()
	if err != nil {
		return err
	}
	files := []OriginalWorkOutboxFile{
		{Name: "", Device: originalWorkOutboxDevice(root), Inode: originalWorkOutboxInode(root), Mode: 0700},
		{Name: OriginalWorkOutboxIndexName, Device: originalWorkOutboxDevice(index), Inode: originalWorkOutboxInode(index), Mode: 0600, Bytes: uint64(len(outbox.index)), Sha256: originalWorkOutboxDigest(outbox.index)},
	}
	memberKVs := map[string]OriginalWorkOutboxMember{}
	for name, member := range outbox.members {
		memberKVs[name] = member
	}
	if pending := outbox.checkpoint.Pending; pending != nil {
		memberKVs[pending.Name] = *pending
	}
	for _, member := range memberKVs {
		files = append(files, OriginalWorkOutboxFile{Name: member.Name, Device: outbox.checkpoint.DirectoryDevice, Inode: member.Inode, Mode: 0400, Bytes: member.Bytes, Sha256: member.Sha256})
	}
	read := func(ctx context.Context, name string) ([]byte, error) { return outbox.readMember(ctx, memberKVs[name]) }
	rebound, err := RebindOriginalWorkOutboxInventory(owner, scope, outbox.raw, outbox.index, files, files, read)
	if err != nil {
		return err
	}
	if !bytes.Equal(rebound.Checkpoint, outbox.raw) || !bytes.Equal(rebound.Index, outbox.index) {
		return originalWorkOutboxLoss("outbox admission changed original inventory ancestry", nil)
	}
	if _, err := outbox.census(owner, outbox.checkpoint.Pending); err != nil {
		return err
	}
	return outbox.check(owner)
}
