//go:build linux

package connect

// Real retained originals, signed requests and named inode changes exercise the
// producer's capture path. No fixture substitutes a successful custody verdict.

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

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

func TestWholeWorkOutboxPendingCreateRetriesSameCompleteBoundary(t *testing.T) {
	oob := &wholeWorkTestOob{entered: make(chan struct{}), ordinary: NewNoContractClientOob()}
	client := newWholeWorkTestClient(t, oob, nil)
	settings, request, _ := wholeWorkCaptureFixture(t, client)
	outbox, err := openOriginalWorkOutbox(settings.OutboxDirectory)
	if err != nil {
		t.Fatal(err)
	}
	defer outbox.close()
	requestRaw, _ := request.Bytes()
	manager := client.ContractManager()
	destination, id := NewId(), NewId()
	manager.CreateContract(ContractKey{Destination: DestinationId(destination)}, 0, 100)
	select {
	case <-oob.entered:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	if raw, err := manager.captureOriginalWork(t.Context(), settings, outbox, requestRaw); err == nil || raw != nil {
		t.Fatal("transient pending work retained an immutable incomplete boundary", err)
	}
	if entries, err := outbox.entries(t.Context()); err != nil || len(entries) != 0 || outbox.checkpoint.Records != 0 || outbox.checkpoint.Pending != nil {
		t.Fatal("pending work consumed its phase or poisoned custody", entries, err)
	}
	stored, err := proto.Marshal(&protocol.StoredContract{ContractId: id.Bytes(), SourceId: client.ClientId().Bytes(), DestinationId: destination.Bytes(), TransferByteCount: 100})
	if err != nil {
		t.Fatal(err)
	}
	frame, err := ToFrame(&protocol.CreateContractResult{Contract: &protocol.Contract{StoredContractBytes: stored}}, manager.settings.ProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	oob.stateLock.Lock()
	callback := oob.callback
	oob.stateLock.Unlock()
	callback([]*protocol.Frame{frame}, nil)
	MessagePoolReturn(frame.MessageBytes)
	raw, err := manager.captureOriginalWork(t.Context(), settings, outbox, requestRaw)
	if err != nil {
		t.Fatal("same original permission could not capture after actual callback completion", err)
	}
	var submission protocol.OriginalWorkCutSubmission
	if err := json.Unmarshal(raw, &submission); err != nil {
		t.Fatal(err)
	}
	cut, err := protocol.DecodeOriginalWorkCut(t.Context(), submission.Cut)
	if err != nil || !cut.Complete || len(cut.Contracts) != 1 || !bytes.Equal(cut.Contracts[0].StoredContract, stored) || !bytes.Equal(submission.Request, requestRaw) || outbox.checkpoint.Records != 1 {
		t.Fatal("completed capture changed permission or lost actual work", cut, err)
	}
	manager.CheckpointContract(id, 17, 0)
	retry, err := manager.captureOriginalWork(t.Context(), settings, outbox, requestRaw)
	if err != nil || !bytes.Equal(retry, raw) || outbox.checkpoint.Records != 1 {
		t.Fatal("completed boundary was recaptured after subsequent work", err)
	}
}

func TestWholeWorkOutboxReadOnlyAdmissionKeepsPendingOriginal(t *testing.T) {
	client := newWholeWorkTestClient(t, NewNoContractClientOob(), nil)
	settings, request, _ := wholeWorkCaptureFixture(t, client)
	outbox, err := openOriginalWorkOutbox(settings.OutboxDirectory)
	if err != nil {
		t.Fatal(err)
	}
	defer outbox.close()
	outbox.step = func(stage, _ string) error {
		if stage == "original-synced" {
			return syscall.EIO
		}
		return nil
	}
	requestRaw, _ := request.Bytes()
	if _, err := client.ContractManager().captureOriginalWork(t.Context(), settings, outbox, requestRaw); !errors.Is(err, ErrOriginalWorkOutboxUncertain) {
		t.Fatal(err)
	}
	checkpoint, index := bytes.Clone(outbox.raw), bytes.Clone(outbox.index)
	if err := outbox.close(); err != nil {
		t.Fatal(err)
	}
	scope := OriginalWorkOutboxScope{DomainHash: request.DomainHash, ClientId: request.ClientId, PublicKey: request.PublicKey, RequestPublicKey: settings.RequestPublicKey}
	if err := ValidateOriginalWorkOutbox(t.Context(), settings.OutboxDirectory, scope); err != nil {
		t.Fatal("prepared pending original cannot be read-only admitted", err)
	}
	root, err := os.Open(settings.OutboxDirectory)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := readOriginalWorkOutboxAttribute(root)
	closeErr := root.Close()
	if err != nil || closeErr != nil || !bytes.Equal(actual, checkpoint) {
		t.Fatal("prelaunch validation reconciled pending checkpoint", err, closeErr)
	}
	actual, err = os.ReadFile(filepath.Join(settings.OutboxDirectory, originalWorkOutboxIndexName))
	if err != nil || !bytes.Equal(actual, index) {
		t.Fatal("prelaunch validation appended inventory", err)
	}
	scope.PublicKey[0]++
	if err := ValidateOriginalWorkOutbox(t.Context(), settings.OutboxDirectory, scope); err == nil {
		t.Fatal("current profile silently admitted a historical rotated key")
	}
	reopened, err := openOriginalWorkOutbox(settings.OutboxDirectory)
	if err != nil {
		t.Fatal("actual runtime could not reconcile unchanged pending bytes", err)
	}
	if err := reopened.close(); err != nil {
		t.Fatal(err)
	}
}

func TestWholeWorkOutboxDeletedAcknowledgedOriginalCannotRecapture(t *testing.T) {
	for _, removed := range []string{"original", "inventory", "checkpoint", "directory"} {
		func() {
			client := newWholeWorkTestClient(t, NewNoContractClientOob(), nil)
			settings, request, _ := wholeWorkCaptureFixture(t, client)
			outbox, err := openOriginalWorkOutbox(settings.OutboxDirectory)
			if err != nil {
				t.Fatal(err)
			}
			defer outbox.close()
			requestRaw, _ := request.Bytes()
			first, err := client.ContractManager().captureOriginalWork(t.Context(), settings, outbox, requestRaw)
			if err != nil {
				t.Fatal(err)
			}
			name := originalWorkOutboxName(request)
			wholeWorkTestAdmit(t, client, NewId(), NewId())
			switch removed {
			case "original":
				err = os.Remove(filepath.Join(settings.OutboxDirectory, name))
			case "inventory":
				err = os.Remove(filepath.Join(settings.OutboxDirectory, originalWorkOutboxIndexName))
			case "checkpoint":
				err = syscall.Removexattr(settings.OutboxDirectory, OriginalWorkOutboxAttribute)
			case "directory":
				err = os.Rename(settings.OutboxDirectory, settings.OutboxDirectory+".retained")
				if err == nil {
					err = os.Mkdir(settings.OutboxDirectory, 0700)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if raw, err := client.ContractManager().captureOriginalWork(t.Context(), settings, outbox, requestRaw); !errors.Is(err, ErrOriginalWorkOutboxIdentity) || raw != nil {
				t.Fatal("deleted acknowledged custody recaptured changed current work", raw, err)
			}
			if removed == "original" {
				if _, err := os.Lstat(filepath.Join(settings.OutboxDirectory, name)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("capture recreated the deleted original", err)
				}
			} else {
				retained := settings.OutboxDirectory
				if removed == "directory" {
					retained += ".retained"
				}
				actual, err := os.ReadFile(filepath.Join(retained, name))
				if err != nil || !bytes.Equal(actual, first) {
					t.Fatal("refusal changed surviving original", err)
				}
			}
			if err := outbox.close(); err != nil {
				t.Fatal(err)
			}
			if reopened, err := openOriginalWorkOutbox(settings.OutboxDirectory); err == nil {
				reopened.close()
				t.Fatal("restart inferred new birth from lost acknowledged custody")
			}
		}()
	}
}

func TestWholeWorkOutboxPendingRestartRetainsOnlyOriginalBytes(t *testing.T) {
	for _, stage := range []string{"pending-published", "original-synced", "inventory-synced", "committed-acknowledgement"} {
		func() {
			client := newWholeWorkTestClient(t, NewNoContractClientOob(), nil)
			settings, request, _ := wholeWorkCaptureFixture(t, client)
			outbox, err := openOriginalWorkOutbox(settings.OutboxDirectory)
			if err != nil {
				t.Fatal(err)
			}
			defer outbox.close()
			requestRaw, _ := request.Bytes()
			calls, hit := 0, false
			outbox.step = func(operation, _ string) error {
				if operation == "checkpoint-written" {
					calls++
				}
				if operation == stage || stage == "committed-acknowledgement" && operation == "checkpoint-written" && calls == 2 {
					hit = true
					return syscall.EIO
				}
				return nil
			}
			if _, err := client.ContractManager().captureOriginalWork(t.Context(), settings, outbox, requestRaw); !hit || !errors.Is(err, syscall.EIO) || !errors.Is(err, ErrOriginalWorkOutboxUncertain) || errors.Is(err, ErrOriginalWorkOutboxIdentity) {
				t.Fatal("actual interrupted publication lost its uncertainty or cause", hit, err)
			}
			original, err := os.ReadFile(filepath.Join(settings.OutboxDirectory, originalWorkOutboxName(request)))
			if err != nil {
				t.Fatal(err)
			}
			wholeWorkTestAdmit(t, client, NewId(), NewId())
			outbox.step = nil
			if _, err := client.ContractManager().captureOriginalWork(t.Context(), settings, outbox, requestRaw); !errors.Is(err, ErrOriginalWorkOutboxUncertain) {
				t.Fatal("uncertain live owner recaptured", err)
			}
			if err := outbox.close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := openOriginalWorkOutbox(settings.OutboxDirectory)
			if stage == "pending-published" {
				if err == nil {
					reopened.close()
					t.Fatal("empty pending original became new capture permission")
				}
				if len(original) != 0 {
					t.Fatal("control did not stop before original body")
				}
				return
			}
			if err != nil {
				t.Fatal("complete original could not reconcile", err)
			}
			defer reopened.close()
			other := newWholeWorkTestClient(t, NewNoContractClientOob(), nil, client.ClientId())
			settings.now = func() time.Time { return time.Unix(1400, 0) }
			recovered, err := other.ContractManager().captureOriginalWork(t.Context(), settings, reopened, requestRaw)
			if err != nil || !bytes.Equal(recovered, original) || len(recovered) == 0 || reopened.checkpoint.Pending != nil || reopened.checkpoint.Records != 1 {
				t.Fatal("restart re-signed or reinterpreted original boundary", err)
			}
		}()
	}
}

func TestWholeWorkOutboxReadObservationCanRetryWithoutCustodyInference(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		client := newWholeWorkTestClient(t, NewNoContractClientOob(), nil)
		settings, request, _ := wholeWorkCaptureFixture(t, client)
		outbox, err := openOriginalWorkOutbox(settings.OutboxDirectory)
		if err != nil {
			t.Fatal(err)
		}
		defer outbox.close()
		requestRaw, _ := request.Bytes()
		original, err := client.ContractManager().captureOriginalWork(t.Context(), settings, outbox, requestRaw)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		hit := false
		outbox.step = func(stage, _ string) error {
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
		raw, err := outbox.read(ctx, originalWorkOutboxName(request))
		cause := error(syscall.EIO)
		if canceled {
			cause = context.Canceled
		}
		if !hit || raw != nil || !errors.Is(err, cause) || errors.Is(err, ErrOriginalWorkOutboxIdentity) || outbox.failure != nil {
			t.Fatal("failed readback proved false identity loss", hit, err)
		}
		outbox.step = nil
		actual, err := outbox.read(t.Context(), originalWorkOutboxName(request))
		if err != nil || !bytes.Equal(actual, original) {
			t.Fatal("same owner did not resume exact original", err)
		}
	}
}

// A failed actual read owns its cause even if its partial byte count differs.
// Removing the interruption lets the same owner admit restored exact bytes.
func TestWholeWorkOutboxFailedReadCannotInferOversizedOriginal(t *testing.T) {
	client := newWholeWorkTestClient(t, NewNoContractClientOob(), nil)
	settings, request, _ := wholeWorkCaptureFixture(t, client)
	outbox, err := openOriginalWorkOutbox(settings.OutboxDirectory)
	if err != nil {
		t.Fatal(err)
	}
	defer outbox.close()
	requestRaw, _ := request.Bytes()
	original, err := client.ContractManager().captureOriginalWork(t.Context(), settings, outbox, requestRaw)
	if err != nil {
		t.Fatal(err)
	}
	name := originalWorkOutboxName(request)
	path := filepath.Join(settings.OutboxDirectory, name)
	hit := false
	outbox.readFile = func(file *os.File, buffer []byte) (int, error) {
		if hit || len(original)+1 > len(buffer) {
			t.Fatal("controlled read did not enter its first complete original chunk")
		}
		hit = true
		if err := os.Chmod(path, 0600); err != nil {
			return 0, err
		}
		if err := os.WriteFile(path, append(bytes.Clone(original), '\n'), 0600); err != nil {
			return 0, err
		}
		if err := os.Chmod(path, 0400); err != nil {
			return 0, err
		}
		n, err := file.Read(buffer)
		if err != nil || n != len(original)+1 {
			t.Fatal("controlled actual read did not observe the additional byte", n, err)
		}
		return n, syscall.EIO
	}
	if raw, err := outbox.read(t.Context(), name); !hit || raw != nil || !errors.Is(err, syscall.EIO) || errors.Is(err, ErrOriginalWorkOutboxIdentity) || outbox.failure != nil {
		t.Fatal("failed partial observation inferred irreversible custody loss", hit, err)
	}
	outbox.readFile = nil
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}
	if actual, err := outbox.read(t.Context(), name); err != nil || !bytes.Equal(actual, original) {
		t.Fatal("same retained owner could not reobserve exact original bytes", err)
	}
}

func TestWholeWorkOutboxPositiveReplacementPoisonsRetainedOwner(t *testing.T) {
	for _, changed := range []string{"ancestor", "leaf", "hardlink"} {
		func() {
			client := newWholeWorkTestClient(t, NewNoContractClientOob(), nil)
			settings, request, _ := wholeWorkCaptureFixture(t, client)
			parent := filepath.Join(settings.OutboxDirectory, "parent")
			settings.OutboxDirectory = filepath.Join(parent, "outbox")
			prepareOriginalWorkOutboxTest(t, settings.OutboxDirectory)
			outbox, err := openOriginalWorkOutbox(settings.OutboxDirectory)
			if err != nil {
				t.Fatal(err)
			}
			defer outbox.close()
			requestRaw, _ := request.Bytes()
			original, err := client.ContractManager().captureOriginalWork(t.Context(), settings, outbox, requestRaw)
			if err != nil {
				t.Fatal(err)
			}
			name := originalWorkOutboxName(request)
			path := filepath.Join(settings.OutboxDirectory, name)
			hit := false
			switch changed {
			case "ancestor":
				if err := os.Rename(parent, parent+".retained"); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(settings.OutboxDirectory, 0700); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(path, filepath.Join(filepath.Dir(parent), "alias")); err != nil {
					t.Fatal(err)
				}
			case "leaf":
				outbox.step = func(stage, _ string) error {
					if stage == "original-readback" && !hit {
						hit = true
						if err := os.Rename(path, path+".retained"); err != nil {
							return err
						}
						return os.WriteFile(path, original, 0400)
					}
					return nil
				}
			}
			if raw, err := outbox.read(t.Context(), name); raw != nil || !errors.Is(err, ErrOriginalWorkOutboxIdentity) || changed == "leaf" && !hit {
				t.Fatal("actual replacement survived original read", changed, err)
			}
			outbox.step = nil
			switch changed {
			case "ancestor":
				if err := os.Remove(settings.OutboxDirectory); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(parent); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(parent+".retained", parent); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Remove(filepath.Join(filepath.Dir(parent), "alias")); err != nil {
					t.Fatal(err)
				}
			case "leaf":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(path+".retained", path); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := outbox.entries(t.Context()); !errors.Is(err, ErrOriginalWorkOutboxIdentity) {
				t.Fatal("restoring names revived poisoned owner", err)
			}
		}()
	}
}

func TestWholeWorkOutboxBirthDoesNotRewriteLegacyOrMissingInventory(t *testing.T) {
	for _, existing := range []string{"prepared", "pristine", "empty-lock", "old-original"} {
		func() {
			path := t.TempDir()
			if err := os.Chmod(path, 0700); err != nil {
				t.Fatal(err)
			}
			if existing == "prepared" {
				prepareOriginalWorkOutboxTest(t, path)
			}
			if existing == "empty-lock" {
				if err := os.WriteFile(filepath.Join(path, originalWorkOutboxIndexName), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if existing == "old-original" {
				if err := os.WriteFile(filepath.Join(path, "old.json"), []byte("original bytes"), 0400); err != nil {
					t.Fatal(err)
				}
			}
			outbox, err := openOriginalWorkOutbox(path)
			if existing != "prepared" {
				if err == nil {
					outbox.close()
					t.Fatal("old uncheckpointed bytes became authentic birth")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if outbox.checkpoint.Records != 0 || len(outbox.index) != 0 {
				t.Fatal("empty birth invented originals")
			}
			if err := outbox.close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := openOriginalWorkOutbox(path)
			if err != nil {
				t.Fatal("authentic empty birth did not reopen", err)
			}
			if err := reopened.close(); err != nil {
				t.Fatal(err)
			}
		}()
	}
}
