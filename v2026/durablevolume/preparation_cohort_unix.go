//go:build linux || darwin

// A cohort holds all root leases and authenticates all retained prefixes before
// its first mutation. Publication is incremental, never an atomic rollback: a
// failed later root resumes through the same exact per-root journals and cohort.
package durablevolume

import (
	"context"
	"errors"
	"os"
)

// The request itself is bounded separately from all referenced plan bytes.
func readPreparationCohort(ctx context.Context, reference Reference, scope ownerScope) (PreparationCohort, error) {
	var cohort PreparationCohort
	raw, err := preparationReadReference(ctx, reference, maximumPreparationRequestBytes, "cohort")
	if err != nil {
		return cohort, err
	}
	if err := decodeStrict(raw, &cohort); err != nil {
		return cohort, err
	}
	limit := cohort.Limits
	if scope != daemonScope || cohort.Schema != PreparationCohortSchema || cohort.Scope != "daemon" ||
		limit.MaxRoots < 2 || limit.MaxRoots > 256 || len(cohort.Plans) < 2 || uint64(len(cohort.Plans)) > limit.MaxRoots ||
		limit.MaxPlanBytes == 0 || limit.MaxPlanBytes > 64*1024*1024 || limit.MaxControlBytes == 0 || limit.MaxControlBytes > 64*1024*1024 ||
		limit.MaxEntries == 0 || limit.MaxEntries > 1024*1024 || limit.MaxBytes == 0 || limit.MaxBytes > 1024*1024*1024*1024 ||
		limit.MaxOwnerAttributes == 0 || limit.MaxOwnerAttributes > 4096 || limit.MaxOwnerAttributeBytes == 0 || limit.MaxOwnerAttributeBytes > 4096*4096 {
		return cohort, errors.New("preparation cohort schema, scope or aggregate capacity is invalid")
	}
	seen := map[string]bool{}
	if cohort.RetainedDeclaration != nil {
		if !canonical(cohort.RetainedDeclaration.Path) || !validDigest(cohort.RetainedDeclaration.Sha256) || cohort.RetainedDeclaration.Path == reference.Path {
			return cohort, errors.New("cohort requires an exact separate retained declaration")
		}
		seen[cohort.RetainedDeclaration.Path] = true
	}
	for _, plan := range cohort.Plans {
		if !canonical(plan.Path) || !validDigest(plan.Sha256) || seen[plan.Path] {
			return cohort, errors.New("preparation cohort repeats or omits an exact plan reference")
		}
		seen[plan.Path] = true
	}
	return cohort, ctx.Err()
}

// Attribute/control records are retained in memory across admission, so their
// aggregate byte ceilings apply before any individual bounded read allocates.
func preparationCohortFileSize(path string, absent bool) (uint64, error) {
	file, err := openProtectedFile(path)
	if absent && errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	info, observeErr := file.Stat()
	if err := errors.Join(observeErr, sameNamedFile(file, path), file.Close()); err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 {
		return 0, errors.New("cohort metadata is not a finite regular file")
	}
	return uint64(info.Size()), nil
}

