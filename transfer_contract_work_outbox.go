package connect

// The root checkpoint authenticates birth and the complete append-only index.
// A disappearing acknowledged leaf is never indistinguishable from new work.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/urnetwork/connect/protocol"
)

// OriginalWorkOutboxAttribute is the bounded root-owned custody checkpoint.
// Export/restore must retain it together with .owner.lock and every named leaf.
const OriginalWorkOutboxAttribute = "user.urnetwork.sdk-work.v1"
const OriginalWorkOutboxSchema = "urnetwork-sdk-whole-work-outbox-v1"
const OriginalWorkOutboxIndexName = ".owner.lock"
const MaximumOriginalWorkOutboxRecords = maximumOriginalWorkOutboxRecords
const MaximumOriginalWorkOutboxBytes = maximumOriginalWorkOutboxBytes
const MaximumOriginalWorkOutboxIndexBytes = maximumOriginalWorkOutboxIndexBytes
const originalWorkOutboxIndexName = OriginalWorkOutboxIndexName
const maximumOriginalWorkOutboxIndexBytes = maximumOriginalWorkOutboxRecords * 512

var ErrOriginalWorkOutboxIdentity = errors.New("whole-work outbox custody changed")
var ErrOriginalWorkOutboxUncertain = errors.New("whole-work outbox publication requires joined reconciliation")
var errOriginalWorkOutboxUncaptured = errors.New("whole-work boundary has not been captured")

// OriginalWorkOutboxMember binds both an immutable name and its physical inode.
// Restore may translate physical coordinates, preserving names, bytes and hashes.
type OriginalWorkOutboxMember struct {
	Name   string `json:"name"`
	Inode  uint64 `json:"inode"`
	Bytes  uint64 `json:"bytes"`
	Sha256 string `json:"sha256"`
}

// OriginalWorkOutboxCheckpoint is a new schema, never inferred from old leaves.
// IndexSha256 covers exact canonical JSON lines, including each final newline.
type OriginalWorkOutboxCheckpoint struct {
	DirectoryDevice uint64                    `json:"directory_device"`
	Schema          string                    `json:"schema"`
	DirectoryInode  uint64                    `json:"directory_inode"`
	IndexInode      uint64                    `json:"index_inode"`
	IndexBytes      uint64                    `json:"index_bytes"`
	IndexSha256     string                    `json:"index_sha256"`
	Records         uint64                    `json:"records"`
	OriginalBytes   uint64                    `json:"original_bytes"`
	Pending         *OriginalWorkOutboxMember `json:"pending,omitempty"`
}

type originalWorkOutboxAncestor struct {
	root *os.Root
	name string
	info os.FileInfo
}

// The lifecycle joins the only mutator before releasing these descriptors.
// Hooks expose real I/O boundaries and are nil outside deterministic tests.
type originalWorkOutbox struct {
	root       *os.Root
	directory  *os.File
	lock       *os.File
	ancestors  []originalWorkOutboxAncestor
	checkpoint OriginalWorkOutboxCheckpoint
	raw        []byte
	index      []byte
	members    map[string]OriginalWorkOutboxMember
	lockInfo   os.FileInfo
	failure    error
	step       func(string, string) error
	readFile   func(*os.File, []byte) (int, error)
}

func originalWorkOutboxDigest(raw []byte) string {
	hash := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(hash[:])
}

func originalWorkOutboxLoss(detail string, cause error) error {
	return errors.Join(ErrOriginalWorkOutboxIdentity, errors.New(detail), cause)
}

// Failed observations retain their cause. Only a missing retained object proves
// loss; callers must never compare the zero value returned by another failure.
func originalWorkOutboxObservation(detail string, cause error) error {
	if errors.Is(cause, os.ErrNotExist) {
		return originalWorkOutboxLoss(detail, cause)
	}
	return fmt.Errorf("%s: %w", detail, cause)
}

func originalWorkOutboxCanonicalName(name string) bool {
	if len(name) != 69 || !strings.HasSuffix(name, ".json") {
		return false
	}
	raw, err := hex.DecodeString(name[:64])
	return err == nil && hex.EncodeToString(raw)+".json" == name
}

