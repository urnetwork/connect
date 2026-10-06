//go:build linux

// A private write-ahead control binds one accepted plan to one original root.
// Lost acknowledgements reconcile exact complete bytes; unknown partial data,
// missing completed members or a replacement control never become fresh work.
package durablevolume

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"unsafe"
)

const preparationControlSchema = "urnetwork-storage-preparation-control-v1"
const preparationAnchorSchema = "urnetwork-storage-preparation-anchor-v1"

// Exact bytes fit only for bounded metadata/attributes. Large prepared files
// remain in the reviewed immutable staging bundle and are copied in chunks.
type preparationStep struct {
	Kind      string             `json:"kind"`
	Path      string             `json:"path"`
	Attribute string             `json:"attribute,omitempty"`
	Mode      uint32             `json:"mode,omitempty"`
	Bytes     uint64             `json:"bytes,omitempty"`
	Sha256    string             `json:"sha256,omitempty"`
	Source    *PreparationSource `json:"source,omitempty"`
	Raw       []byte             `json:"raw,omitempty"`
	Move      bool               `json:"move,omitempty"`
}

// Both original inode generations and the accepted plan bind the control.
type preparationControlHeader struct {
	Schema       string              `json:"schema"`
	PlanSha256   string              `json:"plan_sha256"`
	CohortSha256 string              `json:"cohort_sha256,omitempty"`
	Root         PreparationIdentity `json:"root"`
	Control      PreparationIdentity `json:"control"`
}

// One hash-linked pending/complete pair acknowledges each exact mutation.
type preparationControlRecord struct {
	Sequence       uint64              `json:"sequence"`
	PreviousSha256 string              `json:"previous_sha256"`
	Phase          string              `json:"phase"`
	Step           preparationStep     `json:"step"`
	Identity       PreparationIdentity `json:"identity"`
	Sha256         string              `json:"sha256"`
}

// The root retains its original external control even across process reopen.
type preparationAnchor struct {
	Schema       string              `json:"schema"`
	PlanSha256   string              `json:"plan_sha256"`
	CohortSha256 string              `json:"cohort_sha256,omitempty"`
	RootInode    uint64              `json:"root_inode"`
	Control      PreparationIdentity `json:"control"`
}

// All fields belong to one synchronous invocation, which closes every handle
// before returning. Reconciliation always opens a distinct joined invocation.
type preparationApply struct {
	admission   *preparationAdmission
	archive     *preparationRestoreArchive
	plan        PreparationPlan
	reference   Reference
	control     *os.File
	controlStat syscall.Stat_t
	header      preparationControlHeader
	anchor      []byte
	previous    string
	sequence    uint64
	completed   []preparationControlRecord
	pending     *preparationControlRecord
	position    int
	retained    map[string]syscall.Stat_t
	attributes  map[string][]byte
	moveSources map[string]PreparationSource
	hooks       *preparationHooks
	failed      error
	adapter     PreparationAdapter
	inventory   Inventory
	readOnly    bool
	complete    bool
	prepared    PreparationResult
	cohort      Reference
}

// Absence is legal only for a separately reviewed, still-fresh plan target.
func preparationAbsent(parent *os.File, name string) error {
	fd, err := syscall.Openat(int(parent.Fd()), name, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, syscall.ENOENT) {
		return nil
	}
	if err != nil {
		return namedObservation("preparation target could not be observed", err)
	}
	file := os.NewFile(uintptr(fd), name)
	return errors.Join(ErrIdentity, errors.New("preparation target already exists without this plan's custody"), file.Close())
}

// Fresh enrollment refuses every preexisting owner attribute, not only the
// expected generation name. Missing former custody cannot look pristine.
func preparationRequireNoAttributes(file *os.File) error {
	names, err := listInventoryAttributes(file)
	if err != nil {
		return err
	}
	for _, name := range names {
		if strings.HasPrefix(name, ownerAttributeNamespace) {
			return errors.Join(ErrIdentity, errors.New("fresh preparation found previous owner custody metadata"))
		}
	}
	return nil
}

// Only CREATE is exposed; neither reconciliation nor a repeated command can
// replace original owner authority. Every successful update is really synced.
func preparationCreateAttribute(file *os.File, name string, raw []byte) error {
	if len(raw) == 0 || len(raw) > 4096 {
		return errors.New("preparation attribute exceeds its fixed capacity")
	}
	key, err := syscall.BytePtrFromString(name)
	if err != nil {
		return err
	}
	_, _, errno := syscall.Syscall6(syscall.SYS_FSETXATTR, file.Fd(), uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(&raw[0])), uintptr(len(raw)), 1, 0)
	runtime.KeepAlive(file)
	runtime.KeepAlive(raw)
	if errno != 0 {
		return errno
	}
	return file.Sync()
}

