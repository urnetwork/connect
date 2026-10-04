//go:build linux

// Restore planning authenticates a stopped source archive before staging exact
// old payloads. Only fixed adapters may compute new physical checkpoint bytes;
// the existing accepted-plan publisher owns every target mutation and recovery.
package durablevolume

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// Restore is a new empty target with known historical source, never a renamed
// fresh request. Shared coverage is explicit; omission retains the old profile.
func (self PreparationRequest) validateRestore() error {
	if self.Purpose == "fresh" {
		if self.RestoreSource != nil {
			return errors.New("fresh preparation cannot contain retained restore authority")
		}
		for _, owner := range self.Owners {
			if owner.RestoreCoverage != "" {
				return errors.New("fresh preparation cannot select retained owner coverage")
			}
		}
		return nil
	}
	if self.Purpose != "restore" || self.RestoreSource == nil || len(self.Owners) == 0 {
		return errors.New("restore requires fixed owner coverage and an explicit complete source archive")
	}
	for _, owner := range self.Owners {
		if owner.RestoreCoverage != "" && owner.RestoreCoverage != PreparationCompleteUnion || len(self.Owners) != 1 && owner.RestoreCoverage != PreparationCompleteUnion {
			return errors.New("shared restore coverage requires the explicit complete-union profile for every owner")
		}
	}
	source := self.RestoreSource
	if !canonical(source.Directory) || !canonical(source.Inventory.Path) || !validDigest(source.Inventory.Sha256) ||
		!canonical(source.FormerWriterFence.Path) || !validDigest(source.FormerWriterFence.Sha256) || source.Inventory.Path == source.FormerWriterFence.Path {
		return errors.New("restore source requires protected archive, inventory and stopped-source references")
	}
	for _, path := range []string{self.RootPath, self.StagingDirectory, self.MarkerPath, self.LeasePath, self.ControlPath, self.DeclarationPath, self.FormerWriterFence.Path, source.Inventory.Path, source.FormerWriterFence.Path} {
		if beneath(source.Directory, path) || beneath(path, source.Directory) {
			return errors.New("restore archive overlaps target, staging or external authority")
		}
	}
	for _, path := range []string{source.Inventory.Path, source.FormerWriterFence.Path} {
		if beneath(self.RootPath, path) || beneath(self.StagingDirectory, path) {
			return errors.New("restore authority must remain outside target and staging")
		}
	}
	return nil
}

// Pure restore layout and semantic readback are distinct from fresh builders.
func preparationAdapterAdmission(request PreparationRequest, adapter PreparationAdapter) error {
	if request.Purpose == "restore" {
		if adapter.Restore == nil || adapter.InspectRestore == nil {
			return errors.New("fixed retained-source and restore checkpoint adapters are required")
		}
	} else if adapter.Build == nil || adapter.Inspect == nil {
		return errors.New("fixed fresh preparation adapters are required")
	}
	return nil
}

// The original report and separately retained stop assertion remain available
// on every apply/retry. A copied archive is not treated as the original inode.
func readPreparationRestoreInventory(ctx context.Context, request PreparationRequest) (Inventory, error) {
	if request.Purpose != "restore" {
		return Inventory{}, errors.New("physical source read requires explicit restore purpose")
	}
	if err := request.validateRestore(); err != nil {
		return Inventory{}, err
	}
	source := request.RestoreSource
	report, err := LoadPhysicalInventory(ctx, source.Inventory)
	if err != nil {
		return Inventory{}, err
	}
	var fence FormerWriterFence
	if err := readReference(ctx, source.FormerWriterFence, maximumConfigBytes, &fence); err != nil {
		return Inventory{}, err
	}
	if fence.Schema != FormerWriterFenceSchema || fence.RootPath != report.StateRoot.Path || fence.DeclarationSha256 != report.Declaration.Sha256 ||
		fence.LeaseSha256 != report.StateRoot.LeaseSha256 || !fence.FormerWritersStopped || strings.TrimSpace(fence.Evidence) == "" || len(fence.Evidence) > 4096 {
		return Inventory{}, errors.New("restore source stop assertion differs from original exported custody")
	}
	limits := request.Limits
	if uint64(len(report.Entries)) > limits.MaxEntries || report.TotalBytes > limits.MaxBytes ||
		report.TotalOwnerAttributes > limits.MaxOwnerAttributes || report.TotalOwnerAttributeBytes > limits.MaxOwnerAttributeBytes {
		return Inventory{}, errors.New("restore source exceeds the explicitly reviewed target capacities")
	}
	for _, entry := range report.Entries {
		if entry.Path != "" && !preparationRelative(entry.Path, limits.MaxDepth, false) {
			return Inventory{}, errors.New("restore source exceeds reviewed target depth")
		}
	}
	return report, ctx.Err()
}