func originalWorkOutboxValidMember(member OriginalWorkOutboxMember) bool {
	if !originalWorkOutboxCanonicalName(member.Name) || member.Inode == 0 || member.Bytes == 0 || member.Bytes > protocol.MaximumOriginalWorkSubmissionBytes {
		return false
	}
	raw, err := hex.DecodeString(strings.TrimPrefix(member.Sha256, "sha256:"))
	return err == nil && len(raw) == sha256.Size && "sha256:"+hex.EncodeToString(raw) == member.Sha256
}

// DecodeOriginalWorkOutboxInventory validates original grammar without
// normalizing it. Preparation and restore share this exact logical admission.
func DecodeOriginalWorkOutboxInventory(checkpointRaw, indexRaw []byte) (OriginalWorkOutboxCheckpoint, []OriginalWorkOutboxMember, error) {
	var checkpoint OriginalWorkOutboxCheckpoint
	if len(checkpointRaw) == 0 || len(checkpointRaw) > 4096 || len(indexRaw) > maximumOriginalWorkOutboxIndexBytes {
		return checkpoint, nil, originalWorkOutboxLoss("outbox custody exceeds its bound", nil)
	}
	if err := json.Unmarshal(checkpointRaw, &checkpoint); err != nil {
		return checkpoint, nil, originalWorkOutboxLoss("outbox checkpoint cannot decode", err)
	}
	canonical, err := json.Marshal(checkpoint)
	if err != nil || !bytes.Equal(canonical, checkpointRaw) || checkpoint.Schema != OriginalWorkOutboxSchema || checkpoint.DirectoryInode == 0 || checkpoint.IndexInode == 0 || checkpoint.IndexBytes > uint64(len(indexRaw)) || checkpoint.Records > maximumOriginalWorkOutboxRecords || checkpoint.OriginalBytes > maximumOriginalWorkOutboxBytes {
		return checkpoint, nil, originalWorkOutboxLoss("outbox checkpoint is not original canonical custody", err)
	}
	committed := indexRaw[:checkpoint.IndexBytes]
	if originalWorkOutboxDigest(committed) != checkpoint.IndexSha256 {
		return checkpoint, nil, originalWorkOutboxLoss("outbox committed inventory was lost or changed", nil)
	}
	var members []OriginalWorkOutboxMember
	memberKVs := map[string]bool{}
	var total uint64
	for len(committed) != 0 {
		end := bytes.IndexByte(committed, '\n')
		if end < 0 || end > 511 || len(members) >= maximumOriginalWorkOutboxRecords {
			return checkpoint, nil, originalWorkOutboxLoss("outbox inventory has an incomplete or oversized record", nil)
		}
		line := committed[:end]
		var member OriginalWorkOutboxMember
		if err := json.Unmarshal(line, &member); err != nil {
			return checkpoint, nil, originalWorkOutboxLoss("outbox inventory cannot decode", err)
		}
		canonical, err := json.Marshal(member)
		if err != nil || !bytes.Equal(line, canonical) || !originalWorkOutboxValidMember(member) || memberKVs[member.Name] || member.Bytes > maximumOriginalWorkOutboxBytes-total {
			return checkpoint, nil, originalWorkOutboxLoss("outbox inventory is not original complete custody", err)
		}
		memberKVs[member.Name] = true
		members = append(members, member)
		total += member.Bytes
		committed = committed[end+1:]
	}
	if uint64(len(members)) != checkpoint.Records || total != checkpoint.OriginalBytes {
		return checkpoint, nil, originalWorkOutboxLoss("outbox inventory totals differ", nil)
	}
	tail := indexRaw[checkpoint.IndexBytes:]
	if pending := checkpoint.Pending; pending != nil {
		if !originalWorkOutboxValidMember(*pending) || memberKVs[pending.Name] || checkpoint.Records >= maximumOriginalWorkOutboxRecords || pending.Bytes > maximumOriginalWorkOutboxBytes-total {
			return checkpoint, nil, originalWorkOutboxLoss("outbox pending original does not extend its inventory", nil)
		}
		line, err := json.Marshal(pending)
		if err != nil || len(tail) != 0 && !bytes.Equal(tail, append(line, '\n')) {
			return checkpoint, nil, originalWorkOutboxLoss("outbox pending inventory is partial or conflicting", err)
		}
	} else if len(tail) != 0 {
		return checkpoint, nil, originalWorkOutboxLoss("outbox has inventory outside its acknowledged checkpoint", nil)
	}
	return checkpoint, members, nil
}