// Named members are opened below the retained root or one retained external
// metadata parent; no operation accepts an arbitrary path from a journal.
func (self *preparationApply) parent(path string) (_ *os.File, name string, resultErr error) {
	request := self.admission.request
	parentPath := filepath.Dir(path)
	if original := self.admission.directories[parentPath]; original != nil {
		fd, err := syscall.Dup(int(original.Fd()))
		if err != nil {
			return nil, "", err
		}
		syscall.CloseOnExec(fd)
		return os.NewFile(uintptr(fd), parentPath), filepath.Base(path), nil
	}
	if !beneath(request.RootPath, parentPath) {
		return nil, "", errors.New("preparation operation is outside its retained namespace")
	}
	relative, err := filepath.Rel(request.RootPath, parentPath)
	if err != nil {
		return nil, "", err
	}
	fd, err := syscall.Dup(int(self.admission.root.Fd()))
	if err != nil {
		return nil, "", err
	}
	syscall.CloseOnExec(fd)
	file := os.NewFile(uintptr(fd), request.RootPath)
	for _, part := range strings.Split(relative, "/") {
		if part == "." {
			continue
		}
		next, openErr := syscall.Openat(int(file.Fd()), part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
		closeErr := file.Close()
		if openErr != nil {
			return nil, "", errors.Join(namedObservation("preparation parent could not be opened", openErr), closeErr)
		}
		file = os.NewFile(uintptr(next), filepath.Join(file.Name(), part))
		if err := errors.Join(closeErr, preparationPrivate(file, true)); err != nil {
			return nil, "", errors.Join(err, file.Close())
		}
		var observed syscall.Stat_t
		if err := syscall.Fstat(next, &observed); err != nil {
			return nil, "", errors.Join(err, file.Close())
		}
		retained, present := self.retained[file.Name()]
		if !present || !preparationSameIdentity(retained, observed) {
			return nil, "", errors.Join(ErrIdentity, errors.New("preparation parent is not an acknowledged original directory"), file.Close())
		}
	}
	return file, filepath.Base(path), nil
}

// Byte changes are checked separately from physical and protection identity.
func preparationSameIdentity(a, b syscall.Stat_t) bool {
	return a.Dev == b.Dev && a.Ino == b.Ino && a.Mode == b.Mode && a.Uid == b.Uid && a.Gid == b.Gid
}

// Unchanged retained file metadata avoids rereading an acknowledged prefix.
func preparationSameFile(a, b syscall.Stat_t) bool {
	return preparationSameIdentity(a, b) && a.Size == b.Size && a.Mtim == b.Mtim && a.Ctim == b.Ctim && a.Nlink == 1 && b.Nlink == 1
}

// Control metadata remains unchanged between our own acknowledged appends.
func (self *preparationApply) check() error {
	if self.failed != nil {
		return self.failed
	}
	if err := self.admission.check(); err != nil {
		return err
	}
	if self.archive != nil {
		if err := self.archive.checkRoot(); err != nil {
			return err
		}
	}
	if self.control != nil {
		if err := sameNamedFile(self.control, self.admission.request.ControlPath); err != nil {
			return err
		}
		var stat syscall.Stat_t
		if err := syscall.Fstat(int(self.control.Fd()), &stat); err != nil {
			return unavailableObservation("preparation control could not be observed", err)
		}
		if !preparationSameFile(self.controlStat, stat) {
			return errors.Join(ErrIdentity, errors.New("preparation control changed outside this owner"))
		}
	}
	if len(self.anchor) != 0 {
		raw, err := readInventoryAttribute(self.admission.root, PreparationAttribute, 4096)
		if err != nil {
			return err
		}
		if !bytes.Equal(raw, self.anchor) {
			return errors.Join(ErrIdentity, errors.New("preparation root lost its original plan reservation"))
		}
	}
	return nil
}

// All hooks run after real publication. A later invocation must read back
// exact pending state; this instance never retries an uncertain mutation.
func (self *preparationApply) after(stage, path string) error {
	if self.hooks != nil && self.hooks.after != nil {
		return self.hooks.after(stage, path)
	}
	return nil
}

// Confirmed identity loss takes precedence over a recoverable lost acknowledgement.
func (self *preparationApply) uncertain(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrIdentity) {
		self.failed = err
		return err
	}
	self.failed = errors.Join(ErrPreparationUncertain, err)
	return self.failed
}

// Appends are write-ahead and bounded. A torn record is retained and refused;
// its absence is never guessed as permission to repeat another step.
func (self *preparationApply) append(phase string, step preparationStep, identity PreparationIdentity) error {
	if err := self.check(); err != nil {
		return err
	}
	record := preparationControlRecord{Sequence: self.sequence + 1, PreviousSha256: self.previous, Phase: phase, Step: step, Identity: identity}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	record.Sha256 = preparationDigest(raw)
	raw, err = json.Marshal(record)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if len(raw) > maximumPreparationControlRecordBytes || self.controlStat.Size > maximumPreparationControlBytes-int64(len(raw)) {
		return errors.New("preparation control exceeds its finite record or byte capacity")
	}
	n, err := self.control.WriteAt(raw, self.controlStat.Size)
	if err != nil || n != len(raw) {
		return self.uncertain(errors.Join(io.ErrShortWrite, err))
	}
	if err := self.control.Sync(); err != nil {
		return self.uncertain(err)
	}
	if err := self.after("control-"+phase, step.Path); err != nil {
		return self.uncertain(err)
	}
	if err := syscall.Fstat(int(self.control.Fd()), &self.controlStat); err != nil {
		return self.uncertain(err)
	}
	self.sequence, self.previous = record.Sequence, record.Sha256
	if phase == "pending" {
		self.pending = &record
	} else {
		self.completed = append(self.completed, record)
		self.pending = nil
	}
	return self.uncertain(self.check())
}

