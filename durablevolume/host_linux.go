//go:build linux

// Linux kernel facts are joined with actual no-follow descriptors. Paths alone
// never authorize creation on a fallback mount or after namespace replacement.
package durablevolume

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Fixed host paths are not configurable from a runtime declaration.
type linuxHost struct{}

// Each owner gets an immutable adapter with no process-global state.
func defaultHost() Host { return linuxHost{} }

// Linux's encoded dev_t is stable across stat and block-device identity reads.
func deviceNumber(number uint64) Device {
	return Device{Major: uint32((number>>8)&0xfff | (number>>32)&0xfffff000), Minor: uint32(number&0xff | (number>>12)&0xffffff00)}
}

// Reads only a bounded kernel snapshot; malformed escapes cannot alter a path.
func (self linuxHost) Mounts() ([]Mount, error) {
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, 4*1024*1024+1))
	if err := errors.Join(readErr, file.Close()); err != nil {
		return nil, err
	}
	if len(raw) > 4*1024*1024 {
		return nil, errors.New("mount information exceeds its byte bound")
	}
	return parseMounts(raw)
}

// Parsing is independent of ambient mounts so escaped and competing entries
// can be qualified without mount privileges or changes to the host namespace.
func parseMounts(raw []byte) ([]Mount, error) {
	decodePath := func(value string) (string, error) {
		var result strings.Builder
		for index := 0; index < len(value); index++ {
			if value[index] != '\\' {
				result.WriteByte(value[index])
				continue
			}
			if index+4 > len(value) {
				return "", errors.New("mount path escape is truncated")
			}
			switch value[index : index+4] {
			case `\040`:
				result.WriteByte(' ')
			case `\011`:
				result.WriteByte('\t')
			case `\012`:
				result.WriteByte('\n')
			case `\134`:
				result.WriteByte('\\')
			default:
				return "", errors.New("mount path escape is unknown")
			}
			index += 3
		}
		return result.String(), nil
	}
	var result []Mount
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 4096), 64*1024)
	for scanner.Scan() {
		parts := strings.Fields(scanner.Text())
		separator := -1
		for index, part := range parts {
			if part == "-" {
				separator = index
				break
			}
		}
		if len(result) >= 8192 || separator < 6 || len(parts) != separator+4 {
			return nil, errors.New("mount information has an invalid or unbounded record")
		}
		id, idErr := strconv.ParseUint(parts[0], 10, 64)
		parent, parentErr := strconv.ParseUint(parts[1], 10, 64)
		deviceParts := strings.Split(parts[2], ":")
		if idErr != nil || parentErr != nil || id == 0 || len(deviceParts) != 2 {
			return nil, errors.New("mount identity is malformed")
		}
		major, majorErr := strconv.ParseUint(deviceParts[0], 10, 32)
		minor, minorErr := strconv.ParseUint(deviceParts[1], 10, 32)
		root, rootErr := decodePath(parts[3])
		path, pathErr := decodePath(parts[4])
		if err := errors.Join(majorErr, minorErr, rootErr, pathErr); err != nil {
			return nil, err
		}
		readOnly := false
		for _, option := range strings.Split(parts[5]+","+parts[separator+3], ",") {
			readOnly = readOnly || option == "ro"
		}
		result = append(result, Mount{Id: id, ParentId: parent, Device: Device{Major: uint32(major), Minor: uint32(minor)}, Root: root, Path: path, FilesystemType: parts[separator+1], ReadOnly: readOnly})
	}
	return result, scanner.Err()
}

// Re-enumerated device names are accepted only through the configured uuid.
func (self linuxHost) DeviceUuid(uuid string) (Device, error) {
	var stat syscall.Stat_t
	if err := syscall.Stat(filepath.Join("/dev/disk/by-uuid", uuid), &stat); err != nil {
		return Device{}, err
	}
	if stat.Mode&syscall.S_IFMT != syscall.S_IFBLK {
		return Device{}, errors.Join(ErrIdentity, errors.New("filesystem uuid does not resolve to a block device"))
	}
	return deviceNumber(stat.Rdev), nil
}

