//go:build linux || darwin || freebsd

// Actual pre-send originals and real named inode transitions exercise custody.
// Fixtures explicitly prepare their first birth; runtime never enrolls a root.
package connect

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/urnetwork/connect/protocol"
	"golang.org/x/sys/unix"
)

// Only a newly created synthetic root may receive its pure fresh attribute.
func prepareOriginalContractStoreTest(t *testing.T, directory string, scope OriginalContractStoreScope) {
	t.Helper()
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	lease, err := os.OpenFile(filepath.Join(directory, OriginalContractStoreLeaseName), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(lease.Sync(), lease.Close()); err != nil {
		t.Fatal(err)
	}
	root, err := os.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	raw, err := BuildFreshOriginalContractStoreCheckpoint(t.Context(), root, scope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readOriginalContractStoreAttribute(root); !errors.Is(err, ErrOriginalContractStoreIdentity) {
		t.Fatal("synthetic first birth was already present", err)
	}
	// The test owns this newly created empty root on all supported hosts. The
	// actual Linux offline publisher separately uses no-replace xattr creation.
	if err := unix.Fsetxattr(int(root.Fd()), OriginalContractStoreAttribute, raw, 0); err != nil {
		t.Fatal(err)
	}
	if err := root.Sync(); err != nil {
		t.Fatal(err)
	}
}

// A copied inventory reports actual inode coordinates and exact original bytes.
func originalContractStoreTestFiles(t *testing.T, directory string) []OriginalContractStoreFile {
	t.Helper()
	info, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	files := []OriginalContractStoreFile{{Name: "", Device: originalWorkOutboxDevice(info), Inode: originalWorkOutboxInode(info), Mode: 0700}}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		path := filepath.Join(directory, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, OriginalContractStoreFile{Name: entry.Name(), Device: originalWorkOutboxDevice(info), Inode: originalWorkOutboxInode(info), Mode: uint32(info.Mode().Perm()), Bytes: uint64(len(raw)), Sha256: originalWorkOutboxDigest(raw)})
	}
	return files
}

// Ordinary macOS TMPDIR has a symlink ancestor. Make that precondition explicit
// on every supported host and keep a separate runtime alias rejection check.
func TestOriginalContractStoreFixtureWithSymlinkTempDir(t *testing.T) {
	useSymlinkCustodyTempDir(t)
	t.Run("physical custody and explicit alias", func(t *testing.T) {
		client, oob := newOriginalCreationTestClient(t)
		scope := client.ContractManager().contractCreation.scope
		if err := ValidateOriginalContractStore(t.Context(), oob.directory, scope); err != nil {
			t.Fatal("positive fixture retained the temporary-directory alias", err)
		}
		client.ContractManager().CreateContract(ContractKey{Destination: DestinationId(NewId())}, 0, 100)
		if oob.readErr != nil || len(oob.retained) == 0 {
			t.Fatal("physical fixture did not retain its pre-send original", oob.readErr)
		}
		alias := filepath.Join(physicalTempDir(t), "explicit-alias")
		if err := os.Symlink(oob.directory, alias); err != nil {
			t.Fatal(err)
		}
		if err := ValidateOriginalContractStore(t.Context(), alias, scope); !errors.Is(err, ErrOriginalContractStoreIdentity) {
			t.Fatal("explicit alias became physical contract custody", err)
		}
		if err := ValidateOriginalContractStore(t.Context(), oob.directory, scope); err != nil {
			t.Fatal("alias rejection changed physical custody", err)
		}
	})
}

func TestOriginalContractStoreReadbackRetainsNamedInodeAndLinks(t *testing.T) {
	for _, change := range []string{"replacement", "hardlink", "size"} {
		func() {
			client, oob := newOriginalCreationTestClient(t)
			client.ContractManager().CreateContract(ContractKey{Destination: DestinationId(NewId())}, 0, 100)
			if oob.readErr != nil {
				t.Fatal(oob.readErr)
			}
			store, err := openOriginalContractStore(t.Context(), oob.directory)
			if err != nil {
				t.Fatal(err)
			}
			defer store.close()
			name, _ := originalContractLeafName("request", oob.retained)
			path := filepath.Join(oob.directory, name)
			hit := false
			store.step = func(stage, _ string) error {
				if stage != "original-readback" {
					return nil
				}
				hit = true
				switch change {
				case "replacement":
					if err := os.Rename(path, path+".held"); err != nil {
						return err
					}
					return os.WriteFile(path, oob.retained, 0400)
				case "hardlink":
					return os.Link(path, path+".linked")
				default:
					if err := os.Chmod(path, 0600); err != nil {
						return err
					}
					if err := os.WriteFile(path, append(bytes.Clone(oob.retained), '\n'), 0600); err != nil {
						return err
					}
					return os.Chmod(path, 0400)
				}
			}
			if raw, err := store.read(t.Context(), name); !hit || raw != nil || !errors.Is(err, ErrOriginalContractStoreIdentity) {
				t.Fatal("changed named original was acknowledged", change, hit, err)
			}
		}()
	}
}

func TestOriginalContractStoreFailedReadbackKeepsCauseAndSameOwner(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		func() {
			client, oob := newOriginalCreationTestClient(t)
			client.ContractManager().CreateContract(ContractKey{Destination: DestinationId(NewId())}, 0, 100)
			if oob.readErr != nil {
				t.Fatal(oob.readErr)
			}
			store, err := openOriginalContractStore(t.Context(), oob.directory)
			if err != nil {
				t.Fatal(err)
			}
			defer store.close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			hit := false
			store.step = func(stage, _ string) error {
				if stage == "original-readback" {
					hit = true
					if canceled {
						cancel()
						return nil
					}
					return syscall.EIO
				}
				return nil
			}
			name, _ := originalContractLeafName("request", oob.retained)
			raw, err := store.read(ctx, name)
			cause := error(syscall.EIO)
			if canceled {
				cause = context.Canceled
			}
			if !hit || raw != nil || !errors.Is(err, cause) || errors.Is(err, ErrOriginalContractStoreIdentity) || store.failure != nil {
				t.Fatal("failed observation poisoned unchanged original", hit, err)
			}
			store.step = nil
			if raw, err := store.read(t.Context(), name); err != nil || !bytes.Equal(raw, oob.retained) {
				t.Fatal("same owner could not retry original", err)
			}
		}()
	}
}

