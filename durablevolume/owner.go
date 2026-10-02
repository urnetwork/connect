// Every owner pins filesystem identity and a volume lease before using state.
// Concurrent checks borrow descriptors; close joins those operations first.
package durablevolume

import (
	"errors"
	"os"
	"sync"
)

// Access determines the lease and whether available write reserve is required.
type Access uint8

const (
	ReadOnly Access = iota + 1
	ReadWrite
	Snapshot
)

var ErrBusy = errors.New("durable volume has another owner")
var ErrClosed = errors.New("durable volume owner is closed")
var ErrUnsupported = errors.New("durable volume ownership requires Linux")
var ErrIdentity = errors.New("durable volume identity is lost")
var ErrUnavailable = errors.New("durable volume is temporarily unavailable")

// Resource pressure pauses admission without changing identity or custody.
type UnavailableError struct {
	Reason string
}

// The owner can retry after capacity or writable availability is restored.
func (self *UnavailableError) Error() string { return ErrUnavailable.Error() + ": " + self.Reason }

// Callers distinguish capacity pressure from a poisoned identity generation.
func (self *UnavailableError) Unwrap() error { return ErrUnavailable }

// A busy lease is pending ownership, never permission to reset or wait forever.
type BusyError struct {
	Root   string
	Access Access
}

// The caller can retain its original work and retry after the other owner joins.
func (self *BusyError) Error() string { return ErrBusy.Error() }

// Allows errors.Is without parsing a diagnostic string.
func (self *BusyError) Unwrap() error { return ErrBusy }

// Kernel device numbers are compared with the uuid's current block device.
type Device struct {
	Major uint32 `json:"major"`
	Minor uint32 `json:"minor"`
}

// One kernel mount record; changing any coordinate invalidates an active owner.
type Mount struct {
	Id             uint64
	ParentId       uint64
	Device         Device
	Root           string
	Path           string
	FilesystemType string
	ReadOnly       bool
}

// File-descriptor filesystem facts supplement namespace and marker identity.
type Filesystem struct {
	Id              [2]int32
	Type            int64
	ReadOnly        bool
	AvailableBytes  uint64
	AvailableInodes uint64
}

// Host supplies kernel facts only. It cannot replace protected file reads,
// marker hashes, path checks, leases or descriptor ownership with a verdict.
// Implementations must support concurrent calls and return finite snapshots.
type Host interface {
	Mounts() ([]Mount, error)
	DeviceUuid(string) (Device, error)
	Filesystem(*os.File) (Filesystem, error)
}

// Methods are concurrency-safe. No external Host method runs under stateLock.
// Close rejects new operations, joins existing borrows and closes exactly once.
type Owner struct {
	stateLock sync.Mutex
	active    uint64
	closing   bool
	failed    error
	stopping  chan struct{}
	joined    chan struct{}
	closed    chan struct{}
	closeErr  error

	spec       VolumeSpec
	scope      ownerScope
	reference  Reference
	rootSpec   StateRootSpec
	rootPath   string
	access     Access
	host       Host
	mount      Mount
	filesystem Filesystem
	mountFile  *os.File
	rootFile   *os.File
	markerFile *os.File
	leaseFile  *os.File
	// An instance-local failure seam precedes real named-file observations.
	// Production leaves it nil; it can refuse, never fabricate successful facts.
	observeFile func(string, *os.File, string) error
}

// Uses the fixed Linux host adapter; no runtime flag selects a synthetic host.
func Open(reference Reference, rootPath string, access Access) (*Owner, error) {
	return OpenWithHost(reference, rootPath, access, defaultHost())
}

// Explicit instance-owned host adapters make kernel boundary tests causal.
// Actual file ownership and I/O checks remain mandatory for every adapter.
func OpenWithHost(reference Reference, rootPath string, access Access, host Host) (*Owner, error) {
	return openForScope(reference, rootPath, access, host, daemonScope)
}

// Owner devices may use their declared system filesystem; this explicit API
// retains every other physical identity, protection, lease and reserve check.
func OpenOwnerLocal(reference Reference, rootPath string, access Access) (*Owner, error) {
	return OpenOwnerLocalWithHost(reference, rootPath, access, defaultHost())
}

// Kernel facts remain the only substitutable boundary. A daemon entry point
// never calls this function based on a schema it discovers in untrusted input.
func OpenOwnerLocalWithHost(reference Reference, rootPath string, access Access, host Host) (*Owner, error) {
	return openForScope(reference, rootPath, access, host, ownerLocalScope)
}

