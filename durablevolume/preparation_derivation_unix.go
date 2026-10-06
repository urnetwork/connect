//go:build linux

// A fixed restore profile may derive at most two unsigned physical censuses while every
// signed member stays byte-exact. Both census generations remain bound by the
// accepted plan; target publication preserves the reviewed staging inodes.
package durablevolume

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
)

// Physical metadata has a fixed flat namespace. Shared ownership requires the
// separately explicit and complete disjoint coverage profile.
func validatePreparationPhysicalMetadata(request PreparationRequest, owner PreparationOwnerPlan) error {
	metadata := owner.PhysicalMetadata
	if metadata == nil {
		return nil
	}
	legacy := owner.Owner.RestoreCoverage == "" && owner.ExclusiveRoot && len(request.Owners) == 1
	union := owner.Owner.RestoreCoverage == PreparationCompleteUnion
	if request.Purpose != "restore" || owner.Owner.Purpose != "restore" || !legacy && !union ||
		!preparationRelative(metadata.Path, 1, false) || metadata.MaximumBytes == 0 || metadata.MaximumBytes > 8*1024*1024 {
		return errors.New("physical metadata requires bounded exclusive or complete-union restore coverage")
	}
	if metadata.CompanionPath != "" && (!preparationRelative(metadata.CompanionPath, 1, false) || metadata.CompanionPath == metadata.Path) {
		return errors.New("physical metadata companion is invalid or aliases the original census")
	}
	if _, err := preparationRootRenameNumber(); err != nil {
		return err
	}
	present := map[string]bool{}
	for _, file := range owner.Files {
		if file.Kind != "file" || file.Mode != 0600 && file.Mode != 0400 || !preparationRelative(file.Path, 1, false) {
			return errors.New("physical metadata restore requires exact private flat members")
		}
		if preparationMetadataOwns(metadata, file.Path) {
			if file.Mode != 0600 || present[file.Path] || file.Bytes == 0 || file.Bytes > metadata.MaximumBytes {
				return errors.New("physical metadata census is duplicated or outside its reviewed capacity")
			}
			present[file.Path] = true
		}
	}
	if !present[metadata.Path] || metadata.CompanionPath != "" && !present[metadata.CompanionPath] {
		return errors.New("physical metadata lacks its original census file")
	}
	return nil
}

// The optional companion does not extend authority to any other member.
func preparationMetadataOwns(metadata *PreparationPhysicalMetadata, path string) bool {
	return metadata != nil && (path == metadata.Path || metadata.CompanionPath != "" && path == metadata.CompanionPath)
}

// The primary path remains first so absent companions preserve legacy plans.
func preparationMetadataPaths(metadata *PreparationPhysicalMetadata) []string {
	if metadata == nil {
		return nil
	}
	paths := []string{metadata.Path}
	if metadata.CompanionPath != "" {
		paths = append(paths, metadata.CompanionPath)
	}
	return paths
}

// The retained original metadata lives outside the moved owner namespace.
// Its deterministic plan-owned name cannot collide with an application member.
func preparationOriginalMetadataPath(request PreparationRequest, owner PreparationOwnerPlan) string {
	return filepath.Join(request.StagingDirectory, owner.StagingName+"-original-metadata")
}

// The separately retained companion never overwrites the first original.
func preparationOriginalMetadataMemberPath(request PreparationRequest, owner PreparationOwnerPlan, path string) string {
	original := preparationOriginalMetadataPath(request, owner)
	if owner.PhysicalMetadata != nil && path == owner.PhysicalMetadata.CompanionPath {
		return original + "-companion"
	}
	return original
}

// Large metadata is read once with a finite bound and before/after named-file
// checks. An error returns no admitted bytes, even after a complete read.
func preparationReadOriginalMetadata(ctx context.Context, source PreparationSource, maximum uint64) (result []byte, resultErr error) {
	defer func() {
		if resultErr != nil {
			result = nil
		}
	}()
	if source.File.Kind != "file" || source.File.Mode != 0600 || source.File.Bytes == 0 || source.File.Bytes > maximum || maximum > 8*1024*1024 {
		return nil, errors.New("original physical metadata has invalid read bounds")
	}
	file, err := preparationOpenAbsolute(source.Path, false)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	identity, err := preparationIdentity(file)
	if err != nil {
		return nil, err
	}
	if identity != source.Identity {
		return nil, errors.Join(ErrIdentity, errors.New("original physical metadata generation changed"))
	}
	result, err = boundedProtectedReadContext(ctx, file, int(maximum))
	if err != nil {
		return nil, err
	}
	if uint64(len(result)) != source.File.Bytes || preparationDigest(result) != source.File.Sha256 {
		return nil, errors.Join(ErrIdentity, errors.New("original physical metadata bytes changed"))
	}
	return result, errors.Join(sameNamedFile(file, source.Path), ctx.Err())
}