// Root paths are sorted before leases are acquired; locks are nonblocking.
// No global state lock, RPC, service start, or signer is involved.
func prepareCohort(ctx context.Context, reference Reference, adapter PreparationAdapter, host Host, scope ownerScope, apply bool, hooks *preparationHooks) (result PreparationCohortResult, resultErr error) {
	cohort, err := readPreparationCohort(ctx, reference, scope)
	if err != nil {
		return result, err
	}
	applications := make([]*preparationApply, 0, len(cohort.Plans))
	var retainedGuards []*Owner
	defer func() {
		for index := len(applications) - 1; index >= 0; index-- {
			resultErr = errors.Join(resultErr, applications[index].close())
		}
		for _, guard := range retainedGuards {
			resultErr = errors.Join(resultErr, guard.Close())
		}
		if resultErr != nil {
			result = PreparationCohortResult{}
		}
	}()
	limit := cohort.Limits
	var planBytes, controlBytes, entries, memberBytes, attributes uint64
	sharedParents := map[string]*os.File{}
	for _, plan := range cohort.Plans {
		size, err := preparationCohortFileSize(plan.Path, false)
		if err != nil {
			return result, err
		}
		if size > limit.MaxPlanBytes-planBytes {
			return result, errors.New("cohort aggregate plan bytes exceed approval")
		}
		planBytes += size
		self, err := openPreparationApplication(ctx, plan, adapter, host, scope, hooks, sharedParents)
		if err != nil {
			return result, err
		}
		applications = append(applications, self)
		self.cohort = reference
		request := self.admission.request
		if len(applications) > 1 && applications[len(applications)-2].admission.request.RootPath >= request.RootPath {
			return result, errors.New("cohort roots must be distinct and sorted by original logical path")
		}
		size, err = preparationCohortFileSize(request.ControlPath, true)
		if err != nil {
			return result, err
		}
		var maximumControl uint64
		if err := preparationControlCapacity(ctx, request, self.plan, &maximumControl); err != nil {
			return result, err
		}
		size = max(size, maximumControl)
		if size > limit.MaxControlBytes-controlBytes {
			return result, errors.New("cohort retained control bytes exceed approval")
		}
		controlBytes += size
		for _, source := range self.plan.Sources {
			if entries == limit.MaxEntries || source.File.Bytes > limit.MaxBytes-memberBytes {
				return result, errors.New("cohort member count or byte capacity is exceeded")
			}
			entries++
			memberBytes += source.File.Bytes
		}
		for _, owner := range self.plan.Owners {
			if uint64(len(owner.Attributes)) > limit.MaxOwnerAttributes-attributes {
				return result, errors.New("cohort owner attribute count exceeds approval")
			}
			attributes += uint64(len(owner.Attributes))
		}
		if attributes > limit.MaxOwnerAttributeBytes/4096 {
			return result, errors.New("cohort fixed checkpoint byte allowance exceeds approval")
		}
	}
	if err := validatePreparationCohortNamespaces(reference, cohort.RetainedDeclaration, applications); err != nil {
		return result, err
	}
	declaration, retainedGuards, err := preparationCohortRetained(ctx, cohort, applications, host)
	if err != nil {
		return result, err
	}
	checkRetained := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, guard := range retainedGuards {
			if err := guard.CheckRead(); err != nil {
				return err
			}
		}
		if cohort.RetainedDeclaration != nil {
			_, err := preparationReadReference(ctx, *cohort.RetainedDeclaration, maximumConfigBytes, "retained cohort declaration")
			return err
		}
		return nil
	}
	if err := preparationCohortReserves(applications); err != nil {
		return result, err
	}
	// Every original prefix, source and target is validated before openControl
	// can create the first header. All admitted leases remain held until join.
	for _, self := range applications {
		if err := self.preflight(); err != nil {
			return result, err
		}
	}
	for _, self := range applications {
		if err := self.check(); err != nil {
			return result, err
		}
	}
	if err := checkRetained(); err != nil {
		return result, err
	}
	result = PreparationCohortResult{Schema: PreparationCohortResultSchema, Cohort: reference, Applied: apply, Complete: true}
	for _, self := range applications {
		result.Roots = append(result.Roots, self.admission.request.RootPath)
		result.Complete = result.Complete && self.complete
	}
	if apply {
		for _, self := range applications {
			if err := self.checkRestore(); err != nil {
				return result, err
			}
			prepared := self.prepared
			if self.complete {
				// Read-only preflight already authenticated the completed bytes.
				// Recheck retained named identities without replaying their hashes.
				if err := self.finalCensus(); err != nil {
					return result, err
				}
			} else {
				if err := self.openControl(); err != nil {
					return result, err
				}
				prepared, err = self.run()
				if err != nil {
					return result, err
				}
			}
			result.Results = append(result.Results, prepared)
			if err := self.after("cohort-root-complete", self.admission.request.RootPath); err != nil {
				return result, self.uncertain(err)
			}
			if err := checkRetained(); err != nil {
				return result, err
			}
		}
		result.Complete = true
	}
	for _, self := range applications {
		if err := self.checkRestore(); err != nil {
			return result, err
		}
	}
	if result.Complete {
		result.DeclarationDocument, result.DeclarationSha256 = string(declaration), preparationDigest(declaration)
	}
	return result, ctx.Err()
}

