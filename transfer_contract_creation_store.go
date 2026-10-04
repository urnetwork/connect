// Individual request and admission leaves are create-once durable originals.
// Their presence never certifies a complete population or an empty extender set.
package connect

import (
	"bytes"
	"context"
	"encoding/json"
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
	root       *os.Root
	directory  *os.File
	lock       *os.File
	lockInfo   os.FileInfo
	ancestors  []originalContractStoreAncestor
	checkpoint OriginalContractStoreCheckpoint
	raw        []byte
	failure    error
	step       func(string, string) error
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
		if err != nil {
			return nil, originalContractStoreObservation("original contract ancestor is absent", err)
		}
		if !info.IsDir() {
			return nil, originalContractStoreLoss("original contract ancestor is not physical", nil)
		}
		next, err := parent.OpenRoot(name)
		if err != nil {
			return nil, err
		}
		self.ancestors = append(self.ancestors, originalContractStoreAncestor{root: next, name: name, info: info})
		opened, err := next.Stat(".")
		if err != nil {
			return nil, err
		}
		if !os.SameFile(info, opened) || info.Mode() != opened.Mode() || !originalWorkOutboxSameOwner(info, opened) {
			return nil, originalContractStoreLoss("original contract ancestor changed", nil)
		}
		parent = next
	}
	self.root = parent
	self.directory, err = self.root.Open(".")
	if err != nil {
		return nil, err
	}
	info, err := self.directory.Stat()
	if err != nil {
		return nil, err
	}
	if !originalWorkOutboxPrivate(info, true, 0700) {
		return nil, originalContractStoreLoss("original contract directory is not private", nil)
	}
	self.raw, err = readOriginalContractStoreAttribute(self.directory)
	if err != nil {
		return nil, err
	}
	var original OriginalContractStoreCheckpoint
	if err := json.Unmarshal(self.raw, &original); err != nil {
		return nil, originalContractStoreLoss("original contract birth cannot decode", err)
	}
	self.checkpoint, err = DecodeOriginalContractStoreCheckpoint(self.raw, original.Scope)
	if err != nil {
		return nil, err
	}
	self.lock, err = lockOriginalContractStore(self.root)
	if err != nil {
		return nil, originalContractStoreObservation("original contract lease is absent", err)
	}
	self.lockInfo, err = self.lock.Stat()
	if err != nil {
		return nil, err
	}
	if self.checkpoint.DirectoryDevice != originalWorkOutboxDevice(info) || self.checkpoint.DirectoryInode != originalWorkOutboxInode(info) || self.checkpoint.LeaseInode != originalWorkOutboxInode(self.lockInfo) || originalWorkOutboxDevice(self.lockInfo) != self.checkpoint.DirectoryDevice {
		return nil, originalContractStoreLoss("original contract root or lease differs from birth", nil)
	}
	if _, _, err := self.capacity(ctx); err != nil {
		return nil, err
	}
	return self, nil
}

// Release the lease only after every retained operation has completed.
func (self *originalContractStore) close() error {
	var result error
	if self.lock != nil {
		result = self.lock.Close()
		self.lock = nil
	}
	if self.directory != nil {
		result = errors.Join(result, self.directory.Close())
		self.directory = nil
	}
	for index := len(self.ancestors) - 1; index >= 0; index-- {
		result = errors.Join(result, self.ancestors[index].root.Close())
	}
	self.ancestors = nil
	return result
}

