//go:build linux || darwin

package durablevolume

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestInventoryOriginalRequestAndSdkOutboxSchemasAreExplicitlyBounded(t *testing.T) {
	for _, name := range []string{"user.urnetwork.validator.requests.v1", "user.urnetwork.sdk-work.v1"} {
		func() {
			fixture := newVolumeFixture(t)
			fixture.custody(t)
			raw := bytes.Repeat([]byte{'a'}, 128)
			if limit, known := ownerAttributeLimit(name); !known || limit != 4096 {
				t.Fatal("owner schema lost its reviewed bound", limit, known)
			}
			if err := unix.Setxattr(fixture.root, name, raw, unix.XATTR_CREATE); err != nil {
				t.Fatal(err)
			}
			owner := fixture.open(t, Snapshot)
			fence := fixture.fence(t)
			report, err := owner.Inventory(t.Context(), fence, inventoryCustodyLimits(t))
			if err != nil || report.TotalOwnerAttributes != 1 {
				t.Fatal("actual original owner schema was omitted", report, err)
			}
			if len(report.Entries) == 0 || len(report.Entries[0].OwnerAttributes) != 1 || report.Entries[0].OwnerAttributes[0].Name != name || !bytes.Equal(report.Entries[0].OwnerAttributes[0].Value, raw) {
				t.Fatal("original attribute bytes changed", report)
			}
			// An adjacent but unregistered name remains outside the fixed schema.
			if err := owner.Close(); err != nil {
				t.Fatal(err)
			}
			if err := unix.Setxattr(fixture.root, name+".next", raw, unix.XATTR_CREATE); err != nil {
				t.Fatal(err)
			}
			second := fixture.open(t, Snapshot)
			if _, err := second.Inventory(t.Context(), fence, inventoryCustodyLimits(t)); err == nil {
				t.Fatal("unknown adjacent schema acquired blanket owner authority")
			}
		}()
	}
}

func TestInventoryOwnerAttributeBoundsRetainSameSnapshot(t *testing.T) {
	fixture := newVolumeFixture(t)
	fixture.custody(t)
	name := "user.urnetwork.native-journal-custody"
	for _, path := range []string{fixture.root, filepath.Join(fixture.root, "journal")} {
		if err := unix.Setxattr(path, name, []byte("retained-original-custody"), unix.XATTR_CREATE); err != nil {
			t.Fatal(err)
		}
	}
	owner := fixture.open(t, Snapshot)
	fence := fixture.fence(t)
	for _, kind := range []string{"count", "bytes", "undeclared"} {
		limits := inventoryCustodyLimits(t)
		switch kind {
		case "count":
			limits.MaxOwnerAttributes = 1
		case "bytes":
			limits.MaxOwnerAttributeBytes = 1
		case "undeclared":
			limits.MaxOwnerAttributes = 0
		}
		report, err := owner.Inventory(t.Context(), fence, limits)
		if err == nil || errors.Is(err, ErrIdentity) || len(report.Entries) != 0 {
			t.Fatal("metadata budget emitted a partial report or poisoned identity", kind, report, err)
		}
		if report, err := owner.Inventory(t.Context(), fence, inventoryCustodyLimits(t)); err != nil || report.TotalOwnerAttributes != 2 {
			t.Fatal("same snapshot could not retry its declared bound", kind, report, err)
		}
	}
}

func TestInventoryOwnerAttributeObservationRecoversWithoutIdentityInference(t *testing.T) {
	for _, stage := range []string{"inventory-attributes-list", "inventory-attribute-read"} {
		for _, cause := range []error{syscall.EIO, syscall.EMFILE, syscall.EOPNOTSUPP} {
			fixture := newVolumeFixture(t)
			fixture.custody(t)
			owner := fixture.open(t, Snapshot)
			fence := fixture.fence(t)
			owner.observeFile = func(operation string, _ *os.File, _ string) error {
				if operation == stage {
					return cause
				}
				return nil
			}
			report, err := owner.Inventory(t.Context(), fence, inventoryCustodyLimits(t))
			if !errors.Is(err, ErrUnavailable) || !errors.Is(err, cause) || errors.Is(err, ErrIdentity) || len(report.Entries) != 0 {
				t.Fatal("failed attribute observation became changed identity or complete report", stage, cause, err)
			}
			owner.observeFile = nil
			if _, err := owner.Inventory(t.Context(), fence, inventoryCustodyLimits(t)); err != nil {
				t.Fatal("same retained snapshot could not recover observation", stage, cause, err)
			}
		}
	}
}

