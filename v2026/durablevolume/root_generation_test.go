//go:build linux || darwin

// A closed process cannot silently enroll an empty replacement state root.
package durablevolume

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// Filesystem and external lease identity alone do not prove journal continuity.
func TestOwnerReopenRejectsEmptyReplacementRoot(t *testing.T) {
	for _, access := range []Access{ReadOnly, ReadWrite, Snapshot} {
		fixture := newVolumeFixture(t)
		owner := fixture.open(t, ReadWrite)
		if err := os.WriteFile(filepath.Join(fixture.root, "completed"), []byte("retained synthetic receipt\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := owner.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(fixture.root, fixture.root+"-retained"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(fixture.root, 0700); err != nil {
			t.Fatal(err)
		}
		reopened, err := OpenWithHost(fixture.reference, fixture.root, access, fixture.host)
		if reopened != nil {
			reopened.Close()
		}
		if !errors.Is(err, ErrIdentity) {
			t.Errorf("access %v silently admitted empty replacement: %v", access, err)
		}
		if err := os.Remove(fixture.root); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(fixture.root+"-retained", fixture.root); err != nil {
			t.Fatal(err)
		}
		retained := fixture.open(t, access)
		if err := retained.Check(); err != nil {
			t.Fatal("original root cannot reopen", err)
		}
	}
}

// A fresh nonce is required even if a recycled inode number equals the binding.
func TestOwnerRootGenerationMismatchPoisonsSameInode(t *testing.T) {
	for _, replacement := range []string{"missing", "wrong", "short", "long", "oversized"} {
		fixture := newVolumeFixture(t)
		owner := fixture.open(t, ReadWrite)
		original, err := readRootGeneration(owner.rootFile)
		if err != nil {
			t.Fatal(err)
		}
		var before, after unix.Stat_t
		if err := unix.Stat(fixture.root, &before); err != nil {
			t.Fatal(err)
		}
		var raw []byte
		switch replacement {
		case "missing":
			err = unix.Removexattr(fixture.root, RootGenerationAttribute)
		case "wrong":
			raw = make([]byte, RootGenerationBytes)
		case "short":
			raw = make([]byte, RootGenerationBytes-1)
		case "long":
			raw = make([]byte, RootGenerationBytes+1)
		case "oversized":
			raw = make([]byte, 128)
		}
		if raw != nil {
			err = unix.Setxattr(fixture.root, RootGenerationAttribute, raw, 0)
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := unix.Stat(fixture.root, &after); err != nil || before.Ino != after.Ino {
			t.Fatal("test changed inode", err)
		}
		if err := owner.CheckRead(); !errors.Is(err, ErrIdentity) {
			t.Errorf("%s admitted reused inode without original nonce: %v", replacement, err)
		}
		if err := unix.Setxattr(fixture.root, RootGenerationAttribute, original, 0); err != nil {
			t.Fatal(err)
		}
		if err := owner.CheckWrite(); !errors.Is(err, ErrIdentity) {
			t.Errorf("%s resurrected invalidated generation: %v", replacement, err)
		}
		if err := owner.Close(); err != nil {
			t.Fatal(err)
		}
		fixture.open(t, ReadWrite)
	}
}

// Copies retain backup metadata but cannot silently acquire the old inode binding.
func TestOwnerCopiedNonceCannotAuthorizeReplacementRoot(t *testing.T) {
	fixture := newVolumeFixture(t)
	owner := fixture.open(t, ReadWrite)
	nonce, err := readRootGeneration(owner.rootFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(fixture.root, fixture.root+"-retained"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(fixture.root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(fixture.root, RootGenerationAttribute, nonce, 0); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenWithHost(fixture.reference, fixture.root, ReadWrite, fixture.host)
	if reopened != nil {
		reopened.Close()
	}
	if !errors.Is(err, ErrIdentity) {
		t.Fatal("copied nonce inferred physical rebind", err)
	}
}

// Kernel failure is retryable; unsupported xattrs never create a weaker fallback.
func TestOwnerRootGenerationUnavailableCanRetry(t *testing.T) {
	for _, cause := range []error{syscall.EIO, syscall.EMFILE, syscall.EOPNOTSUPP, syscall.ENOSYS} {
		fixture := newVolumeFixture(t)
		owner := fixture.open(t, ReadWrite)
		owner.observeFile = func(step string, _ *os.File, _ string) error {
			if step == "root-generation" {
				return cause
			}
			return nil
		}
		err := owner.CheckRead()
		if !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrIdentity) || !errors.Is(err, cause) {
			t.Errorf("%v observation classification: %v", cause, err)
		}
		if (cause == syscall.ENOSYS || cause == syscall.EOPNOTSUPP) && !errors.Is(err, ErrUnsupported) {
			t.Error("unsupported filesystem silently weakened policy", err)
		}
		owner.observeFile = nil
		if err := owner.CheckWrite(); err != nil {
			t.Fatal("same approved generation did not recover", err)
		}
	}
}

// Old declarations cannot be interpreted as implicit physical-root enrollment.
func TestOwnerRootGenerationRequiresVersionedAuthority(t *testing.T) {
	for _, scope := range []ownerScope{daemonScope, ownerLocalScope} {
		for _, missing := range []string{"schema", "inode", "nonce"} {
			fixture := newVolumeFixture(t)
			if scope == ownerLocalScope {
				fixture.config.Schema = OwnerLocalSchema
			}
			switch missing {
			case "schema":
				fixture.config.Schema = "urnetwork-durable-volumes-v1"
				if scope == ownerLocalScope {
					fixture.config.Schema = "urnetwork-owner-local-volumes-v1"
				}
			case "inode":
				fixture.config.Volumes[0].StateRoots[0].RootInode = 0
			case "nonce":
				fixture.config.Volumes[0].StateRoots[0].GenerationSha256 = ""
			}
			fixture.writeConfig(t)
			if _, err := loadForScope(fixture.reference, scope); err == nil {
				t.Fatalf("scope %v accepted absent %s authority", scope, missing)
			}
		}
	}
}

// The explicit signing-device scope has the same cross-restart custody fence.
func TestOwnerLocalReopenRejectsEmptyReplacementRoot(t *testing.T) {
	fixture := newVolumeFixture(t)
	fixture.config.Schema = OwnerLocalSchema
	fixture.writeConfig(t)
	owner, err := OpenOwnerLocalWithHost(fixture.reference, fixture.root, ReadWrite, fixture.host)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(fixture.root, fixture.root+"-retained"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(fixture.root, 0700); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenOwnerLocalWithHost(fixture.reference, fixture.root, ReadWrite, fixture.host)
	if reopened != nil {
		reopened.Close()
	}
	if !errors.Is(err, ErrIdentity) {
		t.Fatal("owner-local enrolled empty replacement", err)
	}
}