// Header identity is recorded before any target mutation. Its inode is also
// bound on the original root, so replacing an entire completed journal fails.
func (self *preparationApply) openControl() (resultErr error) {
	if self.control != nil {
		return self.reserveControl()
	}
	mutated := false
	defer func() {
		if mutated && resultErr != nil {
			resultErr = self.uncertain(resultErr)
		}
	}()
	request := self.admission.request
	parent := self.admission.directories[filepath.Dir(request.ControlPath)]
	fd, err := syscall.Openat(int(parent.Fd()), filepath.Base(request.ControlPath), syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	created := false
	if errors.Is(err, syscall.ENOENT) {
		if err := errors.Join(preparationEmpty(self.admission.ctx, self.admission.root), preparationRequireNoAttributes(self.admission.root)); err != nil {
			return err
		}
		for _, path := range []string{request.MarkerPath, request.LeasePath, request.DeclarationPath} {
			if err := preparationAbsent(self.admission.directories[filepath.Dir(path)], filepath.Base(path)); err != nil {
				return err
			}
		}
		if err := self.check(); err != nil {
			return err
		}
		fd, err = syscall.Openat(int(parent.Fd()), filepath.Base(request.ControlPath), syscall.O_RDWR|syscall.O_CREAT|syscall.O_EXCL|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
		created = err == nil
		mutated = created
	}
	if err != nil {
		return namedObservation("preparation control could not be opened", err)
	}
	self.control = os.NewFile(uintptr(fd), request.ControlPath)
	if err := preparationPrivate(self.control, false); err != nil {
		return err
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.Join(ErrBusy, err)
	}
	identity, err := preparationIdentity(self.control)
	if err != nil {
		return err
	}
	if created {
		self.header = preparationControlHeader{Schema: preparationControlSchema, PlanSha256: self.reference.Sha256, CohortSha256: self.cohort.Sha256, Root: self.plan.Root, Control: identity}
		raw, err := json.Marshal(self.header)
		if err != nil {
			return err
		}
		raw = append(raw, '\n')
		n, err := self.control.Write(raw)
		if err != nil || n != len(raw) {
			return self.uncertain(errors.Join(io.ErrShortWrite, err))
		}
		if err := self.control.Sync(); err != nil {
			return self.uncertain(err)
		}
		if err := parent.Sync(); err != nil {
			return self.uncertain(err)
		}
		if err := self.after("control-header", request.ControlPath); err != nil {
			return self.uncertain(err)
		}
		self.previous = preparationDigest(raw[:len(raw)-1])
	} else if err := self.readControl(); err != nil {
		return err
	}
	if self.header.Schema != preparationControlSchema || self.header.PlanSha256 != self.reference.Sha256 || self.header.CohortSha256 != self.cohort.Sha256 || self.header.Root != self.plan.Root || self.header.Control != identity {
		return errors.Join(ErrIdentity, errors.New("preparation control belongs to another plan or physical generation"))
	}
	if err := syscall.Fstat(fd, &self.controlStat); err != nil {
		return err
	}
	return self.reserveControl()
}

// A cohort may already hold and authenticate this control without writing it.
// Enrollment still occurs only at the original write-ahead publication boundary.
func (self *preparationApply) reserveControl() (resultErr error) {
	mutated := false
	defer func() {
		if mutated && resultErr != nil {
			resultErr = self.uncertain(resultErr)
		}
	}()
	request := self.admission.request
	anchor, err := json.Marshal(preparationAnchor{Schema: preparationAnchorSchema, PlanSha256: self.reference.Sha256, CohortSha256: self.cohort.Sha256, RootInode: self.plan.Root.Inode, Control: self.header.Control})
	if err != nil {
		return err
	}
	retained, err := readInventoryAttribute(self.admission.root, PreparationAttribute, 4096)
	if errors.Is(err, syscall.ENODATA) {
		if self.sequence != 0 {
			return errors.Join(ErrIdentity, errors.New("existing preparation control lost its root reservation"))
		}
		if err := errors.Join(preparationEmpty(self.admission.ctx, self.admission.root), preparationRequireNoAttributes(self.admission.root)); err != nil {
			return err
		}
		if err := self.check(); err != nil {
			return err
		}
		mutated = true
		if err := preparationCreateAttribute(self.admission.root, PreparationAttribute, anchor); err != nil {
			return self.uncertain(err)
		}
		if err := self.after("root-reservation", request.RootPath); err != nil {
			return self.uncertain(err)
		}
		retained = anchor
	} else if err != nil {
		return err
	}
	if !bytes.Equal(retained, anchor) {
		return errors.Join(ErrIdentity, errors.New("preparation root is reserved by another control generation"))
	}
	self.anchor = anchor
	return self.check()
}

// The exact original prefix is verified once on reopen. JSON duplicate keys,
// torn tails, reordered records and unknown phases refuse without truncation.
func (self *preparationApply) readControl() error {
	raw, err := boundedProtectedReadContext(self.admission.ctx, self.control, maximumPreparationControlBytes)
	if err != nil {
		return err
	}
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		return errors.Join(ErrPreparationUncertain, errors.New("preparation control has an unknown or partial tail"))
	}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 4096), maximumPreparationControlRecordBytes)
	if !scanner.Scan() {
		return errors.Join(ErrIdentity, errors.New("preparation control header is absent"))
	}
	if err := decodeStrict(scanner.Bytes(), &self.header); err != nil {
		return err
	}
	self.previous = preparationDigest(scanner.Bytes())
	maximum := 2 * (self.admission.request.Limits.MaxEntries + self.admission.request.Limits.MaxOwnerAttributes + 8)
	for scanner.Scan() {
		if err := self.admission.ctx.Err(); err != nil {
			return err
		}
		if self.sequence >= maximum {
			return errors.New("preparation control record capacity exceeded")
		}
		var record preparationControlRecord
		if err := decodeStrict(scanner.Bytes(), &record); err != nil {
			return err
		}
		hash := record.Sha256
		record.Sha256 = ""
		canonical, err := json.Marshal(record)
		if err != nil {
			return err
		}
		record.Sha256 = hash
		if record.Sequence != self.sequence+1 || record.PreviousSha256 != self.previous || preparationDigest(canonical) != hash {
			return errors.Join(ErrIdentity, errors.New("preparation control prefix changed"))
		}
		switch record.Phase {
		case "pending":
			if self.pending != nil || record.Identity != (PreparationIdentity{}) {
				return errors.New("preparation control has overlapping or malformed intent")
			}
			self.pending = &record
		case "complete":
			if self.pending == nil || !reflect.DeepEqual(self.pending.Step, record.Step) || record.Identity.Inode == 0 {
				return errors.New("preparation completion lost its original intent")
			}
			self.completed = append(self.completed, record)
			self.pending = nil
		default:
			return errors.New("preparation control phase is unsupported")
		}
		self.sequence, self.previous = record.Sequence, record.Sha256
	}
	return scanner.Err()
}