// Every path component is opened relative to its retained parent. The root
// lease is acquired before interpreting either birth or retained original bytes.
func openOriginalWorkOutbox(directory string) (*originalWorkOutbox, error) {
	return openOriginalWorkOutboxContext(context.Background(), directory)
}

// Startup uses the same bounded owner as its first polling cycle.
func openOriginalWorkOutboxContext(ctx context.Context, directory string) (_ *originalWorkOutbox, resultErr error) {
	return openOriginalWorkOutboxOwned(ctx, directory, true)
}

// Read-only admission borrows the same exclusive physical owner but leaves an
// exact complete pending member for the subsequent runtime owner to reconcile.
func openOriginalWorkOutboxOwned(ctx context.Context, directory string, reconcile bool) (_ *originalWorkOutbox, resultErr error) {
	if ctx == nil {
		return nil, errors.New("whole-work outbox requires its caller context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory || directory == string(filepath.Separator) {
		return nil, errors.New("whole-work outbox requires a canonical absolute directory")
	}
	self := &originalWorkOutbox{members: map[string]OriginalWorkOutboxMember{}}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, self.close())
		}
	}()
	root, err := os.OpenRoot(string(filepath.Separator))
	if err != nil {
		return nil, err
	}
	self.ancestors = append(self.ancestors, originalWorkOutboxAncestor{root: root})
	components := strings.Split(strings.TrimPrefix(directory, string(filepath.Separator)), string(filepath.Separator))
	for _, name := range components {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		parent := self.ancestors[len(self.ancestors)-1].root
		info, err := parent.Lstat(name)
		if err != nil {
			return nil, originalWorkOutboxObservation("cannot observe original outbox path", err)
		}
		if !info.IsDir() {
			return nil, originalWorkOutboxLoss("outbox ancestor is not a physical directory", nil)
		}
		next, err := parent.OpenRoot(name)
		if err != nil {
			return nil, err
		}
		self.ancestors = append(self.ancestors, originalWorkOutboxAncestor{root: next, name: name, info: info})
		opened, err := next.Stat(".")
		if err != nil {
			return nil, err
		}
		if !os.SameFile(info, opened) {
			return nil, originalWorkOutboxLoss("outbox ancestor changed during open", nil)
		}
	}
	self.root = self.ancestors[len(self.ancestors)-1].root
	self.directory, err = self.root.Open(".")
	if err != nil {
		return nil, err
	}
	info, err := self.directory.Stat()
	if err != nil {
		return nil, err
	}
	if !originalWorkOutboxPrivate(info, true, 0700) {
		return nil, originalWorkOutboxLoss("outbox directory is not privately owned", nil)
	}
	raw, err := readOriginalWorkOutboxAttribute(self.directory)
	if err != nil {
		if errors.Is(err, errOriginalWorkOutboxAttributeAbsent) {
			return nil, originalWorkOutboxLoss("outbox requires its explicitly prepared original birth checkpoint", err)
		}
		return nil, err
	}
	self.lock, err = lockOriginalWorkOutbox(self.root)
	if err != nil {
		return nil, originalWorkOutboxObservation("cannot acquire original outbox inventory", err)
	}
	self.lockInfo, err = self.lock.Stat()
	if err != nil {
		return nil, err
	}
	index, err := self.readIndex(ctx)
	if err != nil {
		return nil, err
	}
	checkpoint, members, err := DecodeOriginalWorkOutboxInventory(raw, index)
	if err != nil {
		return nil, err
	}
	if checkpoint.DirectoryDevice != originalWorkOutboxDevice(info) || checkpoint.DirectoryInode != originalWorkOutboxInode(info) || checkpoint.IndexInode != originalWorkOutboxInode(self.lockInfo) || originalWorkOutboxDevice(self.lockInfo) != checkpoint.DirectoryDevice {
		return nil, originalWorkOutboxLoss("outbox root or inventory inode differs from birth", nil)
	}
	self.checkpoint, self.raw, self.index = checkpoint, raw, index
	for _, member := range members {
		self.members[member.Name] = member
	}

	if err := self.check(ctx); err != nil {
		return nil, err
	}
	if _, err := self.census(ctx, self.checkpoint.Pending); err != nil {
		return nil, err
	}
	for _, member := range self.members {
		if _, err := self.readMember(ctx, member); err != nil {
			return nil, err
		}
	}
	if self.checkpoint.Pending != nil {
		if reconcile {
			if err := self.completePending(ctx); err != nil {
				return nil, err
			}
		} else if _, err := self.readMember(ctx, *self.checkpoint.Pending); err != nil {
			return nil, err
		}
	}
	return self, nil
}