func TestInventoryOwnerAttributeAdmissionRetainsCancellation(t *testing.T) {
	fixture := newVolumeFixture(t)
	fixture.custody(t)
	owner := fixture.open(t, Snapshot)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	observed := false
	owner.observeFile = func(stage string, _ *os.File, _ string) error {
		if stage == "inventory-attribute-read" {
			observed = true
			cancel()
		}
		return nil
	}
	report, err := owner.Inventory(ctx, fixture.fence(t), inventoryCustodyLimits(t))
	if !observed || !errors.Is(err, context.Canceled) || errors.Is(err, ErrIdentity) || len(report.Entries) != 0 {
		t.Fatal("canceled attribute admission continued or changed identity", observed, report, err)
	}
	owner.observeFile = nil
	if _, err := owner.Inventory(t.Context(), fixture.fence(t), inventoryCustodyLimits(t)); err != nil {
		t.Fatal(err)
	}
}

func TestInventoryConcurrentOwnerAttributeChangePoisonsSnapshot(t *testing.T) {
	fixture := newVolumeFixture(t)
	fixture.custody(t)
	path, name := filepath.Join(fixture.root, "journal", "empty-lock"), "user.urnetwork.snapshot."+strings.Repeat("c", 64)
	original := []byte("retained-before-admission")
	if err := unix.Setxattr(path, name, original, unix.XATTR_CREATE); err != nil {
		t.Fatal(err)
	}
	owner := fixture.open(t, Snapshot)
	changed := false
	owner.observeFile = func(stage string, _ *os.File, observed string) error {
		if stage == "inventory-attribute-read" && observed == path && !changed {
			changed = true
			return unix.Setxattr(path, name, []byte("changed-after-retained-stat"), unix.XATTR_REPLACE)
		}
		return nil
	}
	report, err := owner.Inventory(t.Context(), fixture.fence(t), inventoryCustodyLimits(t))
	if !changed || !errors.Is(err, ErrIdentity) || len(report.Entries) != 0 {
		t.Fatal("concurrent metadata mutation produced a complete inventory", changed, report, err)
	}
	owner.observeFile = nil
	if err := unix.Setxattr(path, name, original, unix.XATTR_REPLACE); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Inventory(t.Context(), fixture.fence(t), inventoryCustodyLimits(t)); !errors.Is(err, ErrIdentity) {
		t.Fatal("restored bytes revived invalidated snapshot", err)
	}
}

func TestInventoryAttributeCallerFailureAndCapacityRemainDistinct(t *testing.T) {
	fixture := newVolumeFixture(t)
	path := filepath.Join(fixture.root, "lock")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	name := "user.urnetwork.native-journal-custody"
	if err := unix.Setxattr(path, name, []byte("more-than-four-bytes"), unix.XATTR_CREATE); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readInventoryAttribute(file, name, 4); err == nil || errors.Is(err, ErrIdentity) || errors.Is(err, ErrUnavailable) {
		t.Fatal("actual kernel size refusal was misclassified", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := readInventoryAttribute(file, name, 4096); err == nil || !errors.Is(err, syscall.EBADF) || errors.Is(err, ErrUnavailable) || errors.Is(err, ErrIdentity) {
		t.Fatal("closed caller descriptor became resource or identity verdict", err)
	}
}

func TestInventoryRefusesPriorSchemaAndMalformedCustodyNames(t *testing.T) {
	fixture := newVolumeFixture(t)
	fixture.custody(t)
	owner := fixture.open(t, Snapshot)
	fence := fixture.fence(t)
	limits := inventoryCustodyLimits(t)
	report, err := owner.Inventory(t.Context(), fence, limits)
	if err != nil {
		t.Fatal(err)
	}
	report.Schema = "urnetwork-durable-volume-inventory-v2"
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(fixture.mount), "old-inventory.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.VerifyInventory(t.Context(), Reference{Path: path, Sha256: testDigest(raw)}, fence, limits); err == nil {
		t.Fatal("old file-only schema became complete owner custody")
	}
	for _, name := range []string{"user.urnetwork.snapshot.short", "user.urnetwork.snapshot." + strings.Repeat("A", 64)} {
		if err := unix.Setxattr(fixture.root, name, []byte("original"), unix.XATTR_CREATE); err != nil {
			t.Fatal(err)
		}
		if _, err := owner.Inventory(t.Context(), fence, limits); err == nil {
			t.Fatal("malformed owner attribute was omitted", name)
		}
		if err := unix.Removexattr(fixture.root, name); err != nil {
			t.Fatal(err)
		}
	}
}
