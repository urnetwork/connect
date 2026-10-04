//go:build linux

// Cohort planners retain each exact public plan under its already approved
// staging parent. This is not target enrollment and never replaces an artifact.
package durablevolume

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Borrows the plan value and retains only canonical public bytes. The digest
// determines the one artifact name; a complete lost-ack artifact may be reused,
// while any different/partial existing inode remains untouched and refused.
func RetainPreparationPlan(ctx context.Context, plan PreparationPlan) (result Reference, resultErr error) {
	var request PreparationRequest
	if err := decodePreparationRequest(plan.RequestBytes, &request); err != nil {
		return result, err
	}
	if err := request.validate(daemonScope); err != nil {
		return result, err
	}
	if plan.Schema != PreparationPlanSchema || plan.RestartAuthorized || preparationDigest(plan.RequestBytes) != plan.RequestSha256 || plan.Request.Sha256 != plan.RequestSha256 {
		return result, errors.New("cohort staging plan lost its exact daemon request")
	}
	requestRaw, err := preparationReadReference(ctx, plan.Request, maximumPreparationRequestBytes, "cohort plan request")
	if err != nil {
		return result, err
	}
	if !bytes.Equal(requestRaw, plan.RequestBytes) {
		return result, errors.New("cohort staging request differs")
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		return result, err
	}
	raw = append(raw, '\n')
	if uint64(len(raw)) > request.Limits.MaxPlanBytes || len(raw) > maximumPreparationPlanBytes {
		return result, errors.New("cohort staging plan exceeds reviewed capacity")
	}
	identity, present := plan.Directories[request.StagingDirectory]
	if !present {
		return result, errors.New("cohort plan omits retained staging identity")
	}
	return retainPreparationArtifact(ctx, request.StagingDirectory, "plan", raw, &identity)
}

// Borrows the public request and retains its exact canonical bytes in staging.
// A later public planner rechecks all facts; this grants no target authority.
func RetainPreparationRequest(ctx context.Context, request PreparationRequest) (Reference, error) {
	if err := request.validate(daemonScope); err != nil {
		return Reference{}, err
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return Reference{}, err
	}
	raw = append(raw, '\n')
	if len(raw) > maximumPreparationRequestBytes {
		return Reference{}, errors.New("cohort staging request exceeds its byte bound")
	}
	return retainPreparationArtifact(ctx, request.StagingDirectory, "request", raw, nil)
}

// Borrows a bounded fixed cohort document and retains it beside its plans.
// Check/Apply still authenticate every referenced plan and all aggregate bounds.
func RetainPreparationCohort(ctx context.Context, cohort PreparationCohort, staging string) (Reference, error) {
	if cohort.Schema != PreparationCohortSchema || cohort.Scope != "daemon" || len(cohort.Plans) < 2 || len(cohort.Plans) > 256 || !canonical(staging) {
		return Reference{}, errors.New("cohort artifact requires the explicit finite daemon profile")
	}
	raw, err := json.Marshal(cohort)
	if err != nil {
		return Reference{}, err
	}
	raw = append(raw, '\n')
	if len(raw) > maximumPreparationRequestBytes {
		return Reference{}, errors.New("cohort artifact exceeds its byte bound")
	}
	return retainPreparationArtifact(ctx, staging, "cohort", raw, nil)
}

// Private no-replace staging artifacts admit exact lost acknowledgements only.
func retainPreparationArtifact(ctx context.Context, staging, kind string, raw []byte, expected *PreparationIdentity) (result Reference, resultErr error) {
	if ctx == nil {
		return result, errors.New("cohort staging requires a bounded context")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	directory, err := openPhysicalDirectory(staging)
	if err != nil {
		return result, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, directory.Close())
		if resultErr != nil {
			result = Reference{}
		}
	}()
	original, err := preparationIdentity(directory)
	if err != nil {
		return result, err
	}
	if expected != nil && *expected != original {
		return result, errors.Join(ErrIdentity, errors.New("cohort staging parent differs from reviewed plan"))
	}
	check := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		identity, err := preparationIdentity(directory)
		if err != nil {
			return err
		}
		if identity != original {
			return errors.Join(ErrIdentity, errors.New("cohort staging parent generation changed"))
		}
		return errors.Join(preparationPrivate(directory, true), sameNamedFile(directory, staging))
	}
	if err := check(); err != nil {
		return result, err
	}
	digest := preparationDigest(raw)
	name := "cohort-" + kind + "-" + strings.TrimPrefix(digest, "sha256:") + ".json"
	path := filepath.Join(staging, name)
	fd, err := syscall.Openat(int(directory.Fd()), name, syscall.O_RDWR|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	created := err == nil
	if errors.Is(err, syscall.EEXIST) {
		fd, err = syscall.Openat(int(directory.Fd()), name, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	}
	if err != nil {
		return result, namedObservation("cohort plan artifact could not be opened", err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer func() {
		resultErr = errors.Join(resultErr, file.Close())
		if created && resultErr != nil && !errors.Is(resultErr, ErrIdentity) {
			resultErr = errors.Join(ErrPreparationUncertain, resultErr)
		}
	}()
	if created {
		if err := file.Chmod(0600); err != nil {
			return result, err
		}
	}
	if err := preparationPrivate(file, false); err != nil {
		return result, err
	}
	if created {
		n, err := file.Write(raw)
		if err != nil || n != len(raw) {
			return result, errors.Join(io.ErrShortWrite, err)
		}
		if err := errors.Join(file.Sync(), directory.Sync()); err != nil {
			return result, err
		}
	}
	if err := errors.Join(preparationVerifyFile(ctx, file, uint64(len(raw)), digest), sameNamedFile(file, path), check()); err != nil {
		return result, err
	}
	return Reference{Path: path, Sha256: digest}, nil
}