// Planning owns the new staging namespace. It keeps the original metadata
// inode and publishes only separately derived bytes at the target member name.
func preparePhysicalMetadata(ctx context.Context, admission *preparationAdmission, adapter PreparationAdapter, report Inventory, plan *PreparationPlan, index int) (resultErr error) {
	owner := plan.Owners[index]
	if err := validatePreparationPhysicalMetadata(admission.request, owner); err != nil {
		return err
	}
	if owner.PhysicalMetadata == nil {
		return nil
	}
	if adapter.RebindRestore == nil {
		return errors.New("physical metadata restore lacks its fixed derivation adapter")
	}
	parentPath := filepath.Join(admission.request.StagingDirectory, owner.StagingName)
	parent, err := preparationOpenAbsolute(parentPath, true)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, parent.Close()) }()
	originals := map[string]PreparationSource{}
	targets := make([]PreparationSource, 0, len(owner.Files))
	for _, member := range owner.Files {
		if err := ctx.Err(); err != nil {
			return err
		}
		path := filepath.Join(parentPath, member.Path)
		file, err := preparationOpenAbsolute(path, false)
		if err != nil {
			return err
		}
		identity, observeErr := preparationIdentity(file)
		readErr := preparationVerifyFile(ctx, file, member.Bytes, member.Sha256)
		if err := errors.Join(observeErr, readErr, sameNamedFile(file, path), file.Close()); err != nil {
			return err
		}
		source := PreparationSource{File: member, Path: path, Identity: identity}
		if preparationMetadataOwns(owner.PhysicalMetadata, member.Path) {
			originals[member.Path] = source
		} else {
			targets = append(targets, source)
		}
	}
	derivedBytes := map[string][]byte{}
	for _, path := range preparationMetadataPaths(owner.PhysicalMetadata) {
		raw, err := preparationReadOriginalMetadata(ctx, originals[path], owner.PhysicalMetadata.MaximumBytes)
		if err != nil {
			return err
		}
		derived, err := adapter.RebindRestore(ctx, owner, report, raw, targets)
		if err := errors.Join(err, ctx.Err()); err != nil {
			return err
		}
		if len(derived) == 0 || uint64(len(derived)) > owner.PhysicalMetadata.MaximumBytes {
			return errors.New("derived physical metadata exceeds its fixed runtime capacity")
		}
		derivedBytes[path] = derived
	}
	// Both transformations finish before moving either original. The two
	// retained buffers are independently bounded, never sized by disk input.
	owner.Files = append([]PreparationFile(nil), owner.Files...)
	for _, path := range preparationMetadataPaths(owner.PhysicalMetadata) {
		original, derived := originals[path], derivedBytes[path]
		if err := admission.check(); err != nil {
			return err
		}
		backup := preparationOriginalMetadataMemberPath(admission.request, owner, path)
		staging := admission.directories[admission.request.StagingDirectory]
		if err := preparationRenameRoot(parent, path, staging, filepath.Base(backup)); err != nil {
			return err
		}
		original.Path = backup
		if _, err := preparationReadOriginalMetadata(ctx, original, owner.PhysicalMetadata.MaximumBytes); err != nil {
			return err
		}
		if err := func() (writeErr error) {
			fd, err := syscall.Openat(int(parent.Fd()), path, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
			if err != nil {
				return err
			}
			file := os.NewFile(uintptr(fd), filepath.Join(parentPath, path))
			defer func() { writeErr = errors.Join(writeErr, file.Close()) }()
			for offset := 0; offset < len(derived); {
				if err := ctx.Err(); err != nil {
					return err
				}
				part := derived[offset:min(offset+64*1024, len(derived))]
				n, err := file.Write(part)
				if err != nil || n != len(part) {
					return errors.Join(err, io.ErrShortWrite)
				}
				offset += n
			}
			return errors.Join(file.Sync(), parent.Sync(), staging.Sync(), sameNamedFile(file, file.Name()), admission.check())
		}(); err != nil {
			return err
		}
		var derivedFile PreparationFile
		for fileIndex := range owner.Files {
			if owner.Files[fileIndex].Path == path {
				owner.Files[fileIndex].Bytes = uint64(len(derived))
				owner.Files[fileIndex].Sha256 = preparationDigest(derived)
				derivedFile = owner.Files[fileIndex]
			}
		}
		plan.Derivations = append(plan.Derivations, PreparationDerivation{OwnerIndex: index, Original: original, Derived: derivedFile})
	}
	plan.Owners[index] = owner
	return nil
}

