//go:build linux

// Host facts are synthetic; plan parsing, physical roots, retained steps,
// xattrs, no-replace publication, child exits and production guard reopening
// are real. These package adapters are not exposed by any production CLI.
package durablevolume

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

type preparationFixture struct {
	volume    *volumeFixture
	request   PreparationRequest
	reference Reference
	plan      PreparationPlan
	accepted  Reference
}

func preparationTestAdapter() PreparationAdapter {
	return PreparationAdapter{Build: func(ctx context.Context, parent *os.File, name string, owner PreparationOwner) (PreparationOwnerPlan, error) {
		if err := syscall.Mkdirat(int(parent.Fd()), name, 0700); err != nil {
			return PreparationOwnerPlan{}, err
		}
		fd, err := syscall.Openat(int(parent.Fd()), name, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if err != nil {
			return PreparationOwnerPlan{}, err
		}
		directory := os.NewFile(uintptr(fd), filepath.Join(parent.Name(), name))
		defer directory.Close()
		raw := []byte("exact reviewed public bytes\n")
		fileFd, err := syscall.Openat(fd, "record.bin", syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
		if err != nil {
			return PreparationOwnerPlan{}, err
		}
		file := os.NewFile(uintptr(fileFd), "record.bin")
		_, writeErr := file.Write(raw)
		if err := errors.Join(writeErr, file.Sync(), file.Close(), directory.Sync(), parent.Sync(), ctx.Err()); err != nil {
			return PreparationOwnerPlan{}, err
		}
		return PreparationOwnerPlan{Owner: owner, StagingName: name, Files: []PreparationFile{{Path: "record.bin", Kind: "file", Mode: 0600, Bytes: uint64(len(raw)), Sha256: testDigest(raw)}}, Attributes: []PreparationAttributeSpec{{Path: ".", Name: "user.urnetwork.attempt-ledger-custody"}}, Census: json.RawMessage(`{"purpose":"synthetic-no-signing"}`)}, nil
	}, Inspect: func(ctx context.Context, root *os.File, owner PreparationOwnerPlan) ([]PreparedAttribute, error) {
		fd, err := syscall.Openat(int(root.Fd()), "record.bin", syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return nil, err
		}
		file := os.NewFile(uintptr(fd), "record.bin")
		raw, readErr := io.ReadAll(io.LimitReader(file, 128))
		if err := errors.Join(readErr, file.Close(), ctx.Err()); err != nil {
			return nil, err
		}
		if string(raw) != "exact reviewed public bytes\n" {
			return nil, errors.New("test adapter refuses different public bytes")
		}
		identity, err := preparationIdentity(root)
		if err != nil {
			return nil, err
		}
		anchor, err := json.Marshal(struct {
			Schema string
			Inode  uint64
			Sha256 string
		}{Schema: "synthetic-preparation-test", Inode: identity.Inode, Sha256: testDigest(raw)})
		if err != nil {
			return nil, err
		}
		return []PreparedAttribute{{Spec: owner.Attributes[0], Raw: anchor}}, nil
	}}
}

func newPreparationFixture(t *testing.T) *preparationFixture {
	t.Helper()
	volume := newVolumeFixture(t)
	root, metadata, staging := filepath.Join(volume.mount, "prepare-root"), filepath.Join(volume.mount, "prepare-metadata"), filepath.Join(volume.mount, "prepare-staging")
	for _, path := range []string{root, metadata, staging} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	request := PreparationRequest{Schema: PreparationRequestSchema, Purpose: "fresh", Scope: "daemon", MountPath: volume.mount, FilesystemUuid: "1234-abcd", FilesystemType: "ext4", MinAvailableBytes: 1024, MinAvailableInodes: 8, RootPath: root, MarkerPath: filepath.Join(metadata, "identity"), LeasePath: filepath.Join(metadata, "lease"), DeclarationPath: filepath.Join(metadata, "declaration.json"), ControlPath: filepath.Join(metadata, "control.jsonl"), StagingDirectory: staging, Limits: PreparationLimits{MaxEntries: 16, MaxBytes: 1024 * 1024, MaxDepth: 4, MaxOwnerAttributes: 4, MaxOwnerAttributeBytes: 16384, MaxPlanBytes: 128 * 1024}, Owners: []PreparationOwner{{Kind: "synthetic-test-only", RelativePath: ".", Purpose: "fresh", Inputs: json.RawMessage(`{"public":true}`)}}}
	var stat syscall.Stat_t
	if err := syscall.Stat(root, &stat); err != nil {
		t.Fatal(err)
	}
	fence, err := json.Marshal(PreparationFence{Schema: PreparationFenceSchema, RootPath: root, RootInode: stat.Ino, Purpose: "fresh", FormerWritersStopped: true, NoPreviousOwnerState: true, Evidence: "synthetic fixture, never a live service"})
	if err != nil {
		t.Fatal(err)
	}
	request.FormerWriterFence = Reference{Path: filepath.Join(metadata, "fence.json"), Sha256: testDigest(fence)}
	if err := os.WriteFile(request.FormerWriterFence.Path, fence, 0600); err != nil {
		t.Fatal(err)
	}
	self := &preparationFixture{volume: volume, request: request}
	self.writeRequest(t)
	return self
}

func (self *preparationFixture) writeRequest(t *testing.T) {
	t.Helper()
	raw, err := json.MarshalIndent(self.request, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	self.reference = Reference{Path: filepath.Join(filepath.Dir(self.request.ControlPath), "request.json"), Sha256: testDigest(raw)}
	if err := os.WriteFile(self.reference.Path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func (self *preparationFixture) build(t *testing.T) {
	t.Helper()
	plan, err := PlanPreparationWithHost(t.Context(), self.reference, preparationTestAdapter(), self.volume.host)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	self.plan = plan
	self.accepted = Reference{Path: filepath.Join(filepath.Dir(self.request.ControlPath), "plan.json"), Sha256: testDigest(raw)}
	if err := os.WriteFile(self.accepted.Path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(self.request.RootPath)
	if err != nil || len(entries) != 0 {
		t.Fatal("planning mutated its target", err)
	}
}

func (self *preparationFixture) apply(ctx context.Context, hooks *preparationHooks) (PreparationResult, error) {
	return applyPreparation(ctx, self.accepted, preparationTestAdapter(), self.volume.host, daemonScope, hooks)
}

func TestPreparationReviewedPlanOpensActualGuard(t *testing.T) {
	f := newPreparationFixture(t)
	f.build(t)
	result, err := f.apply(t.Context(), nil)
	if err != nil || result.RestartAuthorized {
		t.Fatal(result, err)
	}
	owner, err := OpenWithHost(result.Declaration, f.request.RootPath, ReadWrite, f.volume.host)
	if err != nil {
		t.Fatal("prepared declaration did not admit actual guard", err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(f.request.ControlPath)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := f.apply(t.Context(), nil)
	if err != nil || repeated != result {
		t.Fatal("exact joined plan did not retain completion", err)
	}
	after, err := os.ReadFile(f.request.ControlPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("repeated apply appended or reset completed intent", err)
	}
}

func TestPreparationRefusesMissingChangedAndUnknownAuthority(t *testing.T) {
	for _, mode := range []string{"wrong-plan", "missing-root", "replaced-root", "unjoined", "old-custody", "changed-stage", "canceled", "retained", "restore"} {
		func() {
			f := newPreparationFixture(t)
			if mode == "retained" || mode == "restore" {
				f.request.Purpose = mode
				f.writeRequest(t)
				if _, err := PlanPreparationWithHost(t.Context(), f.reference, preparationTestAdapter(), f.volume.host); err == nil {
					t.Fatal("unsupported purpose became fresh")
				}
				return
			}
			f.build(t)
			ctx := t.Context()
			switch mode {
			case "wrong-plan":
				f.accepted.Sha256 = "sha256:" + strings.Repeat("00", 32)
			case "missing-root":
				if err := os.Remove(f.request.RootPath); err != nil {
					t.Fatal(err)
				}
			case "replaced-root":
				if err := os.Rename(f.request.RootPath, f.request.RootPath+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(f.request.RootPath, 0700); err != nil {
					t.Fatal(err)
				}
			case "unjoined":
				if err := os.WriteFile(f.request.FormerWriterFence.Path, []byte(`{}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "old-custody":
				if err := os.WriteFile(filepath.Join(f.request.RootPath, "old-signed-intent"), []byte("retained"), 0600); err != nil {
					t.Fatal(err)
				}
			case "changed-stage":
				if err := os.WriteFile(f.plan.Sources[0].Path, []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			case "canceled":
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			}
			result, err := f.apply(ctx, nil)
			if err == nil || result != (PreparationResult{}) {
				t.Fatal("unapproved/changed input was applied", mode, result, err)
			}
			switch mode {
			case "wrong-plan":
				if !strings.Contains(err.Error(), "plan bytes differ") {
					t.Fatal("wrong control failure", err)
				}
			case "unjoined":
				if !strings.Contains(err.Error(), "former-writer fence bytes differ") {
					t.Fatal("wrong control failure", err)
				}
			case "canceled":
				if !errors.Is(err, context.Canceled) {
					t.Fatal("wrong cancellation failure", err)
				}
			default:
				if !errors.Is(err, ErrIdentity) {
					t.Fatal("physical custody control failed before its intended boundary", mode, err)
				}
			}
			if _, err := os.Lstat(f.request.ControlPath); !os.IsNotExist(err) {
				t.Fatal("refusal created preparation control", err)
			}
		}()
	}
}

func TestPreparationPressureAndObservationRetryOriginalPlan(t *testing.T) {
	for _, mode := range []string{"bytes", "inodes", "read-only", "observation"} {
		func() {
			f := newPreparationFixture(t)
			f.build(t)
			old := f.volume.host.filesystem
			f.volume.host.change(func() {
				switch mode {
				case "bytes":
					f.volume.host.filesystem.AvailableBytes = 0
				case "inodes":
					f.volume.host.filesystem.AvailableInodes = 0
				case "read-only":
					f.volume.host.filesystem.ReadOnly = true
				case "observation":
					f.volume.host.filesystemErr = syscall.EIO
				}
			})
			if _, err := f.apply(t.Context(), nil); !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrIdentity) {
				t.Fatal("pre-admission pressure became integrity loss", mode, err)
			}
			if _, err := os.Lstat(f.request.ControlPath); !os.IsNotExist(err) {
				t.Fatal("pre-admission failure mutated custody", err)
			}
			f.volume.host.change(func() { f.volume.host.filesystem = old; f.volume.host.filesystemErr = nil })
			if _, err := f.apply(t.Context(), nil); err != nil {
				t.Fatal("same original accepted plan could not continue", err)
			}
		}()
	}
}

func TestPreparationCompletedCustodyCannotBeRecreated(t *testing.T) {
	for _, mode := range []string{"all-members", "byte-identical-member", "control", "generation", "checkpoint", "declaration", "root"} {
		func() {
			f := newPreparationFixture(t)
			f.build(t)
			if _, err := f.apply(t.Context(), nil); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(f.request.RootPath, "record.bin")
			switch mode {
			case "all-members":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "byte-identical-member", "control":
				if mode == "control" {
					path = f.request.ControlPath
				}
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(path, path+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "generation":
				if err := syscall.Removexattr(f.request.RootPath, RootGenerationAttribute); err != nil {
					t.Fatal(err)
				}
			case "checkpoint":
				if err := syscall.Removexattr(f.request.RootPath, "user.urnetwork.attempt-ledger-custody"); err != nil {
					t.Fatal(err)
				}
			case "declaration":
				if err := os.Remove(f.request.DeclarationPath); err != nil {
					t.Fatal(err)
				}
			case "root":
				if err := os.Rename(f.request.RootPath, f.request.RootPath+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(f.request.RootPath, 0700); err != nil {
					t.Fatal(err)
				}
			}
			result, err := f.apply(t.Context(), nil)
			if !errors.Is(err, ErrIdentity) || result != (PreparationResult{}) {
				t.Fatal("lost completed custody was recreated or admitted", mode, result, err)
			}
			if mode == "all-members" || mode == "declaration" {
				if mode == "declaration" {
					path = f.request.DeclarationPath
				}
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatal("lost completed member was reconstructed", err)
				}
			}
		}()
	}
}

func TestPreparationPendingLostAcknowledgementResumesExactBytes(t *testing.T) {
	for _, stage := range []string{"control-header", "root-reservation", "control-pending", "member-sync", "parent-sync", "attribute-sync", "control-complete"} {
		func() {
			f := newPreparationFixture(t)
			f.build(t)
			called := false
			_, err := f.apply(t.Context(), &preparationHooks{after: func(operation, path string) error {
				if operation == stage && !called {
					called = true
					return syscall.EIO
				}
				return nil
			}})
			if !called || !errors.Is(err, ErrPreparationUncertain) {
				t.Fatal("publication refusal lost uncertainty", stage, called, err)
			}
			if _, err := f.apply(t.Context(), nil); err != nil {
				t.Fatal("exact joined lost-ack plan did not reconcile", stage, err)
			}
			raw, err := os.ReadFile(filepath.Join(f.request.RootPath, "record.bin"))
			if err != nil || string(raw) != "exact reviewed public bytes\n" {
				t.Fatal("reconciliation changed reviewed bytes", err)
			}
		}()
	}
}

func TestPreparationUnknownPartialControlAndPayloadRemainStopped(t *testing.T) {
	for _, mode := range []string{"control", "payload"} {
		func() {
			f := newPreparationFixture(t)
			f.build(t)
			_, err := f.apply(t.Context(), &preparationHooks{after: func(operation, path string) error {
				if operation == "control-pending" && filepath.Base(path) == "record.bin" {
					return syscall.EIO
				}
				return nil
			}})
			if !errors.Is(err, ErrPreparationUncertain) {
				t.Fatal(err)
			}
			path := filepath.Join(f.request.RootPath, "record.bin")
			raw := []byte("unknown partial")
			if mode == "control" {
				path = f.request.ControlPath
				before, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				raw = append(before, []byte(`{"torn":`)...)
			}
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := f.apply(t.Context(), nil); err == nil {
				t.Fatal("unknown partial custody was repaired")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(raw, after) {
				t.Fatal("unknown original bytes were overwritten", err)
			}
		}()
	}
}

func TestPreparationPostPublicationCancellationRequiresReadback(t *testing.T) {
	f := newPreparationFixture(t)
	f.build(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	called := false
	_, err := f.apply(ctx, &preparationHooks{after: func(operation, path string) error {
		if operation == "member-sync" && filepath.Base(path) == "record.bin" {
			called = true
			cancel()
		}
		return nil
	}})
	if !called || !errors.Is(err, context.Canceled) || !errors.Is(err, ErrPreparationUncertain) {
		t.Fatal("post-publication cancellation became pre-admission refusal", called, err)
	}
	if _, err := f.apply(t.Context(), nil); err != nil {
		t.Fatal("joined cancellation could not read back exact original pending bytes", err)
	}
}

func TestPreparationChildCrashJoinsBeforeExactResume(t *testing.T) {
	if path := os.Getenv("URNETWORK_PREPARATION_CRASH_PLAN"); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var plan PreparationPlan
		if err := json.Unmarshal(raw, &plan); err != nil {
			t.Fatal(err)
		}
		host := &fixtureHost{mounts: []Mount{{Id: 1, ParentId: 1, Device: Device{Major: plan.Mount.Device.Major ^ 1, Minor: plan.Mount.Device.Minor}, Root: "/", Path: "/", FilesystemType: "ext4"}, plan.Mount}, uuidDevice: plan.Mount.Device, filesystem: plan.Filesystem}
		_, err = applyPreparation(t.Context(), Reference{Path: path, Sha256: testDigest(raw)}, preparationTestAdapter(), host, daemonScope, &preparationHooks{after: func(stage, path string) error {
			if stage == os.Getenv("URNETWORK_PREPARATION_CRASH_STAGE") {
				os.Exit(73)
			}
			return nil
		}})
		t.Fatal("crash barrier was not reached", err)
	}
	for _, stage := range []string{"root-reservation", "control-pending", "member-sync", "attribute-sync", "control-complete"} {
		func() {
			f := newPreparationFixture(t)
			f.build(t)
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(binary, "-test.run=^TestPreparationChildCrashJoinsBeforeExactResume$")
			cmd.Env = append(os.Environ(), "URNETWORK_PREPARATION_CRASH_PLAN="+f.accepted.Path, "URNETWORK_PREPARATION_CRASH_STAGE="+stage)
			raw, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 73 {
				t.Fatalf("child did not exit at real publication: %v %s", err, raw)
			}
			if _, err := f.apply(t.Context(), nil); err != nil {
				t.Fatal("joined child custody could not resume", stage, err)
			}
		}()
	}
}

func TestPreparationSeparateScopeAndRootLease(t *testing.T) {
	f := newPreparationFixture(t)
	f.build(t)
	if _, err := ApplyOwnerLocalPreparationWithHost(t.Context(), f.accepted, preparationTestAdapter(), f.volume.host); err == nil {
		t.Fatal("daemon plan gained owner-local authority")
	}
	file, err := os.Open(f.request.RootPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if _, err := f.apply(t.Context(), nil); !errors.Is(err, ErrBusy) {
		t.Fatal("active root owner was bypassed", err)
	}
	other := newPreparationFixture(t)
	other.build(t)
	if _, err := other.apply(t.Context(), nil); err != nil {
		t.Fatal("unrelated root was blocked", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.apply(t.Context(), nil); err != nil {
		t.Fatal("joined original root could not continue", err)
	}
}

func TestPreparationOwnerLocalSystemFilesystemRemainsExplicit(t *testing.T) {
	f := newPreparationFixture(t)
	f.request.Scope = "owner-local"
	f.request.MountPath = "/"
	f.writeRequest(t)
	f.volume.host.change(func() {
		device := f.volume.host.uuidDevice
		f.volume.host.mounts = []Mount{{Id: 1, ParentId: 1, Device: device, Root: "/", Path: "/", FilesystemType: "ext4"}}
	})
	if _, err := PlanPreparationWithHost(t.Context(), f.reference, preparationTestAdapter(), f.volume.host); err == nil {
		t.Fatal("daemon planner gained owner-local system-filesystem scope")
	}
	plan, err := PlanOwnerLocalPreparationWithHost(t.Context(), f.reference, preparationTestAdapter(), f.volume.host)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	ref := Reference{Path: filepath.Join(filepath.Dir(f.request.ControlPath), "owner-plan.json"), Sha256: testDigest(raw)}
	if err := os.WriteFile(ref.Path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	result, err := ApplyOwnerLocalPreparationWithHost(t.Context(), ref, preparationTestAdapter(), f.volume.host)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenWithHost(result.Declaration, f.request.RootPath, ReadWrite, f.volume.host); err == nil {
		t.Fatal("daemon admitted separately prepared owner-local declaration")
	}
	owner, err := OpenOwnerLocalWithHost(result.Declaration, f.request.RootPath, ReadWrite, f.volume.host)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPreparationRemountAndUnknownOwnerMetadataRefuse(t *testing.T) {
	for _, mode := range []string{"remount", "nested", "unknown-attribute"} {
		func() {
			f := newPreparationFixture(t)
			f.build(t)
			if mode == "unknown-attribute" {
				if _, err := f.apply(t.Context(), nil); err != nil {
					t.Fatal(err)
				}
				if err := syscall.Setxattr(f.request.RootPath, "user.urnetwork.foreign-unreviewed", []byte("unreviewed"), 1); err != nil {
					t.Fatal(err)
				}
			} else {
				f.volume.host.change(func() {
					if mode == "remount" {
						f.volume.host.mounts[1].Id++
					} else {
						f.volume.host.mounts = append(f.volume.host.mounts, Mount{Id: 17, ParentId: 7, Device: f.volume.host.uuidDevice, Root: "/", Path: filepath.Dir(f.request.ControlPath), FilesystemType: "ext4"})
					}
				})
			}
			if _, err := f.apply(t.Context(), nil); !errors.Is(err, ErrIdentity) {
				t.Fatal("changed mount or unknown owner was admitted", mode, err)
			}
		}()
	}
}

func TestPreparationAdmitsAllCapacityDimensionsBeforeTargetEffects(t *testing.T) {
	for _, mode := range []string{"bytes", "attributes", "plan"} {
		func() {
			f := newPreparationFixture(t)
			switch mode {
			case "bytes":
				f.request.Limits.MaxBytes = 1
			case "attributes":
				f.request.Limits.MaxOwnerAttributeBytes = 1
			case "plan":
				f.request.Limits.MaxPlanBytes = 4096
			}
			f.writeRequest(t)
			if _, err := PlanPreparationWithHost(t.Context(), f.reference, preparationTestAdapter(), f.volume.host); err == nil {
				t.Fatal("insufficient explicit capacity was admitted", mode)
			}
			if entries, err := os.ReadDir(f.request.RootPath); err != nil || len(entries) != 0 {
				t.Fatal("capacity refusal changed target", err)
			}
			if _, err := os.Lstat(f.request.ControlPath); !os.IsNotExist(err) {
				t.Fatal("capacity refusal created a control", err)
			}
		}()
	}
}