// A file cannot be written into a detached old path and then reported present.
func (self *originalContractStore) checkPath(ctx context.Context) (resultErr error) {
	if self.failure != nil {
		return self.failure
	}
	defer func() {
		if errors.Is(resultErr, ErrOriginalContractStoreIdentity) {
			self.failure = resultErr
		}
	}()
	if err := self.boundary(ctx, "path-observation", ""); err != nil {
		return err
	}
	for index := 1; index < len(self.ancestors); index++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		ancestor := self.ancestors[index]
		listed, err := self.ancestors[index-1].root.Lstat(ancestor.name)
		if err != nil {
			return originalContractStoreObservation("original contract ancestor disappeared", err)
		}
		if !listed.IsDir() || !os.SameFile(listed, ancestor.info) || listed.Mode() != ancestor.info.Mode() || !originalWorkOutboxSameOwner(listed, ancestor.info) {
			return originalContractStoreLoss("original contract custody path changed", nil)
		}
	}
	if self.lock != nil {
		opened, err := self.lock.Stat()
		if err != nil {
			return err
		}
		listed, err := self.root.Lstat(".creation.lock")
		if err != nil {
			return originalContractStoreObservation("original contract lease disappeared", err)
		}
		if !originalWorkOutboxPrivate(opened, false, 0600) || opened.Size() != 0 || !originalWorkOutboxSameState(opened, listed) || !os.SameFile(opened, self.lockInfo) {
			return originalContractStoreLoss("original contract custody lease changed", nil)
		}
	}
	raw, err := readOriginalContractStoreAttribute(self.directory)
	if err != nil {
		return err
	}
	if !bytes.Equal(raw, self.raw) {
		return originalContractStoreLoss("original contract birth changed", nil)
	}
	return ctx.Err()
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
		if entry.Name() == OriginalContractStoreLeaseName {
			continue
		}
		if !originalContractStoreName(entry.Name()) {
			return 0, 0, errors.New("original contract custody contains an unknown leaf")
		}
		info, err := self.root.Lstat(entry.Name())
		if err != nil {
			return 0, 0, originalContractStoreObservation("original contract census member disappeared", err)
		}
		if !originalWorkOutboxPrivate(info, false, 0400) || originalWorkOutboxDevice(info) != self.checkpoint.DirectoryDevice || info.Size() <= 0 || info.Size() > protocol.MaximumOriginalContractAdmissionBytes {
			return 0, 0, originalContractStoreLoss("original contract custody contains a partial or aliased leaf", nil)
		}
		count++
		total += info.Size()
		if count > maximumOriginalContractLeaves || total > maximumOriginalContractStoreBytes {
			return 0, 0, errors.New("original contract custody exceeds capacity")
		}
	}
	return count, total, self.checkPath(ctx)
}

// Reopen only protected exact bytes while retaining physical path custody.
func (self *originalContractStore) read(ctx context.Context, name string) (raw []byte, resultErr error) {
	defer func() {
		if resultErr != nil {
			raw = nil
		}
		if errors.Is(resultErr, ErrOriginalContractStoreIdentity) {
			self.failure = resultErr
		}
	}()
	if !originalContractStoreName(name) {
		return nil, errors.New("original contract leaf name is invalid")
	}
	if err := self.checkPath(ctx); err != nil {
		return nil, err
	}
	info, err := self.root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !originalWorkOutboxPrivate(info, false, 0400) || originalWorkOutboxDevice(info) != self.checkpoint.DirectoryDevice || info.Size() <= 0 || info.Size() > protocol.MaximumOriginalContractAdmissionBytes {
		return nil, originalContractStoreLoss("original contract retained leaf is not immutable", nil)
	}
	file, err := self.root.OpenFile(name, os.O_RDONLY|originalWorkOutboxNoFollow(), 0)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !originalWorkOutboxSameState(info, opened) {
		return nil, originalContractStoreLoss("original contract retained leaf changed during open", nil)
	}
	if err := self.boundary(ctx, "original-read", name); err != nil {
		return nil, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, protocol.MaximumOriginalContractAdmissionBytes+1))
	if err := errors.Join(readErr, ctx.Err()); err != nil {
		return nil, err
	}
	if err := self.boundary(ctx, "original-readback", name); err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil {
		return nil, err
	}
	named, err := self.root.Lstat(name)
	if err != nil {
		return nil, originalContractStoreObservation("original contract retained name disappeared", err)
	}
	if !originalWorkOutboxSameState(info, after) || !originalWorkOutboxSameState(after, named) || int64(len(raw)) != info.Size() {
		return nil, originalContractStoreLoss("original contract retained inode changed during read", nil)
	}
	kind := "request"
	if strings.HasPrefix(name, "admission-") {
		kind = "admission"
	}
	expected, err := originalContractLeafName(kind, raw)
	if err != nil || expected != name {
		return nil, originalContractStoreLoss("original contract retained content digest differs", err)
	}
	return raw, self.checkPath(ctx)
}