// Available blocks/inodes use the process's available allocation, not totals.
func (self linuxHost) Filesystem(directory *os.File) (Filesystem, error) {
	var state syscall.Statfs_t
	if err := syscall.Fstatfs(int(directory.Fd()), &state); err != nil {
		return Filesystem{}, err
	}
	if state.Bsize <= 0 || state.Bavail > math.MaxUint64/uint64(state.Bsize) {
		return Filesystem{}, errors.New("filesystem available-byte arithmetic is invalid")
	}
	return Filesystem{Id: state.Fsid.X__val, Type: state.Type, ReadOnly: state.Flags&1 != 0,
		AvailableBytes: state.Bavail * uint64(state.Bsize), AvailableInodes: state.Ffree}, nil
}

// Owned objects cannot be shared-writable, aliased files or special devices.
func unavailableObservation(reason string, err error) error {
	if err == nil || errors.Is(err, ErrIdentity) || errors.Is(err, ErrUnavailable) || errors.Is(err, ErrClosed) || errors.Is(err, os.ErrClosed) || errors.Is(err, os.ErrInvalid) || errors.Is(err, syscall.EBADF) {
		return err
	}
	return errors.Join(&UnavailableError{Reason: reason}, err)
}

// A successful namespace refusal proves absence/alias; I/O refusal proves none.
func namedObservation(reason string, err error) error {
	if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.ELOOP) {
		return errors.Join(ErrIdentity, err)
	}
	return unavailableObservation(reason, err)
}

// Protection is identity only after an actual successful metadata observation.
func protected(file *os.File, directory bool) error {
	var stat syscall.Stat_t
	if file == nil {
		return ErrClosed
	}
	if err := syscall.Fstat(int(file.Fd()), &stat); err != nil {
		return unavailableObservation("durable descriptor protection could not be observed", err)
	}
	want := uint32(syscall.S_IFREG)
	if directory {
		want = syscall.S_IFDIR
	}
	if stat.Mode&syscall.S_IFMT != want || stat.Mode&0022 != 0 || stat.Uid != 0 && stat.Uid != uint32(os.Geteuid()) || !directory && stat.Nlink != 1 {
		return errors.Join(ErrIdentity, fmt.Errorf("durable path %q is not a protected physical object (mode %o, uid %d, links %d)", file.Name(), stat.Mode, stat.Uid, stat.Nlink))
	}
	return nil
}

// A root-owned sticky ancestor protects each owned child from other users.
// The selected directory itself still requires the stricter private policy.
func protectedAncestor(file *os.File) error {
	if err := protected(file, true); err == nil || !errors.Is(err, ErrIdentity) {
		return err
	}
	var stat syscall.Stat_t
	if err := syscall.Fstat(int(file.Fd()), &stat); err != nil {
		return unavailableObservation("durable ancestor metadata could not be observed", err)
	}
	if stat.Mode&syscall.S_IFMT != syscall.S_IFDIR || stat.Uid != 0 || stat.Mode&syscall.S_ISVTX == 0 {
		return errors.Join(ErrIdentity, errors.New("durable ancestor is not protected"))
	}
	return nil
}

// Walk each component from a descriptor; no ancestor symlink is followed.
func openPhysicalDirectory(path string) (*os.File, error) {
	if path != "/" && !canonical(path) {
		return nil, errors.New("durable directory path is not canonical")
	}
	fd, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, unavailableObservation("durable ancestry could not be opened", err)
	}
	file := os.NewFile(uintptr(fd), "/")
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if err := protectedAncestor(file); err != nil {
			return nil, errors.Join(err, file.Close())
		}
		if part == "" {
			continue
		}
		next, openErr := syscall.Openat(int(file.Fd()), part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
		closeErr := file.Close()
		if openErr != nil {
			return nil, errors.Join(namedObservation("durable ancestry could not be opened", openErr), unavailableObservation("durable ancestor could not be closed", closeErr))
		}
		file = os.NewFile(uintptr(next), path)
		if closeErr != nil {
			return nil, unavailableObservation("durable ancestors could not be closed", errors.Join(closeErr, file.Close()))
		}
	}
	if err := protected(file, true); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}