// The SDK joins this owner before releasing its retained directory lease.
func (self *originalWorkOutbox) close() error {
	var result error
	if self.lock != nil {
		result = errors.Join(result, self.lock.Close())
		self.lock = nil
	}
	if self.directory != nil {
		result = errors.Join(result, self.directory.Close())
		self.directory = nil
	}
	for i := len(self.ancestors) - 1; i >= 0; i-- {
		result = errors.Join(result, self.ancestors[i].root.Close())
	}
	self.ancestors, self.root = nil, nil
	return result
}

func (self *originalWorkOutbox) boundary(ctx context.Context, stage, name string) error {
	if err := ctx.Err(); err != nil {
		return errors.Join(err, context.Cause(ctx))
	}
	if self.step != nil {
		if err := self.step(stage, name); err != nil {
			return err
		}
	}
	return errors.Join(ctx.Err(), context.Cause(ctx))
}

func (self *originalWorkOutbox) checkPhysical(ctx context.Context) error {
	if err := self.boundary(ctx, "physical-observation", ""); err != nil {
		return err
	}
	for i := 1; i < len(self.ancestors); i++ {
		anchor := self.ancestors[i]
		info, err := self.ancestors[i-1].root.Lstat(anchor.name)
		if err != nil {
			return originalWorkOutboxObservation("cannot observe retained outbox ancestor", err)
		}
		if !info.IsDir() || !os.SameFile(info, anchor.info) || info.Mode() != anchor.info.Mode() || !originalWorkOutboxSameOwner(info, anchor.info) {
			return originalWorkOutboxLoss("outbox ancestor changed after admission", nil)
		}
	}
	info, err := self.root.Lstat(originalWorkOutboxIndexName)
	if err != nil {
		return originalWorkOutboxObservation("cannot observe retained outbox inventory", err)
	}
	opened, err := self.lock.Stat()
	if err != nil {
		return originalWorkOutboxObservation("cannot observe retained inventory descriptor", err)
	}
	if !originalWorkOutboxPrivate(info, false, 0600) || !originalWorkOutboxSameState(info, opened) || !os.SameFile(info, self.lockInfo) {
		return originalWorkOutboxLoss("outbox inventory was replaced or unprotected", nil)
	}
	return nil
}

func (self *originalWorkOutbox) check(ctx context.Context) (resultErr error) {
	if self.failure != nil {
		return self.failure
	}
	defer func() {
		if errors.Is(resultErr, ErrOriginalWorkOutboxIdentity) {
			self.failure = resultErr
		}
	}()
	if err := self.checkPhysical(ctx); err != nil {
		return err
	}
	if err := self.boundary(ctx, "checkpoint-read", ""); err != nil {
		return err
	}
	raw, err := readOriginalWorkOutboxAttribute(self.directory)
	if err != nil {
		if errors.Is(err, errOriginalWorkOutboxAttributeAbsent) {
			return originalWorkOutboxLoss("outbox birth checkpoint disappeared", err)
		}
		return err
	}
	if !bytes.Equal(raw, self.raw) {
		return originalWorkOutboxLoss("outbox checkpoint changed outside its owner", nil)
	}
	index, err := self.readIndex(ctx)
	if err != nil {
		return err
	}
	if !bytes.Equal(index, self.index) {
		return originalWorkOutboxLoss("outbox inventory changed outside its owner", nil)
	}
	return nil
}

