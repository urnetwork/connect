//go:build linux || darwin

// Planning retains protected physical descriptors and hashes bounded public
// staging bytes. It never enrolls a target, changes old custody or starts work.
package durablevolume

import (
	"github.com/urnetwork/connect/durablesys"

	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const maximumPreparationRequestBytes = 1024 * 1024
const maximumPreparationPlanBytes = 8 * 1024 * 1024
const maximumPreparationControlBytes = 64 * 1024 * 1024
const maximumPreparationControlRecordBytes = 64 * 1024

// The complete request remains hash-bound even if JSON whitespace differs.
func preparationDigest(raw []byte) string {
	digest := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(digest[:])
}

// The whole original request remains byte-exact. Only embedded owner inputs
// receive the same canonical whitespace on both sides of plan serialization.
func decodePreparationRequest(raw []byte, request *PreparationRequest) error {
	if err := decodeStrict(raw, request); err != nil {
		return err
	}
	for index := range request.Owners {
		var normalized bytes.Buffer
		if err := json.Compact(&normalized, request.Owners[index].Inputs); err != nil {
			return err
		}
		request.Owners[index].Inputs = append(json.RawMessage(nil), normalized.Bytes()...)
	}
	return nil
}

// Reference reads are finite, cancellable and protected through every ancestor.
func preparationReadReference(ctx context.Context, reference Reference, maximum int, label string) ([]byte, error) {
	if ctx == nil || !canonical(reference.Path) || !validDigest(reference.Sha256) {
		return nil, fmt.Errorf("%s path and exact hash are required", label)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := openProtectedFile(reference.Path)
	if err != nil {
		return nil, err
	}
	raw, readErr := boundedProtectedReadContext(ctx, file, maximum)
	if err := errors.Join(readErr, sameNamedFile(file, reference.Path), file.Close()); err != nil {
		return nil, err
	}
	if preparationDigest(raw) != reference.Sha256 {
		return nil, fmt.Errorf("%s bytes differ from the accepted hash", label)
	}
	return raw, nil
}

// Fresh roots are either already private or explicitly created through one
// reviewed staged inode. Retained/restore never fall back to either fresh mode.
func (self PreparationRequest) validate(scope ownerScope) error {
	expectedScope := "daemon"
	if scope == ownerLocalScope {
		expectedScope = "owner-local"
	}
	if self.Schema != PreparationRequestSchema || self.Scope != expectedScope || self.Purpose != "fresh" && self.Purpose != "restore" {
		return errors.New("storage preparation requires an explicit scope and supported fresh or restore purpose")
	}
	if err := self.validateRestore(); err != nil {
		return err
	}
	if self.RootCreation != "" && self.RootCreation != "create-private" {
		return errors.New("preparation root creation profile is unsupported")
	}
	limit := self.Limits
	maximumOwners, maximumOwnerAttributes := 32, uint64(128)
	switch self.CapacityProfile {
	case "":
	case "urnetwork-preparation-many-owners-v1":
		// Counts are explicit opt-in bounds, not a larger allocation or a
		// bypass of the request, plan, per-attribute or control byte limits.
		maximumOwners, maximumOwnerAttributes = 2048, 2048
	default:
		return errors.New("storage preparation capacity profile is unsupported")
	}
	maximumEntries, maximumDepth := uint64(10000), uint64(16)
	if self.Purpose == "restore" {
		maximumEntries = MaximumPhysicalInventoryEntries
		// Restore must represent the complete namespace already admitted by
		// physical inventory. The reviewed request still chooses its exact
		// finite depth; fresh preparation keeps its existing smaller profile.
		maximumDepth = 32
	}
	if limit.MaxEntries == 0 || limit.MaxEntries > maximumEntries || limit.MaxBytes == 0 || limit.MaxBytes > 1024*1024*1024*1024 ||
		limit.MaxDepth == 0 || limit.MaxDepth > maximumDepth || limit.MaxOwnerAttributes == 0 || limit.MaxOwnerAttributes > maximumOwnerAttributes ||
		limit.MaxOwnerAttributeBytes == 0 || limit.MaxOwnerAttributeBytes > maximumOwnerAttributes*4096 || limit.MaxPlanBytes < 4096 || limit.MaxPlanBytes > maximumPreparationPlanBytes ||
		len(self.Owners) == 0 || len(self.Owners) > maximumOwners {
		return errors.New("storage preparation capacities are absent or exceed the finite profile")
	}
	// Reuse daemon/owner-local declaration validation without enrolling any
	// marker, lease or nonce. These placeholder hashes grant no runtime owner.
	zeroHash := preparationDigest(nil)
	config := self.declaration(1, zeroHash, zeroHash, zeroHash)
	if err := config.validateForScope(scope); err != nil {
		return err
	}
	paths := []string{self.RootPath, self.StagingDirectory, self.MarkerPath, self.LeasePath, self.DeclarationPath, self.ControlPath, self.FormerWriterFence.Path}
	seen := map[string]bool{}
	for _, path := range paths {
		if !canonical(path) || !beneath(self.MountPath, path) || path == self.MountPath || seen[path] {
			return errors.New("preparation paths are absent, aliased or outside the selected mount")
		}
		seen[path] = true
	}
	if !validDigest(self.FormerWriterFence.Sha256) || beneath(self.RootPath, self.StagingDirectory) || beneath(self.StagingDirectory, self.RootPath) {
		return errors.New("preparation staging or former-writer reference is invalid")
	}
	for _, path := range paths[2:] {
		if beneath(self.RootPath, path) || beneath(self.StagingDirectory, path) {
			return errors.New("preparation metadata must be outside target and staging namespaces")
		}
	}
	for _, owner := range self.Owners {
		if owner.Kind == "" || len(owner.Kind) > 128 || owner.RelativePath != "." || owner.Purpose != self.Purpose || len(owner.Inputs) == 0 || len(owner.Inputs) > maximumPreparationRequestBytes {
			if self.Purpose == "fresh" {
				return errors.New("preparation owner requires a fixed fresh kind at the precreated root")
			}
			return errors.New("preparation owner requires its exact fixed kind and declared purpose")
		}
	}
	return nil
}

// Declaration publication is last; the result still grants no restart.
func (self PreparationRequest) declaration(inode uint64, generation, marker, lease string) Config {
	schema := Schema
	if self.Scope == "owner-local" {
		schema = OwnerLocalSchema
	}
	return Config{Schema: schema, Volumes: []VolumeSpec{{MountPath: self.MountPath, FilesystemUuid: self.FilesystemUuid, FilesystemType: self.FilesystemType,
		MarkerPath: self.MarkerPath, MarkerSha256: marker, MinAvailableBytes: self.MinAvailableBytes, MinAvailableInodes: self.MinAvailableInodes,
		StateRoots: []StateRootSpec{{Path: self.RootPath, LeasePath: self.LeasePath, LeaseSha256: lease, RootInode: inode, GenerationSha256: generation}}}}}
}

// One synchronous command owns these descriptors. Concurrent commands acquire
// independent nonblocking root leases; no mutable state is process-global.
type preparationAdmission struct {
	ctx          context.Context
	request      PreparationRequest
	scope        ownerScope
	host         Host
	root         *os.File
	rootSource   string
	rootAtTarget bool
	directories  map[string]*os.File
	identities   map[string]PreparationIdentity
	mount        Mount
	filesystem   Filesystem
}

// Only genuinely private caller-owned preparation targets and metadata are
// admitted; group-writable test directories do not receive an exception.
func preparationPrivate(file *os.File, directory bool) error {
	if err := protected(file, directory); err != nil {
		return err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return unavailableObservation("preparation protection could not be observed", err)
	}
	if stat.Mode&0077 != 0 || stat.Uid != uint32(os.Geteuid()) {
		return errors.Join(ErrIdentity, errors.New("preparation path must be private and owned by the applying identity"))
	}
	return nil
}

// Inode facts come from a retained descriptor, never a pathname-derived claim.
func preparationIdentity(file *os.File) (PreparationIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return PreparationIdentity{}, unavailableObservation("preparation inode could not be observed", err)
	}
	return PreparationIdentity{Device: durablesys.StatDevice(&stat), Inode: stat.Ino, Mode: uint32(stat.Mode), Uid: stat.Uid, Gid: stat.Gid}, nil
}

// Mount selection is identical to runtime admission and independently covers
// every staging/metadata parent, including nested mounts outside the target.
func (self *preparationAdmission) facts() (Mount, Filesystem, error) {
	request := self.request
	probe := &Owner{host: self.host, scope: self.scope, rootPath: request.RootPath,
		spec:     VolumeSpec{MountPath: request.MountPath, FilesystemUuid: request.FilesystemUuid, FilesystemType: request.FilesystemType, MarkerPath: request.MarkerPath},
		rootSpec: StateRootSpec{LeasePath: request.LeasePath}}
	mount, err := probe.mountFacts()
	if err != nil {
		return Mount{}, Filesystem{}, err
	}
	mounts, err := self.host.Mounts()
	if err != nil {
		return Mount{}, Filesystem{}, unavailableObservation("preparation mounts could not be observed", err)
	}
	if len(mounts) > 8192 {
		return Mount{}, Filesystem{}, errors.Join(ErrIdentity, errors.New("preparation mount census exceeds its bound"))
	}
	for path := range self.directories {
		for _, other := range mounts {
			if other.Path != mount.Path && beneath(mount.Path, other.Path) && beneath(other.Path, path) {
				return Mount{}, Filesystem{}, errors.Join(ErrIdentity, errors.New("another mount covers preparation custody"))
			}
		}
	}
	probeFile := self.root
	if probeFile == nil {
		probeFile = self.directories[filepath.Dir(request.RootPath)]
	}
	filesystem, err := self.host.Filesystem(probeFile)
	if err != nil {
		return Mount{}, Filesystem{}, unavailableObservation("preparation filesystem could not be observed", err)
	}
	if filesystem.Type != filesystemMagic(request.FilesystemType) {
		return Mount{}, Filesystem{}, errors.Join(ErrIdentity, errors.New("preparation descriptor filesystem type differs"))
	}
	if mount.ReadOnly || filesystem.ReadOnly || filesystem.AvailableBytes < request.MinAvailableBytes || filesystem.AvailableInodes < request.MinAvailableInodes {
		return Mount{}, Filesystem{}, &UnavailableError{Reason: "preparation write reserve is unavailable"}
	}
	return mount, filesystem, nil
}

// Every parent remains precreated. A missing target is admitted only by the
// explicit create-private profile and exact staged source, never runtime fallback.
func openPreparationAdmission(ctx context.Context, request PreparationRequest, host Host, scope ownerScope, rootSource string) (_ *preparationAdmission, resultErr error) {
	return openPreparationAdmissionWithParents(ctx, request, host, scope, rootSource, nil)
}

// A synchronous cohort may share the same retained private-parent description
// for sibling root moves. Duplicates share its flock; unrelated owners do not.
func openPreparationAdmissionWithParents(ctx context.Context, request PreparationRequest, host Host, scope ownerScope, rootSource string, sharedParents map[string]*os.File) (_ *preparationAdmission, resultErr error) {
	if ctx == nil || host == nil {
		return nil, errors.New("preparation context and host are required")
	}
	if err := errors.Join(ctx.Err(), request.validate(scope)); err != nil {
		return nil, err
	}
	self := &preparationAdmission{ctx: ctx, request: request, scope: scope, host: host, directories: map[string]*os.File{}, identities: map[string]PreparationIdentity{}}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, self.close())
		}
	}()
	paths := []string{request.RootPath, request.StagingDirectory, filepath.Dir(request.MarkerPath), filepath.Dir(request.LeasePath), filepath.Dir(request.DeclarationPath), filepath.Dir(request.ControlPath), filepath.Dir(request.FormerWriterFence.Path)}
	if request.RootCreation == "create-private" {
		paths[0] = filepath.Dir(request.RootPath)
	}
	for _, path := range paths {
		if self.directories[path] != nil {
			continue
		}
		var file *os.File
		var err error
		if original := sharedParents[path]; original != nil {
			var fd int
			fd, err = syscall.Dup(int(original.Fd()))
			if err == nil {
				syscall.CloseOnExec(fd)
				file = os.NewFile(uintptr(fd), path)
			}
		} else {
			file, err = openPhysicalDirectory(path)
		}
		if err != nil {
			return nil, err
		}
		self.directories[path] = file
		if err := preparationPrivate(file, true); err != nil {
			return nil, err
		}
		identity, err := preparationIdentity(file)
		if err != nil {
			return nil, err
		}
		self.identities[path] = identity
	}
	if request.RootCreation == "create-private" {
		parent := self.directories[filepath.Dir(request.RootPath)]
		if err := syscall.Flock(int(parent.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			return nil, errors.Join(ErrBusy, err)
		}
		if rootSource == "" {
			if err := preparationAbsent(parent, filepath.Base(request.RootPath)); err != nil {
				return nil, err
			}
		} else if err := self.openStagedRoot(rootSource); err != nil {
			return nil, err
		}
	} else {
		if rootSource != "" {
			return nil, errors.New("precreated preparation cannot select a staged root")
		}
		self.root = self.directories[request.RootPath]
	}
	if self.root != nil {
		if err := syscall.Flock(int(self.root.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			return nil, errors.Join(ErrBusy, err)
		}
	}
	var err error
	self.mount, self.filesystem, err = self.facts()
	if err != nil {
		return nil, err
	}
	if err := self.check(); err != nil {
		return nil, err
	}
	if sharedParents != nil && request.RootCreation == "create-private" {
		parentPath := filepath.Dir(request.RootPath)
		if sharedParents[parentPath] == nil {
			sharedParents[parentPath] = self.directories[parentPath]
		}
	}
	return self, nil
}

// Rechecks run before every mutation and after sync. Observation failures stay
// unavailable; a successful identity comparison can prove changed custody.
func (self *preparationAdmission) check() error {
	if err := self.ctx.Err(); err != nil {
		return err
	}
	mount, filesystem, err := self.facts()
	if err != nil {
		return err
	}
	if mount != self.mount || filesystem.Id != self.filesystem.Id || filesystem.Type != self.filesystem.Type {
		return errors.Join(ErrIdentity, errors.New("preparation mount generation changed"))
	}
	for path, file := range self.directories {
		observedPath := path
		if path == self.request.RootPath && self.rootSource != "" && !self.rootAtTarget {
			observedPath = self.rootSource
		}
		if err := errors.Join(sameNamedFile(file, observedPath), preparationPrivate(file, true)); err != nil {
			return err
		}
		identity, err := preparationIdentity(file)
		if err != nil {
			return err
		}
		if identity != self.identities[path] || deviceNumber(identity.Device) != mount.Device {
			return errors.Join(ErrIdentity, errors.New("preparation named directory or filesystem changed"))
		}
	}
	if self.request.RootCreation == "create-private" {
		absent := self.request.RootPath
		if self.rootAtTarget {
			absent = self.rootSource
		}
		if err := preparationAbsent(self.directories[filepath.Dir(absent)], filepath.Base(absent)); err != nil {
			return err
		}
	}
	return self.ctx.Err()
}

// The synchronous invocation joins all retained directories and its root lease.
func (self *preparationAdmission) close() error {
	var result error
	for path, file := range self.directories {
		result = errors.Join(result, file.Close())
		delete(self.directories, path)
	}
	return result
}

// Context-bounded empty checks do not infer historical freshness. The external
// explicit fence and absence of any custody attribute are independent inputs.
func preparationEmpty(ctx context.Context, file *os.File) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	fd, err := unix.Openat(int(file.Fd()), ".", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	view := os.NewFile(uintptr(fd), file.Name())
	names, readErr := view.Readdirnames(1)
	if errors.Is(readErr, io.EOF) {
		readErr = nil
	}
	if err := errors.Join(readErr, view.Close(), ctx.Err()); err != nil {
		return err
	}
	if len(names) != 0 {
		return errors.Join(ErrIdentity, errors.New("fresh preparation target is not empty; existing custody cannot be reenrolled"))
	}
	return nil
}

// The fence is read every time; changing it does not renew a partially applied
// plan or change the original declaration's meaning.
func (self *preparationAdmission) fence() error {
	raw, err := preparationReadReference(self.ctx, self.request.FormerWriterFence, 64*1024, "former-writer fence")
	if err != nil {
		return err
	}
	var fence PreparationFence
	if err := decodeStrict(raw, &fence); err != nil {
		return err
	}
	fresh := self.request.Purpose == "fresh" && fence.NoPreviousOwnerState && !fence.NoPreviousTargetState
	restore := self.request.Purpose == "restore" && !fence.NoPreviousOwnerState && fence.NoPreviousTargetState
	if fence.Schema != PreparationFenceSchema || fence.RootPath != self.request.RootPath || !self.fenceRootMatches(fence) ||
		fence.Purpose != self.request.Purpose || !fence.FormerWritersStopped || !fresh && !restore || strings.TrimSpace(fence.Evidence) == "" || len(fence.Evidence) > 8192 {
		return errors.New("explicit target history and stopped former-writer evidence are required")
	}
	return nil
}

// Directory creation is staging-only here. Every returned source is fully
// read and bound independently of an adapter's claimed digest.
func planPreparation(ctx context.Context, reference Reference, adapter PreparationAdapter, host Host, scope ownerScope) (result PreparationPlan, resultErr error) {
	raw, err := preparationReadReference(ctx, reference, maximumPreparationRequestBytes, "request")
	if err != nil {
		return result, err
	}
	var request PreparationRequest
	if err := decodePreparationRequest(raw, &request); err != nil {
		return result, err
	}
	if err := preparationAdapterAdmission(request, adapter); err != nil {
		return result, err
	}
	admission, err := openPreparationAdmission(ctx, request, host, scope, "")
	if err != nil {
		return result, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, admission.close())
		if resultErr != nil {
			result = PreparationPlan{}
		}
	}()
	if err := errors.Join(admission.fence(), admission.freshRoot()); err != nil {
		return result, err
	}
	var archive *preparationRestoreArchive
	if request.Purpose == "restore" {
		archive, err = openPreparationRestoreArchive(ctx, request, host)
		if err != nil {
			return result, err
		}
		defer func() { resultErr = errors.Join(resultErr, archive.close()) }()
	}
	for _, path := range []string{request.MarkerPath, request.LeasePath, request.DeclarationPath, request.ControlPath} {
		if err := preparationAbsent(admission.directories[filepath.Dir(path)], filepath.Base(path)); err != nil {
			return result, err
		}
	}
	result = PreparationPlan{Schema: PreparationPlanSchema, Request: reference, RequestSha256: reference.Sha256, RequestBytes: append(json.RawMessage(nil), raw...), Root: admission.identities[request.RootPath], Directories: admission.identities, Mount: admission.mount, Filesystem: admission.filesystem, Nonce: make([]byte, 32), Generation: make([]byte, RootGenerationBytes), Marker: make([]byte, 32), Lease: make([]byte, 32), RestartAuthorized: false}
	if archive != nil {
		identity := archive.identity
		result.RestoreArchive = &identity
	}
	for _, value := range [][]byte{result.Nonce, result.Generation, result.Marker, result.Lease} {
		if _, err := rand.Read(value); err != nil {
			return result, err
		}
	}
	if request.RootCreation == "create-private" {
		if err := admission.stageRoot(result.Nonce); err != nil {
			return result, err
		}
		result.Root, result.RootSource = admission.identities[request.RootPath], admission.rootSource
	}
	for index, owner := range request.Owners {
		if err := admission.check(); err != nil {
			return result, err
		}
		name := fmt.Sprintf("preparation-%s-%02d", hex.EncodeToString(result.Nonce), index)
		var prepared PreparationOwnerPlan
		if archive == nil {
			prepared, err = adapter.Build(ctx, admission.directories[request.StagingDirectory], name, owner)
		} else {
			prepared, err = adapter.Restore(ctx, name, owner, archive.inventory)
		}
		if err != nil {
			return result, err
		}
		if !reflect.DeepEqual(prepared.Owner, owner) || prepared.StagingName != name {
			return result, errors.New("preparation adapter changed its exact public owner scope")
		}
		result.Owners = append(result.Owners, prepared)
		if prepared.PhysicalMetadata != nil && archive == nil {
			return result, errors.New("fresh preparation cannot derive retained physical metadata")
		}
	}
	if archive != nil {
		// Pure owner views must form a complete disjoint union before any
		// source member is staged. The original full report remains retained.
		if err := validatePreparationRestoreCoverage(archive.inventory, result.Owners); err != nil {
			return result, err
		}
		for index, owner := range result.Owners {
			if err := archive.stage(admission.directories[request.StagingDirectory], owner.StagingName, owner); err != nil {
				return result, err
			}
			if err := preparePhysicalMetadata(ctx, admission, adapter, archive.inventory, &result, index); err != nil {
				return result, err
			}
		}
	}
	if err := bindPreparationSources(ctx, request, &result, nil); err != nil {
		return result, err
	}
	if err := preparationControlCapacity(ctx, request, result); err != nil {
		return result, err
	}
	if err := errors.Join(admission.check(), admission.fence(), admission.freshRoot()); err != nil {
		return result, err
	}
	if archive != nil {
		if err := archive.check(); err != nil {
			return result, err
		}
	}
	encoded, err := json.Marshal(result)
	if err != nil || uint64(len(encoded)) > request.Limits.MaxPlanBytes {
		return result, errors.Join(errors.New("preparation plan exceeds its reviewed byte capacity"), err)
	}
	return result, nil
}

