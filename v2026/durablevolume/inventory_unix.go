//go:build linux || darwin

// Traversal stays on real no-follow descriptors and refuses partial inventories.
// Bounded reads and context checks permit a stopped owner to cancel the work.
package durablevolume

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// Exact file metadata guards detect concurrent mutation without changing bytes.
func inventoryStat(file *os.File) (unix.Stat_t, error) {
	var stat unix.Stat_t
	err := unix.Fstat(int(file.Fd()), &stat)
	return stat, err
}

// Changing times, size, links or identity during a read defeats completeness.
func unchangedInventoryStat(before, after unix.Stat_t) bool {
	return before.Dev == after.Dev && before.Ino == after.Ino && before.Mode == after.Mode && before.Uid == after.Uid && before.Gid == after.Gid && before.Size == after.Size && before.Nlink == after.Nlink && before.Mtim == after.Mtim && before.Ctim == after.Ctim
}

// Snapshot callers retain the exclusive lease throughout the bounded walk.
func (self *Owner) inventory(ctx context.Context, result *Inventory) (resultErr error) {
	root, err := self.openChild("", false)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, unavailableObservation("inventory root could not be closed", root.Close()))
	}()
	rootStat, err := inventoryStat(root)
	if err != nil {
		return unavailableObservation("inventory root metadata could not be observed", err)
	}
	result.PhysicalRoot = PhysicalRoot{Device: statDevice(&rootStat), Inode: rootStat.Ino}
	generation, err := self.rootGeneration()
	if err != nil {
		return err
	}
	result.RootGeneration = hex.EncodeToString(generation)
	// Limit escaped path and metadata reporting independently of file contents.
	// This leaves room for declarations within the 64 MiB evidence reader bound.
	var entryReportBytes uint64
	appendEntry := func(entry InventoryEntry) error {
		raw, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		entryReportBytes += uint64(len(raw)) + 1
		if entryReportBytes > 60*1024*1024 {
			return errors.New("inventory encoded metadata report bound exhausted")
		}
		result.Entries = append(result.Entries, entry)
		return nil
	}
	var visit func(*os.File, string, uint64) error
	visit = func(file *os.File, relative string, depth uint64) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if uint64(len(result.Entries)) >= result.Limits.MaxEntries || depth > result.Limits.MaxDepth {
			return errors.New("durable inventory work bound exhausted")
		}
		before, err := inventoryStat(file)
		if err != nil {
			return unavailableObservation("inventory metadata could not be observed", err)
		}
		directory := before.Mode&syscall.S_IFMT == syscall.S_IFDIR
		if err := protected(file, directory); err != nil {
			return err
		}
		if statDevice(&before) != self.mount.Device {
			return errors.Join(ErrIdentity, errors.New("inventory entered another filesystem"))
		}
		if err := self.childMount(filepath.Join(self.rootPath, relative)); err != nil {
			return err
		}
		entry := InventoryEntry{Path: relative, Mode: uint32(before.Mode) & 07777, Uid: before.Uid, Gid: before.Gid}
		if result.Schema == PhysicalInventorySchema {
			entry.Physical = &PhysicalRoot{Device: statDevice(&before), Inode: before.Ino}
		}
		entry.OwnerAttributes, err = self.inventoryAttributes(ctx, file, relative, result)
		if err != nil {
			return err
		}
		if directory {
			entry.Kind = "directory"
			if err := appendEntry(entry); err != nil {
				return err
			}
			remaining := result.Limits.MaxEntries - uint64(len(result.Entries))
			children, err := file.ReadDir(int(remaining + 1))
			if err != nil && !errors.Is(err, io.EOF) {
				return unavailableObservation("inventory directory could not be read", err)
			}
			if uint64(len(children)) > remaining {
				return errors.New("durable inventory entry bound exhausted")
			}
			sort.Slice(children, func(i, j int) bool { return children[i].Name() < children[j].Name() })
			for _, child := range children {
				if err := ctx.Err(); err != nil {
					return err
				}
				path := filepath.Join(relative, child.Name())
				if !utf8.ValidString(path) {
					return errors.New("durable inventory name is not valid utf-8")
				}
				if _, err := relativeParts(path); err != nil {
					return err
				}
				if err := self.check(false); err != nil {
					return err
				}
				fd, err := unix.Openat(int(file.Fd()), child.Name(), syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
				if err != nil {
					return namedObservation("inventory child could not be opened", err)
				}
				opened := os.NewFile(uintptr(fd), filepath.Join(self.rootPath, path))
				childErr := visit(opened, path, depth+1)
				nameErr := sameNamedFileObserved(opened, filepath.Join(self.rootPath, path), self.observeFile)
				closeErr := unavailableObservation("inventory child could not be closed", opened.Close())
				if err := errors.Join(childErr, nameErr, closeErr); err != nil {
					return err
				}
			}
		} else {
			entry.Kind = "file"
			if before.Size < 0 || uint64(before.Size) > result.Limits.MaxBytes-result.TotalBytes {
				return errors.New("durable inventory byte bound exhausted")
			}
			entry.Size = uint64(before.Size)
			hash := sha256.New()
			buffer := make([]byte, 128*1024)
			for offset := int64(0); offset < before.Size; {
				if err := ctx.Err(); err != nil {
					return err
				}
				want := int64(len(buffer))
				if before.Size-offset < want {
					want = before.Size - offset
				}
				n, err := file.ReadAt(buffer[:want], offset)
				if n != int(want) || err != nil {
					if err != nil && !errors.Is(err, io.EOF) {
						return unavailableObservation("inventory file bytes could not be observed", err)
					}
					return errors.Join(ErrIdentity, errors.New("durable inventory file read was incomplete"), err)
				}
				_, _ = hash.Write(buffer[:n])
				offset += int64(n)
			}
			entry.Sha256 = "sha256:" + hex.EncodeToString(hash.Sum(nil))
			result.TotalBytes += entry.Size
			if err := appendEntry(entry); err != nil {
				return err
			}
		}
		after, err := inventoryStat(file)
		if err != nil {
			return unavailableObservation("inventory metadata could not be reobserved", err)
		}
		if !unchangedInventoryStat(before, after) {
			return errors.Join(ErrIdentity, errors.New("durable inventory changed during traversal"))
		}
		return errors.Join(self.childMount(filepath.Join(self.rootPath, relative)), self.check(false))
	}
	if err := visit(root, "", 0); err != nil {
		return err
	}
	sort.Slice(result.Entries, func(i, j int) bool { return result.Entries[i].Path < result.Entries[j].Path })
	return nil
}
