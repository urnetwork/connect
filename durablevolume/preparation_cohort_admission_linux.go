//go:build linux

// Cohort admission retains every original control and verifies its exact plan
// prefix without appending, syncing, creating or repairing any target member.
package durablevolume

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
)

var errPreparationIncomplete = errors.New("preparation has an unacknowledged next step")

// An absent control is admitted only under the original explicit fresh-target
// fence and a wholly empty target. Existing journal bytes stay read-only here.
func (self *preparationApply) preflightControl() error {
	request := self.admission.request
	parent := self.admission.directories[filepath.Dir(request.ControlPath)]
	fd, err := syscall.Openat(int(parent.Fd()), filepath.Base(request.ControlPath), syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, syscall.ENOENT) {
		if err := errors.Join(preparationEmpty(self.admission.ctx, self.admission.root), preparationRequireNoAttributes(self.admission.root)); err != nil {
			return err
		}
		for _, path := range []string{request.MarkerPath, request.LeasePath, request.DeclarationPath} {
			if err := preparationAbsent(self.admission.directories[filepath.Dir(path)], filepath.Base(path)); err != nil {
				return err
			}
		}
		return self.check()
	}
	if err != nil {
		return namedObservation("cohort control could not be observed", err)
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
	if err := self.readControl(); err != nil {
		return err
	}
	if self.header.Schema != preparationControlSchema || self.header.PlanSha256 != self.reference.Sha256 || self.header.CohortSha256 != self.cohort.Sha256 || self.header.Root != self.plan.Root || self.header.Control != identity {
		return errors.Join(ErrIdentity, errors.New("cohort control differs from its accepted plan and generation"))
	}
	if err := syscall.Fstat(fd, &self.controlStat); err != nil {
		return unavailableObservation("cohort control stat is unavailable", err)
	}
	anchor, err := json.Marshal(preparationAnchor{Schema: preparationAnchorSchema, PlanSha256: self.reference.Sha256, CohortSha256: self.cohort.Sha256, RootInode: self.plan.Root.Inode, Control: identity})
	if err != nil {
		return err
	}
	retained, err := readInventoryAttribute(self.admission.root, PreparationAttribute, 4096)
	if errors.Is(err, syscall.ENODATA) {
		if self.sequence != 0 {
			return errors.Join(ErrIdentity, errors.New("cohort control lost its completed root reservation"))
		}
		if err := errors.Join(preparationEmpty(self.admission.ctx, self.admission.root), preparationRequireNoAttributes(self.admission.root)); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if !bytes.Equal(retained, anchor) {
		return errors.Join(ErrIdentity, errors.New("cohort root reservation names another physical control"))
	} else {
		self.anchor = anchor
	}
	return self.check()
}

// A missing future step is not an integrity failure. An existing pending step
// must be byte-exact; no observation in this method writes or acknowledges it.
func (self *preparationApply) preflightNextStep(step preparationStep) error {
	if self.pending == nil {
		if err := self.requireAbsent(step); err != nil {
			return err
		}
		return errPreparationIncomplete
	}
	if !reflect.DeepEqual(self.pending.Step, step) {
		return errors.Join(ErrIdentity, errors.New("cohort pending intent differs from its accepted plan"))
	}
	if step.Kind == "root-directory" && !self.admission.rootAtTarget {
		if err := self.freshReservedRoot(); err != nil {
			return err
		}
		return errPreparationIncomplete
	}
	if step.Move && self.requireAbsent(step) == nil {
		// An original staged inode may still await its no-replace move.
		// Validate it without calling the mutation-capable observer.
		file, err := preparationOpenAbsolute(step.Source.Path, false)
		if err != nil {
			return err
		}
		identity, observeErr := preparationIdentity(file)
		if observeErr == nil && identity != step.Source.Identity {
			observeErr = errors.Join(ErrIdentity, errors.New("cohort pending staged inode changed"))
		}
		if observeErr == nil {
			observeErr = preparationVerifyFile(self.admission.ctx, file, step.Bytes, step.Sha256)
		}
		if err := errors.Join(observeErr, file.Close()); err != nil {
			return err
		}
		return errPreparationIncomplete
	}
	_, err := self.observe(step, false)
	if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENODATA) {
		// Absence must belong to this exact pending destination. Missing
		// acknowledged ancestors or a present competing target still refuse.
		if err := self.requireAbsent(step); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	return errPreparationIncomplete
}

// Reuse the publication sequence to authenticate the whole retained prefix,
// then reject unreserved members and metadata even when later steps are absent.
func (self *preparationApply) preflight() (resultErr error) {
	if err := self.preflightControl(); err != nil {
		return err
	}
	self.readOnly = true
	defer func() { self.readOnly, self.position = false, 0 }()
	prepared, err := self.run()
	self.complete = err == nil
	if self.complete {
		self.prepared = prepared
	}
	if err != nil && !errors.Is(err, errPreparationIncomplete) {
		return err
	}
	if err := self.census(true); err != nil {
		return err
	}
	request := self.admission.request
	for _, path := range []string{request.MarkerPath, request.LeasePath, request.DeclarationPath} {
		if _, present := self.retained[path]; !present {
			if err := preparationAbsent(self.admission.directories[filepath.Dir(path)], filepath.Base(path)); err != nil {
				return err
			}
		}
	}
	return errors.Join(self.check(), self.checkRestore())
}