func (self *originalWorkOutbox) readIndex(ctx context.Context) ([]byte, error) {
	if err := self.boundary(ctx, "inventory-read", originalWorkOutboxIndexName); err != nil {
		return nil, err
	}
	info, err := self.lock.Stat()
	if err != nil {
		return nil, err
	}
	if !originalWorkOutboxPrivate(info, false, 0600) || info.Size() < 0 || info.Size() > maximumOriginalWorkOutboxIndexBytes {
		return nil, originalWorkOutboxLoss("outbox inventory has invalid custody or size", nil)
	}
	raw := make([]byte, int(info.Size()))
	if _, err := self.lock.ReadAt(raw, 0); err != nil {
		return nil, err
	}
	after, err := self.lock.Stat()
	if err != nil {
		return nil, err
	}
	if !originalWorkOutboxSameState(info, after) {
		return nil, originalWorkOutboxLoss("outbox inventory changed during read", nil)
	}
	return raw, ctx.Err()
}

// A complete census is required even when the requested name is absent.
func (self *originalWorkOutbox) census(ctx context.Context, pending *OriginalWorkOutboxMember) (_ []string, resultErr error) {
	file, err := self.root.Open(".")
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	entries, err := file.ReadDir(maximumOriginalWorkOutboxRecords + 2)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(entries) > maximumOriginalWorkOutboxRecords+1 {
		return nil, originalWorkOutboxLoss("outbox census exceeds its complete inventory", nil)
	}
	seenKVs := map[string]bool{}
	var names []string
	for _, entry := range entries {
		if err := self.boundary(ctx, "census-member", entry.Name()); err != nil {
			return nil, err
		}
		name := entry.Name()
		if name == originalWorkOutboxIndexName {
			seenKVs[name] = true
			continue
		}
		member, ok := self.members[name]
		if !ok && pending != nil && pending.Name == name {
			member, ok = *pending, true
		}
		if !ok || seenKVs[name] {
			return nil, originalWorkOutboxLoss("outbox has an original outside its birth inventory", nil)
		}
		info, err := self.root.Lstat(name)
		if err != nil {
			return nil, originalWorkOutboxObservation("cannot observe inventoried original", err)
		}
		if !originalWorkOutboxPrivate(info, false, 0400) || originalWorkOutboxDevice(info) != self.checkpoint.DirectoryDevice || originalWorkOutboxInode(info) != member.Inode || info.Size() < 0 || uint64(info.Size()) != member.Bytes {
			return nil, originalWorkOutboxLoss("outbox inventoried original is missing, partial or replaced", nil)
		}
		seenKVs[name] = true
		names = append(names, name)
	}
	if !seenKVs[originalWorkOutboxIndexName] || len(seenKVs) != len(self.members)+1+func() int {
		if pending != nil {
			return 1
		}
		return 0
	}() {
		return nil, originalWorkOutboxLoss("outbox lost an acknowledged or pending original", nil)
	}
	return names, nil
}

func (self *originalWorkOutbox) entries(ctx context.Context) (names []string, resultErr error) {
	if ctx == nil {
		return nil, errors.New("outbox inventory requires its caller context")
	}
	if err := self.check(ctx); err != nil {
		return nil, err
	}
	defer func() {
		if errors.Is(resultErr, ErrOriginalWorkOutboxIdentity) {
			self.failure = resultErr
		}
	}()
	if self.checkpoint.Pending != nil {
		return nil, ErrOriginalWorkOutboxUncertain
	}
	names, err := self.census(ctx, nil)
	if err != nil {
		return nil, err
	}
	return names, self.check(ctx)
}