// Relative names cannot hide aliases, traversal, internal reservation paths or
// capacity expansion through an unbounded depth or component.
func preparationRelative(path string, maximumDepth uint64, allowRoot bool) bool {
	if path == "." {
		return allowRoot
	}
	if path == "" || filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\x00\n\r") || path == ".." || strings.HasPrefix(path, "../") {
		return false
	}
	parts := strings.Split(path, "/")
	if uint64(len(parts)) > maximumDepth {
		return false
	}
	for _, part := range parts {
		if len(part) == 0 || len(part) > 255 {
			return false
		}
	}
	return true
}

// Plan members are sorted parent-first. Empty directories remain explicit
// members; no hidden files or unspecified owner attribute can be published.
func bindPreparationSources(ctx context.Context, request PreparationRequest, plan *PreparationPlan, retained []PreparationSource) error {
	seen, attributes := map[string]bool{}, map[string]bool{}
	retainedSources := map[string]PreparationSource{}
	for _, source := range retained {
		if _, exists := retainedSources[source.Path]; exists {
			return errors.New("prepared source paths are duplicated")
		}
		retainedSources[source.Path] = source
	}
	bytesUsed, attributesUsed := uint64(0), uint64(0)
	for _, owner := range plan.Owners {
		if err := validatePreparationPhysicalMetadata(request, owner); err != nil {
			return err
		}
		if len(owner.Census) == 0 || len(owner.Files) == 0 && len(owner.Attributes) == 0 {
			return errors.New("preparation adapter returned no exact file or attribute census")
		}
		if owner.ExclusiveRoot && len(plan.Owners) != 1 {
			return errors.New("preparation owner requires its own exclusive root namespace")
		}
		for _, file := range owner.Files {
			if !preparationRelative(file.Path, request.Limits.MaxDepth, false) || seen[file.Path] || file.Mode&0077 != 0 || file.Mode&^uint32(0700) != 0 ||
				file.Kind != "file" && file.Kind != "directory" || file.Kind == "directory" && (file.Bytes != 0 || file.Sha256 != "" || file.Mode&0100 == 0) || file.Kind == "file" && (!validDigest(file.Sha256) || file.Mode&0100 != 0) {
				return errors.New("preparation member path, mode, kind or digest is invalid")
			}
			if uint64(len(seen)) >= request.Limits.MaxEntries || file.Bytes > request.Limits.MaxBytes-bytesUsed {
				return errors.New("preparation member count or byte capacity exceeded")
			}
			seen[file.Path] = true
			bytesUsed += file.Bytes
			path := filepath.Join(request.StagingDirectory, owner.StagingName, file.Path)
			opened, err := preparationOpenAbsolute(path, file.Kind == "directory")
			if retained != nil && owner.PhysicalMetadata != nil {
				targetPath := filepath.Join(request.RootPath, file.Path)
				if errors.Is(err, syscall.ENOENT) {
					prior, present := retainedSources[path]
					if !present || prior.File != file {
						return errors.Join(ErrIdentity, errors.New("moved restore source lacks its original reviewed identity"))
					}
					opened, err = preparationOpenAbsolute(targetPath, false)
					if errors.Is(err, syscall.ENOENT) {
						return errors.Join(ErrIdentity, errors.New("reviewed restore member is absent from both source and target"), err)
					}
				} else if err == nil {
					target, targetErr := preparationOpenAbsolute(targetPath, false)
					if targetErr == nil {
						return errors.Join(ErrIdentity, errors.New("restore member exists at both reviewed source and target"), opened.Close(), target.Close())
					}
					if !errors.Is(targetErr, syscall.ENOENT) {
						return errors.Join(targetErr, opened.Close())
					}
				}
			}
			if err != nil {
				return err
			}
			identity, identityErr := preparationIdentity(opened)
			readErr := identityErr
			if readErr == nil && identity.Mode&0777 != file.Mode {
				readErr = errors.Join(ErrIdentity, errors.New("staged member mode differs"))
			}
			if readErr == nil && file.Kind == "file" {
				readErr = preparationVerifyFile(ctx, opened, file.Bytes, file.Sha256)
			}
			if err := errors.Join(readErr, opened.Close()); err != nil {
				return err
			}
			plan.Sources = append(plan.Sources, PreparationSource{File: file, Path: path, Identity: identity})
		}
		for _, attribute := range owner.Attributes {
			key := attribute.Path + "\x00" + attribute.Name
			if !preparationRelative(attribute.Path, request.Limits.MaxDepth, true) || !strings.HasPrefix(attribute.Name, "user.urnetwork.") || len(attribute.Name) > 255 ||
				attribute.Name == PreparationAttribute || attribute.Name == RootGenerationAttribute || attributes[key] {
				return errors.New("preparation owner attribute is invalid or duplicated")
			}
			attributes[key] = true
			attributesUsed++
		}
	}
	if attributesUsed > request.Limits.MaxOwnerAttributes || attributesUsed*4096 > request.Limits.MaxOwnerAttributeBytes {
		return errors.New("preparation owner attribute count exceeds its capacity")
	}
	for _, owner := range plan.Owners {
		for _, attribute := range owner.Attributes {
			if attribute.Path != "." && !seen[attribute.Path] {
				return errors.New("preparation owner attribute target is absent from the exact census")
			}
		}
	}
	for name := range seen {
		parent := filepath.Dir(name)
		if parent != "." {
			found := false
			for _, source := range plan.Sources {
				if source.File.Path == parent && source.File.Kind == "directory" {
					found = true
					break
				}
			}
			if !found {
				return errors.New("preparation member parent is not explicitly included")
			}
		}
	}
	sort.Slice(plan.Sources, func(i, j int) bool {
		a, b := plan.Sources[i].File.Path, plan.Sources[j].File.Path
		if strings.Count(a, "/") != strings.Count(b, "/") {
			return strings.Count(a, "/") < strings.Count(b, "/")
		}
		return a < b
	})
	return ctx.Err()
}

