//go:build linux

// Explicit fresh root creation stages one private inode for review. Accepted
// apply reserves that original inode/control before a no-replace move; a lost
// completed root is never created again or inferred fresh from its absence.
package durablevolume

import (
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"unsafe"
)

// Linux amd64/arm64 are the prepared runtime targets. No rename fallback may
// overwrite a competing target on an unqualified platform.
func preparationRootRenameNumber() (uintptr, error) {
	switch runtime.GOARCH {
	case "amd64":
		return 316, nil
	case "arm64":
		return 276, nil
	default:
		return 0, ErrUnsupported
	}
}

// Only the atomic Linux no-replace operation may publish a reviewed root.
func preparationRenameRoot(source *os.File, sourceName string, target *os.File, targetName string) error {
	number, err := preparationRootRenameNumber()
	if err != nil {
		return err
	}
	old, err := syscall.BytePtrFromString(sourceName)
	if err != nil {
		return err
	}
	next, err := syscall.BytePtrFromString(targetName)
	if err != nil {
		return err
	}
	_, _, errno := syscall.Syscall6(number, source.Fd(), uintptr(unsafe.Pointer(old)), target.Fd(), uintptr(unsafe.Pointer(next)), 1, 0)
	runtime.KeepAlive(source)
	runtime.KeepAlive(target)
	runtime.KeepAlive(old)
	runtime.KeepAlive(next)
	if errno != 0 {
		return errno
	}
	return nil
}

// The accepted nonce selects a single stage; user paths cannot name another.
func preparationStagedRootPath(request PreparationRequest, nonce []byte) string {
	return filepath.Join(request.StagingDirectory, "preparation-root-"+hex.EncodeToString(nonce))
}

// A new root is fenced by its parent and explicit zero historical custody.
// Precreated roots retain their original exact root-inode fence semantics.
func (self *preparationAdmission) fenceRootMatches(fence PreparationFence) bool {
	if self.request.RootCreation == "create-private" {
		return fence.RootInode == 0 && fence.ParentInode != 0 && fence.ParentInode == self.identities[filepath.Dir(self.request.RootPath)].Inode
	}
	return fence.ParentInode == 0 && fence.RootInode == self.identities[self.request.RootPath].Inode
}

// Before stage construction the target must remain absent, not an empty alias.
func (self *preparationAdmission) freshRoot() error {
	if self.root == nil {
		if self.request.RootCreation != "create-private" {
			return errors.New("fresh root descriptor is absent")
		}
		return preparationAbsent(self.directories[filepath.Dir(self.request.RootPath)], filepath.Base(self.request.RootPath))
	}
	return errors.Join(preparationEmpty(self.ctx, self.root), preparationRequireNoAttributes(self.root))
}

// The new directory exists only in staging until exact plan application. Its
// inode is included in the returned plan; a later same-byte replacement fails.
func (self *preparationAdmission) stageRoot(nonce []byte) error {
	if self.root != nil || self.request.RootCreation != "create-private" || len(nonce) != 32 {
		return errors.New("private root staging scope differs")
	}
	if err := self.check(); err != nil {
		return err
	}
	parent := self.directories[self.request.StagingDirectory]
	path := preparationStagedRootPath(self.request, nonce)
	name := filepath.Base(path)
	if err := syscall.Mkdirat(int(parent.Fd()), name, 0700); err != nil {
		return err
	}
	file, err := preparationOpenAbsolute(path, true)
	if err != nil {
		return err
	}
	self.root = file
	self.rootSource = path
	self.directories[self.request.RootPath] = file
	identity, err := preparationIdentity(file)
	if err != nil {
		return err
	}
	self.identities[self.request.RootPath] = identity
	if identity.Mode&07777 != 0700 {
		return errors.Join(ErrIdentity, errors.New("staged root is not exactly private"))
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.Join(ErrBusy, err)
	}
	return errors.Join(file.Sync(), parent.Sync(), self.check())
}

// Reopen selects either the original staged inode or a retained moved inode.
// Both missing, both present, or an unreserved preexisting target are refused.
func (self *preparationAdmission) openStagedRoot(source string) error {
	return self.openStagedRootWithObservation(source, nil)
}

