//go:build darwin

package connect

import (
	"bytes"
	"errors"
	"os"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// Root cause: x/sys's Darwin Fsetxattr dropped XATTR_REPLACE and XATTR_CREATE,
// so a deleted outbox anchor was silently re-created and a fresh anchor could
// overwrite an existing one. Both conditions must reach the kernel.
func TestOriginalWorkOutboxAttributeKeepsCreateAndReplaceConditions(t *testing.T) {
	directory, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	read := func() ([]byte, error) {
		raw := make([]byte, 256)
		n, err := unix.Fgetxattr(int(directory.Fd()), OriginalWorkOutboxAttribute, raw)
		if err != nil {
			return nil, err
		}
		return raw[:n], nil
	}
	err = replaceOriginalWorkOutboxAttribute(directory, []byte("replacement"), false)
	if !originalWorkOutboxAttributeAbsent(err) {
		t.Fatalf("replace of an absent anchor returned %v, want absence", err)
	}
	if value, err := read(); !originalWorkOutboxAttributeAbsent(err) {
		t.Fatalf("refused replace enrolled anchor %q %v", value, err)
	}
	if err := replaceOriginalWorkOutboxAttribute(directory, []byte("original"), true); err != nil {
		t.Fatal(err)
	}
	if err := replaceOriginalWorkOutboxAttribute(directory, []byte("competing"), true); !errors.Is(err, syscall.EEXIST) {
		t.Fatalf("fresh anchor over an existing anchor returned %v, want EEXIST", err)
	}
	if value, err := read(); err != nil || !bytes.Equal(value, []byte("original")) {
		t.Fatalf("refused fresh anchor changed the original to %q %v", value, err)
	}
	if err := replaceOriginalWorkOutboxAttribute(directory, []byte("advanced"), false); err != nil {
		t.Fatal(err)
	}
	if value, err := read(); err != nil || !bytes.Equal(value, []byte("advanced")) {
		t.Fatalf("replace of an existing anchor left %q %v", value, err)
	}
}
