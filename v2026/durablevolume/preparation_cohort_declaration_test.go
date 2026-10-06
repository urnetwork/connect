//go:build linux

// Counts and serialized bytes are independent runtime limits. These compact
// synthetic declarations exercise the accepted count edges without real mounts.
package durablevolume

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

// Each separate planned declaration names one root on a shared synthetic mount.
func preparationCohortCompactRoots(mounts, roots int) []preparationCohortDeclarationRoot {
	var result []preparationCohortDeclarationRoot
	for m := 0; m < mounts; m++ {
		mount := fmt.Sprintf("/%c", 'a'+m)
		for index := 0; index < roots; index++ {
			root := StateRootSpec{Path: fmt.Sprintf("%s/r%x", mount, index), LeasePath: fmt.Sprintf("%s/l%x", mount, index), LeaseSha256: testDigest([]byte("synthetic lease")), RootInode: uint64(index + 1), GenerationSha256: testDigest([]byte("synthetic root generation"))}
			volume := VolumeSpec{MountPath: mount, FilesystemUuid: "abcd", FilesystemType: "ext4", MarkerPath: mount + "/m", MarkerSha256: testDigest([]byte("synthetic marker")), StateRoots: []StateRootSpec{root}, MinAvailableBytes: 1, MinAvailableInodes: 1}
			result = append(result, preparationCohortDeclarationRoot{volume: volume})
		}
	}
	return result
}

// Sixty-four independent plans for one mount produce one usable volume entry.
func TestPreparationCohortDeclarationJoinsExactPerMountBoundary(t *testing.T) {
	roots := preparationCohortCompactRoots(1, 64)
	raw, untouched, err := preparationCohortDeclaration(Config{}, roots)
	var config Config
	if err != nil {
		t.Fatal(err)
	}
	if err := decodeStrict(raw, &config); err != nil {
		t.Fatal(err)
	}
	if len(untouched) != 0 || len(config.Volumes) != 1 || len(config.Volumes[0].StateRoots) != 64 {
		t.Fatal("duplicate mount plans were not joined", config)
	}
	overflow := preparationCohortCompactRoots(1, 65)
	if _, _, err := preparationCohortDeclaration(Config{}, overflow); err == nil {
		t.Fatal("65 roots admitted on one mount")
	}
}

// The four-volume 256-root profile fits only when its actual bytes also fit.
func TestPreparationCohortDeclarationJoinsExactTotalBoundary(t *testing.T) {
	roots := preparationCohortCompactRoots(4, 64)
	raw, _, err := preparationCohortDeclaration(Config{}, roots)
	if err != nil || len(raw) > maximumConfigBytes {
		t.Fatal("exact accepted total refused", len(raw), err)
	}
	var config Config
	if err := decodeStrict(raw, &config); err != nil {
		t.Fatal(err)
	}
	if err := config.validate(); err != nil || len(config.Volumes) != 4 {
		t.Fatal("combined total is not a runtime declaration", err)
	}
	extra := preparationCohortCompactRoots(5, 1)[4]
	if _, _, err := preparationCohortDeclaration(Config{}, append(roots, extra)); err == nil {
		t.Fatal("257 roots admitted")
	}
	for index := range roots {
		root := &roots[index].volume.StateRoots[0]
		root.Path += strings.Repeat("x", 100)
		root.LeasePath += strings.Repeat("y", 100)
	}
	if _, _, err := preparationCohortDeclaration(Config{}, roots); err == nil || !strings.Contains(err.Error(), "byte capacity") {
		t.Fatal("count-only admission bypassed serialized capacity", err)
	}
}