// Bounded reads compare observations only after every syscall succeeded.
func (self *originalWorkOutbox) readMember(ctx context.Context, member OriginalWorkOutboxMember) (_ []byte, resultErr error) {
	info, err := self.root.Lstat(member.Name)
	if err != nil {
		return nil, originalWorkOutboxObservation("cannot observe retained original", err)
	}
	if !originalWorkOutboxPrivate(info, false, 0400) || originalWorkOutboxDevice(info) != self.checkpoint.DirectoryDevice || originalWorkOutboxInode(info) != member.Inode || info.Size() < 0 || uint64(info.Size()) != member.Bytes {
		return nil, originalWorkOutboxLoss("retained original differs from inventory", nil)
	}
	file, err := self.root.OpenFile(member.Name, os.O_RDONLY|originalWorkOutboxNoFollow(), 0)
	if err != nil {
		return nil, originalWorkOutboxObservation("cannot open retained original", err)
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !originalWorkOutboxSameState(info, opened) {
		return nil, originalWorkOutboxLoss("retained original changed during open", nil)
	}
	raw := make([]byte, 0, int(member.Bytes))
	buffer := make([]byte, 32*1024)
	read := file.Read
	if self.readFile != nil {
		read = func(raw []byte) (int, error) { return self.readFile(file, raw) }
	}
	for {
		if err := self.boundary(ctx, "original-read", member.Name); err != nil {
			return nil, err
		}
		n, readErr := read(buffer)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, readErr
		}
		if uint64(n) > member.Bytes-uint64(len(raw)) {
			return nil, originalWorkOutboxLoss("retained original grew during read", nil)
		}
		raw = append(raw, buffer[:n]...)
		if errors.Is(readErr, io.EOF) {
			break
		}
		if n == 0 {
			return nil, io.ErrNoProgress
		}
	}
	if err := self.boundary(ctx, "original-readback", member.Name); err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil {
		return nil, err
	}
	named, err := self.root.Lstat(member.Name)
	if err != nil {
		return nil, originalWorkOutboxObservation("cannot reobserve retained original name", err)
	}
	if !originalWorkOutboxSameState(info, after) || !originalWorkOutboxSameState(info, named) || uint64(len(raw)) != member.Bytes || originalWorkOutboxDigest(raw) != member.Sha256 {
		return nil, originalWorkOutboxLoss("retained original changed during read", nil)
	}
	return raw, self.check(ctx)
}

func (self *originalWorkOutbox) read(ctx context.Context, name string) (_ []byte, resultErr error) {
	if _, err := self.entries(ctx); err != nil {
		return nil, err
	}
	member, ok := self.members[name]
	if !ok {
		return nil, errors.Join(errOriginalWorkOutboxUncaptured, os.ErrNotExist)
	}
	defer func() {
		if errors.Is(resultErr, ErrOriginalWorkOutboxIdentity) {
			self.failure = resultErr
		}
	}()
	raw, err := self.readMember(ctx, member)
	if err != nil {
		return nil, err
	}
	if _, err := self.entries(ctx); err != nil {
		return nil, err
	}
	return raw, nil
}

// The exact pending member is published before its body. A failed mutation
// blocks this instance; joined reopen may reconcile only complete retained bytes.
func (self *originalWorkOutbox) retain(ctx context.Context, name string, raw []byte) (resultErr error) {
	if _, err := self.entries(ctx); err != nil {
		return err
	}
	if !originalWorkOutboxCanonicalName(name) || len(raw) == 0 || len(raw) > protocol.MaximumOriginalWorkSubmissionBytes || self.checkpoint.Records >= maximumOriginalWorkOutboxRecords || uint64(len(raw)) > maximumOriginalWorkOutboxBytes-self.checkpoint.OriginalBytes {
		return errors.New("whole-work outbox has no complete-record capacity")
	}
	if _, ok := self.members[name]; ok {
		return originalWorkOutboxLoss("outbox boundary was already captured", nil)
	}
	file, err := self.root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL|originalWorkOutboxNoFollow(), 0600)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, file.Close())
		if resultErr != nil {
			resultErr = errors.Join(ErrOriginalWorkOutboxUncertain, resultErr)
			self.failure = resultErr
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !originalWorkOutboxPrivate(info, false, 0600) {
		return originalWorkOutboxLoss("new original is not privately owned", nil)
	}
	next := self.checkpoint
	next.Pending = &OriginalWorkOutboxMember{Name: name, Inode: originalWorkOutboxInode(info), Bytes: uint64(len(raw)), Sha256: originalWorkOutboxDigest(raw)}
	if err := self.publish(ctx, next); err != nil {
		return err
	}
	if err := self.boundary(ctx, "pending-published", name); err != nil {
		return err
	}
	for offset := 0; offset < len(raw); {
		if err := self.boundary(ctx, "original-write", name); err != nil {
			return err
		}
		chunk := raw[offset:min(offset+32*1024, len(raw))]
		n, err := file.Write(chunk)
		if err != nil {
			return err
		}
		if n != len(chunk) {
			return io.ErrShortWrite
		}
		offset += n
	}
	if err := file.Chmod(0400); err != nil {
		return err
	}
	if err := errors.Join(file.Sync(), self.directory.Sync()); err != nil {
		return err
	}
	if err := self.boundary(ctx, "original-synced", name); err != nil {
		return err
	}
	return self.completePending(ctx)
}