// Attribute publication and regular members share exactly the same retained
// pending/completed lifecycle. No step may skip or reorder original intent.
func (self *preparationApply) step(step preparationStep) error {
	if err := self.check(); err != nil {
		return err
	}
	if self.position < len(self.completed) {
		completed := self.completed[self.position]
		if !reflect.DeepEqual(completed.Step, step) {
			return errors.Join(ErrIdentity, errors.New("preparation continuation differs from the original completed intent"))
		}
		identity, err := self.observe(step, false)
		if err != nil {
			return err
		}
		if identity != completed.Identity {
			return errors.Join(ErrIdentity, errors.New("preparation completed member was replaced"))
		}
		self.position++
		return nil
	}
	if self.readOnly {
		return self.preflightNextStep(step)
	}
	if self.pending != nil {
		if !reflect.DeepEqual(self.pending.Step, step) {
			return errors.Join(ErrIdentity, errors.New("preparation continuation differs from the original pending intent"))
		}
	} else {
		if err := self.requireAbsent(step); err != nil {
			return err
		}
		if err := self.append("pending", step, PreparationIdentity{}); err != nil {
			return err
		}
	}
	identity, err := self.observe(step, true)
	if err != nil {
		return err
	}
	if err := self.append("complete", step, identity); err != nil {
		return self.uncertain(err)
	}
	self.position++
	return nil
}

// New intent is recorded only after proving that its exact destination is absent.
func (self *preparationApply) requireAbsent(step preparationStep) error {
	if step.Kind == "attribute" {
		file, err := self.attributeTarget(step.Path)
		if err != nil {
			return err
		}
		defer file.Close()
		_, err = readInventoryAttribute(file, step.Attribute, 4096)
		if errors.Is(err, syscall.ENODATA) {
			return nil
		}
		if err != nil {
			return err
		}
		return errors.Join(ErrIdentity, errors.New("unreserved preparation attribute already exists"))
	}
	parent, name, err := self.parent(step.Path)
	if err != nil {
		return err
	}
	defer parent.Close()
	return preparationAbsent(parent, name)
}

// Only actual original directories or an already copied listed file can host
// a checkpoint. A replaced inode is refused even when all payload bytes match.
func (self *preparationApply) attributeTarget(path string) (*os.File, error) {
	if path == self.admission.request.RootPath {
		fd, err := syscall.Dup(int(self.admission.root.Fd()))
		if err != nil {
			return nil, err
		}
		syscall.CloseOnExec(fd)
		return os.NewFile(uintptr(fd), path), nil
	}
	parent, name, err := self.parent(path)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	fd, err := syscall.Openat(int(parent.Fd()), name, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, namedObservation("preparation attribute target could not be observed", err)
	}
	file := os.NewFile(uintptr(fd), path)
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	old, present := self.retained[path]
	if !present || !preparationSameIdentity(old, stat) {
		return nil, errors.Join(ErrIdentity, errors.New("preparation attribute target differs from its original member"), file.Close())
	}
	return file, nil
}