// Sync content, immutable metadata and parent before transport/publication.
// Failed partial leaves remain visible and are never removed or overwritten.
func (self *originalContractStore) retain(ctx context.Context, name string, raw []byte) (resultErr error) {
	if err := self.admit(ctx, name, raw); err != nil {
		return err
	}
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
	file, err := self.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|originalWorkOutboxNoFollow(), 0600)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, file.Close())
		if resultErr != nil {
			self.failure = resultErr
		}
	}()
	before, err := file.Stat()
	if err != nil {
		return err
	}
	if !originalWorkOutboxPrivate(before, false, 0600) || originalWorkOutboxDevice(before) != self.checkpoint.DirectoryDevice {
		return originalContractStoreLoss("new original contract inode is unprotected", nil)
	}
	if err := self.boundary(ctx, "original-write", name); err != nil {
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
	if writeErr != nil {
		return writeErr
	}
	if err := self.directory.Sync(); err != nil {
		return err
	}
	if err := self.boundary(ctx, "original-synced", name); err != nil {
		return err
	}
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	named, err := self.root.Lstat(name)
	if err != nil {
		return originalContractStoreObservation("new original contract name disappeared", err)
	}
	if !os.SameFile(before, opened) || !originalWorkOutboxPrivate(opened, false, 0400) || !originalWorkOutboxSameState(opened, named) || opened.Size() != int64(len(raw)) {
		return originalContractStoreLoss("new original contract inode changed before acknowledgement", nil)
	}
	observed, err := self.read(ctx, name)
	if err != nil {
		return err
	}
	if !bytes.Equal(raw, observed) {
		return originalContractStoreLoss("new original contract final readback differs", nil)
	}
	return self.checkPath(ctx)
}

// Observation barriers preserve the caller's context and exact syscall cause.
func (self *originalContractStore) boundary(ctx context.Context, stage, name string) error {
	if ctx == nil {
		return errors.New("original contract custody context absent")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if self.step != nil {
		if err := self.step(stage, name); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// Portable admission borrows the complete protected physical namespace.
func (self *originalContractStore) inventory(ctx context.Context) (_ []OriginalContractStoreFile, resultErr error) {
	if _, _, err := self.capacity(ctx); err != nil {
		return nil, err
	}
	root, err := self.directory.Stat()
	if err != nil {
		return nil, err
	}
	file, err := self.root.Open(".")
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	entries, err := file.ReadDir(maximumOriginalContractLeaves + 2)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	files := []OriginalContractStoreFile{{Name: "", Device: originalWorkOutboxDevice(root), Inode: originalWorkOutboxInode(root), Mode: 0700}}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		info, err := self.root.Lstat(entry.Name())
		if err != nil {
			return nil, originalContractStoreObservation("original creation census member disappeared", err)
		}
		mode, digest := uint32(0400), "sha256:"+strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(entry.Name(), "request-"), "admission-"), ".json")
		if entry.Name() == OriginalContractStoreLeaseName {
			mode, digest = 0600, originalWorkOutboxDigest(nil)
		}
		if !originalWorkOutboxPrivate(info, false, os.FileMode(mode)) || info.Size() < 0 {
			return nil, originalContractStoreLoss("original creation census protection changed", nil)
		}
		files = append(files, OriginalContractStoreFile{Name: entry.Name(), Device: originalWorkOutboxDevice(info), Inode: originalWorkOutboxInode(info), Mode: mode, Bytes: uint64(info.Size()), Sha256: digest})
	}
	if _, err := originalContractStoreFiles(files); err != nil {
		return nil, err
	}
	return files, self.checkPath(ctx)
}

// Original signed identity and pre-send closure precede any candidate write.
func (self *originalContractStore) admit(ctx context.Context, name string, raw []byte) error {
	if !originalContractStoreName(name) {
		return errors.New("original contract leaf name is invalid")
	}
	var request protocol.OriginalContractRequest
	var err error
	kind := "request"
	if strings.HasPrefix(name, "admission-") {
		kind = "admission"
		admission, decodeErr := protocol.DecodeOriginalContractAdmission(ctx, raw)
		if decodeErr != nil {
			return decodeErr
		}
		request, err = protocol.DecodeOriginalContractRequest(ctx, admission.Request)
		if err != nil {
			return err
		}
		requestName, _ := originalContractLeafName("request", admission.Request)
		previous, err := self.read(ctx, requestName)
		if err != nil {
			return originalContractStoreObservation("original admission lost pre-send request custody", err)
		}
		if !bytes.Equal(previous, admission.Request) {
			return originalContractStoreLoss("original admission pre-send bytes differ", nil)
		}
	} else {
		request, err = protocol.DecodeOriginalContractRequest(ctx, raw)
		if err != nil {
			return err
		}
	}
	expected, err := originalContractLeafName(kind, raw)
	scope := self.checkpoint.Scope
	if err != nil || expected != name || request.DomainHash != scope.DomainHash || request.ClientId != scope.ClientId || request.PublicKey != scope.PublicKey {
		return originalContractStoreLoss("original contract differs from its approved namespace", err)
	}
	return ctx.Err()
}