// A fresh source path is never created by observation.
func preparationOpenAbsolute(path string, directory bool) (*os.File, error) {
	var file *os.File
	var err error
	if directory {
		file, err = openPhysicalDirectory(path)
	} else {
		file, err = openProtectedFile(path)
	}
	if err != nil {
		return nil, err
	}
	if err := preparationPrivate(file, directory); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}

// Hash work is chunked and exactly bounded, including detection of appended
// bytes. The caller independently retains/rechecks the named inode.
func preparationVerifyFile(ctx context.Context, file *os.File, size uint64, digest string) error {
	return preparationVerifyFileWithRead(ctx, file, size, digest, nil)
}

// The optional observer reports actual bytes read and cannot supply data or
// change an admission verdict. Ordinary callers do not install an observer.
func preparationVerifyFileWithRead(ctx context.Context, file *os.File, size uint64, digest string, read func(int)) error {
	var before, after unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &before); err != nil {
		return unavailableObservation("preparation file size could not be observed", err)
	}
	if before.Size < 0 || uint64(before.Size) != size {
		return errors.Join(ErrIdentity, errors.New("preparation file size differs from reviewed bytes"))
	}
	hash := sha256.New()
	buffer := make([]byte, 64*1024)
	for offset := uint64(0); offset < size; {
		if err := ctx.Err(); err != nil {
			return err
		}
		part := buffer[:min(uint64(len(buffer)), size-offset)]
		n, err := file.ReadAt(part, int64(offset))
		if read != nil && n > 0 {
			read(n)
		}
		if err != nil || n != len(part) {
			return errors.Join(unavailableObservation("preparation exact file read failed", err), io.ErrUnexpectedEOF)
		}
		_, _ = hash.Write(part)
		offset += uint64(n)
	}
	if err := unix.Fstat(int(file.Fd()), &after); err != nil {
		return unavailableObservation("preparation file could not be reobserved", err)
	}
	if before.Dev != after.Dev || before.Ino != after.Ino || before.Mode != after.Mode || before.Size != after.Size || before.Mtim != after.Mtim || before.Ctim != after.Ctim || before.Uid != after.Uid || before.Gid != after.Gid || before.Nlink != 1 || after.Nlink != 1 || "sha256:"+hex.EncodeToString(hash.Sum(nil)) != digest {
		return errors.Join(ErrIdentity, errors.New("preparation file changed or differs from reviewed bytes"))
	}
	return ctx.Err()
}

