//go:build linux

package connect

// Actual copied originals are rebound to new physical inodes and then reopened
// by the real producer owner. The signed submission bytes remain byte-exact.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func originalWorkOutboxTestFiles(t *testing.T, path string) []OriginalWorkOutboxFile {
	t.Helper()
	root, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	files := []OriginalWorkOutboxFile{{Name: "", Device: originalWorkOutboxDevice(root), Inode: originalWorkOutboxInode(root), Mode: 0700}}
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		info, err := os.Lstat(filepath.Join(path, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(path, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, OriginalWorkOutboxFile{Name: entry.Name(), Device: originalWorkOutboxDevice(info), Inode: originalWorkOutboxInode(info), Mode: uint32(info.Mode().Perm()), Bytes: uint64(len(raw)), Sha256: originalWorkOutboxDigest(raw)})
	}
	return files
}

func TestWholeWorkOutboxPortableRestoreRebindsOnlyPhysicalCustody(t *testing.T) {
	for _, phase := range []string{"empty", "committed", "pending"} {
		func() {
			client := newWholeWorkTestClient(t, NewNoContractClientOob(), nil)
			settings, request, _ := wholeWorkCaptureFixture(t, client)
			outbox, err := openOriginalWorkOutbox(settings.OutboxDirectory)
			if err != nil {
				t.Fatal(err)
			}
			defer outbox.close()
			requestRaw, _ := request.Bytes()
			if phase == "pending" {
				outbox.step = func(stage, _ string) error {
					if stage == "original-synced" {
						return syscall.EIO
					}
					return nil
				}
			}
			if phase != "empty" {
				_, err := client.ContractManager().captureOriginalWork(t.Context(), settings, outbox, requestRaw)
				if phase == "committed" && err != nil || phase == "pending" && !errors.Is(err, ErrOriginalWorkOutboxUncertain) {
					t.Fatal(err)
				}
			}
			checkpoint := bytes.Clone(outbox.raw)
			index, err := os.ReadFile(filepath.Join(settings.OutboxDirectory, originalWorkOutboxIndexName))
			if err != nil {
				t.Fatal(err)
			}
			originals := originalWorkOutboxTestFiles(t, settings.OutboxDirectory)
			if err := outbox.close(); err != nil {
				t.Fatal(err)
			}
			target := t.TempDir()
			if err := os.Chmod(target, 0700); err != nil {
				t.Fatal(err)
			}
			for _, file := range originals {
				if file.Name == "" {
					continue
				}
				raw, err := os.ReadFile(filepath.Join(settings.OutboxDirectory, file.Name))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(target, file.Name), raw, os.FileMode(file.Mode)); err != nil {
					t.Fatal(err)
				}
			}
			targets := originalWorkOutboxTestFiles(t, target)
			scope := OriginalWorkOutboxScope{DomainHash: request.DomainHash, ClientId: request.ClientId, PublicKey: request.PublicKey, RequestPublicKey: settings.RequestPublicKey}
			read := func(ctx context.Context, name string) ([]byte, error) {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				return os.ReadFile(filepath.Join(target, name))
			}
			rebound, err := RebindOriginalWorkOutboxInventory(t.Context(), scope, checkpoint, index, originals, targets, read)
			if err != nil {
				t.Fatal(err)
			}
			var restored OriginalWorkOutboxCheckpoint
			if err := json.Unmarshal(rebound.Checkpoint, &restored); err != nil {
				t.Fatal(err)
			}
			if restored.DirectoryInode == outbox.checkpoint.DirectoryInode || (restored.Pending != nil) != (phase == "pending") {
				t.Fatal("physical restore retained old root or changed pending phase", restored)
			}
			reversed, err := OriginalWorkOutboxRestoreOriginalIndex(t.Context(), rebound.Index, originals)
			if err != nil || !bytes.Equal(index, reversed) {
				t.Fatal("physical restore changed original index ancestry", err)
			}
			if err := os.WriteFile(filepath.Join(target, originalWorkOutboxIndexName), rebound.Index, 0600); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Setxattr(target, OriginalWorkOutboxAttribute, rebound.Checkpoint, 1); err != nil {
				t.Fatal(err)
			}
			reopened, err := openOriginalWorkOutbox(target)
			if err != nil {
				t.Fatal("copied owner failed actual restart", err)
			}
			defer reopened.close()
			if phase == "empty" {
				if entries, err := reopened.entries(t.Context()); err != nil || len(entries) != 0 {
					t.Fatal("empty restored birth invented work", entries, err)
				}
			} else {
				other := newWholeWorkTestClient(t, NewNoContractClientOob(), nil, client.ClientId())
				settings.now = func() time.Time { return time.Unix(1400, 0) }
				actual, err := other.ContractManager().captureOriginalWork(t.Context(), settings, reopened, requestRaw)
				original, readErr := os.ReadFile(filepath.Join(settings.OutboxDirectory, originalWorkOutboxName(request)))
				if err != nil || readErr != nil || !bytes.Equal(actual, original) {
					t.Fatal("restored restart recaptured expired prior-generation boundary", err, readErr)
				}
			}
			current, err := os.ReadFile(filepath.Join(settings.OutboxDirectory, originalWorkOutboxIndexName))
			if err != nil || !bytes.Equal(current, index) {
				t.Fatal("restore mutated original source inventory", err)
			}
		}()
	}
}