// The instance-owned barrier exercises failures immediately before the actual
// moved-root attribute read. Production calls supply no observer or substitute.
func (self *preparationAdmission) openStagedRootWithObservation(source string, observe func(*os.File) error) error {
	request := self.request
	if !canonical(source) || filepath.Dir(source) != request.StagingDirectory || !strings.HasPrefix(filepath.Base(source), "preparation-root-") {
		return errors.New("staged preparation root is outside its exact namespace")
	}
	file, err := preparationOpenAbsolute(source, true)
	atTarget := false
	if errors.Is(err, syscall.ENOENT) {
		file, err = preparationOpenAbsolute(request.RootPath, true)
		atTarget = true
		if err != nil {
			return err
		}
		var raw []byte
		var readErr error
		if observe != nil {
			readErr = observe(file)
		}
		if readErr = errors.Join(readErr, self.ctx.Err()); readErr == nil {
			raw, readErr = readInventoryAttribute(file, PreparationAttribute, 4096)
		}
		if readErr != nil {
			return errors.Join(readErr, file.Close())
		}
		if len(raw) == 0 {
			return errors.Join(ErrIdentity, errors.New("moved preparation root lacks its original reservation"), file.Close())
		}
	} else if err != nil {
		return err
	}
	self.root, self.rootSource, self.rootAtTarget = file, source, atTarget
	self.directories[request.RootPath] = file
	identity, err := preparationIdentity(file)
	if err != nil {
		return err
	}
	self.identities[request.RootPath] = identity
	return nil
}

// The root move is its own write-ahead step, before any runtime owner member.
func preparationRootStep(request PreparationRequest, plan PreparationPlan) preparationStep {
	return preparationStep{Kind: "root-directory", Path: request.RootPath, Mode: 0700,
		Source: &PreparationSource{Path: plan.RootSource, Identity: plan.Root, File: PreparationFile{Path: ".", Kind: "directory", Mode: 0700}}}
}

// Before first publication only the exact preparation reservation may exist.
// Unknown staged content remains intact and cannot become runtime custody.
func (self *preparationApply) freshReservedRoot() error {
	if err := preparationEmpty(self.admission.ctx, self.admission.root); err != nil {
		return err
	}
	names, err := listInventoryAttributes(self.admission.root)
	if err != nil {
		return err
	}
	for _, name := range names {
		if strings.HasPrefix(name, ownerAttributeNamespace) && name != PreparationAttribute {
			return errors.Join(ErrIdentity, errors.New("staged root contains unreviewed custody metadata"))
		}
	}
	return self.check()
}

// An exact pending move can be synced after the failed invocation joins. The
// source inode, both parent identities, and reservation remain authoritative.
func (self *preparationApply) observeRoot(step preparationStep, pending bool) (_ PreparationIdentity, resultErr error) {
	request := self.admission.request
	if request.RootCreation != "create-private" || !reflect.DeepEqual(step, preparationRootStep(request, self.plan)) {
		return PreparationIdentity{}, errors.Join(ErrIdentity, errors.New("private root move differs from its accepted plan"))
	}
	mutated := false
	defer func() {
		if mutated && resultErr != nil {
			resultErr = self.uncertain(resultErr)
		}
	}()
	if !pending && !self.admission.rootAtTarget {
		return PreparationIdentity{}, errors.Join(ErrIdentity, errors.New("completed private root was moved or lost"))
	}
	sourceParent := self.admission.directories[request.StagingDirectory]
	targetParent := self.admission.directories[filepath.Dir(request.RootPath)]
	if pending {
		if err := self.freshReservedRoot(); err != nil {
			return PreparationIdentity{}, err
		}
		if !self.admission.rootAtTarget {
			if err := preparationRenameRoot(sourceParent, filepath.Base(self.plan.RootSource), targetParent, filepath.Base(request.RootPath)); err != nil {
				if errors.Is(err, syscall.EEXIST) {
					return PreparationIdentity{}, errors.Join(ErrIdentity, errors.New("reviewed private root target was claimed"), err)
				}
				return PreparationIdentity{}, self.uncertain(err)
			}
			mutated = true
			self.admission.rootAtTarget = true
			if err := self.after("root-rename", request.RootPath); err != nil {
				return PreparationIdentity{}, err
			}
		}
		mutated = true
		if err := errors.Join(self.admission.root.Sync(), sourceParent.Sync()); err != nil {
			return PreparationIdentity{}, err
		}
		if err := self.after("root-source-parent-sync", request.RootPath); err != nil {
			return PreparationIdentity{}, err
		}
		if err := targetParent.Sync(); err != nil {
			return PreparationIdentity{}, err
		}
		if err := self.after("root-target-parent-sync", request.RootPath); err != nil {
			return PreparationIdentity{}, err
		}
	}
	if err := self.check(); err != nil {
		return PreparationIdentity{}, err
	}
	identity, err := preparationIdentity(self.admission.root)
	if err != nil {
		return PreparationIdentity{}, err
	}
	if identity != self.plan.Root {
		return PreparationIdentity{}, errors.Join(ErrIdentity, errors.New("private root inode changed across publication"))
	}
	return identity, nil
}