// A fixed adapter must account for every old member and owner checkpoint.
// The old base-preparation anchor is retained in the reviewed inventory; the
// new target gets a separate plan/control anchor, never a copy of that writer.
func validatePreparationRestoreOwner(report Inventory, owner PreparationOwnerPlan) error {
	if owner.Owner.Purpose != "restore" || owner.Owner.RelativePath != "." || len(owner.Census) == 0 {
		return errors.New("restore adapter lacks its exact retained semantic census")
	}
	files := map[string]PreparationFile{}
	attributes := map[PreparationAttributeSpec]bool{}
	for _, entry := range report.Entries {
		if entry.Path != "" {
			files[entry.Path] = PreparationFile{Path: entry.Path, Kind: entry.Kind, Mode: entry.Mode, Bytes: entry.Size, Sha256: entry.Sha256}
		}
		for _, attribute := range entry.OwnerAttributes {
			if entry.Path == "" && attribute.Name == PreparationAttribute {
				continue
			}
			path := entry.Path
			if path == "" {
				path = "."
			}
			attributes[PreparationAttributeSpec{Path: path, Name: attribute.Name}] = true
		}
	}
	if len(files) != len(owner.Files) || len(attributes) != len(owner.Attributes) {
		return errors.New("restore adapter omitted or invented retained members or checkpoints")
	}
	for _, file := range owner.Files {
		if expected, ok := files[file.Path]; !ok || expected != file {
			return errors.New("restore adapter changed original portable member bytes")
		}
		delete(files, file.Path)
	}
	for _, attribute := range owner.Attributes {
		if !attributes[attribute] {
			return errors.New("restore adapter changed the original owner checkpoint destinations")
		}
		delete(attributes, attribute)
	}
	return nil
}

// Every source file and protocol head has exactly one fixed owner. Views may
// omit unrelated names only when this complete union independently covers them;
// duplicate ownership is refused, never collapsed by map assignment.
func validatePreparationRestoreCoverage(report Inventory, owners []PreparationOwnerPlan) error {
	if len(owners) == 1 && owners[0].Owner.RestoreCoverage == "" {
		return validatePreparationRestoreOwner(report, owners[0])
	}
	files := map[string]PreparationFile{}
	attributes := map[PreparationAttributeSpec]bool{}
	for _, entry := range report.Entries {
		if entry.Path != "" {
			if _, found := files[entry.Path]; found {
				return errors.New("restore coverage source repeats an original member")
			}
			files[entry.Path] = PreparationFile{Path: entry.Path, Kind: entry.Kind, Mode: entry.Mode, Bytes: entry.Size, Sha256: entry.Sha256}
		}
		for _, attribute := range entry.OwnerAttributes {
			if entry.Path == "" && attribute.Name == PreparationAttribute {
				continue
			}
			path := entry.Path
			if path == "" {
				path = "."
			}
			key := PreparationAttributeSpec{Path: path, Name: attribute.Name}
			if attributes[key] {
				return errors.New("restore coverage source repeats an original checkpoint")
			}
			attributes[key] = true
		}
	}
	for _, owner := range owners {
		if owner.Owner.Purpose != "restore" || owner.Owner.RelativePath != "." || owner.Owner.RestoreCoverage != PreparationCompleteUnion || len(owner.Census) == 0 || owner.ExclusiveRoot && len(owners) != 1 {
			return errors.New("restore coverage changed its fixed owner scope or overlaps an exclusive root")
		}
		for _, file := range owner.Files {
			if original, found := files[file.Path]; !found || original != file {
				return errors.New("restore coverage overlaps, invents or changes an original member")
			}
			delete(files, file.Path)
		}
		for _, attribute := range owner.Attributes {
			if !attributes[attribute] {
				return errors.New("restore coverage overlaps or invents an original checkpoint")
			}
			delete(attributes, attribute)
		}
	}
	if len(files) != 0 || len(attributes) != 0 {
		return errors.New("restore coverage omitted original members or owner checkpoints")
	}
	return nil
}

