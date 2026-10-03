//go:build linux

// Cohort controls exercise real roots, locks, journals, synced pending bytes,
// physical replacement and reopened production admission with synthetic facts.
package durablevolume

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
)

type preparationCohortFixture struct {
	members   []*preparationFixture
	cohort    PreparationCohort
	reference Reference
}

// Both roots share a physical filesystem but retain distinct plan/fence paths.
func newPreparationCohortFixture(t *testing.T, private bool) *preparationCohortFixture {
	t.Helper()
	first := newPreparationFixture(t)
	second := &preparationFixture{volume: first.volume, request: first.request}
	second.request.RootPath += "-second"
	metadata := filepath.Join(first.volume.mount, "prepare-metadata-second")
	second.request.StagingDirectory += "-second"
	for _, path := range []string{second.request.RootPath, metadata, second.request.StagingDirectory} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	second.request.MarkerPath = filepath.Join(metadata, "identity")
	second.request.LeasePath = filepath.Join(metadata, "lease")
	second.request.DeclarationPath = filepath.Join(metadata, "declaration.json")
	second.request.ControlPath = filepath.Join(metadata, "control.jsonl")
	second.request.FormerWriterFence.Path = filepath.Join(metadata, "fence.json")
	f := &preparationCohortFixture{members: []*preparationFixture{first, second}}
	for _, member := range f.members {
		var identity syscall.Stat_t
		if err := syscall.Stat(member.request.RootPath, &identity); err != nil {
			t.Fatal(err)
		}
		fence := PreparationFence{Schema: PreparationFenceSchema, RootPath: member.request.RootPath, RootInode: identity.Ino, Purpose: "fresh", FormerWritersStopped: true, NoPreviousOwnerState: true, Evidence: "synthetic joined cohort root"}
		if private {
			if err := os.Remove(member.request.RootPath); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Stat(filepath.Dir(member.request.RootPath), &identity); err != nil {
				t.Fatal(err)
			}
			fence.RootInode, fence.ParentInode = 0, identity.Ino
			member.request.RootCreation = "create-private"
		}
		raw, err := json.Marshal(fence)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(member.request.FormerWriterFence.Path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		member.request.FormerWriterFence.Sha256 = testDigest(raw)
		member.writeRequest(t)
		if private {
			privateRootPreparationPlan(t, member)
		} else {
			member.build(t)
		}
	}
	f.cohort = PreparationCohort{Schema: PreparationCohortSchema, Scope: "daemon", Plans: []Reference{first.accepted, second.accepted},
		Limits: PreparationCohortLimits{MaxRoots: 2, MaxPlanBytes: 1024 * 1024, MaxControlBytes: 1024 * 1024, MaxEntries: 32, MaxBytes: 2 * 1024 * 1024, MaxOwnerAttributes: 8, MaxOwnerAttributeBytes: 8 * 4096}}
	f.reference = f.document(t, "cohort.json", f.cohort)
	return f
}

func (self *preparationCohortFixture) document(t *testing.T, name string, value any) Reference {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(self.members[0].request.ControlPath), name)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return Reference{Path: path, Sha256: testDigest(raw)}
}

func (self *preparationCohortFixture) apply(ctx context.Context, hooks *preparationHooks) (PreparationCohortResult, error) {
	return prepareCohort(ctx, self.reference, preparationTestAdapter(), self.members[0].volume.host, daemonScope, true, hooks)
}

