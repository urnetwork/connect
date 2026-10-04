//go:build linux

// The explicit larger profile retains a complete archive-sized owner census.
// No owner, per-record bound or accepted-plan dimension is inferred from absence.
package durablevolume

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// Independent marker inodes avoid attributing aggregate xattr capacity to the
// root inode. This is a synthetic adapter, never a production application kind.
func preparationManyOwnersAdapter() PreparationAdapter {
	inspect := func(ctx context.Context, root *os.File, owner PreparationOwnerPlan) ([]PreparedAttribute, error) {
		member := owner.Files[0]
		fd, err := syscall.Openat(int(root.Fd()), member.Path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return nil, err
		}
		file := os.NewFile(uintptr(fd), member.Path)
		identity, statErr := preparationIdentity(file)
		if err := errors.Join(statErr, preparationVerifyFile(ctx, file, member.Bytes, member.Sha256), file.Close()); err != nil {
			return nil, err
		}
		raw, err := json.Marshal(struct {
			Inode  uint64 `json:"inode"`
			Sha256 string `json:"sha256"`
		}{Inode: identity.Inode, Sha256: member.Sha256})
		return []PreparedAttribute{{Spec: owner.Attributes[0], Raw: raw}}, err
	}
	return PreparationAdapter{
		Build: func(ctx context.Context, parent *os.File, name string, owner PreparationOwner) (PreparationOwnerPlan, error) {
			var input struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(owner.Inputs, &input); err != nil || input.Name == "" || filepath.Base(input.Name) != input.Name {
				return PreparationOwnerPlan{}, errors.New("synthetic owner name is invalid")
			}
			if err := syscall.Mkdirat(int(parent.Fd()), name, 0700); err != nil {
				return PreparationOwnerPlan{}, err
			}
			fd, err := syscall.Openat(int(parent.Fd()), name, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
			if err != nil {
				return PreparationOwnerPlan{}, err
			}
			directory := os.NewFile(uintptr(fd), name)
			defer directory.Close()
			memberFd, err := syscall.Openat(fd, input.Name, syscall.O_RDWR|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
			if err != nil {
				return PreparationOwnerPlan{}, err
			}
			member := os.NewFile(uintptr(memberFd), input.Name)
			raw := []byte("synthetic retained member: " + input.Name + "\n")
			n, writeErr := member.Write(raw)
			if err := errors.Join(writeErr, member.Sync(), member.Close(), directory.Sync(), parent.Sync(), ctx.Err()); err != nil || n != len(raw) {
				return PreparationOwnerPlan{}, errors.Join(errors.New("synthetic staging write was incomplete"), err)
			}
			return PreparationOwnerPlan{Owner: owner, StagingName: name, Census: append(json.RawMessage(nil), owner.Inputs...),
				Files:      []PreparationFile{{Path: input.Name, Kind: "file", Mode: 0600, Bytes: uint64(len(raw)), Sha256: testDigest(raw)}},
				Attributes: []PreparationAttributeSpec{{Path: input.Name, Name: "user.urnetwork.attempt-ledger-custody"}}}, nil
		},
		Inspect: inspect,
	}
}

// Literal wire construction keeps this control buildable on the old profile.
func preparationManyOwnersRequest(t *testing.T, f *preparationFixture, count int, profile string) {
	t.Helper()
	f.request.Owners = nil
	for index := 0; index < count; index++ {
		input, err := json.Marshal(map[string]string{"name": fmt.Sprintf("member-%04d.bin", index)})
		if err != nil {
			t.Fatal(err)
		}
		f.request.Owners = append(f.request.Owners, PreparationOwner{Kind: "synthetic-many-owners", RelativePath: ".", Purpose: "fresh", Inputs: input})
	}
	f.request.Limits = PreparationLimits{MaxEntries: 2 * uint64(count+1), MaxBytes: 1024 * 1024, MaxDepth: 4,
		MaxOwnerAttributes: 2 * uint64(count), MaxOwnerAttributeBytes: 2 * uint64(count) * 4096, MaxPlanBytes: 8 * 1024 * 1024}
	f.writeRequest(t)
	raw, err := os.ReadFile(f.reference.Path)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if profile != "" {
		wire["capacity_profile"], err = json.Marshal(profile)
		if err != nil {
			t.Fatal(err)
		}
	}
	raw, err = json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.reference.Path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	f.reference.Sha256 = testDigest(raw)
}

// 512 retained segments and two active heads have independent markers, an
// explicit two-times attribute margin, and one unchanged completed control.
func TestPreparationManyOwnersPublishesCompleteArchiveSizedCensus(t *testing.T) {
	f := newPreparationFixture(t)
	preparationManyOwnersRequest(t, f, 514, "urnetwork-preparation-many-owners-v1")
	adapter := preparationManyOwnersAdapter()
	plan, err := PlanPreparationWithHost(t.Context(), f.reference, adapter, f.volume.host)
	if err != nil || len(plan.Owners) != 514 || len(plan.Sources) != 514 {
		t.Fatal("explicit many-owner profile cannot retain the complete namespace", err)
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	accepted := Reference{Path: filepath.Join(filepath.Dir(f.request.ControlPath), "many-plan.json"), Sha256: testDigest(raw)}
	if err := os.WriteFile(accepted.Path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	result, err := ApplyPreparationWithHost(t.Context(), accepted, adapter, f.volume.host)
	if err != nil || result.RestartAuthorized {
		t.Fatal("many-owner apply lost accepted custody", err)
	}
	control, err := os.ReadFile(f.request.ControlPath)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := ApplyPreparationWithHost(t.Context(), accepted, adapter, f.volume.host)
	after, readErr := os.ReadFile(f.request.ControlPath)
	if err != nil || readErr != nil || repeated != result || !bytes.Equal(control, after) {
		t.Fatal("many-owner replay reset completed control", err, readErr)
	}
	for _, owner := range plan.Owners {
		member := owner.Files[0]
		path := filepath.Join(f.request.RootPath, member.Path)
		actual, err := os.ReadFile(path)
		if err != nil || testDigest(actual) != member.Sha256 {
			t.Fatal("many-owner apply lost original member", member.Path, err)
		}
		if count, err := syscall.Getxattr(path, owner.Attributes[0].Name, nil); err != nil || count == 0 {
			t.Fatal("many-owner apply lost original head", member.Path, err)
		}
	}
}

// Increasing one profile dimension does not opt an old request into another
// profile, relax any byte bound, or let an unknown profile reach the adapter.
func TestPreparationManyOwnersRefusesUnreviewedAndUnboundedProfiles(t *testing.T) {
	for _, fault := range []string{"legacy", "unknown", "owners", "attributes", "attribute-bytes", "plan-bytes"} {
		f := newPreparationFixture(t)
		profile, count := "urnetwork-preparation-many-owners-v1", 514
		if fault == "legacy" {
			profile = ""
		} else if fault == "unknown" {
			profile = "synthetic-unapproved-profile"
		} else if fault == "owners" {
			count = 2049
		}
		preparationManyOwnersRequest(t, f, count, profile)
		raw, err := os.ReadFile(f.reference.Path)
		if err != nil {
			t.Fatal(err)
		}
		var wire map[string]json.RawMessage
		var limits map[string]uint64
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(wire["limits"], &limits); err != nil {
			t.Fatal(err)
		}
		switch fault {
		case "owners":
			limits["max_owner_attributes"], limits["max_owner_attribute_bytes"] = 2048, 2048*4096
		case "attributes":
			limits["max_owner_attributes"] = 2049
		case "attribute-bytes":
			limits["max_owner_attribute_bytes"] = 2048*4096 + 1
		case "plan-bytes":
			limits["max_plan_bytes"] = 8*1024*1024 + 1
		}
		wire["limits"], err = json.Marshal(limits)
		if err != nil {
			t.Fatal(err)
		}
		raw, err = json.Marshal(wire)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.reference.Path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		f.reference.Sha256 = testDigest(raw)
		calls := 0
		adapter := preparationManyOwnersAdapter()
		build := adapter.Build
		adapter.Build = func(ctx context.Context, parent *os.File, name string, owner PreparationOwner) (PreparationOwnerPlan, error) {
			calls++
			return build(ctx, parent, name, owner)
		}
		_, err = PlanPreparationWithHost(t.Context(), f.reference, adapter, f.volume.host)
		if err == nil || calls != 0 || !strings.Contains(err.Error(), "profile") {
			t.Fatal("invalid profile reached preparation effects or another boundary", fault, calls, err)
		}
		for _, path := range []string{f.request.RootPath, f.request.StagingDirectory} {
			entries, err := os.ReadDir(path)
			if err != nil || len(entries) != 0 {
				t.Fatal("refused profile mutated custody", fault, path, err)
			}
		}
	}
}

// A lost real attribute-sync acknowledgement retains completed preceding
// owners. The next joined public invocation reconciles only the exact plan.
func TestPreparationManyOwnersReconcilesOriginalPartialHeadUnion(t *testing.T) {
	f := newPreparationFixture(t)
	preparationManyOwnersRequest(t, f, 39, "urnetwork-preparation-many-owners-v1")
	adapter := preparationManyOwnersAdapter()
	plan, err := PlanPreparationWithHost(t.Context(), f.reference, adapter, f.volume.host)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	accepted := Reference{Path: filepath.Join(filepath.Dir(f.request.ControlPath), "many-partial-plan.json"), Sha256: testDigest(raw)}
	if err := os.WriteFile(accepted.Path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("synthetic original head sync acknowledgment lost")
	reached := false
	hooks := &preparationHooks{after: func(stage, path string) error {
		if stage == "attribute-sync" && strings.Contains(path, "member-0032.bin:") {
			reached = true
			return cause
		}
		return nil
	}}
	_, err = applyPreparation(t.Context(), accepted, adapter, f.volume.host, daemonScope, hooks)
	if !reached || !errors.Is(err, cause) {
		t.Fatal("partial union did not reach its real original sync", reached, err)
	}
	control, err := os.ReadFile(f.request.ControlPath)
	if err != nil {
		t.Fatal(err)
	}
	identities := make(map[string]syscall.Stat_t)
	for _, owner := range plan.Owners[:33] {
		path := filepath.Join(f.request.RootPath, owner.Files[0].Path)
		var identity syscall.Stat_t
		if err := syscall.Stat(path, &identity); err != nil {
			t.Fatal(err)
		}
		identities[path] = identity
	}
	result, err := ApplyPreparationWithHost(t.Context(), accepted, adapter, f.volume.host)
	after, readErr := os.ReadFile(f.request.ControlPath)
	if err != nil || readErr != nil || result.RestartAuthorized || !bytes.HasPrefix(after, control) {
		t.Fatal("joined partial union discarded original control", err, readErr)
	}
	for path, original := range identities {
		var current syscall.Stat_t
		if err := syscall.Stat(path, &current); err != nil || current.Ino != original.Ino || current.Dev != original.Dev {
			t.Fatal("joined partial union replaced an acknowledged member", path, err)
		}
	}
}