// One planner holds a read-only archive descriptor and exclusive local flock.
// The external stop assertion covers historical writers outside this lock.
// Observations are finite metadata maps, not one descriptor per retained file.
type preparationRestoreArchive struct {
	ctx       context.Context
	host      Host
	path      string
	root      *os.File
	identity  PreparationIdentity
	mount     Mount
	inventory Inventory
	entries   map[string]InventoryEntry
	observed  map[string]syscall.Stat_t
}

// Admission is completed before staging any restored owner payload.
func openPreparationRestoreArchive(ctx context.Context, request PreparationRequest, host Host) (_ *preparationRestoreArchive, resultErr error) {
	report, err := readPreparationRestoreInventory(ctx, request)
	if err != nil {
		return nil, err
	}
	root, err := preparationOpenAbsolute(request.RestoreSource.Directory, true)
	if err != nil {
		return nil, err
	}
	self := &preparationRestoreArchive{ctx: ctx, host: host, path: request.RestoreSource.Directory, root: root, inventory: report,
		entries: map[string]InventoryEntry{}, observed: map[string]syscall.Stat_t{}}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, self.close())
		}
	}()
	if err := syscall.Flock(int(root.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, errors.Join(ErrBusy, err)
	}
	self.identity, err = preparationIdentity(root)
	if err != nil {
		return nil, err
	}
	self.mount, err = self.mountFact()
	if err != nil {
		return nil, err
	}
	for _, entry := range report.Entries {
		self.entries[entry.Path] = entry
	}
	if err := self.walk(true); err != nil {
		return nil, err
	}
	if err := self.check(); err != nil {
		return nil, err
	}
	return self, nil
}

// The selected archive view may be on a different device from the target,
// while nested mount changes inside it are refused, including bind mounts.
func (self *preparationRestoreArchive) mountFact() (Mount, error) {
	mounts, err := self.host.Mounts()
	if err != nil {
		return Mount{}, unavailableObservation("restore archive mounts could not be observed", err)
	}
	if len(mounts) > 8192 {
		return Mount{}, errors.New("restore archive mount observation exceeds the finite bound")
	}
	var selected Mount
	for _, mount := range mounts {
		if beneath(self.path, mount.Path) && mount.Path != self.path {
			return Mount{}, errors.Join(ErrIdentity, errors.New("restore archive contains a nested mount"))
		}
		if beneath(mount.Path, self.path) {
			if mount.Path == selected.Path {
				return Mount{}, errors.Join(ErrIdentity, errors.New("restore archive has ambiguous physical mount views"))
			}
			if len(mount.Path) > len(selected.Path) {
				selected = mount
			}
		}
	}
	if selected.Path == "" || selected.Device != deviceNumber(self.identity.Device) {
		return Mount{}, errors.Join(ErrIdentity, errors.New("restore archive physical mount differs"))
	}
	return selected, nil
}