func (self *originalWorkOutbox) completePending(ctx context.Context) error {
	pending := self.checkpoint.Pending
	if pending == nil {
		return originalWorkOutboxLoss("outbox completion has no pending original", nil)
	}
	if _, err := self.census(ctx, pending); err != nil {
		return err
	}
	if _, err := self.readMember(ctx, *pending); err != nil {
		return err
	}
	line, err := json.Marshal(pending)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if uint64(len(self.index)) == self.checkpoint.IndexBytes {
		if err := self.check(ctx); err != nil {
			return err
		}
		if n, err := self.lock.WriteAt(line, int64(len(self.index))); err != nil {
			return errors.Join(ErrOriginalWorkOutboxUncertain, err)
		} else if n != len(line) {
			return errors.Join(ErrOriginalWorkOutboxUncertain, io.ErrShortWrite)
		}
		self.index = append(self.index, line...)
	}
	if err := self.lock.Sync(); err != nil {
		return errors.Join(ErrOriginalWorkOutboxUncertain, err)
	}
	if err := self.boundary(ctx, "inventory-synced", pending.Name); err != nil {
		return errors.Join(ErrOriginalWorkOutboxUncertain, err)
	}
	next := self.checkpoint
	next.IndexBytes, next.IndexSha256 = uint64(len(self.index)), originalWorkOutboxDigest(self.index)
	next.Records++
	next.OriginalBytes += pending.Bytes
	next.Pending = nil
	if err := self.publish(ctx, next); err != nil {
		return err
	}
	self.members[pending.Name] = *pending
	_, err = self.entries(ctx)
	return err
}

func (self *originalWorkOutbox) publish(ctx context.Context, checkpoint OriginalWorkOutboxCheckpoint) error {
	if err := self.checkPhysical(ctx); err != nil {
		return err
	}
	if err := self.check(ctx); err != nil {
		return err
	}
	raw, err := json.Marshal(checkpoint)
	if err != nil || len(raw) > 4096 {
		return errors.Join(errors.New("outbox checkpoint exceeds its bound"), err)
	}
	if err := replaceOriginalWorkOutboxAttribute(self.directory, raw, false); err != nil {
		return errors.Join(ErrOriginalWorkOutboxUncertain, err)
	}
	if err := errors.Join(self.directory.Sync(), self.boundary(ctx, "checkpoint-written", "")); err != nil {
		return errors.Join(ErrOriginalWorkOutboxUncertain, err)
	}
	actual, err := readOriginalWorkOutboxAttribute(self.directory)
	if err != nil {
		return errors.Join(ErrOriginalWorkOutboxUncertain, err)
	}
	if !bytes.Equal(actual, raw) {
		return errors.Join(ErrOriginalWorkOutboxUncertain, originalWorkOutboxLoss("outbox checkpoint changed during acknowledgement", nil))
	}
	if err := self.checkPhysical(ctx); err != nil {
		return errors.Join(ErrOriginalWorkOutboxUncertain, err)
	}
	self.checkpoint, self.raw = checkpoint, raw
	return nil
}
