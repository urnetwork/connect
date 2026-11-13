//go:build linux || darwin

package durablevolume

import (
	"bytes"
	"testing"

	"golang.org/x/sys/unix"
)

// Exact newly prepared namespaces enter full inventory; nearby schemas do not.
func TestInventoryCreationAndProviderPublicationBirthSchemasAreBounded(t *testing.T) {
	for _, name := range []string{"user.urnetwork.original-contracts.v1", "user.urnetwork.validator.publications.v1"} {
		func() {
			fixture := newVolumeFixture(t)
			fixture.custody(t)
			raw := bytes.Repeat([]byte{'b'}, 128)
			if limit, known := ownerAttributeLimit(name); !known || limit != 4096 {
				t.Fatal("new original birth schema lost exact capacity", name, limit, known)
			}
			if err := unix.Setxattr(fixture.root, name, raw, unix.XATTR_CREATE); err != nil {
				t.Fatal(err)
			}
			owner := fixture.open(t, Snapshot)
			report, err := owner.Inventory(t.Context(), fixture.fence(t), inventoryCustodyLimits(t))
			if err != nil || report.TotalOwnerAttributes != 1 || len(report.Entries[0].OwnerAttributes) != 1 || report.Entries[0].OwnerAttributes[0].Name != name || !bytes.Equal(report.Entries[0].OwnerAttributes[0].Value, raw) {
				t.Fatal("complete original birth was omitted or changed", name, err)
			}
			if err := owner.Close(); err != nil {
				t.Fatal(err)
			}
			if err := unix.Setxattr(fixture.root, name+".other", raw, unix.XATTR_CREATE); err != nil {
				t.Fatal(err)
			}
			second := fixture.open(t, Snapshot)
			defer second.Close()
			if _, err := second.Inventory(t.Context(), fixture.fence(t), inventoryCustodyLimits(t)); err == nil {
				t.Fatal("unknown adjacent schema acquired original custody authority", name)
			}
		}()
	}
}