// Faults in the second root cannot create a control or reservation in the first.
func TestPreparationCohortChecksAllRootsBeforeFirstMutation(t *testing.T) {
	for _, fault := range []string{"unreviewed-member", "changed-source", "busy-peer", "foreign-output", "canceled"} {
		f := newPreparationCohortFixture(t, false)
		result, err := CheckPreparationCohortWithHost(t.Context(), f.reference, preparationTestAdapter(), f.members[0].volume.host)
		if err != nil || result.Applied || result.RestartAuthorized || len(result.Roots) != 2 || len(result.Results) != 0 {
			t.Fatal("valid cohort read-only baseline refused", err)
		}
		second := f.members[1]
		ctx := t.Context()
		var held *os.File
		switch fault {
		case "unreviewed-member":
			err = os.WriteFile(filepath.Join(second.request.RootPath, "foreign.bin"), []byte("retained unknown"), 0600)
		case "changed-source":
			err = os.WriteFile(second.plan.Sources[0].Path, []byte("changed"), 0600)
		case "busy-peer":
			held, err = os.Open(second.request.RootPath)
			if err == nil {
				err = syscall.Flock(int(held.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			}
		case "foreign-output":
			err = os.WriteFile(second.request.DeclarationPath, []byte("unreviewed declaration"), 0600)
		case "canceled":
			var cancel context.CancelFunc
			ctx, cancel = context.WithCancel(ctx)
			cancel()
		}
		if err != nil {
			t.Fatal(err)
		}
		result, err = f.apply(ctx, nil)
		if held != nil {
			if closeErr := held.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
		}
		if err == nil || !reflect.DeepEqual(result, PreparationCohortResult{}) {
			t.Fatal("invalid later root admitted", fault, err)
		}
		if fault == "busy-peer" && !errors.Is(err, ErrBusy) || fault == "canceled" && !errors.Is(err, context.Canceled) {
			t.Fatal("fault refused at unrelated boundary", fault, err)
		}
		first := f.members[0]
		entries, readErr := os.ReadDir(first.request.RootPath)
		if readErr != nil || len(entries) != 0 {
			t.Fatal("refused cohort changed first target", fault, readErr)
		}
		if _, statErr := os.Stat(first.request.ControlPath); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatal("refused cohort created first journal", fault, statErr)
		}
	}
}

// A completed first root and fully retained pending second member survive a
// joined retry; runtime guards open only the resulting exact declarations.
func TestPreparationCohortResumesCompletedPeerAndPendingWrite(t *testing.T) {
	f := newPreparationCohortFixture(t, false)
	lost := errors.New("synthetic second member acknowledgement lost")
	hooks := &preparationHooks{after: func(stage, path string) error {
		if stage == "member-sync" && path == filepath.Join(f.members[1].request.RootPath, "record.bin") {
			return lost
		}
		return nil
	}}
	if _, err := f.apply(t.Context(), hooks); !errors.Is(err, ErrPreparationUncertain) || !errors.Is(err, lost) {
		t.Fatal("did not reach retained second member", err)
	}
	firstControl, err := os.ReadFile(f.members[0].request.ControlPath)
	if err != nil {
		t.Fatal(err)
	}
	secondPrefix, err := os.ReadFile(f.members[1].request.ControlPath)
	if err != nil {
		t.Fatal(err)
	}
	result, err := f.apply(t.Context(), nil)
	if err != nil || !result.Applied || result.RestartAuthorized || len(result.Results) != 2 {
		t.Fatal("cohort cannot resume exact pending work", err)
	}
	after, err := os.ReadFile(f.members[0].request.ControlPath)
	if err != nil || !bytes.Equal(after, firstControl) {
		t.Fatal("healthy completed root was reset", err)
	}
	after, err = os.ReadFile(f.members[1].request.ControlPath)
	if err != nil || !bytes.HasPrefix(after, secondPrefix) {
		t.Fatal("pending original journal was reconstructed", err)
	}
	for index, prepared := range result.Results {
		owner, err := OpenWithHost(prepared.Declaration, f.members[index].request.RootPath, ReadWrite, f.members[index].volume.host)
		if err != nil {
			t.Fatal("completed cohort cannot admit actual owner", index, err)
		}
		if err := owner.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if repeated, err := f.apply(t.Context(), nil); err != nil || !reflect.DeepEqual(result, repeated) {
		t.Fatal("completed cohort replay changed result", err)
	}
}

// Membership and approval dimensions remain authoritative after a first root
// completes. A solo call cannot bypass the retained cohort's remaining roots.
func TestPreparationCohortBindsOriginalMembershipAcrossRestart(t *testing.T) {
	f := newPreparationCohortFixture(t, false)
	stop := errors.New("synthetic first root completed")
	if _, err := f.apply(t.Context(), &preparationHooks{after: func(stage, path string) error {
		if stage == "cohort-root-complete" {
			return stop
		}
		return nil
	}}); !errors.Is(err, stop) {
		t.Fatal(err)
	}
	original, err := os.ReadFile(f.members[0].request.ControlPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.members[0].apply(t.Context(), nil); !errors.Is(err, ErrIdentity) {
		t.Fatal("solo application bypassed retained cohort", err)
	}
	changed := f.cohort
	changed.Limits.MaxBytes++
	ref := f.document(t, "changed-cohort.json", changed)
	if _, err := ApplyPreparationCohortWithHost(t.Context(), ref, preparationTestAdapter(), f.members[0].volume.host); !errors.Is(err, ErrIdentity) {
		t.Fatal("changed approval adopted partial original cohort", err)
	}
	after, err := os.ReadFile(f.members[0].request.ControlPath)
	if err != nil || !bytes.Equal(original, after) {
		t.Fatal("refused replacement mutated original journal", err)
	}
	if _, err := f.apply(t.Context(), nil); err != nil {
		t.Fatal("original cohort cannot resume", err)
	}
}

// Unknown partial bytes stay intact and prevent any earlier root from writing.
func TestPreparationCohortRefusesPartialPeerWithoutRewritingCompletion(t *testing.T) {
	f := newPreparationCohortFixture(t, false)
	path := filepath.Join(f.members[1].request.RootPath, "record.bin")
	if _, err := f.apply(t.Context(), &preparationHooks{after: func(stage, current string) error {
		if stage == "member-sync" && current == path {
			return errors.New("synthetic stopped writer")
		}
		return nil
	}}); !errors.Is(err, ErrPreparationUncertain) {
		t.Fatal(err)
	}
	original, err := os.ReadFile(f.members[0].request.ControlPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.apply(t.Context(), nil); !errors.Is(err, ErrIdentity) {
		t.Fatal("partial target reached publication", err)
	}
	after, err := os.ReadFile(f.members[0].request.ControlPath)
	partial, readErr := os.ReadFile(path)
	if err != nil || readErr != nil || !bytes.Equal(after, original) || string(partial) != "partial" {
		t.Fatal("partial refusal changed original bytes", err, readErr)
	}
}

// Sibling create-private plans share a parent description without self-locking;
// each original staged inode still moves without replacement to its own path.
func TestPreparationCohortCreatesSiblingRootsFromOriginalStagedInodes(t *testing.T) {
	f := newPreparationCohortFixture(t, true)
	result, err := f.apply(t.Context(), nil)
	if err != nil || !result.Applied {
		t.Fatal("cohort self-conflicted on shared private parent", err)
	}
	for _, member := range f.members {
		var stat syscall.Stat_t
		if err := syscall.Stat(member.request.RootPath, &stat); err != nil || stat.Ino != member.plan.Root.Inode || stat.Mode&0777 != 0700 {
			t.Fatal("staged root lost its reviewed inode or protection", err)
		}
	}
	if _, err := f.apply(t.Context(), nil); err != nil {
		t.Fatal("private-root cohort cannot reopen", err)
	}
}

// Individual reserves cannot each spend the same shared filesystem headroom.
func TestPreparationCohortJoinsAggregateCapacityBeforeEffects(t *testing.T) {
	for _, fault := range []string{"plan-bytes", "control-bytes", "entries", "owner-bytes", "shared-reserve"} {
		f := newPreparationCohortFixture(t, false)
		changed := f.cohort
		switch fault {
		case "plan-bytes":
			changed.Limits.MaxPlanBytes = 1
		case "control-bytes":
			changed.Limits.MaxControlBytes = 1
		case "entries":
			changed.Limits.MaxEntries = 1
		case "owner-bytes":
			changed.Limits.MaxOwnerAttributeBytes = 4096
		case "shared-reserve":
			f.members[0].volume.host.filesystem.AvailableBytes = 1500
		}
		ref := f.document(t, "capacity-cohort.json", changed)
		if _, err := ApplyPreparationCohortWithHost(t.Context(), ref, preparationTestAdapter(), f.members[0].volume.host); err == nil {
			t.Fatal("aggregate capacity was ignored", fault)
		}
		for _, member := range f.members {
			if _, err := os.Stat(member.request.ControlPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("capacity refusal wrote control", fault, err)
			}
		}
	}
}
