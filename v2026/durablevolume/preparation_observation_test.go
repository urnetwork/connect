//go:build linux || darwin

// Real staged plans and interrupted root moves distinguish failed observations
// from observed different bytes. Every recovery retains its original journal.
package durablevolume

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestPreparationPlanRetentionReadFailurePreservesOriginalInputs(t *testing.T) {
	f := newPreparationFixture(t)
	f.build(t)
	retained, err := RetainPreparationPlan(t.Context(), f.plan)
	if err != nil {
		t.Fatal("exact plan retention baseline failed", err)
	}
	planRaw, err := os.ReadFile(retained.Path)
	if err != nil {
		t.Fatal(err)
	}
	requestRaw, err := os.ReadFile(f.reference.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, fault := range []string{"canceled", "missing"} {
		func() {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cause := error(context.Canceled)
			if fault == "canceled" {
				cancel()
			} else {
				cause = os.ErrNotExist
				if err := os.Remove(f.reference.Path); err != nil {
					t.Fatal(err)
				}
			}
			result, err := RetainPreparationPlan(ctx, f.plan)
			if result != (Reference{}) || !errors.Is(err, cause) || strings.Contains(err.Error(), "differs") {
				t.Fatal("unread cohort request invented changed staging identity", fault, result, err)
			}
			actual, readErr := os.ReadFile(retained.Path)
			if readErr != nil || !bytes.Equal(actual, planRaw) {
				t.Fatal("failed request read changed retained original plan", fault, readErr)
			}
			if fault == "missing" {
				if err := os.WriteFile(f.reference.Path, requestRaw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			resumed, err := RetainPreparationPlan(t.Context(), f.plan)
			if err != nil || resumed != retained {
				t.Fatal("healthy read could not reuse exact retained plan", fault, resumed, err)
			}
		}()
	}
}

func TestPreparationPlanRetentionObservedDifferenceRemainsRefusal(t *testing.T) {
	f := newPreparationFixture(t)
	f.build(t)
	retained, err := RetainPreparationPlan(t.Context(), f.plan)
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(f.reference.Path)
	if err != nil {
		t.Fatal(err)
	}
	planRaw, err := os.ReadFile(retained.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.reference.Path, append(bytes.Clone(original), '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := RetainPreparationPlan(t.Context(), f.plan)
	if result != (Reference{}) || err == nil || !strings.Contains(err.Error(), "bytes differ from the accepted hash") {
		t.Fatal("observed changed cohort request lost its exact refusal", result, err)
	}
	actual, readErr := os.ReadFile(retained.Path)
	if readErr != nil || !bytes.Equal(actual, planRaw) {
		t.Fatal("different request replaced original retained plan", readErr)
	}
	if err := os.WriteFile(f.reference.Path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if resumed, err := RetainPreparationPlan(t.Context(), f.plan); err != nil || resumed != retained {
		t.Fatal("restored original request lost exact artifact continuity", resumed, err)
	}
}

type preparationMovedReadFixture struct {
	fixture *preparationFixture
	control []byte
	anchor  []byte
	inode   uint64
}

// The public implementation performs the actual no-replace rename, then loses
// its acknowledgment. No test supplies a fabricated checkpoint or moved inode.
func newPreparationMovedReadFixture(t *testing.T) *preparationMovedReadFixture {
	t.Helper()
	f := newPrivateRootPreparationFixture(t)
	privateRootPreparationPlan(t, f)
	fired := false
	_, err := f.apply(t.Context(), &preparationHooks{after: func(stage, path string) error {
		if stage == "root-rename" && path == f.request.RootPath {
			fired = true
			return syscall.EIO
		}
		return nil
	}})
	if !fired || !errors.Is(err, ErrPreparationUncertain) || !errors.Is(err, syscall.EIO) {
		t.Fatal("real root move did not retain its uncertain publication", fired, err)
	}
	control, err := os.ReadFile(f.request.ControlPath)
	if err != nil {
		t.Fatal(err)
	}
	anchor := make([]byte, 4096)
	n, err := unix.Getxattr(f.request.RootPath, PreparationAttribute, anchor)
	if err != nil || n == 0 {
		t.Fatal("moved root did not retain its original reservation", n, err)
	}
	var stat unix.Stat_t
	if err := unix.Stat(f.request.RootPath, &stat); err != nil || stat.Ino != f.plan.Root.Inode {
		t.Fatal("actual pending move changed its admitted inode", err)
	}
	return &preparationMovedReadFixture{fixture: f, control: control, anchor: anchor[:n], inode: stat.Ino}
}

func (self *preparationMovedReadFixture) unchanged(t *testing.T) {
	t.Helper()
	f := self.fixture
	control, err := os.ReadFile(f.request.ControlPath)
	if err != nil || !bytes.Equal(control, self.control) {
		t.Fatal("failed observation changed original pending journal", err)
	}
	raw := make([]byte, 4096)
	n, err := unix.Getxattr(f.request.RootPath, PreparationAttribute, raw)
	if err != nil || !bytes.Equal(raw[:n], self.anchor) {
		t.Fatal("failed observation changed original root reservation", err)
	}
	var stat unix.Stat_t
	if err := unix.Stat(f.request.RootPath, &stat); err != nil || stat.Ino != self.inode {
		t.Fatal("failed observation replaced original moved inode", err)
	}
}

func (self *preparationMovedReadFixture) resume(t *testing.T) {
	t.Helper()
	f := self.fixture
	result, err := f.apply(t.Context(), nil)
	if err != nil || result.RestartAuthorized {
		t.Fatal("healthy public retry could not reconcile exact pending root", err)
	}
	control, err := os.ReadFile(f.request.ControlPath)
	if err != nil || !bytes.HasPrefix(control, self.control) {
		t.Fatal("healthy recovery lost original write-ahead prefix", err)
	}
	again, err := f.apply(t.Context(), nil)
	after, readErr := os.ReadFile(f.request.ControlPath)
	if err != nil || readErr != nil || result != again || !bytes.Equal(control, after) {
		t.Fatal("completed exact replay changed custody or duplicated journal", err, readErr)
	}
	var stat unix.Stat_t
	if err := unix.Stat(f.request.RootPath, &stat); err != nil || stat.Ino != self.inode {
		t.Fatal("healthy recovery replaced original moved root", err)
	}
}

func TestPreparationMovedRootReadFailurePreservesPendingRecovery(t *testing.T) {
	for _, fault := range []string{"io", "closed", "canceled"} {
		func() {
			f := newPreparationMovedReadFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			admission := &preparationAdmission{ctx: ctx, request: f.fixture.request, directories: map[string]*os.File{}, identities: map[string]PreparationIdentity{}}
			var observed *os.File
			cause := error(syscall.EIO)
			if fault == "closed" {
				cause = syscall.EBADF
			} else if fault == "canceled" {
				cause = context.Canceled
			}
			err := admission.openStagedRootWithObservation(f.fixture.plan.RootSource, func(file *os.File) error {
				observed = file
				if file.Name() != f.fixture.request.RootPath {
					t.Fatal("fault did not reach the actual moved original root", file.Name())
				}
				switch fault {
				case "io":
					return inventoryAttributeObservation(syscall.EIO)
				case "closed":
					return file.Close()
				case "canceled":
					cancel()
				}
				return nil
			})
			if observed == nil || !errors.Is(err, cause) || errors.Is(err, ErrIdentity) || admission.root != nil || strings.Contains(err.Error(), "lacks its original reservation") {
				t.Fatal("failed moved-root read invented missing reservation", fault, err)
			}
			if _, err := observed.Stat(); !errors.Is(err, os.ErrClosed) {
				t.Fatal("failed moved-root admission leaked its descriptor", fault, err)
			}
			if err := admission.close(); err != nil {
				t.Fatal(err)
			}
			f.unchanged(t)
			f.resume(t)
		}()
	}
}

func TestPreparationMovedRootObservedReservationLossRemainsIdentity(t *testing.T) {
	for _, fault := range []string{"absent", "empty", "different"} {
		f := newPreparationMovedReadFixture(t)
		root := f.fixture.request.RootPath
		var err error
		switch fault {
		case "absent":
			err = unix.Removexattr(root, PreparationAttribute)
		case "empty":
			err = unix.Setxattr(root, PreparationAttribute, nil, unix.XATTR_REPLACE)
		case "different":
			err = unix.Setxattr(root, PreparationAttribute, []byte(`{"synthetic":"different reservation"}`), unix.XATTR_REPLACE)
		}
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.fixture.apply(t.Context(), nil)
		if !errors.Is(err, ErrIdentity) {
			t.Fatal("observed original reservation loss was treated as a read outage", fault, err)
		}
		control, err := os.ReadFile(f.fixture.request.ControlPath)
		if err != nil || !bytes.Equal(control, f.control) {
			t.Fatal("observed missing reservation changed original control", fault, err)
		}
		if err := unix.Setxattr(root, PreparationAttribute, f.anchor, 0); err != nil {
			t.Fatal(err)
		}
		f.unchanged(t)
		f.resume(t)
	}
}
