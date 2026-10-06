//go:build linux

// Every accepted public plan fits its fixed control grammar before target
// effects. JSON escaping and pending/completed metadata are counted explicitly.
package durablevolume

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
)

// Conservative actual wire records bound all legal inode/sequence widths and
// each adapter's fixed maximum checkpoint. Unknown eventual bytes may vary in
// value, but []byte encodes at a fixed base64 length in the retained intent.
func preparationControlCapacity(ctx context.Context, request PreparationRequest, plan PreparationPlan, cohortCapacity ...*uint64) error {
	identity := PreparationIdentity{Device: ^uint64(0), Inode: ^uint64(0), Mode: ^uint32(0), Uid: ^uint32(0), Gid: ^uint32(0)}
	digest := "sha256:" + strings.Repeat("f", 64)
	headerValue := preparationControlHeader{Schema: preparationControlSchema, PlanSha256: digest, Root: identity, Control: identity}
	if len(cohortCapacity) != 0 {
		headerValue.CohortSha256 = digest
	}
	header, err := json.Marshal(headerValue)
	if err != nil {
		return err
	}
	used := uint64(len(header) + 1)
	reserve := func(step preparationStep) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		record, err := json.Marshal(preparationControlRecord{Sequence: ^uint64(0), PreviousSha256: digest, Phase: "complete", Step: step, Identity: identity, Sha256: digest})
		if err != nil {
			return err
		}
		// A pending record is smaller, so two complete-size records are a safe
		// finite bound without relying on the future target's allocated inode.
		if len(record)+1 > maximumPreparationControlRecordBytes {
			return errors.New("preparation control record exceeds its finite encoded capacity")
		}
		pair := uint64(2 * (len(record) + 1))
		if pair > maximumPreparationControlBytes-used {
			return errors.New("preparation control record census exceeds its finite total byte capacity")
		}
		used += pair
		return nil
	}
	if request.RootCreation == "create-private" {
		if err := reserve(preparationRootStep(request, plan)); err != nil {
			return err
		}
	}
	attribute := func(path, name string, raw []byte) preparationStep {
		return preparationStep{Kind: "attribute", Path: path, Attribute: name, Bytes: uint64(len(raw)), Sha256: digest, Raw: raw}
	}
	if err := reserve(attribute(request.RootPath, RootGenerationAttribute, plan.Generation)); err != nil {
		return err
	}
	for _, source := range plan.Sources {
		step := preparationStep{Kind: source.File.Kind, Path: filepath.Join(request.RootPath, source.File.Path), Mode: source.File.Mode, Bytes: source.File.Bytes, Sha256: source.File.Sha256}
		step.Move = preparationSourceMoves(plan, source)
		if source.File.Kind == "file" {
			step.Source = &source
		}
		if err := reserve(step); err != nil {
			return err
		}
	}
	maximumAttribute := make([]byte, 4096)
	for _, owner := range plan.Owners {
		for _, spec := range owner.Attributes {
			path := request.RootPath
			if spec.Path != "." {
				path = filepath.Join(path, spec.Path)
			}
			if err := reserve(attribute(path, spec.Name, maximumAttribute)); err != nil {
				return err
			}
		}
	}
	declaration, err := json.MarshalIndent(request.declaration(plan.Root.Inode, digest, digest, digest), "", "  ")
	if err != nil {
		return err
	}
	for _, metadata := range []struct {
		path string
		raw  []byte
	}{
		{path: request.MarkerPath, raw: plan.Marker},
		{path: request.LeasePath, raw: plan.Lease},
		{path: request.DeclarationPath, raw: append(declaration, '\n')},
	} {
		if err := reserve(preparationStep{Kind: "file", Path: metadata.path, Mode: 0600, Bytes: uint64(len(metadata.raw)), Sha256: digest, Raw: metadata.raw}); err != nil {
			return err
		}
	}
	if len(cohortCapacity) == 1 && cohortCapacity[0] != nil {
		*cohortCapacity[0] = used
	}
	return ctx.Err()
}
