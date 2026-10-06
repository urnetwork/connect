//go:build linux

// Initial kernel observations have the same retry contract as retained owner
// checks. Constructor failure must also release its already-acquired lease.
package durablevolume

import (
	"bytes"
	"errors"
	"os"
	"syscall"
	"testing"
)

// Neither a first statfs failure nor a failed mount/UUID census proves changed
// custody. A subsequent exact opening must retain all bytes and acquire its lease.
func TestOwnerConstructorObservationRefusalRetainsRetryAndCustody(t *testing.T) {
	for _, local := range []bool{false, true} {
		fixture := newVolumeFixture(t)
		open := OpenWithHost
		if local {
			fixture.config.Schema = OwnerLocalSchema
			fixture.writeConfig(t)
			open = OpenOwnerLocalWithHost
		}
		paths := []string{fixture.reference.Path, fixture.marker, fixture.config.Volumes[0].StateRoots[0].LeasePath}
		original := map[string][]byte{}
		for _, path := range paths {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			original[path] = raw
		}
		for _, stage := range []string{"mounts", "uuid", "filesystem"} {
			for _, cause := range []error{syscall.EIO, syscall.EMFILE} {
				fixture.host.change(func() {
					switch stage {
					case "mounts":
						fixture.host.mountsErr = cause
					case "uuid":
						fixture.host.uuidErr = cause
					case "filesystem":
						fixture.host.filesystemErr = cause
					}
				})
				owner, err := open(fixture.reference, fixture.root, ReadWrite, fixture.host)
				if owner != nil || !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrIdentity) || !errors.Is(err, cause) {
					if owner != nil {
						owner.Close()
					}
					t.Errorf("initial %s observation local=%v cause=%v lost retry classification: %v", stage, local, cause, err)
				}
				fixture.host.change(func() { fixture.host.mountsErr = nil; fixture.host.uuidErr = nil; fixture.host.filesystemErr = nil })
				// Snapshot's exclusive lease proves that the failed constructor joined.
				owner, err = open(fixture.reference, fixture.root, Snapshot, fixture.host)
				if err != nil {
					t.Fatal("initial observation failure retained its lease", stage, local, err)
				}
				if err := owner.CheckRead(); err != nil {
					t.Fatal(err)
				}
				if err := owner.Close(); err != nil {
					t.Fatal(err)
				}
				for _, path := range paths {
					raw, err := os.ReadFile(path)
					if err != nil || !bytes.Equal(raw, original[path]) {
						t.Fatalf("constructor changed custody %s: %v", path, err)
					}
				}
			}
		}
	}
}

// An invalid descriptor supplied by a host is a caller failure, not a proven
// replacement or a retryable kernel resource-pressure observation.
func TestOwnerConstructorInvalidDescriptorRemainsCallerFailure(t *testing.T) {
	fixture := newVolumeFixture(t)
	fixture.host.change(func() { fixture.host.filesystemErr = syscall.EBADF })
	owner, err := OpenWithHost(fixture.reference, fixture.root, ReadWrite, fixture.host)
	if owner != nil || !errors.Is(err, syscall.EBADF) || errors.Is(err, ErrIdentity) || errors.Is(err, ErrUnavailable) {
		if owner != nil {
			owner.Close()
		}
		t.Fatal("invalid initial descriptor changed error scope", err)
	}
	fixture.host.change(func() { fixture.host.filesystemErr = nil })
	owner, err = OpenWithHost(fixture.reference, fixture.root, Snapshot, fixture.host)
	if err != nil {
		t.Fatal("failed initial caller retained its lease", err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
}
