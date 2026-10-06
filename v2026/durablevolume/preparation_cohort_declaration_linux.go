//go:build linux

// A cohort emits one usable declaration, preserving every unselected root.
// Existing mount authority stays exact while it still serves a healthy root;
// independent restored markers never become duplicate runtime mount records.
package durablevolume

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
)

// One planned physical root and its historical identity, if it is a restore.
type preparationCohortDeclarationRoot struct {
	volume   VolumeSpec
	original *Inventory
}

// Pure construction proves counts, overlaps and exact serialized capacity
// before any journal reservation. Count admission never promises byte capacity.
func preparationCohortDeclaration(retained Config, roots []preparationCohortDeclarationRoot) ([]byte, []string, error) {
	volumes := map[string]VolumeSpec{}
	oldRoots := map[string]StateRootSpec{}
	oldMounts := map[string]string{}
	for _, volume := range retained.Volumes {
		volumes[volume.MountPath] = volume
		for _, root := range volume.StateRoots {
			oldRoots[root.Path], oldMounts[root.Path] = root, volume.MountPath
		}
	}
	selected := map[string]preparationCohortDeclarationRoot{}
	for _, item := range roots {
		if len(item.volume.StateRoots) != 1 {
			return nil, nil, errors.New("cohort member declaration must contain exactly its planned root")
		}
		root := item.volume.StateRoots[0]
		if _, present := selected[root.Path]; present {
			return nil, nil, errors.New("cohort combined declaration repeats a root")
		}
		old, present := oldRoots[root.Path]
		if item.original != nil {
			if !present || old != item.original.StateRoot || oldMounts[root.Path] != item.original.MountPath || root.Path != item.original.StateRoot.Path {
				return nil, nil, errors.New("cohort restore differs from retained original root authority")
			}
			oldVolume := volumes[oldMounts[root.Path]]
			if oldVolume.FilesystemUuid != item.original.FilesystemUuid || oldVolume.MarkerSha256 != item.original.MarkerSha256 {
				return nil, nil, errors.New("cohort restore differs from retained original volume authority")
			}
		} else if present {
			return nil, nil, errors.New("cohort cannot enroll retained custody as fresh")
		}
		selected[root.Path] = item
	}
	var untouched []string
	healthyMounts := map[string]bool{}
	for path := range oldRoots {
		if _, present := selected[path]; !present {
			untouched = append(untouched, path)
			healthyMounts[oldMounts[path]] = true
		}
	}
	for mount, volume := range volumes {
		var remaining []StateRootSpec
		for _, root := range volume.StateRoots {
			if _, present := selected[root.Path]; !present {
				remaining = append(remaining, root)
			}
		}
		if len(remaining) == 0 {
			delete(volumes, mount)
		} else {
			volume.StateRoots = remaining
			volumes[mount] = volume
		}
	}
	// Input plans have a deterministic original-root order. When no healthy
	// root remains, the first selected marker becomes the combined mount marker.
	for _, item := range roots {
		volume, present := volumes[item.volume.MountPath]
		if !present {
			volume = item.volume
			volume.StateRoots = nil
		} else {
			if volume.FilesystemUuid != item.volume.FilesystemUuid || volume.FilesystemType != item.volume.FilesystemType {
				return nil, nil, errors.New("cohort repeats a mount with different filesystem authority")
			}
			if healthyMounts[item.volume.MountPath] {
				if volume.MinAvailableBytes < item.volume.MinAvailableBytes || volume.MinAvailableInodes < item.volume.MinAvailableInodes {
					return nil, nil, errors.New("cohort new reserve would change untouched root authority")
				}
			} else {
				volume.MinAvailableBytes = max(volume.MinAvailableBytes, item.volume.MinAvailableBytes)
				volume.MinAvailableInodes = max(volume.MinAvailableInodes, item.volume.MinAvailableInodes)
			}
		}
		volume.StateRoots = append(volume.StateRoots, item.volume.StateRoots[0])
		volumes[volume.MountPath] = volume
	}
	combined := Config{Schema: Schema}
	for _, volume := range volumes {
		sort.Slice(volume.StateRoots, func(i, j int) bool { return volume.StateRoots[i].Path < volume.StateRoots[j].Path })
		combined.Volumes = append(combined.Volumes, volume)
	}
	sort.Slice(combined.Volumes, func(i, j int) bool { return combined.Volumes[i].MountPath < combined.Volumes[j].MountPath })
	if err := combined.validateForScope(daemonScope); err != nil {
		return nil, nil, err
	}
	raw, err := json.Marshal(combined)
	if err != nil {
		return nil, nil, err
	}
	raw = append(raw, '\n')
	if len(raw) > maximumConfigBytes {
		return nil, nil, errors.New("cohort combined declaration exceeds runtime byte capacity")
	}
	sort.Strings(untouched)
	return raw, untouched, nil
}

// Retained-root guards share their ordinary read lease with healthy writers.
// No snapshot lease, owner checkpoint, original cohort hash or journal changes.
func preparationCohortRetained(ctx context.Context, cohort PreparationCohort, applications []*preparationApply, host Host) (raw []byte, guards []*Owner, resultErr error) {
	defer func() {
		if resultErr != nil {
			for _, guard := range guards {
				resultErr = errors.Join(resultErr, guard.Close())
			}
			guards = nil
		}
	}()
	var retained Config
	if cohort.RetainedDeclaration != nil {
		bytes, err := preparationReadReference(ctx, *cohort.RetainedDeclaration, maximumConfigBytes, "retained cohort declaration")
		if err != nil {
			return nil, nil, err
		}
		if err := errors.Join(decodeStrict(bytes, &retained), retained.validateForScope(daemonScope)); err != nil {
			return nil, nil, err
		}
	}
	roots := make([]preparationCohortDeclarationRoot, 0, len(applications))
	for _, self := range applications {
		request, plan := self.admission.request, self.plan
		volume := request.declaration(plan.Root.Inode, preparationDigest(plan.Generation), preparationDigest(plan.Marker), preparationDigest(plan.Lease)).Volumes[0]
		item := preparationCohortDeclarationRoot{volume: volume}
		if request.Purpose == "restore" {
			if cohort.RetainedDeclaration == nil {
				return nil, nil, errors.New("restore cohort requires its complete retained daemon declaration")
			}
			item.original = &self.inventory
		}
		roots = append(roots, item)
	}
	raw, paths, err := preparationCohortDeclaration(retained, roots)
	if err != nil {
		return nil, nil, err
	}
	for _, path := range paths {
		for _, self := range applications {
			request := self.admission.request
			for _, output := range []string{request.StagingDirectory, request.RootPath, request.MarkerPath, request.LeasePath, request.DeclarationPath, request.ControlPath} {
				if beneath(path, output) || output == request.StagingDirectory && beneath(output, path) {
					return nil, guards, errors.New("cohort publication overlaps untouched root custody")
				}
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, guards, err
		}
		guard, err := OpenWithHost(*cohort.RetainedDeclaration, path, ReadOnly, host)
		if err != nil {
			return nil, guards, err
		}
		guards = append(guards, guard)
	}
	return raw, guards, ctx.Err()
}