// A pending complete publication can be synced and acknowledged after its
// former owner joins. Unknown/partial bytes are never filled in or replaced.
func (self *preparationApply) observe(step preparationStep, pending bool) (_ PreparationIdentity, resultErr error) {
	if step.Move {
		return self.observeMovedRestoreFile(step, pending)
	}
	if step.Kind == "attribute" {
		return self.observeAttribute(step, pending)
	}
	if step.Kind == "root-directory" {
		return self.observeRoot(step, pending)
	}
	mutated := false
	defer func() {
		if mutated && resultErr != nil {
			resultErr = self.uncertain(resultErr)
		}
	}()
	parent, name, err := self.parent(step.Path)
	if err != nil {
		return PreparationIdentity{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, parent.Close()) }()
	flags := syscall.O_RDONLY | syscall.O_CLOEXEC | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
	if step.Kind == "directory" {
		flags |= syscall.O_DIRECTORY
	}
	fd, err := syscall.Openat(int(parent.Fd()), name, flags, 0)
	created := false
	if errors.Is(err, syscall.ENOENT) && pending {
		if err := self.check(); err != nil {
			return PreparationIdentity{}, err
		}
		if step.Kind == "directory" {
			if err := syscall.Mkdirat(int(parent.Fd()), name, step.Mode); err != nil {
				return PreparationIdentity{}, self.uncertain(err)
			}
			mutated = true
			fd, err = syscall.Openat(int(parent.Fd()), name, flags, 0)
		} else {
			fd, err = syscall.Openat(int(parent.Fd()), name, syscall.O_RDWR|syscall.O_CREAT|syscall.O_EXCL|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, step.Mode)
		}
		created = err == nil
		mutated = mutated || created
	}
	if err != nil {
		return PreparationIdentity{}, namedObservation("preparation member could not be observed", err)
	}
	file := os.NewFile(uintptr(fd), step.Path)
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	if err := preparationPrivate(file, step.Kind == "directory"); err != nil {
		return PreparationIdentity{}, err
	}
	identity, err := preparationIdentity(file)
	if err != nil {
		return PreparationIdentity{}, err
	}
	if identity.Mode&0777 != step.Mode || deviceNumber(identity.Device) != self.admission.mount.Device {
		return PreparationIdentity{}, errors.Join(ErrIdentity, errors.New("preparation member mode or filesystem differs"))
	}
	if created && step.Kind == "file" {
		if err := self.copyFile(file, step); err != nil {
			return PreparationIdentity{}, self.uncertain(err)
		}
	}
	if step.Kind == "file" {
		if err := preparationVerifyFile(self.admission.ctx, file, step.Bytes, step.Sha256); err != nil {
			return PreparationIdentity{}, err
		}
	}
	if step.Kind == "directory" && pending {
		if err := preparationEmpty(self.admission.ctx, file); err != nil {
			return PreparationIdentity{}, err
		}
	}
	if pending {
		mutated = true
		if err := file.Sync(); err != nil {
			return PreparationIdentity{}, err
		}
		if err := self.after("member-sync", step.Path); err != nil {
			return PreparationIdentity{}, err
		}
		if err := parent.Sync(); err != nil {
			return PreparationIdentity{}, err
		}
		if err := self.after("parent-sync", step.Path); err != nil {
			return PreparationIdentity{}, self.uncertain(err)
		}
	}
	if err := self.sameMember(file, parent, name); err != nil {
		return PreparationIdentity{}, err
	}
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil {
		return PreparationIdentity{}, err
	}
	self.retained[step.Path] = stat
	if err := self.check(); err != nil {
		return PreparationIdentity{}, err
	}
	return identity, nil
}

// Real named and opened descriptors must still identify the same original member.
func (self *preparationApply) sameMember(file, parent *os.File, name string) error {
	fd, err := syscall.Openat(int(parent.Fd()), name, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return namedObservation("preparation named member could not be observed", err)
	}
	named := os.NewFile(uintptr(fd), name)
	observed, observeErr := preparationIdentity(named)
	opened, openErr := preparationIdentity(file)
	if err := errors.Join(observeErr, openErr, named.Close()); err != nil {
		return err
	}
	if observed != opened {
		return errors.Join(ErrIdentity, errors.New("preparation member was replaced during publication"))
	}
	return nil
}

// File copying retains original staging inodes and hashes and uses finite
// chunks. No temporary name, truncation or arbitrary cleanup is performed.
func (self *preparationApply) copyFile(target *os.File, step preparationStep) (resultErr error) {
	var reader io.Reader
	if step.Source != nil {
		source, err := preparationOpenAbsolute(step.Source.Path, false)
		if err != nil {
			return err
		}
		defer func() { resultErr = errors.Join(resultErr, source.Close()) }()
		identity, err := preparationIdentity(source)
		if err != nil {
			return err
		}
		if identity != step.Source.Identity {
			return errors.Join(ErrIdentity, errors.New("preparation staged source inode changed"))
		}
		if err := preparationVerifyFile(self.admission.ctx, source, step.Bytes, step.Sha256); err != nil {
			return err
		}
		reader = io.NewSectionReader(source, 0, int64(step.Bytes))
	} else {
		if uint64(len(step.Raw)) != step.Bytes || preparationDigest(step.Raw) != step.Sha256 {
			return errors.New("preparation metadata differs from reviewed bytes")
		}
		reader = bytes.NewReader(step.Raw)
	}
	buffer := make([]byte, 64*1024)
	for copied := uint64(0); copied < step.Bytes; {
		if err := self.admission.ctx.Err(); err != nil {
			return err
		}
		part := buffer[:min(uint64(len(buffer)), step.Bytes-copied)]
		n, err := io.ReadFull(reader, part)
		if err != nil {
			return err
		}
		written, err := target.Write(part[:n])
		if err != nil || written != n {
			return errors.Join(io.ErrShortWrite, err)
		}
		copied += uint64(n)
	}
	return self.admission.ctx.Err()
}

