//go:build linux

// A fixed restore profile may derive one unsigned physical census while every
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

// This first physical-metadata profile is a flat, exclusive owner namespace.
// Shared-owner union admission is distinct; no overlapping claim is filtered.
func validatePreparationPhysicalMetadata(request PreparationRequest, owner PreparationOwnerPlan) error {
	metadata := owner.PhysicalMetadata
	if metadata == nil {
		return nil
	}
	if request.Purpose != "restore" || owner.Owner.Purpose != "restore" || !owner.ExclusiveRoot || len(request.Owners) != 1 ||
		!preparationRelative(metadata.Path, 1, false) || metadata.MaximumBytes == 0 || metadata.MaximumBytes > 8*1024*1024 {
		return errors.New("physical metadata requires a bounded exclusive restore profile")
	}
	if _, err := preparationRootRenameNumber(); err != nil {
		return err
	}
	present := false
	for _, file := range owner.Files {
		if file.Kind != "file" || file.Mode != 0600 || !preparationRelative(file.Path, 1, false) {
			return errors.New("physical metadata restore requires exact private flat members")
		}
		if file.Path == metadata.Path {
			if present || file.Bytes == 0 || file.Bytes > metadata.MaximumBytes {
				return errors.New("physical metadata census is duplicated or outside its reviewed capacity")
			}
			present = true
		}
	}
	if !present {
		return errors.New("physical metadata lacks its original census file")
	}
	return nil
}

// The retained original metadata lives outside the moved owner namespace.
// Its deterministic plan-owned name cannot collide with an application member.
func preparationOriginalMetadataPath(request PreparationRequest, owner PreparationOwnerPlan) string {
	return filepath.Join(request.StagingDirectory, owner.StagingName+"-original-metadata")
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
	var original PreparationSource
	targets := make([]PreparationSource, 0, len(owner.Files)-1)
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
		if member.Path == owner.PhysicalMetadata.Path {
			original = source
		} else {
			targets = append(targets, source)
		}
	}
	raw, err := preparationReadOriginalMetadata(ctx, original, owner.PhysicalMetadata.MaximumBytes)
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
	if err := admission.check(); err != nil {
		return err
	}
	backup := preparationOriginalMetadataPath(admission.request, owner)
	staging := admission.directories[admission.request.StagingDirectory]
	if err := preparationRenameRoot(parent, owner.PhysicalMetadata.Path, staging, filepath.Base(backup)); err != nil {
		return err
	}
	original.Path = backup
	if _, err := preparationReadOriginalMetadata(ctx, original, owner.PhysicalMetadata.MaximumBytes); err != nil {
		return err
	}
	fd, err := syscall.Openat(int(parent.Fd()), owner.PhysicalMetadata.Path, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), filepath.Join(parentPath, owner.PhysicalMetadata.Path))
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
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
	if err := errors.Join(file.Sync(), parent.Sync(), staging.Sync(), sameNamedFile(file, file.Name()), admission.check()); err != nil {
		return err
	}
	owner.Files = append([]PreparationFile(nil), owner.Files...)
	var derivedFile PreparationFile
	for fileIndex := range owner.Files {
		if owner.Files[fileIndex].Path == owner.PhysicalMetadata.Path {
			owner.Files[fileIndex].Bytes = uint64(len(derived))
			owner.Files[fileIndex].Sha256 = preparationDigest(derived)
			derivedFile = owner.Files[fileIndex]
		}
	}
	plan.Owners[index] = owner
	plan.Derivations = append(plan.Derivations, PreparationDerivation{OwnerIndex: index, Original: original, Derived: derivedFile})
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
	derivations := map[int]PreparationDerivation{}
	for _, derivation := range plan.Derivations {
		if _, present := derivations[derivation.OwnerIndex]; present || derivation.OwnerIndex < 0 || derivation.OwnerIndex >= len(plan.Owners) {
			return errors.New("physical metadata derivation owner is invalid or repeated")
		}
		derivations[derivation.OwnerIndex] = derivation
	}
	for index, owner := range plan.Owners {
		if err := validatePreparationPhysicalMetadata(request, owner); err != nil {
			return err
		}
		derivation, derived := derivations[index]
		if owner.PhysicalMetadata == nil {
			if derived {
				return errors.New("ordinary preparation cannot acquire a physical metadata rewrite")
			}
			continue
		}
		if !derived || adapter.RebindRestore == nil || derivation.Original.Path != preparationOriginalMetadataPath(request, owner) || derivation.Original.File.Path != owner.PhysicalMetadata.Path || derivation.Derived.Path != owner.PhysicalMetadata.Path {
			return errors.New("physical metadata lost original derivation lineage")
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
		found := false
		for fileIndex, file := range expected.Files {
			if file.Path == owner.PhysicalMetadata.Path {
				if file != derivation.Original.File {
					return errors.New("original physical metadata differs from its exported authority")
				}
				expected.Files[fileIndex] = derivation.Derived
				found = true
			}
		}
		if !found || !reflect.DeepEqual(expected, owner) {
			return errors.New("physical derivation changed a signed member or unrelated owner field")
		}
		raw, err := preparationReadOriginalMetadata(ctx, derivation.Original, owner.PhysicalMetadata.MaximumBytes)
		if err != nil {
			return err
		}
		targets := make([]PreparationSource, 0, len(owner.Files)-1)
		for _, source := range plan.Sources {
			if preparationSourceMoves(plan, source) && source.File.Path != owner.PhysicalMetadata.Path {
				targets = append(targets, source)
			}
		}
		derivedRaw, err := adapter.RebindRestore(ctx, originalOwner, report, raw, targets)
		if err := errors.Join(err, ctx.Err()); err != nil {
			return err
		}
		if uint64(len(derivedRaw)) != derivation.Derived.Bytes || len(derivedRaw) == 0 || uint64(len(derivedRaw)) > owner.PhysicalMetadata.MaximumBytes || preparationDigest(derivedRaw) != derivation.Derived.Sha256 {
			return errors.Join(ErrIdentity, errors.New("physical metadata derivation no longer matches the reviewed result"))
		}
	}
	return nil
}