// Relative traversal borrows at most two descriptors at each step and never
// follows a symlink or creates a missing archived member.
func (self *preparationRestoreArchive) open(relative string, directory bool) (*os.File, error) {
	if relative != "" && !preparationRelative(relative, self.inventory.Limits.MaxDepth, false) {
		return nil, errors.New("restore member path is outside the reviewed source")
	}
	fd, err := syscall.Openat(int(self.root.Fd()), ".", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, unavailableObservation("restore archive descriptor could not be borrowed", err)
	}
	file := os.NewFile(uintptr(fd), self.path)
	parts := []string{}
	if relative != "" {
		parts = strings.Split(relative, "/")
	}
	for index, part := range parts {
		if err := self.ctx.Err(); err != nil {
			return nil, errors.Join(err, file.Close())
		}
		isDirectory := index < len(parts)-1 || directory
		flags := syscall.O_RDONLY | syscall.O_CLOEXEC | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
		if isDirectory {
			flags |= syscall.O_DIRECTORY
		}
		next, openErr := syscall.Openat(int(file.Fd()), part, flags, 0)
		path := filepath.Join(file.Name(), part)
		closeErr := file.Close()
		if openErr != nil {
			return nil, errors.Join(namedObservation("restore archived member could not be opened", openErr), closeErr)
		}
		file = os.NewFile(uintptr(next), path)
		if err := errors.Join(closeErr, preparationPrivate(file, isDirectory)); err != nil {
			return nil, errors.Join(err, file.Close())
		}
	}
	return file, nil
}

// Every source checkpoint value is retained byte-for-byte. Unknown protocol
// metadata cannot disappear from a plausible restore by filtering a name list.
func (self *preparationRestoreArchive) attributes(file *os.File, entry InventoryEntry) error {
	want := map[string][]byte{}
	for _, attribute := range entry.OwnerAttributes {
		want[attribute.Name] = attribute.Value
	}
	if entry.Path == "" {
		raw, err := hex.DecodeString(self.inventory.RootGeneration)
		if err != nil {
			return err
		}
		want[RootGenerationAttribute] = raw
	}
	names, err := listInventoryAttributes(file)
	if err != nil {
		return err
	}
	for _, name := range names {
		if !strings.HasPrefix(name, ownerAttributeNamespace) {
			continue
		}
		value, exists := want[name]
		if !exists {
			return errors.Join(ErrIdentity, errors.New("restore archive has an unreviewed owner attribute"))
		}
		actual, err := readInventoryAttribute(file, name, 4096)
		if err != nil {
			return err
		}
		if !bytes.Equal(value, actual) {
			return errors.Join(ErrIdentity, errors.New("restore archive checkpoint bytes differ"))
		}
		delete(want, name)
	}
	if len(want) != 0 {
		return errors.Join(ErrIdentity, errors.New("restore archive is missing original owner authority"))
	}
	return self.ctx.Err()
}