// Exact fully written pending attributes can be synced; missing completed ones refuse.
func (self *preparationApply) observeAttribute(step preparationStep, pending bool) (_ PreparationIdentity, resultErr error) {
	mutated := false
	defer func() {
		if mutated && resultErr != nil {
			resultErr = self.uncertain(resultErr)
		}
	}()
	file, err := self.attributeTarget(step.Path)
	if err != nil {
		return PreparationIdentity{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	identity, err := preparationIdentity(file)
	if err != nil {
		return PreparationIdentity{}, err
	}
	retained, err := readInventoryAttribute(file, step.Attribute, 4096)
	if errors.Is(err, syscall.ENODATA) && pending {
		if err := self.check(); err != nil {
			return PreparationIdentity{}, err
		}
		mutated = true
		if err := preparationCreateAttribute(file, step.Attribute, step.Raw); err != nil {
			return PreparationIdentity{}, err
		}
		if err := self.after("attribute-sync", step.Path+":"+step.Attribute); err != nil {
			return PreparationIdentity{}, self.uncertain(err)
		}
		retained, err = readInventoryAttribute(file, step.Attribute, 4096)
	}
	if err != nil {
		return PreparationIdentity{}, err
	}
	if !bytes.Equal(retained, step.Raw) || uint64(len(retained)) != step.Bytes || preparationDigest(retained) != step.Sha256 {
		return PreparationIdentity{}, errors.Join(ErrIdentity, errors.New("preparation attribute differs from original pending or completed authority"))
	}
	if pending {
		mutated = true
		if err := file.Sync(); err != nil {
			return PreparationIdentity{}, self.uncertain(err)
		}
	}
	self.attributes[step.Path+"\x00"+step.Attribute] = append([]byte(nil), retained...)
	if _, present := self.retained[step.Path]; present {
		var stat syscall.Stat_t
		if err := syscall.Fstat(int(file.Fd()), &stat); err != nil {
			return PreparationIdentity{}, err
		}
		self.retained[step.Path] = stat
	}
	return identity, self.check()
}

// Completion checks the entire target namespace once and only reobserves
// unchanged file metadata thereafter. No acknowledged leaf can disappear.
func (self *preparationApply) finalCensus() error {
	return self.census(false)
}

// Partial admission accepts only the exact acknowledged/pending prefix. Future
// plan members are not permission to adopt files already present without intent.
func (self *preparationApply) census(partial bool) error {
	request := self.admission.request
	allowedAttributes := map[string]bool{request.RootPath + "\x00" + PreparationAttribute: true, request.RootPath + "\x00" + RootGenerationAttribute: true}
	for _, owner := range self.plan.Owners {
		for _, attribute := range owner.Attributes {
			path := request.RootPath
			if attribute.Path != "." {
				path = filepath.Join(path, attribute.Path)
			}
			allowedAttributes[path+"\x00"+attribute.Name] = true
		}
	}
	if partial {
		allowedAttributes = map[string]bool{}
		if len(self.anchor) != 0 {
			allowedAttributes[request.RootPath+"\x00"+PreparationAttribute] = true
		}
		for key := range self.attributes {
			allowedAttributes[key] = true
		}
	}
	checkAttributes := func(file *os.File, path string) error {
		names, err := listInventoryAttributes(file)
		if err != nil {
			return err
		}
		for _, name := range names {
			if strings.HasPrefix(name, ownerAttributeNamespace) && !allowedAttributes[path+"\x00"+name] {
				return errors.Join(ErrIdentity, errors.New("prepared namespace retains unreviewed owner metadata"))
			}
		}
		return nil
	}
	if err := checkAttributes(self.admission.root, request.RootPath); err != nil {
		return err
	}
	expected := map[string]bool{}
	for _, source := range self.plan.Sources {
		if _, present := self.retained[filepath.Join(request.RootPath, source.File.Path)]; partial && !present {
			continue
		}
		expected[source.File.Path] = true
	}
	var visit func(*os.File, string, uint64) error
	count := uint64(0)
	visit = func(directory *os.File, relative string, depth uint64) error {
		if err := self.admission.ctx.Err(); err != nil {
			return err
		}
		if depth > request.Limits.MaxDepth {
			return errors.New("prepared namespace depth exceeds its capacity")
		}
		fd, err := syscall.Openat(int(directory.Fd()), ".", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		view := os.NewFile(uintptr(fd), directory.Name())
		names, readErr := view.Readdirnames(int(request.Limits.MaxEntries-count) + 1)
		if errors.Is(readErr, io.EOF) {
			readErr = nil
		}
		if err := errors.Join(readErr, view.Close()); err != nil {
			return err
		}
		for _, name := range names {
			count++
			if count > request.Limits.MaxEntries {
				return errors.New("prepared namespace count exceeds its capacity")
			}
			path := name
			if relative != "" {
				path = relative + "/" + name
			}
			if !expected[path] {
				return errors.Join(ErrIdentity, errors.New("prepared root contains an unreviewed member"))
			}
			absolute := filepath.Join(request.RootPath, path)
			retained, present := self.retained[absolute]
			if !present {
				return errors.Join(ErrIdentity, errors.New("prepared member lacks an acknowledged identity"))
			}
			file, err := preparationOpenAbsolute(absolute, retained.Mode&syscall.S_IFMT == syscall.S_IFDIR)
			if err != nil {
				return err
			}
			var stat syscall.Stat_t
			err = syscall.Fstat(int(file.Fd()), &stat)
			if err == nil && (!preparationSameIdentity(retained, stat) || stat.Mode&syscall.S_IFMT == syscall.S_IFREG && !preparationSameFile(retained, stat)) {
				err = errors.Join(ErrIdentity, errors.New("prepared member changed after acknowledgement"))
			}
			if err == nil && stat.Mode&syscall.S_IFMT == syscall.S_IFDIR {
				err = visit(file, path, depth+1)
			}
			if err == nil {
				err = checkAttributes(file, absolute)
			}
			if err := errors.Join(err, file.Close()); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(self.admission.root, "", 0); err != nil {
		return err
	}
	if count != uint64(len(expected)) {
		return errors.Join(ErrIdentity, errors.New("prepared root lost an acknowledged member"))
	}
	for key, raw := range self.attributes {
		parts := strings.SplitN(key, "\x00", 2)
		file, err := self.attributeTarget(parts[0])
		if err != nil {
			return err
		}
		current, readErr := readInventoryAttribute(file, parts[1], 4096)
		if err := errors.Join(readErr, file.Close()); err != nil {
			return err
		}
		if !bytes.Equal(current, raw) {
			return errors.Join(ErrIdentity, errors.New("prepared checkpoint changed after acknowledgement"))
		}
	}
	return self.check()
}

// Admission retains all physical leases without creating a control or writing
// a target. Cohorts can admit every member before executing their first step.
func openPreparationApplication(ctx context.Context, reference Reference, adapter PreparationAdapter, host Host, scope ownerScope, hooks *preparationHooks, sharedParents map[string]*os.File) (_ *preparationApply, resultErr error) {
	plan, request, err := readPreparationPlan(ctx, reference, scope)
	if err != nil {
		return nil, err
	}
	if err := preparationAdapterAdmission(request, adapter); err != nil {
		return nil, err
	}
	var inventory Inventory
	if request.Purpose == "restore" {
		inventory, err = readPreparationRestoreInventory(ctx, request)
		if err != nil {
			return nil, err
		}
		expectedOwners := make([]PreparationOwnerPlan, 0, len(plan.Owners))
		for _, owner := range plan.Owners {
			expected, err := adapter.Restore(ctx, owner.StagingName, owner.Owner, inventory)
			if err != nil {
				return nil, err
			}
			if owner.PhysicalMetadata == nil && !reflect.DeepEqual(expected, owner) {
				return nil, errors.New("accepted restore owner differs from its original fixed semantic census")
			}
			expectedOwners = append(expectedOwners, expected)
		}
		if err := validatePreparationRestoreCoverage(inventory, expectedOwners); err != nil {
			return nil, err
		}
	}
	if err := validatePhysicalMetadataDerivations(ctx, request, plan, adapter, inventory); err != nil {
		return nil, err
	}
	admission, err := openPreparationAdmissionWithParents(ctx, request, host, scope, plan.RootSource, sharedParents)
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, admission.close())
		}
	}()
	if !reflect.DeepEqual(admission.identities, plan.Directories) || admission.identities[request.RootPath] != plan.Root || admission.mount != plan.Mount || admission.filesystem.Id != plan.Filesystem.Id || admission.filesystem.Type != plan.Filesystem.Type {
		return nil, errors.Join(ErrIdentity, errors.New("accepted preparation physical generation changed"))
	}
	if err := admission.fence(); err != nil {
		return nil, err
	}
	self := &preparationApply{admission: admission, plan: plan, reference: reference, retained: map[string]syscall.Stat_t{}, attributes: map[string][]byte{}, moveSources: preparationMoveSources(plan), hooks: hooks, adapter: adapter, inventory: inventory}
	if request.Purpose == "restore" {
		self.archive, err = openPreparationRestoreArchive(ctx, request, host)
		if err != nil {
			return nil, err
		}
		defer func() {
			if resultErr != nil {
				resultErr = errors.Join(resultErr, self.archive.close())
			}
		}()
		if plan.RestoreArchive == nil || self.archive.identity != *plan.RestoreArchive {
			return nil, errors.Join(ErrIdentity, errors.New("accepted restore archive physical generation changed"))
		}
		if err := self.archive.authenticate(hooks); err != nil {
			return nil, err
		}
	}
	return self, nil
}

// No failed invocation retries a mutation. A joined new invocation reads back
// the same original plan and never resets completed or pending journal records.
func applyPreparation(ctx context.Context, reference Reference, adapter PreparationAdapter, host Host, scope ownerScope, hooks *preparationHooks) (result PreparationResult, resultErr error) {
	self, err := openPreparationApplication(ctx, reference, adapter, host, scope, hooks, nil)
	if err != nil {
		return result, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, self.close())
		if resultErr != nil {
			result = PreparationResult{}
		}
	}()
	if err := self.checkRestore(); err != nil {
		return result, err
	}
	if err := self.openControl(); err != nil {
		return result, err
	}
	return self.run()
}

// Closing joins this synchronous application's retained control and all roots.
func (self *preparationApply) close() error {
	var err error
	if self.control != nil {
		err = self.control.Close()
		self.control = nil
	}
	if self.archive != nil {
		err = errors.Join(err, self.archive.close())
		self.archive = nil
	}
	return errors.Join(err, self.admission.close())
}

// Both read-only cohort admission and publication walk the same exact sequence.
// The read-only walk stops at the first unacknowledged step without mutation.
func (self *preparationApply) run() (result PreparationResult, resultErr error) {
	// Full source names/attributes are checked at operation boundaries, never
	// rehashed for every target member or journal append.
	defer func() {
		resultErr = errors.Join(resultErr, self.checkRestore())
		if resultErr != nil {
			result = PreparationResult{}
		}
	}()
	plan, reference := self.plan, self.reference
	admission := self.admission
	request, ctx, scope := admission.request, admission.ctx, admission.scope
	adapter, inventory := self.adapter, self.inventory
	var err error
	if request.RootCreation == "create-private" {
		if err := self.step(preparationRootStep(request, plan)); err != nil {
			return result, err
		}
	}
	attributeStep := func(path, name string, raw []byte) preparationStep {
		return preparationStep{Kind: "attribute", Path: path, Attribute: name, Bytes: uint64(len(raw)), Sha256: preparationDigest(raw), Raw: raw}
	}
	if err := self.step(attributeStep(request.RootPath, RootGenerationAttribute, plan.Generation)); err != nil {
		return result, err
	}
	for _, source := range plan.Sources {
		step := preparationStep{Kind: source.File.Kind, Path: filepath.Join(request.RootPath, source.File.Path), Mode: source.File.Mode, Bytes: source.File.Bytes, Sha256: source.File.Sha256}
		step.Move = preparationSourceMoves(plan, source)
		if source.File.Kind == "file" {
			copySource := source
			step.Source = &copySource
		}
		if err := self.step(step); err != nil {
			return result, err
		}
	}
	attributeBytes := uint64(0)
	for _, owner := range plan.Owners {
		if err := self.finalCensus(); err != nil {
			return result, err
		}
		var attributes []PreparedAttribute
		if request.Purpose == "restore" {
			attributes, err = adapter.InspectRestore(ctx, admission.root, owner, inventory)
		} else {
			attributes, err = adapter.Inspect(ctx, admission.root, owner)
		}
		if err != nil {
			return result, err
		}
		if len(attributes) != len(owner.Attributes) {
			return result, errors.New("preparation adapter changed its fixed attribute census")
		}
		for index, attribute := range attributes {
			if attribute.Spec != owner.Attributes[index] || len(attribute.Raw) == 0 || len(attribute.Raw) > 4096 || uint64(len(attribute.Raw)) > request.Limits.MaxOwnerAttributeBytes-attributeBytes {
				return result, errors.New("preparation owner checkpoint exceeds or changes reviewed capacity")
			}
			attributeBytes += uint64(len(attribute.Raw))
			path := request.RootPath
			if attribute.Spec.Path != "." {
				path = filepath.Join(path, attribute.Spec.Path)
			}
			if err := self.step(attributeStep(path, attribute.Spec.Name, attribute.Raw)); err != nil {
				return result, err
			}
		}
	}
	for _, metadata := range []struct {
		path string
		raw  []byte
	}{{request.MarkerPath, plan.Marker}, {request.LeasePath, plan.Lease}} {
		if err := self.step(preparationStep{Kind: "file", Path: metadata.path, Mode: 0600, Bytes: uint64(len(metadata.raw)), Sha256: preparationDigest(metadata.raw), Raw: metadata.raw}); err != nil {
			return result, err
		}
	}
	if err := self.finalCensus(); err != nil {
		return result, err
	}
	declaration := request.declaration(plan.Root.Inode, preparationDigest(plan.Generation), preparationDigest(plan.Marker), preparationDigest(plan.Lease))
	if err := declaration.validateForScope(scope); err != nil {
		return result, err
	}
	raw, err := json.MarshalIndent(declaration, "", "  ")
	if err != nil {
		return result, err
	}
	raw = append(raw, '\n')
	if err := self.step(preparationStep{Kind: "file", Path: request.DeclarationPath, Mode: 0600, Bytes: uint64(len(raw)), Sha256: preparationDigest(raw), Raw: raw}); err != nil {
		return result, err
	}
	if self.pending != nil || self.position != len(self.completed) {
		return result, errors.Join(ErrIdentity, errors.New("preparation control retains unconsumed original intent"))
	}
	if err := self.finalCensus(); err != nil {
		return result, err
	}
	return PreparationResult{Schema: PreparationResultSchema, Plan: reference, Declaration: Reference{Path: request.DeclarationPath, Sha256: preparationDigest(raw)}, RestartAuthorized: false}, nil
}

// Filesystem type constants have exactly the same daemon/owner-local meaning.
func filesystemMagic(name string) int64 {
	switch name {
	case "ext4":
		return 0xef53
	case "xfs":
		return 0x58465342
	case "btrfs":
		return 0x9123683e
	}
	return -1
}