// A count overflow refuses before opening nonexistent healthy roots or touching
// either real target; the complete original declaration remains unchanged.
func TestPreparationCohortDeclarationOverflowPrecedesFirstMutation(t *testing.T) {
	f := newPreparationCohortFixture(t, false)
	volume := f.members[0].request.declaration(1, testDigest([]byte("generation")), testDigest([]byte("marker")), testDigest([]byte("lease"))).Volumes[0]
	volume.StateRoots = nil
	for index := 0; index < 63; index++ {
		volume.StateRoots = append(volume.StateRoots, StateRootSpec{Path: filepath.Join(volume.MountPath, fmt.Sprintf("healthy-%d", index)), LeasePath: filepath.Join(volume.MountPath, fmt.Sprintf("healthy-lease-%d", index)), LeaseSha256: testDigest([]byte("lease")), RootInode: uint64(index + 1), GenerationSha256: testDigest([]byte("generation"))})
	}
	config := Config{Schema: Schema, Volumes: []VolumeSpec{volume}}
	ref := f.document(t, "retained-declaration.json", config)
	f.cohort.RetainedDeclaration = &ref
	f.reference = f.document(t, "cohort-with-retained.json", f.cohort)
	if _, err := f.apply(t.Context(), nil); err == nil || !strings.Contains(err.Error(), "roots or positive reserve") {
		t.Fatal("overflow did not refuse during combined admission", err)
	}
	for _, member := range f.members {
		if _, err := os.Stat(member.request.ControlPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("overflow created a journal", err)
		}
		entries, err := os.ReadDir(member.request.RootPath)
		if err != nil || len(entries) != 0 {
			t.Fatal("overflow changed a target", err)
		}
	}
}

// A healthy owner keeps its original generation, lease, marker and reserve;
// the same mount can simultaneously admit both completed restored/fresh peers.
func TestPreparationCohortCombinedDeclarationPreservesHealthyOwner(t *testing.T) {
	f := newPreparationCohortFixture(t, false)
	volume := f.members[0].volume
	retained, err := Load(volume.reference)
	if err != nil {
		t.Fatal(err)
	}
	// The fixture's existing declared root is independent of both fresh plans.
	ref := volume.reference
	originalCohort := []byte(`{"schema":"synthetic-retained-root-cohort","cohort_sha256":"` + testDigest([]byte("original unrelated cohort")) + `"}`)
	if err := syscall.Setxattr(volume.root, PreparationAttribute, originalCohort, 1); err != nil {
		t.Fatal(err)
	}
	f.cohort.RetainedDeclaration = &ref
	f.reference = f.document(t, "cohort-preserving-healthy.json", f.cohort)
	healthy, err := OpenWithHost(ref, retained.Volumes[0].StateRoots[0].Path, ReadWrite, volume.host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := healthy.Close(); err != nil {
			t.Error(err)
		}
	})
	result, err := f.apply(t.Context(), nil)
	if err != nil || !result.Complete || result.DeclarationDocument == "" {
		t.Fatal("healthy peer blocked combined publication", err)
	}
	var combined Config
	if err := decodeStrict([]byte(result.DeclarationDocument), &combined); err != nil {
		t.Fatal(err)
	}
	oldVolume := retained.Volumes[0]
	newVolume := combined.Volumes[0]
	var found bool
	for _, root := range newVolume.StateRoots {
		if root == oldVolume.StateRoots[0] {
			found = true
		}
	}
	newVolume.StateRoots = oldVolume.StateRoots
	if !found || !reflect.DeepEqual(oldVolume, newVolume) {
		t.Fatal("healthy authority was changed")
	}
	path := filepath.Join(filepath.Dir(f.reference.Path), "combined.json")
	if err := os.WriteFile(path, []byte(result.DeclarationDocument), 0600); err != nil {
		t.Fatal(err)
	}
	combinedRef := Reference{Path: path, Sha256: result.DeclarationSha256}
	for _, member := range f.members {
		guard, err := OpenWithHost(combinedRef, member.request.RootPath, ReadWrite, volume.host)
		if err != nil {
			t.Fatal("combined runtime guard refused", err)
		}
		if err := guard.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := healthy.CheckWrite(); err != nil {
		t.Fatal("healthy owner lost admission", err)
	}
	retainedAnchor := make([]byte, len(originalCohort))
	if size, err := syscall.Getxattr(volume.root, PreparationAttribute, retainedAnchor); err != nil || size != len(originalCohort) || !bytes.Equal(retainedAnchor, originalCohort) {
		t.Fatal("untouched original per-root cohort authority changed", err)
	}
	checked, err := CheckPreparationCohortWithHost(t.Context(), f.reference, preparationTestAdapter(), volume.host)
	if err != nil || !checked.Complete || checked.Applied || !bytes.Equal([]byte(result.DeclarationDocument), []byte(checked.DeclarationDocument)) {
		t.Fatal("read-only completed recovery changed declaration", err)
	}
	var decoded Config
	if err := json.Unmarshal([]byte(checked.DeclarationDocument), &decoded); err != nil {
		t.Fatal(err)
	}
}