// Only metadata is revisited after the one bounded source copy; unchanged
// payloads are not repeatedly hashed for each member publication.
func (self *preparationRestoreArchive) walk(enroll bool) error {
	visited := map[string]bool{}
	var visit func(string) error
	visit = func(relative string) (resultErr error) {
		if err := self.ctx.Err(); err != nil {
			return err
		}
		entry, exists := self.entries[relative]
		if !exists || visited[relative] {
			return errors.Join(ErrIdentity, errors.New("restore archive has an unreviewed member"))
		}
		visited[relative] = true
		file, err := self.open(relative, entry.Kind == "directory")
		if err != nil {
			return err
		}
		defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
		before, err := inventoryStat(file)
		if err != nil {
			return unavailableObservation("restore archive metadata could not be observed", err)
		}
		kind := uint32(syscall.S_IFREG)
		if entry.Kind == "directory" {
			kind = syscall.S_IFDIR
		}
		if before.Dev != self.identity.Device || before.Mode&syscall.S_IFMT != kind || before.Mode&07777 != entry.Mode ||
			entry.Kind == "file" && (before.Size < 0 || uint64(before.Size) != entry.Size || before.Nlink != 1) {
			return errors.Join(ErrIdentity, errors.New("restore archive member metadata differs"))
		}
		if !enroll && !unchangedInventoryStat(self.observed[relative], before) {
			return errors.Join(ErrIdentity, errors.New("restore source generation changed during planning"))
		}
		if err := self.attributes(file, entry); err != nil {
			return err
		}
		if entry.Kind == "directory" {
			for {
				names, err := file.Readdirnames(128)
				if err != nil && err != io.EOF {
					return unavailableObservation("restore directory enumeration failed", err)
				}
				for _, name := range names {
					if err := visit(filepath.Join(relative, name)); err != nil {
						return err
					}
				}
				if err == io.EOF {
					break
				}
			}
		}
		after, err := inventoryStat(file)
		if err != nil {
			return unavailableObservation("restore archive metadata could not be reobserved", err)
		}
		if !unchangedInventoryStat(before, after) {
			return errors.Join(ErrIdentity, errors.New("restore archive member changed during observation"))
		}
		if err := sameNamedFile(file, filepath.Join(self.path, relative)); err != nil {
			return err
		}
		if enroll {
			self.observed[relative] = after
		}
		return nil
	}
	if err := visit(""); err != nil {
		return err
	}
	if len(visited) != len(self.entries) {
		return errors.Join(ErrIdentity, errors.New("restore archive is missing retained members"))
	}
	return self.ctx.Err()
}

// The complete selected mount and root name remain the same before and after
// cold source copying; observation failures do not assert proven custody loss.
func (self *preparationRestoreArchive) checkRoot() error {
	if err := errors.Join(self.ctx.Err(), sameNamedFile(self.root, self.path)); err != nil {
		return err
	}
	actual, err := self.mountFact()
	if err != nil {
		return err
	}
	if actual != self.mount {
		return errors.Join(ErrIdentity, errors.New("restore archive mount generation changed"))
	}
	return nil
}

// A full member/attribute census is separate from the constant-size root
// checks used during target publication. It never rereads immutable payloads.
func (self *preparationRestoreArchive) check() error {
	if err := self.checkRoot(); err != nil {
		return err
	}
	return self.walk(false)
}

// Apply cannot substitute its staged copy for lost original archive custody.
// Hash each copied original once at admission, under its retained source flock,
// and keep its names, physical generation and owner attributes until close.
func (self *preparationRestoreArchive) authenticate(hooks *preparationHooks) error {
	for _, entry := range self.inventory.Entries {
		if entry.Kind != "file" {
			continue
		}
		file, err := self.open(entry.Path, false)
		if err != nil {
			return err
		}
		var read func(int)
		if hooks != nil && hooks.sourceRead != nil {
			read = func(n int) { hooks.sourceRead(filepath.Join(self.path, entry.Path), n) }
		}
		readErr := preparationVerifyFileWithRead(self.ctx, file, entry.Size, entry.Sha256, read)
		if err := errors.Join(readErr, file.Close()); err != nil {
			return err
		}
	}
	return self.check()
}

// Completed target progress remains usable only with the same reviewed source.
// This check is deliberately outside per-member mutation loops.
func (self *preparationApply) checkRestore() error {
	if self.archive == nil {
		return nil
	}
	return self.archive.check()
}