// Loading the accepted plan also authenticates its unchanged request bytes.
func readPreparationPlan(ctx context.Context, reference Reference, scope ownerScope) (PreparationPlan, PreparationRequest, error) {
	raw, err := preparationReadReference(ctx, reference, maximumPreparationPlanBytes, "plan")
	if err != nil {
		return PreparationPlan{}, PreparationRequest{}, err
	}
	var plan PreparationPlan
	if err := decodeStrict(raw, &plan); err != nil {
		return plan, PreparationRequest{}, err
	}
	var request PreparationRequest
	if err := decodePreparationRequest(plan.RequestBytes, &request); err != nil {
		return plan, request, err
	}
	if err := request.validate(scope); err != nil {
		return plan, request, err
	}
	if plan.Schema != PreparationPlanSchema || plan.RestartAuthorized || plan.RequestSha256 != plan.Request.Sha256 || preparationDigest(plan.RequestBytes) != plan.RequestSha256 || len(plan.Nonce) != 32 || len(plan.Generation) != RootGenerationBytes || len(plan.Marker) != 32 || len(plan.Lease) != 32 || uint64(len(raw)) > request.Limits.MaxPlanBytes || len(plan.Owners) != len(request.Owners) {
		return plan, request, errors.New("accepted preparation plan is malformed or outside its exact scope")
	}
	if request.Purpose == "fresh" && plan.RestoreArchive != nil || request.Purpose == "restore" && (plan.RestoreArchive == nil || plan.RestoreArchive.Inode == 0) {
		return plan, request, errors.New("accepted preparation purpose differs from retained source authority")
	}
	if request.RootCreation == "create-private" {
		if plan.RootSource != preparationStagedRootPath(request, plan.Nonce) {
			return plan, request, errors.New("accepted plan changed its exact staged root")
		}
	} else if plan.RootSource != "" {
		return plan, request, errors.New("precreated plan cannot contain staged root authority")
	}
	requestRaw, err := preparationReadReference(ctx, plan.Request, maximumPreparationRequestBytes, "request")
	if err != nil {
		return plan, request, err
	}
	if !bytes.Equal(requestRaw, plan.RequestBytes) {
		return plan, request, errors.New("accepted preparation request differs")
	}
	for index, owner := range plan.Owners {
		var normalized bytes.Buffer
		if err := json.Compact(&normalized, owner.Owner.Inputs); err != nil {
			return plan, request, err
		}
		owner.Owner.Inputs = append(json.RawMessage(nil), normalized.Bytes()...)
		plan.Owners[index] = owner
		if !reflect.DeepEqual(owner.Owner, request.Owners[index]) || owner.StagingName != fmt.Sprintf("preparation-%s-%02d", hex.EncodeToString(plan.Nonce), index) {
			return plan, request, errors.New("accepted owner plan changed its fixed scope")
		}
	}
	copyPlan := plan
	copyPlan.Sources = nil
	if err := bindPreparationSources(ctx, request, &copyPlan, plan.Sources); err != nil {
		return plan, request, err
	}
	if !reflect.DeepEqual(plan.Sources, copyPlan.Sources) {
		return plan, request, errors.Join(ErrIdentity, errors.New("prepared source generation or census differs from the accepted plan"))
	}
	if err := preparationControlCapacity(ctx, request, plan); err != nil {
		return plan, request, err
	}
	return plan, request, nil
}