func TestOriginalContractStorePublicationCannotAcknowledgeReplacedInode(t *testing.T) {
	client, oob := newOriginalCreationTestClient(t)
	client.ContractManager().CreateContract(ContractKey{Destination: DestinationId(NewId())}, 0, 100)
	if oob.readErr != nil {
		t.Fatal(oob.readErr)
	}
	request, err := protocol.DecodeOriginalContractRequest(t.Context(), oob.retained)
	if err != nil {
		t.Fatal(err)
	}
	request.RequestId = [16]byte(NewId())
	request, err = protocol.SignOriginalContractRequest(t.Context(), request, ed25519.NewKeyFromSeed(bytes.Repeat([]byte{121}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := request.Bytes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	name, _ := originalContractLeafName("request", raw)
	store, err := openOriginalContractStore(t.Context(), oob.directory)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	hit := false
	store.step = func(stage, member string) error {
		if stage != "original-synced" {
			return nil
		}
		hit = true
		path := filepath.Join(oob.directory, member)
		if err := os.Rename(path, path+".held"); err != nil {
			return err
		}
		return os.WriteFile(path, raw, 0400)
	}
	if err := store.retain(t.Context(), name, raw); !hit || !errors.Is(err, ErrOriginalContractStoreIdentity) {
		t.Fatal("publication acknowledged a replacement inode", hit, err)
	}
	if retained, err := os.ReadFile(filepath.Join(oob.directory, name+".held")); err != nil || !bytes.Equal(retained, raw) {
		t.Fatal("refusal changed original publication", err)
	}
}

func TestOriginalContractStorePreparedBirthAndIndependentHistoricalScope(t *testing.T) {
	client, oob := newOriginalCreationTestClient(t)
	scope := client.ContractManager().contractCreation.scope
	if err := ValidateOriginalContractStore(t.Context(), oob.directory, scope); err != nil {
		t.Fatal(err)
	}
	unprepared := physicalTempDir(t)
	if err := os.Chmod(unprepared, 0700); err != nil {
		t.Fatal(err)
	}
	if err := ValidateOriginalContractStore(t.Context(), unprepared, scope); !errors.Is(err, ErrOriginalContractStoreIdentity) {
		t.Fatal("unprepared root became original custody", err)
	}
	if entries, err := os.ReadDir(unprepared); err != nil || len(entries) != 0 {
		t.Fatal("runtime created absent birth", entries, err)
	}
	destination := NewId()
	client.ContractManager().CreateContract(ContractKey{Destination: DestinationId(destination)}, 0, 100)
	if oob.readErr != nil {
		t.Fatal(oob.readErr)
	}
	originalCreationTestReply(t, client, oob, destination)
	for _, kind := range []string{"provider-key", "source-generation", "client", "domain"} {
		changed := scope
		switch kind {
		case "provider-key":
			changed.PublicKey[0]++
		case "source-generation":
			changed.SourceGeneration[0]++
		case "client":
			changed.ClientId[0]++
		default:
			changed.DomainHash[0]++
		}
		if err := ValidateOriginalContractStore(t.Context(), oob.directory, changed); !errors.Is(err, ErrOriginalContractStoreIdentity) {
			t.Fatal("originals selected their own historical approval", kind, err)
		}
	}
	if err := ValidateOriginalContractStore(t.Context(), oob.directory, scope); err != nil {
		t.Fatal("unchanged original scope did not remain admissible", err)
	}
}

func TestOriginalContractStorePortableRestoreKeepsCompleteOriginalClosure(t *testing.T) {
	client, oob := newOriginalCreationTestClient(t)
	destination := NewId()
	client.ContractManager().CreateContract(ContractKey{Destination: DestinationId(destination)}, 0, 100)
	if oob.readErr != nil {
		t.Fatal(oob.readErr)
	}
	originalCreationTestReply(t, client, oob, destination)
	store, err := openOriginalContractStore(t.Context(), oob.directory)
	if err != nil {
		t.Fatal(err)
	}
	scope, checkpoint := store.checkpoint.Scope, bytes.Clone(store.raw)
	if err := store.close(); err != nil {
		t.Fatal(err)
	}
	originals := originalContractStoreTestFiles(t, oob.directory)
	target := physicalTempDir(t)
	if err := os.Chmod(target, 0700); err != nil {
		t.Fatal(err)
	}
	for _, file := range originals {
		if file.Name == "" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(oob.directory, file.Name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(target, file.Name), raw, os.FileMode(file.Mode)); err != nil {
			t.Fatal(err)
		}
	}
	targets := originalContractStoreTestFiles(t, target)
	read := func(ctx context.Context, name string) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return os.ReadFile(filepath.Join(target, name))
	}
	rebound, err := RebindOriginalContractStoreInventory(t.Context(), scope, checkpoint, originals, targets, read)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(rebound, checkpoint) {
		t.Fatal("copied physical custody was not rebound")
	}
	root, err := os.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(unix.Fsetxattr(int(root.Fd()), OriginalContractStoreAttribute, rebound, 0), root.Sync(), root.Close()); err != nil {
		t.Fatal(err)
	}
	if err := ValidateOriginalContractStore(t.Context(), target, scope); err != nil {
		t.Fatal("actual restored owner refused original closure", err)
	}
	for _, file := range originals {
		if file.Name == "" {
			continue
		}
		before, err := os.ReadFile(filepath.Join(oob.directory, file.Name))
		if err != nil {
			t.Fatal(err)
		}
		after, err := os.ReadFile(filepath.Join(target, file.Name))
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("restore changed original signed bytes", err)
		}
	}
	for _, change := range []string{"missing-request", "read-error", "different-birth"} {
		files := append([]OriginalContractStoreFile(nil), originals...)
		expected := scope
		borrow := read
		switch change {
		case "missing-request":
			for index, file := range files {
				if len(file.Name) > 8 && file.Name[:8] == "request-" {
					files = append(files[:index], files[index+1:]...)
					break
				}
			}
		case "read-error":
			borrow = func(context.Context, string) ([]byte, error) { return nil, syscall.EIO }
		case "different-birth":
			expected.SourceGeneration[0]++
		}
		if raw, err := RebindOriginalContractStoreInventory(t.Context(), expected, checkpoint, files, targets, borrow); err == nil || raw != nil || change == "read-error" && (!errors.Is(err, syscall.EIO) || errors.Is(err, ErrOriginalContractStoreIdentity)) {
			t.Fatal("invalid or interrupted original closure became restorable", change, err)
		}
	}
}