// Staging copies only complete, reviewed payloads and never old physical
// checkpoint attributes. Every original checkpoint remains in the inventory.
func (self *preparationRestoreArchive) stage(parent *os.File, name string, owner PreparationOwnerPlan) (resultErr error) {
	if err := self.check(); err != nil {
		return err
	}
	if name == "" || filepath.Base(name) != name || name == "." || name == ".." {
		return errors.New("restore staging requires one private named namespace")
	}
	if err := syscall.Mkdirat(int(parent.Fd()), name, 0700); err != nil {
		return err
	}
	fd, err := syscall.Openat(int(parent.Fd()), name, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	stage := os.NewFile(uintptr(fd), filepath.Join(parent.Name(), name))
	defer func() { resultErr = errors.Join(resultErr, stage.Close()) }()
	if err := preparationPrivate(stage, true); err != nil {
		return err
	}
	files := append([]PreparationFile(nil), owner.Files...)
	sort.Slice(files, func(i, j int) bool {
		a, b := files[i].Path, files[j].Path
		if strings.Count(a, "/") != strings.Count(b, "/") {
			return strings.Count(a, "/") < strings.Count(b, "/")
		}
		return a < b
	})
	for _, member := range files {
		if err := self.ctx.Err(); err != nil {
			return err
		}
		if err := self.stageMember(stage, member); err != nil {
			return err
		}
	}
	return errors.Join(stage.Sync(), parent.Sync(), sameNamedFile(stage, stage.Name()), self.check())
}

// Target parents are newly created staging-only paths. Reads and writes are
// chunked, with original named generation and digest checked before admission.
func (self *preparationRestoreArchive) stageMember(stage *os.File, member PreparationFile) (resultErr error) {
	relativeParent := filepath.Dir(member.Path)
	if relativeParent == "." {
		relativeParent = ""
	}
	view := &preparationRestoreArchive{ctx: self.ctx, root: stage, path: stage.Name(), inventory: self.inventory}
	parent, err := view.open(relativeParent, true)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, parent.Close()) }()
	name := filepath.Base(member.Path)
	if member.Kind == "directory" {
		if err := syscall.Mkdirat(int(parent.Fd()), name, 0700); err != nil {
			return err
		}
		child, err := view.open(member.Path, true)
		if err != nil {
			return err
		}
		return errors.Join(child.Sync(), sameNamedFile(child, child.Name()), child.Close(), parent.Sync())
	}
	source, err := self.open(member.Path, false)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, source.Close()) }()
	before, err := inventoryStat(source)
	if err != nil {
		return unavailableObservation("restore source file could not be observed", err)
	}
	if !unchangedInventoryStat(self.observed[member.Path], before) {
		return errors.Join(ErrIdentity, errors.New("restore source file changed before copying"))
	}
	fd, err := syscall.Openat(int(parent.Fd()), name, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	target := os.NewFile(uintptr(fd), filepath.Join(parent.Name(), name))
	defer func() { resultErr = errors.Join(resultErr, target.Close()) }()
	hasher := sha256.New()
	buffer := make([]byte, 64*1024)
	for offset := uint64(0); offset < member.Bytes; {
		if err := self.ctx.Err(); err != nil {
			return err
		}
		chunk := buffer[:min(uint64(len(buffer)), member.Bytes-offset)]
		n, err := source.ReadAt(chunk, int64(offset))
		if err != nil || n != len(chunk) {
			return errors.Join(unavailableObservation("restore source exact read failed", err), io.ErrUnexpectedEOF)
		}
		if err := self.ctx.Err(); err != nil {
			return err
		}
		if n, err := target.Write(chunk); err != nil || n != len(chunk) {
			return errors.Join(err, io.ErrShortWrite)
		}
		_, _ = hasher.Write(chunk)
		offset += uint64(len(chunk))
	}
	after, err := inventoryStat(source)
	if err != nil {
		return unavailableObservation("restore source file could not be reobserved", err)
	}
	if !unchangedInventoryStat(before, after) || "sha256:"+hex.EncodeToString(hasher.Sum(nil)) != member.Sha256 {
		return errors.Join(ErrIdentity, errors.New("restore payload changed or differs from original exported bytes"))
	}
	// Immutable evidence stays read-only after its private staging write. The
	// original reviewed mode is part of custody, including retry after sync.
	if err := target.Chmod(os.FileMode(member.Mode)); err != nil {
		return err
	}
	return errors.Join(self.ctx.Err(), sameNamedFile(source, filepath.Join(self.path, member.Path)), sameNamedFile(target, target.Name()), target.Sync(), parent.Sync())
}

// Closing the synchronous planner releases only this archive, never its source.
func (self *preparationRestoreArchive) close() error {
	return self.root.Close()
}