func TestWholeWorkOutboxRestoreRefusesIncompleteScopeAndReadFailure(t *testing.T) {
	client := newWholeWorkTestClient(t, NewNoContractClientOob(), nil)
	settings, request, _ := wholeWorkCaptureFixture(t, client)
	outbox, err := openOriginalWorkOutbox(settings.OutboxDirectory)
	if err != nil {
		t.Fatal(err)
	}
	defer outbox.close()
	requestRaw, _ := request.Bytes()
	if _, err := client.ContractManager().captureOriginalWork(t.Context(), settings, outbox, requestRaw); err != nil {
		t.Fatal(err)
	}
	originals := originalWorkOutboxTestFiles(t, settings.OutboxDirectory)
	scope := OriginalWorkOutboxScope{DomainHash: request.DomainHash, ClientId: request.ClientId, PublicKey: request.PublicKey, RequestPublicKey: settings.RequestPublicKey}
	for _, change := range []string{"read-error", "different-client", "different-approver", "different-provider-key", "omitted-leaf", "unknown-leaf", "whitespace-checkpoint", "reordered-boundary"} {
		func() {
			profile := scope
			checkpoint, index := bytes.Clone(outbox.raw), bytes.Clone(outbox.index)
			files := append([]OriginalWorkOutboxFile(nil), originals...)
			read := func(context.Context, string) ([]byte, error) {
				return os.ReadFile(filepath.Join(settings.OutboxDirectory, originalWorkOutboxName(request)))
			}
			switch change {
			case "read-error":
				read = func(context.Context, string) ([]byte, error) { return nil, syscall.EIO }
			case "different-client":
				profile.ClientId[0]++
			case "different-provider-key":
				profile.PublicKey[0]++
			case "different-approver":
				profile.RequestPublicKey[0]++
			case "omitted-leaf":
				files = files[:len(files)-1]
			case "unknown-leaf":
				files = append(files, OriginalWorkOutboxFile{Name: "unknown", Inode: 99999999, Mode: 0400, Bytes: 1, Sha256: originalWorkOutboxDigest([]byte{1})})
			case "whitespace-checkpoint":
				checkpoint = append(checkpoint, '\n')
			case "reordered-boundary":
				index = append([]byte{' '}, index...)
			}
			result, err := RebindOriginalWorkOutboxInventory(t.Context(), profile, checkpoint, index, files, originals, read)
			if err == nil || result.Checkpoint != nil || result.Index != nil {
				t.Fatal("incomplete or unrelated originals became valid restore", result, err)
			}
			if change == "read-error" && (!errors.Is(err, syscall.EIO) || errors.Is(err, ErrOriginalWorkOutboxIdentity)) {
				t.Fatal("failed borrowed read became changed bytes", err)
			}
		}()
	}
}
