// Individual request and admission leaves are create-once durable originals.
// Their presence never certifies a complete population or an empty extender set.
package connect

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/urnetwork/connect/protocol"
)

const maximumOriginalContractLeaves = 2 * protocol.MaximumOriginalWorkContracts
const maximumOriginalContractStoreBytes = 128 * 1024 * 1024

// The retained parent descriptors detect renamed or replaced path components.
type originalContractStoreAncestor struct {
	root *os.Root
	name string
	info os.FileInfo
}

// The short-lived exclusive lease owns one complete bounded publication.
type originalContractStore struct {
	root      *os.Root
	lock      *os.File
	ancestors []originalContractStoreAncestor
}

// Create physical private custody without traversing symlinks. A separate lock
// name and leaf grammar prevent accidentally opening the complete-cut outbox.
func openOriginalContractStore(ctx context.Context, directory string) (_ *originalContractStore, resultErr error) {
	if ctx == nil {
		return nil, errors.New("original contract custody requires an owner")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory || directory == string(filepath.Separator) {
		return nil, errors.New("original contract custody requires a canonical directory")
	}
	self := &originalContractStore{}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, self.close())
		}
	}()
	parent, err := os.OpenRoot(string(filepath.Separator))
	if err != nil {
		return nil, err
	}
	self.ancestors = append(self.ancestors, originalContractStoreAncestor{root: parent})
	for _, name := range strings.Split(strings.TrimPrefix(directory, string(filepath.Separator)), string(filepath.Separator)) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		info, err := parent.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			if err := parent.Mkdir(name, 0700); err != nil {
				return nil, err
			}
			file, err := parent.Open(".")
			if err != nil {
				return nil, err
			}
			if err := errors.Join(file.Sync(), file.Close()); err != nil {
				return nil, err
			}
			info, err = parent.Lstat(name)
		}
		if err != nil || !info.IsDir() {
			return nil, errors.Join(errors.New("original contract ancestor is not physical"), err)
		}
		next, err := parent.OpenRoot(name)
		if err != nil {
			return nil, err
		}
		self.ancestors = append(self.ancestors, originalContractStoreAncestor{root: next, name: name, info: info})
		opened, err := next.Stat(".")
		if err != nil || !os.SameFile(info, opened) {
			return nil, errors.Join(errors.New("original contract ancestor changed"), err)
		}
		parent = next
	}
	self.root = parent
	info, err := self.root.Stat(".")
	if err != nil || info.Mode().Perm() != 0700 {
		return nil, errors.Join(errors.New("original contract directory is not private"), err)
	}
	if _, _, err := self.capacity(ctx); err != nil {
		return nil, err
	}
	self.lock, err = lockOriginalContractStore(self.root)
	if err != nil {
		return nil, err
	}
	return self, nil
}

// Release the lease only after every retained operation has completed.
func (self *originalContractStore) close() error {
	var result error
	if self.lock != nil {
		result = self.lock.Close()
	}
	for index := len(self.ancestors) - 1; index >= 0; index-- {
		result = errors.Join(result, self.ancestors[index].root.Close())
	}
	return result
}

// A file cannot be written into a detached old path and then reported present.
func (self *originalContractStore) checkPath(ctx context.Context) error {
	for index := 1; index < len(self.ancestors); index++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		ancestor := self.ancestors[index]
		listed, err := self.ancestors[index-1].root.Lstat(ancestor.name)
		if err != nil || !listed.IsDir() || !os.SameFile(listed, ancestor.info) {
			return errors.Join(errors.New("original contract custody path changed"), err)
		}
	}
	if self.lock != nil {
		opened, err := self.lock.Stat()
		if err != nil {
			return err
		}
		listed, err := self.root.Lstat(".creation.lock")
		if err != nil || !listed.Mode().IsRegular() || listed.Mode().Perm() != 0600 || !os.SameFile(opened, listed) {
			return errors.Join(errors.New("original contract custody lease changed"), err)
		}
	}
	return nil
}

// Validate the narrow immutable namespace and enforce finite disk custody.
func (self *originalContractStore) capacity(ctx context.Context) (int, int64, error) {
	if err := self.checkPath(ctx); err != nil {
		return 0, 0, err
	}
	file, err := self.root.Open(".")
	if err != nil {
		return 0, 0, err
	}
	entries, readErr := file.ReadDir(maximumOriginalContractLeaves + 2)
	closeErr := file.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return 0, 0, errors.Join(readErr, closeErr)
	}
	if closeErr != nil {
		return 0, 0, closeErr
	}
	count := 0
	var total int64
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return 0, 0, err
		}
		if entry.Name() == ".creation.lock" {
			continue
		}
		name := strings.TrimPrefix(strings.TrimPrefix(entry.Name(), "request-"), "admission-")
		digest, err := hex.DecodeString(strings.TrimSuffix(name, ".json"))
		if name == entry.Name() || len(digest) != sha256.Size || err != nil || hex.EncodeToString(digest)+".json" != name {
			return 0, 0, errors.New("original contract custody contains an unknown leaf")
		}
		info, err := self.root.Lstat(entry.Name())
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0400 || info.Size() <= 0 || info.Size() > protocol.MaximumOriginalContractAdmissionBytes {
			return 0, 0, errors.Join(errors.New("original contract custody contains a partial leaf"), err)
		}
		count++
		total += info.Size()
		if count > maximumOriginalContractLeaves || total > maximumOriginalContractStoreBytes {
			return 0, 0, errors.New("original contract custody exceeds capacity")
		}
	}
	return count, total, nil
}

// Reopen only protected exact bytes while retaining physical path custody.
func (self *originalContractStore) read(ctx context.Context, name string) ([]byte, error) {
	if err := self.checkPath(ctx); err != nil {
		return nil, err
	}
	info, err := self.root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0400 || info.Size() <= 0 || info.Size() > protocol.MaximumOriginalContractAdmissionBytes {
		return nil, errors.New("original contract retained leaf is not immutable")
	}
	file, err := self.root.Open(name)
	if err != nil {
		return nil, err
	}
	opened, statErr := file.Stat()
	if statErr != nil || !os.SameFile(info, opened) {
		file.Close()
		return nil, errors.Join(errors.New("original contract retained leaf changed"), statErr)
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, protocol.MaximumOriginalContractAdmissionBytes+1))
	if err := errors.Join(readErr, file.Close(), ctx.Err()); err != nil {
		return nil, err
	}
	if int64(len(raw)) != info.Size() {
		return nil, errors.New("original contract retained size changed")
	}
	return raw, self.checkPath(ctx)
}

// Sync content, immutable metadata and parent before transport/publication.
// Failed partial leaves remain visible and are never removed or overwritten.
func (self *originalContractStore) retain(ctx context.Context, name string, raw []byte) error {
	count, total, err := self.capacity(ctx)
	if err != nil {
		return err
	}
	if previous, err := self.read(ctx, name); err == nil {
		if !bytes.Equal(previous, raw) {
			return errors.New("original contract retained bytes conflict")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if count >= maximumOriginalContractLeaves || len(raw) == 0 || len(raw) > protocol.MaximumOriginalContractAdmissionBytes || int64(len(raw)) > maximumOriginalContractStoreBytes-total {
		return errors.New("original contract custody has no remaining capacity")
	}
	file, err := self.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	n, writeErr := file.Write(raw)
	if writeErr == nil && n != len(raw) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = file.Chmod(0400)
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return err
	}
	parent, err := self.root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(parent.Sync(), parent.Close(), self.checkPath(ctx))
}