// The exact planned namespace decides movement. An absent optional field
// retains the historical copy path and byte-identical control step format.
func preparationSourceMoves(plan PreparationPlan, source PreparationSource) bool {
	for _, owner := range plan.Owners {
		if owner.PhysicalMetadata != nil && filepath.Base(filepath.Dir(source.Path)) == owner.StagingName {
			return true
		}
	}
	return false
}

// Build the immutable lookup once per joined invocation rather than scanning
// thousands of historical source records before every individual move.
func preparationMoveSources(plan PreparationPlan) map[string]PreparationSource {
	sources := map[string]PreparationSource{}
	for _, source := range plan.Sources {
		if preparationSourceMoves(plan, source) {
			sources[source.Path] = source
		}
	}
	return sources
}

// Accepted derivation replays the fixed pure transformation over the retained
// original census and reviewed member identities. No runtime alias is added.
func validatePhysicalMetadataDerivations(ctx context.Context, request PreparationRequest, plan PreparationPlan, adapter PreparationAdapter, report Inventory) error {
	derivations := map[int]map[string]PreparationDerivation{}
	for _, derivation := range plan.Derivations {
		if derivation.OwnerIndex < 0 || derivation.OwnerIndex >= len(plan.Owners) {
			return errors.New("physical metadata derivation owner is invalid or repeated")
		}
		if derivations[derivation.OwnerIndex] == nil {
			derivations[derivation.OwnerIndex] = map[string]PreparationDerivation{}
		}
		path := derivation.Original.File.Path
		if _, present := derivations[derivation.OwnerIndex][path]; present {
			return errors.New("physical metadata derivation owner is invalid or repeated")
		}
		derivations[derivation.OwnerIndex][path] = derivation
	}
	for index, owner := range plan.Owners {
		if err := validatePreparationPhysicalMetadata(request, owner); err != nil {
			return err
		}
		derived := derivations[index]
		if owner.PhysicalMetadata == nil {
			if len(derived) != 0 {
				return errors.New("ordinary preparation cannot acquire a physical metadata rewrite")
			}
			continue
		}
		paths := preparationMetadataPaths(owner.PhysicalMetadata)
		if len(derived) != len(paths) || adapter.RebindRestore == nil {
			return errors.New("physical metadata lost original derivation lineage")
		}
		for _, path := range paths {
			derivation, present := derived[path]
			if !present || derivation.Original.Path != preparationOriginalMetadataMemberPath(request, owner, path) || derivation.Derived.Path != path {
				return errors.New("physical metadata lost original derivation lineage")
			}
		}
		expected, err := adapter.Restore(ctx, owner.StagingName, owner.Owner, report)
		if err != nil {
			return err
		}
		if expected.PhysicalMetadata == nil || !reflect.DeepEqual(expected.PhysicalMetadata, owner.PhysicalMetadata) {
			return errors.New("physical metadata derivation differs from its fixed owner")
		}
		originalOwner := expected
		expected.Files = append([]PreparationFile(nil), expected.Files...)
		found := 0
		for fileIndex, file := range expected.Files {
			if preparationMetadataOwns(owner.PhysicalMetadata, file.Path) {
				derivation := derived[file.Path]
				if file != derivation.Original.File {
					return errors.New("original physical metadata differs from its exported authority")
				}
				expected.Files[fileIndex] = derivation.Derived
				found++
			}
		}
		if found != len(paths) || !reflect.DeepEqual(expected, owner) {
			return errors.New("physical derivation changed a signed member or unrelated owner field")
		}
		targets := make([]PreparationSource, 0, len(owner.Files)-len(paths))
		for _, source := range plan.Sources {
			if filepath.Base(filepath.Dir(source.Path)) == owner.StagingName && !preparationMetadataOwns(owner.PhysicalMetadata, source.File.Path) {
				targets = append(targets, source)
			}
		}
		for _, path := range paths {
			derivation := derived[path]
			raw, err := preparationReadOriginalMetadata(ctx, derivation.Original, owner.PhysicalMetadata.MaximumBytes)
			if err != nil {
				return err
			}
			derivedRaw, err := adapter.RebindRestore(ctx, originalOwner, report, raw, targets)
			if err := errors.Join(err, ctx.Err()); err != nil {
				return err
			}
			if uint64(len(derivedRaw)) != derivation.Derived.Bytes || len(derivedRaw) == 0 || uint64(len(derivedRaw)) > owner.PhysicalMetadata.MaximumBytes || preparationDigest(derivedRaw) != derivation.Derived.Sha256 {
				return errors.Join(ErrIdentity, errors.New("physical metadata derivation no longer matches the reviewed result"))
			}
		}
	}
	return nil
}