// No target can contain another root, its source archive, or a peer's immutable
// input/metadata. Shared protected staging parents are allowed; destinations
// and root generations remain distinct even when byte contents are identical.
func validatePreparationCohortNamespaces(reference Reference, retained *Reference, applications []*preparationApply) error {
	destinations := map[string]bool{}
	for _, self := range applications {
		request := self.admission.request
		for _, path := range []string{request.MarkerPath, request.LeasePath, request.DeclarationPath, request.ControlPath} {
			if destinations[path] {
				return errors.New("cohort repeats an external publication destination")
			}
			destinations[path] = true
		}
	}
	for _, self := range applications {
		request := self.admission.request
		paths := []string{reference.Path, self.reference.Path, self.plan.Request.Path, request.StagingDirectory, request.FormerWriterFence.Path,
			request.MarkerPath, request.LeasePath, request.DeclarationPath, request.ControlPath}
		inputs := []string{reference.Path, self.reference.Path, self.plan.Request.Path, request.FormerWriterFence.Path}
		if retained != nil {
			paths = append(paths, retained.Path)
			inputs = append(inputs, retained.Path)
		}
		if request.RestoreSource != nil {
			paths = append(paths, request.RestoreSource.Directory, request.RestoreSource.Inventory.Path, request.RestoreSource.FormerWriterFence.Path)
			inputs = append(inputs, request.RestoreSource.Inventory.Path, request.RestoreSource.FormerWriterFence.Path)
		}
		for _, other := range applications {
			root := other.admission.request.RootPath
			if self != other && (beneath(root, request.RootPath) || beneath(request.RootPath, root) || self.plan.Root.Device == other.plan.Root.Device && self.plan.Root.Inode == other.plan.Root.Inode) {
				return errors.New("cohort roots overlap or repeat a physical generation")
			}
			for _, path := range paths {
				if beneath(root, path) {
					return errors.New("cohort input or metadata overlaps a target root")
				}
			}
		}
		for _, path := range inputs {
			if destinations[path] {
				return errors.New("cohort output aliases an immutable input")
			}
		}
	}
	return nil
}

// Roots sharing a filesystem cannot each spend the same reported free reserve.
// The aggregate remains a preflight observation, not a reservation against
// unrelated healthy owners, whose later consumption may cause safe refusal.
func preparationCohortReserves(applications []*preparationApply) error {
	type reserve struct{ bytes, inodes, availableBytes, availableInodes uint64 }
	type filesystemIdentity struct {
		device Device
		id     [2]int32
		kind   int64
	}
	reserves := map[filesystemIdentity]reserve{}
	for _, self := range applications {
		admission := self.admission
		key := filesystemIdentity{device: admission.mount.Device, id: admission.filesystem.Id, kind: admission.filesystem.Type}
		current, present := reserves[key]
		if !present {
			current.availableBytes, current.availableInodes = admission.filesystem.AvailableBytes, admission.filesystem.AvailableInodes
		}
		current.availableBytes = min(current.availableBytes, admission.filesystem.AvailableBytes)
		current.availableInodes = min(current.availableInodes, admission.filesystem.AvailableInodes)
		if current.bytes > current.availableBytes || current.inodes > current.availableInodes || admission.request.MinAvailableBytes > current.availableBytes-current.bytes || admission.request.MinAvailableInodes > current.availableInodes-current.inodes {
			return &UnavailableError{Reason: "cohort aggregate byte or inode reserve is unavailable"}
		}
		current.bytes += admission.request.MinAvailableBytes
		current.inodes += admission.request.MinAvailableInodes
		reserves[key] = current
	}
	return nil
}
