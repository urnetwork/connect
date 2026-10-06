//go:build linux || darwin

// Restored physical censuses bind reviewed staging inodes. Their members move
// without replacement; a retry joins only the exact source/target transition,
// never invents missing bytes or replaces a competing published generation.
package durablevolume

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"syscall"

	"golang.org/x/sys/unix"
)

// Every move is already a hash-linked pending step before its first syscall.
// Both parents are synced before acknowledgement; completed loss stays loss.
func (self *preparationApply) observeMovedRestoreFile(step preparationStep, pending bool) (_ PreparationIdentity, resultErr error) {
	if self.admission.request.Purpose != "restore" || step.Kind != "file" || step.Source == nil || !step.Move {
		return PreparationIdentity{}, errors.New("physical member move requires its exact restore step")
	}
	reviewed, present := self.moveSources[step.Source.Path]
	expected := preparationStep{Kind: "file", Path: filepath.Join(self.admission.request.RootPath, reviewed.File.Path),
		Mode: reviewed.File.Mode, Bytes: reviewed.File.Bytes, Sha256: reviewed.File.Sha256, Source: &reviewed, Move: true}
	if !present || !reflect.DeepEqual(expected, step) {
		return PreparationIdentity{}, errors.Join(ErrIdentity, errors.New("physical member move changed its reviewed source"))
	}
	mutated := false
	defer func() {
		if mutated && resultErr != nil {
			resultErr = self.uncertain(resultErr)
		}
	}()
	sourceParent, err := preparationOpenAbsolute(filepath.Dir(step.Source.Path), true)
	if err != nil {
		return PreparationIdentity{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, sourceParent.Close()) }()
	targetParent, targetName, err := self.parent(step.Path)
	if err != nil {
		return PreparationIdentity{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, targetParent.Close()) }()
	open := func(parent *os.File, name string) (*os.File, error) {
		fd, err := unix.Openat(int(parent.Fd()), name, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if errors.Is(err, syscall.ENOENT) {
			return nil, nil
		}
		if err != nil {
			return nil, namedObservation("physical restore member could not be observed", err)
		}
		file := os.NewFile(uintptr(fd), filepath.Join(parent.Name(), name))
		if err := preparationPrivate(file, false); err != nil {
			return nil, errors.Join(err, file.Close())
		}
		return file, nil
	}
	sourceName := filepath.Base(step.Source.Path)
	source, err := open(sourceParent, sourceName)
	if err != nil {
		return PreparationIdentity{}, err
	}
	if source != nil {
		defer func() { resultErr = errors.Join(resultErr, source.Close()) }()
	}
	target, err := open(targetParent, targetName)
	if err != nil {
		return PreparationIdentity{}, err
	}
	if target != nil {
		defer func() { resultErr = errors.Join(resultErr, target.Close()) }()
	}
	if source != nil && target != nil || source == nil && target == nil {
		return PreparationIdentity{}, errors.Join(ErrIdentity, errors.New("physical restore source and target are ambiguous or both lost"))
	}
	if !pending && source != nil {
		return PreparationIdentity{}, errors.Join(ErrIdentity, errors.New("completed physical restore member moved back or disappeared"))
	}
	file := target
	if file == nil {
		file = source
	}
	identity, err := preparationIdentity(file)
	if err != nil {
		return PreparationIdentity{}, err
	}
	if identity != step.Source.Identity || identity.Mode&0777 != step.Mode || deviceNumber(identity.Device) != self.admission.mount.Device {
		return PreparationIdentity{}, errors.Join(ErrIdentity, errors.New("physical restore member generation or protection differs"))
	}
	if err := preparationVerifyFile(self.admission.ctx, file, step.Bytes, step.Sha256); err != nil {
		return PreparationIdentity{}, err
	}
	if source != nil {
		if err := errors.Join(self.sameMember(source, sourceParent, sourceName), self.check()); err != nil {
			return PreparationIdentity{}, err
		}
		if err := preparationRenameRoot(sourceParent, sourceName, targetParent, targetName); err != nil {
			if errors.Is(err, syscall.EEXIST) {
				return PreparationIdentity{}, errors.Join(ErrIdentity, errors.New("physical restore target was claimed before publication"), err)
			}
			return PreparationIdentity{}, self.uncertain(err)
		}
		mutated = true
		if err := self.after("restore-member-rename", step.Path); err != nil {
			return PreparationIdentity{}, err
		}
	}
	if err := errors.Join(self.sameMember(file, targetParent, targetName), preparationAbsent(sourceParent, sourceName)); err != nil {
		return PreparationIdentity{}, err
	}
	if pending {
		mutated = true
		if err := errors.Join(file.Sync(), sourceParent.Sync()); err != nil {
			return PreparationIdentity{}, err
		}
		if err := self.after("restore-source-parent-sync", step.Path); err != nil {
			return PreparationIdentity{}, err
		}
		if err := targetParent.Sync(); err != nil {
			return PreparationIdentity{}, err
		}
		if err := self.after("restore-target-parent-sync", step.Path); err != nil {
			return PreparationIdentity{}, err
		}
	}
	if err := errors.Join(self.sameMember(file, targetParent, targetName), sameNamedFile(sourceParent, filepath.Dir(step.Source.Path))); err != nil {
		return PreparationIdentity{}, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return PreparationIdentity{}, unavailableObservation("published physical member could not be observed", err)
	}
	self.retained[step.Path] = stat
	return identity, self.check()
}