// Scope is selected by the caller before decoding the operational policy.
func openForScope(reference Reference, rootPath string, access Access, host Host, scope ownerScope) (*Owner, error) {
	config, err := loadForScope(reference, scope)
	if err != nil {
		return nil, err
	}
	if host == nil || !canonical(rootPath) || access < ReadOnly || access > Snapshot {
		return nil, errors.New("durable volume host, root or access is incomplete")
	}
	for _, spec := range config.Volumes {
		for _, declared := range spec.StateRoots {
			if declared.Path != rootPath {
				continue
			}
			self := &Owner{spec: spec, scope: scope, reference: reference, rootSpec: declared, rootPath: rootPath, access: access, host: host, stopping: make(chan struct{}), joined: make(chan struct{}), closed: make(chan struct{})}
			if err := self.open(); err != nil {
				return nil, errors.Join(err, self.Close())
			}
			return self, nil
		}
	}
	return nil, errors.New("durable owner root is not explicitly declared")
}

// Active calls retain descriptor lifetime without holding a lock during I/O.
func (self *Owner) borrow() error {
	if self == nil {
		return ErrClosed
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed == nil || self.closing {
		return errors.Join(ErrClosed, self.failed)
	}
	if self.failed != nil {
		return self.failed
	}
	self.active++
	return nil
}

// A failed check remains sticky. An in-flight success cannot outrun close or
// another check's integrity failure and then acknowledge fresh admission.
func (self *Owner) release(result error) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if errors.Is(result, ErrIdentity) && self.failed == nil {
		self.failed = result
	}
	self.active--
	if self.closing && self.active == 0 {
		close(self.joined)
	}
	if self.closing {
		return errors.Join(ErrClosed, result, self.failed)
	}
	return errors.Join(result, self.failed)
}

// Revalidates uuid, mount, marker, physical root and selected access mode.
func (self *Owner) Check() error {
	if err := self.borrow(); err != nil {
		return err
	}
	return self.release(self.check(self.access == ReadWrite))
}

// A read-only or snapshot lease never admits a custody mutation.
func (self *Owner) CheckWrite() error {
	if err := self.borrow(); err != nil {
		return err
	}
	if self.access != ReadWrite {
		return self.release(errors.New("durable volume owner does not permit writes"))
	}
	return self.release(self.check(true))
}

// Identity-only admission permits inspection under read-only/full conditions.
// This never grants mutation, releases the owner lease, or clears proven loss.
func (self *Owner) CheckRead() error {
	if err := self.borrow(); err != nil {
		return err
	}
	return self.release(self.check(false))
}

// The path is immutable and only identifies the already selected root.
func (self *Owner) RootPath() string {
	if self == nil {
		return ""
	}
	return self.rootPath
}

// Opens a caller-owned descendant descriptor. The approved root is never made.
func (self *Owner) OpenDirectory(relative string, create bool) (*os.File, error) {
	if err := self.borrow(); err != nil {
		return nil, err
	}
	file, err := self.openDirectory(relative, create)
	err = self.release(err)
	if err != nil && file != nil {
		err = errors.Join(err, file.Close())
		file = nil
	}
	return file, err
}

// Checks the caller's borrowed descriptor against its approved relative name.
func (self *Owner) CheckDirectory(relative string, directory *os.File) error {
	if err := self.borrow(); err != nil {
		return err
	}
	return self.release(self.checkDirectory(relative, directory, self.access == ReadWrite))
}

// The named retained descriptor is still authenticated without write reserve.
func (self *Owner) CheckReadDirectory(relative string, directory *os.File) error {
	if err := self.borrow(); err != nil {
		return err
	}
	return self.release(self.checkDirectory(relative, directory, false))
}

// Joins bounded checks before closing. It never deletes or recreates state.
func (self *Owner) Close() error {
	if self == nil || self.closed == nil {
		return nil
	}
	first := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closing {
			return false
		}
		self.closing = true
		close(self.stopping)
		if self.active == 0 {
			close(self.joined)
		}
		return true
	}()
	if !first {
		<-self.closed
		return self.closeErr
	}
	<-self.joined
	var result error
	for _, file := range []*os.File{self.rootFile, self.leaseFile, self.markerFile, self.mountFile} {
		if file != nil {
			result = errors.Join(result, file.Close())
		}
	}
	self.closeErr = result
	close(self.closed)
	return result
}