// Opens the leaf relative to validated physical ancestry without blocking on
// fifo/device objects. Regular-file and link-count checks precede every read.
func openProtectedFile(path string) (*os.File, error) {
	parent, err := openPhysicalDirectory(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	fd, openErr := syscall.Openat(int(parent.Fd()), filepath.Base(path), syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	closeErr := parent.Close()
	if openErr != nil {
		return nil, errors.Join(namedObservation("durable named file could not be opened", openErr), unavailableObservation("durable ancestor could not be closed", closeErr))
	}
	file := os.NewFile(uintptr(fd), path)
	if err := errors.Join(unavailableObservation("durable ancestor could not be closed", closeErr), protected(file, false)); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}

// Concurrent readers use ReadAt rather than a shared file offset.
func boundedProtectedRead(file *os.File, maximum int) ([]byte, error) {
	return boundedProtectedReadContext(context.Background(), file, maximum)
}

// Finite chunks admit cancellation before each actual descriptor read.
func boundedProtectedReadContext(ctx context.Context, file *os.File, maximum int) ([]byte, error) {
	if ctx == nil {
		return nil, errors.New("durable inventory context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := protected(file, false); err != nil {
		return nil, err
	}
	before, err := file.Stat()
	if err != nil {
		return nil, unavailableObservation("durable file metadata could not be observed", err)
	}
	if before.Size() <= 0 || before.Size() > int64(maximum) {
		return nil, errors.Join(ErrIdentity, errors.New("durable protected file exceeds its size bound"))
	}
	raw := make([]byte, int(before.Size())+1)
	n := 0
	for n < len(raw) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		count, readErr := file.ReadAt(raw[n:min(n+128*1024, len(raw))], int64(n))
		n += count
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, unavailableObservation("durable file bytes could not be observed", readErr)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil {
		return nil, unavailableObservation("durable file metadata could not be reobserved", err)
	}
	if n != int(before.Size()) || after.Size() != before.Size() || after.Mode() != before.Mode() || !after.ModTime().Equal(before.ModTime()) {
		return nil, errors.Join(ErrIdentity, errors.New("durable protected file changed during read"))
	}
	return raw[:n], protected(file, false)
}

// Conflicting pathname replacement is refused even if copied bytes match.
func readProtectedFile(path string, maximum int) ([]byte, error) {
	return readProtectedFileContext(context.Background(), path, maximum)
}

// Context admission precedes even path resolution/open of external evidence.
func readProtectedFileContext(ctx context.Context, path string, maximum int) ([]byte, error) {
	if ctx == nil {
		return nil, errors.New("durable inventory context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := openProtectedFile(path)
	if err != nil {
		return nil, err
	}
	raw, readErr := boundedProtectedReadContext(ctx, file, maximum)
	matchErr := sameNamedFile(file, path)
	err = errors.Join(readErr, matchErr, unavailableObservation("durable evidence could not be closed", file.Close()), ctx.Err())
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// Opened and named objects must remain the same protected physical generation.
func sameNamedFile(file *os.File, path string) error {
	return sameNamedFileObserved(file, path, nil)
}

// Failure injection retains all successful real descriptor/path operations.
func sameNamedFileObserved(file *os.File, path string, observe func(string, *os.File, string) error) error {
	if observe != nil {
		if err := observe("opened-stat", file, path); err != nil {
			return unavailableObservation("durable opened metadata could not be observed", err)
		}
	}
	opened, err := file.Stat()
	if err != nil {
		return unavailableObservation("durable opened metadata could not be observed", err)
	}
	var current *os.File
	if observe != nil {
		if err := observe("named-open", file, path); err != nil {
			return namedObservation("durable named file could not be opened", err)
		}
	}
	if opened.IsDir() {
		current, err = openPhysicalDirectory(path)
	} else {
		current, err = openProtectedFile(path)
	}
	if err != nil {
		return namedObservation("durable named file could not be opened", err)
	}
	var nameErr error
	if observe != nil {
		nameErr = observe("named-stat", current, path)
	}
	var named os.FileInfo
	if nameErr == nil {
		named, nameErr = current.Stat()
	}
	closeErr := current.Close()
	if observe != nil {
		closeErr = errors.Join(closeErr, observe("named-close", current, path))
	}
	closeErr = unavailableObservation("durable named file could not be closed", closeErr)
	if nameErr != nil {
		return errors.Join(unavailableObservation("durable named metadata could not be observed", nameErr), closeErr)
	}
	if !os.SameFile(opened, named) || opened.Mode() != named.Mode() {
		return errors.Join(ErrIdentity, errors.New("durable physical path was replaced"), closeErr)
	}
	return errors.Join(protected(file, opened.IsDir()), closeErr)
}

// Select one exact mount and reject a nested mount over the marker or owner.
func (self *Owner) mountFacts() (Mount, error) {
	mounts, err := self.host.Mounts()
	if err != nil {
		return Mount{}, errors.Join(&UnavailableError{Reason: "kernel mount census could not be observed"}, err)
	}
	if len(mounts) == 0 || len(mounts) > 8192 {
		return Mount{}, errors.Join(ErrIdentity, errors.New("durable mount census is empty or unbounded"))
	}
	var selected, root Mount
	selectedCount, rootCount := 0, 0
	for _, mount := range mounts {
		if mount.Path == "/" {
			root, rootCount = mount, rootCount+1
		}
		if mount.Path == self.spec.MountPath {
			selected, selectedCount = mount, selectedCount+1
		}
	}
	if selectedCount != 1 || rootCount != 1 || selected.Id == 0 || selected.FilesystemType != self.spec.FilesystemType {
		return Mount{}, errors.Join(ErrIdentity, errors.New("approved durable mount is absent, ambiguous or has another filesystem type"))
	}
	if selected.Device == root.Device && self.scope != ownerLocalScope {
		return Mount{}, errors.Join(ErrIdentity, errors.New("daemon durable mount is on the root filesystem"))
	}
	for _, mount := range mounts {
		if mount.Path != selected.Path && beneath(selected.Path, mount.Path) && (beneath(mount.Path, self.rootPath) || beneath(mount.Path, self.spec.MarkerPath) || beneath(mount.Path, self.rootSpec.LeasePath)) {
			return Mount{}, errors.Join(ErrIdentity, errors.New("another mount covers the durable marker or owner root"))
		}
	}
	device, err := self.host.DeviceUuid(self.spec.FilesystemUuid)
	if err != nil {
		if errors.Is(err, ErrIdentity) {
			return Mount{}, err
		}
		return Mount{}, errors.Join(&UnavailableError{Reason: "kernel uuid device could not be observed"}, err)
	}
	if device != selected.Device {
		return Mount{}, errors.Join(ErrIdentity, errors.New("durable mount differs from the approved filesystem uuid"))
	}
	return selected, nil
}

// Constructor failure retains all existing bytes and releases only our lease.
func (self *Owner) open() error {
	var err error
	self.mount, err = self.mountFacts()
	if err != nil {
		return err
	}
	self.mountFile, err = openPhysicalDirectory(self.spec.MountPath)
	if err != nil {
		return err
	}
	self.rootFile, err = openPhysicalDirectory(self.rootPath)
	if err != nil {
		return err
	}
	self.markerFile, err = openProtectedFile(self.spec.MarkerPath)
	if err != nil {
		return err
	}
	self.leaseFile, err = openProtectedFile(self.rootSpec.LeasePath)
	if err != nil {
		return err
	}
	lock := syscall.LOCK_SH | syscall.LOCK_NB
	if self.access == Snapshot {
		lock = syscall.LOCK_EX | syscall.LOCK_NB
	}
	if err := syscall.Flock(int(self.leaseFile.Fd()), lock); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return &BusyError{Root: self.rootPath, Access: self.access}
		}
		return err
	}
	self.filesystem, err = self.host.Filesystem(self.rootFile)
	if err != nil {
		return unavailableObservation("initial durable filesystem facts could not be observed", err)
	}
	return self.check(self.access == ReadWrite)
}

// Every admission rechecks the kernel namespace, current uuid and marker bytes.
func (self *Owner) check(write bool) error {
	mount, err := self.mountFacts()
	if err != nil {
		return err
	}
	identity := mount
	identity.ReadOnly = self.mount.ReadOnly
	if identity != self.mount {
		return errors.Join(ErrIdentity, errors.New("durable mount generation changed"))
	}
	for _, entry := range []struct {
		file *os.File
		path string
	}{
		{file: self.mountFile, path: self.spec.MountPath},
		{file: self.rootFile, path: self.rootPath},
		{file: self.markerFile, path: self.spec.MarkerPath},
		{file: self.leaseFile, path: self.rootSpec.LeasePath},
	} {
		if err := sameNamedFileObserved(entry.file, entry.path, self.observeFile); err != nil {
			return err
		}
		var stat syscall.Stat_t
		if err := syscall.Fstat(int(entry.file.Fd()), &stat); err != nil {
			return unavailableObservation("durable descriptor device could not be observed", err)
		}
		if deviceNumber(stat.Dev) != mount.Device {
			return errors.Join(ErrIdentity, errors.New("durable descriptor belongs to another filesystem"))
		}
	}
	for _, identity := range []struct {
		file   *os.File
		digest string
	}{
		{file: self.markerFile, digest: self.spec.MarkerSha256},
		{file: self.leaseFile, digest: self.rootSpec.LeaseSha256},
	} {
		raw, err := boundedProtectedRead(identity.file, maximumMarkerBytes)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(raw)
		if "sha256:"+hex.EncodeToString(digest[:]) != identity.digest {
			return errors.Join(ErrIdentity, errors.New("durable volume marker or root lease bytes differ"))
		}
	}
	if _, err := self.rootGeneration(); err != nil {
		return err
	}
	filesystem, err := self.host.Filesystem(self.rootFile)
	if err != nil {
		return errors.Join(&UnavailableError{Reason: "kernel filesystem facts could not be observed"}, err)
	}
	typeMatches := self.spec.FilesystemType == "ext4" && filesystem.Type == 0xef53 || self.spec.FilesystemType == "xfs" && filesystem.Type == 0x58465342 || self.spec.FilesystemType == "btrfs" && filesystem.Type == 0x9123683e
	if !typeMatches || filesystem.Id != self.filesystem.Id || filesystem.Type != self.filesystem.Type {
		return errors.Join(ErrIdentity, errors.New("durable filesystem descriptor identity changed"))
	}
	if write && (self.access != ReadWrite || mount.ReadOnly || filesystem.ReadOnly || filesystem.AvailableBytes < self.spec.MinAvailableBytes || filesystem.AvailableInodes < self.spec.MinAvailableInodes) {
		return &UnavailableError{Reason: "filesystem is read-only or below its byte/inode reserve"}
	}
	return nil
}

// A relative descendant has a finite depth and cannot use a symlink or dot path.
func relativeParts(relative string) ([]string, error) {
	if relative == "" {
		return nil, nil
	}
	if filepath.IsAbs(relative) || filepath.Clean(relative) != relative || relative == "." || len(relative) > 4096 {
		return nil, errors.New("durable child path is not canonical")
	}
	parts := strings.Split(relative, string(filepath.Separator))
	if len(parts) > 32 {
		return nil, errors.New("durable child path exceeds its depth bound")
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "\x00\n\r") {
			return nil, errors.New("durable child path escapes its owner")
		}
	}
	return parts, nil
}

// A bind mount can keep the same device number while changing child custody.
func (self *Owner) childMount(path string) error {
	mounts, err := self.host.Mounts()
	if err != nil {
		return errors.Join(&UnavailableError{Reason: "durable child mount census could not be observed"}, err)
	}
	if len(mounts) == 0 || len(mounts) > 8192 {
		return errors.Join(ErrIdentity, errors.New("durable child mount census is empty or unbounded"))
	}
	for _, mount := range mounts {
		if mount.Id != self.mount.Id && beneath(self.spec.MountPath, mount.Path) && beneath(mount.Path, path) {
			return errors.Join(ErrIdentity, errors.New("durable child crosses another mount"))
		}
	}
	return nil
}

// Child walking always stays on the retained descriptor and same filesystem.
func (self *Owner) openChild(relative string, create bool) (*os.File, error) {
	parts, err := relativeParts(relative)
	if err != nil {
		return nil, err
	}
	fd, err := syscall.Openat(int(self.rootFile.Fd()), ".", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, unavailableObservation("durable child root could not be opened", err)
	}
	file := os.NewFile(uintptr(fd), self.rootPath)
	path := self.rootPath
	for _, part := range parts {
		path = filepath.Join(path, part)
		if create {
			if err := self.check(true); err != nil {
				return nil, errors.Join(err, file.Close())
			}
		}
		next, openErr := syscall.Openat(int(file.Fd()), part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
		if errors.Is(openErr, syscall.ENOENT) && create {
			if err := syscall.Mkdirat(int(file.Fd()), part, 0700); err != nil && !errors.Is(err, syscall.EEXIST) {
				if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) || errors.Is(err, syscall.EROFS) {
					err = errors.Join(&UnavailableError{Reason: "filesystem refused directory allocation"}, err)
				}
				return nil, errors.Join(err, file.Close())
			}
			next, openErr = syscall.Openat(int(file.Fd()), part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
		}
		if openErr != nil {
			if errors.Is(openErr, syscall.ELOOP) || errors.Is(openErr, syscall.ENOTDIR) {
				openErr = errors.Join(ErrIdentity, openErr)
			}
			if !errors.Is(openErr, syscall.ENOENT) {
				openErr = unavailableObservation("durable child could not be opened", openErr)
			}
			return nil, errors.Join(openErr, unavailableObservation("durable child parent could not be closed", file.Close()))
		}
		child := os.NewFile(uintptr(next), path)
		if create {
			if err := errors.Join(child.Sync(), file.Sync()); err != nil {
				return nil, errors.Join(err, child.Close(), file.Close())
			}
		}
		if err := file.Close(); err != nil {
			return nil, unavailableObservation("durable child parent could not be closed", errors.Join(err, child.Close()))
		}
		file = child
		if err := protected(file, true); err != nil {
			return nil, errors.Join(err, unavailableObservation("durable child could not be closed", file.Close()))
		}
		var stat syscall.Stat_t
		if err := syscall.Fstat(next, &stat); err != nil {
			return nil, errors.Join(&UnavailableError{Reason: "durable child device could not be observed"}, err, file.Close())
		}
		if deviceNumber(stat.Dev) != self.mount.Device {
			return nil, errors.Join(ErrIdentity, errors.New("durable child enters another filesystem"), file.Close())
		}
		if err := self.childMount(path); err != nil {
			return nil, errors.Join(err, file.Close())
		}
	}
	return file, nil
}

// A checked new descriptor cannot redirect the caller to a later fallback root.
func (self *Owner) openDirectory(relative string, create bool) (*os.File, error) {
	if create && self.access != ReadWrite {
		return nil, errors.New("durable read-only owner cannot create descendants")
	}
	if err := self.check(self.access == ReadWrite); err != nil {
		return nil, err
	}
	file, err := self.openChild(relative, create)
	if err != nil {
		return nil, err
	}
	if err := self.check(self.access == ReadWrite); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if err := sameNamedFileObserved(file, filepath.Join(self.rootPath, relative), self.observeFile); err != nil {
		return nil, errors.Join(err, unavailableObservation("durable child could not be closed", file.Close()))
	}
	return file, nil
}

// Descriptor and logical name must agree before a caller acknowledges its write.
func (self *Owner) checkDirectory(relative string, directory *os.File, write bool) error {
	if directory == nil {
		return ErrClosed
	}
	if _, err := relativeParts(relative); err != nil {
		return err
	}
	if err := self.check(write); err != nil {
		return err
	}
	current, err := self.openChild(relative, false)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.ELOOP) {
			err = errors.Join(ErrIdentity, err)
		}
		return err
	}
	want, wantErr := current.Stat()
	got, gotErr := directory.Stat()
	closeErr := unavailableObservation("durable descendant could not be closed", current.Close())
	if gotErr != nil {
		return errors.Join(unavailableObservation("borrowed durable descriptor could not be observed", gotErr), closeErr)
	}
	if wantErr != nil {
		return errors.Join(&UnavailableError{Reason: "durable descendant metadata could not be observed"}, wantErr, closeErr)
	}
	if !os.SameFile(want, got) || want.Mode() != got.Mode() {
		return errors.Join(ErrIdentity, errors.New("durable descendant descriptor changed"), closeErr)
	}
	if err := sameNamedFileObserved(directory, filepath.Join(self.rootPath, relative), self.observeFile); err != nil {
		return errors.Join(err, closeErr)
	}
	return errors.Join(closeErr, self.check(write))
}

// A compile-time assertion keeps the public host facts narrow and explicit.
var _ Host = linuxHost{}
